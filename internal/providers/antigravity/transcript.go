package antigravity

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const (
	transcriptSchema         = "antigravity_cli_transcript_v1"
	maxTranscriptBytes       = 64 << 20
	maxTranscriptLineBytes   = 4 << 20
	maxTranscriptDirectories = 10000
	maxTranscriptCycleBytes  = 256 << 20
	maxTranscriptEvents      = 100000
	maxTranscriptExactNumber = 1<<53 - 1
)

var transcriptConversationID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type transcriptCollectionError struct{ code, reason string }

func (e *transcriptCollectionError) Error() string               { return "antigravity: " + e.reason }
func (e *transcriptCollectionError) CollectionErrorCode() string { return e.code }

func transcriptError(code, reason string) error {
	return &transcriptCollectionError{code: code, reason: reason}
}

func defaultTranscriptsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain")
}

// collectTranscripts reads only the native CLI 1.4.3 transcript location. It
// never follows conversation symlinks, scans application logs, or copies text,
// thinking, tools, authentication or current-model metadata into telemetry.
// The transcript format is observed, not a promised versioned public schema.
func collectTranscripts(ctx context.Context, opts shared.TelemetryCollectOptions) ([]shared.TelemetryEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := shared.ExpandHome(opts.Path("transcripts_dir", defaultTranscriptsDir()))
	if root == "" {
		return nil, transcriptError("missing", "transcript directory unavailable")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, transcriptIOError(err)
	}
	if !info.IsDir() {
		return nil, transcriptError("unsupported_schema", "transcript root is not a directory")
	}
	dir, err := os.Open(root)
	if err != nil {
		return nil, transcriptIOError(err)
	}
	entries, err := dir.ReadDir(maxTranscriptDirectories + 1)
	_ = dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, transcriptIOError(err)
	}
	if len(entries) > maxTranscriptDirectories {
		return nil, transcriptError("parse_error", "transcript directory limit exceeded")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	account := opts.Path("account_id", defaultAccountID)
	var events []shared.TelemetryEvent
	var totalTokens int64
	bytesRead := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		path := filepath.Join(root, entry.Name())
		missing := false
		for _, component := range []string{".system_generated", "logs", "transcript.jsonl"} {
			path = filepath.Join(path, component)
			st, statErr := os.Lstat(path)
			if os.IsNotExist(statErr) {
				missing = true
				break
			}
			if statErr != nil {
				return nil, transcriptIOError(statErr)
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return nil, transcriptError("unsupported_schema", "symlinked transcript component")
			}
			if component != "transcript.jsonl" && !st.IsDir() {
				return nil, transcriptError("unsupported_schema", "invalid transcript directory structure")
			}
			if component == "transcript.jsonl" && !st.Mode().IsRegular() {
				return nil, transcriptError("unsupported_schema", "transcript is not a regular file")
			}
		}
		if missing {
			continue
		}
		if !transcriptConversationID.MatchString(entry.Name()) {
			return nil, transcriptError("unsupported_schema", "unsupported conversation identity")
		}
		data, readErr := readStableTranscript(path)
		if readErr != nil {
			return nil, readErr
		}
		bytesRead += len(data)
		if bytesRead > maxTranscriptCycleBytes {
			return nil, transcriptError("parse_error", "transcript collection size limit exceeded")
		}
		batch, parseErr := parseTranscript(ctx, data, strings.ToLower(entry.Name()), account)
		if parseErr != nil {
			return nil, parseErr
		}
		for _, event := range batch {
			// Each total includes all nonnegative known token buckets. Bound
			// the whole account batch before SQLite/JSON aggregation, not just
			// individual records; never return a partially accepted batch.
			if totalTokens > maxTranscriptExactNumber-*event.TotalTokens {
				return nil, transcriptError("parse_error", "transcript collection token total exceeds exact JSON numeric range")
			}
			totalTokens += *event.TotalTokens
		}
		events = append(events, batch...)
		if len(events) > maxTranscriptEvents {
			return nil, transcriptError("parse_error", "transcript event limit exceeded")
		}
	}
	return events, nil
}

func transcriptIOError(err error) error {
	// Preserve errno classification without putting private filesystem paths in
	// daemon logs. No source content or native parse error is included either.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		err = pathErr.Err
	}
	return fmt.Errorf("antigravity: read transcript: %w", err)
}

func readStableTranscript(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, transcriptIOError(err)
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, transcriptIOError(err)
	}
	if !before.Mode().IsRegular() {
		return nil, transcriptError("unsupported_schema", "transcript is not a regular file")
	}
	if before.Size() > maxTranscriptBytes {
		return nil, transcriptError("parse_error", "transcript size limit exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTranscriptBytes+1))
	if err != nil {
		return nil, transcriptIOError(err)
	}
	after, err := f.Stat()
	if err != nil {
		return nil, transcriptIOError(err)
	}
	if int64(len(data)) != before.Size() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, transcriptError("error", "transcript changed during collection; retry")
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, transcriptIOError(err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, current) || before.Size() != current.Size() || !before.ModTime().Equal(current.ModTime()) {
		return nil, transcriptError("error", "transcript replaced during collection; retry")
	}
	return data, nil
}

// RawMessage keeps missing/null/fraction/string counters distinct from zero.
// Fields carrying native content are intentionally not represented.
type transcriptRow struct {
	Type      string          `json:"type"`
	Status    string          `json:"status"`
	Source    string          `json:"source"`
	CreatedAt string          `json:"created_at"`
	StepIndex json.RawMessage `json:"step_index"`
	Input     json.RawMessage `json:"input_tokens"`
	Output    json.RawMessage `json:"output_tokens"`
	CacheRead json.RawMessage `json:"cache_read_tokens"`
}

func transcriptInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return value, err == nil && value >= 0 && value <= maxTranscriptExactNumber
}

func parseTranscript(ctx context.Context, data []byte, conversation, account string) ([]shared.TelemetryEvent, error) {
	// A writer may leave an incomplete last line. Ignore it until the newline
	// commits the record; a malformed complete line fails the whole collection.
	end := bytes.LastIndexByte(data, '\n')
	if len(data)-end-1 > maxTranscriptLineBytes {
		return nil, transcriptError("parse_error", "transcript line limit exceeded")
	}
	if end < 0 {
		return nil, nil
	}
	data = data[:end+1]
	namespace := sha256.Sum256([]byte(account))
	session := "antigravity:" + hex.EncodeToString(namespace[:16]) + ":" + conversation
	seen := make(map[int64]shared.TelemetryEvent)
	var events []shared.TelemetryEvent
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := bytes.IndexByte(data, '\n')
		line := bytes.TrimSpace(data[:n])
		data = data[n+1:]
		if len(line) == 0 {
			continue
		}
		if len(line) > maxTranscriptLineBytes {
			return nil, transcriptError("parse_error", "transcript line limit exceeded")
		}
		var row transcriptRow
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &row) != nil {
			return nil, transcriptError("parse_error", "invalid transcript JSON record")
		}
		if row.Type == "" || (row.Type == "PLANNER_RESPONSE" && (row.Status == "" || row.Source == "")) {
			return nil, transcriptError("unsupported_schema", "unrecognized transcript record schema")
		}
		hasCounters := len(row.Input) != 0 || len(row.Output) != 0 || len(row.CacheRead) != 0
		if row.Type != "PLANNER_RESPONSE" {
			if hasCounters {
				return nil, transcriptError("unsupported_schema", "usage on an unsupported transcript record")
			}
			continue
		}
		if row.Status == "ACTIVE" || row.Status == "ERROR" || row.Status == "ABORTED" || row.Status == "CANCELLED" {
			continue
		}
		if row.Status != "DONE" || row.Source != "MODEL" {
			if hasCounters {
				return nil, transcriptError("unsupported_schema", "usage on an unsupported transcript state or source")
			}
			continue
		}
		// Native older/non-metered responses can omit all three counters. They
		// are unknown usage, not zero-token requests. A partially populated or
		// explicitly null counter tuple is malformed and remains fail-closed.
		if len(row.Input) == 0 && len(row.Output) == 0 && len(row.CacheRead) == 0 {
			continue
		}
		step, stepOK := transcriptInteger(row.StepIndex)
		input, inputOK := transcriptInteger(row.Input)
		output, outputOK := transcriptInteger(row.Output)
		cache, cacheOK := transcriptInteger(row.CacheRead)
		if !stepOK || !inputOK || !outputOK || !cacheOK {
			return nil, transcriptError("parse_error", "completed transcript usage requires nonnegative integer counters and step identity")
		}
		occurred, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
		if err != nil || occurred.IsZero() {
			return nil, transcriptError("parse_error", "completed transcript usage requires a valid timestamp")
		}
		if output > math.MaxInt64-input || cache > math.MaxInt64-input-output {
			return nil, transcriptError("parse_error", "transcript token total overflow")
		}
		// Preserve the native three counters. This derived traffic total includes
		// cached reads; it is not the CLI's billable total or an estimated cost.
		total := input + output + cache
		if total > maxTranscriptExactNumber {
			return nil, transcriptError("parse_error", "transcript token total exceeds exact JSON numeric range")
		}
		event := shared.TelemetryEvent{
			SchemaVersion: transcriptSchema, Channel: shared.TelemetryChannelJSONL,
			OccurredAt: occurred.UTC(), AccountID: account, SessionID: session,
			MessageID: "step:" + strconv.FormatInt(step, 10), ProviderID: providerID,
			AgentName: providerID, EventType: shared.TelemetryEventTypeMessageUsage,
			TokenUsage: core.TokenUsage{InputTokens: core.Int64Ptr(input), OutputTokens: core.Int64Ptr(output), CacheReadTokens: core.Int64Ptr(cache), TotalTokens: core.Int64Ptr(total), Requests: core.Int64Ptr(1)},
			Status:     shared.TelemetryStatusOK,
			Payload:    map[string]any{"schema": transcriptSchema, "step_index": step},
		}
		if previous, ok := seen[step]; ok {
			if !previous.OccurredAt.Equal(event.OccurredAt) || *previous.InputTokens != input || *previous.OutputTokens != output || *previous.CacheReadTokens != cache {
				return nil, transcriptError("parse_error", "conflicting completed transcript step")
			}
			continue
		}
		seen[step] = event
		events = append(events, event)
	}
	return events, nil
}

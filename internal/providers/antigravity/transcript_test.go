package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

const testTranscriptConversation = "11111111-2222-4333-8444-555555555555"

// Synthetic fixture matching the flat schema observed in CLI 1.4.3. Source
// content is a canary: none of it may survive in events or diagnostic errors.
func transcriptFixture(overrides map[string]any) string {
	row := map[string]any{
		"type": "PLANNER_RESPONSE", "status": "DONE", "source": "MODEL",
		"created_at": "2026-10-20T07:08:51Z", "step_index": 3,
		"input_tokens": 278, "output_tokens": 4, "cache_read_tokens": 30214,
		"content": "PRIVATE_CONTENT_CANARY", "thinking": "PRIVATE_THINKING_CANARY",
		"tool_calls":   []any{map[string]any{"arguments": "PRIVATE_TOOL_CANARY"}},
		"access_token": "PRIVATE_TOKEN_CANARY",
	}
	for key, value := range overrides {
		if value == nil {
			delete(row, key)
		} else {
			row[key] = value
		}
	}
	data, _ := json.Marshal(row)
	return string(data) + "\n"
}

func writeTranscriptFixture(t *testing.T, root, conversation, data string) string {
	t.Helper()
	path := filepath.Join(root, conversation, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func transcriptOptions(root, account string) shared.TelemetryCollectOptions {
	return shared.TelemetryCollectOptions{Paths: map[string]string{"transcripts_dir": root, "account_id": account}}
}

func requireTranscriptError(t *testing.T, err error, code string) {
	t.Helper()
	var classified interface{ CollectionErrorCode() string }
	if !errors.As(err, &classified) || classified.CollectionErrorCode() != code {
		t.Fatalf("error=%v, want code %s", err, code)
	}
	if strings.Contains(err.Error(), "PRIVATE_") {
		t.Fatal("diagnostic exposed content")
	}
}

func TestTranscriptCollectFinalUsagePrivacyAndUnknowns(t *testing.T) {
	root := t.TempDir()
	data := `{"type":"USER_INPUT","content":"PRIVATE_USER_CANARY"}` + "\n"
	data += transcriptFixture(map[string]any{"status": "ACTIVE", "input_tokens": nil})
	data += transcriptFixture(nil)
	writeTranscriptFixture(t, root, testTranscriptConversation, data)
	events, err := collectTranscripts(context.Background(), transcriptOptions(root, "profile-one"))
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	event := events[0]
	if event.AccountID != "profile-one" || event.SchemaVersion != transcriptSchema || event.Channel != shared.TelemetryChannelJSONL || event.EventType != shared.TelemetryEventTypeMessageUsage {
		t.Fatalf("wrong attribution: %+v", event)
	}
	if *event.InputTokens != 278 || *event.OutputTokens != 4 || *event.CacheReadTokens != 30214 || *event.TotalTokens != 30496 || *event.Requests != 1 {
		t.Fatalf("wrong token buckets: %+v", event.TokenUsage)
	}
	if event.ReasoningTokens != nil || event.CacheWriteTokens != nil || event.CostUSD != nil || event.ModelRaw != "" || event.WorkspaceID != "" {
		t.Fatal("inferred unknown fields")
	}
	if event.OccurredAt.Format("2006-01-02T15:04:05Z") != "2026-10-20T07:08:51Z" {
		t.Fatal("event timestamp is not native timestamp")
	}
	encoded, _ := json.Marshal(event)
	if strings.Contains(string(encoded), "PRIVATE_") || strings.Contains(string(encoded), root) {
		t.Fatal("event retained native content or source path")
	}
	if len(event.Payload) != 2 {
		t.Fatalf("payload is not minimal: %v", event.Payload)
	}
}

func TestTranscriptZeroIsKnownAndMissingIsError(t *testing.T) {
	unknown := transcriptFixture(map[string]any{"input_tokens": nil, "output_tokens": nil, "cache_read_tokens": nil})
	unknownRows, unknownErr := parseTranscript(context.Background(), []byte(unknown), testTranscriptConversation, "a")
	if unknownErr != nil || len(unknownRows) != 0 {
		t.Fatalf("unknown usage became a zero request: %v %v", unknownRows, unknownErr)
	}
	rows, err := parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0})), testTranscriptConversation, "a")
	if err != nil || len(rows) != 1 || rows[0].InputTokens == nil || *rows[0].TotalTokens != 0 {
		t.Fatalf("zero usage lost: %v %v", rows, err)
	}
	for _, key := range []string{"input_tokens", "output_tokens", "cache_read_tokens", "step_index", "created_at"} {
		t.Run(key, func(t *testing.T) {
			rows, err := parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{key: nil})), testTranscriptConversation, "a")
			if len(rows) != 0 {
				t.Fatal("incomplete counters emitted")
			}
			requireTranscriptError(t, err, "parse_error")
		})
	}
}

func TestTranscriptRejectsMalformedNumbersAndOverflow(t *testing.T) {
	for _, value := range []any{-1, 1.5, "12", true, json.RawMessage("null"), json.RawMessage("9007199254740992"), json.RawMessage("9223372036854775808")} {
		t.Run(stringifyTranscriptTestValue(value), func(t *testing.T) {
			_, err := parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{"input_tokens": value})), testTranscriptConversation, "a")
			requireTranscriptError(t, err, "parse_error")
		})
	}
	_, err := parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{"input_tokens": json.RawMessage("9223372036854775807")})), testTranscriptConversation, "a")
	requireTranscriptError(t, err, "parse_error")
	_, err = parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{"input_tokens": json.RawMessage("9007199254740991")})), testTranscriptConversation, "a")
	requireTranscriptError(t, err, "parse_error")
	_, err = parseTranscript(context.Background(), []byte(transcriptFixture(map[string]any{"created_at": "PRIVATE_BAD_TIMESTAMP"})), testTranscriptConversation, "a")
	requireTranscriptError(t, err, "parse_error")
}

func stringifyTranscriptTestValue(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestTranscriptAppendPartialThenDoneAndDuplicateConflict(t *testing.T) {
	root := t.TempDir()
	opts := transcriptOptions(root, "a")
	active := transcriptFixture(map[string]any{"status": "ACTIVE", "input_tokens": nil})
	done := transcriptFixture(nil)
	path := writeTranscriptFixture(t, root, testTranscriptConversation, active+strings.TrimSuffix(done, "\n"))
	rows, err := collectTranscripts(context.Background(), opts)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unterminated row emitted: %v %v", rows, err)
	}
	if err := os.WriteFile(path, []byte(active+done+done), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err = collectTranscripts(context.Background(), opts)
	if err != nil || len(rows) != 1 {
		t.Fatalf("repeated DONE duplicated: %v %v", rows, err)
	}
	conflict := done + transcriptFixture(map[string]any{"output_tokens": 5})
	if err := os.WriteFile(path, []byte(conflict), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err = collectTranscripts(context.Background(), opts)
	if len(rows) != 0 {
		t.Fatal("part of a conflicting file escaped")
	}
	requireTranscriptError(t, err, "parse_error")
}

func TestTranscriptSkipsOtherSourcesAndMalformedCompleteLineFails(t *testing.T) {
	data := transcriptFixture(map[string]any{"source": "USER", "input_tokens": nil, "output_tokens": nil, "cache_read_tokens": nil}) + transcriptFixture(map[string]any{"status": "ERROR"})
	rows, err := parseTranscript(context.Background(), []byte(data), testTranscriptConversation, "a")
	if err != nil || len(rows) != 0 {
		t.Fatalf("non-completed model response emitted: %v %v", rows, err)
	}
	for _, line := range []string{"PRIVATE_INVALID_JSON\n", "[]\n", "null\n", `{"type":true}` + "\n"} {
		rows, err = parseTranscript(context.Background(), []byte(transcriptFixture(nil)+line), testTranscriptConversation, "a")
		if len(rows) != 0 {
			t.Fatal("partial results escaped parse failure")
		}
		requireTranscriptError(t, err, "parse_error")
	}
	for _, fields := range []map[string]any{{"type": nil}, {"status": nil}, {"source": nil}} {
		_, err = parseTranscript(context.Background(), []byte(transcriptFixture(fields)), testTranscriptConversation, "a")
		requireTranscriptError(t, err, "unsupported_schema")
	}
	for _, fields := range []map[string]any{{"type": "FUTURE_RESPONSE"}, {"status": "FUTURE_STATE"}, {"source": "FUTURE_SOURCE"}} {
		_, err = parseTranscript(context.Background(), []byte(transcriptFixture(fields)), testTranscriptConversation, "a")
		requireTranscriptError(t, err, "unsupported_schema")
	}
}

func TestTranscriptOnlyExpectedPathsAndSymlinkBoundary(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	writeTranscriptFixture(t, other, testTranscriptConversation, transcriptFixture(nil))
	if err := os.WriteFile(filepath.Join(root, "transcript.jsonl"), []byte(transcriptFixture(nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, testTranscriptConversation), filepath.Join(root, testTranscriptConversation)); err != nil {
		t.Fatal(err)
	}
	rows, err := collectTranscripts(context.Background(), transcriptOptions(root, "a"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("outside expected tree scanned: %v %v", rows, err)
	}
	if err := os.Remove(filepath.Join(root, testTranscriptConversation)); err != nil {
		t.Fatal(err)
	}
	conversationDir := filepath.Join(root, testTranscriptConversation)
	if err := os.Mkdir(conversationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, testTranscriptConversation, ".system_generated"), filepath.Join(conversationDir, ".system_generated")); err != nil {
		t.Fatal(err)
	}
	_, err = collectTranscripts(context.Background(), transcriptOptions(root, "a"))
	requireTranscriptError(t, err, "unsupported_schema")
}

func TestTranscriptMissingCancellationAndSizeBounds(t *testing.T) {
	root := t.TempDir()
	_, err := collectTranscripts(context.Background(), transcriptOptions(filepath.Join(root, "PRIVATE_ABSENT"), "a"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing not classified: %v", err)
	}
	if strings.Contains(err.Error(), "PRIVATE_ABSENT") {
		t.Fatal("error includes path")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = collectTranscripts(ctx, transcriptOptions(root, "a"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled=%v", err)
	}
	path := writeTranscriptFixture(t, root, testTranscriptConversation, "")
	if err := os.Truncate(path, maxTranscriptBytes+1); err != nil {
		t.Fatal(err)
	}
	_, err = collectTranscripts(context.Background(), transcriptOptions(root, "a"))
	requireTranscriptError(t, err, "parse_error")
	_, err = parseTranscript(context.Background(), []byte(strings.Repeat("x", maxTranscriptLineBytes+1)+"\n"), testTranscriptConversation, "a")
	requireTranscriptError(t, err, "parse_error")
	_, err = parseTranscript(context.Background(), []byte(strings.Repeat("x", maxTranscriptLineBytes+1)), testTranscriptConversation, "a")
	requireTranscriptError(t, err, "parse_error")
}

func transcriptIngestRequest(event shared.TelemetryEvent) telemetry.IngestRequest {
	return telemetry.IngestRequest{SourceSystem: telemetry.SourceSystem(providerID), SourceChannel: telemetry.SourceChannelJSONL, SourceSchemaVersion: event.SchemaVersion, OccurredAt: event.OccurredAt, SessionID: event.SessionID, MessageID: event.MessageID, ProviderID: event.ProviderID, AccountID: event.AccountID, AgentName: event.AgentName, EventType: telemetry.EventTypeMessageUsage, TokenUsage: event.TokenUsage, Status: telemetry.EventStatusOK, Payload: event.Payload}
}

func TestTranscriptReplayRestartAndProfileIsolation(t *testing.T) {
	root := t.TempDir()
	writeTranscriptFixture(t, root, testTranscriptConversation, transcriptFixture(nil))
	db := filepath.Join(t.TempDir(), "usage.db")
	var first shared.TelemetryEvent
	var firstID string
	for run := 0; run < 3; run++ {
		rows, err := collectTranscripts(context.Background(), transcriptOptions(root, "profile-one"))
		if err != nil || len(rows) != 1 {
			t.Fatalf("collect: %v %v", rows, err)
		}
		if run == 0 {
			first = rows[0]
		} else if !reflect.DeepEqual(first, rows[0]) {
			t.Fatal("poll/restart changed event identity")
		}
		store, err := telemetry.OpenStore(db)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Ingest(context.Background(), transcriptIngestRequest(rows[0]))
		if err != nil {
			t.Fatal(err)
		}
		if result.Deduped != (run > 0) {
			t.Fatalf("run %d dedup=%v", run, result.Deduped)
		}
		if run == 0 {
			firstID = result.EventID
		} else if result.EventID != firstID {
			t.Fatal("restart created another event")
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Two explicit accounts are separate even if a user later copies a transcript.
	// Choose one source for copied history; do not guess cross-account lineage.
	rows, err := collectTranscripts(context.Background(), transcriptOptions(root, "profile-two"))
	if err != nil {
		t.Fatal(err)
	}
	if telemetry.BuildDedupKey(transcriptIngestRequest(first)) == telemetry.BuildDedupKey(transcriptIngestRequest(rows[0])) {
		t.Fatal("separate account namespace collapsed")
	}
}

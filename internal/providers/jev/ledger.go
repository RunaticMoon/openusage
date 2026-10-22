package jev

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const maxSafeInteger int64 = 9007199254740991
const maxFileBytes int64 = 64 * 1024 * 1024
const maxScanBytes int64 = 256 * 1024 * 1024

var dayFile = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.jsonl$`)

// Decode only accounting fields. Unknown fields (including prompts, arbitrary
// tool payloads and credentials) never become snapshot metadata or diagnostics.
type ledgerEntry struct {
	Version      int      `json:"version"`
	ID           string   `json:"id"`
	Timestamp    string   `json:"timestamp"`
	Model        string   `json:"model"`
	InputTokens  *int64   `json:"inputTokens"`
	OutputTokens *int64   `json:"outputTokens"`
	Requests     *int64   `json:"requests"`
	Source       string   `json:"source"`
	Supersedes   []string `json:"supersedes"`
	at           time.Time
}

func (e ledgerEntry) requests() int64 {
	if e.Requests == nil {
		return 1
	}
	return *e.Requests
}
func validText(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 1024 && !strings.ContainsAny(s, "\x00\r\n")
}
func (e *ledgerEntry) validate() bool {
	if e.Version != 1 || !validText(e.ID) || !validText(e.Model) || e.InputTokens == nil || e.OutputTokens == nil {
		return false
	}
	for _, n := range []*int64{e.InputTokens, e.OutputTokens, e.Requests} {
		if n != nil && (*n < 0 || *n > maxSafeInteger) {
			return false
		}
	}
	if e.requests() < 1 || *e.InputTokens > maxSafeInteger-*e.OutputTokens {
		return false
	}
	if e.Source != "" && e.Source != "typesafe-console-csv" {
		return false
	}
	if len(e.Supersedes) > 100000 || (len(e.Supersedes) > 0 && e.Source != "typesafe-console-csv") {
		return false
	}
	for _, id := range e.Supersedes {
		if !validText(id) || id == e.ID {
			return false
		}
	}
	at, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return false
	}
	e.at = at.UTC()
	return true
}

func readLedger(ctx context.Context, root string) ([]ledgerEntry, error) {
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("jev: usage metadata directory is unavailable")
	}
	rows := make(map[string]ledgerEntry)
	var order []string
	superseded := make(map[string]bool)
	var bytes int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !dayFile.MatchString(file.Name()) {
			continue
		}
		if _, err := time.Parse("2006-01-02.jsonl", file.Name()); err != nil {
			return nil, fmt.Errorf("jev: invalid metadata date")
		}
		info, err := file.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes {
			return nil, fmt.Errorf("jev: unsafe or oversized metadata file")
		}
		bytes += info.Size()
		if bytes > maxScanBytes {
			return nil, fmt.Errorf("jev: metadata scan exceeds size limit")
		}
		entries, err := readFile(ctx, filepath.Join(root, file.Name()), info)
		if err != nil {
			return nil, err
		}
		for _, row := range entries {
			if old, ok := rows[row.ID]; ok {
				if !reflect.DeepEqual(old, row) {
					return nil, fmt.Errorf("jev: conflicting duplicate metadata identity")
				}
				continue
			}
			rows[row.ID] = row
			order = append(order, row.ID)
			if len(rows) > 100000 {
				return nil, fmt.Errorf("jev: metadata record limit exceeded")
			}
			for _, id := range row.Supersedes {
				superseded[id] = true
			}
		}
	}
	result := make([]ledgerEntry, 0, len(rows))
	for _, id := range order {
		if !superseded[id] {
			result = append(result, rows[id])
		}
	}
	return result, nil
}
func readFile(ctx context.Context, path string, expected os.FileInfo) ([]ledgerEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("jev: unable to open usage metadata")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(expected, opened) {
		return nil, fmt.Errorf("jev: metadata file changed during scan")
	}
	scanner := bufio.NewScanner(io.LimitReader(f, maxFileBytes+1))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var rows []ledgerEntry
	var size int64
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		size += int64(len(scanner.Bytes())) + 1
		if size > maxFileBytes {
			return nil, fmt.Errorf("jev: metadata file exceeds size limit")
		}
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		var row ledgerEntry
		if json.Unmarshal(scanner.Bytes(), &row) != nil || !row.validate() {
			return nil, fmt.Errorf("jev: invalid usage metadata")
		}
		var optional struct {
			Requests   json.RawMessage `json:"requests"`
			Source     json.RawMessage `json:"source"`
			Supersedes json.RawMessage `json:"supersedes"`
		}
		if json.Unmarshal(scanner.Bytes(), &optional) != nil {
			return nil, fmt.Errorf("jev: invalid usage metadata")
		}
		for _, value := range []json.RawMessage{optional.Requests, optional.Source, optional.Supersedes} {
			if strings.TrimSpace(string(value)) == "null" {
				return nil, fmt.Errorf("jev: invalid optional metadata field")
			}
		}
		rows = append(rows, row)
	}
	if scanner.Err() != nil {
		return nil, fmt.Errorf("jev: unable to read complete usage metadata")
	}
	return rows, nil
}

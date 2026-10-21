package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/klauspost/compress/zstd"
)

func fixture(t *testing.T, seeded bool, rows ...map[string]any) []byte {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "session", "version": 4, "id": "synthetic-session", "createdAt": 1800000000000, "isSeeded": seeded, "delegationDepth": 0, "cwd": "/PRIVATE_SOURCE_PATH"}); err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		row["seq"], row["time"] = i, 1800000000000+i
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

func row(kind string, fields map[string]any) map[string]any {
	return map[string]any{"type": kind, "data": fields}
}
func sample(input int) map[string]any {
	return map[string]any{"inputTokens": input, "outputTokens": 2, "cacheReadTokens": 3, "cacheWriteTokens": 4, "reasoningTokens": 1, "totalTokens": input + 9}
}
func message(turn, step, input int) map[string]any {
	return row("assistant/message", map[string]any{"turn": turn, "step": step, "usage": sample(input), "message": map[string]any{"source": map[string]any{"kind": "llm", "provider": "vendor-a", "model": "model-a"}, "content": "PRIVATE_SOURCE_BODY person@example.invalid"}})
}
func retry(turn, step int) map[string]any {
	return row("llm/retry-started", map[string]any{"turn": turn, "step": step})
}
func eventTotals(events []shared.TelemetryEvent) int64 {
	var n int64
	for _, e := range events {
		n += *e.TotalTokens
	}
	return n
}

func TestAccountingSlotsRetryFallbackCompactionAndPrivacy(t *testing.T) {
	stream := []any{
		map[string]any{"type": "chunk", "chunk": map[string]any{"type": "usage", "usage": sample(100)}},
		map[string]any{"type": "chunk", "chunk": map[string]any{"type": "usage", "usage": sample(5)}},
		map[string]any{"type": "chunk", "chunk": map[string]any{"type": "finish", "reason": map[string]any{"kind": "error", "message": "PRIVATE_ERROR_BODY"}}},
	}
	data := fixture(t, false,
		row("request/context", map[string]any{"provider": "vendor-b", "model": "model-b"}),
		message(0, 0, 0), message(0, 0, 30), message(0, 0, 20),
		retry(99, 99), message(0, 0, 10), // unrelated retry does not close the slot
		retry(0, 0), row("assistant/attempt", map[string]any{"turn": 0, "step": 0, "stream": stream}),
		message(0, 1, 7), message(0, 0, 8), // returning to an older tuple adds a new epoch
		row("compaction/summary", map[string]any{"provider": "vendor-c", "model": "model-c", "usage": sample(2), "summary": "PRIVATE_SUMMARY"}),
		message(0, 0, 4), // compaction does not close the assistant replacement slot
	)
	_, events, err := parseLog(context.Background(), data, "personal-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || eventTotals(events) != 73 {
		t.Fatalf("events=%d total=%d", len(events), eventTotals(events))
	}
	if *events[0].InputTokens != 10 || *events[1].InputTokens != 5 || *events[3].InputTokens != 4 {
		t.Fatal("slot/fallback semantics changed")
	}
	if events[1].ProviderID != "vendor-b" || events[0].ProviderID != "vendor-a" || events[4].ProviderID != "vendor-c" {
		t.Fatal("billing route lost")
	}
	if events[1].Payload["outcome"] != "error" || events[1].Status != shared.TelemetryStatusOK {
		t.Fatal("reported failed-attempt usage lost")
	}
	for _, event := range events {
		encoded, _ := json.Marshal(event.Payload)
		if strings.Contains(string(encoded), "PRIVATE") || strings.Contains(string(encoded), "example.invalid") || len(encoded) > 150 {
			t.Fatalf("non-accounting payload retained: %d bytes", len(encoded))
		}
		if *event.TotalTokens != *event.InputTokens+*event.OutputTokens+*event.CacheReadTokens+*event.CacheWriteTokens {
			t.Fatal("reasoning counted twice")
		}
	}
	_, again, err := parseLog(context.Background(), data, "personal-a")
	if err != nil || again[0].MessageID != events[0].MessageID {
		t.Fatal("replay identity changed")
	}
	_, other, err := parseLog(context.Background(), data, "personal-b")
	if err != nil || other[0].MessageID == events[0].MessageID {
		t.Fatal("account identity collided")
	}
}

func TestSeedCutAndRestoreBoundary(t *testing.T) {
	data := fixture(t, true,
		row("request/context", map[string]any{"provider": "vendor-b", "model": "model-b"}),
		message(0, 0, 100), row("session/end-seed", map[string]any{"inherited": true}),
		message(0, 1, 200), row("session/end-seed", map[string]any{"inherited": true}),
		message(0, 2, 3), row("session/end-seed", map[string]any{}), message(0, 3, 4),
	)
	_, events, err := parseLog(context.Background(), data, "test")
	if err != nil || len(events) != 2 || eventTotals(events) != 25 {
		t.Fatalf("nested seed/restore: events=%d err=%v", len(events), err)
	}
	for _, invalid := range [][]byte{fixture(t, true, message(0, 0, 1)), fixture(t, false, row("session/end-seed", map[string]any{"inherited": true}))} {
		if _, _, err := parseLog(context.Background(), invalid, "test"); err == nil {
			t.Fatal("inconsistent seed accepted")
		}
	}
}

func TestUsageAbsenceExplicitZeroAndInvalidNumericFields(t *testing.T) {
	noUsage := message(0, 0, 1)
	delete(noUsage["data"].(map[string]any), "usage")
	zero := message(0, 1, 1)
	zero["data"].(map[string]any)["usage"] = map[string]any{"inputTokens": 0, "outputTokens": 0}
	_, events, err := parseLog(context.Background(), fixture(t, false, noUsage, zero), "test")
	if err != nil || len(events) != 1 || *events[0].TotalTokens != 0 || events[0].ReasoningTokens != nil {
		t.Fatal("absence confused with reported zero")
	}
	for _, bad := range []any{-1, 1.5, float64(maxSafeInteger) + 1, "123", nil} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			m := message(0, 0, 1)
			m["data"].(map[string]any)["usage"].(map[string]any)["inputTokens"] = bad
			if _, _, err := parseLog(context.Background(), fixture(t, false, m), "test"); err == nil {
				t.Fatal("invalid numeric accepted")
			}
		})
	}
	bad := message(0, 0, 1)
	bad["data"].(map[string]any)["usage"].(map[string]any)["totalTokens"] = 999
	if _, _, err := parseLog(context.Background(), fixture(t, false, bad), "test"); err == nil {
		t.Fatal("inconsistent total accepted")
	}
}

func encodedFrames(t *testing.T, data []byte) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	var out []byte
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if len(line) > 0 {
			out = append(out, encoder.EncodeAll(line, nil)...)
		}
	}
	return out
}
func writeLog(t *testing.T, root, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGenerationSelectionConcatenatedZstdAndFailClosed(t *testing.T) {
	root := t.TempDir()
	data := fixture(t, false, message(0, 0, 10))
	compressed := encodedFrames(t, data)
	writeLog(t, root, "a/session.v3.jsonl", []byte("invalid retained predecessor"))
	writeLog(t, root, "a/session.v3.jsonl.zstd", []byte("another predecessor"))
	path := writeLog(t, root, "a/session.v4.jsonl.zstd", compressed)
	events, err := collect(context.Background(), root, "test")
	if err != nil || len(events) != 1 || eventTotals(events) != 19 {
		t.Fatalf("collect=%d err=%v", len(events), err)
	}
	for name, invalid := range map[string][]byte{"torn": compressed[:len(compressed)-1], "checksum": append(append([]byte{}, compressed[:len(compressed)-1]...), compressed[len(compressed)-1]^1)} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			if got, err := collect(context.Background(), root, "test"); err == nil || len(got) != 0 {
				t.Fatal("partial/corrupt log did not fail closed")
			}
		})
	}
	writeLog(t, root, "a/session.v4.jsonl.zstd", compressed)
	writeLog(t, root, "b/session.v4.jsonl", data[:len(data)-1])
	if got, err := collect(context.Background(), root, "test"); err == nil || len(got) != 0 {
		t.Fatal("partial plaintext accepted alongside valid source")
	}
	if err := os.Remove(filepath.Join(root, "b/session.v4.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeLog(t, root, "a/session.v5.jsonl", data)
	if _, err := collect(context.Background(), root, "test"); err != errSchema {
		t.Fatalf("future generation fallback: %v", err)
	}
}

func TestBoundedReadsAndProviderErrorClassification(t *testing.T) {
	root := t.TempDir()
	path := writeLog(t, root, "session.v4.jsonl", nil)
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxCompressedBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readLog(path); err == nil {
		t.Fatal("oversized source accepted")
	}
	if _, _, err := parseLog(context.Background(), append(bytes.Repeat([]byte("x"), maxLineBytes+1), '\n'), "test"); err == nil {
		t.Fatal("oversized line accepted")
	}
	provider := New()
	_, err = provider.Collect(context.Background(), shared.TelemetryCollectOptions{Paths: map[string]string{"account_id": "configured", "sessions_dir": filepath.Join(root, "missing")}})
	if err == nil || err.(*collectionError).CollectionErrorCode() != "missing" || strings.Contains(err.Error(), root) {
		t.Fatalf("diagnostic=%v", err)
	}
}

func TestFetchAndDefaultPersistenceRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DSH_HOME", root)
	if DefaultSessionsDir() != filepath.Join(root, "sessions") {
		t.Fatal("DSH_HOME ignored")
	}
	writeLog(t, root, "sessions/a/session.v4.jsonl", fixture(t, false, message(0, 0, 10)))
	snap, err := New().Fetch(context.Background(), core.AccountConfig{ID: "personal", Provider: ID})
	if err != nil || snap.Status != core.StatusOK || *snap.Metrics["total_tokens"].Used != 19 || *snap.Metrics["requests"].Used != 1 {
		t.Fatalf("fetch status=%v err=%v", snap.Status, err)
	}
	if len(snap.ModelUsage) != 1 || *snap.ModelUsage[0].CachedTokens != 7 {
		t.Fatal("model cache write tokens omitted")
	}
	t.Setenv("DSH_HOME", "")
	t.Setenv("HOME", root)
	if DefaultSessionsDir() != filepath.Join(root, ".dsh", "sessions") {
		t.Fatal("default home ignored")
	}
}

func TestCollectRejectsUnsafeAggregate(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "session.v4.jsonl", fixture(t, false, message(0, 0, int(maxSafeInteger-9)), message(0, 1, 1)))
	if events, err := collect(context.Background(), root, "test"); err != errParse || len(events) != 0 {
		t.Fatalf("unsafe aggregate: events=%d err=%v", len(events), err)
	}
}

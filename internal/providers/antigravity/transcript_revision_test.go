package antigravity

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

func TestRewrittenCompletedTranscriptHasNoCorrectionAuthority(t *testing.T) {
	root := t.TempDir()
	path := writeTranscriptFixture(t, root, testTranscriptConversation, transcriptFixture(nil))
	store, err := telemetry.OpenStore(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	var originalID string
	for run, output := range []int{4, 8} {
		if err := os.WriteFile(path, []byte(transcriptFixture(map[string]any{"output_tokens": output})), 0o600); err != nil {
			t.Fatal(err)
		}
		events, err := collectTranscripts(ctx, transcriptOptions(root, "profile-one"))
		if err != nil || len(events) != 1 {
			t.Fatalf("collect count=%d error=%v", len(events), err)
		}
		result, err := store.Ingest(ctx, transcriptIngestRequest(events[0]))
		if err != nil {
			t.Fatal(err)
		}
		if run == 0 {
			originalID = result.EventID
		} else if !result.Deduped || result.EventID != originalID {
			t.Fatal("rewriting settled usage created a second request")
		}
	}
	// A settled transcript has no native update sequence. File mtime and poll
	// time must not be invented as correction authority. Existing store policy
	// preserves these positive counters; this is a documented limitation, not
	// a claim that a rewritten settled record has been reconciled.
	var count, output, total int64
	if err := store.DB().QueryRow(`SELECT COUNT(*), SUM(output_tokens), SUM(total_tokens) FROM usage_events WHERE event_type='message_usage'`).Scan(&count, &output, &total); err != nil {
		t.Fatal(err)
	}
	if count != 1 || output != 4 || total != 30496 {
		t.Fatalf("settled record policy changed: count=%d output=%d total=%d", count, output, total)
	}
}

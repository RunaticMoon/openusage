package antigravity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func TestOptionalAbsentDefaultTranscriptSourceIsSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	p := New()
	ctx := context.Background()
	for _, opts := range []shared.TelemetryCollectOptions{{}, p.DefaultCollectOptions()} {
		events, err := p.Collect(ctx, opts)
		if err != nil || len(events) != 0 {
			t.Fatalf("unconfigured absent default source: events=%d error=%v", len(events), err)
		}
	}
	for _, opts := range []shared.TelemetryCollectOptions{
		transcriptOptions(defaultTranscriptsDir(), "explicit-profile"),
		{Paths: map[string]string{"transcripts_dir": filepath.Join(home, "custom-root")}},
	} {
		events, err := p.Collect(ctx, opts)
		if !errors.Is(err, os.ErrNotExist) || len(events) != 0 {
			t.Fatalf("configured missing source lost error: events=%d error=%v", len(events), err)
		}
	}
}

func TestOptionalPresentMalformedDefaultTranscriptStillFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeTranscriptFixture(t, defaultTranscriptsDir(), testTranscriptConversation, "INVALID_SYNTHETIC_RECORD\n")
	events, err := New().Collect(context.Background(), shared.TelemetryCollectOptions{})
	if len(events) != 0 {
		t.Fatal("malformed default source emitted events")
	}
	requireTranscriptError(t, err, "parse_error")
}

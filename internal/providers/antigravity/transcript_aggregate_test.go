package antigravity

import (
	"context"
	"testing"
)

func TestTranscriptCollectionBoundsAggregateAcrossConversations(t *testing.T) {
	root := t.TempDir()
	first := map[string]any{"input_tokens": maxTranscriptExactNumber - 1, "output_tokens": 0, "cache_read_tokens": 0}
	writeTranscriptFixture(t, root, testTranscriptConversation, transcriptFixture(first))
	other := "99999999-2222-4333-8444-555555555555"
	for _, input := range []int64{1, 2} {
		writeTranscriptFixture(t, root, other, transcriptFixture(map[string]any{
			"input_tokens": input, "output_tokens": 0, "cache_read_tokens": 0,
		}))
		events, err := collectTranscripts(context.Background(), transcriptOptions(root, "profile-one"))
		if input == 1 {
			if err != nil || len(events) != 2 || *events[0].TotalTokens+*events[1].TotalTokens != maxTranscriptExactNumber {
				t.Fatalf("exact boundary rejected: count=%d error=%v", len(events), err)
			}
		} else {
			if len(events) != 0 {
				t.Fatal("unsafe aggregate returned a partial batch")
			}
			requireTranscriptError(t, err, "parse_error")
		}
	}
}

func TestTranscriptCollectionBoundsAggregateWithinConversation(t *testing.T) {
	root := t.TempDir()
	data := transcriptFixture(map[string]any{"input_tokens": maxTranscriptExactNumber, "output_tokens": 0, "cache_read_tokens": 0})
	data += transcriptFixture(map[string]any{"step_index": 4, "input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 1})
	writeTranscriptFixture(t, root, testTranscriptConversation, data)
	events, err := collectTranscripts(context.Background(), transcriptOptions(root, "profile-one"))
	if len(events) != 0 {
		t.Fatal("unsafe single-conversation aggregate returned events")
	}
	requireTranscriptError(t, err, "parse_error")
}

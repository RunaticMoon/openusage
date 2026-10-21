package codex

import (
	"sort"
	"time"
)

// Turn terminals can close a competing sampler, but never supply a model
// request's start/end, output, or outcome. These scalars come from the same
// configured incremental JSONL pass; no second transcript scan is needed.
type nativeTurnTerminal struct {
	ThreadID, TurnID string
	At               time.Time
}

// Merge each file's bounded batch before moving to the next file. Both the
// retained result and the per-account cache contain at most 8192 terminals;
// temporary merge/dedup storage contains at most 16384. Chunking also bounds an
// unexpectedly large caller batch. Sorting makes file iteration order irrelevant.
func mergeCodexTurnTerminals(current, incoming []nativeTurnTerminal) []nativeTurnTerminal {
	if len(current) > codexTimingPendingLimit {
		current = mergeCodexTurnTerminals(nil, current)
	}
	for len(incoming) > 0 {
		n := min(len(incoming), codexTimingPendingLimit)
		merged := make([]nativeTurnTerminal, 0, len(current)+n)
		seen := make(map[nativeTurnTerminal]struct{}, len(current)+n)
		for _, batch := range [][]nativeTurnTerminal{current, incoming[:n]} {
			for _, terminal := range batch {
				if !nativeTimingIdentifier(terminal.ThreadID) || !nativeTimingIdentifier(terminal.TurnID) || terminal.At.IsZero() {
					continue
				}
				terminal.At = terminal.At.UTC()
				if _, exists := seen[terminal]; exists {
					continue
				}
				seen[terminal] = struct{}{}
				merged = append(merged, terminal)
			}
		}
		sort.Slice(merged, func(i, j int) bool {
			if !merged[i].At.Equal(merged[j].At) {
				return merged[i].At.After(merged[j].At)
			}
			if merged[i].ThreadID != merged[j].ThreadID {
				return merged[i].ThreadID < merged[j].ThreadID
			}
			return merged[i].TurnID < merged[j].TurnID
		})
		keep := min(len(merged), codexTimingPendingLimit)
		clear(merged[keep:])
		current = merged[:keep:keep]
		incoming = incoming[n:]
	}
	return current
}

func collectCodexTurnTerminal(record sessionLine, state *telemetryParserState) {
	p := record.EventPayload
	if p == nil || state.cliVersion != "0.163.0" || !nativeTimingIdentifier(state.nativeThreadID) || !nativeTimingIdentifier(p.TurnID) {
		return
	}
	if p.Type != "task_complete" && p.Type != "turn_complete" && p.Type != "turn_aborted" {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil || at.IsZero() {
		return
	}
	state.nativeTerminals = append(state.nativeTerminals, nativeTurnTerminal{ThreadID: state.nativeThreadID, TurnID: p.TurnID, At: at.UTC()})
	if len(state.nativeTerminals) > codexTimingPendingLimit {
		copy(state.nativeTerminals, state.nativeTerminals[len(state.nativeTerminals)-codexTimingPendingLimit:])
		state.nativeTerminals = state.nativeTerminals[:codexTimingPendingLimit]
	}
}

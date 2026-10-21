package antigravity

import (
	"context"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// System identifies native transcript collection and optional status-line input.
func (p *Provider) System() string { return p.ID() }

func (p *Provider) DefaultCollectOptions() shared.TelemetryCollectOptions {
	return shared.TelemetryCollectOptions{
		Paths: map[string]string{
			"status_file":     DefaultStatusFilePath(),
			"transcripts_dir": defaultTranscriptsDir(),
		},
	}
}

// Collect reads identifiable completed native transcript steps. Optional TUI
// status-line observations never create message_usage or raw_envelope events
// and a missing or malformed status file cannot block transcript history.
func (p *Provider) Collect(ctx context.Context, opts shared.TelemetryCollectOptions) ([]shared.TelemetryEvent, error) {
	return collectTranscripts(ctx, opts)
}

// ParseHookPayload validates explicit hook input without inferring a request
// from a state transition, token revision, repeated count or session change.
func (p *Provider) ParseHookPayload(raw []byte, _ shared.TelemetryCollectOptions) ([]shared.TelemetryEvent, error) {
	_, err := parseStatusLinePayload(raw)
	return nil, err
}

package opencode

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

// parseV2TelemetryEvent consumes the allowlisted native v2 step envelope. The
// plugin enriches final usage with model attribution from step.started, but no
// prompt, response, provider state or error body crosses the hook boundary.
func parseV2TelemetryEvent(raw []byte) ([]shared.TelemetryEvent, bool, error) {
	var event struct {
		Type     string `json:"type"`
		Created  int64  `json:"created"`
		Location struct {
			Directory string `json:"directory"`
		} `json:"location"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, false, nil
	}
	if event.Type != "session.step.ended" && event.Type != "session.step.failed" {
		return nil, false, nil
	}
	sessionID := shared.FirstPathString(event.Data, []string{"sessionID"})
	messageID := shared.FirstPathString(event.Data, []string{"assistantMessageID"})
	if sessionID == "" || messageID == "" {
		return nil, true, fmt.Errorf("opencode v2 step is missing stable session/message IDs")
	}
	if _, ok := event.Data["tokens"].(map[string]any); !ok {
		return nil, true, nil
	}
	u := extractUsage(event.Data)
	status := shared.TelemetryStatusOK
	if event.Type == "session.step.failed" {
		status = shared.TelemetryStatusError
	}
	return []shared.TelemetryEvent{{
		SchemaVersion: "opencode_event_v2",
		Channel:       shared.TelemetryChannelHook,
		OccurredAt:    shared.UnixAuto(event.Created),
		WorkspaceID:   shared.SanitizeWorkspace(event.Location.Directory),
		SessionID:     strings.TrimSpace(sessionID), MessageID: strings.TrimSpace(messageID),
		ProviderID: core.FirstNonEmpty(shared.FirstPathString(event.Data, []string{"model", "providerID"}), "opencode"),
		AgentName:  core.FirstNonEmpty(shared.FirstPathString(event.Data, []string{"agent"}), "opencode"),
		EventType:  shared.TelemetryEventTypeMessageUsage,
		ModelRaw:   core.FirstNonEmpty(shared.FirstPathString(event.Data, []string{"model", "modelID"}), shared.FirstPathString(event.Data, []string{"model", "id"})),
		TokenUsage: core.TokenUsage{
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			ReasoningTokens: u.ReasoningTokens, CacheReadTokens: u.CacheReadTokens,
			CacheWriteTokens: u.CacheWriteTokens, TotalTokens: u.TotalTokens,
			CostUSD: u.CostUSD, Requests: core.Int64Ptr(1),
		},
		Status:  status,
		Payload: map[string]any{"event_type": event.Type},
	}}, true, nil
}

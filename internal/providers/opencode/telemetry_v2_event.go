package opencode

import (
	"encoding/json"
	"fmt"
	"math"
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
	u, known, err := parseV2HookUsage(event.Data)
	if err != nil {
		return nil, true, err
	}
	if !known {
		return nil, true, nil
	}
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

// Restrict extraction to validated native fields; absent buckets remain nil.
// The shared normalizer's total is the known-bucket subtotal for partial usage.
func parseV2HookUsage(data map[string]any) (usage, bool, error) {
	const maxSafeInteger = float64(1<<53 - 1)
	clean := map[string]any{}
	known := false
	subtotal := float64(0)
	copyNumbers := func(from, to map[string]any, keys []string) error {
		for _, key := range keys {
			raw, present := from[key]
			if !present {
				continue
			}
			value, ok := raw.(float64)
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value || value > maxSafeInteger {
				return fmt.Errorf("invalid opencode v2 token counter")
			}
			subtotal += value
			if subtotal > maxSafeInteger {
				return fmt.Errorf("opencode v2 token subtotal exceeds safe integer range")
			}
			to[key] = value
			known = true
		}
		return nil
	}
	if raw, present := data["tokens"]; present {
		tokens, ok := raw.(map[string]any)
		if !ok {
			return usage{}, false, fmt.Errorf("invalid opencode v2 tokens object")
		}
		cleanTokens := map[string]any{}
		if err := copyNumbers(tokens, cleanTokens, []string{"input", "output", "reasoning"}); err != nil {
			return usage{}, false, err
		}
		if raw, present := tokens["cache"]; present {
			cache, ok := raw.(map[string]any)
			if !ok {
				return usage{}, false, fmt.Errorf("invalid opencode v2 cache object")
			}
			cleanCache := map[string]any{}
			if err := copyNumbers(cache, cleanCache, []string{"read", "write"}); err != nil {
				return usage{}, false, err
			}
			cleanTokens["cache"] = cleanCache
		}
		clean["tokens"] = cleanTokens
	}
	if raw, present := data["cost"]; present {
		value, ok := raw.(float64)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return usage{}, false, fmt.Errorf("invalid opencode v2 cost")
		}
		clean["cost"] = value
		known = true
	}
	return extractUsage(clean), known, nil
}

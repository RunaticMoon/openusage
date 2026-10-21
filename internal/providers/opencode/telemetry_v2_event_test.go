package opencode

import (
	"strings"
	"testing"

	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

func TestParseV2StepHook(t *testing.T) {
	payload := `{"event":{"type":"session.step.ended","created":1792540800000,"location":{"directory":"/synthetic/project"},"data":{"sessionID":"ses_1","assistantMessageID":"msg_1","model":{"id":"model-v2","providerID":"anthropic"},"tokens":{"input":100,"output":30,"reasoning":5,"cache":{"read":20,"write":2}},"cost":0.12,"text":"discard"}}}`
	for _, kind := range []string{"session.step.ended", "session.step.failed"} {
		events, err := ParseTelemetryHookPayload([]byte(strings.ReplaceAll(payload, "session.step.ended", kind)))
		if err != nil || len(events) != 1 {
			t.Fatalf("parse %s: events=%d err=%v", kind, len(events), err)
		}
		ev := events[0]
		if ev.MessageID != "msg_1" || ev.SessionID != "ses_1" || ev.ModelRaw != "model-v2" || ev.ProviderID != "anthropic" || ev.TotalTokens == nil || *ev.TotalTokens != 157 {
			t.Fatalf("unexpected normalized event: %+v", ev)
		}
		if len(ev.Payload) != 1 {
			t.Fatalf("payload retained extra fields: %#v", ev.Payload)
		}
		if kind == "session.step.failed" && ev.Status != shared.TelemetryStatusError {
			t.Fatalf("status=%s", ev.Status)
		}
	}
}

func TestParseV2StepHookMissingUsageAndIdentity(t *testing.T) {
	events, err := ParseTelemetryHookPayload([]byte(`{"event":{"type":"session.step.failed","data":{"sessionID":"ses_1","assistantMessageID":"msg_1","error":"discard"}}}`))
	if err != nil || len(events) != 0 {
		t.Fatalf("absent usage synthesized event: len=%d err=%v", len(events), err)
	}
	_, err = ParseTelemetryHookPayload([]byte(`{"event":{"type":"session.step.ended","data":{"tokens":{"input":2}}}}`))
	if err == nil {
		t.Fatal("missing stable identity should fail")
	}
}

func TestParseV2StepHookPreservesUnknownAndZero(t *testing.T) {
	prefix := `{"event":{"type":"session.step.ended","created":1792540800000,"data":{"sessionID":"s","assistantMessageID":"m",`
	for _, tc := range []struct {
		name, fields string
		input, cost  bool
	}{
		{"partial", `"tokens":{"input":0}`, true, false},
		{"cost-only", `"cost":0`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, err := ParseTelemetryHookPayload([]byte(prefix + tc.fields + `}}}`))
			if err != nil || len(events) != 1 {
				t.Fatalf("events=%d err=%v", len(events), err)
			}
			ev := events[0]
			if (ev.InputTokens != nil) != tc.input || (ev.CostUSD != nil) != tc.cost {
				t.Fatalf("known zero lost: %+v", ev.TokenUsage)
			}
			if ev.InputTokens != nil && *ev.InputTokens != 0 {
				t.Fatal("zero input changed")
			}
			if ev.CostUSD != nil && *ev.CostUSD != 0 {
				t.Fatal("zero cost changed")
			}
			if ev.OutputTokens != nil || ev.ReasoningTokens != nil || ev.CacheReadTokens != nil || ev.CacheWriteTokens != nil {
				t.Fatalf("missing field became zero: %+v", ev.TokenUsage)
			}
		})
	}
	for _, fields := range []string{`"tokens":"bad"`, `"tokens":null`, `"tokens":{"input":"0"}`, `"tokens":{"input":-1}`, `"tokens":{"input":1.5}`, `"tokens":{"input":9007199254740992}`, `"tokens":{"input":9007199254740991,"output":1}`, `"tokens":{"cache":[]}`, `"tokens":{"cache":{"read":null}}`, `"cost":"0"`, `"cost":-1`, `"tokens":{"input":1},"cost":null`} {
		if _, err := ParseTelemetryHookPayload([]byte(prefix + fields + `}}}`)); err == nil {
			t.Fatalf("malformed usage accepted: %s", fields)
		}
	}
	events, err := ParseTelemetryHookPayload([]byte(prefix + `"tokens":{},"usage":{"input_tokens":999}}}}`))
	if err != nil || len(events) != 0 {
		t.Fatal("unknown fields synthesized usage")
	}
}

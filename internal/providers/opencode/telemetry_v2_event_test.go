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

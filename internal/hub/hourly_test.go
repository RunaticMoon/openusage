package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
)

func TestHourlyHubPreservesContractAndHostIsolation(t *testing.T) {
	srv, store := newTestServer(t)
	series := map[string][]core.TimePoint{
		"analytics_tokens":       {{Date: "2026-10-23T10:00:00Z", Value: 100}, {Date: "2026-10-23T11:00:00Z", Value: 0}},
		"analytics_cost_partial": {{Date: "2026-10-23T10:00:00Z", Value: 1}},
	}
	for _, host := range []string{"hourly-a", "hourly-b"} {
		snap := makeSnap("codex", "personal")
		snap.HourlySeries = series
		body, err := json.Marshal(core.RemoteEnvelope{Machine: host, SentAt: time.Now(), Snapshots: []core.UsageSnapshot{snap}})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		srv.handlePush(response, httptest.NewRequest(http.MethodPost, "/v1/push", bytes.NewReader(body)))
		if response.Code != http.StatusOK {
			t.Fatalf("push status=%d", response.Code)
		}
	}
	response := httptest.NewRecorder()
	srv.handleSnapshots(response, httptest.NewRequest(http.MethodGet, "/v1/snapshots", nil))
	var snapshots map[string]core.UsageSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("lost host boundary: %d", len(snapshots))
	}
	for _, host := range []string{"hourly-a", "hourly-b"} {
		if !reflect.DeepEqual(snapshots[host+":codex:personal"].HourlySeries, series) {
			t.Fatalf("lost hourly contract for %s", host)
		}
	}
	view := store.Snapshots()["hourly-a:codex:personal"]
	view.HourlySeries["analytics_tokens"][0].Value = 999
	delete(view.HourlySeries, "analytics_cost_partial")
	if !reflect.DeepEqual(store.Snapshots()["hourly-a:codex:personal"].HourlySeries, series) {
		t.Fatal("view mutated Hub storage")
	}
}

func TestHourlyLegacyCompatibilityDoesNotInventOrSilentlyForward(t *testing.T) {
	legacy := makeSnap("codex", "personal")
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hourly_series") {
		t.Fatal("old snapshot acquired hourly observations")
	}
	var newer core.UsageSnapshot
	if err := json.Unmarshal(data, &newer); err != nil || newer.HourlySeries != nil {
		t.Fatalf("legacy decode=%v hourly=%+v", err, newer.HourlySeries)
	}
	legacy.HourlySeries = map[string][]core.TimePoint{"analytics_tokens": {{Date: "2026-10-23T10:00:00Z", Value: 1}}}
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		ProviderID  string                      `json:"provider_id"`
		AccountID   string                      `json:"account_id"`
		DailySeries map[string][]core.TimePoint `json:"daily_series,omitempty"`
	}
	if err := json.Unmarshal(data, &old); err != nil || old.ProviderID != legacy.ProviderID || old.AccountID != legacy.AccountID {
		t.Fatalf("legacy schema rejected new data: %v", err)
	}
	forwarded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(forwarded), "hourly_series") {
		t.Fatal("old typed decoder unexpectedly forwards hourly fields")
	}
}

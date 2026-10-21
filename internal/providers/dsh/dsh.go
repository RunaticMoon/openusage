// Package dsh reads provider-reported accounting from DeepSeek Harness v4 logs.
// It never starts dsh, reads credentials, or sends model/API requests.
package dsh

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/providerbase"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
)

const ID = "dsh"
const SchemaVersion = "dsh_jsonl_v4"

type Provider struct{ providerbase.Base }

func New() *Provider {
	widget := providerbase.DefaultDashboard(providerbase.WithColorRole(core.DashboardColorRoleSky))
	widget.CompactRows = []core.DashboardCompactRow{
		{Label: "Usage", Keys: []string{"requests", "total_tokens"}, MaxSegments: 2},
		{Label: "Tokens", Keys: []string{"input_tokens", "output_tokens", "cache_read_tokens"}, MaxSegments: 3},
	}
	return &Provider{Base: providerbase.New(core.ProviderSpec{
		ID:        ID,
		Info:      core.ProviderInfo{Name: "DeepSeek Harness", Capabilities: []string{"local_stats", "token_usage", "by_model"}, DocURL: "https://github.com/deepseek-ai/deepseek-harness"},
		Auth:      core.ProviderAuthSpec{Type: core.ProviderAuthTypeLocal, DefaultAccountID: ID},
		Setup:     core.ProviderSetupSpec{Quickstart: []string{"Read existing DeepSeek Harness v4 sessions; no credentials or model calls are required.", "Set provider_paths.sessions_dir for a non-default persistence root."}},
		Dashboard: widget,
	})}
}

func DefaultSessionsDir() string {
	home := strings.TrimSpace(os.Getenv("DSH_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(userHome, ".dsh")
	}
	return filepath.Join(shared.ExpandHome(home), "sessions")
}

func (p *Provider) System() string { return ID }
func (p *Provider) DefaultCollectOptions() shared.TelemetryCollectOptions {
	return shared.TelemetryCollectOptions{Paths: map[string]string{"sessions_dir": DefaultSessionsDir()}}
}
func (p *Provider) ParseHookPayload([]byte, shared.TelemetryCollectOptions) ([]shared.TelemetryEvent, error) {
	return nil, shared.ErrHookUnsupported
}
func (p *Provider) Collect(ctx context.Context, opts shared.TelemetryCollectOptions) ([]shared.TelemetryEvent, error) {
	root := shared.ExpandHome(opts.Path("sessions_dir", DefaultSessionsDir()))
	account := opts.Path("account_id", ID)
	events, err := collectWithPerformance(ctx, root, account, opts.PerformanceEnabled)
	if os.IsNotExist(err) && opts.Path("account_id", "") == "" && root == DefaultSessionsDir() {
		return nil, nil
	}
	return events, classify(err)
}

func (p *Provider) Fetch(ctx context.Context, acct core.AccountConfig) (core.UsageSnapshot, error) {
	snap := core.NewUsageSnapshot(ID, acct.ID)
	events, err := collect(ctx, shared.ExpandHome(acct.Path("sessions_dir", DefaultSessionsDir())), acct.ID)
	if err != nil {
		snap.Status, snap.Message = core.StatusError, "Unable to read DeepSeek Harness usage"
		return snap, classify(err)
	}
	snap.Status = core.StatusOK
	snap.Message = "No provider-reported usage in DeepSeek Harness logs"
	if len(events) == 0 {
		return snap, nil
	}
	totals := map[string]int64{}
	models := map[string]*core.ModelUsageRecord{}
	sessions := map[string]bool{}
	for _, event := range events {
		sessions[event.SessionID] = true
		for key, value := range map[string]*int64{"input_tokens": event.InputTokens, "output_tokens": event.OutputTokens, "cache_read_tokens": event.CacheReadTokens, "cache_write_tokens": event.CacheWriteTokens, "reasoning_tokens": event.ReasoningTokens, "total_tokens": event.TotalTokens, "requests": event.Requests} {
			if value != nil {
				if totals[key] > maxSafeInteger-*value {
					snap.Status, snap.Message = core.StatusError, "DeepSeek Harness usage exceeds safe numeric range"
					return snap, classify(errParse)
				}
				totals[key] += *value
			}
		}
		key := event.ProviderID + "\x00" + event.ModelRaw
		record := models[key]
		if record == nil {
			record = &core.ModelUsageRecord{RawModelID: event.ModelRaw, RawSource: "jsonl", Window: "all-time", InputTokens: core.Float64Ptr(0), OutputTokens: core.Float64Ptr(0), CachedTokens: core.Float64Ptr(0), TotalTokens: core.Float64Ptr(0), Requests: core.Float64Ptr(0)}
			record.SetDimension("upstream_provider", event.ProviderID)
			models[key] = record
		}
		for _, pair := range []struct {
			dst *float64
			src *int64
		}{{record.InputTokens, event.InputTokens}, {record.OutputTokens, event.OutputTokens}, {record.CachedTokens, event.CacheReadTokens}, {record.TotalTokens, event.TotalTokens}, {record.Requests, event.Requests}} {
			if pair.src != nil {
				*pair.dst += float64(*pair.src)
			}
		}
		if event.CacheWriteTokens != nil {
			*record.CachedTokens += float64(*event.CacheWriteTokens)
		}
		if event.ReasoningTokens != nil {
			if record.ReasoningTokens == nil {
				record.ReasoningTokens = core.Float64Ptr(0)
			}
			*record.ReasoningTokens += float64(*event.ReasoningTokens)
		}
	}
	totals["total_sessions"] = int64(len(sessions))
	for key, total := range totals {
		unit := "tokens"
		if key == "requests" {
			unit = "requests"
		}
		if key == "total_sessions" {
			unit = "sessions"
		}
		snap.Metrics[key] = core.Metric{Used: core.Float64Ptr(float64(total)), Unit: unit, Window: "all-time"}
	}
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		snap.AppendModelUsage(*models[key])
	}
	snap.Message = fmt.Sprintf("%d observed model calls across %d sessions", len(events), len(sessions))
	return snap, nil
}

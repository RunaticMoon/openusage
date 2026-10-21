// Package antigravity adapts native CLI transcript usage and optional TUI
// status-line observations to the OpenUsage provider and telemetry contracts.
package antigravity

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/janekbaraniewski/openusage/internal/core"
	"github.com/janekbaraniewski/openusage/internal/providers/providerbase"
	"github.com/janekbaraniewski/openusage/internal/providers/shared"
	"github.com/janekbaraniewski/openusage/internal/telemetry"
)

const (
	providerID           = "antigravity"
	defaultAccountID     = "antigravity"
	defaultUsageWindow   = "session"
	quotaNearLimitRatio  = 0.15
	statusFilePathEnvVar = "OPENUSAGE_ANTIGRAVITY_STATUS_FILE"
)

// Provider exposes Antigravity's documented status-line data as a local
// provider. It does not authenticate independently or make network calls.
type Provider struct {
	providerbase.Base
}

// New returns the Antigravity provider.
func New() *Provider {
	return &Provider{
		Base: providerbase.New(core.ProviderSpec{
			ID: providerID,
			Info: core.ProviderInfo{
				Name:         "Antigravity CLI",
				Capabilities: []string{"local_config", "statusline", "token_usage", "quota", "native_transcripts"},
				DocURL:       "https://antigravity.google/docs/cli/statusline/",
			},
			Auth: core.ProviderAuthSpec{
				Type:             core.ProviderAuthTypeLocal,
				DefaultAccountID: defaultAccountID,
			},
			Setup: core.ProviderSetupSpec{
				DocsURL: "https://antigravity.google/docs/cli/statusline/",
				Quickstart: []string{
					"Install and run the Antigravity CLI so ~/.gemini/antigravity-cli exists.",
					"Use provider_paths.transcripts_dir for an alternate profile's native brain directory.",
					"Optionally run `openusage integrations install antigravity` to connect TUI quota/context snapshots.",
				},
			},
			Dashboard: dashboardWidget(),
		}),
	}
}

// DetailWidget keeps the provider's detail view focused on the generic usage
// and token sections. Antigravity does not expose code-stat or tool-call data
// through the status-line contract.
func (p *Provider) DetailWidget() core.DetailWidget {
	return core.DefaultDetailWidget()
}

// DefaultStatusFilePath returns the path written by the installed status-line
// command. An environment override is useful for alternate installations and
// keeps tests isolated without changing the persisted account schema.
func DefaultStatusFilePath() string {
	if path := strings.TrimSpace(os.Getenv(statusFilePathEnvVar)); path != "" {
		return path
	}
	stateDir, err := telemetry.DefaultStateDir()
	if err != nil || strings.TrimSpace(stateDir) == "" {
		return ""
	}
	return filepath.Join(stateDir, "antigravity-status.json")
}

func statusFilePath(acct core.AccountConfig) string {
	return shared.ExpandHome(acct.Path("status_file", DefaultStatusFilePath()))
}

// Fetch projects the latest captured status-line document into a snapshot.
func (p *Provider) Fetch(ctx context.Context, acct core.AccountConfig) (core.UsageSnapshot, error) {
	snap := core.NewUsageSnapshot(p.ID(), acct.ID)
	path := statusFilePath(acct)
	snap.SetDiagnostic("statusline_history", "unsupported: status-line observations have no stable request identity")
	snap.SetDiagnostic("native_history_coverage", "Only completed records reporting counters; older records without counters omitted")
	if strings.TrimSpace(acct.Path("transcripts_dir", "")) != "" {
		snap.SetDiagnostic("native_transcripts", "configured; check source collector health for collection results")
	}

	if err := ctx.Err(); err != nil {
		return snap, err
	}
	if path == "" {
		snap.Status = core.StatusUnknown
		snap.Message = "Antigravity status-line path is unavailable"
		return snap, nil
	}

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			snap.Status = core.StatusUnknown
			snap.Message = "No Antigravity status-line data yet; transcript history is collected separately"
			snap.SetDiagnostic("setup", "TUI quota/context unavailable; native transcript history does not require a status-line callback")
			return snap, nil
		}
		return snap, fmt.Errorf("antigravity: read status file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return snap, fmt.Errorf("antigravity: stat status file: %w", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return snap, fmt.Errorf("antigravity: read status file: %w", err)
	}
	payload, err := parseStatusLinePayloadAt(data, info.ModTime())
	if err != nil {
		snap.Status = core.StatusError
		snap.Message = "Antigravity status-line data is malformed"
		snap.SetDiagnostic("parse_error", err.Error())
		return snap, nil
	}

	projectSnapshot(&snap, payload)
	return snap, nil
}

// HasChanged lets the dashboard avoid reparsing an unchanged status file.
func (p *Provider) HasChanged(acct core.AccountConfig, since time.Time) (bool, error) {
	path := statusFilePath(acct)
	if path == "" {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.ModTime().After(since), nil
}

func projectSnapshot(snap *core.UsageSnapshot, payload statusLinePayload) {
	if snap == nil {
		return
	}

	snap.Timestamp = payloadReceivedAt(payload)
	snap.Status = statusFromQuota(payload)

	modelID := strings.TrimSpace(payload.Model.ID)
	modelName := strings.TrimSpace(payload.Model.DisplayName)
	if modelName == "" {
		modelName = modelID
	}
	if modelID != "" {
		snap.SetAttribute("model_id", modelID)
	}
	if modelName != "" {
		snap.SetAttribute("model", modelName)
	}
	if workspace := shared.SanitizeWorkspace(statusWorkspace(payload)); workspace != "" {
		snap.SetAttribute("workspace", workspace)
	}
	if payload.SessionID != "" {
		snap.SetAttribute("session_id", payload.SessionID)
	}
	if payload.ConversationID != "" {
		snap.SetAttribute("conversation_id", payload.ConversationID)
	}
	if payload.Version != "" {
		snap.SetAttribute("cli_version", payload.Version)
	}
	if payload.Product != "" {
		snap.SetAttribute("product", payload.Product)
	}
	if payload.PlanTier != "" {
		snap.SetAttribute("plan_tier", payload.PlanTier)
	}
	if payload.AgentState != "" {
		snap.Raw["agent_state"] = payload.AgentState
	}

	projectContextMetrics(snap, payload.ContextWindow)
	projectQuotaMetrics(snap, payload)

	if total := cumulativeTotalTokens(payload.ContextWindow); total > 0 {
		snap.Metrics["total_tokens"] = core.Metric{
			Used:   core.Float64Ptr(float64(total)),
			Unit:   "tokens",
			Window: defaultUsageWindow,
		}
	}

	projectCurrentUsageMetrics(snap, payload.ContextWindow.CurrentUsage)
	if len(payload.Quota) == 0 && payload.expiredQuotaWindows > 0 {
		snap.Message = "Quota windows expired, waiting for fresh data"
		snap.SetDiagnostic("quota", "All quota windows in the last status-line snapshot are past their reset time; run agy to refresh")
	}
	if snap.Message == "" {
		if modelName != "" {
			snap.Message = fmt.Sprintf("Antigravity CLI (%s)", modelName)
		} else {
			snap.Message = "Antigravity CLI status line"
		}
	}
}

func projectContextMetrics(snap *core.UsageSnapshot, contextWindow statusLineContextWindow) {
	used, remaining, hasPercent := contextPercentages(contextWindow)
	if hasPercent {
		snap.Metrics["context_window"] = core.Metric{
			Limit:     core.Float64Ptr(100),
			Used:      core.Float64Ptr(used),
			Remaining: core.Float64Ptr(remaining),
			Unit:      "%",
			Window:    defaultUsageWindow,
		}
	}

	if contextWindow.TotalInputTokens > 0 {
		snap.Metrics["total_input_tokens"] = core.Metric{
			Used:   core.Float64Ptr(float64(contextWindow.TotalInputTokens)),
			Unit:   "tokens",
			Window: defaultUsageWindow,
		}
	}
	if contextWindow.TotalOutputTokens > 0 {
		snap.Metrics["total_output_tokens"] = core.Metric{
			Used:   core.Float64Ptr(float64(contextWindow.TotalOutputTokens)),
			Unit:   "tokens",
			Window: defaultUsageWindow,
		}
	}
	if !hasPercent && contextWindow.ContextWindowSize != nil && *contextWindow.ContextWindowSize > 0 {
		usedTokens := cumulativeTotalTokens(contextWindow)
		if usedTokens > 0 {
			snap.Metrics["context_window"] = core.Metric{
				Limit:  core.Float64Ptr(float64(*contextWindow.ContextWindowSize)),
				Used:   core.Float64Ptr(float64(usedTokens)),
				Unit:   "tokens",
				Window: defaultUsageWindow,
			}
		}
	}
}

func projectCurrentUsageMetrics(snap *core.UsageSnapshot, usage statusLineCurrentUsage) {
	setTokenMetric := func(key string, value int64) {
		if value <= 0 {
			return
		}
		snap.Metrics[key] = core.Metric{Used: core.Float64Ptr(float64(value)), Unit: "tokens", Window: "current"}
	}
	setTokenMetric("current_input_tokens", usage.InputTokens)
	setTokenMetric("current_output_tokens", usage.OutputTokens)
	setTokenMetric("current_cache_read_tokens", usage.CacheReadTokens)
	setTokenMetric("current_cache_write_tokens", usage.CacheWriteTokensValue())
	// The contract does not define whether cache counters are included in
	// input_tokens. Keep the reported components instead of inventing a sum.
}

func projectQuotaMetrics(snap *core.UsageSnapshot, payload statusLinePayload) {
	keys := make([]string, 0, len(payload.Quota))
	for key := range payload.Quota {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	worst := 1.0
	worstName := ""
	found := false
	for _, name := range keys {
		quota := payload.Quota[name]
		if quota.RemainingFraction == nil {
			continue
		}
		remaining := clamp(*quota.RemainingFraction, 0, 1)
		remainingPercent := remaining * 100
		key := "quota_" + sanitizeMetricName(name)
		snap.Metrics[key] = core.Metric{
			Limit:     core.Float64Ptr(100),
			Used:      core.Float64Ptr(100 - remainingPercent),
			Remaining: core.Float64Ptr(remainingPercent),
			Unit:      "%",
			Window:    "quota",
		}
		if reset := quotaResetTime(quota, payloadReceivedAt(payload)); !reset.IsZero() {
			snap.Resets[key+"_reset"] = reset
		}
		if !found || remaining < worst {
			worst = remaining
			worstName = name
			found = true
		}
	}

	if !found {
		return
	}
	remainingPercent := worst * 100
	snap.Metrics["quota"] = core.Metric{
		Limit:     core.Float64Ptr(100),
		Used:      core.Float64Ptr(100 - remainingPercent),
		Remaining: core.Float64Ptr(remainingPercent),
		Unit:      "%",
		Window:    "quota",
	}
	if quota, ok := payload.Quota[worstName]; ok {
		if reset := quotaResetTime(quota, payloadReceivedAt(payload)); !reset.IsZero() {
			snap.Resets["quota_reset"] = reset
		}
	}
}

func statusFromQuota(payload statusLinePayload) core.Status {
	worst, ok := worstQuotaFraction(payload)
	if !ok {
		return core.StatusOK
	}
	if worst <= 0 {
		return core.StatusLimited
	}
	if worst < quotaNearLimitRatio {
		return core.StatusNearLimit
	}
	return core.StatusOK
}

func worstQuotaFraction(payload statusLinePayload) (float64, bool) {
	worst := 1.0
	found := false
	for _, quota := range payload.Quota {
		if quota.RemainingFraction == nil {
			continue
		}
		fraction := clamp(*quota.RemainingFraction, 0, 1)
		if !found || fraction < worst {
			worst = fraction
			found = true
		}
	}
	return worst, found
}

func contextPercentages(contextWindow statusLineContextWindow) (float64, float64, bool) {
	if contextWindow.UsedPercentage == nil && contextWindow.RemainingPercentage == nil {
		return 0, 0, false
	}
	used := 0.0
	remaining := 0.0
	if contextWindow.UsedPercentage != nil {
		used = clamp(*contextWindow.UsedPercentage, 0, 100)
	}
	if contextWindow.RemainingPercentage != nil {
		remaining = clamp(*contextWindow.RemainingPercentage, 0, 100)
	}
	if contextWindow.UsedPercentage == nil {
		used = 100 - remaining
	}
	if contextWindow.RemainingPercentage == nil {
		remaining = 100 - used
	}
	return used, remaining, true
}

func cumulativeTotalTokens(contextWindow statusLineContextWindow) int64 {
	return contextWindow.TotalInputTokens + contextWindow.TotalOutputTokens
}

func statusWorkspace(payload statusLinePayload) string {
	if current := strings.TrimSpace(payload.Workspace.CurrentDir); current != "" {
		return current
	}
	if project := strings.TrimSpace(payload.Workspace.ProjectDir); project != "" {
		return project
	}
	return strings.TrimSpace(payload.CWD)
}

func payloadReceivedAt(payload statusLinePayload) time.Time {
	if !payload.ReceivedAt.IsZero() {
		return payload.ReceivedAt.UTC()
	}
	return time.Now().UTC()
}

func quotaResetTime(quota statusLineQuota, receivedAt time.Time) time.Time {
	if reset := strings.TrimSpace(quota.ResetTime); reset != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, reset); err == nil {
			return parsed.UTC()
		}
	}
	if quota.ResetInSeconds != nil && *quota.ResetInSeconds > 0 {
		return receivedAt.Add(time.Duration(*quota.ResetInSeconds) * time.Second)
	}
	return time.Time{}
}

func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

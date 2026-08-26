package quota

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const usageAnalysisQueryTimeout = 5 * time.Second

type KeySnapshot struct {
	ID                string              `json:"id"`
	Name              string              `json:"name"`
	FingerprintPrefix string              `json:"fingerprint_prefix"`
	KeySuffix         string              `json:"key_suffix,omitempty"`
	Enabled           bool                `json:"enabled"`
	AllowedModels     []string            `json:"allowed_models"`
	FiveHourBudgetUSD float64             `json:"five_hour_budget_usd"`
	SevenDayBudgetUSD float64             `json:"seven_day_budget_usd"`
	AccessRules       []AccessRule        `json:"access_rules"`
	AccessTimezone    string              `json:"access_timezone"`
	DollarSpend       DollarSpendSnapshot `json:"dollar_spend"`
	ActualTokens      ActualTokenSnapshot `json:"actual_tokens"`
}

type ActualTokenSnapshot struct {
	Available  bool  `json:"available"`
	Cycle      int64 `json:"cycle"`
	CycleKnown bool  `json:"cycle_known"`
	Total      int64 `json:"total"`
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	Output     int64 `json:"output"`
}

type TokenTotalsSnapshot struct {
	Available bool  `json:"available"`
	Input     int64 `json:"input"`
	Cached    int64 `json:"cached"`
	Output    int64 `json:"output"`
}

type StatusSnapshot struct {
	Provider               string `json:"provider"`
	MeteringMode           string `json:"metering_mode"`
	Configured             bool   `json:"configured"`
	PersistenceDegraded    bool   `json:"persistence_degraded"`
	AnalysisReaderDegraded bool   `json:"analysis_reader_degraded"`
	PersistenceFailures    uint64 `json:"persistence_failures"`
	RetentionFailures      uint64 `json:"retention_failures"`
	PendingLogs            int64  `json:"pending_logs"`
	DroppedDecisionLogs    uint64 `json:"dropped_decision_logs"`
}

type SummarySnapshot struct {
	Status      StatusSnapshot      `json:"status"`
	Keys        []KeySnapshot       `json:"keys"`
	TokenTotals TokenTotalsSnapshot `json:"token_totals"`
}

type UsageTrendPoint struct {
	At               time.Time `json:"at"`
	Units            int64     `json:"units"`
	RequestCount     int64     `json:"request_count"`
	InputTokens      int64     `json:"input_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	InputCostMicros  int64     `json:"input_cost_micros"`
	CachedCostMicros int64     `json:"cached_cost_micros"`
	OutputCostMicros int64     `json:"output_cost_micros"`
	CostMicros       int64     `json:"cost_micros"`
}

type UsageTrendSnapshot struct {
	From   time.Time         `json:"from"`
	To     time.Time         `json:"to"`
	Points []UsageTrendPoint `json:"points"`
}

type UsageAnalysisPoint struct {
	Start            time.Time `json:"start"`
	Label            string    `json:"label"`
	Units            int64     `json:"units"`
	RequestCount     int64     `json:"request_count"`
	InputTokens      int64     `json:"input_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	InputCostMicros  int64     `json:"input_cost_micros"`
	CachedCostMicros int64     `json:"cached_cost_micros"`
	OutputCostMicros int64     `json:"output_cost_micros"`
	CostMicros       int64     `json:"cost_micros"`
}

type UsageModelBreakdown struct {
	Model            string `json:"model"`
	InputTokens      int64  `json:"input_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	RequestCount     int64  `json:"request_count"`
	InputCostMicros  int64  `json:"input_cost_micros"`
	CachedCostMicros int64  `json:"cached_cost_micros"`
	OutputCostMicros int64  `json:"output_cost_micros"`
	CostMicros       int64  `json:"cost_micros"`
}

type UsageAnalysisBreakdown struct {
	InputTokens      int64                 `json:"input_tokens"`
	CachedTokens     int64                 `json:"cached_tokens"`
	OutputTokens     int64                 `json:"output_tokens"`
	InputCostMicros  int64                 `json:"input_cost_micros"`
	CachedCostMicros int64                 `json:"cached_cost_micros"`
	OutputCostMicros int64                 `json:"output_cost_micros"`
	CostMicros       int64                 `json:"cost_micros"`
	Models           []UsageModelBreakdown `json:"models"`
}

type UsageAnalysisSnapshot struct {
	From          time.Time            `json:"from"`
	To            time.Time            `json:"to"`
	Timezone      string               `json:"timezone"`
	Granularity   string               `json:"granularity"`
	AvailableFrom *time.Time           `json:"available_from,omitempty"`
	RetentionDays int                  `json:"retention_days"`
	TotalTokens   int64                `json:"total_tokens"`
	RequestCount  int64                `json:"request_count"`
	Points        []UsageAnalysisPoint `json:"points"`
	UsageAnalysisBreakdown
}

// ModelUsageRankingSnapshot is the durable, all-Key model rollup for one
// local calendar day. Costs are the request-time snapshots stored with usage.
type ModelUsageRankingSnapshot struct {
	From         time.Time             `json:"from"`
	To           time.Time             `json:"to"`
	Timezone     string                `json:"timezone"`
	TotalTokens  int64                 `json:"total_tokens"`
	RequestCount int64                 `json:"request_count"`
	CostMicros   int64                 `json:"cost_micros"`
	Models       []UsageModelBreakdown `json:"models"`
}

type DecisionLogPage struct {
	Logs       []DecisionLog `json:"logs"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

type OperationalLogPage struct {
	Logs       []OperationalLog `json:"logs"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	Total      int              `json:"total"`
	TotalPages int              `json:"total_pages"`
}

// LogStorageSnapshot separates the physical SQLite footprint from the
// logical payload owned by each mutually exclusive log view. Usage excludes
// content-blocking rows even though both are stored in request_logs.
type LogStorageSnapshot struct {
	DatabaseBytes    int64 `json:"database_bytes"`
	UsageBytes       int64 `json:"usage_bytes"`
	UsageRows        int64 `json:"usage_rows"`
	ForbiddenBytes   int64 `json:"forbidden_bytes"`
	ForbiddenRows    int64 `json:"forbidden_rows"`
	OperationalBytes int64 `json:"operational_bytes"`
	OperationalRows  int64 `json:"operational_rows"`
	RetentionDays    int   `json:"retention_days"`
}

// Summary exposes only the current dollar-meter product state. CPA accounts
// and upstream quota snapshots are intentionally not represented here.
func (engine *Engine) Summary(now time.Time) SummarySnapshot {
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	engine.policiesMu.RLock()
	policies := make([]KeyPolicy, 0, len(engine.policiesByID))
	for _, policy := range engine.policiesByID {
		policies = append(policies, policy)
	}
	engine.policiesMu.RUnlock()
	sort.Slice(policies, func(left, right int) bool {
		return strings.ToLower(policies[left].Name) < strings.ToLower(policies[right].Name)
	})
	keys := make([]KeySnapshot, 0, len(policies))
	for _, policy := range policies {
		fingerprint := policy.KeySHA256
		if len(fingerprint) > 12 {
			fingerprint = fingerprint[:12]
		}
		keys = append(keys, KeySnapshot{
			ID: policy.ID, Name: policy.Name, FingerprintPrefix: fingerprint,
			KeySuffix: policy.KeySuffix, Enabled: policy.Enabled,
			AllowedModels:     append([]string(nil), policy.AllowedModels...),
			FiveHourBudgetUSD: policy.FiveHourBudgetUSD, SevenDayBudgetUSD: policy.SevenDayBudgetUSD,
			AccessRules: append([]AccessRule(nil), policy.AccessRules...), AccessTimezone: policy.AccessTimezone,
			DollarSpend: engine.dollarSpendSnapshot(policy, now),
		})
	}
	return SummarySnapshot{Status: StatusSnapshot{
		Provider: cfg.Provider, MeteringMode: "key_dollar_rate_card_actual_token_usage",
		Configured: len(keys) > 0, PersistenceDegraded: engine.persistenceDegraded.Load(),
		AnalysisReaderDegraded: engine.AnalysisReaderDegraded(), PersistenceFailures: engine.persistenceFailures.Load(),
		RetentionFailures: engine.retentionFailures.Load(), PendingLogs: engine.pendingLogCount.Load(),
		DroppedDecisionLogs: engine.droppedDecisionLogs.Load(),
	}, Keys: keys}
}

func (engine *Engine) SummaryWithActualTokens(now time.Time) (SummarySnapshot, error) {
	summary := engine.Summary(now)
	cycleStarts := make(map[string]*time.Time, len(summary.Keys))
	for index := range summary.Keys {
		cycleStart := now.UTC().Add(-sevenDayWindow)
		cycleStarts[summary.Keys[index].ID] = &cycleStart
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageAnalysisQueryTimeout)
	defer cancel()
	totals, err := engine.store.LoadKeyActualTokenTotals(ctx, cycleStarts)
	if err != nil {
		return SummarySnapshot{}, err
	}
	tokenTotals, err := engine.store.LoadCompletedTokenTotals(ctx)
	if err != nil {
		return SummarySnapshot{}, err
	}
	for index := range summary.Keys {
		key := &summary.Keys[index]
		total := totals[key.ID]
		key.ActualTokens = ActualTokenSnapshot{Available: true, Cycle: total.Cycle, CycleKnown: true,
			Total: total.Total, Input: total.Input, Cached: total.Cached, Output: total.Output}
	}
	summary.TokenTotals = tokenTotals
	return summary, nil
}

func (engine *Engine) Policies() []KeyPolicy {
	engine.policiesMu.RLock()
	defer engine.policiesMu.RUnlock()
	policies := make([]KeyPolicy, 0, len(engine.policiesByID))
	for _, policy := range engine.policiesByID {
		policies = append(policies, policy)
	}
	sort.Slice(policies, func(left, right int) bool { return policies[left].ID < policies[right].ID })
	return policies
}

// UpsertPolicy stores only an HMAC fingerprint of the selected CPA API Key.
func (engine *Engine) UpsertPolicy(policy KeyPolicy, rawAPIKey string) (KeyPolicy, error) {
	if engine == nil {
		return KeyPolicy{}, fmt.Errorf("codex-carpool is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.policiesMu.Lock()
	existing, exists := engine.policiesByID[policy.ID]
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	if rawAPIKey = strings.TrimSpace(rawAPIKey); rawAPIKey != "" {
		policy.KeySHA256 = FingerprintAPIKey(rawAPIKey, cfg.KeyHMACSecret)
		policy.KeySuffix = APIKeySuffix(rawAPIKey)
	} else if exists {
		policy.KeySHA256 = existing.KeySHA256
		policy.KeySuffix = existing.KeySuffix
	}
	validated, err := normalizePolicy(policy)
	if err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, err
	}
	if len(validated.AllowedModels) > 0 {
		catalog, catalogErr := engine.store.ListModelCatalog()
		if catalogErr != nil {
			engine.policiesMu.Unlock()
			return KeyPolicy{}, fmt.Errorf("read synchronized CPA model catalog: %w", catalogErr)
		}
		if err := validateAllowedModels(validated.AllowedModels, catalog); err != nil {
			engine.policiesMu.Unlock()
			return KeyPolicy{}, err
		}
	}
	if exists && existing.KeySHA256 != validated.KeySHA256 && engine.pendingRequestsForKey(existing.ID) > 0 {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, fmt.Errorf("cannot rebind Key policy %q while admitted requests await terminal usage", existing.ID)
	}
	if otherID, duplicate := engine.policiesByHash[validated.KeySHA256]; duplicate && otherID != validated.ID {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, fmt.Errorf("the API key fingerprint is already managed by %q", otherID)
	}
	if err := engine.store.UpsertPolicy(validated); err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, err
	}
	if exists {
		delete(engine.policiesByHash, existing.KeySHA256)
	}
	engine.policiesByID[validated.ID] = validated
	engine.policiesByHash[validated.KeySHA256] = validated.ID
	engine.policiesMu.Unlock()
	engine.keyState(validated.ID)
	engine.keySpendState(validated.ID)
	return validated, nil
}

func (engine *Engine) pendingRequestsForKey(keyID string) int {
	engine.pendingMu.Lock()
	defer engine.pendingMu.Unlock()
	count := 0
	for _, marker := range engine.pendingRequests {
		if marker.KeyID == keyID {
			count++
		}
	}
	return count
}

func validateAllowedModels(allowed []string, catalog []ModelCatalogEntry) error {
	if len(allowed) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(catalog))
	for _, model := range catalog {
		if model.Available {
			known[model.ID] = struct{}{}
		}
	}
	if len(known) == 0 {
		return fmt.Errorf("synchronized CPA model catalog is empty; sync models before applying a restriction")
	}
	for _, model := range allowed {
		if _, exists := known[model]; !exists {
			return fmt.Errorf("model %q is not in the synchronized CPA model catalog; sync models and retry", model)
		}
	}
	return nil
}

func (engine *Engine) ResetPolicyUsage(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.policiesMu.RLock()
	_, exists := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if keyID == "" || !exists {
		return fmt.Errorf("Key policy %q was not found", keyID)
	}
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush Key usage before reset: %w", err)
	}
	if err := engine.store.ResetPolicyUsage(keyID); err != nil {
		return err
	}
	engine.discardKeyAccounting(keyID)
	return nil
}

func (engine *Engine) DeletePolicy(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.policiesMu.RLock()
	policy, exists := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if keyID == "" || !exists {
		return fmt.Errorf("Key policy %q was not found", keyID)
	}
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush Key usage before delete: %w", err)
	}
	if err := engine.store.DeletePolicy(keyID); err != nil {
		return err
	}
	engine.policiesMu.Lock()
	delete(engine.policiesByID, keyID)
	delete(engine.policiesByHash, policy.KeySHA256)
	engine.policiesMu.Unlock()
	engine.discardKeyAccounting(keyID)
	return nil
}

func (engine *Engine) discardKeyAccounting(keyID string) {
	engine.statesMu.Lock()
	delete(engine.states.keys, keyID)
	delete(engine.states.spend, keyID)
	engine.statesMu.Unlock()
	engine.cyclesMu.Lock()
	delete(engine.cycles, keyID)
	engine.cyclesMu.Unlock()
	engine.discardPendingRequestsForKey(keyID)
}

type InstallationSnapshot struct {
	Settings InstallationSettings `json:"settings"`
}

func (engine *Engine) Installation() InstallationSnapshot {
	engine.configMu.RLock()
	defer engine.configMu.RUnlock()
	return InstallationSnapshot{Settings: InstallationSettings{RecordRetention: engine.config.RecordRetention}}
}

func (engine *Engine) ConfigureInstallation(settings InstallationSettings) (InstallationSnapshot, error) {
	if engine == nil {
		return InstallationSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.configMu.RLock()
	current := engine.config
	engine.configMu.RUnlock()
	normalized, err := normalizeInstallationSettings(settings, current.KeyHMACSecret)
	if err != nil {
		return InstallationSnapshot{}, err
	}
	settings = normalized.InstallationSettings
	if err := engine.flushPending(); err != nil {
		return InstallationSnapshot{}, fmt.Errorf("flush before updating settings: %w", err)
	}
	if _, err := engine.store.SaveInstallation(settings, current.KeyHMACSecret); err != nil {
		return InstallationSnapshot{}, err
	}
	current.RecordRetention = settings.RecordRetention
	current.RecordRetentionDuration, _ = time.ParseDuration(current.RecordRetention)
	engine.configMu.Lock()
	engine.config = current
	engine.configMu.Unlock()
	return engine.Installation(), nil
}

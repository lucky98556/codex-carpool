package quota

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const usageAnalysisQueryTimeout = 5 * time.Second

// WindowSnapshot reports one Key's calibrated actual-Token x ledger plus the
// bounded provisional estimate awaiting durable settlement. Both retain the
// serving account's official reset identity; neither creates a local rolling
// seven-day quota or assigns account-wide percentage movement to the Key.
type WindowSnapshot struct {
	// Capacity is the effective guard capacity in the active official window.
	// It can be larger than PolicyCapacity after an operator lowers x mid-week:
	// decreases take effect only after the official reset, while increases are
	// immediately raised into this durable cycle.
	Capacity            int64      `json:"capacity"`
	PolicyCapacity      int64      `json:"policy_capacity"`
	HasDeferredDecrease bool       `json:"has_deferred_decrease"`
	Used                int64      `json:"used"`
	Completed           int64      `json:"completed"`
	Provisional         int64      `json:"provisional"`
	Reserved            int64      `json:"reserved"`
	Remaining           int64      `json:"remaining"`
	Multiplier          float64    `json:"multiplier"`
	ResetAt             *time.Time `json:"reset_at,omitempty"`
	// Explicit x fields keep the management UI independent from the internal
	// fixed-point scale while preserving the integer fields for diagnostics.
	CapacityX       float64 `json:"capacity_x"`
	PolicyCapacityX float64 `json:"policy_capacity_x"`
	UsedX           float64 `json:"used_x"`
	ConfirmedX      float64 `json:"confirmed_x"`
	ProvisionalX    float64 `json:"provisional_x"`
	ReservedX       float64 `json:"reserved_x"`
	RemainingX      float64 `json:"remaining_x"`
	MeteringMode    string  `json:"metering_mode,omitempty"`
}

type KeySnapshot struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	FingerprintPrefix string         `json:"fingerprint_prefix"`
	Enabled           bool           `json:"enabled"`
	AllowedModels     []string       `json:"allowed_models"`
	NeedsRebind       bool           `json:"needs_rebind"`
	AllocationX       float64        `json:"allocation_x"`
	AccessRules       []AccessRule   `json:"access_rules"`
	AccessTimezone    string         `json:"access_timezone"`
	Allocation        WindowSnapshot `json:"allocation"`
	ActualTokens      ActualTokenSnapshot `json:"actual_tokens"`
}

// ActualTokenSnapshot separates the current official weekly window from the
// durable cumulative total. CycleKnown prevents the panel from presenting
// zero usage while the official reset identity is unavailable.
type ActualTokenSnapshot struct {
	Available  bool  `json:"available"`
	Cycle      int64 `json:"cycle"`
	CycleKnown bool  `json:"cycle_known"`
	Total      int64 `json:"total"`
}

type StatusSnapshot struct {
	Provider               string  `json:"provider"`
	MeteringMode           string  `json:"metering_mode"`
	RequestUnits           int64   `json:"request_units"`
	Configured             bool    `json:"configured"`
	PersistenceDegraded    bool    `json:"persistence_degraded"`
	AnalysisReaderDegraded bool    `json:"analysis_reader_degraded"`
	AccountSourceConflict  bool    `json:"account_source_conflict"`
	PersistenceFailures    uint64  `json:"persistence_failures"`
	RetentionFailures      uint64  `json:"retention_failures"`
	PendingLogs            int64   `json:"pending_logs"`
	DroppedDecisionLogs    uint64  `json:"dropped_decision_logs"`
	PoolCapacityX          float64 `json:"pool_capacity_x"`
	AllocatedX             float64 `json:"allocated_x"`
}

type SummarySnapshot struct {
	Status   StatusSnapshot        `json:"status"`
	Keys     []KeySnapshot         `json:"keys"`
	Accounts []AccountPoolSnapshot `json:"accounts"`
}

type AccountPoolSnapshot struct {
	AccountPoolEntry
	Quota *OfficialQuotaSnapshot `json:"quota,omitempty"`
	Fresh bool                   `json:"fresh"`
}

type UsageTrendPoint struct {
	At           time.Time `json:"at"`
	Units        int64     `json:"units"`
	RequestCount int64     `json:"request_count"`
}

type UsageTrendSnapshot struct {
	From   time.Time         `json:"from"`
	To     time.Time         `json:"to"`
	Points []UsageTrendPoint `json:"points"`
}

// UsageAnalysisPoint is one local-calendar bucket of actual, completed Key
// token usage. It is intentionally calculated from the durable key_actual
// ledger, rather than from an official percentage or an estimated allocation.
type UsageAnalysisPoint struct {
	Start        time.Time `json:"start"`
	Label        string    `json:"label"`
	Units        int64     `json:"units"`
	RequestCount int64     `json:"request_count"`
}

// UsageAnalysisSnapshot is the bounded management-plane response for the
// single-Key day/month/year and custom-date analysis. It never participates in
// a CPA request admission or triggers an official quota refresh.
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
}

// DecisionLogPage is the bounded Key-level request audit response. It is
// intentionally separate from OperationalLogPage: these rows describe a
// managed Key's routing and usage settlement, not plugin lifecycle events.
type DecisionLogPage struct {
	Logs       []DecisionLog `json:"logs"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// OperationalLogPage is the bounded management response for runtime logs.
// Page metadata lets the panel search retained history without loading every
// row into the browser.
type OperationalLogPage struct {
	Logs       []OperationalLog `json:"logs"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	Total      int              `json:"total"`
	TotalPages int              `json:"total_pages"`
}

// QuotaDebugSnapshot is an on-demand, copy-safe explanation of the current
// Key allocation. It deliberately contains no API Key, OAuth credential,
// prompt, response body, or full account identity, so an operator can share
// it while investigating an unexpected local x guard value.
type QuotaDebugSnapshot struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	Key         QuotaDebugKey             `json:"key"`
	Formula     QuotaDebugFormula         `json:"formula"`
	Allocation  WindowSnapshot            `json:"allocation"`
	Accounts    []QuotaDebugAccountWindow `json:"accounts"`
	RecentLogs  []QuotaDebugDecisionLog   `json:"recent_logs"`
}

type QuotaDebugKey struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	AllocationX float64 `json:"allocation_x"`
}

// QuotaDebugFormula documents the conservative local scale. Codex exposes an
// official weekly percentage and reset time, not a total weekly Token budget.
type QuotaDebugFormula struct {
	Description              string  `json:"description"`
	RequestReservationX      float64 `json:"request_reservation_x"`
	FixedPointUnitsPerX      int64   `json:"fixed_point_units_per_x"`
	RequestReservationTokens int64   `json:"request_reservation_tokens"`
	BaseRequestsPerX         int64   `json:"base_requests_per_x"`
	PerXCapacityTokens       int64   `json:"per_x_capacity_tokens"`
	ProvisionalPercentCap    float64 `json:"provisional_percent_cap"`
	CalibrationMethod        string  `json:"calibration_method"`
	EnforcementMethod        string  `json:"enforcement_method"`
}

// QuotaDebugAccountWindow describes one account-owned weekly source. The
// selected account's official percentage delta is attributed only after
// completed Token evidence is durably available.
type QuotaDebugAccountWindow struct {
	AccountLabel                   string     `json:"account_label"`
	AccountSuffix                  string     `json:"account_suffix"`
	AccountCapacityX               float64    `json:"account_capacity_x"`
	KeyAllocationX                 float64    `json:"key_allocation_x"`
	Eligible                       bool       `json:"eligible"`
	Eligibility                    string     `json:"eligibility"`
	WindowResetAt                  *time.Time `json:"window_reset_at,omitempty"`
	KeyCapacity                    int64      `json:"key_capacity"`
	KeyPolicyCapacity              int64      `json:"key_policy_capacity"`
	KeyHasDeferredDecrease         bool       `json:"key_has_deferred_decrease"`
	KeyUsed                        int64      `json:"key_used"`
	KeyRemaining                   int64      `json:"key_remaining"`
	AccountCompleted               int64      `json:"account_completed"`
	AccountProvisional             int64      `json:"account_provisional"`
	AccountReserved                int64      `json:"account_reserved"`
	OfficialWeeklyUsedPercent      *float64   `json:"official_weekly_used_percent,omitempty"`
	OfficialWeeklyRemainingPercent *float64   `json:"official_weekly_remaining_percent,omitempty"`
	OfficialWeeklyGuardTokens      int64      `json:"official_weekly_guard_tokens"`
	OfficialWeeklyGuardX           float64    `json:"official_weekly_guard_x"`
	ProvisionalLimitX               float64    `json:"provisional_limit_x"`
	OfficialSnapshotObservedAt     *time.Time `json:"official_snapshot_observed_at,omitempty"`
	EstimatedTokensPerX            int64      `json:"estimated_tokens_per_x"`
	CalibrationSamples             int64      `json:"calibration_samples"`
	CalibrationSource              string     `json:"calibration_source"`
	CalibrationObservedAt          *time.Time `json:"calibration_observed_at,omitempty"`
}

// QuotaDebugDecisionLog is a compact, redacted tail of the existing decision
// log. It makes the diagnostic useful without exposing a full account ID.
type QuotaDebugDecisionLog struct {
	RequestedAt   time.Time `json:"requested_at"`
	AccountSuffix string    `json:"account_suffix"`
	Model         string    `json:"model"`
	Decision      string    `json:"decision"`
	StatusCode    int       `json:"status_code"`
	Reason        string    `json:"reason"`
	Units         int64     `json:"units"`
}

func (engine *Engine) windowSnapshot(state *keyMeterState, now time.Time, capacity int64, multiplier float64, window time.Duration, fiveHour bool) WindowSnapshot {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.completed.prune(now)
	state.reservations.prune(now)
	completed, completedIndex := state.completed.weeklyUnits, 0
	reserved, reservedIndex := state.reservations.weeklyUnits, 0
	if fiveHour {
		completed, completedIndex = state.completed.fiveUnits, state.completed.fiveStart
		reserved, reservedIndex = state.reservations.fiveUnits, state.reservations.fiveStart
	}
	used := completed + reserved
	remaining := capacity - used
	if remaining < 0 {
		remaining = 0
	}
	result := WindowSnapshot{Capacity: capacity, Used: used, Completed: completed, Reserved: reserved, Remaining: remaining, Multiplier: multiplier}
	for _, meter := range []struct {
		state *meterState
		index int
	}{{state.completed, completedIndex}, {state.reservations, reservedIndex}} {
		if meter.index >= len(meter.state.events) {
			continue
		}
		reset := meter.state.events[meter.index].At.Add(window).UTC()
		if result.ResetAt == nil || reset.Before(*result.ResetAt) {
			result.ResetAt = &reset
		}
	}
	return result
}

func (engine *Engine) requestUnits() int64 {
	engine.configMu.RLock()
	defer engine.configMu.RUnlock()
	return engine.config.RequestUnits
}

func (engine *Engine) allocationWindowSnapshot(policy KeyPolicy, requestUnits int64, now time.Time) WindowSnapshot {
	policyCapacity := engine.globalAllocationCapacity(policy, requestUnits)
	result := WindowSnapshot{Multiplier: policy.AllocationX, PolicyCapacity: policyCapacity}
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	result.Capacity, result.Used, result.ResetAt = engine.globalAllocationStateLocked(policy.ID, now, policyCapacity)
	result.Completed = 0
	result.Provisional = 0
	result.Reserved = 0
	cutoff := now.UTC().UnixMilli()
	for cycleKey, cycle := range engine.allocationCycles {
		if cycleKey.KeyID != policy.ID || cycleKey.WindowResetAt <= cutoff {
			continue
		}
		result.Completed += cycle.Completed
		result.Provisional += cycle.Provisional
		result.Reserved += cycle.Reserved
	}
	if result.Capacity > policyCapacity {
		// An old global capacity or a pre-upgrade account-shard ledger remains
		// effective until its official reset. New admissions never add capacities
		// together across accounts.
		result.HasDeferredDecrease = true
	}
	result.Remaining = result.Capacity - result.Used
	if result.Remaining < 0 {
		result.Remaining = 0
	}
	result.CapacityX = float64(result.Capacity) / float64(officialXUnitsPerX)
	result.PolicyCapacityX = float64(result.PolicyCapacity) / float64(officialXUnitsPerX)
	result.UsedX = float64(result.Used) / float64(officialXUnitsPerX)
	result.ConfirmedX = float64(result.Completed) / float64(officialXUnitsPerX)
	result.ProvisionalX = float64(result.Provisional) / float64(officialXUnitsPerX)
	result.ReservedX = float64(result.Reserved) / float64(officialXUnitsPerX)
	result.RemainingX = float64(result.Remaining) / float64(officialXUnitsPerX)
	result.MeteringMode = "official_calibrated_actual_token_x_with_bounded_provisional_guard"
	return result
}

// QuotaDebug returns a bounded, read-only explanation of the official x guard
// and its Token attribution evidence for one managed Key. It runs only from
// the management panel and never touches the scheduler hot path or makes an
// upstream CPA request.
func (engine *Engine) QuotaDebug(keyID string, now time.Time) (QuotaDebugSnapshot, error) {
	if engine == nil {
		return QuotaDebugSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return QuotaDebugSnapshot{}, fmt.Errorf("key_id is required")
	}
	if err := engine.flushPending(); err != nil {
		return QuotaDebugSnapshot{}, err
	}
	now = now.UTC()
	engine.configMu.RLock()
	requestUnits := engine.config.RequestUnits
	engine.configMu.RUnlock()
	engine.policiesMu.RLock()
	policy, exists := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !exists {
		return QuotaDebugSnapshot{}, fmt.Errorf("managed Key policy %q was not found", keyID)
	}

	type accountDebugInput struct {
		entry    AccountPoolEntry
		snapshot OfficialQuotaSnapshot
		hasQuota bool
	}
	engine.poolMu.RLock()
	accounts := make([]accountDebugInput, 0, len(engine.accountPool))
	for _, entry := range engine.accountPool {
		item := accountDebugInput{entry: entry}
		if snapshot, found := engine.quotaSnapshots[entry.AuthID]; found {
			item.snapshot, item.hasQuota = snapshot, true
		}
		accounts = append(accounts, item)
	}
	engine.poolMu.RUnlock()
	sort.Slice(accounts, func(left, right int) bool {
		return strings.ToLower(accounts[left].entry.Name) < strings.ToLower(accounts[right].entry.Name)
	})

	cycles := make(map[allocationCycleKey]allocationCycleState)
	engine.allocationMu.Lock()
	for cycleKey, cycle := range engine.allocationCycles {
		if cycleKey.KeyID == keyID && cycleKey.WindowResetAt > now.UnixMilli() {
			cycles[cycleKey] = cycle
		}
	}
	engine.allocationMu.Unlock()

	poolTokensPerX := engine.poolTokensPerX(requestUnits)
	result := QuotaDebugSnapshot{
		GeneratedAt: now,
		Key: QuotaDebugKey{
			ID: keyID, Name: policy.Name, AllocationX: policy.AllocationX,
		},
		Formula: QuotaDebugFormula{
			Description:         "每个 Key 使用一份全局 x 余额；官方周百分比只用于校准 Token/x 和保护账号总池，不直接扣到 Key；完成回调先形成有上限的待确认值，后续官方轮询按该 Key 在当前官方窗口内的实际 Token 重建计量。",
			RequestReservationX: float64(admissionReservationUnits) / float64(officialXUnitsPerX),
			FixedPointUnitsPerX: officialXUnitsPerX,
			// Deprecated Token-named fields remain for older diagnostic
			// consumers. Actual Key Tokens are converted with the official
			// calibration; a bounded provisional guard protects poll intervals.
			RequestReservationTokens: admissionReservationUnits,
			BaseRequestsPerX:         defaultSevenDayBaseRequests,
			PerXCapacityTokens:       poolTokensPerX,
			ProvisionalPercentCap:    officialProvisionalPercentCap,
			CalibrationMethod:        "aligned_official_percent_delta_with_completed_account_tokens",
			EnforcementMethod:        "official_calibrated_actual_token_x_plus_bounded_provisional_guard",
		},
		Allocation: engine.allocationWindowSnapshot(policy, requestUnits, now),
		Accounts:   make([]QuotaDebugAccountWindow, 0, len(accounts)),
	}
	for index, account := range accounts {
		entry := account.entry
		item := QuotaDebugAccountWindow{
			AccountLabel:           fmt.Sprintf("账号 %d", index+1),
			AccountSuffix:          quotaDebugAccountSuffix(entry.AuthID),
			AccountCapacityX:       entry.CapacityX,
			KeyAllocationX:         policy.AllocationX,
			ProvisionalLimitX:      float64(engine.provisionalXLimit(entry.AuthID)) / float64(officialXUnitsPerX),
			KeyCapacity:            result.Allocation.Capacity,
			KeyPolicyCapacity:      result.Allocation.PolicyCapacity,
			KeyHasDeferredDecrease: result.Allocation.HasDeferredDecrease,
			KeyUsed:                result.Allocation.Used,
			KeyRemaining:           result.Allocation.Remaining,
			Eligibility:            "disabled",
		}
		if !entry.Enabled || entry.CapacityX <= 0 {
			result.Accounts = append(result.Accounts, item)
			continue
		}
		calibration := engine.quotaCalibrationView(entry.AuthID, requestUnits)
		item.EstimatedTokensPerX = calibration.TokensPerX
		item.CalibrationSamples = calibration.Samples
		item.CalibrationSource = calibration.Source
		if !calibration.ObservedAt.IsZero() {
			observedAt := calibration.ObservedAt.UTC()
			item.CalibrationObservedAt = &observedAt
		}
		if !account.hasQuota {
			item.Eligibility = "official_snapshot_missing"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		snapshot := account.snapshot
		if !snapshot.ObservedAt.IsZero() {
			observedAt := snapshot.ObservedAt.UTC()
			item.OfficialSnapshotObservedAt = &observedAt
		}
		usedPercent := normalizeUsedPercent(snapshot.Secondary.UsedPercent)
		remainingPercent := 100 - usedPercent
		item.OfficialWeeklyUsedPercent = &usedPercent
		item.OfficialWeeklyRemainingPercent = &remainingPercent
		if snapshot.LastError != "" {
			item.Eligibility = "official_snapshot_error"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		if snapshot.hasPendingEstimatedSecondaryReset() {
			item.Eligibility = "weekly_reset_confirmation_pending"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		if !snapshot.Allowed || snapshot.LimitReached {
			item.Eligibility = "official_weekly_quota_exhausted"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		if !snapshot.usableAt(now) {
			item.Eligibility = "official_snapshot_stale"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		resetAt, hasReset := officialWeeklyResetAt(snapshot.Secondary, snapshot.ObservedAt, now)
		if !hasReset {
			item.Eligibility = "official_weekly_reset_unknown"
			result.Accounts = append(result.Accounts, item)
			continue
		}
		resetAt = resetAt.UTC()
		item.WindowResetAt = &resetAt
		for _, target := range engine.officialAccountWindowTargets(entry, snapshot, requestUnits, now) {
			item.OfficialWeeklyGuardTokens += target.Capacity
		}
		item.OfficialWeeklyGuardX = float64(item.OfficialWeeklyGuardTokens) / float64(officialXUnitsPerX)
		item.Eligible = true
		item.Eligibility = "eligible"
		if cycle, found := cycles[allocationCycleKey{KeyID: keyID, AuthID: entry.AuthID, WindowResetAt: resetAt.UnixMilli()}]; found {
			item.AccountCompleted = cycle.Completed
			item.AccountProvisional = cycle.Provisional
			item.AccountReserved = cycle.Reserved
		}
		result.Accounts = append(result.Accounts, item)
	}

	logs, _, err := engine.store.ListDecisionLogsPage(keyID, "", "", 50, 0)
	if err != nil {
		return QuotaDebugSnapshot{}, err
	}
	result.RecentLogs = make([]QuotaDebugDecisionLog, 0, len(logs))
	for _, log := range logs {
		result.RecentLogs = append(result.RecentLogs, QuotaDebugDecisionLog{
			RequestedAt: log.RequestedAt.UTC(), AccountSuffix: quotaDebugAccountSuffix(log.AuthID),
			Model: log.Model, Decision: log.Decision, StatusCode: log.StatusCode, Reason: log.Reason, Units: log.Units,
		})
	}
	return result, nil
}

func quotaDebugAccountSuffix(authID string) string {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	const visible = 4
	if len(authID) <= visible {
		return "••••" + authID
	}
	return "••••" + authID[len(authID)-visible:]
}

// Summary is read-only and never queries SQLite per request. It exposes only
// configured policies; CPA Keys without a policy remain intentionally absent
// from enforcement and continue through the normal CPA scheduler.
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
			ID:                policy.ID,
			Name:              policy.Name,
			FingerprintPrefix: fingerprint,
			Enabled:           policy.Enabled,
			AllowedModels:     append([]string(nil), policy.AllowedModels...),
			NeedsRebind:       policy.NeedsRebind,
			AllocationX:       policy.AllocationX,
			AccessRules:       append([]AccessRule(nil), policy.AccessRules...),
			AccessTimezone:    policy.AccessTimezone,
			Allocation:        engine.allocationWindowSnapshot(policy, cfg.RequestUnits, now),
		})
	}
	accounts := engine.AccountPool(now)
	poolCapacity, allocated := engine.PoolAllocation()
	return SummarySnapshot{Status: StatusSnapshot{
		Provider: cfg.Provider, MeteringMode: "official_calibrated_actual_token_x_with_bounded_provisional_guard", RequestUnits: cfg.RequestUnits,
		Configured: len(keys) > 0, PersistenceDegraded: engine.persistenceDegraded.Load() || engine.allocationDegraded.Load(), AccountSourceConflict: engine.accountSourceConflict.Load(),
		AnalysisReaderDegraded: engine.AnalysisReaderDegraded(),
		PersistenceFailures:    engine.persistenceFailures.Load(), RetentionFailures: engine.retentionFailures.Load(),
		PendingLogs: engine.pendingLogCount.Load(), DroppedDecisionLogs: engine.droppedDecisionLogs.Load(),
		PoolCapacityX: poolCapacity, AllocatedX: allocated,
	}, Keys: keys, Accounts: accounts}
}

// SummaryWithActualTokens enriches the request-path-free Summary with one
// bounded management-plane query for every Key. It intentionally avoids one
// SQLite query per row when the panel contains many managed Keys.
func (engine *Engine) SummaryWithActualTokens(now time.Time) (SummarySnapshot, error) {
	summary := engine.Summary(now)
	cycleStarts := make(map[string]*time.Time, len(summary.Keys))
	for index := range summary.Keys {
		key := &summary.Keys[index]
		if key.Allocation.ResetAt == nil {
			cycleStarts[key.ID] = nil
			continue
		}
		cycleStart := key.Allocation.ResetAt.UTC().Add(-sevenDayWindow)
		cycleStarts[key.ID] = &cycleStart
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageAnalysisQueryTimeout)
	defer cancel()
	totals, err := engine.store.LoadKeyActualTokenTotals(ctx, cycleStarts)
	if err != nil {
		return SummarySnapshot{}, err
	}
	for index := range summary.Keys {
		key := &summary.Keys[index]
		total := totals[key.ID]
		key.ActualTokens = ActualTokenSnapshot{
			Available:  true,
			Cycle:      total.Cycle,
			CycleKnown: cycleStarts[key.ID] != nil,
			Total:      total.Total,
		}
	}
	return summary, nil
}

// AccountPool returns the plugin-owned configuration joined with the last
// official quota snapshot. It never performs an upstream request, so panel
// rendering and scheduler admission stay independent from CPA's proxy path.
func (engine *Engine) AccountPool(now time.Time) []AccountPoolSnapshot {
	engine.poolMu.RLock()
	defer engine.poolMu.RUnlock()
	items := make([]AccountPoolSnapshot, 0, len(engine.accountPool))
	for _, entry := range engine.accountPool {
		item := AccountPoolSnapshot{AccountPoolEntry: entry}
		if quota, ok := engine.quotaSnapshots[entry.AuthID]; ok {
			copy := quota
			item.Quota = &copy
			item.Fresh = quota.freshAt(now)
		}
		items = append(items, item)
	}
	sort.Slice(items, func(left, right int) bool {
		return strings.ToLower(items[left].Name) < strings.ToLower(items[right].Name)
	})
	return items
}

func (engine *Engine) PoolAllocation() (float64, float64) {
	activeLedgers := engine.activeAllocationLedgerKeys(time.Now().UTC())
	engine.poolMu.RLock()
	var capacity float64
	for _, entry := range engine.accountPool {
		if entry.Enabled {
			capacity += entry.CapacityX
		}
	}
	engine.poolMu.RUnlock()
	engine.policiesMu.RLock()
	var allocated float64
	for _, policy := range engine.policiesByID {
		if policyConsumesPoolAllocation(policy, activeLedgers) {
			allocated += policy.AllocationX
		}
	}
	engine.policiesMu.RUnlock()
	return capacity, allocated
}

// policyConsumesPoolAllocation keeps a disabled Key's allocation reserved
// until its existing official windows reset. Disabling stops new routing, but
// it must not make historical consumption available to another Key mid-cycle.
func policyConsumesPoolAllocation(policy KeyPolicy, activeLedgers map[string]struct{}) bool {
	if _, active := activeLedgers[policy.ID]; active {
		return true
	}
	return policy.Enabled && !policy.NeedsRebind
}

// validatePolicySetAgainstPool is shared by the panel, startup bootstrap, and
// dormant host reconfiguration paths. Every way a policy can enter memory
// must preserve the same shared-pool x invariant as a panel save.
func validatePolicySetAgainstPool(policies map[string]KeyPolicy, pool map[string]AccountPoolEntry, activeLedgers map[string]struct{}) error {
	for keyID := range activeLedgers {
		if _, exists := policies[keyID]; !exists {
			return fmt.Errorf("active official weekly allocation for removed Key policy %q cannot be reconstructed; wait for the official reset before changing the policy set", keyID)
		}
	}
	var capacity float64
	for _, entry := range pool {
		if entry.Enabled {
			capacity += entry.CapacityX
		}
	}
	if capacity == 0 {
		return nil // Allow first-time setup; routing stays unavailable until configured.
	}
	var allocated float64
	for _, policy := range policies {
		if policyConsumesPoolAllocation(policy, activeLedgers) {
			allocated += policy.AllocationX
		}
	}
	if allocated > capacity+0.000001 {
		return fmt.Errorf("Key allocations %.2fx exceed the enabled account pool %.2fx", allocated, capacity)
	}
	return nil
}

func accountPoolRoutingChanged(current, next AccountPoolEntry) bool {
	return current.Enabled != next.Enabled ||
		strings.TrimSpace(current.AuthIndex) != strings.TrimSpace(next.AuthIndex) ||
		math.Abs(current.CapacityX-next.CapacityX) > 0.000001
}

func validatePoolSchedulerAliases(pool map[string]AccountPoolEntry) error {
	seen := make(map[string]string, len(pool))
	for authID, entry := range pool {
		if !entry.Enabled {
			continue
		}
		alias := strings.TrimSpace(entry.AuthIndex)
		if alias == "" || alias == authID {
			continue
		}
		if existing, exists := seen[alias]; exists && existing != authID {
			return fmt.Errorf("CPA scheduler auth_index %q is already configured as %q", alias, existing)
		}
		seen[alias] = authID
	}
	return nil
}

func (engine *Engine) UpsertAccountPoolEntry(entry AccountPoolEntry) (AccountPoolEntry, error) {
	entries, err := engine.UpsertAccountPoolEntries([]AccountPoolEntry{entry})
	if err != nil {
		return AccountPoolEntry{}, err
	}
	return entries[0], nil
}

// UpsertAccountPoolEntries atomically applies a selected set of CPA accounts.
// It is used by the bulk account dialog so policy-capacity validation, SQLite,
// and the in-memory scheduler state all move together.
func (engine *Engine) UpsertAccountPoolEntries(entries []AccountPoolEntry) ([]AccountPoolEntry, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("account pool entries are required")
	}
	normalized := make([]AccountPoolEntry, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry, err := normalizeAccountPoolEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[entry.AuthID]; exists {
			return nil, fmt.Errorf("account pool repeats auth_id %q", entry.AuthID)
		}
		seen[entry.AuthID] = struct{}{}
		normalized = append(normalized, entry)
	}
	engine.officialQuotaMu.Lock()
	defer engine.officialQuotaMu.Unlock()
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.poolMu.RLock()
	current := make(map[string]AccountPoolEntry, len(engine.accountPool))
	for authID, entry := range engine.accountPool {
		current[authID] = entry
	}
	engine.poolMu.RUnlock()
	for _, entry := range normalized {
		existing, exists := current[entry.AuthID]
		if !exists || accountPoolRoutingChanged(existing, entry) {
			if engine.hasActiveAllocationLedger(time.Now().UTC()) {
				return nil, fmt.Errorf("account pool membership, capacity_x, and enabled state cannot change until all active official weekly allocation windows reset")
			}
			break
		}
	}
	engine.poolMu.Lock()
	next := make(map[string]AccountPoolEntry, len(current)+len(normalized))
	for authID, entry := range current {
		next[authID] = entry
	}
	now := time.Now().UTC()
	for index := range normalized {
		normalized[index].UpdatedAt = now
		next[normalized[index].AuthID] = normalized[index]
	}
	if err := validatePoolSchedulerAliases(next); err != nil {
		engine.poolMu.Unlock()
		return nil, err
	}
	if err := engine.validatePoolAllocation(next); err != nil {
		engine.poolMu.Unlock()
		return nil, err
	}
	if err := engine.store.UpsertAccountPoolEntries(normalized); err != nil {
		engine.poolMu.Unlock()
		return nil, err
	}
	engine.accountPool = next
	engine.poolMu.Unlock()
	engine.bumpAccountSourceRevision()
	// Capacity changes are an immediate scheduler boundary. Re-base the local
	// account-window ledger from the latest official snapshot before returning
	// so a reduced x value cannot keep the previous larger allowance alive.
	engine.rebuildOfficialAccountWindows(time.Now().UTC())
	return normalized, nil
}

func (engine *Engine) DeleteAccountPoolEntry(authID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return fmt.Errorf("auth_id is required")
	}
	engine.officialQuotaMu.Lock()
	defer engine.officialQuotaMu.Unlock()
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	if pending := engine.pendingSettlementsForAccount(authID); pending > 0 {
		return fmt.Errorf("cannot remove account %q while %d admitted request(s) are awaiting terminal usage", authID, pending)
	}
	// Removing one account changes every remaining Key share. Waiting for the
	// official weekly ledgers to finish is the only plugin-only way to avoid
	// granting a new share while the old share is still charged.
	if engine.hasActiveAllocationLedger(time.Now().UTC()) {
		return fmt.Errorf("cannot remove account %q until all active official weekly allocation windows reset", authID)
	}
	engine.poolMu.Lock()
	if _, exists := engine.accountPool[authID]; !exists {
		engine.poolMu.Unlock()
		return fmt.Errorf("account pool entry %q was not found", authID)
	}
	next := make(map[string]AccountPoolEntry, len(engine.accountPool)-1)
	for id, entry := range engine.accountPool {
		if id != authID {
			next[id] = entry
		}
	}
	if err := engine.validatePoolAllocation(next); err != nil {
		engine.poolMu.Unlock()
		return err
	}
	if err := engine.flushAllocationPersistence(); err != nil {
		engine.poolMu.Unlock()
		return fmt.Errorf("flush account allocation before delete: %w", err)
	}
	if err := engine.store.DeleteAccountPoolEntry(authID); err != nil {
		engine.poolMu.Unlock()
		return err
	}
	// Keep the hidden snapshot while this account is absent from the pool. If
	// the same CPA identity is re-added before its official reset, rebuilding
	// from this baseline and the durable buckets must not create fresh shared
	// account allowance.
	engine.accountPool = next
	engine.poolMu.Unlock()
	engine.bumpAccountSourceRevision()
	engine.rebuildOfficialAccountWindows(time.Now().UTC())
	return nil
}

func (engine *Engine) UpdateOfficialQuota(snapshot OfficialQuotaSnapshot) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	// Keep the upstream fact that reset_at was omitted before normalization
	// derives a local fallback. The derived marker is persisted, but this raw
	// fact is the authoritative way to decide whether a newly elapsed week
	// needs the conservative two-poll confirmation.
	secondaryResetWasOmitted := snapshot.Secondary.ResetAt == nil && snapshot.Secondary.LimitWindowSeconds > 0
	snapshot = normalizeOfficialQuotaSnapshot(snapshot)
	if secondaryResetWasOmitted {
		// Preserve the upstream omission even if a future normalization change
		// supplies a fallback timestamp before it marks the snapshot estimated.
		snapshot.Secondary.ResetEstimated = true
	}
	if snapshot.AuthID == "" {
		return fmt.Errorf("auth_id is required")
	}
	// A normal official refresh must not hold the admission lock while SQLite
	// persists the snapshot. Account changes take this same lock order, so the
	// later in-memory reconciliation can still form a short admission boundary.
	engine.officialQuotaMu.Lock()
	defer engine.officialQuotaMu.Unlock()
	engine.poolMu.RLock()
	entry, exists := engine.accountPool[snapshot.AuthID]
	previous, hasPrevious := engine.quotaSnapshots[snapshot.AuthID]
	engine.poolMu.RUnlock()
	if !exists {
		return fmt.Errorf("account %q is not in the shared pool", snapshot.AuthID)
	}
	elapsedInferredSecondary := false
	var resetAt time.Time
	var resetKind string
	if hasPrevious {
		// Stabilization preserves a live inferred reset identity across ordinary
		// polling jitter. An omitted upstream reset after the prior identity has
		// elapsed is always a tentative new week; decide that from the raw input
		// before stabilization can retain the older fallback timestamp.
		rawSecondary := snapshot.Secondary
		elapsedInferredSecondary = secondaryResetWasOmitted &&
			previous.Secondary.ResetAt != nil &&
			snapshot.Secondary.ResetAt != nil &&
			(previous.Secondary.LimitWindowSeconds <= 0 || snapshot.Secondary.LimitWindowSeconds <= 0 || previous.Secondary.LimitWindowSeconds == snapshot.Secondary.LimitWindowSeconds) &&
			!previous.Secondary.ResetAt.After(snapshot.ObservedAt) &&
			snapshot.Secondary.ResetAt.After(*previous.Secondary.ResetAt)
		// reset_after_seconds is estimated, but a jump to a materially later
		// identity together with a lower official percentage is still evidence
		// that Codex refreshed the account early. Require the same durable
		// two-poll confirmation used for other inferred resets before releasing
		// the Key ledger.
		earlyInferredSecondary := snapshot.Secondary.ResetEstimated &&
			previous.Secondary.ResetAt != nil &&
			snapshot.Secondary.ResetAt != nil &&
			(previous.Secondary.LimitWindowSeconds <= 0 || snapshot.Secondary.LimitWindowSeconds <= 0 || previous.Secondary.LimitWindowSeconds == snapshot.Secondary.LimitWindowSeconds) &&
			previous.Secondary.ResetAt.After(snapshot.ObservedAt) &&
			snapshot.Secondary.ResetAt.After(previous.Secondary.ResetAt.Add(quotaResetStabilityTolerance)) &&
			snapshot.Secondary.UsedPercent+0.000001 < previous.Secondary.UsedPercent
		candidateAt := time.Time{}
		if elapsedInferredSecondary || earlyInferredSecondary {
			candidateAt = previous.Secondary.ResetAt.UTC()
		}
		snapshot = stabilizeOfficialQuotaSnapshot(previous, snapshot, snapshot.ObservedAt)
		// Do not use := here: this block must update the outer snapshot that is
		// later persisted and published to admissions. A short declaration would
		// shadow it and silently discard an estimated-reset candidate.
		snapshot, resetAt, resetKind = reconcileSecondaryReset(previous, snapshot)
		if (elapsedInferredSecondary || earlyInferredSecondary) && !snapshot.hasPendingEstimatedSecondaryReset() && resetAt.IsZero() {
			seenAt := snapshot.ObservedAt.UTC()
			snapshot.SecondaryEstimatedResetCandidateAt = &candidateAt
			snapshot.SecondaryEstimatedResetCandidateSeenAt = &seenAt
			// reset_at was absent upstream, so a local fallback must not release
			// the old ledger before this candidate has a second observation.
			resetAt = time.Time{}
			resetKind = ""
		}
		if earlyInferredSecondary && resetKind == "estimated_confirmed" {
			// Stabilization intentionally kept the still-live old inferred identity
			// while confirmation was pending. Once confirmed, publish the raw new
			// reset identity so reconciliation starts a genuinely fresh Key cycle.
			snapshot.Secondary = rawSecondary
		}
		if !resetAt.IsZero() {
			// Expire durable old-window reservations before persisting the new
			// identity. If the release fails, the saved prior snapshot keeps the
			// reconciliation retryable instead of silently stranding a reservation.
			engine.adminMu.Lock()
			expired, err := engine.expireAllocationReservationsAtOfficialReset(snapshot.AuthID, resetAt)
			engine.adminMu.Unlock()
			if err != nil {
				return err
			}
			if len(expired) > 0 {
				released := engine.expirePendingSettlements(expired)
				for _, item := range expired {
					engine.enqueueDecision(DecisionLog{
						KeyID: item.Key.KeyID, AuthID: item.Key.AuthID, RequestedAt: time.UnixMilli(item.Key.BucketAt).UTC(),
						Decision: "expired", Reason: "reservation_expired_at_official_reset", Units: item.Units,
					})
				}
				message := "官方周额度账期已刷新，已终结 %d 个未回调用量预留（释放 %d 个本进程结算等待）"
				if resetKind == "estimated_confirmed" {
					message = "官方周额度账期推算已连续确认，已终结 %d 个未回调用量预留（释放 %d 个本进程结算等待）"
				}
				engine.LogOperational("warn", "reservation_expired_at_official_reset", fmt.Sprintf(message, len(expired), released), snapshot.AuthID, "")
			}
		}
	}
	_, err := engine.reconcileOfficialX(entry, snapshot)
	if err != nil {
		// Do not publish a newer snapshot unless its Token-derived Key ledger and
		// official calibration watermark are durably reconciled.
		return fmt.Errorf("reconcile official x usage: %w", err)
	}
	if err := engine.store.UpsertOfficialQuotaSnapshot(snapshot); err != nil {
		return err
	}
	// The SQLite write above is the expensive part of a normal refresh. Only
	// hold adminMu for the memory handoff that admissions must see atomically.
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.poolMu.Lock()
	// Account edits and deletion share officialQuotaMu, so this is defensive
	// only; keeping it makes the update safe even if a future caller bypasses
	// the panel mutation path.
	if current, present := engine.accountPool[snapshot.AuthID]; !present {
		engine.poolMu.Unlock()
		return fmt.Errorf("account %q was removed while updating its quota", snapshot.AuthID)
	} else {
		entry = current
	}
	engine.quotaSnapshots[snapshot.AuthID] = snapshot
	engine.poolMu.Unlock()
	if snapshot.LastError == "" {
		engine.configMu.RLock()
		requestUnits := engine.config.RequestUnits
		engine.configMu.RUnlock()
		engine.allocationMu.Lock()
		engine.replaceOfficialAccountWindowsLocked(entry, snapshot, requestUnits, snapshot.ObservedAt)
		engine.allocationMu.Unlock()
	}
	return nil
}

func (engine *Engine) validatePoolAllocation(pool map[string]AccountPoolEntry) error {
	engine.policiesMu.RLock()
	defer engine.policiesMu.RUnlock()
	activeLedgers := engine.activeAllocationLedgerKeys(time.Now().UTC())
	return validatePolicySetAgainstPool(engine.policiesByID, pool, activeLedgers)
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

// UpsertPolicy never stores a raw CPA Key. New policies receive a one-time
// raw value from CPA's existing API Key selector; later edits keep its HMAC.
func (engine *Engine) UpsertPolicy(policy KeyPolicy, rawAPIKey string) (KeyPolicy, error) {
	if engine == nil {
		return KeyPolicy{}, fmt.Errorf("codex-carpool is not initialized")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	rawAPIKey = strings.TrimSpace(rawAPIKey)
	engine.policiesMu.Lock()
	existing, exists := engine.policiesByID[policy.ID]
	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	if rawAPIKey != "" {
		policy.KeySHA256 = FingerprintAPIKey(rawAPIKey, cfg.KeyHMACSecret)
		policy.FingerprintScheme = hmacFingerprintScheme
	} else if exists {
		policy.KeySHA256 = existing.KeySHA256
		policy.FingerprintScheme = existing.FingerprintScheme
	}
	validated, err := normalizePolicy(policy, nil, cfg.RequestUnits)
	if err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, err
	}
	modelCatalog, err := engine.store.ListModelCatalog()
	if err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, fmt.Errorf("load synchronized model catalog: %w", err)
	}
	if err := validateAllowedModels(validated.AllowedModels, modelCatalog); err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, err
	}
	if exists && existing.KeySHA256 != validated.KeySHA256 {
		if pending := engine.pendingSettlementsForKey(existing.ID); pending > 0 {
			engine.policiesMu.Unlock()
			return KeyPolicy{}, fmt.Errorf("cannot rebind Key policy %q while %d admitted request(s) are awaiting terminal usage", existing.ID, pending)
		}
	}
	if err := engine.validatePolicyAllocationLocked(validated); err != nil {
		engine.policiesMu.Unlock()
		return KeyPolicy{}, err
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
	return validated, nil
}

// validatePolicyAllocationLocked runs while policiesMu is held by UpsertPolicy
// and adminMu prevents concurrent account-pool changes.
func (engine *Engine) validatePolicyAllocationLocked(candidate KeyPolicy) error {
	engine.poolMu.RLock()
	var capacity float64
	for _, entry := range engine.accountPool {
		if entry.Enabled {
			capacity += entry.CapacityX
		}
	}
	engine.poolMu.RUnlock()
	if capacity == 0 {
		return nil
	}
	activeLedgers := engine.activeAllocationLedgerKeys(time.Now().UTC())
	candidateAllocation := candidate.AllocationX
	candidateConsumes := policyConsumesPoolAllocation(candidate, activeLedgers)
	if _, active := activeLedgers[candidate.ID]; active {
		// A policy edit is allowed during an active official window. Increasing x
		// is checked against the shared pool immediately; decreasing x is stored
		// for the next official window while the durable current-window ledger
		// continues to account for already admitted work.
		candidateConsumes = true
	}
	var allocated float64
	if candidateConsumes {
		allocated += candidateAllocation
	}
	for keyID, policy := range engine.policiesByID {
		if keyID != candidate.ID && policyConsumesPoolAllocation(policy, activeLedgers) {
			allocated += policy.AllocationX
		}
	}
	if allocated > capacity+0.000001 {
		return fmt.Errorf("Key allocation %.2fx exceeds the remaining shared pool %.2fx", candidateAllocation, capacity-(allocated-candidateAllocation))
	}
	return nil
}

// validateAllowedModels keeps the enforcement policy tied to the CPA-synced
// catalog. Empty means "allow all", which intentionally requires no catalog.
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
		return fmt.Errorf("synchronized CPA Codex model catalog is empty; sync models before applying a restriction")
	}
	for _, model := range allowed {
		if _, exists := known[model]; !exists {
			return fmt.Errorf("model %q is not in the synchronized CPA Codex model catalog; sync models and retry", model)
		}
	}
	return nil
}

// ResetPolicyUsage keeps the Key policy and audit logs intact while
// establishing a new local accounting boundary. Codex account snapshots are
// not reset because the upstream account has already consumed that quota.
func (engine *Engine) ResetPolicyUsage(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("key_id is required")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.policiesMu.RLock()
	_, exists := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !exists {
		return fmt.Errorf("Key policy %q was not found", keyID)
	}
	// Persist every already-queued event before resetting only the accounting
	// rows. The retained decision log therefore still describes pre-reset usage.
	// Admissions and terminal callbacks wait on adminMu while the reset runs.
	// Requests admitted before this explicit administrative boundary belong to
	// the discarded history even if their terminal callback arrives later.
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush Key usage before reset: %w", err)
	}
	if err := engine.flushAllocationPersistence(); err != nil {
		return fmt.Errorf("flush Key allocation before reset: %w", err)
	}
	if err := engine.store.ResetPolicyUsage(keyID); err != nil {
		return err
	}
	engine.discardKeyAccounting(keyID)
	return nil
}

// DeletePolicy removes a Key and all plugin-owned per-Key accounting. A later
// re-add is an explicit fresh allocation; the underlying Codex account's
// official quota snapshot remains untouched.
func (engine *Engine) DeletePolicy(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("key_id is required")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	engine.policiesMu.RLock()
	policy, exists := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	if !exists {
		return fmt.Errorf("Key policy %q was not found", keyID)
	}
	// Close the in-memory-to-SQLite gap before the reset transaction. adminMu
	// excludes new admissions and terminal callbacks for the duration, so no
	// deleted history can be queued again behind this boundary.
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush Key usage before delete: %w", err)
	}
	if err := engine.flushAllocationPersistence(); err != nil {
		return fmt.Errorf("flush Key allocation before delete: %w", err)
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

type InstallationSnapshot struct {
	Settings InstallationSettings `json:"settings"`
}

func (engine *Engine) Installation() InstallationSnapshot {
	engine.configMu.RLock()
	defer engine.configMu.RUnlock()
	return InstallationSnapshot{Settings: InstallationSettings{
		RequestUnits:    engine.config.RequestUnits,
		RecordRetention: engine.config.RecordRetention,
		AuthDirectory:   engine.config.AuthDirectory,
	}}
}

// AuthDirectory returns the configured, read-only CPA auth directory. The
// synchronizer resolves it at refresh time so an administrator can switch CPA
// storage without restarting or retaining credentials from the old directory.
func (engine *Engine) AuthDirectory() string {
	if engine == nil {
		return ""
	}
	engine.configMu.RLock()
	defer engine.configMu.RUnlock()
	return engine.config.AuthDirectory
}

// SetAccountSourceConflict controls a fail-closed guard for managed Keys when
// the file-backed synchronizer detects duplicate or unprovable enabled pool
// identities. A pending auth-dir verification cannot be cleared through this
// legacy helper; only PublishAccountSourceScan may reopen that boundary.
func (engine *Engine) SetAccountSourceConflict(conflict bool) {
	if engine == nil {
		return
	}
	engine.sourceGuardMu.Lock()
	if !conflict && engine.accountSourceVerificationRevision != 0 {
		engine.accountSourceConflict.Store(true)
	} else {
		engine.accountSourceConflict.Store(conflict)
	}
	engine.sourceGuardMu.Unlock()
}

// AccountSourceRevision is a lock-free configuration generation for the
// file-backed synchronizer. It deliberately exposes no source data, only the
// fact that a completed scan may have become stale while it was reading files.
func (engine *Engine) AccountSourceRevision() uint64 {
	if engine == nil {
		return 0
	}
	return engine.accountSourceRevision.Load()
}

// PublishAccountSourceScan applies one file-backed scan only if it inspected
// the current configuration generation. A stale or incomplete scan always
// leaves the guard closed; only a complete, conflict-free scan may reopen
// managed admissions.
func (engine *Engine) PublishAccountSourceScan(revision uint64, conflict, complete bool) bool {
	if engine == nil {
		return false
	}
	engine.sourceGuardMu.Lock()
	defer engine.sourceGuardMu.Unlock()
	current := engine.accountSourceRevision.Load()
	if revision != current {
		// An account-pool mutation can invalidate a scan even when the mutation
		// was synchronously validated. Keep the boundary conservative until the
		// forced follow-up scan publishes the current generation.
		engine.accountSourceVerificationRevision = current
		engine.accountSourceConflict.Store(true)
		return false
	}
	if !complete {
		// A transient JSON rewrite or unreadable sibling is still an unprovable
		// account-source state. Pause managed Keys until a later complete scan
		// can establish that one official Codex account is not counted twice.
		engine.accountSourceVerificationRevision = revision
		engine.accountSourceConflict.Store(true)
		return true
	}
	if complete && engine.accountSourceVerificationRevision == revision {
		engine.accountSourceVerificationRevision = 0
	}
	engine.accountSourceConflict.Store(conflict)
	return true
}

// bumpAccountSourceRevision invalidates an in-flight source scan after an
// account-pool mutation. Once the native file-backed integration enabled the
// source guard, every mutation pauses managed admissions until the current
// pool is completely revalidated.
func (engine *Engine) bumpAccountSourceRevision() {
	if engine == nil {
		return
	}
	engine.sourceGuardMu.Lock()
	revision := engine.accountSourceRevision.Add(1)
	if engine.accountSourceVerificationEnabled || engine.accountSourceVerificationRevision != 0 {
		engine.accountSourceVerificationRevision = revision
		engine.accountSourceConflict.Store(true)
	}
	engine.sourceGuardMu.Unlock()
}

// RequireAccountSourceVerification enables the file-backed source guard and
// closes managed admissions until one complete current-generation scan proves
// every enabled CPA auth source distinct. The native bridge calls it before
// exposing an Engine that uses CPA's auth-dir; plain Engine users remain
// opt-in because they may supply accounts through another trusted boundary.
func (engine *Engine) RequireAccountSourceVerification() {
	if engine == nil {
		return
	}
	engine.sourceGuardMu.Lock()
	revision := engine.accountSourceRevision.Add(1)
	engine.accountSourceVerificationEnabled = true
	engine.accountSourceVerificationRevision = revision
	engine.accountSourceConflict.Store(true)
	engine.sourceGuardMu.Unlock()
}

// markAccountSourceChanged closes managed admissions before a new auth-dir
// becomes visible to another scheduler callback. The synchronizer clears this
// guard only after a scan of the new generation has proven every enabled
// source distinct.
func (engine *Engine) markAccountSourceChanged() {
	if engine == nil {
		return
	}
	engine.sourceGuardMu.Lock()
	revision := engine.accountSourceRevision.Add(1)
	engine.accountSourceVerificationEnabled = true
	engine.accountSourceVerificationRevision = revision
	engine.accountSourceConflict.Store(true)
	engine.sourceGuardMu.Unlock()
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
	// Older API clients only send the pre-auth-dir fields. Preserve an existing
	// custom directory instead of silently reverting it to the default.
	if strings.TrimSpace(settings.AuthDirectory) == "" {
		settings.AuthDirectory = current.AuthDirectory
	}
	normalized, err := normalizeInstallationSettings(settings, current.KeyHMACSecret)
	if err != nil {
		return InstallationSnapshot{}, err
	}
	settings = normalized.InstallationSettings
	// request_units is now only a diagnostic/fallback scale: uncalibrated Token
	// totals do not consume x, while calibrated guards use the persisted official
	// account scale. It therefore needs only the same in-flight callback barrier
	// as host Reconfigure and must not be tied to an active official x ledger.
	if settings.RequestUnits != current.RequestUnits && engine.pendingSettlements.Load() > 0 {
		return InstallationSnapshot{}, fmt.Errorf("request_units cannot change until admitted requests settle")
	}
	if err := engine.flushPending(); err != nil {
		return InstallationSnapshot{}, fmt.Errorf("flush before updating settings: %w", err)
	}
	if _, err := engine.store.SaveInstallation(settings, current.KeyHMACSecret, nil); err != nil {
		return InstallationSnapshot{}, err
	}
	current.RequestUnits = settings.RequestUnits
	if current.RequestUnits <= 0 {
		current.RequestUnits = defaultRequestUnits
	}
	current.RecordRetention = settings.RecordRetention
	if current.RecordRetention == "" {
		current.RecordRetention = defaultRecordRetention
	}
	current.RecordRetentionDuration, _ = time.ParseDuration(current.RecordRetention)
	authDirectoryChanged := current.AuthDirectory != settings.AuthDirectory
	current.AuthDirectory = settings.AuthDirectory
	engine.configMu.Lock()
	engine.config = current
	engine.configMu.Unlock()
	if authDirectoryChanged {
		engine.markAccountSourceChanged()
	}
	return engine.Installation(), nil
}

func (engine *Engine) UsageRecords(keyID string, limit int) ([]UsageEvent, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	if err := engine.flushPending(); err != nil {
		return nil, err
	}
	return engine.store.ListUsageEvents(keyID, limit)
}

func (engine *Engine) UsageTrend(keyID string, now time.Time, window time.Duration, bins int) (UsageTrendSnapshot, error) {
	if engine == nil {
		return UsageTrendSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return UsageTrendSnapshot{}, fmt.Errorf("key_id is required")
	}
	if window != fiveHourWindow && window != sevenDayWindow {
		return UsageTrendSnapshot{}, fmt.Errorf("unsupported trend window")
	}
	if bins < 2 || bins > 48 {
		return UsageTrendSnapshot{}, fmt.Errorf("trend bins must be between 2 and 48")
	}
	if err := engine.flushPending(); err != nil {
		return UsageTrendSnapshot{}, err
	}
	now = now.UTC()
	bin := window / time.Duration(bins)
	from := now.Add(-window)
	points, err := engine.store.ListUsageTrend(keyID, from, bin)
	if err != nil {
		return UsageTrendSnapshot{}, err
	}
	return UsageTrendSnapshot{From: from, To: now, Points: points}, nil
}

// UsageAnalysis aggregates actual completed token usage by a local calendar
// interval. The date range is validated by the management API before it gets
// here; the additional checks keep the Engine safe for other callers too.
func (engine *Engine) UsageAnalysis(keyID string, from, until time.Time, location *time.Location, granularity string) (UsageAnalysisSnapshot, error) {
	if engine == nil {
		return UsageAnalysisSnapshot{}, fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return UsageAnalysisSnapshot{}, fmt.Errorf("key_id is required")
	}
	if location == nil {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis timezone is required")
	}
	if !until.After(from) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis end must be after start")
	}
	granularity = strings.ToLower(strings.TrimSpace(granularity))
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "hour" && granularity != "day" && granularity != "month" && granularity != "year" {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis granularity must be hour, day, month, or year")
	}
	firstDay := analysisPeriodStart(from.In(location), "day", location)
	lastDay := analysisPeriodStart(until.Add(-time.Nanosecond).In(location), "day", location)
	if lastDay.After(firstDay.AddDate(0, 0, 365)) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("analysis range must not exceed 366 days")
	}
	// Hourly charts are intentionally bounded so an accidental year-long
	// selection cannot create thousands of browser chart points.
	if granularity == "hour" && lastDay.After(firstDay.AddDate(0, 0, 30)) {
		return UsageAnalysisSnapshot{}, fmt.Errorf("hour granularity range must not exceed 31 days")
	}
	// Analysis is a management-only view. Do not force the global settlement
	// queue to drain just to render a chart: the normal worker persists it each
	// second, while quota admission and completion stay on their own path.
	if engine.store.RestoreAnalysisReader() {
		engine.LogOperational("info", "usage_analysis_reader_restored", "年度用量分析已恢复为独立 SQLite 只读连接", "", keyID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageAnalysisQueryTimeout)
	defer cancel()
	buckets, availableFrom, err := engine.store.LoadUsageAnalysisSnapshot(ctx, keyID, from, until)
	if err != nil {
		return UsageAnalysisSnapshot{}, err
	}

	from = from.UTC()
	until = until.UTC()
	points := make(map[time.Time]UsageAnalysisPoint)
	for _, bucket := range buckets {
		start := analysisPeriodStart(bucket.At.In(location), granularity, location)
		point := points[start]
		point.Start = start.UTC()
		point.Label = analysisPeriodLabel(start, granularity)
		point.Units += bucket.Units
		point.RequestCount += bucket.RequestCount
		points[start] = point
	}

	first := analysisPeriodStart(from.In(location), granularity, location)
	last := analysisPeriodStart(until.Add(-time.Nanosecond).In(location), granularity, location)
	result := UsageAnalysisSnapshot{
		From:          from,
		To:            until,
		Timezone:      location.String(),
		Granularity:   granularity,
		AvailableFrom: availableFrom,
		RetentionDays: int(usageAnalysisRetention / (24 * time.Hour)),
		Points:        make([]UsageAnalysisPoint, 0),
	}
	for start := first; !start.After(last); start = nextAnalysisPeriod(start, granularity, location) {
		point, ok := points[start]
		if !ok {
			point = UsageAnalysisPoint{Start: start.UTC(), Label: analysisPeriodLabel(start, granularity)}
		}
		result.Points = append(result.Points, point)
		result.TotalTokens += point.Units
		result.RequestCount += point.RequestCount
	}
	return result, nil
}

func analysisPeriodStart(value time.Time, granularity string, location *time.Location) time.Time {
	value = value.In(location)
	switch granularity {
	case "hour":
		// Subtract local clock components instead of UTC-truncating so zones
		// with half-hour offsets and repeated DST hours retain their identity.
		return value.Add(-time.Duration(value.Minute())*time.Minute -
			time.Duration(value.Second())*time.Second -
			time.Duration(value.Nanosecond()))
	case "year":
		return time.Date(value.Year(), time.January, 1, 0, 0, 0, 0, location)
	case "month":
		return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, location)
	default:
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, location)
	}
}

func nextAnalysisPeriod(value time.Time, granularity string, location *time.Location) time.Time {
	switch granularity {
	case "hour":
		return value.Add(time.Hour).In(location)
	case "year":
		return value.AddDate(1, 0, 0).In(location)
	case "month":
		return value.AddDate(0, 1, 0).In(location)
	default:
		return value.AddDate(0, 0, 1).In(location)
	}
}

func analysisPeriodLabel(value time.Time, granularity string) string {
	switch granularity {
	case "hour":
		return value.Format("2006-01-02 15:00")
	case "year":
		return value.Format("2006")
	case "month":
		return value.Format("2006-01")
	default:
		return value.Format("2006-01-02")
	}
}

func (engine *Engine) DecisionLogs(keyID string, limit int) ([]DecisionLog, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	if err := engine.flushPending(); err != nil {
		return nil, err
	}
	return engine.store.ListDecisionLogs(keyID, limit)
}

// DecisionLogPage filters and pages one Key's compact decision logs. Pending
// scheduler writes are flushed first, so an operator can see the latest
// allow/block or settlement event without loading unbounded history.
func (engine *Engine) DecisionLogPage(keyID, decision, search string, page, pageSize int) (DecisionLogPage, error) {
	if engine == nil {
		return DecisionLogPage{}, fmt.Errorf("codex-carpool is not initialized")
	}
	const (
		defaultPageSize = 20
		maxPageSize     = 100
		maxPage         = 1_000_000
	)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	} else if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if page <= 0 {
		page = 1
	} else if page > maxPage {
		page = maxPage
	}
	if err := engine.flushPending(); err != nil {
		return DecisionLogPage{}, err
	}
	items, total, err := engine.store.ListDecisionLogsPage(keyID, decision, search, pageSize, (page-1)*pageSize)
	if err != nil {
		return DecisionLogPage{}, err
	}
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
		if page > totalPages {
			page = totalPages
			items, total, err = engine.store.ListDecisionLogsPage(keyID, decision, search, pageSize, (page-1)*pageSize)
			if err != nil {
				return DecisionLogPage{}, err
			}
		}
	}
	return DecisionLogPage{Logs: items, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}, nil
}

// ClearDecisionLogs establishes an administrative boundary around the
// asynchronous decision queue, then clears only the selected Key's audit rows.
func (engine *Engine) ClearDecisionLogs(keyID string) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	keyID = strings.TrimSpace(keyID)
	if keyID == "" {
		return fmt.Errorf("key_id is required")
	}
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	if err := engine.flushPending(); err != nil {
		return fmt.Errorf("flush decision logs before clear: %w", err)
	}
	return engine.store.ClearDecisionLogs(keyID)
}

// OperationalLogs is intentionally separate from Key decision logs so the
// panel can show plugin failures and lifecycle events without mixing them into
// a customer's per-Key usage analysis.
func (engine *Engine) OperationalLogs(level, query string, limit int) ([]OperationalLog, error) {
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	return engine.store.ListOperationalLogs(level, query, limit)
}

// OperationalLogPage returns a bounded page of lifecycle and error logs. The
// page-size ceiling keeps the management endpoint predictable even when a
// broad search matches a long retention period.
func (engine *Engine) OperationalLogPage(level, query string, page, pageSize int) (OperationalLogPage, error) {
	if engine == nil {
		return OperationalLogPage{}, fmt.Errorf("codex-carpool is not initialized")
	}
	const (
		defaultPageSize = 20
		maxPageSize     = 100
		maxPage         = 1_000_000
	)
	if pageSize <= 0 {
		pageSize = defaultPageSize
	} else if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	if page <= 0 {
		page = 1
	} else if page > maxPage {
		page = maxPage
	}
	items, total, err := engine.store.ListOperationalLogsPage(level, query, pageSize, (page-1)*pageSize)
	if err != nil {
		return OperationalLogPage{}, err
	}
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
		if page > totalPages {
			page = totalPages
			items, total, err = engine.store.ListOperationalLogsPage(level, query, pageSize, (page-1)*pageSize)
			if err != nil {
				return OperationalLogPage{}, err
			}
		}
	}
	return OperationalLogPage{Logs: items, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}, nil
}

// ClearOperationalLogs clears only the global plugin runtime/error log. New
// lifecycle events written after this call remain visible as a new boundary.
func (engine *Engine) ClearOperationalLogs() error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	return engine.store.ClearOperationalLogs()
}

// LogOperational is used only by lifecycle, management, and bounded
// background paths. Scheduler hot-path decisions remain in the existing
// batched DecisionLog queue and never perform a SQLite write per request.
func (engine *Engine) LogOperational(level, event, message, authID, keyID string) {
	if engine == nil || engine.usageClosed.Load() {
		return
	}
	_ = engine.store.AppendOperationalLog(OperationalLog{
		OccurredAt: time.Now().UTC(),
		Level:      level,
		Event:      event,
		Message:    message,
		AuthID:     authID,
		KeyID:      keyID,
	})
}

// AnalysisReaderDegraded exposes a management-only performance fallback. It
// never affects the quota guard, account scheduling, or Token settlement.
func (engine *Engine) AnalysisReaderDegraded() bool {
	return engine != nil && engine.store != nil && engine.store.AnalysisReaderDegraded()
}

func (engine *Engine) Models() ([]ModelCatalogEntry, error) { return engine.store.ListModelCatalog() }

func (engine *Engine) ReplaceModels(models []ModelCatalogEntry) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	seen := make(map[string]struct{}, len(models))
	normalized := make([]ModelCatalogEntry, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		model.DisplayName = strings.TrimSpace(model.DisplayName)
		if model.DisplayName == "" {
			model.DisplayName = model.ID
		}
		model.Owner = strings.TrimSpace(model.Owner)
		model.Available = true
		normalized = append(normalized, model)
	}
	// A failed or partial CPA model lookup must never erase the most recent
	// usable catalog. The panel already treats an empty upstream result as a
	// failed sync; keep the same safety boundary for direct management calls.
	if len(normalized) == 0 {
		return fmt.Errorf("refuse to replace the CPA Codex model catalog with an empty result")
	}
	return engine.store.ReplaceModelCatalog(normalized, time.Now().UTC())
}

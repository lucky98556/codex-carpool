package quota

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	// officialCalibrationBlend keeps a fresh account measurement responsive
	// when an observation would make the provisional guard stricter. A larger
	// Tokens-per-x observation relaxes an over-estimate and applies immediately.
	officialCalibrationBlend = 0.30
	// Codex reports the weekly window in whole percentage points. Between
	// official polls, provisional Token evidence is therefore bounded to one
	// smallest observable step of the selected account. Every later poll replaces
	// it from the Key's complete current-window Token total, so unrelated account
	// traffic cannot become a Key charge and false early 429s stay bounded.
	officialProvisionalPercentCap = 1.0
	// A very small percentage delta can be rounded by the upstream panel. Do
	// not turn that into an unbounded local allowance; a 20x upward correction
	// is already far beyond the old conservative fallback and still bounded.
	officialCalibrationMaximumFallbackFactor = int64(20)
)

// quotaCalibration is plugin-owned metadata. It stores no credential, Key,
// prompt, or upstream response body: only the derived Token equivalent of one
// configured account x and the last successful official observation.
type quotaCalibration struct {
	AuthID           string
	TokensPerX       int64
	Samples          int64
	AccountCapacityX float64
	WindowResetAt    int64
	ObservedAt       time.Time
}

// quotaCalibrationView is the read-only form used by scheduling diagnostics.
// Fallback is explicit so an operator can distinguish a newly configured
// account from one that has already received an official-percent calibration.
type quotaCalibrationView struct {
	TokensPerX int64
	Samples    int64
	ObservedAt time.Time
	Source     string
}

// officialCalibrationObservation aligns an official account percentage change
// with the plugin-visible account Token evidence from exactly the same period.
// It calibrates Token/x; it is not itself a managed Key charge.
type officialCalibrationObservation struct {
	AuthID           string
	CompletedTokens  int64
	ConsumedX        float64
	AccountCapacityX float64
	WindowResetAt    int64
	ObservedAt       time.Time
}

func fallbackTokensPerX(requestUnits int64) int64 {
	return multiplierCapacity(1, requestUnits, defaultSevenDayBaseRequests)
}

func capacityForX(multiplier float64, tokensPerX int64) int64 {
	if multiplier <= 0 || tokensPerX <= 0 {
		return 0
	}
	value := multiplier * float64(tokensPerX)
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Floor(value))
}

func (engine *Engine) quotaCalibrationView(authID string, requestUnits int64) quotaCalibrationView {
	fallback := fallbackTokensPerX(requestUnits)
	if engine == nil || authID == "" {
		return quotaCalibrationView{TokensPerX: fallback, Source: "fallback"}
	}
	engine.poolMu.RLock()
	entry, accountExists := engine.accountPool[authID]
	engine.poolMu.RUnlock()
	if !accountExists || !entry.Enabled || entry.CapacityX <= 0 {
		return quotaCalibrationView{TokensPerX: fallback, Source: "fallback"}
	}
	engine.calibrationMu.RLock()
	calibration, found := engine.quotaCalibrations[authID]
	engine.calibrationMu.RUnlock()
	if !found || calibration.TokensPerX <= 0 ||
		math.Abs(calibration.AccountCapacityX-entry.CapacityX) > 0.000001 {
		// The operator-entered account x is part of the learned scale. A
		// capacity edit therefore invalidates the old conversion until a new
		// aligned official observation confirms the updated account.
		return quotaCalibrationView{TokensPerX: fallback, Source: "fallback"}
	}
	tokensPerX := calibration.TokensPerX
	if tokensPerX < fallback {
		// The plugin cannot observe usage made outside its own callbacks. Such
		// traffic lowers completed-local-Tokens / official-consumed-x and cannot
		// prove that one x is smaller than the explicit product fallback. Keeping
		// this floor prevents unrelated account traffic from overcharging a Key.
		tokensPerX = fallback
	}
	return quotaCalibrationView{
		TokensPerX: tokensPerX,
		Samples:    calibration.Samples,
		ObservedAt: calibration.ObservedAt,
		Source:     "official_percent_delta",
	}
}

// provisionalXUnits converts a terminal Token total into a temporary x charge
// only after the account has a trustworthy official calibration. The caller
// additionally caps the interval total before it participates in admission;
// a later official observation atomically replaces it with the full-window
// charge derived from that Key's own durable Token total.
func (engine *Engine) provisionalXUnits(authID string, tokens, requestUnits int64) int64 {
	if tokens <= 0 {
		return 0
	}
	calibration := engine.quotaCalibrationView(authID, requestUnits)
	// A fallback is useful in diagnostics but is not trustworthy enough to
	// consume a customer's x balance. The first aligned official observation
	// establishes the account-local scale; until then the five-minute official
	// poll remains the authority.
	if calibration.Source != "official_percent_delta" || calibration.TokensPerX <= 0 {
		return 0
	}
	units := capacityForX(float64(tokens)/float64(calibration.TokensPerX), officialXUnitsPerX)
	if units <= 0 {
		return 1
	}
	return units
}

func (engine *Engine) provisionalXLimit(authID string) int64 {
	if engine == nil || authID == "" {
		return 0
	}
	engine.poolMu.RLock()
	entry, found := engine.accountPool[authID]
	engine.poolMu.RUnlock()
	if !found || !entry.Enabled || entry.CapacityX <= 0 {
		return 0
	}
	return capacityForX(entry.CapacityX*officialProvisionalPercentCap/100, officialXUnitsPerX)
}

// poolTokensPerX derives one Key-wide Token scale from the configured pool.
// AccountCapacityX is an operator supplied weight: a 20x account contributes
// twenty parts to this scale while a 1x account contributes one. It does not
// split a Key's assigned x across accounts; the selected account is protected
// separately by its own official quota window.
func (engine *Engine) poolTokensPerX(requestUnits int64) int64 {
	if engine == nil {
		return fallbackTokensPerX(requestUnits)
	}
	engine.poolMu.RLock()
	entries := make([]AccountPoolEntry, 0, len(engine.accountPool))
	for _, entry := range engine.accountPool {
		if entry.Enabled && entry.CapacityX > 0 {
			entries = append(entries, entry)
		}
	}
	engine.poolMu.RUnlock()
	if len(entries) == 0 {
		return fallbackTokensPerX(requestUnits)
	}
	var totalWeight float64
	var weightedTokens float64
	for _, entry := range entries {
		calibration := engine.quotaCalibrationView(entry.AuthID, requestUnits)
		totalWeight += entry.CapacityX
		weightedTokens += entry.CapacityX * float64(calibration.TokensPerX)
	}
	if totalWeight <= 0 || weightedTokens <= 0 {
		return fallbackTokensPerX(requestUnits)
	}
	if weightedTokens/totalWeight >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Floor(weightedTokens / totalWeight))
}

// globalAllocationCapacity is a single Key allowance in fixed-point x units.
// Operator-entered account and Key x values therefore use the same scale; no
// guessed Token/x constant participates in enforcement.
func (engine *Engine) globalAllocationCapacity(policy KeyPolicy, requestUnits int64) int64 {
	return capacityForX(policy.AllocationX, officialXUnitsPerX)
}

// reconcileOfficialX uses official weekly percentage changes to calibrate the
// practical Token value of one account x, then rebuilds the current Key ledger
// exclusively from each Key's own durable Token usage. Account traffic outside
// codex-carpool can therefore make calibration more conservative, but can never
// be assigned directly to a managed Key.
func (engine *Engine) reconcileOfficialX(entry AccountPoolEntry, snapshot OfficialQuotaSnapshot) (officialCalibrationObservation, error) {
	if engine == nil || entry.AuthID == "" || entry.CapacityX <= 0 ||
		snapshot.LastError != "" || snapshot.hasPendingEstimatedSecondaryReset() ||
		snapshot.Secondary.ResetAt == nil || !snapshot.Secondary.ResetAt.After(snapshot.ObservedAt) ||
		snapshot.Secondary.LimitWindowSeconds <= 0 || snapshot.ObservedAt.IsZero() {
		return officialCalibrationObservation{}, nil
	}
	resetAt := snapshot.Secondary.ResetAt.UTC().UnixMilli()
	officialObservedAt := snapshot.ObservedAt.UTC()
	next := officialXReconciliationState{
		AuthID: entry.AuthID, WindowResetAt: resetAt,
		UsedPercent:      normalizeUsedPercent(snapshot.Secondary.UsedPercent),
		AccountCapacityX: entry.CapacityX,
		// Actual usage and provisional x are stored in minute-end buckets. Use
		// only fully completed minutes: a callback that lands after the official
		// response in the same minute must remain provisional until a later
		// percentage observation, not be cleared by an earlier upstream value.
		ObservedAt: officialObservedAt.Truncate(usageBucketWindow),
	}
	previous, found, err := engine.store.LoadOfficialXReconciliationState(entry.AuthID)
	if err != nil {
		return officialCalibrationObservation{}, err
	}
	// A new account, official reset, or operator capacity change starts a fresh
	// percentage baseline. Historical Token rows are never reinterpreted.
	if !found || previous.WindowResetAt != next.WindowResetAt ||
		math.Abs(previous.AccountCapacityX-next.AccountCapacityX) > 0.000001 {
		// The first baseline starts at the beginning of its observation minute.
		// Usage completed after installation in that minute is then available
		// to the first measurable official percentage change.
		return officialCalibrationObservation{}, engine.store.ApplyOfficialXReconciliation(next, nil, time.Time{})
	}
	if !next.ObservedAt.After(previous.ObservedAt) {
		return officialCalibrationObservation{}, nil
	}
	percentDelta := next.UsedPercent - previous.UsedPercent

	// Freeze terminal callbacks only for the local flush/query/commit boundary.
	// No network operation happens under adminMu.
	engine.adminMu.Lock()
	defer engine.adminMu.Unlock()
	if err := engine.flushAllocationPersistence(); err != nil {
		// A provisional mutation queued by the last completed callback must be
		// durable before the transaction clears and replaces that interval.
		// Otherwise the older mutation could land after reconciliation and be
		// counted again after a restart.
		return officialCalibrationObservation{}, fmt.Errorf("flush provisional x before official reconciliation: %w", err)
	}
	if err := engine.flushPending(); err != nil {
		return officialCalibrationObservation{}, fmt.Errorf("flush completed usage before official x reconciliation: %w", err)
	}
	if engine.pendingSettlementsForAccount(entry.AuthID) > 0 {
		// Do not advance the durable percentage watermark ahead of CPA's
		// terminal usage callback. A later poll can then reconcile the same
		// cumulative official observation with complete Key attribution evidence.
		return officialCalibrationObservation{}, nil
	}
	intervalAccountTokens, err := engine.store.CompletedAccountUsageBetween(entry.AuthID, previous.ObservedAt, next.ObservedAt)
	if err != nil {
		return officialCalibrationObservation{}, err
	}
	intervalKeyTokens, err := engine.store.CompletedKeyAccountUsageBetween(entry.AuthID, previous.ObservedAt, next.ObservedAt)
	if err != nil {
		return officialCalibrationObservation{}, err
	}
	provisionalByKey := engine.provisionalXByKeyBetween(
		entry.AuthID, resetAt, previous.ObservedAt, next.ObservedAt,
	)
	for keyID, provisionalUnits := range provisionalByKey {
		if provisionalUnits > 0 && intervalKeyTokens[keyID] <= 0 {
			// Never clear a durable provisional guard unless the same interval
			// has durable Token evidence with which to attribute the official
			// replacement. This keeps a failed usage enqueue conservative
			// instead of silently releasing the Key's allowance.
			return officialCalibrationObservation{}, fmt.Errorf("provisional x for Key %q has no durable Token attribution evidence", keyID)
		}
	}

	consumedUnits := int64(0)
	observation := officialCalibrationObservation{}
	if percentDelta > 0.000001 {
		consumedUnits = capacityForX(entry.CapacityX*percentDelta/100, officialXUnitsPerX)
		observation = officialCalibrationObservation{
			AuthID:           entry.AuthID,
			CompletedTokens:  intervalAccountTokens,
			ConsumedX:        float64(consumedUnits) / float64(officialXUnitsPerX),
			AccountCapacityX: entry.CapacityX,
			WindowResetAt:    resetAt,
			ObservedAt:       next.ObservedAt,
		}
		// Update the scale before rebuilding Key charges so this same official
		// observation repairs the visible x value instead of waiting for a later
		// poll. Calibration is optional; the explicit fallback remains usable.
		if err := engine.maybeCalibrateOfficialQuota(entry, observation); err != nil {
			engine.LogOperational("warn", "official_quota_calibration_failed", "官方额度校准未更新："+err.Error(), snapshot.AuthID, "")
		}
	}

	windowStart := snapshot.Secondary.ResetAt.UTC().Add(-time.Duration(snapshot.Secondary.LimitWindowSeconds) * time.Second)
	windowKeyTokens, err := engine.store.CompletedKeyAccountUsageBetween(entry.AuthID, windowStart, next.ObservedAt)
	if err != nil {
		return officialCalibrationObservation{}, err
	}

	engine.policiesMu.RLock()
	policies := make(map[string]KeyPolicy, len(windowKeyTokens))
	for keyID := range windowKeyTokens {
		if policy, exists := engine.policiesByID[keyID]; exists {
			policies[keyID] = policy
		}
	}
	engine.policiesMu.RUnlock()

	charges := make([]officialXCharge, 0, len(policies))
	if len(policies) > 0 {
		engine.configMu.RLock()
		requestUnits := engine.config.RequestUnits
		engine.configMu.RUnlock()
		calibration := engine.quotaCalibrationView(entry.AuthID, requestUnits)
		keyIDs := make([]string, 0, len(policies))
		for keyID := range policies {
			if windowKeyTokens[keyID] <= 0 {
				continue
			}
			keyIDs = append(keyIDs, keyID)
		}
		sort.Strings(keyIDs)
		for _, keyID := range keyIDs {
			units := capacityForX(float64(windowKeyTokens[keyID])/float64(calibration.TokensPerX), officialXUnitsPerX)
			if units == 0 {
				// Preserve a positive, sub-micro-x usage sample without allowing
				// it to disappear from the durable guard through integer flooring.
				units = 1
			}
			if units <= 0 {
				continue
			}
			policyCapacity := engine.globalAllocationCapacity(policies[keyID], 0)
			if policyCapacity <= 0 {
				continue
			}
			charges = append(charges, officialXCharge{
				KeyID: keyID, AuthID: entry.AuthID, WindowResetAt: resetAt,
				BucketAt: next.ObservedAt.UnixMilli(),
				Units:    units, CapacityUnits: policyCapacity,
			})
		}
	}
	if math.Abs(percentDelta) > 0.000001 {
		if err := engine.store.ApplyOfficialXReconciliation(next, charges, previous.ObservedAt); err != nil {
			return officialCalibrationObservation{}, err
		}
	} else if err := engine.store.ReplaceOfficialXCharges(entry.AuthID, resetAt, next.ObservedAt, charges); err != nil {
		// Keep the calibration watermark on the last measurable percentage
		// change, but still refresh the Key ledger as durable Tokens arrive.
		return officialCalibrationObservation{}, err
	}
	engine.applyOfficialXReconciliation(
		entry.AuthID, resetAt, next.ObservedAt, charges,
	)
	return observation, nil
}

// provisionalXByKeyBetween returns the already-flushed provisional guard for
// one official observation interval. Callers hold adminMu, so no completion
// can change these buckets between this validation and reconciliation.
func (engine *Engine) provisionalXByKeyBetween(authID string, windowResetAt int64, after, through time.Time) map[string]int64 {
	result := make(map[string]int64)
	if engine == nil || authID == "" || windowResetAt <= 0 || !through.After(after) {
		return result
	}
	afterMillis := after.UTC().UnixMilli()
	throughMillis := through.UTC().UnixMilli()
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for key := range engine.allocationBucketsByAuth[authID] {
		if key.WindowResetAt != windowResetAt || key.BucketAt <= afterMillis || key.BucketAt > throughMillis {
			continue
		}
		if provisional := engine.allocationBuckets[key].Provisional; provisional > 0 {
			result[key.KeyID] += provisional
		}
	}
	return result
}

// applyOfficialXReconciliation mirrors the already-committed full-window
// SQLite replacement. Completed charges are rebuilt from durable per-Key Token
// aggregates; provisional charges through the observation are replaced too.
func (engine *Engine) applyOfficialXReconciliation(authID string, windowResetAt int64, observedAt time.Time, charges []officialXCharge) {
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for key := range engine.allocationBucketsByAuth[authID] {
		if key.WindowResetAt != windowResetAt {
			continue
		}
		bucket := engine.allocationBuckets[key]
		cycleKey := allocationCycleKey{
			KeyID: key.KeyID, AuthID: key.AuthID, WindowResetAt: key.WindowResetAt,
		}
		cycle := engine.allocationCycles[cycleKey]
		if bucket.Completed > 0 {
			cycle.Completed -= bucket.Completed
			if cycle.Completed < 0 {
				cycle.Completed = 0
			}
			bucket.Completed = 0
		}
		if key.BucketAt <= observedAt.UTC().UnixMilli() && bucket.Provisional > 0 {
			cycle.Provisional -= bucket.Provisional
			if cycle.Provisional < 0 {
				cycle.Provisional = 0
			}
			bucket.Provisional = 0
		}
		engine.allocationCycles[cycleKey] = cycle
		engine.setAllocationBucketLocked(key, bucket)
		if bucket.Completed == 0 && bucket.Provisional == 0 && bucket.Reserved == 0 {
			engine.deleteAllocationBucketLocked(key, bucket)
		}
	}
	for _, charge := range charges {
		key := allocationBucketKey{
			KeyID: charge.KeyID, AuthID: charge.AuthID,
			WindowResetAt: charge.WindowResetAt, BucketAt: charge.BucketAt,
		}
		bucket := engine.allocationBuckets[key]
		bucket.Completed = charge.Units
		if charge.CapacityUnits > bucket.Capacity {
			bucket.Capacity = charge.CapacityUnits
		}
		if charge.CapacityUnits > bucket.GlobalCapacity {
			bucket.GlobalCapacity = charge.CapacityUnits
		}
		engine.setAllocationBucketLocked(key, bucket)
		cycleKey := allocationCycleKey{
			KeyID: charge.KeyID, AuthID: charge.AuthID, WindowResetAt: charge.WindowResetAt,
		}
		cycle := engine.allocationCycles[cycleKey]
		cycle.Completed += charge.Units
		if charge.CapacityUnits > cycle.Capacity {
			cycle.Capacity = charge.CapacityUnits
		}
		if charge.CapacityUnits > cycle.GlobalCapacity {
			cycle.GlobalCapacity = charge.CapacityUnits
		}
		engine.allocationCycles[cycleKey] = cycle
	}
}

// maybeCalibrateOfficialQuota learns an account's practical Token equivalent
// from the exact completed-Token interval aligned with a measurable official
// percentage change. It never uses adjacent panel polls as an independent
// watermark, because rounded unchanged polls would omit part of the Token
// evidence and make the following provisional x estimate jump upward.
// Other clients using the same Codex account can make the raw sample smaller;
// that sample is only a lower bound and cannot reduce the product fallback.
func (engine *Engine) maybeCalibrateOfficialQuota(entry AccountPoolEntry, observation officialCalibrationObservation) error {
	if engine == nil || entry.AuthID == "" || entry.CapacityX <= 0 ||
		observation.AuthID != entry.AuthID || observation.CompletedTokens <= 0 ||
		observation.ConsumedX <= 0 || observation.WindowResetAt <= 0 || observation.ObservedAt.IsZero() ||
		math.Abs(observation.AccountCapacityX-entry.CapacityX) > 0.000001 {
		return nil
	}
	observedTokensPerX := int64(math.Floor(float64(observation.CompletedTokens) / observation.ConsumedX))
	if observedTokensPerX <= 0 {
		return nil
	}
	engine.configMu.RLock()
	fallback := fallbackTokensPerX(engine.config.RequestUnits)
	engine.configMu.RUnlock()
	maximumObserved := int64(math.MaxInt64)
	if fallback > 0 && fallback <= math.MaxInt64/officialCalibrationMaximumFallbackFactor {
		maximumObserved = fallback * officialCalibrationMaximumFallbackFactor
	}
	if observedTokensPerX > maximumObserved {
		// Retain the prior estimate when a rounded upstream percentage would
		// otherwise make the local guard less conservative than its evidence.
		return nil
	}

	engine.calibrationMu.RLock()
	previousCalibration, hasCalibration := engine.quotaCalibrations[entry.AuthID]
	engine.calibrationMu.RUnlock()
	estimated := observedTokensPerX
	samples := int64(1)
	if hasCalibration && previousCalibration.TokensPerX > 0 &&
		previousCalibration.WindowResetAt == observation.WindowResetAt &&
		math.Abs(previousCalibration.AccountCapacityX-entry.CapacityX) <= 0.000001 {
		// A larger Token/x value means the old provisional guard was too strict,
		// so accept that correction immediately. Move toward a smaller value
		// gradually; the official percentage ledger still applies the complete
		// authoritative charge at every measurable poll.
		if observedTokensPerX < previousCalibration.TokensPerX {
			estimated = int64(math.Floor(float64(previousCalibration.TokensPerX)*(1-officialCalibrationBlend) + float64(observedTokensPerX)*officialCalibrationBlend))
		}
		samples = previousCalibration.Samples + 1
	}
	if estimated < fallback {
		// Official account consumption can include clients that never traverse
		// this plugin. Their Tokens are unavailable locally, so a smaller sample
		// is only a lower bound and must not shrink a managed Key's allowance.
		estimated = fallback
	}
	if estimated <= 0 {
		return nil
	}
	calibration := quotaCalibration{
		AuthID:           entry.AuthID,
		TokensPerX:       estimated,
		Samples:          samples,
		AccountCapacityX: entry.CapacityX,
		WindowResetAt:    observation.WindowResetAt,
		ObservedAt:       observation.ObservedAt.UTC(),
	}
	if err := engine.store.UpsertQuotaCalibration(calibration); err != nil {
		return err
	}
	engine.calibrationMu.Lock()
	engine.quotaCalibrations[entry.AuthID] = calibration
	engine.calibrationMu.Unlock()
	return nil
}

package quota

import (
	"sort"
	"strings"
	"time"
)

// SchedulerCandidate is the stable, non-sensitive subset of CPA's scheduler
// candidate list needed by the shared-pool selector.
type SchedulerCandidate struct {
	// AuthID is the plugin's stable account-pool identity. CPAAuthID is the
	// original scheduler identity that must be returned to CPA after a pool
	// entry has been selected through an optional auth_index alias.
	AuthID    string
	CPAAuthID string
	Priority  int
	Status    string
}

// ResolveSchedulerCandidates maps CPA's current candidate IDs to the stable
// account-pool identity. CPA versions may expose an auth-file ID rather than
// its relative path; auth_index is the explicitly saved bridge for that case.
func (engine *Engine) ResolveSchedulerCandidates(candidates []SchedulerCandidate) []SchedulerCandidate {
	if engine == nil || len(candidates) == 0 {
		return candidates
	}
	engine.poolMu.RLock()
	aliases := make(map[string]string, len(engine.accountPool))
	conflicts := make(map[string]struct{})
	directIDs := make(map[string]struct{}, len(engine.accountPool))
	for authID, entry := range engine.accountPool {
		if !entry.Enabled {
			continue
		}
		directIDs[authID] = struct{}{}
		alias := strings.TrimSpace(entry.AuthIndex)
		if alias == "" || alias == authID {
			continue
		}
		if existing, exists := aliases[alias]; exists && existing != authID {
			conflicts[alias] = struct{}{}
			continue
		}
		aliases[alias] = authID
	}
	engine.poolMu.RUnlock()

	resolved := make([]SchedulerCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.AuthID = strings.TrimSpace(candidate.AuthID)
		candidate.CPAAuthID = candidate.AuthID
		if candidate.AuthID != "" {
			_, direct := directIDs[candidate.AuthID]
			if !direct {
				if _, conflict := conflicts[candidate.AuthID]; !conflict {
					if mapped, exists := aliases[candidate.AuthID]; exists {
						candidate.AuthID = mapped
					}
				}
			}
		}
		resolved = append(resolved, candidate)
	}
	return resolved
}

// CPAAuthIDForPoolAuthID returns the original CPA identity for a successful
// internal account-pool selection. Direct account IDs remain backwards
// compatible when no alias was recorded.
func CPAAuthIDForPoolAuthID(candidates []SchedulerCandidate, authID string) string {
	for _, candidate := range candidates {
		if candidate.AuthID == authID && strings.TrimSpace(candidate.CPAAuthID) != "" {
			return candidate.CPAAuthID
		}
	}
	return authID
}

// ResolvePoolAuthID applies the same alias mapping to CPA's usage callback so
// completed Token settlement always reaches the ledger that admitted it.
func (engine *Engine) ResolvePoolAuthID(authID string) string {
	authID = strings.TrimSpace(authID)
	if engine == nil || authID == "" {
		return authID
	}
	engine.poolMu.RLock()
	defer engine.poolMu.RUnlock()
	if direct, exists := engine.accountPool[authID]; exists && direct.Enabled {
		return authID
	}
	mapped := ""
	for poolAuthID, entry := range engine.accountPool {
		if !entry.Enabled {
			continue
		}
		if strings.TrimSpace(entry.AuthIndex) != authID {
			continue
		}
		if mapped != "" && mapped != poolAuthID {
			return authID
		}
		mapped = poolAuthID
	}
	if mapped != "" {
		return mapped
	}
	return authID
}

// selectPoolAccounts returns every CPA-eligible account in descending routing
// preference. It excludes accounts whose plugin-owned, account-wide ledger is
// already full when another account can serve the request. If every candidate
// is full, it returns those candidates for Admit to distinguish a Key x cap
// from account-wide exhaustion under the same reservation lock.
func (engine *Engine) selectPoolAccounts(candidates []SchedulerCandidate, requestUnits int64, now time.Time) ([]string, string) {
	available := make(map[string]SchedulerCandidate, len(candidates))
	for _, candidate := range candidates {
		candidate.AuthID = strings.TrimSpace(candidate.AuthID)
		if candidate.AuthID != "" {
			available[candidate.AuthID] = candidate
		}
	}
	engine.poolMu.RLock()
	type choice struct {
		authID   string
		priority int
		score    float64
		windows  []officialAccountWindowTarget
	}
	choices := make([]choice, 0, len(engine.accountPool))
	poolConfigured := false
	candidateMatchesPool := false
	snapshotPending := false
	confirmedExhausted := false
	for authID, entry := range engine.accountPool {
		if !entry.Enabled {
			continue
		}
		poolConfigured = true
		candidate, listed := available[authID]
		if !listed {
			continue
		}
		candidateMatchesPool = true
		snapshot, known := engine.quotaSnapshots[authID]
		if !known || snapshot.ObservedAt.IsZero() || now.UTC().Sub(snapshot.ObservedAt) > quotaSnapshotHardAge || snapshot.LastError != "" {
			snapshotPending = true
			continue
		}
		if snapshot.hasPendingEstimatedSecondaryReset() {
			// The snapshot already describes the candidate new week, but it must
			// not select an account or create a fresh Key x ledger before its
			// second confirming observation arrives.
			snapshotPending = true
			continue
		}
		if _, hasReset := officialWeeklyResetAt(snapshot.Secondary, snapshot.ObservedAt, now); !hasReset {
			snapshotPending = true
			continue
		}
		if snapshot.LimitReached || !snapshot.Allowed {
			confirmedExhausted = true
			continue
		}
		windows := engine.officialAccountWindowTargets(entry, snapshot, requestUnits, now)
		if len(windows) == 0 {
			snapshotPending = true
			continue
		}
		choices = append(choices, choice{authID: authID, priority: candidate.Priority, score: snapshot.availabilityScore(entry.CapacityX), windows: windows})
	}
	engine.poolMu.RUnlock()
	if !poolConfigured {
		return nil, "quota_pool_unconfigured"
	}
	if len(choices) == 0 {
		if !candidateMatchesPool {
			// A missing CPA candidate is a configuration/identity mismatch, not an
			// official quota failure. Keeping it distinct makes the operator-facing
			// 503 actionable without ever selecting an unverified account.
			return nil, "quota_candidate_mismatch"
		}
		if snapshotPending {
			return nil, "quota_snapshot_unavailable"
		}
		if confirmedExhausted {
			return nil, "quota_pool_exhausted"
		}
		return nil, "quota_account_unavailable"
	}
	// Do not hold poolMu while waiting for allocationMu. Account configuration
	// is immutable for this short pass and reserveAccountAllocation rechecks
	// the actual target before it makes a durable admission reservation.
	engine.allocationMu.Lock()
	availableChoices := choices[:0]
	exhaustedChoices := make([]choice, 0, len(choices))
	localExhausted := false
	for _, candidate := range choices {
		remainingScore, available := engine.officialAccountWindowAvailabilityLocked(candidate.windows, requestUnits)
		if !available {
			localExhausted = true
			exhaustedChoices = append(exhaustedChoices, candidate)
			continue
		}
		candidate.score *= remainingScore
		availableChoices = append(availableChoices, candidate)
	}
	engine.allocationMu.Unlock()
	choices = availableChoices
	if len(choices) == 0 {
		if localExhausted {
			// Let Admit inspect the same exhausted candidates under its normal
			// reservation order. This preserves the distinction between a Key that
			// has consumed its one global x balance and a pool that is only
			// account-wide exhausted; neither path can create a new reservation.
			result := make([]string, 0, len(exhaustedChoices))
			for _, candidate := range exhaustedChoices {
				result = append(result, candidate.authID)
			}
			return result, ""
		}
		if confirmedExhausted {
			return nil, "quota_pool_exhausted"
		}
		return nil, "quota_snapshot_unavailable"
	}
	sort.Slice(choices, func(left, right int) bool {
		if choices[left].score != choices[right].score {
			return choices[left].score > choices[right].score
		}
		if choices[left].priority != choices[right].priority {
			return choices[left].priority < choices[right].priority
		}
		return choices[left].authID < choices[right].authID
	})
	result := make([]string, 0, len(choices))
	for _, choice := range choices {
		result = append(result, choice.authID)
	}
	return result, ""
}

// officialAccountWindowAvailabilityLocked returns the smallest remaining
// fraction across the active official windows. allocationMu protects both
// the state and concurrent reservations.
func (engine *Engine) officialAccountWindowAvailabilityLocked(targets []officialAccountWindowTarget, requestUnits int64) (float64, bool) {
	remainingScore := 1.0
	for _, target := range targets {
		state, exists := engine.officialAccountWindows[target.Key]
		if !exists || exceedsLimit(state.Completed+state.Reserved, requestUnits, state.Capacity) {
			return 0, false
		}
		if state.Capacity <= 0 {
			return 0, false
		}
		remaining := state.Capacity - state.Completed - state.Reserved
		fraction := float64(remaining) / float64(state.Capacity)
		if fraction < remainingScore {
			remainingScore = fraction
		}
	}
	return remainingScore, true
}

// StalePoolCandidates returns configured accounts that should be refreshed in
// the background. It never blocks a request or performs network I/O.
func (engine *Engine) StalePoolCandidates(candidates []SchedulerCandidate, now time.Time) []string {
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if authID := strings.TrimSpace(candidate.AuthID); authID != "" {
			allowed[authID] = struct{}{}
		}
	}
	engine.poolMu.RLock()
	defer engine.poolMu.RUnlock()
	result := make([]string, 0)
	for authID, entry := range engine.accountPool {
		if !entry.Enabled {
			continue
		}
		if _, exists := allowed[authID]; !exists {
			continue
		}
		snapshot, exists := engine.quotaSnapshots[authID]
		// A fresh snapshot is still insufficient once the weekly reset identity
		// has expired or is uncertain.
		// Trigger a deduplicated background refresh immediately so the next
		// request does not wait for the one-minute freshness timeout.
		_, hasWeeklyReset := officialWeeklyResetAt(snapshot.Secondary, snapshot.ObservedAt, now)
		if !exists || !snapshot.freshAt(now) || snapshot.hasPendingEstimatedSecondaryReset() || !hasWeeklyReset {
			result = append(result, authID)
		}
	}
	return result
}

func (engine *Engine) AccountByID(authID string) (AccountPoolEntry, bool) {
	engine.poolMu.RLock()
	defer engine.poolMu.RUnlock()
	entry, exists := engine.accountPool[strings.TrimSpace(authID)]
	return entry, exists
}

// PendingEstimatedResetConfirmationAt returns the earliest safe time to poll
// an account again after an inferred weekly reset. Keeping this separate from
// routing lets the synchronizer shorten its normal success interval without
// turning every blocked scheduler request into an upstream HTTP call.
func (engine *Engine) PendingEstimatedResetConfirmationAt(authID string) (time.Time, bool) {
	if engine == nil {
		return time.Time{}, false
	}
	engine.poolMu.RLock()
	snapshot, exists := engine.quotaSnapshots[strings.TrimSpace(authID)]
	engine.poolMu.RUnlock()
	if !exists || snapshot.SecondaryEstimatedResetCandidateAt == nil || snapshot.SecondaryEstimatedResetCandidateSeenAt == nil {
		return time.Time{}, false
	}
	return snapshot.SecondaryEstimatedResetCandidateSeenAt.Add(quotaResetStabilityTolerance), true
}

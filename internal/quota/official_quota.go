package quota

import (
	"math"
	"strings"
	"time"
)

// OfficialQuotaWindow stores the single Codex account window used by this
// plugin's shared pool. Percent is always the upstream used value, not a
// locally estimated token percentage.
type OfficialQuotaWindow struct {
	UsedPercent       float64    `json:"used_percent"`
	LimitWindowSeconds int64      `json:"limit_window_seconds"`
	ResetAt            *time.Time `json:"reset_at,omitempty"`
	// ResetEstimated marks a reset identity derived from the receive time
	// because the upstream response did not include one. It is persisted only
	// inside the plugin database, never exposed through the management API.
	ResetEstimated bool `json:"-"`
	// BaselineAt is the first successful observation of this official window.
	// Local completed work after it remains charged until the upstream reset,
	// even when a later upstream percentage is delayed or unchanged.
	BaselineAt time.Time `json:"-"`
}

// OfficialQuotaSnapshot is owned by this plugin's SQLite database. It never
// contains OAuth tokens, request prompts, or response content.
type OfficialQuotaSnapshot struct {
	AuthID       string              `json:"auth_id"`
	PlanType     string              `json:"plan_type"`
	Allowed      bool                `json:"allowed"`
	LimitReached bool                `json:"limit_reached"`
	Primary      OfficialQuotaWindow `json:"primary"`
	Secondary    OfficialQuotaWindow `json:"secondary"`
	ObservedAt   time.Time           `json:"observed_at"`
	LastError    string              `json:"last_error,omitempty"`
	// SecondaryEstimatedResetCandidateAt and SeenAt form a durable two-success
	// confirmation for a weekly reset whose identity was inferred locally. They
	// are internal accounting metadata and are never exposed to the panel.
	SecondaryEstimatedResetCandidateAt     *time.Time `json:"-"`
	SecondaryEstimatedResetCandidateSeenAt *time.Time `json:"-"`
}

// hasPendingEstimatedSecondaryReset reports a locally inferred weekly reset
// that still needs its durable second observation. Until it is confirmed, a
// new Key allocation ledger must not be created from the tentative reset.
func (snapshot OfficialQuotaSnapshot) hasPendingEstimatedSecondaryReset() bool {
	return snapshot.SecondaryEstimatedResetCandidateAt != nil
}

const (
	// officialWeeklyWindowSeconds is the product-level shared-pool period. The
	// current Codex panel exposes the normal account limit as a weekly quota.
	// wham/usage can retain an obsolete primary_window duration (for example,
	// 18000) even though that same quota is shown as weekly by Codex/CPA.
	officialWeeklyWindowSeconds = int64(7 * 24 * 60 * 60)
	quotaSnapshotFreshFor = time.Minute
	quotaSnapshotHardAge  = 10 * time.Minute
	// reset_after_seconds is converted using the local receive time when the
	// upstream response omits reset_at. Preserve a nearby future reset identity
	// across polls so millisecond/network jitter cannot manufacture a new weekly
	// allocation ledger before Codex actually resets the account.
	quotaResetStabilityTolerance = 2 * time.Minute
)

func normalizeOfficialQuotaSnapshot(snapshot OfficialQuotaSnapshot) OfficialQuotaSnapshot {
	snapshot.AuthID = strings.TrimSpace(snapshot.AuthID)
	snapshot.PlanType = strings.TrimSpace(snapshot.PlanType)
	snapshot.LastError = strings.TrimSpace(snapshot.LastError)
	snapshot = normalizeCurrentWeeklyQuota(snapshot)
	snapshot.Secondary.UsedPercent = normalizeUsedPercent(snapshot.Secondary.UsedPercent)
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now().UTC()
	} else {
		snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	}
	snapshot.Secondary = normalizeOfficialQuotaWindow(snapshot.Secondary, snapshot.ObservedAt)
	snapshot.SecondaryEstimatedResetCandidateAt = normalizeOptionalTime(snapshot.SecondaryEstimatedResetCandidateAt)
	snapshot.SecondaryEstimatedResetCandidateSeenAt = normalizeOptionalTime(snapshot.SecondaryEstimatedResetCandidateSeenAt)
	return snapshot
}

// normalizeCurrentWeeklyQuota migrates the former two-window storage shape to
// Codex's current product shape: primary_window is the normal account weekly
// quota, while secondary_window may describe a model-specific quota. A shared
// pool has one total account allowance, so it must use the normal quota only.
// Keeping that canonical value in Secondary preserves the existing durable
// weekly-ledger schema and also upgrades snapshots written by older releases.
func normalizeCurrentWeeklyQuota(snapshot OfficialQuotaSnapshot) OfficialQuotaSnapshot {
	weekly := snapshot.Secondary
	if snapshot.Primary.LimitWindowSeconds > 0 {
		weekly = snapshot.Primary
	}
	if weekly.LimitWindowSeconds > 0 {
		weekly.LimitWindowSeconds = officialWeeklyWindowSeconds
	}
	snapshot.Primary = OfficialQuotaWindow{}
	snapshot.Secondary = weekly
	return snapshot
}

func normalizeOptionalTime(value *time.Time) *time.Time {
	if value == nil || value.IsZero() {
		return nil
	}
	normalized := value.UTC()
	return &normalized
}

func normalizeOfficialQuotaWindow(window OfficialQuotaWindow, observedAt time.Time) OfficialQuotaWindow {
	if window.ResetAt != nil {
		value := window.ResetAt.UTC()
		window.ResetAt = &value
	} else if window.LimitWindowSeconds > 0 && !observedAt.IsZero() {
		// Some Codex payloads contain only the window length. Use the first
		// observation as a conservative identity and retain it across later
		// polls; deriving it again from every poll would silently open quota.
		value := observedAt.UTC().Add(time.Duration(window.LimitWindowSeconds) * time.Second)
		window.ResetAt = &value
		window.ResetEstimated = true
	}
	if !window.BaselineAt.IsZero() {
		window.BaselineAt = window.BaselineAt.UTC()
	} else if window.ResetAt != nil && !observedAt.IsZero() {
		window.BaselineAt = observedAt.UTC()
	}
	return window
}

func stabilizeOfficialQuotaSnapshot(previous, next OfficialQuotaSnapshot, now time.Time) OfficialQuotaSnapshot {
	next.Primary = stabilizeOfficialQuotaWindow(previous.Primary, next.Primary, now)
	next.Secondary = stabilizeOfficialQuotaWindow(previous.Secondary, next.Secondary, now)
	return next
}

// reconcileSecondaryReset determines whether an ended weekly ledger can be
// released. Explicit reset identities are authoritative immediately. A reset
// derived from a window length requires two successful observations at least
// quotaResetStabilityTolerance apart, and the first observation is persisted
// so a process restart cannot turn that confirmation into a bypass.
func reconcileSecondaryReset(previous, next OfficialQuotaSnapshot) (OfficialQuotaSnapshot, time.Time, string) {
	next.SecondaryEstimatedResetCandidateAt = normalizeOptionalTime(previous.SecondaryEstimatedResetCandidateAt)
	next.SecondaryEstimatedResetCandidateSeenAt = normalizeOptionalTime(previous.SecondaryEstimatedResetCandidateSeenAt)
	if next.LastError != "" || next.Secondary.ResetAt == nil || !next.Secondary.ResetAt.After(next.ObservedAt) {
		return next, time.Time{}, ""
	}
	if candidateAt := next.SecondaryEstimatedResetCandidateAt; candidateAt != nil {
		if !next.Secondary.ResetEstimated && next.Secondary.ResetAt.After(*candidateAt) {
			next.SecondaryEstimatedResetCandidateAt = nil
			next.SecondaryEstimatedResetCandidateSeenAt = nil
			return next, *candidateAt, "explicit"
		}
		if next.Secondary.ResetEstimated && previous.Secondary.LimitWindowSeconds == next.Secondary.LimitWindowSeconds {
			if seenAt := next.SecondaryEstimatedResetCandidateSeenAt; seenAt != nil && !next.ObservedAt.Before(seenAt.Add(quotaResetStabilityTolerance)) {
				next.SecondaryEstimatedResetCandidateAt = nil
				next.SecondaryEstimatedResetCandidateSeenAt = nil
				return next, *candidateAt, "estimated_confirmed"
			}
		}
		return next, time.Time{}, ""
	}
	// Codex can explicitly roll an account into a new weekly cycle before the
	// previously advertised reset timestamp is reached (for example after an
	// upstream quota reset). A later explicit reset identity together with a
	// lower used percentage is authoritative evidence of that transition. Do
	// not keep charging Keys against the superseded future-dated cycle.
	if previous.Secondary.ResetAt != nil && !next.Secondary.ResetEstimated &&
		next.Secondary.ResetAt.After(previous.Secondary.ResetAt.Add(quotaResetStabilityTolerance)) &&
		next.Secondary.UsedPercent+0.000001 < previous.Secondary.UsedPercent {
		return next, previous.Secondary.ResetAt.UTC(), "explicit_early"
	}
	if previous.Secondary.ResetAt == nil || previous.Secondary.ResetAt.After(next.ObservedAt) || !next.Secondary.ResetAt.After(*previous.Secondary.ResetAt) {
		return next, time.Time{}, ""
	}
	if !next.Secondary.ResetEstimated {
		return next, *previous.Secondary.ResetAt, "explicit"
	}
	// A locally derived reset is tentative even when the old window was stored
	// by an earlier release without its estimate marker, or the first refresh
	// arrives within the tolerance period. Persist the candidate now; routing
	// blocks new Key allocation until a later matching poll confirms it.
	candidateAt, seenAt, pending := estimatedSecondaryResetCandidate(previous, next)
	if !pending {
		return next, time.Time{}, ""
	}
	next.SecondaryEstimatedResetCandidateAt = &candidateAt
	next.SecondaryEstimatedResetCandidateSeenAt = &seenAt
	return next, time.Time{}, ""
}

// estimatedSecondaryResetCandidate identifies a newly inferred weekly window
// whose prior identity has elapsed. It deliberately uses the raw upstream
// observation: nearby reset_after_seconds values can later be stabilized to
// the old identity, but they must not erase this first durable confirmation.
func estimatedSecondaryResetCandidate(previous, next OfficialQuotaSnapshot) (time.Time, time.Time, bool) {
	if !next.Secondary.ResetEstimated {
		return time.Time{}, time.Time{}, false
	}
	return elapsedSecondaryResetCandidate(previous, next)
}

// elapsedSecondaryResetCandidate validates the time identity transition. Its
// caller must already know that the new identity came from an omitted
// upstream reset value; explicit Codex reset identities are released at once.
func elapsedSecondaryResetCandidate(previous, next OfficialQuotaSnapshot) (time.Time, time.Time, bool) {
	if next.LastError != "" || previous.Secondary.ResetAt == nil || next.Secondary.ResetAt == nil {
		return time.Time{}, time.Time{}, false
	}
	if !next.Secondary.ResetAt.After(next.ObservedAt) {
		return time.Time{}, time.Time{}, false
	}
	if previous.Secondary.LimitWindowSeconds > 0 && next.Secondary.LimitWindowSeconds > 0 && previous.Secondary.LimitWindowSeconds != next.Secondary.LimitWindowSeconds {
		return time.Time{}, time.Time{}, false
	}
	if previous.Secondary.ResetAt.After(next.ObservedAt) || !next.Secondary.ResetAt.After(*previous.Secondary.ResetAt) {
		return time.Time{}, time.Time{}, false
	}
	return previous.Secondary.ResetAt.UTC(), next.ObservedAt.UTC(), true
}

func stabilizeOfficialQuotaWindow(previous, next OfficialQuotaWindow, now time.Time) OfficialQuotaWindow {
	if previous.LimitWindowSeconds > 0 && next.LimitWindowSeconds > 0 && previous.LimitWindowSeconds != next.LimitWindowSeconds {
		return next
	}
	if previous.ResetAt == nil || next.ResetAt == nil || !previous.ResetAt.After(now.UTC()) || !next.ResetAt.After(now.UTC()) {
		return next
	}
	sameWindow := next.ResetEstimated
	if !sameWindow {
		delta := previous.ResetAt.Sub(*next.ResetAt)
		if delta < 0 {
			delta = -delta
		}
		sameWindow = delta <= quotaResetStabilityTolerance
	}
	if !sameWindow {
		return next
	}
	// The reset timestamp is the durable ledger identity. A fallback identity
	// must survive every later response that still omits reset fields; a nearby
	// explicit/reset-after value is treated the same way to absorb jitter. A
	// current reset_after_seconds value remains estimated even if an older
	// plugin version persisted this same window without its estimate marker.
	value := previous.ResetAt.UTC()
	next.ResetAt = &value
	next.ResetEstimated = previous.ResetEstimated || next.ResetEstimated
	next.UsedPercent = math.Max(previous.UsedPercent, next.UsedPercent)
	if !previous.BaselineAt.IsZero() {
		next.BaselineAt = previous.BaselineAt.UTC()
	}
	return next
}

func normalizeUsedPercent(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func (snapshot OfficialQuotaSnapshot) freshAt(now time.Time) bool {
	return snapshot.LastError == "" && !snapshot.ObservedAt.IsZero() && now.UTC().Sub(snapshot.ObservedAt) <= quotaSnapshotFreshFor
}

func (snapshot OfficialQuotaSnapshot) usableAt(now time.Time) bool {
	if snapshot.LimitReached || !snapshot.Allowed || snapshot.ObservedAt.IsZero() {
		return false
	}
	if now.UTC().Sub(snapshot.ObservedAt) > quotaSnapshotHardAge {
		return false
	}
	return true
}

// availabilityScore is only a routing preference. Limits are still enforced
// by Codex itself; the shared pool follows its one official weekly allowance.
func (snapshot OfficialQuotaSnapshot) availabilityScore(capacityX float64) float64 {
	if capacityX <= 0 || !snapshot.Allowed || snapshot.LimitReached {
		return 0
	}
	return capacityX * (100 - snapshot.Secondary.UsedPercent) / 100
}

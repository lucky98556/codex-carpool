//go:build linux && cgo

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"codex-carpool/internal/quota"
)

const (
	codexUsageURL          = "https://chatgpt.com/backend-api/wham/usage"
	quotaSyncInterval       = 5 * time.Minute
	quotaSyncTimeout        = 15 * time.Second
	quotaSyncFailureInitial = time.Minute
	quotaSyncFailureMaximum = 15 * time.Minute
	// quotaInitialSnapshotWait is used only by the management surface after an
	// operator saves an account pool or explicitly requests a refresh.  The
	// model scheduler never waits for this value or performs upstream I/O.
	quotaInitialSnapshotWait = 20 * time.Second
	quotaCredentialRefreshSkew = 2 * time.Minute
	quotaCredentialFallbackTTL = 10 * time.Minute
	// One synchronizer-wide spacing prevents startup, a large account pool, or
	// a manual refresh from turning into a burst against Codex's usage endpoint.
	quotaSyncMinimumSpacing   = 2 * time.Second
	manualQuotaRefreshCooldown = 30 * time.Second
	maxQuotaBodyBytes         = 1 << 20
	maxCodexAuthFileBytes     = 1 << 20
	// A small fixed worker pool keeps a large CPA account list from producing
	// one network goroutine per account, while the scheduler remains entirely
	// non-blocking.
	quotaSyncWorkers   = 3
	quotaSyncQueueSize = 128
)

var errQuotaRefreshSkipped = errors.New("quota refresh skipped")
var errTransientCodexAuthFile = errors.New("transient CPA auth-file read")
var errMissingCodexAccountIdentity = errors.New("CPA Codex credential has no stable account_id")

// accountSourceReconcileInterval is intentionally independent from quota
// polling. Tests shorten it to prove that a source retarget cannot wait for
// the five-minute official-quota throttle.
var accountSourceReconcileInterval = 15 * time.Second

// quotaSynchronizer is deliberately outside quota.Engine: network I/O is
// never allowed in the scheduler hot path. Requests merely signal stale
// accounts, while one deduplicated worker refreshes each account through a
// direct official read after resolving its active CPA credential.
type quotaSynchronizer struct {
	engine *quota.Engine
	ctx    context.Context
	cancel context.CancelFunc

	mu                      sync.Mutex
	inFlight                map[string]struct{}
	nextRefreshAt           map[string]time.Time
	refreshFailures         map[string]int
	lastErrorLog            map[string]time.Time
	lastManualRefreshAt     time.Time
	nextOfficialRequestAt   time.Time
	credentials             map[string]cachedQuotaCredential
	sourceIdentities        map[string]cachedSourceIdentity
	readSourceIdentity      func(string) (string, error)
	sourceConflict          bool
	sourceConflictReason    string
	sourceConflictCheckedAt time.Time
	manualRefreshMu         sync.Mutex
	// sourceScanMu serializes full filesystem scans. A forced configuration
	// recheck therefore cannot be overwritten by an older clean scan that
	// started before the configuration changed.
	sourceScanMu sync.Mutex
	// refreshGate is the shutdown admission boundary for every direct official
	// refresh and every local CPA auth-file read. Draining takes its write lock,
	// so it returns only after older unrelated refreshes have finished.
	refreshGate sync.RWMutex
	lifecycleMu     sync.Mutex
	started         bool
	closing         bool
	draining        bool
	cacheOnly       bool
	jobs            chan string
	stop            chan struct{}
	done            chan struct{}
	workers         sync.WaitGroup
	start           sync.Once
	close           sync.Once
}

// cachedQuotaCredential is process-memory-only. It is intentionally never
// persisted, logged, or returned to the management API.
type cachedQuotaCredential struct {
	accessToken    string
	accountID      string
	expiresAt      time.Time
	sourcePath     string
	physicalID     string
	fileModifiedAt time.Time
	fileChangedAt  time.Time
	fileSize       int64
	contentDigest  string
}

// cachedSourceIdentity avoids re-reading an unchanged CPA JSON file every
// reconciliation tick. It contains only a stable account ID, never an OAuth
// token. A missing stable ID is cached too so an identity-less sole account
// keeps its documented behaviour without repeated JSON parsing.
type cachedSourceIdentity struct {
	physicalID string
	modifiedAt time.Time
	changedAt  time.Time
	size       int64
	identity   string
	missing    bool
	contentDigest string
}

// quotaRefreshSchedule tells the management API whether a refresh job was
// actually accepted by the bounded worker queue. "scheduled" must never mean
// merely that a cooldown check passed.
type quotaRefreshSchedule struct {
	Scheduled          int `json:"scheduled"`
	SkippedInFlight    int `json:"skipped_in_flight"`
	SkippedUnavailable int `json:"skipped_unavailable"`
	QueueFull          int `json:"queue_full"`
}

// quotaSnapshotReadiness is the operator-facing result of waiting for a
// queued refresh. It deliberately reports only already-redacted errors held
// in the plugin-owned snapshot; OAuth credentials never leave the synchronizer.
type quotaSnapshotReadiness struct {
	Ready   []string          `json:"ready"`
	Pending []string          `json:"pending"`
	Errors  map[string]string `json:"errors,omitempty"`
}

func newQuotaSynchronizer(engine *quota.Engine) *quotaSynchronizer {
	ctx, cancel := context.WithCancel(context.Background())
	return &quotaSynchronizer{
		engine:          engine,
		ctx:             ctx,
		cancel:          cancel,
		inFlight:        make(map[string]struct{}),
		nextRefreshAt:   make(map[string]time.Time),
		refreshFailures: make(map[string]int),
		lastErrorLog:    make(map[string]time.Time),
		credentials:     make(map[string]cachedQuotaCredential),
		sourceIdentities: make(map[string]cachedSourceIdentity),
		readSourceIdentity: stableCodexAccountIdentity,
		jobs:            make(chan string, quotaSyncQueueSize),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
}

// BeginShutdownDrain keeps the plugin loaded while it safely waits for a
// terminal usage callback or an official weekly reset. Credential reads remain
// available in this phase: an access token can expire long before a weekly
// window ends, so cache-only operation here could make safe shutdown impossible.
func (syncer *quotaSynchronizer) BeginShutdownDrain() {
	if syncer == nil {
		return
	}
	syncer.refreshGate.Lock()
	syncer.lifecycleMu.Lock()
	if !syncer.closing {
		syncer.draining = true
	}
	syncer.lifecycleMu.Unlock()
	syncer.refreshGate.Unlock()
	// Do not wait for the normal five-minute cadence after admissions close:
	// only accounts that still own a durable pending reservation need an
	// official refresh to complete the safe drain.
	syncer.TriggerPendingNow()
}

// EnterShutdownMode starts the final shutdown phase after the accounting drain
// has completed. It prevents new auth-file reads before the native plugin is
// unloaded, while allowing an already cached credential to be discarded.
func (syncer *quotaSynchronizer) EnterShutdownMode() {
	if syncer == nil {
		return
	}
	syncer.refreshGate.Lock()
	syncer.lifecycleMu.Lock()
	syncer.cacheOnly = true
	syncer.lifecycleMu.Unlock()
	syncer.refreshGate.Unlock()
}

func (syncer *quotaSynchronizer) usesCachedCredentialsOnly() bool {
	if syncer == nil {
		return true
	}
	syncer.lifecycleMu.Lock()
	cacheOnly := syncer.cacheOnly
	syncer.lifecycleMu.Unlock()
	return cacheOnly
}

func (syncer *quotaSynchronizer) isDraining() bool {
	if syncer == nil {
		return false
	}
	syncer.lifecycleMu.Lock()
	draining := syncer.draining
	syncer.lifecycleMu.Unlock()
	return draining
}

func (syncer *quotaSynchronizer) Start() {
	if syncer == nil || syncer.engine == nil {
		return
	}
	syncer.start.Do(func() {
		syncer.lifecycleMu.Lock()
		if syncer.closing {
			syncer.lifecycleMu.Unlock()
			return
		}
		for worker := 0; worker < quotaSyncWorkers; worker++ {
			syncer.workers.Add(1)
			go syncer.runWorker()
		}
		go func() {
			defer close(syncer.done)
			syncer.TriggerAll()
			quotaTicker := time.NewTicker(quotaSyncInterval)
			defer quotaTicker.Stop()
			sourceTicker := time.NewTicker(accountSourceReconcileInterval)
			defer sourceTicker.Stop()
			for {
				select {
				case <-syncer.stop:
					return
				case <-quotaTicker.C:
					syncer.TriggerAll()
				case <-sourceTicker.C:
					if !syncer.isDraining() {
						// Keep source verification outside scheduler and refresh-worker
						// throttles. Close waits for this coordinator, so native unload
						// never leaves a scanner goroutine behind.
						syncer.RefreshAccountSourceConflict()
					}
				}
			}
		}()
		syncer.started = true
		syncer.lifecycleMu.Unlock()
	})
}

func (syncer *quotaSynchronizer) Close() {
	if syncer == nil {
		return
	}
	syncer.close.Do(func() {
		// Cancel direct official HTTP before waiting for any lifecycle lock. The
		// remaining auth-file reads never enter CPA's non-cancellable native
		// callback ABI. auth-dir is intentionally required to live on a healthy
		// host-local filesystem; a dead remote mount cannot be cancelled by Go.
		syncer.cancel()
		syncer.refreshGate.Lock()
		syncer.lifecycleMu.Lock()
		syncer.closing = true
		syncer.cacheOnly = true
		started := syncer.started
		syncer.lifecycleMu.Unlock()
		syncer.refreshGate.Unlock()
		close(syncer.stop)
		if started {
			// Native plugins may be unloaded as soon as the shutdown callback
			// returns. Do not leave a quota worker or a WaitGroup helper running
			// after that boundary; the canceled request must finish before Close does.
			<-syncer.done
			syncer.workers.Wait()
		}
		// Management-triggered source scans do not use refreshGate (a refresh
		// worker may already hold its read lock while requesting one). Once
		// closing is visible, no new scan may start; wait for any existing scan
		// before native code can be unloaded.
		syncer.sourceScanMu.Lock()
		syncer.sourceScanMu.Unlock()
		syncer.mu.Lock()
		clear(syncer.credentials)
		clear(syncer.sourceIdentities)
		syncer.mu.Unlock()
	})
}

func (syncer *quotaSynchronizer) TriggerAll() {
	if syncer.isDraining() {
		syncer.TriggerPending()
		return
	}
	syncer.triggerAll(false)
}

func (syncer *quotaSynchronizer) TriggerAllNow() {
	if syncer.isDraining() {
		syncer.TriggerPendingNow()
		return
	}
	syncer.triggerAll(true)
}

// TriggerPending schedules only accounts that still block a graceful close.
// It is intentionally private: normal scheduler traffic continues to use the
// broader stale-candidate path while the plugin is serving requests.
func (syncer *quotaSynchronizer) TriggerPending() {
	if syncer == nil || syncer.engine == nil {
		return
	}
	syncer.trigger(syncer.engine.PendingSettlementAuthIDs(), false)
}

func (syncer *quotaSynchronizer) TriggerPendingNow() {
	if syncer == nil || syncer.engine == nil {
		return
	}
	syncer.trigger(syncer.engine.PendingSettlementAuthIDs(), true)
}

func (syncer *quotaSynchronizer) triggerAll(force bool) {
	if syncer == nil || syncer.engine == nil {
		return
	}
	// Auth files are normally replaced atomically, but an operator can retarget
	// an internal symlink without changing plugin settings. Recheck physical
	// sources on the bounded background cadence so an alias can never continue
	// to count the same official Codex account twice after that change.
	syncer.RefreshAccountSourceConflict()
	accounts := syncer.engine.AccountPool(time.Now().UTC())
	authIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.Enabled {
			authIDs = append(authIDs, account.AuthID)
		}
	}
	syncer.trigger(authIDs, force)
}

func (syncer *quotaSynchronizer) Trigger(authIDs []string) {
	syncer.trigger(authIDs, false)
}

// TriggerNow is reserved for a newly configured account. Scheduler traffic
// must use Trigger so it cannot turn a stale/error snapshot into an upstream
// polling loop; management requests use RequestManualRefresh for a global
// cooldown before they enter this force path.
func (syncer *quotaSynchronizer) TriggerNow(authIDs []string) {
	_ = syncer.trigger(authIDs, true)
}

// WaitForUsableSnapshot waits only for a refresh job that has already been
// queued by a management action. It never starts an official request itself,
// so a model request cannot turn a missing snapshot into an upstream polling
// loop. One usable snapshot is sufficient: the router can then safely select
// that account while the remaining batch entries continue their spaced sync.
func (syncer *quotaSynchronizer) WaitForUsableSnapshot(authIDs []string, timeout time.Duration) quotaSnapshotReadiness {
	return syncer.waitForUsableSnapshot(authIDs, time.Time{}, timeout)
}

// WaitForRefreshedUsableSnapshot is the stricter management variant used
// after changing an auth directory or re-saving an account. A pre-existing
// snapshot cannot satisfy it: the account must publish a snapshot observed
// after this management action started.
func (syncer *quotaSynchronizer) WaitForRefreshedUsableSnapshot(authIDs []string, notBefore time.Time, timeout time.Duration) quotaSnapshotReadiness {
	return syncer.waitForUsableSnapshot(authIDs, notBefore, timeout)
}

func (syncer *quotaSynchronizer) waitForUsableSnapshot(authIDs []string, notBefore time.Time, timeout time.Duration) quotaSnapshotReadiness {
	if syncer == nil || syncer.engine == nil {
		return quotaSnapshotReadiness{Pending: normalizedAuthIDs(authIDs)}
	}
	if timeout <= 0 {
		timeout = quotaInitialSnapshotWait
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for {
		readiness := syncer.snapshotReadiness(authIDs, notBefore)
		if len(readiness.Ready) > 0 || len(readiness.Pending) == 0 {
			return readiness
		}
		select {
		case <-syncer.ctx.Done():
			return readiness
		case <-deadline.C:
			return readiness
		case <-ticker.C:
		}
	}
}

func (syncer *quotaSynchronizer) snapshotReadiness(authIDs []string, notBefore time.Time) quotaSnapshotReadiness {
	wanted := make(map[string]struct{})
	for _, authID := range normalizedAuthIDs(authIDs) {
		wanted[authID] = struct{}{}
	}
	readiness := quotaSnapshotReadiness{Errors: make(map[string]string)}
	for _, account := range syncer.engine.AccountPool(time.Now().UTC()) {
		if _, requested := wanted[account.AuthID]; !requested || !account.Enabled {
			continue
		}
		delete(wanted, account.AuthID)
		if account.Quota == nil {
			readiness.Pending = append(readiness.Pending, account.AuthID)
			continue
		}
		if message := strings.TrimSpace(account.Quota.LastError); message != "" {
			readiness.Errors[account.AuthID] = message
			continue
		}
		if !account.Fresh || account.Quota.ObservedAt.IsZero() || (!notBefore.IsZero() && !account.Quota.ObservedAt.After(notBefore)) || account.Quota.Secondary.ResetAt == nil || account.Quota.SecondaryEstimatedResetCandidateAt != nil {
			readiness.Pending = append(readiness.Pending, account.AuthID)
			continue
		}
		readiness.Ready = append(readiness.Ready, account.AuthID)
	}
	for authID := range wanted {
		readiness.Pending = append(readiness.Pending, authID)
	}
	sort.Strings(readiness.Ready)
	sort.Strings(readiness.Pending)
	if len(readiness.Errors) == 0 {
		readiness.Errors = nil
	}
	return readiness
}

func normalizedAuthIDs(authIDs []string) []string {
	seen := make(map[string]struct{}, len(authIDs))
	result := make([]string, 0, len(authIDs))
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, duplicate := seen[authID]; duplicate {
			continue
		}
		seen[authID] = struct{}{}
		result = append(result, authID)
	}
	sort.Strings(result)
	return result
}

// RequestManualRefresh applies a synchronizer-wide cooldown before bypassing
// per-account refresh backoff. It is intentionally separate from TriggerNow:
// saving a new account needs an immediate first snapshot, while repeated panel
// clicks must not burst the official Codex usage endpoint.
func (syncer *quotaSynchronizer) RequestManualRefresh(authIDs []string) (quotaRefreshSchedule, time.Duration) {
	if syncer == nil || syncer.engine == nil {
		return quotaRefreshSchedule{}, 0
	}
	// Do not hold syncer.mu while trigger calls startRefresh. This narrow mutex
	// serializes panel clicks so two callers cannot both pass the global manual
	// cooldown and schedule a burst.
	syncer.manualRefreshMu.Lock()
	defer syncer.manualRefreshMu.Unlock()
	now := time.Now().UTC()
	syncer.mu.Lock()
	availableAt := syncer.lastManualRefreshAt.Add(manualQuotaRefreshCooldown)
	if !syncer.lastManualRefreshAt.IsZero() && now.Before(availableAt) {
		syncer.mu.Unlock()
		return quotaRefreshSchedule{}, availableAt.Sub(now)
	}
	syncer.mu.Unlock()
	result := syncer.trigger(authIDs, true)
	if result.Scheduled == 0 {
		// A full queue or already-running account did not send upstream traffic,
		// so it must not consume the operator's manual-refresh cooldown.
		return result, 0
	}
	syncer.mu.Lock()
	syncer.lastManualRefreshAt = now
	syncer.mu.Unlock()
	return result, 0
}

func (syncer *quotaSynchronizer) trigger(authIDs []string, force bool) quotaRefreshSchedule {
	result := quotaRefreshSchedule{}
	if syncer == nil || syncer.engine == nil {
		return result
	}
	if syncer.isDraining() {
		// A management refresh can race with native shutdown. Once admissions
		// are closed, no unrelated account may start a new auth-file read:
		// only durable pending settlements are still relevant to the drain.
		authIDs = syncer.engine.PendingSettlementAuthIDs()
	}
	seen := make(map[string]struct{}, len(authIDs))
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			result.SkippedUnavailable++
			continue
		}
		if _, duplicate := seen[authID]; duplicate {
			continue
		}
		seen[authID] = struct{}{}
		entry, exists := syncer.engine.AccountByID(authID)
		if !exists || !entry.Enabled {
			result.SkippedUnavailable++
			continue
		}
		if !syncer.startRefresh(authID, force) {
			result.SkippedInFlight++
			continue
		}
		// Scheduler and management requests must never wait for upstream quota
		// I/O. If the bounded queue is full, the next five-minute tick (or a
		// later request) will retry the account.
		select {
		case <-syncer.stop:
			syncer.finishRefresh(authID, false)
			result.SkippedUnavailable++
		case syncer.jobs <- authID:
			result.Scheduled++
		default:
			syncer.finishRefresh(authID, false)
			result.QueueFull++
		}
	}
	return result
}

func (syncer *quotaSynchronizer) runWorker() {
	defer syncer.workers.Done()
	for {
		select {
		case <-syncer.stop:
			return
		case authID := <-syncer.jobs:
			syncer.refreshQueued(authID)
		}
	}
}

func (syncer *quotaSynchronizer) refreshQueued(authID string) {
	select {
	case <-syncer.stop:
		syncer.finishRefresh(authID, false)
		return
	default:
	}
	syncer.completeQueuedRefresh(authID, syncer.refreshOne(authID))
}

// completeQueuedRefresh applies the outcome after a worker has released its
// refresh gate. Cancellation of the synchronizer's own root context is an
// expected part of native-plugin shutdown, not an official quota failure.
func (syncer *quotaSynchronizer) completeQueuedRefresh(authID string, err error) {
	if errors.Is(err, errQuotaRefreshSkipped) || syncer.isShutdownCancellation(err) {
		// A drain-only account was settled, removed, or superseded while this
		// job waited in the bounded queue. A cancellation from Close is likewise
		// not a failed refresh. Retain prior backoff and diagnosis in both cases.
		syncer.discardRefresh(authID)
		return
	}
	if err != nil {
		syncer.finishRefresh(authID, false)
		syncer.recordError(authID, err)
		return
	}
	syncer.finishRefresh(authID, true)
	if syncer.clearRefreshError(authID) {
		syncer.engine.LogOperational("info", "quota_sync_recovered", "官方额度同步已恢复", authID, "")
	}
}

func (syncer *quotaSynchronizer) isShutdownCancellation(err error) bool {
	return syncer != nil && syncer.ctx != nil && errors.Is(err, context.Canceled) && errors.Is(syncer.ctx.Err(), context.Canceled)
}

func (syncer *quotaSynchronizer) startRefresh(authID string, force bool) bool {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if _, exists := syncer.inFlight[authID]; exists {
		return false
	}
	if !force && time.Now().UTC().Before(syncer.nextRefreshAt[authID]) {
		return false
	}
	syncer.inFlight[authID] = struct{}{}
	return true
}

func (syncer *quotaSynchronizer) finishRefresh(authID string, succeeded bool) {
	now := time.Now().UTC()
	confirmationAt := time.Time{}
	if succeeded && syncer != nil && syncer.engine != nil {
		if candidateAt, pending := syncer.engine.PendingEstimatedResetConfirmationAt(authID); pending {
			confirmationAt = candidateAt
		}
	}
	syncer.mu.Lock()
	if succeeded {
		delete(syncer.refreshFailures, authID)
		nextRefreshAt := now.Add(quotaSyncInterval)
		if confirmationAt.After(now) && confirmationAt.Before(nextRefreshAt) {
			// The candidate stays blocked in the scheduler, so schedule exactly
			// one timely confirmation rather than leaving traffic unavailable for
			// the ordinary five-minute poll interval.
			nextRefreshAt = confirmationAt
		}
		syncer.nextRefreshAt[authID] = nextRefreshAt
	} else {
		failures := syncer.refreshFailures[authID] + 1
		if failures > 8 {
			failures = 8
		}
		syncer.refreshFailures[authID] = failures
		backoff := quotaSyncFailureInitial << (failures - 1)
		if backoff > quotaSyncFailureMaximum {
			backoff = quotaSyncFailureMaximum
		}
		syncer.nextRefreshAt[authID] = now.Add(backoff)
	}
	delete(syncer.inFlight, authID)
	syncer.mu.Unlock()
}

func (syncer *quotaSynchronizer) discardRefresh(authID string) {
	if syncer == nil {
		return
	}
	syncer.mu.Lock()
	delete(syncer.inFlight, authID)
	syncer.mu.Unlock()
}

func (syncer *quotaSynchronizer) refreshOne(authID string) error {
	release, allowed := syncer.beginRefresh(authID)
	if !allowed {
		// A queued job either belongs to an account whose terminal callback has
		// already settled, or arrived after final shutdown started.
		return errQuotaRefreshSkipped
	}
	defer release()
	if !syncer.isDraining() {
		// This happens only in a bounded background worker, never in the CPA
		// scheduler callback. It narrows the window in which a retargeted alias
		// could otherwise retain an old account snapshot until the next tick.
		syncer.RefreshAccountSourceConflictIfDue()
	}
	entry, exists := syncer.engine.AccountByID(authID)
	if !exists || !entry.Enabled {
		return errQuotaRefreshSkipped
	}
	accessToken, accountID, err := syncer.resolveCredentialWhileRefreshing(entry.AuthID)
	if err != nil {
		return err
	}
	if err := syncer.waitForOfficialQuotaSlot(); err != nil {
		return err
	}
	snapshot, err := fetchOfficialQuota(syncer.ctx, accessToken, accountID)
	if err != nil {
		if isCredentialAuthorizationFailure(err) {
			// CPA may rotate a credential before its advertised JWT expiry. Drop
			// only this process-memory entry so the backoff retry reads the active
			// credential again, rather than reusing a known-invalid token.
			syncer.invalidateCredential(entry.AuthID)
		}
		return err
	}
	snapshot.AuthID = entry.AuthID
	snapshot.ObservedAt = time.Now().UTC()
	return syncer.engine.UpdateOfficialQuota(snapshot)
}

// waitForOfficialQuotaSlot serializes only the outbound official usage reads.
// Credential parsing and SQLite persistence remain concurrent, while every
// HTTP request is spaced globally and is cancelable during native shutdown.
func (syncer *quotaSynchronizer) waitForOfficialQuotaSlot() error {
	if syncer == nil || syncer.ctx == nil {
		return fmt.Errorf("quota synchronizer is unavailable")
	}
	now := time.Now().UTC()
	syncer.mu.Lock()
	slot := syncer.nextOfficialRequestAt
	if slot.Before(now) {
		slot = now
	}
	syncer.nextOfficialRequestAt = slot.Add(quotaSyncMinimumSpacing)
	syncer.mu.Unlock()
	if delay := time.Until(slot); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-syncer.ctx.Done():
			return syncer.ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// beginRefresh admits a whole refresh under one read lock. BeginShutdownDrain
// takes the write lock before changing state, so once it returns no unrelated
// queued refresh can advance into either an auth-file read or a direct quota request.
func (syncer *quotaSynchronizer) beginRefresh(authID string) (func(), bool) {
	if syncer == nil || syncer.engine == nil || strings.TrimSpace(authID) == "" {
		return func() {}, false
	}
	syncer.refreshGate.RLock()
	syncer.lifecycleMu.Lock()
	allowed := !syncer.closing && !syncer.cacheOnly && (!syncer.draining || syncer.engine.HasPendingSettlementForAuth(authID))
	syncer.lifecycleMu.Unlock()
	if !allowed {
		syncer.refreshGate.RUnlock()
		return func() {}, false
	}
	return syncer.refreshGate.RUnlock, true
}

func (syncer *quotaSynchronizer) resolveCredential(authID string) (string, string, error) {
	if syncer == nil {
		return "", "", fmt.Errorf("quota synchronizer is unavailable")
	}
	syncer.refreshGate.RLock()
	defer syncer.refreshGate.RUnlock()
	return syncer.resolveCredentialWhileRefreshing(authID)
}

func (syncer *quotaSynchronizer) resolveCredentialWhileRefreshing(authID string) (string, string, error) {
	syncer.lifecycleMu.Lock()
	if syncer.cacheOnly || syncer.closing {
		syncer.lifecycleMu.Unlock()
		credential, ok := syncer.cachedCredential(authID)
		if !ok {
			return "", "", fmt.Errorf("shutdown quota refresh requires a cached CPA credential")
		}
		return credential.accessToken, credential.accountID, nil
	}
	syncer.lifecycleMu.Unlock()

	path, info, err := syncer.authFilePath(authID)
	if err != nil {
		return "", "", err
	}
	if credential, ok := syncer.freshCachedCredential(authID, path, info, time.Now().UTC()); ok {
		return credential.accessToken, credential.accountID, nil
	}
	credential, err := readCodexQuotaCredential(path)
	if err != nil {
		if errors.Is(err, errTransientCodexAuthFile) {
			// CPA can briefly expose a truncated JSON file while rotating an OAuth
			// credential. Reuse a token only when the same physical file version
			// is still present; otherwise a same-size/same-mtime rewrite could make
			// the plugin fetch one Codex account while CPA routes another.
			latestInfo := info
			if refreshedInfo, statErr := os.Stat(path); statErr == nil {
				latestInfo = refreshedInfo
			}
			if cached, ok := syncer.unexpiredCachedCredential(authID, path, latestInfo, time.Now().UTC()); ok {
				return cached.accessToken, cached.accountID, nil
			}
		}
		return "", "", err
	}
	// Cache only after the file has been completely parsed. The value stays in
	// process memory and is invalidated whenever CPA atomically replaces the
	// auth file, changes its timestamp, or changes directories.
	syncer.cacheCredential(authID, credential.accessToken, credential.accountID, path, info, credential.contentDigest)
	return credential.accessToken, credential.accountID, nil
}

func (syncer *quotaSynchronizer) authFilePath(authID string) (string, fs.FileInfo, error) {
	if syncer == nil || syncer.engine == nil {
		return "", nil, fmt.Errorf("quota synchronizer is unavailable")
	}
	return resolveCodexAuthFile(syncer.engine.AuthDirectory(), authID)
}

func (syncer *quotaSynchronizer) cacheCredential(authID, accessToken, accountID, sourcePath string, info fs.FileInfo, suppliedDigest ...string) {
	if syncer == nil || strings.TrimSpace(authID) == "" || strings.TrimSpace(accessToken) == "" {
		return
	}
	if info == nil {
		return
	}
	contentDigest := ""
	if len(suppliedDigest) > 0 {
		contentDigest = strings.TrimSpace(suppliedDigest[0])
	}
	if contentDigest == "" {
		var err error
		contentDigest, err = codexAuthFileContentDigest(sourcePath, info)
		if err != nil {
			return
		}
	}
	syncer.mu.Lock()
	syncer.credentials[authID] = cachedQuotaCredential{
		accessToken:    accessToken,
		accountID:      accountID,
		expiresAt:      quotaCredentialExpiresAt(accessToken, time.Now().UTC()),
		sourcePath:     sourcePath,
		physicalID:     codexAuthFileIdentity(info),
		fileModifiedAt: info.ModTime().UTC(),
		fileChangedAt:  codexAuthFileChangeTime(info),
		fileSize:       info.Size(),
		contentDigest:  contentDigest,
	}
	syncer.mu.Unlock()
}

func (syncer *quotaSynchronizer) cachedCredential(authID string) (cachedQuotaCredential, bool) {
	if syncer == nil {
		return cachedQuotaCredential{}, false
	}
	syncer.mu.Lock()
	credential, ok := syncer.credentials[authID]
	syncer.mu.Unlock()
	return credential, ok
}

// freshCachedCredential avoids reparsing an unchanged CPA auth file for every
// quota poll. The cache remains process-only and is refreshed before a JWT
// expiry; opaque tokens use a deliberately short conservative lifetime instead.
func (syncer *quotaSynchronizer) freshCachedCredential(authID, sourcePath string, info fs.FileInfo, now time.Time) (cachedQuotaCredential, bool) {
	credential, ok := syncer.cachedCredential(authID)
	if !ok || !credentialMatchesSource(credential, sourcePath, info) ||
		credential.expiresAt.IsZero() || !now.Add(quotaCredentialRefreshSkew).Before(credential.expiresAt) {
		return cachedQuotaCredential{}, false
	}
	return credential, true
}

func (syncer *quotaSynchronizer) unexpiredCachedCredential(authID, sourcePath string, info fs.FileInfo, now time.Time) (cachedQuotaCredential, bool) {
	credential, ok := syncer.cachedCredential(authID)
	if !ok || !credentialMatchesSource(credential, sourcePath, info) || credential.expiresAt.IsZero() || !now.Add(quotaCredentialRefreshSkew).Before(credential.expiresAt) {
		return cachedQuotaCredential{}, false
	}
	return credential, true
}

// credentialMatchesSource keeps quota polling tied to the current CPA file
// version. The process-only content digest closes a same-size/same-mtime
// rewrite even if the filesystem exposes coarse ctime.
func credentialMatchesSource(credential cachedQuotaCredential, sourcePath string, info fs.FileInfo) bool {
	if info == nil || credential.sourcePath != sourcePath ||
		credential.physicalID != codexAuthFileIdentity(info) ||
		!credential.fileModifiedAt.Equal(info.ModTime().UTC()) ||
		!credential.fileChangedAt.Equal(codexAuthFileChangeTime(info)) ||
		credential.fileSize != info.Size() {
		return false
	}
	contentDigest, err := codexAuthFileContentDigest(sourcePath, info)
	return err == nil && contentDigest == credential.contentDigest
}

func (syncer *quotaSynchronizer) invalidateCredential(authID string) {
	if syncer == nil || strings.TrimSpace(authID) == "" {
		return
	}
	syncer.mu.Lock()
	delete(syncer.credentials, authID)
	syncer.mu.Unlock()
}

// ClearCredentials removes only process-memory OAuth material. It is called
// after the operator changes auth-dir so a later refresh cannot reuse a token
// resolved from the previous CPA credential store.
func (syncer *quotaSynchronizer) ClearCredentials() {
	if syncer == nil {
		return
	}
	syncer.mu.Lock()
	clear(syncer.credentials)
	// Auth-dir changes call ClearCredentials before a forced source scan. Drop
	// the old directory's identity cache at the same boundary.
	clear(syncer.sourceIdentities)
	syncer.mu.Unlock()
}

// RefreshAccountSourceConflict detects historical aliases and copied OAuth
// files that resolve to one stable Codex account. New saves are rejected
// earlier, but this startup/configuration pass keeps old SQLite rows
// fail-closed before they can double-count official capacity.
func (syncer *quotaSynchronizer) RefreshAccountSourceConflict() {
	syncer.refreshAccountSourceConflict(true)
}

// RefreshAccountSourceConflictIfDue is the bounded-cadence form used by the
// source coordinator and before direct quota refreshes. It must not be called
// by scheduler code.
func (syncer *quotaSynchronizer) RefreshAccountSourceConflictIfDue() {
	syncer.refreshAccountSourceConflict(false)
}

func (syncer *quotaSynchronizer) sourceScanAllowed() bool {
	if syncer == nil {
		return false
	}
	syncer.lifecycleMu.Lock()
	allowed := !syncer.closing
	syncer.lifecycleMu.Unlock()
	return allowed
}

func (syncer *quotaSynchronizer) refreshAccountSourceConflict(force bool) {
	if syncer == nil || syncer.engine == nil {
		return
	}
	if !syncer.sourceScanAllowed() {
		return
	}
	syncer.sourceScanMu.Lock()
	defer syncer.sourceScanMu.Unlock()
	if !syncer.sourceScanAllowed() {
		return
	}
	now := time.Now().UTC()
	syncer.mu.Lock()
	if !force && !syncer.sourceConflictCheckedAt.IsZero() && now.Sub(syncer.sourceConflictCheckedAt) < accountSourceReconcileInterval {
		syncer.mu.Unlock()
		return
	}
	syncer.mu.Unlock()
	sourceRevision := syncer.engine.AccountSourceRevision()
	root, rootErr := resolveCodexAuthDirectory(syncer.engine.AuthDirectory())
	seenPaths := make(map[string]struct{})
	seenPhysicalFiles := make(map[string]struct{})
	seenSources := 0
	identities := make(map[string]struct{})
	unknownIdentitySource := false
	conflict := false
	uncertain := false
	reason := ""
	if rootErr != nil {
		uncertain = true
	} else {
		for _, account := range syncer.engine.AccountPool(now) {
			if !account.Enabled {
				continue
			}
			path, info, err := resolveCodexAuthFileFromRoot(root, account.AuthID)
			if err != nil {
				uncertain = true
				continue
			}
			physicalID := codexAuthFileIdentity(info)
			if _, exists := seenPaths[path]; exists {
				conflict = true
				reason = "检测到多个账号池条目指向同一 CPA 认证文件"
				break
			}
			if physicalID != "" {
				if _, exists := seenPhysicalFiles[physicalID]; exists {
					conflict = true
					reason = "检测到多个账号池条目指向同一 CPA 认证文件"
					break
				}
			}
			identity, identityMissing, err := syncer.sourceIdentity(path, info)
			if err != nil {
				// A temporary file rewrite is still unprovable and PublishAccountSourceScan
				// below keeps managed admissions closed until a complete pass succeeds.
				uncertain = true
				continue
			}
			if identityMissing {
				if unknownIdentitySource || seenSources > 0 {
					conflict = true
					reason = "多个账号池条目中存在无法确认稳定 Codex 身份的认证文件"
					break
				}
				unknownIdentitySource = true
				seenPaths[path] = struct{}{}
				if physicalID != "" {
					seenPhysicalFiles[physicalID] = struct{}{}
				}
				seenSources++
				continue
			}
			if unknownIdentitySource {
				conflict = true
				reason = "无法确认稳定 Codex 身份的认证文件不能与其他账号池条目混用"
				break
			}
			if _, exists := identities[identity]; exists {
				conflict = true
				reason = "检测到多个账号池条目属于同一 Codex 账号"
				break
			}
			identities[identity] = struct{}{}
			seenPaths[path] = struct{}{}
			if physicalID != "" {
				seenPhysicalFiles[physicalID] = struct{}{}
			}
			seenSources++
		}
	}
	syncer.mu.Lock()
	// A transient file rewrite must never clear an already confirmed duplicate:
	// until every enabled entry can be proven distinct again, retaining the 503
	// guard is safer than reopening a potentially double-counted account pool.
	// sourceScanMu serializes scans and Engine publishes their generation-aware
	// result below, so this state only represents the current configuration.
	if !conflict && uncertain && unknownIdentitySource {
		// A single identity-less source is permitted only while it is the only
		// account whose distinctness must be established. If another configured
		// source is temporarily unreadable, opening the pool would again make
		// that guarantee impossible.
		conflict = true
		reason = "无法确认稳定身份的账号池条目暂时无法与其他认证文件完成复核"
	}
	if !conflict && uncertain && syncer.sourceConflict {
		conflict = true
		reason = syncer.sourceConflictReason
		if reason == "" {
			reason = "账号池身份暂时无法完整复核"
		}
	}
	// Engine publishes the generation comparison and guard transition under one
	// lock. Evaluate every local fail-closed rule first, otherwise a transient
	// rewrite could leave syncer.sourceConflict=true after Engine was reopened.
	complete := !conflict && !uncertain
	if !syncer.engine.PublishAccountSourceScan(sourceRevision, conflict, complete) {
		syncer.mu.Unlock()
		return
	}
	changed := syncer.sourceConflict != conflict || (conflict && syncer.sourceConflictReason != reason)
	syncer.sourceConflict = conflict
	if conflict {
		syncer.sourceConflictReason = reason
	} else {
		syncer.sourceConflictReason = ""
	}
	// Record completion rather than scan start. A slow scan therefore cannot
	// immediately trigger a second full JSON pass from an already-fired ticker.
	syncer.sourceConflictCheckedAt = time.Now().UTC()
	syncer.mu.Unlock()
	if changed && conflict {
		syncer.engine.LogOperational("error", "duplicate_auth_source", reason+"；受控 Key 已安全暂停", "", "")
	}
	if changed && !conflict {
		syncer.engine.LogOperational("info", "duplicate_auth_source_resolved", "重复 CPA 认证文件配置已解除，受控 Key 已恢复", "", "")
	}
}

func quotaCredentialExpiresAt(accessToken string, now time.Time) time.Time {
	parts := strings.Split(strings.TrimSpace(accessToken), ".")
	if len(parts) >= 2 {
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			payload, err = base64.URLEncoding.DecodeString(parts[1])
		}
		if err == nil {
			var claims struct {
				ExpiresAt int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt > 0 {
				return time.Unix(claims.ExpiresAt, 0).UTC()
			}
		}
	}
	return now.UTC().Add(quotaCredentialFallbackTTL)
}

func (syncer *quotaSynchronizer) recordError(authID string, cause error) {
	if syncer == nil || syncer.engine == nil || strings.TrimSpace(authID) == "" || cause == nil {
		return
	}
	if errors.Is(cause, errTransientCodexAuthFile) {
		// A CPA in-place rewrite is not evidence that the last confirmed Codex
		// snapshot is wrong. Keep that snapshot usable until its normal hard age,
		// while the retry backoff verifies the newly written credential. Replacing
		// it with LastError here would turn a millisecond-sized auth-file rewrite
		// into an immediate 503 for an otherwise healthy shared pool.
		if syncer.shouldLogRefreshError(authID) {
			syncer.engine.LogOperational("warn", "quota_sync_auth_file_retry", safeOperationalError(cause), authID, "")
		}
		return
	}
	// A stale successful snapshot remains visible for diagnosis but cannot be
	// selected after its hard age. Keep the same redacted message in the
	// snapshot and operational log so neither SQLite path can retain a token.
	message := safeOperationalError(cause)
	current := quota.OfficialQuotaSnapshot{AuthID: authID, ObservedAt: time.Now().UTC(), LastError: message}
	for _, account := range syncer.engine.AccountPool(time.Now().UTC()) {
		if account.AuthID == authID && account.Quota != nil {
			current = *account.Quota
			current.LastError = message
			break
		}
	}
	_ = syncer.engine.UpdateOfficialQuota(current)
	if syncer.shouldLogRefreshError(authID) {
		syncer.engine.LogOperational("error", "quota_sync_failed", message, authID, "")
	}
}

// Repeated network failures are persisted at most once per account per
// synchronizer interval. This keeps the useful error trail without turning a
// broken upstream link into an unbounded write stream.
func (syncer *quotaSynchronizer) shouldLogRefreshError(authID string) bool {
	now := time.Now().UTC()
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if previous := syncer.lastErrorLog[authID]; !previous.IsZero() && now.Sub(previous) < quotaSyncInterval {
		return false
	}
	syncer.lastErrorLog[authID] = now
	return true
}

func (syncer *quotaSynchronizer) clearRefreshError(authID string) bool {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	if _, exists := syncer.lastErrorLog[authID]; !exists {
		return false
	}
	delete(syncer.lastErrorLog, authID)
	return true
}

func safeOperationalError(cause error) string {
	if cause == nil {
		return "官方额度同步失败"
	}
	message := strings.TrimSpace(cause.Error())
	lower := strings.ToLower(message)
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "access_token") || strings.Contains(lower, "id_token") {
		return "官方额度同步失败（敏感错误内容已隐藏）"
	}
	if len(message) > 480 {
		message = message[:480] + "…"
	}
	if message == "" {
		return "官方额度同步失败"
	}
	return "官方额度同步失败：" + message
}

// MarkRateLimited applies a Codex 429 immediately. The follow-up refresh is
// still required for a current weekly snapshot, but concurrent scheduler calls
// stop selecting this account before that network round trip ends.
func (syncer *quotaSynchronizer) MarkRateLimited(authID, failureBody string) {
	if syncer == nil || syncer.engine == nil || strings.TrimSpace(authID) == "" {
		return
	}
	snapshot := quota.OfficialQuotaSnapshot{AuthID: strings.TrimSpace(authID), Allowed: false, LimitReached: true, ObservedAt: time.Now().UTC()}
	for _, account := range syncer.engine.AccountPool(snapshot.ObservedAt) {
		if account.AuthID == snapshot.AuthID && account.Quota != nil {
			snapshot = *account.Quota
			snapshot.Allowed = false
			snapshot.LimitReached = true
			snapshot.ObservedAt = time.Now().UTC()
			break
		}
	}
	// A real upstream 429 is stronger evidence than any older refresh error.
	// Clear the diagnostic error so routing immediately treats this as a
	// confirmed exhaustion and returns 429 until the queued refresh recovers it.
	snapshot.LastError = ""
	var payload struct {
		Error struct {
			ResetsAt        json.RawMessage `json:"resets_at"`
			ResetsInSeconds json.RawMessage `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(failureBody), &payload) == nil {
		if resetAt, err := rawUnixTime(payload.Error.ResetsAt); err == nil && resetAt != nil {
			snapshot.Secondary.ResetAt = resetAt
		} else if seconds, err := rawInt(payload.Error.ResetsInSeconds); err == nil && seconds > 0 {
			value := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
			snapshot.Secondary.ResetAt = &value
		}
	}
	_ = syncer.engine.UpdateOfficialQuota(snapshot)
	syncer.engine.LogOperational("warn", "official_quota_exhausted", "官方额度已耗尽，账号已暂时停止调度", snapshot.AuthID, "")
}

// discoverCodexAccounts mirrors CPA's file-store identity: a scheduler auth
// ID is the path relative to auth-dir. This keeps a selected candidate and its
// local credential tied to the same CPA account without a Host RPC.
func discoverCodexAccounts() ([]quota.AccountPoolEntry, error) {
	engine := currentEngine()
	if engine == nil {
		return nil, fmt.Errorf("codex-carpool is not initialized")
	}
	root, err := resolveCodexAuthDirectory(engine.AuthDirectory())
	if err != nil {
		return nil, err
	}
	accounts := make([]quota.AccountPoolEntry, 0)
	type discoveredSource struct {
		path string
		info fs.FileInfo
		identity string
	}
	sources := make([]discoveredSource, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A malformed or unreadable sibling must not hide healthy Codex
			// accounts. A root failure is still actionable and returned.
			if path == root {
				return walkErr
			}
			return nil
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// CPA keeps the logical path encountered during WalkDir as the auth ID.
		// Resolve the file separately so a JSON symlink remains supported only
		// when its final target stays inside the configured auth-dir.
		resolved, info, err := resolveCodexAuthFile(engine.AuthDirectory(), filepath.ToSlash(relative))
		if err != nil {
			return nil
		}
		for _, source := range sources {
			if source.path == resolved || os.SameFile(source.info, info) {
				return nil
			}
		}
		file, err := readCodexAuthFile(resolved)
		if err != nil || !file.isCodex() || file.Disabled {
			return nil
		}
		identity, err := stableCodexAccountIdentity(resolved)
		if err != nil && !errors.Is(err, errMissingCodexAccountIdentity) {
			return nil
		}
		for _, source := range sources {
			if identity != "" && source.identity == identity {
				return nil
			}
		}
		name := file.displayName(filepath.Base(path))
		accounts = append(accounts, quota.AccountPoolEntry{
			AuthID:    filepath.ToSlash(relative),
			Name:      name,
			CapacityX: codexPlanCapacityX(file.planType()),
			Enabled:   true,
		})
		sources = append(sources, discoveredSource{path: resolved, info: info, identity: identity})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan CPA auth directory: %w", err)
	}
	sort.Slice(accounts, func(left, right int) bool {
		if accounts[left].Name == accounts[right].Name {
			return accounts[left].AuthID < accounts[right].AuthID
		}
		return accounts[left].Name < accounts[right].Name
	})
	return accounts, nil
}

type codexAuthFile struct {
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	Email       string `json:"email"`
	Label       string `json:"label"`
	Name        string `json:"name"`
	PlanType    string `json:"plan_type"`
	Disabled    bool   `json:"disabled"`
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	AccountID   string `json:"account_id"`
	Tokens      struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
	// FileTokenStore also supports the generic singular token object. Keep it
	// alongside the Codex-native top-level fields so a CPA-readable auth JSON
	// cannot become unavailable only when codex-carpool refreshes its quota.
	Token struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		AccountID   string `json:"account_id"`
	} `json:"token"`
}

func (file codexAuthFile) isCodex() bool {
	return strings.EqualFold(strings.TrimSpace(file.Type), "codex") || strings.EqualFold(strings.TrimSpace(file.Provider), "codex")
}

func (file codexAuthFile) displayName(fallback string) string {
	for _, value := range []string{file.Label, file.Email, file.Name, fallback} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "Codex 账号"
}

// planType follows CPA's persisted account metadata first and uses the ID-token
// claim only as a compatibility fallback for older Codex auth files.
func (file codexAuthFile) planType() string {
	if planType := strings.TrimSpace(file.PlanType); planType != "" {
		return planType
	}
	return chatGPTPlanType(file.idToken())
}

func (file codexAuthFile) idToken() string {
	for _, value := range []string{file.IDToken, file.Tokens.IDToken, file.Token.IDToken} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// codexPlanCapacityX converts the subscription identity CPA already stores into
// the same x unit used by account-pool and Key allocations. Unknown plans keep
// the conservative 1x fallback and remain editable by the operator.
func codexPlanCapacityX(planType string) float64 {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "pro", "prolite", "pro_lite", "pro-lite", "chatgpt_pro":
		return 20
	default:
		return 1
	}
}

type codexQuotaCredential struct {
	accessToken   string
	accountID     string
	contentDigest string
}

func readCodexQuotaCredential(path string) (codexQuotaCredential, error) {
	file, contentDigest, err := readCodexAuthFileWithDigest(path)
	if err != nil {
		return codexQuotaCredential{}, err
	}
	if !file.isCodex() {
		return codexQuotaCredential{}, fmt.Errorf("CPA auth file is not a Codex credential")
	}
	if file.Disabled {
		return codexQuotaCredential{}, fmt.Errorf("CPA Codex credential is disabled")
	}
	accessToken := strings.TrimSpace(file.AccessToken)
	if accessToken == "" {
		accessToken = strings.TrimSpace(file.Tokens.AccessToken)
	}
	if accessToken == "" {
		accessToken = strings.TrimSpace(file.Token.AccessToken)
	}
	if accessToken == "" {
		return codexQuotaCredential{}, fmt.Errorf("CPA Codex credential has no access token")
	}
	idToken := file.idToken()
	accountID := strings.TrimSpace(file.AccountID)
	if accountID == "" {
		accountID = strings.TrimSpace(file.Tokens.AccountID)
	}
	if accountID == "" {
		accountID = strings.TrimSpace(file.Token.AccountID)
	}
	if accountID == "" {
		accountID = chatGPTAccountID(idToken)
	}
	return codexQuotaCredential{accessToken: accessToken, accountID: accountID, contentDigest: contentDigest}, nil
}

// stableCodexAccountIdentity is deliberately derived only from CPA's declared
// account_id (or the equivalent ID-token claim). Access tokens rotate and must
// never become a persistent account identity or enter the plugin database.
func stableCodexAccountIdentity(path string) (string, error) {
	credential, err := readCodexQuotaCredential(path)
	if err != nil {
		return "", err
	}
	identity := strings.TrimSpace(credential.accountID)
	if identity == "" {
		return "", errMissingCodexAccountIdentity
	}
	return identity, nil
}

// sourceIdentity returns a cached stable identity only when the resolved file
// still has the same physical identity and content digest. Metadata avoids
// reparsing ordinary files, while hashing the small CPA JSON file closes the
// same-size/same-mtime rewrite case even on filesystems with coarse ctime.
// Any read or JSON error stays uncached so CPA's next atomic credential rewrite
// can be verified rather than masked by stale safety state.
func (syncer *quotaSynchronizer) sourceIdentity(path string, info fs.FileInfo) (string, bool, error) {
	if syncer == nil || info == nil {
		return "", false, fmt.Errorf("read CPA auth file identity: missing file metadata")
	}
	physicalID := codexAuthFileIdentity(info)
	modifiedAt := info.ModTime().UTC()
	changedAt := codexAuthFileChangeTime(info)
	contentDigest, err := codexAuthFileContentDigest(path, info)
	if err != nil {
		return "", false, err
	}
	syncer.mu.Lock()
	cached, exists := syncer.sourceIdentities[path]
	syncer.mu.Unlock()
	if exists && cached.physicalID == physicalID && cached.size == info.Size() && cached.modifiedAt.Equal(modifiedAt) && cached.changedAt.Equal(changedAt) && cached.contentDigest == contentDigest {
		return cached.identity, cached.missing, nil
	}
	reader := syncer.readSourceIdentity
	if reader == nil {
		reader = stableCodexAccountIdentity
	}
	identity, err := reader(path)
	if errors.Is(err, errMissingCodexAccountIdentity) {
		identity, err = "", nil
	}
	if err != nil {
		return "", false, err
	}
	syncer.mu.Lock()
	syncer.sourceIdentities[path] = cachedSourceIdentity{
		physicalID: physicalID,
		modifiedAt: modifiedAt,
		changedAt:  changedAt,
		size:       info.Size(),
		identity:   identity,
		missing:    identity == "",
		contentDigest: contentDigest,
	}
	syncer.mu.Unlock()
	return identity, identity == "", nil
}

// codexAuthFileContentDigest hashes at most one configured CPA credential file.
// The digest is process-memory-only and rejects a content rewrite even when an
// operator restores the original size and mtime or ctime is coarse.
func codexAuthFileContentDigest(path string, info fs.FileInfo) (string, error) {
	if info == nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxCodexAuthFileBytes {
		return "", fmt.Errorf("read CPA auth file identity: unsupported file metadata")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read CPA auth file identity: %w", err)
	}
	if int64(len(raw)) != info.Size() {
		return "", fmt.Errorf("%w: CPA auth file changed while reading", errTransientCodexAuthFile)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func readCodexAuthFile(path string) (codexAuthFile, error) {
	file, _, err := readCodexAuthFileWithDigest(path)
	return file, err
}

func readCodexAuthFileWithDigest(path string) (codexAuthFile, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return codexAuthFile{}, "", fmt.Errorf("read CPA auth file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCodexAuthFileBytes {
		return codexAuthFile{}, "", fmt.Errorf("CPA auth file is not a supported regular JSON file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return codexAuthFile{}, "", fmt.Errorf("read CPA auth file: %w", err)
	}
	if int64(len(raw)) != info.Size() {
		return codexAuthFile{}, "", fmt.Errorf("%w: CPA auth file changed while reading", errTransientCodexAuthFile)
	}
	var file codexAuthFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return codexAuthFile{}, "", fmt.Errorf("%w: decode CPA auth file: %v", errTransientCodexAuthFile, err)
	}
	sum := sha256.Sum256(raw)
	return file, hex.EncodeToString(sum[:]), nil
}

// validateDistinctCodexAccountSource rejects two logical CPA auth IDs that
// resolve to one file (including hard links) or stable Codex identity. One
// identity-less file is allowed only as the sole enabled pool entry; otherwise
// separate ledger rows could count the same official allowance twice.
func validateDistinctCodexAccountSource(engine *quota.Engine, candidate quota.AccountPoolEntry) error {
	return validateDistinctCodexAccountSources(engine, []quota.AccountPoolEntry{candidate})
}

// validateDistinctCodexAccountSources validates the effective account pool
// after a batch is applied. It preserves the single-save protection while also
// catching aliases and duplicate stable identities inside the same selection.
func validateDistinctCodexAccountSources(engine *quota.Engine, candidates []quota.AccountPoolEntry) error {
	if engine == nil {
		return fmt.Errorf("codex-carpool is not initialized")
	}
	if len(candidates) == 0 {
		return fmt.Errorf("at least one CPA Codex account is required")
	}
	validationAuthIDs := make(map[string]struct{})
	for _, existing := range engine.AccountPool(time.Now().UTC()) {
		if existing.Enabled {
			validationAuthIDs[strings.TrimSpace(existing.AuthID)] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		authID := strings.TrimSpace(candidate.AuthID)
		if authID == "" {
			return fmt.Errorf("CPA Codex auth_id is required")
		}
		// Retain the old single-save behaviour: a selected source must be a real
		// local Codex JSON even when the row is initially disabled.
		if _, _, err := resolveCodexAuthFile(engine.AuthDirectory(), authID); err != nil {
			return fmt.Errorf("resolve configured CPA Codex account: %w", err)
		}
		validationAuthIDs[authID] = struct{}{}
	}
	seenCandidates := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		authID := strings.TrimSpace(candidate.AuthID)
		if _, exists := seenCandidates[authID]; exists {
			return fmt.Errorf("account pool repeats auth_id %q", authID)
		}
		seenCandidates[authID] = struct{}{}
	}
	type sourceIdentity struct {
		authID   string
		path     string
		info     fs.FileInfo
		identity string
	}
	authIDs := make([]string, 0, len(validationAuthIDs))
	for authID := range validationAuthIDs {
		authIDs = append(authIDs, authID)
	}
	sort.Strings(authIDs)
	sources := make([]sourceIdentity, 0, len(authIDs))
	for _, authID := range authIDs {
		source, info, err := resolveCodexAuthFile(engine.AuthDirectory(), authID)
		if err != nil {
			return fmt.Errorf("read configured CPA Codex account %q: %w", authID, err)
		}
		identity, err := stableCodexAccountIdentity(source)
		if err != nil && !errors.Is(err, errMissingCodexAccountIdentity) {
			return fmt.Errorf("read configured CPA Codex account %q identity: %w", authID, err)
		}
		identity = strings.TrimSpace(identity)
		for _, existing := range sources {
			if source == existing.path || os.SameFile(info, existing.info) {
				return fmt.Errorf("CPA Codex auth file is already configured as %q", existing.authID)
			}
			if identity == "" || existing.identity == "" {
				return fmt.Errorf("CPA Codex account %q cannot share a pool with an authentication file whose stable account identity is unavailable", existing.authID)
			}
			if identity == existing.identity {
				return fmt.Errorf("CPA Codex account is already configured as %q", existing.authID)
			}
		}
		sources = append(sources, sourceIdentity{authID: authID, path: source, info: info, identity: identity})
	}
	return nil
}

func resolveCodexAuthDirectory(directory string) (string, error) {
	directory = strings.TrimSpace(directory)
	if directory == "" {
		return "", fmt.Errorf("CPA auth directory is required")
	}
	if directory == "~" || strings.HasPrefix(directory, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve CPA auth directory: %w", err)
		}
		if directory == "~" {
			directory = home
		} else {
			directory = filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(directory, "~/")))
		}
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve CPA auth directory: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("read CPA auth directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("CPA auth directory is not a directory")
	}
	return filepath.EvalSymlinks(root)
}

func resolveCodexAuthFile(directory, authID string) (string, fs.FileInfo, error) {
	root, err := resolveCodexAuthDirectory(directory)
	if err != nil {
		return "", nil, err
	}
	return resolveCodexAuthFileFromRoot(root, authID)
}

// resolveCodexAuthFileFromRoot avoids resolving auth-dir once per account
// during the 15-second reconciliation scan. root must already be canonicalized
// by resolveCodexAuthDirectory.
func resolveCodexAuthFileFromRoot(root, authID string) (string, fs.FileInfo, error) {
	identifier := filepath.Clean(filepath.FromSlash(strings.TrimSpace(authID)))
	if identifier == "." || filepath.IsAbs(identifier) || identifier == ".." || strings.HasPrefix(identifier, ".."+string(filepath.Separator)) || !strings.EqualFold(filepath.Ext(identifier), ".json") {
		return "", nil, fmt.Errorf("invalid CPA Codex auth_id")
	}
	candidate := filepath.Join(root, identifier)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", nil, fmt.Errorf("resolve CPA auth file: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", nil, fmt.Errorf("CPA Codex auth file escapes auth directory")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", nil, fmt.Errorf("read CPA auth file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxCodexAuthFileBytes {
		return "", nil, fmt.Errorf("CPA auth file is not a supported regular JSON file")
	}
	return resolved, info, nil
}

// codexAuthFileIdentity makes hard-link detection O(1) on the Linux-only
// native plugin build. A resolved path catches symlinks; device/inode catches
// distinct paths pointing to the same physical JSON file.
func codexAuthFileIdentity(info fs.FileInfo) string {
	if info == nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
}

// codexAuthFileChangeTime reads Linux ctime from the same stat record used for
// hard-link detection. CPA normally replaces credentials atomically; ctime is
// the low-cost fallback for a manual in-place rewrite that preserves mtime.
func codexAuthFileChangeTime(info fs.FileInfo) time.Time {
	if info == nil {
		return time.Time{}
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return time.Time{}
	}
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).UTC()
}

type chatGPTAuthClaims struct {
	AccountID string `json:"chatgpt_account_id"`
	PlanType  string `json:"chatgpt_plan_type"`
	CodexAuth struct {
		AccountID string `json:"chatgpt_account_id"`
		PlanType  string `json:"chatgpt_plan_type"`
	} `json:"https://api.openai.com/auth"`
}

func decodeChatGPTAuthClaims(idToken string) (chatGPTAuthClaims, bool) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) < 2 {
		return chatGPTAuthClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return chatGPTAuthClaims{}, false
		}
	}
	var claims chatGPTAuthClaims
	if json.Unmarshal(payload, &claims) != nil {
		return chatGPTAuthClaims{}, false
	}
	return claims, true
}

func chatGPTAccountID(idToken string) string {
	claims, ok := decodeChatGPTAuthClaims(idToken)
	if !ok {
		return ""
	}
	if accountID := strings.TrimSpace(claims.CodexAuth.AccountID); accountID != "" {
		return accountID
	}
	return strings.TrimSpace(claims.AccountID)
}

func chatGPTPlanType(idToken string) string {
	claims, ok := decodeChatGPTAuthClaims(idToken)
	if !ok {
		return ""
	}
	if planType := strings.TrimSpace(claims.CodexAuth.PlanType); planType != "" {
		return planType
	}
	return strings.TrimSpace(claims.PlanType)
}

// isOfficialQuotaExhaustion intentionally matches CPA's Codex usage-limit
// signal only. A generic upstream 429 can be a temporary connection or rate
// limit and must not freeze the whole shared account pool.
func isOfficialQuotaExhaustion(body string) bool {
	return containsUsageLimitType(json.RawMessage(body), 0)
}

func containsUsageLimitType(raw json.RawMessage, depth int) bool {
	if len(raw) == 0 || depth > 2 {
		return false
	}
	var payload struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Body     json.RawMessage `json:"body"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(payload.Type), "usage_limit_reached") {
		return true
	}
	return containsUsageLimitType(payload.Error, depth+1) ||
		containsUsageLimitType(payload.Body, depth+1) ||
		containsUsageLimitType(payload.Response, depth+1)
}

// fetchOfficialQuota deliberately bypasses CPA's HTTP bridge. The plugin reads
// its active credential from CPA's local auth-dir, then sends this small
// read-only request itself. That keeps official quota polling out of CPA's
// proxy scheduler, generic request monitor, and downstream usage accounting.
func fetchOfficialQuota(ctx context.Context, accessToken, accountID string) (quota.OfficialQuotaSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, quotaSyncTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("create official Codex quota request: %w", err)
	}
	headers := http.Header{
		"Authorization": {"Bearer " + accessToken},
		"Content-Type":  {"application/json"},
		"User-Agent":    {"codex-carpool/1.0"},
	}
	if accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	request.Header = headers
	response, err := (&http.Client{Timeout: quotaSyncTimeout}).Do(request)
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("request official Codex quota: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxQuotaBodyBytes+1))
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("read official Codex quota response: %w", err)
	}
	if len(body) > maxQuotaBodyBytes {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("official Codex quota response exceeded %d bytes", maxQuotaBodyBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return quota.OfficialQuotaSnapshot{}, officialQuotaHTTPError{statusCode: response.StatusCode}
	}
	snapshot, err := parseOfficialQuota(body)
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, err
	}
	return snapshot, nil
}

type officialQuotaHTTPError struct {
	statusCode int
}

func (err officialQuotaHTTPError) Error() string {
	return fmt.Sprintf("official Codex quota request returned HTTP %d", err.statusCode)
}

func isCredentialAuthorizationFailure(cause error) bool {
	var httpError officialQuotaHTTPError
	return errors.As(cause, &httpError) && (httpError.statusCode == http.StatusUnauthorized || httpError.statusCode == http.StatusForbidden)
}

func parseOfficialQuota(raw []byte) (quota.OfficialQuotaSnapshot, error) {
	var payload struct {
	PlanType string `json:"plan_type"`
	RateLimit struct {
		Allowed      *bool           `json:"allowed"`
		LimitReached *bool           `json:"limit_reached"`
		Primary      json.RawMessage `json:"primary_window"`
		Secondary    json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("decode official Codex quota: %w", err)
	}
	primary, err := parseOfficialWindow(payload.RateLimit.Primary)
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, err
	}
	secondary, err := parseOfficialWindow(payload.RateLimit.Secondary)
	if err != nil {
		return quota.OfficialQuotaSnapshot{}, err
	}
	weekly := primary
	if weekly.LimitWindowSeconds == 0 {
		weekly = secondary
	}
	if weekly.LimitWindowSeconds == 0 {
		return quota.OfficialQuotaSnapshot{}, fmt.Errorf("official Codex quota response has no rate-limit windows")
	}
	// The current Codex/CPA quota panel treats primary_window as the account's
	// normal weekly allowance even when wham/usage still reports its retired
	// 18000-second duration. secondary_window is model-specific when present,
	// so it must not become a second global shared-pool ceiling. If the old
	// duration produced a local reset fallback, discard it and let the weekly
	// normalizer create a conservative weekly identity instead.
	if weekly.ResetEstimated && weekly.LimitWindowSeconds != 7*24*60*60 {
		weekly.ResetAt = nil
		weekly.ResetEstimated = false
	}
	weekly.LimitWindowSeconds = 7 * 24 * 60 * 60
	// Older or differently entitled Codex responses can omit "allowed". A
	// complete window response without an explicit limit flag is usable; an
	// omitted optional field must not turn every account into a false 429.
	limitReached := payload.RateLimit.LimitReached != nil && *payload.RateLimit.LimitReached
	allowed := !limitReached
	if payload.RateLimit.Allowed != nil {
		allowed = *payload.RateLimit.Allowed
	}
	if limitReached {
		allowed = false
	}
	return quota.OfficialQuotaSnapshot{PlanType: strings.TrimSpace(payload.PlanType), Allowed: allowed, LimitReached: limitReached, Secondary: weekly}, nil
}

func parseOfficialWindow(raw json.RawMessage) (quota.OfficialQuotaWindow, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return quota.OfficialQuotaWindow{}, nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return quota.OfficialQuotaWindow{}, fmt.Errorf("decode official Codex quota window: %w", err)
	}
	used, err := rawFloat(value["used_percent"])
	if err != nil {
		return quota.OfficialQuotaWindow{}, err
	}
	seconds, err := rawInt(value["limit_window_seconds"])
	if err != nil {
		return quota.OfficialQuotaWindow{}, err
	}
	resetAt, err := rawUnixTime(value["reset_at"])
	if err != nil {
		return quota.OfficialQuotaWindow{}, err
	}
	resetEstimated := false
	if resetAt == nil {
		remaining, err := rawInt(value["reset_after_seconds"])
		if err != nil {
			return quota.OfficialQuotaWindow{}, err
		}
		if remaining > 0 {
			value := time.Now().UTC().Add(time.Duration(remaining) * time.Second)
			resetAt = &value
			resetEstimated = true
		}
	}
	return quota.OfficialQuotaWindow{UsedPercent: used, LimitWindowSeconds: seconds, ResetAt: resetAt, ResetEstimated: resetEstimated}, nil
}

func rawFloat(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.Float64()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("invalid official quota number")
	}
	return strconv.ParseFloat(strings.TrimSpace(text), 64)
}

func rawInt(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.Int64()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("invalid official quota integer")
	}
	return strconv.ParseInt(strings.TrimSpace(text), 10, 64)
}

func rawUnixTime(raw json.RawMessage) (*time.Time, error) {
	value, err := rawInt(raw)
	if err != nil || value <= 0 {
		return nil, err
	}
	if value > 1_000_000_000_000 {
		instant := time.UnixMilli(value).UTC()
		return &instant, nil
	}
	instant := time.Unix(value, 0).UTC()
	return &instant, nil
}

// sortAccountIDs gives deterministic manual refresh responses and makes tests
// independent from map iteration order.
func sortAccountIDs(ids []string) []string {
	copy := append([]string(nil), ids...)
	sort.Strings(copy)
	return copy
}

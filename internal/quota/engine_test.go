package quota

import (
	"math"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestStandaloneRuntimeConfigUsesCarpoolDataPath(t *testing.T) {
	cfg, err := StandaloneRuntimeConfig()
	if err != nil {
		t.Fatalf("StandaloneRuntimeConfig() error = %v", err)
	}
	if cfg.DatabasePath != "/CLIProxyAPI/plugins/codex-carpool/data/codex-carpool.db" {
		t.Fatalf("database path = %q", cfg.DatabasePath)
	}
}

func TestCompletedUsageUsesActualTokensAndSafeFallback(t *testing.T) {
	if got := completedUsageUnits(CompletedUsage{Generate: true, TotalTokens: 1234}, 200000); got != 1234 {
		t.Fatalf("total token units = %d, want 1234", got)
	}
	if got := completedUsageUnits(CompletedUsage{Generate: true, InputTokens: 100, OutputTokens: 50, ReasoningTokens: 25}, 200000); got != 175 {
		t.Fatalf("breakdown token units = %d, want 175", got)
	}
	if got := completedUsageUnits(CompletedUsage{Generate: true}, 200000); got != 200000 {
		t.Fatalf("missing usage fallback = %d, want reservation", got)
	}
	if got := completedUsageUnits(CompletedUsage{Generate: true, Failed: true}, 200000); got != 0 {
		t.Fatalf("failed request units = %d, want 0", got)
	}
	if got := completedUsageUnits(CompletedUsage{Generate: true, Failed: true, TotalTokens: 1234}, 200000); got != 1234 {
		t.Fatalf("failed request with actual tokens = %d, want 1234", got)
	}
}

func TestReservationReleaseDoesNotLeaveAStaleCharge(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	state := newKeyMeterState(nil, now)
	state.reservations.addEvent(now, 200)
	if !state.reservations.removeEvent(now, 200) {
		t.Fatal("reservation was not released")
	}
	if state.reservations.weeklyUnits != 0 || len(state.reservations.events) != 0 {
		t.Fatalf("reservation state = %+v, want empty", state.reservations)
	}
}

func TestRollingWindowReportsEarliestIndividualRelease(t *testing.T) {
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	first := now.Add(-4 * time.Hour)
	second := now.Add(-2 * time.Hour)
	state := newKeyMeterState([]meterEvent{{At: first, Units: 30}, {At: second, Units: 20}}, now)

	snapshot := (&Engine{}).windowSnapshot(state, now, 100, 1, fiveHourWindow, true)
	if snapshot.ResetAt == nil || !snapshot.ResetAt.Equal(first.Add(fiveHourWindow)) {
		t.Fatalf("earliest release = %v, want %v", snapshot.ResetAt, first.Add(fiveHourWindow))
	}

	// When the oldest usage expires, the later usage remains in the rolling window.
	snapshot = (&Engine{}).windowSnapshot(state, now.Add(2*time.Hour), 100, 1, fiveHourWindow, true)
	if snapshot.Used != 20 || snapshot.ResetAt == nil || !snapshot.ResetAt.Equal(second.Add(fiveHourWindow)) {
		t.Fatalf("post-expiry snapshot = %+v, want only the later usage remaining", snapshot)
	}
}

func TestDecisionLogQueueReservationIsBounded(t *testing.T) {
	engine := &Engine{}
	if got := engine.reserveDecisionLogSlots(maxPendingDecisionLogs + 1); got != maxPendingDecisionLogs {
		t.Fatalf("initial reserved slots = %d, want %d", got, maxPendingDecisionLogs)
	}
	if got := engine.reserveDecisionLogSlots(1); got != 0 {
		t.Fatalf("overflow reserved slots = %d, want 0", got)
	}
}

func TestPendingSettlementAuthIDsAreDistinctAndSorted(t *testing.T) {
	engine := &Engine{pendingSettlementsByAuth: map[string]int64{
		"account-b":    2,
		"account-a":    1,
		"account-zero": 0,
	}}
	ids := engine.PendingSettlementAuthIDs()
	if len(ids) != 2 || ids[0] != "account-a" || ids[1] != "account-b" {
		t.Fatalf("PendingSettlementAuthIDs() = %v", ids)
	}
	if !engine.HasPendingSettlementForAuth("account-a") || engine.HasPendingSettlementForAuth("account-zero") || engine.HasPendingSettlementForAuth("missing") {
		t.Fatal("HasPendingSettlementForAuth() did not reflect the pending-account map")
	}
}

func TestLegacyFingerprintCannotBeEnabledWithoutRebind(t *testing.T) {
	policy := KeyPolicy{
		ID:                 "legacy",
		Name:               "Legacy",
		KeySHA256:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		FingerprintScheme:  "legacy-sha256-v1",
		FiveHourMultiplier: 3,
		SevenDayMultiplier: 10,
		Enabled:            true,
	}
	if _, err := normalizePolicy(policy, nil, 1); err == nil {
		t.Fatal("enabled legacy fingerprint should require a rebind")
	}
	policy.Enabled = false
	normalized, err := normalizePolicy(policy, nil, 1)
	if err != nil || !normalized.NeedsRebind {
		t.Fatalf("paused legacy policy = %+v, err=%v", normalized, err)
	}
}

func TestUnmanagedAndPausedKeysBypass(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", FiveHourMultiplier: 1, SevenDayMultiplier: 1, Enabled: true})
	defer engine.Close()
	now := time.Now().UTC()
	if result := engine.Admit("unknown", "gpt-5", now); !result.Bypass {
		t.Fatalf("unknown Key = %+v, want bypass", result)
	}
	policy := engine.Policies()[0]
	policy.Enabled = false
	if _, err := engine.UpsertPolicy(policy, ""); err != nil {
		t.Fatalf("pause policy: %v", err)
	}
	if result := engine.Admit("managed-key", "gpt-5", now); !result.Bypass {
		t.Fatalf("paused Key = %+v, want bypass", result)
	}
}

func TestManagedKeyWithoutSchedulerCandidatesFailsClosed(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer engine.Close()
	result := engine.Admit("managed-key", "gpt-5", time.Now().UTC())
	if result.Allowed || result.Code != "quota_scheduler_candidates_required" {
		t.Fatalf("admission without CPA candidates = %+v, want fail-closed decision", result)
	}
}

func TestManagedKeyWithUnmatchedSchedulerCandidateReportsMismatch(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	_ = readyNativeCandidate(t, engine, now)

	result := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "different-cpa-auth-id"}})
	if result.Allowed || result.Code != "quota_candidate_mismatch" {
		t.Fatalf("unmatched CPA candidate admission = %+v, want explicit mismatch", result)
	}
}

func TestSchedulerAuthIndexAliasUsesOnePoolLedgerIdentity(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "accounts/account-a.json", AuthIndex: "cpa-auth-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "cpa-auth-a", AuthIndex: "cpa-auth-a", Name: "Disabled", CapacityX: 1, Enabled: false}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(disabled duplicate) error = %v", err)
	}
	resolved := engine.ResolveSchedulerCandidates([]SchedulerCandidate{{AuthID: "cpa-auth-a", Priority: 2}})
	if len(resolved) != 1 || resolved[0].AuthID != "accounts/account-a.json" || resolved[0].CPAAuthID != "cpa-auth-a" {
		t.Fatalf("resolved scheduler candidate = %+v", resolved)
	}
	if got := CPAAuthIDForPoolAuthID(resolved, "accounts/account-a.json"); got != "cpa-auth-a" {
		t.Fatalf("CPAAuthIDForPoolAuthID() = %q", got)
	}
	if got := engine.ResolvePoolAuthID("cpa-auth-a"); got != "accounts/account-a.json" {
		t.Fatalf("ResolvePoolAuthID() = %q", got)
	}
}

func TestAccountPoolBatchValidationIsAtomic(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	entries := []AccountPoolEntry{
		{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true},
		{AuthID: "account-b", Name: "B", CapacityX: 0, Enabled: true},
	}
	if _, err := engine.UpsertAccountPoolEntries(entries); err == nil {
		t.Fatal("UpsertAccountPoolEntries() accepted an invalid batch")
	}
	if pool := engine.AccountPool(time.Now().UTC()); len(pool) != 0 {
		t.Fatalf("invalid batch partially persisted account pool = %+v", pool)
	}

	entries[1].CapacityX = 1
	entries[0].AuthIndex = "same-cpa-auth-id"
	entries[1].AuthIndex = "same-cpa-auth-id"
	if _, err := engine.UpsertAccountPoolEntries(entries); err == nil {
		t.Fatal("UpsertAccountPoolEntries() accepted duplicate CPA scheduler aliases")
	}
	if pool := engine.AccountPool(time.Now().UTC()); len(pool) != 0 {
		t.Fatalf("duplicate alias batch partially persisted account pool = %+v", pool)
	}
	entries[1].AuthIndex = "different-cpa-auth-id"
	stored, err := engine.UpsertAccountPoolEntries(entries)
	if err != nil || len(stored) != 2 {
		t.Fatalf("UpsertAccountPoolEntries(valid) = %+v, err=%v", stored, err)
	}
	if pool := engine.AccountPool(time.Now().UTC()); len(pool) != 2 {
		t.Fatalf("valid batch pool = %+v, want two accounts", pool)
	}
}

func TestAllocationSettlementRetryCommitsBeforeRecovery(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer engine.Close()
	now := time.Now().UTC()
	key := allocationBucketKey{KeyID: "managed", AuthID: "account-a", WindowResetAt: now.Add(sevenDayWindow).UnixMilli(), BucketAt: usageBucketEnd(now).UnixMilli()}
	if err := engine.store.applyAllocationMutations([]allocationMutation{{Key: key, ReservedDelta: 1}}); err != nil {
		t.Fatalf("seed reservation: %v", err)
	}
	engine.allocationDegraded.Store(true)
	engine.allocationRetryMu.Lock()
	engine.allocationRetry = []allocationMutation{{Key: key, CompletedDelta: 3, ReservedDelta: -1}}
	engine.allocationRetryMu.Unlock()
	engine.retryAllocationMutations()
	if engine.allocationDegraded.Load() {
		t.Fatal("successful settlement retry must reopen allocation persistence")
	}
	records, err := engine.store.LoadAllocationBuckets(now)
	if err != nil {
		t.Fatalf("LoadAllocationBuckets(): %v", err)
	}
	if len(records) != 1 || records[0].CompletedUnits != 3 || records[0].ReservedUnits != 0 {
		t.Fatalf("retried allocation buckets = %+v, want settled record", records)
	}
}

func TestAllocationShutdownDeadlineDoesNotWaitForSQLiteBusyTimeout(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	engine.store.mu.Lock()
	deadline := time.Now().Add(75 * time.Millisecond)
	started := time.Now()
	err := engine.waitForAllocationPersistenceUntil(deadline)
	elapsed := time.Since(started)
	engine.store.mu.Unlock()
	if err == nil {
		t.Fatal("shutdown allocation barrier unexpectedly succeeded while SQLite was locked")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown barrier waited %s, want deadline-bounded return", elapsed)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() after SQLite unlock = %v", err)
	}
}

func TestCompletionTokenUsageReplacesAdmissionReservationWithProvisionalX(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer engine.Close()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	wantProvisional := engine.provisionalXUnits("account-a", 3, engine.config.RequestUnits)
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	snapshot := engine.Summary(now).Keys[0].Allocation
	if snapshot.Completed != 0 || snapshot.Provisional != wantProvisional || snapshot.Reserved != 0 || snapshot.Used != wantProvisional {
		t.Fatalf("completion snapshot = %+v, want %d provisional x units and no reservation", snapshot, wantProvisional)
	}
	logs, err := engine.DecisionLogs("managed", 20)
	if err != nil {
		t.Fatalf("DecisionLogs() error = %v", err)
	}
	for _, entry := range logs {
		if entry.Decision == "completed" && entry.Units == 3 && entry.AuthID == "account-a" && entry.Model == "gpt-5" {
			return
		}
	}
	t.Fatalf("logs = %+v, want completed usage audit record", logs)
}

func TestConfigureInstallationAllowsRequestUnitsChangeAfterPendingSettlementCompletes(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	settings := InstallationSettings{RequestUnits: 2, RecordRetention: "168h"}
	if _, err := engine.ConfigureInstallation(settings); err == nil {
		t.Fatal("ConfigureInstallation() unexpectedly changed request_units while a settlement was pending")
	}

	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	if _, err := engine.ConfigureInstallation(settings); err != nil {
		t.Fatalf("ConfigureInstallation() after settlement = %v, want x ledger independence", err)
	}
	if got := engine.Installation().Settings.RequestUnits; got != 2 {
		t.Fatalf("request_units = %d, want updated fallback 2", got)
	}
}

func TestConfigureInstallationPreservesCustomAuthDirectoryForLegacyRequest(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	settings := engine.Installation().Settings
	settings.AuthDirectory = "/srv/cliproxy/custom-auth"
	if _, err := engine.ConfigureInstallation(settings); err != nil {
		t.Fatalf("ConfigureInstallation() with custom auth directory: %v", err)
	}
	if _, err := engine.ConfigureInstallation(InstallationSettings{
		RequestUnits:    settings.RequestUnits,
		RecordRetention: settings.RecordRetention,
	}); err != nil {
		t.Fatalf("ConfigureInstallation() legacy request: %v", err)
	}
	if got := engine.Installation().Settings.AuthDirectory; got != settings.AuthDirectory {
		t.Fatalf("auth directory = %q, want %q", got, settings.AuthDirectory)
	}
}

func TestConfigureInstallationAuthDirectoryClosesSourceGuardAndBumpsRevision(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	before := engine.AccountSourceRevision()
	settings := engine.Installation().Settings
	settings.AuthDirectory = "/srv/cliproxy/reconciled-auth"
	if _, err := engine.ConfigureInstallation(settings); err != nil {
		t.Fatalf("ConfigureInstallation() auth-dir change: %v", err)
	}
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("auth-dir change did not close the account-source guard before reconciliation")
	}
	if got := engine.AccountSourceRevision(); got != before+1 {
		t.Fatalf("AccountSourceRevision() = %d, want %d", got, before+1)
	}

	if !engine.PublishAccountSourceScan(engine.AccountSourceRevision(), false, true) {
		t.Fatal("complete scan did not clear the initial auth-dir verification guard")
	}
	if _, err := engine.ConfigureInstallation(InstallationSettings{
		RequestUnits:    settings.RequestUnits,
		RecordRetention: settings.RecordRetention,
	}); err != nil {
		t.Fatalf("ConfigureInstallation() legacy auth-dir-preserving request: %v", err)
	}
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("unchanged auth-dir unexpectedly closed the account-source guard")
	}
	if got := engine.AccountSourceRevision(); got != before+1 {
		t.Fatalf("unchanged auth-dir bumped AccountSourceRevision() to %d", got)
	}
}

func TestAccountSourceScanPublicationIsAtomicAndRequiresCompleteAuthDirRecheck(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	staleRevision := engine.AccountSourceRevision()
	settings := engine.Installation().Settings
	settings.AuthDirectory = "/srv/cliproxy/next-auth-dir"
	if _, err := engine.ConfigureInstallation(settings); err != nil {
		t.Fatalf("ConfigureInstallation() auth-dir change: %v", err)
	}
	currentRevision := engine.AccountSourceRevision()
	if currentRevision == staleRevision {
		t.Fatal("auth-dir change did not create a new source generation")
	}
	if engine.PublishAccountSourceScan(staleRevision, false, true) {
		t.Fatal("stale source scan was allowed to publish")
	}
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("stale source scan reopened the fail-closed guard")
	}
	engine.SetAccountSourceConflict(false)
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("legacy source-conflict setter bypassed the pending auth-dir verification")
	}
	if !engine.PublishAccountSourceScan(currentRevision, false, false) {
		t.Fatal("current incomplete source scan was unexpectedly rejected")
	}
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("incomplete auth-dir scan reopened the fail-closed guard")
	}
	if !engine.PublishAccountSourceScan(currentRevision, false, true) {
		t.Fatal("complete current source scan was unexpectedly rejected")
	}
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("complete conflict-free source scan did not reopen the guard")
	}
}

func TestRequiredAccountSourceVerificationClosesForIncompleteScanAndPoolMutation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	engine.RequireAccountSourceVerification()
	revision := engine.AccountSourceRevision()
	if !engine.PublishAccountSourceScan(revision, false, false) {
		t.Fatal("incomplete startup source scan was unexpectedly rejected")
	}
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("incomplete startup source scan reopened managed admissions")
	}
	if !engine.PublishAccountSourceScan(revision, false, true) {
		t.Fatal("complete startup source scan was unexpectedly rejected")
	}
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("complete startup source scan did not reopen managed admissions")
	}

	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("account-pool mutation did not require a new complete source scan")
	}
}

func TestCloseDrainsAdmittedUsageBeforeReleasingSQLite(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- engine.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for !engine.admissionsClosed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("Close() did not stop new admissions")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); result.Allowed || result.Code != "quota_unavailable" {
		t.Fatalf("admission during close = %+v, want shutdown denial", result)
	}

	// This represents CPA publishing a terminal usage record after plugin
	// shutdown has begun. It must replace the reservation before Close releases
	// the database lock.
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not finish after the final usage record")
	}

	reopened, err := Open(engine.config)
	if err != nil {
		t.Fatalf("Open(reopened) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	keys := reopened.Summary(now).Keys
	wantProvisional := reopened.provisionalXUnits("account-a", 3, reopened.config.RequestUnits)
	if len(keys) != 1 || keys[0].Allocation.Completed != 0 || keys[0].Allocation.Provisional != wantProvisional || keys[0].Allocation.Reserved != 0 {
		t.Fatalf("reopened allocation = %+v, want durable provisional charge %d", keys, wantProvisional)
	}
}

func TestClosePreservesOnlyAdmissionMarker(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	config := engine.config
	if err := engine.CloseConservatively(); err != nil {
		t.Fatalf("CloseConservatively() = %v", err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(reopened) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	keys := reopened.Summary(now).Keys
	wantReserved := admissionReservationUnits
	if len(keys) != 1 || keys[0].Allocation.Reserved != wantReserved || keys[0].Allocation.Completed != 0 {
		t.Fatalf("reopened allocation = %+v, want admission marker %d", keys, wantReserved)
	}
}

func TestMarkerNormalizationDoesNotExpandCurrentReservation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.CloseConservatively() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	if err := engine.conservativelyChargeUnsettledAllocations(); err != nil {
		t.Fatalf("conservativelyChargeUnsettledAllocations() = %v", err)
	}
	keys := engine.Summary(now).Keys
	wantReserved := admissionReservationUnits
	if len(keys) != 1 || keys[0].Allocation.Reserved != wantReserved {
		t.Fatalf("normalized allocation = %+v, want reserved=%d", keys, wantReserved)
	}
}

func TestConservativeRecoveryUsesOriginalCycleCapacityAfterConfigurationEdit(t *testing.T) {
	policy := KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 20, Enabled: true}
	engine := newTestEngine(t, policy)
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("initial admission = %+v, want allowed", result)
	}

	// A lower policy value is saved immediately, but the current official window
	// must retain its frozen 20x ledger so completed work cannot be reinterpreted.
	updated := engine.Policies()[0]
	updated.AllocationX = 1
	if _, err := engine.UpsertPolicy(updated, ""); err != nil {
		t.Fatalf("UpsertPolicy(reduced allocation) = %v", err)
	}
	if snapshot := engine.Summary(now).Keys[0].Allocation; snapshot.Capacity != 20*officialXUnitsPerX {
		t.Fatalf("active allocation after decrease = %+v, want preserved 20x capacity", snapshot)
	}
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err == nil {
		t.Fatal("UpsertAccountPoolEntry(reduced capacity) succeeded during an active official ledger")
	}
	config := engine.config
	if err := engine.CloseConservatively(); err != nil {
		t.Fatalf("CloseConservatively() = %v", err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(reopened) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	wantCapacity := int64(20) * officialXUnitsPerX
	keys := reopened.Summary(now).Keys
	if len(keys) != 1 || keys[0].Allocation.Reserved != admissionReservationUnits || keys[0].Allocation.Capacity != wantCapacity {
		t.Fatalf("recovered allocation = %+v, want marker %d and immutable original capacity %d", keys, admissionReservationUnits, wantCapacity)
	}
	records, err := reopened.store.LoadAllocationBuckets(now)
	if err != nil {
		t.Fatalf("LoadAllocationBuckets() = %v", err)
	}
	if len(records) != 1 || records[0].CapacityUnits != wantCapacity {
		t.Fatalf("durable allocation snapshot = %+v, want capacity %d", records, wantCapacity)
	}
}

func TestLegacyFullRequestReservationsNormalizeToMarkers(t *testing.T) {
	engine := newTestEngineWithRequestUnits(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true}, defaultRequestUnits)
	defer func() { _ = engine.CloseConservatively() }()
	now := time.Now().UTC().Truncate(time.Minute)
	key := allocationBucketKey{KeyID: "managed", AuthID: "account-a", WindowResetAt: now.Add(sevenDayWindow).UnixMilli(), BucketAt: usageBucketEnd(now).UnixMilli()}
	legacyReserved := 2 * engine.config.RequestUnits
	if err := engine.store.applyAllocationMutations([]allocationMutation{{Key: key, ReservedDelta: legacyReserved}}); err != nil {
		t.Fatalf("seed legacy reservation: %v", err)
	}
	engine.allocationMu.Lock()
	engine.setAllocationBucketLocked(key, allocationBucketState{Reserved: legacyReserved})
	engine.allocationCycles[allocationCycleKey{KeyID: key.KeyID, AuthID: key.AuthID, WindowResetAt: key.WindowResetAt}] = allocationCycleState{Reserved: legacyReserved}
	engine.allocationMu.Unlock()
	if err := engine.conservativelyChargeUnsettledAllocations(); err != nil {
		t.Fatalf("conservativelyChargeUnsettledAllocations() = %v", err)
	}
	if err := engine.conservativelyChargeUnsettledAllocations(); err != nil {
		t.Fatalf("second conservativelyChargeUnsettledAllocations() = %v", err)
	}
	engine.allocationMu.Lock()
	cycle := engine.allocationCycles[allocationCycleKey{KeyID: key.KeyID, AuthID: key.AuthID, WindowResetAt: key.WindowResetAt}]
	engine.allocationMu.Unlock()
	if cycle.Reserved != 2 {
		t.Fatalf("legacy cycle = %+v, want two admission markers", cycle)
	}
	records, err := engine.store.LoadAllocationBuckets(now)
	if err != nil || len(records) != 1 || records[0].ReservedUnits != 2 {
		t.Fatalf("durable normalized reservation = %+v, %v; want two markers", records, err)
	}
}

func TestDeletePolicyPurgesUsageAndReaddStartsAtZero(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}

	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Second), candidates); !result.Allowed {
		t.Fatalf("second admission = %+v, want allowed", result)
	}
	if err := engine.DeletePolicy("managed"); err != nil {
		t.Fatalf("DeletePolicy() = %v, want explicit reset", err)
	}
	if len(engine.Policies()) != 0 || engine.PendingSettlementCount() != 0 {
		t.Fatalf("deleted policy state = policies:%+v pending:%d", engine.Policies(), engine.PendingSettlementCount())
	}
	if usage, err := engine.UsageRecords("managed", 20); err != nil || len(usage) != 0 {
		t.Fatalf("UsageRecords() after delete = %+v, %v; want empty", usage, err)
	}
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) != 0 {
		t.Fatalf("DecisionLogs() after delete = %+v, %v; want empty", logs, err)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true}, "managed-key"); err != nil {
		t.Fatalf("UpsertPolicy(readd) = %v", err)
	}
	keys := engine.Summary(now).Keys
	if len(keys) != 1 || keys[0].Allocation.Used != 0 || keys[0].Allocation.Completed != 0 ||
		keys[0].Allocation.Provisional != 0 || keys[0].Allocation.Reserved != 0 {
		t.Fatalf("re-added allocation = %+v, want zero history", keys)
	}
}

func TestResetPolicyUsageKeepsPolicyAndStartsAtZero(t *testing.T) {
	policy := KeyPolicy{
		ID:            "managed",
		Name:          "Managed",
		AllocationX:   1,
		Enabled:       true,
		AllowedModels: []string{"gpt-5"},
		AccessRules: []AccessRule{{
			Weekdays: []int{1, 2, 3, 4, 5, 6, 7},
			Start:    "00:00",
			End:      "23:59",
		}},
		AccessTimezone: "Asia/Shanghai",
	}
	engine := newTestEngine(t, policy)
	defer func() { _ = engine.Close() }()
	now := time.Date(2026, time.July, 30, 4, 0, 0, 0, time.UTC)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	engine.LogOperational("warn", "before_reset", "preserve this runtime log", "account-a", "managed")
	if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Second), candidates); !result.Allowed {
		t.Fatalf("second admission = %+v, want allowed", result)
	}

	if err := engine.ResetPolicyUsage("managed"); err != nil {
		t.Fatalf("ResetPolicyUsage() = %v", err)
	}
	policies := engine.Policies()
	if len(policies) != 1 || policies[0].Name != policy.Name || policies[0].AllocationX != policy.AllocationX ||
		len(policies[0].AllowedModels) != 1 || policies[0].AllowedModels[0] != "gpt-5" ||
		len(policies[0].AccessRules) != 1 || policies[0].AccessTimezone != "Asia/Shanghai" {
		t.Fatalf("policy after reset = %+v, want unchanged policy", policies)
	}
	if engine.PendingSettlementCount() != 0 {
		t.Fatalf("pending settlements after reset = %d, want zero", engine.PendingSettlementCount())
	}
	keys := engine.Summary(now).Keys
	if len(keys) != 1 || keys[0].Allocation.Used != 0 || keys[0].Allocation.Completed != 0 ||
		keys[0].Allocation.Provisional != 0 || keys[0].Allocation.Reserved != 0 {
		t.Fatalf("reset allocation = %+v, want zero history", keys)
	}
	if usage, err := engine.UsageRecords("managed", 20); err != nil || len(usage) != 0 {
		t.Fatalf("UsageRecords() after reset = %+v, %v; want empty", usage, err)
	}
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) == 0 || logs[0].Decision != "completed" {
		t.Fatalf("DecisionLogs() after reset = %+v, %v; want retained completed audit row", logs, err)
	}
	operational, err := engine.OperationalLogs("", "before_reset", 20)
	if err != nil || len(operational) != 1 || operational[0].Event != "before_reset" {
		t.Fatalf("OperationalLogs() after reset = %+v, %v; want retained runtime row", operational, err)
	}
	analysis, err := engine.UsageAnalysis("managed", now.Add(-time.Hour), now.Add(time.Hour), time.UTC, "day")
	if err != nil || analysis.TotalTokens != 0 || analysis.RequestCount != 0 {
		t.Fatalf("UsageAnalysis() after reset = %+v, %v; want empty", analysis, err)
	}
	actualSummary, err := engine.SummaryWithActualTokens(now)
	if err != nil || len(actualSummary.Keys) != 1 || actualSummary.Keys[0].ActualTokens.Total != 0 {
		t.Fatalf("SummaryWithActualTokens() after reset = %+v, %v; want zero lifetime total", actualSummary.Keys, err)
	}
	if result := engine.Admit("managed-key", "gpt-5", now.Add(2*time.Second), candidates); !result.Allowed {
		t.Fatalf("post-reset admission = %+v, want preserved policy to remain active", result)
	}
}

func TestClearLogCategoriesDoNotChangeQuotaOrEachOther(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Date(2026, time.July, 30, 5, 0, 0, 0, time.UTC)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	engine.LogOperational("error", "preserved_runtime_log", "runtime failure", "account-a", "managed")
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) == 0 {
		t.Fatalf("DecisionLogs() before clear = %+v, %v; want seeded row", logs, err)
	}
	usageBefore, err := engine.UsageRecords("managed", 20)
	if err != nil || len(usageBefore) == 0 {
		t.Fatalf("UsageRecords() before clear = %+v, %v; want seeded usage", usageBefore, err)
	}

	if err := engine.ClearDecisionLogs("managed"); err != nil {
		t.Fatalf("ClearDecisionLogs() = %v", err)
	}
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) != 0 {
		t.Fatalf("DecisionLogs() after clear = %+v, %v; want empty", logs, err)
	}
	if usage, err := engine.UsageRecords("managed", 20); err != nil || len(usage) != len(usageBefore) {
		t.Fatalf("UsageRecords() after decision clear = %+v, %v; want unchanged usage", usage, err)
	}
	if logs, err := engine.OperationalLogs("", "preserved_runtime_log", 20); err != nil || len(logs) != 1 {
		t.Fatalf("OperationalLogs() after decision clear = %+v, %v; want retained runtime row", logs, err)
	}

	if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Second), candidates); !result.Allowed {
		t.Fatalf("second admission = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now.Add(time.Second), Generate: true, TotalTokens: 4})
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) == 0 {
		t.Fatalf("DecisionLogs() after reseed = %+v, %v; want new row", logs, err)
	}
	if err := engine.ClearOperationalLogs(); err != nil {
		t.Fatalf("ClearOperationalLogs() = %v", err)
	}
	if logs, err := engine.OperationalLogs("", "", 20); err != nil || len(logs) != 0 {
		t.Fatalf("OperationalLogs() after clear = %+v, %v; want empty", logs, err)
	}
	if logs, err := engine.DecisionLogs("managed", 20); err != nil || len(logs) == 0 {
		t.Fatalf("DecisionLogs() after operational clear = %+v, %v; want retained decision row", logs, err)
	}
}

func TestFailedCompletionReleasesReservationAndCreatesFailureLog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer engine.Close()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, Failed: true, FailureStatus: 502})
	snapshot := engine.Summary(now).Keys[0].Allocation
	if snapshot.Completed != 0 || snapshot.Provisional != 0 || snapshot.Reserved != 0 {
		t.Fatalf("failed completion snapshot = %+v, want released reservation", snapshot)
	}
	logs, err := engine.DecisionLogs("managed", 20)
	if err != nil {
		t.Fatalf("DecisionLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Decision != "failed" || logs[0].StatusCode != 502 || logs[0].AuthID != "account-a" {
		t.Fatalf("logs = %+v, want failed completion audit record", logs)
	}
}

func TestFailedCompletionWithActualTokensUsesProvisionalX(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	wantProvisional := engine.provisionalXUnits("account-a", 3, engine.config.RequestUnits)
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, Failed: true, FailureStatus: 502, TotalTokens: 3})
	snapshot := engine.Summary(now).Keys[0].Allocation
	if snapshot.Completed != 0 || snapshot.Provisional != wantProvisional || snapshot.Reserved != 0 {
		t.Fatalf("failed completion snapshot = %+v, want %d provisional x units from actual Tokens", snapshot, wantProvisional)
	}
	logs, err := engine.DecisionLogs("managed", 20)
	if err != nil {
		t.Fatalf("DecisionLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Decision != "failed" || logs[0].Units != 3 || logs[0].Reason != "upstream_failed_with_actual_usage" {
		t.Fatalf("logs = %+v, want failed actual-token settlement", logs)
	}
}

func TestModelAllowListBlocksOnlyManagedKey(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 3, AllowedModels: []string{"gpt-5"}, Enabled: true})
	defer engine.Close()
	now := time.Now().UTC()
	blocked := engine.Admit("managed-key", "gpt-5-mini", now)
	if blocked.Allowed || blocked.Code != "model_not_allowed" {
		t.Fatalf("model block = %+v", blocked)
	}
	candidates := readyNativeCandidate(t, engine, now)
	if allowed := engine.Admit("managed-key", "gpt-5", now, candidates); !allowed.Allowed {
		t.Fatalf("allowed model = %+v", allowed)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	if unmanaged := engine.Admit("other-key", "gpt-5-mini", now); !unmanaged.Bypass {
		t.Fatalf("unmanaged model request = %+v, want bypass", unmanaged)
	}
}

func TestModelCatalogAndPolicyPersist(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Team Key", FiveHourMultiplier: 3, SevenDayMultiplier: 10, AllowedModels: []string{"gpt-5"}, Enabled: true})
	path := engine.config.DatabasePath
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", DisplayName: "GPT-5", Owner: "openai"}}); err != nil {
		t.Fatalf("ReplaceModels() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	cfg, err := NormalizeConfig(Config{DatabasePath: path, RequestUnits: 1})
	if err != nil {
		t.Fatalf("NormalizeConfig(reopen): %v", err)
	}
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(reopen): %v", err)
	}
	defer reopened.Close()
	if policies := reopened.Policies(); len(policies) != 1 || policies[0].FiveHourMultiplier != 3 || len(policies[0].AllowedModels) != 1 {
		t.Fatalf("reopened policies = %+v", policies)
	}
	models, err := reopened.Models()
	if err != nil || len(models) != 1 || models[0].ID != "gpt-5" {
		t.Fatalf("reopened models = %+v, err=%v", models, err)
	}
}

func TestReplaceModelsRejectsEmptyResultWithoutErasingCatalog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", DisplayName: "GPT-5", Owner: "openai"}}); err != nil {
		t.Fatalf("ReplaceModels(initial) error = %v", err)
	}
	if err := engine.ReplaceModels(nil); err == nil {
		t.Fatal("ReplaceModels(empty) succeeded, want rejection")
	}
	models, err := engine.Models()
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5" {
		t.Fatalf("catalog after rejected empty replacement = %+v", models)
	}
}

func TestValidateAllowedModelsRequiresSynchronizedCatalog(t *testing.T) {
	if err := validateAllowedModels(nil, nil); err != nil {
		t.Fatalf("unrestricted policy = %v, want nil", err)
	}
	catalog := []ModelCatalogEntry{{ID: "gpt-5", Available: true}, {ID: "gpt-5-mini", Available: false}}
	if err := validateAllowedModels([]string{"gpt-5"}, catalog); err != nil {
		t.Fatalf("known model = %v, want nil", err)
	}
	if err := validateAllowedModels([]string{"gpt-5-mini"}, catalog); err == nil {
		t.Fatal("unavailable model was accepted")
	}
	if err := validateAllowedModels([]string{"gpt-5-nonexistent"}, catalog); err == nil {
		t.Fatal("unknown model was accepted")
	}
}

func TestLegacyFingerprintMigrationPausesInsteadOfBlockingStartup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	_, err = store.db.Exec(`INSERT INTO key_policies(
key_id, name, key_sha256, group_id, five_hour_percent, seven_day_percent,
max_concurrency, five_hour_multiplier, seven_day_multiplier, allowed_models_json,
fingerprint_scheme, enabled, created_at, updated_at
) VALUES ('legacy', 'Legacy', ?, '', 0, 0, 1, 3, 10, '[]', '', 1, 1, 1)`, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		_ = store.Close()
		t.Fatalf("seed legacy policy: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
	cfg, err := NormalizeConfig(Config{DatabasePath: path, RequestUnits: 1, KeyHMACSecret: "test-only-hmac-secret-with-at-least-32-characters", RecordRetention: "168h"})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() should migrate legacy policy instead of failing: %v", err)
	}
	defer engine.Close()
	policies := engine.Policies()
	if len(policies) != 1 || policies[0].Enabled || !policies[0].NeedsRebind {
		t.Fatalf("migrated policies = %+v, want paused policy requiring rebind", policies)
	}
}

func newTestEngine(t *testing.T, policy KeyPolicy) *Engine {
	return newTestEngineWithRequestUnits(t, policy, 1)
}

func newTestEngineWithRequestUnits(t *testing.T, policy KeyPolicy, requestUnits int64) *Engine {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	secret := "test-only-hmac-secret-with-at-least-32-characters"
	policy.KeySHA256 = FingerprintAPIKey("managed-key", secret)
	cfg, err := NormalizeConfig(Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    requestUnits,
		KeyHMACSecret:   secret,
		RecordRetention: "168h",
		BootstrapKeys:   []KeyPolicy{policy},
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return engine
}

func readyNativeCandidate(t *testing.T, engine *Engine, now time.Time) []SchedulerCandidate {
	t.Helper()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	return []SchedulerCandidate{{AuthID: "account-a"}}
}

func updateOfficialUsagePercent(t *testing.T, engine *Engine, authID string, usedPercent float64, observedAt, resetAt time.Time) {
	t.Helper()
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: authID, Allowed: usedPercent < 100, LimitReached: usedPercent >= 100,
		Secondary: OfficialQuotaWindow{
			UsedPercent: usedPercent, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt,
		},
		ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(%s, %.2f%%) = %v", authID, usedPercent, err)
	}
}

func seedQuotaCalibration(t *testing.T, engine *Engine, authID string, accountCapacityX float64, tokensPerX int64, observedAt, resetAt time.Time) {
	t.Helper()
	calibration := quotaCalibration{
		AuthID: authID, TokensPerX: tokensPerX, Samples: 1,
		AccountCapacityX: accountCapacityX, WindowResetAt: resetAt.UTC().UnixMilli(), ObservedAt: observedAt.UTC(),
	}
	if err := engine.store.UpsertQuotaCalibration(calibration); err != nil {
		t.Fatalf("UpsertQuotaCalibration() = %v", err)
	}
	engine.calibrationMu.Lock()
	engine.quotaCalibrations[authID] = calibration
	engine.calibrationMu.Unlock()
}

func TestBucketKeySeparatesOneKeyAcrossAccountsForOfficialXAttribution(t *testing.T) {
	recordedAt := time.Now().UTC()
	first := bucketKey(UsageEvent{
		Scope: "key_account_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: recordedAt,
	})
	second := bucketKey(UsageEvent{
		Scope: "key_account_actual", KeyID: "managed", AuthID: "account-b", RecordedAt: recordedAt,
	})
	if first == second {
		t.Fatalf("Key/account bucket keys collided: first=%+v second=%+v", first, second)
	}
}

func TestAccountSourceConflictFailsClosedOnlyForManagedKeys(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)

	engine.SetAccountSourceConflict(true)
	if admission := engine.Admit("managed-key", "gpt-5", now, candidates); admission.Allowed || admission.Code != "quota_account_source_conflict" {
		t.Fatalf("managed admission = %+v, want duplicate-source 503", admission)
	}
	if admission := engine.Admit("unmanaged-key", "gpt-5", now, candidates); !admission.Bypass {
		t.Fatalf("unmanaged admission = %+v, want bypass", admission)
	}
}

func TestSharedPoolSelectsFreshOfficialQuotaCandidate(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", FiveHourMultiplier: 1, SevenDayMultiplier: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(account-a): %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-b", AuthIndex: "b", Name: "B", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(account-b): %v", err)
	}
	now := time.Now().UTC()
	for _, snapshot := range []OfficialQuotaSnapshot{
		{AuthID: "account-a", Allowed: true, Secondary: OfficialQuotaWindow{UsedPercent: 10, LimitWindowSeconds: 7 * 24 * 60 * 60}, ObservedAt: now},
		{AuthID: "account-b", Allowed: true, Secondary: OfficialQuotaWindow{UsedPercent: 30, LimitWindowSeconds: 7 * 24 * 60 * 60}, ObservedAt: now},
	} {
		if err := engine.UpdateOfficialQuota(snapshot); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s): %v", snapshot.AuthID, err)
		}
	}
	result := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-a"}, {AuthID: "account-b"}})
	if !result.Allowed || result.AuthID != "account-b" {
		t.Fatalf("Admit() = %+v, want account-b", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-b", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
}

func TestSharedPoolEnforcesKeyAllocationAtRuntime(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 0, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Second), candidates); !result.Allowed {
		t.Fatalf("admission = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now.Add(time.Second), Generate: true, TotalTokens: 100})
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		// A 5% movement on the configured 20x account confirms exactly 1x.
		Secondary:  OfficialQuotaWindow{UsedPercent: 5, LimitWindowSeconds: 7 * 24 * 60 * 60, ResetAt: &resetAt},
		ObservedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(confirmed x) = %v", err)
	}
	result := engine.Admit("managed-key", "gpt-5", now.Add(3*time.Minute), candidates)
	if result.Allowed || result.Code != "quota_allocation_exhausted" {
		t.Fatalf("over-allocation admission = %+v, want allocation 429", result)
	}
	snapshot := engine.Summary(now.Add(3 * time.Minute)).Keys[0].Allocation
	if snapshot.Used != officialXUnitsPerX || snapshot.Remaining != 0 {
		t.Fatalf("allocation snapshot = %+v, want exhausted one-x allocation", snapshot)
	}
}

func TestFractionalGlobalAllocationIsNotSplitPerAccount(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 0.1, Enabled: true})
	defer func() { _ = engine.Close() }()
	entries := []AccountPoolEntry{
		{AuthID: "account-small", Name: "Small", CapacityX: 1, Enabled: true},
		{AuthID: "account-large", Name: "Large", CapacityX: 20, Enabled: true},
	}
	if _, err := engine.UpsertAccountPoolEntries(entries); err != nil {
		t.Fatalf("UpsertAccountPoolEntries() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	weeklyReset := now.Add(sevenDayWindow)
	for _, authID := range []string{"account-small", "account-large"} {
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &weeklyReset},
			ObservedAt: now,
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s): %v", authID, err)
		}
	}
	targets := engine.allocationTargets(engine.Policies()[0], engine.config.RequestUnits, now)
	if len(targets) != 2 || targets[0].Capacity != officialXUnitsPerX/10 || targets[1].Capacity != officialXUnitsPerX/10 {
		t.Fatalf("fractional global allocation targets = %+v, want one complete 0.1x balance on each eligible account", targets)
	}
}

func TestGlobalKeyAllocationIsNotReducedBySelectedAccountWeight(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true})
	defer func() { _ = engine.Close() }()
	entries := []AccountPoolEntry{
		{AuthID: "account-small", Name: "Small", CapacityX: 1, Enabled: true},
		{AuthID: "account-large", Name: "Large", CapacityX: 20, Enabled: true},
	}
	if _, err := engine.UpsertAccountPoolEntries(entries); err != nil {
		t.Fatalf("UpsertAccountPoolEntries() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	for _, authID := range []string{"account-small", "account-large"} {
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
			ObservedAt: now,
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s): %v", authID, err)
		}
	}
	large := []SchedulerCandidate{{AuthID: "account-large"}}
	requestedAt := now.Add(time.Second)
	if result := engine.Admit("managed-key", "gpt-5", requestedAt, large); !result.Allowed {
		t.Fatalf("large-account admission = %+v, want full global 5x allowance", result)
	}
	// The official percentage calibrates the account scale only. The managed
	// Key must still provide its own full 5x Token evidence before the global
	// allocation is exhausted; unrelated account percentage is not Key usage.
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-large", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true,
		TotalTokens: 5 * fallbackTokensPerX(engine.config.RequestUnits),
	})
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-large", Allowed: true,
		// A 25% movement on the configured 20x account confirms the scale
		// represented by this Key's independently recorded 5x Token usage.
		Secondary:  OfficialQuotaWindow{UsedPercent: 25, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(confirmed 5x) = %v", err)
	}
	if allocation := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation; allocation.Completed != 5*officialXUnitsPerX {
		t.Fatalf("global Token-derived allocation = %+v, want completed 5x", allocation)
	}
	if result := engine.Admit("managed-key", "gpt-5", now.Add(10*time.Minute), large); result.Allowed || result.Code != "quota_allocation_exhausted" {
		t.Fatalf("admission after full global 5x = %+v, want allocation 429", result)
	}
}

func TestGlobalKeyAllocationFollowsAccountSwitch(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	entries := []AccountPoolEntry{
		{AuthID: "account-small", Name: "Small", CapacityX: 1, Enabled: true},
		{AuthID: "account-large", Name: "Large", CapacityX: 20, Enabled: true},
	}
	if _, err := engine.UpsertAccountPoolEntries(entries); err != nil {
		t.Fatalf("UpsertAccountPoolEntries() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	for _, authID := range []string{"account-small", "account-large"} {
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
			ObservedAt: now,
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s): %v", authID, err)
		}
	}
	for index, item := range []struct {
		authID      string
		usedPercent float64
	}{
		{authID: "account-large", usedPercent: 2.5},
		{authID: "account-small", usedPercent: 50},
	} {
		requestedAt := now.Add(time.Duration(index+1) * time.Second)
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: item.authID}}); !result.Allowed {
			t.Fatalf("switched-account admission %d = %+v, want one shared 1x balance", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: item.authID, Model: "gpt-5", RequestedAt: requestedAt, Generate: true, TotalTokens: 100})
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: item.authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{UsedPercent: item.usedPercent, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
			ObservedAt: now.Add(time.Duration(index+2) * time.Minute),
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s confirmed x) = %v", item.authID, err)
		}
	}
	if err := engine.flushAllocationPersistence(); err != nil {
		t.Fatalf("flushAllocationPersistence() = %v", err)
	}
	records, err := engine.store.LoadAllocationBuckets(now)
	if err != nil || len(records) != 2 {
		t.Fatalf("LoadAllocationBuckets() = %+v, err=%v; want one durable bucket per selected account", records, err)
	}
	for _, record := range records {
		if record.GlobalCapacityUnits != officialXUnitsPerX {
			t.Fatalf("durable global capacity for %s = %d, want one unsplit x balance %d", record.AuthID, record.GlobalCapacityUnits, officialXUnitsPerX)
		}
	}
	if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Minute), []SchedulerCandidate{{AuthID: "account-small"}}); result.Allowed || result.Code != "quota_allocation_exhausted" {
		t.Fatalf("admission after both accounts consumed one global x = %+v, want allocation 429", result)
	}
}

func TestOfficialWeeklyRemainingIsSharedAcrossKeys(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "first", Name: "First", AllocationX: 0.5, Enabled: true})
	defer func() { _ = engine.Close() }()
	first := engine.Policies()[0]
	if _, err := engine.UpsertPolicy(first, "first-key"); err != nil {
		t.Fatalf("UpsertPolicy(first): %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "second", Name: "Second", AllocationX: 0.5, Enabled: true}, "second-key"); err != nil {
		t.Fatalf("UpsertPolicy(second): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	weeklyReset := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		// The official snapshot, not a local request-count estimate, owns the
		// account-wide remaining allowance.
		Secondary:  OfficialQuotaWindow{UsedPercent: 93, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &weeklyReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index, rawKey := range []string{"first-key", "second-key"} {
		requestedAt := now.Add(time.Duration(index) * time.Second)
		if result := engine.Admit(rawKey, "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("admission %d = %+v, want allowed", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: rawKey, AuthID: "account-a", Model: "gpt-5", RequestedAt: requestedAt, Generate: true, TotalTokens: 1})
	}
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: false, LimitReached: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 100, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &weeklyReset},
		ObservedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(exhausted) = %v", err)
	}
	if result := engine.Admit("first-key", "gpt-5", now.Add(2*time.Minute), candidates); result.Allowed || result.Code != "quota_pool_exhausted" {
		t.Fatalf("shared weekly remainder must stop both Keys: %+v", result)
	}
}

func TestNormalizeOfficialQuotaSnapshotMigratesLegacyPrimaryToWeekly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(6 * 24 * time.Hour)
	snapshot := normalizeOfficialQuotaSnapshot(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true, ObservedAt: now,
		Primary:   OfficialQuotaWindow{UsedPercent: 24, LimitWindowSeconds: int64(fiveHourWindow / time.Second), ResetAt: &resetAt},
		Secondary: OfficialQuotaWindow{UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
	})
	if snapshot.Primary.LimitWindowSeconds != 0 {
		t.Fatalf("legacy primary still published after migration: %+v", snapshot.Primary)
	}
	if snapshot.Secondary.UsedPercent != 24 || snapshot.Secondary.LimitWindowSeconds != int64(sevenDayWindow/time.Second) || snapshot.Secondary.ResetAt == nil || !snapshot.Secondary.ResetAt.Equal(resetAt) {
		t.Fatalf("migrated weekly snapshot = %+v, want primary's weekly quota", snapshot.Secondary)
	}
}

func TestRecordUsageAfterPolicyDisabledIsAnalysed(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: false}, ""); err != nil {
		t.Fatalf("UpsertPolicy(disabled): %v", err)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 3})
	usage, err := engine.UsageRecords("managed", 20)
	if err != nil || len(usage) != 1 || usage[0].Units != 3 {
		t.Fatalf("UsageRecords() = %+v, %v; want disabled in-flight actual usage", usage, err)
	}
	logs, err := engine.DecisionLogs("managed", 20)
	if err != nil || len(logs) == 0 || logs[0].Reason != "actual_usage_after_policy_disabled" {
		t.Fatalf("DecisionLogs() = %+v, %v; want disabled in-flight audit record", logs, err)
	}
}

func TestActiveKeyAllocationDecreaseIsSavedWithoutShrinkingCurrentWindow(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 1, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(confirmed x) = %v", err)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 0.5, Enabled: true}, ""); err != nil {
		t.Fatalf("UpsertPolicy(reduced allocation) = %v", err)
	}
	if policy := engine.Policies()[0]; policy.AllocationX != 0.5 {
		t.Fatalf("saved allocation_x = %.2f, want 0.50", policy.AllocationX)
	}
	wantCompleted := capacityForX(float64(1)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
	if snapshot := engine.Summary(now.Add(time.Minute)).Keys[0].Allocation; snapshot.Capacity != officialXUnitsPerX || snapshot.Completed != wantCompleted {
		t.Fatalf("current window after decrease = %+v, want original capacity and recorded usage", snapshot)
	}
}

func TestOpenCannotBootstrapOverActiveSharedPool(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})

	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	cfg.BootstrapKeys = append(cfg.BootstrapKeys, KeyPolicy{
		ID:          "second",
		Name:        "Second",
		KeySHA256:   FingerprintAPIKey("second-key", cfg.KeyHMACSecret),
		AllocationX: 20,
		Enabled:     true,
	})
	if _, err := Open(cfg); err == nil {
		t.Fatal("Open(bootstrap pool over-allocation) error = nil, want active shared-pool rejection")
	}

	cfg.BootstrapKeys = cfg.BootstrapKeys[:len(cfg.BootstrapKeys)-1]
	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() after rejected bootstrap = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	for _, policy := range reopened.Policies() {
		if policy.ID == "second" {
			t.Fatal("rejected Open() must not write the bootstrap policy")
		}
	}
}

func TestReconfigureCannotBypassActiveWeeklyAllocationGuards(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})

	engine.configMu.RLock()
	cfg := engine.config
	engine.configMu.RUnlock()
	cfg.RequestUnits++
	if err := engine.Reconfigure(cfg); err != nil {
		t.Fatalf("Reconfigure(request_units change) = %v, want x ledger independence", err)
	}

	engine.configMu.RLock()
	cfg = engine.config
	engine.configMu.RUnlock()
	cfg.BootstrapKeys = append(cfg.BootstrapKeys, KeyPolicy{
		ID:          "second",
		Name:        "Second",
		KeySHA256:   FingerprintAPIKey("second-key", cfg.KeyHMACSecret),
		AllocationX: 20,
		Enabled:     true,
	})
	if err := engine.Reconfigure(cfg); err == nil {
		t.Fatal("Reconfigure(bootstrap pool over-allocation) error = nil, want active shared-pool rejection")
	}
	for _, policy := range engine.Policies() {
		if policy.ID == "second" {
			t.Fatal("rejected Reconfigure() must not write the bootstrap policy")
		}
	}
}

func TestDisabledKeyKeepsActiveAllocationReservedInSharedPool(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(time.Minute), now.Add(sevenDayWindow))
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: false}, ""); err != nil {
		t.Fatalf("UpsertPolicy(disabled): %v", err)
	}
	_, allocated := engine.PoolAllocation()
	if allocated != 1 {
		t.Fatalf("PoolAllocation() allocated = %.2f, want disabled Key's active 1x", allocated)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "second", Name: "Second", AllocationX: 20, Enabled: true}, "second-key"); err == nil {
		t.Fatal("UpsertPolicy(second) error = nil, want active disabled Key counted in pool")
	}
}

func TestAccountPoolTopologyCannotChangeWithActiveWeeklyLedger(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(time.Minute), now.Add(sevenDayWindow))
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-b", Name: "B", CapacityX: 20, Enabled: true}); err == nil {
		t.Fatal("UpsertAccountPoolEntry(add) error = nil, want active-week rejection")
	}
	if err := engine.DeleteAccountPoolEntry("account-a"); err == nil {
		t.Fatal("DeleteAccountPoolEntry() error = nil, want active-week rejection")
	}
}

func TestDuplicateUsageCallbackCannotConsumeAnotherReservation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	firstAt := now
	secondAt := now.Add(time.Second)
	for _, requestedAt := range []time.Time{firstAt, secondAt} {
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("Admit(%s) = %+v, want allowed", requestedAt, result)
		}
	}
	first := CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: firstAt, Generate: true, TotalTokens: 2}
	engine.RecordUsage(first)
	engine.RecordUsage(first)
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: secondAt, Generate: true, TotalTokens: 3})
	snapshot := engine.Summary(now).Keys[0].Allocation
	wantProvisional := engine.provisionalXUnits("account-a", 2, engine.config.RequestUnits) +
		engine.provisionalXUnits("account-a", 3, engine.config.RequestUnits)
	if snapshot.Completed != 0 || snapshot.Provisional != wantProvisional || snapshot.Reserved != 0 {
		t.Fatalf("allocation = %+v, want deduplicated provisional charge %d", snapshot, wantProvisional)
	}
	usage, err := engine.UsageRecords("managed", 20)
	if err != nil || len(usage) != 1 || usage[0].Units != 5 {
		t.Fatalf("UsageRecords() = %+v, %v; want deduplicated total", usage, err)
	}
}

func TestSharedPoolAllocationStartsANewLedgerAtOfficialWeeklyReset(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	firstReset := now.Add(30 * time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &firstReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(first): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index := 0; index < int(defaultSevenDayBaseRequests); index++ {
		requestedAt := now.Add(time.Duration(index) * time.Second)
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("first-window admission %d = %+v, want allowed", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: requestedAt, Generate: true, TotalTokens: 1})
	}
	updateOfficialUsagePercent(t, engine, "account-a", 100, now.Add(2*time.Minute), firstReset)
	if result := engine.Admit("managed-key", "gpt-5", now.Add(3*time.Minute), candidates); result.Allowed || result.Code != "quota_pool_exhausted" {
		t.Fatalf("first window should be exhausted: %+v", result)
	}

	nextNow := firstReset.Add(time.Minute)
	nextReset := nextNow.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &nextReset},
		ObservedAt: nextNow,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(next): %v", err)
	}
	if result := engine.Admit("managed-key", "gpt-5", nextNow, candidates); !result.Allowed {
		t.Fatalf("official reset must start a new Key allocation window: %+v", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: nextNow, Generate: true, TotalTokens: 1})
}

func TestSharedPoolDoesNotCreateANewLedgerForNearbyResetFallbackJitter(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	firstReset := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &firstReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(first): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index := 0; index < int(defaultSevenDayBaseRequests); index++ {
		requestedAt := now.Add(time.Duration(index) * time.Second)
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("first-window admission %d = %+v, want allowed", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: requestedAt, Generate: true, TotalTokens: 1})
	}
	updateOfficialUsagePercent(t, engine, "account-a", 100, now.Add(2*time.Minute), firstReset)

	// This models two reset_after_seconds polls that describe the same future
	// Codex window but were converted using slightly different local receive
	// times. It must retain the existing ledger identity rather than free quota.
	jitteredReset := firstReset.Add(750 * time.Millisecond)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 100, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &jitteredReset},
		ObservedAt: now.Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(jittered): %v", err)
	}
	if result := engine.Admit("managed-key", "gpt-5", now.Add(5*time.Minute), candidates); result.Allowed || (result.Code != "quota_allocation_exhausted" && result.Code != "quota_pool_exhausted") {
		t.Fatalf("nearby reset jitter must not start a new ledger: %+v", result)
	}
}

func TestResetAfterSnapshotDowngradesLegacyUnmarkedWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	legacyReset := now.Add(sevenDayWindow)
	currentReset := legacyReset.Add(time.Second)
	previous := OfficialQuotaSnapshot{
		ObservedAt: now,
		Secondary: OfficialQuotaWindow{
			LimitWindowSeconds: int64(sevenDayWindow / time.Second),
			ResetAt:            &legacyReset,
			// Older releases persisted reset_after_seconds with this false value.
			ResetEstimated: false,
		},
	}
	next := OfficialQuotaSnapshot{
		ObservedAt: now.Add(time.Minute),
		Secondary: OfficialQuotaWindow{
			LimitWindowSeconds: int64(sevenDayWindow / time.Second),
			ResetAt:            &currentReset,
			ResetEstimated:     true,
		},
	}
	stabilized := stabilizeOfficialQuotaSnapshot(previous, next, next.ObservedAt)
	if !stabilized.Secondary.ResetEstimated {
		t.Fatal("current reset_after_seconds snapshot must downgrade a legacy unmarked window to estimated")
	}
	if stabilized.Secondary.ResetAt == nil || !stabilized.Secondary.ResetAt.Equal(legacyReset) {
		t.Fatalf("stabilized reset = %v, want prior weekly identity %v", stabilized.Secondary.ResetAt, legacyReset)
	}
}

func TestSharedPoolKeepsMissingResetFallbackAcrossPolling(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	first := OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
		ObservedAt: now,
	}
	if err := engine.UpdateOfficialQuota(first); err != nil {
		t.Fatalf("UpdateOfficialQuota(first): %v", err)
	}
	firstQuota := engine.AccountPool(now)[0].Quota
	if firstQuota == nil || firstQuota.Secondary.ResetAt == nil || !firstQuota.Secondary.ResetEstimated {
		t.Fatalf("first missing-reset snapshot = %+v, want persisted fallback identity", firstQuota)
	}
	firstReset := *firstQuota.Secondary.ResetAt
	firstBaseline := firstQuota.Secondary.BaselineAt

	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
		ObservedAt: now.Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(second): %v", err)
	}
	secondQuota := engine.AccountPool(now)[0].Quota
	if secondQuota == nil || secondQuota.Secondary.ResetAt == nil || !secondQuota.Secondary.ResetAt.Equal(firstReset) {
		t.Fatalf("missing reset created a new official window: first=%v second=%+v", firstReset, secondQuota)
	}
	if !secondQuota.Secondary.BaselineAt.Equal(firstBaseline) {
		t.Fatalf("missing reset moved local accounting baseline: first=%v second=%v", firstBaseline, secondQuota.Secondary.BaselineAt)
	}
}

func TestEstimatedWeeklyResetRequiresPersistentDoubleConfirmation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	initial := OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
		ObservedAt: now,
	}
	if err := engine.UpdateOfficialQuota(initial); err != nil {
		t.Fatalf("initial UpdateOfficialQuota(): %v", err)
	}
	initialQuota := engine.AccountPool(now)[0].Quota
	if initialQuota == nil || initialQuota.Secondary.ResetAt == nil || !initialQuota.Secondary.ResetEstimated {
		t.Fatalf("initial missing-reset snapshot = %+v, want estimated weekly identity", initialQuota)
	}
	if result := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-a"}}); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}

	// The first inferred next-window snapshot arrives inside the tolerance
	// period. It must still create a candidate and keep new Key allocation
	// closed until the second confirmation, rather than silently opening a new
	// weekly ledger because the prior reset is only one minute old.
	firstPostReset := initialQuota.Secondary.ResetAt.Add(time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
		ObservedAt: firstPostReset,
	}); err != nil {
		t.Fatalf("first post-reset UpdateOfficialQuota(): %v", err)
	}
	if pending := engine.PendingSettlementCount(); pending != 1 {
		t.Fatalf("single estimated refresh released pending settlement: %d", pending)
	}
	if result := engine.Admit("managed-key", "gpt-5", firstPostReset, []SchedulerCandidate{{AuthID: "account-a"}}); result.Allowed || result.Code != "quota_snapshot_unavailable" {
		t.Fatalf("unconfirmed estimated reset admission = %+v, want snapshot-unavailable block", result)
	}
	if stale := engine.StalePoolCandidates([]SchedulerCandidate{{AuthID: "account-a"}}, firstPostReset); len(stale) != 1 || stale[0] != "account-a" {
		t.Fatalf("unconfirmed estimated reset stale candidates = %v, want account-a refresh", stale)
	}
	if confirmationAt, pending := engine.PendingEstimatedResetConfirmationAt("account-a"); !pending || !confirmationAt.Equal(firstPostReset.Add(quotaResetStabilityTolerance)) {
		t.Fatalf("estimated reset confirmation schedule = %v pending:%v, want %v", confirmationAt, pending, firstPostReset.Add(quotaResetStabilityTolerance))
	}
	snapshots, err := engine.store.LoadOfficialQuotaSnapshots()
	if err != nil {
		t.Fatalf("LoadOfficialQuotaSnapshots(): %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].SecondaryEstimatedResetCandidateAt == nil || snapshots[0].SecondaryEstimatedResetCandidateSeenAt == nil {
		t.Fatalf("estimated reset candidate was not persisted: %+v", snapshots)
	}

	// Restart the engine against the same SQLite database. Process-local
	// settlement maps are deliberately cleared to model an unclean exit: only
	// the durable reservation and the persisted reset candidate may complete
	// the accounting on the next process.
	engine.configMu.RLock()
	config := engine.config
	engine.configMu.RUnlock()
	engine.pendingSettlements.Store(0)
	engine.settlementMu.Lock()
	engine.pendingSettlementsByKey = make(map[string]int64)
	engine.pendingSettlementsByAuth = make(map[string]int64)
	engine.pendingSettlementsByBucket = make(map[allocationBucketKey]int64)
	engine.settlementMu.Unlock()
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() before restart: %v", err)
	}
	restarted, err := Open(config)
	if err != nil {
		t.Fatalf("Open() after restart: %v", err)
	}
	engine = restarted

	secondPostReset := firstPostReset.Add(quotaResetStabilityTolerance + time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second)},
		ObservedAt: secondPostReset,
	}); err != nil {
		t.Fatalf("second post-reset UpdateOfficialQuota(): %v", err)
	}
	confirmedQuota := engine.AccountPool(secondPostReset)[0].Quota
	if confirmedQuota == nil || confirmedQuota.SecondaryEstimatedResetCandidateAt != nil || confirmedQuota.SecondaryEstimatedResetCandidateSeenAt != nil {
		t.Fatalf("confirmed estimated reset retained its persisted candidate: %+v", confirmedQuota)
	}
	engine.allocationMu.Lock()
	oldBuckets := 0
	for bucket := range engine.allocationBuckets {
		if bucket.AuthID == "account-a" && bucket.WindowResetAt <= initialQuota.Secondary.ResetAt.UnixMilli() {
			oldBuckets++
		}
	}
	engine.allocationMu.Unlock()
	if oldBuckets != 0 {
		t.Fatalf("confirmed estimated reset retained %d old allocation bucket(s)", oldBuckets)
	}
	if result := engine.Admit("managed-key", "gpt-5", secondPostReset, []SchedulerCandidate{{AuthID: "account-a"}}); !result.Allowed {
		t.Fatalf("confirmed estimated reset admission = %+v, want allowed", result)
	} else {
		engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: secondPostReset, Generate: true, TotalTokens: 1})
	}
}

func TestDelayedOfficialSnapshotDoesNotDoubleCountLocalCompletion(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(first): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now.Add(5 * time.Minute),
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(delayed): %v", err)
	}
	engine.allocationMu.Lock()
	defer engine.allocationMu.Unlock()
	for key, state := range engine.officialAccountWindows {
		if key.AuthID == "account-a" && key.Kind == officialSecondaryWindow {
			if state.Capacity != officialXUnitsPerX || state.Completed != 0 || state.Reserved != 0 {
				t.Fatalf("delayed snapshot mixed local Tokens into the official x guard: %+v", state)
			}
			return
		}
	}
	t.Fatal("secondary official account window was not rebuilt")
}

func TestSharedPoolFallsBackWhenPreferredAccountIsLocallyFull(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	for _, entry := range []AccountPoolEntry{
		{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true},
		{AuthID: "account-b", AuthIndex: "b", Name: "B", CapacityX: 1, Enabled: true},
	} {
		if _, err := engine.UpsertAccountPoolEntry(entry); err != nil {
			t.Fatalf("UpsertAccountPoolEntry(%s): %v", entry.AuthID, err)
		}
	}
	now := time.Now().UTC().Truncate(time.Minute)
	for _, authID := range []string{"account-a", "account-b"} {
		resetAt := now.Add(sevenDayWindow)
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
			ObservedAt: now,
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s): %v", authID, err)
		}
	}
	engine.allocationMu.Lock()
	for key, state := range engine.officialAccountWindows {
		if key.AuthID == "account-a" {
			state.Reserved = state.Capacity
			engine.officialAccountWindows[key] = state
		}
	}
	engine.allocationMu.Unlock()
	result := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{
		{AuthID: "account-a", Priority: 0},
		{AuthID: "account-b", Priority: 1},
	})
	if !result.Allowed || result.AuthID != "account-b" {
		t.Fatalf("Admit() = %+v, want fallback to account-b", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-b", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
}

func TestAllocationSettlementIndexesOnlyOutstandingReservation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	lookup := allocationPendingKey{KeyID: "managed", AuthID: "account-a"}
	engine.allocationMu.Lock()
	pendingCount := len(engine.pendingAllocationBuckets[lookup])
	engine.allocationMu.Unlock()
	if pendingCount != 1 {
		t.Fatalf("pending allocation index count = %d, want one reservation", pendingCount)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: now, Generate: true, TotalTokens: 1})
	engine.allocationMu.Lock()
	pendingCount = len(engine.pendingAllocationBuckets[lookup])
	engine.allocationMu.Unlock()
	if pendingCount != 0 {
		t.Fatalf("pending allocation index count after settlement = %d, want zero", pendingCount)
	}
}

func TestAmbiguousUsageCallbackKeepsEveryReservation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	firstAt := now
	secondAt := now.Add(time.Minute)
	for _, requestedAt := range []time.Time{firstAt, secondAt} {
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("Admit(%s) = %+v, want allowed", requestedAt, result)
		}
	}

	// CPA v7.2.97 does not provide a request ID. A callback that cannot match
	// either minute bucket must not settle whichever reservation happens to be
	// first in a map iteration; both charges remain conservative until a
	// uniquely correlated callback arrives.
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: now.Add(5 * time.Minute), Generate: true, TotalTokens: 1})
	lookup := allocationPendingKey{KeyID: "managed", AuthID: "account-a"}
	engine.allocationMu.Lock()
	pendingCount := len(engine.pendingAllocationBuckets[lookup])
	reserved := int64(0)
	completed := int64(0)
	for key := range engine.pendingAllocationBuckets[lookup] {
		bucket := engine.allocationBuckets[key]
		reserved += bucket.Reserved
		completed += bucket.Completed
	}
	engine.allocationMu.Unlock()
	if pendingCount != 2 || reserved != 2*engine.config.RequestUnits || completed != 0 {
		t.Fatalf("ambiguous callback changed reservation state: pending=%d reserved=%d completed=%d", pendingCount, reserved, completed)
	}
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil {
		t.Fatalf("DecisionLogs(): %v", err)
	}
	if len(logs) != 1 || logs[0].Reason != "ambiguous_usage_callback" {
		t.Fatalf("ambiguous callback logs = %+v, want one ignored ambiguity record", logs)
	}

	// Exact original request times remain safe to settle after the rejected
	// callback, allowing CPA's normal records to release their own reservation.
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: firstAt, Generate: true, TotalTokens: 1})
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: secondAt, Generate: true, TotalTokens: 1})
}

func TestOfficialWeeklyResetExpiresUnresolvedReservations(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	previousReset := now.Add(3 * time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &previousReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("initial UpdateOfficialQuota(): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for _, requestedAt := range []time.Time{now, now.Add(time.Minute)} {
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("Admit(%s) = %+v, want allowed", requestedAt, result)
		}
	}
	if pending := engine.pendingSettlements.Load(); pending != 2 {
		t.Fatalf("pending settlements before reset = %d, want 2", pending)
	}

	// A successful new weekly snapshot is the only authority that can end a
	// missing terminal callback. Wall-clock expiry by itself is intentionally
	// insufficient because an old upstream response must never release quota.
	afterReset := previousReset.Add(time.Minute)
	nextReset := afterReset.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &nextReset},
		ObservedAt: afterReset,
	}); err != nil {
		t.Fatalf("post-reset UpdateOfficialQuota(): %v", err)
	}
	if pending := engine.pendingSettlements.Load(); pending != 0 {
		t.Fatalf("pending settlements after official reset = %d, want 0", pending)
	}
	engine.allocationMu.Lock()
	oldBucketCount := 0
	for key := range engine.allocationBuckets {
		if key.AuthID == "account-a" && key.WindowResetAt <= previousReset.UnixMilli() {
			oldBucketCount++
		}
	}
	engine.allocationMu.Unlock()
	if oldBucketCount != 0 {
		t.Fatalf("old official-window allocation buckets remained = %d", oldBucketCount)
	}
	if err := engine.flushPending(); err != nil {
		t.Fatalf("flushPending(): %v", err)
	}
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil {
		t.Fatalf("DecisionLogs(): %v", err)
	}
	expiredReservations := int64(0)
	for _, entry := range logs {
		if entry.Decision == "expired" && entry.Reason == "reservation_expired_at_official_reset" {
			expiredReservations += entry.Units
		}
	}
	if expiredReservations != 2 {
		t.Fatalf("decision logs = %+v, want official-reset expiration audit", logs)
	}
	if err := engine.DeletePolicy("managed"); err != nil {
		t.Fatalf("DeletePolicy() after official reset = %v", err)
	}
	if err := engine.DeleteAccountPoolEntry("account-a"); err != nil {
		t.Fatalf("DeleteAccountPoolEntry() after official reset = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() after official reset = %v", err)
	}
}

func TestStalePastSnapshotCannotExpireReservation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	previousReset := now.Add(time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &previousReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("initial UpdateOfficialQuota(): %v", err)
	}
	if result := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-a"}}); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}

	// This payload still names the completed old reset. It is stale, not proof
	// that Codex has issued a new weekly window, even though its observation
	// time is now later than that reset.
	staleObservedAt := previousReset.Add(3 * time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &previousReset},
		ObservedAt: staleObservedAt,
	}); err != nil {
		t.Fatalf("stale UpdateOfficialQuota(): %v", err)
	}
	if pending := engine.pendingSettlements.Load(); pending != 1 {
		t.Fatalf("stale snapshot released reservation: pending=%d, want 1", pending)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: now, Generate: true, TotalTokens: 1})
}

func TestRecoveredCallbackCannotDrainNewPendingSettlement(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("old Admit() = %+v, want allowed", result)
	}

	// Model a process restart: durable reservations remain, while callback
	// waits are process-local and must not be reconstructed blindly.
	engine.pendingSettlements.Store(0)
	engine.settlementMu.Lock()
	engine.pendingSettlementsByKey = make(map[string]int64)
	engine.pendingSettlementsByAuth = make(map[string]int64)
	engine.pendingSettlementsByBucket = make(map[allocationBucketKey]int64)
	engine.settlementMu.Unlock()
	newAt := now.Add(time.Minute)
	if result := engine.Admit("managed-key", "gpt-5", newAt, candidates); !result.Allowed {
		t.Fatalf("new Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: now, Generate: true, TotalTokens: 1})
	if pending := engine.pendingSettlements.Load(); pending != 1 {
		t.Fatalf("recovered callback drained new request: pending=%d, want 1", pending)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: newAt, Generate: true, TotalTokens: 1})
	if pending := engine.pendingSettlements.Load(); pending != 0 {
		t.Fatalf("new callback pending=%d, want 0", pending)
	}
}

func TestFailedZeroUsageDoesNotRetainEmptyAllocationBucket(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if result := engine.Admit("managed-key", "gpt-5", now, candidates); !result.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", result)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: now, Generate: true, Failed: true, FailureStatus: 502})
	if err := engine.flushAllocationPersistence(); err != nil {
		t.Fatalf("flushAllocationPersistence(): %v", err)
	}
	records, err := engine.store.LoadAllocationBuckets(now)
	if err != nil {
		t.Fatalf("LoadAllocationBuckets(): %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("empty failed allocation bucket persisted = %+v", records)
	}
	engine.allocationMu.Lock()
	bucketCount := len(engine.allocationBuckets)
	cycleCount := len(engine.allocationCycles)
	engine.allocationMu.Unlock()
	if bucketCount != 0 || cycleCount != 0 {
		t.Fatalf("empty failed allocation bucket remained in memory: buckets=%d cycles=%d", bucketCount, cycleCount)
	}
}

func TestPruneAllocationStateDropsExpiredCompletedBuckets(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	expiredCompleted := allocationBucketKey{KeyID: "completed", AuthID: "account-a", WindowResetAt: now.Add(-time.Minute).UnixMilli(), BucketAt: now.Add(-2 * time.Minute).UnixMilli()}
	expiredReserved := allocationBucketKey{KeyID: "reserved", AuthID: "account-a", WindowResetAt: now.Add(-time.Minute).UnixMilli(), BucketAt: now.Add(-2 * time.Minute).UnixMilli()}
	active := allocationBucketKey{KeyID: "active", AuthID: "account-a", WindowResetAt: now.Add(sevenDayWindow).UnixMilli(), BucketAt: now.UnixMilli()}
	expiredWindow := officialAccountWindowKey{AuthID: "account-a", Kind: officialSecondaryWindow, WindowResetAt: now.Add(-time.Minute).UnixMilli()}
	pendingExpiredWindow := officialAccountWindowKey{AuthID: "account-b", Kind: officialSecondaryWindow, WindowResetAt: now.Add(-time.Minute).UnixMilli()}

	engine.allocationMu.Lock()
	for key, state := range map[allocationBucketKey]allocationBucketState{
		expiredCompleted: {Completed: 3},
		expiredReserved:  {Reserved: 5},
		active:           {Completed: 7},
	} {
		engine.setAllocationBucketLocked(key, state)
		cycleKey := allocationCycleKey{KeyID: key.KeyID, AuthID: key.AuthID, WindowResetAt: key.WindowResetAt}
		cycle := engine.allocationCycles[cycleKey]
		cycle.Completed += state.Completed
		cycle.Reserved += state.Reserved
		engine.allocationCycles[cycleKey] = cycle
	}
	engine.officialAccountWindows[expiredWindow] = officialAccountWindowState{Completed: 3}
	engine.officialAccountWindows[pendingExpiredWindow] = officialAccountWindowState{Reserved: 5}
	engine.allocationMu.Unlock()

	engine.pruneAllocationState(now)
	engine.allocationMu.Lock()
	_, completedExists := engine.allocationBuckets[expiredCompleted]
	reservedState, reservedExists := engine.allocationBuckets[expiredReserved]
	_, activeExists := engine.allocationBuckets[active]
	_, completedCycleExists := engine.allocationCycles[allocationCycleKey{KeyID: expiredCompleted.KeyID, AuthID: expiredCompleted.AuthID, WindowResetAt: expiredCompleted.WindowResetAt}]
	_, expiredWindowExists := engine.officialAccountWindows[expiredWindow]
	_, pendingExpiredWindowExists := engine.officialAccountWindows[pendingExpiredWindow]
	pendingCount := len(engine.pendingAllocationBuckets[allocationPendingKey{KeyID: "reserved", AuthID: "account-a"}])
	engine.allocationMu.Unlock()
	if completedExists || !reservedExists || reservedState.Reserved != 5 || !activeExists {
		t.Fatalf("pruned allocation buckets completed=%v reserved=%+v active=%v", completedExists, reservedState, activeExists)
	}
	if completedCycleExists || expiredWindowExists || !pendingExpiredWindowExists || pendingCount != 1 {
		t.Fatalf("pruned indexes cycle=%v expiredWindow=%v pendingWindow=%v pending=%d", completedCycleExists, expiredWindowExists, pendingExpiredWindowExists, pendingCount)
	}
}

func TestSchedulerMarkersDoNotExhaustSharedPoolAcrossRestart(t *testing.T) {
	policy := KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true}
	engine := newTestEngineWithRequestUnits(t, policy, defaultRequestUnits)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index := 0; index < int(defaultSevenDayBaseRequests); index++ {
		if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Duration(index)*time.Second), candidates); !result.Allowed {
			t.Fatalf("reservation %d = %+v, want allowed", index, result)
		}
	}
	// A process crash has no graceful Close call to wait for terminal callbacks.
	// Reset this in-memory-only shutdown counter so this test can close the test
	// store and verify that the already durable reservations are recovered by a
	// new process.
	engine.pendingSettlements.Store(0)
	engine.settlementMu.Lock()
	engine.pendingSettlementsByKey = make(map[string]int64)
	engine.pendingSettlementsByAuth = make(map[string]int64)
	engine.pendingSettlementsByBucket = make(map[allocationBucketKey]int64)
	engine.settlementMu.Unlock()
	config := engine.config
	if err := engine.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(restart): %v", err)
	}
	defer func() { _ = reopened.Close() }()
	snapshot := reopened.Summary(now).Keys[0].Allocation
	if snapshot.Reserved != defaultSevenDayBaseRequests || snapshot.Completed != 0 {
		t.Fatalf("recovered scheduler markers = %+v, want %d lightweight markers", snapshot, defaultSevenDayBaseRequests)
	}
	if result := reopened.Admit("managed-key", "gpt-5", now.Add(time.Minute), candidates); !result.Allowed {
		t.Fatalf("scheduler markers must not exhaust the confirmed x allowance: %+v", result)
	}
}

func TestRepeatedSchedulerPicksUseActualUsageInsteadOfRequestEstimate(t *testing.T) {
	engine := newTestEngineWithRequestUnits(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 10, Enabled: true}, defaultRequestUnits)
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)

	const schedulerPicks = 300
	const completedRequests = 150
	const tokensPerRequest = int64(80000)
	for index := 0; index < schedulerPicks; index++ {
		if result := engine.Admit("managed-key", "gpt-5", now.Add(time.Duration(index)*time.Millisecond), candidates); !result.Allowed {
			t.Fatalf("scheduler pick %d = %+v, want allowed", index, result)
		}
	}
	for index := 0; index < completedRequests; index++ {
		engine.RecordUsage(CompletedUsage{
			APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
			RequestedAt: now.Add(time.Duration(index) * time.Millisecond),
			Generate:    true, TotalTokens: tokensPerRequest,
		})
	}

	allocation := engine.Summary(now).Keys[0].Allocation
	wantReserved := int64(schedulerPicks - completedRequests)
	wantProvisional := engine.provisionalXUnits("account-a", tokensPerRequest, engine.config.RequestUnits) * completedRequests
	if allocation.Completed != 0 || allocation.Provisional != wantProvisional || allocation.Reserved != wantReserved || allocation.Used != wantProvisional+wantReserved {
		t.Fatalf("allocation = %+v, want provisional=%d from completed Tokens and reserved=%d lightweight markers; scheduler picks must not be charged as %d-Token requests", allocation, wantProvisional, wantReserved, defaultRequestUnits)
	}
}

func TestSharedPoolReturnsExhaustedOnlyAfterConfirmedOfficialSnapshot(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC()
	first := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-a"}})
	if first.Allowed || first.Code != "quota_snapshot_unavailable" {
		t.Fatalf("Admit() without snapshot = %+v, want snapshot unavailable", first)
	}
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{AuthID: "account-a", Allowed: false, LimitReached: true, Secondary: OfficialQuotaWindow{UsedPercent: 100, LimitWindowSeconds: 7 * 24 * 60 * 60}, ObservedAt: now}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	second := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-a"}})
	if second.Allowed || second.Code != "quota_pool_exhausted" {
		t.Fatalf("Admit() exhausted = %+v, want pool exhausted", second)
	}
}

func TestStalePoolCandidatesRefreshesFreshSnapshotWhoseWeeklyResetExpired(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC()
	expiredReset := now.Add(-30 * time.Second)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &expiredReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	stale := engine.StalePoolCandidates([]SchedulerCandidate{{AuthID: "account-a"}}, now)
	if len(stale) != 1 || stale[0] != "account-a" {
		t.Fatalf("StalePoolCandidates() = %v, want immediate weekly-reset refresh", stale)
	}
}

func TestPoolRejectsOverAllocation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "first", Name: "First", FiveHourMultiplier: 1, SevenDayMultiplier: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry: %v", err)
	}
	_, err := engine.UpsertPolicy(KeyPolicy{ID: "second", Name: "Second", AllocationX: 0.1, Enabled: true}, "second-key")
	if err == nil {
		t.Fatal("UpsertPolicy() error = nil, want total allocation rejection")
	}
}

func TestDeleteAccountPoolEntryKeepsHiddenOfficialSnapshotForSafeReAdd(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{AuthID: "account-a", Allowed: true, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("UpdateOfficialQuota(): %v", err)
	}
	if err := engine.DeleteAccountPoolEntry("account-a"); err != nil {
		t.Fatalf("DeleteAccountPoolEntry(): %v", err)
	}
	snapshots, err := engine.store.LoadOfficialQuotaSnapshots()
	if err != nil {
		t.Fatalf("LoadOfficialQuotaSnapshots(): %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].AuthID != "account-a" {
		t.Fatalf("persisted snapshots = %+v, want retained hidden account snapshot", snapshots)
	}
	if accounts := engine.AccountPool(time.Now().UTC()); len(accounts) != 0 {
		t.Fatalf("visible account pool = %+v, want removed account hidden from panel", accounts)
	}
}

func TestDeleteAccountCannotRebaseAnActiveSharedPool(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "first", Name: "First", AllocationX: 0.5, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "second", Name: "Second", AllocationX: 0.5, Enabled: true}, "second-key"); err != nil {
		t.Fatalf("UpsertPolicy(second): %v", err)
	}
	entry := AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}
	if _, err := engine.UpsertAccountPoolEntry(entry); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(): %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	snapshot := OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}
	if err := engine.UpdateOfficialQuota(snapshot); err != nil {
		t.Fatalf("UpdateOfficialQuota(first): %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index := 0; index < 10; index++ {
		requestedAt := now.Add(time.Duration(index) * time.Second)
		if result := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("first Key admission %d = %+v, want allowed", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", RequestedAt: requestedAt, Generate: true, TotalTokens: 1})
	}
	// Before the first trustworthy calibration, Tokens remain analytics only.
	// The 1x account's official movement from 0% to 50% establishes the first
	// Key's confirmed 0.5x usage and protects account-pool topology.
	updateOfficialUsagePercent(t, engine, "account-a", 50, now.Add(2*time.Minute), resetAt)
	if err := engine.DeleteAccountPoolEntry("account-a"); err == nil {
		t.Fatal("DeleteAccountPoolEntry() error = nil, want active-week rejection")
	}
	for index := 0; index < 5; index++ {
		requestedAt := now.Add(3*time.Minute + time.Duration(index)*time.Second)
		if result := engine.Admit("second-key", "gpt-5", requestedAt, candidates); !result.Allowed {
			t.Fatalf("second Key admission %d = %+v, want remaining shared allowance", index, result)
		}
		engine.RecordUsage(CompletedUsage{APIKey: "second-key", AuthID: "account-a", RequestedAt: requestedAt, Generate: true, TotalTokens: 1})
	}
	updateOfficialUsagePercent(t, engine, "account-a", 100, now.Add(5*time.Minute), resetAt)
	if result := engine.Admit("second-key", "gpt-5", now.Add(6*time.Minute), candidates); result.Allowed || result.Code != "quota_pool_exhausted" {
		t.Fatalf("delete/re-add reset shared account remainder: %+v", result)
	}
}

func TestOperationalLogsAreStoredSeparatelyAndFilterable(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	engine.LogOperational("info", "plugin_started", "codex-carpool 已启动", "", "")
	engine.LogOperational("error", "quota_sync_failed", "官方额度同步失败：HTTP 401", "account-a", "")
	logs, err := engine.OperationalLogs("error", "", 20)
	if err != nil {
		t.Fatalf("OperationalLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Level != "error" || logs[0].AuthID != "account-a" || logs[0].Event != "quota_sync_failed" {
		t.Fatalf("filtered operational logs = %+v", logs)
	}
	all, err := engine.OperationalLogs("", "", 20)
	if err != nil || len(all) != 2 {
		t.Fatalf("all operational logs = %+v, err=%v", all, err)
	}
	matched, err := engine.OperationalLogs("", "HTTP 401", 20)
	if err != nil || len(matched) != 1 || matched[0].Event != "quota_sync_failed" {
		t.Fatalf("searched operational logs = %+v, err=%v", matched, err)
	}
	page, err := engine.OperationalLogPage("", "", 1, 1)
	if err != nil || page.Total != 2 || page.TotalPages != 2 || page.Page != 1 || page.PageSize != 1 || len(page.Logs) != 1 {
		t.Fatalf("first operational-log page = %+v, err=%v", page, err)
	}
	page, err = engine.OperationalLogPage("", "", 2, 1)
	if err != nil || page.Page != 2 || len(page.Logs) != 1 || page.Logs[0].Event == "" {
		t.Fatalf("second operational-log page = %+v, err=%v", page, err)
	}
	page, err = engine.OperationalLogPage("error", "HTTP 401", 9, 20)
	if err != nil || page.Total != 1 || page.TotalPages != 1 || page.Page != 1 || len(page.Logs) != 1 || page.Logs[0].Event != "quota_sync_failed" {
		t.Fatalf("clamped searched operational-log page = %+v, err=%v", page, err)
	}
}

func TestDecisionLogsAreFilterableAndPagedInSQLite(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	if err := engine.store.FlushUsageAndLogs(nil, []DecisionLog{
		{KeyID: "managed", AuthID: "account-a", Model: "gpt-5", RequestContent: "用户发送了你好", RequestedAt: now.Add(-2 * time.Minute), Decision: "completed", Reason: "actual_token_settlement", Units: 12},
		{KeyID: "managed", AuthID: "account-a", Model: "gpt-5-mini", RequestedAt: now.Add(-time.Minute), Decision: "blocked", StatusCode: 403, Reason: "model_not_allowed"},
		{KeyID: "other", AuthID: "account-b", Model: "gpt-5", RequestedAt: now, Decision: "failed", StatusCode: 502, Reason: "upstream_failed"},
	}); err != nil {
		t.Fatalf("FlushUsageAndLogs() error = %v", err)
	}
	page, err := engine.DecisionLogPage("managed", "", "", 1, 1)
	if err != nil || page.Total != 2 || page.TotalPages != 2 || page.Page != 1 || page.PageSize != 1 || len(page.Logs) != 1 || page.Logs[0].Decision != "blocked" {
		t.Fatalf("first decision-log page = %+v, err=%v", page, err)
	}
	page, err = engine.DecisionLogPage("managed", "completed", "settlement", 4, 20)
	if err != nil || page.Total != 1 || page.TotalPages != 1 || page.Page != 1 || len(page.Logs) != 1 || page.Logs[0].Decision != "completed" {
		t.Fatalf("filtered decision-log page = %+v, err=%v", page, err)
	}
	page, err = engine.DecisionLogPage("managed", "", "用户发送", 1, 20)
	if err != nil || page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].RequestContent != "用户发送了你好" || page.Logs[0].AuthID != "account-a" {
		t.Fatalf("request-content search page = %+v, err=%v", page, err)
	}
	if _, err := engine.DecisionLogPage("managed", "unknown", "", 1, 20); err == nil {
		t.Fatal("DecisionLogPage() accepted an invalid decision filter")
	}
}

func TestQuotaDebugExplainsGlobalKeyReservationAcrossAccounts(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	entries := []AccountPoolEntry{
		{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true},
		{AuthID: "account-b", AuthIndex: "b", Name: "B", CapacityX: 20, Enabled: true},
	}
	if _, err := engine.UpsertAccountPoolEntries(entries); err != nil {
		t.Fatalf("UpsertAccountPoolEntries() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	for _, item := range []struct {
		authID string
		used   float64
	}{
		{authID: "account-a", used: 21},
		{authID: "account-b", used: 32},
	} {
		resetAt := now.Add(sevenDayWindow)
		if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
			AuthID: item.authID, Allowed: true,
			Secondary:  OfficialQuotaWindow{UsedPercent: item.used, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
			ObservedAt: now,
		}); err != nil {
			t.Fatalf("UpdateOfficialQuota(%s) = %v", item.authID, err)
		}
	}
	if admission := engine.Admit("managed-key", "gpt-5", now, []SchedulerCandidate{{AuthID: "account-b"}}); !admission.Allowed || admission.AuthID != "account-b" {
		t.Fatalf("Admit() = %+v, want account-b reservation", admission)
	}

	debug, err := engine.QuotaDebug("managed", now)
	if err != nil {
		t.Fatalf("QuotaDebug() = %v", err)
	}
	if debug.Formula.RequestReservationTokens != 1 || debug.Formula.BaseRequestsPerX != defaultSevenDayBaseRequests || debug.Formula.PerXCapacityTokens != defaultSevenDayBaseRequests {
		t.Fatalf("debug formula = %+v", debug.Formula)
	}
	if debug.Formula.FixedPointUnitsPerX != officialXUnitsPerX || math.Abs(debug.Formula.RequestReservationX-0.000001) > 0.000000001 {
		t.Fatalf("debug x formula = %+v", debug.Formula)
	}
	if debug.Formula.EnforcementMethod != "official_calibrated_actual_token_x_plus_bounded_provisional_guard" ||
		debug.Formula.CalibrationMethod != "aligned_official_percent_delta_with_completed_account_tokens" ||
		debug.Formula.ProvisionalPercentCap != officialProvisionalPercentCap {
		t.Fatalf("debug enforcement method = %+v", debug.Formula)
	}
	if debug.Allocation.Capacity != officialXUnitsPerX || debug.Allocation.Completed != 0 || debug.Allocation.Reserved != 1 || debug.Allocation.Used != 1 {
		t.Fatalf("debug aggregate allocation = %+v, want one reservation in the global Key balance", debug.Allocation)
	}
	if len(debug.Accounts) != 2 {
		t.Fatalf("debug accounts = %+v, want two accounts", debug.Accounts)
	}
	bySuffix := make(map[string]QuotaDebugAccountWindow, len(debug.Accounts))
	for _, account := range debug.Accounts {
		bySuffix[account.AccountSuffix] = account
	}
	if account := bySuffix["••••nt-a"]; !account.Eligible || account.KeyCapacity != officialXUnitsPerX || account.AccountReserved != 0 || account.AccountLabel == "" {
		t.Fatalf("account A diagnostic = %+v, want an uncharged account in the shared global Key balance", account)
	}
	if account := bySuffix["••••nt-b"]; !account.Eligible || account.KeyCapacity != officialXUnitsPerX || account.AccountReserved != 1 || account.AccountCompleted != 0 || account.AccountLabel == "" {
		t.Fatalf("account B diagnostic = %+v, want the selected account to carry the reservation without shrinking the Key balance", account)
	}
}

func TestOfficialPercentCalibrationUsesCompletedAccountTokens(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(initial) = %v", err)
	}
	requestedAt := now.Add(time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: "account-a"}}); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	// The dedicated account aggregate must carry the terminal value, not the
	// one-token admission reservation, before it is matched to the next
	// official percentage poll.
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: requestedAt, Generate: true, TotalTokens: 30})
	nextObservedAt := now.Add(5 * time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{UsedPercent: 50, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt},
		ObservedAt: nextObservedAt,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(calibration sample) = %v", err)
	}
	if units, err := engine.store.CompletedAccountUsageBetween("account-a", now, nextObservedAt); err != nil || units != 30 {
		t.Fatalf("CompletedAccountUsageBetween() = %d, %v; want 30 completed tokens", units, err)
	}
	debug, err := engine.QuotaDebug("managed", nextObservedAt)
	if err != nil {
		t.Fatalf("QuotaDebug() = %v", err)
	}
	if len(debug.Accounts) != 1 {
		t.Fatalf("debug accounts = %+v, want one account", debug.Accounts)
	}
	account := debug.Accounts[0]
	if account.CalibrationSource != "official_percent_delta" || account.CalibrationSamples != 1 || account.EstimatedTokensPerX != 60 {
		t.Fatalf("calibration diagnostic = %+v, want one 60-token/x official sample", account)
	}
	if account.KeyCapacity != officialXUnitsPerX {
		t.Fatalf("official x key capacity = %d, want 1x fixed point", account.KeyCapacity)
	}
	calibrations, err := engine.store.LoadQuotaCalibrations()
	if err != nil || len(calibrations) != 1 || calibrations[0].AuthID != "account-a" || calibrations[0].TokensPerX != 60 {
		t.Fatalf("persisted calibrations = %+v, err=%v; want account-a at 60 tokens/x", calibrations, err)
	}
}

func TestOfficialPercentJumpChargesKeyByCalibratedActualTokens(t *testing.T) {
	engine := newTestEngineWithRequestUnits(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true}, defaultRequestUnits)
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	seedQuotaCalibration(t, engine, "account-a", 20, 3_800_000, now, resetAt)

	requestedAt := now.Add(time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: "account-a"}}); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 3_020_000,
	})
	// A 12.5% movement on a configured 20x account represents 2.5 account x.
	// It may include traffic outside this plugin and must not become 2.5 Key x.
	updateOfficialUsagePercent(t, engine, "account-a", 12.5, now.Add(5*time.Minute), resetAt)

	calibration := engine.quotaCalibrationView("account-a", defaultRequestUnits)
	want := capacityForX(float64(3_020_000)/float64(calibration.TokensPerX), officialXUnitsPerX)
	allocation := engine.Summary(now.Add(5 * time.Minute)).Keys[0].Allocation
	if allocation.Completed != want || allocation.Completed < 450_000 || allocation.Completed > 550_000 {
		t.Fatalf("Token-derived allocation = %+v, calibration=%+v, want about 0.5x (%d), not the 2.5x account delta", allocation, calibration, want)
	}

	// A later poll with the same rounded percentage is a full-window refresh,
	// not another additive charge.
	updateOfficialUsagePercent(t, engine, "account-a", 12.5, now.Add(6*time.Minute), resetAt)
	if allocation = engine.Summary(now.Add(6 * time.Minute)).Keys[0].Allocation; allocation.Completed != want {
		t.Fatalf("unchanged-percent allocation = %+v, want idempotent full-window charge %d", allocation, want)
	}
}

func TestExplicitEarlyOfficialResetRetiresKeyWindowDurably(t *testing.T) {
	engine := newTestEngineWithRequestUnits(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true}, defaultRequestUnits)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	oldReset := now.Add(24 * time.Hour)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, oldReset)
	seedQuotaCalibration(t, engine, "account-a", 20, 3_000_000, now, oldReset)
	requestedAt := now.Add(time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: "account-a"}}); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 3_000_000,
	})
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(3*time.Minute), oldReset)
	if allocation := engine.Summary(now.Add(3 * time.Minute)).Keys[0].Allocation; allocation.Completed <= 0 {
		t.Fatalf("old-window allocation = %+v, want a completed charge before reset", allocation)
	}

	// The old reset is still in the future, but Codex explicitly advertises a
	// later identity and drops usage to zero. This is an early official reset.
	observedAt := now.Add(4 * time.Minute)
	newReset := oldReset.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, observedAt, newReset)
	if allocation := engine.Summary(observedAt).Keys[0].Allocation; allocation.Completed != 0 || allocation.Provisional != 0 || allocation.Reserved != 0 {
		t.Fatalf("new-window allocation = %+v, want the retired Key cycle cleared", allocation)
	}
	records, err := engine.store.LoadAllocationBuckets(observedAt)
	if err != nil {
		t.Fatalf("LoadAllocationBuckets() = %v", err)
	}
	for _, record := range records {
		if record.AuthID == "account-a" && record.WindowResetAt <= oldReset.UnixMilli() {
			t.Fatalf("retired allocation remained durable: %+v", record)
		}
	}

	config := engine.config
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(restart) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if allocation := reopened.Summary(observedAt).Keys[0].Allocation; allocation.Completed != 0 || allocation.Provisional != 0 || allocation.Reserved != 0 {
		t.Fatalf("restarted new-window allocation = %+v, retired cycle must not reload", allocation)
	}
}

func TestEstimatedEarlyOfficialResetRequiresConfirmationAndRetiresKeyWindow(t *testing.T) {
	engine := newTestEngineWithRequestUnits(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true}, defaultRequestUnits)
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	oldReset := now.Add(24 * time.Hour)
	updateOfficialUsagePercent(t, engine, "account-a", 40, now, oldReset)
	seedQuotaCalibration(t, engine, "account-a", 20, fallbackTokensPerX(defaultRequestUnits), now, oldReset)
	requestedAt := now.Add(time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: "account-a"}}); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 3_000_000,
	})
	updateOfficialUsagePercent(t, engine, "account-a", 45, now.Add(2*time.Minute), oldReset)
	if allocation := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation; allocation.Completed <= 0 {
		t.Fatalf("old-window allocation = %+v, want completed usage before reset", allocation)
	}

	// reset_after_seconds responses carry an estimated absolute time. A large
	// forward jump and percentage drop is tentative on the first observation.
	firstObservedAt := now.Add(3 * time.Minute)
	firstNewReset := firstObservedAt.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary: OfficialQuotaWindow{
			UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second),
			ResetAt: &firstNewReset, ResetEstimated: true,
		},
		ObservedAt: firstObservedAt,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(first inferred reset) = %v", err)
	}
	if allocation := engine.Summary(firstObservedAt).Keys[0].Allocation; allocation.Completed <= 0 {
		t.Fatalf("first inferred reset released allocation early: %+v", allocation)
	}

	secondObservedAt := firstObservedAt.Add(quotaResetStabilityTolerance)
	secondNewReset := secondObservedAt.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary: OfficialQuotaWindow{
			UsedPercent: 0, LimitWindowSeconds: int64(sevenDayWindow / time.Second),
			ResetAt: &secondNewReset, ResetEstimated: true,
		},
		ObservedAt: secondObservedAt,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(confirmed inferred reset) = %v", err)
	}
	if allocation := engine.Summary(secondObservedAt).Keys[0].Allocation; allocation.Completed != 0 || allocation.Provisional != 0 || allocation.Reserved != 0 {
		t.Fatalf("confirmed inferred reset allocation = %+v, want fresh Key cycle", allocation)
	}
	accounts := engine.AccountPool(secondObservedAt)
	if len(accounts) != 1 || accounts[0].Quota == nil || accounts[0].Quota.Secondary.ResetAt == nil ||
		!accounts[0].Quota.Secondary.ResetAt.Equal(secondNewReset) {
		t.Fatalf("confirmed inferred reset snapshot = %+v, want raw new identity %v", accounts, secondNewReset)
	}
}

func TestAlignedCalibrationMigrationClearsLegacyDerivedCacheOnlyOnce(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	seedQuotaCalibration(t, engine, "account-a", 20, 123, now, resetAt)
	if _, err := engine.store.db.Exec(`INSERT INTO key_account_allocation_buckets(
key_id, auth_id, window_reset_at, bucket_at, completed_units, provisional_units,
reserved_units, capacity_units, global_capacity_units, updated_at
) VALUES
('managed', 'account-a', ?, ?, 123, 456, 1, 1000, 1000, ?),
('empty', 'account-a', ?, ?, 0, 99, 0, 1000, 1000, ?)`,
		resetAt.UnixMilli(), now.UnixMilli(), now.UnixMilli(),
		resetAt.UnixMilli(), now.Add(time.Minute).UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatalf("seed provisional allocation buckets = %v", err)
	}
	if _, err := engine.store.db.Exec(`DELETE FROM plugin_metadata WHERE name = ?`, alignedQuotaCalibrationMetadataName); err != nil {
		t.Fatalf("remove migration marker = %v", err)
	}
	config := engine.config
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(migration) = %v", err)
	}
	calibrations, err := reopened.store.LoadQuotaCalibrations()
	if err != nil || len(calibrations) != 0 {
		t.Fatalf("legacy calibrations after migration = %+v, err=%v; want cleared derived cache", calibrations, err)
	}
	buckets, err := reopened.store.LoadAllocationBuckets(now)
	if err != nil || len(buckets) != 1 || buckets[0].CompletedUnits != 123 ||
		buckets[0].ProvisionalUnits != 0 || buckets[0].ReservedUnits != 1 {
		t.Fatalf("allocation buckets after migration = %+v, err=%v; want confirmed/reserved preserved and provisional cleared", buckets, err)
	}
	seedQuotaCalibration(t, reopened, "account-a", 20, 456, now.Add(time.Minute), resetAt)
	if err := reopened.Close(); err != nil {
		t.Fatalf("Close(reopened) = %v", err)
	}

	finalEngine, err := Open(config)
	if err != nil {
		t.Fatalf("Open(second restart) = %v", err)
	}
	defer func() { _ = finalEngine.Close() }()
	calibrations, err = finalEngine.store.LoadQuotaCalibrations()
	if err != nil || len(calibrations) != 1 || calibrations[0].TokensPerX != 456 {
		t.Fatalf("post-migration calibration = %+v, err=%v; want one-time migration", calibrations, err)
	}
}

func TestOfficialPercentDeltaSplitsXByManagedTokenShareAndLeavesUnmanagedShareUnassigned(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "first", Name: "First", AllocationX: 10, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "second", Name: "Second", AllocationX: 10, Enabled: true}, "second-key"); err != nil {
		t.Fatalf("UpsertPolicy(second) = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for _, item := range []struct {
		apiKey      string
		requestedAt time.Time
		tokens      int64
	}{
		{apiKey: "managed-key", requestedAt: now.Add(time.Minute), tokens: 75},
		{apiKey: "second-key", requestedAt: now.Add(2 * time.Minute), tokens: 25},
	} {
		if admission := engine.Admit(item.apiKey, "gpt-5", item.requestedAt, candidates); !admission.Allowed {
			t.Fatalf("Admit(%s) = %+v, want allowed", item.apiKey, admission)
		}
		engine.RecordUsage(CompletedUsage{
			APIKey: item.apiKey, AuthID: "account-a", Model: "gpt-5",
			RequestedAt: item.requestedAt, Generate: true, TotalTokens: item.tokens,
		})
	}
	// This request is deliberately outside Key management. It consumes half of
	// the official account delta and must stay in the denominator without being
	// charged to either managed Key.
	engine.RecordUsage(CompletedUsage{
		APIKey: "unmanaged-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: now.Add(3 * time.Minute), Generate: true, TotalTokens: 100,
	})
	updateOfficialUsagePercent(t, engine, "account-a", 10, now.Add(5*time.Minute), resetAt)

	byID := make(map[string]WindowSnapshot)
	for _, key := range engine.Summary(now.Add(5 * time.Minute)).Keys {
		byID[key.ID] = key.Allocation
	}
	if got := byID["first"].Completed; got != 750_000 {
		t.Fatalf("first confirmed x units = %d, want 750000 (0.75x)", got)
	}
	if got := byID["second"].Completed; got != 250_000 {
		t.Fatalf("second confirmed x units = %d, want 250000 (0.25x)", got)
	}
}

func TestOfficialPercentWatermarkWaitsForAMeasurableDeltaAndPersistsAcrossRestart(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	for index, requestedAt := range []time.Time{now.Add(time.Minute), now.Add(3 * time.Minute)} {
		if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
			t.Fatalf("Admit(%d) = %+v, want allowed", index, admission)
		}
		engine.RecordUsage(CompletedUsage{
			APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
			RequestedAt: requestedAt, Generate: true, TotalTokens: 10,
		})
		if index == 0 {
			// A rounded upstream percentage that has not moved refreshes the
			// current window from actual Key Tokens using the explicit fallback;
			// it must not apply another account-percentage charge.
			updateOfficialUsagePercent(t, engine, "account-a", 0, now.Add(2*time.Minute), resetAt)
			snapshot := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation
			wantCompleted := capacityForX(float64(10)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
			if snapshot.Completed != wantCompleted || snapshot.Provisional != 0 {
				t.Fatalf("unchanged-percent allocation = %+v, want Token-derived completed %d", snapshot, wantCompleted)
			}
		}
	}
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(5*time.Minute), resetAt)
	calibration := engine.quotaCalibrationView("account-a", engine.config.RequestUnits)
	if calibration.Source != "official_percent_delta" || calibration.TokensPerX != fallbackTokensPerX(engine.config.RequestUnits) {
		t.Fatalf("aligned calibration = %+v, want the product fallback floor because unobserved account traffic can only lower the sample", calibration)
	}
	wantCompleted := capacityForX(float64(20)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
	if snapshot := engine.Summary(now.Add(5 * time.Minute)).Keys[0].Allocation; snapshot.Completed != wantCompleted || snapshot.Provisional != 0 {
		t.Fatalf("confirmed allocation = %+v, want Token-derived %d with no provisional", snapshot, wantCompleted)
	}
	finalRequestedAt := now.Add(6 * time.Minute)
	if result := engine.Admit("managed-key", "gpt-5", finalRequestedAt, candidates); !result.Allowed {
		t.Fatalf("remaining Token-derived allocation admission = %+v, want allowed", result)
	}
	// Close is deliberately strict: every admitted request needs a terminal
	// callback. Settle this guard-only admission without changing Token usage.
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: finalRequestedAt, Generate: true, Failed: true,
	})

	config := engine.config
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(restart) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if snapshot := reopened.Summary(now.Add(6 * time.Minute)).Keys[0].Allocation; snapshot.Completed != wantCompleted {
		t.Fatalf("restarted allocation = %+v, want durable Token-derived %d", snapshot, wantCompleted)
	}
	reopenedRequestedAt := now.Add(6*time.Minute + time.Second)
	if result := reopened.Admit("managed-key", "gpt-5", reopenedRequestedAt, candidates); !result.Allowed {
		t.Fatalf("restarted remaining allocation admission = %+v, want allowed", result)
	}
	reopened.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: reopenedRequestedAt, Generate: true, Failed: true,
	})
}

func TestProvisionalXPersistsAndIsReplacedByTokenDerivedWindowCharge(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	seedQuotaCalibration(t, engine, "account-a", 20, fallbackTokensPerX(engine.config.RequestUnits), now, resetAt)
	requestedAt := now.Add(time.Minute)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	tokensPerX := fallbackTokensPerX(engine.config.RequestUnits)
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: tokensPerX,
	})
	if err := engine.flushAllocationPersistence(); err != nil {
		t.Fatalf("flushAllocationPersistence() = %v", err)
	}
	allocation := engine.Summary(requestedAt).Keys[0].Allocation
	wantProvisional := engine.provisionalXLimit("account-a")
	if allocation.Completed != 0 || allocation.Provisional != wantProvisional ||
		allocation.Reserved != 0 || allocation.Used != wantProvisional {
		t.Fatalf("provisional allocation = %+v, want bounded provisional %d", allocation, wantProvisional)
	}
	secondRequestedAt := requestedAt.Add(time.Second)
	if admission := engine.Admit("managed-key", "gpt-5", secondRequestedAt, candidates); !admission.Allowed {
		t.Fatalf("provisional guard admission = %+v, bounded estimate must not exhaust 1x before official poll", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: secondRequestedAt, Generate: true, Failed: true,
	})

	config := engine.config
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(restart) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	allocation = reopened.Summary(requestedAt).Keys[0].Allocation
	if allocation.Completed != 0 || allocation.Provisional != wantProvisional || allocation.Used != wantProvisional {
		t.Fatalf("restarted provisional allocation = %+v, want durable bounded provisional %d", allocation, wantProvisional)
	}

	updateOfficialUsagePercent(t, reopened, "account-a", 5, now.Add(5*time.Minute), resetAt)
	allocation = reopened.Summary(now.Add(5 * time.Minute)).Keys[0].Allocation
	if allocation.Completed != officialXUnitsPerX || allocation.Provisional != 0 ||
		allocation.Used != officialXUnitsPerX {
		t.Fatalf("reconciled allocation = %+v, want provisional x atomically replaced by the Key's 1x Token charge", allocation)
	}
	records, err := reopened.store.LoadAllocationBuckets(now)
	if err != nil {
		t.Fatalf("LoadAllocationBuckets() = %v", err)
	}
	var completed, provisional int64
	for _, record := range records {
		if record.KeyID == "managed" && record.AuthID == "account-a" {
			completed += record.CompletedUnits
			provisional += record.ProvisionalUnits
		}
	}
	if completed != officialXUnitsPerX || provisional != 0 {
		t.Fatalf("durable reconciled allocation = completed:%d provisional:%d, want Token-derived:%d provisional:0", completed, provisional, officialXUnitsPerX)
	}
}

func TestDiagnosticCalibrationBoundsCurrentMeteredXBeforeOfficialPoll(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)

	// Reproduce the operator diagnostic: even when a stale calibration converts
	// one completion to 5x, the temporary guard may cover only the account's
	// smallest observable official percentage step (0.2x for a 20x account).
	const tokensPerX int64 = 5_630_766
	seedQuotaCalibration(t, engine, "account-a", 20, tokensPerX, now, now.Add(sevenDayWindow))
	requestedAt := now.Add(time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 5 * tokensPerX,
	})

	allocation := engine.Summary(requestedAt).Keys[0].Allocation
	wantProvisional := engine.provisionalXLimit("account-a")
	if allocation.Completed != 0 || allocation.Provisional != wantProvisional ||
		allocation.Used != wantProvisional || allocation.Remaining != 5*officialXUnitsPerX-wantProvisional {
		t.Fatalf("diagnostic allocation = %+v, want bounded provisional %d", allocation, wantProvisional)
	}
	secondRequestedAt := requestedAt.Add(time.Second)
	if admission := engine.Admit("managed-key", "gpt-5", secondRequestedAt, candidates); !admission.Allowed {
		t.Fatalf("post-diagnostic admission = %+v, bounded estimate must not exhaust 5x before official poll", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: secondRequestedAt, Generate: true, TotalTokens: 5 * tokensPerX,
	})
	if allocation = engine.Summary(secondRequestedAt).Keys[0].Allocation; allocation.Provisional != wantProvisional {
		t.Fatalf("second provisional allocation = %+v, interval cap must remain %d", allocation, wantProvisional)
	}
}

func TestOfficialDeltaCannotReleaseProvisionalXWithoutDurableTokenEvidence(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	seedQuotaCalibration(t, engine, "account-a", 20, 10, now, resetAt)
	requestedAt := now.Add(time.Minute)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	// Model a terminal allocation mutation whose analysis event could not be
	// enqueued. The official poll must not erase this conservative guard.
	provisionalUnits := engine.provisionalXUnits("account-a", 10, engine.config.RequestUnits)
	settlement := engine.settleAccountAllocation(
		"managed", "account-a", usageBucketEnd(requestedAt), requestedAt,
		admissionReservationUnits, provisionalUnits, engine.provisionalXLimit("account-a"),
	)
	engine.finishPendingSettlement(settlement.Key, settlement.Matched)
	if !settlement.Matched {
		t.Fatalf("settleAccountAllocation() = %+v, want matched provisional guard", settlement)
	}
	if err := engine.flushAllocationPersistence(); err != nil {
		t.Fatalf("flushAllocationPersistence() = %v", err)
	}
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary: OfficialQuotaWindow{
			UsedPercent: 5, LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &resetAt,
		},
		ObservedAt: now.Add(5 * time.Minute),
	}); err == nil {
		t.Fatal("UpdateOfficialQuota() error = nil, want missing-attribution rejection")
	}
	allocation := engine.Summary(now.Add(5 * time.Minute)).Keys[0].Allocation
	wantProvisional := engine.provisionalXLimit("account-a")
	if allocation.Completed != 0 || allocation.Provisional != wantProvisional {
		t.Fatalf("allocation after rejected reconciliation = %+v, want retained provisional %d", allocation, wantProvisional)
	}
	state, found, err := engine.store.LoadOfficialXReconciliationState("account-a")
	if err != nil || !found || state.UsedPercent != 0 {
		t.Fatalf("official watermark = %+v, found=%t, err=%v; want retained 0%% baseline", state, found, err)
	}
}

func TestOfficialObservationDoesNotClearLaterCompletionFromTheSameMinute(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 2, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	seedQuotaCalibration(t, engine, "account-a", 20, 10, now, resetAt)
	// Refresh the published snapshot without moving the durable percentage
	// watermark, so the later request remains eligible.
	updateOfficialUsagePercent(t, engine, "account-a", 0, now.Add(time.Minute), resetAt)
	requestedAt := now.Add(time.Minute + 50*time.Second)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 10,
	})
	provisionalUnits := engine.provisionalXLimit("account-a")
	// Model an official response observed ten seconds before the completion.
	// Even though it is delivered to the plugin afterwards, it cannot include
	// that completion and therefore must not clear its provisional x.
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(time.Minute+40*time.Second), resetAt)
	allocation := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation
	if allocation.Completed != 0 || allocation.Provisional != provisionalUnits {
		t.Fatalf("same-minute earlier observation allocation = %+v, want retained provisional %d", allocation, provisionalUnits)
	}
	updateOfficialUsagePercent(t, engine, "account-a", 10, now.Add(3*time.Minute), resetAt)
	allocation = engine.Summary(now.Add(3 * time.Minute)).Keys[0].Allocation
	wantCompleted := capacityForX(float64(10)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
	if allocation.Completed != wantCompleted || allocation.Provisional != 0 {
		t.Fatalf("later observation allocation = %+v, want Token-derived %d and no provisional", allocation, wantCompleted)
	}
}

func TestOfficialPercentWatermarkWaitsForPendingTerminalUsage(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 0, now, resetAt)
	requestedAt := now.Add(time.Minute)
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if admission := engine.Admit("managed-key", "gpt-5", requestedAt, candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}

	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(2*time.Minute), resetAt)
	state, found, err := engine.store.LoadOfficialXReconciliationState("account-a")
	if err != nil || !found || state.UsedPercent != 0 {
		t.Fatalf("pending reconciliation state = %+v, found=%t, err=%v; want original 0%% watermark", state, found, err)
	}
	if snapshot := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation; snapshot.Completed != 0 {
		t.Fatalf("pending terminal allocation = %+v, want no unattributed x charge", snapshot)
	}

	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: requestedAt, Generate: true, TotalTokens: 100,
	})
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(3*time.Minute), resetAt)
	if snapshot := engine.Summary(now.Add(3 * time.Minute)).Keys[0].Allocation; snapshot.Completed != officialXUnitsPerX {
		t.Fatalf("completed reconciliation allocation = %+v, want delayed 1x charge", snapshot)
	}
}

func TestUncalibratedTokensDoNotConsumeProvisionalX(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if admission := engine.Admit("managed-key", "gpt-5", now.Add(time.Minute), candidates); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: now.Add(time.Minute), Generate: true, TotalTokens: 100_000_000,
	})
	allocation := engine.Summary(now.Add(time.Minute)).Keys[0].Allocation
	if allocation.Completed != 0 || allocation.Provisional != 0 || allocation.Reserved != 0 || allocation.Used != 0 {
		t.Fatalf("uncalibrated allocation = %+v, fallback estimate must not consume x", allocation)
	}
}

func TestCalibrationWithDifferentAccountCapacityDoesNotConsumeProvisionalX(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	// The pool contains account-a as 20x. A calibration learned while it was
	// configured as 1x must not be reused after that operator edit.
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	seedQuotaCalibration(t, engine, "account-a", 1, 10, now, now.Add(sevenDayWindow))
	if got := engine.provisionalXUnits("account-a", 100_000_000, engine.config.RequestUnits); got != 0 {
		t.Fatalf("provisionalXUnits() = %d, want 0 for a stale account-capacity calibration", got)
	}
}

func TestActiveKeyAllocationCanIncreaseAndDeferDecreaseToNextOfficialWindow(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)
	if admission := engine.Admit("managed-key", "gpt-5", now, candidates); !admission.Allowed {
		t.Fatalf("initial Admit() = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
	resetAt := now.Add(sevenDayWindow)
	updateOfficialUsagePercent(t, engine, "account-a", 5, now.Add(2*time.Minute), resetAt)

	policy := engine.Policies()[0]
	policy.AllocationX = 5
	if _, err := engine.UpsertPolicy(policy, ""); err != nil {
		t.Fatalf("UpsertPolicy(increase active allocation) = %v", err)
	}
	firstCompleted := capacityForX(float64(1)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
	if snapshot := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation; snapshot.Capacity != 5*officialXUnitsPerX || snapshot.Completed != firstCompleted || snapshot.Used != firstCompleted {
		t.Fatalf("increased active allocation snapshot = %+v", snapshot)
	}

	next := now.Add(3 * time.Minute)
	if admission := engine.Admit("managed-key", "gpt-5", next, candidates); !admission.Allowed {
		t.Fatalf("Admit() after active increase = %+v, want allowed", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: next, Generate: true, TotalTokens: 1})
	updateOfficialUsagePercent(t, engine, "account-a", 10, now.Add(5*time.Minute), resetAt)
	if err := engine.flushAllocationPersistence(); err != nil {
		t.Fatalf("flushAllocationPersistence() = %v", err)
	}
	records, err := engine.store.LoadAllocationBuckets(now)
	if err != nil || len(records) == 0 {
		t.Fatalf("LoadAllocationBuckets() = %+v, err=%v", records, err)
	}
	maxCapacity := int64(0)
	completed := int64(0)
	for _, record := range records {
		if record.CapacityUnits > maxCapacity {
			maxCapacity = record.CapacityUnits
		}
		completed += record.CompletedUnits
	}
	if maxCapacity != 5*officialXUnitsPerX {
		t.Fatalf("increased durable capacity maximum = %d, want %d", maxCapacity, 5*officialXUnitsPerX)
	}
	// Reconciliation is a full-window replacement, so multiple request buckets
	// may be compacted into one durable Token-derived charge.
	secondCompleted := capacityForX(float64(2)/float64(fallbackTokensPerX(engine.config.RequestUnits)), officialXUnitsPerX)
	if completed != secondCompleted {
		t.Fatalf("increased durable completed usage = %d, want Token-derived %d", completed, secondCompleted)
	}

	policy.AllocationX = 1
	if _, err := engine.UpsertPolicy(policy, ""); err != nil {
		t.Fatalf("UpsertPolicy(decrease active allocation) = %v", err)
	}
	if snapshot := engine.Summary(now.Add(5 * time.Minute)).Keys[0].Allocation; snapshot.Capacity != 5*officialXUnitsPerX || snapshot.PolicyCapacity != officialXUnitsPerX || !snapshot.HasDeferredDecrease || snapshot.Completed != secondCompleted {
		t.Fatalf("decreased active allocation snapshot = %+v, want preserved 5x capacity and usage", snapshot)
	}
	if saved := engine.Policies()[0]; saved.AllocationX != 1 {
		t.Fatalf("saved reduced allocation = %.2f, want 1", saved.AllocationX)
	}
}

func TestAllocationDecreaseKeepsOverageUntilTheNextOfficialWindow(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	if _, err := engine.UpsertAccountPoolEntry(AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 20, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	firstReset := now.Add(30 * time.Minute)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &firstReset},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(first) = %v", err)
	}
	candidates := []SchedulerCandidate{{AuthID: "account-a"}}
	if admission := engine.Admit("managed-key", "gpt-5", now, candidates); !admission.Allowed {
		t.Fatalf("initial Admit() = %+v, want allowed", admission)
	}
	// The account moves by 10% of its configured 20x capacity, so this Key owns
	// 2x in the current official window. That confirmed usage remains auditable
	// after the policy is reduced below it.
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 100})
	updateOfficialUsagePercent(t, engine, "account-a", 10, now.Add(2*time.Minute), firstReset)
	if _, err := engine.UpsertPolicy(KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true}, ""); err != nil {
		t.Fatalf("UpsertPolicy(decrease) = %v", err)
	}
	if snapshot := engine.Summary(now.Add(2 * time.Minute)).Keys[0].Allocation; snapshot.Capacity != 5*officialXUnitsPerX || snapshot.Completed != 2*officialXUnitsPerX {
		t.Fatalf("current window after decrease = %+v, want preserved 5x capacity and overage", snapshot)
	}

	nextNow := firstReset.Add(time.Minute)
	nextReset := nextNow.Add(sevenDayWindow)
	if err := engine.UpdateOfficialQuota(OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  OfficialQuotaWindow{LimitWindowSeconds: int64(sevenDayWindow / time.Second), ResetAt: &nextReset},
		ObservedAt: nextNow,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(next) = %v", err)
	}
	if admission := engine.Admit("managed-key", "gpt-5", nextNow, candidates); !admission.Allowed {
		t.Fatalf("next-window Admit() = %+v, want allowed", admission)
	}
	if snapshot := engine.Summary(nextNow).Keys[0].Allocation; snapshot.Capacity != officialXUnitsPerX {
		t.Fatalf("next window allocation = %+v, want new 1x capacity", snapshot)
	}
}

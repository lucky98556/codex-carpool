//go:build linux && cgo

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"codex-carpool/internal/quota"
)

func TestCodexPlanCapacityUsesCPAAndIDTokenMetadata(t *testing.T) {
	for _, planType := range []string{"pro", "prolite", "pro-lite"} {
		if capacity := codexPlanCapacityX(planType); capacity != 20 {
			t.Fatalf("%q capacity = %v, want 20x", planType, capacity)
		}
	}
	for _, planType := range []string{"plus", "free", "", "unknown"} {
		if capacity := codexPlanCapacityX(planType); capacity != 1 {
			t.Fatalf("%q capacity = %v, want conservative 1x", planType, capacity)
		}
	}

	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"pro"}}`))
	file := codexAuthFile{IDToken: "header." + payload + ".signature"}
	if planType := file.planType(); planType != "pro" {
		t.Fatalf("ID-token plan type = %q, want pro", planType)
	}
	file.PlanType = "plus"
	if planType := file.planType(); planType != "plus" {
		t.Fatalf("CPA plan type = %q, want top-level plus", planType)
	}
}

func TestParseOfficialQuotaTreatsMissingAllowedAsUsable(t *testing.T) {
	snapshot, err := parseOfficialQuota([]byte(`{
  "plan_type": "plus",
  "rate_limit": {
    "primary_window": {"used_percent": 12.5, "limit_window_seconds": 18000, "reset_after_seconds": 60},
    "secondary_window": {"used_percent": 20, "limit_window_seconds": 604800, "reset_after_seconds": 120}
  }
}`))
	if err != nil {
		t.Fatalf("parseOfficialQuota() error = %v", err)
	}
	if !snapshot.Allowed || snapshot.LimitReached {
		t.Fatalf("snapshot eligibility = allowed:%v limit_reached:%v, want usable", snapshot.Allowed, snapshot.LimitReached)
	}
	if snapshot.Primary.LimitWindowSeconds != 0 || snapshot.Secondary.LimitWindowSeconds != 604800 || snapshot.Secondary.UsedPercent != 12.5 {
		t.Fatalf("current weekly snapshot = primary:%+v secondary:%+v, want primary's weekly allowance", snapshot.Primary, snapshot.Secondary)
	}
	if snapshot.Secondary.ResetAt != nil || snapshot.Secondary.ResetEstimated {
		t.Fatalf("retired primary fallback must not create a short reset: %+v", snapshot.Secondary)
	}
}

func TestParseOfficialQuotaAllowsWeeklyWindowWithoutPrimaryWindow(t *testing.T) {
	snapshot, err := parseOfficialQuota([]byte(`{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "secondary_window": {"used_percent": 36, "limit_window_seconds": 604800, "reset_after_seconds": 120}
  }
}`))
	if err != nil {
		t.Fatalf("parseOfficialQuota() error = %v", err)
	}
	if snapshot.Primary.LimitWindowSeconds != 0 || snapshot.Secondary.LimitWindowSeconds != 604800 {
		t.Fatalf("weekly-only snapshot windows = primary:%+v secondary:%+v", snapshot.Primary, snapshot.Secondary)
	}
	if !snapshot.Allowed || snapshot.LimitReached {
		t.Fatalf("weekly-only snapshot eligibility = allowed:%v limit_reached:%v", snapshot.Allowed, snapshot.LimitReached)
	}
}

func TestParseOfficialQuotaLimitReachedOverridesAllowed(t *testing.T) {
	snapshot, err := parseOfficialQuota([]byte(`{
  "rate_limit": {
    "allowed": true,
    "limit_reached": true,
    "primary_window": {"used_percent": 100, "limit_window_seconds": 18000},
    "secondary_window": {"used_percent": 20, "limit_window_seconds": 604800}
  }
}`))
	if err != nil {
		t.Fatalf("parseOfficialQuota() error = %v", err)
	}
	if snapshot.Allowed || !snapshot.LimitReached {
		t.Fatalf("snapshot eligibility = allowed:%v limit_reached:%v, want exhausted", snapshot.Allowed, snapshot.LimitReached)
	}
}

func TestChatGPTAccountIDReadsCodexJWTClaim(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-from-jwt"}}`))
	if got := chatGPTAccountID("header." + payload + ".signature"); got != "account-from-jwt" {
		t.Fatalf("chatGPTAccountID() = %q, want nested Codex account ID", got)
	}
}

func TestOfficialQuotaExhaustionDoesNotTreatEvery429AsPoolExhaustion(t *testing.T) {
	if isOfficialQuotaExhaustion(`{"error":{"type":"rate_limit_error"}}`) {
		t.Fatal("temporary rate limit was classified as an exhausted official quota")
	}
	if !isOfficialQuotaExhaustion(`{"error":{"type":"usage_limit_reached"}}`) {
		t.Fatal("Codex usage_limit_reached was not classified as an exhausted official quota")
	}
	if !isOfficialQuotaExhaustion(`{"body":{"error":{"type":"usage_limit_reached"}}}`) {
		t.Fatal("nested Codex usage_limit_reached was not classified as an exhausted official quota")
	}
}

func TestSafeOperationalErrorHidesCredentialMaterial(t *testing.T) {
	message := safeOperationalError(fmt.Errorf("request failed: Bearer secret-token"))
	if strings.Contains(message, "secret-token") || !strings.Contains(message, "已隐藏") {
		t.Fatalf("safeOperationalError() = %q, want redacted message", message)
	}
}

func TestAdmissionStatusCodeKeepsPersistenceFailureOutOfQuota429(t *testing.T) {
	if got := admissionStatusCode("quota_persistence_unavailable"); got != http.StatusServiceUnavailable {
		t.Fatalf("admissionStatusCode(quota_persistence_unavailable) = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := admissionStatusCode("quota_allocation_exhausted"); got != http.StatusTooManyRequests {
		t.Fatalf("admissionStatusCode(quota_allocation_exhausted) = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestQuotaSynchronizerAppliesPerAccountRefreshCooldown(t *testing.T) {
	syncer := newQuotaSynchronizer(nil)
	if !syncer.startRefresh("account-a", false) {
		t.Fatal("first scheduled refresh was unexpectedly throttled")
	}
	syncer.finishRefresh("account-a", true)
	if syncer.startRefresh("account-a", false) {
		t.Fatal("scheduled refresh bypassed the successful refresh cooldown")
	}
	if !syncer.startRefresh("account-a", true) {
		t.Fatal("explicit administrator refresh was throttled")
	}
	syncer.finishRefresh("account-a", false)
	if syncer.startRefresh("account-a", false) {
		t.Fatal("failed refresh retry bypassed exponential backoff")
	}
}

func TestQuotaSynchronizerWaitsForFirstUsableSnapshot(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	defer syncer.Close()
	go func() {
		time.Sleep(20 * time.Millisecond)
		now := time.Now().UTC()
		resetAt := now.Add(7 * 24 * time.Hour)
		_ = engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{
			AuthID: "account-a", Allowed: true, ObservedAt: now,
			Secondary: quota.OfficialQuotaWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAt: &resetAt},
		})
	}()
	readiness := syncer.WaitForUsableSnapshot([]string{"account-a"}, time.Second)
	if len(readiness.Ready) != 1 || readiness.Ready[0] != "account-a" || len(readiness.Pending) != 0 || len(readiness.Errors) != 0 {
		t.Fatalf("WaitForUsableSnapshot() = %+v, want one ready account", readiness)
	}
}

func TestQuotaSynchronizerReportsSnapshotErrorWithoutWaitingForTimeout(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	if err := engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{AuthID: "account-a", ObservedAt: time.Now().UTC(), LastError: "官方额度请求返回 HTTP 401"}); err != nil {
		t.Fatalf("UpdateOfficialQuota() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	defer syncer.Close()
	readiness := syncer.WaitForUsableSnapshot([]string{"account-a"}, time.Second)
	if got := readiness.Errors["account-a"]; got != "官方额度请求返回 HTTP 401" || len(readiness.Pending) != 0 || len(readiness.Ready) != 0 {
		t.Fatalf("WaitForUsableSnapshot() = %+v, want immediate stored error", readiness)
	}
}

func TestQuotaSynchronizerDoesNotTreatPreexistingSnapshotAsRefreshed(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	now := time.Now().UTC()
	resetAt := now.Add(7 * 24 * time.Hour)
	if err := engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true, ObservedAt: now,
		Secondary: quota.OfficialQuotaWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAt: &resetAt},
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	defer syncer.Close()
	readiness := syncer.WaitForRefreshedUsableSnapshot([]string{"account-a"}, time.Now().UTC(), 30*time.Millisecond)
	if len(readiness.Ready) != 0 || len(readiness.Pending) != 1 || readiness.Pending[0] != "account-a" {
		t.Fatalf("WaitForRefreshedUsableSnapshot() = %+v, must not accept a preexisting snapshot", readiness)
	}
}

func TestQuotaSynchronizerSchedulesEstimatedResetConfirmation(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}

	observedAt := time.Now().UTC().Truncate(time.Minute)
	previousResetAt := observedAt.Add(time.Minute)
	if err := engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  quota.OfficialQuotaWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAt: &previousResetAt, ResetEstimated: true},
		ObservedAt: observedAt,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(initial) error = %v", err)
	}
	firstPostReset := previousResetAt.Add(time.Minute)
	if err := engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  quota.OfficialQuotaWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second)},
		ObservedAt: firstPostReset,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota(candidate) error = %v", err)
	}
	confirmationAt, pending := engine.PendingEstimatedResetConfirmationAt("account-a")
	if !pending {
		t.Fatal("estimated reset candidate was not pending")
	}

	syncer := newQuotaSynchronizer(engine)
	defer syncer.Close()
	syncer.finishRefresh("account-a", true)
	syncer.mu.Lock()
	nextRefreshAt := syncer.nextRefreshAt["account-a"]
	syncer.mu.Unlock()
	if !nextRefreshAt.Equal(confirmationAt) {
		t.Fatalf("next refresh = %v, want estimated reset confirmation at %v", nextRefreshAt, confirmationAt)
	}
}

func TestQuotaSynchronizerAppliesGlobalManualRefreshCooldown(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	syncer := newQuotaSynchronizer(engine)
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	if scheduled, retryAfter := syncer.RequestManualRefresh([]string{"account-a"}); scheduled.Scheduled != 1 || retryAfter != 0 {
		t.Fatalf("first manual refresh = %+v retry:%v, want one scheduled job", scheduled, retryAfter)
	}
	if scheduled, retryAfter := syncer.RequestManualRefresh([]string{"account-a"}); scheduled.Scheduled != 0 || retryAfter <= 0 {
		t.Fatalf("second manual refresh = %+v retry:%v, want cooldown", scheduled, retryAfter)
	}
}

func TestQuotaSynchronizerManualRefreshReportsNoSchedulableAccount(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	syncer := newQuotaSynchronizer(engine)
	scheduled, retryAfter := syncer.RequestManualRefresh([]string{"missing"})
	if retryAfter != 0 || scheduled.Scheduled != 0 || scheduled.SkippedUnavailable != 1 {
		t.Fatalf("manual refresh result = %+v retry:%v, want unscheduled missing account", scheduled, retryAfter)
	}
}

func TestQuotaSynchronizerManualRefreshReportsQueueFullWithoutCooldown(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	// An unbuffered queue with no worker makes the non-blocking schedule path
	// deterministic without performing any network or auth-file I/O.
	syncer.jobs = make(chan string)
	scheduled, retryAfter := syncer.RequestManualRefresh([]string{"account-a"})
	if retryAfter != 0 || scheduled.Scheduled != 0 || scheduled.QueueFull != 1 {
		t.Fatalf("queue-full manual refresh = %+v retry:%v", scheduled, retryAfter)
	}
	syncer.mu.Lock()
	cooldownSet := !syncer.lastManualRefreshAt.IsZero()
	syncer.mu.Unlock()
	if cooldownSet {
		t.Fatal("queue-full refresh unexpectedly consumed the manual cooldown")
	}
}

func TestSourceIdentityCacheReusesOnlyUnchangedFileVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"token","account_id":"first"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	syncer := newQuotaSynchronizer(nil)
	calls := 0
	syncer.readSourceIdentity = func(_ string) (string, error) {
		calls++
		return "first", nil
	}
	for index := 0; index < 2; index++ {
		identity, missing, err := syncer.sourceIdentity(path, info)
		if err != nil || missing || identity != "first" {
			t.Fatalf("sourceIdentity() = %q missing:%v err:%v", identity, missing, err)
		}
	}
	if calls != 1 {
		t.Fatalf("unchanged source parsed %d times, want one", calls)
	}
	firstModifiedAt := info.ModTime()
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"token","account_id":"other"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(changed) error = %v", err)
	}
	// Preserve size and mtime to cover the cache-bypass edge case. Content
	// identity, rather than filesystem ctime resolution, must invalidate it.
	if err := os.Chtimes(path, firstModifiedAt, firstModifiedAt); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	changedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(changed) error = %v", err)
	}
	if changedInfo.Size() != info.Size() || !changedInfo.ModTime().Equal(firstModifiedAt) {
		t.Fatalf("test precondition changed size:%d/%d mtime:%s/%s", changedInfo.Size(), info.Size(), changedInfo.ModTime(), firstModifiedAt)
	}
	syncer.readSourceIdentity = func(_ string) (string, error) {
		calls++
		return "other", nil
	}
	identity, missing, err := syncer.sourceIdentity(path, changedInfo)
	if err != nil || missing || identity != "other" {
		t.Fatalf("sourceIdentity(changed) = %q missing:%v err:%v", identity, missing, err)
	}
	if calls != 2 {
		t.Fatalf("same-size same-mtime changed source parsed %d times, want two", calls)
	}
}

func TestCachedCredentialRejectsSameSizeSameMTimeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(path, []byte("first-cache"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	syncer := newQuotaSynchronizer(nil)
	syncer.cacheCredential("account.json", "opaque-cache-token", "first-account", path, info)
	firstModifiedAt := info.ModTime()
	if err := os.WriteFile(path, []byte("other-cache"), 0o600); err != nil {
		t.Fatalf("WriteFile(changed) error = %v", err)
	}
	if err := os.Chtimes(path, firstModifiedAt, firstModifiedAt); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	changedInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(changed) error = %v", err)
	}
	if changedInfo.Size() != info.Size() || !changedInfo.ModTime().Equal(firstModifiedAt) {
		t.Fatalf("test precondition changed size:%d/%d mtime:%s/%s", changedInfo.Size(), info.Size(), changedInfo.ModTime(), firstModifiedAt)
	}
	if _, ok := syncer.freshCachedCredential("account.json", path, changedInfo, time.Now().UTC()); ok {
		t.Fatal("same-size same-mtime changed credential unexpectedly reused the old OAuth cache")
	}
}

func TestRejectRetiredPolicyFields(t *testing.T) {
	if err := rejectRetiredPolicyFields([]byte(`{"policy":{"allocation_x":1}}`)); err != nil {
		t.Fatalf("allocation_x payload rejected: %v", err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"policy":{"group_id":"legacy"}}`),
		[]byte(`{"policy":{"five_hour_percent":10}}`),
		[]byte(`{"policy":{"sevenDayPercent":10}}`),
		[]byte(`{"policy":{"five_hour_multiplier":1}}`),
		[]byte(`{"policy":{"sevenDayMultiplier":1}}`),
		[]byte(`{"policy":{"max_concurrency":1}}`),
	} {
		if err := rejectRetiredPolicyFields(raw); err == nil {
			t.Fatalf("retired policy payload %s was accepted", raw)
		}
	}
}

func TestQuotaSynchronizerShutdownUsesOnlyCachedCredential(t *testing.T) {
	root := t.TempDir()
	authID := filepath.ToSlash(filepath.Join("codex", "account.json"))
	path := filepath.Join(root, filepath.FromSlash(authID))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"access-token","account_id":"chatgpt-account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	syncer := newQuotaSynchronizer(engine)
	accessToken, accountID, err := syncer.resolveCredential(authID)
	if err != nil {
		t.Fatalf("resolveCredential() from auth file: %v", err)
	}
	if accessToken != "access-token" || accountID != "chatgpt-account" {
		t.Fatalf("file credential = %q, %q", accessToken, accountID)
	}
	syncer.EnterShutdownMode()
	accessToken, accountID, err = syncer.resolveCredential(authID)
	if err != nil {
		t.Fatalf("resolveCredential() during shutdown: %v", err)
	}
	if accessToken != "access-token" || accountID != "chatgpt-account" {
		t.Fatalf("shutdown credential = %q, %q", accessToken, accountID)
	}
	if _, _, err := syncer.resolveCredential("codex/missing.json"); err == nil {
		t.Fatal("missing shutdown cache unexpectedly accepted a credential")
	}
}

func TestCodexAuthFilesDiscoverAndReloadByCPAIdentity(t *testing.T) {
	root := t.TempDir()
	authID := filepath.ToSlash(filepath.Join("nested", "primary.json"))
	path := filepath.Join(root, filepath.FromSlash(authID))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","label":"主账号","plan_type":"pro","access_token":"token-one","account_id":"account-one"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "disabled.json"), []byte(`{"type":"codex","disabled":true,"access_token":"ignored"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() disabled error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.json"), []byte(`{"type":"gemini","access_token":"ignored"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() other error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	runtime.mu.Lock()
	previous := runtime.engine
	runtime.engine = engine
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.engine = previous
		runtime.mu.Unlock()
	}()
	accounts, err := discoverCodexAccounts()
	if err != nil {
		t.Fatalf("discoverCodexAccounts() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].AuthID != authID || accounts[0].Name != "主账号" {
		t.Fatalf("discoverCodexAccounts() = %+v", accounts)
	}
	if accounts[0].CapacityX != 20 {
		t.Fatalf("discovered Pro account capacity = %v, want 20x", accounts[0].CapacityX)
	}
	syncer := newQuotaSynchronizer(engine)
	token, accountID, err := syncer.resolveCredential(authID)
	if err != nil || token != "token-one" || accountID != "account-one" {
		t.Fatalf("first credential = %q, %q, %v", token, accountID, err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","label":"主账号","access_token":"token-two","account_id":"account-two"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() rotated token error = %v", err)
	}
	rotatedAt := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, rotatedAt, rotatedAt); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	token, accountID, err = syncer.resolveCredential(authID)
	if err != nil || token != "token-two" || accountID != "account-two" {
		t.Fatalf("rotated credential = %q, %q, %v", token, accountID, err)
	}
}

func TestResolveCodexAuthFileRejectsPathOutsideDirectory(t *testing.T) {
	if _, _, err := resolveCodexAuthFile(t.TempDir(), "../outside.json"); err == nil {
		t.Fatal("resolveCodexAuthFile() accepted a path outside auth-dir")
	}
}

func TestCodexAuthFileSupportsCPAGenericTokenObject(t *testing.T) {
	root := t.TempDir()
	authID := "codex/generic.json"
	path := filepath.Join(root, filepath.FromSlash(authID))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","token":{"access_token":"generic-access","id_token":"generic-id","account_id":"generic-account"}}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	credential, err := readCodexQuotaCredential(path)
	if err != nil || credential.accessToken != "generic-access" || credential.accountID != "generic-account" {
		t.Fatalf("readCodexQuotaCredential() = %+v, %v", credential, err)
	}
}

func TestCodexAuthDiscoveryAllowsOnlyInternalJSONSymlink(t *testing.T) {
	root := t.TempDir()
	targetDir := filepath.Join(root, "private")
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	target := filepath.Join(targetDir, "credential.data")
	if err := os.WriteFile(target, []byte(`{"type":"codex","label":"软链接账号","access_token":"symlink-token","account_id":"symlink-account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	aliasDir := filepath.Join(root, "aliases")
	if err := os.MkdirAll(aliasDir, 0o700); err != nil {
		t.Fatalf("MkdirAll() alias error = %v", err)
	}
	alias := filepath.Join(aliasDir, "account.json")
	if err := os.Symlink(filepath.Join("..", "private", "credential.data"), alias); err != nil {
		t.Fatalf("Symlink() internal target error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	runtime.mu.Lock()
	previous := runtime.engine
	runtime.engine = engine
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.engine = previous
		runtime.mu.Unlock()
	}()
	accounts, err := discoverCodexAccounts()
	if err != nil || len(accounts) != 1 || accounts[0].AuthID != "aliases/account.json" {
		t.Fatalf("discoverCodexAccounts() = %+v, %v", accounts, err)
	}
	if _, _, err := resolveCodexAuthFile(root, "aliases/account.json"); err != nil {
		t.Fatalf("resolveCodexAuthFile() rejected internal symlink: %v", err)
	}

	external := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(external, []byte(`{"type":"codex","access_token":"external"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() external error = %v", err)
	}
	if err := os.Symlink(external, filepath.Join(root, "escaped.json")); err != nil {
		t.Fatalf("Symlink() external target error = %v", err)
	}
	if _, _, err := resolveCodexAuthFile(root, "escaped.json"); err == nil {
		t.Fatal("resolveCodexAuthFile() accepted a symlink outside auth-dir")
	}
}

func TestCodexAuthDiscoveryAndPoolRejectDuplicatePhysicalSource(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "primary.json")
	if err := os.WriteFile(target, []byte(`{"type":"codex","access_token":"shared-token","account_id":"shared-account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Symlink("primary.json", filepath.Join(root, "alias.json")); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	runtime.mu.Lock()
	previous := runtime.engine
	runtime.engine = engine
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.engine = previous
		runtime.mu.Unlock()
	}()
	accounts, err := discoverCodexAccounts()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("discoverCodexAccounts() = %+v, %v; want one physical source", accounts, err)
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "primary.json", Name: "primary", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	if err := validateDistinctCodexAccountSource(engine, quota.AccountPoolEntry{AuthID: "alias.json", Name: "alias", CapacityX: 1, Enabled: true}); err == nil {
		t.Fatal("validateDistinctCodexAccountSource() accepted a duplicate physical credential")
	}
	// Existing SQLite rows can predate the save-time validation. The background
	// reconciliation must catch that historical state and pause managed Keys.
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "alias.json", Name: "alias", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(alias) error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	syncer.RefreshAccountSourceConflict()
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("RefreshAccountSourceConflict() did not flag historical duplicate sources")
	}
}

func TestCodexAuthDiscoveryAndPoolRejectDuplicateAccountIdentity(t *testing.T) {
	root := t.TempDir()
	for name, token := range map[string]string{"primary.json": "primary-token", "copy.json": "copy-token"} {
		payload := fmt.Sprintf(`{"type":"codex","access_token":%q,"account_id":"same-account"}`, token)
		if err := os.WriteFile(filepath.Join(root, name), []byte(payload), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	runtime.mu.Lock()
	previous := runtime.engine
	runtime.engine = engine
	runtime.mu.Unlock()
	defer func() {
		runtime.mu.Lock()
		runtime.engine = previous
		runtime.mu.Unlock()
	}()
	accounts, err := discoverCodexAccounts()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("discoverCodexAccounts() = %+v, %v; want one stable account identity", accounts, err)
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "primary.json", Name: "primary", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(primary) error = %v", err)
	}
	if err := validateDistinctCodexAccountSource(engine, quota.AccountPoolEntry{AuthID: "copy.json", Name: "copy", CapacityX: 1, Enabled: true}); err == nil {
		t.Fatal("validateDistinctCodexAccountSource() accepted a copied credential for the same Codex account")
	}
	// Historical SQLite rows can contain copied files even though new saves are
	// rejected. Reconciliation must pause managed Keys in that state.
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "copy.json", Name: "copy", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(copy) error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	syncer.RefreshAccountSourceConflictIfDue()
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("RefreshAccountSourceConflictIfDue() did not flag duplicate Codex account identity")
	}
	// A short CPA rewrite must not clear a previously confirmed duplicate just
	// because one sibling cannot be parsed during this reconciliation pass.
	if err := os.WriteFile(filepath.Join(root, "copy.json"), []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("WriteFile() partial copy error = %v", err)
	}
	syncer.RefreshAccountSourceConflict()
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("transient auth rewrite incorrectly cleared duplicate account identity conflict")
	}
}

func TestCodexAccountPoolAllowsOneMissingStableIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unknown.json"), []byte(`{"type":"codex","access_token":"token-without-account-id"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "known.json"), []byte(`{"type":"codex","access_token":"token-with-account-id","account_id":"known-account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(known) error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if err := validateDistinctCodexAccountSource(engine, quota.AccountPoolEntry{AuthID: "unknown.json", Name: "unknown", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("validateDistinctCodexAccountSource(single unknown) error = %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "unknown.json", Name: "unknown", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	syncer.RefreshAccountSourceConflict()
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("RefreshAccountSourceConflict() blocked one physically distinct identity-less account")
	}
	if err := validateDistinctCodexAccountSource(engine, quota.AccountPoolEntry{AuthID: "known.json", Name: "known", CapacityX: 1, Enabled: true}); err == nil {
		t.Fatal("validateDistinctCodexAccountSource() accepted a known account beside an identity-less account")
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "known.json", Name: "known", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(known historical) error = %v", err)
	}
	syncer.RefreshAccountSourceConflict()
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("RefreshAccountSourceConflict() did not fail closed for an identity-less account mixed with another account")
	}
}

func TestQuotaSynchronizerReconcilesSourcesIndependentlyOfQuotaRefresh(t *testing.T) {
	previousInterval := accountSourceReconcileInterval
	accountSourceReconcileInterval = 5 * time.Millisecond
	defer func() { accountSourceReconcileInterval = previousInterval }()

	root := t.TempDir()
	for _, name := range []string{"first.json", "second.json"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(`{"type":"codex","access_token":"shared-token","account_id":"shared-account"}`), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	defer func() {
		syncer.Close()
		_ = engine.Close()
	}()

	// Start with an empty pool so TriggerAll cannot schedule any official HTTP
	// request. Adding historical entries afterwards proves the independent
	// source ticker, rather than quota polling, catches the duplicate identity.
	syncer.Start()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "first.json", Name: "first", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(first) error = %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "second.json", Name: "second", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry(second) error = %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		if time.Now().After(deadline) {
			t.Fatal("independent source reconciler did not flag duplicate identity without a quota refresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAuthDirectoryChangeStaysClosedUntilSourceScanIsComplete(t *testing.T) {
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(oldRoot, "account.json"), []byte(`{"type":"codex","access_token":"old-token","account_id":"account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(newRoot, "account.json"), []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("WriteFile(new partial) error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   oldRoot,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account.json", Name: "account", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	syncer := newQuotaSynchronizer(engine)
	engine.RequireAccountSourceVerification()
	syncer.RefreshAccountSourceConflict()
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("initial complete source scan did not open the pool")
	}

	settings := engine.Installation().Settings
	settings.AuthDirectory = newRoot
	if _, err := engine.ConfigureInstallation(settings); err != nil {
		t.Fatalf("ConfigureInstallation() error = %v", err)
	}
	syncer.ClearCredentials()
	syncer.RefreshAccountSourceConflict()
	if !engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("partial auth-dir scan reopened the pool after configuration change")
	}
	if err := os.WriteFile(filepath.Join(newRoot, "account.json"), []byte(`{"type":"codex","access_token":"new-token","account_id":"account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile(new complete) error = %v", err)
	}
	syncer.RefreshAccountSourceConflict()
	if engine.Summary(time.Now().UTC()).Status.AccountSourceConflict {
		t.Fatal("complete source scan did not reopen the pool after configuration change")
	}
}

func TestQuotaSynchronizerRejectsCacheForTransientAuthRewrite(t *testing.T) {
	root := t.TempDir()
	authID := "account.json"
	path := filepath.Join(root, authID)
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"cached-token","account_id":"cached-account"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	syncer := newQuotaSynchronizer(engine)
	if _, _, err := syncer.resolveCredential(authID); err != nil {
		t.Fatalf("initial resolveCredential() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("WriteFile() partial auth error = %v", err)
	}
	if _, _, err := syncer.resolveCredential(authID); err == nil {
		t.Fatal("transient rewrite unexpectedly reused a credential from changed auth-file contents")
	}
	if err := os.WriteFile(path, []byte(`{"type":"codex","disabled":true,"access_token":"new-token"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() disabled auth error = %v", err)
	}
	if _, _, err := syncer.resolveCredential(authID); err == nil {
		t.Fatal("disabled credential was incorrectly masked by the transient cache")
	}
}

func TestQuotaSynchronizerDoesNotReuseCacheAfterAuthSymlinkRetarget(t *testing.T) {
	root := t.TempDir()
	primary := filepath.Join(root, "primary.json")
	if err := os.WriteFile(primary, []byte(`{"type":"codex","access_token":"cached-token"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() primary error = %v", err)
	}
	authID := "account.json"
	link := filepath.Join(root, authID)
	if err := os.Symlink("primary.json", link); err != nil {
		t.Fatalf("Symlink() primary error = %v", err)
	}
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		AuthDirectory:   root,
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	syncer := newQuotaSynchronizer(engine)
	if _, _, err := syncer.resolveCredential(authID); err != nil {
		t.Fatalf("initial resolveCredential() error = %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatalf("Remove() old symlink error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "replacement.json"), []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("WriteFile() replacement error = %v", err)
	}
	if err := os.Symlink("replacement.json", link); err != nil {
		t.Fatalf("Symlink() replacement error = %v", err)
	}
	if _, _, err := syncer.resolveCredential(authID); err == nil {
		t.Fatal("retargeted auth symlink incorrectly reused the old cached credential")
	}
}

func TestQuotaCredentialExpiresAtUsesJWTAndFallback(t *testing.T) {
	now := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1785157200}`))
	jwt := "header." + payload + ".signature"
	if got := quotaCredentialExpiresAt(jwt, now); !got.Equal(time.Unix(1785157200, 0).UTC()) {
		t.Fatalf("quotaCredentialExpiresAt(jwt) = %v", got)
	}
	if got := quotaCredentialExpiresAt("opaque-token", now); !got.Equal(now.Add(quotaCredentialFallbackTTL)) {
		t.Fatalf("quotaCredentialExpiresAt(opaque) = %v", got)
	}
}

func TestCredentialAuthorizationFailure(t *testing.T) {
	if !isCredentialAuthorizationFailure(officialQuotaHTTPError{statusCode: 401}) {
		t.Fatal("401 did not invalidate the cached credential")
	}
	if isCredentialAuthorizationFailure(officialQuotaHTTPError{statusCode: 429}) {
		t.Fatal("429 incorrectly invalidated the cached credential")
	}
}

func TestQuotaSynchronizerShutdownDrainAllowsCredentialRenewal(t *testing.T) {
	syncer := newQuotaSynchronizer(nil)
	syncer.BeginShutdownDrain()
	if syncer.usesCachedCredentialsOnly() {
		t.Fatal("shutdown drain disabled credential renewal before final shutdown")
	}
	syncer.lifecycleMu.Lock()
	draining := syncer.draining
	syncer.lifecycleMu.Unlock()
	if !draining {
		t.Fatal("shutdown drain state was not recorded")
	}
}

func TestQuotaSynchronizerShutdownDrainWaitsForOlderRefresh(t *testing.T) {
	syncer := newQuotaSynchronizer(nil)
	syncer.refreshGate.RLock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		syncer.BeginShutdownDrain()
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		syncer.refreshGate.RUnlock()
		t.Fatal("shutdown-drain goroutine did not start")
	}
	// Yield after the start handshake so the goroutine reaches the blocked
	// writer acquisition instead of allowing this assertion to pass before it
	// has run at all.
	goruntime.Gosched()
	select {
	case <-done:
		t.Fatal("BeginShutdownDrain() did not wait for an older refresh")
	case <-time.After(20 * time.Millisecond):
	}
	syncer.refreshGate.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("BeginShutdownDrain() did not complete after the refresh released")
	}
}

func TestQuotaSynchronizerShutdownDrainAdmitsOnlyPendingAccount(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.UpsertPolicy(quota.KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true}, "managed-key"); err != nil {
		t.Fatalf("UpsertPolicy() error = %v", err)
	}
	if _, err := engine.UpsertAccountPoolEntry(quota.AccountPoolEntry{AuthID: "account-a", AuthIndex: "a", Name: "A", CapacityX: 1, Enabled: true}); err != nil {
		t.Fatalf("UpsertAccountPoolEntry() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(7 * 24 * time.Hour)
	if err := engine.UpdateOfficialQuota(quota.OfficialQuotaSnapshot{
		AuthID: "account-a", Allowed: true,
		Secondary:  quota.OfficialQuotaWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour) / time.Second), ResetAt: &resetAt},
		ObservedAt: now,
	}); err != nil {
		t.Fatalf("UpdateOfficialQuota() error = %v", err)
	}
	if admission := engine.Admit("managed-key", "gpt-5", now, []quota.SchedulerCandidate{{AuthID: "account-a"}}); !admission.Allowed {
		t.Fatalf("Admit() = %+v, want allowed", admission)
	}

	syncer := newQuotaSynchronizer(engine)
	syncer.BeginShutdownDrain()
	if release, allowed := syncer.beginRefresh("account-b"); allowed {
		release()
		t.Fatal("shutdown drain admitted an unrelated account")
	}
	if release, allowed := syncer.beginRefresh("account-a"); !allowed {
		t.Fatal("shutdown drain rejected the account with a pending settlement")
	} else {
		release()
	}
	// A stale queued account must be removed from inFlight without pretending
	// its previous sync error recovered or changing its retry schedule.
	nextRefreshAt := now.Add(9 * time.Minute)
	syncer.mu.Lock()
	syncer.lastErrorLog["account-b"] = now
	syncer.nextRefreshAt["account-b"] = nextRefreshAt
	syncer.mu.Unlock()
	if !syncer.startRefresh("account-b", true) {
		t.Fatal("startRefresh(account-b) = false, want queued stale job")
	}
	syncer.refreshQueued("account-b")
	syncer.mu.Lock()
	_, inFlight := syncer.inFlight["account-b"]
	_, retainedError := syncer.lastErrorLog["account-b"]
	retainedNext := syncer.nextRefreshAt["account-b"]
	syncer.mu.Unlock()
	if inFlight || !retainedError || !retainedNext.Equal(nextRefreshAt) {
		t.Fatalf("skipped refresh changed diagnostic state: in_flight=%v error=%v next=%v", inFlight, retainedError, retainedNext)
	}
	engine.RecordUsage(quota.CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 1})
}

func TestQuotaSynchronizerShutdownCancellationKeepsQuotaDiagnosis(t *testing.T) {
	syncer := newQuotaSynchronizer(nil)
	now := time.Now().UTC()
	nextRefreshAt := now.Add(9 * time.Minute)
	syncer.mu.Lock()
	syncer.inFlight["account-a"] = struct{}{}
	syncer.lastErrorLog["account-a"] = now
	syncer.nextRefreshAt["account-a"] = nextRefreshAt
	syncer.mu.Unlock()

	if syncer.isShutdownCancellation(fmt.Errorf("official quota request: %w", context.Canceled)) {
		t.Fatal("an active synchronizer treated an unrelated canceled request as shutdown")
	}
	syncer.cancel()
	if !syncer.isShutdownCancellation(fmt.Errorf("official quota request: %w", context.Canceled)) {
		t.Fatal("synchronizer shutdown cancellation was not recognized")
	}
	syncer.completeQueuedRefresh("account-a", fmt.Errorf("official quota request: %w", context.Canceled))

	syncer.mu.Lock()
	_, inFlight := syncer.inFlight["account-a"]
	_, retainedError := syncer.lastErrorLog["account-a"]
	retainedNext := syncer.nextRefreshAt["account-a"]
	syncer.mu.Unlock()
	if inFlight || !retainedError || !retainedNext.Equal(nextRefreshAt) {
		t.Fatalf("shutdown cancellation changed quota diagnosis: in_flight=%v error=%v next=%v", inFlight, retainedError, retainedNext)
	}
}

func TestQuotaSynchronizerCloseCancelsBeforeLifecycleLock(t *testing.T) {
	syncer := newQuotaSynchronizer(nil)
	syncer.lifecycleMu.Lock()
	done := make(chan struct{})
	go func() {
		syncer.Close()
		close(done)
	}()
	select {
	case <-syncer.ctx.Done():
	case <-time.After(time.Second):
		syncer.lifecycleMu.Unlock()
		t.Fatal("Close() did not cancel direct HTTP before waiting for lifecycleMu")
	}
	syncer.lifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after lifecycleMu released")
	}
}

func TestQuotaSynchronizerCloseWaitsForAllWorkers(t *testing.T) {
	cfg, err := quota.NormalizeConfig(quota.Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := quota.Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = engine.Close() }()

	syncer := newQuotaSynchronizer(engine)
	syncer.Start()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	syncer.workers.Add(1)
	go func() {
		defer syncer.workers.Done()
		<-release
	}()
	closed := make(chan struct{})
	go func() {
		syncer.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close() returned while a synchronizer worker was still running")
	case <-time.After(2200 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close() did not finish after every synchronizer worker exited")
	}
}

package quota

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAnalysisReaderFallsBackAndRecoversAfterStartupFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	originalOpen := openUsageAnalysisReader
	openAttempts := 0
	openUsageAnalysisReader = func(driverName, dsn string) (*sql.DB, error) {
		openAttempts++
		if openAttempts == 1 {
			return nil, errors.New("simulated analysis reader failure")
		}
		return originalOpen(driverName, dsn)
	}
	t.Cleanup(func() { openUsageAnalysisReader = originalOpen })

	store, err := OpenStore(filepath.Join(t.TempDir(), "codex-carpool.db"))
	if err != nil {
		t.Fatalf("OpenStore() must keep the quota store available: %v", err)
	}
	defer func() { _ = store.Close() }()
	if !store.AnalysisReaderDegraded() || store.analysisDB != nil {
		t.Fatalf("analysis reader fallback = degraded:%v reader:%v, want degraded writer fallback", store.AnalysisReaderDegraded(), store.analysisDB)
	}

	now := time.Now().UTC().Truncate(time.Minute)
	if err := store.UpsertUsageBuckets([]UsageEvent{{
		Scope: "key_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: now,
		Units: 17, RequestCount: 1, MeteredBy: "test",
	}}); err != nil {
		t.Fatalf("UpsertUsageBuckets() through writer fallback: %v", err)
	}
	// Keep the first chart on the bounded writer fallback, then prove a later
	// management retry returns the chart to a dedicated WAL reader.
	store.analysisMu.Lock()
	store.analysisReaderRetryAt = time.Now().Add(time.Hour)
	store.analysisMu.Unlock()
	points, availableFrom, err := store.LoadUsageAnalysisSnapshot(context.Background(), "managed", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || availableFrom == nil || len(points) != 1 || points[0].Units != 17 {
		t.Fatalf("writer fallback analysis = points:%+v available:%v err:%v", points, availableFrom, err)
	}
	store.analysisMu.Lock()
	store.analysisReaderRetryAt = time.Time{}
	store.analysisMu.Unlock()
	if !store.RestoreAnalysisReader() || store.AnalysisReaderDegraded() || store.analysisDB == nil {
		t.Fatalf("analysis reader should recover after transient startup failure: degraded=%v reader=%v", store.AnalysisReaderDegraded(), store.analysisDB)
	}
}

func TestAccessScheduleIsUnrestrictedWhenNoRuleExists(t *testing.T) {
	policy := KeyPolicy{}
	if !policy.AllowsAt(time.Date(2026, time.July, 28, 2, 15, 0, 0, time.UTC)) {
		t.Fatal("policy without access rules must remain unrestricted")
	}

	normalized, err := normalizePolicy(KeyPolicy{ID: "managed", Name: "Managed", KeySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AllocationX: 1, AccessTimezone: "UTC"}, nil, 1)
	if err != nil {
		t.Fatalf("normalizePolicy() error = %v", err)
	}
	if len(normalized.AccessRules) != 0 || normalized.AccessTimezone != "" {
		t.Fatalf("unrestricted policy = %+v, want no persisted access timezone", normalized)
	}
}

func TestAccessScheduleHonorsWeekdaysAndOvernightIntervals(t *testing.T) {
	policy := KeyPolicy{
		AccessTimezone: "Asia/Shanghai",
		AccessRules: []AccessRule{{Weekdays: []int{1}, Start: "08:00", End: "09:00"}},
	}
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.AllowsAt(time.Date(2026, time.July, 27, 8, 30, 0, 0, shanghai)) { // Monday
		t.Fatal("Monday 08:30 should be permitted")
	}
	if policy.AllowsAt(time.Date(2026, time.July, 27, 9, 0, 0, 0, shanghai)) {
		t.Fatal("the end of a non-overnight interval must be exclusive")
	}
	if policy.AllowsAt(time.Date(2026, time.July, 28, 8, 30, 0, 0, shanghai)) {
		t.Fatal("Tuesday should not inherit a Monday-only interval")
	}

	overnight := KeyPolicy{
		AccessTimezone: "Asia/Shanghai",
		AccessRules: []AccessRule{{Weekdays: []int{5}, Start: "23:00", End: "01:00"}},
	}
	if !overnight.AllowsAt(time.Date(2026, time.July, 31, 23, 20, 0, 0, shanghai)) { // Friday
		t.Fatal("Friday 23:20 should be permitted by an overnight interval")
	}
	if !overnight.AllowsAt(time.Date(2026, time.August, 1, 0, 30, 0, 0, shanghai)) { // Saturday
		t.Fatal("Saturday 00:30 should be permitted by Friday's overnight interval")
	}
	if overnight.AllowsAt(time.Date(2026, time.August, 1, 1, 0, 0, 0, shanghai)) {
		t.Fatal("the overnight interval end must be exclusive")
	}
}

func TestAdmissionBlocksManagedKeyOutsideConfiguredSchedule(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{
		ID:              "managed",
		Name:            "Managed",
		AllocationX:     1,
		Enabled:         true,
		AccessTimezone:  "UTC",
		AccessRules:     []AccessRule{{Weekdays: []int{1}, Start: "08:00", End: "09:00"}},
	})
	defer func() { _ = engine.Close() }()

	now := time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC) // Tuesday
	admission := engine.Admit("managed-key", "gpt-5", now)
	if admission.Allowed || admission.Bypass || admission.Code != "access_schedule_closed" {
		t.Fatalf("outside schedule admission = %+v, want a managed access-schedule block", admission)
	}
}

func TestMultipleAccessRulesPersistAcrossEngineOpen(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	secret := "test-only-hmac-secret-with-at-least-32-characters"
	policy := KeyPolicy{
		ID:             "managed",
		Name:           "Managed",
		AllocationX:    1,
		Enabled:        true,
		AccessTimezone: "Asia/Shanghai",
		AccessRules: []AccessRule{
			{Weekdays: []int{1, 2, 3, 4, 5}, Start: "08:00", End: "09:00"},
			{Weekdays: []int{6, 7}, Start: "20:00", End: "22:00"},
		},
	}
	cfg, err := NormalizeConfig(Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		RequestUnits:    1,
		KeyHMACSecret:   secret,
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := engine.UpsertPolicy(policy, "managed-key"); err != nil {
		_ = engine.Close()
		t.Fatalf("UpsertPolicy() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	engine, err = Open(cfg)
	if err != nil {
		t.Fatalf("reopen Engine error = %v", err)
	}
	defer func() { _ = engine.Close() }()

	policies := engine.Policies()
	if len(policies) != 1 || len(policies[0].AccessRules) != 2 {
		t.Fatalf("persisted access rules = %+v, want both intervals", policies)
	}
	if policies[0].AccessRules[1].Start != "20:00" || policies[0].AccessRules[1].End != "22:00" {
		t.Fatalf("second persisted access rule = %+v, want weekend interval", policies[0].AccessRules[1])
	}
}

func TestUsageAnalysisGroupsActualLedgerBySelectedCalendarDay(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, time.July, 27, 23, 30, 0, 0, time.UTC) // 07/28 local
	second := time.Date(2026, time.July, 28, 16, 30, 0, 0, time.UTC) // 07/29 local
	if err := engine.store.UpsertUsageBuckets([]UsageEvent{
		{Scope: "key_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: first, Units: 120, RequestCount: 1, MeteredBy: "actual"},
		{Scope: "key_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: second, Units: 80, RequestCount: 2, MeteredBy: "actual"},
	}); err != nil {
		t.Fatalf("UpsertUsageBuckets() error = %v", err)
	}

	from := time.Date(2026, time.July, 28, 0, 0, 0, 0, location).UTC()
	until := time.Date(2026, time.July, 30, 0, 0, 0, 0, location).UTC()
	analysis, err := engine.UsageAnalysis("managed", from, until, location, "day")
	if err != nil {
		t.Fatalf("UsageAnalysis() error = %v", err)
	}
	if analysis.TotalTokens != 200 || analysis.RequestCount != 3 || len(analysis.Points) != 2 {
		t.Fatalf("analysis = %+v, want 200 tokens / 3 requests over two days", analysis)
	}
	if analysis.AvailableFrom == nil || analysis.RetentionDays != 366 {
		t.Fatalf("analysis coverage = %+v, want retained-history metadata", analysis)
	}
	if analysis.Points[0].Label != "2026-07-28" || analysis.Points[0].Units != 120 || analysis.Points[1].Label != "2026-07-29" || analysis.Points[1].Units != 80 {
		t.Fatalf("analysis points = %+v, want local calendar grouping", analysis.Points)
	}
	hourly, err := engine.UsageAnalysis("managed", from, until, location, "hour")
	if err != nil {
		t.Fatalf("hourly UsageAnalysis() error = %v", err)
	}
	if len(hourly.Points) != 48 ||
		hourly.Points[7].Label != "2026-07-28 07:00" || hourly.Points[7].Units != 120 ||
		hourly.Points[24].Label != "2026-07-29 00:00" || hourly.Points[24].Units != 80 {
		t.Fatalf("hourly analysis = %+v, want 48 local-hour points", hourly.Points)
	}
	monthly, err := engine.UsageAnalysis("managed", from, until, location, "month")
	if err != nil {
		t.Fatalf("monthly UsageAnalysis() error = %v", err)
	}
	if len(monthly.Points) != 1 || monthly.Points[0].Label != "2026-07" || monthly.Points[0].Units != 200 {
		t.Fatalf("monthly analysis = %+v, want one July total", monthly)
	}
	yearly, err := engine.UsageAnalysis("managed", from, until, location, "year")
	if err != nil {
		t.Fatalf("yearly UsageAnalysis() error = %v", err)
	}
	if len(yearly.Points) != 1 || yearly.Points[0].Label != "2026" || yearly.Points[0].Units != 200 {
		t.Fatalf("yearly analysis = %+v, want one 2026 total", yearly)
	}
}

func TestSummaryWithActualTokensReportsCycleAndCumulativeTotal(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 5, Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Minute)
	resetAt := now.Add(2 * 24 * time.Hour)
	cycleKey := allocationCycleKey{KeyID: "managed", AuthID: "account-a", WindowResetAt: resetAt.UnixMilli()}
	engine.allocationMu.Lock()
	engine.allocationCycles[cycleKey] = allocationCycleState{Capacity: officialXUnitsPerX, GlobalCapacity: 5 * officialXUnitsPerX}
	engine.allocationMu.Unlock()
	if err := engine.store.UpsertUsageBuckets([]UsageEvent{
		{Scope: "key_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: now.Add(-6 * 24 * time.Hour), Units: 70, RequestCount: 1, MeteredBy: "actual"},
		{Scope: "key_actual", KeyID: "managed", AuthID: "account-a", RecordedAt: now.Add(-time.Hour), Units: 30, RequestCount: 1, MeteredBy: "actual"},
	}); err != nil {
		t.Fatalf("UpsertUsageBuckets() error = %v", err)
	}

	summary, err := engine.SummaryWithActualTokens(now)
	if err != nil {
		t.Fatalf("SummaryWithActualTokens() error = %v", err)
	}
	if len(summary.Keys) != 1 {
		t.Fatalf("summary keys = %d, want 1", len(summary.Keys))
	}
	actual := summary.Keys[0].ActualTokens
	if !actual.Available || !actual.CycleKnown || actual.Cycle != 30 || actual.Total != 100 {
		t.Fatalf("actual Token summary = %+v, want cycle=30 total=100", actual)
	}
	if _, err := engine.store.db.Exec(`DELETE FROM usage_analysis_buckets WHERE bucket_at < ?`, resetAt.Add(-sevenDayWindow).UnixMilli()); err != nil {
		t.Fatalf("prune pre-cycle analysis history: %v", err)
	}
	summary, err = engine.SummaryWithActualTokens(now)
	if err != nil || summary.Keys[0].ActualTokens.Total != 100 || summary.Keys[0].ActualTokens.Cycle != 30 {
		t.Fatalf("actual Token total after analysis retention = %+v, %v; want lifetime total 100 and cycle 30", summary.Keys[0].ActualTokens, err)
	}

	engine.allocationMu.Lock()
	delete(engine.allocationCycles, cycleKey)
	engine.allocationMu.Unlock()
	summary, err = engine.SummaryWithActualTokens(now)
	if err != nil {
		t.Fatalf("SummaryWithActualTokens() without official cycle error = %v", err)
	}
	actual = summary.Keys[0].ActualTokens
	if actual.CycleKnown || actual.Total != 100 {
		t.Fatalf("unknown-cycle actual Token summary = %+v, want unknown cycle with retained total", actual)
	}
}

func TestUsageAnalysisRejectsMoreThan366LocalDays(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, time.March, 1, 0, 0, 0, 0, location).UTC()
	until := time.Date(2027, time.March, 3, 0, 0, 0, 0, location).UTC()
	if _, err := engine.UsageAnalysis("managed", from, until, location, "day"); err == nil {
		t.Fatal("UsageAnalysis() should reject more than 366 local calendar days across DST")
	}
}

func TestUsageAnalysisLimitsHourlyRangeTo31CalendarDays(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, time.July, 1, 0, 0, 0, 0, location).UTC()
	until := time.Date(2026, time.August, 2, 0, 0, 0, 0, location).UTC()
	if _, err := engine.UsageAnalysis("managed", from, until, location, "hour"); err == nil {
		t.Fatal("hourly UsageAnalysis() should reject more than 31 local calendar days")
	}
}

func TestUsageAnalysisBackfillIsOneTimeAndUsesPreUpgradeBucketBoundary(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	// A legacy minute bucket ending at midnight represents usage in the prior
	// minute. The analysis backfill must retain that original calendar day.
	legacyBucketAt := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := engine.store.db.Exec(`INSERT INTO usage_buckets(scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by)
VALUES ('key_actual', 'managed', '', 'account-a', ?, 12, 1, 'legacy_actual')`, legacyBucketAt.UnixMilli()); err != nil {
		t.Fatalf("seed legacy usage bucket: %v", err)
	}
	if _, err := engine.store.db.Exec(`DELETE FROM usage_analysis_buckets`); err != nil {
		t.Fatalf("clear usage analysis buckets: %v", err)
	}
	if _, err := engine.store.db.Exec(`DELETE FROM plugin_metadata WHERE name = ?`, usageAnalysisBackfillMetadataName); err != nil {
		t.Fatalf("clear usage analysis migration marker: %v", err)
	}
	if err := engine.store.backfillUsageAnalysisBuckets(); err != nil {
		t.Fatalf("backfillUsageAnalysisBuckets() error = %v", err)
	}
	from := legacyBucketAt.Add(-24 * time.Hour)
	until := legacyBucketAt
	points, err := engine.store.ListUsageAnalysisBuckets("managed", from, until)
	if err != nil || len(points) != 1 || points[0].Units != 12 {
		t.Fatalf("backfilled historical day = %+v, err=%v; want prior-day 12 tokens", points, err)
	}

	// New plugin writes are already mirrored transactionally. An old raw row
	// inserted after the migration marker must not make normal restart work scan
	// and rewrite the whole source ledger again.
	if _, err := engine.store.db.Exec(`INSERT INTO usage_buckets(scope, scope_id, group_id, auth_id, bucket_at, units, request_count, metered_by)
VALUES ('key_actual', 'managed', '', 'account-a', ?, 99, 1, 'simulated_late_raw')`, legacyBucketAt.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed post-migration raw bucket: %v", err)
	}
	if err := engine.store.backfillUsageAnalysisBuckets(); err != nil {
		t.Fatalf("second backfillUsageAnalysisBuckets() error = %v", err)
	}
	points, err = engine.store.ListUsageAnalysisBuckets("managed", from, until)
	if err != nil || len(points) != 1 || points[0].Units != 12 {
		t.Fatalf("one-time backfill = %+v, err=%v; want unchanged 12 tokens", points, err)
	}
}

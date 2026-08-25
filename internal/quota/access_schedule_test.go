package quota

import (
	"testing"
	"time"
)

func TestAccessScheduleIsUnrestrictedWhenNoRuleExists(t *testing.T) {
	policy, err := normalizePolicy(KeyPolicy{ID: "managed", Name: "Managed", KeySHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", AccessTimezone: "UTC"})
	if err != nil {
		t.Fatalf("normalizePolicy() error = %v", err)
	}
	if len(policy.AccessRules) != 0 || policy.AccessTimezone != "" || !policy.AllowsAt(time.Now()) {
		t.Fatalf("unrestricted policy = %+v", policy)
	}
}

func TestAccessScheduleHonorsWeekdaysAndOvernightIntervals(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	policy := KeyPolicy{AccessTimezone: location.String(), AccessRules: []AccessRule{{Weekdays: []int{5}, Start: "23:00", End: "01:00"}}}
	if !policy.AllowsAt(time.Date(2026, time.July, 31, 23, 20, 0, 0, location)) || !policy.AllowsAt(time.Date(2026, time.August, 1, 0, 30, 0, 0, location)) {
		t.Fatal("overnight interval should cover both sides of midnight")
	}
	if policy.AllowsAt(time.Date(2026, time.August, 1, 1, 0, 0, 0, location)) {
		t.Fatal("interval end must be exclusive")
	}
}

func TestAdmissionBlocksManagedKeyOutsideConfiguredSchedule(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true, AccessTimezone: "UTC",
		AccessRules: []AccessRule{{Weekdays: []int{1}, Start: "08:00", End: "09:00"}}})
	defer func() { _ = engine.Close() }()
	admission := engine.Admit("managed-key", "gpt-5", time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC))
	if admission.Allowed || admission.Bypass || admission.Code != "access_schedule_closed" {
		t.Fatalf("outside schedule admission = %+v", admission)
	}
}

func TestUsageAnalysisGroupsActualTokensByCalendarDay(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	location, _ := time.LoadLocation("Asia/Shanghai")
	first := time.Date(2026, time.July, 27, 23, 30, 0, 0, time.UTC)
	second := time.Date(2026, time.July, 28, 16, 30, 0, 0, time.UTC)
	if err := engine.store.UpsertUsageBuckets([]UsageEvent{
		{Scope: "key_actual", KeyID: "managed", Model: "gpt-5", RequestedAt: first, RecordedAt: first, Units: 120, RequestCount: 1,
			InputTokens: 70, CachedTokens: 30, OutputTokens: 20, InputCostMicros: 7, CachedCostMicros: 3, OutputCostMicros: 2, CostMicros: 12},
		{Scope: "key_actual", KeyID: "managed", Model: "gpt-5", RequestedAt: second, RecordedAt: second, Units: 80, RequestCount: 2,
			InputTokens: 40, CachedTokens: 20, OutputTokens: 20, InputCostMicros: 4, CachedCostMicros: 2, OutputCostMicros: 2, CostMicros: 8},
	}); err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, time.July, 28, 0, 0, 0, 0, location).UTC()
	until := time.Date(2026, time.July, 30, 0, 0, 0, 0, location).UTC()
	analysis, err := engine.UsageAnalysis("managed", from, until, location, "day")
	if err != nil || analysis.TotalTokens != 200 || analysis.RequestCount != 3 || len(analysis.Points) != 2 {
		t.Fatalf("analysis = %+v, err=%v", analysis, err)
	}
	if point := analysis.Points[0]; point.InputTokens != 70 || point.CachedTokens != 30 || point.OutputTokens != 20 || point.CostMicros != 12 {
		t.Fatalf("first analysis tooltip point = %+v", point)
	}
	if point := analysis.Points[1]; point.InputTokens != 40 || point.CachedTokens != 20 || point.OutputTokens != 20 || point.CostMicros != 8 {
		t.Fatalf("second analysis tooltip point = %+v", point)
	}
}

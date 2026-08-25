package quota

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestOpenSeedsDefaultModelRatesOnlyForEmptyRateCard(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	config, err := NormalizeConfig(Config{
		DatabasePath:    filepath.Join(t.TempDir(), "codex-carpool.db"),
		KeyHMACSecret:   "test-only-hmac-secret-with-at-least-32-characters",
		RecordRetention: "168h",
	})
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	engine, err := Open(config)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	rates := engine.ModelRates()
	if len(rates) != len(defaultModelRates) {
		_ = engine.Close()
		t.Fatalf("seeded rate count = %d, want %d", len(rates), len(defaultModelRates))
	}
	for _, expected := range defaultModelRates {
		actual, exists := engine.modelRate(expected.Model)
		if !exists || actual.InputUSDPerMillion != expected.InputUSDPerMillion || actual.CachedUSDPerMillion != expected.CachedUSDPerMillion || actual.OutputUSDPerMillion != expected.OutputUSDPerMillion {
			_ = engine.Close()
			t.Fatalf("seeded rate for %q = %+v, exists=%t", expected.Model, actual, exists)
		}
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "operator-custom", InputUSDPerMillion: 3, CachedUSDPerMillion: 0.3, OutputUSDPerMillion: 12}}); err != nil {
		_ = engine.Close()
		t.Fatalf("ReplaceModelRates() error = %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(config)
	if err != nil {
		t.Fatalf("Open(reopened) error = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	rates = reopened.ModelRates()
	if len(rates) != 1 || rates[0].Model != "operator-custom" || rates[0].OutputUSDPerMillion != 12 {
		t.Fatalf("reopened rates = %+v, want preserved operator rate only", rates)
	}
}

func TestCostMicrosBillsCachedInputOnlyOnce(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{
		Model:               "codex-test",
		InputUSDPerMillion:  10,
		CachedUSDPerMillion: 2.5,
		OutputUSDPerMillion: 40,
	})
	if err != nil {
		t.Fatalf("normalizeModelRate() error = %v", err)
	}

	// CPA input includes its cached subset. The 250k cached tokens must use
	// only the cache rate, not be charged once more at the normal input rate.
	got := costMicros(rate, 1_000_000, 250_000, 100_000)
	const want int64 = 12_125_000 // $7.50 uncached + $0.625 cached + $4 output
	if got != want {
		t.Fatalf("costMicros() = %d, want %d", got, want)
	}
}

func TestCostBreakdownMicrosPreservesEveryPriceComponent(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{
		Model: "all-components", InputUSDPerMillion: 10, CachedUSDPerMillion: 2.5, OutputUSDPerMillion: 40,
	})
	if err != nil {
		t.Fatalf("normalizeModelRate() error = %v", err)
	}
	got := costBreakdownMicros(rate, 1_000_000, 250_000, 100_000)
	if got.Input != 7_500_000 || got.Cached != 625_000 || got.Output != 4_000_000 || got.Total != 12_125_000 {
		t.Fatalf("costBreakdownMicros() = %+v", got)
	}
}

func TestManagedKeyModelAllowlistBlocksOnlyUnselectedModels(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", Available: true}, {ID: "gpt-5-mini", Available: true}}); err != nil {
		t.Fatalf("ReplaceModels() error = %v", err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5"}, {Model: "gpt-5-mini"}}); err != nil {
		t.Fatalf("ReplaceModelRates() error = %v", err)
	}
	policy := engine.Policies()[0]
	policy.AllowedModels = []string{"gpt-5"}
	updated, err := engine.UpsertPolicy(policy, "")
	if err != nil || len(updated.AllowedModels) != 1 || updated.AllowedModels[0] != "gpt-5" {
		t.Fatalf("UpsertPolicy(model allowlist) = %+v, err=%v", updated, err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := []SchedulerCandidate{{AuthID: "cpa-account"}}
	blocked := engine.Admit("managed-key", "gpt-5-mini", now, candidates)
	if blocked.Allowed || blocked.Code != "model_not_allowed" {
		t.Fatalf("disallowed model admission = %+v", blocked)
	}
	allowed := engine.Admit("managed-key", "gpt-5", now, candidates)
	if !allowed.Allowed {
		t.Fatalf("allowed model admission = %+v", allowed)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: allowed.AuthID, Model: "gpt-5", RequestedAt: now, Generate: true, Failed: true})
}

func TestUsageAnalysisBreakdownUsesAllRowsAndStoredComponentCosts(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	from := time.Now().UTC().Truncate(time.Hour)
	logs := make([]DecisionLog, 0, 12)
	events := make([]UsageEvent, 0, 12)
	for index := 0; index < 12; index++ {
		at := from.Add(time.Duration(index) * time.Minute)
		model := []string{"gpt-5", "gpt-5-mini"}[index%2]
		logs = append(logs, DecisionLog{
			KeyID: "managed", Model: model, RequestedAt: at,
			Decision: "completed", Units: 60, InputTokens: 30, CachedTokens: 20, OutputTokens: 10,
			InputCostMicros: 3, CachedCostMicros: 2, OutputCostMicros: 5, CostMicros: 10,
		})
		events = append(events, UsageEvent{Scope: "key_actual", KeyID: "managed", Model: model, RequestedAt: at, RecordedAt: at,
			Units: 60, RequestCount: 1, InputTokens: 30, CachedTokens: 20, OutputTokens: 10,
			InputCostMicros: 3, CachedCostMicros: 2, OutputCostMicros: 5, CostMicros: 10})
	}
	if err := engine.store.FlushUsageAndLogs(events, logs); err != nil {
		t.Fatalf("FlushUsageAndLogs() error = %v", err)
	}
	got, err := engine.store.LoadUsageAnalysisBreakdown(context.Background(), "managed", from, from.Add(time.Hour))
	if err != nil {
		t.Fatalf("LoadUsageAnalysisBreakdown() error = %v", err)
	}
	if got.InputTokens != 360 || got.CachedTokens != 240 || got.OutputTokens != 120 || got.InputCostMicros != 36 || got.CachedCostMicros != 24 || got.OutputCostMicros != 60 || got.CostMicros != 120 || len(got.Models) != 2 {
		t.Fatalf("full-range analysis breakdown = %+v", got)
	}
	if err := engine.store.ClearDecisionLogs("managed"); err != nil {
		t.Fatalf("ClearDecisionLogs() error = %v", err)
	}
	retained, err := engine.store.LoadUsageAnalysisBreakdown(context.Background(), "managed", from, from.Add(time.Hour))
	if err != nil || retained.CostMicros != 120 || retained.InputTokens != 360 {
		t.Fatalf("analysis after clearing audit logs = %+v, err=%v", retained, err)
	}
}

func TestModelUsageRankingAggregatesAllKeysByStoredDailyCost(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	from := time.Date(2026, time.August, 25, 0, 0, 0, 0, time.UTC)
	events := []UsageEvent{
		{Scope: "key_actual", KeyID: "managed", Model: "gpt-high", RequestedAt: from.Add(time.Hour), RecordedAt: from.Add(time.Hour), Units: 100, RequestCount: 1, InputTokens: 70, CachedTokens: 20, OutputTokens: 10, CostMicros: 900},
		{Scope: "key_actual", KeyID: "other", Model: "gpt-low", RequestedAt: from.Add(2 * time.Hour), RecordedAt: from.Add(2 * time.Hour), Units: 200, RequestCount: 1, InputTokens: 140, CachedTokens: 40, OutputTokens: 20, CostMicros: 200},
		{Scope: "key_actual", KeyID: "other", Model: "gpt-high", RequestedAt: from.Add(3 * time.Hour), RecordedAt: from.Add(3 * time.Hour), Units: 50, RequestCount: 1, InputTokens: 35, CachedTokens: 10, OutputTokens: 5, CostMicros: 450},
	}
	if err := engine.store.FlushUsageAndLogs(events, nil); err != nil {
		t.Fatalf("FlushUsageAndLogs() error = %v", err)
	}
	got, err := engine.ModelUsageRanking(from, from.Add(24*time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("ModelUsageRanking() error = %v", err)
	}
	if got.TotalTokens != 350 || got.RequestCount != 3 || got.CostMicros != 1550 || len(got.Models) != 2 {
		t.Fatalf("global daily ranking = %+v", got)
	}
	if first := got.Models[0]; first.Model != "gpt-high" || first.TotalTokens != 150 || first.CostMicros != 1350 {
		t.Fatalf("first model ranking = %+v", first)
	}
	if second := got.Models[1]; second.Model != "gpt-low" || second.TotalTokens != 200 || second.CostMicros != 200 {
		t.Fatalf("second model ranking = %+v", second)
	}
	if _, err := engine.ModelUsageRanking(from, from.Add(48*time.Hour), time.UTC); err == nil {
		t.Fatal("multi-day model ranking should be rejected")
	}
}

func TestClearDecisionLogsWithoutKeyClearsGlobalLogView(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	logs := []DecisionLog{
		{KeyID: "managed", Model: "gpt-5", RequestedAt: now, Decision: "completed"},
		{KeyID: "other", Model: "gpt-5-mini", RequestedAt: now.Add(time.Second), Decision: "completed"},
	}
	if err := engine.store.FlushUsageAndLogs(nil, logs); err != nil {
		t.Fatalf("FlushUsageAndLogs() error = %v", err)
	}
	if err := engine.ClearDecisionLogs(""); err != nil {
		t.Fatalf("ClearDecisionLogs(global) error = %v", err)
	}
	page, err := engine.DecisionLogPage("", "", "", 1, 10)
	if err != nil {
		t.Fatalf("DecisionLogPage() error = %v", err)
	}
	if page.Total != 0 || len(page.Logs) != 0 {
		t.Fatalf("global decision logs after clear = %+v", page)
	}
}

func TestUnlimitedDollarWindowStillReportsSettledSpend(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	state := newKeyMeterState([]meterEvent{{At: now.Add(-time.Hour), Units: 1_250_000}}, now)

	snapshot := fixedWindowSnapshot(state, now, 0, now.Add(-time.Hour), fiveHourWindow)
	wantRefresh := now.Add(4 * time.Hour)
	if snapshot.Limited || snapshot.BudgetUSD != 0 || snapshot.SpentUSD != 1.25 || snapshot.CoolingUntil != nil || snapshot.RefreshAt == nil || !snapshot.RefreshAt.Equal(wantRefresh) {
		t.Fatalf("unlimited snapshot = %+v", snapshot)
	}
}

func TestDollarCoolingUsesLaterWindowRecovery(t *testing.T) {
	first := time.Date(2026, time.August, 4, 2, 0, 0, 0, time.UTC)
	second := time.Date(2026, time.August, 4, 11, 0, 0, 0, time.UTC)
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	state := newKeyMeterState([]meterEvent{{At: first, Units: 1_000_000}, {At: second, Units: 1_000_000}}, now)

	five := fixedWindowSnapshot(state, now, 1_000_000, second, fiveHourWindow)
	week := fixedWindowSnapshot(state, now, 1_000_000, first, sevenDayWindow)
	later := laterCoolingUntil(five.CoolingUntil, week.CoolingUntil)
	if later == nil || !later.Equal(first.Add(sevenDayWindow)) {
		t.Fatalf("later cooling = %v, want %v", later, first.Add(sevenDayWindow))
	}
}

func TestManagedDollarBudgetChargesOnlyManagedKeyUsage(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", FiveHourBudgetUSD: 0.5, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", OutputUSDPerMillion: 10}}); err != nil {
		t.Fatalf("ReplaceModelRates() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := readyNativeCandidate(t, engine, now)

	// A request that bypasses the managed Key can affect the account's official
	// progress, but must never consume this Key's independent dollar ledger.
	engine.RecordUsage(CompletedUsage{APIKey: "external-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 100_000, OutputTokens: 100_000})
	first := engine.Admit("managed-key", "gpt-5", now.Add(time.Second), candidates)
	if !first.Allowed {
		t.Fatalf("first managed admission = %+v, want allowed after external traffic", first)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: first.AuthID, Model: "gpt-5", RequestedAt: now.Add(time.Second), Generate: true, TotalTokens: 60_000, OutputTokens: 60_000})

	policy := engine.Policies()[0]
	spend := engine.dollarSpendSnapshot(policy, now.Add(2*time.Second)).FiveHour
	if !spend.Limited || spend.SpentUSD != 0.6 || spend.RemainingUSD != 0 || spend.CoolingUntil == nil {
		t.Fatalf("managed Key five-hour dollar spend = %+v, want a $0.60 exhausted $0.50 window", spend)
	}
	second := engine.Admit("managed-key", "gpt-5", now.Add(2*time.Second), candidates)
	if second.Allowed || second.Code != "key_dollar_budget_exhausted" {
		t.Fatalf("second managed admission = %+v, want dollar-budget 429", second)
	}
}

func TestManagedDollarBudgetCyclesStayAnchoredToFirstRequest(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", FiveHourBudgetUSD: 0.5, SevenDayBudgetUSD: 10, Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", OutputUSDPerMillion: 10}}); err != nil {
		t.Fatalf("ReplaceModelRates() error = %v", err)
	}

	// Keep the request deliberately away from a minute boundary. The previous
	// implementation started its cost window at the end of this minute instead
	// of at this request's own timestamp.
	requestedAt := time.Now().UTC().Truncate(time.Minute).Add(17*time.Second + 321*time.Millisecond)
	candidates := readyNativeCandidate(t, engine, requestedAt)
	first := engine.Admit("managed-key", "gpt-5", requestedAt, candidates)
	if !first.Allowed {
		t.Fatalf("first admission = %+v, want allowed", first)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: first.AuthID, Model: "gpt-5", RequestedAt: requestedAt,
		Generate: true, OutputTokens: 20_000, TotalTokens: 20_000,
	})
	secondAt := requestedAt.Add(time.Minute)
	second := engine.Admit("managed-key", "gpt-5", secondAt, candidates)
	if !second.Allowed {
		t.Fatalf("second admission = %+v, want allowed", second)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: second.AuthID, Model: "gpt-5", RequestedAt: secondAt,
		Generate: true, OutputTokens: 40_000, TotalTokens: 40_000})

	policy := engine.Policies()[0]
	beforeFiveHourRelease := engine.dollarSpendSnapshot(policy, requestedAt.Add(fiveHourWindow-time.Millisecond)).FiveHour
	if beforeFiveHourRelease.SpentUSD != 0.6 || beforeFiveHourRelease.RefreshAt == nil || !beforeFiveHourRelease.RefreshAt.Equal(requestedAt.Add(fiveHourWindow)) ||
		beforeFiveHourRelease.CoolingUntil == nil || !beforeFiveHourRelease.CoolingUntil.Equal(requestedAt.Add(fiveHourWindow)) {
		t.Fatalf("five-hour spend immediately before request-time release = %+v", beforeFiveHourRelease)
	}
	afterFiveHourRelease := engine.dollarSpendSnapshot(policy, requestedAt.Add(fiveHourWindow)).FiveHour
	if afterFiveHourRelease.SpentUSD != 0 || afterFiveHourRelease.RefreshAt != nil || afterFiveHourRelease.CoolingUntil != nil {
		t.Fatalf("five-hour spend at the fixed cycle boundary = %+v, want a full reset", afterFiveHourRelease)
	}
	thirdAt := requestedAt.Add(fiveHourWindow + time.Second)
	third := engine.Admit("managed-key", "gpt-5", thirdAt, candidates)
	if !third.Allowed {
		t.Fatalf("third admission = %+v, want a new five-hour cycle", third)
	}
	cycles := engine.budgetCycles(policy.ID)
	if !cycles.FiveHourStartedAt.Equal(thirdAt.Truncate(time.Millisecond)) || !cycles.SevenDayStartedAt.Equal(requestedAt.Truncate(time.Millisecond)) {
		t.Fatalf("cycles after the next request = %+v", cycles)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: third.AuthID, Model: "gpt-5", RequestedAt: thirdAt, Generate: true})
	afterSevenDayRelease := engine.dollarSpendSnapshot(policy, requestedAt.Add(sevenDayWindow)).SevenDay
	if afterSevenDayRelease.SpentUSD != 0 || afterSevenDayRelease.RefreshAt != nil || afterSevenDayRelease.CoolingUntil != nil {
		t.Fatalf("seven-day spend at the fixed cycle boundary = %+v, want a full reset", afterSevenDayRelease)
	}
	fourthAt := requestedAt.Add(sevenDayWindow + time.Second)
	fourth := engine.Admit("managed-key", "gpt-5", fourthAt, candidates)
	if !fourth.Allowed {
		t.Fatalf("fourth admission = %+v, want new five-hour and seven-day cycles", fourth)
	}
	cycles = engine.budgetCycles(policy.ID)
	if !cycles.FiveHourStartedAt.Equal(fourthAt.Truncate(time.Millisecond)) || !cycles.SevenDayStartedAt.Equal(fourthAt.Truncate(time.Millisecond)) {
		t.Fatalf("cycles after the seven-day boundary = %+v", cycles)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: fourth.AuthID, Model: "gpt-5", RequestedAt: fourthAt, Generate: true})
}

func TestProviderTokenSemanticsProduceNonOverlappingBillableBuckets(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{Model: "test", InputUSDPerMillion: 10, CachedUSDPerMillion: 2.5, OutputUSDPerMillion: 40})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		record     CompletedUsage
		wantTokens normalizedUsageTokens
		wantCost   int64
	}{
		{name: "Codex cached and reasoning subsets", record: CompletedUsage{Provider: "codex", InputTokens: 1_000_000, CacheReadTokens: 250_000, OutputTokens: 100_000, ReasoningTokens: 50_000}, wantTokens: normalizedUsageTokens{Input: 750_000, Cached: 250_000, Output: 100_000}, wantCost: 12_125_000},
		{name: "OpenAI-compatible cached and reasoning subsets", record: CompletedUsage{ExecutorType: "OpenAICompatExecutor", InputTokens: 1_000_000, CacheReadTokens: 250_000, OutputTokens: 100_000, ReasoningTokens: 50_000}, wantTokens: normalizedUsageTokens{Input: 750_000, Cached: 250_000, Output: 100_000}, wantCost: 12_125_000},
		{name: "Claude independent cache and reasoning", record: CompletedUsage{ExecutorType: "ClaudeExecutor", InputTokens: 1_000_000, CacheReadTokens: 250_000, OutputTokens: 100_000, ReasoningTokens: 50_000}, wantTokens: normalizedUsageTokens{Input: 1_000_000, Cached: 250_000, Output: 150_000}, wantCost: 16_625_000},
		{name: "Gemini separate reasoning", record: CompletedUsage{Provider: "gemini", InputTokens: 1_000_000, CacheReadTokens: 250_000, OutputTokens: 100_000, ReasoningTokens: 50_000}, wantTokens: normalizedUsageTokens{Input: 750_000, Cached: 250_000, Output: 150_000}, wantCost: 14_125_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cost, tokens := costBreakdownForUsage(rate, test.record)
			if tokens != test.wantTokens || cost.Total != test.wantCost {
				t.Fatalf("tokens=%+v cost=%+v, want tokens=%+v cost=%d", tokens, cost, test.wantTokens, test.wantCost)
			}
		})
	}
}

func TestUsageCallbackAliasSettlesAgainstRequestedRateName(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "client-alias", OutputUSDPerMillion: 10}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	captureID := engine.CaptureRequestContent("managed-key", "client-alias", "application/json", []byte(`{"prompt":"alias request"}`), now)
	admission := engine.AdmitCaptured("managed-key", "upstream-model", captureID, now, []SchedulerCandidate{{AuthID: "account-a"}})
	if !admission.Allowed {
		t.Fatalf("alias admission = %+v", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: admission.AuthID, Model: "upstream-model", Alias: "client-alias",
		Provider: "codex", RequestedAt: now, Generate: true, OutputTokens: 100_000, TotalTokens: 100_000})
	if err := engine.flushPending(); err != nil {
		t.Fatal(err)
	}
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil || len(logs) != 1 || logs[0].Model != "client-alias" || logs[0].RequestContent != "alias request" || logs[0].CostMicros != 1_000_000 {
		t.Fatalf("alias logs = %+v, err=%v", logs, err)
	}
}

func TestManagedDollarAdmissionUsesCPAAuthWithoutAccountPool(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", OutputUSDPerMillion: 10}}); err != nil {
		t.Fatalf("ReplaceModelRates() error = %v", err)
	}
	now := time.Now().UTC().Truncate(time.Minute)
	candidates := []SchedulerCandidate{{AuthID: "cpa-auth-without-pool"}}
	admission := engine.Admit("managed-key", "gpt-5", now, candidates)
	if !admission.Allowed || admission.AuthID != candidates[0].AuthID {
		t.Fatalf("Admit() = %+v, want direct CPA candidate admission without account pool", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: admission.AuthID, Model: "gpt-5", RequestedAt: now,
		Generate: true, OutputTokens: 100_000, TotalTokens: 100_000,
	})
	spend := engine.dollarSpendSnapshot(engine.Policies()[0], now.Add(time.Second)).FiveHour
	if spend.SpentUSD != 1 {
		t.Fatalf("direct CPA candidate spend = %+v, want $1.00", spend)
	}
	// Actual Token summaries are management-plane SQLite reads, so persist the
	// terminal callback before asserting the durable aggregate.
	if err := engine.flushPending(); err != nil {
		t.Fatalf("flush direct CPA candidate usage = %v", err)
	}
	summary, err := engine.SummaryWithActualTokens(now.Add(time.Second))
	if err != nil {
		t.Fatalf("SummaryWithActualTokens() error = %v", err)
	}
	if len(summary.Keys) != 1 || !summary.Keys[0].ActualTokens.Available || summary.Keys[0].ActualTokens.Output != 100_000 || summary.Keys[0].ActualTokens.Total != 100_000 {
		t.Fatalf("direct CPA candidate actual Token totals = %+v", summary.Keys)
	}
}

func TestPendingCallbackCheckpointSurvivesPluginReload(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	secret := "test-only-hmac-secret-with-at-least-32-characters"
	path := filepath.Join(t.TempDir(), "codex-carpool.db")
	policy := KeyPolicy{ID: "managed", Name: "Managed", KeySHA256: FingerprintAPIKey("managed-key", secret), Enabled: true}
	cfg, err := NormalizeConfig(Config{DatabasePath: path, KeyHMACSecret: secret, RecordRetention: "168h", BootstrapKeys: []KeyPolicy{policy}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", OutputUSDPerMillion: 10}}); err != nil {
		t.Fatal(err)
	}
	requestedAt := time.Now().UTC().Truncate(time.Second)
	admission := engine.Admit("managed-key", "gpt-5", requestedAt, []SchedulerCandidate{{AuthID: "account-a"}})
	if !admission.Allowed {
		t.Fatalf("Admit() = %+v", admission)
	}
	if err := engine.CloseConservatively(); err != nil {
		t.Fatalf("CloseConservatively() = %v", err)
	}

	reopened, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open(reloaded) = %v", err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.PendingSettlementCount() != 1 {
		t.Fatalf("reloaded pending callbacks = %d, want 1", reopened.PendingSettlementCount())
	}
	cycles := reopened.budgetCycles("managed")
	wantCycleStart := requestedAt.Truncate(time.Millisecond)
	if !cycles.FiveHourStartedAt.Equal(wantCycleStart) || !cycles.SevenDayStartedAt.Equal(wantCycleStart) {
		t.Fatalf("reloaded budget cycles = %+v, want both anchored at %v", cycles, wantCycleStart)
	}
	reopened.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: requestedAt,
		Generate: true, OutputTokens: 100_000, TotalTokens: 100_000})
	if reopened.PendingSettlementCount() != 0 {
		t.Fatalf("settled pending callbacks = %d, want 0", reopened.PendingSettlementCount())
	}
	spend := reopened.dollarSpendSnapshot(reopened.Policies()[0], requestedAt.Add(time.Second)).FiveHour
	if spend.SpentUSD != 1 || spend.RefreshAt == nil || !spend.RefreshAt.Equal(wantCycleStart.Add(fiveHourWindow)) {
		t.Fatalf("reloaded request-time rate spend = %+v, want $1 and original fixed-cycle boundary", spend)
	}
}

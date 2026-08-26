package quota

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type modelRateRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn modelRateRoundTripFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

// Build mocked headers through net/http so their keys have the same canonical
// spelling as headers returned by a real HTTP transport.
func modelRateResponseHeader(name, value string) http.Header {
	header := make(http.Header)
	header.Set(name, value)
	return header
}

func TestModelsDevCompleteRateProfileAndProviderMatch(t *testing.T) {
	raw := []byte(`{
  "openai": {"id":"openai","name":"OpenAI","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{
    "input":2,"output":12,"cache_read":0.2,"cache_write":2.5,
    "tiers":[{"tier":{"size":272000},"input":4,"output":18,"cache_read":0.4,"cache_write":5}]
  },"experimental":{"modes":{"fast":{"cost":{"input":4,"output":24,"cache_read":0.4,"cache_write":5},"provider":{"body":{"service_tier":"priority"}}}}}}}},
  "relay": {"id":"relay","name":"Relay","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":99,"output":99}}}}
}`)
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal(raw, &providers); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 1, 2, 3, 0, time.UTC)
	rates, unmatched := modelRatesFromModelsDev([]ModelCatalogEntry{
		{ID: "gpt-5.6-terra", Owner: "OpenAI", Available: true},
		{ID: "manual-alias", Owner: "OpenAI", Available: true},
	}, providers, now)
	if len(rates) != 1 || unmatched != 1 {
		t.Fatalf("rates=%+v unmatched=%d", rates, unmatched)
	}
	rate := rates[0]
	if rate.Provider != "openai" || rate.Source != "models.dev" || rate.CacheReadUSDPerMillion != 0.2 || rate.CacheWriteUSDPerMillion != 2.5 || len(rate.Tiers) != 1 || len(rate.Modes) != 1 || rate.Modes[0].ServiceTier != "priority" {
		t.Fatalf("complete rate profile = %+v", rate)
	}
}

func TestProviderOwnerMatchingDoesNotUseBroadPrefixes(t *testing.T) {
	if providerMatchesOwner(normalizedProviderID("openai-compatible"), "openai") {
		t.Fatal("openai-compatible owner incorrectly matched the OpenAI provider by prefix")
	}
	if !providerMatchesOwner(normalizedProviderID("Claude"), "anthropic") || !providerMatchesOwner(normalizedProviderID("Grok"), "xai") {
		t.Fatal("canonical provider aliases did not match exactly")
	}
}

func TestCompleteBillingUsesCacheWriteContextTierAndServiceMode(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{
		Model: "gpt-5.6-terra", InputUSDPerMillion: 2, CacheReadUSDPerMillion: .2, CacheWriteUSDPerMillion: 2.5, OutputUSDPerMillion: 12,
		Tiers: []ModelRateTier{{ContextOverTokens: 272_000, InputUSDPerMillion: 4, CacheReadUSDPerMillion: .4, CacheWriteUSDPerMillion: 5, OutputUSDPerMillion: 18}},
		Modes: []ModelRateMode{{Name: "fast", ServiceTier: "priority", InputUSDPerMillion: 4, CacheReadUSDPerMillion: .4, CacheWriteUSDPerMillion: 5, OutputUSDPerMillion: 24}},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := CompletedUsage{Provider: "openai", InputTokens: 300_000, CacheReadTokens: 100_000, CacheCreationTokens: 50_000, OutputTokens: 10_000}
	cost, tokens := costBreakdownForUsage(rate, record)
	if tokens.Input != 150_000 || tokens.CacheRead != 100_000 || tokens.CacheWrite != 50_000 || cost.Total != 1_070_000 {
		t.Fatalf("standard tier tokens=%+v cost=%+v", tokens, cost)
	}
	record.ServiceTier = "priority"
	cost, _ = costBreakdownForUsage(rate, record)
	if cost.Total != 1_130_000 {
		t.Fatalf("priority mode cost=%+v, want 1130000", cost)
	}
	record.ServiceTier = "auto"
	cost, _ = costBreakdownForUsage(rate, record)
	if cost.Total != 1_070_000 {
		t.Fatalf("auto tier must use the base/tier rate when CPA does not report the final service tier: cost=%+v", cost)
	}
}

func TestReasoningPriceAppliesOnlyToSeparatelyReportedReasoning(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{Model: "reasoning", InputUSDPerMillion: 10, ReasoningUSDPerMillion: 7, OutputUSDPerMillion: 40})
	if err != nil {
		t.Fatal(err)
	}
	cost, tokens := costBreakdownForUsage(rate, CompletedUsage{Provider: "anthropic", InputTokens: 100_000, OutputTokens: 10_000, ReasoningTokens: 20_000})
	if tokens.Reasoning != 20_000 || cost.Reasoning != 140_000 || cost.Total != 1_540_000 {
		t.Fatalf("tokens=%+v cost=%+v", tokens, cost)
	}
}

func TestModelsDevSyncUpdatesMatchesAndFailureRetainsRates(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5.6-terra", Owner: "OpenAI", Available: true}, {ID: "gpt-retired", Owner: "OpenAI", Available: true}, {ID: "manual-alias", Owner: "", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5.6-terra", InputUSDPerMillion: 9}, {Model: "manual-alias", InputUSDPerMillion: 3}}); err != nil {
		t.Fatal(err)
	}
	payload := `{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12,"cache_read":0.2,"cache_write":2.5}},"gpt-retired":{"id":"gpt-retired","cost":{"input":1,"output":4}}}}}`
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: modelRateResponseHeader("ETag", `"rate-v1"`), Body: io.NopCloser(strings.NewReader(payload))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	rates := engine.ModelRates()
	if len(rates) != 3 || rates[0].Model != "gpt-5.6-terra" || rates[0].Source != "models.dev" || rates[1].Model != "gpt-retired" || rates[1].Source != "models.dev" || rates[2].Model != "manual-alias" || rates[2].InputUSDPerMillion != 3 {
		t.Fatalf("rates after successful sync = %+v", rates)
	}
	withoutRetired := `{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12,"cache_read":0.2,"cache_write":2.5}}}}}`
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: modelRateResponseHeader("ETag", `"rate-v2"`), Body: io.NopCloser(strings.NewReader(withoutRetired))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	rates = engine.ModelRates()
	if len(rates) != 2 || rates[0].Model != "gpt-5.6-terra" || rates[1].Model != "manual-alias" || engine.ModelRateSyncStatus().RetiredModels != 1 {
		t.Fatalf("stale synchronized rate was not retired or manual rate changed: rates=%+v status=%+v", rates, engine.ModelRateSyncStatus())
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("If-None-Match") != `"rate-v2"` {
			t.Fatalf("If-None-Match = %q", request.Header.Get("If-None-Match"))
		}
		return &http.Response{StatusCode: http.StatusNotModified, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if engine.ModelRateSyncStatus().RetiredModels != 0 {
		t.Fatalf("304 sync repeated a previous retired count: %+v", engine.ModelRateSyncStatus())
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadGateway, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err == nil {
		t.Fatal("failed refresh unexpectedly succeeded")
	}
	after := engine.ModelRates()
	if len(after) != 2 || after[0].InputUSDPerMillion != 2 || engine.ModelRateSyncStatus().LastError == "" {
		t.Fatalf("failed sync changed rates or omitted status: rates=%+v status=%+v", after, engine.ModelRateSyncStatus())
	}
}

func TestReenablingModelsDevSyncForcesFullReconciliation(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5.6-terra", Owner: "OpenAI", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5.6-terra", Source: "manual", InputUSDPerMillion: 99}}); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncMu.Lock()
	engine.rateSyncStatus = ModelRateSyncStatus{Enabled: false, ETag: `"old"`, CatalogFingerprint: modelCatalogFingerprint([]ModelCatalogEntry{{ID: "gpt-5.6-terra", Owner: "OpenAI", Available: true}})}
	engine.rateSyncMu.Unlock()
	engine.rateSyncClient = modelRateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("If-None-Match"); got != "" {
			t.Fatalf("re-enabled synchronization sent stale If-None-Match %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"openai":{"id":"openai","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12}}}}}`))}, nil
	})
	status, err := engine.SetModelRateSyncEnabled(true)
	if err != nil {
		t.Fatal(err)
	}
	rate, found := engine.modelRate("gpt-5.6-terra")
	if !found || rate.Source != "models.dev" || rate.InputUSDPerMillion != 2 || status.LastError != "" {
		t.Fatalf("re-enabled synchronization did not reconcile rates: rate=%+v found=%t status=%+v", rate, found, status)
	}
}

func TestRequestedModelRateSyncRunsWithoutWaitingForSchedule(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", Owner: "OpenAI", Available: true}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	engine.rateSyncMu.Lock()
	engine.rateSyncStatus = ModelRateSyncStatus{Enabled: true, LastSuccess: timePointer(now)}
	engine.rateSyncMu.Unlock()
	requested := make(chan struct{}, 1)
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requested <- struct{}{}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":1,"output":4}}}}}`))}, nil
	})
	engine.RequestModelRateSync()
	select {
	case <-requested:
	case <-time.After(time.Second):
		t.Fatal("requested synchronization did not wake the managed loop")
	}
}

func TestRateSyncTogglePublishesOnlyAfterPersistenceSucceeds(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if _, err := engine.store.db.Exec(`DROP TABLE plugin_metadata`); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SetModelRateSyncEnabled(true); err == nil {
		t.Fatal("rate synchronization toggle unexpectedly succeeded without metadata storage")
	}
	if engine.ModelRateSyncStatus().Enabled {
		t.Fatal("failed persistence leaked the enabled state into memory")
	}
}

func TestSynchronizedRatesAndStatusRollbackTogether(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", Owner: "OpenAI", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", InputUSDPerMillion: 9}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.db.Exec(`DROP TABLE plugin_metadata`); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":1,"output":4}}}}}`))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err == nil {
		t.Fatal("synchronization unexpectedly succeeded without metadata storage")
	}
	rate, found := engine.modelRate("gpt-5")
	if !found || rate.InputUSDPerMillion != 9 {
		t.Fatalf("failed atomic synchronization changed the in-memory rate: found=%t rate=%+v", found, rate)
	}
	rates, err := engine.store.ListModelRates()
	if err != nil || len(rates) != 1 || rates[0].InputUSDPerMillion != 9 {
		t.Fatalf("failed atomic synchronization changed the stored rate: rates=%+v err=%v", rates, err)
	}
}

func TestManualRateReplacementSharesSynchronizationWriteLock(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	engine.rateSyncRunMu.Lock()
	started, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := engine.ReplaceModelRates([]ModelRate{{Model: "manual", InputUSDPerMillion: 1}})
		finished <- err
	}()
	<-started
	select {
	case err := <-finished:
		engine.rateSyncRunMu.Unlock()
		t.Fatalf("manual replacement bypassed synchronization lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	engine.rateSyncRunMu.Unlock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestTierStartsAtDeclaredContextSize(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{
		Model: "tiered", InputUSDPerMillion: 1, OutputUSDPerMillion: 1,
		Tiers: []ModelRateTier{{ContextOverTokens: 100, InputUSDPerMillion: 2, OutputUSDPerMillion: 3}},
	})
	if err != nil {
		t.Fatal(err)
	}
	cost, _ := costBreakdownForUsage(rate, CompletedUsage{Provider: "openai", InputTokens: 100})
	if cost.Total != 200 {
		t.Fatalf("tier boundary cost = %d, want 200", cost.Total)
	}
}

func TestOpenAIReasoningSubsetUsesDedicatedRateWithoutDoubleBilling(t *testing.T) {
	rate, err := normalizeModelRate(ModelRate{Model: "gpt-reasoning", ReasoningUSDPerMillion: 7, OutputUSDPerMillion: 40})
	if err != nil {
		t.Fatal(err)
	}
	cost, tokens := costBreakdownForUsage(rate, CompletedUsage{Provider: "openai", OutputTokens: 100_000, ReasoningTokens: 25_000})
	if tokens.Reasoning != 25_000 || tokens.Output != 75_000 || cost.Reasoning != 175_000 || cost.Output != 3_000_000 || cost.Total != 3_175_000 {
		t.Fatalf("OpenAI reasoning split tokens=%+v cost=%+v", tokens, cost)
	}
}

func TestModelsDevSyncRetiresLastStaleRateButRejectsEmptyUpstreamCatalog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-old", Owner: "OpenAI", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{
		{Model: "gpt-old", Source: "models.dev", InputUSDPerMillion: 2, OutputUSDPerMillion: 10},
		{Model: "manual-alias", Source: "manual", InputUSDPerMillion: 3},
	}); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"openai":{"id":"openai","models":{"another-model":{"id":"another-model","cost":{"input":1,"output":4}}}}}`))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	rates := engine.ModelRates()
	if len(rates) != 1 || rates[0].Model != "manual-alias" || engine.ModelRateSyncStatus().RetiredModels != 1 {
		t.Fatalf("last stale synchronized rate was not retired safely: rates=%+v status=%+v", rates, engine.ModelRateSyncStatus())
	}

	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-old", Source: "models.dev", InputUSDPerMillion: 2}}); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err == nil {
		t.Fatal("empty upstream catalog unexpectedly succeeded")
	}
	rates = engine.ModelRates()
	if len(rates) != 1 || rates[0].Model != "gpt-old" {
		t.Fatalf("empty upstream catalog changed synchronized rates: %+v", rates)
	}
}

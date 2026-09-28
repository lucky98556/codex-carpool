package quota

import (
	"crypto/sha256"
	"encoding/hex"
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

func modelRateCatalog(providers string, canonicalIDs ...string) string {
	models := make(map[string]json.RawMessage, len(canonicalIDs))
	for _, id := range canonicalIDs {
		models[id] = json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(modelsDevCatalog{Models: models, Providers: decodeTestProviders(providers)})
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func decodeTestProviders(raw string) map[string]modelsDevProvider {
	var providers map[string]modelsDevProvider
	if err := json.Unmarshal([]byte(raw), &providers); err != nil {
		panic(err)
	}
	return providers
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
	}, modelsDevCatalog{Models: map[string]json.RawMessage{"openai/gpt-5.6-terra": json.RawMessage(`{}`)}, Providers: providers}, now)
	if len(rates) != 1 || unmatched != 1 {
		t.Fatalf("rates=%+v unmatched=%d", rates, unmatched)
	}
	rate := rates[0]
	if rate.Provider != "openai" || rate.Source != "models.dev" || rate.CacheReadUSDPerMillion != 0.2 || rate.CacheWriteUSDPerMillion != 2.5 || len(rate.Tiers) != 1 || len(rate.Modes) != 1 || rate.Modes[0].ServiceTier != "priority" {
		t.Fatalf("complete rate profile = %+v", rate)
	}
}

func TestModelsDevSyncIncludesManuallyAddedNonGPTModels(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", Owner: "OpenAI", Available: true}, {ID: "claude-opus-4-6", Owner: "Claude", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{
		{Model: "claude-sonnet-4-6", InputUSDPerMillion: 99},
		{Model: "gemini-2.5-flash", InputUSDPerMillion: 99},
		{Model: "manual-alias", InputUSDPerMillion: 7},
	}); err != nil {
		t.Fatal(err)
	}
	response := modelRateCatalog(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":2,"output":10}}}},"anthropic":{"id":"anthropic","models":{"claude-sonnet-4-6":{"id":"claude-sonnet-4-6","cost":{"input":3,"output":15}},"claude-opus-4-6":{"id":"claude-opus-4-6","cost":{"input":5,"output":25}}}},"reseller":{"id":"reseller","models":{"claude-sonnet-4-6":{"id":"claude-sonnet-4-6","cost":{"input":99,"output":99}},"gemini-2.5-flash":{"id":"gemini-2.5-flash","cost":{"input":99,"output":99}}}},"google":{"id":"google","models":{"gemini-2.5-flash":{"id":"gemini-2.5-flash","cost":{"input":0.3,"output":2.5}}}}}`, "openai/gpt-5", "anthropic/claude-sonnet-4-6", "anthropic/claude-opus-4-6", "google/gemini-2.5-flash")
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		model, provider string
		input           float64
	}{
		{"claude-opus-4-6", "anthropic", 5},
		{"claude-sonnet-4-6", "anthropic", 3},
		{"gemini-2.5-flash", "google", 0.3},
		{"gpt-5", "openai", 2},
	} {
		rate, found := engine.modelRate(expected.model)
		if !found || rate.Provider != expected.provider || rate.Source != "models.dev" || rate.InputUSDPerMillion != expected.input {
			t.Fatalf("%s synchronized rate = %+v, found=%t", expected.model, rate, found)
		}
	}
	if rate, found := engine.modelRate("manual-alias"); !found || rate.Source != "manual" || rate.InputUSDPerMillion != 7 {
		t.Fatalf("unmatched manual alias changed: %+v, found=%t", rate, found)
	}
	if status := engine.ModelRateSyncStatus(); status.MatchedModels != 4 || status.UnmatchedModels != 1 {
		t.Fatalf("rate sync status = %+v", status)
	}
}

func TestModelsDevSyncUsesFirstPartyPricesForCPAGeminiAndDeepSeekModels(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	// The shared fixture's manual gpt-5 rate is unrelated to this catalog.
	if _, err := engine.ReplaceModelRates(nil); err != nil {
		t.Fatal(err)
	}
	// CPA's owner describes the access channel, not necessarily the model vendor.
	if err := engine.ReplaceModels([]ModelCatalogEntry{
		{ID: "gemini-2.5-flash", Owner: "antigravity", Available: true},
		{ID: "deepseek-v4-flash", Owner: "openai-compatibility", Available: true},
	}); err != nil {
		t.Fatal(err)
	}
	response := modelRateCatalog(`{"google":{"id":"google","models":{"gemini-2.5-flash":{"id":"gemini-2.5-flash","cost":{"input":0.3,"output":2.5}}}},"deepseek":{"id":"deepseek","models":{"deepseek-v4-flash":{"id":"deepseek-v4-flash","cost":{"input":0.15,"output":0.6}}}},"reseller":{"id":"reseller","models":{"gemini-2.5-flash":{"id":"gemini-2.5-flash","cost":{"input":99,"output":99}},"deepseek-v4-flash":{"id":"deepseek-v4-flash","cost":{"input":99,"output":99}}}}}`, "google/gemini-2.5-flash", "deepseek/deepseek-v4-flash")
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		model, provider string
		input           float64
	}{
		{"gemini-2.5-flash", "google", 0.3},
		{"deepseek-v4-flash", "deepseek", 0.15},
	} {
		rate, found := engine.modelRate(expected.model)
		if !found || rate.Provider != expected.provider || rate.InputUSDPerMillion != expected.input {
			t.Fatalf("CPA model %s synchronized rate = %+v, found=%t", expected.model, rate, found)
		}
	}
	if status := engine.ModelRateSyncStatus(); status.MatchedModels != 2 || status.UnmatchedModels != 0 {
		t.Fatalf("CPA model rate sync status = %+v", status)
	}
}

func TestRemovedCPAModelRetiresButManualAdditionKeepsSyncing(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "cpa-model", Available: true}, {ID: "remaining-model", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "manual-model", Source: "manual"}}); err != nil {
		t.Fatal(err)
	}
	response := modelRateCatalog(`{"new-lab":{"models":{"cpa-model":{"id":"cpa-model","cost":{"input":1,"output":2}},"remaining-model":{"id":"remaining-model","cost":{"input":1,"output":2}},"manual-model":{"id":"manual-model","cost":{"input":1,"output":2}}}}}`, "new-lab/cpa-model", "new-lab/remaining-model", "new-lab/manual-model")
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rate, found := engine.modelRate("manual-model"); !found || !rate.RateCardOnly || rate.Source != "models.dev" {
		t.Fatalf("manual model origin was not retained after sync: %+v, found=%t", rate, found)
	}
	stored, err := engine.store.ListModelRates()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 || stored[0].Model != "cpa-model" || stored[0].RateCardOnly || stored[1].Model != "manual-model" || !stored[1].RateCardOnly {
		t.Fatalf("manual origin was not persisted: %+v", stored)
	}
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "remaining-model", Available: true}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, found := engine.modelRate("cpa-model"); found {
		t.Fatal("removed CPA model still has a synchronized rate")
	}
	if rate, found := engine.modelRate("manual-model"); !found || !rate.RateCardOnly || rate.Source != "models.dev" {
		t.Fatalf("manual model stopped synchronizing after CPA catalog update: %+v, found=%t", rate, found)
	}
}

func TestModelCatalogFingerprintChangesAfterFirstPartyMatchingUpgrade(t *testing.T) {
	model := ModelCatalogEntry{ID: "gemini-2.5-flash", Owner: "antigravity", Available: true}
	oldFingerprint := sha256.Sum256([]byte(model.ID + "\x00" + normalizedProviderID(model.Owner)))
	if got := modelCatalogFingerprint([]ModelCatalogEntry{model}); got == hex.EncodeToString(oldFingerprint[:]) {
		t.Fatal("old ETag would skip reconciliation with the upgraded matching rules")
	}
}

func TestModelsDevManualModelAdditionInvalidatesCatalogETag(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	if err := engine.ReplaceModels([]ModelCatalogEntry{{ID: "gpt-5", Owner: "OpenAI", Available: true}}); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: modelRateResponseHeader("ETag", `"prices-v1"`), Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":2,"output":10}}}}}`, "openai/gpt-5")))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", Source: "models.dev", InputUSDPerMillion: 2}, {Model: "claude-sonnet-4-6", Source: "manual", InputUSDPerMillion: 9}}); err != nil {
		t.Fatal(err)
	}
	engine.rateSyncClient = modelRateRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("If-None-Match"); got != "" {
			t.Fatalf("new manual model reused stale ETag %q", got)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"anthropic":{"id":"anthropic","models":{"claude-sonnet-4-6":{"id":"claude-sonnet-4-6","cost":{"input":3,"output":15}}}}}`, "anthropic/claude-sonnet-4-6")))}, nil
	})
	if err := engine.SyncModelRates(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rate, found := engine.modelRate("claude-sonnet-4-6"); !found || rate.Source != "models.dev" || rate.InputUSDPerMillion != 3 {
		t.Fatalf("new manual model was not synchronized: %+v, found=%t", rate, found)
	}
}

func TestModelsDevDoesNotUseResellerPriceWhenFirstPartyPriceIsMissing(t *testing.T) {
	candidates := modelRateSyncCandidates([]ModelCatalogEntry{{ID: "claude-sonnet-4-6", Available: false}}, []ModelRate{{Model: "gpt-seed"}, {Model: "claude-sonnet-4-6", Source: "manual"}})
	if len(candidates) != 2 || candidates[1].Owner != "" || !candidates[1].Available {
		t.Fatalf("manual model candidates = %+v", candidates)
	}
	price := 99.0
	providers := map[string]modelsDevProvider{"reseller": {Models: map[string]modelsDevModel{
		"claude-sonnet-4-6": {ID: "claude-sonnet-4-6", Cost: modelsDevCost{Input: &price, Output: &price}},
	}}}
	if rates, unmatched := modelRatesFromModelsDev(candidates, modelsDevCatalog{Models: map[string]json.RawMessage{"anthropic/claude-sonnet-4-6": json.RawMessage(`{}`)}, Providers: providers}, time.Now()); len(rates) != 0 || unmatched != 1 {
		t.Fatalf("first-party price missing: rates=%+v unmatched=%d", rates, unmatched)
	}
}

func TestCanonicalModelProvidersRejectsAmbiguousIDs(t *testing.T) {
	providers := canonicalModelProviders(map[string]json.RawMessage{
		"lab-a/shared-model": json.RawMessage(`{}`),
		"lab-b/shared-model": json.RawMessage(`{}`),
	})
	if providers["shared-model"] != "" {
		t.Fatalf("ambiguous model matched provider %q", providers["shared-model"])
	}
}

func TestModelsDevMatchesNewLabWithoutCodeMapping(t *testing.T) {
	upstream := modelsDevCatalog{
		Models: map[string]json.RawMessage{"new-lab/orbit-1": json.RawMessage(`{}`)},
		Providers: decodeTestProviders(`{"new-lab":{"models":{"orbit-1":{"id":"orbit-1","cost":{"input":2,"output":8}}}},"reseller":{"models":{"orbit-1":{"id":"orbit-1","cost":{"input":99,"output":99}}}}}`),
	}
	rates, unmatched := modelRatesFromModelsDev([]ModelCatalogEntry{{ID: "orbit-1", Owner: "openai-compatibility", Available: true}}, upstream, time.Now())
	if len(rates) != 1 || unmatched != 0 || rates[0].Provider != "new-lab" || rates[0].InputUSDPerMillion != 2 {
		t.Fatalf("new lab rate = %+v, unmatched=%d", rates, unmatched)
	}
}

func TestModelsDevLabProviderIDFormattingMustBeUnambiguous(t *testing.T) {
	providers := decodeTestProviders(`{"xai":{"models":{"grok-4":{"id":"grok-4","cost":{"input":3,"output":15}}}},"relay":{"models":{"grok-4":{"id":"grok-4","cost":{"input":99,"output":99}}}}}`)
	upstream := modelsDevCatalog{Models: map[string]json.RawMessage{"x-ai/grok-4": json.RawMessage(`{}`)}, Providers: providers}
	rates, unmatched := modelRatesFromModelsDev([]ModelCatalogEntry{{ID: "grok-4", Available: true}}, upstream, time.Now())
	if len(rates) != 1 || unmatched != 0 || rates[0].Provider != "xai" || rates[0].InputUSDPerMillion != 3 {
		t.Fatalf("normalized lab match = %+v, unmatched=%d", rates, unmatched)
	}
	providers["x_ai"] = providers["xai"]
	if rates, unmatched = modelRatesFromModelsDev([]ModelCatalogEntry{{ID: "grok-4", Available: true}}, upstream, time.Now()); len(rates) != 0 || unmatched != 1 {
		t.Fatalf("ambiguous normalized provider match = %+v, unmatched=%d", rates, unmatched)
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
	payload := modelRateCatalog(`{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12,"cache_read":0.2,"cache_write":2.5}},"gpt-retired":{"id":"gpt-retired","cost":{"input":1,"output":4}}}}}`, "openai/gpt-5.6-terra", "openai/gpt-retired")
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
	withoutRetired := modelRateCatalog(`{"openai":{"id":"openai","name":"OpenAI","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12,"cache_read":0.2,"cache_write":2.5}}}}}`, "openai/gpt-5.6-terra", "openai/gpt-retired")
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
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"openai":{"id":"openai","models":{"gpt-5.6-terra":{"id":"gpt-5.6-terra","cost":{"input":2,"output":12}}}}}`, "openai/gpt-5.6-terra")))}, nil
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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":1,"output":4}}}}}`, "openai/gpt-5")))}, nil
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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"openai":{"id":"openai","models":{"gpt-5":{"id":"gpt-5","cost":{"input":1,"output":4}}}}}`, "openai/gpt-5")))}, nil
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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(modelRateCatalog(`{"openai":{"id":"openai","models":{"another-model":{"id":"another-model","cost":{"input":1,"output":4}}}}}`, "openai/another-model", "openai/gpt-old")))}, nil
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

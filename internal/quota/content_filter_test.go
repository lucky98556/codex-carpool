package quota

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestContentFilterUsesLiteralCaseInsensitiveMatching(t *testing.T) {
	matcher := compileContentFilterMatcher([]ContentFilterTerm{
		{ID: "custom-phrase", Value: "Blocked Phrase", Category: "custom", Source: contentFilterSourceCustom, Enabled: true},
		{ID: "custom-literal", Value: "a.*b", Category: "custom", Source: contentFilterSourceCustom, Enabled: true},
	})
	match := matcher.Match("prefix BLOCKED\n\tPHRASE suffix")
	if !match.Matched || match.Term != "Blocked Phrase" || match.Category != "custom" {
		t.Fatalf("case-insensitive literal match = %+v", match)
	}
	if match := matcher.Match("aXXb"); match.Matched {
		t.Fatalf("regex syntax must stay literal, got %+v", match)
	}
	if match := matcher.Match("literal a.*b value"); !match.Matched || match.Term != "a.*b" {
		t.Fatalf("literal metacharacter match = %+v", match)
	}
}

func TestContentFilterDefaultsOffAndValidatesBoundedTerms(t *testing.T) {
	settings, err := normalizeContentFilterSettings(ContentFilterSettings{})
	if err != nil {
		t.Fatalf("normalizeContentFilterSettings() = %v", err)
	}
	if settings.Enabled || len(settings.Terms) != len(builtinContentFilterTerms) {
		t.Fatalf("default content-filter settings = %+v", settings)
	}
	if _, err := normalizeContentFilterSettings(ContentFilterSettings{Terms: []ContentFilterTerm{{
		ID: "custom-long-category", Value: "blocked", Category: strings.Repeat("c", maxContentFilterCategoryRunes+1), Enabled: true,
	}}}); err == nil {
		t.Fatal("overlong content-filter category error = nil")
	}
	if _, err := normalizeContentFilterSettings(ContentFilterSettings{Terms: []ContentFilterTerm{{
		ID: "custom-duplicate-one", Value: "Same Phrase", Enabled: true,
	}, {
		ID: "custom-duplicate-two", Value: " same   phrase ", Enabled: true,
	}}}); err == nil {
		t.Fatal("normalized duplicate content-filter term error = nil")
	}
}

func TestContentFilterBlocksBeforeModelAndUsesDedicatedLogs(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{
		ID: "managed", Name: "Managed Key", KeySuffix: "MeEL", AllocationX: 1, Enabled: true,
		AllowedModels: []string{"allowed-model"},
	})
	defer engine.Close()
	settings, err := engine.ConfigureContentFilter(ContentFilterSettings{Enabled: true, Terms: []ContentFilterTerm{{
		ID: "custom-blocked-phrase", Value: "blocked phrase", Category: "custom", Enabled: true,
	}}})
	if err != nil {
		t.Fatalf("ConfigureContentFilter() = %v", err)
	}
	if !settings.Enabled || len(settings.Terms) <= 1 {
		t.Fatalf("configured content-filter settings = %+v", settings)
	}

	now := time.Now().UTC()
	captureID := engine.CaptureRequestContent("managed-key", "denied-model", []byte(`{"messages":[{"role":"user","content":"Please check this BLOCKED   PHRASE now"}]}`), now)
	if captureID == "" {
		t.Fatal("CaptureRequestContent() returned an empty correlation id")
	}
	admission := engine.AdmitCaptured("managed-key", "denied-model", captureID, now)
	if admission.Allowed || admission.Code != "content_forbidden" || admission.KeyID != "managed" {
		t.Fatalf("forbidden admission = %+v", admission)
	}
	page, err := engine.DecisionLogPage("", "forbidden", "Managed Key", 1, 10)
	if err != nil {
		t.Fatalf("DecisionLogPage(forbidden) = %v", err)
	}
	if page.Total != 1 || len(page.Logs) != 1 {
		t.Fatalf("forbidden logs = %+v", page)
	}
	log := page.Logs[0]
	if log.KeyID != "managed" || log.KeySuffix != "MeEL" || log.StatusCode != 403 || log.MatchedTerm != "blocked phrase" || log.MatchedCategory != "custom" || !strings.Contains(log.RequestContent, "BLOCKED") {
		t.Fatalf("forbidden log = %+v", log)
	}
	if page, err := engine.DecisionLogPage("", "forbidden", "MeEL", 1, 10); err != nil || page.Total != 1 {
		t.Fatalf("search forbidden logs by CPA suffix = %+v, %v", page, err)
	}
	if err := engine.ClearForbiddenDecisionLogs(""); err != nil {
		t.Fatalf("ClearForbiddenDecisionLogs() = %v", err)
	}
	if page, err := engine.DecisionLogPage("", "forbidden", "", 1, 10); err != nil || page.Total != 0 {
		t.Fatalf("forbidden logs after clear = %+v, %v", page, err)
	}

	settings.Enabled = false
	if _, err := engine.ConfigureContentFilter(settings); err != nil {
		t.Fatalf("disable content filter = %v", err)
	}
	captureID = engine.CaptureRequestContent("managed-key", "denied-model", []byte(`{"messages":[{"role":"user","content":"blocked phrase"}]}`), now.Add(time.Second))
	admission = engine.AdmitCaptured("managed-key", "denied-model", captureID, now.Add(time.Second))
	if admission.Code != "model_not_allowed" {
		t.Fatalf("disabled filter admission = %+v, want model_not_allowed", admission)
	}
}

func TestManagedKeyPersistsOnlyOriginalCPASuffixForDisplay(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer engine.Close()
	policy := engine.Policies()[0]
	const rawCPAKey = "cpa-secret-value-MeEL"
	saved, err := engine.UpsertPolicy(policy, rawCPAKey)
	if err != nil {
		t.Fatalf("UpsertPolicy(rebind) = %v", err)
	}
	if saved.KeySuffix != "MeEL" {
		t.Fatalf("saved key suffix = %q, want MeEL", saved.KeySuffix)
	}
	saved.Name = "Renamed"
	saved.KeySuffix = ""
	saved, err = engine.UpsertPolicy(saved, "")
	if err != nil {
		t.Fatalf("UpsertPolicy(edit) = %v", err)
	}
	if saved.KeySuffix != "MeEL" {
		t.Fatalf("edited key suffix = %q, want preserved MeEL", saved.KeySuffix)
	}
	summary := engine.Summary(time.Now().UTC())
	if len(summary.Keys) != 1 || summary.Keys[0].KeySuffix != "MeEL" {
		t.Fatalf("summary key suffix = %+v", summary.Keys)
	}
	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("json.Marshal(summary) = %v", err)
	}
	if strings.Contains(string(raw), rawCPAKey) {
		t.Fatal("summary must never expose the full CPA API Key")
	}
}

package quota

import (
	"strings"
	"testing"
	"time"
)

func TestIPWhitelistNormalizationAndIndependentSwitch(t *testing.T) {
	base := KeyPolicy{ID: "managed", Name: "Managed", KeySHA256: strings.Repeat("a", 64)}
	base.IPWhitelist = []string{" 203.0.113.10 ", "198.51.100.27/24", "203.0.113.10", "2001:db8::/32"}
	policy, err := normalizePolicy(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(policy.IPWhitelist, ";"); got != "203.0.113.10;198.51.100.0/24;2001:db8::/32" {
		t.Fatalf("normalized whitelist = %q", got)
	}
	if !policy.AllowsIP("192.0.2.9") {
		t.Fatal("saved but disabled whitelist must not reject requests")
	}
	policy.IPWhitelistEnabled = true
	for _, address := range []string{"203.0.113.10", "198.51.100.75", "2001:db8::1"} {
		if !policy.AllowsIP(address) {
			t.Fatalf("allowed address %q was rejected", address)
		}
	}
	for _, address := range []string{"", "198.51.101.1", "2001:db9::1", "not-an-ip", "203.0.113.10,198.51.100.75"} {
		if policy.AllowsIP(address) {
			t.Fatalf("unlisted or invalid address %q was allowed", address)
		}
	}
	base.IPWhitelistEnabled = true
	base.IPWhitelist = nil
	if _, err := normalizePolicy(base); err == nil {
		t.Fatal("enabled empty whitelist must be rejected")
	}
	base.IPWhitelist = []string{"example.com"}
	if _, err := normalizePolicy(base); err == nil {
		t.Fatal("hostnames must not be accepted as IPs")
	}
}

func TestTrackOnlyIPWhitelistRejectsAndLogsButUnmanagedKeyBypasses(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: false,
		IPWhitelistEnabled: true, IPWhitelist: []string{"203.0.113.0/24"}})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Second)
	captureID := engine.CaptureRequestContent("managed-key", "gpt-5", "application/json", []byte(`{"prompt":"IP audit"}`), now)
	denied := engine.AdmitCapturedFromIP("managed-key", "gpt-5", captureID, "198.51.100.9", now)
	if denied.Allowed || denied.Bypass || denied.Code != "ip_not_allowed" || denied.KeyID != "managed" {
		t.Fatalf("IP whitelist rejection = %+v", denied)
	}
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil || len(logs) != 1 || logs[0].Decision != "blocked" || logs[0].StatusCode != 403 ||
		logs[0].Reason != "ip_not_allowed" || logs[0].RequestContent != "IP audit" {
		t.Fatalf("IP whitelist request log = %+v, err=%v", logs, err)
	}
	if admitted := engine.AdmitCapturedFromIP("managed-key", "gpt-5", "", "203.0.113.45", now.Add(time.Second)); !admitted.Bypass || admitted.KeyID != "managed" {
		t.Fatalf("track-only allowed IP = %+v", admitted)
	}
	if missing := engine.AdmitCapturedFromIP("managed-key", "gpt-5", "", "", now.Add(2*time.Second)); missing.Code != "ip_not_allowed" {
		t.Fatalf("missing ingress IP = %+v", missing)
	}
	if unmanaged := engine.AdmitCapturedFromIP("unmanaged-key", "gpt-5", "", "198.51.100.9", now); !unmanaged.Bypass {
		t.Fatalf("unmanaged key should remain unaffected = %+v", unmanaged)
	}
	policies, err := engine.store.LoadPolicies()
	if err != nil || len(policies) != 1 || !policies[0].IPWhitelistEnabled || len(policies[0].IPWhitelist) != 1 {
		t.Fatalf("persisted IP whitelist = %+v, err=%v", policies, err)
	}
}

func TestIndependentIPWhitelistUpdatePreservesQuotaAndStatus(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: false,
		FiveHourBudgetUSD: 70, SevenDayBudgetUSD: 500})
	defer func() { _ = engine.Close() }()
	updated, err := engine.UpdatePolicyIPWhitelist("managed", []string{" 203.0.113.7 ", "198.51.100.0/24"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.Disabled || updated.FiveHourBudgetUSD != 70 || updated.SevenDayBudgetUSD != 500 ||
		!updated.IPWhitelistEnabled || len(updated.IPWhitelist) != 2 || updated.IPWhitelist[0] != "203.0.113.7" {
		t.Fatalf("independent whitelist update changed quota or status: %+v", updated)
	}
	if _, err := engine.UpdatePolicyIPWhitelist("managed", nil, true); err == nil {
		t.Fatal("invalid whitelist update unexpectedly succeeded")
	}
	quotaEdit := updated
	quotaEdit.IPWhitelist = nil // The quota editor does not submit independent IP settings.
	quotaEdit.IPWhitelistEnabled = false
	quotaEdit.Enabled = true
	quotaEdit.FiveHourBudgetUSD = 80
	saved, err := engine.UpsertPolicy(quotaEdit, "")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Enabled || saved.FiveHourBudgetUSD != 80 || !saved.IPWhitelistEnabled || len(saved.IPWhitelist) != 2 {
		t.Fatalf("quota edit reset the independent whitelist: %+v", saved)
	}
	stored, err := engine.store.LoadPolicies()
	if err != nil || len(stored) != 1 || !stored[0].IPWhitelistEnabled || stored[0].FiveHourBudgetUSD != 80 {
		t.Fatalf("persisted independent settings = %+v, err=%v", stored, err)
	}
	disabled, err := engine.UpdatePolicyIPWhitelist("managed", saved.IPWhitelist, false)
	if err != nil || disabled.IPWhitelistEnabled || len(disabled.IPWhitelist) != 2 || !disabled.Enabled {
		t.Fatalf("turning off whitelist changed its entries or Key mode: %+v, err=%v", disabled, err)
	}
}

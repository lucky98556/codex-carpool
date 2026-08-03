//go:build linux && cgo

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"codex-carpool/internal/quota"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestManagementLanguageFollowsForwardedCPAHeader(t *testing.T) {
	chinese := pluginapi.ManagementRequest{Headers: http.Header{"Accept-Language": []string{"zh-CN,zh;q=0.9"}}}
	if got := managementLanguage(chinese); got != "zh" {
		t.Fatalf("managementLanguage(chinese) = %q, want zh", got)
	}
	english := pluginapi.ManagementRequest{Headers: http.Header{"Accept-Language": []string{"en-US,en;q=0.9"}}}
	if got := managementLanguage(english); got != "en" {
		t.Fatalf("managementLanguage(english) = %q, want en", got)
	}
	if got := localizedManagementMessage("en", "用量分析暂时不可用，请稍后重试", "Usage analysis is temporarily unavailable. Please retry."); got != "Usage analysis is temporarily unavailable. Please retry." {
		t.Fatalf("localized English message = %q", got)
	}
}

func TestManagementErrorsUseStableCodeAndCPAEnglishOrChinese(t *testing.T) {
	tests := []struct {
		cause       string
		code        string
		chinese     string
		english     string
	}{
		{
			cause:   "Key allocation 5.00x exceeds the remaining shared pool 1.00x",
			code:    "shared_pool_capacity_exceeded",
			chinese: "共享账号池剩余可分配 x 不足，无法保存此分配。",
			english: "The shared account pool does not have enough remaining x allocation for this change.",
		},
		{
			cause:   "access_rules[0].weekdays is required",
			code:    "access_schedule_invalid",
			chinese: "访问时段配置无效，请检查时区、星期和起止时间。",
			english: "The access schedule is invalid. Check the time zone, weekdays, and start/end times.",
		},
		{
			cause:   "hour granularity range must not exceed 31 days",
			code:    "analysis_hour_range_invalid",
			chinese: "按小时统计最多支持 31 个自然日，请缩短日期区间。",
			english: "Hourly analysis supports at most 31 calendar days. Shorten the date range.",
		},
		{
			cause:   "unexpected sqlite detail",
			code:    "operation_failed",
			chinese: "操作未完成，请稍后重试。",
			english: "The operation could not be completed. Please retry.",
		},
	}
	for _, test := range tests {
		if got := managementErrorCode(errors.New(test.cause)); got != test.code {
			t.Fatalf("managementErrorCode(%q) = %q, want %q", test.cause, got, test.code)
		}
		if got := localizedManagementError("zh", test.code); got != test.chinese {
			t.Fatalf("Chinese %s = %q, want %q", test.code, got, test.chinese)
		}
		if got := localizedManagementError("en", test.code); got != test.english {
			t.Fatalf("English %s = %q, want %q", test.code, got, test.english)
		}
	}
}

func TestAdmissionErrorsAreLocalizedWithoutChangingTheirStableCode(t *testing.T) {
	if got := localizedAdmissionMessage("zh", "quota_snapshot_unavailable", "ignored"); got != "暂无可用的官方额度快照，请稍后重试。" {
		t.Fatalf("Chinese snapshot message = %q", got)
	}
	if got := localizedAdmissionMessage("en", "model_not_allowed", "ignored"); got != "This API Key is not allowed to use the requested model." {
		t.Fatalf("English model message = %q", got)
	}
	if got := localizedAdmissionMessage("zh", "unknown_code", "original English detail"); got != "请求暂时无法处理，请稍后重试。" {
		t.Fatalf("Chinese unknown fallback = %q", got)
	}
	if got := localizedAdmissionMessage("en", "quota_allocation_exhausted", "ignored"); got != "This API Key has exhausted its shared-pool allocation for the current official window." {
		t.Fatalf("English allocation message = %q", got)
	}
}

func TestManagementFailureDoesNotExposeRawInternalDetail(t *testing.T) {
	request := pluginapi.ManagementRequest{Headers: http.Header{"Accept-Language": []string{"zh-CN"}}}
	raw, err := managementFailure(request, http.StatusBadRequest, errors.New("sqlite busy at /private/plugin.db"))
	if err != nil {
		t.Fatalf("managementFailure() error = %v", err)
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var response pluginapi.ManagementResponse
	if err := json.Unmarshal(wrapped.Result, &response); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	var body managementErrorBody
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatalf("decode management error body: %v", err)
	}
	if body.Code != "operation_failed" || body.Error != "操作未完成，请稍后重试。" {
		t.Fatalf("localized management body = %#v", body)
	}
	if string(response.Body) == "sqlite busy at /private/plugin.db" {
		t.Fatal("raw internal error detail leaked to management response")
	}
}

func TestLocalizedQuotaResponsesDoNotMutateOrExposeUpstreamErrors(t *testing.T) {
	readiness := quotaSnapshotReadiness{Errors: map[string]string{"account-a": "official quota returned HTTP 401 at /private/auth.json"}}
	localizedReadiness := localizedQuotaReadiness("en", readiness)
	if got := localizedReadiness.Errors["account-a"]; got != "Official quota synchronization failed. See plugin runtime and error logs." {
		t.Fatalf("localized readiness = %q", got)
	}
	if got := readiness.Errors["account-a"]; got == localizedReadiness.Errors["account-a"] {
		t.Fatal("localizing readiness must not overwrite the synchronizer diagnostic")
	}

	accounts := []quota.AccountPoolSnapshot{{Quota: &quota.OfficialQuotaSnapshot{AuthID: "account-a", LastError: "official quota returned HTTP 401 at /private/auth.json"}}}
	localizedAccounts := localizedAccountPoolSnapshots("zh", accounts)
	if got := localizedAccounts[0].Quota.LastError; got != "官方额度同步失败，请查看插件运行与错误日志。" {
		t.Fatalf("localized account error = %q", got)
	}
	if got := accounts[0].Quota.LastError; got != "official quota returned HTTP 401 at /private/auth.json" {
		t.Fatalf("source account error was mutated: %q", got)
	}
}

func TestManagementFailureLoggingKeepsOnlySafeOperationalFailures(t *testing.T) {
	if !managementFailureShouldLog("operation_failed") || !managementFailureShouldLog("account_discovery_failed") {
		t.Fatal("operational management failures should be retained in the plugin log")
	}
	if managementFailureShouldLog("invalid_json") {
		t.Fatal("ordinary client validation must not flood the operational log")
	}
	if got := safeManagementFailureDetail(errors.New("authorization: Bearer secret-token")); got != "sensitive diagnostic detail redacted" {
		t.Fatalf("sensitive diagnostic = %q", got)
	}
}

func TestManagementRegistrationUsesRequestedChineseMenuName(t *testing.T) {
	raw, err := handleMethod(pluginabi.MethodManagementRegister, nil)
	if err != nil {
		t.Fatalf("register management routes: %v", err)
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var registration managementRegistration
	if err := json.Unmarshal(wrapped.Result, &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if len(registration.Resources) != 1 || registration.Resources[0].Menu != "Codex 拼车" {
		t.Fatalf("registered menu = %#v, want Codex 拼车", registration.Resources)
	}
	resetRegistered := false
	for _, route := range registration.Routes {
		if route.Method == http.MethodPost && route.Path == "/codex-carpool/keys/reset" {
			resetRegistered = true
			break
		}
	}
	if !resetRegistered {
		t.Fatalf("management routes = %#v, want POST /codex-carpool/keys/reset", registration.Routes)
	}
}

func TestPluginRegistrationAndRequestInterceptorCleanup(t *testing.T) {
	registration := pluginRegistration()
	if registration.Metadata.GitHubRepository != pluginGitHubRepository {
		t.Fatalf("plugin repository = %q, want %q", registration.Metadata.GitHubRepository, pluginGitHubRepository)
	}
	if !registration.Capabilities.RequestInterceptor {
		t.Fatal("plugin registration must enable the native request interceptor")
	}
	raw, err := handleMethod(pluginabi.MethodRequestInterceptAfter, nil)
	if err != nil {
		t.Fatalf("request.intercept_after: %v", err)
	}
	var wrapped envelope
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var response pluginapi.RequestInterceptResponse
	if err := json.Unmarshal(wrapped.Result, &response); err != nil {
		t.Fatalf("decode interceptor response: %v", err)
	}
	if len(response.ClearHeaders) != 1 || response.ClearHeaders[0] != requestContextHeader {
		t.Fatalf("interceptor cleanup = %#v, want %q", response.ClearHeaders, requestContextHeader)
	}
}

func TestParseUsageAnalysisRangeUsesInclusiveLocalDates(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	from, until, location, err := parseUsageAnalysisRange("2026-07-01", "2026-07-28", "Asia/Shanghai", now)
	if err != nil {
		t.Fatalf("parseUsageAnalysisRange() error = %v", err)
	}
	if location.String() != "Asia/Shanghai" {
		t.Fatalf("location = %q, want Asia/Shanghai", location)
	}
	wantFrom := time.Date(2026, time.July, 1, 0, 0, 0, 0, location).UTC()
	wantUntil := time.Date(2026, time.July, 29, 0, 0, 0, 0, location).UTC()
	if !from.Equal(wantFrom) || !until.Equal(wantUntil) {
		t.Fatalf("range = %v to %v, want %v to %v", from, until, wantFrom, wantUntil)
	}
}

func TestParseUsageAnalysisRangeRejectsInvalidRange(t *testing.T) {
	now := time.Date(2026, time.July, 28, 12, 0, 0, 0, time.UTC)
	if _, _, _, err := parseUsageAnalysisRange("2026-07-29", "2026-07-28", "Asia/Shanghai", now); err == nil {
		t.Fatal("reversed analysis dates should fail")
	}
	if _, _, _, err := parseUsageAnalysisRange("2026-01-01", "2027-01-02", "Asia/Shanghai", now); err == nil {
		t.Fatal("analysis range above one year should fail")
	}
	// Two spring transitions and one fall transition make this 367-day local
	// range shorter than 366*24 elapsed hours in New York. The cap is defined
	// in operator-facing civil dates, so it must still reject the request.
	if _, _, _, err := parseUsageAnalysisRange("2026-03-01", "2027-03-02", "America/New_York", now); err == nil {
		t.Fatal("analysis range above 366 local dates should fail across DST")
	}
}

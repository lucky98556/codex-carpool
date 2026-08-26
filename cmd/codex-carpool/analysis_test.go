//go:build linux && cgo

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseLogTimeRangeUsesRFC3339AndRejectsReversedRange(t *testing.T) {
	from, to, err := parseLogTimeRange("2026-08-25T08:30:00+08:00", "2026-08-25T09:45:00+08:00")
	if err != nil {
		t.Fatalf("parseLogTimeRange() error = %v", err)
	}
	if got, want := from.Format(time.RFC3339), "2026-08-25T00:30:00Z"; got != want {
		t.Fatalf("from = %q, want %q", got, want)
	}
	if got, want := to.Format(time.RFC3339), "2026-08-25T01:45:00Z"; got != want {
		t.Fatalf("to = %q, want %q", got, want)
	}
	if _, _, err := parseLogTimeRange("2026-08-25T10:00:00Z", "2026-08-25T09:00:00Z"); err == nil {
		t.Fatal("reversed log time range error = nil")
	}
	if _, _, err := parseLogTimeRange("2026-08-25 10:00", ""); err == nil {
		t.Fatal("non-RFC3339 log time error = nil")
	}
}

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
		cause   string
		code    string
		chinese string
		english string
	}{
		{
			cause:   "the API key fingerprint is already managed by managed-2",
			code:    "api_key_already_managed",
			chinese: "该 CPA Key 已添加，请直接编辑现有设置。",
			english: "This CPA Key has already been added. Edit its existing settings instead.",
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
			cause:   `invalid RE2 expression "(": error parsing regexp`,
			code:    "content_filter_expression_invalid",
			chinese: "正则表达式无效，请检查 RE2 语法、长度和重复项。",
			english: "The regular expression is invalid. Check its RE2 syntax, length, and duplicates.",
		},
		{
			cause:   `content-filter expression ".*" must not match empty text`,
			code:    "content_filter_expression_empty",
			chinese: "正则表达式不能匹配空内容，请缩小匹配范围。",
			english: "The regular expression must not match empty content. Narrow its match scope.",
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
	if got := localizedAdmissionMessage("zh", "key_dollar_budget_exhausted", "ignored"); got != "此 Key 已达到设置的额度。" {
		t.Fatalf("Chinese dollar-budget message = %q", got)
	}
	if got := localizedAdmissionMessage("en", "model_not_allowed", "ignored"); got != "This API Key is not allowed to use the requested model." {
		t.Fatalf("English model message = %q", got)
	}
	if got := localizedAdmissionMessage("zh", "unknown_code", "original English detail"); got != "请求暂时无法处理，请稍后重试。" {
		t.Fatalf("Chinese unknown fallback = %q", got)
	}
	if got := localizedAdmissionMessage("en", "key_dollar_budget_exhausted", "ignored"); got != "This API key has reached its configured quota." {
		t.Fatalf("English dollar-budget message = %q", got)
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

func TestManagementFailureLoggingKeepsOnlySafeOperationalFailures(t *testing.T) {
	if !managementFailureShouldLog("operation_failed") || !managementFailureShouldLog("usage_analysis_unavailable") {
		t.Fatal("operational management failures should be retained in the plugin log")
	}
	if managementFailureShouldLog("invalid_json") {
		t.Fatal("ordinary client validation must not flood the operational log")
	}
	if got := safeManagementFailureDetail(errors.New("authorization: Bearer secret-token")); got != "sensitive diagnostic detail redacted" {
		t.Fatalf("sensitive diagnostic = %q", got)
	}
}

func TestManagementRegistrationUsesUsageManagementMenuName(t *testing.T) {
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
	if len(registration.Resources) != 1 || registration.Resources[0].Menu != pluginDisplayName {
		t.Fatalf("registered menu = %#v, want %s", registration.Resources, pluginDisplayName)
	}
	expectedRoutes := map[string]bool{
		http.MethodPost + " /codex-carpool/keys/reset":       false,
		http.MethodGet + " /codex-carpool/model-ranking":     false,
		http.MethodGet + " /codex-carpool/logs":              false,
		http.MethodDelete + " /codex-carpool/logs":           false,
		http.MethodGet + " /codex-carpool/log-storage":       false,
		http.MethodGet + " /codex-carpool/content-filter":    false,
		http.MethodPut + " /codex-carpool/content-filter":    false,
		http.MethodGet + " /codex-carpool/forbidden-logs":    false,
		http.MethodDelete + " /codex-carpool/forbidden-logs": false,
		http.MethodPut + " /codex-carpool/rate-sync":         false,
	}
	for _, route := range registration.Routes {
		key := route.Method + " " + route.Path
		if _, ok := expectedRoutes[key]; ok {
			expectedRoutes[key] = true
		}
	}
	for route, registered := range expectedRoutes {
		if !registered {
			t.Fatalf("management routes = %#v, want %s", registration.Routes, route)
		}
	}
}

func TestPluginRegistrationAndRequestInterceptorCleanup(t *testing.T) {
	registration := pluginRegistration()
	if registration.Metadata.Name != pluginDisplayName {
		t.Fatalf("plugin display name = %q, want %q", registration.Metadata.Name, pluginDisplayName)
	}
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

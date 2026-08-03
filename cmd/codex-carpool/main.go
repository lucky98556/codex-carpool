//go:build linux && cgo

package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unsafe"

	"codex-carpool/internal/quota"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginName             = "codex-carpool"
	pluginGitHubRepository = "https://github.com/lucky98556/codex-carpool"
	apiPrefix              = "/v0/management/" + pluginName
	requestContextHeader   = "X-Codex-Carpool-Request-ID"
	// Native shutdown has no host-side timeout. Keep a short plugin-owned
	// drain window, then preserve durable reservations and release CPA instead
	// of holding a reload or process stop until an upstream weekly reset.
	pluginShutdownDrainTimeout = 20 * time.Second
)

// managementLanguage uses the browser language CPA forwards through its
// management request. It deliberately defaults to English for API clients;
// the built-in panel always sends its synchronized CPA locale explicitly.
func managementLanguage(request pluginapi.ManagementRequest) string {
	language := strings.ToLower(strings.TrimSpace(request.Query.Get("lang")))
	if language == "" {
		language = strings.ToLower(strings.TrimSpace(request.Headers.Get("Accept-Language")))
	}
	if strings.HasPrefix(language, "zh") {
		return "zh"
	}
	return "en"
}

func localizedManagementMessage(language, chinese, english string) string {
	if language == "zh" {
		return chinese
	}
	return english
}

// managementErrorBody deliberately exposes a stable, UI-safe code alongside a
// localized message. The original error remains inside the plugin process for
// diagnosis; it is not copied to the browser where file paths or low-level
// SQLite details would be both confusing and potentially sensitive.
type managementErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func managementFailure(request pluginapi.ManagementRequest, status int, cause error) ([]byte, error) {
	code := managementErrorCode(cause)
	if managementFailureShouldLog(code) {
		if engine := currentEngine(); engine != nil {
			// Keep the browser response localized and free of implementation details,
			// while retaining a bounded, redacted diagnostic in the plugin-owned log.
			engine.LogOperational(
				"warn",
				"management_request_failed",
				fmt.Sprintf("code=%s; detail=%s", code, safeManagementFailureDetail(cause)),
				strings.TrimSpace(request.Query.Get("auth_id")),
				strings.TrimSpace(request.Query.Get("key_id")),
			)
		}
	}
	return managementJSON(status, managementErrorBody{
		Error: localizedManagementError(managementLanguage(request), code),
		Code:  code,
	})
}

func managementFailureShouldLog(code string) bool {
	switch code {
	case "engine_unavailable", "shutdown_in_progress", "quota_synchronizer_unavailable",
		"quota_refresh_unavailable", "usage_records_unavailable", "model_catalog_unavailable",
		"usage_analysis_unavailable", "account_discovery_failed", "operation_failed":
		return true
	default:
		return false
	}
}

func safeManagementFailureDetail(cause error) string {
	message := strings.TrimSpace(cause.Error())
	if message == "" {
		return "no diagnostic detail"
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{"bearer ", "authorization", "access_token", "id_token", "api_key", "api key", "secret", "password"} {
		if strings.Contains(lower, marker) {
			return "sensitive diagnostic detail redacted"
		}
	}
	const maxRunes = 480
	if len([]rune(message)) > maxRunes {
		return string([]rune(message)[:maxRunes]) + "…"
	}
	return message
}

// localizedQuotaReadiness prevents a transient upstream error string from
// leaking through otherwise localized success/202 responses. Its original,
// bounded diagnostic remains in the plugin's operational log.
func localizedQuotaReadiness(language string, readiness quotaSnapshotReadiness) quotaSnapshotReadiness {
	if len(readiness.Errors) == 0 {
		return readiness
	}
	localized := readiness
	localized.Errors = make(map[string]string, len(readiness.Errors))
	message := localizedManagementMessage(language,
		"官方额度同步失败，请查看插件运行与错误日志。",
		"Official quota synchronization failed. See plugin runtime and error logs.")
	for authID := range readiness.Errors {
		localized.Errors[authID] = message
	}
	return localized
}

// localizedAccountPoolSnapshots clones only the management response. The
// engine's durable official snapshot is intentionally left untouched so its
// operational logs and retry logic retain the original diagnostic context.
func localizedAccountPoolSnapshots(language string, accounts []quota.AccountPoolSnapshot) []quota.AccountPoolSnapshot {
	localized := append([]quota.AccountPoolSnapshot(nil), accounts...)
	message := localizedManagementMessage(language,
		"官方额度同步失败，请查看插件运行与错误日志。",
		"Official quota synchronization failed. See plugin runtime and error logs.")
	for index := range localized {
		if localized[index].Quota == nil || strings.TrimSpace(localized[index].Quota.LastError) == "" {
			continue
		}
		snapshot := *localized[index].Quota
		snapshot.LastError = message
		localized[index].Quota = &snapshot
	}
	return localized
}

func localizedSummary(language string, summary quota.SummarySnapshot) quota.SummarySnapshot {
	summary.Accounts = localizedAccountPoolSnapshots(language, summary.Accounts)
	return summary
}

func managementErrorCode(cause error) string {
	message := strings.ToLower(strings.TrimSpace(cause.Error()))
	switch {
	case strings.Contains(message, "not initialized"):
		return "engine_unavailable"
	case strings.Contains(message, "safe shutdown") || strings.Contains(message, "shutdown deferred"):
		return "shutdown_in_progress"
	case strings.Contains(message, "invalid json body"):
		return "invalid_json"
	case strings.Contains(message, "api_key is required"):
		return "api_key_required"
	case strings.Contains(message, "key_id is required"):
		return "key_id_required"
	case strings.Contains(message, "auth_id is required"):
		return "auth_id_required"
	case strings.Contains(message, "analysis timezone"):
		return "analysis_timezone_invalid"
	case strings.Contains(message, "analysis dates must use"):
		return "analysis_date_invalid"
	case strings.Contains(message, "hour granularity range"):
		return "analysis_hour_range_invalid"
	case strings.Contains(message, "analysis end") || strings.Contains(message, "analysis range"):
		return "analysis_range_invalid"
	case strings.Contains(message, "analysis granularity"):
		return "analysis_granularity_invalid"
	case strings.Contains(message, "unsupported trend window") || strings.Contains(message, "trend bins"):
		return "trend_request_invalid"
	case strings.Contains(message, "access_rules") || strings.Contains(message, "access_timezone"):
		return "access_schedule_invalid"
	case strings.Contains(message, "is retired; configure"):
		return "retired_policy_field"
	case strings.Contains(message, "exceed the enabled account pool") || strings.Contains(message, "exceeds the remaining shared pool"):
		return "shared_pool_capacity_exceeded"
	case strings.Contains(message, "account pool entries are required"):
		return "account_pool_empty"
	case strings.Contains(message, "account pool repeats") || strings.Contains(message, "auth_index") && strings.Contains(message, "already configured"):
		return "account_pool_duplicate"
	case strings.Contains(message, "account pool membership") && strings.Contains(message, "cannot change"):
		return "account_pool_window_locked"
	case strings.Contains(message, "cannot remove account") && strings.Contains(message, "awaiting terminal usage"):
		return "account_has_pending_usage"
	case strings.Contains(message, "cannot remove account") && strings.Contains(message, "official weekly"):
		return "account_window_locked"
	case strings.Contains(message, "account pool entry") && strings.Contains(message, "was not found"):
		return "account_not_found"
	case strings.Contains(message, "is not in the shared pool"):
		return "account_not_in_pool"
	case strings.Contains(message, "cannot rebind key policy"):
		return "policy_has_pending_usage"
	case strings.Contains(message, "api key fingerprint is already managed"):
		return "api_key_already_managed"
	case strings.Contains(message, "model catalog is empty"):
		return "model_catalog_empty"
	case strings.Contains(message, "is not in the synchronized cpa codex model catalog"):
		return "model_not_in_catalog"
	case strings.Contains(message, "key policy") && strings.Contains(message, "was not found"):
		return "policy_not_found"
	case strings.Contains(message, "require a plugin restart"):
		return "restart_required"
	case strings.Contains(message, "request_units cannot change"):
		return "request_units_window_locked"
	case strings.Contains(message, "official quota synchronizer is unavailable"):
		return "quota_synchronizer_unavailable"
	case strings.Contains(message, "quota refresh") && strings.Contains(message, "cooldown"):
		return "quota_refresh_cooldown"
	case strings.Contains(message, "no immediate account quota refresh task"):
		return "quota_refresh_unavailable"
	case strings.Contains(message, "read usage records"):
		return "usage_records_unavailable"
	case strings.Contains(message, "read model catalog"):
		return "model_catalog_unavailable"
	case strings.Contains(message, "plugin route not found"):
		return "route_not_found"
	case strings.Contains(message, "usage analysis is temporarily unavailable"):
		return "usage_analysis_unavailable"
	case strings.Contains(message, "discover") || strings.Contains(message, "auth file"):
		return "account_discovery_failed"
	default:
		return "operation_failed"
	}
}

func localizedManagementError(language, code string) string {
	chinese, english := "操作未完成，请稍后重试。", "The operation could not be completed. Please retry."
	switch code {
	case "engine_unavailable":
		chinese, english = "插件额度引擎暂不可用，请稍后重试。", "The plugin quota engine is temporarily unavailable. Please retry."
	case "shutdown_in_progress":
		chinese, english = "插件正在安全停止并结算用量，请稍后重试。", "The plugin is safely shutting down and settling usage. Please retry shortly."
	case "invalid_json":
		chinese, english = "请求数据格式不正确。", "The request body is invalid."
	case "api_key_required":
		chinese, english = "纳入管理时必须选择一个 CPA Key。", "Select a CPA Key before managing it."
	case "key_id_required":
		chinese, english = "请选择要操作的 Key。", "Select a Key to continue."
	case "auth_id_required":
		chinese, english = "请选择要操作的 Codex 账号。", "Select a Codex account to continue."
	case "analysis_timezone_invalid":
		chinese, english = "统计时区无效，请使用 IANA 时区（例如 Asia/Shanghai）。", "The reporting time zone is invalid. Use an IANA time zone such as Asia/Shanghai."
	case "analysis_date_invalid":
		chinese, english = "统计日期必须使用 YYYY-MM-DD 格式。", "Reporting dates must use the YYYY-MM-DD format."
	case "analysis_range_invalid":
		chinese, english = "统计结束日期不得早于开始日期，且区间最多 366 天。", "The end date must not precede the start date, and the range is limited to 366 days."
	case "analysis_hour_range_invalid":
		chinese, english = "按小时统计最多支持 31 个自然日，请缩短日期区间。", "Hourly analysis supports at most 31 calendar days. Shorten the date range."
	case "analysis_granularity_invalid":
		chinese, english = "统计粒度仅支持按小时、按日、按月或按年。", "Analysis granularity must be hourly, daily, monthly, or yearly."
	case "trend_request_invalid":
		chinese, english = "趋势统计参数无效。", "The trend request is invalid."
	case "access_schedule_invalid":
		chinese, english = "访问时段配置无效，请检查时区、星期和起止时间。", "The access schedule is invalid. Check the time zone, weekdays, and start/end times."
	case "retired_policy_field":
		chinese, english = "该旧版额度字段已不再支持，请使用共享池分配（x）和当前策略字段。", "This legacy quota field is no longer supported. Use shared-pool allocation (x) and current policy fields."
	case "shared_pool_capacity_exceeded":
		chinese, english = "共享账号池剩余可分配 x 不足，无法保存此分配。", "The shared account pool does not have enough remaining x allocation for this change."
	case "account_pool_empty":
		chinese, english = "请至少选择一个共享账号池账号。", "Select at least one shared account-pool account."
	case "account_pool_duplicate":
		chinese, english = "账号池存在重复账号或重复 CPA 调度身份。", "The account pool contains a duplicate account or CPA scheduler identity."
	case "account_pool_window_locked":
		chinese, english = "当前官方周账期尚未结束，暂不能变更账号池成员、容量或启用状态。", "The current official weekly window has not ended, so account-pool members, capacity, and enabled state cannot change yet."
	case "account_has_pending_usage":
		chinese, english = "该账号仍有等待实际用量回调的请求，暂不能移除。", "This account still has requests awaiting terminal usage, so it cannot be removed yet."
	case "account_window_locked":
		chinese, english = "该账号当前官方周账期尚未结束，暂不能移除。", "This account cannot be removed until its current official weekly window resets."
	case "account_not_found":
		chinese, english = "未找到该共享账号池账号。", "The shared account-pool account was not found."
	case "account_not_in_pool":
		chinese, english = "该账号不在当前共享账号池中。", "This account is not in the current shared account pool."
	case "policy_has_pending_usage":
		chinese, english = "该 Key 仍有等待实际用量回调的请求，暂不能更换绑定的 CPA Key。", "This Key still has requests awaiting terminal usage, so its bound CPA Key cannot be replaced yet."
	case "api_key_already_managed":
		chinese, english = "该 CPA Key 已被其他策略管理。", "This CPA Key is already managed by another policy."
	case "model_catalog_empty":
		chinese, english = "暂无可用的 Codex 模型目录，请先同步模型。", "No Codex model catalog is available. Sync models first."
	case "model_not_in_catalog":
		chinese, english = "选择的模型不在当前 CPA Codex 模型目录中，请同步后重试。", "A selected model is not in the current CPA Codex model catalog. Sync and retry."
	case "policy_not_found":
		chinese, english = "未找到该 Key 管理策略。", "The Key management policy was not found."
	case "restart_required":
		chinese, english = "修改数据库路径或 Key 加密密钥后需要重启插件并迁移 Key。", "Changing the database path or Key encryption secret requires a plugin restart and Key migration."
	case "request_units_window_locked":
		chinese, english = "当前仍有未结算请求或未结束的官方周账期，暂不能修改请求计量单位。", "Request units cannot change while requests are unsettled or official weekly windows remain active."
	case "quota_synchronizer_unavailable":
		chinese, english = "官方额度同步器暂不可用，请稍后重试。", "The official quota synchronizer is temporarily unavailable. Please retry."
	case "quota_refresh_cooldown":
		chinese, english = "官方额度刚刷新过，请稍后再试。", "Official quota was refreshed recently. Please retry shortly."
	case "quota_refresh_unavailable":
		chinese, english = "当前没有可立即刷新官方额度的账号。", "No account is currently available for an immediate official-quota refresh."
	case "usage_records_unavailable":
		chinese, english = "使用记录暂时不可读取，请稍后重试。", "Usage records are temporarily unavailable. Please retry."
	case "model_catalog_unavailable":
		chinese, english = "模型目录暂时不可读取，请稍后重试。", "The model catalog is temporarily unavailable. Please retry."
	case "route_not_found":
		chinese, english = "未找到插件管理接口。", "The plugin management route was not found."
	case "usage_analysis_unavailable":
		chinese, english = "用量分析暂时不可用，请稍后重试。", "Usage analysis is temporarily unavailable. Please retry."
	case "account_discovery_failed":
		chinese, english = "读取 CPA Codex 账号失败，请检查认证目录后重试。", "Could not read CPA Codex accounts. Check the auth directory and retry."
	}
	return localizedManagementMessage(language, chinese, english)
}

// schedulerLanguage reads the caller's language from the same scheduler
// headers already used to identify the managed API Key. This keeps quota 403,
// 429, and 503 responses localized without requiring any CPA ABI change.
func schedulerLanguage(request pluginapi.SchedulerPickRequest) string {
	for name, values := range request.Options.Headers {
		if !strings.EqualFold(name, "accept-language") || len(values) == 0 {
			continue
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(values[0])), "zh") {
			return "zh"
		}
		break
	}
	return "en"
}

func localizedAdmissionMessage(language, code, fallback string) string {
	chinese, english := "请求暂时无法处理，请稍后重试。", fallback
	switch code {
	case "quota_unavailable":
		chinese, english = "额度守卫暂不可用，请稍后重试。", "The quota guard is temporarily unavailable. Please retry."
	case "access_schedule_closed":
		chinese, english = "当前时间不在此 Key 允许访问的时段内。", "The current time is outside this API Key's allowed access schedule."
	case "model_not_allowed":
		chinese, english = "此 Key 不允许使用所请求的模型。", "This API Key is not allowed to use the requested model."
	case "quota_account_source_conflict":
		chinese, english = "共享账号身份正在核验或存在冲突，受控 Key 已暂停。", "Shared account identities are being verified or have a conflict; managed API Keys are paused."
	case "quota_persistence_unavailable":
		chinese, english = "额度账本暂不可用，请稍后重试。", "The quota ledger is temporarily unavailable. Please retry."
	case "quota_scheduler_candidates_required":
		chinese, english = "CPA 未提供可用的 Codex 调度账号候选。", "CPA did not provide usable Codex scheduler account candidates."
	case "quota_pool_unconfigured":
		chinese, english = "尚未配置可用的 Codex 共享账号池。", "No usable Codex shared account pool is configured."
	case "quota_snapshot_unavailable":
		chinese, english = "暂无可用的官方额度快照，请稍后重试。", "No current official quota snapshot is available. Please retry."
	case "quota_candidate_mismatch":
		chinese, english = "CPA 当前调度账号与共享账号池不匹配。", "CPA's current scheduler candidates do not match the shared account pool."
	case "quota_pool_exhausted":
		chinese, english = "共享账号池的官方额度已耗尽。", "The shared account pool's official quota is exhausted."
	case "quota_allocation_exhausted":
		chinese, english = "该 Key 在当前官方账期内的共享池分配已用完。", "This API Key has exhausted its shared-pool allocation for the current official window."
	case "quota_account_unavailable":
		chinese, english = "当前没有可用的共享 Codex 账号。", "No shared Codex account is currently available."
	}
	return localizedManagementMessage(language, chinese, english)
}

// pluginVersion is set by Makefile with -ldflags at build time. Keeping it in
// the native metadata lets CPA distinguish a newly installed shared library
// from an older in-process panel resource.
var pluginVersion = "dev"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler          bool `json:"scheduler"`
	UsagePlugin        bool `json:"usage_plugin"`
	RequestInterceptor bool `json:"request_interceptor"`
	ManagementAPI      bool `json:"management_api"`
}

// Native RPC cannot serialize Go handlers. The host binds management.handle
// back to this plugin for each route declared in this JSON-only manifest.
type managementRegistration struct {
	Routes    []managementRoute `json:"routes"`
	Resources []resourceRoute   `json:"resources"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description"`
}

type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type policyRequest struct {
	Policy quota.KeyPolicy `json:"policy"`
	APIKey string          `json:"api_key"`
}

// rejectRetiredPolicyFields keeps the management API aligned with the single
// allocation_x product model. SQLite migration still reads retired fields from
// older rows, but a current client must never silently accept a setting that
// codex-carpool no longer enforces.
func rejectRetiredPolicyFields(raw []byte) error {
	var envelope struct {
		Policy map[string]json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return err
	}
	for field := range envelope.Policy {
		normalized := strings.ToLower(strings.ReplaceAll(field, "_", ""))
		switch normalized {
		case "groupid", "fivehourpercent", "sevendaypercent", "fivehourmultiplier", "sevendaymultiplier", "maxconcurrency":
			return fmt.Errorf("%s is retired; configure name, allocation_x, allowed_models, access_rules, access_timezone, and enabled instead", field)
		}
	}
	return nil
}

type setupRequest struct {
	Settings quota.InstallationSettings `json:"settings"`
}

type modelsRequest struct {
	Models []quota.ModelCatalogEntry `json:"models"`
}

type accountRequest struct {
	Account quota.AccountPoolEntry `json:"account"`
}

type accountsRequest struct {
	Accounts []quota.AccountPoolEntry `json:"accounts"`
}

var runtime struct {
	mu            sync.RWMutex
	accountPoolMu sync.Mutex
	engine        *quota.Engine
	syncer        *quotaSynchronizer
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) (status C.int) {
	defer func() {
		if recover() != nil {
			status = 1
		}
	}()
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (status C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	defer func() {
		if recover() != nil {
			if engine := currentEngine(); engine != nil {
				engine.LogOperational("error", "plugin_panic", "插件调用发生未处理异常", "", "")
			}
			if response != nil && response.ptr != nil {
				C.free(response.ptr)
				response.ptr = nil
				response.len = 0
			}
			writeResponse(response, errorEnvelope("plugin_internal_error", "codex-carpool encountered an internal error"))
			status = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		if uint64(requestLen) > uint64(1<<31-1) {
			writeResponse(response, errorEnvelope("request_too_large", "plugin request exceeds the supported size"))
			return 1
		}
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() { _ = recover() }()
	runtime.mu.Lock()
	engine := runtime.engine
	syncer := runtime.syncer
	runtime.mu.Unlock()
	if engine == nil {
		if syncer != nil {
			syncer.Close()
		}
		runtime.mu.Lock()
		if runtime.syncer == syncer {
			runtime.syncer = nil
		}
		runtime.mu.Unlock()
		return
	}
	engine.LogOperational("info", "plugin_stopping", "插件正在停止", "", "")
	// Stop new admissions before deciding whether the synchronizer can exit.
	// If a terminal callback is missing, the synchronizer must remain alive to
	// obtain the next official weekly snapshot, which safely ends that old
	// reservation. Stopping it first would make shutdown wait forever.
	engine.CloseAdmissions()
	if syncer != nil {
		// Keep normal credential renewal available while a missing terminal
		// callback is reconciled. A cached OAuth access token can expire before
		// Codex's weekly reset; the synchronizer enters cache-only final shutdown
		// only after the accounting drain has reached zero.
		syncer.BeginShutdownDrain()
	}
	deadline := time.Now().Add(pluginShutdownDrainTimeout)
	for engine.PendingSettlementCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	conservative := engine.PendingSettlementCount() > 0
	if conservative {
		engine.LogOperational("warn", "plugin_shutdown_conservative", "关闭等待超时，已保留未结算请求的持久化标记；下次启动时继续等待实际用量结算", "", "")
		log.Printf("codex-carpool shutdown drain timed out; preserving %d durable reservations", engine.PendingSettlementCount())
	}
	if syncer != nil {
		syncer.Close()
	}
	runtime.mu.Lock()
	if runtime.syncer == syncer {
		runtime.syncer = nil
	}
	runtime.mu.Unlock()

	var err error
	if conservative {
		err = engine.CloseConservatively()
	} else {
		err = engine.Close()
		if err != nil {
			// SQLite can still fail after all callbacks drained. Preserve the
			// durable reservation state and return from the native callback rather
			// than retrying forever inside CPA's synchronous shutdown ABI.
			engine.LogOperational("warn", "plugin_shutdown_conservative", "关闭持久化超时，已保留可恢复的结算标记", "", "")
			err = engine.CloseConservatively()
		}
	}
	if err != nil {
		log.Printf("codex-carpool shutdown completed conservatively with persistence warning: %v", err)
	} else {
		log.Printf("codex-carpool shutdown completed")
	}
	releaseRuntimeEngine(engine)
}

func releaseRuntimeEngine(engine *quota.Engine) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.engine == engine {
		runtime.engine = nil
	}
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(request); err != nil {
			return nil, err
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodRequestInterceptBefore:
		return interceptRequestBeforeAuth(request)
	case pluginabi.MethodRequestInterceptAfter:
		return okEnvelope(pluginapi.RequestInterceptResponse{ClearHeaders: []string{requestContextHeader}})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []managementRoute{
				{Method: http.MethodGet, Path: "/" + pluginName + "/setup", Description: "Returns codex-carpool metering settings."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/setup", Description: "Saves codex-carpool settings without editing CLIProxyAPI configuration."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/summary", Description: "Returns shared-pool allocation, official snapshots, and Key usage counters."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/keys", Description: "Lists downstream API Key policies without secrets."},
				{Method: http.MethodPost, Path: "/" + pluginName + "/keys", Description: "Creates a shared-pool allocation policy for a CPA API Key."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/keys", Description: "Updates a Key allocation, model policy, remark, or management state."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/keys", Description: "Deletes one Key policy and its plugin-owned history."},
				{Method: http.MethodPost, Path: "/" + pluginName + "/keys/reset", Description: "Resets one Key's plugin-owned usage while preserving its policy."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/records", Description: "Lists compact usage buckets for one Key."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/trend", Description: "Returns real completed-token trend bins for one Key."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/analysis", Description: "Returns daily, monthly, or yearly completed-token analysis for one Key and a selected date range."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/logs", Description: "Filters and pages compact per-Key routing and usage decision logs."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/logs", Description: "Clears routing and usage decision logs for one Key without changing its quota."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/debug/quota", Description: "Returns a copy-safe local Token-guard and official-week allocation diagnosis for one Key."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/operation-logs", Description: "Lists plugin runtime and error logs."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/operation-logs", Description: "Clears plugin runtime and error logs without changing Key usage or quota."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/models", Description: "Lists the CPA-synchronized Codex model catalog."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/models", Description: "Replaces the CPA-synchronized Codex model catalog."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/accounts", Description: "Lists the independent Codex shared account pool and official quota snapshots."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/accounts/discover", Description: "Lists CPA Codex accounts available to add to the shared pool."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/accounts", Description: "Adds or updates one shared-pool Codex account."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/accounts/batch", Description: "Atomically adds or updates multiple shared-pool Codex accounts."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/accounts", Description: "Removes one account from the shared pool."},
				{Method: http.MethodPost, Path: "/" + pluginName + "/accounts/refresh", Description: "Schedules an official quota refresh without using the model proxy path."},
			},
			Resources: []resourceRoute{{
				Path:        "/panel",
				// CPA's registration ABI does not include a locale or menu-i18n map.
				// Use the Chinese product name requested for the current CPAMP menu;
				// the panel itself still follows CPA's live Chinese/English locale.
				Menu:        "Codex 拼车",
				Description: "codex 拼车插件",
			}},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(request []byte) error {
	// The host must call lifecycle registration to enable a native plugin, but
	// its YAML payload is intentionally ignored. Quota settings belong only to
	// this plugin's SQLite database and are configured through the setup panel.
	cfg, err := quota.StandaloneRuntimeConfig()
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.engine == nil {
		engine, err := quota.Open(cfg)
		if err != nil {
			return err
		}
		runtime.engine = engine
		runtime.syncer = newQuotaSynchronizer(engine)
		// No managed admission may observe persisted account-pool rows until the
		// file-backed CPA sources have been completely verified. This also makes
		// a temporarily unreadable historical row fail closed during startup.
		engine.RequireAccountSourceVerification()
		runtime.syncer.RefreshAccountSourceConflict()
		runtime.syncer.Start()
		engine.LogOperational("info", "plugin_started", "codex-carpool 已启动", "", "")
		if engine.AnalysisReaderDegraded() {
			engine.LogOperational("warn", "usage_analysis_reader_degraded", "年度用量分析已降级为共享 SQLite 连接；额度守卫与实际 Token 结算不受影响", "", "")
		}
		return nil
	}
	if runtime.engine.IsClosing() {
		return fmt.Errorf("codex-carpool is completing a safe shutdown; wait for the existing accounting drain")
	}
	// CLIProxyAPI can invoke plugin reconfiguration whenever its own YAML is
	// saved or reloaded. This plugin deliberately has no YAML-owned runtime
	// settings, so retaining the SQLite-backed engine is the only safe no-op.
	runtime.engine.LogOperational("info", "plugin_reconfigured", "CPA 已重新加载插件配置", "", "")
	return nil
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "CLIProxyAPI community",
			GitHubRepository: pluginGitHubRepository,
		},
		Capabilities: registrationCapability{Scheduler: true, UsagePlugin: true, RequestInterceptor: true, ManagementAPI: true},
	}
}

func interceptRequestBeforeAuth(raw []byte) ([]byte, error) {
	var request pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode request interceptor request: %w", err)
	}
	engine := currentEngine()
	if engine == nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	model := strings.TrimSpace(request.RequestedModel)
	if model == "" {
		model = strings.TrimSpace(request.Model)
	}
	captureID := engine.CaptureRequestContent(apiKeyFromHeaders(request.Headers), model, request.Body, nowUTC())
	if captureID == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Headers: http.Header{requestContextHeader: []string{captureID}},
	})
}

func pickAuth(raw []byte) ([]byte, error) {
	var request pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", err)
	}
	language := schedulerLanguage(request)
	engine := currentEngine()
	if engine == nil {
		return errorEnvelope("quota_unavailable", localizedAdmissionMessage(language, "quota_unavailable", "codex-carpool is not initialized")), nil
	}
	if !isCodexRoute(request) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	candidates := make([]quota.SchedulerCandidate, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidates = append(candidates, quota.SchedulerCandidate{AuthID: candidate.ID, Priority: candidate.Priority, Status: candidate.Status})
	}
	candidates = engine.ResolveSchedulerCandidates(candidates)
	if syncer := currentSynchronizer(); syncer != nil {
		syncer.Trigger(engine.StalePoolCandidates(candidates, nowUTC()))
	}
	admission := engine.AdmitCaptured(
		apiKeyFromHeaders(request.Options.Headers),
		request.Model,
		headerValue(request.Options.Headers, requestContextHeader),
		nowUTC(),
		candidates,
	)
	if admission.Bypass {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if !admission.Allowed {
		return errorEnvelopeWithStatus(admission.Code, localizedAdmissionMessage(language, admission.Code, admission.Message), admissionStatusCode(admission.Code)), nil
	}
	// A managed Key is routed only to a snapshot-eligible internal ledger. CPA
	// still receives its original candidate ID, and no OAuth credential is ever
	// copied into the request path or returned to the client.
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: quota.CPAAuthIDForPoolAuthID(candidates, admission.AuthID)})
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(record.Provider), "codex") {
		return okEnvelope(map[string]any{})
	}
	engine := currentEngine()
	if engine != nil {
		authID := engine.ResolvePoolAuthID(record.AuthID)
		engine.RecordUsage(quota.CompletedUsage{
			APIKey:          record.APIKey,
			AuthID:          authID,
			Model:           record.Model,
			RequestedAt:     record.RequestedAt,
			Generate:        record.Generate,
			Failed:          record.Failed,
			FailureStatus:   record.Failure.StatusCode,
			InputTokens:     record.Detail.InputTokens,
			OutputTokens:    record.Detail.OutputTokens,
			ReasoningTokens: record.Detail.ReasoningTokens,
			TotalTokens:     record.Detail.TotalTokens,
		})
		if record.Failed && record.Failure.StatusCode == http.StatusTooManyRequests {
			if syncer := currentSynchronizer(); syncer != nil {
				if isOfficialQuotaExhaustion(record.Failure.Body) {
					syncer.MarkRateLimited(authID, record.Failure.Body)
				}
				syncer.Trigger([]string{authID})
			}
		}
	}
	return okEnvelope(map[string]any{})
}

func isCodexRoute(request pluginapi.SchedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(request.Provider), "codex") {
		return true
	}
	for _, provider := range request.Providers {
		if strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return true
		}
	}
	return false
}

func handleManagement(raw []byte) ([]byte, error) {
	var request pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
	language := managementLanguage(request)
	fail := func(status int, cause error) ([]byte, error) {
		return managementFailure(request, status, cause)
	}
	engine := currentEngine()
	if engine == nil {
		return fail(http.StatusServiceUnavailable, errors.New("quota engine is not initialized"))
	}
	if engine.IsClosing() {
		return fail(http.StatusServiceUnavailable, errors.New("codex-carpool is completing a safe shutdown"))
	}
	path := strings.TrimRight(strings.TrimSpace(request.Path), "/")
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if strings.HasSuffix(path, "/panel") && method == http.MethodGet {
		return managementHTML(http.StatusOK, panelHTML())
	}
	switch {
	case path == apiPrefix+"/setup" && method == http.MethodGet:
		return managementJSON(http.StatusOK, engine.Installation())
	case path == apiPrefix+"/setup" && method == http.MethodPut:
		var payload setupRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		// Auth-dir changes and account saves share this boundary. Serializing
		// them prevents a concurrent save from validating one directory and
		// committing an entry after another request switches directories.
		runtime.accountPoolMu.Lock()
		setup, err := engine.ConfigureInstallation(payload.Settings)
		runtime.accountPoolMu.Unlock()
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		var readiness quotaSnapshotReadiness
		if syncer := currentSynchronizer(); syncer != nil {
			syncer.ClearCredentials()
			syncer.RefreshAccountSourceConflict()
			authIDs := make([]string, 0)
			for _, account := range engine.AccountPool(nowUTC()) {
				if account.Enabled {
					authIDs = append(authIDs, account.AuthID)
				}
			}
			if len(authIDs) > 0 {
				refreshStartedAt := nowUTC()
				syncer.TriggerNow(authIDs)
				readiness = syncer.WaitForRefreshedUsableSnapshot(authIDs, refreshStartedAt, quotaInitialSnapshotWait)
			}
		}
		engine.LogOperational("info", "installation_updated", "插件运行设置已更新", "", "")
		if len(readiness.Ready) == 0 && (len(readiness.Pending) > 0 || len(readiness.Errors) > 0) {
			engine.LogOperational("warn", "quota_sync_pending", "认证目录已更新，但尚未取得可用于路由的官方额度快照", "", "")
			return managementJSON(http.StatusAccepted, map[string]any{"settings": setup.Settings, "status": "quota_sync_pending", "quota": localizedQuotaReadiness(language, readiness)})
		}
		return managementJSON(http.StatusOK, map[string]any{"settings": setup.Settings, "status": "ready", "quota": localizedQuotaReadiness(language, readiness)})
	case path == apiPrefix+"/summary" && method == http.MethodGet:
		now := nowUTC()
		summary := engine.Summary(now)
		// Analytics is management-only: a read failure leaves Token cells unknown
		// but must not hide the authoritative allocation and official pool status.
		if enriched, err := engine.SummaryWithActualTokens(now); err == nil {
			summary = enriched
		}
		return managementJSON(http.StatusOK, localizedSummary(language, summary))
	case path == apiPrefix+"/keys" && method == http.MethodGet:
		return managementJSON(http.StatusOK, map[string]any{"keys": engine.Policies()})
	case path == apiPrefix+"/keys" && (method == http.MethodPost || method == http.MethodPut):
		var payload policyRequest
		if err := rejectRetiredPolicyFields(request.Body); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		if method == http.MethodPost && strings.TrimSpace(payload.APIKey) == "" {
			return fail(http.StatusBadRequest, errors.New("api_key is required when creating a policy"))
		}
		policy, err := engine.UpsertPolicy(payload.Policy, payload.APIKey)
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "key_policy_saved", "Key 管理策略已保存", "", policy.ID)
		return managementJSON(http.StatusOK, map[string]any{"key": policy})
	case path == apiPrefix+"/keys" && method == http.MethodDelete:
		if err := engine.DeletePolicy(strings.TrimSpace(request.Query.Get("key_id"))); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "key_policy_deleted", "Key 已解除插件管理", "", strings.TrimSpace(request.Query.Get("key_id")))
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/keys/reset" && method == http.MethodPost:
		keyID := strings.TrimSpace(request.Query.Get("key_id"))
		if err := engine.ResetPolicyUsage(keyID); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		// Keep an audit record of the reset. Existing decision and operational
		// logs are deliberately preserved by the accounting reset transaction.
		engine.LogOperational("info", "key_usage_reset", "Key 插件用量已重置", "", keyID)
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/records" && method == http.MethodGet:
		keyID := strings.TrimSpace(request.Query.Get("key_id"))
		if keyID == "" {
			return fail(http.StatusBadRequest, errors.New("key_id is required"))
		}
		records, err := engine.UsageRecords(keyID, parseLimit(request.Query.Get("limit")))
		if err != nil {
			return fail(http.StatusInternalServerError, errors.New("read usage records failed"))
		}
		return managementJSON(http.StatusOK, map[string]any{"records": records})
	case path == apiPrefix+"/trend" && method == http.MethodGet:
		window := 5 * time.Hour
		if strings.EqualFold(strings.TrimSpace(request.Query.Get("window")), "seven") {
			window = 7 * 24 * time.Hour
		}
		trend, err := engine.UsageTrend(strings.TrimSpace(request.Query.Get("key_id")), nowUTC(), window, 12)
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, trend)
	case path == apiPrefix+"/analysis" && method == http.MethodGet:
		from, until, location, err := parseUsageAnalysisRange(request.Query.Get("from"), request.Query.Get("to"), request.Query.Get("timezone"), nowUTC())
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		analysis, err := engine.UsageAnalysis(strings.TrimSpace(request.Query.Get("key_id")), from, until, location, strings.TrimSpace(request.Query.Get("granularity")))
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fail(http.StatusServiceUnavailable, errors.New("usage analysis is temporarily unavailable"))
			}
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, analysis)
	case path == apiPrefix+"/logs" && method == http.MethodGet:
		pageSize := strings.TrimSpace(request.Query.Get("page_size"))
		if pageSize == "" {
			// Preserve the previous ?limit= compatibility for external panel
			// clients while the built-in panel uses explicit pagination.
			pageSize = request.Query.Get("limit")
		}
		logs, err := engine.DecisionLogPage(strings.TrimSpace(request.Query.Get("key_id")), strings.TrimSpace(request.Query.Get("decision")), strings.TrimSpace(request.Query.Get("query")), parsePage(request.Query.Get("page")), parseLogPageSize(pageSize))
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, logs)
	case path == apiPrefix+"/logs" && method == http.MethodDelete:
		keyID := strings.TrimSpace(request.Query.Get("key_id"))
		if err := engine.ClearDecisionLogs(keyID); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/debug/quota" && method == http.MethodGet:
		debug, err := engine.QuotaDebug(strings.TrimSpace(request.Query.Get("key_id")), nowUTC())
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, debug)
	case path == apiPrefix+"/operation-logs" && method == http.MethodGet:
		logs, err := engine.OperationalLogPage(strings.TrimSpace(request.Query.Get("level")), strings.TrimSpace(request.Query.Get("query")), parsePage(request.Query.Get("page")), parseLogPageSize(request.Query.Get("page_size")))
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, logs)
	case path == apiPrefix+"/operation-logs" && method == http.MethodDelete:
		if err := engine.ClearOperationalLogs(); err != nil {
			return fail(http.StatusInternalServerError, errors.New("clear operational logs failed"))
		}
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/models" && method == http.MethodGet:
		models, err := engine.Models()
		if err != nil {
			return fail(http.StatusInternalServerError, errors.New("read model catalog failed"))
		}
		return managementJSON(http.StatusOK, map[string]any{"models": models})
	case path == apiPrefix+"/models" && method == http.MethodPut:
		var payload modelsRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		if err := engine.ReplaceModels(payload.Models); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "model_catalog_synced", fmt.Sprintf("已同步 %d 个 Codex 模型", len(payload.Models)), "", "")
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/accounts" && method == http.MethodGet:
		return managementJSON(http.StatusOK, map[string]any{"accounts": localizedAccountPoolSnapshots(language, engine.AccountPool(nowUTC()))})
	case path == apiPrefix+"/accounts/discover" && method == http.MethodGet:
		accounts, err := discoverCodexAccounts()
		if err != nil {
			return fail(http.StatusBadGateway, err)
		}
		return managementJSON(http.StatusOK, map[string]any{"accounts": accounts})
	case path == apiPrefix+"/accounts/batch" && method == http.MethodPut:
		var payload accountsRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		// Validate the entire future pool and save it under one management lock so
		// a batch cannot partially pass duplicate-account protection or capacity
		// validation while another account save is in flight.
		runtime.accountPoolMu.Lock()
		if err := validateDistinctCodexAccountSources(engine, payload.Accounts); err != nil {
			runtime.accountPoolMu.Unlock()
			return fail(http.StatusBadRequest, err)
		}
		accounts, err := engine.UpsertAccountPoolEntries(payload.Accounts)
		runtime.accountPoolMu.Unlock()
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		var readiness quotaSnapshotReadiness
		if syncer := currentSynchronizer(); syncer != nil {
			syncer.RefreshAccountSourceConflict()
			authIDs := make([]string, 0, len(accounts))
			for _, account := range accounts {
				if account.Enabled {
					authIDs = append(authIDs, account.AuthID)
				}
			}
			if len(authIDs) > 0 {
				refreshStartedAt := nowUTC()
				syncer.TriggerNow(authIDs)
				// The saved pool is not reported as usable until a worker has written
				// at least one local official snapshot. This closes the save→first
				// model-request race without moving upstream I/O into CPA routing.
				readiness = syncer.WaitForRefreshedUsableSnapshot(authIDs, refreshStartedAt, quotaInitialSnapshotWait)
			}
		}
		engine.LogOperational("info", "account_pool_batch_saved", fmt.Sprintf("已批量保存 %d 个共享账号池配置", len(accounts)), "", "")
		if len(readiness.Ready) == 0 && (len(readiness.Pending) > 0 || len(readiness.Errors) > 0) {
			engine.LogOperational("warn", "quota_sync_pending", "账号池已保存，但尚未取得可用于路由的官方额度快照", "", "")
			return managementJSON(http.StatusAccepted, map[string]any{"status": "quota_sync_pending", "accounts": accounts, "quota": localizedQuotaReadiness(language, readiness)})
		}
		return managementJSON(http.StatusOK, map[string]any{"status": "ready", "accounts": accounts, "quota": localizedQuotaReadiness(language, readiness)})
	case path == apiPrefix+"/accounts" && method == http.MethodPut:
		var payload accountRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		// Keep validation and the SQLite account-pool mutation atomic relative to
		// another management save, otherwise two simultaneous alias saves could
		// both validate before either one becomes visible.
		runtime.accountPoolMu.Lock()
		if err := validateDistinctCodexAccountSource(engine, payload.Account); err != nil {
			runtime.accountPoolMu.Unlock()
			return fail(http.StatusBadRequest, err)
		}
		account, err := engine.UpsertAccountPoolEntry(payload.Account)
		runtime.accountPoolMu.Unlock()
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		var readiness quotaSnapshotReadiness
		if syncer := currentSynchronizer(); syncer != nil && account.Enabled {
			syncer.RefreshAccountSourceConflict()
			refreshStartedAt := nowUTC()
			syncer.TriggerNow([]string{account.AuthID})
			readiness = syncer.WaitForRefreshedUsableSnapshot([]string{account.AuthID}, refreshStartedAt, quotaInitialSnapshotWait)
		} else if syncer := currentSynchronizer(); syncer != nil {
			syncer.RefreshAccountSourceConflict()
		}
		engine.LogOperational("info", "account_pool_saved", "共享账号池配置已保存", account.AuthID, "")
		if account.Enabled && len(readiness.Ready) == 0 && (len(readiness.Pending) > 0 || len(readiness.Errors) > 0) {
			engine.LogOperational("warn", "quota_sync_pending", "账号已保存，但尚未取得可用于路由的官方额度快照", account.AuthID, "")
			return managementJSON(http.StatusAccepted, map[string]any{"status": "quota_sync_pending", "account": account, "quota": localizedQuotaReadiness(language, readiness)})
		}
		return managementJSON(http.StatusOK, map[string]any{"status": "ready", "account": account, "quota": localizedQuotaReadiness(language, readiness)})
	case path == apiPrefix+"/accounts" && method == http.MethodDelete:
		authID := strings.TrimSpace(request.Query.Get("auth_id"))
		runtime.accountPoolMu.Lock()
		defer runtime.accountPoolMu.Unlock()
		if err := engine.DeleteAccountPoolEntry(authID); err != nil {
			return fail(http.StatusBadRequest, err)
		}
		if syncer := currentSynchronizer(); syncer != nil {
			syncer.RefreshAccountSourceConflict()
		}
		engine.LogOperational("info", "account_pool_deleted", "账号已从共享池移除", authID, "")
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/accounts/refresh" && method == http.MethodPost:
		syncer := currentSynchronizer()
		if syncer == nil {
			return fail(http.StatusServiceUnavailable, errors.New("official quota synchronizer is unavailable"))
		}
		authIDs := make([]string, 0)
		if authID := strings.TrimSpace(request.Query.Get("auth_id")); authID != "" {
			authIDs = append(authIDs, authID)
		} else {
			for _, account := range engine.AccountPool(nowUTC()) {
				if account.Enabled {
					authIDs = append(authIDs, account.AuthID)
				}
			}
		}
		refreshStartedAt := nowUTC()
		refresh, retryAfter := syncer.RequestManualRefresh(authIDs)
		if retryAfter > 0 {
			return fail(http.StatusTooManyRequests, errors.New("quota refresh cooldown"))
		}
		if refresh.Scheduled == 0 {
			return managementJSON(http.StatusConflict, map[string]any{
				"error":   localizedManagementError(language, "quota_refresh_unavailable"),
				"code":    "quota_refresh_unavailable",
				"refresh": refresh,
			})
		}
		if len(authIDs) == 1 {
			engine.LogOperational("info", "quota_refresh_requested", "已请求刷新官方额度", authIDs[0], "")
		} else {
			engine.LogOperational("info", "quota_refresh_requested", "已请求刷新全部官方额度", "", "")
		}
		readiness := syncer.WaitForRefreshedUsableSnapshot(authIDs, refreshStartedAt, quotaInitialSnapshotWait)
		if len(readiness.Ready) == 0 && (len(readiness.Pending) > 0 || len(readiness.Errors) > 0) {
			return managementJSON(http.StatusAccepted, map[string]any{"status": "quota_sync_pending", "refresh": refresh, "quota": localizedQuotaReadiness(language, readiness)})
		}
		return managementJSON(http.StatusOK, map[string]any{"status": "ready", "refresh": refresh, "quota": localizedQuotaReadiness(language, readiness)})
	default:
		return fail(http.StatusNotFound, errors.New("plugin route not found"))
	}
}

func currentEngine() *quota.Engine {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.engine
}

func currentSynchronizer() *quotaSynchronizer {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.syncer
}

func apiKeyFromHeaders(headers map[string][]string) string {
	for name, values := range headers {
		if !strings.EqualFold(name, "authorization") {
			continue
		}
		for _, value := range values {
			parts := strings.Fields(strings.TrimSpace(value))
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				return parts[1]
			}
		}
	}
	for _, expected := range []string{"x-api-key", "api-key"} {
		for name, values := range headers {
			if strings.EqualFold(name, expected) && len(values) > 0 {
				return strings.TrimSpace(values[0])
			}
		}
	}
	return ""
}

func headerValue(headers map[string][]string, expected string) string {
	for name, values := range headers {
		if strings.EqualFold(name, expected) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func parseLimit(raw string) int {
	var limit int
	_, _ = fmt.Sscanf(strings.TrimSpace(raw), "%d", &limit)
	if limit <= 0 || limit > 500 {
		return 100
	}
	return limit
}

func parseLogPageSize(raw string) int {
	limit := parseLimit(raw)
	if limit > 100 {
		return 100
	}
	return limit
}

func parsePage(raw string) int {
	var page int
	_, _ = fmt.Sscanf(strings.TrimSpace(raw), "%d", &page)
	if page <= 0 || page > 1_000_000 {
		return 1
	}
	return page
}

// parseUsageAnalysisRange accepts inclusive local dates and turns them into a
// UTC half-open interval. It deliberately caps the management query at one
// year, matching the retained bounded-token history and avoiding panel scans
// that could affect normal administration responsiveness.
func parseUsageAnalysisRange(rawFrom, rawTo, rawTimezone string, now time.Time) (time.Time, time.Time, *time.Location, error) {
	timezone := strings.TrimSpace(rawTimezone)
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("invalid analysis timezone")
	}
	parseDate := func(raw string) (time.Time, error) {
		value, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), location)
		if err != nil {
			return time.Time{}, fmt.Errorf("analysis dates must use YYYY-MM-DD")
		}
		return value, nil
	}
	now = now.In(location)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	if strings.TrimSpace(rawFrom) != "" {
		if from, err = parseDate(rawFrom); err != nil {
			return time.Time{}, time.Time{}, nil, err
		}
	}
	if strings.TrimSpace(rawTo) != "" {
		if to, err = parseDate(rawTo); err != nil {
			return time.Time{}, time.Time{}, nil, err
		}
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("analysis end date must not be before start date")
	}
	// Compare civil dates rather than elapsed hours: a DST transition can make
	// 367 local dates appear shorter than 366*24 hours (or the reverse).
	if to.After(from.AddDate(0, 0, 365)) {
		return time.Time{}, time.Time{}, nil, fmt.Errorf("analysis range must not exceed 366 days")
	}
	until := to.AddDate(0, 0, 1)
	return from.UTC(), until.UTC(), location, nil
}

func nowUTC() time.Time {
	return time.Now().UTC()
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	return errorEnvelopeWithStatus(code, message, 0)
}

// admissionStatusCode is serialized in the native plugin ABI error envelope.
// CLIProxyAPI v7.2.97 preserves it to the client, so quota exhaustion is a
// real HTTP 429 rather than an indistinguishable scheduler failure.
func admissionStatusCode(code string) int {
	switch code {
	case "model_not_allowed", "access_schedule_closed":
		return http.StatusForbidden
	case "quota_pool_unconfigured", "quota_snapshot_unavailable", "quota_candidate_mismatch", "quota_account_unavailable", "quota_scheduler_candidates_required", "quota_unavailable", "quota_account_source_conflict", "quota_persistence_unavailable":
		// SQLite/accounting recovery is a temporary plugin outage, never a
		// downstream Key quota exhaustion. Reserve 429 exclusively for a real
		// allocation or official-account limit so callers retry it correctly.
		return http.StatusServiceUnavailable
	default:
		return http.StatusTooManyRequests
	}
}

func errorEnvelopeWithStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func managementJSON(status int, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  []string{"application/json; charset=utf-8"},
			"Cache-Control": []string{"no-store"},
		},
		Body: body,
	})
}

func managementHTML(status int, body string) ([]byte, error) {
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":            []string{"text/html; charset=utf-8"},
			"Cache-Control":           []string{"no-store"},
			"Content-Security-Policy": []string{"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"},
		},
		Body: []byte(body),
	})
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

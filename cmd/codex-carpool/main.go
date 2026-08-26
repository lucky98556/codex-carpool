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
	pluginDisplayName      = "用量管理"
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
	case "engine_unavailable", "shutdown_in_progress", "usage_records_unavailable",
		"model_catalog_unavailable", "usage_analysis_unavailable", "operation_failed":
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
	case strings.Contains(message, "invalid re2 expression"):
		return "content_filter_expression_invalid"
	case strings.Contains(message, "content-filter expression") && strings.Contains(message, "must not match empty"):
		return "content_filter_expression_empty"
	case strings.Contains(message, "content-filter term") || strings.Contains(message, "content-filter categor"):
		return "content_filter_expression_invalid"
	case strings.Contains(message, "cannot rebind key policy"):
		return "policy_has_pending_usage"
	case strings.Contains(message, "api key fingerprint is already managed"):
		return "api_key_already_managed"
	case strings.Contains(message, "model catalog is empty"):
		return "model_catalog_empty"
	case strings.Contains(message, "is not in the synchronized cpa model catalog"):
		return "model_not_in_catalog"
	case strings.Contains(message, "key policy") && strings.Contains(message, "was not found"):
		return "policy_not_found"
	case strings.Contains(message, "read usage records"):
		return "usage_records_unavailable"
	case strings.Contains(message, "read model catalog"):
		return "model_catalog_unavailable"
	case strings.Contains(message, "plugin route not found"):
		return "route_not_found"
	case strings.Contains(message, "usage analysis is temporarily unavailable"):
		return "usage_analysis_unavailable"
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
	case "content_filter_expression_invalid":
		chinese, english = "正则表达式无效，请检查 RE2 语法、长度和重复项。", "The regular expression is invalid. Check its RE2 syntax, length, and duplicates."
	case "content_filter_expression_empty":
		chinese, english = "正则表达式不能匹配空内容，请缩小匹配范围。", "The regular expression must not match empty content. Narrow its match scope."
	case "policy_has_pending_usage":
		chinese, english = "该 Key 仍有等待实际用量回调的请求，暂不能更换绑定的 CPA Key。", "This Key still has requests awaiting terminal usage, so its bound CPA Key cannot be replaced yet."
	case "api_key_already_managed":
		chinese, english = "该 CPA Key 已添加，请直接编辑现有设置。", "This CPA Key has already been added. Edit its existing settings instead."
	case "model_catalog_empty":
		chinese, english = "暂无可用的 CPA 模型目录，请先同步模型。", "No CPA model catalog is available. Sync models first."
	case "model_not_in_catalog":
		chinese, english = "选择的模型不在当前 CPA 模型目录中，请同步后重试。", "A selected model is not in the current CPA model catalog. Sync and retry."
	case "policy_not_found":
		chinese, english = "未找到该 Key 的额度设置。", "The quota settings for this Key were not found."
	case "usage_records_unavailable":
		chinese, english = "使用记录暂时不可读取，请稍后重试。", "Usage records are temporarily unavailable. Please retry."
	case "model_catalog_unavailable":
		chinese, english = "模型目录暂时不可读取，请稍后重试。", "The model catalog is temporarily unavailable. Please retry."
	case "route_not_found":
		chinese, english = "未找到插件管理接口。", "The plugin management route was not found."
	case "usage_analysis_unavailable":
		chinese, english = "用量分析暂时不可用，请稍后重试。", "Usage analysis is temporarily unavailable. Please retry."
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
	case "model_rate_not_configured":
		chinese, english = "所请求模型尚未配置费率。", "The requested model has no configured rate."
	case "key_dollar_budget_exhausted":
		chinese, english = "此 Key 已达到设置的额度。", "This API key has reached its configured quota."
	case "content_forbidden":
		chinese, english = "请求内容命中拦截表达式，已拒绝处理。", "The request was rejected because it matched a content-blocking expression."
	case "quota_persistence_unavailable":
		chinese, english = "额度账本暂不可用，请稍后重试。", "The quota ledger is temporarily unavailable. Please retry."
	case "quota_scheduler_candidates_required":
		chinese, english = "CPA 未提供可用的调度账号候选。", "CPA did not provide usable scheduler account candidates."
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

type setupRequest struct {
	Settings quota.InstallationSettings `json:"settings"`
}

type modelsRequest struct {
	Models []quota.ModelCatalogEntry `json:"models"`
}

type ratesRequest struct {
	Rates []quota.ModelRate `json:"rates"`
}

type rateSyncRequest struct {
	Enabled bool `json:"enabled"`
}

type contentFilterRequest struct {
	Settings quota.ContentFilterSettings `json:"settings"`
}

var runtime struct {
	mu     sync.RWMutex
	engine *quota.Engine
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
	runtime.mu.Unlock()
	if engine == nil {
		return
	}
	engine.LogOperational("info", "plugin_stopping", "插件正在停止", "", "")
	// Stop new admissions before draining terminal CPA settlements.
	engine.CloseAdmissions()
	deadline := time.Now().Add(pluginShutdownDrainTimeout)
	for engine.PendingSettlementCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	conservative := engine.PendingSettlementCount() > 0
	if conservative {
		engine.LogOperational("warn", "plugin_shutdown_conservative", "关闭等待超时，将保存未结算请求的回调标记；插件重载后可继续匹配实际 Token", "", "")
		log.Printf("codex-carpool shutdown drain timed out; checkpointing %d callback markers", engine.PendingSettlementCount())
	}
	var err error
	if conservative {
		err = engine.CloseConservatively()
	} else {
		err = engine.Close()
		if err != nil {
			// Do not retry forever inside CPA's synchronous shutdown ABI.
			engine.LogOperational("warn", "plugin_shutdown_conservative", "关闭持久化失败，请检查插件运行日志", "", "")
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
				{Method: http.MethodGet, Path: "/" + pluginName + "/summary", Description: "Returns Key dollar budgets, settled spend, and Token usage counters."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/keys", Description: "Lists downstream API Key policies without secrets."},
				{Method: http.MethodPost, Path: "/" + pluginName + "/keys", Description: "Creates a five-hour and seven-day dollar budget policy for a CPA API Key."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/keys", Description: "Updates a Key dollar budget, remark, or budget-enforcement state."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/keys", Description: "Deletes one Key policy and its plugin-owned history."},
				{Method: http.MethodPost, Path: "/" + pluginName + "/keys/reset", Description: "Resets one Key's plugin-owned usage while preserving its policy."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/records", Description: "Lists compact usage buckets for one Key."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/trend", Description: "Returns real completed-token trend bins for one Key."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/analysis", Description: "Returns daily, monthly, or yearly completed-token analysis for one Key and a selected date range."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/model-ranking", Description: "Returns the all-Key daily model Token and dollar usage ranking."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/logs", Description: "Filters and pages compact per-Key routing and usage decision logs."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/logs", Description: "Clears routing and usage decision logs for one Key without changing its quota."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/log-storage", Description: "Returns the SQLite footprint and per-log logical storage usage."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/operation-logs", Description: "Lists plugin runtime and error logs."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/operation-logs", Description: "Clears plugin runtime and error logs without changing Key usage or quota."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/content-filter", Description: "Returns the RE2 content filter and its built-in/custom expressions."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/content-filter", Description: "Atomically enables, disables, or updates RE2 content filtering."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/forbidden-logs", Description: "Filters and pages dedicated content-expression interception logs."},
				{Method: http.MethodDelete, Path: "/" + pluginName + "/forbidden-logs", Description: "Clears content-expression interception logs for one Key or all Keys without changing quota or other logs."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/models", Description: "Lists the CPA-synchronized available model catalog."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/models", Description: "Replaces the CPA-synchronized available model catalog."},
				{Method: http.MethodGet, Path: "/" + pluginName + "/rates", Description: "Lists the operator-maintained per-model dollar rate card."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/rates", Description: "Atomically replaces the complete per-model Token rate card."},
				{Method: http.MethodPut, Path: "/" + pluginName + "/rate-sync", Description: "Enables or disables models.dev rate synchronization."},
			},
			Resources: []resourceRoute{{
				Path: "/panel",
				// CPA's registration ABI does not include a locale or menu-i18n map.
				// Use the Chinese product name requested for the current CPAMP menu;
				// the panel itself still follows CPA's live Chinese/English locale.
				Menu:        pluginDisplayName,
				Description: "CPA Key 额度、费率与用量管理",
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
		// CPA supplies scheduler candidates per request; terminal CPA usage
		// callbacks are the only source for Token and dollar accounting.
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
			Name:             pluginDisplayName,
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
	captureID := engine.CaptureRequestContent(apiKeyFromHeaders(request.Headers), model, request.Headers.Get("Content-Type"), request.Body, nowUTC())
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
	candidates := make([]quota.SchedulerCandidate, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidates = append(candidates, quota.SchedulerCandidate{AuthID: candidate.ID, Priority: candidate.Priority, Status: candidate.Status})
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
		message := localizedAdmissionMessage(language, admission.Code, admission.Message)
		if admission.RetryAt != nil {
			if language == "zh" {
				message += " 冷却至 " + admission.RetryAt.In(time.Local).Format("2006-01-02 15:04:05")
			} else {
				message += " Retry after " + admission.RetryAt.UTC().Format(time.RFC3339)
			}
		}
		return errorEnvelopeWithStatus(admission.Code, message, admissionStatusCode(admission.Code)), nil
	}
	// A managed Key is routed to the candidate selected by CPA. No OAuth
	// credential is copied into the request path or returned to the client.
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: admission.AuthID})
}

func handleUsage(raw []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("decode usage record: %w", err)
	}
	engine := currentEngine()
	if engine != nil {
		authID := strings.TrimSpace(record.AuthID)
		engine.RecordUsage(quota.CompletedUsage{
			APIKey:              record.APIKey,
			AuthID:              authID,
			Model:               record.Model,
			Alias:               record.Alias,
			Provider:            record.Provider,
			ExecutorType:        record.ExecutorType,
			ServiceTier:         record.ServiceTier,
			RequestedAt:         record.RequestedAt,
			Generate:            record.Generate,
			Failed:              record.Failed,
			FailureStatus:       record.Failure.StatusCode,
			InputTokens:         record.Detail.InputTokens,
			OutputTokens:        record.Detail.OutputTokens,
			ReasoningTokens:     record.Detail.ReasoningTokens,
			CachedTokens:        record.Detail.CachedTokens,
			CacheReadTokens:     record.Detail.CacheReadTokens,
			CacheCreationTokens: record.Detail.CacheCreationTokens,
			TotalTokens:         record.Detail.TotalTokens,
		})
	}
	return okEnvelope(map[string]any{})
}

func handleManagement(raw []byte) ([]byte, error) {
	var request pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, fmt.Errorf("decode management request: %w", err)
	}
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
		setup, err := engine.ConfigureInstallation(payload.Settings)
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "installation_updated", "插件运行设置已更新", "", "")
		return managementJSON(http.StatusOK, map[string]any{"settings": setup.Settings, "status": "ready"})
	case path == apiPrefix+"/summary" && method == http.MethodGet:
		now := nowUTC()
		summary := engine.Summary(now)
		// Analytics is management-only: a read failure leaves Token cells unknown
		// but must not hide the authoritative Key dollar-window status.
		if enriched, err := engine.SummaryWithActualTokens(now); err == nil {
			summary = enriched
		}
		return managementJSON(http.StatusOK, summary)
	case path == apiPrefix+"/keys" && method == http.MethodGet:
		return managementJSON(http.StatusOK, map[string]any{"keys": engine.Policies()})
	case path == apiPrefix+"/keys" && (method == http.MethodPost || method == http.MethodPut):
		var payload policyRequest
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
		engine.LogOperational("info", "key_policy_saved", "Key 额度设置已保存", "", policy.ID)
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
	case path == apiPrefix+"/model-ranking" && method == http.MethodGet:
		from, until, location, err := parseUsageAnalysisRange(request.Query.Get("from"), request.Query.Get("to"), request.Query.Get("timezone"), nowUTC())
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		ranking, err := engine.ModelUsageRanking(from, until, location)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fail(http.StatusServiceUnavailable, errors.New("usage analysis is temporarily unavailable"))
			}
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, ranking)
	case path == apiPrefix+"/logs" && method == http.MethodGet:
		pageSize := strings.TrimSpace(request.Query.Get("page_size"))
		if pageSize == "" {
			// Preserve the previous ?limit= compatibility for external panel
			// clients while the built-in panel uses explicit pagination.
			pageSize = request.Query.Get("limit")
		}
		from, to, err := parseLogTimeRange(request.Query.Get("from"), request.Query.Get("to"))
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		logs, err := engine.DecisionLogPageInRange(strings.TrimSpace(request.Query.Get("key_id")), strings.TrimSpace(request.Query.Get("decision")), strings.TrimSpace(request.Query.Get("query")), from, to, parsePage(request.Query.Get("page")), parseLogPageSize(pageSize))
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
	case path == apiPrefix+"/log-storage" && method == http.MethodGet:
		storage, err := engine.LogStorage()
		if err != nil {
			return fail(http.StatusInternalServerError, errors.New("read log database size failed"))
		}
		return managementJSON(http.StatusOK, storage)
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
	case path == apiPrefix+"/content-filter" && method == http.MethodGet:
		return managementJSON(http.StatusOK, map[string]any{"settings": engine.ContentFilterSettings()})
	case path == apiPrefix+"/content-filter" && method == http.MethodPut:
		var payload contentFilterRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		settings, err := engine.ConfigureContentFilter(payload.Settings)
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "content_filter_updated", fmt.Sprintf("内容正则拦截已更新：启用=%t，表达式=%d", settings.Enabled, len(settings.Terms)), "", "")
		return managementJSON(http.StatusOK, map[string]any{"settings": settings})
	case path == apiPrefix+"/forbidden-logs" && method == http.MethodGet:
		pageSize := strings.TrimSpace(request.Query.Get("page_size"))
		if pageSize == "" {
			pageSize = request.Query.Get("limit")
		}
		logs, err := engine.DecisionLogPage(strings.TrimSpace(request.Query.Get("key_id")), "forbidden", strings.TrimSpace(request.Query.Get("query")), parsePage(request.Query.Get("page")), parseLogPageSize(pageSize))
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		return managementJSON(http.StatusOK, logs)
	case path == apiPrefix+"/forbidden-logs" && method == http.MethodDelete:
		keyID := strings.TrimSpace(request.Query.Get("key_id"))
		if err := engine.ClearForbiddenDecisionLogs(keyID); err != nil {
			return fail(http.StatusBadRequest, err)
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
		engine.LogOperational("info", "model_catalog_synced", fmt.Sprintf("已同步 %d 个 CPA 可用模型", len(payload.Models)), "", "")
		if engine.ModelRateSyncStatus().Enabled {
			// Reconcile prices on the managed loop so this local catalog update is
			// never blocked by the external models.dev request.
			engine.RequestModelRateSync()
		}
		return managementJSON(http.StatusOK, map[string]string{"status": "ok"})
	case path == apiPrefix+"/rates" && method == http.MethodGet:
		return managementJSON(http.StatusOK, map[string]any{"rates": engine.ModelRates(), "sync": engine.ModelRateSyncStatus()})
	case path == apiPrefix+"/rates" && method == http.MethodPut:
		var payload ratesRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		rates, err := engine.ReplaceModelRates(payload.Rates)
		if err != nil {
			return fail(http.StatusBadRequest, err)
		}
		engine.LogOperational("info", "model_rates_saved", fmt.Sprintf("模型费率已更新：%d 个模型", len(rates)), "", "")
		return managementJSON(http.StatusOK, map[string]any{"rates": rates, "sync": engine.ModelRateSyncStatus()})
	case path == apiPrefix+"/rate-sync" && method == http.MethodPut:
		var payload rateSyncRequest
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return fail(http.StatusBadRequest, errors.New("invalid JSON body"))
		}
		status, err := engine.SetModelRateSyncEnabled(payload.Enabled)
		if err != nil {
			return fail(http.StatusInternalServerError, err)
		}
		level, event, message := "info", "model_rate_sync_disabled", "models.dev 价格同步已关闭"
		if payload.Enabled {
			event, message = "model_rate_sync_enabled", "models.dev 价格同步已开启"
			if status.LastError != "" {
				level, message = "warn", "models.dev 价格同步已开启，首次同步失败并保留原费率："+status.LastError
			}
		}
		engine.LogOperational(level, event, message, "", "")
		return managementJSON(http.StatusOK, map[string]any{"rates": engine.ModelRates(), "sync": status})
	default:
		return fail(http.StatusNotFound, errors.New("plugin route not found"))
	}
}

func currentEngine() *quota.Engine {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.engine
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

func parseLogTimeRange(rawFrom, rawTo string) (time.Time, time.Time, error) {
	parse := func(raw, name string) (time.Time, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return time.Time{}, nil
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("log %s time must use RFC3339", name)
		}
		return value.UTC(), nil
	}
	from, err := parse(rawFrom, "start")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to, err := parse(rawTo, "end")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("log end time must not be before start time")
	}
	return from, to, nil
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
	case "model_not_allowed", "access_schedule_closed", "content_forbidden":
		return http.StatusForbidden
	case "quota_scheduler_candidates_required", "quota_unavailable", "quota_persistence_unavailable", "model_rate_not_configured":
		// SQLite/accounting recovery is a temporary plugin outage, never a
		// downstream Key quota exhaustion. Reserve 429 exclusively for a real
		// configured dollar-budget exhaustion so callers retry it correctly.
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

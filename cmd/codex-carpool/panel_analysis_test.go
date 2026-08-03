//go:build linux && cgo

package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestPanelIncludesDateAnalysisAndAccessScheduleControls(t *testing.T) {
	page := panelHTML()
	for _, expected := range []string{
		`id="analysis-from"`,
		`id="analysis-to"`,
		`id="analysis-granularity"`,
		`id="analysis-apply"`,
		`<option value="today" selected>今日</option>`,
		`<option value="hour" selected>按小时</option>`,
		`applyAnalysisPreset('today')`,
		`hour:copy('按小时','Hourly')`,
		`id="access-limited"`,
		`id="access-weekdays"`,
		`id="access-timezone"`,
		`/analysis?`,
		`access_rules`,
		`id="access-rule-add"`,
		`data-access-rule`,
		`available_from`,
		`retention_days`,
		`function renderAccessRules`,
		`const cpaLocale=`,
		`const uiText=`,
		`Accept-Language`,
		`function showToast`,
		`id='toast-region'`,
		`/accounts/refresh`,
		`function quotaSyncMessage`,
		`access_schedule_closed`,
		`quota_allocation_exhausted`,
		`management_request_failed`,
		`official quota synchronization failed. See plugin runtime and error logs.`,
		`diagnosticEvents`,
		`#toast-region>div`,
		`const pairs=`,
		`analysis_reader_degraded`,
		`Usage analysis is temporarily unavailable. Please retry.`,
		`class="dashboard-grid"`,
		`class="key-analysis"`,
		`id="analysis-metrics"`,
		`id="model-mix"`,
		`当前计量 x 用量`,
		`待官方确认估算`,
		`Provisional estimate`,
		`allocationX(window,'provisional')`,
		`data-cc-label="keyManageAction"`,
		`data-cc-label="operation"`,
		`data-key-edit=`,
		`data-key-reset=`,
		`data-key-delete=`,
		`/keys/reset?key_id=`,
		`key_usage_reset`,
		`id="decision-clear"`,
		`id="operation-clear"`,
		`function clearDecisionLogs()`,
		`function clearOperationalLogs()`,
		`api('/logs?key_id='+encodeURIComponent(key.id),{method:'DELETE'})`,
		`api('/operation-logs',{method:'DELETE'})`,
		`all logs will be preserved`,
		`class="pool-card"`,
		`id="official-account-pool"`,
		`class="pool-actions"`,
		`class="pool-button pool-icon-button" id="quota-refresh" aria-label="刷新额度" title="刷新额度"`,
		`.pool-icon-button{`,
		`id="cc-icon-shield"`,
		`id="cc-icon-gauge"`,
		`id="cc-icon-github"`,
		`class="github-link"`,
		`href="https://github.com/lucky98556/codex-carpool"`,
		`target="_blank" rel="noopener noreferrer"`,
		`.github-link{`,
		`class="cc-icon-sprite"`,
		`--cc-primary:#168a67`,
		`linear-gradient(145deg,#0b4f3d 0%,#116d51 52%,#239b72 100%)!important`,
		`#official-account-pool.pool-card{`,
		`#official-account-pool .pool-button{`,
		`class="pool-metric"`,
		`class="pool-account-list"`,
		`.cc-panel .key-panel>.table-wrap{max-height:270px`,
		`.cc-panel .pool-account-list{max-height:312px`,
		`.cc-panel .key-table thead th{position:sticky`,
		`.cc-panel .key-table th:nth-child(4){width:10%}`,
		`.cc-panel .key-table th:nth-child(8){width:20%}`,
		`id="key-usage-heading">官方确认</th>`,
		`id="key-cycle-heading">周期 Token</th>`,
		`id="key-total-heading">累计 Token</th>`,
		`function confirmedAllocationText(window)`,
		`key.actual_tokens||{}`,
		`actualAvailable=Boolean(actual.available)`,
		`actualAvailable&&actual.cycle_known?tokens(actual.cycle):'—'`,
		`uiText('周期 Token','Cycle Tokens')`,
		`uiText('累计 Token','Total Tokens')`,
		`class="key-token-cell"`,
		`.cc-panel .key-table td.key-row-actions{padding-right:6px!important;padding-left:6px!important}`,
		`.cc-panel .log-table tbody td{overflow:hidden;text-overflow:ellipsis;white-space:nowrap!important;overflow-wrap:normal!important}`,
		`.cc-panel .log-table .decision{display:block;max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}`,
		`className='log-column-resizer'`,
		`role','separator'`,
		`localStorage.setItem(storageKey,JSON.stringify(widths.map(width=>Math.round(width))))`,
		`applyLogCellTitles(document.getElementById('logs'))`,
		`applyLogCellTitles(document.getElementById('operation-logs'))`,
		`id="decision-log-tools"`,
		`id="forbidden-log-tools" hidden`,
		`id="operation-log-tools" hidden`,
		`.cc-panel .log-tools[hidden]{display:none!important}`,
		`for(const name of validTabs){const selected=name===tab`,
		`if(view)view.hidden=!selected;if(tools)tools.hidden=!selected`,
		`class="log-action-column"`,
		`.cc-panel .decision-log-table th.log-action-column,.cc-panel .decision-log-table td.log-action{position:sticky!important`,
		`document.addEventListener('click',event=>{const target=event.target instanceof Element?event.target.closest('[data-log-tab]'):null;`,
		`window.__ccActivateLogTab=activate`,
		`document.dispatchEvent(new CustomEvent('codex-carpool:log-tab-changed',{detail:{tab}}))`,
		`document.addEventListener('codex-carpool:log-tab-changed',()=>requestAnimationFrame(installVisibleTables))`,
		`#policy-dialog[open],#account-dialog[open],#auth-directory-dialog[open],#log-detail-dialog[open],#content-filter-dialog[open],#quota-debug-dialog[open]{display:flex;flex-direction:column}`,
		`#policy-dialog .form-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px}`,
		`#policy-dialog::backdrop`,
		`row.onkeydown=event=>{if(event.target!==row)return;`,
		`data-key-edit=`,
		`data-key-delete=`,
		`request_content`,
		`请求内容`,
		`最后一条用户文本（最长 2000 字符）`,
		`id="log-detail-dialog"`,
		`class="row-action log-detail-open"`,
		`data-log-index=`,
		`set('log-detail-request',log.request_content)`,
		`$('logs').addEventListener('click'`,
		`.log-detail-grid{display:grid`,
		`cells[8].textContent=log.auth_id||'—'`,
		`面板数据已刷新。`,
		`Dashboard data refreshed.`,
		`const toastKey=(ok?'ok:':'error:')+String(value)`,
		`const dialogMessageIDs=['policy-dialog','account-dialog','auth-directory-dialog','content-filter-dialog','quota-debug-dialog']`,
		`class="dialog-message" role="alert" aria-live="assertive" hidden`,
		`.dialog-message[hidden]{display:none}`,
		`className='account-capacity-field'`,
		`unit.textContent='x'`,
		`discoveredCapacity=Number(account.capacity_x)`,
		`#account-dialog .account-capacity-unit{`,
		`if(typeof window.__ccRenderAnalysis==='function'){window.__ccRenderAnalysis();return}`,
		`window.__ccRenderAnalysis=render`,
		`.cc-panel .legacy-key-detail{width:100%;min-width:0}`,
		`id="content-filter-open"`,
		`id="content-filter-enabled"`,
		`id="content-filter-search"`,
		`class="content-filter-list-head"`,
		`class="content-filter-value"`,
		`#content-filter-dialog{box-sizing:border-box!important;display:none!important;width:min(820px,calc(100vw - 32px))!important`,
		`#content-filter-dialog>footer{display:flex!important;flex:0 0 auto!important;align-items:center!important;justify-content:flex-end!important`,
		`id="quota-debug-open"`,
		`id="forbidden-log-view" class="log-view" hidden`,
		`id="forbidden-search"`,
		`id="forbidden-clear"`,
		`api('/content-filter')`,
		`api('/forbidden-logs?'+params)`,
		`['CPA Key 尾号','CPA Key suffix']`,
		`key?.key_suffix`,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("panel HTML missing %q", expected)
		}
	}
	if strings.Contains(page, `uiText('待确认 +','Provisional +')`) {
		t.Fatal("Key list must keep provisional usage in the analysis panel instead of repeating it in the table")
	}
	if strings.Contains(page, `const quotaDebugActions=document.querySelector('.page-head .actions')`) {
		t.Fatal("quota diagnostics must be a stable utility action instead of a runtime-inserted button")
	}
	if strings.Contains(page, `id="window"`) {
		t.Fatal("old trend-window selector should not remain after analysis controls replace it")
	}
	if strings.Contains(page, `applyPrototypeLayout`) || strings.Contains(page, `cc-key-analysis`) {
		t.Fatal("panel must not reparent startup-critical sections at runtime")
	}
	if strings.Contains(page, `linear-gradient(142deg,#095b4f`) || strings.Contains(page, `#a2ffbd,#55f1c1 54%,#43c7ff`) {
		t.Fatal("approved light account-pool palette must not retain the retired saturated theme")
	}
	if strings.Contains(page, `code.iconify.design`) || strings.Contains(page, `<iconify-icon`) {
		t.Fatal("panel icons must be embedded so the management page has no runtime icon CDN dependency")
	}
	if strings.Contains(page, `.pool-panel{background:#f7faff!important`) || strings.Contains(page, `.pool-panel .pool-account{background:#fff!important`) {
		t.Fatal("account-pool palette must inherit CPA theme variables instead of pinning light colors")
	}
	if strings.Contains(page, `.cc-panel .pool-card{background:linear-gradient(145deg,color-mix(in srgb,var(--card,#fff) 96%,#eaf2ff)`) {
		t.Fatal("retired light account-pool palette must not compete with the approved emerald production theme")
	}
	if strings.Contains(page, `id="message"`) || strings.Contains(page, `pageMessage=document.getElementById('message')`) || strings.Contains(page, `.cc-panel .message{`) {
		t.Fatal("page-level inline messages must not compete with dialog feedback and top-right toasts")
	}
	if got := strings.Count(page, `class="dialog-message" role="alert" aria-live="assertive" hidden`); got != 5 {
		t.Fatalf("dialog-local message containers = %d, want 5", got)
	}
}

func TestPreviewUsesProductionPanelStylesAsSingleSource(t *testing.T) {
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve panel test source path")
	}
	projectRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	previewPath := filepath.Join(projectRoot, "ui-preview", "index.html")
	raw, err := os.ReadFile(previewPath)
	if err != nil {
		t.Fatalf("read UI preview: %v", err)
	}
	preview := string(raw)
	for _, expected := range []string{
		`href="../cmd/codex-carpool/web/styles.css"`,
		`class="app cc-panel"`,
		`<th>官方确认</th><th>周期 Token</th><th>累计 Token</th>`,
		`id="official-account-pool"`,
		`class="pool-metric"`,
		`class="pool-account-list"`,
	} {
		if !strings.Contains(preview, expected) {
			t.Fatalf("UI preview missing production-style marker %q", expected)
		}
	}
	for _, retired := range []string{`href="styles.css"`, `href="palette.css"`} {
		if strings.Contains(preview, retired) {
			t.Fatalf("UI preview still loads independent style source %q", retired)
		}
	}
	for _, retiredFile := range []string{"styles.css", "palette.css"} {
		if _, err := os.Stat(filepath.Join(projectRoot, "ui-preview", retiredFile)); !os.IsNotExist(err) {
			t.Fatalf("preview-only style %s must not exist; stat error = %v", retiredFile, err)
		}
	}
}

func TestPanelKeepsInteractiveStatesReadableAcrossThemes(t *testing.T) {
	page := panelHTML()
	for _, expected := range []string{
		`.cc-panel label.search>input{`,
		`border:0!important`,
		`.cc-panel label.search>input#search`,
		`.cc-panel label.search>input#decision-search`,
		`.cc-panel label.search>input#operation-search`,
		`.cc-panel button.primary{`,
		`background:linear-gradient(135deg,var(--cc-primary),var(--cc-primary-strong))!important`,
		`.cc-panel .key-table tr.selected{`,
		`.cc-panel .key-table tbody tr[role="button"].selected{`,
		`table-layout:fixed!important`,
		`outline:0!important`,
		`.cc-panel #keys .pill{background:transparent!important;box-shadow:none!important}`,
		`.cc-panel .logs .tab[aria-selected="true"]`,
		`html[data-cpa-theme="dark"] .cc-panel .stats .stat-icon.violet`,
		`html[data-cpa-theme="dark"] .cc-panel .decision.allow`,
		`html[data-cpa-theme="dark"] .cc-panel .decision.reject`,
		`html[data-cpa-theme="dark"] .cc-panel .level.info`,
		`html[data-cpa-theme="dark"] .cc-panel .log-http.ok`,
		`statusTone=!account.enabled?`,
		`pool-account-status '+statusTone`,
		`cells.length<10`,
		`badge=cells[5].querySelector('.decision')`,
		`cells[9].textContent=description`,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("panel HTML missing interactive-state marker %q", expected)
		}
	}
	if strings.Contains(page, `'--blue':['--el-color-primary']`) ||
		strings.Contains(page, `'--green':['--el-color-success']`) ||
		strings.Contains(page, `'--red':['--el-color-danger']`) {
		t.Fatal("CPA theme synchronization must not replace the plugin's semantic brand and status colors")
	}
	if got := strings.Count(page, `if(from===en&&en==='To')`); got < 2 {
		t.Fatalf("standalone To localization guards = %d, want both core and log guards so Token is never rewritten", got)
	}
	if strings.Contains(page, `cells[2].textContent=output.decision`) ||
		strings.Contains(page, `cells[3].textContent=output.description+account`) ||
		strings.Contains(page, `cells[2].textContent=decision`) ||
		strings.Contains(page, `cells[3].textContent=description+account`) {
		t.Fatal("decision localization must not overwrite the Key fingerprint or model columns")
	}
	if strings.Contains(page, `id="catalog-state"`) {
		t.Fatal("the retired official-sync tag must not remain in the account-pool header")
	}
	if got := strings.Count(page, `badge=cells[5].querySelector('.decision')`); got < 2 {
		t.Fatalf("decision badge column guards = %d, want both app and bootstrap guards", got)
	}
}

func TestPanelEnglishLocaleCoversStaticAndDynamicDashboardCopy(t *testing.T) {
	page := panelHTML()
	for _, expected := range []string{
		`Latest model sync: `,
		`Quota diagnostics`,
		`Unconfigured CPA Keys remain unrestricted`,
		`Official weekly quota · Real-time account health`,
		`Official quota is synchronized on demand and every 5 minutes`,
		`Requests safely return 503 when no current official quota snapshot is available.`,
		`Search model, reason, or account ID`,
		`Search event, description, account, or Key ID`,
		`Actual Token trend chart`,
		`Quota diagnostic content`,
		`Per page 10`,
		`Total ')+num(total)+uiText(' 条 · 每页 ',' · Per page '`,
		`uiText('已耗尽','Exhausted')`,
		`uiText('可用','Available')`,
		`uiText('容量','capacity')`,
		`uiText('额度诊断','Quota diagnostics')`,
		`const renderHostLabel=()=>`,
		`english()?'Codex Carpool':'Codex 拼车'`,
		`Usage, policy decisions, and Token settlements for the selected Key`,
		`Plugin lifecycle, configuration, official quota sync, and exception records`,
		`Configure shared account pool`,
		`CPA authentication directory`,
		`Pause management (unrestricted)`,
		`Forbidden-phrase filtering`,
		`Forbidden-phrase logs`,
		`Original CPA Key suffix: `,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("panel HTML missing bilingual marker %q", expected)
		}
	}
}

func TestPanelUsesScopedBilingualLoadingFeedback(t *testing.T) {
	page := panelHTML()
	for _, expected := range []string{
		`const loadingTargets={full:`,
		`node.setAttribute('aria-busy','true')`,
		`Switching Key…`,
		`Loading usage analysis…`,
		`Loading usage logs…`,
		`Loading runtime logs…`,
		`Loading forbidden-phrase logs…`,
		`Refreshing official quota…`,
		`selected!==state.selected`,
		`requestID!==decisionRequestID`,
		`requestID!==operationRequestID`,
		`.cc-loading-overlay`,
		`class="cc-loading-title"`,
		`.cc-panel .cc-loading-title{`,
		`@media(prefers-reduced-motion:reduce)`,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("panel HTML missing loading-feedback marker %q", expected)
		}
	}
	if strings.Contains(page, `document.body.classList.add('cc-module-loading')`) {
		t.Fatal("loading feedback must stay scoped to dashboard modules")
	}
}

func TestPanelUsesEmbeddedTemplateWithBoundedModelSyncAndObservation(t *testing.T) {
	page := panelHTML()
	if strings.Contains(panelTemplate, `strings.ReplaceAll(page,`) {
		t.Fatal("embedded panel must not retain runtime rewrite chains")
	}
	if strings.Contains(page, `Promise.all(auths.map`) {
		t.Fatal("model synchronization must not fan out unbounded requests per account")
	}
	for _, expected := range []string{
		`modelCatalogFreshForMs=30*60*1000`,
		`modelSyncConcurrency=2`,
		`mapWithConcurrency`,
		`if(auths.length){const results=await mapWithConcurrency`,
		`window.__ccSyncLocale`,
		`__ccFetchTimeoutInstalled`,
		`mutationTimeoutMs=30000`,
		`const requestSignal=typeof Request!=='undefined'&&input instanceof Request?input.signal:undefined;`,
		`__ccPanelBridge?.attach?.`,
		`__ccPanelBridge?.afterRender?.()`,
		`function render(){renderStats();renderKeys();renderDetail();renderLogs();renderOperationalLogs();window.__ccPanelBridge?.afterRender?.()}`,
		`decision-log-table`,
		`log-key-fingerprint`,
		`allowed.slice(0,3)`,
		`title="'+escapeHTML(allModels)+'"`,
		`usage-line-canvas`,
		`line-chart-tooltip`,
		`context.strokeStyle='#168a67'`,
		`window.__ccRenderUsageLineChart=renderUsageLineChart`,
		`window.state=panel.state`,
		`window.tokens=panel.tokens`,
		`batchLocalizer('__ccLocalizeCore')`,
		`window.requestAnimationFrame||window.setTimeout`,
		`observer.observe(document.body,{childList:true,subtree:true})`,
		`Account-pool configuration removed.`,
		`Select a managed Key first.`,
		`Generate quota diagnostics first.`,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("panel HTML missing bounded-performance marker %q", expected)
		}
	}
	if strings.Contains(page, `function render(){renderStats();renderKeys();renderDetail();renderLogs();renderOperationalLogs()}window.__ccPanelBridge?.afterRender?.()`) {
		t.Fatal("panel render bridge runs outside render(), so async data refreshes can restore the legacy detail layout")
	}
	if strings.Contains(page, `<td class="decision `) {
		t.Fatal("decision badge classes must not replace table-cell display semantics")
	}
	// Two observers mirror CPA theme state and one batched observer localizes
	// newly rendered fragments. Per-localizer observers remain disabled so a
	// log refresh cannot trigger several full subtree scans.
	if got := strings.Count(page, `new MutationObserver`); got != 3 {
		t.Fatalf("active panel observer count = %d, want 3 bounded observers", got)
	}
	if strings.Contains(page, `window.MutationObserver=`) {
		t.Fatal("panel must not override the browser MutationObserver API")
	}
	observerStart := strings.LastIndex(page, `const observer=new MutationObserver`)
	if observerStart < 0 || strings.Contains(page[observerStart:], `characterData`) {
		t.Fatal("live locale observation must not rescan text mutations after every dynamic render")
	}
	if strings.Contains(page[observerStart:], `observer.observe(document.documentElement`) {
		t.Fatal("live locale observation must not watch the plugin's own lang attribute")
	}
	if !strings.Contains(page, `event.stopPropagation();const key=keyFor`) {
		t.Fatal("Key row actions must stop click propagation before editing or deleting")
	}
	if strings.Contains(page, `[' 页','']`) || !strings.Contains(page, `[' 页','\u200B']`) {
		t.Fatal("page localization must not use an empty reverse page-suffix mapping")
	}
	if strings.Contains(page, panelUnsafeLogTranslator) || !strings.Contains(page, `output.replace(/\bTo\b/g,to)`) {
		t.Fatal("page localization must preserve Token while translating the standalone word To")
	}
	if strings.Contains(page, `window.addEventListener('languagechange',sync)`) {
		t.Fatal("panel must not retain the pre-refactor locale handler")
	}
	// Adjacent IIFEs are used for isolated panel enhancements. The separator is
	// essential: without it, the browser tries to invoke the first IIFE result.
	if strings.Contains(page, `dialog.addEventListener('close',()=>clear(dialog))})

(()=>`) {
		t.Fatal("adjacent panel IIFEs must have an explicit statement boundary")
	}
	if !strings.Contains(page, `dialog.addEventListener('close',()=>clear(dialog))});`) {
		t.Fatal("dialog error handler must terminate before the following enhancement IIFE")
	}
	coreAttach := strings.Index(page, `window.__ccPanelBridge?.attach?.({state,render,renderLogs,renderOperationalLogs,cpaLocale,uiText,tokens,showToast,say});})();`)
	dialogEnhancement := strings.Index(page, `const clear=dialog=>{const node=dialog.querySelector('.dialog-message')`)
	if coreAttach < 0 || dialogEnhancement < 0 || coreAttach > dialogEnhancement {
		t.Fatal("panel closure state must attach to the bridge before independent enhancement IIFEs run")
	}
	if strings.Contains(page[dialogEnhancement:], `dialogMessageIDs.map($)`) {
		t.Fatal("independent dialog enhancement must not reference main-IIFE private identifiers")
	}
}

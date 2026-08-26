//go:build linux && cgo

package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestPanelUsesDollarBudgetsAndNoAccountPoolUI(t *testing.T) {
	page := panelHTML()
	for _, marker := range []string{
		`id="five-hour-budget"`, `id="seven-day-budget"`, `id="rate-card-dialog"`,
		`id="allowed-models-list"`, `id="allowed-models-search"`, `id="allowed-models-all"`,
		`id="access-limited"`, `id="access-rule-list"`, `id="access-rule-add"`,
		`input_usd_per_million`, `cache_read_usd_per_million`, `cache_write_usd_per_million`, `reasoning_usd_per_million`, `output_usd_per_million`,
		`id="rate-sync-toggle" role="switch"`, `models.dev 价格同步`, `async function toggleRateSync()`,
		`function enabledCPAAuthEntries(payload)`, `host('/auth-files')`,
		`host('/auth-files/models?name='+encodeURIComponent(value(auth.id||auth.name)))`,
		`Promise.allSettled(auths.map(auth=>`, `!file?.disabled&&status!=='disabled'`,
		`existing catalog was kept; retry later`,
		`(payload.models||[]).filter(model=>model.available!==false)`,
		`<th>5 小时</th>`, `<th>7 天</th>`, `<th>消耗（USD）</th>`,
		`function dollarBudgetText(window)`, `key.actual_tokens||{}`,
		`function extractCPAKeys(payload)`, `payload?.apiKeys`, `payload?.keys`, `payload?.data`,
		`allowed_models:Array.from($('policy-dialog').__allowedModels||[])`,
		`function renderPolicyModels()`,
		`function readAccessPolicy()`, `$(name+'-log-view')`, `$(name+'-log-tools')`,
		`input_cost_micros`, `cached_cost_micros`, `output_cost_micros`,
		`function actualTotalText(key)`, `function renderRangeTokenBreakdown(stats)`,
		`id="analysis-from"`, `id="analysis-to"`, `id="analysis-granularity"`,
		`id="content-filter-open"`, `id="logs"`, `id="operation-logs"`,
		`内容正则拦截`, `使用安全的 RE2 正则表达式`, `maxlength="512" spellcheck="false"`,
		`搜索表达式或分类`, `添加 RE2 正则表达式`, `命中表达式`,
		`settings||{enabled:true,terms:[]}`, `const builtin=item.term.source==='builtin'`,
		`data-remove-term="'+item.index+'"`, `uiText('内置','Built-in')`,
		`id="decision-log-tools"`, `id="log-detail-dialog"`,
		`class="meter-workbench card"`, `id="key-detail-panel"`,
		`id="key-detail-backdrop"`, `id="key-status-filter"`,
		`class="key-analysis-drawer"`, `class="line-chart"`,
		`function localDateISO(date=new Date())`, `function elapsedTrendPoints(points,now=Date.now())`,
		`const values=elapsedTrendPoints(points).slice(-12)`, `const values=elapsedTrendPoints(points).slice(-24)`,
		`uiText('今日暂无','None today')`, `uiText('暂无活跃数据','No activity data')`,
		`class="budget-refresh"`, `uiText('下次刷新','Next refresh')`, `window?.refresh_at`,
		`budgetBoundaryTimer=0`, `function scheduleBudgetBoundaryRefresh()`,
		`Math.min(...boundaries)-now+150`, `scheduleBudgetBoundaryRefresh();render()`,
		`class="chart-tooltip" role="tooltip" hidden`, `data-chart-point="`,
		`function chartTooltipHTML(point)`, `function wireLineChartTooltip(node,values)`,
		`point.input_tokens`, `point.cached_tokens`, `point.output_tokens`, `point.cost_micros`,
		`id="cost-donut"`, `id="model-ranking"`, `id="usage-heatmap"`,
		`id="analysis-log-snapshot"`, `id="risk-list"`,
		`正在连接用量管理`, `插件管理&nbsp; / &nbsp;用量管理</b><span class="utility-meta">`,
		`已连接用量管理`, `Connected to usage management`,
		`class="version-badge">v`, `class="github-link"`,
		`['用量管理','Usage management']`, `const translateExact=value=>`,
		`document.title=text('用量管理','Usage management')`,
		`<h2>用量管理</h2>`, `<strong>已添加 Key</strong>`,
		`uiText('已添加 Key','Added Keys')`, `number((state.summary.keys||[]).length)`,
		`uiText('额度限制','Budget enforced')`, `uiText('仅统计','Track only')`,
		`启用额度限制`, `仅统计（超额不限制）`,
		`Key 添加后的首个请求分别启动固定 5 小时和 7 天周期，到点整体刷新`,
		`模型或别名必须在费率设置中存在`,
		`账本持久化异常，已添加 Key 请求暂停`,
		`移除 Key`, `uiText('从用量管理中移除此 Key？','Remove this Key from usage management?')`,
		`uiText('额度设置已保存。','Quota settings saved.')`,
		`securePrefix='enc::v1::'`, `secureSalt='cli-proxy-api-webui::secure-storage'`,
		`JSON.parse(decode(raw)||'null')`, `dataset.toastKey`,
		`await loadKeys();openPolicy(null)`,
		`async function loadKeys(){const payload=await host('/config')`,
		`uiText('缓存读取','Cache read')`, `uiText('缓存写入','Cache write')`, `uiText('推理','Reasoning')`,
		`existing?.source==='models.dev'&&state.rateSync?.enabled`,
		`rates.push({...existing,...edited,source:'manual',provider:'',tiers:[],modes:[]`, `if(unchanged){rates.push(existing);return}`,
		`async function followRequestedRateSync(previousAttempt)`, `rateSyncFollowupGeneration`, `state.rateSync?.last_attempt`,
		`sync.retired_models||0`, `uiText('匹配 / 未匹配 / 已移除：'`,
		`function renderRateCapabilityNotice()`, `service_tier 为 auto 或未知时使用基础价`,
		`id="rate-card-search"`, `id="rate-card-status-filter"`, `id="rate-card-filter-empty"`,
		`function filterRateRows()`, `function updateRateRowStatus(row)`,
		`data-rate-search="'+escapeHTML(searchText)+'"`, `data-rate-status="'+status+'"`,
		`const autoRefreshIntervalMS=5*60*1000`,
		`document.visibilityState!=='visible'`,
		`window.setInterval(refreshIfDue,autoRefreshIntervalMS)`,
		`document.addEventListener('visibilitychange',refreshIfDue)`,
		`refreshPageData({loading:false,resetLogs:false})`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("panel HTML missing %q", marker)
		}
	}
	for _, retired := range []string{`official-account-pool`, `account-dialog`, `auth-directory-dialog`, `account-pool`, `五小时倍率`, `allocation_x`, `账号池`, `class="heading page-head"`, `<h1>codex-carpool`, `CPA Key 美元计量 · 全模型费率 · 5 小时与 7 天滚动预算`} {
		if strings.Contains(page, retired) {
			t.Fatalf("new panel still contains retired account-pool/x marker %q", retired)
		}
	}
	if strings.Contains(page, `function cycleTokenText(`) || strings.Contains(page, `周期 Token`) {
		t.Fatal("new panel must not depend on the retired official quota cycle")
	}
	for _, retired := range []string{`modelDefinitionChannels`, `host('/model-definitions/`, `支持的 Codex 模型`, `call('','/v1/models'`} {
		if strings.Contains(page, retired) {
			t.Fatalf("new panel still contains retired Codex-only model-sync marker %q", retired)
		}
	}
	if strings.Contains(page, `Promise.all([loadKeys(),loadSummary(),loadModels(),loadRates()])`) {
		t.Fatal("panel boot must not depend on CPA API-key or wallet state")
	}
	for _, retired := range []string{`host('/api-keys')`, `wallet must has at least one account`, `现有面板不受影响`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains wallet-dependent CPA Key loading marker %q", retired)
		}
	}
	if strings.Contains(page, `unhandledrejection`) {
		t.Fatal("panel must not surface unrelated CPA host promise rejections as plugin errors")
	}
	for _, retired := range []string{`uiText('管理中 Key','Managed Keys')`, `暂停管理（不限额）`, `解除管理`, `Remove management`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains misleading stopped-accounting copy %q", retired)
		}
	}
	for _, retired := range []string{`正在连接 codex-carpool`, `已连接 codex-carpool`, `插件管理&nbsp; / &nbsp;codex-carpool`, `Connecting to codex-carpool`, `Connected to codex-carpool`, `Plugin management / codex-carpool`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains retired product name %q", retired)
		}
	}
	for _, retired := range []string{`class="collapsed-analysis-note"`, `统计图默认收起`, `Charts are collapsed by default`, `.cc-panel .collapsed-analysis-note`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains redundant collapsed-chart notice %q", retired)
		}
	}
	for _, retired := range []string{`>违禁词拦截<`, `>违禁词日志<`, `>关键词列表<`, `placeholder="添加关键词"`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains retired literal content-filter copy %q", retired)
		}
	}
}

func TestPanelUsesProductionStylesAndPreviewSource(t *testing.T) {
	page := panelHTML()
	for _, marker := range []string{
		`.cc-panel`, `label.search>input`, `position:sticky!important`,
		`#content-filter-dialog .content-filter-search input`, `#rate-card-dialog`,
		`#policy-dialog .policy-models{display:grid!important`,
		`#policy-dialog .policy-model-item{display:flex!important`,
		`--cc-primary:#168a67`, `target="_blank" rel="noopener noreferrer"`,
		`.cc-panel .utility-meta{display:inline-flex`, `.cc-panel .version-badge{display:inline-flex`,
		`.cc-panel .meter-workbench`, `.cc-panel .key-analysis-drawer`,
		`#key-detail-backdrop{position:fixed!important;inset:0!important`,
		`#key-detail-panel{position:fixed!important;inset:0 0 0 auto!important`,
		`#key-detail-panel>.drawer-body{position:relative!important;display:flex!important`,
		`#key-detail-panel>.drawer-body>*{flex:0 0 auto!important;width:100%!important`,
		`#key-detail-panel .drawer-section{height:auto!important;min-height:0!important;overflow:hidden!important`,
		`#key-detail-panel .cc-icon{display:block!important;width:16px!important`,
		`#key-detail-panel{left:0!important;right:0!important;width:100%!important`,
		`#key-detail-panel .detail-section-head{align-items:flex-start!important;flex-direction:column!important`,
		`.cc-panel .key-action-column,.cc-panel .key-action{position:sticky!important`,
		`.cc-panel .log-action::before,.cc-panel .log-action::after,.cc-panel .row-action::before,.cc-panel .row-action::after{display:none!important;content:none!important}`,
		`.cc-panel .log-action{font-size:0!important}`,
		`.cc-panel .row-action{min-height:30px;padding:0 8px;border-radius:8px;font-size:13px!important}`,
		`#policy-dialog[open],#rate-card-dialog[open],#log-detail-dialog[open],#key-log-dialog[open],#content-filter-dialog[open]{display:flex!important`,
		`#policy-dialog>header,#rate-card-dialog>header,#log-detail-dialog>header,#key-log-dialog>header,#content-filter-dialog>header`,
		`#policy-dialog>.form,#rate-card-dialog>.form,#log-detail-dialog>.form,#key-log-dialog>.form,#content-filter-dialog>.form`,
		`#policy-dialog button,#rate-card-dialog button,#log-detail-dialog button,#key-log-dialog button,#content-filter-dialog button`,
		`#policy-dialog button:focus-visible,#rate-card-dialog button:focus-visible,#log-detail-dialog button:focus-visible,#key-log-dialog button:focus-visible,#content-filter-dialog button:focus-visible`,
		`#rate-card-dialog .rate-card-row{margin-top:8px!important`,
		`class="rate-card-scroll"`,
		`#rate-card-dialog>.rate-card-form{display:flex!important;flex-direction:column!important;min-height:0!important;overflow:hidden!important`,
		`#rate-card-dialog .rate-card-table{display:flex!important;flex:1 1 auto!important;min-height:0!important`,
		`#rate-card-dialog .rate-card-scroll{flex:1 1 auto!important;min-width:1010px!important;min-height:0!important`,
		`#rate-card-dialog .rate-sync-bar{display:flex!important`,
		`#rate-card-dialog #rate-sync-toggle[aria-checked="true"] .rate-sync-track`,
		`#rate-card-dialog .rate-card-tools{display:grid!important`,
		`#rate-card-dialog label.rate-card-search{position:relative!important`,
		`#rate-card-dialog .rate-card-row[hidden],#rate-card-dialog .rate-card-filter-empty[hidden]{display:none!important`,
		`#log-detail-dialog .log-detail-grid{display:grid!important`,
		`#policy-dialog .dialog-message,#rate-card-dialog .dialog-message,#content-filter-dialog .dialog-message`,
		`id="content-filter-enabled" type="checkbox" role="switch"`,
		`class="content-filter-switch" aria-hidden="true"`,
		`class="content-filter-count">0 / 0`,
		`#content-filter-dialog label.content-filter-toggle{position:relative!important;display:grid!important`,
		`#content-filter-dialog label.content-filter-toggle>input,#content-filter-dialog label.content-filter-term-switch>input{position:absolute!important;width:1px!important`,
		`#content-filter-dialog .content-filter-list-head,#content-filter-dialog .content-filter-term{display:grid!important`,
		`class="content-filter-term-switch"`,
		`#content-filter-dialog #content-filter-value{font-family:`,
		`#content-filter-dialog .content-filter-fixed{justify-self:end!important`,
		`#content-filter-dialog .content-filter-help{display:block!important`,
		`id="access-limited" type="checkbox" role="switch"`,
		`class="policy-switch" aria-hidden="true"`,
		`#policy-dialog .schedule-toggle input{position:absolute!important;width:1px!important`,
		`#policy-dialog .policy-model-item input{flex:0 0 auto!important;width:16px!important;min-width:16px!important;height:16px!important;min-height:16px!important`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("panel HTML missing style guard %q", marker)
		}
	}
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("resolve panel test source path")
	}
	previewPath := filepath.Join(filepath.Dir(sourceFile), "..", "..", "ui-preview", "dollar-preview.html")
	raw, err := os.ReadFile(previewPath)
	if err != nil {
		t.Fatalf("read dollar preview: %v", err)
	}
	preview := string(raw)
	for _, marker := range []string{`../cmd/codex-carpool/web/index.html`, `../cmd/codex-carpool/web/styles.css`, `../cmd/codex-carpool/web/app.js`, `managementKey`, `keyDialogLogs`} {
		if !strings.Contains(preview, marker) {
			t.Fatalf("dollar preview missing %q", marker)
		}
	}
}

func TestPanelKeepsSearchAndOperationColumnsHostSafe(t *testing.T) {
	page := panelHTML()
	for _, marker := range []string{
		`border:0!important`, `box-shadow:none!important`,
		`.cc-panel label.search>.cc-icon{position:absolute!important`,
		`.cc-panel label.search>.search-clear{position:absolute!important`,
		`.cc-panel label.search>.search-clear[hidden]{display:none!important}`,
		`.cc-panel .mini-trend .mini-dot{fill:var(--cc-primary)`,
		`.cc-panel .mini-trend-empty{display:grid!important`,
		`.cc-panel .usage-overview-grid{--usage-overview-height:382px;display:grid!important;grid-template-columns:minmax(0,1fr) 340px!important;align-items:stretch!important`,
		`.cc-panel .usage-overview-grid>.key-table-wrap{align-self:stretch!important`,
		`overflow:auto!important;overscroll-behavior:contain!important;scrollbar-gutter:stable!important`,
		`.cc-panel .key-table-caption{position:sticky!important;top:0!important;left:0!important`,
		`.cc-panel .key-table thead th{position:sticky!important;top:49px!important`,
		`.cc-panel .key-table th:nth-child(1){width:14%}`,
		`.cc-panel .key-table{min-width:800px;table-layout:fixed}`,
		`.cc-panel .key-table th:nth-child(4){width:19%}`,
		`.cc-panel .key-table th:nth-child(7){width:17%;text-align:right}`,
		`.cc-panel .key-table th,.cc-panel .key-table td{padding-right:10px;padding-left:10px}`,
		`.cc-panel .key-table .budget-cell em{white-space:nowrap}`,
		`.cc-panel .key-action{padding-right:4px!important;padding-left:4px!important}`,
		`.cc-panel .key-row-actions{display:flex!important;align-items:center!important;justify-content:flex-end!important`,
		`.cc-panel .key-table .pill{white-space:nowrap!important}`,
		`.cc-panel .mini-trend{display:block;width:100%;max-width:132px;height:34px}`,
		`.cc-panel .daily-model-card{isolation:isolate!important;align-self:stretch!important;display:flex!important`,
		`.cc-panel .daily-model-ranking{display:flex!important;flex:1 1 auto!important;flex-direction:column!important`,
		`overflow-x:hidden!important;overflow-y:auto!important`,
		`.cc-panel .usage-overview-grid>.key-table-wrap,.cc-panel .daily-model-card{width:100%!important;height:var(--usage-overview-height)!important}`,
		`id="daily-model-ranking" class="daily-model-ranking"`,
		`id="daily-model-total" class="daily-model-total"`,
		`function renderDailyModelRanking()`, `items=snapshot.models||[]`,
		`uiText(' 个',' models')`, `今日有用量的模型数量：`,
		`api('/model-ranking?'+params)`,
		`uiText('今日合计','Today total')`,
		`<td colspan="7" class="empty">'+uiText('暂无已添加 Key`,
		`.cc-panel .budget-refresh{display:flex!important`,
		`.cc-panel .chart-tooltip{position:absolute!important`,
		`.cc-panel .chart-tooltip[hidden]{display:none!important}`,
		`.cc-panel .line-chart .chart-hit:hover,.cc-panel .line-chart .chart-hit:focus`,
		`#policy-dialog .policy-model-search>.cc-icon{position:absolute!important`,
		`#content-filter-dialog .content-filter-search>.cc-icon{position:absolute!important`,
		`padding:0 10px 0 36px!important`, `text-indent:0!important`,
		`.cc-panel #toast-region{position:fixed!important;top:22px!important;right:22px!important;bottom:auto!important;left:auto!important`,
		`id="key-detail-panel"`, `class="key-analysis-drawer"`, `class="log-action-column"`,
		`position:sticky!important;right:0`, `aria-live="assertive"`,
		`.cc-panel .log-tools[hidden],.cc-panel .log-view[hidden]{display:none!important}`,
		`.cc-panel .decision-log-table{min-width:1678px!important`,
		`.cc-panel .operation-log-table{min-width:1280px!important`,
		`.cc-panel .forbidden-log-table{min-width:1505px!important`,
		`.cc-panel .log-table th,.cc-panel .log-table td{min-width:0!important;overflow:hidden!important;text-overflow:ellipsis!important;white-space:nowrap!important}`,
		`.cc-panel .log-table th:first-child,.cc-panel .log-table td:first-child{min-width:200px!important;width:200px!important;max-width:200px!important;overflow:visible!important;text-overflow:clip!important;white-space:nowrap!important}`,
		`class="log-request-preview"><span>`, `function previewText(input,max=96)`,
		`uiText('管理','Manage')`, `uiText('日志','Logs')`, `点击“管理”查看详细分析`,
		`selectedLogs:[]`, `keyLogs:[]`, `keyTrends:{}`, `dailyModelUsage:{models:[]}`,
		`id="key-log-dialog"`, `id="key-logs"`, `data-key-log="`,
		`function renderKeyLogs()`, `async function loadKeyLogs(reset=false)`, `function openKeyLogs(keyID)`,
		`key_id:state.keyLogKeyID`, `page_size:'10'`,
		`id="key-log-from"`, `id="key-log-to"`, `id="key-log-search"`, `id="key-log-query"`, `id="key-log-filter-clear"`,
		`data-clear-search="key-log-search" data-clear-query="key-log-query"`, `params.from=from.toISOString()`, `params.to=to.toISOString()`, `params.query=query`,
		`id="key-log-prev"`, `id="key-log-page-number"`, `id="key-log-next"`,
		`#key-log-dialog>.key-log-form{display:grid!important;grid-template-rows:auto minmax(0,1fr)!important`,
		`#key-log-dialog .key-log-filters{display:grid!important`,
		`#key-log-dialog .key-log-table thead th{position:sticky!important;top:0!important`,
		`#key-log-dialog .key-log-table{min-width:1120px!important;table-layout:fixed!important`,
		`#key-log-dialog>footer{justify-content:space-between!important`,
		`['Key 请求日志','Key request logs']`,
		`const range=todayRange(),keys=state.summary.keys||[]`,
		`function loadSelectedLogs()`,
		`const params=new URLSearchParams({page:String(state.decisionPage.page||1)`,
		`id="decision-query"`, `id="operation-query"`, `id="forbidden-query"`,
		`id="log-storage-summary"`, `id="log-database-size"`, `id="usage-log-size"`, `id="forbidden-log-size"`, `id="operation-log-size"`,
		`async function loadLogStorage()`, `api('/log-storage')`, `function renderLogStorage()`,
		`.cc-panel .log-storage-summary{display:flex!important`,
		`const wireLogQuery=(name,loader)=>`, `api('/logs',{method:'DELETE'})`,
		`data-clear-search="search"`,
		`data-clear-search="decision-search" data-clear-query="decision-query"`,
		`data-clear-search="operation-search" data-clear-query="operation-query"`,
		`data-clear-search="forbidden-search" data-clear-query="forbidden-query"`,
		`function wireSearchClears()`,
		`input.dispatchEvent(new Event('input',{bubbles:true}))`,
		`['清除搜索','Clear search']`,
		`cpaLocale:locale`,
	} {
		if !strings.Contains(page, marker) {
			t.Fatalf("panel missing host-isolation marker %q", marker)
		}
	}
	if strings.Contains(page, `(snapshot.models||[]).slice(0,5)`) {
		t.Fatal("daily model ranking must render every returned model and use its scroll container")
	}
	for _, retired := range []string{`<th>最近模型</th>`, `['最近模型','Latest model']`, `keyLatestModels`, `<td colspan="8" class="empty">'+uiText('暂无已添加 Key`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains retired latest-model column marker %q", retired)
		}
	}
	for _, retired := range []string{`.cc-panel #keys tr{cursor:pointer}`, `.cc-panel #keys tr.selected`, `row.classList.toggle('selected'`, `tabindex="0" role="button"`} {
		if strings.Contains(page, retired) {
			t.Fatalf("Key list still contains retired row-selection marker %q", retired)
		}
	}
	if strings.Contains(page, `uiText('待确认 +','Provisional +')`) || strings.Contains(page, `allocation_x`) {
		t.Fatal("panel must not render legacy provisional or x allocation values")
	}
	if strings.Contains(page, `key-detail-drawer`) {
		t.Fatal("panel must not use the retired oversized detail drawer")
	}
	for _, retired := range []string{`Key 额度管理`, `Key 美元额度策略已保存`, `美元消耗`, `编辑策略`, `受控 Key`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still contains retired user-facing copy %q", retired)
		}
	}
	if strings.Contains(page, `uiText('展开','Open')`) || strings.Contains(page, `点击“展开”`) {
		t.Fatal("Key list management action must not retain the retired expand label")
	}
	if strings.Count(page, `清除日志`) < 3 || strings.Contains(page, `>清除</button>`) {
		t.Fatalf("all three log tabs must use the explicit clear-log copy, got %d", strings.Count(page, `清除日志`))
	}
	if count := strings.Count(page, `class="search-clear"`); count != 5 {
		t.Fatalf("dashboard and Key-log searches must expose five stable inline clear controls, got %d", count)
	}
	for _, retired := range []string{`.slice(0,30)`, `latestModel=(state.logs||[]).find`, `api('/logs?key_id='+encodeURIComponent(state.selected),{method:'DELETE'})`, `key_id:state.selected,page:String(state.decisionPage.page`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel still couples public Key data or logs to selection via %q", retired)
		}
	}
	for _, retired := range []string{`class="weekly-bars"`, `.cc-panel .weekly-bars`, `.cc-panel .bars{`, `sparkBars`} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel must not keep retired bar-only trend marker %q", retired)
		}
	}
	for _, retired := range []string{`function defaultSparkPoints(`, `const iso=now.toISOString().slice(0,10)`, `from.toISOString().slice(0,10)`, `today.toISOString().slice(0,10)`} {
		if strings.Contains(page, retired) {
			t.Fatalf("today trend must not use synthetic data or UTC calendar dates via %q", retired)
		}
	}
	for _, retired := range []string{
		"\n.dialog-message{",
		"\n.dialog-message.ok{",
		"\n.rate-card-head{",
		"\n.rate-card-row{",
		"\n.rate-status{",
		"\n.rate-card-add,",
		"\n.log-detail-grid{",
		"\n.log-detail-section{",
	} {
		if strings.Contains(page, retired) {
			t.Fatalf("panel must not keep unscoped dialog style marker %q", retired)
		}
	}
}

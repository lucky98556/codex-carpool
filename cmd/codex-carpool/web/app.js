/* Frontend application: talks only to the plugin JSON API. */
(()=>{const root=document.documentElement,sync=()=>{let value='';try{const parentRoot=window.parent.document.documentElement;value=String(parentRoot.dataset.theme||parentRoot.dataset.colorScheme||parentRoot.className||'').toLowerCase()}catch{};root.dataset.cpaTheme=value.includes('dark')?'dark':(value.includes('light')?'light':'')};sync();try{const parentRoot=window.parent.document.documentElement;new MutationObserver(sync).observe(parentRoot,{attributes:true,attributeFilter:['class','data-theme','data-color-scheme']})}catch{};matchMedia('(prefers-color-scheme: dark)').addEventListener?.('change',sync)})();

(()=>{
const base='/v0/management/codex-carpool',authKey='cli-proxy-auth',legacyKey='managementKey',selectedKeyStorage='codex-carpool:selected-key',prefix='enc::v1::',salt='cli-proxy-api-webui::secure-storage';
const state={summary:{keys:[],status:{},accounts:[]},policies:[],models:[],logs:[],decisionPage:{page:1,page_size:10,total:0,total_pages:0},operationLogs:[],operationPage:{page:1,page_size:10,total:0,total_pages:0},trend:{points:[]},rawKeys:[],rawAccounts:[],selected:(()=>{try{return String(localStorage.getItem(selectedKeyStorage)||'').trim()}catch{return ''}})(),editing:null,modelSyncError:''};const $=id=>document.getElementById(id);const text=v=>String(v??'').trim();const escapeHTML=v=>{const node=document.createElement('span');node.textContent=String(v??'');return node.innerHTML};
const num=v=>new Intl.NumberFormat(state.locale==='en'?'en-US':'zh-CN',{maximumFractionDigits:2}).format(Number(v)||0);const tokens=v=>{const n=Number(v)||0;if(n>=1000000)return num(n/1000000)+'M';if(n>=1000)return num(n/1000)+'K';return num(n)};
function showToast(value,ok=false){if(!value)return;let region=document.getElementById('toast-region');if(!region){region=document.createElement('div');region.id='toast-region';region.setAttribute('aria-live','assertive');region.setAttribute('aria-atomic','true');region.style.cssText='position:fixed;z-index:10000;top:18px;right:18px;display:grid;gap:9px;width:min(420px,calc(100vw - 36px));pointer-events:none';document.body.append(region)}const toastKey=(ok?'ok:':'error:')+String(value),duplicate=Array.from(region.children).find(node=>node.dataset.toastKey===toastKey);if(duplicate)return;const item=document.createElement('div');item.dataset.toastKey=toastKey;item.setAttribute('role',ok?'status':'alert');item.tabIndex=0;item.textContent=String(value);item.style.cssText='pointer-events:auto;padding:12px 14px;border:1px solid '+(ok?'rgba(56,161,105,.32)':'rgba(224,79,95,.35)')+';border-radius:11px;background:'+(ok?'#effaf3':'#fff4f5')+';box-shadow:0 14px 36px rgba(15,23,42,.18);color:'+(ok?'#217a4b':'#b42334')+';font-size:13px;line-height:1.5;cursor:pointer';const close=()=>item.remove();item.onclick=close;region.append(item);setTimeout(close,ok?3600:6500)}
const dialogMessageIDs=['policy-dialog','account-dialog','auth-directory-dialog','content-filter-dialog','quota-debug-dialog'];function activeDialog(){return dialogMessageIDs.map($).find(dialog=>dialog?.open)||null}function clearDialogMessage(dialog){const node=dialog?.querySelector('.dialog-message');if(!node)return;node.hidden=true;node.textContent='';node.className='dialog-message'}function friendlyMessage(value){const area=document.createElement('textarea');area.innerHTML=String(value||'');const message=area.value.trim();if(message.includes('Key allocation')&&message.includes('exceeds the remaining shared pool'))return uiText('共享账号池剩余可分配 x 不足，无法提高该 Key 的分配。','The shared account pool does not have enough remaining x allocation to increase this Key.');return message}function say(value,ok=false){const message=friendlyMessage(value),dialog=activeDialog();if(dialog){const node=dialog.querySelector('.dialog-message');if(!message){clearDialogMessage(dialog);return}if(node){node.hidden=false;node.textContent=message;node.className='dialog-message'+(ok?' ok':'');return}}showToast(message,ok)}
function decode(raw){if(!raw||!raw.startsWith(prefix))return raw;try{const encrypted=atob(raw.slice(prefix.length)),key=new TextEncoder().encode(salt+'|'+location.host+'|'+navigator.userAgent),bytes=new Uint8Array(encrypted.length);for(let i=0;i<encrypted.length;i++)bytes[i]=encrypted.charCodeAt(i)^key[i%key.length];return new TextDecoder().decode(bytes)}catch{return ''}}
function managementKey(){try{let raw=decode(localStorage.getItem(authKey)||''),stored=JSON.parse(raw||'{}'),key=text(stored?.state?.managementKey||stored?.managementKey);if(key)return key;raw=decode(localStorage.getItem(legacyKey)||'');stored=JSON.parse(raw||'null');if(typeof stored==='string'&&stored)return stored;if(stored?.managementKey)return stored.managementKey}catch{}throw new Error(uiText('未找到 CPAMP 已保存的管理密钥。请在同一地址登录 CPAMP 并启用记住管理密钥。','CPAMP has no saved management key. Sign in to CPAMP at this address and enable remembering the management key.'))}
const cpaLocale=()=>{let value=String(navigator.language||'en-US');try{const root=parent.document.documentElement;value=String(root.lang||root.dataset.locale||root.dataset.language||root.getAttribute('data-locale')||root.getAttribute('data-language')||value)}catch{}return /^zh\b/i.test(value)?'zh-CN':'en-US'};const uiText=(chinese,english)=>cpaLocale().startsWith('zh')?chinese:english;async function call(root,path,options={}){const response=await fetch(root+path,Object.assign({},options,{headers:Object.assign({'Authorization':'Bearer '+managementKey(),'Accept-Language':cpaLocale()},options.headers||{})}));const data=await response.json().catch(()=>({error:uiText('接口未返回 JSON','Response did not contain JSON')}));if(!response.ok)throw new Error(data.error||uiText('请求失败 ','Request failed ')+response.status);return data}const api=(path,opt)=>call(base,path,opt),host=(path,opt)=>call('/v0/management',path,opt);
// Module-level loading states preserve the current layout while each API-backed
// section refreshes independently. A small depth counter prevents overlapping
// requests from removing another request's loading layer.
const loadingTargets={full:['#stats','.key-panel>.table-wrap','.key-analysis','#official-account-pool','.logs'],key:['.key-analysis','#decision-log-view'],analysis:['.key-analysis'],decision:['#decision-log-view'],operation:['#operation-log-view'],forbidden:['#forbidden-log-view'],accounts:['#official-account-pool'],catalog:['.key-panel>.table-wrap','.key-analysis']};
let trendRequestID=0,decisionRequestID=0,operationRequestID=0;
function setModuleLoading(targets,active,label=uiText('正在加载…','Loading…')){for(const target of (Array.isArray(targets)?targets:[targets])){const node=typeof target==='string'?document.querySelector(target):target;if(!node)continue;const depth=Math.max(0,Number(node.dataset.ccLoadingDepth)||0);if(active){node.dataset.ccLoadingDepth=String(depth+1);node.classList.add('cc-module-loading');node.setAttribute('aria-busy','true');let overlay=node.querySelector(':scope>.cc-loading-overlay');if(!overlay){overlay=document.createElement('div');overlay.className='cc-loading-overlay';overlay.setAttribute('role','status');overlay.setAttribute('aria-live','polite');overlay.innerHTML='<div class="cc-loading-title"><span class="cc-loading-spinner" aria-hidden="true"></span><strong></strong></div><div class="cc-loading-lines" aria-hidden="true"><i></i><i></i><i></i></div>';node.append(overlay)}overlay.querySelector('strong').textContent=label;continue}const next=Math.max(0,depth-1);if(next){node.dataset.ccLoadingDepth=String(next);continue}delete node.dataset.ccLoadingDepth;node.classList.remove('cc-module-loading');node.removeAttribute('aria-busy');node.querySelector(':scope>.cc-loading-overlay')?.remove();node.classList.remove('cc-module-loaded');void node.offsetWidth;node.classList.add('cc-module-loaded');setTimeout(()=>node.classList.remove('cc-module-loaded'),260)}}
async function withModuleLoading(targets,task,label){setModuleLoading(targets,true,label);try{return await task()}finally{setModuleLoading(targets,false)}}
async function withButtonLoading(button,task){if(!button)return task();const disabled=button.disabled;button.disabled=true;button.classList.add('cc-button-loading');try{return await task()}finally{button.classList.remove('cc-button-loading');button.disabled=disabled}}
function chooseTheme(){try{const root=parent.document.documentElement,apply=()=>{const styles=parent.getComputedStyle(root),map={'--bg':['--el-bg-color-page','--app-bg'],'--card':['--el-bg-color','--el-fill-color-blank'],'--text':['--el-text-color-primary'],'--muted':['--el-text-color-regular'],'--line':['--el-border-color','--el-border-color-light']};for(const [local,candidates] of Object.entries(map)){for(const source of candidates){const value=styles.getPropertyValue(source).trim();if(value){document.documentElement.style.setProperty(local,value);break}}}const theme=(root.dataset.theme||root.getAttribute('data-theme')||root.className||'').toLowerCase();if(theme.includes('dark'))document.documentElement.style.setProperty('--shadow','none')};apply();new MutationObserver(apply).observe(root,{attributes:true,attributeFilter:['data-theme','class','style']})}catch{}}chooseTheme();
function policyFor(id){return state.policies.find(item=>item.id===id)}function keyFor(id){return state.summary.keys.find(item=>item.id===id)}function selectKey(id){state.selected=text(id);try{if(state.selected)localStorage.setItem(selectedKeyStorage,state.selected);else localStorage.removeItem(selectedKeyStorage)}catch{}}function suffix(key){const value=text(key?.key_suffix);return value?'••••'+value:uiText('尾号未知','Suffix unavailable')}function suffixTitle(key){const value=text(key?.key_suffix);return value?uiText('原始 CPA Key 尾号：','Original CPA Key suffix: ')+value:uiText('历史记录未保存原始 CPA Key 尾号；重新绑定后会显示。','This legacy record did not store the original CPA Key suffix; rebind it to display the suffix.')}function allocationX(window,name){const explicit=Number(window?.[name+'_x']);if(Number.isFinite(explicit))return Math.max(0,explicit);return Math.max(0,(Number(window?.[name])||0)/1000000)}function percent(window){const capacity=allocationX(window,'capacity');return capacity?Math.min(100,Math.max(0,allocationX(window,'used')/capacity*100)):0}function meter(window,green=false){return '<div class="meter '+(green?'green':'')+'"><i style="width:'+percent(window)+'%"></i></div>'}function formatTime(value){return new Date(value).toLocaleString(state.locale==='en'?'en-US':'zh-CN',{month:'numeric',day:'numeric',hour:'2-digit',minute:'2-digit',hour12:false})}
function renderSyncStatus(){const status=state.summary.status||{},warning=status.persistence_degraded?uiText('持久化异常：暂停受控 Key','Persistence issue: managed Keys are paused'):(status.account_source_conflict?uiText('认证目录正在复核或存在重复、无法确认的账号身份：暂停受控 Key','Auth directory is being verified or has duplicate or unverifiable accounts: managed Keys are paused'):(status.analysis_reader_degraded?uiText('年度用量分析正在使用受限回退连接','Annual usage analysis is using a limited fallback connection'):state.modelSyncError)),details=[];if(state.models[0]?.synced_at)details.push(uiText('最近模型同步：','Latest model sync: ')+formatTime(state.models[0].synced_at));if(status.dropped_decision_logs)details.push(uiText('已丢弃 ','Dropped ')+status.dropped_decision_logs+uiText(' 条低优先级日志',' low-priority logs'));$('sync-state').textContent=warning||uiText('已连接 codex-carpool','Connected to codex-carpool');$('sync-state').parentElement.classList.toggle('warn',Boolean(warning));$('sync-time').textContent=details.join(' · ')}
function renderStats(){
	const status=state.summary.status||{},managed=state.summary.keys.filter(key=>key.enabled).length,accounts=(state.summary.accounts||[]).filter(account=>account.enabled).length,capacity=Number(status.pool_capacity_x)||0,allocated=Number(status.allocated_x)||0;
	const items=[
		[uiText('受控 Key','Managed Keys'),managed,'shield','violet'],
		[uiText('账号池','Account pool'),accounts+uiText(' 个',''),'users','indigo'],
		[uiText('已分配','Allocated'),num(allocated)+'x','gauge','plum'],
		[uiText('可分配','Available'),num(Math.max(0,capacity-allocated))+'x','trend','purple']
	];
	$('stats').innerHTML=items.map(item=>'<article class="card"><span class="stat-icon '+item[3]+'"><svg class="cc-icon" aria-hidden="true"><use href="#cc-icon-'+item[2]+'"></use></svg></span><div><div class="stat-name">'+item[0]+'</div><div class="stat-value">'+item[1]+'</div></div></article>').join('');
	renderAccounts();
	renderSyncStatus()
}
function officialWeeklyText(window){return Number(window?.limit_window_seconds)>0?uiText('周剩余 ','Weekly remaining ')+num(Math.max(0,100-Number(window?.used_percent||0)))+'%':''}function renderAccounts(){const accounts=state.summary.accounts||[],reset=value=>value?formatTime(value):'—',poolCapacity=Number(state.summary.status?.pool_capacity_x)||accounts.reduce((sum,account)=>sum+(Number(account.capacity_x)||0),0),allocated=Number(state.summary.status?.allocated_x)||0,available=Math.max(0,poolCapacity-allocated),healthy=accounts.filter(account=>account.enabled&&account.quota?.allowed&&!account.quota?.limit_reached&&!text(account.quota?.last_error)).length;const overview='<div class="pool-overview"><div><span class="muted">'+uiText('共享池可用额度','Available shared-pool allocation')+'</span><strong>'+num(available)+'x <small class="muted">/ '+num(poolCapacity)+'x</small></strong></div><div class="pool-health"><span class="muted">'+uiText('池健康度 · ','Pool health · ')+healthy+'/'+accounts.length+uiText(' 账号可用',' accounts available')+'</span><i><span style="width:'+(accounts.length?Math.round(healthy/accounts.length*100):0)+'%"></span></i></div></div>';$('account-pool').innerHTML=accounts.length?overview+accounts.map(account=>{const quota=account.quota,error=text(quota?.last_error),status=!account.enabled?uiText('已停用','Disabled'):(!quota?uiText('等待同步','Awaiting sync'):(error?uiText('同步失败','Sync failed'):(quota.limit_reached||!quota.allowed?uiText('已耗尽','Exhausted'):uiText('可用','Available')))),auth=encodeURIComponent(account.auth_id||''),weekly=quota?.secondary,remaining=Math.max(0,100-Number(weekly?.used_percent||0)),window=officialWeeklyText(weekly),resetAt=weekly?.reset_at,accountName=account.name||uiText('Codex 账号','Codex account');return '<div class="pool-account"><div class="pool-account-head"><strong class="pool-account-name" title="'+escapeHTML(accountName)+'">'+escapeHTML(accountName)+' · '+num(account.capacity_x)+'x</strong><span class="pool-account-status">'+status+'</span></div><div class="pool-account-meta"><span>'+(window||uiText('等待官方周额度数据','Waiting for official weekly quota data'))+'</span><span>'+num(account.capacity_x)+'x '+uiText('容量','capacity')+'</span></div><div class="pool-meter"><span style="width:'+(weekly?.limit_window_seconds>0?remaining:0)+'%"></span></div><div class="pool-account-reset"><span>'+(resetAt?uiText('周恢复：','Weekly reset: ')+reset(resetAt):uiText('尚未获得官方恢复时间','Official reset time is not available yet'))+'</span><button type="button" class="account-remove" data-auth="'+auth+'">'+uiText('移除','Remove')+'</button></div>'+(error?'<div class="pool-error">'+uiText('同步失败：','Sync failed: ')+escapeHTML(error)+'</div>':'')+'</div>'}).join(''):'<div class="empty">'+uiText('尚未配置共享账号池。','No shared account pool configured.')+'</div>';document.querySelectorAll('.account-remove').forEach(button=>{button.onclick=()=>removeAccount(decodeURIComponent(button.dataset.auth||''))})}
// The approved product view promotes account-pool capacity, availability and
// health to equal visual metrics while preserving the existing quota data.
renderAccounts=function(){
	const accounts=state.summary.accounts||[],reset=value=>value?formatTime(value):'—',poolCapacity=Number(state.summary.status?.pool_capacity_x)||accounts.reduce((sum,account)=>sum+(Number(account.capacity_x)||0),0),allocated=Number(state.summary.status?.allocated_x)||0,available=Math.max(0,poolCapacity-allocated),healthy=accounts.filter(account=>account.enabled&&account.quota?.allowed&&!account.quota?.limit_reached&&!text(account.quota?.last_error)).length,healthPercent=accounts.length?Math.round(healthy/accounts.length*100):0;
	const overview='<div class="pool-overview">'
		+'<div class="pool-metric"><span class="pool-metric-icon"><svg class="cc-icon" aria-hidden="true"><use href="#cc-icon-gauge"></use></svg></span><div><span class="muted">'+uiText('合计可用额度','Total capacity')+'</span><strong>'+num(poolCapacity)+'x</strong></div></div>'
		+'<div class="pool-metric"><span class="pool-metric-icon"><svg class="cc-icon" aria-hidden="true"><use href="#cc-icon-bolt"></use></svg></span><div><span class="muted">'+uiText('可用额度','Available capacity')+'</span><strong>'+num(available)+'x</strong></div></div>'
		+'<div class="pool-metric pool-health"><span class="pool-metric-icon"><svg class="cc-icon" aria-hidden="true"><use href="#cc-icon-shield"></use></svg></span><div><span class="muted">'+uiText('池健康度','Pool health')+'</span><strong>'+healthy+' / '+accounts.length+'</strong></div><i><span style="width:'+healthPercent+'%"></span></i></div>'
		+'</div>';
	$('account-pool').innerHTML=accounts.length?overview+'<div class="pool-account-list">'+accounts.map(account=>{
		const quota=account.quota,error=text(quota?.last_error),status=!account.enabled?uiText('已停用','Disabled'):(!quota?uiText('等待同步','Awaiting sync'):(error?uiText('同步失败','Sync failed'):(quota.limit_reached||!quota.allowed?uiText('已耗尽','Exhausted'):uiText('可用','Available')))),statusTone=!account.enabled?'is-muted':(!quota?'is-pending':(error?'is-error':(quota.limit_reached||!quota.allowed?'is-exhausted':'is-available'))),auth=encodeURIComponent(account.auth_id||''),weekly=quota?.secondary,remaining=Math.max(0,100-Number(weekly?.used_percent||0)),window=officialWeeklyText(weekly),resetAt=weekly?.reset_at,accountName=account.name||uiText('Codex 账号','Codex account');
		return '<div class="pool-account">'
			+'<div class="pool-account-head"><div class="pool-account-identity"><span class="pool-account-icon"><svg class="cc-icon" aria-hidden="true"><use href="#cc-icon-bolt"></use></svg></span><strong class="pool-account-name" title="'+escapeHTML(accountName)+'">'+escapeHTML(accountName)+' · '+num(account.capacity_x)+'x</strong></div><span class="pool-account-status '+statusTone+'">'+status+'</span></div>'
			+'<div class="pool-account-meta"><span>'+(window||uiText('等待官方周额度数据','Waiting for official weekly quota data'))+'</span><span>'+num(account.capacity_x)+'x '+uiText('容量','capacity')+'</span></div>'
			+'<div class="pool-meter"><span style="width:'+(weekly?.limit_window_seconds>0?remaining:0)+'%"></span></div>'
			+'<div class="pool-account-reset"><span>'+(resetAt?uiText('周恢复：','Weekly reset: ')+reset(resetAt):uiText('尚未获得官方恢复时间','Official reset time is not available yet'))+'</span><button type="button" class="account-remove" data-auth="'+auth+'">'+uiText('移除','Remove')+'</button></div>'
			+(error?'<div class="pool-error">'+uiText('同步失败：','Sync failed: ')+escapeHTML(error)+'</div>':'')
			+'</div>'
	}).join('')+'</div>':'<div class="empty">'+uiText('尚未配置共享账号池。','No shared account pool configured.')+'</div>';
	document.querySelectorAll('.account-remove').forEach(button=>{button.onclick=()=>removeAccount(decodeURIComponent(button.dataset.auth||''))})
};
function completedTokenText(window){return num(allocationX(window,'confirmed'))+'x'}function provisionalTokenText(window){return num(allocationX(window,'provisional'))+'x'}function confirmedAllocationText(window){return completedTokenText(window)+' / '+num(allocationX(window,'capacity'))+'x'}function allocationText(window){return num(allocationX(window,'used'))+'x / '+num(allocationX(window,'capacity'))+'x'}
function renderKeys(){
	const allKeys=state.summary.keys||[],filter=text($('search').value).toLowerCase(),keys=allKeys.filter(key=>(key.name+' '+text(key.key_suffix)).toLowerCase().includes(filter));
	$('key-usage-heading').textContent=uiText('官方确认','Official confirmed');
	$('key-cycle-heading').textContent=uiText('周期 Token','Cycle Tokens');
	$('key-total-heading').textContent=uiText('累计 Token','Total Tokens');
	if(!allKeys.some(key=>key.id===state.selected))selectKey(allKeys[0]?.id||'');
	$('keys').innerHTML=keys.length?keys.map(key=>{
		const id=encodeURIComponent(key.id),allowed=key.allowed_models||[],allModels=allowed.length?allowed.join(', '):uiText('允许全部','Allow all'),previewModels=allowed.length?allowed.slice(0,3).join(', ')+(allowed.length>3?uiText('，另有 ',' + ')+(allowed.length-3)+uiText(' 个',' more'):''):allModels,badge=key.needs_rebind?uiText('需重新绑定','Rebind required'):(key.enabled?uiText('管理中','Managed'):uiText('不限制','Unrestricted')),editText=uiText('编辑','Edit'),resetText=uiText('重置','Reset'),deleteText=uiText('删除','Delete'),actual=key.actual_tokens||{},actualAvailable=Boolean(actual.available),cycleTokens=actualAvailable&&actual.cycle_known?tokens(actual.cycle):'—',totalTokens=actualAvailable?tokens(actual.total):'—',cycleTitle=uiText('当前官方周期实际 Token：','Actual Tokens in current official cycle: ')+cycleTokens,totalTitle=uiText('累计实际 Token：','Cumulative actual Tokens: ')+totalTokens;
		return '<tr role="button" tabindex="0" aria-label="'+uiText('查看 ','View ')+escapeHTML(key.name)+uiText(' 详情',' details')+'" class="'+(key.id===state.selected?'selected':'')+'" data-id="'+id+'"><td><div class="key-name">'+escapeHTML(key.name)+'</div><div class="suffix" title="'+escapeHTML(suffixTitle(key))+'">'+escapeHTML(uiText('CPA ','CPA ')+suffix(key))+'</div></td><td><strong>'+num(key.allocation_x)+'x</strong></td><td class="key-usage-cell"><strong>'+confirmedAllocationText(key.allocation)+'</strong></td><td class="key-token-cell" title="'+escapeHTML(cycleTitle)+'"><strong>'+cycleTokens+'</strong></td><td class="key-token-cell" title="'+escapeHTML(totalTitle)+'"><strong>'+totalTokens+'</strong></td><td class="model-cell" title="'+escapeHTML(allModels)+'"><span class="model-truncate">'+escapeHTML(previewModels)+'</span></td><td><span class="pill '+(key.enabled&&!key.needs_rebind?'':'off')+'">'+badge+'</span></td><td class="key-row-actions"><button type="button" class="row-action" data-key-edit="'+id+'">'+editText+'</button><button type="button" class="row-action" data-key-reset="'+id+'">'+resetText+'</button><button type="button" class="row-action danger" data-key-delete="'+id+'">'+deleteText+'</button></td></tr>'
	}).join(''):'<tr><td colspan="6" class="empty">'+uiText('暂无受控 Key。请先配置账号池并同步 CPA Key。','No managed Keys yet. Configure the account pool and synchronize CPA Keys first.')+'</td></tr>';
	document.querySelectorAll('tr[data-id]').forEach(row=>{
		const choose=()=>{const selected=decodeURIComponent(row.dataset.id);if(selected===state.selected)return;selectKey(selected);state.decisionPage.page=1;renderKeys();withModuleLoading(loadingTargets.key,async()=>{await Promise.all([loadTrend(),loadDecisionLogs(true)]);if(state.selected===selected)render()},uiText('正在切换 Key…','Switching Key…')).catch(error=>say(error.message))};
		row.onclick=choose;
		row.onkeydown=event=>{if(event.target!==row)return;if(event.key==='Enter'||event.key===' '){event.preventDefault();choose()}}
	});
	document.querySelectorAll('[data-key-edit]').forEach(button=>{button.onclick=event=>{event.stopPropagation();const key=keyFor(decodeURIComponent(button.dataset.keyEdit||''));if(key)openPolicy(policyFor(key.id)).catch(error=>say(error.message))}});
	document.querySelectorAll('[data-key-reset]').forEach(button=>{button.onclick=event=>{event.stopPropagation();const key=keyFor(decodeURIComponent(button.dataset.keyReset||''));if(key)resetPolicyUsage(key)}});
	document.querySelectorAll('[data-key-delete]').forEach(button=>{button.onclick=event=>{event.stopPropagation();const key=keyFor(decodeURIComponent(button.dataset.keyDelete||''));if(key)removePolicy(key)}})
}

function renderDetail(){
	if(typeof window.__ccRenderAnalysis==='function'){window.__ccRenderAnalysis();return}
	const key=keyFor(state.selected);
	if(!key){$('detail').innerHTML='<div class="legacy-key-detail"><h2>'+uiText('当前 Key 详情','Current Key details')+'</h2><div class="empty">'+uiText('选择一个受控 Key 后查看分配、模型与日志。','Select a managed Key to view its allocation, models, and logs.')+'</div></div>';return}
	const allocation=key.allocation||{},logs=state.logs||[],settled=logs.filter(log=>log.decision==='completed'),visibleDecisions=logs.filter(log=>log.decision!=='ignored'&&log.decision!=='expired'),successRate=visibleDecisions.length?Math.round(settled.length/visibleDecisions.length*100):0,models=key.allowed_models?.length?key.allowed_models.join(', '):uiText('默认允许全部模型','All models allowed by default'),remaining=allocationX(allocation,'remaining');
	$('detail').innerHTML='<div class="legacy-key-detail"><div class="section-title"><h2>当前 Key 详情</h2><div class="actions"><button type="button" id="edit">编辑策略</button><button type="button" id="unmanage" class="danger">解除管理</button></div></div><strong>'+escapeHTML(key.name)+'</strong><div class="remark">'+escapeHTML(suffix(key))+' · Key 分配是一份全局 x 余额；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，下一次官方周百分比更新后自动校正；增加 x 即时生效，降低 x 在当前官方周账期结束后生效'+(key.needs_rebind?' · 旧策略需重新绑定 CPA Key 后才可启用':'')+'</div><div class="window-grid"><div class="window"><span class="muted">共享池分配</span><strong>'+num(key.allocation_x)+'x</strong><div class="muted" style="margin-top:8px">创建时已校验所有 Key 总分配不超过账号池容量</div></div><div class="window"><span class="muted">当前计量 x 用量</span><strong>'+allocationText(allocation)+'</strong>'+meter(allocation,true)+'<div class="muted" style="margin-top:8px">官方确认 '+completedTokenText(allocation)+' · 待确认估算 '+provisionalTokenText(allocation)+' · 剩余 '+num(remaining)+'x'+(allocation.reset_at?' · 最早释放 '+formatTime(allocation.reset_at):'')+'</div></div></div><div class="key-analytics"><div><span class="muted">当前页决策</span><strong>'+num(visibleDecisions.length)+'</strong></div><div><span class="muted">完成结算</span><strong>'+num(settled.length)+'</strong></div><div><span class="muted">当前页成功率</span><strong>'+num(successRate)+'%</strong></div></div><div class="policy-line schedule-summary"><div><strong>访问时段</strong><div class="muted">'+escapeHTML(accessSummary(policyFor(key.id)))+'</div></div></div><div class="policy-line"><div><strong>模型策略</strong><div class="muted model-detail" title="'+escapeHTML(models)+'">'+escapeHTML(models)+'</div></div><span class="pill '+(key.enabled&&!key.needs_rebind?'':'off')+'">'+(key.needs_rebind?'需重新绑定':(key.enabled?'管理中':'不限制'))+'</span></div></div>';
	$('edit').onclick=()=>openPolicy(policyFor(key.id)).catch(error=>say(error.message));$('unmanage').onclick=()=>removePolicy(key)
}
function formatLogTime(value){const date=new Date(value),pad=part=>String(part).padStart(2,'0');return date.getFullYear()+'-'+pad(date.getMonth()+1)+'-'+pad(date.getDate())+' '+pad(date.getHours())+':'+pad(date.getMinutes())+':'+pad(date.getSeconds())}
function renderLogs(){
	const logs=state.logs||[],page=state.decisionPage||{},total=Math.max(0,Number(page.total)||0),current=Math.max(1,Number(page.page)||1),totalPages=Math.max(0,Number(page.total_pages)||0),displayPages=Math.max(1,totalPages);
	$('logs').innerHTML=logs.length?logs.map((log,index)=>{
		const completed=log.decision==='completed',blocked=log.decision==='blocked',failed=log.decision==='failed',ignored=log.decision==='ignored',expired=log.decision==='expired',status=Number(log.status_code||0),tokenText=Number(log.units)>0?tokens(log.units):'—',decision=blocked?uiText('拦截','Blocked'):(failed?uiText('失败','Failed'):(completed?uiText('完成 ','Completed ')+tokenText+' Token':(ignored?uiText('已忽略','Ignored'):(expired?uiText('已释放','Released'):uiText('已记录','Recorded'))))),key=keyFor(log.key_id)||keyFor(state.selected),keyName=key?.name||'—',suffixSource=text(log.key_suffix)?log:key,fingerprint=suffixSource?suffix(suffixSource):uiText('尾号未知','Suffix unavailable'),account=log.auth_id||'—',requestContent=log.request_content||'—',description=completed?uiText('实际 Token 结算','Actual Token settlement'):(failed?uiText('上游请求未完成','Upstream request did not complete'):(log.reason||uiText('策略记录','Policy record'))),requestedAt=formatLogTime(log.requested_at),viewText=uiText('查看','View');
		return '<tr><td class="log-time" title="'+escapeHTML(requestedAt)+'">'+escapeHTML(requestedAt)+'</td><td class="log-key-name" title="'+escapeHTML(keyName)+'">'+escapeHTML(keyName)+'</td><td class="log-key-fingerprint" title="'+escapeHTML(suffixSource?suffixTitle(suffixSource):uiText('CPA Key 尾号不可用','CPA Key suffix unavailable'))+'">'+escapeHTML(fingerprint)+'</td><td class="log-model" title="'+escapeHTML(log.model||'')+'">'+escapeHTML(log.model||'—')+'</td><td class="log-request-content" title="'+escapeHTML(requestContent)+'">'+escapeHTML(requestContent)+'</td><td class="log-decision" title="'+escapeHTML(decision)+'"><span class="decision '+(blocked||failed?'reject':(completed?'allow':'throttle'))+'" title="'+escapeHTML(decision)+'">'+escapeHTML(decision)+'</span></td><td class="log-token" title="'+escapeHTML(tokenText)+'">'+escapeHTML(tokenText)+'</td><td class="log-http '+(status>=400?'bad':(status>0?'ok':''))+'" title="'+escapeHTML(status||'—')+'">'+escapeHTML(status||'—')+'</td><td class="log-account" title="'+escapeHTML(account)+'">'+escapeHTML(account)+'</td><td class="log-description" title="'+escapeHTML(description)+'">'+escapeHTML(description)+'</td><td class="log-action" title="'+escapeHTML(viewText)+'"><button type="button" class="row-action log-detail-open" data-log-index="'+index+'">'+viewText+'</button></td></tr>'
	}).join(''):'<tr><td colspan="11" class="empty">'+uiText('暂无该 Key 的使用或策略日志。','No usage or policy logs for this Key yet.')+'</td></tr>';
	$('decision-page-info').textContent=uiText('共 ','Total ')+num(total)+uiText(' 条 · 每页 ',' · Per page ')+num(page.page_size||10)+uiText(' 条','');
	$('decision-page-number').textContent=uiText('第 ','Page ')+num(current)+' / '+num(displayPages)+uiText(' 页','');
	$('decision-prev').disabled=current<=1||total===0;
	$('decision-next').disabled=total===0||current>=totalPages;
	renderTrend()
}
function renderOperationalLogs(){
	const logs=state.operationLogs||[],page=state.operationPage||{},total=Math.max(0,Number(page.total)||0),current=Math.max(1,Number(page.page)||1),totalPages=Math.max(0,Number(page.total_pages)||0),displayPages=Math.max(1,totalPages);
	$('operation-logs').innerHTML=logs.length?logs.map(log=>{
		const level=text(log.level)||'info',levelText=level.toUpperCase(),account=log.auth_id||'—',keyText=log.key_id?' · Key '+String(log.key_id).slice(-8):'',occurredAt=formatTime(log.occurred_at),eventText=log.event||'—',messageText=(log.message||'—')+keyText;
		return '<tr><td title="'+escapeHTML(occurredAt)+'">'+escapeHTML(occurredAt)+'</td><td title="'+escapeHTML(levelText)+'"><span class="level '+escapeHTML(level)+'">'+escapeHTML(levelText)+'</span></td><td title="'+escapeHTML(eventText)+'">'+escapeHTML(eventText)+'</td><td class="log-account" title="'+escapeHTML(account)+'">'+escapeHTML(account)+'</td><td title="'+escapeHTML(messageText)+'">'+escapeHTML(messageText)+'</td></tr>'
	}).join(''):'<tr><td colspan="5" class="empty">'+uiText('暂无插件运行或错误日志。','No plugin runtime or error logs yet.')+'</td></tr>';
	$('operation-page-info').textContent=uiText('共 ','Total ')+num(total)+uiText(' 条 · 每页 ',' · Per page ')+num(page.page_size||10)+uiText(' 条','');
	$('operation-page-number').textContent=uiText('第 ','Page ')+num(current)+' / '+num(displayPages)+uiText(' 页','');
	$('operation-prev').disabled=current<=1||total===0;
	$('operation-next').disabled=total===0||current>=totalPages
}
// Canvas keeps the usage trend responsive without adding a charting dependency
// or retaining hundreds of DOM nodes for long date ranges.
function renderUsageLineChart(points){
	const host=$('chart'),labels=$('trend-labels');
	if(!host||!labels)return;
	host.className='bars line-chart';
	host.replaceChildren();
	labels.replaceChildren();
	labels.hidden=true;
	if(!points.length){host.innerHTML='<div class="empty">当前日期区间暂无实际完成结算。</div>';return}
	const canvas=document.createElement('canvas'),tooltip=document.createElement('div'),width=Math.max(320,Math.floor(host.getBoundingClientRect().width||480)),height=88,dpr=Math.min(2,Math.max(1,window.devicePixelRatio||1)),context=canvas.getContext('2d');
	canvas.className='usage-line-canvas';
	canvas.setAttribute('role','img');
	canvas.setAttribute('aria-label',uiText('实际 Token 用量折线图','Actual Token usage line chart'));
	canvas.width=Math.round(width*dpr);
	canvas.height=Math.round(height*dpr);
	canvas.style.width=width+'px';
	canvas.style.height=height+'px';
	tooltip.className='line-chart-tooltip';
	tooltip.hidden=true;
	host.append(canvas,tooltip);
	if(!context)return;
	context.scale(dpr,dpr);
	const pad={left:45,right:14,top:12,bottom:27},plotWidth=Math.max(1,width-pad.left-pad.right),plotHeight=Math.max(1,height-pad.top-pad.bottom),maximum=Math.max(1,...points.map(point=>Number(point.units)||0)),xFor=index=>pad.left+(points.length===1?plotWidth/2:index/(points.length-1)*plotWidth),yFor=value=>pad.top+plotHeight-(Number(value)||0)/maximum*plotHeight,coordinates=points.map((point,index)=>({x:xFor(index),y:yFor(point.units),point}));
	context.font='10px Inter, "PingFang SC", sans-serif';
	context.textBaseline='middle';
	for(let row=0;row<=3;row++){
		const y=pad.top+plotHeight*row/3,value=maximum*(1-row/3);
		context.beginPath();context.strokeStyle='#e7eef7';context.lineWidth=1;context.moveTo(pad.left,y);context.lineTo(width-pad.right,y);context.stroke();
		context.fillStyle='#8798ad';context.textAlign='right';context.fillText(tokens(value),pad.left-7,y)
	}
	const area=context.createLinearGradient(0,pad.top,0,pad.top+plotHeight);area.addColorStop(0,'rgba(22,138,103,.28)');area.addColorStop(1,'rgba(22,138,103,.025)');
	context.beginPath();coordinates.forEach((item,index)=>index?context.lineTo(item.x,item.y):context.moveTo(item.x,item.y));context.lineTo(coordinates[coordinates.length-1].x,pad.top+plotHeight);context.lineTo(coordinates[0].x,pad.top+plotHeight);context.closePath();context.fillStyle=area;context.fill();
	context.beginPath();coordinates.forEach((item,index)=>index?context.lineTo(item.x,item.y):context.moveTo(item.x,item.y));context.strokeStyle='#168a67';context.lineWidth=2.2;context.lineJoin='round';context.lineCap='round';context.stroke();
	coordinates.forEach(item=>{context.beginPath();context.arc(item.x,item.y,3,0,Math.PI*2);context.fillStyle='#fff';context.fill();context.strokeStyle='#168a67';context.lineWidth=2;context.stroke()});
	const labelCount=Math.min(6,points.length),usedIndexes=new Set;
	for(let slot=0;slot<labelCount;slot++){
		const index=labelCount===1?0:Math.round(slot*(points.length-1)/(labelCount-1));
		if(usedIndexes.has(index))continue;
		usedIndexes.add(index);
		const raw=String(points[index].label||''),label=raw.replace(/^\d{4}[-/]/,'');
		context.fillStyle='#8798ad';context.textAlign=index===0?'left':(index===points.length-1?'right':'center');context.fillText(label,xFor(index),height-10)
	}
	const showTooltip=event=>{
		const rect=canvas.getBoundingClientRect(),mouseX=(event.clientX-rect.left)*(width/Math.max(1,rect.width));
		let nearest=coordinates[0];for(const item of coordinates)if(Math.abs(item.x-mouseX)<Math.abs(nearest.x-mouseX))nearest=item;
		tooltip.textContent=String(nearest.point.label||'')+' · '+tokens(nearest.point.units)+' Token · '+num(nearest.point.request_count||0)+uiText(' 次请求',' requests');
		tooltip.hidden=false;
		const tipLeft=Math.min(width-80,Math.max(80,nearest.x));tooltip.style.left=tipLeft+'px';tooltip.style.top=Math.max(32,nearest.y-8)+'px'
	};
	canvas.onmousemove=showTooltip;
	canvas.onmouseleave=()=>{tooltip.hidden=true}
}
window.__ccRenderUsageLineChart=renderUsageLineChart;
let lineChartResizeTimer=0;
window.addEventListener('resize',()=>{clearTimeout(lineChartResizeTimer);lineChartResizeTimer=setTimeout(()=>renderUsageLineChart(state.trend?.points||[]),120)},{passive:true});
function renderTrend(){const trend=state.trend||{points:[]},points=trend.points||[];renderUsageLineChart(points);const coverage=trend.available_from?uiText(' · 数据起于 ',' · Data from ')+new Date(trend.available_from).toLocaleDateString(state.locale==='en'?'en-US':'zh-CN'):uiText(' · 暂无已结算历史',' · No settled history'),mode=trend.granularity==='year'?uiText('按年','yearly'):trend.granularity==='month'?uiText('按月','monthly'):trend.granularity==='hour'?uiText('按小时','hourly'):uiText('按日','daily');$('trend-from').textContent=(trend.timezone||'Asia/Shanghai')+' · '+mode+' · '+num(points.length)+uiText(' 个区间 · 保留 ',' periods · Retained ')+num(trend.retention_days||366)+uiText(' 天',' days')+coverage;$('trend-total').textContent=uiText('实际 ','Actual ')+tokens(trend.total_tokens||0)+' Token · '+num(trend.request_count||0)+uiText(' 次请求',' requests')}
// Refresh the approved analysis layout after every core data render, including async API updates.
function render(){renderStats();renderKeys();renderDetail();renderLogs();renderOperationalLogs();window.__ccPanelBridge?.afterRender?.()}
// Presets choose a useful matching resolution: today is hourly, short ranges
// are daily, and a full year is monthly.
function localDateValue(value){const pad=part=>String(part).padStart(2,'0');return value.getFullYear()+'-'+pad(value.getMonth()+1)+'-'+pad(value.getDate())}function applyAnalysisPreset(preset){const today=new Date(),from=new Date(today),granularity=$('analysis-granularity');if(preset==='seven'){from.setDate(today.getDate()-6);granularity.value='day'}else if(preset==='month'){from.setDate(1);granularity.value='day'}else if(preset==='year'){from.setMonth(0,1);granularity.value='month'}else if(preset==='today'){granularity.value='hour'}else return;$('analysis-from').value=localDateValue(from);$('analysis-to').value=localDateValue(today)}function analysisTimezone(){try{return Intl.DateTimeFormat().resolvedOptions().timeZone||'Asia/Shanghai'}catch{return 'Asia/Shanghai'}}async function loadTrend(){const requestID=++trendRequestID,selected=state.selected;if(!selected){state.trend={points:[]};return true}if(!$('analysis-from').value||!$('analysis-to').value)applyAnalysisPreset('today');const params=new URLSearchParams({key_id:selected,from:$('analysis-from').value,to:$('analysis-to').value,timezone:analysisTimezone(),granularity:$('analysis-granularity').value||'hour'}),payload=await api('/analysis?'+params);if(requestID!==trendRequestID||selected!==state.selected)return false;state.trend=payload;return true}
async function loadDecisionLogs(resetPage=false){const requestID=++decisionRequestID;if(resetPage)state.decisionPage.page=1;const selected=state.selected;if(!selected){state.logs=[];state.decisionPage={page:1,page_size:Math.max(1,Number($('decision-page-size')?.value)||10),total:0,total_pages:0};return true}const decision=text($('decision-filter')?.value),query=text($('decision-search')?.value),pageSize=Math.max(1,Number($('decision-page-size')?.value)||10),params=new URLSearchParams({key_id:selected,page:String(Math.max(1,Number(state.decisionPage.page)||1)),page_size:String(pageSize)});if(decision)params.set('decision',decision);if(query)params.set('query',query);const payload=await api('/logs?'+params);if(requestID!==decisionRequestID||selected!==state.selected)return false;state.logs=payload.logs||[];state.decisionPage={page:Number(payload.page)||1,page_size:Number(payload.page_size)||pageSize,total:Number(payload.total)||0,total_pages:Number(payload.total_pages)||0};return true}
async function loadOperationalLogs(resetPage=false){const requestID=++operationRequestID;if(resetPage)state.operationPage.page=1;const level=text($('operation-level')?.value),query=text($('operation-search')?.value),pageSize=Math.max(1,Number($('operation-page-size')?.value)||10),params=new URLSearchParams({page:String(Math.max(1,Number(state.operationPage.page)||1)),page_size:String(pageSize)});if(level)params.set('level',level);if(query)params.set('query',query);const payload=await api('/operation-logs?'+params);if(requestID!==operationRequestID)return false;state.operationLogs=payload.logs||[];state.operationPage={page:Number(payload.page)||1,page_size:Number(payload.page_size)||pageSize,total:Number(payload.total)||0,total_pages:Number(payload.total_pages)||0};return true}
async function refresh({loading=true}={}){const task=async()=>{try{say('');const [summary,policies,models]=await Promise.all([api('/summary'),api('/keys'),api('/models'),loadOperationalLogs()]);state.summary=summary;state.policies=policies.keys||[];state.models=models.models||[];if(!state.summary.keys.some(key=>key.id===state.selected)){selectKey(state.summary.keys[0]?.id||'');state.decisionPage.page=1}await Promise.all([loadTrend(),loadDecisionLogs()]);render();return true}catch(error){say(error.message);return false}};return loading?withModuleLoading(loadingTargets.full,task,uiText('正在刷新面板…','Refreshing dashboard…')):task()}
const modelCatalogFreshForMs=30*60*1000,modelSyncConcurrency=2;let modelSyncInFlight=null;function modelCatalogFresh(){const syncedAt=Date.parse(state.models[0]?.synced_at||'');return state.models.length>0&&Number.isFinite(syncedAt)&&Date.now()-syncedAt<modelCatalogFreshForMs}async function mapWithConcurrency(items,limit,worker){const results=new Array(items.length);let next=0;const workers=Array.from({length:Math.min(Math.max(1,limit),items.length)},async()=>{for(;;){const index=next++;if(index>=items.length)return;try{results[index]={ok:true,value:await worker(items[index])}}catch(error){results[index]={ok:false,error}}}});await Promise.all(workers);return results}async function fetchCPAKeys(){const payload=await host('/api-keys');return (payload['api-keys']||payload.api_keys||[]).filter(key=>typeof key==='string'&&text(key))}async function loadCPAKeys(){state.rawKeys=await fetchCPAKeys();return state.rawKeys.length}async function syncCPAModels({announce=true,force=false}={}){if(!force&&modelCatalogFresh())return state.models.length;if(modelSyncInFlight)return modelSyncInFlight;modelSyncInFlight=(async()=>{const authPayload=await host('/auth-files');const auths=(authPayload.files||[]).filter(file=>text(file.provider||file.type).toLowerCase()==='codex'&&!file.disabled&&!file.unavailable);let staticModels=[];try{const payload=await host('/model-definitions/codex');staticModels=payload.models||[]}catch{}let perAuth=[],failed=0;if(auths.length){const results=await mapWithConcurrency(auths,modelSyncConcurrency,auth=>host('/auth-files/models?name='+encodeURIComponent(auth.id||auth.name)));perAuth=results.filter(result=>result.ok).map(result=>result.value);failed=results.length-perAuth.length}const seen=new Map;[...staticModels,...perAuth.flatMap(payload=>payload.models||[])].forEach(model=>{const id=text(model.id||model.name);if(id&&!seen.has(id))seen.set(id,{id,display_name:text(model.display_name||model.displayName||id),owner:text(model.owned_by||model.owner||'openai')})});if(!seen.size)throw new Error(uiText('CPA 未返回可用 Codex 模型，已保留现有目录。','CPA did not return usable Codex models; the current catalog was retained.'));const syncedAt=new Date().toISOString(),models=Array.from(seen.values());await api('/models',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({models})});state.models=models.map(model=>({...model,available:true,synced_at:syncedAt}));render();if(failed&&announce)say(uiText('部分账号的模型目录未读取，已保留可用模型。','Some account model catalogs could not be read; available models were retained.'));if(announce)say(uiText('已同步 ','Synchronized ')+models.length+uiText(' 个 Codex 模型。',' Codex models.'),true);return models.length})();try{const count=await modelSyncInFlight;state.modelSyncError='';renderSyncStatus();return count}catch(error){state.modelSyncError=uiText('模型目录同步失败，已保留上次成功目录','Model catalog synchronization failed; the last successful catalog was retained');renderSyncStatus();throw error}finally{modelSyncInFlight=null}}async function syncCPA(){const button=$('sync');button.disabled=true;try{say(uiText('正在从 CPA 同步 Key 与 Codex 模型…','Synchronizing CPA Keys and Codex models…'));const [keys,models]=await Promise.all([fetchCPAKeys(),syncCPAModels({announce:false,force:true})]);state.rawKeys=[];say(uiText('已同步 ','Synchronized ')+keys.length+uiText(' 个 CPA Key 与 ',' CPA Keys and ')+models+uiText(' 个 Codex 模型。',' Codex models.'),true)}catch(error){say(error.message)}finally{button.disabled=false}}function newID(){return 'key-'+(crypto.randomUUID?.()||Date.now().toString(36))}function mask(raw){return raw?'••••'+String(raw).slice(-4):uiText('已隐藏','Hidden')}function fillSourceOptions(){const select=$('source');select.replaceChildren();if(!state.rawKeys.length){const option=document.createElement('option');option.value='';option.textContent=uiText('请先同步 CPA Key','Synchronize CPA Keys first');select.append(option);return}state.rawKeys.forEach((key,index)=>{const option=document.createElement('option');option.value=String(index);option.textContent='CPA Key '+(index+1)+' · '+mask(key);select.append(option)})}
function fillModels(allowed){const wrap=$('model-list');wrap.replaceChildren();if(!state.models.length){wrap.textContent=uiText('请先同步 CPA 模型目录。','Synchronize the CPA model catalog first.');wrap.className='models empty';return}wrap.className='models';state.models.forEach(model=>{const label=document.createElement('label'),input=document.createElement('input'),name=document.createElement('span'),id=document.createElement('small');label.className='model-row';input.type='checkbox';input.value=model.id;input.checked=allowed.has(model.id);name.textContent=model.display_name||model.id;id.className='muted';id.textContent=model.id;label.append(input,name,id);wrap.append(label)})}function setAccessLimited(value){const wrap=$('access-rule-wrap');wrap.hidden=!value;if(value&&!wrap.querySelector('[data-access-rule]'))renderAccessRules([])}function accessRuleCard(rule={}){const card=document.createElement('div'),selected=new Set(rule.weekdays||[1,2,3,4,5,6,7]),weekdays=uiText(['周一','周二','周三','周四','周五','周六','周日'],['Mon','Tue','Wed','Thu','Fri','Sat','Sun']);card.className='access-rule';card.dataset.accessRule='';card.innerHTML='<div class="access-rule-head"><strong>'+uiText('访问时段','Access time slot')+'</strong><button type="button" class="access-rule-remove">'+uiText('移除','Remove')+'</button></div><div class="form-grid"><label>'+uiText('开始时间','Start time')+'<input class="access-start" type="time"></label><label>'+uiText('结束时间','End time')+'<input class="access-end" type="time"></label></div><div class="weekday-picks access-weekdays" aria-label="'+uiText('允许访问的星期','Allowed weekdays')+'">'+weekdays.map((day,index)=>'<label><input type="checkbox" value="'+(index+1)+'">'+day+'</label>').join('')+'</div><p class="hint">'+uiText('开始时间晚于结束时间时，此时段跨午夜延续到次日。','If the start time is later than the end time, the slot continues past midnight into the next day.')+'</p>';card.querySelector('.access-start').value=rule.start||'08:00';card.querySelector('.access-end').value=rule.end||'09:00';card.querySelectorAll('.access-weekdays input').forEach(input=>{input.checked=selected.has(Number(input.value))});card.querySelector('.access-rule-remove').onclick=()=>{const rules=document.querySelectorAll('[data-access-rule]');if(rules.length<=1){$('access-limited').checked=false;setAccessLimited(false);return}card.remove()};return card}function addAccessRule(rule={}){const wrap=$('access-rule-wrap'),button=$('access-rule-add');if(wrap.querySelectorAll('[data-access-rule]').length>=16){say(uiText('一个 Key 最多配置 16 个访问时段。','A Key supports at most 16 access time slots.'));return}wrap.insertBefore(accessRuleCard(rule),button)}function renderAccessRules(rules){const wrap=$('access-rule-wrap'),timezone=$('access-timezone'),label=timezone?.closest('label');if(label&&label.closest('[data-access-rule]')){const row=document.createElement('div');row.className='access-timezone-row';row.append(label);wrap.insertBefore(row,wrap.firstChild)}wrap.querySelectorAll('[data-access-rule]').forEach(rule=>rule.remove());(rules&&rules.length?rules:[{}]).forEach(rule=>addAccessRule(rule))}function fillAccessRule(policy){const rules=policy?.access_rules||[],limited=rules.length>0;$('access-limited').checked=limited;$('access-timezone').value=policy?.access_timezone||'Asia/Shanghai';renderAccessRules(rules);setAccessLimited(limited)}function readAccessPolicy(){if(!$('access-limited').checked)return {access_rules:[],access_timezone:''};const rules=Array.from(document.querySelectorAll('[data-access-rule]')).map(card=>({weekdays:Array.from(card.querySelectorAll('.access-weekdays input:checked')).map(input=>Number(input.value)),start:text(card.querySelector('.access-start').value),end:text(card.querySelector('.access-end').value)}));const timezone=text($('access-timezone').value);if(!rules.length||rules.some(rule=>!rule.weekdays.length))throw new Error(uiText('每个访问时段都至少选择一个星期。','Select at least one weekday for every access time slot.'));if(rules.some(rule=>!rule.start||!rule.end||rule.start===rule.end))throw new Error(uiText('每个访问时段的开始和结束时间必须存在且不能相同。','Every access time slot needs different start and end times.'));if(!timezone)throw new Error(uiText('请填写访问时段的 IANA 时区，例如 Asia/Shanghai。','Enter an IANA time zone, for example Asia/Shanghai.'));return {access_rules:rules,access_timezone:timezone}}function accessSummary(policy){const labels=uiText(['','周一','周二','周三','周四','周五','周六','周日'],['','Mon','Tue','Wed','Thu','Fri','Sat','Sun']),rules=policy?.access_rules||[];if(!rules.length)return uiText('不限时访问','Unrestricted access');const textRules=rules.map(rule=>{const days=(rule.weekdays||[]).map(day=>labels[Number(day)]||'').filter(Boolean);return ((days.length===7?uiText('每天','Every day'):days.join(uiText('、',', ')))||uiText('指定星期','Selected weekdays'))+' '+(rule.start||'')+'–'+(rule.end||'')});return textRules.join(uiText('；','; '))+' · '+(policy.access_timezone||'Asia/Shanghai')}
function authFileBase(value){const normalized=text(value).replaceAll('\\','/').replace(/\/+$/,'');return normalized.slice(normalized.lastIndexOf('/')+1).toLowerCase()}
async function attachCPASchedulerAuthIndices(accounts){try{const payload=await host('/auth-files'),files=(payload.files||[]).filter(file=>text(file.provider||file.type).toLowerCase()==='codex'&&!file.disabled&&!file.unavailable);return accounts.map(account=>{const source=text(account.auth_id),base=authFileBase(source),matches=files.filter(file=>{const id=text(file.id),name=text(file.name);return id===source||name===source||(base!==''&&(authFileBase(id)===base||authFileBase(name)===base))});const schedulerID=text(matches.length===1?matches[0].id:'');return schedulerID?{...account,auth_index:schedulerID}:account})}catch{return accounts}}
// Saved operator values win; a newly discovered account starts from the x
// capacity inferred from CPA's current Codex plan metadata.
function renderAccountChoices(){const wrap=$('account-list'),configured=new Map((state.summary.accounts||[]).map(account=>[account.auth_id,account]));wrap.replaceChildren();state.rawAccounts.forEach((account,index)=>{const saved=configured.get(account.auth_id),row=document.createElement('div'),check=document.createElement('input'),name=document.createElement('div'),label=document.createElement('span'),identity=document.createElement('small'),capacityField=document.createElement('div'),capacity=document.createElement('input'),unit=document.createElement('span'),savedCapacity=Number(saved?.capacity_x),discoveredCapacity=Number(account.capacity_x),initialCapacity=Number.isFinite(savedCapacity)&&savedCapacity>0?savedCapacity:(Number.isFinite(discoveredCapacity)&&discoveredCapacity>0?discoveredCapacity:1);row.className='account-select-row';row.dataset.accountIndex=String(index);check.type='checkbox';check.checked=Boolean(saved?.enabled);check.setAttribute('aria-label',uiText('加入 ','Add ')+(account.name||account.auth_id));name.className='account-select-name';label.textContent=account.name||mask(account.auth_id);identity.textContent=mask(account.auth_id)+(saved?uiText(' · 已配置',' · Configured'):'');name.append(label,identity);capacityField.className='account-capacity-field';capacity.type='number';capacity.min='0.01';capacity.step='0.01';capacity.value=String(initialCapacity);capacity.setAttribute('aria-label',(account.name||account.auth_id)+uiText(' 容量',' capacity'));unit.className='account-capacity-unit';unit.textContent='x';unit.setAttribute('aria-hidden','true');capacityField.append(capacity,unit);row.append(check,name,capacityField);wrap.append(row)})}
async function openAccounts(){const payload=await api('/accounts/discover');state.rawAccounts=await attachCPASchedulerAuthIndices(payload.accounts||[]);if(!state.rawAccounts.length)throw new Error(uiText('CPA 中没有可用的 Codex 账号。','CPA has no usable Codex accounts.'));renderAccountChoices();$('account-dialog').showModal();say('')}
function closeAccounts(){state.rawAccounts=[];if($('account-dialog').open)$('account-dialog').close()}
async function openAuthSettings(){const payload=await api('/setup'),settings=payload.settings||{};$('auth-directory').value=text(settings.auth_directory)||'~/.cli-proxy-api';$('auth-directory-dialog').showModal();say('')}
function closeAuthSettings(){if($('auth-directory-dialog').open)$('auth-directory-dialog').close()}
async function saveAuthSettings(){const directory=text($('auth-directory').value);if(!directory){say(uiText('请填写 CPA 认证目录。','Enter the CPA authentication directory.'));return}try{const payload=await api('/setup'),settings=payload.settings||{};settings.auth_directory=directory;const saved=await api('/setup',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({settings})});closeAuthSettings();say(quotaSyncMessage(saved,uiText('认证目录已保存','Authentication directory saved')),saved.status==='ready');await refresh()}catch(error){say(error.message)}}
function quotaSyncMessage(payload,success){const quota=payload?.quota||{},ready=(quota.ready||[]).length,pending=(quota.pending||[]).length,errors=Object.values(quota.errors||{}).filter(Boolean),base=success||uiText('账号池已保存','Account pool saved');if(ready)return base+uiText('；已取得 ','; obtained ')+ready+uiText(' 个官方周额度快照。',' official weekly quota snapshot(s).');if(errors.length)return base+uiText('；官方额度同步失败，请查看插件运行与错误日志。','; official quota synchronization failed. See plugin runtime and error logs.');if(pending)return base+uiText('；官方额度仍在同步中，完成前受控 Key 会安全返回 503。','; official quota synchronization is still pending. Managed Keys safely return 503 until it completes.');return base}
async function saveAccount(){const rows=Array.from(document.querySelectorAll('#account-list [data-account-index]')),configured=new Map((state.summary.accounts||[]).map(account=>[account.auth_id,account])),accounts=[];for(const row of rows){if(!row.querySelector('input[type=checkbox]').checked)continue;const account=state.rawAccounts[Number(row.dataset.accountIndex)]||{},saved=configured.get(account.auth_id),capacity=Number(row.querySelector('input[type=number]').value);if(!account.auth_id||!Number.isFinite(capacity)||capacity<=0){say(uiText('每个已选账号都需要填写正数容量。','Every selected account needs a positive capacity.'));return}accounts.push({auth_id:account.auth_id,auth_index:text(account.auth_index)||text(saved?.auth_index),name:account.name,capacity_x:capacity,enabled:true})}if(!accounts.length){say(uiText('请至少选择一个 CPA Codex 账号。','Select at least one CPA Codex account.'));return}try{const payload=await api('/accounts/batch',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({accounts})});closeAccounts();say(quotaSyncMessage(payload,uiText('已原子保存 ','Saved ')+accounts.length+uiText(' 个账号',' account(s) atomically')),payload.status==='ready');await refresh()}catch(error){say(error.message)}}
async function removeAccount(authID){if(!authID||!confirm(uiText('移除该账号池配置？已分配 Key 超过剩余池容量，或仍有官方周账期未结束时，系统会拒绝移除。','Remove this account-pool configuration? The plugin will refuse removal when managed Key allocations exceed the remaining pool capacity or an official weekly window is still active.')))return;try{await api('/accounts?auth_id='+encodeURIComponent(authID),{method:'DELETE'});say(uiText('已移除账号池配置。','Account-pool configuration removed.'),true);await refresh()}catch(error){say(error.message)}}
async function openPolicy(policy){const rebind=Boolean(policy?.needs_rebind);if(!policy||rebind){say(uiText('正在从 CPA 读取可用 Key…','Reading available Keys from CPA…'));await loadCPAKeys()}if(!state.models.length)await syncCPAModels({announce:false});state.editing=policy||null;const dialog=$('policy-dialog');$('dialog-title').textContent=policy?(rebind?uiText('重新绑定 CPA Key','Rebind CPA Key'):uiText('编辑 Key 策略','Edit Key policy')):uiText('纳入 CPA Key 管理','Manage CPA Key');$('source-wrap').style.display=policy&&!rebind?'none':'grid';fillSourceOptions();$('name').value=policy?.name||'';$('allocation').value=policy?.allocation_x||1;$('enabled').value=String(policy?.enabled??true);fillModels(new Set(policy?.allowed_models||[]));fillAccessRule(policy);$('policy-hint').textContent=rebind?uiText('旧版 SHA 指纹不能安全转换，请选择 CPA 当前 Key 重新绑定后再启用。','Legacy SHA fingerprints cannot be converted safely. Select a current CPA Key, rebind it, then enable it.'):uiText('每个 Key 只有一份全局 x 余额；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，官方周百分比更新后自动校正。账号耗尽时自动切换。','Each Key has one global x balance. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile when the official weekly percentage updates. Routing switches automatically when an account is exhausted.');dialog.showModal();say('')}
function closePolicy(){state.rawKeys=[];state.editing=null;if($('policy-dialog').open)$('policy-dialog').close()}
async function savePolicy(){const existing=state.editing,name=text($('name').value),allocation=Number($('allocation').value),needsKey=!existing||existing.needs_rebind;if(!name||!Number.isFinite(allocation)||allocation<=0){say(uiText('请填写 Key 备注名称与正数共享池分配。','Enter a Key remark and a positive shared-pool allocation.'));return}let apiKey='';if(needsKey){apiKey=state.rawKeys[Number($('source').value)]||'';if(!apiKey){say(uiText('请先同步并选择 CPA Key。','Synchronize and select a CPA Key first.'));return}}const allowed=Array.from(document.querySelectorAll('#model-list input:checked')).map(input=>input.value);let access;try{access=readAccessPolicy()}catch(error){say(error.message);return}const policy={id:existing?.id||newID(),name,allocation_x:allocation,allowed_models:allowed,enabled:$('enabled').value==='true',...access};try{await api('/keys',{method:existing?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({policy,api_key:apiKey})});closePolicy();say(uiText('策略已保存。原始 Key 已从页面内存清除。','Policy saved. The original Key was cleared from page memory.'),true);await refresh()}catch(error){say(error.message)}}
async function removePolicy(key){if(!confirm(uiText('删除“'+key.name+'”的管理策略？该 Key 的插件用量、分析与日志会一并清空；重新添加后从 0 开始。','Delete the management policy for “'+key.name+'”? This Key’s plugin usage, analytics, and logs will be cleared; re-adding it starts from zero.')))return;try{await api('/keys?key_id='+encodeURIComponent(key.id),{method:'DELETE'});selectKey('');say(uiText('已删除管理策略和该 Key 的插件数据；重新添加将从 0 开始。','The management policy and this Key’s plugin data were deleted; re-adding it will start from zero.'),true);await refresh()}catch(error){say(error.message)}}
async function resetPolicyUsage(key){if(!confirm(uiText('重置“'+key.name+'”的额度与用量？该 Key 的插件额度账本和用量分析会从零开始；备注、分配、模型限制、访问时段以及全部日志都会保留。官方账号真实额度不会重置。','Reset quota and usage for “'+key.name+'”? This Key’s plugin quota ledger and usage analytics will restart from zero; its remark, allocation, model restrictions, access schedule, and all logs will be preserved. Official account quota will not be reset.')))return;try{await api('/keys/reset?key_id='+encodeURIComponent(key.id),{method:'POST'});say(uiText('该 Key 的额度与用量已重置，策略和日志保持不变。','This Key’s quota and usage were reset; its policy and logs remain unchanged.'),true);await refresh()}catch(error){say(error.message)}}
async function initialize(){await refresh();try{const models=await syncCPAModels({announce:false});if(!modelCatalogFresh())say(uiText('已自动同步 ','Automatically synchronized ')+models+uiText(' 个 Codex 模型。',' Codex models.'),true)}catch(error){say(uiText('模型目录自动同步失败，可点击“同步 CPA Key 与模型”重试。','Automatic model synchronization failed. Use “Sync CPA keys and models” to retry.'))}}let decisionSearchTimer=0,operationSearchTimer=0;const reloadDecisionLogs=(resetPage=false)=>loadDecisionLogs(resetPage).then(renderLogs).catch(error=>say(error.message)),reloadOperationalLogs=(resetPage=false)=>loadOperationalLogs(resetPage).then(renderOperationalLogs).catch(error=>say(error.message));$('refresh').onclick=refresh;$('sync').onclick=syncCPA;$('auth-settings').onclick=()=>openAuthSettings().catch(error=>say(error.message));$('accounts').onclick=()=>openAccounts().catch(error=>say(error.message));$('quota-refresh').onclick=async()=>{try{const payload=await api('/accounts/refresh',{method:'POST'});say(quotaSyncMessage(payload,uiText('官方额度刷新完成','Official quota refresh completed')),payload.status==='ready');await refresh()}catch(error){say(error.message)}};$('manage').onclick=()=>openPolicy(null).catch(error=>say(error.message));$('search').oninput=()=>{renderKeys();renderDetail();renderLogs()};$('analysis-preset').onchange=()=>{const preset=$('analysis-preset').value;if(preset!=='custom')applyAnalysisPreset(preset);loadTrend().then(renderTrend).catch(error=>say(error.message))};$('analysis-apply').onclick=()=>{if(!$('analysis-from').value||!$('analysis-to').value){say(uiText('请选择完整的统计日期区间。','Select a complete reporting date range.'));return}$('analysis-preset').value='custom';loadTrend().then(renderTrend).catch(error=>say(error.message))};$('analysis-from').onchange=$('analysis-to').onchange=()=>{$('analysis-preset').value='custom'};$('analysis-granularity').onchange=()=>loadTrend().then(renderTrend).catch(error=>say(error.message));$('access-limited').onchange=()=>setAccessLimited($('access-limited').checked);$('access-rule-add').onclick=()=>{if(!$('access-limited').checked){$('access-limited').checked=true;setAccessLimited(true)}addAccessRule({})};$('decision-refresh').onclick=()=>reloadDecisionLogs();$('decision-filter').onchange=()=>reloadDecisionLogs(true);$('decision-page-size').onchange=()=>reloadDecisionLogs(true);$('decision-search').oninput=()=>{clearTimeout(decisionSearchTimer);decisionSearchTimer=setTimeout(()=>reloadDecisionLogs(true),220)};$('decision-prev').onclick=()=>{if(state.decisionPage.page>1){state.decisionPage.page--;reloadDecisionLogs()}};$('decision-next').onclick=()=>{if(state.decisionPage.page<state.decisionPage.total_pages){state.decisionPage.page++;reloadDecisionLogs()}};$('operation-refresh').onclick=()=>reloadOperationalLogs();$('operation-level').onchange=()=>reloadOperationalLogs(true);$('operation-page-size').onchange=()=>reloadOperationalLogs(true);$('operation-search').oninput=()=>{clearTimeout(operationSearchTimer);operationSearchTimer=setTimeout(()=>reloadOperationalLogs(true),220)};$('operation-prev').onclick=()=>{if(state.operationPage.page>1){state.operationPage.page--;reloadOperationalLogs()}};$('operation-next').onclick=()=>{if(state.operationPage.page<state.operationPage.total_pages){state.operationPage.page++;reloadOperationalLogs()}};$('account-select-all').onclick=()=>document.querySelectorAll('#account-list input[type=checkbox]').forEach(input=>{input.checked=true});$('close').onclick=closePolicy;$('cancel').onclick=closePolicy;$('save').onclick=savePolicy;$('account-close').onclick=closeAccounts;$('account-cancel').onclick=closeAccounts;$('account-save').onclick=saveAccount;$('auth-directory-close').onclick=closeAuthSettings;$('auth-directory-cancel').onclick=closeAuthSettings;$('auth-directory-save').onclick=saveAuthSettings;$('policy-dialog').addEventListener('close',()=>{state.rawKeys=[];state.editing=null});$('account-dialog').addEventListener('close',()=>{state.rawAccounts=[]});setInterval(()=>{if(document.visibilityState==='visible'&&!$('policy-dialog').open&&!$('account-dialog').open&&!$('auth-directory-dialog').open)refresh({loading:false}).catch(()=>{})},5*60*1000);initialize();
// Rebind the network-backed controls to local loaders. Data is rendered only
// after the matching request finishes, so switching no longer flashes stale
// content from the previously selected Key or log page.
const reloadTrendWithLoading=()=>withModuleLoading(loadingTargets.analysis,async()=>{if(await loadTrend()){renderTrend();window.__ccPanelBridge?.afterRender?.()}},uiText('正在加载用量分析…','Loading usage analysis…')).catch(error=>say(error.message));
const reloadDecisionWithLoading=(resetPage=false)=>withModuleLoading(loadingTargets.decision,async()=>{if(await loadDecisionLogs(resetPage)){renderLogs();window.__ccPanelBridge?.afterRender?.()}},uiText('正在加载使用日志…','Loading usage logs…')).catch(error=>say(error.message));
const reloadOperationWithLoading=(resetPage=false)=>withModuleLoading(loadingTargets.operation,async()=>{if(await loadOperationalLogs(resetPage))renderOperationalLogs()},uiText('正在加载运行日志…','Loading runtime logs…')).catch(error=>say(error.message));
async function clearDecisionLogs(){
	const key=keyFor(state.selected);
	if(!key){say(uiText('请先选择一个受控 Key。','Select a managed Key first.'));return}
	if(!confirm(uiText('清除“'+key.name+'”的使用与策略日志？只会删除该 Key 的请求、决策和结算日志，额度、用量分析、策略及插件运行日志不会改变。此操作无法撤销。','Clear usage and policy logs for “'+key.name+'”? Only this Key’s request, decision, and settlement logs will be deleted. Quota, usage analytics, policy, and plugin runtime logs will not change. This action cannot be undone.')))return;
	try{
		await withButtonLoading($('decision-clear'),()=>withModuleLoading(loadingTargets.decision,async()=>{
			await api('/logs?key_id='+encodeURIComponent(key.id),{method:'DELETE'});
			state.decisionPage.page=1;
			await loadDecisionLogs(true);
			renderLogs();
			window.__ccPanelBridge?.afterRender?.();
		},uiText('正在清除使用日志…','Clearing usage logs…')));
		say(uiText('该 Key 的使用与策略日志已清除。','Usage and policy logs for this Key were cleared.'),true)
	}catch(error){say(error.message)}
}
async function clearOperationalLogs(){
	if(!confirm(uiText('清除全部插件运行与错误日志？Key 的额度、用量分析、策略以及使用与策略日志不会改变。此操作无法撤销。','Clear all plugin runtime and error logs? Key quota, usage analytics, policies, and usage/policy logs will not change. This action cannot be undone.')))return;
	try{
		await withButtonLoading($('operation-clear'),()=>withModuleLoading(loadingTargets.operation,async()=>{
			await api('/operation-logs',{method:'DELETE'});
			state.operationPage.page=1;
			await loadOperationalLogs(true);
			renderOperationalLogs();
		},uiText('正在清除运行日志…','Clearing runtime logs…')));
		say(uiText('插件运行与错误日志已清除。','Plugin runtime and error logs were cleared.'),true)
	}catch(error){say(error.message)}
}
const refreshHandler=$('refresh').onclick,syncHandler=$('sync').onclick,accountHandler=$('accounts').onclick,quotaRefreshHandler=$('quota-refresh').onclick,manageHandler=$('manage').onclick;
$('refresh').onclick=()=>withButtonLoading($('refresh'),async()=>{if(await refreshHandler())say(uiText('面板数据已刷新。','Dashboard data refreshed.'),true)}).catch(error=>say(error.message));
$('sync').onclick=()=>withButtonLoading($('sync'),()=>withModuleLoading(loadingTargets.catalog,()=>syncHandler(),uiText('正在同步 Key 与模型…','Synchronizing Keys and models…'))).catch(error=>say(error.message));
$('accounts').onclick=()=>withButtonLoading($('accounts'),()=>withModuleLoading(loadingTargets.accounts,()=>accountHandler(),uiText('正在读取账号池…','Loading account pool…'))).catch(error=>say(error.message));
$('quota-refresh').onclick=()=>withButtonLoading($('quota-refresh'),()=>withModuleLoading(loadingTargets.accounts,()=>quotaRefreshHandler(),uiText('正在刷新官方额度…','Refreshing official quota…'))).catch(error=>say(error.message));
$('manage').onclick=()=>withButtonLoading($('manage'),()=>withModuleLoading('.key-panel>.table-wrap',()=>manageHandler(),uiText('正在读取 CPA Key…','Loading CPA Keys…'))).catch(error=>say(error.message));
$('analysis-preset').onchange=()=>{const preset=$('analysis-preset').value;if(preset!=='custom')applyAnalysisPreset(preset);reloadTrendWithLoading()};
$('analysis-apply').onclick=()=>{if(!$('analysis-from').value||!$('analysis-to').value){say(uiText('请选择完整的统计日期区间。','Select a complete reporting date range.'));return}$('analysis-preset').value='custom';reloadTrendWithLoading()};
$('analysis-granularity').onchange=reloadTrendWithLoading;
$('decision-refresh').onclick=()=>reloadDecisionWithLoading();
$('decision-filter').onchange=()=>reloadDecisionWithLoading(true);
$('decision-page-size').onchange=()=>reloadDecisionWithLoading(true);
$('decision-search').oninput=()=>{clearTimeout(decisionSearchTimer);decisionSearchTimer=setTimeout(()=>reloadDecisionWithLoading(true),220)};
$('decision-prev').onclick=()=>{if(state.decisionPage.page>1){state.decisionPage.page--;reloadDecisionWithLoading()}};
$('decision-next').onclick=()=>{if(state.decisionPage.page<state.decisionPage.total_pages){state.decisionPage.page++;reloadDecisionWithLoading()}};
$('operation-refresh').onclick=()=>reloadOperationWithLoading();
$('operation-level').onchange=()=>reloadOperationWithLoading(true);
$('operation-page-size').onchange=()=>reloadOperationWithLoading(true);
$('operation-search').oninput=()=>{clearTimeout(operationSearchTimer);operationSearchTimer=setTimeout(()=>reloadOperationWithLoading(true),220)};
$('operation-prev').onclick=()=>{if(state.operationPage.page>1){state.operationPage.page--;reloadOperationWithLoading()}};
$('operation-next').onclick=()=>{if(state.operationPage.page<state.operationPage.total_pages){state.operationPage.page++;reloadOperationWithLoading()}};
$('decision-clear').onclick=clearDecisionLogs;
$('operation-clear').onclick=clearOperationalLogs;
// Keep the table compact while exposing the complete stored log row on demand.
// This is display-only and does not change request-log retention or persistence.
const closeLogDetail=()=>{if($('log-detail-dialog').open)$('log-detail-dialog').close()};
const openLogDetail=(log,row)=>{
	if(!log)return;
	const cells=row?.children||[],key=keyFor(log.key_id)||keyFor(state.selected),suffixSource=text(log.key_suffix)?log:key;
	const set=(id,value)=>{$(id).textContent=value===undefined||value===null||value===''?'—':String(value)};
	set('log-detail-time',formatLogTime(log.requested_at));
	set('log-detail-key-name',key?.name);
	set('log-detail-key-id',suffixSource?suffix(suffixSource):uiText('尾号未知','Suffix unavailable'));
	set('log-detail-model',log.model);
	set('log-detail-decision',cells[5]?.textContent);
	set('log-detail-token',cells[6]?.textContent);
	set('log-detail-http',Number(log.status_code)||'—');
	set('log-detail-account',log.auth_id);
	set('log-detail-reason',log.reason);
	set('log-detail-matched-term',log.matched_term);
	set('log-detail-matched-category',log.matched_category);
	set('log-detail-request',log.request_content);
	set('log-detail-description',cells[9]?.textContent||log.reason);
	$('log-detail-dialog').showModal();
	window.__ccLocalizeAdditional?.($('log-detail-dialog'));
};
$('logs').addEventListener('click',event=>{
	const button=event.target.closest('.log-detail-open');
	if(!button)return;
	openLogDetail(state.logs?.[Number(button.dataset.logIndex)],button.closest('tr'));
});
$('log-detail-close').onclick=closeLogDetail;
$('log-detail-close-footer').onclick=closeLogDetail;
// Earlier compatibility renderers were written before the request-content
// column existed. Wrap their final output once so every column keeps its value.
queueMicrotask(()=>{
	const localizedRenderLogs=renderLogs;
	renderLogs=function(){
		localizedRenderLogs();
		const rows=Array.from(document.querySelectorAll('#logs tr')),logs=state.logs||[];
		rows.forEach((row,index)=>{
			const log=logs[index],cells=row.children;
			if(!log||cells.length<11)return;
			const request=text(log.request_content)||'—',description=cells[7].textContent;
			cells[4].textContent=request;
			cells[4].title=request;
			cells[7].textContent=Number(log.status_code)||'—';
			cells[7].className='log-http '+(Number(log.status_code)>=400?'bad':(Number(log.status_code)>0?'ok':''));
			cells[9].textContent=description;
			cells[9].title=description;
		});
	};
});
async function openQuotaDebug(){if(!state.selected){say(uiText('请先选择一个受控 Key。','Select a managed Key first.'));return}const button=$('quota-debug-open');if(button)button.disabled=true;try{const payload=await api('/debug/quota?key_id='+encodeURIComponent(state.selected));$('quota-debug-output').value=JSON.stringify(payload,null,2);$('quota-debug-dialog').showModal()}catch(error){say(error.message)}finally{if(button)button.disabled=false}}async function copyQuotaDebug(){const output=$('quota-debug-output');if(!output.value){say(uiText('请先生成额度诊断。','Generate quota diagnostics first.'));return}try{await navigator.clipboard.writeText(output.value)}catch{output.focus();output.select();if(!document.execCommand('copy'))throw new Error(uiText('浏览器拒绝复制，请手动复制诊断内容。','The browser denied copying. Copy the diagnostic content manually.'))}say(uiText('额度调试诊断已复制，可直接提供给开发排查。','Quota diagnostics copied. You can provide them directly for support.'),true)}const quotaDebugButton=$('quota-debug-open');if(quotaDebugButton)quotaDebugButton.textContent=uiText('额度诊断','Quota diagnostics');$('quota-debug-open').onclick=()=>openQuotaDebug();$('quota-debug-close').onclick=()=>{$('quota-debug-dialog').close()};$('quota-debug-copy').onclick=()=>copyQuotaDebug().catch(error=>say(error.message));window.__ccFeature={api,keyFor,suffix,suffixTitle,escapeHTML,formatLogTime,num,uiText,say,withModuleLoading,loadingTargets};window.__ccPanelBridge?.attach?.({state,render,renderLogs,renderOperationalLogs,cpaLocale,uiText,tokens,showToast,say});})();

// Dialog validation and save failures stay with the form; page-level feedback
// uses only the deduplicated top-right toast.
(()=>{
	const clear=dialog=>{const node=dialog.querySelector('.dialog-message');if(!node)return;node.hidden=true;node.textContent='';node.className='dialog-message'};
	document.querySelectorAll('dialog').forEach(dialog=>{dialog.addEventListener('input',()=>clear(dialog));dialog.addEventListener('close',()=>clear(dialog))});
})();

// Delegate tab clicks from the stable document root. CPA theme/localization
// refreshes may replace a button node, so listeners bound only to the initial
// buttons can disappear while the visible controls remain on screen.
(()=>{const validTabs=new Set(['decision','forbidden','operation']),copy=tab=>tab==='operation'?uiText('插件生命周期、配置、官方额度同步与异常记录','Plugin lifecycle, configuration, official quota sync, and exception records'):(tab==='forbidden'?uiText('全部受控 Key 的违禁词命中、请求摘要与拦截结果','Forbidden-phrase matches, request excerpts, and blocks across all managed Keys'):uiText('当前 Key 的使用、策略决策与 Token 结算记录','Usage, policy decisions, and Token settlements for the selected Key'));const activate=tab=>{if(!validTabs.has(tab))return;document.querySelectorAll('[data-log-tab]').forEach(button=>{const selected=button.dataset.logTab===tab;button.classList.toggle('active',selected);button.setAttribute('aria-selected',String(selected))});for(const name of validTabs){const selected=name===tab,view=document.getElementById(name+'-log-view'),tools=document.getElementById(name+'-log-tools');if(view)view.hidden=!selected;if(tools)tools.hidden=!selected}const description=document.getElementById('log-tab-description');if(description)description.textContent=copy(tab);document.dispatchEvent(new CustomEvent('codex-carpool:log-tab-changed',{detail:{tab}}))};document.addEventListener('click',event=>{const target=event.target instanceof Element?event.target.closest('[data-log-tab]'):null;if(!target)return;event.preventDefault();activate(target.dataset.logTab)},true);window.__ccActivateLogTab=activate;activate('decision')})();

function createDisabledObserver(){return {observe(){}}}
(()=>{const pairs=[
['插件只读 CPA 的 Codex JSON 认证文件，用相对文件路径匹配 CPA 调度账号；OAuth Token 仅保留在插件进程内存，不写入插件数据库。允许最终目标仍在目录内的 JSON 软链接，但同一真实文件或相同 Codex account_id 只能加入共享池一次。没有稳定账号身份的文件仅可作为池中唯一账号，不能与其他账号混用。请使用本机可用目录，不要配置 NFS/FUSE 等远程或异常挂载。默认目录为 ~/.cli-proxy-api。','The plugin reads CPA Codex JSON authentication files and matches CPA scheduler accounts by relative path. OAuth tokens stay only in process memory and are never stored in the plugin database. JSON symbolic links are allowed only when their final targets remain inside the directory. The same physical file or Codex account_id can be added to the shared pool only once. A file without a stable account identity can be used only as the pool’s sole account and cannot be mixed with other accounts. Use a local directory; do not configure NFS, FUSE, or other remote or abnormal mounts. The default is ~/.cli-proxy-api.'],
['Codex 拼车插件 · 未配置策略的 CPA Key 不限制 · 官方账号窗口驱动的共享总池','Codex carpool plugin · Unconfigured CPA Keys remain unrestricted · Shared pool driven by official account windows'],
['官方周额度 · 账号池实时健康度','Official weekly quota · Real-time account health'],
['仅在后台按需与每 5 分钟同步官方额度；官方返回 ','Official quota is synchronized on demand and every 5 minutes; an account is paused only when the official API returns '],
[' 才会暂停账号并等待恢复时间。',' before being paused until its reset time.'],
['Key 仅配置一个 x 分配；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，下一次官方周百分比更新后自动校正。没有可用官方快照时会安全返回 503。','A Key has one x allocation. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile at the next official weekly percentage update. Requests safely return 503 when no current official quota snapshot is available.'],
['当前 Key 的使用、策略决策与 Token 结算记录','Usage, policy decisions, and Token settlements for the selected Key'],
['插件生命周期、配置、官方额度同步与异常记录','Plugin lifecycle, configuration, official quota sync, and exception records'],
['搜索模型、原因或账号标识','Search model, reason, or account ID'],
['搜索事件、说明、账号或 Key 标识','Search event, description, account, or Key ID'],
['正在连接 codex-carpool','Connecting to codex-carpool'],
['日期区间实际 Token','Actual Tokens in date range'],
['实际 Token 趋势图','Actual Token trend chart'],
['实时 Token 结算','Real-time Token settlement'],
['额度调试诊断内容','Quota diagnostic content'],
['CPA Codex 账号列表','CPA Codex account list'],
['纳入管理','Manage Key'],
['每页 100 条','Per page 100'],['每页 50 条','Per page 50'],['每页 20 条','Per page 20'],['每页 10 条','Per page 10'],
['批量配置共享账号池','Configure shared account pool'],['从 CPA 选择账号','Select accounts from CPA'],['全选可用','Select all available'],['保存所选账号','Save selected accounts'],
['CPA 认证目录','CPA authentication directory'],['保存并刷新','Save and refresh'],
['启用管理','Enable management'],['暂停管理（不限制）','Pause management (unrestricted)'],
['例如：开发组 Key','For example: Development Key'],['允许访问的星期','Allowed weekdays'],
['可勾选任意星期；全选即每天。开始时间晚于结束时间时，时段会跨午夜延续到次日。','Select any weekdays; selecting all means every day. If the start time is later than the end time, the slot continues past midnight into the next day.'],
['额度诊断','Quota diagnostics'],['额度概览','Quota overview'],['分析时间预设','Analysis time preset'],['分析开始日期','Analysis start date'],['分析结束日期','Analysis end date'],['分析粒度','Analysis granularity'],
['搜索使用日志','Search usage logs'],['决策状态','Decision status'],['每页使用日志数','Usage logs per page'],['搜索运行日志','Search runtime logs'],['运行日志级别','Runtime log level'],['每页日志数','Logs per page'],
['插件管理','Plugin management'],['正在连接','Connecting'],['刷新','Refresh'],['认证目录','Auth directory'],['同步 CPA Key 与模型','Sync CPA keys and models'],['Codex 拼车插件','Codex carpool plugin'],['受控 Key','Managed Keys'],['账号池','Account pool'],['已分配','Allocated'],['可分配','Available'],['Key 分配与使用','Key allocation and usage'],['搜索备注或 CPA Key 尾号','Search remark or CPA Key suffix'],['纳入 Key 管理','Manage Key'],['Key 备注','Key remark'],['分配额度','Allocation'],['模型策略','Model policy'],['状态','Status'],['当前 Key 详情','Current Key details'],['编辑策略','Edit policy'],['解除管理','Stop managing'],['共享池分配','Shared-pool allocation'],['周账期 Token 守卫用量','Weekly Token guard usage'],['已结算','Settled'],['剩余','Remaining'],['当前页决策','Decisions on this page'],['完成结算','Completed settlements'],['当前页成功率','Success rate on this page'],['默认允许全部模型','All models allowed by default'],['允许全部','Allow all'],['管理中','Managed'],['不限制','Unrestricted'],['需重新绑定','Rebind required'],['单 Key 实际 Token 分析','Per-Key actual Token analysis'],['按日、月、年或自定义日期区间；仅统计已落库的实际完成结算（通常延迟不超过 1 秒）','Daily, monthly, yearly, or custom range; only durable completed usage is counted (normally within 1 second).'],['今日','Today'],['近 7 天','Last 7 days'],['本月','This month'],['本年','This year'],['自定义','Custom'],['按日','Daily'],['按月','Monthly'],['按年','Yearly'],['查询','Query'],['官方账号池','Official account pool'],['刷新额度','Refresh quota'],['配置账号池','Configure account pool'],['日志与诊断','Logs and diagnostics'],['使用与策略日志','Usage and policy logs'],['违禁词拦截日志','Forbidden-phrase logs'],['插件运行与错误日志','Plugin runtime and error logs'],['违禁词拦截','Forbidden-phrase filtering'],['全部决策','All decisions'],['已完成','Completed'],['已拦截','Blocked'],['上游失败','Upstream failed'],['未匹配/忽略','Unmatched / ignored'],['账期释放','Window released'],['每页','Per page'],['刷新日志','Refresh logs'],['上一页','Previous'],['下一页','Next'],['关闭','Close'],['取消','Cancel'],['保存设置','Save settings'],['保存策略','Save policy'],['Key 备注名称','Key remark name'],['共享池分配（x）','Shared-pool allocation (x)'],['允许模型','Allowed models'],['未勾选任何模型＝默认允许全部','No selection means all models are allowed'],['允许访问时段','Allowed access times'],['不设置＝不限时访问','No setting means unrestricted access'],['仅允许指定时段访问','Allow only specified times'],['时区','Time zone'],['开始时间','Start time'],['结束时间','End time'],['周一','Mon'],['周二','Tue'],['周三','Wed'],['周四','Thu'],['周五','Fri'],['周六','Sat'],['周日','Sun'],['添加时段','Add time slot'],['移除','Remove'],['复制诊断','Copy diagnostics'],['额度调试诊断','Quota diagnostics'],['共享池可用额度','Available shared-pool allocation'],['池健康度','Pool health'],['账号可用','accounts available'],['周剩余','Weekly remaining'],['周恢复','Weekly reset'],['等待配置','Awaiting setup'],['官方同步','Official sync'],['同步失败','Sync failed'],['当前日期区间暂无实际完成结算。','No completed actual usage in this date range.'],['数据起于','Data available from'],['暂无已结算历史','No settled history yet'],['个区间','periods'],['保留','Retained'],['次请求','requests'],['实际','Actual'],['持久化异常：暂停受控 Key','Persistence issue: managed Keys are paused'],['认证目录正在复核或存在重复、无法确认的账号身份：暂停受控 Key','Auth directory is being verified or has duplicate or unverifiable accounts: managed Keys are paused'],['年度用量分析正在使用受限回退连接','Annual usage analysis is using a limited fallback connection'],['用量分析暂时不可用，请稍后重试','Usage analysis is temporarily unavailable. Please retry.'],['请选择完整的统计日期区间。','Select a complete reporting date range.'],['一个 Key 最多配置 16 个访问时段。','A Key supports at most 16 access time slots.'],['每个访问时段都至少选择一个星期。','Select at least one weekday for every access time slot.'],['每个访问时段的开始和结束时间必须存在且不能相同。','Every access time slot needs different start and end times.'],['请填写访问时段的 IANA 时区，例如 Asia/Shanghai。','Enter an IANA time zone, for example Asia/Shanghai.'],['Usage analysis is temporarily unavailable. Please retry.','Usage analysis is temporarily unavailable. Please retry.']];const ordered=pairs.slice().sort((a,b)=>Math.max(b[0].length,b[1].length)-Math.max(a[0].length,a[1].length));let applying=false;const language=()=>cpaLocale().startsWith('zh')?'zh':'en';const translate=value=>{let output=String(value??''),english=language()==='en';for(const [zh,en] of ordered){const from=english?zh:en,to=english?en:zh;if(from===en&&en==='To')output=output.replace(/\bTo\b/g,to);else if(from!==to)output=output.split(from).join(to)}return output};window.__ccLocalizeCore=root=>{if(!root||applying)return;applying=true;const visit=node=>{if(!node)return;if(node.nodeType===Node.TEXT_NODE){const parent=node.parentElement;if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){const next=translate(node.nodeValue);if(next!==node.nodeValue)node.nodeValue=next}return}if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;for(const name of ['title','placeholder','aria-label']){if(node.hasAttribute(name)){const next=translate(node.getAttribute(name));if(next!==node.getAttribute(name))node.setAttribute(name,next)}}for(const child of node.childNodes)visit(child)};visit(root);applying=false};window.__ccSyncLocale=()=>{const next=language(),changed=state.locale!==next;state.locale=next;document.documentElement.lang=next==='zh'?'zh-CN':'en';window.__ccLocalizeCore(document.body);if(changed&&typeof render==='function')render()};window.__ccSyncLocale();createDisabledObserver(records=>{if(applying)return;for(const record of records){for(const node of record.addedNodes)window.__ccLocalizeCore(node);if(record.type==='characterData')window.__ccLocalizeCore(record.target)}}).observe(document.body,{childList:true,subtree:true,characterData:true});try{const root=parent.document.documentElement;createDisabledObserver(window.__ccSyncLocale).observe(root,{attributes:true,attributeFilter:['lang','class','data-locale','data-language']})}catch{};window.addEventListener('languagechange',window.__ccSyncLocale)})();

function createDisabledObserver(){return {observe(){}}}
// Keep the English page suffix non-empty: an empty reverse-translation key
// makes String.split('') rewrite every character and can freeze the panel.
(()=>{const pairs=[['至','To'],['窗口开始','Window start'],['时间','Time'],['模型','Model'],['决策','Decision'],['说明','Description'],['级别','Level'],['事件','Event'],['共 ','Total '],['第 ','Page '],[' 页','\u200B'],['全部级别','All levels'],['仅错误','Errors only'],['仅警告','Warnings only'],['仅运行','Info only'],['尚未配置共享账号池。','No shared account pool configured.'],['Key 仅配置一个 x 分配；实际 Token 按实际使用账号的官方额度窗口结算与恢复，官方未返回的窗口不会限制。','A Key has one x allocation. Actual Tokens settle and reset by the official window of the account used; unavailable official windows do not restrict it.'],['可一次勾选多个 CPA Codex 账号，并分别填写容量 x。已保存的启用账号会默认勾选；未勾选的既有账号不会被修改。整批会先校验账号身份和总分配，再原子保存并同步官方额度。','Select multiple CPA Codex accounts and set each capacity x. Existing enabled accounts are preselected; unselected existing accounts remain unchanged. The batch is identity- and capacity-checked, then saved atomically and synchronized.'],['插件只读 CPA 的 Codex JSON 认证文件，用相对文件路径匹配 CPA 调度账号；OAuth Token 仅保留在插件进程内存，不写入插件数据库。','The plugin reads CPA Codex JSON auth files and matches CPA scheduler accounts by relative path; OAuth tokens stay only in process memory and are never stored in the plugin database.'],['可复制此诊断交给开发排查。仅包含本地守卫公式、脱敏账号尾号、官方周百分比/恢复时间和最近 50 条精简决策；不包含 API Key、OAuth 凭据、提示词或正文。','Copy this diagnostic for support. It contains local guard formulas, masked account suffixes, official weekly percentages/reset times and the latest 50 compact decisions; it excludes API Keys, OAuth credentials, prompts and bodies.'],['当前 Key 的使用、策略决策与 Token 结算记录','Usage, policy decisions and Token settlements for the selected Key'],['插件生命周期、配置、官方额度同步与异常记录','Plugin lifecycle, configuration, official quota sync and exception records'],['Usage analysis is temporarily unavailable. Please retry.','Usage analysis is temporarily unavailable. Please retry.']];let applying=false;const english=()=>document.documentElement.lang.toLowerCase().startsWith('en');const translate=value=>{let output=String(value??'');for(const [zh,en] of pairs){const from=english()?zh:en,to=english()?en:zh;if(from===en&&en==='To')output=output.replace(/\bTo\b/g,to);else if(from!==to)output=output.split(from).join(to)}return output};window.__ccLocalizeLogs=root=>{if(!root||applying)return;applying=true;const walk=node=>{if(node.nodeType===Node.TEXT_NODE){const parent=node.parentElement;if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){const next=translate(node.nodeValue);if(next!==node.nodeValue)node.nodeValue=next}return}if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;for(const name of ['title','placeholder','aria-label'])if(node.hasAttribute(name)){const next=translate(node.getAttribute(name));if(next!==node.getAttribute(name))node.setAttribute(name,next)}for(const child of node.childNodes)walk(child)};walk(root);applying=false};window.__ccLocalizeLogs(document.body);createDisabledObserver(records=>{if(!applying)for(const record of records){for(const node of record.addedNodes)window.__ccLocalizeLogs(node);if(record.type==='characterData')window.__ccLocalizeLogs(record.target)}}).observe(document.body,{childList:true,subtree:true,characterData:true});createDisabledObserver(()=>window.__ccLocalizeLogs(document.body)).observe(document.documentElement,{attributes:true,attributeFilter:['lang']})})();

(()=>{const pick=pair=>uiText(pair[0],pair[1]);const decisionReasons={access_schedule_closed:['当前时间不在该 Key 的允许访问时段内。','The current time is outside this Key\'s allowed access schedule.'],model_not_allowed:['该 Key 不允许使用此模型。','This Key is not allowed to use the requested model.'],quota_unavailable:['额度引擎暂不可用。','The quota engine is temporarily unavailable.'],quota_account_source_conflict:['共享账号身份正在核验或存在冲突。','Shared account identities are being verified or have a conflict.'],quota_persistence_unavailable:['额度账本暂不可用。','The quota ledger is temporarily unavailable.'],quota_scheduler_candidates_required:['CPA 未提供可用调度账号候选。','CPA did not provide scheduler account candidates.'],quota_pool_unconfigured:['尚未配置共享账号池。','No shared account pool is configured.'],quota_snapshot_unavailable:['没有可用的官方额度快照。','No current official quota snapshot is available.'],quota_candidate_mismatch:['CPA 调度候选与共享账号池不匹配。','CPA scheduler candidates do not match the shared account pool.'],quota_pool_exhausted:['共享账号池官方额度已耗尽。','Official shared-pool allowance is exhausted.'],quota_allocation_exhausted:['该 Key 在当前官方账期内的共享池分配已用完。','This Key has exhausted its shared-pool allocation for the current official window.'],quota_account_unavailable:['当前没有可用的共享账号。','No shared account is currently available.'],quota_persistence_stopping:['额度账本正在停止。','The quota ledger is stopping.'],reservation_expired_at_official_reset:['官方周账期刷新，未完成预留已释放。','The official weekly window reset and unfinished reservations were released.'],unmatched_usage_callback:['未匹配到待结算请求的用量回调已记录。','An unmatched usage callback was recorded.']};const operationMessages={plugin_started:['插件已启动。','Plugin started.'],plugin_stopping:['插件正在停止。','Plugin is stopping.'],plugin_shutdown_conservative:['插件关闭超时，未结算预留已安全保留。','Plugin shutdown timed out; unsettled reservations were safely retained.'],plugin_reconfigured:['CPA 已重新加载插件配置。','CPA reloaded the plugin configuration.'],plugin_panic:['插件调用发生未处理异常。','An unhandled plugin call exception occurred.'],installation_updated:['插件运行设置已更新。','Plugin runtime settings were updated.'],management_request_failed:['插件管理请求失败，请查看后方详情。','Plugin management request failed; see the diagnostic detail.'],key_policy_saved:['Key 管理策略已保存。','Key management policy saved.'],key_policy_deleted:['Key 已解除插件管理。','Key removed from plugin management.'],key_usage_reset:['Key 插件用量已重置。','Key plugin usage reset.'],model_catalog_synced:['Codex 模型目录已同步。','Codex model catalog synchronized.'],account_pool_batch_saved:['共享账号池已批量保存。','Shared account pool saved in a batch.'],account_pool_saved:['共享账号池配置已保存。','Shared account-pool configuration saved.'],account_pool_deleted:['账号已从共享池移除。','Account removed from the shared pool.'],quota_refresh_requested:['已请求刷新官方额度。','Official quota refresh requested.'],quota_sync_pending:['账号池已保存，正在等待官方额度快照。','The account pool is saved and waiting for official quota snapshots.'],quota_sync_recovered:['官方额度同步已恢复。','Official quota synchronization recovered.'],quota_sync_failed:['官方额度同步失败，请检查认证目录和账号状态。','Official quota synchronization failed. Check the auth directory and account status.'],quota_sync_auth_file_retry:['CPA 认证文件正在重试读取。','Retrying the CPA auth-file read.'],duplicate_auth_source:['发现重复或无法确认的 CPA 认证身份，受控 Key 已暂停。','Duplicate or unverifiable CPA auth identities were found; managed Keys are paused.'],duplicate_auth_source_resolved:['CPA 认证身份冲突已解除，受控 Key 已恢复。','CPA auth identity conflict resolved; managed Keys resumed.'],official_quota_exhausted:['官方额度已耗尽，账号暂时停止调度。','Official quota is exhausted; the account is temporarily unavailable for scheduling.'],reservation_expired_at_official_reset:['官方周账期刷新，未完成预留已释放。','Official weekly window reset; unfinished reservations were released.'],official_quota_calibration_failed:['官方额度校准未更新。','Official quota calibration was not updated.'],usage_analysis_reader_degraded:['用量分析已降级为共享数据库连接；额度守卫不受影响。','Usage analysis fell back to the shared database connection; quota guarding is unaffected.'],usage_analysis_reader_restored:['用量分析已恢复独立只读数据库连接。','Usage analysis restored its independent read-only database connection.']};const decisionText=log=>{const status=Number(log.status_code||0),kind=log.decision;let decision=kind==='blocked'?uiText('拦截 ','Blocked ')+status:kind==='failed'?uiText('失败','Failed')+(status?' '+status:''):kind==='completed'?uiText('完成 ','Completed ')+tokens(log.units)+' Token':kind==='ignored'?uiText('已忽略','Ignored'):kind==='expired'?uiText('已释放','Released'):uiText('已记录','Recorded');let description=kind==='completed'?uiText('实际 Token 结算','Actual Token settlement'):kind==='failed'?uiText('上游请求未完成','Upstream request did not complete'):pick(decisionReasons[log.reason]||[log.reason||'策略记录','Policy record']);return {decision,description}};const renderDecisionLogs=renderLogs;renderLogs=function(){renderDecisionLogs();const rows=Array.from(document.querySelectorAll('#logs tr')),logs=state.logs||[];rows.forEach((row,index)=>{const log=logs[index],cells=row.children;if(!log||cells.length<8)return;const output=decisionText(log),account=(['completed','failed','ignored','expired'].includes(log.decision)&&log.auth_id)?' · '+uiText('账号 ','Account ')+'••••'+String(log.auth_id).slice(-4):'',badge=cells[4].querySelector('.decision');if(badge)badge.textContent=output.decision;else cells[4].textContent=output.decision;cells[7].textContent=output.description+account})};const renderOperationLogs=renderOperationalLogs;renderOperationalLogs=function(){renderOperationLogs();const rows=Array.from(document.querySelectorAll('#operation-logs tr')),logs=state.operationLogs||[];rows.forEach((row,index)=>{const log=logs[index],cells=row.children;if(!log||cells.length<4)return;const account=log.auth_id?' · '+uiText('账号 ','Account ')+'••••'+String(log.auth_id).slice(-4):'',key=log.key_id?' · Key '+String(log.key_id).slice(-8):'';cells[2].textContent=log.event||'—';cells[3].textContent=pick(operationMessages[log.event]||[uiText('已记录插件运行事件。','Plugin runtime event recorded.'),uiText('Plugin runtime event recorded.','Plugin runtime event recorded.')])+account+key})}})();

function createDisabledObserver(){return {observe(){}}}
(()=>{const pairs=[['未找到 CPAMP 已保存的管理密钥。请在同一地址登录 CPAMP 并启用记住管理密钥。','CPAMP has no saved management key. Sign in to CPAMP at this address and enable remembering the management key.'],['最近模型同步：','Latest model sync:'],['已丢弃 ','Dropped '],[' 条低优先级日志',' low-priority logs'],['已连接 codex-carpool','Connected to codex-carpool'],['等待配置','Awaiting setup'],['周恢复：','Weekly reset:'],['尚未获得官方恢复时间','Official reset time is not available yet'],['等待官方周额度数据','Waiting for official weekly quota data'],['默认允许全部模型','All models are allowed by default'],['旧策略需重新绑定 CPA Key 后才可启用','The legacy policy must be rebound to a current CPA Key before it can be enabled'],['创建时已校验所有 Key 总分配不超过账号池容量','All Key allocations were validated against account-pool capacity when created'],['实际 Token 结算','Actual Token settlement'],['上游请求未完成','Upstream request did not complete'],['策略记录','Policy record'],['暂无该 Key 的使用或策略日志。','No usage or policy logs for this Key yet.'],['暂无插件运行或错误日志。','No plugin runtime or error logs yet.'],['CPA 未返回可同步的 Codex 模型目录，已保留现有目录。','CPA did not return a Codex model catalog to synchronize; the current catalog was retained.'],['CPA 未返回可用 Codex 模型，已保留现有目录。','CPA did not return usable Codex models; the current catalog was retained.'],['模型目录同步失败，已保留上次成功目录','Model catalog synchronization failed; the last successful catalog was retained'],['正在从 CPA 同步 Key 与 Codex 模型…','Synchronizing CPA Keys and Codex models…'],['请先同步 CPA Key','Synchronize CPA Keys first'],['请先同步 CPA 模型目录。','Synchronize the CPA model catalog first.'],['CPA 中没有可用的 Codex 账号。','CPA has no usable Codex accounts.'],['请填写 CPA 认证目录。','Enter the CPA authentication directory.'],['每个已选账号都需要填写正数容量。','Every selected account needs a positive capacity.'],['请至少选择一个 CPA Codex 账号。','Select at least one CPA Codex account.'],['正在从 CPA 读取可用 Key…','Reading available Keys from CPA…'],['旧版 SHA 指纹不能安全转换，请选择 CPA 当前 Key 重新绑定后再启用。','Legacy SHA fingerprints cannot be converted safely. Select a current CPA Key, rebind it, then enable it.'],['仅配置一个共享池 x 分配；实际 Token 按实际使用账号的官方周窗口结算，账号耗尽时自动切换。','Configure one shared-pool x allocation. Actual Tokens settle in the official weekly window of the account used, and routing switches automatically when an account is exhausted.'],['请填写 Key 备注名称与正数共享池分配。','Enter a Key remark and a positive shared-pool allocation.'],['请先同步并选择 CPA Key。','Synchronize and select a CPA Key first.'],['策略已保存。原始 Key 已从页面内存清除。','Policy saved. The original Key was cleared from page memory.'],['已解除管理，该 Key 已恢复 CPA 原调度。','Management removed. This Key has returned to CPA scheduling.'],['模型目录自动同步失败，可点击“同步 CPA Key 与模型”重试。','Automatic model synchronization failed. Use “Sync CPA keys and models” to retry.'],['请选择完整的统计日期区间。','Select a complete reporting date range.'],['不限时访问','Unrestricted access'],['每天','Every day'],['指定星期','Selected weekdays'],['请至少选择一个允许访问的星期。','Select at least one allowed weekday.'],['访问开始和结束时间必须存在且不能相同。','Access start and end times are required and must differ.'],['请填写访问时段的 IANA 时区，例如 Asia/Shanghai。','Enter an IANA time zone for the access schedule, for example Asia/Shanghai.'],['额度调试诊断已复制，可直接提供给开发排查。','Quota diagnostics copied. You can provide them directly for support.'],['浏览器拒绝复制，请手动复制诊断内容。','The browser denied copying. Copy the diagnostic content manually.']];const english=()=>cpaLocale().startsWith('en');const translate=value=>pairs.reduce((output,[zh,en])=>output.split(english()?zh:en).join(english()?en:zh),String(value??''));window.__ccLocalizeAdditional=root=>{if(!root)return;const walk=node=>{if(node.nodeType===Node.TEXT_NODE){const parent=node.parentElement;if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){const next=translate(node.nodeValue);if(next!==node.nodeValue)node.nodeValue=next}return}if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;for(const name of ['title','placeholder','aria-label'])if(node.hasAttribute(name)){const next=translate(node.getAttribute(name));if(next!==node.getAttribute(name))node.setAttribute(name,next)}for(const child of node.childNodes)walk(child)};walk(root)};window.__ccLocalizeAdditional(document.body);createDisabledObserver(records=>{for(const record of records){for(const node of record.addedNodes)window.__ccLocalizeAdditional(node);if(record.type==='characterData')window.__ccLocalizeAdditional(record.target)}}).observe(document.body,{childList:true,subtree:true,characterData:true})})();

(()=>{const previousOperationalRenderer=renderOperationalLogs;const diagnosticEvents=new Set(['management_request_failed','quota_sync_failed','quota_sync_auth_file_retry','duplicate_auth_source','official_quota_calibration_failed']);const diagnosticText=message=>String(message||'').replace(/^官方额度同步失败[：:]?/, '').trim();renderOperationalLogs=function(){previousOperationalRenderer();const rows=Array.from(document.querySelectorAll('#operation-logs tr')),logs=state.operationLogs||[];rows.forEach((row,index)=>{const log=logs[index],cells=row.children;if(!log||cells.length<4||!diagnosticEvents.has(log.event))return;const detail=diagnosticText(log.message);if(detail)cells[3].textContent=(cells[3].textContent||'')+' · '+detail})};const style=document.createElement('style');style.textContent='#toast-region>div{background:var(--card,#fff)!important;color:var(--ink,#15243b)!important;border-color:color-mix(in srgb,var(--line,#dbe4ef) 82%,var(--blue,#409eff))!important}#toast-region>div[role=alert]{border-color:color-mix(in srgb,var(--red,#e04f5f) 45%,var(--line,#dbe4ef))!important}';document.head.append(style)})();

(()=>{const localizeAll=root=>{for(const localize of [window.__ccLocalizeCore,window.__ccLocalizeLogs,window.__ccLocalizeAdditional])if(typeof localize==='function')localize(root)};let queued=false,roots=[];const flush=()=>{queued=false;const batch=roots;roots=[];for(const root of batch)localizeAll(root)};const queue=root=>{const node=root?.nodeType===3?root.parentElement:root;if(!node)return;if(roots.some(item=>item===node||item.contains?.(node)))return;roots=roots.filter(item=>!node.contains?.(item));roots.push(node);if(queued)return;queued=true;(window.requestAnimationFrame||window.setTimeout)(flush,0)};const observer=new MutationObserver(records=>{let localeChanged=false;for(const record of records){if(record.type==='attributes'){localeChanged=true;continue}for(const node of record.addedNodes)queue(node)}if(localeChanged){roots=[];if(typeof window.__ccSyncLocale==='function')window.__ccSyncLocale();else localizeAll(document.body)}});observer.observe(document.body,{childList:true,subtree:true});try{observer.observe(parent.document.documentElement,{attributes:true,attributeFilter:['lang','class','data-locale','data-language']})}catch{};queue(document.body)})();

/* Product interaction overrides. */
/* Labels introduced by the approved static layout. Dynamic content continues
 * to use the panel's existing uiText helper, so it follows CPA's language. */
(()=>{
  const labels={keyManageAction:['Key 管理','Key Management'],operation:['操作','Actions'],clearLogs:['清除日志','Clear logs']};
  const english=()=>String(window.cpaLocale?.()||document.documentElement.lang||'').toLowerCase().startsWith('en');
  const renderHostLabel=()=>{
    try{
      const hostDocument=parent.document;
      if(hostDocument===document||!hostDocument.body)return;
      const desired=english()?'Codex Carpool':'Codex 拼车';
      const walker=hostDocument.createTreeWalker(hostDocument.body,4);
      while(walker.nextNode()){
        const node=walker.currentNode,value=String(node.nodeValue||''),label=value.trim();
        if(label==='Codex 拼车'||label==='Codex Carpool')node.nodeValue=value.replace(label,desired);
      }
    }catch{}
  };
  const renderLabels=()=>document.querySelectorAll('[data-cc-label]').forEach(node=>{
    const pair=labels[node.dataset.ccLabel];
    if(pair)node.textContent=pair[english()?1:0];
  });
  const previous=window.__ccSyncLocale;
  window.__ccSyncLocale=()=>{previous?.();renderLabels();renderHostLabel()};
  renderLabels();
  renderHostLabel();
})();

// The panel's core API keeps the durable ledger and query state private. This
// independent view renderer consumes only the bridge's read-only state after
// each core render, so the presentation can evolve without changing quota
// admission, storage, or CPA routing.
(()=>{
  const get=id=>document.getElementById(id);
  const english=()=>String(window.cpaLocale?.()||navigator.language||'').toLowerCase().startsWith('en');
  const copy=(zh,en)=>english()?en:zh;
  const escape=value=>{const node=document.createElement('span');node.textContent=String(value??'');return node.innerHTML};
  const formatNumber=value=>new Intl.NumberFormat(english()?'en-US':'zh-CN',{maximumFractionDigits:2}).format(Number(value)||0);
  const formatTokens=value=>{const amount=Number(value)||0;if(amount>=1000000)return formatNumber(amount/1000000)+'M';if(amount>=1000)return formatNumber(amount/1000)+'K';return formatNumber(amount)};
  const xValue=(allocation,name)=>{const explicit=Number(allocation?.[name+'_x']);if(Number.isFinite(explicit))return Math.max(0,explicit);return Math.max(0,(Number(allocation?.[name])||0)/1000000)};
  const dateLabel=value=>{const raw=String(value||'');return raw||'—'};
  const policyAccess=policy=>{
    const rules=policy?.access_rules||[];
    if(!rules.length)return copy('不限时访问','Unrestricted access');
    const days=english()?['','Mon','Tue','Wed','Thu','Fri','Sat','Sun']:['','周一','周二','周三','周四','周五','周六','周日'];
    return rules.map(rule=>{
      const picked=(rule.weekdays||[]).map(day=>days[Number(day)]).filter(Boolean);
      const all=picked.length===7?copy('每天','Every day'):picked.join(english()?', ':'、');
      return (all||copy('指定星期','Selected days'))+' '+(rule.start||'')+'–'+(rule.end||'');
    }).join(' · ');
  };
  const setStaticCopy=()=>{
    const preset={today:copy('今日','Today'),seven:copy('近 7 天','Last 7 days'),month:copy('本月','This month'),year:copy('本年','This year'),custom:copy('自定义','Custom')};
    document.querySelectorAll('#analysis-preset option').forEach(option=>{option.textContent=preset[option.value]||option.textContent});
    const granularity={hour:copy('按小时','Hourly'),day:copy('按日','Daily'),month:copy('按月','Monthly'),year:copy('按年','Yearly')};
    document.querySelectorAll('#analysis-granularity option').forEach(option=>{option.textContent=granularity[option.value]||option.textContent});
    const title=get('analysis-name');
    if(title&&!title.dataset.hasKey)title.textContent=copy('单 Key 用量分析','Single-Key Usage Analysis');
    const subtitle=document.querySelector('.key-analysis .analysis-title small');
    if(subtitle)subtitle.textContent=copy('按真实 Token 结算和官方周账期守卫统计','Uses actual Token settlements and the official weekly-window guard');
    const separator=document.querySelector('.key-analysis .analysis-controls span');
    if(separator)separator.textContent=copy('至','to');
    const apply=get('analysis-apply');
    if(apply)apply.textContent=copy('查询','Apply');
  };
  const render=()=>{
    const state=window.state;
    if(!state||!state.summary)return;
    setStaticCopy();
    const key=(state.summary.keys||[]).find(item=>item.id===state.selected);
    const detail=get('detail'),metrics=get('analysis-metrics'),models=get('model-mix'),chart=get('chart'),labels=get('trend-labels');
    if(!detail||!metrics||!models||!chart||!labels)return;
    const title=get('analysis-name');
    if(!key){
      if(title){title.textContent=copy('单 Key 用量分析','Single-Key Usage Analysis');delete title.dataset.hasKey}
      detail.innerHTML='<div class="empty">'+copy('选择一个受控 Key 后查看真实用量统计。','Select a managed Key to view real usage statistics.')+'</div>';
      metrics.innerHTML='';models.innerHTML='';window.__ccRenderUsageLineChart?.([]);
      return;
    }
    const allocation=key.allocation||{},capacity=xValue(allocation,'capacity'),policyCapacity=xValue(allocation,'policy_capacity'),used=xValue(allocation,'used'),confirmed=xValue(allocation,'confirmed'),provisional=xValue(allocation,'provisional'),reserved=xValue(allocation,'reserved'),remaining=xValue(allocation,'remaining'),usage=capacity?Math.min(100,Math.round(used/capacity*100)):0,deferred=Boolean(allocation.has_deferred_decrease),deferredNote=deferred?'<p class="allocation-notice">'+copy('当前官方周账期仍按旧分配守卫 '+formatNumber(capacity)+'x；已配置的较低额度将在官方重置后生效（当前策略 '+formatNumber(policyCapacity)+'x）。','The current official window keeps its prior '+formatNumber(capacity)+'x allocation. The reduced '+formatNumber(policyCapacity)+'x policy takes effect after the official reset.')+'</p>':'';
    if(title){title.textContent=copy('单 Key 用量分析 · ','Single-Key Usage Analysis · ')+key.name;title.dataset.hasKey='true'}
    detail.innerHTML='<div class="usage-ring" style="--usage:'+usage+'%"><div><strong>'+formatNumber(usage)+'%</strong><small>'+copy('已计量','metered')+'</small></div></div><div class="usage-numbers"><p><small>'+copy('官方确认消耗','Official confirmed usage')+'</small><strong>'+formatNumber(confirmed)+'x</strong></p><p><small>'+copy('待官方确认估算','Provisional estimate')+'</small><strong>'+formatNumber(provisional)+'x</strong></p><p><small>'+copy('请求预留','Request reservations')+'</small><strong>'+formatNumber(reserved)+'x</strong></p><p class="remaining"><small>'+copy('剩余分配','Remaining allocation')+'</small><strong>'+formatNumber(remaining)+'x / '+formatNumber(capacity)+'x</strong></p>'+deferredNote+'</div>';
    const logs=state.logs||[],decisions=logs.filter(log=>!['ignored','expired'].includes(log.decision)),completed=logs.filter(log=>log.decision==='completed'),success=decisions.length?Math.round(completed.length/decisions.length*100):null,trend=state.trend||{};
    metrics.innerHTML='<div><small>'+copy('请求次数','Requests')+'</small><strong>'+formatNumber(trend.request_count||0)+'</strong></div><div><small>'+copy('当前页成功率','Current page success')+'</small><strong class="success">'+(success===null?'—':formatNumber(success)+'%')+'</strong></div><div><small>'+copy('实际 Token','Actual Tokens')+'</small><strong>'+formatTokens(trend.total_tokens||0)+'</strong></div>';
    const allowed=key.allowed_models||[],policy=(state.policies||[]).find(item=>item.id===key.id),items=allowed.slice(0,4),allModels=allowed.length?allowed.join(', '):copy('默认允许全部模型','All models allowed by default');
    models.innerHTML='<small>'+copy('允许模型','Allowed models')+'</small>'+(items.length?items.map(model=>'<div title="'+escape(allModels)+'"><span>'+escape(model)+'</span><b>'+copy('允许','Allowed')+'</b></div>').join(''):'<div title="'+escape(allModels)+'"><span>'+escape(allModels)+'</span><b>'+copy('默认','Default')+'</b></div>')+(allowed.length>items.length?'<div title="'+escape(allModels)+'"><span>'+copy('其余 '+(allowed.length-items.length)+' 个模型','+'+(allowed.length-items.length)+' models')+'</span><b>…</b></div>':'')+'<div class="policy-summary" title="'+escape(policyAccess(policy))+'">'+copy('访问时段：','Access: ')+escape(policyAccess(policy))+'</div>';
    const points=trend.points||[];
    window.__ccRenderUsageLineChart?.(points);
    const mode=trend.granularity==='year'?copy('按年','yearly'):trend.granularity==='month'?copy('按月','monthly'):trend.granularity==='hour'?copy('按小时','hourly'):copy('按日','daily'),coverage=trend.available_from?copy('数据起于 ','Data from ')+new Date(trend.available_from).toLocaleDateString(english()?'en-US':'zh-CN'):copy('暂无已结算历史','No settled history');
    get('analysis-chart-title').textContent=copy('近 ','Last ')+formatNumber(points.length)+copy(' 个区间 Token 用量',' period Token usage');
    get('trend-from').textContent=(trend.timezone||'Asia/Shanghai')+' · '+mode+' · '+coverage;
    get('trend-total').textContent=copy('实际 ','Actual ')+formatTokens(trend.total_tokens||0)+' Token · '+formatNumber(trend.request_count||0)+' '+copy('次请求','requests');
  };
  // Make the approved analysis renderer the single source of truth even for
  // core-only paths such as local search updates.
  window.__ccRenderAnalysis=render;
  const bridge=window.__ccPanelBridge;
  if(bridge){
    const previous=bridge.afterRender?.bind(bridge);
    bridge.afterRender=()=>{previous?.();render()};
  }
  document.addEventListener('DOMContentLoaded',render,{once:true});
})();

// The existing locale hook already observes every rendered fragment. Wrap it
// so the revised global-balance copy remains bilingual without an extra
// observer or a second full-page localization pass.
(()=>{const previous=window.__ccLocalizeAdditional;if(typeof previous!=='function')return;const pairs=[['Key 分配是一份全局 x 余额，不按账号比例切分；账号容量用于共享池总量、官方额度窗口与 Token/x 权重；增加 x 即时生效，降低 x 在当前官方周账期结束后生效','The Key allocation is one global x balance and is not split by account weight. Account capacity governs shared-pool total, official quota windows, and Token/x weighting. Increases take effect immediately; decreases take effect after the current official weekly window.'],['每个 Key 只有一份全局 x 余额，不会按账号比例拆分；实际 Token 按命中账号的官方周窗口结算，账号耗尽时自动切换。','Each Key has one global x balance and is not split by account weight. Actual Tokens settle in the official weekly window of the selected account, and routing switches automatically when an account is exhausted.'],['每个 Key 只有一份全局 x 余额，不按账号容量比例拆分；账号容量用于官方窗口保护与 Token/x 权重。没有可用官方快照时，为避免错误放行会暂时返回 503；其他可用账号仍可正常路由。','Each Key has one global x balance and is not split by account weight. Account capacity protects official windows and weights the Token/x scale. When no current official snapshot is available, routing safely returns 503; other current accounts remain routable.']];const localize=root=>{const english=cpaLocale().startsWith('en'),walk=node=>{if(!node)return;if(node.nodeType===Node.TEXT_NODE){const parent=node.parentElement;if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){let value=node.nodeValue||'';for(const [zh,en] of pairs){const from=english?zh:en,to=english?en:zh;if(from!==to)value=value.split(from).join(to)}node.nodeValue=value}return}if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;for(const child of node.childNodes)walk(child)};walk(root)};window.__ccLocalizeAdditional=root=>{previous(root);localize(root)};window.__ccLocalizeAdditional(document.body)})();

// Extend the existing bilingual pass for the official-percentage x ledger.
// Reusing the current hook avoids a second MutationObserver on the CPA panel.
(()=>{
	const previous=window.__ccLocalizeAdditional;if(typeof previous!=='function')return;
	const pairs=[
		['官方确认用量','Official confirmed usage'],
		['官方确认 x 用量','Official confirmed x usage'],
		['当前计量 x 用量','Current metered x usage'],
		['待官方确认估算','Provisional estimate'],
		['已计量','Metered'],
		['Key 仅配置一个 x 分配；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，下一次官方周百分比更新后自动校正。没有可用官方快照时会安全返回 503。','A Key has one x allocation. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile at the next official weekly percentage update. Without a current official snapshot, routing safely returns 503.'],
		['每个 Key 只有一份全局 x 余额；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，官方周百分比更新后自动校正。账号耗尽时自动切换。','Each Key has one global x balance. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile when the official weekly percentage updates. Routing switches automatically when an account is exhausted.'],
		['Key 分配是一份全局 x 余额；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，官方周百分比更新后自动校正；增加 x 即时生效，降低 x 在当前官方周账期结束后生效','The Key allocation is one global x balance. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile when the official weekly percentage updates. Increases apply immediately; decreases apply after the current official weekly window.']
	];
	const localize=root=>{const english=cpaLocale().startsWith('en'),walk=node=>{if(!node)return;if(node.nodeType===Node.TEXT_NODE){const parent=node.parentElement;if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){let value=node.nodeValue||'';for(const [zh,en] of pairs){const from=english?zh:en,to=english?en:zh;if(from!==to)value=value.split(from).join(to)}node.nodeValue=value}return}if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;for(const name of ['title','placeholder','aria-label'])if(node.hasAttribute(name)){let value=node.getAttribute(name)||'';for(const [zh,en] of pairs){const from=english?zh:en,to=english?en:zh;if(from!==to)value=value.split(from).join(to)}node.setAttribute(name,value)}for(const child of node.childNodes)walk(child)};walk(root)};
	window.__ccLocalizeAdditional=root=>{previous(root);localize(root)};
window.__ccLocalizeAdditional(document.body)
})();

function applyLogCellTitles(body){
	if(!body)return;
	body.querySelectorAll('td').forEach(cell=>{
		const value=(cell.innerText||cell.textContent||'').trim();
		if(value&&value!=='—')cell.title=value;
		else cell.removeAttribute('title');
	});
}

// Keep the final log renderer aligned with the expanded table after legacy
// localization wrappers have run. The bridge is reattached so bootstrap's
// post-render localization sees these final column positions as well.
(()=>{
	const previousDecisionRenderer=renderLogs;
	renderLogs=function(){
		previousDecisionRenderer();
		const rows=Array.from(document.querySelectorAll('#logs tr')),logs=state.logs||[];
		rows.forEach((row,index)=>{
			const log=logs[index],cells=row.children;
			if(!log||cells.length<10)return;
			const status=Number(log.status_code||0),kind=log.decision;
			const decision=kind==='blocked'?uiText('拦截 ','Blocked ')+status:kind==='failed'?uiText('失败','Failed')+(status?' '+status:''):kind==='completed'?uiText('完成 ','Completed ')+tokens(log.units)+' Token':kind==='ignored'?uiText('已忽略','Ignored'):kind==='expired'?uiText('已释放','Released'):uiText('已记录','Recorded');
			const badge=cells[5].querySelector('.decision');
			cells[4].textContent=log.request_content||'—';
			cells[4].title=log.request_content||'';
			if(badge)badge.textContent=decision;
			else cells[5].textContent=decision;
			cells[7].textContent=status||'—';
			cells[7].className='log-http '+(status>=400?'bad':(status>0?'ok':''));
			cells[8].textContent=log.auth_id||'—';
			cells[8].title=log.auth_id||'';
			const description=(cells[9].textContent||log.reason||uiText('策略记录','Policy record')).replace(/\s*·\s*(?:账号|Account)\s*••••.{0,4}$/u,'');
			cells[9].textContent=description;
			cells[9].title=description;
		});
		applyLogCellTitles(document.getElementById('logs'));
	};
	const previousOperationRenderer=renderOperationalLogs;
	renderOperationalLogs=function(){
		previousOperationRenderer();
		const rows=Array.from(document.querySelectorAll('#operation-logs tr')),logs=state.operationLogs||[];
		rows.forEach((row,index)=>{
			const log=logs[index],cells=row.children;
			if(!log||cells.length<5)return;
			cells[3].textContent=log.auth_id||'—';
			cells[3].title=log.auth_id||'';
		});
		applyLogCellTitles(document.getElementById('operation-logs'));
	};
	window.__ccPanelBridge?.attach?.({state,render,renderLogs,renderOperationalLogs,cpaLocale,uiText,tokens,showToast,say});
})();

// Forbidden-phrase management and its dedicated audit tab are isolated from
// quota accounting. The feature is disabled by default and uses only literal,
// case-insensitive phrase matching configured by the plugin-owned API.
(()=>{
	const feature=window.__ccFeature,state=window.state;
	if(!feature||!state)return;
	const {api,keyFor,suffix,suffixTitle,escapeHTML,formatLogTime,num,uiText,say,withModuleLoading,loadingTargets}=feature,$=id=>document.getElementById(id);
	let settings={enabled:false,terms:[]},logs=[],page={page:1,page_size:10,total:0,total_pages:0},requestID=0,searchTimer=0;
	const sourceText=source=>source==='builtin'?uiText('内置','Built-in'):uiText('自定义','Custom');
	const categoryText=value=>({weapons:uiText('武器','Weapons'),drugs:uiText('毒品','Drugs'),sexual_minors:uiText('未成年人性内容','Sexual content involving minors'),fraud:uiText('欺诈','Fraud'),malware:uiText('恶意软件','Malware'),cyber_abuse:uiText('网络滥用','Cyber abuse'),violence:uiText('暴力','Violence'),custom:uiText('自定义','Custom')})[value]||value||uiText('自定义','Custom');
	const closeDialog=()=>{if($('content-filter-dialog')?.open)$('content-filter-dialog').close()};
	function renderTerms(){
		$('content-filter-enabled').checked=Boolean(settings.enabled);
		const terms=settings.terms||[],query=String($('content-filter-search')?.value||'').trim().toLocaleLowerCase(),visible=terms.map((term,index)=>({term,index})).filter(({term})=>!query||[term.value,categoryText(term.category),sourceText(term.source)].some(value=>String(value||'').toLocaleLowerCase().includes(query)));
		$('content-filter-count').textContent=uiText('显示 '+visible.length+' / 共 '+terms.length,'Showing '+visible.length+' / '+terms.length);
		$('content-filter-terms').innerHTML=visible.length?visible.map(({term,index})=>'<div class="content-filter-term" data-term-index="'+index+'"><input type="checkbox" '+(term.enabled?'checked':'')+' aria-label="'+escapeHTML(term.enabled?uiText('停用关键词','Disable phrase'):uiText('启用关键词','Enable phrase'))+'"><code class="content-filter-value" title="'+escapeHTML(term.value)+'">'+escapeHTML(term.value)+'</code><span class="content-filter-category" title="'+escapeHTML(categoryText(term.category))+'">'+escapeHTML(categoryText(term.category))+'</span><span class="content-filter-source">'+escapeHTML(sourceText(term.source))+'</span><span class="content-filter-action">'+(term.source==='custom'?'<button type="button" class="danger content-filter-remove" aria-label="'+escapeHTML(uiText('删除关键词','Delete phrase'))+'">'+escapeHTML(uiText('删除','Delete'))+'</button>':'—')+'</span></div>').join(''):'<div class="content-filter-empty">'+escapeHTML(uiText('没有匹配的关键词。','No matching phrases.'))+'</div>';
		$('content-filter-terms').querySelectorAll('[data-term-index]').forEach(row=>{
			const index=Number(row.dataset.termIndex),term=settings.terms[index];
			row.querySelector('input[type=checkbox]').onchange=event=>{term.enabled=event.target.checked};
			row.querySelector('.content-filter-remove')?.addEventListener('click',()=>{settings.terms.splice(index,1);renderTerms()});
		});
	}
	async function openSettings(){
		const button=$('content-filter-open');if(button)button.disabled=true;
		try{const payload=await api('/content-filter');settings=payload.settings||{enabled:false,terms:[]};$('content-filter-search').value='';renderTerms();$('content-filter-dialog').showModal();window.__ccLocalizeAdditional?.($('content-filter-dialog'))}catch(error){say(error.message)}finally{if(button)button.disabled=false}
	}
	function addTerm(){
		const value=String($('content-filter-value').value||'').trim(),category=String($('content-filter-category').value||'').trim()||'custom';
		if(!value){say(uiText('请输入要添加的关键词。','Enter a phrase to add.'));return}
		if((settings.terms||[]).some(term=>String(term.value||'').trim().toLocaleLowerCase()===value.toLocaleLowerCase())){say(uiText('该关键词已经存在。','That phrase already exists.'));return}
		settings.terms.push({id:'',value,category,source:'custom',enabled:true});$('content-filter-value').value='';$('content-filter-category').value='';renderTerms();
	}
	async function saveSettings(){
		settings.enabled=$('content-filter-enabled').checked;
		const button=$('content-filter-save');button.disabled=true;
		try{const payload=await api('/content-filter',{method:'PUT',headers:{'Content-Type':'application/json'},body:JSON.stringify({settings})});settings=payload.settings||settings;closeDialog();say(uiText('违禁词拦截设置已保存。','Forbidden-phrase filtering settings saved.'),true)}catch(error){say(error.message)}finally{button.disabled=false}
	}
	async function loadLogs(reset=false){
		return withModuleLoading(loadingTargets.forbidden,async()=>{
			const id=++requestID;if(reset)page.page=1;
			const size=Math.max(1,Number($('forbidden-page-size').value)||10),params=new URLSearchParams({page:String(Math.max(1,Number(page.page)||1)),page_size:String(size)}),query=String($('forbidden-search').value||'').trim();if(query)params.set('query',query);
			const payload=await api('/forbidden-logs?'+params);if(id!==requestID)return;logs=payload.logs||[];page={page:Number(payload.page)||1,page_size:Number(payload.page_size)||size,total:Number(payload.total)||0,total_pages:Number(payload.total_pages)||0};renderLogs();
		},uiText('正在加载违禁词拦截日志…','Loading forbidden-phrase logs…'));
	}
	function renderLogs(){
		const total=Math.max(0,Number(page.total)||0),current=Math.max(1,Number(page.page)||1),pages=Math.max(1,Number(page.total_pages)||0),viewText=uiText('查看','View');
		$('forbidden-logs').innerHTML=logs.length?logs.map((log,index)=>{const key=keyFor(log.key_id),keyName=key?.name||'—',suffixSource=text(log.key_suffix)?log:key,keySuffix=suffixSource?suffix(suffixSource):uiText('尾号未知','Suffix unavailable'),request=log.request_content||'—',account=log.auth_id||'—',description=uiText('命中违禁词策略，已在路由前拒绝请求。','Matched the forbidden-phrase policy and was rejected before routing.');return '<tr><td title="'+escapeHTML(formatLogTime(log.requested_at))+'">'+escapeHTML(formatLogTime(log.requested_at))+'</td><td title="'+escapeHTML(keyName)+'">'+escapeHTML(keyName)+'</td><td title="'+escapeHTML(suffixSource?suffixTitle(suffixSource):uiText('CPA Key 尾号不可用','CPA Key suffix unavailable'))+'">'+escapeHTML(keySuffix)+'</td><td title="'+escapeHTML(log.model||'')+'">'+escapeHTML(log.model||'—')+'</td><td title="'+escapeHTML(request)+'">'+escapeHTML(request)+'</td><td title="'+escapeHTML(log.matched_term||'')+'">'+escapeHTML(log.matched_term||'—')+'</td><td title="'+escapeHTML(categoryText(log.matched_category))+'">'+escapeHTML(categoryText(log.matched_category))+'</td><td title="'+escapeHTML(account)+'">'+escapeHTML(account)+'</td><td title="'+escapeHTML(description)+'">'+escapeHTML(description)+'</td><td class="log-action"><button type="button" class="row-action forbidden-log-detail" data-log-index="'+index+'">'+viewText+'</button></td></tr>'}).join(''):'<tr><td colspan="10" class="empty">'+uiText('暂无违禁词拦截日志。','No forbidden-phrase interception logs yet.')+'</td></tr>';
		$('forbidden-page-info').textContent=uiText('共 ','Total ')+num(total)+uiText(' 条 · 每页 ',' · Per page ')+num(page.page_size||10)+uiText(' 条','');$('forbidden-page-number').textContent=uiText('第 ','Page ')+num(current)+' / '+num(pages)+uiText(' 页','');$('forbidden-prev').disabled=current<=1||!total;$('forbidden-next').disabled=!total||current>=Number(page.total_pages||0);window.__ccLocalizeAdditional?.($('forbidden-log-view'));
	}
	function openDetail(log){
		if(!log)return;const key=keyFor(log.key_id),suffixSource=text(log.key_suffix)?log:key,set=(id,value)=>{$(id).textContent=value===undefined||value===null||value===''?'—':String(value)};
		set('log-detail-time',formatLogTime(log.requested_at));set('log-detail-key-name',key?.name);set('log-detail-key-id',suffixSource?suffix(suffixSource):uiText('尾号未知','Suffix unavailable'));set('log-detail-model',log.model);set('log-detail-decision',uiText('违禁词拦截','Forbidden-phrase block'));set('log-detail-token','—');set('log-detail-http',Number(log.status_code)||403);set('log-detail-account',log.auth_id);set('log-detail-reason',log.reason);set('log-detail-matched-term',log.matched_term);set('log-detail-matched-category',categoryText(log.matched_category));set('log-detail-request',log.request_content);set('log-detail-description',uiText('命中违禁词策略，已在模型、访问时段和额度判断前拒绝。','Matched the forbidden-phrase policy and was rejected before model, schedule, and quota checks.'));$('log-detail-dialog').showModal();window.__ccLocalizeAdditional?.($('log-detail-dialog'));
	}
	async function clearLogs(){if(!confirm(uiText('清除全部违禁词拦截日志？其他使用日志、运行日志、额度和策略不会改变。此操作无法撤销。','Clear all forbidden-phrase interception logs? Other usage logs, runtime logs, quota, and policies will not change. This action cannot be undone.')))return;const button=$('forbidden-clear');button.disabled=true;try{await api('/forbidden-logs',{method:'DELETE'});page.page=1;await loadLogs(true);say(uiText('违禁词拦截日志已清除。','Forbidden-phrase interception logs cleared.'),true)}catch(error){say(error.message)}finally{button.disabled=false}}
	$('content-filter-open').onclick=openSettings;$('content-filter-close').onclick=closeDialog;$('content-filter-cancel').onclick=closeDialog;$('content-filter-search').oninput=renderTerms;$('content-filter-add').onclick=addTerm;$('content-filter-save').onclick=saveSettings;$('forbidden-refresh').onclick=()=>loadLogs().catch(error=>say(error.message));$('forbidden-page-size').onchange=()=>loadLogs(true).catch(error=>say(error.message));$('forbidden-search').oninput=()=>{clearTimeout(searchTimer);searchTimer=setTimeout(()=>loadLogs(true).catch(error=>say(error.message)),220)};$('forbidden-prev').onclick=()=>{if(page.page>1){page.page--;loadLogs().catch(error=>say(error.message))}};$('forbidden-next').onclick=()=>{if(page.page<page.total_pages){page.page++;loadLogs().catch(error=>say(error.message))}};$('forbidden-clear').onclick=clearLogs;$('forbidden-logs').onclick=event=>{const button=event.target.closest('.forbidden-log-detail');if(button)openDetail(logs[Number(button.dataset.logIndex)])};
	document.addEventListener('codex-carpool:log-tab-changed',event=>{if(event.detail?.tab==='forbidden')loadLogs().catch(error=>say(error.message))});
})();

// Bilingual labels introduced by request-content logging. Reuse the existing
// localization hook so CPA locale changes do not add another observer.
(()=>{
	const previous=window.__ccLocalizeAdditional;
	if(typeof previous!=='function')return;
	const pairs=[
		['当前 Key 的使用、策略决策、最后一条用户文本（最长 2000 字符）与 Token 结算记录','Usage, policy decisions, the latest user text (up to 2,000 characters), and Token settlements for the selected Key'],
		['搜索模型、请求内容、原因或账号','Search model, request content, reason, or account'],
		['请求内容','Request content'],
		['日志详情','Log details'],
		['CPA Key 尾号','CPA Key suffix'],
		['违禁词拦截','Forbidden-phrase filtering'],
		['违禁词拦截日志','Forbidden-phrase logs'],
		['全部受控 Key 的违禁词命中、请求摘要与拦截结果','Forbidden-phrase matches, request excerpts, and blocks across all managed Keys'],
		['搜索 Key、模型、请求内容或命中词','Search Key, model, request content, or matched phrase'],
		['搜索违禁词日志','Search forbidden-phrase logs'],
		['每页违禁词日志数','Forbidden-phrase logs per page'],
		['开启违禁词拦截','Enable forbidden-phrase filtering'],
		['仅检查已纳管 Key 的用户文本；命中后优先返回 403，未开启时不参与请求链路。','Only user text from managed Keys is checked. Matches return 403 before other policies; when disabled, the filter is not in the request path.'],
		['内置种子仅覆盖明确的高风险短语，用于降低误伤；它不能替代完整的内容审核，主开关默认关闭。','Built-in seeds cover only explicit high-risk phrases to reduce false positives. They do not replace comprehensive moderation, and the master switch is off by default.'],
		['关键词列表','Phrase list'],
		['完整显示实际匹配内容；支持按关键词、分类或来源筛选。','Full matching phrases are shown; filter by phrase, category, or source.'],
		['搜索关键词、分类或来源','Search phrase, category, or source'],
		['状态','Status'],
		['关键词','Phrase'],
		['来源','Source'],
		['操作','Actions'],
		['内置','Built-in'],
		['自定义','Custom'],
		['添加自定义关键词','Add a custom phrase'],
		['分类（可选）','Category (optional)'],
		['添加','Add'],
		['保存设置','Save settings'],
		['命中词','Matched phrase'],
		['分类','Category'],
		['原因代码','Reason code'],
		['查看','View'],
		['账号','Account']
	];
	const localize=root=>{
		const english=cpaLocale().startsWith('en');
		const walk=node=>{
			if(!node)return;
			if(node.nodeType===Node.TEXT_NODE){
				const parent=node.parentElement;
				if(parent&&!['SCRIPT','STYLE','TEXTAREA'].includes(parent.tagName)){
					let value=node.nodeValue||'';
					for(const [zh,en] of pairs)value=value.split(english?zh:en).join(english?en:zh);
					node.nodeValue=value;
				}
				return;
			}
			if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;
			for(const name of ['title','placeholder','aria-label'])if(node.hasAttribute(name)){
				let value=node.getAttribute(name)||'';
				for(const [zh,en] of pairs)value=value.split(english?zh:en).join(english?en:zh);
				node.setAttribute(name,value);
			}
			for(const child of node.childNodes)walk(child);
		};
		walk(root);
	};
	window.__ccLocalizeAdditional=root=>{previous(root);localize(root)};
	window.__ccLocalizeAdditional(document.body);
})();

// Keep every log column on one line and expose its complete rendered value
// through the native hover tooltip. Column resizing is presentation-only and
// persists per browser without changing log rows or backend pagination.
(()=>{
	const minimumColumnWidth=56;
	const previousDecisionRenderer=renderLogs;
	renderLogs=function(){previousDecisionRenderer();applyLogCellTitles(document.getElementById('logs'))};
	const previousOperationRenderer=renderOperationalLogs;
	renderOperationalLogs=function(){previousOperationRenderer();applyLogCellTitles(document.getElementById('operation-logs'))};

	const installResizableColumns=(table,storageKey)=>{
		if(!table||table.dataset.resizableColumns==='true'||table.hidden||table.getBoundingClientRect().width<=0)return;
		const headers=Array.from(table.tHead?.rows?.[0]?.cells||[]);
		if(!headers.length)return;
		const measured=headers.map(header=>Math.max(minimumColumnWidth,Math.round(header.getBoundingClientRect().width)));
		let widths=measured;
		try{
			const saved=JSON.parse(localStorage.getItem(storageKey)||'null');
			if(Array.isArray(saved)&&saved.length===headers.length&&saved.every(value=>Number.isFinite(Number(value))&&Number(value)>=minimumColumnWidth))widths=saved.map(Number);
		}catch{}
		const colgroup=document.createElement('colgroup'),columns=headers.map(()=>document.createElement('col'));
		columns.forEach(column=>colgroup.append(column));
		table.insertBefore(colgroup,table.firstChild);
		const applyWidths=()=>{
			columns.forEach((column,index)=>{column.style.width=widths[index]+'px'});
			const total=Math.round(widths.reduce((sum,width)=>sum+width,0));
			table.style.width=total+'px';
			table.style.minWidth=total+'px';
		};
		const saveWidths=()=>{try{localStorage.setItem(storageKey,JSON.stringify(widths.map(width=>Math.round(width))))}catch{}};
		headers.forEach((header,index)=>{
			header.title=(header.textContent||'').trim();
			const handle=document.createElement('span');
			handle.className='log-column-resizer';
			handle.tabIndex=0;
			handle.setAttribute('role','separator');
			handle.setAttribute('aria-orientation','vertical');
			handle.setAttribute('aria-label',uiText('调整列宽：','Resize column: ')+(header.textContent||'').trim());
			const resizeBy=delta=>{widths[index]=Math.max(minimumColumnWidth,widths[index]+delta);applyWidths()};
			handle.addEventListener('pointerdown',event=>{
				event.preventDefault();
				event.stopPropagation();
				const startX=event.clientX,startWidth=widths[index];
				table.classList.add('is-resizing');
				handle.classList.add('is-dragging');
				handle.setPointerCapture?.(event.pointerId);
				const move=moveEvent=>{widths[index]=Math.max(minimumColumnWidth,startWidth+moveEvent.clientX-startX);applyWidths()};
				const finish=()=>{
					handle.removeEventListener('pointermove',move);
					handle.removeEventListener('pointerup',finish);
					handle.removeEventListener('pointercancel',finish);
					handle.classList.remove('is-dragging');
					table.classList.remove('is-resizing');
					saveWidths();
				};
				handle.addEventListener('pointermove',move);
				handle.addEventListener('pointerup',finish);
				handle.addEventListener('pointercancel',finish);
			});
			handle.addEventListener('keydown',event=>{
				if(event.key!=='ArrowLeft'&&event.key!=='ArrowRight')return;
				event.preventDefault();
				resizeBy(event.key==='ArrowLeft'?-12:12);
				saveWidths();
			});
			header.append(handle);
		});
		table.dataset.resizableColumns='true';
		applyWidths();
	};
	const installVisibleTables=()=>{
		applyLogCellTitles(document.getElementById('logs'));
		applyLogCellTitles(document.getElementById('operation-logs'));
		applyLogCellTitles(document.getElementById('forbidden-logs'));
		installResizableColumns(document.querySelector('.decision-log-table'),'codex-carpool:decision-log-widths:v1');
		installResizableColumns(document.querySelector('.operation-log-table'),'codex-carpool:operation-log-widths:v1');
		installResizableColumns(document.querySelector('.forbidden-log-table'),'codex-carpool:forbidden-log-widths:v1');
	};
	// Log renderers already reapply cell titles after every data refresh. Keep
	// tab changes explicit as well, avoiding another observer per log body.
	document.addEventListener('codex-carpool:log-tab-changed',()=>requestAnimationFrame(installVisibleTables));
	requestAnimationFrame(installVisibleTables);
	window.__ccPanelBridge?.attach?.({state,render,renderLogs,renderOperationalLogs,cpaLocale,uiText,tokens,showToast,say});
})();

(()=>{
  const readTimeoutMs=15000;
  const mutationTimeoutMs=30000;
  const locale=()=>{
    let value='';
    try{
      const root=window.parent.document.documentElement;
      value=String(root.lang||root.dataset.locale||root.dataset.language||'');
    }catch{}
    if(!value)value=navigator.language||'en-US';
    return /^zh\b/i.test(value)?'zh-CN':'en-US';
  };
  const text=(zh,en)=>locale().startsWith('zh')?zh:en;
  // CPA may supply generic panel styles with !important. Keep the account-pool
  // feature card visually stable inside that host theme without changing the
  // source template's startup-critical DOM structure at runtime.

const designStyle=document.createElement('style');

// Palette and responsive layout are now supplied by panel-approved.css. Keep
// this element for backwards-compatible bootstrap ordering, without a second
// competing account-pool theme.
designStyle.textContent='';

document.head.append(designStyle);
  const toast=(message,ok=true)=>{
    if(!document.body||!message)return;
    let region=document.getElementById('toast-region');
    if(!region){
      region=document.createElement('div');
      region.id='toast-region';
      region.setAttribute('aria-live','assertive');
      region.style.cssText='position:fixed;z-index:10000;top:18px;right:18px;display:grid;gap:9px;width:min(420px,calc(100vw - 36px));pointer-events:none';
      document.body.append(region);
    }
    const item=document.createElement('div');
    item.setAttribute('role',ok?'status':'alert');
    item.textContent=message;
    item.style.cssText='pointer-events:auto;padding:12px 14px;border:1px solid '+(ok?'rgba(56,161,105,.32)':'rgba(224,79,95,.35)')+';border-radius:11px;background:'+(ok?'var(--card,#effaf3)':'var(--card,#fff4f5)')+';box-shadow:0 14px 36px rgba(15,23,42,.18);color:'+(ok?'var(--green,#217a4b)':'var(--red,#b42334)')+';font-size:13px;line-height:1.5;cursor:pointer';
    const close=()=>item.remove();
    item.onclick=close;
    region.append(item);
    setTimeout(close,ok?3600:6500);
  };

  // The later localization scripts are independent classic scripts. These
  // temporary fallbacks keep their load order safe; attach() replaces them
  // with the main panel's real closure state before those scripts execute.
  window.state=window.state||{locale:''};
  window.render=window.render||(()=>{});
  window.renderLogs=window.renderLogs||(()=>{});
  window.renderOperationalLogs=window.renderOperationalLogs||(()=>{});
  window.uiText=window.uiText||text;
  window.tokens=window.tokens||(value=>String(Number(value)||0));
  window.cpaLocale=window.cpaLocale||locale;
  window.__ccToast=toast;
  window.__ccPanelBridge={
    attach(panel){
      if(!panel||!panel.state)return;
      panel.state.locale=panel.cpaLocale().startsWith('zh')?'zh':'en';
      window.state=panel.state;
      window.render=panel.render;
      window.renderLogs=panel.renderLogs;
      window.renderOperationalLogs=panel.renderOperationalLogs;
      window.uiText=panel.uiText;
      window.tokens=panel.tokens;
      window.cpaLocale=panel.cpaLocale;
      window.showToast=panel.showToast;
      window.say=panel.say;
    },
    afterRender(){
      window.__ccAfterRender?.();
    }
  };

  const nativeFetch=window.fetch?.bind(window);
  if(nativeFetch&&!window.__ccFetchTimeoutInstalled){
    window.__ccFetchTimeoutInstalled=true;
    window.fetch=async(input,init={})=>{
      if(typeof AbortController==='undefined')return nativeFetch(input,init);
      const options=init||{};
      const url=typeof input==='string'?input:input?.url||'';
      const method=String(options.method||input?.method||'GET').toUpperCase();
      // These routes synchronously wait up to 20 seconds for an official
      // snapshot after saving. Their browser timeout must exceed that server
      // contract, otherwise a completed save can look like a client failure.
      const waitsForOfficialSnapshot=(method==='PUT'||method==='POST')&&/\/codex-carpool\/(?:setup|accounts(?:\/batch|\/refresh)?)(?:\?|$)/.test(url);
      const timeoutMs=waitsForOfficialSnapshot?mutationTimeoutMs:readTimeoutMs;
      const controller=new AbortController();
      // A Request may already carry a caller-owned signal. Preserve it unless
      // this call explicitly supplied a replacement signal in RequestInit.
      const requestSignal=typeof Request!=='undefined'&&input instanceof Request?input.signal:undefined;
      const callerSignal=Object.prototype.hasOwnProperty.call(options,'signal')?options.signal:requestSignal;
      let timedOut=false;
      let detachCaller;
      if(callerSignal){
        if(callerSignal.aborted)controller.abort(callerSignal.reason);
        else{
          detachCaller=()=>controller.abort(callerSignal.reason);
          callerSignal.addEventListener('abort',detachCaller,{once:true});
        }
      }
      const timer=setTimeout(()=>{timedOut=true;controller.abort()},timeoutMs);
      try{
        const response=await nativeFetch(input,{...options,signal:controller.signal});
        if(response.ok&&method==='PUT'&&/\/models(?:\?|$)/.test(url)){
          const manual=window.__ccManualModelSyncActive===true;
          window.__ccManualModelSyncActive=false;
          if(!manual)toast(text('已自动同步 Codex 模型目录。','Codex model catalog synchronized automatically.'));
        }
        return response;
      }catch(error){
        if(timedOut&&!callerSignal?.aborted){
          throw new Error(waitsForOfficialSnapshot
            ?text('等待官方额度同步超时。服务端可能仍在完成保存，请刷新页面确认最终状态后再重试。','Waiting for official quota synchronization timed out. The server may still be completing the save; refresh the page to confirm the final state before retrying.')
            :text('管理请求超时，请检查 CPA 连接后重试。','The management request timed out. Check the CPA connection and retry.'));
        }
        throw error;
      }finally{
        clearTimeout(timer);
        if(detachCaller)callerSignal.removeEventListener('abort',detachCaller);
      }
    };
  }

  const fixQuotaCopy=root=>{
    if(!root)return;
    const replacement=text(
      '每个 Key 只有一份全局 x 余额；取得可信官方校准后，实际 Token 才会折算为有上限的待确认 x，官方周百分比更新后自动校正。没有可用官方快照时会安全返回 503；其他可用账号仍可正常路由。',
      'Each Key has one global x balance. Actual Tokens become a bounded provisional x charge only after a trustworthy official calibration, then reconcile when the official weekly percentage updates. Without a current official snapshot, routing safely returns 503; other current accounts remain routable.'
    );
    const obsolete=[
      'Key 仅配置一个 x 分配；实际 Token 按实际使用账号的官方额度窗口结算与恢复，官方未返回的窗口不会限制。',
      'A Key has one x allocation. Actual Tokens settle and reset by the official window of the account used; unavailable official windows do not restrict it.',
      'Key 仅配置一个 x 分配；实际 Token 按实际使用账号的官方额度窗口结算与恢复。没有可用官方快照时，为避免错误放行会暂时返回 503；其他可用账号仍可正常路由。',
      'A Key has one x allocation. Actual Tokens settle and reset by the official window of the account used. When no current official snapshot is available, routing safely returns 503; other current accounts remain routable.'
    ];
    const rewrite=node=>{
      if(node.nodeType===Node.TEXT_NODE){
        for(const value of obsolete)if(node.nodeValue?.includes(value))node.nodeValue=node.nodeValue.replace(value,replacement);
        return;
      }
      if(node.nodeType!==Node.ELEMENT_NODE||['SCRIPT','STYLE','TEXTAREA'].includes(node.tagName))return;
      for(const child of node.childNodes)rewrite(child);
    };
    rewrite(root);
  };

  const dynamicDecisionReasons={
    access_schedule_closed:['当前时间不在该 Key 的允许访问时段内。','The current time is outside this Key\'s allowed access schedule.'],
    model_not_allowed:['该 Key 不允许使用此模型。','This Key is not allowed to use the requested model.'],
    quota_unavailable:['额度引擎暂不可用。','The quota engine is temporarily unavailable.'],
    quota_account_source_conflict:['共享账号身份正在核验或存在冲突。','Shared account identities are being verified or have a conflict.'],
    quota_persistence_unavailable:['额度账本暂不可用。','The quota ledger is temporarily unavailable.'],
    quota_scheduler_candidates_required:['CPA 未提供可用调度账号候选。','CPA did not provide scheduler account candidates.'],
    quota_pool_unconfigured:['尚未配置共享账号池。','No shared account pool is configured.'],
    quota_snapshot_unavailable:['没有可用的官方额度快照。','No current official quota snapshot is available.'],
    quota_candidate_mismatch:['CPA 调度候选与共享账号池不匹配。','CPA scheduler candidates do not match the shared account pool.'],
    quota_pool_exhausted:['共享账号池官方额度已耗尽。','Official shared-pool allowance is exhausted.'],
    quota_allocation_exhausted:['该 Key 在当前官方账期内的共享池分配已用完。','This Key has exhausted its shared-pool allocation for the current official window.'],
    quota_account_unavailable:['当前没有可用的共享账号。','No shared account is currently available.'],
    quota_persistence_stopping:['额度账本正在停止。','The quota ledger is stopping.'],
    reservation_expired_at_official_reset:['官方周账期刷新，未完成预留已释放。','The official weekly window reset and unfinished reservations were released.'],
    unmatched_usage_callback:['未匹配到待结算请求的用量回调已记录。','An unmatched usage callback was recorded.']
  };
  const dynamicOperationMessages={
    plugin_started:['插件已启动。','Plugin started.'],plugin_stopping:['插件正在停止。','Plugin is stopping.'],plugin_shutdown_conservative:['插件关闭超时，未结算预留已安全保留。','Plugin shutdown timed out; unsettled reservations were safely retained.'],plugin_reconfigured:['CPA 已重新加载插件配置。','CPA reloaded the plugin configuration.'],plugin_panic:['插件调用发生未处理异常。','An unhandled plugin call exception occurred.'],installation_updated:['插件运行设置已更新。','Plugin runtime settings updated.'],management_request_failed:['插件管理请求失败，请查看后方详情。','Plugin management request failed; see the diagnostic detail.'],key_policy_saved:['Key 管理策略已保存。','Key management policy saved.'],key_policy_deleted:['Key 已解除插件管理。','Key removed from plugin management.'],key_usage_reset:['Key 插件用量已重置。','Key plugin usage reset.'],model_catalog_synced:['Codex 模型目录已同步。','Codex model catalog synchronized.'],account_pool_batch_saved:['共享账号池已批量保存。','Shared account pool saved in a batch.'],account_pool_saved:['共享账号池配置已保存。','Shared account-pool configuration saved.'],account_pool_deleted:['账号已从共享池移除。','Account removed from the shared pool.'],quota_refresh_requested:['已请求刷新官方额度。','Official quota refresh requested.'],quota_sync_pending:['账号池已保存，正在等待官方额度快照。','The account pool is saved and waiting for official quota snapshots.'],quota_sync_recovered:['官方额度同步已恢复。','Official quota synchronization recovered.'],quota_sync_failed:['官方额度同步失败，请检查认证目录和账号状态。','Official quota synchronization failed. Check the auth directory and account status.'],quota_sync_auth_file_retry:['CPA 认证文件正在重试读取。','Retrying the CPA auth-file read.'],duplicate_auth_source:['发现重复或无法确认的 CPA 认证身份，受控 Key 已暂停。','Duplicate or unverifiable CPA auth identities were found; managed Keys are paused.'],duplicate_auth_source_resolved:['CPA 认证身份冲突已解除，受控 Key 已恢复。','CPA auth identity conflict resolved; managed Keys resumed.'],official_quota_exhausted:['官方额度已耗尽，账号暂时停止调度。','Official quota is exhausted; the account is temporarily unavailable for scheduling.'],reservation_expired_at_official_reset:['官方周账期刷新，未完成预留已释放。','Official weekly window reset; unfinished reservations were released.'],official_quota_calibration_failed:['官方额度校准未更新。','Official quota calibration was not updated.'],usage_analysis_reader_degraded:['用量分析已降级为共享数据库连接；额度守卫不受影响。','Usage analysis fell back to the shared database connection; quota guarding is unaffected.'],usage_analysis_reader_restored:['用量分析已恢复独立只读数据库连接。','Usage analysis restored its independent read-only database connection.']
  };
  const dynamicTokenText=value=>{
    const amount=Number(value)||0;
    const compact=amount>=1000000?amount/1000000:amount>=1000?amount/1000:amount;
    const suffix=amount>=1000000?'M':amount>=1000?'K':'';
    return new Intl.NumberFormat(locale().startsWith('zh')?'zh-CN':'en-US',{maximumFractionDigits:2}).format(compact)+suffix;
  };
  const enhanceDynamicLogs=()=>{
    const panelState=window.state;
    if(!panelState)return;
    const decisionRows=Array.from(document.querySelectorAll('#logs tr'));
    (panelState.logs||[]).forEach((log,index)=>{
      const cells=decisionRows[index]?.children;
      if(!cells||cells.length<10)return;
      const status=Number(log.status_code||0),kind=log.decision;
      const decision=kind==='blocked'?text('拦截 ','Blocked ')+status:kind==='failed'?text('失败','Failed')+(status?' '+status:''):kind==='completed'?text('完成 ','Completed ')+dynamicTokenText(log.units)+' Token':kind==='ignored'?text('已忽略','Ignored'):kind==='expired'?text('已释放','Released'):text('已记录','Recorded');
      const description=kind==='completed'?text('实际 Token 结算','Actual Token settlement'):kind==='failed'?text('上游请求未完成','Upstream request did not complete'):(dynamicDecisionReasons[log.reason]?text(...dynamicDecisionReasons[log.reason]):log.reason||text('策略记录','Policy record'));
      const badge=cells[5].querySelector('.decision');
      cells[4].textContent=log.request_content||'—';
      cells[4].title=log.request_content||'';
      if(badge)badge.textContent=decision;
      else cells[5].textContent=decision;
      cells[7].textContent=status||'—';
      cells[8].textContent=log.auth_id||'—';
      cells[8].title=log.auth_id||'';
      cells[9].textContent=description;
      cells[9].title=description;
    });
    const operationRows=Array.from(document.querySelectorAll('#operation-logs tr'));
    (panelState.operationLogs||[]).forEach((log,index)=>{
      const cells=operationRows[index]?.children;
      if(!cells||cells.length<5)return;
      const description=dynamicOperationMessages[log.event]?text(...dynamicOperationMessages[log.event]):log.event||text('已记录插件运行事件。','Plugin runtime event recorded.');
      const key=log.key_id?' · Key '+String(log.key_id).slice(-8):'';
      cells[3].textContent=log.auth_id||'—';
      cells[3].title=log.auth_id||'';
      cells[4].textContent=description+key;
    });
  };

  // The existing observer calls each localizer for every nested mutation.
  // Batch roots per localizer so one render produces at most one walk per
  // top-level changed subtree, without replacing the browser API globally.
  const batchLocalizer=name=>{
    const original=window[name];
    if(typeof original!=='function')return;
    let roots=[];
    let scheduled=false;
    window[name]=root=>{
      if(!root)return;
      if(roots.some(item=>item===root||item.contains?.(root)))return;
      for(let index=roots.length-1;index>=0;index--)if(root.contains?.(roots[index]))roots.splice(index,1);
      roots.push(root);
      if(scheduled)return;
      scheduled=true;
      queueMicrotask(()=>{
        scheduled=false;
        const batch=roots;
        roots=[];
        for(const item of batch){
          original(item);
          fixQuotaCopy(item);
        }
      });
    };
  };
  document.addEventListener('DOMContentLoaded',()=>{
    // Keep the policy dialog's inline validation visually distinct after the
    // panel's independently loaded enhancement scripts have initialized it.
    const policyError=document.getElementById('policy-error');
    if(policyError){
      policyError.style.borderRadius='9px';
      policyError.style.background='color-mix(in srgb,var(--red) 8%,transparent)';
    }
    document.getElementById('sync')?.addEventListener('click',()=>{
      window.__ccManualModelSyncActive=true;
    },true);
    fixQuotaCopy(document.body);
    batchLocalizer('__ccLocalizeCore');
    batchLocalizer('__ccLocalizeLogs');
    batchLocalizer('__ccLocalizeAdditional');
    window.__ccAfterRender=enhanceDynamicLogs;
    enhanceDynamicLogs();
  },{once:true});
})();

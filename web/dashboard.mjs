import { resourceBase, readPanelKey, validKey, decodeStored } from './session.mjs';
import { createSettingsEditor } from './settings.mjs';

const $ = id => document.getElementById(id);
const base = resourceBase(location.href);
const endpoint = base + '/v0/management/plugins/codex-quota-scheduler/quota';
const bansEndpoint = base + '/v0/management/plugins/codex-quota-scheduler/bans';
const plans = {team_standard:'Team Standard',team_premium:'Team Premium',plus:'Plus',pro_5x:'Pro 5x',pro_20x:'Pro 20x'};
const upstreamPlans = {team:'Team Standard',self_serve_business_prolite:'Team Premium',plus:'Plus'};
const reasons = {weekly_budget_rebalance:'按周日均预算重新平衡',weekly_remaining_rebalance:'按周余量重新平衡',manual:'手动选择',manual_selection:'手动选择',manual_cleared:'恢复自动调配',initial:'首次选择',active_missing:'当前账号已不可用',quota_exhausted:'额度已用尽'};
const warmupStates = {confirmed:'已确认激活',pending_confirmation:'等待额度确认',attempted:'已尝试',blocked:'已停止重试',failed:'等待重试'};
let state = null, key = '', manual = false, busy = false, timer = null, failures = 0, authEpoch = 0, operating = false;
let storedKey = readStoredKey();
let currentView = 'overview', pendingOperation = null;
const editor = createSettingsEditor({api,notify:feedback,onView:showView,onSaved:async saved=>{if(saved)await refresh(true);}});

function readStoredKey() {
  try { return readPanelKey(localStorage, base, location.host, navigator.userAgent); } catch { return ''; }
}
function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = String(text);
  return node;
}
function timestamp(value) {
  const date = new Date(value);
  return value && Number.isFinite(date.getTime()) && date.getFullYear() > 2000 ? date : null;
}
function dateText(value) {
  const date = timestamp(value);
  return date ? date.toLocaleString('zh-CN', {month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'}) : '尚无记录';
}
function age(value) {
  const date = timestamp(value);
  if (!date) return '尚未查询';
  const mins = Math.max(0, Math.floor((Date.now() - date.getTime()) / 60000));
  return mins < 1 ? '刚刚' : mins < 60 ? mins + ' 分钟前' : Math.floor(mins / 60) + ' 小时前';
}
function resetText(window) {
  if (!window.cycle_started || window.placeholder_reset) return '周期尚未开始';
  const reset = timestamp(window.reset_at);
  if (!reset) return '重置时间未知';
  if (reset <= new Date()) return '已到重置时间，等待查询确认';
  return dateText(window.reset_at) + ' 重置';
}
function pct(value) {
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0,Math.min(100,value)).toFixed(1).replace(/\.0$/,'') + '%' : '未知';
}
function banFor(account) {
  return (state.bans || []).find(b=>b.auth_id === account.auth_id || (account.auth_index && b.auth_id === account.auth_index));
}
function usable(account) { return account.fresh && account.eligible && !banFor(account); }
function feedback(message,kind='error') { $('feedback').textContent=message; $('feedback').dataset.kind=kind; $('feedback').hidden=!message; }
async function api(path,options={}) {
  if(!key)throw new Error('请先连接当前 CPA。');
  const epoch=authEpoch,controller=new AbortController(),timeout=setTimeout(()=>controller.abort(),10000);
  try {
    const response=await fetch(base+'/v0/management/plugins/codex-quota-scheduler'+path,{
      method:options.method||'GET',headers:{Authorization:'Bearer '+key,'Content-Type':'application/json'},
      body:options.body===undefined?undefined:JSON.stringify(options.body),cache:'no-store',redirect:'error',credentials:'omit',signal:controller.signal
    });
    if(epoch!==authEpoch)throw new Error('登录状态已变化，此次操作已停止。');
    if(response.status===401||response.status===403){login('管理密钥未通过验证，请重新连接。');throw new Error('登录失效，请重新连接。');}
    const result=await response.json().catch(()=>({}));
    if(!response.ok){
      const messages={invalid_settings:'设置未保存，请检查标记的参数。',auth_not_active:'账号当前不可用，请刷新后选择。',quota_not_fresh:'账号额度快照已过期，请等待下一轮查询。',auth_quarantined:'账号仍处于冷却或恢复探测中。',auth_not_eligible:'账号额度条件不满足切换要求。',state_persistence_failed:'状态未能写入磁盘，操作未完成。',generation_not_active:'调度器正在切换实例，请稍后再试。'};
      const error=new Error(messages[result.error]||'操作未完成（HTTP '+response.status+'），请检查 CPA 连接或配置。');
      if(result.error==='invalid_settings')error.fields=result.fields;
      if(result.error==='recovery_persistence_failed')error.message='内存状态已更新，但写入状态文件失败，重启后可能恢复旧记录。请检查“连接与高级”中的状态路径和服务器写入权限。';
      throw error;
    }
    return result;
  } catch(error) {
    if(error.name==='AbortError')throw new Error('请求超时。写操作不会自动重复提交，请重新读取核对结果。');
    if(error instanceof TypeError)throw new Error('无法连接 CPA，请检查网络后重试。');
    throw error;
  } finally {clearTimeout(timeout);}
}
async function showView(view) {
  if(!['overview','allocation','warmup','advanced'].includes(view))return;
  currentView=view;
  document.querySelectorAll('[data-panel]').forEach(panel=>panel.hidden=panel.id!=='view-'+view);
  document.querySelectorAll('.view-nav [data-view]').forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.view===view)));
  if(view!=='overview') {
    try {await editor.load();}
    catch(error){$('settings-unavailable').hidden=false;feedback(error.message);}
  }
}
async function mutate(path,method,body,message) {
  if(operating||editor.isSaving())return;
  operating=true;const epoch=authEpoch;
  document.querySelectorAll('.runtime-action').forEach(button=>button.disabled=true);
  try {await api(path,{method,body});if(epoch===authEpoch){feedback(message,'success');await refresh(true);}}
  catch(error){if(epoch===authEpoch){feedback(error.message);await refresh(true);}}
  finally {operating=false;if(state)render();schedule();}
}
function action(label,fn,disabled=false) {
  const button=element('button','runtime-action compact',label);button.type='button';button.disabled=disabled||operating||editor.isSaving();button.addEventListener('click',fn);return button;
}
function confirmOperation(title,description,fn) {
  $('operation-title').textContent=title;$('operation-description').textContent=description;pendingOperation=fn;$('operation-dialog').showModal();
}
function quotaCell(window, weekly) {
  const cell = element('td', weekly ? 'weekly' : '');
  if (!window) { cell.append(element('span','subtext','尚无快照')); return cell; }
  cell.append(element('div','quota-value',pct(window.remaining_percent)));
  const bar = element('div','bar');
  const fill = element('div','bar-fill' + (window.remaining_percent < 15 ? ' low' : ''));
  fill.style.width = typeof window.remaining_percent === 'number' ? Math.max(0,Math.min(100,window.remaining_percent)) + '%' : '0%';
  bar.append(fill);
  cell.append(bar,element('div','subtext',resetText(window)));
  if (window.limit_reached || !window.allowed) cell.append(element('div','subtext error','上游限制中'));
  return cell;
}
function renderAccounts() {
  const body = $('accounts-body'); body.replaceChildren();
  const search = $('search').value.toLowerCase().trim();
  const accounts = [...(state.snapshots || [])].sort((a,b) => Number(b.auth_id === state.serial_active_auth_id) - Number(a.auth_id === state.serial_active_auth_id) || a.auth_id.localeCompare(b.auth_id));
  const visible = accounts.filter(a => [a.auth_id,plans[a.plan_prior]||a.plan_prior,a.upstream_plan_type].some(value=>String(value||'').toLowerCase().includes(search)));
  for (const account of visible) {
    const active = state.scheduler_mode==='serial' && account.auth_id === state.serial_active_auth_id;
    const row = element('tr', active ? 'is-active' : '');
    const info = element('td');
    info.append(element('div','account-id',account.auth_id));
    const meta = element('div','account-meta');
    const ban = banFor(account);
    if (active) meta.append(element('span','badge active','当前'));
    const banLabels = {cooldown:'冷却中',probe_ready:'待恢复探测',half_open:'恢复探测中',probation:'恢复观察中'};
    meta.append(element('span','badge' + (usable(account) ? ' good' : ' warning'),ban ? banLabels[ban.state] || '恢复观察中' : !account.fresh ? (state.quota_probe_on_demand ? '缓存 · 调用时更新' : '待更新') : account.auth_health?.blocked ? '认证受阻' : account.eligible ? '可用' : '暂不可用'));
    const configuredPlan = plans[account.plan_prior] || account.plan_prior || '套餐未知';
    const upstreamPlan = upstreamPlans[account.upstream_plan_type] || account.upstream_plan_type;
    const automatic = ['cpa_usage','cpa_auth_files'].includes(account.plan_source);
    const planLabel = element('span','',account.plan_source==='default' && upstreamPlan ? upstreamPlan : configuredPlan);
    planLabel.title = account.upstream_plan_type ? '上游标签：'+account.upstream_plan_type+' · '+(account.upstream_plan_source==='cpa_usage'?'CPA 额度查询':'CPA 认证信息')+' · '+dateText(account.upstream_plan_observed_at) : '上游尚未返回可识别的套餐标签';
    meta.append(planLabel);
    info.append(meta);
    const capacity = (account.five_hour_capacity_weight_prior || 1)+'×';
    const planStatus = automatic ? '自动识别 · 容量参考 '+capacity : account.plan_source==='account_override' ? '手动覆盖 · 容量参考 '+capacity : '默认档位 '+configuredPlan+' · 容量参考 '+capacity;
    info.append(element('span','subtext',planStatus));
    if (upstreamPlan && !automatic) info.append(element('span','subtext','上游：'+upstreamPlan+(account.upstream_plan_fresh?'':'（缓存已过期）')));
    if (ban) info.append(element('span','subtext','冷却到期 ' + dateText(ban.reset_at)));
    const pollError=state.quota_polls?.[account.auth_id]?.Error || '';
    if(pollError) info.append(element('span','subtext error',/401|403/.test(pollError)?'额度查询认证被上游拒绝，请检查凭据':'额度查询暂不可达，当前余量来自缓存；网络失败不代表额度耗尽'));
    if (account.auth_health) {
      const health=account.auth_health;
      const labels={expiry_conflict:'过期标记冲突 · 等待两次认证确认',token_expired:'凭据已过期 · 需要重新登录或更新',repaired:'已自动恢复过期标记',repair_disabled:'过期标记冲突 · 自动恢复已关闭',verification_failed:'认证验证失败或上游连接异常',credential_changed:'认证文件发生变化 · 等待重新验证',repair_deferred:'等待账号恢复后验证',repair_failed:'恢复未确认 · 稍后重新检查',repeated_expiry:'过期标记再次被写回 · 暂停重复修复',host_unavailable:'CPA 认证检查暂不可用',checked:'未发现过期阻断'};
      info.append(element('span','subtext',labels[health.reason]||health.reason));
      if(timestamp(health.repaired_at)) info.append(element('span','subtext','最近自动恢复 '+dateText(health.repaired_at)));
    }
    if (account.reason && account.reason!=='eligible' && !account.auth_health?.blocked) {
      const labels={not_allowed:'上游暂不可用',limit_reached:'额度已耗尽',serial_threshold:'达到设定阈值',quota_unknown:'等待额度确认'};
      info.append(element('span','subtext',labels[account.reason]||account.reason));
    }
    row.append(info,quotaCell(account.windows?.find(w => w.window === '5h'),false),quotaCell(account.windows?.find(w => w.window === 'weekly'),true));
    const budget = element('td');
    const amount = account.weekly_budget_known && Number.isFinite(account.weekly_budget_percent_per_day) ? account.weekly_budget_percent_per_day.toFixed(1) + '% / 天' : '未知';
    budget.append(element('strong','',amount),element('span','subtext',account.fresh ? '截至下次周重置' : '快照已过期'));
    row.append(budget);
    const balanced=state.balanced_accounts?.[account.auth_id];
    if(state.scheduler_mode==='balanced')budget.append(element('span','subtext','已分配 '+(balanced?.picks||0)+' 次 · 在途估计 '+(balanced?.pending_estimate||0)));
    const poll = state.quota_polls?.[account.auth_id] || state.quota_polls?.[account.auth_index];
    const observed = (account.windows || []).map(w=>w.observed_at).filter(v=>timestamp(v)).sort().at(-1);
    const freshness = element('td');
    freshness.append(element('span','',age(observed)),element('span','subtext',dateText(observed)));
    if (poll?.Error) freshness.append(element('span','subtext error',poll.Error));
    if (poll?.Error && timestamp(poll.NextAt)) freshness.append(element('span','subtext','下次查询 ' + dateText(poll.NextAt)));
    row.append(freshness);
    const operations=element('td','account-actions');
    if(state.scheduler_mode==='serial')operations.append(action(active&&state.serial_manual_selection?'已指定':'设为当前',()=>mutate('/serial-active','PUT',{auth_id:account.auth_id},'已指定当前账号；后续新请求按此选择调度。'),!usable(account)||(active&&state.serial_manual_selection)));
    if(ban)operations.append(action('解除冷却',()=>confirmOperation('解除此账号的冷却','仅解除本地等待，不会重置上游额度。请确认账号已恢复。',()=>mutate('/unban','POST',{auth_id:account.auth_id},'已解除此账号的本地冷却。'))));
    operations.append(action('设置套餐',async()=>{await showView('allocation');$('account-plans')?.scrollIntoView({block:'center'});}));
    row.append(operations);body.append(row);
  }
  $('empty').hidden = visible.length > 0;
  $('empty').textContent = accounts.length ? '没有匹配的账号。' : '尚无额度快照。请在“连接与高级”检查 CPA 管理连接，并确认认证文件已启用。';
}
function renderPolicy() {
  const policy = $('policy'); policy.replaceChildren();
  const noReserve = state.serial_5h_handoff_mode === '429_only';
  const modes = {balanced:'均衡并发 · 会话粘性',serial:'串行调度',legacy:'传统调度',shadow:'观察模式',enforce:'强制调度'};
  const hold = String(state.serial_weekly_rebalance_min_hold || '').replace(/([hm])0s$/,'$1').replace(/(\d+)h/g,'$1 小时 ').replace(/(\d+)m/g,'$1 分钟 ').replace(/(\d+)s/g,'$1 秒').trim();
  const pairs = [
    ['调度模式',modes[state.scheduler_mode] || state.scheduler_mode],
    ['分配策略',state.scheduler_mode==='balanced'?'新会话按 5h 余量、套餐容量和周日均预算分配':state.serial_allocation_policy === 'sustainable' ? '按周日均预算平衡' : '按周剩余比例平衡'],
    ['5h 切换',noReserve ? '额度用尽 / 上游限额时切换' : state.serial_5h_handoff_mode],
    ['5h 预留',noReserve ? '0%（不提前预留）' : pct(state.reserve_5h_percent)],
    [state.scheduler_mode==='balanced'?'并发分配':'主动再平衡最短持有',state.scheduler_mode==='balanced'?'同一会话保持账号，各会话可并发':hold],
    ...(state.scheduler_mode==='balanced' ? [
      ['会话绑定',state.sticky_seconds>0 ? (state.balanced_sticky_bindings||0)+' 个 · 空闲 '+state.sticky_seconds+' 秒后到期' : '已关闭'],
      ['绑定命中 / 不可用换号',(state.balanced_session_hits||0)+' / '+(state.balanced_session_switches||0)],
      ['缺少会话标识请求',(state.balanced_unkeyed_requests||0)+' 次 · 无标识时按单次请求分配']
    ] : [['最近切换',dateText(state.serial_last_switch_at)]])
  ];
  for (const [label,value] of pairs) {
    const row = element('div'); row.append(element('dt','',label),element('dd','',value || '—')); policy.append(row);
  }
}
function renderWarmups() {
  $('warmup-enabled').textContent = state.warmup_enabled ? '已启用' : '已关闭';
  $('warmup-description').textContent = state.warmup_enabled ? '按账号和额度周期记录结果。已确认的周期不再预热，等待确认和失败记录也受冷却约束。' : '当前不发起预热请求；已有记录继续保留。真实请求与额度查询仍可确认周期已开始。';
  const list = $('warmups'); list.replaceChildren();
  for (const warmup of state.warmups || []) {
    const item = element('li');
    const line = element('div','warmup-line');
    const outcomeLabel = warmup.state==='failed' && warmup.dispatch_state==='not_sent' ? '未发送 · 等待重试' : warmup.state==='failed' && warmup.dispatch_state==='uncertain' ? '结果未知 · 暂停重复请求' : warmupStates[warmup.state] || warmup.state;
    line.append(element('span','warmup-id',warmup.auth_id + ' · ' + warmup.window),element('span','badge' + (warmup.blocked ? ' warning' : ''),outcomeLabel));
    item.append(line,element('div','subtext','记录于 ' + dateText(warmup.outcome_at || warmup.activated_at || warmup.completed_at || warmup.attempted_at)));
    if (timestamp(warmup.suppress_until)) item.append(element('div','subtext',(warmup.dispatch_state==='not_sent'?'最早重试时间 ':'抑制重复预热至 ') + dateText(warmup.suppress_until)));
    if (warmup.error) {
      const errors={auth_binding_stale:'CPA 中的账号当前不可用，请求尚未发送。',auth_binding_changed:'CPA 账号绑定已变更，等待最新额度确认。',cpa_inventory_unavailable:'CPA 账号列表暂时不可用，请求尚未发送。',management_key_unavailable:'无法读取 CPA 管理密钥，请求尚未发送。',timeout:'请求超时，等待确认是否已执行。',warmup_failed:'旧版未保存具体原因，需核对 CPA 日志。'};
      item.append(element('div','subtext error',errors[warmup.error]||warmup.error));
    }
    if(warmup.blocked)item.append(action('允许再次重试',()=>mutate('/warmup-retry','POST',{auth_id:warmup.auth_id},'已解除预热重试阻止；仍遵循冷却、间隔和每日预算。')));
    else if(warmup.error)item.append(action('重新安排预热',()=>confirmOperation('重新安排此账号的预热','仅在确认上次请求未执行或原因已修复后继续。仍遵循账号冷却、最小间隔和每日预算。',()=>mutate('/warmup-retry','POST',{auth_id:warmup.auth_id},'已重新安排；下一轮先检查周期是否已激活。'))));
    list.append(item);
  }
  if (!list.children.length) list.append(element('li','subtext','暂无预热记录'));
  $('retry-all').disabled=operating||editor.isSaving()||!(state.warmups||[]).some(w=>w.blocked);
}
function renderBans() {
  const list=$('bans-list');list.replaceChildren();
  for(const ban of state.bans||[]) {
    const item=element('li'),line=element('div','warmup-line');
    line.append(element('span','warmup-id',ban.auth_id),action('解除冷却',()=>confirmOperation('解除此账号的冷却','请确认上游额度已经恢复。本操作只移除本地冷却记录。',()=>mutate('/unban','POST',{auth_id:ban.auth_id},'已解除本地冷却。'))));
    item.append(line,element('p','subtext','到期 '+dateText(ban.reset_at)));list.append(item);
  }
  if(!list.children.length)list.append(element('li','subtext','没有等待恢复的账号'));
  $('unban-all').disabled=operating||editor.isSaving()||!(state.bans||[]).length;
}
function render() {
  const accounts = state.snapshots || [];
  $('content').hidden = false; $('login').hidden = true;
  const balanced=state.scheduler_mode==='balanced';
  $('active-account').textContent = balanced?'多账号按会话承接请求':state.serial_active_auth_id || '等待首个请求选择账号';
  $('selection-mode').textContent = balanced?'均衡并发 · 会话粘性':state.serial_manual_selection ? '手动指定' : '自动调配';
  $('auto-select').hidden=balanced||!state.serial_manual_selection;
  $('auto-select').disabled=operating||editor.isSaving();
  $('active-detail').textContent = state.serial_active_auth_id ? '选中于 ' + dateText(state.serial_selected_at) + (state.serial_last_switch_reason ? ' · ' + (reasons[state.serial_last_switch_reason] || state.serial_last_switch_reason) : '') : '额度查询继续运行，收到请求后按可用额度选择。';
  if(balanced)$('active-detail').textContent='新会话按可用预算均衡分配；同一会话续聊、工具调用和并发请求保持账号，只有额度耗尽或账号不可用时换号。切换后继续绑定替换账号。';
  $('eligible-count').replaceChildren(document.createTextNode(String(accounts.filter(usable).length)),element('small','', ' / ' + accounts.length));
  $('switch-count').textContent = String(state.serial_switches ?? 0);
  $('cooldown-count').textContent = String(state.quarantine?.cooldown ?? 0);
  const observed = (state.snapshots || []).flatMap(a=>a.windows || []).map(w=>w.observed_at).filter(v=>timestamp(v)).sort().at(-1);
  $('updated-at').textContent = '最近额度观测 ' + dateText(observed) + ' · ' + String(state.fresh_snapshots ?? 0) + ' 个新鲜快照' + (state.quota_probe_on_demand ? ' · 按需探测，空闲读取缓存' : ' · 周期探测');
  renderAccounts(); renderPolicy(); renderWarmups();renderBans();
  editor.setAccounts(accounts.map(a=>a.auth_id));
}
function notice(message) { $('notice').textContent = message; $('notice').hidden = !message; }
function login(message) {
  authEpoch++; key = ''; state = null; manual = false;
  editor.reset();$('feedback').hidden=true;$('operation-dialog').close();pendingOperation=null;$('bans-list').replaceChildren();
  $('accounts-body').replaceChildren(); $('warmups').replaceChildren();
  $('active-account').textContent = ''; $('content').hidden = true; $('login').hidden = false;
  showView('overview');
  $('connection').textContent = '等待登录'; $('connection').dataset.state = '';
  notice(message); clearTimeout(timer);
}
function schedule() {
  clearTimeout(timer);
  if (key && $('auto-refresh').checked && !document.hidden) timer = setTimeout(refresh, Math.min(120000,15000 * 2 ** failures));
}
async function refresh(force=false) {
  if (busy || document.hidden || (force!==true && (operating||editor.isSaving()))) return;
  clearTimeout(timer);
  if (!manual) {
    storedKey = readStoredKey(); key = storedKey;
    if (!key) { login(''); return; }
  }
  if (!key) { login(''); return; }
  const epoch = authEpoch;
  busy = true; $('refresh').disabled = true;
  const controller = new AbortController();
  const timeout = setTimeout(()=>controller.abort(),10000);
  try {
    const options = {headers:{Authorization:'Bearer ' + key},cache:'no-store',redirect:'error',credentials:'omit',signal:controller.signal};
    const [response,bansResponse] = await Promise.all([fetch(endpoint,options),fetch(bansEndpoint,options)]);
    if (epoch !== authEpoch) return;
    if ([response.status,bansResponse.status].some(status=>status === 401 || status === 403)) { login('管理密钥未通过验证，请重新连接当前 CPA。'); return; }
    if (!response.ok) throw new Error('额度接口返回 HTTP ' + response.status + '。请检查 CPA 插件是否已启用。');
    if (!bansResponse.ok) throw new Error('冷却状态接口返回 HTTP ' + bansResponse.status + '。请检查 CPA 插件是否已启用。');
    const [result,bansResult] = await Promise.all([response.json(),bansResponse.json()]);
    if (epoch !== authEpoch) return;
    if (typeof result.enabled !== 'boolean' || (result.snapshots != null && !Array.isArray(result.snapshots)) || !Array.isArray(bansResult.bans)) throw new Error('额度接口返回格式不匹配，请检查当前 CPA 的插件版本。');
    result.bans = bansResult.bans;
    state = result; failures = 0; render();
    $('connection').textContent = '已连接 CPA'; $('connection').dataset.state = 'ok';
    $('last-read').textContent = '面板更新 ' + new Date().toLocaleTimeString('zh-CN');
    const warnings = [];
    if (!state.enabled) warnings.push('调度器已关闭。');
    if (state.generation_managed && !state.generation_active) warnings.push('当前插件实例未取得运行权，请检查 CPA 插件加载状态。');
    if (state.last_error || state.quota_refresh_error) warnings.push('额度查询异常：' + (state.quota_refresh_error || state.last_error));
    notice(warnings.join(' '));
  } catch (error) {
    if (epoch !== authEpoch) return;
    failures = Math.min(failures + 1,3);
    $('connection').textContent = '连接异常'; $('connection').dataset.state = '';
    notice((error.name === 'AbortError' ? '读取超时，请检查 CPA 连接。' : error instanceof TypeError ? '无法读取额度，请检查网络与 CPA 服务。' : error.message) + (state ? ' 下方保留上次快照，数据尚未更新。' : ''));
  } finally { clearTimeout(timeout); busy = false; $('refresh').disabled = false; schedule(); }
}
function syncTheme() {
  document.documentElement.dataset.embedded=String(parent!==window);
  let theme = '';
  try { if (parent !== window) theme = parent.document.documentElement.getAttribute('data-theme'); } catch { /* A remote panel cannot supply a theme. */ }
  if (!theme) {
    try { const raw = decodeStored(localStorage.getItem('cli-proxy-theme'),location.host,navigator.userAgent); theme = raw?.state?.theme || raw?.theme; } catch { /* Storage is optional. */ }
  }
  document.documentElement.dataset.theme = theme === 'dark' || ((!theme || theme === 'system' || theme === 'auto') && matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'white';
}
$('refresh').addEventListener('click',refresh);
document.querySelectorAll('[data-view]').forEach(button=>button.addEventListener('click',()=>showView(button.dataset.view)));
$('auto-select').addEventListener('click',()=>mutate('/serial-active','DELETE',undefined,'已恢复自动调配，后续请求重新选择账号。'));
$('unban-all').addEventListener('click',()=>confirmOperation('解除全部冷却','将移除所有本地冷却记录。仅在确认相关账号的上游额度已恢复后执行。',()=>mutate('/unban-all','POST',{},'已解除全部本地冷却。')));
$('retry-all').addEventListener('click',()=>confirmOperation('恢复已阻止的预热重试','仅清除因失败而被阻止的预热记录。已确认周期、等待间隔和每日预算继续保留。',()=>mutate('/warmup-retry','POST',{all:true},'已恢复被阻止的预热重试。')));
$('cancel-operation').addEventListener('click',()=>{$('operation-dialog').close();pendingOperation=null;});
$('confirm-operation').addEventListener('click',()=>{const fn=pendingOperation;pendingOperation=null;$('operation-dialog').close();fn?.();});
$('search').addEventListener('input',()=>{if(state) renderAccounts();});
$('auto-refresh').addEventListener('change',schedule);
$('login').addEventListener('submit',event=>{
  event.preventDefault();
  const candidate = validKey($('management-key').value);
  if (!candidate) { notice('请输入有效的管理密钥。'); return; }
  authEpoch++; key = candidate; manual = true; $('management-key').value = ''; refresh();
});
window.addEventListener('storage',event=>{
  syncTheme();
  if (!event.key || /^(cli-proxy-auth|managementKey|apiBase|apiUrl|isLoggedIn)/.test(event.key)) {
    const next = readStoredKey();
    if (next !== storedKey || event.key === null || (event.key === 'isLoggedIn' && event.newValue !== 'true')) {
      storedKey = next; login('CPA 登录信息已变化，请重新连接。');
    }
  }
});
document.addEventListener('visibilitychange',()=>{clearTimeout(timer); if(!document.hidden && key) refresh();});
matchMedia('(prefers-color-scheme: dark)').addEventListener('change',syncTheme);
try { if(parent !== window) new MutationObserver(syncTheme).observe(parent.document.documentElement,{attributes:true,attributeFilter:['data-theme']}); } catch { /* Same-origin embedding is optional. */ }
syncTheme(); key = storedKey; refresh();

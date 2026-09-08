import { resourceBase, readPanelKey, validKey, decodeStored } from './session.mjs';

const $ = id => document.getElementById(id);
const base = resourceBase(location.href);
const endpoint = base + '/v0/management/plugins/codex-quota-scheduler/quota';
const bansEndpoint = base + '/v0/management/plugins/codex-quota-scheduler/bans';
const plans = {team_standard:'Team Standard',team_premium:'Team Premium',plus:'Plus',pro_5x:'Pro 5x',pro_20x:'Pro 20x'};
const reasons = {weekly_budget_rebalance:'按周日均预算重新平衡',weekly_remaining_rebalance:'按周余量重新平衡',manual:'手动选择',initial:'首次选择',active_missing:'当前账号已不可用',quota_exhausted:'额度已用尽'};
const warmupStates = {confirmed:'已确认激活',pending_confirmation:'等待额度确认',attempted:'已尝试',blocked:'已停止重试',failed:'等待重试'};
let state = null, key = '', manual = false, busy = false, timer = null, failures = 0, authEpoch = 0;
let storedKey = readStoredKey();

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
  const visible = accounts.filter(a => a.auth_id.toLowerCase().includes(search) || (plans[a.plan_prior] || a.plan_prior || '').toLowerCase().includes(search));
  for (const account of visible) {
    const active = account.auth_id === state.serial_active_auth_id;
    const row = element('tr', active ? 'is-active' : '');
    const info = element('td');
    info.append(element('div','account-id',account.auth_id));
    const meta = element('div','account-meta');
    const ban = banFor(account);
    if (active) meta.append(element('span','badge active','当前'));
    const banLabels = {cooldown:'冷却中',probe_ready:'待恢复探测',half_open:'恢复探测中',probation:'恢复观察中'};
    meta.append(element('span','badge' + (usable(account) ? ' good' : ' warning'),ban ? banLabels[ban.state] || '恢复观察中' : !account.fresh ? '待更新' : account.eligible ? '可用' : '暂不可用'));
    meta.append(element('span','',plans[account.plan_prior] || account.plan_prior || '套餐未知'));
    info.append(meta);
    if (ban) info.append(element('span','subtext','冷却到期 ' + dateText(ban.reset_at)));
    if (account.reason) info.append(element('span','subtext',account.reason));
    row.append(info,quotaCell(account.windows?.find(w => w.window === '5h'),false),quotaCell(account.windows?.find(w => w.window === 'weekly'),true));
    const budget = element('td');
    const amount = account.weekly_budget_known && Number.isFinite(account.weekly_budget_percent_per_day) ? account.weekly_budget_percent_per_day.toFixed(1) + '% / 天' : '未知';
    budget.append(element('strong','',amount),element('span','subtext',account.fresh ? '截至下次周重置' : '快照已过期'));
    row.append(budget);
    const poll = state.quota_polls?.[account.auth_id] || state.quota_polls?.[account.auth_index];
    const observed = (account.windows || []).map(w=>w.observed_at).filter(v=>timestamp(v)).sort().at(-1);
    const freshness = element('td');
    freshness.append(element('span','',age(observed)),element('span','subtext',dateText(observed)));
    if (poll?.Error) freshness.append(element('span','subtext error',poll.Error));
    if (poll?.Error && timestamp(poll.NextAt)) freshness.append(element('span','subtext','下次查询 ' + dateText(poll.NextAt)));
    row.append(freshness); body.append(row);
  }
  $('empty').hidden = visible.length > 0;
  $('empty').textContent = accounts.length ? '没有匹配的账号。' : '尚无额度快照。请检查插件配置中的 CPA 管理连接，以及认证文件是否已启用。';
}
function renderPolicy() {
  const policy = $('policy'); policy.replaceChildren();
  const noReserve = state.serial_5h_handoff_mode === '429_only';
  const modes = {serial:'串行调度',legacy:'传统调度',shadow:'观察模式',enforce:'强制调度'};
  const hold = String(state.serial_weekly_rebalance_min_hold || '').replace(/([hm])0s$/,'$1').replace(/(\d+)h/g,'$1 小时 ').replace(/(\d+)m/g,'$1 分钟 ').replace(/(\d+)s/g,'$1 秒').trim();
  const pairs = [
    ['调度模式',modes[state.scheduler_mode] || state.scheduler_mode],
    ['分配策略',state.serial_allocation_policy === 'sustainable' ? '按周日均预算平衡' : '按周剩余比例平衡'],
    ['5h 切换',noReserve ? '额度用尽 / 上游限额时切换' : state.serial_5h_handoff_mode],
    ['5h 预留',noReserve ? '0%（不提前预留）' : pct(state.reserve_5h_percent)],
    ['主动再平衡最短持有',hold],
    ['最近切换',dateText(state.serial_last_switch_at)]
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
    line.append(element('span','warmup-id',warmup.auth_id + ' · ' + warmup.window),element('span','badge' + (warmup.blocked ? ' warning' : ''),warmupStates[warmup.state] || warmup.state));
    item.append(line,element('div','subtext','记录于 ' + dateText(warmup.outcome_at || warmup.activated_at || warmup.completed_at || warmup.attempted_at)));
    if (timestamp(warmup.suppress_until)) item.append(element('div','subtext','抑制重复预热至 ' + dateText(warmup.suppress_until)));
    if (warmup.error) item.append(element('div','subtext error',warmup.error));
    list.append(item);
  }
  if (!list.children.length) list.append(element('li','subtext','暂无预热记录'));
}
function render() {
  const accounts = state.snapshots || [];
  $('content').hidden = false; $('login').hidden = true;
  $('active-account').textContent = state.serial_active_auth_id || '等待首个请求选择账号';
  $('selection-mode').textContent = state.serial_manual_selection ? '手动指定' : '自动调配';
  $('active-detail').textContent = state.serial_active_auth_id ? '选中于 ' + dateText(state.serial_selected_at) + (state.serial_last_switch_reason ? ' · ' + (reasons[state.serial_last_switch_reason] || state.serial_last_switch_reason) : '') : '额度查询继续运行，收到请求后按可用额度选择。';
  $('eligible-count').replaceChildren(document.createTextNode(String(accounts.filter(usable).length)),element('small','', ' / ' + accounts.length));
  $('switch-count').textContent = String(state.serial_switches ?? 0);
  $('cooldown-count').textContent = String(state.quarantine?.cooldown ?? 0);
  $('updated-at').textContent = '最近额度查询 ' + dateText(state.last_refresh) + ' · ' + String(state.fresh_snapshots ?? 0) + ' 个新鲜快照';
  renderAccounts(); renderPolicy(); renderWarmups();
}
function notice(message) { $('notice').textContent = message; $('notice').hidden = !message; }
function login(message) {
  authEpoch++; key = ''; state = null; manual = false;
  $('accounts-body').replaceChildren(); $('warmups').replaceChildren();
  $('active-account').textContent = ''; $('content').hidden = true; $('login').hidden = false;
  $('connection').textContent = '等待登录'; $('connection').dataset.state = '';
  notice(message); clearTimeout(timer);
}
function schedule() {
  clearTimeout(timer);
  if (key && $('auto-refresh').checked && !document.hidden) timer = setTimeout(refresh, Math.min(120000,15000 * 2 ** failures));
}
async function refresh() {
  if (busy || document.hidden) return;
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
  let theme = '';
  try { if (parent !== window) theme = parent.document.documentElement.getAttribute('data-theme'); } catch { /* A remote panel cannot supply a theme. */ }
  if (!theme) {
    try { const raw = decodeStored(localStorage.getItem('cli-proxy-theme'),location.host,navigator.userAgent); theme = raw?.state?.theme || raw?.theme; } catch { /* Storage is optional. */ }
  }
  document.documentElement.dataset.theme = theme === 'dark' || ((!theme || theme === 'system' || theme === 'auto') && matchMedia('(prefers-color-scheme: dark)').matches) ? 'dark' : 'white';
}
$('refresh').addEventListener('click',refresh);
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

export const poolWeeklyDefaults = Object.freeze({team_standard:1,team_premium:5,plus:1,pro_5x:5,pro_20x:10});
export const poolWeeklyLabels = Object.freeze({team_standard:'普通 Team',team_premium:'Team 5x',plus:'Plus',pro_5x:'Pro 5x',pro_20x:'Pro 20x'});
const number = value => typeof value==='number' && Number.isFinite(value);
export const poolNumber = value => number(value) ? value.toLocaleString('zh-CN',{maximumFractionDigits:2}) : '—';
export function poolWeeklyView(pool) {
  if(!pool || !(pool.total_capacity>0))return {text:'—',used:'等待额度',fill:0,complete:false,detail:'等待账号池周额度快照。'};
  const complete=pool.complete===true&&number(pool.remaining_percent);
  const lower=Math.max(0,Math.min(100,pool.minimum_percent||0)),upper=Math.max(lower,Math.min(100,pool.maximum_percent||0));
  return {
    text:complete?poolNumber(pool.remaining_percent)+'%':poolNumber(lower)+'% – '+poolNumber(upper)+'%',
    used:complete?'已用 '+poolNumber(100-pool.remaining_percent)+'%':'部分额度待确认',
    fill:complete?Math.max(0,Math.min(100,pool.remaining_percent)):lower,
    complete,
    detail:'全池 '+pool.accounts+' 个账号 · 总容量 '+poolNumber(pool.total_capacity)+' Team · 已观测剩余 '+poolNumber(pool.remaining_capacity)+' Team'+(pool.cached_accounts?' · '+pool.cached_accounts+' 个账号使用缓存估算':'')+(pool.unknown_accounts?' · '+pool.unknown_accounts+' 个账号周额度待确认':'')
  };
}

export function poolWindowView(pool, kind='weekly') {
  const five=kind==='5h',label=five?'5h':'周';
  if(!pool || !(pool.total_capacity>0)) {
    const pending=five&&pool?.unclassified_accounts>0;
    const inapplicable=five&&pool?.inventory_accounts>0&&!pending;
    return {text:pending?'待确认':inapplicable?'不适用':'—',used:inapplicable?'当前账号均未纳入 5h 统计':'等待额度',fill:0,complete:false,detail:pending?'尚未确认有 5h 限制的账号，缺失数据不等于无限额。':inapplicable?'无 5h 限制或已手动排除的账号不参与分子、分母。':'等待账号池'+label+'额度快照。',balance:'—',total:'—',coverage:'—',quality:pending?'范围待确认':inapplicable?'无需统计':'等待观测',scope:five?poolScope(pool):'全账号池',nextReset:null,caption:five?'有 5h 限制的账号':'各账号当前周窗口'};
  }
  const view=poolWeeklyView(pool);
  if(five)view.detail=view.detail.replace('全池 ','有 5h 限制的 ').replace('个账号周额度待确认','个账号 5h 额度待确认');
  return {...view,
    balance:poolNumber(pool.remaining_capacity)+' Team',total:poolNumber(pool.total_capacity)+' Team',
    coverage:poolNumber(pool.coverage_percent??(pool.complete?100:0))+'%',
    quality:five&&pool.unclassified_accounts?'范围待确认':!view.complete?'额度待确认':pool.cached_accounts?'含缓存估算':'观测完整',
    scope:five?poolScope(pool):'全池 '+pool.accounts+' 个账号',
    caption:five&&pool.unclassified_accounts?'已确认有 5h 限制的账号':five?'当前 5h 窗口剩余':'各账号当前周窗口',
    nextReset:pool.next_reset_at||null
  };
}
function poolScope(pool) {
  if(!pool)return '仅统计有 5h 限制的账号';
  return '纳入 '+(pool.accounts||0)+' 个'+(pool.no_limit_reported_accounts?' · 未返回限制 '+pool.no_limit_reported_accounts+' 个':'')+(pool.excluded_accounts?' · 手动排除 '+pool.excluded_accounts+' 个':'')+(pool.unclassified_accounts?' · 范围待确认 '+pool.unclassified_accounts+' 个':'');
}

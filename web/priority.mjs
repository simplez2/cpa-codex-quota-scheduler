export function quotaPriorityText(account) {
  const p = account?.selection_priority;
  if (!p) return '优先级待确认';
  const labels = {
    five_hour_reset_first:'5h 优先 · 临近重置先用',
    five_hour_dormant:'5h 后续 · 周期尚未启动',
    five_hour_reset_unknown:'5h 优先 · 重置时间待确认',
    weekly_only_backup:'仅周限 · 后备额度',
    five_hour_unconfirmed:'周额度可见 · 5h 待确认',
    quota_unknown:'额度待确认',
    serial_threshold:'已达切号阈值',
    limit_reached:'额度已耗尽',
    not_allowed:'上游限制中'
  };
  return (labels[p.reason] || '优先级待确认') + (p.cached ? ' · 缓存估算' : '');
}

export function resetCreditsText(account) {
  if (account?.reset_credits_known !== true || !Number.isSafeInteger(account.reset_credits) || account.reset_credits < 0) return '官方可用重置次数：未返回';
  return '官方可用重置次数：' + account.reset_credits + (account.reset_credits_fresh === false ? '（缓存）' : '');
}

function resetCreditDate(value) {
  const n=Date.parse(value);
  return Number.isFinite(n)?new Intl.DateTimeFormat('zh-CN',{timeZone:'Asia/Singapore',year:'numeric',month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',second:'2-digit',hourCycle:'h23'}).format(n):'';
}

const resetCreditAutoReasons={
  details_unconfirmed:'等待到期明细',details_incomplete:'明细不完整',details_changed:'用卡明细已变化，等待重新检查',scheduled:'等待临近到期',no_available_credits:'没有未过期的卡',
  due:'正在核验',plan_unsupported:'套餐支持情况待确认',too_late:'已过安全操作时间',not_applicable:'用卡条件待重新核验',
  quota_unused:'额度尚未消耗',identity_unconfirmed:'凭据身份待确认',quota_not_confirmed:'额度查询未确认',
  previous_attempt_requires_review:'前次结果待人工核查，已停止自动消耗',verification_pending:'官方已确认，等待明细核验',
  used:'已使用并核验',recovery_pending:'用卡已确认，额度或路由恢复仍待核验',dispatch_canceled:'操作取消，需核查记录',adapter_unsupported:'自定义额度接口不支持自动用卡',
  durable_state_required:'需配置持久化状态目录',reset_credit_journal_invalid:'用卡记录异常，已停止自动消耗',
  reset_credit_journal_unavailable:'无法保存用卡记录，已停止自动消耗',reset_credit_journal_full:'用卡记录已满，已停止自动消耗',
  generation_inactive:'插件正在切换',request_id_unavailable:'无法生成操作编号'
};

function resetCreditNotices(d) {
  const lines=[];
  if(d.credits?.length>10)lines.push('另有 '+(d.credits.length-10)+' 张，请查看官方明细。');
  if(d.error)lines.push('到期明细查询失败，稍后重试；缓存时间不作为自动用卡依据。');
  else if(d.count>0 && !d.complete)lines.push('部分到期明细未返回，自动用卡暂停。');
  return lines;
}

function resetCreditLastAction(d) {
  if(d.last_action==='verified')return '最近自动用卡：'+resetCreditDate(d.action_at)+'（GMT+8，已核验）';
  if(['pending','unknown','confirmed'].includes(d.last_action))return '最近自动用卡：'+resetCreditDate(d.action_at)+'（GMT+8）· '+(d.last_action==='confirmed'?'官方确认，明细待核验':'结果待核查，不自动重试');
  return '';
}

export function resetCreditDetails(account, now=Date.now()) {
  const d=account?.reset_credit_details;
  if(!d)return [];
  const lines=[];
  if(d.supported_available_known===true && Number.isSafeInteger(d.supported_available_count) && d.supported_available_count>=0) {
    lines.push('当前套餐可用重置卡：'+d.supported_available_count+' 张'+(d.supported_available_fresh?'':'（缓存，使用前重新核验）'));
  }
  for(const [i,c] of (Array.isArray(d.credits)?d.credits:[]).slice(0,10).entries()) {
    const expires=resetCreditDate(c.expires_at);
    if(!expires)continue;
    const expired=Date.parse(c.expires_at)<=now;
    lines.push('第 '+(i+1)+' 次到期：'+expires+'（GMT+8）'+(expired?' · 已过期':d.list_fresh?'':' · 缓存待核验'));
  }
  lines.push(...resetCreditNotices(d));
  lines.push(d.auto_enabled?'到期自动用卡：已开启 · 提前 '+d.auto_before+' · '+(resetCreditAutoReasons[d.auto_reason]||'等待检查'):'到期自动用卡：已关闭');
  const lastAction=resetCreditLastAction(d);
  if(lastAction)lines.push(lastAction);
  return lines;
}

// Presentation only: counts still come from the backend's independent official
// total and supported-card observations. A missing value must remain unknown.
export function resetCreditView(account, now=Date.now()) {
  const d=account?.reset_credit_details;
  const count=value=>Number.isSafeInteger(value)&&value>=0?value:null;
  const official=account?.reset_credits_known===true?count(account.reset_credits):null;
  const supported=d?.supported_available_known===true?count(d.supported_available_count):null;
  const credits=(Array.isArray(d?.credits)?d.credits:[]).map((credit,index)=>({
    ordinal:index+1,
    expiresAt:credit.expires_at,
    expires:Date.parse(credit.expires_at),
    supported:credit.supported,expired:Date.parse(credit.expires_at)<=now,
    text:resetCreditDate(credit.expires_at),
  })).filter(credit=>Number.isFinite(credit.expires));
  const next=credits.filter(credit=>credit.expires>now).sort((a,b)=>a.expires-b.expires)[0];
  const autoBefore=typeof d?.auto_before==='string'?d.auto_before.replace(/^(\d+)h0m0s$/,'$1 小时').replace(/^(\d+)m0s$/,'$1 分钟').replace(/^(\d+)s$/,'$1 秒'):'';
  const notes=d?resetCreditNotices(d):[];
  const lastAction=d?resetCreditLastAction(d):'';
  if(lastAction)notes.push(lastAction);
  return {
    official,officialCached:official!==null&&account.reset_credits_fresh===false,
    supported,supportedCached:supported!==null&&d.supported_available_fresh!==true,
    nextExpiry:next?.expiresAt||'',nextExpiryText:next?resetCreditDate(next.expiresAt).slice(0,-3):'',listFresh:d?.list_fresh===true,
    credits:credits.slice(0,10),notes,
    automatic:d?d.auto_enabled===true:null,
    autoBefore,automaticText:d?.auto_enabled?(resetCreditAutoReasons[d.auto_reason]||'等待检查'):'',
  };
}

export function observedResetsText(phase) {
  const count = phase?.observed_reset_count;
  if (!Number.isSafeInteger(count) || count < 0) return '本地重置观测待确认';
  return '当前周本地已观察 5h 重置 ' + count + ' 次';
}

export function compareAccountPriorities(a, b) {
  const rank = account => Number.isSafeInteger(account?.selection_priority?.rank) && account.selection_priority.rank > 0 ? account.selection_priority.rank : Number.MAX_SAFE_INTEGER;
  return rank(a) - rank(b) || a.auth_id.localeCompare(b.auth_id);
}

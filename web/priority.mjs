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

export function observedResetsText(phase) {
  const count = phase?.observed_reset_count;
  if (!Number.isSafeInteger(count) || count < 0) return '本地重置观测待确认';
  return '当前周本地已观察 5h 重置 ' + count + ' 次';
}

export function compareAccountPriorities(a, b) {
  const rank = account => Number.isSafeInteger(account?.selection_priority?.rank) && account.selection_priority.rank > 0 ? account.selection_priority.rank : Number.MAX_SAFE_INTEGER;
  return rank(a) - rank(b) || a.auth_id.localeCompare(b.auth_id);
}

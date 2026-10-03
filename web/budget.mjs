// Rates describe each credential's own weekly quota, independent of plan size.
export function weeklyBudgetPercentPerMinute(account) {
  if (!account?.weekly_budget_known) return null;
  const minute = account.weekly_budget_percent_per_minute;
  if (minute !== undefined) return Number.isFinite(minute) && minute >= 0 ? minute : null;
  const daily = account.weekly_budget_percent_per_day;
  return Number.isFinite(daily) && daily >= 0 ? daily / 1440 : null;
}

export function formatWeeklyBudgetRate(rate) {
  if (!Number.isFinite(rate) || rate < 0) return '未知';
  return rate.toLocaleString('zh-CN', {maximumSignificantDigits: 6}) + '% / 分钟';
}

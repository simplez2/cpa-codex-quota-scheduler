// Rates describe each credential's own weekly quota, independent of plan size.
export function weeklyBudgetPercentPerMinute(account) {
  if (!account?.weekly_budget_known) return null;
  const minute = account.weekly_budget_percent_per_minute;
  if (minute !== undefined) return Number.isFinite(minute) && minute >= 0 ? minute : null;
  const daily = account.weekly_budget_percent_per_day;
  return Number.isFinite(daily) && daily >= 0 ? daily / 1440 : null;
}

// Keep minute-resolution scheduling telemetry; present its daily equivalent.
export function formatWeeklyBudgetRate(ratePerMinute) {
  if (!Number.isFinite(ratePerMinute) || ratePerMinute < 0) return '未知';
  const daily = ratePerMinute * 1440;
  if (!Number.isFinite(daily)) return '未知';
  return daily.toLocaleString('zh-CN', {maximumSignificantDigits: 6}) + '% / 天';
}

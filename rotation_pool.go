package main

import (
	"math"
	"sort"
	"time"
)

// Simulate constant forecast demand between confirmed reset events. Quota
// carries across empty time buckets, expires at its own reset, and every refill
// is capped by the same remaining weekly reservoir. This is diagnostic only.
func adqRecoveryMargin(accounts []adqAccountMetrics, demand float64, horizon time.Duration, now time.Time) float64 {
	if !finiteADQ(demand) || demand <= 0 || horizon <= 0 {
		return math.NaN()
	}
	type reservoir struct {
		remaining, weekly, capacity float64
		reset                       time.Time
		weeklyOnly                  bool
	}
	end := now.Add(horizon)
	items := make([]reservoir, 0, len(accounts))
	events := []time.Time{end}
	for _, account := range accounts {
		if account.State == "UNHEALTHY" || account.State == "AUTH_FAILED" ||
			(account.ProviderState == adqProviderOverload && now.Before(account.ProviderRetryAt)) {
			continue
		}
		remaining := math.Min(account.HEffective, account.WEffective)
		if account.WeeklyOnly {
			remaining = account.WEffective
		}
		items = append(items, reservoir{remaining: remaining, weekly: account.WEffective,
			capacity: account.H, reset: account.Next5hReset, weeklyOnly: account.WeeklyOnly})
		if !account.WeeklyOnly && account.Next5hReset.After(now) && account.Next5hReset.Before(end) {
			events = append(events, account.Next5hReset)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Before(events[j]) })
	margin, previous := math.Inf(1), now
	for _, at := range events {
		need := demand * at.Sub(previous).Hours()
		if need > 0 {
			available := 0.0
			for _, item := range items {
				available += item.remaining
			}
			margin = math.Min(margin, available/need)
			sort.SliceStable(items, func(i, j int) bool {
				a, b := items[i].reset, items[j].reset
				if a.IsZero() || !a.After(previous) {
					a = end
				}
				if b.IsZero() || !b.After(previous) {
					b = end
				}
				return a.Before(b)
			})
			for i := range items {
				debit := math.Min(need, items[i].remaining)
				items[i].remaining -= debit
				items[i].weekly = math.Max(0, items[i].weekly-debit)
				need -= debit
			}
		}
		if at.After(previous) {
			for i := range items {
				if !items[i].weeklyOnly && items[i].reset.Equal(at) {
					items[i].remaining = math.Min(items[i].capacity, items[i].weekly)
				}
			}
		}
		previous = at
	}
	return margin
}

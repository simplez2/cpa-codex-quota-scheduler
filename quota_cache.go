package main

import "time"

// Balanced traffic has many active accounts. Poll healthy busy accounts at
// most once per minute by default, increasing to the configured active cadence
// near depletion. Idle accounts retain the standby interval and per-auth backoff.
func (s *schedulerRuntimeState) quotaPollIntervalLocked(id, serialActive string, now time.Time) time.Duration {
	cfg := s.cfg
	if cfg.SchedulerMode != "balanced" {
		if id == serialActive {
			return cfg.RefreshInterval
		}
		return cfg.QuotaRefreshCooldown
	}
	account := s.balancedAccounts[id]
	if account == nil || now.Sub(account.LastPicked) > 5*time.Minute {
		return cfg.QuotaRefreshCooldown
	}
	interval := min(cfg.QuotaRefreshCooldown, max(cfg.RefreshInterval, time.Minute))
	for _, w := range s.quotas[id].Windows {
		if w.UsedPercent >= 90 {
			return min(interval, cfg.RefreshInterval)
		}
	}
	return interval
}

// Fresh response headers can cover one scheduled probe; they never renew the
// authoritative snapshot or override a failed poll's backoff. A full probe is
// still required within the standby interval or half the freshness limit.
func quotaHeaderCacheDeadline(q quotaSnapshot, poll quotaPollState, cfg pluginConfig, interval time.Duration, now time.Time) time.Time {
	if poll.Failures > 0 || q.HeaderObservedAt.IsZero() || q.HeaderObservedAt.After(now) || !quotaSnapshotFresh(q, now, cfg.StaleAfter) {
		return time.Time{}
	}
	deadline := minTime(q.HeaderObservedAt.Add(interval), q.RefreshedAt.Add(min(cfg.QuotaRefreshCooldown, cfg.StaleAfter/2)))
	for _, w := range q.Windows {
		if !w.Allowed || w.LimitReached || w.UsedPercent >= 90 {
			return time.Time{}
		}
		if !w.ResetAt.IsZero() {
			deadline = minTime(deadline, w.ResetAt.Add(2*time.Second))
		}
	}
	return deadline
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

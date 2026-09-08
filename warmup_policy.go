package main

import (
	"sort"
	"strings"
	"time"
)

const (
	warmupBudgetWindow   = 24 * time.Hour
	warmupQuotaMaxAge    = 2 * time.Minute
	warmupUncertainDelay = 5 * time.Hour
	warmupFailureLimit   = 3
)

// Keep a separate admission ledger: pruning a completed cycle or explicitly
// clearing a block must not refund traffic already sent. It contains no tokens.
type warmupAttempt struct {
	AuthID string    `json:"auth_id"`
	At     time.Time `json:"at"`
}

type warmupTrafficStatus struct {
	MinInterval     string    `json:"min_interval"`
	MaxPerDay       int       `json:"max_per_day"`
	AttemptsLast24h int       `json:"attempts_last_24h"`
	NextAllowedAt   time.Time `json:"next_allowed_at,omitempty"`
	HoldReason      string    `json:"hold_reason,omitempty"`
}

// The caller holds warmupMu. Union by admission identity makes repeated state
// loads and sibling-window outcomes idempotent, including after hot reload.
func (s *schedulerRuntimeState) mergeWarmupAttemptsLocked(incoming []warmupAttempt, now time.Time) {
	seen := make(map[warmupAttempt]struct{})
	cutoff := now.Add(-warmupBudgetWindow)
	add := func(attempt warmupAttempt) {
		attempt.AuthID = strings.TrimSpace(attempt.AuthID)
		attempt.At = attempt.At.UTC()
		if attempt.AuthID != "" && attempt.At.After(cutoff) {
			seen[attempt] = struct{}{}
		}
	}
	for _, attempt := range s.warmupAttempts {
		add(attempt)
	}
	for _, attempt := range incoming {
		add(attempt)
	}
	// Migrate pre-budget state conservatively. This cannot reconstruct attempts
	// that an older version already overwrote, but retains every known attempt.
	for _, entry := range s.warmups {
		add(warmupAttempt{AuthID: entry.AuthID, At: entry.AttemptedAt})
	}
	s.warmupAttempts = make([]warmupAttempt, 0, len(seen))
	for attempt := range seen {
		s.warmupAttempts = append(s.warmupAttempts, attempt)
	}
	sort.Slice(s.warmupAttempts, func(i, j int) bool {
		if !s.warmupAttempts[i].At.Equal(s.warmupAttempts[j].At) {
			return s.warmupAttempts[i].At.Before(s.warmupAttempts[j].At)
		}
		return s.warmupAttempts[i].AuthID < s.warmupAttempts[j].AuthID
	})
}

// Global holds prevent sequential account rotation from turning one upstream
// failure into a pool-wide probe burst. Ordinary client routing is unaffected.
func (s *schedulerRuntimeState) warmupTrafficStatusLocked(cfg pluginConfig, now time.Time) warmupTrafficStatus {
	interval, maxPerDay := cfg.WarmupMinInterval, cfg.WarmupMaxPerDay
	if interval < time.Minute {
		interval = 15 * time.Minute
	}
	if maxPerDay < 1 {
		maxPerDay = 8
	}
	s.mergeWarmupAttemptsLocked(nil, now)
	out := warmupTrafficStatus{MinInterval: interval.String(), MaxPerDay: maxPerDay, AttemptsLast24h: len(s.warmupAttempts)}
	hold := func(until time.Time, reason string) {
		if now.Before(until) && !until.Before(out.NextAllowedAt) {
			out.NextAllowedAt, out.HoldReason = until, reason
		}
	}
	if n := len(s.warmupAttempts); n > 0 {
		hold(s.warmupAttempts[n-1].At.Add(interval), "min_interval")
		if n >= maxPerDay {
			hold(s.warmupAttempts[n-maxPerDay].At.Add(warmupBudgetWindow), "daily_budget")
		}
	}
	blocked := false
	for _, entry := range s.warmups {
		if entry.Blocked {
			blocked = true
		}
		if entry.Error != "" {
			retryAt := entry.SuppressUntil
			if retryAt.IsZero() {
				retryAt = entry.AttemptedAt.Add(warmupFailureDelay(cfg.WarmupRetryAfter, entry.Failures))
			}
			hold(retryAt, "failure_backoff")
		} else if entry.CompletedAt.IsZero() && entry.OutcomeAt.IsZero() {
			hold(entry.SuppressUntil, "uncertain_outcome")
		}
	}
	if blocked {
		out.HoldReason = "manual_retry_required"
		out.NextAllowedAt = time.Time{}
	}
	return out
}

func warmupFailureDelay(base time.Duration, failures int) time.Duration {
	if base < time.Minute {
		base = 15 * time.Minute
	}
	for n := 1; n < failures && base < 6*time.Hour; n++ {
		base *= 2
	}
	if base > 6*time.Hour {
		base = 6 * time.Hour
	}
	return base
}

func warmupQuotaHasHeadroom(snapshot quotaSnapshot, cfg pluginConfig) bool {
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class != "" && window.UsedPercent >= 100-reserveForWindow(cfg, class) {
			return false
		}
	}
	return true
}

// A new row on the same account must not evade a failed or uncertain request's
// cooldown. Successful weekly activation still permits a later fresh 5h cycle.
func (s *schedulerRuntimeState) warmupSuppressedAccountsLocked(now time.Time, retryAfter time.Duration) map[string]bool {
	suppressed := make(map[string]bool)
	for _, entry := range s.warmups {
		if entry.Blocked || ((entry.Error != "" || entry.CompletedAt.IsZero()) && warmupEntrySuppressesNow(entry, now, retryAfter)) {
			suppressed[entry.AuthID] = true
		}
	}
	return suppressed
}

// Recheck immediately before dispatch. Inventory discovery and the instance
// lease can take time; quota headers or real traffic can change the decision.
func (s *schedulerRuntimeState) warmupCandidateStillEligible(candidate warmupCandidate, now time.Time) bool {
	if _, banned := banStore.lookup(candidate.Snapshot.AuthID); banned {
		return false
	}
	s.mu.RLock()
	snapshot, ok := s.quotas[candidate.Snapshot.AuthID]
	cfg, activeID, poll := s.cfg, s.serialActiveAuthID, s.quotaPolls[candidate.Snapshot.AuthID]
	s.mu.RUnlock()
	if !ok || activeID == candidate.Snapshot.AuthID || snapshot.AuthIndex != candidate.Snapshot.AuthIndex || poll.Error != "" ||
		!warmupSnapshotFresh(snapshot, now, cfg.StaleAfter) || !warmupQuotaHasHeadroom(snapshot, cfg) {
		return false
	}
	window, ok := unstartedWarmupWindow(snapshot, now)
	return ok && window.Class == candidate.Window.Class
}

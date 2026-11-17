package main

import (
	"math"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func balancedResetWeighted(cfg pluginConfig) bool {
	return cfg.SchedulerMode == "balanced" && cfg.BalancedPriorityPolicy == "reset_weighted"
}

// Called only by actual routing/usage events with s.mu held. A healthy standby
// interval must not make a confirmed weekly-only account disappear forever
// from catch-up allocation. Request a new read; never promote cached absence
// into fresh eligibility, invent usage, or shorten an upstream error backoff.
func (s *schedulerRuntimeState) requestWeeklyCatchupProbesLocked(now time.Time) {
	if !s.cfg.Enabled || !s.cfg.QuotaProbeOnDemand || !balancedResetWeighted(s.cfg) || s.cfg.WeeklyOnlyCatchupSharePercent <= 0 {
		return
	}
	age := max(time.Second, min(5*time.Minute, s.cfg.StaleAfter/2))
	for id, q := range s.quotas {
		if id != q.AuthID || q.FiveHourLimitStatus != "not_reported" || q.FiveHourLimitObservedAt.IsZero() ||
			now.Before(q.FiveHourLimitObservedAt.Add(age)) {
			continue
		}
		p := s.quotaPolls[id]
		if !p.AttemptedAt.IsZero() && now.Before(p.AttemptedAt.Add(age)) {
			continue
		}
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: id}, q, true, s.cfg, now)
		horizon, known := weeklyResetHorizon(choice, s.cfg, now)
		if !known || horizon > s.cfg.WeeklyCatchupWindow || !choice.Eligible || choice.FiveHourKnown || choice.WeeklyProtected {
			continue
		}
		if s.quotaPolls == nil {
			s.quotaPolls = make(map[string]quotaPollState)
		}
		p.CatchupPending = true
		s.quotaPolls[id] = p
	}
}

// A real weekly reset is distinct from credential expiry and a moving, full
// window placeholder. Passing a reset never establishes that quota refilled.
func weeklyResetHorizon(choice serialCandidate, cfg pluginConfig, now time.Time) (time.Duration, bool) {
	if !choice.QuotaKnown || !choice.WeeklyKnown {
		return 0, false
	}
	var horizon time.Duration
	for _, w := range choice.Snapshot.Windows {
		if normalizeWindowClass(w.Class) != "weekly" || !w.ResetAt.After(now) ||
			quotaWindowHasPlaceholderReset(w, choice.Snapshot.RefreshedAt, now) {
			continue
		}
		observed := w.ObservedAt
		if observed.IsZero() {
			observed = choice.Snapshot.RefreshedAt
		}
		if observed.IsZero() || observed.After(now) ||
			(now.Sub(observed) > cfg.StaleAfter && !cfg.QuotaProbeOnDemand) {
			continue
		}
		if value := w.ResetAt.Sub(now); horizon == 0 || value < horizon {
			horizon = value
		}
	}
	return horizon, horizon > 0
}

func weeklyOnlyCatchup(choice serialCandidate, cfg pluginConfig, now time.Time) bool {
	if !balancedResetWeighted(cfg) || cfg.WeeklyOnlyCatchupSharePercent <= 0 || !choice.Eligible ||
		choice.FiveHourKnown || choice.WindowClass != "weekly" || choice.WeeklyProtected ||
		poolFiveHourLimitStatus(quotaSnapshot{}, choice.Snapshot, now, cfg.StaleAfter) != "not_reported" {
		return false
	}
	horizon, known := weeklyResetHorizon(choice, cfg, now)
	return known && horizon <= cfg.WeeklyCatchupWindow
}

const balancedWeeklyHandbackGrace = 5 * time.Minute

// Give a hot weekly fallback one short handback window after 5h becomes
// selectable. New sessions, parent-only affinity, and old idle bindings do not
// earn this grace. Only an accepted continuation starts the fixed deadline.
func balancedWeeklyHandbackDeadline(binding balancedSessionBinding, choice serialCandidate, cfg pluginConfig, now time.Time) time.Time {
	if !balancedResetWeighted(cfg) || binding.AuthID != choice.Candidate.ID || binding.AuthIndex == "" ||
		binding.AuthIndex != choice.Snapshot.AuthIndex || now.Sub(binding.LastUsedAt) >= balancedWeeklyHandbackGrace ||
		!choice.Eligible || !choice.WeeklyKnown || choice.WeeklyProtected || choice.FiveHourKnown ||
		choice.WindowClass != "weekly" || !quotaSnapshotFresh(choice.Snapshot, now, cfg.StaleAfter) ||
		poolFiveHourLimitStatus(quotaSnapshot{}, choice.Snapshot, now, cfg.StaleAfter) != "not_reported" {
		return time.Time{}
	}
	if binding.WeeklyHandbackBy.IsZero() {
		return now.Add(balancedWeeklyHandbackGrace)
	}
	if now.Before(binding.WeeklyHandbackBy) {
		return binding.WeeklyHandbackBy
	}
	return time.Time{}
}

// Weights distribute estimated work for new sessions. Both reset boosts are
// bounded at 2x, so a near reset improves priority without monopolizing the
// entire eligible pool or replacing the observed remaining quota fraction.
func balancedResetWeight(choice serialCandidate, cfg pluginConfig, now time.Time, fiveRemaining, weeklyRemaining float64) float64 {
	_, capacity, _ := resolvedQuotaPlan(cfg, choice.Candidate.ID, choice.Snapshot, now)
	fiveFactor, weeklyFactor := 1.0, 1.0
	if choice.FiveHourKnown {
		fiveFactor = math.Max(.01, clampADQ(fiveRemaining, 0, 100)/100)
		if choice.FiveHourCycleActive && choice.FiveHourResetAt.After(now) {
			fiveFactor *= 1 + clampADQ(1-choice.FiveHourResetAt.Sub(now).Hours()/5, 0, 1)
		}
	}
	if choice.WeeklyKnown {
		weeklyFactor = math.Max(.01, clampADQ(weeklyRemaining, 0, 100)/100)
		if serialBudgetEnabled(cfg) && cfg.WeeklyCatchupWindow > 0 {
			if horizon, known := weeklyResetHorizon(choice, cfg, now); known {
				weeklyFactor *= 1 + clampADQ(1-float64(horizon)/float64(cfg.WeeklyCatchupWindow), 0, 1)
			}
		}
	}
	return capacity * fiveFactor * weeklyFactor
}

// Cap the combined weekly-only allocation while usable 5h candidates remain.
// Existing conversation affinity and actual variable costs mean this is a
// new-session allocation target, not a hard percentage cap on upstream usage.
// With no eligible 5h candidate, weekly-only accounts retain full fallback use.
func capBalancedCatchupWeights(weights map[string]float64, weeklyOnly map[string]bool, cfg pluginConfig) {
	if !balancedResetWeighted(cfg) {
		return
	}
	main, backup := 0.0, 0.0
	for id, weight := range weights {
		if weeklyOnly[id] {
			backup += weight
		} else {
			main += weight
		}
	}
	share := cfg.WeeklyOnlyCatchupSharePercent / 100
	if main <= 0 || backup <= 0 || share <= 0 || share >= 1 {
		return
	}
	limit := main * share / (1 - share)
	if backup > limit {
		for id := range weeklyOnly {
			weights[id] *= limit / backup
		}
	}
}

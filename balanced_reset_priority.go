package main

import (
	"math"
	"time"
)

func balancedResetWeighted(cfg pluginConfig) bool {
	return cfg.SchedulerMode == "balanced" && cfg.BalancedPriorityPolicy == "reset_weighted"
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

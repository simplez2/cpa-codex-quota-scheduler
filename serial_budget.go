package main

import (
	"math"
	"strings"
	"time"
)

const (
	serialBudgetResetFloor    = time.Minute
	weeklyBudgetMinutesPerDay = 24 * 60
)

// Plan multipliers are approximate 5h priors, not guaranteed message counts or
// weekly capacities. These are configuration aliases, not native SKU mappings.
func normalizeQuotaPlan(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "plus":
		return "plus", true
	case "team", "business", "standard", "team_standard", "business_standard":
		return "team_standard", true
	case "pro_5x", "pro5", "pro5x":
		return "pro_5x", true
	case "pro_20x", "pro20", "pro20x":
		return "pro_20x", true
	case "team_premium", "business_premium", "premium":
		return "team_premium", true
	default:
		return "", false
	}
}

func quotaPlanForAuth(cfg pluginConfig, id string) (string, float64) {
	plan := cfg.QuotaAccountPlans[id]
	if plan == "" {
		plan = cfg.QuotaDefaultPlan
	}
	plan, ok := normalizeQuotaPlan(plan)
	if !ok {
		plan = "team_standard"
	}
	return plan, quotaPlanWeight(plan)
}

func quotaPlanWeight(plan string) float64 {
	switch plan {
	case "pro_5x", "team_premium":
		return 5
	case "pro_20x":
		return 20
	default:
		return 1
	}
}

func serialBudgetEnabled(cfg pluginConfig) bool { return cfg.SerialAllocationPolicy == "sustainable" }

// Preserve daily units for existing comparisons and management API consumers.
// The shared calculation uses the exact remaining minutes, with only a one-minute
// floor so a reset seconds away cannot produce an unbounded scheduling weight.
func serialWeeklyBudget(choice serialCandidate, cfg pluginConfig, now time.Time) (float64, bool) {
	rate, known := serialWeeklyBudgetPerMinute(choice, cfg, now)
	return rate * weeklyBudgetMinutesPerDay, known
}

// Compute each credential's own weekly percentage budget per minute. Keep this
// independent of plan weight: a 20x prior must not scale its own quota percentage.
func serialWeeklyBudgetPerMinute(choice serialCandidate, cfg pluginConfig, now time.Time) (float64, bool) {
	if !choice.QuotaKnown || !choice.WeeklyKnown {
		return 0, false
	}
	var budget float64
	found := false
	for _, w := range choice.Snapshot.Windows {
		if normalizeWindowClass(w.Class) != "weekly" || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
			continue
		}
		observed := w.ObservedAt
		if observed.IsZero() {
			observed = choice.Snapshot.RefreshedAt
		}
		if observed.IsZero() || observed.After(now) || (now.Sub(observed) > cfg.StaleAfter && !(cfg.QuotaProbeOnDemand && !w.ResetAt.IsZero() && now.Before(w.ResetAt))) {
			return 0, false
		}
		remaining := math.Max(0, 100-w.UsedPercent)
		// Only a missing reset or a provider placeholder uses a complete week.
		// A full window can still have a confirmed imminent reset (e.g. a refill).
		horizon := 7 * 24 * time.Hour
		if !w.ResetAt.IsZero() && !quotaWindowHasPlaceholderReset(w, choice.Snapshot.RefreshedAt, now) {
			horizon = w.ResetAt.Sub(now)
		}
		if horizon < serialBudgetResetFloor {
			horizon = serialBudgetResetFloor
		}
		if horizon > 7*24*time.Hour {
			horizon = 7 * 24 * time.Hour
		}
		rate := math.Max(0, remaining-cfg.ReserveWeeklyPercent) / horizon.Minutes()
		if !found || rate < budget {
			budget = rate
			found = true
		}
	}
	return budget, found
}

func serialBudgetTier(choice serialCandidate, ctx serialSortContext) int {
	if !choice.WeeklyBudgetKnown {
		return int(^uint(0) >> 1)
	}
	group := serialBudgetGroup(choice)
	// Fixed pool-wide 5% bands preserve a transitive comparator. The much
	// wider 20% preemption threshold plus observations/hold prevents flapping.
	band := math.Max(ctx.maxWeeklyBudget[group]*0.05, 0.000001)
	return int(math.Floor(math.Max(0, ctx.maxWeeklyBudget[group]-choice.WeeklyBudgetPerDay) / band))
}

func serialBudgetGroup(choice serialCandidate) string {
	if choice.WeeklyProtected {
		return choice.WindowClass + ":protected"
	}
	return choice.WindowClass + ":available"
}

// Completion headers arrive after the upstream actually sampled them. They
// cannot undo a stricter fresh probe during an unexpired cycle. Keep distinct
// reset anchors as independent constraints rather than trusting a newer local
// completion timestamp as evidence that quota renewed.
func (s *schedulerRuntimeState) serialConservativeQuotaLocked(snapshot quotaSnapshot, now time.Time) quotaSnapshot {
	native, ok := s.quotaNative[snapshot.AuthID]
	if !ok || native.AuthIndex != snapshot.AuthIndex || !quotaSnapshotFresh(native, now, s.cfg.StaleAfter) {
		return snapshot
	}
	snapshot.Windows = append([]quotaWindow(nil), snapshot.Windows...)
	for _, probe := range native.Windows {
		if probe.ObservedAt.After(now) || (!probe.ResetAt.IsZero() && !now.Before(probe.ResetAt)) {
			continue
		}
		matched := false
		for i, w := range snapshot.Windows {
			if normalizeWindowClass(w.Class) != normalizeWindowClass(probe.Class) ||
				(!w.ResetAt.Equal(probe.ResetAt) && !quotaRunwaySameReset(w.ResetAt, probe.ResetAt)) {
				continue
			}
			snapshot.Windows[i].UsedPercent = math.Max(w.UsedPercent, probe.UsedPercent)
			snapshot.Windows[i].Allowed = w.Allowed && probe.Allowed
			snapshot.Windows[i].LimitReached = w.LimitReached || probe.LimitReached
			matched = true
		}
		if !matched {
			snapshot.Windows = append(snapshot.Windows, probe)
		}
	}
	return snapshot
}

func serialRebalanceAdvantage(current, next serialCandidate, cfg pluginConfig) (float64, bool) {
	if serialBudgetEnabled(cfg) && current.WeeklyBudgetKnown && next.WeeklyBudgetKnown {
		if cfg.SerialBudgetRebalancePercent <= 0 {
			return 0, false
		}
		// Absolute floor prevents tiny near-reserve budgets producing arbitrarily
		// large relative improvements and hopping between nearly exhausted peers.
		delta := next.WeeklyBudgetPerDay - current.WeeklyBudgetPerDay
		denominator := math.Max(current.WeeklyBudgetPerDay, 1)
		advantage := 100 * delta / denominator
		return advantage, delta > 0 && advantage >= cfg.SerialBudgetRebalancePercent
	}
	gap := next.WeeklyRemaining - current.WeeklyRemaining
	return gap, cfg.SerialWeeklyRebalancePercent > 0 && gap >= cfg.SerialWeeklyRebalancePercent && gap > cfg.SwitchHysteresisPercent
}

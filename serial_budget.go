package main

import (
	"math"
	"strings"
	"time"
)

const serialBudgetResetFloor = 6 * time.Hour

// Plan multipliers are approximate 5h priors, not guaranteed message counts or
// weekly capacities. Ambiguous native "pro"/"team" labels cannot identify a seat.
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
	switch plan {
	case "pro_5x", "team_premium":
		return plan, 5
	case "pro_20x":
		return plan, 20
	default:
		return plan, 1
	}
}

func serialBudgetEnabled(cfg pluginConfig) bool { return cfg.SerialAllocationPolicy == "sustainable" }

// Compute a normalized weekly percentage budget per day. Keep this independent
// of plan weight: multiplying by 20 here would drain a Pro account's weekly
// fraction before small accounts contribute, stranding its future 5h cycles.
func serialWeeklyBudget(choice serialCandidate, cfg pluginConfig, now time.Time) (float64, bool) {
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
		if observed.IsZero() || observed.After(now) || now.Sub(observed) > cfg.StaleAfter {
			return 0, false
		}
		remaining := math.Max(0, 100-w.UsedPercent)
		// No reset / never-started placeholder: a complete week, not an inferred
		// imminent reset. Full-but-confirmed windows lose no safety from this.
		horizon := 7 * 24 * time.Hour
		if w.UsedPercent > 0 && !w.ResetAt.IsZero() {
			horizon = w.ResetAt.Sub(now)
		}
		if horizon < serialBudgetResetFloor {
			horizon = serialBudgetResetFloor
		}
		if horizon > 7*24*time.Hour {
			horizon = 7 * 24 * time.Hour
		}
		rate := math.Max(0, remaining-cfg.ReserveWeeklyPercent) / (horizon.Hours() / 24)
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

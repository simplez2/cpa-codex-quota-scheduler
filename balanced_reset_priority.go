package main

import (
	"math"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func balancedResetWeighted(cfg pluginConfig) bool {
	return cfg.SchedulerMode == "balanced" && cfg.BalancedPriorityPolicy == "reset_weighted"
}

func weeklyCatchupProbeHorizon(cfg pluginConfig) time.Duration {
	limit := cfg.WeeklyOnlyPriorityWindow
	if cfg.WeeklyOnlyCatchupSharePercent > 0 {
		limit = max(limit, cfg.WeeklyCatchupWindow)
	}
	return limit
}

// Called only by actual routing/usage events with s.mu held. A healthy standby
// interval must not make a confirmed weekly-only account disappear forever
// from catch-up allocation. Request a new read; never promote cached absence
// into fresh eligibility, invent usage, or shorten an upstream error backoff.
func (s *schedulerRuntimeState) requestWeeklyCatchupProbesLocked(now time.Time) {
	if !s.cfg.Enabled || !s.cfg.QuotaProbeOnDemand || !balancedResetWeighted(s.cfg) {
		return
	}
	horizonLimit := weeklyCatchupProbeHorizon(s.cfg)
	if horizonLimit <= 0 {
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
		if !known || horizon > horizonLimit || !choice.Eligible || choice.FiveHourKnown || choice.WeeklyProtected {
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
	if !balancedResetWeighted(cfg) || cfg.WeeklyOnlyCatchupSharePercent <= 0 || !confirmedAvailableWeeklyOnly(choice, cfg, now) {
		return false
	}
	horizon, known := weeklyResetHorizon(choice, cfg, now)
	return known && horizon <= cfg.WeeklyCatchupWindow
}

func confirmedAvailableWeeklyOnly(choice serialCandidate, cfg pluginConfig, now time.Time) bool {
	return choice.Eligible && choice.WeeklyKnown && !choice.WeeklyProtected && !choice.FiveHourKnown &&
		choice.WindowClass == "weekly" &&
		poolFiveHourLimitStatus(quotaSnapshot{}, choice.Snapshot, now, cfg.StaleAfter) == "not_reported"
}

func weeklyOnlyUrgent(choice serialCandidate, cfg pluginConfig, now time.Time) bool {
	if !balancedResetWeighted(cfg) || cfg.WeeklyOnlyPriorityWindow <= 0 || !confirmedAvailableWeeklyOnly(choice, cfg, now) ||
		!quotaSnapshotFresh(choice.Snapshot, now, cfg.StaleAfter) {
		return false
	}
	horizon, known := weeklyResetHorizon(choice, cfg, now)
	return known && horizon <= cfg.WeeklyOnlyPriorityWindow
}

// The final half hour shares one denominator. This bounds the influence of
// small reset timestamp differences without treating a passed reset as a refill.
const weeklyDeadlinePaceFloor = 30 * time.Minute

func weeklyDeadlinePace(effective float64, horizon time.Duration) float64 {
	if !finiteADQ(effective) || effective <= 0 || horizon <= 0 {
		return 0
	}
	return effective / max(weeklyDeadlinePaceFloor, horizon).Hours()
}

// A direct binding can move between turns, after both cache age bands. Keep
// a source that is also spending down a confirmed final weekly window.
// The caller already excluded CPA's request-scoped credential pins.
func (s *schedulerRuntimeState) balancedColdWeeklyHandoffLocked(session string, binding balancedSessionBinding, source serialCandidate, now time.Time) bool {
	if !balancedResetWeighted(s.cfg) || s.cfg.WeeklyOnlyPriorityWindow <= 0 || session == "" ||
		!source.Eligible || !source.FiveHourKnown || binding.AuthID != source.Candidate.ID ||
		binding.AuthIndex == "" || binding.AuthIndex != source.Snapshot.AuthIndex ||
		adqCacheState(now.Sub(binding.LastUsedAt), s.adqPolicyLocked()) != "cold" {
		return false
	}
	if horizon, known := weeklyResetHorizon(source, s.cfg, now); known && horizon <= s.cfg.WeeklyOnlyPriorityWindow {
		return false
	}
	active, lifecycleObserved := s.concurrency.sessionActivity(session, binding.AuthID)
	return !active && (lifecycleObserved || !s.balancedSessionHasPendingLocked(session, binding.AuthID, now))
}

// An elective handoff must have room for this whole request, including the
// completed work that the upstream quota reading has not acknowledged yet.
// The definitive selection still needs a free slot and an atomic reservation.
func (s *schedulerRuntimeState) balancedUrgentWeeklyCanTakeLocked(choice serialCandidate, cost float64, now time.Time) bool {
	if !choice.Priority.WeeklyUrgent {
		return false
	}
	input, ok := s.adqInputForChoiceLocked(choice, now)
	if !ok {
		return false
	}
	metrics := adqAssessAccount(input, s.adqPolicyLocked(), now)
	return metrics.Eligible && metrics.WEffective >= cost
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

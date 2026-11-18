package main

import (
	"math"
	"strings"
	"time"
)

const defaultQuotaURL = "https://chatgpt.com/backend-api/wham/usage"

type capacityInterval struct {
	start, end time.Time
	delta      float64 // native weekly percentage points, including flat observations
}

// Use the lesser of two disjoint five-minute rates, including idle time.
// Only whole intervals fully inside a bin contribute their measured drop.
// Ignoring crossing intervals underestimates (never invents) measured work.
func capacityRecentRate(o quotaRunwayObservation, now time.Time) float64 {
	const bin = 5 * time.Minute
	if len(o.intervals) == 0 || o.observedAt.After(now) || now.Sub(o.observedAt) > bin ||
		o.intervals[0].start.After(now.Add(-2*bin)) {
		return 0
	}
	var work [2]float64
	for i := range work {
		a := now.Add(-time.Duration(2-i) * bin)
		b := a.Add(bin)
		for _, v := range o.intervals {
			if !v.start.Before(a) && !v.end.After(b) {
				work[i] += v.delta
			}
		}
	}
	return min(work[0], work[1]) / bin.Hours()
}

// A confirmed natural weekly sprint has precedence unless the observed
// service of EVERY spendable urgent account leaves enough room to defer it
// through the earlier compulsory 5h refill. All units are per credential.
// Never extrapolate a short observation over an entire day.
func (s *schedulerRuntimeState) capacityDetourLocked(group, urgent []serialCandidate, cost float64, now time.Time) bool {
	if len(group) == 0 || group[0].opportunityTier != 1 || len(urgent) == 0 {
		return false
	}
	var resume time.Time
	for _, c := range group {
		if c.FiveHourResetAt.After(resume) {
			resume = c.FiveHourResetAt
		}
	}
	for _, c := range urgent {
		in, ok := s.adqInputForChoiceLocked(c, now)
		if !ok {
			return false
		}
		m := adqAssessAccount(in, s.adqPolicyLocked(), now)
		if !m.Eligible || m.WEffective < cost {
			continue
		}
		h, ok := weeklyResetHorizon(c, s.cfg, now)
		if !ok || !now.Add(h).After(resume) {
			return false
		}
		key := quotaRunwayKey{c.Snapshot.AuthID, c.Snapshot.AuthIndex, "weekly"}
		o := s.rotationRunway.windows[key]
		if s.rotationRunway.bindings[key.authID] != key.authIndex || !quotaRunwaySameReset(o.resetAt, now.Add(h)) {
			return false
		}
		rate := capacityRecentRate(o, now) * m.W / 100
		run := min(time.Hour, now.Add(h).Sub(resume)).Hours()
		if rate <= 0 || .5*rate*run < m.WEffective+cost {
			return false
		}
	}
	return true
}

// A forecast influences placement, never admission. Both endpoints must carry
// a timezone. No forecast is inferred from card counts or coincident clocks.
func parseCollectiveWindow(raw string) (time.Time, time.Time, bool) {
	a, b, ok := strings.Cut(raw, "/")
	early, ea := time.Parse(time.RFC3339, a)
	late, eb := time.Parse(time.RFC3339, b)
	return early, late, ok && ea == nil && eb == nil && !early.IsZero() && !late.Before(early) && late.Sub(early) <= 24*time.Hour
}

func capacityDeadlineEnabled(cfg pluginConfig) bool {
	return cfg.SchedulerMode == "balanced" && cfg.BalancedPriorityPolicy == "capacity_deadline"
}

func (s *schedulerRuntimeState) collectiveHorizonLocked(now time.Time) (time.Time, bool) {
	a, b := s.cfg.collectiveEarliest, s.cfg.collectiveLatest
	if a.IsZero() || !now.Before(b) || (s.quotaEpoch.Reason == "provider_reset" &&
		!s.quotaEpoch.ConfirmedAt.Before(a.Add(-5*time.Hour)) && !s.quotaEpoch.ConfirmedAt.After(now)) {
		return time.Time{}, false
	}
	// Once inside the uncertainty interval, budget for a reset at any time.
	return a, !now.Before(a.Add(-5 * time.Hour))
}

func capacityWindowFresh(q quotaSnapshot, class string, cfg pluginConfig, now time.Time) bool {
	for _, w := range q.Windows {
		if normalizeWindowClass(w.Class) != class {
			continue
		}
		at := w.ObservedAt
		if at.IsZero() {
			at = q.RefreshedAt
		}
		if !at.IsZero() && !at.After(now) && now.Sub(at) <= cfg.StaleAfter && (w.ResetAt.IsZero() || w.ResetAt.After(now)) && w.Allowed && !w.LimitReached {
			return true
		}
	}
	return false
}

// A verified card or native refill ends speculative spending for this
// credential only. The pool-wide stop requires the existing confirmed epoch.
func (s *schedulerRuntimeState) credentialSawRefillLocked(c serialCandidate, now time.Time) bool {
	from := s.cfg.collectiveEarliest.Add(-5 * time.Hour)
	inRange := func(at time.Time) bool { return !at.IsZero() && !at.Before(from) && !at.After(now) }
	v := s.resetCredits[c.Candidate.ID]
	if v.AuthIndex == c.Snapshot.AuthIndex && v.LastAction == "verified" && inRange(v.ActionAt) {
		return true
	}
	sweep := s.quotaEpoch.Sweep
	t := sweep.Targets[c.Candidate.ID]
	return sweep.Reason == "provider_reset" && t.AuthIndex == c.Snapshot.AuthIndex && t.Evidence && inRange(t.ObservedAt)
}

// Upper bound on later 5h capacity under back-to-back replenishments. Excess
// weekly stock cannot be recovered from later tranches even in that best case.
// A refill exactly at the spending deadline cannot serve the old balance.
func unavoidableTranche(week, head, full float64, next, end time.Time, cycle time.Duration) float64 {
	if !finiteADQ(week) || !finiteADQ(head) || !finiteADQ(full) || week <= 0 || head <= 0 || full <= 0 ||
		cycle <= 0 || next.IsZero() || end.IsZero() || !next.Before(end) {
		return 0
	}
	n := 1 + (end.Sub(next)-1)/cycle
	return min(head, max(0, week-float64(n)*full))
}

// Only the existing operator-enabled auto-use policy implies an impending
// replacement. An optional/manual card has option value, not a deadline.
// Never route merely to create eligibility on an otherwise unused account.
func (s *schedulerRuntimeState) plannedCardLocked(c serialCandidate, now time.Time) time.Time {
	if !s.cfg.ResetCreditsAutoUse || s.cfg.QuotaURL != defaultQuotaURL || c.Snapshot.AuthIndex == "" {
		return time.Time{}
	}
	q := c.Snapshot
	state := s.resetCredits[c.Candidate.ID]
	if state.AuthIndex != q.AuthIndex || state.ObservedAt.IsZero() || now.Before(state.ObservedAt) ||
		now.Sub(state.ObservedAt) > resetCreditsCacheTTL || state.Error != "" || !state.Complete || !state.CountKnown ||
		state.Count == 0 || (q.ResetCreditsKnown && q.ResetCreditsObservedAt.After(state.ObservedAt) && q.ResetCredits != state.Count) {
		return time.Time{}
	}
	if state.AutoReason != "" && state.AutoReason != "scheduled" && state.AutoReason != "due" {
		return time.Time{}
	}
	if state.LastAction != "" && !state.ActionAt.IsZero() && !state.ObservedAt.After(state.ActionAt) {
		return time.Time{} // a pre-redemption list cannot identify another pending card
	}
	used := false
	for _, w := range q.Windows {
		used = used || w.UsedPercent > 0
	}
	if used {
		for _, card := range state.Credits {
			if card.ExpiresAt.After(now) {
				at := card.ExpiresAt.Add(-s.cfg.ResetCreditsAutoUseBefore)
				if card.Supported && at.After(now) {
					return at
				}
				break // do not jump over the provider's first card
			}
		}
	}
	return time.Time{}
}

func (s *schedulerRuntimeState) capacityFlexHoldLocked(c serialCandidate, now time.Time) bool {
	if !capacityDeadlineEnabled(s.cfg) || !confirmedAvailableWeeklyOnly(c, s.cfg, now) || !capacityWindowFresh(c.Snapshot, "weekly", s.cfg, now) {
		return false
	}
	_, burst := s.collectiveHorizonLocked(now)
	card := s.plannedCardLocked(c, now)
	return (burst && !s.credentialSawRefillLocked(c, now)) || (!card.IsZero() && card.Sub(now) <= 5*time.Hour)
}

type capacityOffer struct {
	choice serialCandidate
	at     time.Time
	weight float64
}

// A confirmed personal weekly deadline is handled before these tiers. Then
// protect a genuinely nonrecoverable 5h slice; spend down an already planned
// card locally; finally share a forecast sprint by immediately usable stock.
// Optional stock and projected refills never enter the reservation ledger.
func (s *schedulerRuntimeState) capacityOffersLocked(choices []serialCandidate, cost float64, now time.Time) [][]serialCandidate {
	if !capacityDeadlineEnabled(s.cfg) || s.adqReservations == nil {
		return nil
	}
	p := s.adqPolicyLocked()
	collective, burst := s.collectiveHorizonLocked(now)
	var lots [3][]capacityOffer
	for _, c := range choices {
		if !c.Eligible || c.WeeklyProtected || c.Snapshot.AuthIndex == "" || !capacityWindowFresh(c.Snapshot, "weekly", s.cfg, now) ||
			(c.FiveHourKnown && !capacityWindowFresh(c.Snapshot, "5h", s.cfg, now)) {
			continue
		}
		in, known := s.adqInputForChoiceLocked(c, now)
		if !known {
			continue
		}
		m := adqAssessAccount(in, p, now)
		spend := m.WEffective
		if !m.WeeklyOnly {
			spend = min(spend, m.HEffective)
		}
		if !m.Eligible || spend < cost || !finiteADQ(spend) {
			continue
		}
		card := s.plannedCardLocked(c, now)
		end := collective
		accountBurst := burst
		if !end.IsZero() && s.credentialSawRefillLocked(c, now) {
			end = time.Time{}
			accountBurst = false
		}
		personal := card
		if h, ok := weeklyResetHorizon(c, s.cfg, now); ok && (personal.IsZero() || now.Add(h).Before(personal)) {
			personal = now.Add(h)
		}
		if !personal.IsZero() && (end.IsZero() || personal.Before(end)) {
			end = personal
		}
		if !m.WeeklyOnly && c.FiveHourCycleActive && c.FiveHourResetAt.After(now) {
			full := in.FiveHourCapacity * (1 - adqFiveHourSafety(p))
			amount := unavoidableTranche(m.WEffective, m.HEffective, full, c.FiveHourResetAt, end, p.FiveHourWindow)
			if accountBurst && !c.FiveHourResetAt.Before(collective) {
				// If the common reset is on time, either reservoir can serve
				// this job. If it is late or absent and the next real 5h cycle
				// has only one tranche for the burst, flexible stock is more
				// useful. Protect that option without displacing a personal
				// deadline or counting any future supply as spendable.
				recourse := c.FiveHourResetAt.Add(p.FiveHourWindow)
				if !personal.IsZero() && personal.Before(recourse) {
					recourse = personal
				}
				amount = max(amount, unavoidableTranche(m.WEffective, m.HEffective, full, c.FiveHourResetAt, recourse, p.FiveHourWindow))
			}
			if amount >= cost {
				lots[0] = append(lots[0], capacityOffer{c, c.FiveHourResetAt, amount})
			}
		}
		if !card.IsZero() && card.Sub(now) <= p.FiveHourWindow {
			lots[1] = append(lots[1], capacityOffer{c, card, spend})
		}
		if accountBurst {
			lots[2] = append(lots[2], capacityOffer{c, collective, spend})
		}
	}
	var out [][]serialCandidate
	for tier, offers := range lots {
		if len(offers) == 0 {
			continue
		}
		first := offers[0].at
		for _, o := range offers {
			if o.at.Before(first) {
				first = o.at
			}
		}
		maxAt := first.Add(weeklyDeadlineSlack(max(0, first.Sub(now))))
		var group []serialCandidate
		for _, o := range offers {
			if o.at.After(maxAt) {
				continue
			}
			c := o.choice
			c.opportunityWeight, c.opportunityTier = o.weight, tier+1
			group = append(group, c)
		}
		out = append(out, group)
	}
	return out
}

// Ramp the ordinary catch-up target between its entry and the firm final
// window, instead of immediately spending the full configured backup share.
func catchupCapacityRamp(c serialCandidate, cfg pluginConfig, now time.Time) float64 {
	h, ok := weeklyResetHorizon(c, cfg, now)
	width := cfg.WeeklyCatchupWindow - cfg.WeeklyOnlyPriorityWindow
	if !ok || width <= 0 {
		return 1
	}
	return math.Max(0, math.Min(1, float64(cfg.WeeklyCatchupWindow-h)/float64(width)))
}

func capPlacementCatchup(weights map[string]float64, weekly map[string]bool, cfg pluginConfig, ramp float64) {
	if capacityDeadlineEnabled(cfg) && len(weekly) < len(weights) {
		if ramp <= 0 {
			for id := range weekly {
				weights[id] = 0
			}
			return
		}
		cfg.WeeklyOnlyCatchupSharePercent *= ramp
	}
	capBalancedCatchupWeights(weights, weekly, cfg)
}

type placementTier struct {
	choices []serialCandidate
	mode    adqAllocationMode
}

// Identical ordering for admission preflight and the atomic picker. A cold
// binding can stay on its own compulsory tranche or migrate to weekly-only;
// we never tear down a conversation to chase another dual-limited account.
func (s *schedulerRuntimeState) placementTiersLocked(choices []serialCandidate, cost float64, now time.Time, source string) []placementTier {
	var urgent []serialCandidate
	for _, c := range choices {
		if c.Priority.WeeklyUrgent && (source == "" || s.balancedUrgentWeeklyCanTakeLocked(c, cost, now)) {
			urgent = append(urgent, c)
		}
	}
	lots := s.capacityOffersLocked(choices, cost, now)
	var out []placementTier
	appendLot := func(group []serialCandidate) bool {
		if source != "" {
			var keep []serialCandidate
			for _, c := range group {
				if c.Candidate.ID == source && c.opportunityTier != 3 {
					out = append(out, placementTier{[]serialCandidate{c}, adqAllocateOpportunity})
					return true
				}
				if !c.FiveHourKnown {
					keep = append(keep, c)
				}
			}
			if len(keep) == 0 {
				return false
			}
			group = keep
		}
		out = append(out, placementTier{group, adqAllocateOpportunity})
		return true
	}
	if len(lots) > 0 && s.capacityDetourLocked(lots[0], urgent, cost, now) {
		if source == "" {
			appendLot(lots[0])
			lots = lots[1:]
		} else if c, ok := serialChoiceByID(lots[0], source); ok {
			appendLot([]serialCandidate{c})
			lots = lots[1:]
		}
	}
	if len(urgent) > 0 {
		out = append(out, placementTier{urgent, adqAllocateWeeklyDeadline})
	}
	for _, g := range lots {
		if !appendLot(g) && source != "" {
			break
		}
	}
	return out
}

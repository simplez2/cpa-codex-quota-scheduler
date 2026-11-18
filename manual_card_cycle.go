package main

import (
	"fmt"
	"math"
	"time"
)

// All percentages below refer to one credential's weekly capacity. Only the
// optimistic schedule is projected; no card or future tranche is reserved.
type manualCardCycle struct {
	State                                string  `json:"state"`
	Cards                                int     `json:"feasible_cards"`
	PacketPercent                        float64 `json:"packet_week_percent,omitempty"`
	EarliestHours                        float64 `json:"earliest_hours"`
	AdvanceHours                         float64 `json:"advance_hours,omitempty"`
	FreeHours                            float64 `json:"free_horizon_hours"`
	PacedAdvanceHours                    float64 `json:"paced_advance_hours,omitempty"`
	packet, value, pacedWork, pacedValue float64
}

func observedWindow(q quotaSnapshot, class string, after, now time.Time, age time.Duration) (quotaWindow, bool) {
	for _, w := range q.Windows {
		if normalizeWindowClass(w.Class) != class {
			continue
		}
		at := w.ObservedAt
		if at.IsZero() {
			at = q.RefreshedAt
		}
		return w, !at.IsZero() && at.After(after) && !at.After(now) && now.Sub(at) <= age && w.ResetAt.After(now)
	}
	return quotaWindow{}, false
}

// The provider selects the earliest unexpired card. Cards that expire before
// a possible exercise are skipped; a still-live unsupported first card blocks
// the projection. After exercise both clocks start a new cycle.
func feasibleCardChain(cards []resetCredit, now, first time.Time, cycle, natural, autoBefore time.Duration) int {
	at, n := first, 0
	for _, c := range cards {
		if !c.ExpiresAt.After(now) {
			continue
		}
		if autoBefore > 0 && c.Supported && !c.ExpiresAt.Add(-autoBefore).After(at) {
			break
		}
		if !c.ExpiresAt.After(at) {
			continue
		}
		if !c.Supported || c.GrantedAt.After(at) {
			break
		}
		n++
		if cycle <= 0 || cycle >= natural {
			break
		}
		at = at.Add(cycle)
	}
	return n
}

func cardTranches(work, full float64) float64 {
	if work <= 0 {
		return 0
	}
	return max(1, math.Ceil(work/full-1e-10)) // tolerate representation error at exact multiples
}

func manualCycleFor(q, native quotaSnapshot, st resetCreditState, m adqAccountMetrics, fiveCapacity, demand float64, cfg pluginConfig, p adqPolicy, collective, now time.Time) *manualCardCycle {
	if !capacityDeadlineEnabled(cfg) || !cfg.PreferResetCredits || cfg.QuotaURL != defaultQuotaURL ||
		q.AuthIndex == "" || q.AuthIndex != st.AuthIndex || !st.Complete || !st.CountKnown || st.Count <= 0 || st.Count != len(st.Credits) ||
		st.Error != "" || st.ObservedAt.IsZero() || st.ObservedAt.After(now) || now.Sub(st.ObservedAt) > resetCreditsCacheTTL ||
		(q.ResetCreditsKnown && q.ResetCreditsObservedAt.After(st.ObservedAt) && q.ResetCredits != st.Count) ||
		(st.LastAction != "" && (st.LastAction != "verified" || !st.ObservedAt.After(st.ActionAt))) {
		return nil
	}
	switch st.AutoReason {
	case "", "disabled", "scheduled", "due", "used", "quota_unused", "plan_unsupported":
	default:
		return nil
	}
	barrier := st.QuotaAfter
	if st.ActionAt.After(barrier) {
		barrier = st.ActionAt
	}
	five, ok5 := observedWindow(q, "5h", barrier, now, cfg.StaleAfter)
	week, okw := observedWindow(q, "weekly", barrier, now, cfg.StaleAfter)
	if !ok5 || !okw || m.WeeklyOnly || m.W <= 0 || fiveCapacity <= 0 || !finiteADQ(m.W) || !finiteADQ(fiveCapacity) ||
		p.FiveHourWindow <= 0 || p.WeeklyWindow <= 0 {
		return nil
	}
	if !barrier.IsZero() {
		_, fresh5 := observedWindow(native, "5h", barrier, now, cfg.StaleAfter)
		_, freshW := observedWindow(native, "weekly", barrier, now, cfg.StaleAfter)
		if native.AuthIndex != q.AuthIndex || !fresh5 || !freshW {
			return nil
		}
	}
	full := fiveCapacity * (1 - adqFiveHourSafety(p))
	h, w := min(full, m.HEffective), m.WEffective
	if !finiteADQ(w) || !finiteADQ(h) || full <= 0 {
		return nil
	}
	next, free := five.ResetAt, week.ResetAt
	if quotaWindowHasPlaceholderReset(five, q.RefreshedAt, now) {
		next = now.Add(p.FiveHourWindow)
	}
	if quotaWindowHasPlaceholderReset(week, q.RefreshedAt, now) {
		free = now.Add(p.WeeklyWindow)
	}
	if !collective.IsZero() && collective.Before(free) {
		free = collective
	}
	span := p.FiveHourWindow
	freshWeek := m.W * (1 - adqWeeklySafety(p))
	rounds := cardTranches(freshWeek, full) - 1
	cycle := p.WeeklyWindow
	if rounds >= 0 && rounds < float64(p.WeeklyWindow)/float64(span) {
		cycle = time.Duration(rounds) * span
	}
	ready := &manualCardCycle{FreeHours: max(0, free.Sub(now).Hours())}
	lead := time.Duration(0)
	if cfg.ResetCreditsAutoUse && (st.AutoReason == "scheduled" || st.AutoReason == "due") {
		lead = cfg.ResetCreditsAutoUseBefore
	}
	if w <= 0 {
		// In-flight and unsettled work can bring the effective ledger to zero;
		// only fresh *native* weekly use may tell the customer they are at reserve.
		raw, ok := observedWindow(native, "weekly", barrier, now, cfg.StaleAfter)
		if !ok || native.AuthIndex != q.AuthIndex || 100-raw.UsedPercent > 100*adqWeeklySafety(p)+1e-9 ||
			100-week.UsedPercent > 100*adqWeeklySafety(p)+1e-9 {
			return nil
		}
		ready.Cards = feasibleCardChain(st.Credits, now, now, cycle, p.WeeklyWindow, lead)
		if ready.Cards == 0 {
			return nil
		}
		ready.State = "at_reserve"
		if !free.After(now.Add(span)) {
			ready.State = "free_reset_near"
		}
		return ready
	}
	if !m.Eligible || h <= 0 || !week.Allowed || week.LimitReached || !five.Allowed || five.LimitReached || !free.After(now) {
		return nil
	}
	// A known expiry auto-use can replace this cycle before either scenario
	// completes. It is not a second independent manual exercise.
	if cfg.ResetCreditsAutoUse && (st.AutoReason == "scheduled" || st.AutoReason == "due") {
		for _, c := range st.Credits {
			if !c.ExpiresAt.After(now) {
				continue
			}
			if at := c.ExpiresAt.Add(-cfg.ResetCreditsAutoUseBefore); c.Supported && at.Before(free) {
				free = at
			}
			break
		}
	}
	optionValue := func(n int, advance, wait float64) float64 {
		return (2 - math.Exp2(1-float64(n))) * advance / (1 + wait/span.Hours())
	}
	// Complement a burst with a steady demand scenario. At r <= F/P, all
	// future refills sustain r continuously; only today's head can force a gap.
	rate := full / span.Hours()
	if finiteADQ(demand) && demand > 0 {
		rate = min(rate, demand)
	}
	window := next.Sub(now).Hours()
	x := min(w, h, rate*window)
	limit := free.Sub(now).Hours()
	if x > 0 && rate > 0 && finiteADQ(rate) {
		before := window + w/rate
		after := window + (w-x)/rate
		if x >= w {
			after = w / rate
		}
		if after < limit && min(limit, before) > after {
			d := p.WeeklyWindow
			if hours := freshWeek / rate; hours < p.WeeklyWindow.Hours() {
				d = time.Duration(hours * float64(time.Hour))
			}
			n := feasibleCardChain(st.Credits, now, now.Add(time.Duration(after*float64(time.Hour))), d, p.WeeklyWindow, lead)
			if n > 0 {
				ready.State, ready.Cards, ready.pacedWork = "progress", n, x
				ready.PacketPercent, ready.EarliestHours = 100*x/m.W, after
				ready.PacedAdvanceHours = min(limit, before) - after
				ready.pacedValue = optionValue(n, ready.PacedAdvanceHours, after)
			}
		}
	}
	fallback := func() *manualCardCycle {
		if ready.State != "" {
			return ready
		}
		return nil
	}
	left := cardTranches(w, full)
	if left > 2+float64(free.Sub(now))/float64(span) {
		return fallback()
	} // impossible, also bounds duration conversion
	packet := w - (left-1)*full
	if packet <= 0 || packet > h+1e-10*full {
		return fallback()
	} // no service in this window can advance the completion date
	wait := time.Duration(0)
	if left > 1 {
		wait = next.Sub(now) + time.Duration(left-2)*span
	}
	first := now.Add(wait)
	if !first.Before(free) {
		return fallback()
	}
	skip := next.Add(time.Duration(left-1) * span)
	if free.Before(skip) {
		skip = free
	}
	advance := skip.Sub(first)
	if advance <= 0 {
		return fallback()
	}
	n := feasibleCardChain(st.Credits, now, first, cycle, p.WeeklyWindow, lead)
	if n == 0 {
		return fallback()
	}
	ready.State, ready.Cards, ready.packet = "advance", n, packet
	ready.PacketPercent, ready.EarliestHours, ready.AdvanceHours = 100*packet/m.W, wait.Hours(), advance.Hours()
	// Discount each subsequent, optional customer exercise by half. Cap the
	// marginal value at 2, and discount opportunities far into the future.
	ready.value = optionValue(n, advance.Hours(), wait.Hours())
	return ready
}

// Retain the previous and current native list across polls: an unexpired card
// disappearing (even with unchanged count) may mean the customer redeemed it.
func manualCardQuotaBarrier(prev, next resetCreditState) time.Time {
	if prev.AuthIndex != next.AuthIndex {
		return time.Time{}
	}
	if !next.Complete || !next.CountKnown || prev.ObservedAt.IsZero() {
		return prev.QuotaAfter
	}
	if !prev.Complete || !prev.CountKnown {
		return next.ObservedAt
	}
	keys := map[string]bool{}
	for _, c := range next.Credits {
		keys[c.Key] = true
	}
	for _, c := range prev.Credits {
		if c.Supported && c.ExpiresAt.After(next.ObservedAt) && !keys[c.Key] {
			return next.ObservedAt
		}
	}
	return prev.QuotaAfter
}

// Fair scheduling chooses the resource class first. Lend only ordinary dual
// turns, never a weekly catch-up turn, a sticky turn or an opportunity tier.
// A donor lends at most 3:1 in predicted work (plus a 3-request startup burst).
type cardLoan struct {
	owner    *balancedAccount
	index    string
	diverted bool
}

func (l cardLoan) commit(cost float64) {
	if l.owner == nil {
		return
	}
	if l.owner.loanIndex != l.index {
		l.owner.loanIndex, l.owner.loanDebt = l.index, 0
	}
	if l.diverted {
		l.owner.loanDebt += cost
	} else {
		l.owner.loanDebt = max(0, l.owner.loanDebt-3*cost)
	}
}

func (s *schedulerRuntimeState) cardCyclePickLocked(base adqDecision, choices []serialCandidate, inputs []adqAccountInput, cost float64, p adqPolicy, now time.Time) (adqDecision, cardLoan) {
	if base.Metrics.WeeklyOnly || len(inputs) < 2 || !capacityDeadlineEnabled(s.cfg) || !s.cfg.PreferResetCredits || cost <= 0 ||
		min(base.Metrics.WEffective, base.Metrics.HEffective) < cost {
		return base, cardLoan{}
	}
	metrics := base.PoolBefore.Accounts
	if len(metrics) != len(inputs) {
		return base, cardLoan{}
	}
	free, _ := s.collectiveHorizonLocked(now)
	liveFull := 0
	for _, m := range metrics {
		if m.Eligible && !m.WeeklyOnly && m.FullWidthContribution == 1 && min(m.HEffective, m.WEffective) >= cost {
			liveFull++
		}
	}
	crosses := func(m adqAccountMetrics) bool { return m.FullWidthContribution == 1 && m.WEffective-cost < m.H-1e-9 }
	var loan cardLoan
	best, score := -1, 0.
	var plan *manualCardCycle
	for i, in := range inputs {
		m, c := metrics[i], choices[i]
		if in.ID == base.AuthID {
			loan = cardLoan{owner: s.balancedAccounts[c.Candidate.ID], index: in.AuthIndex}
		}
		if !m.Eligible || m.WeeklyOnly || !c.Eligible || c.WeeklyProtected || min(m.WEffective, m.HEffective) < cost {
			continue
		}
		end := free
		if !end.IsZero() && s.credentialSawRefillLocked(c, now) {
			end = time.Time{}
		}
		option := manualCycleFor(c.Snapshot, s.quotaNative[in.ID], s.resetCredits[in.ID], m, in.FiveHourCapacity, base.PoolBefore.CurrentDemandPerHour, s.cfg, p, end, now)
		if option == nil || (option.State != "advance" && option.State != "progress") {
			continue
		}
		// Preserve the last complete 5h source whenever the ordinary placement
		// preserves it. Other credible sources let the card account cross F.
		if liveFull == 1 && crosses(m) && !crosses(base.Metrics) {
			continue
		}
		v := option.value/max(cost, option.packet) + option.pacedValue/max(cost, option.pacedWork)
		if v > score || (v == score && best >= 0 && in.ID < inputs[best].ID) {
			best, score, plan = i, v, option
		}
	}
	if best < 0 || loan.owner == nil {
		return base, loan
	}
	if inputs[best].ID != base.AuthID {
		if loan.owner.loanIndex == loan.index && loan.owner.loanDebt+cost > 3*cost+1e-9 {
			return base, loan
		}
		loan.diverted = true
		base.AuthID, base.Metrics = inputs[best].ID, metrics[best]
		base.PoolAfter = adqComputePool(adqProjectedMetrics(metrics, base.AuthID, cost, cost), base.PoolBefore.CurrentDemandPerHour, p, now)
	}
	base.Reason = fmt.Sprintf("手动重置卡：本轮再用约 %.2f%% 周额度，可将到保留线的理论最早时间提前 %.2f 小时；估计可接续 %d 张。", plan.PacketPercent, plan.AdvanceHours, plan.Cards)
	if plan.State == "progress" {
		base.Reason = fmt.Sprintf("手动重置卡：按持续流量情景，本轮使用约 %.2f%% 周额度有望提前 %.2f 小时到保留线。", plan.PacketPercent, plan.PacedAdvanceHours)
	}
	return base, loan
}

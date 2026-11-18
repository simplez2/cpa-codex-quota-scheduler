package main

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func cycleCards(id string, now time.Time, n int) resetCreditState {
	v := resetCreditState{AuthIndex: id, CountKnown: true, Complete: true, Count: n, ObservedAt: now, AutoReason: "disabled"}
	for i := 0; i < n; i++ {
		v.Credits = append(v.Credits, resetCredit{Key: fmt.Sprint(i), Supported: true, ExpiresAt: now.Add(time.Duration(10+i) * 24 * time.Hour)})
	}
	return v
}

// Independent oracle: enumerate exactly how many future tranches are needed
// for every integer choice of work in the current one (16 is not hard-coded by the planner).
func TestManualCardMarginalPacketOracle(t *testing.T) {
	s, _, now := weightedResetFixture(t, true)
	cfg, p := s.cfg, s.adqPolicyLocked()
	cfg.BalancedPriorityPolicy = "capacity_deadline"
	q := s.quotas["five-sooner"]
	r := now.Add(3 * time.Hour)
	for i := range q.Windows {
		if q.Windows[i].Class == "5h" {
			q.Windows[i].ResetAt = r
		} else {
			q.Windows[i].ResetAt = now.Add(6 * 24 * time.Hour)
		}
	}
	st := cycleCards(q.AuthIndex, now, 3)
	for _, f := range []int{12, 16, 20} {
		for w := 1; w <= 98; w++ {
			finish := func(x int) time.Time {
				if x >= w {
					return now
				}
				at := r
				rem := w - x
				for rem > f {
					rem -= f
					at = at.Add(5 * time.Hour)
				}
				return at
			}
			for h := 1; h <= f; h++ {
				m := adqAccountMetrics{W: 100, H: float64(f), WEffective: float64(w), HEffective: float64(h), Eligible: true}
				got := manualCycleFor(q, q, st, m, float64(f), 0, cfg, p, time.Time{}, now)
				first := finish(h)
				packet := 0
				for x := 1; x <= h; x++ {
					if finish(x).Before(finish(0)) {
						packet = x
						break
					}
				}
				if packet == 0 {
					if got != nil && got.State == "advance" {
						t.Fatal(f, w, h, got)
					}
					continue
				}
				if got == nil || math.Abs(got.PacketPercent-float64(packet)) > 1e-8 || got.EarliestHours != first.Sub(now).Hours() || got.AdvanceHours != finish(0).Sub(first).Hours() {
					t.Fatal(f, w, h, packet, got)
				}
			}
		}
	}
}

func TestManualCardEvidenceAndRollover(t *testing.T) {
	s, _, now := weightedResetFixture(t, true)
	cfg, p := s.cfg, s.adqPolicyLocked()
	cfg.BalancedPriorityPolicy = "capacity_deadline"
	q := s.quotas["five-sooner"]
	id := q.AuthIndex
	m := adqAccountMetrics{W: 100, H: 16, WEffective: 34, HEffective: 10, Eligible: true}
	test := func(v resetCreditState, group time.Time) *manualCardCycle {
		return manualCycleFor(q, q, v, m, 16, 0, cfg, p, group, now)
	}
	st := cycleCards(id, now, 3)
	for _, f := range []func(*resetCreditState){
		func(v *resetCreditState) { v.AuthIndex = "same-workspace-other-credential" }, func(v *resetCreditState) { v.Error = "upstream" }, func(v *resetCreditState) { v.Complete = false },
		func(v *resetCreditState) { v.CountKnown = false }, func(v *resetCreditState) { v.ObservedAt = now.Add(-resetCreditsCacheTTL - time.Second) },
		func(v *resetCreditState) { v.LastAction = "confirmed"; v.ActionAt = now.Add(-time.Minute) }, func(v *resetCreditState) { v.Credits[0].Supported = false },
		func(v *resetCreditState) { v.Credits = nil }, func(v *resetCreditState) { v.AutoReason = "previous_attempt_requires_review" },
		func(v *resetCreditState) { v.QuotaAfter = now }, func(v *resetCreditState) { v.ObservedAt = now.Add(time.Minute) },
	} {
		v := cycleCards(id, now, 3)
		f(&v)
		if test(v, time.Time{}) != nil {
			t.Fatalf("accepted %+v", v)
		}
	}
	if test(st, now.Add(time.Hour)) != nil {
		t.Fatal("forecast can precede first depletion")
	}
	q.ResetCreditsKnown, q.ResetCredits, q.ResetCreditsObservedAt = true, 1, now.Add(time.Second)
	if test(st, time.Time{}) != nil {
		t.Fatal("conflicting native count")
	}
	q.ResetCreditsKnown = false
	v := cycleCards(id, now, 3)
	v.Credits[0].ExpiresAt = now.Add(time.Hour)
	v.Credits[1].ExpiresAt = now.Add(12 * time.Hour)
	v.Credits[2].ExpiresAt = now.Add(15 * time.Hour)
	if a := test(v, time.Time{}); a == nil || a.Cards != 1 {
		t.Fatal("count must match sequential exercise times after advancing cycles", a)
	}
	cfg.PreferResetCredits = false
	if test(st, time.Time{}) != nil {
		t.Fatal("toggle ignored")
	}
	cfg.PreferResetCredits = true
	fresh := cycleCards(id, now.Add(time.Minute), 3)
	fresh.Credits[0].Key = "replacement"
	if !manualCardQuotaBarrier(st, fresh).Equal(fresh.ObservedAt) {
		t.Fatal("card replacement of identical count missed")
	}
	m.WEffective = 0
	if test(st, time.Time{}) != nil {
		t.Fatal("predicted depletion mislabeled as confirmed")
	}
	for i := range q.Windows {
		if q.Windows[i].Class == "weekly" {
			q.Windows[i].UsedPercent = 100
		}
	}
	if a := test(st, time.Time{}); a == nil || a.State != "at_reserve" {
		t.Fatal("genuine native exhaustion should be surfaced", a)
	}
	if a := test(st, now.Add(time.Hour)); a == nil || a.State != "free_reset_near" {
		t.Fatal(a)
	}
}

type cardReplayResult struct{ first, served, owned, other, flex, exercised int }

// Replay the whole picker with identical arrivals. Fake provider snapshots
// debit both dimensions. Optional modeled customer exercises replace both clocks.
func cardReplay(t *testing.T, prefer bool, demand []int, stock, richCards, otherCards int, flexH int, exerciseFrom ...int) cardReplayResult {
	t.Helper()
	s, req, start := weightedResetFixture(t, true)
	s.cfg.BalancedPriorityPolicy = "capacity_deadline"
	s.cfg.PreferResetCredits = prefer
	s.cfg.WeeklyOnlyPriorityWindow = 24 * time.Hour
	s.cfg.ReserveWeeklyPercent = 2
	setUnitCost(s)
	s.resetCredits = map[string]resetCreditState{}
	ids := []string{"five-sooner", "five-later", "weekly-backup"}
	if flexH == 0 {
		ids = ids[:2]
	}
	req.Candidates = nil
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: providerCodex})
	}
	w := [3]int{stock, stock, 70}
	h := [2]int{16, 16}
	next, free, cards := [2]int{5, 5}, [3]int{144, 144, flexH}, [2]int{richCards, otherCards}
	out := cardReplayResult{first: -1}
	redeem := func(i, tick int) {
		if len(exerciseFrom) > 0 && tick >= exerciseFrom[0] && i < 2 && cards[i] > 0 && w[i] == 0 {
			w[i], h[i], next[i], free[i] = 98, 16, tick+5, tick+168
			cards[i]--
			out.exercised++
		}
	}
	for tick, n := range demand {
		for i := range next {
			if tick == next[i] {
				h[i] = 16
				next[i] += 5
			}
		}
		for j := 0; j < n; j++ {
			for i := range cards {
				redeem(i, tick)
			}
			now := start.Add(time.Duration(tick)*time.Hour + time.Duration(1+j)*time.Second)
			for i, id := range ids {
				q := s.quotas[id]
				q.RefreshedAt, q.FiveHourLimitObservedAt = now, now
				for k := range q.Windows {
					z := &q.Windows[k]
					z.ObservedAt = now
					if z.Class == "weekly" {
						z.UsedPercent = 98 - float64(w[i])
						z.ResetAt = start.Add(time.Duration(free[i]) * time.Hour)
					} else {
						z.UsedPercent = 100 * (1 - float64(h[i])/16)
						z.ResetAt = start.Add(time.Duration(next[i]) * time.Hour)
					}
				}
				s.quotas[id], s.quotaNative[id] = q, q
				if i < 2 {
					s.resetCredits[id] = cycleCards(id, now, cards[i])
				}
			}
			got := s.balancedPick(req, now)
			if got.AuthID == "" {
				continue
			}
			i := 0
			for i < len(ids) && ids[i] != got.AuthID {
				i++
			}
			if i == len(ids) || w[i] <= 0 || (i < 2 && h[i] <= 0) {
				t.Fatal("unsafe replay", tick, h, w, got)
			}
			w[i]--
			if i < 2 {
				h[i]--
			}
			out.served++
			switch i {
			case 0:
				out.owned++
				if w[0] == 0 && out.first < 0 {
					out.first = tick
				}
			case 1:
				out.other++
			case 2:
				out.flex++
			}
			redeem(i, tick)
			s.adqReservations.Release(s.adqLastDecision.ReservationID)
			s.balancedAccounts[got.AuthID].Pending = nil
		}
	}
	return out
}

func TestManualCardRoutingReplay(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, tc := range []struct {
		name                                     string
		perHour, hours, stock, otherCards, flexH int
	}{
		{"light", 2, 48, 34, 1, 0}, {"normal", 4, 35, 50, 1, 0}, {"saturated", 12, 17, 34, 1, 0}, {"weekly-sprint", 3, 8, 34, 1, 12}, {"weekly-catchup", 5, 4, 50, 1, 28}, {"no-other-cards", 3, 56, 98, 0, 0},
	} {
		d := make([]int, tc.hours)
		for i := range d {
			d[i] = tc.perHour
		}
		a := cardReplay(t, false, d, tc.stock, 3, tc.otherCards, tc.flexH)
		b := cardReplay(t, true, d, tc.stock, 3, tc.otherCards, tc.flexH)
		t.Logf("CARD %s first_rich_h=%d/%d served=%d/%d other=%d/%d flex=%d/%d", tc.name, a.first, b.first, a.served, b.served, a.other, b.other, a.flex, b.flex)
		if b.served != a.served || (tc.flexH > 0 && b.flex != a.flex) {
			t.Fatal("lost service or weekly allocations", tc, a, b)
		}
		if tc.name == "light" && (b.first < 0 || b.first >= a.first) {
			t.Fatal("no improvement", a, b)
		}
		if tc.name == "weekly-sprint" && b.flex != b.served {
			t.Fatal("ignored urgent weekly-only account")
		}
		if tc.name == "weekly-catchup" && (b.flex == 0 || b.flex == b.served || b.other == 0) {
			t.Fatal("lost ordinary pooled shares", b)
		}
	}
}

func TestManualCardExerciseReplay(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, last := range []int{19, 24, 29, 34} {
		d := make([]int, last+1)
		for i := range d {
			d[i] = 2
		}
		d[last] = 35
		a, b := cardReplay(t, false, d, 34, 3, 0, 0, 0), cardReplay(t, true, d, 34, 3, 0, 0, 0)
		held := cardReplay(t, true, d, 34, 3, 0, 0, last)
		t.Logf("EXERCISE burst_h=%d first_rich_h=%d/%d served=%d/%d synthetic_cards=%d/%d new_held_until_burst=%d", last, a.first, b.first, a.served, b.served, a.exercised, b.exercised, held.served)
		if a.exercised > 3 || b.exercised > 3 || (a.served > 68+98*a.exercised) || (b.served > 68+98*b.exercised) {
			t.Fatal("credited unavailable work", a, b)
		}
	}
}

func TestManualCardBurstSensitivity(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	win, loss, tie, old, newServed, early, late, oldCards, newCards := 0, 0, 0, 0, 0, 0, 0, 0, 0
	equalN, equalOld, equalNew := 0, 0, 0
	for last := 15; last < 35; last++ {
		for _, pace := range []int{2, 3, 4} {
			for _, burst := range []int{16, 35} {
				d := make([]int, last+1)
				for i := range d {
					d[i] = pace
				}
				d[last] = burst
				a, b := cardReplay(t, false, d, 34, 3, 0, 0, 0), cardReplay(t, true, d, 34, 3, 0, 0, 0)
				old += a.served
				newServed += b.served
				oldCards += a.exercised
				newCards += b.exercised
				if a.exercised == b.exercised {
					equalN++
					equalOld += a.served
					equalNew += b.served
				}
				switch {
				case a.served < b.served:
					win++
				case a.served > b.served:
					loss++
				default:
					tie++
				}
				at, bt := a.first, b.first
				if at < 0 {
					at = len(d)
				}
				if bt < 0 {
					bt = len(d)
				}
				if bt < at {
					early++
				} else if bt > at {
					late++
				}
			}
		}
	}
	t.Logf("CARD_GRID scenarios=%d higher/same/lower=%d/%d/%d served=%d/%d rich_earlier/later=%d/%d synthetic_cards=%d/%d", win+tie+loss, win, tie, loss, old, newServed, early, late, oldCards, newCards)
	t.Logf("CARD_GRID_EQUAL_EXERCISES cases=%d served=%d/%d", equalN, equalOld, equalNew)
	if early < 90 || late > 0 || newServed < old || equalN == 0 {
		t.Fatal("card readiness or aggregate service regression on the fixed traces")
	}
}

func TestManualCardBothNewAnchorsRequired(t *testing.T) {
	s, _, now := weightedResetFixture(t, true)
	cfg, p := s.cfg, s.adqPolicyLocked()
	cfg.BalancedPriorityPolicy = "capacity_deadline"
	cfg.ReserveWeeklyPercent = 2
	q := s.quotas["five-sooner"]
	st := cycleCards(q.AuthIndex, now, 2)
	st.QuotaAfter = now.Add(-time.Minute)
	m := adqAccountMetrics{W: 100, H: 16, WEffective: 98, HEffective: 16, Eligible: true}
	for i := range q.Windows {
		z := &q.Windows[i]
		z.UsedPercent = 0
		z.ObservedAt = now
		if z.Class == "5h" {
			z.ResetAt = now.Add(5 * time.Hour)
		} else {
			z.ResetAt = now.Add(7 * 24 * time.Hour)
		}
	}
	native := q
	native.Windows = append([]quotaWindow(nil), q.Windows...)
	for i := range native.Windows {
		if native.Windows[i].Class == "5h" {
			native.Windows[i].ObservedAt = st.QuotaAfter.Add(-time.Second)
		}
	}
	if manualCycleFor(q, native, st, m, 16, 0, cfg, p, time.Time{}, now) != nil {
		t.Fatal("one new clock isn't a confirmed refill")
	}
	if got := manualCycleFor(q, q, st, m, 16, 0, cfg, p, time.Time{}, now); got == nil || got.EarliestHours != 30 || got.FreeHours != 168 || got.Cards != 2 {
		t.Fatal("expected the new weekly cycle and six intervening 5h windows", got)
	}
}

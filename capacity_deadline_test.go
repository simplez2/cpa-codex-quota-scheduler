package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func setUnitCost(s *schedulerRuntimeState) {
	s.globalCostSamples = make([]float64, minimumQuantileSamples)
	for i := range s.globalCostSamples {
		s.globalCostSamples[i] = 1
	}
}

func TestCapacityTrancheAndForecastBoundaries(t *testing.T) {
	now := time.Now()
	next := now.Add(time.Hour)
	for _, c := range []struct {
		w, h float64
		span time.Duration
		want float64
	}{
		{60, 16, 10 * time.Hour, 16}, {32, 16, 10 * time.Hour, 0}, {35, 16, 10 * time.Hour, 3},
		{35, 16, 10*time.Hour + 1, 0}, {6, 12, 5 * 24 * time.Hour, 0}, {60, 16, 0, 0},
	} {
		if got := unavoidableTranche(c.w, c.h, 16, next, next.Add(c.span), 5*time.Hour); got != c.want {
			t.Fatalf("%+v: %v", c, got)
		}
	}
	early := "2026-11-19T09:00:00+08:00"
	late := "2026-11-19T11:00:00+08:00"
	raw := early + "/" + late
	changes, fields := validatePanelSettings(nil, map[string]any{"balanced_priority_policy": "capacity_deadline", "collective_reset_window": raw})
	if len(fields) > 0 || changes["collective_reset_window"] != raw {
		t.Fatalf("invalid roundtrip %+v %+v", changes, fields)
	}
	for _, x := range []string{late + "/" + early, early, early + "/2026-11-22T05:00:00Z", "2026-11-19T09:00:00/" + late} {
		if _, _, ok := parseCollectiveWindow(x); ok {
			t.Fatal("accepted invalid interval", x)
		}
	}
	s := &schedulerRuntimeState{}
	s.cfg.collectiveEarliest, s.cfg.collectiveLatest, _ = parseCollectiveWindow(raw)
	a, b := s.cfg.collectiveEarliest, s.cfg.collectiveLatest
	for _, c := range []struct {
		at          time.Time
		open, burst bool
	}{{a.Add(-6 * time.Hour), true, false}, {a.Add(-5 * time.Hour), true, true}, {a, true, true}, {b.Add(-1), true, true}, {b, false, false}} {
		at, burst := s.collectiveHorizonLocked(c.at)
		if (!at.IsZero()) != c.open || burst != c.burst {
			t.Fatal(c, at, burst)
		}
	}
	s.quotaEpoch.Reason, s.quotaEpoch.ConfirmedAt = "bootstrap", a
	if _, ok := s.collectiveHorizonLocked(a); !ok {
		t.Fatal("bootstrap treated as a reset")
	}
	s.quotaEpoch.Reason = "provider_reset"
	if at, _ := s.collectiveHorizonLocked(a); !at.IsZero() {
		t.Fatal("continued sprint after observed group reset")
	}
}

func TestCapacityNaturalWeeklyMigrationAndNoBounce(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, last := weeklyMigrationFixture(t)
	s.cfg.BalancedPriorityPolicy = "capacity_deadline"
	setMigrationWindow(s, weeklyMigrationSource, "weekly", func(w *quotaWindow) { w.UsedPercent = 92; w.ResetAt = last.Add(5 * 24 * time.Hour) })
	if p := s.balancedPick(req, last.Add(time.Minute)); p.AuthID != weeklyMigrationSource {
		t.Fatal("lost warm cache", p)
	}
	s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: weeklyMigrationSource, AuthIndex: weeklyMigrationSource, Model: req.Model, RequestedAt: last.Add(time.Minute)}, last.Add(2*time.Minute))
	at := last.Add(time.Hour)
	if p := s.concurrencyPreferred(req, s.cfg, at); p != "" {
		t.Fatal("old 5h queue prevented migration", p)
	}
	got := s.balancedPick(req, at)
	if !migrationTarget(got.AuthID) {
		t.Fatal("did not spend naturally expiring weekly stock", got)
	}
	setMigrationWindow(s, weeklyMigrationSource, "5h", func(w *quotaWindow) { w.UsedPercent = 0; w.ResetAt = at.Add(5 * time.Hour) })
	if p := s.balancedPick(req, at.Add(time.Minute)); p.AuthID != got.AuthID {
		t.Fatal("5h refill pulled back weekly conversation", p)
	}
}

func TestCapacitySoftEventsAndCardsAreNotSupply(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := weightedResetFixture(t, true)
	s.cfg.BalancedPriorityPolicy = "capacity_deadline"
	s.cfg.WeeklyOnlyPriorityWindow = 24 * time.Hour
	setUnitCost(s)
	for _, c := range req.Candidates {
		setMigrationWindow(s, c.ID, "weekly", func(w *quotaWindow) { w.ResetAt = now.Add(6 * 24 * time.Hour) })
	}
	id := "weekly-backup"
	card := resetCreditState{AuthIndex: id, ObservedAt: now, CountKnown: true, Count: 1, Complete: true, AutoReason: "scheduled", Credits: []resetCredit{{Supported: true, ExpiresAt: now.Add(50 * time.Minute)}}}
	s.resetCredits = map[string]resetCreditState{id: card}
	s.cfg.ResetCreditsAutoUse = false
	if p := s.balancedPick(req, now); p.AuthID == id {
		t.Fatal("unredeemed manual card changed placement")
	}
	s.cfg.ResetCreditsAutoUse = true
	if p := s.balancedPick(req, now); p.AuthID != id || !strings.Contains(s.adqLastDecision.Reason, "用卡") {
		t.Fatalf("known local cutover missed: %+v %s", p, s.adqLastDecision.Reason)
	}
	card.AuthIndex = "replacement"
	s.resetCredits[id] = card
	if p := s.balancedPick(req, now); p.AuthID == id {
		t.Fatal("card crossed credential identity")
	}
	s.cfg.collectiveEarliest, s.cfg.collectiveLatest = now.Add(time.Hour), now.Add(3*time.Hour)
	for _, c := range req.Candidates {
		setMigrationWindow(s, c.ID, "weekly", func(w *quotaWindow) { w.UsedPercent = 98 })
	}
	if p := s.balancedPick(req, now); p.AuthID != "" {
		t.Fatalf("forecast created spendable quota: %+v", p)
	}
	setMigrationWindow(s, id, "weekly", func(w *quotaWindow) { w.UsedPercent = 12 })
	c := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: id}, s.quotas[id], true, s.cfg, now)
	if !s.capacityFlexHoldLocked(c, now) {
		t.Fatal("flexible campaign binding was not retained")
	}
	card.AuthIndex = id
	card.LastAction = "verified"
	card.ActionAt = now
	s.resetCredits[id] = card
	if s.capacityFlexHoldLocked(c, now) {
		t.Fatal("continued speculative sprint on already refilled credential")
	}
}

func TestCapacityEarlierRealLossNeedsWeeklySlack(t *testing.T) {
	s, req, now := urgentWeeklyFixture(t)
	s.cfg.BalancedPriorityPolicy = "capacity_deadline"
	setUnitCost(s)
	dual := "five-later"
	flex := "weekly-backup"
	setMigrationWindow(s, dual, "weekly", func(w *quotaWindow) { w.ResetAt = now.Add(7 * time.Hour); w.UsedPercent = 20 })
	setMigrationWindow(s, dual, "5h", func(w *quotaWindow) { w.ResetAt = now.Add(time.Hour); w.UsedPercent = 20 })
	setMigrationWindow(s, flex, "weekly", func(w *quotaWindow) { w.ResetAt = now.Add(10 * time.Hour); w.UsedPercent = 93 })
	var rows []serialCandidate
	for _, c := range req.Candidates {
		if c.ID == dual || c.ID == flex {
			rows = append(rows, inspectSerialCandidate(c, s.quotas[c.ID], true, s.cfg, now))
		}
	}
	check := func(want adqAllocationMode) {
		t.Helper()
		out := s.placementTiersLocked(rows, 1, now, "")
		if len(out) == 0 || out[0].mode != want {
			t.Fatalf("want tier %d, got %+v", want, out)
		}
	}
	check(adqAllocateWeeklyDeadline)
	o := quotaRunwayObservation{observedAt: now, resetAt: now.Add(10 * time.Hour)}
	for i := 10; i > 0; i-- {
		a := now.Add(-time.Duration(i) * time.Minute)
		o.intervals = append(o.intervals, capacityInterval{a, a.Add(time.Minute), 1})
	}
	key := quotaRunwayKey{flex, flex, "weekly"}
	s.rotationRunway.bindings = map[string]string{flex: flex}
	s.rotationRunway.windows = map[quotaRunwayKey]quotaRunwayObservation{key: o}
	check(adqAllocateOpportunity)
	for i := 5; i < 10; i++ {
		o.intervals[i].delta = 0
	}
	s.rotationRunway.windows[key] = o
	check(adqAllocateWeeklyDeadline)
}

func TestCapacityForecastProbeHonorsRealDemandAndBackoff(t *testing.T) {
	s, _, now := weightedResetFixture(t, true)
	s.cfg.BalancedPriorityPolicy = "capacity_deadline"
	s.cfg.WeeklyOnlyPriorityWindow = 0
	s.cfg.WeeklyOnlyCatchupSharePercent = 0
	id := "weekly-backup"
	q := s.quotas[id]
	q.RefreshedAt = now.Add(-20 * time.Minute)
	q.FiveHourLimitObservedAt = q.RefreshedAt
	q.Windows[0].ResetAt = now.Add(5 * 24 * time.Hour)
	q.Windows[0].ObservedAt = q.RefreshedAt
	s.quotas[id] = q
	s.quotaPolls = map[string]quotaPollState{id: {AuthIndex: q.AuthIndex, AttemptedAt: q.RefreshedAt, NextAt: now.Add(10 * time.Minute)}}
	s.requestWeeklyCatchupProbesLocked(now)
	if s.quotaPolls[id].CatchupPending {
		t.Fatal("created demand with no forecast and no natural urgency")
	}
	s.cfg.collectiveEarliest, s.cfg.collectiveLatest = now.Add(time.Hour), now.Add(2*time.Hour)
	s.requestWeeklyCatchupProbesLocked(now)
	if s.quotaProbeReason(id, now) != "weekly_catchup" {
		t.Fatal("standby never woken during genuine foreground campaign demand")
	}
	p := s.quotaPolls[id]
	p.Failures, p.Error = 1, "quota_http_429"
	s.quotaPolls[id] = p
	if got := s.quotaProbeReason(id, now); got != "" {
		t.Fatal("forecast overrode provider backoff", got)
	}
}

// Finite, indivisible real jobs; two stocks, only one has a renewing 5h cap.
// Enumerate every feasible hourly assignment with perfect future knowledge.
// Actual policy measurements below call balancedPick, not a reimplementation.
type mixedStudy struct {
	name                               string
	d                                  []int
	week, head, flex, first, flexReset int
	announced, latest                  int
}

func studyOracle(c mixedStudy) int {
	type key struct{ t, w, h, f int }
	memo := map[key]int{}
	var solve func(int, int, int, int) int
	solve = func(t, w, h, f int) int {
		if t == len(c.d) {
			return 0
		}
		if t >= c.first && (t-c.first)%5 == 0 {
			h = 16
		}
		k := key{t, w, h, f}
		if v, ok := memo[k]; ok {
			return v
		}
		v := 0
		for x := 0; x <= min(c.d[t], w, h); x++ {
			for y := 0; y <= min(c.d[t]-x, f); y++ {
				v = max(v, x+y+solve(t+1, w-x, h-x, f-y))
			}
		}
		memo[k] = v
		return v
	}
	return solve(0, c.week, c.head, c.flex)
}

type studyResult struct{ served, dual, flex, oldWeek, earlyDual int }

func runMixedStudy(t *testing.T, c mixedStudy, policy string) studyResult {
	s, req, start := weightedResetFixture(t, true)
	s.cfg.BalancedPriorityPolicy = policy
	s.cfg.WeeklyOnlyPriorityWindow = 24 * time.Hour
	s.cfg.ReserveWeeklyPercent = 2
	setUnitCost(s)
	req.Candidates = nil
	dual, flex := "five-sooner", "weekly-backup"
	for _, id := range []string{dual, flex} {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: providerCodex})
	}
	if c.announced > 0 {
		s.cfg.collectiveEarliest = start.Add(time.Duration(c.announced) * time.Hour)
		s.cfg.collectiveLatest = start.Add(time.Duration(c.latest) * time.Hour)
	}
	w, h, f := c.week, c.head, c.flex
	next := c.first
	out := studyResult{}
	for tick, d := range c.d {
		if tick == next {
			h = 16
			next += 5
		}
		for j := 0; j < d; j++ {
			now := start.Add(time.Duration(tick)*time.Hour + time.Duration(j+1)*time.Second)
			for _, id := range []string{dual, flex} {
				q := s.quotas[id]
				q.RefreshedAt, q.FiveHourLimitObservedAt = now, now
				for i := range q.Windows {
					z := &q.Windows[i]
					z.ObservedAt = now
					switch {
					case z.Class == "5h":
						z.UsedPercent = 100 * (1 - float64(h)/16)
						z.ResetAt = start.Add(time.Duration(next) * time.Hour)
					case id == dual:
						z.UsedPercent = 98 - float64(w)
						z.ResetAt = start.Add(144 * time.Hour)
					default:
						z.UsedPercent = 98 - float64(f)
						z.ResetAt = start.Add(time.Duration(c.flexReset) * time.Hour)
					}
				}
				s.quotas[id], s.quotaNative[id] = q, q
			}
			pick := s.balancedPick(req, now)
			switch pick.AuthID {
			case dual:
				w--
				h--
				out.dual++
				if tick < c.first {
					out.earlyDual++
				}
			case flex:
				f--
				out.flex++
			case "":
				continue
			default:
				t.Fatal("unexpected choice", pick)
			}
			if w < 0 || h < 0 || f < 0 || math.Abs(s.adqLastDecision.ReservationWeekly-1) > 1e-9 {
				t.Fatal("admitted without available unit", policy, c.name, w, h, f)
			}
			out.served++
			s.adqReservations.Release(s.adqLastDecision.ReservationID)
			s.balancedAccounts[pick.AuthID].Pending = nil
		}
	}
	out.oldWeek = f
	return out
}
func TestCapacityMixedBenchmark(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	cases := []mixedStudy{
		{"quiet-then-burst", []int{8, 0, 0, 18, 20}, 40, 8, 16, 3, 32, 5, 6},
		{"natural-12h-low-dual-week", []int{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3}, 6, 12, 40, 2, 12, 0, 0},
		{"surprise-no-forecast", []int{4, 0, 0, 12, 14}, 30, 8, 16, 3, 140, 0, 0},
		{"early-vs-forecast", []int{7, 0, 19}, 40, 7, 17, 2, 36, 4, 6},
		{"forecast-is-late", []int{3, 2, 4, 6, 1, 10, 20}, 46, 8, 24, 3, 35, 4, 7},
		{"forecast-never-happens", []int{4, 1, 2, 0, 0, 0, 16, 16}, 40, 8, 12, 3, 100, 2, 4},
	}
	for _, c := range cases {
		old := runMixedStudy(t, c, "reset_weighted")
		n := runMixedStudy(t, c, "capacity_deadline")
		best := studyOracle(c)
		if n.served > best || old.served > best {
			t.Fatal("oracle violated", c.name)
		}
		if c.name == "natural-12h-low-dual-week" && n.flex != n.served {
			t.Fatal("natural priority lost", n)
		}
		if (c.name == "quiet-then-burst" || c.name == "forecast-never-happens") && n.served != best {
			t.Fatal("lost a recoverable 5h tranche", c.name, n, best)
		}
		t.Log(fmt.Sprintf("MIXED %s legacy=%d new=%d oracle=%d dual_before_first=%d/%d old_flex_left=%d/%d", c.name, old.served, n.served, best, old.earlyDual, n.earlyDual, old.oldWeek, n.oldWeek))
	}
}

func TestCapacityMixedSensitivity(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	var count, oldOptimal, newOptimal, wins, losses, oldRegret, newRegret, worst int
	for _, first := range []int{2, 4} {
		for _, head := range []int{4, 9} {
			for _, stock := range []int{10, 38} {
				for _, f := range []int{6, 18} {
					for _, ann := range []int{0, 2, 5} {
						for _, load := range [][]int{{2, 3, 0, 3, 9, 18}, {4, 0, 10, 0, 0, 20}, {1, 1, 1, 1, 1, 1}} {
							c := mixedStudy{"grid", load, stock, head, f, first, 36, ann, ann + 2}
							opt := studyOracle(c)
							a := runMixedStudy(t, c, "reset_weighted").served
							b := runMixedStudy(t, c, "capacity_deadline").served
							if a > opt || b > opt {
								t.Fatal("exceeded oracle", c, a, b, opt)
							}
							count++
							if a == opt {
								oldOptimal++
							}
							if b == opt {
								newOptimal++
							}
							if b > a {
								wins++
							}
							if b < a {
								losses++
							}
							oldRegret += opt - a
							newRegret += opt - b
							worst = max(worst, opt-b)
						}
					}
				}
			}
		}
	}
	t.Logf("GRID cases=%d legacy_optimal=%d new_optimal=%d wins=%d losses=%d legacy_unserved_vs_oracle=%d new_unserved_vs_oracle=%d worst_new_gap=%d", count, oldOptimal, newOptimal, wins, losses, oldRegret, newRegret, worst)
}

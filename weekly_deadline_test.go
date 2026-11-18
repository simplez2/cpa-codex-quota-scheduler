package main

import (
	"math"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestWeeklyDeadlineChoosesEarliestSpendableGroup(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := deadlinePaceFixture(t)
	s.globalCostSamples = make([]float64, minimumQuantileSamples)
	for i := range s.globalCostSamples {
		s.globalCostSamples[i] = 0.4
	}
	setMigrationWindow(s, "weekly-backup", "weekly", func(w *quotaWindow) {
		w.UsedPercent, w.ResetAt = 97, now.Add(time.Hour)
	})
	// The sooner account has only one spendable unit, while its peer has 60.
	if got := s.balancedPick(req, now); got.AuthID != "weekly-backup" {
		t.Fatalf("large later allowance preempted the first deadline: %+v", got)
	}
	setMigrationWindow(s, "weekly-backup", "weekly", func(w *quotaWindow) { w.UsedPercent = 97.9999 })
	if got := s.balancedPick(req, now); got.AuthID != "weekly-second" {
		t.Fatalf("too-small early offer hid a later spendable weekly offer: %+v", got)
	}
	// An almost simultaneous pair shares a group when distant. Close to
	// reset the same gap must no longer let the later credential compete.
	gap := 124 * time.Second
	if weeklyDeadlineSlack(11*time.Hour) < gap || weeklyDeadlineSlack(20*time.Minute) >= gap ||
		weeklyDeadlineSlack(23*time.Hour) > 30*time.Minute {
		t.Fatal("deadline group did not contract near reset or exceeded its cap")
	}
}

func TestWeeklyDeadlineAccountsForForcedWorkBeforePlacingFlexibleTurns(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := urgentWeeklyFixture(t)
	for _, id := range []string{"weekly-backup", "weekly-second"} {
		setMigrationWindow(s, id, "weekly", func(w *quotaWindow) {
			w.UsedPercent, w.ResetAt = 10, now.Add(12*time.Hour)
		})
	}
	pinned := withBalancedSession(req, "pinned-thread")
	pinned.Options.Metadata["pinned_auth_id"] = "weekly-backup"
	var lastCost float64
	for i := 0; i < 16; i++ {
		if p := s.balancedPick(pinned, now); p.AuthID != "weekly-backup" {
			t.Fatalf("lost a pinned turn: %+v", p)
		}
		lastCost = s.adqLastDecision.ReservationWeekly
	}
	for i := 0; i < 16; i++ {
		if p := s.balancedPick(req, now); p.AuthID != "weekly-second" {
			t.Fatalf("flexible turn %d repeated already-forced work: %+v", i, p)
		}
	}
	status := s.balancedStatusLocked(now)
	if math.Abs(status["weekly-backup"].WeeklyRecentWork-16*lastCost) > 1e-8 ||
		math.Abs(status["weekly-second"].WeeklyRecentWork-16*lastCost) > 1e-8 {
		t.Fatalf("recent service did not include exactly the admitted work: %+v", status)
	}
}

func TestWeeklyServiceSettlementCancellationAndIdentity(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := urgentWeeklyFixture(t)
	s.cfg.StaleAfter = 2 * time.Hour
	first := withBalancedSession(req, "single")
	first.Options.Headers = map[string][]string{concurrencyHeader: {"cancel-before-dispatch"}}
	pick := s.balancedPick(first, now)
	account := s.balancedAccounts[pick.AuthID]
	epoch := account.weeklyService.epoch
	if account.weeklyService.atTime(epoch, now) <= 0 {
		t.Fatal("missing admitted service")
	}
	s.rollbackUndispatchedConcurrency("cancel-before-dispatch")
	if v := account.weeklyService.atTime(epoch, now); v > 1e-8 {
		t.Fatalf("undispatched work was charged: %v", v)
	}
	s.pricing = map[string]modelPricing{req.Model: {Model: req.Model, CompletionPricePer1M: 1, PriceMultiplier: 1}}
	origin := now.Add(time.Second)
	first.Options.Headers = nil
	pick = s.balancedPick(first, origin)
	account = s.balancedAccounts[pick.AuthID]
	epoch = account.weeklyService.epoch
	done := origin.Add(weeklyServiceHalfLife)
	record := pluginapi.UsageRecord{AuthID: pick.AuthID, AuthIndex: epoch.index, Model: req.Model, RequestedAt: origin,
		Detail: pluginapi.UsageDetail{OutputTokens: 2_000_000}}
	s.observeBalancedUsage(record, done)
	s.observeBalancedUsage(record, done)
	if got := account.weeklyService.atTime(epoch, done); math.Abs(got-1) > 1e-7 {
		t.Fatalf("late or duplicate usage changed discounted actual work: %v", got)
	}
	later := done.Add(time.Second)
	if p := s.balancedPick(first, later); p.AuthID != pick.AuthID {
		t.Fatalf("lost urgent continuity before failure: %+v", p)
	}
	record.RequestedAt, record.Failed = later, true
	s.observeBalancedUsage(record, later)
	if got := account.weeklyService.atTime(epoch, later); math.Abs(got-weeklyServiceDiscount(time.Second)) > 1e-7 {
		t.Fatalf("failed request still charged its prediction or tokens: %v", got)
	}
	next := weeklyServiceEpoch{epoch.index, epoch.reset.Add(7 * 24 * time.Hour)}
	account.weeklyService.assign(next, 3, done)
	account.weeklyService.correct(epoch, -2, origin, done)
	if got := account.weeklyService.atTime(next, done); got != 3 {
		t.Fatalf("a prior cycle charged the new one: %v", got)
	}
	if got := account.weeklyService.atTime(weeklyServiceEpoch{"replacement", next.reset}, done); got != 0 {
		t.Fatalf("a replacement credential inherited service: %v", got)
	}
}

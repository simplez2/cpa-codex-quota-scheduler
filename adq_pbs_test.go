package main

import (
	"sync"
	"testing"
	"time"
)

func TestADQWeeklyHardGateAndEffectiveQuota(t *testing.T) {
	p := defaultADQPolicy()
	in := adqAccountInput{ID: "dead", WeeklyCapacity: 100, WeeklyRemaining: 0, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy}
	m := adqAssessAccount(in, p, time.Now())
	if m.Eligible || m.State != "ACCOUNT_DEAD" || m.FullWidthContribution != 0 {
		t.Fatalf("weekly hard gate failed: %#v", m)
	}
	in.WeeklyRemaining = 100
	in.FiveHourRemaining = 16
	in.ReservationWeekly = 100
	m = adqAssessAccount(in, p, time.Now())
	if m.Eligible || m.WEffective != 0 {
		t.Fatalf("reservation did not enforce weekly gate: %#v", m)
	}
}

func TestADQWidthAndProjectedLeximin(t *testing.T) {
	p := defaultADQPolicy()
	now := time.Now()
	inputs := []adqAccountInput{
		{ID: "a", WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy, BurnMean: 1, BurnP95: 1.2},
		{ID: "b", WeeklyCapacity: 100, WeeklyRemaining: 50, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy, BurnMean: 1, BurnP95: 1.2},
		{ID: "c", WeeklyCapacity: 100, WeeklyRemaining: 8, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy, BurnMean: 1, BurnP95: 1.2},
	}
	ms := make([]adqAccountMetrics, 0, len(inputs))
	for _, in := range inputs {
		ms = append(ms, adqAssessAccount(in, p, now))
	}
	pool := adqComputePool(ms, 0, p, now)
	if pool.FullWidth != 2 {
		t.Fatalf("FullWidth=%d want 2", pool.FullWidth)
	}
	if pool.EffectiveWidth <= 2 || pool.EffectiveWidth >= 3 {
		t.Fatalf("EffectiveWidth=%v", pool.EffectiveWidth)
	}
	d, ok := adqChoose(inputs, 0, p, now)
	if !ok {
		t.Fatal("no candidate")
	}
	if d.AuthID == "c" {
		t.Fatalf("tail candidate should not win projected leximin: %#v", d)
	}
}

func TestADQFillProbabilityAndLockSLA(t *testing.T) {
	if got := adqFillProbability(0, 5*time.Hour, 0, 0); got != 1 {
		t.Fatalf("zero remaining p=%v", got)
	}
	if got := adqFillProbability(10, 0, 1, 2); got != 0 {
		t.Fatalf("zero horizon p=%v", got)
	}
	if got := adqLockSLAProbability(.95, 6); got < .39 || got > .40 {
		t.Fatalf("sla=%v", got)
	}
	if got := adqLockCost(16, 0, 1); !finiteADQ(got) {
		t.Fatalf("p=0 lock cost should be bounded for planner fallback: %v", got)
	}
}

func TestADQReservationIsAtomicAndIdempotent(t *testing.T) {
	b := newADQReservationBook()
	b.SetCapacity("a", 1, 1)
	now := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := b.TryReserve(adqReservationRequest{AuthID: "a", FiveHour: .2, Weekly: .2, TTL: time.Minute}, now); ok {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 5 {
		t.Fatalf("successes=%d want 5", successes)
	}
	rs := b.Snapshot(now)
	if len(rs) != 5 {
		t.Fatalf("reservations=%d", len(rs))
	}
	if !b.Release(rs[0].ID) || !b.Release(rs[0].ID) {
		t.Fatal("release must be idempotent")
	}
	if f, w := b.Reserved("a", now); f >= 1 || w >= 1 {
		t.Fatalf("released reservation still counted f=%v w=%v", f, w)
	}
}

func TestADQProviderOverloadIsNotQuota(t *testing.T) {
	if got := classifyADQProviderError(503, "server_is_overloaded", "upstream overloaded"); got != adqProviderOverload {
		t.Fatalf("got %q", got)
	}
	if got := classifyADQProviderError(503, "usage_limit_reached", "limit"); got != adqProviderOverload {
		t.Fatalf("got %q", got)
	}
	if got := classifyADQProviderError(522, "", "cloudflare timeout"); got != adqProviderOverload {
		t.Fatalf("got %q", got)
	}
}

func TestADQCacheStateDoesNotOverrideBurn(t *testing.T) {
	p := defaultADQPolicy()
	warm := adqAccountInput{ID: "warm", WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy, CacheAge: 10 * time.Minute, CacheHitProbability: 1, CacheWarmBurn: .5, CacheColdBurn: 4, BurnMean: .5, BurnP95: 1}
	cold := warm
	cold.ID = "cold"
	cold.CacheAge = 2 * time.Hour
	cold.CacheHitProbability = 0
	wm := adqAssessAccount(warm, p, time.Now())
	cm := adqAssessAccount(cold, p, time.Now())
	if wm.CacheState != "warm" || cm.CacheState != "cold" {
		t.Fatalf("cache state warm=%q cold=%q", wm.CacheState, cm.CacheState)
	}
	if wm.BurnP95 >= cm.BurnP95 {
		t.Fatalf("warm cache did not reduce expected burn warm=%v cold=%v", wm.BurnP95, cm.BurnP95)
	}
}

func TestADQResetTransitionIgnoresDormantPlaceholder(t *testing.T) {
	p := defaultADQPolicy()
	now := time.Now()
	_ = p
	prev := adqAssessAccount(adqAccountInput{ID: "a", WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16, WeeklyResetAt: now.Add(24 * time.Hour), Healthy: true, ProviderState: adqProviderHealthy}, p, now)
	cur := prev
	cur.w = prev.w
	cur.NextWeekReset = prev.NextWeekReset
	if adqResetTransition(prev, cur, now) {
		t.Fatal("unchanged dormant snapshot falsely detected reset")
	}
	cur.w = 90
	cur.NextWeekReset = now.Add(48 * time.Hour)
	if !adqResetTransition(prev, cur, now) {
		t.Fatal("real reset transition not detected")
	}
}

func TestADQFiveHourHasNoImplicitSafetyReserve(t *testing.T) {
	p := defaultADQPolicy()
	in := adqAccountInput{
		ID: "full", WeeklyCapacity: 100, WeeklyRemaining: 100,
		FiveHourCapacity: 16, FiveHourRemaining: 16,
		Healthy: true, ProviderState: adqProviderHealthy,
	}
	m := adqAssessAccount(in, p, time.Now())
	if m.HEffective != 16 {
		t.Fatalf("5h effective quota=%v want 16; an implicit safety reserve was applied", m.HEffective)
	}
	if m.WEffective >= 100 {
		t.Logf("weekly safety is intentionally independent: effective=%v", m.WEffective)
	}
}

func TestADQNormalCDFTailsAndFutureWidthUseWeeklyReservoir(t *testing.T) {
	if got := adqNormalCDF(-9); got != 0 {
		t.Fatalf("negative tail=%v want 0", got)
	}
	if got := adqNormalCDF(9); got != 1 {
		t.Fatalf("positive tail=%v want 1", got)
	}
	p := defaultADQPolicy()
	m := adqAssessAccount(adqAccountInput{
		ID: "partial", WeeklyCapacity: 100, WeeklyRemaining: 100,
		FiveHourCapacity: 16, FiveHourRemaining: 8,
		Healthy: true, ProviderState: adqProviderHealthy,
	}, p, time.Now())
	if m.FullWidthContribution != 1 {
		t.Fatalf("weekly reservoir did not preserve one future full 5h cycle: %#v", m)
	}
	if m.EffectiveWidthContribution != 1 {
		t.Fatalf("effective future width=%v want 1", m.EffectiveWidthContribution)
	}
	if !m.Tail || m.HEffective >= m.H {
		t.Fatalf("current partial 5h headroom was not retained as an independent gate: %#v", m)
	}
}

func TestADQProjectedDebitAppliesToWeeklyAndFiveHour(t *testing.T) {
	p := defaultADQPolicy()
	now := time.Now()
	accounts := []adqAccountMetrics{
		adqAssessAccount(adqAccountInput{ID: "a", WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy}, p, now),
		adqAssessAccount(adqAccountInput{ID: "b", WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true, ProviderState: adqProviderHealthy}, p, now),
	}
	projected := adqProjectedMetrics(accounts, "a", 4, 4)
	if projected[0].WEffective != accounts[0].WEffective-4 || projected[0].HEffective != accounts[0].HEffective-4 {
		t.Fatalf("projected debit did not hit both windows: before=%#v after=%#v", accounts[0], projected[0])
	}
	if projected[1].WEffective != accounts[1].WEffective || projected[1].HEffective != accounts[1].HEffective {
		t.Fatalf("non-candidate account was modified: %#v", projected[1])
	}
}

func TestADQReservationSnapshotExpiresEveryAccount(t *testing.T) {
	b := newADQReservationBook()
	b.SetCapacity("a", 1, 1)
	b.SetCapacity("b", 1, 1)
	now := time.Now()
	ra, ok := b.TryReserve(adqReservationRequest{AuthID: "a", FiveHour: .2, Weekly: .2, TTL: time.Second}, now.Add(-2*time.Second))
	if !ok {
		t.Fatal("reserve a failed")
	}
	rb, ok := b.TryReserve(adqReservationRequest{AuthID: "b", FiveHour: .2, Weekly: .2, TTL: time.Second}, now.Add(-2*time.Second))
	if !ok {
		t.Fatal("reserve b failed")
	}
	_ = b.Snapshot(now)
	if got, _ := b.Get(ra.ID); got.Status != adqReservationExpired {
		t.Fatalf("reservation a status=%v", got.Status)
	}
	if got, _ := b.Get(rb.ID); got.Status != adqReservationExpired {
		t.Fatalf("reservation b status=%v", got.Status)
	}
	if f, w := b.Reserved("a", now); f != 0 || w != 0 {
		t.Fatalf("expired reservation a still counted f=%v w=%v", f, w)
	}
}

func TestADQSettlingDebtPersistsUntilQuotaChangesOrTTL(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	baselineAt := now.Add(-time.Second)
	baseline := quotaSnapshot{
		AuthID: "a", RefreshedAt: baselineAt,
		Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 10, ResetAt: now.Add(4 * time.Hour), ObservedAt: baselineAt},
			{Class: "weekly", UsedPercent: 20, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: baselineAt},
		},
	}
	b := newADQReservationBook()
	b.SetCapacity("a", 16, 100)
	r, ok := b.TryReserve(adqReservationRequest{AuthID: "a", FiveHour: .2, Weekly: .3, TTL: time.Second}, baselineAt)
	if !ok || !b.Settle(r.ID, .2, .3, baseline, now, 2*time.Minute) {
		t.Fatalf("could not create settling debt: %#v ok=%v", r, ok)
	}
	if f, w := b.Reserved("a", now); f != 0 || w != 0 {
		t.Fatalf("completed reservation remained active: f=%v w=%v", f, w)
	}
	if f, w := b.Settling("a", now); f != .2 || w != .3 {
		t.Fatalf("settling debt missing: f=%v w=%v", f, w)
	}

	unchanged := baseline
	unchanged.RefreshedAt = now.Add(time.Second)
	for i := range unchanged.Windows {
		unchanged.Windows[i].ObservedAt = unchanged.RefreshedAt
	}
	if changed := b.ObserveQuota("a", unchanged, unchanged.RefreshedAt); changed != 0 {
		t.Fatalf("unchanged telemetry cleared %d settlement layers", changed)
	}
	if f, w := b.Settling("a", unchanged.RefreshedAt); f != .2 || w != .3 {
		t.Fatalf("unchanged telemetry lost debt: f=%v w=%v", f, w)
	}

	fiveUpdated := unchanged
	fiveUpdated.RefreshedAt = now.Add(2 * time.Second)
	fiveUpdated.Windows = append([]quotaWindow(nil), unchanged.Windows...)
	fiveUpdated.Windows[0].ObservedAt = fiveUpdated.RefreshedAt
	fiveUpdated.Windows[0].UsedPercent = 11
	if changed := b.ObserveQuota("a", fiveUpdated, fiveUpdated.RefreshedAt); changed != 1 {
		t.Fatalf("5h update acknowledged %d layers; want 1", changed)
	}
	if f, w := b.Settling("a", fiveUpdated.RefreshedAt); f != 0 || w != .3 {
		t.Fatalf("5h and weekly settlement were not independent: f=%v w=%v", f, w)
	}

	weeklyReset := fiveUpdated
	weeklyReset.RefreshedAt = now.Add(3 * time.Second)
	weeklyReset.Windows = append([]quotaWindow(nil), fiveUpdated.Windows...)
	weeklyReset.Windows[1].ObservedAt = weeklyReset.RefreshedAt
	weeklyReset.Windows[1].UsedPercent = 0
	weeklyReset.Windows[1].ResetAt = now.Add(7 * 24 * time.Hour)
	if changed := b.ObserveQuota("a", weeklyReset, weeklyReset.RefreshedAt); changed != 1 {
		t.Fatalf("weekly reset acknowledged %d layers; want 1", changed)
	}
	if f, w := b.Settling("a", weeklyReset.RefreshedAt); f != 0 || w != 0 {
		t.Fatalf("acknowledged settlement remained: f=%v w=%v", f, w)
	}
	if got, _ := b.Get(r.ID); got.Status != adqReservationReconciled || got.SettlementReason != "telemetry" {
		t.Fatalf("settlement terminal state=%#v", got)
	}

	r2, ok := b.TryReserve(adqReservationRequest{AuthID: "a", FiveHour: .2, Weekly: .2, TTL: time.Second}, now.Add(4*time.Second))
	if !ok || !b.Settle(r2.ID, .2, .2, weeklyReset, now.Add(4*time.Second), 30*time.Second) {
		t.Fatal("could not seed TTL settlement")
	}
	if f, w := b.Settling("a", now.Add(35*time.Second)); f != 0 || w != 0 {
		t.Fatalf("TTL did not release settlement: f=%v w=%v", f, w)
	}
	if got, _ := b.Get(r2.ID); got.Status != adqReservationReconciled || got.SettlementReason != "ttl" {
		t.Fatalf("TTL terminal state=%#v", got)
	}
}

func TestADQExpiredReservationFallsBackToPendingDebit(t *testing.T) {
	now := time.Now()
	b := newADQReservationBook()
	b.SetCapacity("a", 1, 1)
	r, ok := b.TryReserve(adqReservationRequest{AuthID: "a", FiveHour: .4, Weekly: .4, TTL: time.Second}, now)
	if !ok {
		t.Fatal("reservation failed")
	}
	account := &balancedAccount{Pending: []balancedPending{{At: now, Cost: .4, ReservationID: r.ID}}}
	if got := adqPendingWithoutReservation(account, b, now.Add(500*time.Millisecond)); got != 0 {
		t.Fatalf("active reservation was double-counted as pending: %v", got)
	}
	if got := adqPendingWithoutReservation(account, b, now.Add(2*time.Second)); got != .4 {
		t.Fatalf("expired long request disappeared from effective quota: %v", got)
	}
}

func TestADQSettlingDebtBalancesFreshSessionsButDoesNotBreakSticky(t *testing.T) {
	p := defaultADQPolicy()
	now := time.Now()
	in := adqAccountInput{
		ID: "a", WeeklyCapacity: 100, WeeklyRemaining: 10,
		FiveHourCapacity: 16, FiveHourRemaining: 2,
		SettlingFiveHour: 2, SettlingWeekly: 10,
		Healthy: true, ProviderState: adqProviderHealthy,
	}
	fresh := adqAssessAccount(in, p, now)
	if fresh.Eligible {
		t.Fatalf("fresh work ignored unconfirmed completed usage: %#v", fresh)
	}
	in.Sticky = true
	sticky := adqAssessAccount(in, p, now)
	if !sticky.Eligible || sticky.Reason != "sticky_settlement_pending" {
		t.Fatalf("telemetry lag forced a sticky migration: %#v", sticky)
	}
}

func TestADQProviderOverloadCircuitReopensAfterRetry(t *testing.T) {
	p := defaultADQPolicy()
	now := time.Now()
	m := adqAssessAccount(adqAccountInput{
		ID: "overload", WeeklyCapacity: 100, WeeklyRemaining: 100,
		FiveHourCapacity: 16, FiveHourRemaining: 16,
		Healthy: true, ProviderState: adqProviderOverload,
		ProviderRetryAt: now.Add(-time.Second),
	}, p, now)
	if !m.Eligible || m.ProviderState != adqProviderHealthy {
		t.Fatalf("cooled overload circuit did not reopen: %#v", m)
	}
}

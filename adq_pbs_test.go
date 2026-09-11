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

func TestADQNormalCDFTailsAndWidthUseBothWindows(t *testing.T) {
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
	if m.FullWidthContribution != 0 {
		t.Fatalf("partial 5h window counted as FullWidth: %#v", m)
	}
	if m.EffectiveWidthContribution != .5 {
		t.Fatalf("effective width=%v want .5", m.EffectiveWidthContribution)
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

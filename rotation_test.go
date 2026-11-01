package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestRotationProtectsFutureWidthBeforeFairShare(t *testing.T) {
	now := time.Now()
	accounts := []adqAccountInput{
		{ID: "healthy", WeeklyCapacity: 100, WeeklyRemaining: 66, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true},
		{ID: "last-full", WeeklyCapacity: 100, WeeklyRemaining: 17, FiveHourCapacity: 16, FiveHourRemaining: 16, Healthy: true},
	}
	decision, ok := adqChooseWithAllocation(accounts, 1, 2, defaultADQPolicy(), now,
		map[string]adqAllocationScore{"healthy": {Credit: 0}, "last-full": {Credit: 1}})
	if !ok || decision.AuthID != "healthy" || decision.PoolAfter.FullWidth != 2 {
		t.Fatalf("fair share destroyed future width: %+v", decision)
	}
	accounts[1].Sticky = true
	decision, ok = adqChooseWithAllocation(accounts, 1, 2, defaultADQPolicy(), now, nil)
	if !ok || decision.AuthID != "last-full" {
		t.Fatal("forecast moved an eligible conversation")
	}
}

func TestRotationSparsePhasesCarryUsableQuota(t *testing.T) {
	now := time.Now()
	policy := defaultADQPolicy()
	metrics := []adqAccountMetrics{}
	for i := 0; i < 7; i++ {
		metrics = append(metrics, adqAssessAccount(adqAccountInput{ID: "funded", Healthy: true,
			WeeklyCapacity: 100, WeeklyRemaining: 100, FiveHourCapacity: 16, FiveHourRemaining: 16,
			PhaseAnchor: now.Add(-time.Duration(i) * 30 * time.Minute)}, policy, now))
	}
	pool := adqComputePool(metrics, 1, policy, now)
	if pool.MPhase < 1 || pool.Bottleneck == "PHASE_CONSTRAINED" {
		t.Fatalf("empty buckets lost quota: %+v", pool)
	}
	unknown := adqComputePool(metrics, math.NaN(), policy, now)
	if unknown.DemandKnown || !math.IsNaN(unknown.MSystem) || unknown.Bottleneck != "FORECAST_UNKNOWN" {
		t.Fatalf("unknown became zero demand: %+v", unknown)
	}
}

func TestRotationRecoveryTimelineHonorsGapsAndWeeklyBacking(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name                    string
		current, weekly, demand float64
		safe                    bool
	}{
		{"bridged", 1, 20, 1, true}, {"gap-before-reset", 1, 20, 5, false}, {"no-weekly-refill", 1, 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := []adqAccountMetrics{{H: 10, HEffective: tc.current, WEffective: tc.weekly, Next5hReset: now.Add(time.Hour)}}
			margin := adqRecoveryMargin(metrics, tc.demand, 5*time.Hour, now)
			if (margin >= 1) != tc.safe {
				t.Fatalf("margin=%v safe=%v", margin, tc.safe)
			}
		})
	}
}

func TestRotationLearnsThirtyMinutePollsWithoutWeakeningAdmissionTracker(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: map[string]quotaSnapshot{}}
	s.cfg.QuotaRefreshCooldown = 30 * time.Minute
	var q quotaSnapshot
	for i, used := range []float64{10, 20, 30} {
		at := start.Add(time.Duration(i) * 30 * time.Minute)
		q = runwayTestSnapshot(at, start.Add(4*time.Hour), used)
		s.observeRotationQuotaLocked(q, at)
		s.quotaRunway.Observe(q, at)
		s.quotas[q.AuthID] = q
	}
	mean, p90, p95, known := s.rotationBurnLocked(q, "5h", 16, q.RefreshedAt)
	if !known || math.Abs(mean-3.2) > 1e-9 || p90 < mean || p95 < p90 {
		t.Fatalf("native cadence failed: %v %v %v %v", mean, p90, p95, known)
	}
	if demand := s.rotationDemandLocked(q.RefreshedAt); !finiteADQ(demand) || demand <= 0 {
		t.Fatalf("demand not connected: %v", demand)
	}
	s.pacingAccounts = map[string]*accountPacingState{q.AuthID: {Capacities: map[string]capacityEstimate{
		"weekly": {Credits: 1000, Samples: 1}, "5h": {Credits: 160, Samples: 1},
	}}}
	if demand := s.adqForecastDemandLocked(q.RefreshedAt); math.Abs(demand-32) > 1e-8 {
		t.Fatalf("forecast units differ from calibrated quota: %v", demand)
	}
	if s.quotaRunway.Assess(q, s.cfg, q.RefreshedAt)[0].RateKnown {
		t.Fatal("long cadence weakened the admission guard")
	}
	q.AuthIndex = "replacement"
	if _, _, _, known = s.rotationBurnLocked(q, "5h", 16, q.RefreshedAt); known {
		t.Fatal("replacement inherited rate")
	}
	q.AuthIndex = "index-a"
	if _, _, _, known = s.rotationBurnLocked(q, "5h", 16, q.RefreshedAt.Add(3*time.Hour)); known {
		t.Fatal("stale rate survived")
	}
}

func TestRotationWeeklyOnlyIsAvailableWithoutPhantomFiveHourWidth(t *testing.T) {
	now := time.Now()
	q, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "weekly", "index", now)
	if err != nil {
		t.Fatal(err)
	}
	status := runtimeADQAccountFor(q, defaultPluginConfig(), defaultADQPolicy(), pacingStateSnapshot{}, adqProviderCircuit{}, nil, nil, now)
	if status == nil || !status.Eligible || !status.WeeklyOnly || status.H != 0 || status.FullWidthContribution != 0 {
		t.Fatalf("weekly-only diagnostic: %+v", status)
	}
}

func TestRotationAdvancesWarmupWhenActiveQuotaCannotBridge(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: map[string]quotaSnapshot{}}
	for i, used := range []float64{94, 96, 98} {
		at := now.Add(time.Duration(i-2) * time.Minute)
		q := phaseSnapshot("active", at, now.Add(3*time.Hour), used)
		for i := range q.Windows {
			q.Windows[i].Source = quotaSourceProbe
		}
		s.observeRotationQuotaLocked(q, at)
		s.quotas[q.AuthID] = q
	}
	deadline := s.rotationActivationDeadlineLocked("idle", now.Add(2*time.Hour), now)
	if deadline.After(now.Add(time.Minute)) {
		t.Fatalf("held phase despite exhaustion: %v", deadline)
	}
	s.cfg.WarmupEnabled, s.cfg.QuotaProbeOnDemand = true, true
	q := readyWarmupQuota("idle", now.Add(-20*time.Minute))
	s.quotas["idle"] = q
	s.fiveHourPhases = map[string]fiveHourPhaseState{"idle": {ActivateAt: now.Add(2 * time.Hour)}}
	s.quotaPolls = map[string]quotaPollState{"idle": {AttemptedAt: now.Add(-20 * time.Minute), NextAt: now.Add(10 * time.Minute)}}
	if reason := s.quotaProbeReason("idle", now); reason != "warmup" {
		t.Fatalf("urgent forecast could not refresh stale idle credential: %s", reason)
	}
	p := s.quotaPolls["idle"]
	p.Failures = 1
	s.quotaPolls["idle"] = p
	if reason := s.quotaProbeReason("idle", now); reason != "" {
		t.Fatalf("forecast bypassed failed-probe backoff: %s", reason)
	}
}

func TestWarmupHistorySurvivesExpiredCycleAndReload(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	s := newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	claimManagedRuntimeForTest(t, s)
	entry := warmupEntry{AuthID: "a", AuthIndex: "idx-a", Window: "5h", AttemptedAt: now.Add(-6 * time.Hour),
		CompletedAt: now.Add(-6 * time.Hour), ActivatedAt: now.Add(-6 * time.Hour), ResetAt: now.Add(-time.Hour), SuppressUntil: now.Add(-time.Hour)}
	s.warmups = map[string]warmupEntry{"a|5h": entry}
	s.rememberWarmupLocked(entry, true, now)
	if !s.pruneExpiredWarmups(now) || len(s.warmups) != 0 || len(s.warmupHistory) != 1 {
		t.Fatal("expiry erased visible history")
	}
	if !s.persistBanState() {
		t.Fatal("state write failed")
	}
	restored := schedulerRuntimeState{cfg: s.cfg}
	restored.loadBanState(s.cfg.StatePath)
	if len(restored.warmupHistory) != 1 || len(restored.status().WarmupHistory) != 1 {
		t.Fatal("history lost on reload")
	}
}

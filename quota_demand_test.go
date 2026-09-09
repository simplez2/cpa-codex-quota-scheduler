package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestDemandPollingConsumesActivityOnceAndPreservesIdleCache(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			w.Write([]byte(`{"files":[{"id":"a","name":"a.json","auth_index":"idx","provider":"codex","status":"active"}]}`))
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status_code":200,"body":"{\"rate_limit\":{\"primary_window\":{\"used_percent\":10,\"limit_window_seconds\":18000,\"reset_after_seconds\":600}}}"}`))
	}))
	defer server.Close()
	s := newRefreshTestState(t, server.URL)
	s.refreshOnce(context.Background())
	if calls != 1 {
		t.Fatalf("initial=%d", calls)
	}
	original := s.quotas["a"].RefreshedAt
	makeDue := func() { p := s.quotaPolls["a"]; p.NextAt = time.Time{}; s.quotaPolls["a"] = p }
	makeDue()
	s.refreshOnce(context.Background())
	if calls != 1 || s.quotas["a"].RefreshedAt != original {
		t.Fatal("idle poll or fabricated freshness")
	}
	s.observeUsage(pluginapi.UsageRecord{Provider: "codex", Generate: true, AuthID: "a"})
	makeDue()
	s.refreshOnce(context.Background())
	if calls != 2 {
		t.Fatalf("usage failed to trigger: %d", calls)
	}
	makeDue()
	s.refreshOnce(context.Background())
	if calls != 2 {
		t.Fatal("consumed activity polled repeatedly")
	}
}
func TestDemandResetEventIgnoresMovingPlaceholderAndLimitsFailureRetries(t *testing.T) {
	s := newRefreshTestState(t, "http://unused.test")
	now := time.Now()
	reset := now.Add(-time.Minute)
	q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(0)), "a", "idx", now.Add(-time.Hour))
	q.Windows[0].ResetAt = reset
	s.quotas["a"] = q
	s.quotaPolls["a"] = quotaPollState{AttemptedAt: now.Add(-time.Hour)}
	if got := s.quotaProbeReason("a", now); got != "reset" {
		t.Fatalf("reset reason=%s", got)
	}
	p := s.quotaPolls["a"]
	p.AttemptedAt = now
	p.Reason = "reset"
	s.quotaPolls["a"] = p
	if s.quotaProbeReason("a", now) != "" {
		t.Fatal("repeated reset probe")
	}
	placeholder, _ := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000}}}`), "b", "idx-b", now.Add(-6*time.Hour))
	s.quotas["b"] = placeholder
	s.quotaPolls["b"] = quotaPollState{AttemptedAt: now.Add(-6 * time.Hour)}
	if got := s.quotaProbeReason("b", now); got != "" {
		t.Fatalf("expired placeholder triggered probe: %s", got)
	}
	p.Failures = 3
	s.quotaPolls["a"] = p
	if s.quotaProbeReason("a", now) != "" {
		t.Fatal("unbounded idle retries")
	}
}
func TestElapsedCooldownPreservesOtherWindowsProbationAndWarmupHistory(t *testing.T) {
	for _, kind := range []string{"expired", "future", "weekly_block", "probation", "half_open"} {
		t.Run(kind, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s := newRefreshTestState(t, "http://unused.test")
			now := time.Now()
			q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(100)), "a", "idx", now.Add(-time.Hour))
			q.Windows[0].ResetAt = now.Add(-time.Minute)
			q.Windows[1].ResetAt = now.Add(time.Hour)
			entry := banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(-time.Minute)}
			switch kind {
			case "future":
				entry.ResetAt = now.Add(time.Minute)
			case "weekly_block":
				q.Windows[1].LimitReached = true
			case "probation":
				entry.Kind = banKindProbation
			case "half_open":
				entry.Phase = banPhaseHalfOpen
				entry.ProbeStartedAt = now
			}
			banStore.set("a", entry)
			s.warmups["a|weekly"] = warmupEntry{AuthID: "a", Window: "weekly", ActivatedAt: now, ResetAt: now.Add(time.Hour)}
			before := q.RefreshedAt
			s.releaseElapsedQuotaCooldowns(map[string]quotaSnapshot{"a": q}, now)
			_, banned := banStore.lookup("a")
			if banned != (kind != "expired") {
				t.Fatalf("banned=%v", banned)
			}
			if len(s.warmups) != 1 || q.RefreshedAt != before {
				t.Fatal("changed warmup history or claimed a fresh probe")
			}
		})
	}
}

func TestDemandIdleCachePreservesWeeklySchedulingAndExpiry(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.QuotaProbeOnDemand = true
	q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(0)), "a", "idx", now.Add(-time.Hour))
	for i := range q.Windows {
		q.Windows[i].ResetAt = now.Add(time.Hour)
	}
	q.Windows[1].UsedPercent = 60
	candidate := pluginapi.SchedulerAuthCandidate{ID: "a"}
	choice := inspectSerialCandidate(candidate, q, true, cfg, now)
	if !choice.WeeklyKnown || choice.WeeklyRemaining != 40 {
		t.Fatalf("idle budget discarded: %+v", choice)
	}
	if budget, known := serialWeeklyBudget(choice, cfg, now); !known || budget <= 0 {
		t.Fatalf("idle weekly budget unknown: %v %v", budget, known)
	}
	q.Windows[1].UsedPercent = 100
	if inspectSerialCandidate(candidate, q, true, cfg, now).Eligible {
		t.Fatal("idle weekly limit discarded")
	}
	q.Windows[1].ResetAt = now.Add(-time.Second)
	if !inspectSerialCandidate(candidate, q, true, cfg, now).Eligible {
		t.Fatal("expired weekly limit retained")
	}
	if quotaSnapshotFresh(q, now, cfg.StaleAfter) {
		t.Fatal("fabricated freshness")
	}
	cfg.QuotaProbeOnDemand = false
	if quotaSchedulingUsable(q, now, cfg) {
		t.Fatal("periodic mode changed")
	}
}

func TestDemandRetainsCachedPlanWithoutRefreshingObservation(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.QuotaProbeOnDemand = true
	q := quotaSnapshot{Plan: quotaPlanObservation{Type: "self_serve_business_prolite", Source: "cpa_usage", ObservedAt: now.Add(-time.Hour)}}
	plan, weight, source := resolvedQuotaPlan(cfg, "a", q, now)
	if plan != "team_premium" || weight != 5 || source != "cpa_usage" || q.Plan.fresh(now, cfg.StaleAfter) {
		t.Fatal("cached plan lost or made fresh")
	}
	cfg.QuotaProbeOnDemand = false
	_, _, source = resolvedQuotaPlan(cfg, "a", q, now)
	if source != "default" {
		t.Fatal("periodic behavior changed")
	}
}

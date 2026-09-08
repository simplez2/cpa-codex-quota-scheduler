package main

import (
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func balancedFixture(now time.Time) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest) {
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "balanced"
	cfg.StatePath = ""
	s := &schedulerRuntimeState{cfg: cfg, quotas: map[string]quotaSnapshot{}}
	r := pluginapi.SchedulerPickRequest{Model: "gpt-5.5", Provider: providerCodex}
	for _, id := range []string{"a", "b"} {
		s.quotas[id] = quotaSnapshot{AuthID: id, AuthIndex: id, RefreshedAt: now, Windows: []quotaWindow{
			{Class: "5h", WindowSeconds: 18000, UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
			{Class: "weekly", WindowSeconds: 604800, UsedPercent: 20, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now},
		}}
		r.Candidates = append(r.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: providerCodex})
	}
	return s, r
}

func TestBalancedParallelRequestsReserveFairShareAtomically(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req := balancedFixture(time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if response, err := s.schedulerPick(req); err != nil || !response.Handled {
				t.Errorf("pick=%+v error=%v", response, err)
			}
		}()
	}
	wg.Wait()
	for _, id := range []string{"a", "b"} {
		if s.balancedAccounts[id].Picks != 100 {
			t.Fatalf("uneven concurrent picks: %+v", s.balancedStatusLocked(time.Now()))
		}
	}
	if s.serialActiveAuthID != "" {
		t.Fatal("balanced routing committed a serial primary")
	}
}

func TestBalancedPlanCapacityAndWeeklyBudgetShares(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.cfg.QuotaAccountPlans = map[string]string{"b": "pro_5x"}
	for i := 0; i < 600; i++ {
		s.balancedPick(req, now)
	}
	if s.balancedAccounts["a"].Picks != 100 || s.balancedAccounts["b"].Picks != 500 {
		t.Fatalf("capacity shares: %+v", s.balancedStatusLocked(now))
	}
	s, req = balancedFixture(now)
	s.quotas["a"].Windows[1].UsedPercent = 60
	for i := 0; i < 300; i++ {
		s.balancedPick(req, now)
	}
	if a, b := s.balancedAccounts["a"].Picks, s.balancedAccounts["b"].Picks; a < 50 || b <= a {
		t.Fatalf("both accounts must contribute, 80%% should carry more: %d %d", a, b)
	}
}

func TestBalancedHardLimitsZeroReserveAndPinnedIsolation(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.cfg.Reserve5hPercent = 20 // 429-only must ignore this old setting.
	s.quotas["a"].Windows[0].UsedPercent = 99.9
	req.Options.Metadata = map[string]any{"pinned_auth_id": "a"}
	if got := s.balancedPick(req, now); got.AuthID != "a" {
		t.Fatalf("early reserve: %+v", got)
	}
	s.quotas["a"].Windows[1].UsedPercent = 100
	if got := s.balancedPick(req, now); got.Handled {
		t.Fatal("weekly exhaustion allowed by fresh 5h or pin")
	}
	req.Options.Metadata = nil
	if got := s.balancedPick(req, now); got.AuthID != "b" {
		t.Fatal("did not hand off hard-limited account")
	}
	banStore.set("b", banEntry{ResetAt: now.Add(time.Hour), Window: "5h"})
	if got := s.balancedPick(req, now); got.Handled {
		t.Fatal("quarantined account selected")
	}
}

func TestBalancedCompletionCorrectionIsBoundedAndDeduplicated(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	first := s.balancedPick(req, now)
	s.balancedPick(req, now.Add(time.Millisecond))
	record := pluginapi.UsageRecord{Provider: providerCodex, Generate: true, AuthID: first.AuthID, Model: req.Model, RequestedAt: now, Detail: pluginapi.UsageDetail{OutputTokens: 1000000}}
	s.observeBalancedUsage(record, now.Add(time.Second))
	a := s.balancedAccounts[first.AuthID]
	credit := a.Credit
	s.observeBalancedUsage(record, now.Add(2*time.Second))
	if a.Credit != credit || len(a.Pending) != 0 {
		t.Fatal("duplicate completion changed balance")
	}
	for i := 0; i < 50; i++ {
		s.balancedPick(req, now.Add(time.Duration(i+3)*time.Second))
	}
	if a.Picks < 2 {
		t.Fatal("large completion permanently starved account")
	}
	if pending := s.balancedStatusLocked(now.Add(3 * time.Hour))[first.AuthID].Pending; pending != 0 {
		t.Fatal("expired pending never recovered")
	}
}

func TestBalancedModeIsAcceptedByPanelValidation(t *testing.T) {
	changes, errors := validatePanelSettings(nil, map[string]any{"scheduler_mode": "balanced"})
	if len(errors) != 0 || changes["scheduler_mode"] != "balanced" {
		t.Fatalf("%v %v", changes, errors)
	}
}

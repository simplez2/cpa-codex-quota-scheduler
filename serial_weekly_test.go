package main

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func newWeeklyBalanceTestState(now time.Time) *schedulerRuntimeState {
	state := newSerialTestState(now)
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"
	state.serialSelectedAt = now.Add(-10 * time.Minute)
	state.identities = map[string]string{"index-primary": "primary", "index-backup": "backup"}
	state.quotaPolls = make(map[string]quotaPollState)
	for id, used := range map[string]float64{"primary": 60, "backup": 20} {
		state.quotas[id] = quotaSnapshot{AuthID: id, AuthIndex: "index-" + id, RefreshedAt: now, Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 10, Allowed: true, ObservedAt: now, ResetAt: now.Add(4 * time.Hour)},
			{Class: "weekly", UsedPercent: used, Allowed: true, ObservedAt: now, ResetAt: now.Add(4 * 24 * time.Hour)},
		}}
		state.quotaPolls[id] = quotaPollState{AuthIndex: "index-" + id, AttemptedAt: now}
	}
	return &state
}

func observeWeeklyBalanceTestQuota(state *schedulerRuntimeState, id string, now time.Time) {
	q := state.quotas[id]
	q.RefreshedAt = now
	q.Windows = append([]quotaWindow(nil), q.Windows...)
	for i := range q.Windows {
		q.Windows[i].ObservedAt = now
	}
	state.quotas[id] = q
}

func TestSerialWeeklyColdStartPrioritizesWeeklyCapacity(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newWeeklyBalanceTestState(now)
	state.serialActiveAuthID = ""
	state.serialSelectedAt = time.Time{}
	q := state.quotas["primary"]
	q.ResetCredits = 1
	q.Windows[0].UsedPercent = 0 // full 5h must not trump 40% versus 80% weekly
	q.Windows[1].ResetAt = now.Add(3 * time.Hour)
	state.quotas["primary"] = q
	req := serialTestRequest()
	req.Candidates[1].Priority = 1000
	if got := state.serialPick(req, now); got.AuthID != "backup" {
		t.Fatalf("40%% weekly with drain/credits/priority beat 80%% weekly: %#v", got)
	}
}

func TestSerialWeeklyRequiresIndependentObservationsAndRebindsSessions(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newWeeklyBalanceTestState(now)
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"weekly-balance-session"}}
	state.serialOverdraft = map[string]serialOverdraftBinding{"other-session": {AuthID: "primary", LastUsedAt: now}}
	if got := state.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("switched without confirmation: %#v", got)
	}
	for i := 1; i <= 100; i++ {
		if got := state.serialPick(req, now.Add(time.Duration(i)*time.Second)); got.AuthID != "primary" {
			t.Fatalf("cached observation became confirmation at request %d: %#v", i, got)
		}
	}
	q := state.quotas["backup"]
	q.RefreshedAt = now.Add(2 * time.Minute)
	state.quotas["backup"] = q // a fresh outer timestamp is not new weekly evidence
	observeWeeklyBalanceTestQuota(state, "primary", now.Add(2*time.Minute))
	if got := state.serialPick(req, now.Add(2*time.Minute)); got.AuthID != "primary" || state.serialWeeklyRebalance.Confirmations != 1 {
		t.Fatalf("one-sided observation counted twice: %#v evidence=%#v", got, state.serialWeeklyRebalance)
	}
	observeWeeklyBalanceTestQuota(state, "backup", now.Add(2*time.Minute))
	if got := state.serialPick(req, now.Add(2*time.Minute)); got.AuthID != "backup" {
		t.Fatalf("confirmed 80%% weekly account did not replace 40%%: %#v", got)
	}
	if got := state.serialPick(req, now.Add(3*time.Minute)); got.AuthID != "backup" {
		t.Fatalf("same session returned to old account: %#v", got)
	}
	if state.serialSwitches != 1 || state.serialLastSwitchReason != "weekly_rebalance" {
		t.Fatalf("switch bookkeeping: %d %s", state.serialSwitches, state.serialLastSwitchReason)
	}
	if b := state.serialOverdraft["other-session"]; b.AuthID != "backup" || !b.LastUsedAt.Equal(now) {
		t.Fatalf("other session was not moved without extending TTL: %#v", b)
	}
}

func TestSerialWeeklyMinimumHoldAndSmallAdvantage(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newWeeklyBalanceTestState(now)
	state.serialSelectedAt = now.Add(-time.Minute)
	state.serialPick(serialTestRequest(), now)
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(state, id, now.Add(2*time.Minute))
	}
	if got := state.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "primary" || state.serialWeeklyRebalance.Confirmations != 2 {
		t.Fatalf("hold did not delay a confirmed switch: %#v", got)
	}
	if got := state.serialPick(serialTestRequest(), now.Add(4*time.Minute)); got.AuthID != "backup" {
		t.Fatalf("expired hold did not allow confirmed switch: %#v", got)
	}
	for _, gap := range []float64{1, 9.99, 10} {
		t.Run(fmt.Sprintf("gap_%g", gap), func(t *testing.T) {
			s := newWeeklyBalanceTestState(now)
			s.quotas["backup"].Windows[1].UsedPercent = 60 - gap
			s.serialPick(serialTestRequest(), now)
			for _, id := range []string{"primary", "backup"} {
				observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
			}
			got := s.serialPick(serialTestRequest(), now.Add(2*time.Minute))
			if (got.AuthID == "backup") != (gap >= 10) {
				t.Fatalf("gap=%v selected=%s", gap, got.AuthID)
			}
		})
	}
}

func TestSerialWeeklyInvalidEvidenceResetsConfirmation(t *testing.T) {
	cases := map[string]func(*schedulerRuntimeState, time.Time){
		"failed_poll": func(s *schedulerRuntimeState, _ time.Time) {
			p := s.quotaPolls["backup"]
			p.Error = "quota_http_503"
			p.Failures = 1
			s.quotaPolls["backup"] = p
		},
		"missing_observation": func(s *schedulerRuntimeState, _ time.Time) { s.quotas["backup"].Windows[1].ObservedAt = time.Time{} },
		"stale_weekly_with_fresh_outer": func(s *schedulerRuntimeState, at time.Time) {
			s.quotas["backup"].Windows[1].ObservedAt = at.Add(-time.Hour)
		},
		"future_observation": func(s *schedulerRuntimeState, at time.Time) {
			s.quotas["backup"].Windows[1].ObservedAt = at.Add(time.Hour)
		},
		"wrong_inventory_binding": func(s *schedulerRuntimeState, _ time.Time) { s.identities["index-backup"] = "another-auth" },
		"wrong_poll_binding": func(s *schedulerRuntimeState, _ time.Time) {
			s.quotaPolls["backup"] = quotaPollState{AuthIndex: "old-index"}
		},
		"lost_advantage": func(s *schedulerRuntimeState, _ time.Time) { s.quotas["backup"].Windows[1].UsedPercent = 59 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			resetBanStoreForTest()
			now := time.Now()
			s := newWeeklyBalanceTestState(now)
			s.serialPick(serialTestRequest(), now)
			for _, id := range []string{"primary", "backup"} {
				observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
			}
			mutate(s, now.Add(2*time.Minute))
			if got := s.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 0 {
				t.Fatalf("invalid evidence allowed rebalance: %#v evidence=%#v", got, s.serialWeeklyRebalance)
			}
		})
	}
}

func TestSerialWeeklyNewBindingRequiresNewConfirmation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialPick(serialTestRequest(), now)
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
	}
	q := s.quotas["backup"]
	q.AuthIndex = "replacement-index"
	s.quotas["backup"] = q
	delete(s.identities, "index-backup")
	s.identities[q.AuthIndex] = "backup"
	s.quotaPolls["backup"] = quotaPollState{AuthIndex: q.AuthIndex}
	if got := s.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 1 {
		t.Fatalf("new identity inherited old confirmation: %#v evidence=%#v", got, s.serialWeeklyRebalance)
	}
}

func TestSerialWeeklyHardLimitsOverrideWeeklyAndHold(t *testing.T) {
	for _, class := range []string{"5h", "weekly"} {
		t.Run(class, func(t *testing.T) {
			resetBanStoreForTest()
			now := time.Now()
			s := newWeeklyBalanceTestState(now)
			s.serialActiveAuthID = "backup" // high weekly, but a hard window is unavailable
			s.serialSelectedAt = now
			for i, w := range s.quotas["backup"].Windows {
				if w.Class == class {
					s.quotas["backup"].Windows[i].UsedPercent = 100
				}
			}
			if got := s.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
				t.Fatalf("hard %s limit did not force immediate switch: %#v", class, got)
			}
		})
	}
}

func TestSerialWeeklyCycleCannotDowngradeWeeklyCapacity(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialActiveAuthID = "backup" // 80% remaining
	s.serialFiveHourCycle = map[string]time.Time{"backup": now.Add(-time.Hour)}
	if got := s.serialPick(serialTestRequest(), now); got.AuthID != "backup" {
		t.Fatalf("5h reset rotated 80%% weekly to 40%% weekly: %#v", got)
	}
	if !s.serialFiveHourCycle["backup"].Equal(s.quotas["backup"].Windows[0].ResetAt) {
		t.Fatal("blocked cycle boundary was not consumed")
	}
}

func TestSerialWeeklyManualAndPinnedSelection(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialSelectionSource = "manual"
	s.serialPick(serialTestRequest(), now)
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
	}
	if got := s.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 0 {
		t.Fatalf("manual selection was rebalanced: %#v", got)
	}
	s.serialSelectionSource = "auto"
	req := serialTestRequest()
	req.Options.Metadata = map[string]any{"pinned_auth_id": "backup"}
	if got := s.serialPick(req, now.Add(2*time.Minute)); got.AuthID != "backup" || s.serialActiveAuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 0 {
		t.Fatalf("request pin affected global balancing: %#v", got)
	}
}

func TestSerialWeeklyConcurrentConfirmationCommitsOnce(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialPick(serialTestRequest(), now)
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := s.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "backup" {
				t.Errorf("concurrent pick: %#v", got)
			}
		}()
	}
	wg.Wait()
	if s.serialSwitches != 1 {
		t.Fatalf("committed %d switches", s.serialSwitches)
	}
}

func TestSerialWeeklyReloadKeepsHoldButDropsEvidence(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialSelectedAt = now.Add(-time.Minute)
	s.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	s.initializeGenerationOwnership(s.cfg.StatePath)
	if err := s.reserveGenerationOwnership(s.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, s)
	s.serialPick(serialTestRequest(), now)
	if !s.persistBanState() {
		t.Fatal("persist failed")
	}
	restored := newWeeklyBalanceTestState(now)
	restored.loadBanState(s.cfg.StatePath)
	if !restored.serialSelectedAt.Equal(s.serialSelectedAt) || restored.serialWeeklyRebalance.Confirmations != 0 {
		t.Fatal("reload lost hold or restored transient confirmations")
	}
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(restored, id, now.Add(2*time.Minute))
	}
	if got := restored.serialPick(serialTestRequest(), now.Add(2*time.Minute)); got.AuthID != "primary" || restored.serialWeeklyRebalance.Confirmations != 1 {
		t.Fatalf("reload reused old confirmation: %#v", got)
	}
}

func TestSerialWeeklyRebalanceConfig(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("serial_weekly_rebalance_percent: 15\nserial_weekly_rebalance_min_hold: 10m\n"))
	if err != nil || cfg.SerialWeeklyRebalancePercent != 15 || cfg.SerialWeeklyRebalanceMinHold != 10*time.Minute {
		t.Fatalf("config: %#v %v", cfg, err)
	}
	for _, raw := range []string{"serial_weekly_rebalance_percent: -1", "serial_weekly_rebalance_percent: .nan", "serial_weekly_rebalance_percent: 101", "serial_weekly_rebalance_min_hold: 10s", "serial_weekly_rebalance_min_hold: invalid"} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid config accepted: %s", raw)
		}
	}
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.cfg.SerialWeeklyRebalancePercent = 0
	s.serialPick(pluginapi.SchedulerPickRequest{Candidates: serialTestRequest().Candidates}, now)
	if s.serialWeeklyRebalance.Confirmations != 0 {
		t.Fatal("disabled balancing accumulated evidence")
	}
}

func TestSerialWeeklyBalancesEightHoursOfDemandAcrossPool(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newWeeklyBalanceTestState(now)
	s.serialActiveAuthID = ""
	s.serialSelectedAt = time.Time{}
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"continuous-work"}}
	req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: "third", Provider: providerCodex, Priority: 10})
	s.identities["index-third"] = "third"
	for _, candidate := range req.Candidates {
		id := candidate.ID
		s.quotas[id] = quotaSnapshot{AuthID: id, AuthIndex: "index-" + id, RefreshedAt: now, Windows: []quotaWindow{
			{Class: "5h", Allowed: true, ObservedAt: now, ResetAt: now.Add(5 * time.Hour)},
			{Class: "weekly", Allowed: true, ObservedAt: now, ResetAt: now.Add(7 * 24 * time.Hour)},
		}}
	}
	uses := make(map[string]int)
	weeklySwitches := 0
	for tick := 0; tick < 240; tick++ {
		at := now.Add(time.Duration(tick) * 2 * time.Minute)
		for _, candidate := range req.Candidates {
			id := candidate.ID
			observeWeeklyBalanceTestQuota(s, id, at)
			w := &s.quotas[id].Windows[0]
			if !at.Before(w.ResetAt) {
				w.UsedPercent = 0
				w.ResetAt = at.Add(5 * time.Hour)
			}
		}
		before := s.serialSwitches
		picked := s.serialPick(req, at)
		if !picked.Handled {
			t.Fatalf("pool unavailable at tick %d", tick)
		}
		if s.serialSwitches > before && s.serialLastSwitchReason == "weekly_rebalance" {
			weeklySwitches++
		}
		q := s.quotas[picked.AuthID]
		q.Windows[0].UsedPercent += 1.5
		q.Windows[1].UsedPercent += 0.4
		if q.Windows[0].UsedPercent >= 100 || q.Windows[1].UsedPercent >= 100 {
			t.Fatalf("exhausted %s at tick %d", picked.AuthID, tick)
		}
		s.quotas[picked.AuthID] = q
		uses[picked.AuthID]++
	}
	min, max := 100.0, 0.0
	for _, candidate := range req.Candidates {
		remaining := 100 - s.quotas[candidate.ID].Windows[1].UsedPercent
		if uses[candidate.ID] == 0 || remaining < 55 {
			t.Fatalf("account %s not preserved: uses=%v remaining=%v", candidate.ID, uses, remaining)
		}
		if remaining < min {
			min = remaining
		}
		if remaining > max {
			max = remaining
		}
	}
	if max-min > s.cfg.SerialWeeklyRebalancePercent+2 || weeklySwitches < 4 {
		t.Fatalf("pool not balanced: min=%v max=%v weekly_switches=%d", min, max, weeklySwitches)
	}
	t.Logf("8h simulation: requests=%v weekly_remaining_range=%.1f..%.1f weekly_switches=%d", uses, min, max, weeklySwitches)
}

func TestSerialWeeklyCycleChangeAndChallengerChangeRestartEvidence(t *testing.T) {
	for _, change := range []string{"weekly_cycle", "challenger"} {
		t.Run(change, func(t *testing.T) {
			resetBanStoreForTest()
			now := time.Now()
			s := newWeeklyBalanceTestState(now)
			req := serialTestRequest()
			if change == "weekly_cycle" {
				s.quotas["backup"].Windows[1].ResetAt = now.Add(time.Minute)
			}
			s.serialPick(req, now)
			at := now.Add(2 * time.Minute)
			for _, id := range []string{"primary", "backup"} {
				observeWeeklyBalanceTestQuota(s, id, at)
			}
			if change == "weekly_cycle" {
				s.quotas["backup"].Windows[1].ResetAt = at.Add(7 * 24 * time.Hour)
			} else {
				q := s.quotas["backup"]
				q.AuthID, q.AuthIndex = "third", "index-third"
				s.quotas["third"] = q
				s.identities["index-third"] = "third"
				req.Candidates[0].ID = "third"
			}
			if got := s.serialPick(req, at); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 1 {
				t.Fatalf("%s reused confirmation: %#v evidence=%#v", change, got, s.serialWeeklyRebalance)
			}
		})
	}
}

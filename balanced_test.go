package main

import (
	"fmt"
	"math"
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

func TestADQSequentialCompletionsStayEvenWhileQuotaTelemetryLags(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now().UTC()
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "balanced"
	cfg.StatePath = ""
	state := &schedulerRuntimeState{
		cfg:                 cfg,
		quotas:              make(map[string]quotaSnapshot),
		identities:          make(map[string]string),
		balancedAccounts:    make(map[string]*balancedAccount),
		balancedSessions:    make(map[string]balancedSessionBinding),
		adqPolicy:           defaultADQPolicy(),
		adqReservations:     newADQReservationBook(),
		adqProviderCircuits: make(map[string]adqProviderCircuit),
		globalCostSamples:   []float64{.1, .1, .1, .1, .1, .1, .1, .1},
	}
	req := pluginapi.SchedulerPickRequest{Model: "gpt-5.6-luna", Provider: providerCodex}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("team-%d", i)
		snapshot := quotaSnapshot{
			AuthID: id, AuthIndex: id, RefreshedAt: now,
			Windows: []quotaWindow{
				{Class: "5h", WindowSeconds: 18000, UsedPercent: 0, Allowed: true, ResetAt: now.Add(5 * time.Hour), ObservedAt: now},
				{Class: "weekly", WindowSeconds: 604800, UsedPercent: 0, Allowed: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now},
			},
		}
		state.quotas[id] = snapshot
		state.identities[id] = id
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: providerCodex})
	}

	counts := make(map[string]int)
	for i := 0; i < 50; i++ {
		at := now.Add(time.Duration(i) * time.Millisecond)
		pick := state.balancedPick(withBalancedSession(req, fmt.Sprintf("fresh-%d", i)), at)
		if !pick.Handled || pick.AuthID == "" {
			t.Fatalf("pick %d failed: %#v", i, pick)
		}
		counts[pick.AuthID]++
		state.observeBalancedUsage(pluginapi.UsageRecord{
			Provider: providerCodex, Generate: true, AuthID: pick.AuthID, AuthIndex: pick.AuthID,
			Model: req.Model, RequestedAt: at,
		}, at.Add(500*time.Microsecond))
	}
	minPicks, maxPicks := 50, 0
	for i := 0; i < 5; i++ {
		value := counts[fmt.Sprintf("team-%d", i)]
		if value < minPicks {
			minPicks = value
		}
		if value > maxPicks {
			maxPicks = value
		}
	}
	if maxPicks-minPicks > 1 {
		t.Fatalf("stale telemetry produced uneven allocation: counts=%v", counts)
	}
	if state.adqReservations == nil {
		t.Fatal("ADQ reservations were not active")
	}
	_, settling := state.adqReservations.Settling("", now.Add(time.Second))
	if settling <= 0 {
		t.Fatal("completed token-less requests did not retain settling debt")
	}

	stickyReq := withBalancedSession(req, "sticky-conversation")
	first := state.balancedPick(stickyReq, now.Add(2*time.Second))
	for i := 1; i < 6; i++ {
		at := now.Add(2*time.Second + time.Duration(i)*time.Millisecond)
		next := state.balancedPick(stickyReq, at)
		if next.AuthID != first.AuthID {
			t.Fatalf("settlement waterline moved sticky session from %q to %q", first.AuthID, next.AuthID)
		}
		state.observeBalancedUsage(pluginapi.UsageRecord{
			Provider: providerCodex, Generate: true, AuthID: next.AuthID, AuthIndex: next.AuthID,
			Model: req.Model, RequestedAt: at,
		}, at.Add(500*time.Microsecond))
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

func balancedAllocationFixture(now time.Time, adq bool) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest) {
	s, req := balancedFixture(now)
	s.cfg.ReserveWeeklyPercent = 0
	s.quotas["a"].Windows[1].UsedPercent = 60
	s.quotas["a"].Windows[1].ResetAt = now.Add(24 * time.Hour)
	s.quotas["b"].Windows[1].ResetAt = now.Add(7 * 24 * time.Hour)
	if adq {
		s.adqReservations = newADQReservationBook()
		s.adqPolicy = defaultADQPolicy()
		s.adqPolicy.WeeklySafetyMargin = 0
		s.adqPolicy.QuotaSafetyMargin = 0
	}
	return s, req
}

func TestBalancedWeeklyAllocationPoliciesApplyToADQAndFallback(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, adq := range []bool{false, true} {
		for _, policy := range []string{"sustainable", "weekly_remaining"} {
			t.Run(fmt.Sprintf("adq=%t/%s", adq, policy), func(t *testing.T) {
				now := time.Now().UTC()
				s, req := balancedAllocationFixture(now, adq)
				s.cfg.SerialAllocationPolicy = policy
				const rounds = 270
				for i := 0; i < rounds; i++ {
					at := now.Add(time.Duration(i) * time.Millisecond)
					pick := s.balancedPick(withBalancedSession(req, fmt.Sprintf("allocation-%d", i)), at)
					if !pick.Handled {
						t.Fatalf("round %d was not admitted", i)
					}
					// Release the isolated fake work; keep unchanged observed quotas
					// so the configured distribution is independently measurable.
					s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: pick.AuthID, AuthIndex: pick.AuthID, Model: req.Model, RequestedAt: at, Failed: true}, at.Add(time.Microsecond))
				}
				wantA := 210.0 // 40/1 : 80/7 = 7:2
				if policy == "weekly_remaining" {
					wantA = 90 // 40 : 80 = 1:2
				}
				if got := s.balancedAccounts["a"].Picks; math.Abs(float64(got)-wantA) > 1 {
					t.Fatalf("wrong distribution: a=%d b=%d want a=%.0f", got, s.balancedAccounts["b"].Picks, wantA)
				}
				if adq && s.adqDecisions != rounds {
					t.Fatalf("ADQ path was bypassed: decisions=%d", s.adqDecisions)
				}
			})
		}
	}
}

func TestBalancedWeeklyPolicyChangePreservesExistingSession(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, adq := range []bool{false, true} {
		t.Run(fmt.Sprintf("adq=%t", adq), func(t *testing.T) {
			now := time.Now().UTC()
			s, req := balancedAllocationFixture(now, adq)
			s.cfg.SerialAllocationPolicy = "sustainable"
			existing := withBalancedSession(req, "existing-session")
			first := s.balancedPick(existing, now)
			if first.AuthID != "a" {
				t.Fatalf("daily-budget first choice: %+v", first)
			}
			s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: first.AuthID, AuthIndex: first.AuthID, Model: req.Model, RequestedAt: now, Failed: true}, now.Add(time.Microsecond))
			s.cfg.SerialAllocationPolicy = "weekly_remaining"
			freshAt := now.Add(time.Millisecond)
			fresh := s.balancedPick(withBalancedSession(req, "fresh-session"), now.Add(time.Millisecond))
			if fresh.AuthID != "b" {
				t.Fatalf("new policy did not affect fresh session: %+v", fresh)
			}
			s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: fresh.AuthID, AuthIndex: fresh.AuthID, Model: req.Model, RequestedAt: freshAt, Failed: true}, freshAt.Add(time.Microsecond))
			for i := 2; i < 8; i++ {
				at := now.Add(time.Duration(i) * time.Millisecond)
				next := s.balancedPick(existing, at)
				if next.AuthID != first.AuthID {
					t.Fatalf("policy change moved live session: %+v", next)
				}
				s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: next.AuthID, AuthIndex: next.AuthID, Model: req.Model, RequestedAt: at, Failed: true}, at.Add(time.Microsecond))
			}
		})
	}
}

func TestBalancedADQRejectionDoesNotMintCreditOrBypassReservations(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now().UTC()
	s, req := balancedAllocationFixture(now, true)
	for _, id := range []string{"a", "b"} {
		s.quotas[id].Windows[0].UsedPercent = 99.9999
	}
	for i := 0; i < 5; i++ {
		if pick := s.balancedPick(req, now.Add(time.Duration(i)*time.Millisecond)); pick.Handled {
			t.Fatalf("reservation failure used unreserved fallback: %+v", pick)
		}
	}
	for id, account := range s.balancedAccounts {
		if account.Credit != 0 || account.Picks != 0 || len(account.Pending) != 0 {
			t.Fatalf("rejected work changed allocation for %s: %+v", id, account)
		}
	}
	if s.adqReservationCollisions == 0 {
		t.Fatal("test never reached the reservation gate")
	}
}

func TestBalancedADQWeightsAccountForReservationsAndSettling(t *testing.T) {
	now := time.Now().UTC()
	s, req := balancedAllocationFixture(now, true)
	s.cfg.SerialAllocationPolicy = "weekly_remaining"
	// Equal percentages/reset anchors: only local unsettled work differs.
	s.quotas["a"] = quotaSnapshot{AuthID: "a", AuthIndex: "a", RefreshedAt: now, Windows: append([]quotaWindow(nil), s.quotas["b"].Windows...)}
	choices := []serialCandidate{}
	inputs := []adqAccountInput{}
	for _, candidate := range req.Candidates {
		choice := inspectSerialCandidate(candidate, s.quotas[candidate.ID], true, s.cfg, now)
		input, ok := s.adqInputForChoiceLocked(choice, now)
		if !ok {
			t.Fatal("missing ADQ input")
		}
		choices = append(choices, choice)
		inputs = append(inputs, input)
	}
	inputs[0].ReservationFiveHour = 1
	inputs[0].SettlingWeekly = 5
	_, weights := s.balancedADQAllocationLocked(choices, inputs, 1, now, s.adqPolicyLocked())
	if weights["a"] >= weights["b"] {
		t.Fatalf("local debt ignored: %v", weights)
	}
}

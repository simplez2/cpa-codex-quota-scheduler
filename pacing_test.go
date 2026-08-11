package main

import (
	"math"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestUsageCreditsMatchesKeeperCostFormula(t *testing.T) {
	pricing := map[string]modelPricing{
		"gpt-test": {
			Model:                "gpt-test",
			PromptPricePer1M:     3,
			CompletionPricePer1M: 15,
			CacheReadPricePer1M:  0.3,
			CacheWritePricePer1M: 3.75,
			PriceMultiplier:      1,
		},
	}
	credits, ok := usageCredits(pluginapi.UsageRecord{
		Model: "gpt-test",
		Detail: pluginapi.UsageDetail{
			InputTokens:         1_000_000,
			OutputTokens:        500_000,
			CacheReadTokens:     200_000,
			CacheCreationTokens: 100_000,
		},
	}, pricing)
	want := 0.7*3 + 0.2*0.3 + 0.1*3.75 + 0.5*15
	if !ok || math.Abs(credits-want) > 0.0000001 {
		t.Fatalf("credits=%v ok=%v; want %v", credits, ok, want)
	}
}

func TestRequestCostPredictionUsesConfiguredQuantile(t *testing.T) {
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		costSamples: map[string][]float64{
			costSampleKey("gpt-5.6-sol", "medium"): {1, 2, 3, 4, 5, 6, 7, 8},
		},
	}
	got := state.predictCostLocked(pluginapi.SchedulerPickRequest{
		Model: "gpt-5.6-sol",
		Options: pluginapi.SchedulerOptions{Metadata: map[string]any{
			"reasoning_effort": "medium",
		}},
	}, false)
	if math.Abs(got-6.25) > 0.0000001 {
		t.Fatalf("P75 prediction=%v; want 6.25", got)
	}
}

func TestCapacityCalibrationUsesKeeperWindowCostAndDelta(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(5 * 24 * time.Hour)
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), pacingAccounts: make(map[string]*accountPacingState)}
	first := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now,
		Windows: []quotaWindow{{
			Class:                   "weekly",
			UsedPercent:             10,
			Allowed:                 true,
			ResetAt:                 reset,
			WindowUsageCredits:      200,
			WindowUsageCreditsKnown: true,
		}},
	}
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": first}, now)
	second := first
	second.RefreshedAt = now.Add(time.Minute)
	second.Windows = append([]quotaWindow(nil), first.Windows...)
	second.Windows[0].UsedPercent = 12
	second.Windows[0].WindowUsageCredits = 240
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": second}, now.Add(time.Minute))
	estimate := state.pacingAccounts["acct"].Capacities["weekly"]
	if estimate.Samples < 2 || math.Abs(estimate.Credits-2000) > 0.0000001 {
		t.Fatalf("unexpected capacity estimate: %#v", estimate)
	}
}

func TestPacingPickWillNotCrossReserve(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"tight": {
				AuthID:      "tight",
				RefreshedAt: now,
				Windows:     []quotaWindow{{Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()), UsedPercent: 90, Allowed: true, ResetAt: now.Add(24 * time.Hour)}},
			},
			"safe": {
				AuthID:      "safe",
				RefreshedAt: now,
				Windows:     []quotaWindow{{Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()), UsedPercent: 50, Allowed: true, ResetAt: now.Add(24 * time.Hour)}},
			},
		},
		pacingAccounts: map[string]*accountPacingState{
			"tight": {Capacities: map[string]capacityEstimate{"weekly": {Credits: 100, Samples: 2}}, LastQuota: make(map[string]quotaCalibrationPoint)},
			"safe":  {Capacities: map[string]capacityEstimate{"weekly": {Credits: 1000, Samples: 2}}, LastQuota: make(map[string]quotaCalibrationPoint)},
		},
	}
	banStore = banState{bans: make(map[string]banEntry)}
	got, choices := state.pacingPick(pluginapi.SchedulerPickRequest{
		Model: "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "tight", Provider: providerCodex},
			{ID: "safe", Provider: providerCodex},
		},
	}, now)
	if !got.Handled || got.AuthID != "safe" || len(choices) != 1 {
		t.Fatalf("reserve-aware pick=%#v choices=%#v", got, choices)
	}
}

func TestShadowAuditsDynamicChoiceWithoutTakingOver(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "shadow"
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"five": {
				AuthID:      "five",
				RefreshedAt: now,
				Windows:     []quotaWindow{{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), UsedPercent: 80, Allowed: true, ResetAt: now.Add(150 * time.Minute)}},
			},
			"weekly": {
				AuthID:      "weekly",
				RefreshedAt: now,
				Windows:     []quotaWindow{{Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()), UsedPercent: 10, Allowed: true, ResetAt: now.Add(84 * time.Hour)}},
			},
		},
	}
	banStore = banState{bans: make(map[string]banEntry)}
	req := pluginapi.SchedulerPickRequest{
		Model: "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "weekly", Provider: providerCodex},
			{ID: "five", Provider: providerCodex},
		},
	}
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "five" {
		t.Fatalf("shadow returned %#v; want corrected legacy five", got)
	}
	if len(state.decisionHistory) != 1 || !state.decisionHistory[0].Disagreed || state.decisionHistory[0].DynamicAuthHash != shortIdentityHash("weekly") {
		t.Fatalf("shadow audit missing disagreement: %#v", state.decisionHistory)
	}
	state.cfg.SchedulerMode = "enforce"
	got, err = state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "weekly" {
		t.Fatalf("enforce returned %#v; want dynamic weekly", got)
	}
}

func TestStickyBindingRequiresHysteresisConfirmations(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.SwitchHysteresisPercent = 2
	cfg.SwitchConfirmations = 3
	state := schedulerRuntimeState{cfg: cfg, stickyBindings: make(map[string]stickyBinding)}
	req := pluginapi.SchedulerPickRequest{Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Session-ID": {"private-session"}}}}
	first := state.applyStickyLocked(req, []pacingCandidate{
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "a"}, ScorePercent: 10},
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "b"}, ScorePercent: 5},
	}, now)
	if first.Candidate.ID != "a" {
		t.Fatalf("initial sticky pick=%q", first.Candidate.ID)
	}
	challenger := []pacingCandidate{
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "b"}, ScorePercent: 15},
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "a"}, ScorePercent: 10},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		got := state.applyStickyLocked(req, challenger, now.Add(time.Duration(attempt)*time.Second))
		if got.Candidate.ID != "a" {
			t.Fatalf("switched on confirmation %d: %q", attempt, got.Candidate.ID)
		}
	}
	got := state.applyStickyLocked(req, challenger, now.Add(3*time.Second))
	if got.Candidate.ID != "b" || state.sessionSwitches != 1 {
		t.Fatalf("third confirmation did not switch: pick=%q switches=%d", got.Candidate.ID, state.sessionSwitches)
	}
	for key := range state.stickyBindings {
		if key == "private-session" {
			t.Fatal("raw session identifier was stored")
		}
	}
}

func TestPredictionDebitIsReconciledWhenUsageHasNoTokens(t *testing.T) {
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), pacingAccounts: make(map[string]*accountPacingState)}
	state.recordPredictedDebit("acct", 3)
	state.observeUsageCost(pluginapi.UsageRecord{Provider: providerCodex, AuthID: "acct", Generate: true})
	account := state.pacingAccounts["acct"]
	if account.DeficitCredits != 0 || len(account.PendingPredicted) != 0 {
		t.Fatalf("speculative debit was not reconciled: %#v", account)
	}
}

func TestHealthyLegacyChoiceDoesNotSpendResetCreditFirst(t *testing.T) {
	now := time.Now()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotas: map[string]quotaSnapshot{
			"credit": {AuthID: "credit", ResetCredits: 1, RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 20, Allowed: true}}},
			"plain":  {AuthID: "plain", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 10, Allowed: true}}},
		},
	}
	banStore = banState{bans: make(map[string]banEntry)}
	got, err := state.legacySchedulerPick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "credit", Provider: providerCodex},
		{ID: "plain", Provider: providerCodex},
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthID != "plain" {
		t.Fatalf("healthy reset-credit account was consumed first: %#v", got)
	}
}

func TestMixedProviderRouteAlwaysFallsThrough(t *testing.T) {
	state := schedulerRuntimeState{cfg: defaultPluginConfig()}
	got, err := state.schedulerPick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "codex", Provider: providerCodex},
		{ID: "third-party", Provider: "openai"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Handled {
		t.Fatalf("mixed-provider route was intercepted: %#v", got)
	}
}

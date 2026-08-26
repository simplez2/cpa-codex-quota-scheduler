package main

import (
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func projectedFiveHourState(now time.Time, used, predicted float64) schedulerRuntimeState {
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	cfg.Serial5hHandoffMode = "custom_threshold"
	cfg.Serial5hSwitchPercent = 95
	cfg.SerialSwitchPercent = 98
	state := schedulerRuntimeState{
		cfg:                cfg,
		serialActiveAuthID: "primary",
		serialSelectionSource: "auto",
		quotas: map[string]quotaSnapshot{
			"primary": {
				AuthID: "primary", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), UsedPercent: used, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
					{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
				},
			},
			"backup": {
				AuthID: "backup", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
					{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
				},
			},
		},
		warmups: make(map[string]warmupEntry),
		costSamples: map[string][]float64{},
		pacingAccounts: map[string]*accountPacingState{
			"primary": {Capacities: map[string]capacityEstimate{"5h": {Credits: 100, Samples: 8}}, LastQuota: map[string]quotaCalibrationPoint{}},
			"backup":  {Capacities: map[string]capacityEstimate{"5h": {Credits: 100, Samples: 8}}, LastQuota: map[string]quotaCalibrationPoint{}},
		},
	}
	samples := make([]float64, minimumQuantileSamples)
	for i := range samples {
		samples[i] = predicted
	}
	state.costSamples[costSampleKey("gpt-5.6-sol", "*")] = samples
	return state
}

func projectedFiveHourRequest() pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "primary", Provider: providerCodex, Priority: 10},
			{ID: "backup", Provider: providerCodex, Priority: 10},
		},
	}
}

func TestSerialProjectedFiveHourUsageHandsOffBeforeObservedThreshold(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := projectedFiveHourState(now, 94, 2)

	got := state.serialPick(projectedFiveHourRequest(), now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("projected 5h threshold did not move request to backup: %#v", got)
	}
	if state.serialLastSwitchReason != "serial_projected_threshold" {
		t.Fatalf("switch reason = %q; want serial_projected_threshold", state.serialLastSwitchReason)
	}
	if pending := state.pacingAccounts["backup"].PendingPredicted; len(pending) != 1 || pending[0] != 2 {
		t.Fatalf("backup predicted debit = %#v; want one 2-credit reservation", pending)
	}
}

func TestSerialProjectedFiveHourAdmissionSerializesConcurrentReservations(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := projectedFiveHourState(now, 94, 0.6)
	req := projectedFiveHourRequest()

	const workers = 32
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- state.serialPick(req, now)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	primaryPicks := 0
	backupPicks := 0
	for got := range results {
		if !got.Handled {
			t.Fatalf("concurrent admission returned unhandled: %#v", got)
		}
		switch got.AuthID {
		case "primary":
			primaryPicks++
		case "backup":
			backupPicks++
		default:
			t.Fatalf("unexpected auth: %#v", got)
		}
	}
	if primaryPicks > 1 {
		t.Fatalf("%d concurrent requests were admitted to primary at 94%% with 0.6%% predicted cost; want at most 1", primaryPicks)
	}
	if backupPicks == 0 {
		t.Fatal("projected admission never handed concurrent traffic to backup")
	}
}

func TestSerialProjectedFiveHourGuardFailsOpenWithoutCapacityEstimate(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := projectedFiveHourState(now, 94, 2)
	state.pacingAccounts = make(map[string]*accountPacingState)

	got := state.serialPick(projectedFiveHourRequest(), now)
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("unknown capacity fabricated a projected threshold: %#v", got)
	}
}

func TestSerialProjectedFiveHourGuardUsesKeeperWindowCostBootstrap(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := projectedFiveHourState(now, 94, 2)
	state.pacingAccounts = make(map[string]*accountPacingState)
	primary := state.quotas["primary"]
	primary.Windows[0].WindowUsageCreditsKnown = true
	primary.Windows[0].WindowUsageCredits = 94
	state.quotas["primary"] = primary

	got := state.serialPick(projectedFiveHourRequest(), now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("Keeper window-cost capacity bootstrap did not guard the request: %#v", got)
	}
}

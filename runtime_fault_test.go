package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}

func TestPartialQuotaProbeSnapshotCarriesForwardFreshMissingWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now.Add(-time.Minute),
		Windows: []quotaWindow{
			{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now.Add(-time.Minute)},
			{Class: "monthly", UsedPercent: 60, Allowed: true, ResetAt: now.Add(20 * 24 * time.Hour), ObservedAt: now.Add(-time.Minute)},
		},
	}
	current := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now,
		Windows:     []quotaWindow{{Class: "weekly", UsedPercent: 21, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now}},
	}
	merged := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	if len(merged.Windows) != 2 {
		t.Fatalf("partial snapshot lost an active window: %#v", merged.Windows)
	}
	var monthly quotaWindow
	for _, window := range merged.Windows {
		if window.Class == "monthly" {
			monthly = window
		}
	}
	if monthly.Class == "" || !monthly.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("monthly carry-forward missing or freshness changed: %#v", monthly)
	}
	if !quotaSnapshotFresh(merged, now, 15*time.Minute) {
		t.Fatal("fresh carried window should remain schedulable")
	}
	if quotaSnapshotFresh(merged, now.Add(16*time.Minute), 15*time.Minute) {
		t.Fatal("stale carried window kept the account schedulable indefinitely")
	}
}

func TestPartialQuotaProbeSnapshotDoesNotCarryExpiredWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now.Add(-time.Minute),
		Windows:     []quotaWindow{{Class: "monthly", UsedPercent: 100, Allowed: false, ResetAt: now.Add(-time.Second), ObservedAt: now.Add(-time.Minute)}},
	}
	current := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ObservedAt: now}}}
	merged := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	if len(merged.Windows) != 1 || merged.Windows[0].Class != "weekly" {
		t.Fatalf("expired prior-cycle window was carried forward: %#v", merged.Windows)
	}
}

func TestCapacityCalibrationDoesNotUseCrossResetDelta(t *testing.T) {
	now := time.Now().UTC()
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), pacingAccounts: make(map[string]*accountPacingState)}
	first := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{{
		Class: "weekly", UsedPercent: 90, Allowed: true, ResetAt: now.Add(time.Hour), ObservedAt: now,
		WindowUsageCredits: 900, WindowUsageCreditsKnown: true,
	}}}
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": first}, now)
	secondObserved := now.Add(2 * time.Hour)
	second := quotaSnapshot{AuthID: "acct", RefreshedAt: secondObserved, Windows: []quotaWindow{{
		Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: secondObserved,
		WindowUsageCredits: 10, WindowUsageCreditsKnown: true,
	}}}
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": second}, secondObserved)
	estimate := state.pacingAccounts["acct"].Capacities["weekly"]
	if estimate.Samples != 2 || estimate.Credits != 1000 {
		t.Fatalf("cross-reset delta polluted capacity estimate: %#v", estimate)
	}
}

func TestHotReloadIsRaceSafeAndPreservesPacingState(t *testing.T) {
	resetBanStoreForTest()
	statePath := filepath.Join(t.TempDir(), "state.json")
	baseCfg := defaultPluginConfig()
	baseCfg.Enabled = false
	baseCfg.SchedulerMode = "legacy"
	baseCfg.StatePath = statePath
	now := time.Now()
	schedulerRuntime.stop()
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = baseCfg
	schedulerRuntime.quotas = map[string]quotaSnapshot{"acct": {
		AuthID: "acct", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: now.Add(24 * time.Hour), ObservedAt: now}},
	}}
	schedulerRuntime.identities = make(map[string]string)
	schedulerRuntime.pricing = make(map[string]modelPricing)
	schedulerRuntime.costSamples = map[string][]float64{"sentinel": {1, 2, 3}}
	schedulerRuntime.pacingAccounts = map[string]*accountPacingState{"acct": {
		DeficitCredits: 7,
		Capacities:     map[string]capacityEstimate{"weekly": {Credits: 1000, Samples: 4, UpdatedAt: now}},
		LastQuota:      make(map[string]quotaCalibrationPoint),
	}}
	schedulerRuntime.stickyBindings = make(map[string]stickyBinding)
	schedulerRuntime.mu.Unlock()

	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "acct", Provider: providerCodex}}}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_, _ = schedulerRuntime.schedulerPick(req)
			}
		}()
	}
	for iteration := 0; iteration < 20; iteration++ {
		raw := []byte(fmt.Sprintf("enabled: false\nscheduler_mode: legacy\nstate_path: %q\n", statePath))
		configureSchedulerRuntime(raw)
		refreshedAt := time.Now()
		schedulerRuntime.mu.Lock()
		schedulerRuntime.quotas = map[string]quotaSnapshot{"acct": {
			AuthID: "acct", RefreshedAt: refreshedAt,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: refreshedAt.Add(24 * time.Hour), ObservedAt: refreshedAt}},
		}}
		schedulerRuntime.mu.Unlock()
	}
	wg.Wait()

	schedulerRuntime.mu.RLock()
	account := schedulerRuntime.pacingAccounts["acct"]
	generation := schedulerRuntime.configGeneration
	samples := append([]float64(nil), schedulerRuntime.costSamples["sentinel"]...)
	schedulerRuntime.mu.RUnlock()
	if account == nil || account.Capacities["weekly"].Samples != 4 || len(samples) != 3 {
		t.Fatalf("hot reload discarded pacing state: account=%#v samples=%v", account, samples)
	}
	if generation < 20 {
		t.Fatalf("config generation=%d; want at least 20", generation)
	}
}

func TestRuntimeStatusExposesShadowPacingAndQuarantineMetrics(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg:              cfg,
		configGeneration: 7,
		quotas: map[string]quotaSnapshot{"acct": {
			AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
			Windows: []quotaWindow{{
				Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now,
				WindowUsageCredits: 250, WindowUsageCreditsKnown: true,
			}},
		}},
		pricing:           map[string]modelPricing{"gpt-5.6-sol": builtInFallbackPricing("gpt-5.6-sol")},
		globalCostSamples: []float64{1, 2, 3, 4, 5, 6, 7, 8},
		costSamples: map[string][]float64{
			costSampleKey("gpt-5.6-sol", "medium"): {1, 2, 3, 4, 5, 6, 7, 8},
		},
		pacingAccounts: map[string]*accountPacingState{"acct": {
			DeficitCredits: 10,
			LastAccruedAt:  now,
			Capacities: map[string]capacityEstimate{
				"weekly": {Credits: 1000, Samples: 4, UpdatedAt: now},
			},
			LastQuota:        make(map[string]quotaCalibrationPoint),
			PendingPredicted: []float64{3},
		}},
		stickyBindings: map[string]stickyBinding{"session-hash": {AuthID: "acct", LastUsedAt: now}},
		decisionHistory: []schedulerDecisionAudit{{
			At: now, Mode: "shadow", Model: "gpt-5.6-sol", SessionHash: "session-hash",
			LegacyAuthHash: "legacy-hash", DynamicAuthHash: "dynamic-hash", ReturnedAuthHash: "legacy-hash", Disagreed: true,
		}},
		sessionSwitches:     2,
		shadowDisagreements: 3,
		warmups:             make(map[string]warmupEntry),
	}
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})

	status := state.status()
	if status.ConfigGeneration != 7 || status.StickyBindings != 1 || status.SessionSwitches != 2 || status.ShadowDisagreements != 3 {
		t.Fatalf("missing scheduler counters: %#v", status)
	}
	if len(status.CostProfiles) == 0 || status.CostProfiles[0].Samples != 8 || status.CostProfiles[0].P95 <= status.CostProfiles[0].P75 {
		t.Fatalf("cost quantiles missing: %#v", status.CostProfiles)
	}
	if len(status.Pacing) != 1 || len(status.Pacing[0].Windows) != 1 || status.Pacing[0].Windows[0].CapacitySamples != 4 || status.Pacing[0].PendingPredictedRequests != 1 {
		t.Fatalf("pacing status missing: %#v", status.Pacing)
	}
	if status.Quarantine.ProbeReady != 1 || status.Quarantine.Probation != 1 {
		t.Fatalf("quarantine status missing: %#v", status.Quarantine)
	}
	if len(status.RecentDecisions) != 1 || status.RecentDecisions[0].ReturnedAuthHash != "legacy-hash" {
		t.Fatalf("decision history missing or unredacted: %#v", status.RecentDecisions)
	}
	if len(status.Snapshots) != 1 || len(status.Snapshots[0].Windows) != 1 || status.Snapshots[0].Windows[0].ObservedAt == "" {
		t.Fatalf("per-window freshness missing: %#v", status.Snapshots)
	}
}

func TestConcurrentPersistenceKeepsLatestQuarantineState(t *testing.T) {
	resetBanStoreForTest()
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := defaultPluginConfig()
	cfg.StatePath = path
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	state.initializeGenerationOwnership(path)
	if err := state.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &state)

	const entries = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < entries; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			authID := fmt.Sprintf("acct-%02d", index)
			banStore.set(authID, banEntry{
				ResetAt: time.Now().Add(time.Hour), Window: "probation", Kind: banKindProbation,
			})
			state.persistBanState()
		}(index)
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != 6 || len(persisted.Bans) != entries {
		t.Fatalf("persisted state version=%d bans=%d; want version 6 and %d bans", persisted.Version, len(persisted.Bans), entries)
	}
}

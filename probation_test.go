package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func resetBanStoreForTest() {
	banStore = banState{bans: make(map[string]banEntry)}
}

func TestHeaderless429CreatesProbation(t *testing.T) {
	resetBanStoreForTest()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = cfg
	schedulerRuntime.identities = make(map[string]string)
	schedulerRuntime.quotas = make(map[string]quotaSnapshot)
	schedulerRuntime.pricing = make(map[string]modelPricing)
	schedulerRuntime.pacingAccounts = make(map[string]*accountPacingState)
	schedulerRuntime.mu.Unlock()

	raw, err := json.Marshal(pluginapi.UsageRecord{
		Provider: providerCodex,
		AuthID:   "acct",
		Generate: true,
		Failed:   true,
		Failure:  pluginapi.UsageFailure{StatusCode: statusTooManyRequests},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleUsage(raw); err != nil {
		t.Fatal(err)
	}
	entry, ok := banStore.lookup("acct")
	if !ok {
		t.Fatal("headerless 429 did not create quarantine state")
	}
	if entry.Kind != banKindProbation || entry.Phase != banPhaseCooldown {
		t.Fatalf("entry = %#v; want probation cooldown", entry)
	}
	if entry.ResetAt.Before(time.Now().Add(10 * time.Minute)) {
		t.Fatalf("probation deadline too short: %v", entry.ResetAt)
	}
}

func TestExpiredProbationRequiresSingleHalfOpenProbe(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute),
		Window:  "probation",
		Kind:    banKindProbation,
		Phase:   banPhaseCooldown,
	})
	if !banStore.schedulable("acct", now) {
		t.Fatal("expired probation should be probe-ready")
	}

	const workers = 32
	start := make(chan struct{})
	results := make(chan [2]bool, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute)
			results <- [2]bool{allowed, started}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	allowedCount := 0
	startedCount := 0
	for result := range results {
		if result[0] {
			allowedCount++
		}
		if result[1] {
			startedCount++
		}
	}
	if allowedCount != 1 || startedCount != 1 {
		t.Fatalf("allowed=%d started=%d; want one half-open probe", allowedCount, startedCount)
	}
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Phase != banPhaseHalfOpen || entry.ProbeAttempts != 1 {
		t.Fatalf("half-open state = %#v, ok=%v", entry, ok)
	}
}

func TestHalfOpenSuccessClearsProbation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})
	allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute)
	if !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	_, cleared, changed := banStore.completeProbe("acct", now.Add(time.Second), now.Add(2*time.Second), true, time.Minute)
	if !changed || !cleared {
		t.Fatalf("changed=%v cleared=%v; want successful clear", changed, cleared)
	}
	if _, ok := banStore.lookup("acct"); ok {
		t.Fatal("successful half-open probe left quarantine state behind")
	}
}

func TestHalfOpenNon429FailureReturnsToShortCooldown(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	entry, cleared, changed := banStore.completeProbe("acct", now.Add(time.Second), now.Add(2*time.Second), false, 90*time.Second)
	if !changed || cleared {
		t.Fatalf("changed=%v cleared=%v; want retry cooldown", changed, cleared)
	}
	if entry.Phase != banPhaseCooldown || !entry.ResetAt.Equal(now.Add(92*time.Second)) {
		t.Fatalf("retry entry = %#v", entry)
	}
}

func TestHalfOpen429RestartsCooldownWithoutShorteningIt(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	firstReset := now.Add(5 * time.Minute)
	banStore.record429("acct", banEntry{
		ResetAt:  firstReset,
		Window:   "probation",
		BannedAt: now.Add(time.Second),
		Kind:     banKindProbation,
	}, now.Add(time.Second))
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Phase != banPhaseCooldown || !entry.ResetAt.Equal(firstReset) || !entry.ProbeStartedAt.IsZero() {
		t.Fatalf("429 did not close half-open probe safely: %#v, ok=%v", entry, ok)
	}
	banStore.record429("acct", banEntry{
		ResetAt:  now.Add(time.Minute),
		Window:   "probation",
		BannedAt: now.Add(2 * time.Second),
		Kind:     banKindProbation,
	}, now.Add(2*time.Second))
	entry, _ = banStore.lookup("acct")
	if !entry.ResetAt.Equal(firstReset) {
		t.Fatalf("repeated 429 shortened cooldown to %v; want %v", entry.ResetAt, firstReset)
	}
	stats := banStore.stats()
	if stats.Total429s != 2 || stats.Probation429s != 2 || stats.ProbeFailures != 1 {
		t.Fatalf("unexpected state counters: %#v", stats)
	}
}

func TestStatusReadRetainsProbeReadyProbation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})
	first := currentBanStatus()
	second := currentBanStatus()
	if first.Count != 1 || second.Count != 1 || first.Bans[0].State != string(banDispositionProbeReady) {
		t.Fatalf("status reads changed probe-ready state: first=%#v second=%#v", first, second)
	}
	if _, ok := banStore.lookup("acct"); !ok {
		t.Fatal("status read deleted probe-ready probation")
	}
}

func TestLoadBanStateRetainsExpiredAndHalfOpenEntries(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "state.json")
	state := persistedBanState{
		Version: 2,
		Bans: map[string]banEntry{
			"expired": {
				ResetAt:  now.Add(-time.Minute),
				Window:   "temporary fallback (headers missing)",
				BannedAt: now.Add(-20 * time.Minute),
			},
			"probing": {
				ResetAt:         now.Add(-2 * time.Minute),
				Window:          "probation",
				Kind:            banKindProbation,
				Phase:           banPhaseHalfOpen,
				ProbeStartedAt:  now.Add(-time.Minute),
				ProbeLeaseUntil: now.Add(10 * time.Minute),
				ProbeAttempts:   1,
			},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	loadBanState(path)

	expired, ok := banStore.lookup("expired")
	if !ok || expired.Kind != banKindProbation || banEntryDisposition(expired, now) != banDispositionProbeReady {
		t.Fatalf("expired legacy entry was not restored as probe-ready: %#v, ok=%v", expired, ok)
	}
	probing, ok := banStore.lookup("probing")
	if !ok || banEntryDisposition(probing, now) != banDispositionHalfOpen || probing.ProbeAttempts != 1 {
		t.Fatalf("half-open lease was not restored: %#v, ok=%v", probing, ok)
	}
}

func TestSchedulerAllowsOnlyOneConcurrentHalfOpenDecision(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "legacy"
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"acct": {
				AuthID:      "acct",
				RefreshedAt: now,
				Windows: []quotaWindow{{
					Class:       "5h",
					UsedPercent: 1,
					Allowed:     true,
					ResetAt:     now.Add(4 * time.Hour),
				}},
			},
		},
		decisionHistory: make([]schedulerDecisionAudit, 0),
	}
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})
	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "acct", Provider: providerCodex}}}

	const workers = 24
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response, err := state.schedulerPick(req)
			results <- response
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("schedulerPick error: %v", err)
		}
	}
	handled := 0
	for response := range results {
		if response.Handled {
			handled++
			if response.AuthID != "acct" {
				t.Fatalf("unexpected auth id: %#v", response)
			}
		}
	}
	if handled != 1 {
		t.Fatalf("handled decisions=%d; want exactly one half-open request", handled)
	}
}

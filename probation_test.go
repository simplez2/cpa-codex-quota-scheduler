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

func TestHalfOpenCompletionRejectsRequestPredatingProbe(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	bannedAt := now.Add(-time.Hour)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota,
	})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	_, cleared, changed := banStore.completeProbe(
		"acct",
		now.Add(-500*time.Millisecond),
		now.Add(time.Second),
		true,
		time.Minute,
	)
	if changed || cleared {
		t.Fatalf("pre-probe completion changed quarantine: changed=%v cleared=%v", changed, cleared)
	}
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Phase != banPhaseHalfOpen || !entry.BannedAt.Equal(bannedAt) || !entry.ProbeStartedAt.Equal(now) {
		t.Fatalf("pre-probe completion disturbed half-open identity: entry=%#v ok=%v", entry, ok)
	}
}

func TestHalfOpenCompletionRejectsMissingRequestedAt(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	bannedAt := now.Add(-time.Hour)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota,
	})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	_, cleared, changed := banStore.completeProbe("acct", time.Time{}, now.Add(time.Second), true, time.Minute)
	if changed || cleared {
		t.Fatalf("missing RequestedAt changed quarantine: changed=%v cleared=%v", changed, cleared)
	}
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Phase != banPhaseHalfOpen || !entry.BannedAt.Equal(bannedAt) || !entry.ProbeStartedAt.Equal(now) {
		t.Fatalf("missing RequestedAt disturbed half-open identity: entry=%#v ok=%v", entry, ok)
	}
}

func TestRollbackProbeRequiresExactHalfOpenIdentity(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	bannedAt := now.Add(-time.Hour)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota,
	})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	if banStore.rollbackProbe("acct", bannedAt.Add(time.Second), now) {
		t.Fatal("rollback accepted a different ban identity")
	}
	if banStore.rollbackProbe("acct", bannedAt, now.Add(time.Second)) {
		t.Fatal("rollback accepted a different probe identity")
	}
	if !banStore.rollbackProbe("acct", bannedAt, now) {
		t.Fatal("exact half-open identity was not rolled back")
	}
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Phase != banPhaseCooldown || entry.ProbeAttempts != 0 || banEntryDisposition(entry, now) != banDispositionProbeReady {
		t.Fatalf("rollback state = %#v ok=%v; want original probe-ready cooldown", entry, ok)
	}
	if stats := banStore.stats(); stats.ProbeStarts != 0 {
		t.Fatalf("rolled-back probe remained counted: %#v", stats)
	}
}

func TestBlockedProbeCannotAutoRecoverOrBeWeakenedByDelayed429(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	bannedAt := now.Add(-time.Hour)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota,
	})
	if allowed, started := banStore.tryStartProbe("acct", now, 10*time.Minute); !allowed || !started {
		t.Fatal("failed to start half-open probe")
	}
	if !banStore.blockProbe("acct", bannedAt, now, "cyber_policy", now.Add(time.Second)) {
		t.Fatal("terminal failure did not block the exact probe")
	}
	blocked, ok := banStore.lookup("acct")
	if !ok || blocked.Kind != banKindBlocked || !blocked.ResetAt.IsZero() || banStore.schedulable("acct", now.Add(365*24*time.Hour)) {
		t.Fatalf("terminal quarantine is not permanent: entry=%#v ok=%v", blocked, ok)
	}
	if allowed, started := banStore.tryStartProbe("acct", now.Add(365*24*time.Hour), time.Minute); allowed || started {
		t.Fatalf("terminal quarantine entered half-open automatically: allowed=%v started=%v", allowed, started)
	}

	banStore.record429("acct", banEntry{
		ResetAt: now.Add(2 * time.Hour), BannedAt: now.Add(2 * time.Second),
		Window: "5h", Kind: banKindQuota,
	}, now.Add(2*time.Second))
	after429, ok := banStore.lookup("acct")
	if !ok || after429.Kind != banKindBlocked || !after429.ResetAt.IsZero() || !after429.BannedAt.Equal(blocked.BannedAt) {
		t.Fatalf("delayed 429 weakened terminal quarantine: before=%#v after=%#v ok=%v", blocked, after429, ok)
	}
	if removed := banStore.clearBlocked("acct", false); removed != 1 || !banStore.schedulable("acct", now) {
		t.Fatalf("explicit retry did not clear terminal quarantine: removed=%d", removed)
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

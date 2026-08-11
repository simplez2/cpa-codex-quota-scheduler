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

func newSerialTestState(now time.Time) schedulerRuntimeState {
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	return schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"primary": {
				AuthID: "primary", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 60, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now}},
			},
			"backup": {
				AuthID: "backup", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 10, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now}},
			},
		},
		warmups: make(map[string]warmupEntry),
	}
}

func serialTestRequest() pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "backup", Provider: providerCodex, Priority: 10},
			{ID: "primary", Provider: providerCodex, Priority: 10},
		},
	}
}

func TestSerialSchedulerUsesOneGlobalAuthAcrossSessions(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"session-a"}}
	first, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Handled || first.AuthID != "primary" {
		t.Fatalf("initial serial pick = %#v", first)
	}

	state.mu.Lock()
	backup := state.quotas["backup"]
	backup.Windows[0].UsedPercent = 90
	state.quotas["backup"] = backup
	state.mu.Unlock()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"session-b"}}
	second, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if second.AuthID != "primary" {
		t.Fatalf("another session preempted active auth: %#v", second)
	}
}

func TestSerialSchedulerSwitchesAtConfiguredThreshold(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	if got, _ := state.schedulerPick(req); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}

	state.mu.Lock()
	primary := state.quotas["primary"]
	primary.Windows[0].UsedPercent = 98
	state.quotas["primary"] = primary
	state.mu.Unlock()
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("threshold did not switch to backup: %#v", got)
	}
	if state.serialSwitches != 1 || state.serialLastSwitchReason != "serial_threshold" {
		t.Fatalf("switch state = count %d reason %q", state.serialSwitches, state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerPreemptsMonthlyWhenWeeklyBecomesEligible(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"monthly": {
				AuthID: "monthly", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "monthly", UsedPercent: 4, Allowed: true, ResetAt: now.Add(30 * 24 * time.Hour), ObservedAt: now}},
			},
			"weekly": {
				AuthID: "weekly", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now}},
			},
		},
		warmups: make(map[string]warmupEntry),
	}
	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "monthly", Provider: providerCodex, Priority: 10},
			{ID: "weekly", Provider: providerCodex, Priority: 10},
		},
	}

	if got, err := state.schedulerPick(req); err != nil || got.AuthID != "monthly" {
		t.Fatalf("initial monthly pick = %#v, err=%v", got, err)
	}
	state.mu.Lock()
	weekly := state.quotas["weekly"]
	weekly.Windows[0].UsedPercent = 0
	weekly.Windows[0].LimitReached = false
	refreshedAt := time.Now()
	weekly.RefreshedAt = refreshedAt
	weekly.Windows[0].ObservedAt = refreshedAt
	state.quotas["weekly"] = weekly
	state.mu.Unlock()

	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "weekly" {
		t.Fatalf("weekly reset did not preempt monthly: %#v", got)
	}
	if state.serialActiveAuthID != "weekly" || state.serialSwitches != 1 || state.serialLastSwitchReason != "higher_priority_window_available" {
		t.Fatalf("preemption state auth=%q switches=%d reason=%q", state.serialActiveAuthID, state.serialSwitches, state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerHigherPriorityPreemptionIsAtomic(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg:                cfg,
		serialActiveAuthID: "monthly",
		quotas: map[string]quotaSnapshot{
			"monthly": {
				AuthID: "monthly", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "monthly", UsedPercent: 4, Allowed: true, ResetAt: now.Add(30 * 24 * time.Hour), ObservedAt: now}},
			},
			"weekly": {
				AuthID: "weekly", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 0, Allowed: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now}},
			},
		},
		warmups: make(map[string]warmupEntry),
	}
	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "monthly", Provider: providerCodex, Priority: 10},
			{ID: "weekly", Provider: providerCodex, Priority: 10},
		},
	}

	const workers = 64
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := state.schedulerPick(req)
			results <- got
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for got := range results {
		if !got.Handled || got.AuthID != "weekly" {
			t.Fatalf("concurrent preemption pick = %#v", got)
		}
	}
	if state.serialActiveAuthID != "weekly" || state.serialSwitches != 1 || state.serialLastSwitchReason != "higher_priority_window_available" {
		t.Fatalf("atomic preemption state auth=%q switches=%d reason=%q", state.serialActiveAuthID, state.serialSwitches, state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerSwitchesAfterQuarantine(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	if got, _ := state.schedulerPick(req); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	banStore.set("primary", banEntry{ResetAt: now.Add(time.Hour), Window: "probation", Kind: banKindProbation})
	if !state.markSerialUnavailable("primary", "429", now) {
		t.Fatal("429 did not release the active auth")
	}
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "backup" || state.serialLastSwitchReason != "429" {
		t.Fatalf("quarantine failover = %#v reason=%q", got, state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerConcurrentPicksClaimOnlyOneAuth(t *testing.T) {
	resetBanStoreForTest()
	state := newSerialTestState(time.Now())
	req := serialTestRequest()
	const workers = 64
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := state.schedulerPick(req)
			results <- got
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for got := range results {
		if !got.Handled || got.AuthID != "primary" {
			t.Fatalf("concurrent serial pick escaped primary: %#v", got)
		}
	}
}

func TestSerialSchedulerRetrySubsetUsesStableProvisionalWithoutSwitch(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	if got := state.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	retry := serialTestRequest()
	retry.Candidates = retry.Candidates[:1]

	first := state.serialPick(retry, now.Add(time.Second))
	second := state.serialPick(retry, now.Add(30*time.Second))
	if !first.Handled || first.AuthID != "backup" || !second.Handled || second.AuthID != "backup" {
		t.Fatalf("retry fallback picks = %#v then %#v", first, second)
	}
	if state.serialActiveAuthID != "primary" || state.serialSwitches != 0 {
		t.Fatalf("retry subset changed committed auth=%q switches=%d", state.serialActiveAuthID, state.serialSwitches)
	}
	if state.serialFallbackAuthID != "backup" || state.serialFallbacks != 2 {
		t.Fatalf("provisional auth=%q fallbacks=%d", state.serialFallbackAuthID, state.serialFallbacks)
	}
}

func TestSerialSchedulerActiveReappearsClearsProvisional(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	if got := state.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	retry := req
	retry.Candidates = retry.Candidates[:1]
	if got := state.serialPick(retry, now.Add(time.Second)); got.AuthID != "backup" {
		t.Fatalf("provisional pick = %#v", got)
	}
	if got := state.serialPick(req, now.Add(30*time.Second)); got.AuthID != "primary" {
		t.Fatalf("reappeared active pick = %#v", got)
	}
	if state.serialMissingAuthID != "" || state.serialFallbackAuthID != "" || !state.serialMissingSince.IsZero() || state.serialMissingCount != 0 {
		t.Fatalf("missing state not cleared: auth=%q provisional=%q since=%v count=%d", state.serialMissingAuthID, state.serialFallbackAuthID, state.serialMissingSince, state.serialMissingCount)
	}
	if state.serialSwitches != 0 {
		t.Fatalf("reappearance counted as %d switches", state.serialSwitches)
	}
}

func TestSerialSchedulerCommitsConfirmedMissingAfterGrace(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	if got := state.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	retry := serialTestRequest()
	retry.Candidates = retry.Candidates[:1]
	if got := state.serialPick(retry, now.Add(time.Second)); got.AuthID != "backup" {
		t.Fatalf("first provisional pick = %#v", got)
	}
	if got := state.serialPick(retry, now.Add(30*time.Second)); got.AuthID != "backup" {
		t.Fatalf("second provisional pick = %#v", got)
	}
	confirmedAt := now.Add(time.Second + serialCandidateMissingGrace)
	if got := state.serialPick(retry, confirmedAt); !got.Handled || got.AuthID != "backup" {
		t.Fatalf("confirmed pick = %#v", got)
	}
	if state.serialActiveAuthID != "backup" || state.serialSwitches != 1 || state.serialLastSwitchReason != "candidate_unavailable_confirmed" {
		t.Fatalf("confirmed state auth=%q switches=%d reason=%q", state.serialActiveAuthID, state.serialSwitches, state.serialLastSwitchReason)
	}
	if state.serialFallbacks != 2 {
		t.Fatalf("provisional fallbacks=%d; want 2", state.serialFallbacks)
	}
}

func TestSerialSchedulerPinnedRequestDoesNotChangeCommittedAuth(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	if got := state.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	pinned := serialTestRequest()
	pinned.Candidates = pinned.Candidates[:1]
	pinned.Options.Metadata = map[string]any{"pinned_auth_id": "backup"}
	got := state.serialPick(pinned, now.Add(time.Second))
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("pinned pick = %#v", got)
	}
	if state.serialActiveAuthID != "primary" || state.serialSwitches != 0 || state.serialFallbacks != 0 {
		t.Fatalf("pinned request polluted serial state: auth=%q switches=%d provisional=%d", state.serialActiveAuthID, state.serialSwitches, state.serialFallbacks)
	}
}

func TestSerialSchedulerConcurrentConfirmedMissingSwitchesOnce(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	if got := state.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	retry := serialTestRequest()
	retry.Candidates = retry.Candidates[:1]
	state.serialPick(retry, now.Add(time.Second))
	state.serialPick(retry, now.Add(2*time.Second))

	const workers = 64
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	var wg sync.WaitGroup
	confirmedAt := now.Add(time.Second + serialCandidateMissingGrace)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- state.serialPick(retry, confirmedAt)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for got := range results {
		if !got.Handled || got.AuthID != "backup" {
			t.Fatalf("concurrent confirmed pick = %#v", got)
		}
	}
	if state.serialActiveAuthID != "backup" || state.serialSwitches != 1 {
		t.Fatalf("concurrent confirmation auth=%q switches=%d", state.serialActiveAuthID, state.serialSwitches)
	}
}

func TestSerialSchedulerHalfOpenCollisionDoesNotSwitchCommittedAuth(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.serialActiveAuthID = "primary"
	state.serialSelectedAt = now
	banStore.set("primary", banEntry{
		ResetAt: now.Add(-time.Second), Window: "probation", Kind: banKindProbation, Phase: banPhaseCooldown,
	})
	req := serialTestRequest()

	const workers = 32
	start := make(chan struct{})
	results := make(chan pluginapi.SchedulerPickResponse, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := state.schedulerPick(req)
			results <- got
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	primaryPicks := 0
	for got := range results {
		if !got.Handled || (got.AuthID != "primary" && got.AuthID != "backup") {
			t.Fatalf("half-open collision pick = %#v", got)
		}
		if got.AuthID == "primary" {
			primaryPicks++
		}
	}
	if primaryPicks != 1 {
		t.Fatalf("primary half-open probes=%d; want exactly 1", primaryPicks)
	}
	if state.serialActiveAuthID != "primary" || state.serialSwitches != 0 {
		t.Fatalf("half-open collision changed committed auth=%q switches=%d", state.serialActiveAuthID, state.serialSwitches)
	}
	if stats := banStore.stats(); stats.ProbeStarts != 1 {
		t.Fatalf("probe starts=%d; want 1", stats.ProbeStarts)
	}
}

func TestSerialSchedulerKeepsUnknownQuotaOnOneDeterministicAuth(t *testing.T) {
	resetBanStoreForTest()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg, quotas: make(map[string]quotaSnapshot)}
	req := pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "b", Provider: providerCodex, Priority: 1},
		{ID: "a", Provider: providerCodex, Priority: 2},
	}}
	for i := 0; i < 10; i++ {
		got, err := state.schedulerPick(req)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Handled || got.AuthID != "a" {
			t.Fatalf("unknown-quota serial pick = %#v", got)
		}
	}
}

func TestSerialSchedulerKeepsCurrentWhenEveryBackupReachedSoftThreshold(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	if got, _ := state.schedulerPick(req); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	state.mu.Lock()
	for _, authID := range []string{"primary", "backup"} {
		snapshot := state.quotas[authID]
		snapshot.Windows[0].UsedPercent = 98
		state.quotas[authID] = snapshot
	}
	state.mu.Unlock()
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("soft-threshold pool escaped the current auth: %#v", got)
	}
}

func TestSerialStatePersistenceIncludesActiveAuth(t *testing.T) {
	resetBanStoreForTest()
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	cfg.StatePath = path
	state := schedulerRuntimeState{
		cfg:                    cfg,
		warmups:                make(map[string]warmupEntry),
		serialActiveAuthID:     "primary",
		serialSelectedAt:       now,
		serialSwitches:         3,
		serialFallbacks:        7,
		serialLastSwitchAt:     now,
		serialLastSwitchReason: "serial_threshold",
	}
	state.initializeGenerationOwnership(path)
	if err := state.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &state)
	state.persistBanState()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != 5 || persisted.SerialActiveAuthID != "primary" || persisted.SerialSwitches != 3 || persisted.SerialFallbacks != 7 {
		t.Fatalf("serial persistence = %#v", persisted)
	}
}

func TestSerialWarmupActivatesFullBackupWithoutReplacingActiveTrafficAuth(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg:                cfg,
		serialActiveAuthID: "primary",
		quotas: map[string]quotaSnapshot{
			"primary": {
				AuthID: "primary", AuthIndex: "idx-primary", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "5h", UsedPercent: 1, Allowed: true, ResetAt: now.Add(time.Hour), ObservedAt: now}},
			},
			"backup": {
				AuthID: "backup", AuthIndex: "idx-backup", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now}},
			},
		},
	}
	candidate, ok := state.findWarmupCandidate(map[string]warmupAuthBinding{
		"primary":     {AuthID: "primary", AuthIndex: "idx-primary"},
		"idx-primary": {AuthID: "primary", AuthIndex: "idx-primary"},
		"backup":      {AuthID: "backup", AuthIndex: "idx-backup"},
		"idx-backup":  {AuthID: "backup", AuthIndex: "idx-backup"},
	}, now)
	if !ok || candidate.Snapshot.AuthID != "backup" {
		t.Fatalf("warmup candidate = %#v, ok=%v; want full backup", candidate, ok)
	}
	if state.serialActiveAuthID != "primary" {
		t.Fatalf("serial active auth changed to %q during warmup selection", state.serialActiveAuthID)
	}
}

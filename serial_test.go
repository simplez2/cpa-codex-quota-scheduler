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

func TestSerialSchedulerDrainAllowsActivePastThresholdNearReset(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	if got, _ := state.schedulerPick(req); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}

	state.mu.Lock()
	primary := state.quotas["primary"]
	// 99% used, reset in 4h: inside the default 6h drain window, so crossing
	// the 98% soft threshold must not switch away.
	primary.Windows[0].UsedPercent = 99
	primary.Windows[0].ResetAt = now.Add(4 * time.Hour)
	state.quotas["primary"] = primary
	state.mu.Unlock()
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("drain mode did not keep active auth: %#v", got)
	}
}

func TestSerialSchedulerDrainPrefersExpiringAccountOverFreshBackup(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	backup := state.quotas["backup"]
	backup.Windows[0].UsedPercent = 5
	backup.Windows[0].ResetAt = now.Add(6 * 24 * time.Hour)
	state.quotas["backup"] = backup
	primary := state.quotas["primary"]
	primary.Windows[0].UsedPercent = 90
	primary.Windows[0].ResetAt = now.Add(3 * time.Hour)
	state.quotas["primary"] = primary
	state.mu.Unlock()

	got, err := state.schedulerPick(serialTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("drain account should be preferred over fresh backup: %#v", got)
	}
}

func TestSerialSchedulerDrainStopsAtFullWindow(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	primary := state.quotas["primary"]
	primary.Windows[0].UsedPercent = 100
	primary.Windows[0].ResetAt = now.Add(2 * time.Hour)
	state.quotas["primary"] = primary
	state.mu.Unlock()

	got, err := state.schedulerPick(serialTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("below drain floor should switch to backup: %#v", got)
	}
}

func TestSerialOverdraftPinsSessionToExhaustedAuth(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"in-flight"}}
	if got, _ := state.schedulerPick(req); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}

	state.mu.Lock()
	primary := state.quotas["primary"]
	// Outside the drain window (reset in 2 days) so the only reason to stay is
	// the in-flight session overdraft pin.
	primary.Windows[0].UsedPercent = 99
	state.quotas["primary"] = primary
	state.mu.Unlock()

	// The in-flight session keeps using the exhausted auth.
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("overdraft session was moved off exhausted auth: %#v", got)
	}

	// A different session routes to the fresh backup.
	other := serialTestRequest()
	other.Options.Headers = map[string][]string{"X-Session-ID": {"new-session"}}
	fresh, err := state.schedulerPick(other)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.Handled || fresh.AuthID != "backup" {
		t.Fatalf("new session did not move to backup: %#v", fresh)
	}

	// The in-flight session stays pinned on subsequent requests.
	again, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Handled || again.AuthID != "primary" {
		t.Fatalf("overdraft pin did not persist: %#v", again)
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

func TestSerialSchedulerHardFiveHourLimitFallsBackToSoftThresholdBackup(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.quotas["primary"] = quotaSnapshot{
		AuthID: "primary", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
			{Class: "weekly", UsedPercent: 40, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
		},
	}
	state.quotas["backup"] = quotaSnapshot{
		AuthID: "backup", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 98, Allowed: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
			{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
		},
	}
	state.mu.Unlock()
	got := state.serialPick(serialTestRequest(), now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("hard exhausted primary did not switch to soft-threshold backup: %#v", got)
	}
	if state.serialLastSwitchReason != "limit_reached" {
		t.Fatalf("switch reason = %q; want limit_reached", state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerPrefersLeastUsedFiveHourBackupAfterInitialSelection(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialLastSelected = map[string]time.Time{
		"primary": now.Add(-2 * time.Hour),
		"backup":  now.Add(-30 * time.Minute),
		"third":   now.Add(-10 * time.Minute),
	}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 98, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["third"] = quotaSnapshot{AuthID: "third", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 40, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()
	got := state.serialPick(pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "primary", Provider: providerCodex},
			{ID: "backup", Provider: providerCodex},
			{ID: "third", Provider: providerCodex},
		},
	}, now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("least-used 5h backup was not selected: %#v", got)
	}
}

func TestSerialSchedulerWeeklyReservePreemptsProtectedCurrent(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialLastSelected = map[string]time.Time{"primary": now.Add(-time.Hour)}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 95, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()
	got := state.serialPick(pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "primary", Provider: providerCodex},
			{ID: "backup", Provider: providerCodex},
		},
	}, now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("weekly-protected current was not preempted: %#v", got)
	}
}

func TestSerialSchedulerRotatesOnceWhenFiveHourCycleAdvances(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	oldReset := now.Add(-time.Hour)
	newReset := now.Add(4 * time.Hour)
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"
	state.serialLastSelected = map[string]time.Time{
		"primary": now.Add(-5 * time.Hour),
		"backup":  now.Add(-10 * time.Hour),
	}
	state.serialFiveHourCycle = map[string]time.Time{"primary": oldReset}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: newReset, ObservedAt: now},
		{Class: "weekly", UsedPercent: 45, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: newReset, ObservedAt: now},
		{Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()

	got := state.serialPick(serialTestRequest(), now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("new 5h cycle did not rotate to balanced backup: %#v", got)
	}
	if state.serialLastSwitchReason != "five_hour_cycle_rotation" {
		t.Fatalf("switch reason = %q; want five_hour_cycle_rotation", state.serialLastSwitchReason)
	}
	if !state.serialFiveHourCycle["backup"].Equal(newReset) {
		t.Fatalf("selected cycle anchor = %v; want %v", state.serialFiveHourCycle["backup"], newReset)
	}

	again := state.serialPick(serialTestRequest(), now.Add(time.Minute))
	if !again.Handled || again.AuthID != "backup" {
		t.Fatalf("cycle rotation repeated inside the same cycle: %#v", again)
	}
}

func TestSerialSchedulerDoesNotTreatFutureFiveHourResetDriftAsNewCycle(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"
	state.serialLastSelected = map[string]time.Time{
		"primary": now.Add(-time.Hour),
		"backup":  now.Add(-2 * time.Hour),
	}
	state.serialFiveHourCycle = map[string]time.Time{"primary": now.Add(3 * time.Hour)}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()

	got := state.serialPick(serialTestRequest(), now)
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("future reset-anchor drift rotated the active auth: %#v", got)
	}
}

func TestSerialSchedulerDoesNotCreateOverdraftForNewHardLimitedSessionWithoutBackup(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 40, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()
	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "primary", Provider: providerCodex},
		},
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{"X-Session-ID": {"new-hard-limited-session"}}},
	}

	first := state.serialPick(req, now)
	if first.Handled {
		t.Fatalf("new session was routed to a hard-limited auth: %#v", first)
	}
	second := state.serialPick(req, now.Add(time.Second))
	if second.Handled {
		t.Fatalf("new session acquired an overdraft binding after hard limit: %#v", second)
	}
	if len(state.serialOverdraft) != 0 {
		t.Fatalf("unexpected overdraft bindings: %#v", state.serialOverdraft)
	}
}

func TestSerialSchedulerClearsOverdraftWhenFiveHourLimitIsReached(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"session-hard-limit"}}
	session := schedulerSessionHash(req)
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialOverdraft = map[string]serialOverdraftBinding{
		session: {AuthID: "primary", LastUsedAt: now},
	}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 40, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()

	got := state.serialPick(req, now.Add(time.Second))
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("hard-limited session was not handed off: %#v", got)
	}
	if binding, ok := state.serialOverdraft[session]; !ok || binding.AuthID != "backup" {
		t.Fatalf("hard-limited overdraft binding was not replaced by backup: %#v", state.serialOverdraft)
	}
}

func TestSerialSchedulerManualSelectionDoesNotRotateOnFiveHourCycleAdvance(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "manual"
	state.serialFiveHourCycle = map[string]time.Time{"primary": now.Add(time.Hour)}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 40, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()

	got := state.serialPick(serialTestRequest(), now)
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("manual primary was rotated automatically: %#v", got)
	}
}

func TestSerialSchedulerFiveHourDrainDoesNotCoverWholeCycle(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	window := quotaWindow{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), Allowed: true, ResetAt: now.Add(4 * time.Hour)}
	if serialWindowDrains(window, cfg, now) {
		t.Fatal("5h window entered drain mode four hours before reset")
	}
	window.ResetAt = now.Add(20 * time.Minute)
	if !serialWindowDrains(window, cfg, now) {
		t.Fatal("5h window did not enter drain mode during its final 30 minutes")
	}
}

func TestInspectSerialCandidateHardLimitOverridesEarlierSoftThreshold(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	choice := inspectSerialCandidate(
		pluginapi.SchedulerAuthCandidate{ID: "acct", Provider: providerCodex},
		quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{
			{Class: "weekly", UsedPercent: 98, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
			{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
		}},
		true,
		cfg,
		now,
	)
	if choice.Eligible || choice.Reason != "limit_reached" {
		t.Fatalf("hard limit was masked by earlier soft threshold: %#v", choice)
	}
}

func TestInspectSerialCandidateReserveAwareHandoffIsUserSelectable(t *testing.T) {
	now := time.Now()
	candidate := pluginapi.SchedulerAuthCandidate{ID: "acct", Provider: providerCodex}
	thresholdOnly := defaultPluginConfig()
	thresholdOnly.SerialSwitchPercent = 98
	thresholdOnly.SerialHandoffMode = "threshold_only"
	reserveAware := thresholdOnly
	reserveAware.SerialHandoffMode = "reserve_aware"
	reserveAware.Reserve5hPercent = 15
	reserveAware.ReserveWeeklyPercent = 8
	reserveAware.ReserveMonthlyPercent = 12

	for _, test := range []struct {
		class string
		used  float64
		reset time.Duration
	}{
		{class: "5h", used: 86, reset: 2 * time.Hour},
		{class: "weekly", used: 93, reset: 4 * 24 * time.Hour},
		{class: "monthly", used: 89, reset: 20 * 24 * time.Hour},
	} {
		snapshot := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{
			{Class: test.class, UsedPercent: test.used, Allowed: true, ResetAt: now.Add(test.reset), ObservedAt: now},
		}}
		choice := inspectSerialCandidate(candidate, snapshot, true, thresholdOnly, now)
		if !choice.Eligible || choice.Reason != "eligible" {
			t.Fatalf("threshold-only mode unexpectedly reserved %s capacity: %#v", test.class, choice)
		}
		choice = inspectSerialCandidate(candidate, snapshot, true, reserveAware, now)
		if choice.Eligible || choice.Reason != "serial_threshold" {
			t.Fatalf("reserve-aware mode did not hand off before consuming the %s reserve: %#v", test.class, choice)
		}
	}
}

func TestInspectSerialCandidate5hHandoffModesAreIndependentFromGlobalPolicy(t *testing.T) {
	now := time.Now()
	candidate := pluginapi.SchedulerAuthCandidate{ID: "acct", Provider: providerCodex}
	snapshot := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 91, Allowed: true, ResetAt: now.Add(2 * time.Hour), ObservedAt: now},
		{Class: "weekly", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}

	custom := defaultPluginConfig()
	custom.SerialSwitchPercent = 98
	custom.SerialHandoffMode = "threshold_only"
	custom.Serial5hHandoffMode = "custom_threshold"
	custom.Serial5hSwitchPercent = 90
	choice := inspectSerialCandidate(candidate, snapshot, true, custom, now)
	if choice.Eligible || choice.Reason != "serial_threshold" {
		t.Fatalf("custom 5h threshold did not trigger: %#v", choice)
	}

	inherit := custom
	inherit.Serial5hHandoffMode = "inherit_global"
	choice = inspectSerialCandidate(candidate, snapshot, true, inherit, now)
	if !choice.Eligible || choice.Reason != "eligible" {
		t.Fatalf("inherit_global did not preserve 98%% behavior: %#v", choice)
	}

	reserve := custom
	reserve.Serial5hHandoffMode = "reserve_aware"
	reserve.Serial5hSwitchPercent = 90
	reserve.Reserve5hPercent = 15
	choice = inspectSerialCandidate(candidate, snapshot, true, reserve, now)
	if choice.Eligible || choice.Reason != "serial_threshold" {
		t.Fatalf("5h reserve-aware mode did not trigger: %#v", choice)
	}

	hardOnly := custom
	hardOnly.Serial5hHandoffMode = "429_only"
	choice = inspectSerialCandidate(candidate, snapshot, true, hardOnly, now)
	if !choice.Eligible || choice.Reason != "eligible" {
		t.Fatalf("429_only incorrectly applied a soft 5h threshold: %#v", choice)
	}
}

func TestSortSerialCandidatesKeepsWeeklyReserveProtected(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	choices := []serialCandidate{
		{
			Candidate:       pluginapi.SchedulerAuthCandidate{ID: "protected", Priority: 100},
			QuotaKnown:      true,
			WeeklyKnown:     true,
			WeeklyRemaining: 5,
			WeeklyProtected: true,
			DrainActive:     true,
			ResetCredits:    1,
			LastSelectedAt:  now.Add(-2 * time.Hour),
		},
		{
			Candidate:       pluginapi.SchedulerAuthCandidate{ID: "safe", Priority: 1},
			QuotaKnown:      true,
			WeeklyKnown:     true,
			WeeklyRemaining: 40,
			LastSelectedAt:  now.Add(-time.Hour),
		},
	}
	sortSerialCandidates(choices, cfg)
	if choices[0].Candidate.ID != "safe" {
		t.Fatalf("weekly-protected account outranked safe capacity: %#v", choices)
	}
}

func TestSortSerialCandidatesHysteresisIsInputOrderIndependent(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	base := []serialCandidate{
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "a"}, WeeklyKnown: true, WeeklyRemaining: 50, FiveHourKnown: true, FiveHourUsed: 40, LastSelectedAt: now.Add(-time.Hour)},
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "b"}, WeeklyKnown: true, WeeklyRemaining: 49, FiveHourKnown: true, FiveHourUsed: 10, LastSelectedAt: now.Add(-2 * time.Hour)},
		{Candidate: pluginapi.SchedulerAuthCandidate{ID: "c"}, WeeklyKnown: true, WeeklyRemaining: 47, FiveHourKnown: true, FiveHourUsed: 0, LastSelectedAt: now.Add(-3 * time.Hour)},
	}
	orders := [][]int{{0, 1, 2}, {2, 0, 1}, {1, 2, 0}}
	for _, order := range orders {
		choices := []serialCandidate{base[order[0]], base[order[1]], base[order[2]]}
		sortSerialCandidates(choices, cfg)
		got := []string{choices[0].Candidate.ID, choices[1].Candidate.ID, choices[2].Candidate.ID}
		want := []string{"b", "a", "c"}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("input order %v produced %v; want %v", order, got, want)
			}
		}
	}
}

func TestSerialSchedulerConsumesBlockedFiveHourCycleRotation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	newReset := now.Add(4 * time.Hour)
	state := newSerialTestState(now)
	state.mu.Lock()
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"
	state.serialFiveHourCycle = map[string]time.Time{"primary": now.Add(-time.Hour)}
	state.quotas["primary"] = quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: newReset, ObservedAt: now},
		{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.quotas["backup"] = quotaSnapshot{AuthID: "backup", RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: newReset, ObservedAt: now},
		{Class: "weekly", UsedPercent: 95, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
	}}
	state.mu.Unlock()

	first := state.serialPick(serialTestRequest(), now)
	if !first.Handled || first.AuthID != "primary" {
		t.Fatalf("protected backup received blocked cycle rotation: %#v", first)
	}
	if !state.serialFiveHourCycle["primary"].Equal(newReset) {
		t.Fatalf("blocked cycle transition was not consumed: %v", state.serialFiveHourCycle["primary"])
	}

	state.mu.Lock()
	backup := state.quotas["backup"]
	backup.Windows[1].UsedPercent = 20
	state.quotas["backup"] = backup
	state.mu.Unlock()
	second := state.serialPick(serialTestRequest(), now.Add(time.Minute))
	if !second.Handled || second.AuthID != "primary" {
		t.Fatalf("consumed cycle rotated late after protection changed: %#v", second)
	}
}

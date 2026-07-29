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
		serialLastSwitchAt:     now,
		serialLastSwitchReason: "serial_threshold",
	}
	state.persistBanState()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != 4 || persisted.SerialActiveAuthID != "primary" || persisted.SerialSwitches != 3 {
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
	candidate, ok := state.findWarmupCandidate(map[string]bool{
		"primary": true, "idx-primary": true, "backup": true, "idx-backup": true,
	}, now)
	if !ok || candidate.Snapshot.AuthID != "backup" {
		t.Fatalf("warmup candidate = %#v, ok=%v; want full backup", candidate, ok)
	}
	if state.serialActiveAuthID != "primary" {
		t.Fatalf("serial active auth changed to %q during warmup selection", state.serialActiveAuthID)
	}
}

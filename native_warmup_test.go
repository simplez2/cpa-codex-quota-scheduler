package main

import (
	"errors"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func warmupSchedulerRequest(nonce string, authIDs ...string) pluginapi.SchedulerPickRequest {
	candidates := make([]pluginapi.SchedulerAuthCandidate, 0, len(authIDs))
	for _, authID := range authIDs {
		candidates = append(candidates, pluginapi.SchedulerAuthCandidate{ID: authID, Provider: providerCodex})
	}
	return pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Options: pluginapi.SchedulerOptions{Headers: map[string][]string{
			warmupRequestHeader: []string{nonce},
		}},
		Candidates: candidates,
	}
}

func TestWarmupNonceSelectsExactAuthOnce(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := schedulerRuntimeState{
		cfg:          defaultPluginConfig(),
		warmupLeases: make(map[string]warmupLease),
	}
	nonce, err := state.registerWarmupLease("target", now)
	if err != nil {
		t.Fatal(err)
	}

	request := warmupSchedulerRequest(nonce, "other", "target")
	response, err := state.schedulerPick(request)
	if err != nil || !response.Handled || response.AuthID != "target" {
		t.Fatalf("first warmup pick = %#v, err=%v; want exact target", response, err)
	}

	response, err = state.schedulerPick(request)
	if err == nil || response.Handled || response.AuthID != "" {
		t.Fatalf("reused nonce pick = %#v, err=%v; nonce must be single-use", response, err)
	}
}

func TestRecoveryWarmupNonceSelectsOnlyOwnedHalfOpenAuth(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	banStore.set("target", banEntry{
		ResetAt:  now.Add(-time.Minute),
		BannedAt: now.Add(-2 * time.Hour),
		Window:   "5h",
		Kind:     banKindQuota,
		Phase:    banPhaseCooldown,
	})
	allowed, started := banStore.tryStartProbe("target", now, 10*time.Minute)
	if !allowed || !started {
		t.Fatal("failed to reserve the recovery half-open probe")
	}

	state := schedulerRuntimeState{
		cfg:          defaultPluginConfig(),
		warmupLeases: make(map[string]warmupLease),
	}
	entry, ok := banStore.lookup("target")
	if !ok {
		t.Fatal("half-open recovery entry disappeared")
	}
	nonce, err := state.registerRecoveryWarmupLease("target", entry.BannedAt, entry.ProbeStartedAt, now)
	if err != nil {
		t.Fatal(err)
	}
	response, err := state.schedulerPick(warmupSchedulerRequest(nonce, "other", "target"))
	if err != nil || !response.Handled || response.AuthID != "target" {
		t.Fatalf("recovery warmup pick = %#v, err=%v; want the owned half-open auth", response, err)
	}

	normalNonce, err := state.registerWarmupLease("target", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response, err = state.schedulerPick(warmupSchedulerRequest(normalNonce, "target"))
	if err == nil || response.Handled {
		t.Fatalf("ordinary warmup bypassed half-open quarantine: response=%#v err=%v", response, err)
	}
}

func TestRecoveryWarmupNonceRejectsDifferentHalfOpenGeneration(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	banStore.set("target", banEntry{
		ResetAt:  now.Add(-time.Minute),
		BannedAt: now.Add(-2 * time.Hour),
		Window:   "5h",
		Kind:     banKindQuota,
		Phase:    banPhaseCooldown,
	})
	allowed, started := banStore.tryStartProbe("target", now, 10*time.Minute)
	if !allowed || !started {
		t.Fatal("failed to reserve the current half-open probe")
	}

	state := schedulerRuntimeState{
		cfg:          defaultPluginConfig(),
		warmupLeases: make(map[string]warmupLease),
	}
	entry, ok := banStore.lookup("target")
	if !ok {
		t.Fatal("half-open recovery entry disappeared")
	}
	staleNonce, err := state.registerRecoveryWarmupLease("target", entry.BannedAt, now.Add(500*time.Millisecond), now)
	if err != nil {
		t.Fatal(err)
	}
	response, err := state.schedulerPick(warmupSchedulerRequest(staleNonce, "target"))
	if err == nil || response.Handled {
		t.Fatalf("mismatched recovery generation selected quarantined auth: response=%#v err=%v", response, err)
	}
	if _, err := state.registerRecoveryWarmupLease("target", time.Time{}, time.Time{}, now); err == nil {
		t.Fatal("recovery lease accepted an incomplete identity")
	}
}

func TestRecoveryWarmupNonceRequiresLiveHalfOpenBan(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	banStore.set("target", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: now.Add(-2 * time.Hour),
		Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
	})
	allowed, started := banStore.tryStartProbe("target", now, 10*time.Minute)
	if !allowed || !started {
		t.Fatal("failed to reserve the recovery half-open probe")
	}
	entry, ok := banStore.lookup("target")
	if !ok {
		t.Fatal("half-open recovery entry disappeared")
	}
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), warmupLeases: make(map[string]warmupLease)}
	nonce, err := state.registerRecoveryWarmupLease("target", entry.BannedAt, entry.ProbeStartedAt, now)
	if err != nil {
		t.Fatal(err)
	}
	banStore.clear("target")
	response, err := state.schedulerPick(warmupSchedulerRequest(nonce, "target"))
	if err == nil || response.Handled {
		t.Fatalf("stale recovery nonce selected an already-cleared auth: response=%#v err=%v", response, err)
	}
}

func TestWarmupFakeNonceIsRejectedWithoutFallback(t *testing.T) {
	resetBanStoreForTest()
	state := schedulerRuntimeState{
		cfg:          defaultPluginConfig(),
		warmupLeases: make(map[string]warmupLease),
	}
	response, err := state.schedulerPick(warmupSchedulerRequest("not-a-real-lease", "fallback"))
	if err == nil || response.Handled || response.AuthID != "" {
		t.Fatalf("fake nonce pick = %#v, err=%v; want hard rejection without fallback", response, err)
	}
}

func TestWarmupMissingTargetIsRejectedWithoutFallbackAndConsumesNonce(t *testing.T) {
	resetBanStoreForTest()
	state := schedulerRuntimeState{
		cfg:          defaultPluginConfig(),
		warmupLeases: make(map[string]warmupLease),
	}
	nonce, err := state.registerWarmupLease("target", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	response, err := state.schedulerPick(warmupSchedulerRequest(nonce, "fallback"))
	if err == nil || response.Handled || response.AuthID != "" {
		t.Fatalf("missing target pick = %#v, err=%v; want hard rejection without fallback", response, err)
	}

	response, err = state.schedulerPick(warmupSchedulerRequest(nonce, "target"))
	if err == nil || response.Handled || response.AuthID != "" {
		t.Fatalf("second pick after missing target = %#v, err=%v; consumed nonce must not revive", response, err)
	}
}

func TestParseWarmupJSONAcceptsOnlyCompletedStatus(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantEvent      string
		wantCode       string
		wantErr        bool
		wantIncomplete bool
	}{
		{name: "completed", body: `{"id":"resp_1","status":"completed","output":[]}`, wantEvent: "response.completed"},
		{name: "failed", body: `{"id":"resp_1","status":"failed","error":{"code":"auth_unavailable","message":"must not be persisted"}}`, wantEvent: "response.failed", wantCode: "auth_unavailable", wantErr: true},
		{name: "failed by error type", body: `{"id":"resp_1","status":"failed","error":{"type":"cyber_policy","message":"must not be persisted"}}`, wantEvent: "response.failed", wantCode: "cyber_policy", wantErr: true},
		{name: "incomplete", body: `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, wantEvent: "response.incomplete", wantErr: true},
		{name: "missing terminal status", body: `{"id":"resp_1","output":[]}`, wantErr: true, wantIncomplete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := parseWarmupResponse([]byte(test.body))
			if (err != nil) != test.wantErr {
				t.Fatalf("out=%#v err=%v; wantErr=%v", out, err, test.wantErr)
			}
			if out.TerminalEvent != test.wantEvent || out.ErrorCode != test.wantCode {
				t.Fatalf("out=%#v; want event=%q code=%q", out, test.wantEvent, test.wantCode)
			}
			if test.wantIncomplete && !errors.Is(err, errWarmupStreamIncomplete) {
				t.Fatalf("err=%v; want errWarmupStreamIncomplete", err)
			}
		})
	}
}

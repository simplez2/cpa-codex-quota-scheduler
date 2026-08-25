package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func newManualSelectionRuntime(t *testing.T, files []cpaAuthFileEntry, snapshots map[string]quotaSnapshot) *schedulerRuntimeState {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" || r.Header.Get("Authorization") != "Bearer test-management-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	}))
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-management-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	state.cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	state.cfg.CPAManagementKeyFile = keyPath
	state.cfg.SchedulerMode = "serial"
	state.cfg.StaleAfter = 15 * time.Minute
	state.quotas = snapshots
	claimManagedRuntimeForTest(t, state)
	t.Cleanup(state.stop)
	return state
}

func freshManualQuota(authID, authIndex string, used float64, now time.Time) quotaSnapshot {
	return quotaSnapshot{AuthID: authID, AuthIndex: authIndex, RefreshedAt: now, Windows: []quotaWindow{{
		Class: "weekly", UsedPercent: used, Allowed: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now,
	}}}
}

func TestManagementRegistrationExposesSerialActiveRoutes(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range managementRegistration().Routes {
		if route.Path == managementRoutePrefix+"/serial-active" {
			seen[route.Method] = true
		}
	}
	if !seen[http.MethodPut] || !seen[http.MethodDelete] || len(seen) != 2 {
		t.Fatalf("serial-active routes = %#v", seen)
	}
}

func TestDecodeManagementSerialActiveRequestIsStrict(t *testing.T) {
	valid, err := decodeManagementSerialActiveRequest([]byte(`{"auth_id":" account "}`))
	if err != nil || valid.AuthID != "account" {
		t.Fatalf("valid=%#v err=%v", valid, err)
	}
	for name, body := range map[string][]byte{
		"empty": nil, "invalid": []byte(`{`), "missing": []byte(`{}`),
		"unknown": []byte(`{"auth_id":"account","force":true}`), "blank": []byte(`{"auth_id":" "}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeManagementSerialActiveRequest(body); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestManualSerialSelectionSupportsOAuthAndPATAndPersists(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC().Truncate(time.Second)
	files := []cpaAuthFileEntry{
		{ID: "oauth-auth", AuthIndex: "oauth-index", Name: "oauth.json", Provider: providerCodex, Status: "active"},
		{ID: "pat-auth", AuthIndex: "pat-index", Name: "pat.json", Provider: providerCodex, Status: "active", Note: "codex access token via sidecar"},
	}
	state := newManualSelectionRuntime(t, files, map[string]quotaSnapshot{
		"oauth-index": freshManualQuota("oauth-auth", "oauth-index", 5, now),
		"pat-index":   freshManualQuota("pat-auth", "pat-index", 10, now),
	})
	if _, err := state.setManualSerialActive("oauth-index", now); err != nil {
		t.Fatal(err)
	}
	status := state.status()
	if status.SerialActiveAuthID != "oauth-auth" || status.SerialSelectionSource != "manual" || !status.SerialManualSelection || status.SerialManualActiveAuthID != "oauth-auth" {
		t.Fatalf("OAuth status=%#v", status)
	}
	if _, err := state.setManualSerialActive("pat.json", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if state.serialSwitches != 2 {
		t.Fatalf("manual account transitions = %d; want 2", state.serialSwitches)
	}
	raw, err := os.ReadFile(state.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SerialActiveAuthID != "pat-auth" || persisted.SerialSelectionSource != "manual" {
		t.Fatalf("persisted=%#v", persisted)
	}
	newOwner := newManagedRuntimeForTest(t, state.cfg.StatePath)
	claimManagedRuntimeForTest(t, newOwner)
	t.Cleanup(newOwner.stop)
	if newOwner.serialActiveAuthID != "pat-auth" || normalizeSerialSelectionSource(newOwner.serialSelectionSource) != "manual" {
		t.Fatalf("hot reload lost manual state auth=%q source=%q", newOwner.serialActiveAuthID, newOwner.serialSelectionSource)
	}
}

func TestManualSerialSelectionRepeatedPutIsNoOp(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC().Truncate(time.Second)
	state := newManualSelectionRuntime(t, []cpaAuthFileEntry{{ID: "active", AuthIndex: "active-index", Provider: providerCodex, Status: "active"}}, map[string]quotaSnapshot{
		"active": freshManualQuota("active", "active-index", 5, now),
	})
	if result, err := state.setManualSerialActive("active", now); err != nil || !result.changed {
		t.Fatalf("initial manual selection result=%#v err=%v", result, err)
	}
	selectedAt := state.serialSelectedAt
	lastSwitchAt := state.serialLastSwitchAt
	lastSwitchReason := state.serialLastSwitchReason
	switches := state.serialSwitches
	state.serialMissingAuthID = "missing"
	state.serialFallbackAuthID = "fallback"
	state.serialMissingSince = now.Add(-time.Minute)
	state.serialMissingCount = 2

	state.cfg.CPAManagementURL = "http://127.0.0.1:1/true-no-op-must-not-read-inventory"
	if result, err := state.setManualSerialActive("active", now.Add(time.Minute)); err != nil || result.changed {
		t.Fatalf("repeated manual selection result=%#v err=%v", result, err)
	}
	if !state.serialSelectedAt.Equal(selectedAt) || !state.serialLastSwitchAt.Equal(lastSwitchAt) || state.serialLastSwitchReason != lastSwitchReason || state.serialSwitches != switches {
		t.Fatalf("no-op changed switch metadata: selected=%v last=%v reason=%q switches=%d", state.serialSelectedAt, state.serialLastSwitchAt, state.serialLastSwitchReason, state.serialSwitches)
	}
	if state.serialMissingAuthID != "missing" || state.serialFallbackAuthID != "fallback" || !state.serialMissingSince.Equal(now.Add(-time.Minute)) || state.serialMissingCount != 2 {
		t.Fatalf("no-op changed missing state: auth=%q fallback=%q since=%v count=%d", state.serialMissingAuthID, state.serialFallbackAuthID, state.serialMissingSince, state.serialMissingCount)
	}
}

func TestManualSerialSelectionRejectsUnsafeCandidates(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC()
	files := []cpaAuthFileEntry{
		{ID: "active", AuthIndex: "active-index", Provider: providerCodex, Status: "active"},
		{ID: "disabled", AuthIndex: "disabled-index", Provider: providerCodex, Status: "active", Disabled: true},
		{ID: "unavailable", AuthIndex: "unavailable-index", Provider: providerCodex, Status: "active", Unavailable: true},
	}
	state := newManualSelectionRuntime(t, files, map[string]quotaSnapshot{"active": freshManualQuota("active", "active-index", 99, now)})
	for _, authID := range []string{"missing", "disabled", "unavailable", "active"} {
		if _, err := state.setManualSerialActive(authID, now); err == nil {
			t.Fatalf("unsafe auth %q was accepted", authID)
		}
	}
	state.quotas["active"] = freshManualQuota("active", "active-index", 5, now.Add(-time.Hour))
	if _, err := state.setManualSerialActive("active", now); err == nil {
		t.Fatal("stale auth was accepted")
	}
	state.quotas["active"] = freshManualQuota("active", "active-index", 5, now)
	banStore.set("active", banEntry{ResetAt: now.Add(time.Hour), Window: "weekly", Kind: banKindQuota, Phase: banPhaseCooldown})
	if _, err := state.setManualSerialActive("active", now); err == nil {
		t.Fatal("quarantined auth was accepted")
	}
}

func TestManualSelectionSafelyFallsBackOnHardFailure(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC()
	state := &schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotas: map[string]quotaSnapshot{
			"manual": freshManualQuota("manual", "manual-index", 100, now),
			"backup": freshManualQuota("backup", "backup-index", 10, now),
		},
		serialActiveAuthID: "manual", serialSelectionSource: "manual", serialSelectedAt: now.Add(-time.Hour),
		serialOverdraft: make(map[string]serialOverdraftBinding),
	}
	response := state.serialPick(pluginapi.SchedulerPickRequest{Provider: providerCodex, Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "manual", Provider: providerCodex}, {ID: "backup", Provider: providerCodex},
	}}, now)
	if !response.Handled || response.AuthID != "backup" || state.serialSelectionSource != "auto" {
		t.Fatalf("hard-limit failover=%#v source=%q", response, state.serialSelectionSource)
	}
	state.serialActiveAuthID, state.serialSelectionSource = "backup", "manual"
	if !state.markSerialUnavailable("backup", "429", now.Add(time.Second)) || state.serialActiveAuthID != "" || state.serialSelectionSource != "auto" {
		t.Fatalf("429 failover auth=%q source=%q", state.serialActiveAuthID, state.serialSelectionSource)
	}
}

func TestClearManualSerialSelectionPersistsAutoMode(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC()
	state := newManualSelectionRuntime(t, []cpaAuthFileEntry{{ID: "active", AuthIndex: "active-index", Provider: providerCodex, Status: "active"}}, map[string]quotaSnapshot{
		"active": freshManualQuota("active", "active-index", 5, now),
	})
	if _, err := state.setManualSerialActive("active", now); err != nil {
		t.Fatal(err)
	}
	if result, err := state.clearManualSerialActive(now.Add(time.Second)); err != nil || !result.changed {
		t.Fatalf("clear result=%#v err=%v", result, err)
	}
	if state.serialSwitches != 2 {
		t.Fatalf("manual select plus clear transitions = %d; want 2", state.serialSwitches)
	}
	status := state.status()
	if result, err := state.clearManualSerialActive(now.Add(2 * time.Second)); err != nil || result.changed {
		t.Fatalf("automatic clear should be a no-op result=%#v err=%v", result, err)
	}
	if status.SerialActiveAuthID != "" || status.SerialSelectionSource != "auto" || status.SerialManualSelection || status.SerialManualActiveAuthID != "" {
		t.Fatalf("auto status=%#v", status)
	}
}

func TestManualSerialPersistenceFailureRestoresAllRuntimeState(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC().Truncate(time.Second)
	files := []cpaAuthFileEntry{
		{ID: "existing", AuthIndex: "existing-index", Provider: providerCodex, Status: "active"},
		{ID: "replacement", AuthIndex: "replacement-index", Provider: providerCodex, Status: "active"},
	}
	state := newManualSelectionRuntime(t, files, map[string]quotaSnapshot{
		"existing":    freshManualQuota("existing", "existing-index", 5, now),
		"replacement": freshManualQuota("replacement", "replacement-index", 10, now),
	})
	state.serialActiveAuthID = "existing"
	state.serialSelectionSource = "manual"
	state.serialSelectedAt = now.Add(-2 * time.Hour)
	state.serialSwitches = 7
	state.serialLastSwitchAt = now.Add(-time.Hour)
	state.serialLastSwitchReason = "prior"
	state.serialMissingAuthID = "missing"
	state.serialFallbackAuthID = "fallback"
	state.serialMissingSince = now.Add(-time.Minute)
	state.serialMissingCount = 2
	previousReplacementSelected := now.Add(-3 * time.Hour)
	previousReplacementCycle := now.Add(90 * time.Minute)
	state.serialLastSelected = make(map[string]time.Time)
	state.serialFiveHourCycle = make(map[string]time.Time)
	state.serialLastSelected["replacement"] = previousReplacementSelected
	state.serialFiveHourCycle["replacement"] = previousReplacementCycle
	state.cfg.StatePath = t.TempDir()

	if _, err := state.setManualSerialActive("replacement", now); err == nil {
		t.Fatal("manual selection unexpectedly persisted to a directory")
	}
	assertManualRuntimeRestored(t, state, "existing", "manual", now.Add(-2*time.Hour), 7, now.Add(-time.Hour), "prior", "missing", "fallback", now.Add(-time.Minute), 2)
	if !state.serialLastSelected["replacement"].Equal(previousReplacementSelected) || !state.serialFiveHourCycle["replacement"].Equal(previousReplacementCycle) {
		t.Fatalf("selection history was not restored: last=%v cycle=%v", state.serialLastSelected["replacement"], state.serialFiveHourCycle["replacement"])
	}

	if _, err := state.clearManualSerialActive(now); err == nil {
		t.Fatal("manual clear unexpectedly persisted to a directory")
	}
	assertManualRuntimeRestored(t, state, "existing", "manual", now.Add(-2*time.Hour), 7, now.Add(-time.Hour), "prior", "missing", "fallback", now.Add(-time.Minute), 2)
}

func assertManualRuntimeRestored(t *testing.T, state *schedulerRuntimeState, authID, source string, selectedAt time.Time, switches uint64, lastSwitchAt time.Time, reason, missingAuthID, fallbackAuthID string, missingSince time.Time, missingCount int) {
	t.Helper()
	if state.serialActiveAuthID != authID || state.serialSelectionSource != source || !state.serialSelectedAt.Equal(selectedAt) || state.serialSwitches != switches || !state.serialLastSwitchAt.Equal(lastSwitchAt) || state.serialLastSwitchReason != reason {
		t.Fatalf("switch state was not restored: auth=%q source=%q selected=%v switches=%d last=%v reason=%q", state.serialActiveAuthID, state.serialSelectionSource, state.serialSelectedAt, state.serialSwitches, state.serialLastSwitchAt, state.serialLastSwitchReason)
	}
	if state.serialMissingAuthID != missingAuthID || state.serialFallbackAuthID != fallbackAuthID || !state.serialMissingSince.Equal(missingSince) || state.serialMissingCount != missingCount {
		t.Fatalf("missing state was not restored: auth=%q fallback=%q since=%v count=%d", state.serialMissingAuthID, state.serialFallbackAuthID, state.serialMissingSince, state.serialMissingCount)
	}
}

func TestQuotaStatusReturnsFutureWarmupModel(t *testing.T) {
	state := &schedulerRuntimeState{cfg: defaultPluginConfig()}
	state.cfg.WarmupModel = "gpt-future-preview-2027"
	status := state.status()
	if status.WarmupModel != "gpt-future-preview-2027" || status.SerialSelectionSource != "auto" {
		t.Fatalf("status=%#v", status)
	}
}
func TestManualSerialSelectionFailsClosedForSupersededGeneration(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	oldOwner.serialActiveAuthID = "existing"
	oldOwner.serialSelectionSource = "auto"
	newOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, newOwner)
	t.Cleanup(func() {
		newOwner.stop()
		oldOwner.stop()
	})
	_, err := oldOwner.setManualSerialActive("replacement", time.Now())
	typed, ok := err.(*managementSerialActiveError)
	if !ok || typed.Code != "generation_not_active" {
		t.Fatalf("superseded error=%#v", err)
	}
	if oldOwner.serialActiveAuthID != "existing" || oldOwner.serialSelectionSource != "auto" {
		t.Fatalf("superseded generation mutated auth=%q source=%q", oldOwner.serialActiveAuthID, oldOwner.serialSelectionSource)
	}
}

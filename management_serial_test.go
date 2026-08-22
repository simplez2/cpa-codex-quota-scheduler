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
	status := state.status()
	if result, err := state.clearManualSerialActive(now.Add(2 * time.Second)); err != nil || result.changed {
		t.Fatalf("automatic clear should be a no-op result=%#v err=%v", result, err)
	}
	if status.SerialActiveAuthID != "" || status.SerialSelectionSource != "auto" || status.SerialManualSelection || status.SerialManualActiveAuthID != "" {
		t.Fatalf("auto status=%#v", status)
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

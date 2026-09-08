package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func withWarmupManagementState(t *testing.T, entries map[string]warmupEntry) {
	t.Helper()
	fixture := newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	claimManagedRuntimeForTest(t, fixture)
	schedulerRuntime.mu.Lock()
	oldCfg := schedulerRuntime.cfg
	schedulerRuntime.cfg = fixture.cfg
	schedulerRuntime.mu.Unlock()
	schedulerRuntime.generationMu.Lock()
	oldGeneration := schedulerRuntime.generation
	schedulerRuntime.generation = fixture.generation
	schedulerRuntime.generationMu.Unlock()
	schedulerRuntime.warmupMu.Lock()
	oldWarmups := schedulerRuntime.warmups
	schedulerRuntime.warmups = entries
	schedulerRuntime.warmupMu.Unlock()
	t.Cleanup(func() {
		schedulerRuntime.generationMu.Lock()
		schedulerRuntime.generation = oldGeneration
		schedulerRuntime.generationMu.Unlock()
		schedulerRuntime.warmupMu.Lock()
		schedulerRuntime.warmups = oldWarmups
		schedulerRuntime.warmupMu.Unlock()
		schedulerRuntime.mu.Lock()
		schedulerRuntime.cfg = oldCfg
		schedulerRuntime.mu.Unlock()
	})
}

func TestRecoveryRoutesReportPersistenceFailureAfterMemoryChange(t *testing.T) {
	for _, route := range []string{"unban", "unban-all", "warmup-retry"} {
		t.Run(route, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			withWarmupManagementState(t, map[string]warmupEntry{"acct|5h": {AuthID: "acct", Window: "5h", Blocked: true}})
			banStore.set("acct", banEntry{ResetAt: time.Now().Add(time.Hour), Window: "weekly"})
			schedulerRuntime.mu.Lock()
			schedulerRuntime.cfg.StatePath = t.TempDir() // Rename onto a directory must fail on every OS.
			schedulerRuntime.mu.Unlock()
			response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementRoutePrefix + "/" + route, Body: []byte(`{"auth_id":"acct"}`)})
			body := managementResponseJSON(t, response)
			if response.StatusCode != http.StatusServiceUnavailable || body["error"] != "recovery_persistence_failed" || body["memory_changed"] != true {
				t.Fatalf("false durable success: %d %v", response.StatusCode, body)
			}
		})
	}
}

func managementResponseJSON(t *testing.T, response pluginapi.ManagementResponse) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("decode management response: %v body=%s", err, response.Body)
	}
	return payload
}

func TestWarmupRetryManagementRouteClearsOnlyBlockedTarget(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now().UTC()
	banStore.set("first", banEntry{ResetAt: now.Add(time.Hour), Window: "weekly", Kind: banKindQuota, Phase: banPhaseCooldown})
	withWarmupManagementState(t, map[string]warmupEntry{
		warmupKey("first", "weekly"):  {AuthID: "first", Window: "weekly", Blocked: true, Error: "cyber_policy"},
		warmupKey("second", "weekly"): {AuthID: "second", Window: "weekly", Blocked: true, Error: "http_403"},
		warmupKey("retry", "weekly"):  {AuthID: "retry", Window: "weekly", Error: "http_503"},
	})

	response := dispatchManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management" + managementRoutePrefix + "/warmup-retry",
		Body:   []byte(`{"auth_id":"first"}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	payload := managementResponseJSON(t, response)
	if payload["removed"] != float64(1) || payload["auth_id"] != "first" || payload["all"] != false {
		t.Fatalf("unexpected response: %v", payload)
	}
	schedulerRuntime.warmupMu.Lock()
	_, firstExists := schedulerRuntime.warmups[warmupKey("first", "weekly")]
	_, secondExists := schedulerRuntime.warmups[warmupKey("second", "weekly")]
	_, retryExists := schedulerRuntime.warmups[warmupKey("retry", "weekly")]
	schedulerRuntime.warmupMu.Unlock()
	if firstExists || !secondExists || !retryExists {
		t.Fatalf("blocked-only target clear failed: first=%v second=%v retry=%v", firstExists, secondExists, retryExists)
	}
	if _, ok := banStore.lookup("first"); !ok {
		t.Fatal("warmup retry route must not clear quota quarantine")
	}
}

func TestWarmupRetryManagementRouteSupportsExplicitAll(t *testing.T) {
	withWarmupManagementState(t, map[string]warmupEntry{
		warmupKey("first", "5h"):  {AuthID: "first", Window: "5h", Blocked: true, Error: "cyber_policy"},
		warmupKey("second", "5h"): {AuthID: "second", Window: "5h", Blocked: true, Error: "http_403"},
		warmupKey("retry", "5h"):  {AuthID: "retry", Window: "5h", Error: "http_503"},
	})
	response := dispatchManagement(pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementRoutePrefix + "/warmup-retry",
		Body:   []byte(`{"all":true}`),
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	payload := managementResponseJSON(t, response)
	if payload["removed"] != float64(2) || payload["all"] != true {
		t.Fatalf("unexpected response: %v", payload)
	}
	schedulerRuntime.warmupMu.Lock()
	_, retryExists := schedulerRuntime.warmups[warmupKey("retry", "5h")]
	remaining := len(schedulerRuntime.warmups)
	schedulerRuntime.warmupMu.Unlock()
	if !retryExists || remaining != 1 {
		t.Fatalf("all=true removed retryable state: exists=%v remaining=%d", retryExists, remaining)
	}
}

func TestWarmupRetryManagementRouteRejectsImplicitAllAndInvalidJSON(t *testing.T) {
	withWarmupManagementState(t, map[string]warmupEntry{})
	for name, body := range map[string][]byte{
		"missing selector": nil,
		"invalid json":     []byte("{"),
	} {
		t.Run(name, func(t *testing.T) {
			response := dispatchManagement(pluginapi.ManagementRequest{
				Method: http.MethodPost,
				Path:   managementRoutePrefix + "/warmup-retry",
				Body:   body,
			})
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
			}
		})
	}
}

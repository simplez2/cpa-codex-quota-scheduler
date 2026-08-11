package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeeperRefreshGateSerializesAcrossInstancesAndBacksOff(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	targets := []keeperRefreshTarget{{AuthIndex: "idx-acct", Reason: "missing"}}
	fingerprint := keeperRefreshFingerprint(targets)
	now := time.Now().UTC().Truncate(time.Second)

	var reserved atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, _, err := reserveKeeperRefreshGate(statePath, fingerprint, 1, now, 2*time.Minute)
			if err != nil {
				t.Errorf("reserve gate: %v", err)
				return
			}
			if ok {
				reserved.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := reserved.Load(); got != 1 {
		t.Fatalf("concurrent reservations = %d; want 1", got)
	}

	record, ok, _, err := reserveKeeperRefreshGate(statePath, fingerprint, 1, now.Add(time.Minute), 2*time.Minute)
	if err != nil || ok || record.Attempt != 1 {
		t.Fatalf("cooldown reservation record=%#v ok=%v err=%v", record, ok, err)
	}
	record, ok, _, err = reserveKeeperRefreshGate(statePath, fingerprint, 1, now.Add(2*time.Minute), 2*time.Minute)
	if err != nil || !ok || record.Attempt != 2 || record.NextAllowedAt.Sub(record.RequestedAt) != 4*time.Minute {
		t.Fatalf("second reservation record=%#v ok=%v err=%v", record, ok, err)
	}
}

func TestBanResetConfirmationRefreshRunsWhileWarmupDisabled(t *testing.T) {
	var calls atomic.Int32
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeTestJSON(t, w, map[string]any{"accepted": 1, "limit": 1})
	}))
	defer keeper.Close()
	state := newRefreshTestState(t, keeper.URL)
	cfg := state.cfg
	cfg.Enabled = true
	cfg.WarmupEnabled = false
	cfg.KeeperRefreshCooldown = 2 * time.Minute
	targets := []keeperRefreshTarget{{
		AuthIndex: "idx-acct", Reason: "ban_reset_confirmation",
		ObservedAt: time.Now().UTC().Add(-time.Minute),
	}}
	err := state.requestKeeperQuotaRefreshTargets(context.Background(), cfg, "", "session-token", targets, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fail()
	}
	status := state.status()
	if status.WarmupEnabled || status.KeeperRefreshRequests != 1 || status.KeeperRefreshAccepted != 1 {
		t.Fail()
	}
}

func TestStaleKeeperCacheRefreshRunsWhileWarmupDisabled(t *testing.T) {
	var calls atomic.Int32
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quota/refresh" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		writeTestJSON(t, w, map[string]any{
			"tasks":    []map[string]any{{"authIndex": "idx-acct"}},
			"accepted": 1,
			"limit":    1,
		})
	}))
	defer keeper.Close()

	state := newRefreshTestState(t, keeper.URL)
	state.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	state.cfg.Enabled = true
	state.cfg.WarmupEnabled = false
	defer state.stop()

	cache := keeperCacheResponse{Items: []keeperCacheItem{{
		AuthIndex: "idx-acct",
		FileName:  "acct",
		Status:    "pending",
	}}}
	err := state.maybeRequestKeeperQuotaRefresh(
		context.Background(), state.cfg, "", "session-token",
		[]string{"idx-acct"}, cache, nil, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("warmup-disabled cache refresh calls = %d; want 1", got)
	}
	status := state.status()
	if status.WarmupEnabled || status.KeeperRefreshRequests != 1 || status.KeeperRefreshAccepted != 1 {
		t.Fatalf("warmup-disabled cache refresh status = %#v", status)
	}
}

func TestKeeperRefreshGateChangedEvidenceResetsBackoff(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC()
	first := keeperRefreshFingerprint([]keeperRefreshTarget{{AuthIndex: "idx", Reason: "missing"}})
	second := keeperRefreshFingerprint([]keeperRefreshTarget{{AuthIndex: "idx", Reason: "stale", ObservedAt: now.Add(-time.Hour)}})
	if _, ok, _, err := reserveKeeperRefreshGate(statePath, first, 1, now, 2*time.Minute); err != nil || !ok {
		t.Fatalf("first reserve ok=%v err=%v", ok, err)
	}
	record, ok, _, err := reserveKeeperRefreshGate(statePath, second, 1, now.Add(time.Second), 2*time.Minute)
	if err != nil || !ok || record.Attempt != 1 {
		t.Fatalf("changed evidence record=%#v ok=%v err=%v", record, ok, err)
	}
}

func TestKeeperRefreshGateRecoversCorruptRecord(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	path := keeperRefreshGatePath(statePath)
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	fingerprint := keeperRefreshFingerprint([]keeperRefreshTarget{{AuthIndex: "idx", Reason: "missing"}})
	_, ok, recovered, err := reserveKeeperRefreshGate(statePath, fingerprint, 1, time.Now(), 2*time.Minute)
	if err != nil || !ok || !recovered {
		t.Fatalf("corrupt recovery ok=%v recovered=%v err=%v", ok, recovered, err)
	}
	matches, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("corrupt quarantine matches=%v err=%v", matches, err)
	}
}

func TestCollectKeeperRefreshTargetsUsesKeeperNativeFailureTTL(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	stale := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)
	expires := now.Add(10 * time.Minute)
	quotas := map[string]quotaSnapshot{
		"fresh": {AuthID: "fresh", AuthIndex: "fresh", RefreshedAt: fresh, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: fresh}}},
		"stale": {AuthID: "stale", AuthIndex: "stale", RefreshedAt: stale, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: stale}}},
		"zero":  {AuthID: "zero", AuthIndex: "zero", Windows: []quotaWindow{{Class: "5h", Allowed: true}}},
	}
	status := http.StatusUnauthorized
	cache := keeperCacheResponse{Items: []keeperCacheItem{
		{AuthIndex: "blocked", Status: "failed", HTTPStatusCode: &status, RefreshedAt: json.RawMessage(fmt.Sprintf("%q", now.Format(time.RFC3339))), ExpiresAt: json.RawMessage(fmt.Sprintf("%q", expires.Format(time.RFC3339)))},
	}}
	targets := collectKeeperRefreshTargets([]string{"fresh", "stale", "zero", "missing", "blocked"}, cache, quotas, now, 15*time.Minute)
	got := make(map[string]string)
	for _, target := range targets {
		got[target.AuthIndex] = target.Reason
	}
	if len(got) != 3 || got["stale"] != "stale" || got["zero"] != "missing_refreshed_at" || got["missing"] != "missing" {
		t.Fatalf("targets = %#v", got)
	}
	if _, exists := got["fresh"]; exists {
		t.Fatal("fresh quota was refreshed")
	}
	if _, exists := got["blocked"]; exists {
		t.Fatal("unexpired Keeper failure bypassed native failure TTL")
	}
}

func TestRefreshOnceQueuesStaleKeeperCacheThenWarmsExactlyOnce(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	dir := t.TempDir()
	passwordPath := filepath.Join(dir, "keeper-password")
	managementKeyPath := filepath.Join(dir, "management-key")
	if err := os.WriteFile(passwordPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managementKeyPath, []byte("management-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var fresh atomic.Bool
	var refreshCalls atomic.Int32
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{{
				"identity": "idx-acct", "file_name": "acct", "provider": providerCodex,
			}}})
		case "/api/v1/quota/cache":
			observedAt := time.Now().UTC().Truncate(time.Second)
			if !fresh.Load() {
				observedAt = observedAt.Add(-time.Hour)
			}
			used, allowed, reached := 0.0, true, false
			seconds := int64((5 * time.Hour).Seconds())
			resetAfter := seconds
			cost := 0.0
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{{
				AuthIndex: "idx-acct", FileName: "acct", Status: "completed",
				RefreshedAt: json.RawMessage(fmt.Sprintf("%q", observedAt.Format(time.RFC3339))),
				Quota: &keeperCheckResponse{Quota: []keeperQuotaRow{{
					Label: "5h", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached,
					Window: &keeperQuotaWindow{Seconds: &seconds}, ResetAfterSeconds: &resetAfter,
					ResetAt: json.RawMessage(fmt.Sprintf("%d", observedAt.Add(5*time.Hour).Unix())), WindowUsageCost: &cost,
				}}},
			}}})
		case "/api/v1/quota/refresh":
			refreshCalls.Add(1)
			fresh.Store(true)
			writeTestJSON(t, w, map[string]any{
				"tasks": []map[string]any{{"authIndex": "idx-acct"}}, "rejected": []any{},
				"accepted": 1, "skipped": 0, "limit": 1,
			})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer keeper.Close()

	var warmupCalls atomic.Int32
	management := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{{
				"id": "acct", "auth_index": "idx-acct", "provider": providerCodex,
				"status": "active", "note": "Agent Identity via sidecar",
			}}})
		case "/v0/management/api-call":
			warmupCalls.Add(1)
			writeTestJSON(t, w, cpaAPICallResponse{StatusCode: http.StatusOK, Body: `{"status":"completed"}`})
		default:
			http.NotFound(w, r)
		}
	}))
	defer management.Close()

	state := newRefreshTestState(t, keeper.URL)
	state.cfg.KeeperPasswordFile = passwordPath
	state.cfg.WarmupEnabled = true
	state.cfg.WarmupExecutionMode = "management"
	state.cfg.KeeperRefreshCooldown = 2 * time.Minute
	state.cfg.CPAManagementURL = management.URL + "/v0/management/api-call"
	state.cfg.CPAManagementKeyFile = managementKeyPath
	state.cfg.WarmupSidecarURL = management.URL + "/backend-api/codex"
	state.cfg.StaleAfter = 15 * time.Minute
	defer state.stop()

	state.refreshOnce(context.Background())
	state.wg.Wait()
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("first stale scan refresh calls = %d; want 1", got)
	}
	if got := warmupCalls.Load(); got != 0 {
		t.Fatalf("stale cache triggered %d warmups; want 0", got)
	}

	state.refreshOnce(context.Background())
	state.wg.Wait()
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("fresh scan queued another refresh: %d", got)
	}
	if got := warmupCalls.Load(); got != 1 {
		t.Fatalf("fresh full quota warmups = %d; want 1", got)
	}

	state.refreshOnce(context.Background())
	state.wg.Wait()
	if got := warmupCalls.Load(); got != 1 {
		t.Fatalf("pending confirmation repeated warmup: %d", got)
	}
	status := state.status()
	if status.KeeperRefreshTargets != 0 || status.KeeperRefreshRequests != 1 || status.KeeperRefreshError != "" {
		t.Fatalf("keeper refresh status = %#v", status)
	}
}

func TestHotReloadGenerationsQueueOnlyOneKeeperRefresh(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	passwordPath := filepath.Join(t.TempDir(), "keeper-password")
	if err := os.WriteFile(passwordPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	var refreshCalls atomic.Int32
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{{"identity": "idx", "file_name": "acct", "provider": providerCodex}}})
		case "/api/v1/quota/cache":
			used, allowed, reached, seconds := 0.0, true, false, int64((5 * time.Hour).Seconds())
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{{
				AuthIndex: "idx", FileName: "acct", Status: "completed",
				RefreshedAt: json.RawMessage(fmt.Sprintf("%q", staleAt.Format(time.RFC3339))),
				Quota:       &keeperCheckResponse{Quota: []keeperQuotaRow{{Label: "5h", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached, Window: &keeperQuotaWindow{Seconds: &seconds}}}},
			}}})
		case "/api/v1/quota/refresh":
			refreshCalls.Add(1)
			writeTestJSON(t, w, map[string]any{"tasks": []map[string]any{{"authIndex": "idx"}}, "accepted": 1, "limit": 1})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer keeper.Close()

	old := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, old)
	incoming := newManagedRuntimeForTest(t, statePath)
	for _, state := range []*schedulerRuntimeState{old, incoming} {
		state.cfg.KeeperURL = keeper.URL
		state.cfg.KeeperPasswordFile = passwordPath
		state.cfg.WarmupEnabled = true
		state.cfg.KeeperRefreshCooldown = 2 * time.Minute
		state.cfg.StaleAfter = 15 * time.Minute
		state.cfg.CPAManagementURL = ""
	}
	t.Cleanup(func() {
		old.stop()
		incoming.stop()
	})

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, state := range []*schedulerRuntimeState{old, incoming} {
		state := state
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			state.refreshOnce(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("hot reload refresh calls = %d; want 1", got)
	}
}

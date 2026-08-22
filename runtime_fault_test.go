package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func newRefreshTestState(t *testing.T, keeperURL string) *schedulerRuntimeState {
	t.Helper()
	passwordPath := filepath.Join(t.TempDir(), "keeper-password")
	if err := os.WriteFile(passwordPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultPluginConfig()
	cfg.KeeperURL = keeperURL
	cfg.KeeperPasswordFile = passwordPath
	cfg.WarmupEnabled = false
	cfg.StatePath = ""
	now := time.Now()
	return &schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"old": {
				AuthID:      "old",
				RefreshedAt: now,
				Windows: []quotaWindow{{
					Class:       "weekly",
					UsedPercent: 10,
					Allowed:     true,
					ResetAt:     now.Add(24 * time.Hour),
					ObservedAt:  now,
				}},
			},
		},
		identities:     make(map[string]string),
		pricing:        map[string]modelPricing{"old-model": {Model: "old-model", PromptPricePer1M: 1, PriceMultiplier: 1}},
		costSamples:    make(map[string][]float64),
		pacingAccounts: make(map[string]*accountPacingState),
		stickyBindings: make(map[string]stickyBinding),
		warmups:        make(map[string]warmupEntry),
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode test response: %v", err)
	}
}

func TestEnabledKeeperCodexIdentitiesExcludesDisabledHistory(t *testing.T) {
	files, indexes := enabledKeeperCodexIdentities([]keeperIdentity{
		{Identity: "idx-active", FileName: "active.json", Provider: "codex"},
		{Identity: "idx-disabled", FileName: "disabled.json", Provider: "codex", Disabled: true},
		{Identity: "idx-deleted", FileName: "deleted.json", Provider: "codex", IsDeleted: true},
		{Identity: "idx-other", FileName: "other.json", Provider: "anthropic", Type: "claude"},
	})
	if len(indexes) != 1 || indexes[0] != "idx-active" {
		t.Fatalf("enabled indexes = %#v; want only idx-active", indexes)
	}
	if len(files) != 1 || files["idx-active"] != "active.json" {
		t.Fatalf("enabled identity map = %#v; want only active credential", files)
	}
}

func TestRefreshOnceNeverRequestsDisabledKeeperIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{
				{"identity": "idx-active", "file_name": "active.json", "provider": providerCodex, "disabled": false},
				{"identity": "idx-disabled", "file_name": "disabled.json", "provider": providerCodex, "disabled": true},
			}})
		case "/api/v1/quota/cache":
			var request struct {
				AuthIndexes []string `json:"auth_indexes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode quota cache request: %v", err)
			}
			requested = append([]string(nil), request.AuthIndexes...)
			used, allowed, reached, seconds := 1.0, true, false, int64(7*24*time.Hour/time.Second)
			quota := &keeperCheckResponse{Quota: []keeperQuotaRow{{
				Label: "weekly", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached,
				Window: &keeperQuotaWindow{Seconds: &seconds},
			}}}
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{
				{
					AuthIndex: "idx-active", FileName: "active.json", Status: "completed",
					RefreshedAt: json.RawMessage(fmt.Sprintf("%q", now.Format(time.RFC3339))), Quota: quota,
				},
				// A defensive server-side regression fixture: even if Keeper returns
				// an unrequested historical row, it must not enter scheduler state.
				{
					AuthIndex: "idx-disabled", FileName: "disabled.json", Status: "completed",
					RefreshedAt: json.RawMessage(fmt.Sprintf("%q", now.Format(time.RFC3339))), Quota: quota,
				},
			}})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	if len(requested) != 1 || requested[0] != "idx-active" {
		t.Fatalf("quota cache auth_indexes = %#v; disabled history must not be requested", requested)
	}
	if state.refreshes != 1 || state.lastError != "" {
		t.Fatalf("filtered refresh failed: refreshes=%d error=%q", state.refreshes, state.lastError)
	}
	if _, ok := state.quotas["idx-disabled"]; ok {
		t.Fatal("unrequested disabled Keeper cache row entered scheduler quota state")
	}
	if _, ok := state.quotas["disabled.json"]; ok {
		t.Fatal("unrequested disabled Keeper cache filename entered scheduler quota state")
	}
}

func TestRefreshOnceKeepsCacheButFailsClosedWhenCPAInventoryUnavailable(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var refreshCalls int
	keeper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{{
				"identity": "idx-active", "file_name": "active.json", "provider": providerCodex,
			}}})
		case "/api/v1/quota/cache":
			used, allowed, reached, seconds := 10.0, true, false, int64(7*24*time.Hour/time.Second)
			observedAt := now.Add(-time.Hour)
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{{
				AuthIndex: "idx-active", FileName: "active.json", Status: "completed",
				RefreshedAt: json.RawMessage(fmt.Sprintf("%q", observedAt.Format(time.RFC3339))),
				Quota: &keeperCheckResponse{Quota: []keeperQuotaRow{{
					Label: "weekly", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached,
					Window: &keeperQuotaWindow{Seconds: &seconds},
				}}},
			}}})
		case "/api/v1/quota/refresh":
			refreshCalls++
			writeTestJSON(t, w, map[string]any{"accepted": 1})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer keeper.Close()

	state := newRefreshTestState(t, keeper.URL)
	state.cfg.CPAManagementURL, state.cfg.CPAManagementKeyFile = newTestCPAAuthInventory(t, nil, http.StatusServiceUnavailable)
	state.cfg.StaleAfter = 15 * time.Minute
	state.refreshOnce(context.Background())
	if refreshCalls != 0 {
		t.Fatalf("Keeper refresh calls = %d; want 0 when CPA inventory is unavailable", refreshCalls)
	}
	if state.refreshes != 1 {
		t.Fatalf("usable read-only cache was not committed: refreshes=%d", state.refreshes)
	}
	if _, ok := state.quotas["idx-active"]; !ok {
		t.Fatal("last readable quota snapshot was discarded")
	}
	if got := state.status().KeeperRefreshError; got != "auth_inventory_unavailable" {
		t.Fatalf("keeper refresh error = %q; want auth_inventory_unavailable", got)
	}
}

func TestKeeperTimeoutPreservesLastKnownQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	state.refreshOnce(ctx)
	if _, ok := state.quotas["old"]; !ok {
		t.Fatal("Keeper timeout erased the last-known quota snapshot")
	}
	if state.refreshes != 0 {
		t.Fatalf("refreshes=%d; timed-out refresh must not commit", state.refreshes)
	}
}

func TestKeeper403PreservesLastKnownQuotaAndReportsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("{\"detail\":\"forbidden\"}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	if _, ok := state.quotas["old"]; !ok {
		t.Fatal("Keeper 403 erased the last-known quota snapshot")
	}
	if !strings.Contains(state.lastError, "HTTP 403") {
		t.Fatalf("lastError=%q; want Keeper 403", state.lastError)
	}
}

func TestKeeperEmptyIdentitySetPreservesLastKnownQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	if _, ok := state.quotas["old"]; !ok {
		t.Fatal("empty Keeper data erased the last-known quota snapshot")
	}
	if !strings.Contains(state.lastError, "no active Codex identities") {
		t.Fatalf("lastError=%q; want empty identity diagnostic", state.lastError)
	}
}

func TestPricingFailureKeepsLastKnownRateCardWhileQuotaRefreshSucceeds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{{
				"identity": "idx", "file_name": "acct", "provider": providerCodex,
			}}})
		case "/api/v1/quota/cache":
			used, allowed, reached, seconds, cost := 12.0, true, false, int64(5*60*60), 24.0
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{{
				AuthIndex:   "idx",
				FileName:    "acct",
				Status:      "completed",
				RefreshedAt: json.RawMessage(fmt.Sprintf("%q", now.Format(time.RFC3339))),
				Quota: &keeperCheckResponse{Quota: []keeperQuotaRow{{
					Label: "5h", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached,
					Window:          &keeperQuotaWindow{Seconds: &seconds},
					ResetAt:         json.RawMessage(fmt.Sprintf("%d", now.Add(4*time.Hour).Unix())),
					WindowUsageCost: &cost,
				}}},
			}}})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("{\"detail\":\"pricing unavailable\"}"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	if state.refreshes != 1 || state.lastError != "" {
		t.Fatalf("quota refresh failed because pricing failed: refreshes=%d error=%q", state.refreshes, state.lastError)
	}
	if _, ok := state.quotas["acct"]; !ok {
		t.Fatal("valid quota snapshot was not committed")
	}
	if _, ok := state.pricing["old-model"]; !ok || len(state.pricing) != 1 {
		t.Fatalf("last-known pricing was not retained: %#v", state.pricing)
	}
}

func TestEmptyPricingResponseIsNotAuthoritative(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/pricing" {
			http.NotFound(w, r)
			return
		}
		writeTestJSON(t, w, map[string]any{"pricing": []any{}})
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.KeeperURL = server.URL
	pricing, ok := fetchKeeperPricing(context.Background(), cfg, "token")
	if ok || pricing != nil {
		t.Fatalf("empty pricing became authoritative: ok=%v pricing=%#v", ok, pricing)
	}
}

func TestPartialKeeperSnapshotCarriesForwardFreshMissingWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now.Add(-time.Minute),
		Windows: []quotaWindow{
			{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now.Add(-time.Minute)},
			{Class: "monthly", UsedPercent: 60, Allowed: true, ResetAt: now.Add(20 * 24 * time.Hour), ObservedAt: now.Add(-time.Minute)},
		},
	}
	current := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now,
		Windows:     []quotaWindow{{Class: "weekly", UsedPercent: 21, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now}},
	}
	merged := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	if len(merged.Windows) != 2 {
		t.Fatalf("partial snapshot lost an active window: %#v", merged.Windows)
	}
	var monthly quotaWindow
	for _, window := range merged.Windows {
		if window.Class == "monthly" {
			monthly = window
		}
	}
	if monthly.Class == "" || !monthly.ObservedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("monthly carry-forward missing or freshness changed: %#v", monthly)
	}
	if !quotaSnapshotFresh(merged, now, 15*time.Minute) {
		t.Fatal("fresh carried window should remain schedulable")
	}
	if quotaSnapshotFresh(merged, now.Add(16*time.Minute), 15*time.Minute) {
		t.Fatal("stale carried window kept the account schedulable indefinitely")
	}
}

func TestPartialKeeperSnapshotDoesNotCarryExpiredWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := quotaSnapshot{
		AuthID:      "acct",
		RefreshedAt: now.Add(-time.Minute),
		Windows:     []quotaWindow{{Class: "monthly", UsedPercent: 100, Allowed: false, ResetAt: now.Add(-time.Second), ObservedAt: now.Add(-time.Minute)}},
	}
	current := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ObservedAt: now}}}
	merged := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	if len(merged.Windows) != 1 || merged.Windows[0].Class != "weekly" {
		t.Fatalf("expired prior-cycle window was carried forward: %#v", merged.Windows)
	}
}

func TestCapacityCalibrationDoesNotUseCrossResetDelta(t *testing.T) {
	now := time.Now().UTC()
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), pacingAccounts: make(map[string]*accountPacingState)}
	first := quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{{
		Class: "weekly", UsedPercent: 90, Allowed: true, ResetAt: now.Add(time.Hour), ObservedAt: now,
		WindowUsageCredits: 900, WindowUsageCreditsKnown: true,
	}}}
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": first}, now)
	secondObserved := now.Add(2 * time.Hour)
	second := quotaSnapshot{AuthID: "acct", RefreshedAt: secondObserved, Windows: []quotaWindow{{
		Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: secondObserved,
		WindowUsageCredits: 10, WindowUsageCreditsKnown: true,
	}}}
	state.updateCalibrationsLocked(map[string]quotaSnapshot{"acct": second}, secondObserved)
	estimate := state.pacingAccounts["acct"].Capacities["weekly"]
	if estimate.Samples != 2 || estimate.Credits != 1000 {
		t.Fatalf("cross-reset delta polluted capacity estimate: %#v", estimate)
	}
}

func TestHotReloadIsRaceSafeAndPreservesPacingState(t *testing.T) {
	resetBanStoreForTest()
	statePath := filepath.Join(t.TempDir(), "state.json")
	baseCfg := defaultPluginConfig()
	baseCfg.Enabled = false
	baseCfg.SchedulerMode = "legacy"
	baseCfg.StatePath = statePath
	now := time.Now()
	schedulerRuntime.stop()
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = baseCfg
	schedulerRuntime.quotas = map[string]quotaSnapshot{"acct": {
		AuthID: "acct", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: now.Add(24 * time.Hour), ObservedAt: now}},
	}}
	schedulerRuntime.identities = make(map[string]string)
	schedulerRuntime.pricing = make(map[string]modelPricing)
	schedulerRuntime.costSamples = map[string][]float64{"sentinel": {1, 2, 3}}
	schedulerRuntime.pacingAccounts = map[string]*accountPacingState{"acct": {
		DeficitCredits: 7,
		Capacities:     map[string]capacityEstimate{"weekly": {Credits: 1000, Samples: 4, UpdatedAt: now}},
		LastQuota:      make(map[string]quotaCalibrationPoint),
	}}
	schedulerRuntime.stickyBindings = make(map[string]stickyBinding)
	schedulerRuntime.mu.Unlock()

	req := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "acct", Provider: providerCodex}}}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 100; iteration++ {
				_, _ = schedulerRuntime.schedulerPick(req)
			}
		}()
	}
	for iteration := 0; iteration < 20; iteration++ {
		raw := []byte(fmt.Sprintf("enabled: false\nscheduler_mode: legacy\nstate_path: %q\n", statePath))
		configureSchedulerRuntime(raw)
		refreshedAt := time.Now()
		schedulerRuntime.mu.Lock()
		schedulerRuntime.quotas = map[string]quotaSnapshot{"acct": {
			AuthID: "acct", RefreshedAt: refreshedAt,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: refreshedAt.Add(24 * time.Hour), ObservedAt: refreshedAt}},
		}}
		schedulerRuntime.mu.Unlock()
	}
	wg.Wait()

	schedulerRuntime.mu.RLock()
	account := schedulerRuntime.pacingAccounts["acct"]
	generation := schedulerRuntime.configGeneration
	samples := append([]float64(nil), schedulerRuntime.costSamples["sentinel"]...)
	schedulerRuntime.mu.RUnlock()
	if account == nil || account.Capacities["weekly"].Samples != 4 || len(samples) != 3 {
		t.Fatalf("hot reload discarded pacing state: account=%#v samples=%v", account, samples)
	}
	if generation < 20 {
		t.Fatalf("config generation=%d; want at least 20", generation)
	}
}

func TestRuntimeStatusExposesShadowPacingAndQuarantineMetrics(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg:              cfg,
		configGeneration: 7,
		quotas: map[string]quotaSnapshot{"acct": {
			AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
			Windows: []quotaWindow{{
				Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(5 * 24 * time.Hour), ObservedAt: now,
				WindowUsageCredits: 250, WindowUsageCreditsKnown: true,
			}},
		}},
		pricing:           map[string]modelPricing{"gpt-5.6-sol": builtInFallbackPricing("gpt-5.6-sol")},
		globalCostSamples: []float64{1, 2, 3, 4, 5, 6, 7, 8},
		costSamples: map[string][]float64{
			costSampleKey("gpt-5.6-sol", "medium"): {1, 2, 3, 4, 5, 6, 7, 8},
		},
		pacingAccounts: map[string]*accountPacingState{"acct": {
			DeficitCredits: 10,
			LastAccruedAt:  now,
			Capacities: map[string]capacityEstimate{
				"weekly": {Credits: 1000, Samples: 4, UpdatedAt: now},
			},
			LastQuota:        make(map[string]quotaCalibrationPoint),
			PendingPredicted: []float64{3},
		}},
		stickyBindings: map[string]stickyBinding{"session-hash": {AuthID: "acct", LastUsedAt: now}},
		decisionHistory: []schedulerDecisionAudit{{
			At: now, Mode: "shadow", Model: "gpt-5.6-sol", SessionHash: "session-hash",
			LegacyAuthHash: "legacy-hash", DynamicAuthHash: "dynamic-hash", ReturnedAuthHash: "legacy-hash", Disagreed: true,
		}},
		sessionSwitches:     2,
		shadowDisagreements: 3,
		warmups:             make(map[string]warmupEntry),
	}
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation})

	status := state.status()
	if status.ConfigGeneration != 7 || status.StickyBindings != 1 || status.SessionSwitches != 2 || status.ShadowDisagreements != 3 {
		t.Fatalf("missing scheduler counters: %#v", status)
	}
	if len(status.CostProfiles) == 0 || status.CostProfiles[0].Samples != 8 || status.CostProfiles[0].P95 <= status.CostProfiles[0].P75 {
		t.Fatalf("cost quantiles missing: %#v", status.CostProfiles)
	}
	if len(status.Pacing) != 1 || len(status.Pacing[0].Windows) != 1 || status.Pacing[0].Windows[0].CapacitySamples != 4 || status.Pacing[0].PendingPredictedRequests != 1 {
		t.Fatalf("pacing status missing: %#v", status.Pacing)
	}
	if status.Quarantine.ProbeReady != 1 || status.Quarantine.Probation != 1 {
		t.Fatalf("quarantine status missing: %#v", status.Quarantine)
	}
	if len(status.RecentDecisions) != 1 || status.RecentDecisions[0].ReturnedAuthHash != "legacy-hash" {
		t.Fatalf("decision history missing or unredacted: %#v", status.RecentDecisions)
	}
	if len(status.Snapshots) != 1 || len(status.Snapshots[0].Windows) != 1 || status.Snapshots[0].Windows[0].ObservedAt == "" {
		t.Fatalf("per-window freshness missing: %#v", status.Snapshots)
	}
}

func TestConcurrentPersistenceKeepsLatestQuarantineState(t *testing.T) {
	resetBanStoreForTest()
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := defaultPluginConfig()
	cfg.StatePath = path
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	state.initializeGenerationOwnership(path)
	if err := state.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &state)

	const entries = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < entries; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			authID := fmt.Sprintf("acct-%02d", index)
			banStore.set(authID, banEntry{
				ResetAt: time.Now().Add(time.Hour), Window: "probation", Kind: banKindProbation,
			})
			state.persistBanState()
		}(index)
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Version != 5 || len(persisted.Bans) != entries {
		t.Fatalf("persisted state version=%d bans=%d; want version 5 and %d bans", persisted.Version, len(persisted.Bans), entries)
	}
}

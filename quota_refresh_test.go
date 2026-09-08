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
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func nativeQuotaTestBody(used float64) string {
	return fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%v,"limit_window_seconds":18000,"reset_after_seconds":600},"secondary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`, used)
}

func newRefreshTestState(t *testing.T, baseURL string) *schedulerRuntimeState {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultPluginConfig()
	cfg.CPAManagementURL = baseURL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.StatePath = ""
	cfg.WarmupEnabled = false
	return &schedulerRuntimeState{cfg: cfg, quotas: make(map[string]quotaSnapshot), identities: make(map[string]string), quotaPolls: make(map[string]quotaPollState), warmups: make(map[string]warmupEntry)}
}

func TestNativeQuotaRefreshUsesOnlyCPA(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing CPA authentication")
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{
				{"id": "a", "auth_index": "idx-a", "provider": "codex", "unavailable": true, "id_token": map[string]string{"chatgpt_account_id": "account-a"}},
				{"id": "disabled", "auth_index": "idx-disabled", "provider": "codex", "disabled": true},
				{"id": "other", "auth_index": "idx-other", "provider": "claude"},
			}})
		case "/v0/management/api-call":
			requests++
			var call cpaAPICallRequest
			if json.NewDecoder(r.Body).Decode(&call) != nil {
				t.Fatal("invalid call")
			}
			if call.AuthIndex != "idx-a" || call.Method != "GET" || call.URL != "https://chatgpt.com/backend-api/wham/usage" || call.Header["Authorization"] != "Bearer $TOKEN$" || call.Header["ChatGPT-Account-Id"] != "account-a" {
				t.Errorf("wrong quota request: %#v", call)
			}
			writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(25)})
		default:
			t.Errorf("unexpected external-service route: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	state.refreshOnce(context.Background())
	if requests != 1 || len(state.quotas) != 2 || state.quotas["a"].Windows[0].UsedPercent != 25 {
		t.Fatalf("quota/cache/throttle failed: requests=%d quotas=%#v", requests, state.quotas)
	}
	if state.lastError != "" {
		t.Fatal(state.lastError)
	}
}

func TestNativeQuotaFailurePreservesCacheAndBackoff(t *testing.T) {
	failed := false
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "idx", "provider": "codex"}}})
			return
		}
		calls++
		if failed {
			writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 429, Header: map[string][]string{"Retry-After": {"300"}}, Body: "private upstream body"})
			return
		}
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(45)})
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	before := state.quotas["a"]
	state.serialWeeklyRebalance = serialWeeklyRebalanceState{CandidateAuthID: "a", Confirmations: 1}
	failed = true
	state.quotaPolls["a"] = quotaPollState{AuthIndex: "idx"}
	state.refreshOnce(context.Background())
	state.refreshOnce(context.Background())
	if calls != 2 || state.quotas["a"].RefreshedAt != before.RefreshedAt || state.quotaPolls["a"].NextAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatal("failure lost cache or retry guard")
	}
	if strings.Contains(state.lastError, "private") || state.lastError != "quota_http_429" {
		t.Fatalf("unredacted/incorrect error: %s", state.lastError)
	}
	if state.serialWeeklyRebalance.Confirmations != 0 {
		t.Fatal("failed poll retained proactive weekly confirmation")
	}
}

func TestNativeQuotaEmptyInventoryPrunesCache(t *testing.T) {
	empty := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			files := []map[string]any{{"id": "a", "auth_index": "idx", "provider": "codex"}}
			if empty {
				files = nil
			}
			writeTestJSON(t, w, map[string]any{"files": files})
			return
		}
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(45)})
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	empty = true
	state.refreshOnce(context.Background())
	if len(state.quotas) != 0 || len(state.identities) != 0 || len(state.quotaPolls) != 0 {
		t.Fatal("disabled/deleted identity remained cached")
	}
}

func TestNativeQuotaParserAndTime(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	q, err := parseNativeQuota([]byte(nativeQuotaTestBody(25)), "a", "idx", now)
	if err != nil || len(q.Windows) != 2 || !q.Windows[0].ResetAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("native parse: %#v %v", q, err)
	}
	for _, raw := range []string{`{}`, `{"rate_limit":{}}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000}}}`, strings.ReplaceAll(nativeQuotaTestBody(25), `"used_percent":25`, `"used_percent":101`)} {
		if _, err := parseNativeQuota([]byte(raw), "a", "idx", now); err == nil {
			t.Fatalf("malformed quota admitted: %s", raw)
		}
	}
	seconds := int64(600)
	row := quotaProbeResponse{Quota: []quotaProbeRow{{Label: "5h", ResetAfterSeconds: &seconds}}}
	first := normalizeQuotaSnapshot("idx", "a", row, now, now)
	later := normalizeQuotaSnapshot("idx", "a", row, now, now.Add(5*time.Minute))
	if !first.Windows[0].ResetAt.Equal(later.Windows[0].ResetAt) {
		t.Fatal("cached countdown moved")
	}
}

func TestQuotaMergePreservesNewerHardObservation(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	prev := quotaSnapshot{RefreshedAt: now.Add(-time.Minute), Windows: []quotaWindow{{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ObservedAt: now, ResetAt: now.Add(time.Hour)}}}
	old := quotaSnapshot{RefreshedAt: now.Add(-time.Minute), Windows: []quotaWindow{{Class: "5h", UsedPercent: 80, Allowed: true, ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(time.Hour)}}}
	got := mergePartialQuotaSnapshot(prev, old, now, cfg.StaleAfter)
	if got.Windows[0].UsedPercent != 100 {
		t.Fatal("older quota rolled back fresh hard limit")
	}
	got.Windows = append(got.Windows, quotaWindow{Class: "weekly", Allowed: true, ObservedAt: now.Add(-16 * time.Minute), ResetAt: now.Add(time.Hour)})
	if inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "a"}, got, true, cfg, now).Eligible {
		t.Fatal("stale sibling erased hard limit")
	}
}

func TestQuotaPollStatePersistsWithoutCredentials(t *testing.T) {
	state := newRefreshTestState(t, "http://127.0.0.1")
	state.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	state.initializeGenerationOwnership(state.cfg.StatePath)
	if err := state.reserveGenerationOwnership(state.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, state)
	now := time.Now().UTC().Truncate(time.Second)
	q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(20)), "a", "idx", now)
	state.quotas["a"] = q
	state.quotas["idx"] = q
	state.quotaPolls["a"] = quotaPollState{AuthIndex: "idx", AttemptedAt: now, NextAt: now.Add(2 * time.Minute)}
	if !state.persistBanState() {
		t.Fatal("persist failed")
	}
	other := newRefreshTestState(t, "http://127.0.0.1")
	other.loadBanState(state.cfg.StatePath)
	if !other.quotaPolls["a"].NextAt.Equal(now.Add(2*time.Minute)) || len(other.quotas["a"].Windows) != 2 {
		t.Fatal("quota cache/backoff not restored")
	}
	raw, _ := os.ReadFile(state.cfg.StatePath)
	if strings.Contains(string(raw), "test-key") || strings.Contains(string(raw), "$TOKEN$") {
		t.Fatal("credential persisted")
	}
}

func TestNativeQuotaReloadRetainsPerAccountCooldown(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "idx", "provider": "codex"}}})
			return
		}
		calls++
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(35)})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "state.json")
	first := newRefreshTestState(t, server.URL)
	first.cfg.StatePath = path
	first.initializeGenerationOwnership(path)
	if err := first.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	first.refreshOnce(context.Background())
	second := newRefreshTestState(t, server.URL)
	second.cfg.StatePath = path
	second.initializeGenerationOwnership(path)
	if err := second.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	second.refreshOnce(context.Background())
	if calls != 1 || second.quotas["a"].Windows[0].UsedPercent != 35 || first.generationOwnerActive() {
		t.Fatalf("takeover lost cache/backoff or owner fence: calls=%d cache=%#v", calls, second.quotas)
	}
}

func TestNativeQuotaPollCannotOverwriteHeaderReceivedDuringRequest(t *testing.T) {
	var state *schedulerRuntimeState
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "idx", "provider": "codex"}}})
			return
		}
		state.observeUsage(pluginapi.UsageRecord{Provider: "codex", Generate: true, AuthID: "a", ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"100"},
		}})
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(80)})
	}))
	defer server.Close()
	state = newRefreshTestState(t, server.URL)
	initial, _ := parseNativeQuota([]byte(nativeQuotaTestBody(70)), "a", "idx", time.Now().Add(-time.Minute))
	state.quotas["a"] = initial
	state.quotas["idx"] = initial
	state.refreshOnce(context.Background())
	if state.quotas["a"].Windows[0].UsedPercent != 100 || state.quotas["idx"].Windows[0].UsedPercent != 100 {
		t.Fatalf("poll rolled back a newer hard limit or its alias: a=%#v idx=%#v", state.quotas["a"], state.quotas["idx"])
	}
}

func TestNativeQuotaPollingBatchDoesNotStarveStandby(t *testing.T) {
	var order []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			var files []map[string]any
			for _, id := range []string{"a", "b", "c"} {
				files = append(files, map[string]any{"id": id, "auth_index": "idx-" + id, "provider": "codex"})
			}
			writeTestJSON(t, w, map[string]any{"files": files})
			return
		}
		var req cpaAPICallRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		order = append(order, req.AuthIndex)
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(10)})
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.cfg.QuotaRefreshBatch = 1
	state.serialActiveAuthID = "b"
	for i := 0; i < 3; i++ {
		state.refreshOnce(context.Background())
	}
	if strings.Join(order, ",") != "idx-b,idx-a,idx-c" {
		t.Fatalf("batch fairness/active priority: %v", order)
	}
}

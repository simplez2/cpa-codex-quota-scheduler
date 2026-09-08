package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCPAWarmupDispatchEvidenceControlsRetryAndPersists(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	for _, tc := range []struct {
		name, dispatch, code string
		outerStatus          int
		missing              bool
		delay                time.Duration
	}{
		{"missing_binding", "not_sent", "auth_binding_stale", 0, true, 15 * time.Minute},
		{"gateway_unknown", "uncertain", "response_incomplete", 502, false, 5 * time.Hour},
		{"completed", "completed", "", 200, false, 5 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0/management/auth-files":
					files := []map[string]any{}
					if !tc.missing {
						files = append(files, map[string]any{"id": "a", "auth_index": "idx-a", "provider": "codex", "id_token": map[string]string{"chatgpt_account_id": "workspace-a"}})
					}
					writeTestJSON(t, w, map[string]any{"files": files})
				case "/v0/management/api-call":
					calls++
					var call cpaAPICallRequest
					if json.NewDecoder(r.Body).Decode(&call) != nil {
						t.Error("invalid CPA envelope")
					}
					if call.AuthIndex != "idx-a" || call.URL != "https://chatgpt.com/backend-api/codex/responses" || call.Header["ChatGPT-Account-Id"] != "workspace-a" || call.Header["Authorization"] != "Bearer $TOKEN$" {
						t.Errorf("wrong native CPA request: %+v", call)
					}
					if tc.outerStatus != 200 {
						w.WriteHeader(tc.outerStatus)
						_, _ = w.Write([]byte(`{"error":"failed to read response"}`))
						return
					}
					writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: `data: {"type":"response.completed","response":{"status":"completed"}}

`})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
			}))
			defer server.Close()
			cfg := defaultPluginConfig()
			cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
			cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
			cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "management-key")
			if err := os.WriteFile(cfg.CPAManagementKeyFile, []byte("local-test"), 0600); err != nil {
				t.Fatal(err)
			}
			s := newManagedRuntimeForTest(t, cfg.StatePath)
			s.cfg = cfg
			claimManagedRuntimeForTest(t, s)
			candidate := warmupCandidate{Snapshot: readyWarmupQuota("a", time.Now()), Window: quotaWindow{Class: "5h"}}
			s.executeWarmup(context.Background(), cfg, candidate)
			entry := s.warmups["a|5h"]
			if entry.DispatchState != tc.dispatch || entry.Error != tc.code || entry.SuppressUntil.Sub(entry.OutcomeAt) != tc.delay {
				t.Fatalf("outcome: %+v", entry)
			}
			if tc.missing && calls != 0 || !tc.missing && calls != 1 {
				t.Fatalf("dispatches=%d", calls)
			}
			loaded := schedulerRuntimeState{cfg: cfg}
			loaded.loadBanState(cfg.StatePath)
			if loaded.warmups["a|5h"].DispatchState != tc.dispatch {
				t.Fatal("reload lost dispatch evidence")
			}
			if !tc.missing {
				if _, _, ok := loaded.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, entry.OutcomeAt.Add(time.Hour), cfg.WarmupRetryAfter, entry.OutcomeAt.Add(time.Minute)); ok {
					t.Fatal("reload repeated completed/uncertain request")
				}
			}
		})
	}
}

func TestScopedWarmupRetryPreservesCyclesBansAndAttemptBudget(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), warmups: map[string]warmupEntry{
		"a|5h":     {AuthID: "a", Window: "5h", Error: "warmup_failed", SuppressUntil: now.Add(5 * time.Hour)},
		"a|weekly": {AuthID: "a", Window: "weekly", CompletedAt: now, ResetAt: now.Add(time.Hour)},
		"b|5h":     {AuthID: "b", Window: "5h", Error: "timeout", DispatchState: "uncertain", SuppressUntil: now.Add(5 * time.Hour)},
	}, warmupAttempts: []warmupAttempt{{AuthID: "a", At: now.Add(-time.Hour)}}}
	banStore.set("a", banEntry{ResetAt: now.Add(time.Hour), Window: "weekly"})
	if state.clearBlockedWarmupState("", true) != 0 {
		t.Fatal("bulk retry cleared uncertain outcomes")
	}
	if state.clearBlockedWarmupState("a", false) != 1 || len(state.warmups) != 2 {
		t.Fatal("scoped retry affected other account or success")
	}
	if len(state.warmupAttempts) != 1 {
		t.Fatal("retry refunded attempt budget")
	}
	if _, ok := banStore.lookup("a"); !ok {
		t.Fatal("retry bypassed quota ban")
	}
}

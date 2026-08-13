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
	"sync/atomic"
	"testing"
	"time"
)

func TestManagementWarmupNonRetryableFailuresStayBlocked(t *testing.T) {
	for _, errorCode := range []string{
		"cyber_policy",
		"cyber_abuse",
		"auth_unavailable",
		"deactivated_workspace",
	} {
		t.Run(errorCode, func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			keyPath := filepath.Join(t.TempDir(), "management-key")
			if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0/management/auth-files":
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
						"id": "acct", "auth_index": "idx-acct", "provider": providerCodex,
						"status": "active", "note": "Agent Identity via sidecar",
					}}})
				case "/v0/management/api-call":
					apiCalls.Add(1)
					upstream, _ := json.Marshal(map[string]any{
						"id": "resp_1", "status": "failed",
						"error": map[string]any{"code": errorCode, "message": "sensitive upstream detail"},
					})
					_ = json.NewEncoder(w).Encode(cpaAPICallResponse{
						StatusCode: http.StatusServiceUnavailable,
						Body:       string(upstream),
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			state := newManagementWarmupRuntimeForRetryTest(t, server.URL, keyPath)
			defer state.stop()
			state.scheduleWarmup(context.Background(), nil)
			state.wg.Wait()
			if got := apiCalls.Load(); got != 1 {
				t.Fatalf("first api-call count = %d; want 1", got)
			}
			entry := state.warmups[warmupKey("acct", "5h")]
			if !entry.Blocked || entry.Error != errorCode {
				t.Fatalf("non-retryable outcome = %#v", entry)
			}
			if strings.Contains(entry.Error, "sensitive") {
				t.Fatalf("persisted error leaked upstream detail: %q", entry.Error)
			}

			// Retry delay is intentionally negligible. The second scheduling pass
			// must still make no API call because policy/auth failures remain
			// blocked until an explicit reset or a changed CPA auth binding.
			state.scheduleWarmup(context.Background(), nil)
			state.wg.Wait()
			if got := apiCalls.Load(); got != 1 {
				t.Fatalf("blocked error %q retried immediately: api-calls=%d", errorCode, got)
			}
		})
	}
}

func TestManagementWarmupWaitsForStartupGrace(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "startup request should not occur", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	state := newManagementWarmupRuntimeForRetryTest(t, server.URL, keyPath)
	defer state.stop()
	state.generationMu.Lock()
	state.generation.ClaimedAt = time.Now().UTC()
	state.generationMu.Unlock()
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	if got := requests.Load(); got != 0 {
		t.Fatalf("startup grace allowed %d CPA management requests; want 0", got)
	}
	if len(state.warmups) != 0 {
		t.Fatalf("startup grace recorded a warmup attempt: %#v", state.warmups)
	}
}

func TestManagementWarmupHTTPAuthFailuresStayBlocked(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			keyPath := filepath.Join(t.TempDir(), "management-key")
			if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var apiCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v0/management/auth-files":
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
						"id": "acct", "auth_index": "idx-acct", "provider": providerCodex,
						"status": "active", "note": "Agent Identity via sidecar",
					}}})
				case "/v0/management/api-call":
					apiCalls.Add(1)
					_ = json.NewEncoder(w).Encode(cpaAPICallResponse{
						StatusCode: status,
						Body:       `{"error":{"message":"Bearer must-not-be-persisted"}}`,
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			state := newManagementWarmupRuntimeForRetryTest(t, server.URL, keyPath)
			defer state.stop()
			state.scheduleWarmup(context.Background(), nil)
			state.wg.Wait()
			entry := state.warmups[warmupKey("acct", "5h")]
			wantCode := fmt.Sprintf("http_%d", status)
			if !entry.Blocked || entry.Error != wantCode || strings.Contains(entry.Error, "Bearer") || strings.Contains(entry.Error, "must-not") {
				t.Fatalf("HTTP auth outcome = %#v; want blocked %q without response text", entry, wantCode)
			}

			state.scheduleWarmup(context.Background(), nil)
			state.wg.Wait()
			if got := apiCalls.Load(); got != 1 {
				t.Fatalf("HTTP auth failure %d retried immediately: api-calls=%d", status, got)
			}
		})
	}
}

func TestManagementWarmup429DoesNotImmediatelyRetry(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"id": "acct", "auth_index": "idx-acct", "provider": providerCodex,
				"status": "active", "note": "Agent Identity via sidecar",
			}}})
		case "/v0/management/api-call":
			apiCalls.Add(1)
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: statusTooManyRequests})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	state := newManagementWarmupRuntimeForRetryTest(t, server.URL, keyPath)
	defer state.stop()
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("first api-call count = %d; want 1", got)
	}
	entry, ok := banStore.lookup("acct")
	if !ok || entry.Kind != banKindProbation || entry.Phase != banPhaseCooldown {
		t.Fatalf("429 did not enter probation: entry=%#v ok=%v", entry, ok)
	}
	warmup := state.warmups[warmupKey("acct", "5h")]
	if warmup.Error != "http_429" || warmup.Blocked {
		t.Fatalf("429 warmup outcome = %#v; want retryable marker guarded by quarantine", warmup)
	}

	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("429 credential retried before probation expired: api-calls=%d", got)
	}
}

func newManagementWarmupRuntimeForRetryTest(t *testing.T, serverURL, keyPath string) *schedulerRuntimeState {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.WarmupEnabled = true
	cfg.WarmupExecutionMode = "management"
	cfg.CPAManagementURL = serverURL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.WarmupSidecarURL = serverURL + "/backend-api/codex"
	cfg.WarmupRetryAfter = time.Nanosecond
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	state := newManagedRuntimeForTest(t, cfg.StatePath)
	state.cfg = cfg
	state.quotas = map[string]quotaSnapshot{
		"acct": {
			AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now,
			Windows: []quotaWindow{{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now}},
		},
	}
	claimManagedRuntimeForTest(t, state)
	return state
}

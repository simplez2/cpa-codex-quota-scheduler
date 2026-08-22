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

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestUnstartedWarmupWindowRecognizesMissingAndPlaceholderReset(t *testing.T) {
	now := time.Now()
	window, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true},
	}}, now)
	if !ok || window.Class != "5h" {
		t.Fatalf("unstarted 5h window = %#v, ok=%v", window, ok)
	}
	observedAt := now.Add(-time.Minute)
	window, ok = unstartedWarmupWindow(quotaSnapshot{RefreshedAt: observedAt, Windows: []quotaWindow{
		{
			Class: "5h", WindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true,
			ResetAt: observedAt.Add(5 * time.Hour), ObservedAt: observedAt,
			WindowUsageCreditsKnown: true,
		},
	}}, now)
	if !ok || window.Class != "5h" {
		t.Fatalf("full-duration placeholder reset = %#v, ok=%v; want warmup", window, ok)
	}
	if _, ok := unstartedWarmupWindow(quotaSnapshot{RefreshedAt: observedAt, Windows: []quotaWindow{
		{
			Class: "5h", WindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 4 * 60 * 60,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true,
			ResetAt: observedAt.Add(4 * time.Hour), ObservedAt: observedAt,
			WindowUsageCreditsKnown: true,
		},
	}}, now); ok {
		t.Fatal("fixed active reset window must not be warmed")
	}
	if _, ok := unstartedWarmupWindow(quotaSnapshot{RefreshedAt: observedAt, Windows: []quotaWindow{
		{
			Class: "5h", WindowSeconds: 5 * 60 * 60, ResetAfterSeconds: 5 * 60 * 60,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true,
			ResetAt: observedAt.Add(5 * time.Hour), ObservedAt: observedAt,
			WindowUsageCredits: 0.01, WindowUsageCreditsKnown: true,
		},
	}}, now); ok {
		t.Fatal("non-zero usage cost proves the cycle already started")
	}
	if _, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 100, Allowed: true},
	}}, now); ok {
		t.Fatal("fully used window must not be warmed")
	}
}

func TestWarmupWindowPrefersFiveHourOverWeekly(t *testing.T) {
	now := time.Now()
	window, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "weekly", UsedPercent: 0, Allowed: true},
		{Class: "5h", UsedPercent: 0, Allowed: true},
	}}, now)
	if !ok || window.Class != "5h" {
		t.Fatalf("window = %#v, ok=%v; want 5h", window, ok)
	}
}

func TestUnstartedWarmupIgnoresUnknownKeeperRows(t *testing.T) {
	now := time.Now()
	window, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "unknown", UsedPercent: 100, Allowed: false, LimitReached: true},
		{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now},
	}}, now)
	if !ok || window.Class != "5h" {
		t.Fatalf("recognized warmup window = %#v, ok=%v; unrelated unknown row must be ignored", window, ok)
	}
	if _, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{{
		Class: "unknown", UsedPercent: 0, Allowed: true,
	}}}, now); ok {
		t.Fatal("unknown-only quota snapshot must not become a warmup candidate")
	}
}

func TestWarmupIncludesMonthlyOnlyAccount(t *testing.T) {
	now := time.Now()
	window, ok := unstartedWarmupWindow(quotaSnapshot{RefreshedAt: now, Windows: []quotaWindow{
		{
			Class: "monthly", WindowSeconds: 2628000, ResetAfterSeconds: 2628000,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true,
			ResetAt: now.Add(2628000 * time.Second), ObservedAt: now,
			WindowUsageCreditsKnown: true,
		},
	}}, now)
	if !ok || window.Class != "monthly" {
		t.Fatalf("monthly placeholder = %#v, ok=%v; want warmup", window, ok)
	}
}

func TestCPAAuthFilesEndpoint(t *testing.T) {
	got, err := cpaAuthFilesEndpoint("http://127.0.0.1:8317/v0/management/api-call")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8317/v0/management/auth-files" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestTrustedWarmupCredentialNote(t *testing.T) {
	tests := map[string]bool{
		"Agent Identity via sidecar":             true,
		"Codex Access Token via sidecar":         true,
		"Agent Identity via gateway":             true,
		"Codex Access Token via gateway":         true,
		"  cOdEx AcCeSs ToKeN vIa GaTeWaY\t\r\n": true,
		"":                                       false,
		"via sidecar":                            false,
		"via gateway":                            false,
		"Official OAuth":                         false,
		"Agent Identity via gateway extra":       false,
		"prefix Codex Access Token via sidecar":  false,
	}
	for note, want := range tests {
		if got := trustedWarmupCredentialNote(note); got != want {
			t.Errorf("trustedWarmupCredentialNote(%q)=%v want %v", note, got, want)
		}
	}
}

func TestWarmupEligibleAuthsRequiresActiveSidecarCredential(t *testing.T) {
	files := []cpaAuthFileEntry{
		{ID: "sidecar", AuthIndex: "idx-sidecar", Provider: "codex", Status: "active", Note: "Codex Access Token via sidecar"},
		{ID: "gateway", AuthIndex: "idx-gateway", Provider: "codex", Status: "active", Note: "Agent Identity via gateway"},
		{ID: "native", AuthIndex: "idx-native", Provider: "codex", Status: "active", Note: "Official OAuth"},
		{ID: "disabled", AuthIndex: "idx-disabled", Provider: "codex", Status: "disabled", Disabled: true, Note: "Agent Identity via sidecar"},
		{ID: "third-party", AuthIndex: "idx-third", Provider: "openai", Status: "active", Note: "via sidecar"},
	}
	got := warmupEligibleAuths(files)
	wantBindings := map[string]warmupAuthBinding{
		"sidecar":     {AuthID: "sidecar", AuthIndex: "idx-sidecar"},
		"idx-sidecar": {AuthID: "sidecar", AuthIndex: "idx-sidecar"},
		"gateway":     {AuthID: "gateway", AuthIndex: "idx-gateway"},
		"idx-gateway": {AuthID: "gateway", AuthIndex: "idx-gateway"},
	}
	for key, want := range wantBindings {
		if binding, ok := got[key]; !ok || binding != want {
			t.Fatalf("active Identity binding for %q = %#v, ok=%v want %#v", key, binding, ok, want)
		}
	}
	for _, key := range []string{"native", "idx-native", "disabled", "idx-disabled", "third-party", "idx-third"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%q must not be warmup eligible", key)
		}
	}
}

func TestWarmupEligibilityDiagnosticsExplainRejectedAuths(t *testing.T) {
	files := []cpaAuthFileEntry{
		{ID: "sidecar", AuthIndex: "idx-sidecar", Provider: "codex", Status: "active", Note: "Codex Access Token via sidecar"},
		{ID: "oauth", AuthIndex: "idx-oauth", Provider: "codex", Status: "active", Note: "Official OAuth"},
		{ID: "disabled", AuthIndex: "idx-disabled", Provider: "codex", Disabled: true, Note: "via sidecar"},
		{ID: "unavailable", AuthIndex: "idx-unavailable", Provider: "codex", Unavailable: true, Note: "via sidecar"},
		{ID: "inactive", AuthIndex: "idx-inactive", Provider: "codex", Status: "error", Note: "via sidecar"},
		{ID: "missing-index", Provider: "codex", Status: "active", Note: "via sidecar"},
		{ID: "other", AuthIndex: "idx-other", Provider: "openai", Status: "active", Note: "via sidecar"},
	}
	eligible, stats := warmupEligibleAuthsWithStats(files)
	if len(eligible) != 2 || stats.Seen != len(files) || stats.Eligible != 1 {
		t.Fatalf("eligible=%#v stats=%#v", eligible, stats)
	}
	wantRejected := map[string]int{
		"missing_sidecar_marker": 1,
		"disabled":               1,
		"unavailable":            1,
		"inactive_status":        1,
		"missing_auth_index":     1,
		"provider_mismatch":      1,
	}
	for reason, want := range wantRejected {
		if got := stats.Rejected[reason]; got != want {
			t.Fatalf("rejected[%q]=%d want %d; all=%#v", reason, got, want, stats.Rejected)
		}
	}
}

func TestHostAuthDiscoveryRequiresSidecarMarkerOnlyForManagement(t *testing.T) {
	files := []pluginapi.HostAuthFileEntry{
		{ID: "sidecar", AuthIndex: "idx-sidecar", Provider: "codex", Status: "active", Note: "Agent Identity via sidecar"},
		{ID: "gateway", AuthIndex: "idx-gateway", Provider: "codex", Status: "active", Note: "Codex Access Token via gateway"},
		{ID: "oauth", AuthIndex: "idx-oauth", Provider: "codex", Status: "active", Note: "Official OAuth"},
	}
	management, stats := warmupEligibleHostAuthsWithStats(files, true)
	if _, ok := management["sidecar"]; !ok || stats.Eligible != 2 || stats.Rejected["missing_sidecar_marker"] != 1 {
		t.Fatalf("management auth discovery = %#v stats=%#v", management, stats)
	}
	native, nativeStats := warmupEligibleHostAuthsWithStats(files, false)
	if _, ok := native["oauth"]; !ok || nativeStats.Eligible != 3 {
		t.Fatalf("native auth discovery = %#v stats=%#v", native, nativeStats)
	}
}

func TestNativeWarmupDoesNotFallBackToManagementAuthDiscovery(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.WarmupExecutionMode = "native"
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(cfg.CPAManagementKeyFile, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&schedulerRuntimeState{}).cpaWarmupEligibleAuths(context.Background(), cfg); err == nil {
		t.Fatal("native mode without Host API must fail closed")
	}
	if hits != 0 {
		t.Fatalf("native auth discovery silently fell back to management: hits=%d", hits)
	}
}

func TestManagementWarmupUsesOnlyActiveIdentityProxyAuths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/auth-files" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"gateway","auth_index":"idx-gateway","provider":"codex","status":"active","note":"Codex Access Token via gateway"},{"id":"native","auth_index":"idx-native","provider":"codex","status":"active","note":"official oauth"}]}`))
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.WarmupExecutionMode = "management"
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(cfg.CPAManagementKeyFile, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	eligible, err := (&schedulerRuntimeState{}).cpaWarmupEligibleAuths(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if binding, ok := eligible["gateway"]; !ok || binding.AuthIndex != "idx-gateway" {
		t.Fatalf("gateway binding = %#v ok=%v", binding, ok)
	}
	if _, ok := eligible["native"]; ok {
		t.Fatal("management warmup must not send an official native credential to the sidecar endpoint")
	}
}

func TestFindWarmupCandidateUsesCurrentCPAAuthIndex(t *testing.T) {
	now := time.Now()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotas: map[string]quotaSnapshot{
			"account": {
				AuthID: "account", AuthIndex: "stale-index", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "monthly", UsedPercent: 0, Allowed: true, ObservedAt: now}},
			},
		},
	}
	candidate, ok := state.findWarmupCandidate(map[string]warmupAuthBinding{
		"account": {AuthID: "account", AuthIndex: "current-index"},
	}, now)
	if !ok || candidate.Snapshot.AuthIndex != "current-index" {
		t.Fatalf("candidate = %#v, ok=%v; want current CPA auth index", candidate, ok)
	}
}

func TestManagementWarmupRevalidatesAuthBindingBeforeAPICall(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	apiCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		case "/v0/management/api-call":
			apiCalls++
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: http.StatusOK, Body: `{"status":"completed"}`})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	candidate := warmupCandidate{Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "stale"}, Window: quotaWindow{Class: "5h", Allowed: true}}
	state.executeManagementWarmup(context.Background(), cfg, candidate)
	if apiCalls != 0 {
		t.Fatalf("stale auth binding reached api-call: calls=%d", apiCalls)
	}
	entry := state.warmups[warmupKey("acct", "5h")]
	if entry.Error != "auth_binding_stale" || entry.Blocked {
		t.Fatalf("stale binding outcome=%#v", entry)
	}
}

func TestManagementWarmupUsesMinimalResponsesRequest(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/management/auth-files":
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"id": "acct", "auth_index": "current", "provider": "codex", "status": "active", "note": "Codex Access Token via sidecar",
			}}})
		case "/v0/management/api-call":
			var call cpaAPICallRequest
			if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
				t.Errorf("decode api-call: %v", err)
			}
			if call.AuthIndex != "current" {
				t.Errorf("auth_index=%q", call.AuthIndex)
			}
			if call.Header["Accept"] != "text/event-stream" {
				t.Errorf("Accept=%q", call.Header["Accept"])
			}
			if call.Header["X-OpenAI-Internal-Codex-Responses-Lite"] != "true" {
				t.Errorf("responses-lite=%q", call.Header["X-OpenAI-Internal-Codex-Responses-Lite"])
			}
			if call.Header["Originator"] != "codex_cli_rs" || call.Header["X-Codex-Routing-Hint"] != "model=gpt-5.6-luna" {
				t.Errorf("Codex headers=%#v", call.Header)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(call.Data), &payload); err != nil {
				t.Errorf("decode responses payload: %v", err)
			}
			reasoning, _ := payload["reasoning"].(map[string]any)
			input, _ := payload["input"].([]any)
			var additionalTools map[string]any
			var developerMessage map[string]any
			var userMessage map[string]any
			var developerContent []any
			var userContent []any
			var developerText map[string]any
			var userText map[string]any
			if len(input) == 3 {
				additionalTools, _ = input[0].(map[string]any)
				developerMessage, _ = input[1].(map[string]any)
				userMessage, _ = input[2].(map[string]any)
				developerContent, _ = developerMessage["content"].([]any)
				userContent, _ = userMessage["content"].([]any)
			}
			if len(developerContent) == 1 {
				developerText, _ = developerContent[0].(map[string]any)
			}
			if len(userContent) == 1 {
				userText, _ = userContent[0].(map[string]any)
			}
			additionalToolsList, additionalToolsOK := additionalTools["tools"].([]any)
			_, hasTopLevelTools := payload["tools"]
			include, includeOK := payload["include"].([]any)
			textControls, _ := payload["text"].(map[string]any)
			_, hasMaxOutputTokens := payload["max_output_tokens"]
			if payload["store"] != false || payload["stream"] != true || payload["tool_choice"] != "auto" ||
				payload["parallel_tool_calls"] != false || reasoning["effort"] != "low" || reasoning["context"] != "all_turns" ||
				additionalTools["type"] != "additional_tools" || additionalTools["role"] != "developer" ||
				!additionalToolsOK || len(additionalToolsList) != 0 || hasTopLevelTools ||
				developerMessage["type"] != "message" || developerMessage["role"] != "developer" ||
				developerText["type"] != "input_text" || developerText["text"] != "Reply briefly." ||
				userMessage["type"] != "message" || userMessage["role"] != "user" ||
				userText["type"] != "input_text" || userText["text"] != "hello" ||
				!includeOK || len(include) != 1 || include[0] != "reasoning.encrypted_content" ||
				textControls["verbosity"] != "low" || hasMaxOutputTokens {
				t.Errorf("non-minimal warmup payload: %#v", payload)
			}
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: http.StatusOK, Body: `{"status":"completed"}`})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	candidate := warmupCandidate{Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "stale"}, Window: quotaWindow{Class: "5h", Allowed: true}}
	state.executeManagementWarmup(context.Background(), cfg, candidate)
	entry := state.warmups[warmupKey("acct", "5h")]
	if entry.Status != http.StatusOK || entry.Error != "" {
		t.Fatalf("warmup outcome=%#v", entry)
	}
}

func TestWarmupSuccessfulRequestWithoutHeadersUsesFallbackWindow(t *testing.T) {
	if got := warmupFallbackWindow(quotaWindow{Class: "5h"}); got != 5*time.Hour {
		t.Fatalf("5h fallback = %v", got)
	}
	if got := warmupFallbackWindow(quotaWindow{Class: "weekly"}); got != 7*24*time.Hour {
		t.Fatalf("weekly fallback = %v", got)
	}
	if got := warmupFallbackWindow(quotaWindow{Class: "monthly", WindowSeconds: 2628000}); got != 2628000*time.Second {
		t.Fatalf("monthly exact fallback = %v", got)
	}
}

func TestWarmupSuppressionExpiresAtReset(t *testing.T) {
	now := time.Now()
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		"a|5h": {AuthID: "a", Window: "5h", AttemptedAt: now.Add(-time.Hour), ResetAt: now.Add(-time.Minute)},
	}}
	s.warmupMu.Lock()
	if s.warmupSuppressedLocked("a|5h", now, 15*time.Minute) {
		t.Fatal("expired reset should allow a new warmup")
	}
	if _, ok := s.warmups["a|5h"]; ok {
		t.Fatal("expired warmup state should be removed")
	}
	s.warmupMu.Unlock()
}

func TestWarmupSuppressionDiscardsLifecycleCancellation(t *testing.T) {
	now := time.Now()
	key := warmupKey("a", "weekly")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "a", Window: "weekly", AttemptedAt: now.Add(-time.Second),
			Error: "cancelled",
		},
	}}
	s.warmupMu.Lock()
	if s.warmupSuppressedLocked(key, now, 15*time.Minute) {
		t.Fatal("lifecycle cancellation should not suppress the next active generation")
	}
	if _, ok := s.warmups[key]; ok {
		t.Fatal("cancelled lifecycle state should be removed before retry")
	}
	s.warmupMu.Unlock()
}

func TestWarmupSuppressionKeepsReturnedHTTPStatusDuringBackoff(t *testing.T) {
	now := time.Now()
	key := warmupKey("a", "weekly")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "a", Window: "weekly", AttemptedAt: now.Add(-time.Second),
			Status: http.StatusOK, Error: "cancelled",
		},
	}}
	s.warmupMu.Lock()
	if !s.warmupSuppressedLocked(key, now, 15*time.Minute) {
		t.Fatal("an attempt with a returned HTTP status must obey retry_after")
	}
	if _, ok := s.warmups[key]; !ok {
		t.Fatal("returned HTTP outcome must not be discarded as lifecycle cancellation")
	}
	s.warmupMu.Unlock()
}

func TestWarmupSuppressionKeepsBlockedCancellation(t *testing.T) {
	now := time.Now()
	key := warmupKey("a", "weekly")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "a", Window: "weekly", AttemptedAt: now.Add(-time.Second),
			Error: "cancelled", Blocked: true,
		},
	}}
	s.warmupMu.Lock()
	if !s.warmupSuppressedLocked(key, now, 15*time.Minute) {
		t.Fatal("blocked state must remain suppressed regardless of its error code")
	}
	if _, ok := s.warmups[key]; !ok {
		t.Fatal("blocked state must not be removed")
	}
	s.warmupMu.Unlock()
}

func TestPriorGenerationWarmupRetryIsOneShot(t *testing.T) {
	now := time.Now()
	claimedAt := now.Add(-time.Minute)
	key := warmupKey("acct", "5h")
	candidate := warmupCandidate{Snapshot: quotaSnapshot{AuthID: "acct"}, Window: quotaWindow{Class: "5h"}}
	state := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {AuthID: "acct", Window: "5h", AttemptedAt: claimedAt.Add(-time.Second)},
	}}

	state.warmupMu.Lock()
	got, gotKey, ok := state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, now, 15*time.Minute, claimedAt)
	state.warmupMu.Unlock()
	if !ok || got.Snapshot.AuthID != "acct" || gotKey != key {
		t.Fatalf("previous-generation retry candidate=%#v key=%q ok=%v", got, gotKey, ok)
	}
	if _, exists := state.warmups[key]; exists {
		t.Fatal("previous-generation retryable state was not removed")
	}

	state.warmups[key] = warmupEntry{AuthID: "acct", Window: "5h", AttemptedAt: now}
	state.warmupMu.Lock()
	_, _, ok = state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, now.Add(time.Second), 15*time.Minute, claimedAt)
	state.warmupMu.Unlock()
	if ok {
		t.Fatal("same generation retried the failed warmup more than once")
	}

	for name, entry := range map[string]warmupEntry{
		"blocked": {AuthID: "acct", Window: "5h", AttemptedAt: claimedAt.Add(-time.Second), Error: "http_400", Blocked: true},
		"429":     {AuthID: "acct", Window: "5h", AttemptedAt: claimedAt.Add(-time.Second), Error: "http_429", Status: statusTooManyRequests},
		"success": {AuthID: "acct", Window: "5h", AttemptedAt: claimedAt.Add(-time.Second), Status: http.StatusOK},
	} {
		state.warmups[key] = entry
		state.warmupMu.Lock()
		_, _, ok = state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, now, 15*time.Minute, claimedAt)
		state.warmupMu.Unlock()
		if ok {
			t.Fatalf("%s outcome was incorrectly retried across generations", name)
		}
	}
}

func TestRetryableWarmupFromPriorGenerationOnlyRetriesUnfinishedAttempts(t *testing.T) {
	now := time.Now().UTC()
	claimedAt := now.Add(-time.Minute)
	priorAttempt := claimedAt.Add(-time.Second)

	tests := []struct {
		name  string
		entry warmupEntry
		want  bool
	}{
		{
			name:  "admitted attempt without outcome retries",
			entry: warmupEntry{AttemptedAt: priorAttempt},
			want:  true,
		},
		{
			name:  "lifecycle cancellation without status retries",
			entry: warmupEntry{AttemptedAt: priorAttempt, Error: "cancelled"},
			want:  true,
		},
		{
			name:  "status free transport failure waits for backoff",
			entry: warmupEntry{AttemptedAt: priorAttempt, Error: "warmup_failed"},
			want:  false,
		},
		{
			name:  "http 200 with generic sse error waits for backoff",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: http.StatusOK, Error: "error"},
			want:  false,
		},
		{
			name:  "clean http 200 does not retry",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: http.StatusOK},
			want:  false,
		},
		{
			name:  "http 502 waits for backoff",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: http.StatusBadGateway, Error: "http_502"},
			want:  false,
		},
		{
			name:  "cyber policy never retries",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: http.StatusOK, Error: "cyber_policy", Blocked: true},
			want:  false,
		},
		{
			name:  "auth unavailable never retries",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: http.StatusServiceUnavailable, Error: "auth_unavailable", Blocked: true},
			want:  false,
		},
		{
			name:  "http 429 does not use generation retry",
			entry: warmupEntry{AttemptedAt: priorAttempt, Status: statusTooManyRequests, Error: "http_429"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryableWarmupFromPriorGeneration(tt.entry, claimedAt); got != tt.want {
				t.Fatalf("retryableWarmupFromPriorGeneration() = %v; want %v; entry=%#v", got, tt.want, tt.entry)
			}
		})
	}
}

func TestPriorGenerationHTTPFailureKeepsRetryBackoff(t *testing.T) {
	now := time.Now().UTC()
	claimedAt := now.Add(-time.Minute)
	retryAfter := 15 * time.Minute
	key := warmupKey("acct", "weekly")
	candidate := warmupCandidate{Snapshot: quotaSnapshot{AuthID: "acct"}, Window: quotaWindow{Class: "weekly"}}
	state := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "acct", Window: "weekly", AttemptedAt: claimedAt.Add(-time.Second),
			Status: http.StatusBadGateway, Error: "http_502",
		},
	}}

	state.warmupMu.Lock()
	_, _, ok := state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, now, retryAfter, claimedAt)
	_, preserved := state.warmups[key]
	state.warmupMu.Unlock()
	if ok {
		t.Fatal("prior-generation HTTP 502 bypassed retry_after")
	}
	if !preserved {
		t.Fatal("prior-generation HTTP 502 outcome was discarded during backoff")
	}

	afterBackoff := now.Add(retryAfter)
	state.warmupMu.Lock()
	_, _, ok = state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, afterBackoff, retryAfter, claimedAt)
	state.warmupMu.Unlock()
	if !ok {
		t.Fatal("prior-generation HTTP 502 did not become retryable after retry_after")
	}
}

func TestNextWarmupCandidateSkipsSuppressedAccount(t *testing.T) {
	now := time.Now()
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		warmupKey("first", "weekly"): {
			AuthID:      "first",
			Window:      "weekly",
			AttemptedAt: now.Add(-time.Minute),
			ResetAt:     now.Add(6 * 24 * time.Hour),
		},
	}}
	candidates := []warmupCandidate{
		{Snapshot: quotaSnapshot{AuthID: "first"}, Window: quotaWindow{Class: "weekly"}},
		{Snapshot: quotaSnapshot{AuthID: "second"}, Window: quotaWindow{Class: "weekly"}},
	}

	s.warmupMu.Lock()
	candidate, key, ok := s.nextWarmupCandidateLocked(candidates, now, 15*time.Minute)
	s.warmupMu.Unlock()
	if !ok || candidate.Snapshot.AuthID != "second" || key != warmupKey("second", "weekly") {
		t.Fatalf("next warmup candidate = %#v, key=%q, ok=%v; want second", candidate, key, ok)
	}
}

func TestWarmupCandidateStatusExcludesConfirmedCurrentCycle(t *testing.T) {
	now := time.Now().UTC()
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		warmupKey("acct", "weekly"): {
			AuthID: "acct", Window: "weekly", AttemptedAt: now.Add(-time.Minute),
			CompletedAt: now.Add(-time.Minute), ActivatedAt: now.Add(-time.Minute),
			ResetAt: now.Add(7 * 24 * time.Hour), SuppressUntil: now.Add(7 * 24 * time.Hour), Status: http.StatusOK,
		},
	}}
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now},
		Window: quotaWindow{
			Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			ResetAfterSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAfterSecondsKnown: true,
			ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now, Allowed: true,
		},
	}
	if got := state.countActionableWarmupCandidates([]warmupCandidate{candidate}, now, 15*time.Minute); got != 0 {
		t.Fatalf("actionable warmup candidates = %d; want 0 for confirmed current cycle", got)
	}
}

func TestWarmupCandidateStatusCountsExpiredOrStaleSuppression(t *testing.T) {
	now := time.Now().UTC()
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now},
		Window:   quotaWindow{Class: "weekly", ResetAt: now.Add(7 * 24 * time.Hour), ObservedAt: now, Allowed: true},
	}
	tests := []warmupEntry{
		{AuthID: "acct", Window: "weekly", AttemptedAt: now.Add(-time.Hour)},
		{AuthID: "acct", Window: "weekly", ActivatedAt: now.Add(-8 * 24 * time.Hour), ResetAt: now.Add(-24 * time.Hour)},
	}
	for _, entry := range tests {
		state := schedulerRuntimeState{warmups: map[string]warmupEntry{warmupKey("acct", "weekly"): entry}}
		if got := state.countActionableWarmupCandidates([]warmupCandidate{candidate}, now, 15*time.Minute); got != 1 {
			t.Fatalf("actionable warmup candidates = %d for entry %#v; want 1", got, entry)
		}
	}
}

func TestNextWarmupCandidateDiscardsStaleActivationAfterExternalReset(t *testing.T) {
	now := time.Now()
	oldReset := now.Add(20 * 24 * time.Hour)
	newPlaceholderReset := now.Add(30 * 24 * time.Hour)
	key := warmupKey("monthly", "monthly")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "monthly", Window: "monthly", AttemptedAt: now.Add(-48 * time.Hour),
			ActivatedAt: now.Add(-48 * time.Hour), ResetAt: oldReset, Status: http.StatusOK,
		},
	}}
	candidates := []warmupCandidate{{
		Snapshot: quotaSnapshot{AuthID: "monthly", RefreshedAt: now},
		Window: quotaWindow{
			Class:                  "monthly",
			WindowSeconds:          int64((30 * 24 * time.Hour).Seconds()),
			ResetAfterSeconds:      int64((30 * 24 * time.Hour).Seconds()),
			ResetAfterSecondsKnown: true,
			UsedPercent:            0,
			Allowed:                true,
			ResetAt:                newPlaceholderReset,
		},
	}}

	s.warmupMu.Lock()
	candidate, gotKey, ok := s.nextWarmupCandidateLocked(candidates, now, 15*time.Minute)
	s.warmupMu.Unlock()
	if !ok || candidate.Snapshot.AuthID != "monthly" || gotKey != key {
		t.Fatalf("candidate = %#v, key=%q, ok=%v; stale activation should not suppress", candidate, gotKey, ok)
	}
	if _, exists := s.warmups[key]; exists {
		t.Fatal("stale warmup activation was not removed")
	}
}

func TestNextWarmupCandidateRetriesUnconfirmedWarmupAfterFreshGrace(t *testing.T) {
	now := time.Now()
	windowSeconds := int64((5 * time.Hour).Seconds())
	key := warmupKey("pending", "5h")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "pending", Window: "5h", AttemptedAt: now.Add(-time.Hour),
			CompletedAt: now.Add(-time.Hour), SuppressUntil: now.Add(4 * time.Hour), Status: http.StatusOK,
		},
	}}
	candidates := []warmupCandidate{{
		Snapshot: quotaSnapshot{AuthID: "pending", RefreshedAt: now},
		Window: quotaWindow{
			Class: "5h", WindowSeconds: windowSeconds, ResetAfterSeconds: windowSeconds,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true, ResetAt: now.Add(5 * time.Hour),
		},
	}}

	s.warmupMu.Lock()
	candidate, gotKey, ok := s.nextWarmupCandidateLocked(candidates, now, 15*time.Minute)
	s.warmupMu.Unlock()
	if !ok || candidate.Snapshot.AuthID != "pending" || gotKey != key {
		t.Fatalf("candidate = %#v, key=%q, ok=%v; unconfirmed warmup should retry after fresh grace", candidate, gotKey, ok)
	}
	if _, exists := s.warmups[key]; exists {
		t.Fatal("stale pending warmup state was not removed")
	}
}

func TestNextWarmupCandidateKeepsPendingWarmupDuringGrace(t *testing.T) {
	now := time.Now()
	windowSeconds := int64((5 * time.Hour).Seconds())
	key := warmupKey("pending", "5h")
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "pending", Window: "5h", AttemptedAt: now.Add(-10 * time.Minute),
			CompletedAt: now.Add(-10 * time.Minute), SuppressUntil: now.Add(5 * time.Hour), Status: http.StatusOK,
		},
	}}
	candidates := []warmupCandidate{{
		Snapshot: quotaSnapshot{AuthID: "pending", RefreshedAt: now},
		Window: quotaWindow{
			Class: "5h", WindowSeconds: windowSeconds, ResetAfterSeconds: windowSeconds,
			ResetAfterSecondsKnown: true, UsedPercent: 0, Allowed: true, ResetAt: now.Add(5 * time.Hour),
		},
	}}

	s.warmupMu.Lock()
	_, _, ok := s.nextWarmupCandidateLocked(candidates, now, 15*time.Minute)
	s.warmupMu.Unlock()
	if ok {
		t.Fatal("pending warmup must remain suppressed during confirmation grace")
	}
	if _, exists := s.warmups[key]; !exists {
		t.Fatal("pending warmup state was removed before confirmation grace elapsed")
	}
}

func TestBlockedWarmupRequiresChangedCPAAuthBinding(t *testing.T) {
	now := time.Now()
	key := warmupKey("acct", "5h")
	state := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {AuthID: "acct", AuthIndex: "old-index", Window: "5h", AttemptedAt: now.Add(-time.Hour), Error: "cyber_policy", Blocked: true},
	}}
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "old-index", RefreshedAt: now},
		Window:   quotaWindow{Class: "5h", UsedPercent: 0, Allowed: true},
	}
	state.warmupMu.Lock()
	_, _, ok := state.nextWarmupCandidateLocked([]warmupCandidate{candidate}, now, 15*time.Minute)
	state.warmupMu.Unlock()
	if ok {
		t.Fatal("blocked policy failure must not retry with the same CPA auth binding")
	}

	candidate.Snapshot.AuthIndex = "new-index"
	state.warmupMu.Lock()
	got, gotKey, ok := state.nextWarmupCandidateLocked([]warmupCandidate{candidate}, now, 15*time.Minute)
	state.warmupMu.Unlock()
	if !ok || got.Snapshot.AuthIndex != "new-index" || gotKey != key {
		t.Fatalf("changed binding candidate = %#v key=%q ok=%v", got, gotKey, ok)
	}
	if _, exists := state.warmups[key]; exists {
		t.Fatal("blocked warmup state was not cleared after CPA auth binding changed")
	}
}

func TestExplicitWarmupRetryClearsOnlyBlockedTarget(t *testing.T) {
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		warmupKey("first", "5h"):  {AuthID: "first", Window: "5h", Blocked: true, Error: "cyber_policy"},
		warmupKey("second", "5h"): {AuthID: "second", Window: "5h", Blocked: true, Error: "http_403"},
		warmupKey("retry", "5h"):  {AuthID: "retry", Window: "5h", Error: "http_503"},
	}}
	if removed := state.clearBlockedWarmupState("first", false); removed != 1 {
		t.Fatalf("removed blocked target = %d; want 1", removed)
	}
	if _, ok := state.warmups[warmupKey("first", "5h")]; ok {
		t.Fatal("explicit target remained blocked")
	}
	if _, ok := state.warmups[warmupKey("second", "5h")]; !ok {
		t.Fatal("unrelated blocked target was cleared")
	}
	if removed := state.clearBlockedWarmupState("", true); removed != 1 {
		t.Fatalf("clear all blocked removed = %d; want 1", removed)
	}
	if _, ok := state.warmups[warmupKey("retry", "5h")]; !ok {
		t.Fatal("retryable warmup failure must not be removed by blocked-only reset")
	}
}

func TestCompletedWarmupWithoutHeadersStaysPendingWithoutFakeReset(t *testing.T) {
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry)}
	state.cfg.StatePath = ""
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct"},
		Window:   quotaWindow{Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds())},
	}
	state.recordWarmupOutcome(candidate, http.StatusOK, nil, nil)
	entry := state.warmups[warmupKey("acct", "weekly")]
	if entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() || !entry.ResetAt.IsZero() || entry.SuppressUntil.IsZero() {
		t.Fatalf("pending warmup entry = %#v", entry)
	}
}

func TestPendingWarmupConfirmsOnlyFromFreshStableKeeperAnchor(t *testing.T) {
	now := time.Now().UTC()
	key := warmupKey("acct", "weekly")
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		key: {
			AuthID: "acct", AuthIndex: "idx-acct", Window: "weekly",
			AttemptedAt: now.Add(-2 * time.Minute), CompletedAt: now.Add(-2 * time.Minute),
			SuppressUntil: now.Add(7 * 24 * time.Hour), Status: http.StatusOK,
		},
	}}
	stableReset := now.Add(6 * 24 * time.Hour)
	quotas := map[string]quotaSnapshot{"acct": {
		AuthID: "acct", RefreshedAt: now,
		Windows: []quotaWindow{{
			Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			UsedPercent: 0, Allowed: true, ResetAt: stableReset, ObservedAt: now,
		}},
	}}
	if !state.confirmPendingWarmups(quotas, now) {
		t.Fatal("fresh stable Keeper anchor did not confirm pending warmup")
	}
	entry := state.warmups[key]
	if entry.ActivatedAt.IsZero() || !entry.ResetAt.Equal(stableReset) || !entry.SuppressUntil.Equal(stableReset) {
		t.Fatalf("confirmed warmup entry = %#v", entry)
	}
}

func TestPruneExpiredWarmupsRemovesOldCycles(t *testing.T) {
	now := time.Now()
	s := &schedulerRuntimeState{warmups: map[string]warmupEntry{
		"expired|weekly": {AuthID: "expired", Window: "weekly", ResetAt: now.Add(-time.Minute)},
		"active|weekly":  {AuthID: "active", Window: "weekly", ResetAt: now.Add(time.Hour)},
	}}
	if !s.pruneExpiredWarmups(now) {
		t.Fatal("expired warmup should report a state change")
	}
	if _, ok := s.warmups["expired|weekly"]; ok {
		t.Fatal("expired warmup was not removed")
	}
	if _, ok := s.warmups["active|weekly"]; !ok {
		t.Fatal("active warmup must be preserved")
	}
}

func TestFindWarmupCandidateSkipsQuarantinedAuth(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"quarantined": {
				AuthID: "quarantined", AuthIndex: "idx-q", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now}},
			},
			"healthy": {
				AuthID: "healthy", AuthIndex: "idx-h", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now}},
			},
		},
	}
	banStore.set("quarantined", banEntry{
		ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation,
	})
	candidate, ok := state.findWarmupCandidate(map[string]warmupAuthBinding{
		"quarantined": {AuthID: "quarantined", AuthIndex: "idx-q"},
		"idx-q":       {AuthID: "quarantined", AuthIndex: "idx-q"},
		"healthy":     {AuthID: "healthy", AuthIndex: "idx-h"},
		"idx-h":       {AuthID: "healthy", AuthIndex: "idx-h"},
	}, now)
	if !ok || candidate.Snapshot.AuthID != "healthy" {
		t.Fatalf("warmup candidate = %#v, ok=%v; quarantined auth must be skipped", candidate, ok)
	}
}

func TestFindWarmupCandidateRejectsCarriedStaleRecognizedWindow(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	cfg.StaleAfter = 15 * time.Minute

	// The outer snapshot and weekly row are fresh, but the monthly row was
	// carried through successive partial Keeper responses. Its own observation
	// is authoritative for warmup and must not be refreshed by the envelope.
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"acct": {
				AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "weekly", UsedPercent: 0, Allowed: true, ObservedAt: now},
					{Class: "monthly", UsedPercent: 0, Allowed: true, ObservedAt: now.Add(-16 * time.Minute)},
				},
			},
		},
	}
	candidates := state.findWarmupCandidates(map[string]warmupAuthBinding{
		"acct": {AuthID: "acct", AuthIndex: "idx-acct"},
	}, nil, now)
	if len(candidates) != 0 {
		t.Fatalf("stale carried window admitted warmup candidates: %#v", candidates)
	}
	if state.warmupSkippedStaleLast != 1 {
		t.Fatalf("stale warmup diagnostics = %d; want 1", state.warmupSkippedStaleLast)
	}
}

func TestWarmupSnapshotFreshRequiresOwnObservationForEveryRecognizedWindow(t *testing.T) {
	now := time.Now().UTC()
	snapshot := quotaSnapshot{
		RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", Allowed: true, ObservedAt: now},
			{Class: "unknown", Allowed: true},
		},
	}
	if !warmupSnapshotFresh(snapshot, now, 15*time.Minute) {
		t.Fatal("an unobserved unknown row should not invalidate a fresh recognized window")
	}
	snapshot.Windows = append(snapshot.Windows, quotaWindow{Class: "weekly", Allowed: true})
	if warmupSnapshotFresh(snapshot, now, 15*time.Minute) {
		t.Fatal("recognized row without its own ObservedAt was accepted for warmup")
	}
}

func TestWarmupHeaderless429EntersProbation(t *testing.T) {
	resetBanStoreForTest()
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/auth-files" {
			_, _ = w.Write([]byte(`{"files":[{"id":"acct","auth_index":"idx","provider":"codex","status":"active","note":"Agent Identity via sidecar"}]}`))
			return
		}
		if err := json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: statusTooManyRequests}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	cfg := defaultPluginConfig()
	cfg.CPAManagementURL = server.URL
	cfg.CPAManagementKeyFile = keyPath
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "idx"},
		Window:   quotaWindow{Class: "5h", Allowed: true},
	}
	state.executeWarmup(context.Background(), cfg, candidate)

	entry, ok := banStore.lookup("acct")
	if !ok || entry.Kind != banKindProbation || entry.Phase != banPhaseCooldown {
		t.Fatalf("warmup 429 quarantine = %#v, ok=%v", entry, ok)
	}
	if entry.ResetAt.Before(time.Now().Add(10 * time.Minute)) {
		t.Fatalf("warmup probation deadline too short: %v", entry.ResetAt)
	}
	stats := banStore.stats()
	if stats.Total429s != 1 || stats.Probation429s != 1 {
		t.Fatalf("warmup 429 counters = %#v", stats)
	}
	warm := state.warmups[warmupKey("acct", "5h")]
	if warm.Status != statusTooManyRequests || warm.Error == "" {
		t.Fatalf("warmup outcome = %#v", warm)
	}
}

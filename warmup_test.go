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

func TestUnstartedWarmupWindowRequiresZeroUsageAndNoReset(t *testing.T) {
	now := time.Now()
	window, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true},
	}}, now)
	if !ok || window.Class != "5h" {
		t.Fatalf("unstarted 5h window = %#v, ok=%v", window, ok)
	}
	if _, ok := unstartedWarmupWindow(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 0, Allowed: true, ResetAt: now.Add(time.Hour)},
	}}, now); ok {
		t.Fatal("active reset window must not be warmed")
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

func TestCPAAuthFilesEndpoint(t *testing.T) {
	got, err := cpaAuthFilesEndpoint("http://127.0.0.1:8317/v0/management/api-call")
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8317/v0/management/auth-files" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestWarmupEligibleAuthsRequiresActiveSidecarCredential(t *testing.T) {
	files := []cpaAuthFileEntry{
		{ID: "sidecar", AuthIndex: "idx-sidecar", Provider: "codex", Status: "active", Note: "Codex Access Token via sidecar"},
		{ID: "native", AuthIndex: "idx-native", Provider: "codex", Status: "active", Note: "Official OAuth"},
		{ID: "disabled", AuthIndex: "idx-disabled", Provider: "codex", Status: "disabled", Disabled: true, Note: "Agent Identity via sidecar"},
		{ID: "third-party", AuthIndex: "idx-third", Provider: "openai", Status: "active", Note: "via sidecar"},
	}
	got := warmupEligibleAuths(files)
	if !got["sidecar"] || !got["idx-sidecar"] {
		t.Fatal("active sidecar credential should be eligible")
	}
	for _, key := range []string{"native", "idx-native", "disabled", "idx-disabled", "third-party", "idx-third"} {
		if got[key] {
			t.Fatalf("%q must not be warmup eligible", key)
		}
	}
}

func TestWarmupSuccessfulRequestWithoutHeadersUsesFallbackWindow(t *testing.T) {
	if got := warmupFallbackWindow("5h"); got != 5*time.Hour {
		t.Fatalf("5h fallback = %v", got)
	}
	if got := warmupFallbackWindow("weekly"); got != 7*24*time.Hour {
		t.Fatalf("weekly fallback = %v", got)
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
	candidate, ok := state.findWarmupCandidate(map[string]bool{
		"quarantined": true, "idx-q": true, "healthy": true, "idx-h": true,
	}, now)
	if !ok || candidate.Snapshot.AuthID != "healthy" {
		t.Fatalf("warmup candidate = %#v, ok=%v; quarantined auth must be skipped", candidate, ok)
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

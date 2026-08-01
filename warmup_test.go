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
		Snapshot: quotaSnapshot{AuthID: "monthly"},
		Window:   quotaWindow{Class: "monthly", ResetAt: newPlaceholderReset},
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

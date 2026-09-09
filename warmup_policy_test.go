package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func readyWarmupQuota(id string, now time.Time) quotaSnapshot {
	return quotaSnapshot{AuthID: id, AuthIndex: "idx-" + id, RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", WindowSeconds: 18000, Allowed: true, ObservedAt: now},
		{Class: "weekly", WindowSeconds: 604800, UsedPercent: 20, Allowed: true, ObservedAt: now, ResetAt: now.Add(24 * time.Hour)},
	}}
}

func TestWarmupTrafficBudgetIsRollingAndSurvivesStateReload(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC()
	cfg := defaultPluginConfig()
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	cfg.WarmupMaxPerDay = 2
	state := newManagedRuntimeForTest(t, cfg.StatePath)
	state.cfg = cfg
	claimManagedRuntimeForTest(t, state)
	state.warmupAttempts = []warmupAttempt{{AuthID: "a", At: now.Add(-23 * time.Hour)}, {AuthID: "a", At: now.Add(-time.Hour)}}
	state.warmups["b|5h"] = warmupEntry{AuthID: "a", Window: "5h", AttemptedAt: now.Add(-time.Hour), ResetAt: now.Add(-time.Minute)}
	if !state.pruneExpiredWarmups(now) || !state.persistBanState() {
		t.Fatal("could not save pruned state")
	}
	loaded := schedulerRuntimeState{cfg: cfg}
	loaded.loadBanState(cfg.StatePath)
	loaded.loadBanState(cfg.StatePath)
	status := loaded.warmupTrafficStatusLocked(cfg, now, "a")
	if status.AttemptsLast24h != 2 || status.HoldReason != "daily_budget" || !status.NextAllowedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("reloaded budget: %#v", status)
	}
	if status = loaded.warmupTrafficStatusLocked(cfg, now.Add(time.Hour), "a"); status.HoldReason != "" || status.AttemptsLast24h != 1 {
		t.Fatalf("rolling expiry: %#v", status)
	}
	// A clock rollback must wait; it must not erase a future admission.
	loaded.warmupAttempts = []warmupAttempt{{AuthID: "a", At: now.Add(time.Minute)}}
	status = loaded.warmupTrafficStatusLocked(cfg, now)
	if status.HoldReason != "min_interval" || !status.NextAllowedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("clock rollback bypassed spacing: %#v", status)
	}
}

func TestWarmupFailureBackoffAndManualRecoveryPreserveBudget(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
	candidate := warmupCandidate{Snapshot: readyWarmupQuota("a", time.Now()), Window: quotaWindow{Class: "5h"}}
	for failure := 1; failure <= 3; failure++ {
		state.recordWarmupError(candidate, http.StatusBadGateway, errors.New("upstream failed"))
		entry := state.warmups[warmupKey("a", "5h")]
		want := 15 * time.Minute * time.Duration(1<<(failure-1))
		if entry.Failures != failure || entry.SuppressUntil.Sub(entry.OutcomeAt) != want || entry.Blocked != (failure == 3) {
			t.Fatalf("failure %d: %#v", failure, entry)
		}
		status := state.warmupTrafficStatusLocked(cfg, entry.OutcomeAt, "a")
		if failure < 3 && status.HoldReason != "failure_backoff" {
			t.Fatalf("pool did not back off: %#v", status)
		}
	}
	status := state.warmupTrafficStatusLocked(cfg, time.Now(), "a")
	if status.HoldReason != "manual_retry_required" {
		t.Fatalf("retry cap did not stop pool: %#v", status)
	}
	before := status.AttemptsLast24h
	if state.clearBlockedWarmupState("a", false) != 1 {
		t.Fatal("explicit recovery did not clear block")
	}
	if after := state.warmupTrafficStatusLocked(cfg, time.Now(), "a"); after.AttemptsLast24h != before || after.HoldReason != "" {
		t.Fatalf("recovery refunded traffic budget: %#v", after)
	}
}

func TestWarmupUncertainOutcomeSuppressesAllAccountWindowsAndTakeover(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errWarmupStreamIncomplete} {
		state := schedulerRuntimeState{cfg: cfg, warmups: make(map[string]warmupEntry)}
		candidate := warmupCandidate{Snapshot: readyWarmupQuota("a", time.Now()), Window: quotaWindow{Class: "5h"}}
		state.recordWarmupError(candidate, 0, err)
		entry := state.warmups["a|5h"]
		if entry.SuppressUntil.Sub(entry.OutcomeAt) < 5*time.Hour {
			t.Fatalf("uncertain outcome retried too soon: %#v", entry)
		}
		candidate.Window.Class = "weekly"
		if _, _, ok := state.nextWarmupCandidateForGenerationLocked([]warmupCandidate{candidate}, entry.OutcomeAt.Add(time.Hour), cfg.WarmupRetryAfter, entry.OutcomeAt.Add(time.Minute)); ok {
			t.Fatal("window or generation change bypassed uncertain request hold")
		}
	}
}

func TestWarmupEligibilityRejectsUnsafeOrUnnecessaryRequests(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	for _, scenario := range []string{"healthy", "weekly_exhausted", "weekly_reserve", "weekly_disallowed", "active", "poll_failed", "stale", "binding_changed", "used_since_admission"} {
		t.Run(scenario, func(t *testing.T) {
			q := readyWarmupQuota("a", now)
			candidate := warmupCandidate{Snapshot: q, Window: q.Windows[0]}
			state := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: make(map[string]quotaSnapshot), quotaPolls: make(map[string]quotaPollState)}
			switch scenario {
			case "weekly_exhausted":
				q.Windows[1].UsedPercent = 100
			case "weekly_reserve":
				q.Windows[1].UsedPercent = 95
			case "weekly_disallowed":
				q.Windows[1].Allowed = false
			case "active":
				state.serialActiveAuthID = "a"
			case "poll_failed":
				state.quotaPolls["a"] = quotaPollState{Error: "quota_http_403"}
			case "stale":
				q.Windows[1].ObservedAt = now.Add(-warmupQuotaMaxAge - time.Second)
			case "binding_changed":
				q.AuthIndex = "replacement-index"
			case "used_since_admission":
				q.Windows[0].UsedPercent = 1
			}
			state.quotas["a"] = q
			if got := state.warmupCandidateStillEligible(candidate, now); got != (scenario == "healthy") {
				t.Fatalf("dispatch allowed=%v for %s", got, scenario)
			}
		})
	}
}

func TestWarmupPolicyFailureJournalStopsAnotherAccount(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	path := filepath.Join(t.TempDir(), "state.json")
	entry := warmupEntry{AuthID: "a", AuthIndex: "idx-a", Window: "5h", AttemptedAt: now.Add(-time.Minute), OutcomeAt: now, Blocked: true, Error: "cyber_policy"}
	if err := appendWarmupOutcomeJournal(path, warmupOutcomeJournalRecord{Version: warmupOutcomeJournalVersion, Key: "a|5h", Entry: entry, RecordedAt: now}); err != nil {
		t.Fatal(err)
	}
	state := schedulerRuntimeState{}
	if _, merged, err := state.mergePersistedWarmupsLocked(path); err != nil || !merged {
		t.Fatalf("merge=%v, err=%v", merged, err)
	}
	status := state.warmupTrafficStatusLocked(defaultPluginConfig(), now.Add(time.Hour), "a")
	if status.HoldReason != "manual_retry_required" || status.AttemptsLast24h != 1 {
		t.Fatalf("retired failure lost: %#v", status)
	}
	if state.pruneExpiredWarmups(now.Add(8 * 24 * time.Hour)) {
		t.Fatal("pruning removed manual policy block")
	}
}

func TestWarmupHonorsRetryAfterAndSSEQuotaFailure(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{cfg: cfg}
	now := time.Now()
	headers := http.Header{"Retry-After": []string{"7200"}}
	candidate := warmupCandidate{Snapshot: readyWarmupQuota("a", now), Window: quotaWindow{Class: "5h"}, RetryAt: warmupRetryDeadline(headers, now)}
	state.recordWarmupStreamError(candidate, cfg, http.StatusOK, headers, now, "test", &warmupStreamTerminalError{Event: "response.failed", Code: "usage_limit_reached"})
	if _, exists := banStore.lookup("a"); !exists {
		t.Fatal("SSE quota failure was not quarantined")
	}
	entry := state.warmups["a|5h"]
	if entry.Status != 429 || entry.SuppressUntil.Before(now.Add(2*time.Hour)) {
		t.Fatalf("quota Retry-After lost: %#v", entry)
	}
	banStore.clear("a")
	if status := state.warmupTrafficStatusLocked(cfg, now.Add(time.Hour), "a"); status.HoldReason != "failure_backoff" {
		t.Fatalf("clearing quarantine bypassed warmup backoff: %#v", status)
	}
}

func TestScheduleWarmupConcurrentAndReloadedCallsSendOnce(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	root := t.TempDir()
	key := filepath.Join(root, "key")
	if err := os.WriteFile(key, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls, inventoryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing management authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/management/auth-files":
			inventoryCalls.Add(1)
			files := []cpaAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Status: "active", Note: "imported account"}, {ID: "b", AuthIndex: "idx-b", Provider: "codex", Status: "active", Note: "imported account"}}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
		case "/v0/management/api-call":
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: 200, Body: "{\"status\":\"completed\"}"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cfg := defaultPluginConfig()
	cfg.StatePath = filepath.Join(root, "state.json")
	cfg.WarmupEnabled = true
	cfg.CPAManagementURL, cfg.CPAManagementKeyFile = server.URL+"/v0/management/api-call", key
	newState := func() *schedulerRuntimeState {
		s := newManagedRuntimeForTest(t, cfg.StatePath)
		s.cfg = cfg
		s.identities = map[string]string{"idx-a": "a", "idx-b": "b"}
		s.quotas = map[string]quotaSnapshot{"a": readyWarmupQuota("a", time.Now()), "b": readyWarmupQuota("b", time.Now())}
		claimManagedRuntimeForTest(t, s)
		return s
	}
	state := newState()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); state.scheduleWarmup(context.Background(), nil) }()
	}
	wg.Wait()
	state.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent admissions sent %d requests", calls.Load())
	}
	before := inventoryCalls.Load()
	state.scheduleWarmup(context.Background(), nil)
	reloaded := newState()
	reloaded.scheduleWarmup(context.Background(), nil)
	reloaded.wg.Wait()
	if calls.Load() != 1 || inventoryCalls.Load() != before {
		t.Fatalf("interval/reload created traffic: model=%d inventory=%d (before=%d)", calls.Load(), inventoryCalls.Load(), before)
	}
	if status := reloaded.status(); status.WarmupTraffic.AttemptsLast24h != 1 || status.WarmupTraffic.HoldReason != "min_interval" {
		t.Fatalf("status lost traffic hold: %#v", status.WarmupTraffic)
	}
}

func TestWarmupBudgetConfigValidation(t *testing.T) {
	for _, raw := range []string{"warmup_min_interval: garbage", "warmup_min_interval: 0s", "warmup_min_interval: 25h", "warmup_max_per_day: 0", "warmup_max_per_day: 1001"} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid warmup policy: %s", raw)
		}
	}
	cfg, err := parsePluginConfig([]byte("warmup_min_interval: 20m\nwarmup_max_per_day: 3"))
	if err != nil || cfg.WarmupMinInterval != 20*time.Minute || cfg.WarmupMaxPerDay != 3 {
		t.Fatalf("valid warmup policy: %#v err=%v", cfg, err)
	}
}

func TestScheduleWarmupDoesNotSendWhenAdmissionCannotPersist(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	root := t.TempDir()
	cfg := defaultPluginConfig()
	cfg.StatePath, cfg.CPAManagementKeyFile = filepath.Join(root, "state.json"), filepath.Join(root, "key")
	cfg.WarmupEnabled = true
	if err := os.WriteFile(cfg.CPAManagementKeyFile, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			// Failure occurs after lease/state merge but before admission commit.
			if err := os.Remove(cfg.StatePath); err != nil && !os.IsNotExist(err) {
				t.Error(err)
			}
			if err := os.Mkdir(cfg.StatePath, 0700); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []cpaAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Provider: "codex", Status: "active", Note: "imported account"}}})
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	state := newManagedRuntimeForTest(t, cfg.StatePath)
	state.cfg = cfg
	state.quotas = map[string]quotaSnapshot{"a": readyWarmupQuota("a", time.Now())}
	claimManagedRuntimeForTest(t, state)
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	if calls.Load() != 0 || state.warmupRunning {
		t.Fatalf("failed admission still sent traffic: calls=%d running=%v", calls.Load(), state.warmupRunning)
	}
}

func TestWarmupTakeoverTransfersSiblingWindowsTogether(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	now := time.Now()
	state := schedulerRuntimeState{cfg: cfg}
	q := readyWarmupQuota("a", now)
	q.Windows[1].UsedPercent, q.Windows[1].ResetAt = 0, time.Time{}
	candidate := warmupCandidate{Snapshot: q, Window: q.Windows[0]}
	state.recordWarmupOutcome(candidate, http.StatusOK, nil, nil)
	path := filepath.Join(t.TempDir(), "state.json")
	lease, acquired, err := acquireWarmupInstanceLease(path, now)
	if err != nil || !acquired {
		t.Fatalf("lease acquired=%v err=%v", acquired, err)
	}
	defer lease.release()
	if err := state.persistWarmupLeaseOutcome(lease, candidate); err != nil {
		t.Fatal(err)
	}
	loaded := schedulerRuntimeState{}
	if _, _, err := loaded.mergePersistedWarmupsLocked(path); err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"5h", "weekly"} {
		entry := loaded.warmups[warmupKey("a", class)]
		if entry.CompletedAt.IsZero() || !entry.SuppressUntil.After(now) {
			t.Fatalf("lost %s outcome after takeover: %#v", class, entry)
		}
	}
	if len(loaded.warmupAttempts) != 1 {
		t.Fatalf("siblings counted as %d requests", len(loaded.warmupAttempts))
	}
}

func TestWarmupConfirmationRequiresNewObservationAndMatchingIdentity(t *testing.T) {
	now := time.Now()
	for _, scenario := range []string{"carried_window", "changed_identity", "fresh"} {
		entry := warmupEntry{AuthID: "a", AuthIndex: "idx-a", Window: "5h", AttemptedAt: now.Add(-time.Minute), CompletedAt: now.Add(-time.Minute)}
		state := schedulerRuntimeState{warmups: map[string]warmupEntry{"a|5h": entry}}
		q := readyWarmupQuota("a", now)
		q.Windows[0].ResetAt = now.Add(time.Hour)
		if scenario == "carried_window" {
			q.Windows[0].ObservedAt = now.Add(-2 * time.Minute)
		}
		if scenario == "changed_identity" {
			q.AuthIndex = "replacement"
		}
		if confirmed := state.confirmPendingWarmups(map[string]quotaSnapshot{"a": q}, now); confirmed != (scenario == "fresh") {
			t.Fatalf("confirmation=%v for %s", confirmed, scenario)
		}
	}
}

func TestAccountWarmupBudgetAndFailureCannotBlockSibling(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.WarmupMaxPerDay = 2
	s := schedulerRuntimeState{cfg: cfg, warmups: map[string]warmupEntry{"a|5h": {AuthID: "a", Blocked: true, Error: "http_401", AttemptedAt: now.Add(-time.Hour)}}}
	s.warmupAttempts = []warmupAttempt{{AuthID: "a", At: now.Add(-2 * time.Hour)}, {AuthID: "a", At: now.Add(-time.Hour)}}
	if s.warmupTrafficStatusLocked(cfg, now, "a").HoldReason != "manual_retry_required" {
		t.Fatal("account hold lost")
	}
	if s.warmupTrafficStatusLocked(cfg, now).HoldReason != "" || s.warmupTrafficStatusLocked(cfg, now, "b").HoldReason != "" {
		t.Fatal("sibling blocked")
	}
	if s.warmupTrafficStatusLocked(cfg, now, "b").AttemptsLast24h != 0 {
		t.Fatal("budget shared")
	}
	s.warmupAttempts = append(s.warmupAttempts, warmupAttempt{AuthID: "b", At: now})
	if s.warmupTrafficStatusLocked(cfg, now).HoldReason != "min_interval" {
		t.Fatal("global spacing lost")
	}
}

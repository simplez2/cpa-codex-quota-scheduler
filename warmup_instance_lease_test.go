package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWarmupInstanceLeaseSerializesIndependentRuntimes(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	first, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil || !acquired {
		t.Fatalf("first lease acquired=%v err=%v", acquired, err)
	}
	second, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if acquired || second != nil {
		t.Fatal("second runtime acquired an already held warmup lease")
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	third, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil || !acquired {
		t.Fatalf("lease after release acquired=%v err=%v", acquired, err)
	}
	if err := third.release(); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupInstanceLeaseIgnoresStaleMetadataAndReleaseIsOwnerScoped(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	lockPath := statePath + ".warmup.lock"
	stale, err := json.Marshal(warmupInstanceLeaseRecord{
		Owner:     "stale-owner",
		ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, stale, 0600); err != nil {
		t.Fatal(err)
	}
	first, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil || !acquired {
		t.Fatalf("stale metadata recovery acquired=%v err=%v", acquired, err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	second, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil || !acquired {
		t.Fatalf("second owner acquired=%v err=%v", acquired, err)
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	third, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if acquired || third != nil {
		t.Fatal("an old owner's repeated release disturbed the current lease")
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
}

func TestWarmupOutcomeJournalRecoversPastPartialTail(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	journalPath := warmupOutcomeJournalPath(statePath)
	if err := os.WriteFile(journalPath, []byte(`{"version":1,"key":"partial"`), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record := warmupOutcomeJournalRecord{
		Version: warmupOutcomeJournalVersion,
		Key:     warmupKey("acct", "5h"),
		Entry: warmupEntry{
			AuthID: "acct", Window: "5h", AttemptedAt: now, CompletedAt: now, Status: http.StatusOK,
		},
		RecordedAt: now,
	}
	if err := appendWarmupOutcomeJournal(statePath, record); err != nil {
		t.Fatal(err)
	}
	records, err := readWarmupOutcomeJournal(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != record.Key || records[0].Entry.Status != http.StatusOK {
		t.Fatalf("journal partial-tail recovery = %#v", records)
	}
}

func TestWarmupOutcomeJournalCarriesRecoveryBanClear(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	lease, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
	if err != nil || !acquired {
		t.Fatalf("acquire warmup lease: acquired=%v err=%v", acquired, err)
	}
	defer lease.release()

	now := time.Now()
	bannedAt := now.Add(-6 * time.Hour)
	probeStartedAt := now.Add(-time.Second)
	candidate := warmupCandidate{
		Snapshot:         quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct"},
		Window:           quotaWindow{Class: "5h"},
		RecoveryProbe:    true,
		RecoveryBannedAt: bannedAt,
		ProbeStartedAt:   probeStartedAt,
	}
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		warmupKey("acct", "5h"): {
			AuthID: "acct", AuthIndex: "idx-acct", Window: "5h",
			AttemptedAt: now.Add(-time.Second), CompletedAt: now,
			Status: http.StatusOK,
		},
	}}
	if err := state.persistWarmupLeaseOutcome(lease, candidate); err != nil {
		t.Fatal(err)
	}
	records, err := readWarmupOutcomeJournal(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !records[0].BanCleared || records[0].Ban != nil {
		t.Fatalf("recovery clear journal = %#v", records)
	}

	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
	})
	active := schedulerRuntimeState{warmups: make(map[string]warmupEntry)}
	active.warmupMu.Lock()
	mutations, merged, err := active.mergePersistedWarmupsLocked(statePath)
	active.warmupMu.Unlock()
	if err != nil || !merged {
		t.Fatalf("merge recovery clear: merged=%v err=%v", merged, err)
	}
	active.applyWarmupJournalBanMutations(mutations)
	if entry, ok := banStore.lookup("acct"); ok {
		t.Fatalf("active generation retained cleared recovery ban: %#v", entry)
	}

	newer := banEntry{
		ResetAt: now.Add(time.Hour), BannedAt: now.Add(time.Minute),
		Window: "weekly", Kind: banKindQuota, Phase: banPhaseCooldown,
	}
	banStore.set("acct", newer)
	active.applyWarmupJournalBanMutations(mutations)
	if entry, ok := banStore.lookup("acct"); !ok || !entry.BannedAt.Equal(newer.BannedAt) {
		t.Fatalf("old recovery clear removed newer ban: %#v, ok=%v", entry, ok)
	}
}

func TestManagementRetryCompactsOldBlockedJournalBeforeClearing(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, state)
	t.Cleanup(func() { state.stop() })
	now := time.Now().UTC().Truncate(time.Millisecond)
	entry := warmupEntry{
		AuthID: "acct", AuthIndex: "idx-acct", Window: "5h",
		AttemptedAt: now.Add(-time.Minute), CompletedAt: now.Add(-30 * time.Second),
		Status: http.StatusServiceUnavailable, Error: "cyber_policy", Blocked: true,
	}
	if err := appendWarmupOutcomeJournal(statePath, warmupOutcomeJournalRecord{
		Version: warmupOutcomeJournalVersion,
		Key:     warmupKey("acct", "5h"), Entry: entry,
		Ban:        &banEntry{BannedAt: now.Add(-time.Minute), Window: "recovery blocked: cyber_policy", Kind: banKindBlocked},
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	removed, err := state.clearBlockedWarmupStateSafe("acct", false)
	if err != nil || removed != 2 {
		t.Fatalf("management retry removed=%d err=%v; want warmup and terminal ban", removed, err)
	}
	state.warmupMu.Lock()
	_, warmupExists := state.warmups[warmupKey("acct", "5h")]
	state.warmupMu.Unlock()
	if warmupExists {
		t.Fatal("explicit retry left the old blocked warmup journal entry active")
	}
	if _, ok := banStore.lookup("acct"); ok {
		t.Fatal("explicit retry left the terminal ban active")
	}
	if _, err := os.Stat(warmupOutcomeJournalPath(statePath)); !os.IsNotExist(err) {
		t.Fatalf("old blocked journal was not compacted: %v", err)
	}

	// A new generation must not be able to replay the removed blocked outcome.
	resetBanStoreForTest()
	reader := &schedulerRuntimeState{}
	reader.loadBanState(statePath)
	reader.warmupMu.Lock()
	_, warmupExists = reader.warmups[warmupKey("acct", "5h")]
	reader.warmupMu.Unlock()
	if warmupExists {
		t.Fatal("blocked warmup resurrected after management retry and reload")
	}
	if _, ok := banStore.lookup("acct"); ok {
		t.Fatal("terminal ban resurrected after management retry and reload")
	}
}

func TestWarmupOutcomeJournalCarriesRecoveryCooldownWithExactCAS(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		errorCode   string
		replacement func(time.Time, time.Time) banEntry
	}{
		{
			name:      "authoritative_429",
			status:    statusTooManyRequests,
			errorCode: "http_429",
			replacement: func(bannedAt, now time.Time) banEntry {
				return banEntry{
					ResetAt: now.Add(2 * time.Hour), BannedAt: bannedAt.Add(time.Hour),
					Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
				}
			},
		},
		{
			name:      "short_5xx_cooldown",
			status:    http.StatusServiceUnavailable,
			errorCode: "http_503",
			replacement: func(bannedAt, now time.Time) banEntry {
				return banEntry{
					ResetAt: now.Add(2 * time.Minute), BannedAt: bannedAt,
					Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			statePath := filepath.Join(t.TempDir(), "state.json")
			lease, acquired, err := acquireWarmupInstanceLease(statePath, time.Now())
			if err != nil || !acquired {
				t.Fatalf("acquire warmup lease: acquired=%v err=%v", acquired, err)
			}
			defer lease.release()

			now := time.Now().UTC().Truncate(time.Millisecond)
			bannedAt := now.Add(-6 * time.Hour)
			probeStartedAt := now.Add(-time.Second)
			candidate := warmupCandidate{
				Snapshot:      quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct"},
				Window:        quotaWindow{Class: "5h"},
				RecoveryProbe: true, RecoveryBannedAt: bannedAt, ProbeStartedAt: probeStartedAt,
			}
			state := schedulerRuntimeState{warmups: map[string]warmupEntry{
				warmupKey("acct", "5h"): {
					AuthID: "acct", AuthIndex: "idx-acct", Window: "5h",
					AttemptedAt: now.Add(-time.Second), Status: test.status, Error: test.errorCode,
				},
			}}
			replacement := test.replacement(bannedAt, now)
			banStore.set("acct", replacement)
			if err := state.persistWarmupLeaseOutcome(lease, candidate); err != nil {
				t.Fatal(err)
			}
			records, err := readWarmupOutcomeJournal(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || records[0].Ban == nil || records[0].BanCleared ||
				!records[0].RecoveryBannedAt.Equal(bannedAt) || !records[0].RecoveryProbeStart.Equal(probeStartedAt) {
				t.Fatalf("recovery cooldown journal = %#v", records)
			}

			resetBanStoreForTest()
			banStore.set("acct", banEntry{
				ResetAt: probeStartedAt.Add(-time.Second), BannedAt: bannedAt,
				Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
			})
			active := schedulerRuntimeState{warmups: make(map[string]warmupEntry)}
			active.warmupMu.Lock()
			mutations, merged, err := active.mergePersistedWarmupsLocked(statePath)
			active.warmupMu.Unlock()
			if err != nil || !merged {
				t.Fatalf("merge recovery cooldown: merged=%v err=%v", merged, err)
			}
			active.applyWarmupJournalBanMutations(mutations)
			got, ok := banStore.lookup("acct")
			if !ok || !got.BannedAt.Equal(replacement.BannedAt) || !got.ResetAt.Equal(replacement.ResetAt) || got.Phase != banPhaseCooldown {
				t.Fatalf("matching recovery cooldown was not applied: got=%#v ok=%v want=%#v", got, ok, replacement)
			}

			resetBanStoreForTest()
			different := banEntry{
				ResetAt: now.Add(4 * time.Hour), BannedAt: now.Add(time.Minute),
				Window: "weekly", Kind: banKindQuota, Phase: banPhaseHalfOpen,
				ProbeStartedAt: now, ProbeLeaseUntil: now.Add(time.Minute),
			}
			banStore.set("acct", different)
			active.applyWarmupJournalBanMutations(mutations)
			if got, ok := banStore.lookup("acct"); !ok || !got.BannedAt.Equal(different.BannedAt) {
				t.Fatalf("old recovery cooldown replaced a different ban: got=%#v ok=%v", got, ok)
			}

			resetBanStoreForTest()
			sameCooldown := banEntry{
				ResetAt: now.Add(5 * time.Hour), BannedAt: bannedAt,
				Window: "weekly", Kind: banKindQuota, Phase: banPhaseCooldown,
			}
			banStore.set("acct", sameCooldown)
			active.applyWarmupJournalBanMutations(mutations)
			if got, ok := banStore.lookup("acct"); !ok || !got.ResetAt.Equal(sameCooldown.ResetAt) || got.Window != sameCooldown.Window {
				t.Fatalf("old recovery cooldown replaced a terminal cooldown: got=%#v ok=%v", got, ok)
			}

			resetBanStoreForTest()
			active.applyWarmupJournalBanMutations(mutations)
			if got, ok := banStore.lookup("acct"); ok {
				t.Fatalf("old recovery cooldown resurrected an absent ban: %#v", got)
			}
		})
	}
}

func TestWarmupOutcomeJournalRejectsCompletelyCorruptContent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	journalPath := warmupOutcomeJournalPath(statePath)
	corrupt := `not-json
still-not-json
`
	if err := os.WriteFile(journalPath, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := readWarmupOutcomeJournal(statePath)
	if err == nil || !strings.Contains(err.Error(), "no valid record") {
		t.Fatalf("corrupt journal records=%#v err=%v", records, err)
	}
}

func TestWarmupOutcomeJournalEnforcesPostAppendSizeLimit(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	journalPath := warmupOutcomeJournalPath(statePath)
	if err := os.WriteFile(journalPath, []byte(strings.Repeat("x", warmupOutcomeJournalMaxBytes)), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err := appendWarmupOutcomeJournal(statePath, warmupOutcomeJournalRecord{
		Version: warmupOutcomeJournalVersion,
		Key:     warmupKey("acct", "5h"),
		Entry:   warmupEntry{AuthID: "acct", Window: "5h", AttemptedAt: now},
	})
	if err == nil {
		t.Fatal("journal append exceeded the configured size limit")
	}
	stat, statErr := os.Stat(journalPath)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if stat.Size() != warmupOutcomeJournalMaxBytes {
		t.Fatalf("failed oversized append changed journal size: %d", stat.Size())
	}
}

func TestManagementWarmupTakeoverMergesRetiredOutcomeWithoutDuplicate(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[{"id":"acct","auth_index":"idx-acct","provider":"codex","status":"active","note":"Agent Identity via sidecar"}]}`))
		case "/v0/management/api-call":
			var request cpaAPICallRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode api-call: %v", err)
			}
			if request.AuthIndex != "idx-acct" {
				t.Errorf("auth_index = %q", request.AuthIndex)
			}
			var upstream map[string]any
			if err := json.Unmarshal([]byte(request.Data), &upstream); err != nil {
				t.Errorf("decode upstream request: %v", err)
			} else {
				input, _ := upstream["input"].([]any)
				_, hasTopLevelTools := upstream["tools"]
				additionalTools, _ := input[0].(map[string]any)
				if _, hasMaxOutputTokens := upstream["max_output_tokens"]; hasMaxOutputTokens ||
					upstream["stream"] != true || len(input) != 3 || hasTopLevelTools ||
					additionalTools["type"] != "additional_tools" {
					t.Errorf("unexpected Codex warmup request: %#v", upstream)
				}
			}
			if apiCalls.Add(1) == 1 {
				close(requestStarted)
			}
			<-releaseRequest
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{
				StatusCode: http.StatusOK,
				Body:       `{"id":"resp_1","status":"completed","output":[]}`,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := defaultPluginConfig()
	cfg.WarmupEnabled = true
	cfg.WarmupExecutionMode = "management"
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.WarmupSidecarURL = server.URL + "/backend-api/codex"
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	newState := func() *schedulerRuntimeState {
		state := newManagedRuntimeForTest(t, cfg.StatePath)
		state.cfg = cfg
		state.quotas = map[string]quotaSnapshot{
			"acct": {
				AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now, WindowUsageCreditsKnown: true},
					{Class: "weekly", UsedPercent: 0, Allowed: true, ObservedAt: now, WindowUsageCreditsKnown: true},
				},
			},
		}
		return state
	}
	retired := newState()
	claimManagedRuntimeForTest(t, retired)
	retired.scheduleWarmup(context.Background(), nil)
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("retired owner management warmup did not start")
	}

	active := newState()
	claimManagedRuntimeForTest(t, active)
	close(releaseRequest)
	retired.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("api-call count after completion = %d", got)
	}
	if !retired.generationStatus().Superseded {
		t.Fatalf("retired owner did not observe generation takeover: %#v", retired.generationStatus())
	}
	records, err := readWarmupOutcomeJournal(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("retired request outcome journal count = %d; want 1", len(records))
	}
	weeklyJournal := records[0].Entries[warmupKey("acct", "weekly")]
	if records[0].Entry.Status != http.StatusOK || records[0].Entry.CompletedAt.IsZero() ||
		weeklyJournal.Status != http.StatusOK || weeklyJournal.CompletedAt.IsZero() {
		t.Fatalf("retired request outcome journal = %#v", records)
	}

	// The new owner acquires the same cross-DSO lease, merges the narrow
	// retired outcome, persists it under its generation fence, and suppresses
	// a duplicate activation.
	active.scheduleWarmup(context.Background(), nil)
	active.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("api-call count after takeover = %d; retired outcome was not merged", got)
	}
	active.warmupMu.Lock()
	merged := active.warmups[warmupKey("acct", "5h")]
	mergedWeekly := active.warmups[warmupKey("acct", "weekly")]
	active.warmupMu.Unlock()
	if merged.Status != http.StatusOK || merged.CompletedAt.IsZero() ||
		mergedWeekly.Status != http.StatusOK || mergedWeekly.CompletedAt.IsZero() {
		t.Fatalf("active runtime did not merge completed warmup state: 5h=%#v weekly=%#v", merged, mergedWeekly)
	}
	if _, err := os.Stat(warmupOutcomeJournalPath(cfg.StatePath)); !os.IsNotExist(err) {
		t.Fatalf("merged outcome journal was not compacted: %v", err)
	}
}

func TestManagementRecoveryWarmupTakeoverAppliesCASClearBeforeCandidateSelection(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	keyPath := filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(keyPath, []byte("test-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var apiCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/management/auth-files":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[{"id":"acct","auth_index":"idx-acct","provider":"codex","status":"active","note":"Agent Identity via sidecar"}]}`))
		case "/v0/management/api-call":
			if apiCalls.Add(1) == 1 {
				close(requestStarted)
			}
			<-releaseRequest
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cpaAPICallResponse{
				StatusCode: http.StatusOK,
				Body:       `{"id":"resp_recovery","status":"completed","output":[]}`,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := defaultPluginConfig()
	cfg.WarmupEnabled = true
	cfg.WarmupExecutionMode = "management"
	cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = keyPath
	cfg.WarmupSidecarURL = server.URL + "/backend-api/codex"
	cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC().Truncate(time.Millisecond)
	newState := func() *schedulerRuntimeState {
		state := newManagedRuntimeForTest(t, cfg.StatePath)
		state.cfg = cfg
		state.quotas = map[string]quotaSnapshot{
			"acct": {
				AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", UsedPercentKnown: true, Allowed: true, AllowedKnown: true, LimitReachedKnown: true, ObservedAt: now, WindowUsageCreditsKnown: true},
					{Class: "weekly", UsedPercentKnown: true, Allowed: true, AllowedKnown: true, LimitReachedKnown: true, ObservedAt: now, WindowUsageCreditsKnown: true},
				},
			},
		}
		return state
	}
	bannedAt := now.Add(-6 * time.Hour)
	banStore.set("acct", banEntry{
		ResetAt: now.Add(-time.Minute), BannedAt: bannedAt,
		Window: "5h", Kind: banKindQuota, Phase: banPhaseCooldown,
	})

	retired := newState()
	claimManagedRuntimeForTest(t, retired)
	retired.scheduleWarmup(context.Background(), nil)
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery warmup did not start")
	}
	halfOpen, ok := banStore.lookup("acct")
	if !ok || halfOpen.Phase != banPhaseHalfOpen || halfOpen.ProbeStartedAt.IsZero() {
		t.Fatalf("recovery request did not own a half-open lease: entry=%#v ok=%v", halfOpen, ok)
	}

	active := newState()
	claimManagedRuntimeForTest(t, active)
	close(releaseRequest)
	retired.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("retired recovery api-call count = %d; want 1", got)
	}
	records, err := readWarmupOutcomeJournal(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("recovery takeover journal count = %d; want 1", len(records))
	}
	record := records[0]
	if !record.BanCleared || record.Ban != nil ||
		!record.RecoveryBannedAt.Equal(halfOpen.BannedAt) || !record.RecoveryProbeStart.Equal(halfOpen.ProbeStartedAt) {
		t.Fatalf("recovery takeover lost CAS identity: %#v", record)
	}
	for _, class := range []string{"5h", "weekly"} {
		entry := record.Entries[warmupKey("acct", class)]
		if entry.Status != http.StatusOK || entry.CompletedAt.IsZero() {
			t.Fatalf("recovery journal missing %s sibling outcome: %#v", class, record.Entries)
		}
	}

	// Simulate the separately loaded active DSO, which read the last persisted
	// probe-ready cooldown just before the retiring owner saved its half-open
	// reservation. scheduleWarmup must merge the CAS tombstone before candidate
	// discovery, clear that exact stale cooldown, and suppress a second
	// activation using both sibling outcomes.
	resetBanStoreForTest()
	activeLoadedCooldown := halfOpen
	activeLoadedCooldown.Phase = banPhaseCooldown
	activeLoadedCooldown.ProbeStartedAt = time.Time{}
	activeLoadedCooldown.ProbeLeaseUntil = time.Time{}
	activeLoadedCooldown.ResetAt = halfOpen.ProbeStartedAt.Add(-time.Second)
	banStore.set("acct", activeLoadedCooldown)
	active.scheduleWarmup(context.Background(), nil)
	active.wg.Wait()
	if got := apiCalls.Load(); got != 1 {
		t.Fatalf("active generation repeated recovery warmup: api-calls=%d", got)
	}
	if entry, ok := banStore.lookup("acct"); ok {
		t.Fatalf("active generation retained journal-cleared half-open ban: %#v", entry)
	}
	active.warmupMu.Lock()
	merged5h := active.warmups[warmupKey("acct", "5h")]
	mergedWeekly := active.warmups[warmupKey("acct", "weekly")]
	active.warmupMu.Unlock()
	if merged5h.Status != http.StatusOK || merged5h.CompletedAt.IsZero() ||
		mergedWeekly.Status != http.StatusOK || mergedWeekly.CompletedAt.IsZero() {
		t.Fatalf("active generation did not merge recovery siblings: 5h=%#v weekly=%#v", merged5h, mergedWeekly)
	}
	if _, err := os.Stat(warmupOutcomeJournalPath(cfg.StatePath)); !os.IsNotExist(err) {
		t.Fatalf("recovery outcome journal was not compacted: %v", err)
	}
}

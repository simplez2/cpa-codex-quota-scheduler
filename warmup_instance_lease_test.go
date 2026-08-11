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
			} else if got := upstream["max_output_tokens"]; got != float64(16) {
				t.Errorf("max_output_tokens = %#v; want 16", got)
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
				Windows: []quotaWindow{{Class: "5h", UsedPercent: 0, Allowed: true, ObservedAt: now}},
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
	if len(records) != 1 || records[0].Entry.Status != http.StatusOK || records[0].Entry.CompletedAt.IsZero() {
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
	active.warmupMu.Unlock()
	if merged.Status != http.StatusOK || merged.CompletedAt.IsZero() {
		t.Fatalf("active runtime did not merge completed warmup state: %#v", merged)
	}
	if _, err := os.Stat(warmupOutcomeJournalPath(cfg.StatePath)); !os.IsNotExist(err) {
		t.Fatalf("merged outcome journal was not compacted: %v", err)
	}
}

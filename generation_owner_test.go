package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func newManagedRuntimeForTest(t *testing.T, statePath string) *schedulerRuntimeState {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.StatePath = statePath
	state := &schedulerRuntimeState{
		cfg:                   cfg,
		quotas:                make(map[string]quotaSnapshot),
		identities:            make(map[string]string),
		pricing:               make(map[string]modelPricing),
		costSamples:           make(map[string][]float64),
		pacingAccounts:        make(map[string]*accountPacingState),
		stickyBindings:        make(map[string]stickyBinding),
		warmups:               make(map[string]warmupEntry),
		warmupLeases:          make(map[string]warmupLease),
		banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.initializeGenerationOwnership(statePath)
	if err := state.reserveGenerationOwnership(statePath); err != nil {
		t.Fatalf("reserve generation: %v", err)
	}
	return state
}

func claimManagedRuntimeForTest(t *testing.T, state *schedulerRuntimeState) {
	t.Helper()
	claimed, active, err := state.claimGenerationAfterSuccessfulRefresh()
	if err != nil || !claimed || !active {
		t.Fatalf("claim generation claimed=%v active=%v err=%v", claimed, active, err)
	}
	// Most tests exercise steady-state behavior. Backdate only the in-memory
	// timestamp so the production startup grace does not add wall-clock waits.
	state.generationMu.Lock()
	state.generation.ClaimedAt = time.Now().UTC().Add(-warmupStartupGrace - time.Second)
	state.generationMu.Unlock()
}

func TestGenerationClaimsOnlyAfterSuccessfulRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var ready atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			writeTestJSON(t, w, map[string]any{"session_token": "token"})
		case "/api/v1/usage/identities":
			if !ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writeTestJSON(t, w, map[string]any{"identities": []map[string]any{{
				"identity": "idx", "file_name": "acct", "provider": providerCodex,
			}}})
		case "/api/v1/quota/cache":
			used, allowed, reached, seconds := 0.0, true, false, int64(5*60*60)
			writeTestJSON(t, w, keeperCacheResponse{Items: []keeperCacheItem{{
				AuthIndex: "idx", FileName: "acct", Status: "completed",
				RefreshedAt: json.RawMessage(fmt.Sprintf("%q", now.Format(time.RFC3339))),
				Quota: &keeperCheckResponse{Quota: []keeperQuotaRow{{
					Label: "5h", UsedPercent: &used, Allowed: &allowed, LimitReached: &reached,
					Window:  &keeperQuotaWindow{Seconds: &seconds},
					ResetAt: json.RawMessage(fmt.Sprintf("%d", now.Add(5*time.Hour).Unix())),
				}}},
			}}})
		case "/api/v1/pricing":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	state := newRefreshTestState(t, server.URL)
	state.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	state.initializeGenerationOwnership(state.cfg.StatePath)
	if err := state.reserveGenerationOwnership(state.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	state.refreshOnce(context.Background())
	if status := state.generationStatus(); status.Claimed || status.Active {
		t.Fatalf("failed refresh claimed generation: %#v", status)
	}
	record, err := readSchedulerGenerationRecord(state.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if record.Active || record.ActiveGeneration != 0 {
		t.Fatalf("failed refresh replaced active owner: %#v", record)
	}

	ready.Store(true)
	state.refreshOnce(context.Background())
	status := state.generationStatus()
	if !status.Claimed || !status.Active || state.refreshes != 1 {
		t.Fatalf("successful refresh did not claim generation: status=%#v refreshes=%d error=%q", status, state.refreshes, state.lastError)
	}
}

func TestGenerationJournalCompactsLegacyOversize(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	record := schedulerGenerationRecord{
		Version:          schedulerGenerationRecordVersion,
		HighestReserved:  7,
		ActiveGeneration: 7,
		ActiveOwner:      "owner",
		Active:           true,
		ClaimedAt:        time.Now().UTC().Truncate(time.Second),
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	line = append(line, '\n')
	repeat := generationRecordMaxBytes/len(line) + 2
	raw := bytes.Repeat(line, repeat)
	if len(raw) <= generationRecordMaxBytes || len(raw) > generationRecordRecoveryMaxBytes {
		t.Fatalf("invalid legacy journal fixture size %d", len(raw))
	}
	path := schedulerGenerationPath(statePath)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := readSchedulerGenerationRecord(statePath)
	if err != nil {
		t.Fatalf("recover legacy oversized journal: %v", err)
	}
	if got.HighestReserved != record.HighestReserved || got.ActiveGeneration != record.ActiveGeneration || got.ActiveOwner != record.ActiveOwner {
		t.Fatalf("compaction changed generation record: %#v", got)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() >= generationRecordCompactAt {
		t.Fatalf("journal was not compacted: %d bytes", stat.Size())
	}
	if _, err := os.Stat(schedulerGenerationLockPath(statePath)); err != nil {
		t.Fatalf("stable generation lock was not created: %v", err)
	}
}

func TestGenerationJournalAllowsTruncatedFinalAppend(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	record := schedulerGenerationRecord{Version: schedulerGenerationRecordVersion, HighestReserved: 3}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	raw := append(append(line, '\n'), []byte("{\"version\":1,\"highest_reserved\":")...)
	if err := os.WriteFile(schedulerGenerationPath(statePath), raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readSchedulerGenerationRecord(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.HighestReserved != 3 {
		t.Fatalf("truncated append hid last complete record: %#v", got)
	}
	updated, err := updateSchedulerGenerationRecord(statePath, func(record *schedulerGenerationRecord) (bool, error) {
		record.HighestReserved++
		return true, nil
	})
	if err != nil {
		t.Fatalf("write after truncated-tail recovery: %v", err)
	}
	if updated.HighestReserved != 4 {
		t.Fatalf("unexpected generation after recovery write: %#v", updated)
	}
	got, err = readSchedulerGenerationRecord(statePath)
	if err != nil || got.HighestReserved != 4 {
		t.Fatalf("journal was not durably healed: record=%#v err=%v", got, err)
	}
}

func TestGenerationJournalRejectsCompleteCorruptFinalRecordWithoutNewline(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	raw := []byte("{\"version\":1,\"highest_reserved\":3}\n{\"version\":1,broken}")
	if err := os.WriteFile(schedulerGenerationPath(statePath), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchedulerGenerationRecord(statePath); err == nil {
		t.Fatal("complete corrupt final generation record was accepted")
	}
}

func TestGenerationJournalRejectsNonTruncationGarbageAtEOF(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	raw := []byte("{\"version\":1,\"highest_reserved\":3}\nx")
	if err := os.WriteFile(schedulerGenerationPath(statePath), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchedulerGenerationRecord(statePath); err == nil {
		t.Fatal("non-truncation garbage at EOF was accepted")
	}
}

func TestGenerationJournalRejectsUnsupportedVersion(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(schedulerGenerationPath(statePath), []byte("{\"version\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchedulerGenerationRecord(statePath); err == nil {
		t.Fatal("unsupported generation version was accepted")
	}
}

func TestGenerationJournalRejectsNonMonotonicHistory(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	raw := []byte("{\"version\":1,\"highest_reserved\":5,\"active_generation\":4}\n{\"version\":1,\"highest_reserved\":4,\"active_generation\":4}\n")
	if err := os.WriteFile(schedulerGenerationPath(statePath), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSchedulerGenerationRecord(statePath); err == nil {
		t.Fatal("non-monotonic generation history was accepted")
	}
}

func TestNewGenerationPermanentlySupersedesOldOwner(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	newOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, newOwner)

	if oldOwner.generationOwnerActive() {
		t.Fatal("old generation remained active after takeover")
	}
	if status := oldOwner.generationStatus(); !status.Superseded {
		t.Fatalf("old generation was not permanently superseded: %#v", status)
	}
	if err := newOwner.releaseGenerationOwnership(); err != nil {
		t.Fatal(err)
	}
	if oldOwner.generationCanRefresh() || oldOwner.generationOwnerActive() {
		t.Fatal("released newer owner revived a retired generation")
	}
}

func TestGenerationClaimAuthoritativelyAppliesDeletedResetConfirmation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Now().UTC()
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	oldOwner.banResetConfirmations["acct"] = banResetConfirmation{
		AuthID: "acct", BannedAt: now.Add(-time.Hour), BanResetAt: now.Add(24 * time.Hour),
		FirstSnapshotAt: now.Add(-2 * time.Minute), LastSnapshotAt: now.Add(-2 * time.Minute),
		Confirmations: 1, Reason: "weekly_reset_anchor_advanced",
	}
	if !oldOwner.persistBanState() {
		t.Fatal("old owner did not persist first reset confirmation")
	}

	newOwner := newManagedRuntimeForTest(t, statePath)
	newOwner.loadBanState(statePath)
	if _, ok := newOwner.banResetConfirmations["acct"]; !ok {
		t.Fatal("incoming generation did not load the pre-fence confirmation")
	}

	// The retiring owner observes contradictory evidence before takeover and
	// persists the deletion. Claim must replace, rather than merge, this map.
	if !oldOwner.dropBanResetConfirmation("acct") || !oldOwner.persistBanState() {
		t.Fatal("old owner did not persist confirmation deletion")
	}
	claimManagedRuntimeForTest(t, newOwner)
	if _, ok := newOwner.banResetConfirmations["acct"]; ok {
		t.Fatal("incoming generation resurrected a reset confirmation deleted before the fence")
	}

	t.Cleanup(func() {
		oldOwner.stop()
		newOwner.stop()
	})
}

func TestGenerationTombstoneCannotReviveRetiredReservation(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	retired := newManagedRuntimeForTest(t, statePath)
	newOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, newOwner)
	if err := newOwner.releaseGenerationOwnership(); err != nil {
		t.Fatal(err)
	}

	claimed, active, err := retired.claimGenerationAfterSuccessfulRefresh()
	if err != nil {
		t.Fatal(err)
	}
	if claimed || active || !retired.generationStatus().Superseded {
		t.Fatalf("retired reservation revived after tombstone: claimed=%v active=%v status=%#v", claimed, active, retired.generationStatus())
	}
}

func TestGenerationCommitAndClaimShareOneFence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	markerPath := filepath.Join(t.TempDir(), "commit-marker")
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	newOwner := newManagedRuntimeForTest(t, statePath)

	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		_, err := oldOwner.withGenerationOwnerCommit(func() error {
			close(commitEntered)
			<-releaseCommit
			return os.WriteFile(markerPath, []byte("old-owner"), 0600)
		})
		commitDone <- err
	}()
	<-commitEntered

	type claimResult struct {
		claimed bool
		active  bool
		err     error
	}
	claimDone := make(chan claimResult, 1)
	go func() {
		claimed, active, err := newOwner.claimGenerationAfterSuccessfulRefresh()
		claimDone <- claimResult{claimed: claimed, active: active, err: err}
	}()
	select {
	case result := <-claimDone:
		t.Fatalf("claim bypassed in-flight commit fence: %#v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCommit)
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	result := <-claimDone
	if result.err != nil || !result.claimed || !result.active {
		t.Fatalf("claim after commit fence = %#v", result)
	}
	committed, err := newOwner.withGenerationOwnerCommit(func() error {
		return os.WriteFile(markerPath, []byte("new-owner"), 0600)
	})
	if err != nil || !committed {
		t.Fatalf("new owner commit committed=%v err=%v", committed, err)
	}
	retiredClosureRan := false
	committed, err = oldOwner.withGenerationOwnerCommit(func() error {
		retiredClosureRan = true
		return os.WriteFile(markerPath, []byte("retired-owner"), 0600)
	})
	if err != nil || committed || retiredClosureRan {
		t.Fatalf("retired commit committed=%v closure_ran=%v err=%v", committed, retiredClosureRan, err)
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "new-owner" {
		t.Fatalf("retired generation overwrote marker: %q", raw)
	}
}

func TestManagedUnclaimedStateCannotPersist(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := newManagedRuntimeForTest(t, statePath)
	banStore.set("acct", banEntry{Kind: banKindQuota, ResetAt: time.Now().Add(time.Hour)})
	if state.persistBanState() {
		t.Fatal("managed but unclaimed runtime persisted shared state")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("managed but unclaimed state file exists: %v", err)
	}
}

func TestManagedUnclaimedRuntimeFailsClosed(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	var managementCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		managementCalls.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	state := newManagedRuntimeForTest(t, statePath)
	state.cfg.WarmupEnabled = true
	state.cfg.WarmupExecutionMode = "management"
	state.cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	state.cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "management-key")
	state.cfg.WarmupSidecarURL = server.URL + "/backend-api/codex"

	status := state.generationStatus()
	if status.Active || status.Claimed || !status.Managed {
		t.Fatalf("unclaimed runtime reported active: %#v", status)
	}
	if state.admitBackgroundWorker() {
		state.wg.Done()
		t.Fatal("unclaimed runtime admitted a background worker")
	}
	banStore.set("acct", banEntry{Kind: banKindQuota, ResetAt: time.Now().Add(time.Hour)})
	if state.persistBanState() {
		t.Fatal("unclaimed runtime persisted shared state")
	}
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	if got := managementCalls.Load(); got != 0 {
		t.Fatalf("unclaimed runtime issued %d management requests", got)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("unclaimed runtime created shared state: %v", err)
	}
}

func TestEmptyStatePathPreservesLegacyWriter(t *testing.T) {
	state := &schedulerRuntimeState{cfg: defaultPluginConfig()}
	state.cfg.StatePath = ""
	state.initializeGenerationOwnership("")
	committed := false
	ok, err := state.withGenerationOwnerCommit(func() error {
		committed = true
		return nil
	})
	if err != nil || !ok || !committed || !state.generationOwnerActive() {
		t.Fatalf("legacy writer gate ok=%v committed=%v active=%v err=%v", ok, committed, state.generationOwnerActive(), err)
	}
}

func TestOwnerShutdownPersistsAndAppendsTombstone(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	state := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, state)
	banStore.set("acct", banEntry{Kind: banKindQuota, Window: "weekly", BannedAt: time.Now(), ResetAt: time.Now().Add(time.Hour)})
	state.stop()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted.Bans["acct"]; !ok {
		t.Fatalf("owner shutdown did not persist quarantine: %#v", persisted.Bans)
	}
	record, err := readSchedulerGenerationRecord(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if record.Active || record.ActiveGeneration == 0 || record.ReleasedAt.IsZero() {
		t.Fatalf("owner shutdown did not append tombstone: %#v", record)
	}
}

func TestSupersededShutdownCannotReleaseNewOwner(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	newOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, newOwner)
	oldOwner.stop()

	record, err := readSchedulerGenerationRecord(statePath)
	if err != nil {
		t.Fatal(err)
	}
	newOwnership := newOwner.generationSnapshot()
	if !record.Active || record.ActiveGeneration != newOwnership.Ticket || record.ActiveOwner != newOwnership.Owner {
		t.Fatalf("superseded shutdown disturbed new owner: record=%#v new=%#v", record, newOwnership)
	}
}

func TestGenerationTakeoverPreservesSerialActiveAuth(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	oldOwner := newManagedRuntimeForTest(t, statePath)
	claimManagedRuntimeForTest(t, oldOwner)
	oldOwner.quotas = map[string]quotaSnapshot{
		"primary": {
			AuthID: "primary", RefreshedAt: now,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 60, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now}},
		},
		"backup": {
			AuthID: "backup", RefreshedAt: now,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 10, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now}},
		},
	}
	request := serialTestRequest()
	first, err := oldOwner.schedulerPick(request)
	if err != nil || !first.Handled || first.AuthID != "primary" {
		t.Fatalf("old owner initial pick=%#v err=%v", first, err)
	}

	newOwner := newManagedRuntimeForTest(t, statePath)
	newOwner.loadBanState(statePath)
	newOwner.quotas = map[string]quotaSnapshot{
		"primary": {
			AuthID: "primary", RefreshedAt: now,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 60, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now}},
		},
		"backup": {
			AuthID: "backup", RefreshedAt: now,
			Windows: []quotaWindow{{Class: "weekly", UsedPercent: 90, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now}},
		},
	}
	if newOwner.serialActiveAuthID != "primary" {
		t.Fatalf("new generation did not restore serial active auth: %q", newOwner.serialActiveAuthID)
	}
	claimManagedRuntimeForTest(t, newOwner)
	t.Cleanup(func() {
		oldOwner.stop()
		newOwner.stop()
	})

	next, err := newOwner.schedulerPick(request)
	if err != nil || !next.Handled || next.AuthID != "primary" {
		t.Fatalf("takeover changed fill-first active auth=%#v err=%v", next, err)
	}
	if oldOwner.persistBanState() {
		t.Fatal("retired owner persisted state after takeover")
	}
	if !oldOwner.generationStatus().Superseded {
		t.Fatalf("retired owner did not become superseded: %#v", oldOwner.generationStatus())
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.SerialActiveAuthID != "primary" {
		t.Fatalf("takeover state lost serial active auth: %#v", persisted)
	}
}

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func resetWarmupSnapshot(t *testing.T, at time.Time) quotaSnapshot {
	t.Helper()
	q, err := parseNativeQuota([]byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`), "a", "idx-a", at)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func completedOldWarmup(at time.Time) warmupEntry {
	return warmupEntry{AuthID: "a", AuthIndex: "idx-a", Window: "weekly", Status: 200, DispatchState: "completed",
		AttemptedAt: at.Add(-48 * time.Hour), CompletedAt: at.Add(-48 * time.Hour), ActivatedAt: at.Add(-48 * time.Hour),
		OutcomeAt: at.Add(-48 * time.Hour), ResetAt: at.Add(48 * time.Hour), SuppressUntil: at.Add(48 * time.Hour)}
}

func TestWarmupResetRequiresIndependentMovingNativeObservations(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	for _, scenario := range []string{"reset", "same_observation", "too_soon", "fixed_zero_anchor", "header", "partial", "recent_success", "uncertain", "blocked", "replacement", "weekly_threshold", "usage_after_probe", "inconsistent_anchor", "intervening_warmup", "late_confirmation"} {
		t.Run(scenario, func(t *testing.T) {
			first := time.Now().UTC().Add(-time.Minute)
			now := first.Add(time.Minute)
			cfg := defaultPluginConfig()
			cfg.WarmupEnabled = true
			cfg.ReserveWeeklyPercent = 0
			entry := completedOldWarmup(first)
			state := schedulerRuntimeState{cfg: cfg, warmups: map[string]warmupEntry{"a|weekly": entry},
				quotas: map[string]quotaSnapshot{}, quotaPolls: map[string]quotaPollState{}}
			q := resetWarmupSnapshot(t, first)
			state.quotas["a"] = q
			if state.reconcileWarmupReset(q, first) || !warmupEntrySuppressesNow(state.warmups["a|weekly"], first, cfg.WarmupRetryAfter) {
				t.Fatal("a single observation erased a completed activation")
			}
			q = resetWarmupSnapshot(t, now)
			switch scenario {
			case "same_observation":
				q = resetWarmupSnapshot(t, first)
			case "too_soon":
				q = resetWarmupSnapshot(t, first.Add(10*time.Second))
			case "fixed_zero_anchor":
				q.Windows[1].ResetAt = first.Add(7 * 24 * time.Hour)
				q.Windows[1].ResetAfterSeconds -= 60
			case "header":
				q.Windows[1].Source = quotaSourceHeader
			case "partial":
				q.Windows = q.Windows[:1]
			case "recent_success":
				entry.CompletedAt, entry.ActivatedAt, entry.OutcomeAt = now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Minute)
			case "uncertain":
				entry.DispatchState, entry.Error, entry.ActivatedAt = "uncertain", "response_incomplete", time.Time{}
			case "blocked":
				entry.Blocked = true
			case "replacement":
				q.AuthIndex = "replacement"
			case "weekly_threshold":
				q.Windows[1].UsedPercent = 98
			case "usage_after_probe":
				state.quotaPolls["a"] = quotaPollState{LastUsageAt: now.Add(time.Second)}
			case "inconsistent_anchor":
				q.Windows[1].ResetAt = now.Add(time.Hour)
			case "intervening_warmup":
				entry.OutcomeAt = first.Add(-time.Hour)
			case "late_confirmation":
				now = first.Add(11 * time.Minute)
				q = resetWarmupSnapshot(t, now)
			}
			state.warmups["a|weekly"] = entry
			state.quotas["a"] = q
			got := state.reconcileWarmupReset(q, now)
			if got != (scenario == "reset") {
				t.Fatalf("reset confirmed=%v for %s", got, scenario)
			}
			if warmupEntrySuppressesNow(state.warmups["a|weekly"], now, cfg.WarmupRetryAfter) == got {
				t.Fatal("reset confirmation did not control old success suppression")
			}
		})
	}
}

func TestWarmupResetPreflightActuallyDispatchesBeforeStandbyCooldown(t *testing.T) {
	for _, scenario := range []string{"reset_confirmation", "due_5h", "quota_backoff", "not_due"} {
		t.Run(scenario, func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			calls := 0
			files := []map[string]any{{"id": "a", "auth_index": "idx-a", "provider": "codex", "status": "active"}}
			for i := 0; i < 10; i++ {
				id := fmt.Sprintf("account-%d", i)
				files = append(files, map[string]any{"id": id, "auth_index": "idx-" + id, "provider": "codex", "status": "active"})
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/management/auth-files" {
					writeTestJSON(t, w, map[string]any{"files": files})
					return
				}
				calls++
				if scenario == "due_5h" {
					writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(0)})
					return
				}
				writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: `{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":18000},"secondary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":604800}}}`})
			}))
			defer server.Close()
			state := newRefreshTestState(t, server.URL)
			state.cfg.WarmupEnabled, state.cfg.QuotaProbeOnDemand, state.cfg.ReserveWeeklyPercent = true, true, 0
			now := time.Now().UTC()
			first := now.Add(-time.Minute)
			q := resetWarmupSnapshot(t, first)
			poll := quotaPollState{AuthIndex: "idx-a", AttemptedAt: first, NextAt: now.Add(30 * time.Minute)}
			state.quotas["a"] = q
			if scenario == "reset_confirmation" || scenario == "quota_backoff" {
				state.warmups["a|weekly"] = completedOldWarmup(first)
				state.reconcileWarmupReset(q, first)
			} else {
				q = readyWarmupQuota("a", now.Add(-8*time.Minute))
				poll.AttemptedAt = q.RefreshedAt
				state.quotas["a"] = q
				activateAt := now.Add(-time.Minute)
				if scenario == "not_due" {
					activateAt = now.Add(time.Hour)
				}
				state.fiveHourPhases = map[string]fiveHourPhaseState{"a": {AuthID: "a", AuthIndex: "idx-a", ActivateAt: activateAt}}
			}
			if scenario == "quota_backoff" {
				poll.Failures, poll.Error = 1, "quota_http_429"
			}
			state.quotaPolls["a"] = poll
			// A minority reset must work without changing the global epoch/quorum.
			state.quotaEpoch.ID = "old-epoch"
			state.quotaEpoch.Accounts = map[string]quotaEpochAccountState{}
			state.quotaEpoch.Accounts["a"] = quotaEpochAccountState{AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountWarmed}
			for i := 0; i < 10; i++ {
				id := fmt.Sprintf("account-%d", i)
				state.quotaEpoch.Accounts[id] = quotaEpochAccountState{AuthID: id, AuthIndex: "idx-" + id, State: quotaEpochAccountWarmed}
				other := readyWarmupQuota(id, first)
				state.quotas[id] = other
				state.quotaPolls[id] = quotaPollState{AuthIndex: other.AuthIndex, AttemptedAt: first, NextAt: now.Add(30 * time.Minute)}
			}
			state.refreshOnce(context.Background())
			want := 0
			if scenario == "reset_confirmation" || scenario == "due_5h" {
				want = 1
			}
			if calls != want {
				t.Fatalf("actual native calls=%d want=%d", calls, want)
			}
			if scenario == "reset_confirmation" && (!warmupEntryResetConfirmed(state.warmups["a|weekly"]) || state.quotaEpoch.ID != "old-epoch") {
				t.Fatal("single-account reset remained stuck or changed global epoch")
			}
		})
	}
}

func TestWarmupResetReceiptSurvivesReloadAndOldOutcomeJournal(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now().UTC()
	state := newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	claimManagedRuntimeForTest(t, state)
	state.cfg.WarmupEnabled, state.cfg.ReserveWeeklyPercent = true, 0
	entry := completedOldWarmup(now)
	state.warmups["a|weekly"] = entry
	sibling := entry
	sibling.AuthID, sibling.AuthIndex = "b", "idx-b"
	state.warmups["b|weekly"] = sibling
	state.warmupAttempts = []warmupAttempt{{AuthID: "a", At: now.Add(-time.Hour)}}
	for _, at := range []time.Time{now.Add(-time.Minute), now} {
		q := resetWarmupSnapshot(t, at)
		state.quotas["a"] = q
		state.reconcileWarmupReset(q, at)
	}
	if !warmupEntryResetConfirmed(state.warmups["a|weekly"]) || !state.persistBanState() {
		t.Fatal("could not persist confirmed account reset")
	}
	if err := appendWarmupOutcomeJournal(state.cfg.StatePath, warmupOutcomeJournalRecord{Version: warmupOutcomeJournalVersion, Key: "a|weekly", Entry: entry}); err != nil {
		t.Fatal(err)
	}
	reloaded := schedulerRuntimeState{cfg: state.cfg}
	reloaded.loadBanState(state.cfg.StatePath)
	if _, _, err := reloaded.mergePersistedWarmupsLocked(state.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	if warmupEntrySuppressesNow(reloaded.warmups["a|weekly"], now, state.cfg.WarmupRetryAfter) ||
		!warmupEntrySuppressesNow(reloaded.warmups["b|weekly"], now, state.cfg.WarmupRetryAfter) || len(reloaded.warmupAttempts) != 1 {
		t.Fatal("reload resurrected old suppression, cleared a sibling, or refunded the budget")
	}
	// A newer real admission always supersedes the invalidated success receipt.
	newEntry := entry
	newEntry.AttemptedAt, newEntry.OutcomeAt, newEntry.CompletedAt, newEntry.ActivatedAt = now.Add(time.Minute), now.Add(time.Minute), time.Time{}, time.Time{}
	newEntry.DispatchState = "uncertain"
	reloaded.mergeWarmupEntriesLocked(map[string]warmupEntry{"a|weekly": newEntry})
	if !warmupEntrySuppressesNow(reloaded.warmups["a|weekly"], now.Add(time.Minute), state.cfg.WarmupRetryAfter) {
		t.Fatal("reset receipt bypassed newer uncertain admission")
	}
}

func TestWarmupResetRecoveredCycleAdmitsOneActivationAfterReload(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "idx-a", "provider": "codex", "status": "active", "id_token": map[string]string{"chatgpt_account_id": "workspace-a"}}}})
			return
		}
		calls.Add(1)
		writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"})
	}))
	defer server.Close()
	base := newRefreshTestState(t, server.URL)
	path := filepath.Join(t.TempDir(), "state.json")
	state := newManagedRuntimeForTest(t, path)
	claimManagedRuntimeForTest(t, state)
	base.cfg.StatePath, base.cfg.WarmupEnabled, base.cfg.ReserveWeeklyPercent = path, true, 0
	state.cfg = base.cfg
	state.warmups["a|weekly"] = completedOldWarmup(time.Now())
	for _, at := range []time.Time{time.Now().Add(-time.Minute), time.Now()} {
		q := resetWarmupSnapshot(t, at)
		state.quotas["a"] = q
		state.reconcileWarmupReset(q, at)
	}
	if !state.persistBanState() {
		t.Fatal("could not persist reset proof")
	}
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	state.scheduleWarmup(context.Background(), nil)
	state.wg.Wait()
	reloaded := newManagedRuntimeForTest(t, path)
	claimManagedRuntimeForTest(t, reloaded)
	reloaded.cfg = state.cfg
	reloaded.scheduleWarmup(context.Background(), nil)
	reloaded.wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("recovered weekly/5h activation sent %d model requests", calls.Load())
	}
}

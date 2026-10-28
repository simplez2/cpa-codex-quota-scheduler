package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestRecoveryRetryAfterAndResetAreIndependent(t *testing.T) {
	now := time.Now()
	header := http.Header{"Retry-After": {"18"}, "X-Codex-Primary-Window-Minutes": {"300"},
		"X-Codex-Primary-Used-Percent": {"100"}, "X-Codex-Primary-Reset-At": {fmt.Sprint(now.Add(7 * time.Second).Unix())}}
	entry, authoritative := quarantineEntryFor429(header, now, time.Second, 2*time.Second)
	if !authoritative || !entry.ResetAt.Equal(now.Add(18*time.Second)) {
		t.Fatalf("server Retry-After was shortened: %+v authoritative=%v", entry, authoritative)
	}
	var guard recoveryGuard
	full, used := 100.0, 10.0
	old := continuityQuota("a", now, &full, now.Add(7*time.Second), 20)
	guard.observe(old, now, 15*time.Minute)
	guard.require("a", "a-index", now.Add(time.Second), now.Add(18*time.Second))
	freshAt := now.Add(10 * time.Second)
	fresh := continuityQuota("a", freshAt, &used, freshAt.Add(5*time.Hour), 20)
	guard.observe(fresh, freshAt, 15*time.Minute)
	if allowed, guarded := guard.reserve("a", "a-index", "req", freshAt); allowed || !guarded {
		t.Fatal("positive quota observation bypassed Retry-After")
	}
	if allowed, guarded := guard.reserve("a", "a-index", "req", now.Add(18*time.Second)); !allowed || !guarded {
		t.Fatal("valid new-cycle observation was discarded until another quota probe")
	}
}

func TestRecoveryAfterAuthSingleFlightWithoutConcurrencyLimit(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	full, used := 100.0, 10.0
	old := continuityQuota("a", now.Add(-2*time.Minute), &full, now.Add(-time.Minute), 20)
	fresh := continuityQuota("a", now.Add(-time.Second), &used, now.Add(5*time.Hour), 20)
	s := continuityStateWithQuotas(fresh)
	s.cfg.AccountConcurrencyEnabled, s.cfg.StatePath = false, ""
	s.recovery.observe(old, now, s.cfg.StaleAfter)
	s.recovery.observe(fresh, now, s.cfg.StaleAfter)
	const workers = 16
	start := make(chan struct{})
	results := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id := fmt.Sprintf("request-%d", i)
			response := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: id, ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a", "selected_auth_index": "a-index"}})
			if !response.Terminate {
				results <- id
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var admitted []string
	for id := range results {
		admitted = append(admitted, id)
	}
	if len(admitted) != 1 {
		t.Fatalf("admitted %d upstream requests: %v", len(admitted), admitted)
	}
	if !s.recovery.finish(admitted[0], true, time.Now(), time.Second, time.Minute) {
		t.Fatal("successful exact owner did not complete recovery")
	}
	response := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "subsequent", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a", "selected_auth_index": "a-index"}})
	if response.Terminate {
		t.Fatalf("credential did not resume after successful probe: %+v", response)
	}
}

func TestRecoveryKeepsInflightOwnerOnNew429AndExtendsCooldown(t *testing.T) {
	now := time.Now()
	var bans banState
	bans.set("a", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, ResetAt: now.Add(-time.Second)})
	if allowed, _ := bans.tryStartProbeOwned("a", "owner", now, time.Minute); !allowed {
		t.Fatal("failed to claim initial half-open probe")
	}
	bans.markProbeDispatched("a", "owner")
	bans.record429("a", banEntry{Kind: banKindQuota, BannedAt: now, ResetAt: now.Add(time.Hour), LocalBackoff: time.Second, LocalBackoffMax: time.Minute}, now.Add(-time.Hour))
	entry, _ := bans.lookup("a")
	if entry.ProbeOwner != "owner" || !entry.ProbeDispatched || banEntryDisposition(entry, now.Add(2*time.Hour)) != banDispositionHalfOpen {
		t.Fatalf("asynchronous 429 released an in-flight probe: %+v", entry)
	}
	bans.completeOwnedProbe("owner", true, now.Add(time.Second), time.Second, time.Minute)
	entry, exists := bans.lookup("a")
	if !exists || entry.Phase != banPhaseCooldown || entry.ResetAt.Before(now.Add(time.Hour)) {
		t.Fatalf("an older probe success erased newer 429 evidence: %+v", entry)
	}
}

func TestRecoveryCancellationAndFailureRequireFreshObservation(t *testing.T) {
	now := time.Now()
	var guard recoveryGuard
	guard.accounts = map[string]recoveryEntry{"a": {AuthIndex: "a-index", Ready: true, Generation: 1}}
	if allowed, _ := guard.reserve("a", "a-index", "owner", now); !allowed || !guard.dispatched("a", "owner") {
		t.Fatal("could not dispatch probe")
	}
	guard.finish("owner", false, now.Add(time.Hour), 3*time.Second, 10*time.Second)
	entry := guard.snapshot()["a"]
	if entry.Ready || entry.RetryAt.Before(now.Add(time.Hour+3*time.Second)) {
		t.Fatalf("failed probe lost its guard or retry delay: %+v", entry)
	}
	if allowed, _ := guard.reserve("a", "a-index", "other", now.Add(24*time.Hour)); allowed {
		t.Fatal("elapsed backoff without a fresh observation opened recovery")
	}
	if got := recoveryRetryDelay(10, 3*time.Second, 10*time.Second); got != 10*time.Second {
		t.Fatalf("retry cap = %v", got)
	}
}

func TestRecoveryRestoresGuardWithoutTrustingSavedReadiness(t *testing.T) {
	var guard recoveryGuard
	guard.restore(map[string]recoveryEntry{"a": {AuthIndex: "idx", Ready: true, Generation: 1}})
	if allowed, guarded := guard.reserve("a", "idx", "request", time.Now()); allowed || !guarded {
		t.Fatal("restored readiness authorized generation without a fresh native observation")
	}
	guard.restore(map[string]recoveryEntry{"b": {AuthIndex: "idx-b", Ready: true, Owner: "running", Dispatched: true}})
	entry := guard.snapshot()["b"]
	if !entry.Dispatched || entry.Owner != "running" {
		t.Fatalf("restoration lost an active lease: %+v", entry)
	}
}

func TestRecoveryReleasesOnlyOtherOwnedCredential(t *testing.T) {
	var guard recoveryGuard
	now := time.Now()
	guard.accounts = map[string]recoveryEntry{
		"a": {AuthIndex: "a-index", Ready: true, Owner: "req", ReservedAt: now},
		"b": {AuthIndex: "b-index", Ready: true, Owner: "req", ReservedAt: now},
	}
	guard.finishOther("req", "b", now, time.Second, time.Minute)
	entries := guard.snapshot()
	if entries["a"].Owner != "" || entries["b"].Owner != "req" {
		t.Fatalf("wrong reservation released: %+v", entries)
	}
}

func TestRecoveryHostRestartReleasesOrphanButHotReloadKeepsLease(t *testing.T) {
	marker := currentRecoveryHostMarker()
	if marker == "" {
		t.Skip("stable host process marker is unavailable")
	}
	now := time.Now()
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%v", restart), func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			previousMarker := marker
			if restart {
				previousMarker = marker + ":previous-process"
			}
			state := persistedBanState{
				HostMarker: previousMarker,
				Recovery: map[string]recoveryEntry{"a": {AuthIndex: "a-index", ObservedAt: now.Add(-time.Hour), Ready: true,
					Owner: "running", Dispatched: true, Generation: 1, OwnerGeneration: 1}},
				Bans: map[string]banEntry{"a": {Kind: banKindQuota, Phase: banPhaseHalfOpen, Window: "5h",
					ResetAt: now.Add(-time.Hour), ProbeOwner: "running", ProbeDispatched: true, ProbeStartedAt: now.Add(-time.Minute)}},
			}
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := defaultPluginConfig()
			cfg.StatePath, cfg.HalfOpenRetryAfter = "", time.Second
			s := schedulerRuntimeState{cfg: cfg}
			s.loadBanState(path)
			guard := s.recovery.snapshot()["a"]
			ban, _ := banStore.lookup("a")
			if restart {
				if guard.Owner != "" || guard.Dispatched || guard.Ready || !guard.RetryAt.After(now) || ban.Phase != banPhaseCooldown || !ban.ResetAt.After(now) {
					t.Fatalf("orphan lease not converted to guarded retry: guard=%+v ban=%+v", guard, ban)
				}
			} else if guard.Owner != "running" || !guard.Dispatched || ban.ProbeOwner != "running" || ban.Phase != banPhaseHalfOpen {
				t.Fatalf("hot reload lost an active lease: guard=%+v ban=%+v", guard, ban)
			}
		})
	}
}

func TestContinuityWaitsWhileFiveHourProbeRuns(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	used := 10.0
	q := continuityQuota("a", now, &used, now.Add(5*time.Hour), 20)
	s := continuityStateWithQuotas(q)
	banStore.set("a", banEntry{Kind: banKindQuota, Phase: banPhaseHalfOpen, Window: "5h", ResetAt: now.Add(-time.Minute),
		ProbeOwner: "owner", ProbeStartedAt: now.Add(-time.Second), ProbeLeaseUntil: now.Add(time.Minute)})
	req := continuityRequest("a")
	req.RequestID = "other"
	if got := s.assessContinuity(req, now); got.Action != "wait" || got.Reason != "five_hour_recheck" {
		t.Fatalf("running probe dropped a waiting request: %+v", got)
	}
}

func TestRecoveryRejectsChangedBindingUntilNewNativeObservation(t *testing.T) {
	var guard recoveryGuard
	guard.accounts = map[string]recoveryEntry{"a": {AuthIndex: "old", Ready: true}}
	if allowed, guarded := guard.reserve("a", "new", "request", time.Now()); allowed || !guarded {
		t.Fatal("changed credential binding bypassed recovery")
	}
}

func TestRecoverySameWorkspaceKeepsDistinctCredentialsIndependent(t *testing.T) {
	now := time.Now()
	var guard recoveryGuard
	full, used := 100.0, 1.0
	a := continuityQuota("a", now, &full, now.Add(time.Hour), 20)
	b := continuityQuota("b", now, &used, now.Add(time.Hour), 20)
	a.AccountID, b.AccountID = "same-workspace", "same-workspace"
	guard.observe(a, now, 15*time.Minute)
	guard.observe(b, now, 15*time.Minute)
	if allowed, guarded := guard.reserve("b", "b-index", "other", now); !allowed || guarded {
		t.Fatal("workspace account ID wrongly blocked a different credential")
	}
}

func TestRecoveryProtectsCodexCompactNativeFallback(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	used := 10.0
	q := continuityQuota("a", now, &used, now.Add(time.Hour), 20)
	s := continuityStateWithQuotas(q)
	s.cfg.StatePath, s.cfg.AccountConcurrencyEnabled = "", false
	s.recovery.accounts = map[string]recoveryEntry{"a": {AuthIndex: "a-index", Ready: true, Generation: 1}}
	request := func(id, auth, index string) pluginapi.RequestInterceptResponse {
		return s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: id, ToFormat: "openai-response", Metadata: map[string]any{"selected_auth_id": auth, "selected_auth_index": index}})
	}
	if got := request("first", "a", "a-index"); got.Terminate {
		t.Fatalf("first compact recovery probe rejected: %+v", got)
	}
	if got := request("second", "a", "a-index"); !got.Terminate {
		t.Fatal("native compact fallback bypassed recovery probe")
	}
	if got := request("third", "third-party", "third-index"); got.Terminate {
		t.Fatalf("third-party format was incorrectly treated as Codex: %+v", got)
	}
}

func TestRecoveryProtectsCompactBeforeFirstQuotaProbe(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath, cfg.AccountConcurrencyEnabled = "", false
	s := schedulerRuntimeState{cfg: cfg}
	s.recovery.require("a", "a-index", time.Now(), time.Now().Add(time.Hour))
	got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "native", ToFormat: "openai-response", Metadata: map[string]any{"selected_auth_id": "a", "selected_auth_index": "a-index"}})
	if !got.Terminate {
		t.Fatal("compact request bypassed a known Codex 429 before first quota probe")
	}
}

func TestRecoveryProtectsCompactWhenOnlyCodexBanIsKnown(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	cfg := defaultPluginConfig()
	cfg.StatePath, cfg.AccountConcurrencyEnabled = "", false
	s := schedulerRuntimeState{cfg: cfg}
	banStore.set("a", banEntry{Kind: banKindProbation, Phase: banPhaseCooldown, ResetAt: time.Now().Add(time.Hour)})
	got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "native", ToFormat: "openai-response", Metadata: map[string]any{"selected_auth_id": "a", "selected_auth_index": "a-index"}})
	if !got.Terminate {
		t.Fatal("compact request bypassed a Codex 429 ban before first quota probe")
	}
}

func TestRecoveryPickRejectsOnlyWhenEveryCandidateIsBlocked(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	full := 100.0
	q := continuityQuota("b", now, &full, now.Add(time.Hour), 20)
	s := continuityStateWithQuotas(q)
	s.cfg.StatePath, s.cfg.SchedulerMode = "", "serial"
	s.recovery.observe(q, now, s.cfg.StaleAfter)
	// The production scheduler candidate does not include auth_index metadata.
	req := pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-test", Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "b", Provider: "codex"}}}
	got, err := s.schedulerPickUncapped(req)
	if err != nil || !got.Handled || !got.Reject || got.RejectCode != "quota_recovery_wait" {
		t.Fatalf("known exhausted candidate allowed native fallback: %+v %v", got, err)
	}
	req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: "unknown", Provider: "codex"})
	got, err = s.schedulerPickUncapped(req)
	if err != nil || got.Reject {
		t.Fatalf("unknown candidate was treated as confirmed exhausted: %+v %v", got, err)
	}
	s.cfg.ContinuityEnabled = false
	req.Candidates = req.Candidates[:1]
	got, err = s.schedulerPickUncapped(req)
	if err != nil || got.Reject {
		t.Fatalf("disabled continuity changed native fallback: %+v %v", got, err)
	}
}

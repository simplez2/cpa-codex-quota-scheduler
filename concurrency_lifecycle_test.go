package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestConcurrencyReconfigureQuiescingKeepsVerifiedForegroundAdmission(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	req := correlatedConcurrencyRequest(s, base, "during-reconfigure", "conversation")
	// Reconfiguration quiesces background work before replacing the policy. The
	// credential ledger and this native CPA execution remain valid throughout.
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	picked, err := s.schedulerPick(req)
	if err != nil || !picked.Handled || picked.AuthID == "" {
		t.Fatalf("verified execution falsely rejected during reconfigure: %+v %v", picked, err)
	}
	after := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "during-reconfigure", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": picked.AuthID}})
	if after.Terminate || concurrencyCount(s).Active != 1 {
		t.Fatalf("native execution not admitted: %+v %+v", after, concurrencyCount(s))
	}
	blocked := correlatedConcurrencyRequest(s, base, "busy-during-reconfigure", "conversation")
	if _, err := s.schedulerPick(blocked); err == nil {
		t.Fatal("quiescing bypassed the credential concurrency cap")
	}
	s.concurrencyComplete("busy-during-reconfigure")
	s.concurrencyComplete("during-reconfigure")
	if concurrencyCount(s).Active != 0 {
		t.Fatal("terminal event did not release the slot")
	}
}

func TestConcurrencyShutdownMakesRetainedNativeCallbacksPassive(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountConcurrencyWait = 0
	req := correlatedConcurrencyRequest(s, base, "old-stream", "old-session")
	picked, err := s.schedulerPick(req)
	if err != nil || !picked.Handled {
		t.Fatal(err)
	}
	if s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "old-stream", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": picked.AuthID}}).Terminate {
		t.Fatal("setup admission failed")
	}
	s.stop()
	// A native selector/session cache may retain the old scheduler object after
	// CPA has removed its lifecycle hooks. It must hand control back to CPA.
	before := s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "after-unload"})
	if len(before.Headers) != 0 || len(before.ClearHeaders) != 0 {
		t.Fatalf("retired callback modified the active generation headers: %+v", before)
	}
	got, err := s.schedulerPick(base)
	if err != nil || got.Handled {
		t.Fatalf("retired selector rejected or scheduled a new execution: %+v %v", got, err)
	}
	after := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "after-unload", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": picked.AuthID}})
	if after.Terminate || len(after.ClearHeaders) != 0 {
		t.Fatalf("retired after-auth callback blocked native execution: %+v", after)
	}
	if concurrencyCount(s).Active != 1 {
		t.Fatal("shutdown discarded the old stream ledger")
	}
	s.concurrencyComplete("old-stream")
	if concurrencyCount(s).Active != 0 {
		t.Fatal("old stream completion did not release its slot")
	}
}

func TestConcurrencyAfterAuthRecoversGenerationHandoffWithinCredentialCap(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	// CPA has already demonstrated lifecycle callbacks in this generation.
	s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "capability-observation"})
	s.concurrencyComplete("capability-observation")
	// The native request had a before-auth token belonging to a retired DSO.
	base.Options.Headers = map[string][]string{concurrencyHeader: {"old-generation-token"}}
	picked, err := s.schedulerPick(base)
	if err != nil || picked.Handled || concurrencyCount(s).Active != 0 {
		t.Fatalf("native after-auth fallback was rejected or admitted early: %+v %v", picked, err)
	}
	after := pluginapi.RequestInterceptRequest{RequestID: "handoff-first", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if got := s.concurrencyAfter(after); got.Terminate || concurrencyCount(s).Active != 1 {
		t.Fatalf("verified handoff was not admitted: %+v %+v", got, concurrencyCount(s))
	}
	after.RequestID = "handoff-busy"
	if got := s.concurrencyAfter(after); !got.Terminate || concurrencyCount(s).Credentials["a"].Active != 1 {
		t.Fatalf("late admission exceeded credential cap: %+v %+v", got, concurrencyCount(s))
	}
	after.RequestID = "handoff-peer"
	after.Metadata["selected_auth_id"] = "b"
	if got := s.concurrencyAfter(after); got.Terminate || concurrencyCount(s).Active != 2 {
		t.Fatalf("independent credential could not be admitted: %+v %+v", got, concurrencyCount(s))
	}
	for _, id := range []string{"handoff-first", "handoff-busy", "handoff-peer"} {
		s.concurrencyComplete(id)
	}
	if concurrencyCount(s).Active != 0 {
		t.Fatal("native terminal events did not release recovered slots")
	}
}

func TestConcurrencyAfterAuthRecoversStrippedHeaderButRejectsMissingNativeIdentity(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "stripped-header"})
	base.Options.Headers = nil
	if picked, err := s.schedulerPick(base); err != nil || picked.Handled {
		t.Fatalf("lost private header did not defer to native selection: %+v %v", picked, err)
	}
	after := pluginapi.RequestInterceptRequest{RequestID: "stripped-header", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if got := s.concurrencyAfter(after); got.Terminate || concurrencyCount(s).Active != 1 {
		t.Fatalf("CPA identity failed recovery: %+v", got)
	}
	s.concurrencyComplete(after.RequestID)
	for _, invalid := range []pluginapi.RequestInterceptRequest{
		{ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}},
		{RequestID: "missing-credential", ToFormat: "codex"},
	} {
		if got := s.concurrencyAfter(invalid); !got.Terminate || concurrencyCount(s).Active != 0 {
			t.Fatalf("unverifiable native lifecycle bypassed protection: %+v", got)
		}
	}
}

func TestConcurrencyRecoveredQueueCancellationCannotResurrectLifecycle(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, _ := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = time.Second
	after := pluginapi.RequestInterceptRequest{RequestID: "holding", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if s.concurrencyAfter(after).Terminate {
		t.Fatal("could not prepare held slot")
	}
	after.RequestID = "canceled-recovery"
	result := make(chan pluginapi.RequestInterceptResponse, 1)
	go func() { result <- s.concurrencyAfter(after) }()
	deadline := time.Now().Add(time.Second)
	for concurrencyCount(s).Waiting != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if concurrencyCount(s).Waiting != 1 {
		t.Fatal("recovered execution did not enter the FIFO queue")
	}
	s.concurrencyComplete("canceled-recovery")
	select {
	case got := <-result:
		if !got.Terminate || concurrencyCount(s).Active != 1 {
			t.Fatalf("canceled execution was admitted: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled lifecycle continued waiting")
	}
	s.concurrency.mu.Lock()
	_, resurrected := s.concurrency.requests["canceled-recovery"]
	s.concurrency.mu.Unlock()
	if resurrected {
		t.Fatal("canceled lifecycle was registered again")
	}
	s.concurrencyComplete("holding")
	if concurrencyCount(s).Active != 0 || concurrencyCount(s).Waiting != 0 {
		t.Fatal("recovered queue leaked slots or waiters")
	}
}

func TestConcurrencyRecoveredSlotsSurviveEnablingProtection(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, _ := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	s.cfg.AccountConcurrencyEnabled = false
	after := pluginapi.RequestInterceptRequest{RequestID: "cap-disabled-handoff", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if got := s.concurrencyAfter(after); got.Terminate || concurrencyCount(s).Active != 1 {
		t.Fatalf("recovered native slot was not tracked with the cap off: %+v", got)
	}
	s.mu.Lock()
	s.cfg.AccountConcurrencyEnabled = true
	s.mu.Unlock()
	after.RequestID = "cap-enabled-busy"
	if got := s.concurrencyAfter(after); !got.Terminate || concurrencyCount(s).Active != 1 {
		t.Fatalf("enabling the cap lost the existing native execution: %+v", got)
	}
	s.concurrencyComplete("cap-disabled-handoff")
	s.concurrencyComplete("cap-enabled-busy")
	after.RequestID = "cap-enabled-idle"
	if s.concurrencyAfter(after).Terminate {
		t.Fatal("recovered execution did not release its credential")
	}
	s.concurrencyComplete(after.RequestID)
}

func TestConcurrencyNativeCompletionBeforeRecoveryPreventsAdmission(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, _ := balancedFixture(time.Now())
	s.cfg.AccountConcurrencyWait = 0
	s.concurrencyComplete("already-canceled")
	after := pluginapi.RequestInterceptRequest{RequestID: "already-canceled", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if got := s.concurrencyAfter(after); !got.Terminate || concurrencyCount(s).Active != 0 {
		t.Fatalf("late after-auth resurrected completed native execution: %+v", got)
	}
	if token := s.concurrency.register("already-canceled", time.Now()); token != "" {
		t.Fatal("duplicate before-auth resurrected a terminal native ID")
	}
}

func TestConcurrencyTerminalIdentityMemoryIsBoundedAndKeepsRecentIDs(t *testing.T) {
	var g accountConcurrencyGate
	for i := 0; i < concurrencyRegistrationLimit+12; i++ {
		g.release(fmt.Sprintf("terminal-%d", i))
	}
	g.mu.Lock()
	count := len(g.completed)
	ring := len(g.completedIDs)
	recent := g.completed[fmt.Sprintf("terminal-%d", concurrencyRegistrationLimit+11)]
	old := g.completed["terminal-0"]
	g.mu.Unlock()
	if count != concurrencyRegistrationLimit || ring != concurrencyRegistrationLimit || !recent || old {
		t.Fatalf("terminal history is unbounded or lost recent cancellations: count=%d ring=%d recent=%v old=%v", count, ring, recent, old)
	}
}

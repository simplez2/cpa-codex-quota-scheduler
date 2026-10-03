package main

import (
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

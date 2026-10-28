package main

import (
	"fmt"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func correlatedConcurrencyRequest(s *schedulerRuntimeState, base pluginapi.SchedulerPickRequest, id, session string) pluginapi.SchedulerPickRequest {
	before := s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: id})
	base.Options.Headers = map[string][]string(before.Headers)
	if session != "" {
		base.Options.Metadata = map[string]any{"canonical_session_id": session}
	}
	return base
}
func concurrencyCount(s *schedulerRuntimeState) accountConcurrencyStatus {
	return s.concurrencyStatus(s.cfg, s.quotas)
}
func TestAccountConcurrencyParallelAdmissionsNeverExceedLimit(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 2
	s.cfg.AccountConcurrencyWait = 0
	var wg sync.WaitGroup
	results := make(chan string, 200)
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("request-%d", i)
			req := correlatedConcurrencyRequest(s, base, id, "")
			response, err := s.schedulerPick(req)
			if err == nil && response.Handled {
				results <- id
			} else {
				s.concurrency.release(id)
			}
		}(i)
	}
	wg.Wait()
	close(results)
	status := concurrencyCount(s)
	if status.Active != 4 || status.Accounts["a"].Active != 2 || status.Accounts["b"].Active != 2 {
		t.Fatalf("oversubscribed or uneven admissions: %+v", status)
	}
	for id := range results {
		s.concurrency.release(id)
		s.concurrency.release(id)
	}
	if got := concurrencyCount(s).Active; got != 0 {
		t.Fatalf("duplicate completion leaked/released wrong slot: %d", got)
	}
}
func TestAccountConcurrencyBusyStickyWaitsWithoutSwitching(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	first := correlatedConcurrencyRequest(s, base, "first", "conversation")
	got, err := s.schedulerPick(first)
	if err != nil {
		t.Fatal(err)
	}
	second := correlatedConcurrencyRequest(s, base, "second", "conversation")
	if _, err := s.schedulerPick(second); err == nil {
		t.Fatal("busy sticky conversation moved to another available account")
	}
	s.mu.RLock()
	binding, _ := s.balancedSessionLocked(schedulerSessionHash(second), time.Now())
	s.mu.RUnlock()
	if binding.AuthID != got.AuthID {
		t.Fatal("busy queue changed sticky binding")
	}
	different := correlatedConcurrencyRequest(s, base, "different", "different-conversation")
	other, err := s.schedulerPick(different)
	if err != nil || other.AuthID == got.AuthID {
		t.Fatalf("new conversation failed to use idle account: %+v %v", other, err)
	}
	s.concurrency.release("first")
	s.concurrency.release("different")
	retry, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, "fresh-retry", "conversation"))
	if err != nil || retry.AuthID != got.AuthID {
		t.Fatalf("sticky continuation did not recover: %+v %v", retry, err)
	}
}
func TestAccountConcurrencyQueuedRequestWakesOnCompletion(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	base.Candidates = base.Candidates[:1]
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = time.Second
	first := correlatedConcurrencyRequest(s, base, "first", "")
	if _, err := s.schedulerPick(first); err != nil {
		t.Fatal(err)
	}
	queued := correlatedConcurrencyRequest(s, base, "queued", "")
	done := make(chan error, 1)
	go func() { _, err := s.schedulerPick(queued); done <- err }()
	deadline := time.Now().Add(time.Second)
	for concurrencyCount(s).Waiting == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if concurrencyCount(s).Waiting != 1 {
		t.Fatal("request did not enter bounded queue")
	}
	s.concurrency.release("first")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion did not wake queue")
	}
	if status := concurrencyCount(s); status.Active != 1 || status.Waiting != 0 {
		t.Fatalf("queue bookkeeping: %+v", status)
	}
}
func TestAccountConcurrencyCanceledQueueCannotDispatch(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	base.Candidates = base.Candidates[:1]
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = time.Second
	first := correlatedConcurrencyRequest(s, base, "first", "")
	if _, err := s.schedulerPick(first); err != nil {
		t.Fatal(err)
	}
	queued := correlatedConcurrencyRequest(s, base, "cancelled", "")
	done := make(chan error, 1)
	go func() { _, err := s.schedulerPick(queued); done <- err }()
	deadline := time.Now().Add(time.Second)
	for concurrencyCount(s).Waiting == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	s.concurrency.release("cancelled")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled request was admitted")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not wake queue")
	}
	if concurrencyCount(s).Active != 1 {
		t.Fatal("cancellation released another request's slot")
	}
}
func TestAccountConcurrencyAliasesShareCapAndNewIdentityDoesNotResetIt(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	base.Candidates = base.Candidates[:1]
	first := correlatedConcurrencyRequest(s, base, "first", "")
	if _, err := s.schedulerPick(first); err != nil {
		t.Fatal(err)
	}
	snapshot := s.quotas["a"]
	snapshot.AuthID = "alias"
	s.quotas["alias"] = snapshot
	alias := base
	alias.Candidates = []pluginapi.SchedulerAuthCandidate{{ID: "alias", Provider: providerCodex, Attributes: map[string]string{"auth_index": "a"}}}
	second := correlatedConcurrencyRequest(s, alias, "second", "")
	if _, err := s.schedulerPick(second); err == nil {
		t.Fatal("auth_index alias bypassed credential cap")
	}
	s.concurrency.release("first")
	second = correlatedConcurrencyRequest(s, alias, "fresh-alias", "")
	if _, err := s.schedulerPick(second); err != nil {
		t.Fatal(err)
	}
}
func TestAccountConcurrencyCredentialsStayIndependentForSameWorkspaceAndUser(t *testing.T) {
	for _, sameUser := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_user_%t", sameUser), func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s, base := balancedFixture(time.Now())
			s.cfg.AccountMaxConcurrency = 1
			s.cfg.AccountConcurrencyWait = 0
			for _, id := range []string{"a", "b"} {
				q := s.quotas[id]
				q.AccountID = "team-workspace"
				s.quotas[id] = q
			}
			userB := "user-b@example.test"
			if sameUser {
				userB = "user-a@example.test"
			}
			base.Candidates[0].Metadata = map[string]any{"email": "user-a@example.test", "chatgpt_user_id": "user-a"}
			base.Candidates[1].Metadata = map[string]any{"email": userB}
			if sameUser {
				base.Candidates[1].Metadata["chatgpt_user_id"] = "user-a"
			}
			for _, id := range []string{"first", "second"} {
				req := correlatedConcurrencyRequest(s, base, id, "")
				if _, err := s.schedulerPick(req); err != nil {
					t.Fatal(err)
				}
			}
			got := concurrencyCount(s)
			if got.Scope != "credential" || got.MaxPerCredential != 1 || got.Credentials["a"].Active != 1 || got.Credentials["b"].Active != 1 {
				t.Fatalf("independent CPA credentials were merged: %+v", got)
			}
			if got.Accounts["a"] != got.Credentials["a"] || got.MaxPerAccount != got.MaxPerCredential {
				t.Fatal("legacy status aliases diverged")
			}
			third := correlatedConcurrencyRequest(s, base, "third", "")
			if _, err := s.schedulerPick(third); err == nil {
				t.Fatal("credential cap allowed a third execution")
			}
		})
	}
}
func TestAccountConcurrencyRetryMovesOneSlotAndOldUsageCannotReleaseIt(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountConcurrencyWait = 0
	req := correlatedConcurrencyRequest(s, base, "execution", "")
	first, err := s.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Candidates = schedulerCandidatesWithout(req.Candidates, first.AuthID)
	second, err := s.schedulerPick(req)
	if err != nil || second.AuthID == first.AuthID {
		t.Fatalf("retry: %+v %v", second, err)
	}
	s.observeUsage(pluginapi.UsageRecord{Provider: providerCodex, AuthID: first.AuthID, RequestedAt: time.Now(), Failed: true, Failure: pluginapi.UsageFailure{StatusCode: 503}})
	status := concurrencyCount(s)
	if status.Active != 1 || status.Accounts[first.AuthID].Active != 0 || status.Accounts[second.AuthID].Active != 1 {
		t.Fatalf("retry leaked slot or delayed usage released new attempt: %+v", status)
	}
	s.concurrency.release("execution")
	s.concurrency.release("execution")
	if concurrencyCount(s).Active != 0 {
		t.Fatal("terminal retry completion did not release slot")
	}
}
func TestAccountConcurrencySlotsSurviveLongStreamsAndSwitchChanges(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	base.Candidates = base.Candidates[:1]
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	s.cfg.AccountConcurrencyEnabled = false
	for _, id := range []string{"first", "second"} {
		req := correlatedConcurrencyRequest(s, base, id, "")
		if _, err := s.schedulerPick(req); err != nil {
			t.Fatal(err)
		}
	}
	s.concurrency.mu.Lock()
	s.concurrency.requests["first"].At = time.Now().Add(-48 * time.Hour)
	s.concurrency.mu.Unlock()
	s.cfg.AccountConcurrencyEnabled = true
	third := correlatedConcurrencyRequest(s, base, "third", "")
	if _, err := s.schedulerPick(third); err == nil {
		t.Fatal("enabling limit discarded disabled-mode/long-stream slots")
	}
	s.concurrency.release("first")
	if _, err := s.schedulerPick(third); err == nil {
		t.Fatal("new work admitted before lowered cap drained")
	}
	s.concurrency.release("second")
	third = correlatedConcurrencyRequest(s, base, "fresh-third", "")
	if _, err := s.schedulerPick(third); err != nil {
		t.Fatal(err)
	}
}
func TestAccountConcurrencyWarmupSharesForegroundSlot(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	base.Candidates = base.Candidates[:1]
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	candidate := warmupCandidate{Snapshot: s.quotas["a"]}
	s.selectionMu.Lock()
	warmupID, ok := s.beginWarmupConcurrency(candidate, s.cfg)
	s.selectionMu.Unlock()
	if !ok {
		t.Fatal("warmup not admitted")
	}
	request := correlatedConcurrencyRequest(s, base, "real-request", "")
	if _, err := s.schedulerPick(request); err == nil {
		t.Fatal("foreground exceeded credential cap while warmup running")
	}
	s.concurrency.release(warmupID)
	// A client retry starts a new CPA request lifecycle after admission timed out.
	request = correlatedConcurrencyRequest(s, base, "fresh-foreground", "")
	if _, err := s.schedulerPick(request); err != nil {
		t.Fatal(err)
	}
	before := len(s.warmupAttempts)
	s.selectionMu.Lock()
	_, ok = s.beginWarmupConcurrency(candidate, s.cfg)
	s.selectionMu.Unlock()
	if ok || len(s.warmupAttempts) != before {
		t.Fatal("busy foreground allowed warmup or consumed its daily budget")
	}
}
func TestAccountConcurrencyGuardStripsSpoofedHeadersAndStopsNativeFallback(t *testing.T) {
	s, base := balancedFixture(time.Now())
	s.cfg.AccountConcurrencyWait = 0
	before := s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "native-id", Headers: http.Header{concurrencyHeader: []string{"spoofed"}}})
	if before.Headers.Get(concurrencyHeader) == "" || before.Headers.Get(concurrencyHeader) == "spoofed" {
		t.Fatal("client supplied correlation accepted")
	}
	if got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "native-id", ToFormat: "codex"}); !got.Terminate || got.StatusCode != 503 {
		t.Fatal("unreserved native fallback could exceed cap")
	}
	s.concurrencyComplete("native-id")
	before = s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "admitted-id"})
	req := base
	req.Options.Headers = map[string][]string(before.Headers)
	if _, err := s.schedulerPick(req); err != nil {
		t.Fatal(err)
	}
	got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "admitted-id", ToFormat: "codex"})
	if got.Terminate || len(got.ClearHeaders) != 1 {
		t.Fatal("admitted request blocked or internal correlation forwarded")
	}
	if got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "third-party", ToFormat: "gemini"}); got.Terminate {
		t.Fatal("concurrency gate affected third-party provider")
	}
}
func TestAccountConcurrencySerialKeepsPrimaryWhenBusy(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	state := newSerialTestState(time.Now())
	s := &state
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	base := serialTestRequest()
	first := correlatedConcurrencyRequest(s, base, "first", "")
	selected, err := s.schedulerPick(first)
	if err != nil {
		t.Fatal(err)
	}
	second := correlatedConcurrencyRequest(s, base, "second", "")
	if _, err := s.schedulerPick(second); err == nil {
		t.Fatal("serial busy primary silently changed")
	}
	if s.serialActiveAuthID != selected.AuthID {
		t.Fatal("concurrency collision changed serial primary")
	}
}

func TestAccountConcurrencyMixedRouteAccountsOnlyNativeCodexSelection(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	first := correlatedConcurrencyRequest(s, base, "mixed-one", "")
	first.Providers = []string{"codex", "gemini"}
	if got, err := s.schedulerPick(first); err != nil || got.Handled {
		t.Fatalf("mixed routing changed: %+v %v", got, err)
	}
	after := pluginapi.RequestInterceptRequest{RequestID: "mixed-one", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if s.concurrencyAfter(after).Terminate {
		t.Fatal("native Codex selection was not admitted")
	}
	second := correlatedConcurrencyRequest(s, base, "mixed-two", "")
	second.Providers = first.Providers
	s.schedulerPick(second)
	after.RequestID = "mixed-two"
	if !s.concurrencyAfter(after).Terminate {
		t.Fatal("mixed Codex path exceeded cap")
	}
	if s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "mixed-two", ToFormat: "gemini"}).Terminate {
		t.Fatal("third party affected")
	}
	s.concurrencyComplete("mixed-one")
	if !s.concurrencyAfter(after).Terminate {
		t.Fatal("a timed-out execution was resurrected after release")
	}
	s.concurrencyComplete("mixed-two")
	fresh := correlatedConcurrencyRequest(s, base, "mixed-fresh", "")
	fresh.Providers = first.Providers
	s.schedulerPick(fresh)
	after.RequestID = "mixed-fresh"
	if s.concurrencyAfter(after).Terminate {
		t.Fatal("released slot was not available to native route")
	}
	if got := concurrencyCount(s).Accounts["a"].Active; got != 1 {
		t.Fatalf("native slot count: %d", got)
	}
}
func TestAccountConcurrencyUndispatchedCompletionRefundsOnlyItsOwnPrediction(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	for _, id := range []string{"cancel-before-upstream", "other-live"} {
		req := correlatedConcurrencyRequest(s, base, id, "")
		if _, err := s.schedulerPick(req); err != nil {
			t.Fatal(err)
		}
	}
	s.concurrencyComplete("cancel-before-upstream")
	pending := 0
	for _, account := range s.balancedAccounts {
		pending += len(account.Pending)
	}
	if pending != 1 || concurrencyCount(s).Active != 1 {
		t.Fatalf("cancel cleared unrelated reservation or leaked: pending=%d", pending)
	}
}
func TestAccountConcurrencyRejectsDifferentNativeCredential(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	req := correlatedConcurrencyRequest(s, base, "pick", "")
	selected, err := s.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	other := "a"
	if selected.AuthID == "a" {
		other = "b"
	}
	if !s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "pick", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": other}}).Terminate {
		t.Fatal("native credential did not match admitted account")
	}
}

func TestAccountConcurrencyRejectedPickCannotFallBackToNativeAuth(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountConcurrencyWait = 0
	s.cfg.AccountMaxConcurrency = 1
	for _, id := range []string{"first", "second"} {
		if _, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, id, "")); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, "overflow", ""))
	if _, ok := err.(*accountConcurrencyError); !ok {
		t.Fatalf("untyped admission failure: %v", err)
	}
	after := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "overflow", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}})
	if !after.Terminate || after.StatusCode != 503 || !strings.Contains(string(after.ResponseBody), "account_concurrency_busy") {
		t.Fatalf("unsafe/generic native fallback: %+v", after)
	}
}

func TestAccountConcurrencyNativeRetryKeepsQueueDeadline(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	base.Candidates = base.Candidates[:1]
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 40 * time.Millisecond
	if _, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, "active", "")); err != nil {
		t.Fatal(err)
	}
	waiting := correlatedConcurrencyRequest(s, base, "queued", "")
	if _, err := s.schedulerPick(waiting); err == nil {
		t.Fatal("full pool admitted queued request")
	}
	start := time.Now()
	if _, err := s.schedulerPick(waiting); err == nil {
		t.Fatal("retry bypassed cap")
	}
	if elapsed := time.Since(start); elapsed >= 20*time.Millisecond {
		t.Fatalf("retry reset queue duration: %v", elapsed)
	}
}

func TestAccountConcurrencyExpiredStickyCannotMoveWhenNativeRetryFiltersBoundAuth(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = 0
	selected, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, "first", "sticky"))
	if err != nil {
		t.Fatal(err)
	}
	queued := correlatedConcurrencyRequest(s, base, "queued", "sticky")
	if _, err := s.schedulerPick(queued); err == nil {
		t.Fatal("busy sticky admission was not rejected")
	}
	queued.Candidates = schedulerCandidatesWithout(queued.Candidates, selected.AuthID)
	if response, err := s.schedulerPick(queued); err == nil || response.Handled {
		t.Fatalf("expired native retry escaped sticky account: %+v %v", response, err)
	}
}

func TestAccountConcurrencyQueueOrderScopedToCredential(t *testing.T) {
	var g accountConcurrencyGate
	now := time.Now()
	g.register("older", now)
	g.register("newer", now)
	g.mu.Lock()
	defer g.mu.Unlock()
	older, newer := g.requests["older"], g.requests["newer"]
	older.QueueDeadline, newer.QueueDeadline = now.Add(time.Minute), now.Add(time.Minute)
	g.startWaitLocked(older, map[string]bool{"auth:a": true}, "a", "sticky_credential", now)
	g.startWaitLocked(newer, map[string]bool{"auth:a": true}, "a", "sticky_credential", now)
	if g.blockedLocked("auth:a", older, 1, now) {
		t.Fatal("oldest waiter did not own the available slot")
	}
	if !g.blockedLocked("auth:a", newer, 1, now) || !g.blockedLocked("auth:a", nil, 1, now) {
		t.Fatal("new traffic or warmup jumped the queue")
	}
	if g.blockedLocked("auth:b", newer, 1, now) {
		t.Fatal("queue blocked an unrelated credential")
	}
	older.QueueDeadline = now.Add(-time.Millisecond)
	if g.blockedLocked("auth:a", newer, 1, now) {
		t.Fatal("expired waiter still reserved priority")
	}
	older.QueueDeadline = now.Add(time.Minute)
	g.keyLocked([]string{"auth:a", "index:shared"})
	canonical := g.keyLocked([]string{"auth:0", "index:shared"})
	if !newer.WaitKeys[canonical] || !g.blockedLocked(canonical, newer, 1, now) {
		t.Fatal("alias reconciliation erased queue priority")
	}
	delete(g.requests, "older")
	if g.blockedLocked(canonical, newer, 1, now) {
		t.Fatal("cancelled waiter blocked new work")
	}
}
func TestAccountConcurrencyNativeTimeoutCannotRetryOtherCredential(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency, s.cfg.AccountConcurrencyWait = 1, 0
	base.Providers = []string{"codex", "gemini"}
	for _, id := range []string{"active", "queued"} {
		s.schedulerPick(correlatedConcurrencyRequest(s, base, id, ""))
	}
	after := pluginapi.RequestInterceptRequest{RequestID: "active", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}}
	if s.concurrencyAfter(after).Terminate {
		t.Fatal("first native execution was rejected")
	}
	after.RequestID = "queued"
	if !s.concurrencyAfter(after).Terminate {
		t.Fatal("busy native execution was admitted")
	}
	after.Metadata["selected_auth_id"] = "b"
	if !s.concurrencyAfter(after).Terminate {
		t.Fatal("expired native execution escaped to an idle credential")
	}
	status := concurrencyCount(s)
	if status.Rejected != 1 || len(status.RecentRejections) != 1 || status.RecentRejections[0].AuthID != "a" || status.RecentRejections[0].Reason != "native_credential" || status.Active != 1 || status.Waiting != 0 {
		t.Fatalf("incorrect rejection diagnostic: %+v", status)
	}
}
func TestAccountConcurrencyWarmupYieldsToQueuedForeground(t *testing.T) {
	s, _ := balancedFixture(time.Now())
	now := time.Now()
	s.concurrency.register("queued", now)
	s.concurrency.mu.Lock()
	r := s.concurrency.requests["queued"]
	r.QueueDeadline = now.Add(time.Minute)
	key := s.concurrency.keyLocked([]string{"auth:a"})
	s.concurrency.startWaitLocked(r, map[string]bool{key: true}, "a", "sticky_credential", now)
	s.concurrency.waiting++
	s.concurrency.mu.Unlock()
	candidate := warmupCandidate{Snapshot: s.quotas["a"]}
	candidate.Snapshot.AuthID = "a"
	if _, ok := s.beginWarmupConcurrency(candidate, s.cfg); ok {
		t.Fatal("warmup jumped queued foreground work")
	}
	status := concurrencyCount(s)
	if status.Accounts["a"].Waiting != 1 || status.PoolWaiting != 0 || status.Accounts["a"].Active != 0 {
		t.Fatalf("wrong bound queue status: %+v", status)
	}
	s.concurrency.endWait("queued")
	if id, ok := s.beginWarmupConcurrency(candidate, s.cfg); !ok {
		t.Fatal("finished queue still blocked warmup")
	} else {
		s.concurrency.release(id)
	}
}

func TestAccountConcurrencyExpiredQueueCannotTakeNewlyFreeSlot(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native_%t", native), func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s, base := balancedFixture(time.Now())
			s.cfg.AccountMaxConcurrency = 1
			s.cfg.AccountConcurrencyWait = time.Second
			req := correlatedConcurrencyRequest(s, base, "expired", "")
			s.concurrency.mu.Lock()
			r := s.concurrency.requests["expired"]
			r.NativeSelection = native
			r.QueueDeadline = time.Now().Add(-time.Millisecond)
			key := s.concurrency.keyLocked([]string{"auth:a"})
			s.concurrency.startWaitLocked(r, map[string]bool{key: true}, "a", "sticky_credential", time.Now().Add(-time.Second))
			s.concurrency.mu.Unlock()
			if native {
				got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "expired", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": "a"}})
				if !got.Terminate || !strings.Contains(string(got.ResponseBody), "account_concurrency_busy") {
					t.Fatal("expired native waiter took a free slot")
				}
			} else if got, err := s.schedulerPick(req); err == nil || got.Handled {
				t.Fatal("expired waiter took a free slot")
			}
			s.cfg.AccountConcurrencyEnabled = false
			if got := s.concurrencyAfter(pluginapi.RequestInterceptRequest{RequestID: "expired", ToFormat: "codex"}); !got.Terminate {
				t.Fatal("disabling protection resurrected a terminal lifecycle")
			}
			if concurrencyCount(s).Active != 0 || concurrencyCount(s).Rejected != 1 {
				t.Fatal("expired waiter consumed capacity or was recorded twice")
			}
		})
	}
}
func TestAccountConcurrencyUsableBusyCredentialQueuesDespiteExhaustedSibling(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, base := balancedFixture(time.Now())
	s.cfg.AccountMaxConcurrency = 1
	s.cfg.AccountConcurrencyWait = time.Second
	exhausted := s.quotas["b"]
	for i := range exhausted.Windows {
		exhausted.Windows[i].UsedPercent = 100
		exhausted.Windows[i].LimitReached = true
	}
	s.quotas["b"] = exhausted
	first, err := s.schedulerPick(correlatedConcurrencyRequest(s, base, "active", ""))
	if err != nil || first.AuthID != "a" {
		t.Fatalf("initial pick: %+v %v", first, err)
	}
	queued := correlatedConcurrencyRequest(s, base, "queued", "")
	done := make(chan error, 1)
	go func() {
		response, err := s.schedulerPick(queued)
		if err == nil && response.AuthID != "a" {
			err = fmt.Errorf("queue selected %q", response.AuthID)
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for concurrencyCount(s).Waiting == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if concurrencyCount(s).Waiting != 1 {
		s.concurrencyComplete("queued")
		t.Fatal("idle exhausted credential bypassed usable credential queue")
	}
	s.concurrencyComplete("active")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued request did not use released capacity")
	}
	s.concurrencyComplete("queued")
}

package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func withBalancedSession(req pluginapi.SchedulerPickRequest, session string) pluginapi.SchedulerPickRequest {
	req.Options.Metadata = map[string]any{"canonical_session_id": session, "caller_scope": "client-a"}
	return req
}

func TestBalancedSessionIdentityUsesCPACanonicalAndNativeAliases(t *testing.T) {
	_, req := balancedFixture(time.Now())
	req = withBalancedSession(req, "thread-1")
	expected := schedulerSessionHash(req)
	if len(expected) != 32 || strings.Contains(expected, "thread") {
		t.Fatal("session identity was not hashed")
	}
	req.Model = "different-model"
	req.Options.Headers = map[string][]string{"Session-Id": {"ignored"}, "X-Client-Request-Id": {"request-2"}}
	if schedulerSessionHash(req) != expected {
		t.Fatal("canonical identity changed with model or request ID")
	}
	for _, headers := range []map[string][]string{
		{"Session-Id": {"thread-1"}}, {"Session_id": {"thread-1"}}, {"Thread-Id": {"thread-1"}},
		{"Thread_id": {"thread-1"}}, {"X-Session-ID": {"thread-1"}}, {"X-Session-Affinity": {"thread-1"}},
		{"X-Codex-Turn-Metadata": {`{"session_id":"thread-1"}`}},
		{"Session-Id": {"parent"}, "X-Codex-Turn-Metadata": {`{"session_id":"parent","thread_id":"thread-1"}`}},
	} {
		req.Options.Metadata = map[string]any{"caller_scope": "client-a"}
		req.Options.Headers = headers
		if schedulerSessionHash(req) != expected {
			t.Fatalf("native alias mismatch: %v", headers)
		}
	}
	req.Options.Headers = map[string][]string{"X-Client-Request-Id": {"request-only"}}
	if schedulerSessionHash(req) != "" {
		t.Fatal("per-request ID became conversation identity")
	}
	req = withBalancedSession(req, "thread-1")
	req.Options.Metadata["caller_scope"] = "client-b"
	if schedulerSessionHash(req) == expected {
		t.Fatal("caller scopes shared a binding")
	}
	req = withBalancedSession(req, strings.Repeat("x", 4097))
	if schedulerSessionHash(req) != "" {
		t.Fatal("oversized identity accepted")
	}
}

func TestBalancedConcurrentConversationStaysWhileNewConversationsBalance(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := withBalancedSession(req, "one-conversation")
			r.Options.Headers = map[string][]string{"X-Client-Request-Id": {fmt.Sprintf("request-%d", i)}}
			got, err := s.schedulerPick(r)
			if err != nil || got.AuthID != "a" {
				t.Errorf("same conversation moved: %+v %v", got, err)
			}
		}(i)
	}
	wg.Wait()
	if s.balancedSessionHits != 199 {
		t.Fatalf("hits=%d", s.balancedSessionHits)
	}
	if got := s.balancedPick(withBalancedSession(req, "new-conversation"), time.Now()); got.AuthID != "b" {
		t.Fatal("sticky work was not charged toward fresh allocation")
	}
	s, req = balancedFixture(now)
	for i := 0; i < 200; i++ {
		s.balancedPick(withBalancedSession(req, fmt.Sprint(i)), now)
	}
	if s.balancedAccounts["a"].Picks != 100 || s.balancedAccounts["b"].Picks != 100 {
		t.Fatal("fresh conversations did not balance")
	}
}

func TestBalancedStickyIgnoresRelativeBudgetsAndSoftTiers(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "conversation")
	if s.balancedPick(req, now).AuthID != "a" {
		t.Fatal("fixture did not bind a")
	}
	s.cfg.QuotaAccountPlans = map[string]string{"b": "pro_20x"}
	s.cfg.ReserveWeeklyPercent = 20
	s.cfg.Serial5hHandoffMode = "custom_threshold"
	s.cfg.Serial5hSwitchPercent = 70
	s.quotas["a"].Windows[0].UsedPercent = 99.9
	s.quotas["a"].Windows[1].UsedPercent = 99
	for i := 0; i < 20; i++ {
		if s.balancedPick(req, now.Add(time.Second)).AuthID != "a" {
			t.Fatal("soft budget/threshold preempted a conversation")
		}
	}
	fresh := withBalancedSession(req, "new")
	if s.balancedPick(fresh, now.Add(time.Second)).AuthID != "b" {
		t.Fatal("fresh conversation did not prefer budget")
	}
}

func TestBalancedSessionFailsOverOnlyWhenUnavailableAndDoesNotBounce(t *testing.T) {
	for _, reason := range []string{"429", "5h", "weekly", "removed"} {
		t.Run(reason, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			now := time.Now()
			s, req := balancedFixture(now)
			req = withBalancedSession(req, "conversation")
			if s.balancedPick(req, now).AuthID != "a" {
				t.Fatal("fixture")
			}
			all := req.Candidates
			switch reason {
			case "429":
				banStore.set("a", banEntry{ResetAt: now.Add(time.Hour), Window: "5h"})
			case "5h":
				s.quotas["a"].Windows[0].UsedPercent = 100
			case "weekly":
				s.quotas["a"].Windows[1].UsedPercent = 100
			case "removed":
				req.Candidates = req.Candidates[1:]
			}
			if s.balancedPick(req, now).AuthID != "b" {
				t.Fatal("unavailable account retained")
			}
			resetBanStoreForTest()
			req.Candidates = all
			s.quotas["a"].Windows[0].UsedPercent = 0
			s.quotas["a"].Windows[1].UsedPercent = 0
			for i := 0; i < 10; i++ {
				if s.balancedPick(req, now).AuthID != "b" {
					t.Fatal("conversation bounced back after recovery")
				}
			}
			if s.balancedSessionSwitches != 1 {
				t.Fatalf("switches=%d", s.balancedSessionSwitches)
			}
		})
	}
}

func TestBalancedPinAndParentBindingsAreIsolated(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, base := balancedFixture(now)
	parent := withBalancedSession(base, "parent")
	s.balancedPick(parent, now)
	pin := withBalancedSession(base, "parent")
	pin.Options.Metadata["pinned_auth_id"] = "b"
	if s.balancedPick(pin, now).AuthID != "b" || s.balancedPick(parent, now).AuthID != "a" {
		t.Fatal("pin overwrote parent")
	}
	child := withBalancedSession(base, "child")
	child.Options.Metadata["parent_session_id"] = "parent"
	if s.balancedPick(child, now).AuthID != "a" {
		t.Fatal("native parent binding not inherited")
	}
	child.Candidates = child.Candidates[1:]
	if s.balancedPick(child, now).AuthID != "b" {
		t.Fatal("child did not fail over")
	}
	if s.balancedPick(parent, now).AuthID != "a" {
		t.Fatal("child failover overwrote parent")
	}
	other := withBalancedSession(base, "parent")
	other.Options.Metadata["caller_scope"] = "other-client"
	other.Candidates = other.Candidates[1:]
	s.balancedPick(other, now)
	if s.balancedPick(parent, now).AuthID != "a" {
		t.Fatal("other caller overwrote parent")
	}
}

func TestBalancedBindingExpiryLongGenerationAndLateCompletion(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "long")
	s.cfg.StickySeconds = 1
	s.balancedPick(req, now)
	key := schedulerSessionHash(req)
	if _, ok := s.balancedSessionLocked(key, now.Add(10*time.Second)); !ok {
		t.Fatal("in-flight generation expired")
	}
	record := pluginapi.UsageRecord{AuthID: "a", AuthIndex: "a", Model: req.Model, RequestedAt: now}
	s.observeBalancedUsage(record, now.Add(10*time.Second))
	if _, ok := s.balancedSessionLocked(key, now.Add(10500*time.Millisecond)); !ok {
		t.Fatal("completion did not renew idle TTL")
	}
	if _, ok := s.balancedSessionLocked(key, now.Add(12*time.Second)); ok {
		t.Fatal("idle conversation never expired")
	}
	if s.balancedPick(req, now.Add(12*time.Second)).AuthID != "b" {
		t.Fatal("expired binding prevented fresh allocation")
	}
	if s.balancedSessionSwitches != 0 {
		t.Fatal("ordinary expiry counted as forced failover")
	}
	// A delayed old-account completion must never rebind a switched conversation.
	s, req = balancedFixture(now)
	req = withBalancedSession(req, "late")
	s.balancedPick(req, now)
	req.Candidates = req.Candidates[1:]
	s.balancedPick(req, now.Add(time.Second))
	s.observeBalancedUsage(record, now.Add(5*time.Second))
	binding, _ := s.balancedSessionLocked(schedulerSessionHash(req), now.Add(5*time.Second))
	if binding.AuthID != "b" || !binding.LastUsedAt.Equal(now.Add(time.Second)) {
		t.Fatal("late completion modified replacement binding")
	}
}

func TestBalancedSessionPersistenceReloadValidationAndDisable(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "durable")
	s.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	s.initializeGenerationOwnership(s.cfg.StatePath)
	if err := s.reserveGenerationOwnership(s.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, s)
	s.balancedPick(req, now)
	restored, _ := balancedFixture(now)
	restored.cfg.StatePath = s.cfg.StatePath
	restored.loadBanState(restored.cfg.StatePath)
	if got := restored.balancedPick(req, time.Now()); got.AuthID != "a" || restored.balancedSessionHits != 1 {
		t.Fatalf("reload lost binding: %+v", got)
	}
	key := schedulerSessionHash(req)
	newer := restored.balancedSessions[key]
	newer.LastUsedAt = time.Now()
	restored.balancedSessions[key] = newer
	restored.restoreBalancedSessionsLocked(map[string]balancedSessionBinding{key: {AuthID: "b", LastUsedAt: now.Add(-time.Minute)}, "invalid": {AuthID: "b", LastUsedAt: now}, strings.Repeat("a", 32): {AuthID: "b", LastUsedAt: now.Add(-24 * time.Hour)}, strings.Repeat("b", 32): {AuthID: "b", LastUsedAt: now.Add(time.Hour)}}, time.Now())
	if len(restored.balancedSessions) != 1 || restored.balancedSessions[key].AuthID != "a" {
		t.Fatal("invalid or old persisted entries replaced current state")
	}
	restored.cfg.StickySeconds = 0
	restored.pruneBalancedSessionsLocked(time.Now())
	if len(restored.balancedSessions) != 0 {
		t.Fatal("disabled stickiness retained bindings")
	}
}

func TestBalancedSessionCacheIsBoundedAndCredentialReplacementIsFresh(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	for i := 0; i < balancedSessionLimit+10; i++ {
		s.bindBalancedSessionLocked(fmt.Sprintf("%032x", i), "a", "a", now)
	}
	if len(s.balancedSessions) != balancedSessionLimit {
		t.Fatal("unbounded session cache")
	}
	s, req = balancedFixture(now)
	req = withBalancedSession(req, "credential")
	s.balancedPick(req, now)
	q := s.quotas["a"]
	q.AuthIndex = "replacement"
	s.quotas["a"] = q
	if got := s.balancedPick(req, now); got.AuthID != "b" {
		t.Fatal("credential replacement inherited old identity binding")
	}
}

func TestBalancedNewConversationDoesNotFavorFirstAccountAtCreditCeiling(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.balancedPick(withBalancedSession(req, "long-running"), now)
	// A low-cost completion can refund an earlier, larger estimate. Both
	// accounts then reach the pick-time credit ceiling on the following call.
	cost := s.predictCostLocked(req, false)
	s.balancedAccounts["a"].Credit = cost * 16
	s.balancedAccounts["b"].Credit = cost * 8
	if got := s.balancedPick(withBalancedSession(req, "new"), now.Add(time.Second)); got.AuthID != "b" {
		t.Fatal("credit ceiling starved the unused account")
	}
}

func TestBalancedQueuedTimestampCannotInvalidateNewerSessionBinding(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "queued")
	s.balancedPick(req, now)
	s.balancedPick(req, now.Add(time.Second))
	if got := s.balancedPick(req, now.Add(time.Millisecond)); got.AuthID != "a" {
		t.Fatal("queued older timestamp invalidated the current binding")
	}
	if !s.balancedSessions[schedulerSessionHash(req)].LastUsedAt.Equal(now.Add(time.Second)) {
		t.Fatal("queued request rolled the session clock back")
	}
}

func TestBalancedLongGenerationLeaseSurvivesSnapshotReload(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "active-during-reload")
	s.cfg.StickySeconds = 1
	s.balancedPick(req, now)
	saved := s.snapshotBalancedSessionsLocked(now.Add(10 * time.Second))
	restored, _ := balancedFixture(now)
	restored.cfg.StickySeconds = 1
	restored.restoreBalancedSessionsLocked(saved, now.Add(10500*time.Millisecond))
	if _, ok := restored.balancedSessionLocked(schedulerSessionHash(req), now.Add(10500*time.Millisecond)); !ok {
		t.Fatal("reload treated active generation as expired idle time")
	}
}

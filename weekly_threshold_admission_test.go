package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestReserveRecheckedAfterSelectionAndOnNativeDispatch(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	for _, mode := range []string{"balanced", "serial"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			s, req := balancedFixture(now)
			s.cfg.SchedulerMode, s.cfg.AccountConcurrencyEnabled = mode, true
			s.identities = map[string]string{"a": "a", "b": "b"}
			// A foreground request was granted a slot while its account was healthy.
			before := s.concurrencyBefore(pluginapi.RequestInterceptRequest{RequestID: "turn"})
			req.Options.Headers = before.Headers
			pick, err := s.schedulerPick(req)
			if err != nil || pick.AuthID == "" {
				t.Fatalf("initial pick: %+v %v", pick, err)
			}
			id := pick.AuthID
			q := s.quotas[id]
			q.Windows[1].UsedPercent = 98
			s.quotas[id] = q
			after := pluginapi.RequestInterceptRequest{RequestID: "turn", ToFormat: "codex", Metadata: map[string]any{"selected_auth_id": id}}
			if got := s.concurrencyAfter(after); !got.Terminate || !strings.Contains(string(got.ResponseBody), "weekly_quota_reserved") {
				t.Fatalf("spent reserve after pick: %+v", got)
			}
			s.concurrencyComplete("turn")
			if concurrencyCount(s).Active != 0 {
				t.Fatal("undispatched slot leaked")
			}
			// Same final check for native compact/changed generation, even with
			// concurrency enforcement disabled. A healthy sibling is independent.
			s.cfg.AccountConcurrencyEnabled = false
			after.RequestID, after.ToFormat = "native", "openai-response"
			if got := s.concurrencyAfter(after); !got.Terminate {
				t.Fatal("native path spent reserve")
			}
			other := "a"
			if id == other {
				other = "b"
			}
			after.Metadata["selected_auth_id"] = other
			if got := s.concurrencyAfter(after); got.Terminate {
				t.Fatal("healthy sibling denied")
			}
			q.Windows[1].UsedPercent = 0
			q.Windows[1].ResetAt = now.Add(7 * 24 * time.Hour)
			s.quotas[id] = q
			after.Metadata["selected_auth_id"] = id
			if got := s.concurrencyAfter(after); got.Terminate {
				t.Fatal("verified new week remained blocked")
			}
		})
	}
}

func TestExplicitFiveHourReserveSurvivesLastResortAndReservations(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	for _, mode := range []string{"balanced", "serial"} {
		s, req := balancedFixture(time.Now())
		s.cfg.SchedulerMode, s.cfg.Serial5hHandoffMode, s.cfg.Reserve5hPercent = mode, "reserve_aware", 2
		s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
		if got := s.adqPolicyLocked().FiveHourSafetyMargin; got != .02 {
			t.Fatalf("atomic floor %v", got)
		}
		for _, id := range []string{"a", "b"} {
			s.quotas[id].Windows[0].UsedPercent = 98
		}
		got, err := s.schedulerPickUncapped(req)
		if err != nil || !got.Handled || !got.Reject || got.AuthID != "" {
			t.Fatalf("%s delegated configured 5h reserve: %+v %v", mode, got, err)
		}
		s.quotas["b"].Windows[0].UsedPercent = 5
		if got, err = s.schedulerPickUncapped(req); err != nil || got.AuthID != "b" {
			t.Fatalf("%s didn't select healthy sibling: %+v %v", mode, got, err)
		}
		s.cfg.Serial5hHandoffMode = "429_only"
		if got := s.adqPolicyLocked().FiveHourSafetyMargin; got != 0 {
			t.Fatalf("created implicit 5h reserve: %v", got)
		}
	}
}

func TestLateCompletionCannotReopenReserveOrUndoRefill(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now)
	q := s.quotas["a"]
	q.Windows[1].UsedPercent = 98
	w := q.Windows[1]
	lower := 96.
	p := quotaWindowPatch{Class: "weekly", UsedPercent: &lower, ResetAt: &w.ResetAt}
	for _, start := range []time.Time{now.Add(-time.Minute), now.Add(time.Second), {}} {
		if got := orderedQuotaHeaders(q, []quotaWindowPatch{p}, start, now.Add(time.Minute), s.cfg); len(got) != 0 {
			t.Fatal("same-cycle completion reopened the floor")
		}
	}
	freshReset := w.ResetAt.Add(24 * time.Hour)
	next := quotaWindowPatch{Class: "weekly", UsedPercent: &lower, ResetAt: &freshReset}
	if got := orderedQuotaHeaders(q, []quotaWindowPatch{next}, now, now.Add(time.Minute), s.cfg); len(got) != 1 {
		t.Fatal("new cycle signal suppressed")
	}
	q.Windows[1].UsedPercent = 4
	q.Windows[1].ResetAt = freshReset
	if got := orderedQuotaHeaders(q, []quotaWindowPatch{p}, now.Add(-time.Minute), now.Add(time.Minute), s.cfg); len(got) != 0 {
		t.Fatal("old cycle overwrote the refilled account")
	}
}

func TestWeeklyThresholdBlocksBalancedLastResortAndNativeFallback(t *testing.T) {
	for _, adq := range []bool{false, true} {
		t.Run(map[bool]string{false: "compatibility", true: "adq"}[adq], func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			now := time.Now()
			s, req := balancedFixture(now)
			s.cfg.ReserveWeeklyPercent = 0 // The production configuration uses the 98% handoff.
			if adq {
				s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
			}
			for _, id := range []string{"a", "b"} {
				q := s.quotas[id]
				q.Windows[1].UsedPercent = 98
				s.quotas[id] = q
			}
			got, err := s.schedulerPickUncapped(req)
			if err != nil || !got.Handled || !got.Reject || got.AuthID != "" {
				t.Fatalf("weekly threshold must reject instead of spending reserve or falling back: %+v %v", got, err)
			}
		})
	}
}

func TestNonGeneratingHandshakeAvoidsReservedCredentialWithoutCreatingUsage(t *testing.T) {
	now := time.Now()
	s, req := balancedFixture(now)
	req.Options.Metadata = map[string]any{"generate": false}
	s.quotas["a"].Windows[1].UsedPercent = 98
	got, err := s.schedulerPick(req)
	if err != nil || !got.Handled || got.AuthID != "b" || len(s.balancedAccounts) > 0 || len(s.balancedSessions) > 0 {
		t.Fatalf("handshake %+v %v", got, err)
	}
	req.Options.Metadata["pinned_auth_id"] = "a"
	if got, _ = s.schedulerPick(req); !got.Reject || got.AuthID != "" {
		t.Fatalf("pinned handshake spent reserve: %+v", got)
	}
}

func TestBalancedNearFloorReservesGuardCostOnlyOnConstrainedCredential(t *testing.T) {
	s, req := expiryPriorityFixture(t, "balanced", true)
	q := s.quotas["five-sooner"]
	q.Windows[1].UsedPercent = 95
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	got, err := s.schedulerPickUncapped(req)
	if err != nil || got.AuthID != "five-later" {
		t.Fatalf("spent headroom that only fits the usual estimate: %+v %v", got, err)
	}
	// Healthy selection remains on its ordinary estimate, without charging
	// every credential the guard estimate of the one that was declined.
	if s.adqLastDecision.ReservationWeekly != s.predictCostLocked(req, false) {
		t.Fatalf("healthy reservation inflated: %+v", s.adqLastDecision)
	}
}

func TestWeeklyThresholdDoesNotMakeADQPanelAccountEligible(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now)
	s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
	q := s.quotas["a"]
	q.Windows[1].UsedPercent = 98
	s.quotas["a"] = q
	for _, account := range s.status().Snapshots {
		if account.AuthID == "a" && account.ADQ != nil && account.ADQ.Eligible {
			t.Fatalf("ADQ advertised a threshold-blocked account as eligible: %+v", account.ADQ)
		}
	}
}

func TestWeeklyThresholdRoutesToHealthyWeeklyOnlyBackup(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial"} {
		for _, remaining := range []float64{2, 1, 0} {
			t.Run(fmt.Sprintf("%s/remaining-%g", mode, remaining), func(t *testing.T) {
				s, req := expiryPriorityFixture(t, mode, mode == "balanced")
				for _, id := range []string{"five-sooner", "five-later"} {
					q := s.quotas[id]
					q.Windows[1].UsedPercent = 100 - remaining
					s.quotas[id], s.quotaNative[id] = q, q
				}
				got, err := s.schedulerPickUncapped(req)
				if err != nil || got.AuthID != "weekly-backup" {
					t.Fatalf("weekly handoff must precede 5h preference: %+v %v", got, err)
				}
			})
		}
	}
}

func TestWeeklyThresholdPreservesExplicitContinuationAndUnknownFallback(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	req = withBalancedSession(req, "existing")
	if got := s.balancedPick(req, now); got.AuthID != "a" {
		t.Fatal("fixture did not bind the original account")
	}
	s.cfg.SerialSoftContinuation = true
	q := s.quotas["a"]
	q.Windows[1].UsedPercent = 98
	s.quotas["a"] = q
	if got := s.balancedPick(req, now); got.AuthID != "a" {
		t.Fatalf("explicit existing-session continuation changed: %+v", got)
	}
	if got := s.balancedPick(withBalancedSession(req, "new"), now); got.AuthID != "b" {
		t.Fatalf("new session consumed the protected weekly quota: %+v", got)
	}
	s.cfg.SerialSoftContinuation = false
	req.Candidates = append(req.Candidates[:1:1], pluginapi.SchedulerAuthCandidate{ID: "unknown", Provider: providerCodex})
	if s.allCandidatesReserveBlocked(req, now) != "" {
		t.Fatal("unknown quota was turned into a confirmed weekly block")
	}
}

func TestWeeklyThresholdADQReservationPreservesConfiguredHeadroom(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now)
	s.cfg.ReserveWeeklyPercent = 0
	if got := s.adqPolicyLocked().WeeklySafetyMargin; got < .02 || got > .02000001 {
		t.Fatalf("ADQ did not retain the 98 percent handoff headroom: %v", got)
	}
	// Five-hour zero-reserve mode remains independent of weekly protection.
	if got := s.adqPolicyLocked().FiveHourSafetyMargin; got != 0 {
		t.Fatalf("weekly handoff created an implicit 5h reserve: %v", got)
	}
	s.cfg.SerialSwitchPercent = 100
	if got := s.adqPolicyLocked().WeeklySafetyMargin; got >= .02 {
		t.Fatalf("disabled weekly threshold remained in the reservation policy: %v", got)
	}
}

func TestWeeklyThresholdADQDenialCannotDelegateBelowReserve(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
	for _, id := range []string{"a", "b"} {
		q := s.quotas[id]
		q.Windows[1].UsedPercent = 97.9
		s.quotas[id] = q
	}
	got, err := s.schedulerPickUncapped(req)
	if err != nil || !got.Handled || !got.Reject || got.AuthID != "" {
		t.Fatalf("insufficient spendable quota escaped atomic admission: %+v %v", got, err)
	}
	if len(s.adqReservations.Snapshot(now)) != 0 {
		t.Fatal("rejected work retained a quota reservation")
	}
}

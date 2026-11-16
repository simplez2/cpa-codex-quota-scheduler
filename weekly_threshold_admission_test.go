package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

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
	if s.allCandidatesWeeklyHandoffBlocked(req, now) {
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

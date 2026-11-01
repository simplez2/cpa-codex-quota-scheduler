package main

import (
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestWeeklyOnlyAffinityYieldsWhenFiveHourQuotaReturns(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial", "enforce"} {
		for _, plan := range []string{"pro", "self_serve_business_prolite"} {
			t.Run(mode+"/"+plan, func(t *testing.T) {
				s, req := expiryPriorityFixture(t, mode, mode == "balanced")
				s.cfg.SerialSoftContinuation = true
				weekly := s.quotas["weekly-backup"]
				weekly.Plan = quotaPlanObservation{Type: plan, Source: "cpa_usage", ObservedAt: weekly.RefreshedAt}
				s.quotas[weekly.AuthID], s.quotaNative[weekly.AuthID] = weekly, weekly
				usable := s.quotas["five-sooner"]
				exhaustPriorityFixture(s, "five-sooner", "5h")
				exhaustPriorityFixture(s, "five-later", "5h")
				req = withBalancedSession(req, "weekly-fallback-conversation")
				first, err := s.schedulerPickUncapped(req)
				if err != nil || !first.Handled || first.AuthID != "weekly-backup" {
					t.Fatalf("weekly fallback unavailable: %+v %v", first, err)
				}
				s.quotas[usable.AuthID], s.quotaNative[usable.AuthID] = usable, usable
				got, err := s.schedulerPickUncapped(req)
				if err != nil || !got.Handled || got.AuthID != "five-sooner" {
					t.Fatalf("weekly-only affinity bypassed usable 5h: %+v %v", got, err)
				}
				if mode != "enforce" {
					binding, ok := s.balancedSessionLocked(schedulerSessionHash(req), time.Now())
					if !ok || binding.AuthID != "five-sooner" {
						t.Fatalf("fallback binding was not replaced: %+v", binding)
					}
				}
			})
		}
	}
}

func TestWeeklyOnlyBindingDoesNotQueueAheadOfUsableFiveHour(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial"} {
		t.Run(mode, func(t *testing.T) {
			s, req := expiryPriorityFixture(t, mode, false)
			req = withBalancedSession(req, "old-weekly-binding")
			s.bindBalancedSessionLocked(schedulerSessionHash(req), "weekly-backup", "weekly-backup", time.Now())
			if got := s.concurrencyPreferred(req, s.cfg, time.Now()); got != "" {
				t.Fatalf("queue still waits for weekly-only affinity: %q", got)
			}
			req.Options.Metadata = map[string]any{"pinned_auth_id": "weekly-backup"}
			if got := s.concurrencyPreferred(req, s.cfg, time.Now()); got != "weekly-backup" {
				t.Fatalf("explicit CPA credential pin lost: %q", got)
			}
		})
	}
}

func TestWeeklyOnlyADQReservesOnlyWeeklyAndNeverInventsFiveHourQuota(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req := expiryPriorityFixture(t, "balanced", true)
	now := time.Now()
	weekly := s.quotas["weekly-backup"]
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: weekly.AuthID}, weekly, true, s.cfg, now)
	input, ok := s.adqInputForChoiceLocked(choice, now)
	if !ok || !input.WeeklyOnly || input.FiveHourCapacity != 0 || input.FiveHourRemaining != 0 {
		t.Fatalf("confirmed weekly-only quota fell out of atomic admission: %+v, ok=%v", input, ok)
	}
	id, reservation, ok, handled := s.adqRouteChoicesWithAllocationLocked(req, []serialCandidate{choice}, 1, now, "", "", true)
	if !ok || !handled || id != weekly.AuthID || reservation.Weekly != 1 || reservation.FiveHour != 0 {
		t.Fatalf("weekly-only debit not scoped to weekly quota: id=%s reservation=%+v ok=%v handled=%v", id, reservation, ok, handled)
	}
	if s.adqLastDecision.Metrics.FullWidthContribution != 0 {
		t.Fatal("weekly-only backup became a phantom 5h source")
	}
	s.adqReservations.Settle(reservation.ID, .5, .5, weekly, now, time.Minute)
	if five, week := s.adqReservations.Settling(weekly.AuthID, now); five != 0 || week != .5 {
		t.Fatalf("settlement fabricated a 5h debit: five=%v week=%v", five, week)
	}
	s.adqReservations.Release(reservation.ID)
	_, _, ok, handled = s.adqRouteChoicesWithAllocationLocked(req, []serialCandidate{choice}, 1000, now, "", "", true)
	if ok || !handled {
		t.Fatal("weekly over-reservation fell back to unreserved native selection")
	}
}

func TestWeeklyOnlyUnknownAbsenceCannotEnterUnlimitedAdmission(t *testing.T) {
	s, _ := expiryPriorityFixture(t, "balanced", true)
	now := time.Now()
	weekly := s.quotas["weekly-backup"]
	for _, kind := range []string{"unknown", "stale"} {
		q := weekly
		if kind == "unknown" {
			q.FiveHourLimitStatus, q.FiveHourLimitObservedAt = "unknown", time.Time{}
		} else {
			q.FiveHourLimitObservedAt = now.Add(-2 * s.cfg.StaleAfter)
		}
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: q.AuthID}, q, true, s.cfg, now)
		if _, ok := s.adqInputForChoiceLocked(choice, now); ok {
			t.Fatalf("%s absence inferred as no 5h limit", kind)
		}
	}
}

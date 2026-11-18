package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func urgentWeeklyFixture(t *testing.T) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest, time.Time) {
	t.Helper()
	s, req, now := weightedResetFixture(t, true)
	s.cfg.StickySeconds = 14400
	s.cfg.WeeklyOnlyPriorityWindow = 24 * time.Hour
	s.cfg.ReserveWeeklyPercent = 2
	for i, item := range []struct {
		id   string
		used float64
	}{{"weekly-backup", 6}, {"weekly-second", 40}, {"weekly-reserved", 98}} {
		q := s.quotas["weekly-backup"]
		q.AuthID, q.AuthIndex = item.id, item.id
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		q.Windows[0].UsedPercent = item.used
		q.Windows[0].ResetAt = now.Add(13*time.Hour - time.Duration(i)*time.Minute)
		s.quotas[item.id], s.quotaNative[item.id], s.identities[item.id] = q, q, item.id
		if i != 0 {
			req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: item.id, Provider: providerCodex})
		}
	}
	return s, req, now
}

func TestBalancedPlacesNewSessionsOnUrgentWeeklyWithoutMovingLiveBinding(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := urgentWeeklyFixture(t)
	main := withBalancedSession(req, "main-existing")
	main.Candidates = main.Candidates[1:2]
	if got := s.balancedPick(main, now); got.AuthID != "five-later" {
		t.Fatalf("could not bind the existing session: %+v", got)
	}
	counts := make(map[string]int)
	for i := 0; i < 8; i++ {
		fresh := withBalancedSession(req, fmt.Sprintf("fresh-%d", i))
		pick := s.balancedPick(fresh, now.Add(time.Duration(i+1)*time.Second))
		if pick.AuthID != "weekly-backup" && pick.AuthID != "weekly-second" {
			t.Fatalf("new session skipped urgent reservable quotas: %+v", pick)
		}
		decision := s.adqLastDecision
		if decision.ReservationFiveHour != 0 || decision.ReservationWeekly <= 0 {
			t.Fatalf("urgent selection missed atomic weekly debit: %+v", decision)
		}
		counts[pick.AuthID]++
	}
	if len(counts) != 2 {
		t.Fatalf("one of the urgent credentials was starved: %v", counts)
	}
	main.Candidates = req.Candidates
	if got := s.balancedPick(main, now.Add(9*time.Second)); got.AuthID != "five-later" || s.balancedSessionSwitches != 0 {
		t.Fatalf("deadline preempted the usable existing conversation: %+v", got)
	}
	pin := withBalancedSession(req, "pin")
	pin.Options.Metadata["pinned_auth_id"] = "five-later"
	if got := s.balancedPick(pin, now.Add(10*time.Second)); got.AuthID != "five-later" {
		t.Fatalf("deadline overrode the CPA pin: %+v", got)
	}
}

func TestBalancedUrgentWeeklyFallsBackWithoutCrossingAdmissionBoundaries(t *testing.T) {
	for _, state := range []string{"unreservable", "unreservable_without_alternative", "stale", "outside", "window_elapsed", "absent_unconfirmed", "not_in_host_candidates"} {
		t.Run(state, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s, req, now := urgentWeeklyFixture(t)
			s.cfg.WeeklyOnlyCatchupSharePercent = 0
			for _, id := range []string{"weekly-backup", "weekly-second"} {
				q := s.quotas[id]
				switch state {
				case "unreservable", "unreservable_without_alternative":
					q.Windows[0].UsedPercent = 97.999
				case "stale":
					q.Windows[0].ObservedAt = now.Add(-2 * s.cfg.StaleAfter)
				case "outside":
					q.Windows[0].ResetAt = now.Add(24*time.Hour + time.Second)
				case "window_elapsed":
					q.Windows[0].ResetAt = now.Add(-time.Second)
				case "absent_unconfirmed":
					q.FiveHourLimitStatus = "unknown"
				}
				s.quotas[id], s.quotaNative[id] = q, q
			}
			if state == "not_in_host_candidates" {
				req.Candidates = req.Candidates[1:3]
			}
			if state == "unreservable_without_alternative" {
				req.Candidates = append(req.Candidates[:1:1], req.Candidates[3])
			}
			pick := s.balancedPick(withBalancedSession(req, "fresh"), now)
			if state == "unreservable_without_alternative" {
				if !pick.Handled || !pick.Reject || pick.RejectCode != "quota_reservation_unavailable" {
					t.Fatalf("cannot fall through to unreserved native routing: %+v", pick)
				}
			} else if pick.AuthID != "five-sooner" && pick.AuthID != "five-later" {
				t.Fatalf("%s promoted an unavailable weekly offer: %+v", state, pick)
			}
		})
	}
}

func TestUrgentWeeklyEndpointUsesConfiguredPriorityAndForegroundRefresh(t *testing.T) {
	s, req, now := urgentWeeklyFixture(t)
	s.cfg.WeeklyOnlyCatchupSharePercent = 0
	q := s.quotas["weekly-backup"]
	weeklyChoice := inspectSerialCandidate(req.Candidates[0], q, true, s.cfg, now)
	five := inspectSerialCandidate(req.Candidates[1], s.quotas["five-later"], true, s.cfg, now)
	if weeklyChoice.Priority.WeeklyCatchup || !weeklyChoice.Priority.WeeklyUrgent ||
		compareQuotaPlacementPriority(weeklyChoice.Priority, five.Priority) >= 0 ||
		quotaPriorityStatus(weeklyChoice, s.cfg, now).Reason != "weekly_only_urgent" {
		t.Fatalf("status disagrees with the urgent placement: %+v", weeklyChoice.Priority)
	}
	q.RefreshedAt, q.FiveHourLimitObservedAt = now.Add(-20*time.Minute), now.Add(-20*time.Minute)
	q.Windows[0].ObservedAt = q.RefreshedAt
	s.quotas[q.AuthID] = q
	s.quotaPolls = map[string]quotaPollState{q.AuthID: {AuthIndex: q.AuthIndex, AttemptedAt: q.RefreshedAt, NextAt: now.Add(10 * time.Minute)}}
	s.requestWeeklyCatchupProbesLocked(now)
	if got := s.quotaProbeReason(q.AuthID, now); got != "weekly_catchup" {
		t.Fatalf("priority-only mode did not refresh on demand: %q", got)
	}
	for _, v := range []string{"-1h", "30m", "25h", "bad"} {
		if _, err := parsePluginConfig([]byte("weekly_only_priority_window: " + v)); err == nil {
			t.Fatalf("accepted invalid priority window %s", v)
		}
	}
	changes, errors := validatePanelSettings(nil, map[string]any{"weekly_only_priority_window": "24h"})
	if len(errors) != 0 || changes["weekly_only_priority_window"] != "24h0m0s" {
		t.Fatalf("invalid panel round trip: %v %v", changes, errors)
	}
}

package main

import (
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func weightedResetFixture(t *testing.T, adq bool) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest, time.Time) {
	s, req := expiryPriorityFixture(t, "balanced", adq)
	s.cfg.BalancedPriorityPolicy = "reset_weighted"
	s.cfg.StickySeconds = 0
	now := time.Now()
	for _, id := range []string{"five-sooner", "five-later", "weekly-backup"} {
		q := s.quotas[id]
		q.RefreshedAt = now
		q.FiveHourLimitObservedAt = now
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		for i := range q.Windows {
			q.Windows[i].ObservedAt = now
			q.Windows[i].UsedPercent = 20
			q.Windows[i].ResetAt = now.Add(3 * time.Hour)
			if q.Windows[i].Class == "weekly" {
				q.Windows[i].ResetAt = now.Add(6 * 24 * time.Hour)
				if id == "five-later" || id == "weekly-backup" {
					q.Windows[i].ResetAt = now.Add(24 * time.Hour)
				}
			}
		}
		s.quotas[id], s.quotaNative[id] = q, q
	}
	return s, req, now
}

func TestBalancedResetWeightedAllocatesNearWeeklyResetWithoutHoarding(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, adq := range []bool{false, true} {
		s, req, now := weightedResetFixture(t, adq)
		counts := map[string]int{}
		for i := 0; i < 600; i++ {
			got := s.balancedPick(req, now.Add(time.Duration(i)*time.Millisecond))
			if !got.Handled || got.Reject {
				t.Fatalf("adq=%v request=%d got=%+v", adq, i, got)
			}
			counts[got.AuthID]++
			if adq {
				s.adqReservations.Release(s.adqLastDecision.ReservationID)
				s.balancedAccounts[got.AuthID].Pending = nil // Model completed test work, without telemetry lag.
			}
		}
		if counts["five-later"] <= counts["five-sooner"] || counts["five-sooner"] < 100 {
			t.Fatalf("adq=%v near weekly reset did not receive bounded preference: %v", adq, counts)
		}
		if n := counts["weekly-backup"]; n < 30 || n > 100 {
			t.Fatalf("adq=%v weekly catch-up was hoarded or exceeded the 15%% target: %v", adq, counts)
		}
		t.Logf("adq=%v allocation=%v", adq, counts)
	}
}

func TestBalancedResetWeightedWeeklyIsBackupOutsideCatchupAndFullFallback(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := weightedResetFixture(t, true)
	q := s.quotas["weekly-backup"]
	q.Windows[0].ResetAt = now.Add(72 * time.Hour)
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	for i := 0; i < 100; i++ {
		got := s.balancedPick(req, now)
		if got.Reject || got.AuthID == "weekly-backup" {
			t.Fatalf("non-urgent weekly reserve displaced 5h: %+v", got)
		}
		s.adqReservations.Release(s.adqLastDecision.ReservationID)
		s.balancedAccounts[got.AuthID].Pending = nil
	}
	for _, id := range []string{"five-sooner", "five-later"} {
		exhaustPriorityFixture(s, id, "5h")
	}
	if got := s.balancedPick(req, now); got.Reject || got.AuthID != "weekly-backup" {
		t.Fatalf("weekly-only full fallback lost: %+v", got)
	}
}

func TestBalancedResetWeightedPreservesWeeklyGateAndRequiresConfirmedReset(t *testing.T) {
	s, req, now := weightedResetFixture(t, false)
	s.cfg.ReserveWeeklyPercent, s.cfg.SerialHandoffMode = 2, "reserve_aware"
	for _, kind := range []string{"reserved", "unknown_absence", "expired_reset", "placeholder"} {
		q := s.quotas["weekly-backup"]
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		switch kind {
		case "reserved":
			q.Windows[0].UsedPercent = 98
		case "unknown_absence":
			q.FiveHourLimitStatus, q.FiveHourLimitObservedAt = "unknown", time.Time{}
		case "expired_reset":
			q.Windows[0].ResetAt = now.Add(-time.Second)
		case "placeholder":
			q.Windows[0].UsedPercent = 0
			q.Windows[0].ResetAt = now.Add(7 * 24 * time.Hour)
		}
		choice := inspectSerialCandidate(req.Candidates[0], q, true, s.cfg, now)
		if choice.Priority.WeeklyCatchup {
			t.Fatalf("%s invented catch-up eligibility: %+v", kind, choice.Priority)
		}
	}
}

func TestBalancedCatchupShareIsCombinedAndFallbackNotCapped(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SchedulerMode, cfg.BalancedPriorityPolicy = "balanced", "reset_weighted"
	weights := map[string]float64{"five": 1, "week1": 20, "week2": 10}
	weekly := map[string]bool{"week1": true, "week2": true}
	capBalancedCatchupWeights(weights, weekly, cfg)
	share := (weights["week1"] + weights["week2"]) / (weights["five"] + weights["week1"] + weights["week2"])
	if share < .14999 || share > .15001 {
		t.Fatalf("combined share=%v weights=%v", share, weights)
	}
	weights = map[string]float64{"week1": 20, "week2": 10}
	capBalancedCatchupWeights(weights, weekly, cfg)
	if weights["week1"] != 20 || weights["week2"] != 10 {
		t.Fatal("weekly-only fallback was throttled without usable 5h")
	}
}

func TestBalancedResetPriorityConfigValidation(t *testing.T) {
	for _, raw := range []string{"balanced_priority_policy: invalid", "weekly_catchup_window: 30m", "weekly_catchup_window: 169h", "weekly_only_catchup_share_percent: 41", "weekly_only_catchup_share_percent: -.1"} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	cfg, err := parsePluginConfig([]byte("scheduler_mode: balanced\nbalanced_priority_policy: reset_weighted\nweekly_catchup_window: 48h\nweekly_only_catchup_share_percent: 15\n"))
	if err != nil || !balancedResetWeighted(cfg) || cfg.WeeklyCatchupWindow != 48*time.Hour || cfg.WeeklyOnlyCatchupSharePercent != 15 {
		t.Fatalf("valid settings rejected: %+v %v", cfg, err)
	}
}

func TestBalancedCatchupSessionAffinitySurvivesNewFiveHourCandidates(t *testing.T) {
	s, req, now := weightedResetFixture(t, false)
	s.cfg.StickySeconds = 14400
	choice := inspectSerialCandidate(req.Candidates[0], s.quotas["weekly-backup"], true, s.cfg, now)
	if !choice.Priority.WeeklyCatchup {
		t.Fatal("fixture did not enter catch-up")
	}
	if quotaAffinityMustYield(choice, []serialCandidate{inspectSerialCandidate(req.Candidates[1], s.quotas["five-later"], true, s.cfg, now)}, s.cfg) {
		t.Fatal("permitted catch-up conversation was forced to migrate")
	}
	changes, fields := validatePanelSettings(nil, map[string]any{"balanced_priority_policy": "reset_weighted", "weekly_catchup_window": "48h", "weekly_only_catchup_share_percent": 15.})
	if len(fields) != 0 || changes["weekly_catchup_window"] != "48h0m0s" {
		t.Fatalf("panel cannot validate catch-up settings: %v %v", changes, fields)
	}
}

func warmWeeklyFallbackFixture(t *testing.T) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest, time.Time) {
	t.Helper()
	s, req, now := weightedResetFixture(t, true)
	s.cfg.StickySeconds = 14400
	s.cfg.StaleAfter = 15 * time.Minute
	s.cfg.ReserveWeeklyPercent = 2
	q := s.quotas["weekly-backup"]
	q.Windows[0].ResetAt = now.Add(72 * time.Hour)
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	backupOnly := withBalancedSession(req, "weekly-fallback")
	backupOnly.Candidates = backupOnly.Candidates[:1]
	if got := s.balancedPick(backupOnly, now); got.AuthID != q.AuthID {
		t.Fatalf("could not place fallback session: %+v", got)
	}
	return s, withBalancedSession(req, "weekly-fallback"), now
}

func TestBalancedWarmWeeklyHandbackDefersOnlyActiveFallbackAndDoesNotSlide(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, now := warmWeeklyFallbackFixture(t)
	firstReturn := now.Add(time.Minute)
	key := schedulerSessionHash(req)

	fresh := withBalancedSession(req, "new-conversation")
	if got := s.balancedPick(fresh, firstReturn); got.AuthID == "" || got.AuthID == "weekly-backup" {
		t.Fatalf("warm handback diverted a new conversation from 5h: %+v", got)
	}
	child := withBalancedSession(req, "child")
	child.Options.Metadata["parent_session_id"] = "weekly-fallback"
	if got := s.balancedPick(child, firstReturn); got.AuthID == "" || got.AuthID == "weekly-backup" {
		t.Fatalf("a child inherited the backup's warm handback: %+v", got)
	}
	for _, offset := range []time.Duration{0, 3 * time.Minute, 4 * time.Minute} {
		at := firstReturn.Add(offset)
		if got := s.balancedPick(req, at); got.AuthID != "weekly-backup" {
			t.Fatalf("warm continuation at %v was moved: %+v", offset, got)
		}
		decision := s.adqLastDecision
		if decision.ReservationFiveHour != 0 || decision.ReservationWeekly <= 0 {
			t.Fatalf("warm continuation did not go through weekly-only reservation: %+v", decision)
		}
		want := firstReturn.Add(balancedWeeklyHandbackGrace)
		if deadline := s.balancedSessions[key].WeeklyHandbackBy; !deadline.Equal(want) {
			t.Fatalf("continuation slid its handback deadline: got %v want %v", deadline, want)
		}
	}
	if s.balancedWeeklyDeferrals != 3 {
		t.Fatalf("deferral counter=%d", s.balancedWeeklyDeferrals)
	}
	if got := s.balancedPick(req, firstReturn.Add(balancedWeeklyHandbackGrace)); got.AuthID == "" || got.AuthID == "weekly-backup" {
		t.Fatalf("backup continued after the deadline: %+v", got)
	}
	if binding := s.balancedSessions[key]; !binding.WeeklyHandbackBy.IsZero() || s.balancedSessionSwitches != 1 {
		t.Fatalf("handback left an old deadline or rebound repeatedly: %+v", binding)
	}
}

func TestBalancedWarmWeeklyHandbackNeverOverridesQuotaEvidence(t *testing.T) {
	for _, why := range []string{"stale", "reserve", "unreservable", "idle", "busy", "index_replaced"} {
		t.Run(why, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s, req, now := warmWeeklyFallbackFixture(t)
			at := now.Add(time.Minute)
			q := s.quotas["weekly-backup"]
			switch why {
			case "stale":
				q.FiveHourLimitObservedAt = now.Add(-2 * s.cfg.StaleAfter)
			case "reserve":
				q.Windows[0].UsedPercent = 98
			case "unreservable":
				q.Windows[0].UsedPercent = 97.999
			case "idle":
				at = now.Add(balancedWeeklyHandbackGrace)
			case "busy":
				// The normal queue removes a busy weekly backup when other 5h
				// options are available. Handback cannot reinstate that candidate.
				if got := s.concurrencyPreferred(req, s.cfg, at); got != "" {
					t.Fatalf("optional backup handback queued ahead of 5h: %q", got)
				}
				req.Candidates = req.Candidates[1:]
			case "index_replaced":
				q.AuthIndex = "new-index"
			}
			s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
			got := s.balancedPick(req, at)
			if got.AuthID == "" || got.AuthID == q.AuthID || s.balancedWeeklyDeferrals != 0 {
				t.Fatalf("route bypassed %s: %+v, deferrals %d", why, got, s.balancedWeeklyDeferrals)
			}
		})
	}
}

func TestBalancedADQDoesNotResurrectAffinityForReplacedCredential(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.cfg.BalancedPriorityPolicy = "reset_weighted"
	s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
	s.balancedAccounts = map[string]*balancedAccount{"b": {Credit: 1, LastSeen: now}}
	req = withBalancedSession(req, "same-session")
	s.bindBalancedSessionLocked(schedulerSessionHash(req), "a", "retired-index", now)
	if got := s.balancedPick(req, now); got.AuthID != "b" {
		t.Fatalf("ADQ restored the binding for a replaced credential: %+v", got)
	}
}

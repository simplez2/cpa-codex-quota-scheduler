package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func expiryPriorityFixture(t *testing.T, mode string, adq bool) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest) {
	t.Helper()
	now := time.Now().UTC()
	cfg := defaultPluginConfig()
	cfg.SchedulerMode, cfg.StatePath = mode, ""
	s := &schedulerRuntimeState{cfg: cfg, quotas: map[string]quotaSnapshot{}, quotaNative: map[string]quotaSnapshot{}, identities: map[string]string{}}
	if adq {
		s.adqPolicy, s.adqReservations = defaultADQPolicy(), newADQReservationBook()
	}
	req := pluginapi.SchedulerPickRequest{Provider: providerCodex, Model: "gpt-5.6-sol"}
	// A larger weekly budget and more local cycle generations must never
	// override the actual 5h deadline. The weekly-only account resets first.
	for _, item := range []struct {
		id                             string
		fiveMinutes                    int
		fiveRemaining, weeklyRemaining float64
	}{{"weekly-backup", 0, 0, 99}, {"five-later", 180, 92, 93}, {"five-sooner", 40, 82, 49}} {
		primary := "null"
		if item.fiveMinutes > 0 {
			primary = fmt.Sprintf(`{"used_percent":%g,"limit_window_seconds":18000,"reset_at":%d}`, 100-item.fiveRemaining, now.Add(time.Duration(item.fiveMinutes)*time.Minute).Unix())
		}
		weeklyReset := now.Add(6 * 24 * time.Hour)
		if item.fiveMinutes == 0 {
			weeklyReset = now.Add(time.Hour)
		}
		raw := fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":%s,"secondary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_at":%d}}}`, primary, 100-item.weeklyRemaining, weeklyReset.Unix())
		q, err := parseNativeQuota([]byte(raw), item.id, item.id, now)
		if err != nil {
			t.Fatal(err)
		}
		s.quotas[item.id], s.quotaNative[item.id], s.identities[item.id] = q, q, item.id
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: item.id, Provider: providerCodex})
	}
	return s, req
}

func exhaustPriorityFixture(s *schedulerRuntimeState, id, class string) {
	q := s.quotas[id]
	q.Windows = append([]quotaWindow(nil), q.Windows...)
	for i := range q.Windows {
		if q.Windows[i].Class == class {
			q.Windows[i].UsedPercent, q.Windows[i].LimitReached = 100, true
		}
	}
	s.quotas[id], s.quotaNative[id] = q, q
}

func TestQuotaPriorityAllModesSpendExpiringFiveHourBeforeWeeklyBackup(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial", "legacy", "shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			s, req := expiryPriorityFixture(t, mode, mode == "balanced")
			for i, want := range []string{"five-sooner", "five-later", "weekly-backup"} {
				r := withBalancedSession(req, fmt.Sprintf("new-%d", i))
				got, err := s.schedulerPickUncapped(r)
				if err != nil || !got.Handled || got.AuthID != want {
					t.Fatalf("step %d got=%+v err=%v want=%s", i, got, err, want)
				}
				if want != "weekly-backup" {
					exhaustPriorityFixture(s, want, "5h")
				}
			}
		})
	}
}

func TestQuotaPriorityDemandCachedFiveHourStillPrecedesFreshWeeklyOnly(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial", "legacy", "shadow", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			s, req := expiryPriorityFixture(t, mode, mode == "balanced")
			for _, id := range []string{"five-sooner", "five-later"} {
				q := s.quotas[id]
				q.RefreshedAt = q.RefreshedAt.Add(-30 * time.Minute)
				q.FiveHourLimitObservedAt = q.RefreshedAt
				for i := range q.Windows {
					q.Windows[i].ObservedAt = q.RefreshedAt
				}
				s.quotas[id], s.quotaNative[id] = q, q
			}
			got, err := s.schedulerPickUncapped(req)
			if err != nil || got.AuthID != "five-sooner" {
				t.Fatalf("idle cache inverted quota pool order: %+v %v", got, err)
			}
		})
	}
}

func TestQuotaPriorityWeeklyReserveCannotSpendBackupBeforeFiveHour(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial"} {
		t.Run(mode, func(t *testing.T) {
			s, req := expiryPriorityFixture(t, mode, mode == "balanced")
			q := s.quotas["five-sooner"]
			q.Windows[1].UsedPercent = 95 // Within the soft reserve, still hard-eligible.
			s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
			got, err := s.schedulerPickUncapped(req)
			if err != nil || got.AuthID != "five-sooner" {
				t.Fatalf("weekly reserve replaced expiring 5h: %+v %v", got, err)
			}
			exhaustPriorityFixture(s, "five-sooner", "weekly")
			got, err = s.schedulerPickUncapped(req)
			if err != nil || got.AuthID != "five-later" {
				t.Fatalf("weekly hard limit bypassed: %+v %v", got, err)
			}
		})
	}
}

func TestQuotaPriorityBusyAccountUsesNextFiveHourBeforeWeekly(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req := expiryPriorityFixture(t, "balanced", true)
	s.cfg.AccountMaxConcurrency, s.cfg.AccountConcurrencyWait = 1, 0
	defer func() {
		for _, id := range []string{"r0", "r1", "r2", "same"} {
			s.concurrency.release(id)
		}
	}()
	for i, want := range []string{"five-sooner", "five-later", "weekly-backup"} {
		got, err := s.schedulerPick(correlatedConcurrencyRequest(s, req, fmt.Sprintf("r%d", i), fmt.Sprintf("conversation-%d", i)))
		if err != nil || got.AuthID != want {
			t.Fatalf("busy sequence %d: %+v %v want=%s", i, got, err, want)
		}
	}
	if _, err := s.schedulerPick(correlatedConcurrencyRequest(s, req, "same", "conversation-0")); err == nil {
		t.Fatal("busy binding silently migrated")
	}
}

func TestQuotaPriorityReservationFailureAdvancesWithoutBypassingGate(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req := expiryPriorityFixture(t, "balanced", true)
	q := s.quotas["five-sooner"]
	q.Windows[0].UsedPercent = 99.99 // Positive official headroom, smaller than a request reservation.
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	got := s.balancedPick(req, time.Now())
	if got.AuthID != "five-later" || s.adqDecisions != 1 {
		t.Fatalf("reservation gate bypassed: %+v decisions=%d", got, s.adqDecisions)
	}
	if s.balancedAccounts["five-sooner"].Credit != 0 || s.balancedAccounts["five-sooner"].Picks != 0 {
		t.Fatal("rejected priority group minted credit or committed work")
	}
	if len(s.adqReservations.Snapshot(time.Now())) != 1 {
		t.Fatal("accepted group did not reserve exactly once")
	}
	q = s.quotas["five-later"]
	q.Windows[0].UsedPercent = 99.99
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	got = s.balancedPick(withBalancedSession(req, "fallback-session"), time.Now())
	if got.AuthID != "weekly-backup" {
		t.Fatalf("weekly fallback unavailable after all 5h admission failed: %+v", got)
	}
}

func TestQuotaPriorityWeeklyBackupAffinityCannotBypassFiveHour(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"balanced", "serial"} {
		t.Run(mode, func(t *testing.T) {
			s, req := expiryPriorityFixture(t, mode, mode == "balanced")
			s.bindBalancedSessionLocked(schedulerSessionHash(withBalancedSession(req, "existing")), "weekly-backup", "weekly-backup", time.Now())
			for _, tc := range []struct{ session, want string }{{"existing", "five-sooner"}, {"new", "five-sooner"}, {"existing", "five-sooner"}} {
				got, err := s.schedulerPickUncapped(withBalancedSession(req, tc.session))
				if err != nil || got.AuthID != tc.want {
					t.Fatalf("session=%s got=%+v err=%v want=%s", tc.session, got, err, tc.want)
				}
			}
			pinned := req
			pinned.Options.Metadata = map[string]any{"pinned_auth_id": "weekly-backup"}
			if got, err := s.schedulerPickUncapped(pinned); err != nil || got.AuthID != "weekly-backup" {
				t.Fatalf("explicit CPA account pin was ignored: %+v %v", got, err)
			}
		})
	}
}

func TestQuotaPriorityPlaceholderAndLocalGenerationDoNotInventUrgency(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req := expiryPriorityFixture(t, "balanced", false)
	now := time.Now()
	q := s.quotas["five-later"]
	q.Windows[0].UsedPercent, q.Windows[0].ResetAt = 0, q.RefreshedAt.Add(5*time.Hour)
	s.quotas[q.AuthID], s.quotaNative[q.AuthID] = q, q
	s.fiveHourPhases = map[string]fiveHourPhaseState{"five-later": {AuthID: "five-later", AuthIndex: "five-later", Cycle: 100}, "five-sooner": {AuthID: "five-sooner", AuthIndex: "five-sooner", Cycle: 1}}
	if got := s.balancedPick(req, now); got.AuthID != "five-sooner" {
		t.Fatalf("generation or placeholder invented priority: %+v", got)
	}
	status := s.status()
	for _, item := range status.Snapshots {
		if item.AuthID == "weekly-backup" && (item.FiveHourLimitStatus != "not_reported" || item.SelectionPriority.Reason != "weekly_only_backup") {
			t.Fatalf("weekly-only misclassified: %+v", item)
		}
		if item.AuthID == "five-later" && item.SelectionPriority.Reason != "five_hour_dormant" {
			t.Fatalf("placeholder got deadline priority: %+v", item)
		}
	}
	partial := quotaSnapshot{AuthID: "partial", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(24 * time.Hour)}}}
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "partial"}, partial, true, s.cfg, now)
	if got := quotaPriorityStatus(choice, s.cfg, now); got.Reason != "five_hour_unconfirmed" {
		t.Fatalf("partial was called weekly-only: %+v", got)
	}
}

func TestQuotaResetCreditsDistinguishesUnknownZeroAndPositive(t *testing.T) {
	now := time.Now().UTC()
	base := `"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}`
	for _, tc := range []struct {
		extra string
		known bool
		count int
	}{{"", false, 0}, {`,"rate_limit_reset_credits_available_count":null`, false, 0}, {`,"rate_limit_reset_credits_available_count":-1`, false, 0}, {`,"rate_limit_reset_credits_available_count":0`, true, 0}, {`,"rate_limit_reset_credits_available_count":3`, true, 3}} {
		q, err := parseNativeQuota([]byte("{"+base+tc.extra+"}"), "auth", "index", now)
		if err != nil || q.ResetCreditsKnown != tc.known || q.ResetCredits != tc.count {
			t.Fatalf("extra=%s q=%+v err=%v", tc.extra, q, err)
		}
		if tc.known && !q.ResetCreditsObservedAt.Equal(now) {
			t.Fatal("count timestamp not anchored to observation")
		}
		data, err := json.Marshal(q)
		if err != nil {
			t.Fatal(err)
		}
		var restored quotaSnapshot
		if err := json.Unmarshal(data, &restored); err != nil || restored.ResetCreditsKnown != tc.known || restored.ResetCredits != tc.count {
			t.Fatalf("count evidence lost on persistence: %+v %v", restored, err)
		}
	}
}

func TestQuotaResetCreditsOlderHeaderCannotOverwriteNewNativeCount(t *testing.T) {
	now := time.Now().UTC()
	previous := quotaSnapshot{AuthID: "a", AuthIndex: "i", RefreshedAt: now, ResetCredits: 3, ResetCreditsKnown: true, ResetCreditsObservedAt: now.Add(-time.Minute)}
	current := quotaSnapshot{AuthID: "a", AuthIndex: "i", RefreshedAt: now.Add(-time.Second), ResetCredits: 0, ResetCreditsKnown: true, ResetCreditsObservedAt: now.Add(-time.Second)}
	got := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	if got.ResetCredits != 0 || !got.ResetCreditsKnown || !got.ResetCreditsObservedAt.Equal(current.ResetCreditsObservedAt) {
		t.Fatalf("late header resurrected spent credits: %+v", got)
	}
	current.ResetCreditsKnown = false
	current.ResetCreditsObservedAt = time.Time{}
	if got = mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute); got.ResetCreditsKnown {
		t.Fatal("missing newer count inferred as known")
	}
}

func TestFiveHourResetObservationsDoNotCountElapsedUnobservedCycles(t *testing.T) {
	now := time.Now().UTC()
	s := schedulerRuntimeState{cfg: defaultPluginConfig()}
	first := phaseSnapshot("account", now, now.Add(40*time.Minute), 10)
	s.observeFiveHourPhaseLocked(first, now)
	if got := fiveHourPhaseStatus(s.fiveHourPhases["account"], now); got.ObservedResets != 0 {
		t.Fatalf("first sighting counted as a reset: %+v", got)
	}
	next := first
	next.RefreshedAt = now.Add(20 * time.Hour)
	next.Windows = append([]quotaWindow(nil), first.Windows...)
	next.Windows[0].ObservedAt, next.Windows[0].ResetAt = next.RefreshedAt, next.RefreshedAt.Add(5*time.Hour)
	next.Windows[1].ObservedAt = next.RefreshedAt
	s.observeFiveHourPhaseLocked(next, next.RefreshedAt)
	got := fiveHourPhaseStatus(s.fiveHourPhases["account"], next.RefreshedAt)
	if got.Cycle != 2 || got.ObservedResets != 1 {
		t.Fatalf("missed windows were invented as observed resets: %+v", got)
	}
	next.Windows[1].ResetAt = now.Add(7 * 24 * time.Hour)
	s.observeFiveHourPhaseLocked(next, next.RefreshedAt)
	if got := fiveHourPhaseStatus(s.fiveHourPhases["account"], next.RefreshedAt); got.ObservedResets != 0 {
		t.Fatalf("new weekly epoch retained an old local count: %+v", got)
	}
}

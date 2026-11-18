package main

import (
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const weeklyMigrationSource = "five-sooner"

func weeklyMigrationFixture(t *testing.T) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest, time.Time) {
	t.Helper()
	s, base, start := urgentWeeklyFixture(t)
	s.cfg.StaleAfter = 2 * time.Hour // Let the test advance past the cache bands.
	s.cfg.WeeklyOnlyCatchupSharePercent = 0
	req := withBalancedSession(base, "known-session")
	for _, candidate := range req.Candidates {
		if candidate.ID != weeklyMigrationSource {
			continue
		}
		seed := req
		seed.Candidates = []pluginapi.SchedulerAuthCandidate{candidate}
		if got := s.balancedPick(seed, start); got.Reject || got.AuthID != weeklyMigrationSource {
			t.Fatalf("could not seed previously bound 5h session: %+v", got)
		}
		s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: candidate.ID, AuthIndex: candidate.ID, Model: req.Model, RequestedAt: start}, start.Add(time.Second))
		return s, req, start.Add(time.Second)
	}
	t.Fatal("missing source candidate")
	return nil, req, start
}

func migrationTarget(id string) bool { return id == "weekly-backup" || id == "weekly-second" }

func TestBalancedColdWeeklyHandoffPreservesWarmThenKeepsNewBinding(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, completedAt := weeklyMigrationFixture(t)
	horizon := 50 * time.Minute
	// The high-value cache band still protects the binding, even though the
	// final weekly candidates already outrank it for new work.
	warmAt := completedAt.Add(horizon)
	if got := s.concurrencyPreferred(req, s.cfg, warmAt); got != weeklyMigrationSource {
		t.Fatalf("warm preflight ignored cache affinity: %q", got)
	}
	if got := s.balancedPick(req, warmAt); got.AuthID != weeklyMigrationSource {
		t.Fatalf("warm turn migrated: %+v", got)
	}
	s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: weeklyMigrationSource, AuthIndex: weeklyMigrationSource, Model: req.Model, RequestedAt: warmAt}, warmAt.Add(time.Second))

	coldAt := warmAt.Add(horizon + 2*time.Second)
	if got := s.concurrencyPreferred(req, s.cfg, coldAt); got != "" {
		t.Fatalf("cold binding still blocked the weekly lane at preflight: %q", got)
	}
	moved := s.balancedPick(req, coldAt)
	if moved.Reject || !migrationTarget(moved.AuthID) || s.balancedWeeklyColdMigrations != 1 {
		t.Fatalf("cold turn did not migrate: %+v; migrations=%d", moved, s.balancedWeeklyColdMigrations)
	}
	if d := s.adqLastDecision; d.ReservationWeekly <= 0 || d.ReservationFiveHour != 0 {
		t.Fatalf("weekly handoff missed its quota reservation: %+v", d)
	}
	// A late completion on the old credential cannot change the newly persisted
	// session binding. Every subsequent request offers both 5h credentials.
	s.observeBalancedUsage(pluginapi.UsageRecord{AuthID: weeklyMigrationSource, AuthIndex: weeklyMigrationSource, Model: req.Model, RequestedAt: warmAt}, coldAt.Add(time.Second))
	for _, offset := range []time.Duration{2 * time.Second, 3 * time.Second, 10 * time.Minute} {
		if got := s.balancedPick(req, coldAt.Add(offset)); got.Reject || got.AuthID != moved.AuthID {
			t.Fatalf("available 5h traffic stole the weekly binding: %+v", got)
		}
	}
	if s.balancedSessionSwitches != 1 {
		t.Fatalf("expected one credential change, got %d", s.balancedSessionSwitches)
	}
}

func setMigrationWindow(s *schedulerRuntimeState, id, class string, change func(*quotaWindow)) {
	q := s.quotas[id]
	q.Windows = append([]quotaWindow(nil), q.Windows...)
	for i := range q.Windows {
		if q.Windows[i].Class == class {
			change(&q.Windows[i])
		}
	}
	s.quotas[id], s.quotaNative[id] = q, q
}

func TestBalancedUrgentWeeklyFailoverDoesNotBounceWhenFiveHourRecovers(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, completedAt := weeklyMigrationFixture(t)
	setMigrationWindow(s, weeklyMigrationSource, "5h", func(w *quotaWindow) { w.UsedPercent = 100 })
	moved := s.balancedPick(req, completedAt.Add(time.Second))
	if moved.Reject || !migrationTarget(moved.AuthID) || s.balancedWeeklyColdMigrations != 0 {
		t.Fatalf("5h exhaustion did not use the urgent weekly lane: %+v", moved)
	}
	setMigrationWindow(s, weeklyMigrationSource, "5h", func(w *quotaWindow) {
		w.UsedPercent = 20
		w.ObservedAt = completedAt.Add(2 * time.Second)
		w.ResetAt = w.ObservedAt.Add(5 * time.Hour)
	})
	if got := s.balancedPick(req, completedAt.Add(2*time.Second)); got.Reject || got.AuthID != moved.AuthID {
		t.Fatalf("5h refill forced a bounce: %+v", got)
	}
	setMigrationWindow(s, moved.AuthID, "weekly", func(w *quotaWindow) { w.UsedPercent = 98 })
	if got := s.balancedPick(req, completedAt.Add(3*time.Second)); got.Reject || !migrationTarget(got.AuthID) || got.AuthID == moved.AuthID {
		t.Fatalf("weekly reserve did not move to the remaining urgent credential: %+v", got)
	}
}

func TestBalancedColdWeeklyHandoffRequiresSafeTurnAndSpendableTarget(t *testing.T) {
	for _, obstruction := range []string{"pinned", "running", "weekly_source_also_urgent", "not_reservable", "stale"} {
		t.Run(obstruction, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			s, req, completedAt := weeklyMigrationFixture(t)
			now := completedAt.Add(time.Hour)
			switch obstruction {
			case "pinned":
				req.Options.Metadata["pinned_auth_id"] = weeklyMigrationSource
			case "running":
				s.concurrency.register("still-running", completedAt)
				live := s.concurrency.requests["still-running"]
				live.SessionKey, live.AuthID, live.Key = schedulerSessionHash(req), weeklyMigrationSource, weeklyMigrationSource
			case "weekly_source_also_urgent":
				setMigrationWindow(s, weeklyMigrationSource, "weekly", func(w *quotaWindow) { w.ResetAt = now.Add(12 * time.Hour) })
			case "not_reservable":
				for _, id := range []string{"weekly-backup", "weekly-second"} {
					setMigrationWindow(s, id, "weekly", func(w *quotaWindow) { w.UsedPercent = 97.9999 })
				}
			case "stale":
				for _, id := range []string{"weekly-backup", "weekly-second"} {
					q := s.quotas[id]
					q.FiveHourLimitObservedAt = now.Add(-3 * time.Hour)
					s.quotas[id], s.quotaNative[id] = q, q
				}
			}
			if got := s.concurrencyPreferred(req, s.cfg, now); got != weeklyMigrationSource {
				t.Fatalf("%s: preferred source lost before admission: %q", obstruction, got)
			}
			if got := s.balancedPick(req, now); got.Reject || got.AuthID != weeklyMigrationSource || s.balancedWeeklyColdMigrations != 0 {
				t.Fatalf("%s: unsafe elective move: %+v", obstruction, got)
			}
		})
	}
}

func TestConcurrencyColdWeeklyHandoffKeepsOldQueueIfWeeklySlotsBusy(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	s, req, completedAt := weeklyMigrationFixture(t)
	now := completedAt.Add(time.Hour)
	s.cfg.AccountConcurrencyEnabled, s.cfg.AccountMaxConcurrency = true, 1
	for _, c := range req.Candidates {
		if !migrationTarget(c.ID) {
			continue
		}
		s.concurrency.register(c.ID, now)
		s.concurrency.requests[c.ID].Key = s.concurrency.keyLocked(s.concurrencyAliases(c))
	}
	if got := s.concurrencyPreferred(req, s.cfg, now); got != weeklyMigrationSource {
		t.Fatalf("all weekly slots occupied, removed the usable 5h affinity: %q", got)
	}
	s.concurrency.release("weekly-second")
	if got := s.concurrencyPreferred(req, s.cfg, now); got != "" {
		t.Fatalf("available weekly sibling could not pass preflight: %q", got)
	}
}

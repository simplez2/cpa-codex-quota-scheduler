package main

import (
	"testing"
	"time"
)

func TestNewWeeklyCycleClearsOldFiveHourPhaseDelay(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: map[string]quotaSnapshot{}, fiveHourPhases: map[string]fiveHourPhaseState{}}
	q := phaseSnapshot("a", now, now.Add(5*time.Hour), 0)
	q.Windows[1].UsedPercent = 0
	q.Windows[1].ResetAt = now.Add(7 * 24 * time.Hour)
	s.quotas["a"] = q
	s.fiveHourPhases["a"] = fiveHourPhaseState{AuthID: "a", AuthIndex: "a", Cycle: 5, ActiveResetAt: now.Add(-time.Minute), PlannedResetAt: now.Add(-time.Minute), ActivateAt: now.Add(4 * time.Hour)}
	window, ok := unstartedWarmupWindow(q, now)
	if !ok || window.Class != "weekly" {
		t.Fatalf("new week classified as idle 5h: %+v", window)
	}
	planned := s.planFiveHourWarmups([]warmupCandidate{{Snapshot: q, Window: window}}, now)
	if !planned[0].ActivateAt.IsZero() || !s.fiveHourPhases["a"].ActivateAt.IsZero() {
		t.Fatal("weekly bootstrap was delayed four hours")
	}
	// A confirmed new maximum-period epoch also bypasses a stale short-window plan.
	s.fiveHourPhases["a"] = fiveHourPhaseState{AuthID: "a", AuthIndex: "a", Cycle: 5, ActiveResetAt: now.Add(-time.Minute), PlannedResetAt: now.Add(-time.Minute), ActivateAt: now.Add(4 * time.Hour)}
	q.Windows[1].UsedPercent = 10
	planned = s.planFiveHourWarmups([]warmupCandidate{{Snapshot: q, Window: q.Windows[0], EpochID: "new-week"}}, now)
	if !planned[0].ActivateAt.IsZero() || !s.fiveHourPhases["a"].ActivateAt.IsZero() {
		t.Fatal("epoch admission retained an idle phase delay")
	}
}

func TestEpochFollowupBypassesStandbyCooldownButRetainsFailureBackoff(t *testing.T) {
	now := time.Now()
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotaPolls: map[string]quotaPollState{"a": {NextAt: now.Add(30 * time.Minute)}}, quotaEpoch: quotaEpochState{ID: "new-week", Reason: "bootstrap", Accounts: map[string]quotaEpochAccountState{"a": {AuthID: "a", State: quotaEpochAccountPending, ObservedAt: now.Add(-time.Hour), LastProbeAt: now.Add(-time.Minute), ProbeAttempts: 1}}}}
	if reason := s.quotaProbeReason("a", now); reason != "epoch_pending" {
		t.Fatalf("30m cooldown stalled epoch: %q", reason)
	}
	poll := s.quotaPolls["a"]
	poll.Failures = 1
	s.quotaPolls["a"] = poll
	if reason := s.quotaProbeReason("a", now); reason != "" {
		t.Fatalf("failure backoff bypassed: %q", reason)
	}
}

package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSerialLiveSessionOutranksChangingGlobalBudget(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s := newSerialTestState(now)
	s.cfg.StickySeconds = 21600
	first := withBalancedSession(serialTestRequest(), "cached-session")
	if got := s.serialPick(first, now); got.AuthID != "primary" {
		t.Fatalf("initial=%+v", got)
	}
	q := s.quotas["backup"]
	q.Windows[0].UsedPercent = 0
	s.quotas["backup"] = q
	second := withBalancedSession(serialTestRequest(), "new-session")
	if got := s.serialPick(second, now.Add(time.Minute)); got.AuthID != "backup" {
		t.Fatalf("new session did not re-rank=%+v", got)
	}
	for _, offset := range []time.Duration{2 * time.Minute, 31 * time.Minute, 2 * time.Hour} {
		if got := s.serialPick(first, now.Add(offset)); got.AuthID != "primary" {
			t.Fatalf("priority broke sticky at %v: %+v", offset, got)
		}
	}
	// A new reset cycle is also only a ranking change, never a conversation migration.
	q = s.quotas["primary"]
	q.RefreshedAt = now.Add(2 * time.Hour)
	q.Windows[0].ObservedAt = q.RefreshedAt
	s.quotas["primary"] = q
	if got := s.serialPick(first, now.Add(2*time.Hour+time.Second)); got.AuthID != "primary" {
		t.Fatalf("reset broke sticky: %+v", got)
	}
}

func TestSerialBindingSurvivesStateReloadAndHonorsThresholdNearReset(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s := newSerialTestState(now)
	s.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	s.initializeGenerationOwnership(s.cfg.StatePath)
	if err := s.reserveGenerationOwnership(s.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &s)
	s.cfg.StickySeconds = 21600
	s.cfg.Serial5hHandoffMode, s.cfg.Serial5hSwitchPercent = "custom_threshold", 80
	for _, id := range []string{"primary", "backup"} {
		q := s.quotas[id]
		q.AuthIndex = id
		q.Windows = append(q.Windows, quotaWindow{Class: "5h", WindowSeconds: 18000, UsedPercent: 10, Allowed: true, ResetAt: now.Add(13 * time.Minute), ObservedAt: now})
		s.quotas[id] = q
	}
	req := withBalancedSession(serialTestRequest(), "persistent-session")
	if got := s.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("initial=%+v", got)
	}
	s.serialActiveAuthID = "backup"
	if !s.persistBanState() {
		t.Fatal("persist failed")
	}
	restored := schedulerRuntimeState{cfg: s.cfg}
	restored.loadBanState(restored.cfg.StatePath)
	if got := restored.serialPick(req, now.Add(time.Minute)); got.AuthID != "primary" {
		t.Fatalf("reload lost binding=%+v", got)
	}
	q := restored.quotas["primary"]
	q.Windows[1].UsedPercent = 80
	restored.quotas["primary"] = q
	if got := restored.serialPick(req, now.Add(2*time.Minute)); got.AuthID != "backup" {
		t.Fatalf("near-reset ignored 80 percent threshold=%+v", got)
	}
}

func TestBalancedConfiguredThresholdEndsStickyWithoutHardLimit(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	s.cfg.Serial5hHandoffMode, s.cfg.Serial5hSwitchPercent = "custom_threshold", 80
	req = withBalancedSession(req, "threshold-session")
	first := s.balancedPick(req, now)
	q := s.quotas[first.AuthID]
	q.Windows[0].UsedPercent = 80
	s.quotas[first.AuthID] = q
	got := s.balancedPick(req, now.Add(time.Second))
	if !got.Handled || got.AuthID == first.AuthID {
		t.Fatalf("configured threshold did not hand off=%+v", got)
	}
}

func TestFiveHourDeadlinePriorityDoesNotOverrideWeeklyTier(t *testing.T) {
	now := time.Now()
	s, req := balancedFixture(now)
	for _, policy := range []string{"sustainable", "weekly_remaining"} {
		s.cfg.SerialAllocationPolicy = policy
		q := s.quotas["b"]
		q.Windows[0].ResetAt = now.Add(20 * time.Minute)
		s.quotas["b"] = q
		choices := []serialCandidate{}
		for _, candidate := range req.Candidates {
			choice := inspectSerialCandidate(candidate, s.quotas[candidate.ID], true, s.cfg, now)
			s.annotateSerialCandidateLocked(&choice, now)
			choices = append(choices, choice)
		}
		sortSerialCandidates(choices, s.cfg)
		if choices[0].Candidate.ID != "b" {
			t.Fatalf("%s ignored nearest 5h reset: %+v", policy, choices)
		}
		if balancedWeight(choices[0], s.cfg, now) <= balancedWeight(choices[1], s.cfg, now) {
			t.Fatalf("%s balanced weight ignored expiry", policy)
		}
	}
	s.cfg.SerialAllocationPolicy = "weekly_remaining"
	a := s.quotas["a"]
	a.Windows[1].UsedPercent = 0
	s.quotas["a"] = a
	choices := []serialCandidate{}
	for _, candidate := range req.Candidates {
		choice := inspectSerialCandidate(candidate, s.quotas[candidate.ID], true, s.cfg, now)
		s.annotateSerialCandidateLocked(&choice, now)
		choices = append(choices, choice)
	}
	sortSerialCandidates(choices, s.cfg)
	if choices[0].Candidate.ID != "a" {
		t.Fatal("5h expiry overrode material weekly imbalance")
	}
}

func TestSerialConcurrencyQueuesForSessionBindingAndRenewsLongStream(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s := newSerialTestState(now)
	s.cfg.StickySeconds = 60
	s.serialActiveAuthID = "backup"
	req := withBalancedSession(serialTestRequest(), "long-stream")
	key := schedulerSessionHash(req)
	s.bindBalancedSessionLocked(key, "primary", "", now.Add(-3*time.Hour))
	s.concurrency.register("live", now.Add(-3*time.Hour))
	s.concurrency.requests["live"].SessionKey = key
	s.concurrency.requests["live"].AuthID = "primary"
	s.concurrency.requests["live"].Key = "auth:primary"
	s.concurrency.requests["live"].Dispatched = true
	if got := s.concurrencyPreferred(req, s.cfg, now); got != "primary" {
		t.Fatalf("queue targeted global primary rather than live session=%s", got)
	}
	if got := s.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("long stream binding expired=%+v", got)
	}
	s.concurrencyComplete("live")
	if binding := s.balancedSessions[key]; now.Sub(binding.LastUsedAt) > time.Second {
		t.Fatal("completion did not renew binding")
	}
}

package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func phaseSnapshot(id string, now, reset time.Time, used float64) quotaSnapshot {
	return quotaSnapshot{AuthID: id, AuthIndex: id, RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", WindowSeconds: 18000, UsedPercent: used, Allowed: true, ResetAt: reset, ObservedAt: now},
		{Class: "weekly", WindowSeconds: 604800, UsedPercent: 10, Allowed: true, ResetAt: now.Truncate(24 * time.Hour).Add(6 * 24 * time.Hour), ObservedAt: now},
	}}
}

func TestPhaseCapabilityRequiresDelayedRequestAndDormantEvidence(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	for _, mode := range []string{fiveHourPhaseFirstUse, fiveHourPhaseFixed, "no_dormant_evidence", "stale_dormant_evidence"} {
		t.Run(mode, func(t *testing.T) {
			s := schedulerRuntimeState{cfg: defaultPluginConfig()}
			reset := now.Add(-time.Hour)
			first := phaseSnapshot("a", reset.Add(-time.Hour), reset, 15)
			s.observeFiveHourPhaseLocked(first, reset.Add(-time.Hour))
			activation := reset.Add(40 * time.Minute)
			if mode != "no_dormant_evidence" {
				dormantAt := activation.Add(-time.Minute)
				if mode == "stale_dormant_evidence" {
					dormantAt = activation.Add(-10 * time.Minute)
				}
				dormant := phaseSnapshot("a", dormantAt, dormantAt.Add(5*time.Hour), 0)
				s.observeFiveHourPhaseLocked(dormant, activation)
			}
			s.noteFiveHourActivationLocked("a", "a", activation)
			nextReset := activation.Add(5 * time.Hour)
			if mode == fiveHourPhaseFixed {
				nextReset = reset.Add(5 * time.Hour)
			}
			next := phaseSnapshot("a", activation.Add(time.Minute), nextReset, 1)
			s.observeFiveHourPhaseLocked(next, activation.Add(time.Minute))
			phase := s.fiveHourPhases["a"]
			expected := mode
			if mode == "no_dormant_evidence" || mode == "stale_dormant_evidence" {
				expected = fiveHourPhaseUnknown
			}
			if phase.AnchorMode != expected || phase.Cycle != 2 || phase.ActiveResetAt != nextReset {
				t.Fatalf("phase=%+v expected=%s", phase, expected)
			}
		})
	}
}

func TestPhaseSecondGenerationPlansSpreadAcrossFiveHoursAndAreDurable(t *testing.T) {
	now := time.Now().Truncate(fiveHourPhaseWindow).Add(time.Second)
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: map[string]quotaSnapshot{}}
	s.cfg.StatePath = filepath.Join(t.TempDir(), "state.json")
	s.initializeGenerationOwnership(s.cfg.StatePath)
	if err := s.reserveGenerationOwnership(s.cfg.StatePath); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &s)
	candidates := []warmupCandidate{}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("a%d", i)
		reset := now.Add(-time.Second)
		s.observeFiveHourPhaseLocked(phaseSnapshot(id, reset.Add(-time.Hour), reset, 50), reset.Add(-time.Hour))
		q := phaseSnapshot(id, now, now.Add(5*time.Hour), 0)
		s.quotas[id] = q
		candidates = append(candidates, warmupCandidate{Snapshot: q, Window: q.Windows[0]})
	}
	planned := s.planFiveHourWarmups(candidates, now)
	slots := map[int]bool{}
	immediate := 0
	for _, candidate := range planned {
		if candidate.ActivateAt.Before(now) || !candidate.ActivateAt.Before(now.Add(5*time.Hour)) {
			t.Fatalf("outside horizon=%+v", candidate)
		}
		if !candidate.ActivateAt.After(now) {
			immediate++
		}
		bucket := phaseBucket(candidate.ActivateAt)
		if slots[bucket] {
			t.Fatal("repeated phase slot")
		}
		slots[bucket] = true
	}
	if immediate != 1 {
		t.Fatalf("immediate capacity=%d", immediate)
	}
	if !s.persistBanState() {
		t.Fatal("phase state was not persisted")
	}
	original := cloneFiveHourPhases(s.fiveHourPhases)
	restored := schedulerRuntimeState{cfg: s.cfg}
	restored.loadBanState(restored.cfg.StatePath)
	retried := restored.planFiveHourWarmups(candidates, now.Add(time.Minute))
	for _, candidate := range retried {
		if !candidate.ActivateAt.Equal(original[candidate.Snapshot.AuthID].ActivateAt) {
			t.Fatal("reload moved a persisted idle plan")
		}
	}
	// No deferred candidate can be sent early, including the admission recheck.
	restored.warmups = map[string]warmupEntry{}
	for _, candidate := range retried {
		if candidate.ActivateAt.After(now.Add(time.Minute)) && restored.warmupCandidateStillEligible(candidate, now.Add(time.Minute)) {
			t.Fatal("deferred warmup dispatched early")
		}
	}
}

func TestPhaseFirstGenerationAndWeeklyActivationAreNeverDelayed(t *testing.T) {
	now := time.Now()
	s := schedulerRuntimeState{cfg: defaultPluginConfig(), quotas: map[string]quotaSnapshot{}}
	q := phaseSnapshot("a", now, now.Add(5*time.Hour), 0)
	s.quotas["a"] = q
	planned := s.planFiveHourWarmups([]warmupCandidate{{Snapshot: q, Window: q.Windows[0]}}, now)
	if !planned[0].ActivateAt.IsZero() {
		t.Fatal("first generation was delayed")
	}
	s.fiveHourPhases["a"] = fiveHourPhaseState{AuthID: "a", AuthIndex: "a", Cycle: 1, AnchorMode: fiveHourPhaseUnknown, ActiveResetAt: now.Add(-time.Minute), PlannedResetAt: now.Add(-time.Minute), ActivateAt: now.Add(time.Hour)}
	planned = s.planFiveHourWarmups([]warmupCandidate{{Snapshot: q, Window: quotaWindow{Class: "weekly"}}}, now)
	if !planned[0].ActivateAt.IsZero() || !s.fiveHourPhases["a"].ActivateAt.IsZero() {
		t.Fatal("weekly activation retained phase delay")
	}
}

func TestPhaseFixedWindowDoesNotClaimReanchoringAndNaturalCallsBypassDelay(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedFixture(now)
	q := phaseSnapshot("a", now, now.Add(5*time.Hour), 0)
	s.quotas["a"] = q
	phase := fiveHourPhaseState{AuthID: "a", AuthIndex: "a", Cycle: 1, ActiveResetAt: now.Add(-time.Minute), AnchorMode: fiveHourPhaseFixed}
	s.fiveHourPhases = map[string]fiveHourPhaseState{"a": phase}
	planned := s.planFiveHourWarmups([]warmupCandidate{{Snapshot: q, Window: q.Windows[0]}}, now)
	if !planned[0].ActivateAt.IsZero() {
		t.Fatal("fixed provider window was artificially delayed")
	}
	phase.AnchorMode = fiveHourPhaseUnknown
	phase.ActivateAt = now.Add(3 * time.Hour)
	phase.PlannedResetAt = phase.ActiveResetAt
	s.fiveHourPhases["a"] = phase
	req.Candidates = req.Candidates[:1]
	if got := s.balancedPick(withBalancedSession(req, "natural"), now); !got.Handled || got.AuthID != "a" {
		t.Fatalf("natural request waited for phase=%+v", got)
	}
}

func TestPhaseCredentialReplacementAndProviderResetDiscardOldSchedule(t *testing.T) {
	now := time.Now()
	s := schedulerRuntimeState{cfg: defaultPluginConfig()}
	q := phaseSnapshot("a", now, now.Add(time.Hour), 10)
	s.observeFiveHourPhaseLocked(q, now)
	phase := s.fiveHourPhases["a"]
	phase.ActivateAt = now.Add(2 * time.Hour)
	phase.PlannedResetAt = now.Add(-time.Hour)
	s.fiveHourPhases["a"] = phase
	q.AuthIndex = "replacement"
	s.observeFiveHourPhaseLocked(q, now)
	if got := s.fiveHourPhases["a"]; got.Cycle != 1 || !got.ActivateAt.IsZero() || got.AuthIndex != "replacement" {
		t.Fatalf("old credential plan leaked=%+v", got)
	}
	q.Windows[0].ResetAt = now.Add(4 * time.Hour)
	q.Windows[0].ObservedAt = now.Add(time.Second)
	s.observeFiveHourPhaseLocked(q, now.Add(time.Second))
	if got := s.fiveHourPhases["a"]; got.Cycle != 1 || got.AnchorMode != fiveHourPhaseUnknown {
		t.Fatalf("provider reset falsely reanchored=%+v", got)
	}
}

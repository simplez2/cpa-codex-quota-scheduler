package main

import (
	"testing"
	"time"
)

func quotaEpochTestSnapshot(authID, authIndex string, used float64, observedAt, resetAt time.Time, placeholder bool) quotaSnapshot {
	after := int64(resetAt.Sub(observedAt).Seconds())
	if placeholder {
		after = int64((7 * 24 * time.Hour).Seconds())
	}
	return quotaSnapshot{
		AuthID: authID, AuthIndex: authIndex, RefreshedAt: observedAt,
		Windows: []quotaWindow{{
			Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			ResetAfterSeconds: after, ResetAfterSecondsKnown: true,
			UsedPercent: used, Allowed: true, ResetAt: resetAt,
			ObservedAt: observedAt, Source: quotaSourceProbe,
		}},
	}
}

func TestQuotaEpochDormantPlaceholderDoesNotLookLikeProviderReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previousAt := now.Add(-time.Minute)
	previous := quotaEpochTestSnapshot("a", "idx-a", 0, previousAt, previousAt.Add(7*24*time.Hour), true)
	current := quotaEpochTestSnapshot("a", "idx-a", 0, now, now.Add(7*24*time.Hour), true)
	if reason, reset := quotaEpochResetEvidence(previous, current, now); reset {
		t.Fatalf("moving dormant placeholder detected as reset: reason=%q", reason)
	}
}

func TestQuotaEpochRequiresQuorumAndCreatesOneIdempotentEpoch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		quotas:     make(map[string]quotaSnapshot),
		quotaPolls: make(map[string]quotaPollState),
		quotaEpoch: newQuotaEpochState(),
		warmups:    make(map[string]warmupEntry),
	}
	inventory := make(map[string]cpaAuthFileEntry)
	previous := make(map[string]quotaSnapshot)
	current := make(map[string]quotaSnapshot)
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		index := "idx-" + id
		inventory[id] = cpaAuthFileEntry{ID: id, AuthIndex: index}
		previous[id] = quotaEpochTestSnapshot(id, index, 70, now.Add(-2*time.Minute), now.Add(-time.Second), false)
		current[id] = quotaEpochTestSnapshot(id, index, 0, now, now.Add(7*24*time.Hour), true)
		state.quotas[id] = previous[id]
		state.quotas[index] = previous[id]
	}

	for _, id := range []string{"a", "b"} {
		state.quotas[id] = current[id]
		if transition := state.observeQuotaEpochProbeLocked(id, previous[id], current[id], inventory, now); transition.ID != "" {
			t.Fatalf("epoch confirmed before quorum: %#v", transition)
		}
	}
	state.quotas["c"] = current["c"]
	transition := state.observeQuotaEpochProbeLocked("c", previous["c"], current["c"], inventory, now)
	if transition.ID == "" || transition.Reason != "provider_reset" {
		t.Fatalf("quorum did not confirm provider reset: %#v", transition)
	}
	if state.quotaEpoch.Sequence != 1 || state.quotaEpoch.ID != transition.ID || len(state.quotaEpoch.Accounts) != 5 {
		t.Fatalf("unexpected epoch state after quorum: %#v", state.quotaEpoch)
	}
	for id, account := range state.quotaEpoch.Accounts {
		if account.State != quotaEpochAccountPending {
			t.Fatalf("account %s state=%q want pending", id, account.State)
		}
	}
	if duplicate := state.observeQuotaEpochProbeLocked("c", current["c"], current["c"], inventory, now.Add(time.Second)); duplicate.ID != "" {
		t.Fatalf("same observation created duplicate epoch: %#v", duplicate)
	}
	if state.quotaEpoch.Sequence != 1 {
		t.Fatalf("duplicate observation advanced sequence to %d", state.quotaEpoch.Sequence)
	}
}

func TestQuotaEpochResetSweepRetriesLaggedTelemetryAndKeepsOldSnapshotsPending(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		quotas:     make(map[string]quotaSnapshot),
		quotaPolls: make(map[string]quotaPollState),
		quotaEpoch: newQuotaEpochState(),
		warmups:    make(map[string]warmupEntry),
	}
	inventory := make(map[string]cpaAuthFileEntry)
	previous := make(map[string]quotaSnapshot)
	reset := make(map[string]quotaSnapshot)
	for i := 0; i < 5; i++ {
		id := string(rune('a' + i))
		index := "idx-" + id
		inventory[id] = cpaAuthFileEntry{ID: id, AuthIndex: index}
		previous[id] = quotaEpochTestSnapshot(id, index, 70, now.Add(-2*time.Minute), now.Add(24*time.Hour), false)
		reset[id] = quotaEpochTestSnapshot(id, index, 0, now, now.Add(7*24*time.Hour), true)
		state.quotas[id] = previous[id]
		state.quotas[index] = previous[id]
	}

	state.quotas["a"] = reset["a"]
	if transition := state.observeQuotaEpochProbeLocked("a", previous["a"], reset["a"], inventory, now); transition.ID != "" {
		t.Fatalf("single reset signal confirmed epoch: %#v", transition)
	}
	for offset, id := range []string{"b", "c", "d", "e"} {
		at := now.Add(time.Duration(offset+1) * time.Second)
		lagged := quotaEpochTestSnapshot(id, "idx-"+id, 70, at, now.Add(24*time.Hour), false)
		state.recordQuotaEpochProbeAttemptLocked(id, "epoch_reset_sweep", at)
		state.quotas[id] = lagged
		if transition := state.observeQuotaEpochProbeLocked(id, previous[id], lagged, inventory, at); transition.ID != "" {
			t.Fatalf("lagged first pass confirmed epoch for %s: %#v", id, transition)
		}
	}
	if !state.quotaEpoch.Sweep.Active || state.quotaEpoch.Sweep.Round != 2 || state.quotaEpoch.Sweep.NextRoundAt.IsZero() {
		t.Fatalf("lagged sweep did not schedule another round: %#v", state.quotaEpoch.Sweep)
	}
	nextRound := state.quotaEpoch.Sweep.NextRoundAt
	before := state.quotaPolls["b"].NextAt
	state.prepareQuotaEpochInventoryLocked(inventory, now.Add(10*time.Second))
	if !state.quotaPolls["b"].NextAt.Equal(before) {
		t.Fatalf("inventory refresh bypassed next reset round: before=%s after=%s", before, state.quotaPolls["b"].NextAt)
	}
	if reason := state.quotaEpochProbeReasonLocked("a", nextRound); reason != "" {
		t.Fatalf("evidence account was redundantly reprobed: %q", reason)
	}
	if reason := state.quotaEpochProbeReasonLocked("b", nextRound); reason != "epoch_reset_sweep" {
		t.Fatalf("lagged account was not due in round two: %q", reason)
	}

	for offset, id := range []string{"b", "c"} {
		at := nextRound.Add(time.Duration(offset) * time.Second)
		current := quotaEpochTestSnapshot(id, "idx-"+id, 0, at, at.Add(7*24*time.Hour), true)
		state.recordQuotaEpochProbeAttemptLocked(id, "epoch_reset_sweep", at)
		old := state.quotas[id]
		state.quotas[id] = current
		transition := state.observeQuotaEpochProbeLocked(id, old, current, inventory, at)
		if id == "b" && transition.ID != "" {
			t.Fatalf("epoch confirmed before delayed quorum: %#v", transition)
		}
		if id == "c" && (transition.ID == "" || transition.Reason != "provider_reset") {
			t.Fatalf("delayed quorum did not confirm epoch: %#v", transition)
		}
	}
	for _, id := range []string{"d", "e"} {
		account := state.quotaEpoch.Accounts[id]
		if account.State != quotaEpochAccountPending || !account.ObservedAt.IsZero() {
			t.Fatalf("old-cycle snapshot for %s was treated as new-epoch usage: %#v", id, account)
		}
	}
	if reason := state.quotaEpochProbeReasonLocked("d", nextRound.Add(2*time.Second)); reason != "epoch_pending" {
		t.Fatalf("lagged epoch account did not receive a follow-up probe: %q", reason)
	}
}

func TestQuotaEpochLateAccountResetJoinsCurrentEpochWithoutDuplicateSweep(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := quotaEpochTestSnapshot("a", "idx-a", 70, now.Add(-time.Minute), now.Add(24*time.Hour), false)
	current := quotaEpochTestSnapshot("a", "idx-a", 0, now, now.Add(7*24*time.Hour), true)
	state := schedulerRuntimeState{
		quotas:     map[string]quotaSnapshot{"a": previous, "idx-a": previous},
		quotaPolls: make(map[string]quotaPollState),
		quotaEpoch: quotaEpochState{
			ID: "week-current", Sequence: 1, Reason: "provider_reset", ConfirmedAt: now.Add(-time.Minute),
			ResetAt: current.Windows[0].ResetAt,
			Accounts: map[string]quotaEpochAccountState{
				"a": {AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountPending},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
	}
	inventory := map[string]cpaAuthFileEntry{"a": {ID: "a", AuthIndex: "idx-a"}}
	state.quotas["a"] = current
	if transition := state.observeQuotaEpochProbeLocked("a", previous, current, inventory, now); transition.ID != "" {
		t.Fatalf("late account reset created a duplicate epoch: %#v", transition)
	}
	if state.quotaEpoch.Sweep.Active || state.quotaEpoch.Sequence != 1 {
		t.Fatalf("late reset started another pool sweep: %#v", state.quotaEpoch)
	}
	account := state.quotaEpoch.Accounts["a"]
	if account.State != quotaEpochAccountPending || account.ObservedAt.IsZero() {
		t.Fatalf("late reset did not make the account warmup-ready: %#v", account)
	}
}

func TestQuotaEpochWarmupAllowanceUsesTenMinuteSnapshotAndStopsAfterUsage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-5 * time.Minute)
	snapshot := quotaEpochTestSnapshot("a", "idx-a", 0, observedAt, observedAt.Add(7*24*time.Hour), true)
	epoch := newQuotaEpochState()
	epoch.ID = "week-1"
	epoch.Accounts["a"] = quotaEpochAccountState{
		AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountPending, ObservedAt: observedAt,
	}
	if id, ok := quotaEpochWarmupAllowance(epoch, snapshot, quotaPollState{}, now); !ok || id != epoch.ID {
		t.Fatalf("fresh epoch snapshot not admitted: id=%q ok=%v", id, ok)
	}
	if _, ok := quotaEpochWarmupAllowance(epoch, snapshot, quotaPollState{}, observedAt.Add(11*time.Minute)); ok {
		t.Fatal("epoch snapshot older than ten minutes was admitted")
	}
	if _, ok := quotaEpochWarmupAllowance(epoch, snapshot, quotaPollState{LastUsageAt: observedAt.Add(time.Second)}, now); ok {
		t.Fatal("synthetic warmup admitted after real account usage")
	}
}

func TestQuotaEpochNaturalAndSuccessfulWarmupAreIdempotentPerEpoch(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := quotaEpochTestSnapshot("a", "idx-a", 0, now, now.Add(7*24*time.Hour), true)
	state := schedulerRuntimeState{
		quotas: map[string]quotaSnapshot{"a": snapshot, "idx-a": snapshot},
		quotaEpoch: quotaEpochState{
			ID: "week-1", Accounts: map[string]quotaEpochAccountState{
				"a": {AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountPending, ObservedAt: now},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
	}
	if !state.markQuotaEpochNatural("a", "idx-a", now.Add(time.Second)) {
		t.Fatal("first real usage did not mark natural activation")
	}
	if state.markQuotaEpochNatural("a", "idx-a", now.Add(2*time.Second)) {
		t.Fatal("repeated real usage reported another epoch transition")
	}
	if state.quotaEpoch.Accounts["a"].State != quotaEpochAccountNatural {
		t.Fatalf("natural state lost: %#v", state.quotaEpoch.Accounts["a"])
	}
	if state.recordQuotaEpochWarmupOutcome("a", "old-week", true, false, "", now) {
		t.Fatal("old epoch warmup outcome mutated current epoch")
	}
}

func TestQuotaEpochTransitionClearsRetryableOldWarmupsButKeepsHardBlocks(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		quotaEpoch: quotaEpochState{
			ID: "week-2", Accounts: map[string]quotaEpochAccountState{
				"a": {AuthID: "a", State: quotaEpochAccountPending, ObservedAt: now},
				"b": {AuthID: "b", State: quotaEpochAccountPending, ObservedAt: now},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
		warmups: map[string]warmupEntry{
			"a|5h": {AuthID: "a", Window: "5h", AttemptedAt: now.Add(-time.Hour), Error: "auth_unavailable", Blocked: true},
			"b|5h": {AuthID: "b", Window: "5h", AttemptedAt: now.Add(-time.Hour), Error: "cyber_policy", Blocked: true},
		},
	}
	state.applyQuotaEpochTransition(quotaEpochTransition{ID: "week-2", Reason: "provider_reset"})
	if _, exists := state.warmups["a|5h"]; exists {
		t.Fatal("retryable provider outage survived a new epoch")
	}
	if _, exists := state.warmups["b|5h"]; !exists {
		t.Fatal("hard policy block was cleared by a quota reset")
	}
	if account := state.quotaEpoch.Accounts["b"]; account.State != quotaEpochAccountBlocked || account.Error != "cyber_policy" {
		t.Fatalf("hard block not reflected in epoch account state: %#v", account)
	}
}

func TestQuotaEpochPendingUnobservedAccountGetsFinitePostSweepProbes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		quotaEpoch: quotaEpochState{
			ID: "week-3", ConfirmedAt: now.Add(-time.Minute),
			Accounts: map[string]quotaEpochAccountState{
				"a": {AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountPending},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
	}
	for attempt := 0; attempt < quotaEpochPendingProbeLimit; attempt++ {
		at := now.Add(time.Duration(attempt) * (quotaEpochPendingProbeDelay + time.Second))
		if reason := state.quotaEpochProbeReasonLocked("a", at); reason != "epoch_pending" {
			t.Fatalf("attempt %d reason=%q want epoch_pending", attempt+1, reason)
		}
		state.recordQuotaEpochProbeAttemptLocked("a", "epoch_pending", at)
	}
	if reason := state.quotaEpochProbeReasonLocked("a", now.Add(10*time.Minute)); reason != "" {
		t.Fatalf("finite retry cap was ignored: %q", reason)
	}
	account := state.quotaEpoch.Accounts["a"]
	if account.ProbeAttempts != quotaEpochPendingProbeLimit || account.LastProbeAt.IsZero() {
		t.Fatalf("probe accounting missing: %#v", account)
	}
}

func TestQuotaEpochPendingStaleAccountGetsFreshProbe(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-quotaEpochWarmupSnapshotAge - time.Second)
	state := schedulerRuntimeState{
		quotaEpoch: quotaEpochState{
			ID: "week-4", ConfirmedAt: observedAt,
			Accounts: map[string]quotaEpochAccountState{
				"a": {AuthID: "a", AuthIndex: "idx-a", State: quotaEpochAccountPending, ObservedAt: observedAt},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
	}
	if reason := state.quotaEpochProbeReasonLocked("a", now); reason != "epoch_pending" {
		t.Fatalf("stale pending account reason=%q want epoch_pending", reason)
	}
	state.recordQuotaEpochProbeAttemptLocked("a", "epoch_pending", now)
	account := state.quotaEpoch.Accounts["a"]
	if account.ProbeAttempts != 1 || !account.LastProbeAt.Equal(now) {
		t.Fatalf("stale pending probe was not accounted: %#v", account)
	}
}

func TestQuotaEpochResetPendingGetsOneDelayedCatchupAfterFastProbeBudget(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	confirmedAt := now.Add(-11 * time.Minute)
	lastProbeAt := now.Add(-quotaEpochWarmupSnapshotAge - time.Second)
	state := schedulerRuntimeState{
		quotaEpoch: quotaEpochState{
			ID:          "week-delayed",
			Reason:      "provider_reset",
			ConfirmedAt: confirmedAt,
			Accounts: map[string]quotaEpochAccountState{
				"a": {
					AuthID:        "a",
					AuthIndex:     "idx-a",
					State:         quotaEpochAccountPending,
					ProbeAttempts: quotaEpochPendingProbeLimit,
					LastProbeAt:   lastProbeAt,
				},
			},
			Sweep: quotaEpochSweepState{Targets: make(map[string]quotaEpochSweepTarget)},
		},
	}
	if reason := state.quotaEpochProbeReasonLocked("a", now); reason != "epoch_pending" {
		t.Fatalf("delayed reset catch-up reason=%q want epoch_pending", reason)
	}
	state.recordQuotaEpochProbeAttemptLocked("a", "epoch_pending", now)
	account := state.quotaEpoch.Accounts["a"]
	if account.ProbeAttempts != quotaEpochPendingProbeLimit+1 || !account.LastProbeAt.Equal(now) {
		t.Fatalf("delayed catch-up probe was not accounted: %#v", account)
	}
	if reason := state.quotaEpochProbeReasonLocked("a", now.Add(5*time.Minute)); reason != "" {
		t.Fatalf("reset catch-up escaped bounded window: %q", reason)
	}

	state.quotaEpoch.Reason = "bootstrap"
	state.quotaEpoch.Accounts["a"] = quotaEpochAccountState{
		AuthID:        "a",
		AuthIndex:     "idx-a",
		State:         quotaEpochAccountPending,
		ProbeAttempts: quotaEpochPendingProbeLimit,
		LastProbeAt:   lastProbeAt,
	}
	if reason := state.quotaEpochProbeReasonLocked("a", now); reason != "" {
		t.Fatalf("non-reset epoch exceeded finite probe budget: %q", reason)
	}
}

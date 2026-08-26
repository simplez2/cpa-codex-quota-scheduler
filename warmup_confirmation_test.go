package main

import (
	"testing"
	"time"
)

func TestWarmupConfirmationRequiresExplicitPerWindowObservation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		"acct|5h": {
			AuthID:      "acct",
			AuthIndex:   "idx",
			Window:      "5h",
			CompletedAt: completedAt,
		},
	}}
	current := map[string]quotaSnapshot{"idx": {
		AuthID:      "acct",
		AuthIndex:   "idx",
		RefreshedAt: completedAt.Add(time.Second),
		Windows: []quotaWindow{{
			Class: "5h",
			// A newer snapshot envelope is not evidence that this window was
			// actually observed in a partial-cache response.
			ObservedAt: time.Time{},
		}},
	}}

	targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current)
	if len(targets) != 1 || targets[0].Reason != "warmup_pending_confirmation" {
		t.Fatalf("zero-observation window was incorrectly treated as confirmed: %#v", targets)
	}
}

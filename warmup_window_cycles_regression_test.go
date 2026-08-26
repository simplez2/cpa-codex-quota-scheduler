package main

import (
	"testing"
	"time"
)

func TestNormalizeQuotaSnapshotUsesDurationOverPositionalLabel(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	zero := 0.0
	allowed := true
	tests := []struct {
		name    string
		seconds int64
		want    string
	}{
		{name: "weekly-only-primary", seconds: int64((7 * 24 * time.Hour).Seconds()), want: "weekly"},
		{name: "monthly-only-primary", seconds: int64((30 * 24 * time.Hour).Seconds()), want: "monthly"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seconds := tc.seconds
			response := keeperCheckResponse{Quota: []keeperQuotaRow{{
				Label: "primary", UsedPercent: &zero, Allowed: &allowed,
				Window: &keeperQuotaWindow{Seconds: &seconds},
			}}}
			snapshot := normalizeQuotaSnapshot("idx", "acct", response, now, now)
			if len(snapshot.Windows) != 1 || snapshot.Windows[0].Class != tc.want {
				t.Fatalf("normalized positional window = %#v; want class %q", snapshot.Windows, tc.want)
			}
		})
	}
}

func TestNormalizeQuotaSnapshotFallsBackToExplicitLabelWithoutDuration(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	zero := 0.0
	allowed := true
	response := keeperCheckResponse{Quota: []keeperQuotaRow{{Label: "weekly", UsedPercent: &zero, Allowed: &allowed}}}
	snapshot := normalizeQuotaSnapshot("idx", "acct", response, now, now)
	if len(snapshot.Windows) != 1 || snapshot.Windows[0].Class != "weekly" {
		t.Fatalf("explicit label fallback = %#v", snapshot.Windows)
	}
}

func TestNormalizeQuotaSnapshotAnchorsResetAfterToKeeperObservation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	observedAt := now.Add(-10 * time.Minute)
	zero := 0.0
	allowed := true
	five := int64((5 * time.Hour).Seconds())
	remaining := int64((4*time.Hour + 30*time.Minute).Seconds())
	response := keeperCheckResponse{Quota: []keeperQuotaRow{{
		Label: "primary", UsedPercent: &zero, Allowed: &allowed,
		Window: &keeperQuotaWindow{Seconds: &five}, ResetAfterSeconds: &remaining,
	}}}
	snapshot := normalizeQuotaSnapshot("idx", "acct", response, observedAt, now)
	if len(snapshot.Windows) != 1 {
		t.Fatalf("normalized windows = %#v", snapshot.Windows)
	}
	want := observedAt.Add(time.Duration(remaining) * time.Second)
	if !snapshot.Windows[0].ResetAt.Equal(want) {
		t.Fatalf("reset_at=%v want Keeper-observation anchor %v", snapshot.Windows[0].ResetAt, want)
	}
}

func TestCarriedStaleWindowRequestsKeeperRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	current := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}},
	}}
	previous := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", Allowed: true, ObservedAt: now},
			{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-16 * time.Minute)},
		},
	}}
	targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute)
	if len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "carried_stale_window" {
		t.Fatalf("carried stale refresh targets = %#v", targets)
	}
}

func TestFreshCarriedWindowDoesNotRequestKeeperRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	current := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
	previous := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-5 * time.Minute)}}}}
	if targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
		t.Fatalf("fresh carried sibling requested refresh: %#v", targets)
	}
}

func TestExpiredMissingWindowDoesNotRequestKeeperRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	current := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
	previous := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(-time.Minute), ObservedAt: now.Add(-16 * time.Minute)}}}}
	if targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
		t.Fatalf("expired non-carried sibling requested refresh: %#v", targets)
	}
}

func TestKeeperRefreshTargetMergeDeduplicatesCarriedWindow(t *testing.T) {
	base := []keeperRefreshTarget{{AuthIndex: "idx", Reason: "stale"}}
	carried := []keeperRefreshTarget{{AuthIndex: "idx", Reason: "carried_stale_window"}}
	merged := mergeKeeperRefreshTargets(base, carried)
	if len(merged) != 1 || merged[0].Reason != "stale" {
		t.Fatalf("merged refresh targets = %#v", merged)
	}
}

func TestCarriedStaleWindowWithoutResetRequestsKeeperRefresh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	current := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
	previous := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "weekly", Allowed: true, ObservedAt: now.Add(-16 * time.Minute)}},
	}}
	targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute)
	if len(targets) != 1 || targets[0].Reason != "carried_stale_window" {
		t.Fatalf("stale zero-reset carried sibling was not refreshed: %#v", targets)
	}
}

func TestStaleOuterSnapshotCannotProduceCarriedRefreshTarget(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	current := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", Allowed: true, ObservedAt: now}}}}
	previous := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now.Add(-16 * time.Minute),
		Windows: []quotaWindow{{Class: "weekly", Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now.Add(-16 * time.Minute)}},
	}}
	if targets := collectCarriedStaleWindowRefreshTargets([]string{"idx"}, current, previous, now, 15*time.Minute); len(targets) != 0 {
		t.Fatalf("window from an outer-stale snapshot cannot be carried but requested refresh: %#v", targets)
	}
}

func TestPendingWarmupRequestsFreshKeeperConfirmation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		"acct|5h":     {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
		"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
	}}
	current := map[string]quotaSnapshot{"idx": {AuthID: "acct", AuthIndex: "idx", RefreshedAt: completedAt.Add(-time.Second)}}
	targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current)
	if len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "warmup_pending_confirmation" {
		t.Fatalf("pending warmup confirmation targets = %#v", targets)
	}
}

func TestPostWarmupConfirmableFiveHourDoesNotRequestRedundantConfirmation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	observedAt := completedAt.Add(time.Second)
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		"acct|5h": {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
	}}
	current := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: observedAt,
		Windows: []quotaWindow{{Class: "5h", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: observedAt}},
	}}
	if targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current); len(targets) != 0 {
		t.Fatalf("confirmable post-warmup 5h observation requested redundant refresh: %#v", targets)
	}
}

func TestPostWarmupFiveHourObservationDoesNotConfirmMissingWeeklySibling(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	observedAt := completedAt.Add(time.Second)
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		"acct|5h":     {AuthID: "acct", AuthIndex: "idx", Window: "5h", CompletedAt: completedAt},
		"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
	}}
	current := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: observedAt,
		Windows: []quotaWindow{{Class: "5h", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: observedAt}},
	}}
	targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current)
	if len(targets) != 1 || targets[0].AuthIndex != "idx" || targets[0].Reason != "warmup_pending_confirmation" {
		t.Fatalf("fresh 5h incorrectly confirmed missing weekly sibling: %#v", targets)
	}
}

func TestPendingWarmupMasksCarriedResetBeforeConfirmation(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	oldReset := now.Add(6 * 24 * time.Hour)
	state := schedulerRuntimeState{
		warmups: map[string]warmupEntry{
			"acct|weekly": {AuthID: "acct", AuthIndex: "idx", Window: "weekly", CompletedAt: completedAt},
		},
		quotas: map[string]quotaSnapshot{
			"idx": {
				AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 10, Allowed: true, ResetAt: oldReset, ObservedAt: completedAt.Add(-time.Second)}},
			},
		},
	}
	current := map[string]quotaSnapshot{"idx": {
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "5h", UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now}},
	}}
	if targets := state.pendingWarmupKeeperRefreshTargets([]string{"idx"}, current); len(targets) != 1 {
		t.Fatalf("pending weekly did not request confirmation refresh: %#v", targets)
	}
	masked := state.quotas["idx"]
	if len(masked.Windows) != 1 || !masked.Windows[0].ResetAt.IsZero() {
		t.Fatalf("old carried weekly reset remained confirmable: %#v", masked.Windows)
	}
	// Even if a newer snapshot envelope later carries this masked row, the
	// existing confirmation function cannot accept it until Keeper supplies a
	// real post-warmup reset anchor for weekly itself.
	masked.RefreshedAt = now.Add(time.Second)
	if changed := state.confirmPendingWarmups(map[string]quotaSnapshot{"idx": masked}, now); changed {
		t.Fatal("masked stale weekly reset was incorrectly confirmed")
	}
}

func TestWarmupConfirmationRefreshSkipsActivatedBlockedAndInactiveAuths(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	completedAt := now.Add(-time.Minute)
	state := schedulerRuntimeState{warmups: map[string]warmupEntry{
		"active-done|5h":    {AuthID: "active-done", AuthIndex: "done", Window: "5h", CompletedAt: completedAt, ActivatedAt: completedAt},
		"active-blocked|5h": {AuthID: "active-blocked", AuthIndex: "blocked", Window: "5h", CompletedAt: completedAt, Blocked: true},
		"inactive|5h":       {AuthID: "inactive", AuthIndex: "inactive", Window: "5h", CompletedAt: completedAt},
	}}
	if targets := state.pendingWarmupKeeperRefreshTargets([]string{"done", "blocked"}, nil); len(targets) != 0 {
		t.Fatalf("non-actionable warmup entries requested confirmation refresh: %#v", targets)
	}
}

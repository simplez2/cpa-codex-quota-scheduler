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

func TestPlaceholderResetRequiresApproximatelyFullCycle(t *testing.T) {
	now := time.Now().UTC()
	seconds := int64((5 * time.Hour).Seconds())
	base := quotaWindow{Class: "5h", WindowSeconds: seconds, ResetAfterSecondsKnown: true, Allowed: true, ResetAt: now.Add(5 * time.Hour), ObservedAt: now}
	within := base
	within.ResetAfterSeconds = seconds - 2
	if !quotaWindowHasPlaceholderReset(within, now, now) {
		t.Fatalf("near-full resetAfter was not accepted as placeholder: %#v", within)
	}
	oversized := base
	oversized.ResetAfterSeconds = seconds + 60
	oversized.ResetAt = now.Add(5*time.Hour + time.Minute)
	if quotaWindowHasPlaceholderReset(oversized, now, now) {
		t.Fatalf("oversized resetAfter was accepted as placeholder: %#v", oversized)
	}
}

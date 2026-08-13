package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fullResetSnapshot(authID string, refreshedAt time.Time) quotaSnapshot {
	const monthlySeconds = int64(30 * 24 * 60 * 60)
	return quotaSnapshot{
		AuthID:      authID,
		AuthIndex:   "idx-" + authID,
		RefreshedAt: refreshedAt,
		Windows: []quotaWindow{{
			Class:                   "monthly",
			WindowSeconds:           monthlySeconds,
			ResetAfterSeconds:       monthlySeconds,
			ResetAfterSecondsKnown:  true,
			UsedPercent:             0,
			Allowed:                 true,
			LimitReached:            false,
			ResetAt:                 refreshedAt.Add(time.Duration(monthlySeconds) * time.Second),
			ObservedAt:              refreshedAt,
			WindowUsageCredits:      0,
			WindowUsageCreditsKnown: true,
		}},
	}
}

func TestReconcileExternalResetClearsOnlyAfterTwoFreshSnapshots(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg:                   defaultPluginConfig(),
		warmups:               make(map[string]warmupEntry),
		warmupLeases:          make(map[string]warmupLease),
		banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{
		Kind:     banKindQuota,
		Phase:    banPhaseCooldown,
		Window:   "5h", // Historical classifier bug: the reset timestamp actually belongs to the monthly window.
		BannedAt: now.Add(-10 * 24 * time.Hour),
		ResetAt:  now.Add(20 * 24 * time.Hour),
	})

	first := fullResetSnapshot("acct", now.Add(-2*time.Minute))
	cleared := state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": first}, now)
	if len(cleared) != 0 {
		t.Fatalf("first snapshot cleared = %#v; want confirmation only", cleared)
	}
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("quota ban cleared after only one snapshot")
	}

	second := fullResetSnapshot("acct", now.Add(-time.Minute))
	cleared = state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": second}, now)
	if _, ok := cleared["acct"]; !ok {
		t.Fatalf("second fresh snapshot cleared = %#v; want acct", cleared)
	}
	if _, banned := banStore.lookup("acct"); banned {
		t.Fatal("stale quota ban remained after two matching fresh snapshots")
	}
	if state.banExternalResetClears != 1 || state.lastBanClearReason != "external_new_cycle" {
		t.Fatalf("clear diagnostics = count %d reason %q", state.banExternalResetClears, state.lastBanClearReason)
	}
}

func TestReconcileExternalResetClearsObsoleteMonthlyBanAfterWeeklyOnlyPlanChange(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	entry := banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h",
		BannedAt: now.Add(-5 * 24 * time.Hour), ResetAt: now.Add(19 * 24 * time.Hour),
	}
	banStore.set("acct", entry)
	weekly := func(observedAt time.Time) quotaSnapshot {
		return quotaSnapshot{
			AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: observedAt,
			Windows: []quotaWindow{{
				Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
				ResetAfterSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAfterSecondsKnown: true,
				UsedPercent: 0, Allowed: true, ResetAt: observedAt.Add(7 * 24 * time.Hour),
				ObservedAt: observedAt, Source: quotaSourceKeeper, WindowUsageCreditsKnown: true,
			}},
		}
	}
	first := weekly(now.Add(-2 * time.Minute))
	if cleared := state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": first}, now); len(cleared) != 0 {
		t.Fatalf("first plan-shape observation cleared = %#v; want confirmation only", cleared)
	}
	confirmation := state.banResetConfirmations["acct"]
	if confirmation.Confirmations != 1 || confirmation.Reason != "monthly_window_set_replaced_by_weekly" {
		t.Fatalf("plan-shape confirmation = %#v", confirmation)
	}
	second := weekly(now.Add(-time.Minute))
	if cleared := state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": second}, now); len(cleared) != 1 {
		t.Fatalf("second plan-shape observation cleared = %#v; want acct", cleared)
	}
	if _, banned := banStore.lookup("acct"); banned {
		t.Fatal("obsolete monthly cooldown survived two weekly-only Keeper snapshots")
	}
}

func TestPlatformResetRepairsMisclassifiedCooldownAndAdmitsSameCycleWarmup(t *testing.T) {
	resetBanStoreForTest()
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, time.August, 12, 18, 50, 30, 0, location)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
		quotas: make(map[string]quotaSnapshot),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	// Production incident: a roughly monthly cooldown was persisted under the
	// historical generic 5h label before a platform reset exposed a fresh
	// weekly-only Team quota shape.
	banStore.set("jspp-team", banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h",
		BannedAt: time.Date(2026, time.August, 7, 15, 22, 15, 0, location),
		ResetAt:  time.Date(2026, time.August, 31, 21, 37, 38, 0, location),
	})
	weekly := func(observedAt time.Time) quotaSnapshot {
		const seconds = int64(7 * 24 * 60 * 60)
		return quotaSnapshot{
			AuthID: "jspp-team", AuthIndex: "idx-jspp-team", RefreshedAt: observedAt,
			Windows: []quotaWindow{{
				Class: "weekly", WindowSeconds: seconds, ResetAfterSeconds: seconds, ResetAfterSecondsKnown: true,
				Allowed: true, ResetAt: observedAt.Add(7 * 24 * time.Hour), ObservedAt: observedAt,
				Source: quotaSourceKeeper, WindowUsageCreditsKnown: true,
			}},
		}
	}
	binding := map[string]warmupAuthBinding{"jspp-team": {AuthID: "jspp-team", AuthIndex: "idx-jspp-team"}}
	first := weekly(time.Date(2026, time.August, 12, 18, 49, 50, 0, location))
	if cleared := state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"jspp-team": first}, now); len(cleared) != 0 {
		t.Fatalf("first platform-reset observation cleared = %#v", cleared)
	}
	confirmation := state.banResetConfirmations["jspp-team"]
	if confirmation.Confirmations != 1 || confirmation.Reason != "monthly_window_set_replaced_by_weekly" {
		t.Fatalf("first platform-reset confirmation = %#v", confirmation)
	}
	state.quotas["jspp-team"] = first
	if candidates := state.findWarmupCandidates(binding, nil, now); len(candidates) != 0 {
		t.Fatalf("quarantined account warmed before second observation: %#v", candidates)
	}
	second := weekly(time.Date(2026, time.August, 12, 18, 50, 25, 0, location))
	if cleared := state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"jspp-team": second}, now); len(cleared) != 1 {
		t.Fatalf("second platform-reset observation cleared = %#v", cleared)
	}
	if _, banned := banStore.lookup("jspp-team"); banned {
		t.Fatal("misclassified cooldown survived two fresh observations")
	}
	state.quotas["jspp-team"] = second
	candidates := state.findWarmupCandidates(binding, nil, now)
	if len(candidates) != 1 || candidates[0].Window.Class != "weekly" {
		t.Fatalf("same-cycle platform-reset warmup candidates = %#v", candidates)
	}
}

func TestReconcileExternalResetWindowSetReplacementFailsClosed(t *testing.T) {
	now := time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*quotaSnapshot)
	}{
		{name: "old class still present", mutate: func(snapshot *quotaSnapshot) {
			snapshot.Windows = append(snapshot.Windows, quotaWindow{
				Class: "monthly", WindowSeconds: int64((30 * 24 * time.Hour).Seconds()), UsedPercent: 0,
				Allowed: true, ResetAt: now.Add(19 * 24 * time.Hour), ObservedAt: snapshot.RefreshedAt, Source: quotaSourceKeeper,
			})
		}},
		{name: "unknown row", mutate: func(snapshot *quotaSnapshot) {
			snapshot.Windows = append(snapshot.Windows, quotaWindow{Class: "unknown", Allowed: true, ObservedAt: snapshot.RefreshedAt, Source: quotaSourceKeeper})
		}},
		{name: "header overlay", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].Source = quotaSourceMixed }},
		{name: "duration mismatch", mutate: func(snapshot *quotaSnapshot) {
			snapshot.Windows[0].WindowSeconds = int64((30 * 24 * time.Hour).Seconds())
		}},
		{name: "stale row", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ObservedAt = now.Add(-2 * time.Hour) }},
		{name: "limited row", mutate: func(snapshot *quotaSnapshot) {
			snapshot.Windows[0].UsedPercent = 100
			snapshot.Windows[0].LimitReached = true
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetBanStoreForTest()
			state := schedulerRuntimeState{
				cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
				warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
			}
			state.cfg.StatePath = ""
			state.cfg.StaleAfter = time.Hour
			entry := banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h", BannedAt: now.Add(-5 * 24 * time.Hour), ResetAt: now.Add(19 * 24 * time.Hour)}
			banStore.set("acct", entry)
			for _, observedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
				snapshot := quotaSnapshot{
					AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: observedAt,
					Windows: []quotaWindow{{
						Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()), UsedPercent: 0,
						Allowed: true, ResetAt: observedAt.Add(7 * 24 * time.Hour), ObservedAt: observedAt, Source: quotaSourceKeeper,
					}},
				}
				test.mutate(&snapshot)
				state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
			}
			if _, banned := banStore.lookup("acct"); !banned {
				t.Fatal("unsafe plan-shape evidence cleared quota cooldown")
			}
		})
	}
}

func TestReconcileExternalResetNeverClearsProbation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg:                   defaultPluginConfig(),
		warmups:               make(map[string]warmupEntry),
		warmupLeases:          make(map[string]warmupLease),
		banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{
		Kind:     banKindProbation,
		Phase:    banPhaseCooldown,
		Window:   "probation",
		BannedAt: now.Add(-2 * time.Hour),
		ResetAt:  now.Add(30 * 24 * time.Hour),
	})

	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		if cleared := state.reconcileExternallyResetQuotaBans(
			map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", refreshedAt)}, now,
		); len(cleared) != 0 {
			t.Fatalf("probation cleared = %#v", cleared)
		}
	}
	entry, banned := banStore.lookup("acct")
	if !banned || entry.Kind != banKindProbation {
		t.Fatalf("probation ban = %#v, banned=%v; must be preserved", entry, banned)
	}
	if state.banExternalResetClears != 0 {
		t.Fatalf("external reset clear count = %d; want 0", state.banExternalResetClears)
	}
}

func TestReconcileExternalResetRequiresDistinctIncreasingSnapshots(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour)})
	snapshot := fullResetSnapshot("acct", now.Add(-time.Minute))
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("identical RefreshedAt counted as a second reset confirmation")
	}
	confirmation := state.banResetConfirmations["acct"]
	if confirmation.Confirmations != 1 {
		t.Fatalf("confirmation count = %d; want 1", confirmation.Confirmations)
	}
}

func TestReconcileExternalResetDoesNotCombineExpiredFirstConfirmation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = 15 * time.Minute
	banStore.set("acct", banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly",
		BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour),
	})
	state.reconcileExternallyResetQuotaBans(
		map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", now.Add(-2*time.Minute))}, now,
	)

	// No successful Keeper refresh runs during this gap. The old observation is
	// no longer fresh and cannot be combined with one recovery snapshot.
	later := now.Add(2 * time.Hour)
	state.reconcileExternallyResetQuotaBans(
		map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", later.Add(-time.Minute))}, later,
	)
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("expired first confirmation combined with a single recovery snapshot")
	}
	confirmation := state.banResetConfirmations["acct"]
	if confirmation.Confirmations != 1 || !confirmation.LastSnapshotAt.Equal(later.Add(-time.Minute)) {
		t.Fatalf("expired confirmation was not restarted: %#v", confirmation)
	}
}

func TestReconcileExternalResetRejectsUnsafeSnapshotsAndHalfOpen(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		mutate func(*quotaSnapshot)
	}{
		{name: "snapshot before ban", mutate: func(snapshot *quotaSnapshot) { snapshot.RefreshedAt = now.Add(-2 * time.Hour) }},
		{name: "reset anchor unchanged", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ResetAt = now.Add(20 * 24 * time.Hour) }},
		{name: "reset anchor regressed", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ResetAt = now.Add(19 * 24 * time.Hour) }},
		{name: "effective window missing", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].Class = "weekly" }},
		{name: "used percent full despite flag", mutate: func(snapshot *quotaSnapshot) {
			snapshot.Windows[0].UsedPercent = 100
			snapshot.Windows[0].LimitReached = false
		}},
		{name: "window observed before ban", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ObservedAt = now.Add(-2 * time.Hour) }},
		{name: "window observed in future", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ObservedAt = now.Add(time.Minute) }},
		{name: "window observation missing", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].ObservedAt = time.Time{} }},
		{name: "not allowed", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].Allowed = false }},
		{name: "limit reached", mutate: func(snapshot *quotaSnapshot) { snapshot.Windows[0].LimitReached = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetBanStoreForTest()
			state := schedulerRuntimeState{
				cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
				warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
			}
			state.cfg.StatePath = ""
			state.cfg.StaleAfter = 3 * time.Hour
			banStore.set("acct", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour)})
			for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
				snapshot := fullResetSnapshot("acct", refreshedAt)
				test.mutate(&snapshot)
				state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
			}
			if _, banned := banStore.lookup("acct"); !banned {
				t.Fatal("unsafe snapshot cleared quota ban")
			}
		})
	}

	resetBanStoreForTest()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{
		Kind: banKindQuota, Phase: banPhaseHalfOpen, Window: "monthly",
		BannedAt: now.Add(-time.Hour), ResetAt: now.Add(-time.Minute),
		ProbeStartedAt: now.Add(-10 * time.Second), ProbeLeaseUntil: now.Add(time.Minute),
	})
	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", refreshedAt)}, now)
	}
	if entry, banned := banStore.lookup("acct"); !banned || entry.Phase != banPhaseHalfOpen {
		t.Fatalf("half-open quota state = %#v banned=%v; must not be cleared", entry, banned)
	}
}

func TestReconcileExternalResetDoesNotCountReusedWindowTwice(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	entry := banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly",
		BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour),
	}
	banStore.set("acct", entry)
	windowObservedAt := now.Add(-2 * time.Minute)
	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		snapshot := fullResetSnapshot("acct", refreshedAt)
		snapshot.Windows[0].ObservedAt = windowObservedAt
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	}
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("outer RefreshedAt changes counted a reused window as two confirmations")
	}
	if confirmation := state.banResetConfirmations["acct"]; confirmation.Confirmations != 1 || !confirmation.LastSnapshotAt.Equal(windowObservedAt) {
		t.Fatalf("reused-window confirmation = %#v; want one confirmation at window observation", confirmation)
	}
}

func TestReconcileExternalResetClearsHistoricalMonthlyBanAfterNewCycleHasUsage(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
		quotas: make(map[string]quotaSnapshot),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown,
		Window:   "5h", // historical under-classification; the span proves monthly
		BannedAt: time.Date(2026, time.August, 7, 0, 0, 0, 0, time.UTC),
		ResetAt:  time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
	})

	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		snapshot := fullResetSnapshot("acct", refreshedAt)
		snapshot.Windows[0].UsedPercent = 8
		snapshot.Windows[0].WindowUsageCredits = 42
		snapshot.Windows[0].ResetAt = time.Date(2026, time.September, 9, 0, 0, 0, 0, time.UTC)
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
		state.quotas["acct"] = snapshot
	}
	if _, banned := banStore.lookup("acct"); banned {
		t.Fatal("historical monthly ban remained after two fresh usable new-cycle snapshots")
	}
	if got := effectiveBanWindowClass(banEntry{
		Window:   "5h",
		BannedAt: time.Date(2026, time.August, 7, 0, 0, 0, 0, time.UTC),
		ResetAt:  time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
	}); got != "monthly" {
		t.Fatalf("effective historical ban class = %q; want monthly", got)
	}
	candidates := state.findWarmupCandidates(map[string]warmupAuthBinding{
		"acct": {AuthID: "acct", AuthIndex: "idx-acct"},
	}, nil, now)
	if len(candidates) != 0 {
		t.Fatalf("already-used new cycle became a warmup candidate: %#v", candidates)
	}
}

func TestReconcileExternalResetPreservesCurrentWeeklyLimitAndIgnoresMonthlyAnchor(t *testing.T) {
	resetBanStoreForTest()
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	entry := banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown,
		Window:   "5h", // historical label; seven-day span proves weekly
		BannedAt: time.Date(2026, time.August, 9, 15, 31, 0, 0, time.UTC),
		ResetAt:  time.Date(2026, time.August, 16, 15, 31, 0, 0, time.UTC),
	}
	banStore.set("acct", entry)
	if got := effectiveBanWindowClass(entry); got != "weekly" {
		t.Fatalf("effective current ban class = %q; want weekly", got)
	}

	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		snapshot := fullResetSnapshot("acct", refreshedAt)
		snapshot.Windows = append(snapshot.Windows, quotaWindow{
			Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			UsedPercent: 100, Allowed: true, LimitReached: true,
			ResetAt: entry.ResetAt, ObservedAt: refreshedAt,
		})
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	}
	if current, banned := banStore.lookup("acct"); !banned || !sameQuotaBan(current, entry) {
		t.Fatalf("current weekly ban was cleared by unrelated monthly anchor: %#v banned=%v", current, banned)
	}
}

func TestReconcileExternalResetFailsClosedForNearEndMisclassifiedWeeklyBan(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	entry := banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown,
		// Historical releases could call this 5h even when the weekly window was
		// limiting. With only four hours left, remaining duration cannot prove
		// which class it was.
		Window: "5h", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(4 * time.Hour),
	}
	banStore.set("acct", entry)
	if got := effectiveBanWindowClass(entry); got != "" {
		t.Fatalf("ambiguous near-end class = %q; want fail-closed empty class", got)
	}

	for _, observedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		snapshot := quotaSnapshot{
			AuthID: "acct", AuthIndex: "idx-acct", RefreshedAt: observedAt,
			Windows: []quotaWindow{
				{
					Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()),
					UsedPercent: 0, Allowed: true, ResetAt: entry.ResetAt.Add(time.Hour), ObservedAt: observedAt,
				},
				{
					Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
					UsedPercent: 100, Allowed: true, LimitReached: true,
					ResetAt: entry.ResetAt, ObservedAt: observedAt,
				},
			},
		}
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	}
	if current, banned := banStore.lookup("acct"); !banned || !sameQuotaBan(current, entry) {
		t.Fatalf("ambiguous weekly ban was cleared by unrelated 5h anchor: %#v banned=%v", current, banned)
	}
}

func TestReconcileExternalResetDoesNotUseMonthlyAnchorForRealFiveHourBan(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	entry := banEntry{
		Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h",
		BannedAt: now.Add(-time.Hour), ResetAt: now.Add(4 * time.Hour),
	}
	banStore.set("acct", entry)
	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		snapshot := fullResetSnapshot("acct", refreshedAt)
		snapshot.Windows = append(snapshot.Windows, quotaWindow{
			Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()),
			UsedPercent: 100, Allowed: true, LimitReached: true,
			ResetAt: entry.ResetAt, ObservedAt: refreshedAt,
		})
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": snapshot}, now)
	}
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("real 5h ban was cleared using the unrelated monthly reset anchor")
	}
}

func TestReconcileExternalResetDefersRecentWarmup429(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		warmups: map[string]warmupEntry{
			warmupKey("acct", "monthly"): {AuthID: "acct", Window: "monthly", AttemptedAt: now.Add(-5 * time.Minute), Status: statusTooManyRequests, Error: "http_429"},
		},
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	state.cfg.WarmupRetryAfter = 15 * time.Minute
	banStore.set("acct", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour)})
	for _, refreshedAt := range []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute)} {
		state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", refreshedAt)}, now)
	}
	if _, banned := banStore.lookup("acct"); !banned {
		t.Fatal("recent warmup 429 was immediately cleared and could loop")
	}

	later := now.Add(16 * time.Minute)
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", later.Add(-time.Minute))}, later)
	if _, banned := banStore.lookup("acct"); banned {
		t.Fatal("quota ban remained after warmup 429 retry guard elapsed and reset evidence stayed fresh")
	}
}

func TestBanResetConfirmationSurvivesStateReload(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "state.json")
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
	}
	state.cfg.StatePath = path
	state.cfg.StaleAfter = time.Hour
	state.initializeGenerationOwnership(path)
	if err := state.reserveGenerationOwnership(path); err != nil {
		t.Fatal(err)
	}
	claimManagedRuntimeForTest(t, &state)
	banStore.set("acct", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "monthly", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour)})
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", now.Add(-2*time.Minute))}, now)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted persistedBanState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	confirmation, ok := persisted.BanResetConfirmations["acct"]
	if !ok || confirmation.Confirmations != 1 {
		t.Fatalf("persisted confirmation = %#v ok=%v", confirmation, ok)
	}
	reloaded := schedulerRuntimeState{
		cfg: state.cfg, warmups: make(map[string]warmupEntry), warmupLeases: make(map[string]warmupLease),
		banResetConfirmations: persisted.BanResetConfirmations,
	}
	reloaded.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": fullResetSnapshot("acct", now.Add(-time.Minute))}, now)
	if _, banned := banStore.lookup("acct"); banned {
		t.Fatal("reloaded first confirmation did not combine with the second snapshot")
	}
}

func TestExternalResetClearMakesAccountImmediatelyWarmupEligible(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now().UTC()
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry),
		warmupLeases: make(map[string]warmupLease), banResetConfirmations: make(map[string]banResetConfirmation),
		quotas: make(map[string]quotaSnapshot),
	}
	state.cfg.StatePath = ""
	state.cfg.StaleAfter = time.Hour
	banStore.set("acct", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h", BannedAt: now.Add(-time.Hour), ResetAt: now.Add(20 * 24 * time.Hour)})
	first := fullResetSnapshot("acct", now.Add(-2*time.Minute))
	second := fullResetSnapshot("acct", now.Add(-time.Minute))
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": first}, now)
	state.reconcileExternallyResetQuotaBans(map[string]quotaSnapshot{"acct": second}, now)
	state.quotas["acct"] = second
	candidates := state.findWarmupCandidates(map[string]warmupAuthBinding{
		"acct": {AuthID: "acct", AuthIndex: "idx-acct"},
	}, nil, now)
	if len(candidates) != 1 || candidates[0].Snapshot.AuthID != "acct" {
		t.Fatalf("same-refresh warmup candidates = %#v", candidates)
	}
}

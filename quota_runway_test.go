package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func runwayTestSnapshot(at, reset time.Time, used float64) quotaSnapshot {
	return quotaSnapshot{AuthID: "account-a", AuthIndex: "index-a", RefreshedAt: at,
		Windows: []quotaWindow{{Class: "5h", WindowSeconds: 18000, UsedPercent: used, Allowed: true,
			ObservedAt: at, ResetAt: reset, Source: quotaSourceProbe}}}
}

func runwayTestSeed(t *quotaRunwayTracker, start time.Time) quotaSnapshot {
	reset := start.Add(4 * time.Hour)
	var snapshot quotaSnapshot
	for index, used := range []float64{10, 12, 13} {
		at := start.Add(time.Duration(index) * time.Minute)
		snapshot = runwayTestSnapshot(at, reset, used)
		t.Observe(snapshot, at)
	}
	return snapshot
}

func runwayTestNear(t *testing.T, actual, expected float64) {
	t.Helper()
	if math.Abs(actual-expected) > 0.000001 {
		t.Fatalf("got %.9f, want %.9f", actual, expected)
	}
}

func runwayTestReserveConfig() pluginConfig {
	cfg := defaultPluginConfig()
	cfg.Serial5hHandoffMode = "reserve_aware"
	cfg.Reserve5hPercent = 15
	return cfg
}

func TestQuotaRunwayStaticUntilTwoIndependentIncrements(t *testing.T) {
	var tracker quotaRunwayTracker
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	cfg := runwayTestReserveConfig()
	first := runwayTestSnapshot(start, start.Add(4*time.Hour), 10)
	tracker.Observe(first, start)
	for repeat := 0; repeat < 10; repeat++ {
		tracker.Observe(first, start)
	}
	item := tracker.Assess(first, cfg, start)[0]
	if item.RateKnown || item.Samples != 0 || item.RunwaySeconds != nil {
		t.Fatalf("repeated cached observation taught rate: %+v", item)
	}
	runwayTestNear(t, item.HeadroomPercent, 75)
	second := runwayTestSnapshot(start.Add(time.Minute), first.Windows[0].ResetAt, 12)
	tracker.Observe(second, second.RefreshedAt)
	if item = tracker.Assess(second, cfg, second.RefreshedAt)[0]; item.RateKnown || item.Samples != 1 {
		t.Fatalf("single increment should retain static reserve: %+v", item)
	}
	third := runwayTestSnapshot(start.Add(2*time.Minute), first.Windows[0].ResetAt, 13)
	tracker.Observe(third, third.RefreshedAt)
	item = tracker.Assess(third, cfg, third.RefreshedAt.Add(time.Minute))[0]
	if !item.RateKnown || item.Samples != 2 || item.RunwaySeconds == nil || item.Reason != "probe_rate_guard" {
		t.Fatalf("missing prediction: %+v", item)
	}
	// Recent peak 120 pp/h outranks the EWMA of 105 pp/h.
	runwayTestNear(t, item.BurnPercentPerHour, 120)
	runwayTestNear(t, item.CacheDebitPercent, 2)
	runwayTestNear(t, item.ForecastReservePercent, 4)
	runwayTestNear(t, item.EffectiveReservePercent, 19)
	runwayTestNear(t, item.HeadroomPercent, 66)
	runwayTestNear(t, *item.RunwaySeconds, 1980)
}

func TestQuotaRunwayHeadersCannotTeachOrRefreshProbe(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, source := range []quotaSource{quotaSourceHeader, quotaSourceMixed, quotaSourceUnknown} {
		t.Run(string(source), func(t *testing.T) {
			var tracker quotaRunwayTracker
			snapshot := runwayTestSeed(&tracker, start)
			snapshot.Windows[0].Source = source
			snapshot.Windows[0].UsedPercent = 4
			snapshot.Windows[0].ObservedAt = start.Add(3 * time.Minute)
			tracker.Observe(snapshot, start.Add(3*time.Minute))
			item := tracker.Assess(snapshot, runwayTestReserveConfig(), start.Add(3*time.Minute))[0]
			if !item.RateKnown || item.Samples != 2 || !item.ObservedAt.Equal(start.Add(2*time.Minute)) {
				t.Fatalf("header mutated trusted rate: %+v", item)
			}
			runwayTestNear(t, item.RemainingPercent, 87)
			runwayTestNear(t, item.CacheDebitPercent, 2)
			// A stricter header already accounts for 1 pp of the 2 pp cache debit.
			snapshot.Windows[0].UsedPercent = 14
			item = tracker.Assess(snapshot, runwayTestReserveConfig(), start.Add(3*time.Minute))[0]
			runwayTestNear(t, item.CacheDebitPercent, 1)
			runwayTestNear(t, item.HeadroomPercent, 66)
		})
	}
}

func TestQuotaRunwayResetsEvidenceOnDiscontinuity(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(*quotaSnapshot)
	}{
		{"usage_falls", func(q *quotaSnapshot) { q.Windows[0].UsedPercent = 5 }},
		{"cycle_changes", func(q *quotaSnapshot) { q.Windows[0].ResetAt = q.Windows[0].ResetAt.Add(5 * time.Hour) }},
		{"binding_changes", func(q *quotaSnapshot) { q.AuthIndex = "replacement-index" }},
		{"missing_binding", func(q *quotaSnapshot) { q.AuthIndex = "" }},
		{"duration_changes", func(q *quotaSnapshot) { q.Windows[0].WindowSeconds = 17900 }},
		{"time_reverses", func(q *quotaSnapshot) { q.RefreshedAt = start; q.Windows[0].ObservedAt = start }},
		{"future_timestamp", func(q *quotaSnapshot) { q.RefreshedAt = start.Add(time.Hour); q.Windows[0].ObservedAt = q.RefreshedAt }},
		{"missing_window_timestamp", func(q *quotaSnapshot) { q.Windows[0].ObservedAt = time.Time{} }},
		{"different_window_timestamp", func(q *quotaSnapshot) { q.Windows[0].ObservedAt = start.Add(2 * time.Minute) }},
		{"nonfinite_usage", func(q *quotaSnapshot) { q.Windows[0].UsedPercent = math.NaN() }},
		{"moving_zero_placeholder", func(q *quotaSnapshot) {
			q.Windows[0].UsedPercent = 0
			q.Windows[0].ResetAt = q.RefreshedAt.Add(5 * time.Hour)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var tracker quotaRunwayTracker
			snapshot := runwayTestSeed(&tracker, start)
			snapshot.RefreshedAt = start.Add(3 * time.Minute)
			snapshot.Windows[0].ObservedAt = snapshot.RefreshedAt
			snapshot.Windows[0].UsedPercent = 14
			test.edit(&snapshot)
			tracker.Observe(snapshot, start.Add(3*time.Minute))
			item := tracker.Assess(snapshot, defaultPluginConfig(), start.Add(3*time.Minute))[0]
			if item.RateKnown || item.Samples >= quotaRunwayMinimumSamples || item.RunwaySeconds != nil {
				t.Fatalf("discontinuity retained learned prediction: %+v", item)
			}
		})
	}
}

func TestQuotaRunwayLongPollAndIdleHistoryExpire(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	var tracker quotaRunwayTracker
	snapshot := runwayTestSeed(&tracker, start)
	late := start.Add(20 * time.Minute)
	item := tracker.Assess(snapshot, defaultPluginConfig(), late)[0]
	if item.RateKnown || item.Fresh || item.RunwaySeconds != nil {
		t.Fatalf("stale sample still predicts: %+v", item)
	}
	// A poll delivered 18m after dispatch is not fresh rate evidence.
	tracker.Observe(snapshot, late)
	snapshot.RefreshedAt, snapshot.Windows[0].ObservedAt = late, late
	snapshot.Windows[0].UsedPercent = 20
	tracker.Observe(snapshot, late)
	if item = tracker.Assess(snapshot, defaultPluginConfig(), late)[0]; item.RateKnown || item.Samples != 0 {
		t.Fatalf("long poll failed to cold-start: %+v", item)
	}
	// An idle account may refresh repeatedly; old bursts still expire.
	tracker = quotaRunwayTracker{}
	snapshot = runwayTestSeed(&tracker, start)
	for minute := 3; minute <= 19; minute++ {
		at := start.Add(time.Duration(minute) * time.Minute)
		snapshot.RefreshedAt, snapshot.Windows[0].ObservedAt = at, at
		tracker.Observe(snapshot, at)
	}
	if item = tracker.Assess(snapshot, defaultPluginConfig(), snapshot.RefreshedAt)[0]; item.RateKnown || item.Samples != 0 {
		t.Fatalf("idle history retained rate: %+v", item)
	}
}

func TestQuotaRunwayFailureForgetAndBindingIsolation(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	var tracker quotaRunwayTracker
	snapshot := runwayTestSeed(&tracker, start)
	other := snapshot
	other.AuthID, other.AuthIndex = "account-b", "index-b"
	if item := tracker.Assess(other, defaultPluginConfig(), other.RefreshedAt)[0]; item.RateKnown {
		t.Fatal("different account inherited a rate")
	}
	other = snapshot
	other.AuthIndex = "replacement-index"
	if item := tracker.Assess(other, defaultPluginConfig(), other.RefreshedAt)[0]; item.RateKnown {
		t.Fatal("different binding inherited a rate")
	}
	tracker.ForgetAuth(snapshot.AuthID)
	if item := tracker.Assess(snapshot, defaultPluginConfig(), snapshot.RefreshedAt)[0]; item.RateKnown || item.Samples != 0 {
		t.Fatalf("forgotten account retained rate: %+v", item)
	}
}

func TestQuotaRunwayHardLimitAndReserveCap(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	var tracker quotaRunwayTracker
	var snapshot quotaSnapshot
	for index, used := range []float64{10, 30, 50} {
		at := start.Add(time.Duration(index) * time.Minute)
		snapshot = runwayTestSnapshot(at, start.Add(4*time.Hour), used)
		tracker.Observe(snapshot, at)
	}
	cfg := runwayTestReserveConfig()
	item := tracker.Assess(snapshot, cfg, snapshot.RefreshedAt.Add(time.Minute))[0]
	runwayTestNear(t, item.EffectiveReservePercent, 50)
	runwayTestNear(t, item.CacheDebitPercent, 20)
	runwayTestNear(t, item.HeadroomPercent, 0)
	cfg.Reserve5hPercent = 65
	item = tracker.Assess(snapshot, cfg, snapshot.RefreshedAt)[0]
	runwayTestNear(t, item.EffectiveReservePercent, 65)
	// Upstream hard blocks take precedence even with substantial percentage left.
	snapshot.Windows[0].Allowed = false
	item = tracker.Assess(snapshot, cfg, snapshot.RefreshedAt)[0]
	if !item.HardLimited || item.HeadroomPercent != 0 || item.Reason != "hard_limit" {
		t.Fatalf("hard limit bypassed: %+v", item)
	}
	if _, err := json.Marshal(item); err != nil {
		t.Fatalf("status must remain JSON serializable: %v", err)
	}
}

func TestQuotaRunwayWindowsRemainSeparateAndResetNeedsConfirmation(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	var tracker quotaRunwayTracker
	var snapshot quotaSnapshot
	for index := 0; index < 3; index++ {
		at := start.Add(time.Duration(index) * time.Minute)
		snapshot = runwayTestSnapshot(at, start.Add(4*time.Hour), 10+float64(index))
		snapshot.Windows = append(snapshot.Windows, quotaWindow{Class: "weekly", WindowSeconds: 604800,
			UsedPercent: 10 + float64(index)*0.1, Allowed: true, ObservedAt: at,
			ResetAt: start.Add(5 * 24 * time.Hour), Source: quotaSourceProbe})
		tracker.Observe(snapshot, at)
	}
	items := tracker.Assess(snapshot, defaultPluginConfig(), snapshot.RefreshedAt)
	if len(items) != 2 || !items[0].RateKnown || !items[1].RateKnown {
		t.Fatalf("missing independent window evidence: %+v", items)
	}
	runwayTestNear(t, items[0].BurnPercentPerHour, 60)
	runwayTestNear(t, items[1].BurnPercentPerHour, 6)
	snapshot.Windows[0].ResetAt = start.Add(time.Minute)
	item := tracker.Assess(snapshot, defaultPluginConfig(), snapshot.RefreshedAt)[0]
	if item.Fresh || item.RateKnown || item.Reason != "reset_unconfirmed" {
		t.Fatalf("expired reset treated as renewed quota: %+v", item)
	}
}

func TestQuotaRunwaySmallResetDriftCannotAccumulate(t *testing.T) {
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	var tracker quotaRunwayTracker
	snapshot := runwayTestSeed(&tracker, start)
	for index := 1; index <= 3; index++ {
		at := start.Add(time.Duration(2+index) * time.Minute)
		snapshot.RefreshedAt, snapshot.Windows[0].ObservedAt = at, at
		snapshot.Windows[0].UsedPercent++
		snapshot.Windows[0].ResetAt = snapshot.Windows[0].ResetAt.Add(2 * time.Second)
		tracker.Observe(snapshot, at)
	}
	if item := tracker.Assess(snapshot, defaultPluginConfig(), snapshot.RefreshedAt)[0]; item.RateKnown || item.Samples != 0 {
		t.Fatalf("cumulative reset drift retained old cycle: %+v", item)
	}
}

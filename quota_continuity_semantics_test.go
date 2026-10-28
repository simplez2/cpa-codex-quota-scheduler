package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestContinuityWeeklyOnlySupersedesOlderFiveHourRestriction(t *testing.T) {
	now := time.Now()
	previous, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "a", "idx", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	current, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "a", "idx", now)
	if err != nil {
		t.Fatal(err)
	}
	if current.FiveHourLimitStatus != "not_reported" {
		t.Fatalf("weekly-only response not classified: %+v", current)
	}
	merged := mergePartialQuotaSnapshot(previous, current, now, 15*time.Minute)
	for _, window := range merged.Windows {
		if normalizeWindowClass(window.Class) == "5h" {
			t.Fatalf("older exhausted 5h window survived a complete weekly-only response: %+v", merged.Windows)
		}
	}
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "a"}, merged, true, defaultPluginConfig(), now)
	if !choice.QuotaKnown || !choice.Eligible || !choice.WeeklyKnown || choice.FiveHourKnown {
		t.Fatalf("confirmed weekly-only account is not normally usable: %+v", choice)
	}
}

func TestContinuityMissingNativeSlotCannotProveFiveHourAbsence(t *testing.T) {
	now := time.Now()
	for _, body := range []string{
		`{"rate_limit":{"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`,
		`{"rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`,
	} {
		partial, err := parseNativeQuota([]byte(body), "a", "idx", now)
		if err != nil {
			t.Fatal(err)
		}
		if partial.FiveHourLimitStatus != "unknown" || poolFiveHourLimitStatus(partial, partial, now, time.Minute) != "unknown" {
			t.Fatalf("omitted slot was interpreted as absence of a limit: %+v", partial)
		}
		previous := quotaSnapshot{AuthID: "a", AuthIndex: "idx", RefreshedAt: now.Add(-time.Minute), Windows: []quotaWindow{{
			Class: "5h", WindowSeconds: 18000, UsedPercent: 100, LimitReached: true,
			Allowed: true, ResetAt: now.Add(time.Hour), ObservedAt: now.Add(-time.Minute),
		}}}
		merged := mergePartialQuotaSnapshot(previous, partial, now, time.Minute*15)
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "a"}, merged, true, defaultPluginConfig(), now)
		if choice.Eligible || choice.Reason != "limit_reached" {
			t.Fatalf("partial response erased a known 5h restriction: %+v", choice)
		}
	}
}

func TestContinuityWeeklyOnlyPreservesConcurrentFiveHourEvidence(t *testing.T) {
	now := time.Now()
	current, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "a", "idx", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, time.Second} {
		previous := quotaSnapshot{AuthID: "a", AuthIndex: "idx", RefreshedAt: now.Add(-time.Minute), Windows: []quotaWindow{{
			Class: "5h", WindowSeconds: 18000, UsedPercent: 100, Allowed: true, LimitReached: true,
			ResetAt: now.Add(time.Hour), ObservedAt: now.Add(offset), Source: quotaSourceHeader,
		}}}
		merged := mergePartialQuotaSnapshot(previous, current, now.Add(2*time.Second), 15*time.Minute)
		if got := poolFiveHourLimitStatus(current, merged, now.Add(2*time.Second), 15*time.Minute); got != "limited" {
			t.Fatalf("concurrent 5h evidence at offset %v was classified as %q", offset, got)
		}
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "a"}, merged, true, defaultPluginConfig(), now.Add(2*time.Second))
		if choice.Eligible || choice.Reason != "limit_reached" {
			t.Fatalf("concurrent 5h evidence at offset %v was lost: %+v", offset, choice)
		}
	}
}

func TestContinuityRoutesWeeklyOnlyWhenOtherAccountIsExhausted(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	for _, mode := range []string{"serial", "balanced"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			previous, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "weekly", "idx-weekly", now.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			weekly, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "weekly", "idx-weekly", now)
			if err != nil {
				t.Fatal(err)
			}
			exhausted, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "exhausted", "idx-exhausted", now)
			if err != nil {
				t.Fatal(err)
			}
			cfg := defaultPluginConfig()
			cfg.StatePath, cfg.SchedulerMode, cfg.StickySeconds = "", mode, 0
			state := &schedulerRuntimeState{
				cfg:             cfg,
				quotas:          map[string]quotaSnapshot{"weekly": mergePartialQuotaSnapshot(previous, weekly, now, cfg.StaleAfter), "exhausted": exhausted},
				quotaNative:     map[string]quotaSnapshot{"weekly": weekly, "exhausted": exhausted},
				identities:      map[string]string{"idx-weekly": "weekly", "idx-exhausted": "exhausted"},
				adqReservations: newADQReservationBook(),
			}
			req := pluginapi.SchedulerPickRequest{Provider: providerCodex, Model: "gpt-5.6-sol", Candidates: []pluginapi.SchedulerAuthCandidate{
				{ID: "exhausted", Provider: providerCodex, Attributes: map[string]string{"auth_index": "idx-exhausted"}},
				{ID: "weekly", Provider: providerCodex, Attributes: map[string]string{"auth_index": "idx-weekly"}},
			}}
			got, err := state.schedulerPick(req)
			if err != nil || !got.Handled || got.AuthID != "weekly" {
				t.Fatalf("weekly-only account not selected: response=%+v error=%v", got, err)
			}
		})
	}
}

func TestContinuityExpiredFiveHourNeedsNewObservation(t *testing.T) {
	observedAt := time.Now()
	old, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":60},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`), "a", "idx", observedAt)
	if err != nil {
		t.Fatal(err)
	}
	afterReset := observedAt.Add(61 * time.Second)
	tracker := quotaRunwayTracker{}
	assessments := tracker.Assess(old, defaultPluginConfig(), afterReset)
	if len(assessments) != 2 || assessments[0].Fresh || !assessments[0].HardLimited || assessments[0].HeadroomPercent != 0 {
		t.Fatalf("elapsed reset was mistaken for a fresh 5h refill: %+v", assessments)
	}
	recovered, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000,"reset_after_seconds":17999},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86339}}}`), "a", "idx", afterReset)
	if err != nil {
		t.Fatal(err)
	}
	assessments = tracker.Assess(recovered, defaultPluginConfig(), afterReset)
	if len(assessments) != 2 || !assessments[0].Fresh || assessments[0].HardLimited || assessments[0].HeadroomPercent <= 0 {
		t.Fatalf("newly observed refill was not recognized: %+v", assessments)
	}
}

func TestContinuitySchedulesResetFromNewNativeSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 1)
			return
		}
		_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: http.StatusOK, Body: `{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":10},"secondary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400}}}`})
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	q, ok := state.quotas["a"]
	if !ok || len(q.Windows) != 2 {
		t.Fatalf("native snapshot missing: %+v", q)
	}
	want := q.Windows[0].ResetAt.Add(2 * time.Second)
	if got := state.quotaPolls["a"].NextAt; !got.Equal(want) {
		t.Fatalf("new reset not scheduled: next=%v want=%v", got, want)
	}
}

func TestContinuityNewResetPreservesConcurrentPollGuard(t *testing.T) {
	guardUntil := time.Now().Add(30 * time.Minute)
	var state *schedulerRuntimeState
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 1)
			return
		}
		state.mu.Lock()
		poll := state.quotaPolls["a"]
		poll.NextAt = guardUntil
		state.quotaPolls["a"] = poll
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: http.StatusOK, Body: `{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":10}}}`})
	}))
	defer server.Close()
	state = newRefreshTestState(t, server.URL)
	state.refreshOnce(context.Background())
	if got := state.quotaPolls["a"].NextAt; !got.Equal(guardUntil) {
		t.Fatalf("new reset shortened a concurrent guard: got=%v want=%v", got, guardUntil)
	}
}

func TestContinuityDemandHonorsPollBackoffAfterRepeatedFailures(t *testing.T) {
	now := time.Now()
	state := &schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotaPolls: map[string]quotaPollState{"a": {
			Reason: "continuity", Error: "quota_http_429", Failures: 3, AttemptedAt: now, NextAt: now.Add(5 * time.Minute),
		}},
		continuityDemandUntil: map[string]time.Time{"a": now.Add(time.Hour)},
	}
	if got := state.quotaProbeReason("a", now.Add(time.Minute)); got != "" {
		t.Fatalf("continuity bypassed 429 retry guard: %q", got)
	}
	if got := state.quotaProbeReason("a", now.Add(5*time.Minute)); got != "continuity" {
		t.Fatalf("live demand did not resume after backoff: %q", got)
	}
}

func TestContinuityDoesNotTreatStaleOrFutureFiveHourAsConfirmed(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	full := 100.0
	stale := continuityQuota("a", now.Add(-time.Hour), &full, now.Add(time.Hour), 20)
	state := continuityStateWithQuotas(stale)
	if got := state.assessContinuity(continuityRequest("a"), now); got.Action != "wait" || got.Reason != "five_hour_recheck" {
		t.Fatalf("stale exhaustion was not identified as needing a recheck: %+v", got)
	}
	if got := state.quotaProbeReason("a", now); got != "continuity" {
		t.Fatalf("stale exhaustion did not request recheck: %q", got)
	}
	future := continuityQuota("a", now.Add(time.Minute), &full, now.Add(time.Hour), 20)
	state = continuityStateWithQuotas(future)
	if got := state.assessContinuity(continuityRequest("a"), now); got.Action != "unavailable" || got.Reason != "quota_unknown" {
		t.Fatalf("future observation was treated as known: %+v", got)
	}
}

func TestContinuityExpiredWeeklyOrMonthlyDoesNotProveFiveHourOnlyBlock(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	full := 100.0
	for _, class := range []string{"weekly", "monthly"} {
		t.Run(class, func(t *testing.T) {
			q := continuityQuota("a", now, &full, now.Add(time.Hour), 20)
			if class == "weekly" {
				q.Windows[0].UsedPercent = 100
				q.Windows[0].ResetAt = now.Add(-time.Second)
			} else {
				q.Windows = append(q.Windows, quotaWindow{Class: "monthly", WindowSeconds: 30 * 24 * 3600,
					UsedPercent: 100, Allowed: true, ResetAt: now.Add(-time.Second), ObservedAt: now, Source: quotaSourceProbe})
			}
			state := continuityStateWithQuotas(q)
			got := state.assessContinuity(continuityRequest("a"), now)
			if got.Action == "ready" || got.Reason == "five_hour_exhausted" {
				t.Fatalf("expired %s hard limit was silently treated as recovered: %+v", class, got)
			}
		})
	}
}

func TestContinuityPacingReclaimsCanceledUndispatchedWave(t *testing.T) {
	now := time.Now()
	state := &schedulerRuntimeState{}
	for _, id := range []string{"first", "second", "third"} {
		state.continuity.pace(id, time.Hour, 30*time.Second, 6*time.Hour, now)
	}
	// No request entered the native concurrency lifecycle: these terminal
	// events represent cancellation while the host was assessing or waiting.
	for _, id := range []string{"first", "second", "third"} {
		state.continuityComplete(id)
	}
	if delay := state.continuity.pace("new", time.Hour, 30*time.Second, 6*time.Hour, time.Now()); delay > 0 {
		t.Fatalf("canceled, undispatched wave left a phantom pacing delay: %v", delay)
	}
}

func TestContinuityPacingReusesPermitAfterDispatch(t *testing.T) {
	state := &schedulerRuntimeState{}
	now := time.Now()
	if delay := state.continuity.pace("same", time.Hour, 30*time.Second, 6*time.Hour, now); delay != 0 {
		t.Fatalf("first request unexpectedly delayed: %v", delay)
	}
	state.continuityDispatched("same")
	before := state.continuity.nextPermit
	if delay := state.continuity.pace("same", time.Hour, 30*time.Second, 6*time.Hour, time.Now()); delay != 0 || !state.continuity.nextPermit.Equal(before) {
		t.Fatalf("same dispatched execution consumed a second permit: delay=%v next=%v before=%v", delay, state.continuity.nextPermit, before)
	}
}

func TestContinuityPacingRetainsReservationAcrossResetJitter(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	used := 80.0
	reset := now.Add(time.Hour)
	q := continuityQuota("a", now, &used, reset, 20)
	state := continuityStateWithQuotas(q)
	for i, value := range []float64{60, 70, 80} {
		at := now.Add(time.Duration(i-2) * time.Minute)
		state.quotaRunway.Observe(continuityQuota("a", at, &value, reset, 20), at)
	}
	state.continuity.usage = map[string][]time.Time{"a": {now.Add(-2 * time.Minute), now.Add(-time.Minute), now.Add(-30 * time.Second)}}
	state.continuity.usageBindings = map[string]string{"a": "a-index"}
	first := continuityRequest("a")
	first.RequestID = "first"
	if got := state.assessContinuity(first, now); got.Action != "ready" {
		t.Fatalf("first paced execution not ready: %+v", got)
	}
	second := first
	second.RequestID = "second"
	if got := state.assessContinuity(second, now); got.Action != "wait" || got.Reason != "pacing" {
		t.Fatalf("second execution did not obtain a waiting reservation: %+v", got)
	}
	reserved := state.continuity.reservations["second"]
	later := now.Add(time.Second)
	updated := continuityQuota("a", later, &used, reset.Add(time.Second), 20)
	state.quotaRunway.Observe(updated, later)
	state.quotas["a"], state.quotas["a-index"], state.quotaNative["a"] = updated, updated, updated
	if got := state.assessContinuity(second, later); got.Action != "wait" || got.Reason != "pacing" || !state.continuity.reservations["second"].Equal(reserved) {
		t.Fatalf("one-second reset jitter erased an existing reservation: response=%+v reserved=%v current=%v", got, reserved, state.continuity.reservations["second"])
	}
}

func TestContinuityProjectedZeroHeadroomDoesNotDisablePacing(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	used := 99.0
	reset := now.Add(time.Hour)
	q := continuityQuota("a", now.Add(-time.Minute), &used, reset, 20)
	state := continuityStateWithQuotas(q)
	for i, value := range []float64{85, 92, 99} {
		at := now.Add(time.Duration(i-3) * time.Minute)
		state.quotaRunway.Observe(continuityQuota("a", at, &value, reset, 20), at)
	}
	state.continuity.usage = map[string][]time.Time{"a": {now.Add(-2 * time.Minute), now.Add(-time.Minute), now.Add(-30 * time.Second)}}
	state.continuity.usageBindings = map[string]string{"a": "a-index"}
	var five *quotaRunwayWindowAssessment
	for _, item := range state.quotaRunway.Assess(q, state.cfg, now) {
		if item.Window == "5h" {
			copy := item
			five = &copy
		}
	}
	if five == nil || !five.RateKnown || five.HardLimited || five.RemainingPercent-five.CacheDebitPercent > 0 {
		t.Fatalf("fixture did not establish measured zero projected headroom: %+v", five)
	}
	first, second := continuityRequest("a"), continuityRequest("a")
	first.RequestID, second.RequestID = "first", "second"
	_ = state.assessContinuity(first, now)
	if got := state.assessContinuity(second, now); got.Action == "ready" {
		t.Fatalf("measured zero headroom was treated as no need for pacing: %+v", got)
	}
}

func TestContinuityUnrelatedPinnedPoolIsNotDelayedByOtherQueue(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	used := 80.0
	reset := now.Add(time.Hour)
	state := continuityStateWithQuotas(continuityQuota("a", now, &used, reset, 20), continuityQuota("b", now, &used, reset, 20))
	state.continuity.usage = make(map[string][]time.Time)
	state.continuity.usageBindings = make(map[string]string)
	for _, id := range []string{"a", "b"} {
		for i, value := range []float64{60, 70, 80} {
			at := now.Add(time.Duration(i-2) * time.Minute)
			state.quotaRunway.Observe(continuityQuota(id, at, &value, reset, 20), at)
		}
		state.continuity.usage[id] = []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute), now.Add(-30 * time.Second)}
		state.continuity.usageBindings[id] = id + "-index"
	}
	first, queued := continuityRequest("a"), continuityRequest("a")
	first.RequestID, queued.RequestID = "first-a", "queued-a"
	if got := state.assessContinuity(first, now); got.Action != "ready" {
		t.Fatalf("first pool A execution was not ready: %+v", got)
	}
	if got := state.assessContinuity(queued, now); got.Action != "wait" || got.Reason != "pacing" {
		t.Fatalf("pool A did not retain its own pacing queue: %+v", got)
	}
	unrelated := continuityRequest("b")
	unrelated.RequestID = "first-b"
	if got := state.assessContinuity(unrelated, now); got.Action != "ready" {
		t.Fatalf("unrelated model/pin pool was delayed by pool A's queue: %+v", got)
	}
}

func TestContinuityFreshCycleDropsOldPacingReservations(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	used := 1.0
	state := continuityStateWithQuotas(continuityQuota("a", now, &used, now.Add(5*time.Hour), 20))
	state.continuity.setPacingCycle(map[string]continuityCycle{"a": {index: "a-index", reset: now.Add(time.Hour)}}, now)
	state.continuity.pace("old-first", time.Hour, 30*time.Second, 6*time.Hour, now)
	state.continuity.pace("old-waiter", time.Hour, 30*time.Second, 6*time.Hour, now)
	req := continuityRequest("a")
	req.RequestID = "old-waiter"
	if got := state.assessContinuity(req, now.Add(time.Second)); got.Action != "ready" {
		t.Fatalf("fresh cycle did not release the old waiter: %+v", got)
	}
	if delay := state.continuity.pace("new", time.Hour, 30*time.Second, 6*time.Hour, now.Add(time.Second)); delay > 0 {
		t.Fatalf("old cycle reservations delayed the fresh cycle: %v", delay)
	}
}

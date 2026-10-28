package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func continuityQuota(id string, observed time.Time, fivePercent *float64, fiveReset time.Time, weeklyPercent float64) quotaSnapshot {
	q := quotaSnapshot{AuthID: id, AuthIndex: id + "-index", RefreshedAt: observed,
		FiveHourLimitStatus: "not_reported", FiveHourLimitObservedAt: observed,
		Windows: []quotaWindow{{Class: "weekly", WindowSeconds: 7 * 24 * 3600, UsedPercent: weeklyPercent,
			Allowed: true, ResetAt: observed.Add(5 * 24 * time.Hour), ObservedAt: observed, Source: quotaSourceProbe}}}
	if fivePercent != nil {
		q.FiveHourLimitStatus = "limited"
		q.Windows = append(q.Windows, quotaWindow{Class: "5h", WindowSeconds: 5 * 3600, UsedPercent: *fivePercent,
			Allowed: true, ResetAt: fiveReset, ObservedAt: observed, Source: quotaSourceProbe})
	}
	return q
}

func continuityStateWithQuotas(quotas ...quotaSnapshot) *schedulerRuntimeState {
	cfg := defaultPluginConfig()
	cfg.ContinuityPollInterval = 30 * time.Second
	state := &schedulerRuntimeState{cfg: cfg, quotas: map[string]quotaSnapshot{}, quotaNative: map[string]quotaSnapshot{}, identities: map[string]string{}}
	for _, q := range quotas {
		state.quotas[q.AuthID], state.quotas[q.AuthIndex], state.quotaNative[q.AuthID] = q, q, q
		state.identities[q.AuthIndex] = q.AuthID
	}
	return state
}

func continuityRequest(ids ...string) pluginapi.SchedulerContinuityRequest {
	pick := pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-test"}
	for _, id := range ids {
		pick.Candidates = append(pick.Candidates, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "codex", Metadata: map[string]any{"auth_index": id + "-index"}})
	}
	return pluginapi.SchedulerContinuityRequest{RequestID: "execution-1", Pick: pick}
}

func TestContinuityWaitRequiresRealPostResetObservation(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	full := 100.0
	reset := now.Add(2 * time.Second)
	q := continuityQuota("limited", now, &full, reset, 20)
	// Native top-level limit flags can be shared by both windows.
	for i := range q.Windows {
		q.Windows[i].Allowed = false
		q.Windows[i].LimitReached = true
	}
	s := continuityStateWithQuotas(q)
	s.cfg.QuotaProbeOnDemand = false
	req := continuityRequest("limited")
	if got := s.assessContinuity(req, now); got.Action != "wait" || got.Reason != "five_hour_exhausted" {
		t.Fatalf("hard 5h limit: %#v", got)
	}
	later := reset.Add(3 * time.Second)
	if got := s.assessContinuity(req, later); got.Action != "wait" {
		t.Fatalf("elapsed reset was mistaken for refill: %#v", got)
	}
	if got := s.quotaProbeReason("limited", later); got != "periodic" {
		t.Fatalf("periodic probe reason = %q", got)
	}
	s.cfg.QuotaProbeOnDemand = true
	if got := s.quotaProbeReason("limited", later); got != "continuity" {
		t.Fatalf("waiting did not request a real post-reset probe: %q", got)
	}
	s.quotaPolls = map[string]quotaPollState{"limited": {NextAt: later.Add(time.Minute), Failures: 3, Error: "rate_limited"}}
	if got := s.quotaProbeReason("limited", later); got != "" {
		t.Fatalf("continuity overrode per-auth retry backoff: %q", got)
	}
	used := 4.0
	fresh := continuityQuota("limited", later, &used, later.Add(5*time.Hour), 20)
	s.quotas["limited"], s.quotaNative["limited"] = fresh, fresh
	s.quotaPolls["limited"] = quotaPollState{}
	if got := s.assessContinuity(req, later); got.Action != "ready" {
		t.Fatalf("confirmed fresh post-reset quota did not resume: %#v", got)
	}
}

func TestContinuityWeeklyOnlyAndUncertainCandidates(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	full := 100.0
	limited := continuityQuota("limited", now, &full, now.Add(time.Hour), 40)
	weekly := continuityQuota("weekly", now, nil, time.Time{}, 30)
	s := continuityStateWithQuotas(limited, weekly)
	req := continuityRequest("limited", "weekly", "unknown")
	if got := s.assessContinuity(req, now); got.Action != "ready" || got.Reason != "weekly_only_available" {
		t.Fatalf("fresh weekly-only candidate should remain available: %#v", got)
	}
	req = continuityRequest("limited", "unknown")
	if got := s.assessContinuity(req, now); got.Action != "unavailable" || got.Reason != "quota_unknown" {
		t.Fatalf("unknown candidate was interpreted as weekly-only or exhausted: %#v", got)
	}
	delete(s.quotaNative, "weekly")
	if got := s.assessContinuity(continuityRequest("weekly"), now); got.Action != "unavailable" {
		t.Fatalf("missing native data proved weekly-only: %#v", got)
	}
	s.quotaPolls = map[string]quotaPollState{"weekly": {NextAt: now.Add(30 * time.Second), Error: "rate_limited", Failures: 3}}
	if got := s.quotaProbeReason("weekly", now); got != "" {
		t.Fatalf("missing-native demand bypassed failure guard: %q", got)
	}
	if got := s.quotaProbeReason("weekly", now.Add(30*time.Second)); got != "continuity" {
		t.Fatalf("missing-native demand did not request a guarded native observation: %q", got)
	}
	s.quotaNative["weekly"] = weekly
	s.quotaPolls["weekly"] = quotaPollState{}
	if got := s.assessContinuity(continuityRequest("weekly"), now.Add(s.cfg.StaleAfter+time.Second)); got.Action != "unavailable" {
		t.Fatalf("stale native data proved weekly-only: %#v", got)
	}
}

func TestContinuityDoesNotWaitThroughOtherHardLimits(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	full := 100.0
	q := continuityQuota("both", now, &full, now.Add(time.Hour), 100)
	s := continuityStateWithQuotas(q)
	if got := s.assessContinuity(continuityRequest("both"), now); got.Action != "unavailable" {
		t.Fatalf("weekly hard limit treated as 5h-only recovery: %#v", got)
	}
	q.Windows[0].UsedPercent = 20
	q.Windows[0].Allowed = false // independent restriction on weekly window
	s = continuityStateWithQuotas(q)
	if got := s.assessContinuity(continuityRequest("both"), now); got.Action != "unavailable" {
		t.Fatalf("weekly disallowed state was ignored: %#v", got)
	}
}

func TestContinuityPacingIsMeasuredAndDeduplicatesRequests(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	used := 80.0
	q := continuityQuota("a", now, &used, now.Add(time.Hour), 20)
	s := continuityStateWithQuotas(q)
	for i, value := range []float64{60, 70, 80} {
		at := now.Add(time.Duration(i-2) * time.Minute)
		sample := continuityQuota("a", at, &value, now.Add(time.Hour), 20)
		s.quotaRunway.Observe(sample, at)
	}
	s.continuity.usage = map[string][]time.Time{"a": {now.Add(-2 * time.Minute), now.Add(-time.Minute), now.Add(-30 * time.Second)}}
	s.continuity.usageBindings = map[string]string{"a": "a-index"}
	first := continuityRequest("a")
	first.RequestID = "first"
	if got := s.assessContinuity(first, now); got.Action != "ready" {
		t.Fatalf("first measured permit: %#v", got)
	}
	next := s.continuity.nextPermit
	if got := s.assessContinuity(first, now); got.Action != "ready" || !s.continuity.nextPermit.Equal(next) {
		t.Fatalf("same execution spent two permits: %#v", got)
	}
	second := first
	second.RequestID = "second"
	if got := s.assessContinuity(second, now); got.Action != "wait" || got.Reason != "pacing" || got.RetryAfterMS <= 0 {
		t.Fatalf("second execution was not paced: %#v", got)
	}
	reserved, next := s.continuity.reservations["second"], s.continuity.nextPermit
	if got := s.assessContinuity(second, now); got.Action != "wait" || !s.continuity.reservations["second"].Equal(reserved) || !s.continuity.nextPermit.Equal(next) {
		t.Fatalf("recheck consumed a second pacing slot: %#v", got)
	}
	third := first
	third.RequestID = "third"
	if got := s.assessContinuity(third, now); got.Action != "wait" || !s.continuity.reservations["third"].After(reserved) {
		t.Fatalf("simultaneous requests share an unpaced release: %#v", got)
	}
	s.continuityComplete("third")
	if _, exists := s.continuity.reservations["third"]; exists {
		t.Fatal("terminal event retained pacing reservation")
	}
}

func TestContinuityReleasesOnlyTheDefinitive429Attempt(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	used := 25.0
	q := continuityQuota("a", now, &used, now.Add(time.Hour), 20)
	s := continuityStateWithQuotas(q)
	s.concurrency.register("failed", now)
	s.concurrency.register("other", now)
	s.concurrency.mu.Lock()
	s.concurrency.requests["failed"].Key = "auth:a"
	s.concurrency.requests["failed"].AuthID = "a"
	s.concurrency.requests["failed"].Dispatched = true
	s.concurrency.requests["other"].Key = "auth:b"
	s.concurrency.requests["other"].AuthID = "b"
	s.concurrency.requests["other"].Dispatched = true
	s.concurrency.mu.Unlock()

	// An actual classified 429 updates the header overlay before the next
	// assessment. A headerless probation is not proof of a 5h exhaustion.
	s.observeUsage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", AuthIndex: "a-index", Generate: true, Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 429}, ResponseHeaders: http.Header{
			"X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Used-Percent": {"100"},
			"X-Codex-Primary-Reset-At": {strconv.FormatInt(q.Windows[1].ResetAt.Unix(), 10)},
		}})
	banStore.record429("a", banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h", BannedAt: time.Now(), ResetAt: q.Windows[1].ResetAt}, time.Time{})
	req := continuityRequest("a")
	req.RequestID, req.PreviousAttemptRejected429 = "failed", true
	if got := s.assessContinuity(req, time.Now()); got.Action != "wait" || got.Reason != "five_hour_exhausted" {
		t.Fatalf("classified 5h 429 did not wait: %#v", got)
	}
	// A repeated assessment cannot release another execution or resurrect the
	// prior one. It keeps the same lifecycle for the eventual replacement.
	s.assessContinuity(req, time.Now())
	s.concurrency.mu.Lock()
	failed, other := *s.concurrency.requests["failed"], *s.concurrency.requests["other"]
	s.concurrency.mu.Unlock()
	if failed.Key != "" || failed.Dispatched || other.Key != "auth:b" || !other.Dispatched {
		t.Fatalf("wrong concurrency slots after 429: failed=%#v other=%#v", failed, other)
	}

	resetBanStoreForTest()
	banStore.record429("a", banEntry{Kind: banKindProbation, Phase: banPhaseCooldown, Window: "probation", BannedAt: now, ResetAt: now.Add(time.Minute)}, time.Time{})
	if got := s.assessContinuity(continuityRequest("a"), time.Now()); got.Action == "wait" {
		t.Fatalf("unclassified probation was assumed to be 5h: %#v", got)
	}
}

func TestContinuityPacingScopesDoNotClearOtherModelsOrJitter(t *testing.T) {
	now := time.Now()
	var gate continuityGate
	a := map[string]continuityCycle{"a": {index: "a-index", reset: now.Add(time.Hour)}}
	b := map[string]continuityCycle{"b": {index: "b-index", reset: now.Add(time.Hour)}}
	gate.setPacingCycle(a, now)
	gate.pace("a-first", time.Minute, 30*time.Second, 6*time.Hour, now)
	gate.pace("a-waiter", time.Minute, 30*time.Second, 6*time.Hour, now)
	waitA := gate.reservations["a-waiter"]
	gate.setPacingCycle(b, now)
	gate.pace("b-waiter", time.Minute, 30*time.Second, 6*time.Hour, now)
	waitB := gate.reservations["b-waiter"]
	if waitA.IsZero() || waitB.IsZero() || !gate.reservations["a-waiter"].Equal(waitA) {
		t.Fatal("different pinned account discarded another request's pacing reservation")
	}
	jitter := map[string]continuityCycle{"a": {index: "a-index", reset: now.Add(time.Hour + time.Second)}}
	gate.setPacingCycle(jitter, now.Add(time.Second))
	if !gate.reservations["a-waiter"].Equal(waitA) || !gate.reservations["b-waiter"].Equal(waitB) {
		t.Fatal("one-second reset jitter discarded a reservation")
	}
	newCycle := map[string]continuityCycle{"a": {index: "a-index", reset: now.Add(6 * time.Hour)}}
	gate.setPacingCycle(newCycle, now.Add(2*time.Second))
	if _, exists := gate.reservations["a-waiter"]; exists {
		t.Fatal("new cycle retained an obsolete pacing reservation")
	}
	if !gate.reservations["b-waiter"].Equal(waitB) {
		t.Fatal("one account's reset discarded another account's pacing reservation")
	}
}

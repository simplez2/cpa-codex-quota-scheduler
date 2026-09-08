package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentQuotaRefreshDoesNotQueueDuplicateInventory(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		w.Write([]byte(`{"files":[]}`))
	}))
	defer server.Close()
	s := newRefreshTestState(t, server.URL)
	done := make(chan struct{})
	go func() { s.refreshOnce(context.Background()); close(done) }()
	<-entered
	second := make(chan struct{})
	go func() { s.refreshOnce(context.Background()); close(second) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("duplicate refresh waited and queued")
	}
	close(release)
	<-done
	if calls.Load() != 1 {
		t.Fatalf("inventory calls=%d", calls.Load())
	}
}

func TestHeaderCacheNeverRenewsQuotaOrDelaysHardLimitChecks(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now.Add(-30 * time.Second))
	q := s.quotas["a"]
	q.HeaderObservedAt = now
	before := q.RefreshedAt
	deadline := quotaHeaderCacheDeadline(q, quotaPollState{}, s.cfg, time.Minute, now)
	if !deadline.After(now) || deadline.After(before.Add(s.cfg.QuotaRefreshCooldown)) || q.RefreshedAt != before {
		t.Fatal("header cache must defer within the full-probe deadline")
	}
	if got := quotaHeaderCacheDeadline(q, quotaPollState{Failures: 1}, s.cfg, time.Minute, now); !got.IsZero() {
		t.Fatal("header cache masked failed native poll")
	}
	q.Windows[0].UsedPercent = 99
	if got := quotaHeaderCacheDeadline(q, quotaPollState{}, s.cfg, time.Minute, now); !got.IsZero() {
		t.Fatal("low quota probe deferred")
	}
}

func TestBalancedPollCadenceReducesBusyHealthyAccountQueries(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now)
	s.balancedAccounts = map[string]*balancedAccount{"a": {LastPicked: now}}
	if got := s.quotaPollIntervalLocked("a", "", now); got != time.Minute {
		t.Fatalf("healthy busy cadence=%s", got)
	}
	if got := s.quotaPollIntervalLocked("b", "", now); got != 2*time.Minute {
		t.Fatalf("idle cadence=%s", got)
	}
	s.quotas["a"].Windows[0].UsedPercent = 95
	if got := s.quotaPollIntervalLocked("a", "", now); got != 30*time.Second {
		t.Fatalf("near-limit cadence=%s", got)
	}
}

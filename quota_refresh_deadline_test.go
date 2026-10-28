package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func quotaDeadlineInventory(w http.ResponseWriter, count int) {
	files := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		id := string(rune('a' + index))
		files = append(files, map[string]any{"id": id, "auth_index": "idx-" + id, "provider": "codex"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
}

func quotaDeadlineSeed(state *schedulerRuntimeState) quotaSnapshot {
	snapshot, _ := parseNativeQuota([]byte(nativeQuotaTestBody(45)), "a", "idx-a", time.Now().Add(-time.Minute))
	state.quotas["a"], state.quotas["idx-a"] = snapshot, snapshot
	state.quotaPolls["a"] = quotaPollState{AuthIndex: "idx-a"}
	state.serialActiveAuthID = "a"
	return snapshot
}

func TestQuotaRefreshDeadlineBoundsEntireBatch(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 8)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	state := newRefreshTestState(t, server.URL)
	state.cfg.RefreshInterval = time.Second
	before := quotaDeadlineSeed(state)
	started := time.Now()
	state.refreshOnce(context.Background())
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("batch timeout multiplied by account count: %s", elapsed)
	}
	if requests.Load() != 1 || state.quotaRefreshRequests != 1 {
		t.Fatalf("started a new request after batch expiration: upstream=%d recorded=%d", requests.Load(), state.quotaRefreshRequests)
	}
	poll := state.quotaPolls["a"]
	if poll.Failures != 1 || poll.Error != context.DeadlineExceeded.Error() || state.lastError != poll.Error {
		t.Fatalf("timeout not recorded as failure: poll=%+v last=%q", poll, state.lastError)
	}
	if poll.NextAt.Before(poll.AttemptedAt.Add(time.Second)) || !state.quotas["a"].RefreshedAt.Equal(before.RefreshedAt) {
		t.Fatal("timeout discarded cache or shortened cooldown")
	}
}

func TestQuotaRefreshDeadlineCancellationStopsSlowRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 8)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			close(started)
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	state := newRefreshTestState(t, server.URL)
	before := quotaDeadlineSeed(state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		state.refreshOnce(ctx)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota request never started")
	}
	state.mu.RLock()
	demandAt := state.quotaPolls["a"].AttemptedAt
	state.mu.RUnlock()
	state.recordQuotaDemand(pluginapi.UsageRecord{AuthID: "a"}, demandAt)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stopping refresh waited for slow upstream timeout")
	}
	if requests.Load() != 1 || state.quotaPolls["a"].Error != "" || state.quotaPolls["a"].Failures != 0 || !state.quotaPolls["a"].NextAt.IsZero() {
		t.Fatalf("shutdown cancellation left a failure/backoff: requests=%d poll=%+v", requests.Load(), state.quotaPolls["a"])
	}
	if !state.quotas["a"].RefreshedAt.Equal(before.RefreshedAt) {
		t.Fatal("cancellation advanced quota observation")
	}
	if !state.quotaPolls["a"].LastUsageAt.Equal(demandAt) || state.quotaProbeReason("a", demandAt) != "usage" {
		t.Fatal("cancellation discarded usage recorded while the quota request was in flight")
	}
}

func TestQuotaRefreshLegacyCanceledPollRetriesWithoutErasingRealFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 2)
			return
		}
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(40)})
	}))
	defer server.Close()
	state := newRefreshTestState(t, server.URL)
	quotaDeadlineSeed(state)
	guard := time.Now().Add(30 * time.Minute)
	state.quotaPolls["a"] = quotaPollState{AuthIndex: "idx-a", Error: context.Canceled.Error(), Failures: 1, NextAt: guard}
	state.quotaPolls["b"] = quotaPollState{AuthIndex: "idx-b", Error: "quota_http_401", Failures: 2, NextAt: guard}
	state.refreshOnce(context.Background())
	if requests.Load() != 1 || state.quotaPolls["a"].Error != "" || state.quotas["a"].Windows[0].UsedPercent != 40 {
		t.Fatalf("legacy cancellation remained deferred: requests=%d poll=%+v", requests.Load(), state.quotaPolls["a"])
	}
	if p := state.quotaPolls["b"]; p.Error != "quota_http_401" || p.Failures != 2 || !p.NextAt.Equal(guard) {
		t.Fatalf("real authentication failure guard erased: %+v", p)
	}
}

func TestQuotaRefreshDeadlineIncludesSlowInventory(t *testing.T) {
	var quotaRequests atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaRequests.Add(1)
			return
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	state := newRefreshTestState(t, server.URL)
	state.cfg.RefreshInterval = time.Second
	before := quotaDeadlineSeed(state)
	started := time.Now()
	state.refreshOnce(context.Background())
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("inventory escaped refresh budget: %s", elapsed)
	}
	if quotaRequests.Load() != 0 || state.lastError != "cpa_inventory_timeout" || !state.quotas["a"].RefreshedAt.Equal(before.RefreshedAt) {
		t.Fatalf("inventory timeout mutated cache or continued batch: requests=%d last=%q", quotaRequests.Load(), state.lastError)
	}
}

func TestQuotaRefreshDeadlineBodyTimeoutRetainsRetryAfter(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "auth-files") {
			quotaDeadlineInventory(w, 2)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	state := newRefreshTestState(t, server.URL)
	state.cfg.RefreshInterval = time.Second
	quotaDeadlineSeed(state)
	started := time.Now()
	state.refreshOnce(context.Background())
	poll := state.quotaPolls["a"]
	if poll.Error != context.DeadlineExceeded.Error() || poll.Failures != 1 || poll.NextAt.Before(started.Add(5*time.Minute)) {
		t.Fatalf("partial management response lost timeout/Retry-After: %+v", poll)
	}
}

func TestQuotaRefreshDeadlineCannotShortenNewCooldown(t *testing.T) {
	var state *schedulerRuntimeState
	guardUntil := time.Now().Add(10 * time.Minute)
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
		_ = json.NewEncoder(w).Encode(cpaAPICallResponse{StatusCode: 429, Header: map[string][]string{"Retry-After": {"300"}}})
	}))
	defer server.Close()
	state = newRefreshTestState(t, server.URL)
	quotaDeadlineSeed(state)
	state.refreshOnce(context.Background())
	if poll := state.quotaPolls["a"]; !poll.NextAt.Equal(guardUntil) || poll.Failures != 1 {
		t.Fatalf("failure shortened an already extended cooldown: %+v", poll)
	}
}

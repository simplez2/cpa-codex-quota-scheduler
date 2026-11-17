package main

import (
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestCatchupDemandRefreshesStandbyWithoutFabricatingUsageOrQuota(t *testing.T) {
	s, _, now := weightedResetFixture(t, false)
	q := s.quotas["weekly-backup"]
	q.RefreshedAt, q.FiveHourLimitObservedAt = now.Add(-20*time.Minute), now.Add(-20*time.Minute)
	for i := range q.Windows {
		q.Windows[i].ObservedAt = q.RefreshedAt
	}
	s.quotas[q.AuthID] = q
	s.quotaPolls = map[string]quotaPollState{q.AuthID: {AuthIndex: q.AuthIndex, AttemptedAt: q.RefreshedAt, NextAt: now.Add(10 * time.Minute)}}
	if got := s.quotaProbeReason(q.AuthID, now); got != "" {
		t.Fatalf("idle pool triggered a read: %s", got)
	}
	s.recordQuotaDemand(pluginapi.UsageRecord{AuthID: "five-later", AuthIndex: "five-later"}, now)
	if got := s.quotaProbeReason(q.AuthID, now); got != "weekly_catchup" {
		t.Fatalf("standby cooldown starved catch-up: %s", got)
	}
	if !s.quotaPolls[q.AuthID].LastUsageAt.IsZero() || s.quotas[q.AuthID].RefreshedAt != q.RefreshedAt {
		t.Fatal("read-only catch-up demand fabricated usage or freshness")
	}
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: q.AuthID}, q, true, s.cfg, now)
	if choice.Priority.WeeklyCatchup {
		t.Fatal("stale absence was promoted before a real observation")
	}
	p := s.quotaPolls[q.AuthID]
	p.CatchupPending, p.AttemptedAt, p.NextAt = false, now, now.Add(30*time.Minute)
	s.quotaPolls[q.AuthID] = p
	s.recordQuotaDemand(pluginapi.UsageRecord{AuthID: "five-later", AuthIndex: "five-later"}, now.Add(time.Second))
	if s.quotaPolls[q.AuthID].CatchupPending {
		t.Fatal("a second foreground request created a probe storm")
	}
}

func TestCatchupDemandPreservesErrorBackoffAndQuotaBoundary(t *testing.T) {
	for _, kind := range []string{"backoff", "weekly_reserved", "outside_window", "unknown_absence"} {
		s, _, now := weightedResetFixture(t, false)
		q := s.quotas["weekly-backup"]
		q.RefreshedAt, q.FiveHourLimitObservedAt = now.Add(-20*time.Minute), now.Add(-20*time.Minute)
		q.Windows[0].ObservedAt = q.RefreshedAt
		p := quotaPollState{AuthIndex: q.AuthIndex, AttemptedAt: q.RefreshedAt, NextAt: now.Add(10 * time.Minute)}
		switch kind {
		case "backoff":
			p.Failures, p.Error = 1, "quota_http_429"
		case "weekly_reserved":
			q.Windows[0].UsedPercent = 99
		case "outside_window":
			q.Windows[0].ResetAt = now.Add(72 * time.Hour)
		case "unknown_absence":
			q.FiveHourLimitStatus = "unknown"
		}
		s.quotas[q.AuthID] = q
		s.quotaPolls = map[string]quotaPollState{q.AuthID: p}
		s.requestWeeklyCatchupProbesLocked(now)
		if got := s.quotaProbeReason(q.AuthID, now); got != "" {
			t.Fatalf("%s bypassed a read boundary: %s", kind, got)
		}
		if kind != "backoff" && s.quotaPolls[q.AuthID].CatchupPending {
			t.Fatalf("%s acquired catch-up demand", kind)
		}
	}
}

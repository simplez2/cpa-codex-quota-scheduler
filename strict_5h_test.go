package main

import (
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func strictFiveHourState(now time.Time) schedulerRuntimeState {
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	cfg.SerialSwitchPercent = 98
	cfg.Serial5hHandoffMode = "custom_threshold"
	cfg.Serial5hSwitchPercent = 95
	return schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"primary": {
				AuthID: "primary", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), UsedPercent: 96, Allowed: true, ResetAt: now.Add(20 * time.Minute), ObservedAt: now},
					{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
				},
			},
			"backup": {
				AuthID: "backup", RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", WindowSeconds: int64((5 * time.Hour).Seconds()), UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
					{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(4 * 24 * time.Hour), ObservedAt: now},
				},
			},
		},
		warmups: make(map[string]warmupEntry),
	}
}

func strictFiveHourRequest(session string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-sol",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "primary", Provider: providerCodex, Priority: 10},
			{ID: "backup", Provider: providerCodex, Priority: 10},
		},
	}
	if session != "" {
		req.Options.Headers = map[string][]string{"X-Session-ID": {session}}
	}
	return req
}

func TestSerialSchedulerCustomFiveHourThresholdOverridesDrain(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := strictFiveHourState(now)
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"

	got := state.serialPick(strictFiveHourRequest(""), now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("strict 5h threshold did not override drain: %#v", got)
	}
	if state.serialLastSwitchReason != "serial_threshold" {
		t.Fatalf("switch reason = %q; want serial_threshold", state.serialLastSwitchReason)
	}
}

func TestSerialSchedulerCustomFiveHourThresholdMovesPinnedSession(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := strictFiveHourState(now)
	state.serialActiveAuthID = "primary"
	state.serialSelectionSource = "auto"
	req := strictFiveHourRequest("long-running-task")
	session := schedulerSessionHash(req)
	state.serialOverdraft = map[string]serialOverdraftBinding{
		session: {AuthID: "primary", LastUsedAt: now.Add(-time.Minute)},
	}

	got := state.serialPick(req, now)
	if !got.Handled || got.AuthID != "backup" {
		t.Fatalf("strict 5h threshold kept the old session on primary: %#v", got)
	}
	if binding, ok := state.serialOverdraft[session]; !ok || binding.AuthID != "backup" {
		t.Fatalf("session binding was not moved to backup: %#v", state.serialOverdraft)
	}
}

func TestSerialSchedulerInheritedFiveHourThresholdStillAllowsDrain(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := strictFiveHourState(now)
	state.cfg.Serial5hHandoffMode = "inherit_global"
	state.cfg.SerialSwitchPercent = 95
	state.serialActiveAuthID = "primary"

	got := state.serialPick(strictFiveHourRequest(""), now)
	if !got.Handled || got.AuthID != "primary" {
		t.Fatalf("legacy inherited threshold no longer drains: %#v", got)
	}
}

func TestInspectSerialCandidateIgnoresExpiredWindowForClass(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	choice := inspectSerialCandidate(
		pluginapi.SchedulerAuthCandidate{ID: "acct", Provider: providerCodex},
		quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 50, Allowed: true, ResetAt: now.Add(-time.Minute), ObservedAt: now},
			{Class: "monthly", UsedPercent: 10, Allowed: true, ResetAt: now.Add(20 * 24 * time.Hour), ObservedAt: now},
		}},
		true,
		cfg,
		now,
	)
	if !choice.QuotaKnown || !choice.Eligible || choice.WindowClass != "monthly" {
		t.Fatalf("expired 5h window affected active class: %#v", choice)
	}
}

func TestInspectSerialCandidateAllExpiredWindowsIsQuotaUnknown(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	choice := inspectSerialCandidate(
		pluginapi.SchedulerAuthCandidate{ID: "acct", Provider: providerCodex},
		quotaSnapshot{AuthID: "acct", RefreshedAt: now, Windows: []quotaWindow{
			{Class: "5h", UsedPercent: 50, Allowed: true, ResetAt: now.Add(-time.Minute), ObservedAt: now},
			{Class: "weekly", UsedPercent: 20, Allowed: true, ResetAt: now.Add(-time.Second), ObservedAt: now},
		}},
		true,
		cfg,
		now,
	)
	if choice.QuotaKnown || choice.Reason != "quota_unknown" {
		t.Fatalf("all-expired snapshot was treated as known quota: %#v", choice)
	}
}

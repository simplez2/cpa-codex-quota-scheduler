package main

import (
	"sync"
	"testing"
	"time"
)

func continuityTestState(now time.Time) *schedulerRuntimeState {
	state := newSerialTestState(now)
	state.cfg.Serial5hHandoffMode = "reserve_aware"
	state.cfg.Reserve5hPercent = 15
	for _, id := range []string{"primary", "backup"} {
		q := state.quotas[id]
		q.Windows = append(q.Windows, quotaWindow{
			Class: "5h", UsedPercent: 20, Allowed: true,
			ResetAt: now.Add(4 * time.Hour), ObservedAt: now,
		})
		state.quotas[id] = q
	}
	return &state
}

func TestSerialContinuityHandsSameSessionToBackupAtSafetyReserve(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	if state.cfg.SerialSoftContinuation || state.cfg.Serial5hHandoffMode != "reserve_aware" {
		t.Fatalf("explicit reserve policy = soft continuation %v, 5h mode %q", state.cfg.SerialSoftContinuation, state.cfg.Serial5hHandoffMode)
	}
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"ongoing-conversation"}}
	if got := state.serialPick(req, now); !got.Handled || got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	other := serialTestRequest()
	other.Options.Headers = map[string][]string{"X-Session-ID": {"other-conversation"}}
	if got := state.serialPick(other, now); got.AuthID != "primary" {
		t.Fatalf("second conversation initial pick = %#v", got)
	}
	otherHash := schedulerSessionHash(other)
	previousBindingTime := state.serialOverdraft[otherHash].LastUsedAt
	q := state.quotas["primary"]
	q.Windows[1].UsedPercent = 100 - state.cfg.Reserve5hPercent
	state.quotas["primary"] = q
	for step := 1; step <= 3; step++ {
		got := state.serialPick(req, now.Add(time.Duration(step)*time.Second))
		if !got.Handled || got.AuthID != "backup" {
			t.Fatalf("same conversation request %d returned depleted primary: %#v", step, got)
		}
	}
	if state.serialSwitches != 1 || state.serialLastSwitchReason != "serial_threshold" {
		t.Fatalf("handoff should commit once: count %d reason %q", state.serialSwitches, state.serialLastSwitchReason)
	}
	if binding := state.serialOverdraft[otherHash]; binding.AuthID != "backup" || !binding.LastUsedAt.Equal(previousBindingTime) {
		t.Fatalf("other conversation was not moved without extending its TTL: %#v", binding)
	}
}

func TestSerialContinuityIgnoresRestoredSoftBinding(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	state.serialActiveAuthID = "backup"
	state.serialSelectedAt = now
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"restored-conversation"}}
	session := schedulerSessionHash(req)
	state.serialOverdraft = map[string]serialOverdraftBinding{
		session: {AuthID: "primary", LastUsedAt: now.Add(-time.Minute)},
	}
	for step := 0; step < 2; step++ {
		if got := state.serialPick(req, now.Add(time.Duration(step)*time.Second)); !got.Handled || got.AuthID != "backup" {
			t.Fatalf("old binding intercepted the committed primary: %#v", got)
		}
	}
	if binding := state.serialOverdraft[session]; binding.AuthID != "backup" {
		t.Fatalf("old binding was not repaired: %#v", binding)
	}
	if state.serialSwitches != 0 {
		t.Fatalf("repairing a stale session must not change global primary: %d", state.serialSwitches)
	}
}

func TestSerialSoftContinuationLateHeadersCannotEraseNativeHardLimit(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	state.cfg.SerialSoftContinuation = true
	state.serialActiveAuthID = "backup"
	state.serialSelectedAt = now
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"legacy-conversation"}}
	session := schedulerSessionHash(req)
	state.serialOverdraft = map[string]serialOverdraftBinding{
		session: {AuthID: "primary", LastUsedAt: now.Add(-time.Minute)},
	}
	// A late completion still reports available quota, but the independent
	// fresh probe has already confirmed this account's 5h hard limit.
	native := state.quotas["primary"]
	native.Windows = append([]quotaWindow(nil), native.Windows...)
	native.Windows[1].UsedPercent = 100
	native.Windows[1].Allowed = false
	native.Windows[1].LimitReached = true
	native.Windows[1].Source = quotaSourceProbe
	state.quotaNative = map[string]quotaSnapshot{"primary": native}
	state.quotas["primary"].Windows[1].Source = quotaSourceMixed
	if got := state.serialPick(req, now); !got.Handled || got.AuthID != "backup" {
		t.Fatalf("legacy binding bypassed native hard limit: %#v", got)
	}
	if binding, exists := state.serialOverdraft[session]; exists && binding.AuthID != "backup" {
		t.Fatalf("exhausted legacy binding was retained: %#v", binding)
	}
	if got := state.serialPick(req, now.Add(time.Second)); !got.Handled || got.AuthID != "backup" {
		t.Fatalf("follow-up request returned to the exhausted account: %#v", got)
	}
	if state.serialSwitches != 0 || state.serialActiveAuthID != "backup" {
		t.Fatalf("repair changed global primary: %q, switches %d", state.serialActiveAuthID, state.serialSwitches)
	}
}

func TestSerialContinuityConcurrentRequestsCommitSingleHandoff(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"parallel-conversation"}}
	if got := state.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	q := state.quotas["primary"]
	q.Windows[1].UsedPercent = 100 - state.cfg.Reserve5hPercent
	state.quotas["primary"] = q
	const requests = 16
	results := make(chan string, requests)
	var workers sync.WaitGroup
	for step := 0; step < requests; step++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			got := state.serialPick(req, now.Add(time.Second))
			results <- got.AuthID
		}()
	}
	workers.Wait()
	close(results)
	for id := range results {
		if id != "backup" {
			t.Fatalf("concurrent handoff returned %q", id)
		}
	}
	if state.serialSwitches != 1 || state.serialActiveAuthID != "backup" {
		t.Fatalf("concurrent handoff committed %d switches to %q", state.serialSwitches, state.serialActiveAuthID)
	}
}

func TestSerialContinuityReserveStillServesWhenNoBackupExists(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	req := serialTestRequest()
	req.Candidates = req.Candidates[1:]
	req.Options.Headers = map[string][]string{"X-Session-ID": {"only-account"}}
	if got := state.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("initial pick = %#v", got)
	}
	q := state.quotas["primary"]
	q.Windows[1].UsedPercent = 100 - state.cfg.Reserve5hPercent
	state.quotas["primary"] = q
	if got := state.serialPick(req, now.Add(time.Second)); !got.Handled || got.AuthID != "primary" {
		t.Fatalf("soft safety reserve interrupted an otherwise usable account: %#v", got)
	}
	q.Windows[1].UsedPercent = 100
	q.Windows[1].LimitReached = true
	state.quotas["primary"] = q
	if got := state.serialPick(req, now.Add(2*time.Second)); got.Handled {
		t.Fatalf("hard limit was bypassed by soft fallback: %#v", got)
	}
}

func TestSerialContinuityExplicitPinDoesNotChangePrimary(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := continuityTestState(now)
	state.serialActiveAuthID = "backup"
	state.serialSelectedAt = now
	req := serialTestRequest()
	req.Options.Metadata = map[string]any{"pinned_auth_id": "primary"}
	q := state.quotas["primary"]
	q.Windows[1].UsedPercent = 100 - state.cfg.Reserve5hPercent
	state.quotas["primary"] = q
	if got := state.serialPick(req, now); !got.Handled || got.AuthID != "primary" {
		t.Fatalf("explicit pin was ignored: %#v", got)
	}
	if state.serialActiveAuthID != "backup" || state.serialSwitches != 0 {
		t.Fatalf("explicit pin mutated global selection: %q, switches %d", state.serialActiveAuthID, state.serialSwitches)
	}
}

func TestSerialSafetyReserveCannotBeBypassedByDrain(t *testing.T) {
	now := time.Now()
	for _, class := range []string{"5h", "weekly"} {
		t.Run(class, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.SerialHandoffMode = "reserve_aware"
			cfg.Serial5hHandoffMode = "reserve_aware"
			cfg.Reserve5hPercent = 15
			window := quotaWindow{
				Class: class, UsedPercent: 100 - reserveForWindow(cfg, class),
				Allowed: true, ObservedAt: now, ResetAt: now.Add(10 * time.Minute),
			}
			q := quotaSnapshot{AuthID: "primary", RefreshedAt: now, Windows: []quotaWindow{window}}
			choice := inspectSerialCandidate(serialTestRequest().Candidates[1], q, true, cfg, now)
			if !choice.DrainActive || choice.Eligible || choice.Reason != "serial_threshold" {
				t.Fatalf("drain bypassed safety reserve: %#v", choice)
			}
			cfg.SerialHandoffMode = "threshold_only"
			cfg.Serial5hHandoffMode = "inherit_global"
			q.Windows[0].UsedPercent = 99
			legacy := inspectSerialCandidate(serialTestRequest().Candidates[1], q, true, cfg, now)
			if !legacy.Eligible || !legacy.DrainActive {
				t.Fatalf("explicit threshold-only drain compatibility was lost: %#v", legacy)
			}
		})
	}
}

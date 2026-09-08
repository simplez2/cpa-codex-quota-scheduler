package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func serial429OnlyTestState(now time.Time) *schedulerRuntimeState {
	state := newBudgetTestState(now)
	// Keep equal weekly budgets so these tests isolate the 5h handoff policy.
	state.quotas["primary"].Windows[1].UsedPercent = 20
	state.quotas["backup"].Windows[1].UsedPercent = 20
	state.serialSelectedAt = now.Add(-time.Hour)
	return state
}

func TestSerial429OnlyDefaultsKeepCurrentAccountAt98And99Percent(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := serial429OnlyTestState(now)
	if state.cfg.Serial5hHandoffMode != "429_only" || state.cfg.Reserve5hPercent != 0 {
		t.Fatalf("5h defaults retain a reserve: mode=%q reserve=%v", state.cfg.Serial5hHandoffMode, state.cfg.Reserve5hPercent)
	}
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"keep-until-429"}}
	for step, used := range []float64{98, 99, 99.9} {
		at := now.Add(time.Duration(step) * time.Minute)
		for _, id := range []string{"primary", "backup"} {
			observeWeeklyBalanceTestQuota(state, id, at)
		}
		state.quotas["primary"].Windows[0].UsedPercent = used
		if got := state.serialPick(req, at); !got.Handled || got.AuthID != "primary" {
			t.Fatalf("%.1f%% used switched a usable account: %#v", used, got)
		}
	}
	if state.serialSwitches != 0 {
		t.Fatalf("5h soft threshold caused %d switches", state.serialSwitches)
	}
}

func TestSerial429OnlyIgnoresConfiguredAndForecastReservesInRanking(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	state := serial429OnlyTestState(now)
	// An old configured reserve must not override an explicit 429-only policy.
	state.cfg.Reserve5hPercent = 15
	for step, used := range []float64{60, 75, 90} {
		at := now.Add(time.Duration(step-2) * time.Minute)
		q := state.quotas["primary"]
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		q.RefreshedAt = at
		for i := range q.Windows {
			q.Windows[i].ObservedAt = at
			q.Windows[i].Source = quotaSourceProbe
		}
		q.Windows[0].WindowSeconds = 18000
		q.Windows[1].WindowSeconds = 604800
		q.Windows[0].UsedPercent = used
		state.quotaRunway.Observe(q, at)
		state.quotas["primary"] = q
	}
	at := now.Add(time.Minute)
	assessment := state.quotaRunway.Assess(state.quotas["primary"], state.cfg, at)[0]
	if !assessment.RateKnown || assessment.HeadroomPercent != 0 || assessment.CacheDebitPercent <= 0 {
		t.Fatalf("test did not establish an exhausted forecast: %#v", assessment)
	}
	choice := inspectSerialCandidate(serialTestRequest().Candidates[1], state.quotas["primary"], true, state.cfg, at)
	state.annotateSerialCandidateLocked(&choice, at)
	if !choice.Eligible || choice.CapacityHeadroom != 10 {
		t.Fatalf("429-only ranking deducted speculative reserve: %#v", choice)
	}
	if got := state.serialPick(serialTestRequest(), at); !got.Handled || got.AuthID != "primary" {
		t.Fatalf("forecast forced 5h handoff: %#v", got)
	}
}

func TestSerial429OnlyStillSwitchesOnAuthoritativeHardLimits(t *testing.T) {
	for _, scenario := range []string{"100_percent", "limit_reached", "disallowed", "weekly_exhausted"} {
		t.Run(scenario, func(t *testing.T) {
			resetBanStoreForTest()
			now := time.Now()
			state := serial429OnlyTestState(now)
			w := &state.quotas["primary"].Windows[0]
			switch scenario {
			case "100_percent":
				w.UsedPercent = 100
			case "limit_reached":
				w.UsedPercent, w.LimitReached = 99, true
			case "disallowed":
				w.Allowed = false
			case "weekly_exhausted":
				state.quotas["primary"].Windows[1].UsedPercent = 100
			}
			req := serialTestRequest()
			req.Options.Headers = map[string][]string{"X-Session-ID": {"hard-limited-session"}}
			state.setSerialOverdraftLocked(schedulerSessionHash(req), "primary", now)
			if got := state.serialPick(req, now); !got.Handled || got.AuthID != "backup" {
				t.Fatalf("hard constraint was ignored: %#v", got)
			}
		})
	}
}

func TestSerial429OnlyUsage429QuarantinesAndHandsSameSessionToBackup(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	state := serial429OnlyTestState(now)
	state.quotas["primary"].Windows[0].UsedPercent = 99
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"usage-429-session"}}
	if got := state.serialPick(req, now); got.AuthID != "primary" {
		t.Fatalf("initial account = %#v", got)
	}
	// Exercise the real usage callback's shared quarantine store without
	// replacing the process-global runtime locks or unrelated test state.
	schedulerRuntime.mu.Lock()
	oldCfg, oldActive, oldPacing := schedulerRuntime.cfg, schedulerRuntime.serialActiveAuthID, schedulerRuntime.pacingAccounts
	schedulerRuntime.cfg = state.cfg
	schedulerRuntime.serialActiveAuthID = ""
	schedulerRuntime.pacingAccounts = make(map[string]*accountPacingState)
	schedulerRuntime.mu.Unlock()
	t.Cleanup(func() {
		schedulerRuntime.mu.Lock()
		schedulerRuntime.cfg, schedulerRuntime.serialActiveAuthID, schedulerRuntime.pacingAccounts = oldCfg, oldActive, oldPacing
		schedulerRuntime.mu.Unlock()
	})
	raw, err := json.Marshal(pluginapi.UsageRecord{
		Provider: providerCodex, AuthID: "primary", Generate: true, Failed: true,
		RequestedAt: now, Failure: pluginapi.UsageFailure{StatusCode: http.StatusTooManyRequests},
		ResponseHeaders: http.Header{"Retry-After": {fmt.Sprint(20 * 60)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleUsage(raw); err != nil {
		t.Fatal(err)
	}
	ban, exists := banStore.lookup("primary")
	if !exists || ban.Phase != banPhaseCooldown || ban.ResetAt.Before(now.Add(20*time.Minute)) {
		t.Fatalf("usage 429 did not preserve Retry-After quarantine: %#v exists=%v", ban, exists)
	}
	for step := 0; step < 3; step++ {
		if got := state.serialPick(req, now.Add(time.Duration(step+1)*time.Second)); !got.Handled || got.AuthID != "backup" {
			t.Fatalf("request %d returned to quarantined account: %#v", step, got)
		}
	}
}

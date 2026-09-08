package main

import (
	"math"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func newBudgetTestState(now time.Time) *schedulerRuntimeState {
	s := newWeeklyBalanceTestState(now)
	s.cfg.SerialAllocationPolicy = "sustainable"
	return s
}

func TestSerialBudgetConsidersResetDeadline(t *testing.T) {
	for _, test := range []struct {
		name                      string
		primaryReset, backupReset time.Duration
		want                      string
	}{
		{"same_reset_80_beats_40", 4 * 24 * time.Hour, 4 * 24 * time.Hour, "backup"},
		{"40_tomorrow_beats_80_in_six_days", 24 * time.Hour, 6 * 24 * time.Hour, "primary"},
		{"40_six_days_loses_80_tomorrow", 6 * 24 * time.Hour, 24 * time.Hour, "backup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetBanStoreForTest()
			now := time.Now()
			s := newBudgetTestState(now)
			s.serialActiveAuthID = ""
			s.serialSelectedAt = time.Time{}
			s.quotas["primary"].Windows[1].ResetAt = now.Add(test.primaryReset)
			s.quotas["backup"].Windows[1].ResetAt = now.Add(test.backupReset)
			if got := s.serialPick(serialTestRequest(), now); got.AuthID != test.want {
				t.Fatalf("pick=%#v want=%s", got, test.want)
			}
		})
	}
}

func TestSerialBudgetRebalancesByFreshBudgetNotRawWeekly(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newBudgetTestState(now)
	s.serialActiveAuthID = "backup" // 80%, but six days left
	s.quotas["primary"].Windows[1].ResetAt = now.Add(24 * time.Hour)
	s.quotas["backup"].Windows[1].ResetAt = now.Add(6 * 24 * time.Hour)
	req := serialTestRequest()
	req.Options.Headers = map[string][]string{"X-Session-ID": {"same-budget-session"}}
	if got := s.serialPick(req, now); got.AuthID != "backup" {
		t.Fatal("unconfirmed preemption")
	}
	if e := s.serialWeeklyRebalance; e.Metric != "weekly_budget_relative_percent" || e.Confirmations != 1 || e.CandidateAuthID != "primary" {
		t.Fatalf("evidence=%#v", e)
	}
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(s, id, now.Add(2*time.Minute))
	}
	if got := s.serialPick(req, now.Add(2*time.Minute)); got.AuthID != "primary" || s.serialLastSwitchReason != "weekly_budget_rebalance" {
		t.Fatalf("pick=%#v reason=%s", got, s.serialLastSwitchReason)
	}
	if got := s.serialPick(req, now.Add(3*time.Minute)); got.AuthID != "primary" {
		t.Fatal("session returned to old primary")
	}
}

func TestSerialBudgetPlanWeightsDoNotDrainWeeklyFraction(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newBudgetTestState(now)
	s.cfg.Serial5hHandoffMode = "reserve_aware"
	s.cfg.Reserve5hPercent = 15
	s.serialActiveAuthID = ""
	s.cfg.QuotaAccountPlans = map[string]string{"primary": "pro_20x"}
	if got := s.serialPick(serialTestRequest(), now); got.AuthID != "backup" {
		t.Fatalf("20x prior made 40%% beat 80%%: %#v", got)
	}
	s.serialActiveAuthID = ""
	s.quotas["primary"].Windows[1].UsedPercent = 20
	if got := s.serialPick(serialTestRequest(), now); got.AuthID != "primary" {
		t.Fatalf("equal budget did not use 20x 5h headroom: %#v", got)
	}
	s.serialActiveAuthID = ""
	s.quotas["primary"].Windows[0].UsedPercent = 90
	if got := s.serialPick(serialTestRequest(), now); got.AuthID != "backup" {
		t.Fatalf("plan weight bypassed safety reserve: %#v", got)
	}
}

func TestSerialBudgetPlaceholderAndMissingResetAreConservative(t *testing.T) {
	now := time.Now()
	s := newBudgetTestState(now)
	q := s.quotas["primary"]
	for _, reset := range []time.Time{now.Add(time.Minute), time.Time{}, now.Add(7 * 24 * time.Hour)} {
		q.Windows[1].UsedPercent = 0
		q.Windows[1].ResetAt = reset
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "primary"}, q, true, s.cfg, now)
		rate, known := serialWeeklyBudget(choice, s.cfg, now)
		if !known || math.Abs(rate-92.0/7) > 0.000001 {
			t.Fatalf("placeholder earned expiry priority: %v %v", rate, known)
		}
	}
	q.Windows[1].UsedPercent = 60
	q.Windows[1].ResetAt = now.Add(time.Second)
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "primary"}, q, true, s.cfg, now)
	if rate, _ := serialWeeklyBudget(choice, s.cfg, now); rate != 128 {
		t.Fatalf("reset floor did not bound rate: %v", rate)
	}
}

func TestSerialBudgetBurnGuardHandoffsBeforeStaticReserve(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newBudgetTestState(now)
	s.cfg.Serial5hHandoffMode = "reserve_aware"
	s.cfg.Reserve5hPercent = 15
	s.serialSelectedAt = now // risk handoff bypasses min hold
	for step := 0; step < 3; step++ {
		at := now.Add(time.Duration(step-2) * time.Minute)
		q := s.quotas["primary"]
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		q.RefreshedAt = at
		q.Windows[0].WindowSeconds = 18000
		q.Windows[1].WindowSeconds = 604800
		for i := range q.Windows {
			q.Windows[i].Source = quotaSourceProbe
			q.Windows[i].ObservedAt = at
		}
		q.Windows[0].UsedPercent = 60 + float64(step)*5
		s.quotaRunway.Observe(q, at)
		s.quotas["primary"] = q
	}
	// 30% observed remaining > static 15%, but 1m cache debit 5% +
	// two-minute forecast 10% + static 15% leave no safe new-request headroom.
	if got := s.serialPick(serialTestRequest(), now.Add(time.Minute)); got.AuthID != "backup" {
		t.Fatalf("burn guard failed: %#v", got)
	}
	if s.serialLastSwitchReason != "serial_threshold" {
		t.Fatalf("guard became hard quota: %s", s.serialLastSwitchReason)
	}
}

func TestSerialBudgetHeaderCompletionDoesNotConfirmProbeTwice(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newBudgetTestState(now)
	s.quotaNative = map[string]quotaSnapshot{}
	for _, id := range []string{"primary", "backup"} {
		q := s.quotas[id]
		q.Windows = append([]quotaWindow(nil), q.Windows...)
		s.quotaNative[id] = q
	}
	s.serialPick(serialTestRequest(), now)
	for _, id := range []string{"primary", "backup"} {
		observeWeeklyBalanceTestQuota(s, id, now.Add(time.Minute))
		s.quotas[id].Windows[1].Source = quotaSourceMixed
	}
	if got := s.serialPick(serialTestRequest(), now.Add(time.Minute)); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 1 {
		t.Fatalf("old header confirmed new evidence: %#v %#v", got, s.serialWeeklyRebalance)
	}
}

func TestSerialBudgetConfigurationAndPlanPriors(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("quota_default_plan: team\nquota_account_plans:\n  alpha: pro_5x\n  beta: pro_20x\n  gamma: team_premium\n"))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]float64{"alpha": 5, "beta": 20, "gamma": 5, "unspecified": 1} {
		if _, weight := quotaPlanForAuth(cfg, id); weight != want {
			t.Fatalf("%s weight=%v", id, weight)
		}
	}
	if cfg.SerialAllocationPolicy != "sustainable" || cfg.SerialBudgetRebalancePercent != 20 || cfg.SerialSoftContinuation || cfg.Serial5hHandoffMode != "429_only" || cfg.Reserve5hPercent != 0 {
		t.Fatalf("defaults=%#v", cfg)
	}
	for _, raw := range []string{"quota_default_plan: pro", "quota_account_plans: {account: unknown}", "serial_budget_rebalance_percent: .nan", "serial_budget_rebalance_percent: -1", "serial_allocation_policy: imaginary"} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestSerialBudgetOtherWindowClassCannotDistortBands(t *testing.T) {
	cfg := defaultPluginConfig()
	a := serialCandidate{Candidate: pluginapi.SchedulerAuthCandidate{ID: "low-budget-pro"}, WindowClass: "5h", WeeklyKnown: true, WeeklyBudgetKnown: true, WeeklyBudgetPerDay: 2, CapacityHeadroom: 1800}
	b := serialCandidate{Candidate: pluginapi.SchedulerAuthCandidate{ID: "healthy-standard"}, WindowClass: "5h", WeeklyKnown: true, WeeklyBudgetKnown: true, WeeklyBudgetPerDay: 10, CapacityHeadroom: 80}
	c := serialCandidate{Candidate: pluginapi.SchedulerAuthCandidate{ID: "other-window"}, WindowClass: "weekly", WeeklyKnown: true, WeeklyBudgetKnown: true, WeeklyBudgetPerDay: 368, CapacityHeadroom: 0}
	for _, choices := range [][]serialCandidate{{a, b}, {c, a, b}, {b, a, c}} {
		sortSerialCandidates(choices, cfg)
		if choices[0].Candidate.ID != "healthy-standard" {
			t.Fatalf("other class distorted pool bands: %#v", choices)
		}
	}
}

func TestSerialBudgetNativeQuotaCannotBeUndoneByLateHeaders(t *testing.T) {
	resetBanStoreForTest()
	now := time.Now()
	s := newBudgetTestState(now)
	s.quotaNative = map[string]quotaSnapshot{}
	for step := 0; step < 2; step++ {
		at := now.Add(time.Duration(step) * time.Minute)
		for _, id := range []string{"primary", "backup"} {
			observeWeeklyBalanceTestQuota(s, id, at)
			q := s.quotas[id]
			q.Windows = append([]quotaWindow(nil), q.Windows...)
			q.Windows[1].UsedPercent = 60
			s.quotaNative[id] = q
			if id == "backup" {
				s.quotas[id].Windows[1].UsedPercent = 20
				s.quotas[id].Windows[1].Source = quotaSourceMixed
			}
		}
		if got := s.serialPick(serialTestRequest(), at); got.AuthID != "primary" || s.serialWeeklyRebalance.Confirmations != 0 {
			t.Fatalf("late header manufactured advantage: %#v evidence=%#v", got, s.serialWeeklyRebalance)
		}
	}
	q := s.quotaNative["backup"]
	q.Windows[1].UsedPercent = 100
	q.Windows[1].LimitReached = true
	s.quotaNative["backup"] = q
	s.serialActiveAuthID = ""
	if got := s.serialPick(serialTestRequest(), now.Add(time.Minute)); got.AuthID != "primary" {
		t.Fatal("late header erased native hard limit")
	}
}

func TestSerialBudgetDuplicateFiveHourWindowsKeepTightestHeadroom(t *testing.T) {
	now := time.Now()
	s := newBudgetTestState(now)
	s.cfg.Serial5hHandoffMode = "reserve_aware"
	s.cfg.Reserve5hPercent = 15
	q := s.quotas["primary"]
	q.Windows = append(q.Windows, quotaWindow{Class: "5h", WindowSeconds: 3600, UsedPercent: 70, Allowed: true, ObservedAt: now, ResetAt: now.Add(time.Hour)})
	for i := 0; i < 2; i++ {
		choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "primary"}, q, true, s.cfg, now)
		s.annotateSerialCandidateLocked(&choice, now)
		if choice.CapacityHeadroom != 15 {
			t.Fatalf("overwritten headroom=%v", choice.CapacityHeadroom)
		}
		q.Windows[0], q.Windows[2] = q.Windows[2], q.Windows[0]
	}
}

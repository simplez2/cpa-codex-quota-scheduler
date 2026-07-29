package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func TestSerialSchedulerPrefersWeeklyAccountBeforeMonthly(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = cfg
	schedulerRuntime.serialActiveAuthID = ""
	schedulerRuntime.mu.Unlock()
	request := pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "weekly", Provider: providerCodex},
		{ID: "five-hour", Provider: providerCodex},
		{ID: "monthly-credit", Provider: providerCodex},
	}}
	schedulerRuntime.mu.Lock()
	schedulerRuntime.quotas = map[string]quotaSnapshot{
		"weekly":         {AuthID: "weekly", RefreshedAt: now, Windows: []quotaWindow{{Class: "weekly", UsedPercent: 1, Allowed: true}}},
		"five-hour":      {AuthID: "five-hour", RefreshedAt: now, Windows: []quotaWindow{{Class: "5h", UsedPercent: 90, Allowed: true}}},
		"monthly-credit": {AuthID: "monthly-credit", ResetCredits: 1, RefreshedAt: now, Windows: []quotaWindow{{Class: "monthly", UsedPercent: 1, Allowed: true}}},
	}
	schedulerRuntime.mu.Unlock()
	got, err := schedulerRuntime.schedulerPick(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthID != "weekly" || !got.Handled {
		t.Fatalf("selected %q, handled=%v; want weekly/true", got.AuthID, got.Handled)
	}
}

func TestSchedulerFallsBackWhenQuotaIsStale(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "legacy"
	cfg.StatePath = ""
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = cfg
	schedulerRuntime.quotas = map[string]quotaSnapshot{}
	schedulerRuntime.mu.Unlock()
	got, err := schedulerRuntime.schedulerPick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: providerCodex}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Handled {
		t.Fatal("stale/missing quota must preserve native scheduler")
	}
}

func TestClassifyBanAcceptsResetAfterHeaders(t *testing.T) {
	h := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Window-Minutes":      []string{"300"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"60"},
	}
	entry, ok := classifyAndBuildBan(h)
	if !ok || entry.Window != "5h" || entry.ResetAt.Before(time.Now().Add(50*time.Second)) {
		t.Fatalf("unexpected ban entry: %#v, ok=%v", entry, ok)
	}
}

func TestObserveUsageMergesOnlyPresentHeaderFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	resetAt := now.Add(6 * time.Hour)
	refreshedAt := now.Add(-time.Minute)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotas: map[string]quotaSnapshot{
			"acct": {
				AuthID:      "acct",
				RefreshedAt: refreshedAt,
				Windows: []quotaWindow{{
					Class:         "weekly",
					WindowSeconds: 7 * 24 * 60 * 60,
					UsedPercent:   7,
					Allowed:       true,
					ResetAt:       resetAt,
					Source:        quotaSourceKeeper,
				}},
			},
		},
		identities: map[string]string{},
	}

	state.observeUsage(pluginapi.UsageRecord{
		Provider: providerCodex,
		AuthID:   "acct",
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes": []string{"10080"},
		},
	})
	got := state.quotas["acct"]
	if got.Windows[0].UsedPercent != 7 || !got.Windows[0].ResetAt.Equal(resetAt) {
		t.Fatalf("minutes-only header overwrote Keeper values: %#v", got.Windows[0])
	}
	if !got.RefreshedAt.Equal(refreshedAt) || !got.HeaderObservedAt.IsZero() {
		t.Fatalf("minutes-only header changed freshness: keeper=%v header=%v", got.RefreshedAt, got.HeaderObservedAt)
	}

	state.observeUsage(pluginapi.UsageRecord{
		Provider: providerCodex,
		AuthID:   "acct",
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Used-Percent": []string{"9"},
		},
	})
	got = state.quotas["acct"]
	if got.Windows[0].UsedPercent != 9 || !got.Windows[0].ResetAt.Equal(resetAt) {
		t.Fatalf("used-percent patch did not preserve reset: %#v", got.Windows[0])
	}
	if !got.RefreshedAt.Equal(refreshedAt) || got.HeaderObservedAt.IsZero() {
		t.Fatalf("header signal freshness was not separated: keeper=%v header=%v", got.RefreshedAt, got.HeaderObservedAt)
	}
	if got.Windows[0].Source != quotaSourceMixed {
		t.Fatalf("merged source=%q; want mixed", got.Windows[0].Source)
	}
}

func TestSchedulerRejectsCredentialWhenAnyActiveWindowIsExhausted(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"blocked": {
				AuthID:      "blocked",
				RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(time.Hour)},
					{Class: "weekly", UsedPercent: 1, Allowed: true, ResetAt: now.Add(24 * time.Hour)},
				},
			},
			"healthy": {
				AuthID:      "healthy",
				RefreshedAt: now,
				Windows: []quotaWindow{
					{Class: "5h", UsedPercent: 20, Allowed: true, ResetAt: now.Add(time.Hour)},
					{Class: "weekly", UsedPercent: 30, Allowed: true, ResetAt: now.Add(24 * time.Hour)},
				},
			},
		},
	}
	banStore = banState{bans: make(map[string]banEntry)}
	got, err := state.schedulerPick(pluginapi.SchedulerPickRequest{Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "blocked", Provider: providerCodex},
		{ID: "healthy", Provider: providerCodex},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "healthy" {
		t.Fatalf("selected %q handled=%v; exhausted 5h window must exclude blocked", got.AuthID, got.Handled)
	}
}

func TestExpiredWindowDoesNotKeepPreviousCycleExhaustionActive(t *testing.T) {
	now := time.Now()
	state := schedulerRuntimeState{cfg: defaultPluginConfig()}
	evaluation := state.evaluateQuotaSnapshot(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(-time.Minute)},
		{Class: "weekly", UsedPercent: 25, Allowed: true, ResetAt: now.Add(24 * time.Hour)},
	}}, now)
	if !evaluation.Known || !evaluation.Eligible || evaluation.Bottleneck.Class != "weekly" || evaluation.ActiveWindows != 1 {
		t.Fatalf("unexpected evaluation after reset: %#v", evaluation)
	}
}

func TestAllExpiredWindowsFallBackToNativeScheduling(t *testing.T) {
	now := time.Now()
	state := schedulerRuntimeState{cfg: defaultPluginConfig()}
	evaluation := state.evaluateQuotaSnapshot(quotaSnapshot{Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 100, Allowed: true, LimitReached: true, ResetAt: now.Add(-time.Minute)},
	}}, now)
	if evaluation.Known || evaluation.Eligible || evaluation.Reason != "no_active_windows" {
		t.Fatalf("all-expired snapshot must be unknown: %#v", evaluation)
	}
}

func TestSchedulerConfigDefaultsToSerialAndValidatesThreshold(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("scheduler_mode: invalid\nsticky_seconds: -1\nnormal_cost_quantile: 0.99\nguard_cost_quantile: 0.5\nhigh_cost_quantile: 0.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchedulerMode != "serial" || cfg.SerialSwitchPercent != 98 || cfg.StickySeconds != 1500 {
		t.Fatalf("unsafe mode/sticky defaults: mode=%q sticky=%d", cfg.SchedulerMode, cfg.StickySeconds)
	}
	if cfg.NormalCostQuantile != 0.75 || cfg.GuardCostQuantile != 0.90 || cfg.HighCostQuantile != 0.95 {
		t.Fatalf("invalid quantiles were not reset: %#v", cfg)
	}
}

func TestSerialConfigExampleParsesWithSafeRolloutValues(t *testing.T) {
	raw, err := os.ReadFile("SERIAL_CONFIG.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Plugins struct {
			Configs map[string]yamlPluginConfig `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	pluginYAML, ok := document.Plugins.Configs[pluginName]
	if !ok {
		t.Fatalf("missing %s config", pluginName)
	}
	encoded, err := yaml.Marshal(pluginYAML)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := parsePluginConfig(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchedulerMode != "serial" || cfg.SerialSwitchPercent != 98 || !cfg.SerialPreferActiveCycle || !cfg.WarmupEnabled {
		t.Fatalf("unsafe serial example: mode=%q threshold=%v active_cycle=%v warmup=%v", cfg.SchedulerMode, cfg.SerialSwitchPercent, cfg.SerialPreferActiveCycle, cfg.WarmupEnabled)
	}
	if cfg.WarmupModel != "gpt-5.6-sol" || strings.Join(cfg.WindowOrder, ",") != "5h,weekly,monthly" {
		t.Fatalf("unexpected warmup/window config: model=%q order=%v", cfg.WarmupModel, cfg.WindowOrder)
	}
}

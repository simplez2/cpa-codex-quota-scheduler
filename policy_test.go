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

func TestSerialSchedulerFollowsConfiguredWindowOrder(t *testing.T) {
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
	if got.AuthID != "five-hour" || !got.Handled {
		t.Fatalf("selected %q, handled=%v; want five-hour/true", got.AuthID, got.Handled)
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

func TestClassifyBanUsesDurationForMonthlyPrimaryWindow(t *testing.T) {
	now := time.Now()
	h := http.Header{
		"X-Codex-Primary-Used-Percent":        []string{"100"},
		"X-Codex-Primary-Window-Minutes":      []string{"43800"},
		"X-Codex-Primary-Reset-After-Seconds": []string{"2000000"},
	}
	entry, ok := classifyAndBuildBanAt(h, now)
	if !ok || entry.Window != "monthly" {
		t.Fatalf("monthly primary ban = %#v, ok=%v", entry, ok)
	}
	if entry.ResetAt.Before(now.Add(20 * 24 * time.Hour)) {
		t.Fatalf("monthly reset was truncated: %v", entry.ResetAt)
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
		Generate: true,
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
		Generate: true,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Used-Percent": []string{"9"},
		},
	})
	got = state.quotas["acct"]
	if got.Windows[0].UsedPercent != 7 || !got.Windows[0].ResetAt.Equal(resetAt) {
		t.Fatalf("durationless patch overwrote Keeper values: %#v", got.Windows[0])
	}
	if !got.RefreshedAt.Equal(refreshedAt) || !got.HeaderObservedAt.IsZero() {
		t.Fatalf("durationless patch changed freshness: keeper=%v header=%v", got.RefreshedAt, got.HeaderObservedAt)
	}

	state.observeUsage(pluginapi.UsageRecord{
		Provider: providerCodex,
		AuthID:   "acct",
		Generate: true,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes": []string{"10080"},
			"X-Codex-Secondary-Used-Percent":   []string{"9"},
		},
	})
	got = state.quotas["acct"]
	if got.Windows[0].UsedPercent != 9 || !got.Windows[0].ResetAt.Equal(resetAt) {
		t.Fatalf("validated patch did not preserve reset: %#v", got.Windows[0])
	}
	if !got.RefreshedAt.Equal(refreshedAt) || got.HeaderObservedAt.IsZero() {
		t.Fatalf("validated header freshness was not separated: keeper=%v header=%v", got.RefreshedAt, got.HeaderObservedAt)
	}
	if got.Windows[0].Source != quotaSourceMixed {
		t.Fatalf("merged source=%q; want mixed", got.Windows[0].Source)
	}
}

func TestObserveUsageIgnoresZeroDurationPlaceholder(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	state := schedulerRuntimeState{
		cfg: defaultPluginConfig(),
		quotas: map[string]quotaSnapshot{
			"acct": {
				AuthID:      "acct",
				RefreshedAt: now,
				Windows: []quotaWindow{{
					Class:         "weekly",
					WindowSeconds: 7 * 24 * 60 * 60,
					UsedPercent:   92,
					Allowed:       true,
					Source:        quotaSourceKeeper,
				}},
			},
		},
		identities: map[string]string{},
	}

	state.observeUsage(pluginapi.UsageRecord{
		Provider: providerCodex,
		AuthID:   "acct",
		Generate: true,
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes": []string{"0"},
			"X-Codex-Secondary-Used-Percent":   []string{"0"},
		},
	})

	got := state.quotas["acct"]
	if len(got.Windows) != 1 || got.Windows[0].UsedPercent != 92 || got.Windows[0].Source != quotaSourceKeeper {
		t.Fatalf("placeholder corrupted Keeper quota: %#v", got.Windows)
	}
}

func TestSerialSchedulerPrefersResetCreditWithinWindowClass(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	cfg.StatePath = ""
	state := schedulerRuntimeState{
		cfg: cfg,
		quotas: map[string]quotaSnapshot{
			"active": {
				AuthID: "active", RefreshedAt: now,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 50, Allowed: true}},
			},
			"credit": {
				AuthID: "credit", RefreshedAt: now, ResetCredits: 1,
				Windows: []quotaWindow{{Class: "weekly", UsedPercent: 0, Allowed: true}},
			},
		},
	}
	got, err := state.schedulerPick(pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "active", Provider: providerCodex},
			{ID: "credit", Provider: providerCodex},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Handled || got.AuthID != "credit" {
		t.Fatalf("selected %#v; want reset-credit account", got)
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
	if cfg.WarmupExecutionMode != "management" {
		t.Fatalf("unsafe warmup transport default = %q; want management", cfg.WarmupExecutionMode)
	}
	native, err := parsePluginConfig([]byte("warmup_execution_mode: native\n"))
	if err != nil {
		t.Fatal(err)
	}
	if native.WarmupExecutionMode != "native" {
		t.Fatalf("explicit native warmup mode = %q", native.WarmupExecutionMode)
	}
}

func TestWarmupModelAcceptsFutureIDsAndRejectsHeaderUnsafeValues(t *testing.T) {
	const futureModel = "openai/gpt-6.preview:2027"
	cfg, err := parsePluginConfig([]byte("warmup_model: " + futureModel + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WarmupModel != futureModel {
		t.Fatalf("future warmup model = %q; want %q", cfg.WarmupModel, futureModel)
	}

	invalid := map[string]string{
		"crlf":        "gpt-safe\r\nX-Injected: true",
		"space":       "gpt future",
		"unicode":     "gpt-未来",
		"too_long":    strings.Repeat("a", maxWarmupModelLength+1),
		"header_meta": "gpt-safe;unsafe",
	}
	for name, model := range invalid {
		t.Run(name, func(t *testing.T) {
			raw, err := yaml.Marshal(yamlPluginConfig{WarmupModel: model})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parsePluginConfig(raw); err == nil {
				t.Fatalf("unsafe warmup model %q was accepted", model)
			}
		})
	}
}

func TestPluginRegistrationExposesKeeperRefreshCooldown(t *testing.T) {
	registration := pluginRegistration()
	matched := 0
	for _, field := range registration.Metadata.ConfigFields {
		if field.Name != "keeper_refresh_cooldown" {
			continue
		}
		matched++
		if field.Type != pluginapi.ConfigFieldTypeString {
			t.Fatalf("keeper_refresh_cooldown type = %q; want string", field.Type)
		}
		if !strings.Contains(field.Description, "targeted Keeper quota refresh") || !strings.Contains(field.Description, "2m") {
			t.Fatalf("keeper_refresh_cooldown description = %q", field.Description)
		}
	}
	if matched != 1 {
		t.Fatalf("keeper_refresh_cooldown registration count = %d; want 1", matched)
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
	if cfg.WarmupExecutionMode != "management" || cfg.WarmupModel != "gpt-5.6-luna" || strings.Join(cfg.WindowOrder, ",") != "5h,weekly,monthly" {
		t.Fatalf("unexpected warmup/window config: mode=%q model=%q order=%v", cfg.WarmupExecutionMode, cfg.WarmupModel, cfg.WindowOrder)
	}
}

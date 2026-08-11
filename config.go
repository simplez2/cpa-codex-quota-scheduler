package main

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginConfig is deliberately made up of ordinary scalar values.  CPA sends
// the plugin configuration as YAML on every register/reconfigure call, while
// duration values are most convenient and least surprising when written as
// strings ("30s", "15m", ...).
type pluginConfig struct {
	Enabled                 bool
	Priority                int
	SchedulerMode           string
	SerialSwitchPercent     float64
	SerialPreferActiveCycle bool
	KeeperURL               string
	KeeperPasswordFile      string
	CPAManagementURL        string
	CPAManagementKeyFile    string
	WarmupEnabled           bool
	WarmupExecutionMode     string
	WarmupModel             string
	WarmupSidecarURL        string
	WarmupRetryAfter        time.Duration
	KeeperRefreshCooldown   time.Duration
	RefreshInterval         time.Duration
	StaleAfter              time.Duration
	StatePath               string
	SoftLimitPercent        float64
	Reserve5hPercent        float64
	ReserveWeeklyPercent    float64
	ReserveMonthlyPercent   float64
	LowQuotaPercent         float64
	FallbackBan             time.Duration
	MaxBan                  time.Duration
	HalfOpenProbeTimeout    time.Duration
	HalfOpenRetryAfter      time.Duration
	StickySeconds           int
	SwitchHysteresisPercent float64
	SwitchConfirmations     int
	CostSampleLimit         int
	DecisionHistoryLimit    int
	NormalCostQuantile      float64
	GuardCostQuantile       float64
	HighCostQuantile        float64
	ShadowLogInterval       time.Duration
	PreferResetCredits      bool
	WindowOrder             []string
}

type yamlPluginConfig struct {
	Enabled                 *bool    `yaml:"enabled"`
	Priority                *int     `yaml:"priority"`
	SchedulerMode           string   `yaml:"scheduler_mode"`
	SerialSwitchPercent     *float64 `yaml:"serial_switch_percent"`
	SerialPreferActiveCycle *bool    `yaml:"serial_prefer_active_cycle"`
	KeeperURL               string   `yaml:"keeper_url"`
	KeeperPasswordFile      string   `yaml:"keeper_password_file"`
	CPAManagementURL        string   `yaml:"cpa_management_url"`
	CPAManagementKeyFile    string   `yaml:"cpa_management_key_file"`
	WarmupEnabled           *bool    `yaml:"warmup_enabled"`
	WarmupExecutionMode     string   `yaml:"warmup_execution_mode"`
	WarmupModel             string   `yaml:"warmup_model"`
	WarmupSidecarURL        string   `yaml:"warmup_sidecar_url"`
	WarmupRetryAfter        string   `yaml:"warmup_retry_after"`
	KeeperRefreshCooldown   string   `yaml:"keeper_refresh_cooldown"`
	RefreshInterval         string   `yaml:"refresh_interval"`
	StaleAfter              string   `yaml:"stale_after"`
	StatePath               string   `yaml:"state_path"`
	SoftLimitPercent        *float64 `yaml:"soft_limit_percent"`
	Reserve5hPercent        *float64 `yaml:"reserve_5h_percent"`
	ReserveWeeklyPercent    *float64 `yaml:"reserve_weekly_percent"`
	ReserveMonthlyPercent   *float64 `yaml:"reserve_monthly_percent"`
	LowQuotaPercent         *float64 `yaml:"low_quota_percent"`
	FallbackBan             string   `yaml:"fallback_ban"`
	MaxBan                  string   `yaml:"max_ban"`
	HalfOpenProbeTimeout    string   `yaml:"half_open_probe_timeout"`
	HalfOpenRetryAfter      string   `yaml:"half_open_retry_after"`
	StickySeconds           *int     `yaml:"sticky_seconds"`
	SwitchHysteresisPercent *float64 `yaml:"switch_hysteresis_percent"`
	SwitchConfirmations     *int     `yaml:"switch_confirmations"`
	CostSampleLimit         *int     `yaml:"cost_sample_limit"`
	DecisionHistoryLimit    *int     `yaml:"decision_history_limit"`
	NormalCostQuantile      *float64 `yaml:"normal_cost_quantile"`
	GuardCostQuantile       *float64 `yaml:"guard_cost_quantile"`
	HighCostQuantile        *float64 `yaml:"high_cost_quantile"`
	ShadowLogInterval       string   `yaml:"shadow_log_interval"`
	PreferResetCredits      *bool    `yaml:"prefer_reset_credits"`
	WindowOrder             []string `yaml:"window_order"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:                 true,
		SchedulerMode:           "serial",
		SerialSwitchPercent:     98,
		SerialPreferActiveCycle: true,
		KeeperPasswordFile:      "/run/secrets/keeper_login_password",
		CPAManagementURL:        "http://127.0.0.1:8317/v0/management/api-call",
		CPAManagementKeyFile:    "/run/secrets/management_key",
		WarmupExecutionMode:     "management",
		WarmupModel:             "gpt-5.6-luna",
		WarmupSidecarURL:        "http://codex-agent-identity-gateway:8787/backend-api/codex",
		WarmupRetryAfter:        15 * time.Minute,
		KeeperRefreshCooldown:   2 * time.Minute,
		RefreshInterval:         30 * time.Second,
		StaleAfter:              15 * time.Minute,
		StatePath:               "/var/lib/codex-quota-scheduler/state.json",
		SoftLimitPercent:        98,
		Reserve5hPercent:        15,
		ReserveWeeklyPercent:    8,
		ReserveMonthlyPercent:   12,
		LowQuotaPercent:         20,
		FallbackBan:             15 * time.Minute,
		MaxBan:                  24 * time.Hour,
		HalfOpenProbeTimeout:    15 * time.Minute,
		HalfOpenRetryAfter:      2 * time.Minute,
		StickySeconds:           1500,
		SwitchHysteresisPercent: 2,
		SwitchConfirmations:     3,
		CostSampleLimit:         512,
		DecisionHistoryLimit:    100,
		NormalCostQuantile:      0.75,
		GuardCostQuantile:       0.90,
		HighCostQuantile:        0.95,
		ShadowLogInterval:       5 * time.Minute,
		PreferResetCredits:      true,
		WindowOrder:             []string{"5h", "weekly", "monthly"},
	}
}

func parsePluginConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	var in yamlPluginConfig
	if err := yaml.Unmarshal(raw, &in); err != nil {
		return cfg, err
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Priority != nil {
		cfg.Priority = *in.Priority
	}
	if strings.TrimSpace(in.SchedulerMode) != "" {
		cfg.SchedulerMode = strings.ToLower(strings.TrimSpace(in.SchedulerMode))
	}
	if in.SerialSwitchPercent != nil {
		cfg.SerialSwitchPercent = *in.SerialSwitchPercent
	}
	if in.SerialPreferActiveCycle != nil {
		cfg.SerialPreferActiveCycle = *in.SerialPreferActiveCycle
	}
	if strings.TrimSpace(in.KeeperURL) != "" {
		cfg.KeeperURL = strings.TrimSpace(in.KeeperURL)
	}
	if strings.TrimSpace(in.KeeperPasswordFile) != "" {
		cfg.KeeperPasswordFile = strings.TrimSpace(in.KeeperPasswordFile)
	}
	if strings.TrimSpace(in.CPAManagementURL) != "" {
		cfg.CPAManagementURL = strings.TrimSpace(in.CPAManagementURL)
	}
	if strings.TrimSpace(in.CPAManagementKeyFile) != "" {
		cfg.CPAManagementKeyFile = strings.TrimSpace(in.CPAManagementKeyFile)
	}
	if in.WarmupEnabled != nil {
		cfg.WarmupEnabled = *in.WarmupEnabled
	}
	if strings.TrimSpace(in.WarmupExecutionMode) != "" {
		cfg.WarmupExecutionMode = strings.TrimSpace(in.WarmupExecutionMode)
	}
	if strings.TrimSpace(in.WarmupModel) != "" {
		cfg.WarmupModel = strings.TrimSpace(in.WarmupModel)
	}
	if strings.TrimSpace(in.WarmupSidecarURL) != "" {
		cfg.WarmupSidecarURL = strings.TrimRight(strings.TrimSpace(in.WarmupSidecarURL), "/")
	}
	if v, ok := parseDuration(in.WarmupRetryAfter); ok {
		cfg.WarmupRetryAfter = v
	}
	if v, ok := parseDuration(in.KeeperRefreshCooldown); ok {
		cfg.KeeperRefreshCooldown = v
	}
	if v, ok := parseDuration(in.RefreshInterval); ok {
		cfg.RefreshInterval = v
	}
	if v, ok := parseDuration(in.StaleAfter); ok {
		cfg.StaleAfter = v
	}
	if strings.TrimSpace(in.StatePath) != "" {
		cfg.StatePath = strings.TrimSpace(in.StatePath)
	}
	if in.SoftLimitPercent != nil {
		cfg.SoftLimitPercent = *in.SoftLimitPercent
	}
	if in.Reserve5hPercent != nil {
		cfg.Reserve5hPercent = *in.Reserve5hPercent
	}
	if in.ReserveWeeklyPercent != nil {
		cfg.ReserveWeeklyPercent = *in.ReserveWeeklyPercent
	}
	if in.ReserveMonthlyPercent != nil {
		cfg.ReserveMonthlyPercent = *in.ReserveMonthlyPercent
	}
	if in.LowQuotaPercent != nil {
		cfg.LowQuotaPercent = *in.LowQuotaPercent
	}
	if v, ok := parseDuration(in.FallbackBan); ok {
		cfg.FallbackBan = v
	}
	if v, ok := parseDuration(in.MaxBan); ok {
		cfg.MaxBan = v
	}
	if v, ok := parseDuration(in.HalfOpenProbeTimeout); ok {
		cfg.HalfOpenProbeTimeout = v
	}
	if v, ok := parseDuration(in.HalfOpenRetryAfter); ok {
		cfg.HalfOpenRetryAfter = v
	}
	if in.StickySeconds != nil {
		cfg.StickySeconds = *in.StickySeconds
	}
	if in.SwitchHysteresisPercent != nil {
		cfg.SwitchHysteresisPercent = *in.SwitchHysteresisPercent
	}
	if in.SwitchConfirmations != nil {
		cfg.SwitchConfirmations = *in.SwitchConfirmations
	}
	if in.CostSampleLimit != nil {
		cfg.CostSampleLimit = *in.CostSampleLimit
	}
	if in.DecisionHistoryLimit != nil {
		cfg.DecisionHistoryLimit = *in.DecisionHistoryLimit
	}
	if in.NormalCostQuantile != nil {
		cfg.NormalCostQuantile = *in.NormalCostQuantile
	}
	if in.GuardCostQuantile != nil {
		cfg.GuardCostQuantile = *in.GuardCostQuantile
	}
	if in.HighCostQuantile != nil {
		cfg.HighCostQuantile = *in.HighCostQuantile
	}
	if v, ok := parseDuration(in.ShadowLogInterval); ok {
		cfg.ShadowLogInterval = v
	}
	if in.PreferResetCredits != nil {
		cfg.PreferResetCredits = *in.PreferResetCredits
	}
	if len(in.WindowOrder) > 0 {
		cfg.WindowOrder = normalizeWindowOrder(in.WindowOrder)
	}

	// Keep malformed/unsafe values from turning a configuration reload into a
	// routing outage.  The plugin remains loaded and falls back to CPA's native
	// scheduler until a usable Keeper snapshot is available.
	cfg.SchedulerMode = normalizeSchedulerMode(cfg.SchedulerMode)
	cfg.WarmupExecutionMode = normalizeWarmupExecutionMode(cfg.WarmupExecutionMode)
	if cfg.RefreshInterval < time.Second {
		cfg.RefreshInterval = time.Second
	}
	if cfg.WarmupRetryAfter < time.Minute {
		cfg.WarmupRetryAfter = 15 * time.Minute
	}
	if cfg.KeeperRefreshCooldown < 30*time.Second || cfg.KeeperRefreshCooldown > 24*time.Hour {
		cfg.KeeperRefreshCooldown = 2 * time.Minute
	}
	if cfg.WarmupModel == "" {
		cfg.WarmupModel = "gpt-5.6-luna"
	}
	if cfg.WarmupSidecarURL == "" {
		cfg.WarmupSidecarURL = "http://codex-agent-identity-gateway:8787/backend-api/codex"
	}
	if cfg.StaleAfter < cfg.RefreshInterval {
		cfg.StaleAfter = 15 * time.Minute
	}
	if cfg.FallbackBan <= 0 {
		cfg.FallbackBan = 15 * time.Minute
	}
	if cfg.MaxBan < cfg.FallbackBan {
		cfg.MaxBan = 24 * time.Hour
	}
	if cfg.HalfOpenProbeTimeout < time.Minute || cfg.HalfOpenProbeTimeout > 2*time.Hour {
		cfg.HalfOpenProbeTimeout = 15 * time.Minute
	}
	if cfg.HalfOpenRetryAfter < time.Second || cfg.HalfOpenRetryAfter > cfg.FallbackBan {
		cfg.HalfOpenRetryAfter = 2 * time.Minute
	}
	if cfg.SoftLimitPercent <= 0 || cfg.SoftLimitPercent > 100 {
		cfg.SoftLimitPercent = 98
	}
	if cfg.SerialSwitchPercent <= 0 || cfg.SerialSwitchPercent > 100 {
		cfg.SerialSwitchPercent = 98
	}
	if cfg.Reserve5hPercent < 0 || cfg.Reserve5hPercent >= 100 {
		cfg.Reserve5hPercent = 15
	}
	if cfg.ReserveWeeklyPercent < 0 || cfg.ReserveWeeklyPercent >= 100 {
		cfg.ReserveWeeklyPercent = 8
	}
	if cfg.ReserveMonthlyPercent < 0 || cfg.ReserveMonthlyPercent >= 100 {
		cfg.ReserveMonthlyPercent = 12
	}
	if cfg.LowQuotaPercent <= 0 || cfg.LowQuotaPercent > 100 {
		cfg.LowQuotaPercent = 20
	}
	if cfg.StickySeconds < 0 {
		cfg.StickySeconds = 1500
	}
	if cfg.SwitchHysteresisPercent < 0 || cfg.SwitchHysteresisPercent > 100 {
		cfg.SwitchHysteresisPercent = 2
	}
	if cfg.SwitchConfirmations < 1 || cfg.SwitchConfirmations > 100 {
		cfg.SwitchConfirmations = 3
	}
	if cfg.CostSampleLimit < 32 || cfg.CostSampleLimit > 100000 {
		cfg.CostSampleLimit = 512
	}
	if cfg.DecisionHistoryLimit < 1 || cfg.DecisionHistoryLimit > 10000 {
		cfg.DecisionHistoryLimit = 100
	}
	if cfg.NormalCostQuantile <= 0 || cfg.NormalCostQuantile > 1 ||
		cfg.GuardCostQuantile <= 0 || cfg.GuardCostQuantile > 1 ||
		cfg.HighCostQuantile <= 0 || cfg.HighCostQuantile > 1 ||
		cfg.NormalCostQuantile > cfg.GuardCostQuantile || cfg.GuardCostQuantile > cfg.HighCostQuantile {
		cfg.NormalCostQuantile = 0.75
		cfg.GuardCostQuantile = 0.90
		cfg.HighCostQuantile = 0.95
	}
	if cfg.ShadowLogInterval < 0 {
		cfg.ShadowLogInterval = 5 * time.Minute
	}
	if len(cfg.WindowOrder) == 0 {
		cfg.WindowOrder = []string{"5h", "weekly", "monthly"}
	}
	return cfg, nil
}

func normalizeWarmupExecutionMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "native", "host", "host-model", "host_model":
		return "native"
	default:
		return "management"
	}
}

func parseDuration(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}

func normalizeSchedulerMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "serial", "fill-first", "fill_first":
		return "serial"
	case "legacy":
		return "legacy"
	case "enforce":
		return "enforce"
	case "shadow":
		return "shadow"
	default:
		return "serial"
	}
}

func normalizeWindowOrder(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		item = normalizeWindowClass(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func normalizeWindowClass(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "5h", "5-hour", "five-hour", "five_hour", "primary":
		return "5h"
	case "week", "weekly", "7d", "7-day", "secondary":
		return "weekly"
	case "month", "monthly", "30d", "30-day":
		return "monthly"
	default:
		return ""
	}
}

package main

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const maxWarmupModelLength = 256

// pluginConfig is deliberately made up of ordinary scalar values.  CPA sends
// the plugin configuration as YAML on every register/reconfigure call, while
// duration values are most convenient and least surprising when written as
// strings ("30s", "15m", ...).
type pluginConfig struct {
	AuthExpiryAutoRepair         bool
	QuotaURL                     string
	QuotaRefreshBatch            int
	Enabled                      bool
	Priority                     int
	SchedulerMode                string
	SerialSwitchPercent          float64
	SerialSoftContinuation       bool
	SerialAllocationPolicy       string
	SerialBudgetRebalancePercent float64
	QuotaDefaultPlan             string
	QuotaAccountPlans            map[string]string
	SerialHandoffMode            string
	Serial5hHandoffMode          string
	Serial5hSwitchPercent        float64
	SerialPreferActiveCycle      bool
	SerialWeeklyRebalancePercent float64
	SerialWeeklyRebalanceMinHold time.Duration
	DrainWindowHours             float64
	CPAManagementURL             string
	CPAManagementKeyFile         string
	WarmupEnabled                bool
	WarmupModel                  string
	WarmupRetryAfter             time.Duration
	WarmupMinInterval            time.Duration
	WarmupMaxPerDay              int
	QuotaRefreshCooldown         time.Duration
	RefreshInterval              time.Duration
	StaleAfter                   time.Duration
	StatePath                    string
	SoftLimitPercent             float64
	Reserve5hPercent             float64
	ReserveWeeklyPercent         float64
	ReserveMonthlyPercent        float64
	LowQuotaPercent              float64
	FallbackBan                  time.Duration
	MaxBan                       time.Duration
	HalfOpenProbeTimeout         time.Duration
	HalfOpenRetryAfter           time.Duration
	StickySeconds                int
	SwitchHysteresisPercent      float64
	SwitchConfirmations          int
	CostSampleLimit              int
	DecisionHistoryLimit         int
	NormalCostQuantile           float64
	GuardCostQuantile            float64
	HighCostQuantile             float64
	ShadowLogInterval            time.Duration
	PreferResetCredits           bool
	WindowOrder                  []string
}

type yamlPluginConfig struct {
	AuthExpiryAutoRepair         *bool             `yaml:"auth_expiry_auto_repair"`
	QuotaURL                     string            `yaml:"quota_url"`
	QuotaRefreshBatch            *int              `yaml:"quota_refresh_batch"`
	Enabled                      *bool             `yaml:"enabled"`
	Priority                     *int              `yaml:"priority"`
	SchedulerMode                string            `yaml:"scheduler_mode"`
	SerialSwitchPercent          *float64          `yaml:"serial_switch_percent"`
	SerialSoftContinuation       *bool             `yaml:"serial_soft_continuation"`
	SerialAllocationPolicy       string            `yaml:"serial_allocation_policy"`
	SerialBudgetRebalancePercent *float64          `yaml:"serial_budget_rebalance_percent"`
	QuotaDefaultPlan             string            `yaml:"quota_default_plan"`
	QuotaAccountPlans            map[string]string `yaml:"quota_account_plans"`
	SerialHandoffMode            string            `yaml:"serial_handoff_mode"`
	Serial5hHandoffMode          string            `yaml:"serial_5h_handoff_mode"`
	Serial5hSwitchPercent        *float64          `yaml:"serial_5h_switch_percent"`
	SerialPreferActiveCycle      *bool             `yaml:"serial_prefer_active_cycle"`
	SerialWeeklyRebalancePercent *float64          `yaml:"serial_weekly_rebalance_percent"`
	SerialWeeklyRebalanceMinHold string            `yaml:"serial_weekly_rebalance_min_hold"`
	DrainWindowHours             *float64          `yaml:"drain_window_hours"`
	CPAManagementURL             string            `yaml:"cpa_management_url"`
	CPAManagementKeyFile         string            `yaml:"cpa_management_key_file"`
	WarmupEnabled                *bool             `yaml:"warmup_enabled"`
	WarmupModel                  string            `yaml:"warmup_model"`
	WarmupRetryAfter             string            `yaml:"warmup_retry_after"`
	WarmupMinInterval            string            `yaml:"warmup_min_interval"`
	WarmupMaxPerDay              *int              `yaml:"warmup_max_per_day"`
	QuotaRefreshCooldown         string            `yaml:"quota_refresh_cooldown"`
	RefreshInterval              string            `yaml:"refresh_interval"`
	StaleAfter                   string            `yaml:"stale_after"`
	StatePath                    string            `yaml:"state_path"`
	SoftLimitPercent             *float64          `yaml:"soft_limit_percent"`
	Reserve5hPercent             *float64          `yaml:"reserve_5h_percent"`
	ReserveWeeklyPercent         *float64          `yaml:"reserve_weekly_percent"`
	ReserveMonthlyPercent        *float64          `yaml:"reserve_monthly_percent"`
	LowQuotaPercent              *float64          `yaml:"low_quota_percent"`
	FallbackBan                  string            `yaml:"fallback_ban"`
	MaxBan                       string            `yaml:"max_ban"`
	HalfOpenProbeTimeout         string            `yaml:"half_open_probe_timeout"`
	HalfOpenRetryAfter           string            `yaml:"half_open_retry_after"`
	StickySeconds                *int              `yaml:"sticky_seconds"`
	SwitchHysteresisPercent      *float64          `yaml:"switch_hysteresis_percent"`
	SwitchConfirmations          *int              `yaml:"switch_confirmations"`
	CostSampleLimit              *int              `yaml:"cost_sample_limit"`
	DecisionHistoryLimit         *int              `yaml:"decision_history_limit"`
	NormalCostQuantile           *float64          `yaml:"normal_cost_quantile"`
	GuardCostQuantile            *float64          `yaml:"guard_cost_quantile"`
	HighCostQuantile             *float64          `yaml:"high_cost_quantile"`
	ShadowLogInterval            string            `yaml:"shadow_log_interval"`
	PreferResetCredits           *bool             `yaml:"prefer_reset_credits"`
	WindowOrder                  []string          `yaml:"window_order"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		AuthExpiryAutoRepair:         true,
		QuotaURL:                     "https://chatgpt.com/backend-api/wham/usage",
		QuotaRefreshBatch:            8,
		Enabled:                      true,
		SchedulerMode:                "serial",
		SerialSwitchPercent:          98,
		SerialAllocationPolicy:       "sustainable",
		SerialBudgetRebalancePercent: 20,
		QuotaDefaultPlan:             "team_standard",
		SerialHandoffMode:            "threshold_only",
		Serial5hHandoffMode:          "429_only",
		Serial5hSwitchPercent:        98,
		SerialPreferActiveCycle:      true,
		SerialWeeklyRebalancePercent: 10,
		SerialWeeklyRebalanceMinHold: 5 * time.Minute,
		DrainWindowHours:             6,
		CPAManagementURL:             "http://127.0.0.1:8317/v0/management/api-call",
		CPAManagementKeyFile:         "/run/secrets/management_key",
		WarmupModel:                  "gpt-5.6-luna",
		WarmupRetryAfter:             15 * time.Minute,
		WarmupMinInterval:            15 * time.Minute,
		WarmupMaxPerDay:              8,
		QuotaRefreshCooldown:         2 * time.Minute,
		RefreshInterval:              30 * time.Second,
		StaleAfter:                   15 * time.Minute,
		StatePath:                    "/var/lib/codex-quota-scheduler/state.json",
		SoftLimitPercent:             98,
		Reserve5hPercent:             0,
		ReserveWeeklyPercent:         8,
		ReserveMonthlyPercent:        12,
		LowQuotaPercent:              20,
		FallbackBan:                  15 * time.Minute,
		MaxBan:                       24 * time.Hour,
		HalfOpenProbeTimeout:         15 * time.Minute,
		HalfOpenRetryAfter:           2 * time.Minute,
		StickySeconds:                1500,
		SwitchHysteresisPercent:      2,
		SwitchConfirmations:          3,
		CostSampleLimit:              512,
		DecisionHistoryLimit:         100,
		NormalCostQuantile:           0.75,
		GuardCostQuantile:            0.90,
		HighCostQuantile:             0.95,
		ShadowLogInterval:            5 * time.Minute,
		PreferResetCredits:           true,
		WindowOrder:                  []string{"5h", "weekly", "monthly"},
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
	if strings.TrimSpace(in.QuotaURL) != "" {
		cfg.QuotaURL = strings.TrimSpace(in.QuotaURL)
	}
	if in.QuotaRefreshBatch != nil {
		cfg.QuotaRefreshBatch = *in.QuotaRefreshBatch
	}
	if cfg.QuotaRefreshBatch < 1 || cfg.QuotaRefreshBatch > 100 {
		return cfg, fmt.Errorf("quota_refresh_batch must be between 1 and 100")
	}
	if endpoint, err := url.Parse(cfg.QuotaURL); err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return cfg, fmt.Errorf("quota_url must be an absolute HTTP(S) URL without credentials")
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
	if in.SerialSoftContinuation != nil {
		cfg.SerialSoftContinuation = *in.SerialSoftContinuation
	}
	if in.SerialAllocationPolicy != "" {
		if in.SerialAllocationPolicy != "sustainable" && in.SerialAllocationPolicy != "weekly_remaining" {
			return cfg, fmt.Errorf("serial_allocation_policy must be sustainable or weekly_remaining")
		}
		cfg.SerialAllocationPolicy = in.SerialAllocationPolicy
	}
	if in.SerialBudgetRebalancePercent != nil {
		v := *in.SerialBudgetRebalancePercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return cfg, fmt.Errorf("serial_budget_rebalance_percent must be between 0 and 100 (0 disables budget preemption)")
		}
		cfg.SerialBudgetRebalancePercent = v
	}
	if in.QuotaDefaultPlan != "" {
		plan, ok := normalizeQuotaPlan(in.QuotaDefaultPlan)
		if !ok {
			return cfg, fmt.Errorf("unknown quota_default_plan")
		}
		cfg.QuotaDefaultPlan = plan
	}
	if in.QuotaAccountPlans != nil {
		cfg.QuotaAccountPlans = make(map[string]string, len(in.QuotaAccountPlans))
		for authID, rawPlan := range in.QuotaAccountPlans {
			plan, ok := normalizeQuotaPlan(rawPlan)
			id := strings.TrimSpace(authID)
			if !ok || id == "" {
				return cfg, fmt.Errorf("quota_account_plans requires nonempty auth IDs and supported plan names")
			}
			cfg.QuotaAccountPlans[id] = plan
		}
	}
	if strings.TrimSpace(in.SerialHandoffMode) != "" {
		cfg.SerialHandoffMode = strings.ToLower(strings.TrimSpace(in.SerialHandoffMode))
	}
	if strings.TrimSpace(in.Serial5hHandoffMode) != "" {
		cfg.Serial5hHandoffMode = strings.ToLower(strings.TrimSpace(in.Serial5hHandoffMode))
	}
	if in.Serial5hSwitchPercent != nil {
		cfg.Serial5hSwitchPercent = *in.Serial5hSwitchPercent
		if strings.TrimSpace(in.Serial5hHandoffMode) == "" {
			cfg.Serial5hHandoffMode = "custom_threshold"
		}
	}
	if in.SerialPreferActiveCycle != nil {
		cfg.SerialPreferActiveCycle = *in.SerialPreferActiveCycle
	}
	if in.SerialWeeklyRebalancePercent != nil {
		v := *in.SerialWeeklyRebalancePercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 100 {
			return cfg, fmt.Errorf("serial_weekly_rebalance_percent must be between 0 and 100 (0 disables proactive balancing)")
		}
		cfg.SerialWeeklyRebalancePercent = v
	}
	if strings.TrimSpace(in.SerialWeeklyRebalanceMinHold) != "" {
		v, ok := parseDuration(in.SerialWeeklyRebalanceMinHold)
		if !ok || v < time.Minute || v > 24*time.Hour {
			return cfg, fmt.Errorf("serial_weekly_rebalance_min_hold must be between 1m and 24h")
		}
		cfg.SerialWeeklyRebalanceMinHold = v
	}
	if in.DrainWindowHours != nil {
		cfg.DrainWindowHours = *in.DrainWindowHours
	}
	if strings.TrimSpace(in.CPAManagementURL) != "" {
		cfg.CPAManagementURL = strings.TrimSpace(in.CPAManagementURL)
	}
	if strings.TrimSpace(in.CPAManagementKeyFile) != "" {
		cfg.CPAManagementKeyFile = strings.TrimSpace(in.CPAManagementKeyFile)
	}
	if in.AuthExpiryAutoRepair != nil {
		cfg.AuthExpiryAutoRepair = *in.AuthExpiryAutoRepair
	}
	if in.WarmupEnabled != nil {
		cfg.WarmupEnabled = *in.WarmupEnabled
	}
	if strings.TrimSpace(in.WarmupModel) != "" {
		model, err := validateWarmupModel(in.WarmupModel)
		if err != nil {
			return cfg, err
		}
		cfg.WarmupModel = model
	}
	if v, ok := parseDuration(in.WarmupRetryAfter); ok {
		cfg.WarmupRetryAfter = v
	}
	if strings.TrimSpace(in.WarmupMinInterval) != "" {
		v, ok := parseDuration(in.WarmupMinInterval)
		if !ok || v < time.Minute || v > 24*time.Hour {
			return cfg, fmt.Errorf("warmup_min_interval must be between 1m and 24h")
		}
		cfg.WarmupMinInterval = v
	}
	if in.WarmupMaxPerDay != nil {
		if *in.WarmupMaxPerDay < 1 || *in.WarmupMaxPerDay > 1000 {
			return cfg, fmt.Errorf("warmup_max_per_day must be between 1 and 1000")
		}
		cfg.WarmupMaxPerDay = *in.WarmupMaxPerDay
	}
	if v, ok := parseDuration(in.QuotaRefreshCooldown); ok {
		cfg.QuotaRefreshCooldown = v
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
	// scheduler until a usable quota probe snapshot is available.
	cfg.SchedulerMode = normalizeSchedulerMode(cfg.SchedulerMode)
	cfg.SerialHandoffMode = normalizeSerialHandoffMode(cfg.SerialHandoffMode)
	cfg.Serial5hHandoffMode = normalizeSerial5hHandoffMode(cfg.Serial5hHandoffMode)
	if cfg.RefreshInterval < time.Second {
		cfg.RefreshInterval = time.Second
	}
	if cfg.WarmupRetryAfter < time.Minute {
		cfg.WarmupRetryAfter = 15 * time.Minute
	}
	if cfg.QuotaRefreshCooldown < 30*time.Second || cfg.QuotaRefreshCooldown > 24*time.Hour {
		cfg.QuotaRefreshCooldown = 2 * time.Minute
	}
	if cfg.WarmupModel == "" {
		cfg.WarmupModel = "gpt-5.6-luna"
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
	if cfg.Serial5hSwitchPercent <= 0 || cfg.Serial5hSwitchPercent > 100 {
		cfg.Serial5hSwitchPercent = cfg.SerialSwitchPercent
	}
	if cfg.DrainWindowHours <= 0 || cfg.DrainWindowHours > 168 {
		cfg.DrainWindowHours = 6
	}
	if cfg.Reserve5hPercent < 0 || cfg.Reserve5hPercent >= 100 {
		cfg.Reserve5hPercent = 0
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

func normalizeSerialHandoffMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "reserve", "reserve-aware", "reserve_aware", "safe", "safe-reserve":
		return "reserve_aware"
	default:
		return "threshold_only"
	}
}

func normalizeSerial5hHandoffMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "custom", "threshold", "threshold-only", "threshold_only", "custom-threshold", "custom_threshold":
		return "custom_threshold"
	case "reserve", "reserve-aware", "reserve_aware", "safe", "safe-reserve":
		return "reserve_aware"
	case "429", "429-only", "429_only", "hard-limit", "hard_limit", "hard-limit-only", "hard_limit_only":
		return "429_only"
	default:
		return "inherit_global"
	}
}

func validateWarmupModel(raw string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", fmt.Errorf("warmup_model must not be empty")
	}
	if len(model) > maxWarmupModelLength {
		return "", fmt.Errorf("warmup_model exceeds %d bytes", maxWarmupModelLength)
	}
	for _, char := range model {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		switch char {
		case '-', '_', '.', '/', ':':
			continue
		default:
			return "", fmt.Errorf("warmup_model contains a character that is unsafe for an HTTP header")
		}
	}
	return model, nil
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
	case "balanced":
		return "balanced"
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

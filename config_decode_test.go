package main

import (
	"math"
	"testing"
)

func TestDedicatedFiveHourThresholdWithoutModeInfersCustomThreshold(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("serial_5h_switch_percent: 95\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Serial5hHandoffMode != "custom_threshold" || cfg.Serial5hSwitchPercent != 95 {
		t.Fatalf("5h compatibility inference = mode %q threshold %v; want custom_threshold/95", cfg.Serial5hHandoffMode, cfg.Serial5hSwitchPercent)
	}
}

func TestExplicitFiveHourModeIsNotOverriddenByThresholdCompatibility(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
	}{
		{name: "inherit", mode: "inherit_global"},
		{name: "reserve", mode: "reserve_aware"},
		{name: "hard-only", mode: "429_only"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte("serial_5h_handoff_mode: " + test.mode + "\nserial_5h_switch_percent: 95\n")
			cfg, err := parsePluginConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Serial5hHandoffMode != test.mode || cfg.Serial5hSwitchPercent != 95 {
				t.Fatalf("explicit mode changed: mode=%q threshold=%v", cfg.Serial5hHandoffMode, cfg.Serial5hSwitchPercent)
			}
		})
	}
}

func TestNonFinitePercentagesFallBackToFiniteDefaults(t *testing.T) {
	cfg, err := parsePluginConfig([]byte(`
serial_switch_percent: .nan
serial_5h_switch_percent: .inf
soft_limit_percent: -.inf
reserve_5h_percent: .nan
reserve_weekly_percent: .inf
reserve_monthly_percent: -.inf
low_quota_percent: .nan
switch_hysteresis_percent: .inf
normal_cost_quantile: .nan
guard_cost_quantile: .inf
high_cost_quantile: -.inf
`))
	if err != nil {
		t.Fatal(err)
	}
	values := []float64{
		cfg.SerialSwitchPercent,
		cfg.Serial5hSwitchPercent,
		cfg.SoftLimitPercent,
		cfg.Reserve5hPercent,
		cfg.ReserveWeeklyPercent,
		cfg.ReserveMonthlyPercent,
		cfg.LowQuotaPercent,
		cfg.SwitchHysteresisPercent,
		cfg.NormalCostQuantile,
		cfg.GuardCostQuantile,
		cfg.HighCostQuantile,
	}
	for index, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			t.Fatalf("config value %d remained non-finite: %v", index, value)
		}
	}
	if cfg.SerialSwitchPercent != 98 || cfg.Serial5hSwitchPercent != 98 || cfg.SoftLimitPercent != 98 {
		t.Fatalf("threshold defaults = global %v 5h %v soft %v", cfg.SerialSwitchPercent, cfg.Serial5hSwitchPercent, cfg.SoftLimitPercent)
	}
	if cfg.Reserve5hPercent != 15 || cfg.ReserveWeeklyPercent != 8 || cfg.ReserveMonthlyPercent != 12 {
		t.Fatalf("reserve defaults = 5h %v weekly %v monthly %v", cfg.Reserve5hPercent, cfg.ReserveWeeklyPercent, cfg.ReserveMonthlyPercent)
	}
	if cfg.NormalCostQuantile != 0.75 || cfg.GuardCostQuantile != 0.90 || cfg.HighCostQuantile != 0.95 {
		t.Fatalf("quantile defaults = %v/%v/%v", cfg.NormalCostQuantile, cfg.GuardCostQuantile, cfg.HighCostQuantile)
	}
}

func TestNonFiniteDrainWindowFallsBackToDefault(t *testing.T) {
	for _, raw := range []string{"drain_window_hours: .nan\n", "drain_window_hours: .inf\n", "drain_window_hours: -.inf\n"} {
		cfg, err := parsePluginConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DrainWindowHours != 6 {
			t.Fatalf("non-finite drain %q produced %v; want 6", raw, cfg.DrainWindowHours)
		}
	}
}

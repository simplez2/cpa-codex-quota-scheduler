package main

import (
	"math"
	"testing"
	"time"
)

func poolAccount(id, plan string, remaining float64) runtimeQuotaStatus {
	return runtimeQuotaStatus{AuthID: id, Plan: plan, Fresh: true, Windows: []runtimeQuotaWindowStatus{{Window: "weekly", Allowed: true, RemainingPercent: remaining}}}
}

func TestPoolWeeklyUsesUserEquivalentsAndRetainsThresholdAccounts(t *testing.T) {
	cfg := defaultPluginConfig()
	accounts := []runtimeQuotaStatus{poolAccount("a", "pro_20x", 50), poolAccount("b", "pro_5x", 100), poolAccount("c", "team_premium", 0), poolAccount("d", "plus", 100)}
	accounts[0].Eligible = false // A 5h routing restriction does not erase weekly inventory.
	accounts[0].Reason = "serial_threshold"
	accounts = append(accounts, accounts[0]) // Alias snapshots must not double-count capacity.
	got := poolWeeklyStatus(accounts, cfg, time.Now())
	if !got.Complete || got.Accounts != 4 || got.TotalCapacity != 21 || got.RemainingPercent == nil || math.Abs(*got.RemainingPercent-1100./21) > 1e-9 {
		t.Fatalf("wrong weighted pool: %+v", got)
	}
	if quotaPlanWeight("pro_20x") != 20 || quotaPlanWeight("team_premium") != 5 {
		t.Fatal("reporting weights changed routing priors")
	}
}

func TestPoolWeeklyUnknownAndExpiredWindowsRemainInDenominator(t *testing.T) {
	now := time.Now()
	a := poolAccount("a", "team_standard", 50)
	a.Fresh = false
	b := runtimeQuotaStatus{AuthID: "b", Plan: "pro_5x"}
	c := poolAccount("c", "team_standard", 100)
	c.Windows[0].CycleStarted = true
	c.Windows[0].ResetAt = now.Add(-time.Minute).Format(time.RFC3339)
	got := poolWeeklyStatus([]runtimeQuotaStatus{a, b, c}, defaultPluginConfig(), now)
	if got.Complete || got.RemainingPercent != nil || got.UnknownAccounts != 2 || got.CachedAccounts != 1 || got.TotalCapacity != 7 || math.Abs(got.MinimumPercent-50./7) > 1e-9 || math.Abs(got.MaximumPercent-650./7) > 1e-9 {
		t.Fatalf("unknown quota was invented or omitted: %+v", got)
	}
}

func TestPoolWeeklyWeightsAreEditableAndValidated(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("pool_weekly_capacity_weights: {pro_20x: 12, plus: 2}\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := poolWeeklyStatus([]runtimeQuotaStatus{poolAccount("a", "pro_20x", 50), poolAccount("b", "plus", 100)}, cfg, time.Now())
	if got.RemainingPercent == nil || math.Abs(*got.RemainingPercent-800./14) > 1e-9 {
		t.Fatalf("override ignored: %+v", got)
	}
	if changes, fields := validatePanelSettings(nil, map[string]any{"pool_weekly_capacity_weights": map[string]float64{"pro_20x": 12}}); len(fields) > 0 || changes["pool_weekly_capacity_weights"] == nil {
		t.Fatalf("API cannot edit weights: %v", fields)
	}
	for _, raw := range []string{"{pro_20x: 0}", "{plus: -1}", "{plus: .nan}", "{pro: 2}"} {
		if _, err := parsePluginConfig([]byte("pool_weekly_capacity_weights: " + raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPoolWeeklyIncludesUnobservedInventoryCredentials(t *testing.T) {
	ifCfg := defaultPluginConfig()
	ifCfg.QuotaAccountPlans = map[string]string{"missing": "pro_20x"}
	now := time.Now()
	accounts := poolWeeklyAccounts([]runtimeQuotaStatus{poolAccount("known", "team_standard", 100)}, map[string]string{"i1": "known", "i2": "missing", "alias": "missing"}, ifCfg, now)
	got := poolWeeklyStatus(accounts, ifCfg, now)
	if got.Accounts != 2 || got.TotalCapacity != 11 || got.UnknownAccounts != 1 || got.Complete || math.Abs(got.MinimumPercent-100./11) > 1e-9 || got.MaximumPercent != 100 {
		t.Fatalf("unobserved inventory capacity omitted: %+v", got)
	}
}

package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestFiveHourBalanceSurvivesRuntimeStatusJSONSanitizer(t *testing.T) {
	pool := poolFiveHourStatus([]runtimeQuotaStatus{fiveHourPoolAccount("limited", "team_standard", 75)}, defaultPluginConfig(), time.Now())
	raw, err := json.Marshal(runtimeStatus{PoolFiveHour: pool})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	var balance map[string]any
	if err := json.Unmarshal(response["pool_5h"], &balance); err != nil {
		t.Fatal(err)
	}
	if balance["total_capacity"] != float64(1) || balance["remaining_percent"] != float64(75) || balance["accounts"] != float64(1) || balance["inventory_accounts"] != float64(1) {
		t.Fatalf("public API lost embedded balance: %s", response["pool_5h"])
	}
}

func fiveHourPoolAccount(id, plan string, remaining float64) runtimeQuotaStatus {
	account := poolAccount(id, plan, 75)
	account.FiveHourLimitStatus = "limited"
	account.Windows = append(account.Windows, runtimeQuotaWindowStatus{Window: "5h", Allowed: true, RemainingPercent: remaining})
	return account
}

func TestFiveHourPoolExcludesUncappedAndUnclassifiedWithoutChangingWeekly(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.PoolFiveHourExcludedAccounts = []string{"manual"}
	accounts := []runtimeQuotaStatus{
		fiveHourPoolAccount("limited", "pro_20x", 50),
		poolAccount("uncapped", "team_standard", 100),
		{AuthID: "missing", Plan: "pro_5x"},
		fiveHourPoolAccount("manual", "plus", 100),
	}
	accounts[1].FiveHourLimitStatus = "not_reported"
	accounts[0].Eligible = false // Quota inventory is separate from route eligibility.
	accounts = append(accounts, accounts[0])
	weekly := poolWeeklyStatus(accounts, cfg, time.Now())
	five := poolFiveHourStatus(accounts, cfg, time.Now())
	if five.Accounts != 1 || five.InventoryAccounts != 4 || five.ExcludedAccounts != 1 || five.NoLimitReportedAccounts != 1 || five.UnclassifiedAccounts != 1 || five.ScopeComplete || five.TotalCapacity != 10 || five.RemainingPercent == nil || *five.RemainingPercent != 50 {
		t.Fatalf("wrong five-hour scope: %+v", five)
	}
	if weekly.Accounts != 4 || weekly.TotalCapacity != 17 || weekly.UnknownAccounts != 1 {
		t.Fatalf("five-hour exclusions changed weekly inventory: %+v", weekly)
	}
	if len(five.InventoryAuthIDs) != 4 {
		t.Fatal("unobserved credentials missing from manual exclusion editor", five.InventoryAuthIDs)
	}
}

func TestFiveHourPoolExpiredAndUnstartedWindows(t *testing.T) {
	now := time.Now()
	expired := fiveHourPoolAccount("expired", "team_standard", 0)
	expired.Windows[1].CycleStarted = true
	expired.Windows[1].ResetAt = now.Add(-time.Minute).Format(time.RFC3339)
	idle := fiveHourPoolAccount("idle", "pro_5x", 100)
	idle.Fresh = false
	idle.Windows[1].PlaceholderReset = true
	idle.Windows[1].ResetAt = now.Add(-time.Hour).Format(time.RFC3339)
	got := poolFiveHourStatus([]runtimeQuotaStatus{expired, idle}, defaultPluginConfig(), now)
	if got.Complete || got.UnknownAccounts != 1 || got.CachedAccounts != 1 || got.TotalCapacity != 6 || got.RemainingPercent != nil || math.Abs(got.MinimumPercent-500./6) > 1e-9 || got.MaximumPercent != 100 {
		t.Fatalf("expired cycle invented a refill or idle pool omitted: %+v", got)
	}
}

func TestFiveHourLimitAbsenceNeedsCurrentFullNativeResponse(t *testing.T) {
	now := time.Now()
	full, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":600}}}`), "a", "i", now)
	if err != nil {
		t.Fatal(err)
	}
	old := quotaSnapshot{Windows: []quotaWindow{{Class: "5h"}}}
	if got := poolFiveHourLimitStatus(full, old, now, time.Minute); got != "not_reported" {
		t.Fatal(got)
	}
	laterHeader := old
	laterHeader.Windows = append([]quotaWindow(nil), old.Windows...)
	laterHeader.Windows[0].ObservedAt = now.Add(time.Second)
	if got := poolFiveHourLimitStatus(full, laterHeader, now.Add(time.Second), time.Minute); got != "limited" {
		t.Fatal("newer 5h header did not prove a limit", got)
	}
	if got := poolFiveHourLimitStatus(full, old, now.Add(2*time.Minute), time.Minute); got != "unknown" {
		t.Fatal(got)
	}
	partial := quotaSnapshot{Windows: []quotaWindow{{Class: "weekly", ObservedAt: now}}}
	if got := poolFiveHourLimitStatus(quotaSnapshot{}, partial, now, time.Minute); got != "unknown" {
		t.Fatal("a partial weekly header proves no absence:", got)
	}
	if got := poolFiveHourLimitStatus(quotaSnapshot{}, old, now, time.Minute); got != "limited" {
		t.Fatal(got)
	}
	unknown := quotaSnapshot{RefreshedAt: now, Windows: []quotaWindow{{Class: "unknown", ObservedAt: now}}}
	if got := poolFiveHourLimitStatus(unknown, old, now, time.Minute); got != "unknown" {
		t.Fatal(got)
	}
	merged := mergePartialQuotaSnapshot(old, full, now, time.Minute)
	if got := poolFiveHourLimitStatus(quotaSnapshot{}, merged, now, time.Minute); got != "not_reported" {
		t.Fatal("failed next probe lost absence evidence", got)
	}
	raw, _ := json.Marshal(merged)
	var restored quotaSnapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if got := poolFiveHourLimitStatus(quotaSnapshot{}, restored, now.Add(2*time.Minute), time.Minute); got != "unknown" {
		t.Fatal("stale absence survived as unlimited", got)
	}
	// A valid native response with no windows is rejected before classification.
	if _, err := parseNativeQuota([]byte(`{"rate_limit":{"primary_window":null,"secondary_window":null}}`), "a", "i", now); err == nil {
		t.Fatal("missing quota treated as unlimited")
	}
}

func TestFiveHourReportingWeightsAndExclusionsAreIndependent(t *testing.T) {
	cfg, err := parsePluginConfig([]byte("pool_5h_capacity_weights: {pro_20x: 20}\npool_5h_excluded_accounts: [manual]\n"))
	if err != nil {
		t.Fatal(err)
	}
	accounts := []runtimeQuotaStatus{fiveHourPoolAccount("pro", "pro_20x", 50), fiveHourPoolAccount("plus", "plus", 100), fiveHourPoolAccount("manual", "team_standard", 100)}
	five := poolFiveHourStatus(accounts, cfg, time.Now())
	weekly := poolWeeklyStatus(accounts, cfg, time.Now())
	if five.TotalCapacity != 21 || five.RemainingPercent == nil || math.Abs(*five.RemainingPercent-1100./21) > 1e-9 || weekly.TotalCapacity != 12 {
		t.Fatalf("independent weights not applied: five=%+v weekly=%+v", five, weekly)
	}
	if changes, fields := validatePanelSettings(nil, map[string]any{"pool_5h_capacity_weights": map[string]float64{"pro_20x": 20}, "pool_5h_excluded_accounts": []string{"manual"}}); len(fields) > 0 || len(changes) != 2 {
		t.Fatalf("settings API: %v %v", changes, fields)
	}
	for _, text := range []string{"pool_5h_capacity_weights: {plus: 0}", "pool_5h_capacity_weights: {plus: .nan}", "pool_5h_capacity_weights: {bad: 2}", "pool_5h_excluded_accounts: ['']"} {
		if _, err := parsePluginConfig([]byte(text)); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}

func TestFiveHourPoolRequiresSpendableWeeklyBacking(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, mode                       string
		used, reserve                    float64
		allowed, exhausted, wantIncluded bool
	}{
		{"before-threshold", "threshold_only", 97.9, 0, true, false, true},
		{"at-threshold", "threshold_only", 98, 0, true, false, false},
		{"above-threshold", "threshold_only", 99, 0, true, false, false},
		{"hard-exhausted", "429_only", 100, 0, true, false, false},
		{"provider-exhausted", "429_only", 50, 0, true, true, false},
		{"provider-disallowed", "429_only", 50, 0, false, false, false},
		{"at-reserve", "reserve_aware", 95, 5, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultPluginConfig()
			cfg.SerialHandoffMode, cfg.SerialSwitchPercent, cfg.ReserveWeeklyPercent = tc.mode, 98, tc.reserve
			a := fiveHourPoolAccount("account", "pro_20x", 100)
			a.Windows[0].RemainingPercent = 100 - tc.used
			a.Windows[0].Allowed, a.Windows[0].LimitReached = tc.allowed, tc.exhausted
			got := poolFiveHourStatus([]runtimeQuotaStatus{a, a}, cfg, now)
			if tc.wantIncluded {
				if got.Accounts != 1 || got.TotalCapacity != 10 || got.RemainingCapacity != 10 || got.WeeklyBlockedAccounts != 0 {
					t.Fatalf("usable weekly backing omitted: %+v", got)
				}
			} else if got.Accounts != 0 || got.TotalCapacity != 0 || got.RemainingCapacity != 0 || got.WeeklyBlockedAccounts != 1 {
				t.Fatalf("unspendable 5h capacity counted: %+v", got)
			}
			if got.InventoryAccounts != 1 || poolWeeklyStatus([]runtimeQuotaStatus{a}, cfg, now).TotalCapacity != 10 {
				t.Fatal("usable 5h filtering erased weekly inventory")
			}
		})
	}
}

func TestFiveHourPoolWeeklyResetNeedsFreshBalanceBeforeReentry(t *testing.T) {
	now := time.Now()
	cfg := defaultPluginConfig()
	a := fiveHourPoolAccount("account", "team_standard", 100)
	a.Windows[0].CycleStarted = true
	a.Windows[0].ResetAt = now.Add(-time.Second).Format(time.RFC3339)
	a.Windows[0].RemainingPercent = 0
	got := poolFiveHourStatus([]runtimeQuotaStatus{a}, cfg, now)
	if got.TotalCapacity != 0 || got.WeeklyUnconfirmedAccounts != 1 || got.ScopeComplete {
		t.Fatalf("expired weekly cycle fabricated available 5h capacity: %+v", got)
	}
	a.Windows[0].ResetAt = now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	a.Windows[0].RemainingPercent = 100
	got = poolFiveHourStatus([]runtimeQuotaStatus{a}, cfg, now)
	if got.TotalCapacity != 1 || got.RemainingCapacity != 1 || got.WeeklyUnconfirmedAccounts != 0 {
		t.Fatalf("new weekly observation failed to restore 5h pool: %+v", got)
	}
	a.Windows = a.Windows[1:]
	got = poolFiveHourStatus([]runtimeQuotaStatus{a}, cfg, now)
	if got.TotalCapacity != 0 || got.WeeklyUnconfirmedAccounts != 1 {
		t.Fatalf("missing weekly backing inferred unlimited: %+v", got)
	}
}

func TestPoolNextResetDoesNotPromiseWholePoolRefill(t *testing.T) {
	now := time.Now()
	a := fiveHourPoolAccount("a", "team_standard", 50)
	b := fiveHourPoolAccount("b", "team_standard", 70)
	a.Windows[1].CycleStarted = true
	a.Windows[1].ResetAt = now.Add(2 * time.Hour).Format(time.RFC3339)
	b.Windows[1].CycleStarted = true
	b.Windows[1].ResetAt = now.Add(time.Hour).Format(time.RFC3339)
	got := poolFiveHourStatus([]runtimeQuotaStatus{a, b}, defaultPluginConfig(), now)
	if got.NextResetAt != b.Windows[1].ResetAt {
		t.Fatalf("wrong next reset: %+v", got)
	}
}

package main

import (
	"math"
	"time"
)

// Reporting multipliers are editable independently of routing priors.
func defaultPoolWeeklyCapacityWeights() map[string]float64 {
	return map[string]float64{"team_standard": 1, "team_premium": 10, "plus": 1, "pro_5x": 10, "pro_20x": 20}
}

// A successfully fetched inventory includes credentials with no quota snapshot.
// Keep their capacity in the denominator until their weekly balance is observed.
func poolWeeklyAccounts(accounts []runtimeQuotaStatus, identities map[string]string, cfg pluginConfig, now time.Time) []runtimeQuotaStatus {
	out := append([]runtimeQuotaStatus(nil), accounts...)
	seen := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		seen[account.AuthID] = true
	}
	for index, id := range identities {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		plan, _, _ := resolvedQuotaPlan(cfg, id, quotaSnapshot{}, now)
		out = append(out, runtimeQuotaStatus{AuthID: id, AuthIndex: index, Plan: plan})
	}
	return out
}

type runtimePoolWeeklyStatus struct {
	Unit              string             `json:"unit"`
	CapacityWeights   map[string]float64 `json:"capacity_weights"`
	Accounts          int                `json:"accounts"`
	KnownAccounts     int                `json:"known_accounts"`
	CachedAccounts    int                `json:"cached_accounts"`
	UnknownAccounts   int                `json:"unknown_accounts"`
	TotalCapacity     float64            `json:"total_capacity"`
	KnownCapacity     float64            `json:"known_capacity"`
	RemainingCapacity float64            `json:"remaining_capacity"`
	RemainingPercent  *float64           `json:"remaining_percent"`
	MinimumPercent    float64            `json:"minimum_percent"`
	MaximumPercent    float64            `json:"maximum_percent"`
	CoveragePercent   float64            `json:"coverage_percent"`
	Complete          bool               `json:"complete"`
	Plans             map[string]int     `json:"plans"`
	LastObservedAt    string             `json:"last_observed_at,omitempty"`
	NextResetAt       string             `json:"next_reset_at,omitempty"`
}

func poolWeeklyStatus(accounts []runtimeQuotaStatus, cfg pluginConfig, now time.Time) runtimePoolWeeklyStatus {
	return poolWindowStatus(accounts, cfg.PoolWeeklyCapacityWeights, "weekly", now)
}

func poolWindowStatus(accounts []runtimeQuotaStatus, overrides map[string]float64, class string, now time.Time) runtimePoolWeeklyStatus {
	weights := defaultPoolWeeklyCapacityWeights()
	for plan, value := range overrides {
		if value > 0 && value <= 1000 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			weights[plan] = value
		}
	}
	out := runtimePoolWeeklyStatus{Unit: "x", CapacityWeights: weights, Plans: make(map[string]int)}
	seen := make(map[string]bool)
	for _, account := range accounts {
		if account.AuthID == "" || seen[account.AuthID] {
			continue
		}
		seen[account.AuthID] = true
		plan, ok := normalizeQuotaPlan(account.Plan)
		if !ok {
			plan = "team_standard"
		}
		weight := weights[plan]
		out.Accounts++
		out.Plans[plan]++
		out.TotalCapacity += weight
		known := false
		for _, window := range account.Windows {
			if normalizeWindowClass(window.Window) != class {
				continue
			}
			// An expired official cycle needs a new observation, not an invented refill.
			if reset, err := time.Parse(time.RFC3339, window.ResetAt); err == nil && window.CycleStarted && !window.PlaceholderReset && !reset.After(now) {
				break
			}
			remaining := window.RemainingPercent
			if math.IsNaN(remaining) || math.IsInf(remaining, 0) || remaining < 0 || remaining > 100 {
				break
			}
			out.KnownAccounts++
			out.KnownCapacity += weight
			out.RemainingCapacity += weight * remaining / 100
			if !account.Fresh {
				out.CachedAccounts++
			}
			if observed, err := time.Parse(time.RFC3339, window.ObservedAt); err == nil {
				previous, _ := time.Parse(time.RFC3339, out.LastObservedAt)
				if observed.After(previous) {
					out.LastObservedAt = window.ObservedAt
				}
			}
			if reset, err := time.Parse(time.RFC3339, window.ResetAt); err == nil && window.CycleStarted && !window.PlaceholderReset && reset.After(now) {
				previous, _ := time.Parse(time.RFC3339, out.NextResetAt)
				if previous.IsZero() || reset.Before(previous) {
					out.NextResetAt = window.ResetAt
				}
			}
			known = true
			break
		}
		if !known {
			out.UnknownAccounts++
		}
	}
	if out.TotalCapacity > 0 {
		out.MinimumPercent = 100 * out.RemainingCapacity / out.TotalCapacity
		out.MaximumPercent = math.Min(100, 100*(out.RemainingCapacity+out.TotalCapacity-out.KnownCapacity)/out.TotalCapacity)
		out.CoveragePercent = 100 * out.KnownCapacity / out.TotalCapacity
		out.Complete = out.UnknownAccounts == 0
		if out.Complete {
			value := out.MinimumPercent
			out.RemainingPercent = &value
		}
	}
	return out
}

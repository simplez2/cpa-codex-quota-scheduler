package main

import (
	"sort"
	"time"
)

// Only a full, current native account response can establish absence of a 5h
// window. Missing snapshots and partial response headers cannot establish it.
func poolFiveHourLimitStatus(native quotaSnapshot, observed quotaSnapshot, now time.Time, stale time.Duration) string {
	kind, at := native.FiveHourLimitStatus, native.FiveHourLimitObservedAt
	if observed.FiveHourLimitObservedAt.After(at) {
		kind, at = observed.FiveHourLimitStatus, observed.FiveHourLimitObservedAt
	}
	if kind != "" && !at.IsZero() {
		for _, window := range observed.Windows {
			// The native probe timestamp is captured before the request. A
			// completion with the same timestamp cannot safely be ordered before
			// it, so keep the observed 5h limit until a strictly newer probe.
			if normalizeWindowClass(window.Class) == "5h" && !window.ObservedAt.Before(at) {
				return "limited"
			}
		}
		if kind == "limited" {
			return "limited"
		}
		if kind == "not_reported" && !now.Before(at) && now.Sub(at) <= stale {
			return "not_reported"
		}
		return "unknown"
	}
	for _, window := range observed.Windows {
		if normalizeWindowClass(window.Class) == "5h" && !native.RefreshedAt.IsZero() && window.ObservedAt.After(native.RefreshedAt) {
			return "limited"
		}
	}
	if !native.RefreshedAt.IsZero() {
		for _, window := range native.Windows {
			if normalizeWindowClass(window.Class) == "5h" {
				return "limited"
			}
		}
		if !quotaSnapshotFresh(native, now, stale) || len(native.Windows) == 0 {
			return "unknown"
		}
		for _, window := range native.Windows {
			class := normalizeWindowClass(window.Class)
			if class != "weekly" && class != "monthly" {
				return "unknown"
			}
		}
		return "not_reported"
	}
	for _, window := range observed.Windows {
		if normalizeWindowClass(window.Class) == "5h" {
			return "limited"
		}
	}
	return "unknown"
}

type runtimePoolFiveHourStatus struct {
	runtimePoolWeeklyStatus
	InventoryAccounts       int      `json:"inventory_accounts"`
	ExcludedAccounts        int      `json:"excluded_accounts"`
	NoLimitReportedAccounts int      `json:"no_limit_reported_accounts"`
	UnclassifiedAccounts    int      `json:"unclassified_accounts"`
	ScopeComplete           bool     `json:"scope_complete"`
	InventoryAuthIDs        []string `json:"inventory_auth_ids"`
}

func poolFiveHourStatus(accounts []runtimeQuotaStatus, cfg pluginConfig, now time.Time) runtimePoolFiveHourStatus {
	out := runtimePoolFiveHourStatus{}
	excluded := map[string]bool{}
	for _, id := range cfg.PoolFiveHourExcludedAccounts {
		excluded[id] = true
	}
	seen := map[string]bool{}
	limited := []runtimeQuotaStatus{}
	for _, account := range accounts {
		if account.AuthID == "" || seen[account.AuthID] {
			continue
		}
		seen[account.AuthID] = true
		out.InventoryAccounts++
		out.InventoryAuthIDs = append(out.InventoryAuthIDs, account.AuthID)
		if excluded[account.AuthID] {
			out.ExcludedAccounts++
			continue
		}
		if account.FiveHourLimitStatus == "not_reported" {
			out.NoLimitReportedAccounts++
			continue
		}
		hasLimit := account.FiveHourLimitStatus == "limited"
		if account.FiveHourLimitStatus == "" {
			for _, window := range account.Windows {
				if normalizeWindowClass(window.Window) == "5h" {
					hasLimit = true
					break
				}
			}
		}
		if hasLimit {
			limited = append(limited, account)
		} else {
			out.UnclassifiedAccounts++
		}
	}
	out.runtimePoolWeeklyStatus = poolWindowStatus(limited, cfg.PoolFiveHourCapacityWeights, "5h", now)
	out.ScopeComplete = out.UnclassifiedAccounts == 0
	sort.Strings(out.InventoryAuthIDs)
	return out
}

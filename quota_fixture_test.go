package main

import (
	"encoding/json"
	"time"
)

// Fixture builder for policy tests. Runtime uses parseNativeQuota.
type quotaProbeResponse struct {
	Quota                               []quotaProbeRow `json:"quota"`
	RateLimitResetCreditsAvailableCount *int            `json:"rateLimitResetCreditsAvailableCount"`
}

type quotaProbeRow struct {
	Key               string            `json:"key"`
	Label             string            `json:"label"`
	UsedPercent       *float64          `json:"usedPercent"`
	Allowed           *bool             `json:"allowed"`
	LimitReached      *bool             `json:"limitReached"`
	Window            *quotaProbeWindow `json:"window"`
	ResetAt           json.RawMessage   `json:"resetAt"`
	ResetAfterSeconds *int64            `json:"resetAfterSeconds"`
	WindowUsageCost   *float64          `json:"window_usage_cost"`
}

type quotaProbeWindow struct {
	Seconds *int64 `json:"seconds"`
}

func normalizeQuotaSnapshot(index, fileName string, response quotaProbeResponse, refreshedAt, now time.Time) quotaSnapshot {
	out := quotaSnapshot{AuthID: fileName, AuthIndex: index, RefreshedAt: refreshedAt}
	if response.RateLimitResetCreditsAvailableCount != nil && *response.RateLimitResetCreditsAvailableCount > 0 {
		out.ResetCredits = *response.RateLimitResetCreditsAvailableCount
	}
	for _, row := range response.Quota {
		class := normalizeWindowClass(row.Label)
		seconds := int64(0)
		if row.Window != nil && row.Window.Seconds != nil {
			seconds = *row.Window.Seconds
		}
		if class == "" {
			class = windowClassFromSeconds(seconds)
		}
		if class == "" {
			// A future quota probe window type should not make the plugin unusable;
			// retain it as an unknown/lowest-priority window.
			class = "unknown"
		}
		used := 0.0
		if row.UsedPercent != nil {
			used = clampPercent(*row.UsedPercent)
		}
		allowed := true
		if row.Allowed != nil {
			allowed = *row.Allowed
		}
		limitReached := false
		if row.LimitReached != nil {
			limitReached = *row.LimitReached
		}
		resetAt := parseQuotaTime(row.ResetAt)
		if resetAt.IsZero() && row.ResetAfterSeconds != nil && *row.ResetAfterSeconds > 0 {
			resetAt = refreshedAt.Add(time.Duration(*row.ResetAfterSeconds) * time.Second)
		}
		window := quotaWindow{
			Class:         class,
			WindowSeconds: seconds,
			UsedPercent:   used,
			Allowed:       allowed,
			LimitReached:  limitReached || used >= usedPercentThreshold,
			ResetAt:       resetAt,
			Source:        quotaSourceProbe,
			ObservedAt:    refreshedAt,
		}
		if row.ResetAfterSeconds != nil && *row.ResetAfterSeconds >= 0 {
			window.ResetAfterSeconds = *row.ResetAfterSeconds
			window.ResetAfterSecondsKnown = true
		}
		if row.WindowUsageCost != nil && *row.WindowUsageCost >= 0 {
			window.WindowUsageCredits = *row.WindowUsageCost
			window.WindowUsageCreditsKnown = true
		}
		out.Windows = append(out.Windows, window)
	}
	return out
}

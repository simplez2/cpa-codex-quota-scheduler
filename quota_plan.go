package main

import (
	"strings"
	"time"
)

// Native labels are distinct from the user-configured aliases in
// normalizeQuotaPlan. In particular, native "business" is an enterprise SKU,
// not the configurable alias for Team Standard.
// Verified against openai/codex d6489472f3c15e87d2d7763a5fde033545c530f8:
// codex-rs/tui/src/status/helpers.rs and protocol/src/account.rs.
func nativeQuotaPlan(raw string) (string, bool) {
	switch normalizeNativePlanType(raw) {
	case "plus":
		return "plus", true
	case "team":
		return "team_standard", true
	case "self_serve_business_prolite":
		return "team_premium", true
	default:
		// Do not infer a fixed capacity from an unknown, metered, or ambiguous
		// SKU (including pro). The configured fallback remains available.
		return "", false
	}
}

func normalizeNativePlanType(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) > 80 {
		return ""
	}
	for _, c := range raw {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return raw
}

type quotaPlanObservation struct {
	Type       string
	Source     string
	ObservedAt time.Time
}

func (p quotaPlanObservation) fresh(now time.Time, maxAge time.Duration) bool {
	age := now.Sub(p.ObservedAt)
	return p.Type != "" && (p.Source == "cpa_usage" || p.Source == "cpa_auth_files") &&
		!p.ObservedAt.IsZero() && age >= 0 && age <= maxAge
}

func resolvedQuotaPlan(cfg pluginConfig, id string, snapshot quotaSnapshot, now time.Time) (plan string, weight float64, source string) {
	plan, weight = quotaPlanForAuth(cfg, id)
	if cfg.QuotaAccountPlans[id] != "" {
		return plan, weight, "account_override"
	}
	if snapshot.Plan.fresh(now, cfg.StaleAfter) {
		if detected, ok := nativeQuotaPlan(snapshot.Plan.Type); ok {
			return detected, quotaPlanWeight(detected), snapshot.Plan.Source
		}
	}
	return plan, weight, "default"
}

func mergeQuotaPlanObservation(previous, current quotaSnapshot, now time.Time, maxAge time.Duration) quotaPlanObservation {
	// An empty newer observation deliberately clears an old SKU. Reading a
	// cache or receiving quota-only completion headers must not renew its age.
	if previous.AuthID != "" && previous.AuthID == current.AuthID && previous.AuthIndex == current.AuthIndex &&
		previous.Plan.fresh(now, maxAge) && previous.Plan.ObservedAt.After(current.Plan.ObservedAt) {
		return previous.Plan
	}
	return current.Plan
}

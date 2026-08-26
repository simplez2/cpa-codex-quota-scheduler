package main

import (
	"encoding/json"
	"strings"
)

type runtimeEffectiveFiveHourPolicy struct {
	Mode                        string  `json:"mode"`
	ThresholdPercent            float64 `json:"threshold_percent,omitempty"`
	ThresholdSource             string  `json:"threshold_source"`
	ReservePercent              float64 `json:"reserve_percent,omitempty"`
	StrictBoundary              bool    `json:"strict_boundary"`
	ProjectedAdmissionGuard     bool    `json:"projected_admission_guard"`
	DrainCanOverrideBoundary    bool    `json:"drain_can_override_boundary"`
	SessionContinuationPastLine bool    `json:"session_continuation_past_boundary"`
	QuotaReady                  bool    `json:"quota_ready"`
	GenerationReady             bool    `json:"generation_ready"`
	PolicyReady                 bool    `json:"policy_ready"`
	CurrentPrimary5hUsed        float64 `json:"current_primary_5h_used_percent,omitempty"`
	CurrentPrimaryBoundaryHit   bool    `json:"current_primary_boundary_reached"`
}

// MarshalJSON augments the existing runtime status without removing or
// renaming any field consumed by current CPA panels. Operators should reason
// from effective_5h_policy rather than trying to infer behavior from several
// raw config fields whose interaction depends on handoff mode.
func (status runtimeStatus) MarshalJSON() ([]byte, error) {
	type runtimeStatusAlias runtimeStatus
	return json.Marshal(struct {
		runtimeStatusAlias
		EffectiveFiveHourPolicy runtimeEffectiveFiveHourPolicy `json:"effective_5h_policy"`
	}{
		runtimeStatusAlias:      runtimeStatusAlias(status),
		EffectiveFiveHourPolicy: effectiveFiveHourPolicy(status),
	})
}

func effectiveFiveHourPolicy(status runtimeStatus) runtimeEffectiveFiveHourPolicy {
	mode := normalizeSerial5hHandoffMode(status.Serial5hHandoffMode)
	policy := runtimeEffectiveFiveHourPolicy{
		Mode:                    mode,
		ThresholdSource:         "none",
		ReservePercent:          status.Reserve5hPercent,
		ProjectedAdmissionGuard: mode == "custom_threshold",
		QuotaReady:              status.FreshSnapshots > 0,
		GenerationReady:         !status.GenerationManaged || status.GenerationActive,
	}

	switch mode {
	case "custom_threshold":
		policy.ThresholdPercent = normalizedRuntimeThreshold(status.Serial5hSwitchPercent, status.SerialSwitchPercent)
		policy.ThresholdSource = "serial_5h_switch_percent"
		policy.StrictBoundary = true
		policy.DrainCanOverrideBoundary = false
		policy.SessionContinuationPastLine = false
	case "reserve_aware":
		// This mirrors the current scheduler implementation: reserve-aware 5h
		// handoff also retains the global threshold as an OR condition.
		policy.ThresholdPercent = normalizedRuntimeThreshold(status.SerialSwitchPercent, 98)
		policy.ThresholdSource = "serial_switch_percent"
		policy.DrainCanOverrideBoundary = true
		policy.SessionContinuationPastLine = true
	case "429_only":
		policy.DrainCanOverrideBoundary = false
		policy.SessionContinuationPastLine = true
	default: // inherit_global
		globalMode := normalizeSerialHandoffMode(status.SerialHandoffMode)
		policy.Mode = "inherit_global/" + globalMode
		policy.ThresholdPercent = normalizedRuntimeThreshold(status.SerialSwitchPercent, 98)
		policy.ThresholdSource = "serial_switch_percent"
		policy.DrainCanOverrideBoundary = true
		policy.SessionContinuationPastLine = true
	}

	policy.CurrentPrimary5hUsed = runtimePrimaryFiveHourUsed(status)
	if policy.StrictBoundary && policy.CurrentPrimary5hUsed > 0 {
		policy.CurrentPrimaryBoundaryHit = policy.CurrentPrimary5hUsed >= policy.ThresholdPercent
	}
	policy.PolicyReady = status.Enabled && strings.EqualFold(strings.TrimSpace(status.SchedulerMode), "serial") &&
		policy.QuotaReady && policy.GenerationReady
	return policy
}

func normalizedRuntimeThreshold(value, fallback float64) float64 {
	if value <= 0 || value > 100 {
		value = fallback
	}
	if value <= 0 || value > 100 {
		return 98
	}
	return value
}

func runtimePrimaryFiveHourUsed(status runtimeStatus) float64 {
	primary := strings.TrimSpace(status.SerialActiveAuthID)
	if primary == "" {
		return 0
	}
	maxUsed := 0.0
	for _, snapshot := range status.Snapshots {
		if strings.TrimSpace(snapshot.AuthID) != primary {
			continue
		}
		for _, window := range snapshot.Windows {
			if normalizeWindowClass(window.Window) != "5h" || !window.Active {
				continue
			}
			if window.UsedPercent > maxUsed {
				maxUsed = window.UsedPercent
			}
		}
	}
	return maxUsed
}

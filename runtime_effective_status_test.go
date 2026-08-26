package main

import (
	"encoding/json"
	"testing"
)

func TestRuntimeStatusExposesStrictEffectiveFiveHourPolicy(t *testing.T) {
	status := runtimeStatus{
		Enabled:               true,
		SchedulerMode:         "serial",
		SerialSwitchPercent:   98,
		SerialHandoffMode:     "threshold_only",
		Serial5hHandoffMode:   "custom_threshold",
		Serial5hSwitchPercent: 95,
		Reserve5hPercent:      15,
		FreshSnapshots:        2,
		GenerationManaged:     true,
		GenerationActive:      true,
		SerialActiveAuthID:    "primary",
		Snapshots: []runtimeQuotaStatus{{
			AuthID: "primary",
			Windows: []runtimeQuotaWindowStatus{{
				Window: "5h", UsedPercent: 96, Active: true,
			}},
		}},
	}

	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Serial5hSwitchPercent float64                        `json:"serial_5h_switch_percent"`
		Effective             runtimeEffectiveFiveHourPolicy `json:"effective_5h_policy"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Serial5hSwitchPercent != 95 {
		t.Fatalf("legacy raw config field disappeared: %#v", decoded)
	}
	if decoded.Effective.Mode != "custom_threshold" || decoded.Effective.ThresholdPercent != 95 || decoded.Effective.ThresholdSource != "serial_5h_switch_percent" {
		t.Fatalf("effective strict policy = %#v", decoded.Effective)
	}
	if !decoded.Effective.StrictBoundary || !decoded.Effective.ProjectedAdmissionConfigured || decoded.Effective.DrainCanOverrideBoundary || decoded.Effective.SessionContinuationPastLine {
		t.Fatalf("strict safety semantics = %#v", decoded.Effective)
	}
	if decoded.Effective.ProjectionFallback != "observed_threshold_when_capacity_unknown" {
		t.Fatalf("projection fallback = %q", decoded.Effective.ProjectionFallback)
	}
	if !decoded.Effective.PolicyReady || decoded.Effective.CurrentPrimary5hUsed != 96 || !decoded.Effective.CurrentPrimaryBoundaryHit {
		t.Fatalf("runtime readiness/current boundary = %#v", decoded.Effective)
	}
}

func TestRuntimeStatusReportsInheritedPolicyAsNonStrict(t *testing.T) {
	policy := effectiveFiveHourPolicy(runtimeStatus{
		Enabled:               true,
		SchedulerMode:         "serial",
		SerialSwitchPercent:   98,
		SerialHandoffMode:     "reserve_aware",
		Serial5hHandoffMode:   "inherit_global",
		Serial5hSwitchPercent: 95,
		FreshSnapshots:        1,
	})
	if policy.Mode != "inherit_global/reserve_aware" || policy.ThresholdPercent != 98 || policy.ThresholdSource != "serial_switch_percent" {
		t.Fatalf("inherited effective policy = %#v", policy)
	}
	if policy.StrictBoundary || policy.ProjectedAdmissionConfigured || !policy.DrainCanOverrideBoundary || !policy.SessionContinuationPastLine {
		t.Fatalf("inherited safety flags = %#v", policy)
	}
	if !policy.PolicyReady {
		t.Fatalf("unmanaged runtime with fresh quota should be ready: %#v", policy)
	}
}

func TestRuntimeStatusPolicyReadyRequiresFreshQuotaAndActiveGeneration(t *testing.T) {
	base := runtimeStatus{
		Enabled:               true,
		SchedulerMode:         "serial",
		Serial5hHandoffMode:   "custom_threshold",
		Serial5hSwitchPercent: 95,
		FreshSnapshots:        1,
		GenerationManaged:     true,
		GenerationActive:      true,
	}
	if !effectiveFiveHourPolicy(base).PolicyReady {
		t.Fatal("healthy managed runtime was not ready")
	}
	noQuota := base
	noQuota.FreshSnapshots = 0
	if effectiveFiveHourPolicy(noQuota).PolicyReady {
		t.Fatal("runtime without fresh quota reported policy_ready")
	}
	inactiveGeneration := base
	inactiveGeneration.GenerationActive = false
	if effectiveFiveHourPolicy(inactiveGeneration).PolicyReady {
		t.Fatal("inactive managed generation reported policy_ready")
	}
}

func TestReserveAwareEffectivePolicyDocumentsGlobalThresholdOrReserve(t *testing.T) {
	policy := effectiveFiveHourPolicy(runtimeStatus{
		Enabled:             true,
		SchedulerMode:       "serial",
		SerialSwitchPercent: 98,
		Serial5hHandoffMode: "reserve_aware",
		Reserve5hPercent:    15,
		FreshSnapshots:      1,
	})
	if policy.Mode != "reserve_aware" || policy.ThresholdPercent != 98 || policy.ThresholdSource != "serial_switch_percent" || policy.ReservePercent != 15 {
		t.Fatalf("reserve-aware effective policy = %#v", policy)
	}
}

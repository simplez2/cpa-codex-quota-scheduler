package main

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// serialAdmissionGate makes the serial decision and its predicted debit one
// admission transaction. Without this gate, several concurrent scheduler
// calls can all observe the same Keeper percentage before any one of them has
// reserved its expected request cost and collectively overshoot a strict 5h
// handoff boundary.
var serialAdmissionGate sync.Mutex

type serialAdmissionEstimate struct {
	Known            bool
	ObservedPercent  float64
	ProjectedPercent float64
	PredictedCredits float64
	PendingCredits   float64
	CapacityCredits  float64
}

func serialSoftThresholdReason(reason string) bool {
	return reason == "serial_threshold" || reason == "serial_projected_threshold"
}

// serialFiveHourCapacityCreditsLocked returns the best currently available
// estimate of a 5h window's total credit capacity. Caller must hold s.mu.
func (s *schedulerRuntimeState) serialFiveHourCapacityCreditsLocked(authID string, snapshot quotaSnapshot, now time.Time) float64 {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		authID = strings.TrimSpace(snapshot.AuthID)
	}
	if authID != "" {
		if state := s.pacingAccountLocked(authID); state != nil {
			if capacity := state.Capacities["5h"].Credits; capacity > 0 && !math.IsNaN(capacity) && !math.IsInf(capacity, 0) {
				return capacity
			}
		}
	}

	// Keeper can expose the current window usage cost before EWMA calibration
	// has accumulated enough samples. Derive a one-snapshot capacity estimate
	// rather than disabling the guard during that bootstrap interval.
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != "5h" {
			continue
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		if !window.WindowUsageCreditsKnown || window.WindowUsageCredits <= 0 || window.UsedPercent <= 0 {
			continue
		}
		capacity := window.WindowUsageCredits * 100 / window.UsedPercent
		if capacity > 0 && !math.IsNaN(capacity) && !math.IsInf(capacity, 0) {
			return capacity
		}
	}
	return 0
}

// serialFiveHourAdmissionEstimateLocked projects the current request plus all
// admitted-but-not-yet-observed requests onto the current 5h percentage.
// Caller must hold s.mu.
func (s *schedulerRuntimeState) serialFiveHourAdmissionEstimateLocked(req pluginapi.SchedulerPickRequest, choice serialCandidate, now time.Time) serialAdmissionEstimate {
	estimate := serialAdmissionEstimate{ObservedPercent: choice.FiveHourUsed}
	if normalizeSerial5hHandoffMode(s.cfg.Serial5hHandoffMode) != "custom_threshold" || !choice.QuotaKnown || !choice.FiveHourKnown {
		return estimate
	}

	authID := strings.TrimSpace(choice.Snapshot.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(choice.Candidate.ID)
	}
	capacity := s.serialFiveHourCapacityCreditsLocked(authID, choice.Snapshot, now)
	if capacity <= 0 {
		return estimate
	}

	lowQuota := choice.FiveHourRemaining <= s.cfg.LowQuotaPercent
	predicted := s.predictCostLocked(req, lowQuota)
	if predicted <= 0 || math.IsNaN(predicted) || math.IsInf(predicted, 0) {
		return estimate
	}
	pending := 0.0
	if state := s.pacingAccountLocked(authID); state != nil {
		for _, value := range state.PendingPredicted {
			if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
				pending += value
			}
	}
	projected := choice.FiveHourUsed + (pending+predicted)*100/capacity
	if projected < choice.FiveHourUsed {
		projected = choice.FiveHourUsed
	}
	estimate.Known = true
	estimate.ProjectedPercent = projected
	estimate.PredictedCredits = predicted
	estimate.PendingCredits = pending
	estimate.CapacityCredits = capacity
	return estimate
}

// serialProjectedFiveHourBoundaryLocked applies only to the dedicated custom
// 5h policy. Unknown capacity fails open to the existing observed-percent
// guard; it never fabricates a quota percentage.
func (s *schedulerRuntimeState) serialProjectedFiveHourBoundaryLocked(req pluginapi.SchedulerPickRequest, choice serialCandidate, now time.Time) bool {
	estimate := s.serialFiveHourAdmissionEstimateLocked(req, choice, now)
	if !estimate.Known {
		return false
	}
	threshold := s.cfg.Serial5hSwitchPercent
	if threshold <= 0 || threshold > 100 {
		threshold = s.cfg.SerialSwitchPercent
		if threshold <= 0 || threshold > 100 {
			threshold = 98
		}
	}
	return estimate.ProjectedPercent >= threshold
}

// recordSerialPredictedDebit reserves the selected request immediately after
// the serial decision while serialAdmissionGate is still held. Usage callback
// processing consumes this same PendingPredicted queue on completion.
func (s *schedulerRuntimeState) recordSerialPredictedDebit(authID string, req pluginapi.SchedulerPickRequest, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	lowQuota := false
	if snapshot, found := s.quotas[authID]; found && quotaSnapshotFresh(snapshot, now, s.cfg.StaleAfter) {
		for _, window := range snapshot.Windows {
			if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
				continue
			}
			if 100-window.UsedPercent <= s.cfg.LowQuotaPercent {
				lowQuota = true
				break
			}
		}
	}
	predicted := s.predictCostLocked(req, lowQuota)
	if predicted <= 0 || math.IsNaN(predicted) || math.IsInf(predicted, 0) {
		return
	}
	state := s.pacingAccountLocked(authID)
	state.DeficitCredits -= predicted
	state.PendingPredicted = append(state.PendingPredicted, predicted)
	if len(state.PendingPredicted) > 256 {
		state.PendingPredicted = state.PendingPredicted[len(state.PendingPredicted)-256:]
	}
}

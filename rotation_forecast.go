package main

import (
	"math"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func inspectSerialCandidateForRotation(snapshot quotaSnapshot, cfg pluginConfig, now time.Time) serialCandidate {
	return inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: snapshot.AuthID, Provider: providerCodex}, snapshot, true, cfg, now)
}

// Rotation forecasts use native quota increments, never API price discounts.
// They can advance background activation, but cannot reject a real request.
// All access is serialized by schedulerRuntimeState.mu.
func rotationSampleHorizon(cfg pluginConfig) time.Duration {
	horizon := 3 * cfg.QuotaRefreshCooldown
	if horizon < quotaRunwayMaxSampleGap {
		horizon = quotaRunwayMaxSampleGap
	}
	if horizon > 2*time.Hour {
		horizon = 2 * time.Hour
	}
	return horizon
}

func (s *schedulerRuntimeState) observeRotationQuotaLocked(snapshot quotaSnapshot, now time.Time) {
	s.rotationRunway.maxSampleGap = rotationSampleHorizon(s.cfg)
	s.rotationRunway.Observe(snapshot, now)
}

func (s *schedulerRuntimeState) rotationBurnLocked(snapshot quotaSnapshot, class string, capacity float64, now time.Time) (mean, p90, p95 float64, known bool) {
	id, index := strings.TrimSpace(snapshot.AuthID), strings.TrimSpace(snapshot.AuthIndex)
	tracker := &s.rotationRunway
	observation, ok := tracker.windows[quotaRunwayKey{id, index, class}]
	if !ok || tracker.bindings[id] != index || observation.observedAt.After(now) ||
		now.Sub(observation.observedAt) > rotationSampleHorizon(s.cfg) || !now.Before(observation.resetAt) {
		return
	}
	windowFound := false
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) == class && quotaRunwaySameReset(window.ResetAt, observation.resetAt) {
			windowFound = true
		}
	}
	if !windowFound || capacity <= 0 {
		return
	}
	rates := make([]float64, 0, len(observation.samples))
	for _, sample := range observation.samples {
		if !sample.at.After(now) && now.Sub(sample.at) <= rotationSampleHorizon(s.cfg) {
			rates = append(rates, sample.rate*capacity/100)
		}
	}
	if len(rates) < quotaRunwayMinimumSamples {
		return
	}
	mean = observation.ewma * capacity / 100
	p90 = math.Max(mean, sampleQuantile(rates, .90))
	p95 = math.Max(p90, sampleQuantile(rates, .95))
	known = finiteADQ(p95) && p95 > 0
	return
}

// Use plan-normalized units consistently across credentials. An absent rate is
// unknown, rather than zero demand or an unlimited runway. No extra polls are
// introduced; ordinary native probes supply these observations.
func (s *schedulerRuntimeState) rotationDemandLocked(now time.Time) float64 {
	return s.rotationDemandInUnitsLocked(now, true)
}

func (s *schedulerRuntimeState) adqForecastDemandLocked(now time.Time) float64 {
	return s.rotationDemandInUnitsLocked(now, false)
}

func (s *schedulerRuntimeState) rotationDemandInUnitsLocked(now time.Time, normalized bool) float64 {
	total := 0.0
	seen := make(map[string]bool)
	for _, snapshot := range s.quotas {
		id := strings.TrimSpace(snapshot.AuthID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		_, weight, _ := resolvedQuotaPlan(s.cfg, id, snapshot, now)
		class, capacity := "5h", 100*weight*defaultADQPolicy().MaxQuotaRatio5h
		weeklyCapacity := 100 * weight
		if !normalized {
			state := s.pacingAccounts[id]
			weekly, weekKnown := adqCapacityEstimateForClass(state, "weekly")
			five, fiveKnown := adqCapacityEstimateForClass(state, "5h")
			if !weekKnown {
				weekly, weekKnown = adqCapacityFromSnapshot(snapshot, "weekly")
			}
			if !fiveKnown {
				five, fiveKnown = adqCapacityFromSnapshot(snapshot, "5h")
			}
			weeklyCapacity, capacity, _, _ = adqResolveCapacities(weekly, weekKnown, five, fiveKnown, weight, defaultADQPolicy().MaxQuotaRatio5h)
		}
		if _, found := fiveHourWindow(snapshot); !found {
			if poolFiveHourLimitStatus(quotaSnapshot{}, snapshot, now, s.cfg.StaleAfter) != "not_reported" {
				continue
			}
			class, capacity = "weekly", weeklyCapacity
		}
		_, _, p95, known := s.rotationBurnLocked(snapshot, class, capacity, now)
		if known {
			total += p95
		} else if account := s.balancedAccounts[id]; account != nil && !account.LastPicked.IsZero() && now.Sub(account.LastPicked) < rotationSampleHorizon(s.cfg) {
			return math.NaN()
		}
	}
	if total <= 0 {
		return math.NaN()
	}
	return total
}

// Bring a planned activation forward if the currently active 5h reservoir
// cannot bridge the wait at observed demand. Unknown demand keeps ordinary
// best-effort scheduling. This never fabricates a provider reset or balance.
func (s *schedulerRuntimeState) rotationActivationDeadlineLocked(id string, target, now time.Time) time.Time {
	if !target.After(now) {
		return target
	}
	demand := s.rotationDemandLocked(now)
	if !finiteADQ(demand) || demand <= 0 {
		return target
	}
	remaining := 0.0
	seen := make(map[string]bool)
	for _, snapshot := range s.quotas {
		other := strings.TrimSpace(snapshot.AuthID)
		if other == "" || other == id || seen[other] || !quotaSnapshotFresh(snapshot, now, s.cfg.StaleAfter) {
			continue
		}
		seen[other] = true
		choice := inspectSerialCandidateForRotation(snapshot, s.cfg, now)
		if !choice.Eligible || !choice.FiveHourCycleActive {
			continue
		}
		_, weight, _ := resolvedQuotaPlan(s.cfg, other, snapshot, now)
		capacity := 100 * weight
		remaining += math.Min(choice.FiveHourRemaining/100*capacity*defaultADQPolicy().MaxQuotaRatio5h, choice.WeeklyRemaining/100*capacity)
	}
	seconds := remaining / demand * 3600
	if !finiteADQ(seconds) || seconds > fiveHourPhaseWindow.Seconds() {
		return target
	}
	deadline := now.Add(time.Duration(seconds*float64(time.Second)) - 2*time.Minute)
	if deadline.Before(now) {
		deadline = now
	}
	if deadline.Before(target) {
		return deadline
	}
	return target
}

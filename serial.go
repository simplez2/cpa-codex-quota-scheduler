package main

import (
	"sort"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// serialCandidate is the fill-first view of one CPA auth candidate. Serial
// mode deliberately ignores session identity: every request uses one global
// active auth until it becomes unsafe or unavailable.
type serialCandidate struct {
	Candidate    pluginapi.SchedulerAuthCandidate
	Snapshot     quotaSnapshot
	QuotaKnown   bool
	Eligible     bool
	Reason       string
	WindowClass  string
	CycleActive  bool
	MaxUsed      float64
	ResetCredits int
}

func serialWindowClass(snapshot quotaSnapshot) string {
	hasFiveHour := false
	hasMonthly := false
	for _, window := range snapshot.Windows {
		switch window.Class {
		case "weekly":
			return "weekly"
		case "monthly":
			hasMonthly = true
		case "5h":
			hasFiveHour = true
		}
	}
	if hasMonthly {
		return "monthly"
	}
	if hasFiveHour {
		return "5h"
	}
	return "unknown"
}

func serialWindowRank(class string) int {
	switch class {
	case "weekly":
		return 0
	case "monthly":
		return 1
	case "5h":
		return 2
	default:
		return 3
	}
}

func inspectSerialCandidate(candidate pluginapi.SchedulerAuthCandidate, snapshot quotaSnapshot, found bool, cfg pluginConfig, now time.Time) serialCandidate {
	choice := serialCandidate{
		Candidate:   candidate,
		Snapshot:    snapshot,
		Eligible:    true,
		Reason:      "quota_unknown",
		WindowClass: "unknown",
	}
	if !found || !quotaSnapshotFresh(snapshot, now, cfg.StaleAfter) {
		return choice
	}

	choice.QuotaKnown = true
	choice.Reason = "eligible"
	choice.WindowClass = serialWindowClass(snapshot)
	choice.ResetCredits = snapshot.ResetCredits
	threshold := cfg.SerialSwitchPercent
	if threshold <= 0 || threshold > 100 {
		threshold = 98
	}
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		if !window.Allowed {
			choice.Eligible = false
			choice.Reason = "not_allowed"
			return choice
		}
		if window.LimitReached || window.UsedPercent >= usedPercentThreshold {
			choice.Eligible = false
			choice.Reason = "limit_reached"
			return choice
		}
		if window.UsedPercent > choice.MaxUsed {
			choice.MaxUsed = window.UsedPercent
		}
		if window.UsedPercent > warmupMinimumAvailablePercent || (!window.ResetAt.IsZero() && now.Before(window.ResetAt)) {
			choice.CycleActive = true
		}
		if window.UsedPercent >= threshold {
			choice.Eligible = false
			choice.Reason = "serial_threshold"
			return choice
		}
	}
	return choice
}

func serialCandidateLess(a, b serialCandidate, cfg pluginConfig) bool {
	if rankA, rankB := serialWindowRank(a.WindowClass), serialWindowRank(b.WindowClass); rankA != rankB {
		return rankA < rankB
	}
	if cfg.SerialPreferActiveCycle && a.CycleActive != b.CycleActive {
		return a.CycleActive
	}
	if cfg.PreferResetCredits && (a.ResetCredits > 0) != (b.ResetCredits > 0) {
		return a.ResetCredits > 0
	}
	if a.MaxUsed != b.MaxUsed {
		return a.MaxUsed > b.MaxUsed
	}
	if a.Candidate.Priority != b.Candidate.Priority {
		return a.Candidate.Priority > b.Candidate.Priority
	}
	return a.Candidate.ID < b.Candidate.ID
}

func (s *schedulerRuntimeState) serialPick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	banned := make(map[string]bool, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if !banStore.schedulable(candidate.ID, now) {
			banned[strings.TrimSpace(candidate.ID)] = true
		}
	}

	s.mu.Lock()
	cfg := s.cfg
	previous := strings.TrimSpace(s.serialActiveAuthID)
	currentSeen := false
	currentReason := "candidate_unavailable"
	choices := make([]serialCandidate, 0, len(req.Candidates))
	thresholdChoices := make([]serialCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" {
			continue
		}
		if banned[candidate.ID] {
			if candidate.ID == previous {
				currentSeen = true
				currentReason = "quarantined"
			}
			continue
		}
		snapshot, found := s.quotas[candidate.ID]
		choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
		if candidate.ID == previous {
			currentSeen = true
			if choice.Eligible {
				s.mu.Unlock()
				return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}
			}
			currentReason = choice.Reason
			continue
		}
		if choice.Eligible {
			choices = append(choices, choice)
		} else if choice.Reason == "serial_threshold" {
			thresholdChoices = append(thresholdChoices, choice)
		}
	}
	if previous != "" && !currentSeen {
		currentReason = "candidate_unavailable"
	}

	thresholdFallback := false
	if len(choices) == 0 && previous != "" && currentSeen && currentReason == "serial_threshold" {
		// A soft threshold is a switch preference, not a reason to delegate to
		// CPA when every backup is equally full. Keep one account active until
		// a hard limit or 429 provides an authoritative failover signal.
		s.mu.Unlock()
		return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
	}
	if len(choices) == 0 && previous == "" && len(thresholdChoices) > 0 {
		choices = thresholdChoices
		thresholdFallback = true
	}
	sort.SliceStable(choices, func(i, j int) bool { return serialCandidateLess(choices[i], choices[j], cfg) })
	changed := false
	if len(choices) == 0 {
		if previous != "" {
			s.serialActiveAuthID = ""
			s.serialSelectedAt = time.Time{}
			s.serialSwitches++
			s.serialLastSwitchAt = now
			s.serialLastSwitchReason = currentReason
			changed = true
		}
		s.mu.Unlock()
		if changed {
			s.persistBanState()
		}
		return pluginapi.SchedulerPickResponse{Handled: false}
	}

	selected := choices[0].Candidate.ID
	reason := "initial_selection"
	if previous != "" {
		reason = currentReason
		s.serialSwitches++
	} else if strings.TrimSpace(s.serialLastSwitchReason) != "" && !s.serialLastSwitchAt.IsZero() {
		// A 429 handler clears the active auth before the next scheduler call.
		// Preserve that authoritative reason when the replacement is claimed.
		reason = s.serialLastSwitchReason
	} else if thresholdFallback {
		reason = "threshold_fallback"
	}
	s.serialActiveAuthID = selected
	s.serialSelectedAt = now
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = reason
	changed = true
	s.mu.Unlock()
	if changed {
		s.persistBanState()
	}
	return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
}

// markSerialUnavailable releases the active auth immediately after a 429 or
// another explicit failure signal. The caller persists the combined serial and
// quarantine state once, after all mutations are complete.
func (s *schedulerRuntimeState) markSerialUnavailable(authID, reason string, now time.Time) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if normalizeSchedulerMode(s.cfg.SchedulerMode) != "serial" || strings.TrimSpace(s.serialActiveAuthID) != authID {
		return false
	}
	s.serialActiveAuthID = ""
	s.serialSelectedAt = time.Time{}
	s.serialSwitches++
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = strings.TrimSpace(reason)
	return true
}

// claimSerialWarmupAuth makes warmup obey the same single-active-account rule
// as real traffic. It never replaces an already selected active auth.
func (s *schedulerRuntimeState) claimSerialWarmupAuth(authID string, now time.Time) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	s.mu.Lock()
	if normalizeSchedulerMode(s.cfg.SchedulerMode) != "serial" {
		s.mu.Unlock()
		return true
	}
	current := strings.TrimSpace(s.serialActiveAuthID)
	changed := false
	if current == "" {
		s.serialActiveAuthID = authID
		s.serialSelectedAt = now
		s.serialLastSwitchAt = now
		if strings.TrimSpace(s.serialLastSwitchReason) == "" {
			s.serialLastSwitchReason = "warmup_initial_selection"
		}
		current = authID
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.persistBanState()
	}
	return current == authID
}

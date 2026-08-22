package main

import (
	"sort"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const (
	// CPA temporarily removes an auth from request candidates for 60 seconds
	// after transient upstream failures. Keep the committed serial auth longer
	// than that host cooldown so a 408/5xx retry cannot become a permanent
	// account switch.
	serialCandidateMissingGrace         = 90 * time.Second
	serialCandidateMissingConfirmations = 3
)

// serialOverdraftTTL bounds how long an exhausted account keeps serving an
// in-flight session after the global switch. It is intentionally short: the
// courtesy window exists to finish the current task, not to run indefinitely.
const serialOverdraftTTL = 30 * time.Minute

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
	DrainActive  bool
	MaxUsed      float64
	ResetCredits int
}

// serialWindowDrains reports whether the window is close enough to its reset
// that the scheduler should let it run past the soft threshold instead of
// switching early. The official courtesy keeps an in-flight session alive to
// completion without extra charge after the limit is hit, and new requests are
// only blocked once Keeper marks the window as fully consumed, so burning the
// final percent near a reset is free value that would otherwise expire.
func serialWindowDrains(window quotaWindow, cfg pluginConfig, now time.Time) bool {
	if cfg.DrainWindowHours <= 0 {
		return false
	}
	class := normalizeWindowClass(window.Class)
	if class == "" || !window.Allowed || window.LimitReached {
		return false
	}
	if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return false
	}
	return window.ResetAt.Sub(now) <= time.Duration(cfg.DrainWindowHours*float64(time.Hour))
}

func serialWindowClass(snapshot quotaSnapshot, order []string) string {
	selected := "unknown"
	selectedRank := windowRankInOrder(selected, order)
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class == "" {
			continue
		}
		if rank := windowRankInOrder(class, order); rank < selectedRank {
			selected = class
			selectedRank = rank
		}
	}
	return selected
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
	choice.WindowClass = serialWindowClass(snapshot, cfg.WindowOrder)
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
		if quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {
			choice.CycleActive = true
		}
		if window.UsedPercent >= threshold && !serialWindowDrains(window, cfg, now) {
			choice.Eligible = false
			choice.Reason = "serial_threshold"
			return choice
		}
		if serialWindowDrains(window, cfg, now) {
			choice.DrainActive = true
		}
	}
	return choice
}

func serialCandidateLess(a, b serialCandidate, cfg pluginConfig) bool {
	if a.DrainActive != b.DrainActive {
		return a.DrainActive
	}
	if rankA, rankB := windowRankInOrder(a.WindowClass, cfg.WindowOrder), windowRankInOrder(b.WindowClass, cfg.WindowOrder); rankA != rankB {
		return rankA < rankB
	}
	if cfg.PreferResetCredits && (a.ResetCredits > 0) != (b.ResetCredits > 0) {
		return a.ResetCredits > 0
	}
	if cfg.SerialPreferActiveCycle && a.CycleActive != b.CycleActive {
		return a.CycleActive
	}
	if a.MaxUsed != b.MaxUsed {
		return a.MaxUsed > b.MaxUsed
	}
	if a.Candidate.Priority != b.Candidate.Priority {
		return a.Candidate.Priority > b.Candidate.Priority
	}
	return a.Candidate.ID < b.Candidate.ID
}

func serialPinnedAuthID(req pluginapi.SchedulerPickRequest) string {
	return extractMetadataString(req.Options.Metadata, "pinned_auth_id")
}

// resetSerialMissingLocked clears request-scoped absence tracking. The caller
// must hold s.mu.
func (s *schedulerRuntimeState) resetSerialMissingLocked() {
	s.serialMissingAuthID = ""
	s.serialFallbackAuthID = ""
	s.serialMissingSince = time.Time{}
	s.serialMissingCount = 0
}

// serialOverdraftAuthLocked returns the auth a session was pinned to for
// overdraft continuation, if that pin is still valid. The caller must hold s.mu.
func (s *schedulerRuntimeState) serialOverdraftAuthLocked(session string, now time.Time) string {
	if session == "" || s.serialOverdraft == nil {
		return ""
	}
	binding, ok := s.serialOverdraft[session]
	if !ok || now.Sub(binding.LastUsedAt) > serialOverdraftTTL {
		delete(s.serialOverdraft, session)
		return ""
	}
	return binding.AuthID
}

func (s *schedulerRuntimeState) setSerialOverdraftLocked(session, authID string, now time.Time) {
	if session == "" || authID == "" {
		return
	}
	if s.serialOverdraft == nil {
		s.serialOverdraft = make(map[string]serialOverdraftBinding)
	}
	for key, binding := range s.serialOverdraft {
		if now.Sub(binding.LastUsedAt) > serialOverdraftTTL {
			delete(s.serialOverdraft, key)
		}
	}
	s.serialOverdraft[session] = serialOverdraftBinding{AuthID: authID, LastUsedAt: now}
}

// serialRequestLocalPickLocked selects one candidate without changing the
// committed global serial auth. It is used for explicit pinned requests and
// transient candidate subsets. The caller must hold s.mu.
func (s *schedulerRuntimeState) serialRequestLocalPickLocked(
	candidates []pluginapi.SchedulerAuthCandidate,
	pinnedAuthID string,
	blocked map[string]banDisposition,
	cfg pluginConfig,
	now time.Time,
) pluginapi.SchedulerPickResponse {
	choices := make([]serialCandidate, 0, len(candidates))
	thresholdChoices := make([]serialCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" || (pinnedAuthID != "" && candidate.ID != pinnedAuthID) {
			continue
		}
		if _, unavailable := blocked[candidate.ID]; unavailable {
			continue
		}
		snapshot, found := s.quotas[candidate.ID]
		choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
		if choice.Eligible {
			choices = append(choices, choice)
		} else if choice.Reason == "serial_threshold" {
			thresholdChoices = append(thresholdChoices, choice)
		}
	}
	if len(choices) == 0 {
		choices = thresholdChoices
	}
	if len(choices) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	sort.SliceStable(choices, func(i, j int) bool { return serialCandidateLess(choices[i], choices[j], cfg) })
	return pluginapi.SchedulerPickResponse{AuthID: choices[0].Candidate.ID, Handled: true}
}

// serialOverdraftPickLocked serves one request from the session's pinned
// overdraft auth. The candidate must still be present in CPA's list; the
// quota check is skipped on purpose because the whole point of the pin is to
// keep using the exhausted account while the official courtesy allows it.
func (s *schedulerRuntimeState) serialOverdraftPickLocked(
	candidates []pluginapi.SchedulerAuthCandidate,
	session, overdraftAuthID string,
	blocked map[string]banDisposition,
	now time.Time,
) (pluginapi.SchedulerPickResponse, bool) {
	if overdraftAuthID == "" {
		return pluginapi.SchedulerPickResponse{}, false
	}
	for _, candidate := range candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID != overdraftAuthID {
			continue
		}
		if _, unavailable := blocked[candidate.ID]; unavailable {
			delete(s.serialOverdraft, session)
			return pluginapi.SchedulerPickResponse{}, false
		}
		s.setSerialOverdraftLocked(session, candidate.ID, now)
		return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}, true
	}
	delete(s.serialOverdraft, session)
	return pluginapi.SchedulerPickResponse{}, false
}

func (s *schedulerRuntimeState) serialPick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	blocked := make(map[string]banDisposition, len(req.Candidates))
	for _, candidate := range req.Candidates {
		authID := strings.TrimSpace(candidate.ID)
		if authID == "" {
			continue
		}
		if entry, ok := banStore.lookup(authID); ok {
			disposition := banEntryDisposition(entry, now)
			if disposition != banDispositionProbeReady {
				blocked[authID] = disposition
			}
		}
	}

	s.mu.Lock()
	cfg := s.cfg
	session := schedulerSessionHash(req)
	previous := strings.TrimSpace(s.serialActiveAuthID)
	overdraftOverride := ""
	sessionPinned := ""
	if previous != "" && session != "" {
		// Sticky binding: record which auth this session is being served by
		// on every pick. When that auth later becomes ineligible (threshold
		// crossed or limit reached), this existing binding lets the
		// overdraft route below keep serving the same conversation on the
		// exhausted account via the official courtesy window, without ever
		// having depended on catching the exact switch-over request. Never
		// overwrite an existing binding that points at a different auth:
		// that would destroy an active overdraft pin.
		sessionPinned = s.serialOverdraftAuthLocked(session, now)
		if sessionPinned == "" || sessionPinned == previous {
			s.setSerialOverdraftLocked(session, previous, now)
			sessionPinned = previous
		}
	}
	if pinnedAuthID := serialPinnedAuthID(req); pinnedAuthID != "" {
		response := s.serialRequestLocalPickLocked(req.Candidates, pinnedAuthID, blocked, cfg, now)
		s.mu.Unlock()
		return response
	}
	if overdraftAuthID := s.serialOverdraftAuthLocked(session, now); overdraftAuthID != "" && overdraftAuthID != previous {
		if response, ok := s.serialOverdraftPickLocked(req.Candidates, session, overdraftAuthID, blocked, now); ok {
			s.mu.Unlock()
			return response
		}
		previous = strings.TrimSpace(s.serialActiveAuthID)
	}
	currentSeen := false
	currentEligible := false
	currentChoice := serialCandidate{}
	currentReason := "candidate_unavailable"
	choices := make([]serialCandidate, 0, len(req.Candidates))
	thresholdChoices := make([]serialCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" {
			continue
		}
		if disposition, unavailable := blocked[candidate.ID]; unavailable {
			if candidate.ID == previous {
				currentSeen = true
				if disposition == banDispositionHalfOpen {
					// The outer atomic lease gate will reject every request except
					// the single admitted probe. Do not convert that collision into
					// a permanent global account switch.
					s.resetSerialMissingLocked()
					s.mu.Unlock()
					return pluginapi.SchedulerPickResponse{AuthID: candidate.ID, Handled: true}
				}
				currentReason = "quarantined"
			}
			continue
		}
		snapshot, found := s.quotas[candidate.ID]
		choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
		if candidate.ID == previous {
			currentSeen = true
			if choice.Eligible {
				currentEligible = true
				currentChoice = choice
				continue
			}
			if session != "" && sessionPinned == candidate.ID &&
				(choice.Reason == "serial_threshold" || choice.Reason == "limit_reached") &&
				overdraftOverride == "" {
				// In-flight overdraft: this exact conversation was already
				// being served by the now-exhausted auth, so remember to keep
				// serving it there (official courtesy). Do not short-circuit:
				// the global switch below must still move every other session
				// onto a fresh backup.
				overdraftOverride = candidate.ID
				s.setSerialOverdraftLocked(session, candidate.ID, now)
			}
			if session != "" && (choice.Reason == "serial_threshold" || choice.Reason == "limit_reached") {
				// The active auth just crossed the threshold: pin this in-flight
				// session to it so the official courtesy can finish the task,
				// while every other session moves to the fresh backup below.
				s.setSerialOverdraftLocked(session, candidate.ID, now)
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
	if currentSeen {
		s.resetSerialMissingLocked()
	}
	if currentEligible {
		// Serial mode remains fill-first within one quota class, but window_order
		// is a strict pool priority. When a higher-priority class becomes usable
		// again (for example, weekly resets while monthly is active), preempt the
		// lower-priority auth exactly once. Unknown/stale quota snapshots never
		// trigger a preemption.
		sort.SliceStable(choices, func(i, j int) bool { return serialCandidateLess(choices[i], choices[j], cfg) })
		currentRank := windowRankInOrder(currentChoice.WindowClass, cfg.WindowOrder)
		if currentChoice.QuotaKnown && len(choices) > 0 && choices[0].QuotaKnown &&
			windowRankInOrder(choices[0].WindowClass, cfg.WindowOrder) < currentRank {
			selected := choices[0].Candidate.ID
			s.serialActiveAuthID = selected
			s.serialSelectionSource = "auto"
			s.serialSelectedAt = now
			s.serialSwitches++
			s.serialLastSwitchAt = now
			s.serialLastSwitchReason = "higher_priority_window_available"
			s.mu.Unlock()
			s.persistBanState()
			return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
		}
		s.mu.Unlock()
		return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
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
	if previous != "" && !currentSeen {
		if len(choices) == 0 {
			choices = thresholdChoices
		}
		sort.SliceStable(choices, func(i, j int) bool { return serialCandidateLess(choices[i], choices[j], cfg) })
		if s.serialMissingAuthID != previous {
			s.resetSerialMissingLocked()
			s.serialMissingAuthID = previous
			s.serialMissingSince = now
		}
		s.serialMissingCount++
		if len(choices) == 0 {
			s.mu.Unlock()
			return pluginapi.SchedulerPickResponse{Handled: false}
		}
		selected := ""
		for _, choice := range choices {
			if choice.Candidate.ID == s.serialFallbackAuthID {
				selected = choice.Candidate.ID
				break
			}
		}
		if selected == "" {
			selected = choices[0].Candidate.ID
			s.serialFallbackAuthID = selected
		}
		confirmed := s.serialMissingCount >= serialCandidateMissingConfirmations &&
			!s.serialMissingSince.IsZero() && now.Sub(s.serialMissingSince) >= serialCandidateMissingGrace
		if !confirmed {
			s.serialFallbacks++
			s.mu.Unlock()
			return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
		}
		s.serialActiveAuthID = selected
		s.serialSelectionSource = "auto"
		s.serialSelectedAt = now
		s.serialSwitches++
		s.serialLastSwitchAt = now
		s.serialLastSwitchReason = "candidate_unavailable_confirmed"
		s.resetSerialMissingLocked()
		s.mu.Unlock()
		s.persistBanState()
		return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
	}

	sort.SliceStable(choices, func(i, j int) bool { return serialCandidateLess(choices[i], choices[j], cfg) })
	changed := false
	if len(choices) == 0 {
		if previous != "" {
			s.serialActiveAuthID = ""
			s.serialSelectionSource = "auto"
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
	s.serialSelectionSource = "auto"
	s.serialSelectedAt = now
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = reason
	s.resetSerialMissingLocked()
	changed = true
	s.mu.Unlock()
	if changed {
		s.persistBanState()
	}
	if overdraftOverride != "" {
		return pluginapi.SchedulerPickResponse{AuthID: overdraftOverride, Handled: true}
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
	s.serialSelectionSource = "auto"
	s.serialSelectedAt = time.Time{}
	s.serialSwitches++
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = strings.TrimSpace(reason)
	s.resetSerialMissingLocked()
	return true
}

func normalizeSerialSelectionSource(source string) string {
	if strings.EqualFold(strings.TrimSpace(source), "manual") {
		return "manual"
	}
	return "auto"
}

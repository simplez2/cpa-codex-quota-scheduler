package main

import (
	"math"
	"sort"
	"strings"
	"time"
)

const (
	fiveHourPhaseWindow   = 5 * time.Hour
	fiveHourPhaseBucket   = 15 * time.Minute
	fiveHourPhaseSkew     = 2 * time.Minute
	fiveHourPhaseUnknown  = "UNKNOWN"
	fiveHourPhaseFirstUse = "FIRST_USE_AFTER_RESET"
	fiveHourPhaseFixed    = "FIXED_PROVIDER_WINDOW"
)

// Each credential owns its official anchor and an independent idle activation
// plan. ActivateAt is local scheduling only: it never changes quota ResetAt.
type fiveHourPhaseState struct {
	AuthID            string    `json:"auth_id"`
	AuthIndex         string    `json:"auth_index,omitempty"`
	Cycle             uint64    `json:"cycle_generation"`
	AnchorMode        string    `json:"anchor_mode"`
	ActiveResetAt     time.Time `json:"official_reset_at,omitempty"`
	WeeklyResetAt     time.Time `json:"weekly_reset_at,omitempty"`
	ObservedAt        time.Time `json:"observed_at,omitempty"`
	DormantObservedAt time.Time `json:"dormant_observed_at,omitempty"`
	ActivationAt      time.Time `json:"activation_at,omitempty"`
	PlannedResetAt    time.Time `json:"planned_reset_at,omitempty"`
	ActivateAt        time.Time `json:"activate_not_before,omitempty"`
}

type runtimeFiveHourPhaseStatus struct {
	Cycle           uint64    `json:"cycle_generation"`
	AnchorMode      string    `json:"anchor_mode"`
	State           string    `json:"state"`
	ActivateAt      time.Time `json:"activate_not_before,omitempty"`
	OfficialResetAt time.Time `json:"official_reset_at,omitempty"`
	Bucket          int       `json:"phase_bucket"`
}

func cloneFiveHourPhases(states map[string]fiveHourPhaseState) map[string]fiveHourPhaseState {
	out := make(map[string]fiveHourPhaseState, len(states))
	for id, state := range states {
		out[id] = state
	}
	return out
}

func fiveHourWindow(snapshot quotaSnapshot) (quotaWindow, bool) {
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) == "5h" {
			return window, true
		}
	}
	return quotaWindow{}, false
}

func nearPhaseTime(a, b time.Time) bool {
	return !a.IsZero() && !b.IsZero() && math.Abs(a.Sub(b).Seconds()) <= fiveHourPhaseSkew.Seconds()
}

// Observations come from native CPA quota probes/headers, never timer-based
// invented balances. A delayed successful request plus a fresh dormant probe
// is needed to distinguish a first-use anchor from a fixed provider window.
func (s *schedulerRuntimeState) observeFiveHourPhaseLocked(snapshot quotaSnapshot, now time.Time) {
	id := strings.TrimSpace(snapshot.AuthID)
	window, found := fiveHourWindow(snapshot)
	if id == "" || !found {
		return
	}
	if s.fiveHourPhases == nil {
		s.fiveHourPhases = make(map[string]fiveHourPhaseState)
	}
	state := s.fiveHourPhases[id]
	if state.AuthIndex != "" && state.AuthIndex != snapshot.AuthIndex {
		state = fiveHourPhaseState{}
	}
	state.AuthID, state.AuthIndex = id, snapshot.AuthIndex
	if state.AnchorMode == "" {
		state.AnchorMode = fiveHourPhaseUnknown
	}
	observed := window.ObservedAt
	if observed.IsZero() {
		observed = snapshot.RefreshedAt
	}
	if observed.IsZero() || observed.Before(state.ObservedAt) || observed.After(now) {
		return
	}
	// Weekly first activation has priority over phase recovery. A fresh new
	// official weekly anchor starts generation one again, keeping capability.
	for _, weekly := range snapshot.Windows {
		if normalizeWindowClass(weekly.Class) != "weekly" || weekly.ResetAt.IsZero() || !quotaWindowCycleStarted(weekly, snapshot.RefreshedAt, now) {
			continue
		}
		if !state.WeeklyResetAt.IsZero() && !nearPhaseTime(weekly.ResetAt, state.WeeklyResetAt) {
			mode := state.AnchorMode
			state = fiveHourPhaseState{AuthID: id, AuthIndex: snapshot.AuthIndex, AnchorMode: mode}
		}
		state.WeeklyResetAt = weekly.ResetAt
		break
	}
	state.ObservedAt = observed
	started := quotaWindowCycleStarted(window, snapshot.RefreshedAt, now)
	if !started {
		if !state.ActiveResetAt.IsZero() && !observed.Before(state.ActiveResetAt) {
			state.DormantObservedAt = observed
		}
		s.fiveHourPhases[id] = state
		return
	}
	if window.ResetAt.IsZero() || !window.ResetAt.After(now) {
		s.fiveHourPhases[id] = state
		return
	}
	if state.ActiveResetAt.IsZero() {
		state.Cycle = 1
	} else if !nearPhaseTime(state.ActiveResetAt, window.ResetAt) {
		previousReset := state.ActiveResetAt
		if !observed.Before(previousReset) && window.ResetAt.After(previousReset) {
			state.Cycle++
			if !state.ActivationAt.IsZero() && state.ActivationAt.After(previousReset.Add(5*time.Minute)) &&
				!state.DormantObservedAt.IsZero() && !state.DormantObservedAt.After(state.ActivationAt) &&
				state.ActivationAt.Sub(state.DormantObservedAt) <= 5*time.Minute && !observed.Before(state.ActivationAt) {
				length := time.Duration(window.WindowSeconds) * time.Second
				if length <= 0 {
					length = fiveHourPhaseWindow
				}
				if length == fiveHourPhaseWindow {
					if nearPhaseTime(window.ResetAt, state.ActivationAt.Add(length)) {
						state.AnchorMode = fiveHourPhaseFirstUse
					} else {
						steps := math.Round(window.ResetAt.Sub(previousReset).Seconds() / length.Seconds())
						if steps >= 1 && nearPhaseTime(window.ResetAt, previousReset.Add(time.Duration(steps)*length)) {
							state.AnchorMode = fiveHourPhaseFixed
						}
					}
				}
			}
		} else {
			// An unexpected provider reset is a new first generation, rather
			// than evidence that our local activation plan changed its clock.
			state.Cycle = 1
		}
		state.PlannedResetAt, state.ActivateAt = time.Time{}, time.Time{}
		state.ActivationAt, state.DormantObservedAt = time.Time{}, time.Time{}
	}
	state.ActiveResetAt = window.ResetAt
	s.fiveHourPhases[id] = state
}

func (s *schedulerRuntimeState) noteFiveHourActivationLocked(id, index string, at time.Time) {
	state, exists := s.fiveHourPhases[id]
	if !exists || (state.AuthIndex != "" && state.AuthIndex != index) || at.IsZero() || state.ActiveResetAt.IsZero() || at.Before(state.ActiveResetAt) {
		return
	}
	if state.ActivationAt.IsZero() || at.Before(state.ActivationAt) {
		state.ActivationAt = at
	}
	s.fiveHourPhases[id] = state
}

func phaseBucket(at time.Time) int {
	if at.IsZero() {
		return -1
	}
	seconds := int64(fiveHourPhaseWindow / time.Second)
	offset := ((at.Unix() % seconds) + seconds) % seconds
	return int(offset / int64(fiveHourPhaseBucket/time.Second))
}

// Weighted phase deficit with largest-gap tie breaking spreads small pools
// across the full 300 minutes instead of packing them into adjacent buckets.
// One dormant credential remains immediately activatable when all anchors end.
func (s *schedulerRuntimeState) nextFiveHourActivationLocked(id string, now time.Time) time.Time {
	const buckets = 20
	occupied := [buckets]float64{}
	ready := false
	for other, phase := range s.fiveHourPhases {
		if other == id {
			continue
		}
		snapshot, ok := s.quotas[other]
		if !ok || snapshot.AuthIndex != phase.AuthIndex {
			continue
		}
		window, ok := fiveHourWindow(snapshot)
		if !ok || (!window.ResetAt.IsZero() && window.ResetAt.After(now) && (!window.Allowed || window.LimitReached || window.UsedPercent >= 100)) {
			continue
		}
		blocked := false
		for _, w := range snapshot.Windows {
			if normalizeWindowClass(w.Class) != "5h" && w.ResetAt.After(now) && (!w.Allowed || w.LimitReached || w.UsedPercent >= 100) {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		anchor := phase.ActiveResetAt
		if !phase.ActivateAt.IsZero() && !phase.PlannedResetAt.IsZero() {
			anchor = phase.ActivateAt
			if !phase.ActivateAt.After(now) {
				ready = true
			}
		}
		if phase.ActiveResetAt.After(now) && quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {
			ready = true
		}
		bucket := phaseBucket(anchor)
		if bucket < 0 {
			continue
		}
		_, weight, _ := resolvedQuotaPlan(s.cfg, other, snapshot, now)
		occupied[bucket] += math.Max(1, weight)
	}
	if !ready {
		return now
	}
	best, bestCapacity, bestDistance := -1, math.Inf(1), -1
	for b := 0; b < buckets; b++ {
		distance := buckets
		for o, capacity := range occupied {
			if capacity <= 0 {
				continue
			}
			delta := int(math.Abs(float64(b - o)))
			if buckets-delta < delta {
				delta = buckets - delta
			}
			if delta < distance {
				distance = delta
			}
		}
		if occupied[b] < bestCapacity || (occupied[b] == bestCapacity && distance > bestDistance) {
			best, bestCapacity, bestDistance = b, occupied[b], distance
		}
	}
	base := time.Unix(now.Unix()/int64(fiveHourPhaseWindow/time.Second)*int64(fiveHourPhaseWindow/time.Second), 0).UTC()
	target := base.Add(time.Duration(best) * fiveHourPhaseBucket)
	if target.Before(now) {
		target = target.Add(fiveHourPhaseWindow)
	}
	return target
}

// Assign once per expired official cycle and persist before dispatch. Plans
// are only for idle 5h activation; weekly bootstrap and natural calls bypass.
func (s *schedulerRuntimeState) planFiveHourWarmups(candidates []warmupCandidate, now time.Time) []warmupCandidate {
	s.mu.Lock()
	changed := false
	indices := make([]int, len(candidates))
	for i := range indices {
		indices[i] = i
	}
	sort.Slice(indices, func(i, j int) bool {
		return candidates[indices[i]].Snapshot.AuthID < candidates[indices[j]].Snapshot.AuthID
	})
	for _, index := range indices {
		candidate := &candidates[index]
		s.observeFiveHourPhaseLocked(candidate.Snapshot, now)
		if normalizeWindowClass(candidate.Window.Class) != "5h" {
			if phase, ok := s.fiveHourPhases[candidate.Snapshot.AuthID]; ok && !phase.PlannedResetAt.IsZero() {
				phase.PlannedResetAt, phase.ActivateAt = time.Time{}, time.Time{}
				s.fiveHourPhases[candidate.Snapshot.AuthID] = phase
				changed = true
			}
			continue
		}
		phase, ok := s.fiveHourPhases[candidate.Snapshot.AuthID]
		if !ok || phase.Cycle == 0 || phase.ActiveResetAt.IsZero() || phase.ActiveResetAt.After(now) || phase.AnchorMode == fiveHourPhaseFixed {
			continue
		}
		if !phase.PlannedResetAt.Equal(phase.ActiveResetAt) || phase.ActivateAt.IsZero() {
			phase.PlannedResetAt = phase.ActiveResetAt
			phase.ActivateAt = s.nextFiveHourActivationLocked(candidate.Snapshot.AuthID, now)
			s.fiveHourPhases[candidate.Snapshot.AuthID] = phase
			changed = true
		}
		candidate.ActivateAt = phase.ActivateAt
	}
	s.mu.Unlock()
	if changed {
		s.persistBanState()
	}
	return candidates
}

func fiveHourPhaseStatus(state fiveHourPhaseState, now time.Time) runtimeFiveHourPhaseStatus {
	status := runtimeFiveHourPhaseStatus{Cycle: state.Cycle, AnchorMode: state.AnchorMode, OfficialResetAt: state.ActiveResetAt, Bucket: phaseBucket(state.ActiveResetAt), State: "observing"}
	if status.AnchorMode == "" {
		status.AnchorMode = fiveHourPhaseUnknown
	}
	switch {
	case state.AnchorMode == fiveHourPhaseFixed:
		status.State = "fixed_window"
	case state.ActivateAt.After(now) && !state.PlannedResetAt.IsZero():
		status.State, status.ActivateAt = "idle_activation_deferred", state.ActivateAt
	case state.ActiveResetAt.After(now):
		status.State = "active"
	case !state.PlannedResetAt.IsZero():
		status.State, status.ActivateAt = "activation_due", state.ActivateAt
	}
	return status
}

func (s *schedulerRuntimeState) restoreFiveHourPhasesLocked(saved map[string]fiveHourPhaseState, warmups map[string]warmupEntry, cycles map[string]time.Time, now time.Time) {
	s.fiveHourPhases = make(map[string]fiveHourPhaseState)
	for id, state := range saved {
		snapshot, ok := s.quotas[id]
		if !ok || state.AuthID != id || state.AuthIndex != snapshot.AuthIndex {
			continue
		}
		s.fiveHourPhases[id] = state
	}
	for _, entry := range warmups {
		if normalizeWindowClass(entry.Window) != "5h" || entry.ActivatedAt.IsZero() || entry.ResetAt.IsZero() {
			continue
		}
		snapshot, ok := s.quotas[entry.AuthID]
		if !ok || snapshot.AuthIndex != entry.AuthIndex {
			continue
		}
		if _, exists := s.fiveHourPhases[entry.AuthID]; !exists {
			s.fiveHourPhases[entry.AuthID] = fiveHourPhaseState{AuthID: entry.AuthID, AuthIndex: entry.AuthIndex, Cycle: 1, AnchorMode: fiveHourPhaseUnknown, ActiveResetAt: entry.ResetAt, ObservedAt: entry.ActivatedAt}
		}
	}
	for id, reset := range cycles {
		if _, exists := s.fiveHourPhases[id]; exists || reset.IsZero() {
			continue
		}
		if snapshot, ok := s.quotas[id]; ok {
			window, found := fiveHourWindow(snapshot)
			if !found || !window.ResetAt.Equal(reset) || !quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {
				continue
			}
			s.fiveHourPhases[id] = fiveHourPhaseState{AuthID: id, AuthIndex: snapshot.AuthIndex, Cycle: 1, AnchorMode: fiveHourPhaseUnknown, ActiveResetAt: reset}
		}
	}
	for id, snapshot := range s.quotas {
		if id == snapshot.AuthID {
			s.observeFiveHourPhaseLocked(snapshot, now)
		}
	}
}

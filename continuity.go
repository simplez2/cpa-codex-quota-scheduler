package main

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// The host owns the cancellable wait. This state only contains bounded,
// in-memory pacing reservations and redacted diagnostics; no request is replayed.
type continuityGate struct {
	mu            sync.Mutex
	usage         map[string][]time.Time
	usageBindings map[string]string
	reservations  map[string]time.Time
	dispatched    map[string]bool
	scopes        map[string]map[string]continuityCycle
	intervals     map[string]time.Duration
	cycles        map[string]continuityCycle
	currentScope  map[string]continuityCycle
	lastByAuth    map[string]time.Time
	nextPermit    time.Time
	lastPermit    time.Time
	lastInterval  time.Duration
	calls         uint64
	lastAction    string
	lastReason    string
	lastWaitMS    int64
	lastResetAt   string
	lastChecked   time.Time
}

type continuityCycle struct {
	index      string
	reset      time.Time
	weeklyOnly bool
}

type runtimeContinuityStatus struct {
	Enabled        bool   `json:"enabled"`
	PacingEnabled  bool   `json:"pacing_enabled"`
	HostCalls      uint64 `json:"host_calls"`
	HostObserved   bool   `json:"host_observed"`
	MaxWaitMS      int64  `json:"max_wait_ms"`
	PollIntervalMS int64  `json:"poll_interval_ms"`
	LastAction     string `json:"last_action,omitempty"`
	LastReason     string `json:"last_reason,omitempty"`
	LastWaitMS     int64  `json:"last_wait_ms"`
	LastResetAt    string `json:"last_reset_at,omitempty"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
}

const continuityUsageHorizon = 15 * time.Minute
const continuityMaxReservations = 2048

func (s *schedulerRuntimeState) continuityStatus(cfg pluginConfig) runtimeContinuityStatus {
	g := &s.continuity
	g.mu.Lock()
	defer g.mu.Unlock()
	out := runtimeContinuityStatus{Enabled: cfg.Enabled && cfg.ContinuityEnabled,
		PacingEnabled: cfg.Enabled && cfg.ContinuityEnabled && cfg.ContinuityPacing,
		HostCalls:     g.calls, HostObserved: g.calls != 0, MaxWaitMS: cfg.ContinuityMaxWait.Milliseconds(),
		PollIntervalMS: cfg.ContinuityPollInterval.Milliseconds(), LastAction: g.lastAction,
		LastReason: g.lastReason, LastWaitMS: g.lastWaitMS, LastResetAt: g.lastResetAt}
	if !g.lastChecked.IsZero() {
		out.LastCheckedAt = g.lastChecked.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *schedulerRuntimeState) noteContinuityUsage(authID, authIndex string, now time.Time) {
	s.mu.RLock()
	id := s.canonicalAuthIDLocked(authID, authIndex)
	q, found := s.lookupQuotaLocked(id, authIndex)
	index := strings.TrimSpace(q.AuthIndex)
	valid := found && index != "" && s.identities[index] == id &&
		(strings.TrimSpace(authIndex) == "" || strings.TrimSpace(authIndex) == index)
	s.mu.RUnlock()
	if id == "" || !valid {
		return
	}
	g := &s.continuity
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.usage == nil {
		g.usage = make(map[string][]time.Time)
	}
	if g.usageBindings == nil {
		g.usageBindings = make(map[string]string)
	}
	if g.usageBindings[id] != index {
		delete(g.usage, id)
		g.usageBindings[id] = index
	}
	values := g.usage[id]
	first := 0
	for first < len(values) && now.Sub(values[first]) > continuityUsageHorizon {
		first++
	}
	values = append(values[first:], now)
	if len(values) > 512 {
		values = append([]time.Time(nil), values[len(values)-512:]...)
	}
	g.usage[id] = values
}

func (s *schedulerRuntimeState) continuityComplete(id string) {
	g := &s.continuity
	g.mu.Lock()
	id = strings.TrimSpace(id)
	delete(g.reservations, id)
	delete(g.dispatched, id)
	delete(g.scopes, id)
	delete(g.intervals, id)
	g.recomputeNextLocked(time.Now())
	g.mu.Unlock()
}

// Only a request which reaches after-auth has actually consumed an upstream
// pacing permit. A canceled, undispatched reservation cannot impose a delay.
func (s *schedulerRuntimeState) continuityDispatched(id string, selected ...string) {
	g := &s.continuity
	g.mu.Lock()
	id = strings.TrimSpace(id)
	if _, ok := g.reservations[id]; ok && !g.dispatched[id] {
		if g.dispatched == nil {
			g.dispatched = make(map[string]bool)
		}
		g.dispatched[id] = true
		g.lastPermit = time.Now()
		if g.lastByAuth == nil {
			g.lastByAuth = make(map[string]time.Time)
		}
		authID := ""
		if len(selected) > 0 {
			authID = strings.TrimSpace(selected[0])
		}
		if authID != "" {
			g.lastByAuth[authID] = g.lastPermit
		} else {
			for candidate := range g.scopes[id] {
				g.lastByAuth[candidate] = g.lastPermit
			}
		}
		g.recomputeNextLocked(g.lastPermit)
	}
	g.mu.Unlock()
}

func (g *continuityGate) recomputeNextLocked(now time.Time) {
	// Reclaim canceled tail reservations without pulling an existing waiter
	// ahead of its already promised time.
	next := g.lastPermit.Add(g.lastInterval)
	if next.Before(now) {
		next = now
	}
	for id, at := range g.reservations {
		if g.dispatched[id] {
			continue
		}
		if at.Add(g.lastInterval).After(next) {
			next = at.Add(g.lastInterval)
		}
	}
	g.nextPermit = next
}

func (g *continuityGate) clearPacing(now time.Time) {
	g.mu.Lock()
	for id := range g.reservations {
		if !g.dispatched[id] {
			delete(g.reservations, id)
			delete(g.scopes, id)
			delete(g.intervals, id)
		}
	}
	g.lastPermit = time.Time{}
	g.lastInterval = 0
	g.nextPermit = now
	g.cycles, g.currentScope, g.lastByAuth = nil, nil, nil
	g.mu.Unlock()
}

func continuitySameScope(a, b map[string]continuityCycle) bool {
	if len(a) != len(b) {
		return false
	}
	for id, cycle := range a {
		other, ok := b[id]
		if !ok || !continuitySameCycle(cycle, other) {
			return false
		}
	}
	return true
}

func continuitySameCycle(a, b continuityCycle) bool {
	return a.index == b.index && a.weeklyOnly == b.weeklyOnly &&
		(a.weeklyOnly || quotaRunwaySameReset(a.reset, b.reset))
}

func (g *continuityGate) setPacingCycle(scope map[string]continuityCycle, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cycles == nil {
		g.cycles = make(map[string]continuityCycle)
	}
	changed := make(map[string]bool)
	for id, cycle := range scope {
		if old, exists := g.cycles[id]; exists {
			if continuitySameCycle(old, cycle) {
				// Preserve the original anchor so small timestamp jitter does not
				// accumulate indefinitely into a false cycle transition.
				continue
			}
			changed[id] = true
			delete(g.lastByAuth, id)
		}
		g.cycles[id] = cycle
	}
	for requestID, previous := range g.scopes {
		if g.dispatched[requestID] {
			continue
		}
		for id := range previous {
			if changed[id] {
				delete(g.reservations, requestID)
				delete(g.scopes, requestID)
				delete(g.intervals, requestID)
				break
			}
		}
	}
	g.currentScope = scope
	g.recomputeNextLocked(now)
}

func (g *continuityGate) clearPacingScope(scope map[string]continuityCycle, requestID string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.reservations {
		if !g.dispatched[id] && (id == requestID || (len(scope) > 0 && continuitySameScope(g.scopes[id], scope))) {
			delete(g.reservations, id)
			delete(g.scopes, id)
			delete(g.intervals, id)
		}
	}
	g.recomputeNextLocked(now)
}

func (s *schedulerRuntimeState) pruneContinuityUsage(inventory map[string]cpaAuthFileEntry) {
	g := &s.continuity
	g.mu.Lock()
	defer g.mu.Unlock()
	for id := range g.usage {
		entry, exists := inventory[id]
		if !exists || g.usageBindings[id] != strings.TrimSpace(entry.AuthIndex) {
			delete(g.usage, id)
			delete(g.usageBindings, id)
			delete(g.cycles, id)
			delete(g.lastByAuth, id)
		}
	}
	for id, cycle := range g.cycles {
		entry, exists := inventory[id]
		if !exists || cycle.index != strings.TrimSpace(entry.AuthIndex) {
			delete(g.cycles, id)
			delete(g.lastByAuth, id)
		}
	}
}

func (g *continuityGate) record(response pluginapi.SchedulerContinuityResponse, now time.Time) {
	g.mu.Lock()
	g.calls++
	g.lastAction, g.lastReason, g.lastWaitMS = response.Action, response.Reason, response.RetryAfterMS
	g.lastResetAt, g.lastChecked = response.ResetAt, now
	g.mu.Unlock()
}

// Reservation IDs are host execution IDs, not client supplied session names.
// Canceled lifecycles release their reservation through the normal terminal hook.
func (g *continuityGate) pace(id string, interval, poll, maxWait time.Duration, now time.Time) time.Duration {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 512 || interval < time.Second {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reservations == nil {
		g.reservations = make(map[string]time.Time)
	}
	if g.scopes == nil {
		g.scopes = make(map[string]map[string]continuityCycle)
	}
	if g.intervals == nil {
		g.intervals = make(map[string]time.Duration)
	}
	for key, at := range g.reservations {
		if !g.dispatched[key] && now.After(at.Add(maxWait)) {
			delete(g.reservations, key)
			delete(g.scopes, key)
			delete(g.intervals, key)
		}
	}
	if at, ok := g.reservations[id]; ok {
		if !g.dispatched[id] && !continuitySameScope(g.scopes[id], g.currentScope) {
			delete(g.reservations, id)
			delete(g.scopes, id)
			delete(g.intervals, id)
			g.recomputeNextLocked(now)
		} else {
			if now.Before(at) {
				return at.Sub(now)
			}
			// Keep the permit until the terminal event so repeated host checks for
			// the same execution never consume another slot.
			return 0
		}
	}
	if len(g.reservations) >= continuityMaxReservations {
		return poll
	}
	at := now
	if len(g.currentScope) == 0 {
		// Compatibility for a caller without account scope. Production host
		// assessments always supply the verified eligible-account scope.
		at = g.nextPermit
	} else {
		for candidate := range g.currentScope {
			if next := g.lastByAuth[candidate].Add(interval); next.After(at) {
				at = next
			}
		}
		for previousID, previousAt := range g.reservations {
			if g.dispatched[previousID] || !continuityScopesOverlap(g.scopes[previousID], g.currentScope) {
				continue
			}
			if next := previousAt.Add(max(interval, g.intervals[previousID])); next.After(at) {
				at = next
			}
		}
	}
	if at.Before(now) {
		at = now
	}
	// Do not allocate a reservation beyond this request's bounded horizon.
	if at.Sub(now) > maxWait {
		return poll
	}
	g.nextPermit = at.Add(interval)
	g.lastInterval = interval
	g.reservations[id] = at
	copyScope := make(map[string]continuityCycle, len(g.currentScope))
	for authID, cycle := range g.currentScope {
		copyScope[authID] = cycle
	}
	g.scopes[id] = copyScope
	g.intervals[id] = interval
	if !at.After(now) {
		return 0
	}
	return at.Sub(now)
}

func continuityScopesOverlap(a, b map[string]continuityCycle) bool {
	for id := range a {
		if _, ok := b[id]; ok {
			return true
		}
	}
	return false
}

type continuityReadyCandidate struct {
	id         string
	snapshot   quotaSnapshot
	weeklyOnly bool
}

func continuityPacingScope(ready []continuityReadyCandidate) map[string]continuityCycle {
	scope := make(map[string]continuityCycle, len(ready))
	for _, candidate := range ready {
		if candidate.weeklyOnly {
			scope[candidate.id] = continuityCycle{index: candidate.snapshot.AuthIndex, weeklyOnly: true}
			continue
		}
		for _, w := range candidate.snapshot.Windows {
			if normalizeWindowClass(w.Class) == "5h" {
				scope[candidate.id] = continuityCycle{index: candidate.snapshot.AuthIndex, reset: w.ResetAt}
				break
			}
		}
	}
	return scope
}

// Called with s.mu held. All credentials are host-prequalified for this exact
// provider/model/pin, but neither the host nor this assessment grants admission.
func (s *schedulerRuntimeState) continuityClassifyLocked(candidate pluginapi.SchedulerAuthCandidate, now time.Time) (string, time.Time, continuityReadyCandidate) {
	id := strings.TrimSpace(candidate.ID)
	index := candidateAuthIndex(candidate)
	if id == "" || strings.EqualFold(strings.TrimSpace(candidate.Status), "disabled") {
		return "excluded", time.Time{}, continuityReadyCandidate{}
	}
	if _, active := s.adqProviderCircuitActiveLocked(id, index, now); active {
		return "excluded", time.Time{}, continuityReadyCandidate{}
	}
	entry, banned := banStore.lookup(id)
	if banned {
		entry = normalizeBanEntry(entry)
		if entry.Kind != banKindQuota ||
			!strings.Contains(strings.ToLower(entry.Window), "5h") || strings.Contains(strings.ToLower(entry.Window), "both") {
			return "excluded", time.Time{}, continuityReadyCandidate{}
		}
		if entry.Phase == banPhaseHalfOpen {
			return "five_hour_recheck", entry.ResetAt, continuityReadyCandidate{}
		}
	}
	q, found := s.lookupQuotaLocked(id, index)
	if !found || q.AuthID != id || q.AuthIndex == "" || (index != "" && q.AuthIndex != index) || s.identities[q.AuthIndex] != id {
		return "unknown", time.Time{}, continuityReadyCandidate{}
	}
	q = s.serialConservativeQuotaLocked(q, now)
	var five *quotaWindow
	otherHard := false
	otherUncertain := false
	for i := range q.Windows {
		w := &q.Windows[i]
		if normalizeWindowClass(w.Class) == "5h" {
			if five == nil || w.UsedPercent > five.UsedPercent {
				five = w
			}
		} else {
			if w.ObservedAt.IsZero() || w.ObservedAt.After(now) || normalizeWindowClass(w.Class) == "" || normalizeWindowClass(w.Class) == "unknown" {
				return "unknown", time.Time{}, continuityReadyCandidate{}
			}
			if now.Sub(w.ObservedAt) > s.cfg.StaleAfter || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
				otherUncertain = true
			} else if w.UsedPercent >= usedPercentThreshold {
				otherHard = true
			}
		}
	}
	if otherHard {
		return "excluded", time.Time{}, continuityReadyCandidate{}
	}
	for _, w := range q.Windows {
		if normalizeWindowClass(w.Class) == "5h" || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
			continue
		}
		// Native top-level rejection flags are copied to both windows. A flag
		// present only on a different window is an independent hard block.
		if (!w.Allowed && (five == nil || five.Allowed)) || (w.LimitReached && (five == nil || !five.LimitReached)) {
			return "excluded", time.Time{}, continuityReadyCandidate{}
		}
	}
	// The native response's top-level allowed/limit flags can apply to both
	// windows. Numeric 5h exhaustion or a classified 5h 429 identifies the
	// particular constraint without treating a weekly-only account as limited.
	if five != nil && !five.ResetAt.IsZero() && !five.ObservedAt.IsZero() &&
		(five.UsedPercent >= usedPercentThreshold || (banned && entry.Kind == banKindQuota && !entry.BannedAt.Before(five.ObservedAt))) {
		if five.ObservedAt.After(now) || !five.ResetAt.After(five.ObservedAt) {
			return "unknown", time.Time{}, continuityReadyCandidate{}
		}
		poll := s.quotaPolls[id]
		if otherUncertain || now.Sub(five.ObservedAt) > s.cfg.StaleAfter || !now.Before(five.ResetAt) || poll.Error != "" {
			// Historical exhaustion justifies a bounded recheck, not a claim
			// that the current quota is known or that a reset has refilled it.
			return "five_hour_recheck", five.ResetAt, continuityReadyCandidate{}
		}
		return "five_hour", five.ResetAt, continuityReadyCandidate{}
	}
	// CPA's independent cooldown may begin slightly after the plugin's 429
	// timestamp. Keep clients in the host wait loop briefly after our deadline
	// rather than release them into a still-cooling native selector.
	if banned && (banEntryDisposition(entry, now) != banDispositionProbeReady || now.Before(entry.ResetAt.Add(2*time.Second))) {
		return "five_hour_recheck", entry.ResetAt, continuityReadyCandidate{}
	}
	native, nativeOK := s.quotaNative[id]
	poll := s.quotaPolls[id]
	if !nativeOK || native.AuthID != id || native.AuthIndex != q.AuthIndex || poll.Error != "" ||
		!quotaSnapshotFresh(native, now, s.cfg.StaleAfter) || !quotaSnapshotFresh(q, now, s.cfg.StaleAfter) {
		return "unknown", time.Time{}, continuityReadyCandidate{}
	}
	// An elapsed reset by itself is not a refill. Require the original native
	// observation to contain active windows from the current cycle.
	for _, w := range q.Windows {
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			return "unknown", time.Time{}, continuityReadyCandidate{}
		}
		if !w.Allowed || w.LimitReached || w.UsedPercent >= usedPercentThreshold {
			return "excluded", time.Time{}, continuityReadyCandidate{}
		}
	}
	class := poolFiveHourLimitStatus(native, q, now, s.cfg.StaleAfter)
	if class == "unknown" {
		return "unknown", time.Time{}, continuityReadyCandidate{}
	}
	if class == "not_reported" {
		weekly := false
		for _, w := range native.Windows {
			weekly = weekly || normalizeWindowClass(w.Class) == "weekly"
		}
		if !weekly {
			return "unknown", time.Time{}, continuityReadyCandidate{}
		}
	}
	return "ready", time.Time{}, continuityReadyCandidate{id: id, snapshot: q, weeklyOnly: class == "not_reported"}
}

// Estimate a sustainable request spacing only from a fresh measured 5h burn
// rate and actual recent completions. Missing/insufficient measurements produce
// no throttle, never an invented capacity or promise of future quota.
func (s *schedulerRuntimeState) continuityPacingIntervalLocked(ready []continuityReadyCandidate, now time.Time) (time.Duration, []string) {
	if len(ready) == 0 {
		return 0, nil
	}
	g := &s.continuity
	g.mu.Lock()
	defer g.mu.Unlock()
	target, observed := 0.0, 0.0
	var recheck []string
	for _, candidate := range ready {
		if candidate.weeklyOnly {
			return 0, nil
		}
		if g.usageBindings[candidate.id] != candidate.snapshot.AuthIndex {
			return 0, nil
		}
		var assessment *quotaRunwayWindowAssessment
		for _, item := range s.quotaRunway.Assess(candidate.snapshot, s.cfg, now) {
			if item.Window == "5h" {
				copy := item
				assessment = &copy
				break
			}
		}
		if assessment == nil || !assessment.Fresh || !assessment.RateKnown || assessment.HardLimited ||
			assessment.ResetAt.IsZero() || !now.Before(assessment.ResetAt) || assessment.BurnPercentPerHour <= 0 {
			return 0, nil
		}
		values := g.usage[candidate.id]
		first := 0
		for first < len(values) && (values[first].After(now) || now.Sub(values[first]) > continuityUsageHorizon) {
			first++
		}
		values = values[first:]
		if len(values) < 3 {
			return 0, nil
		}
		span := now.Sub(values[0])
		if span < 30*time.Second {
			return 0, nil
		}
		perHour := float64(len(values)) / span.Hours()
		perRequest := assessment.BurnPercentPerHour / perHour
		headroom := math.Max(0, assessment.RemainingPercent-assessment.CacheDebitPercent)
		if perRequest <= 0 {
			return 0, nil
		}
		observed += perHour / 3600
		if headroom <= 0 {
			recheck = append(recheck, candidate.id)
			continue
		}
		target += (headroom / perRequest) / assessment.ResetAt.Sub(now).Seconds()
	}
	if target <= 0 && len(recheck) > 0 {
		return 0, recheck
	}
	if target <= 0 || observed <= target || math.IsNaN(target) || math.IsInf(target, 0) {
		return 0, recheck
	}
	seconds := math.Min(1/target, s.cfg.ContinuityMaxWait.Seconds())
	if seconds < 1 {
		return 0, recheck
	}
	return time.Duration(seconds * float64(time.Second)), recheck
}

func (s *schedulerRuntimeState) assessContinuity(req pluginapi.SchedulerContinuityRequest, now time.Time) pluginapi.SchedulerContinuityResponse {
	if req.PreviousAttemptRejected429 {
		s.releaseRejectedContinuityAttempt(req.RequestID)
	}
	s.mu.Lock()
	cfg := s.cfg
	result := pluginapi.SchedulerContinuityResponse{Action: "unavailable", Reason: "quota_unknown", MaxWaitMS: cfg.ContinuityMaxWait.Milliseconds()}
	var demand []string
	if !cfg.Enabled || !cfg.ContinuityEnabled || s.detached {
		result.Action, result.Reason = "disabled", "disabled"
		s.continuity.clearPacing(now)
	} else if !schedulerRequestGenerationDisabled(req.Pick) && len(req.Pick.Candidates) > 0 && codexOnlySchedulerRequest(req.Pick) {
		unknown, waiting, recheck := false, false, false
		var earliest time.Time
		ready := make([]continuityReadyCandidate, 0, len(req.Pick.Candidates))
		pin := serialPinnedAuthID(req.Pick)
		for _, candidate := range req.Pick.Candidates {
			if pin != "" && pin != candidate.ID {
				continue
			}
			class, reset, item := s.continuityClassifyLocked(candidate, now)
			switch class {
			case "ready":
				allowed, guarded := s.recovery.reserve(item.id, item.snapshot.AuthIndex, req.RequestID, now)
				if allowed {
					ready = append(ready, item)
				} else if guarded {
					waiting, recheck = true, true
					demand = append(demand, candidate.ID)
				}
			case "unknown":
				unknown = true
				if q, ok := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate)); ok &&
					q.AuthID == candidate.ID && q.AuthIndex != "" && s.identities[q.AuthIndex] == q.AuthID {
					// A restored cache is not a complete native observation. Ask for
					// a real one without shortening the credential's existing backoff.
					demand = append(demand, candidate.ID)
				}
			case "five_hour", "five_hour_recheck":
				waiting = true
				if class == "five_hour_recheck" {
					recheck = true
					demand = append(demand, candidate.ID)
				}
				if earliest.IsZero() || reset.Before(earliest) {
					earliest = reset
				}
				if class == "five_hour" && !now.Before(reset.Add(2*time.Second)) {
					demand = append(demand, candidate.ID)
				}
			}
		}
		if len(ready) > 0 {
			result.Action, result.Reason = "ready", "available"
			for _, item := range ready {
				if item.weeklyOnly {
					result.Reason = "weekly_only_available"
					break
				}
			}
			paced := false
			scope := continuityPacingScope(ready)
			if cfg.ContinuityPacing {
				s.continuity.setPacingCycle(scope, now)
			} else {
				s.continuity.clearPacing(now)
			}
			if cfg.ContinuityPacing && !unknown {
				interval, rechecks := s.continuityPacingIntervalLocked(ready, now)
				demand = append(demand, rechecks...)
				if interval == 0 && len(rechecks) == len(ready) {
					paced = true
					result.Action, result.Reason = "wait", "pacing_recheck"
					result.RetryAfterMS = cfg.ContinuityPollInterval.Milliseconds()
				}
				if interval > 0 {
					paced = true
					if wait := s.continuity.pace(req.RequestID, interval, cfg.ContinuityPollInterval, cfg.ContinuityMaxWait, now); wait > 0 {
						result.Action, result.Reason = "wait", "pacing"
						result.RetryAfterMS = minDuration(wait, cfg.ContinuityPollInterval).Milliseconds()
					}
				}
			}
			if !paced {
				// Fresh data, weekly-only capacity or a configuration change may
				// eliminate the prior pressure. Wake all old reservations on their
				// next host recheck; this never changes actual upstream quota.
				s.continuity.clearPacingScope(scope, req.RequestID, now)
			}
		} else if unknown {
			result.Reason = "quota_unknown"
		} else if waiting {
			result.Action, result.Reason = "wait", "five_hour_exhausted"
			if recheck {
				result.Reason = "five_hour_recheck"
			}
			if !earliest.IsZero() {
				result.ResetAt = earliest.UTC().Format(time.RFC3339)
			}
			result.RetryAfterMS = cfg.ContinuityPollInterval.Milliseconds()
			if now.Before(earliest) && earliest.Sub(now) < cfg.ContinuityPollInterval {
				result.RetryAfterMS = max(int64(1), earliest.Sub(now).Milliseconds())
			}
		} else {
			result.Reason = "no_eligible_account"
		}
	}
	if len(demand) > 0 {
		if s.continuityDemandUntil == nil {
			s.continuityDemandUntil = make(map[string]time.Time)
		}
		demandTTL := min(10*time.Minute, 2*max(cfg.ContinuityPollInterval, cfg.RefreshInterval))
		for _, id := range demand {
			s.continuityDemandUntil[id] = now.Add(demandTTL)
		}
	}
	s.mu.Unlock()
	s.continuity.record(result, now)
	return result
}

func handleSchedulerContinuity(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerContinuityRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(schedulerRuntime.assessContinuity(req, time.Now()))
}

package main

import (
	"math"
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
const (
	serialOverdraftTTL                  = 30 * time.Minute
	serialFiveHourCycleAdvanceTolerance = 5 * time.Minute
)

// serialCandidate is the pool-aware view of one CPA auth candidate. Serial
// mode keeps one global active auth at a time, while quota safety, weekly
// balance, and persisted selection history decide the next committed auth.
type serialCandidate struct {
	Candidate         pluginapi.SchedulerAuthCandidate
	Snapshot          quotaSnapshot
	QuotaKnown        bool
	Eligible          bool
	Reason            string
	WindowClass       string
	CycleActive       bool
	DrainActive       bool
	MaxUsed           float64
	FiveHourUsed      float64
	FiveHourRemaining float64
	FiveHourResetAt   time.Time
	FiveHourKnown     bool
	WeeklyRemaining   float64
	WeeklyKnown       bool
	WeeklyProtected   bool
	LastSelectedAt    time.Time
	ResetCredits      int
}

// serialWindowDrains reports whether the window is close enough to its reset
// that the scheduler should let it run past the soft threshold instead of
// switching early. This is a local utilization policy based on observed quota
// state; it does not assume or promise any upstream overdraft or billing rule.
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
	drainDuration := time.Duration(cfg.DrainWindowHours * float64(time.Hour))
	// A fixed six-hour drain window would cover the entire 5h quota cycle and
	// permanently disable its soft switch threshold. Cap drain mode to the
	// final 10% of each known window; weekly/monthly windows still retain the
	// configured six-hour behavior, while 5h drains only in its final 30m.
	if seconds := effectiveWindowSeconds(window); seconds > 0 {
		maxDrain := time.Duration(seconds) * time.Second / 10
		if maxDrain > 0 && drainDuration > maxDrain {
			drainDuration = maxDrain
		}
	}
	return window.ResetAt.Sub(now) <= drainDuration
}

func serialWindowClass(snapshot quotaSnapshot, order []string, now time.Time) string {
	selected := "unknown"
	selectedRank := windowRankInOrder(selected, order)
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
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

// serialWindowHandoffPolicy returns the effective soft-handoff policy for one
// quota window. The 5h window can opt into a panel-selected override without
// changing the legacy global threshold behavior used by weekly/monthly or by
// existing configurations that omit the override.
func serialWindowHandoffPolicy(cfg pluginConfig, class string) (mode string, threshold float64) {
	mode = normalizeSerialHandoffMode(cfg.SerialHandoffMode)
	threshold = cfg.SerialSwitchPercent
	if threshold <= 0 || threshold > 100 {
		threshold = 98
	}
	if normalizeWindowClass(class) != "5h" {
		return mode, threshold
	}
	switch normalizeSerial5hHandoffMode(cfg.Serial5hHandoffMode) {
	case "custom_threshold":
		mode = "threshold_only"
		threshold = cfg.Serial5hSwitchPercent
		if threshold <= 0 || threshold > 100 {
			threshold = cfg.SerialSwitchPercent
			if threshold <= 0 || threshold > 100 {
				threshold = 98
			}
		}
	case "reserve_aware":
		mode = "reserve_aware"
	case "429_only":
		mode = "429_only"
	}
	return mode, threshold
}

// serialFiveHourStrictThresholdReached reports whether the operator-selected
// dedicated 5h threshold has been crossed. custom_threshold is intentionally
// strict: unlike inherited/global thresholds it is a safety handoff boundary,
// so drain and in-flight session continuation must not route another request
// to the old auth once a healthy replacement is available.
func serialFiveHourStrictThresholdReached(snapshot quotaSnapshot, cfg pluginConfig, now time.Time) bool {
	if normalizeSerial5hHandoffMode(cfg.Serial5hHandoffMode) != "custom_threshold" {
		return false
	}
	threshold := cfg.Serial5hSwitchPercent
	if threshold <= 0 || threshold > 100 {
		threshold = cfg.SerialSwitchPercent
		if threshold <= 0 || threshold > 100 {
			threshold = 98
		}
	}
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != "5h" {
			continue
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		if !window.Allowed || window.LimitReached || window.UsedPercent >= usedPercentThreshold {
			continue
		}
		if window.UsedPercent >= threshold {
			return true
		}
	}
	return false
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

	activeWindows := 0
	choice.ResetCredits = snapshot.ResetCredits
	choice.WindowClass = serialWindowClass(snapshot, cfg.WindowOrder, now)
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		activeWindows++
		remaining := math.Max(0, math.Min(100, 100-window.UsedPercent))
		switch normalizeWindowClass(window.Class) {
		case "5h":
			if !choice.FiveHourKnown || remaining < choice.FiveHourRemaining {
				choice.FiveHourKnown = true
				choice.FiveHourRemaining = remaining
				choice.FiveHourUsed = 100 - remaining
				choice.FiveHourResetAt = window.ResetAt
			}
		case "weekly":
			if !choice.WeeklyKnown || remaining < choice.WeeklyRemaining {
				choice.WeeklyKnown = true
				choice.WeeklyRemaining = remaining
			}
		}
	}
	if activeWindows == 0 {
		return choice
	}

	choice.QuotaKnown = true
	choice.Reason = "eligible"
	choice.WeeklyProtected = choice.WeeklyKnown && choice.WeeklyRemaining <= cfg.ReserveWeeklyPercent
	reason := "eligible"
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		if window.UsedPercent > choice.MaxUsed {
			choice.MaxUsed = window.UsedPercent
		}
		if quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {
			choice.CycleActive = true
		}
		draining := serialWindowDrains(window, cfg, now)
		if draining {
			choice.DrainActive = true
		}
		// Inspect every active quota window before deciding. Keeper does not
		// guarantee row order, and a soft weekly threshold must never mask a
		// later hard 5h limit (or vice versa).
		if !window.Allowed {
			reason = "not_allowed"
			continue
		}
		if window.LimitReached || window.UsedPercent >= usedPercentThreshold {
			if reason != "not_allowed" {
				reason = "limit_reached"
			}
			continue
		}
		class := normalizeWindowClass(window.Class)
		handoffMode, threshold := serialWindowHandoffPolicy(cfg, class)
		reserve := reserveForWindow(cfg, class)
		reserveReached := handoffMode == "reserve_aware" &&
			class != "" && reserve > 0 &&
			100-window.UsedPercent <= reserve
		thresholdReached := handoffMode != "429_only" && window.UsedPercent >= threshold
		strictFiveHourBoundary := class == "5h" && normalizeSerial5hHandoffMode(cfg.Serial5hHandoffMode) == "custom_threshold"
		if reason == "eligible" && (thresholdReached || reserveReached) && (!draining || strictFiveHourBoundary) {
			reason = "serial_threshold"
		}
	}
	choice.Reason = reason
	choice.Eligible = reason == "eligible"
	return choice
}

type serialSortContext struct {
	coldStart          bool
	weeklyBand         float64
	maxWeeklyRemaining [2]float64
	weeklyKnown        [2]bool
}

func newSerialSortContext(choices []serialCandidate, cfg pluginConfig) serialSortContext {
	ctx := serialSortContext{coldStart: true, weeklyBand: cfg.SwitchHysteresisPercent}
	if ctx.weeklyBand < 0 {
		ctx.weeklyBand = 0
	}
	for _, choice := range choices {
		if !choice.LastSelectedAt.IsZero() {
			ctx.coldStart = false
		}
		if !choice.WeeklyKnown {
			continue
		}
		group := 0
		if choice.WeeklyProtected {
			group = 1
		}
		if !ctx.weeklyKnown[group] || choice.WeeklyRemaining > ctx.maxWeeklyRemaining[group] {
			ctx.weeklyKnown[group] = true
			ctx.maxWeeklyRemaining[group] = choice.WeeklyRemaining
		}
	}
	return ctx
}

func serialWeeklyBalanceTier(choice serialCandidate, ctx serialSortContext) int {
	if !choice.WeeklyKnown {
		return int(^uint(0) >> 1)
	}
	group := 0
	if choice.WeeklyProtected {
		group = 1
	}
	delta := ctx.maxWeeklyRemaining[group] - choice.WeeklyRemaining
	if delta <= ctx.weeklyBand {
		return 0
	}
	if ctx.weeklyBand <= 0 {
		return int(math.Ceil(delta * 1000))
	}
	return 1 + int(math.Floor((delta-ctx.weeklyBand)/ctx.weeklyBand))
}

func serialCandidateLess(a, b serialCandidate, cfg pluginConfig, ctx serialSortContext) bool {
	// Weekly reserve is a hard pool partition. Reset credits, priority, drain,
	// or a fresh 5h window cannot spend a protected weekly account while an
	// unprotected choice exists.
	if a.WeeklyProtected != b.WeeklyProtected {
		return !a.WeeklyProtected
	}
	if a.DrainActive != b.DrainActive {
		return a.DrainActive
	}
	if rankA, rankB := windowRankInOrder(a.WindowClass, cfg.WindowOrder), windowRankInOrder(b.WindowClass, cfg.WindowOrder); rankA != rankB {
		return rankA < rankB
	}
	if cfg.PreferResetCredits && (a.ResetCredits > 0) != (b.ResetCredits > 0) {
		return a.ResetCredits > 0
	}
	// Preserve the historical cold-start choice as a pool-wide mode. Fairness
	// rules apply after
	// the first committed selection so existing CPA priorities remain stable.
	if ctx.coldStart {
		if a.Candidate.Priority != b.Candidate.Priority {
			return a.Candidate.Priority > b.Candidate.Priority
		}
		if a.MaxUsed != b.MaxUsed {
			return a.MaxUsed > b.MaxUsed
		}
	}
	// Compute a pool-relative tier before sorting instead of comparing each
	// pair against hysteresis. Pairwise bands are non-transitive and can make
	// the result depend on CPA candidate input order.
	if a.WeeklyKnown != b.WeeklyKnown {
		return a.WeeklyKnown
	}
	if tierA, tierB := serialWeeklyBalanceTier(a, ctx), serialWeeklyBalanceTier(b, ctx); tierA != tierB {
		return tierA < tierB
	}
	// Within a balanced weekly band, consume the least-used 5h window first.
	// The old fill-first ordering used MaxUsed descending, which repeatedly
	// selected the account that had already spent the most.
	if a.FiveHourKnown != b.FiveHourKnown {
		return a.FiveHourKnown
	}
	if a.FiveHourKnown && a.FiveHourUsed != b.FiveHourUsed {
		return a.FiveHourUsed < b.FiveHourUsed
	}
	if a.WeeklyKnown && b.WeeklyKnown && a.WeeklyRemaining != b.WeeklyRemaining {
		return a.WeeklyRemaining > b.WeeklyRemaining
	}
	if cfg.SerialPreferActiveCycle && a.CycleActive != b.CycleActive {
		return a.CycleActive
	}
	if a.MaxUsed != b.MaxUsed {
		return a.MaxUsed < b.MaxUsed
	}
	// Oldest selection wins ties, giving stable round-robin behavior across
	// equal snapshots without introducing request-level randomness.
	if a.LastSelectedAt.IsZero() != b.LastSelectedAt.IsZero() {
		return a.LastSelectedAt.IsZero()
	}
	if !a.LastSelectedAt.IsZero() && !b.LastSelectedAt.Equal(a.LastSelectedAt) {
		return a.LastSelectedAt.Before(b.LastSelectedAt)
	}
	if a.Candidate.Priority != b.Candidate.Priority {
		return a.Candidate.Priority > b.Candidate.Priority
	}
	return a.Candidate.ID < b.Candidate.ID
}

func sortSerialCandidates(choices []serialCandidate, cfg pluginConfig) {
	ctx := newSerialSortContext(choices, cfg)
	sort.SliceStable(choices, func(i, j int) bool {
		return serialCandidateLess(choices[i], choices[j], cfg, ctx)
	})
}

func serialPinnedAuthID(req pluginapi.SchedulerPickRequest) string {
	return extractMetadataString(req.Options.Metadata, "pinned_auth_id")
}

func (s *schedulerRuntimeState) annotateSerialCandidateLocked(choice *serialCandidate) {
	if choice == nil {
		return
	}
	if s.serialLastSelected != nil {
		choice.LastSelectedAt = s.serialLastSelected[choice.Candidate.ID]
	}
}

func (s *schedulerRuntimeState) markSerialSelectedLocked(authID string, resetAt, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	if s.serialLastSelected == nil {
		s.serialLastSelected = make(map[string]time.Time)
	}
	s.serialLastSelected[authID] = now
	if !resetAt.IsZero() {
		if s.serialFiveHourCycle == nil {
			s.serialFiveHourCycle = make(map[string]time.Time)
		}
		s.serialFiveHourCycle[authID] = resetAt
	}
}

func (s *schedulerRuntimeState) serialFiveHourCycleAdvancedLocked(choice serialCandidate, now time.Time) bool {
	if !choice.FiveHourKnown || choice.FiveHourResetAt.IsZero() || s.serialFiveHourCycle == nil {
		return false
	}
	previousResetAt := s.serialFiveHourCycle[choice.Candidate.ID]
	// Keeper can move an unstarted 5h placeholder reset forward on each
	// refresh. That drift is not a new quota cycle. Only rotate after the
	// previously committed reset boundary has actually elapsed and the new
	// anchor advances materially beyond it.
	return !previousResetAt.IsZero() && !now.Before(previousResetAt) &&
		choice.FiveHourResetAt.After(previousResetAt.Add(serialFiveHourCycleAdvanceTolerance))
}

func serialChoiceByID(choices []serialCandidate, authID string) (serialCandidate, bool) {
	for _, choice := range choices {
		if choice.Candidate.ID == authID {
			return choice, true
		}
	}
	return serialCandidate{}, false
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
		s.annotateSerialCandidateLocked(&choice)
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
	sortSerialCandidates(choices, cfg)
	return pluginapi.SchedulerPickResponse{AuthID: choices[0].Candidate.ID, Handled: true}
}

// serialOverdraftPickLocked serves one request from the session's pinned
// overdraft auth. The candidate must still be present in CPA's list. Legacy
// soft thresholds may keep a short continuation binding, but a dedicated 5h
// custom threshold is a strict safety boundary and invalidates that binding.
func (s *schedulerRuntimeState) serialOverdraftPickLocked(
	candidates []pluginapi.SchedulerAuthCandidate,
	session, overdraftAuthID string,
	blocked map[string]banDisposition,
	cfg pluginConfig,
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
		snapshot, found := s.quotas[candidate.ID]
		if found {
			choice := inspectSerialCandidate(candidate, snapshot, true, cfg, now)
			if choice.QuotaKnown && (choice.Reason == "limit_reached" || choice.Reason == "not_allowed" || serialFiveHourStrictThresholdReached(snapshot, cfg, now)) {
				delete(s.serialOverdraft, session)
				return pluginapi.SchedulerPickResponse{}, false
			}
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
		// Only read an existing binding here. A newly observed session must not
		// be pinned to a primary that is already exhausted; it is eligible for
		// the replacement account. A binding is created below only after the
		// current primary is confirmed eligible.
		sessionPinned = s.serialOverdraftAuthLocked(session, now)
	}
	if pinnedAuthID := serialPinnedAuthID(req); pinnedAuthID != "" {
		response := s.serialRequestLocalPickLocked(req.Candidates, pinnedAuthID, blocked, cfg, now)
		s.mu.Unlock()
		return response
	}
	if overdraftAuthID := s.serialOverdraftAuthLocked(session, now); overdraftAuthID != "" && overdraftAuthID != previous {
		if response, ok := s.serialOverdraftPickLocked(req.Candidates, session, overdraftAuthID, blocked, cfg, now); ok {
			s.mu.Unlock()
			return response
		}
		previous = strings.TrimSpace(s.serialActiveAuthID)
	}
	currentSeen := false
	currentEligible := false
	currentStrictFiveHourBoundary := false
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
		s.annotateSerialCandidateLocked(&choice)
		strictFiveHourBoundary := found && serialFiveHourStrictThresholdReached(snapshot, cfg, now)
		if candidate.ID == previous {
			currentSeen = true
			currentStrictFiveHourBoundary = strictFiveHourBoundary
			if choice.Eligible {
				currentEligible = true
				currentChoice = choice
				if session != "" && (sessionPinned == "" || sessionPinned == candidate.ID) {
					s.setSerialOverdraftLocked(session, candidate.ID, now)
					sessionPinned = candidate.ID
				}
				continue
			}
			if session != "" && sessionPinned == candidate.ID &&
				choice.Reason == "serial_threshold" && !strictFiveHourBoundary && overdraftOverride == "" {
				// Legacy soft-threshold continuation: preserve this exact
				// conversation on the previous auth while the global primary
				// moves. A dedicated 5h custom threshold is excluded because it
				// is an operator-selected strict safety boundary.
				overdraftOverride = candidate.ID
				s.setSerialOverdraftLocked(session, candidate.ID, now)
			} else if session != "" && sessionPinned == candidate.ID &&
				(choice.Reason == "limit_reached" || choice.Reason == "not_allowed" || strictFiveHourBoundary) {
				delete(s.serialOverdraft, session)
				sessionPinned = ""
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
		// A drain-window account is intentionally allowed to consume its
		// expiring quota for legacy/inherited policies. Dedicated 5h custom
		// thresholds never reach this branch after crossing their boundary.
		if currentChoice.DrainActive {
			s.mu.Unlock()
			return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
		}
		cycleBaselineMissing := currentChoice.FiveHourKnown && !currentChoice.FiveHourResetAt.IsZero() &&
			(s.serialFiveHourCycle == nil || s.serialFiveHourCycle[previous].IsZero())
		cycleObservedAdvanced := s.serialFiveHourCycleAdvancedLocked(currentChoice, now)
		cycleAdvanced := normalizeSerialSelectionSource(s.serialSelectionSource) != "manual" && cycleObservedAdvanced
		// Serial mode remains committed to one account at a time, and window_order
		// is a strict pool priority. When a higher-priority class becomes usable
		// again (for example, weekly resets while monthly is active), preempt the
		// lower-priority auth exactly once. Unknown/stale quota snapshots never
		// trigger a preemption.
		sortSerialCandidates(choices, cfg)
		currentRank := windowRankInOrder(currentChoice.WindowClass, cfg.WindowOrder)
		cycleRotationEligible := cycleAdvanced && len(choices) > 0 &&
			windowRankInOrder(choices[0].WindowClass, cfg.WindowOrder) <= currentRank &&
			(!choices[0].WeeklyProtected || currentChoice.WeeklyProtected)
		if currentChoice.QuotaKnown && len(choices) > 0 && choices[0].QuotaKnown &&
			((windowRankInOrder(choices[0].WindowClass, cfg.WindowOrder) < currentRank) ||
				(currentChoice.WeeklyProtected && !choices[0].WeeklyProtected) || cycleRotationEligible) {
			selected := choices[0].Candidate.ID
			s.serialActiveAuthID = selected
			s.serialSelectionSource = "auto"
			s.serialSelectedAt = now
			s.markSerialSelectedLocked(selected, choices[0].FiveHourResetAt, now)
			s.serialSwitches++
			s.serialLastSwitchAt = now
			reason := "higher_priority_window_available"
			if cycleRotationEligible {
				reason = "five_hour_cycle_rotation"
			} else if currentChoice.WeeklyProtected && !choices[0].WeeklyProtected {
				reason = "weekly_reserve"
			}
			s.serialLastSwitchReason = reason
			s.mu.Unlock()
			s.persistBanState()
			return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
		}
		if cycleBaselineMissing || cycleObservedAdvanced {
			if s.serialFiveHourCycle == nil {
				s.serialFiveHourCycle = make(map[string]time.Time)
			}
			s.serialFiveHourCycle[previous] = currentChoice.FiveHourResetAt
			s.mu.Unlock()
			s.persistBanState()
			return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
		}
		s.mu.Unlock()
		return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
	}

	thresholdFallback := false
	if len(choices) == 0 && previous != "" && currentSeen &&
		(currentReason == "limit_reached" || currentReason == "not_allowed" || currentReason == "quarantined") &&
		len(thresholdChoices) > 0 {
		// A hard-exhausted primary must never delegate to CPA merely because
		// every backup is at the soft threshold. Use the least-used backup and
		// let overdraft affect only the already-pinned session.
		choices = thresholdChoices
		thresholdFallback = true
	}
	if len(choices) == 0 && previous != "" && currentSeen && currentReason == "serial_threshold" {
		if currentStrictFiveHourBoundary && len(thresholdChoices) > 0 {
			// A dedicated 5h threshold is strict for the current auth. If every
			// replacement has also crossed a soft threshold, move to the best
			// replacement instead of knowingly keeping the boundary-crossed
			// current auth. This is still bounded by actual pool availability.
			choices = thresholdChoices
			thresholdFallback = true
		} else {
			// Legacy/global soft thresholds remain a preference: when no safer
			// replacement exists, keep one committed account until an
			// authoritative hard failure requires failover.
			s.mu.Unlock()
			return pluginapi.SchedulerPickResponse{AuthID: previous, Handled: true}
		}
	}
	if len(choices) == 0 && previous == "" && len(thresholdChoices) > 0 {
		choices = thresholdChoices
		thresholdFallback = true
	}
	if previous != "" && !currentSeen {
		if len(choices) == 0 {
			choices = thresholdChoices
		}
		sortSerialCandidates(choices, cfg)
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
		if selectedChoice, ok := serialChoiceByID(choices, selected); ok {
			s.markSerialSelectedLocked(selected, selectedChoice.FiveHourResetAt, now)
		} else {
			s.markSerialSelectedLocked(selected, time.Time{}, now)
		}
		s.serialSwitches++
		s.serialLastSwitchAt = now
		s.serialLastSwitchReason = "candidate_unavailable_confirmed"
		s.resetSerialMissingLocked()
		s.mu.Unlock()
		s.persistBanState()
		return pluginapi.SchedulerPickResponse{AuthID: selected, Handled: true}
	}

	sortSerialCandidates(choices, cfg)
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
	s.markSerialSelectedLocked(selected, choices[0].FiveHourResetAt, now)
	if session != "" && overdraftOverride == "" {
		s.setSerialOverdraftLocked(session, selected, now)
	}
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

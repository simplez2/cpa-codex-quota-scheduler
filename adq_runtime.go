package main

import (
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// adqProviderCircuit is a local, bounded circuit for transient provider
// failures. It is deliberately separate from quota bans: 503/522 and provider
// overload do not spend or invalidate quota.
type adqProviderCircuit struct {
	State      adqProviderState
	RetryAt    time.Time
	Failures   int
	LastAt     time.Time
	LastStatus int
}

// candidateAuthIndex extracts the stable CPA auth_index when the host exposes
// it as routing metadata. The scheduler ABI keeps this field optional, so all
// lookups also work when only the auth record ID is available.
func candidateAuthIndex(candidate pluginapi.SchedulerAuthCandidate) string {
	for key, value := range candidate.Attributes {
		if strings.EqualFold(strings.TrimSpace(key), "auth_index") {
			return strings.TrimSpace(value)
		}
	}
	for key, value := range candidate.Metadata {
		if strings.EqualFold(strings.TrimSpace(key), "auth_index") {
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

// canonicalAuthIDLocked resolves the ID used by CPA's scheduler to the ID
// stored in quota, pacing, reservation and circuit state. Quota snapshots are
// indexed by both auth ID and auth_index, but tests and older CPA builds can
// expose those values in different fields. Keep the resolver deterministic and
// never synthesize an ID when there is no mapping.
func (s *schedulerRuntimeState) canonicalAuthIDLocked(authID, authIndex string) string {
	authID = strings.TrimSpace(authID)
	authIndex = strings.TrimSpace(authIndex)
	// Exact auth IDs win over alias maps. This avoids mis-resolving a valid ID
	// when an older CPA build happens to reuse that string as another index.
	if authID != "" {
		if snapshot, ok := s.quotas[authID]; ok && strings.TrimSpace(snapshot.AuthID) != "" {
			return strings.TrimSpace(snapshot.AuthID)
		}
	}
	if authIndex != "" {
		if snapshot, ok := s.quotas[authIndex]; ok && strings.TrimSpace(snapshot.AuthID) != "" {
			return strings.TrimSpace(snapshot.AuthID)
		}
	}
	if authID != "" {
		if mapped := strings.TrimSpace(s.identities[authID]); mapped != "" {
			return mapped
		}
	}
	if authIndex != "" {
		if mapped := strings.TrimSpace(s.identities[authIndex]); mapped != "" {
			return mapped
		}
	}
	// A manually seeded state may contain only a snapshot value under another
	// alias. The scan is small (one entry per CPA account) and only runs on the
	// routing/control path, never on token or request bodies.
	for _, snapshot := range s.quotas {
		canonical := strings.TrimSpace(snapshot.AuthID)
		if canonical == "" {
			continue
		}
		if (authID != "" && (canonical == authID || strings.TrimSpace(snapshot.AuthIndex) == authID)) ||
			(authIndex != "" && strings.TrimSpace(snapshot.AuthIndex) == authIndex) {
			return canonical
		}
	}
	return authID
}

// adqProviderCircuitActiveLocked returns the active transient circuit for an
// auth ID or one of its CPA aliases. Expired overload/auth circuits are
// removed here so the next request becomes the single recovery probe.
func (s *schedulerRuntimeState) adqProviderCircuitActiveLocked(authID, authIndex string, now time.Time) (adqProviderCircuit, bool) {
	keys := []string{strings.TrimSpace(authID), strings.TrimSpace(authIndex), s.canonicalAuthIDLocked(authID, authIndex)}
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		circuit := s.adqCircuitLocked(key, now)
		if circuit.State == adqProviderOverload || circuit.State == adqProviderAuth {
			if circuit.RetryAt.IsZero() || now.Before(circuit.RetryAt) {
				return circuit, true
			}
		}
	}
	return adqProviderCircuit{State: adqProviderHealthy}, false
}

func (s *schedulerRuntimeState) adqProviderCircuitActive(authID, authIndex string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, active := s.adqProviderCircuitActiveLocked(authID, authIndex, now)
	return active
}

// filterADQProviderCircuits is shared by serial, balanced, pacing and legacy
// paths. A provider overload/auth circuit is a short local failover signal; it
// must not be converted into a quota ban or sent back to CPA for another retry.
func (s *schedulerRuntimeState) filterADQProviderCircuits(candidates []pluginapi.SchedulerAuthCandidate, now time.Time) []pluginapi.SchedulerAuthCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, active := s.adqProviderCircuitActiveLocked(candidate.ID, candidateAuthIndex(candidate), now); active {
			continue
		}
		filtered = append(filtered, candidate)
	}
	return filtered
}

func (s *schedulerRuntimeState) adqPolicyLocked() adqPolicy {
	policy := s.adqPolicy
	if policy.FiveHourWindow <= 0 || policy.WeeklyWindow <= 0 {
		policy = defaultADQPolicy()
	}
	policy.FiveHourSafetyMargin = 0
	if !s.cfg.SerialSoftContinuation {
		if mode, _ := serialWindowHandoffPolicy(s.cfg, "5h"); mode == "reserve_aware" {
			policy.FiveHourSafetyMargin = s.cfg.Reserve5hPercent / 100
		}
		mode, threshold := serialWindowHandoffPolicy(s.cfg, "weekly")
		if mode != "429_only" {
			reserve := 100 - threshold
			if mode == "reserve_aware" {
				reserve = max(reserve, s.cfg.ReserveWeeklyPercent)
			}
			policy.WeeklySafetyMargin = max(policy.WeeklySafetyMargin, reserve/100)
		}
	}
	policy.SettlementTimeout = adqSettlementTTL(s.cfg.QuotaRefreshCooldown)
	return policy
}

func adqCircuitBackoff(base time.Duration, failures int) time.Duration {
	if base <= 0 {
		base = 2 * time.Minute
	}
	if failures < 1 {
		failures = 1
	}
	if failures > 6 {
		failures = 6
	}
	backoff := base
	for i := 1; i < failures; i++ {
		if backoff >= 15*time.Minute {
			return 15 * time.Minute
		}
		backoff *= 2
	}
	if backoff > 15*time.Minute {
		backoff = 15 * time.Minute
	}
	return backoff
}

func (s *schedulerRuntimeState) observeADQProviderOutcome(record pluginapi.UsageRecord, now time.Time) {
	if !strings.EqualFold(strings.TrimSpace(record.Provider), providerCodex) {
		return
	}
	state := classifyADQProviderError(record.Failure.StatusCode, "", record.Failure.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	authID := s.canonicalAuthIDLocked(record.AuthID, record.AuthIndex)
	if authID == "" {
		return
	}
	if s.adqProviderCircuits == nil {
		s.adqProviderCircuits = make(map[string]adqProviderCircuit)
	}
	if !record.Failed {
		if circuit, ok := s.adqProviderCircuits[authID]; ok && circuit.State != adqProviderQuota {
			delete(s.adqProviderCircuits, authID)
		}
		return
	}
	switch state {
	case adqProviderOverload:
		circuit := s.adqProviderCircuits[authID]
		circuit.State = adqProviderOverload
		circuit.Failures++
		circuit.LastAt = now
		circuit.LastStatus = record.Failure.StatusCode
		circuit.RetryAt = now.Add(adqCircuitBackoff(s.cfg.HalfOpenRetryAfter, circuit.Failures))
		s.adqProviderCircuits[authID] = circuit
	case adqProviderAuth:
		// Authentication failures need a slower retry, but must not be turned
		// into a quota exhaustion event.
		s.adqProviderCircuits[authID] = adqProviderCircuit{
			State: adqProviderAuth, Failures: 1, LastAt: now,
			LastStatus: record.Failure.StatusCode, RetryAt: now.Add(30 * time.Minute),
		}
	case adqProviderQuota:
		// 429/quota events are owned by banStore and the native quota refresh.
		// Remove any transient circuit so a later reset observation can reopen it.
		delete(s.adqProviderCircuits, authID)
	}
}

func (s *schedulerRuntimeState) adqCircuitLocked(authID string, now time.Time) adqProviderCircuit {
	key := strings.TrimSpace(authID)
	circuit := s.adqProviderCircuits[key]
	if (circuit.State == adqProviderOverload || circuit.State == adqProviderAuth) && !circuit.RetryAt.IsZero() && !now.Before(circuit.RetryAt) {
		delete(s.adqProviderCircuits, key)
		return adqProviderCircuit{State: adqProviderHealthy}
	}
	return circuit
}

func adqCapacityEstimateForClass(state *accountPacingState, class string) (float64, bool) {
	if state == nil {
		return 0, false
	}
	estimate := state.Capacities[normalizeWindowClass(class)]
	if estimate.Credits <= 0 || !finiteADQ(estimate.Credits) || estimate.Samples < 1 {
		return 0, false
	}
	return estimate.Credits, true
}

func adqCapacityFromSnapshot(snapshot quotaSnapshot, class string) (float64, bool) {
	class = normalizeWindowClass(class)
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != class || !window.WindowUsageCreditsKnown || window.WindowUsageCredits <= 0 || window.UsedPercent <= 0 {
			continue
		}
		capacity := window.WindowUsageCredits * 100 / window.UsedPercent
		if finiteADQ(capacity) && capacity > 0 {
			return capacity, true
		}
	}
	return 0, false
}

func adqResolveCapacities(weekly float64, weeklyKnown bool, fiveHour float64, fiveHourKnown bool, planWeight, ratio float64) (resolvedWeekly, resolvedFiveHour float64, source string, fullyObserved bool) {
	if ratio <= 0 || !finiteADQ(ratio) {
		ratio = defaultADQPolicy().MaxQuotaRatio5h
	}
	if planWeight <= 0 || !finiteADQ(planWeight) {
		planWeight = 1
	}
	switch {
	case weeklyKnown && fiveHourKnown:
		return weekly, fiveHour, "observed", true
	case weeklyKnown:
		return weekly, weekly * ratio, "observed_weekly", false
	case fiveHourKnown:
		return fiveHour / ratio, fiveHour, "observed_5h", false
	default:
		resolvedWeekly = 100 * planWeight
		return resolvedWeekly, resolvedWeekly * ratio, "plan_prior", false
	}
}

func adqRunwayBurnForClass(assessments []quotaRunwayWindowAssessment, class string, capacity float64) float64 {
	class = normalizeWindowClass(class)
	for _, item := range assessments {
		if normalizeWindowClass(item.Window) != class || !item.RateKnown || item.BurnPercentPerHour <= 0 {
			continue
		}
		burn := item.BurnPercentPerHour * capacity / 100
		if finiteADQ(burn) && burn > 0 {
			return burn
		}
	}
	return 0
}

func adqPendingWithoutReservation(account *balancedAccount, reservations *adqReservationBook, now time.Time) float64 {
	if account == nil {
		return 0
	}
	total := 0.0
	for _, pending := range account.Pending {
		reservationID := strings.TrimSpace(pending.ReservationID)
		covered := reservationID != "" && reservations != nil && reservations.CoversPending(reservationID, now)
		if !covered && pending.Cost > 0 && finiteADQ(pending.Cost) {
			total += pending.Cost
		}
	}
	return total
}

func (s *schedulerRuntimeState) adqInputForChoiceLocked(choice serialCandidate, now time.Time) (adqAccountInput, bool) {
	rawID := strings.TrimSpace(choice.Candidate.ID)
	weeklyOnly := !choice.FiveHourKnown && poolFiveHourLimitStatus(quotaSnapshot{}, choice.Snapshot, now, s.cfg.StaleAfter) == "not_reported"
	if rawID == "" || !choice.QuotaKnown || !choice.WeeklyKnown || (!choice.FiveHourKnown && !weeklyOnly) {
		return adqAccountInput{}, false
	}
	snapshot := choice.Snapshot
	id := s.canonicalAuthIDLocked(rawID, snapshot.AuthIndex)
	if id == "" {
		id = rawID
	}
	state := s.pacingAccounts[id]
	if state == nil && rawID != id {
		state = s.pacingAccounts[rawID]
	}
	if state == nil && strings.TrimSpace(snapshot.AuthIndex) != "" {
		state = s.pacingAccounts[strings.TrimSpace(snapshot.AuthIndex)]
	}
	weeklyCapacity, weeklyKnown := adqCapacityEstimateForClass(state, "weekly")
	fiveCapacity, fiveKnown := adqCapacityEstimateForClass(state, "5h")
	if !weeklyKnown {
		weeklyCapacity, weeklyKnown = adqCapacityFromSnapshot(snapshot, "weekly")
	}
	if !fiveKnown {
		fiveCapacity, fiveKnown = adqCapacityFromSnapshot(snapshot, "5h")
	}
	if weeklyOnly {
		// An upgrade invalidates the former plan's 5h capacity calibration.
		fiveCapacity, fiveKnown = 0, false
	}
	plan, planWeight, _ := resolvedQuotaPlan(s.cfg, id, snapshot, now)
	policy := s.adqPolicyLocked()
	weeklyCapacity, fiveCapacity, capacitySource, fullyObserved := adqResolveCapacities(
		weeklyCapacity, weeklyKnown, fiveCapacity, fiveKnown, planWeight, policy.MaxQuotaRatio5h,
	)
	if weeklyOnly {
		fiveCapacity, fullyObserved = 0, weeklyKnown
	}
	reservationsFive, reservationsWeek := 0.0, 0.0
	settlingFive, settlingWeek := 0.0, 0.0
	if s.adqReservations != nil {
		reservationsFive, reservationsWeek = s.adqReservations.Reserved(id, now)
		settlingFive, settlingWeek = s.adqReservations.Settling(id, now)
	}
	account := s.balancedAccounts[id]
	if account == nil && rawID != id {
		account = s.balancedAccounts[rawID]
	}
	pending := adqPendingWithoutReservation(account, s.adqReservations, now)
	assessments := s.quotaRunway.Assess(snapshot, s.cfg, now)
	burnClass, burnCapacity := "5h", fiveCapacity
	if weeklyOnly {
		burnClass, burnCapacity = "weekly", weeklyCapacity
	}
	burnMean, burnP90, burnP95, burnKnown := s.rotationBurnLocked(snapshot, burnClass, burnCapacity, now)
	if !burnKnown {
		burnMean = adqRunwayBurnForClass(assessments, burnClass, burnCapacity)
		burnP90, burnP95 = burnMean, burnMean
	}
	circuit := s.adqCircuitLocked(id, now)
	if circuit.State == "" && rawID != id {
		circuit = s.adqCircuitLocked(rawID, now)
	}
	// CPA has already filtered these candidates for this model's availability.
	// Its historical error status can survive cooldown recovery or an error on
	// another model. Treating it as an active outage makes every ADQ choice fail
	// and silently delegates threshold/weight decisions back to native routing.
	status := strings.ToLower(strings.TrimSpace(choice.Candidate.Status))
	healthy := status == "" || status == "active" || status == "error"
	if choice.Reason == "not_allowed" || choice.Reason == "limit_reached" {
		healthy = false
	}
	if reservedHandoffClass(choice, s.cfg, now) != "" && !s.cfg.SerialSoftContinuation {
		healthy = false
	}
	return adqAccountInput{
		ID:                    id,
		AuthIndex:             snapshot.AuthIndex,
		Plan:                  plan,
		WeeklyCapacity:        weeklyCapacity,
		WeeklyRemaining:       weeklyCapacity * clampADQ(choice.WeeklyRemaining, 0, 100) / 100,
		FiveHourCapacity:      fiveCapacity,
		FiveHourRemaining:     fiveCapacity * clampADQ(choice.FiveHourRemaining, 0, 100) / 100,
		WeeklyResetAt:         adqResetForClass(snapshot, "weekly"),
		FiveHourResetAt:       choice.FiveHourResetAt,
		BurnMean:              burnMean,
		BurnP90:               burnP90,
		BurnP95:               burnP95,
		ReservationFiveHour:   reservationsFive,
		ReservationWeekly:     reservationsWeek,
		SettlingFiveHour:      settlingFive,
		SettlingWeekly:        settlingWeek,
		PendingFiveHour:       pending,
		PendingWeekly:         pending,
		ProviderState:         circuit.State,
		ProviderRetryAt:       circuit.RetryAt,
		Healthy:               healthy,
		Sticky:                false,
		AbsoluteCapacityKnown: fullyObserved,
		CapacitySource:        capacitySource,
		PhaseAnchor:           s.fiveHourPhases[id].ActiveResetAt,
		PhaseAnchorMode:       s.fiveHourPhases[id].AnchorMode,
		WeeklyOnly:            weeklyOnly,
	}, true
}

func adqResetForClass(snapshot quotaSnapshot, class string) time.Time {
	class = normalizeWindowClass(class)
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) == class {
			return window.ResetAt
		}
	}
	return time.Time{}
}

func (s *schedulerRuntimeState) adqFilterChoicesLocked(choices []serialCandidate, excluded map[string]bool) []serialCandidate {
	filtered := make([]serialCandidate, 0, len(choices))
	for _, choice := range choices {
		rawID := strings.TrimSpace(choice.Candidate.ID)
		canonical := s.canonicalAuthIDLocked(rawID, choice.Snapshot.AuthIndex)
		if canonical == "" {
			canonical = rawID
		}
		if !excluded[rawID] && !excluded[canonical] {
			filtered = append(filtered, choice)
		}
	}
	return filtered
}

func (s *schedulerRuntimeState) adqRouteChoicesLocked(req pluginapi.SchedulerPickRequest, choices []serialCandidate, cost float64, now time.Time, sessionKey, stickyAuthID string) (string, adqReservation, bool) {
	id, reservation, ok, _ := s.adqRouteChoicesWithAllocationLocked(req, choices, cost, now, sessionKey, stickyAuthID, adqAllocateNative)
	return id, reservation, ok
}

type adqAllocationMode uint8

const (
	adqAllocateNative adqAllocationMode = iota
	adqAllocateBalanced
	adqAllocateWeeklyDeadline
	adqAllocateOpportunity
)

// handled distinguishes unavailable telemetry (legacy fallback) from an ADQ
// admission failure. A hard rejection must never fall through unreserved.
func (s *schedulerRuntimeState) adqRouteChoicesWithAllocationLocked(req pluginapi.SchedulerPickRequest, choices []serialCandidate, cost float64, now time.Time, sessionKey, stickyAuthID string, mode adqAllocationMode) (string, adqReservation, bool, bool) {
	if len(choices) == 0 || s.adqReservations == nil {
		return "", adqReservation{}, false, false
	}
	policy := s.adqPolicyLocked()
	balanced := mode != adqAllocateNative
	excluded := make(map[string]bool)
	for len(excluded) < len(choices) {
		available := s.adqFilterChoicesLocked(choices, excluded)
		if len(available) == 0 {
			return "", adqReservation{}, false, true
		}
		inputs := make([]adqAccountInput, 0, len(available))
		for _, choice := range available {
			input, ok := s.adqInputForChoiceLocked(choice, now)
			if !ok {
				return "", adqReservation{}, false, false
			}
			input.Sticky = false
			inputs = append(inputs, input)
		}
		if mode == adqAllocateWeeklyDeadline {
			available, inputs = weeklyDeadlineGroup(available, inputs, cost, s.cfg, policy, now)
			if len(inputs) == 0 {
				return "", adqReservation{}, false, true
			}
		}
		// Session affinity is a hard preference whenever the sticky candidate is
		// present in this tier; ADQ's own hard weekly/5h gate still applies.
		session := strings.TrimSpace(sessionKey)
		if session == "" {
			session = schedulerSessionHash(req)
		}
		stickyID := strings.TrimSpace(stickyAuthID)
		// Balanced selection has already checked the binding's credential index
		// and decided whether priority requires yielding. Do not restore it here.
		if stickyID == "" && session != "" && !balanced {
			if binding, ok := s.balancedSessions[session]; ok {
				stickyID = strings.TrimSpace(binding.AuthID)
			}
		}
		stickyCanonical := s.canonicalAuthIDLocked(stickyID, "")
		if stickyID != "" {
			for i := range inputs {
				if inputs[i].ID == stickyID || inputs[i].ID == stickyCanonical {
					inputs[i].Sticky = true
				}
			}
		}
		for _, input := range inputs {
			fiveLimit := maxADQ(0, input.FiveHourRemaining-input.PendingFiveHour-input.FiveHourCapacity*adqFiveHourSafety(policy))
			weekLimit := maxADQ(0, input.WeeklyRemaining-input.PendingWeekly-input.WeeklyCapacity*adqWeeklySafety(policy))
			s.adqReservations.SetCapacity(input.ID, fiveLimit, weekLimit)
		}
		var allocation map[string]adqAllocationScore
		var weights map[string]float64
		if mode == adqAllocateWeeklyDeadline {
			allocation, weights = s.weeklyDeadlineScoresLocked(available, inputs, cost, policy, now)
		} else if balanced {
			allocation, weights = s.balancedADQAllocationLocked(available, inputs, cost, now, policy)
		}
		decision, ok := adqChooseWithAllocation(inputs, s.adqForecastDemandLocked(now), cost, policy, now, allocation)
		if !ok {
			return "", adqReservation{}, false, true
		}
		var loan cardLoan
		if mode == adqAllocateBalanced && stickyID == "" && serialPinnedAuthID(req) == "" {
			decision, loan = s.cardCyclePickLocked(decision, available, inputs, cost, policy, now)
		}
		// Near the floor, outstanding work must reserve the guard quantile.
		// Use the selected credential's headroom; a low sibling must not tax
		// healthy accounts or change fair-share work units.
		m := decision.Metrics
		admissionCost := cost
		if (m.W > 0 && 100*m.WEffective <= s.cfg.LowQuotaPercent*m.W) ||
			(adqFiveHourSafety(policy) > 0 && m.H > 0 && 100*m.HEffective <= s.cfg.LowQuotaPercent*m.H) {
			admissionCost = max(cost, s.predictCostLocked(req, true))
		}
		fiveCost := admissionCost
		if decision.Metrics.WeeklyOnly {
			fiveCost = 0
		}
		reservation, ok := s.adqReservations.TryReserve(adqReservationRequest{
			AuthID: decision.AuthID, SessionKey: session, Model: req.Model,
			FiveHour: fiveCost, Weekly: admissionCost, TTL: policy.ReservationTimeout,
		}, now)
		if ok {
			loan.commit(cost)
			reservation.cardLoan = loan
			if mode == adqAllocateWeeklyDeadline {
				decision.Reason = "在最早一批周重置中按所需消耗速度和近期已分配工作量选择凭证。"
			} else if mode == adqAllocateOpportunity {
				switch available[0].opportunityTier {
				case 1:
					decision.Reason = "优先使用错过本轮 5h 后无法在截止前补用的额度。"
				case 2:
					decision.Reason = "既有到期用卡计划临近，优先使用该账号当前可预留的额度。"
				case 3:
					decision.Reason = "集体重置预估区间临近，按各账号本轮实际可用额度分配。"
				}
			}
			decision.ReservationID = reservation.ID
			decision.ReservationFiveHour = reservation.FiveHour
			decision.ReservationWeekly = reservation.Weekly
			s.adqDecisions++
			s.adqLastDecision = decision
			selectedID := decision.AuthID
			for _, choice := range available {
				candidateID := strings.TrimSpace(choice.Candidate.ID)
				canonical := s.canonicalAuthIDLocked(candidateID, choice.Snapshot.AuthIndex)
				if canonical == decision.AuthID || candidateID == decision.AuthID {
					selectedID = candidateID
					break
				}
			}
			if balanced {
				for i, input := range inputs {
					if score, found := allocation[input.ID]; found {
						if account := s.balancedAccounts[available[i].Candidate.ID]; account != nil {
							account.Weight = weights[input.ID]
							if mode == adqAllocateBalanced || mode == adqAllocateOpportunity {
								account.Credit = score.Credit
							}
						}
					}
				}
			}
			return selectedID, reservation, true, true
		}
		excluded[decision.AuthID] = true
		s.adqReservationCollisions++
	}
	return "", adqReservation{}, false, true
}

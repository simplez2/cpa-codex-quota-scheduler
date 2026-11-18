package main

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const balancedPendingTTL = 2 * time.Hour

type balancedPending struct {
	Session       string
	RequestToken  string
	At            time.Time
	Model         string
	Cost          float64
	ReservationID string
	weeklyEpoch   weeklyServiceEpoch
}

// Credits are local fair-share work units, never an upstream quota reservation.
// Selection and its predicted debit are atomic, including simultaneous calls.
type balancedAccount struct {
	Credit        float64
	LastPicked    time.Time
	LastSeen      time.Time
	Picks         uint64
	Weight        float64
	Pending       []balancedPending
	Completed     map[string]time.Time
	weeklyService weeklyService
}

type balancedAccountStatus struct {
	Picks            uint64    `json:"picks"`
	Pending          int       `json:"pending_estimate"`
	Weight           float64   `json:"weight"`
	LastPicked       time.Time `json:"last_picked"`
	WeeklyRecentWork float64   `json:"recent_weekly_work_units,omitempty"`
}

// Both policies allocate predicted work, rather than equal request counts.
// The 5h fraction scales available headroom without adding an admission reserve.
func balancedWeight(choice serialCandidate, cfg pluginConfig, now time.Time) float64 {
	return balancedWeightWithQuota(choice, cfg, now, choice.FiveHourRemaining, choice.WeeklyRemaining)
}

func balancedWeightWithQuota(choice serialCandidate, cfg pluginConfig, now time.Time, fiveRemaining, weeklyRemaining float64) float64 {
	if balancedResetWeighted(cfg) {
		return balancedResetWeight(choice, cfg, now, fiveRemaining, weeklyRemaining)
	}
	_, capacity, _ := resolvedQuotaPlan(cfg, choice.Candidate.ID, choice.Snapshot, now)
	fiveFactor, weeklyFactor := 1.0, 1.0
	if choice.FiveHourKnown {
		fiveFactor = math.Max(.01, clampADQ(fiveRemaining, 0, 100)/100)
		// Spend real expiring 5h capacity first. A moving full placeholder
		// is dormant capacity and never receives a fabricated deadline boost.
		if choice.FiveHourCycleActive && choice.FiveHourResetAt.After(now) {
			fiveFactor *= 300 / math.Max(1, choice.FiveHourResetAt.Sub(now).Minutes())
		}
	}
	if choice.WeeklyKnown {
		weeklyFactor = clampADQ(weeklyRemaining, 0, 100) / 100
		if serialBudgetEnabled(cfg) {
			// The reserve already determines the candidate tier. Normalize the
			// remaining fraction by its reset horizon, without capping imminent
			// resets at a full-week rate or subtracting the reserve twice.
			copyCfg := cfg
			copyCfg.ReserveWeeklyPercent = 0
			if budget, known := serialWeeklyBudget(choice, copyCfg, now); known && choice.WeeklyRemaining > 0 {
				weeklyFactor = budget * clampADQ(weeklyRemaining/choice.WeeklyRemaining, 0, 1) / (100.0 / 7)
			}
		}
		// Settled work can briefly consume the scheduling waterline while
		// telemetry lags. Keep a small fair share; hard gates remain in ADQ.
		weeklyFactor = math.Max(.01, weeklyFactor)
	}
	return capacity * fiveFactor * weeklyFactor
}

// Compute a tentative fair-share round. Only a successful ADQ reservation
// commits these credits; collision retries cannot mint additional credit.
func (s *schedulerRuntimeState) balancedADQAllocationLocked(choices []serialCandidate, inputs []adqAccountInput, cost float64, now time.Time, policy adqPolicy) (map[string]adqAllocationScore, map[string]float64) {
	weights := make(map[string]float64, len(inputs))
	weeklyOnly := make(map[string]bool)
	opportunity := len(choices) > 0 && choices[0].opportunityTier > 0
	maxRamp := 0.
	total := 0.0
	for i, input := range inputs {
		metrics := adqAssessAccount(input, policy, now)
		if !metrics.Eligible {
			continue
		}
		five, weekly := 100.0, 100.0
		if metrics.H > 0 {
			five = 100 * metrics.HEffective / metrics.H
		}
		if metrics.W > 0 {
			weekly = 100 * metrics.WEffective / metrics.W
		}
		weight := balancedWeightWithQuota(choices[i], s.cfg, now, five, weekly)
		if opportunity {
			weight = choices[i].opportunityWeight
		}
		weights[input.ID] = weight
		if metrics.WeeklyOnly {
			weeklyOnly[input.ID] = true
			if capacityDeadlineEnabled(s.cfg) {
				maxRamp = max(maxRamp, catchupCapacityRamp(choices[i], s.cfg, now))
			}
		}
	}
	if !opportunity {
		capPlacementCatchup(weights, weeklyOnly, s.cfg, maxRamp)
	}
	for _, weight := range weights {
		total += weight
	}
	scores := make(map[string]adqAllocationScore, len(weights))
	limit := cost * math.Max(8, float64(len(weights))*2)
	for i, input := range inputs {
		weight, eligible := weights[input.ID]
		if !eligible || total <= 0 {
			continue
		}
		account := s.balancedAccounts[choices[i].Candidate.ID]
		credit, lastPicked := 0.0, time.Time{}
		if account != nil {
			credit, lastPicked = account.Credit, account.LastPicked
		}
		scores[input.ID] = adqAllocationScore{Credit: math.Max(-limit, math.Min(limit, credit+cost*weight/total)), LastPicked: lastPicked}
	}
	return scores, weights
}

func (s *schedulerRuntimeState) balancedPick(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	blocked := map[string]bool{}
	for _, candidate := range req.Candidates {
		if !banStore.schedulable(candidate.ID, now) {
			blocked[strings.TrimSpace(candidate.ID)] = true
		}
	}
	s.mu.Lock()
	now = s.balancedTimeLocked(now)
	bindingChanged := false
	defer func() {
		s.mu.Unlock()
		// New conversations and failovers are durable. Continuations renew in
		// memory and piggyback on the regular state flush instead of disk I/O
		// for every model/tool request.
		if bindingChanged {
			s.persistBanState()
		}
	}()
	if !s.cfg.Enabled {
		return pluginapi.SchedulerPickResponse{}
	}
	s.requestWeeklyCatchupProbesLocked(now)
	if s.balancedAccounts == nil {
		s.balancedAccounts = map[string]*balancedAccount{}
	}
	for id, account := range s.balancedAccounts {
		if now.Sub(account.LastSeen) > balancedPendingTTL {
			delete(s.balancedAccounts, id)
		}
	}
	pinned := serialPinnedAuthID(req)
	session, parent := "", ""
	if s.cfg.StickySeconds > 0 && pinned == "" {
		session = schedulerSessionHash(req)
		parent = schedulerParentSessionHash(req)
	}
	s.pruneBalancedSessionsLocked(now)
	binding, bound := s.balancedSessionLocked(session, now)
	directBinding := bound
	if !bound && parent != "" {
		binding, bound = s.balancedSessionLocked(parent, now)
	}
	var sticky *serialCandidate
	choices := []serialCandidate{}
	seen := map[string]bool{}
	for _, candidate := range req.Candidates {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" || seen[candidate.ID] || blocked[candidate.ID] || (pinned != "" && pinned != candidate.ID) {
			continue
		}
		seen[candidate.ID] = true
		snapshot, found := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
		snapshot = s.serialConservativeQuotaLocked(snapshot, now)
		choice := inspectSerialCandidate(candidate, snapshot, found, s.cfg, now)
		choice.flexSprint = s.capacityFlexHoldLocked(choice, now)
		if !choice.Eligible && choice.Reason != "serial_threshold" {
			continue
		}
		if weeklyHandoffBlocked(choice, s.cfg, now) && !(s.cfg.SerialSoftContinuation && bound &&
			candidate.ID == binding.AuthID && (binding.AuthIndex == "" || binding.AuthIndex == snapshot.AuthIndex)) {
			continue
		}
		// Identify current affinity before testing for a safe handoff at a turn
		// boundary. Hard quota and CPA's filtered candidates stay authoritative.
		if bound && (choice.Eligible || s.cfg.SerialSoftContinuation) && choice.Candidate.ID == binding.AuthID &&
			(binding.AuthIndex == "" || !found || binding.AuthIndex == snapshot.AuthIndex) {
			copy := choice
			sticky = &copy
		}
		choices = append(choices, choice)
	}
	if len(choices) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	// Sorting makes simultaneous equal-share starts independent of host order.
	sort.Slice(choices, func(i, j int) bool { return choices[i].Candidate.ID < choices[j].Candidate.ID })
	cost := math.Max(.001, s.predictCostLocked(req, false))
	limit := cost * math.Max(8, float64(len(choices))*2)
	for _, choice := range choices {
		id := choice.Candidate.ID
		account := s.balancedAccounts[id]
		if account == nil {
			account = &balancedAccount{}
			s.balancedAccounts[id] = account
		}
		if now.Sub(account.LastSeen) > 15*time.Minute {
			account.Credit = 0
		}
		account.LastSeen = now
		account.Weight = balancedWeight(choice, s.cfg, now)
		account.prune(now)
	}
	selected := ""
	adqRejected := false
	reservation := adqReservation{}
	var handbackDeadline time.Time
	coldMigration := false
	remaining := choices
	if sticky != nil && (sticky.Priority.WeeklyUrgent || sticky.flexSprint) {
		// Spend the rest of this confirmed weekly cycle on the bound account.
		// Validate and reserve it independently of how many 5h accounts recover
		// or whether the ordinary priority tier has complete telemetry.
		id, reserved, ok := s.adqRouteChoicesLocked(req, []serialCandidate{*sticky}, cost, now, session, sticky.Candidate.ID)
		if ok {
			selected, reservation = id, reserved
			s.balancedSessionHits++
		} else {
			adqRejected = true
			remaining = make([]serialCandidate, 0, len(choices)-1)
			for _, choice := range choices {
				if choice.Candidate.ID != sticky.Candidate.ID {
					remaining = append(remaining, choice)
				}
			}
			sticky = nil
		}
	}
	canMoveCold := selected == "" && sticky != nil && directBinding && s.balancedColdWeeklyHandoffLocked(session, binding, *sticky, now)
	if selected == "" && (sticky == nil || canMoveCold) && pinned == "" && (s.cfg.WeeklyOnlyPriorityWindow > 0 || capacityDeadlineEnabled(s.cfg)) {
		source := ""
		if canMoveCold {
			source = binding.AuthID
		}
		failed := map[string]bool{}
		for _, tier := range s.placementTiersLocked(remaining, cost, now, source) {
			var group []serialCandidate
			for _, c := range tier.choices {
				if !failed[c.Candidate.ID] {
					group = append(group, c)
				}
			}
			if len(group) == 0 {
				continue
			}
			id, r, ok, _ := s.adqRouteChoicesWithAllocationLocked(req, group, cost, now, session, "", tier.mode)
			if ok {
				selected, reservation = id, r
				coldMigration = canMoveCold && source != id
				if canMoveCold && source == id {
					s.balancedSessionHits++
				}
				break
			}
			adqRejected = true
			for _, c := range group {
				failed[c.Candidate.ID] = true
			}
		}
		if selected == "" && len(failed) > 0 {
			var rest []serialCandidate
			for _, c := range remaining {
				if !failed[c.Candidate.ID] {
					rest = append(rest, c)
				}
			}
			remaining = rest
			if sticky != nil && failed[sticky.Candidate.ID] {
				sticky = nil
			}
		}
	}
	for selected == "" && len(remaining) > 0 {
		group, rest := nextQuotaPriorityGroup(remaining, s.cfg, now)
		groupSticky := sticky
		if groupSticky != nil && quotaAffinityMustYield(*groupSticky, group, s.cfg) {
			if directBinding {
				deadline := balancedWeeklyHandbackDeadline(binding, *groupSticky, s.cfg, now)
				if !deadline.IsZero() {
					// Admit this continuation on its own weekly reservation. Do not
					// put a non-catch-up backup into the new-session allocation tier.
					id, reserved, ok := s.adqRouteChoicesLocked(req, []serialCandidate{*groupSticky}, cost, now, session, groupSticky.Candidate.ID)
					if ok {
						selected, reservation, handbackDeadline = id, reserved, deadline
						s.balancedSessionHits++
						s.balancedWeeklyDeferrals++
						break
					}
				}
			}
			groupSticky = nil
		}
		if groupSticky != nil {
			if _, present := serialChoiceByID(group, groupSticky.Candidate.ID); !present {
				group = append(group, *groupSticky)
			}
		}
		adqSelected, reserved, adqOK, adqHandled := s.adqRouteChoicesWithAllocationLocked(req, group, cost, now, session, func() string {
			if groupSticky == nil {
				return ""
			}
			return groupSticky.Candidate.ID
		}(), adqAllocateBalanced)
		if adqOK {
			// ADQ commits the policy's fair-share round only after reservation;
			// the fallback cannot overwrite this decision or double-credit it.
			selected = adqSelected
			reservation = reserved
			if groupSticky != nil && selected == groupSticky.Candidate.ID {
				s.balancedSessionHits++
			}
		} else if adqHandled {
			adqRejected = true
			// Complete telemetry with a hard gate/collision is an admission failure,
			// not permission to bypass reservations through the legacy fallback.
			remaining = rest
			if groupSticky != nil {
				sticky = nil
			}
			continue
		} else {
			weights, weeklyOnly := map[string]float64{}, map[string]bool{}
			maxRamp := 0.
			for _, choice := range group {
				weights[choice.Candidate.ID] = balancedWeight(choice, s.cfg, now)
				if !choice.FiveHourKnown && choice.QuotaKnown {
					weeklyOnly[choice.Candidate.ID] = true
					if capacityDeadlineEnabled(s.cfg) {
						maxRamp = max(maxRamp, catchupCapacityRamp(choice, s.cfg, now))
					}
				}
			}
			capPlacementCatchup(weights, weeklyOnly, s.cfg, maxRamp)
			total := 0.0
			for _, choice := range group {
				s.balancedAccounts[choice.Candidate.ID].Weight = weights[choice.Candidate.ID]
				total += s.balancedAccounts[choice.Candidate.ID].Weight
			}
			bestScore := math.Inf(-1)
			for _, choice := range group {
				id := choice.Candidate.ID
				account := s.balancedAccounts[id]
				account.Credit = math.Max(-limit, math.Min(limit, account.Credit+cost*account.Weight/total))
				// Capped credits can tie after many cheap sticky continuations. Prefer
				// the least recently selected account instead of permanently favoring
				// the lexicographically first credential at that ceiling.
				if account.Credit > bestScore || (account.Credit == bestScore && selected != "" && account.LastPicked.Before(s.balancedAccounts[selected].LastPicked)) {
					selected = id
					bestScore = account.Credit
				}
			}
			if groupSticky != nil {
				selected = groupSticky.Candidate.ID
				s.balancedSessionHits++
			}
		}
		break
	}
	if selected == "" {
		if adqRejected {
			return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: "quota_reservation_unavailable", RejectReason: "No Codex credential has enough unreserved quota for this request"}
		}
		return pluginapi.SchedulerPickResponse{}
	}
	account := s.balancedAccounts[selected]
	account.LastPicked = now
	account.Picks++
	if len(account.Pending) >= 256 {
		account.Pending = account.Pending[1:]
	}
	pending := balancedPending{At: now, Model: normalizeModelName(req.Model), Cost: cost, Session: session, RequestToken: schedulerHeader(req.Options, concurrencyHeader), ReservationID: reservation.ID}
	selectedChoice, _ := serialChoiceByID(choices, selected)
	index := selectedChoice.Snapshot.AuthIndex
	if selectedChoice.Priority.WeeklyUrgent && index != "" {
		if horizon, ok := weeklyResetHorizon(selectedChoice, s.cfg, now); ok {
			pending.weeklyEpoch = weeklyServiceEpoch{index, now.Add(horizon)}
			account.weeklyService.assign(pending.weeklyEpoch, cost, now)
		}
	}
	if pending.weeklyEpoch.index == "" {
		account.Credit -= cost
	} else {
		// Deadline service has its own ledger. Return to ordinary sharing
		// with neutral credit once this weekly sprint ends.
		account.Credit = 0
	}
	account.Pending = append(account.Pending, pending)
	if session != "" {
		bindingChanged = s.bindBalancedSessionLocked(session, selected, index, now)
		if coldMigration {
			s.balancedWeeklyColdMigrations++
		}
		if !handbackDeadline.IsZero() && s.balancedSessions[session].WeeklyHandbackBy.IsZero() {
			current := s.balancedSessions[session]
			current.WeeklyHandbackBy = handbackDeadline
			s.balancedSessions[session] = current
			bindingChanged = true
		}
	} else if pinned == "" {
		s.balancedUnkeyedRequests++
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: selected}
}

func (a *balancedAccount) prune(now time.Time) {
	pending := a.Pending[:0]
	for _, p := range a.Pending {
		if now.Sub(p.At) < balancedPendingTTL {
			pending = append(pending, p)
		}
	}
	a.Pending = pending
	for key, at := range a.Completed {
		if now.Sub(at) > balancedPendingTTL {
			delete(a.Completed, key)
		}
	}
}

func (s *schedulerRuntimeState) observeBalancedUsage(record pluginapi.UsageRecord, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = s.balancedTimeLocked(now)
	rawID := strings.TrimSpace(record.AuthID)
	id := s.canonicalAuthIDLocked(rawID, record.AuthIndex)
	if id == "" {
		id = rawID
	}
	accountID := rawID
	if accountID == "" {
		accountID = id
	}
	account := s.balancedAccounts[accountID]
	if account == nil && id != accountID {
		accountID = id
		account = s.balancedAccounts[accountID]
	}
	if account == nil && strings.TrimSpace(record.AuthIndex) != "" {
		accountID = strings.TrimSpace(record.AuthIndex)
		account = s.balancedAccounts[accountID]
	}
	if account == nil || record.RequestedAt.IsZero() {
		return
	}
	account.prune(now)
	model := normalizeModelName(record.Model)
	key := fmt.Sprintf("%d:%s", record.RequestedAt.UnixNano(), model)
	if _, duplicate := account.Completed[key]; duplicate {
		return
	}
	// CPA's ABI has no shared pick/completion request ID. Match the closest
	// same-model start, within a bounded tolerance; unmatched late events never
	// debit new work. Pending is therefore explicitly an estimate in the panel.
	index := -1
	distance := 30 * time.Second
	for i, p := range account.Pending {
		if p.Model != model || (p.weeklyEpoch.index != "" && record.AuthIndex != "" && p.weeklyEpoch.index != record.AuthIndex) {
			continue
		}
		delta := p.At.Sub(record.RequestedAt)
		if delta < 0 {
			delta = -delta
		}
		if delta < distance {
			index = i
			distance = delta
		}
	}
	if index < 0 {
		return
	}
	pending := account.Pending[index]
	if binding, ok := s.balancedSessions[pending.Session]; ok && (binding.AuthID == accountID || binding.AuthID == id || binding.AuthID == rawID) &&
		(record.AuthIndex == "" || binding.AuthIndex == "" || record.AuthIndex == binding.AuthIndex) && now.After(binding.LastUsedAt) {
		binding.LastUsedAt = now
		s.balancedSessions[pending.Session] = binding
	}
	predicted := pending.Cost
	account.Pending = append(account.Pending[:index], account.Pending[index+1:]...)
	if account.Completed == nil {
		account.Completed = map[string]time.Time{}
	}
	if len(account.Completed) >= 512 {
		oldestKey := ""
		oldest := now
		for k, at := range account.Completed {
			if oldestKey == "" || at.Before(oldest) {
				oldestKey = k
				oldest = at
			}
		}
		delete(account.Completed, oldestKey)
	}
	account.Completed[key] = now

	actual, known := usageCredits(record, s.pricing)
	validActual := known && !record.Failed && actual >= 0 && finiteADQ(actual)
	if pending.weeklyEpoch.index != "" && (record.Failed || validActual) {
		charged := actual
		if record.Failed {
			charged = 0
		}
		account.weeklyService.correct(pending.weeklyEpoch, charged-predicted, pending.At, now)
	}
	if reservationID := strings.TrimSpace(pending.ReservationID); reservationID != "" && s.adqReservations != nil {
		if record.Failed {
			s.adqReservations.Release(reservationID)
		} else {
			// Completion ends the concurrency reservation, but provider quota
			// percentages can lag behind it. Retain actual usage when available
			// and the original prediction otherwise until a newer quota
			// observation acknowledges the burn or the bounded TTL expires.
			debit := predicted
			if validActual {
				debit = actual
			}
			snapshot := s.quotas[id]
			if strings.TrimSpace(snapshot.AuthID) == "" {
				snapshot = s.quotas[accountID]
			}
			s.adqReservations.Settle(reservationID, debit, debit, snapshot, now, s.adqPolicyLocked().SettlementTimeout)
		}
	}
	if validActual && pending.weeklyEpoch.index == "" {
		// A very large completion changes future share without starving an
		// account forever. This estimate cannot mark its quota exhausted.
		limit := math.Max(predicted, .001) * 16
		account.Credit = math.Max(-limit, math.Min(limit, account.Credit+predicted-actual))
	}
}

func (s *schedulerRuntimeState) balancedStatusLocked(now time.Time) map[string]balancedAccountStatus {
	result := map[string]balancedAccountStatus{}
	for id, account := range s.balancedAccounts {
		pending := 0
		for _, p := range account.Pending {
			if now.Sub(p.At) < balancedPendingTTL {
				pending++
			}
		}
		status := balancedAccountStatus{Picks: account.Picks, Pending: pending, Weight: account.Weight, LastPicked: account.LastPicked}
		if q, ok := s.lookupQuotaLocked(id, ""); ok {
			epoch := weeklyServiceEpoch{q.AuthIndex, adqResetForClass(q, "weekly")}
			status.WeeklyRecentWork = account.weeklyService.atTime(epoch, now)
		}
		result[id] = status
	}
	return result
}

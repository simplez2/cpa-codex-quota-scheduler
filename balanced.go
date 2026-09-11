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
	At            time.Time
	Model         string
	Cost          float64
	ReservationID string
}

// Credits are local fair-share work units, never an upstream quota reservation.
// Selection and its predicted debit are atomic, including simultaneous calls.
type balancedAccount struct {
	Credit     float64
	LastPicked time.Time
	LastSeen   time.Time
	Picks      uint64
	Weight     float64
	Pending    []balancedPending
	Completed  map[string]time.Time
}

type balancedAccountStatus struct {
	Picks      uint64    `json:"picks"`
	Pending    int       `json:"pending_estimate"`
	Weight     float64   `json:"weight"`
	LastPicked time.Time `json:"last_picked"`
}

func balancedWeight(choice serialCandidate, cfg pluginConfig, now time.Time) float64 {
	_, capacity, _ := resolvedQuotaPlan(cfg, choice.Candidate.ID, choice.Snapshot, now)
	// At identical percentages/reset times, larger plans carry proportionally
	// more work. The scarcer of 5h headroom and weekly daily budget sets pace.
	pace := 1.0
	if choice.FiveHourKnown {
		pace = math.Max(.01, choice.FiveHourRemaining/100)
	}
	budget, known := serialWeeklyBudget(choice, cfg, now)
	if known {
		if choice.WeeklyProtected {
			copyCfg := cfg
			copyCfg.ReserveWeeklyPercent = 0
			budget, _ = serialWeeklyBudget(choice, copyCfg, now)
		}
		pace = math.Min(pace, math.Max(.01, budget/(100.0/7)))
	}
	return capacity * pace
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
	if !bound && parent != "" {
		binding, bound = s.balancedSessionLocked(parent, now)
	}
	var sticky *serialCandidate
	choices := []serialCandidate{}
	bestTier := 10
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
		if !choice.Eligible && choice.Reason != "serial_threshold" {
			continue
		}
		// A live conversation stays on an available account even if another
		// plan or weekly budget scores better. Hard quota and CPA's filtered
		// candidates remain authoritative; soft tiers only place new sessions.
		if bound && choice.Candidate.ID == binding.AuthID &&
			(binding.AuthIndex == "" || !found || binding.AuthIndex == snapshot.AuthIndex) {
			copy := choice
			sticky = &copy
		}
		tier := 0
		if choice.WeeklyProtected {
			tier = 1
		}
		if !choice.QuotaKnown {
			tier = 2
		}
		if !choice.Eligible {
			tier = 3
		}
		if tier < bestTier {
			choices = nil
			bestTier = tier
		}
		if tier == bestTier {
			choices = append(choices, choice)
		}
	}
	if len(choices) == 0 {
		return pluginapi.SchedulerPickResponse{}
	}
	if sticky != nil {
		present := false
		for _, choice := range choices {
			if choice.Candidate.ID == sticky.Candidate.ID {
				present = true
				break
			}
		}
		if !present {
			choices = append(choices, *sticky)
		}
	}
	// Sorting makes simultaneous equal-share starts independent of host order.
	sort.Slice(choices, func(i, j int) bool { return choices[i].Candidate.ID < choices[j].Candidate.ID })
	cost := math.Max(.001, s.predictCostLocked(req, false))
	limit := cost * math.Max(8, float64(len(choices))*2)
	total := 0.0
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
		total += account.Weight
		account.prune(now)
	}
	selected := ""
	reservation := adqReservation{}
	adqSelected, adqReservation, adqOK := s.adqRouteChoicesLocked(req, choices, cost, now, session, func() string {
		if sticky == nil {
			return ""
		}
		return sticky.Candidate.ID
	}())
	if adqOK {
		// ADQ owns the final choice whenever both windows have observed absolute
		// capacities. The legacy credit score remains a fallback and is never
		// allowed to overwrite a reserved decision.
		selected = adqSelected
		reservation = adqReservation
		if sticky != nil {
			s.balancedSessionHits++
		}
	} else {
		bestScore := math.Inf(-1)
		for _, choice := range choices {
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
		if sticky != nil {
			selected = sticky.Candidate.ID
			s.balancedSessionHits++
		}
	}
	if selected == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	account := s.balancedAccounts[selected]
	account.Credit -= cost
	account.LastPicked = now
	account.Picks++
	if len(account.Pending) >= 256 {
		account.Pending = account.Pending[1:]
	}
	account.Pending = append(account.Pending, balancedPending{At: now, Model: normalizeModelName(req.Model), Cost: cost, Session: session, ReservationID: reservation.ID})
	if session != "" {
		var index string
		for _, choice := range choices {
			if choice.Candidate.ID == selected {
				index = choice.Snapshot.AuthIndex
				break
			}
		}
		bindingChanged = s.bindBalancedSessionLocked(session, selected, index, now)
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
		if p.Model != model {
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
	if reservationID := strings.TrimSpace(pending.ReservationID); reservationID != "" && s.adqReservations != nil {
		// A reservation is only held until CPA reports the matching request. Any
		// failure or token-less completion releases the speculative debit; a
		// successful tokenized completion reconciles both quota windows once.
		if validActual {
			s.adqReservations.Reconcile(reservationID, actual, actual)
		} else {
			s.adqReservations.Release(reservationID)
		}
	}
	if validActual {
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
		result[id] = balancedAccountStatus{account.Picks, pending, account.Weight, account.LastPicked}
	}
	return result
}

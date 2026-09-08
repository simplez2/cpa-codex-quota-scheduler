package main

import "time"

const serialWeeklyRebalanceConfirmations = 2

// Confirmation evidence is deliberately transient. A reload restores the
// selected-at hold, but must observe a challenger again before rebalancing.
type serialWeeklyRebalanceState struct {
	PrimaryAuthID         string    `json:"primary_auth_id,omitempty"`
	CandidateAuthID       string    `json:"candidate_auth_id,omitempty"`
	PrimaryAuthIndex      string    `json:"primary_auth_index,omitempty"`
	CandidateAuthIndex    string    `json:"candidate_auth_index,omitempty"`
	PrimaryObservedAt     time.Time `json:"primary_observed_at,omitempty"`
	CandidateObservedAt   time.Time `json:"candidate_observed_at,omitempty"`
	PrimaryResetAt        time.Time `json:"primary_reset_at,omitempty"`
	CandidateResetAt      time.Time `json:"candidate_reset_at,omitempty"`
	AdvantagePercent      float64   `json:"advantage_percent"`
	Confirmations         int       `json:"confirmations"`
	Metric                string    `json:"metric,omitempty"`
	PrimaryBudgetPerDay   float64   `json:"primary_weekly_budget_percent_per_day"`
	CandidateBudgetPerDay float64   `json:"candidate_weekly_budget_percent_per_day"`
}

type serialWeeklyEvidence struct {
	observedAt time.Time
	resetAt    time.Time
}

// Do not confuse a freshly copied outer snapshot with a new weekly reading.
// Automatic optimization requires an inventory-bound identity, no failed poll,
// and actual per-window observation timestamps. Normal fallback can still use
// the existing, more permissive quota-unknown availability policy.
func (s *schedulerRuntimeState) serialWeeklyEvidenceLocked(choice serialCandidate, now time.Time) (serialWeeklyEvidence, bool) {
	q := choice.Snapshot
	if !choice.Eligible || !choice.QuotaKnown || !choice.WeeklyKnown ||
		q.AuthID != choice.Candidate.ID || q.AuthIndex == "" || s.identities[q.AuthIndex] != q.AuthID {
		return serialWeeklyEvidence{}, false
	}
	if poll, ok := s.quotaPolls[q.AuthID]; ok &&
		(poll.AuthIndex != q.AuthIndex || poll.Error != "" || poll.Failures > 0) {
		return serialWeeklyEvidence{}, false
	}
	if native, ok := s.quotaNative[q.AuthID]; ok && native.AuthIndex == q.AuthIndex {
		q = native
	} else {
		for _, w := range q.Windows {
			if normalizeWindowClass(w.Class) == "weekly" && (w.Source == quotaSourceHeader || w.Source == quotaSourceMixed) {
				return serialWeeklyEvidence{}, false
			}
		}
	}
	var evidence serialWeeklyEvidence
	for _, w := range q.Windows {
		if normalizeWindowClass(w.Class) != "weekly" || (!w.ResetAt.IsZero() && !now.Before(w.ResetAt)) {
			continue
		}
		if w.ObservedAt.IsZero() || w.ObservedAt.After(now) || now.Sub(w.ObservedAt) > s.cfg.StaleAfter {
			return serialWeeklyEvidence{}, false
		}
		if evidence.observedAt.IsZero() || w.ObservedAt.Before(evidence.observedAt) {
			evidence.observedAt = w.ObservedAt
		}
		if !w.ResetAt.IsZero() && (evidence.resetAt.IsZero() || w.ResetAt.Before(evidence.resetAt)) {
			evidence.resetAt = w.ResetAt
		}
	}
	return evidence, !evidence.observedAt.IsZero() && evidence.observedAt.After(s.serialSelectedAt)
}

func serialWeeklyCycleChanged(previous, next, now time.Time) bool {
	// Idle 0% placeholders may advance before their old reset. Only an elapsed
	// boundary or lost reset identity invalidates otherwise independent samples.
	return previous.IsZero() != next.IsZero() ||
		(!previous.IsZero() && !now.Before(previous) && next.After(previous))
}

func serialCyclePreservesWeekly(current, next serialCandidate, cfg pluginConfig) bool {
	if serialBudgetEnabled(cfg) && current.WeeklyBudgetKnown && next.WeeklyBudgetKnown {
		return next.WeeklyBudgetPerDay >= current.WeeklyBudgetPerDay*0.95
	}
	return !current.WeeklyKnown || (next.WeeklyKnown &&
		next.WeeklyRemaining+cfg.SwitchHysteresisPercent >= current.WeeklyRemaining)
}

// The caller holds s.mu. Request count is never confirmation count: BOTH
// weekly observations must advance before another confirmation can be earned.
func (s *schedulerRuntimeState) serialWeeklyRebalancePickLocked(current serialCandidate, choices []serialCandidate, cfg pluginConfig, now time.Time) (serialCandidate, bool) {
	reset := func() (serialCandidate, bool) {
		s.serialWeeklyRebalance = serialWeeklyRebalanceState{}
		return serialCandidate{}, false
	}
	if (serialBudgetEnabled(cfg) && cfg.SerialBudgetRebalancePercent <= 0) ||
		(!serialBudgetEnabled(cfg) && cfg.SerialWeeklyRebalancePercent <= 0) || normalizeSerialSelectionSource(s.serialSelectionSource) == "manual" {
		return reset()
	}
	primary, ok := s.serialWeeklyEvidenceLocked(current, now)
	if !ok {
		return reset()
	}
	var selected serialCandidate
	var candidate serialWeeklyEvidence
	for _, choice := range choices {
		if choice.WindowClass != current.WindowClass {
			continue
		}
		if _, better := serialRebalanceAdvantage(current, choice, cfg); !better {
			continue
		}
		if evidence, valid := s.serialWeeklyEvidenceLocked(choice, now); valid {
			selected, candidate = choice, evidence
			break
		}
	}
	if selected.Candidate.ID == "" {
		return reset()
	}
	e := &s.serialWeeklyRebalance
	if e.PrimaryAuthID != current.Candidate.ID || e.CandidateAuthID != selected.Candidate.ID ||
		e.PrimaryAuthIndex != current.Snapshot.AuthIndex || e.CandidateAuthIndex != selected.Snapshot.AuthIndex ||
		primary.observedAt.Before(e.PrimaryObservedAt) || candidate.observedAt.Before(e.CandidateObservedAt) ||
		now.Sub(e.PrimaryObservedAt) > cfg.StaleAfter || now.Sub(e.CandidateObservedAt) > cfg.StaleAfter ||
		serialWeeklyCycleChanged(e.PrimaryResetAt, primary.resetAt, now) ||
		serialWeeklyCycleChanged(e.CandidateResetAt, candidate.resetAt, now) {
		*e = serialWeeklyRebalanceState{
			PrimaryAuthID: current.Candidate.ID, CandidateAuthID: selected.Candidate.ID,
			PrimaryAuthIndex: current.Snapshot.AuthIndex, CandidateAuthIndex: selected.Snapshot.AuthIndex,
			PrimaryResetAt: primary.resetAt, CandidateResetAt: candidate.resetAt,
		}
	}
	if primary.observedAt.After(e.PrimaryObservedAt) && candidate.observedAt.After(e.CandidateObservedAt) {
		e.PrimaryObservedAt, e.CandidateObservedAt = primary.observedAt, candidate.observedAt
		if e.Confirmations < serialWeeklyRebalanceConfirmations {
			e.Confirmations++
		}
	}
	e.AdvantagePercent, _ = serialRebalanceAdvantage(current, selected, cfg)
	e.Metric = "weekly_remaining_percentage_points"
	if serialBudgetEnabled(cfg) && current.WeeklyBudgetKnown && selected.WeeklyBudgetKnown {
		e.Metric = "weekly_budget_relative_percent"
		e.PrimaryBudgetPerDay, e.CandidateBudgetPerDay = current.WeeklyBudgetPerDay, selected.WeeklyBudgetPerDay
	}
	return selected, e.Confirmations >= serialWeeklyRebalanceConfirmations &&
		!s.serialSelectedAt.IsZero() && now.Sub(s.serialSelectedAt) >= cfg.SerialWeeklyRebalanceMinHold
}

func (s *schedulerRuntimeState) resetSerialWeeklyEvidenceForAuthLocked(authID string) {
	if s.serialWeeklyRebalance.PrimaryAuthID == authID || s.serialWeeklyRebalance.CandidateAuthID == authID {
		s.serialWeeklyRebalance = serialWeeklyRebalanceState{}
	}
}

// An automatic preemption moves subsequent requests of existing sessions too.
// It cannot affect an already-running stream and does not extend binding TTLs.
func (s *schedulerRuntimeState) rebindSerialSessionsLocked(previous, selected string) {
	for session, binding := range s.serialOverdraft {
		if binding.AuthID == previous {
			binding.AuthID = selected
			s.serialOverdraft[session] = binding
		}
	}
}

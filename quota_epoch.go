package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	quotaEpochSweepWindow       = 10 * time.Minute
	quotaEpochSweepRoundDelay   = 45 * time.Second
	quotaEpochSweepMaxRounds    = 4
	quotaEpochWarmupSnapshotAge = 10 * time.Minute
	quotaEpochResetCatchupAge   = 15 * time.Minute
	quotaEpochResetAnchorSkew   = 15 * time.Minute
	quotaEpochResetRisePercent  = 10.0
	quotaEpochPendingProbeLimit = 3
	quotaEpochPendingProbeDelay = 30 * time.Second

	quotaEpochAccountPending = "pending"
	quotaEpochAccountNatural = "natural"
	quotaEpochAccountWarmed  = "warmed"
	quotaEpochAccountBlocked = "blocked"
)

type quotaEpochAccountState struct {
	AuthID        string    `json:"auth_id"`
	AuthIndex     string    `json:"auth_index,omitempty"`
	State         string    `json:"state"`
	ObservedAt    time.Time `json:"observed_at,omitempty"`
	LastUsageAt   time.Time `json:"last_usage_at,omitempty"`
	LastWarmEpoch string    `json:"last_warm_epoch,omitempty"`
	LastWarmAt    time.Time `json:"last_warm_at,omitempty"`
	ProbeAttempts int       `json:"probe_attempts,omitempty"`
	LastProbeAt   time.Time `json:"last_probe_at,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type quotaEpochSweepTarget struct {
	AuthID         string    `json:"auth_id"`
	AuthIndex      string    `json:"auth_index,omitempty"`
	AttemptedAt    time.Time `json:"attempted_at,omitempty"`
	ObservedAt     time.Time `json:"observed_at,omitempty"`
	Evidence       bool      `json:"evidence,omitempty"`
	EvidenceReason string    `json:"evidence_reason,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type quotaEpochSweepState struct {
	Active         bool                             `json:"active"`
	Reason         string                           `json:"reason,omitempty"`
	TriggerAuthID  string                           `json:"trigger_auth_id,omitempty"`
	TriggerReason  string                           `json:"trigger_reason,omitempty"`
	StartedAt      time.Time                        `json:"started_at,omitempty"`
	Deadline       time.Time                        `json:"deadline,omitempty"`
	Round          int                              `json:"round,omitempty"`
	RoundStartedAt time.Time                        `json:"round_started_at,omitempty"`
	NextRoundAt    time.Time                        `json:"next_round_at,omitempty"`
	CompletedAt    time.Time                        `json:"completed_at,omitempty"`
	Required       int                              `json:"required"`
	EpochID        string                           `json:"epoch_id,omitempty"`
	Targets        map[string]quotaEpochSweepTarget `json:"targets,omitempty"`
}

type quotaEpochState struct {
	ID          string                            `json:"id,omitempty"`
	Sequence    uint64                            `json:"sequence,omitempty"`
	Reason      string                            `json:"reason,omitempty"`
	ConfirmedAt time.Time                         `json:"confirmed_at,omitempty"`
	ResetAt     time.Time                         `json:"reset_at,omitempty"`
	Accounts    map[string]quotaEpochAccountState `json:"accounts,omitempty"`
	Sweep       quotaEpochSweepState              `json:"sweep"`
}

type quotaEpochTransition struct {
	ID     string
	Reason string
}

type runtimeQuotaEpochAccountStatus struct {
	AuthID        string `json:"auth_id"`
	AuthIndex     string `json:"auth_index,omitempty"`
	State         string `json:"state"`
	ObservedAt    string `json:"observed_at,omitempty"`
	LastUsageAt   string `json:"last_usage_at,omitempty"`
	LastWarmEpoch string `json:"last_warm_epoch,omitempty"`
	LastWarmAt    string `json:"last_warm_at,omitempty"`
	ProbeAttempts int    `json:"probe_attempts,omitempty"`
	LastProbeAt   string `json:"last_probe_at,omitempty"`
	Error         string `json:"error,omitempty"`
}

type runtimeQuotaEpochStatus struct {
	ID             string                           `json:"id,omitempty"`
	Sequence       uint64                           `json:"sequence"`
	Reason         string                           `json:"reason,omitempty"`
	ConfirmedAt    string                           `json:"confirmed_at,omitempty"`
	ResetAt        string                           `json:"reset_at,omitempty"`
	SweepActive    bool                             `json:"sweep_active"`
	SweepReason    string                           `json:"sweep_reason,omitempty"`
	SweepStartedAt string                           `json:"sweep_started_at,omitempty"`
	SweepDeadline  string                           `json:"sweep_deadline,omitempty"`
	SweepRound     int                              `json:"sweep_round"`
	SweepMaxRounds int                              `json:"sweep_max_rounds"`
	SweepNextRound string                           `json:"sweep_next_round_at,omitempty"`
	SweepObserved  int                              `json:"sweep_observed"`
	SweepTargets   int                              `json:"sweep_targets"`
	SweepEvidence  int                              `json:"sweep_evidence"`
	SweepRequired  int                              `json:"sweep_required"`
	Pending        int                              `json:"pending"`
	Natural        int                              `json:"natural"`
	Warmed         int                              `json:"warmed"`
	Blocked        int                              `json:"blocked"`
	Accounts       []runtimeQuotaEpochAccountStatus `json:"accounts,omitempty"`
}

func newQuotaEpochState() quotaEpochState {
	return quotaEpochState{
		Accounts: make(map[string]quotaEpochAccountState),
		Sweep: quotaEpochSweepState{
			Targets: make(map[string]quotaEpochSweepTarget),
		},
	}
}

func normalizeQuotaEpochState(state quotaEpochState) quotaEpochState {
	out := cloneQuotaEpochState(state)
	if out.Accounts == nil {
		out.Accounts = make(map[string]quotaEpochAccountState)
	}
	if out.Sweep.Targets == nil {
		out.Sweep.Targets = make(map[string]quotaEpochSweepTarget)
	}
	if out.Sweep.Active {
		if out.Sweep.Round < 1 {
			out.Sweep.Round = 1
		}
		if out.Sweep.RoundStartedAt.IsZero() {
			out.Sweep.RoundStartedAt = out.Sweep.StartedAt
		}
	}
	for key, account := range out.Accounts {
		id := strings.TrimSpace(account.AuthID)
		if id == "" {
			id = strings.TrimSpace(key)
		}
		if id == "" {
			delete(out.Accounts, key)
			continue
		}
		account.AuthID = id
		switch account.State {
		case quotaEpochAccountPending, quotaEpochAccountNatural, quotaEpochAccountWarmed, quotaEpochAccountBlocked:
		default:
			account.State = quotaEpochAccountPending
		}
		if key != id {
			delete(out.Accounts, key)
		}
		out.Accounts[id] = account
	}
	for key, target := range out.Sweep.Targets {
		id := strings.TrimSpace(target.AuthID)
		if id == "" {
			id = strings.TrimSpace(key)
		}
		if id == "" {
			delete(out.Sweep.Targets, key)
			continue
		}
		target.AuthID = id
		if key != id {
			delete(out.Sweep.Targets, key)
		}
		out.Sweep.Targets[id] = target
	}
	if out.Sweep.Required < 0 || out.Sweep.Required > len(out.Sweep.Targets) {
		out.Sweep.Required = quotaEpochQuorum(len(out.Sweep.Targets))
	}
	return out
}

func cloneQuotaEpochState(state quotaEpochState) quotaEpochState {
	out := state
	out.Accounts = make(map[string]quotaEpochAccountState, len(state.Accounts))
	for id, account := range state.Accounts {
		out.Accounts[id] = account
	}
	out.Sweep.Targets = make(map[string]quotaEpochSweepTarget, len(state.Sweep.Targets))
	for id, target := range state.Sweep.Targets {
		out.Sweep.Targets[id] = target
	}
	return out
}

func quotaEpochQuorum(accounts int) int {
	if accounts <= 1 {
		return accounts
	}
	required := (accounts + 1) / 2
	if required < 2 {
		required = 2
	}
	return required
}

func quotaEpochWeeklyWindow(snapshot quotaSnapshot) (quotaWindow, bool) {
	windows := normalizeQuotaWindowSet(snapshot.Windows, snapshot.RefreshedAt, snapshot.RefreshedAt, 15*time.Minute)
	for _, window := range windows {
		if normalizeWindowClass(window.Class) == "weekly" {
			return window, true
		}
	}
	return quotaWindow{}, false
}

func quotaEpochResetEvidence(previous, current quotaSnapshot, now time.Time) (string, bool) {
	if strings.TrimSpace(previous.AuthID) == "" || strings.TrimSpace(current.AuthID) == "" ||
		strings.TrimSpace(previous.AuthID) != strings.TrimSpace(current.AuthID) ||
		(strings.TrimSpace(previous.AuthIndex) != "" && strings.TrimSpace(current.AuthIndex) != "" &&
			strings.TrimSpace(previous.AuthIndex) != strings.TrimSpace(current.AuthIndex)) {
		return "", false
	}
	previousWindow, previousOK := quotaEpochWeeklyWindow(previous)
	currentWindow, currentOK := quotaEpochWeeklyWindow(current)
	if !previousOK || !currentOK {
		return "", false
	}
	previousAt := previousWindow.ObservedAt
	if previousAt.IsZero() {
		previousAt = previous.RefreshedAt
	}
	currentAt := currentWindow.ObservedAt
	if currentAt.IsZero() {
		currentAt = current.RefreshedAt
	}
	if currentAt.IsZero() || (!previousAt.IsZero() && !currentAt.After(previousAt)) {
		return "", false
	}
	previousPlaceholder := quotaWindowHasPlaceholderReset(previousWindow, previous.RefreshedAt, previousAt)
	currentPlaceholder := quotaWindowHasPlaceholderReset(currentWindow, current.RefreshedAt, currentAt)
	refilled := previousWindow.UsedPercent-currentWindow.UsedPercent >= quotaEpochResetRisePercent
	// A never-used account exposes a reset_at value that slides forward with
	// every observation. That placeholder is not a provider reset event.
	if previousPlaceholder && currentPlaceholder && !refilled {
		return "", false
	}
	anchorAdvanced := !previousWindow.ResetAt.IsZero() && !currentWindow.ResetAt.IsZero() &&
		!previousPlaceholder && currentWindow.ResetAt.After(previousWindow.ResetAt.Add(2*time.Minute))
	expiredRolled := !previousWindow.ResetAt.IsZero() && !previousWindow.ResetAt.After(now) &&
		!previousPlaceholder && currentWindow.ResetAt.After(now)
	if refilled && (currentWindow.ResetAt.IsZero() || currentWindow.ResetAt.After(now)) {
		return "weekly_refill", true
	}
	if (anchorAdvanced || expiredRolled) && currentWindow.UsedPercent <= previousWindow.UsedPercent+2 {
		return "weekly_reset_anchor", true
	}
	return "", false
}

func quotaEpochResetMatchesCurrent(state quotaEpochState, current quotaSnapshot, now time.Time) bool {
	if state.ID == "" || state.ConfirmedAt.IsZero() || state.ResetAt.IsZero() ||
		now.Before(state.ConfirmedAt) || now.Sub(state.ConfirmedAt) > quotaEpochResetCatchupAge {
		return false
	}
	window, ok := quotaEpochWeeklyWindow(current)
	if !ok || window.ResetAt.IsZero() {
		return false
	}
	delta := window.ResetAt.Sub(state.ResetAt)
	if delta < 0 {
		delta = -delta
	}
	return delta <= quotaEpochResetAnchorSkew
}

func (s *schedulerRuntimeState) beginQuotaEpochSweepLocked(inventory map[string]cpaAuthFileEntry, reason, triggerAuthID, triggerReason string, now time.Time) bool {
	if len(inventory) == 0 {
		return false
	}
	resetSweep := reason == "provider_reset"
	forceProbe := false
	if s.quotaEpoch.Sweep.Active {
		if resetSweep && s.quotaEpoch.Sweep.Reason != "provider_reset" {
			s.quotaEpoch.Sweep.Reason = "provider_reset"
			s.quotaEpoch.Sweep.TriggerAuthID = strings.TrimSpace(triggerAuthID)
			s.quotaEpoch.Sweep.TriggerReason = strings.TrimSpace(triggerReason)
			s.quotaEpoch.Sweep.StartedAt = now
			s.quotaEpoch.Sweep.Deadline = now.Add(quotaEpochSweepWindow)
			s.quotaEpoch.Sweep.Round = 1
			s.quotaEpoch.Sweep.RoundStartedAt = now
			s.quotaEpoch.Sweep.NextRoundAt = time.Time{}
			s.quotaEpoch.Sweep.EpochID = ""
			for id, target := range s.quotaEpoch.Sweep.Targets {
				target.AttemptedAt = time.Time{}
				target.ObservedAt = time.Time{}
				target.Evidence = false
				target.EvidenceReason = ""
				target.Error = ""
				s.quotaEpoch.Sweep.Targets[id] = target
			}
			forceProbe = true
		}
	} else {
		s.quotaEpoch.Sweep = quotaEpochSweepState{
			Active:         true,
			Reason:         reason,
			TriggerAuthID:  strings.TrimSpace(triggerAuthID),
			TriggerReason:  strings.TrimSpace(triggerReason),
			StartedAt:      now,
			Deadline:       now.Add(quotaEpochSweepWindow),
			Round:          1,
			RoundStartedAt: now,
			Targets:        make(map[string]quotaEpochSweepTarget, len(inventory)),
		}
		forceProbe = true
	}
	if s.quotaEpoch.Sweep.Targets == nil {
		s.quotaEpoch.Sweep.Targets = make(map[string]quotaEpochSweepTarget, len(inventory))
	}
	for id := range s.quotaEpoch.Sweep.Targets {
		if _, exists := inventory[id]; !exists {
			delete(s.quotaEpoch.Sweep.Targets, id)
		}
	}
	for id, auth := range inventory {
		target := s.quotaEpoch.Sweep.Targets[id]
		newTarget := strings.TrimSpace(target.AuthID) == ""
		target.AuthID = id
		target.AuthIndex = strings.TrimSpace(auth.AuthIndex)
		s.quotaEpoch.Sweep.Targets[id] = target
		if forceProbe || newTarget {
			poll := s.quotaPolls[id]
			if !strings.Contains(poll.Error, "401") && !strings.Contains(poll.Error, "403") {
				poll.NextAt = time.Time{}
				s.quotaPolls[id] = poll
			}
		}
	}
	s.quotaEpoch.Sweep.Required = quotaEpochQuorum(len(s.quotaEpoch.Sweep.Targets))
	return true
}

func (s *schedulerRuntimeState) prepareQuotaEpochInventoryLocked(inventory map[string]cpaAuthFileEntry, now time.Time) bool {
	if s.quotaEpoch.Accounts == nil {
		s.quotaEpoch.Accounts = make(map[string]quotaEpochAccountState)
	}
	for id := range s.quotaEpoch.Accounts {
		if _, exists := inventory[id]; !exists {
			delete(s.quotaEpoch.Accounts, id)
		}
	}
	if s.quotaEpoch.ID == "" && !s.quotaEpoch.Sweep.Active {
		return s.beginQuotaEpochSweepLocked(inventory, "bootstrap", "", "", now)
	}
	if s.quotaEpoch.Sweep.Active {
		return s.beginQuotaEpochSweepLocked(inventory, s.quotaEpoch.Sweep.Reason, s.quotaEpoch.Sweep.TriggerAuthID, s.quotaEpoch.Sweep.TriggerReason, now)
	}
	missing := false
	for id := range inventory {
		if _, exists := s.quotaEpoch.Accounts[id]; !exists {
			missing = true
			break
		}
	}
	if missing {
		return s.beginQuotaEpochSweepLocked(inventory, "inventory_change", "", "", now)
	}
	return false
}

func (s *schedulerRuntimeState) quotaEpochProbeReasonLocked(authID string, now time.Time) string {
	authID = strings.TrimSpace(authID)
	if s.quotaEpoch.Sweep.Active && now.Before(s.quotaEpoch.Sweep.Deadline) &&
		(s.quotaEpoch.Sweep.NextRoundAt.IsZero() || !now.Before(s.quotaEpoch.Sweep.NextRoundAt)) {
		target, exists := s.quotaEpoch.Sweep.Targets[authID]
		roundStartedAt := s.quotaEpoch.Sweep.RoundStartedAt
		if roundStartedAt.IsZero() {
			roundStartedAt = s.quotaEpoch.Sweep.StartedAt
		}
		if exists && !target.Evidence && (target.AttemptedAt.IsZero() || target.AttemptedAt.Before(roundStartedAt)) {
			if s.quotaEpoch.Sweep.Reason == "provider_reset" {
				return "epoch_reset_sweep"
			}
			return "epoch_bootstrap"
		}
	}
	account, exists := s.quotaEpoch.Accounts[authID]
	needsObservation := account.ObservedAt.IsZero() || now.Before(account.ObservedAt) ||
		now.Sub(account.ObservedAt) > quotaEpochWarmupSnapshotAge
	if !exists || s.quotaEpoch.ID == "" || account.State != quotaEpochAccountPending || !needsObservation {
		return ""
	}
	if account.ProbeAttempts >= quotaEpochPendingProbeLimit {
		// A provider reset can leave one account on an old telemetry snapshot
		// through the fast follow-up probes. Admit one delayed catch-up probe
		// after the warmup snapshot horizon, but keep it inside the bounded
		// reset catch-up window so idle accounts do not resume periodic polling.
		if s.quotaEpoch.Reason != "provider_reset" || s.quotaEpoch.ConfirmedAt.IsZero() ||
			now.Before(s.quotaEpoch.ConfirmedAt) || now.Sub(s.quotaEpoch.ConfirmedAt) > quotaEpochResetCatchupAge ||
			account.LastProbeAt.IsZero() || now.Before(account.LastProbeAt.Add(quotaEpochWarmupSnapshotAge)) {
			return ""
		}
	}
	if !account.LastProbeAt.IsZero() && now.Before(account.LastProbeAt.Add(quotaEpochPendingProbeDelay)) {
		return ""
	}
	return "epoch_pending"
}

func (s *schedulerRuntimeState) recordQuotaEpochProbeAttemptLocked(authID, reason string, now time.Time) {
	authID = strings.TrimSpace(authID)
	if reason == "epoch_reset_sweep" || reason == "epoch_bootstrap" {
		target, exists := s.quotaEpoch.Sweep.Targets[authID]
		if exists {
			target.AttemptedAt = now
			target.Error = ""
			s.quotaEpoch.Sweep.Targets[authID] = target
		}
		return
	}
	if reason != "epoch_pending" {
		return
	}
	account, exists := s.quotaEpoch.Accounts[authID]
	needsObservation := account.ObservedAt.IsZero() || now.Before(account.ObservedAt) ||
		now.Sub(account.ObservedAt) > quotaEpochWarmupSnapshotAge
	if !exists || account.State != quotaEpochAccountPending || !needsObservation {
		return
	}
	account.ProbeAttempts++
	account.LastProbeAt = now
	s.quotaEpoch.Accounts[authID] = account
}

func quotaEpochAccountFromSnapshot(snapshot quotaSnapshot, now time.Time) quotaEpochAccountState {
	id := strings.TrimSpace(snapshot.AuthID)
	account := quotaEpochAccountState{
		AuthID: id, AuthIndex: strings.TrimSpace(snapshot.AuthIndex),
		ObservedAt: snapshot.RefreshedAt, State: quotaEpochAccountPending,
	}
	if _, needsWarmup := unstartedWarmupWindow(snapshot, now); !needsWarmup {
		account.State = quotaEpochAccountNatural
	}
	return account
}

func (s *schedulerRuntimeState) confirmQuotaEpochLocked(reason string, now time.Time) quotaEpochTransition {
	s.quotaEpoch.Sequence++
	resetAt := quotaEpochMedianResetLocked(s.quotas, s.quotaEpoch.Sweep.Targets, reason == "provider_reset")
	id := adqEpochID(resetAt, now)
	if id == s.quotaEpoch.ID {
		id = fmt.Sprintf("%s-%d", id, s.quotaEpoch.Sequence)
	}
	s.quotaEpoch.ID = id
	s.quotaEpoch.Reason = reason
	s.quotaEpoch.ConfirmedAt = now
	s.quotaEpoch.ResetAt = resetAt
	s.quotaEpoch.Accounts = make(map[string]quotaEpochAccountState, len(s.quotaEpoch.Sweep.Targets))
	for authID, target := range s.quotaEpoch.Sweep.Targets {
		account := quotaEpochAccountState{AuthID: authID, AuthIndex: target.AuthIndex, State: quotaEpochAccountPending}
		if snapshot, ok := s.quotas[authID]; ok && !target.ObservedAt.IsZero() && !target.ObservedAt.Before(s.quotaEpoch.Sweep.StartedAt) {
			if reason != "provider_reset" || target.Evidence {
				account = quotaEpochAccountFromSnapshot(snapshot, now)
			} else if _, needsWarmup := unstartedWarmupWindow(snapshot, now); needsWarmup {
				account.ObservedAt = snapshot.RefreshedAt
			}
		}
		if poll := s.quotaPolls[authID]; !poll.LastUsageAt.IsZero() && !poll.LastUsageAt.Before(s.quotaEpoch.Sweep.StartedAt) {
			account.State = quotaEpochAccountNatural
			account.LastUsageAt = poll.LastUsageAt
			account.Error = ""
		}
		s.quotaEpoch.Accounts[authID] = account
	}
	s.quotaEpoch.Sweep.EpochID = id
	return quotaEpochTransition{ID: id, Reason: reason}
}

func quotaEpochMedianResetLocked(quotas map[string]quotaSnapshot, targets map[string]quotaEpochSweepTarget, evidenceOnly bool) time.Time {
	values := make([]time.Time, 0, len(targets))
	for id, target := range targets {
		if target.ObservedAt.IsZero() || (evidenceOnly && !target.Evidence) {
			continue
		}
		if window, ok := quotaEpochWeeklyWindow(quotas[id]); ok && !window.ResetAt.IsZero() {
			values = append(values, window.ResetAt)
		}
	}
	if len(values) == 0 {
		return time.Time{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Before(values[j]) })
	return values[len(values)/2]
}

func quotaEpochSweepRoundProgress(sweep quotaEpochSweepState) (attempted, remaining int) {
	roundStartedAt := sweep.RoundStartedAt
	if roundStartedAt.IsZero() {
		roundStartedAt = sweep.StartedAt
	}
	for _, target := range sweep.Targets {
		if target.Evidence {
			continue
		}
		remaining++
		if !target.AttemptedAt.IsZero() && !target.AttemptedAt.Before(roundStartedAt) {
			attempted++
		}
	}
	return attempted, remaining
}

func (s *schedulerRuntimeState) scheduleNextQuotaEpochSweepRoundLocked(now time.Time) bool {
	sweep := &s.quotaEpoch.Sweep
	if !sweep.Active || sweep.Reason != "provider_reset" || sweep.Round >= quotaEpochSweepMaxRounds {
		return false
	}
	next := now.Add(quotaEpochSweepRoundDelay)
	if !sweep.Deadline.IsZero() && !next.Before(sweep.Deadline) {
		return false
	}
	sweep.Round++
	sweep.RoundStartedAt = next
	sweep.NextRoundAt = next
	for authID, target := range sweep.Targets {
		if target.Evidence {
			continue
		}
		poll := s.quotaPolls[authID]
		if poll.Error == "" || poll.NextAt.Before(next) {
			poll.NextAt = next
			s.quotaPolls[authID] = poll
		}
	}
	return true
}

func quotaEpochSweepCounts(sweep quotaEpochSweepState) (observed, evidence int) {
	for _, target := range sweep.Targets {
		if !target.ObservedAt.IsZero() && !target.ObservedAt.Before(sweep.StartedAt) {
			observed++
		}
		if target.Evidence {
			evidence++
		}
	}
	return observed, evidence
}

func (s *schedulerRuntimeState) finalizeQuotaEpochSweepLocked(now time.Time) quotaEpochTransition {
	sweep := &s.quotaEpoch.Sweep
	if !sweep.Active {
		return quotaEpochTransition{}
	}
	observed, evidence := quotaEpochSweepCounts(*sweep)
	var transition quotaEpochTransition
	if sweep.Reason == "provider_reset" && sweep.EpochID == "" && sweep.Required > 0 && evidence >= sweep.Required {
		transition = s.confirmQuotaEpochLocked("provider_reset", now)
	} else if (sweep.Reason == "bootstrap" || sweep.Reason == "inventory_change") && s.quotaEpoch.ID == "" &&
		observed > 0 && (observed == len(sweep.Targets) || !now.Before(sweep.Deadline)) {
		transition = s.confirmQuotaEpochLocked("bootstrap", now)
	} else if sweep.Reason == "inventory_change" && s.quotaEpoch.ID != "" {
		for authID, target := range sweep.Targets {
			if _, exists := s.quotaEpoch.Accounts[authID]; exists || target.ObservedAt.IsZero() {
				continue
			}
			if snapshot, ok := s.quotas[authID]; ok {
				s.quotaEpoch.Accounts[authID] = quotaEpochAccountFromSnapshot(snapshot, now)
			}
		}
	}
	roundAttempted, roundRemaining := quotaEpochSweepRoundProgress(*sweep)
	roundComplete := roundRemaining == 0 || roundAttempted == roundRemaining
	if transition.ID == "" && sweep.Reason == "provider_reset" && now.Before(sweep.Deadline) && roundComplete &&
		s.scheduleNextQuotaEpochSweepRoundLocked(now) {
		return transition
	}
	if transition.ID != "" || !now.Before(sweep.Deadline) ||
		(sweep.Reason != "provider_reset" && observed == len(sweep.Targets)) {
		sweep.Active = false
		sweep.CompletedAt = now
		sweep.NextRoundAt = time.Time{}
	}
	return transition
}

func (s *schedulerRuntimeState) observeQuotaEpochProbeLocked(authID string, previous, current quotaSnapshot, inventory map[string]cpaAuthFileEntry, now time.Time) quotaEpochTransition {
	authID = strings.TrimSpace(authID)
	reason, resetEvidence := quotaEpochResetEvidence(previous, current, now)
	newEpochEvidence := resetEvidence && !quotaEpochResetMatchesCurrent(s.quotaEpoch, current, now)
	if newEpochEvidence && (!s.quotaEpoch.Sweep.Active || s.quotaEpoch.Sweep.Reason != "provider_reset") {
		s.beginQuotaEpochSweepLocked(inventory, "provider_reset", authID, reason, now)
	}
	if target, exists := s.quotaEpoch.Sweep.Targets[authID]; exists {
		target.AuthID = authID
		target.AuthIndex = strings.TrimSpace(current.AuthIndex)
		target.ObservedAt = current.RefreshedAt
		target.Error = ""
		if newEpochEvidence {
			target.Evidence = true
			target.EvidenceReason = reason
		}
		s.quotaEpoch.Sweep.Targets[authID] = target
	}
	if account, exists := s.quotaEpoch.Accounts[authID]; exists && !current.RefreshedAt.Before(s.quotaEpoch.ConfirmedAt) {
		updated := account
		canAdoptSnapshot := s.quotaEpoch.Reason != "provider_reset" || account.State != quotaEpochAccountPending || resetEvidence
		if !canAdoptSnapshot {
			_, canAdoptSnapshot = unstartedWarmupWindow(current, now)
		}
		if canAdoptSnapshot {
			updated = quotaEpochAccountFromSnapshot(current, now)
			updated.LastUsageAt = account.LastUsageAt
			updated.LastWarmEpoch = account.LastWarmEpoch
			updated.LastWarmAt = account.LastWarmAt
			updated.ProbeAttempts = account.ProbeAttempts
			updated.LastProbeAt = account.LastProbeAt
			if account.State == quotaEpochAccountWarmed || account.State == quotaEpochAccountBlocked {
				updated.State = account.State
				updated.Error = account.Error
			}
		} else {
			updated.ObservedAt = time.Time{}
			updated.Error = ""
		}
		s.quotaEpoch.Accounts[authID] = updated
	}
	return s.finalizeQuotaEpochSweepLocked(now)
}

func (s *schedulerRuntimeState) observeQuotaEpochProbeErrorLocked(authID, code string, now time.Time) quotaEpochTransition {
	authID = strings.TrimSpace(authID)
	if target, exists := s.quotaEpoch.Sweep.Targets[authID]; exists {
		target.Error = strings.TrimSpace(code)
		s.quotaEpoch.Sweep.Targets[authID] = target
	}
	if account, exists := s.quotaEpoch.Accounts[authID]; exists && account.State == quotaEpochAccountPending && account.ObservedAt.IsZero() {
		account.Error = strings.TrimSpace(code)
		s.quotaEpoch.Accounts[authID] = account
	}
	return s.finalizeQuotaEpochSweepLocked(now)
}

func (s *schedulerRuntimeState) beginQuotaEpochResetFromHeadersLocked(authID string, previous, current quotaSnapshot, now time.Time) bool {
	reason, evidence := quotaEpochResetEvidence(previous, current, now)
	if !evidence || quotaEpochResetMatchesCurrent(s.quotaEpoch, current, now) {
		return false
	}
	inventory := make(map[string]cpaAuthFileEntry)
	seen := make(map[string]struct{})
	for _, snapshot := range s.quotas {
		id := strings.TrimSpace(snapshot.AuthID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		inventory[id] = cpaAuthFileEntry{ID: id, AuthIndex: strings.TrimSpace(snapshot.AuthIndex)}
	}
	if !s.beginQuotaEpochSweepLocked(inventory, "provider_reset", authID, "header_"+reason, now) {
		return false
	}
	if target, exists := s.quotaEpoch.Sweep.Targets[strings.TrimSpace(authID)]; exists {
		target.Evidence = true
		target.EvidenceReason = "header_" + reason
		s.quotaEpoch.Sweep.Targets[strings.TrimSpace(authID)] = target
	}
	return true
}

func (s *schedulerRuntimeState) markQuotaEpochNatural(authID, authIndex string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	authID = s.canonicalAuthIDLocked(authID, authIndex)
	account, exists := s.quotaEpoch.Accounts[authID]
	if !exists || s.quotaEpoch.ID == "" {
		return false
	}
	stateChanged := account.State == quotaEpochAccountPending
	if stateChanged {
		account.State = quotaEpochAccountNatural
		account.Error = ""
	}
	if now.After(account.LastUsageAt) {
		account.LastUsageAt = now
	}
	s.quotaEpoch.Accounts[authID] = account
	return stateChanged
}

func (s *schedulerRuntimeState) recordQuotaEpochWarmupOutcome(authID, epochID string, success, blocked bool, code string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if epochID == "" || epochID != s.quotaEpoch.ID {
		return false
	}
	authID = s.canonicalAuthIDLocked(authID, "")
	account, exists := s.quotaEpoch.Accounts[authID]
	if !exists {
		return false
	}
	switch {
	case success:
		account.State = quotaEpochAccountWarmed
		account.LastWarmEpoch = epochID
		account.LastWarmAt = now
		account.Error = ""
	case blocked:
		account.State = quotaEpochAccountBlocked
		account.Error = strings.TrimSpace(code)
	default:
		account.State = quotaEpochAccountPending
		account.Error = strings.TrimSpace(code)
	}
	s.quotaEpoch.Accounts[authID] = account
	return true
}

func (s *schedulerRuntimeState) quotaEpochWarmupAllowanceLocked(snapshot quotaSnapshot, poll quotaPollState, now time.Time) (string, bool) {
	return quotaEpochWarmupAllowance(s.quotaEpoch, snapshot, poll, now)
}

func quotaEpochWarmupAllowance(state quotaEpochState, snapshot quotaSnapshot, poll quotaPollState, now time.Time) (string, bool) {
	if state.ID == "" {
		return "", false
	}
	account, exists := state.Accounts[strings.TrimSpace(snapshot.AuthID)]
	if !exists || account.State != quotaEpochAccountPending || account.ObservedAt.IsZero() ||
		account.AuthIndex != strings.TrimSpace(snapshot.AuthIndex) || !account.ObservedAt.Equal(snapshot.RefreshedAt) ||
		now.Before(account.ObservedAt) || now.Sub(account.ObservedAt) > quotaEpochWarmupSnapshotAge ||
		poll.Error != "" || poll.LastUsageAt.After(account.ObservedAt) {
		return "", false
	}
	return state.ID, true
}

func (s *schedulerRuntimeState) quotaEpochHasPendingLocked() bool {
	for _, account := range s.quotaEpoch.Accounts {
		if account.State == quotaEpochAccountPending {
			return true
		}
	}
	return false
}

func quotaEpochHardWarmupBlock(entry warmupEntry) bool {
	code := strings.ToLower(strings.TrimSpace(entry.Error))
	if canonicalNonRetryableWarmupCode(code) != "" {
		return true
	}
	switch code {
	case "http_401", "http_403", "invalid_warmup_model", "invalid_warmup_request", "invalid_management_url":
		return true
	default:
		return false
	}
}

func (s *schedulerRuntimeState) applyQuotaEpochTransition(transition quotaEpochTransition) {
	if transition.ID == "" {
		return
	}
	blocked := make(map[string]string)
	s.mu.RLock()
	targets := make(map[string]quotaEpochAccountState, len(s.quotaEpoch.Accounts))
	if s.quotaEpoch.ID == transition.ID {
		for id, account := range s.quotaEpoch.Accounts {
			targets[id] = account
		}
	}
	s.mu.RUnlock()
	s.warmupMu.Lock()
	for key, entry := range s.warmups {
		account, target := targets[strings.TrimSpace(entry.AuthID)]
		if !target {
			continue
		}
		if entry.Blocked && quotaEpochHardWarmupBlock(entry) {
			blocked[strings.TrimSpace(entry.AuthID)] = entry.Error
			continue
		}
		clear := transition.Reason != "bootstrap"
		if !clear && account.State == quotaEpochAccountPending && entry.EpochID != transition.ID {
			revision := warmupEntryRevisionTime(entry)
			clear = revision.IsZero() || (!account.ObservedAt.IsZero() && account.ObservedAt.Sub(revision) >= warmupStaleActivationGrace)
		}
		if clear {
			delete(s.warmups, key)
		}
	}
	if transition.Reason != "bootstrap" {
		attempts := s.warmupAttempts[:0]
		for _, attempt := range s.warmupAttempts {
			if _, target := targets[strings.TrimSpace(attempt.AuthID)]; !target {
				attempts = append(attempts, attempt)
			}
		}
		s.warmupAttempts = attempts
	}
	s.warmupMu.Unlock()
	if len(blocked) > 0 {
		s.mu.Lock()
		if s.quotaEpoch.ID == transition.ID {
			for authID, code := range blocked {
				account := s.quotaEpoch.Accounts[authID]
				account.State = quotaEpochAccountBlocked
				account.Error = code
				s.quotaEpoch.Accounts[authID] = account
			}
		}
		s.mu.Unlock()
	}
	s.persistBanState()
}

func (s *schedulerRuntimeState) requestQuotaEpochRefresh() {
	if !s.admitBackgroundWorker() {
		return
	}
	go func() {
		defer s.wg.Done()
		s.refreshOnce(context.Background())
	}()
}

func runtimeQuotaEpoch(state quotaEpochState) runtimeQuotaEpochStatus {
	out := runtimeQuotaEpochStatus{
		ID: state.ID, Sequence: state.Sequence, Reason: state.Reason, SweepActive: state.Sweep.Active,
		SweepReason: state.Sweep.Reason, SweepTargets: len(state.Sweep.Targets),
		SweepRequired: state.Sweep.Required, SweepRound: state.Sweep.Round, SweepMaxRounds: quotaEpochSweepMaxRounds,
	}
	if !state.ConfirmedAt.IsZero() {
		out.ConfirmedAt = state.ConfirmedAt.Format(time.RFC3339)
	}
	if !state.ResetAt.IsZero() {
		out.ResetAt = state.ResetAt.Format(time.RFC3339)
	}
	if !state.Sweep.StartedAt.IsZero() {
		out.SweepStartedAt = state.Sweep.StartedAt.Format(time.RFC3339)
	}
	if !state.Sweep.Deadline.IsZero() {
		out.SweepDeadline = state.Sweep.Deadline.Format(time.RFC3339)
	}
	if !state.Sweep.NextRoundAt.IsZero() {
		out.SweepNextRound = state.Sweep.NextRoundAt.Format(time.RFC3339)
	}
	out.SweepObserved, out.SweepEvidence = quotaEpochSweepCounts(state.Sweep)
	for _, account := range state.Accounts {
		item := runtimeQuotaEpochAccountStatus{
			AuthID: account.AuthID, AuthIndex: account.AuthIndex, State: account.State,
			LastWarmEpoch: account.LastWarmEpoch, ProbeAttempts: account.ProbeAttempts, Error: account.Error,
		}
		if !account.ObservedAt.IsZero() {
			item.ObservedAt = account.ObservedAt.Format(time.RFC3339)
		}
		if !account.LastUsageAt.IsZero() {
			item.LastUsageAt = account.LastUsageAt.Format(time.RFC3339)
		}
		if !account.LastWarmAt.IsZero() {
			item.LastWarmAt = account.LastWarmAt.Format(time.RFC3339)
		}
		if !account.LastProbeAt.IsZero() {
			item.LastProbeAt = account.LastProbeAt.Format(time.RFC3339)
		}
		switch account.State {
		case quotaEpochAccountPending:
			out.Pending++
		case quotaEpochAccountNatural:
			out.Natural++
		case quotaEpochAccountWarmed:
			out.Warmed++
		case quotaEpochAccountBlocked:
			out.Blocked++
		}
		out.Accounts = append(out.Accounts, item)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].AuthID < out.Accounts[j].AuthID })
	return out
}

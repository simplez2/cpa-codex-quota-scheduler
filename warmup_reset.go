package main

import "time"

const warmupResetConfirmationDelay = 30 * time.Second

// Two independent native observations must keep moving a full, unused window
// forward. A fixed reset anchor with rounded 0% usage is already running.
type warmupResetCheck struct {
	Revision   time.Time
	ObservedAt time.Time
	ResetAt    time.Time
}

func warmupEntryResetConfirmed(entry warmupEntry) bool {
	return !entry.ResetConfirmedAt.IsZero() && entry.ResetConfirmedAt.After(entry.AttemptedAt) &&
		entry.ResetConfirmedAt.After(entry.CompletedAt) && entry.ResetConfirmedAt.After(entry.ActivatedAt) &&
		entry.ResetConfirmedAt.After(entry.OutcomeAt)
}

func warmupResetWindow(entry warmupEntry, q quotaSnapshot, now time.Time, grace time.Duration) (quotaWindow, bool) {
	if entry.Blocked || entry.Error != "" || entry.Failures != 0 || entry.Status < 200 || entry.Status >= 300 ||
		(entry.DispatchState != "" && entry.DispatchState != "completed") || warmupEntryResetConfirmed(entry) ||
		entry.CompletedAt.IsZero() || entry.ActivatedAt.IsZero() || entry.ResetAt.IsZero() ||
		entry.AuthID != q.AuthID || entry.AuthIndex == "" || entry.AuthIndex != q.AuthIndex ||
		q.RefreshedAt.IsZero() || now.Before(q.RefreshedAt) {
		return quotaWindow{}, false
	}
	grace = max(grace, warmupStaleActivationGrace)
	if q.RefreshedAt.Before(warmupEntryRevisionTime(entry).Add(grace)) {
		return quotaWindow{}, false
	}
	for _, w := range q.Windows {
		if normalizeWindowClass(w.Class) != normalizeWindowClass(entry.Window) {
			continue
		}
		if w.Source != quotaSourceProbe || !w.ObservedAt.Equal(q.RefreshedAt) ||
			!w.Allowed || w.LimitReached || !quotaWindowHasPlaceholderReset(w, q.RefreshedAt, now) ||
			!resetAnchorDiffers(entry.ResetAt, w.ResetAt) {
			return quotaWindow{}, false
		}
		// Both the countdown and absolute anchor must describe a full window.
		// Inconsistent provider data cannot erase an execution receipt.
		delta := w.ResetAt.Sub(w.ObservedAt.Add(time.Duration(w.WindowSeconds) * time.Second))
		if delta < -warmupResetPlaceholderSkew || delta > warmupResetPlaceholderSkew {
			return quotaWindow{}, false
		}
		return w, true
	}
	return quotaWindow{}, false
}

// Called with warmupMu held. This admits quota reads only, never generation.
func (s *schedulerRuntimeState) warmupResetProbeDueLocked(q quotaSnapshot, poll quotaPollState, cfg pluginConfig, now time.Time) bool {
	if poll.Failures != 0 || poll.Error != "" || s.warmupSuppressedAccountsLocked(now, cfg.WarmupRetryAfter)[q.AuthID] {
		return false
	}
	for key, entry := range s.warmups {
		if _, ok := warmupResetWindow(entry, q, now, cfg.WarmupRetryAfter); !ok {
			continue
		}
		interval := max(cfg.WarmupMinInterval, 15*time.Minute)
		if check, ok := s.warmupResetChecks[key]; ok && check.Revision.Equal(warmupEntryRevisionTime(entry)) {
			interval = warmupResetConfirmationDelay
		}
		if !now.Before(poll.AttemptedAt.Add(interval)) {
			return true
		}
	}
	return false
}

func (s *schedulerRuntimeState) reconcileWarmupReset(q quotaSnapshot, now time.Time) bool {
	s.mu.RLock()
	cfg := s.cfg
	current := s.quotas[q.AuthID]
	poll := s.quotaPolls[q.AuthID]
	s.mu.RUnlock()
	// A newer concurrent usage/header or a partial carried-forward response
	// takes precedence over the native read used to confirm this reset.
	if !cfg.WarmupEnabled || !warmupQuotaHasHeadroom(q, cfg) || !warmupSnapshotFresh(q, now, cfg.StaleAfter) ||
		current.AuthIndex != q.AuthIndex || current.RefreshedAt.After(q.RefreshedAt) ||
		poll.LastUsageAt.After(q.RefreshedAt) || poll.Error != "" {
		return false
	}
	if _, banned := banStore.lookup(q.AuthID); banned {
		return false
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupRunning || s.warmupSuppressedAccountsLocked(now, cfg.WarmupRetryAfter)[q.AuthID] {
		return false
	}
	if s.warmupResetChecks == nil {
		s.warmupResetChecks = make(map[string]warmupResetCheck)
	}
	changed := false
	for key, entry := range s.warmups {
		if entry.AuthID != q.AuthID {
			continue
		}
		w, ok := warmupResetWindow(entry, q, now, cfg.WarmupRetryAfter)
		if !ok {
			delete(s.warmupResetChecks, key)
			continue
		}
		revision := warmupEntryRevisionTime(entry)
		previous, exists := s.warmupResetChecks[key]
		if !exists || !previous.Revision.Equal(revision) || q.RefreshedAt.Sub(previous.ObservedAt) > quotaEpochSweepWindow {
			s.warmupResetChecks[key] = warmupResetCheck{Revision: revision, ObservedAt: q.RefreshedAt, ResetAt: w.ResetAt}
			continue
		}
		gap := q.RefreshedAt.Sub(previous.ObservedAt)
		if gap < warmupResetConfirmationDelay || w.ResetAt.Sub(previous.ResetAt) < gap-warmupResetPlaceholderSkew {
			continue
		}
		// Retain a newer receipt until admission overwrites it. Simply deleting
		// would let an old cross-generation outcome journal resurrect suppression.
		entry.ResetConfirmedAt = q.RefreshedAt
		s.warmups[key] = entry
		delete(s.warmupResetChecks, key)
		changed = true
	}
	return changed
}

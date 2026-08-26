package main

import (
	"strings"
	"time"
)

// pendingWarmupKeeperRefreshTargets closes the confirmation loop after a
// successful warmup that did not return authoritative quota reset headers.
//
// Each pending quota class must have its own post-warmup Keeper observation and
// a confirmable fixed reset anchor. A newer 5h row cannot confirm weekly, and a
// newer weekly row cannot confirm 5h. This matters for partial Keeper cache
// responses where the snapshot envelope may be new while one sibling window is
// still missing, carried, or still exposing a moving full-duration placeholder.
//
// Until that proof exists, the old reset anchor for the pending class is masked
// from both the raw refresh snapshot and the previous runtime snapshot. That is
// deliberate fail-closed behavior: confirmPendingWarmups cannot accidentally
// bless a stale carried weekly reset merely because a fresh 5h row made the
// outer snapshot newer. The existing Keeper refresh gate still controls
// duplicate/backoff behavior, and this path never sends another model request.
func (s *schedulerRuntimeState) pendingWarmupKeeperRefreshTargets(indexes []string, current map[string]quotaSnapshot) []keeperRefreshTarget {
	activeIndexes := make(map[string]struct{}, len(indexes))
	for _, rawIndex := range indexes {
		if index := strings.TrimSpace(rawIndex); index != "" {
			activeIndexes[index] = struct{}{}
		}
	}

	s.warmupMu.Lock()
	entries := make([]warmupEntry, 0, len(s.warmups))
	for _, entry := range s.warmups {
		entries = append(entries, entry)
	}
	s.warmupMu.Unlock()

	now := time.Now()
	targets := make([]keeperRefreshTarget, 0)
	seenTarget := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Blocked || entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		class := normalizeWindowClass(entry.Window)
		if index == "" || class == "" {
			continue
		}
		if _, active := activeIndexes[index]; !active {
			continue
		}

		snapshot, ok := current[index]
		if ok && warmupEntryHasConfirmablePostCompletionObservation(entry, snapshot, now) {
			continue
		}

		// The pending class is not yet independently proven. Prevent an old
		// carried ResetAt from being consumed by confirmPendingWarmups later in
		// this refresh cycle, then ask Keeper for a newer quota-only observation.
		maskUnconfirmedWarmupReset(current, index, class, entry.CompletedAt)
		s.maskRuntimeWarmupReset(index, class, entry.CompletedAt)

		if _, duplicate := seenTarget[index]; duplicate {
			continue
		}
		observedAt := time.Time{}
		if ok {
			observedAt = snapshot.RefreshedAt
		}
		targets = append(targets, keeperRefreshTarget{
			AuthIndex:  index,
			Reason:     "warmup_pending_confirmation",
			ObservedAt: observedAt,
		})
		seenTarget[index] = struct{}{}
	}
	return targets
}

func warmupEntryHasConfirmablePostCompletionObservation(entry warmupEntry, snapshot quotaSnapshot, now time.Time) bool {
	class := normalizeWindowClass(entry.Window)
	if class == "" || entry.CompletedAt.IsZero() {
		return false
	}
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != class {
			continue
		}
		// A snapshot-level refreshed_at cannot prove that this particular quota
		// class was observed in a partial-cache response. Normalized Keeper and
		// response-header rows carry an explicit per-window ObservedAt.
		observedAt := window.ObservedAt
		if observedAt.IsZero() || !observedAt.After(entry.CompletedAt) {
			return false
		}
		if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
			return false
		}
		if quotaWindowHasPlaceholderReset(window, observedAt, now) {
			return false
		}
		return quotaWindowCycleStarted(window, observedAt, now)
	}
	return false
}

func maskUnconfirmedWarmupReset(snapshots map[string]quotaSnapshot, authIndex, class string, completedAt time.Time) {
	authIndex = strings.TrimSpace(authIndex)
	class = normalizeWindowClass(class)
	if authIndex == "" || class == "" || completedAt.IsZero() {
		return
	}
	for key, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.AuthIndex) != authIndex && strings.TrimSpace(key) != authIndex {
			continue
		}
		changed := false
		windows := append([]quotaWindow(nil), snapshot.Windows...)
		for i := range windows {
			if normalizeWindowClass(windows[i].Class) != class {
				continue
			}
			observedAt := windows[i].ObservedAt
			if !observedAt.IsZero() && observedAt.After(completedAt) {
				continue
			}
			windows[i].ResetAt = time.Time{}
			windows[i].ResetAfterSeconds = 0
			windows[i].ResetAfterSecondsKnown = false
			changed = true
		}
		if changed {
			snapshot.Windows = windows
			snapshots[key] = snapshot
		}
	}
}

func (s *schedulerRuntimeState) maskRuntimeWarmupReset(authIndex, class string, completedAt time.Time) {
	s.mu.Lock()
	maskUnconfirmedWarmupReset(s.quotas, authIndex, class, completedAt)
	s.mu.Unlock()
}

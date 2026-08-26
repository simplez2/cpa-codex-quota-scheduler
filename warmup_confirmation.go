package main

import (
	"strings"
	"time"
)

// pendingWarmupKeeperRefreshTargets closes the confirmation loop after a
// successful warmup that did not return authoritative quota reset headers.
//
// Each pending quota class must have its own Keeper observation newer than the
// completed model request. A newer 5h row cannot confirm weekly, and a newer
// weekly row cannot confirm 5h. This matters for partial Keeper cache responses
// where the snapshot envelope may be new while one sibling window is still
// missing or carried from an older observation.
//
// This helper never issues model traffic. It only asks the existing Keeper
// refresh path for a newer quota observation, and the normal cross-instance
// refresh gate still controls duplicate/backoff behavior.
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

	targets := make([]keeperRefreshTarget, 0)
	seen := make(map[string]struct{})
	for _, entry := range entries {
		if entry.Blocked || entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() {
			continue
		}
		index := strings.TrimSpace(entry.AuthIndex)
		if index == "" {
			continue
		}
		if _, active := activeIndexes[index]; !active {
			continue
		}
		if _, duplicate := seen[index]; duplicate {
			continue
		}

		snapshot, ok := current[index]
		if ok && warmupEntryHasPostCompletionObservation(entry, snapshot) {
			// This quota class already has a post-warmup observation. The normal
			// confirmPendingWarmups pass will either confirm its fixed reset
			// anchor or leave it under the existing retry/grace policy. Other
			// pending classes on the same auth are still evaluated independently.
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
		seen[index] = struct{}{}
	}
	return targets
}

func warmupEntryHasPostCompletionObservation(entry warmupEntry, snapshot quotaSnapshot) bool {
	class := normalizeWindowClass(entry.Window)
	if class == "" || entry.CompletedAt.IsZero() {
		return false
	}
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) != class {
			continue
		}
		observedAt := window.ObservedAt
		if observedAt.IsZero() {
			observedAt = snapshot.RefreshedAt
		}
		return !observedAt.IsZero() && observedAt.After(entry.CompletedAt)
	}
	return false
}

package main

import (
	"strings"
	"time"
)

// pendingWarmupKeeperRefreshTargets closes the confirmation loop after a
// successful warmup that did not return authoritative quota reset headers.
//
// A pending warmup must be confirmed by a Keeper observation newer than the
// completed model request. Otherwise the regular /quota/cache row may remain a
// pre-warmup placeholder for up to stale_after, making 5h/weekly activation
// appear stuck even though the warmup request succeeded.
//
// This helper never issues model traffic. It only asks the existing Keeper
// refresh path for one newer quota observation, and the normal cross-instance
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
		if ok && !snapshot.RefreshedAt.IsZero() && snapshot.RefreshedAt.After(entry.CompletedAt) {
			// A post-warmup Keeper observation already exists. The normal
			// confirmPendingWarmups pass will either confirm its fixed reset
			// anchor or leave the entry under the existing retry/grace policy.
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

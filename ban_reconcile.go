package main

import (
	"log/slog"
	"sort"
	"strings"
	"time"
)

// banResetConfirmation persists the two-snapshot proof used to distinguish a
// genuinely newer quota cycle from a stale Keeper row. It contains only quota
// timestamps and the CPA auth id; no credential material is stored.
type banResetConfirmation struct {
	AuthID          string    `json:"auth_id"`
	BannedAt        time.Time `json:"banned_at"`
	BanResetAt      time.Time `json:"ban_reset_at"`
	FirstSnapshotAt time.Time `json:"first_snapshot_at"`
	LastSnapshotAt  time.Time `json:"last_snapshot_at"`
	Confirmations   int       `json:"confirmations"`
	Reason          string    `json:"reason"`
}

func (s *schedulerRuntimeState) reconcileExternallyResetQuotaBans(quotas map[string]quotaSnapshot, now time.Time) map[string]struct{} {
	s.mu.RLock()
	staleAfter := s.cfg.StaleAfter
	warmupRetryAfter := s.cfg.WarmupRetryAfter
	s.mu.RUnlock()
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}

	canonical := make(map[string]quotaSnapshot)
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			continue
		}
		if previous, ok := canonical[authID]; !ok || snapshot.RefreshedAt.After(previous.RefreshedAt) {
			canonical[authID] = snapshot
		}
	}

	cleared := make(map[string]struct{})
	stateChanged := false
	for authID, snapshot := range canonical {
		entry, banned := banStore.lookup(authID)
		entry = normalizeBanEntry(entry)
		if !banned || entry.Kind != banKindQuota || entry.Phase != banPhaseCooldown || entry.BannedAt.IsZero() ||
			snapshot.RefreshedAt.IsZero() || now.Before(snapshot.RefreshedAt) ||
			now.Sub(snapshot.RefreshedAt) > staleAfter || !snapshot.RefreshedAt.After(entry.BannedAt) {
			if s.dropBanResetConfirmation(authID) {
				stateChanged = true
			}
			continue
		}

		reason, evidenceAt, evidence := quotaSnapshotProvesNewQuotaCycle(snapshot, entry, now, staleAfter)
		if !evidence {
			if s.dropBanResetConfirmation(authID) {
				stateChanged = true
			}
			continue
		}

		confirmation, advanced := s.advanceBanResetConfirmation(authID, entry, evidenceAt, reason, now, staleAfter)
		if advanced {
			stateChanged = true
			s.mu.Lock()
			s.banResetConfirmationEvents++
			s.mu.Unlock()
			slog.Info("codex-quota-scheduler: observed fresh new-cycle evidence for quarantined credential",
				"auth_id", authID,
				"confirmations", confirmation.Confirmations,
				"reason", reason,
				"window_observed_at", evidenceAt.Format(time.RFC3339))
		}
		if confirmation.Confirmations < 2 {
			continue
		}
		if s.recentWarmup429(authID, now, warmupRetryAfter) {
			continue
		}

		current, stillBanned := banStore.lookup(authID)
		if !stillBanned || !sameQuotaBan(current, entry) {
			continue
		}
		if _, removed := banStore.clear(authID); !removed {
			continue
		}
		s.dropBanResetConfirmation(authID)
		s.clearWarmupStateForAuth(authID)
		cleared[authID] = struct{}{}
		stateChanged = true
		s.mu.Lock()
		s.banExternalResetClears++
		s.lastBanClearReason = "external_new_cycle"
		s.lastBanClearAt = now
		s.mu.Unlock()
		slog.Info("codex-quota-scheduler: cleared stale quota cooldown after external new cycle",
			"auth_id", authID,
			"ban_cleared_reason", "external_new_cycle",
			"window_observed_at", evidenceAt.Format(time.RFC3339))
	}
	if stateChanged {
		s.persistBanState()
	}
	return cleared
}

func (s *schedulerRuntimeState) recentWarmup429(authID string, now time.Time, retryAfter time.Duration) bool {
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	authID = strings.TrimSpace(authID)
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	for key, entry := range s.warmups {
		if strings.TrimSpace(entry.AuthID) != authID && !strings.HasPrefix(key, authID+"|") {
			continue
		}
		if entry.Status == statusTooManyRequests && !entry.AttemptedAt.IsZero() && now.Sub(entry.AttemptedAt) < retryAfter {
			return true
		}
	}
	return false
}

func sameQuotaBan(a, b banEntry) bool {
	a = normalizeBanEntry(a)
	b = normalizeBanEntry(b)
	return a.Kind == banKindQuota && b.Kind == banKindQuota && a.Phase == banPhaseCooldown && b.Phase == banPhaseCooldown &&
		a.BannedAt.Equal(b.BannedAt) && a.ResetAt.Equal(b.ResetAt)
}

// quotaSnapshotProvesNewQuotaCycle deliberately does not require 0% usage.
// Clearing a stale quota ban and warming an untouched account are different
// operations: an account can already be a few percent into a newer cycle and
// still prove that an older ban is obsolete. Warmup eligibility remains strict
// (0% used / zero known usage credits) in warmup.go.
func quotaSnapshotProvesNewQuotaCycle(snapshot quotaSnapshot, entry banEntry, now time.Time, staleAfter time.Duration) (string, time.Time, bool) {
	if len(snapshot.Windows) == 0 {
		return "", time.Time{}, false
	}
	banClass := effectiveBanWindowClass(entry)
	if banClass == "" || entry.ResetAt.IsZero() {
		return "", time.Time{}, false
	}
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class != banClass {
			continue
		}
		observedAt := window.ObservedAt
		// The window's own observation time is the freshness proof. Falling
		// back to the outer snapshot timestamp would let Keeper re-emit one
		// merged/stale window under a newer envelope and count it as an
		// independent reset observation.
		if observedAt.IsZero() || !observedAt.After(entry.BannedAt) || now.Before(observedAt) || now.Sub(observedAt) > staleAfter {
			return "", time.Time{}, false
		}
		if !window.Allowed || window.LimitReached || window.UsedPercent >= usedPercentThreshold ||
			window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
			return "", time.Time{}, false
		}
		// A later reset anchor for the effective ban window is the bounded proof
		// that the old cooldown belongs to an earlier cycle. Requiring strictly
		// later (rather than merely different) anchors prevents a stale or
		// regressed monthly row from clearing a real 5h/weekly ban.
		if window.ResetAt.After(entry.ResetAt.Add(warmupResetPlaceholderSkew)) {
			return banClass + "_reset_anchor_advanced", observedAt, true
		}
		return "", time.Time{}, false
	}
	return "", time.Time{}, false
}

// effectiveBanWindowClass repairs only provable historical under-classification.
// Older releases could label every reset as 5h. A remaining span longer than a
// 5h window proves that label cannot be literal and may be promoted. A 5h label
// with <=6h remaining is ambiguous: it may be a real 5h ban or a weekly/monthly
// ban observed near cycle end, so external reconciliation must fail closed.
// Explicit weekly/monthly header classes remain authoritative and are never
// inferred upward from their remaining time.
func effectiveBanWindowClass(entry banEntry) string {
	declared := banWindowClass(entry.Window)
	if entry.BannedAt.IsZero() || entry.ResetAt.IsZero() || !entry.ResetAt.After(entry.BannedAt) {
		return ""
	}
	inferred := windowClassFromSeconds(int64(entry.ResetAt.Sub(entry.BannedAt) / time.Second))
	switch declared {
	case "weekly", "monthly":
		return declared
	case "5h":
		if windowClassRank(inferred) > windowClassRank(declared) {
			return inferred
		}
	}
	return ""
}

func windowClassRank(class string) int {
	switch normalizeWindowClass(class) {
	case "5h":
		return 1
	case "weekly":
		return 2
	case "monthly":
		return 3
	default:
		return 0
	}
}

// pendingBanResetKeeperRefreshTargets returns only first-confirmation bans.
// One precise Keeper refresh is enough to obtain an independent window
// observation; confirmations at two or more are never kept refreshing while a
// separate warmup-429 guard or another safety condition delays the clear.
func (s *schedulerRuntimeState) pendingBanResetKeeperRefreshTargets(quotas map[string]quotaSnapshot) []keeperRefreshTarget {
	s.banResetMu.Lock()
	pending := make(map[string]banResetConfirmation)
	for authID, confirmation := range s.banResetConfirmations {
		if confirmation.Confirmations == 1 && !confirmation.LastSnapshotAt.IsZero() {
			pending[authID] = confirmation
		}
	}
	s.banResetMu.Unlock()
	if len(pending) == 0 {
		return nil
	}

	canonical := make(map[string]quotaSnapshot)
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			continue
		}
		if previous, ok := canonical[authID]; !ok || snapshot.RefreshedAt.After(previous.RefreshedAt) {
			canonical[authID] = snapshot
		}
	}
	targets := make([]keeperRefreshTarget, 0, len(pending))
	for authID, confirmation := range pending {
		entry, banned := banStore.lookup(authID)
		if !banned || !entry.BannedAt.Equal(confirmation.BannedAt) || !entry.ResetAt.Equal(confirmation.BanResetAt) {
			continue
		}
		snapshot, ok := canonical[authID]
		if !ok {
			continue
		}
		authIndex := strings.TrimSpace(snapshot.AuthIndex)
		if authIndex == "" {
			continue
		}
		targets = append(targets, keeperRefreshTarget{
			AuthIndex:  authIndex,
			Reason:     "ban_reset_confirmation",
			ObservedAt: confirmation.LastSnapshotAt,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].AuthIndex < targets[j].AuthIndex })
	return targets
}

func banWindowClass(window string) string {
	window = strings.ToLower(strings.TrimSpace(window))
	for _, candidate := range []string{"5h", "weekly", "week", "monthly", "month"} {
		if strings.HasPrefix(window, candidate) {
			return normalizeWindowClass(candidate)
		}
	}
	return ""
}

func resetAnchorDiffers(a, b time.Time) bool {
	delta := a.Sub(b)
	if delta < 0 {
		delta = -delta
	}
	return delta > warmupResetPlaceholderSkew
}

func (s *schedulerRuntimeState) advanceBanResetConfirmation(authID string, entry banEntry, snapshotAt time.Time, reason string, now time.Time, staleAfter time.Duration) (banResetConfirmation, bool) {
	s.banResetMu.Lock()
	defer s.banResetMu.Unlock()
	if s.banResetConfirmations == nil {
		s.banResetConfirmations = make(map[string]banResetConfirmation)
	}
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	current := s.banResetConfirmations[authID]
	currentExpired := current.LastSnapshotAt.IsZero() || now.Before(current.LastSnapshotAt) || now.Sub(current.LastSnapshotAt) > staleAfter
	if !current.BannedAt.Equal(entry.BannedAt) || !current.BanResetAt.Equal(entry.ResetAt) || current.Reason != reason || currentExpired {
		current = banResetConfirmation{
			AuthID: authID, BannedAt: entry.BannedAt, BanResetAt: entry.ResetAt,
			FirstSnapshotAt: snapshotAt, LastSnapshotAt: snapshotAt,
			Confirmations: 1, Reason: reason,
		}
		s.banResetConfirmations[authID] = current
		return current, true
	}
	if !snapshotAt.After(current.LastSnapshotAt) {
		return current, false
	}
	current.LastSnapshotAt = snapshotAt
	current.Confirmations++
	s.banResetConfirmations[authID] = current
	return current, true
}

func (s *schedulerRuntimeState) dropBanResetConfirmation(authID string) bool {
	s.banResetMu.Lock()
	defer s.banResetMu.Unlock()
	if _, ok := s.banResetConfirmations[authID]; !ok {
		return false
	}
	delete(s.banResetConfirmations, authID)
	return true
}

func (s *schedulerRuntimeState) clearWarmupStateForAuth(authID string) {
	authID = strings.TrimSpace(authID)
	s.warmupMu.Lock()
	for key, entry := range s.warmups {
		if strings.TrimSpace(entry.AuthID) == authID || strings.HasPrefix(key, authID+"|") {
			delete(s.warmups, key)
		}
	}
	for nonce, lease := range s.warmupLeases {
		if lease.AuthID == authID {
			delete(s.warmupLeases, nonce)
		}
	}
	s.warmupMu.Unlock()
}

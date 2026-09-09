package main

import (
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func (s *schedulerRuntimeState) recordQuotaDemand(record pluginapi.UsageRecord, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := strings.TrimSpace(record.AuthID)
	if id == "" {
		id = s.identities[strings.TrimSpace(record.AuthIndex)]
	}
	if id == "" {
		return
	}
	if s.quotaPolls == nil {
		s.quotaPolls = make(map[string]quotaPollState)
	}
	p := s.quotaPolls[id]
	p.LastUsageAt = now
	s.quotaPolls[id] = p
}

// Background ticks may maintain local state, but cannot continually poll an
// idle account. Each actual observation consumes the triggering event.
func (s *schedulerRuntimeState) quotaProbeReason(id string, now time.Time) string {
	s.mu.RLock()
	cfg := s.cfg
	p := s.quotaPolls[id]
	q := s.quotas[id]
	health := s.authExpiry[id]
	s.mu.RUnlock()
	if !cfg.QuotaProbeOnDemand {
		return "periodic"
	}
	if now.Before(p.NextAt) {
		return ""
	}
	if p.LastUsageAt.After(p.AttemptedAt) {
		return "usage"
	}
	if p.AttemptedAt.IsZero() || (q.RefreshedAt.IsZero() && p.Failures < 3) {
		return "initial"
	}
	if p.Failures > 0 && p.Failures < 3 && (p.Reason == "initial" || p.Reason == "reset" || p.Reason == "warmup") {
		return p.Reason
	}
	for _, w := range q.Windows {
		if !w.ResetAt.IsZero() && !quotaWindowHasPlaceholderReset(w, q.RefreshedAt, q.RefreshedAt) && !now.Before(w.ResetAt.Add(2*time.Second)) && p.AttemptedAt.Before(w.ResetAt) {
			return "reset"
		}
	}

	if p.Failures >= 3 && (p.Reason == "warmup_confirmation" || p.Reason == "auth_confirmation" || p.Reason == "reset_confirmation") {
		return ""
	}
	s.warmupMu.Lock()
	completedAfterProbe := false
	for _, entry := range s.warmups {
		if entry.AuthID == id && !entry.CompletedAt.IsZero() && p.AttemptedAt.Before(entry.CompletedAt) {
			completedAfterProbe = true
			break
		}
	}
	s.warmupMu.Unlock()
	if completedAfterProbe {
		return "warmup_confirmation"
	}
	if health.Status.Confirmations == 1 && health.Status.Reason == "expiry_conflict" {
		return "auth_confirmation"
	}
	s.banResetMu.Lock()
	confirm := s.banResetConfirmations[id]
	s.banResetMu.Unlock()
	if confirm.Confirmations == 1 {
		return "reset_confirmation"
	}
	if !cfg.WarmupEnabled {
		return ""
	}
	if _, banned := banStore.lookup(id); banned {
		return ""
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	if s.warmupRunning || s.warmupTrafficStatusLocked(cfg, now).HoldReason != "" || s.warmupTrafficStatusLocked(cfg, now, id).HoldReason != "" {
		return ""
	}
	window, needs := unstartedWarmupWindow(q, now)
	if !needs {
		return ""
	}
	if s.warmupSuppressedAccountsLocked(now, cfg.WarmupRetryAfter)[id] {
		return ""
	}
	entry, found := s.warmups[warmupKey(id, window.Class)]
	if found && warmupEntrySuppressesNow(entry, now, cfg.WarmupRetryAfter) {
		return ""
	}
	// At most one preflight per admission interval even if CPA cannot admit a warmup.
	interval := max(cfg.WarmupMinInterval, 15*time.Minute)
	if !quotaSnapshotFresh(q, now, cfg.StaleAfter) && !now.Before(p.AttemptedAt.Add(interval)) {
		return "warmup"
	}
	return ""
}

// An authoritative cooldown is a timed restriction. Expiration releases that
// restriction only: still-active quota windows, auth failures and probation
// remain independent constraints. No snapshot timestamp or balance is invented.
func (s *schedulerRuntimeState) releaseElapsedQuotaCooldowns(quotas map[string]quotaSnapshot, now time.Time) {
	changed := false
	for id, entry := range banStore.snapshot() {
		entry = normalizeBanEntry(entry)
		if entry.Kind != banKindQuota || entry.Phase != banPhaseCooldown || entry.ResetAt.IsZero() || now.Before(entry.ResetAt.Add(2*time.Second)) {
			continue
		}
		q, exists := quotas[id]
		if !exists || len(q.Windows) == 0 {
			continue
		}
		blocked := false
		for _, w := range q.Windows {
			if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
				continue
			}
			if !w.Allowed || w.LimitReached || w.UsedPercent >= usedPercentThreshold {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		banStore.mu.Lock()
		current, present := banStore.bans[id]
		removed := present && sameQuotaBan(current, entry)
		if removed {
			delete(banStore.bans, id)
		}
		banStore.mu.Unlock()
		if !removed {
			continue
		}
		s.dropBanResetConfirmation(id)
		s.mu.Lock()
		s.banExternalResetClears++
		s.lastBanClearReason = "scheduled_reset"
		s.lastBanClearAt = now
		s.mu.Unlock()
		changed = true
	}
	if changed {
		s.persistBanState()
	}
}

// Idle demand-mode observations remain scheduling estimates until their known
// reset. This never refreshes the UI timestamp or bypasses warmup freshness.
func quotaSchedulingUsable(q quotaSnapshot, now time.Time, cfg pluginConfig) bool {
	if quotaSnapshotFresh(q, now, cfg.StaleAfter) {
		return true
	}
	if !cfg.QuotaProbeOnDemand || q.RefreshedAt.IsZero() || now.Before(q.RefreshedAt) || len(q.Windows) == 0 {
		return false
	}
	for _, w := range q.Windows {
		if w.ResetAt.IsZero() {
			return false
		}
	}
	return true
}

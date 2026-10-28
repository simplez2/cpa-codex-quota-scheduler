package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func (s *schedulerRuntimeState) recordQuotaDemand(record pluginapi.UsageRecord, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.canonicalAuthIDLocked(record.AuthID, record.AuthIndex)
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

// resetCPACredentialCooldown clears CPA's runtime credential/model cooldown
// after an upstream reset-card action. The host callback deliberately leaves
// the token document untouched; only runtime routing state is reset.
func (s *schedulerRuntimeState) resetCPACredentialCooldown(ctx context.Context, authID string, call authExpiryHostCall) (pluginapi.HostRoutingResetCooldownResponse, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("auth_id_required")
	}
	s.mu.RLock()
	snapshot, found := s.lookupQuotaLocked(authID, "")
	cfg := s.cfg
	s.mu.RUnlock()
	authIndex := ""
	if found {
		authIndex = strings.TrimSpace(snapshot.AuthIndex)
	}
	if authIndex == "" {
		files, err := cpaManagementAuthFiles(ctx, cfg)
		if err != nil {
			return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("auth_inventory_unavailable")
		}
		for _, file := range files {
			provider := strings.TrimSpace(file.Provider)
			if provider == "" {
				provider = strings.TrimSpace(file.Type)
			}
			if !strings.EqualFold(provider, providerCodex) || file.Disabled {
				continue
			}
			if strings.TrimSpace(file.ID) == authID || strings.TrimSpace(file.Name) == authID || strings.TrimSpace(file.AuthIndex) == authID {
				authIndex = strings.TrimSpace(file.AuthIndex)
				break
			}
		}
	}
	if authIndex == "" {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("auth_index_unavailable")
	}
	if call == nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("host_callback_unavailable")
	}
	raw, err := call(pluginabi.MethodHostRoutingResetCooldown, pluginapi.HostRoutingResetCooldownRequest{AuthIndex: authIndex})
	if err != nil {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("cpa_cooldown_reset_failed")
	}
	var result pluginapi.HostRoutingResetCooldownResponse
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.AuthIndex) != authIndex {
		return pluginapi.HostRoutingResetCooldownResponse{}, fmt.Errorf("cpa_cooldown_reset_invalid_response")
	}
	return result, nil
}

// requestImmediateQuotaProbe turns an explicit recovery action, such as a
// manual unban after an upstream reset card, into demand for the next quota
// refresh tick. It does not claim fresh quota or clear a real query failure;
// a successful probe must do both.
func (s *schedulerRuntimeState) requestImmediateQuotaProbe(authID string, now time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if canonical := s.canonicalAuthIDLocked(authID, ""); canonical != "" {
		authID = canonical
	}
	if s.quotaPolls == nil {
		s.quotaPolls = make(map[string]quotaPollState)
	}
	p := s.quotaPolls[authID]
	p.LastUsageAt = now
	p.NextAt = time.Time{}
	s.quotaPolls[authID] = p
}

// Background ticks may maintain local state, but cannot continually poll an
// idle account. Each actual observation consumes the triggering event.
func (s *schedulerRuntimeState) quotaProbeReason(id string, now time.Time) string {
	s.mu.RLock()
	cfg := s.cfg
	p := s.quotaPolls[id]
	q := s.quotas[id]
	health := s.authExpiry[id]
	phase := s.fiveHourPhases[id]
	continuityUntil := s.continuityDemandUntil[id]
	epochReason := s.quotaEpochProbeReasonLocked(id, now)
	s.mu.RUnlock()
	if !cfg.QuotaProbeOnDemand {
		return "periodic"
	}
	// Epoch follow-ups are bounded by their own probe count and spacing. A
	// successful standby poll's long cooldown must not stall new-week activation.
	// Failed probes still retain their independent backoff.
	if epochReason != "" && (p.Failures == 0 || !now.Before(p.NextAt)) {
		return epochReason
	}
	if now.Before(p.NextAt) {
		return ""
	}
	// A live host continuity assessment may ask for a post-reset observation.
	// It never overrides per-credential backoff or authenticates a refill.
	if cfg.ContinuityEnabled && now.Before(continuityUntil) {
		return "continuity"
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
	if normalizeWindowClass(window.Class) == "5h" && phase.ActivateAt.After(now) {
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

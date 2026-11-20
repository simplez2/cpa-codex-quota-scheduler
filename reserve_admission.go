package main

import (
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// A non-generating WS handshake normally belongs to CPA. If the host could
// bind it to a reserved credential, choose a safe sibling before that happens.
// Do not create billing samples, reservations, session affinity or probe leases.
func (s *schedulerRuntimeState) nativeReserveAlternative(req pluginapi.SchedulerPickRequest, now time.Time) pluginapi.SchedulerPickResponse {
	if len(req.Candidates) == 0 || !codexOnlySchedulerRequest(req) {
		return pluginapi.SchedulerPickResponse{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.cfg
	mode := normalizeSchedulerMode(cfg.SchedulerMode)
	if !cfg.Enabled || s.detached || cfg.SerialSoftContinuation || (mode != "balanced" && mode != "serial") {
		return pluginapi.SchedulerPickResponse{}
	}
	pin := serialPinnedAuthID(req)
	class, unknown := "", ""
	var healthy []serialCandidate
	binding, bound := s.balancedSessionLocked(schedulerSessionHash(req), now)
	if !bound {
		binding, bound = s.balancedSessionLocked(schedulerParentSessionHash(req), now)
	}
	boundID := ""
	for _, c := range req.Candidates {
		if c.ID == "" || (pin != "" && c.ID != pin) {
			continue
		}
		q, ok := s.lookupQuotaLocked(c.ID, candidateAuthIndex(c))
		q = s.serialConservativeQuotaLocked(q, now)
		if w := protectedWindow(q, cfg, now); w != "" {
			class = w
			continue
		}
		choice := inspectSerialCandidate(c, q, ok, cfg, now)
		if !choice.Eligible || !banStore.schedulable(c.ID, now) {
			continue
		}
		if circuit := s.adqProviderCircuits[c.ID]; circuit.RetryAt.After(now) {
			continue
		}
		if choice.QuotaKnown {
			healthy = append(healthy, choice)
			if bound && binding.AuthID == c.ID && (binding.AuthIndex == "" || binding.AuthIndex == q.AuthIndex) {
				boundID = c.ID
			}
		} else if !choice.QuotaKnown && unknown == "" {
			unknown = c.ID
		}
	}
	if class == "" {
		return pluginapi.SchedulerPickResponse{}
	}
	if boundID != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: boundID}
	}
	if len(healthy) > 0 {
		sortSerialCandidates(healthy, cfg)
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: healthy[0].Candidate.ID}
	}
	if unknown != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: unknown}
	}
	return pluginapi.SchedulerPickResponse{Handled: true, Reject: true, RejectCode: class + "_quota_reserved", RejectReason: "No unreserved Codex credential is available for this handshake"}
}

// Completion order is not sample order. A delayed header must not put spent
// quota back into an open cycle. A fresh native read can still report a refill,
// even at the same reset anchor; an advanced week can still trigger the reset sweep.
func orderedQuotaHeaders(q quotaSnapshot, patches []quotaWindowPatch, started, now time.Time, cfg pluginConfig) []quotaWindowPatch {
	out := make([]quotaWindowPatch, 0, len(patches))
	for _, p := range patches {
		w, ok := adqSettlementCurrentWindow(q, p.Class)
		if !ok || w.ResetAt.IsZero() || !w.ResetAt.After(now) || (p.Class != "weekly" && p.Class != "5h") {
			out = append(out, p)
			continue
		}
		observed := w.ObservedAt
		if observed.IsZero() {
			observed = q.RefreshedAt
		}
		if p.ResetAt != nil && p.ResetAt.Before(w.ResetAt.Add(-5*time.Minute)) {
			// For an active, anchored cycle an older epoch cannot overwrite a
			// newer probe (including after a reset card). Ignore its usage too.
			if (!started.IsZero() && started.Before(observed)) || !quotaWindowHasPlaceholderReset(w, q.RefreshedAt, now) {
				continue
			}
		}
		sameCycle := p.ResetAt == nil || sameQuotaCycle(w.ResetAt, *p.ResetAt)
		if sameCycle && p.UsedPercent != nil && *p.UsedPercent < w.UsedPercent &&
			(started.IsZero() || !started.After(observed) || reserveBoundary(w, cfg)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Only explicit 5h reserve mode participates. 429_only has no synthetic floor.
func reserveBoundary(w quotaWindow, cfg pluginConfig) bool {
	class := normalizeWindowClass(w.Class)
	mode, threshold := serialWindowHandoffPolicy(cfg, class)
	switch class {
	case "weekly":
		if mode == "reserve_aware" {
			threshold = min(threshold, 100-cfg.ReserveWeeklyPercent)
		}
	case "5h":
		if mode != "reserve_aware" || cfg.Reserve5hPercent <= 0 {
			return false
		}
		threshold = 100 - cfg.Reserve5hPercent
	default:
		return false
	}
	return w.UsedPercent >= threshold
}

func protectedWindow(q quotaSnapshot, cfg pluginConfig, now time.Time) string {
	for _, w := range q.Windows {
		if (w.ResetAt.IsZero() && !quotaSnapshotFresh(q, now, cfg.StaleAfter)) ||
			(!w.ResetAt.IsZero() && !w.ResetAt.After(now)) {
			continue
		}
		if reserveBoundary(w, cfg) {
			return normalizeWindowClass(w.Class)
		}
	}
	return ""
}

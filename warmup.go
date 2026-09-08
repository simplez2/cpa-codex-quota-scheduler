package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// warmupEntry records one low-cost activation attempt. It intentionally stores
// no token or response body, only the auth/window bookkeeping needed to avoid
// repeating the request after a CPA restart.
type warmupEntry struct {
	AuthID        string    `json:"auth_id"`
	AuthIndex     string    `json:"auth_index,omitempty"`
	Window        string    `json:"window"`
	AttemptedAt   time.Time `json:"attempted_at"`
	OutcomeAt     time.Time `json:"outcome_at,omitempty"`
	Failures      int       `json:"failures,omitempty"`
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	ActivatedAt   time.Time `json:"activated_at,omitempty"`
	ResetAt       time.Time `json:"reset_at,omitempty"`
	SuppressUntil time.Time `json:"suppress_until,omitempty"`
	Status        int       `json:"status,omitempty"`
	Error         string    `json:"error,omitempty"`
	DispatchState string    `json:"dispatch_state,omitempty"`
	Blocked       bool      `json:"blocked,omitempty"`
}

type warmupCandidate struct {
	Snapshot quotaSnapshot
	Window   quotaWindow
	RetryAt  time.Time
}

type warmupAuthBinding struct {
	AuthID    string
	AuthIndex string
	AccountID string
}

type cpaAPICallRequest struct {
	AuthIndex string            `json:"auth_index"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Header    map[string]string `json:"header"`
	Data      string            `json:"data"`
}

type cpaAPICallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

type cpaAuthFileEntry struct {
	IDToken struct {
		AccountID string `json:"chatgpt_account_id"`
		PlanType  string `json:"plan_type"`
	} `json:"id_token"`
	ID          string `json:"id"`
	AuthIndex   string `json:"auth_index"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Note        string `json:"note"`
}

type warmupAuthEligibilityStats struct {
	Seen     int
	Eligible int
	Rejected map[string]int
}

func newWarmupAuthEligibilityStats() warmupAuthEligibilityStats {
	return warmupAuthEligibilityStats{Rejected: make(map[string]int)}
}

func (stats *warmupAuthEligibilityStats) reject(reason string) {
	if stats.Rejected == nil {
		stats.Rejected = make(map[string]int)
	}
	stats.Rejected[reason]++
}

const (
	cpaWarmupResponsesURL         = "https://chatgpt.com/backend-api/codex/responses"
	warmupMinimumAvailablePercent = 0.000001
	warmupMinimumUsageCredits     = 0.000001
	warmupResetPlaceholderSkew    = 3 * time.Second
	warmupStaleActivationGrace    = 30 * time.Minute
	warmupStartupGrace            = 15 * time.Second
	warmupMaxResponseBytes        = 2 << 20
	warmupRequestTimeout          = 45 * time.Second
)

// warmupStartupReady keeps activation traffic away from CPA while a newly
// claimed plugin generation is still starting. The next quota probe refresh will
// retry after CPA's API server and auth registry have had time to settle.
func (s *schedulerRuntimeState) warmupStartupReady(now time.Time) bool {
	ownership := s.generationSnapshot()
	if !ownership.Managed || ownership.ClaimedAt.IsZero() {
		return true
	}
	return !now.Before(ownership.ClaimedAt.Add(warmupStartupGrace))
}

// scheduleWarmup is called after a fresh quota probe snapshot. It deliberately
// admits one request under a durable pool-wide traffic budget and instance lease.
func (s *schedulerRuntimeState) scheduleWarmup(parent context.Context, skipAuthIDs map[string]struct{}) {
	s.mu.RLock()
	cfg := s.cfg
	foregroundBusy := false
	if cfg.SchedulerMode == "balanced" {
		now := time.Now()
		for _, account := range s.balancedAccounts {
			if now.Sub(account.LastPicked) < time.Minute {
				foregroundBusy = true
				break
			}
			for _, pending := range account.Pending {
				if now.Sub(pending.At) < balancedPendingTTL {
					foregroundBusy = true
					break
				}
			}
		}
	}
	s.mu.RUnlock()
	if !cfg.Enabled || !cfg.WarmupEnabled || foregroundBusy || strings.TrimSpace(cfg.StatePath) == "" || parent.Err() != nil {
		return
	}
	if !s.generationOwnerActive() {
		return
	}
	now := time.Now()
	if !s.warmupStartupReady(now) {
		return
	}
	generationClaimedAt := s.generationSnapshot().ClaimedAt
	if strings.TrimSpace(cfg.CPAManagementURL) == "" || strings.TrimSpace(cfg.CPAManagementKeyFile) == "" {
		return
	}
	now = time.Now()
	s.warmupMu.Lock()
	if s.warmupRunning {
		s.warmupMu.Unlock()
		return
	}
	if s.warmups == nil {
		s.warmups = make(map[string]warmupEntry)
	}
	instanceLease, acquired, err := acquireWarmupInstanceLease(cfg.StatePath, now)
	if err != nil {
		s.warmupMu.Unlock()
		slog.Warn("codex-quota-scheduler: warmup skipped because the cross-instance lease is unavailable", "error", err)
		return
	}
	if !acquired {
		s.warmupMu.Unlock()
		slog.Debug("codex-quota-scheduler: warmup skipped because another plugin instance is active")
		return
	}
	releaseInstanceLease := func() {
		if err := instanceLease.release(); err != nil {
			slog.Warn("codex-quota-scheduler: could not release warmup instance lease", "error", err)
		}
	}
	journalBans, mergedJournal, err := s.mergePersistedWarmupsLocked(cfg.StatePath)
	if err != nil {
		s.warmupMu.Unlock()
		releaseInstanceLease()
		slog.Warn("codex-quota-scheduler: warmup skipped because persisted warmup state is unavailable", "error", err)
		return
	}
	// Apply narrow outcomes from a superseded request only after releasing
	// warmupMu, preserving the banStore -> warmupMu lock order used elsewhere.
	s.warmupMu.Unlock()
	for authID, entry := range journalBans {
		banStore.set(authID, entry)
	}
	if !s.generationOwnerActive() {
		releaseInstanceLease()
		return
	}
	if mergedJournal && s.persistBanState() {
		if err := clearWarmupOutcomeJournal(cfg.StatePath); err != nil {
			slog.Warn("codex-quota-scheduler: could not compact merged warmup outcome journal", "error", err)
		}
	}
	// Check before auth discovery so budget exhaustion creates no additional
	// management traffic. Persisted/journal outcomes must be merged first.
	s.warmupMu.Lock()
	hold := s.warmupTrafficStatusLocked(cfg, time.Now()).HoldReason
	s.warmupMu.Unlock()
	if hold != "" {
		releaseInstanceLease()
		return
	}
	if s.pruneExpiredWarmups(time.Now()) {
		s.persistBanState()
	}
	eligible, err := s.cpaWarmupEligibleAuths(parent, cfg)
	if err != nil {
		releaseInstanceLease()
		code, _ := classifyWarmupFailure(0, err)
		slog.Warn("codex-quota-scheduler: warmup skipped because CPA auth status is unavailable", "error_code", code)
		return
	}
	candidates := s.findWarmupCandidates(eligible, skipAuthIDs, time.Now())
	if len(candidates) == 0 || parent.Err() != nil || !s.generationOwnerActive() {
		releaseInstanceLease()
		return
	}
	now = time.Now()
	s.warmupMu.Lock()
	if s.warmupRunning || s.warmupTrafficStatusLocked(cfg, now).HoldReason != "" {
		s.warmupMu.Unlock()
		releaseInstanceLease()
		return
	}
	candidate, key, ok := s.nextWarmupCandidateForGenerationLocked(candidates, now, cfg.WarmupRetryAfter, generationClaimedAt)
	if !ok {
		s.warmupMu.Unlock()
		releaseInstanceLease()
		return
	}
	if !s.admitBackgroundWorker() {
		s.warmupMu.Unlock()
		releaseInstanceLease()
		return
	}
	failures := s.warmups[key].Failures
	s.warmups[key] = warmupEntry{
		AuthID:      candidate.Snapshot.AuthID,
		AuthIndex:   candidate.Snapshot.AuthIndex,
		Window:      candidate.Window.Class,
		AttemptedAt: now,
		Failures:    failures,
		// A crash after admission gives no proof that upstream did not execute.
		SuppressUntil: now.Add(warmupUncertainDelay),
	}
	s.mergeWarmupAttemptsLocked([]warmupAttempt{{AuthID: candidate.Snapshot.AuthID, At: now}}, now)
	s.warmupRunning = true
	s.warmupMu.Unlock()
	if !s.persistBanState() {
		s.warmupMu.Lock()
		s.warmupRunning = false
		s.warmupMu.Unlock()
		s.wg.Done()
		releaseInstanceLease()
		slog.Warn("codex-quota-scheduler: warmup skipped because admission could not be persisted")
		return
	}

	go func() {
		defer s.wg.Done()
		defer releaseInstanceLease()
		defer func() {
			s.warmupMu.Lock()
			s.warmupRunning = false
			s.warmupMu.Unlock()
		}()
		executed := false
		if parent.Err() == nil && s.generationOwnerActive() && s.warmupCandidateStillEligible(candidate, time.Now()) {
			executed = true
			s.executeWarmup(parent, cfg, candidate)
		}
		// A request admitted by the previous generation may finish after
		// takeover. Transfer only that lease-protected outcome so the new owner
		// does not repeat the activation; never persist the old full snapshot.
		if executed && !s.generationOwnerActive() {
			if err := s.persistWarmupLeaseOutcome(instanceLease, candidate); err != nil {
				slog.Warn("codex-quota-scheduler: could not persist warmup lease outcome", "error", err)
			}
		}
	}()
}

func (s *schedulerRuntimeState) findWarmupCandidate(eligible map[string]warmupAuthBinding, now time.Time) (warmupCandidate, bool) {
	candidates := s.findWarmupCandidates(eligible, nil, now)
	if len(candidates) == 0 {
		return warmupCandidate{}, false
	}
	return candidates[0], true
}

func (s *schedulerRuntimeState) findWarmupCandidates(eligible map[string]warmupAuthBinding, skipAuthIDs map[string]struct{}, now time.Time) []warmupCandidate {
	s.mu.RLock()
	quotas := make(map[string]quotaSnapshot, len(s.quotas))
	for key, snapshot := range s.quotas {
		quotas[key] = snapshot
	}
	cfg := s.cfg
	activeID := s.serialActiveAuthID
	polls := make(map[string]quotaPollState, len(s.quotaPolls))
	for key, poll := range s.quotaPolls {
		polls[key] = poll
	}
	s.mu.RUnlock()

	seen := make(map[string]struct{})
	candidates := make([]warmupCandidate, 0)
	skippedBanned := 0
	skippedStale := 0
	skippedIneligible := 0
	skippedNotNeeded := 0
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" || strings.TrimSpace(snapshot.AuthIndex) == "" {
			skippedIneligible++
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		if authID == activeID || polls[authID].Error != "" || !warmupQuotaHasHeadroom(snapshot, cfg) {
			skippedIneligible++
			continue
		}
		if !warmupSnapshotFresh(snapshot, now, cfg.StaleAfter) {
			skippedStale++
			continue
		}
		binding := eligible[authID]
		if strings.TrimSpace(binding.AuthID) == "" {
			binding = eligible[strings.TrimSpace(snapshot.AuthIndex)]
		}
		binding.AuthID = strings.TrimSpace(binding.AuthID)
		binding.AuthIndex = strings.TrimSpace(binding.AuthIndex)
		if binding.AuthID == "" || binding.AuthIndex == "" {
			skippedIneligible++
			continue
		}
		if _, skip := skipAuthIDs[binding.AuthID]; skip {
			continue
		}
		// A replacement credential needs its own quota observation. Rebinding a
		// stale snapshot could warm a different workspace with unknown limits.
		if snapshot.AuthID != binding.AuthID || snapshot.AuthIndex != binding.AuthIndex {
			skippedIneligible++
			continue
		}
		// Quarantined credentials recover only through the serialized half-open
		// scheduler path. Warmup must never bypass that lease with a second probe.
		if _, quarantined := banStore.lookup(binding.AuthID); quarantined {
			skippedBanned++
			continue
		}
		if window, ok := unstartedWarmupWindow(snapshot, now); ok {
			candidates = append(candidates, warmupCandidate{Snapshot: snapshot, Window: window})
		} else {
			skippedNotNeeded++
		}
	}
	if len(candidates) == 0 {
		s.mu.Lock()
		s.warmupCandidatesLast = 0
		s.warmupSkippedBannedLast = skippedBanned
		s.warmupSkippedStaleLast = skippedStale
		s.warmupSkippedIneligibleLast = skippedIneligible
		s.warmupSkippedNotNeededLast = skippedNotNeeded
		s.mu.Unlock()
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ri, rj := warmupWindowRank(candidates[i].Window.Class), warmupWindowRank(candidates[j].Window.Class)
		if ri != rj {
			return ri < rj
		}
		// Preserve reset credits for real traffic when there is another equally
		// eligible account without them.
		if candidates[i].Snapshot.ResetCredits != candidates[j].Snapshot.ResetCredits {
			return candidates[i].Snapshot.ResetCredits < candidates[j].Snapshot.ResetCredits
		}
		return candidates[i].Snapshot.AuthID < candidates[j].Snapshot.AuthID
	})
	actionableCandidates := s.countActionableWarmupCandidates(candidates, now, cfg.WarmupRetryAfter)
	s.mu.Lock()
	s.warmupCandidatesLast = actionableCandidates
	s.warmupSkippedBannedLast = skippedBanned
	s.warmupSkippedStaleLast = skippedStale
	s.warmupSkippedIneligibleLast = skippedIneligible
	s.warmupSkippedNotNeededLast = skippedNotNeeded
	s.mu.Unlock()
	return candidates
}

// countActionableWarmupCandidates reports candidates that can actually be
// attempted now. The raw quota snapshot may continue to expose a moving 100%
// placeholder briefly after a successful activation, but a confirmed warmup
// entry suppresses another request until its fixed reset anchor. Management
// status should reflect that execution state instead of displaying a false
// pending candidate.
func (s *schedulerRuntimeState) countActionableWarmupCandidates(candidates []warmupCandidate, now time.Time, retryAfter time.Duration) int {
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	count := 0
	suppressed := s.warmupSuppressedAccountsLocked(now, retryAfter)
	for _, candidate := range candidates {
		key := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
		entry, ok := s.warmups[key]
		if !suppressed[candidate.Snapshot.AuthID] &&
			(!ok || staleWarmupState(entry, candidate, now, retryAfter) || !warmupEntrySuppressesNow(entry, now, retryAfter)) {
			count++
		}
	}
	return count
}

func warmupEntrySuppressesNow(entry warmupEntry, now time.Time, retryAfter time.Duration) bool {
	if entry.Blocked {
		return true
	}
	if !entry.SuppressUntil.IsZero() && now.Before(entry.SuppressUntil) {
		return true
	}
	if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) {
		return false
	}
	if !entry.ActivatedAt.IsZero() && !entry.ResetAt.IsZero() && now.Before(entry.ResetAt) {
		return true
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	return !entry.AttemptedAt.IsZero() && now.Sub(entry.AttemptedAt) < retryAfter
}

// warmupSnapshotFresh is intentionally stricter than the ordinary scheduling
// freshness check. A partial quota probe response can carry a missing window forward
// under a newer outer RefreshedAt. That is useful for routing continuity, but an
// activation request must never be admitted from such an indefinitely carried
// 0% row: every recognized window needs its own fresh observation.
func warmupSnapshotFresh(snapshot quotaSnapshot, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	if staleAfter > warmupQuotaMaxAge {
		staleAfter = warmupQuotaMaxAge
	}
	if snapshot.RefreshedAt.IsZero() || now.Before(snapshot.RefreshedAt) || now.Sub(snapshot.RefreshedAt) > staleAfter {
		return false
	}
	for _, window := range snapshot.Windows {
		if normalizeWindowClass(window.Class) == "" {
			continue
		}
		observedAt := window.ObservedAt
		if observedAt.IsZero() || now.Before(observedAt) || now.Sub(observedAt) > staleAfter {
			return false
		}
	}
	return true
}

func unstartedWarmupWindow(snapshot quotaSnapshot, now time.Time) (quotaWindow, bool) {
	// quota probe reports a future reset even for a window that has not started yet:
	// resetAfterSeconds equals the full window duration and the reset timestamp
	// moves forward with every observation. Treat that as a placeholder rather
	// than proof of activation. Monthly-only accounts are included because their
	// primary cycle otherwise never receives a real anchor.
	if len(snapshot.Windows) == 0 {
		return quotaWindow{}, false
	}
	var selected quotaWindow
	foundUnstarted := false
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class == "" {
			// quota probe intentionally preserves additional/future quota rows. An
			// unrelated unclassified meter must not suppress activation of a
			// recognized 5h/weekly/monthly Codex window.
			continue
		}
		// Every account-wide window constrains the generation request, even
		// when a different row is the cycle we would like to activate.
		if window.LimitReached || !window.Allowed || window.UsedPercent >= usedPercentThreshold {
			return quotaWindow{}, false
		}
		if window.UsedPercent > warmupMinimumAvailablePercent {
			continue
		}
		if window.WindowUsageCreditsKnown && window.WindowUsageCredits > warmupMinimumUsageCredits {
			continue
		}
		if quotaWindowNeedsActivation(window, snapshot.RefreshedAt, now) {
			window.Class = class
			if !foundUnstarted || warmupWindowRank(class) < warmupWindowRank(selected.Class) {
				selected = window
				foundUnstarted = true
			}
		}
	}
	return selected, foundUnstarted
}

func warmupWindowRank(class string) int {
	switch class {
	case "5h":
		return 0
	case "weekly":
		return 1
	case "monthly":
		return 2
	default:
		return 10
	}
}

func quotaWindowNeedsActivation(window quotaWindow, refreshedAt, now time.Time) bool {
	if window.UsedPercent > warmupMinimumAvailablePercent || window.LimitReached || !window.Allowed {
		return false
	}
	if window.WindowUsageCreditsKnown && window.WindowUsageCredits > warmupMinimumUsageCredits {
		return false
	}
	if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return true
	}
	return quotaWindowHasPlaceholderReset(window, refreshedAt, now)
}

func quotaWindowHasPlaceholderReset(window quotaWindow, refreshedAt, now time.Time) bool {
	if window.ResetAt.IsZero() || !now.Before(window.ResetAt) || window.WindowSeconds <= 0 {
		return false
	}
	if window.UsedPercent > warmupMinimumAvailablePercent {
		return false
	}
	if window.WindowUsageCreditsKnown && window.WindowUsageCredits > warmupMinimumUsageCredits {
		return false
	}
	toleranceSeconds := int64(warmupResetPlaceholderSkew / time.Second)
	if window.ResetAfterSecondsKnown {
		return window.ResetAfterSeconds >= window.WindowSeconds-toleranceSeconds
	}
	observedAt := window.ObservedAt
	if observedAt.IsZero() {
		observedAt = refreshedAt
	}
	if observedAt.IsZero() {
		return false
	}
	expectedReset := observedAt.Add(time.Duration(window.WindowSeconds) * time.Second)
	delta := window.ResetAt.Sub(expectedReset)
	if delta < 0 {
		delta = -delta
	}
	return delta <= warmupResetPlaceholderSkew
}

func quotaWindowCycleStarted(window quotaWindow, refreshedAt, now time.Time) bool {
	if window.UsedPercent > warmupMinimumAvailablePercent {
		return true
	}
	if window.WindowUsageCreditsKnown && window.WindowUsageCredits > warmupMinimumUsageCredits {
		return true
	}
	if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
		return false
	}
	return !quotaWindowHasPlaceholderReset(window, refreshedAt, now)
}

func warmupKey(authID, window string) string {
	return strings.TrimSpace(authID) + "|" + strings.TrimSpace(window)
}

// nextWarmupCandidateLocked skips accounts already activated or recently
// attempted, allowing later full accounts to make progress on the next refresh.
// The caller must hold warmupMu.
func (s *schedulerRuntimeState) nextWarmupCandidateLocked(candidates []warmupCandidate, now time.Time, retryAfter time.Duration) (warmupCandidate, string, bool) {
	return s.nextWarmupCandidateForGenerationLocked(candidates, now, retryAfter, time.Time{})
}

// Generation changes never shorten a persisted request's suppression period.
func (s *schedulerRuntimeState) nextWarmupCandidateForGenerationLocked(candidates []warmupCandidate, now time.Time, retryAfter time.Duration, generationClaimedAt time.Time) (warmupCandidate, string, bool) {
	suppressed := s.warmupSuppressedAccountsLocked(now, retryAfter)
	for _, candidate := range candidates {
		if suppressed[candidate.Snapshot.AuthID] {
			continue
		}
		key := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
		if entry, ok := s.warmups[key]; ok && staleWarmupState(entry, candidate, now, retryAfter) {
			delete(s.warmups, key)
			slog.Info("codex-quota-scheduler: discarded stale warmup state after fresh unstarted quota snapshot",
				"auth_id", candidate.Snapshot.AuthID,
				"window", candidate.Window.Class)
		}
		if s.warmupSuppressedForGenerationLocked(key, now, retryAfter, generationClaimedAt) {
			continue
		}
		return candidate, key, true
	}
	return warmupCandidate{}, "", false
}

func staleWarmupState(entry warmupEntry, candidate warmupCandidate, now time.Time, retryAfter time.Duration) bool {
	if entry.Blocked {
		return false
	}
	// A drifting zero-usage placeholder is not proof that a completed request
	// failed. Keep the original suppression deadline, including pending rows.
	if now.Before(entry.SuppressUntil) || now.Before(entry.ResetAt) {
		return false
	}
	observedAt := candidate.Snapshot.RefreshedAt
	completedAt := entry.ActivatedAt
	if completedAt.IsZero() {
		completedAt = entry.CompletedAt
	}
	if completedAt.IsZero() || observedAt.IsZero() || !observedAt.After(completedAt) {
		return false
	}
	grace := warmupStaleActivationGrace
	if retryAfter > grace {
		grace = retryAfter
	}
	if now.Sub(completedAt) < grace || !quotaWindowNeedsActivation(candidate.Window, observedAt, now) {
		return false
	}
	if entry.ResetAt.IsZero() || candidate.Window.ResetAt.IsZero() {
		return true
	}
	if quotaWindowHasPlaceholderReset(candidate.Window, observedAt, now) {
		return true
	}
	return resetAnchorDiffers(entry.ResetAt, candidate.Window.ResetAt)
}

func (s *schedulerRuntimeState) pruneExpiredWarmups(now time.Time) bool {
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	changed := false
	for key, entry := range s.warmups {
		if entry.Blocked || entry.Error != "" || now.Before(entry.SuppressUntil) {
			continue
		}
		expiredReset := !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt)
		expiredPending := entry.ResetAt.IsZero() && !entry.SuppressUntil.IsZero() && !now.Before(entry.SuppressUntil)
		if expiredReset || expiredPending {
			delete(s.warmups, key)
			changed = true
		}
	}
	return changed
}

func (s *schedulerRuntimeState) confirmPendingWarmups(quotas map[string]quotaSnapshot, now time.Time) bool {
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
	changed := false
	s.warmupMu.Lock()
	for key, entry := range s.warmups {
		if entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() || entry.Blocked || entry.Error != "" {
			continue
		}
		snapshot, ok := canonical[strings.TrimSpace(entry.AuthID)]
		if !ok || snapshot.AuthIndex != entry.AuthIndex || !snapshot.RefreshedAt.After(entry.CompletedAt) {
			continue
		}
		for _, window := range snapshot.Windows {
			if !window.ObservedAt.After(entry.CompletedAt) || normalizeWindowClass(window.Class) != normalizeWindowClass(entry.Window) || window.ResetAt.IsZero() ||
				!now.Before(window.ResetAt) || quotaWindowHasPlaceholderReset(window, snapshot.RefreshedAt, now) ||
				!quotaWindowCycleStarted(window, snapshot.RefreshedAt, now) {
				continue
			}
			entry.ActivatedAt = entry.CompletedAt
			entry.ResetAt = window.ResetAt
			entry.SuppressUntil = window.ResetAt
			entry.Error = ""
			s.warmups[key] = entry
			changed = true
			slog.Info("codex-quota-scheduler: confirmed warmup reset anchor from quota probe",
				"auth_id", entry.AuthID,
				"window", entry.Window,
				"reset_at", window.ResetAt.Format(time.RFC3339))
			break
		}
	}
	s.warmupMu.Unlock()
	return changed
}

func (s *schedulerRuntimeState) warmupSuppressedLocked(key string, now time.Time, retryAfter time.Duration) bool {
	return s.warmupSuppressedForGenerationLocked(key, now, retryAfter, time.Time{})
}

func (s *schedulerRuntimeState) warmupSuppressedForGenerationLocked(key string, now time.Time, retryAfter time.Duration, generationClaimedAt time.Time) bool {
	entry, ok := s.warmups[key]
	if !ok {
		return false
	}
	if entry.Blocked {
		return true
	}
	return warmupEntrySuppressesNow(entry, now, retryAfter)
}

func (s *schedulerRuntimeState) executeWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
	model, err := validateWarmupModel(cfg.WarmupModel)
	if err != nil {
		s.recordWarmupNotSent(candidate, "invalid_warmup_model")
		return
	}
	cfg.WarmupModel = model
	s.executeCPAWarmup(parent, cfg, candidate)
}

func (s *schedulerRuntimeState) executeCPAWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
	ctx, cancel := context.WithTimeout(parent, warmupRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		s.recordWarmupNotSent(candidate, "cancelled_before_dispatch")
		return
	}

	keyRaw, err := os.ReadFile(cfg.CPAManagementKeyFile)
	if err != nil {
		s.recordWarmupNotSent(candidate, "management_key_unavailable")
		return
	}
	managementKey := strings.TrimSpace(string(keyRaw))
	if managementKey == "" {
		s.recordWarmupNotSent(candidate, "management_key_unavailable")
		return
	}
	// Auth files can be atomically replaced while a warmup worker is waiting
	// behind another account. Re-resolve immediately before api-call so a stale
	// auth_index is never intentionally sent to CPA. Keep AuthID stable because
	// the instance lease and persisted warmup key were admitted for that ID.
	bindings, err := s.cpaWarmupEligibleAuths(ctx, cfg)
	if err != nil {
		s.recordWarmupNotSent(candidate, "cpa_inventory_unavailable")
		return
	}
	binding := bindings[strings.TrimSpace(candidate.Snapshot.AuthID)]
	if strings.TrimSpace(binding.AuthID) == "" {
		binding = bindings[strings.TrimSpace(candidate.Snapshot.AuthIndex)]
	}
	if strings.TrimSpace(binding.AuthID) == "" || strings.TrimSpace(binding.AuthIndex) == "" {
		s.recordWarmupNotSent(candidate, "auth_binding_stale")
		return
	}
	if strings.TrimSpace(binding.AuthID) != strings.TrimSpace(candidate.Snapshot.AuthID) ||
		strings.TrimSpace(binding.AuthIndex) != strings.TrimSpace(candidate.Snapshot.AuthIndex) {
		s.recordWarmupNotSent(candidate, "auth_binding_changed")
		return
	}

	// Use the native Codex Responses shape; CPA owns credential substitution,
	// account proxy selection, and the outbound request. No model-router lease.
	payload, err := json.Marshal(map[string]any{
		"model":        cfg.WarmupModel,
		"instructions": "Reply with OK only.",
		"input":        []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "Ping"}}}},
		"stream":       true,
		"store":        false,
		"reasoning":    map[string]any{"effort": "low"},
		"text":         map[string]any{"verbosity": "low"},
	})
	if err != nil {
		s.recordWarmupNotSent(candidate, "invalid_warmup_request")
		return
	}
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$", "Accept": "text/event-stream",
		"Content-Type": "application/json", "User-Agent": "codex_cli_rs/cpa-quota-scheduler",
	}
	accountID := binding.AccountID
	if accountID == "" {
		accountID = candidate.Snapshot.AccountID
	}
	if accountID != "" {
		headers["ChatGPT-Account-Id"] = accountID
	}
	callBody, err := json.Marshal(cpaAPICallRequest{
		AuthIndex: candidate.Snapshot.AuthIndex,
		Method:    http.MethodPost,
		URL:       cpaWarmupResponsesURL,
		Header:    headers,
		Data:      string(payload),
	})
	if err != nil {
		s.recordWarmupNotSent(candidate, "invalid_warmup_request")
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CPAManagementURL, bytes.NewReader(callBody))
	if err != nil {
		s.recordWarmupNotSent(candidate, "invalid_management_url")
		return
	}
	req.Header.Set("Authorization", "Bearer "+managementKey)
	req.Header.Set("Content-Type", "application/json")
	requestedAt := time.Now()
	resp, err := (&http.Client{Timeout: warmupRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("CPA api-call failed: %w", err))
		return
	}
	candidate.RetryAt = warmupRetryDeadline(resp.Header, time.Now())
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, warmupMaxResponseBytes+1))
	_ = resp.Body.Close()
	if len(raw) > warmupMaxResponseBytes {
		readErr = errWarmupStreamIncomplete
	}
	if readErr != nil {
		s.recordWarmupError(candidate, resp.StatusCode, fmt.Errorf("read CPA api-call response: %w", readErr))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout {
			// CPA reports both transport and response-read failures as 502. The
			// model may already have run even though no api-call envelope arrived.
			s.recordWarmupError(candidate, resp.StatusCode, errWarmupStreamIncomplete)
		} else {
			s.recordWarmupError(candidate, resp.StatusCode, warmupHTTPStatusError(resp.StatusCode, raw, "CPA api-call"))
		}
		return
	}
	var result cpaAPICallResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("decode CPA api-call response: %w", err))
		return
	}
	responseHeaders := make(http.Header, len(result.Header))
	for key, values := range result.Header {
		for _, value := range values {
			responseHeaders.Add(key, value)
		}
	}
	windows := quotaWindowsFromHeaders(responseHeaders, time.Now())
	if retryAt := warmupRetryDeadline(responseHeaders, time.Now()); retryAt.After(candidate.RetryAt) {
		candidate.RetryAt = retryAt
	}
	if result.StatusCode == statusTooManyRequests {
		s.recordWarmup429(candidate, cfg, responseHeaders, requestedAt, "cpa_api_call")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		s.recordWarmupError(candidate, result.StatusCode, warmupHTTPStatusError(result.StatusCode, []byte(result.Body), "warmup upstream"))
		return
	}
	if _, err := parseWarmupResponse([]byte(result.Body)); err != nil {
		s.recordWarmupStreamError(candidate, cfg, result.StatusCode, responseHeaders, requestedAt, "cpa_api_call", err)
		return
	}
	s.recordWarmupOutcome(candidate, result.StatusCode, windows, nil)

	activated := 0
	for _, window := range windows {
		if !window.ResetAt.IsZero() && time.Now().Before(window.ResetAt) {
			activated++
		}
	}
	slog.Info("codex-quota-scheduler: Codex warmup completed",
		"auth_id", candidate.Snapshot.AuthID,
		"window", candidate.Window.Class,
		"status", result.StatusCode,
		"activated_windows", activated)
}

func warmupRetryDeadline(headers http.Header, now time.Time) time.Time {
	if delay := retryAfterDuration(headers, now); delay > 0 {
		return now.Add(delay)
	}
	return time.Time{}
}

func (s *schedulerRuntimeState) recordWarmupStreamError(candidate warmupCandidate, cfg pluginConfig, status int, headers http.Header, requestedAt time.Time, transport string, err error) {
	code, blocked := classifyWarmupFailure(status, err)
	if !blocked && (code == "usage_limit_reached" || code == "rate_limit_exceeded" || code == "insufficient_quota") {
		s.recordWarmup429(candidate, cfg, headers, requestedAt, transport)
		status = statusTooManyRequests
	}
	s.recordWarmupError(candidate, status, err)
}

func (s *schedulerRuntimeState) recordWarmup429(candidate warmupCandidate, cfg pluginConfig, headers http.Header, requestedAt time.Time, transport string) {
	now := time.Now()
	entry, authoritative := quarantineEntryFor429(headers, now, cfg.FallbackBan, cfg.MaxBan)
	banStore.record429(candidate.Snapshot.AuthID, entry, requestedAt)
	s.markSerialUnavailable(candidate.Snapshot.AuthID, "warmup_429", now)
	s.persistAfterBanChange()
	slog.Warn("codex-quota-scheduler: warmup received 429; credential quarantined",
		"auth_id", candidate.Snapshot.AuthID,
		"transport", transport,
		"kind", entry.Kind,
		"authoritative", authoritative,
		"window", entry.Window,
		"probe_ready_at", entry.ResetAt.Format(time.RFC3339))
}

func (s *schedulerRuntimeState) recordWarmupError(candidate warmupCandidate, status int, err error) {
	s.recordWarmupOutcome(candidate, status, nil, err)
	s.warmupMu.Lock()
	entry := s.warmups[warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)]
	s.warmupMu.Unlock()
	slog.Warn("codex-quota-scheduler: Codex warmup failed",
		"auth_id", candidate.Snapshot.AuthID,
		"window", candidate.Window.Class,
		"error_code", entry.Error,
		"retryable", !entry.Blocked)
}

// Only failures known to precede model dispatch get a normal retry interval.
// Network errors, cancellations during dispatch, and truncated output remain
// uncertain outcomes and keep their full duplicate-prevention suppression.
type warmupNotSentError struct{ Code string }

func (e *warmupNotSentError) Error() string { return e.Code }

func (s *schedulerRuntimeState) recordWarmupNotSent(candidate warmupCandidate, code string) {
	s.recordWarmupError(candidate, 0, &warmupNotSentError{Code: code})
}

func (s *schedulerRuntimeState) recordWarmupOutcome(candidate warmupCandidate, status int, windows []quotaWindow, err error) {
	now := time.Now()
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	// Preserve upstream Retry-After/reset evidence even after probation clears.
	ban, _ := banStore.lookup(candidate.Snapshot.AuthID)
	s.warmupMu.Lock()
	if s.warmups == nil {
		s.warmups = make(map[string]warmupEntry)
	}
	targetKey := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
	target := s.warmups[targetKey]
	target.AuthID = candidate.Snapshot.AuthID
	target.AuthIndex = candidate.Snapshot.AuthIndex
	target.Window = candidate.Window.Class
	target.Status = status
	target.OutcomeAt = now
	if err != nil {
		target.DispatchState = "rejected"
		target.Error, target.Blocked = classifyWarmupFailure(status, err)
		for _, previous := range s.warmups {
			if previous.AuthID == target.AuthID && previous.Failures > target.Failures {
				target.Failures = previous.Failures
			}
		}
		target.Failures++
		if target.Failures >= warmupFailureLimit {
			target.Blocked = true
		}
		target.CompletedAt = time.Time{}
		target.ActivatedAt = time.Time{}
		target.ResetAt = time.Time{}
		delay := warmupFailureDelay(cfg.WarmupRetryAfter, target.Failures)
		// Missing transport outcome or a truncated response can still have
		// consumed quota upstream. Do not repeat it after a short retry interval.
		var notSent *warmupNotSentError
		if errors.As(err, &notSent) {
			target.DispatchState = "not_sent"
		} else if status == 0 || strings.ReplaceAll(target.Error, ".", "_") == "response_incomplete" || errors.Is(err, errWarmupStreamIncomplete) {
			target.DispatchState = "uncertain"
			if delay < warmupUncertainDelay {
				delay = warmupUncertainDelay
			}
		}
		target.SuppressUntil = now.Add(delay)
		if ban.ResetAt.After(target.SuppressUntil) {
			target.SuppressUntil = ban.ResetAt
		}
		if candidate.RetryAt.After(target.SuppressUntil) {
			target.SuppressUntil = candidate.RetryAt
		}
	} else {
		target.DispatchState = "completed"
		target.Error = ""
		target.Blocked = false
		target.Failures = 0
		for key, previous := range s.warmups {
			if previous.AuthID == target.AuthID && !previous.Blocked {
				previous.Failures = 0
				s.warmups[key] = previous
			}
		}
	}
	if target.AttemptedAt.IsZero() {
		target.AttemptedAt = now
	}
	if err == nil && status >= 200 && status < 300 {
		target.CompletedAt = now
		target.SuppressUntil = now.Add(warmupFallbackWindow(candidate.Window))
	}
	s.warmups[targetKey] = target
	if err == nil && status >= 200 && status < 300 {
		// One successful generation request starts every unstarted quota window
		// attached to the same Codex workspace. Persist pending sibling entries
		// now instead of issuing a second low-cost request for weekly/monthly while
		// quota probe or the upstream response headers are still converging.
		for _, covered := range warmupActivationWindows(candidate.Snapshot, now) {
			class := normalizeWindowClass(covered.Class)
			if class == "" {
				continue
			}
			key := warmupKey(candidate.Snapshot.AuthID, class)
			entry := s.warmups[key]
			if entry.Blocked || (!entry.ActivatedAt.IsZero() && !entry.ResetAt.IsZero() && now.Before(entry.ResetAt)) {
				continue
			}
			entry.AuthID = candidate.Snapshot.AuthID
			entry.AuthIndex = candidate.Snapshot.AuthIndex
			entry.Window = class
			entry.AttemptedAt = target.AttemptedAt
			entry.CompletedAt = target.CompletedAt
			entry.OutcomeAt = target.OutcomeAt
			entry.Failures = 0
			entry.ActivatedAt = time.Time{}
			entry.ResetAt = time.Time{}
			entry.SuppressUntil = now.Add(warmupFallbackWindow(covered))
			entry.Status = status
			entry.DispatchState = "completed"
			entry.Error = ""
			entry.Blocked = false
			s.warmups[key] = entry
		}
	}
	for _, window := range windows {
		if window.ResetAt.IsZero() || !now.Before(window.ResetAt) || quotaWindowHasPlaceholderReset(window, now, now) {
			continue
		}
		key := warmupKey(candidate.Snapshot.AuthID, window.Class)
		entry := s.warmups[key]
		if entry.Blocked {
			continue
		}
		entry.AuthID = candidate.Snapshot.AuthID
		entry.AuthIndex = candidate.Snapshot.AuthIndex
		entry.Window = window.Class
		entry.AttemptedAt = target.AttemptedAt
		entry.CompletedAt = target.CompletedAt
		entry.OutcomeAt = target.OutcomeAt
		entry.Failures = 0
		entry.ActivatedAt = now
		entry.ResetAt = window.ResetAt
		entry.SuppressUntil = window.ResetAt
		entry.Status = status
		entry.DispatchState = "completed"
		entry.Error = ""
		s.warmups[key] = entry
	}
	if current, ok := s.warmups[targetKey]; ok {
		target = current
	}
	// Without an upstream reset header this remains pending_confirmation. The
	// local suppress_until prevents duplicate low-cost calls, while the next
	// fresh quota probe snapshot supplies the real reset anchor shown in status.
	s.warmups[targetKey] = target
	s.warmupMu.Unlock()
	s.persistBanState()
}

// warmupActivationWindows returns every recognized window that the admitted
// request is expected to start. A single generation activates the workspace,
// not just the highest-priority row selected as the scheduling label.
func warmupActivationWindows(snapshot quotaSnapshot, now time.Time) []quotaWindow {
	covered := make([]quotaWindow, 0, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		class := normalizeWindowClass(window.Class)
		if class == "" || !quotaWindowNeedsActivation(window, snapshot.RefreshedAt, now) {
			continue
		}
		window.Class = class
		covered = append(covered, window)
	}
	return covered
}

func classifyWarmupFailure(status int, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var notSent *warmupNotSentError
	if errors.As(err, &notSent) {
		return notSent.Code, notSent.Code == "invalid_warmup_model" || notSent.Code == "invalid_warmup_request" || notSent.Code == "invalid_management_url"
	}
	// These status semantics are authoritative even if an inconsistent body
	// supplies a different error code. A 429 is governed only by the quota/
	// probation quarantine; 401/403 must never be retried automatically.
	if status == statusTooManyRequests {
		return "http_429", false
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Sprintf("http_%d", status), true
	}
	var terminal *warmupStreamTerminalError
	if errors.As(err, &terminal) {
		code := sanitizeWarmupCode(terminal.Code)
		if canonical := canonicalNonRetryableWarmupCode(code); canonical != "" {
			return canonical, true
		}
		if code == "" {
			code = sanitizeWarmupCode(terminal.Event)
		}
		if code == "" {
			code = "response_failed"
		}
		return code, nonRetryableWarmupCode(code)
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", false
	}
	if errors.Is(err, errWarmupStreamIncomplete) {
		return "response_incomplete", false
	}
	lower := strings.ToLower(err.Error())
	for _, code := range []string{"auth_binding_stale", "auth_binding_changed"} {
		if strings.Contains(lower, code) {
			return code, false
		}
	}
	for _, code := range []string{
		"cyber_policy", "cyber_abuse", "abuse", "deactivated_workspace",
		"workspace_deactivated", "account_deactivated", "invalid_refresh_token",
		"invalid_api_key", "auth_unavailable", "no_auth_available", "unauthorized", "forbidden",
	} {
		if strings.Contains(lower, code) || (code == "no_auth_available" && strings.Contains(lower, "no auth available")) {
			return code, true
		}
	}
	if status > 0 {
		code := fmt.Sprintf("http_%d", status)
		blocked := status == http.StatusUnauthorized || status == http.StatusForbidden ||
			(status >= 400 && status < 500 && status != http.StatusRequestTimeout &&
				status != http.StatusConflict && status != http.StatusTooEarly && status != statusTooManyRequests)
		return code, blocked
	}
	return "warmup_failed", false
}

func nonRetryableWarmupCode(code string) bool {
	return canonicalNonRetryableWarmupCode(code) != ""
}

func canonicalNonRetryableWarmupCode(code string) string {
	code = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(code)), "-", "_")
	for _, marker := range []string{
		"cyber_policy", "cyber_abuse", "abuse", "deactivated_workspace",
		"workspace_deactivated", "account_deactivated", "invalid_refresh_token",
		"invalid_api_key", "auth_unavailable", "no_auth_available", "unauthorized", "forbidden",
	} {
		if strings.Contains(code, marker) {
			return marker
		}
	}
	return ""
}

func sanitizeWarmupCode(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range raw {
		if out.Len() >= 80 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_.-")
}

func warmupFallbackWindow(window quotaWindow) time.Duration {
	if window.WindowSeconds > 0 {
		return time.Duration(window.WindowSeconds) * time.Second
	}
	if window.Class == "weekly" {
		return 7 * 24 * time.Hour
	}
	if window.Class == "monthly" {
		return 30 * 24 * time.Hour
	}
	return 5 * time.Hour
}

func (s *schedulerRuntimeState) recordWarmupAuthDiagnostics(source string, stats warmupAuthEligibilityStats, err error) {
	lastError := ""
	if err != nil {
		lastError, _ = classifyWarmupFailure(0, err)
	}
	rejected := make(map[string]int, len(stats.Rejected))
	for reason, count := range stats.Rejected {
		rejected[reason] = count
	}
	s.mu.Lock()
	s.warmupAuthSourceLast = source
	s.warmupAuthCheckedAt = time.Now()
	s.warmupAuthFilesSeenLast = stats.Seen
	s.warmupAuthEligibleLast = stats.Eligible
	s.warmupAuthRejectedLast = rejected
	s.warmupAuthLastError = lastError
	s.mu.Unlock()
}

func (s *schedulerRuntimeState) cpaWarmupEligibleAuths(ctx context.Context, cfg pluginConfig) (map[string]warmupAuthBinding, error) {
	files, err := cpaManagementAuthFiles(ctx, cfg)
	if err != nil {
		s.recordWarmupAuthDiagnostics("management.auth-files", newWarmupAuthEligibilityStats(), err)
		return nil, err
	}
	eligible, stats := warmupEligibleAuthsWithStats(files)
	s.recordWarmupAuthDiagnostics("management.auth-files", stats, nil)
	return eligible, nil
}

// cpaManagementAuthFiles reads the authenticated CPA auth inventory without
// applying warmup-specific transport rules. Quota refresh uses the same raw
// inventory to fail closed before asking quota probe to touch a credential.
func cpaManagementAuthFiles(ctx context.Context, cfg pluginConfig) ([]cpaAuthFileEntry, error) {
	keyRaw, err := os.ReadFile(cfg.CPAManagementKeyFile)
	if err != nil {
		return nil, err
	}
	managementKey := strings.TrimSpace(string(keyRaw))
	if managementKey == "" {
		return nil, errors.New("CPA management key is empty")
	}
	endpoint, err := cpaAuthFilesEndpoint(cfg.CPAManagementURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+managementKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("CPA auth-files returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Files []cpaAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result.Files, nil
}

// cpaActiveCodexAuthIndexes returns the host's current routing inventory. It
// uses the CPA credential status, without inspecting free-form notes.
func cpaActiveCodexAuthIndexes(ctx context.Context, cfg pluginConfig) (map[string]struct{}, error) {
	if hostAPIAvailable() {
		result, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
		if err == nil {
			var response struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}
			if decodeErr := json.Unmarshal(result, &response); decodeErr == nil {
				// A valid empty inventory is authoritative: all Codex auths may
				// have been disabled. Do not fall back to a potentially stale view.
				return activeCodexHostAuthIndexes(response.Files), nil
			}
		}
	}

	files, err := cpaManagementAuthFiles(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return activeCodexManagementAuthIndexes(files), nil
}

func activeCodexManagementAuthIndexes(files []cpaAuthFileEntry) map[string]struct{} {
	indexes := make(map[string]struct{})
	for _, file := range files {
		provider := strings.TrimSpace(file.Provider)
		if provider == "" {
			provider = strings.TrimSpace(file.Type)
		}
		if !strings.EqualFold(provider, providerCodex) || file.Disabled || file.Unavailable {
			continue
		}
		if status := strings.TrimSpace(file.Status); status != "" && !strings.EqualFold(status, "active") {
			continue
		}
		if index := strings.TrimSpace(file.AuthIndex); index != "" {
			indexes[index] = struct{}{}
		}
	}
	return indexes
}

func activeCodexHostAuthIndexes(files []pluginapi.HostAuthFileEntry) map[string]struct{} {
	indexes := make(map[string]struct{})
	for _, file := range files {
		provider := strings.TrimSpace(file.Provider)
		if provider == "" {
			provider = strings.TrimSpace(file.Type)
		}
		if !strings.EqualFold(provider, providerCodex) || file.Disabled || file.Unavailable {
			continue
		}
		if status := strings.TrimSpace(file.Status); status != "" && !strings.EqualFold(status, "active") {
			continue
		}
		if index := strings.TrimSpace(file.AuthIndex); index != "" {
			indexes[index] = struct{}{}
		}
	}
	return indexes
}

func (s *schedulerRuntimeState) clearBlockedWarmupState(authID string, all bool) int {
	authID = strings.TrimSpace(authID)
	removed := 0
	s.warmupMu.Lock()
	for key, entry := range s.warmups {
		// Bulk recovery only lifts explicit blocks. A failed/uncertain outcome
		// requires selecting its exact account; successful cycles stay recorded.
		if (!entry.Blocked && (all || entry.Error == "")) || (!all && strings.TrimSpace(entry.AuthID) != authID && !strings.HasPrefix(key, authID+"|")) {
			continue
		}
		delete(s.warmups, key)
		removed++
	}
	s.warmupMu.Unlock()
	return removed
}

func warmupEligibleAuths(files []cpaAuthFileEntry) map[string]warmupAuthBinding {
	eligible, _ := warmupEligibleAuthsWithStats(files)
	return eligible
}

func warmupEligibleAuthsWithStats(files []cpaAuthFileEntry) (map[string]warmupAuthBinding, warmupAuthEligibilityStats) {
	return eligibleCPACodexAuthsWithStats(files)
}

func eligibleCPACodexAuthsWithStats(files []cpaAuthFileEntry) (map[string]warmupAuthBinding, warmupAuthEligibilityStats) {
	eligible := make(map[string]warmupAuthBinding)
	stats := newWarmupAuthEligibilityStats()
	for _, file := range files {
		stats.Seen++
		provider := strings.TrimSpace(file.Provider)
		if provider == "" {
			provider = strings.TrimSpace(file.Type)
		}
		if !strings.EqualFold(provider, providerCodex) {
			stats.reject("provider_mismatch")
			continue
		}
		if file.Disabled {
			stats.reject("disabled")
			continue
		}
		if file.Unavailable {
			stats.reject("unavailable")
			continue
		}
		if status := strings.TrimSpace(file.Status); status != "" && !strings.EqualFold(status, "active") {
			stats.reject("inactive_status")
			continue
		}
		authIndex := strings.TrimSpace(file.AuthIndex)
		authID := strings.TrimSpace(file.ID)
		if authID == "" {
			authID = strings.TrimSpace(file.Name)
		}
		if authID == "" {
			stats.reject("missing_auth_id")
			continue
		}
		if authIndex == "" {
			stats.reject("missing_auth_index")
			continue
		}
		binding := warmupAuthBinding{AuthID: authID, AuthIndex: authIndex, AccountID: file.IDToken.AccountID}
		for _, key := range []string{file.ID, file.AuthIndex, file.Name} {
			if key = strings.TrimSpace(key); key != "" {
				eligible[key] = binding
			}
		}
		stats.Eligible++
	}
	return eligible, stats
}

func cpaAuthFilesEndpoint(apiCallEndpoint string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(apiCallEndpoint))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("invalid CPA management URL")
	}
	path := strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(path, "/api-call") {
		path = strings.TrimSuffix(path, "/api-call")
	}
	u.Path = path + "/auth-files"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

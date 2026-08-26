package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
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
	CompletedAt   time.Time `json:"completed_at,omitempty"`
	ActivatedAt   time.Time `json:"activated_at,omitempty"`
	ResetAt       time.Time `json:"reset_at,omitempty"`
	SuppressUntil time.Time `json:"suppress_until,omitempty"`
	Status        int       `json:"status,omitempty"`
	Error         string    `json:"error,omitempty"`
	Blocked       bool      `json:"blocked,omitempty"`
}

type warmupCandidate struct {
	Snapshot quotaSnapshot
	Window   quotaWindow
}

type warmupAuthBinding struct {
	AuthID    string
	AuthIndex string
}

type warmupLease struct {
	AuthID    string
	ExpiresAt time.Time
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

// trustedWarmupCredentialNote accepts only the canonical notes emitted by
// Codex Agent Identity. Exact matching keeps official/native credentials and
// lookalike notes from being routed through the privileged sidecar gateway.
func trustedWarmupCredentialNote(note string) bool {
	switch strings.ToLower(strings.TrimSpace(note)) {
	case "agent identity via sidecar",
		"codex access token via sidecar",
		"agent identity via gateway",
		"codex access token via gateway":
		return true
	default:
		return false
	}
}

const (
	warmupMinimumAvailablePercent = 0.000001
	warmupMinimumUsageCredits     = 0.000001
	warmupResetPlaceholderSkew    = 3 * time.Second
	warmupStaleActivationGrace    = 30 * time.Minute
	warmupStartupGrace            = 15 * time.Second
	warmupMaxResponseBytes        = 2 << 20
	warmupRequestTimeout          = 45 * time.Second
)

// warmupStartupReady keeps activation traffic away from CPA while a newly
// claimed plugin generation is still starting. The next Keeper refresh will
// retry after CPA's API server, auth registry, and Agent Identity proxy have
// had time to settle.
func (s *schedulerRuntimeState) warmupStartupReady(now time.Time) bool {
	ownership := s.generationSnapshot()
	if !ownership.Managed || ownership.ClaimedAt.IsZero() {
		return true
	}
	return !now.Before(ownership.ClaimedAt.Add(warmupStartupGrace))
}

// scheduleWarmup is called after a fresh Keeper snapshot. It deliberately
// schedules at most one request at a time so full accounts are activated
// sequentially instead of creating a burst across the pool.
func (s *schedulerRuntimeState) scheduleWarmup(parent context.Context, skipAuthIDs map[string]struct{}) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if !cfg.Enabled || !cfg.WarmupEnabled {
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
	if nativeWarmupRequested(cfg) {
		if !hostAPIAvailable() {
			slog.Warn("codex-quota-scheduler: native warmup skipped because CPA HostModel is unavailable")
			return
		}
	} else if strings.TrimSpace(cfg.CPAManagementURL) == "" ||
		strings.TrimSpace(cfg.CPAManagementKeyFile) == "" || strings.TrimSpace(cfg.WarmupSidecarURL) == "" {
		return
	}
	if s.pruneExpiredWarmups(now) {
		s.persistBanState()
	}

	eligible, err := s.cpaWarmupEligibleAuths(parent, cfg)
	if err != nil {
		code, _ := classifyWarmupFailure(0, err)
		slog.Warn("codex-quota-scheduler: warmup skipped because CPA auth status is unavailable", "error_code", code)
		return
	}
	if !s.generationOwnerActive() {
		return
	}
	candidates := s.findWarmupCandidates(eligible, skipAuthIDs, time.Now())
	if len(candidates) == 0 {
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
	s.warmupMu.Lock()
	if s.warmupRunning {
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
	s.warmups[key] = warmupEntry{
		AuthID:      candidate.Snapshot.AuthID,
		AuthIndex:   candidate.Snapshot.AuthIndex,
		Window:      candidate.Window.Class,
		AttemptedAt: now,
	}
	s.warmupRunning = true
	s.warmupMu.Unlock()
	s.persistBanState()

	go func() {
		defer s.wg.Done()
		defer releaseInstanceLease()
		defer func() {
			s.warmupMu.Lock()
			s.warmupRunning = false
			s.warmupMu.Unlock()
		}()
		executed := false
		if s.generationOwnerActive() {
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
		// CPA's current auth index is authoritative. Keeper snapshots can retain
		// an older index after Agent Identity replaces a Team workspace auth file.
		snapshot.AuthID = binding.AuthID
		snapshot.AuthIndex = binding.AuthIndex
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
	for _, candidate := range candidates {
		key := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
		entry, ok := s.warmups[key]
		if !ok || staleWarmupState(entry, candidate, now, retryAfter) || !warmupEntrySuppressesNow(entry, now, retryAfter) {
			count++
		}
	}
	return count
}

func warmupEntrySuppressesNow(entry warmupEntry, now time.Time, retryAfter time.Duration) bool {
	if entry.Blocked {
		return true
	}
	if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) {
		return false
	}
	if !entry.ActivatedAt.IsZero() && !entry.ResetAt.IsZero() && now.Before(entry.ResetAt) {
		return true
	}
	if !entry.CompletedAt.IsZero() && !entry.SuppressUntil.IsZero() && now.Before(entry.SuppressUntil) {
		return true
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	return !entry.AttemptedAt.IsZero() && now.Sub(entry.AttemptedAt) < retryAfter
}

// warmupSnapshotFresh is intentionally stricter than the ordinary scheduling
// freshness check. A partial Keeper response can carry a missing window forward
// under a newer outer RefreshedAt. That is useful for routing continuity, but an
// activation request must never be admitted from such an indefinitely carried
// 0% row: every recognized window needs its own fresh observation.
func warmupSnapshotFresh(snapshot quotaSnapshot, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
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
	// Keeper reports a future reset even for a window that has not started yet:
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
			// Keeper intentionally preserves additional/future quota rows. An
			// unrelated unclassified meter must not suppress activation of a
			// recognized 5h/weekly/monthly Codex window.
			continue
		}
		// Evaluate each recognized window independently. A weekly/monthly row
		// that has already started must not suppress a fresh 5h window on the
		// same auth. Likewise, one exhausted row must not hide another window
		// that is still waiting for its first activation.
		if window.UsedPercent > warmupMinimumAvailablePercent || window.LimitReached || !window.Allowed {
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

func (s *schedulerRuntimeState) registerWarmupLease(authID string, now time.Time) (string, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", errors.New("warmup auth id is empty")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate warmup lease: %w", err)
	}
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	s.warmupMu.Lock()
	if s.warmupLeases == nil {
		s.warmupLeases = make(map[string]warmupLease)
	}
	for key, lease := range s.warmupLeases {
		if !now.Before(lease.ExpiresAt) {
			delete(s.warmupLeases, key)
		}
	}
	s.warmupLeases[nonce] = warmupLease{AuthID: authID, ExpiresAt: now.Add(warmupRequestTimeout)}
	s.warmupMu.Unlock()
	return nonce, nil
}

func (s *schedulerRuntimeState) consumeWarmupLease(nonce string, now time.Time) (string, bool) {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" {
		return "", false
	}
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	lease, ok := s.warmupLeases[nonce]
	delete(s.warmupLeases, nonce)
	if !ok || !now.Before(lease.ExpiresAt) {
		return "", false
	}
	return lease.AuthID, true
}

func (s *schedulerRuntimeState) releaseWarmupLease(nonce string) {
	s.warmupMu.Lock()
	delete(s.warmupLeases, strings.TrimSpace(nonce))
	s.warmupMu.Unlock()
}

// nextWarmupCandidateLocked skips accounts already activated or recently
// attempted, allowing later full accounts to make progress on the next refresh.
// The caller must hold warmupMu.
func (s *schedulerRuntimeState) nextWarmupCandidateLocked(candidates []warmupCandidate, now time.Time, retryAfter time.Duration) (warmupCandidate, string, bool) {
	return s.nextWarmupCandidateForGenerationLocked(candidates, now, retryAfter, time.Time{})
}

// nextWarmupCandidateForGenerationLocked additionally permits one retry of an
// unfinished, retryable outcome inherited from a previous generation. Once the
// new generation records its own attempt, ordinary retry_after suppression
// applies again.
func (s *schedulerRuntimeState) nextWarmupCandidateForGenerationLocked(candidates []warmupCandidate, now time.Time, retryAfter time.Duration, generationClaimedAt time.Time) (warmupCandidate, string, bool) {
	for _, candidate := range candidates {
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
		return strings.TrimSpace(entry.AuthIndex) != "" && strings.TrimSpace(candidate.Snapshot.AuthIndex) != "" &&
			strings.TrimSpace(entry.AuthIndex) != strings.TrimSpace(candidate.Snapshot.AuthIndex)
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
		if entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() {
			continue
		}
		snapshot, ok := canonical[strings.TrimSpace(entry.AuthID)]
		if !ok || !snapshot.RefreshedAt.After(entry.CompletedAt) {
			continue
		}
		for _, window := range snapshot.Windows {
			if normalizeWindowClass(window.Class) != normalizeWindowClass(entry.Window) || window.ResetAt.IsZero() ||
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
			slog.Info("codex-quota-scheduler: confirmed warmup reset anchor from Keeper",
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
	if retryableWarmupFromPriorGeneration(entry, generationClaimedAt) {
		delete(s.warmups, key)
		slog.Info("codex-quota-scheduler: retrying unfinished warmup from previous generation",
			"auth_id", entry.AuthID,
			"window", entry.Window)
		return false
	}
	// A lifecycle reconfigure cancels the refresh-loop context while the old
	// generation is retiring. That cancellation is not an upstream failure and
	// must not suppress the same candidate in the newly active generation. A
	// non-zero status proves that an upstream HTTP response was received, so it
	// must still obey the ordinary retry interval even if its error was reported
	// as a cancellation.
	if entry.Status == 0 && entry.Error == "cancelled" && entry.CompletedAt.IsZero() && entry.ActivatedAt.IsZero() {
		delete(s.warmups, key)
		return false
	}
	if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) {
		delete(s.warmups, key)
		return false
	}
	if !entry.ActivatedAt.IsZero() && !entry.ResetAt.IsZero() && now.Before(entry.ResetAt) {
		return true
	}
	if !entry.CompletedAt.IsZero() && !entry.SuppressUntil.IsZero() && now.Before(entry.SuppressUntil) {
		return true
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	return !entry.AttemptedAt.IsZero() && now.Sub(entry.AttemptedAt) < retryAfter
}

func retryableWarmupFromPriorGeneration(entry warmupEntry, generationClaimedAt time.Time) bool {
	if generationClaimedAt.IsZero() || entry.Blocked || entry.AttemptedAt.IsZero() ||
		!entry.AttemptedAt.Before(generationClaimedAt) {
		return false
	}
	if !entry.CompletedAt.IsZero() || !entry.ActivatedAt.IsZero() || !entry.ResetAt.IsZero() {
		return false
	}
	// Any HTTP status, including 5xx or a 2xx response followed by an SSE
	// terminal error, proves the request produced a completed upstream outcome.
	// Generation churn must not turn that outcome into an immediate retry; the
	// normal AttemptedAt/retry_after backoff remains authoritative.
	if entry.Status != 0 {
		return false
	}
	// Status-free transport/encoding failures are also completed attempts and
	// must respect backoff. Only an admitted attempt with no recorded outcome,
	// or a lifecycle cancellation, is genuinely unfinished/interrupted and safe
	// to resume immediately in the next generation.
	errorCode := strings.ToLower(strings.TrimSpace(entry.Error))
	return errorCode == "" || errorCode == "cancelled"
}

func (s *schedulerRuntimeState) executeWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
	model, err := validateWarmupModel(cfg.WarmupModel)
	if err != nil {
		s.recordWarmupError(candidate, 0, err)
		return
	}
	cfg.WarmupModel = model
	if nativeWarmupRequested(cfg) {
		s.executeNativeWarmup(parent, cfg, candidate)
		return
	}
	s.executeManagementWarmup(parent, cfg, candidate)
}

func (s *schedulerRuntimeState) executeNativeWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
	if err := parent.Err(); err != nil {
		s.recordWarmupError(candidate, 0, err)
		return
	}
	nonce, err := s.registerWarmupLease(candidate.Snapshot.AuthID, time.Now())
	if err != nil {
		s.recordWarmupError(candidate, 0, err)
		return
	}
	defer s.releaseWarmupLease(nonce)

	body, err := json.Marshal(map[string]any{
		"model":             cfg.WarmupModel,
		"input":             "hello",
		"stream":            false,
		"store":             false,
		"max_output_tokens": 16,
	})
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("encode native warmup request: %w", err))
		return
	}
	requestedAt := time.Now()
	result, err := callHost(pluginabi.MethodHostModelExecute, pluginapi.HostModelExecutionRequest{
		EntryProtocol: "openai-response",
		ExitProtocol:  "openai-response",
		Model:         cfg.WarmupModel,
		Stream:        false,
		Body:          body,
		Headers: http.Header{
			warmupRequestHeader: []string{nonce},
		},
	})
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("native CPA warmup failed: %w", err))
		return
	}
	var response pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(result, &response); err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("decode native CPA warmup response: %w", err))
		return
	}
	windows := quotaWindowsFromHeaders(response.Headers, time.Now())
	if response.StatusCode == statusTooManyRequests {
		s.recordWarmup429(candidate, cfg, response.Headers, requestedAt, "native")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.recordWarmupError(candidate, response.StatusCode, warmupHTTPStatusError(response.StatusCode, response.Body, "native CPA warmup"))
		return
	}
	if _, err := parseWarmupResponse(response.Body); err != nil {
		s.recordWarmupError(candidate, response.StatusCode, err)
		return
	}
	s.recordWarmupOutcome(candidate, response.StatusCode, windows, nil)
	slog.Info("codex-quota-scheduler: native CPA Codex warmup completed",
		"auth_id", candidate.Snapshot.AuthID,
		"window", candidate.Window.Class,
		"status", response.StatusCode,
		"activated_windows", len(windows))
}

func (s *schedulerRuntimeState) executeManagementWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
	ctx, cancel := context.WithTimeout(parent, warmupRequestTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		s.recordWarmupError(candidate, 0, err)
		return
	}

	keyRaw, err := os.ReadFile(cfg.CPAManagementKeyFile)
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("read CPA management key: %w", err))
		return
	}
	managementKey := strings.TrimSpace(string(keyRaw))
	if managementKey == "" {
		s.recordWarmupError(candidate, 0, errors.New("CPA management key is empty"))
		return
	}
	// Auth files can be atomically replaced while a warmup worker is waiting
	// behind another account. Re-resolve immediately before api-call so a stale
	// auth_index is never intentionally sent to CPA. Keep AuthID stable because
	// the instance lease and persisted warmup key were admitted for that ID.
	bindings, err := s.cpaWarmupEligibleAuths(ctx, cfg)
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("refresh warmup auth binding: %w", err))
		return
	}
	binding := bindings[strings.TrimSpace(candidate.Snapshot.AuthID)]
	if strings.TrimSpace(binding.AuthID) == "" {
		binding = bindings[strings.TrimSpace(candidate.Snapshot.AuthIndex)]
	}
	if strings.TrimSpace(binding.AuthID) == "" || strings.TrimSpace(binding.AuthIndex) == "" {
		s.recordWarmupError(candidate, 0, errors.New("auth_binding_stale"))
		return
	}
	if strings.TrimSpace(binding.AuthID) != strings.TrimSpace(candidate.Snapshot.AuthID) {
		s.recordWarmupError(candidate, 0, errors.New("auth_binding_changed"))
		return
	}
	candidate.Snapshot.AuthIndex = strings.TrimSpace(binding.AuthIndex)

	payload, err := json.Marshal(map[string]any{
		"model": cfg.WarmupModel,
		"input": []map[string]any{
			{
				"type":  "additional_tools",
				"role":  "developer",
				"tools": []any{},
			},
			{
				"type": "message",
				"role": "developer",
				"content": []map[string]any{{
					"type": "input_text",
					"text": "Reply briefly.",
				}},
			},
			{
				"type": "message",
				"role": "user",
				"content": []map[string]any{{
					"type": "input_text",
					"text": "hello",
				}},
			},
		},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]any{"effort": "low", "context": "all_turns"},
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
		"text":                map[string]any{"verbosity": "low"},
	})
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("encode warmup request: %w", err))
		return
	}
	callBody, err := json.Marshal(cpaAPICallRequest{
		AuthIndex: candidate.Snapshot.AuthIndex,
		Method:    http.MethodPost,
		URL:       strings.TrimRight(cfg.WarmupSidecarURL, "/") + "/responses",
		Header: map[string]string{
			"Authorization":                          "Bearer $TOKEN$",
			"Accept":                                 "text/event-stream",
			"Content-Type":                           "application/json",
			"Originator":                             "codex_cli_rs",
			"User-Agent":                             "codex_cli_rs/cpa-quota-scheduler",
			"X-Codex-Routing-Hint":                   "model=" + cfg.WarmupModel,
			"X-OpenAI-Internal-Codex-Responses-Lite": "true",
		},
		Data: string(payload),
	})
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("encode CPA api-call: %w", err))
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CPAManagementURL, bytes.NewReader(callBody))
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("build CPA api-call: %w", err))
		return
	}
	req.Header.Set("Authorization", "Bearer "+managementKey)
	req.Header.Set("Content-Type", "application/json")
	requestedAt := time.Now()
	resp, err := (&http.Client{Timeout: warmupRequestTimeout}).Do(req)
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("CPA api-call failed: %w", err))
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, warmupMaxResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		s.recordWarmupError(candidate, resp.StatusCode, fmt.Errorf("read CPA api-call response: %w", readErr))
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.recordWarmupError(candidate, resp.StatusCode, warmupHTTPStatusError(resp.StatusCode, raw, "CPA api-call"))
		return
	}
	var result cpaAPICallResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("decode CPA api-call response: %w", err))
		return
	}
	headers := make(http.Header, len(result.Header))
	for key, values := range result.Header {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	windows := quotaWindowsFromHeaders(headers, time.Now())
	if result.StatusCode == statusTooManyRequests {
		s.recordWarmup429(candidate, cfg, headers, requestedAt, "management")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		s.recordWarmupError(candidate, result.StatusCode, warmupHTTPStatusError(result.StatusCode, []byte(result.Body), "warmup upstream"))
		return
	}
	if _, err := parseWarmupResponse([]byte(result.Body)); err != nil {
		s.recordWarmupError(candidate, result.StatusCode, err)
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
	code, blocked := classifyWarmupFailure(status, err)
	slog.Warn("codex-quota-scheduler: Codex warmup failed",
		"auth_id", candidate.Snapshot.AuthID,
		"window", candidate.Window.Class,
		"error_code", code,
		"retryable", !blocked)
}

func (s *schedulerRuntimeState) recordWarmupOutcome(candidate warmupCandidate, status int, windows []quotaWindow, err error) {
	now := time.Now()
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
	if err != nil {
		target.Error, target.Blocked = classifyWarmupFailure(status, err)
		target.CompletedAt = time.Time{}
		target.ActivatedAt = time.Time{}
		target.ResetAt = time.Time{}
		target.SuppressUntil = time.Time{}
	} else {
		target.Error = ""
		target.Blocked = false
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
		// Keeper or the upstream response headers are still converging.
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
			entry.ActivatedAt = time.Time{}
			entry.ResetAt = time.Time{}
			entry.SuppressUntil = now.Add(warmupFallbackWindow(covered))
			entry.Status = status
			entry.Error = ""
			entry.Blocked = false
			s.warmups[key] = entry
		}
	}
	for _, window := range windows {
		if window.ResetAt.IsZero() || !now.Before(window.ResetAt) {
			continue
		}
		key := warmupKey(candidate.Snapshot.AuthID, window.Class)
		entry := s.warmups[key]
		entry.AuthID = candidate.Snapshot.AuthID
		entry.AuthIndex = candidate.Snapshot.AuthIndex
		entry.Window = window.Class
		entry.AttemptedAt = target.AttemptedAt
		entry.CompletedAt = target.CompletedAt
		entry.ActivatedAt = now
		entry.ResetAt = window.ResetAt
		entry.SuppressUntil = window.ResetAt
		entry.Status = status
		entry.Error = ""
		s.warmups[key] = entry
	}
	if current, ok := s.warmups[targetKey]; ok {
		target = current
	}
	// Without an upstream reset header this remains pending_confirmation. The
	// local suppress_until prevents duplicate low-cost calls, while the next
	// fresh Keeper snapshot supplies the real reset anchor shown in status.
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
	native := nativeWarmupRequested(cfg)
	if hostAPIAvailable() {
		result, err := callHost(pluginabi.MethodHostAuthList, map[string]any{})
		if err == nil {
			var response struct {
				Files []pluginapi.HostAuthFileEntry `json:"files"`
			}
			if decodeErr := json.Unmarshal(result, &response); decodeErr == nil {
				eligible, stats := warmupEligibleHostAuthsWithStats(response.Files, !native)
				// Older CPA hosts may omit Note from host.auth.list. In management
				// mode, fall back to the authenticated auth-files route only when
				// that omission is the sole reason otherwise-valid Codex auths were
				// rejected. Native mode never falls back across transports.
				if native || stats.Eligible > 0 || stats.Rejected["missing_sidecar_marker"] == 0 {
					s.recordWarmupAuthDiagnostics("host.auth.list", stats, nil)
					return eligible, nil
				}
			} else if native {
				err = fmt.Errorf("decode native host.auth.list: %w", decodeErr)
			}
		}
		if native {
			if err == nil {
				err = errors.New("native host.auth.list returned unusable auth metadata")
			} else {
				err = fmt.Errorf("native host.auth.list failed: %w", err)
			}
			s.recordWarmupAuthDiagnostics("host.auth.list", newWarmupAuthEligibilityStats(), err)
			return nil, err
		}
	} else if native {
		err := errors.New("native CPA host callback API is unavailable")
		s.recordWarmupAuthDiagnostics("host.auth.list", newWarmupAuthEligibilityStats(), err)
		return nil, err
	}

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
// inventory to fail closed before asking Keeper to touch a credential.
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
// deliberately does not require an Agent Identity note: official OAuth and
// sidecar-backed PAT credentials are equally valid quota-refresh subjects.
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

func nativeWarmupRequested(cfg pluginConfig) bool {
	return normalizeWarmupExecutionMode(cfg.WarmupExecutionMode) == "native"
}

func (s *schedulerRuntimeState) clearBlockedWarmupState(authID string, all bool) int {
	authID = strings.TrimSpace(authID)
	removed := 0
	s.warmupMu.Lock()
	for key, entry := range s.warmups {
		if !entry.Blocked || (!all && strings.TrimSpace(entry.AuthID) != authID && !strings.HasPrefix(key, authID+"|")) {
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
	return eligibleCPACodexAuthsWithStats(files, true)
}

func eligibleCPACodexAuthsWithStats(files []cpaAuthFileEntry, requireSidecarMarker bool) (map[string]warmupAuthBinding, warmupAuthEligibilityStats) {
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
		// The pinned management request targets the Agent Identity gateway.
		// Requiring an exact canonical Identity note prevents a future native
		// OAuth credential from being sent to the wrong authentication endpoint.
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
		if requireSidecarMarker && !trustedWarmupCredentialNote(file.Note) {
			stats.reject("missing_sidecar_marker")
			continue
		}
		binding := warmupAuthBinding{AuthID: authID, AuthIndex: authIndex}
		for _, key := range []string{file.ID, file.AuthIndex, file.Name} {
			if key = strings.TrimSpace(key); key != "" {
				eligible[key] = binding
			}
		}
		stats.Eligible++
	}
	return eligible, stats
}

func warmupEligibleHostAuths(files []pluginapi.HostAuthFileEntry) map[string]warmupAuthBinding {
	eligible, _ := warmupEligibleHostAuthsWithStats(files, false)
	return eligible
}

func warmupEligibleHostAuthsWithStats(files []pluginapi.HostAuthFileEntry, requireSidecarMarker bool) (map[string]warmupAuthBinding, warmupAuthEligibilityStats) {
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
		status := strings.TrimSpace(file.Status)
		authID := strings.TrimSpace(file.ID)
		if authID == "" {
			authID = strings.TrimSpace(file.Name)
		}
		authIndex := strings.TrimSpace(file.AuthIndex)
		if status != "" && !strings.EqualFold(status, "active") {
			stats.reject("inactive_status")
			continue
		}
		if authID == "" {
			stats.reject("missing_auth_id")
			continue
		}
		if authIndex == "" {
			stats.reject("missing_auth_index")
			continue
		}
		if requireSidecarMarker && !trustedWarmupCredentialNote(file.Note) {
			stats.reject("missing_sidecar_marker")
			continue
		}
		binding := warmupAuthBinding{AuthID: authID, AuthIndex: authIndex}
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

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
)

// warmupEntry records one low-cost activation attempt. It intentionally stores
// no token or response body, only the auth/window bookkeeping needed to avoid
// repeating the request after a CPA restart.
type warmupEntry struct {
	AuthID      string    `json:"auth_id"`
	AuthIndex   string    `json:"auth_index,omitempty"`
	Window      string    `json:"window"`
	AttemptedAt time.Time `json:"attempted_at"`
	ActivatedAt time.Time `json:"activated_at,omitempty"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	Status      int       `json:"status,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type warmupCandidate struct {
	Snapshot quotaSnapshot
	Window   quotaWindow
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
	Status      string `json:"status"`
	Disabled    bool   `json:"disabled"`
	Unavailable bool   `json:"unavailable"`
	Note        string `json:"note"`
}

const (
	warmupMinimumAvailablePercent = 0.000001
	warmupMaxResponseBytes        = 2 << 20
	warmupRequestTimeout          = 45 * time.Second
)

// scheduleWarmup is called after a fresh Keeper snapshot. It deliberately
// schedules at most one request at a time so full accounts are activated
// sequentially instead of creating a burst across the pool.
func (s *schedulerRuntimeState) scheduleWarmup(parent context.Context) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if !cfg.Enabled || !cfg.WarmupEnabled || strings.TrimSpace(cfg.CPAManagementURL) == "" ||
		strings.TrimSpace(cfg.CPAManagementKeyFile) == "" || strings.TrimSpace(cfg.WarmupSidecarURL) == "" {
		return
	}
	now := time.Now()
	if s.pruneExpiredWarmups(now) {
		s.persistBanState()
	}

	eligible, err := s.cpaWarmupEligibleAuths(parent, cfg)
	if err != nil {
		slog.Warn("codex-quota-scheduler: warmup skipped because CPA auth status is unavailable", "error", err)
		return
	}
	candidates := s.findWarmupCandidates(eligible, time.Now())
	if len(candidates) == 0 {
		return
	}

	now = time.Now()
	s.warmupMu.Lock()
	if s.warmupRunning {
		s.warmupMu.Unlock()
		return
	}
	candidate, key, ok := s.nextWarmupCandidateLocked(candidates, now, cfg.WarmupRetryAfter)
	if !ok {
		s.warmupMu.Unlock()
		return
	}
	if s.warmups == nil {
		s.warmups = make(map[string]warmupEntry)
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

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.warmupMu.Lock()
			s.warmupRunning = false
			s.warmupMu.Unlock()
		}()
		s.executeWarmup(parent, cfg, candidate)
	}()
}

func (s *schedulerRuntimeState) findWarmupCandidate(eligible map[string]bool, now time.Time) (warmupCandidate, bool) {
	candidates := s.findWarmupCandidates(eligible, now)
	if len(candidates) == 0 {
		return warmupCandidate{}, false
	}
	return candidates[0], true
}

func (s *schedulerRuntimeState) findWarmupCandidates(eligible map[string]bool, now time.Time) []warmupCandidate {
	s.mu.RLock()
	quotas := make(map[string]quotaSnapshot, len(s.quotas))
	for key, snapshot := range s.quotas {
		quotas[key] = snapshot
	}
	cfg := s.cfg
	s.mu.RUnlock()

	seen := make(map[string]struct{})
	candidates := make([]warmupCandidate, 0)
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" || strings.TrimSpace(snapshot.AuthIndex) == "" {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		if snapshot.RefreshedAt.IsZero() || now.Before(snapshot.RefreshedAt) || now.Sub(snapshot.RefreshedAt) > cfg.StaleAfter {
			continue
		}
		if !eligible[authID] && !eligible[strings.TrimSpace(snapshot.AuthIndex)] {
			continue
		}
		// Quarantined credentials recover only through the serialized half-open
		// scheduler path. Warmup must never bypass that lease with a second probe.
		if _, quarantined := banStore.lookup(authID); quarantined {
			continue
		}
		if window, ok := unstartedWarmupWindow(snapshot, now); ok {
			candidates = append(candidates, warmupCandidate{Snapshot: snapshot, Window: window})
		}
	}
	if len(candidates) == 0 {
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
	return candidates
}

func unstartedWarmupWindow(snapshot quotaSnapshot, now time.Time) (quotaWindow, bool) {
	// A warmup is intentionally limited to the short primary and weekly
	// windows. Starting a monthly window for an account that may remain idle is
	// not cost-optimal.
	for _, class := range []string{"5h", "weekly"} {
		for _, window := range snapshot.Windows {
			if window.Class != class || window.UsedPercent > warmupMinimumAvailablePercent ||
				window.LimitReached || !window.Allowed {
				continue
			}
			if !window.ResetAt.IsZero() && now.Before(window.ResetAt) {
				continue
			}
			if window.ResetAt.IsZero() {
				return window, true
			}
		}
	}
	return quotaWindow{}, false
}

func warmupWindowRank(class string) int {
	switch class {
	case "5h":
		return 0
	case "weekly":
		return 1
	default:
		return 10
	}
}

func warmupKey(authID, window string) string {
	return strings.TrimSpace(authID) + "|" + strings.TrimSpace(window)
}

// nextWarmupCandidateLocked skips accounts already activated or recently
// attempted, allowing later full accounts to make progress on the next refresh.
// The caller must hold warmupMu.
func (s *schedulerRuntimeState) nextWarmupCandidateLocked(candidates []warmupCandidate, now time.Time, retryAfter time.Duration) (warmupCandidate, string, bool) {
	for _, candidate := range candidates {
		key := warmupKey(candidate.Snapshot.AuthID, candidate.Window.Class)
		if s.warmupSuppressedLocked(key, now, retryAfter) {
			continue
		}
		return candidate, key, true
	}
	return warmupCandidate{}, "", false
}

func (s *schedulerRuntimeState) pruneExpiredWarmups(now time.Time) bool {
	s.warmupMu.Lock()
	defer s.warmupMu.Unlock()
	changed := false
	for key, entry := range s.warmups {
		if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) {
			delete(s.warmups, key)
			changed = true
		}
	}
	return changed
}

func (s *schedulerRuntimeState) warmupSuppressedLocked(key string, now time.Time, retryAfter time.Duration) bool {
	entry, ok := s.warmups[key]
	if !ok {
		return false
	}
	if !entry.ResetAt.IsZero() && !now.Before(entry.ResetAt) {
		delete(s.warmups, key)
		return false
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Minute
	}
	return !entry.AttemptedAt.IsZero() && now.Sub(entry.AttemptedAt) < retryAfter
}

func (s *schedulerRuntimeState) executeWarmup(parent context.Context, cfg pluginConfig, candidate warmupCandidate) {
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

	payload, err := json.Marshal(map[string]any{
		"model":  cfg.WarmupModel,
		"input":  []map[string]any{{"role": "user", "content": "hello"}},
		"stream": true,
		"store":  false,
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
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    "codex-cli",
		},
		Data: string(payload),
	})
	if err != nil {
		s.recordWarmupError(candidate, 0, fmt.Errorf("encode CPA api-call: %w", err))
		return
	}

	ctx, cancel := context.WithTimeout(parent, warmupRequestTimeout)
	defer cancel()
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
		s.recordWarmupError(candidate, resp.StatusCode, fmt.Errorf("CPA api-call returned HTTP %d", resp.StatusCode))
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
		now := time.Now()
		entry, authoritative := quarantineEntryFor429(headers, now, cfg.FallbackBan, cfg.MaxBan)
		banStore.record429(candidate.Snapshot.AuthID, entry, requestedAt)
		s.markSerialUnavailable(candidate.Snapshot.AuthID, "warmup_429", now)
		s.persistAfterBanChange()
		slog.Warn("codex-quota-scheduler: warmup received 429; credential quarantined",
			"auth_id", candidate.Snapshot.AuthID,
			"kind", entry.Kind,
			"authoritative", authoritative,
			"window", entry.Window,
			"probe_ready_at", entry.ResetAt.Format(time.RFC3339))
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		s.recordWarmupError(candidate, result.StatusCode, fmt.Errorf("warmup upstream returned HTTP %d", result.StatusCode))
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

func (s *schedulerRuntimeState) recordWarmupError(candidate warmupCandidate, status int, err error) {
	s.recordWarmupOutcome(candidate, status, nil, err)
	slog.Warn("codex-quota-scheduler: Codex warmup failed", "auth_id", candidate.Snapshot.AuthID, "window", candidate.Window.Class, "error", err)
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
		target.Error = err.Error()
	} else {
		target.Error = ""
	}
	if target.AttemptedAt.IsZero() {
		target.AttemptedAt = now
	}
	s.warmups[targetKey] = target
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
		entry.ActivatedAt = now
		entry.ResetAt = window.ResetAt
		entry.Status = status
		entry.Error = ""
		s.warmups[key] = entry
	}
	if current, ok := s.warmups[targetKey]; ok {
		target = current
	}
	// A successful minimal warmup request is itself proof that the account was
	// accepted. If an intermediary omits x-codex reset headers, retain a
	// conservative local activation window so the plugin never retries every
	// few minutes and wastes quota on the same idle account.
	if err == nil && status >= 200 && status < 300 && target.ActivatedAt.IsZero() {
		target.ActivatedAt = now
		target.ResetAt = now.Add(warmupFallbackWindow(candidate.Window.Class))
		s.warmups[targetKey] = target
	}
	s.warmupMu.Unlock()
	s.persistBanState()
}

func warmupFallbackWindow(class string) time.Duration {
	if class == "weekly" {
		return 7 * 24 * time.Hour
	}
	return 5 * time.Hour
}

func (s *schedulerRuntimeState) cpaWarmupEligibleAuths(ctx context.Context, cfg pluginConfig) (map[string]bool, error) {
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
	return warmupEligibleAuths(result.Files), nil
}

func warmupEligibleAuths(files []cpaAuthFileEntry) map[string]bool {
	eligible := make(map[string]bool)
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Provider), providerCodex) {
			continue
		}
		inactive := file.Disabled || file.Unavailable
		if status := strings.TrimSpace(file.Status); status != "" && !strings.EqualFold(status, "active") {
			inactive = true
		}
		// The pinned management request targets the Agent Identity sidecar.
		// Requiring the sidecar marker prevents a future native OAuth credential
		// from being sent to the wrong authentication endpoint.
		if inactive || !strings.Contains(strings.ToLower(file.Note), "via sidecar") {
			continue
		}
		for _, key := range []string{file.ID, file.AuthIndex, file.Name} {
			if key = strings.TrimSpace(key); key != "" {
				eligible[key] = true
			}
		}
	}
	return eligible
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

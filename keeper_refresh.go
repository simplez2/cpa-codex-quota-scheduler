package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	keeperRefreshGateVersion  = 1
	keeperRefreshGateMaxBytes = 64 << 10
	keeperRefreshBackoffMax   = 30 * time.Minute
)

type keeperRefreshTarget struct {
	AuthIndex  string
	Reason     string
	ObservedAt time.Time
}

type keeperRefreshGateRecord struct {
	Version       int       `json:"version"`
	Fingerprint   string    `json:"fingerprint"`
	RequestedAt   time.Time `json:"requested_at"`
	NextAllowedAt time.Time `json:"next_allowed_at"`
	Attempt       int       `json:"attempt"`
	TargetCount   int       `json:"target_count"`
}

type keeperRefreshResponse struct {
	Tasks []struct {
		AuthIndex string `json:"authIndex"`
	} `json:"tasks"`
	Rejected []struct {
		AuthIndex string `json:"authIndex"`
		Error     string `json:"error"`
	} `json:"rejected"`
	Accepted int `json:"accepted"`
	Skipped  int `json:"skipped"`
	Limit    int `json:"limit"`
}

// reserveKeeperRefreshGate provides a fixed-size, cross-DSO throttle for
// Keeper's asynchronous /quota/refresh endpoint. The lock file is stable while
// the JSON record is atomically replaced, so a CPA hot reload cannot enqueue
// the same stale account set from both the retiring and incoming plugin.
func reserveKeeperRefreshGate(statePath, fingerprint string, targetCount int, now time.Time, baseCooldown time.Duration) (keeperRefreshGateRecord, bool, bool, error) {
	statePath = strings.TrimSpace(statePath)
	fingerprint = strings.TrimSpace(fingerprint)
	if statePath == "" {
		return keeperRefreshGateRecord{}, false, false, errors.New("keeper refresh gate requires state_path")
	}
	if fingerprint == "" || targetCount <= 0 {
		return keeperRefreshGateRecord{}, false, false, errors.New("keeper refresh gate requires targets")
	}
	if baseCooldown <= 0 {
		baseCooldown = 2 * time.Minute
	}

	path := keeperRefreshGatePath(statePath)
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return keeperRefreshGateRecord{}, false, false, fmt.Errorf("create keeper refresh gate directory: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return keeperRefreshGateRecord{}, false, false, fmt.Errorf("open keeper refresh gate lock: %w", err)
	}
	locked := false
	deadline := time.Now().Add(generationLockWait)
	for {
		locked, err = tryExclusiveFileLock(lockFile)
		if err != nil || locked || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(generationLockRetry)
	}
	if err != nil {
		_ = lockFile.Close()
		return keeperRefreshGateRecord{}, false, false, fmt.Errorf("lock keeper refresh gate: %w", err)
	}
	if !locked {
		_ = lockFile.Close()
		return keeperRefreshGateRecord{}, false, false, errGenerationLockBusy
	}
	defer func() {
		_ = unlockExclusiveFile(lockFile)
		_ = lockFile.Close()
	}()

	record, recovered, err := readKeeperRefreshGateRecord(path)
	if err != nil {
		return keeperRefreshGateRecord{}, false, false, err
	}
	now = now.UTC()
	if record.Fingerprint == fingerprint && now.Before(record.NextAllowedAt) {
		return record, false, recovered, nil
	}
	attempt := 1
	if record.Fingerprint == fingerprint && record.Attempt > 0 {
		attempt = record.Attempt + 1
	}
	record = keeperRefreshGateRecord{
		Version:       keeperRefreshGateVersion,
		Fingerprint:   fingerprint,
		RequestedAt:   now,
		NextAllowedAt: now.Add(keeperRefreshBackoff(baseCooldown, attempt)),
		Attempt:       attempt,
		TargetCount:   targetCount,
	}
	if err := writeKeeperRefreshGateRecord(path, record); err != nil {
		return keeperRefreshGateRecord{}, false, recovered, err
	}
	return record, true, recovered, nil
}

func keeperRefreshGatePath(statePath string) string {
	return strings.TrimSpace(statePath) + ".keeper-refresh"
}

func readKeeperRefreshGateRecord(path string) (keeperRefreshGateRecord, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return keeperRefreshGateRecord{}, false, nil
		}
		return keeperRefreshGateRecord{}, false, fmt.Errorf("read keeper refresh gate: %w", err)
	}
	if len(raw) == 0 {
		return keeperRefreshGateRecord{}, false, nil
	}
	if len(raw) > keeperRefreshGateMaxBytes {
		return keeperRefreshGateRecord{}, false, fmt.Errorf("keeper refresh gate exceeds %d bytes", keeperRefreshGateMaxBytes)
	}
	var record keeperRefreshGateRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		if quarantineErr := quarantineKeeperRefreshGate(path); quarantineErr != nil {
			return keeperRefreshGateRecord{}, false, fmt.Errorf("decode keeper refresh gate: %w", err)
		}
		return keeperRefreshGateRecord{}, true, nil
	}
	if record.Version != keeperRefreshGateVersion {
		return keeperRefreshGateRecord{}, false, fmt.Errorf("unsupported keeper refresh gate version %d", record.Version)
	}
	if record.Fingerprint == "" || record.Attempt <= 0 || record.TargetCount <= 0 || record.RequestedAt.IsZero() || record.NextAllowedAt.Before(record.RequestedAt) {
		if quarantineErr := quarantineKeeperRefreshGate(path); quarantineErr != nil {
			return keeperRefreshGateRecord{}, false, errors.New("invalid keeper refresh gate record")
		}
		return keeperRefreshGateRecord{}, true, nil
	}
	return record, false, nil
}

func quarantineKeeperRefreshGate(path string) error {
	quarantinePath := fmt.Sprintf("%s.corrupt-%d", path, time.Now().UTC().UnixNano())
	if err := os.Rename(path, quarantinePath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeKeeperRefreshGateRecord(path string, record keeperRefreshGateRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > keeperRefreshGateMaxBytes {
		return fmt.Errorf("keeper refresh gate exceeds %d bytes", keeperRefreshGateMaxBytes)
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".keeper-refresh-*.tmp")
	if err != nil {
		return fmt.Errorf("create keeper refresh gate temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0600)
	if err := writeFileAll(tmp, raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace keeper refresh gate: %w", err)
	}
	return nil
}

func keeperRefreshBackoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = 2 * time.Minute
	}
	limit := keeperRefreshBackoffMax
	if base > limit {
		limit = base
	}
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for current := 1; current < attempt && delay < limit; current++ {
		if delay > limit/2 {
			return limit
		}
		delay *= 2
	}
	if delay > limit {
		return limit
	}
	return delay
}

func keeperRefreshFingerprint(targets []keeperRefreshTarget) string {
	copyTargets := append([]keeperRefreshTarget(nil), targets...)
	sort.Slice(copyTargets, func(i, j int) bool {
		if copyTargets[i].AuthIndex != copyTargets[j].AuthIndex {
			return copyTargets[i].AuthIndex < copyTargets[j].AuthIndex
		}
		if copyTargets[i].Reason != copyTargets[j].Reason {
			return copyTargets[i].Reason < copyTargets[j].Reason
		}
		return copyTargets[i].ObservedAt.Before(copyTargets[j].ObservedAt)
	})
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for _, target := range copyTargets {
		_ = encoder.Encode(struct {
			AuthIndex  string `json:"auth_index"`
			Reason     string `json:"reason"`
			ObservedAt int64  `json:"observed_at"`
		}{
			AuthIndex:  strings.TrimSpace(target.AuthIndex),
			Reason:     strings.TrimSpace(target.Reason),
			ObservedAt: target.ObservedAt.UTC().Unix(),
		})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func keeperRefreshTargetIndexes(targets []keeperRefreshTarget) []string {
	seen := make(map[string]struct{}, len(targets))
	indexes := make([]string, 0, len(targets))
	for _, target := range targets {
		index := strings.TrimSpace(target.AuthIndex)
		if index == "" {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	sort.Strings(indexes)
	return indexes
}

func collectKeeperRefreshTargets(indexes []string, cache keeperCacheResponse, quotas map[string]quotaSnapshot, now time.Time, staleAfter time.Duration) []keeperRefreshTarget {
	items := make(map[string]keeperCacheItem, len(cache.Items))
	for _, item := range cache.Items {
		index := strings.TrimSpace(item.AuthIndex)
		if index == "" {
			continue
		}
		previous, exists := items[index]
		if !exists || keeperCacheItemNewer(item, previous) {
			items[index] = item
		}
	}

	targets := make([]keeperRefreshTarget, 0)
	seen := make(map[string]struct{}, len(indexes))
	for _, rawIndex := range indexes {
		index := strings.TrimSpace(rawIndex)
		if index == "" {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}

		if snapshot, ok := quotas[index]; ok {
			if quotaSnapshotFresh(snapshot, now, staleAfter) {
				continue
			}
			reason := "stale"
			switch {
			case snapshot.RefreshedAt.IsZero():
				reason = "missing_refreshed_at"
			case now.Before(snapshot.RefreshedAt):
				reason = "future_refreshed_at"
			}
			targets = append(targets, keeperRefreshTarget{AuthIndex: index, Reason: reason, ObservedAt: snapshot.RefreshedAt})
			continue
		}

		item, ok := items[index]
		if !ok {
			targets = append(targets, keeperRefreshTarget{AuthIndex: index, Reason: "missing"})
			continue
		}
		observedAt := parseKeeperTime(item.RefreshedAt)
		if strings.EqualFold(strings.TrimSpace(item.Status), "failed") {
			expiresAt := parseKeeperTime(item.ExpiresAt)
			if !expiresAt.IsZero() && now.Before(expiresAt) {
				// Keeper intentionally preserves selected provider failures (for
				// example 401/402) until ExpiresAt. Respect that native backoff.
				continue
			}
			targets = append(targets, keeperRefreshTarget{AuthIndex: index, Reason: "failed_expired", ObservedAt: observedAt})
			continue
		}
		reason := "incomplete"
		if strings.EqualFold(strings.TrimSpace(item.Status), "completed") {
			reason = "no_usable_windows"
		}
		targets = append(targets, keeperRefreshTarget{AuthIndex: index, Reason: reason, ObservedAt: observedAt})
	}
	return targets
}

// collectExpiredQuotaBanRefreshTargets asks Keeper for a fresh observation as
// soon as a quota cooldown has elapsed. A normal cache refresh can briefly
// return a completed row without usable windows while the asynchronous
// refresh task is still settling; without this targeted retry the old ban can
// suppress warmup until a later unrelated refresh happens.
func collectExpiredQuotaBanRefreshTargets(indexToFile map[string]string, quotas map[string]quotaSnapshot, now time.Time) []keeperRefreshTarget {
	if len(indexToFile) == 0 {
		return nil
	}
	byAuth := make(map[string]quotaSnapshot, len(quotas))
	for _, snapshot := range quotas {
		authID := strings.TrimSpace(snapshot.AuthID)
		if authID == "" {
			continue
		}
		if previous, ok := byAuth[authID]; !ok || snapshot.RefreshedAt.After(previous.RefreshedAt) {
			byAuth[authID] = snapshot
		}
	}
	targets := make([]keeperRefreshTarget, 0)
	for authIndex, authID := range indexToFile {
		authID = strings.TrimSpace(authID)
		entry, ok := banStore.lookup(authID)
		if !ok || entry.Kind != banKindQuota || entry.Phase != banPhaseCooldown || entry.ResetAt.IsZero() || now.Before(entry.ResetAt) {
			continue
		}
		observedAt := time.Time{}
		if snapshot, exists := byAuth[authID]; exists {
			observedAt = snapshot.RefreshedAt
		}
		targets = append(targets, keeperRefreshTarget{
			AuthIndex:  strings.TrimSpace(authIndex),
			Reason:     "expired_quota_cooldown",
			ObservedAt: observedAt,
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].AuthIndex < targets[j].AuthIndex })
	return targets
}

func keeperCacheItemNewer(candidate, previous keeperCacheItem) bool {
	candidateCompleted := strings.EqualFold(strings.TrimSpace(candidate.Status), "completed") && candidate.Quota != nil
	previousCompleted := strings.EqualFold(strings.TrimSpace(previous.Status), "completed") && previous.Quota != nil
	if candidateCompleted != previousCompleted {
		return candidateCompleted
	}
	return parseKeeperTime(candidate.RefreshedAt).After(parseKeeperTime(previous.RefreshedAt))
}

func (s *schedulerRuntimeState) reserveKeeperQuotaRefresh(cfg pluginConfig, targets []keeperRefreshTarget, now time.Time) (keeperRefreshGateRecord, bool, bool, error) {
	fingerprint := keeperRefreshFingerprint(targets)
	if strings.TrimSpace(cfg.StatePath) != "" {
		return reserveKeeperRefreshGate(cfg.StatePath, fingerprint, len(targets), now, cfg.KeeperRefreshCooldown)
	}

	// state_path="" is retained for legacy deployments and isolated unit tests.
	// Its throttle is process-local because there is no durable namespace to
	// safely share with another loaded DSO.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keeperRefreshFingerprint == fingerprint && now.Before(s.keeperRefreshNextAllowedAt) {
		return keeperRefreshGateRecord{
			Version:       keeperRefreshGateVersion,
			Fingerprint:   fingerprint,
			RequestedAt:   s.keeperRefreshRequestedAt,
			NextAllowedAt: s.keeperRefreshNextAllowedAt,
			Attempt:       s.keeperRefreshAttempt,
			TargetCount:   len(targets),
		}, false, false, nil
	}
	attempt := 1
	if s.keeperRefreshFingerprint == fingerprint && s.keeperRefreshAttempt > 0 {
		attempt = s.keeperRefreshAttempt + 1
	}
	record := keeperRefreshGateRecord{
		Version:       keeperRefreshGateVersion,
		Fingerprint:   fingerprint,
		RequestedAt:   now.UTC(),
		NextAllowedAt: now.UTC().Add(keeperRefreshBackoff(cfg.KeeperRefreshCooldown, attempt)),
		Attempt:       attempt,
		TargetCount:   len(targets),
	}
	s.keeperRefreshFingerprint = fingerprint
	s.keeperRefreshRequestedAt = record.RequestedAt
	s.keeperRefreshNextAllowedAt = record.NextAllowedAt
	s.keeperRefreshAttempt = record.Attempt
	return record, true, false, nil
}

func (s *schedulerRuntimeState) recordKeeperRefreshGate(targetCount int, record keeperRefreshGateRecord, reserved, recovered bool, errCode string) {
	s.mu.Lock()
	s.keeperRefreshTargetsLast = targetCount
	if !record.RequestedAt.IsZero() {
		s.keeperRefreshRequestedAt = record.RequestedAt
		s.keeperRefreshNextAllowedAt = record.NextAllowedAt
		s.keeperRefreshAttempt = record.Attempt
		s.keeperRefreshFingerprint = record.Fingerprint
	}
	if reserved {
		s.keeperRefreshRequests++
		s.keeperRefreshAcceptedLast = 0
		s.keeperRefreshSkippedLast = 0
		s.keeperRefreshRejectedLast = make(map[string]int)
	}
	if recovered {
		s.keeperRefreshRecoveries++
	}
	if errCode != "" {
		s.keeperRefreshLastError = errCode
	}
	s.mu.Unlock()
}

func (s *schedulerRuntimeState) recordKeeperRefreshResult(response keeperRefreshResponse, errCode string) {
	rejected := make(map[string]int)
	for _, item := range response.Rejected {
		code := sanitizeKeeperRefreshCode(item.Error)
		if code == "" {
			code = "unknown"
		}
		rejected[code]++
	}
	s.mu.Lock()
	s.keeperRefreshAcceptedLast = response.Accepted
	if s.keeperRefreshAcceptedLast == 0 && len(response.Tasks) > 0 {
		s.keeperRefreshAcceptedLast = len(response.Tasks)
	}
	s.keeperRefreshSkippedLast = response.Skipped
	s.keeperRefreshRejectedLast = rejected
	s.keeperRefreshLastError = errCode
	s.mu.Unlock()
}

func (s *schedulerRuntimeState) clearKeeperRefreshTargets() {
	s.mu.Lock()
	s.keeperRefreshTargetsLast = 0
	s.keeperRefreshAcceptedLast = 0
	s.keeperRefreshSkippedLast = 0
	s.keeperRefreshRejectedLast = make(map[string]int)
	s.keeperRefreshLastError = ""
	s.mu.Unlock()
}

func (s *schedulerRuntimeState) recordKeeperRefreshInventoryFailure() {
	s.mu.Lock()
	s.keeperRefreshTargetsLast = 0
	s.keeperRefreshAcceptedLast = 0
	s.keeperRefreshSkippedLast = 0
	s.keeperRefreshRejectedLast = make(map[string]int)
	s.keeperRefreshLastError = "auth_inventory_unavailable"
	s.mu.Unlock()
}

func filterKeeperRefreshTargets(targets []keeperRefreshTarget, activeIndexes map[string]struct{}) []keeperRefreshTarget {
	filtered := make([]keeperRefreshTarget, 0, len(targets))
	for _, target := range targets {
		if _, ok := activeIndexes[strings.TrimSpace(target.AuthIndex)]; ok {
			filtered = append(filtered, target)
		}
	}
	return filtered
}

// activeCPARefreshTargets closes the short synchronization window between CPA
// disabling a credential and Keeper observing that state. If the authenticated
// host inventory is unavailable, quota cache reads remain usable but the
// side-effecting Keeper refresh is skipped.
func (s *schedulerRuntimeState) activeCPARefreshTargets(ctx context.Context, cfg pluginConfig, targets []keeperRefreshTarget) ([]keeperRefreshTarget, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	activeIndexes, err := cpaActiveCodexAuthIndexes(ctx, cfg)
	if err != nil {
		s.recordKeeperRefreshInventoryFailure()
		return nil, fmt.Errorf("load active CPA Codex auth inventory: %w", err)
	}
	filtered := filterKeeperRefreshTargets(targets, activeIndexes)
	if len(filtered) == 0 {
		s.clearKeeperRefreshTargets()
	}
	return filtered, nil
}

func (s *schedulerRuntimeState) requestActiveCPAKeeperQuotaRefreshTargets(ctx context.Context, cfg pluginConfig, password, token string, targets []keeperRefreshTarget, now time.Time) error {
	targets, err := s.activeCPARefreshTargets(ctx, cfg, targets)
	if err != nil || len(targets) == 0 {
		return err
	}
	return s.requestKeeperQuotaRefreshTargets(ctx, cfg, password, token, targets, now)
}

func collectCarriedStaleWindowRefreshTargets(indexes []string, current, previous map[string]quotaSnapshot, now time.Time, staleAfter time.Duration) []keeperRefreshTarget {
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	targets := make([]keeperRefreshTarget, 0)
	for _, rawIndex := range indexes {
		index := strings.TrimSpace(rawIndex)
		if index == "" {
			continue
		}
		currentSnapshot, ok := current[index]
		if !ok {
			// A completely missing cache item is already handled by the normal
			// cache-recovery collector.
			continue
		}
		present := make(map[string]struct{}, len(currentSnapshot.Windows))
		for _, window := range currentSnapshot.Windows {
			if class := normalizeWindowClass(window.Class); class != "" {
				present[class] = struct{}{}
			}
		}
		previousSnapshot, ok := previous[index]
		if !ok {
			continue
		}
		// Match mergePartialQuotaSnapshot: an old snapshot outside the outer
		// freshness envelope is discarded wholesale and therefore cannot leave
		// a carried sibling that needs recovery.
		if previousSnapshot.RefreshedAt.IsZero() || now.Before(previousSnapshot.RefreshedAt) || now.Sub(previousSnapshot.RefreshedAt) > staleAfter {
			continue
		}
		staleCarry := false
		oldestObservedAt := time.Time{}
		for _, window := range previousSnapshot.Windows {
			class := normalizeWindowClass(window.Class)
			if class == "" {
				continue
			}
			if _, exists := present[class]; exists {
				continue
			}
			// Keep this exactly aligned with mergePartialQuotaSnapshot: a missing
			// window is carried while its old reset is unknown or still in the
			// future; an explicitly expired reset is dropped.
			if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
				continue
			}
			observedAt := window.ObservedAt
			if observedAt.IsZero() {
				observedAt = previousSnapshot.RefreshedAt
			}
			if !observedAt.IsZero() && !now.Before(observedAt) && now.Sub(observedAt) <= staleAfter {
				continue
			}
			staleCarry = true
			if oldestObservedAt.IsZero() || (!observedAt.IsZero() && observedAt.Before(oldestObservedAt)) {
				oldestObservedAt = observedAt
			}
		}
		if staleCarry {
			targets = append(targets, keeperRefreshTarget{
				AuthIndex:  index,
				Reason:     "carried_stale_window",
				ObservedAt: oldestObservedAt,
			})
		}
	}
	return targets
}

func mergeKeeperRefreshTargets(groups ...[]keeperRefreshTarget) []keeperRefreshTarget {
	byIndex := make(map[string]keeperRefreshTarget)
	for _, group := range groups {
		for _, target := range group {
			index := strings.TrimSpace(target.AuthIndex)
			if index == "" {
				continue
			}
			if _, exists := byIndex[index]; exists {
				continue
			}
			target.AuthIndex = index
			byIndex[index] = target
		}
	}
	indexes := make([]string, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	sort.Strings(indexes)
	out := make([]keeperRefreshTarget, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, byIndex[index])
	}
	return out
}

func (s *schedulerRuntimeState) maybeRequestKeeperQuotaRefresh(ctx context.Context, cfg pluginConfig, password, token string, indexes []string, cache keeperCacheResponse, quotas map[string]quotaSnapshot, now time.Time) error {
	// Keeper cache recovery is scheduler safety work, not model warmup. It must
	// remain available while warmup is drained so a newly loaded scheduler can
	// obtain a fresh quota snapshot without issuing any model request.
	if !cfg.Enabled {
		s.clearKeeperRefreshTargets()
		return nil
	}
	targets := collectKeeperRefreshTargets(indexes, cache, quotas, now, cfg.StaleAfter)
	s.mu.RLock()
	previous := make(map[string]quotaSnapshot, len(s.quotas))
	for key, snapshot := range s.quotas {
		previous[key] = snapshot
	}
	s.mu.RUnlock()
	targets = mergeKeeperRefreshTargets(
		targets,
		collectCarriedStaleWindowRefreshTargets(indexes, quotas, previous, now, cfg.StaleAfter),
	)
	if len(targets) == 0 {
		s.clearKeeperRefreshTargets()
		return nil
	}
	return s.requestActiveCPAKeeperQuotaRefreshTargets(ctx, cfg, password, token, targets, now)
}

// requestKeeperQuotaRefreshTargets is shared by scheduler cache recovery and
// the one-shot second-snapshot request used to confirm a newer quota cycle.
// Both paths use the same cross-DSO gate and Keeper-native duplicate handling.
func (s *schedulerRuntimeState) requestKeeperQuotaRefreshTargets(ctx context.Context, cfg pluginConfig, password, token string, targets []keeperRefreshTarget, now time.Time) error {
	// Cache recovery and a second observation for an already-pending ban reset
	// are quota-only safety work. They remain available while warmup execution is
	// drained so scheduling can recover without any model call.
	if !cfg.Enabled || len(targets) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	record, reserved, recovered, err := s.reserveKeeperQuotaRefresh(cfg, targets, now)
	if err != nil {
		s.recordKeeperRefreshGate(len(targets), keeperRefreshGateRecord{}, false, false, "gate_unavailable")
		return fmt.Errorf("coordinate Keeper quota refresh: %w", err)
	}
	s.recordKeeperRefreshGate(len(targets), record, reserved, recovered, "")
	if recovered {
		slog.Warn("codex-quota-scheduler: recovered an invalid Keeper refresh throttle record")
	}
	if !reserved {
		return nil
	}

	authIndexes := keeperRefreshTargetIndexes(targets)
	body, err := json.Marshal(map[string]any{"auth_indexes": authIndexes})
	if err != nil {
		s.recordKeeperRefreshResult(keeperRefreshResponse{}, "encode_failed")
		return fmt.Errorf("encode Keeper quota refresh: %w", err)
	}
	responseBody, status, err := keeperJSON(ctx, cfg.KeeperURL, token, http.MethodPost, "/quota/refresh", body)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		token, loginErr := s.keeperSession(ctx, cfg, password, true)
		if loginErr != nil {
			err = loginErr
		} else {
			responseBody, status, err = keeperJSON(ctx, cfg.KeeperURL, token, http.MethodPost, "/quota/refresh", body)
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		s.recordKeeperRefreshResult(keeperRefreshResponse{}, "request_failed")
		return err
	}
	if status < 200 || status >= 300 {
		code := fmt.Sprintf("http_%d", status)
		s.recordKeeperRefreshResult(keeperRefreshResponse{}, code)
		return fmt.Errorf("Keeper quota refresh returned HTTP %d", status)
	}
	var response keeperRefreshResponse
	if err := json.Unmarshal(responseBody, &response); err != nil {
		s.recordKeeperRefreshResult(keeperRefreshResponse{}, "decode_failed")
		return fmt.Errorf("decode Keeper quota refresh response: %w", err)
	}
	duplicate := 0
	for _, rejected := range response.Rejected {
		if strings.EqualFold(strings.TrimSpace(rejected.Error), "duplicate") {
			duplicate++
		}
	}
	accepted := response.Accepted
	if accepted == 0 && len(response.Tasks) > 0 {
		accepted = len(response.Tasks)
	}
	if accepted == 0 && duplicate == 0 {
		s.recordKeeperRefreshResult(response, "all_rejected")
		return errors.New("Keeper quota refresh rejected all targets")
	}
	s.recordKeeperRefreshResult(response, "")
	slog.Info("codex-quota-scheduler: queued native Keeper quota refresh",
		"targets", len(authIndexes),
		"accepted", accepted,
		"already_running", duplicate,
		"attempt", record.Attempt)
	return nil
}

func sanitizeKeeperRefreshCode(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range raw {
		if out.Len() >= 64 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_-")
}

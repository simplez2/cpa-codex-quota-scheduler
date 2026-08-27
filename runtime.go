package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// quotaSource records where the latest values for a normalized window came
// from. Keeper is authoritative; response headers are a low-latency overlay.
type quotaSource string

const (
	quotaSourceUnknown quotaSource = ""
	quotaSourceKeeper  quotaSource = "keeper"
	quotaSourceHeader  quotaSource = "header"
	quotaSourceMixed   quotaSource = "mixed"
)

// quotaWindow is a normalized view of one Keeper quota row.  Keeping the
// normalized form in the plugin means the scheduler does not depend on one
// particular Keeper/provider JSON spelling.
type quotaWindow struct {
	Class                   string
	WindowSeconds           int64
	ResetAfterSeconds       int64
	ResetAfterSecondsKnown  bool
	UsedPercent             float64
	UsedPercentKnown        bool
	Allowed                 bool
	AllowedKnown            bool
	LimitReached            bool
	LimitReachedKnown       bool
	ResetAt                 time.Time
	Source                  quotaSource
	ObservedAt              time.Time
	WindowUsageCredits      float64
	WindowUsageCreditsKnown bool
}

type quotaSnapshot struct {
	AuthID           string
	AuthIndex        string
	Windows          []quotaWindow
	ResetCredits     int
	RefreshedAt      time.Time
	HeaderObservedAt time.Time
}

type schedulerRuntimeState struct {
	lifecycleMu  sync.Mutex
	mu           sync.RWMutex
	persistMu    sync.Mutex
	generationMu sync.Mutex
	generation   schedulerGenerationOwnership

	cfg         pluginConfig
	quotas      map[string]quotaSnapshot // indexed by AuthID and AuthIndex aliases
	identities  map[string]string        // Keeper auth_index -> CPA auth file/AuthID
	lastRefresh time.Time
	lastError   string
	refreshes   int

	cancel                      context.CancelFunc
	wg                          sync.WaitGroup
	stopping                    bool
	warmupMu                    sync.Mutex
	warmupRunning               bool
	warmups                     map[string]warmupEntry
	warmupLeases                map[string]warmupLease
	warmupCandidatesLast        int
	warmupSkippedBannedLast     int
	warmupSkippedStaleLast      int
	warmupSkippedIneligibleLast int
	warmupSkippedNotNeededLast  int
	warmupAuthSourceLast        string
	warmupAuthCheckedAt         time.Time
	warmupAuthFilesSeenLast     int
	warmupAuthEligibleLast      int
	warmupAuthRejectedLast      map[string]int
	warmupAuthLastError         string
	keeperRefreshTargetsLast    int
	keeperRefreshRequests       uint64
	keeperRefreshRequestedAt    time.Time
	keeperRefreshNextAllowedAt  time.Time
	keeperRefreshAttempt        int
	keeperRefreshAcceptedLast   int
	keeperRefreshSkippedLast    int
	keeperRefreshRejectedLast   map[string]int
	keeperRefreshLastError      string
	keeperRefreshFingerprint    string
	keeperRefreshRecoveries     uint64

	banResetMu                 sync.Mutex
	banResetConfirmations      map[string]banResetConfirmation
	banResetConfirmationEvents uint64
	banExternalResetClears     uint64
	lastBanClearReason         string
	lastBanClearAt             time.Time

	sessionToken  string
	sessionExpiry time.Time
	pickCounter   uint64

	pricing             map[string]modelPricing
	costSamples         map[string][]float64
	globalCostSamples   []float64
	pacingAccounts      map[string]*accountPacingState
	stickyBindings      map[string]stickyBinding
	decisionHistory     []schedulerDecisionAudit
	shadowDisagreements uint64
	sessionSwitches     uint64
	lastShadowLog       time.Time
	configGeneration    uint64

	serialActiveAuthID     string
	serialSelectionSource  string
	serialSelectedAt       time.Time
	serialSwitches         uint64
	serialFallbacks        uint64
	serialLastSwitchAt     time.Time
	serialLastSwitchReason string
	serialMissingAuthID    string
	serialFallbackAuthID   string
	serialMissingSince     time.Time
	serialMissingCount     int
	serialOverdraft        map[string]serialOverdraftBinding
	// serialLastSelected records the last committed selection for each auth.
	// It is only a stable round-robin tie breaker; hard quota and quarantine
	// decisions always take precedence.
	serialLastSelected  map[string]time.Time
	serialFiveHourCycle map[string]time.Time
}

// serialOverdraftBinding pins an in-flight session to the auth it started on
// after that auth crossed the serial threshold. The official courtesy lets a
// running conversation continue to completion without extra charge once the
// usage limit is hit, so those requests must keep using the exhausted account
// instead of being silently moved to the fresh one.
type serialOverdraftBinding struct {
	AuthID     string    `json:"auth_id"`
	LastUsedAt time.Time `json:"last_used_at"`
}

var schedulerRuntime schedulerRuntimeState

func configureSchedulerRuntime(raw []byte) {
	schedulerRuntime.lifecycleMu.Lock()
	defer schedulerRuntime.lifecycleMu.Unlock()

	cfg, err := parsePluginConfig(raw)
	if err != nil {
		// A malformed optional plugin config must never prevent CPA from
		// starting or make a request return 502.  Keep the plugin loaded in a
		// safe native-fallback mode and make the error visible in logs/status.
		slog.Warn("codex-quota-scheduler: invalid plugin config; using safe defaults", "error", err)
		cfg = defaultPluginConfig()
		cfg.Enabled = false
	}

	schedulerRuntime.stopLocked()
	schedulerRuntime.initializeGenerationOwnership(cfg.StatePath)
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cfg = cfg
	schedulerRuntime.quotas = make(map[string]quotaSnapshot)
	schedulerRuntime.identities = make(map[string]string)
	schedulerRuntime.lastRefresh = time.Time{}
	schedulerRuntime.lastError = ""
	schedulerRuntime.refreshes = 0
	schedulerRuntime.stopping = false
	schedulerRuntime.warmups = make(map[string]warmupEntry)
	schedulerRuntime.warmupLeases = make(map[string]warmupLease)
	schedulerRuntime.warmupCandidatesLast = 0
	schedulerRuntime.warmupSkippedBannedLast = 0
	schedulerRuntime.warmupSkippedStaleLast = 0
	schedulerRuntime.warmupSkippedIneligibleLast = 0
	schedulerRuntime.warmupSkippedNotNeededLast = 0
	schedulerRuntime.warmupAuthSourceLast = ""
	schedulerRuntime.warmupAuthCheckedAt = time.Time{}
	schedulerRuntime.warmupAuthFilesSeenLast = 0
	schedulerRuntime.warmupAuthEligibleLast = 0
	schedulerRuntime.warmupAuthRejectedLast = make(map[string]int)
	schedulerRuntime.warmupAuthLastError = ""
	schedulerRuntime.keeperRefreshTargetsLast = 0
	schedulerRuntime.keeperRefreshRequests = 0
	schedulerRuntime.keeperRefreshRequestedAt = time.Time{}
	schedulerRuntime.keeperRefreshNextAllowedAt = time.Time{}
	schedulerRuntime.keeperRefreshAttempt = 0
	schedulerRuntime.keeperRefreshAcceptedLast = 0
	schedulerRuntime.keeperRefreshSkippedLast = 0
	schedulerRuntime.keeperRefreshRejectedLast = make(map[string]int)
	schedulerRuntime.keeperRefreshLastError = ""
	schedulerRuntime.keeperRefreshFingerprint = ""
	schedulerRuntime.keeperRefreshRecoveries = 0
	schedulerRuntime.banResetConfirmations = make(map[string]banResetConfirmation)
	schedulerRuntime.sessionToken = ""
	schedulerRuntime.sessionExpiry = time.Time{}
	schedulerRuntime.serialActiveAuthID = ""
	schedulerRuntime.serialSelectionSource = "auto"
	schedulerRuntime.serialSelectedAt = time.Time{}
	schedulerRuntime.serialSwitches = 0
	schedulerRuntime.serialFallbacks = 0
	schedulerRuntime.serialLastSwitchAt = time.Time{}
	schedulerRuntime.serialLastSwitchReason = ""
	schedulerRuntime.serialMissingAuthID = ""
	schedulerRuntime.serialFallbackAuthID = ""
	schedulerRuntime.serialMissingSince = time.Time{}
	schedulerRuntime.serialMissingCount = 0
	schedulerRuntime.serialOverdraft = make(map[string]serialOverdraftBinding)
	schedulerRuntime.serialLastSelected = make(map[string]time.Time)
	schedulerRuntime.serialFiveHourCycle = make(map[string]time.Time)
	if schedulerRuntime.pricing == nil {
		schedulerRuntime.pricing = make(map[string]modelPricing)
	}
	if schedulerRuntime.costSamples == nil {
		schedulerRuntime.costSamples = make(map[string][]float64)
	}
	if schedulerRuntime.pacingAccounts == nil {
		schedulerRuntime.pacingAccounts = make(map[string]*accountPacingState)
	}
	if schedulerRuntime.stickyBindings == nil {
		schedulerRuntime.stickyBindings = make(map[string]stickyBinding)
	}
	schedulerRuntime.configGeneration++
	schedulerRuntime.mu.Unlock()

	loadBanState(cfg.StatePath)
	if !cfg.Enabled || strings.TrimSpace(cfg.KeeperURL) == "" {
		return
	}
	if err := schedulerRuntime.reserveGenerationOwnership(cfg.StatePath); err != nil {
		schedulerRuntime.recordRefreshError(err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	schedulerRuntime.mu.Lock()
	schedulerRuntime.cancel = cancel
	schedulerRuntime.wg.Add(1)
	schedulerRuntime.mu.Unlock()
	go schedulerRuntime.refreshLoop(ctx)
}

func (s *schedulerRuntimeState) stop() {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.stopLocked()
}

// stopLocked drains all admitted workers while the lifecycle gate prevents a
// concurrent reconfigure from adding a new refresh worker to the same
// WaitGroup. Callers must hold lifecycleMu.
func (s *schedulerRuntimeState) stopLocked() {
	s.mu.Lock()
	s.stopping = true
	cancel := s.cancel
	s.cancel = nil
	s.sessionToken = ""
	s.sessionExpiry = time.Time{}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	s.warmupMu.Lock()
	s.warmupRunning = false
	s.warmupLeases = make(map[string]warmupLease)
	s.warmupMu.Unlock()
	// Only the durable generation owner may commit or release shared state.
	// A superseded retired DSO must never overwrite the active instance.
	if s.generationOwnerActive() {
		s.persistBanState()
		if err := s.releaseGenerationOwnership(); err != nil {
			slog.Warn("codex-quota-scheduler: could not release generation ownership", "error", err)
		}
	}
}

func (s *schedulerRuntimeState) admitBackgroundWorker() bool {
	// A runtime with a durable state_path may only start background work after
	// it owns the active generation. Check before touching the WaitGroup so an
	// unclaimed/retired DSO fails closed during a hot reload.
	if !s.generationOwnerActive() {
		return false
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		return false
	}
	s.wg.Add(1)
	s.mu.Unlock()

	// A takeover can occur between the first ownership check and admission.
	// Revalidate after Add so stop() can still drain deterministically; if the
	// generation changed, balance the WaitGroup and never launch the worker.
	if !s.generationOwnerActive() {
		s.wg.Done()
		return false
	}
	return true
}

func (s *schedulerRuntimeState) refreshLoop(ctx context.Context) {
	defer s.wg.Done()
	// Cold-start immediately, then refresh on the configured cadence.  A
	// failed Keeper call is intentionally non-fatal; schedulerPick simply
	// returns Handled=false until a fresh snapshot exists.
	s.refreshOnce(ctx)
	s.mu.RLock()
	interval := s.cfg.RefreshInterval
	s.mu.RUnlock()
	if interval < time.Second {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refreshOnce(ctx)
		}
	}
}

func (s *schedulerRuntimeState) refreshOnce(ctx context.Context) {
	if !s.generationCanRefresh() {
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if !cfg.Enabled || strings.TrimSpace(cfg.KeeperURL) == "" {
		return
	}
	passwordRaw, err := os.ReadFile(cfg.KeeperPasswordFile)
	if err != nil {
		s.recordRefreshError(fmt.Errorf("read Keeper password file: %w", err))
		return
	}
	password := strings.TrimSpace(string(passwordRaw))
	if password == "" {
		s.recordRefreshError(errors.New("Keeper password file is empty"))
		return
	}

	token, err := s.keeperSession(ctx, cfg, password, false)
	if err != nil {
		s.recordRefreshError(err)
		return
	}
	identityBody, status, err := keeperJSON(ctx, cfg.KeeperURL, token, http.MethodGet, "/usage/identities", nil)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// Session TTLs vary between Keeper versions.  Retry once with a fresh
		// login instead of waiting for the next 30-second tick.
		token, err = s.keeperSession(ctx, cfg, password, true)
		if err == nil {
			identityBody, status, err = keeperJSON(ctx, cfg.KeeperURL, token, http.MethodGet, "/usage/identities", nil)
		}
	}
	if err != nil || status < 200 || status >= 300 {
		if err == nil {
			err = fmt.Errorf("Keeper identities returned HTTP %d", status)
		}
		s.recordRefreshError(err)
		return
	}

	var identitiesResp struct {
		Identities []keeperIdentity `json:"identities"`
	}
	if err := json.Unmarshal(identityBody, &identitiesResp); err != nil {
		s.recordRefreshError(fmt.Errorf("decode Keeper identities: %w", err))
		return
	}
	indexToFile, indexes := enabledKeeperCodexIdentities(identitiesResp.Identities)
	if len(indexes) == 0 {
		s.recordRefreshError(errors.New("Keeper returned no active Codex identities"))
		return
	}

	body, _ := json.Marshal(map[string]any{"auth_indexes": indexes})
	cacheBody, status, err := keeperJSON(ctx, cfg.KeeperURL, token, http.MethodPost, "/quota/cache", body)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		token, err = s.keeperSession(ctx, cfg, password, true)
		if err == nil {
			cacheBody, status, err = keeperJSON(ctx, cfg.KeeperURL, token, http.MethodPost, "/quota/cache", body)
		}
	}
	if err != nil || status < 200 || status >= 300 {
		if err == nil {
			err = fmt.Errorf("Keeper quota cache returned HTTP %d", status)
		}
		s.recordRefreshError(err)
		return
	}
	var cacheResp keeperCacheResponse
	if err := json.Unmarshal(cacheBody, &cacheResp); err != nil {
		s.recordRefreshError(fmt.Errorf("decode Keeper quota cache: %w", err))
		return
	}
	now := time.Now()
	quotas := make(map[string]quotaSnapshot, len(cacheResp.Items)*2)
	for _, item := range cacheResp.Items {
		if !strings.EqualFold(strings.TrimSpace(item.Status), "completed") || item.Quota == nil {
			continue
		}
		index := strings.TrimSpace(item.AuthIndex)
		activeFileName, requested := indexToFile[index]
		if index == "" || !requested {
			// Keeper may retain and return historical cache rows. Only commit
			// snapshots for indexes requested from its enabled identity inventory.
			continue
		}
		// The requested inventory is authoritative for the current filename;
		// do not let an old Keeper cache alias change scheduler identity keys.
		fileName := activeFileName
		refreshedAt := parseKeeperTime(item.RefreshedAt)
		snapshot := normalizeQuotaSnapshot(index, fileName, *item.Quota, refreshedAt, now)
		if len(snapshot.Windows) == 0 {
			continue
		}
		if fileName != "" {
			quotas[fileName] = snapshot
		}
		if index != "" {
			quotas[index] = snapshot
		}
	}
	if err := s.maybeRequestKeeperQuotaRefresh(ctx, cfg, password, token, indexes, cacheResp, quotas, now); err != nil {
		s.recordRefreshError(err)
	}
	// A quota ban that has reached its reset boundary must get a targeted
	// Keeper observation even when the broad cache response is temporarily
	// incomplete. This closes the gap where a 5h reset is visible in the UI but
	// the old scheduler cooldown still blocks warmup.
	if expiredTargets := collectExpiredQuotaBanRefreshTargets(indexToFile, quotas, now); len(expiredTargets) > 0 {
		if err := s.requestActiveCPAKeeperQuotaRefreshTargets(ctx, cfg, password, token, expiredTargets, now); err != nil {
			s.recordRefreshError(err)
		}
	}

	if len(quotas) == 0 {
		s.recordRefreshError(errors.New("Keeper quota cache contained no usable Codex windows"))
		return
	}

	pricing, pricingOK := fetchKeeperPricing(ctx, cfg, token)
	_, active, err := s.claimGenerationAfterSuccessfulRefresh()
	if err != nil {
		s.recordRefreshError(err)
		return
	}
	if !active {
		return
	}
	if !s.generationOwnerActive() {
		return
	}

	s.mu.Lock()
	quotas = s.mergePartialQuotaSnapshotsLocked(quotas, now)
	s.updateCalibrationsLocked(quotas, now)
	s.quotas = quotas
	s.identities = indexToFile
	if pricingOK {
		s.pricing = pricing
	}
	s.lastRefresh = now
	s.lastError = ""
	s.refreshes++
	s.mu.Unlock()
	if !s.generationOwnerActive() {
		return
	}
	if s.confirmPendingWarmups(quotas, now) {
		s.persistBanState()
	}
	s.reconcileExternallyResetQuotaBans(quotas, now)
	if confirmationTargets := s.pendingBanResetKeeperRefreshTargets(quotas); len(confirmationTargets) > 0 {
		if err := s.requestActiveCPAKeeperQuotaRefreshTargets(ctx, cfg, password, token, confirmationTargets, now); err != nil {
			s.recordRefreshError(err)
		}
	}
	s.scheduleWarmup(ctx, nil)
}

func fetchKeeperPricing(ctx context.Context, cfg pluginConfig, token string) (map[string]modelPricing, bool) {
	body, status, err := keeperJSON(ctx, cfg.KeeperURL, token, http.MethodGet, "/pricing", nil)
	if err != nil || status < 200 || status >= 300 {
		return nil, false
	}
	var response struct {
		Pricing []modelPricing `json:"pricing"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, false
	}
	out := make(map[string]modelPricing, len(response.Pricing))
	for _, pricing := range response.Pricing {
		model := normalizeModelName(pricing.Model)
		if model == "" {
			continue
		}
		if pricing.PriceMultiplier < 0 {
			pricing.PriceMultiplier = 1
		}
		out[model] = pricing
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func (s *schedulerRuntimeState) recordRefreshError(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Reconfigure/shutdown cancels an in-flight Keeper request by design;
		// it is not an outage and should not fill the CPA error log.
		return
	}
	s.mu.Lock()
	s.lastError = err.Error()
	s.mu.Unlock()
	// Do not include response bodies, URLs with credentials, or secret paths in
	// the log.  The error is intentionally short and operationally actionable.
	slog.Warn("codex-quota-scheduler: Keeper quota refresh unavailable", "error", err)
}

type keeperIdentity struct {
	Identity  string `json:"identity"`
	FileName  string `json:"file_name"`
	Provider  string `json:"provider"`
	Type      string `json:"type"`
	Disabled  bool   `json:"disabled"`
	IsDeleted bool   `json:"is_deleted"`
}

// enabledKeeperCodexIdentities returns only identities that Keeper's synced
// inventory reports as enabled. Keeper retains disabled auth-file rows for
// historical usage reporting, so is_deleted alone is not a sufficient filter.
// Sending those historical indexes to /quota/refresh makes Keeper invoke a
// credential that CPA has intentionally taken out of service.
func enabledKeeperCodexIdentities(identities []keeperIdentity) (map[string]string, []string) {
	indexToFile := make(map[string]string)
	indexes := make([]string, 0, len(identities))
	seen := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if !strings.EqualFold(strings.TrimSpace(identity.Provider), providerCodex) &&
			!strings.EqualFold(strings.TrimSpace(identity.Type), providerCodex) {
			continue
		}
		index := strings.TrimSpace(identity.Identity)
		fileName := strings.TrimSpace(identity.FileName)
		if index == "" || fileName == "" || identity.Disabled || identity.IsDeleted {
			continue
		}
		indexToFile[index] = fileName
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		indexes = append(indexes, index)
	}
	return indexToFile, indexes
}

type keeperCacheResponse struct {
	Items []keeperCacheItem `json:"items"`
}

type keeperCacheItem struct {
	AuthIndex      string               `json:"auth_index"`
	FileName       string               `json:"file_name"`
	Status         string               `json:"status"`
	Quota          *keeperCheckResponse `json:"quota"`
	Error          string               `json:"error"`
	HTTPStatusCode *int                 `json:"http_status_code"`
	ExpiresAt      json.RawMessage      `json:"expires_at"`
	RefreshedAt    json.RawMessage      `json:"refreshed_at"`
}

type keeperCheckResponse struct {
	Quota                               []keeperQuotaRow `json:"quota"`
	RateLimitResetCreditsAvailableCount *int             `json:"rateLimitResetCreditsAvailableCount"`
}

type keeperQuotaRow struct {
	Key               string             `json:"key"`
	Label             string             `json:"label"`
	UsedPercent       *float64           `json:"usedPercent"`
	Allowed           *bool              `json:"allowed"`
	LimitReached      *bool              `json:"limitReached"`
	Window            *keeperQuotaWindow `json:"window"`
	ResetAt           json.RawMessage    `json:"resetAt"`
	ResetAfterSeconds *int64             `json:"resetAfterSeconds"`
	WindowUsageCost   *float64           `json:"window_usage_cost"`
}

type keeperQuotaWindow struct {
	Seconds *int64 `json:"seconds"`
}

func normalizeQuotaSnapshot(index, fileName string, response keeperCheckResponse, refreshedAt, now time.Time) quotaSnapshot {
	out := quotaSnapshot{AuthID: fileName, AuthIndex: index, RefreshedAt: refreshedAt}
	if response.RateLimitResetCreditsAvailableCount != nil && *response.RateLimitResetCreditsAvailableCount > 0 {
		out.ResetCredits = *response.RateLimitResetCreditsAvailableCount
	}
	for _, row := range response.Quota {
		class := normalizeWindowClass(row.Label)
		seconds := int64(0)
		if row.Window != nil && row.Window.Seconds != nil {
			seconds = *row.Window.Seconds
		}
		if class == "" {
			class = windowClassFromSeconds(seconds)
		}
		if class == "" {
			// A future Keeper window type should not make the plugin unusable;
			// retain it as an unknown/lowest-priority window.
			class = "unknown"
		}
		used := 0.0
		if row.UsedPercent != nil {
			used = clampPercent(*row.UsedPercent)
		}
		allowed := true
		if row.Allowed != nil {
			allowed = *row.Allowed
		}
		limitReached := false
		if row.LimitReached != nil {
			limitReached = *row.LimitReached
		}
		resetAt := parseKeeperTime(row.ResetAt)
		if resetAt.IsZero() && row.ResetAfterSeconds != nil && *row.ResetAfterSeconds > 0 {
			resetAt = now.Add(time.Duration(*row.ResetAfterSeconds) * time.Second)
		}
		window := quotaWindow{
			Class:             class,
			WindowSeconds:     seconds,
			UsedPercent:       used,
			UsedPercentKnown:  row.UsedPercent != nil,
			Allowed:           allowed,
			AllowedKnown:      row.Allowed != nil,
			LimitReached:      limitReached || used >= usedPercentThreshold,
			LimitReachedKnown: row.LimitReached != nil,
			ResetAt:           resetAt,
			Source:            quotaSourceKeeper,
			ObservedAt:        refreshedAt,
		}
		if row.ResetAfterSeconds != nil && *row.ResetAfterSeconds >= 0 {
			window.ResetAfterSeconds = *row.ResetAfterSeconds
			window.ResetAfterSecondsKnown = true
		}
		if row.WindowUsageCost != nil && *row.WindowUsageCost >= 0 {
			window.WindowUsageCredits = *row.WindowUsageCost
			window.WindowUsageCreditsKnown = true
		}
		out.Windows = append(out.Windows, window)
	}
	return out
}

func (s *schedulerRuntimeState) mergePartialQuotaSnapshotsLocked(current map[string]quotaSnapshot, now time.Time) map[string]quotaSnapshot {
	if len(current) == 0 || len(s.quotas) == 0 {
		return current
	}
	out := make(map[string]quotaSnapshot, len(current))
	for key, snapshot := range current {
		previous, ok := s.quotas[key]
		if !ok && strings.TrimSpace(snapshot.AuthID) != "" {
			previous, ok = s.quotas[strings.TrimSpace(snapshot.AuthID)]
		}
		if !ok && strings.TrimSpace(snapshot.AuthIndex) != "" {
			previous, ok = s.quotas[strings.TrimSpace(snapshot.AuthIndex)]
		}
		if ok {
			snapshot = mergePartialQuotaSnapshot(previous, snapshot, now, s.cfg.StaleAfter)
		}
		out[key] = snapshot
	}
	return out
}

func mergePartialQuotaSnapshot(previous, current quotaSnapshot, now time.Time, staleAfter time.Duration) quotaSnapshot {
	if previous.RefreshedAt.IsZero() || now.Before(previous.RefreshedAt) || now.Sub(previous.RefreshedAt) > staleAfter {
		return current
	}
	present := make(map[string]struct{}, len(current.Windows))
	for _, window := range current.Windows {
		present[window.Class] = struct{}{}
	}
	for _, window := range previous.Windows {
		if _, ok := present[window.Class]; ok {
			continue
		}
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		if window.ObservedAt.IsZero() {
			window.ObservedAt = previous.RefreshedAt
		}
		current.Windows = append(current.Windows, window)
		present[window.Class] = struct{}{}
	}
	return current
}

func windowClassFromSeconds(seconds int64) string {
	switch {
	case seconds > 0 && seconds <= 6*60*60:
		return "5h"
	case seconds > 6*60*60 && seconds <= 8*24*60*60:
		return "weekly"
	case seconds > 8*24*60*60 && seconds <= 35*24*60*60:
		return "monthly"
	default:
		return ""
	}
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func parseKeeperTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return t
		}
		if n, err := strconv.ParseInt(text, 10, 64); err == nil && n > 0 {
			return time.Unix(n, 0)
		}
		return time.Time{}
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return time.Unix(int64(number), 0)
	}
	return time.Time{}
}

func (s *schedulerRuntimeState) keeperSession(ctx context.Context, cfg pluginConfig, password string, force bool) (string, error) {
	now := time.Now()
	s.mu.RLock()
	if !force && s.sessionToken != "" && now.Before(s.sessionExpiry) {
		token := s.sessionToken
		s.mu.RUnlock()
		return token, nil
	}
	s.mu.RUnlock()

	payload, _ := json.Marshal(map[string]string{"password": password})
	body, status, err := keeperJSON(ctx, cfg.KeeperURL, "", http.MethodPost, "/auth/login", payload)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("Keeper login returned HTTP %d", status)
	}
	var response struct {
		SessionToken string `json:"session_token"`
		Token        string `json:"token"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Keeper login response: %w", err)
	}
	token := strings.TrimSpace(response.SessionToken)
	if token == "" {
		token = strings.TrimSpace(response.Token)
	}
	if token == "" {
		return "", errors.New("Keeper login returned no session token")
	}
	s.mu.Lock()
	s.sessionToken = token
	// Keeper's configured session TTL is currently one week.  Refreshing the
	// cached token every six hours keeps behavior safe across deployments with a
	// shorter TTL and avoids logging in on every quota tick.
	s.sessionExpiry = now.Add(6 * time.Hour)
	s.mu.Unlock()
	return token, nil
}

func keeperJSON(ctx context.Context, baseURL, token, method, path string, body []byte) ([]byte, int, error) {
	endpoint, err := keeperEndpoint(baseURL, path)
	if err != nil {
		return nil, 0, err
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-CPA-Usage-Keeper-Request", "fetch")
	req.Header.Set("X-CPA-Usage-Keeper-Embed", "cpamc")
	if token != "" {
		req.Header.Set("X-CPA-Usage-Keeper-Embed-Session", token)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Keeper request failed: %w", err)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, resp.StatusCode, fmt.Errorf("read Keeper response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return data, resp.StatusCode, fmt.Errorf("Keeper returned HTTP %d", resp.StatusCode)
	}
	return data, resp.StatusCode, nil
}

func keeperEndpoint(baseURL, path string) (string, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "", errors.New("Keeper URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid Keeper URL")
	}
	basePath := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(basePath, "/api/v1") {
		basePath += "/api/v1"
	}
	u.Path = strings.TrimRight(basePath, "/") + "/" + strings.TrimLeft(path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// quotaWindowPatch carries only fields that were actually present and valid in
// one upstream response. Pointers are intentional: a missing field must never
// overwrite Keeper data with a zero value.
type quotaWindowPatch struct {
	Class         string
	WindowSeconds *int64
	UsedPercent   *float64
	ResetAt       *time.Time
}

func (p quotaWindowPatch) hasQuotaSignal() bool {
	return p.UsedPercent != nil || p.ResetAt != nil
}

// observeUsage is called on every Codex completion, not just 429s.  Response
// headers are merged into an existing Keeper snapshot field by field.  A
// header-only observation never advances RefreshedAt, which remains the
// authoritative Keeper freshness boundary.
func (s *schedulerRuntimeState) observeUsage(record pluginapi.UsageRecord) {
	if !record.Generate || !strings.EqualFold(strings.TrimSpace(record.Provider), providerCodex) {
		return
	}
	s.observeUsageCost(record)
	if len(record.ResponseHeaders) == 0 {
		return
	}
	now := time.Now()
	patches := quotaWindowPatchesFromHeaders(record.ResponseHeaders, now)
	if len(patches) == 0 {
		return
	}
	authID := strings.TrimSpace(record.AuthID)
	authIndex := strings.TrimSpace(record.AuthIndex)
	if authID == "" && authIndex == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if authID == "" && authIndex != "" {
		authID = s.identities[authIndex]
	}
	old, ok := s.quotas[authID]
	if !ok && authIndex != "" {
		old, ok = s.quotas[authIndex]
	}
	if !ok {
		// Headers expose only a subset of all possible windows.  Creating a
		// schedulable snapshot from them alone could hide a monthly limit.
		return
	}
	merged, changed, quotaSignal := mergeQuotaWindows(old.Windows, patches, now)
	if !changed && !quotaSignal {
		return
	}
	if old.AuthID == "" {
		old.AuthID = authID
	}
	if old.AuthIndex == "" {
		old.AuthIndex = authIndex
	}
	old.Windows = merged
	if quotaSignal {
		old.HeaderObservedAt = now
	}
	// Keeper's cache is the authoritative source for reset-credit counts.  The
	// response header only exposes a boolean, so never overwrite a cached count.
	if authID != "" {
		s.quotas[authID] = old
	}
	if authIndex != "" {
		s.quotas[authIndex] = old
	}
}

func quotaWindowPatchesFromHeaders(headers http.Header, now time.Time) []quotaWindowPatch {
	var out []quotaWindowPatch
	for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
		// Primary/secondary describe positions, not window classes. OpenAI can
		// emit an unused secondary placeholder with zero duration and 0% used.
		// Inferring a class from the position would then overwrite a real Keeper
		// weekly/monthly window. Only accept a header window whose positive
		// duration maps to a class we understand.
		rawMinutes := strings.TrimSpace(headers.Get(prefix + "window-minutes"))
		minutes, err := strconv.ParseInt(rawMinutes, 10, 64)
		if err != nil || minutes <= 0 {
			continue
		}
		seconds := minutes * 60
		class := windowClassFromSeconds(seconds)
		if class == "" {
			continue
		}
		patch := quotaWindowPatch{Class: class, WindowSeconds: &seconds}
		if raw := strings.TrimSpace(headers.Get(prefix + "used-percent")); raw != "" {
			if used, err := strconv.ParseFloat(raw, 64); err == nil {
				used = clampPercent(used)
				patch.UsedPercent = &used
			}
		}
		if reset, ok := parseHeaderTimeValue(headers.Get(prefix + "reset-at")); ok {
			patch.ResetAt = &reset
		} else if raw := strings.TrimSpace(headers.Get(prefix + "reset-after-seconds")); raw != "" {
			if after, err := strconv.ParseInt(raw, 10, 64); err == nil && after > 0 {
				reset = now.Add(time.Duration(after) * time.Second)
				patch.ResetAt = &reset
			}
		}
		if patch.WindowSeconds != nil || patch.hasQuotaSignal() {
			out = append(out, patch)
		}
	}
	return out
}

func mergeQuotaWindows(existing []quotaWindow, patches []quotaWindowPatch, observedAt time.Time) ([]quotaWindow, bool, bool) {
	out := append([]quotaWindow(nil), existing...)
	changed := false
	quotaSignal := false
	for _, patch := range patches {
		quotaSignal = quotaSignal || patch.hasQuotaSignal()
		index := -1
		for i := range out {
			if out[i].Class == patch.Class {
				index = i
				break
			}
		}
		if index < 0 {
			// Window duration by itself is metadata, not an eligibility signal.
			if !patch.hasQuotaSignal() {
				continue
			}
			window := quotaWindow{Class: patch.Class, Allowed: true, Source: quotaSourceHeader, ObservedAt: observedAt}
			if patch.WindowSeconds != nil {
				window.WindowSeconds = *patch.WindowSeconds
			}
			if patch.UsedPercent != nil {
				window.UsedPercent = *patch.UsedPercent
				window.UsedPercentKnown = true
				window.LimitReached = window.UsedPercent >= usedPercentThreshold
			}
			if patch.ResetAt != nil {
				window.ResetAt = *patch.ResetAt
			}
			out = append(out, window)
			changed = true
			continue
		}
		window := out[index]
		windowChanged := false
		if patch.WindowSeconds != nil && window.WindowSeconds != *patch.WindowSeconds {
			window.WindowSeconds = *patch.WindowSeconds
			windowChanged = true
		}
		if patch.UsedPercent != nil {
			limitReached := *patch.UsedPercent >= usedPercentThreshold
			if window.UsedPercent != *patch.UsedPercent || window.LimitReached != limitReached || !window.UsedPercentKnown {
				window.UsedPercent = *patch.UsedPercent
				window.UsedPercentKnown = true
				window.LimitReached = limitReached
				windowChanged = true
			}
		}
		if patch.ResetAt != nil && !window.ResetAt.Equal(*patch.ResetAt) {
			window.ResetAt = *patch.ResetAt
			windowChanged = true
		}
		if patch.hasQuotaSignal() && (window.ObservedAt.IsZero() || observedAt.After(window.ObservedAt)) {
			window.ObservedAt = observedAt
			windowChanged = true
		}
		if windowChanged {
			window.Source = mergeQuotaSource(window.Source, quotaSourceHeader)
			changed = true
		}
		out[index] = window
	}
	return out, changed, quotaSignal
}

func mergeQuotaSource(current, incoming quotaSource) quotaSource {
	if current == quotaSourceUnknown {
		return incoming
	}
	if incoming == quotaSourceUnknown || current == incoming {
		return current
	}
	return quotaSourceMixed
}

func quotaWindowsFromHeaders(headers http.Header, now time.Time) []quotaWindow {
	windows, _, _ := mergeQuotaWindows(nil, quotaWindowPatchesFromHeaders(headers, now), now)
	return windows
}

func parseHeaderTimeValue(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0), true
	}
	if value, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return value, true
	}
	return time.Time{}, false
}

type schedulerChoice struct {
	candidate pluginapi.SchedulerAuthCandidate
	snapshot  quotaSnapshot
	window    quotaWindow
	rank      int
	near      bool
	unknown   bool
}

func codexOnlySchedulerRequest(req pluginapi.SchedulerPickRequest) bool {
	if strings.TrimSpace(req.Provider) != "" && !strings.EqualFold(strings.TrimSpace(req.Provider), providerCodex) {
		return false
	}
	for _, provider := range req.Providers {
		if !strings.EqualFold(strings.TrimSpace(provider), providerCodex) {
			return false
		}
	}
	for _, candidate := range req.Candidates {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), providerCodex) {
			return false
		}
	}
	return true
}

func (s *schedulerRuntimeState) schedulerPick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if nonce := schedulerOptionHeader(req.Options.Headers, warmupRequestHeader); nonce != "" {
		now := time.Now()
		lease, validLease := s.consumeWarmupLease(nonce, now)
		if !validLease {
			return pluginapi.SchedulerPickResponse{}, fmt.Errorf("invalid or expired internal warmup lease")
		}
		targetAuthID := strings.TrimSpace(lease.AuthID)
		if len(req.Candidates) == 0 || !codexOnlySchedulerRequest(req) {
			return pluginapi.SchedulerPickResponse{}, fmt.Errorf("internal warmup target has no Codex candidates")
		}
		entry, quarantined := banStore.lookup(targetAuthID)
		if lease.RecoveryProbe {
			if !quarantined || banEntryDisposition(entry, now) != banDispositionHalfOpen ||
				lease.RecoveryBannedAt.IsZero() || lease.ProbeStartedAt.IsZero() ||
				!entry.BannedAt.Equal(lease.RecoveryBannedAt) || !entry.ProbeStartedAt.Equal(lease.ProbeStartedAt) {
				return pluginapi.SchedulerPickResponse{}, fmt.Errorf("internal recovery warmup target does not own the half-open lease")
			}
		} else if quarantined {
			return pluginapi.SchedulerPickResponse{}, fmt.Errorf("internal warmup target is quarantined")
		}
		for _, candidate := range req.Candidates {
			if strings.TrimSpace(candidate.ID) == targetAuthID && strings.EqualFold(strings.TrimSpace(candidate.Provider), providerCodex) {
				return pluginapi.SchedulerPickResponse{Handled: true, AuthID: targetAuthID}, nil
			}
		}
		return pluginapi.SchedulerPickResponse{}, fmt.Errorf("internal warmup target is unavailable")
	}
	if schedulerRequestGenerationDisabled(req) {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	// An old DSO may still have an in-flight callback after a new generation
	// claims the shared state. Warmup nonces are handled above because their
	// exact half-open lease is already durable; ordinary traffic must fail over
	// to the active generation instead of making a stale scheduling decision.
	if !s.generationOwnerActive() {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if len(req.Candidates) == 0 || !codexOnlySchedulerRequest(req) {
		// Codex-only isolation is a hard boundary. Mixed and third-party routes
		// always fall through to CPA without a plugin decision.
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	s.mu.RLock()
	mode := normalizeSchedulerMode(s.cfg.SchedulerMode)
	probeLease := s.cfg.HalfOpenProbeTimeout
	s.mu.RUnlock()
	if probeLease <= 0 {
		probeLease = 15 * time.Minute
	}

	remaining := append([]pluginapi.SchedulerAuthCandidate(nil), req.Candidates...)
	var lastLegacy pluginapi.SchedulerPickResponse
	var lastDynamic pluginapi.SchedulerPickResponse
	var lastCandidates []pacingCandidate
	for len(remaining) > 0 {
		// A newer hot-loaded DSO can claim the shared generation while this
		// callback is retrying candidates. Stop immediately instead of allowing
		// the retired instance to make a second scheduling decision.
		if !s.generationOwnerActive() {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		now := time.Now()
		attempt := req
		attempt.Candidates = remaining
		legacy := pluginapi.SchedulerPickResponse{Handled: false}
		dynamic := pluginapi.SchedulerPickResponse{Handled: false}
		returned := pluginapi.SchedulerPickResponse{Handled: false}
		var candidates []pacingCandidate
		if mode == "serial" {
			returned = s.serialPick(attempt, now)
		} else {
			var err error
			legacy, err = s.legacySchedulerPick(attempt, now)
			if err != nil {
				return pluginapi.SchedulerPickResponse{}, err
			}
			if mode != "legacy" {
				dynamic, candidates = s.pacingPick(attempt, now)
			}
			returned = legacy
			if mode == "enforce" && dynamic.Handled {
				returned = dynamic
			}
		}
		lastLegacy, lastDynamic, lastCandidates = legacy, dynamic, candidates
		if !returned.Handled {
			s.recordDecisionAudit(attempt, mode, legacy, dynamic, returned, candidates, now)
			return returned, nil
		}

		allowed, probeStarted := banStore.tryStartProbe(returned.AuthID, now, probeLease)
		if !allowed {
			// Another concurrent pick won the half-open lease after this decision
			// was ranked. Retry with that credential removed instead of issuing a
			// second probe.
			remaining = schedulerCandidatesWithout(remaining, returned.AuthID)
			continue
		}
		if probeStarted {
			entry, stillOwned := banStore.lookup(returned.AuthID)
			if !stillOwned || entry.Phase != banPhaseHalfOpen || !entry.ProbeStartedAt.Equal(now) ||
				!s.persistProbeAdmission(returned.AuthID, entry.BannedAt, entry.ProbeStartedAt) {
				if !s.generationOwnerActive() {
					return pluginapi.SchedulerPickResponse{Handled: false}, nil
				}
				remaining = schedulerCandidatesWithout(remaining, returned.AuthID)
				continue
			}
			slog.Info("codex-quota-scheduler: half-open probe admitted", "auth_id", returned.AuthID, "lease_until", now.Add(probeLease).Format(time.RFC3339))
		}
		s.recordDecisionAudit(attempt, mode, legacy, dynamic, returned, candidates, now)
		if returned.Handled {
			for _, candidate := range candidates {
				if candidate.Candidate.ID == returned.AuthID {
					s.recordPredictedDebit(returned.AuthID, candidate.PredictedCredits)
					break
				}
			}
		}
		return returned, nil
	}

	// Every candidate became unavailable while concurrent picks were racing for
	// a half-open lease. Keep the plugin non-fatal and let CPA apply its normal
	// no-auth/retry behavior; the audit still records the collision.
	now := time.Now()
	returned := pluginapi.SchedulerPickResponse{Handled: false}
	s.recordDecisionAudit(req, mode, lastLegacy, lastDynamic, returned, lastCandidates, now)
	return returned, nil
}

func schedulerRequestGenerationDisabled(req pluginapi.SchedulerPickRequest) bool {
	for key, value := range req.Options.Metadata {
		if !strings.EqualFold(strings.TrimSpace(key), "generate") {
			continue
		}
		generate, ok := value.(bool)
		return ok && !generate
	}
	return false
}

func schedulerOptionHeader(headers map[string][]string, key string) string {
	for candidateKey, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(candidateKey), key) {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func schedulerCandidatesWithout(candidates []pluginapi.SchedulerAuthCandidate, authID string) []pluginapi.SchedulerAuthCandidate {
	authID = strings.TrimSpace(authID)
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == authID {
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func (s *schedulerRuntimeState) legacySchedulerPick(req pluginapi.SchedulerPickRequest, now time.Time) (pluginapi.SchedulerPickResponse, error) {
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	known := make([]schedulerChoice, 0, len(req.Candidates))
	unknown := make([]pluginapi.SchedulerAuthCandidate, 0)
	for _, candidate := range req.Candidates {
		if !banStore.schedulable(candidate.ID, now) {
			continue
		}
		snapshot, found := s.lookupQuota(candidate.ID)
		if !found || !s.snapshotFresh(snapshot, now) {
			unknown = append(unknown, candidate)
			continue
		}
		evaluation := s.evaluateQuotaSnapshot(snapshot, now)
		if !evaluation.Known {
			unknown = append(unknown, candidate)
			continue
		}
		if !evaluation.Eligible {
			// One exhausted active window excludes the whole credential.
			continue
		}
		window := evaluation.Bottleneck
		known = append(known, schedulerChoice{
			candidate: candidate,
			snapshot:  snapshot,
			window:    window,
			rank:      s.windowRank(window.Class),
			near:      window.UsedPercent >= s.softLimit(),
		})
	}
	if len(known) == 0 {
		if len(unknown) == 0 {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		// Do not manufacture a quota decision when no fresh Keeper data exists.
		// Preserve CPA's native fill-first/session-affinity behavior.
		if len(unknown) == len(req.Candidates) {
			return pluginapi.SchedulerPickResponse{Handled: false}, nil
		}
		// Some accounts were known but exhausted.  An unknown account is safer
		// than sending traffic to a credential we know is full.
		chosen := unknown[s.nextPick(len(unknown))]
		return pluginapi.SchedulerPickResponse{AuthID: chosen.ID, Handled: true}, nil
	}

	sort.SliceStable(known, func(i, j int) bool {
		return s.choiceLess(known[i], known[j])
	})
	// Rotate exact-score ties so two healthy accounts do not collapse onto one
	// credential while preserving the explicit window/credit ordering.
	best := known[0]
	tieCount := 1
	for tieCount < len(known) && s.choiceEquivalent(best, known[tieCount]) {
		tieCount++
	}
	if tieCount > 1 {
		best = known[s.nextPick(tieCount)]
	}
	return pluginapi.SchedulerPickResponse{AuthID: best.candidate.ID, Handled: true}, nil
}

func (s *schedulerRuntimeState) authIDForIndex(authIndex string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.TrimSpace(s.identities[strings.TrimSpace(authIndex)])
}

func (s *schedulerRuntimeState) lookupQuota(authID string) (quotaSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.quotas[strings.TrimSpace(authID)]
	return snapshot, ok
}

func quotaSnapshotFresh(snapshot quotaSnapshot, now time.Time, staleAfter time.Duration) bool {
	age := now.Sub(snapshot.RefreshedAt)
	if snapshot.RefreshedAt.IsZero() || age < 0 || age > staleAfter {
		return false
	}
	for _, window := range snapshot.Windows {
		if !window.ResetAt.IsZero() && !now.Before(window.ResetAt) {
			continue
		}
		observedAt := window.ObservedAt
		if observedAt.IsZero() {
			observedAt = snapshot.RefreshedAt
		}
		windowAge := now.Sub(observedAt)
		if observedAt.IsZero() || windowAge < 0 || windowAge > staleAfter {
			return false
		}
	}
	return true
}

func (s *schedulerRuntimeState) snapshotFresh(snapshot quotaSnapshot, now time.Time) bool {
	s.mu.RLock()
	staleAfter := s.cfg.StaleAfter
	s.mu.RUnlock()
	return quotaSnapshotFresh(snapshot, now, staleAfter)
}

func (s *schedulerRuntimeState) softLimit() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.SoftLimitPercent <= 0 {
		return 98
	}
	return s.cfg.SoftLimitPercent
}

func (s *schedulerRuntimeState) windowRank(class string) int {
	s.mu.RLock()
	order := append([]string(nil), s.cfg.WindowOrder...)
	s.mu.RUnlock()
	for i, item := range order {
		if item == class {
			return i
		}
	}
	return len(order) + 10
}

type quotaEvaluation struct {
	Known         bool
	Eligible      bool
	Bottleneck    quotaWindow
	ActiveWindows int
	Reason        string
}

func (s *schedulerRuntimeState) evaluateQuotaSnapshot(snapshot quotaSnapshot, now time.Time) quotaEvaluation {
	s.mu.RLock()
	order := append([]string(nil), s.cfg.WindowOrder...)
	s.mu.RUnlock()
	return quotaEvaluationWithOrder(snapshot, now, order)
}

func (s *schedulerRuntimeState) windowMoreRestrictive(a, b quotaWindow) bool {
	s.mu.RLock()
	order := append([]string(nil), s.cfg.WindowOrder...)
	s.mu.RUnlock()
	return windowMoreRestrictiveWithOrder(a, b, order)
}

func (s *schedulerRuntimeState) bestWindow(snapshot quotaSnapshot, now time.Time) (quotaWindow, bool) {
	evaluation := s.evaluateQuotaSnapshot(snapshot, now)
	return evaluation.Bottleneck, evaluation.Known && evaluation.Eligible
}

func (s *schedulerRuntimeState) choiceLess(a, b schedulerChoice) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	s.mu.RLock()
	preferCredits := s.cfg.PreferResetCredits
	s.mu.RUnlock()
	if preferCredits && a.near && b.near && a.snapshot.ResetCredits != b.snapshot.ResetCredits {
		return a.snapshot.ResetCredits > b.snapshot.ResetCredits
	}
	if a.near != b.near {
		return !a.near
	}
	if a.window.UsedPercent != b.window.UsedPercent {
		return a.window.UsedPercent < b.window.UsedPercent
	}
	if !a.window.ResetAt.Equal(b.window.ResetAt) {
		if a.window.ResetAt.IsZero() {
			return false
		}
		if b.window.ResetAt.IsZero() {
			return true
		}
		return a.window.ResetAt.Before(b.window.ResetAt)
	}
	if a.candidate.Priority != b.candidate.Priority {
		return a.candidate.Priority > b.candidate.Priority
	}
	return a.candidate.ID < b.candidate.ID
}

func (s *schedulerRuntimeState) choiceEquivalent(a, b schedulerChoice) bool {
	s.mu.RLock()
	preferCredits := s.cfg.PreferResetCredits
	s.mu.RUnlock()
	creditsEquivalent := !preferCredits || !(a.near && b.near) || a.snapshot.ResetCredits == b.snapshot.ResetCredits
	return a.rank == b.rank && a.near == b.near &&
		creditsEquivalent &&
		a.window.UsedPercent == b.window.UsedPercent &&
		a.candidate.Priority == b.candidate.Priority
}

func (s *schedulerRuntimeState) nextPick(n int) int {
	if n <= 1 {
		return 0
	}
	s.mu.Lock()
	value := s.pickCounter
	s.pickCounter++
	s.mu.Unlock()
	return int(value % uint64(n))
}

type persistedBanState struct {
	Version                int                               `json:"version"`
	Bans                   map[string]banEntry               `json:"bans"`
	Warmups                map[string]warmupEntry            `json:"warmups,omitempty"`
	BanResetConfirmations  map[string]banResetConfirmation   `json:"ban_reset_confirmations,omitempty"`
	SerialActiveAuthID     string                            `json:"serial_active_auth_id,omitempty"`
	SerialSelectionSource  string                            `json:"serial_selection_source,omitempty"`
	SerialSelectedAt       time.Time                         `json:"serial_selected_at,omitempty"`
	SerialSwitches         uint64                            `json:"serial_switches,omitempty"`
	SerialFallbacks        uint64                            `json:"serial_provisional_fallbacks,omitempty"`
	SerialLastSwitchAt     time.Time                         `json:"serial_last_switch_at,omitempty"`
	SerialLastSwitchReason string                            `json:"serial_last_switch_reason,omitempty"`
	SerialOverdraft        map[string]serialOverdraftBinding `json:"serial_overdraft,omitempty"`
	SerialLastSelected     map[string]time.Time              `json:"serial_last_selected,omitempty"`
	SerialFiveHourCycle    map[string]time.Time              `json:"serial_five_hour_cycle,omitempty"`
	SavedAt                time.Time                         `json:"saved_at"`
}

func loadBanState(path string) {
	schedulerRuntime.loadBanState(path)
}

func (s *schedulerRuntimeState) loadBanState(path string) {
	s.loadBanStateWithConfirmationMode(path, false)
}

// loadBanStateAfterGenerationClaim is called after the durable generation
// fence has moved but before the incoming runtime marks itself claimed in
// memory. Bans and warmup outcomes keep merge semantics so usage delivered to
// the incoming CPA capability is not lost. Reset confirmations are different:
// deletion is meaningful evidence that an intervening snapshot invalidated the
// first proof, so the just-fenced on-disk set is authoritative.
func (s *schedulerRuntimeState) loadBanStateAfterGenerationClaim(path string) {
	s.loadBanStateWithConfirmationMode(path, true)
}

func (s *schedulerRuntimeState) loadBanStateWithConfirmationMode(path string, replaceConfirmations bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		if replaceConfirmations {
			s.replaceBanResetConfirmations(nil)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("codex-quota-scheduler: could not read persisted ban state", "error", err)
		}
		if replaceConfirmations {
			s.replaceBanResetConfirmations(nil)
		}
		return
	}
	var state persistedBanState
	if err := json.Unmarshal(raw, &state); err != nil {
		slog.Warn("codex-quota-scheduler: ignoring invalid persisted ban state", "error", err)
		if replaceConfirmations {
			s.replaceBanResetConfirmations(nil)
		}
		return
	}
	for authID, entry := range state.Bans {
		entry = normalizeBanEntry(entry)
		if strings.TrimSpace(authID) == "" || (entry.Kind != banKindBlocked && entry.ResetAt.IsZero()) {
			continue
		}
		banStore.set(authID, entry)
	}
	s.warmupMu.Lock()
	if s.warmups == nil {
		s.warmups = make(map[string]warmupEntry)
	}
	for key, entry := range state.Warmups {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(entry.AuthID) == "" || entry.AttemptedAt.IsZero() {
			continue
		}
		s.warmups[key] = entry
	}
	s.warmupMu.Unlock()
	validConfirmations := make(map[string]banResetConfirmation, len(state.BanResetConfirmations))
	for authID, confirmation := range state.BanResetConfirmations {
		if strings.TrimSpace(authID) == "" || confirmation.Confirmations < 1 || confirmation.LastSnapshotAt.IsZero() {
			continue
		}
		validConfirmations[authID] = confirmation
	}
	s.banResetMu.Lock()
	if replaceConfirmations || s.banResetConfirmations == nil {
		s.banResetConfirmations = make(map[string]banResetConfirmation, len(validConfirmations))
	}
	for authID, confirmation := range validConfirmations {
		s.banResetConfirmations[authID] = confirmation
	}
	s.banResetMu.Unlock()
	s.mu.Lock()
	s.serialActiveAuthID = strings.TrimSpace(state.SerialActiveAuthID)
	s.serialSelectionSource = normalizeSerialSelectionSource(state.SerialSelectionSource)
	if s.serialActiveAuthID == "" {
		s.serialSelectionSource = "auto"
	}
	s.serialOverdraft = make(map[string]serialOverdraftBinding, len(state.SerialOverdraft))
	if state.SerialOverdraft != nil {
		now := time.Now()
		for session, binding := range state.SerialOverdraft {
			if strings.TrimSpace(session) == "" || strings.TrimSpace(binding.AuthID) == "" {
				continue
			}
			if now.Sub(binding.LastUsedAt) > serialOverdraftTTL {
				continue
			}
			s.serialOverdraft[session] = binding
		}
	}
	s.serialSelectedAt = state.SerialSelectedAt
	s.serialSwitches = state.SerialSwitches
	s.serialFallbacks = state.SerialFallbacks
	s.serialLastSwitchAt = state.SerialLastSwitchAt
	s.serialLastSwitchReason = strings.TrimSpace(state.SerialLastSwitchReason)
	if state.SerialLastSelected != nil {
		s.serialLastSelected = make(map[string]time.Time, len(state.SerialLastSelected))
		for authID, selectedAt := range state.SerialLastSelected {
			if strings.TrimSpace(authID) == "" || selectedAt.IsZero() {
				continue
			}
			s.serialLastSelected[strings.TrimSpace(authID)] = selectedAt
		}
	} else if s.serialLastSelected == nil {
		s.serialLastSelected = make(map[string]time.Time)
	}
	if state.SerialFiveHourCycle != nil {
		s.serialFiveHourCycle = make(map[string]time.Time, len(state.SerialFiveHourCycle))
		for authID, resetAt := range state.SerialFiveHourCycle {
			if strings.TrimSpace(authID) == "" || resetAt.IsZero() {
				continue
			}
			s.serialFiveHourCycle[strings.TrimSpace(authID)] = resetAt
		}
	} else if s.serialFiveHourCycle == nil {
		s.serialFiveHourCycle = make(map[string]time.Time)
	}
	s.mu.Unlock()
}

func (s *schedulerRuntimeState) replaceBanResetConfirmations(confirmations map[string]banResetConfirmation) {
	s.banResetMu.Lock()
	s.banResetConfirmations = make(map[string]banResetConfirmation, len(confirmations))
	for authID, confirmation := range confirmations {
		s.banResetConfirmations[authID] = confirmation
	}
	s.banResetMu.Unlock()
}

func (s *schedulerRuntimeState) persistBanState() bool {
	if !s.generationOwnerActive() {
		return false
	}
	// Serialize snapshot-and-rename, not only the rename. Otherwise an older
	// concurrent snapshot could land after a newer one and lose quarantine
	// state on the next process restart.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	if !s.generationOwnerActive() {
		return false
	}

	s.mu.RLock()
	path := strings.TrimSpace(s.cfg.StatePath)
	serialActiveAuthID := strings.TrimSpace(s.serialActiveAuthID)
	serialSelectionSource := normalizeSerialSelectionSource(s.serialSelectionSource)
	serialSelectedAt := s.serialSelectedAt
	serialSwitches := s.serialSwitches
	serialFallbacks := s.serialFallbacks
	serialLastSwitchAt := s.serialLastSwitchAt
	serialLastSwitchReason := strings.TrimSpace(s.serialLastSwitchReason)
	serialLastSelected := make(map[string]time.Time, len(s.serialLastSelected))
	for authID, selectedAt := range s.serialLastSelected {
		if strings.TrimSpace(authID) == "" || selectedAt.IsZero() {
			continue
		}
		serialLastSelected[strings.TrimSpace(authID)] = selectedAt
	}
	serialFiveHourCycle := make(map[string]time.Time, len(s.serialFiveHourCycle))
	for authID, resetAt := range s.serialFiveHourCycle {
		if strings.TrimSpace(authID) == "" || resetAt.IsZero() {
			continue
		}
		serialFiveHourCycle[strings.TrimSpace(authID)] = resetAt
	}
	serialOverdraft := make(map[string]serialOverdraftBinding, len(s.serialOverdraft))
	now := time.Now()
	for session, binding := range s.serialOverdraft {
		if strings.TrimSpace(session) == "" || strings.TrimSpace(binding.AuthID) == "" {
			continue
		}
		if now.Sub(binding.LastUsedAt) > serialOverdraftTTL {
			continue
		}
		serialOverdraft[session] = binding
	}
	s.mu.RUnlock()
	if path == "" {
		return false
	}
	s.warmupMu.Lock()
	warmups := make(map[string]warmupEntry, len(s.warmups))
	for key, entry := range s.warmups {
		warmups[key] = entry
	}
	s.warmupMu.Unlock()
	s.banResetMu.Lock()
	confirmations := make(map[string]banResetConfirmation, len(s.banResetConfirmations))
	for authID, confirmation := range s.banResetConfirmations {
		confirmations[authID] = confirmation
	}
	s.banResetMu.Unlock()
	state := persistedBanState{
		Version:                5,
		Bans:                   banStore.snapshot(),
		Warmups:                warmups,
		BanResetConfirmations:  confirmations,
		SerialActiveAuthID:     serialActiveAuthID,
		SerialSelectionSource:  serialSelectionSource,
		SerialSelectedAt:       serialSelectedAt,
		SerialSwitches:         serialSwitches,
		SerialFallbacks:        serialFallbacks,
		SerialLastSwitchAt:     serialLastSwitchAt,
		SerialLastSwitchReason: serialLastSwitchReason,
		SerialOverdraft:        serialOverdraft,
		SerialLastSelected:     serialLastSelected,
		SerialFiveHourCycle:    serialFiveHourCycle,
		SavedAt:                time.Now(),
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return false
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Warn("codex-quota-scheduler: could not create ban state directory", "error", err)
		return false
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		slog.Warn("codex-quota-scheduler: could not create ban state temp file", "error", err)
		return false
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0600)
	if err := writeFileAll(tmp, raw); err != nil {
		_ = tmp.Close()
		return false
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return false
	}
	if err := tmp.Close(); err != nil {
		return false
	}
	// Verify and rename under the same generation-file OS lock used by claim.
	// A new owner therefore cannot interleave between the fence check and the
	// shared-state commit.
	committed, err := s.withGenerationOwnerCommit(func() error {
		return os.Rename(tmpName, path)
	})
	if err != nil {
		slog.Warn("codex-quota-scheduler: could not persist ban state", "error", err)
		return false
	}
	if !committed {
		return false
	}
	return true
}

type runtimeStatus struct {
	Enabled                  bool                     `json:"enabled"`
	SchedulerMode            string                   `json:"scheduler_mode"`
	SerialSwitchPercent      float64                  `json:"serial_switch_percent"`
	SerialHandoffMode        string                   `json:"serial_handoff_mode"`
	Serial5hHandoffMode      string                   `json:"serial_5h_handoff_mode"`
	Serial5hSwitchPercent    float64                  `json:"serial_5h_switch_percent"`
	Reserve5hPercent         float64                  `json:"reserve_5h_percent"`
	DrainWindowHours         float64                  `json:"drain_window_hours"`
	WarmupModel              string                   `json:"warmup_model"`
	SerialSelectionSource    string                   `json:"serial_selection_source"`
	SerialManualSelection    bool                     `json:"serial_manual_selection"`
	SerialManualActiveAuthID string                   `json:"serial_manual_active_auth_id,omitempty"`
	SerialActiveAuthID       string                   `json:"serial_active_auth_id,omitempty"`
	SerialSelectedAt         string                   `json:"serial_selected_at,omitempty"`
	SerialSwitches           uint64                   `json:"serial_switches"`
	SerialFallbacks          uint64                   `json:"serial_provisional_fallbacks"`
	SerialFallbackAuth       string                   `json:"serial_provisional_auth_id,omitempty"`
	SerialMissingSince       string                   `json:"serial_candidate_missing_since,omitempty"`
	SerialMissingCount       int                      `json:"serial_candidate_missing_confirmations,omitempty"`
	SerialLastSwitchAt       string                   `json:"serial_last_switch_at,omitempty"`
	SerialSwitchReason       string                   `json:"serial_last_switch_reason,omitempty"`
	SerialOverdraftSessions  int                      `json:"serial_overdraft_sessions"`
	ConfigGeneration         uint64                   `json:"config_generation"`
	RuntimeGeneration        uint64                   `json:"runtime_generation"`
	GenerationManaged        bool                     `json:"generation_managed"`
	GenerationClaimed        bool                     `json:"generation_claimed"`
	GenerationActive         bool                     `json:"generation_active"`
	GenerationReleased       bool                     `json:"generation_released"`
	GenerationSuperseded     bool                     `json:"generation_superseded"`
	GenerationObserved       uint64                   `json:"generation_observed"`
	GenerationOwner          string                   `json:"generation_owner,omitempty"`
	GenerationClaimedAt      string                   `json:"generation_claimed_at,omitempty"`
	GenerationReason         string                   `json:"generation_supersede_reason,omitempty"`
	KeeperConfigured         bool                     `json:"keeper_configured"`
	WarmupEnabled            bool                     `json:"warmup_enabled"`
	WarmupExecutionMode      string                   `json:"warmup_execution_mode"`
	WarmupCandidates         int                      `json:"warmup_candidates"`
	WarmupSkippedBanned      int                      `json:"warmup_skipped_banned"`
	WarmupSkippedStale       int                      `json:"warmup_skipped_stale"`
	WarmupSkippedIneligible  int                      `json:"warmup_skipped_ineligible"`
	WarmupSkippedNotNeeded   int                      `json:"warmup_skipped_not_unstarted"`
	WarmupAuthSource         string                   `json:"warmup_auth_source,omitempty"`
	WarmupAuthCheckedAt      string                   `json:"warmup_auth_checked_at,omitempty"`
	WarmupAuthFilesSeen      int                      `json:"warmup_auth_files_seen"`
	WarmupAuthEligible       int                      `json:"warmup_auth_eligible"`
	WarmupAuthRejected       map[string]int           `json:"warmup_auth_rejected,omitempty"`
	WarmupAuthLastError      string                   `json:"warmup_auth_last_error,omitempty"`
	KeeperRefreshTargets     int                      `json:"keeper_refresh_targets"`
	KeeperRefreshRequests    uint64                   `json:"keeper_refresh_requests"`
	KeeperRefreshRequestedAt string                   `json:"keeper_refresh_requested_at,omitempty"`
	KeeperRefreshNextAt      string                   `json:"keeper_refresh_next_allowed_at,omitempty"`
	KeeperRefreshAttempt     int                      `json:"keeper_refresh_attempt"`
	KeeperRefreshAccepted    int                      `json:"keeper_refresh_accepted"`
	KeeperRefreshSkipped     int                      `json:"keeper_refresh_skipped"`
	KeeperRefreshRejected    map[string]int           `json:"keeper_refresh_rejected,omitempty"`
	KeeperRefreshError       string                   `json:"keeper_refresh_error,omitempty"`
	KeeperRefreshRecoveries  uint64                   `json:"keeper_refresh_gate_recoveries"`
	BanResetPending          int                      `json:"ban_reset_pending_confirmations"`
	BanResetEvents           uint64                   `json:"ban_reset_confirmation_events"`
	BanExternalClears        uint64                   `json:"ban_external_reset_clears"`
	LastBanClearReason       string                   `json:"last_ban_clear_reason,omitempty"`
	LastBanClearAt           string                   `json:"last_ban_clear_at,omitempty"`
	Refreshes                int                      `json:"refreshes"`
	LastRefresh              string                   `json:"last_refresh,omitempty"`
	LastError                string                   `json:"last_error,omitempty"`
	FreshSnapshots           int                      `json:"fresh_snapshots"`
	WindowOrder              []string                 `json:"window_order"`
	PricingModels            int                      `json:"pricing_models"`
	CostProfiles             []runtimeCostProfile     `json:"cost_profiles"`
	Pacing                   []runtimePacingStatus    `json:"pacing,omitempty"`
	StickyBindings           int                      `json:"sticky_bindings"`
	SessionSwitches          uint64                   `json:"session_switches"`
	ShadowDisagreements      uint64                   `json:"shadow_disagreements"`
	Quarantine               runtimeQuarantineStatus  `json:"quarantine"`
	RecentDecisions          []schedulerDecisionAudit `json:"recent_decisions,omitempty"`
	Snapshots                []runtimeQuotaStatus     `json:"snapshots,omitempty"`
	Warmups                  []runtimeWarmupStatus    `json:"warmups,omitempty"`
}

type runtimeQuotaStatus struct {
	AuthID           string                     `json:"auth_id"`
	AuthIndex        string                     `json:"auth_index,omitempty"`
	Window           string                     `json:"window"`
	UsedPercent      float64                    `json:"used_percent"`
	ResetCredits     int                        `json:"reset_credits"`
	ResetAt          string                     `json:"reset_at,omitempty"`
	Fresh            bool                       `json:"fresh"`
	Eligible         bool                       `json:"eligible"`
	ActiveWindows    int                        `json:"active_windows"`
	Reason           string                     `json:"reason,omitempty"`
	Source           string                     `json:"source,omitempty"`
	HeaderObservedAt string                     `json:"header_observed_at,omitempty"`
	Windows          []runtimeQuotaWindowStatus `json:"windows,omitempty"`
}

type runtimeQuotaWindowStatus struct {
	Window                  string  `json:"window"`
	WindowSeconds           int64   `json:"window_seconds,omitempty"`
	ResetAfterSeconds       int64   `json:"reset_after_seconds,omitempty"`
	ResetAfterSecondsKnown  bool    `json:"reset_after_seconds_known"`
	UsedPercent             float64 `json:"used_percent"`
	RemainingPercent        float64 `json:"remaining_percent"`
	Allowed                 bool    `json:"allowed"`
	LimitReached            bool    `json:"limit_reached"`
	Active                  bool    `json:"active"`
	CycleStarted            bool    `json:"cycle_started"`
	PlaceholderReset        bool    `json:"placeholder_reset"`
	ResetAt                 string  `json:"reset_at,omitempty"`
	Source                  string  `json:"source,omitempty"`
	ObservedAt              string  `json:"observed_at,omitempty"`
	WindowUsageCredits      float64 `json:"window_usage_credits,omitempty"`
	WindowUsageCreditsKnown bool    `json:"window_usage_credits_known"`
}

type runtimeWarmupStatus struct {
	AuthID        string `json:"auth_id"`
	Window        string `json:"window"`
	State         string `json:"state"`
	AttemptedAt   string `json:"attempted_at,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
	ActivatedAt   string `json:"activated_at,omitempty"`
	ResetAt       string `json:"reset_at,omitempty"`
	SuppressUntil string `json:"suppress_until,omitempty"`
	Status        int    `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
	Blocked       bool   `json:"blocked,omitempty"`
}

type runtimeCostProfile struct {
	Model   string  `json:"model"`
	Effort  string  `json:"effort"`
	Source  string  `json:"source"`
	Samples int     `json:"samples"`
	P75     float64 `json:"p75_credits"`
	P90     float64 `json:"p90_credits"`
	P95     float64 `json:"p95_credits"`
}

type runtimePacingStatus struct {
	AuthID                   string                      `json:"auth_id"`
	DeficitCredits           float64                     `json:"deficit_credits"`
	DeficitPercent           float64                     `json:"deficit_percent"`
	AccountDebtPercent       float64                     `json:"account_debt_percent"`
	ReferenceCapacityCredits float64                     `json:"reference_capacity_credits"`
	PendingPredictedRequests int                         `json:"pending_predicted_requests"`
	LastAccruedAt            string                      `json:"last_accrued_at,omitempty"`
	Windows                  []runtimePacingWindowStatus `json:"windows,omitempty"`
}

type runtimePacingWindowStatus struct {
	Window                 string  `json:"window"`
	RemainingPercent       float64 `json:"remaining_percent"`
	ReservePercent         float64 `json:"reserve_percent"`
	PacingDebtPercent      float64 `json:"pacing_debt_percent"`
	CapacityCredits        float64 `json:"capacity_credits,omitempty"`
	CapacitySamples        int     `json:"capacity_samples"`
	CapacityUpdatedAt      string  `json:"capacity_updated_at,omitempty"`
	TargetCreditsPerSecond float64 `json:"target_credits_per_second"`
}

type runtimeQuarantineStatus struct {
	Total          int    `json:"total"`
	Cooldown       int    `json:"cooldown"`
	ProbeReady     int    `json:"probe_ready"`
	HalfOpen       int    `json:"half_open"`
	Probation      int    `json:"probation"`
	Total429s      uint64 `json:"total_429s"`
	Probation429s  uint64 `json:"probation_429s"`
	ProbeStarts    uint64 `json:"probe_starts"`
	ProbeSuccesses uint64 `json:"probe_successes"`
	ProbeFailures  uint64 `json:"probe_failures"`
}

type pacingStateSnapshot struct {
	DeficitCredits   float64
	LastAccruedAt    time.Time
	Capacities       map[string]capacityEstimate
	PendingPredicted int
}

func runtimeCostProfileFor(model, effort string, samples []float64) runtimeCostProfile {
	profile := runtimeCostProfile{Model: model, Effort: effort, Source: "bootstrap", Samples: len(samples)}
	if len(samples) > 0 {
		profile.Source = "observed"
		profile.P75 = sampleQuantile(samples, 0.75)
		profile.P90 = sampleQuantile(samples, 0.90)
		profile.P95 = sampleQuantile(samples, 0.95)
		return profile
	}
	profile.P75 = bootstrapCost(0.75)
	profile.P90 = bootstrapCost(0.90)
	profile.P95 = bootstrapCost(0.95)
	return profile
}

func (s *schedulerRuntimeState) status() runtimeStatus {
	s.mu.RLock()
	cfg := s.cfg
	quotas := make(map[string]quotaSnapshot, len(s.quotas))
	for key, snapshot := range s.quotas {
		quotas[key] = snapshot
	}
	costSamples := make(map[string][]float64, len(s.costSamples))
	for key, samples := range s.costSamples {
		costSamples[key] = append([]float64(nil), samples...)
	}
	globalCostSamples := append([]float64(nil), s.globalCostSamples...)
	pacingStates := make(map[string]pacingStateSnapshot, len(s.pacingAccounts))
	for authID, state := range s.pacingAccounts {
		if state == nil {
			continue
		}
		capacities := make(map[string]capacityEstimate, len(state.Capacities))
		for class, estimate := range state.Capacities {
			capacities[class] = estimate
		}
		pacingStates[authID] = pacingStateSnapshot{
			DeficitCredits:   state.DeficitCredits,
			LastAccruedAt:    state.LastAccruedAt,
			Capacities:       capacities,
			PendingPredicted: len(state.PendingPredicted),
		}
	}
	decisions := make([]schedulerDecisionAudit, len(s.decisionHistory))
	for index, decision := range s.decisionHistory {
		decision.Candidates = append([]schedulerCandidateAudit(nil), decision.Candidates...)
		decisions[index] = decision
	}
	bindings := make(map[string]stickyBinding, len(s.stickyBindings))
	for key, binding := range s.stickyBindings {
		bindings[key] = binding
	}
	lastRefresh := s.lastRefresh
	lastError := s.lastError
	refreshes := s.refreshes
	pricingModels := len(s.pricing)
	configGeneration := s.configGeneration
	sessionSwitches := s.sessionSwitches
	shadowDisagreements := s.shadowDisagreements
	serialActiveAuthID := s.serialActiveAuthID
	serialSelectionSource := normalizeSerialSelectionSource(s.serialSelectionSource)
	serialSelectedAt := s.serialSelectedAt
	serialSwitches := s.serialSwitches
	serialFallbacks := s.serialFallbacks
	serialFallbackAuthID := s.serialFallbackAuthID
	serialMissingSince := s.serialMissingSince
	serialMissingCount := s.serialMissingCount
	serialLastSwitchAt := s.serialLastSwitchAt
	serialLastSwitchReason := s.serialLastSwitchReason
	serialOverdraftSessions := len(s.serialOverdraft)
	warmupCandidates := s.warmupCandidatesLast
	warmupSkippedBanned := s.warmupSkippedBannedLast
	warmupSkippedStale := s.warmupSkippedStaleLast
	warmupSkippedIneligible := s.warmupSkippedIneligibleLast
	warmupSkippedNotNeeded := s.warmupSkippedNotNeededLast
	warmupAuthSource := s.warmupAuthSourceLast
	warmupAuthCheckedAt := s.warmupAuthCheckedAt
	warmupAuthFilesSeen := s.warmupAuthFilesSeenLast
	warmupAuthEligible := s.warmupAuthEligibleLast
	warmupAuthRejected := make(map[string]int, len(s.warmupAuthRejectedLast))
	for reason, count := range s.warmupAuthRejectedLast {
		warmupAuthRejected[reason] = count
	}
	warmupAuthLastError := s.warmupAuthLastError
	keeperRefreshTargets := s.keeperRefreshTargetsLast
	keeperRefreshRequests := s.keeperRefreshRequests
	keeperRefreshRequestedAt := s.keeperRefreshRequestedAt
	keeperRefreshNextAt := s.keeperRefreshNextAllowedAt
	keeperRefreshAttempt := s.keeperRefreshAttempt
	keeperRefreshAccepted := s.keeperRefreshAcceptedLast
	keeperRefreshSkipped := s.keeperRefreshSkippedLast
	keeperRefreshRejected := make(map[string]int, len(s.keeperRefreshRejectedLast))
	for reason, count := range s.keeperRefreshRejectedLast {
		keeperRefreshRejected[reason] = count
	}
	keeperRefreshLastError := s.keeperRefreshLastError
	keeperRefreshRecoveries := s.keeperRefreshRecoveries
	banResetEvents := s.banResetConfirmationEvents
	banExternalClears := s.banExternalResetClears
	lastBanClearReason := s.lastBanClearReason
	lastBanClearAt := s.lastBanClearAt
	s.mu.RUnlock()
	generation := s.generationStatus()

	s.warmupMu.Lock()
	warmups := make(map[string]warmupEntry, len(s.warmups))
	for key, entry := range s.warmups {
		warmups[key] = entry
	}
	s.warmupMu.Unlock()
	s.banResetMu.Lock()
	banResetPending := len(s.banResetConfirmations)
	s.banResetMu.Unlock()

	now := time.Now()
	stickyBindings := 0
	stickyTTL := time.Duration(cfg.StickySeconds) * time.Second
	for _, binding := range bindings {
		if cfg.StickySeconds <= 0 || now.Sub(binding.LastUsedAt) <= stickyTTL {
			stickyBindings++
		}
	}

	costProfiles := []runtimeCostProfile{runtimeCostProfileFor("*", "*", globalCostSamples)}
	for key, samples := range costSamples {
		parts := strings.SplitN(key, "\x00", 2)
		model, effort := parts[0], ""
		if len(parts) == 2 {
			effort = parts[1]
		}
		costProfiles = append(costProfiles, runtimeCostProfileFor(model, effort, samples))
	}
	sort.Slice(costProfiles[1:], func(i, j int) bool {
		a, b := costProfiles[i+1], costProfiles[j+1]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Effort < b.Effort
	})

	count := 0
	seen := make(map[string]struct{})
	snapshots := make([]runtimeQuotaStatus, 0)
	pacing := make([]runtimePacingStatus, 0)
	for _, snapshot := range quotas {
		canonical := strings.TrimSpace(snapshot.AuthID)
		if canonical == "" {
			canonical = strings.TrimSpace(snapshot.AuthIndex)
		}
		if canonical == "" {
			continue
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		fresh := quotaSnapshotFresh(snapshot, now, cfg.StaleAfter)
		if fresh {
			count++
		}
		evaluation := quotaEvaluationWithOrder(snapshot, now, cfg.WindowOrder)
		item := runtimeQuotaStatus{
			AuthID:        canonical,
			AuthIndex:     snapshot.AuthIndex,
			Fresh:         fresh,
			ResetCredits:  snapshot.ResetCredits,
			Eligible:      evaluation.Known && evaluation.Eligible,
			ActiveWindows: evaluation.ActiveWindows,
			Reason:        evaluation.Reason,
		}
		window := evaluation.Bottleneck
		if window.Class != "" {
			item.Window = window.Class
			item.UsedPercent = window.UsedPercent
			item.Source = string(window.Source)
			if !window.ResetAt.IsZero() {
				item.ResetAt = window.ResetAt.Format(time.RFC3339)
			}
		}
		if !snapshot.HeaderObservedAt.IsZero() {
			item.HeaderObservedAt = snapshot.HeaderObservedAt.Format(time.RFC3339)
		}
		for _, quotaWindow := range snapshot.Windows {
			active := quotaWindow.ResetAt.IsZero() || now.Before(quotaWindow.ResetAt)
			placeholderReset := quotaWindowHasPlaceholderReset(quotaWindow, snapshot.RefreshedAt, now)
			windowStatus := runtimeQuotaWindowStatus{
				Window:                  quotaWindow.Class,
				WindowSeconds:           quotaWindow.WindowSeconds,
				ResetAfterSeconds:       quotaWindow.ResetAfterSeconds,
				ResetAfterSecondsKnown:  quotaWindow.ResetAfterSecondsKnown,
				UsedPercent:             quotaWindow.UsedPercent,
				RemainingPercent:        math.Max(0, 100-quotaWindow.UsedPercent),
				Allowed:                 quotaWindow.Allowed,
				LimitReached:            quotaWindow.LimitReached,
				Active:                  active,
				CycleStarted:            quotaWindowCycleStarted(quotaWindow, snapshot.RefreshedAt, now),
				PlaceholderReset:        placeholderReset,
				Source:                  string(quotaWindow.Source),
				WindowUsageCredits:      quotaWindow.WindowUsageCredits,
				WindowUsageCreditsKnown: quotaWindow.WindowUsageCreditsKnown,
			}
			if !quotaWindow.ResetAt.IsZero() {
				windowStatus.ResetAt = quotaWindow.ResetAt.Format(time.RFC3339)
			}
			observedAt := quotaWindow.ObservedAt
			if observedAt.IsZero() {
				observedAt = snapshot.RefreshedAt
			}
			if !observedAt.IsZero() {
				windowStatus.ObservedAt = observedAt.Format(time.RFC3339)
			}
			item.Windows = append(item.Windows, windowStatus)
		}
		sort.Slice(item.Windows, func(i, j int) bool {
			return windowRankInOrder(item.Windows[i].Window, cfg.WindowOrder) < windowRankInOrder(item.Windows[j].Window, cfg.WindowOrder)
		})
		snapshots = append(snapshots, item)

		state, ok := pacingStates[canonical]
		if !ok && strings.TrimSpace(snapshot.AuthIndex) != "" {
			state, ok = pacingStates[strings.TrimSpace(snapshot.AuthIndex)]
		}
		pacingItem := runtimePacingStatus{AuthID: canonical}
		if ok {
			pacingItem.DeficitCredits = state.DeficitCredits
			pacingItem.PendingPredictedRequests = state.PendingPredicted
			if !state.LastAccruedAt.IsZero() {
				pacingItem.LastAccruedAt = state.LastAccruedAt.Format(time.RFC3339)
			}
		}
		accountDebt := math.Inf(1)
		referenceCapacity := math.Inf(1)
		for _, quotaWindow := range snapshot.Windows {
			if !quotaWindow.ResetAt.IsZero() && !now.Before(quotaWindow.ResetAt) {
				continue
			}
			reserve := reserveForWindow(cfg, quotaWindow.Class)
			estimate := state.Capacities[quotaWindow.Class]
			windowStatus := runtimePacingWindowStatus{
				Window:            quotaWindow.Class,
				RemainingPercent:  math.Max(0, 100-quotaWindow.UsedPercent),
				ReservePercent:    reserve,
				PacingDebtPercent: windowPacingDebtPercent(quotaWindow, reserve, now),
				CapacityCredits:   estimate.Credits,
				CapacitySamples:   estimate.Samples,
			}
			if !estimate.UpdatedAt.IsZero() {
				windowStatus.CapacityUpdatedAt = estimate.UpdatedAt.Format(time.RFC3339)
			}
			if estimate.Credits > 0 {
				if estimate.Credits < referenceCapacity {
					referenceCapacity = estimate.Credits
				}
				if !quotaWindow.ResetAt.IsZero() {
					secondsLeft := quotaWindow.ResetAt.Sub(now).Seconds()
					if secondsLeft > 0 {
						windowStatus.TargetCreditsPerSecond = estimate.Credits * math.Max(0, windowStatus.RemainingPercent-reserve) / 100 / secondsLeft
					}
				}
			}
			if windowStatus.PacingDebtPercent < accountDebt {
				accountDebt = windowStatus.PacingDebtPercent
			}
			pacingItem.Windows = append(pacingItem.Windows, windowStatus)
		}
		if !math.IsInf(accountDebt, 1) {
			pacingItem.AccountDebtPercent = accountDebt
		}
		if !math.IsInf(referenceCapacity, 1) && referenceCapacity > 0 {
			pacingItem.ReferenceCapacityCredits = referenceCapacity
			pacingItem.DeficitPercent = pacingItem.DeficitCredits * 100 / referenceCapacity
		}
		sort.Slice(pacingItem.Windows, func(i, j int) bool {
			return windowRankInOrder(pacingItem.Windows[i].Window, cfg.WindowOrder) < windowRankInOrder(pacingItem.Windows[j].Window, cfg.WindowOrder)
		})
		pacing = append(pacing, pacingItem)
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].AuthID < snapshots[j].AuthID })
	sort.Slice(pacing, func(i, j int) bool { return pacing[i].AuthID < pacing[j].AuthID })

	banSnapshot := banStore.snapshot()
	banStats := banStore.stats()
	quarantine := runtimeQuarantineStatus{
		Total:          len(banSnapshot),
		Total429s:      banStats.Total429s,
		Probation429s:  banStats.Probation429s,
		ProbeStarts:    banStats.ProbeStarts,
		ProbeSuccesses: banStats.ProbeSuccesses,
		ProbeFailures:  banStats.ProbeFailures,
	}
	for _, entry := range banSnapshot {
		if entry.Kind == banKindProbation {
			quarantine.Probation++
		}
		switch banEntryDisposition(entry, now) {
		case banDispositionCooldown:
			quarantine.Cooldown++
		case banDispositionProbeReady:
			quarantine.ProbeReady++
		case banDispositionHalfOpen:
			quarantine.HalfOpen++
		}
	}

	out := runtimeStatus{
		Enabled:                  cfg.Enabled,
		SchedulerMode:            cfg.SchedulerMode,
		SerialSwitchPercent:      cfg.SerialSwitchPercent,
		SerialHandoffMode:        normalizeSerialHandoffMode(cfg.SerialHandoffMode),
		Serial5hHandoffMode:      normalizeSerial5hHandoffMode(cfg.Serial5hHandoffMode),
		Serial5hSwitchPercent:    cfg.Serial5hSwitchPercent,
		Reserve5hPercent:         cfg.Reserve5hPercent,
		DrainWindowHours:         cfg.DrainWindowHours,
		WarmupModel:              cfg.WarmupModel,
		SerialActiveAuthID:       serialActiveAuthID,
		SerialSelectionSource:    serialSelectionSource,
		SerialManualSelection:    serialSelectionSource == "manual" && strings.TrimSpace(serialActiveAuthID) != "",
		SerialManualActiveAuthID: "",
		SerialSwitches:           serialSwitches,
		SerialFallbacks:          serialFallbacks,
		SerialFallbackAuth:       serialFallbackAuthID,
		SerialMissingCount:       serialMissingCount,
		SerialSwitchReason:       serialLastSwitchReason,
		SerialOverdraftSessions:  serialOverdraftSessions,
		ConfigGeneration:         configGeneration,
		RuntimeGeneration:        generation.Ticket,
		GenerationManaged:        generation.Managed,
		GenerationClaimed:        generation.Claimed,
		GenerationActive:         generation.Active,
		GenerationReleased:       generation.Released,
		GenerationSuperseded:     generation.Superseded,
		GenerationObserved:       generation.ObservedGeneration,
		GenerationOwner:          generation.OwnerFingerprint,
		GenerationReason:         generation.SupersedeReason,
		KeeperConfigured:         strings.TrimSpace(cfg.KeeperURL) != "",
		WarmupEnabled:            cfg.WarmupEnabled,
		WarmupExecutionMode:      normalizeWarmupExecutionMode(cfg.WarmupExecutionMode),
		Refreshes:                refreshes,
		FreshSnapshots:           count,
		WindowOrder:              append([]string(nil), cfg.WindowOrder...),
		PricingModels:            pricingModels,
		CostProfiles:             costProfiles,
		Pacing:                   pacing,
		StickyBindings:           stickyBindings,
		SessionSwitches:          sessionSwitches,
		ShadowDisagreements:      shadowDisagreements,
		WarmupCandidates:         warmupCandidates,
		WarmupSkippedBanned:      warmupSkippedBanned,
		WarmupSkippedStale:       warmupSkippedStale,
		WarmupSkippedIneligible:  warmupSkippedIneligible,
		WarmupSkippedNotNeeded:   warmupSkippedNotNeeded,
		WarmupAuthSource:         warmupAuthSource,
		WarmupAuthFilesSeen:      warmupAuthFilesSeen,
		WarmupAuthEligible:       warmupAuthEligible,
		WarmupAuthRejected:       warmupAuthRejected,
		WarmupAuthLastError:      warmupAuthLastError,
		KeeperRefreshTargets:     keeperRefreshTargets,
		KeeperRefreshRequests:    keeperRefreshRequests,
		KeeperRefreshAttempt:     keeperRefreshAttempt,
		KeeperRefreshAccepted:    keeperRefreshAccepted,
		KeeperRefreshSkipped:     keeperRefreshSkipped,
		KeeperRefreshRejected:    keeperRefreshRejected,
		KeeperRefreshError:       keeperRefreshLastError,
		KeeperRefreshRecoveries:  keeperRefreshRecoveries,
		BanResetPending:          banResetPending,
		BanResetEvents:           banResetEvents,
		BanExternalClears:        banExternalClears,
		LastBanClearReason:       lastBanClearReason,
		Quarantine:               quarantine,
		LastError:                lastError,
		Snapshots:                snapshots,
	}
	if !generation.ClaimedAt.IsZero() {
		out.GenerationClaimedAt = generation.ClaimedAt.Format(time.RFC3339)
	}
	if !warmupAuthCheckedAt.IsZero() {
		out.WarmupAuthCheckedAt = warmupAuthCheckedAt.Format(time.RFC3339)
	}
	if !keeperRefreshRequestedAt.IsZero() {
		out.KeeperRefreshRequestedAt = keeperRefreshRequestedAt.Format(time.RFC3339)
	}
	if !keeperRefreshNextAt.IsZero() {
		out.KeeperRefreshNextAt = keeperRefreshNextAt.Format(time.RFC3339)
	}
	if !serialSelectedAt.IsZero() {
		if out.SerialManualSelection {
			out.SerialManualActiveAuthID = strings.TrimSpace(serialActiveAuthID)
		}
		out.SerialSelectedAt = serialSelectedAt.Format(time.RFC3339)
	}
	if !serialMissingSince.IsZero() {
		out.SerialMissingSince = serialMissingSince.Format(time.RFC3339)
	}
	if !serialLastSwitchAt.IsZero() {
		out.SerialLastSwitchAt = serialLastSwitchAt.Format(time.RFC3339)
	}
	if !lastBanClearAt.IsZero() {
		out.LastBanClearAt = lastBanClearAt.Format(time.RFC3339)
	}
	for index := len(decisions) - 1; index >= 0; index-- {
		out.RecentDecisions = append(out.RecentDecisions, decisions[index])
	}
	for _, entry := range warmups {
		state := "attempted"
		if entry.Blocked {
			state = "blocked"
		} else if entry.Error != "" {
			state = "failed"
		} else if !entry.ActivatedAt.IsZero() && !entry.ResetAt.IsZero() {
			state = "confirmed"
		} else if !entry.CompletedAt.IsZero() {
			state = "pending_confirmation"
		}
		item := runtimeWarmupStatus{AuthID: entry.AuthID, Window: entry.Window, State: state, Status: entry.Status, Error: entry.Error, Blocked: entry.Blocked}
		if !entry.AttemptedAt.IsZero() {
			item.AttemptedAt = entry.AttemptedAt.Format(time.RFC3339)
		}
		if !entry.CompletedAt.IsZero() {
			item.CompletedAt = entry.CompletedAt.Format(time.RFC3339)
		}
		if !entry.ActivatedAt.IsZero() {
			item.ActivatedAt = entry.ActivatedAt.Format(time.RFC3339)
		}
		if !entry.ResetAt.IsZero() {
			item.ResetAt = entry.ResetAt.Format(time.RFC3339)
		}
		if !entry.SuppressUntil.IsZero() {
			item.SuppressUntil = entry.SuppressUntil.Format(time.RFC3339)
		}
		out.Warmups = append(out.Warmups, item)
	}
	sort.Slice(out.Warmups, func(i, j int) bool {
		if out.Warmups[i].AuthID != out.Warmups[j].AuthID {
			return out.Warmups[i].AuthID < out.Warmups[j].AuthID
		}
		return out.Warmups[i].Window < out.Warmups[j].Window
	})
	if !lastRefresh.IsZero() {
		out.LastRefresh = lastRefresh.Format(time.RFC3339)
	}
	return out
}

func (s *schedulerRuntimeState) persistAfterBanChange() {
	s.persistBanState()
}

// persistProbeAdmission is the cross-DSO admission fence for a half-open
// request. With a durable state_path, the exact reservation must be committed
// while this DSO still owns the generation before any request is dispatched or
// returned to CPA. Failure rolls back only the reservation we just created.
func (s *schedulerRuntimeState) persistProbeAdmission(authID string, bannedAt, probeStartedAt time.Time) bool {
	if s.runtimeStatePath() == "" {
		return s.generationOwnerActive()
	}
	if s.persistBanState() {
		return true
	}
	rolledBack := banStore.rollbackProbe(authID, bannedAt, probeStartedAt)
	slog.Warn("codex-quota-scheduler: half-open probe cancelled because the generation fence was not committed",
		"auth_id", strings.TrimSpace(authID),
		"rolled_back", rolledBack)
	return false
}

func (s *schedulerRuntimeState) banDurations() (fallback, max time.Duration) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fallback, max = s.cfg.FallbackBan, s.cfg.MaxBan
	if fallback <= 0 {
		fallback = 15 * time.Minute
	}
	if max <= 0 {
		max = 24 * time.Hour
	}
	return fallback, max
}

func (s *schedulerRuntimeState) halfOpenRetryAfter() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	retryAfter := s.cfg.HalfOpenRetryAfter
	if retryAfter <= 0 {
		retryAfter = 2 * time.Minute
	}
	return retryAfter
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// quotaPollState is per credential, so changes elsewhere in the pool cannot
// reset its backoff. It contains no upstream credentials.
type quotaPollState struct {
	AuthIndex   string
	AttemptedAt time.Time
	NextAt      time.Time
	Failures    int
	Error       string
}

const quotaRefreshRequestTimeout = 10 * time.Second

// One slow management request must not turn a batch of eight accounts into
// several minutes without an active-account observation. Inventory and quota
// calls share a single tick-sized budget; individual calls have a shorter cap.
func quotaRefreshBudget(interval time.Duration) time.Duration {
	if interval < time.Second {
		return time.Second
	}
	if interval > 30*time.Second {
		return 30 * time.Second
	}
	return interval
}

type nativeQuotaWindow struct {
	UsedPercent *float64        `json:"used_percent"`
	Seconds     int64           `json:"limit_window_seconds"`
	ResetAt     json.RawMessage `json:"reset_at"`
	ResetAfter  *int64          `json:"reset_after_seconds"`
}
type nativeRateLimit struct {
	Allowed      *bool              `json:"allowed"`
	LimitReached *bool              `json:"limit_reached"`
	Primary      *nativeQuotaWindow `json:"primary_window"`
	Secondary    *nativeQuotaWindow `json:"secondary_window"`
}

// parseNativeQuota accepts the native wham/usage schema. ObservedAt is captured
// before dispatch, never advanced when a cached snapshot is read.
func parseNativeQuota(raw []byte, authID, index string, observedAt time.Time) (quotaSnapshot, error) {
	var response struct {
		RateLimit  *nativeRateLimit `json:"rate_limit"`
		Additional []struct {
			RateLimit *nativeRateLimit `json:"rate_limit"`
		} `json:"additional_rate_limits"`
		ResetCredits *int `json:"rate_limit_reset_credits_available_count"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return quotaSnapshot{}, errors.New("invalid_quota_json")
	}
	if response.RateLimit == nil {
		return quotaSnapshot{}, errors.New("missing_rate_limit")
	}
	out := quotaSnapshot{AuthID: authID, AuthIndex: index, RefreshedAt: observedAt}
	if response.ResetCredits != nil && *response.ResetCredits >= 0 {
		out.ResetCredits = *response.ResetCredits
	}
	// Additional limits can be feature/model scoped. Do not treat those as
	// account-wide limits until CPA supplies a matching scope in scheduling.
	limit := response.RateLimit
	for _, w := range []*nativeQuotaWindow{limit.Primary, limit.Secondary} {
		if w == nil {
			continue
		}
		if w.UsedPercent == nil || math.IsNaN(*w.UsedPercent) || math.IsInf(*w.UsedPercent, 0) || *w.UsedPercent < 0 || *w.UsedPercent > 100 || w.Seconds <= 0 || w.Seconds > 366*24*3600 || (w.ResetAfter != nil && (*w.ResetAfter < 0 || *w.ResetAfter > 366*24*3600)) {
			return quotaSnapshot{}, errors.New("invalid_quota_window")
		}
		class := windowClassFromSeconds(w.Seconds)
		if class == "" {
			class = "unknown"
		}
		reset := parseQuotaTime(w.ResetAt)
		if reset.IsZero() && w.ResetAfter != nil && *w.ResetAfter > 0 {
			reset = observedAt.Add(time.Duration(*w.ResetAfter) * time.Second)
		}
		allowed := true
		if limit.Allowed != nil {
			allowed = *limit.Allowed
		}
		reached := *w.UsedPercent >= usedPercentThreshold
		// A top-level rejection with no exhausted percentage must still block.
		if limit.LimitReached != nil && *limit.LimitReached {
			reached = true
		}
		window := quotaWindow{Class: class, WindowSeconds: w.Seconds, UsedPercent: *w.UsedPercent, Allowed: allowed, LimitReached: reached, ResetAt: reset, ObservedAt: observedAt, Source: quotaSourceProbe}
		if w.ResetAfter != nil && *w.ResetAfter >= 0 {
			window.ResetAfterSeconds = *w.ResetAfter
			window.ResetAfterSecondsKnown = true
		}
		out.Windows = append(out.Windows, window)
	}
	if len(out.Windows) == 0 {
		return quotaSnapshot{}, errors.New("missing_quota_windows")
	}
	return out, nil
}

func quotaInventory(files []cpaAuthFileEntry) map[string]cpaAuthFileEntry {
	out := make(map[string]cpaAuthFileEntry)
	for _, f := range files {
		provider := strings.TrimSpace(f.Provider)
		if provider == "" {
			provider = strings.TrimSpace(f.Type)
		}
		// Unavailable/cooldown accounts must remain observable so reset recovery
		// does not depend on a successful generation request.
		if !strings.EqualFold(provider, providerCodex) || f.Disabled || strings.EqualFold(f.Status, "disabled") {
			continue
		}
		f.ID = strings.TrimSpace(f.ID)
		if f.ID == "" {
			f.ID = strings.TrimSpace(f.Name)
		}
		f.AuthIndex = strings.TrimSpace(f.AuthIndex)
		if f.ID != "" && f.AuthIndex != "" {
			out[f.ID] = f
		}
	}
	return out
}

func fetchCPAQuota(ctx context.Context, cfg pluginConfig, auth cpaAuthFileEntry) (cpaAPICallResponse, error) {
	var result cpaAPICallResponse
	key, err := os.ReadFile(cfg.CPAManagementKeyFile)
	if err != nil || strings.TrimSpace(string(key)) == "" {
		return result, errors.New("management_key_unavailable")
	}
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json", "User-Agent": "codex_cli_rs/cpa-quota-scheduler"}
	if auth.IDToken.AccountID != "" {
		headers["ChatGPT-Account-Id"] = auth.IDToken.AccountID
	}
	body, _ := json.Marshal(cpaAPICallRequest{AuthIndex: auth.AuthIndex, Method: http.MethodGet, URL: cfg.QuotaURL, Header: headers})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CPAManagementURL, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("invalid_management_url")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("management_request_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result.StatusCode = resp.StatusCode
		result.Header = resp.Header.Clone()
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, errors.New("invalid_management_response")
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("management_http_%d", resp.StatusCode)
	}
	if json.Unmarshal(raw, &result) != nil {
		return result, errors.New("invalid_management_json")
	}
	return result, nil
}

func quotaPollDelay(base time.Duration, failures int) time.Duration {
	if base < time.Second {
		base = 30 * time.Second
	}
	for n := 1; n < failures && base < 30*time.Minute; n++ {
		base *= 2
	}
	if base > 30*time.Minute {
		base = 30 * time.Minute
	}
	return base
}

func (s *schedulerRuntimeState) refreshOnce(ctx context.Context) {
	s.quotaRefreshMu.Lock()
	defer s.quotaRefreshMu.Unlock()
	if ctx.Err() != nil || !s.generationCanRefresh() {
		return
	}
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if !cfg.Enabled {
		return
	}
	batchCtx, cancelBatch := context.WithTimeout(ctx, quotaRefreshBudget(cfg.RefreshInterval))
	defer cancelBatch()
	inventoryCtx, cancelInventory := context.WithTimeout(batchCtx, quotaRefreshRequestTimeout)
	files, err := cpaManagementAuthFiles(inventoryCtx, cfg)
	if inventoryCtx.Err() != nil {
		err = inventoryCtx.Err()
	}
	cancelInventory()
	if err != nil {
		reason := "cpa_inventory_unavailable"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "cpa_inventory_timeout"
		} else if errors.Is(err, context.Canceled) {
			reason = "cpa_inventory_canceled"
		}
		s.recordRefreshError(errors.New(reason))
		return
	}
	inventory := quotaInventory(files)
	// A valid empty inventory is authoritative. Retain bans/history, but remove
	// quota aliases and polling work for deleted/disabled credentials.
	s.mu.Lock()
	if s.quotaPolls == nil {
		s.quotaPolls = make(map[string]quotaPollState)
	}
	next := make(map[string]quotaSnapshot)
	identities := make(map[string]string)
	for id, auth := range inventory {
		identities[auth.AuthIndex] = id
		if q, ok := s.quotas[id]; ok && q.AuthIndex == auth.AuthIndex {
			next[id] = q
			next[auth.AuthIndex] = q
		}
		if p := s.quotaPolls[id]; p.AuthIndex != auth.AuthIndex {
			s.quotaRunway.ForgetAuth(id)
			delete(s.quotaNative, id)
			s.resetSerialWeeklyEvidenceForAuthLocked(id)
			delete(s.quotaPolls, id)
		}
	}
	for id := range s.quotaPolls {
		if _, ok := inventory[id]; !ok {
			s.quotaRunway.ForgetAuth(id)
			delete(s.quotaNative, id)
			s.resetSerialWeeklyEvidenceForAuthLocked(id)
			delete(s.quotaPolls, id)
		}
	}
	s.quotas = next
	s.identities = identities
	activeID := s.serialActiveAuthID
	s.mu.Unlock()
	// Quota polling is read-only; generation ownership is claimed only after a
	// valid inventory, keeping model warmups fenced by the existing owner lease.
	if _, active, claimErr := s.claimGenerationAfterSuccessfulRefresh(); claimErr != nil || !active {
		if claimErr != nil {
			s.recordRefreshError(errors.New("generation_claim_failed"))
		}
		return
	}
	ids := make([]string, 0, len(inventory))
	for id := range inventory {
		ids = append(ids, id)
	}
	s.mu.RLock()
	sort.Slice(ids, func(i, j int) bool {
		if (ids[i] == activeID) != (ids[j] == activeID) {
			return ids[i] == activeID
		}
		a, b := s.quotaPolls[ids[i]].AttemptedAt, s.quotaPolls[ids[j]].AttemptedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return ids[i] < ids[j]
	})
	s.mu.RUnlock()
	attempted := 0
	lastErr := ""
	for _, id := range ids {
		if attempted >= cfg.QuotaRefreshBatch || batchCtx.Err() != nil || !s.generationOwnerActive() {
			break
		}
		auth := inventory[id]
		now := time.Now()
		s.mu.Lock()
		poll := s.quotaPolls[id]
		if now.Before(poll.NextAt) {
			s.mu.Unlock()
			continue
		}
		poll.AuthIndex = auth.AuthIndex
		poll.AttemptedAt = now
		interval := cfg.QuotaRefreshCooldown
		if id == activeID {
			interval = cfg.RefreshInterval
		}
		poll.NextAt = now.Add(interval)
		// A reset timestamp schedules an observation; it never proves renewed quota.
		for _, window := range s.quotas[id].Windows {
			resetProbe := window.ResetAt.Add(2 * time.Second)
			if window.ResetAt.After(now) && resetProbe.Before(poll.NextAt) {
				poll.NextAt = resetProbe
			}
		}
		s.quotaPolls[id] = poll
		s.quotaRefreshRequests++
		s.quotaRefreshRequestedAt = now
		s.mu.Unlock()
		s.persistBanState()
		attempted++
		requestCtx, cancelRequest := context.WithTimeout(batchCtx, quotaRefreshRequestTimeout)
		response, fetchErr := fetchCPAQuota(requestCtx, cfg, auth)
		var snapshot quotaSnapshot
		if fetchErr == nil {
			if response.StatusCode == http.StatusOK {
				snapshot, fetchErr = parseNativeQuota([]byte(response.Body), id, auth.AuthIndex, now)
			} else {
				fetchErr = fmt.Errorf("quota_http_%d", response.StatusCode)
			}
		}
		// Capture context failure before cancelRequest itself changes Err(). A
		// caller cancellation or a body-read deadline cannot count as a fresh
		// successful observation even if response headers arrived previously.
		if requestCtx.Err() != nil {
			fetchErr = requestCtx.Err()
		}
		cancelRequest()
		if !s.generationOwnerActive() {
			return
		}
		s.mu.Lock()
		poll = s.quotaPolls[id]
		if fetchErr != nil {
			s.quotaRunway.ForgetAuth(id)
			delete(s.quotaNative, id)
			s.resetSerialWeeklyEvidenceForAuthLocked(id)
			poll.Failures++
			poll.Error = fetchErr.Error()
			lastErr = poll.Error
			delay := quotaPollDelay(interval, poll.Failures)
			if response.StatusCode == 401 || response.StatusCode == 403 {
				delay = 30 * time.Minute
			}
			if retry := retryAfterDuration(http.Header(response.Header), time.Now()); retry > delay {
				delay = retry
			}
			if retryAt := time.Now().Add(delay); retryAt.After(poll.NextAt) {
				poll.NextAt = retryAt
			}
		} else {
			poll.Failures = 0
			poll.Error = ""
			s.quotaRunway.Observe(snapshot, time.Now())
			if s.quotaNative == nil {
				s.quotaNative = make(map[string]quotaSnapshot)
			}
			s.quotaNative[id] = snapshot
			snapshot = mergePartialQuotaSnapshot(s.quotas[id], snapshot, time.Now(), cfg.StaleAfter)
			s.quotas[id] = snapshot
			s.quotas[auth.AuthIndex] = snapshot
			s.updateCalibrationsLocked(map[string]quotaSnapshot{id: snapshot}, now)
		}
		s.quotaPolls[id] = poll
		s.mu.Unlock()
		if batchCtx.Err() != nil {
			break
		}
	}
	s.mu.Lock()
	// Retain an account failure while its retry is deferred; a successful
	// inventory read is not evidence that its quota query recovered.
	if lastErr == "" {
		for _, id := range ids {
			if p := s.quotaPolls[id]; p.Error != "" {
				lastErr = p.Error
				break
			}
		}
	}
	snapshotCopy := make(map[string]quotaSnapshot, len(s.quotas))
	for k, v := range s.quotas {
		snapshotCopy[k] = v
	}
	s.lastRefresh = time.Now()
	s.refreshes++
	s.lastError = lastErr
	s.quotaRefreshTargetsLast = len(ids)
	s.quotaRefreshLastError = lastErr
	s.mu.Unlock()
	if s.confirmPendingWarmups(snapshotCopy, time.Now()) {
		s.persistBanState()
	}
	s.reconcileExternallyResetQuotaBans(snapshotCopy, time.Now())
	s.persistBanState()
	if batchCtx.Err() == nil {
		s.scheduleWarmup(ctx, nil)
	}
}

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const resetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
const resetCreditsCacheTTL = 15 * time.Minute

type resetCreditCounts struct {
	Count, Applicable           int
	CountKnown, ApplicableKnown bool
}

// Optional reset metadata must never invalidate usable quota windows. Missing,
// malformed and explicit zero counts remain different observations.
func resetCreditCount(raw json.RawMessage) (int, bool) {
	var n int
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 || n > 1000000 {
		return 0, false
	}
	return n, true
}

func parseResetCreditCounts(raw json.RawMessage) resetCreditCounts {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return resetCreditCounts{}
	}
	read := func(snake, camel string) (int, bool) {
		if value, present := fields[snake]; present {
			return resetCreditCount(value)
		}
		return resetCreditCount(fields[camel])
	}
	n, known := read("available_count", "availableCount")
	a, applicableKnown := read("applicable_available_count", "applicableAvailableCount")
	return resetCreditCounts{n, a, known, applicableKnown}
}

// IDs are hashed immediately: consumption selects a credit on the upstream
// side and accepts only redeem_request_id, not a client-selected credit ID.
type resetCredit struct {
	Key       string    `json:"key"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Supported bool      `json:"supported"`
}

type resetCreditState struct {
	AuthIndex    string
	Count        int
	CountKnown   bool
	Complete     bool
	Credits      []resetCredit
	ObservedAt   time.Time
	NextAt       time.Time
	Failures     int
	Error        string
	AutoReason   string
	LastAction   string
	ActionAt     time.Time
	AutoFailures int
}

func resetCreditHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func parseResetCreditList(raw []byte, authIndex string, observedAt time.Time) (resetCreditState, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return resetCreditState{}, errors.New("invalid_reset_credit_list")
	}
	var rows []json.RawMessage
	if len(fields["credits"]) == 0 || string(fields["credits"]) == "null" || json.Unmarshal(fields["credits"], &rows) != nil || len(rows) > 256 {
		return resetCreditState{}, errors.New("invalid_reset_credit_list")
	}
	counts := parseResetCreditCounts(raw)
	out := resetCreditState{AuthIndex: authIndex, Count: counts.Count, CountKnown: counts.CountKnown, Complete: true, ObservedAt: observedAt}
	seen := make(map[string]bool)
	for _, row := range rows {
		var c struct {
			ID        string `json:"id"`
			Type      string `json:"reset_type"`
			Status    string `json:"status"`
			GrantedAt string `json:"granted_at"`
			ExpiresAt string `json:"expires_at"`
			Supported *bool  `json:"is_supported_by_plan"`
		}
		if json.Unmarshal(row, &c) != nil {
			out.Complete = false
			continue
		}
		if c.Type != "codex_rate_limits" || c.Status != "available" {
			continue
		}
		expires, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil || strings.TrimSpace(c.ID) == "" || seen[c.ID] {
			out.Complete = false
			continue
		}
		seen[c.ID] = true
		granted, _ := time.Parse(time.RFC3339Nano, c.GrantedAt)
		out.Credits = append(out.Credits, resetCredit{resetCreditHash(c.ID), granted, expires, c.Supported != nil && *c.Supported})
	}
	sort.Slice(out.Credits, func(i, j int) bool {
		if !out.Credits[i].ExpiresAt.Equal(out.Credits[j].ExpiresAt) {
			return out.Credits[i].ExpiresAt.Before(out.Credits[j].ExpiresAt)
		}
		return out.Credits[i].Key < out.Credits[j].Key
	})
	if out.CountKnown && out.Count != len(out.Credits) {
		out.Complete = false // Never guess a missing card's expiration.
	}
	return out, nil
}

func cloneResetCreditStates(source map[string]resetCreditState) map[string]resetCreditState {
	out := make(map[string]resetCreditState, len(source))
	for id, state := range source {
		state.Credits = append([]resetCredit(nil), state.Credits...)
		out[id] = state
	}
	return out
}

func resetCreditDue(state resetCreditState, now time.Time, before time.Duration) (resetCredit, string) {
	if !state.Complete || !state.CountKnown {
		return resetCredit{}, "details_incomplete"
	}
	for _, credit := range state.Credits {
		if !credit.ExpiresAt.After(now) {
			continue
		}
		// The upstream selects which card to consume. Never bypass an earlier
		// card of unknown support to try consuming a later one.
		if !credit.Supported {
			return resetCredit{}, "plan_unsupported"
		}
		if credit.ExpiresAt.After(now.Add(before)) {
			return resetCredit{}, "scheduled"
		}
		if credit.ExpiresAt.Sub(now) <= 5*time.Second {
			return resetCredit{}, "too_late"
		}
		return credit, "due"
	}
	return resetCredit{}, "no_available_credits"
}

func resetCreditsNextRead(state resetCreditState, cfg pluginConfig, now time.Time) time.Time {
	next := now.Add(resetCreditsCacheTTL)
	if cfg.ResetCreditsAutoUse {
		for _, c := range state.Credits {
			if !c.ExpiresAt.After(now) {
				continue
			}
			due := c.ExpiresAt.Add(-cfg.ResetCreditsAutoUseBefore)
			if !due.After(now) {
				due = now.Add(30 * time.Second)
			}
			if due.Before(next) {
				next = due
			}
		}
	}
	return next
}

func fetchResetCreditList(ctx context.Context, cfg pluginConfig, auth cpaAuthFileEntry) (resetCreditState, cpaAPICallResponse, error) {
	observed := time.Now()
	response, err := fetchCPANative(ctx, cfg, auth, http.MethodGet, resetCreditsURL, "", map[string]string{
		"OpenAI-Beta": "codex-1", "Originator": "Codex Desktop",
	})
	if err == nil && response.StatusCode != http.StatusOK {
		err = fmt.Errorf("reset_credits_http_%d", response.StatusCode)
	}
	if err != nil {
		return resetCreditState{}, response, err
	}
	state, err := parseResetCreditList([]byte(response.Body), auth.AuthIndex, observed)
	return state, response, err
}

// This worker shares the existing lifecycle and cancellation context. Default
// operation only reads lists (at most once per credential per 15 minutes).
// Explicit auto-use remains active during idle periods without generating text.
func (s *schedulerRuntimeState) refreshResetCredits(ctx context.Context) {
	if !s.resetCreditMu.TryLock() {
		return
	}
	defer s.resetCreditMu.Unlock()
	if ctx.Err() != nil || !s.generationOwnerActive() {
		return
	}
	s.mu.RLock()
	cfg, generation := s.cfg, s.configGeneration
	inventory := make(map[string]cpaAuthFileEntry, len(s.resetCreditInventory))
	for id, auth := range s.resetCreditInventory {
		inventory[id] = auth
	}
	states := cloneResetCreditStates(s.resetCredits)
	s.mu.RUnlock()
	// Custom quota adapters do not establish compatibility with this official
	// write API. Do not send their credentials to an unrelated destination.
	if !cfg.Enabled || cfg.QuotaURL != defaultPluginConfig().QuotaURL {
		return
	}
	ids := make([]string, 0, len(inventory))
	for id := range inventory {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := states[ids[i]], states[ids[j]]
		if !a.NextAt.Equal(b.NextAt) {
			return a.NextAt.Before(b.NextAt)
		}
		return ids[i] < ids[j]
	})
	batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	attempted := 0
	for _, id := range ids {
		if attempted >= 4 || batchCtx.Err() != nil || !s.generationOwnerActive() {
			break
		}
		auth, previous := inventory[id], states[id]
		now := time.Now()
		if previous.AuthIndex == auth.AuthIndex && now.Before(previous.NextAt) {
			continue
		}
		attempted++
		readCtx, cancelRead := context.WithTimeout(batchCtx, quotaRefreshRequestTimeout)
		state, response, err := fetchResetCreditList(readCtx, cfg, auth)
		if readCtx.Err() != nil {
			err = readCtx.Err()
		}
		cancelRead()
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			state = previous
			state.AuthIndex, state.Error = auth.AuthIndex, err.Error()
			state.Failures++
			delay := quotaPollDelay(time.Minute, state.Failures)
			if response.StatusCode == 401 || response.StatusCode == 403 {
				delay = 30 * time.Minute
			}
			if retry := retryAfterDuration(http.Header(response.Header), time.Now()); retry > delay {
				delay = retry
			}
			state.NextAt = time.Now().Add(delay)
		} else {
			state.LastAction, state.ActionAt = previous.LastAction, previous.ActionAt
			state.AutoFailures = previous.AutoFailures
			state.NextAt = resetCreditsNextRead(state, cfg, time.Now())
			blocked := false
			if cfg.ResetCreditsAutoUse || state.LastAction != "" {
				state, blocked = s.reconcileResetCreditAttempt(cfg, auth, state)
			}
			if cfg.ResetCreditsAutoUse && !blocked {
				state = s.maybeAutoUseResetCredit(batchCtx, cfg, generation, auth, state)
			} else if !cfg.ResetCreditsAutoUse {
				state.AutoReason = "disabled"
			}
		}
		if !s.generationOwnerActive() {
			return
		}
		s.mu.Lock()
		if s.resetCredits == nil {
			s.resetCredits = make(map[string]resetCreditState)
		}
		if current, ok := s.resetCreditInventory[id]; ok && current.AuthIndex == auth.AuthIndex {
			s.resetCredits[id] = state
			// Publish list counts to the same official count used for scheduling.
			if q, ok := s.quotas[id]; ok && q.AuthIndex == auth.AuthIndex && err == nil && state.CountKnown && !q.ResetCreditsObservedAt.After(state.ObservedAt) {
				q.ResetCredits, q.ResetCreditsKnown, q.ResetCreditsObservedAt = state.Count, true, state.ObservedAt
				s.quotas[id], s.quotas[auth.AuthIndex] = q, q
			}
		}
		s.mu.Unlock()
		s.persistBanState()
	}
}

// The journal is independent of the normal cached state, so a hot reload cannot
// restore an older pre-dispatch snapshot and consume a second credit. Every
// read/modify/fsync/rename is fenced by the cross-DSO generation lock.
type resetCreditAttempt struct {
	RequestID string    `json:"request_id"`
	Account   string    `json:"account"`
	Credit    string    `json:"credit"`
	Before    int       `json:"before"`
	At        time.Time `json:"at"`
	ExpiresAt time.Time `json:"expires_at"`
	Phase     string    `json:"phase"`
}

func (s *schedulerRuntimeState) changeResetCreditJournal(cfg pluginConfig, change func(map[string]resetCreditAttempt) error) error {
	if strings.TrimSpace(cfg.StatePath) == "" {
		return errors.New("durable_state_required")
	}
	committed, err := s.withGenerationOwnerCommit(func() error {
		path := cfg.StatePath + ".reset-credits.json"
		entries := make(map[string]resetCreditAttempt)
		raw, err := os.ReadFile(path)
		if err == nil {
			if len(raw) > 2<<20 || json.Unmarshal(raw, &entries) != nil || entries == nil {
				return errors.New("reset_credit_journal_invalid")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("reset_credit_journal_unavailable")
		}
		if err := change(entries); err != nil {
			return err
		}
		if len(entries) > 4096 {
			return errors.New("reset_credit_journal_full")
		}
		raw, err = json.Marshal(entries)
		if err != nil {
			return errors.New("reset_credit_journal_invalid")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return errors.New("reset_credit_journal_unavailable")
		}
		file, err := os.CreateTemp(filepath.Dir(path), ".reset-credits-*.tmp")
		if err != nil {
			return errors.New("reset_credit_journal_unavailable")
		}
		defer os.Remove(file.Name())
		defer file.Close()
		if file.Chmod(0600) != nil || writeFileAll(file, raw) != nil || file.Sync() != nil || file.Close() != nil || os.Rename(file.Name(), path) != nil {
			return errors.New("reset_credit_journal_unavailable")
		}
		// Persist the rename before issuing a non-idempotent upstream operation.
		// Windows does not expose directory fsync through os.File.Sync.
		if runtime.GOOS != "windows" {
			dir, err := os.Open(filepath.Dir(path))
			if err != nil {
				return errors.New("reset_credit_journal_unavailable")
			}
			err = dir.Sync()
			_ = dir.Close()
			if err != nil {
				return errors.New("reset_credit_journal_unavailable")
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !committed {
		return errors.New("generation_inactive")
	}
	return nil
}

func resetCreditAccount(auth cpaAuthFileEntry) string {
	if auth.IDToken.AccountID != "" {
		return resetCreditHash("account:" + auth.IDToken.AccountID)
	}
	return resetCreditHash("index:" + auth.AuthIndex)
}

// Retry read verification after an acknowledged reset without ever replaying
// the write. A crash or timeout with no acknowledgement stays fail-closed.
func (s *schedulerRuntimeState) reconcileResetCreditAttempt(cfg pluginConfig, auth cpaAuthFileEntry, state resetCreditState) (resetCreditState, bool) {
	blocked := false
	err := s.changeResetCreditJournal(cfg, func(entries map[string]resetCreditAttempt) error {
		account := resetCreditAccount(auth)
		for key, prior := range entries {
			if prior.Account != account || prior.Phase == "verified" {
				continue
			}
			present := false
			for _, c := range state.Credits {
				present = present || c.Key == prior.Credit
			}
			if prior.Phase == "confirmed" && state.CountKnown && state.Complete && state.ObservedAt.After(prior.At) && state.Count < prior.Before && !present {
				prior.Phase = "verified"
				entries[key] = prior
				state.LastAction, state.ActionAt = "verified", prior.At
				continue
			}
			blocked = true
			state.LastAction, state.ActionAt = prior.Phase, prior.At
			state.AutoReason = "previous_attempt_requires_review"
			if prior.Phase == "confirmed" {
				state.AutoReason = "verification_pending"
				state.NextAt = time.Now().Add(time.Minute)
			}
		}
		return nil
	})
	if err != nil {
		state.AutoReason, blocked = err.Error(), true
	}
	return state, blocked
}

func (s *schedulerRuntimeState) resetAutoAuthorized(ctx context.Context, generation uint64) bool {
	if ctx.Err() != nil || !s.generationOwnerActive() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Enabled && s.cfg.ResetCreditsAutoUse && s.configGeneration == generation && !s.stopping && !s.detached
}

func (s *schedulerRuntimeState) maybeAutoUseResetCredit(ctx context.Context, cfg pluginConfig, generation uint64, auth cpaAuthFileEntry, state resetCreditState) resetCreditState {
	credit, reason := resetCreditDue(state, time.Now(), cfg.ResetCreditsAutoUseBefore)
	state.AutoReason = reason
	if reason != "due" || !s.resetAutoAuthorized(ctx, generation) {
		return state
	}
	// Refresh membership immediately before considering a write: a deleted,
	// disabled or replaced inventory entry cannot inherit a cached permission.
	files, err := cpaManagementAuthFiles(ctx, cfg)
	current, exists := quotaInventory(files)[auth.ID]
	if err != nil || !exists || current.AuthIndex != auth.AuthIndex || current.IDToken.AccountID != auth.IDToken.AccountID {
		state.AutoReason = "identity_unconfirmed"
		return state
	}
	readCtx, cancel := context.WithTimeout(ctx, quotaRefreshRequestTimeout)
	observedAt := time.Now()
	response, err := fetchCPAQuota(readCtx, cfg, current)
	var quota quotaSnapshot
	if err == nil && response.StatusCode == http.StatusOK {
		quota, err = parseNativeQuota([]byte(response.Body), auth.ID, auth.AuthIndex, observedAt)
	} else if err == nil {
		err = errors.New("quota_not_confirmed")
	}
	if readCtx.Err() != nil {
		err = readCtx.Err()
	}
	cancel()
	if err != nil {
		state.AutoReason = "quota_not_confirmed"
		state.AutoFailures++
		delay := quotaPollDelay(time.Minute, state.AutoFailures)
		if response.StatusCode == 401 || response.StatusCode == 403 {
			delay = 30 * time.Minute
		}
		if retry := retryAfterDuration(http.Header(response.Header), time.Now()); retry > delay {
			delay = retry
		}
		state.NextAt = time.Now().Add(delay)
		return state
	}
	state.AutoFailures = 0
	if !quota.ResetCreditsApplicableKnown || quota.ResetCreditsApplicable <= 0 {
		state.AutoReason = "not_applicable"
		state.NextAt = resetCreditEligibilityRetry(credit, time.Now())
		return state
	}
	used := false
	for _, window := range quota.Windows {
		used = used || window.UsedPercent > 0
	}
	if !used {
		state.AutoReason = "quota_unused"
		state.NextAt = resetCreditEligibilityRetry(credit, time.Now())
		return state
	}
	banBefore, hadBan := banStore.lookup(auth.ID)
	// Inventory and quota reads can take seconds. Recheck the exact earliest
	// card before reserving; a manual redemption must not redirect this write
	// to a later card that the user intended to keep.
	finalCtx, cancelFinal := context.WithTimeout(ctx, quotaRefreshRequestTimeout)
	final, _, finalErr := fetchResetCreditList(finalCtx, cfg, current)
	if finalCtx.Err() != nil {
		finalErr = finalCtx.Err()
	}
	cancelFinal()
	finalCredit, finalReason := resetCreditDue(final, time.Now(), cfg.ResetCreditsAutoUseBefore)
	if finalErr != nil || finalReason != "due" || finalCredit.Key != credit.Key || final.Count != state.Count {
		state.AutoReason = "details_changed"
		return state
	}
	state.ObservedAt = final.ObservedAt
	// Reserve BEFORE dispatch. No automatic POST retry, including an uncertain
	// response, reconfigure, crash or library replacement. The UUID is also
	// supplied to the native idempotency field used by CPA's own panel.
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		state.AutoReason = "request_id_unavailable"
		return state
	}
	id[6], id[8] = (id[6]&15)|64, (id[8]&63)|128
	requestID := fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16])
	account := resetCreditAccount(current)
	attempt := resetCreditAttempt{requestID, account, credit.Key, state.Count, time.Now(), credit.ExpiresAt, "pending"}
	err = s.changeResetCreditJournal(cfg, func(entries map[string]resetCreditAttempt) error {
		for key, prior := range entries {
			if time.Since(prior.At) > 90*24*time.Hour && prior.Phase == "verified" {
				delete(entries, key)
				continue
			}
			if prior.Credit == credit.Key || (prior.Account == account && (prior.Phase != "verified" || time.Since(prior.At) < time.Minute)) {
				return errors.New("previous_attempt_requires_review")
			}
		}
		entries[requestID] = attempt
		return nil
	})
	if err != nil {
		state.AutoReason = err.Error()
		return state
	}
	state.LastAction, state.ActionAt = "pending", attempt.At
	if !s.resetAutoAuthorized(ctx, generation) || !credit.ExpiresAt.After(time.Now().Add(5*time.Second)) {
		state.AutoReason, state.LastAction = "dispatch_canceled", "unknown"
		return state
	}
	data, _ := json.Marshal(map[string]string{"redeem_request_id": requestID})
	writeCtx, cancelWrite := context.WithTimeout(ctx, quotaRefreshRequestTimeout)
	response, err = fetchCPANative(writeCtx, cfg, current, http.MethodPost, resetCreditsURL+"/consume", string(data), map[string]string{"Content-Type": "application/json"})
	if writeCtx.Err() != nil {
		err = writeCtx.Err()
	}
	cancelWrite()
	var result struct {
		Code string `json:"code"`
	}
	confirmed := err == nil && response.StatusCode >= 200 && response.StatusCode < 300 && json.Unmarshal([]byte(response.Body), &result) == nil && (result.Code == "reset" || result.Code == "already_redeemed")
	attempt.Phase = "unknown"
	state.AutoReason = "previous_attempt_requires_review"
	if confirmed {
		attempt.Phase, state.AutoReason = "confirmed", "verification_pending"
	}
	state.LastAction = attempt.Phase
	_ = s.changeResetCreditJournal(cfg, func(entries map[string]resetCreditAttempt) error {
		entries[requestID] = attempt
		return nil
	})
	// An ambiguous write is never replayed. A fresh read may establish that a
	// reset occurred, but cannot prove which actor consumed a credit.
	if !confirmed || !s.resetAutoAuthorized(ctx, generation) {
		return state
	}
	verified, _, verifyErr := fetchResetCreditList(ctx, cfg, current)
	if verifyErr != nil || verified.Count >= state.Count || !verified.CountKnown {
		return state
	}
	stillPresent := false
	for _, c := range verified.Credits {
		stillPresent = stillPresent || c.Key == credit.Key
	}
	if stillPresent {
		return state
	}
	attempt.Phase = "verified"
	if err := s.changeResetCreditJournal(cfg, func(entries map[string]resetCreditAttempt) error { entries[requestID] = attempt; return nil }); err != nil {
		return state
	}
	verified.LastAction, verified.ActionAt, verified.AutoReason = "verified", attempt.At, "used"
	verified.NextAt = resetCreditsNextRead(verified, cfg, time.Now())
	s.requestImmediateQuotaProbe(auth.ID, time.Now())
	if !s.verifyResetCreditQuotaRecovery(ctx, cfg, generation, current, banBefore, hadBan, attempt.At, callHost) {
		verified.AutoReason = "recovery_pending"
	}
	return verified
}

func resetCreditEligibilityRetry(credit resetCredit, now time.Time) time.Time {
	next := now.Add(5 * time.Minute)
	if lastMinute := credit.ExpiresAt.Add(-time.Minute); lastMinute.Before(next) {
		next = lastMinute
	}
	if next.Before(now.Add(30 * time.Second)) {
		next = now.Add(30 * time.Second)
	}
	return next
}

// A confirmed redemption does not itself establish renewed quota. Re-query
// before clearing only this credential's pre-existing quota cooldown. Network,
// auth and newer concurrent bans retain their own recovery requirements.
func (s *schedulerRuntimeState) verifyResetCreditQuotaRecovery(ctx context.Context, cfg pluginConfig, generation uint64, auth cpaAuthFileEntry, before banEntry, hadBan bool, attemptAt time.Time, call authExpiryHostCall) bool {
	if !s.resetAutoAuthorized(ctx, generation) {
		return false
	}
	observedAt := time.Now()
	readCtx, cancel := context.WithTimeout(ctx, quotaRefreshRequestTimeout)
	defer cancel()
	response, err := fetchCPAQuota(readCtx, cfg, auth)
	if err != nil || response.StatusCode != http.StatusOK || readCtx.Err() != nil {
		return false
	}
	q, err := parseNativeQuota([]byte(response.Body), auth.ID, auth.AuthIndex, observedAt)
	if err != nil || !s.resetAutoAuthorized(ctx, generation) {
		return false
	}
	eval := quotaEvaluationWithOrder(q, time.Now(), cfg.WindowOrder)
	if !eval.Known || !eval.Eligible {
		return false
	}
	s.mu.Lock()
	if current, exists := s.resetCreditInventory[auth.ID]; !exists || current.AuthIndex != auth.AuthIndex {
		s.mu.Unlock()
		return false
	}
	snapshot := mergePartialQuotaSnapshot(s.quotas[auth.ID], q, time.Now(), cfg.StaleAfter)
	s.quotas[auth.ID], s.quotas[auth.AuthIndex] = snapshot, snapshot
	if s.quotaNative == nil {
		s.quotaNative = make(map[string]quotaSnapshot)
	}
	s.quotaNative[auth.ID] = q
	s.mu.Unlock()
	s.recovery.observe(q, time.Now(), cfg.StaleAfter)
	if !s.resetAutoAuthorized(ctx, generation) {
		return false
	}
	if current, exists := banStore.lookup(auth.ID); exists && (!hadBan || !sameQuotaBan(current, before) || normalizeBanEntry(current).Kind != banKindQuota) {
		return false
	}
	_, err = s.resetCPACredentialCooldown(ctx, auth.ID, call)
	if err != nil || !s.resetAutoAuthorized(ctx, generation) {
		return false
	}
	before = normalizeBanEntry(before)
	if hadBan && before.Kind == banKindQuota && before.Phase == banPhaseCooldown && before.BannedAt.Before(attemptAt) {
		banStore.mu.Lock()
		current, exists := banStore.bans[auth.ID]
		removed := exists && sameQuotaBan(current, before)
		if removed {
			delete(banStore.bans, auth.ID)
		}
		banStore.mu.Unlock()
		if removed {
			s.dropBanResetConfirmation(auth.ID)
		}
	}
	s.persistBanState()
	return true
}

type resetCreditPanelRow struct {
	GrantedAt string `json:"granted_at,omitempty"`
	ExpiresAt string `json:"expires_at"`
	Supported bool   `json:"supported"`
}

type resetCreditPanelStatus struct {
	Count           int                   `json:"count"`
	CountKnown      bool                  `json:"count_known"`
	Applicable      int                   `json:"applicable"`
	ApplicableKnown bool                  `json:"applicable_known"`
	ApplicableFresh bool                  `json:"applicable_fresh"`
	ListFresh       bool                  `json:"list_fresh"`
	Complete        bool                  `json:"complete"`
	ObservedAt      string                `json:"observed_at,omitempty"`
	Error           string                `json:"error,omitempty"`
	Credits         []resetCreditPanelRow `json:"credits"`
	AutoEnabled     bool                  `json:"auto_enabled"`
	AutoBefore      string                `json:"auto_before"`
	AutoReason      string                `json:"auto_reason"`
	LastAction      string                `json:"last_action,omitempty"`
	ActionAt        string                `json:"action_at,omitempty"`
}

func resetCreditStatus(state resetCreditState, q quotaSnapshot, cfg pluginConfig, now time.Time) resetCreditPanelStatus {
	out := resetCreditPanelStatus{
		Applicable: q.ResetCreditsApplicable, ApplicableKnown: q.ResetCreditsApplicableKnown,
		ApplicableFresh: q.ResetCreditsApplicableKnown && !q.ResetCreditsApplicableObservedAt.IsZero() && !now.Before(q.ResetCreditsApplicableObservedAt) && now.Sub(q.ResetCreditsApplicableObservedAt) <= cfg.StaleAfter,
		AutoEnabled:     cfg.ResetCreditsAutoUse, AutoBefore: cfg.ResetCreditsAutoUseBefore.String(), AutoReason: "disabled",
		Credits: []resetCreditPanelRow{},
	}
	if cfg.ResetCreditsAutoUse {
		out.AutoReason = "details_unconfirmed"
		if cfg.QuotaURL != defaultPluginConfig().QuotaURL {
			out.AutoReason = "adapter_unsupported"
		}
	}
	if state.AuthIndex != q.AuthIndex || state.ObservedAt.IsZero() {
		return out
	}
	out.Count, out.CountKnown, out.Complete = state.Count, state.CountKnown, state.Complete
	out.ListFresh = state.Error == "" && !now.Before(state.ObservedAt) && now.Sub(state.ObservedAt) <= resetCreditsCacheTTL
	if q.ResetCreditsKnown && q.ResetCreditsObservedAt.After(state.ObservedAt) && q.ResetCredits != state.Count {
		out.ListFresh = false
	}
	out.ObservedAt, out.Error = state.ObservedAt.Format(time.RFC3339), state.Error
	out.LastAction = state.LastAction
	if !state.ActionAt.IsZero() {
		out.ActionAt = state.ActionAt.Format(time.RFC3339)
	}
	if cfg.ResetCreditsAutoUse {
		out.AutoReason = state.AutoReason
	}
	for _, c := range state.Credits {
		row := resetCreditPanelRow{ExpiresAt: c.ExpiresAt.Format(time.RFC3339), Supported: c.Supported}
		if !c.GrantedAt.IsZero() {
			row.GrantedAt = c.GrantedAt.Format(time.RFC3339)
		}
		out.Credits = append(out.Credits, row)
	}
	return out
}

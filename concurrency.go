package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const concurrencyHeader = "X-CPA-Quota-Request"
const concurrencyRegistrationLimit = 8192

type accountConcurrencyError struct{ code, message string }

func (e *accountConcurrencyError) Error() string { return e.code + ": " + e.message }

// Slots have no age expiry: a long-running stream is still an upstream request.
// Only an exact CPA terminal event or a sequential retry releases its slot.
// This ledger deliberately survives configuration reloads and is independent
// of quota prediction/debt and the approximate balanced Pending list.
type concurrencyRequest struct {
	Token           string
	Key             string
	AuthID          string
	At              time.Time
	Warmup          bool
	NativeSelection bool
	Dispatched      bool
	Rejection       string
	QueueDeadline   time.Time
}
type accountConcurrencyGate struct {
	mu        sync.Mutex
	requests  map[string]*concurrencyRequest
	tokens    map[string]string
	aliases   map[string]string
	changed   chan struct{}
	supported bool
	waiting   int
	waits     uint64
	rejected  uint64
}
type accountConcurrencyAccountStatus struct {
	Active  int  `json:"active"`
	Warmup  int  `json:"warmup"`
	Limit   int  `json:"limit"`
	AtLimit bool `json:"at_limit"`
}
type accountConcurrencyStatus struct {
	Enabled       bool                                       `json:"enabled"`
	Supported     bool                                       `json:"lifecycle_observed"`
	MaxPerAccount int                                        `json:"max_per_account"`
	WaitSeconds   float64                                    `json:"wait_seconds"`
	Active        int                                        `json:"active"`
	Waiting       int                                        `json:"waiting"`
	Waits         uint64                                     `json:"waits"`
	Rejected      uint64                                     `json:"rejected"`
	Accounts      map[string]accountConcurrencyAccountStatus `json:"accounts"`
}

func (g *accountConcurrencyGate) initLocked() {
	if g.requests == nil {
		g.requests = make(map[string]*concurrencyRequest)
		g.tokens = make(map[string]string)
		g.aliases = make(map[string]string)
	}
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
}
func (g *accountConcurrencyGate) notifyLocked() {
	g.initLocked()
	close(g.changed)
	g.changed = make(chan struct{})
}
func (g *accountConcurrencyGate) notify() { g.mu.Lock(); defer g.mu.Unlock(); g.notifyLocked() }
func (g *accountConcurrencyGate) register(id string, now time.Time) string {
	if id == "" || len(id) > 512 {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	g.supported = true
	if r := g.requests[id]; r != nil {
		return r.Token
	}
	// Expire only requests which never obtained a slot. Active streams are never
	// discarded, even when CPA's terminal callback is delayed.
	for key, r := range g.requests {
		if r.Key == "" && now.Sub(r.At) > 30*time.Minute {
			delete(g.tokens, r.Token)
			delete(g.requests, key)
		}
	}
	if len(g.requests) >= concurrencyRegistrationLimit {
		return ""
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return ""
	}
	token := hex.EncodeToString(entropy[:])
	g.requests[id] = &concurrencyRequest{Token: token, At: now}
	g.tokens[token] = id
	return token
}
func (g *accountConcurrencyGate) release(id string) *concurrencyRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r := g.requests[id]; r != nil {
		delete(g.tokens, r.Token)
		delete(g.requests, id)
		g.notifyLocked()
		copy := *r
		return &copy
	}
	return nil
}

// Aliases are namespaced and merged when CPA supplies additional identity
// evidence. Learning account_id/index after admission must not reset a count.
func (g *accountConcurrencyGate) keyLocked(aliases []string) string {
	g.initLocked()
	key := ""
	for _, alias := range aliases {
		if existing := g.aliases[alias]; existing != "" && (key == "" || existing < key) {
			key = existing
		}
	}
	if key == "" && len(aliases) > 0 {
		key = aliases[0]
	}
	if key == "" {
		return ""
	}
	old := map[string]bool{}
	for _, alias := range aliases {
		if existing := g.aliases[alias]; existing != "" && existing != key {
			old[existing] = true
		}
	}
	if len(old) > 0 {
		for alias, existing := range g.aliases {
			if old[existing] {
				g.aliases[alias] = key
			}
		}
		for _, r := range g.requests {
			if old[r.Key] {
				r.Key = key
			}
		}
	}
	for _, alias := range aliases {
		g.aliases[alias] = key
	}
	return key
}
func (g *accountConcurrencyGate) countLocked(key string) (active, warmup int) {
	if key == "" {
		return
	}
	for _, r := range g.requests {
		if r.Key == key {
			active++
			if r.Warmup {
				warmup++
			}
		}
	}
	return
}
func (s *schedulerRuntimeState) concurrencyAliases(candidate pluginapi.SchedulerAuthCandidate) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
	canonical := s.canonicalAuthIDLocked(candidate.ID, candidateAuthIndex(candidate))
	out := []string{"auth:" + canonical}
	if candidate.ID != "" && candidate.ID != canonical {
		out = append(out, "auth:"+candidate.ID)
	}
	index := candidateAuthIndex(candidate)
	if ok && snapshot.AuthIndex != "" {
		index = snapshot.AuthIndex
	}
	if index != "" {
		out = append(out, "index:"+index)
	}
	// A Team account_id can identify a shared workspace. Scope it by CPA's
	// explicit user identity; never merge unrelated seats by workspace alone.
	user := extractMetadataString(candidate.Metadata, "chatgpt_user_id", "user_id", "email")
	if ok && snapshot.AccountID != "" && user != "" {
		out = append(out, "member:"+snapshot.AccountID+":"+strings.ToLower(user))
	}
	return out
}
func (s *schedulerRuntimeState) concurrencyPreferred(req pluginapi.SchedulerPickRequest, cfg pluginConfig, now time.Time) string {
	preferred := serialPinnedAuthID(req)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if now.Before(s.balancedClock) {
		now = s.balancedClock
	}
	if preferred == "" && normalizeSchedulerMode(cfg.SchedulerMode) == "balanced" && cfg.StickySeconds > 0 {
		binding, ok := s.balancedSessionLocked(schedulerSessionHash(req), now)
		if !ok {
			binding, ok = s.balancedSessionLocked(schedulerParentSessionHash(req), now)
		}
		if ok {
			preferred = binding.AuthID
		}
	} else if preferred == "" && normalizeSchedulerMode(cfg.SchedulerMode) == "serial" {
		preferred = s.serialActiveAuthID
	}
	for _, candidate := range req.Candidates {
		if candidate.ID != preferred {
			continue
		}
		snapshot, found := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
		choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
		stickyEligible := choice.Eligible || (normalizeSchedulerMode(cfg.SchedulerMode) == "balanced" && choice.Reason == "serial_threshold")
		if stickyEligible && banStore.schedulable(candidate.ID, now) && !s.adqProviderCircuitActiveLockedForConcurrency(candidate, now) {
			return preferred
		}
	}
	return ""
}
func (s *schedulerRuntimeState) adqProviderCircuitActiveLockedForConcurrency(candidate pluginapi.SchedulerAuthCandidate, now time.Time) bool {
	_, active := s.adqProviderCircuitActiveLocked(candidate.ID, candidateAuthIndex(candidate), now)
	return active
}

// Selection and slot admission share selectionMu with synthetic warmup. No
// network call or waiting holds it. Busy sticky conversations stay bound.
func (s *schedulerRuntimeState) schedulerPick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	if schedulerRequestGenerationDisabled(req) || len(req.Candidates) == 0 || !codexOnlySchedulerRequest(req) {
		// Preserve CPA's native mixed-provider routing. If it selects Codex,
		// after-auth admission uses CPA's selected_auth_id and the same ledger.
		token := schedulerHeader(req.Options, concurrencyHeader)
		s.concurrency.mu.Lock()
		if r := s.concurrency.requests[s.concurrency.tokens[token]]; r != nil {
			r.NativeSelection = !schedulerRequestGenerationDisabled(req)
		}
		s.concurrency.mu.Unlock()
		return s.schedulerPickUncapped(req)
	}
	s.mu.RLock()
	initialCfg := s.cfg
	s.mu.RUnlock()
	deadline := time.Now().Add(initialCfg.AccountConcurrencyWait)
	token := schedulerHeader(req.Options, concurrencyHeader)
	waiting := false
	defer func() {
		if waiting {
			s.concurrency.mu.Lock()
			s.concurrency.waiting--
			s.concurrency.mu.Unlock()
		}
	}()
	for {
		s.selectionMu.Lock()
		s.mu.RLock()
		cfg := s.cfg
		stopping := s.stopping
		s.mu.RUnlock()
		aliases := make(map[string][]string, len(req.Candidates))
		for _, candidate := range req.Candidates {
			aliases[candidate.ID] = s.concurrencyAliases(candidate)
		}
		preferred := s.concurrencyPreferred(req, cfg, time.Now())
		g := &s.concurrency
		g.mu.Lock()
		g.initLocked()
		id := g.tokens[token]
		request := g.requests[id]
		if request != nil && !request.QueueDeadline.IsZero() {
			deadline = request.QueueDeadline
		}
		enforce := cfg.Enabled && cfg.AccountConcurrencyEnabled && g.supported
		if enforce && (request == nil || stopping) {
			if request != nil {
				request.Rejection = "account_concurrency_unavailable"
			}
			g.rejected++
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{"account_concurrency_unavailable", "CPA request lifecycle correlation is unavailable"}
		}
		// A rejected lifecycle remains terminal across CPA's alternate credential
		// retries. Otherwise a busy sticky account disappears from Candidates,
		// allowing the same expired request to be silently assigned elsewhere.
		if enforce && request.Rejection != "" {
			code := request.Rejection
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{code, "bounded queue admission already rejected for this execution"}
		}
		// CPA retries attempts sequentially under the same execution RequestID.
		// Detach the old attempt before admitting its replacement; unrelated requests
		// and delayed/duplicate usage records cannot release this slot.
		if request != nil && request.Key != "" {
			request.Key = ""
			request.AuthID = ""
			request.Dispatched = false
			g.notifyLocked()
		}
		attempt := req
		attempt.Candidates = nil
		preferredBusy := false
		for _, candidate := range req.Candidates {
			key := g.keyLocked(aliases[candidate.ID])
			active, _ := g.countLocked(key)
			busy := enforce && active >= cfg.AccountMaxConcurrency
			if candidate.ID == preferred && busy {
				preferredBusy = true
			}
			if !busy {
				attempt.Candidates = append(attempt.Candidates, candidate)
			}
		}
		busy := enforce && (preferredBusy || len(attempt.Candidates) == 0)
		if busy {
			if request != nil && request.QueueDeadline.IsZero() {
				request.QueueDeadline = deadline
			}
			if !waiting {
				waiting = true
				g.waiting++
				g.waits++
			}
			wake := g.changed
			expired := !time.Now().Before(deadline)
			if expired {
				g.rejected++
				if request != nil {
					request.Rejection = "account_concurrency_busy"
				}
			}
			g.mu.Unlock()
			s.selectionMu.Unlock()
			if expired {
				return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{"account_concurrency_busy", "account is busy; bounded queue wait expired"}
			}
			// The native ABI does not expose caller cancellation to Pick. The exact
			// terminal callback removes the registration; a short tick also observes
			// configuration changes, without polling quota or sending upstream requests.
			timer := time.NewTimer(minDuration(time.Until(deadline), 100*time.Millisecond))
			select {
			case <-wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
			continue
		}
		g.mu.Unlock()
		response, err := s.schedulerPickUncapped(attempt)
		g.mu.Lock()
		if request != nil && response.Handled && response.AuthID != "" {
			// Warmup cannot race admission because it shares selectionMu. A terminal
			// cancellation can race selection, so verify the registration again.
			if g.requests[id] != request {
				err = &accountConcurrencyError{"account_concurrency_unavailable", "request ended before admission"}
				response = pluginapi.SchedulerPickResponse{}
			} else {
				request.Key = g.keyLocked(aliases[response.AuthID])
				request.AuthID = response.AuthID
			}
		}
		g.mu.Unlock()
		s.selectionMu.Unlock()
		if err != nil && request != nil {
			s.rollbackUndispatchedConcurrency(request.Token)
		}
		return response, err
	}
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
func concurrencyTermination(code, message string) pluginapi.RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "server_error", "code": code, "message": message}})
	return pluginapi.RequestInterceptResponse{Terminate: true, StatusCode: http.StatusServiceUnavailable, ResponseHeaders: http.Header{"Retry-After": []string{"1"}, "Content-Type": []string{"application/json"}}, ResponseBody: body, ClearHeaders: []string{concurrencyHeader}}
}
func (s *schedulerRuntimeState) concurrencyBefore(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	token := s.concurrency.register(strings.TrimSpace(req.RequestID), time.Now())
	response := pluginapi.RequestInterceptResponse{ClearHeaders: []string{concurrencyHeader}}
	if token != "" {
		response.Headers = make(http.Header)
		response.Headers.Set(concurrencyHeader, token)
	}
	return response
}
func (s *schedulerRuntimeState) concurrencyAfter(req pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
	response := pluginapi.RequestInterceptResponse{ClearHeaders: []string{concurrencyHeader}}
	isCodex := strings.EqualFold(strings.TrimSpace(req.ToFormat), "codex")
	selectedID := extractMetadataString(req.Metadata, "selected_auth_id")
	selectedIndex := extractMetadataString(req.Metadata, "selected_auth_index")
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	deadline := time.Now().Add(cfg.AccountConcurrencyWait)
	waiting := false
	defer func() {
		if waiting {
			s.concurrency.mu.Lock()
			s.concurrency.waiting--
			s.concurrency.mu.Unlock()
		}
	}()
	for {
		s.selectionMu.Lock()
		aliases := s.concurrencyAliases(pluginapi.SchedulerAuthCandidate{ID: selectedID, Attributes: map[string]string{"auth_index": selectedIndex}})
		s.mu.RLock()
		cfg = s.cfg
		s.mu.RUnlock()
		g := &s.concurrency
		g.mu.Lock()
		g.initLocked()
		r := g.requests[req.RequestID]
		if r != nil && !r.QueueDeadline.IsZero() {
			deadline = r.QueueDeadline
		}
		enabled := cfg.Enabled && cfg.AccountConcurrencyEnabled
		if !isCodex {
			if r != nil && r.Key != "" {
				r.Key = ""
				r.AuthID = ""
				r.Dispatched = false
				g.notifyLocked()
			}
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return response
		}
		if enabled && r != nil && r.Rejection != "" {
			code := r.Rejection
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return concurrencyTermination(code, "The selected Codex account is busy or unavailable; try again after an active request finishes")
		}
		// A mixed route is selected by CPA. Account it only after the provider
		// and credential are known, without overriding native provider priority.
		if r != nil && r.NativeSelection && selectedID != "" {
			key := g.keyLocked(aliases)
			if r.Key != key {
				r.Key = ""
				r.AuthID = ""
				g.notifyLocked()
			}
			active, _ := g.countLocked(key)
			if enabled && r.Key == "" && active >= cfg.AccountMaxConcurrency {
				if r.QueueDeadline.IsZero() {
					r.QueueDeadline = deadline
				}
				if !waiting {
					waiting = true
					g.waiting++
					g.waits++
				}
				wake := g.changed
				expired := !time.Now().Before(deadline)
				if expired {
					g.rejected++
				}
				g.mu.Unlock()
				s.selectionMu.Unlock()
				if expired {
					return concurrencyTermination("account_concurrency_busy", "The selected Codex account is busy; bounded queue wait expired")
				}
				timer := time.NewTimer(minDuration(time.Until(deadline), 100*time.Millisecond))
				select {
				case <-wake:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
				case <-timer.C:
				}
				continue
			}
			r.Key = key
			r.AuthID = selectedID
		}
		if enabled && (req.RequestID == "" || r == nil || r.Key == "" || (selectedID != "" && r.Key != g.keyLocked(aliases))) {
			g.rejected++
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return concurrencyTermination("account_concurrency_unavailable", "CPA did not admit this Codex execution through the account concurrency scheduler")
		}
		if r != nil {
			r.Dispatched = true
		}
		g.mu.Unlock()
		s.selectionMu.Unlock()
		return response
	}
}

// Undo only a pick that never reached after-auth execution. Usage reporter IDs
// are unrelated to CPA lifecycle IDs; completed upstream usage keeps its own
// existing telemetry reconciliation and is never freed by an arbitrary event.
func (s *schedulerRuntimeState) rollbackUndispatchedConcurrency(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.balancedAccounts {
		pending := account.Pending[:0]
		for _, p := range account.Pending {
			if p.RequestToken != token {
				pending = append(pending, p)
				continue
			}
			account.Credit += p.Cost
			if s.adqReservations != nil && p.ReservationID != "" {
				s.adqReservations.Release(p.ReservationID)
			}
		}
		account.Pending = pending
	}
}
func (s *schedulerRuntimeState) concurrencyComplete(id string) {
	if r := s.concurrency.release(id); r != nil && !r.Dispatched {
		s.rollbackUndispatchedConcurrency(r.Token)
	}
}

func (s *schedulerRuntimeState) beginWarmupConcurrency(candidate warmupCandidate, cfg pluginConfig) (string, bool) {
	aliases := s.concurrencyAliases(pluginapi.SchedulerAuthCandidate{ID: candidate.Snapshot.AuthID, Attributes: map[string]string{"auth_index": candidate.Snapshot.AuthIndex}})
	// Caller owns selectionMu. Slot allocation is before durable warmup admission
	// so a busy account consumes neither a daily attempt nor failure backoff.
	g := &s.concurrency
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	key := g.keyLocked(aliases)
	active, _ := g.countLocked(key)
	if cfg.Enabled && cfg.AccountConcurrencyEnabled && active >= cfg.AccountMaxConcurrency {
		return "", false
	}
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", false
	}
	id := "warmup:" + hex.EncodeToString(entropy[:])
	g.requests[id] = &concurrencyRequest{Key: key, AuthID: candidate.Snapshot.AuthID, At: time.Now(), Warmup: true}
	return id, true
}
func (s *schedulerRuntimeState) concurrencyStatus(cfg pluginConfig, quotas map[string]quotaSnapshot) accountConcurrencyStatus {
	g := &s.concurrency
	g.mu.Lock()
	defer g.mu.Unlock()
	g.initLocked()
	out := accountConcurrencyStatus{Enabled: cfg.Enabled && cfg.AccountConcurrencyEnabled, Supported: g.supported, MaxPerAccount: cfg.AccountMaxConcurrency, WaitSeconds: cfg.AccountConcurrencyWait.Seconds(), Waiting: g.waiting, Waits: g.waits, Rejected: g.rejected, Accounts: map[string]accountConcurrencyAccountStatus{}}
	for _, q := range quotas {
		if q.AuthID == "" {
			continue
		}
		aliases := []string{"auth:" + q.AuthID}
		if q.AuthIndex != "" {
			aliases = append(aliases, "index:"+q.AuthIndex)
		}
		key := g.keyLocked(aliases)
		active, warmup := g.countLocked(key)
		out.Accounts[q.AuthID] = accountConcurrencyAccountStatus{Active: active, Warmup: warmup, Limit: cfg.AccountMaxConcurrency, AtLimit: out.Enabled && active >= cfg.AccountMaxConcurrency}
	}
	for _, r := range g.requests {
		if r.Key != "" {
			out.Active++
		}
	}
	return out
}
func handleConcurrencyBefore(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(schedulerRuntime.concurrencyBefore(req))
}
func handleConcurrencyAfter(raw []byte) ([]byte, error) {
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(schedulerRuntime.concurrencyAfter(req))
}
func handleConcurrencyComplete(raw []byte) ([]byte, error) {
	var completion pluginapi.RequestCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, err
	}
	schedulerRuntime.concurrencyComplete(completion.RequestID)
	return okEnvelope(map[string]any{})
}

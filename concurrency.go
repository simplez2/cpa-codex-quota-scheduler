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
	SessionKey      string
	Token           string
	Key             string
	AuthID          string
	At              time.Time
	Warmup          bool
	NativeSelection bool
	Dispatched      bool
	Rejection       string
	QueueDeadline   time.Time
	QueueStartedAt  time.Time
	QueueSequence   uint64
	WaitKeys        map[string]bool
	WaitAuthID      string
	WaitReason      string
}
type accountConcurrencyGate struct {
	mu         sync.Mutex
	requests   map[string]*concurrencyRequest
	tokens     map[string]string
	aliases    map[string]string
	changed    chan struct{}
	supported  bool
	waiting    int
	waits      uint64
	rejected   uint64
	sequence   uint64
	rejections []accountConcurrencyRejection
}
type accountConcurrencyRejection struct {
	At            time.Time `json:"at"`
	Code          string    `json:"code"`
	Reason        string    `json:"reason"`
	AuthID        string    `json:"auth_id,omitempty"`
	Mode          string    `json:"mode"`
	Active        int       `json:"active"`
	Limit         int       `json:"limit"`
	WaitedSeconds float64   `json:"waited_seconds"`
}

type accountConcurrencyAccountStatus struct {
	Active            int     `json:"active"`
	Warmup            int     `json:"warmup"`
	Limit             int     `json:"limit"`
	AtLimit           bool    `json:"at_limit"`
	Waiting           int     `json:"waiting"`
	OldestWaitSeconds float64 `json:"oldest_wait_seconds"`
	CredentialKey     string  `json:"credential_key"`
}
type accountConcurrencyStatus struct {
	Enabled          bool                                       `json:"enabled"`
	Supported        bool                                       `json:"lifecycle_observed"`
	Scope            string                                     `json:"scope"`
	MaxPerCredential int                                        `json:"max_per_credential"`
	MaxPerAccount    int                                        `json:"max_per_account"`
	WaitSeconds      float64                                    `json:"wait_seconds"`
	Active           int                                        `json:"active"`
	Waiting          int                                        `json:"waiting"`
	Waits            uint64                                     `json:"waits"`
	PoolWaiting      int                                        `json:"pool_waiting"`
	QueuePolicy      string                                     `json:"queue_policy"`
	RecentRejections []accountConcurrencyRejection              `json:"recent_rejections"`
	Rejected         uint64                                     `json:"rejected"`
	Credentials      map[string]accountConcurrencyAccountStatus `json:"credentials"`
	Accounts         map[string]accountConcurrencyAccountStatus `json:"accounts"`
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
// evidence for the same credential. Learning its auth_index after admission
// must not reset a count. Account/workspace/user identity is never an alias.
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
			for oldKey := range old {
				if r.WaitKeys[oldKey] {
					delete(r.WaitKeys, oldKey)
					r.WaitKeys[key] = true
				}
			}
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

// Queue order is per credential. An older bound waiter has priority over new
// requests and warmup; unrelated credentials remain free to accept work.
// Pool waiters compete only for their candidate keys. Expired or cancelled
// registrations cannot reserve priority or release someone else's active slot.
func (g *accountConcurrencyGate) blockedLocked(key string, request *concurrencyRequest, limit int, now time.Time) bool {
	active, _ := g.countLocked(key)
	if active >= limit {
		return true
	}
	for _, older := range g.requests {
		if older == request || older.QueueSequence == 0 || !older.WaitKeys[key] || older.Rejection != "" {
			continue
		}
		if !older.QueueDeadline.IsZero() && !now.Before(older.QueueDeadline) {
			continue
		}
		if request == nil || request.QueueSequence == 0 || older.QueueSequence < request.QueueSequence {
			return true
		}
	}
	return false
}
func (g *accountConcurrencyGate) startWaitLocked(r *concurrencyRequest, keys map[string]bool, authID, reason string, now time.Time) {
	if r == nil {
		return
	}
	if r.QueueSequence == 0 {
		g.sequence++
		r.QueueSequence = g.sequence
		g.waits++
		if r.QueueStartedAt.IsZero() {
			r.QueueStartedAt = now
		}
	}
	r.WaitKeys, r.WaitAuthID, r.WaitReason = keys, authID, reason
}
func (g *accountConcurrencyGate) endWait(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.waiting--
	if r := g.requests[id]; r != nil && r.QueueSequence != 0 {
		r.QueueSequence = 0
		r.WaitKeys = nil
		g.notifyLocked()
	}
}
func (g *accountConcurrencyGate) rejectLocked(r *concurrencyRequest, code string, cfg pluginConfig) {
	if r != nil && r.Rejection != "" {
		return
	}
	g.rejected++
	event := accountConcurrencyRejection{At: time.Now(), Code: code, Reason: "lifecycle_unavailable", Mode: normalizeSchedulerMode(cfg.SchedulerMode), Limit: cfg.AccountMaxConcurrency}
	if r != nil {
		r.Rejection = code
		event.AuthID, event.Reason = r.WaitAuthID, r.WaitReason
		if event.AuthID == "" {
			event.AuthID = r.AuthID
		}
		if event.Reason == "" {
			event.Reason = "lifecycle_unavailable"
		}
		if !r.QueueStartedAt.IsZero() {
			event.WaitedSeconds = event.At.Sub(r.QueueStartedAt).Seconds()
			if event.WaitedSeconds < 0 {
				event.WaitedSeconds = 0
			}
		}
		if len(r.WaitKeys) == 1 {
			for key := range r.WaitKeys {
				event.Active, _ = g.countLocked(key)
			}
		}
	}
	g.rejections = append(g.rejections, event)
	if len(g.rejections) > 20 {
		g.rejections = append([]accountConcurrencyRejection(nil), g.rejections[len(g.rejections)-20:]...)
	}
}
func concurrencyWaitReason(req pluginapi.SchedulerPickRequest, preferred string, cfg pluginConfig) string {
	if serialPinnedAuthID(req) != "" && preferred != "" {
		return "pinned_credential"
	}
	if preferred != "" && normalizeSchedulerMode(cfg.SchedulerMode) == "serial" {
		return "serial_primary"
	}
	if preferred != "" {
		return "sticky_credential"
	}
	return "pool_full"
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
	// Separate CPA credentials stay independent even for the same workspace/user.
	return out
}
func (s *schedulerRuntimeState) concurrencyPreferred(req pluginapi.SchedulerPickRequest, cfg pluginConfig, now time.Time) string {
	preferred := serialPinnedAuthID(req)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if now.Before(s.balancedClock) {
		now = s.balancedClock
	}
	if preferred == "" && (normalizeSchedulerMode(cfg.SchedulerMode) == "balanced" || normalizeSchedulerMode(cfg.SchedulerMode) == "serial") && cfg.StickySeconds > 0 {
		binding, ok := s.balancedSessionLocked(schedulerSessionHash(req), now)
		if !ok {
			binding, ok = s.balancedSessionLocked(schedulerParentSessionHash(req), now)
		}
		if ok {
			preferred = binding.AuthID
		}
	}
	if preferred == "" && normalizeSchedulerMode(cfg.SchedulerMode) == "serial" && (schedulerSessionHash(req) == "" || normalizeSerialSelectionSource(s.serialSelectionSource) == "manual") {
		preferred = s.serialActiveAuthID
	}
	for _, candidate := range req.Candidates {
		if candidate.ID != preferred {
			continue
		}
		snapshot, found := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
		choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
		stickyEligible := choice.Eligible || (cfg.SerialSoftContinuation && choice.Reason == "serial_threshold")
		if stickyEligible && banStore.schedulable(candidate.ID, now) && !s.adqProviderCircuitActiveLockedForConcurrency(candidate, now) {
			return preferred
		}
	}
	return ""
}

// Match the native serial/balanced eligibility predicate before deciding that a
// free credential can serve queued work. An exhausted idle sibling is not a
// reason to skip the queue for the remaining usable, busy credential.
func (s *schedulerRuntimeState) concurrencySchedulable(req pluginapi.SchedulerPickRequest, cfg pluginConfig, now time.Time) map[string]bool {
	eligible := make(map[string]bool, len(req.Candidates))
	mode := normalizeSchedulerMode(cfg.SchedulerMode)
	pinned := serialPinnedAuthID(req)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if now.Before(s.balancedClock) {
		now = s.balancedClock
	}
	for _, candidate := range req.Candidates {
		if pinned != "" && candidate.ID != pinned {
			continue
		}
		if !banStore.schedulable(candidate.ID, now) || s.adqProviderCircuitActiveLockedForConcurrency(candidate, now) {
			continue
		}
		if mode == "balanced" || mode == "serial" {
			snapshot, found := s.lookupQuotaLocked(candidate.ID, candidateAuthIndex(candidate))
			snapshot = s.serialConservativeQuotaLocked(snapshot, now)
			choice := inspectSerialCandidate(candidate, snapshot, found, cfg, now)
			if !choice.Eligible && !(mode == "balanced" && choice.Reason == "serial_threshold") {
				continue
			}
		}
		eligible[candidate.ID] = true
	}
	return eligible
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
	waitID := ""
	defer func() {
		if waiting {
			s.concurrency.endWait(waitID)
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
		now := time.Now()
		preferred := s.concurrencyPreferred(req, cfg, now)
		eligible := s.concurrencySchedulable(req, cfg, now)
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
			g.rejectLocked(request, "account_concurrency_unavailable", cfg)
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{"account_concurrency_unavailable", "CPA request lifecycle correlation is unavailable"}
		}
		// A rejected lifecycle remains terminal across CPA's alternate credential
		// retries. Otherwise a busy sticky account disappears from Candidates,
		// allowing the same expired request to be silently assigned elsewhere.
		if request != nil && request.Rejection != "" {
			code := request.Rejection
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{code, "bounded queue admission already rejected for this execution"}
		}
		if enforce && request.QueueSequence != 0 && !now.Before(deadline) {
			g.rejectLocked(request, "account_concurrency_busy", cfg)
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return pluginapi.SchedulerPickResponse{}, &accountConcurrencyError{"account_concurrency_busy", concurrencyRejectionMessage("account_concurrency_busy")}
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
		idleEligible := 0
		waitKeys := make(map[string]bool)
		for _, candidate := range req.Candidates {
			key := g.keyLocked(aliases[candidate.ID])
			busy := enforce && g.blockedLocked(key, request, cfg.AccountMaxConcurrency, now)
			if candidate.ID == preferred && busy {
				preferredBusy = true
			}
			if !busy {
				if eligible[candidate.ID] {
					idleEligible++
				}
				attempt.Candidates = append(attempt.Candidates, candidate)
			}
		}
		busy := enforce && (preferredBusy || (len(eligible) > 0 && idleEligible == 0) || len(attempt.Candidates) == 0)
		if busy {
			// All credential aliases have been learned before constructing queue keys.
			for _, candidate := range req.Candidates {
				if (preferred == "" && eligible[candidate.ID]) || preferred == candidate.ID {
					waitKeys[g.keyLocked(aliases[candidate.ID])] = true
				}
			}
			if request != nil && request.QueueDeadline.IsZero() {
				request.QueueDeadline = deadline
			}
			if !waiting {
				waiting = true
				g.waiting++
				waitID = id
			}
			waitAuthID := preferred
			if waitAuthID == "" && len(req.Candidates) == 1 {
				waitAuthID = req.Candidates[0].ID
			}
			g.startWaitLocked(request, waitKeys, waitAuthID, concurrencyWaitReason(req, preferred, cfg), time.Now())
			wake := g.changed
			expired := !time.Now().Before(deadline)
			if expired {
				g.rejectLocked(request, "account_concurrency_busy", cfg)
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
				request.SessionKey = schedulerSessionHash(req)
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
func concurrencyRejectionMessage(code string) string {
	if code == "account_concurrency_busy" {
		return "Codex credential concurrency queue timed out; retry after an active request finishes"
	}
	return "CPA could not verify the request lifecycle for Codex concurrency admission"
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
			s.concurrency.endWait(req.RequestID)
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
		if r != nil && r.Rejection != "" {
			code := r.Rejection
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return concurrencyTermination(code, concurrencyRejectionMessage(code))
		}
		if enabled && r != nil && r.QueueSequence != 0 && !time.Now().Before(deadline) {
			g.rejectLocked(r, "account_concurrency_busy", cfg)
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return concurrencyTermination("account_concurrency_busy", concurrencyRejectionMessage("account_concurrency_busy"))
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
			if enabled && r.Key == "" && g.blockedLocked(key, r, cfg.AccountMaxConcurrency, time.Now()) {
				if r.QueueDeadline.IsZero() {
					r.QueueDeadline = deadline
				}
				if !waiting {
					waiting = true
					g.waiting++
				}
				g.startWaitLocked(r, map[string]bool{key: true}, selectedID, "native_credential", time.Now())
				wake := g.changed
				expired := !time.Now().Before(deadline)
				if expired {
					g.rejectLocked(r, "account_concurrency_busy", cfg)
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
			g.rejectLocked(r, "account_concurrency_unavailable", cfg)
			g.mu.Unlock()
			s.selectionMu.Unlock()
			return concurrencyTermination("account_concurrency_unavailable", "CPA did not admit this Codex execution through the credential concurrency scheduler")
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
	r := s.concurrency.release(id)
	if r == nil {
		return
	}
	if !r.Dispatched {
		s.rollbackUndispatchedConcurrency(r.Token)
		return
	}
	if r.SessionKey != "" {
		s.mu.Lock()
		now := s.balancedTimeLocked(time.Now())
		if binding, ok := s.balancedSessions[r.SessionKey]; ok && binding.AuthID == r.AuthID {
			binding.LastUsedAt = now
			s.balancedSessions[r.SessionKey] = binding
		}
		s.mu.Unlock()
	}
}

func (g *accountConcurrencyGate) hasActiveSession(session, authID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, request := range g.requests {
		if request.SessionKey == session && request.AuthID == authID && request.Key != "" {
			return true
		}
	}
	return false
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
	if cfg.Enabled && cfg.AccountConcurrencyEnabled && g.blockedLocked(key, nil, cfg.AccountMaxConcurrency, time.Now()) {
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
	out := accountConcurrencyStatus{Enabled: cfg.Enabled && cfg.AccountConcurrencyEnabled, Supported: g.supported, Scope: "credential", MaxPerCredential: cfg.AccountMaxConcurrency, MaxPerAccount: cfg.AccountMaxConcurrency, WaitSeconds: cfg.AccountConcurrencyWait.Seconds(), Waiting: g.waiting, Waits: g.waits, Rejected: g.rejected, Accounts: map[string]accountConcurrencyAccountStatus{}}
	out.QueuePolicy = "fifo_per_credential"
	out.RecentRejections = append([]accountConcurrencyRejection(nil), g.rejections...)
	now := time.Now()
	// Keep the original fields as wire aliases for existing panel/API clients.
	out.Credentials = out.Accounts
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
		item := accountConcurrencyAccountStatus{Active: active, Warmup: warmup, Limit: cfg.AccountMaxConcurrency, AtLimit: out.Enabled && active >= cfg.AccountMaxConcurrency, CredentialKey: key}
		for _, r := range g.requests {
			if r.QueueSequence != 0 && r.Rejection == "" && len(r.WaitKeys) == 1 && r.WaitKeys[key] {
				item.Waiting++
				elapsed := now.Sub(r.QueueStartedAt).Seconds()
				if elapsed > item.OldestWaitSeconds {
					item.OldestWaitSeconds = elapsed
				}
			}
		}
		out.Accounts[q.AuthID] = item
	}
	for _, r := range g.requests {
		if r.QueueSequence != 0 && r.Rejection == "" && len(r.WaitKeys) > 1 {
			out.PoolWaiting++
		}
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

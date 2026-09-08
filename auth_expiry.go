package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// Only redacted evidence is persisted. The credential document stays on the
// current refresh stack; it is never exposed to the panel, state file or logs.
type authExpiryStatus struct {
	Reason        string    `json:"reason"`
	Blocked       bool      `json:"blocked"`
	CheckedAt     time.Time `json:"checked_at"`
	RepairedAt    time.Time `json:"repaired_at,omitempty"`
	ExpiredAt     time.Time `json:"expired_at,omitempty"`
	Confirmations int       `json:"confirmations"`
}
type authExpiryState struct {
	Status         authExpiryStatus `json:"status"`
	Fingerprint    string           `json:"fingerprint,omitempty"`
	LastVerifiedAt time.Time        `json:"last_verified_at,omitempty"`
	LastAttemptAt  time.Time        `json:"last_attempt_at,omitempty"`
}
type authExpiryDocument struct {
	Name        string
	Raw         json.RawMessage
	Fingerprint string
	Replacement json.RawMessage
	ExpiredAt   time.Time
	Blocked     bool
	Repairable  bool
}
type authExpiryHostCall func(string, any) (json.RawMessage, error)
type authExpirySave func(context.Context, pluginConfig, authExpiryDocument) error

func cloneAuthExpiryStates(source map[string]authExpiryState) map[string]authExpiryState {
	out := make(map[string]authExpiryState, len(source))
	for id, state := range source {
		out[id] = state
	}
	return out
}
func authDocumentFingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readAuthExpiryDocument(auth cpaAuthFileEntry, call authExpiryHostCall) (authExpiryDocument, error) {
	raw, err := call(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: auth.AuthIndex})
	if err != nil {
		return authExpiryDocument{}, errors.New("host_unavailable")
	}
	var result pluginapi.HostAuthGetResponse
	if json.Unmarshal(raw, &result) != nil || result.AuthIndex != auth.AuthIndex || result.Name != auth.Name || filepath.Base(result.Name) != result.Name || strings.ContainsAny(result.Name, "/\\") || !strings.HasSuffix(result.Name, ".json") || len(result.JSON) == 0 || len(result.JSON) > 1<<20 {
		return authExpiryDocument{}, errors.New("host_unavailable")
	}
	return inspectAuthExpiryDocument(result.Name, result.JSON, time.Now())
}

func inspectAuthExpiryDocument(name string, raw json.RawMessage, now time.Time) (authExpiryDocument, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return authExpiryDocument{}, errors.New("host_unavailable")
	}
	text := func(key string) string {
		var value string
		_ = json.Unmarshal(fields[key], &value)
		return strings.TrimSpace(value)
	}
	var canonical bytes.Buffer
	if json.Compact(&canonical, raw) != nil {
		return authExpiryDocument{}, errors.New("host_unavailable")
	}
	doc := authExpiryDocument{Name: name, Raw: append([]byte(nil), raw...), Fingerprint: authDocumentFingerprint(canonical.Bytes())}
	if text("type") != providerCodex {
		return doc, errors.New("host_unavailable")
	}
	token := text("access_token")
	if token == "" {
		return doc, errors.New("host_unavailable")
	}
	// A JWT's own exp remains authoritative. Never rewrite JWT/OAuth expiry.
	parts := strings.Split(token, ".")
	jwtShaped := len(parts) == 3
	if jwtShaped {
		if payload, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(payload, &claims) == nil && claims.Exp > 0 {
				doc.ExpiredAt = time.Unix(claims.Exp, 0)
				doc.Blocked = !doc.ExpiredAt.After(now)
				return doc, nil
			}
		}
	}
	// Match CPA's top-level expiry precedence; ambiguous or nested expiry is
	// diagnosed only. Repair is intentionally limited to one plain expired key.
	for _, key := range []string{"expired", "expire", "expires_at", "expiresAt", "expiry", "expires"} {
		if value := fields[key]; len(value) > 0 {
			if parsed := parseQuotaTime(value); !parsed.IsZero() {
				doc.ExpiredAt = parsed
				doc.Blocked = !parsed.After(now)
				break
			}
		}
	}
	if !doc.Blocked {
		return doc, nil
	}
	if jwtShaped || text("credential_kind") != "personal_access_token" || text("refresh_token") != "" || len(fields["token"]) > 0 || len(fields["Token"]) > 0 || len(fields["tokens"]) > 0 {
		return doc, nil
	}
	for _, key := range []string{"expire", "expires_at", "expiresAt", "expiry", "expires", "expires_in"} {
		if len(fields[key]) > 0 {
			return doc, nil
		}
	}
	var disabled bool
	if v, exists := fields["disabled"]; exists && (json.Unmarshal(v, &disabled) != nil || disabled) {
		return doc, nil
	}
	if text("status") == "disabled" {
		return doc, nil
	}
	if parseQuotaTime(fields["expired"]).IsZero() {
		return doc, nil
	}
	delete(fields, "expired")
	replacement, err := json.Marshal(fields)
	if err != nil {
		return doc, errors.New("host_unavailable")
	}
	doc.Replacement = replacement
	doc.Repairable = true
	return doc, nil
}

// Called only by the serialized quota refresh loop, once per actual upstream
// observation. Cache reads and failed requests never advance confirmation.
func (s *schedulerRuntimeState) reconcileAuthExpiry(ctx context.Context, cfg pluginConfig, auth cpaAuthFileEntry, doc authExpiryDocument, documentErr error, snapshot quotaSnapshot, fetchErr error, observedAt time.Time, call authExpiryHostCall, save authExpirySave) {
	if !s.generationOwnerActive() {
		return
	}
	s.mu.RLock()
	state := s.authExpiry[auth.ID]
	s.mu.RUnlock()
	now := time.Now()
	state.Status.CheckedAt = now
	finish := func(reason string) {
		state.Status.Reason = reason
		s.mu.Lock()
		if s.authExpiry == nil {
			s.authExpiry = map[string]authExpiryState{}
		}
		s.authExpiry[auth.ID] = state
		s.mu.Unlock()
	}
	reset := func() { state.Status.Confirmations = 0; state.LastVerifiedAt = time.Time{} }
	if documentErr != nil {
		reset()
		finish("host_unavailable")
		return
	}
	state.Status.Blocked = doc.Blocked
	state.Status.ExpiredAt = doc.ExpiredAt
	if !doc.Blocked {
		reset()
		if !state.Status.RepairedAt.IsZero() {
			finish("repaired")
		} else {
			finish("checked")
		}
		return
	}
	if !doc.Repairable {
		reset()
		finish("token_expired")
		return
	}
	if !cfg.AuthExpiryAutoRepair {
		reset()
		finish("repair_disabled")
		return
	}
	if fetchErr != nil || ctx.Err() != nil || snapshot.AuthIndex != auth.AuthIndex || snapshot.RefreshedAt != observedAt || observedAt.Before(doc.ExpiredAt) {
		reset()
		finish("verification_failed")
		return
	}
	if !quotaEvaluationWithOrder(snapshot, now, cfg.WindowOrder).Eligible || auth.Disabled || auth.Unavailable || auth.Status != "active" || authExpiryQuarantined(auth.ID) {
		reset()
		finish("repair_deferred")
		return
	}
	// Read again after network I/O so success cannot validate a replaced token.
	current, err := readAuthExpiryDocument(auth, call)
	if err != nil || current.Fingerprint != doc.Fingerprint {
		reset()
		finish("credential_changed")
		return
	}
	if state.Fingerprint != doc.Fingerprint || now.Sub(state.LastVerifiedAt) > 10*time.Minute {
		reset()
		state.Fingerprint = doc.Fingerprint
	}
	if state.LastVerifiedAt.IsZero() || observedAt.Sub(state.LastVerifiedAt) >= 30*time.Second {
		state.Status.Confirmations++
		state.LastVerifiedAt = observedAt
	}
	if state.Status.Confirmations < 2 {
		finish("expiry_conflict")
		return
	}
	// A repeated write from another owner is not a reason to fight over files.
	if !state.Status.RepairedAt.IsZero() && now.Sub(state.Status.RepairedAt) < 24*time.Hour {
		reset()
		finish("repeated_expiry")
		return
	}
	if !state.LastAttemptAt.IsZero() && now.Sub(state.LastAttemptAt) < 30*time.Minute {
		finish("repair_failed")
		return
	}
	state.LastAttemptAt = now
	finish("expiry_conflict")
	// Persist intent before a credential write so restarts cannot retry an
	// ambiguous save in a tight loop. Require durable generation ownership.
	if strings.TrimSpace(cfg.StatePath) == "" || !s.persistBanState() {
		finish("repair_failed")
		return
	}
	committed, err := s.withGenerationOwnerCommit(func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.RLock()
		enabled := s.cfg.AuthExpiryAutoRepair && s.cfg.Enabled
		s.mu.RUnlock()
		if !enabled || authExpiryQuarantined(auth.ID) {
			return errors.New("repair_deferred")
		}
		raw, err := call(pluginabi.MethodHostAuthGetRuntime, pluginapi.HostAuthGetRequest{AuthIndex: auth.AuthIndex})
		if err != nil {
			return errors.New("host_unavailable")
		}
		var live pluginapi.HostAuthGetRuntimeResponse
		if json.Unmarshal(raw, &live) != nil || live.Auth.ID != auth.ID || live.Auth.AuthIndex != auth.AuthIndex || live.Auth.Name != doc.Name || live.Auth.Disabled || live.Auth.Unavailable || live.Auth.Status != "active" || live.Auth.NextRetryAfter.After(time.Now()) || live.Auth.RuntimeOnly {
			return errors.New("repair_deferred")
		}
		latest, err := readAuthExpiryDocument(auth, call)
		if err != nil || latest.Fingerprint != doc.Fingerprint {
			return errors.New("credential_changed")
		}
		err = save(ctx, cfg, doc)
		if err != nil {
			return errors.New("repair_failed")
		}
		saved, err := readAuthExpiryDocument(auth, call)
		if err != nil || saved.Blocked {
			return errors.New("repair_failed")
		}
		// Ensure CPA persisted exactly the intended document, modulo formatting.
		var a, b any
		_ = json.Unmarshal(saved.Raw, &a)
		_ = json.Unmarshal(doc.Replacement, &b)
		ar, _ := json.Marshal(a)
		br, _ := json.Marshal(b)
		if !bytes.Equal(ar, br) {
			return errors.New("credential_changed")
		}
		return nil
	})
	if err != nil {
		reset()
		finish(err.Error())
		return
	}
	if !committed {
		reset()
		finish("repair_deferred")
		return
	}
	reset()
	state.Status.Blocked = false
	state.Status.RepairedAt = now
	finish("repaired")
	s.persistBanState()
}

func authExpiryQuarantined(id string) bool {
	banStore.mu.Lock()
	defer banStore.mu.Unlock()
	_, found := banStore.bans[id]
	return found
}

// Use CPA's native upload route: its synthesizer preserves routing attributes
// (proxy, prefix, priority and custom headers) as well as the credential JSON.
func saveNativeAuthExpiry(ctx context.Context, cfg pluginConfig, doc authExpiryDocument) error {
	endpoint, err := cpaAuthFilesEndpoint(cfg.CPAManagementURL)
	if err != nil {
		return errors.New("repair_failed")
	}
	key, err := os.ReadFile(cfg.CPAManagementKeyFile)
	if err != nil || strings.TrimSpace(string(key)) == "" {
		return errors.New("repair_failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"?name="+url.QueryEscape(doc.Name), bytes.NewReader(doc.Replacement))
	if err != nil {
		return errors.New("repair_failed")
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(key)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("repair_failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var result struct {
		Status string `json:"status"`
	}
	if err != nil || resp.StatusCode != http.StatusOK || len(raw) > 4096 || json.Unmarshal(raw, &result) != nil || result.Status != "ok" {
		return errors.New("repair_failed")
	}
	return nil
}

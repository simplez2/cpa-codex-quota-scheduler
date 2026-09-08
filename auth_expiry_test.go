package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

type expiryFixture struct {
	t               *testing.T
	raw             json.RawMessage
	auth            cpaAuthFileEntry
	saves           int
	disabled        bool
	mutateAfterRead bool
	saveErr         bool
}

func newExpiryFixture(t *testing.T) *expiryFixture {
	t.Helper()
	f := &expiryFixture{t: t, auth: cpaAuthFileEntry{ID: "expiry.json", Name: "expiry.json", AuthIndex: "expiry-idx", Provider: "codex", Status: "active"}}
	f.raw = []byte(`{"type":"codex","access_token":"private-test-token","credential_kind":"personal_access_token","expired":"2020-01-01T00:00:00Z","disabled":false,"priority":23,"websockets":true,"note":"preserve","custom":{"value":42}}`)
	return f
}
func (f *expiryFixture) call(method string, request any) (json.RawMessage, error) {
	switch method {
	case pluginabi.MethodHostAuthGet:
		raw, _ := json.Marshal(pluginapi.HostAuthGetResponse{AuthIndex: f.auth.AuthIndex, Name: f.auth.Name, JSON: f.raw})
		return raw, nil
	case pluginabi.MethodHostAuthGetRuntime:
		if f.mutateAfterRead {
			f.raw = []byte(strings.Replace(string(f.raw), "preserve", "operator edit", 1))
		}
		raw, _ := json.Marshal(pluginapi.HostAuthGetRuntimeResponse{Auth: pluginapi.HostAuthFileEntry{ID: f.auth.ID, Name: f.auth.Name, AuthIndex: f.auth.AuthIndex, Provider: "codex", Status: "active", Disabled: f.disabled}})
		return raw, nil
	default:
		f.t.Fatalf("unexpected callback %s", method)
		return nil, nil
	}
}
func expiryRuntime(t *testing.T) *schedulerRuntimeState {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	s := newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	claimManagedRuntimeForTest(t, s)
	return s
}
func (f *expiryFixture) observe(s *schedulerRuntimeState, at time.Time, err error) {
	doc, de := readAuthExpiryDocument(f.auth, f.call)
	q, qe := parseNativeQuota([]byte(nativeQuotaTestBody(10)), f.auth.ID, f.auth.AuthIndex, at)
	if qe != nil {
		f.t.Fatal(qe)
	}
	s.reconcileAuthExpiry(context.Background(), s.cfg, f.auth, doc, de, q, err, at, f.call, f.save)
}
func TestAuthExpiryRepairsOnlyAfterTwoFreshObservations(t *testing.T) {
	s := expiryRuntime(t)
	f := newExpiryFixture(t)
	before := string(f.raw)
	at := time.Now().Add(-time.Minute)
	f.observe(s, at, nil)
	if f.saves != 0 || s.authExpiry[f.auth.ID].Status.Confirmations != 1 {
		t.Fatal("first observation repaired")
	}
	f.observe(s, at, nil)
	if f.saves != 0 || s.authExpiry[f.auth.ID].Status.Confirmations != 1 {
		t.Fatal("cache replay advanced")
	}
	f.observe(s, at.Add(31*time.Second), nil)
	if f.saves != 1 || s.authExpiry[f.auth.ID].Status.Reason != "repaired" || s.authExpiry[f.auth.ID].Status.Blocked {
		t.Fatalf("repair missing: saves=%d state=%+v", f.saves, s.authExpiry[f.auth.ID].Status)
	}
	var original, saved map[string]any
	_ = json.Unmarshal([]byte(before), &original)
	_ = json.Unmarshal(f.raw, &saved)
	delete(original, "expired")
	a, _ := json.Marshal(original)
	b, _ := json.Marshal(saved)
	if string(a) != string(b) {
		t.Fatal("unrelated fields changed")
	}
	f.observe(s, time.Now(), nil)
	if f.saves != 1 {
		t.Fatal("repeated repair")
	}
	raw, err := os.ReadFile(s.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-test-token") || strings.Contains(string(raw), "preserve") {
		t.Fatal("credential leaked into state")
	}
	next := newManagedRuntimeForTest(t, s.cfg.StatePath)
	claimManagedRuntimeForTest(t, next)
	if next.authExpiry[f.auth.ID].Status.RepairedAt.IsZero() {
		t.Fatal("repair evidence lost across generation")
	}
	// Another writer reintroduces the same stale field: no write fight.
	f.raw = []byte(before)
	f.observe(next, at, nil)
	f.observe(next, time.Now(), nil)
	if f.saves != 1 || next.authExpiry[f.auth.ID].Status.Reason != "repeated_expiry" {
		t.Fatal("repeated writer not suppressed")
	}
}
func TestAuthExpiryFailureAndConcurrencyGuards(t *testing.T) {
	for _, kind := range []string{"522", "quota", "disabled", "changed", "save_error", "option_disabled", "retired", "token_changed"} {
		t.Run(kind, func(t *testing.T) {
			s := expiryRuntime(t)
			f := newExpiryFixture(t)
			at := time.Now().Add(-time.Minute)
			f.observe(s, at, nil)
			var failure error
			switch kind {
			case "522":
				failure = errors.New("quota_http_522")
			case "quota":
				banStore.set(f.auth.ID, banEntry{Kind: banKindQuota, Window: "5h", BannedAt: at, ResetAt: time.Now().Add(time.Hour)})
			case "disabled":
				f.disabled = true
			case "changed":
				f.mutateAfterRead = true
			case "save_error":
				f.saveErr = true
			case "option_disabled":
				s.cfg.AuthExpiryAutoRepair = false
			case "retired":
				next := newManagedRuntimeForTest(t, s.cfg.StatePath)
				claimManagedRuntimeForTest(t, next)
			case "token_changed":
				f.raw = []byte(strings.Replace(string(f.raw), "private-test-token", "new-private-token", 1))
			}
			f.observe(s, time.Now(), failure)
			if kind == "save_error" {
				if f.saves != 1 {
					t.Fatal("save was not attempted")
				}
				f.observe(s, time.Now(), nil)
				f.observe(s, time.Now().Add(time.Minute), nil)
				if f.saves != 1 {
					t.Fatal("ambiguous save repeated")
				}
			} else if f.saves != 0 {
				t.Fatal("guard allowed save")
			}
			encoded, _ := json.Marshal(s.authExpiry)
			if strings.Contains(string(encoded), "private-test") {
				t.Fatal("secret leak")
			}
		})
	}
}
func TestAuthExpiryTokenBoundary(t *testing.T) {
	now := time.Now()
	jwt := func(exp int64) string {
		p, _ := json.Marshal(map[string]any{"exp": exp})
		return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(p) + ".signature"
	}
	for _, tc := range []struct {
		name            string
		patch           map[string]any
		repair, blocked bool
	}{
		{"pat", nil, true, true},
		{"oauth", map[string]any{"credential_kind": "oauth", "refresh_token": "refresh-secret"}, false, true},
		{"unclassified", map[string]any{"credential_kind": ""}, false, true},
		{"expired_jwt", map[string]any{"access_token": jwt(now.Add(-time.Hour).Unix())}, false, true},
		{"valid_jwt", map[string]any{"access_token": jwt(now.Add(time.Hour).Unix())}, false, false},
		{"extra_expiry", map[string]any{"expires_at": "2020-01-01T00:00:00Z"}, false, true},
		{"nested_token", map[string]any{"token": map[string]string{"expires_at": "2020-01-01T00:00:00Z"}}, false, true},
		{"capital_nested_token", map[string]any{"Token": map[string]string{"expires_at": "2020-01-01T00:00:00Z"}}, false, true},
		{"invalid_jwt", map[string]any{"access_token": "invalid.payload.signature"}, false, true},
		{"disabled", map[string]any{"disabled": true}, false, true},
		{"future", map[string]any{"expired": now.Add(time.Hour).Format(time.RFC3339)}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExpiryFixture(t)
			var d map[string]any
			_ = json.Unmarshal(f.raw, &d)
			for k, v := range tc.patch {
				d[k] = v
			}
			raw, _ := json.Marshal(d)
			doc, err := inspectAuthExpiryDocument(f.auth.Name, raw, now)
			if err != nil || doc.Repairable != tc.repair || doc.Blocked != tc.blocked {
				t.Fatalf("repair=%v blocked=%v err=%v", doc.Repairable, doc.Blocked, err)
			}
		})
	}
}
func TestAuthExpiryPanelDoesNotAdvertiseExpiredAccountAsUsable(t *testing.T) {
	s := expiryRuntime(t)
	f := newExpiryFixture(t)
	at := time.Now()
	f.observe(s, at, nil)
	q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(10)), f.auth.ID, f.auth.AuthIndex, at)
	s.quotas[f.auth.ID] = q
	result := s.status()
	if !result.AuthExpiryAutoRepair || len(result.Snapshots) != 1 || result.Snapshots[0].Eligible || result.Snapshots[0].AuthHealth == nil || !result.Snapshots[0].AuthHealth.Blocked {
		t.Fatal("panel advertised expired credential")
	}
	cfg, err := parsePluginConfig([]byte("auth_expiry_auto_repair: false"))
	if err != nil || cfg.AuthExpiryAutoRepair {
		t.Fatal("panel switch ignored")
	}
}

func (f *expiryFixture) save(_ context.Context, _ pluginConfig, doc authExpiryDocument) error {
	f.saves++
	if f.saveErr {
		return errors.New("private-test-error-must-not-leak")
	}
	if doc.Name != f.auth.Name {
		f.t.Fatal("wrong file")
	}
	f.raw = append([]byte(nil), doc.Replacement...)
	return nil
}

func TestNativeAuthExpirySavePreservesDocumentAndRejectsAmbiguousResponses(t *testing.T) {
	for _, kind := range []string{"ok", "redirect", "server_error", "invalid", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			f := newExpiryFixture(t)
			doc, err := inspectAuthExpiryDocument("account+test@example.test.json", f.raw, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v0/management/auth-files" || r.URL.Query().Get("name") != doc.Name || r.Header.Get("Authorization") != "Bearer management-test-key" {
					t.Error("wrong native upload request")
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != string(doc.Replacement) {
					t.Error("credential document changed in upload")
				}
				switch kind {
				case "ok":
					io.WriteString(w, `{"status":"ok"}`)
				case "redirect":
					w.Header().Set("Location", "/unexpected")
					w.WriteHeader(307)
				case "server_error":
					w.WriteHeader(500)
					io.WriteString(w, "private-test-token")
				case "invalid":
					io.WriteString(w, `{"status":"unknown"}`)
				case "oversize":
					io.WriteString(w, strings.Repeat("x", 4097))
				}
			}))
			defer server.Close()
			keyFile := filepath.Join(t.TempDir(), "management.key")
			if err := os.WriteFile(keyFile, []byte("management-test-key"), 0600); err != nil {
				t.Fatal(err)
			}
			err = saveNativeAuthExpiry(context.Background(), pluginConfig{CPAManagementURL: server.URL + "/v0/management/api-call", CPAManagementKeyFile: keyFile}, doc)
			if (err == nil) != (kind == "ok") || calls != 1 {
				t.Fatalf("unexpected result: calls=%d err=%v", calls, err)
			}
			if err != nil && err.Error() != "repair_failed" {
				t.Fatal("upstream body leaked")
			}
		})
	}
}

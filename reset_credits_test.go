package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestNativeResetCreditSchemaCompatibility(t *testing.T) {
	for _, tc := range []struct {
		extra    string
		known    bool
		count    int
		appKnown bool
		app      int
	}{
		{`"rate_limit_reset_credits":{"available_count":2,"applicable_available_count":0}`, true, 2, true, 0},
		{`"rate_limit_reset_credits":{"available_count":0,"applicable_available_count":0}`, true, 0, true, 0},
		{`"rateLimitResetCredits":{"availableCount":3,"applicableAvailableCount":1}`, true, 3, true, 1},
		{`"rate_limit_reset_credits_available_count":4`, true, 4, false, 0},
		{`"rate_limit_reset_credits":{"available_count":2},"rate_limit_reset_credits_available_count":9`, true, 2, false, 0},
		{`"rate_limit_reset_credits":{"available_count":"secret","applicable_available_count":null}`, false, 0, false, 0},
		{`"rate_limit_reset_credits":{"available_count":-1,"applicable_available_count":1.5}`, false, 0, false, 0},
		{`"rate_limit_reset_credits":"secret","rate_limit_reset_credits_available_count":{}`, false, 0, false, 0},
		{`"rate_limit_reset_credits":{"applicable_available_count":1}`, false, 0, true, 1},
	} {
		t.Run(tc.extra, func(t *testing.T) {
			now := time.Now()
			body := strings.TrimSuffix(nativeQuotaTestBody(40), "}") + "," + tc.extra + "}"
			q, err := parseNativeQuota([]byte(body), "a", "i", now)
			if err != nil || len(q.Windows) != 2 || q.ResetCreditsKnown != tc.known || q.ResetCredits != tc.count || q.ResetCreditsApplicableKnown != tc.appKnown || q.ResetCreditsApplicable != tc.app {
				t.Fatalf("q=%+v err=%v", q, err)
			}
			if tc.appKnown && !q.ResetCreditsApplicableObservedAt.Equal(now) {
				t.Fatal("applicability lost its own observation time")
			}
		})
	}
}

func resetTestCredit(id string, expires time.Time) map[string]any {
	return map[string]any{"id": id, "reset_type": "codex_rate_limits", "status": "available", "is_supported_by_plan": true, "granted_at": "2026-10-08T00:04:39.734115Z", "expires_at": expires.Format(time.RFC3339Nano)}
}

func TestResetCreditListRetainsExactExpiryAndFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	early := now.Add(7*time.Minute + 123456*time.Microsecond)
	late := now.Add(30 * 24 * time.Hour)
	raw, _ := json.Marshal(map[string]any{"available_count": 2, "credits": []any{resetTestCredit("later-private-id", late), resetTestCredit("earlier-private-id", early)}})
	state, err := parseResetCreditList(raw, "index", now)
	if err != nil || !state.Complete || !state.Credits[0].ExpiresAt.Equal(early) || len(state.Credits) != 2 || state.Credits[0].Key != resetCreditHash("earlier-private-id") {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), "private-id") {
		t.Fatal("raw credit ID persisted")
	}
	credit, reason := resetCreditDue(state, now, 10*time.Minute)
	if reason != "due" || !credit.ExpiresAt.Equal(early) {
		t.Fatal(reason)
	}
	for _, bad := range []string{
		`{"available_count":2,"credits":[]}`,
		`{"available_count":1,"credits":[{"id":"a","reset_type":"codex_rate_limits","status":"available","expires_at":"bad"}]}`,
		`{"credits":[]}`,
	} {
		s, err := parseResetCreditList([]byte(bad), "index", now)
		if err == nil {
			if _, reason = resetCreditDue(s, now, 10*time.Minute); reason != "details_incomplete" {
				t.Fatalf("bad details accepted %s: %s", bad, reason)
			}
		}
	}
	for _, bad := range []string{`{}`, `{"credits":null}`, `[]`, `{"credits":{}}`} {
		if _, err := parseResetCreditList([]byte(bad), "index", now); err == nil {
			t.Fatal("invalid list accepted", bad)
		}
	}
	for _, tc := range []struct {
		expiry    time.Time
		supported bool
		reason    string
	}{
		{now.Add(11 * time.Minute), true, "scheduled"},
		{now.Add(10 * time.Minute), true, "due"},
		{now.Add(5 * time.Second), true, "too_late"},
		{now, true, "no_available_credits"},
		{now.Add(-time.Second), true, "no_available_credits"},
		{early, false, "plan_unsupported"},
	} {
		s := resetCreditState{Complete: true, CountKnown: true, Count: 1, Credits: []resetCredit{{Key: "key", ExpiresAt: tc.expiry, Supported: tc.supported}}}
		if _, r := resetCreditDue(s, now, 10*time.Minute); r != tc.reason {
			t.Fatalf("got %s want %s", r, tc.reason)
		}
	}
}

type resetCreditFixture struct {
	t                    *testing.T
	mu                   sync.Mutex
	state                *schedulerRuntimeState
	auth                 cpaAuthFileEntry
	credits              []map[string]any
	applicable           any
	used                 float64
	postCode             string
	postHTTP             int
	quotaHTTP            int
	quotaRetry           string
	onQuota              func()
	disabled             bool
	replaced             bool
	manualDuringQuota    bool
	listHTTP             int
	listRetry            string
	failVerification     bool
	lists, quotas, posts int
	requestID            string
}

func newResetCreditFixture(t *testing.T) *resetCreditFixture {
	t.Helper()
	f := &resetCreditFixture{t: t, applicable: 1, used: 80, postCode: "reset", postHTTP: 200}
	f.auth.ID, f.auth.Name, f.auth.AuthIndex, f.auth.Provider = "account", "account", "index", "codex"
	f.auth.IDToken.AccountID = "account-id"
	f.credits = []map[string]any{resetTestCredit("expiring", time.Now().Add(5*time.Minute)), resetTestCredit("later", time.Now().Add(24*time.Hour))}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.state = newManagedRuntimeForTest(t, filepath.Join(t.TempDir(), "state.json"))
	claimManagedRuntimeForTest(t, f.state)
	f.state.cfg.CPAManagementURL = server.URL + "/v0/management/api-call"
	f.state.cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(f.state.cfg.CPAManagementKeyFile, []byte("fixture-key"), 0600); err != nil {
		t.Fatal(err)
	}
	f.install(f.state)
	return f
}

func (f *resetCreditFixture) install(s *schedulerRuntimeState) {
	s.resetCreditInventory = map[string]cpaAuthFileEntry{f.auth.ID: f.auth}
	s.identities = map[string]string{f.auth.AuthIndex: f.auth.ID}
	q, _ := parseNativeQuota([]byte(nativeQuotaTestBody(80)), f.auth.ID, f.auth.AuthIndex, time.Now())
	s.quotas = map[string]quotaSnapshot{f.auth.ID: q, f.auth.AuthIndex: q}
}

func (f *resetCreditFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer fixture-key" {
		f.t.Error("management key missing")
	}
	if r.URL.Path == "/v0/management/auth-files" {
		a := f.auth
		a.Disabled = f.disabled
		if f.replaced {
			a.AuthIndex = "new-index"
		}
		writeTestJSON(f.t, w, map[string]any{"files": []cpaAuthFileEntry{a}})
		return
	}
	if r.URL.Path != "/v0/management/api-call" {
		f.t.Error("unexpected route", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	var call cpaAPICallRequest
	if json.NewDecoder(r.Body).Decode(&call) != nil {
		f.t.Error("bad call")
		w.WriteHeader(400)
		return
	}
	if call.AuthIndex != f.auth.AuthIndex || call.Header["Authorization"] != "Bearer $TOKEN$" || call.Header["ChatGPT-Account-Id"] != f.auth.IDToken.AccountID {
		f.t.Error("wrong credential binding")
	}
	result := cpaAPICallResponse{StatusCode: 200}
	switch {
	case call.Method == "GET" && call.URL == resetCreditsURL:
		f.lists++
		if call.Header["OpenAI-Beta"] != "codex-1" || call.Header["Originator"] != "Codex Desktop" {
			f.t.Error("missing native list headers")
		}
		if f.listHTTP != 0 {
			result.StatusCode = f.listHTTP
			result.Body = "DO_NOT_LEAK_UPSTREAM_ERROR"
			result.Header = map[string][]string{"Retry-After": {f.listRetry}}
			break
		}
		if f.failVerification && f.posts > 0 {
			f.failVerification = false
			result.StatusCode = 503
			break
		}
		data, _ := json.Marshal(map[string]any{"available_count": len(f.credits), "credits": f.credits})
		result.Body = string(data)
	case call.Method == "GET" && call.URL == defaultPluginConfig().QuotaURL:
		f.quotas++
		if f.onQuota != nil {
			fn := f.onQuota
			f.onQuota = nil
			fn()
		}
		if f.quotaHTTP != 0 {
			result.StatusCode, result.Header = f.quotaHTTP, map[string][]string{"Retry-After": {f.quotaRetry}}
			break
		}
		if f.manualDuringQuota && len(f.credits) > 0 {
			f.credits = f.credits[1:]
			f.manualDuringQuota = false
		}
		result.Body = fmt.Sprintf(`{"account_id":"account-id","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%v,"limit_window_seconds":18000,"reset_after_seconds":3600},"secondary_window":{"used_percent":%v,"limit_window_seconds":604800,"reset_after_seconds":86400}},"rate_limit_reset_credits":{"available_count":%d,"applicable_available_count":%s}}`, f.used, f.used, len(f.credits), mustResetJSON(f.applicable))
	case call.Method == "POST" && call.URL == resetCreditsURL+"/consume":
		f.posts++
		var body map[string]string
		if json.Unmarshal([]byte(call.Data), &body) != nil || len(body) != 1 || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(body["redeem_request_id"]) {
			f.t.Error("invalid native redemption request")
		}
		f.requestID = body["redeem_request_id"]
		journal, err := os.ReadFile(f.state.cfg.StatePath + ".reset-credits.json")
		if err != nil || !strings.Contains(string(journal), f.requestID) || !strings.Contains(string(journal), `"phase":"pending"`) {
			f.t.Error("consume sent before durable intent")
		}
		result.StatusCode = f.postHTTP
		result.Body = mustResetJSON(map[string]string{"code": f.postCode})
		if f.postHTTP == 200 && f.postCode == "reset" {
			f.credits = f.credits[1:]
			f.used = 0
			f.applicable = 0
		}
	default:
		f.t.Error("unexpected call (no model requests allowed)", call.Method, call.URL)
		result.StatusCode = 400
	}
	writeTestJSON(f.t, w, result)
}

func mustResetJSON(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func (f *resetCreditFixture) tick() {
	f.state.mu.Lock()
	if f.state.resetCredits != nil {
		s := f.state.resetCredits[f.auth.ID]
		s.NextAt = time.Time{}
		f.state.resetCredits[f.auth.ID] = s
	}
	f.state.mu.Unlock()
	f.state.refreshResetCredits(context.Background())
}

func TestResetCreditDefaultOffOnlyReadsAndPanelRedactsIDs(t *testing.T) {
	f := newResetCreditFixture(t)
	f.tick()
	if f.posts != 0 || f.quotas != 0 || f.lists != 1 {
		t.Fatalf("writes=%d quotas=%d lists=%d", f.posts, f.quotas, f.lists)
	}
	f.state.refreshResetCredits(context.Background())
	if f.lists != 1 {
		t.Fatal("idle list query storm")
	}
	status := f.state.status().Snapshots[0]
	if status.ResetCredits != 2 || !status.ResetCreditsKnown || len(status.ResetCreditDetails.Credits) != 2 || status.ResetCreditDetails.AutoEnabled {
		t.Fatalf("status %+v", status)
	}
	raw, _ := json.Marshal(status.ResetCreditDetails)
	for _, secret := range []string{"fixture-key", "expiring", "request_id", "account-id", "index"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("panel leaked", secret)
		}
	}
}

func TestResetCreditAutoUsesOnceAndSurvivesReload(t *testing.T) {
	f := newResetCreditFixture(t)
	f.state.cfg.ResetCreditsAutoUse = true
	f.tick()
	if f.posts != 1 || len(f.credits) != 1 || f.state.resetCredits[f.auth.ID].LastAction != "verified" {
		t.Fatalf("posts=%d state=%+v", f.posts, f.state.resetCredits)
	}
	// Restore a stale upstream card to demonstrate the durable per-card fence.
	f.credits = append([]map[string]any{resetTestCredit("expiring", time.Now().Add(5*time.Minute))}, f.credits...)
	f.applicable, f.used = 1, 80
	old := f.state
	reloaded := newManagedRuntimeForTest(t, old.cfg.StatePath)
	reloaded.cfg = old.cfg
	claimManagedRuntimeForTest(t, reloaded)
	f.state = reloaded
	f.install(reloaded)
	f.tick()
	if f.posts != 1 || reloaded.resetCredits[f.auth.ID].AutoReason != "previous_attempt_requires_review" {
		t.Fatalf("duplicate after reload: %+v", reloaded.resetCredits)
	}
}

func TestResetCreditAutoUnknownOutcomeNeverReplays(t *testing.T) {
	for _, status := range []int{200, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newResetCreditFixture(t)
			f.state.cfg.ResetCreditsAutoUse = true
			f.postHTTP, f.postCode = status, "unexpected"
			f.tick()
			f.tick()
			if f.posts != 1 || f.state.resetCredits[f.auth.ID].LastAction != "unknown" {
				t.Fatalf("unknown result replayed %+v", f.state.resetCredits)
			}
			old := f.state
			f.state = newManagedRuntimeForTest(t, old.cfg.StatePath)
			f.state.cfg = old.cfg
			claimManagedRuntimeForTest(t, f.state)
			f.install(f.state)
			f.tick()
			if f.posts != 1 {
				t.Fatal("unknown result replayed across reload")
			}
		})
	}
}

func TestResetCreditConfirmedWriteOnlyRetriesReadVerification(t *testing.T) {
	f := newResetCreditFixture(t)
	f.state.cfg.ResetCreditsAutoUse = true
	f.failVerification = true
	f.tick()
	if f.posts != 1 || f.state.resetCredits[f.auth.ID].LastAction != "confirmed" {
		t.Fatal(f.state.resetCredits)
	}
	f.tick()
	if f.posts != 1 || f.state.resetCredits[f.auth.ID].LastAction != "verified" {
		t.Fatal("verification did not recover", f.state.resetCredits)
	}
}

func TestResetCreditAutoRefusesIneligibleOrChangedInputs(t *testing.T) {
	for _, name := range []string{"applicable-zero", "applicable-unknown", "unused", "future", "expired", "unsupported", "disabled", "replaced", "manual-consume", "custom-adapter", "no-durable-state", "canceled"} {
		t.Run(name, func(t *testing.T) {
			f := newResetCreditFixture(t)
			f.state.cfg.ResetCreditsAutoUse = true
			switch name {
			case "applicable-zero":
				f.applicable = 0
			case "applicable-unknown":
				f.applicable = nil
			case "unused":
				f.used = 0
			case "future":
				f.credits[0]["expires_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
			case "expired":
				f.credits = []map[string]any{resetTestCredit("expired", time.Now().Add(-time.Minute))}
			case "unsupported":
				delete(f.credits[0], "is_supported_by_plan")
			case "disabled":
				f.disabled = true
			case "replaced":
				f.replaced = true
			case "manual-consume":
				f.manualDuringQuota = true
			case "custom-adapter":
				f.state.cfg.QuotaURL = "https://custom.invalid/usage"
			case "no-durable-state":
				f.state.cfg.StatePath = ""
				f.state.resetGenerationOwnership()
			case "canceled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				f.state.refreshResetCredits(ctx)
			}
			if name != "canceled" {
				f.tick()
			}
			if f.posts != 0 {
				t.Fatal("ineligible write", name)
			}
		})
	}
}

func TestResetCreditReadBackoffAndNoErrorDisclosure(t *testing.T) {
	f := newResetCreditFixture(t)
	f.listHTTP = 429
	f.listRetry = "1800"
	f.state.refreshResetCredits(context.Background())
	state := f.state.resetCredits[f.auth.ID]
	if state.NextAt.Before(time.Now().Add(29*time.Minute)) || state.Error != "reset_credits_http_429" {
		t.Fatal(state)
	}
	f.state.refreshResetCredits(context.Background())
	if f.lists != 1 || f.posts != 0 {
		t.Fatal("quota read storm")
	}
	encoded, _ := json.Marshal(f.state.status())
	if strings.Contains(string(encoded), "DO_NOT_LEAK") {
		t.Fatal("upstream error leaked")
	}
}

func TestResetCreditListDoesNotRefreshApplicability(t *testing.T) {
	f := newResetCreditFixture(t)
	q := f.state.quotas[f.auth.ID]
	q.ResetCreditsApplicableKnown = true
	q.ResetCreditsApplicable = 1
	q.ResetCreditsApplicableObservedAt = time.Now().Add(-time.Hour)
	f.state.quotas[f.auth.ID] = q
	f.tick()
	if f.state.status().Snapshots[0].ResetCreditDetails.ApplicableFresh {
		t.Fatal("list query renewed stale applicability")
	}
}

func TestResetCreditConfigDefaultAndValidation(t *testing.T) {
	if defaultPluginConfig().ResetCreditsAutoUse || defaultPluginConfig().ResetCreditsAutoUseBefore != 10*time.Minute {
		t.Fatal("unsafe defaults")
	}
	for _, v := range []string{"0s", "59s", "25h", "bad"} {
		if _, err := parsePluginConfig([]byte("reset_credits_auto_use_before: " + v)); err == nil {
			t.Fatal("bad lead time accepted", v)
		}
	}
	cfg, err := parsePluginConfig([]byte("reset_credits_auto_use: true\nreset_credits_auto_use_before: 20m"))
	if err != nil || !cfg.ResetCreditsAutoUse || cfg.ResetCreditsAutoUseBefore != 20*time.Minute {
		t.Fatal(cfg, err)
	}
}

func TestResetCreditAutoQuotaFailureHonorsRetryAfter(t *testing.T) {
	f := newResetCreditFixture(t)
	f.state.cfg.ResetCreditsAutoUse = true
	f.quotaHTTP, f.quotaRetry = 429, "1800"
	f.state.refreshResetCredits(context.Background())
	state := f.state.resetCredits[f.auth.ID]
	if state.AutoReason != "quota_not_confirmed" || state.NextAt.Before(time.Now().Add(29*time.Minute)) {
		t.Fatal(state)
	}
	f.state.refreshResetCredits(context.Background())
	if f.quotas != 1 || f.posts != 0 {
		t.Fatal("eligibility read violated Retry-After")
	}
}

func TestResetCreditAutoFencedByGenerationTakeover(t *testing.T) {
	f := newResetCreditFixture(t)
	f.state.cfg.ResetCreditsAutoUse = true
	f.onQuota = func() {
		newOwner := newManagedRuntimeForTest(t, f.state.cfg.StatePath)
		claimManagedRuntimeForTest(t, newOwner)
	}
	f.tick()
	if f.posts != 0 || f.state.generationOwnerActive() {
		t.Fatal("retired generation redeemed a credit")
	}
}

func TestResetCreditCorruptJournalBlocksConsumption(t *testing.T) {
	f := newResetCreditFixture(t)
	f.state.cfg.ResetCreditsAutoUse = true
	if err := os.WriteFile(f.state.cfg.StatePath+".reset-credits.json", []byte(`not-json`), 0600); err != nil {
		t.Fatal(err)
	}
	f.tick()
	if f.posts != 0 || f.state.resetCredits[f.auth.ID].AutoReason != "reset_credit_journal_invalid" {
		t.Fatal(f.state.resetCredits)
	}
}

func TestResetCreditNewNativeCountInvalidatesOlderListDisplay(t *testing.T) {
	f := newResetCreditFixture(t)
	f.tick()
	q := f.state.quotas[f.auth.ID]
	q.ResetCredits, q.ResetCreditsObservedAt = 0, time.Now().Add(time.Millisecond)
	f.state.quotas[f.auth.ID], f.state.quotas[f.auth.AuthIndex] = q, q
	status := f.state.status().Snapshots[0]
	if status.ResetCredits != 0 || status.ResetCreditDetails.ListFresh {
		t.Fatal("old list overrode new official count", status)
	}
}

func TestResetCreditCooldownRecoveryIsScopedAndKeepsNewerBans(t *testing.T) {
	for _, name := range []string{"recovered", "still-exhausted", "newer-ban", "probation"} {
		t.Run(name, func(t *testing.T) {
			resetBanStoreForTest()
			t.Cleanup(resetBanStoreForTest)
			f := newResetCreditFixture(t)
			f.state.cfg.ResetCreditsAutoUse = true
			f.used = 0
			before := banEntry{Kind: banKindQuota, Phase: banPhaseCooldown, Window: "5h", BannedAt: time.Now().Add(-time.Minute), ResetAt: time.Now().Add(time.Hour)}
			banStore.set(f.auth.ID, before)
			banStore.set("unrelated", before)
			attemptAt := time.Now().Add(-time.Second)
			switch name {
			case "still-exhausted":
				f.used = 100
			case "newer-ban":
				fresh := before
				fresh.BannedAt = time.Now()
				banStore.set(f.auth.ID, fresh)
			case "probation":
				fresh := before
				fresh.Kind = banKindProbation
				banStore.set(f.auth.ID, fresh)
			}
			calls := 0
			call := func(method string, payload any) (json.RawMessage, error) {
				calls++
				req, ok := payload.(pluginapi.HostRoutingResetCooldownRequest)
				if !ok || method != pluginabi.MethodHostRoutingResetCooldown || req.AuthIndex != f.auth.AuthIndex {
					t.Fatal("unscoped cooldown reset")
				}
				return json.RawMessage(`{"auth_index":"index","models":[]}`), nil
			}
			ok := f.state.verifyResetCreditQuotaRecovery(context.Background(), f.state.cfg, f.state.configGeneration, f.auth, before, true, attemptAt, call)
			_, stillBanned := banStore.lookup(f.auth.ID)
			if name == "recovered" {
				if !ok || calls != 1 || stillBanned {
					t.Fatal("recovered quota stayed blocked", ok, calls, stillBanned)
				}
			} else if ok || calls != 0 || !stillBanned {
				t.Fatal("unverified/newer cooldown cleared", name, ok, calls, stillBanned)
			}
			if _, exists := banStore.lookup("unrelated"); !exists {
				t.Fatal("unrelated credential changed")
			}
		})
	}
}

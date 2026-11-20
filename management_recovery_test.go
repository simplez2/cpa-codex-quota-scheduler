package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagementRecoveryUsesRefreshedCPACooldownsWithoutAnUpstreamRead(t *testing.T) {
	var requests atomic.Int32
	retryAt := time.Now().Add(time.Hour).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v0/management/auth-files":
			writeTestJSON(t, w, map[string]any{"files": []map[string]any{
				{"id": "host-only", "auth_index": "idx-host", "provider": "codex", "unavailable": true,
					"cooldowns": []cpaCooldown{{Scope: "model", ModelKey: "gpt-test", Reason: "quota", RetryAt: retryAt}}},
				{"id": "disabled", "auth_index": "idx-off", "provider": "codex", "disabled": true,
					"cooldowns": []cpaCooldown{{Scope: "credential", Reason: "quota", RetryAt: retryAt}}},
			}})
		case "/v0/management/api-call":
			writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: nativeQuotaTestBody(25)})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	s := newRefreshTestState(t, server.URL)
	s.refreshOnce(context.Background())
	before := requests.Load()
	got := s.managementRecovery(nil, time.Now())
	if got.NativeStatus != "current" || got.CanClearAll || len(got.Accounts) != 1 {
		t.Fatalf("missing native-only row or overbroad bulk action: %+v", got)
	}
	a := got.Accounts[0]
	if a.AuthID != "host-only" || !a.CanClear || a.Plugin != nil || len(a.CPA) != 1 || a.CPA[0].ModelKey != "gpt-test" || !a.CPA[0].RetryAt.Equal(retryAt) {
		t.Fatalf("missing the host model cooldown or account action: %+v", a)
	}
	if requests.Load() != before {
		t.Fatal("reading recovery status issued an upstream request")
	}
}

func TestManagementRecoveryDistinguishesUnknownHostFromEmptyAndShowsQuotaWait(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset := now.Add(time.Hour)
	cfg := defaultPluginConfig()
	files := []cpaAuthFileEntry{
		{ID: "older", AuthIndex: "legacy", Provider: "codex", Unavailable: true, NextRetryAfter: json.RawMessage(`"` + reset.Format(time.RFC3339) + `"`), Cooldowns: json.RawMessage(`{"unexpected":true}`)},
		{ID: "week", AuthIndex: "week-idx", Provider: "codex", Cooldowns: json.RawMessage(`[]`)},
		{ID: "guard", AuthIndex: "new-index", Provider: "codex", Cooldowns: json.RawMessage(`[]`)},
		{ID: "mixed", AuthIndex: "mixed-idx", Provider: "codex"},
	}
	files[3].Cooldowns, _ = json.Marshal([]cpaCooldown{
		{Scope: "model", ModelKey: "gpt-quota", Reason: "quota", RetryAt: reset},
		{Scope: "model", ModelKey: "gpt-other", Reason: "unauthorized", RetryAt: reset},
	})
	weekly := quotaSnapshot{AuthID: "week", AuthIndex: "week-idx", RefreshedAt: now,
		Windows: []quotaWindow{{Class: "weekly", UsedPercent: 100, Allowed: true, ObservedAt: now, ResetAt: reset}}}
	mixed, err := parseNativeQuota([]byte(nativeQuotaTestBody(25)), "mixed", "mixed-idx", now)
	if err != nil {
		t.Fatal(err)
	}
	got := buildManagementRecovery([]managementBanInfo{{AuthID: "mixed", State: "cooldown"}},
		map[string]recoveryEntry{"guard": {AuthIndex: "old-index", Ready: true, RetryAt: reset}}, quotaInventory(files),
		map[string]quotaSnapshot{"week": weekly, "mixed": mixed}, now, cfg, now)
	byID := map[string]managementRecoveryAccount{}
	for _, a := range got.Accounts {
		byID[a.AuthID] = a
	}
	if got.NativeStatus != "partial" || !byID["older"].CPAUnknown || byID["older"].CanClear || byID["mixed"].CanClear {
		t.Fatalf("missing host details or non-quota failures should not offer a CPA reset: %+v", got)
	}
	if q := byID["week"].Quota; len(q) != 1 || q[0].Class != "weekly" || q[0].ResetAt != reset.Format(time.RFC3339) || byID["week"].CanClear {
		t.Fatalf("weekly exhaustion should show the expected reset without offering a CPA clear: %+v", byID["week"])
	}
	if a := byID["guard"]; a.Guard != "awaiting_quota" || a.GuardUntil != "" || a.CanClear {
		t.Fatalf("a replaced credential must await its own quota observation: %+v", a)
	}
	current := buildManagementRecovery(nil, nil, quotaInventory(files[1:2]), nil, now, cfg, now)
	stale := buildManagementRecovery(nil, nil, quotaInventory(files[1:2]), nil, now.Add(-10*time.Minute), cfg, now)
	if current.NativeStatus != "current" || stale.NativeStatus != "stale" || len(current.Accounts) != 0 || len(stale.Accounts) != 0 {
		t.Fatalf("expected distinct empty and stale results: %+v versus %+v", current, stale)
	}
}

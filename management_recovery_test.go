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

func TestManagementRecoveryKeepsCachedExhaustionThroughResetUntilReobserved(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	observed := now.Add(-cfg.StaleAfter - time.Minute)
	reset := now.Add(time.Hour)
	inventory := quotaInventory([]cpaAuthFileEntry{
		{ID: "idle", AuthIndex: "idx-idle", Provider: "codex", Cooldowns: json.RawMessage(`[]`)},
	})
	q := quotaSnapshot{AuthID: "idle", AuthIndex: "idx-idle", RefreshedAt: observed,
		Windows: []quotaWindow{
			// The usage endpoint can repeat the same account-wide restriction
			// flags for both windows even when only the weekly one is full.
			{Class: "5h", Allowed: false, LimitReached: true, UsedPercent: 0, ObservedAt: observed, ResetAt: now.Add(4 * time.Hour)},
			{Class: "weekly", Allowed: false, LimitReached: true, UsedPercent: 100, ObservedAt: observed, ResetAt: reset},
		},
	}
	quotas := map[string]quotaSnapshot{"idle": q}
	assertWeekly := func(at, inventoryAt time.Time, state, nativeStatus string) {
		t.Helper()
		got := buildManagementRecovery(nil, nil, inventory, quotas, inventoryAt, cfg, at)
		if got.NativeStatus != nativeStatus || len(got.Accounts) != 1 || got.CanClearAll || got.Accounts[0].CanClear {
			t.Fatalf("expected one read-only recovery account: %+v", got)
		}
		windows := got.Accounts[0].Quota
		if len(windows) != 1 || windows[0].Class != "weekly" || windows[0].State != state || !windows[0].Cached ||
			windows[0].ObservedAt != observed.Format(time.RFC3339) || windows[0].ResetAt != reset.Format(time.RFC3339) {
			t.Fatalf("expected cached weekly exhaustion without a false 5h attribution: %+v", windows)
		}
	}
	assertWeekly(now, now, "exhausted", "current")
	assertWeekly(now, now.Add(-3*time.Minute), "exhausted", "stale")
	afterReset := reset.Add(time.Second)
	assertWeekly(afterReset, afterReset, "awaiting_quota", "current")

	// Confirm an upstream refill. A separate soft handoff threshold is not a
	// recovery wait, even if that threshold still excludes another account.
	q.RefreshedAt = afterReset
	q.Windows = []quotaWindow{{Class: "weekly", UsedPercent: 98, Allowed: true, ObservedAt: afterReset, ResetAt: reset.Add(7 * 24 * time.Hour)}}
	quotas["idle"] = q
	if got := buildManagementRecovery(nil, nil, inventory, quotas, afterReset, cfg, afterReset); len(got.Accounts) != 0 {
		t.Fatalf("confirmed active window or soft threshold left a false recovery row: %+v", got)
	}

	// Replacing a credential cannot carry forward the prior index's depletion.
	auth := inventory["idle"]
	auth.AuthIndex = "idx-replacement"
	inventory["idle"] = auth
	quotas["idle"] = quotaSnapshot{AuthID: "idle", AuthIndex: "idx-idle", RefreshedAt: observed,
		Windows: []quotaWindow{{Class: "weekly", UsedPercent: 100, Allowed: true, ResetAt: reset}}}
	if got := buildManagementRecovery(nil, nil, inventory, quotas, now, cfg, now); len(got.Accounts) != 0 {
		t.Fatalf("replacement inherited a cooldown from the prior credential: %+v", got)
	}
}

func TestManagementRecoveryShowsUnattributedRestrictionWithoutInventingWindow(t *testing.T) {
	now := time.Now().UTC()
	q := quotaSnapshot{RefreshedAt: now, Windows: []quotaWindow{
		{Class: "5h", UsedPercent: 20, LimitReached: true, ResetAt: now.Add(time.Hour)},
		{Class: "weekly", UsedPercent: 70, LimitReached: true, ResetAt: now.Add(24 * time.Hour)},
	}}
	got := managementRecoveryWindows(q, now, time.Minute)
	if len(got) != 1 || got[0].State != "restricted" || got[0].Class != "" || got[0].ResetAt != "" || got[0].Cached {
		t.Fatalf("shared restriction must not claim a specific quota window was exhausted: %+v", got)
	}
}

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestNativeQuotaPlanResolution(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		raw, plan string
		weight    float64
	}{
		{"team", "team_standard", 1}, {"plus", "plus", 1},
		{"self_serve_business_prolite", "team_premium", 5},
		{"business", "team_standard", 1}, {"pro", "team_standard", 1},
		{"self_serve_business_usage_based", "team_standard", 1}, {"unknown_future", "team_standard", 1},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			cfg := defaultPluginConfig()
			q := quotaSnapshot{AuthID: "a", AuthIndex: "idx", Plan: quotaPlanObservation{Type: tc.raw, Source: "cpa_usage", ObservedAt: now}}
			plan, weight, source := resolvedQuotaPlan(cfg, "a", q, now)
			if plan != tc.plan || weight != tc.weight {
				t.Fatalf("resolution: %s %v %s", plan, weight, source)
			}
			if _, recognized := nativeQuotaPlan(tc.raw); (source == "cpa_usage") != recognized {
				t.Fatalf("source=%s", source)
			}
			cfg.QuotaAccountPlans = map[string]string{"a": "pro_20x"}
			if p, w, s := resolvedQuotaPlan(cfg, "a", q, now); p != "pro_20x" || w != 20 || s != "account_override" {
				t.Fatalf("override: %s %v %s", p, w, s)
			}
			cfg.QuotaAccountPlans = nil
			cfg.QuotaDefaultPlan = "pro_5x"
			cfg.QuotaProbeOnDemand = false
			for _, at := range []time.Time{now.Add(cfg.StaleAfter + time.Second), now.Add(-time.Second)} {
				if p, w, s := resolvedQuotaPlan(cfg, "a", q, at); p != "pro_5x" || w != 5 || s != "default" {
					t.Fatalf("stale/future metadata: %s %v %s", p, w, s)
				}
			}
		})
	}
}

func TestNativeQuotaPlanRefreshUsesExistingQueriesAndClearsRemovedMetadata(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	for _, tc := range []struct{ usage, auth, want, source string }{
		{`"self_serve_business_prolite"`, "team", "self_serve_business_prolite", "cpa_usage"},
		{`null`, "self_serve_business_prolite", "self_serve_business_prolite", "cpa_auth_files"},
		{`{"malformed":true}`, "team", "team", "cpa_auth_files"},
		{`"unknown_future"`, "team", "unknown_future", "cpa_usage"},
	} {
		t.Run(tc.usage, func(t *testing.T) {
			calls := 0
			clear := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/auth-files") {
					label := tc.auth
					if clear {
						label = ""
					}
					writeTestJSON(t, w, map[string]any{"files": []map[string]any{{"id": "a", "auth_index": "idx", "provider": "codex", "id_token": map[string]string{"plan_type": label}}}})
					return
				}
				calls++
				body := nativeQuotaTestBody(10)
				if !clear {
					body = `{"account_id":"account-a","plan_type":` + tc.usage + `,` + body[1:]
				}
				writeTestJSON(t, w, cpaAPICallResponse{StatusCode: 200, Body: body})
			}))
			defer server.Close()
			s := newRefreshTestState(t, server.URL)
			s.refreshOnce(context.Background())
			q := s.quotas["a"]
			if q.Plan.Type != tc.want || q.Plan.Source != tc.source || q.Plan.ObservedAt.IsZero() || q.AccountID != "account-a" {
				t.Fatalf("observation: %+v", q.Plan)
			}
			s.refreshOnce(context.Background())
			if calls != 1 || s.quotas["a"].Plan != q.Plan {
				t.Fatal("cached read made another query or renewed plan age")
			}
			clear = true
			s.quotaPolls["a"] = quotaPollState{AuthIndex: "idx"}
			s.refreshOnce(context.Background())
			if s.quotas["a"].Plan.Type != "" || calls != 2 {
				t.Fatal("removed native metadata retained old capacity")
			}
		})
	}
}

func TestNativeQuotaPlanMergeDoesNotRenewOrCrossAccountBinding(t *testing.T) {
	now := time.Now()
	prior := quotaSnapshot{AuthID: "a", AuthIndex: "idx", RefreshedAt: now.Add(-time.Minute), Plan: quotaPlanObservation{Type: "self_serve_business_prolite", Source: "cpa_usage", ObservedAt: now.Add(-time.Minute)}}
	next := quotaSnapshot{AuthID: "a", AuthIndex: "idx", RefreshedAt: now}
	merged := mergePartialQuotaSnapshot(prior, next, now, 15*time.Minute)
	if merged.Plan != prior.Plan {
		t.Fatal("quota-only headers erased or renewed native plan")
	}
	next.Plan.ObservedAt = now
	if q := mergePartialQuotaSnapshot(prior, next, now, 15*time.Minute); q.Plan.Type != "" {
		t.Fatal("empty newer observation did not clear plan")
	}
	next.Plan = quotaPlanObservation{}
	next.AuthIndex = "replaced"
	if q := mergePartialQuotaSnapshot(prior, next, now, 15*time.Minute); q.Plan.Type != "" {
		t.Fatal("replacement auth inherited native plan")
	}
	// A retained observation still expires on its original clock.
	copy := prior.Plan
	if copy.fresh(now.Add(16*time.Minute), 15*time.Minute) {
		t.Fatal("plan remains fresh indefinitely")
	}
}

func TestNativePremiumWeightsReachBalancedSerialAndPanel(t *testing.T) {
	resetBanStoreForTest()
	t.Cleanup(resetBanStoreForTest)
	now := time.Now()
	s, req := balancedFixture(now)
	for id, label := range map[string]string{"a": "team", "b": "self_serve_business_prolite"} {
		q := s.quotas[id]
		q.Plan = quotaPlanObservation{Type: label, Source: "cpa_usage", ObservedAt: now}
		s.quotas[id] = q
	}
	for i := 0; i < 600; i++ {
		s.balancedPick(req, now)
	}
	if s.balancedAccounts["a"].Picks != 100 || s.balancedAccounts["b"].Picks != 500 {
		t.Fatalf("automatic capacity shares: %+v", s.balancedStatusLocked(now))
	}
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: "b"}, s.quotas["b"], true, s.cfg, now)
	s.annotateSerialCandidateLocked(&choice, now)
	if choice.Plan != "team_premium" || choice.PlanWeight != 5 {
		t.Fatalf("serial capacity: %+v", choice)
	}
	found := false
	for _, q := range s.status().Snapshots {
		if q.AuthID == "b" {
			found = true
		}
		if q.AuthID == "b" && (q.Plan != "team_premium" || q.PlanWeight != 5 || q.PlanSource != "cpa_usage" || !q.UpstreamPlanFresh) {
			t.Fatalf("panel capacity: %+v", q)
		}
	}
	if !found {
		t.Fatal("premium account absent from panel")
	}
}

func TestPanelIgnoresRemovedWarmupKeysInPersistedConfig(t *testing.T) {
	changes, errs := validatePanelSettings(map[string]any{"warmup_execution_mode": "native", "warmup_sidecar_url": "http://removed.invalid"}, map[string]any{"warmup_enabled": true})
	if len(errs) != 0 || changes["warmup_enabled"] != true {
		t.Fatalf("legacy config prevents panel save: %v %v", changes, errs)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func adqCircuitTestState(now time.Time) (*schedulerRuntimeState, pluginapi.SchedulerPickRequest) {
	cfg := defaultPluginConfig()
	cfg.SchedulerMode = "balanced"
	cfg.StatePath = ""
	snapshot := quotaSnapshot{
		AuthID: "canonical-a", AuthIndex: "idx-a", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", WindowSeconds: 18000, UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
			{Class: "weekly", WindowSeconds: 604800, UsedPercent: 20, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now},
		},
	}
	s := &schedulerRuntimeState{
		cfg:                 cfg,
		quotas:              map[string]quotaSnapshot{"canonical-a": snapshot, "idx-a": snapshot, "canonical-b": {AuthID: "canonical-b", AuthIndex: "idx-b", RefreshedAt: now, Windows: snapshot.Windows}},
		identities:          map[string]string{"idx-a": "canonical-a", "idx-b": "canonical-b"},
		adqProviderCircuits: make(map[string]adqProviderCircuit),
	}
	req := pluginapi.SchedulerPickRequest{
		Provider: providerCodex,
		Model:    "gpt-5.6-luna",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "canonical-a", Provider: providerCodex, Attributes: map[string]string{"auth_index": "idx-a"}},
			{ID: "canonical-b", Provider: providerCodex, Attributes: map[string]string{"auth_index": "idx-b"}},
		},
	}
	return s, req
}

func TestADQProviderCircuitFiltersBalancedFallbackAndRecovers(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := adqCircuitTestState(now)
	s.adqProviderCircuits["canonical-a"] = adqProviderCircuit{State: adqProviderOverload, RetryAt: now.Add(time.Minute), LastStatus: 503}
	got, err := s.schedulerPick(req)
	if err != nil || !got.Handled || got.AuthID != "canonical-b" {
		t.Fatalf("active overload was not filtered: got=%#v err=%v", got, err)
	}
	if _, ok := banStore.lookup("canonical-a"); ok {
		t.Fatal("provider overload must not create a quota ban")
	}

	s.adqProviderCircuits["canonical-a"] = adqProviderCircuit{State: adqProviderOverload, RetryAt: now.Add(-time.Second), LastStatus: 503}
	filtered := s.filterADQProviderCircuits(req.Candidates, now)
	if len(filtered) != 2 {
		t.Fatalf("expired overload circuit did not reopen: %#v", filtered)
	}
	if _, ok := s.adqProviderCircuits["canonical-a"]; ok {
		t.Fatal("expired overload circuit was not removed")
	}
}

func TestADQProviderAuthCircuitFiltersAndExpires(t *testing.T) {
	now := time.Now()
	s, req := adqCircuitTestState(now)
	s.adqProviderCircuits["canonical-a"] = adqProviderCircuit{State: adqProviderAuth, RetryAt: now.Add(time.Minute), LastStatus: 401}
	filtered := s.filterADQProviderCircuits(req.Candidates, now)
	if len(filtered) != 1 || filtered[0].ID != "canonical-b" {
		t.Fatalf("active auth circuit was not filtered: %#v", filtered)
	}
	s.adqProviderCircuits["canonical-a"] = adqProviderCircuit{State: adqProviderAuth, RetryAt: now.Add(-time.Second), LastStatus: 401}
	if got := s.filterADQProviderCircuits(req.Candidates, now); len(got) != 2 {
		t.Fatalf("expired auth circuit did not reopen: %#v", got)
	}
}

func TestADQ503UsageLimitWrapperDoesNotQuarantine(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, _ := adqCircuitTestState(now)
	s.observeADQProviderOutcome(pluginapi.UsageRecord{
		Provider: providerCodex, AuthID: "canonical-a", AuthIndex: "idx-a", Generate: true, Failed: true,
		Failure: pluginapi.UsageFailure{StatusCode: 503, Body: `auth_unavailable: usage_limit_reached`},
	}, now)
	circuit := s.adqProviderCircuits["canonical-a"]
	if circuit.State != adqProviderOverload || circuit.LastStatus != 503 {
		t.Fatalf("503 wrapper classified incorrectly: %#v", circuit)
	}
	if _, ok := banStore.lookup("canonical-a"); ok {
		t.Fatal("503 usage-limit wrapper must not create a quota ban")
	}
}

func TestADQCanonicalAuthIDBindsCapacityAndReturnsCPAID(t *testing.T) {
	now := time.Now()
	snapshot := quotaSnapshot{
		AuthID: "canonical", AuthIndex: "idx-canonical", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", WindowSeconds: 18000, UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
			{Class: "weekly", WindowSeconds: 604800, UsedPercent: 20, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now},
		},
	}
	s := &schedulerRuntimeState{
		cfg:        defaultPluginConfig(),
		quotas:     map[string]quotaSnapshot{"canonical": snapshot, "idx-canonical": snapshot},
		identities: map[string]string{"idx-canonical": "canonical"},
		pacingAccounts: map[string]*accountPacingState{
			"canonical": {Capacities: map[string]capacityEstimate{
				"5h": {Credits: 16, Samples: 2}, "weekly": {Credits: 100, Samples: 2},
			}, LastQuota: make(map[string]quotaCalibrationPoint)},
		},
		balancedAccounts:    make(map[string]*balancedAccount),
		adqReservations:     newADQReservationBook(),
		adqProviderCircuits: make(map[string]adqProviderCircuit),
	}
	choice := serialCandidate{
		Candidate: pluginapi.SchedulerAuthCandidate{ID: "cpa-file-id", Provider: providerCodex, Attributes: map[string]string{"auth_index": "idx-canonical"}},
		Snapshot:  snapshot, QuotaKnown: true, WeeklyKnown: true, FiveHourKnown: true,
		WeeklyRemaining: 80, FiveHourRemaining: 90,
	}
	s.mu.Lock()
	selected, reservation, ok := s.adqRouteChoicesLocked(pluginapi.SchedulerPickRequest{Provider: providerCodex, Model: "gpt-5.6-luna"}, []serialCandidate{choice}, 1, now, "", "")
	s.mu.Unlock()
	if !ok || selected != "cpa-file-id" {
		t.Fatalf("canonical mapping did not return CPA candidate ID: selected=%q reservation=%#v ok=%v", selected, reservation, ok)
	}
	if reservation.AuthID != "canonical" {
		t.Fatalf("reservation used non-canonical ID: %#v", reservation)
	}
	if got, _ := s.adqReservations.Reserved("canonical", now); got <= 0 {
		t.Fatalf("canonical reservation was not recorded: %#v", reservation)
	}
}

func TestManagementQuotaStatusJSONIncludesADQFields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cfg := defaultPluginConfig()
	cfg.Enabled = true
	cfg.SchedulerMode = "balanced"
	cfg.StatePath = ""
	snapshot := quotaSnapshot{
		AuthID: "canonical-a", AuthIndex: "idx-a", RefreshedAt: now,
		Windows: []quotaWindow{
			{Class: "5h", WindowSeconds: 18000, UsedPercent: 10, Allowed: true, ResetAt: now.Add(4 * time.Hour), ObservedAt: now},
			{Class: "weekly", WindowSeconds: 604800, UsedPercent: 20, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now},
		},
	}
	state := schedulerRuntimeState{
		cfg:        cfg,
		quotas:     map[string]quotaSnapshot{"canonical-a": snapshot, "idx-a": snapshot},
		identities: map[string]string{"idx-a": "canonical-a"},
		pacingAccounts: map[string]*accountPacingState{"canonical-a": {
			Capacities: map[string]capacityEstimate{
				"5h":     {Credits: 16, Samples: 2, UpdatedAt: now},
				"weekly": {Credits: 100, Samples: 2, UpdatedAt: now},
			},
			LastQuota: make(map[string]quotaCalibrationPoint),
		}},
		adqPolicy:                defaultADQPolicy(),
		adqReservations:          newADQReservationBook(),
		adqReservationCollisions: 7,
		adqProviderCircuits: map[string]adqProviderCircuit{
			"canonical-a": {State: adqProviderOverload, Failures: 2, LastStatus: 503, LastAt: now.Add(-time.Second), RetryAt: now.Add(time.Minute)},
		},
		warmups: make(map[string]warmupEntry),
	}
	state.adqReservations.SetCapacity("canonical-a", 16, 100)
	if _, ok := state.adqReservations.TryReserve(adqReservationRequest{AuthID: "canonical-a", SessionKey: "session", Model: "gpt-5.6-luna", FiveHour: 1, Weekly: 2, TTL: time.Minute}, now); !ok {
		t.Fatal("failed to seed active ADQ reservation")
	}
	response := jsonManagementResponse(http.StatusOK, state.status())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var got runtimeStatus
	if err := json.Unmarshal(response.Body, &got); err != nil {
		t.Fatalf("decode /quota response: %v body=%s", err, response.Body)
	}
	if !got.ADQ.Enabled || got.ADQ.UsingFallback || got.ADQ.ReservationsActive != 1 || got.ADQ.ReservationCollisions != 7 {
		t.Fatalf("ADQ pool status missing: %#v", got.ADQ)
	}
	circuit, ok := got.ADQ.ProviderCircuits["canonical-a"]
	if !ok || !circuit.Active || circuit.RetryAt == "" || circuit.LastStatus != 503 {
		t.Fatalf("provider circuit status missing: %#v", got.ADQ.ProviderCircuits)
	}
	if got.ADQ.Pool.EffectiveWidth <= 0 || got.ADQ.Pool.Bottleneck != "TAIL_DRAIN" {
		t.Fatalf("ADQ pool metrics missing: %#v", got.ADQ.Pool)
	}
	if len(got.Snapshots) != 1 || got.Snapshots[0].ADQ == nil || got.Snapshots[0].ADQ.ProviderRetryAt.IsZero() {
		t.Fatalf("per-account ADQ status missing: %#v", got.Snapshots)
	}
}

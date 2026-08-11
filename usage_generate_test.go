package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestUsageRecordGenerateCompatibility(t *testing.T) {
	if !usageRecordGenerateEnabled([]byte(`{"Provider":"codex"}`)) {
		t.Fatal("legacy usage record without Generate must default to enabled")
	}
	if !usageRecordGenerateEnabled([]byte(`{"Provider":"codex","Generate":true}`)) {
		t.Fatal("explicit Generate=true was disabled")
	}
	if usageRecordGenerateEnabled([]byte(`{"Provider":"codex","Generate":false}`)) {
		t.Fatal("explicit Generate=false was enabled")
	}
}

func TestGenerateFalseObserveUsageHasNoSideEffects(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	snapshot := quotaSnapshot{
		AuthID: "acct", AuthIndex: "idx", RefreshedAt: now,
		Windows: []quotaWindow{{
			Class: "weekly", WindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			UsedPercent: 25, Allowed: true, ResetAt: now.Add(6 * 24 * time.Hour), ObservedAt: now,
		}},
	}
	account := &accountPacingState{
		DeficitCredits:   7,
		PendingPredicted: []float64{3},
		Capacities:       map[string]capacityEstimate{"weekly": {Credits: 100, Samples: 2, UpdatedAt: now}},
		LastQuota:        make(map[string]quotaCalibrationPoint),
	}
	state := schedulerRuntimeState{
		cfg:               defaultPluginConfig(),
		quotas:            map[string]quotaSnapshot{"acct": snapshot, "idx": snapshot},
		identities:        map[string]string{"idx": "acct"},
		pricing:           map[string]modelPricing{"gpt-test": builtInFallbackPricing("gpt-test")},
		pacingAccounts:    map[string]*accountPacingState{"acct": account},
		costSamples:       map[string][]float64{"sentinel": {1, 2}},
		globalCostSamples: []float64{4, 5},
	}

	wantSnapshot := state.quotas["acct"]
	wantAccount := *account
	wantAccount.PendingPredicted = append([]float64(nil), account.PendingPredicted...)
	wantAccount.Capacities = map[string]capacityEstimate{"weekly": account.Capacities["weekly"]}
	wantSamples := append([]float64(nil), state.globalCostSamples...)

	state.observeUsage(pluginapi.UsageRecord{
		Provider: providerCodex, AuthID: "acct", AuthIndex: "idx", Model: "gpt-test",
		Generate: false, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: statusTooManyRequests},
		Detail: pluginapi.UsageDetail{InputTokens: 1000, OutputTokens: 1000},
		ResponseHeaders: http.Header{
			"X-Codex-Secondary-Window-Minutes": {"10080"},
			"X-Codex-Secondary-Used-Percent":   {"99"},
		},
	})

	if got := state.quotas["acct"]; !reflect.DeepEqual(got, wantSnapshot) {
		t.Fatalf("generate=false changed quota snapshot: got=%#v want=%#v", got, wantSnapshot)
	}
	if !reflect.DeepEqual(*state.pacingAccounts["acct"], wantAccount) {
		t.Fatalf("generate=false changed pacing state: got=%#v want=%#v", *state.pacingAccounts["acct"], wantAccount)
	}
	if !reflect.DeepEqual(state.globalCostSamples, wantSamples) || len(state.costSamples) != 1 {
		t.Fatalf("generate=false changed cost samples: global=%v profiles=%v", state.globalCostSamples, state.costSamples)
	}
}

func TestGenerateFalseHandleUsageDoesNotChangeQuarantineOrProbe(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now().UTC()
	banStore.set("probing", banEntry{
		ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation,
		Phase: banPhaseHalfOpen, ProbeStartedAt: now.Add(-time.Second), ProbeLeaseUntil: now.Add(time.Minute), ProbeAttempts: 1,
	})
	wantProbe, _ := banStore.lookup("probing")

	for _, record := range []pluginapi.UsageRecord{
		{Provider: providerCodex, AuthID: "probing", Generate: false, RequestedAt: now, Failed: false},
		{Provider: providerCodex, AuthID: "new-429", Generate: false, RequestedAt: now, Failed: true, Failure: pluginapi.UsageFailure{StatusCode: statusTooManyRequests}},
	} {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = handleUsage(raw); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := banStore.lookup("probing"); !ok || !reflect.DeepEqual(got, wantProbe) {
		t.Fatalf("generate=false completed a half-open probe: got=%#v want=%#v", got, wantProbe)
	}
	if _, ok := banStore.lookup("new-429"); ok {
		t.Fatal("generate=false 429 created a quarantine entry")
	}
}

func TestGenerateFalseSchedulerPickBypassesAllState(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now().UTC()
	banStore.set("acct", banEntry{ResetAt: now.Add(-time.Minute), Window: "probation", Kind: banKindProbation, Phase: banPhaseCooldown})
	wantBan, _ := banStore.lookup("acct")
	state := schedulerRuntimeState{
		cfg:             defaultPluginConfig(),
		pacingAccounts:  make(map[string]*accountPacingState),
		stickyBindings:  make(map[string]stickyBinding),
		decisionHistory: make([]schedulerDecisionAudit, 0),
		warmups:         make(map[string]warmupEntry),
	}
	state.cfg.SchedulerMode = "enforce"
	req := pluginapi.SchedulerPickRequest{
		Provider:   providerCodex,
		Options:    pluginapi.SchedulerOptions{Metadata: map[string]any{"generate": false}},
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "acct", Provider: providerCodex}},
	}
	got, err := state.schedulerPick(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handled {
		t.Fatalf("generate=false scheduler response = %#v; want CPA fallthrough", got)
	}
	if gotBan, ok := banStore.lookup("acct"); !ok || !reflect.DeepEqual(gotBan, wantBan) {
		t.Fatalf("generate=false started a probe: got=%#v want=%#v", gotBan, wantBan)
	}
	if len(state.pacingAccounts) != 0 || len(state.stickyBindings) != 0 || len(state.decisionHistory) != 0 {
		t.Fatalf("generate=false mutated scheduler state: pacing=%v sticky=%v audit=%v", state.pacingAccounts, state.stickyBindings, state.decisionHistory)
	}
}

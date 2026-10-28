package main

import (
	"testing"
	"time"
)

func TestBalancedRecoveredHostErrorsHonorWeeklyWeightsAndThreshold(t *testing.T) {
	resetBanStoreForTest()
	defer resetBanStoreForTest()
	now := time.Now()
	s, req := balancedAllocationFixture(now, true)
	s.cfg.Serial5hHandoffMode = "custom_threshold"
	s.cfg.Serial5hSwitchPercent = 80
	for i := range req.Candidates {
		req.Candidates[i].Status = "error"
	}
	// Both credentials have the same deadlines: b's fuller weekly reservoir
	// must remain selectable even though CPA retained a historical error.
	for _, id := range []string{"a", "b"} {
		q := s.quotas[id]
		q.Windows[0].UsedPercent = 10
		q.Windows[0].ResetAt = now.Add(4 * time.Hour)
		q.Windows[1].ResetAt = now.Add(6 * 24 * time.Hour)
		q.Windows[1].UsedPercent = 60
		if id == "b" {
			q.Windows[1].UsedPercent = 0
		}
		s.quotas[id] = q
	}
	req = withBalancedSession(req, "recovered-session")
	first := s.balancedPick(req, now)
	if !first.Handled || first.AuthID != "b" {
		t.Fatalf("recovered candidates bypassed weekly allocation: %+v", first)
	}
	q := s.quotas["b"]
	q.Windows[0].UsedPercent = 80
	s.quotas["b"] = q
	replacement := s.balancedPick(req, now.Add(time.Second))
	if !replacement.Handled || replacement.AuthID != "a" {
		t.Fatalf("recovered sticky candidate bypassed 80 percent handoff: %+v", replacement)
	}
	if s.adqDecisions != 2 || s.balancedSessionSwitches != 1 {
		t.Fatalf("routing did not reserve and rebind: decisions=%d switches=%d", s.adqDecisions, s.balancedSessionSwitches)
	}
}

func TestRecoveredHostErrorDoesNotBypassCurrentBlocks(t *testing.T) {
	for _, block := range []string{"disabled", "hard_quota", "provider_auth", "provider_overload"} {
		t.Run(block, func(t *testing.T) {
			resetBanStoreForTest()
			defer resetBanStoreForTest()
			now := time.Now()
			s, req := balancedAllocationFixture(now, true)
			for i := range req.Candidates {
				req.Candidates[i].Status = "error"
				if req.Candidates[i].ID == "a" && block == "disabled" {
					req.Candidates[i].Status = "disabled"
				}
			}
			s.adqProviderCircuits = make(map[string]adqProviderCircuit)
			switch block {
			case "hard_quota":
				q := s.quotas["a"]
				q.Windows[0].UsedPercent = 100
				s.quotas["a"] = q
			case "provider_auth":
				s.adqProviderCircuits["a"] = adqProviderCircuit{State: adqProviderAuth, RetryAt: now.Add(time.Minute)}
			case "provider_overload":
				s.adqProviderCircuits["a"] = adqProviderCircuit{State: adqProviderOverload, RetryAt: now.Add(time.Minute)}
			}
			got := s.balancedPick(req, now)
			if !got.Handled || got.AuthID != "b" {
				t.Fatalf("recovery bypassed %s: %+v", block, got)
			}
		})
	}
}

func TestQuotaPanelReportsConfiguredHandoffThreshold(t *testing.T) {
	now := time.Now()
	s, _ := balancedFixture(now)
	s.cfg.Serial5hHandoffMode = "custom_threshold"
	s.cfg.Serial5hSwitchPercent = 80
	q := s.quotas["a"]
	q.Windows[0].UsedPercent = 80
	s.quotas["a"] = q
	for _, account := range s.status().Snapshots {
		if account.AuthID == "a" && (account.Eligible || account.Reason != "serial_threshold") {
			t.Fatalf("threshold account was advertised as usable: %+v", account)
		}
	}
	// Explicit legacy continuation remains part of the existing contract.
	s.cfg.SerialSoftContinuation = true
	for _, account := range s.status().Snapshots {
		if account.AuthID == "a" && !account.Eligible {
			t.Fatalf("explicit continuation was hidden by the panel: %+v", account)
		}
	}
}

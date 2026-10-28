package main

import (
	"testing"
	"time"
)

func TestContinuityConfigDefaultsAndBounds(t *testing.T) {
	cfg, err := parsePluginConfig(nil)
	if err != nil || !cfg.ContinuityEnabled || !cfg.ContinuityPacing || cfg.ContinuityMaxWait != 6*time.Hour || cfg.ContinuityPollInterval != 30*time.Second {
		t.Fatalf("unexpected continuity defaults: %+v, %v", cfg, err)
	}
	for _, raw := range []string{
		"continuity_max_wait: 0s", "continuity_max_wait: -1s", "continuity_max_wait: 24h1s",
		"continuity_max_wait: infinite", "continuity_poll_interval: 0s", "continuity_poll_interval: 301s",
		"continuity_poll_interval: 999999999999999999999h", "continuity_enabled: []",
	} {
		if _, err := parsePluginConfig([]byte(raw)); err == nil {
			t.Errorf("accepted unsafe/unbounded wait: %s", raw)
		}
	}
	for _, raw := range []string{
		"continuity_max_wait: 1s\ncontinuity_poll_interval: 1s",
		"continuity_max_wait: 24h\ncontinuity_poll_interval: 5m",
	} {
		if _, err := parsePluginConfig([]byte(raw)); err != nil {
			t.Errorf("valid boundary rejected: %s: %v", raw, err)
		}
	}
}

func TestContinuityPanelChangesPreserveRoutingAndConcurrency(t *testing.T) {
	before := map[string]any{
		"account_max_concurrency": 2, "reserve_5h_percent": 0.,
		"serial_5h_handoff_mode": "429_only", "continuity_enabled": true,
	}
	changes, fields := validatePanelSettings(before, map[string]any{
		"continuity_enabled": false, "continuity_pacing": false,
		"continuity_max_wait": "8h", "continuity_poll_interval": "45s",
	})
	if len(fields) != 0 || len(changes) != 4 || changes["continuity_enabled"] != false || changes["continuity_max_wait"] != "8h0m0s" {
		t.Fatalf("continuity patch failed: changes=%v fields=%v", changes, fields)
	}
	for _, name := range []string{"account_max_concurrency", "reserve_5h_percent", "serial_5h_handoff_mode"} {
		if _, changed := changes[name]; changed {
			t.Fatalf("continuity update unexpectedly changed %s", name)
		}
	}
	for _, invalid := range []map[string]any{
		{"continuity_max_wait": "25h"}, {"continuity_poll_interval": "0s"},
		{"continuity_enabled": "yes"}, {"continuity_pacing": 1},
	} {
		if _, fields := validatePanelSettings(before, invalid); len(fields) == 0 {
			t.Fatalf("panel accepted invalid continuity settings: %v", invalid)
		}
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestPanelSettingsRejectInvalidOrSilentlyNormalizedValues(t *testing.T) {
	for name, value := range map[string]any{
		"reserve_5h_percent": 100., "refresh_interval": "0s", "stale_after": "1s",
		"quota_refresh_batch": 101., "quota_refresh_cooldown": "1s", "warmup_min_interval": "1s",
		"warmup_max_per_day": 0., "warmup_model": "bad token\nsecret", "scheduler_mode": "typo",
		"serial_5h_handoff_mode": "typo", "window_order": []string{"unknown"},
		"normal_cost_quantile": .99, "max_ban": "1m", "half_open_retry_after": "25h",
		"enabled": false, "store": map[string]any{"version": "9.9.9"},
		"cpa_management_url": "https://private-key@untrusted.test/api-call",
		"quota_url":          "https://example.test?token=private-key",
	} {
		t.Run(name, func(t *testing.T) {
			_, fields := validatePanelSettings(nil, map[string]any{name: value})
			if len(fields) == 0 {
				t.Fatalf("accepted invalid %s", name)
			}
			encoded, _ := json.Marshal(fields)
			if strings.Contains(string(encoded), "private-key") || strings.Contains(string(encoded), "secret") {
				t.Fatal("validation echoed private input")
			}
		})
	}
}

func TestPanelSettingsPreserveOmissionsAndZeroReserve(t *testing.T) {
	before := map[string]any{"reserve_5h_percent": 0., "serial_5h_handoff_mode": "429_only", "store": map[string]any{"version": "0.3.0"}}
	changes, errors := validatePanelSettings(before, map[string]any{"warmup_min_interval": "30m", "quota_account_plans": map[string]string{"auth": "pro_5x"}})
	if len(errors) != 0 || len(changes) != 2 || changes["warmup_min_interval"] != "30m0s" {
		t.Fatalf("changes=%v errors=%v", changes, errors)
	}
	if _, changed := changes["reserve_5h_percent"]; changed {
		t.Fatal("unrelated zero reserve was added to patch")
	}
	if before["reserve_5h_percent"] != 0. || before["serial_5h_handoff_mode"] != "429_only" {
		t.Fatal("validation mutated input configuration")
	}
}

func TestPanelSettingsExposePathsWithoutReadingCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.key")
	if err := os.WriteFile(path, []byte("private-management-key-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultPluginConfig()
	cfg.CPAManagementKeyFile = path
	encoded, err := json.Marshal(panelSettingsValues(cfg))
	if err != nil || strings.Contains(string(encoded), "private-management-key-fixture") {
		t.Fatal("settings returned credential contents")
	}
	if _, found := panelSettingsValues(cfg)["enabled"]; found {
		t.Fatal("runtime settings must not disable the panel's host lifecycle")
	}
}

func TestPanelSettingsValidationRejectsBadBodyAndDoesNotMutateRuntime(t *testing.T) {
	before, _ := json.Marshal(currentPanelSettings())
	for _, body := range [][]byte{nil, []byte("null"), []byte("{}"), []byte("{"), make([]byte, 129<<10)} {
		response := handleSettingsValidate(pluginapi.ManagementRequest{Body: body})
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad body accepted: %d", response.StatusCode)
		}
	}
	response := handleSettingsValidate(pluginapi.ManagementRequest{Body: []byte(`{"config":{},"changes":{"reserve_5h_percent":0,"serial_5h_handoff_mode":"429_only"}}`)})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid settings: %s", response.Body)
	}
	after, _ := json.Marshal(currentPanelSettings())
	if string(before) != string(after) {
		t.Fatal("validation changed running settings")
	}
}

func TestPanelControlsCoverEveryRuntimeSetting(t *testing.T) {
	source, err := os.ReadFile("web/settings.mjs")
	if err != nil {
		t.Fatal(err)
	}
	for key := range panelSettingsValues(defaultPluginConfig()) {
		if !strings.Contains(string(source), "['"+key+"',") {
			t.Errorf("no panel control for %s", key)
		}
	}
}

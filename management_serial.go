package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const managementSerialActiveBodyLimit = 4 << 10

type managementSerialActiveRequest struct {
	AuthID string
}

type managementSerialActiveError struct {
	Status  int
	Code    string
	Message string
}

func (e *managementSerialActiveError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func handleManagementSerialActivePut(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, err := decodeManagementSerialActiveRequest(req.Body)
	if err != nil {
		return managementSerialActiveErrorResponse(err)
	}
	status, err := schedulerRuntime.setManualSerialActive(body.AuthID, time.Now())
	if err != nil {
		return managementSerialActiveErrorResponse(err)
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"changed": status.changed,
		"status":  schedulerRuntime.status(),
	})
}

func handleManagementSerialActiveDelete(_ pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	status, err := schedulerRuntime.clearManualSerialActive(time.Now())
	if err != nil {
		return managementSerialActiveErrorResponse(err)
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"changed": status.changed,
		"status":  schedulerRuntime.status(),
	})
}

func decodeManagementSerialActiveRequest(raw []byte) (managementSerialActiveRequest, error) {
	if len(raw) == 0 || len(raw) > managementSerialActiveBodyLimit {
		return managementSerialActiveRequest{}, &managementSerialActiveError{
			Status: http.StatusBadRequest, Code: "invalid_body", Message: "auth_id is required",
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return managementSerialActiveRequest{}, &managementSerialActiveError{
			Status: http.StatusBadRequest, Code: "invalid_json", Message: "request body must be a JSON object",
		}
	}
	value, ok := fields["auth_id"]
	if !ok || len(fields) != 1 {
		return managementSerialActiveRequest{}, &managementSerialActiveError{
			Status: http.StatusBadRequest, Code: "invalid_body", Message: "only auth_id is accepted",
		}
	}
	var authID string
	if err := json.Unmarshal(value, &authID); err != nil || strings.TrimSpace(authID) == "" {
		return managementSerialActiveRequest{}, &managementSerialActiveError{
			Status: http.StatusBadRequest, Code: "invalid_auth_id", Message: "auth_id must be a non-empty string",
		}
	}
	return managementSerialActiveRequest{AuthID: strings.TrimSpace(authID)}, nil
}

func managementSerialActiveErrorResponse(err error) pluginapi.ManagementResponse {
	status := http.StatusInternalServerError
	code := "serial_active_update_failed"
	message := "could not update serial primary"
	if typed, ok := err.(*managementSerialActiveError); ok {
		status, code, message = typed.Status, typed.Code, typed.Message
	}
	return jsonManagementResponse(status, map[string]any{"error": code, "message": message})
}

type manualSerialActiveResult struct {
	changed bool
}

func (s *schedulerRuntimeState) setManualSerialActive(requestedAuthID string, now time.Time) (manualSerialActiveResult, error) {
	requestedAuthID = strings.TrimSpace(requestedAuthID)
	if requestedAuthID == "" {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusBadRequest, Code: "invalid_auth_id", Message: "auth_id must be a non-empty string",
		}
	}

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if !s.generationOwnerActive() {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "generation_not_active", Message: "scheduler generation is not active",
		}
	}

	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	if normalizeSchedulerMode(cfg.SchedulerMode) != "serial" {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "serial_mode_required", Message: "manual primary selection requires scheduler_mode=serial",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	files, err := cpaManagementAuthFiles(ctx, cfg)
	cancel()
	if err != nil {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusServiceUnavailable, Code: "auth_inventory_unavailable", Message: "current CPA auth inventory is unavailable",
		}
	}
	eligible, _ := eligibleCPACodexAuthsWithStats(files, false)
	binding, ok := eligible[requestedAuthID]
	if !ok {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusUnprocessableEntity, Code: "auth_not_active", Message: "credential is not an active available Codex auth",
		}
	}
	canonicalAuthID := strings.TrimSpace(binding.AuthID)
	if canonicalAuthID == "" {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusUnprocessableEntity, Code: "auth_not_active", Message: "credential has no routable auth id",
		}
	}
	if _, banned := banStore.lookup(canonicalAuthID); banned {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "auth_quarantined", Message: "credential is currently quarantined",
		}
	}

	s.mu.RLock()
	snapshot, found := s.quotas[canonicalAuthID]
	if !found {
		snapshot, found = s.quotas[strings.TrimSpace(binding.AuthIndex)]
	}
	s.mu.RUnlock()
	if !found || !quotaSnapshotFresh(snapshot, now, cfg.StaleAfter) {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "quota_not_fresh", Message: "credential does not have a fresh quota snapshot",
		}
	}
	choice := inspectSerialCandidate(pluginapi.SchedulerAuthCandidate{ID: canonicalAuthID, Provider: providerCodex}, snapshot, true, cfg, now)
	if !choice.QuotaKnown || !choice.Eligible {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "auth_not_eligible", Message: fmt.Sprintf("credential is not eligible for serial selection: %s", choice.Reason),
		}
	}

	s.mu.Lock()
	previousAuthID := s.serialActiveAuthID
	previousSource := s.serialSelectionSource
	previousSelectedAt := s.serialSelectedAt
	previousLastSwitchAt := s.serialLastSwitchAt
	previousLastSwitchReason := s.serialLastSwitchReason
	changed := strings.TrimSpace(previousAuthID) != canonicalAuthID || normalizeSerialSelectionSource(previousSource) != "manual"
	s.serialActiveAuthID = canonicalAuthID
	s.serialSelectionSource = "manual"
	s.serialSelectedAt = now
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = "manual_selection"
	s.resetSerialMissingLocked()
	s.mu.Unlock()

	if !s.persistBanState() {
		s.mu.Lock()
		if s.serialActiveAuthID == canonicalAuthID && normalizeSerialSelectionSource(s.serialSelectionSource) == "manual" && s.serialSelectedAt.Equal(now) {
			s.serialActiveAuthID = previousAuthID
			s.serialSelectionSource = previousSource
			s.serialSelectedAt = previousSelectedAt
			s.serialLastSwitchAt = previousLastSwitchAt
			s.serialLastSwitchReason = previousLastSwitchReason
		}
		s.mu.Unlock()
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusServiceUnavailable, Code: "state_persistence_failed", Message: "manual selection was not persisted",
		}
	}
	return manualSerialActiveResult{changed: changed}, nil
}

func (s *schedulerRuntimeState) clearManualSerialActive(now time.Time) (manualSerialActiveResult, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if !s.generationOwnerActive() {
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusConflict, Code: "generation_not_active", Message: "scheduler generation is not active",
		}
	}
	s.mu.Lock()
	if normalizeSerialSelectionSource(s.serialSelectionSource) != "manual" {
		s.mu.Unlock()
		return manualSerialActiveResult{changed: false}, nil
	}
	previousAuthID := s.serialActiveAuthID
	previousSource := s.serialSelectionSource
	previousSelectedAt := s.serialSelectedAt
	previousLastSwitchAt := s.serialLastSwitchAt
	previousLastSwitchReason := s.serialLastSwitchReason
	s.serialActiveAuthID = ""
	s.serialSelectionSource = "auto"
	s.serialSelectedAt = time.Time{}
	s.serialLastSwitchAt = now
	s.serialLastSwitchReason = "manual_clear"
	s.resetSerialMissingLocked()
	s.mu.Unlock()
	if !s.persistBanState() {
		s.mu.Lock()
		if s.serialActiveAuthID == "" && normalizeSerialSelectionSource(s.serialSelectionSource) == "auto" {
			s.serialActiveAuthID = previousAuthID
			s.serialSelectionSource = previousSource
			s.serialSelectedAt = previousSelectedAt
			s.serialLastSwitchAt = previousLastSwitchAt
			s.serialLastSwitchReason = previousLastSwitchReason
		}
		s.mu.Unlock()
		return manualSerialActiveResult{}, &managementSerialActiveError{
			Status: http.StatusServiceUnavailable, Code: "state_persistence_failed", Message: "automatic selection state was not persisted",
		}
	}
	return manualSerialActiveResult{changed: true}, nil
}

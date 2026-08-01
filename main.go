// Package main implements the Codex Quota Scheduler CPA plugin.
//
// The plugin is limited to Codex credentials. It observes completed requests,
// quarantines 429 credentials, and requires one serialized half-open success
// after cooldown before restoring normal traffic. The scheduler also consumes
// fresh Keeper quota snapshots to keep one global active auth until it reaches
// the configured threshold. Legacy pacing modes remain available for migration.
//
// Three capabilities are registered:
//   - usage_plugin: merges fresh x-codex-* window headers, observes request
//     cost, and drives the cooldown/probation/half-open state machine.
//   - scheduler: defaults to serial fill-first routing, excludes exhausted or
//     quarantined credentials, and never changes model/provider routes.
//   - management_api: exposes non-secret quota/pacing diagnostics plus explicit
//     manual unban operations for upstream reset-card actions.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	void* call;
	void* free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginabi"
	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

const (
	pluginName    = "codex-quota-scheduler"
	pluginVersion = "0.1.5"

	// providerCodex is the CPA provider key for OpenAI Codex (ChatGPT backend).
	providerCodex = "codex"

	// statusTooManyRequests is the HTTP 429 status code.
	statusTooManyRequests = 429

	// Codex rate-limit window sizes, in minutes, as reported by the
	// x-codex-primary-window-minutes / x-codex-secondary-window-minutes
	// response headers.
	windowMinutes5h   = 300   // 5 hours
	windowMinutesWeek = 10080 // 7 days

	// usedPercentThreshold is the "this window is the one that tripped" marker.
	// A 429 carries the window that exhausted at ~100% used.
	usedPercentThreshold = 100

	managementRoutePrefix = "/plugins/" + pluginName
)

// banStore keeps a credential quarantined until an explicit half-open probe
// proves that upstream accepts requests again. Expired cooldowns are retained as
// probe-ready entries; status reads and process restarts must never silently
// turn them into healthy credentials.
var banStore banState

const (
	banKindQuota     = "quota"
	banKindProbation = "probation"

	banPhaseCooldown = "cooldown"
	banPhaseHalfOpen = "half_open"
)

type banDisposition string

const (
	banDispositionHealthy    banDisposition = "healthy"
	banDispositionCooldown   banDisposition = "cooldown"
	banDispositionProbeReady banDisposition = "probe_ready"
	banDispositionHalfOpen   banDisposition = "half_open"
)

type banState struct {
	mu   sync.Mutex
	bans map[string]banEntry // keyed by AuthID

	total429s      uint64
	probation429s  uint64
	probeStarts    uint64
	probeSuccesses uint64
	probeFailures  uint64
}

type banEntry struct {
	// ResetAt is the upstream reset or local probation deadline. Passing it
	// moves the entry to probe_ready; it does not make the credential healthy.
	ResetAt time.Time
	// Window is a human-readable label of the limiting window.
	Window string
	// BannedAt is when the latest cooldown was recorded.
	BannedAt time.Time
	// Kind distinguishes an authoritative quota cooldown from a short
	// probation created when a 429 did not include usable quota headers.
	Kind string
	// Phase is cooldown or half_open. A cooldown past ResetAt is probe_ready.
	Phase string
	// ProbeStartedAt and ProbeLeaseUntil serialize half-open attempts. The lease
	// allows recovery after a crashed or lost request without admitting a burst.
	ProbeStartedAt  time.Time
	ProbeLeaseUntil time.Time
	ProbeAttempts   int
}

type banStateStats struct {
	Total429s      uint64
	Probation429s  uint64
	ProbeStarts    uint64
	ProbeSuccesses uint64
	ProbeFailures  uint64
}

func normalizeBanEntry(entry banEntry) banEntry {
	if entry.Kind != banKindQuota && entry.Kind != banKindProbation {
		if strings.Contains(strings.ToLower(entry.Window), "temporary fallback") ||
			strings.Contains(strings.ToLower(entry.Window), "probation") {
			entry.Kind = banKindProbation
		} else {
			entry.Kind = banKindQuota
		}
	}
	if entry.Phase != banPhaseHalfOpen {
		entry.Phase = banPhaseCooldown
		entry.ProbeStartedAt = time.Time{}
		entry.ProbeLeaseUntil = time.Time{}
	} else if entry.ProbeStartedAt.IsZero() {
		entry.Phase = banPhaseCooldown
		entry.ProbeLeaseUntil = time.Time{}
	}
	return entry
}

func banEntryDisposition(entry banEntry, now time.Time) banDisposition {
	entry = normalizeBanEntry(entry)
	if entry.Phase == banPhaseHalfOpen && !entry.ProbeLeaseUntil.IsZero() && now.Before(entry.ProbeLeaseUntil) {
		return banDispositionHalfOpen
	}
	if entry.ResetAt.IsZero() || now.Before(entry.ResetAt) {
		return banDispositionCooldown
	}
	return banDispositionProbeReady
}

func probeRequestMatches(entry banEntry, requestedAt time.Time) bool {
	if entry.Phase != banPhaseHalfOpen || entry.ProbeStartedAt.IsZero() {
		return false
	}
	if requestedAt.IsZero() {
		return true
	}
	// RequestedAt is host-generated, but tolerate small clock/serialization
	// differences while excluding requests that clearly predate the probe.
	return !requestedAt.Before(entry.ProbeStartedAt.Add(-time.Second))
}

// lookup returns the normalized quarantine entry for the given auth ID.
func (s *banState) lookup(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[strings.TrimSpace(authID)]
	if ok {
		entry = normalizeBanEntry(entry)
	}
	return entry, ok
}

func (s *banState) setCooldownLocked(authID string, entry banEntry) {
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	authID = strings.TrimSpace(authID)
	entry = normalizeBanEntry(entry)
	entry.Phase = banPhaseCooldown
	entry.ProbeStartedAt = time.Time{}
	entry.ProbeLeaseUntil = time.Time{}
	if previous, ok := s.bans[authID]; ok {
		previous = normalizeBanEntry(previous)
		// A repeated 429 must never shorten an already known cooldown.
		if previous.ResetAt.After(entry.ResetAt) {
			entry.ResetAt = previous.ResetAt
			if previous.Kind == banKindQuota && entry.Kind == banKindProbation {
				entry.Kind = previous.Kind
				entry.Window = previous.Window
			}
		}
		if previous.ProbeAttempts > entry.ProbeAttempts {
			entry.ProbeAttempts = previous.ProbeAttempts
		}
	}
	s.bans[authID] = entry
}

// set restores an entry without changing runtime counters or discarding an
// in-flight half-open lease persisted before a restart.
func (s *banState) set(authID string, entry banEntry) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	entry = normalizeBanEntry(entry)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		s.bans = make(map[string]banEntry)
	}
	if previous, ok := s.bans[authID]; ok {
		previous = normalizeBanEntry(previous)
		// Hot reload may read a slightly older on-disk snapshot. Preserve the
		// later cooldown or currently active half-open lease already in memory.
		if previous.ResetAt.After(entry.ResetAt) ||
			(previous.Phase == banPhaseHalfOpen && previous.ProbeLeaseUntil.After(entry.ProbeLeaseUntil)) {
			return
		}
	}
	s.bans[authID] = entry
}

// record429 applies a new cooldown and records whether it closed a half-open
// attempt. Loading persisted state deliberately uses set instead.
func (s *banState) record429(authID string, entry banEntry, requestedAt time.Time) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total429s++
	entry = normalizeBanEntry(entry)
	if entry.Kind == banKindProbation {
		s.probation429s++
	}
	if previous, ok := s.bans[authID]; ok && probeRequestMatches(normalizeBanEntry(previous), requestedAt) {
		s.probeFailures++
	}
	s.setCooldownLocked(authID, entry)
}

// schedulable reports whether the credential may participate in ranking. A
// probe-ready credential is rankable, but the final selection must still call
// tryStartProbe atomically before returning it to CPA.
func (s *banState) schedulable(authID string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[strings.TrimSpace(authID)]
	if !ok {
		return true
	}
	disposition := banEntryDisposition(entry, now)
	return disposition == banDispositionProbeReady
}

// tryStartProbe validates the final scheduler choice and atomically reserves a
// single half-open request. It returns allowed=true for a healthy credential.
func (s *banState) tryStartProbe(authID string, now time.Time, lease time.Duration) (allowed, probeStarted bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false, false
	}
	if lease <= 0 {
		lease = 15 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if !ok {
		return true, false
	}
	entry = normalizeBanEntry(entry)
	if banEntryDisposition(entry, now) != banDispositionProbeReady {
		return false, false
	}
	entry.Phase = banPhaseHalfOpen
	entry.ProbeStartedAt = now
	entry.ProbeLeaseUntil = now.Add(lease)
	entry.ProbeAttempts++
	s.bans[authID] = entry
	s.probeStarts++
	return true, true
}

// completeProbe clears a successful half-open entry or returns a non-429
// failure to a short cooldown. It ignores completions that predate the probe.
func (s *banState) completeProbe(authID string, requestedAt, now time.Time, success bool, retryAfter time.Duration) (banEntry, bool, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return banEntry{}, false, false
	}
	if retryAfter <= 0 {
		retryAfter = 2 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.bans[authID]
	if !ok {
		return banEntry{}, false, false
	}
	entry = normalizeBanEntry(entry)
	if !probeRequestMatches(entry, requestedAt) {
		return entry, false, false
	}
	if success {
		delete(s.bans, authID)
		s.probeSuccesses++
		return entry, true, true
	}
	entry.Phase = banPhaseCooldown
	entry.ResetAt = now.Add(retryAfter)
	entry.ProbeStartedAt = time.Time{}
	entry.ProbeLeaseUntil = time.Time{}
	s.bans[authID] = entry
	s.probeFailures++
	return entry, false, true
}

// clear removes the quarantine entry for authID, if present.
func (s *banState) clear(authID string) (banEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bans == nil {
		return banEntry{}, false
	}
	authID = strings.TrimSpace(authID)
	entry, ok := s.bans[authID]
	if ok {
		delete(s.bans, authID)
		entry = normalizeBanEntry(entry)
	}
	return entry, ok
}

// clearAll removes every active quarantine entry.
func (s *banState) clearAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.bans)
	s.bans = make(map[string]banEntry)
	return n
}

// snapshot returns a normalized copy without deleting probe-ready entries.
func (s *banState) snapshot() map[string]banEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]banEntry, len(s.bans))
	for authID, entry := range s.bans {
		out[authID] = normalizeBanEntry(entry)
	}
	return out
}

func (s *banState) stats() banStateStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return banStateStats{
		Total429s:      s.total429s,
		Probation429s:  s.probation429s,
		ProbeStarts:    s.probeStarts,
		ProbeSuccesses: s.probeSuccesses,
		ProbeFailures:  s.probeFailures,
	}
}

func main() {}

// cliproxy_plugin_init is the native entry point CPA calls when loading the
// plugin. It wires the host reverse-call API and registers our call/free/shutdown
// function pointers.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(_ *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall is the single dispatch entry CPA invokes for every method.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	schedulerRuntime.stop()
}

// handleMethod routes a CPA method to its handler.
func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var lifecycle lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &lifecycle); err != nil {
				// Keep registration compatible with older hosts that send an empty
				// lifecycle payload.  A malformed optional config is handled by
				// configureSchedulerRuntime without taking CPA down.
				slog.Warn("codex-quota-scheduler: invalid lifecycle request", "error", err)
			}
		}
		configureSchedulerRuntime(lifecycle.ConfigYAML)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodUsageHandle:
		return handleUsage(request)
	case pluginabi.MethodSchedulerPick:
		return handleSchedulerPick(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// pluginRegistration declares the plugin's metadata and capabilities.
// Both usage_plugin and scheduler must be true.
func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "simplez2",
			GitHubRepository: "https://github.com/simplez2/cpa-codex-quota-scheduler",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "scheduler_mode", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{"serial", "legacy", "shadow", "enforce"}, Description: "Runtime policy mode. Serial keeps one global active Codex auth, but strictly preempts it when a higher-priority quota window becomes available."},
				{Name: "serial_switch_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "In serial mode, switch away when any active quota window reaches this used percentage. Defaults to 98."},
				{Name: "serial_prefer_active_cycle", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Prefer an already-started quota cycle when choosing the next serial auth."},
				{Name: "keeper_url", Type: pluginapi.ConfigFieldTypeString, Description: "Keeper base URL, for example http://cpa-usage-keeper:8080/keeper."},
				{Name: "keeper_password_file", Type: pluginapi.ConfigFieldTypeString, Description: "Mounted Keeper login-password file; the password is never placed in YAML."},
				{Name: "cpa_management_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA localhost Management API call endpoint used only for pinned warmup requests."},
				{Name: "cpa_management_key_file", Type: pluginapi.ConfigFieldTypeString, Description: "Mounted owner-readable CPA management key file; the key is never placed in YAML or logs."},
				{Name: "warmup_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Activate Codex accounts with 100% available quota whose Keeper reset is missing, expired, or still a full-duration moving placeholder."},
				{Name: "warmup_model", Type: pluginapi.ConfigFieldTypeString, Description: "Model used for the minimal pinned hello activation request; this does not change any route default."},
				{Name: "warmup_sidecar_url", Type: pluginapi.ConfigFieldTypeString, Description: "Internal Codex Agent Identity sidecar base URL used by pinned warmup."},
				{Name: "warmup_retry_after", Type: pluginapi.ConfigFieldTypeString, Description: "Minimum delay before retrying a warmup that did not return a reset window."},
				{Name: "refresh_interval", Type: pluginapi.ConfigFieldTypeString, Description: "How often to read Keeper's cached Codex quota (for example 30s)."},
				{Name: "stale_after", Type: pluginapi.ConfigFieldTypeString, Description: "Maximum age of a quota snapshot before native CPA scheduling is used."},
				{Name: "state_path", Type: pluginapi.ConfigFieldTypeString, Description: "Owner-only JSON file for quarantine, serial active-auth, and warmup bookkeeping; contains no secrets."},
				{Name: "soft_limit_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Avoid a window at or above this percentage when a healthier same-priority choice exists."},
				{Name: "reserve_5h_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Safety reserve retained in every five-hour quota window."},
				{Name: "reserve_weekly_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Safety reserve retained in every weekly quota window."},
				{Name: "reserve_monthly_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Safety reserve retained in every monthly quota window."},
				{Name: "low_quota_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Remaining quota threshold that raises request-cost prediction from P75 to P90."},
				{Name: "fallback_ban", Type: pluginapi.ConfigFieldTypeString, Description: "Temporary 429 ban when upstream reset headers are absent."},
				{Name: "max_ban", Type: pluginapi.ConfigFieldTypeString, Description: "Upper bound for a temporary automatic ban."},
				{Name: "half_open_probe_timeout", Type: pluginapi.ConfigFieldTypeString, Description: "Lease for the single half-open probe after a cooldown expires."},
				{Name: "half_open_retry_after", Type: pluginapi.ConfigFieldTypeString, Description: "Delay before retrying a half-open probe that failed without a 429."},
				{Name: "sticky_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Idle lifetime for hashed session-to-credential bindings; zero disables plugin stickiness."},
				{Name: "switch_hysteresis_percent", Type: pluginapi.ConfigFieldTypeNumber, Description: "Minimum score advantage required before a healthy sticky session may switch."},
				{Name: "switch_confirmations", Type: pluginapi.ConfigFieldTypeInteger, Description: "Consecutive challenger wins required before switching a healthy sticky session."},
				{Name: "cost_sample_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum rolling normal-request credit samples retained in memory."},
				{Name: "decision_history_limit", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum redacted scheduler decisions retained for diagnostics."},
				{Name: "normal_cost_quantile", Type: pluginapi.ConfigFieldTypeNumber, Description: "Request-cost quantile used under normal quota conditions."},
				{Name: "guard_cost_quantile", Type: pluginapi.ConfigFieldTypeNumber, Description: "Request-cost quantile used when a quota window is low."},
				{Name: "high_cost_quantile", Type: pluginapi.ConfigFieldTypeNumber, Description: "Request-cost quantile used for high-effort or long-tail requests."},
				{Name: "shadow_log_interval", Type: pluginapi.ConfigFieldTypeString, Description: "Minimum interval between aggregate shadow disagreement log messages; zero disables them."},
				{Name: "prefer_reset_credits", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Prefer accounts with available Codex reset credits within the same window class."},
				{Name: "window_order", Type: pluginapi.ConfigFieldTypeArray, Description: "Priority order, normally [5h, weekly, monthly]."},
			},
		},
		Capabilities: registrationCapability{
			UsagePlugin:   true,
			Scheduler:     true,
			ManagementAPI: true,
		},
	}
}

// handleUsage observes a completed request. On a Codex 429 it records the
// ban; otherwise it is a no-op.
func handleUsage(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return okEnvelope(map[string]any{})
	}
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		slog.Warn("codex-quota-scheduler: failed to decode usage record", "error", errUnmarshal)
		return okEnvelope(map[string]any{})
	}

	// Only Codex credentials are in scope.
	if !strings.EqualFold(record.Provider, providerCodex) {
		return okEnvelope(map[string]any{})
	}
	// Every Codex response can carry fresh primary/secondary window headers and
	// token accounting. Feed both into the pacing runtime first.
	schedulerRuntime.observeUsage(record)

	authID := strings.TrimSpace(record.AuthID)
	if authID == "" && strings.TrimSpace(record.AuthIndex) != "" {
		authID = schedulerRuntime.authIDForIndex(record.AuthIndex)
	}
	is429 := record.Failed && record.Failure.StatusCode == statusTooManyRequests
	if !is429 {
		if authID == "" {
			return okEnvelope(map[string]any{})
		}
		_, cleared, changed := banStore.completeProbe(
			authID,
			record.RequestedAt,
			time.Now(),
			!record.Failed,
			schedulerRuntime.halfOpenRetryAfter(),
		)
		if changed {
			schedulerRuntime.persistAfterBanChange()
			if cleared {
				slog.Info("codex-quota-scheduler: half-open probe succeeded; credential re-enabled", "auth_id", authID)
			} else {
				slog.Warn("codex-quota-scheduler: half-open probe failed without 429; retry delayed", "auth_id", authID, "status", record.Failure.StatusCode)
			}
		}
		return okEnvelope(map[string]any{})
	}
	if authID == "" {
		slog.Warn("codex-quota-scheduler: 429 received but AuthID is empty, cannot quarantine")
		return okEnvelope(map[string]any{})
	}

	now := time.Now()
	fallback, maxBan := schedulerRuntime.banDurations()
	entry, authoritative := quarantineEntryFor429(record.ResponseHeaders, now, fallback, maxBan)
	if !authoritative {
		slog.Warn("codex-quota-scheduler: quota headers missing on 429; credential entered probation", "auth_id", authID, "retry_at", entry.ResetAt.Format(time.RFC3339))
	}

	banStore.record429(authID, entry, record.RequestedAt)
	schedulerRuntime.markSerialUnavailable(authID, "429", now)
	schedulerRuntime.persistAfterBanChange()
	slog.Info("codex-quota-scheduler: credential quarantined after 429",
		"auth_id", authID,
		"kind", entry.Kind,
		"window", entry.Window,
		"probe_ready_at", entry.ResetAt.Format(time.RFC3339))
	return okEnvelope(map[string]any{})
}

// quarantineEntryFor429 applies the same cooldown policy to ordinary traffic,
// half-open probes, and warmup requests. A 429 without usable quota headers is
// probationary: its deadline only makes the credential probe-ready.
func quarantineEntryFor429(headers http.Header, now time.Time, fallback, maxBan time.Duration) (banEntry, bool) {
	entry, authoritative := classifyAndBuildBanAt(headers, now)
	if authoritative {
		entry.BannedAt = now
		entry.Kind = banKindQuota
		entry.Phase = banPhaseCooldown
		return entry, true
	}
	if fallback <= 0 {
		fallback = 15 * time.Minute
	}
	if retryAfter := retryAfterDuration(headers, now); retryAfter > fallback {
		fallback = retryAfter
	}
	if maxBan > 0 && fallback > maxBan {
		fallback = maxBan
	}
	return banEntry{
		ResetAt:  now.Add(fallback),
		Window:   "probation (quota headers missing)",
		BannedAt: now,
		Kind:     banKindProbation,
		Phase:    banPhaseCooldown,
	}, false
}

// classifyAndBuildBan inspects the upstream x-codex-* response headers and
// decides which rate-limit window was exhausted, returning the ban entry with
// the corresponding reset time. Returns ok=false when the headers are absent
// or inconclusive.
//
// Header reference (ChatGPT/Codex backend, not the public Platform API):
//   - x-codex-primary-window-minutes   = 300 for the 5-hour window
//   - x-codex-primary-reset-at         = Unix seconds, 5-hour window reset
//   - x-codex-primary-used-percent     = 0-100
//   - x-codex-secondary-window-minutes = 10080 for the weekly window
//   - x-codex-secondary-reset-at       = Unix seconds, weekly window reset
//   - x-codex-secondary-used-percent   = 0-100
func classifyAndBuildBan(headers http.Header) (banEntry, bool) {
	return classifyAndBuildBanAt(headers, time.Now())
}

func classifyAndBuildBanAt(headers http.Header, now time.Time) (banEntry, bool) {
	h := headers

	primaryUsed := headerFloat(h, "x-codex-primary-used-percent")
	secondaryUsed := headerFloat(h, "x-codex-secondary-used-percent")
	primaryReset := headerResetTime(h, "x-codex-primary-reset-at", "x-codex-primary-reset-after-seconds", now)
	secondaryReset := headerResetTime(h, "x-codex-secondary-reset-at", "x-codex-secondary-reset-after-seconds", now)

	// Prefer the explicit "which window is full" signal: the window whose
	// used-percent reached the threshold. If both are present, pick the one
	// at threshold; if only one header family is present, use that.
	primaryFull := primaryUsed >= usedPercentThreshold
	secondaryFull := secondaryUsed >= usedPercentThreshold

	switch {
	case secondaryFull && !primaryFull:
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	case primaryFull && !secondaryFull:
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
	case primaryFull && secondaryFull:
		// Both exhausted: must wait for the later reset (weekly) to be safe.
		if !secondaryReset.IsZero() {
			return banEntry{ResetAt: secondaryReset, Window: "week (both full)"}, true
		}
		if !primaryReset.IsZero() {
			return banEntry{ResetAt: primaryReset, Window: "5h (both full, weekly reset missing)"}, true
		}
	default:
		// Neither reports as full via used-percent. Fall back to window-minutes
		// identity if a reset time is present, else give up.
		if !primaryReset.IsZero() && headerInt(h, "x-codex-primary-window-minutes") == windowMinutes5h {
			return banEntry{ResetAt: primaryReset, Window: "5h"}, true
		}
		if !secondaryReset.IsZero() && headerInt(h, "x-codex-secondary-window-minutes") == windowMinutesWeek {
			return banEntry{ResetAt: secondaryReset, Window: "week"}, true
		}
	}
	return banEntry{}, false
}

// handleSchedulerPick applies the Codex-only quota policy. Serial mode keeps a
// deterministic single active auth even during a Keeper outage; legacy modes
// retain their native-fallback behavior.
func handleSchedulerPick(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	response, err := schedulerRuntime.schedulerPick(req)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}

// managementRegistration exposes non-secret quota, pacing, and quarantine
// diagnostics. Manual unban remains available for explicit upstream reset-card
// actions that CPA cannot observe; normal cooldown expiry uses half-open probes.
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        managementRoutePrefix + "/bans",
				Description: "List Codex auths currently held out of the pool by Codex Quota Scheduler.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban",
				Description: "Remove one Codex auth from persisted plugin quarantine. Body: {\"auth_id\":\"...\"}.",
			},
			{
				Method:      http.MethodPost,
				Path:        managementRoutePrefix + "/unban-all",
				Description: "Remove every Codex auth from persisted plugin quarantine.",
			},
			{
				Method:      http.MethodGet,
				Path:        managementRoutePrefix + "/quota",
				Description: "Show the active serial auth, Keeper freshness, pacing diagnostics, and redacted decisions.",
			},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return okEnvelope(dispatchManagement(req))
}

func dispatchManagement(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}

	switch {
	case method == http.MethodGet && matchesManagementPath(req.Path, "/bans"):
		return jsonManagementResponse(http.StatusOK, currentBanStatus())
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban"):
		return handleManagementUnban(req)
	case method == http.MethodPost && matchesManagementPath(req.Path, "/unban-all"):
		return handleManagementUnbanAll()
	case method == http.MethodGet && matchesManagementPath(req.Path, "/quota"):
		return jsonManagementResponse(http.StatusOK, schedulerRuntime.status())
	default:
		return jsonManagementResponse(http.StatusNotFound, map[string]any{
			"error":  "not_found",
			"path":   req.Path,
			"method": method,
		})
	}
}

type managementBanStatus struct {
	Plugin         string              `json:"plugin"`
	Version        string              `json:"version"`
	Count          int                 `json:"count"`
	Total429s      uint64              `json:"total_429s"`
	Probation429s  uint64              `json:"probation_429s"`
	ProbeStarts    uint64              `json:"probe_starts"`
	ProbeSuccesses uint64              `json:"probe_successes"`
	ProbeFailures  uint64              `json:"probe_failures"`
	Bans           []managementBanInfo `json:"bans"`
}

type managementBanInfo struct {
	AuthID                     string `json:"auth_id"`
	Kind                       string `json:"kind"`
	State                      string `json:"state"`
	Window                     string `json:"window"`
	BannedAt                   string `json:"banned_at,omitempty"`
	BannedAtUnix               int64  `json:"banned_at_unix,omitempty"`
	ResetAt                    string `json:"reset_at"`
	ResetAtUnix                int64  `json:"reset_at_unix"`
	RemainingSeconds           int64  `json:"remaining_seconds"`
	ProbeStartedAt             string `json:"probe_started_at,omitempty"`
	ProbeLeaseUntil            string `json:"probe_lease_until,omitempty"`
	ProbeLeaseRemainingSeconds int64  `json:"probe_lease_remaining_seconds,omitempty"`
	ProbeAttempts              int    `json:"probe_attempts"`
}

func currentBanStatus() managementBanStatus {
	now := time.Now()
	snapshot := banStore.snapshot()
	stats := banStore.stats()
	bans := make([]managementBanInfo, 0, len(snapshot))
	for authID, entry := range snapshot {
		state := banEntryDisposition(entry, now)
		remaining := int64(0)
		if state == banDispositionCooldown && now.Before(entry.ResetAt) {
			remaining = int64(entry.ResetAt.Sub(now).Seconds())
		}
		probeRemaining := int64(0)
		if state == banDispositionHalfOpen && now.Before(entry.ProbeLeaseUntil) {
			probeRemaining = int64(entry.ProbeLeaseUntil.Sub(now).Seconds())
		}
		info := managementBanInfo{
			AuthID:                     authID,
			Kind:                       entry.Kind,
			State:                      string(state),
			Window:                     entry.Window,
			ResetAt:                    entry.ResetAt.Format(time.RFC3339),
			ResetAtUnix:                entry.ResetAt.Unix(),
			RemainingSeconds:           remaining,
			ProbeLeaseRemainingSeconds: probeRemaining,
			ProbeAttempts:              entry.ProbeAttempts,
		}
		if !entry.BannedAt.IsZero() {
			info.BannedAt = entry.BannedAt.Format(time.RFC3339)
			info.BannedAtUnix = entry.BannedAt.Unix()
		}
		if !entry.ProbeStartedAt.IsZero() {
			info.ProbeStartedAt = entry.ProbeStartedAt.Format(time.RFC3339)
		}
		if !entry.ProbeLeaseUntil.IsZero() {
			info.ProbeLeaseUntil = entry.ProbeLeaseUntil.Format(time.RFC3339)
		}
		bans = append(bans, info)
	}
	sort.Slice(bans, func(i, j int) bool {
		if bans[i].State != bans[j].State {
			return bans[i].State < bans[j].State
		}
		if bans[i].ResetAtUnix == bans[j].ResetAtUnix {
			return bans[i].AuthID < bans[j].AuthID
		}
		return bans[i].ResetAtUnix < bans[j].ResetAtUnix
	})
	return managementBanStatus{
		Plugin:         pluginName,
		Version:        pluginVersion,
		Count:          len(bans),
		Total429s:      stats.Total429s,
		Probation429s:  stats.Probation429s,
		ProbeStarts:    stats.ProbeStarts,
		ProbeSuccesses: stats.ProbeSuccesses,
		ProbeFailures:  stats.ProbeFailures,
		Bans:           bans,
	}
}

type managementUnbanRequest struct {
	AuthID string `json:"auth_id"`
	All    bool   `json:"all"`
}

func handleManagementUnban(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	var body managementUnbanRequest
	if len(req.Body) > 0 {
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
			return jsonManagementResponse(http.StatusBadRequest, map[string]any{
				"error":   "invalid_json",
				"message": errUnmarshal.Error(),
			})
		}
	}
	if strings.EqualFold(req.Query.Get("all"), "true") || body.All {
		return handleManagementUnbanAll()
	}

	authID := strings.TrimSpace(body.AuthID)
	if authID == "" {
		authID = strings.TrimSpace(req.Query.Get("auth_id"))
	}
	if authID == "" {
		return jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error":   "missing_auth_id",
			"message": "provide auth_id in JSON body or query string",
		})
	}

	entry, removed := banStore.clear(authID)
	if removed {
		schedulerRuntime.persistAfterBanChange()
	}
	if removed {
		slog.Info("codex-quota-scheduler: manually re-enabled credential",
			"auth_id", authID, "window", entry.Window, "reset_at", entry.ResetAt.Format(time.RFC3339))
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"auth_id": authID,
		"removed": removed,
		"status":  currentBanStatus(),
	})
}

func handleManagementUnbanAll() pluginapi.ManagementResponse {
	removed := banStore.clearAll()
	if removed > 0 {
		schedulerRuntime.persistAfterBanChange()
	}
	if removed > 0 {
		slog.Info("codex-quota-scheduler: manually re-enabled all credentials", "removed", removed)
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{
		"ok":      true,
		"removed": removed,
		"status":  currentBanStatus(),
	})
}

func matchesManagementPath(path, suffix string) bool {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return false
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return strings.HasSuffix(path, managementRoutePrefix+suffix)
}

func jsonManagementResponse(status int, v any) pluginapi.ManagementResponse {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		status = http.StatusInternalServerError
		raw, _ = json.Marshal(map[string]any{
			"error":   "marshal_error",
			"message": errMarshal.Error(),
		})
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type": []string{"application/json; charset=utf-8"},
		},
		Body: raw,
	}
}

// ---- header helpers ----

func headerFloat(h http.Header, key string) float64 {
	raw := h.Get(key)
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0
	}
	return v
}

func headerInt(h http.Header, key string) int {
	raw := h.Get(key)
	if raw == "" {
		return 0
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return v
}

func headerUnixTime(h http.Header, key string) time.Time {
	raw := h.Get(key)
	if raw == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

func headerResetTime(h http.Header, absoluteKey, afterKey string, now time.Time) time.Time {
	if absolute := headerUnixTime(h, absoluteKey); !absolute.IsZero() {
		return absolute
	}
	if after := headerInt(h, afterKey); after > 0 {
		return now.Add(time.Duration(after) * time.Second)
	}
	return time.Time{}
}

func retryAfterDuration(headers http.Header, now time.Time) time.Duration {
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}

// ---- envelope / response helpers ----

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	Scheduler     bool `json:"scheduler"`
	ManagementAPI bool `json:"management_api"`
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestClassifyWarmupFailureBlocksPolicyAndAuthenticationErrors(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		err         error
		wantCode    string
		wantBlocked bool
	}{
		{name: "cyber policy terminal", err: &warmupStreamTerminalError{Event: "response.failed", Code: "cyber_policy"}, wantCode: "cyber_policy", wantBlocked: true},
		{name: "abuse text", err: errors.New("abuse: sensitive upstream detail"), wantCode: "abuse", wantBlocked: true},
		{name: "unauthorized", status: http.StatusUnauthorized, err: errors.New("request failed"), wantCode: "http_401", wantBlocked: true},
		{name: "forbidden", status: http.StatusForbidden, err: errors.New("request failed"), wantCode: "http_403", wantBlocked: true},
		{name: "quota", status: http.StatusTooManyRequests, err: errors.New("request failed"), wantCode: "http_429", wantBlocked: false},
		{name: "auth unavailable", status: http.StatusServiceUnavailable, err: errors.New("auth_unavailable: no auth available"), wantCode: "auth_unavailable", wantBlocked: true},
		{name: "incomplete", err: errWarmupStreamIncomplete, wantCode: "response_incomplete", wantBlocked: false},
		{name: "timeout", err: context.DeadlineExceeded, wantCode: "timeout", wantBlocked: false},
		{name: "server error", status: http.StatusServiceUnavailable, err: errors.New("temporary upstream failure"), wantCode: "http_503", wantBlocked: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, blocked := classifyWarmupFailure(test.status, test.err)
			if code != test.wantCode || blocked != test.wantBlocked {
				t.Fatalf("classify = code %q blocked %v; want %q %v", code, blocked, test.wantCode, test.wantBlocked)
			}
		})
	}
}

func TestRecordWarmupErrorPersistsOnlySafeCode(t *testing.T) {
	state := schedulerRuntimeState{cfg: defaultPluginConfig(), warmups: make(map[string]warmupEntry)}
	state.cfg.StatePath = ""
	candidate := warmupCandidate{
		Snapshot: quotaSnapshot{AuthID: "acct", AuthIndex: "idx-acct"},
		Window:   quotaWindow{Class: "5h"},
	}
	state.recordWarmupError(candidate, http.StatusOK, errors.New("cyber_policy: token=must-not-be-persisted"))
	entry := state.warmups[warmupKey("acct", "5h")]
	if entry.Error != "cyber_policy" || !entry.Blocked {
		t.Fatalf("persisted warmup failure = %#v", entry)
	}
	if strings.Contains(entry.Error, "token") || strings.Contains(entry.Error, "must-not") {
		t.Fatalf("sensitive upstream detail persisted: %q", entry.Error)
	}
}

func TestWarmupHTTPStatusErrorUsesOnlyBoundedStructuredMetadata(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantCode    string
		wantBlocked bool
	}{
		{
			name:     "nested auth code",
			status:   http.StatusServiceUnavailable,
			body:     `{"error":{"code":"auth_unavailable","message":"Bearer must-not-be-persisted"}}`,
			wantCode: "auth_unavailable", wantBlocked: true,
		},
		{
			name:     "nested policy type",
			status:   http.StatusServiceUnavailable,
			body:     `{"error":{"type":"cyber_policy","message":"must-not-be-persisted"}}`,
			wantCode: "cyber_policy", wantBlocked: true,
		},
		{
			name:     "detail workspace code",
			status:   http.StatusPaymentRequired,
			body:     `{"detail":{"code":"deactivated_workspace","message":"must-not-be-persisted"}}`,
			wantCode: "deactivated_workspace", wantBlocked: true,
		},
		{
			name:     "nested response abuse type",
			status:   http.StatusServiceUnavailable,
			body:     `{"response":{"error":{"type":"cyber_abuse","message":"must-not-be-persisted"}}}`,
			wantCode: "cyber_abuse", wantBlocked: true,
		},
		{
			name:     "retryable server code",
			status:   http.StatusServiceUnavailable,
			body:     `{"error":{"code":"server_error"}}`,
			wantCode: "server_error", wantBlocked: false,
		},
		{
			name:     "auth status overrides generic body",
			status:   http.StatusUnauthorized,
			body:     `{"error":{"code":"server_error"}}`,
			wantCode: "http_401", wantBlocked: true,
		},
		{
			name:     "429 remains quarantine controlled",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"code":"auth_unavailable"}}`,
			wantCode: "http_429", wantBlocked: false,
		},
		{
			name:     "inner terminal overrides outer generic",
			status:   http.StatusServiceUnavailable,
			body:     `{"code":"server_error","error":{"code":"auth_unavailable"}}`,
			wantCode: "auth_unavailable", wantBlocked: true,
		},
		{
			name:     "message is never classified",
			status:   http.StatusServiceUnavailable,
			body:     `{"error":{"message":"auth_unavailable Bearer must-not-be-persisted"}}`,
			wantCode: "http_503", wantBlocked: false,
		},
		{
			name:     "unknown code is not persisted",
			status:   http.StatusServiceUnavailable,
			body:     `{"error":{"code":"token=must-not-be-persisted"}}`,
			wantCode: "http_503", wantBlocked: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := warmupHTTPStatusError(test.status, []byte(test.body), "test upstream")
			code, blocked := classifyWarmupFailure(test.status, err)
			if code != test.wantCode || blocked != test.wantBlocked {
				t.Fatalf("classify = %q blocked=%v; want %q blocked=%v; err=%v", code, blocked, test.wantCode, test.wantBlocked, err)
			}
			if strings.Contains(strings.ToLower(err.Error()), "bearer") || strings.Contains(err.Error(), "must-not-be-persisted") {
				t.Fatalf("safe HTTP error leaked response content: %v", err)
			}
		})
	}

	oversized := []byte(`{"error":{"code":"auth_unavailable"},"padding":"` + strings.Repeat("x", warmupMaxResponseBytes) + `"}`)
	err := warmupHTTPStatusError(http.StatusServiceUnavailable, oversized, "test upstream")
	if code, blocked := classifyWarmupFailure(http.StatusServiceUnavailable, err); code != "http_503" || blocked {
		t.Fatalf("oversized metadata classify = %q blocked=%v; want bounded http_503 fallback", code, blocked)
	}
}

func TestSafeHostCallbackFailureCodeDoesNotEchoSensitiveMessage(t *testing.T) {
	message := "request rejected by cyber_policy token=must-not-leak"
	if got := safeHostCallbackFailureCode("plugin_error", message); got != "cyber_policy" {
		t.Fatalf("safe callback code = %q; want cyber_policy", got)
	}
	if got := safeHostCallbackFailureCode("plugin_error", "token=must-not-leak"); got != "plugin_error" {
		t.Fatalf("unknown callback code = %q; want sanitized envelope code", got)
	}
}

func TestParseWarmupSSERequiresCompletedTerminalEvent(t *testing.T) {
	body := "event: response.created\n" +
		"data: {\"type\":\"response.created\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	out, err := parseWarmupSSE(body)
	if err != nil || out.TerminalEvent != "response.completed" {
		t.Fatalf("completed SSE = %#v, err=%v", out, err)
	}
}

func TestParseWarmupSSEUsesPayloadTypeWhenEventFieldIsAbsent(t *testing.T) {
	body := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	out, err := parseWarmupSSE(body)
	if err != nil || out.TerminalEvent != "response.completed" {
		t.Fatalf("payload-only completed SSE = %#v, err=%v", out, err)
	}
}

func TestParseWarmupSSERejectsFailedEventWithoutLeakingMessage(t *testing.T) {
	body := "event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"auth_unavailable\",\"message\":\"secret upstream detail\"}}}\n\n"
	out, err := parseWarmupSSE(body)
	if err == nil || out.TerminalEvent != "response.failed" || out.ErrorCode != "auth_unavailable" {
		t.Fatalf("failed SSE = %#v, err=%v", out, err)
	}
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("persistable error leaked upstream message: %v", err)
	}
}

func TestWarmupParsersNeverLetGenericMetadataHideTerminalCode(t *testing.T) {
	jsonBody := []byte(`{"type":"response.failed","status":"failed","code":"cyber_policy","error":{"code":"server_error"},"response":{"status":"failed","error":{"type":"upstream_error"}}}`)
	out, err := parseWarmupJSON(jsonBody)
	if err == nil || out.ErrorCode != "cyber_policy" {
		t.Fatalf("conflicting JSON metadata = %#v, err=%v; terminal code must win", out, err)
	}
	if code, blocked := classifyWarmupFailure(http.StatusOK, err); code != "cyber_policy" || !blocked {
		t.Fatalf("conflicting JSON classify = %q blocked=%v; want cyber_policy blocked", code, blocked)
	}

	sseBody := "event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"code\":\"auth_unavailable\",\"error\":{\"code\":\"server_error\"},\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"upstream_error\"}}}\n\n"
	out, err = parseWarmupSSE(sseBody)
	if err == nil || out.ErrorCode != "auth_unavailable" {
		t.Fatalf("conflicting SSE metadata = %#v, err=%v; terminal code must win", out, err)
	}
	if code, blocked := classifyWarmupFailure(http.StatusOK, err); code != "auth_unavailable" || !blocked {
		t.Fatalf("conflicting SSE classify = %q blocked=%v; want auth_unavailable blocked", code, blocked)
	}
}

func TestParseWarmupSSERejectsErrorEvent(t *testing.T) {
	body := "event: error\n" +
		"data: {\"type\":\"error\",\"code\":\"server_error\"}\n\n"
	out, err := parseWarmupSSE(body)
	if err == nil || out.TerminalEvent != "error" || out.ErrorCode != "server_error" {
		t.Fatalf("error SSE = %#v, err=%v", out, err)
	}
}

func TestParseWarmupSSERejectsCompletedEventWithNonCompletedStatus(t *testing.T) {
	body := "event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n"
	out, err := parseWarmupSSE(body)
	if err == nil || out.ErrorCode != "response_status_failed" {
		t.Fatalf("invalid completed status = %#v, err=%v", out, err)
	}
}

func TestParseWarmupSSERejectsTruncatedStream(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n"
	_, err := parseWarmupSSE(body)
	if !errors.Is(err, errWarmupStreamIncomplete) {
		t.Fatalf("truncated SSE err=%v, want errWarmupStreamIncomplete", err)
	}
}

func TestParseWarmupSSEHandlesCRLFAndMultiLineData(t *testing.T) {
	body := "event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\r\n" +
		"data: \"response\":{\"status\":\"completed\"}}\r\n\r\n"
	out, err := parseWarmupSSE(body)
	if err != nil || out.TerminalEvent != "response.completed" {
		t.Fatalf("CRLF completed SSE = %#v, err=%v", out, err)
	}
}

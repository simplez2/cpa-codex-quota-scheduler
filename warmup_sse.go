package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errWarmupStreamIncomplete = errors.New("warmup response stream ended without a terminal event")

// warmupStreamOutcome contains only non-sensitive terminal metadata. The
// upstream response text is deliberately not retained because it can include
// request or account details that do not belong in the persisted scheduler
// state or management UI.
type warmupStreamOutcome struct {
	TerminalEvent string
	ErrorCode     string
}

// warmupStreamTerminalError is safe to persist and render. It intentionally
// excludes the upstream error message and response body.
type warmupStreamTerminalError struct {
	Event string
	Code  string
}

type warmupErrorMetadata struct {
	Code string `json:"code"`
	Type string `json:"type"`
}

// warmupHTTPStatusError extracts only bounded, structured error metadata from
// an upstream failure. Response messages and bodies may contain credentials or
// account details and are never copied into the returned error, logs, or state.
func warmupHTTPStatusError(status int, body []byte, source string) error {
	if code := warmupResponseFailureCode(body); code != "" {
		return &warmupStreamTerminalError{Event: "response.failed", Code: code}
	}
	return fmt.Errorf("%s returned HTTP %d", source, status)
}

func warmupResponseFailureCode(body []byte) string {
	if len(body) == 0 || len(body) > warmupMaxResponseBytes {
		return ""
	}
	var payload struct {
		warmupErrorMetadata
		Error    *warmupErrorMetadata `json:"error"`
		Detail   *warmupErrorMetadata `json:"detail"`
		Response *struct {
			Error *warmupErrorMetadata `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	metadata := make([]*warmupErrorMetadata, 0, 4)
	if payload.Response != nil {
		metadata = append(metadata, payload.Response.Error)
	}
	metadata = append(metadata, payload.Error, payload.Detail, &payload.warmupErrorMetadata)
	// Prefer a code over a type regardless of envelope depth. Scan every field
	// for a terminal marker before accepting a generic diagnostic, so an outer
	// server_error cannot hide an inner auth/policy failure. Unknown metadata is
	// discarded rather than copied into persistent state.
	values := make([]string, 0, len(metadata)*2)
	for _, field := range []func(*warmupErrorMetadata) string{
		func(value *warmupErrorMetadata) string { return value.Code },
		func(value *warmupErrorMetadata) string { return value.Type },
	} {
		for _, value := range metadata {
			if value == nil {
				continue
			}
			values = append(values, field(value))
		}
	}
	return preferredWarmupMetadataCode(values...)
}

// preferredWarmupMetadataCode gives any terminal policy/auth marker precedence
// over generic transport metadata at every envelope depth. Responses and proxy
// wrappers can duplicate error metadata; a later server_error must never hide an
// earlier cyber_policy/auth_unavailable marker and make the request retryable.
func preferredWarmupMetadataCode(values ...string) string {
	for _, value := range values {
		if canonical := canonicalNonRetryableWarmupCode(sanitizeWarmupCode(value)); canonical != "" {
			return canonical
		}
	}
	for _, value := range values {
		if code := safeWarmupResponseMetadataCode(value); code != "" {
			return code
		}
	}
	return ""
}

// safeWarmupResponseMetadataCode is deliberately allowlisted. Although code
// and type are normally low-sensitivity fields, copying an arbitrary value
// would let a malformed upstream place a token or response text in scheduler
// state. HTTP status remains the fallback diagnostic for unknown metadata.
func safeWarmupResponseMetadataCode(raw string) string {
	code := sanitizeWarmupCode(raw)
	if canonical := canonicalNonRetryableWarmupCode(code); canonical != "" {
		return canonical
	}
	switch code {
	case "server_error", "internal_server_error", "upstream_error", "service_unavailable",
		"overloaded", "rate_limit_exceeded", "insufficient_quota", "invalid_request_error",
		"request_timeout", "bad_gateway", "gateway_timeout", "not_found",
		"response_failed", "response_incomplete":
		return code
	default:
		return ""
	}
}

func parseWarmupResponse(body []byte) (warmupStreamOutcome, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return warmupStreamOutcome{}, errWarmupStreamIncomplete
	}
	if strings.HasPrefix(trimmed, "{") {
		return parseWarmupJSON([]byte(trimmed))
	}
	return parseWarmupSSE(trimmed)
}

func parseWarmupJSON(body []byte) (warmupStreamOutcome, error) {
	var payload struct {
		Type     string               `json:"type"`
		Status   string               `json:"status"`
		Code     string               `json:"code"`
		Error    *warmupErrorMetadata `json:"error"`
		Response *struct {
			Status string               `json:"status"`
			Error  *warmupErrorMetadata `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return warmupStreamOutcome{}, fmt.Errorf("decode warmup response: %w", err)
	}
	event := strings.TrimSpace(payload.Type)
	status := strings.ToLower(strings.TrimSpace(payload.Status))
	code := warmupResponseFailureCode(body)
	if payload.Response != nil {
		if nested := strings.ToLower(strings.TrimSpace(payload.Response.Status)); nested != "" {
			status = nested
		}
	}
	if status == "completed" || (event == "response.completed" && status == "") {
		return warmupStreamOutcome{TerminalEvent: "response.completed"}, nil
	}
	if status == "failed" || status == "incomplete" || event == "response.failed" ||
		event == "response.incomplete" || event == "error" {
		err := &warmupStreamTerminalError{Event: event, Code: code}
		if err.Event == "" {
			err.Event = "response." + status
		}
		return warmupStreamOutcome{TerminalEvent: err.Event, ErrorCode: code}, err
	}
	return warmupStreamOutcome{}, errWarmupStreamIncomplete
}

func (e *warmupStreamTerminalError) Error() string {
	event := strings.TrimSpace(e.Event)
	if event == "" {
		event = "error"
	}
	if code := strings.TrimSpace(e.Code); code != "" {
		return fmt.Sprintf("warmup stream terminated with %s (%s)", event, code)
	}
	return fmt.Sprintf("warmup stream terminated with %s", event)
}

// parseWarmupSSE requires an explicit Responses terminal event. An HTTP 2xx is
// not sufficient: the upstream can emit response.failed/error inside a valid
// SSE response, or the connection can close before response.completed.
func parseWarmupSSE(body string) (warmupStreamOutcome, error) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	// executeWarmup already caps the management response at 2 MiB. Keep the SSE
	// parser independently bounded so a future caller cannot allocate without a
	// limit through one oversized event.
	scanner.Buffer(make([]byte, 64*1024), warmupMaxResponseBytes)

	var eventName string
	dataLines := make([]string, 0, 2)
	flush := func() (warmupStreamOutcome, bool, error) {
		if strings.TrimSpace(eventName) == "" && len(dataLines) == 0 {
			return warmupStreamOutcome{}, false, nil
		}
		out, terminal, err := classifyWarmupSSEEvent(eventName, strings.Join(dataLines, "\n"))
		eventName = ""
		dataLines = dataLines[:0]
		return out, terminal, err
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			out, terminal, err := flush()
			if terminal || err != nil {
				return out, err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventName = strings.TrimSpace(value)
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return warmupStreamOutcome{}, fmt.Errorf("read warmup SSE: %w", err)
	}
	if out, terminal, err := flush(); terminal || err != nil {
		return out, err
	}
	return warmupStreamOutcome{}, errWarmupStreamIncomplete
}

func classifyWarmupSSEEvent(eventName, data string) (warmupStreamOutcome, bool, error) {
	eventName = strings.TrimSpace(eventName)
	payloadType, responseStatus, errorCode := warmupSSEPayloadMetadata(data)
	if eventName == "" {
		eventName = payloadType
	}

	switch eventName {
	case "response.completed":
		if responseStatus != "" && responseStatus != "completed" {
			err := &warmupStreamTerminalError{Event: eventName, Code: "response_status_" + responseStatus}
			return warmupStreamOutcome{TerminalEvent: eventName, ErrorCode: err.Code}, true, err
		}
		return warmupStreamOutcome{TerminalEvent: eventName}, true, nil
	case "response.failed", "response.incomplete", "error":
		err := &warmupStreamTerminalError{Event: eventName, Code: errorCode}
		return warmupStreamOutcome{TerminalEvent: eventName, ErrorCode: errorCode}, true, err
	default:
		return warmupStreamOutcome{}, false, nil
	}
}

func warmupSSEPayloadMetadata(data string) (payloadType, responseStatus, errorCode string) {
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		return "", "", ""
	}
	var payload struct {
		Type     string `json:"type"`
		Response *struct {
			Status string `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return "", "", ""
	}
	payloadType = strings.TrimSpace(payload.Type)
	errorCode = warmupResponseFailureCode([]byte(data))
	if payload.Response != nil {
		responseStatus = strings.ToLower(strings.TrimSpace(payload.Response.Status))
	}
	return payloadType, responseStatus, errorCode
}

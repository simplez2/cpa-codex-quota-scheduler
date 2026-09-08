package main

import (
	"errors"
	"testing"
)

func TestParseWarmupJSONAcceptsOnlyCompletedStatus(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantEvent      string
		wantCode       string
		wantErr        bool
		wantIncomplete bool
	}{
		{name: "completed", body: `{"id":"resp_1","status":"completed","output":[]}`, wantEvent: "response.completed"},
		{name: "failed", body: `{"id":"resp_1","status":"failed","error":{"code":"auth_unavailable","message":"must not be persisted"}}`, wantEvent: "response.failed", wantCode: "auth_unavailable", wantErr: true},
		{name: "failed by error type", body: `{"id":"resp_1","status":"failed","error":{"type":"cyber_policy","message":"must not be persisted"}}`, wantEvent: "response.failed", wantCode: "cyber_policy", wantErr: true},
		{name: "output budget reached", body: `{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, wantEvent: "response.incomplete"},
		{name: "missing terminal status", body: `{"id":"resp_1","output":[]}`, wantErr: true, wantIncomplete: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := parseWarmupResponse([]byte(test.body))
			if (err != nil) != test.wantErr {
				t.Fatalf("out=%#v err=%v; wantErr=%v", out, err, test.wantErr)
			}
			if out.TerminalEvent != test.wantEvent || out.ErrorCode != test.wantCode {
				t.Fatalf("out=%#v; want event=%q code=%q", out, test.wantEvent, test.wantCode)
			}
			if test.wantIncomplete && !errors.Is(err, errWarmupStreamIncomplete) {
				t.Fatalf("err=%v; want errWarmupStreamIncomplete", err)
			}
		})
	}
}

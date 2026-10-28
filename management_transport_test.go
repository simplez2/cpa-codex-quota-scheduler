package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCPAManagementInventoryDoesNotForwardKeyThroughRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/inventory", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	cfg := defaultPluginConfig()
	cfg.CPAManagementURL = source.URL + "/v0/management/api-call"
	cfg.CPAManagementKeyFile = filepath.Join(t.TempDir(), "management-key")
	if err := os.WriteFile(cfg.CPAManagementKeyFile, []byte("synthetic-transport-test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cpaManagementAuthFiles(context.Background(), cfg); err == nil {
		t.Error("redirected inventory must fail closed")
	}
	if got := redirected.Load(); got != 0 {
		t.Errorf("management request followed a redirect %d times; bearer key must stay at the configured endpoint", got)
	}
}

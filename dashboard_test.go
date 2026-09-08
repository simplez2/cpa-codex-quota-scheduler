package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

func TestDashboardMenuAndAssets(t *testing.T) {
	resources := managementRegistration().Resources
	menus := 0
	for _, route := range resources {
		if route.Menu != "" {
			menus++
			if route.Menu != "Codex 额度调度" || route.Path != "/open" {
				t.Fatalf("unexpected dashboard menu: %+v", route)
			}
		}
		response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: dashboardResourcePrefix + route.Path})
		if response.StatusCode != http.StatusOK || len(response.Body) == 0 {
			t.Fatalf("resource %s unavailable: %d", route.Path, response.StatusCode)
		}
		if response.Headers.Get("Cache-Control") != "no-store" || response.Headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("unsafe asset response headers for %s", route.Path)
		}
		if route.Path == "/open" {
			body := string(response.Body)
			for _, required := range []string{"./dashboard.css", "./dashboard.mjs", "v" + pluginVersion} {
				if !strings.Contains(body, required) {
					t.Fatalf("dashboard missing %s", required)
				}
			}
			if !strings.Contains(response.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
				t.Fatal("dashboard must constrain authenticated requests to the same origin")
			}
		}
	}
	if menus != 1 || len(resources) != 5 {
		t.Fatalf("resources=%d menus=%d", len(resources), menus)
	}
	// JSON endpoints must remain behind management authentication, never menus.
	for _, route := range managementRegistration().Routes {
		if route.Menu != "" {
			t.Fatalf("management data route %s exposed as public menu", route.Path)
		}
	}
}

func TestDashboardPublicResourcesAreStaticAndExact(t *testing.T) {
	before := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: dashboardResourcePrefix + "/open"})
	withWarmupManagementState(t, map[string]warmupEntry{
		"private": {AuthID: "private-account-must-not-appear", Error: "private-runtime-state"},
	})
	after := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: dashboardResourcePrefix + "/open"})
	if string(before.Body) != string(after.Body) {
		t.Fatal("public shell depends on private runtime state")
	}
	for _, route := range managementRegistration().Resources {
		response := dispatchManagement(pluginapi.ManagementRequest{Method: http.MethodGet, Path: dashboardResourcePrefix + route.Path})
		if strings.Contains(string(response.Body), "private-account-must-not-appear") || strings.Contains(string(response.Body), "private-runtime-state") {
			t.Fatal("private state leaked into a public resource")
		}
	}
	for _, path := range []string{dashboardResourcePrefix + "/quota", dashboardResourcePrefix + "/open/", "/other/open", dashboardResourcePrefix + "/../index.html"} {
		if _, ok := dashboardResource(http.MethodGet, path); ok {
			t.Fatalf("unexpected public resource: %s", path)
		}
	}
	if _, ok := dashboardResource(http.MethodPost, dashboardResourcePrefix+"/open"); ok {
		t.Fatal("public resource accepted a mutation method")
	}
}

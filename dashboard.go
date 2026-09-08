package main

import (
	"embed"
	"net/http"
	"strings"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
)

// The resource namespace is public. Embed only static assets; account state and
// credentials must continue to travel through CPA's authenticated management API.
//
//go:embed web/index.html web/dashboard.css web/dashboard.mjs web/session.mjs web/settings.mjs
var dashboardFiles embed.FS

const dashboardResourcePrefix = "/v0/resource/plugins/" + pluginName

func dashboardResources() []pluginapi.ResourceRoute {
	return []pluginapi.ResourceRoute{
		{Path: "/open", Menu: "Codex 额度调度", Description: "管理账号与额度、均衡并发调度、套餐分配和预热。"},
		{Path: "/dashboard.css"},
		{Path: "/dashboard.mjs"},
		{Path: "/session.mjs"},
		{Path: "/settings.mjs"},
	}
}

func dashboardResource(method, path string) (pluginapi.ManagementResponse, bool) {
	if method != http.MethodGet {
		return pluginapi.ManagementResponse{}, false
	}
	var file, contentType string
	switch path {
	case dashboardResourcePrefix + "/open":
		file, contentType = "index.html", "text/html; charset=utf-8"
	case dashboardResourcePrefix + "/dashboard.css":
		file, contentType = "dashboard.css", "text/css; charset=utf-8"
	case dashboardResourcePrefix + "/dashboard.mjs":
		file, contentType = "dashboard.mjs", "text/javascript; charset=utf-8"
	case dashboardResourcePrefix + "/session.mjs":
		file, contentType = "session.mjs", "text/javascript; charset=utf-8"
	case dashboardResourcePrefix + "/settings.mjs":
		file, contentType = "settings.mjs", "text/javascript; charset=utf-8"
	default:
		return pluginapi.ManagementResponse{}, false
	}
	body, err := dashboardFiles.ReadFile("web/" + file)
	if err != nil {
		return jsonManagementResponse(http.StatusInternalServerError, map[string]string{"error": "dashboard_asset_missing"}), true
	}
	if file == "index.html" {
		body = []byte(strings.ReplaceAll(string(body), "{{VERSION}}", pluginVersion))
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {contentType},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Referrer-Policy":         {"no-referrer"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: body,
	}, true
}

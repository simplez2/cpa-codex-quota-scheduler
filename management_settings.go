package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/simplez2/cpa-codex-quota-scheduler/cpasdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// The browser persists validated, changed fields through CPA's authenticated
// PATCH config endpoint. This plugin never writes the host's config file or
// exports the contents of its management-key file.
func panelSettingsValues(cfg pluginConfig) map[string]any {
	values := make(map[string]any)
	fields := reflect.TypeOf(yamlPluginConfig{})
	source := reflect.ValueOf(cfg)
	for i := 0; i < fields.NumField(); i++ {
		field := fields.Field(i)
		name := field.Tag.Get("yaml")
		// Disabling the host plugin also removes its panel and API. Lifecycle
		// installation/removal belongs to CPA; all runtime settings are editable.
		if name == "enabled" {
			continue
		}
		value := source.FieldByName(field.Name).Interface()
		if duration, ok := value.(time.Duration); ok {
			value = duration.String()
		}
		if name == "quota_account_plans" && cfg.QuotaAccountPlans == nil {
			value = map[string]string{}
		}
		values[name] = value
	}
	return values
}

func currentPanelSettings() map[string]any {
	schedulerRuntime.mu.RLock()
	values := panelSettingsValues(schedulerRuntime.cfg)
	generation := schedulerRuntime.configGeneration
	schedulerRuntime.mu.RUnlock()
	return map[string]any{"values": values, "defaults": panelSettingsValues(defaultPluginConfig()), "generation": generation}
}

func handleSettingsValidate(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if len(req.Body) == 0 || len(req.Body) > 128<<10 {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "invalid_body"})
	}
	var body struct {
		Config  map[string]any `json:"config"`
		Changes map[string]any `json:"changes"`
	}
	if json.Unmarshal(req.Body, &body) != nil || body.Changes == nil {
		return jsonManagementResponse(http.StatusBadRequest, map[string]string{"error": "invalid_body"})
	}
	changes, fields := validatePanelSettings(body.Config, body.Changes)
	if len(fields) != 0 {
		return jsonManagementResponse(http.StatusUnprocessableEntity, map[string]any{"error": "invalid_settings", "fields": fields})
	}
	return jsonManagementResponse(http.StatusOK, map[string]any{"changes": changes})
}

func validatePanelSettings(config, changes map[string]any) (map[string]any, map[string]string) {
	defaults := panelSettingsValues(defaultPluginConfig())
	fields := map[string]string{}
	merged := map[string]any{}
	for name, value := range config {
		if _, known := defaults[name]; known {
			merged[name] = value
		}
	}
	for name, value := range changes {
		if _, known := defaults[name]; !known {
			fields[name] = "不支持此设置"
			continue
		}
		if value == nil {
			delete(merged, name)
		} else {
			merged[name] = value
		}
	}
	encoded, err := yaml.Marshal(merged)
	var parsed pluginConfig
	if err == nil {
		parsed, err = parsePluginConfig(encoded)
	}
	if err != nil {
		// Parser errors can contain user input: don't reflect a pasted token or
		// credential-bearing URL in the response, browser toast, or logs.
		fields["_form"] = "参数格式或范围不正确，请检查数值、时间、套餐和模型名称。"
		return nil, fields
	}
	actual := panelSettingsValues(parsed)
	out := map[string]any{}
	for name, value := range changes {
		if _, known := defaults[name]; !known {
			continue
		}
		if value == nil {
			out[name] = nil
			continue
		}
		if !equivalentPanelSetting(value, actual[name]) {
			fields[name] = "值超出有效范围或格式不正确，请按字段说明填写。"
		} else {
			out[name] = actual[name]
		}
	}
	// Parser normalization of related fields must also be rejected, otherwise
	// a change can silently reset a different, previously valid operator choice.
	for name, value := range merged {
		if !equivalentPanelSetting(value, actual[name]) {
			fields[name] = "此值与其他设置冲突，或需要修正后才能生效。"
		}
	}
	for _, name := range []string{"cpa_management_url", "warmup_sidecar_url", "quota_url"} {
		value := fmt.Sprint(actual[name])
		endpoint, err := url.Parse(value)
		if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
			fields[name] = "请输入不含密钥、查询参数或片段的完整 HTTP(S) 地址。"
		}
	}
	for _, name := range []string{"state_path", "cpa_management_key_file"} {
		value := fmt.Sprint(actual[name])
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\x00\r\n") {
			fields[name] = "请输入有效的服务器文件路径。"
		}
	}
	return out, fields
}

func equivalentPanelSetting(left, right any) bool {
	if a, ok := left.(string); ok {
		if b, ok := right.(string); ok {
			if strings.TrimSpace(a) == b {
				return true
			}
			ad, ae := time.ParseDuration(a)
			bd, be := time.ParseDuration(b)
			return ae == nil && be == nil && ad == bd
		}
	}
	a, ae := json.Marshal(left)
	b, be := json.Marshal(right)
	return ae == nil && be == nil && string(a) == string(b)
}

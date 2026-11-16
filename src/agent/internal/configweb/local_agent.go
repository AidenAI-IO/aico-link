package configweb

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/configupdate"
	"aico-agent/internal/netproxy"
)

// Host settings deliberately exclude device services and settings overridden
// by launch flags. The existing DTO remains authoritative for model fields.
var localAgentFields = map[string]bool{
	"locale": true, "timezone": true, "prompt": true,
	"max_iterations": true, "context_prune_threshold": true,
	"context_prune_state_limit": true, "context_compaction_threshold": true,
	"ui_grounding_enabled": true,
}

var localConfigFields = map[string]map[string]bool{
	"model":           jsonFields(configupdate.Model{}),
	"model_providers": jsonFields(configupdate.ModelProvider{}),
	"decision":        jsonFields(configupdate.DecisionModel{}),
	"search":          jsonFields(configupdate.Search{}),
	"log":             jsonFields(configupdate.Log{}),
	"agent":           localAgentFields,
}

func jsonFields(value any) map[string]bool {
	fields := map[string]bool{}
	t := reflect.TypeOf(value)
	for i := 0; i < t.NumField(); i++ {
		key := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if key != "" && key != "-" {
			fields[key] = true
		}
	}
	return fields
}

func validateLocalAgentOptions(o Options) error {
	if o.BindAddress != "127.0.0.1" {
		return fmt.Errorf("--local-agent requires --bind 127.0.0.1")
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	for name, value := range map[string]string{"config": o.AgentConfigPath, "web-root": o.WebRoot} {
		if !filepath.IsAbs(value) {
			return fmt.Errorf("--%s must be an absolute path for a local Agent", name)
		}
	}
	u, err := url.Parse(o.AgentHTTPBaseURL)
	if err != nil || u.Scheme != "http" || !netproxy.IsLoopbackHost(u.Hostname()) || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--agent-url must be a loopback HTTP origin")
	}
	return nil
}

func (s *Server) allowLocalRequest(w http.ResponseWriter, r *http.Request) bool {
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	if !netproxy.IsLoopbackHost(host) || !isSameOriginBrowserRequest(r) {
		writeJSONError(w, http.StatusForbidden, "local settings require a same-origin loopback request")
		return false
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	return true
}

func localAgentEndpoint(endpoint apiEndpoint) bool {
	switch endpoint {
	case apiIPhoneMirroring, apiIPhoneMirroringReconnect, apiDeviceSnapshot, apiDeviceStatus, apiConfigGet, apiConfigSchema, apiConfigUpdate,
		apiConfigPlan, apiConfigLocale, apiConfigApplication, apiConfigTest, apiModels,
		apiConversationReset, apiAgentLogs, apiLLMLogs, apiLLMLogExport, apiLLMLogImport:
		return true
	}
	return false
}

func isLocalSettingsRoute(path string) bool {
	switch strings.TrimRight(path, "/") {
	case "", "/basic/language", "/conversation", "/conversation/prompt", "/conversation/tools",
		"/conversation/tools/provider", "/model", "/model/main", "/model/decision", "/model/providers",
		"/model/providers/edit", "/model/models", "/advanced/logs", "/advanced/logs/agent", "/advanced/logs/level":
		return true
	}
	return false
}

func validateLocalConfigPatch(raw json.RawMessage) error {
	var patch map[string]json.RawMessage
	if json.Unmarshal(raw, &patch) != nil || patch == nil {
		return fmt.Errorf("config must be an object")
	}
	for section, data := range patch {
		allowed, ok := localConfigFields[section]
		if !ok {
			return fmt.Errorf("%s is not available for a local Agent", section)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return fmt.Errorf("%s must be an object", section)
		}
		if section == "model_providers" {
			for name, provider := range fields {
				// Provider deletion is validated against selected models by config-update.
				if string(provider) == "null" {
					continue
				}
				if err := validateLocalFields(section+"."+name, provider, allowed); err != nil {
					return err
				}
			}
		} else if err := validateLocalFields(section, data, allowed); err != nil {
			return err
		}
	}
	return nil
}

func validateLocalFields(section string, raw json.RawMessage, allowed map[string]bool) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fmt.Errorf("%s must be an object", section)
	}
	for key := range fields {
		if !allowed[key] {
			return fmt.Errorf("%s.%s is not available for a local Agent", section, key)
		}
	}
	return nil
}

// Projection runs after the canonical CLI has removed credential values.
func (s *Server) projectConfig(config map[string]any) map[string]any {
	if !s.options.LocalAgent {
		return config
	}
	result := map[string]any{}
	for section, fields := range localConfigFields {
		value, ok := config[section]
		if !ok {
			continue
		}
		if section != "agent" {
			result[section] = value
			continue
		}
		filtered := map[string]any{}
		if values, ok := value.(map[string]any); ok {
			for key, value := range values {
				if fields[key] {
					filtered[key] = value
				}
			}
		}
		result[section] = filtered
	}
	return result
}

func localAgentMetadata(value any) any {
	metadata, _ := value.(map[string]any)
	sections, _ := metadata["sections"].([]any)
	filtered := []any{}
	for _, entry := range sections {
		section, _ := entry.(map[string]any)
		name, _ := section["name"].(string)
		allowed, ok := localConfigFields[name]
		if !ok {
			continue
		}
		fields, _ := section["fields"].([]any)
		kept := []any{}
		for _, entry := range fields {
			field, _ := entry.(map[string]any)
			key, _ := field["key"].(string)
			if allowed[key] {
				kept = append(kept, field)
			}
		}
		filtered = append(filtered, map[string]any{"name": name, "fields": kept})
	}
	return map[string]any{"sections": filtered}
}

func (s *Server) localAgentStatus() map[string]any {
	status := map[string]any{"state": "offline", "process_running": false, "port_reachable": false, "url": s.options.AgentHTTPBaseURL}
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(strings.TrimRight(s.options.AgentHTTPBaseURL, "/") + "/health")
	if err != nil {
		return status
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	var payload struct {
		OK     bool `json:"ok"`
		Health struct {
			Status     string `json:"status"`
			DeviceType string `json:"device_type"`
			BridgeType string `json:"bridge_type"`
		} `json:"data"`
	}
	if err != nil || len(body) > maxRequestBodySize || response.StatusCode != http.StatusOK ||
		json.Unmarshal(body, &payload) != nil || !payload.OK || payload.Health.Status != "ok" || payload.Health.BridgeType != "go-agent" {
		return status
	}
	status["state"], status["process_running"], status["port_reachable"] = "running", true, true
	status["device_type"] = payload.Health.DeviceType
	return status
}

func (s *Server) writeLocalSnapshot(w http.ResponseWriter, config map[string]any, valid bool, validationErrors []agent.ConfigValidationError) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "profile": "local", "config": s.projectConfig(config),
		"config_valid": valid, "config_errors": validationErrors,
		"agent_status": s.localAgentStatus(),
		"paths":        map[string]string{"agent_config": s.options.AgentConfigPath},
	})
}

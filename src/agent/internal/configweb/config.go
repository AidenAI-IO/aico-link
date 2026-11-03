package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/logging"
	"aico-agent/internal/wifiproxy"
)

func (s *Server) runAgentCLI(timeout time.Duration, input []byte, args ...string) commandResult {
	env, err := s.agentCommandEnvironment()
	if err != nil {
		return commandResult{ExitCode: 126, Output: []byte(err.Error())}
	}
	return runCommand(timeout, env, input, s.options.AgentBinary, args...)
}

// configValidationState reports the recovery state the page renders: whether the
// Agent runtime accepts the persisted config, and which field to highlight. The
// runtime loader is the authority because it is the loader the Agent boots with,
// so the verdict here cannot drift from the one that keeps the Agent down.
//
// Callers reach this only after `agent config` has already read the same file
// through the editor loader, which rejects an unreadable, undecodable, or
// non-file target before a form is rendered. An unusable file therefore answers
// unavailable from that step, and a failure here names a field to repair.
//
// An absent file is the one exception the runtime loader cannot answer for: there
// is nothing to read a verdict from, and the page renders the built-in defaults so
// a save can create the file.
func configValidationState(path string) (bool, []agent.ConfigValidationError) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return true, []agent.ConfigValidationError{}
	}
	if _, err := agent.LoadRuntimeConfig(path); err != nil {
		return false, agent.ParseConfigValidationErrors(err)
	}
	return true, []agent.ConfigValidationError{}
}

// logAgentRecoveryState names the persisted value that keeps the Agent from
// starting. The startup log is the first place a field engineer looks when the
// Agent is down, so it is reported once, where the portal starts serving.
func (s *Server) logAgentRecoveryState() {
	valid, validationErrors := configValidationState(s.options.AgentConfigPath)
	if valid || len(validationErrors) == 0 {
		return
	}
	logging.Warnf("config_web", "config_web", "Agent config is invalid; starting recovery portal: %s", validationErrors[0].Message)
}

// handleGetConfig returns only the persisted agent.toml projection. Device,
// Wi-Fi, firmware and environment state live behind the explicit snapshot
// resource so callers can evolve each resource independently.
func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	result := s.runAgentCLI(5*time.Second, nil, "config", "--config="+s.options.AgentConfigPath, "--format=json")
	if result.TimedOut {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config timed out")
		return
	}
	if result.ExitCode != 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config unavailable")
		return
	}
	var config map[string]any
	if err := json.Unmarshal(result.Output, &config); err != nil || config == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config returned invalid JSON")
		return
	}
	valid, validationErrors := configValidationState(s.options.AgentConfigPath)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": config,
		"config_valid": valid, "config_errors": validationErrors,
	})
}

func (s *Server) handleGetDeviceSnapshot(w http.ResponseWriter, _ *http.Request) {
	result := s.runAgentCLI(5*time.Second, nil, "config", "--config="+s.options.AgentConfigPath, "--format=json")
	if result.TimedOut {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config timed out")
		return
	}
	if result.ExitCode != 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config unavailable")
		return
	}
	var config map[string]any
	if err := json.Unmarshal(result.Output, &config); err != nil || config == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config returned invalid JSON")
		return
	}
	valid, validationErrors := configValidationState(s.options.AgentConfigPath)
	wifi, wifiErr := loadWiFiConfig(s.options.WiFiConfigPath)
	wifiProxy, wifiProxyErr := wifiproxy.Load(s.options.WiFiProxyConfigPath)
	systemEnv := ""
	if data, err := readFileLimited(s.options.SystemEnvPath, maxSystemEnvSize); err == nil {
		systemEnv = string(data)
	}
	response := map[string]any{
		"ok":            true,
		"config":        config,
		"config_valid":  valid,
		"config_errors": validationErrors,
		"wifi":          wifi.publicValue(wifiProxy),
		"wifi_status":   s.queryWiFiStatus(),
		"agent_status":  s.queryAgentStatus(),
		"firmware":      s.firmwareInfo(),
		"system_env":    systemEnv,
		"storage":       s.storageStatusValue(),
		"paths": map[string]string{
			"agent_config":   s.options.AgentConfigPath,
			"wifi_config":    s.options.WiFiConfigPath,
			"wifi_interface": s.options.WiFiInterface,
			"system_env":     s.options.SystemEnvPath,
		},
	}
	if !valid && len(validationErrors) > 0 {
		response["config_error"] = validationErrors[0].Message
	}
	if wifiErr != nil && !os.IsNotExist(wifiErr) {
		response["wifi_error"] = wifiErr.Error()
	}
	if wifiProxyErr != nil {
		response["wifi_proxy_error"] = wifiProxyErr.Error()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleConfigMeta(w http.ResponseWriter, _ *http.Request) {
	result := s.runAgentCLI(5*time.Second, nil, "config-meta", "--format=json")
	if result.TimedOut || result.ExitCode != 0 || !json.Valid(result.Output) {
		message := "config metadata unavailable"
		if result.ExitCode == 127 {
			message = "agent config unavailable: agent binary not found"
		}
		writeJSONError(w, http.StatusServiceUnavailable, message)
		return
	}
	var metadata any
	if err := json.Unmarshal(result.Output, &metadata); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "config metadata unavailable")
		return
	}
	writeJSON(w, http.StatusOK, metadata)
}

// updateConfig runs `agent config-update` on a patch. Extra flags such as
// `--dry-run` are passed through.
func (s *Server) updateConfig(config json.RawMessage, flags ...string) (map[string]any, int, error) {
	body, err := json.Marshal(map[string]json.RawMessage{"config": config})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	args := append([]string{"config-update", "--config=" + s.options.AgentConfigPath, "--stdin", "--format=json"}, flags...)
	result := s.runAgentCLI(10*time.Second, body, args...)
	if result.TimedOut {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent config update timed out")
	}
	var response map[string]any
	decoder := json.NewDecoder(bytes.NewReader(result.Output))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil || response == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent config update returned invalid JSON")
	}
	ok, _ := response["ok"].(bool)
	if result.ExitCode != 0 || !ok {
		message, _ := response["error"].(string)
		if message == "" {
			message = "agent config update rejected"
		}
		status := http.StatusBadRequest
		if result.ExitCode == 127 {
			status = http.StatusServiceUnavailable
		} else if response["error_kind"] == "internal" {
			status = http.StatusInternalServerError
		}
		return nil, status, fmt.Errorf("%s", message)
	}
	return response, http.StatusOK, nil
}

// readConfigPatch reads a `{"config": {...}}` request body shared by the save
// and plan endpoints.
func readConfigPatch(w http.ResponseWriter, r *http.Request) (json.RawMessage, map[string]json.RawMessage, bool) {
	var request map[string]json.RawMessage
	if !readJSONBody(w, r, &request) {
		return nil, nil, false
	}
	if request == nil {
		writeJSONError(w, http.StatusBadRequest, "request body must be an object")
		return nil, nil, false
	}
	for key := range request {
		if key != "config" {
			writeJSONError(w, http.StatusBadRequest, "only the 'config' field is accepted")
			return nil, nil, false
		}
	}
	config, present := request["config"]
	if !present {
		writeJSONError(w, http.StatusBadRequest, "missing config object")
		return nil, nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(config, &object); err != nil || object == nil {
		writeJSONError(w, http.StatusBadRequest, "config patch must be an object")
		return nil, nil, false
	}
	return config, object, true
}

// handlePlanConfig reports what saving a patch would need before it takes
// effect, without persisting anything, so a page can label its save button.
func (s *Server) handlePlanConfig(w http.ResponseWriter, r *http.Request) {
	if !isSameOriginBrowserRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	config, _, ok := readConfigPatch(w, r)
	if !ok {
		return
	}
	plan, status, err := s.updateConfig(config, "--dry-run")
	if err != nil {
		writeJSONError(w, status, err.Error())
		return
	}
	apply, _ := plan["apply"].(string)
	if apply == "" {
		apply = string(agent.ApplyLive)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"apply":           apply,
		"apply_reasons":   plan["apply_reasons"],
		"changed_paths":   stringSlice(plan["changed_paths"]),
		"reboot_required": apply == string(agent.ApplyReboot),
	})
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	config, object, ok := readConfigPatch(w, r)
	if !ok {
		return
	}
	s.configSaveMu.Lock()
	defer s.configSaveMu.Unlock()
	s.configMu.Lock()
	update, status, err := s.updateConfig(config)
	if err != nil {
		s.configMu.Unlock()
		writeJSONError(w, status, err.Error())
		return
	}

	changed := stringSlice(update["changed_paths"])
	rebootRequired, _ := update["reboot_required"].(bool)
	revision := uint64Value(update["revision"])
	persisted, persistedField := update["persisted"].(bool)
	if !persistedField {
		// The current config-update contract must report persistence explicitly.
		s.configMu.Unlock()
		writeJSONError(w, http.StatusServiceUnavailable, "agent config update omitted persisted state")
		return
	}
	frameServiceChanged := containsAnyString(changed,
		"frame_service.keep_streamon",
		"advanced_settings.hardware.frame_service.keep_streamon",
	)
	if frameServiceChanged {
		s.frameApplyPending = true
	}
	_, storageRequested := object["storage"]
	if hasConfigPathPrefixAny(changed, "storage", "storage_settings.storage") || storageRequested {
		s.storageApplyPending = true
	}
	s.configSavePending = true
	s.configMu.Unlock()
	defer s.finishConfigSave()
	if err := s.applyConfigServices(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "config": update["config"], "persisted": true, "applied": false, "state": "failed", "error": err.Error(), "reboot_required": rebootRequired, "agent_restart_scheduled": false})
		return
	}
	applied, pending := false, false
	payload, reloadErr := s.reloadAgentConfig(r.Context(), revision)
	if reloadErr != nil {
		s.configMu.Lock()
		s.configApplyError = reloadErr.Error()
		s.configApplyErrorRevision = revision
		s.configMu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "config": update["config"], "persisted": persisted, "applied": false,
			"revision": revision, "changed_paths": changed, "reboot_required": rebootRequired,
			"state": "failed", "error": reloadErr.Error(),
		})
		return
	}
	applied, _ = payload["applied"].(bool)
	pending, _ = payload["pending"].(bool)
	if required, ok := payload["reboot_required"].(bool); ok {
		rebootRequired = required
	}
	message := "config saved"
	apply, _ := update["apply"].(string)
	if apply == "" {
		apply = string(agent.ApplyLive)
	}
	if rebootRequired {
		// The Agent's own reload can still report a reboot the plan missed.
		message = "config saved; USB HID configuration changed; reboot required"
		apply = string(agent.MaxApplyLevel(agent.ApplyLevel(apply), agent.ApplyReboot))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                              true,
		"config":                          update["config"],
		"persisted":                       persisted,
		"applied":                         applied,
		"pending":                         pending,
		"state":                           payload["state"],
		"revision":                        revision,
		"changed_paths":                   changed,
		"reboot_required":                 rebootRequired,
		"reboot_reasons":                  update["reboot_reasons"],
		"apply":                           apply,
		"apply_reasons":                   update["apply_reasons"],
		"agent_restart_scheduled":         false,
		"frame_service_restart_scheduled": frameServiceChanged,
		"message":                         message,
	})
}

func (s *Server) handlePutLocale(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Locale *string `json:"locale"`
	}
	if !readJSONBody(w, r, &request) {
		return
	}
	if request.Locale == nil {
		writeJSONError(w, http.StatusBadRequest, "missing locale string")
		return
	}
	if *request.Locale != "zh-CN" && *request.Locale != "en-US" {
		writeJSONError(w, http.StatusBadRequest, "unsupported locale; expected zh-CN or en-US")
		return
	}
	s.configSaveMu.Lock()
	defer s.configSaveMu.Unlock()
	s.configMu.Lock()
	config, _ := json.Marshal(map[string]any{"agent": map[string]string{"locale": *request.Locale}})
	update, status, err := s.updateConfig(config)
	if err != nil {
		s.configMu.Unlock()
		writeJSONError(w, status, err.Error())
		return
	}
	s.configSavePending = true
	s.configMu.Unlock()
	defer s.finishConfigSave()
	if err := s.applyConfigServices(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "persisted": true, "applied": false, "state": "failed", "locale": *request.Locale, "error": err.Error()})
		return
	}
	revision := uint64Value(update["revision"])
	payload, err := s.reloadAgentConfig(r.Context(), revision)
	if err != nil {
		s.configMu.Lock()
		s.configApplyError = err.Error()
		s.configApplyErrorRevision = revision
		s.configMu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "persisted": true, "applied": false, "locale": *request.Locale,
			"agent_restart_scheduled": false, "revision": revision, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "locale": *request.Locale, "persisted": true, "applied": payload["applied"],
		"pending": payload["pending"], "state": payload["state"], "reboot_required": payload["reboot_required"], "revision": revision, "message": "locale saved",
	})
}

// applyConfigServices serializes service replacement while keeping configMu
// available to status requests during slow frame-service restarts.
func (s *Server) applyConfigServices() error {
	s.configServiceMu.Lock()
	defer s.configServiceMu.Unlock()

	s.configMu.Lock()
	framePending := s.frameApplyPending
	storagePending := s.storageApplyPending
	if framePending {
		s.frameApplyPending = false
	}
	if storagePending {
		s.storageApplyPending = false
	}
	s.configMu.Unlock()

	var failures []string
	if framePending {
		if err := s.restartFrameService(); err != nil {
			failures = append(failures, err.Error())
			s.configMu.Lock()
			s.frameApplyPending = true
			s.configMu.Unlock()
		}
	}
	if storagePending {
		if err := s.reconfigureStorage(); err != nil {
			failures = append(failures, err.Error())
			s.configMu.Lock()
			s.storageApplyPending = true
			s.configMu.Unlock()
		}
	}
	s.configMu.Lock()
	s.configApplyError = strings.Join(failures, "; ")
	s.configApplyErrorRevision = 0
	applyError := s.configApplyError
	s.configMu.Unlock()
	if applyError != "" {
		return fmt.Errorf("%s", applyError)
	}
	return nil
}

func (s *Server) restartFrameService() error {
	path := strings.TrimSpace(s.options.FrameServiceInitScript)
	if path == "" {
		return fmt.Errorf("frame service init script path is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "restart")
	cmd.Env = append(os.Environ(), "AGENT_CONFIG="+s.options.AgentConfigPath)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("frame service restart: %w", ctx.Err())
		}
		if _, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("frame service restart: %w", err)
		}
		cmd = exec.CommandContext(ctx, "/bin/sh", path, "restart")
		cmd.Env = append(os.Environ(), "AGENT_CONFIG="+s.options.AgentConfigPath)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("frame service restart: %w", err)
		}
	}
	cfg, err := agent.LoadRuntimeConfig(s.options.AgentConfigPath)
	if err != nil {
		return fmt.Errorf("read frame service config: %w", err)
	}
	return waitForFrameService(ctx, cfg.HID.FrameSocketOrDefault())
}

func waitForFrameService(ctx context.Context, socket string) error {
	// The init script launches a watchdog before the HDMI service starts.
	// TC358743 EDID negotiation alone takes several seconds.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		conn, err := (&net.Dialer{Timeout: 500 * time.Millisecond}).DialContext(ctx, "unix", socket)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("frame service not ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stringSlice(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func uint64Value(value any) uint64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := strconv.ParseUint(string(typed), 10, 64)
		return parsed
	case float64:
		if typed < 0 || typed > float64(^uint64(0)) {
			return 0
		}
		return uint64(typed)
	case uint64:
		return typed
	case string:
		parsed, _ := strconv.ParseUint(strings.TrimSpace(typed), 10, 64)
		return parsed
	default:
		return 0
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasConfigPathPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if value == prefix || strings.HasPrefix(value, prefix+".") {
			return true
		}
	}
	return false
}

func containsAnyString(values []string, targets ...string) bool {
	for _, target := range targets {
		if containsString(values, target) {
			return true
		}
	}
	return false
}

func hasConfigPathPrefixAny(values []string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if hasConfigPathPrefix(values, prefix) {
			return true
		}
	}
	return false
}

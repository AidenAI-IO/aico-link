package configweb

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/wifiproxy"
)

type configTestRequest struct {
	Section string          `json:"section"`
	Values  json.RawMessage `json:"values"`
	Text    string          `json:"text"`
	Audio   string          `json:"audio_base64"`
}

// handleConfigTest keeps the device-only checks in the portal and delegates
// provider checks to the agent command, which owns the runtime adapters.
func (s *Server) handleConfigTest(w http.ResponseWriter, r *http.Request) {
	var request configTestRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	section := strings.TrimSpace(request.Section)
	if section == "" {
		writeJSONError(w, http.StatusBadRequest, "missing 'section' field")
		return
	}
	if len(request.Values) == 0 || string(request.Values) == "null" {
		writeJSONError(w, http.StatusBadRequest, "missing 'values' object")
		return
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(request.Values, &values); err != nil || values == nil {
		writeJSONError(w, http.StatusBadRequest, "missing 'values' object")
		return
	}
	if section == "model" || section == "tts" || section == "stt" || section == "decision" {
		s.handleProviderConfigTest(w, r, section, request)
		return
	}

	results := make([]map[string]any, 0)
	passed := true
	add := func(check string, ok bool, detail string) {
		results = append(results, map[string]any{"check": check, "passed": ok, "detail": detail})
		if !ok {
			passed = false
		}
	}
	stringValue := func(key string) (string, bool) {
		var value string
		raw, ok := values[key]
		if !ok || json.Unmarshal(raw, &value) != nil {
			return "", false
		}
		return strings.TrimSpace(value), true
	}
	numberValue := func(key string) (float64, bool) {
		raw, ok := values[key]
		if !ok {
			return 0, false
		}
		var value float64
		if json.Unmarshal(raw, &value) != nil {
			return 0, false
		}
		return value, true
	}
	boolValue := func(key string) (bool, bool) {
		raw, ok := values[key]
		if !ok {
			return false, false
		}
		var value bool
		if json.Unmarshal(raw, &value) != nil {
			return false, false
		}
		return value, true
	}

	switch section {
	case "audio":
		path, _ := stringValue("socket")
		if path == "" {
			add("socket_exists", false, "socket path is empty")
		} else {
			info, err := os.Stat(path)
			ok := err == nil && info.Mode()&os.ModeSocket != 0
			detail := path + " not found"
			if ok {
				detail = path + " exists"
			}
			add("socket_exists", ok, detail)
		}
	case "log":
		value, ok := numberValue("llm_http_retention_days")
		if !ok {
			add("llm_http_retention_days", false, "not a number")
		} else if value < 0 || value != float64(int(value)) {
			add("llm_http_retention_days", false, fmt.Sprintf("must be >= 0, got %s", formatNumber(value)))
		} else {
			detail := fmt.Sprintf("%d days", int(value))
			if value == 0 {
				detail = "0 (default 7 days)"
			}
			add("llm_http_retention_days", true, detail)
		}
	case "device":
		_, present := values["device_type"]
		value, ok := stringValue("device_type")
		if !present {
			value = "iOS"
			ok = true
		}
		normalized := normalizeDeviceType(value)
		valid := normalized == "iOS" || normalized == "Android" || normalized == "macOS" || normalized == "windows" || normalized == "linux"
		detail := "must be iOS, Android, macOS, windows, or linux"
		if present && !ok {
			valid = false
			detail = "must be a string"
		}
		if valid {
			detail = "effective pointer_mode: " + pointerModeForDeviceType(normalized)
		}
		add("device_type", valid, detail)
	case "hid":
		backend, _ := stringValue("input_backend")
		backend = normalizeInputBackend(backend)
		for _, key := range []string{"keyboard_device", "mouse_device", "android_keyboard_device"} {
			path, _ := stringValue(key)
			if backend == "adb" {
				add(key, true, "skipped when input_backend=adb")
				continue
			}
			if path == "" {
				add(key, false, "path is empty")
				continue
			}
			info, err := os.Stat(path)
			add(key, err == nil && info.Mode()&os.ModeCharDevice != 0, path+map[bool]string{true: " exists", false: " not found"}[err == nil && info.Mode()&os.ModeCharDevice != 0])
		}
		valid := backend == "hid" || backend == "adb"
		add("input_backend", valid, "effective backend: "+backend)
	case "agent":
		// An unset or empty input_mode is valid: voice providers are opt-in, so a
		// device that configured none has no mode, and the Agent stays reachable
		// through the Web UI and HTTP API. This handler sees only the agent
		// section's values and has no provider context, so it cannot resolve the
		// effective mode -- Config.Validate() does that over the full candidate.
		_, modePresent := values["input_mode"]
		mode, modeIsString := stringValue("input_mode")
		switch {
		case modePresent && !modeIsString:
			add("input_mode", false, "must be a string")
		case !modePresent || mode == "":
			add("input_mode", true, "unset (voice not configured; Web UI and HTTP API only)")
		case strings.EqualFold(mode, "audio") || strings.EqualFold(mode, "text"):
			add("input_mode", false, "invalid input_mode: "+mode+" (text and audio modes have been removed; use stt or realtime)")
		default:
			add("input_mode", mode == "stt" || mode == "realtime", fmt.Sprintf("got '%s', allowed: stt/realtime", mode))
		}
		if value, ok := numberValue("vad_speech_threshold"); !ok {
			add("vad_speech_threshold", false, "not a number")
		} else {
			add("vad_speech_threshold", value >= 0 && value <= 1, fmt.Sprintf("must be in range [0.0, 1.0], got %s", formatNumber(value)))
		}
		if value, ok := numberValue("screen_stable_diff_threshold"); !ok {
			add("screen_stable_diff_threshold", false, "not a number")
		} else {
			add("screen_stable_diff_threshold", value >= 0, fmt.Sprintf("must be >= 0, got %s", formatNumber(value)))
		}
		for _, key := range []string{"silence_ms", "min_speech_ms", "voice_followup_timeout_ms", "voice_first_turn_timeout_ms", "voice_max_turns", "voice_max_response_tokens", "screen_stable_timeout_ms", "screen_stable_ms"} {
			value, ok := numberValue(key)
			add(key, ok && value >= 0, fmt.Sprintf("must be >= 0, got %s", formatNumber(value)))
		}
		// The screenshot state limit is optional; when omitted the agent uses
		// its default, so only a value that is present is checked.
		if _, present := values["context_prune_state_limit"]; present {
			value, ok := numberValue("context_prune_state_limit")
			add("context_prune_state_limit", ok && value >= 0 && value == math.Trunc(value), fmt.Sprintf("must be a whole number >= 0, got %s", formatNumber(value)))
		}
		value, ok := numberValue("max_iterations")
		add("max_iterations", ok && value >= -1, fmt.Sprintf("must be >= -1, got %s", formatNumber(value)))
	case "search":
		provider, _ := stringValue("provider")
		allowed := map[string]bool{"duckduckgo": true, "brave": true, "brave-free": true, "tavily": true}
		providerOK := provider != "" && allowed[provider]
		providerDetail := "unknown provider '" + provider + "', known: duckduckgo/brave/brave-free/tavily"
		if provider == "" {
			providerDetail = "provider is empty"
		}
		add("provider", providerOK, providerDetail)
		var apiKey string
		if raw, exists := values["api_key"]; exists {
			_ = json.Unmarshal(raw, &apiKey)
		}
		hasKey := false
		if raw, exists := values["has_api_key"]; exists {
			_ = json.Unmarshal(raw, &hasKey)
		}
		if provider == "brave" || provider == "brave-free" || provider == "tavily" {
			add("api_key", strings.TrimSpace(apiKey) != "" || hasKey, "required for "+provider)
		} else {
			add("api_key", true, "not required for "+provider)
		}
	case "telemetry":
		enabled, _ := boolValue("enabled")
		provider, _ := stringValue("provider")
		providerDetail := provider
		if provider == "" {
			providerDetail = "empty; defaults to langfuse"
		} else if !strings.EqualFold(provider, "langfuse") {
			providerDetail = "got '" + provider + "', allowed: langfuse"
		}
		add("provider", provider == "" || strings.EqualFold(provider, "langfuse"), providerDetail)
		baseURL, _ := stringValue("base_url")
		baseDetail := baseURL
		if enabled && baseURL == "" {
			baseDetail = "required when telemetry.enabled is true"
		} else if baseURL == "" {
			baseDetail = "empty; telemetry disabled or not configured"
		}
		add("base_url", !enabled || baseURL != "", baseDetail)
		if baseURL != "" {
			validatedURL, validationErr := validateHTTPURL(baseURL)
			if validationErr != nil {
				add("endpoint_reachable", false, validationErr.Error())
			} else if env, envErr := s.agentCommandEnvironment(); envErr != nil {
				add("endpoint_reachable", false, envErr.Error())
			} else {
				result := runCommand(10*time.Second, env, nil, "curl", "-sI", "--max-time", "6", "--", validatedURL)
				reachable := result.ExitCode == 0 && strings.Contains(string(result.Output), "HTTP")
				add("endpoint_reachable", reachable, validatedURL+" -> "+strings.TrimRight(string(result.Output), "\r\n"))
			}
		}
		for _, key := range []string{"public_key", "secret_key"} {
			present := false
			if raw, exists := values[key]; exists {
				var value string
				if json.Unmarshal(raw, &value) == nil {
					present = strings.TrimSpace(value) != ""
				}
			}
			if raw, exists := values["has_"+key]; exists {
				var has bool
				if json.Unmarshal(raw, &has) == nil {
					present = present || has
				}
			}
			detail := "empty; telemetry disabled or not configured"
			if enabled && !present {
				detail = "required when telemetry.enabled is true"
			} else if present {
				detail = "set"
			}
			add(key, !enabled || present, detail)
		}
		for _, key := range []string{"upload_timeout_sec", "max_retry"} {
			value, ok := numberValue(key)
			add(key, ok && value >= 0, fmt.Sprintf("must be >= 0, got %s", formatNumber(value)))
		}
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported section: "+section)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": passed, "results": results})
}

func (s *Server) handleProviderConfigTest(w http.ResponseWriter, _ *http.Request, section string, request configTestRequest) {
	body, err := json.Marshal(request)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	env, err := s.agentCommandEnvironment()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	result := runCommand(60*time.Second, env, body, s.options.AgentBinary, "config-test", "--format=json", "--stdin", "--section="+section, "--config="+s.options.AgentConfigPath)
	if result.TimedOut {
		writeJSONError(w, http.StatusServiceUnavailable, "agent config-test timed out")
		return
	}
	var response map[string]any
	if err := json.Unmarshal(result.Output, &response); err != nil || response == nil {
		message := strings.TrimSpace(string(result.Output))
		if message == "" {
			message = "agent config-test returned an unexpected response"
		}
		writeJSONError(w, http.StatusServiceUnavailable, message)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func validateHTTPURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", fmt.Errorf("base_url must be an http:// or https:// URL with a host")
	}
	return parsed.String(), nil
}

func (s *Server) agentCommandEnvironment() ([]string, error) {
	data, err := readFileLimited(s.options.SystemEnvPath, maxSystemEnvSize)
	if err != nil {
		if os.IsNotExist(err) {
			return os.Environ(), nil
		}
		return nil, fmt.Errorf("system env file is unavailable")
	}
	assignments, err := parseSystemEnv(string(data))
	if err != nil {
		return nil, fmt.Errorf("system env file is invalid")
	}
	env := mergeEnvironment(os.Environ(), assignments)
	values := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	proxyConfigured := values["HTTP_PROXY"] != "" || values["http_proxy"] != "" ||
		values["HTTPS_PROXY"] != "" || values["https_proxy"] != "" ||
		values["ALL_PROXY"] != "" || values["all_proxy"] != ""
	generatedWiFiNoProxy := false
	if values["AICO_WIFI_PROXY_ENABLED"] != "0" && strings.TrimSpace(s.options.LocalProxyAddress) != "" {
		localProxy := "http://" + s.options.LocalProxyAddress
		// The generated file contains one value per protocol. Preserve those
		// values independently so SOCKS5 Wi-Fi policies remain SOCKS5 at the
		// Agent boundary instead of being downgraded to HTTP CONNECT.
		generatedValues := map[string]string{}
		if generated, readErr := os.ReadFile(s.options.LocalProxyEnvironmentPath); readErr == nil {
			if assignments, parseErr := parseSystemEnv(string(generated)); parseErr == nil {
				for _, assignment := range assignments {
					if assignment.Key == "HTTP_PROXY" || assignment.Key == "HTTPS_PROXY" || assignment.Key == "ALL_PROXY" || assignment.Key == "NO_PROXY" {
						generatedValues[assignment.Key] = assignment.Value
					}
					if assignment.Key == "NO_PROXY" {
						generatedWiFiNoProxy = true
					}
				}
			}
		}
		if generatedValues["HTTP_PROXY"] == "" {
			generatedValues["HTTP_PROXY"] = localProxy
		}
		if generatedValues["HTTPS_PROXY"] == "" {
			generatedValues["HTTPS_PROXY"] = localProxy
		}
		if generatedValues["ALL_PROXY"] == "" {
			generatedValues["ALL_PROXY"] = localProxy
		}
		env = mergeEnvironment(env, []EnvAssignment{
			{Key: "HTTP_PROXY", Value: generatedValues["HTTP_PROXY"]}, {Key: "http_proxy", Value: generatedValues["HTTP_PROXY"]},
			{Key: "HTTPS_PROXY", Value: generatedValues["HTTPS_PROXY"]}, {Key: "https_proxy", Value: generatedValues["HTTPS_PROXY"]},
			{Key: "ALL_PROXY", Value: generatedValues["ALL_PROXY"]}, {Key: "all_proxy", Value: generatedValues["ALL_PROXY"]},
		})
		if generatedWiFiNoProxy {
			env = mergeEnvironment(env, []EnvAssignment{
				{Key: "NO_PROXY", Value: generatedValues["NO_PROXY"]}, {Key: "no_proxy", Value: generatedValues["NO_PROXY"]},
			})
		}
		proxyConfigured = true
	}
	if proxyConfigured && !generatedWiFiNoProxy && values["NO_PROXY"] == "" && values["no_proxy"] == "" {
		env = mergeEnvironment(env, []EnvAssignment{
			{Key: "NO_PROXY", Value: wifiproxy.DefaultNoProxy}, {Key: "no_proxy", Value: wifiproxy.DefaultNoProxy},
		})
	}
	return env, nil
}

func normalizeDeviceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "ios":
		return "iOS"
	case "android":
		return "Android"
	case "macos":
		return "macOS"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return strings.TrimSpace(value)
	}
}

func pointerModeForDeviceType(value string) string {
	if value == "windows" || value == "linux" {
		return "relative"
	}
	return "absolute"
}

func normalizeInputBackend(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "hid"
	}
	return value
}

func formatNumber(value float64) string {
	if value == float64(int(value)) {
		return strconv.Itoa(int(value))
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

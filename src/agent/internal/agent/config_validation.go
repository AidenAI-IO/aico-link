package agent

import "strings"

// ConfigValidationError identifies the configuration field responsible for a
// semantic validation failure. Config.Validate currently returns one error at
// a time, so callers receive either an empty slice or a single field error.
type ConfigValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ParseConfigValidationErrors converts Config.Validate errors into the stable
// field paths consumed by config-editing clients.
func ParseConfigValidationErrors(err error) []ConfigValidationError {
	if err == nil {
		return []ConfigValidationError{}
	}

	errMsg := err.Error()
	field := ""
	switch {
	case strings.Contains(errMsg, "model: provider is required"):
		field = "model.provider"
	case strings.Contains(errMsg, "model: model is required"):
		field = "model.model"
	// Locale and timezone are matched at the position their validator puts them
	// in, not as bare words: several validators echo the offending value, so
	// `invalid search.provider: timezone (...)` must stay a search.provider error
	// rather than being claimed by the earlier timezone case.
	case strings.Contains(errMsg, "invalid locale:"):
		field = "locale"
	case strings.Contains(errMsg, "unsupported timezone:"), strings.Contains(errMsg, "load timezone "):
		field = "timezone"
	case strings.Contains(errMsg, "search.provider"), strings.Contains(errMsg, "search provider"):
		field = "search.provider"
	case strings.Contains(errMsg, "search.api_key"), strings.Contains(errMsg, "search api_key"):
		field = "search.api_key"
	case strings.Contains(errMsg, "decision."):
		field = firstConfigField(errMsg, "decision.")
	case strings.Contains(errMsg, "model.provider"):
		field = "model.provider"
	case strings.Contains(errMsg, "model.api_mode"):
		field = "model.api_mode"
	case strings.Contains(errMsg, "model.reasoning_budget_tokens"):
		field = "model.reasoning_budget_tokens"
	case strings.Contains(errMsg, "model.max_response_tokens"):
		field = "model.max_response_tokens"
	case strings.Contains(errMsg, "model.context_window"):
		field = "model.context_window"
	case strings.Contains(errMsg, "model.model_max_output_tokens"):
		field = "model.model_max_output_tokens"
	case strings.Contains(errMsg, "model.model"):
		field = "model.model"
	case strings.Contains(errMsg, "model.responses_"):
		// Each Responses API knob reports its own path first, so the shared prefix
		// is enough to name whichever one the page renders.
		field = firstConfigField(errMsg, "model.responses_")
	case strings.Contains(errMsg, "device.device_type"):
		field = "device.device_type"
	case strings.Contains(errMsg, "stt.provider"):
		field = "stt.provider"
	case strings.Contains(errMsg, "tts.provider"):
		field = "tts.provider"
	case strings.Contains(errMsg, "voice_model."):
		field = firstConfigField(errMsg, "voice_model.")
	case strings.Contains(errMsg, "termination_policy."):
		field = firstConfigField(errMsg, "termination_policy.")
	case strings.Contains(errMsg, "storage."):
		field = firstConfigField(errMsg, "storage.")
	case strings.Contains(errMsg, "quick_capture."):
		field = firstConfigField(errMsg, "quick_capture.")
	case strings.Contains(errMsg, "ota."):
		field = firstConfigField(errMsg, "ota.")
	case strings.Contains(errMsg, "voice_notifications."):
		field = firstConfigField(errMsg, "voice_notifications.")
	case strings.Contains(errMsg, "input_mode"):
		field = "input_mode"
	case strings.Contains(errMsg, "hid.keyboard_layout"), strings.Contains(errMsg, "keyboard_layout"):
		field = "hid.keyboard_layout"
	case strings.Contains(errMsg, "hid.pointer_mode"), strings.Contains(errMsg, "pointer_mode"):
		field = "hid.pointer_mode"
	case strings.Contains(errMsg, "max_iterations"):
		field = "max_iterations"
	case strings.Contains(errMsg, "context_prune_state_limit"):
		field = "context_prune_state_limit"
	case strings.Contains(errMsg, "vad_speech_threshold"):
		field = "vad_speech_threshold"
	case strings.Contains(errMsg, "voice_followup_timeout_ms"):
		field = "voice_followup_timeout_ms"
	case strings.Contains(errMsg, "voice_first_turn_timeout_ms"):
		field = "voice_first_turn_timeout_ms"
	case strings.Contains(errMsg, "voice_max_turns"):
		field = "voice_max_turns"
	case strings.Contains(errMsg, "screen_stable_timeout_ms"):
		field = "screen_stable_timeout_ms"
	case strings.Contains(errMsg, "screen_stable_ms"):
		field = "screen_stable_ms"
	case strings.Contains(errMsg, "screen_stable_diff_threshold"):
		field = "screen_stable_diff_threshold"
	case strings.Contains(errMsg, "audio.sample_rate"):
		field = "audio.sample_rate"
	case strings.Contains(errMsg, "audio.channels"):
		field = "audio.channels"
	case strings.Contains(errMsg, "audio.bit_width"):
		field = "audio.bit_width"
	case strings.Contains(errMsg, "audio.backend"):
		field = "audio.backend"
	case strings.Contains(errMsg, "hid.input_backend"):
		field = "hid.input_backend"
	case strings.Contains(errMsg, "log.level"):
		field = "log.level"
	case strings.Contains(errMsg, "telemetry.base_url"):
		field = "telemetry.base_url"
	case strings.Contains(errMsg, "telemetry.public_key"):
		field = "telemetry.public_key"
	case strings.Contains(errMsg, "telemetry.secret_key"):
		field = "telemetry.secret_key"
	case strings.Contains(errMsg, "telemetry.provider"):
		field = "telemetry.provider"
	case strings.Contains(errMsg, "telemetry.upload_timeout_sec"):
		field = "telemetry.upload_timeout_sec"
	case strings.Contains(errMsg, "telemetry.max_retry"):
		field = "telemetry.max_retry"
	case strings.Contains(errMsg, "log.llm_http_retention_days"):
		field = "log.llm_http_retention_days"
	}

	return []ConfigValidationError{{Field: field, Message: errMsg}}
}

// firstConfigField returns the first dotted field path beginning at prefix.
// Validation messages may wrap a field in prose (for example, "invalid
// voice_model.provider: ..."), so keeping the extraction local avoids making
// callers depend on the exact wording of every validator.
func firstConfigField(message, prefix string) string {
	start := strings.Index(message, prefix)
	if start < 0 {
		return ""
	}
	end := start + len(prefix)
	for end < len(message) {
		ch := message[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '.' {
			end++
			continue
		}
		break
	}
	return message[start:end]
}

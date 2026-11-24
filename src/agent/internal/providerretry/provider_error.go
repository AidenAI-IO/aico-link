package providerretry

import (
	"encoding/json"
	"strconv"
	"strings"
)

// ProviderStatus maps structured stream error codes, never free-form messages.
// Unknown codes are permanent failures until a provider-specific mapping exists.
func ProviderStatus(codes ...string) int {
	for _, code := range codes {
		code = strings.ToLower(strings.TrimSpace(code))
		if number, err := strconv.Atoi(code); err == nil && number >= 400 && number < 600 {
			return number
		}
		switch code {
		case "rate_limit_error", "rate_limit_exceeded", "rate_limit", "resource_exhausted", "too_many_requests":
			return 429
		case "overloaded_error", "overloaded":
			return 529
		case "unavailable", "service_unavailable", "server_overloaded":
			return 503
		case "internal", "internal_error", "server_error", "api_error":
			return 500
		case "deadline_exceeded", "timeout", "request_timeout":
			return 504
		case "invalid_argument", "invalid_request_error", "context_length_exceeded", "context_window_exceeded":
			return 400
		case "unauthenticated", "authentication_error", "invalid_api_key":
			return 401
		case "permission_denied", "permission_error":
			return 403
		}
	}
	return 400
}

func JSONStatus(body []byte) int {
	var payload struct {
		Error struct {
			Code   json.RawMessage `json:"code"`
			Type   string          `json:"type"`
			Status string          `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return 400
	}
	var code string
	if json.Unmarshal(payload.Error.Code, &code) != nil {
		code = string(payload.Error.Code)
	}
	return ProviderStatus(code, payload.Error.Type, payload.Error.Status)
}

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aico-agent/internal/providerretry"
)

// ProviderHTTPError preserves structured details from a provider HTTP response
// while keeping the historic error text used by logs and API responses.
type ProviderHTTPError struct {
	StatusCode   int
	ProviderCode string
	Message      string
	Body         string
}

func newProviderStreamError(body []byte) *ProviderHTTPError {
	return newProviderHTTPError(providerretry.JSONStatus(body), body)
}

func newProviderHTTPError(statusCode int, body []byte) *ProviderHTTPError {
	err := &ProviderHTTPError{
		StatusCode: statusCode,
		Body:       string(body),
	}

	var payload struct {
		ErrorType string `json:"error_type"`
		Error     struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil {
		err.ProviderCode = strings.TrimSpace(payload.ErrorType)
		if err.ProviderCode == "" {
			err.ProviderCode = normalizeProviderErrorCode(payload.Error.Code)
		}
		if err.ProviderCode == "" {
			err.ProviderCode = strings.TrimSpace(payload.Error.Type)
		}
		err.Message = strings.TrimSpace(payload.Error.Message)
	}
	return err
}

func normalizeProviderErrorCode(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return ""
	}
	var decoded string
	if json.Unmarshal(raw, &decoded) == nil {
		return strings.TrimSpace(decoded)
	}
	return value
}

func (e *ProviderHTTPError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Body)
}

func (e *ProviderHTTPError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

func (e *ProviderHTTPError) ProviderErrorCode() string {
	if e == nil {
		return ""
	}
	return e.ProviderCode
}

// isProviderContextExceededError reports whether a model request was rejected
// because its input no longer fits in the provider's context window. Providers
// use different status codes and payload shapes for this condition, so prefer a
// structured provider code and fall back to the stable phrases used in their
// messages.
func isProviderContextExceededError(err error) bool {
	if err == nil {
		return false
	}

	var codeErr interface{ ProviderErrorCode() string }
	if errors.As(err, &codeErr) && isContextExceededProviderCode(codeErr.ProviderErrorCode()) {
		return true
	}

	message := err.Error()
	var providerErr *ProviderHTTPError
	if errors.As(err, &providerErr) {
		message = strings.Join([]string{providerErr.Message, providerErr.Body, message}, " ")
	}
	return containsContextExceededMessage(message)
}

func isContextExceededProviderCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "context_length_exceeded",
		"context_window_exceeded",
		"max_context_length_exceeded",
		"maximum_context_length_exceeded",
		"input_too_long",
		"prompt_too_long":
		return true
	default:
		return false
	}
}

func containsContextExceededMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	for _, marker := range []string{
		"context length exceeded",
		"context_length_exceeded",
		"context window exceeded",
		"context_window_exceeded",
		"maximum context length",
		"maximum context window",
		"max context length",
		"exceeds the context window",
		"exceeded the context window",
		"larger than the model's context length",
		"larger than the model context length",
		"prompt is too long",
		"prompt too long",
		"input is too long",
		"input too long",
		"reduce the length of the messages",
		"reduce the length of the prompt",
		"context exceeded",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}

	return (strings.Contains(message, "input token count") || strings.Contains(message, "total number of tokens")) &&
		strings.Contains(message, "exceed") &&
		(strings.Contains(message, "maximum") || strings.Contains(message, "limit"))
}

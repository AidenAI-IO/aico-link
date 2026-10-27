package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

func decodeStrictJSONObject(input string, value any) error {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "{") {
		return fmt.Errorf("expected exactly one JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON object")
	}
	return nil
}

// LLMs frequently stringify structured tool arguments, emitting `"tags": "[]"`
// or `"limit": "3"` instead of a real JSON array or number. Strict decoding
// rejects those payloads and the whole tool call fails. The flex types below
// tolerate those shapes so a malformed-but-recoverable argument still works.

// flexStringSlice decodes a JSON array of strings, but also tolerates a single
// bare string ("foo" -> ["foo"]) and a string that itself contains a JSON
// array ("[]" or "[\"a\",\"b\"]" -> the decoded array).
type flexStringSlice []string

func (f *flexStringSlice) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = nil
		return nil
	}

	switch trimmed[0] {
	case '[':
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = nil
			return nil
		}
		// A string that wraps a JSON array, e.g. "[\"a\",\"b\"]".
		if s[0] == '[' {
			var arr []string
			if err := json.Unmarshal([]byte(s), &arr); err != nil {
				return fmt.Errorf("decode string-wrapped array %q: %w", s, err)
			}
			*f = arr
			return nil
		}
		// A bare value, e.g. "foo". LLMs also emit multi-value fields as one
		// delimited string ("procedure, fact") instead of an array; keeping that
		// as a single element makes exact-match filters such as matchesAny fail
		// against every candidate, silently dropping all results.
		*f = splitDelimitedArgValues(s)
		return nil
	default:
		return fmt.Errorf("cannot decode %s into string slice", trimmed)
	}
}

// flexLiteralStringSlice accepts the same recoverable array shapes as
// flexStringSlice, but preserves a bare scalar exactly. Save operations use it
// so persisted values such as "ACME, Inc." are not silently split.
type flexLiteralStringSlice []string

func (f *flexLiteralStringSlice) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = nil
		return nil
	}
	if trimmed[0] == '[' {
		var arr []string
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return err
		}
		*f = arr
		return nil
	}
	if trimmed[0] != '"' {
		return fmt.Errorf("cannot decode %s into string slice", trimmed)
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*f = nil
		return nil
	}
	if s[0] == '[' {
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return fmt.Errorf("decode string-wrapped array %q: %w", s, err)
		}
		*f = arr
		return nil
	}
	*f = []string{s}
	return nil
}

// splitDelimitedArgValues splits a bare string argument on ASCII and fullwidth
// commas, so "procedure, fact" and "procedure，fact" both yield two values. A
// string with no delimiter returns a single-element slice unchanged.
func splitDelimitedArgValues(s string) []string {
	if !strings.ContainsAny(s, ",，") {
		return []string{s}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '，'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// flexInt decodes a JSON number, but also tolerates a numeric string ("3"),
// a float ("3.0" or 3.0), and an empty/null value (-> 0).
type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*f = 0
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		n, err := parseFlexInt(s)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}

	// Plain JSON number; decode as float so "3.0" style values are accepted.
	var n float64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return fmt.Errorf("cannot decode %s into int", trimmed)
	}
	if n != math.Trunc(n) {
		return fmt.Errorf("cannot decode %s into int: value is not an integer", trimmed)
	}
	*f = flexInt(int(n))
	return nil
}

func parseFlexInt(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	if ff, err := strconv.ParseFloat(s, 64); err == nil {
		if ff != math.Trunc(ff) {
			return 0, fmt.Errorf("cannot decode %q into int: value is not an integer", s)
		}
		return int(ff), nil
	}
	return 0, fmt.Errorf("cannot decode %q into int", s)
}

// decodeChunkRecallQuery tolerantly decodes a recall_session_chunks argument.
func decodeChunkRecallQuery(input string) (ChunkRecallQuery, error) {
	var flex struct {
		ChunkIDs flexStringSlice `json:"chunk_ids"`
		Tags     flexStringSlice `json:"tags"`
		Entities flexStringSlice `json:"entities"`
		Limit    flexInt         `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &flex); err != nil {
		return ChunkRecallQuery{}, err
	}
	return ChunkRecallQuery{
		ChunkIDs: flex.ChunkIDs,
		Tags:     flex.Tags,
		Entities: flex.Entities,
		Limit:    int(flex.Limit),
	}, nil
}

// decodeMemoryQuery tolerantly decodes a recall_memory argument.
func decodeMemoryQuery(input string) (MemoryQuery, error) {
	var flex struct {
		Tags     flexStringSlice `json:"tags"`
		Entities flexStringSlice `json:"entities"`
		Types    flexStringSlice `json:"types"`
		Limit    flexInt         `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &flex); err != nil {
		return MemoryQuery{}, err
	}
	return MemoryQuery{
		Tags:     flex.Tags,
		Entities: flex.Entities,
		Types:    flex.Types,
		Limit:    int(flex.Limit),
	}, nil
}

// decodeDeviceMemoryQuery tolerantly decodes a recall_device_memory argument.
func decodeDeviceMemoryQuery(input string) (DeviceMemoryQuery, error) {
	var flex struct {
		Terms    flexStringSlice `json:"terms"`
		Tags     flexStringSlice `json:"tags"`
		Entities flexStringSlice `json:"entities"`
		DeviceID string          `json:"device_id"`
		Types    flexStringSlice `json:"types"`
		Limit    flexInt         `json:"limit"`
	}
	if err := json.Unmarshal([]byte(input), &flex); err != nil {
		return DeviceMemoryQuery{}, err
	}
	return DeviceMemoryQuery{
		Terms:    flex.Terms,
		Tags:     flex.Tags,
		Entities: flex.Entities,
		DeviceID: flex.DeviceID,
		Types:    flex.Types,
		Limit:    int(flex.Limit),
	}, nil
}

// SaveMemoryRequest represents the input for save_memory tool.
type SaveMemoryRequest struct {
	Type     string
	Title    string
	Content  string
	Tags     []string
	Entities []string
	Evidence []string
	Priority int
	// Sensitive names the kind of secret the memory holds, if any.
	Sensitive string
	// UserRequest quotes the user's words asking to remember this. It is
	// required only for a secret.
	UserRequest string
}

// decodeSaveMemoryRequest tolerantly decodes a save_memory argument.
func decodeSaveMemoryRequest(input string) (SaveMemoryRequest, error) {
	var flex struct {
		Type        string                 `json:"type"`
		Title       string                 `json:"title"`
		Content     string                 `json:"content"`
		Tags        flexLiteralStringSlice `json:"tags"`
		Entities    flexLiteralStringSlice `json:"entities"`
		Evidence    flexLiteralStringSlice `json:"evidence"`
		Priority    flexInt                `json:"priority"`
		Sensitive   string                 `json:"sensitive"`
		UserRequest string                 `json:"user_request"`
	}
	if err := json.Unmarshal([]byte(input), &flex); err != nil {
		return SaveMemoryRequest{}, err
	}
	return SaveMemoryRequest{
		Type:        flex.Type,
		Title:       flex.Title,
		Content:     flex.Content,
		Tags:        flex.Tags,
		Entities:    flex.Entities,
		Evidence:    flex.Evidence,
		Priority:    int(flex.Priority),
		Sensitive:   flex.Sensitive,
		UserRequest: flex.UserRequest,
	}, nil
}

// ForgetMemoryRequest represents the input for forget_memory tool.
type ForgetMemoryRequest struct {
	ID     string
	Reason string
}

// decodeForgetMemoryRequest tolerantly decodes a forget_memory argument.
func decodeForgetMemoryRequest(input string) (ForgetMemoryRequest, error) {
	var req ForgetMemoryRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return ForgetMemoryRequest{}, err
	}
	return req, nil
}

// InspectEpisodeRequest represents the input for inspect_episode tool.
type InspectEpisodeRequest struct {
	ID string `json:"id"`
}

// decodeInspectEpisodeRequest tolerantly decodes an inspect_episode argument,
// tolerating a bare string id ("ep_123") in addition to the JSON object form.
func decodeInspectEpisodeRequest(input string) (InspectEpisodeRequest, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "" && trimmed[0] == '"' {
		var id string
		if err := json.Unmarshal([]byte(trimmed), &id); err != nil {
			return InspectEpisodeRequest{}, err
		}
		return InspectEpisodeRequest{ID: strings.TrimSpace(id)}, nil
	}
	var req InspectEpisodeRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return InspectEpisodeRequest{}, err
	}
	req.ID = strings.TrimSpace(req.ID)
	return req, nil
}

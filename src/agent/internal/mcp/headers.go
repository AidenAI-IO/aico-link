package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	headerProtocolVersion = "MCP-Protocol-Version"
	headerMethod          = "Mcp-Method"
	headerName            = "Mcp-Name"

	errHeaderMismatch = -32020
)

type requestMeta struct {
	ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
}

type paramsWithMeta struct {
	Name string          `json:"name"`
	URI  string          `json:"uri"`
	Meta json.RawMessage `json:"_meta"`
}

type headerValidationError struct {
	Message string
	Data    any
	Code    int
}

func (e *headerValidationError) Error() string { return e.Message }

// isModernRequest is true only for 2026-07-28 per-request-metadata traffic.
// A bare MCP-Protocol-Version: 2026-07-28 header (no params._meta / Mcp-Method)
// stays on the legacy path so conforming clients can call tools/list and ping
// without inventing body metadata. Strict validation runs when the body carries
// modern _meta, or when the client opts in with Mcp-Method.
func isModernRequest(r *http.Request, req jsonRPCRequest) bool {
	bodyVersion, _, _ := extractBodyMeta(req)
	if bodyVersion == protocolModern {
		return true
	}
	headerVersion := strings.TrimSpace(r.Header.Get(headerProtocolVersion))
	if headerVersion == protocolModern && strings.TrimSpace(r.Header.Get(headerMethod)) != "" {
		return true
	}
	return false
}

func extractBodyMeta(req jsonRPCRequest) (protocolVersion, name, uri string) {
	params, err := decodeParams[paramsWithMeta](req.Params)
	if err != nil {
		return "", "", ""
	}
	name = strings.TrimSpace(params.Name)
	uri = strings.TrimSpace(params.URI)
	if len(params.Meta) == 0 || string(params.Meta) == "null" {
		return "", name, uri
	}
	var meta requestMeta
	if err := json.Unmarshal(params.Meta, &meta); err != nil {
		return "", name, uri
	}
	return strings.TrimSpace(meta.ProtocolVersion), name, uri
}

// validateModernHeaders enforces Streamable HTTP 2026-07-28 request metadata.
func validateModernHeaders(r *http.Request, req jsonRPCRequest) *headerValidationError {
	headerVersion := strings.TrimSpace(r.Header.Get(headerProtocolVersion))
	bodyVersion, bodyName, bodyURI := extractBodyMeta(req)

	if headerVersion == "" {
		return &headerValidationError{
			Code:    errHeaderMismatch,
			Message: "Header mismatch: missing MCP-Protocol-Version",
		}
	}
	// Spec 2026-07-28 marks per-request params._meta protocolVersion and
	// clientCapabilities as required on modern requests. This endpoint also
	// accepts header-only modern traffic (no body _meta) for simpler device
	// clients; when body _meta is present it must still match the header.
	if bodyVersion != "" && headerVersion != bodyVersion {
		return &headerValidationError{
			Code:    errHeaderMismatch,
			Message: fmt.Sprintf("Header mismatch: MCP-Protocol-Version %q does not match body %q", headerVersion, bodyVersion),
		}
	}
	if !protocolSupported(headerVersion) {
		return &headerValidationError{
			Code:    errUnsupportedVer,
			Message: "Unsupported protocol version",
			Data: map[string]any{
				"supported": supportedProtocolVersions,
				"requested": headerVersion,
			},
		}
	}

	headerMethodVal := strings.TrimSpace(r.Header.Get(headerMethod))
	if headerMethodVal == "" {
		return &headerValidationError{
			Code:    errHeaderMismatch,
			Message: "Header mismatch: missing Mcp-Method",
		}
	}
	if headerMethodVal != req.Method {
		return &headerValidationError{
			Code:    errHeaderMismatch,
			Message: fmt.Sprintf("Header mismatch: Mcp-Method %q does not match body method %q", headerMethodVal, req.Method),
		}
	}

	switch req.Method {
	case "tools/call", "prompts/get":
		decodedName, err := decodeMCPHeaderValue(r.Header.Get(headerName))
		if err != nil {
			return &headerValidationError{Code: errHeaderMismatch, Message: "Header mismatch: invalid Mcp-Name encoding"}
		}
		if decodedName == "" {
			return &headerValidationError{Code: errHeaderMismatch, Message: "Header mismatch: missing Mcp-Name"}
		}
		if decodedName != bodyName {
			return &headerValidationError{
				Code:    errHeaderMismatch,
				Message: fmt.Sprintf("Header mismatch: Mcp-Name %q does not match body value %q", decodedName, bodyName),
			}
		}
	case "resources/read":
		decodedName, err := decodeMCPHeaderValue(r.Header.Get(headerName))
		if err != nil {
			return &headerValidationError{Code: errHeaderMismatch, Message: "Header mismatch: invalid Mcp-Name encoding"}
		}
		if decodedName == "" {
			return &headerValidationError{Code: errHeaderMismatch, Message: "Header mismatch: missing Mcp-Name"}
		}
		if decodedName != bodyURI {
			return &headerValidationError{
				Code:    errHeaderMismatch,
				Message: fmt.Sprintf("Header mismatch: Mcp-Name %q does not match body uri %q", decodedName, bodyURI),
			}
		}
	}
	return nil
}

func decodeMCPHeaderValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	const prefix = "=?base64?"
	const suffix = "?="
	if strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix) {
		encoded := strings.TrimSuffix(strings.TrimPrefix(value, prefix), suffix)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", err
		}
		return string(decoded), nil
	}
	return value, nil
}

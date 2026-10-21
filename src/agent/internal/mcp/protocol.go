package mcp

import (
	"encoding/json"
	"fmt"
)

const (
	jsonRPCVersion = "2.0"

	protocolModern   = "2026-07-28"
	protocolLegacy25 = "2025-11-25"
	protocolLegacy03 = "2025-03-26"
	protocolLegacy24 = "2024-11-05"

	errParseError     = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
	errUnsupportedVer = -32022
)

var supportedProtocolVersions = []string{
	protocolModern,
	protocolLegacy25,
	protocolLegacy03,
	protocolLegacy24,
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func isNotification(req jsonRPCRequest) bool {
	return len(req.ID) == 0 || string(req.ID) == "null"
}

func errorResponse(id json.RawMessage, code int, message string, data any) jsonRPCResponse {
	return jsonRPCResponse{
		JSONRPC: jsonRPCVersion,
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message, Data: data},
	}
}

func resultResponse(id json.RawMessage, result any) jsonRPCResponse {
	return jsonRPCResponse{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

func decodeParams[T any](raw json.RawMessage) (T, error) {
	var out T
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("invalid params: %w", err)
	}
	return out, nil
}

func protocolSupported(version string) bool {
	for _, v := range supportedProtocolVersions {
		if v == version {
			return true
		}
	}
	return false
}

func preferLegacyProtocol(requested string) string {
	if protocolSupported(requested) {
		return requested
	}
	return protocolLegacy25
}

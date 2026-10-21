package mcp

import (
	"context"
	"encoding/json"
)

// ToolInfo is one MCP tools/list entry.
type ToolInfo struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CallResult is the MCP tools/call payload (legacy-compatible content list).
type CallResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ContentBlock is an MCP content item.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// Backend is implemented by the Agent Server.
type Backend interface {
	ShellEnabled() bool
	DescribeRuntimeTool(name string) (ToolInfo, bool)
	CallRuntimeTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error)
	PhoneBridgeStatus(ctx context.Context) (any, error)
	DeviceStatus(ctx context.Context) (any, error)
	SupportBundle(ctx context.Context) ([]byte, string, error)
}

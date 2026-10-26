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
	ResultType        string         `json:"resultType,omitempty"`
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ContentBlock is an MCP content item (text, image, or embedded resource).
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	MIMEType string            `json:"mimeType,omitempty"`
	Data     string            `json:"data,omitempty"`
	Resource *ResourceContents `json:"resource,omitempty"`
}

// ResourceContents is an embedded resource payload inside a content block.
type ResourceContents struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64
	Text     string `json:"text,omitempty"`
}

// Backend is implemented by the Agent Server.
type Backend interface {
	ShellEnabled() bool
	DescribeRuntimeTool(name string) (ToolInfo, bool)
	CallRuntimeTool(ctx context.Context, name string, arguments json.RawMessage) (CallResult, error)
	PhoneBridgeStatus(ctx context.Context) (any, error)
	DeviceStatus(ctx context.Context) (any, error)
	// SupportBundle returns a base64 blob under the backend concurrency cap
	// (fetch + encode + response write). bytes is the raw archive size before
	// encoding. On success, release must be called after the HTTP response is
	// fully written so peak memory stays gated for slow clients. On error,
	// release is nil (the slot is already freed).
	SupportBundle(ctx context.Context) (blob string, mime string, bytes int, release func(), err error)
}

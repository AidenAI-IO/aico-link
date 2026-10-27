package session

import (
	"aico-agent/internal/agent/messages"
	"errors"
	"time"
)

// AttachmentStore is the session-scoped store used for binary attachments.
// The concrete implementation lives with the context-manager storage layer;
// the session only owns the resource for its lifetime.
type AttachmentStore interface {
	StoreAttachment(mimeType string, data []byte) (messages.Attachment, error)
}

// ArtifactMetadata describes a persisted tool-result artifact.
type ArtifactMetadata struct {
	ToolName   string    `json:"tool_name,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	MIMEType   string    `json:"mime_type"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Sensitive  bool      `json:"sensitive,omitempty"`
	Complete   bool      `json:"complete"`
}

// ArtifactFile is the durable reference returned after an artifact is stored.
type ArtifactFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Complete bool   `json:"complete"`
}

// ArtifactStore is the session-scoped store used for large tool results.
type ArtifactStore interface {
	StoreArtifact(mimeType string, data []byte, metadata ArtifactMetadata) (ArtifactFile, error)
}

const (
	ArtifactSingleMaxBytes  = 8 * 1024 * 1024
	ArtifactSessionMaxBytes = 32 * 1024 * 1024
)

var (
	ErrArtifactTooLarge    = errors.New("artifact exceeds single-artifact size limit")
	ErrArtifactSessionFull = errors.New("session artifact size limit exceeded")
)

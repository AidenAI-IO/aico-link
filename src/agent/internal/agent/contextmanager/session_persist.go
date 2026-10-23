package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/session"
	"aico-agent/internal/logging"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionMetadata is the `<session_id>.meta.json` sidecar. It records session
// lineage so a compaction revision can be traced back to the session it was
// derived from, which the append-only transcript alone cannot express.
//
// Unknown fields are ignored on decode, so sidecars written by older builds
// (which carried a shared artifact_scope_id) still load; those revisions keep
// their artifacts through the concrete artifact_path persisted in messages.
type sessionMetadata struct {
	// ParentSessionID is the session this one was derived from, for example the
	// pre-compaction session of a revision. Empty for root sessions and for
	// sessions created by builds that predate this sidecar.
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// CreatedAt is when the session was created, in UTC.
	CreatedAt time.Time `json:"created_at,omitempty"`
	// UsageStartIndex excludes retained messages from an older context shape.
	// It is immutable for a session, so a stale token snapshot can be rebuilt.
	UsageStartIndex int                 `json:"usage_start_index"`
	Tokens          *session.TokenState `json:"tokens,omitempty"`
}

func tokenMetadataWriter(sessionFolder, sessionID string, metadata sessionMetadata) func(session.TokenState) {
	return func(state session.TokenState) {
		metadata.Tokens = &state
		if err := saveSessionMetadata(sessionFolder, sessionID, metadata); err != nil {
			// The transcript has already committed. Returning an append error would
			// invite duplicate messages on retry. Rebuild this cache on next load.
			logging.Warnf("agent", "cm", "Failed to save token metadata for %s: %v", sessionID, err)
		}
	}
}

// CurrentSessionID returns the session ID recorded as current for the given
// session folder, or an empty string when no session has been created yet.
// It is the single source of truth for "which conversation is live", used for
// log partitioning and storage bookkeeping outside this package.
func CurrentSessionID(sessionFolder string) string {
	return fetchCurrentSession(sessionFolder)
}

// fetchCurrentSession fetches the current session ID from the session folder, should be called when initializing the context manager.
func fetchCurrentSession(sessionFolder string) string {
	// sessionFolder/.current_session content is the session ID
	sessionID := ""
	sessionIDFile := filepath.Join(sessionFolder, ".current_session")
	if _, err := os.Stat(sessionIDFile); err == nil {
		content, err := os.ReadFile(sessionIDFile)
		if err == nil {
			sessionID = strings.TrimSpace(string(content))
		}
	}
	return sessionID
}

func saveCurrentSession(sessionFolder string, sessionID string) error {
	return writeSessionFileAtomically(sessionFolder, ".current_session", []byte(sessionID))
}

// writeSessionFileAtomically installs fileName inside sessionFolder through a
// temporary file and a rename, so readers never observe a partial file.
func writeSessionFileAtomically(sessionFolder, fileName string, data []byte) error {
	targetPath := filepath.Join(sessionFolder, fileName)
	file, err := os.CreateTemp(sessionFolder, fileName+"-*")
	if err != nil {
		return fmt.Errorf("create %s temp file: %w", fileName, err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)

	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod %s temp file: %w", fileName, err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s temp file: %w", fileName, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s temp file: %w", fileName, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s temp file: %w", fileName, err)
	}
	if err := os.Rename(tempPath, targetPath); err != nil {
		return fmt.Errorf("install %s file: %w", fileName, err)
	}
	return nil
}

func sessionMetadataFileName(sessionID string) string {
	return sessionID + ".meta.json"
}

func sessionMetadataPath(sessionFolder, sessionID string) string {
	return filepath.Join(sessionFolder, sessionMetadataFileName(sessionID))
}

// saveSessionMetadata writes the sidecar for sessionID. The session ID is
// validated first so a malformed ID cannot escape the session folder.
func saveSessionMetadata(sessionFolder, sessionID string, metadata sessionMetadata) error {
	if _, err := validateArtifactSessionID(sessionID); err != nil {
		return fmt.Errorf("save session metadata: %w", err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}
	return writeSessionFileAtomically(sessionFolder, sessionMetadataFileName(sessionID), data)
}

func loadSessionMetadata(sessionFolder, sessionID string) (sessionMetadata, bool, error) {
	data, err := os.ReadFile(sessionMetadataPath(sessionFolder, sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return sessionMetadata{}, false, nil
		}
		return sessionMetadata{}, false, fmt.Errorf("read session metadata: %w", err)
	}
	var metadata sessionMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return sessionMetadata{}, false, fmt.Errorf("decode session metadata: %w", err)
	}
	return metadata, true, nil
}

func appendSession(sessionFolder string, sessionID string, messages []messages.Message) error {
	// append to JSONL file, each message is a new line
	sessionFile := filepath.Join(sessionFolder, sessionID+".jsonl")
	file, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open session file %s: %w", sessionFile, err)
	}
	defer file.Close()

	var buf bytes.Buffer
	for _, message := range messages {
		encoded, err := json.Marshal(message)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
		buf.Write(encoded)
		buf.WriteByte('\n')
	}
	if _, err := file.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("failed to write messages to session file %s: %w", sessionFile, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync session file %s: %w", sessionFile, err)
	}
	return nil
}

func loadSession(sessionFolder string, sessionID string) ([]messages.Message, error) {
	sessionFile := filepath.Join(sessionFolder, sessionID+".jsonl")
	if _, err := os.Stat(sessionFile); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat session file %s: %w", sessionFile, err)
	}
	content, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read session file %s: %w", sessionFile, err)
	}
	lines := strings.Split(string(content), "\n")
	messageList := make([]messages.Message, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var message messages.Message
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			return nil, fmt.Errorf("failed to unmarshal message from session file %s: %w", sessionFile, err)
		}
		messageList = append(messageList, message)
	}
	return messageList, nil
}

// ClearAllSessions clears all sessions in the session folder, including the .current_session file.
func ClearAllSessions(sessionFolder string) error {
	sessionIDFile := filepath.Join(sessionFolder, ".current_session")
	if err := os.Remove(sessionIDFile); err != nil && !os.IsNotExist(err) {
		logging.Errorf("agent", "session_persist", "failed to remove current session file %s: %v", sessionIDFile, err)
	}
	entries, err := os.ReadDir(sessionFolder)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to list session files in %s: %w", sessionFolder, err)
	}
	for _, entry := range entries {
		path := filepath.Join(sessionFolder, entry.Name())
		if entry.IsDir() {
			if err := os.RemoveAll(path); err != nil {
				logging.Errorf("agent", "session_persist", "failed to remove session data directory %s: %v", entry.Name(), err)
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") && !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logging.Errorf("agent", "session_persist", "failed to remove session file %s: %v", path, err)
		}
	}
	return nil
}

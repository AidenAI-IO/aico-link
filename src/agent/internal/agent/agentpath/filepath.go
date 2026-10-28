package agentpath

import (
	"aico-agent/internal/logging"
	"os"
	"path/filepath"
	"strings"
)

func ContextManagerSessionFolder(configDir string) string {
	return contextManagerSessionFolder(configDir, "backend")
}

// UserContextManagerSessionFolder returns the persistent session directory
// used by the realtime voice conversation.
func UserContextManagerSessionFolder(configDir string) string {
	return contextManagerSessionFolder(configDir, "user")
}

func contextManagerSessionFolder(configDir, role string) string {
	trimmedConfigDir := strings.TrimSpace(configDir)
	if trimmedConfigDir == "" {
		logging.Fatalf("agent", "filepath", "configDir is required")
	}
	folderPath := filepath.Join(trimmedConfigDir, "sessions", role)
	if err := os.MkdirAll(folderPath, 0755); err != nil {
		logging.Fatalf("agent", "filepath", "failed to create sessions folder %s: %v", folderPath, err)
	}
	return folderPath
}

package contextmanager

import (
	"fmt"
	"path/filepath"
)

// HasSessionAncestor follows compaction revisions using metadata only. A new
// root created by clearing history is deliberately a different conversation.
func (c *ContextManager) HasSessionAncestor(id string) (bool, error) {
	if c == nil || id == "" {
		return false, nil
	}
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.currentSession == nil {
		return false, nil
	}
	current := c.currentSession.SessionID()
	seen := map[string]bool{}
	for current != "" {
		if current == id {
			return true, nil
		}
		if seen[current] || filepath.Base(current) != current || current == "." || current == ".." {
			return false, fmt.Errorf("invalid session lineage")
		}
		seen[current] = true
		metadata, found, err := loadSessionMetadata(c.sessionFolder, current)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		current = metadata.ParentSessionID
	}
	return false, nil
}

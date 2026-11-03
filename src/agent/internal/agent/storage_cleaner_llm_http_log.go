package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/logging"
)

// LLMHTTPLogCleaner cleans up old LLM HTTP log files
type LLMHTTPLogCleaner struct {
	logDir           string
	retentionAge     time.Duration
	priority         int
	currentSessionID func() (string, error)
}

// NewLLMHTTPLogCleaner creates a new LLM HTTP log cleaner
func NewLLMHTTPLogCleaner(logDir string, retentionDays int, priority int) *LLMHTTPLogCleaner {
	return NewLLMHTTPLogCleanerWithSessionProvider(logDir, retentionDays, priority, nil)
}

// NewLLMHTTPLogCleanerWithSessionProvider protects the active session log from cleanup.
func NewLLMHTTPLogCleanerWithSessionProvider(logDir string, retentionDays int, priority int, currentSessionID func() string) *LLMHTTPLogCleaner {
	var provider func() (string, error)
	if currentSessionID != nil {
		provider = func() (string, error) { return currentSessionID(), nil }
	}
	return NewLLMHTTPLogCleanerWithCheckedSessionProvider(logDir, retentionDays, priority, provider)
}

// NewLLMHTTPLogCleanerWithCheckedSessionProvider fails closed when the active session cannot be resolved.
func NewLLMHTTPLogCleanerWithCheckedSessionProvider(logDir string, retentionDays int, priority int, currentSessionID func() (string, error)) *LLMHTTPLogCleaner {
	return &LLMHTTPLogCleaner{
		logDir:           logDir,
		retentionAge:     time.Duration(retentionDays) * 24 * time.Hour,
		priority:         priority,
		currentSessionID: currentSessionID,
	}
}

func (c *LLMHTTPLogCleaner) Name() string {
	return fmt.Sprintf("llm_http_log_%dd", int(c.retentionAge.Hours()/24))
}

func (c *LLMHTTPLogCleaner) Priority() int {
	return c.priority
}

func (c *LLMHTTPLogCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	if c.logDir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(c.logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read log directory: %w", err)
	}

	cutoff := time.Now().Add(-c.retentionAge)
	protectedName, err := c.protectedName()
	if err != nil {
		return 0, err
	}
	var total uint64

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "llm-http-") {
			continue
		}
		if entry.Name() == protectedName {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		logTime := logFileTime(entry.Name(), info.ModTime())
		if c.retentionAge == 0 || logTime.Before(cutoff) {
			total += uint64(info.Size())
		}
	}

	return total, nil
}

func (c *LLMHTTPLogCleaner) Clean(ctx context.Context) (uint64, error) {
	return c.clean(ctx, c.retentionAge == 0)
}

func (c *LLMHTTPLogCleaner) clean(_ context.Context, force bool) (uint64, error) {
	if c.logDir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(c.logDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read log directory: %w", err)
	}

	cutoff := time.Now().Add(-c.retentionAge)
	protectedName, err := c.protectedName()
	if err != nil {
		return 0, err
	}
	var totalFreed uint64
	var deletedCount int

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		if !strings.HasPrefix(entry.Name(), "llm-http-") {
			continue
		}
		if entry.Name() == protectedName {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if !force {
			logTime := logFileTime(entry.Name(), info.ModTime())
			if !logTime.Before(cutoff) {
				continue
			}
		}

		path := filepath.Join(c.logDir, entry.Name())
		size := uint64(info.Size())

		if err := os.Remove(path); err != nil {
			return totalFreed, fmt.Errorf("remove %s: %w", entry.Name(), err)
		}

		totalFreed += size
		deletedCount++
	}

	if deletedCount > 0 {
		_ = logging.LogEvent(logging.Info, "agent", "storage_cleanup", "llm_http_logs_deleted",
			logging.Field{Key: "files", Value: deletedCount},
			logging.Field{Key: "retention_days", Value: int(c.retentionAge.Hours() / 24)},
			logging.Field{Key: "freed_bytes", Value: totalFreed},
		)
	}

	return totalFreed, nil
}

func (c *LLMHTTPLogCleaner) ForceClean(ctx context.Context) (uint64, error) {
	return c.clean(ctx, true)
}

func (c *LLMHTTPLogCleaner) protectedName() (string, error) {
	if c.currentSessionID == nil {
		return "", nil
	}
	sessionID, err := c.currentSessionID()
	if err != nil {
		return "", fmt.Errorf("resolve current session for log cleanup: %w", err)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", nil
	}
	return "llm-http-" + sessionID + ".log", nil
}

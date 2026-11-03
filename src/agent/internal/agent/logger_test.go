package agent

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/logging"
)

func TestLoggerFiltersBelowConfiguredLevel(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{
		logger:       log.New(&output, "", 0),
		minimumLevel: logging.Warn,
	}
	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	got := output.String()
	if strings.Contains(got, "debug message") || strings.Contains(got, "info message") {
		t.Fatalf("logger emitted messages below WARN: %s", got)
	}
	if !strings.Contains(got, "warn message") || !strings.Contains(got, "error message") {
		t.Fatalf("logger omitted WARN or ERROR messages: %s", got)
	}
}

func TestLoggerSetLevelAppliesWithoutRestart(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{
		logger:       log.New(&output, "", 0),
		minimumLevel: logging.Error,
	}
	logger.Warn("hidden warning")
	logger.SetLevel("debug")
	logger.Debug("visible debug")

	got := output.String()
	if strings.Contains(got, "hidden warning") || !strings.Contains(got, "visible debug") {
		t.Fatalf("runtime log level change not applied: %s", got)
	}
}

func TestCleanupOldLogFilesRemovesLogsOlderThanSevenDays(t *testing.T) {
	logDir := t.TempDir()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.Local)

	// Only llm-http logs are cleaned up
	writeTestLogFile(t, logDir, "llm-http-202606120900-session1.log", now)
	writeTestLogFile(t, logDir, "llm-http-20260612090030123.log", now)
	writeTestLogFile(t, logDir, "llm-http-202606140900-session2.log", now.Add(-8*24*time.Hour))
	writeTestLogFile(t, logDir, "llm-http-202606181445-session3.log", now.Add(-8*24*time.Hour))
	writeTestLogFile(t, logDir, "llm-http-20260618144530123.log", now.Add(-8*24*time.Hour))

	// These should not be touched
	writeTestLogFile(t, logDir, "agent-20260612.log", now)
	writeTestLogFile(t, logDir, "custom.log", now.Add(-8*24*time.Hour))
	writeTestLogFile(t, logDir, "llm-http-notadate.log", now)

	if err := cleanupOldLogFiles(logDir, now, 7); err != nil {
		t.Fatalf("cleanupOldLogFiles() error = %v", err)
	}

	// Old llm-http logs should be removed
	assertPathMissing(t, filepath.Join(logDir, "llm-http-202606120900-session1.log"))
	assertPathMissing(t, filepath.Join(logDir, "llm-http-20260612090030123.log"))

	// Recent llm-http logs should remain
	assertPathExists(t, filepath.Join(logDir, "llm-http-202606140900-session2.log"))
	assertPathExists(t, filepath.Join(logDir, "llm-http-202606181445-session3.log"))
	assertPathExists(t, filepath.Join(logDir, "llm-http-20260618144530123.log"))

	// Non-llm-http logs should not be touched
	assertPathExists(t, filepath.Join(logDir, "agent-20260612.log"))
	assertPathExists(t, filepath.Join(logDir, "custom.log"))
	assertPathExists(t, filepath.Join(logDir, "llm-http-notadate.log"))
}

func TestCleanupOldLogFilesUsesConfiguredRetentionDays(t *testing.T) {
	logDir := t.TempDir()
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.Local)

	writeTestLogFile(t, logDir, "llm-http-202606180900-session1.log", now)
	writeTestLogFile(t, logDir, "llm-http-202606190900-session2.log", now)

	if err := cleanupOldLogFiles(logDir, now, 2); err != nil {
		t.Fatalf("cleanupOldLogFiles() error = %v", err)
	}

	assertPathMissing(t, filepath.Join(logDir, "llm-http-202606180900-session1.log"))
	assertPathExists(t, filepath.Join(logDir, "llm-http-202606190900-session2.log"))
}

func TestCleanupOldLogFilesMissingDirIsNotError(t *testing.T) {
	// A missing log directory is normal on a fresh device that has not written
	// any llm-http logs yet. It means there is nothing to clean up, not an error.
	logDir := filepath.Join(t.TempDir(), "does-not-exist")
	now := time.Date(2026, 6, 21, 12, 0, 0, 0, time.Local)

	if err := cleanupOldLogFiles(logDir, now, 7); err != nil {
		t.Fatalf("cleanupOldLogFiles() with missing dir error = %v, want nil", err)
	}
}

func TestNewLoggerCleansOldLogFilesOnStartup(t *testing.T) {
	configDir := t.TempDir()
	logDir := filepath.Join(configDir, "log")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	// Create old llm-http log
	oldLog := filepath.Join(logDir, "llm-http-200001010000-session1.log")
	if err := os.WriteFile(oldLog, []byte("old"), 0644); err != nil {
		t.Fatalf("write old log: %v", err)
	}

	logger, err := NewLogger(configDir, 7)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	// Old llm-http log should be cleaned up
	assertPathMissing(t, oldLog)
}

func TestNewLoggerTreatsUnavailableLLMHTTPLogDirAsNonFatal(t *testing.T) {
	configDir := t.TempDir()
	logDir := filepath.Join(configDir, "log")
	if err := os.WriteFile(logDir, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write log path: %v", err)
	}

	logger, err := NewLogger(configDir, 7)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func writeTestLogFile(t *testing.T, dir, name string, modTime time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(name), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", name, err)
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to be missing, err=%v", path, err)
	}
}

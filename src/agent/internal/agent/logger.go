package agent

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

// Logger is the Agent's leveled logger. It writes message-style records in the
// shared logging format and publishes its severity threshold to the process-wide
// logging helpers, so LogEvent and the Debugf/Infof/Warnf/Errorf family honour
// the same configured level.
//
// Output goes to stderr, which the agent service redirects to
// <config_dir>/log/agent.log.
type Logger struct {
	logger         *log.Logger
	mu             sync.Mutex
	minimumLevel   logging.Level
	storageMonitor *StorageMonitor
}

type LogField = logging.Field

// NewLogger creates a new logger that writes to stdout/stderr
// The init script redirects output to <config_dir>/log/agent.log.
func NewLogger(configDir string, llmHTTPRetentionDays int, configuredLevel ...string) (*Logger, error) {
	logger := log.New(os.Stderr, "", 0)
	level := defaultLogLevel
	if len(configuredLevel) > 0 {
		level = configuredLevel[0]
	}
	result := &Logger{logger: logger, minimumLevel: configuredLoggingLevel(level)}
	result.applyMinimumLevel()

	// Cleanup old llm-http logs in configDir if set
	if configDir != "" {
		llmLogDir := filepath.Join(configDir, "log")
		if err := cleanupOldLogFiles(llmLogDir, time.Now(), llmHTTPRetentionDays); err != nil {
			result.Warn("[logger] log cleanup failed: %v", err)
		}
	}

	return result, nil
}

func cleanupOldLogFiles(logDir string, now time.Time, llmHTTPRetentionDays int) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		// A missing log directory is normal on a fresh device that has not
		// written any llm-http logs yet: there is nothing to clean up.
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read log directory: %w", err)
	}
	retention := time.Duration(LogConfig{
		LLMHTTPRetentionDays: llmHTTPRetentionDays,
	}.LLMHTTPRetentionDaysOrDefault()) * 24 * time.Hour
	cutoff := now.Add(-retention)
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		// Only clean up llm-http logs (not agent.log)
		if !strings.HasPrefix(entry.Name(), "llm-http-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, fmt.Errorf("stat log file %q: %w", entry.Name(), err))
			continue
		}
		logTime := logFileTime(entry.Name(), info.ModTime())
		if !logTime.Before(cutoff) {
			continue
		}
		path := filepath.Join(logDir, entry.Name())
		if err := os.Remove(path); err != nil {
			errs = append(errs, fmt.Errorf("remove old log file %q: %w", entry.Name(), err))
		}
	}
	return errors.Join(errs...)
}

func logFileTime(name string, modTime time.Time) time.Time {
	// Only parse llm-http logs
	if !strings.HasPrefix(name, "llm-http-") || !strings.HasSuffix(name, ".log") {
		return modTime
	}
	// Extract datetime from llm-http-YYYYMMDDHHMMSSmmm.log or legacy
	// llm-http-YYYYMMDDHHMM-*.log names.
	parts := strings.TrimPrefix(name, "llm-http-")
	parts = strings.TrimSuffix(parts, ".log")
	if len(parts) < 12 {
		return modTime
	}
	dateTimeStr := parts[:12]
	if parsed, err := time.ParseInLocation("200601021504", dateTimeStr, time.Local); err == nil {
		// Keep parsed log-file timestamps through the whole named day by adding
		// a 24-hour grace period before comparing them with the retention cutoff.
		return parsed.Add(24 * time.Hour)
	}
	return modTime
}

func (l *Logger) Close() error {
	logging.SetMinimumLevel(logging.Debug)
	return nil
}

func configuredLoggingLevel(level string) logging.Level {
	parsed, ok := logging.ParseLevel(level)
	if !ok {
		return logging.Info
	}
	return parsed
}

func loggingLevelRank(level logging.Level) int {
	switch logging.NormalizeLevel(level) {
	case logging.Debug:
		return 0
	case logging.Info:
		return 1
	case logging.Warn:
		return 2
	case logging.Error:
		return 3
	default:
		return 1
	}
}

func (l *Logger) allows(level logging.Level) bool {
	return loggingLevelRank(level) >= loggingLevelRank(l.minimumLevel)
}

// applyMinimumLevel publishes the instance minimum to the process-wide logging
// helpers (LogEvent and the Debugf/Infof/Warnf/Errorf family) so every logging
// path shares one severity threshold.
func (l *Logger) applyMinimumLevel() {
	minimum := logging.Info
	if l != nil {
		minimum = l.minimumLevel
	}
	logging.SetMinimumLevel(minimum)
}

// SetLevel applies a new minimum severity without restarting the Agent.
func (l *Logger) SetLevel(level string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.minimumLevel = configuredLoggingLevel(level)
	l.applyMinimumLevel()
	l.mu.Unlock()
}

func (l *Logger) Info(format string, args ...interface{}) {
	l.write("INFO", format, args...)
}

func (l *Logger) Error(format string, args ...interface{}) {
	l.write("ERROR", format, args...)
}

func (l *Logger) Debug(format string, args ...interface{}) {
	l.write("DEBUG", format, args...)
}

func (l *Logger) Warn(format string, args ...interface{}) {
	l.write("WARN", format, args...)
}

func (l *Logger) InfoEvent(component, event string, fields ...LogField) {
	l.writeEvent(logging.Info, component, event, fields...)
}

func (l *Logger) WarnEvent(component, event string, fields ...LogField) {
	l.writeEvent(logging.Warn, component, event, fields...)
}

func (l *Logger) ErrorEvent(component, event string, fields ...LogField) {
	l.writeEvent(logging.Error, component, event, fields...)
}

func (l *Logger) DebugEvent(component, event string, fields ...LogField) {
	l.writeEvent(logging.Debug, component, event, fields...)
}

func (l *Logger) SetStorageMonitor(monitor *StorageMonitor) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.storageMonitor = monitor
	l.mu.Unlock()
}

func (l *Logger) write(level string, format string, args ...interface{}) {
	if l == nil || l.logger == nil {
		return
	}
	l.mu.Lock()
	if !l.allows(logging.Level(level)) {
		l.mu.Unlock()
		return
	}
	monitor := l.storageMonitor
	if monitor != nil && !monitor.AllowWrite(StorageCapabilityAgentLog) {
		l.mu.Unlock()
		return
	}
	callerComponent := loggerCallerComponent()
	record := logging.FormatMessageAt(
		time.Now(),
		logging.Level(level),
		"agent",
		callerComponent,
		fmt.Sprintf(format, args...),
	)
	err := l.logger.Output(2, record)
	l.mu.Unlock()
	if err != nil && monitor != nil {
		monitor.HandleWriteError(err)
	}
}

func (l *Logger) writeEvent(level logging.Level, component, event string, fields ...LogField) {
	if l == nil || l.logger == nil {
		return
	}
	l.mu.Lock()
	if !l.allows(level) {
		l.mu.Unlock()
		return
	}
	monitor := l.storageMonitor
	if monitor != nil && !monitor.AllowWrite(StorageCapabilityAgentLog) {
		l.mu.Unlock()
		return
	}
	record := logging.FormatEventAt(time.Now(), level, "agent", component, event, fields...)
	err := l.logger.Output(2, record)
	l.mu.Unlock()
	if err != nil && monitor != nil {
		monitor.HandleWriteError(err)
	}
}

func loggerCallerComponent() string {
	_, file, _, ok := runtime.Caller(3)
	if !ok {
		return "runtime"
	}
	name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
	return logging.NormalizeIdentifier(name, "runtime")
}

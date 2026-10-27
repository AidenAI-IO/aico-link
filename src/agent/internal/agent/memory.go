package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultLockTimeout = 10 * time.Second

// MemoryManager owns the filesystem memory root. Conversation context itself
// lives in the ContextManager transcript; this manager is responsible for the
// long-term memory store, profile regeneration, and clearing persisted memory.
type MemoryManager struct {
	mu               sync.Mutex
	storageDir       string
	extraction       MemoryExtractionConfig
	profileFn        ProfileFn
	profileDebouncer *ProfileDebouncer
	longTerm         *LongTermMemoryStore
	longTermOnce     sync.Once
	lockTimeout      time.Duration
	logger           *Logger
	storageMonitor   *StorageMonitor
}

// EnsureLongTermStore returns the manager's long-term store, creating it under
// memoryDir on first use. All callers share one store so its parsed-Markdown
// cache and profile debouncer are not duplicated.
func (m *MemoryManager) EnsureLongTermStore(memoryDir string) *LongTermMemoryStore {
	if m == nil || memoryDir == "" {
		return nil
	}
	m.longTermOnce.Do(func() {
		if m.longTerm != nil {
			return
		}
		store := NewLongTermMemoryStore(
			filepath.Join(memoryDir, "long_term"),
			WithLifecycleDir(filepath.Join(memoryDir, "lifecycle")),
			WithStoreProfileFn(m.profileFn),
		)
		store.setProfileDebouncer(m.profileDebouncer)
		m.longTerm = store
	})
	return m.longTerm
}

type MemoryManagerOption func(*MemoryManager)

func WithExtractionConfig(cfg MemoryExtractionConfig) MemoryManagerOption {
	return func(m *MemoryManager) { m.extraction = normalizeMemoryExtractionConfig(cfg) }
}

// WithProfileFn sets the long-term profile generation function.
func WithProfileFn(fn ProfileFn) MemoryManagerOption {
	return func(m *MemoryManager) { m.profileFn = fn }
}

// WithMemoryProfileDebouncer sets the profile rebuild debouncer.
func WithMemoryProfileDebouncer(d *ProfileDebouncer) MemoryManagerOption {
	return func(m *MemoryManager) { m.profileDebouncer = d }
}

func WithLongTermMemoryStore(store *LongTermMemoryStore) MemoryManagerOption {
	return func(m *MemoryManager) { m.longTerm = store }
}

func WithMemoryLogger(logger *Logger) MemoryManagerOption {
	return func(m *MemoryManager) { m.logger = logger }
}

func NewMemoryManager(storageDir string, opts ...MemoryManagerOption) *MemoryManager {
	manager := &MemoryManager{
		storageDir:  storageDir,
		extraction:  DefaultMemoryExtractionConfig(),
		lockTimeout: defaultLockTimeout,
	}
	for _, opt := range opts {
		opt(manager)
	}
	return manager
}

func (m *MemoryManager) SetStorageMonitor(monitor *StorageMonitor) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storageMonitor = monitor
}

// effectiveContextWindow returns the yaml-configured fallback window, used only
// when the active model is unknown to the model_specs registry.
func (m *MemoryManager) effectiveContextWindow() int {
	if m == nil {
		return 0
	}
	return normalizeMemoryExtractionConfig(m.extraction).ContextWindow
}

// userProfilePromptMaxChars bounds the user profile in the system prompt so a
// long memory history cannot crowd out the instructions.
const userProfilePromptMaxChars = 1500

// UserProfilePrompt returns the saved profile, preference, and rule memories
// for the system prompt, or "" when none are available.
func (m *MemoryManager) UserProfilePrompt(ctx context.Context) string {
	if m == nil || m.longTerm == nil {
		return ""
	}
	prompt, err := m.longTerm.ProfilePrompt(ctx, userProfilePromptMaxChars)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("[memory] read user profile for prompt failed: %v", err)
		}
		return ""
	}
	return prompt
}

// RequestProfileRebuild asks the long-term store to regenerate profile.md. The
// store debounces bursts internally.
func (m *MemoryManager) RequestProfileRebuild() {
	if m == nil || m.storageDir == "" {
		return
	}
	longTerm := m.EnsureLongTermStore(m.storageDir)
	if longTerm == nil {
		return
	}
	longTerm.RequestProfileRebuild()
}

// ClearAll removes every persisted memory plane under the storage root.
func (m *MemoryManager) ClearAll(ctx context.Context, agentName string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || strings.TrimSpace(m.storageDir) == "" {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	fl := NewFileLock(m.storageDir)
	if err := fl.Lock(m.lockTimeout); err != nil {
		return fmt.Errorf("lock for removing memory %q: %w", agentName, err)
	}
	defer fl.Unlock()

	for _, path := range []string{
		filepath.Join(m.storageDir, "long_term"),
		filepath.Join(m.storageDir, "temporary"),
		filepath.Join(m.storageDir, "device"),
		filepath.Join(m.storageDir, "episodes"),
		filepath.Join(m.storageDir, "lifecycle"),
		filepath.Join(m.storageDir, "notifications"),
		filepath.Join(m.storageDir, "session"),
		filepath.Join(m.storageDir, "session_archive"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove filesystem memory path %q for %q: %w", path, agentName, err)
		}
	}
	return nil
}

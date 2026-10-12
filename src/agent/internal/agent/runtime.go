package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/compactor"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/agent/speech"
	"aiden-agent/internal/agent/statemanager"
	"aiden-agent/internal/agent/tokencounter"
	"aiden-agent/internal/agent/tts"
	"aiden-agent/internal/logging"
	"aiden-agent/internal/util"

	"github.com/google/uuid"
	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/callbacks"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func effectiveMaxIterations(configured int) int {
	if configured <= 0 {
		return math.MaxInt
	}
	return configured
}

const (
	currentEnvironmentHintMaxAge      = 10 * time.Minute
	runtimeSessionEventPersistTimeout = 2 * time.Second
	runtimeTTSCloseTimeout            = 5 * time.Second
	runtimeEpisodeMaintenanceTimeout  = 10 * time.Second
	runtimeTelemetryDrainTimeout      = 35 * time.Second
	maxPublicToolResultRunes          = maxToolObservationRunes
)

type Runtime struct {
	config                  Config
	configReloadMu          sync.Mutex
	configOperations        sync.RWMutex
	configPrepare           ConfigPrepareFunc
	configContextRotate     atomic.Bool
	configUserContextRotate atomic.Bool
	configStatusMu          sync.Mutex
	configStatus            ConfigApplyStatus
	configPending           *Config
	configSequence          uint64
	configWorkerRunning     bool
	configClosed            bool
	configWorkerWG          sync.WaitGroup
	configWorkerCancel      context.CancelFunc
	configMu                sync.RWMutex
	models                  model.Model
	memories                *MemoryManager
	tools                   *ToolSet
	skills                  *SkillManager
	skillsLoaded            bool
	skillsReloadMu          sync.Mutex
	skillsDirty             bool
	runGateInit             sync.Once
	mergeWorker             *MergeWorker
	logger                  *Logger
	profileDebouncer        *ProfileDebouncer
	waitForWakeup           *WaitForWakeupController
	voiceNotifications      *VoiceNotificationManager
	memoryPlane             MemoryPlane
	sessionManager          SessionManager
	contextManager          *contextmanager.ContextManager
	stateManager            *statemanager.StateManager
	runtimeID               string
	telemetrySessionID      string
	runGate                 chan struct{}
	preemptMu               sync.Mutex
	activeCancel            context.CancelCauseFunc
	preemptHooks            []func()
	lastPreemptTime         time.Time
	userContextResetMu      sync.Mutex
	userContextResetHook    func()
	userContextResetGen     uint64
	storage                 *StorageManager
	screenState             *screen.ScreenState
	phoneBridge             *PhoneBridge
	storageMonitor          *StorageMonitor
	ttsManager              *tts.ProviderManager
	ttsManagerOnce          sync.Once
	episodeMaintenance      asyncRuntimeWork
	telemetryExports        asyncRuntimeWork
	episodeMemoryInitErr    error
}

// ConfigSnapshot returns an immutable copy of the currently active runtime
// configuration. Callers must use this accessor instead of reading config
// directly so an internal reload cannot race request goroutines.
func (r *Runtime) ConfigSnapshot() Config {
	if r == nil {
		return Config{}
	}
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return r.config
}

type asyncRuntimeWork struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	closing bool
	ctx     context.Context
	cancel  context.CancelFunc
}

type RunRequest struct {
	Input             string
	Attachments       []InputAttachment
	Turn              TurnInput
	EpisodeID         string
	RequestID         string
	RuntimeContext    string
	DeviceEnvironment *PhoneEnvironment
	StreamWriter      io.Writer
	MaxTokens         int
	EventHandler      func(RunEvent)
	// OnRunActive runs after the previous run and its resources have been
	// preempted and this run owns the runtime gate. Callers can register
	// request-scoped output here without that output being interrupted by this
	// run's own startup preemption.
	OnRunActive   func(context.Context)
	SteerProvider func(context.Context) (RunSteerMessage, bool)
	// FinalSteerProvider is called at terminal executor boundaries. It should
	// atomically consume one last pending steer if available; otherwise it should
	// close the request's steer acceptance window.
	FinalSteerProvider func(context.Context) (RunSteerMessage, bool)
	// SteerInterrupt signals that the current run should pause before scheduling
	// more model/tool work while an out-of-band steering input is being captured.
	SteerInterrupt func() <-chan struct{}
	// SteerWaiter waits for the out-of-band steering capture to resolve. ok=false
	// means the interruption produced no usable input and the run may continue.
	SteerWaiter func(context.Context) (RunSteerMessage, bool, error)
	// AsyncEpisodeMaintenance keeps the episode trace write synchronous but moves
	// lesson extraction and referenced-memory maintenance off the response path.
	AsyncEpisodeMaintenance bool
	UserActionHandler       UserActionHandler
}

type RunResult struct {
	Output                 string      `json:"output"`
	EpisodeID              string      `json:"episode_id,omitempty"`
	Metrics                *RunMetrics `json:"metrics,omitempty"`
	WaitForWakeupRequested bool        `json:"wait_for_wakeup_requested,omitempty"`
	WaitForWakeupReason    string      `json:"wait_for_wakeup_reason,omitempty"`
	// Deprecated: use WaitForWakeupRequested.
	SleepRequested bool `json:"sleep_requested,omitempty"`
	// Deprecated: use WaitForWakeupReason.
	SleepReason    string       `json:"sleep_reason,omitempty"`
	SpeechStreamed bool         `json:"-"`
	Preempted      bool         `json:"preempted,omitempty"`
	TurnFailure    *TurnFailure `json:"-"`
}

func canonicalTurnInputFromRunRequest(req RunRequest) TurnInput {
	turn := normalizeTurnInput(req.Turn)
	if turn.InputText == "" && turn.Modality == TurnModalityText && len(turn.Attachments) == 0 && len(turn.Artifacts) == 0 && len(turn.TelemetryEvents) == 0 && turn.Transcript == "" && turn.OriginalText == "" {
		return NewTextTurnInput(req.Input, req.Attachments)
	}
	if len(turn.Attachments) == 0 && len(req.Attachments) > 0 {
		turn.Attachments = cloneInputAttachments(req.Attachments)
	}
	if turn.OriginalText == "" {
		turn.OriginalText = strings.TrimSpace(req.Input)
	}
	if turn.InputText == "" {
		turn.InputText = normalizeRunInput(req.Input, turn.Attachments)
	}
	return normalizeTurnInput(turn)
}

func episodeStartTimeWithEvents(fallback time.Time, events []TaskEpisodeEvent) time.Time {
	start := fallback
	for _, event := range events {
		eventTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(event.Ts))
		if err != nil || eventTime.IsZero() {
			continue
		}
		eventTime = eventTime.UTC()
		if start.IsZero() || eventTime.Before(start) {
			start = eventTime
		}
	}
	return start
}

func recordPreRunEpisodeEvents(recorder *EpisodeRecorder, events []TaskEpisodeEvent) {
	if recorder == nil {
		return
	}
	for _, event := range events {
		recorder.RecordEvent(event)
	}
}

func (r RunResult) SpokenText() string {
	return speech.BuildText(r.Output)
}

func (r RunResult) SpokenTextForConfig(cfg Config) string {
	if r.WaitForWakeupRequested {
		return ""
	}
	return speech.BuildText(r.Output)
}

type RunSteerMessage struct {
	ID        string    `json:"id,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

type RunMetrics struct {
	TotalDuration    float64 `json:"total_duration_ms"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens,omitempty"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	ContextWindow    int     `json:"context_window,omitempty"`
	FirstTokenTime   float64 `json:"first_token_time_ms,omitempty"`
	ToolCount        int     `json:"tool_count,omitempty"`
	// CachedPromptTokens accumulates provider-reported cached prompt tokens
	// (usage.prompt_tokens_details.cached_tokens) across the run. The prompt-cache
	// hit rate is CachedPromptTokens / PromptTokens.
	CachedPromptTokens int `json:"cached_prompt_tokens,omitempty"`
	// LastPromptTokens holds the largest single prompt-token count in the run.
	// PromptTokens/CompletionTokens/TotalTokens accumulate across model calls in
	// a single run, but the compression heuristic needs the size of one prompt
	// relative to the context window, not the cumulative sum.
	LastPromptTokens int `json:"-"`
}

// CacheHitRate returns the prompt-cache hit ratio in [0,1]: cached prompt tokens
// over total prompt tokens. It returns 0 when no prompt tokens were recorded.
func (m *RunMetrics) CacheHitRate() float64 {
	if m == nil || m.PromptTokens <= 0 {
		return 0
	}
	rate := float64(m.CachedPromptTokens) / float64(m.PromptTokens)
	if rate < 0 {
		return 0
	}
	if rate > 1 {
		return 1
	}
	return rate
}

const (
	runEventToolCall               = "tool_call"
	runEventSTTTranscription       = "stt_transcription"
	runEventVoicePromptSound       = "voice_prompt_sound"
	runEventTTSStreamPreopen       = "tts_stream_preopen"
	runEventMemoryRetrieve         = "memory_retrieve"
	runEventSessionBegin           = "session_begin"
	runEventIterationStart         = "iteration_start"
	runEventIterationEnd           = "iteration_end"
	runEventLoopGuardStop          = "loop_guard_stop"
	runEventToolResultContext      = "tool_result_context"
	runEventContextBudget          = "context_budget"
	runEventHistoricalContextPrune = "historical_context_prune"
	runEventConversationCompaction = "conversation_compaction"
	runEventModelRequestFailure    = "model_request_failure"
	runEventReasoningDelta         = "reasoning_delta"
	runEventReasoningReset         = "reasoning_reset"
	runEventToolProgress           = "tool_progress"
)

func historicalPruneEvent(stats compactor.HistoricalPruneStats, changed bool, err error, reason string) TaskEpisodeEvent {
	metadata := map[string]interface{}{
		"historical_states_dropped":          stats.HistoricalStatesDropped,
		"historical_tool_results_pruned":     stats.HistoricalToolResultsPruned,
		"current_turn_states_dropped":        stats.CurrentTurnStatesDropped,
		"current_turn_tool_exchanges_pruned": stats.CurrentTurnToolExchangesPruned,
		"tokens_before":                      stats.TokensBefore,
		"tokens_after":                       stats.TokensAfter,
		"changed":                            changed,
		"success":                            err == nil,
	}
	if reason != "" {
		metadata["reason"] = reason
	}
	return TaskEpisodeEvent{Type: runEventHistoricalContextPrune, Metadata: metadata}
}

func contextCompactionEvent(stats compactor.CompactionStats, compacted bool, err error, reason string) TaskEpisodeEvent {
	metadata := map[string]interface{}{
		"tokens_before":                 stats.TokensBefore,
		"tokens_after":                  stats.TokensAfter,
		"conversation_summary_required": stats.ConversationSummaryRequired,
		"compacted":                     compacted,
		"success":                       err == nil,
	}
	if reason != "" {
		metadata["reason"] = reason
	}
	return TaskEpisodeEvent{Type: runEventConversationCompaction, Metadata: metadata}
}

type RunEvent struct {
	Type             string     `json:"type"`
	Role             string     `json:"role,omitempty"`
	EpisodeID        string     `json:"episode_id,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	ToolName         string     `json:"tool_name,omitempty"`
	ToolInput        string     `json:"tool_input,omitempty"`
	ToolStatus       string     `json:"tool_status,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	SpeechEligible   bool       `json:"speech_eligible,omitempty"`
	Timestamp        time.Time  `json:"timestamp"`
	IsError          bool       `json:"is_error,omitempty"`
	ToolError        *ToolError `json:"tool_error,omitempty"`
}

// ContextWindowFn returns the active model's spec. It is called on demand so a
// model swap takes effect without a restart. Implementations return a zero
// ContextWindow when the window is unknown; callers then fall back to the
// yaml-configured default.
type ContextWindowFn func() model.ModelSpec

type usageTrackingModel struct {
	inner           model.Model
	metrics         *RunMetrics
	promptCapture   *telemetryPromptCapture
	contextWindowFn ContextWindowFn
}

func (m *usageTrackingModel) CallOptions() []chains.ChainCallOption { return m.inner.CallOptions() }

func (m *usageTrackingModel) Spec() model.ModelSpec {
	if m == nil || m.inner == nil {
		return model.ModelSpec{}
	}
	spec := m.inner.Spec()
	if spec.ContextWindow <= 0 {
		spec.ContextWindow = m.contextWindow()
	}
	return spec
}

func (m *usageTrackingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	startedAt := time.Now()
	res, err := m.inner.GenerateContent(ctx, messages, options...)
	if err == nil {
		recordUsageMetrics(m.metrics, res)
	}
	if m.promptCapture != nil {
		m.promptCapture.Record(ctx, startedAt, time.Now(), messages, options, res, err, m.contextWindow())
	}
	return res, err
}

// GenerateContentFromMessageList forwards provider-specific persisted context
// through the usage wrapper. Responses models use this to replay opaque
// reasoning items when store=false; ordinary models retain the standard path.
func (m *usageTrackingModel) GenerateContentFromMessageList(ctx context.Context, messageList []messages.Message, options ...llms.CallOption) (*llms.ContentResponse, error) {
	inner, ok := m.inner.(interface {
		GenerateContentFromMessageList(context.Context, []messages.Message, ...llms.CallOption) (*llms.ContentResponse, error)
	})
	if !ok {
		return m.GenerateContent(ctx, messages.ConvertMessageList(messageList), options...)
	}

	startedAt := time.Now()
	res, err := inner.GenerateContentFromMessageList(ctx, messageList, options...)
	if err == nil {
		recordUsageMetrics(m.metrics, res)
	}
	if m.promptCapture != nil {
		m.promptCapture.Record(ctx, startedAt, time.Now(), messages.ConvertMessageList(messageList), options, res, err, m.contextWindow())
	}
	return res, err
}

func (m *usageTrackingModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	startedAt := time.Now()
	out, err := m.inner.Call(ctx, prompt, options...)
	if m.promptCapture != nil {
		res := &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: out}}}
		m.promptCapture.Record(ctx, startedAt, time.Now(), []llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart(prompt)},
		}}, options, res, err, m.contextWindow())
	}
	return out, err
}

func (m *usageTrackingModel) contextWindow() int {
	if m == nil {
		return 0
	}
	if m.contextWindowFn != nil {
		if spec := m.contextWindowFn(); spec.ContextWindow > 0 {
			return spec.ContextWindow
		}
	}
	if m.metrics != nil {
		return m.metrics.ContextWindow
	}
	return 0
}

func NewRuntime(cfg Config) (*Runtime, error) {
	var mergeNeeded []SkillMergeJob

	// Sync bundled skills into user directory before loading
	if cfg.BundledSkillsDir != "" && cfg.ConfigDir != "" {
		report, err := SyncBundledSkills(context.Background(), SkillSyncOptions{
			ConfigDir:        cfg.ConfigDir,
			BundledSkillsDir: cfg.BundledSkillsDir,
			MergeModel:       cfg.SkillMergeModel,
			Quiet:            false,
		})
		if err != nil {
			logging.Warnf("agent", "skill_sync", "sync failed (non-fatal): %v", err)
		} else {
			mergeNeeded = report.MergeNeeded
		}
	}
	if cfg.ConfigDir != "" {
		skillsDir := filepath.Join(cfg.ConfigDir, "skills")
		if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
			cfg.SkillsDirs = []string{skillsDir}
		}
	}

	// Load skills from configured directories
	var skillIndex *SkillIndex
	var err error

	if len(cfg.SkillsDirs) > 0 {
		skillIndex, err = LoadSkillsFromDirs(cfg.SkillsDirs)
		if err != nil {
			return nil, fmt.Errorf("load skills: %w", err)
		}
	} else {
		skillIndex = NewSkillIndex()
	}

	// Create logger if ConfigDir is set
	var logger *Logger
	if cfg.ConfigDir != "" {
		logger, err = NewLogger(cfg.ConfigDir, cfg.Log.LLMHTTPRetentionDaysOrDefault(), cfg.Log.LevelOrDefault())
		if err != nil {
			return nil, fmt.Errorf("create logger: %w", err)
		}
		logger.Info("Agent runtime initialized with config from %s", cfg.ConfigDir)
	}

	memoryDir := ""
	if cfg.ConfigDir != "" {
		memoryDir = filepath.Join(cfg.ConfigDir, "memory")
	}

	waitForWakeupController := NewWaitForWakeupController()
	proxy := ProxyConfigFromEnvironment()
	if err := proxy.Validate(); err != nil {
		return nil, fmt.Errorf("proxy environment: %w", err)
	}
	screenState := &screen.ScreenState{}
	toolOptions := []BuiltinToolSetOption{
		WithScreenState(screenState),
		WithWaitForWakeupController(waitForWakeupController),
		WithScreenStableDefaults(cfg.ScreenStableDefaults()),
	}
	pythonErr := prepareManagedPythonPaths(managedPythonRoot, managedPythonTmp)
	if pythonErr != nil {
		if logger != nil {
			logger.Warn("managed python environment unavailable: %v", pythonErr)
		} else {
			logging.Warnf("agent", "runtime", "managed python environment unavailable: %v", pythonErr)
		}
	} else {
		toolOptions = append(toolOptions, WithShellTemporaryDirectory(managedPythonTmp))
	}
	toolSet := NewBuiltinToolSetFromConfig(
		cfg,
		proxy,
		toolOptions...,
	)
	extractionCfg := LoadMemoryExtractionConfig(cfg.ConfigDir)
	modelManagerOptions := []ModelManagerOption{}
	if cfg.ConfigDir != "" {
		modelManagerOptions = append(modelManagerOptions, WithProviderModelMetadataCachePath(filepath.Join(cfg.ConfigDir, "cache", "provider_model_metadata.json")))
		if cfg.Model.LogRawHTTP {
			modelManagerOptions = append(modelManagerOptions, WithLLMRawHTTPLogDir(filepath.Join(cfg.ConfigDir, "log")))
		}
	}
	modelManager := NewModelManager(cfg.Model, proxy, modelManagerOptions...)
	modelManager.prefetchProviderModelSpecIfNeeded()
	profileFn := buildLLMProfileFn(modelManager)

	longTermDir := ""
	if memoryDir != "" {
		longTermDir = filepath.Join(memoryDir, "long_term")
	}
	var debouncer *ProfileDebouncer
	var longTermStore *LongTermMemoryStore
	if longTermDir != "" {
		longTermStore = NewLongTermMemoryStore(longTermDir, WithLifecycleDir(filepath.Join(memoryDir, "lifecycle")), WithStoreProfileFn(profileFn))
		debouncer = NewProfileDebouncer(longTermStore.RegenerateProfileMD, 60*time.Second, logger)
		longTermStore.setProfileDebouncer(debouncer)
	}

	toolSet.RegisterMemoryTools(cfg.ConfigDir, memoryDir, longTermStore)
	toolSet.RegisterEnterTextTool(modelManager, nil) // deviceTypeFn set after runtime construction

	rt := NewRuntimeWithDeps(cfg, modelManager, NewMemoryManager(memoryDir, WithExtractionConfig(extractionCfg), WithProfileFn(profileFn), WithMemoryProfileDebouncer(debouncer), WithLongTermMemoryStore(longTermStore), WithMemoryLogger(logger)), toolSet, skillIndex)

	// Register skill tools after the runtime exists so skill_manage can mark the
	// skill index dirty. The updated index is reloaded at the start of the next run.
	if cfg.ConfigDir != "" {
		skillsDir := filepath.Join(cfg.ConfigDir, "skills")
		manifestPath := filepath.Join(cfg.ConfigDir, "skill-state", ".bundled_manifest.json")
		toolSet.RegisterSkillToolsWithDeviceType(skillsDir, manifestPath, rt.deviceTypeFromState, rt.MarkSkillsDirty)
	}
	rt.logger = logger
	rt.profileDebouncer = debouncer
	rt.waitForWakeup = waitForWakeupController
	rt.storageMonitor = newRuntimeStorageMonitor(cfg, logger)
	rt.SetVoiceNotificationSink(rt.VoiceNotificationSink())
	modelManager.SetStorageMonitor(rt.storageMonitor)
	if rt.memories != nil {
		rt.memories.SetStorageMonitor(rt.storageMonitor)
	}

	// Log runtime start marker.
	if logger != nil {
		logger.Info("[runtime] started: runtime_id=%s", rt.runtimeID)
	}

	if len(mergeNeeded) > 0 && cfg.SkillMergeModel != nil {
		manifestPath := filepath.Join(cfg.ConfigDir, "skill-state", ".bundled_manifest.json")
		worker := NewMergeWorker(cfg.SkillMergeModel, manifestPath)
		worker.onSuccess = func(SkillMergeJob) {
			rt.MarkSkillsDirty()
		}
		worker.Enqueue(mergeNeeded)
		worker.Start(context.Background())
		rt.mergeWorker = worker
	}

	if plane, ok := rt.memoryPlane.(*FilesystemMemoryPlane); ok && plane != nil {
		plane.logger = logger
	} else {
		rt.memoryPlane = NewFilesystemMemoryPlane(memoryDir, extractionCfg, logger, WithMemoryPlaneLongTermStore(longTermStore))
		rt.markInterruptedEpisodesBestEffort()
	}
	if plane, ok := rt.memoryPlane.(*FilesystemMemoryPlane); ok && plane != nil {
		plane.SetStorageWriteGate(rt.storageMonitor)
	}
	rt.startMemoryWorker(logger)

	// Config Web owns SD-card hardware and publishes a state mirror. Agent only
	// consumes that mirror for read-path routing, so the two processes never
	// race mounts, formats, ejections, or migrations.
	rt.storage = NewStorageStateView(cfg.Storage, defaultStorageStatePath, logger)

	rt.screenState = screenState

	// Create PhoneBridge in proxy mode if environment bridge is enabled
	endpoint := strings.TrimSpace(cfg.EnvironmentBridge.Endpoint)
	if cfg.EnvironmentBridge.Enabled && endpoint != "" {
		rt.phoneBridge = NewPhoneBridgeProxy(
			endpoint,
			cfg.EnvironmentBridge.BenchmarkTaskID,
			logger,
		)
		if logger != nil {
			logger.Info("phone-bridge: proxy mode enabled, endpoint=%s", endpoint)
		}
	} else {
		rt.phoneBridge = NewPhoneBridge(logger)
	}
	rt.phoneBridge.SetConfiguredPlatform(cfg.DevicePlatformOrDefault())
	rt.stateManager.RegisterUpdater(rt.phoneBridge)

	return rt, nil
}

// Storage returns the SD/eMMC storage manager, or nil when the runtime was
// built without one (NewRuntimeWithDeps).
func (r *Runtime) Storage() *StorageManager {
	return r.storage
}

func (r *Runtime) PhoneBridge() *PhoneBridge {
	return r.phoneBridge
}

// ttsProviderManager returns the process-wide provider manager shared by all
// TTS entrypoints. The stable manager exists even when TTS is not configured so
// a runtime provider switch is immediately visible to every consumer.
func (r *Runtime) ttsProviderManager() *tts.ProviderManager {
	if r == nil {
		return nil
	}
	r.ttsManagerOnce.Do(func() {
		if r.ttsManager != nil {
			return
		}
		manager, err := newTTSProviderManagerFromConfig(r.ConfigSnapshot(), r.logger)
		if err != nil {
			if r.logger != nil {
				r.logger.Warn("TTS init failed: %v", err)
			} else {
				logging.Warnf("agent", "tts", "init failed, continuing without TTS: %v", err)
			}
		}
		if manager == nil {
			manager = tts.NewProviderManager(nil, &ttsLoggerAdapter{logger: r.logger})
		}
		r.ttsManager = manager
	})
	return r.ttsManager
}

func NewRuntimeWithDeps(cfg Config, models model.Model, memories *MemoryManager, tools *ToolSet, skillIndex *SkillIndex) *Runtime {
	waitForWakeupController := NewWaitForWakeupController()
	if tools != nil {
		if tool, ok := tools.Get(toolWaitForWakeup); ok {
			if waitTool, ok := tool.(*WaitForWakeupTool); ok && waitTool.controller != nil {
				waitForWakeupController = waitTool.controller
			}
		}
	}
	skillManager := NewSkillManager(skillIndex)
	if cfg.ConfigDir != "" {
		skillManager.SetUsagePath(filepath.Join(cfg.ConfigDir, "skill-state", "usage.json"))
	}

	var memoryDir string
	var longTermStore *LongTermMemoryStore
	if cfg.ConfigDir != "" {
		memoryDir = filepath.Join(cfg.ConfigDir, "memory")
		if memories != nil {
			// Use sync.Once to ensure exactly one goroutine creates the fallback store
			longTermStore = memories.EnsureLongTermStore(memoryDir)
		} else {
			// No manager provided, create a standalone store
			storeOpts := []LongTermMemoryOption{WithLifecycleDir(filepath.Join(memoryDir, "lifecycle"))}
			longTermStore = NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"), storeOpts...)
		}
		if tools != nil {
			tools.RegisterMemoryTools(cfg.ConfigDir, memoryDir, longTermStore)
		}
	}

	rt := &Runtime{
		config:        cfg,
		models:        models,
		memories:      memories,
		tools:         tools,
		skills:        skillManager,
		skillsLoaded:  skillIndex != nil && len(skillIndex.Names()) > 0,
		waitForWakeup: waitForWakeupController,
		voiceNotifications: NewVoiceNotificationManager(
			cfg.VoiceNotifications,
			WithVoiceNotificationLocale(resolvedVoiceNotificationLocale(cfg)),
		),
		runtimeID:          uuid.NewString(),
		telemetrySessionID: uuid.NewString(),
		stateManager:       statemanager.NewStateManager(),
	}
	// Partition raw HTTP logs by the live conversation, which is the current
	// ContextManager session.
	if modelManager, ok := models.(*ModelManager); ok {
		modelManager.SetSessionIDProvider(func() string {
			if cfg.ConfigDir == "" {
				return ""
			}
			return contextmanager.CurrentSessionID(agentpath.ContextManagerSessionFolder(cfg.ConfigDir))
		})
	}
	if cfg.ConfigDir != "" {
		rt.memoryPlane = NewFilesystemMemoryPlane(memoryDir, LoadMemoryExtractionConfig(cfg.ConfigDir), nil, WithMemoryPlaneLongTermStore(longTermStore))
		rt.markInterruptedEpisodesBestEffort()
		rt.startMemoryWorker(nil)
	}
	rt.stateManager.RegisterUpdater(&deviceStateUpdater{snapshot: rt.ConfigSnapshot})
	skillManager.SetDeviceTypeFunc(rt.deviceTypeFromState)
	rt.tools.SetRuntimeDeviceTypeFn(rt.deviceTypeFromState)
	rt.sessionManager = newMemoryManagerSessionManager(memories)
	rt.initRunGate()
	return rt
}

func (r *Runtime) startMemoryWorker(runtimeLogger *Logger) {
	if r == nil {
		return
	}
	plane, ok := r.memoryPlane.(*FilesystemMemoryPlane)
	if !ok || plane == nil {
		r.episodeMemoryInitErr = nil
		return
	}
	if runtimeLogger != nil {
		plane.logger = runtimeLogger
	}
	err := plane.StartMemoryWorker(r.models)
	r.episodeMemoryInitErr = err
	if err == nil {
		return
	}
	if runtimeLogger != nil {
		runtimeLogger.Warn("[memory] worker disabled: %v", err)
		return
	}
	logging.Warnf("agent", "memory", "worker disabled: %v", err)
}

// EpisodeMemoryInitializationError reports why the background consolidation
// worker could not start. A nil error means it is running or not configured.
func (r *Runtime) EpisodeMemoryInitializationError() error {
	if r == nil {
		return nil
	}
	return r.episodeMemoryInitErr
}

func (r *Runtime) initRunGate() {
	if r == nil {
		return
	}
	r.runGateInit.Do(func() {
		r.runGate = make(chan struct{}, 1)
		r.runGate <- struct{}{}
	})
}

func (r *Runtime) deviceTypeFromState() string {
	if r != nil && r.stateManager != nil {
		for _, entry := range r.stateManager.GetAllStates() {
			if entry.Key != "device_type" {
				continue
			}
			if deviceType, ok := normalizeDeviceType(entry.Value); ok {
				return deviceType
			}
		}
	}
	if r != nil {
		return r.ConfigSnapshot().DeviceTypeOrDefault()
	}
	return defaultDeviceType
}

func (r *Runtime) devicePlatformFromState() string {
	return deviceTypePlatform(r.deviceTypeFromState())
}

func (r *Runtime) devicePointerModeFromState() string {
	return DeviceConfig{DeviceType: r.deviceTypeFromState()}.PointerModeOrDefault()
}

func (r *Runtime) lockRun(ctx context.Context) (func(), error) {
	if r == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.initRunGate()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.runGate:
		if err := ctx.Err(); err != nil {
			r.runGate <- struct{}{}
			return nil, err
		}
		return func() {
			r.runGate <- struct{}{}
		}, nil
	}
}

// Preempt terminates the currently active run and releases all associated
// resources (audio recording, TTS playback) via registered hooks.
func (r *Runtime) Preempt() {
	if r == nil {
		return
	}
	r.preemptMu.Lock()
	defer r.preemptMu.Unlock()
	if r.activeCancel != nil {
		r.activeCancel(errRunPreempted)
		r.lastPreemptTime = time.Now()
	}
	for i, hook := range r.preemptHooks {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if r.logger != nil {
						r.logger.Warn("[preempt] hook %d panicked: %v", i, recovered)
					} else {
						logging.Warnf("agent", "preempt", "hook %d panicked: %v", i, recovered)
					}
				}
			}()
			hook()
		}()
	}
}

// RegisterPreemptHook adds a callback invoked when a new run preempts the
// current one. Used to release audio resources (stop recording, stop TTS).
func (r *Runtime) RegisterPreemptHook(hook func()) {
	if r == nil || hook == nil {
		return
	}
	r.preemptMu.Lock()
	defer r.preemptMu.Unlock()
	r.preemptHooks = append(r.preemptHooks, hook)
}

// WasPreempted reports whether a preemption occurred within the last duration.
func (r *Runtime) WasPreempted(within time.Duration) bool {
	if r == nil {
		return false
	}
	r.preemptMu.Lock()
	defer r.preemptMu.Unlock()
	return !r.lastPreemptTime.IsZero() && time.Since(r.lastPreemptTime) < within
}

func (r *Runtime) NewEpisodeID() string {
	if r == nil || r.memoryPlane == nil {
		return ""
	}
	return newTaskEpisodeID(time.Now().UTC())
}

func (r *Runtime) MemoryPlane() MemoryPlane {
	if r == nil {
		return nil
	}
	return r.memoryPlane
}

func (r *Runtime) markInterruptedEpisodesBestEffort() {
	plane, ok := r.memoryPlane.(*FilesystemMemoryPlane)
	if !ok || plane == nil || plane.episodes == nil {
		return
	}
	episodes, err := plane.episodes.MarkRunningEpisodesInterruptedWithDetails(context.Background(), "agent restarted before the task episode completed")
	if err != nil {
		if r.logger != nil {
			r.logger.Warn("[memory] mark interrupted episodes failed: %v", err)
		}
		return
	}
	if len(episodes) == 0 {
		return
	}
	r.exportInterruptedEpisodesBestEffort(episodes)
	if r.logger != nil {
		r.logger.Warn("[memory] marked %d running task episode(s) as interrupted", len(episodes))
	}
}

func (r *Runtime) exportInterruptedEpisodesBestEffort(episodes []TaskEpisode) {
	if len(episodes) == 0 {
		return
	}
	for _, episode := range episodes {
		enrichEpisodeTelemetry(&episode, r.ConfigSnapshot())
		r.enrichEpisodeRuntimeTelemetry(&episode)
		if episode.Extra == nil {
			episode.Extra = map[string]interface{}{}
		}
		episode.Extra["interruption_source"] = "agent_restart"
		r.exportEpisodeBestEffort(episode, nil)
	}
}

func (r *Runtime) Run(ctx context.Context, req RunRequest) (result RunResult, runErr error) {

	defer func() {
		if runErr != nil && isLLMTurnFailureSource(runErr) {
			result.TurnFailure = TurnFailureFromError(runErr)
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if plane, ok := r.memoryPlane.(*FilesystemMemoryPlane); ok {
		plane.MemoryTaskStarted()
		defer plane.MemoryTaskFinished()
	}

	// Preempt any currently active run and its resources.
	r.Preempt()

	unlockRun, err := r.lockRun(ctx)
	if err != nil {
		return RunResult{}, err
	}
	defer unlockRun()
	r.configOperations.RLock()
	defer r.configOperations.RUnlock()

	// A realtime session configured without a backend agent must execute the
	// transferred tools itself. Take the decision under the same config snapshot
	// the rest of the run reads, so a reload cannot flip it mid-run.
	cfg := r.ConfigSnapshot()
	if cfg.InputModeOrDefault() == "realtime" && !cfg.VoiceModel.UseBackendAgent {
		return RunResult{}, fmt.Errorf("backend agent is disabled for this realtime session; all processing should be handled by the realtime voice session")
	}

	// Register this run's cancel so future callers can preempt us.
	runCtx, runCancel := context.WithCancelCause(ctx)
	if req.UserActionHandler != nil {
		runCtx = WithUserActionHandler(runCtx, req.UserActionHandler)
	}
	r.preemptMu.Lock()
	r.activeCancel = runCancel
	r.preemptMu.Unlock()
	defer func() {
		r.preemptMu.Lock()
		r.activeCancel = nil
		r.preemptMu.Unlock()
		runCancel(nil)
	}()

	// Check if we were preempted while waiting for runGate.
	if err := runCtx.Err(); err != nil {
		return RunResult{Preempted: true}, err
	}
	return r.withIOSKeyboardIsolationRun(runCtx, func(isolationCtx context.Context) (RunResult, error) {
		if req.OnRunActive != nil {
			req.OnRunActive(isolationCtx)
		}
		return r.run(isolationCtx, req)
	})
}

func (r *Runtime) withIOSKeyboardIsolationRun(
	ctx context.Context,
	action func(context.Context) (RunResult, error),
) (result RunResult, runErr error) {
	if r == nil || r.toolSnapshot() == nil || r.toolSnapshot().iosKeyboardIsolation == nil {
		return action(ctx)
	}
	runErr = r.toolSnapshot().iosKeyboardIsolation.withBatch(ctx, func(batchCtx context.Context) error {
		result, runErr = action(batchCtx)
		return runErr
	})
	return result, runErr
}

func (r *Runtime) run(ctx context.Context, req RunRequest) (result RunResult, runErr error) {
	var err error

	startTime := time.Now()
	metrics := &RunMetrics{}
	turnInput := canonicalTurnInputFromRunRequest(req)
	preRunEvents := cloneTaskEpisodeEvents(turnInput.TelemetryEvents)
	normalizedInput := turnInput.InputText
	cfg := r.ConfigSnapshot()
	if r.waitForWakeup != nil {
		r.waitForWakeup.Consume()
	}

	if r.logger != nil {
		r.logger.Info("Starting agent run: input=%q modality=%s attachments=%d provider=%s model=%s", normalizedInput, turnInput.Modality, len(turnInput.Attachments), cfg.Model.Provider, cfg.Model.Model)
	}

	if normalizedInput == "" {
		return RunResult{}, errors.New("input is required")
	}

	runID := "run_" + uuid.NewString()
	episodeID := strings.TrimSpace(req.EpisodeID)
	if episodeID == "" && r.memoryPlane != nil {
		episodeID = newTaskEpisodeID(startTime.UTC())
	}
	currentHints := r.currentEnvironmentHints()
	promptCapture := newTelemetryPromptCapture(r.ConfigSnapshot().Telemetry.EnabledOrDefault())
	var episodeRecorder *EpisodeRecorder
	var availableTools []langtools.Tool
	chunkRecalls := &atomic.Int64{}
	var output string
	episodeCommitted := false
	defer func() {
		if runErr == nil || episodeCommitted {
			return
		}
		metrics.TotalDuration = float64(time.Since(startTime).Milliseconds())
		if episodeRecorder == nil && r.memoryPlane != nil {
			retrieveReq := MemoryRetrieveRequest{
				Input:        normalizedInput,
				Attachments:  turnInput.Attachments,
				ToolNames:    toolNamesFromTools(availableTools),
				EpisodeID:    episodeID,
				DeviceID:     defaultMemoryDeviceID,
				CurrentHints: currentHints,
			}
			episodeRecorder = r.memoryPlane.NewEpisodeRecorder(retrieveReq, MemoryContext{})
			if episodeRecorder != nil {
				episodeRecorder.setStartedAtIfEarlier(episodeStartTimeWithEvents(startTime.UTC(), preRunEvents))
				episodeID = episodeRecorder.ID()
				startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if err := episodeRecorder.Start(startCtx); err != nil && r.logger != nil {
					r.logger.Warn("[memory] start failed episode trace failed: %v", err)
				}
				cancel()
				recordPreRunEpisodeEvents(episodeRecorder, preRunEvents)
			}
		}
		if episodeRecorder == nil {
			return
		}
		episodeID = episodeRecorder.ID()
		r.commitEpisodeBestEffort(episodeRecorder, normalizedInput, output, metrics, runErr, promptCapture, chunkRecalls, req.AsyncEpisodeMaintenance)
		episodeCommitted = true
	}()

	if err := r.reloadSkillsIfDirty(); err != nil {
		return RunResult{}, err
	}

	contextWindow := r.effectiveContextWindow()
	if contextWindow > 0 {
		metrics.ContextWindow = contextWindow
		if r.logger != nil {
			r.logger.Info("Resolved model context window: context_window=%d", contextWindow)
		}
	}
	m := &usageTrackingModel{inner: r.models, metrics: metrics, promptCapture: promptCapture, contextWindowFn: func() model.ModelSpec {
		if r.models != nil {
			return r.models.Spec()
		}
		return model.ModelSpec{}
	}}

	sessionBeginStart := time.Now()
	beginResult, err := r.beginSession(ctx, SessionBeginRequest{
		AgentName:    "default",
		Input:        normalizedInput,
		Turn:         turnInput,
		RuntimeID:    r.runtimeID,
		EpisodeID:    episodeID,
		RequestID:    req.RequestID,
		RunID:        runID,
		CurrentHints: currentHints,
	})
	sessionBeginDuration := time.Since(sessionBeginStart).Milliseconds()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RunResult{}, ctxErr
		}
		if r.logger != nil {
			r.logger.Warn("[memory] begin session failed; continuing without session update: %v", err)
		}
		beginResult = SessionBeginResult{}
	}
	if beginResult.PendingRecallCounter != nil {
		chunkRecalls = beginResult.PendingRecallCounter
	}
	sessionBeginEvent := TaskEpisodeEvent{
		Type:       runEventSessionBegin,
		Ts:         sessionBeginStart.Format(time.RFC3339Nano),
		DurationMs: &sessionBeginDuration,
	}

	availableTools = r.availableTools()
	availableTools = wrapSessionRecallTelemetry(availableTools, chunkRecalls)
	retrieveReq := MemoryRetrieveRequest{
		Input:        normalizedInput,
		Attachments:  turnInput.Attachments,
		ToolNames:    toolNamesFromTools(availableTools),
		EpisodeID:    episodeID,
		DeviceID:     defaultMemoryDeviceID,
		CurrentHints: currentHints,
	}
	// Memories are no longer retrieved up front. The agent pulls what it needs
	// on demand through the recall tools, which record the referenced IDs on the
	// episode recorder so Episode Memory consolidation checks those records first
	// for updates and conflicts.
	if r.memoryPlane != nil {
		episodeRecorder = r.memoryPlane.NewEpisodeRecorder(retrieveReq, MemoryContext{})
		if episodeRecorder != nil {
			episodeRecorder.setStartedAtIfEarlier(episodeStartTimeWithEvents(startTime.UTC(), preRunEvents))
			if err := episodeRecorder.Start(ctx); err != nil && r.logger != nil {
				r.logger.Warn("[memory] start episode failed: %v", err)
			}
			recordPreRunEpisodeEvents(episodeRecorder, preRunEvents)
			episodeRecorder.RecordEvent(sessionBeginEvent)
		}
	}
	if episodeRecorder != nil {
		episodeID = episodeRecorder.ID()
	}

	maxIterations := effectiveMaxIterations(cfg.MaxIterations)

	// Record tool count in metrics for observability
	metrics.ToolCount = len(availableTools)

	callOptions := r.models.CallOptions()
	if req.MaxTokens > 0 {
		callOptions = append(callOptions, chains.WithMaxTokens(req.MaxTokens))
	}
	var streamCallbackHandler *runtimeCallbackHandler
	if req.StreamWriter != nil || req.EventHandler != nil || req.SteerProvider != nil || r.logger != nil {
		streamCallbackHandler = &runtimeCallbackHandler{
			writer:       req.StreamWriter,
			metrics:      metrics,
			startTime:    startTime,
			logger:       r.logger,
			eventHandler: req.EventHandler,
			episodeID:    episodeID,
			runtimeID:    r.runtimeID,
			requestID:    req.RequestID,
			runID:        runID,
			episode:      episodeRecorder,
		}
	}
	if streamCallbackHandler != nil && req.StreamWriter != nil {
		streamCallbackHandler.ResetStreaming(ctx)
		callOptions = append(callOptions, chains.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			streamCallbackHandler.HandleStreamingFunc(ctx, chunk)
			return nil
		}))
	}
	var executorHandler callbacks.Handler
	if streamCallbackHandler != nil {
		executorHandler = streamCallbackHandler
	}
	profile := r.buildAgentProfile(r.skills, availableTools)
	var steerStatus steerConversationStatus
	var steerRecorder steerConversationRecorder
	if req.SteerProvider != nil {
		tracker := newSteerConversationTracker()
		steerStatus = tracker
		steerRecorder = tracker
	}

	if recoveredManager, err := recoverPendingBackendRunManager(agentpath.ContextManagerSessionFolder(cfg.ConfigDir), r.contextManager); err != nil {
		return RunResult{}, err
	} else if recoveredManager != nil {
		if recoveredManager != r.contextManager {
			recoveredManager.AddAppendMessageHook(r.getStateHook())
		}
		r.contextManager = recoveredManager
	}

	// Configuration changes rotate at the next task boundary, preserving the
	// complete old transcript and provider-specific continuation metadata.
	// setup context manager if not initialized
	hadContextManager := r.contextManager != nil
	if r.contextManager == nil {
		r.contextManager, err = InitializeContextManager(profile.SystemPrompt, agentpath.ContextManagerSessionFolder(cfg.ConfigDir), []contextmanager.AppendMessageHook{r.getStateHook()})
		if err != nil {
			return RunResult{}, err
		}
	}
	if r.configContextRotate.Load() && hadContextManager {
		candidate, resetErr := contextmanager.NewContextManagerCandidate(agentpath.ContextManagerSessionFolder(cfg.ConfigDir), profile.SystemPrompt)
		if resetErr != nil {
			return RunResult{}, resetErr
		}
		if resetErr = r.contextManager.Activate(candidate); resetErr != nil {
			return RunResult{}, resetErr
		}
	}
	if r.configContextRotate.Load() {
		r.configContextRotate.Store(false)
	}
	// append runtime context as assistant message if present (e.g., voice interruption notification)
	if runtimeContext := strings.TrimSpace(req.RuntimeContext); runtimeContext != "" {
		if err := r.contextManager.AppendMessage(messages.Message{
			Role:    messages.MessageRoleAssistant,
			Content: runtimeContext,
		}); err != nil {
			return RunResult{}, err
		}
	}
	// append user message to context manager
	if err := r.contextManager.AppendMessage(userMessageFromInput(r.contextManager, turnInput.InputText, turnInput.Attachments)); err != nil {
		return RunResult{}, err
	}

	notices, err := startRunNotices(func() *contextmanager.ContextManager { return r.contextManager }, runID)
	if err != nil {
		return RunResult{}, err
	}
	// Covers preparation failures before AgentLoop starts as well as panics.
	defer notices.finishOnReturn(ctx, &runErr)

	// The compactor runs on the run's usage-tracking model so its model calls are
	// counted and traced as part of the episode rather than disappearing from the
	// run's token accounting.
	contextCompactor := compactor.NewCompactor(compactor.DefaultProtectRule, m)
	budgetContextWindow := contextWindow
	if budgetContextWindow <= 0 {
		budgetContextWindow = r.models.Spec().ContextWindow
	}
	maxResponseTokens := r.models.Spec().MaxOutput
	if cfg.Model.MaxResponseTokens > 0 {
		maxResponseTokens = cfg.Model.MaxResponseTokens
	}
	if req.MaxTokens > 0 {
		maxResponseTokens = req.MaxTokens
	}
	usableInputBudget := toolResultUsableInputBudget(budgetContextWindow, maxResponseTokens)
	compactionTrigger, compactionEnabled := conversationCompactionTrigger(usableInputBudget, cfg.ContextCompactionThresholdOrDefault())
	contextBudgetOptions := chains.GetLLMCallOptions(callOptions...)
	contextBudgetOptions = append(contextBudgetOptions, llms.WithTools((&FunctionAgent{Tools: profile.Tools}).toolsAsLLM()))
	var contextBudgetCallOptions llms.CallOptions
	for _, option := range contextBudgetOptions {
		if option != nil {
			option(&contextBudgetCallOptions)
		}
	}
	tokenUsage := estimateActivePromptTokens(r.contextManager, contextBudgetOptions)
	messageTokenUsage := activeMessageTokens(r.contextManager, contextBudgetCallOptions)

	// Historical state and tool-result pruning is deterministic and has its own
	// configurable trigger. It is intentionally independent from conversation
	// summarization and provider-managed Responses compaction.
	pruneTrigger, pruneTarget, pruneEnabled := historicalPruneBudgets(usableInputBudget, cfg.ContextPruneThresholdOrDefault())
	if pruneEnabled && messageTokenUsage > pruneTrigger {
		if r.logger != nil {
			r.logger.Info("Historical context prune: token usage reached threshold; tokenUsage=%d trigger=%d target=%d", messageTokenUsage, pruneTrigger, pruneTarget)
		}
		newManager, pruned, pruneErr := contextCompactor.PruneHistorical(r.contextManager, pruneTarget)
		if episodeRecorder != nil {
			episodeRecorder.RecordEvent(historicalPruneEvent(
				contextCompactor.LastPruneStats(),
				pruned,
				pruneErr,
				"threshold",
			))
		}
		if pruneErr != nil {
			return RunResult{}, pruneErr
		}
		if pruned {
			if err = r.contextManager.Activate(newManager); err != nil {
				return RunResult{}, err
			}
			tokenUsage = estimateActivePromptTokens(r.contextManager, contextBudgetOptions)
		}
	}

	if !cfg.Model.ResponsesProviderCompactionEnabled() && compactionEnabled && tokenUsage > compactionTrigger {
		if r.logger != nil {
			r.logger.Info("Compaction: token usage reached the threshold, summarizing conversation... tokenUsage: %d, trigger: %d, contextWindow: %d", tokenUsage, compactionTrigger, contextWindow)
		}
		newManager, compacted, err := contextCompactor.Compact(withTelemetryRole(ctx, telemetryRoleCompaction), r.contextManager, r.sessionChunkWriter())
		if episodeRecorder != nil {
			episodeRecorder.RecordEvent(contextCompactionEvent(
				contextCompactor.LastCompactionStats(),
				compacted,
				err,
				"threshold",
			))
		}
		if err != nil {
			return RunResult{}, err
		}
		if compacted {
			if err = r.contextManager.Activate(newManager); err != nil {
				return RunResult{}, err
			}
		}
	}

	agentLoop := NewAgentLoop(m, profile, maxIterations, executorHandler, episodeRecorder, cfg.ScreenshotPruningOrDefault(), r.contextManager)
	agentLoop.notices = notices
	agentLoop.ScreenState = r.screenState
	agentLoop.SteerRecorder = steerRecorder
	agentLoop.ToolResultObserver = newScreenToolResultObserver(r.screenState)
	agentLoop.SteerInterrupt = req.SteerInterrupt
	agentLoop.SteerProvider = req.SteerProvider
	agentLoop.SteerWaiter = req.SteerWaiter
	agentLoop.TerminationPolicy = NewTerminationPolicy(cfg.TerminationPolicy)
	agentLoop.DevicePlatform = r.devicePlatformFromState()
	agentLoop.PointerMode = r.devicePointerModeFromState()
	agentLoop.ContextBudgetGuard = func(guardCtx context.Context, currentManager *contextmanager.ContextManager, options llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
		if currentManager == nil {
			return nil, false, nil
		}
		messageTokens := activeMessageTokens(currentManager, options)
		toolSchemaTokens := tokencounter.EstimateToolSchemaTokens(options)
		targetTokens := 0
		reason := ""
		hardBudgetExceeded := false
		if pruneEnabled && messageTokens > pruneTrigger {
			targetTokens = pruneTarget
			reason = "active_turn_threshold"
		}
		if usableInputBudget > 0 && activePromptTokens(currentManager, options) > usableInputBudget {
			hardBudgetExceeded = true
			hardTarget := max(1, usableInputBudget-toolSchemaTokens)
			// The emergency pass may break recent-exchange protection, so only
			// reduce as far as the hard request budget requires. Normal pruning
			// will continue toward its lower hysteresis target without touching
			// those recent exchanges once the request is safe again.
			targetTokens = hardTarget
			if reason == "" {
				reason = "active_turn_input_budget"
			} else {
				reason = "active_turn_threshold_and_input_budget"
			}
		}
		if targetTokens == 0 {
			return currentManager, false, nil
		}

		if r.logger != nil {
			r.logger.Info("Context prune: model request reached budget during active run; messageTokens=%d toolSchemaTokens=%d trigger=%d target=%d usableInputBudget=%d reason=%s",
				messageTokens, toolSchemaTokens, pruneTrigger, targetTokens, usableInputBudget, reason)
		}
		var newManager *contextmanager.ContextManager
		var pruned bool
		var pruneErr error
		if hardBudgetExceeded {
			newManager, pruned, pruneErr = contextCompactor.PruneForHardBudget(currentManager, targetTokens)
		} else {
			newManager, pruned, pruneErr = contextCompactor.PruneForBudget(currentManager, targetTokens)
		}
		activeManager := currentManager
		if pruned && newManager != nil {
			activeManager = newManager
		}
		var budgetErr error
		if pruneErr == nil && hardBudgetExceeded {
			afterMessageTokens := activeMessageTokens(activeManager, options)
			if activePromptTokens(activeManager, options) > usableInputBudget {
				budgetErr = fmt.Errorf("context remains over usable input budget after pruning: messageTokens=%d toolSchemaTokens=%d usableInputBudget=%d",
					afterMessageTokens, toolSchemaTokens, usableInputBudget)
			}
		}
		if episodeRecorder != nil {
			episodeRecorder.RecordEvent(historicalPruneEvent(
				contextCompactor.LastPruneStats(),
				pruned,
				errors.Join(pruneErr, budgetErr),
				reason,
			))
		}
		if pruneErr != nil {
			return nil, false, pruneErr
		}
		changed := pruned
		var pendingChunk pendingSessionChunk
		if budgetErr != nil {
			// Deterministic pruning cannot shrink historical user/assistant text.
			// Try the existing summary recovery before failing locally, including
			// when provider-managed compaction disables threshold summaries.
			compactedManager, compacted, compactErr := contextCompactor.Compact(withTelemetryRole(guardCtx, telemetryRoleCompaction), activeManager, &pendingChunk)
			if episodeRecorder != nil {
				episodeRecorder.RecordEvent(contextCompactionEvent(
					contextCompactor.LastCompactionStats(), compacted, compactErr, "active_turn_input_budget",
				))
			}
			if compactErr != nil {
				return nil, false, fmt.Errorf("compact context for hard input budget: %w", compactErr)
			}
			if compacted {
				activeManager = compactedManager
				changed = true
			}
			afterMessageTokens := activeMessageTokens(activeManager, options)
			if activePromptTokens(activeManager, options) > usableInputBudget {
				return nil, false, fmt.Errorf("context remains over usable input budget after pruning and compaction: messageTokens=%d toolSchemaTokens=%d usableInputBudget=%d",
					afterMessageTokens, toolSchemaTokens, usableInputBudget)
			}
		}
		// Only activate a revision after all preparation and budget checks pass.
		if changed {
			if switchErr := currentManager.Activate(activeManager); switchErr != nil {
				return nil, false, switchErr
			}
			// Rejected candidates must not become searchable history. Once the
			// revision is active, a chunk write failure must not roll back the
			// loop's manager; the parent transcript still retains the full span.
			if chunkErr := pendingChunk.persist(guardCtx, r.sessionChunkWriter()); chunkErr != nil {
				if r.logger != nil {
					r.logger.Warn("Context budget recovery: failed to persist accepted summary chunk: %v", chunkErr)
				}
			}
		}
		return currentManager, changed, nil
	}
	compactAgentContext := func(recoveryCtx context.Context, currentManager *contextmanager.ContextManager, triggerReason string) (*contextmanager.ContextManager, bool, error) {
		// Compaction summarises the conversation with the same model as the run;
		// tag it so its prompt becomes a `summarize-context` generation.
		newManager, compacted, compactErr := contextCompactor.Compact(withTelemetryRole(recoveryCtx, telemetryRoleCompaction), currentManager, r.sessionChunkWriter())
		if episodeRecorder != nil {
			episodeRecorder.RecordEvent(contextCompactionEvent(
				contextCompactor.LastCompactionStats(),
				compacted,
				compactErr,
				triggerReason,
			))
		}
		if compactErr != nil {
			return nil, false, compactErr
		}
		if !compacted {
			return currentManager, false, nil
		}
		if switchErr := currentManager.Activate(newManager); switchErr != nil {
			return nil, false, switchErr
		}
		return currentManager, true, nil
	}
	if !cfg.Model.ResponsesProviderCompactionEnabled() && compactionEnabled {
		agentLoop.ContextCompactionTrigger = compactionTrigger
		agentLoop.ContextThresholdCompaction = func(recoveryCtx context.Context, currentManager *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
			if r.logger != nil {
				r.logger.Info("Compaction: context reached the threshold inside the agent loop; compacting before the next model call")
			}
			return compactAgentContext(recoveryCtx, currentManager, "agent_loop_threshold")
		}
	}
	agentLoop.ContextOverflowRecovery = func(recoveryCtx context.Context, currentManager *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
		if r.logger != nil {
			r.logger.Info("Compaction: provider rejected the request because the context window was exceeded; compacting and retrying")
		}
		return compactAgentContext(recoveryCtx, currentManager, "provider_context_exceeded")
	}

	output, err = agentLoop.Run(ctx, normalizedInput, callOptions...)
	// If the agent couldn't parse the LLM output format, extract the raw
	// text and return it as the response instead of failing.
	if errors.Is(err, agents.ErrUnableToParseOutput) {
		raw := err.Error()
		const prefix = "unable to parse agent output: "
		if idx := strings.Index(raw, prefix); idx >= 0 {
			output = strings.TrimSpace(raw[idx+len(prefix):])
			err = nil
		}
	}
	// End execution tracking before session bookkeeping and TTS. Canceling
	// playback after a completed loop must not mark its task interrupted.
	if noticeErr := notices.finish(ctx, err); noticeErr != nil {
		err = errors.Join(err, noticeErr)
	}
	if err != nil {
		return RunResult{}, err
	}

	output = strings.TrimSpace(output)
	metrics.TotalDuration = float64(time.Since(startTime).Milliseconds())
	if r.logger != nil && metrics.PromptTokens > 0 {
		r.logger.Info("LLM usage: prompt_tokens=%d completion_tokens=%d total_tokens=%d cached_tokens=%d cache_hit_rate=%.1f%%",
			metrics.PromptTokens, metrics.CompletionTokens, metrics.TotalTokens,
			metrics.CachedPromptTokens, metrics.CacheHitRate()*100)
	}
	commitReq := SessionCommitRequest{
		AgentName: "default",
		Input:     normalizedInput,
		Output:    output,
		Metrics:   metrics,
		RuntimeID: r.runtimeID,
		EpisodeID: episodeID,
		RequestID: req.RequestID,
		RunID:     runID,
	}
	if steerStatus != nil && steerStatus.HasSteerMessages() {
		commitReq.Steers = steerStatus.SteerMessages()
	}

	_, err = r.commitSession(ctx, commitReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return RunResult{}, ctxErr
		}
		if r.logger != nil {
			r.logger.Warn("[memory] commit session failed; returning model output anyway: %v", err)
		}
	}
	if streamCallbackHandler != nil {
		streamCallbackHandler.HandleAssistantOutput(ctx, output)
	}
	r.commitEpisodeBestEffort(episodeRecorder, normalizedInput, output, metrics, nil, promptCapture, chunkRecalls, req.AsyncEpisodeMaintenance)
	episodeCommitted = true

	waitForWakeupRequested, waitForWakeupReason := false, ""
	if r.waitForWakeup != nil {
		waitForWakeupRequested, waitForWakeupReason = r.waitForWakeup.Consume()
	}
	return RunResult{
		Output:                 output,
		EpisodeID:              episodeID,
		Metrics:                metrics,
		WaitForWakeupRequested: waitForWakeupRequested,
		WaitForWakeupReason:    waitForWakeupReason,
		SleepRequested:         waitForWakeupRequested,
		SleepReason:            waitForWakeupReason,
	}, nil
}

func (r *Runtime) getSystemPrompt() string {
	profile := r.buildAgentProfile(r.skills, r.availableTools())
	return profile.SystemPrompt
}

// sessionChunkWriter builds the writer that persists a compacted conversation
// span as a searchable chunk. Compaction is the only producer of chunks, so both
// the threshold and overflow-recovery paths share this one construction.
func (r *Runtime) sessionChunkWriter() *SessionChunkWriter {
	if r == nil {
		return nil
	}
	configDir := r.ConfigSnapshot().ConfigDir
	if configDir == "" {
		return nil
	}
	extraction := DefaultMemoryExtractionConfig()
	if r.memories != nil {
		extraction = r.memories.extraction
	}
	return NewSessionChunkWriter(agentpath.ContextManagerSessionFolder(configDir), extraction)
}

func (r *Runtime) getStateHook() contextmanager.AppendMessageHook {
	return func(message messages.Message) contextmanager.AppendMessageHookResult {
		// if not user message, just skip
		if message.Role != messages.MessageRoleUser {
			return contextmanager.AppendMessageHookResult{
				Message: &message,
			}
		}
		var attachment *messages.Attachment
		if !messageHasImageAttachment(message) {
			attachment = r.captureStateScreenshot()
		}
		entries := r.stateManager.GetAllStates()
		// If neither runtime state nor a screenshot is available, just skip.
		if len(entries) == 0 && attachment == nil {
			return contextmanager.AppendMessageHookResult{
				Message: &message,
			}
		}
		// format state entries into a list
		// example:
		// key1: value1
		// key2: value2
		// ...
		var formated strings.Builder
		for _, entry := range entries {
			if strings.TrimSpace(entry.Value) == "" {
				continue
			}
			fmt.Fprintf(&formated, "%s: %s\n", entry.Key, entry.Value)
		}
		if formated.Len() == 0 && attachment == nil {
			return contextmanager.AppendMessageHookResult{
				Message: &message,
			}
		}
		// create a new StateMessage
		stateMessage := messages.Message{
			Role:    messages.MessageRoleState,
			Content: formated.String(),
		}
		if attachment != nil {
			stateMessage.Attachments = []messages.Attachment{*attachment}
		}
		return contextmanager.AppendMessageHookResult{
			Before:  []messages.Message{stateMessage},
			Message: &message,
			After:   []messages.Message{},
		}
	}
}

func messageHasImageAttachment(message messages.Message) bool {
	for _, attachment := range message.Attachments {
		if isImageMIMEType(attachment.MIMEType) {
			return true
		}
	}
	return false
}

func (r *Runtime) captureStateScreenshot() *messages.Attachment {
	if r == nil || r.toolSnapshot() == nil || r.contextManager == nil {
		return nil
	}
	screenshotTool, ok := r.toolSnapshot().Get("screenshot")
	if !ok || screenshotTool == nil {
		return nil
	}

	var output string
	var err error
	if fastTool, ok := screenshotTool.(interface {
		CallWithoutStartupWait(context.Context, string) (string, error)
	}); ok {
		output, err = fastTool.CallWithoutStartupWait(context.Background(), "{}")
	} else {
		output, err = screenshotTool.Call(context.Background(), "{}")
	}
	if err != nil {
		if r.logger != nil {
			r.logger.Debug("[state] screenshot unavailable: %v", err)
		}
		return nil
	}
	observation, ok := parseScreenshotObservation(output)
	if !ok {
		if r.logger != nil {
			r.logger.Debug("[state] screenshot output is not a valid visual observation")
		}
		return nil
	}
	attachment, err := r.contextManager.StoreAttachment(observation.MIMEType, observation.ImageBytes)
	if err != nil {
		if r.logger != nil {
			r.logger.Debug("[state] failed to store screenshot attachment: %v", err)
		}
		return nil
	}
	attachment.Source = messages.AttachmentSourceScreenshotObservation
	return &attachment
}

func (r *Runtime) beginSession(ctx context.Context, req SessionBeginRequest) (SessionBeginResult, error) {
	if r == nil || r.sessionManager == nil {
		return SessionBeginResult{}, nil
	}
	return r.sessionManager.BeginRun(ctx, req)
}

func (r *Runtime) commitSession(ctx context.Context, req SessionCommitRequest) (SessionCommitResult, error) {
	if r == nil || r.sessionManager == nil {
		return SessionCommitResult{}, nil
	}
	return r.sessionManager.CommitRun(ctx, req)
}

func (r *Runtime) currentEnvironmentHints() CurrentEnvironmentHints {
	if r.toolSnapshot() != nil {
		return r.toolSnapshot().CurrentEnvironmentHints(currentEnvironmentHintMaxAge)
	}
	return CurrentEnvironmentHints{}
}

func (r *Runtime) effectiveContextWindow() int {
	if r == nil {
		return 0
	}
	if r.models != nil {
		if spec := r.models.Spec(); spec.ContextWindow > 0 {
			return spec.ContextWindow
		}
	}
	if r.memories != nil {
		return r.memories.effectiveContextWindow()
	}
	return 0
}

func (r *Runtime) ClearMemory(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.resetActiveUserContext()
	userSystemPrompt, hasUserContext, err := r.currentUserContextSystemPrompt()
	if err != nil {
		return err
	}
	if err := contextmanager.ClearAllSessions(agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)); err != nil {
		return fmt.Errorf("clear backend context sessions: %w", err)
	}
	if err := contextmanager.ClearAllSessions(agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)); err != nil {
		return fmt.Errorf("clear user context sessions: %w", err)
	}
	if err := r.rotateContext(); err != nil {
		return fmt.Errorf("create backend context session: %w", err)
	}
	if hasUserContext {
		if err := r.rotateUserContext(userSystemPrompt); err != nil {
			return err
		}
	}
	return nil
}

// RegisterUserContextResetHook installs the active realtime-session reset
// callback. Clearing conversation history cancels that live session so it
// cannot continue writing to the previous user context after rotation.
func (r *Runtime) RegisterUserContextResetHook(hook func()) func() {
	if r == nil {
		return func() {}
	}
	r.userContextResetMu.Lock()
	r.userContextResetGen++
	generation := r.userContextResetGen
	r.userContextResetHook = hook
	r.userContextResetMu.Unlock()
	return func() {
		r.userContextResetMu.Lock()
		if r.userContextResetGen == generation {
			r.userContextResetHook = nil
		}
		r.userContextResetMu.Unlock()
	}
}

func (r *Runtime) resetActiveUserContext() {
	if r == nil {
		return
	}
	r.userContextResetMu.Lock()
	hook := r.userContextResetHook
	r.userContextResetMu.Unlock()
	if hook != nil {
		hook()
	}
}

func (r *Runtime) currentUserContextSystemPrompt() (string, bool, error) {
	if r == nil || strings.TrimSpace(r.ConfigSnapshot().ConfigDir) == "" {
		return "", false, nil
	}
	folder := agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)
	currentSessionPath := filepath.Join(folder, ".current_session")
	if _, err := os.Stat(currentSessionPath); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat current user context session: %w", err)
	}
	manager, err := contextmanager.LoadContextManagerFromCurrentSession(folder)
	if err != nil {
		return "", false, fmt.Errorf("load current user context session: %w", err)
	}
	for _, message := range manager.MessageListDump().Messages {
		if message.Role == messages.MessageRoleSystem {
			return message.Content, true, nil
		}
	}
	return "", false, fmt.Errorf("current user context session has no system prompt")
}

func (r *Runtime) rotateUserContext(systemPrompt string) error {
	if r == nil || strings.TrimSpace(r.ConfigSnapshot().ConfigDir) == "" {
		return nil
	}
	folder := agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)
	if _, err := contextmanager.NewContextManager(folder, systemPrompt); err != nil {
		return fmt.Errorf("create user context session: %w", err)
	}
	return nil
}

func (r *Runtime) MarkSkillsDirty() {
	r.skillsReloadMu.Lock()
	defer r.skillsReloadMu.Unlock()
	r.skillsDirty = true
}

func (r *Runtime) reloadSkillsIfDirty() error {
	r.skillsReloadMu.Lock()
	defer r.skillsReloadMu.Unlock()

	if !r.skillsDirty {
		return nil
	}
	if len(r.ConfigSnapshot().SkillsDirs) == 0 {
		r.skillsDirty = false
		return nil
	}

	index, err := LoadSkillsFromDirs(r.ConfigSnapshot().SkillsDirs)
	if err != nil {
		return fmt.Errorf("reload skills: %w", err)
	}
	r.skills.ReplaceIndex(index)
	r.skillsLoaded = len(index.Names()) > 0
	r.skillsDirty = false
	return nil
}

func (r *Runtime) hasLoadedSkills() bool {
	r.skillsReloadMu.Lock()
	defer r.skillsReloadMu.Unlock()
	return r.skillsLoaded
}

func (r *Runtime) ClearAllMemory(ctx context.Context) error {
	r.resetActiveUserContext()
	if err := r.memories.ClearAll(ctx, "default"); err != nil {
		return err
	}

	if err := contextmanager.ClearAllSessions(agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)); err != nil {
		return fmt.Errorf("clear backend context sessions: %w", err)
	}
	if err := contextmanager.ClearAllSessions(agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)); err != nil {
		return fmt.Errorf("clear user context sessions: %w", err)
	}
	if err := r.rotateContext(); err != nil {
		return fmt.Errorf("create backend context session: %w", err)
	}

	return nil
}

func (r *Runtime) ContextDump() contextmanager.MessageListDump {
	if r == nil {
		return contextmanager.MessageListDump{}
	}
	if r.contextManager != nil {
		return r.contextManager.MessageListDump()
	}
	if strings.TrimSpace(r.ConfigSnapshot().ConfigDir) == "" {
		return contextmanager.MessageListDump{}
	}
	manager, err := contextmanager.LoadContextManagerFromCurrentSession(agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir))
	if err != nil || manager == nil {
		return contextmanager.MessageListDump{}
	}
	return manager.MessageListDump()
}

// UserContextDump returns the persisted realtime foreground context.
func (r *Runtime) UserContextDump() contextmanager.MessageListDump {
	if r == nil {
		return contextmanager.MessageListDump{}
	}
	manager, err := contextmanager.LoadContextManagerFromCurrentSession(agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir))
	if err != nil || manager == nil {
		return contextmanager.MessageListDump{}
	}
	return manager.MessageListDump()
}

// WebContextDump returns the context that the Web UI should display for the
// active input mode.
func (r *Runtime) WebContextDump() contextmanager.MessageListDump {
	if r == nil {
		return contextmanager.MessageListDump{}
	}
	if r.ConfigSnapshot().InputModeOrDefault() == "realtime" {
		return r.UserContextDump()
	}
	return r.ContextDump()
}

// ReadContextAttachment reads an attachment registered in the user or backend
// context. It never accepts an arbitrary filesystem path.
func (r *Runtime) ReadContextAttachment(role, attachmentID string) ([]byte, string, error) {
	if r == nil {
		return nil, "", fmt.Errorf("runtime is unavailable")
	}
	var folder string
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		folder = agentpath.UserContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)
	case "backend":
		folder = agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir)
	default:
		return nil, "", fmt.Errorf("invalid context role")
	}
	manager, err := contextmanager.LoadContextManagerFromCurrentSession(folder)
	if err != nil {
		return nil, "", err
	}
	mimeType := "application/octet-stream"
	for _, message := range manager.MessageListDump().Messages {
		for _, attachment := range message.Attachments {
			if filepath.Base(attachment.FilePath) == filepath.Base(attachmentID) {
				mimeType = attachment.MIMEType
				break
			}
		}
	}
	data, err := manager.ReadAttachment(attachmentID)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

func (r *Runtime) rotateContext() error {
	var err error
	if r.contextManager == nil {
		r.contextManager, err = InitializeContextManager(r.getSystemPrompt(), agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir), []contextmanager.AppendMessageHook{r.getStateHook()})
		if err != nil {
			return err
		}
		return nil
	}
	candidate, err := contextmanager.NewContextManagerCandidate(agentpath.ContextManagerSessionFolder(r.ConfigSnapshot().ConfigDir), r.getSystemPrompt())
	if err != nil {
		return err
	}
	return r.contextManager.Activate(candidate)
}

func (r *Runtime) availableTools() []langtools.Tool {
	if r == nil || r.toolSnapshot() == nil {
		return nil
	}
	tools := NewToolSpecs(r.toolSnapshot().All()).AgentToolsForPlatform(r.devicePlatformFromState())
	return r.filterPhoneBridgeAgentTools(tools)
}

// AvailableTools returns the runtime-filtered tools exposed to the
// conversational agent for the current device platform and bridge state.
// Realtime providers use this same catalog when they execute tool calls
// without the legacy backend agent.
func (r *Runtime) AvailableTools() []langtools.Tool {
	return r.availableTools()
}

// Tool returns a registered runtime tool by name. Realtime voice uses this to
// reuse the same recall_memory implementation and long-term store as the
// legacy agent while exposing its own smaller model-facing tool catalog.
func (r *Runtime) Tool(name string) (langtools.Tool, bool) {
	if r == nil || r.toolSnapshot() == nil {
		return nil, false
	}
	return r.toolSnapshot().Get(name)
}

func (r *Runtime) filterPhoneBridgeAgentTools(tools []langtools.Tool) []langtools.Tool {
	if r == nil || r.toolSnapshot() == nil || r.toolSnapshot().phoneBridge == nil {
		return tools
	}
	bridge := r.toolSnapshot().phoneBridge
	bridge.SetConfiguredPlatform(r.devicePlatformFromState())
	status := bridge.getStatus()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	capabilities := bridge.bleCapabilities(ctx)
	cancel()

	openURLAvailable := phoneBridgeReadyForCommand(status, "open_app") || phoneBridgeCanRestoreFromReturnEntry(status)
	clipboardAvailable := phoneBridgeCommandAvailable(status, "clipboard_read", capabilities.Wake) ||
		phoneBridgeCommandAvailable(status, "clipboard_write", capabilities.Wake)
	calendarAvailable := phoneBridgeCommandAvailable(status, "calendar_create", capabilities.Wake) ||
		phoneBridgeCommandAvailable(status, "calendar_query", capabilities.Wake) ||
		phoneBridgeCommandAvailable(status, "calendar_delete", capabilities.Wake)
	contactsQueryAvailable := phoneBridgeCommandAvailable(status, "contacts_query", capabilities.Wake)
	contactsCreateAvailable := phoneBridgeCommandAvailable(status, "contacts_create", capabilities.Wake)
	contactsUpdateAvailable := phoneBridgeCommandAvailable(status, "contacts_update", capabilities.Wake)
	contactsAvailable := contactsQueryAvailable || contactsCreateAvailable || contactsUpdateAvailable
	notificationSendAvailable := phoneBridgeCommandAvailable(status, "notification_send", capabilities.Wake)
	notificationQueryAvailable := capabilities.NotificationQuery
	notificationAvailable := notificationSendAvailable || notificationQueryAvailable

	filtered := make([]langtools.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		available := true
		switch tool.Name() {
		case toolOpenURL:
			available = openURLAvailable
		case toolBridgeClipboard:
			available = clipboardAvailable
		case toolBridgeCalendar:
			available = calendarAvailable
		case toolBridgeContacts:
			available = contactsAvailable
		case toolBridgeNotification:
			available = notificationAvailable
		}
		if available {
			switch tool.Name() {
			case toolBridgeContacts:
				tool = newContactsCapabilityTool(tool, contactsQueryAvailable, contactsCreateAvailable, contactsUpdateAvailable)
			case toolBridgeNotification:
				tool = newNotificationCapabilityTool(tool, notificationSendAvailable, notificationQueryAvailable)
			}
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func toolNamesFromTools(tools []langtools.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		names = append(names, tool.Name())
	}
	return uniqueNonEmpty(names)
}

func wrapSessionRecallTelemetry(tools []langtools.Tool, counter *atomic.Int64) []langtools.Tool {
	if counter == nil {
		return tools
	}
	wrapped := make([]langtools.Tool, 0, len(tools))
	for _, tool := range tools {
		if tool != nil && tool.Name() == "recall_session_chunks" {
			wrapped = append(wrapped, &sessionRecallTelemetryTool{inner: tool, counter: counter})
			continue
		}
		wrapped = append(wrapped, tool)
	}
	return wrapped
}

type sessionRecallTelemetryTool struct {
	inner   langtools.Tool
	counter *atomic.Int64
}

func (t *sessionRecallTelemetryTool) Name() string {
	return t.inner.Name()
}

func (t *sessionRecallTelemetryTool) Description() string {
	return t.inner.Description()
}

func (t *sessionRecallTelemetryTool) ArgsSchema() map[string]any {
	if structured, ok := t.inner.(structuredInputTool); ok {
		return structured.ArgsSchema()
	}
	return nil
}

func (t *sessionRecallTelemetryTool) Call(ctx context.Context, input string) (string, error) {
	output, err := t.inner.Call(ctx, input)
	if err != nil {
		return "", err
	}
	if t.counter != nil {
		t.counter.Add(int64(countRecalledChunks(output)))
	}
	return output, nil
}

func (t *sessionRecallTelemetryTool) ReturnsVisualObservation() bool {
	if visual, ok := t.inner.(visualObservationTool); ok {
		return visual.ReturnsVisualObservation()
	}
	return false
}

// countRecalledChunks reports how many conversation chunks a recall_session_chunks
// call returned. Chunks only exist for spans already compacted out of the live
// transcript, so a non-zero count means the run consulted history it could no
// longer see directly.
func countRecalledChunks(output string) int {
	var payload struct {
		Results []struct {
			ChunkID string `json:"chunk_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return 0
	}
	return len(payload.Results)
}

type episodeMaintenancePlane interface {
	MemoryPlane
	commitEpisodeTrace(ctx context.Context, episode TaskEpisode) error
	commitEpisodeMaintenance(ctx context.Context, episode TaskEpisode)
}

func (r *Runtime) commitEpisodeBestEffort(recorder *EpisodeRecorder, input string, output string, metrics *RunMetrics, runErr error, promptCapture *telemetryPromptCapture, chunkRecalls *atomic.Int64, asyncMaintenance bool) {
	if recorder == nil || r.memoryPlane == nil {
		return
	}
	cfg := DefaultMemoryExtractionConfig()
	if r.memories != nil {
		cfg = r.memories.extraction
	} else if r.ConfigSnapshot().ConfigDir != "" {
		cfg = LoadMemoryExtractionConfig(r.ConfigSnapshot().ConfigDir)
	}
	tags := cfg.extractTagsFromText(input)
	entities := cfg.extractEntitiesFromText(input)
	episode := recorder.Finish(output, metrics, runErr, tags, entities)
	enrichEpisodeTelemetry(&episode, r.ConfigSnapshot())
	r.enrichEpisodeRuntimeTelemetry(&episode)
	enrichEpisodeChunkRecallTelemetry(&episode, chunkRecalls)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if asyncMaintenance {
		if plane, ok := r.memoryPlane.(episodeMaintenancePlane); ok {
			if err := plane.commitEpisodeTrace(ctx, episode); err != nil && r.logger != nil {
				r.logger.Warn("[memory] commit episode trace failed: %v", err)
				return
			}
			r.exportEpisodeBestEffort(episode, promptCapture)
			maintenanceParentCtx, started := r.episodeMaintenance.begin()
			if !started {
				return
			}
			go func() {
				defer r.episodeMaintenance.done()
				maintenanceCtx, maintenanceCancel := context.WithTimeout(maintenanceParentCtx, 10*time.Second)
				defer maintenanceCancel()
				plane.commitEpisodeMaintenance(maintenanceCtx, episode)
			}()
			return
		}
	}
	if err := r.memoryPlane.CommitEpisode(ctx, episode); err != nil && r.logger != nil {
		r.logger.Warn("[memory] commit episode failed: %v", err)
		return
	}
	r.exportEpisodeBestEffort(episode, promptCapture)
}

func (m *asyncRuntimeWork) begin() (context.Context, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return nil, false
	}
	if m.ctx == nil {
		m.ctx, m.cancel = context.WithCancel(context.Background())
	}
	m.wg.Add(1)
	return m.ctx, true
}

func (m *asyncRuntimeWork) done() {
	m.wg.Done()
}

func (m *asyncRuntimeWork) closeAndWait(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	cancel := m.cancel
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return ctx.Err()
	}
}

// enrichEpisodeChunkRecallTelemetry records how many times the run consulted
// compressed conversation history, so a run that answered from stale context
// can be told apart from one that recalled a chunk.
func enrichEpisodeChunkRecallTelemetry(episode *TaskEpisode, chunkRecalls *atomic.Int64) {
	if episode == nil || chunkRecalls == nil {
		return
	}
	if episode.Extra == nil {
		episode.Extra = map[string]interface{}{}
	}
	episode.Extra["session_chunks_recalled"] = chunkRecalls.Load()
}

func (r *Runtime) enrichEpisodeRuntimeTelemetry(episode *TaskEpisode) {
	if r == nil || episode == nil {
		return
	}
	if episode.Extra == nil {
		episode.Extra = map[string]interface{}{}
	}
	if extraString(episode.Extra, "runtime_id") == "" && r.runtimeID != "" {
		episode.Extra["runtime_id"] = r.runtimeID
	}
	contextWindow := r.effectiveContextWindow()
	if contextWindow > 0 {
		episode.Extra["context_window"] = contextWindow
	}
	params, _ := episode.Extra["model_parameters"].(map[string]interface{})
	if len(params) == 0 {
		params = telemetryModelParametersFromModelConfig(r.ConfigSnapshot().Model)
	}
	if contextWindow > 0 {
		if params == nil {
			params = map[string]interface{}{}
		}
		params["context_window"] = contextWindow
	}
	if len(params) > 0 {
		episode.Extra["model_parameters"] = params
	}
}

func telemetryModelParametersFromModelConfig(cfg ModelConfig) map[string]interface{} {
	params := map[string]interface{}{}
	if cfg.Temperature != nil {
		params["temperature"] = *cfg.Temperature
	}
	if cfg.MaxResponseTokens > 0 {
		params["max_response_tokens"] = cfg.MaxResponseTokens
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

func (r *Runtime) exportEpisodeBestEffort(episode TaskEpisode, promptCapture *telemetryPromptCapture) {
	if !r.ConfigSnapshot().Telemetry.EnabledOrDefault() || strings.TrimSpace(episode.UserGoal) == "" {
		return
	}
	if r.ConfigSnapshot().ConfigDir == "" {
		return
	}
	exporter := NewEpisodeExporter(r.ConfigSnapshot().Telemetry, r.logger)
	promptCalls := promptCapture.Snapshot()
	episodesRoot := filepath.Join(r.ConfigSnapshot().ConfigDir, "memory", "episodes")
	episodeDir := EpisodeDirectory(episodesRoot, episode)
	timeout := r.ConfigSnapshot().Telemetry.UploadTimeoutOrDefault()
	parent := context.Background()
	drainOnClose := r.ConfigSnapshot().Telemetry.EnvironmentOrDefault() == "benchmark"
	if drainOnClose {
		var started bool
		parent, started = r.telemetryExports.begin()
		if !started {
			return
		}
	}
	go func() {
		if drainOnClose {
			defer r.telemetryExports.done()
		}
		ctx, cancel := context.WithTimeout(parent, timeout)
		defer cancel()
		if err := exporter.ExportEpisodeDir(ctx, episodeDir, episode, promptCalls); err != nil {
			if r.logger != nil {
				r.logger.Warn("[telemetry] export episode %s failed: %v", episode.ID, err)
			}
		} else if drainOnClose && r.logger != nil {
			r.logger.Info("[telemetry] exported episode %s trace %s", episode.ID, telemetryTraceID(episode.ID))
		}
	}()
}

func (r *Runtime) buildAgentProfile(skills *SkillManager, availableTools []langtools.Tool) RoleProfile {
	return buildProfile(
		AgentConfig{
			Instruction: r.ConfigSnapshot().Instruction,
			Prompt:      r.ConfigSnapshot().Prompt,
			Locale:      r.ConfigSnapshot().LocaleOrDefault(),
			Timezone:    r.ConfigSnapshot().TimezoneOrDefault(),
		},
		skills,
		availableTools,
		agentRoleRules(),
	)
}

// runtimeCallbackHandler implements callbacks.Handler for streaming output and
// tool/agent observability.
type runtimeCallbackHandler struct {
	writer           io.Writer
	metrics          *RunMetrics
	startTime        time.Time
	firstTokenSeen   bool
	streamTokenSeen  bool
	streamLogEmitted bool
	streamErr        error
	logger           *Logger
	eventHandler     func(RunEvent)
	episodeID        string
	runtimeID        string
	requestID        string
	runID            string
	episode          *EpisodeRecorder
	mu               sync.Mutex
	pendingActions   []schema.AgentAction
	reasoningContent strings.Builder
}

func (h *runtimeCallbackHandler) HandleText(ctx context.Context, text string) {
	if h.writer != nil {
		h.writer.Write([]byte(text))
	}
}

func (h *runtimeCallbackHandler) HandleLLMStart(ctx context.Context, prompts []string) {}

func (h *runtimeCallbackHandler) HandleLLMGenerateContentStart(ctx context.Context, ms []llms.MessageContent) {
}

func (h *runtimeCallbackHandler) HandleLLMGenerateContentEnd(ctx context.Context, res *llms.ContentResponse) {
	recordUsageMetrics(h.metrics, res)
}

func recordUsageMetrics(metrics *RunMetrics, res *llms.ContentResponse) {
	if res == nil || metrics == nil || len(res.Choices) == 0 {
		return
	}
	info := res.Choices[0].GenerationInfo
	if info == nil {
		return
	}

	// A single run makes several LLM calls (planner, executor, verifier, and any
	// tool-assisted iterations). Accumulate token counts across calls so the run
	// metrics reflect the whole run rather than only the last call. LastPromptTokens
	// keeps the largest single-call prompt size for the compression heuristic.
	if v, ok := util.UsageMetricInt(info["prompt_tokens"]); ok {
		metrics.PromptTokens += v
		if v > metrics.LastPromptTokens {
			metrics.LastPromptTokens = v
		}
	}
	if v, ok := util.UsageMetricInt(info["completion_tokens"]); ok {
		metrics.CompletionTokens += v
	}
	if v, ok := util.UsageMetricInt(info["total_tokens"]); ok {
		metrics.TotalTokens += v
	}
	if v, ok := util.UsageMetricInt(info["cached_tokens"]); ok {
		metrics.CachedPromptTokens += v
	}
	if v, ok := util.UsageMetricInt(info["reasoning_tokens"]); ok {
		metrics.ReasoningTokens += v
	}
}

func (h *runtimeCallbackHandler) HandleLLMError(ctx context.Context, err error) {}

func (h *runtimeCallbackHandler) HandleChainStart(ctx context.Context, inputs map[string]any) {}

func (h *runtimeCallbackHandler) HandleChainEnd(ctx context.Context, outputs map[string]any) {}

func (h *runtimeCallbackHandler) HandleChainError(ctx context.Context, err error) {}

func (h *runtimeCallbackHandler) HandleToolStart(ctx context.Context, input string) {}

// emitRunEvent forwards a run event to the subscribed handler. Run events are
// live UI/telemetry signals; the durable record of a run is the Episode trace
// plus the ContextManager transcript.
func (h *runtimeCallbackHandler) emitRunEvent(event RunEvent) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.EpisodeID == "" {
		event.EpisodeID = h.episodeID
	}
	if h.eventHandler != nil {
		h.eventHandler(event)
	}
}

func (h *runtimeCallbackHandler) HandleAssistantOutput(ctx context.Context, content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	reasoning := h.ReasoningContent()
	if h.logger != nil {
		h.logger.Info("Assistant output: %s", truncateForLog(content, 1000))
	}
	h.emitRunEvent(RunEvent{
		Type:             "assistant_output",
		Role:             "assistant",
		EpisodeID:        h.episodeID,
		Content:          content,
		ReasoningContent: reasoning,
		Timestamp:        time.Now(),
	})
}

func (h *runtimeCallbackHandler) HandleToolEnd(ctx context.Context, output string) {
	if h.logger != nil {
		h.logger.Info("Tool result: %s", truncateForLog(output, 240))
	}
	action, ok := h.popPendingAction()
	if ok {
		h.emitRunEvent(RunEvent{
			Type:       "tool_result",
			EpisodeID:  h.episodeID,
			ToolCallID: action.ToolID,
			ToolName:   action.Tool,
			ToolInput:  normalizeToolInput(action.ToolInput),
			Content:    output,
			Timestamp:  time.Now(),
		})
	}
}

func (h *runtimeCallbackHandler) HandleToolError(ctx context.Context, err error) {
	if h.logger != nil {
		h.logger.Error("Tool error: %v", err)
	}
	action, ok := h.popPendingAction()
	if ok {
		var toolErr *ToolError
		if !errors.As(err, &toolErr) {
			toolErr = NewToolError(CodeToolExecutionFailed, err.Error())
		}
		content := toolErr.Message
		h.emitRunEvent(RunEvent{
			Type:       "tool_result",
			EpisodeID:  h.episodeID,
			ToolCallID: action.ToolID,
			ToolName:   action.Tool,
			ToolInput:  normalizeToolInput(action.ToolInput),
			Content:    content,
			Timestamp:  time.Now(),
			IsError:    true,
			ToolError:  cloneToolError(toolErr),
		})
	}
}

func (h *runtimeCallbackHandler) HandleNamedToolStart(ctx context.Context, name, input string) {}

func (h *runtimeCallbackHandler) HandleNamedToolEnd(ctx context.Context, name, input, output string) {
	input = normalizeToolInput(input)
	if h.logger != nil {
		h.logger.Info("Tool result: name=%s output=%s", name, truncateForLog(output, 240))
	}
	h.removePendingAction(name, input)
	h.emitRunEvent(RunEvent{
		Type:      "tool_result",
		EpisodeID: h.episodeID,
		ToolName:  name,
		ToolInput: input,
		Content:   output,
		Timestamp: time.Now(),
	})
}

func (h *runtimeCallbackHandler) HandleNamedToolError(ctx context.Context, name, input string, err error) {
	input = normalizeToolInput(input)
	if h.logger != nil {
		h.logger.Error("Tool error: name=%s err=%v", name, err)
	}
	var toolErr *ToolError
	if !errors.As(err, &toolErr) {
		toolErr = NewToolError(CodeToolExecutionFailed, err.Error())
	}
	content := toolErr.Message
	h.removePendingAction(name, input)
	h.emitRunEvent(RunEvent{
		Type:      "tool_result",
		EpisodeID: h.episodeID,
		ToolName:  name,
		ToolInput: input,
		Content:   content,
		Timestamp: time.Now(),
		IsError:   true,
		ToolError: cloneToolError(toolErr),
	})
}

func (h *runtimeCallbackHandler) HandleToolCallStart(ctx context.Context, call ToolCall) {
	content := strings.TrimSpace(call.Content)
	if h.logger != nil {
		if content != "" {
			h.logger.Info("Tool call: name=%s input=%s content=%s",
				call.Spec.Name, truncateForLog(call.Input, 240), truncateForLog(content, 240))
		} else {
			h.logger.Info("Tool call: name=%s input=%s",
				call.Spec.Name, truncateForLog(call.Input, 240))
		}
	}
	h.emitRunEvent(RunEvent{
		Type:       runEventToolCall,
		EpisodeID:  h.episodeID,
		ToolCallID: call.Action.ToolID,
		ToolName:   call.Spec.Name,
		ToolInput:  call.Input,
		Content:    content,
		Timestamp:  time.Now(),
	})
}

func (h *runtimeCallbackHandler) HandleToolProgress(ctx context.Context, call ToolCall, progress ToolProgress) {
	if h == nil {
		return
	}
	h.emitRunEvent(RunEvent{
		Type:       runEventToolProgress,
		EpisodeID:  h.episodeID,
		ToolCallID: call.Action.ToolID,
		ToolName:   call.Spec.Name,
		ToolInput:  call.Input,
		ToolStatus: progress.Status,
		Content:    progress.Content,
		Timestamp:  time.Now(),
	})
}

func (h *runtimeCallbackHandler) BeforeToolCall(ctx context.Context, call ToolCall) (ToolResult, bool) {
	return DefaultBeforeToolCall(ctx, call)
}

func (h *runtimeCallbackHandler) AfterToolCall(ctx context.Context, call ToolCall, result ToolResult) ToolResult {
	return DefaultAfterToolCall(ctx, call, result)
}

func (h *runtimeCallbackHandler) HandleToolCallResult(ctx context.Context, call ToolCall, result ToolResult) {
	output := publicToolResultContent(result)
	logOutput := result.EventOutput()
	if h.logger != nil {
		if result.IsError() {
			h.logger.Error("Tool result: name=%s output=%s", call.Spec.Name, truncateForLog(logOutput, 240))
		} else {
			h.logger.Info("Tool result: name=%s output=%s", call.Spec.Name, truncateForLog(logOutput, 240))
		}
	}
	h.emitRunEvent(RunEvent{
		Type:       "tool_result",
		EpisodeID:  h.episodeID,
		ToolCallID: call.Action.ToolID,
		ToolName:   call.Spec.Name,
		ToolInput:  call.Input,
		Content:    output,
		Timestamp:  time.Now(),
		IsError:    result.IsError(),
		ToolError:  cloneToolError(result.Error),
	})
}

func publicToolResultContent(result ToolResult) string {
	originalChars := utf8.RuneCountInString(result.Output)
	if result.SummaryTruncated && originalChars > maxPublicToolResultRunes {
		return fmt.Sprintf("[Large tool result omitted from public history (%d chars)]", originalChars)
	}
	return result.EventOutput()
}

func (h *runtimeCallbackHandler) HandleAgentAction(ctx context.Context, action schema.AgentAction) {
	content := toolContentFromAction(action)
	if h.logger != nil {
		if content != "" {
			h.logger.Info("Tool call: name=%s input=%s content=%s",
				action.Tool, truncateForLog(action.ToolInput, 240), truncateForLog(content, 240))
		} else {
			h.logger.Info("Tool call: name=%s input=%s",
				action.Tool, truncateForLog(action.ToolInput, 240))
		}
	}
	h.pushPendingAction(action)
	h.emitRunEvent(RunEvent{
		Type:       runEventToolCall,
		EpisodeID:  h.episodeID,
		ToolCallID: action.ToolID,
		ToolName:   action.Tool,
		ToolInput:  action.ToolInput,
		Content:    content,
		Timestamp:  time.Now(),
	})
}

func (h *runtimeCallbackHandler) HandleAgentFinish(ctx context.Context, finish schema.AgentFinish) {}

func (h *runtimeCallbackHandler) HandleRoleOutput(ctx context.Context, role, content, reasoning string) {
	content = strings.TrimSpace(content)
	reasoning = strings.TrimSpace(reasoning)
	if h.logger != nil {
		debugText := content
		if reasoning != "" {
			debugText = reasoning + "\n" + content
		}
		h.logger.Info("Role output: role=%s content=%s", role, truncateForLog(debugText, 1000))
	}
	if reasoning != "" {
		h.setReasoningContent(reasoning)
	}
	h.emitRunEvent(RunEvent{
		Type:             "role_output",
		Role:             role,
		EpisodeID:        h.episodeID,
		Content:          content,
		ReasoningContent: reasoning,
		Timestamp:        time.Now(),
	})
}

func (h *runtimeCallbackHandler) HandleStreamingReasoning(ctx context.Context, chunk []byte) {
	if h == nil || len(chunk) == 0 {
		return
	}
	h.mu.Lock()
	h.reasoningContent.Write(chunk)
	h.mu.Unlock()
	h.emitRunEvent(RunEvent{
		Type:      runEventReasoningDelta,
		Role:      "assistant",
		EpisodeID: h.episodeID,
		// Delta events carry only the newly received chunk. The accumulated
		// reasoning is retained in reasoningContent for the final assistant event.
		ReasoningContent: string(chunk),
		Timestamp:        time.Now(),
	})
}

func (h *runtimeCallbackHandler) StreamingReasoningEnabled() bool {
	return h != nil && h.eventHandler != nil
}

func (h *runtimeCallbackHandler) ResetStreamingReasoning(ctx context.Context) {
	if h == nil {
		return
	}
	h.mu.Lock()
	hadReasoning := h.reasoningContent.Len() > 0
	h.reasoningContent.Reset()
	h.mu.Unlock()
	if hadReasoning {
		h.emitRunEvent(RunEvent{
			Type:      runEventReasoningReset,
			EpisodeID: h.episodeID,
			Timestamp: time.Now(),
		})
	}
}

func (h *runtimeCallbackHandler) ReasoningContent() string {
	if h == nil {
		return ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.TrimSpace(h.reasoningContent.String())
}

func (h *runtimeCallbackHandler) setReasoningContent(reasoning string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.reasoningContent.Reset()
	h.reasoningContent.WriteString(reasoning)
	h.mu.Unlock()
}

func (h *runtimeCallbackHandler) HandleSteerMessage(ctx context.Context, steer RunSteerMessage) {
	if steer.Timestamp.IsZero() {
		steer.Timestamp = time.Now()
	}
	if h.episode != nil {
		h.episode.RecordEvent(TaskEpisodeEvent{
			Type:    "steer",
			Role:    "user",
			Content: steer.Content,
			Ts:      steer.Timestamp.UTC().Format(time.RFC3339Nano),
		})
	}
	h.emitRunEvent(RunEvent{
		Type:      "steer",
		EpisodeID: h.episodeID,
		Content:   steer.Content,
		Timestamp: steer.Timestamp,
	})
}

func (h *runtimeCallbackHandler) HandleRetrieverStart(ctx context.Context, query string) {}

func (h *runtimeCallbackHandler) HandleRetrieverEnd(ctx context.Context, query string, documents []schema.Document) {
}

func (h *runtimeCallbackHandler) HandleStreamingFunc(ctx context.Context, chunk []byte) {
	if h.writer != nil && !h.streamingFailed() {
		if _, err := h.writer.Write(chunk); err != nil {
			h.recordStreamError(err)
		} else if streamWriterEmitted(h.writer) {
			if h.recordStreamToken() {
				h.logStreamEmitted(ctx, chunk)
			}
		}
	}

	// Record first token time
	h.recordFirstToken()
}

func (h *runtimeCallbackHandler) ResetStreaming(ctx context.Context) {
	resetStreamWriterState(h.writer)
	// Drop any text the previous turn streamed that the writer buffered but
	// never emitted, so residual content cannot leak into this turn.
	resetStreamBuffer(h.writer)
	h.mu.Lock()
	h.streamTokenSeen = false
	h.streamLogEmitted = false
	h.streamErr = nil
	h.mu.Unlock()
}

type streamStateResetter interface {
	ResetStreamState()
}

type streamOutputTracker interface {
	StreamEmitted() bool
}

type streamResponseFinisher interface {
	FinishResponse() bool
}

func resetStreamWriterState(writer io.Writer) {
	resetter, ok := writer.(streamStateResetter)
	if !ok {
		return
	}
	resetter.ResetStreamState()
}

func resetStreamBuffer(writer io.Writer) {
	if resetter, ok := writer.(ttsBufferResetter); ok {
		resetter.ResetBuffer()
	}
}

func finishStreamWriterResponse(writer io.Writer) bool {
	finisher, ok := writer.(streamResponseFinisher)
	if !ok {
		return false
	}
	return finisher.FinishResponse()
}

func streamWriterEmitted(writer io.Writer) bool {
	tracker, ok := writer.(streamOutputTracker)
	if !ok {
		return true
	}
	return tracker.StreamEmitted()
}

func (h *runtimeCallbackHandler) FinishStreamingResponse(context.Context) {
	finishStreamWriterResponse(h.writer)
}

func (h *runtimeCallbackHandler) AbortStreamingResponse(context.Context) {
	resetStreamBuffer(h.writer)
	resetStreamWriterState(h.writer)
}

func (h *runtimeCallbackHandler) recordFirstToken() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.firstTokenSeen {
		return
	}
	h.firstTokenSeen = true
	if h.metrics != nil {
		h.metrics.FirstTokenTime = float64(time.Since(h.startTime).Milliseconds())
	}
}

func (h *runtimeCallbackHandler) recordStreamToken() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streamErr != nil {
		return false
	}
	first := !h.streamTokenSeen
	h.streamTokenSeen = true
	return first
}

func (h *runtimeCallbackHandler) streamingFailed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streamErr != nil
}

func (h *runtimeCallbackHandler) recordStreamError(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	firstErr := h.streamErr == nil
	if firstErr {
		h.streamErr = err
	}
	h.streamTokenSeen = false
	h.mu.Unlock()
	if firstErr && h.logger != nil {
		h.logger.Warn("Stream writer failed; continuing without streaming output: %v", err)
	}
}

func (h *runtimeCallbackHandler) logStreamEmitted(ctx context.Context, chunk []byte) {
	if h == nil || h.logger == nil {
		return
	}
	h.mu.Lock()
	if h.streamLogEmitted {
		h.mu.Unlock()
		return
	}
	h.streamLogEmitted = true
	requestID := h.requestID
	runID := h.runID
	h.mu.Unlock()

	role := telemetryRoleFromContext(ctx)
	if role == "" {
		role = "post_parse"
	}
	h.logger.Info("Stream emitted: role=%s chunk_len=%d chunk=%s request_id=%s run_id=%s",
		role, len(chunk), truncateForLog(string(chunk), 120), requestID, runID)
}

var _ callbacks.Handler = (*runtimeCallbackHandler)(nil)

func (h *runtimeCallbackHandler) pushPendingAction(action schema.AgentAction) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pendingActions = append(h.pendingActions, action)
}

func (h *runtimeCallbackHandler) popPendingAction() (schema.AgentAction, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pendingActions) == 0 {
		return schema.AgentAction{}, false
	}
	action := h.pendingActions[0]
	h.pendingActions = h.pendingActions[1:]
	return action, true
}

func (h *runtimeCallbackHandler) removePendingAction(name, input string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	normalizedInput := normalizeToolInput(input)
	for i, action := range h.pendingActions {
		if strings.EqualFold(action.Tool, name) && normalizeToolInput(action.ToolInput) == normalizedInput {
			h.pendingActions = append(h.pendingActions[:i], h.pendingActions[i+1:]...)
			return
		}
	}
}

func normalizeToolInput(input string) string {
	return strings.TrimSuffix(input, "\nObservation:")
}

func truncateForLog(text string, max int) string {
	if max <= 0 || text == "" {
		return text
	}
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "..."
}

func buildLLMProfileFn(models model.Model) ProfileFn {
	return func(ctx context.Context, entries []ProfileEntry) string {
		var input strings.Builder
		for _, e := range entries {
			input.WriteString(fmt.Sprintf("[%s] %s\n", e.Type, e.Content))
		}
		if input.Len() == 0 {
			return ""
		}
		prompt := `Based on the following memory entries about a user, synthesize a concise user profile.
Rules:
- Only include information directly about the user (identity, role, preferences, habits, rules they set).
- Discard transient facts, one-time events, or information not useful for future interactions.
- Group related information under clear headings.
- Keep it concise — no more than 10 lines total.
- Write in the same language as the entries.
- Output markdown starting with "# User Profile".

Memory entries:
` + input.String()
		result, err := llms.GenerateFromSinglePrompt(ctx, models, prompt, llms.WithMaxTokens(400))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(result)
	}
}

// Close releases resources held by the runtime
func (r *Runtime) Close() error {
	r.StopConfigReloads()
	if r.phoneBridge != nil {
		r.phoneBridge.Close()
	}
	maintenanceCtx, maintenanceCancel := context.WithTimeout(context.Background(), runtimeEpisodeMaintenanceTimeout)
	if err := r.episodeMaintenance.closeAndWait(maintenanceCtx); err != nil && r.logger != nil {
		r.logger.Error("episode maintenance drain on close: %v", err)
	}
	maintenanceCancel()
	// Benchmark workers exit immediately after each attempt, so retain their
	// execution evidence before container teardown. Ordinary runtimes keep the
	// existing best-effort export and non-blocking telemetry shutdown behavior.
	if r.ConfigSnapshot().Telemetry.EnvironmentOrDefault() == "benchmark" {
		telemetryCtx, telemetryCancel := context.WithTimeout(context.Background(), runtimeTelemetryDrainTimeout)
		if err := r.telemetryExports.closeAndWait(telemetryCtx); err != nil && r.logger != nil {
			r.logger.Warn("[telemetry] drain on close: %v", err)
		}
		telemetryCancel()
	}
	if plane, ok := r.memoryPlane.(*FilesystemMemoryPlane); ok {
		plane.StopEpisodeMemory()
		plane.StopNotificationMemory()
	}
	if r.storageMonitor != nil {
		r.storageMonitor.Stop()
	}
	if r.storage != nil {
		r.storage.Stop()
	}
	if r.mergeWorker != nil {
		r.mergeWorker.Stop()
	}
	if r.profileDebouncer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := r.profileDebouncer.Flush(ctx); err != nil && r.logger != nil {
			r.logger.Error("profile debouncer flush on close: %v", err)
		}
		cancel()
	}
	if r.ttsManager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), runtimeTTSCloseTimeout)
		if err := r.ttsManager.CloseContext(ctx); err != nil && r.logger != nil {
			r.logger.Warn("close TTS provider: %v", err)
		}
		cancel()
	}
	if r.logger != nil {
		r.logger.Info("Shutting down agent runtime")
		return r.logger.Close()
	}
	return nil
}

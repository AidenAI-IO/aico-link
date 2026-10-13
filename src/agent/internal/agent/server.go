package agent

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/screenprovider"
	"aiden-agent/internal/agent/speech"
	"aiden-agent/internal/agent/tts"
	_ "aiden-agent/internal/agent/tts/adapters/alicloud"
	_ "aiden-agent/internal/agent/tts/adapters/fishaudio"
	_ "aiden-agent/internal/agent/tts/adapters/google_cloud"
	_ "aiden-agent/internal/agent/tts/adapters/minimax"
	_ "aiden-agent/internal/agent/tts/adapters/openrouter"
	_ "aiden-agent/internal/agent/tts/adapters/volcengine"
	"aiden-agent/internal/ble"
)

const (
	agentHTTPReadHeaderTimeout            = 10 * time.Second
	agentHTTPReadTimeout                  = 30 * time.Second
	agentHTTPIdleTimeout                  = 120 * time.Second
	agentHTTPShutdownTimeout              = 5 * time.Second
	benchmarkSeedSessionChunkMaxBodyBytes = 512 * 1024
)

// Server provides HTTP API for agent interactions
type Server struct {
	configDepsMu            sync.RWMutex
	closeOnce               sync.Once
	closing                 atomic.Bool
	httpMu                  sync.Mutex
	httpServer              *http.Server
	httpListener            net.Listener
	runtime                 *Runtime
	addr                    string
	logger                  *Logger
	userFilesReportPath     string
	userFilesToolsDir       string
	userFilesMemoryDir      string
	userFilesSkillsDir      string
	userFilesSkillStateDir  string
	userFilesGenerateMu     sync.Mutex
	userFilesGeneration     *userFilesReportGeneration
	mu                      sync.Mutex
	episodeStore            *TaskEpisodeStore
	sttClient               STTClient
	ttsManager              *tts.ProviderManager // Borrowed from Runtime.
	audioClient             *AudioServiceClient
	ttsPlaybackBackend      tts.AudioServiceBackend
	screenCaptureMu         sync.Mutex
	screenCaptureClient     screenprovider.Provider
	quickCapture            *QuickCaptureController
	recordMu                sync.Mutex
	sttConfigTestSession    *sttConfigTestLiveSession
	bridge                  *PhoneBridge
	bleSocketPath           string
	bleStatusRequest        func(context.Context, string) (ble.RuntimeStatus, error)
	blePairingStartRequest  func(context.Context, string) (ble.RuntimeStatus, error)
	blePairingForgetRequest func(context.Context, string) (ble.ForgetResult, error)
	bleDisconnectRequest    func(context.Context, string) (ble.RuntimeStatus, error)
	bleNotifyRequest        func(context.Context, string, string, []ble.NotificationEvent) (ble.NotificationPublishResult, error)
	bleWakeRequest          func(context.Context, string) error
	androidADB              androidADBController
	liveActivity            *LiveActivityManager
	pendingResults          map[string]*chatPendingResult
	pendingResultsMu        sync.Mutex
	activeRuns              map[string]context.CancelFunc
	activeRunsMu            sync.Mutex
	terminatedRequests      map[string]struct{}
	terminatedRequestsMu    sync.RWMutex
	activeOutputs           map[string]map[*activeTTSOutput]struct{}
	activeOutputsMu         sync.Mutex
	pendingSteers           map[string]pendingSteerMessage
	steerSignals            map[string]chan struct{}
	steersMu                sync.Mutex
	eventBroadcaster        *EventBroadcaster
	storageMonitor          *StorageMonitor
	realtimeChatMu          sync.RWMutex
	realtimeChatHandler     RealtimeChatHandler
}

const maxChatImageAttachments = 4

type MessageAttachment struct {
	Kind       string `json:"kind"`
	Name       string `json:"name,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	Path       string `json:"path,omitempty"`
	Data       string `json:"data,omitempty"`
	Size       int    `json:"size,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	PreviewURL string `json:"preview_url,omitempty"`
}

// Message represents a public chat or tool event.
type Message struct {
	Type             string              `json:"type"` // "user", "assistant", "tool_call", "tool_result"
	Role             string              `json:"role,omitempty"`
	EpisodeID        string              `json:"episode_id,omitempty"`
	RequestID        string              `json:"request_id,omitempty"`
	Status           string              `json:"status,omitempty"`
	Modality         string              `json:"modality,omitempty"`
	OriginalText     string              `json:"original_text,omitempty"`
	Transcript       string              `json:"transcript,omitempty"`
	Content          string              `json:"content"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Usage            *messages.Usage     `json:"usage,omitempty"`
	SpeechEligible   bool                `json:"speech_eligible,omitempty"`
	ToolName         string              `json:"tool_name,omitempty"`
	ToolInput        string              `json:"tool_input,omitempty"`
	ToolError        *ToolError          `json:"tool_error,omitempty"`
	Attachments      []MessageAttachment `json:"attachments,omitempty"`
	Artifacts        []InputArtifact     `json:"artifacts,omitempty"`
	Source           string              `json:"source,omitempty"`
	AudioFile        string              `json:"audio_file,omitempty"`
	AudioDurationMs  int64               `json:"audio_duration_ms,omitempty"`
	Timestamp        time.Time           `json:"timestamp"`
	IsError          bool                `json:"is_error,omitempty"`
}

func normalizePublicMessage(message Message) (Message, bool) {
	switch strings.TrimSpace(message.Type) {
	case "user", "user_input", "steer":
		message.Type = "user"
		message.Role = ""
	case "assistant", "assistant_output":
		message.Type = "assistant"
		if role := strings.TrimSpace(message.Role); role == "assistant" || role == "ai" {
			message.Role = ""
		}
	case runEventToolCall:
		message.Type = runEventToolCall
	case "tool_result":
		message.Type = "tool_result"
	default:
		return Message{}, false
	}
	return message, true
}

func messageFromRunEvent(event RunEvent, fallbackEpisodeID string, requestID string) Message {
	episodeID := event.EpisodeID
	if episodeID == "" {
		episodeID = fallbackEpisodeID
	}
	message := Message{
		Type:             event.Type,
		Role:             event.Role,
		EpisodeID:        episodeID,
		RequestID:        requestID,
		Content:          event.Content,
		ReasoningContent: event.ReasoningContent,
		SpeechEligible:   event.SpeechEligible,
		ToolName:         event.ToolName,
		ToolInput:        event.ToolInput,
		ToolError:        cloneToolError(event.ToolError),
		Timestamp:        event.Timestamp,
		IsError:          event.IsError,
	}
	message, ok := normalizePublicMessage(message)
	if !ok {
		return Message{}
	}
	return message
}

func messageFromReasoningEvent(event RunEvent, fallbackEpisodeID, requestID string) Message {
	episodeID := event.EpisodeID
	if episodeID == "" {
		episodeID = fallbackEpisodeID
	}
	return Message{
		Type:             "assistant",
		Status:           "streaming",
		EpisodeID:        episodeID,
		RequestID:        requestID,
		ReasoningContent: event.ReasoningContent,
		Timestamp:        event.Timestamp,
	}
}

// pollingMessageFromReasoningEvent preserves reset boundaries in the polling
// payload while voice SSE continues to use a normal streaming assistant message.
func pollingMessageFromReasoningEvent(event RunEvent, fallbackEpisodeID, requestID string) Message {
	message := messageFromReasoningEvent(event, fallbackEpisodeID, requestID)
	message.Type = map[string]string{
		runEventReasoningDelta: "assistant_reasoning_delta",
		runEventReasoningReset: "assistant_reasoning_reset",
	}[event.Type]
	return message
}

func messageFromTurnInput(input TurnInput, episodeID, requestID string, attachments []MessageAttachment, timestamp time.Time) Message {
	input = normalizeTurnInput(input)
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	message := Message{
		Type:         "user",
		EpisodeID:    episodeID,
		RequestID:    requestID,
		Modality:     input.Modality,
		OriginalText: input.OriginalText,
		Transcript:   input.Transcript,
		Content:      input.InputText,
		Attachments:  mergeTurnMessageAttachments(input, attachments),
		Artifacts:    sanitizeInputArtifacts(input.Artifacts),
		Source:       input.Source,
		Timestamp:    timestamp,
	}
	if artifact, ok := firstAudioArtifact(input); ok {
		message.AudioFile = artifact.Path
		message.AudioDurationMs = artifact.DurationMS
	}
	return message
}

func mergeTurnMessageAttachments(input TurnInput, attachments []MessageAttachment) []MessageAttachment {
	merged := make([]MessageAttachment, 0, len(attachments)+len(input.Artifacts))
	merged = append(merged, attachments...)
	if input.Transcript != "" {
		for i := range merged {
			if merged[i].Kind == AttachmentKindAudio && merged[i].Transcript == "" {
				merged[i].Transcript = input.Transcript
			}
		}
	}
	for _, artifact := range sanitizeInputArtifacts(input.Artifacts) {
		if artifact.Kind == "" {
			continue
		}
		if messageAttachmentsContainArtifact(merged, artifact) {
			continue
		}
		merged = append(merged, MessageAttachment{
			Kind:       artifact.Kind,
			Name:       artifact.Name,
			MIMEType:   artifact.MIMEType,
			Path:       artifact.Path,
			Size:       int(artifact.Size),
			DurationMS: artifact.DurationMS,
			Transcript: input.Transcript,
		})
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func messageAttachmentsContainArtifact(attachments []MessageAttachment, artifact InputArtifact) bool {
	for _, attachment := range attachments {
		if artifact.Path != "" && attachment.Path == artifact.Path {
			return true
		}
		if attachment.Kind == artifact.Kind && attachment.Name == artifact.Name && attachment.MIMEType == artifact.MIMEType && attachment.Size == int(artifact.Size) {
			return true
		}
	}
	return false
}

// ChatRequest represents an incoming chat request
type ChatRequest struct {
	Message     string              `json:"message"`
	Attachments []MessageAttachment `json:"attachments,omitempty"`
	RequestID   string              `json:"request_id,omitempty"`
	PhoneID     string              `json:"phone_id,omitempty"`
}

// RealtimeChatRequest is the text-only request forwarded to an active
// realtime voice session.
type RealtimeChatRequest struct {
	RequestID string
	Message   string
}

// RealtimeChatEvent is emitted by a realtime chat handler. Delta events are
// streamed to clients; done contains the complete response; error terminates
// the request with an error message.
type RealtimeChatEvent struct {
	Type             string
	Delta            string
	Response         string
	ReasoningContent string
	Error            string
}

const (
	RealtimeChatEventDelta = "delta"
	RealtimeChatEventDone  = "done"
	RealtimeChatEventError = "error"
)

// RealtimeChatHandler submits text to the currently active realtime session.
// The returned channel is closed after a done or error event.
type RealtimeChatHandler func(context.Context, RealtimeChatRequest) (<-chan RealtimeChatEvent, error)

type ChatCancelRequest struct {
	RequestID string `json:"request_id"`
}

type ChatCancelResponse struct {
	RequestID string `json:"request_id"`
	Status    string `json:"status"`
}

type ChatSteerRequest struct {
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}

type ChatSteerResponse struct {
	RequestID string               `json:"request_id"`
	Status    string               `json:"status"`
	Steer     *pendingSteerMessage `json:"steer,omitempty"`
}

type pendingSteerMessage struct {
	ID        string    `json:"id"`
	RequestID string    `json:"request_id"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// ChatResponse represents a chat response
type ChatResponse struct {
	Response string    `json:"response"`
	History  []Message `json:"history"`
}

// chatPendingResult stores intermediate results for async chat requests.
// The agent goroutine appends tool_call / assistant messages as they occur;
// the polling endpoint reads them back with an offset cursor.
type chatPendingResult struct {
	mu       sync.Mutex
	messages []Message
	done     bool
	err      string
	// Per-request history (separate from server-level history)
	history []Message
}

// ChatResultResponse is the JSON shape for GET /api/chat/result.
type ChatResultResponse struct {
	Status       string             `json:"status"`
	Messages     []Message          `json:"messages,omitempty"`
	Response     string             `json:"response,omitempty"`
	History      []Message          `json:"history,omitempty"`
	Error        string             `json:"error,omitempty"`
	LiveActivity *LiveActivityState `json:"live_activity,omitempty"`
}

type ChatStreamEvent struct {
	Type             string    `json:"type"`
	Message          *Message  `json:"message,omitempty"`
	RequestID        string    `json:"request_id,omitempty"`
	EpisodeID        string    `json:"episode_id,omitempty"`
	Delta            string    `json:"delta,omitempty"`
	ReasoningContent string    `json:"reasoning_content,omitempty"`
	Response         string    `json:"response,omitempty"`
	History          []Message `json:"history,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type EpisodeResponse struct {
	Episode TaskEpisode `json:"episode"`
}

type ToolCatalogResponse struct {
	Tools []ToolDescriptor `json:"tools"`
}

type ToolSkillsResponse struct {
	Skills []ToolSkillDefinition `json:"skills"`
}

type ToolInvokeRequest struct {
	Input    json.RawMessage `json:"input,omitempty"`
	RawInput *string         `json:"raw_input,omitempty"`
}

type ToolInvokeResponse struct {
	Tool       ToolDescriptor `json:"tool"`
	RawInput   string         `json:"raw_input"`
	Output     string         `json:"output"`
	IsError    bool           `json:"is_error"`
	Error      string         `json:"error,omitempty"`
	ToolError  *ToolError     `json:"tool_error,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	CalledAt   time.Time      `json:"called_at"`
}

// NewServer creates a new HTTP server
func NewServer(runtime *Runtime, addr string) *Server {
	userFilesBaseDir := strings.TrimSpace(runtime.ConfigSnapshot().ConfigDir)
	if userFilesBaseDir == "" {
		userFilesBaseDir = "/userdata/agent"
	}
	s := &Server{
		runtime:                 runtime,
		addr:                    addr,
		logger:                  runtime.logger,
		userFilesReportPath:     "/userdata/agent/files_report.html",
		userFilesToolsDir:       "/userdata/agent_tools",
		userFilesMemoryDir:      filepath.Join(userFilesBaseDir, "memory"),
		userFilesSkillsDir:      filepath.Join(userFilesBaseDir, "skills"),
		userFilesSkillStateDir:  filepath.Join(userFilesBaseDir, "skill-state"),
		screenCaptureClient:     screenProviderFromRuntime(runtime),
		bridge:                  runtime.PhoneBridge(),
		bleSocketPath:           configuredBLEServiceSocketPath(),
		bleStatusRequest:        ble.RequestStatus,
		blePairingStartRequest:  ble.RequestPairingStart,
		blePairingForgetRequest: ble.RequestPairingForget,
		bleDisconnectRequest:    ble.RequestDisconnect,
		bleNotifyRequest:        ble.RequestPublishNotifications,
		bleWakeRequest:          defaultBLEWake,
		androidADB:              NewAndroidADBManager(runtime.ConfigSnapshot().HID.FrameSocketOrDefault(), runtime.logger),
		liveActivity:            newReloadableLiveActivityManager(runtime.ConfigSnapshot().LiveActivity, runtime.ConfigSnapshot().LocaleOrDefault(), runtime.logger),
		pendingResults:          make(map[string]*chatPendingResult),
		activeRuns:              make(map[string]context.CancelFunc),
		terminatedRequests:      make(map[string]struct{}),
		pendingSteers:           make(map[string]pendingSteerMessage),
		steerSignals:            make(map[string]chan struct{}),
		eventBroadcaster:        NewEventBroadcaster(),
		storageMonitor:          runtime.storageMonitor,
	}
	if s.bridge != nil {
		s.bridge.SetConfiguredPlatform(runtime.devicePlatformFromState())
	}
	if s.liveActivity != nil {
		s.liveActivity.SetLocalUpdateNotifier(defaultBLEWake)
	}
	if s.userFilesMemoryDir != "" {
		s.episodeStore = NewTaskEpisodeStore(filepath.Join(s.userFilesMemoryDir, "episodes"))
	}
	loadQuickActionsForConfig(runtime.ConfigSnapshot().ConfigDir, runtime.logger)
	s.quickCapture = newServerQuickCapture(runtime, s.screenCaptureClient)
	runtime.toolSnapshot().RegisterPhoneBridge(s.bridge)
	// Initialize speech clients if configured.
	cfg := runtime.ConfigSnapshot()
	s.audioClient = NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	s.ttsPlaybackBackend = newTTSPlaybackBackendFromConfig(cfg, s.audioClient, s.logger)

	sttClient, err := NewSTTClientFromConfig(cfg)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("STT init failed: %v", err)
		}
	} else {
		s.sttClient = sttClient
	}

	// Share the process-wide TTS provider manager with every runtime entrypoint.
	s.ttsManager = runtime.ttsProviderManager()
	if manager := s.currentTTSManager(); manager != nil {
		if s.logger != nil {
			s.logger.Info("TTS enabled: provider=%s", manager.Current())
		}
	}
	// Register preempt hook: when a new run starts, release WebUI audio resources.
	runtime.RegisterPreemptHook(func() {
		s.interruptAllActiveOutputs()
	})

	runtime.SetConfigPreparer(s.PrepareConfig)
	return s
}

// SetRealtimeChatHandler installs the text bridge used while the realtime
// voice session is active. Passing nil disables the bridge.
func (s *Server) SetRealtimeChatHandler(handler RealtimeChatHandler) {
	if s == nil {
		return
	}
	s.realtimeChatMu.Lock()
	s.realtimeChatHandler = handler
	s.realtimeChatMu.Unlock()
}

func (s *Server) realtimeChatHandlerSnapshot() RealtimeChatHandler {
	if s == nil {
		return nil
	}
	s.realtimeChatMu.RLock()
	defer s.realtimeChatMu.RUnlock()
	return s.realtimeChatHandler
}

// Handler returns the HTTP API and web UI routes served by Server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API endpoints
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/chat/cancel", s.handleChatCancel)
	mux.HandleFunc("/api/chat/steer", s.handleChatSteer)
	mux.HandleFunc("/api/chat/steer/cancel", s.handleChatSteerCancel)
	mux.HandleFunc("/api/chat/result", s.handleChatResult)
	mux.HandleFunc("/api/live-activity/status", s.handleLiveActivityStatus)
	mux.HandleFunc("/api/live-activity/current", s.handleLiveActivityCurrent)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/context", s.handleContext)
	mux.HandleFunc("/api/context/attachment", s.handleContextAttachment)
	mux.HandleFunc("/api/episodes/", s.handleEpisodes)
	mux.HandleFunc("/api/setup", s.handleSetup)
	if s.benchmarkToken() != "" {
		mux.HandleFunc("/api/benchmark/seed_memory", s.handleBenchmarkSeedMemory)
		mux.HandleFunc("/api/benchmark/seed_episode", s.handleBenchmarkSeedEpisode)
		mux.HandleFunc("/api/benchmark/seed_session_chunk", s.handleBenchmarkSeedSessionChunk)
		mux.HandleFunc("/api/benchmark/episode-memory/process", s.handleBenchmarkProcessEpisodeMemory)
		mux.HandleFunc("/api/benchmark/seed_notification", s.handleBenchmarkSeedNotification)
		mux.HandleFunc("/api/benchmark/notification-memory/process", s.handleBenchmarkProcessNotificationMemory)
		mux.HandleFunc("/api/benchmark/phone_bridge_state", s.handleBenchmarkPhoneBridgeState)
	}
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/clear-all", s.handleClearAll)
	mux.HandleFunc("/api/skills/reload", s.handleSkillsReload)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/", s.handleTools)
	mux.HandleFunc("/api/providers/screenshot", s.handleProviderScreenshot)
	mux.HandleFunc("/api/providers/mnk", s.handleProviderMNK)
	mux.HandleFunc("/api/concurrent", s.handleConcurrent)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/tool-skills", s.handleToolSkills)
	mux.HandleFunc("/api/internal/config/reload", s.handleInternalConfigReload)
	mux.HandleFunc("/api/audio/", s.handleAudioFile)
	mux.HandleFunc("/api/settings/tts", s.handleTTSSettings)
	mux.HandleFunc("/api/tts/providers", s.handleTTSProviders)
	mux.HandleFunc("/api/phone-bridge", s.bridge.HandleWebSocket)
	mux.HandleFunc("/api/phone-bridge/status", s.handleBridgeStatus)
	// HTTP queue endpoints for iOS background compatibility
	mux.HandleFunc("/api/phone-bridge/commands", s.handlePhoneBridgeCommands)
	mux.HandleFunc("/api/phone-bridge/commands/", s.handlePhoneBridgeCommandCancel)
	mux.HandleFunc("/api/phone-bridge/results", s.handlePhoneBridgeResults)
	mux.HandleFunc("/api/phone-bridge/results/", s.handlePhoneBridgeResults)
	mux.HandleFunc("/api/bluetooth/status", s.handleBluetoothStatus)
	mux.HandleFunc("/api/bluetooth/pairing/start", s.handleBluetoothPairingStart)
	mux.HandleFunc("/api/bluetooth/pairing/reset", s.handleBluetoothPairingReset)
	mux.HandleFunc("/api/bluetooth/disconnect", s.handleBluetoothDisconnect)
	mux.HandleFunc("/api/bluetooth/wake/usb-reenumeration", s.handleBluetoothUSBReenumerationWake)
	mux.HandleFunc("/api/phone-notifications/events", s.handlePhoneNotificationEvents)
	mux.HandleFunc("/api/android-adb/status", s.handleAndroidADBStatus)
	mux.HandleFunc("/api/android-adb/pair", s.handleAndroidADBPair)
	mux.HandleFunc("/api/storage/monitor/status", s.handleStorageMonitorStatus)
	mux.HandleFunc("/api/storage/cleanup", s.handleStorageCleanup)

	// Coordinate debug tool. Live frames come from POST /api/providers/screenshot.
	mux.HandleFunc("/api/coordinate-debug/tap", s.handleCoordinateDebugTap)
	mux.HandleFunc("/coordinate-debug", s.handleCoordinateDebug)
	mux.HandleFunc("/coordinate-debug.html", s.handleCoordinateDebug)
	mux.HandleFunc("/keyboard-tap-android-keys", s.handleKeyboardTapAndroidKeys)

	mux.HandleFunc("/user_files", s.handleUserFiles)
	mux.HandleFunc("/user_files/preview", s.handleUserFilesPreview)
	mux.HandleFunc("/user_files/regenerate", s.handleUserFilesRegenerate)

	// Static web UI
	ttydProxy := newTerminalReverseProxy()
	mux.Handle("/webtty", ttydProxy)
	mux.Handle("/webtty/", ttydProxy)
	mux.Handle("/web-ui/", http.StripPrefix("/web-ui/", http.FileServer(http.FS(webUIFiles))))
	mux.HandleFunc("/", s.handleIndex)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.closing.Load() {
			http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
			return
		}
		if s.runtime != nil && (strings.HasPrefix(r.URL.Path, "/api/tools/") || r.URL.Path == "/api/providers/mnk" || r.URL.Path == "/api/providers/screenshot" || r.URL.Path == "/api/coordinate-debug/tap") {
			s.runtime.configOperations.RLock()
			defer s.runtime.configOperations.RUnlock()
		}
		mux.ServeHTTP(w, r)
	})
}

func newTerminalReverseProxy() *httputil.ReverseProxy {
	target, _ := url.Parse("http://127.0.0.1:3000")
	return newTerminalReverseProxyForTarget(target)
}

func newTerminalReverseProxyForTarget(target *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(req *http.Request) {
		originalHost := req.Host
		originalProto := "http"
		if forwardedProto := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
			originalProto = firstForwardedHeaderValue(forwardedProto)
		} else if req.TLS != nil {
			originalProto = "https"
		}
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = originalHost
		req.Header.Set("X-Forwarded-Host", originalHost)
		req.Header.Set("X-Forwarded-Proto", originalProto)
		req.Header.Set("X-Forwarded-Prefix", "/webtty")
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Del("X-Frame-Options")
		csp := strings.TrimSpace(resp.Header.Get("Content-Security-Policy"))
		if !strings.Contains(csp, "frame-ancestors") {
			if csp != "" {
				csp += "; "
			}
			resp.Header.Set("Content-Security-Policy", csp+"frame-ancestors 'self'")
		}
		if location := resp.Header.Get("Location"); location != "" {
			if parsed, err := url.Parse(location); err == nil {
				path := parsed.Path
				if path == "" || path == "/" {
					path = "/webtty/"
				} else if !strings.HasPrefix(path, "/webtty") {
					path = "/webtty" + path
				}
				parsed.Scheme = ""
				parsed.Host = ""
				parsed.Path = path
				parsed.RawPath = ""
				resp.Header.Set("Location", parsed.String())
			}
		}
		return nil
	}
	return proxy
}

// Start starts the HTTP server
func (s *Server) Start() error {
	if s.closing.Load() {
		return nil
	}
	handler := s.Handler()

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           handler,
		ReadHeaderTimeout: agentHTTPReadHeaderTimeout,
		ReadTimeout:       agentHTTPReadTimeout,
		IdleTimeout:       agentHTTPIdleTimeout,
	}

	// Bind explicitly rather than letting ListenAndServe do it, so the readiness
	// milestone is recorded only once the port is actually held. Marking before
	// the bind would report the agent Web UI as listening even when startup
	// failed on, say, an address already in use.
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.addr, err)
	}
	s.httpMu.Lock()
	if s.closing.Load() {
		s.httpMu.Unlock()
		_ = listener.Close()
		return nil
	}
	s.httpServer = srv
	s.httpListener = listener
	s.httpMu.Unlock()

	// Milestone: the agent Web UI (http://192.168.42.1:8080) is now accepting
	// connections. Stamped with kernel uptime so it can be lined up against the
	// init-script timeline in /var/log/aiden_boot_timeline.log.
	if s.logger != nil {
		s.logger.Info("Starting HTTP server on %s%s", s.addr, bootUptimeLogSuffix())
	}
	MarkBootTimeline("agent:listening")

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		return err
	case sig := <-sigCh:
		if s.logger != nil {
			s.logger.Info("Shutting down HTTP server after signal %s", sig)
		}
		s.Close()
		return <-errCh
	}
}

// Close cancels background runs and releases request-owned audio resources.
// The shared TTS provider remains owned by Runtime.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.closing.Store(true)
	s.closeOnce.Do(func() {
		s.httpMu.Lock()
		srv := s.httpServer
		listener := s.httpListener
		s.httpMu.Unlock()
		if listener != nil {
			_ = listener.Close()
		}

		s.cancelAllActiveRuns()
		s.interruptAllActiveOutputs()

		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), agentHTTPShutdownTimeout)
			shutdownErr := srv.Shutdown(ctx)
			cancel()
			if shutdownErr != nil {
				closeErr := srv.Close()
				if s.logger != nil {
					s.logger.Warn("HTTP server shutdown failed: %v", errors.Join(shutdownErr, closeErr))
				}
			}
		}
		s.httpMu.Lock()
		if s.httpServer == srv {
			s.httpServer = nil
		}
		if s.httpListener == listener {
			s.httpListener = nil
		}
		s.httpMu.Unlock()
	})
	// A closed server cannot accept new runs, and request-scoped output
	// registration is rejected while closing, so no termination marker is
	// needed after shutdown begins.
	s.clearAllRequestTerminations()
}

func isLoopbackServerAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleChat handles chat requests.
// Runs asynchronously: generates request_id if not provided, returns {request_id} immediately,
// agent runs in a background goroutine, and intermediate tool_call messages become
// available via GET /api/chat/result?request_id=xxx&offset=N.
// For streaming responses, set Accept: application/x-ndjson or X-Aiden-Stream: ndjson.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if wantsChatStream(r) {
		if s.logger != nil {
			s.logger.Info("Handling chat stream")
		}
		s.handleChatStream(w, r)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		req.RequestID = createRequestID()
	}

	turnInput, historyAttachments, err := s.resolveRequestInput(req)
	if err != nil {
		http.Error(w, "Invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if handler := s.realtimeChatHandlerSnapshot(); handler != nil {
		if len(turnInput.Attachments) > 0 {
			http.Error(w, "realtime chat supports text messages only", http.StatusBadRequest)
			return
		}
		s.handleRealtimeChatAsync(w, req, turnInput)
		return
	}
	episodeID := s.runtime.NewEpisodeID()

	userMsg := messageFromTurnInput(turnInput, episodeID, req.RequestID, historyAttachments, time.Now())

	s.logger.Info("Handling chat async")
	s.handleChatAsync(w, req, turnInput, userMsg)
}

// handleChatCancel stops request-owned work without retaining a termination
// marker for an unknown or already-finished request.
func (s *Server) handleChatCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatCancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}
	source := chatCancelRequestSource(r)

	runCanceled := s.cancelActiveRun(requestID)
	outputCanceled := s.interruptRequestOutputs(requestID)
	if runCanceled || outputCanceled {
		if s.logger != nil {
			s.logger.Info("Chat request canceled: request_id=%s source=%s run_canceled=%t output_canceled=%t", requestID, source, runCanceled, outputCanceled)
		}
		if s.liveActivity != nil {
			s.liveActivity.CancelTask(requestID)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ChatCancelResponse{RequestID: requestID, Status: "canceled"})
		return
	}

	if s.liveActivity != nil {
		if state := s.liveActivity.Snapshot(requestID); state != nil && isCancelableLiveActivityStatus(state.Status) {
			s.liveActivity.CancelTask(requestID)
			s.clearRequestTerminationIfInactive(requestID)
			if s.logger != nil {
				s.logger.Info("Chat request live activity canceled without active run: request_id=%s source=%s", requestID, source)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatCancelResponse{RequestID: requestID, Status: "canceled"})
			return
		}
	}
	// The marker protects a request only while a run or request-scoped output
	// can still race with cancellation. Unknown or already-finished requests do
	// not need a tombstone.
	s.clearRequestTerminationIfInactive(requestID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ChatCancelResponse{RequestID: requestID, Status: "not_running"})
}

func chatCancelRequestSource(r *http.Request) string {
	if r == nil {
		return "unknown"
	}
	remote := strings.TrimSpace(r.RemoteAddr)
	forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	userAgent := strings.TrimSpace(r.UserAgent())
	referer := strings.TrimSpace(r.Referer())
	parts := make([]string, 0, 4)
	if remote != "" {
		parts = append(parts, "remote="+remote)
	}
	if forwardedFor != "" {
		parts = append(parts, "xff="+forwardedFor)
	}
	if userAgent != "" {
		parts = append(parts, "ua="+truncateLogField(userAgent, 120))
	}
	if referer != "" {
		parts = append(parts, "referer="+truncateLogField(referer, 160))
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}

func truncateLogField(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return value[:limit-3] + "..."
}

func (s *Server) handleChatSteer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatSteerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	requestID := strings.TrimSpace(req.RequestID)
	message := strings.TrimSpace(req.Message)
	if requestID == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}
	if message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	steer, ok := s.setPendingSteer(requestID, message)
	if !ok {
		http.Error(w, "request_id is not running", http.StatusConflict)
		return
	}
	if s.logger != nil {
		s.logger.Info("Queued chat steer: request_id=%s len=%d", requestID, len(message))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ChatSteerResponse{RequestID: requestID, Status: "pending", Steer: &steer})
}

func (s *Server) handleChatSteerCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ChatSteerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	requestID := strings.TrimSpace(req.RequestID)
	if requestID == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}

	status := "not_found"
	if s.cancelPendingSteer(requestID) {
		status = "canceled"
		if s.logger != nil {
			s.logger.Info("Canceled chat steer: request_id=%s", requestID)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ChatSteerResponse{RequestID: requestID, Status: status})
}

// registerActiveRun atomically claims a request ID and clears any marker left
// by its previous lifecycle. It rejects registration while the server closes.
func (s *Server) registerActiveRun(requestID string, cancel context.CancelFunc) bool {
	if s == nil || s.closing.Load() {
		return false
	}
	if requestID == "" {
		return true
	}
	// Serialize registration with termination marking. A successful new run is
	// the only point at which reusing a request ID should clear its marker.
	s.terminatedRequestsMu.Lock()
	defer s.terminatedRequestsMu.Unlock()
	s.activeRunsMu.Lock()
	defer s.activeRunsMu.Unlock()
	if s.closing.Load() {
		return false
	}
	if s.activeRuns == nil {
		s.activeRuns = make(map[string]context.CancelFunc)
	}
	if _, exists := s.activeRuns[requestID]; exists {
		return false
	}
	s.activeRuns[requestID] = cancel
	s.deleteRequestTerminationLocked(requestID)
	return true
}

// unregisterActiveRun releases a request ID and drops its termination marker
// once no request-scoped output remains.
func (s *Server) unregisterActiveRun(requestID string) {
	if requestID == "" {
		return
	}
	s.activeRunsMu.Lock()
	delete(s.activeRuns, requestID)
	s.activeRunsMu.Unlock()
	s.clearRequestTerminationIfInactive(requestID)
}

// cancelActiveRun marks an active request before invoking its cancellation
// callback, preventing new request-scoped output from registering meanwhile.
func (s *Server) cancelActiveRun(requestID string) bool {
	if s == nil || requestID == "" {
		return false
	}
	s.terminatedRequestsMu.Lock()
	s.activeRunsMu.Lock()
	cancel, active := s.activeRuns[requestID]
	if active && !s.closing.Load() {
		s.markRequestTerminatedLocked(requestID)
	}
	s.activeRunsMu.Unlock()
	s.terminatedRequestsMu.Unlock()
	if !active {
		return false
	}
	if cancel != nil {
		cancel()
	}
	return true
}

// cancelAllActiveRuns snapshots and cancels every registered run during
// shutdown without holding activeRunsMu while callbacks execute.
func (s *Server) cancelAllActiveRuns() {
	s.activeRunsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.activeRuns))
	for _, cancel := range s.activeRuns {
		if cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	s.activeRunsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// markRequestTerminatedLocked records a marker while terminatedRequestsMu is
// held for writing.
func (s *Server) markRequestTerminatedLocked(requestID string) {
	if s.terminatedRequests == nil {
		s.terminatedRequests = make(map[string]struct{})
	}
	s.terminatedRequests[requestID] = struct{}{}
}

// isRequestTerminatedLocked reports marker state while terminatedRequestsMu is
// held for reading or writing.
func (s *Server) isRequestTerminatedLocked(requestID string) bool {
	_, terminated := s.terminatedRequests[requestID]
	return terminated
}

// deleteRequestTerminationLocked removes a marker while terminatedRequestsMu
// is held for writing and releases an empty map's retained capacity.
func (s *Server) deleteRequestTerminationLocked(requestID string) {
	delete(s.terminatedRequests, requestID)
	if len(s.terminatedRequests) == 0 {
		// Releasing an empty map also releases buckets retained after a burst of
		// cancellations, so the server does not keep the peak map capacity.
		s.terminatedRequests = nil
	}
}

// clearRequestTerminationIfInactive drops a cancellation marker once no
// request-owned work can still observe it. Keeping the marker while another
// run or output is active prevents a late TTS start from racing cancellation.
func (s *Server) clearRequestTerminationIfInactive(requestID string) {
	if s == nil || requestID == "" {
		return
	}

	// Keep the activity check and deletion under one termination critical
	// section so a concurrent cancellation cannot race a cleanup callback.
	s.terminatedRequestsMu.Lock()
	defer s.terminatedRequestsMu.Unlock()

	s.activeRunsMu.Lock()
	_, runActive := s.activeRuns[requestID]
	s.activeRunsMu.Unlock()
	if runActive {
		return
	}

	s.activeOutputsMu.Lock()
	outputActive := len(s.activeOutputs[requestID]) > 0
	s.activeOutputsMu.Unlock()
	if outputActive {
		return
	}

	s.deleteRequestTerminationLocked(requestID)
}

// clearAllRequestTerminations releases all marker storage after shutdown has
// prevented new request-owned work from registering.
func (s *Server) clearAllRequestTerminations() {
	if s == nil {
		return
	}
	s.terminatedRequestsMu.Lock()
	s.terminatedRequests = nil
	s.terminatedRequestsMu.Unlock()
}

// isRequestTerminated reports whether cancellation currently blocks new work
// for a request ID.
func (s *Server) isRequestTerminated(requestID string) bool {
	if s == nil || requestID == "" {
		return false
	}
	s.terminatedRequestsMu.RLock()
	terminated := s.isRequestTerminatedLocked(requestID)
	s.terminatedRequestsMu.RUnlock()
	return terminated
}

// registerActiveOutput attaches TTS output to a live request and returns an
// idempotent cleanup callback. Terminated requests interrupt the output.
func (s *Server) registerActiveOutput(requestID string, output *activeTTSOutput) func() {
	if s == nil || requestID == "" || output == nil {
		return func() {}
	}
	if s.closing.Load() {
		output.interrupt()
		return output.finish
	}
	s.terminatedRequestsMu.Lock()
	s.activeOutputsMu.Lock()
	if s.closing.Load() || s.isRequestTerminatedLocked(requestID) {
		s.activeOutputsMu.Unlock()
		s.terminatedRequestsMu.Unlock()
		output.interrupt()
		return output.finish
	}
	if s.activeOutputs == nil {
		s.activeOutputs = make(map[string]map[*activeTTSOutput]struct{})
	}
	outputs := s.activeOutputs[requestID]
	if outputs == nil {
		outputs = make(map[*activeTTSOutput]struct{})
		s.activeOutputs[requestID] = outputs
	}
	outputs[output] = struct{}{}
	s.activeOutputsMu.Unlock()
	s.terminatedRequestsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeOutputsMu.Lock()
			outputs := s.activeOutputs[requestID]
			if outputs != nil {
				delete(outputs, output)
				if len(outputs) == 0 {
					delete(s.activeOutputs, requestID)
				}
			}
			s.activeOutputsMu.Unlock()
			output.finish()
			s.clearRequestTerminationIfInactive(requestID)
		})
	}
}

// snapshotActiveOutputs copies the outputs currently owned by a request.
func (s *Server) snapshotActiveOutputs(requestID string) []*activeTTSOutput {
	if s == nil || requestID == "" {
		return nil
	}
	s.activeOutputsMu.Lock()
	defer s.activeOutputsMu.Unlock()
	return s.snapshotActiveOutputsLocked(requestID)
}

// snapshotActiveOutputsLocked copies request outputs while activeOutputsMu is
// held.
func (s *Server) snapshotActiveOutputsLocked(requestID string) []*activeTTSOutput {
	outputs := s.activeOutputs[requestID]
	if len(outputs) == 0 {
		return nil
	}
	snapshot := make([]*activeTTSOutput, 0, len(outputs))
	for output := range outputs {
		snapshot = append(snapshot, output)
	}
	return snapshot
}

// interruptRequestOutputs marks and interrupts every output currently owned by
// a request, blocking late output registration until cleanup completes.
func (s *Server) interruptRequestOutputs(requestID string) bool {
	if s == nil || requestID == "" {
		return false
	}
	s.terminatedRequestsMu.Lock()
	s.activeOutputsMu.Lock()
	outputs := s.snapshotActiveOutputsLocked(requestID)
	s.activeOutputsMu.Unlock()
	if len(outputs) > 0 && !s.closing.Load() {
		s.markRequestTerminatedLocked(requestID)
	}
	s.terminatedRequestsMu.Unlock()
	if len(outputs) == 0 {
		return false
	}
	for _, output := range outputs {
		output.interrupt()
	}
	waitForActiveOutputs(outputs, outputInterruptWaitTimeout)
	return true
}

// interruptAllActiveOutputs interrupts TTS outputs for all active requests.
func (s *Server) interruptAllActiveOutputs() {
	s.activeOutputsMu.Lock()
	var all []*activeTTSOutput
	for _, outputs := range s.activeOutputs {
		for output := range outputs {
			all = append(all, output)
		}
	}
	s.activeOutputsMu.Unlock()
	for _, output := range all {
		output.interrupt()
	}
}

func (s *Server) setPendingSteer(requestID, content string) (pendingSteerMessage, bool) {
	s.activeRunsMu.Lock()
	defer s.activeRunsMu.Unlock()
	if requestID == "" || s.activeRuns[requestID] == nil {
		return pendingSteerMessage{}, false
	}
	steer := pendingSteerMessage{
		ID:        createSteerID(),
		RequestID: requestID,
		Content:   strings.TrimSpace(content),
		Timestamp: time.Now(),
	}
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	if s.pendingSteers == nil {
		s.pendingSteers = make(map[string]pendingSteerMessage)
	}
	s.pendingSteers[requestID] = steer
	// Store and signal atomically so cancellation or consumption cannot leave the
	// message and interrupt channel describing different states.
	s.signalSteerLocked(requestID)
	return steer, true
}

func (s *Server) cancelPendingSteer(requestID string) bool {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	if s.pendingSteers == nil {
		return false
	}
	if _, ok := s.pendingSteers[requestID]; !ok {
		return false
	}
	delete(s.pendingSteers, requestID)
	s.resetSteerSignalLocked(requestID)
	return true
}

func (s *Server) clearSteerState(requestID string) {
	if requestID == "" {
		return
	}
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	delete(s.pendingSteers, requestID)
	delete(s.steerSignals, requestID)
}

// consumePendingSteer removes the pending message and rearms its interrupt
// channel in one critical section.
func (s *Server) consumePendingSteer(requestID string) (RunSteerMessage, bool) {
	if requestID == "" {
		return RunSteerMessage{}, false
	}
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	steer, ok := s.pendingSteers[requestID]
	if !ok {
		return RunSteerMessage{}, false
	}
	delete(s.pendingSteers, requestID)
	s.resetSteerSignalLocked(requestID)
	return RunSteerMessage{
		ID:        steer.ID,
		RequestID: steer.RequestID,
		Content:   steer.Content,
		Timestamp: steer.Timestamp,
	}, true
}

// signalSteer closes the current steer signal channel for the request,
// causing any blocking LLM/tool call to observe the interrupt.
func (s *Server) signalSteer(requestID string) {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	s.signalSteerLocked(requestID)
}

func (s *Server) signalSteerLocked(requestID string) {
	if _, ok := s.pendingSteers[requestID]; !ok {
		return
	}
	if s.steerSignals == nil {
		s.steerSignals = make(map[string]chan struct{})
	}
	ch := s.steerSignals[requestID]
	if ch == nil {
		ch = make(chan struct{})
		s.steerSignals[requestID] = ch
	}
	select {
	case <-ch:
		// The signal remains closed until the steer is consumed or canceled.
	default:
		close(ch)
	}
}

// resetSteerSignal creates a fresh signal channel for the request.
// If a steer is already pending, the fresh channel is closed immediately so
// resetting cannot hide an instruction that raced with the reset.
func (s *Server) resetSteerSignal(requestID string) {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	s.resetSteerSignalLocked(requestID)
}

func (s *Server) resetSteerSignalLocked(requestID string) {
	if s.steerSignals == nil {
		s.steerSignals = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	if _, ok := s.pendingSteers[requestID]; ok {
		close(ch)
	}
	s.steerSignals[requestID] = ch
}

// steerSignalChannel returns the current steer signal channel for the request.
func (s *Server) steerSignalChannel(requestID string) <-chan struct{} {
	s.steersMu.Lock()
	defer s.steersMu.Unlock()
	if s.steerSignals[requestID] == nil {
		s.resetSteerSignalLocked(requestID)
	}
	return s.steerSignals[requestID]
}

func (s *Server) prepareRunSteerCallbacks(requestID string) (
	func(context.Context) (RunSteerMessage, bool),
	func() <-chan struct{},
) {
	s.resetSteerSignal(requestID)
	provider := func(context.Context) (RunSteerMessage, bool) {
		return s.consumePendingSteer(requestID)
	}
	interrupt := func() <-chan struct{} {
		return s.steerSignalChannel(requestID)
	}
	return provider, interrupt
}

func createSteerID() string {
	return "steer-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func createRequestID() string {
	return "req-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// handleChatAsync runs the agent in a background goroutine and returns
// {request_id} immediately. Intermediate results are pushed into
// chatPendingResult and served by handleChatResult.
func (s *Server) handleChatAsync(
	w http.ResponseWriter,
	req ChatRequest,
	turnInput TurnInput,
	userMsg Message,
) {
	requestID := req.RequestID
	inputText := turnInput.InputText
	if s.logger != nil {
		s.logger.Info("Chat request (async): %s request_id=%s attachments=%d", inputText, requestID, len(turnInput.Attachments))
	}

	// Create a pending result slot for this request. Reject a request_id that
	// already has an active slot rather than overwriting it — otherwise the
	// in-flight run's poller loses its result stream and the two goroutines can
	// delete each other's state on exit.
	pending := &chatPendingResult{
		messages: make([]Message, 0),
		history:  []Message{userMsg},
	}
	s.pendingResultsMu.Lock()
	if _, exists := s.pendingResults[requestID]; exists {
		s.pendingResultsMu.Unlock()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	s.pendingResults[requestID] = pending
	s.pendingResultsMu.Unlock()

	s.playPromptSoundAsync(promptSoundAgentSend, "agent send")

	runCtx, cancel := context.WithCancel(context.Background())
	if !s.registerActiveRun(requestID, cancel) {
		cancel()
		s.pendingResultsMu.Lock()
		delete(s.pendingResults, requestID)
		s.pendingResultsMu.Unlock()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	s.publishMessage(userMsg)
	if s.liveActivity != nil {
		s.liveActivity.StartTask(requestID, inputText, s.liveActivityPhoneID(req))
	}

	// Return {request_id} immediately
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"request_id": requestID})

	// Run agent in background goroutine
	steerProvider, steerInterrupt := s.prepareRunSteerCallbacks(requestID)
	go func() {
		defer func() {
			s.unregisterActiveRun(requestID)
			s.clearSteerState(requestID)
			// Keep the completed result available for polling for 60s,
			// then clean up. The client (PhoneBridge) polls /api/chat/result
			// every ~1s; immediate deletion creates a race where the app
			// gets status="not_found" and hangs forever waiting for "complete".
			time.AfterFunc(60*time.Second, func() {
				s.pendingResultsMu.Lock()
				delete(s.pendingResults, requestID)
				s.pendingResultsMu.Unlock()
			})
		}()

		var speechWriter *speech.StreamWriter

		// Event handler pushes intermediate messages into the pending result
		// so the client can poll them in near-realtime.
		eventHandler := func(event RunEvent) {
			if event.Type == runEventReasoningDelta || event.Type == runEventReasoningReset {
				msg := pollingMessageFromReasoningEvent(event, userMsg.EpisodeID, requestID)
				pending.mu.Lock()
				// Reasoning events contain deltas, not accumulated prefixes. The
				// polling client appends them to its in-memory draft.
				pending.history = append(pending.history, msg)
				pending.messages = append(pending.messages, msg)
				pending.mu.Unlock()
				if s.liveActivity != nil {
					s.liveActivity.UpdateFromRunEvent(requestID, event)
				}
				return
			}
			msg := messageFromRunEvent(event, userMsg.EpisodeID, requestID)
			if msg.Type != "" {
				if msg, ok := s.publishMessage(msg); ok {
					pending.mu.Lock()
					pending.history = append(pending.history, msg)
					pending.messages = append(pending.messages, msg)
					pending.mu.Unlock()
				}
			}
			if s.liveActivity != nil {
				s.liveActivity.UpdateFromRunEvent(requestID, event)
			}

			toolSpeechStreamed := event.Type == runEventToolCall && speechWriter.FinalizeResponse()
			if toolSpeechStreamed && s.logger != nil {
				s.logger.Info("Tool content TTS already streamed: tool=%s", event.ToolName)
			}
			if text := s.toolCallSpeechText(event); text != "" && !toolSpeechStreamed {
				go s.speakToolContent(runCtx, text)
			}
		}

		runReq := RunRequest{
			Input:                   inputText,
			Attachments:             turnInput.Attachments,
			Turn:                    turnInput,
			EpisodeID:               userMsg.EpisodeID,
			RequestID:               requestID,
			DeviceEnvironment:       s.bridgeEnvironment(),
			EventHandler:            eventHandler,
			AsyncEpisodeMaintenance: true,
			SteerProvider:           steerProvider,
			SteerInterrupt:          steerInterrupt,
		}

		var newStream *streamSessionWriter
		unregisterStreamOutput := func() {}
		ttsManager := s.currentTTSManager()
		cfg := s.runtime.ConfigSnapshot()

		if cfg.VoiceStreamingTTSEnabledOrDefault() && s.currentTTSPlaybackBackend() != nil {
			if ttsManager != nil {
				stream, err := beginManagedTTSStream(runCtx, ttsManager, s.currentTTSPlaybackBackend(), cfg)
				if err != nil {
					if s.logger != nil {
						s.logger.Warn("TTS BeginStream failed: %v", err)
					}
				} else {
					newStream = stream
					output := newActiveTTSOutput(nil)
					output.setStream(newStream)
					runReq.OnRunActive = func(context.Context) {
						unregisterStreamOutput = s.registerActiveOutput(requestID, output)
					}
					speechWriter = speech.NewStreamWriter(newStream)
					runReq.StreamWriter = speechWriter
				}
			}
		}
		defer func() {
			unregisterStreamOutput()
		}()

		result, err := s.runtime.Run(runCtx, runReq)
		if newStream != nil {
			finalSpeechStreamed := speechWriter.FinalizeResponse()
			closeErr := newStream.closeAndWait()
			if closeErr != nil && s.logger != nil {
				s.logger.Error("new TTS stream failed: %v", closeErr)
			}
			result.SpeechStreamed = finalSpeechStreamed && newStream.emittedSpeech(closeErr)
		}

		pending.mu.Lock()
		if err != nil {
			canceled := errors.Is(runCtx.Err(), context.Canceled) || errors.Is(err, context.Canceled)
			if canceled {
				if s.liveActivity != nil {
					s.liveActivity.CancelTask(requestID)
				}
				if s.runtime.WasPreempted(5 * time.Second) {
					pending.err = "preempted by another input source"
				} else {
					pending.err = "request canceled"
				}
			} else {
				errorMsg := Message{
					Type:      "episode_status",
					EpisodeID: userMsg.EpisodeID,
					RequestID: requestID,
					Status:    "failed",
					Content:   "Agent error: " + err.Error(),
					Timestamp: time.Now(),
					IsError:   true,
				}
				if errorMsg, ok := s.publishMessage(errorMsg); ok {
					pending.history = append(pending.history, errorMsg)
					pending.messages = append(pending.messages, errorMsg)
				}
				if s.liveActivity != nil {
					s.liveActivity.FailTask(requestID, err.Error())
				}
				pending.err = err.Error()
			}
			pending.done = true
			pending.mu.Unlock()
			if s.logger != nil {
				if canceled {
					s.logger.Info("Agent run canceled: request_id=%s", requestID)
				} else {
					s.logger.Error("Agent run failed: request_id=%s error=%v", requestID, err)
				}
			}
			if !canceled && !result.SpeechStreamed && s.canSpeakFinalText() {
				prepared := s.runtime.PrepareSpokenText(runCtx, SpokenTextInput{TurnFailure: result.TurnFailure})
				s.speakFinalText(runCtx, requestID, prepared)
			}
			return
		}

		if s.liveActivity != nil {
			s.liveActivity.CompleteTask(requestID, result.Output)
		}
		pending.done = true
		pending.mu.Unlock()

		if s.logger != nil {
			s.logger.Info("Chat request completed: request_id=%s output_len=%d", requestID, len(result.Output))
		}

		// Keep request-scoped final TTS inside the async run goroutine so Stop can
		// still see the active run/output and interrupt it reliably.
		speechText := result.SpokenTextForConfig(s.runtime.ConfigSnapshot())
		if s.canSpeakFinalText() && speechText != "" && !result.SpeechStreamed {
			prepared := s.runtime.PrepareSpokenText(runCtx, SpokenTextInput{ResponseText: speechText, TailAppendable: true})
			s.speakFinalText(runCtx, requestID, prepared)
		}
	}()
}

// handleChatResult serves incremental results for an async chat request.
// GET /api/chat/result?request_id=xxx&offset=N
//
// When the agent is still running, returns:
//
//	{"status":"running","messages":[...messages since offset...]}
//
// When complete, returns:
//
//	{"status":"complete","response":"...","history":[...]}
func (s *Server) handleChatResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	requestID := r.URL.Query().Get("request_id")
	if requestID == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}

	offset := 0
	if offStr := r.URL.Query().Get("offset"); offStr != "" {
		n, err := strconv.Atoi(offStr)
		if err != nil || n < 0 {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		offset = n
	}

	// Long polling: wait=true blocks until task completes or context is canceled.
	waitMode := r.URL.Query().Get("wait") == "true"

	// Add server-side timeout for long polling to prevent indefinite blocking.
	var loopCtx context.Context
	var cancel context.CancelFunc
	if waitMode {
		loopCtx, cancel = context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
	} else {
		loopCtx = r.Context()
	}

	for {
		s.pendingResultsMu.Lock()
		pending := s.pendingResults[requestID]
		s.pendingResultsMu.Unlock()

		if pending == nil {
			liveActivity := s.liveActivitySnapshot(requestID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResultResponse{Status: "not_found", LiveActivity: liveActivity})
			return
		}

		pending.mu.Lock()
		errText := pending.err
		done := pending.done

		msgs := pending.messages
		if offset < len(msgs) {
			msgs = append([]Message(nil), msgs[offset:]...)
		} else {
			msgs = nil
		}

		var historySnapshot []Message
		response := ""
		if done {
			historySnapshot = s.webHistorySnapshot()
			for i := len(historySnapshot) - 1; i >= 0; i-- {
				if historySnapshot[i].Type == "assistant" {
					response = historySnapshot[i].Content
					break
				}
			}
		}
		pending.mu.Unlock()

		liveActivity := s.liveActivitySnapshot(requestID)

		if errText != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResultResponse{
				Status:       "error",
				Messages:     msgs,
				Error:        errText,
				LiveActivity: liveActivity,
			})
			return
		}

		if done {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResultResponse{
				Status:       "complete",
				Response:     response,
				History:      historySnapshot,
				Messages:     msgs,
				LiveActivity: liveActivity,
			})
			return
		}

		// Non-wait mode: return running status immediately.
		if !waitMode {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResultResponse{
				Status:       "running",
				Messages:     msgs,
				LiveActivity: liveActivity,
			})
			return
		}

		// Wait mode: sleep briefly then re-check.
		select {
		case <-loopCtx.Done():
			if errors.Is(loopCtx.Err(), context.DeadlineExceeded) {
				http.Error(w, "long poll timeout", http.StatusRequestTimeout)
			} else {
				http.Error(w, "client disconnected", http.StatusRequestTimeout)
			}
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *Server) liveActivitySnapshot(requestID string) *LiveActivityState {
	if s.liveActivity == nil {
		return nil
	}
	return s.liveActivity.Snapshot(requestID)
}

func (s *Server) liveActivityPhoneID(req ChatRequest) string {
	if phoneID := strings.TrimSpace(req.PhoneID); phoneID != "" {
		return phoneID
	}
	if s != nil && s.bridge != nil {
		if phoneID := strings.TrimSpace(s.bridge.getStatus().PhoneID); phoneID != "" {
			return phoneID
		}
	}
	return ""
}

func (s *Server) bridgeEnvironment() *PhoneEnvironment {
	if s == nil || s.bridge == nil {
		if s != nil && s.runtime != nil && s.runtime.toolSnapshot() != nil {
			s.runtime.toolSnapshot().UpdateDeviceEnvironment(nil)
		}
		return nil
	}
	status := s.bridge.getStatus()
	if status.Environment == nil {
		if s.runtime != nil && s.runtime.toolSnapshot() != nil {
			s.runtime.toolSnapshot().UpdateDeviceEnvironment(nil)
		}
		return nil
	}
	env := clonePhoneEnvironment(*status.Environment)
	if s.runtime != nil && s.runtime.toolSnapshot() != nil {
		s.runtime.toolSnapshot().UpdateDeviceEnvironment(&env)
	}
	return &env
}

func wantsChatStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") ||
		strings.EqualFold(r.Header.Get("X-Aiden-Stream"), "ndjson") ||
		r.URL.Query().Get("stream") == "1"
}

// handleChatStream registers the run independently of the HTTP connection so
// an explicit cancellation request remains the only operation that stops it.
func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		req.RequestID = createRequestID()
	}

	turnInput, historyAttachments, err := s.resolveRequestInput(req)
	if err != nil {
		http.Error(w, "Invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if handler := s.realtimeChatHandlerSnapshot(); handler != nil {
		if len(turnInput.Attachments) > 0 {
			http.Error(w, "realtime chat supports text messages only", http.StatusBadRequest)
			return
		}
		s.handleRealtimeChatStream(w, r, req, turnInput)
		return
	}
	inputText := turnInput.InputText

	stream, ok := newChatStreamWriter(w)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	episodeID := s.runtime.NewEpisodeID()
	userMessage := messageFromTurnInput(turnInput, episodeID, req.RequestID, historyAttachments, time.Now())

	if s.logger != nil {
		s.logger.Info("Chat request: %s attachments=%d stream=ndjson", inputText, len(turnInput.Attachments))
	}

	s.playPromptSoundAsync(promptSoundAgentSend, "agent send")

	// A request_id marks a resumable run. Keep it alive if the streaming
	// HTTP client disconnects; only /api/chat/cancel should stop it.
	runCtx, cancel := context.WithCancel(context.Background())
	if !s.registerActiveRun(req.RequestID, cancel) {
		s.logger.Error("Request ID already in use: %s", req.RequestID)
		cancel()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	var cleanupOnce sync.Once
	cleanupRun := func() {
		cleanupOnce.Do(func() {
			s.unregisterActiveRun(req.RequestID)
			s.clearSteerState(req.RequestID)
			cancel()
		})
	}
	ctx := runCtx
	cleanupRunAtReturn := true
	defer func() {
		if cleanupRunAtReturn {
			s.logger.Info("Cleaning up run at return")
			cleanupRun()
		}
	}()
	s.logger.Info("Appending history")
	s.publishMessage(userMessage)
	if s.liveActivity != nil {
		s.logger.Info("Starting live activity")
		s.liveActivity.StartTask(req.RequestID, inputText, s.liveActivityPhoneID(req))
	}

	steerProvider, steerInterrupt := s.prepareRunSteerCallbacks(req.RequestID)
	s.logger.Info("Creating run request")
	var speechWriter *speech.StreamWriter
	runReq := RunRequest{
		Input:                   inputText,
		Attachments:             turnInput.Attachments,
		Turn:                    turnInput,
		EpisodeID:               episodeID,
		RequestID:               req.RequestID,
		DeviceEnvironment:       s.bridgeEnvironment(),
		AsyncEpisodeMaintenance: true,
		SteerProvider:           steerProvider,
		SteerInterrupt:          steerInterrupt,
		EventHandler: func(event RunEvent) {
			if event.Type == runEventReasoningDelta || event.Type == runEventReasoningReset {
				stream.Write(ChatStreamEvent{
					Type:      map[string]string{runEventReasoningDelta: "assistant_reasoning_delta", runEventReasoningReset: "assistant_reasoning_reset"}[event.Type],
					RequestID: req.RequestID,
					EpisodeID: episodeID,
					// Reasoning delta events carry only the newly received chunk;
					// clients append it to their streaming draft.
					ReasoningContent: event.ReasoningContent,
				})
				if s.liveActivity != nil {
					s.liveActivity.UpdateFromRunEvent(req.RequestID, event)
				}
				return
			}
			message := messageFromRunEvent(event, episodeID, req.RequestID)
			if message.Type != "" {
				if message, ok := s.publishMessage(message); ok {
					stream.Write(ChatStreamEvent{Type: "message", Message: &message})
				}
			}
			if s.liveActivity != nil {
				s.liveActivity.UpdateFromRunEvent(req.RequestID, event)
			}
			toolSpeechStreamed := event.Type == runEventToolCall && speechWriter.FinalizeResponse()
			if toolSpeechStreamed && s.logger != nil {
				s.logger.Info("Tool content TTS already streamed: tool=%s", event.ToolName)
			}
			if text := s.toolCallSpeechText(event); text != "" && !toolSpeechStreamed {
				go s.speakToolContent(ctx, text)
			}
		},
	}

	var newStream *streamSessionWriter
	unregisterStreamOutput := func() {}
	ttsManager := s.currentTTSManager()
	cfg := s.runtime.ConfigSnapshot()
	streamWriters := []io.Writer{
		newChatAssistantStreamWriter(stream, episodeID, req.RequestID),
	}
	if cfg.VoiceStreamingTTSEnabledOrDefault() && s.currentTTSPlaybackBackend() != nil {
		s.logger.Info("Starting TTS stream")
		if ttsManager != nil {
			streamSession, err := beginManagedTTSStream(ctx, ttsManager, s.currentTTSPlaybackBackend(), cfg)
			if err != nil {
				if s.logger != nil {
					s.logger.Warn("TTS BeginStream failed: %v", err)
				}
			} else {
				newStream = streamSession
				output := newActiveTTSOutput(nil)
				output.setStream(newStream)
				runReq.OnRunActive = func(context.Context) {
					unregisterStreamOutput = s.registerActiveOutput(req.RequestID, output)
				}
				speechWriter = speech.NewStreamWriter(newStream)
				streamWriters = append(streamWriters, speechWriter)
			}
		}
	}
	defer func() {
		unregisterStreamOutput()
	}()
	s.logger.Info("Creating stream writer")
	runReq.StreamWriter = newStreamFanoutWriter(streamWriters...)

	s.logger.Info("Running runtime")
	result, err := s.runtime.Run(ctx, runReq)
	if newStream != nil {
		finalSpeechStreamed := speechWriter.FinalizeResponse()
		closeErr := newStream.closeAndWait()
		if closeErr != nil && s.logger != nil {
			s.logger.Error("new TTS stream failed: %v", closeErr)
		}
		result.SpeechStreamed = finalSpeechStreamed && newStream.emittedSpeech(closeErr)
	}
	if err != nil {
		canceled := errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled)
		if s.liveActivity != nil {
			if canceled {
				s.liveActivity.CancelTask(req.RequestID)
			} else {
				s.liveActivity.FailTask(req.RequestID, err.Error())
			}
		}
		if canceled {
			if s.logger != nil {
				s.logger.Info("Agent run canceled: request_id=%s", req.RequestID)
			}
			stream.Write(ChatStreamEvent{Type: "error", Error: "request canceled", History: s.webHistorySnapshot()})
			return
		}
		errorMessage := Message{
			Type:      "episode_status",
			EpisodeID: episodeID,
			RequestID: req.RequestID,
			Status:    "failed",
			Content:   "Agent error: " + err.Error(),
			Timestamp: time.Now(),
			IsError:   true,
		}
		if errorMessage, ok := s.publishMessage(errorMessage); ok {
			stream.Write(ChatStreamEvent{Type: "message", Message: &errorMessage})
		}
		if s.logger != nil {
			s.logger.Error("Agent run failed: %v", err)
		}
		if !result.SpeechStreamed && s.canSpeakFinalText() {
			prepared := s.runtime.PrepareSpokenText(ctx, SpokenTextInput{TurnFailure: result.TurnFailure})
			s.speakFinalText(ctx, req.RequestID, prepared)
		}
		stream.Write(ChatStreamEvent{Type: "error", Error: err.Error(), History: s.webHistorySnapshot()})
		return
	}

	historySnapshot := s.webHistorySnapshot()
	if s.liveActivity != nil {
		s.liveActivity.CompleteTask(req.RequestID, result.Output)
	}

	speechText := result.SpokenTextForConfig(s.runtime.ConfigSnapshot())
	if s.canSpeakFinalText() && speechText != "" && !result.SpeechStreamed {
		prepared := s.runtime.PrepareSpokenText(ctx, SpokenTextInput{ResponseText: speechText, TailAppendable: true})
		// Let request-scoped final TTS outlive the streaming handler while
		// /api/chat/cancel can still interrupt it via the active run/output.
		finalTTSCtx := context.Background()
		finishRun := cleanupRun
		cleanupRunAtReturn = false
		go func(prepared SpokenTextResult, ttsCtx context.Context, finish func()) {
			defer finish()
			s.speakFinalText(ttsCtx, req.RequestID, prepared)
		}(prepared, finalTTSCtx, finishRun)
	}

	stream.Write(ChatStreamEvent{
		Type:     "done",
		Response: result.Output,
		History:  historySnapshot,
	})
}

type chatStreamWriter struct {
	encoder *json.Encoder
	flusher http.Flusher
	mu      sync.Mutex
}

func newChatStreamWriter(w http.ResponseWriter) (*chatStreamWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	header := w.Header()
	header.Set("Content-Type", "application/x-ndjson")
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Accel-Buffering", "no")
	return &chatStreamWriter{
		encoder: json.NewEncoder(w),
		flusher: flusher,
	}, true
}

func (w *chatStreamWriter) Write(event ChatStreamEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.encoder.Encode(event); err == nil {
		w.flusher.Flush()
	}
}

type chatAssistantDeltaWriter struct {
	stream    *chatStreamWriter
	episodeID string
	requestID string

	mu      sync.Mutex
	emitted bool
}

func newChatAssistantDeltaWriter(stream *chatStreamWriter, episodeID, requestID string) *chatAssistantDeltaWriter {
	return &chatAssistantDeltaWriter{
		stream:    stream,
		episodeID: episodeID,
		requestID: requestID,
	}
}

func (w *chatAssistantDeltaWriter) Write(p []byte) (int, error) {
	if w == nil || w.stream == nil || len(p) == 0 {
		return len(p), nil
	}
	delta := string(p)
	w.mu.Lock()
	w.emitted = true
	w.mu.Unlock()
	w.stream.Write(ChatStreamEvent{
		Type:      "assistant_delta",
		RequestID: w.requestID,
		EpisodeID: w.episodeID,
		Delta:     delta,
	})
	return len(p), nil
}

func (w *chatAssistantDeltaWriter) ResetStreamState() {
	if w == nil {
		return
	}
	w.mu.Lock()
	emitted := w.emitted
	w.emitted = false
	w.mu.Unlock()
	if emitted && w.stream != nil {
		w.stream.Write(ChatStreamEvent{
			Type:      "assistant_delta_reset",
			RequestID: w.requestID,
			EpisodeID: w.episodeID,
		})
	}
}

func (w *chatAssistantDeltaWriter) StreamEmitted() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.emitted
}

type chatAssistantStreamWriter struct {
	delta *chatAssistantDeltaWriter
}

func newChatAssistantStreamWriter(stream *chatStreamWriter, episodeID, requestID string) io.Writer {
	delta := newChatAssistantDeltaWriter(stream, episodeID, requestID)
	return &chatAssistantStreamWriter{
		delta: delta,
	}
}

func (w *chatAssistantStreamWriter) Write(p []byte) (int, error) {
	if w == nil || w.delta == nil {
		return len(p), nil
	}
	return w.delta.Write(p)
}

func (w *chatAssistantStreamWriter) ResetStreamState() {
	if w == nil {
		return
	}
	if w.delta != nil {
		w.delta.ResetStreamState()
	}
}

func (w *chatAssistantStreamWriter) StreamEmitted() bool {
	if w == nil || w.delta == nil {
		return false
	}
	return w.delta.StreamEmitted()
}

type streamFanoutWriter struct {
	writers []io.Writer
	mu      sync.Mutex
	emitted bool
}

func newStreamFanoutWriter(writers ...io.Writer) io.Writer {
	filtered := make([]io.Writer, 0, len(writers))
	for _, writer := range writers {
		if writer != nil {
			filtered = append(filtered, writer)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return &streamFanoutWriter{writers: filtered}
}

func (w *streamFanoutWriter) Write(p []byte) (int, error) {
	if w == nil || len(p) == 0 {
		return len(p), nil
	}
	emitted := false
	delivered := false
	for _, writer := range w.writers {
		if writer == nil {
			continue
		}
		n, err := writer.Write(p)
		if streamWriterEmitted(writer) {
			emitted = true
		}
		if err != nil {
			if emitted {
				w.mu.Lock()
				w.emitted = true
				w.mu.Unlock()
			}
			if delivered {
				return len(p), err
			}
			return n, err
		}
		if n == len(p) {
			delivered = true
		}
	}
	if emitted {
		w.mu.Lock()
		w.emitted = true
		w.mu.Unlock()
	}
	return len(p), nil
}

func (w *streamFanoutWriter) ResetStreamState() {
	if w == nil {
		return
	}
	for _, writer := range w.writers {
		if resetter, ok := writer.(streamStateResetter); ok {
			resetter.ResetStreamState()
		}
	}
	w.mu.Lock()
	w.emitted = false
	w.mu.Unlock()
}

// ResetBuffer forwards a buffer reset to every fanned-out writer that supports
// it (e.g. the TTS stream writer), so residual buffered speech does not leak
// across turns on the streaming chat path.
func (w *streamFanoutWriter) ResetBuffer() {
	if w == nil {
		return
	}
	for _, writer := range w.writers {
		if resetter, ok := writer.(ttsBufferResetter); ok {
			resetter.ResetBuffer()
		}
	}
}

func (w *streamFanoutWriter) FinishResponse() bool {
	if w == nil {
		return false
	}
	emitted := false
	for _, writer := range w.writers {
		if finishStreamWriterResponse(writer) {
			emitted = true
		}
	}
	return emitted
}

func (w *streamFanoutWriter) StreamEmitted() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	emitted := w.emitted
	w.mu.Unlock()
	if emitted {
		return true
	}
	for _, writer := range w.writers {
		if streamWriterEmitted(writer) {
			return true
		}
	}
	return false
}

func (s *Server) speakToolContent(ctx context.Context, content string) {
	content = strings.TrimSpace(content)
	if content == "" || s.currentTTSPlaybackBackend() == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ttsCtx, cancel := context.WithTimeout(ctx, toolContentSpeechTimeout)
	defer cancel()
	if s.logger != nil {
		s.logger.Info("Tool content TTS playback: %q", content)
	}
	if err := s.speakText(ttsCtx, content, 0); err != nil {
		if s.logger != nil {
			s.logger.Error("Tool content TTS playback failed: %v", err)
		}
	}
}

func (s *Server) shouldSpeakToolCall(event RunEvent) bool {
	return s.toolCallSpeechText(event) != ""
}

func (s *Server) toolCallSpeechText(event RunEvent) string {
	if event.Type != runEventToolCall || event.ToolName == toolWaitForWakeup {
		return ""
	}
	if s.runtime == nil || !s.runtime.ConfigSnapshot().VoiceToolCallSpeechOrDefault() {
		return ""
	}
	return speech.BuildText(event.Content)
}

func (s *Server) ttsProviderManager() *tts.ProviderManager {
	manager := s.ttsManager
	if manager == nil && s.runtime != nil {
		manager = s.runtime.ttsProviderManager()
	}
	return manager
}

func (s *Server) currentTTSManager() *tts.ProviderManager {
	manager := s.ttsProviderManager()
	if manager == nil || manager.Current() == "" {
		return nil
	}
	return manager
}

func (s *Server) currentTTSPlaybackBackend() tts.AudioServiceBackend {
	if s == nil {
		return nil
	}
	s.configDepsMu.RLock()
	defer s.configDepsMu.RUnlock()
	if s.ttsPlaybackBackend != nil {
		if backend, ok := s.ttsPlaybackBackend.(*audioBackend); ok && s.audioClient != nil && backend.c != s.audioClient {
			return newAudioBackend(s.audioClient)
		}
		return s.ttsPlaybackBackend
	}
	if s.audioClient == nil {
		return nil
	}
	return newAudioBackend(s.audioClient)
}

func (s *Server) canSpeakFinalText() bool {
	if s == nil || s.currentTTSPlaybackBackend() == nil {
		return false
	}
	if s.currentTTSManager() != nil {
		return true
	}
	if s.runtime == nil {
		return false
	}
	return canPlayTTSUnavailableFallback(s.runtime.ConfigSnapshot(), "")
}

// CanSpeakVoiceNotification reports whether a standalone speech path is
// available while no Realtime session is consuming notifications. When
// allowFallbackClip is set, the prerecorded TTS-unavailable clip also counts:
// it is the only audible signal left once the TTS provider cannot be used.
func (s *Server) CanSpeakVoiceNotification(allowFallbackClip bool) bool {
	if s == nil || s.currentTTSPlaybackBackend() == nil {
		return false
	}
	if s.currentTTSManager() != nil {
		return true
	}
	return allowFallbackClip && s.runtime != nil && canPlayTTSUnavailableFallback(s.runtime.ConfigSnapshot(), "")
}

// SpeakVoiceNotification plays notification text through the configured
// standalone TTS provider for an idle period without an active Realtime
// session. When TTS is unavailable and allowFallbackClip is set, the
// prerecorded clip plays instead and the first return value reports that. The
// clip only announces the failure and does not carry the notification text, so
// the returned error stays non-nil and callers must not count it as delivery.
func (s *Server) SpeakVoiceNotification(ctx context.Context, text string, allowFallbackClip bool) (bool, error) {
	if !s.CanSpeakVoiceNotification(allowFallbackClip) {
		return false, errStandaloneTTSUnavailable
	}
	return s.speakTextObservedMode(ctx, "", text, 0, false, allowFallbackClip)
}

// SpeakTurnFailure announces a failed turn through the standalone speech path,
// mirroring what the stt dialog does with SpokenTextModeReplacement. A Realtime
// session that failed cannot report its own failure, so this is the only
// audible signal left; when TTS is also unavailable the prerecorded clip plays
// and the first return value reports that.
func (s *Server) SpeakTurnFailure(ctx context.Context, failure *TurnFailure) (bool, error) {
	if s == nil || failure == nil || s.runtime == nil {
		return false, nil
	}
	if !s.CanSpeakVoiceNotification(true) {
		return false, errStandaloneTTSUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := s.runtime.PrepareSpokenText(ctx, SpokenTextInput{TurnFailure: failure})
	text := strings.TrimSpace(prepared.Text)
	if text == "" {
		return false, nil
	}
	return s.speakTextObservedModeWithFailure(ctx, "", text, 0, false, true, failure.Code)
}

func (s *Server) speakFinalText(ctx context.Context, requestID string, prepared SpokenTextResult) {
	if strings.TrimSpace(prepared.Text) == "" {
		return
	}
	if s.logger != nil {
		s.logger.Info("TTS playback: %q", prepared.Text)
	}
	fallbackPlayed, err := s.speakFinalTextForRequest(ctx, requestID, prepared.Text, 0)
	s.runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
	if err != nil && s.logger != nil {
		if fallbackPlayed {
			s.logger.Warn("TTS playback failed; local fallback played: %v", err)
		} else {
			s.logger.Error("TTS playback failed: %v", err)
		}
	}
}

func (s *Server) speakTextObserved(ctx context.Context, requestID, text string, timeoutAfterLock time.Duration, registerOutput bool) error {
	_, err := s.speakTextObservedMode(ctx, requestID, text, timeoutAfterLock, registerOutput, false)
	return err
}

func (s *Server) speakTextObservedMode(ctx context.Context, requestID, text string, timeoutAfterLock time.Duration, registerOutput, allowFallback bool) (bool, error) {
	return s.speakTextObservedModeWithFailure(ctx, requestID, text, timeoutAfterLock, registerOutput, allowFallback, "")
}

func (s *Server) speakTextObservedModeWithFailure(ctx context.Context, requestID, text string, timeoutAfterLock time.Duration, registerOutput, allowFallback bool, failureCode TurnFailureCode) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	manager := s.currentTTSManager()
	if manager == nil && !allowFallback {
		return false, nil
	}
	cfg := Config{}
	if s.runtime != nil {
		cfg = s.runtime.ConfigSnapshot()
	}
	outputCtx, cancelOutput := context.WithCancel(ctx)
	output := newActiveTTSOutput(cancelOutput)
	unregisterOutput := func() {}
	if registerOutput {
		unregisterOutput = s.registerActiveOutput(requestID, output)
	}
	defer func() {
		unregisterOutput()
		output.finish()
		cancelOutput()
	}()
	if !registerOutput {
		go func() {
			<-outputCtx.Done()
			output.interrupt()
		}()
	}
	if err := outputCtx.Err(); err != nil {
		return false, err
	}
	speakCtx := outputCtx
	cancelTimeout := func() {}
	if timeoutAfterLock > 0 {
		speakCtx, cancelTimeout = context.WithTimeout(outputCtx, timeoutAfterLock)
		defer cancelTimeout()
	}
	if err := speakCtx.Err(); err != nil {
		return false, err
	}

	speechStarted := false
	ttsErr := errTTSNotConfigured
	if manager != nil {
		speechStarted, ttsErr = speakWithTTSManagerObserved(speakCtx, manager, s.currentTTSPlaybackBackend(), cfg, text, func(stream *streamSessionWriter) func() {
			stream.setCancel(cancelOutput)
			output.setStream(stream)
			return func() {
				output.clearStream(stream)
			}
		})
		if ttsErr == nil && !speechStarted && allowFallback {
			ttsErr = errTTSNoAudio
		}
	}
	if ttsErr == nil || !allowFallback {
		return false, ttsErr
	}
	return attemptTTSUnavailableFallback(speakCtx, s.currentTTSPlaybackBackend(), cfg, failureCode, speechStarted, ttsErr)
}

func (s *Server) speakText(ctx context.Context, text string, timeoutAfterLock time.Duration) error {
	return s.speakTextObserved(ctx, "", text, timeoutAfterLock, false)
}

func (s *Server) speakTextForRequest(ctx context.Context, requestID string, text string, timeoutAfterLock time.Duration) error {
	if requestID == "" {
		return s.speakText(ctx, text, timeoutAfterLock)
	}
	if s.isRequestTerminated(requestID) {
		return context.Canceled
	}
	return s.speakTextObserved(ctx, requestID, text, timeoutAfterLock, true)
}

func (s *Server) speakFinalTextForRequest(ctx context.Context, requestID string, text string, timeoutAfterLock time.Duration) (bool, error) {
	if requestID == "" {
		return s.speakTextObservedMode(ctx, "", text, timeoutAfterLock, false, true)
	}
	if s.isRequestTerminated(requestID) {
		return false, context.Canceled
	}
	return s.speakTextObservedMode(ctx, requestID, text, timeoutAfterLock, true, true)
}

func (s *Server) playPromptSoundAsync(kind promptSoundKind, label string) {
	s.logger.Info("Playing prompt sound: %v, %s", kind, label)
	if s.currentTTSPlaybackBackend() == nil {
		return
	}
	go func() {
		if err := playPromptSound(context.Background(), s.currentTTSPlaybackBackend(), kind, false); err != nil && s.logger != nil {
			s.logger.Error("%s prompt sound failed: %v", label, err)
		}
	}()
}

// handleHistory returns the conversation history
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	historySnapshot := s.webHistorySnapshot()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(historySnapshot)
}

type ContextResponse struct {
	User    contextmanager.MessageListDump `json:"user"`
	Backend contextmanager.MessageListDump `json:"backend"`
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ContextResponse{
		User:    s.runtime.UserContextDump(),
		Backend: s.runtime.ContextDump(),
	})
}

func (s *Server) handleContextAttachment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	role := r.URL.Query().Get("role")
	attachmentID := r.URL.Query().Get("attachment")
	data, mimeType, err := s.runtime.ReadContextAttachment(role, attachmentID)
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Check if response writer supports flushing
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Subscribe to events
	ch := s.eventBroadcaster.Subscribe()
	defer s.eventBroadcaster.Unsubscribe(ch)

	// Send initial connection event
	fmt.Fprintf(w, "data: {\"type\":\"connected\"}\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			return
		case msg, ok := <-ch:
			if !ok {
				// Channel closed (server shutdown)
				return
			}

			if !shouldStreamEventMessage(msg) {
				continue
			}

			data, err := json.Marshal(msg)
			if err != nil {
				if s.logger != nil {
					s.logger.Warn("[sse] marshal error: %v", err)
				}
				continue
			}

			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func shouldStreamEventMessage(msg Message) bool {
	_, ok := normalizePublicMessage(msg)
	return ok
}

func (s *Server) handleEpisodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	store := s.episodeStore
	if store == nil && s.runtime != nil {
		if configDir := s.runtime.ConfigSnapshot().ConfigDir; configDir != "" {
			store = NewTaskEpisodeStore(filepath.Join(configDir, "memory", "episodes"))
		}
	}
	if store == nil {
		http.Error(w, "Episode store is not configured", http.StatusNotFound)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/episodes/")
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "Invalid episode id", http.StatusBadRequest)
		return
	}
	episode, err := store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(EpisodeResponse{Episode: episode})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"data": map[string]bool{
			"setup": false,
		},
	})
}

type benchmarkSeedMemoryRequest struct {
	Store        string            `json:"store"`
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Title        string            `json:"title"`
	Content      string            `json:"content"`
	Tags         []string          `json:"tags"`
	Entities     []string          `json:"entities"`
	Evidence     []string          `json:"evidence"`
	SourceRefs   []MemorySourceRef `json:"source_refs"`
	EvidenceRefs []MemorySourceRef `json:"evidence_refs"`
	ExpiresAt    string            `json:"expires_at"`
	Priority     int               `json:"priority"`
}

type benchmarkSeedSessionChunkRequest struct {
	SessionID string             `json:"session_id"`
	Summary   string             `json:"summary"`
	Messages  []messages.Message `json:"messages"`
}

func (s *Server) handleBenchmarkSeedEpisode(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var episode TaskEpisode
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&episode); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "decode body: expected exactly one JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(episode.ID) == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(episode.UserGoal) == "" {
		http.Error(w, "user_goal is required", http.StatusBadRequest)
		return
	}
	if strings.EqualFold(strings.TrimSpace(episode.Status), "running") {
		http.Error(w, "episode must be completed", http.StatusBadRequest)
		return
	}
	store := s.episodeStore
	if store == nil {
		if plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane); ok && plane != nil {
			store = plane.episodes
		}
	}
	if store == nil {
		http.Error(w, "episode store is not configured", http.StatusServiceUnavailable)
		return
	}
	id, err := store.AddEpisode(r.Context(), episode)
	if err != nil {
		http.Error(w, "seed episode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "seeded",
		"id":     id,
	})
}

func (s *Server) handleBenchmarkSeedSessionChunk(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req benchmarkSeedSessionChunkRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, benchmarkSeedSessionChunkMaxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "decode body: expected exactly one JSON object", http.StatusBadRequest)
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Summary = strings.TrimSpace(req.Summary)
	if req.SessionID == "" {
		http.Error(w, "session_id is required", http.StatusBadRequest)
		return
	}
	if req.Summary == "" {
		http.Error(w, "summary is required", http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages are required", http.StatusBadRequest)
		return
	}
	for index, msg := range req.Messages {
		if msg.Role == "" {
			http.Error(w, fmt.Sprintf("messages[%d].role is required", index), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(msg.Content) == "" {
			http.Error(w, fmt.Sprintf("messages[%d].content is required", index), http.StatusBadRequest)
			return
		}
	}
	if s.runtime == nil {
		http.Error(w, "runtime is not configured", http.StatusServiceUnavailable)
		return
	}
	configDir := s.runtime.ConfigSnapshot().ConfigDir
	if strings.TrimSpace(configDir) == "" {
		http.Error(w, "config dir is not configured", http.StatusServiceUnavailable)
		return
	}
	writer := NewSessionChunkWriter(
		agentpath.ContextManagerSessionFolder(configDir),
		LoadMemoryExtractionConfig(configDir),
	)
	if err := writer.WriteChunk(r.Context(), req.SessionID, req.Messages, req.Summary); err != nil {
		http.Error(w, "seed session chunk: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":     "seeded",
		"session_id": req.SessionID,
	})
}

type benchmarkProcessEpisodeMemoryRequest struct {
	EpisodeID string `json:"episode_id"`
}

type benchmarkProcessEpisodeMemoryResponse struct {
	EpisodeID  string                   `json:"episode_id"`
	Status     string                   `json:"status"`
	Assessment *episodeMemoryAssessment `json:"assessment,omitempty"`
	MemoryIDs  []string                 `json:"memory_ids"`
}

type benchmarkSeedNotificationRequest struct {
	Events []ble.NotificationEvent `json:"events"`
}

type benchmarkProcessNotificationMemoryResponse struct {
	MemoryCursor string   `json:"memory_cursor"`
	MemoryIDs    []string `json:"memory_ids"`
}

func (s *Server) handleBenchmarkSeedNotification(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req benchmarkSeedNotificationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "decode notification fixture: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "decode notification fixture: expected exactly one JSON object", http.StatusBadRequest)
		return
	}
	if len(req.Events) == 0 || len(req.Events) > 100 {
		http.Error(w, "events must contain between 1 and 100 entries", http.StatusBadRequest)
		return
	}
	for index, event := range req.Events {
		if strings.TrimSpace(event.Title) == "" && strings.TrimSpace(event.Message) == "" {
			http.Error(w, fmt.Sprintf("events[%d] requires a title or message", index), http.StatusBadRequest)
			return
		}
	}
	plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane)
	if !ok || plane == nil {
		http.Error(w, "filesystem memory plane not configured", http.StatusServiceUnavailable)
		return
	}
	records, err := plane.SeedNotificationMemoryForBenchmark(r.Context(), req.Events)
	if err != nil {
		http.Error(w, "seed notification: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ContextID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "seeded",
		"context_ids": ids,
		"event_count": len(records),
	})
}

func (s *Server) handleBenchmarkProcessNotificationMemory(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane)
	if !ok || plane == nil {
		http.Error(w, "filesystem memory plane not configured", http.StatusServiceUnavailable)
		return
	}
	cursor, memoryIDs, err := plane.ProcessNotificationMemoryNow(r.Context())
	if err != nil {
		code := http.StatusInternalServerError
		var proposalErr *notificationProposalError
		if errors.As(err, &proposalErr) {
			code = http.StatusUnprocessableEntity
		} else if errors.Is(err, errMemoryWorkerBusy) {
			code = http.StatusConflict
		}
		http.Error(w, "process notification memory: "+err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(benchmarkProcessNotificationMemoryResponse{
		MemoryCursor: cursor,
		MemoryIDs:    memoryIDs,
	})
}

func (s *Server) handleBenchmarkProcessEpisodeMemory(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req benchmarkProcessEpisodeMemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.EpisodeID = strings.TrimSpace(req.EpisodeID)
	if req.EpisodeID == "" {
		http.Error(w, "episode_id is required", http.StatusBadRequest)
		return
	}
	plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane)
	if !ok || plane == nil {
		http.Error(w, "episode memory is not configured", http.StatusServiceUnavailable)
		return
	}
	status, memoryIDs, err := plane.ProcessEpisodeMemoryNow(r.Context(), req.EpisodeID)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errMemoryWorkerBusy) {
			code = http.StatusConflict
		}
		http.Error(w, "process episode memory: "+err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(benchmarkProcessEpisodeMemoryResponse{
		EpisodeID:  req.EpisodeID,
		Status:     string(status.Status),
		Assessment: status.Assessment,
		MemoryIDs:  memoryIDs,
	})
}

func (s *Server) handleBenchmarkSeedMemory(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req benchmarkSeedMemoryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "decode body: expected exactly one JSON object", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		http.Error(w, "content is required", http.StatusBadRequest)
		return
	}
	plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane)
	if !ok || plane == nil {
		http.Error(w, "filesystem memory plane not configured", http.StatusServiceUnavailable)
		return
	}
	storeName := strings.ToLower(strings.TrimSpace(req.Store))
	if storeName == "" {
		storeName = "long_term"
	}
	if storeName != "long_term" && storeName != "temporary" && storeName != "device" {
		http.Error(w, "store must be long_term, temporary, or device", http.StatusBadRequest)
		return
	}
	priority := req.Priority
	if priority <= 0 {
		priority = 80
	}
	evidence := req.Evidence
	if len(evidence) == 0 {
		evidence = []string{req.Content}
	}
	var (
		id  string
		err error
	)
	if storeName == "device" {
		if plane.device == nil {
			http.Error(w, "device memory not configured", http.StatusServiceUnavailable)
			return
		}
		id, err = plane.device.Upsert(r.Context(), DeviceMemoryItem{
			ID:         req.ID,
			Type:       req.Type,
			Status:     deviceMemoryStatusActive,
			Priority:   priority,
			Confidence: 0.9,
			Title:      req.Title,
			Summary:    req.Title,
			Content:    req.Content,
			DeviceID:   defaultMemoryDeviceID,
			Tags:       req.Tags,
			Entities:   req.Entities,
		})
	} else {
		store := plane.LongTerm()
		memoryScope := "long_term"
		if storeName == "temporary" {
			store = NewLongTermMemoryStore(filepath.Join(plane.memoryDir, "temporary"))
			memoryScope = "temporary"
		}
		if store == nil {
			http.Error(w, "memory store not configured", http.StatusServiceUnavailable)
			return
		}
		item := MemoryItem{
			ID:               req.ID,
			Type:             req.Type,
			TimeScope:        memoryScope,
			Priority:         priority,
			Confidence:       0.9,
			Title:            req.Title,
			Content:          req.Content,
			Tags:             req.Tags,
			Entities:         req.Entities,
			SourceRefs:       req.SourceRefs,
			EvidenceRefs:     req.EvidenceRefs,
			ExpiresAt:        req.ExpiresAt,
			EvidenceExcerpts: evidence,
		}
		id, err = store.AddMemory(r.Context(), item)
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Error("seed_memory AddMemory failed: %v", err)
		}
		http.Error(w, "seed memory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "seeded",
		"id":     id,
		"store":  storeName,
	})
}

func (s *Server) handleBenchmarkPhoneBridgeState(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBenchmarkRequest(r) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.bridge == nil {
		http.Error(w, "phone bridge is not configured", http.StatusServiceUnavailable)
		return
	}
	var status PhoneBridgeStatus
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	if err := decoder.Decode(&status); err != nil {
		http.Error(w, "decode phone bridge state: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.bridge.ApplyBenchmarkStatus(status); err != nil {
		http.Error(w, "apply phone bridge state: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"status": s.bridge.getStatus(),
	})
}

func (s *Server) benchmarkToken() string {
	if s == nil || s.runtime == nil {
		return ""
	}
	return strings.TrimSpace(s.runtime.ConfigSnapshot().Benchmark.Token)
}

func (s *Server) authorizeBenchmarkRequest(r *http.Request) bool {
	expected := s.benchmarkToken()
	if expected == "" {
		return false
	}
	supplied := strings.TrimSpace(r.Header.Get("X-Aiden-Benchmark-Token"))
	if supplied == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			supplied = strings.TrimSpace(auth[len("Bearer "):])
		}
	}
	if supplied == "" || len(supplied) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1
}

func (s *Server) handleProviderScreenshot(w http.ResponseWriter, r *http.Request) {
	screenprovider.HandleHTTP(w, r, s.providerScreenshotClient())
}

func (s *Server) handleProviderMNK(w http.ResponseWriter, r *http.Request) {
	provider := mnkProviderFromRuntime(s.runtime)
	if provider == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(mnk.MNKErrorResponse{Error: "mnk provider not configured"})
		return
	}
	mnk.NewHTTPHandler(provider).ServeHTTP(w, r)
}

// handleHealth serves GET /health, the environment-bridge readiness probe that
// benchmark uses for environment discovery and platform resolution. The Go
// agent is a fixed-concurrency bridge, so it reports the platform derived from
// its effective device type.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	platform := "ios"
	deviceType := defaultDeviceType
	if s.runtime != nil {
		platform = s.runtime.devicePlatformFromState()
		deviceType = s.runtime.deviceTypeFromState()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"data": map[string]any{
			"status":      "ok",
			"bridge_type": "go-agent",
			"platform":    platform,
			"device_type": deviceType,
			"concurrent":  1,
		},
	})
}

func (s *Server) handleConcurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true,
		"data": map[string]any{
			"bridge_type": "go-agent",
			"concurrent":  1,
		},
	})
}

// handleClear clears the conversation history
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.logger != nil {
		s.logger.Info("Conversation history cleared")
	}
	if err := s.runtime.ClearMemory(r.Context()); err != nil {
		if s.logger != nil {
			s.logger.Error("Clear memory failed: %v", err)
		}
		http.Error(w, fmt.Sprintf("Clear memory failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleClearAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.runtime.ClearAllMemory(r.Context()); err != nil {
		if s.logger != nil {
			s.logger.Error("Clear all memory failed: %v", err)
		}
		http.Error(w, fmt.Sprintf("Clear all memory failed: %v", err), http.StatusInternalServerError)
		return
	}

	if s.logger != nil {
		s.logger.Info("All memory cleared")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleSkillsReload marks the skill index as dirty so the next agent run
// reloads SKILL.md files from disk. Used by external tooling that swaps skill
// files without going through the agent's own skill_manage tool.
func (s *Server) handleSkillsReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.runtime.MarkSkillsDirty()
	if s.logger != nil {
		s.logger.Info("Skills marked dirty; will reload on next run")
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleAudioFile(w http.ResponseWriter, r *http.Request) {
	// Extract filename from URL path
	filename := strings.TrimPrefix(r.URL.Path, "/api/audio/")

	// Reject path traversal attempts in the request
	if strings.Contains(filename, "..") || strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	// Security: only allow base filenames, no path traversal
	filename = filepath.Base(filename)

	// Recordings may live on the SD card and/or eMMC depending on the
	// storage mode; search every root and serve the first hit.
	for _, audioDir := range s.audioReadRoots() {
		resolvedFullPath, ok := resolveAudioFileInDir(audioDir, filename, s.logger)
		if !ok {
			continue
		}
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeFile(w, r, resolvedFullPath)
		return
	}
	http.Error(w, "File not found", http.StatusNotFound)
}

// audioReadRoots lists every directory that may hold archived recordings.
// A non-default storage_path bypasses the storage-mode machinery.
func (s *Server) audioReadRoots() []string {
	archive := s.runtime.ConfigSnapshot().AudioArchive
	if path := archive.ExplicitStoragePath(); path != "" {
		return []string{path}
	}
	if s.runtime != nil {
		if sm := s.runtime.Storage(); sm != nil {
			return sm.ReadRoots(StorageClassAudio)
		}
	}
	return []string{archive.StoragePathOrDefault()}
}

// resolveAudioFileInDir resolves filename inside audioDir with symlinks
// expanded, rejecting anything that escapes the directory.
func resolveAudioFileInDir(audioDir, filename string, logger *Logger) (string, bool) {
	resolvedAudioDir, err := filepath.EvalSymlinks(audioDir)
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Warn("[audio] resolve audio dir: %v", err)
		}
		return "", false
	}

	resolvedFullPath, err := filepath.EvalSymlinks(filepath.Join(audioDir, filename))
	if err != nil {
		if !os.IsNotExist(err) && logger != nil {
			logger.Warn("[audio] resolve file path: %v", err)
		}
		return "", false
	}

	// Verify the resolved path is within the audio directory.
	if !strings.HasPrefix(resolvedFullPath, resolvedAudioDir+string(filepath.Separator)) {
		return "", false
	}
	return resolvedFullPath, true
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/tools":
		s.handleToolCatalog(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/tools/"):
		s.handleToolInvoke(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleToolCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToolCatalogResponse{
		Tools: s.runtime.ToolDescriptors(),
	})
}

func (s *Server) handleToolInvoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	toolName := strings.TrimPrefix(r.URL.Path, "/api/tools/")
	toolName = strings.TrimSpace(toolName)
	if toolName == "" || strings.Contains(toolName, "/") {
		http.NotFound(w, r)
		return
	}

	spec, ok := s.lookupOwnedToolSpec(toolName)
	if !ok {
		http.Error(w, fmt.Sprintf("Unknown tool: %s", toolName), http.StatusNotFound)
		return
	}

	rawInput, err := decodeToolInvokeInput(r.Body)
	if err != nil {
		http.Error(w, "Invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}

	startedAt := time.Now()
	executionCtx := r.Context()
	cancelExecution := context.CancelFunc(func() {})
	if httpToolExecutionSurvivesClientDisconnect(toolName) {
		executionCtx, cancelExecution = detachedHTTPToolExecutionContext(r.Context())
	}
	defer cancelExecution()
	execution := executeToolCall(executionCtx, ToolCallExecution{
		Specs:  NewToolSpecs([]langtools.Tool{spec.Tool}),
		Action: schema.AgentAction{Tool: spec.Name, ToolInput: rawInput},
	})
	duration := time.Since(startedAt).Milliseconds()
	isError := execution.Result.IsError() || execution.Error != nil
	var errStr string
	if execution.Result.Error != nil {
		errStr = execution.Result.Error.Message
	}
	if execution.Error != nil {
		errStr = execution.Error.Error()
	}

	response := ToolInvokeResponse{
		Tool:       spec.Descriptor(),
		RawInput:   execution.Call.Input,
		Output:     execution.Result.Output,
		IsError:    isError,
		ToolError:  execution.Result.Error,
		DurationMs: duration,
		CalledAt:   startedAt,
	}
	if errStr != "" {
		response.Error = errStr
		if response.Output == "" {
			response.Output = errStr
		}
	}

	if s.logger != nil {
		s.logger.Info("HTTP tool invoke: name=%s error=%v", toolName, response.IsError)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

const httpToolExecutionTimeout = 5 * time.Minute

func httpToolExecutionSurvivesClientDisconnect(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "keyboard_tap", "quick_action", toolOpenApp, toolSearchLaunchApp, "enter_text",
		"mouse_move",
		"mouse_scroll", "touch_gesture":
		return true
	default:
		return false
	}
}

// detachedHTTPToolExecutionContext lets an accepted tool invocation finish even
// if the client connection disappears. This matters for iOS HID operations:
// switching USB profiles briefly disconnects the composite ECM interface, which
// can otherwise cancel the very HTTP request that initiated the operation.
// Keep an explicit upper bound so abandoned requests cannot run indefinitely.
func detachedHTTPToolExecutionContext(requestCtx context.Context) (context.Context, context.CancelFunc) {
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	deadline := time.Now().Add(httpToolExecutionTimeout)
	if requestDeadline, ok := requestCtx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	return context.WithDeadline(context.WithoutCancel(requestCtx), deadline)
}

func (s *Server) handleToolSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ToolSkillsResponse{
		Skills: s.runtime.HTTPToolSkills(requestBaseURL(r)),
	})
}

func requestBaseURL(r *http.Request) string {
	if r == nil {
		return ""
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwardedProto := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		scheme = forwardedProto
	}

	host := firstForwardedHeaderValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

func firstForwardedHeaderValue(value string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func (s *Server) lookupOwnedToolSpec(name string) (ToolSpec, bool) {
	return s.runtime.ToolSpecs().LookupHTTP(name)
}

func decodeToolInvokeInput(body io.Reader) (string, error) {
	if body == nil {
		return "", nil
	}

	var req ToolInvokeRequest
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil
		}
		return "", err
	}

	if req.RawInput != nil {
		return *req.RawInput, nil
	}

	trimmed := strings.TrimSpace(string(req.Input))
	if trimmed == "" || trimmed == "null" {
		return "", nil
	}

	if strings.HasPrefix(trimmed, `"`) {
		var plain string
		if err := json.Unmarshal(req.Input, &plain); err == nil {
			return plain, nil
		}
	}

	return trimmed, nil
}

func legacyToolOutputLooksLikeError(output string) bool {
	// Boundary compatibility for older subtools/stubs that still return only a
	// legacy string. Do not use this as the internal source of error truth.
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(output)), "error:")
}

func (s *Server) publishMessage(message Message) (Message, bool) {
	message, ok := normalizePublicMessage(message)
	if !ok {
		return Message{}, false
	}
	// Streaming reasoning is delivered through the request's NDJSON/polling
	// channel. It is not persisted and should not cause every SSE subscriber to
	// reload the full history for each token.
	if message.Status != "streaming" && s.eventBroadcaster != nil {
		s.eventBroadcaster.Broadcast(message)
	}
	return message, true
}

// BroadcastMessage sends a message to all SSE subscribers.
func (s *Server) BroadcastMessage(msg Message) {
	var ok bool
	msg, ok = normalizePublicMessage(msg)
	if !ok {
		return
	}
	// Voice/STT runs have no request-scoped NDJSON connection, so their
	// transient reasoning updates must use the shared SSE channel.
	if (msg.Status != "streaming" || msg.Source == "voice") && s.eventBroadcaster != nil {
		s.eventBroadcaster.Broadcast(msg)
	}
}

func (s *Server) webHistorySnapshot() []Message {
	if s == nil || s.runtime == nil {
		return nil
	}
	dump := s.runtime.WebContextDump()
	result := make([]Message, 0, len(dump.Messages))
	role := "backend"
	if s.runtime.ConfigSnapshot().InputModeOrDefault() == "realtime" {
		role = "user"
	}
	for i, item := range dump.Messages {
		message, ok := webMessageFromContextMessage(item, role)
		if ok {
			message.RequestID = fmt.Sprintf("context-%s-%d", role, i)
			result = append(result, message)
		}
	}
	return result
}

func webMessageFromContextMessage(item messages.Message, contextRole string) (Message, bool) {
	message := Message{Content: item.Content, ReasoningContent: item.ReasoningContent, Timestamp: item.Timestamp}
	if item.Usage != nil {
		usage := *item.Usage
		message.Usage = &usage
	}
	switch item.Role {
	case messages.MessageRoleSystem:
		return Message{}, false
	case messages.MessageRoleUser:
		message.Type = "user"
	case messages.MessageRoleAssistant:
		message.Type = "assistant"
	case messages.MessageRoleToolCall:
		message.Type = "tool_call"
		if len(item.ToolCalls) > 0 {
			message.ToolName = item.ToolCalls[0].Name
			message.ToolInput = item.ToolCalls[0].Arguments
		}
	case messages.MessageRoleToolResult:
		message.Type = "tool_result"
		if len(item.ToolResults) > 0 {
			message.ToolName = item.ToolResults[0].Name
			message.Content = item.ToolResults[0].Content
		}
	case messages.MessageRoleNotice:
		message.Type = "notice"
	case messages.MessageRoleState:
		message.Type = "state"
	default:
		message.Type = "assistant"
	}
	message.Role = string(item.Role)
	for _, attachment := range item.Attachments {
		name := filepath.Base(attachment.FilePath)
		if name == "." || name == string(filepath.Separator) || name == "" {
			continue
		}
		message.Attachments = append(message.Attachments, MessageAttachment{
			Kind:       attachmentKindFromMIME(attachment.MIMEType),
			Name:       name,
			MIMEType:   attachment.MIMEType,
			Size:       int(attachment.FileSize),
			Path:       attachment.FilePath,
			PreviewURL: "/api/context/attachment?role=" + url.QueryEscape(contextRole) + "&attachment=" + url.QueryEscape(name),
		})
	}
	return message, true
}

func attachmentKindFromMIME(mimeType string) string {
	if isImageMIMEType(mimeType) {
		return AttachmentKindImage
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "audio/") {
		return AttachmentKindAudio
	}
	return "file"
}

func (s *Server) resolveRequestInput(req ChatRequest) (TurnInput, []MessageAttachment, error) {
	decodedAttachments, historyAttachments, err := decodeMessageAttachments(req.Attachments)
	if err != nil {
		return TurnInput{}, nil, err
	}

	audioAttachment, nonAudioAttachments, err := splitAudioAttachment(decodedAttachments)
	if err != nil {
		return TurnInput{}, nil, err
	}

	trimmedMessage := strings.TrimSpace(req.Message)
	if audioAttachment != nil {
		audioTranscript := firstAudioAttachmentTranscript(historyAttachments)
		audioInput, err := PrepareAudioInput(s.webAudioInputMode(), s.sttClientSnapshot(), audioAttachment.Data, audioTranscript, trimmedMessage, nonAudioAttachments)
		if err != nil {
			return TurnInput{}, nil, err
		}
		if audioInput.Transcript != "" {
			for i := range historyAttachments {
				if historyAttachments[i].Kind == AttachmentKindAudio {
					historyAttachments[i].Transcript = audioInput.Transcript
					break
				}
			}
		}
		return audioInput, historyAttachments, nil
	}

	turnInput := NewTextTurnInput(trimmedMessage, nonAudioAttachments)
	if turnInput.InputText == "" {
		return TurnInput{}, nil, fmt.Errorf("message or attachment is required")
	}
	return turnInput, historyAttachments, nil
}

func (s *Server) webAudioInputMode() string {
	switch s.runtime.ConfigSnapshot().InputModeOrDefault() {
	case "stt":
		return "stt"
	case "realtime":
		return "text"
	default:
		if s.sttClientSnapshot() != nil {
			return "stt"
		}
		return "text"
	}
}

func decodeMessageAttachments(payloads []MessageAttachment) ([]InputAttachment, []MessageAttachment, error) {
	decoded := make([]InputAttachment, 0, len(payloads))
	history := make([]MessageAttachment, 0, len(payloads))
	imageCount := 0

	for _, payload := range payloads {
		data := strings.TrimSpace(payload.Data)
		if data == "" {
			return nil, nil, fmt.Errorf("attachment %q is missing data", payload.Name)
		}

		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, nil, fmt.Errorf("attachment %q is not valid base64", payload.Name)
		}

		kind := strings.ToLower(strings.TrimSpace(payload.Kind))
		mimeType := strings.ToLower(strings.TrimSpace(payload.MIMEType))
		if kind == "" {
			switch {
			case isImageMIMEType(mimeType):
				kind = AttachmentKindImage
			case strings.HasPrefix(mimeType, "audio/"):
				kind = AttachmentKindAudio
			default:
				kind = "file"
			}
		}
		if kind == AttachmentKindImage || isImageMIMEType(mimeType) {
			kind = AttachmentKindImage
			imageCount++
			if imageCount > maxChatImageAttachments {
				return nil, nil, fmt.Errorf("at most %d image attachments are supported", maxChatImageAttachments)
			}
		}

		decoded = append(decoded, InputAttachment{
			Kind:     kind,
			Name:     payload.Name,
			MIMEType: payload.MIMEType,
			Width:    payload.Width,
			Height:   payload.Height,
			Data:     raw,
		})

		history = append(history, MessageAttachment{
			Kind:       kind,
			Name:       payload.Name,
			MIMEType:   payload.MIMEType,
			Width:      payload.Width,
			Height:     payload.Height,
			Data:       payload.Data,
			Size:       len(raw),
			Transcript: strings.TrimSpace(payload.Transcript),
		})
	}

	return decoded, history, nil
}

func isImageMIMEType(mimeType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/")
}

func splitAudioAttachment(attachments []InputAttachment) (*InputAttachment, []InputAttachment, error) {
	var audio *InputAttachment
	others := make([]InputAttachment, 0, len(attachments))

	for i := range attachments {
		attachment := attachments[i]
		if attachment.Kind == AttachmentKindAudio {
			if audio != nil {
				return nil, nil, fmt.Errorf("only one audio attachment is supported per request")
			}
			copyAttachment := attachment
			audio = &copyAttachment
			continue
		}
		others = append(others, attachment)
	}

	return audio, others, nil
}

func firstAudioAttachmentTranscript(attachments []MessageAttachment) string {
	for _, attachment := range attachments {
		if attachment.Kind == AttachmentKindAudio {
			return strings.TrimSpace(attachment.Transcript)
		}
	}
	return ""
}

func (s *Server) handleBridgeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	status := PhoneBridgeStatus{}
	if s.bridge != nil {
		status = s.bridge.getStatus()
	}
	if s.runtime != nil {
		status.DeviceType = s.runtime.deviceTypeFromState()
		status.PointerMode = s.runtime.devicePointerModeFromState()
	}
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleLiveActivityStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		http.Error(w, "Live Activity state is available only over USB", http.StatusForbidden)
		return
	}
	requestID := strings.TrimSpace(r.URL.Query().Get("request_id"))
	if requestID == "" {
		http.Error(w, `{"error":"missing request_id"}`, http.StatusBadRequest)
		return
	}
	state := s.liveActivitySnapshot(requestID)
	w.Header().Set("Content-Type", "application/json")
	if state == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "ok",
		"live_activity": state,
	})
}

func (s *Server) handleLiveActivityCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		http.Error(w, "Live Activity state is available only over USB", http.StatusForbidden)
		return
	}
	if s.liveActivity == nil {
		http.Error(w, `{"error":"live activity disabled"}`, http.StatusServiceUnavailable)
		return
	}
	phoneID := strings.TrimSpace(r.URL.Query().Get("phone_id"))
	state := s.liveActivity.SnapshotActive()
	if phoneID != "" {
		state = s.liveActivity.SnapshotActiveForPhone(phoneID)
	}
	w.Header().Set("Content-Type", "application/json")
	if state == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_found"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":        "ok",
		"live_activity": state,
	})
}

// handlePhoneBridgeCommands routes /api/phone-bridge/commands
func (s *Server) handlePhoneBridgeCommands(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil {
		http.Error(w, `{"error":"phone bridge not initialized"}`, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.bridge.handleEnqueueCommand(w, r)
	case http.MethodGet:
		s.bridge.handlePollCommands(w, r)
	default:
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handlePhoneBridgeCommandCancel routes cancellation of a queued Phone Bridge
// command to the bridge implementation.
func (s *Server) handlePhoneBridgeCommandCancel(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil {
		http.Error(w, `{"error":"phone bridge not initialized"}`, http.StatusServiceUnavailable)
		return
	}
	s.bridge.handleCancelCommand(w, r)
}

// handlePhoneBridgeResults routes /api/phone-bridge/results and /api/phone-bridge/results/:id
func (s *Server) handlePhoneBridgeResults(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil {
		http.Error(w, `{"error":"phone bridge not initialized"}`, http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/api/phone-bridge/results" {
		if r.Method == http.MethodPost {
			s.bridge.handleSubmitResult(w, r)
		} else {
			http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		}
	} else if strings.HasPrefix(r.URL.Path, "/api/phone-bridge/results/") {
		if r.Method == http.MethodGet {
			s.bridge.handleQueryResult(w, r)
		} else {
			http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		}
	} else {
		http.NotFound(w, r)
	}
}

// handleIndex serves the web UI
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html := webUI
	w.Write([]byte(html))
}

func (s *Server) handleKeyboardTapAndroidKeys(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/keyboard-tap-android-keys" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(keyboardTapAndroidKeysGuideHTML))
}

const coordinateDebugNavLink = `<a href="/coordinate-debug" class="new-chat-btn" style="text-decoration:none;display:inline-flex;align-items:center;">🎯 Coordinate Debug</a>`

const keyboardTapAndroidKeysGuideHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>keyboard_tap Android key guide</title>
    <style>
        :root {
            color-scheme: light;
            --bg: #f3efe6;
            --panel: #fffdf8;
            --line: rgba(52, 56, 49, 0.14);
            --text: #1e241d;
            --muted: #697063;
            --accent: #1f7a63;
            --accent-strong: #155646;
            --code: #f4eee3;
        }

        * {
            box-sizing: border-box;
        }

        body {
            margin: 0;
            font-family: "IBM Plex Sans", "Avenir Next", "Segoe UI", sans-serif;
            background: var(--bg);
            color: var(--text);
        }

        main {
            max-width: 1120px;
            margin: 0 auto;
            padding: 32px 20px 48px;
        }

        h1, h2 {
            margin: 0 0 12px;
        }

        p, li {
            line-height: 1.6;
        }

        .eyebrow {
            margin: 0 0 8px;
            color: var(--muted);
            font-size: 0.78rem;
            font-weight: 700;
            letter-spacing: 0.12em;
            text-transform: uppercase;
        }

        .panel,
        .callout {
            background: var(--panel);
            border: 1px solid var(--line);
            border-radius: 18px;
            padding: 18px 20px;
            margin-top: 18px;
        }

        .callout {
            border-left: 4px solid var(--accent);
        }

        code {
            background: var(--code);
            border-radius: 8px;
            padding: 2px 6px;
            font-family: "SFMono-Regular", "Menlo", monospace;
            font-size: 0.95em;
        }

        a {
            color: var(--accent);
        }

        a:hover {
            color: var(--accent-strong);
        }

        table {
            width: 100%;
            border-collapse: collapse;
            margin-top: 14px;
            background: var(--panel);
            border: 1px solid var(--line);
            border-radius: 16px;
            overflow: hidden;
        }

        th,
        td {
            padding: 12px 14px;
            border-bottom: 1px solid var(--line);
            text-align: left;
            vertical-align: top;
        }

        th {
            background: rgba(31, 122, 99, 0.08);
            font-size: 0.83rem;
            letter-spacing: 0.06em;
            text-transform: uppercase;
        }

        tr:last-child td {
            border-bottom: 0;
        }

        .examples {
            margin-top: 10px;
        }

        .examples li {
            margin-top: 8px;
        }
    </style>
</head>
<body>
    <main>
        <p class="eyebrow">Aiden Agent</p>
        <h1>keyboard_tap Android key guide</h1>
        <p>
            These aliases use <code>hid.usb2</code>. In <code>hid.pointer_mode = "touchscreen"</code>,
            they send one 16-bit Consumer Control usage at a time.
            They require a valid <code>hid.android_keyboard_device</code> such as <code>/dev/hidg2</code>.
            In <code>hid.pointer_mode = "absolute"</code>, the limited media-key interface sends a
            12-bit Consumer Control bitmap and only volume, media, screenshot, and brightness aliases
            are available; Android navigation aliases require <code>hid.pointer_mode = "touchscreen"</code>.
        </p>

        <div class="callout">
            <strong>Rules:</strong>
            <ul>
                <li>Use one Android extension key at a time.</li>
                <li>Do not combine <code>KEYCODE_*</code> aliases, or legacy <code>KEY_USAGE_*</code> compatibility names, with <code>ctrl</code>, <code>alt</code>, <code>shift</code>, <code>meta</code>, or standard keyboard keys.</li>
                <li>Raw short names such as <code>app_switch</code> or <code>refresh</code> are also accepted where listed below.</li>
                <li>Android and vendor ROMs may react differently to some usages even when AOSP maps them.</li>
            </ul>
        </div>

        <div class="panel">
            <h2>Examples</h2>
            <ul class="examples">
                <li><code>{"keys":["KEYCODE_BACK"]}</code></li>
                <li><code>{"keys":["KEYCODE_APP_SWITCH"]}</code></li>
                <li><code>{"keys":["KEYCODE_MEDIA_PLAY_PAUSE"]}</code></li>
                <li><code>{"keys":["KEYCODE_SETTINGS"]}</code></li>
                <li><code>{"keys":["KEYCODE_REFRESH"]}</code></li>
            </ul>
        </div>

        <h2>KEYCODE aliases</h2>
        <table>
            <thead>
                <tr>
                    <th>Alias</th>
                    <th>Raw name</th>
                    <th>Usage</th>
                    <th>AOSP action</th>
                </tr>
            </thead>
            <tbody>
                <tr><td><code>KEYCODE_BACK</code></td><td><code>android_back</code></td><td><code>0x0c0224</code></td><td>Back</td></tr>
                <tr><td><code>KEYCODE_HOME</code></td><td><code>android_home</code></td><td><code>0x0c0223</code></td><td>Home</td></tr>
                <tr><td><code>KEYCODE_APP_SWITCH</code></td><td><code>app_switch</code></td><td><code>0x0c029F</code></td><td>Recent Apps</td></tr>
                <tr><td><code>KEYCODE_MENU</code></td><td><code>menu</code></td><td><code>0x0c0040</code></td><td>Menu</td></tr>
                <tr><td><code>KEYCODE_SEARCH</code></td><td><code>search</code></td><td><code>0x0c0221</code></td><td>Search</td></tr>
                <tr><td><code>KEYCODE_POWER</code></td><td><code>power</code></td><td><code>0x0c0030</code></td><td>Power</td></tr>
                <tr><td><code>KEYCODE_SLEEP</code></td><td><code>sleep</code></td><td><code>0x0c0032</code></td><td>Sleep</td></tr>
                <tr><td><code>KEYCODE_VOLUME_MUTE</code></td><td><code>volume_mute</code></td><td><code>0x0c00E2</code></td><td>Volume Mute</td></tr>
                <tr><td><code>KEYCODE_VOLUME_UP</code></td><td><code>volume_up</code></td><td><code>0x0c00E9</code></td><td>Volume Up</td></tr>
                <tr><td><code>KEYCODE_VOLUME_DOWN</code></td><td><code>volume_down</code></td><td><code>0x0c00EA</code></td><td>Volume Down</td></tr>
                <tr><td><code>KEYCODE_MEDIA_PLAY_PAUSE</code></td><td><code>media_play_pause</code></td><td><code>0x0c00CD</code></td><td>Media Play/Pause</td></tr>
                <tr><td><code>KEYCODE_MEDIA_STOP</code></td><td><code>media_stop</code></td><td><code>0x0c00B7</code></td><td>Media Stop</td></tr>
                <tr><td><code>KEYCODE_MEDIA_NEXT</code></td><td><code>media_next</code></td><td><code>0x0c00B5</code></td><td>Media Next</td></tr>
                <tr><td><code>KEYCODE_MEDIA_PREVIOUS</code></td><td><code>media_previous</code></td><td><code>0x0c00B6</code></td><td>Media Previous</td></tr>
                <tr><td><code>KEYCODE_MEDIA_REWIND</code></td><td><code>media_rewind</code></td><td><code>0x0c00B4</code></td><td>Media Rewind</td></tr>
                <tr><td><code>KEYCODE_MEDIA_FAST_FORWARD</code></td><td><code>media_fast_forward</code></td><td><code>0x0c00B3</code></td><td>Media Fast Forward</td></tr>
            </tbody>
        </table>

        <h2>HID-backed KEYCODE aliases</h2>
        <table>
            <thead>
                <tr>
                    <th>Alias</th>
                    <th>Raw name</th>
                    <th>Usage</th>
                    <th>Android API</th>
                    <th>AOSP action</th>
                </tr>
            </thead>
	            <tbody>
	                <tr><td><code>KEYCODE_SCREENSHOT</code></td><td><code>screenshot</code></td><td><code>0x0c0065</code></td><td>35</td><td>Screenshot</td></tr>
	                <tr><td><code>KEYCODE_SETTINGS</code></td><td><code>settings</code></td><td><code>0x0c019F</code></td><td>11</td><td>Settings</td></tr>
	                <tr><td><code>KEYCODE_WINDOW</code></td><td><code>window</code></td><td><code>0x0c0067</code></td><td>11</td><td>Window</td></tr>
                <tr><td><code>KEYCODE_BRIGHTNESS_UP</code></td><td><code>brightness_up</code></td><td><code>0x0c006F</code></td><td>18</td><td>Brightness Up</td></tr>
                <tr><td><code>KEYCODE_BRIGHTNESS_DOWN</code></td><td><code>brightness_down</code></td><td><code>0x0c0070</code></td><td>18</td><td>Brightness Down</td></tr>
                <tr><td><code>KEYCODE_DICTATE</code></td><td><code>dictate</code></td><td><code>0x0c00D8</code></td><td>36</td><td>Dictate</td></tr>
                <tr><td><code>KEYCODE_EMOJI_PICKER</code></td><td><code>emoji_picker</code></td><td><code>0x0c00D9</code></td><td>35</td><td>Emoji Picker</td></tr>
                <tr><td><code>KEYCODE_MEDIA_AUDIO_TRACK</code></td><td><code>media_audio_track</code></td><td><code>0x0c0173</code></td><td>19</td><td>Media Audio Track</td></tr>
                <tr><td><code>KEYCODE_PROFILE_SWITCH</code></td><td><code>profile_switch</code></td><td><code>0x0c019C</code></td><td>29</td><td>Profile Switch</td></tr>
	                <tr><td><code>KEYCODE_NEW</code></td><td><code>new</code></td><td><code>0x0c0201</code></td><td>36</td><td>New</td></tr>
	                <tr><td><code>KEYCODE_LANGUAGE_SWITCH</code></td><td><code>language_switch</code></td><td><code>0x0c029D</code></td><td>14</td><td>Language Switch</td></tr>
	                <tr><td><code>KEYCODE_FULLSCREEN</code></td><td><code>fullscreen</code></td><td><code>0x0c0232</code></td><td>36</td><td>Fullscreen</td></tr>
	                <tr><td><code>KEYCODE_REFRESH</code></td><td><code>refresh</code></td><td><code>0x0c0227</code></td><td>28</td><td>Refresh</td></tr>
	                <tr><td><code>KEYCODE_CLOSE</code></td><td><code>close</code></td><td><code>0x0c0203</code></td><td>36</td><td>Close</td></tr>
	                <tr><td><code>KEYCODE_PRINT</code></td><td><code>print</code></td><td><code>0x0c0208</code></td><td>36</td><td>Print</td></tr>
	            </tbody>
	        </table>

	        <h2>Legacy HID usage aliases</h2>
	        <p>
	            Older <code>KEY_USAGE_*</code> spellings for the migrated rows remain accepted for compatibility;
	            new calls should use the <code>KEYCODE_*</code> aliases above.
	        </p>

        <div class="panel">
            <h2>Notes</h2>
            <p>
                AOSP resolves HID usage mappings before Linux scan-code mappings. That is why
                <code>KEYCODE_APP_SWITCH</code> uses <code>0x0c029F</code> rather than
                <code>0x0c01A2</code>: the former maps to Recent Apps, while the latter maps to All Apps.
            </p>
            <p>
                <code>KEYCODE_WAKEUP</code>, <code>KEYCODE_SOFT_SLEEP</code>, and <code>KEYCODE_NOTIFICATION</code>
                are reserved aliases but currently return explicit unsupported errors on this gadget.
            </p>
            <p>
                Return to the <a href="/">main UI</a> when you are done.
            </p>
        </div>
    </main>
</body>
</html>
`

package agent

import (
	"aiden-agent/internal/agent/executor"
	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/realtimevoice"
	"aiden-agent/internal/logging"
	"aiden-agent/internal/netproxy"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type SearchConfig struct {
	Provider string `toml:"provider,omitempty"`
	APIKey   string `toml:"api_key,omitempty"`
}

type EnvironmentBridgeConfig struct {
	Enabled          bool   `toml:"-"` // Only set via CLI, not config file
	Endpoint         string `toml:"-"` // Only set via CLI, not config file
	BenchmarkTaskID  string `toml:"-"` // Only set via CLI, not config file
	LocalPhoneBridge bool   `toml:"-"` // Keep the App connection on this Agent while device input uses the environment bridge.
}

type BenchmarkConfig struct {
	Headless  bool `toml:"-"` // Benchmark container without a device or environment bridge.
	MockTools bool `toml:"-"` // Explicit scripted policy fixture mode; requires Token and EnvironmentBridge.

	Token string `toml:"-"` // Only set via benchmark runner CLI flags, never from config file
}

const (
	searchProviderDuckDuckGo = "duckduckgo"
	searchProviderTavily     = "tavily"
	searchProviderBrave      = "brave"

	braveSearchAPIKeyEnv = "BRAVE_SEARCH_API_KEY"
)

func (s SearchConfig) ProviderOrDefault() string {
	return normalizeSearchProvider(s.Provider)
}

func normalizeSearchProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return searchProviderDuckDuckGo
	}
	alias := strings.NewReplacer("_", "-", " ", "-").Replace(normalized)
	switch alias {
	case searchProviderDuckDuckGo:
		return searchProviderDuckDuckGo
	case searchProviderTavily:
		return searchProviderTavily
	case searchProviderBrave, "brave-search", "brave-free":
		return searchProviderBrave
	}
	return normalized
}

func searchAPIKeyOrEnv(configured string, envKeys ...string) string {
	if key := strings.TrimSpace(configured); key != "" {
		return key
	}
	for _, envKey := range envKeys {
		if key := strings.TrimSpace(os.Getenv(envKey)); key != "" {
			return key
		}
	}
	return ""
}

// AudioArchiveConfig controls optional archival of voice recordings as WAV files.
type AudioArchiveConfig struct {
	Enabled     bool   `toml:"enabled"`
	MaxFiles    int    `toml:"max_files,omitempty"`
	MaxSizeMB   int    `toml:"max_size_mb,omitempty"`
	StoragePath string `toml:"storage_path,omitempty"`
}

// FrameServiceConfig controls the board-side HDMI capture service lifecycle.
// The Agent preserves this section so the setup portal can own the setting,
// while frame_service itself reads the persisted TOML at service startup.
type FrameServiceConfig struct {
	KeepStreamOn bool `toml:"keep_streamon"`
}

// MaxFilesOrDefault returns MaxFiles if positive, else 500.
func (c AudioArchiveConfig) MaxFilesOrDefault() int {
	if c.MaxFiles <= 0 {
		return defaultAudioArchiveMaxFiles
	}
	return c.MaxFiles
}

// MaxSizeMBOrDefault returns MaxSizeMB if positive, else 100.
func (c AudioArchiveConfig) MaxSizeMBOrDefault() int {
	if c.MaxSizeMB <= 0 {
		return defaultAudioArchiveMaxSizeMB
	}
	return c.MaxSizeMB
}

// StoragePathOrDefault returns StoragePath if non-empty, else "/userdata/audio".
func (c AudioArchiveConfig) StoragePathOrDefault() string {
	path := strings.TrimSpace(c.StoragePath)
	if path == "" {
		return defaultAudioArchiveStoragePath
	}
	return path
}

// ExplicitStoragePath returns the storage path only when it opts out of
// storage-manager routing, else "". DefaultConfig seeds StoragePath with the
// built-in default and the config portal persists resolved values, so a stored
// "/userdata/audio" is indistinguishable from "never configured" — and it names
// the same directory the storage manager already uses as the audio eMMC tier.
// Treat it as unset; only a non-default path pins recordings to a fixed
// directory.
func (c AudioArchiveConfig) ExplicitStoragePath() string {
	path := strings.TrimSpace(c.StoragePath)
	if path == defaultAudioArchiveStoragePath {
		return ""
	}
	return path
}

// StorageConfig tunes the optional microSD data store managed by
// StorageManager. The storage mode itself is not configurable: a usable
// card enables the SD migration tier; otherwise governed data remains on eMMC.
type StorageConfig struct {
	MountPoint    string `toml:"mount_point,omitempty"`
	Device        string `toml:"device,omitempty"`
	MinCardFreeMB int    `toml:"min_card_free_mb,omitempty"`
	// Watermarks for the background migration of older governed data from
	// eMMC to the SD card: migration starts when eMMC free space drops
	// below MigrateStartFreePct and stops once it reaches MigrateStopFreePct.
	MigrateStartFreePct int `toml:"migrate_start_free_pct,omitempty"`
	MigrateStopFreePct  int `toml:"migrate_stop_free_pct,omitempty"`

	MonitorEnabled       bool                      `toml:"monitor_enabled"`
	RootPath             string                    `toml:"root_path,omitempty"`
	CheckIntervalSeconds int                       `toml:"check_interval_seconds,omitempty"`
	WarningThresholdMB   uint64                    `toml:"warning_threshold_mb,omitempty"`
	CriticalThresholdMB  uint64                    `toml:"critical_threshold_mb,omitempty"`
	EmergencyThresholdMB uint64                    `toml:"emergency_threshold_mb,omitempty"`
	RecoveryHysteresisMB uint64                    `toml:"recovery_hysteresis_mb,omitempty"`
	DegradedMode         StorageDegradedModeConfig `toml:"degraded_mode,omitempty"`
	Cleanup              StorageCleanupConfig      `toml:"cleanup,omitempty"`
}

func (c StorageConfig) MonitorConfig() StorageMonitorConfig {
	return StorageMonitorConfig{
		Enabled:              c.MonitorEnabled,
		RootPath:             c.RootPath,
		CheckIntervalSeconds: c.CheckIntervalSeconds,
		WarningThresholdMB:   c.WarningThresholdMB,
		CriticalThresholdMB:  c.CriticalThresholdMB,
		EmergencyThresholdMB: c.EmergencyThresholdMB,
		RecoveryHysteresisMB: c.RecoveryHysteresisMB,
		DegradedMode:         c.DegradedMode,
		Cleanup:              c.Cleanup,
	}
}

// MountPointOrDefault returns MountPoint if non-empty, else "/mnt/sdcard".
func (c StorageConfig) MountPointOrDefault() string {
	path := strings.TrimSpace(c.MountPoint)
	if path == "" {
		return defaultStorageMountPoint
	}
	return path
}

// DeviceOrDefault returns Device if non-empty, else "mmcblk2".
func (c StorageConfig) DeviceOrDefault() string {
	dev := strings.TrimSpace(c.Device)
	if dev == "" {
		return defaultStorageDevice
	}
	return dev
}

// MinCardFreeMBOrDefault returns MinCardFreeMB if positive, else 64.
func (c StorageConfig) MinCardFreeMBOrDefault() int {
	if c.MinCardFreeMB <= 0 {
		return defaultStorageMinCardFreeMB
	}
	return c.MinCardFreeMB
}

// MigrateWatermarksOrDefault returns the (start, stop) free-space
// percentages for eMMC→SD migration. Defaults: start below 10%, stop at
// 50%. An inverted or out-of-range pair falls back to the defaults so a
// bad config cannot disable or loop the migrator.
func (c StorageConfig) MigrateWatermarksOrDefault() (int, int) {
	start, stop := c.MigrateStartFreePct, c.MigrateStopFreePct
	if start <= 0 {
		start = defaultStorageMigrateStartFreePct
	}
	if stop <= 0 {
		stop = defaultStorageMigrateStopFreePct
	}
	if start >= stop || stop > 95 {
		return defaultStorageMigrateStartFreePct, defaultStorageMigrateStopFreePct
	}
	return start, stop
}

// LogConfig controls local runtime logging and retention.
type LogConfig struct {
	LLMHTTPRetentionDays int    `toml:"llm_http_retention_days,omitempty"`
	Level                string `toml:"level,omitempty"`
}

// LLMHTTPRetentionDaysOrDefault returns LLMHTTPRetentionDays if positive, else 7.
func (c LogConfig) LLMHTTPRetentionDaysOrDefault() int {
	if c.LLMHTTPRetentionDays <= 0 {
		return defaultLLMHTTPLogRetentionDays
	}
	return c.LLMHTTPRetentionDays
}

// LevelOrDefault returns the normalized minimum Agent log severity.
func (c LogConfig) LevelOrDefault() string {
	level := strings.ToLower(strings.TrimSpace(c.Level))
	if level == "" {
		return defaultLogLevel
	}
	return level
}

// OTAConfig controls OTA update behavior.
type OTAConfig struct {
	GitHubProxyURL string `toml:"github_proxy_url,omitempty"`
}

// GitHubProxyURLOrDefault returns GitHubProxyURL if non-empty and valid, else empty string.
func (c OTAConfig) GitHubProxyURLOrDefault() string {
	return strings.TrimSpace(c.GitHubProxyURL)
}

// Validate checks if the GitHub proxy URL is valid when configured
func (c OTAConfig) Validate() error {
	proxyURL := strings.TrimSpace(c.GitHubProxyURL)
	if proxyURL == "" {
		return nil // Empty is valid (no proxy)
	}

	// Parse the URL
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("ota.github_proxy_url: invalid URL: %w", err)
	}

	// Must have a scheme
	if parsed.Scheme == "" {
		return fmt.Errorf("ota.github_proxy_url: must be an absolute URL with scheme (e.g., https://example.com/)")
	}

	// Must be HTTPS
	if parsed.Scheme != "https" {
		return fmt.Errorf("ota.github_proxy_url: must use https, got %s", parsed.Scheme)
	}

	// Must have a host
	if parsed.Host == "" {
		return fmt.Errorf("ota.github_proxy_url: missing host")
	}

	return nil
}

// ModelProvider defines a model service provider configuration.
type ModelProvider struct {
	Type    string `toml:"type"` // Provider type: openai, kimi, ollama, etc.
	APIKey  string `toml:"api_key,omitempty"`
	BaseURL string `toml:"base_url,omitempty"`
}

type Config struct {
	DeviceTypeOverride         string                        `toml:"-" json:"-"`                // Process-local CLI override retained across reloads.
	ModelProviders             map[string]ModelProvider      `toml:"model_providers,omitempty"` // Named model provider configurations
	TTSProviders               map[string]TTSProvider        `toml:"tts_providers,omitempty"`   // Named TTS provider configurations
	STTProviders               map[string]STTProvider        `toml:"stt_providers,omitempty"`   // Named STT provider configurations
	VoiceModelProviders        map[string]VoiceModelProvider `toml:"voice_model_providers,omitempty"`
	Model                      ModelConfig                   `toml:"model"`
	Decision                   DecisionModelConfig           `toml:"decision,omitempty"`
	TTS                        TTSConfig                     `toml:"tts,omitempty"`
	STT                        STTConfig                     `toml:"stt,omitempty"`
	HID                        HIDConfig                     `toml:"hid"`
	Device                     DeviceConfig                  `toml:"device,omitempty"`
	Audio                      AudioConfig                   `toml:"audio,omitempty"`
	VoiceModel                 VoiceModelConfig              `toml:"voice_model,omitempty"`
	AudioArchive               AudioArchiveConfig            `toml:"audio_archive,omitempty"`
	FrameService               FrameServiceConfig            `toml:"frame_service,omitempty"`
	Storage                    StorageConfig                 `toml:"storage,omitempty"`
	VoiceNotifications         VoiceNotificationsConfig      `toml:"voice_notifications,omitempty"`
	QuickCapture               QuickCaptureConfig            `toml:"quick_capture,omitempty"`
	Log                        LogConfig                     `toml:"log,omitempty"`
	OTA                        OTAConfig                     `toml:"ota,omitempty"`
	Search                     SearchConfig                  `toml:"search,omitempty"`
	EnvironmentBridge          EnvironmentBridgeConfig       `toml:"-"` // Only set via CLI flags, never from config file
	Benchmark                  BenchmarkConfig               `toml:"-"` // Only set via CLI flags, never from config file
	LiveActivity               LiveActivityConfig            `toml:"live_activity,omitempty"`
	Locale                     string                        `toml:"locale,omitempty"`
	Timezone                   string                        `toml:"timezone,omitempty"`
	Instruction                string                        `toml:"-"` // Built-in runtime instruction; the legacy custom_instruction key is read but ignored (see applyRuntimeInstructionDefault).
	Prompt                     string                        `toml:"prompt,omitempty"`
	InputMode                  string                        `toml:"input_mode,omitempty"`  // "stt" or "realtime"
	VADBackend                 string                        `toml:"vad_backend,omitempty"` // "rknn", "cpu"
	VADModelPath               string                        `toml:"vad_model_path,omitempty"`
	VADHelperPath              string                        `toml:"vad_helper_path,omitempty"`
	VADSpeechThreshold         float64                       `toml:"vad_speech_threshold,omitempty"`
	SilenceMs                  int                           `toml:"silence_ms,omitempty"`
	MinSpeechMs                int                           `toml:"min_speech_ms,omitempty"`
	VoiceFollowupEnabled       *bool                         `toml:"voice_followup_enabled,omitempty"`
	VoiceFollowupTimeoutMs     int                           `toml:"voice_followup_timeout_ms,omitempty"`
	VoiceFirstTurnTimeoutMs    int                           `toml:"voice_first_turn_timeout_ms,omitempty"`
	VoiceMaxTurns              int                           `toml:"voice_max_turns,omitempty"`
	VoiceInterruptOnWakeup     *bool                         `toml:"voice_interrupt_on_wakeup,omitempty"`
	TapWakeupEnabled           *bool                         `toml:"tap_wakeup_enabled,omitempty"` // hardware tap-button wakeup; default true
	MCPEnabled                 *bool                         `toml:"mcp_enabled,omitempty"`        // Streamable HTTP /mcp; default true
	MCPShellEnabled            *bool                         `toml:"mcp_shell_enabled,omitempty"`  // expose shell via MCP; default false
	MCPConfigWebBaseURL        string                        `toml:"mcp_config_web_base_url,omitempty"`
	VoiceStreamingTTSEnabled   *bool                         `toml:"voice_streaming_tts_enabled,omitempty"`
	VoiceToolCallSpeech        *bool                         `toml:"voice_tool_call_speech,omitempty"`
	VoiceProgressSpeechEnabled *bool                         `toml:"voice_progress_speech_enabled,omitempty"`
	VoiceMaxResponseTokens     int                           `toml:"voice_max_response_tokens,omitempty"`
	MaxIterations              int                           `toml:"max_iterations,omitempty"`
	TerminationPolicy          TerminationPolicyConfig       `toml:"termination_policy,omitempty"`
	ForceSimpleLoop            bool                          `toml:"-"`
	// ContextPruneThreshold is the fraction of the usable model input budget at
	// which deterministic cleanup of stale state and older tool exchanges runs,
	// including during a long-running tool loop. Zero uses
	// defaultContextPruneThreshold. Pruning is meant to run before conversation
	// compaction, so the effective value is capped at ContextCompactionThreshold;
	// read it through
	// ContextPruneThresholdOrDefault rather than directly.
	ContextPruneThreshold float64 `toml:"context_prune_threshold,omitempty"`
	// ContextCompactionThreshold is the fraction of the usable model input
	// budget at which conversation compaction summarizes the transcript. Zero
	// uses defaultContextCompactionThreshold. Read it through
	// ContextCompactionThresholdOrDefault rather than directly, so loaders that
	// do not start from DefaultConfig still get the default.
	ContextCompactionThreshold float64         `toml:"context_compaction_threshold,omitempty"`
	ScreenshotKeepN            int             `toml:"screenshot_keep_n,omitempty"`
	ScreenshotPruneInterval    int             `toml:"screenshot_prune_interval,omitempty"`
	ScreenStableTimeoutMs      int             `toml:"screen_stable_timeout_ms,omitempty"`
	ScreenStableMs             int             `toml:"screen_stable_ms,omitempty"`
	ScreenStableDiffThreshold  float64         `toml:"screen_stable_diff_threshold,omitempty"`
	SkillsDirs                 []string        `toml:"skills_dirs"`
	BundledSkillsDir           string          `toml:"bundled_skills_dir,omitempty"`
	SkillMergeModel            SkillMergeModel `toml:"-"`
	Telemetry                  TelemetryConfig `toml:"telemetry,omitempty"`
	ConfigDir                  string          `toml:"-"`
}

func (c Config) TerminationPolicyOrDefault() TerminationPolicyConfig {
	return c.TerminationPolicy.resolved()
}

type TelemetryConfig struct {
	Enabled           *bool    `toml:"enabled,omitempty"`
	Provider          string   `toml:"provider,omitempty"`
	BaseURL           string   `toml:"base_url,omitempty"`
	PublicKey         string   `toml:"public_key,omitempty"`
	SecretKey         string   `toml:"secret_key,omitempty"`
	UploadScreenshots *bool    `toml:"upload_screenshots,omitempty"`
	UploadTimeoutSec  int      `toml:"upload_timeout_sec,omitempty"`
	MaxRetry          int      `toml:"max_retry,omitempty"`
	Tags              []string `toml:"tags,omitempty"`
	Environment       string   `toml:"environment,omitempty"`
}

type LiveActivityConfig struct {
	Enabled *bool `toml:"enabled,omitempty"`
}

type TTSConfig struct {
	Provider    string  `toml:"provider"` // "minimax", "minimax-cn", "fish-audio", "alicloud", "volcengine"
	APIKey      string  `toml:"api_key,omitempty"`
	Model       string  `toml:"model,omitempty"`
	VoiceID     string  `toml:"voice_id,omitempty"`
	Emotion     string  `toml:"emotion,omitempty"`
	Speed       float64 `toml:"speed,omitempty"`
	ReferenceID string  `toml:"reference_id,omitempty"` // Fish Audio voice reference ID

	// ActiveProviderRecord is runtime-only: it records which
	// [tts_providers.<name>] record the Provider field referred to before resolution
	// replaced the reference with the provider TYPE.
	//
	// Without it the two resolution steps disagree. Load time resolves the
	// reference and rewrites Provider to the type; speak time re-resolves by
	// type, and with two records of the same type (two accounts of one service,
	// the whole point of named records) the type scan can return the other one.
	// The result is speaking with the wrong account's key while the config page
	// shows the right record selected.
	ActiveProviderRecord string `toml:"-"`
}

type STTConfig struct {
	Provider        string `toml:"provider"` // "openai-whisper", "tencent-asr" (legacy: "openai", "tencent", "tencent_asr")
	APIKey          string `toml:"api_key,omitempty"`
	Model           string `toml:"model,omitempty"`
	BaseURL         string `toml:"base_url,omitempty"`
	Language        string `toml:"language,omitempty"` // "zh" (Chinese) or "en" (English)
	AppID           string `toml:"app_id,omitempty"`
	SecretID        string `toml:"secret_id,omitempty"`
	SecretKey       string `toml:"secret_key,omitempty"`
	Region          string `toml:"region,omitempty"`
	EngineModelType string `toml:"engine_model_type,omitempty"` // Deprecated: use Language instead
}

type AudioConfig struct {
	Socket     string `toml:"socket,omitempty"`
	SampleRate int    `toml:"sample_rate,omitempty"`
	Channels   int    `toml:"channels,omitempty"`
	BitWidth   int    `toml:"bit_width,omitempty"`
	Backend    string `toml:"backend,omitempty"`
}

// VoiceModelConfig configures the realtime audio model used by the realtime
// voice path. The path is selected by agent.input_mode, not by API key presence.
type VoiceModelConfig struct {
	Provider               string   `toml:"provider,omitempty"`
	UpstreamProvider       string   `toml:"upstream_provider,omitempty"`
	AgentID                string   `toml:"agent_id,omitempty"`
	APIKey                 string   `toml:"api_key,omitempty"`
	Model                  string   `toml:"model,omitempty"`
	WorkspaceID            string   `toml:"workspace_id,omitempty"`
	Region                 string   `toml:"region,omitempty"`
	AuthMode               string   `toml:"auth_mode,omitempty"`
	ProjectID              string   `toml:"project_id,omitempty"`
	Location               string   `toml:"location,omitempty"`
	Endpoint               string   `toml:"endpoint,omitempty"`
	BaseURL                string   `toml:"base_url,omitempty"`
	RealtimeProtocol       string   `toml:"realtime_protocol,omitempty"`
	ThinkingLevel          string   `toml:"thinking_level,omitempty"`
	Voice                  string   `toml:"voice,omitempty"`
	EnableSpeechEmotion    *bool    `toml:"enable_speech_emotion,omitempty"`
	InputAudioFormat       string   `toml:"input_audio_format,omitempty"`
	OutputAudioFormat      string   `toml:"output_audio_format,omitempty"`
	TurnDetection          string   `toml:"turn_detection,omitempty"`
	TurnDetectionThreshold *float64 `toml:"turn_detection_threshold,omitempty"`
	TurnDetectionSilenceMs int      `toml:"turn_detection_silence_ms,omitempty"`
	// UseBackendAgent controls whether the realtime model can delegate work to
	// the backend agent. When disabled, the runtime tools that the backend agent
	// would otherwise use are exposed directly to the realtime model. Provider
	// reasoning is determined independently by the provider/model.
	UseBackendAgent      bool   `toml:"use_backend_agent,omitempty"`
	ActiveProviderRecord string `toml:"-"`
}

func (c VoiceModelConfig) Enabled() bool { return strings.TrimSpace(c.APIKey) != "" }

func (c VoiceModelConfig) Validate() error {
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	if provider != "" && !realtimevoice.IsProvider(provider) {
		return fmt.Errorf("voice_model.provider: unsupported provider %q", c.Provider)
	}
	if level := strings.ToUpper(strings.TrimSpace(c.ThinkingLevel)); level != "" {
		switch level {
		case "MINIMAL", "LOW", "MEDIUM", "HIGH":
		default:
			return fmt.Errorf("voice_model.thinking_level: unsupported value %q (use LOW, MEDIUM, HIGH, or MINIMAL)", c.ThinkingLevel)
		}
	}
	if provider == "speko" {
		upstream := strings.TrimSpace(c.UpstreamProvider)
		model := strings.TrimSpace(c.Model)
		if upstream == "" || model == "" {
			return errors.New("voice_model.upstream_provider and voice_model.model are required for provider=speko; automatic routing is disabled because it may select an unsupported WebRTC route")
		}
		switch strings.ToLower(upstream) {
		case "google", "gemini", "xai":
		default:
			return fmt.Errorf("voice_model.upstream_provider: unsupported provider %q (want google or xai)", c.UpstreamProvider)
		}
	}
	if provider == "gemini" {
		switch authMode := strings.ToLower(strings.TrimSpace(c.AuthMode)); authMode {
		case "", "api_key":
		case "vertex":
			if strings.TrimSpace(c.ProjectID) == "" || strings.TrimSpace(c.Location) == "" {
				return errors.New("voice_model.project_id and voice_model.location are required for Gemini Vertex auth")
			}
		default:
			return fmt.Errorf("voice_model.auth_mode: unsupported Gemini auth mode %q (want api_key or vertex)", c.AuthMode)
		}
	}
	if region := strings.TrimSpace(c.Region); provider != "speko" && region != "" && region != "cn-beijing" && region != "ap-southeast-1" {
		return fmt.Errorf("voice_model.region: unsupported region %q", c.Region)
	}
	if endpoint := strings.TrimSpace(c.Endpoint); provider != "speko" && endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "wss" &&
			!(u.Scheme == "ws" && netproxy.IsLoopbackHost(u.Hostname()))) {
			return fmt.Errorf("voice_model.endpoint: invalid websocket URL %q", c.Endpoint)
		}
	}
	if base := strings.TrimSpace(c.BaseURL); base != "" {
		u, err := url.Parse(base)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && netproxy.IsLoopbackHost(u.Hostname()))) {
			return fmt.Errorf("voice_model.base_url: invalid HTTP URL %q", c.BaseURL)
		}
	}
	if provider == realtimevoice.ProviderQwen {
		if err := validateQwenTurnDetection(c.TurnDetection, c.TurnDetectionThreshold, c.TurnDetectionSilenceMs, "voice_model"); err != nil {
			return err
		}
	}
	return nil
}

// ProxyConfig is the explicit proxy configuration shared with the TTS adapters
// and other network clients.
type ProxyConfig = netproxy.Config

const DefaultNoProxy = netproxy.DefaultNoProxy

func ProxyConfigFromEnvironment() ProxyConfig {
	p := ProxyConfig{
		HTTPProxy:  firstEnv("HTTP_PROXY", "http_proxy"),
		HTTPSProxy: firstEnv("HTTPS_PROXY", "https_proxy"),
		AllProxy:   firstEnv("ALL_PROXY", "all_proxy"),
		NoProxy:    firstEnv("NO_PROXY", "no_proxy"),
	}
	return p.WithDefaults()
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func (a AudioConfig) SocketOrDefault() string {
	if a.Socket != "" {
		return a.Socket
	}
	return defaultAudioSocket
}

func (a AudioConfig) SampleRateOrDefault() int {
	if a.SampleRate > 0 {
		return a.SampleRate
	}
	return defaultAudioSampleRate
}

func (a AudioConfig) ChannelsOrDefault() int {
	if a.Channels > 0 {
		return a.Channels
	}
	return defaultAudioChannels
}

func (a AudioConfig) BitWidthOrDefault() int {
	if a.BitWidth > 0 {
		return a.BitWidth
	}
	return defaultAudioBitWidth
}

const (
	AudioBackendAuto         = "auto"
	AudioBackendAudioService = "audio_service"
	AudioBackendLocal        = "local"
)

func normalizeAudioBackend(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", AudioBackendAuto:
		return AudioBackendAuto, nil
	case AudioBackendAudioService, "audio-service", "audioservice":
		return AudioBackendAudioService, nil
	case AudioBackendLocal, "pc", "desktop":
		return AudioBackendLocal, nil
	default:
		return "", fmt.Errorf("invalid audio.backend: %s (expected auto, audio_service, or local)", value)
	}
}

func (a AudioConfig) BackendOrDefault() string {
	backend, err := normalizeAudioBackend(a.Backend)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(a.Backend))
	}
	return backend
}

func (c Config) AudioBackendOrDefault() string {
	backend, err := normalizeAudioBackend(c.Audio.Backend)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(c.Audio.Backend))
	}
	if backend != AudioBackendAuto {
		return backend
	}
	if c.HID.InputBackendADB() || c.EnvironmentBridge.Enabled {
		return AudioBackendLocal
	}
	return AudioBackendAudioService
}

type HIDConfig struct {
	KeyboardDevice        string `toml:"keyboard_device,omitempty"`
	KeyboardLayout        string `toml:"keyboard_layout,omitempty"`
	MouseDevice           string `toml:"mouse_device,omitempty"`
	AndroidKeyboardDevice string `toml:"android_keyboard_device,omitempty"`
	FrameSocket           string `toml:"frame_socket,omitempty"`
	// PointerMode selects the hid.usb1 report format: "absolute" (iOS AssistiveTouch
	// plus limited hid.usb2 media keys) or "touchscreen" (Android HID digitizer
	// plus full hid.usb2 Android extension keys).
	PointerMode string `toml:"pointer_mode,omitempty"`
	// InputBackend selects the low-level input path for keyboard/touch tools:
	// "hid" writes USB HID reports, "adb" sends Android adb shell input commands.
	InputBackend string `toml:"input_backend,omitempty"`
}

type DeviceConfig struct {
	Backend    string `toml:"backend,omitempty"`
	DeviceType string `toml:"device_type,omitempty"`
}

func (d DeviceConfig) BackendOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(d.Backend)) {
	case "", "hdmi", "hardware":
		return "hdmi"
	default:
		return strings.ToLower(strings.TrimSpace(d.Backend))
	}
}

func normalizeDeviceType(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "ios":
		return "iOS", true
	case "android":
		return "Android", true
	case "macos", "mac", "darwin":
		return "macOS", true
	case "windows", "win":
		return "windows", true
	case "linux":
		return "linux", true
	default:
		return "", false
	}
}

func (d DeviceConfig) DeviceTypeOrDefault() string {
	if deviceType, ok := normalizeDeviceType(d.DeviceType); ok {
		return deviceType
	}
	return defaultDeviceType
}

func (d DeviceConfig) PlatformOrDefault() string {
	return deviceTypePlatform(d.DeviceTypeOrDefault())
}

func (d DeviceConfig) PointerModeOrDefault() string {
	if d.PlatformOrDefault() == "android" {
		return "touchscreen"
	}
	return "absolute"
}

func deviceTypePlatform(deviceType string) string {
	switch normalized, _ := normalizeDeviceType(deviceType); normalized {
	case "iOS":
		return "ios"
	case "Android":
		return "android"
	case "macOS":
		return "macos"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return "ios"
	}
}

func (c Config) DeviceTypeOrDefault() string {
	if strings.TrimSpace(c.Device.DeviceType) != "" {
		return c.Device.DeviceTypeOrDefault()
	}
	return defaultDeviceType
}

func (c Config) DevicePlatformOrDefault() string {
	return deviceTypePlatform(c.DeviceTypeOrDefault())
}

func (c Config) PointerModeOrDefault() string {
	return DeviceConfig{DeviceType: c.DeviceTypeOrDefault()}.PointerModeOrDefault()
}

func (c Config) HIDConfigForDevice() HIDConfig {
	hid := c.HID
	hid.PointerMode = c.PointerModeOrDefault()
	return hid
}

// OverrideDeviceType applies a process-local device type override and derives
// dependent device settings from the canonical value.
func (c *Config) OverrideDeviceType(value string) error {
	if c == nil {
		return errors.New("cannot override device type on nil config")
	}
	deviceType, ok := normalizeDeviceType(value)
	if !ok {
		return fmt.Errorf("invalid device type override: %s (expected iOS, Android, macOS, windows, or linux)", value)
	}
	c.Device.DeviceType = deviceType
	c.DeviceTypeOverride = deviceType
	c.HID.PointerMode = DeviceConfig{DeviceType: deviceType}.PointerModeOrDefault()
	return nil
}

func (h HIDConfig) KeyboardDeviceOrDefault() string {
	if h.KeyboardDevice != "" {
		return h.KeyboardDevice
	}
	return defaultKeyboardDevice
}

func (h HIDConfig) KeyboardLayoutOrDefault() string {
	layout, _ := mnk.NormalizeKeyboardLayout(h.KeyboardLayout)
	return layout
}

func (h HIDConfig) MouseDeviceOrDefault() string {
	if h.MouseDevice != "" {
		return h.MouseDevice
	}
	return defaultMouseDevice
}

func (h HIDConfig) AndroidKeyboardDeviceOrDefault() string {
	if h.AndroidKeyboardDevice != "" {
		return h.AndroidKeyboardDevice
	}
	return defaultAndroidKeyboardDevice
}

func (h HIDConfig) FrameSocketOrDefault() string {
	if h.FrameSocket != "" {
		return h.FrameSocket
	}
	return defaultFrameServiceSocket
}

func (h HIDConfig) PointerModeOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(h.PointerMode)) {
	case "touchscreen":
		return "touchscreen"
	default:
		return "absolute"
	}
}

func (h HIDConfig) PointerTouchscreen() bool {
	return h.PointerModeOrDefault() == "touchscreen"
}

func (h HIDConfig) InputBackendOrDefault() string {
	switch strings.ToLower(strings.TrimSpace(h.InputBackend)) {
	case "adb":
		return "adb"
	default:
		return "hid"
	}
}

func (h HIDConfig) InputBackendADB() bool {
	return h.InputBackendOrDefault() == "adb"
}

type ModelConfig struct {
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	BaseURL  string `toml:"-"`
	APIKey   string `toml:"api_key,omitempty"`
	// APIMode selects the model wire protocol. Empty and "chat_completions"
	// preserve the historical default; "responses" and "responses_stateful"
	// select OpenAI-compatible Responses transports. Gemini also supports its
	// native "interactions" (local StepList) and "interactions_stateful"
	// (previous_interaction_id) modes.
	APIMode string `toml:"api_mode,omitempty"`
	// ResponsesContextManagement selects provider-side context management for
	// Responses requests. "compaction" is OpenAI's token-based policy and
	// "ark_context_edit" is Volcengine Ark's tool/thinking edit policy.
	ResponsesContextManagement        string `toml:"responses_context_management,omitempty"`
	ResponsesCompactThreshold         int    `toml:"responses_compact_threshold,omitempty"`
	ResponsesContextEditTrigger       int    `toml:"responses_context_edit_trigger,omitempty"`
	ResponsesContextEditKeep          int    `toml:"responses_context_edit_keep,omitempty"`
	ResponsesContextEditClearThinking bool   `toml:"responses_context_edit_clear_thinking,omitempty"`
	// ResponsesTruncation controls the OpenAI-compatible truncation policy used
	// by OpenAI and OpenRouter. Ark does not use this request field.
	// Empty preserves the API default (disabled); "auto" delegates truncation
	// to the provider when supported.
	ResponsesTruncation string `toml:"responses_truncation,omitempty"`
	// ResponsesInclude lists official Responses include values such as
	// reasoning.encrypted_content. It is intentionally provider-configurable
	// because compatible gateways do not all implement the same include set.
	ResponsesInclude []string `toml:"responses_include,omitempty"`
	// Temperature is a pointer so nil (unset) is distinct from an explicit 0.0.
	// Unset means the effective value is resolved at runtime from model metadata
	// or left to providers whose defaults Aiden does not know (see
	// applyModelTemperatureDefault). An explicit value, including 0, is always
	// honored and sent to the provider.
	Temperature       *float64 `toml:"temperature,omitempty"`
	MaxResponseTokens int      `toml:"max_response_tokens,omitempty"`
	LogRawHTTP        bool     `toml:"log_raw_http,omitempty"`
	ReasoningEffort   string   `toml:"reasoning_effort,omitempty"`
	// ReasoningBudgetTokens is an optional exact budget for models whose native
	// reasoning control is budget_tokens (for example older Claude models). A
	// zero value delegates to the model's default or the reasoning effort
	// preset; it is ignored by effort-only providers.
	ReasoningBudgetTokens int `toml:"reasoning_budget_tokens,omitempty"`
	// These override static model metadata; zero means use the registry/fallback.
	ContextWindow        int      `toml:"context_window,omitempty"`
	ModelMaxOutputTokens int      `toml:"model_max_output_tokens,omitempty"`
	Responses            []string `toml:"responses,omitempty"`
}

func (m ModelConfig) ResponsesProviderCompactionEnabled() bool {
	apiMode := normalizeModelAPIMode(m.APIMode)
	return (apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful) &&
		effectiveModelProviderType(m.Provider, m.BaseURL) == "openai" &&
		normalizeResponsesContextManagement(m.ResponsesContextManagement) == responsesContextManagementCompaction
}

// AgentConfig is used internally by the runtime prompt builder.
type AgentConfig struct {
	Instruction string
	Prompt      string
	Locale      string
	Timezone    string
}

// MemoryConfig is used internally by the memory manager.
type MemoryConfig struct {
	Type       string
	WindowSize int
	MemoryKey  string
}

func LoadConfigFromDir(configDir string) (Config, error) {
	return loadConfigFromDir(configDir, LoadConfig)
}

// LoadRuntimeConfigFromDir loads the daemon-facing runtime config from a config
// directory. It preserves the historic agent.toml -> agent.json lookup while
// returning a config with runtime defaults resolved.
func LoadRuntimeConfigFromDir(configDir string) (Config, error) {
	return loadConfigFromDir(configDir, LoadRuntimeConfig)
}

type configFileLoader func(string) (Config, error)

func loadConfigFromDir(configDir string, load configFileLoader) (Config, error) {
	configPath := filepath.Join(configDir, "agent.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		// Fallback to .json for backward compatibility
		configPath = filepath.Join(configDir, "agent.json")
	}

	cfg, err := load(configPath)
	if err != nil {
		return Config{}, err
	}

	// Store the config directory for logger and other uses
	cfg.ConfigDir = configDir

	skillsDir := filepath.Join(configDir, "skills")
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		cfg.SkillsDirs = []string{skillsDir}
	} else {
		cfg.SkillsDirs = []string{}
	}

	if cfg.BundledSkillsDir == "" {
		cfg.BundledSkillsDir = resolveBundledSkillsDir()
	}

	return cfg, nil
}

func resolveBundledSkillsDir() string {
	if v := os.Getenv("AIDEN_BUNDLED_SKILLS_DIR"); v != "" {
		return v
	}
	for _, dir := range bundledSkillsDirCandidates() {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

func bundledSkillsDirCandidates() []string {
	return []string{"/usr/share/aiden/skills"}
}

func LoadConfig(path string) (Config, error) {
	var cfg Config

	if _, err := decodeConfigFile(path, &cfg); err != nil {
		return Config{}, err
	}
	applyRuntimeInstructionDefault(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// LoadRuntimeConfig loads a config file over runtime defaults and returns the
// effective config used by the daemon. Optional speech providers remain opt-in:
// their provider fields are not inherited from DefaultConfig unless the raw
// config explicitly sets them.
func LoadRuntimeConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	metadata, err := decodeConfigFile(path, &cfg)
	if err != nil {
		return Config{}, err
	}

	applyRuntimeOptionalProviderDefaults(&cfg, metadata, true)
	applyVoiceModelProviderDefaults(&cfg, metadata)
	applyDeviceConfigDefaults(&cfg, metadata)

	// Upgrade the legacy voice shapes to named records. This must run after the
	// defaults pass above -- that pass zeroes [tts]/[stt] when the file declares
	// no provider, and migrating first would mint a record out of DefaultConfig
	// for a device that never configured voice at all.
	migrateLegacyVoiceProviders(&cfg, metadata)

	// Expand the voice provider references. Neither call fails: voice is
	// optional at runtime (a TTS init failure is a warning and the agent still
	// starts), so a stale reference must not stop the device from booting. The
	// config page runs Config.ValidateVoiceProviders on save for strict checks.
	resolveTTSProvider(&cfg)
	resolveSTTProvider(&cfg)
	resolveVoiceModelProvider(&cfg)
	cfg.VoiceModel.APIKey = resolveProviderAPIKey(cfg.VoiceModel.APIKey)
	// A stale persisted realtime mode must not brick the daemon when its
	// credential was removed or never configured. Runtime startup falls back to
	// text mode so Config Web can come up and repair the persisted value. The
	// strict Config.Validate() path still rejects explicitly saved realtime
	// configurations without a credential.
	if strings.EqualFold(strings.TrimSpace(cfg.InputMode), "realtime") && !cfg.VoiceModel.Enabled() {
		cfg.InputMode = defaultInputMode
	}

	// Apply provider references to model configurations. This must run before
	// the base_url whitelist below: until the reference is expanded, Provider
	// still holds a [model_providers] section name rather than a provider type, so
	// the whitelist would compare against the wrong value in both directions.
	if err := applyRuntimeModelProviders(&cfg); err != nil {
		return Config{}, err
	}

	// base_url belongs to [model_providers.*]. Drop provider-record values for
	// provider types whose model builders pin their own endpoint.
	clearNonAllowedModelBaseURL(&cfg.Model)

	applyRuntimeModelTemperatureDefaults(&cfg)
	applyRuntimeModelReasoningEffortDefaults(&cfg)
	applyRuntimeInstructionDefault(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func applyDeviceConfigDefaults(cfg *Config, metadata toml.MetaData) {
	if cfg == nil {
		return
	}
	deviceTypeConfigured := metadata.IsDefined("device", "device_type") && strings.TrimSpace(cfg.Device.DeviceType) != ""
	if !deviceTypeConfigured {
		cfg.Device.DeviceType = defaultDeviceType
	} else if deviceType, ok := normalizeDeviceType(cfg.Device.DeviceType); ok {
		cfg.Device.DeviceType = deviceType
	}
	cfg.HID.PointerMode = cfg.PointerModeOrDefault()
}

// applyRuntimeModelTemperatureDefaults resolves the sampling temperature for
// the model when the user has not set it. The default is sourced from the
// model's metadata (some models, e.g. Kimi K3, require a fixed temperature).
// Native Gemini models without a documented default stay unset so Google can
// choose the model default; other providers fall back to
// defaultModelTemperature. An explicit model.temperature always takes
// precedence. This is only called in LoadRuntimeConfig; LoadResolvedConfig
// (config editor) keeps temperature unset so the editor displays empty and saves
// without baking defaults into agent.toml.
func applyRuntimeModelTemperatureDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	applyModelTemperatureDefault(&cfg.Model)
}

func applyModelTemperatureDefault(m *ModelConfig) {
	if m == nil || m.Temperature != nil {
		return
	}
	if spec, ok := LookupModelSpec(m.Provider, m.Model); ok && spec.DefaultTemperature != nil {
		// Copy the value rather than aliasing the registry pointer.
		temp := *spec.DefaultTemperature
		m.Temperature = &temp
		return
	}
	// Some native providers recommend using each model's own default. If the
	// model registry has no reliable value for one of those providers, omit
	// temperature rather than substituting Aiden's cross-provider fallback. The
	// provider has already been resolved by applyRuntimeModelProviders, including
	// named provider records.
	if modelProviderUsesProviderTemperatureDefault(m.Provider) {
		return
	}
	temp := defaultModelTemperature
	m.Temperature = &temp
}

// applyRuntimeModelReasoningEffortDefaults resolves reasoning_effort for the
// model when the user has not set it (empty string). The default is
// sourced from the model's metadata; forced-reasoning models (e.g. Kimi K3)
// pin a lighter effort to keep streaming responsive. Unlike temperature there
// is no global fallback: unknown models stay in auto mode (empty). An explicit
// model.reasoning_effort always takes precedence. Like the temperature defaults,
// this only runs in LoadRuntimeConfig; LoadResolvedConfig (config editor) keeps
// reasoning_effort as-is so the editor does not bake a default into agent.toml.
func applyRuntimeModelReasoningEffortDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	applyModelReasoningEffortDefault(&cfg.Model)
}

func applyModelReasoningEffortDefault(m *ModelConfig) {
	if m == nil || strings.TrimSpace(m.ReasoningEffort) != "" {
		return
	}
	if spec, ok := LookupModelSpec(m.Provider, m.Model); ok && spec.DefaultReasoningEffort != nil {
		m.ReasoningEffort = *spec.DefaultReasoningEffort
	}
}

// applyRuntimeModelProviders resolves provider references in model configurations.
// If model.provider refers to a named provider in [model_providers], apply that provider's
// configuration. Otherwise, treat it as a direct provider type (backward compatibility).
func applyRuntimeModelProviders(cfg *Config) error {
	if cfg == nil {
		return nil
	}

	if err := resolveModelProvider(cfg, &cfg.Model); err != nil {
		return fmt.Errorf("model: %w", err)
	}

	return nil
}

// resolveModelProvider resolves a single model's provider reference.
func resolveModelProvider(cfg *Config, m *ModelConfig) error {
	if m == nil {
		return nil
	}

	providerRef := strings.TrimSpace(m.Provider)
	if providerRef == "" {
		return errors.New("provider is required")
	}

	// Check if this is a reference to a named provider in [model_providers]
	if cfg.ModelProviders != nil {
		if provider, exists := cfg.ModelProviders[providerRef]; exists {
			return applyProviderToModel(provider, providerRef, m)
		}
	}

	// Not a reference, treat as direct provider type (backward compatibility)
	return nil
}

// applyProviderToModel applies a provider configuration to a model config.
func applyProviderToModel(provider ModelProvider, originalRef string, m *ModelConfig) error {
	providerType := effectiveModelProviderType(provider.Type, provider.BaseURL)
	if providerType == "" {
		return fmt.Errorf("provider %q has no provider type specified", originalRef)
	}

	// Replace the reference with the actual provider type
	m.Provider = providerType

	// api_key may be overridden on [model]. base_url belongs to the selected
	// provider record so switching providers also switches the endpoint.
	if m.APIKey == "" && provider.APIKey != "" {
		m.APIKey = provider.APIKey
	}
	m.BaseURL = provider.BaseURL

	return nil
}

// applyRuntimeOptionalProviderDefaults clears the speech providers DefaultConfig
// seeds when the file does not declare them, because the runtime treats them as
// opt-in.
//
// declared reports whether metadata came from a file that was actually read. An
// absent agent.toml still resolves to the built-in defaults (the documented
// first-boot behavior, and the state the config page renders so a save can
// create the file), so it must not be treated as "the file declared nothing":
// zeroing there would turn a device with no config file into a semantically
// invalid configuration.
func applyRuntimeOptionalProviderDefaults(cfg *Config, metadata toml.MetaData, declared bool) {
	if cfg == nil {
		return
	}
	if !declared {
		return
	}

	if !metadata.IsDefined("tts", "provider") || strings.TrimSpace(cfg.TTS.Provider) == "" {
		cfg.TTS = TTSConfig{}
	} else {
		// Normalizing lowercases, so it must not touch a [tts_providers] record
		// name: record names come from the config page and may carry capitals,
		// and a lowercased name would stop matching its own record. Bare
		// provider types still normalize, which is what folds the minimax-ws
		// alias. resolveTTSProvider normalizes the resolved type afterwards.
		if _, isRef := cfg.TTSProviders[strings.TrimSpace(cfg.TTS.Provider)]; !isRef {
			cfg.TTS.Provider = normalizeTTSProvider(cfg.TTS.Provider)
		}
		if cfg.TTS.Provider != defaultTTSProvider {
			clearDefaultTTSProviderFields(cfg, metadata)
		}
	}
	if !metadata.IsDefined("stt", "provider") || strings.TrimSpace(cfg.STT.Provider) == "" {
		cfg.STT = STTConfig{}
	} else if !usesDefaultSTTModel(cfg.STT.Provider) && !metadata.IsDefined("stt", "model") {
		cfg.STT.Model = ""
	}
}

// applyVoiceModelProviderDefaults prevents Qwen defaults from leaking into a
// different realtime provider when those fields were not explicitly set.
func applyVoiceModelProviderDefaults(cfg *Config, metadata toml.MetaData) {
	if cfg == nil || strings.EqualFold(strings.TrimSpace(cfg.VoiceModel.Provider), "qwen") {
		return
	}
	if !metadata.IsDefined("voice_model", "model") {
		cfg.VoiceModel.Model = ""
	}
	if !metadata.IsDefined("voice_model", "voice") {
		cfg.VoiceModel.Voice = ""
	}
	if !metadata.IsDefined("voice_model", "region") {
		cfg.VoiceModel.Region = ""
	}
	if !metadata.IsDefined("voice_model", "turn_detection") {
		cfg.VoiceModel.TurnDetection = ""
	}
	if !metadata.IsDefined("voice_model", "turn_detection_threshold") {
		cfg.VoiceModel.TurnDetectionThreshold = nil
	}
	if !metadata.IsDefined("voice_model", "turn_detection_silence_ms") {
		cfg.VoiceModel.TurnDetectionSilenceMs = 0
	}
}

func clearNonAllowedModelBaseURL(m *ModelConfig) {
	if m == nil {
		return
	}
	definition, ok := lookupModelProviderDefinition(m.Provider)
	if !ok || !definition.allowsCustomBaseURL {
		m.BaseURL = ""
	}
}

func normalizeTTSProvider(provider string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(provider)); normalized {
	case "minimax-ws":
		return defaultTTSProvider
	default:
		return normalized
	}
}

func clearDefaultTTSProviderFields(cfg *Config, metadata toml.MetaData) {
	if !metadata.IsDefined("tts", "model") {
		cfg.TTS.Model = ""
	}
	if !metadata.IsDefined("tts", "voice_id") {
		cfg.TTS.VoiceID = ""
	}
	if !metadata.IsDefined("tts", "emotion") {
		cfg.TTS.Emotion = ""
	}
	if !metadata.IsDefined("tts", "reference_id") {
		cfg.TTS.ReferenceID = ""
	}
	if !metadata.IsDefined("tts", "speed") {
		cfg.TTS.Speed = 0
	}
}

func usesDefaultSTTModel(provider string) bool {
	canonicalProvider, ok := canonicalSTTProviderType(provider)
	return ok && canonicalProvider == defaultSTTProvider
}

// LoadResolvedConfig loads the TOML config file at path over the canonical
// defaults and returns the effective values used by config-editing surfaces.
// The path must identify a file, not a config directory. Missing TOML files are
// treated as "all defaults" so first-boot config pages can render before
// agent.toml has been created.
func LoadResolvedConfig(path string) (Config, error) {
	cfg, err := loadResolvedConfig(path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadResolvedConfigForUpdate loads the config editor view without validating
// the existing values. This is intentionally only for recovery updates: a
// config web save may need to repair a persisted invalid field (for example,
// an old input_mode=realtime without voice_model.api_key) before the candidate
// config can be validated and persisted.
func LoadResolvedConfigForUpdate(path string) (Config, error) {
	return loadResolvedConfig(path)
}

func loadResolvedConfig(path string) (Config, error) {
	exists, err := inspectConfigFilePath(path)
	if err != nil {
		return Config{}, err
	}

	cfg := DefaultConfig()
	var metadata toml.MetaData
	if exists {
		if metadata, err = decodeConfigFile(path, &cfg); err != nil {
			return Config{}, err
		}
		applyDeviceConfigDefaults(&cfg, metadata)
	} else {
		cfg.HID.PointerMode = cfg.PointerModeOrDefault()
	}
	// Optional speech providers are opt-in at runtime, so the editor must not show
	// the DefaultConfig provider for a file that never declared one. The page would
	// otherwise display (and label as required) a value Config Web reports as
	// missing, and re-saving that displayed value is not a change config-update can
	// persist, which leaves the flagged field unrepairable. Sharing the rule with
	// LoadRuntimeConfig also keeps `config-check` agreeing with the recovery state
	// the portal reports.
	//
	// A missing file is not "declared nothing": it resolves to the built-in
	// defaults so the page can render and a save can create the file.
	applyRuntimeOptionalProviderDefaults(&cfg, metadata, exists)
	applyVoiceModelProviderDefaults(&cfg, metadata)

	applyRuntimeInstructionDefault(&cfg)

	// Upgrade flat voice credentials to named records. This backs
	// `agent config --format=json`, which is what the config page reads through,
	// so it has to run here as well as in LoadRuntimeConfig: without it a flat
	// config reaches the page as flat fields with no record, leaving the key
	// invisible and un-editable.
	//
	// References are deliberately NOT resolved here. The page edits the
	// reference, so it must come back as the name it wrote.
	migrateLegacyVoiceProviders(&cfg, metadata)

	return cfg, nil
}

func inspectConfigFilePath(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, errors.New("config path is required")
	}
	if !strings.HasSuffix(path, ".toml") {
		return false, fmt.Errorf("config path must end with .toml: %q", path)
	}

	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read config: %w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return false, fmt.Errorf("resolve config symlink target %q: %w", path, err)
		}
	}

	if !info.Mode().IsRegular() {
		if info.IsDir() {
			return false, fmt.Errorf("config path must be a regular file, got directory: %q", path)
		}
		return false, fmt.Errorf("config path must be a regular file: %q", path)
	}
	return true, nil
}

// applyRuntimeInstructionDefault pins Config.Instruction to the built-in runtime
// instruction. The field is no longer file-configurable (the legacy
// `custom_instruction` key is read and ignored), so this is the single place
// that decides the base instruction for every loader, including the ones that
// do not start from DefaultConfig.
func applyRuntimeInstructionDefault(cfg *Config) {
	if cfg == nil {
		return
	}
	if strings.TrimSpace(cfg.Instruction) == "" {
		cfg.Instruction = defaultInstruction
	}
}

func decodeConfigFile(path string, cfg *Config) (toml.MetaData, error) {
	var metadata toml.MetaData
	var runtimeMap map[string]interface{}

	// Determine format by file extension
	if strings.HasSuffix(path, ".toml") {
		var err error
		// The on-disk schema is grouped by the product settings shown in the
		// configuration UI. Runtime code intentionally keeps its established
		// fields, so normalize the grouped document once at the boundary.
		var grouped map[string]interface{}
		if _, err = toml.DecodeFile(path, &grouped); err != nil {
			return toml.MetaData{}, fmt.Errorf("decode TOML config: %w", err)
		}
		if err = validateGroupedConfigRoot(grouped); err != nil {
			return toml.MetaData{}, err
		}
		runtimeMap = groupedConfigToRuntime(grouped)
		encoded, err := toml.Marshal(runtimeMap)
		if err != nil {
			return toml.MetaData{}, fmt.Errorf("normalize TOML config: %w", err)
		}
		if metadata, err = toml.Decode(string(encoded), cfg); err != nil {
			return toml.MetaData{}, fmt.Errorf("decode TOML config: %w", err)
		}
		if metadata.IsDefined("providers") {
			return toml.MetaData{}, errors.New("[providers] is unsupported; use [model_providers]")
		}
	} else {
		_, err := os.Stat(path)
		if err != nil {
			return toml.MetaData{}, fmt.Errorf("read config: %w", err)
		}
		return toml.MetaData{}, fmt.Errorf("JSON format is deprecated, please use TOML format: %s", path)
	}

	if err := applyLegacyModelMaxTokens(path, metadata, cfg, runtimeMap); err != nil {
		return toml.MetaData{}, err
	}
	if err := applyLegacyAudioBackend(path, metadata, cfg, runtimeMap); err != nil {
		return toml.MetaData{}, err
	}
	applyLegacyContextPruneThreshold(cfg)
	return metadata, nil
}

// groupedConfigToRuntime translates the canonical, product-oriented TOML
// hierarchy into the runtime field names used by the daemon. The grouped
// hierarchy is the only schema written by Config Web; this adapter keeps the
// translation localized to the file boundary instead of spreading TOML names
// through the runtime implementation.
func groupedConfigToRuntime(grouped map[string]interface{}) map[string]interface{} {
	if grouped == nil {
		return map[string]interface{}{}
	}
	result := make(map[string]interface{}, len(grouped))
	moveTable := func(from []string, to string) {
		if value, ok := tableAt(grouped, from...); ok {
			mergeTable(result, to, value)
		}
	}
	moveField := func(from []string, to []string) {
		if value, ok := valueAtTable(grouped, from...); ok {
			setValue(result, to, value)
		}
	}

	moveField([]string{"basic_settings", "language_timezone", "locale"}, []string{"locale"})
	moveField([]string{"basic_settings", "language_timezone", "timezone"}, []string{"timezone"})
	// Advanced HID contains internal device fields. Merge it first so the
	// user-facing keyboard_layout under Basic Settings remains authoritative
	// if both tables are present.
	moveTable([]string{"advanced_settings", "hardware", "hid"}, "hid")
	if device, ok := tableAt(grouped, "basic_settings", "device"); ok {
		copyWithoutKey(device, result, "device", "hid")
		if hid, exists := device["hid"].(map[string]interface{}); exists {
			mergeTable(result, "hid", hid)
		}
	}
	mergeRootTable(grouped, result, []string{"conversation_settings", "agent"})
	moveTable([]string{"conversation_settings", "search"}, "search")
	moveTable([]string{"conversation_settings", "termination_policy"}, "termination_policy")
	moveTable([]string{"model_settings", "model"}, "model")
	moveTable([]string{"model_settings", "providers"}, "model_providers")
	moveTable([]string{"model_settings", "decision"}, "decision")
	moveTable([]string{"voice_settings", "realtime", "providers"}, "voice_model_providers")
	mergeRootTable(grouped, result, []string{"voice_settings", "mode"})
	mergeRootTable(grouped, result, []string{"voice_settings", "classic", "runtime"})
	if realtime, ok := tableAt(grouped, "voice_settings", "realtime"); ok {
		// Realtime session instructions now come from the built-in base plus
		// conversation_settings.agent.prompt, not this removed voice field.
		copyWithoutKey(realtime, result, "voice_model", "providers", "instructions")
	}
	moveTable([]string{"voice_settings", "classic", "stt"}, "stt")
	moveTable([]string{"voice_settings", "classic", "stt", "providers"}, "stt_providers")
	moveTable([]string{"voice_settings", "classic", "tts"}, "tts")
	moveTable([]string{"voice_settings", "classic", "tts", "providers"}, "tts_providers")
	moveTable([]string{"voice_settings", "classic", "audio"}, "audio")
	moveTable([]string{"voice_settings", "classic", "audio_archive"}, "audio_archive")
	moveTable([]string{"memory_settings", "screen"}, "quick_capture")
	moveTable([]string{"memory_settings", "notification"}, "voice_notifications")
	moveTable([]string{"storage_settings", "storage"}, "storage")
	// log_raw_http is exposed through the model API contract because it
	// controls model transport logging, but it belongs to the product's
	// Advanced Settings > Logs TOML group. Map the grouped path back to the
	// runtime ModelConfig field at the file boundary while keeping it out of
	// LogConfig, whose other fields are the local retention settings.
	if log, ok := tableAt(grouped, "advanced_settings", "log"); ok {
		logRuntime := make(map[string]interface{}, len(log))
		for key, value := range log {
			logRuntime[key] = value
		}
		if value, exists := logRuntime["log_raw_http"]; exists {
			model, _ := result["model"].(map[string]interface{})
			if model == nil {
				model = map[string]interface{}{}
			}
			model["log_raw_http"] = value
			result["model"] = model
			delete(logRuntime, "log_raw_http")
		}
		mergeTable(result, "log", logRuntime)
	}
	moveTable([]string{"advanced_settings", "hardware", "frame_service"}, "frame_service")
	moveTable([]string{"advanced_settings", "runtime", "live_activity"}, "live_activity")
	moveTable([]string{"advanced_settings", "runtime", "telemetry"}, "telemetry")
	moveTable([]string{"advanced_settings", "runtime", "ota"}, "ota")

	return result
}

var groupedConfigRoots = map[string]struct{}{
	"basic_settings":        {},
	"conversation_settings": {},
	"model_settings":        {},
	"voice_settings":        {},
	"memory_settings":       {},
	"storage_settings":      {},
	"advanced_settings":     {},
	"about":                 {},
}

func validateGroupedConfigRoot(grouped map[string]interface{}) error {
	for key := range grouped {
		if _, ok := groupedConfigRoots[key]; !ok {
			return fmt.Errorf("unsupported top-level TOML key %q: use the grouped configuration schema", key)
		}
	}
	return nil
}

func copyWithoutKey(source, result map[string]interface{}, target string, excluded ...string) {
	copy := make(map[string]interface{}, len(source))
	for key, value := range source {
		skip := false
		for _, excludedKey := range excluded {
			if key == excludedKey {
				skip = true
				break
			}
		}
		if !skip {
			copy[key] = value
		}
	}
	mergeTable(result, target, copy)
}

func tableAt(root map[string]interface{}, path ...string) (map[string]interface{}, bool) {
	value, ok := valueAtTable(root, path...)
	if !ok {
		return nil, false
	}
	table, ok := value.(map[string]interface{})
	return table, ok
}

func valueAtTable(root map[string]interface{}, path ...string) (interface{}, bool) {
	var value interface{} = root
	for _, key := range path {
		table, ok := value.(map[string]interface{})
		if !ok {
			return nil, false
		}
		value, ok = table[key]
		if !ok {
			return nil, false
		}
	}
	return value, true
}

func mergeTable(root map[string]interface{}, key string, value map[string]interface{}) {
	if existing, ok := root[key].(map[string]interface{}); ok {
		for child, childValue := range value {
			existing[child] = childValue
		}
		return
	}
	root[key] = value
}

func mergeRootTable(grouped, result map[string]interface{}, path []string) {
	table, ok := tableAt(grouped, path...)
	if !ok {
		return
	}
	for key, value := range table {
		result[key] = value
	}
}

func setValue(root map[string]interface{}, path []string, value interface{}) {
	if len(path) == 0 {
		return
	}
	table := root
	for _, key := range path[:len(path)-1] {
		next, ok := table[key].(map[string]interface{})
		if !ok {
			next = make(map[string]interface{})
			table[key] = next
		}
		table = next
	}
	table[path[len(path)-1]] = value
}

// applyLegacyContextPruneThreshold migrates context_prune_threshold from its
// original absolute-token form to the current fraction-of-budget form. The old
// field accepted values like 12000; the new one is a fraction in (0, 1), and
// TOML decodes an integer literal into the float64 field without complaint. A
// device upgrading with the old value would otherwise fail Validate and refuse
// to start.
//
// The old absolute value cannot be converted: it was compared against a token
// count derived from the active model's budget, which is unknown at config load
// time. Falling back to the default fraction reproduces the behaviour such a
// device had before it set the field, which is the closest safe outcome.
// validateContextThresholdFraction enforces the shared bounds for the context
// size thresholds: 0 selects the default, and any other value must lie strictly
// between 0 and 1. A value at or above 1.0 would let the transcript reach the
// whole usable budget before acting, defeating the purpose.
//
// NaN needs its own check because every comparison against it is false, so it
// would otherwise pass both the lower and upper bound and be stored raw. TOML
// accepts nan and inf literals into a float64 field, so this is reachable from a
// hand-edited config rather than only from code.
func validateContextThresholdFraction(field string, value float64) error {
	if math.IsNaN(value) {
		return fmt.Errorf("%s must be a number, got NaN", field)
	}
	if value < 0 || value >= 1 {
		return fmt.Errorf("%s must be 0 or in (0, 1), got %g", field, value)
	}
	return nil
}

func applyLegacyContextPruneThreshold(cfg *Config) {
	if cfg == nil {
		return
	}
	// Only a finite value >= 1 is a plausible legacy token count. NaN and +Inf
	// are not, so they are left alone for Validate to reject rather than being
	// silently replaced by the default, which would hide a malformed config.
	if math.IsNaN(cfg.ContextPruneThreshold) || math.IsInf(cfg.ContextPruneThreshold, 0) {
		return
	}
	if cfg.ContextPruneThreshold < 1 {
		return
	}
	logging.Warnf("agent", "config", "context_prune_threshold = %g looks like a token count; it is now a fraction of the usable input budget. Using the default %g. Set a value in (0, 1) to silence this.", cfg.ContextPruneThreshold, defaultContextPruneThreshold)
	cfg.ContextPruneThreshold = defaultContextPruneThreshold
}

func applyLegacyAudioBackend(path string, metadata toml.MetaData, cfg *Config, runtimeMap map[string]interface{}) error {
	if cfg == nil || metadata.IsDefined("audio", "backend") || !metadata.IsDefined("audio", "playback_backend") {
		return nil
	}
	audio, ok := runtimeMap["audio"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("decode legacy audio backend: audio is not a TOML table in %s", path)
	}
	playbackBackend, ok := audio["playback_backend"].(string)
	if !ok {
		return fmt.Errorf("decode legacy audio backend: audio.playback_backend must be a string")
	}
	cfg.Audio.Backend = playbackBackend
	return nil
}

func applyLegacyModelMaxTokens(path string, metadata toml.MetaData, cfg *Config, runtimeMap map[string]interface{}) error {
	needsModel := metadata.IsDefined("model", "max_tokens") &&
		!metadata.IsDefined("model", "max_response_tokens")
	if !needsModel {
		return nil
	}

	if needsModel {
		value, err := legacyModelMaxTokens(runtimeMap, "model")
		if err != nil {
			return err
		}
		cfg.Model.MaxResponseTokens = value
	}
	return nil
}

func legacyModelMaxTokens(raw map[string]interface{}, section string) (int, error) {
	table, ok := raw[section].(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("%s.max_tokens is defined but %s is not a TOML table", section, section)
	}
	value, ok := table["max_tokens"]
	if !ok {
		return 0, fmt.Errorf("%s.max_tokens is defined but could not be decoded", section)
	}
	tokens, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("%s.max_tokens must be an integer", section)
	}
	return int(tokens), nil
}

func (c Config) Validate() error {
	// Validate providers. Iterate in sorted order so a config with several
	// broken sections reports the same error every run.
	providerNames := make([]string, 0, len(c.ModelProviders))
	for name := range c.ModelProviders {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)
	for _, name := range providerNames {
		if err := validateModelProvider(name, c.ModelProviders[name]); err != nil {
			return err
		}
	}

	if err := validateModelProviderRef("model", c.ModelProviders, c.Model.Provider); err != nil {
		return err
	}

	locale := strings.TrimSpace(c.Locale)
	switch locale {
	case "", localeSimplifiedChinese, localeEnglishUS:
	default:
		return fmt.Errorf("invalid locale: %s (expected zh-CN or en-US)", c.Locale)
	}
	if _, _, err := loadConfiguredTimezone(c.TimezoneOrDefault()); err != nil {
		return err
	}

	switch c.Search.ProviderOrDefault() {
	case searchProviderDuckDuckGo:
	case searchProviderTavily:
		if searchAPIKeyOrEnv(c.Search.APIKey) == "" {
			return errors.New("search.api_key is required when search.provider=tavily")
		}
	case searchProviderBrave:
		if searchAPIKeyOrEnv(c.Search.APIKey, braveSearchAPIKeyEnv) == "" {
			return errors.New("search.api_key or BRAVE_SEARCH_API_KEY is required when search.provider=brave")
		}
	default:
		return fmt.Errorf("invalid search.provider: %s (expected duckduckgo, brave, or tavily)", c.Search.Provider)
	}
	if err := c.Decision.validate(); err != nil {
		return err
	}

	if strings.TrimSpace(c.Model.Provider) == "" {
		return errors.New("model.provider is required")
	}
	if strings.TrimSpace(c.Model.Model) == "" && strings.ToLower(c.Model.Provider) != "fake" {
		return errors.New("model.model is required")
	}
	apiMode := normalizeModelAPIMode(c.Model.APIMode)
	if apiMode == "" {
		return fmt.Errorf("invalid model.api_mode: %s (expected chat_completions, responses, responses_stateful, interactions, or interactions_stateful)", c.Model.APIMode)
	}
	if (apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful) && !c.modelProviderSupportsResponses() {
		return fmt.Errorf("model.api_mode=%s requires a provider transport with an OpenAI-compatible /responses endpoint", apiMode)
	}
	if apiMode == modelAPIModeResponsesStateful && !c.modelProviderSupportsResponsesStateful() {
		return fmt.Errorf("model.api_mode=responses_stateful requires a provider that supports stored Responses and previous_response_id; use responses for stateless-compatible endpoints")
	}
	if (apiMode == modelAPIModeInteractions || apiMode == modelAPIModeInteractionsStateful) && !c.modelProviderSupportsInteractions() {
		return fmt.Errorf("model.api_mode=%s requires the native Gemini Interactions transport", apiMode)
	}
	if apiMode == modelAPIModeInteractionsStateful && !c.modelProviderSupportsInteractionsStateful() {
		return fmt.Errorf("model.api_mode=interactions_stateful requires a provider that supports stored Gemini Interactions and previous_interaction_id; use interactions for local context")
	}
	// An unset api_mode is normalized to chat_completions for historical
	// compatibility, but some providers have no compatible transport at all. Only
	// reject the mode when it was configured explicitly, so a minimal provider
	// record still resolves to that provider's native default.
	if apiMode == modelAPIModeChatCompletions && strings.TrimSpace(c.Model.APIMode) != "" && !c.modelProviderSupportsChatCompletions() {
		return fmt.Errorf("model.api_mode=chat_completions is not supported by provider type %s; leave api_mode empty or use its native transport", c.modelProviderType())
	}
	contextManagement := strings.ToLower(strings.TrimSpace(c.Model.ResponsesContextManagement))
	switch contextManagement {
	case "", "none", "disabled", responsesContextManagementCompaction:
	case responsesContextManagementArkEdits, "ark", "volcengine", "ark-edits":
	default:
		return fmt.Errorf("invalid model.responses_context_management: %s (expected empty, compaction, ark_context_edit, or disabled)", c.Model.ResponsesContextManagement)
	}
	if threshold := c.Model.ResponsesCompactThreshold; threshold != 0 && threshold < 1000 {
		return fmt.Errorf("model.responses_compact_threshold must be 0 or >= 1000, got %d", threshold)
	}
	if err := validateContextThresholdFraction("context_prune_threshold", c.ContextPruneThreshold); err != nil {
		return err
	}
	if err := validateContextThresholdFraction("context_compaction_threshold", c.ContextCompactionThreshold); err != nil {
		return err
	}
	if c.Model.ResponsesContextEditTrigger < 0 {
		return fmt.Errorf("model.responses_context_edit_trigger must be >= 0, got %d", c.Model.ResponsesContextEditTrigger)
	}
	if c.Model.ResponsesContextEditKeep < 0 {
		return fmt.Errorf("model.responses_context_edit_keep must be >= 0, got %d", c.Model.ResponsesContextEditKeep)
	}
	for _, include := range c.Model.ResponsesInclude {
		if strings.TrimSpace(include) == "" {
			return errors.New("model.responses_include entries must be non-empty")
		}
	}
	truncation := strings.ToLower(strings.TrimSpace(c.Model.ResponsesTruncation))
	switch truncation {
	case "", responsesTruncationDisabled, responsesTruncationAuto:
	default:
		return fmt.Errorf("invalid model.responses_truncation: %s (expected empty, auto, or disabled)", c.Model.ResponsesTruncation)
	}
	backend := c.Device.BackendOrDefault()
	switch backend {
	case "hdmi":
	default:
		return fmt.Errorf("invalid device.backend: %s (expected hdmi)", c.Device.Backend)
	}
	if _, ok := normalizeDeviceType(c.Device.DeviceType); !ok {
		return fmt.Errorf("invalid device.device_type: %s (expected iOS, Android, macOS, windows, or linux)", c.Device.DeviceType)
	}
	if _, err := normalizeAudioBackend(c.Audio.Backend); err != nil {
		return err
	}
	resolveVoiceModelProvider(&c)
	if err := c.VoiceModel.Validate(); err != nil {
		return err
	}
	if c.Model.MaxResponseTokens < 0 {
		return fmt.Errorf("model.max_response_tokens must be >= 0, got %d", c.Model.MaxResponseTokens)
	}
	if c.Model.ContextWindow < 0 {
		return fmt.Errorf("model.context_window must be >= 0, got %d", c.Model.ContextWindow)
	}
	if c.Model.ModelMaxOutputTokens < 0 {
		return fmt.Errorf("model.model_max_output_tokens must be >= 0, got %d", c.Model.ModelMaxOutputTokens)
	}
	if c.Model.ReasoningBudgetTokens < 0 {
		return fmt.Errorf("model.reasoning_budget_tokens must be >= 0, got %d", c.Model.ReasoningBudgetTokens)
	}
	// reasoning_budget_tokens maps to Anthropic's native budget_tokens control.
	// Other providers may
	// leave a stale value in config, but it must not be validated or translated
	// as if their APIs used Anthropic's budget_tokens field.
	if c.modelProviderType() == "anthropic" && c.Model.ReasoningBudgetTokens > 0 {
		if c.Model.ReasoningBudgetTokens < minReasoningBudgetTokens {
			return fmt.Errorf("model.reasoning_budget_tokens must be 0 or >= %d, got %d",
				minReasoningBudgetTokens, c.Model.ReasoningBudgetTokens)
		}
		if c.Model.MaxResponseTokens > 0 && c.Model.ReasoningBudgetTokens >= c.Model.MaxResponseTokens {
			return fmt.Errorf("model.reasoning_budget_tokens (%d) must be less than model.max_response_tokens (%d); reasoning tokens are included in the response limit",
				c.Model.ReasoningBudgetTokens, c.Model.MaxResponseTokens)
		}
		if c.Model.ModelMaxOutputTokens > 0 && c.Model.ReasoningBudgetTokens >= c.Model.ModelMaxOutputTokens {
			return fmt.Errorf("model.reasoning_budget_tokens (%d) must be less than model.model_max_output_tokens (%d)",
				c.Model.ReasoningBudgetTokens, c.Model.ModelMaxOutputTokens)
		}
	}
	// Validate input_mode
	if strings.TrimSpace(c.InputMode) != "" {
		mode := strings.ToLower(strings.TrimSpace(c.InputMode))
		switch mode {
		case "stt":
			if strings.TrimSpace(c.STT.Provider) == "" {
				return errors.New("stt.provider is required when input_mode=stt")
			}
		case "realtime":
			if !c.VoiceModel.Enabled() {
				return errors.New("voice_model.api_key is required when input_mode=realtime")
			}
		case "text", "audio":
			return fmt.Errorf("invalid input_mode: %s (text and audio modes have been removed; use stt or realtime)", c.InputMode)
		default:
			return fmt.Errorf("invalid input_mode: %s (expected stt or realtime)", c.InputMode)
		}

		// Validate TTS/STT config only for the legacy STT audio path.
		if mode == "stt" && strings.TrimSpace(c.TTS.Provider) == "" {
			return errors.New("tts.provider is required when input_mode=stt")
		}

		if mode == "stt" || mode == "realtime" {
			if c.Audio.SampleRate != 0 && c.Audio.SampleRate < 8000 {
				return fmt.Errorf("audio.sample_rate must be at least 8000 when set, got %d", c.Audio.SampleRate)
			}
			if c.Audio.Channels != 0 && c.Audio.Channels != 1 {
				return fmt.Errorf("audio.channels must be 1 when input_mode=stt, got %d", c.Audio.Channels)
			}
			if c.Audio.BitWidth != 0 && c.Audio.BitWidth != 16 {
				return fmt.Errorf("audio.bit_width must be 16 when input_mode=stt, got %d", c.Audio.BitWidth)
			}
		}
	}

	if _, err := normalizeVADBackend(c.VADBackend); err != nil {
		return err
	}
	if c.VADSpeechThreshold != 0 && (c.VADSpeechThreshold < 0 || c.VADSpeechThreshold > 1) {
		return fmt.Errorf("vad_speech_threshold must be in [0,1] when set, got %v", c.VADSpeechThreshold)
	}

	if c.VoiceFollowupTimeoutMs < 0 {
		return fmt.Errorf("voice_followup_timeout_ms must be >= 0, got %d", c.VoiceFollowupTimeoutMs)
	}
	if c.VoiceFirstTurnTimeoutMs < 0 {
		return fmt.Errorf("voice_first_turn_timeout_ms must be >= 0, got %d", c.VoiceFirstTurnTimeoutMs)
	}
	if c.VoiceMaxTurns < 0 {
		return fmt.Errorf("voice_max_turns must be >= 0, got %d", c.VoiceMaxTurns)
	}
	if c.VoiceMaxResponseTokens < 0 {
		return fmt.Errorf("voice_max_response_tokens must be >= 0, got %d", c.VoiceMaxResponseTokens)
	}
	if err := c.QuickCapture.Validate(); err != nil {
		return err
	}
	if c.VoiceNotifications.MaxPending < 0 {
		return fmt.Errorf("voice_notifications.max_pending must be >= 0, got %d", c.VoiceNotifications.MaxPending)
	}
	if c.VoiceNotifications.RetentionDays < 0 {
		return fmt.Errorf("voice_notifications.retention_days must be >= 0, got %d", c.VoiceNotifications.RetentionDays)
	}
	if c.VoiceNotifications.ResponseTail.MaxItems < 0 || c.VoiceNotifications.ResponseTail.MaxItems > 1 {
		return fmt.Errorf("voice_notifications.response_tail.max_items must be 0 or 1, got %d", c.VoiceNotifications.ResponseTail.MaxItems)
	}
	if c.VoiceNotifications.ResponseTail.MaxTextChars < 0 {
		return fmt.Errorf("voice_notifications.response_tail.max_text_chars must be >= 0, got %d", c.VoiceNotifications.ResponseTail.MaxTextChars)
	}
	if c.VoiceNotifications.Expiration.DefaultTTLSeconds < 0 {
		return fmt.Errorf("voice_notifications.expiration.default_ttl_seconds must be >= 0, got %d", c.VoiceNotifications.Expiration.DefaultTTLSeconds)
	}
	for code, seconds := range c.VoiceNotifications.Expiration.CodeTTLSeconds {
		if strings.TrimSpace(code) == "" {
			return errors.New("voice_notifications.expiration.code_ttl_seconds contains an empty code")
		}
		if seconds < 0 {
			return fmt.Errorf("voice_notifications.expiration.code_ttl_seconds.%s must be >= 0, got %d", code, seconds)
		}
	}
	if c.Log.LLMHTTPRetentionDays < 0 {
		return fmt.Errorf("log.llm_http_retention_days must be >= 0, got %d", c.Log.LLMHTTPRetentionDays)
	}
	switch c.Log.LevelOrDefault() {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level must be debug, info, warn, or error, got %q", c.Log.Level)
	}
	if err := c.Storage.MonitorConfig().Validate(); err != nil {
		return err
	}
	if c.ScreenshotKeepN < 0 {
		return fmt.Errorf("screenshot_keep_n must be >= 0, got %d", c.ScreenshotKeepN)
	}
	if c.ScreenshotPruneInterval < 0 {
		return fmt.Errorf("screenshot_prune_interval must be >= 0, got %d", c.ScreenshotPruneInterval)
	}
	if c.ScreenStableTimeoutMs < 0 {
		return fmt.Errorf("screen_stable_timeout_ms must be >= 0, got %d", c.ScreenStableTimeoutMs)
	}
	if c.ScreenStableMs < 0 {
		return fmt.Errorf("screen_stable_ms must be >= 0, got %d", c.ScreenStableMs)
	}
	if c.ScreenStableDiffThreshold < 0 {
		return fmt.Errorf("screen_stable_diff_threshold must be >= 0, got %g", c.ScreenStableDiffThreshold)
	}

	if c.MaxIterations < -1 {
		return fmt.Errorf("max_iterations must be >= -1 (-1 means unlimited), got %d", c.MaxIterations)
	}
	if err := c.TerminationPolicy.Validate(); err != nil {
		return err
	}

	if _, ok := mnk.NormalizeKeyboardLayout(c.HID.KeyboardLayout); !ok {
		return fmt.Errorf("invalid hid.keyboard_layout: %s (expected %s)", c.HID.KeyboardLayout, mnk.KeyboardLayoutValuesText())
	}
	switch strings.ToLower(strings.TrimSpace(c.HID.PointerMode)) {
	case "", "absolute", "touchscreen":
	default:
		return fmt.Errorf("invalid hid.pointer_mode: %s (expected absolute or touchscreen)", c.HID.PointerMode)
	}
	switch strings.ToLower(strings.TrimSpace(c.HID.InputBackend)) {
	case "", "hid", "adb":
	default:
		return fmt.Errorf("invalid hid.input_backend: %s (expected hid or adb)", c.HID.InputBackend)
	}

	if err := c.Telemetry.Validate(); err != nil {
		return err
	}
	if err := c.LiveActivity.Validate(); err != nil {
		return err
	}
	if err := c.OTA.Validate(); err != nil {
		return err
	}

	return nil
}

// modelProviderSupportsResponses checks the resolved provider transport rather
// than the configured provider-record name. Named custom OpenAI-compatible
// endpoints therefore remain valid, while native Anthropic and Ollama clients
// are rejected before a configuration is persisted or used at startup.
func (c Config) modelProviderSupportsResponses() bool {
	providerType := c.modelProviderType()
	definition, ok := lookupModelProviderDefinition(providerType)
	return ok && definition.supportsResponses
}

func (c Config) modelProviderSupportsResponsesStateful() bool {
	providerType := c.modelProviderType()
	definition, ok := lookupModelProviderDefinition(providerType)
	return ok && definition.supportsResponsesStateful
}

func (c Config) modelProviderSupportsInteractions() bool {
	providerType := c.modelProviderType()
	definition, ok := lookupModelProviderDefinition(providerType)
	return ok && definition.supportsInteractions
}

func (c Config) modelProviderSupportsInteractionsStateful() bool {
	providerType := c.modelProviderType()
	definition, ok := lookupModelProviderDefinition(providerType)
	return ok && definition.supportsInteractionsStateful
}

func (c Config) modelProviderSupportsChatCompletions() bool {
	providerType := c.modelProviderType()
	definition, ok := lookupModelProviderDefinition(providerType)
	if !ok {
		return true
	}
	return !definition.chatCompletionsUnsupported
}

func (c Config) modelProviderType() string {
	providerType := strings.TrimSpace(c.Model.Provider)
	baseURL := c.Model.BaseURL
	if provider, ok := c.ModelProviders[providerType]; ok {
		providerType = provider.Type
		baseURL = provider.BaseURL
	}
	return effectiveModelProviderType(providerType, baseURL)
}

// effectiveModelProviderType recognizes well-known endpoints that were saved
// as generic OpenAI-compatible transports by older config pages. This keeps
// provider capability checks and runtime request behavior aligned: OpenRouter
// implements only stateless /responses, while Volcengine Ark supports stored
// response chaining with a different set of request fields.
func effectiveModelProviderType(providerType, baseURL string) string {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	if providerType != "openai" {
		return providerType
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return providerType
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai") {
		return "openrouter"
	}
	if host == "ark.cn-beijing.volces.com" || strings.HasSuffix(host, ".ark.cn-beijing.volces.com") {
		return "volcengine"
	}
	return providerType
}

// isKnownProviderType reports whether the value names a built-in model
// provider type (as opposed to a [model_providers] section name).
func isKnownProviderType(providerType string) bool {
	_, ok := lookupModelProviderDefinition(providerType)
	return ok
}

// validateModelProvider validates a single model provider configuration.
func validateModelProvider(name string, p ModelProvider) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("provider name cannot be empty")
	}

	providerType := strings.TrimSpace(p.Type)
	if providerType == "" {
		return fmt.Errorf("model_providers.%s: provider type is required", name)
	}

	if !isKnownProviderType(providerType) {
		return fmt.Errorf("model_providers.%s: unsupported provider type %q", name, providerType)
	}

	return nil
}

// validateModelProviderRef checks that a model's provider field resolves: it
// must either name a [model_providers] section or be a built-in provider type. A
// typo, or a reference left behind after the section was deleted, otherwise
// passes validation and only fails later when the model client is built.
// section is the config section being validated, e.g. "model".
func validateModelProviderRef(section string, providers map[string]ModelProvider, provider string) error {
	ref := strings.TrimSpace(provider)
	if ref == "" {
		return nil
	}
	if _, exists := providers[ref]; exists {
		return nil
	}
	if isKnownProviderType(ref) {
		return nil
	}
	if len(providers) == 0 {
		return fmt.Errorf("%s.provider: unknown provider %q", section, ref)
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("%s.provider: unknown provider %q (not a provider type, and no [model_providers.%s] section; configured: %s)",
		section, ref, ref, strings.Join(names, ", "))
}

func (c Config) LocaleOrDefault() string {
	locale := strings.TrimSpace(c.Locale)
	if locale == "" {
		return defaultLocale
	}
	return locale
}

func (t TelemetryConfig) Validate() error {
	if !t.EnabledOrDefault() {
		return nil
	}
	if strings.TrimSpace(t.BaseURL) == "" {
		return errors.New("telemetry.base_url is required when telemetry.enabled=true")
	}
	if strings.TrimSpace(t.PublicKey) == "" {
		return errors.New("telemetry.public_key is required when telemetry.enabled=true")
	}
	if strings.TrimSpace(t.SecretKey) == "" {
		return errors.New("telemetry.secret_key is required when telemetry.enabled=true")
	}
	switch t.ProviderOrDefault() {
	case "langfuse":
	default:
		return fmt.Errorf("invalid telemetry.provider: %s (expected langfuse)", t.Provider)
	}
	if t.UploadTimeoutSec < 0 {
		return fmt.Errorf("telemetry.upload_timeout_sec must be >= 0, got %d", t.UploadTimeoutSec)
	}
	if t.MaxRetry < 0 {
		return fmt.Errorf("telemetry.max_retry must be >= 0, got %d", t.MaxRetry)
	}
	return nil
}

func (t TelemetryConfig) EnabledOrDefault() bool {
	if t.Enabled != nil {
		return *t.Enabled
	}
	return false
}

func (l LiveActivityConfig) Validate() error {
	return nil
}

func (l LiveActivityConfig) EnabledOrDefault() bool {
	if l.Enabled != nil {
		return *l.Enabled
	}
	return true
}

func (t TelemetryConfig) ProviderOrDefault() string {
	if strings.TrimSpace(t.Provider) != "" {
		return strings.ToLower(strings.TrimSpace(t.Provider))
	}
	return "langfuse"
}

func (t TelemetryConfig) UploadScreenshotsOrDefault() bool {
	if t.UploadScreenshots != nil {
		return *t.UploadScreenshots
	}
	return true
}

func (t TelemetryConfig) UploadTimeoutOrDefault() time.Duration {
	if t.UploadTimeoutSec > 0 {
		return time.Duration(t.UploadTimeoutSec) * time.Second
	}
	return 30 * time.Second
}

func (t TelemetryConfig) MaxRetryOrDefault() int {
	if t.MaxRetry > 0 {
		return t.MaxRetry
	}
	return 2
}

func (t TelemetryConfig) EnvironmentOrDefault() string {
	if strings.TrimSpace(t.Environment) != "" {
		return strings.TrimSpace(t.Environment)
	}
	return "default"
}

// InputModeOrDefault returns the effective input mode. An unset mode is
// inferred from the configured providers rather than defaulting to a fixed
// value: speech providers are opt-in (see
// applyRuntimeOptionalProviderDefaults), so a device that never configured
// voice has no mode to fall back to. Returns "stt" when the classic pair is
// configured, "realtime" when a realtime credential is present, and "" when
// voice is unavailable and only the Web UI can reach the agent.
func (c Config) InputModeOrDefault() string {
	mode := strings.TrimSpace(c.InputMode)
	if mode != "" {
		return strings.ToLower(mode)
	}
	if strings.TrimSpace(c.STT.Provider) != "" && strings.TrimSpace(c.TTS.Provider) != "" {
		return "stt"
	}
	if c.VoiceModel.Enabled() {
		return "realtime"
	}
	return ""
}

func (c Config) VADBackendOrDefault() string {
	backend, err := normalizeVADBackend(c.VADBackend)
	if err != nil {
		return defaultVADBackend
	}
	return backend
}

func (c Config) VoiceFollowupEnabledOrDefault() bool {
	if c.VoiceFollowupEnabled != nil {
		return *c.VoiceFollowupEnabled
	}
	return false
}

func (c Config) VoiceFollowupTimeoutOrDefault() time.Duration {
	if c.VoiceFollowupTimeoutMs > 0 {
		return time.Duration(c.VoiceFollowupTimeoutMs) * time.Millisecond
	}
	return time.Duration(defaultVoiceFollowupTimeoutMs) * time.Millisecond
}

func (c Config) VoiceFirstTurnTimeoutOrDefault() time.Duration {
	if c.VoiceFirstTurnTimeoutMs > 0 {
		return time.Duration(c.VoiceFirstTurnTimeoutMs) * time.Millisecond
	}
	return time.Duration(defaultVoiceFirstTurnTimeoutMs) * time.Millisecond
}

func (c Config) VoiceInterruptOnWakeupOrDefault() bool {
	if c.VoiceInterruptOnWakeup != nil {
		return *c.VoiceInterruptOnWakeup
	}
	return true
}

func (c Config) TapWakeupEnabledOrDefault() bool {
	if c.TapWakeupEnabled != nil {
		return *c.TapWakeupEnabled
	}
	return true
}

func (c Config) MCPEnabledOrDefault() bool {
	if c.MCPEnabled != nil {
		return *c.MCPEnabled
	}
	return true
}

func (c Config) MCPShellEnabledOrDefault() bool {
	if c.MCPShellEnabled != nil {
		return *c.MCPShellEnabled
	}
	return false
}

func (c Config) VoiceStreamingTTSEnabledOrDefault() bool {
	if c.VoiceStreamingTTSEnabled != nil {
		return *c.VoiceStreamingTTSEnabled
	}
	return true
}

func (c Config) VoiceToolCallSpeechOrDefault() bool {
	if c.VoiceToolCallSpeech != nil {
		return *c.VoiceToolCallSpeech
	}
	return true
}

func (c Config) VoiceProgressSpeechEnabledOrDefault() bool {
	if c.VoiceProgressSpeechEnabled != nil {
		return *c.VoiceProgressSpeechEnabled
	}
	return true
}

func (c Config) VoiceMaxResponseTokensOrDefault() int {
	if c.VoiceMaxResponseTokens > 0 {
		return c.VoiceMaxResponseTokens
	}
	return defaultVoiceMaxResponseTokens
}

// ContextCompactionThresholdOrDefault returns the fraction of the usable input
// budget at which conversation compaction runs, falling back to the default
// when unset.
func (c Config) ContextCompactionThresholdOrDefault() float64 {
	if c.ContextCompactionThreshold > 0 {
		return c.ContextCompactionThreshold
	}
	return defaultContextCompactionThreshold
}

// ContextPruneThresholdOrDefault returns the fraction of the usable input
// budget at which deterministic pruning runs. Deterministic pruning is the
// cheap pass that must get a chance to free tokens before the LLM summary, so
// the result never exceeds the compaction threshold even if the file configures
// a larger value.
func (c Config) ContextPruneThresholdOrDefault() float64 {
	threshold := c.ContextPruneThreshold
	if threshold <= 0 {
		threshold = defaultContextPruneThreshold
	}
	return min(threshold, c.ContextCompactionThresholdOrDefault())
}

func (c Config) ScreenshotPruningOrDefault() executor.ScreenshotPruningConfig {
	return executor.ScreenshotPruningConfig{
		KeepN:    c.ScreenshotKeepN,
		Interval: c.ScreenshotPruneInterval,
	}.WithDefaults()
}

func (c Config) ScreenStableDefaults() ScreenStableDefaults {
	return ScreenStableDefaults{
		TimeoutMs:     c.ScreenStableTimeoutMs,
		StableMs:      c.ScreenStableMs,
		DiffThreshold: c.ScreenStableDiffThreshold,
	}
}

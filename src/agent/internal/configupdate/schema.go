package configupdate

import (
	"encoding/json"
	"errors"
	"strings"

	"aiden-agent/internal/agent"
)

// Config is the single definition of the config_web <-> agent wire contract: keys
// are snake_case, agent-level settings live under "agent", and write-only
// credentials report only configured-state flags rather than echoing values.
//
// Keep this struct in lockstep with the config web API.
type Config struct {
	ModelProviders      map[string]ModelProvider      `json:"model_providers,omitempty"`
	TTSProviders        map[string]TTSProvider        `json:"tts_providers,omitempty"`
	STTProviders        map[string]STTProvider        `json:"stt_providers,omitempty"`
	VoiceModelProviders map[string]VoiceModelProvider `json:"voice_model_providers,omitempty"`
	Model               Model                         `json:"model"`
	TTS                 TTS                           `json:"tts"`
	STT                 STT                           `json:"stt"`
	Audio               Audio                         `json:"audio"`
	VoiceModel          VoiceModel                    `json:"voice_model"`
	AudioArchive        AudioArchive                  `json:"audio_archive"`
	FrameService        FrameService                  `json:"frame_service"`
	QuickCapture        QuickCapture                  `json:"quick_capture"`
	Storage             Storage                       `json:"storage"`
	VoiceNotifications  VoiceNotifications            `json:"voice_notifications"`
	Device              Device                        `json:"device"`
	Log                 Log                           `json:"log"`
	OTA                 OTA                           `json:"ota"`
	HID                 HID                           `json:"hid"`
	Search              Search                        `json:"search"`
	Telemetry           Telemetry                     `json:"telemetry"`
	LiveActivity        LiveActivity                  `json:"live_activity"`
	TerminationPolicy   agent.TerminationPolicyConfig `json:"termination_policy"`
	Agent               Agent                         `json:"agent"`
}

// UnmarshalJSON rejects the former top-level providers key so callers do not
// get a successful response for a payload whose provider records were ignored.
func (d *Config) UnmarshalJSON(data []byte) error {
	type canonicalDTO Config
	if err := json.Unmarshal(data, (*canonicalDTO)(d)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if _, exists := fields["providers"]; exists {
		return errors.New(`"providers" is unsupported; use "model_providers"`)
	}
	return nil
}

type Model struct {
	Provider                          string   `json:"provider"`
	APIKey                            string   `json:"api_key,omitempty"`
	Model                             string   `json:"model"`
	APIMode                           string   `json:"api_mode,omitempty"`
	ResponsesContextManagement        string   `json:"responses_context_management,omitempty"`
	ResponsesCompactThreshold         int      `json:"responses_compact_threshold,omitempty"`
	ResponsesContextEditTrigger       int      `json:"responses_context_edit_trigger,omitempty"`
	ResponsesContextEditKeep          int      `json:"responses_context_edit_keep,omitempty"`
	ResponsesContextEditClearThinking bool     `json:"responses_context_edit_clear_thinking,omitempty"`
	ResponsesTruncation               string   `json:"responses_truncation,omitempty"`
	ResponsesInclude                  []string `json:"responses_include,omitempty"`
	ReasoningEffort                   string   `json:"reasoning_effort"`
	ReasoningBudgetTokens             int      `json:"reasoning_budget_tokens"`
	Temperature                       *float64 `json:"temperature,omitempty"`
	MaxResponseTokens                 int      `json:"max_response_tokens"`
	LogRawHTTP                        bool     `json:"log_raw_http"`
	ContextWindow                     int      `json:"context_window"`
	ModelMaxOutputTokens              int      `json:"model_max_output_tokens"`
}

func (d Model) ProviderTestRequest() agent.ModelProviderTestRequest {
	return agent.ModelProviderTestRequest{
		Provider:                          d.Provider,
		APIKey:                            d.APIKey,
		Model:                             d.Model,
		APIMode:                           d.APIMode,
		ResponsesContextManagement:        d.ResponsesContextManagement,
		ResponsesCompactThreshold:         d.ResponsesCompactThreshold,
		ResponsesContextEditTrigger:       d.ResponsesContextEditTrigger,
		ResponsesContextEditKeep:          d.ResponsesContextEditKeep,
		ResponsesContextEditClearThinking: d.ResponsesContextEditClearThinking,
		ResponsesTruncation:               d.ResponsesTruncation,
		ResponsesInclude:                  append([]string(nil), d.ResponsesInclude...),
		Temperature:                       d.Temperature,
		ReasoningEffort:                   d.ReasoningEffort,
		ReasoningBudgetTokens:             d.ReasoningBudgetTokens,
	}
}

// ModelProvider mirrors a single [model_providers.<name>] section. Named providers hold
// the credentials; a model section references one by putting the provider name
// in its own "provider" field.
type ModelProvider struct {
	Type      string `json:"type"`
	APIKey    string `json:"api_key,omitempty"`
	HasAPIKey bool   `json:"has_api_key,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
}

func (d *ModelProvider) UnmarshalJSON(data []byte) error {
	type canonical ModelProvider
	var value canonical
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if hasJSONField(data, "provider") {
		return errors.New(`provider record field "provider" is unsupported; use "type"`)
	}
	*d = ModelProvider(value)
	return nil
}

// TTSProvider mirrors a single [tts_providers.<name>] section. [tts]
// references one by putting the record name in its own "provider" field. speed
// is absent on purpose: it is a listening preference that stays global on [tts]
// so switching voice never changes playback speed.
type TTSProvider struct {
	Type        string `json:"type"`
	APIKey      string `json:"api_key,omitempty"`
	HasAPIKey   bool   `json:"has_api_key,omitempty"`
	Model       string `json:"model,omitempty"`
	VoiceID     string `json:"voice_id,omitempty"`
	Emotion     string `json:"emotion,omitempty"`
	ReferenceID string `json:"reference_id,omitempty"`
}

func (d *TTSProvider) UnmarshalJSON(data []byte) error {
	type canonical TTSProvider
	var value canonical
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if hasJSONField(data, "provider") {
		return errors.New(`provider record field "provider" is unsupported; use "type"`)
	}
	*d = TTSProvider(value)
	return nil
}

// STTProvider mirrors a single [stt_providers.<name>] section. language stays
// on [stt]: it holds regardless of which provider transcribes.
type STTProvider struct {
	Type            string `json:"type"`
	APIKey          string `json:"api_key,omitempty"`
	HasAPIKey       bool   `json:"has_api_key,omitempty"`
	Model           string `json:"model,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	AppID           string `json:"app_id,omitempty"`
	SecretID        string `json:"secret_id,omitempty"`
	HasSecretID     bool   `json:"has_secret_id,omitempty"`
	SecretKey       string `json:"secret_key,omitempty"`
	HasSecretKey    bool   `json:"has_secret_key,omitempty"`
	Region          string `json:"region,omitempty"`
	EngineModelType string `json:"engine_model_type,omitempty"`
}

// VoiceModelProvider mirrors one [voice_model_providers.<name>] record. The
// selected record name stays in voice_model.provider.
type VoiceModelProvider struct {
	Type                   string   `json:"type"`
	UpstreamProvider       string   `json:"upstream_provider,omitempty"`
	AgentID                string   `json:"agent_id,omitempty"`
	APIKey                 string   `json:"api_key,omitempty"`
	HasAPIKey              bool     `json:"has_api_key,omitempty"`
	Model                  string   `json:"model,omitempty"`
	WorkspaceID            string   `json:"workspace_id,omitempty"`
	Region                 string   `json:"region,omitempty"`
	AuthMode               string   `json:"auth_mode,omitempty"`
	ProjectID              string   `json:"project_id,omitempty"`
	Location               string   `json:"location,omitempty"`
	Endpoint               string   `json:"endpoint,omitempty"`
	BaseURL                string   `json:"base_url,omitempty"`
	RealtimeProtocol       string   `json:"realtime_protocol,omitempty"`
	ThinkingLevel          string   `json:"thinking_level,omitempty"`
	Voice                  string   `json:"voice,omitempty"`
	TurnDetection          string   `json:"turn_detection,omitempty"`
	TurnDetectionThreshold *float64 `json:"turn_detection_threshold,omitempty"`
	TurnDetectionSilenceMs int      `json:"turn_detection_silence_ms,omitempty"`
}

func (d *VoiceModelProvider) UnmarshalJSON(data []byte) error {
	type canonical VoiceModelProvider
	var value canonical
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if hasJSONField(data, "provider") {
		return errors.New(`provider record field "provider" is unsupported; use "type"`)
	}
	*d = VoiceModelProvider(value)
	return nil
}

func (d *STTProvider) UnmarshalJSON(data []byte) error {
	type canonical STTProvider
	var value canonical
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if hasJSONField(data, "provider") {
		return errors.New(`provider record field "provider" is unsupported; use "type"`)
	}
	*d = STTProvider(value)
	return nil
}

func hasJSONField(data []byte, key string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return false
	}
	_, exists := fields[key]
	return exists
}

type TTS struct {
	Provider    string  `json:"provider"`
	APIKey      string  `json:"api_key,omitempty"`
	Model       string  `json:"model"`
	VoiceID     string  `json:"voice_id"`
	ReferenceID string  `json:"reference_id"`
	Emotion     string  `json:"emotion"`
	Speed       float64 `json:"speed"`
}

func (d TTS) PlaybackTestRequest(text string) agent.TTSPlaybackTestRequest {
	return agent.TTSPlaybackTestRequest{
		Provider:    d.Provider,
		APIKey:      d.APIKey,
		Model:       d.Model,
		VoiceID:     d.VoiceID,
		ReferenceID: d.ReferenceID,
		Emotion:     d.Emotion,
		Speed:       d.Speed,
		Text:        text,
	}
}

type STT struct {
	Provider        string `json:"provider"`
	Language        string `json:"language"`
	APIKey          string `json:"api_key,omitempty"`
	Model           string `json:"model"`
	BaseURL         string `json:"base_url"`
	AppID           string `json:"app_id"`
	SecretID        string `json:"secret_id,omitempty"`
	SecretKey       string `json:"secret_key,omitempty"`
	Region          string `json:"region"`
	EngineModelType string `json:"engine_model_type"`
}

// VoiceModel mirrors the realtime voice model settings. The credential itself
// is write-only; has_api_key reports whether one is already configured.
type VoiceModel struct {
	Provider               string   `json:"provider"`
	UpstreamProvider       string   `json:"upstream_provider"`
	AgentID                string   `json:"agent_id"`
	APIKey                 string   `json:"api_key,omitempty"`
	HasAPIKey              bool     `json:"has_api_key"`
	Model                  string   `json:"model"`
	WorkspaceID            string   `json:"workspace_id"`
	Region                 string   `json:"region"`
	AuthMode               string   `json:"auth_mode"`
	ProjectID              string   `json:"project_id"`
	Location               string   `json:"location"`
	Endpoint               string   `json:"endpoint"`
	BaseURL                string   `json:"base_url"`
	RealtimeProtocol       string   `json:"realtime_protocol,omitempty"`
	Voice                  string   `json:"voice"`
	EnableSpeechEmotion    *bool    `json:"enable_speech_emotion,omitempty"`
	InputAudioFormat       string   `json:"input_audio_format"`
	OutputAudioFormat      string   `json:"output_audio_format"`
	TurnDetection          string   `json:"turn_detection"`
	TurnDetectionThreshold *float64 `json:"turn_detection_threshold,omitempty"`
	TurnDetectionSilenceMs int      `json:"turn_detection_silence_ms"`
	UseBackendAgent        bool     `json:"use_backend_agent"`
}

func (d STT) TranscriptionTestRequest(wavData []byte) agent.STTTranscriptionTestRequest {
	return agent.STTTranscriptionTestRequest{
		Provider:        d.Provider,
		Language:        d.Language,
		APIKey:          d.APIKey,
		Model:           d.Model,
		BaseURL:         d.BaseURL,
		AppID:           d.AppID,
		SecretID:        d.SecretID,
		SecretKey:       d.SecretKey,
		Region:          d.Region,
		EngineModelType: d.EngineModelType,
		WAVData:         wavData,
	}
}

type Audio struct {
	Socket     string `json:"socket"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	BitWidth   int    `json:"bit_width"`
	Backend    string `json:"backend"`
}

func (d Audio) backend() string {
	return d.Backend
}

func (d *Audio) UnmarshalJSON(data []byte) error {
	type canonical Audio
	if hasJSONField(data, "playback_backend") {
		return errors.New(`"audio.playback_backend" is unsupported; use "audio.backend"`)
	}
	return json.Unmarshal(data, (*canonical)(d))
}

type AudioArchive struct {
	Enabled     bool   `json:"enabled"`
	MaxFiles    int    `json:"max_files"`
	MaxSizeMB   int    `json:"max_size_mb"`
	StoragePath string `json:"storage_path"`
}

type QuickCapture struct {
	Enabled         bool   `json:"enabled"`
	GPIOPin         int    `json:"gpio_pin"`
	ScreenMemoryTTL string `json:"screen_memory_ttl"`
}

type FrameService struct {
	KeepStreamOn bool `json:"keep_streamon"`
}

type Storage struct {
	MonitorEnabled       bool                `json:"monitor_enabled"`
	MountPoint           string              `json:"mount_point"`
	Device               string              `json:"device"`
	MinCardFreeMB        int                 `json:"min_card_free_mb"`
	MigrateStartFreePct  int                 `json:"migrate_start_free_pct"`
	MigrateStopFreePct   int                 `json:"migrate_stop_free_pct"`
	RootPath             string              `json:"root_path"`
	CheckIntervalSeconds int                 `json:"check_interval_seconds"`
	WarningThresholdMB   uint64              `json:"warning_threshold_mb"`
	CriticalThresholdMB  uint64              `json:"critical_threshold_mb"`
	EmergencyThresholdMB uint64              `json:"emergency_threshold_mb"`
	RecoveryHysteresisMB uint64              `json:"recovery_hysteresis_mb"`
	DegradedMode         StorageDegradedMode `json:"degraded_mode"`
	Cleanup              StorageCleanup      `json:"cleanup"`
}

type StorageDegradedMode struct {
	DisableLLMHTTPLog     bool `json:"disable_llm_http_log"`
	DisableAudioArchive   bool `json:"disable_audio_archive"`
	DisableSessionArchive bool `json:"disable_session_archive"`
	MaxAgentLogMB         int  `json:"max_agent_log_mb"`
}

type StorageCleanup struct {
	Enabled                          bool  `json:"enabled"`
	LLMHTTPLogRetentionDays          []int `json:"llm_http_log_retention_days"`
	AudioArchiveRetentionDays        []int `json:"audio_archive_retention_days"`
	SessionArchiveRetentionDays      []int `json:"session_archive_retention_days"`
	NotificationContextRetentionDays []int `json:"notification_context_retention_days"`
	CleanupRetryIntervalSeconds      int   `json:"cleanup_retry_interval_seconds"`
}

type Device struct {
	Backend    string `json:"backend,omitempty"`
	DeviceType string `json:"device_type"`
}

type VoiceNotifications struct {
	Enabled       *bool                         `json:"enabled"`
	MaxPending    int                           `json:"max_pending"`
	RetentionDays int                           `json:"retention_days"`
	ResponseTail  VoiceNotificationResponseTail `json:"response_tail"`
	Expiration    VoiceNotificationExpiration   `json:"expiration"`
}

type VoiceNotificationResponseTail struct {
	Enabled      *bool `json:"enabled"`
	MaxItems     int   `json:"max_items"`
	MaxTextChars int   `json:"max_text_chars"`
}

type VoiceNotificationExpiration struct {
	DefaultTTLSeconds int            `json:"default_ttl_seconds"`
	CodeTTLSeconds    map[string]int `json:"code_ttl_seconds"`
}

type Log struct {
	LLMHTTPRetentionDays int    `json:"llm_http_retention_days"`
	Level                string `json:"level"`
}

type OTA struct {
	GitHubProxyURL string `json:"github_proxy_url"`
}

type HID struct {
	KeyboardDevice        string `json:"keyboard_device"`
	KeyboardLayout        string `json:"keyboard_layout"`
	MouseDevice           string `json:"mouse_device"`
	AndroidKeyboardDevice string `json:"android_keyboard_device"`
	FrameSocket           string `json:"frame_socket"`
	PointerMode           string `json:"pointer_mode"`
	InputBackend          string `json:"input_backend"`
}

type Search struct {
	APIKey    string `json:"api_key,omitempty"`
	Provider  string `json:"provider"`
	HasAPIKey bool   `json:"has_api_key"`
}

type Telemetry struct {
	Enabled           bool     `json:"enabled"`
	Provider          string   `json:"provider"`
	BaseURL           string   `json:"base_url"`
	PublicKey         string   `json:"public_key"`
	SecretKey         string   `json:"secret_key"`
	UploadScreenshots bool     `json:"upload_screenshots"`
	UploadTimeoutSec  int      `json:"upload_timeout_sec"`
	MaxRetry          int      `json:"max_retry"`
	Tags              []string `json:"tags"`
	Environment       string   `json:"environment"`
}

type LiveActivity struct {
	Enabled *bool `json:"enabled"`
}

// Agent is the config_web wire view of the agent-level conversation settings.
// The built-in Agent instruction is runtime content and has no wire field: the
// legacy custom_instruction key is not accepted by config updates (the shared
// unknown-field validation rejects it) and is never emitted here.
type Agent struct {
	Locale                     string  `json:"locale"`
	Timezone                   string  `json:"timezone"`
	Prompt                     string  `json:"prompt"`
	ContextPruneThreshold      float64 `json:"context_prune_threshold,omitempty"`
	ContextCompactionThreshold float64 `json:"context_compaction_threshold,omitempty"`
	InputMode                  string  `json:"input_mode"`
	VADBackend                 string  `json:"vad_backend"`
	VADModelPath               string  `json:"vad_model_path"`
	VADHelperPath              string  `json:"vad_helper_path"`
	VADSpeechThreshold         float64 `json:"vad_speech_threshold"`
	SilenceMs                  int     `json:"silence_ms"`
	MinSpeechMs                int     `json:"min_speech_ms"`
	VoiceFollowupEnabled       bool    `json:"voice_followup_enabled"`
	VoiceFollowupTimeoutMs     int     `json:"voice_followup_timeout_ms"`
	VoiceFirstTurnTimeoutMs    int     `json:"voice_first_turn_timeout_ms"`
	VoiceMaxTurns              int     `json:"voice_max_turns"`
	VoiceInterruptOnWakeup     bool    `json:"voice_interrupt_on_wakeup"`
	GPIOWakeupEnabled          bool    `json:"gpio_wakeup_enabled"`
	VoiceStreamingTTSEnabled   bool    `json:"voice_streaming_tts_enabled"`
	VoiceToolCallSpeech        bool    `json:"voice_tool_call_speech"`
	VoiceProgressSpeechEnabled bool    `json:"voice_progress_speech_enabled"`
	VoiceMaxResponseTokens     int     `json:"voice_max_response_tokens"`
	MaxIterations              int     `json:"max_iterations"`
	ScreenshotKeepN            int     `json:"screenshot_keep_n"`
	ScreenshotPruneInterval    int     `json:"screenshot_prune_interval"`
	ScreenStableTimeoutMs      int     `json:"screen_stable_timeout_ms"`
	ScreenStableMs             int     `json:"screen_stable_ms"`
	ScreenStableDiffThreshold  float64 `json:"screen_stable_diff_threshold"`
}

// hasAPIKeyPlaceholder is substituted for a real key when the wire payload
// reports has_api_key=true. config_web never echoes the stored secret, so this
// lets Validate()'s "key is required" checks pass when a key is in fact saved,
// without the validator ever seeing the secret value.
const hasAPIKeyPlaceholder = "***"

// ToAgentConfig maps the wire DTO onto agent.Config so the canonical
// Config.Validate() can run against it.
func (d Config) ToAgentConfig() agent.Config {
	searchKey := ""
	if strings.TrimSpace(d.Search.APIKey) != "" {
		searchKey = d.Search.APIKey
	} else if d.Search.HasAPIKey {
		searchKey = hasAPIKeyPlaceholder
	}
	voiceModelKey := d.VoiceModel.APIKey
	if strings.TrimSpace(voiceModelKey) == "" && d.VoiceModel.HasAPIKey {
		voiceModelKey = hasAPIKeyPlaceholder
	}
	storage := agent.DefaultConfig().Storage
	storage.MonitorEnabled = d.Storage.MonitorEnabled
	storage.MountPoint = d.Storage.MountPoint
	storage.Device = d.Storage.Device
	storage.MinCardFreeMB = d.Storage.MinCardFreeMB
	storage.MigrateStartFreePct = d.Storage.MigrateStartFreePct
	storage.MigrateStopFreePct = d.Storage.MigrateStopFreePct
	storage.RootPath = d.Storage.RootPath
	storage.CheckIntervalSeconds = d.Storage.CheckIntervalSeconds
	storage.WarningThresholdMB = d.Storage.WarningThresholdMB
	storage.CriticalThresholdMB = d.Storage.CriticalThresholdMB
	storage.EmergencyThresholdMB = d.Storage.EmergencyThresholdMB
	storage.RecoveryHysteresisMB = d.Storage.RecoveryHysteresisMB
	storage.DegradedMode = agent.StorageDegradedModeConfig{
		DisableLLMHTTPLog:     d.Storage.DegradedMode.DisableLLMHTTPLog,
		DisableAudioArchive:   d.Storage.DegradedMode.DisableAudioArchive,
		DisableSessionArchive: d.Storage.DegradedMode.DisableSessionArchive,
		MaxAgentLogMB:         d.Storage.DegradedMode.MaxAgentLogMB,
	}
	storage.Cleanup = agent.StorageCleanupConfig{
		Enabled:                          d.Storage.Cleanup.Enabled,
		LLMHTTPLogRetentionDays:          d.Storage.Cleanup.LLMHTTPLogRetentionDays,
		AudioArchiveRetentionDays:        d.Storage.Cleanup.AudioArchiveRetentionDays,
		SessionArchiveRetentionDays:      d.Storage.Cleanup.SessionArchiveRetentionDays,
		NotificationContextRetentionDays: d.Storage.Cleanup.NotificationContextRetentionDays,
		CleanupRetryIntervalSeconds:      d.Storage.Cleanup.CleanupRetryIntervalSeconds,
	}
	return agent.Config{
		ModelProviders:      d.modelProvidersToAgentConfig(),
		TTSProviders:        d.ttsProvidersToAgentConfig(),
		STTProviders:        d.sttProvidersToAgentConfig(),
		VoiceModelProviders: d.voiceModelProvidersToAgentConfig(),
		Model: agent.ModelConfig{
			Provider:                          d.Model.Provider,
			APIKey:                            d.Model.APIKey,
			Model:                             d.Model.Model,
			APIMode:                           d.Model.APIMode,
			ResponsesContextManagement:        d.Model.ResponsesContextManagement,
			ResponsesCompactThreshold:         d.Model.ResponsesCompactThreshold,
			ResponsesContextEditTrigger:       d.Model.ResponsesContextEditTrigger,
			ResponsesContextEditKeep:          d.Model.ResponsesContextEditKeep,
			ResponsesContextEditClearThinking: d.Model.ResponsesContextEditClearThinking,
			ResponsesTruncation:               d.Model.ResponsesTruncation,
			ResponsesInclude:                  append([]string(nil), d.Model.ResponsesInclude...),
			ReasoningEffort:                   d.Model.ReasoningEffort,
			ReasoningBudgetTokens:             d.Model.ReasoningBudgetTokens,
			Temperature:                       d.Model.Temperature,
			MaxResponseTokens:                 d.Model.MaxResponseTokens,
			LogRawHTTP:                        d.Model.LogRawHTTP,
			ContextWindow:                     d.Model.ContextWindow,
			ModelMaxOutputTokens:              d.Model.ModelMaxOutputTokens,
		},
		TTS: agent.TTSConfig{
			Provider:    d.TTS.Provider,
			APIKey:      d.TTS.APIKey,
			Model:       d.TTS.Model,
			VoiceID:     d.TTS.VoiceID,
			ReferenceID: d.TTS.ReferenceID,
			Emotion:     d.TTS.Emotion,
			Speed:       d.TTS.Speed,
		},
		STT: agent.STTConfig{
			Provider:        d.STT.Provider,
			Language:        d.STT.Language,
			APIKey:          d.STT.APIKey,
			Model:           d.STT.Model,
			BaseURL:         d.STT.BaseURL,
			AppID:           d.STT.AppID,
			SecretID:        d.STT.SecretID,
			SecretKey:       d.STT.SecretKey,
			Region:          d.STT.Region,
			EngineModelType: d.STT.EngineModelType,
		},
		Audio: agent.AudioConfig{
			Socket:     d.Audio.Socket,
			SampleRate: d.Audio.SampleRate,
			Channels:   d.Audio.Channels,
			BitWidth:   d.Audio.BitWidth,
			Backend:    d.Audio.backend(),
		},
		VoiceModel: agent.VoiceModelConfig{
			Provider:               d.VoiceModel.Provider,
			UpstreamProvider:       d.VoiceModel.UpstreamProvider,
			AgentID:                d.VoiceModel.AgentID,
			APIKey:                 voiceModelKey,
			Model:                  d.VoiceModel.Model,
			WorkspaceID:            d.VoiceModel.WorkspaceID,
			Region:                 d.VoiceModel.Region,
			AuthMode:               d.VoiceModel.AuthMode,
			ProjectID:              d.VoiceModel.ProjectID,
			Location:               d.VoiceModel.Location,
			Endpoint:               d.VoiceModel.Endpoint,
			BaseURL:                d.VoiceModel.BaseURL,
			RealtimeProtocol:       d.VoiceModel.RealtimeProtocol,
			Voice:                  d.VoiceModel.Voice,
			EnableSpeechEmotion:    d.VoiceModel.EnableSpeechEmotion,
			InputAudioFormat:       d.VoiceModel.InputAudioFormat,
			OutputAudioFormat:      d.VoiceModel.OutputAudioFormat,
			TurnDetection:          d.VoiceModel.TurnDetection,
			TurnDetectionThreshold: d.VoiceModel.TurnDetectionThreshold,
			TurnDetectionSilenceMs: d.VoiceModel.TurnDetectionSilenceMs,
			UseBackendAgent:        d.VoiceModel.UseBackendAgent,
		},
		AudioArchive: agent.AudioArchiveConfig{
			Enabled:     d.AudioArchive.Enabled,
			MaxFiles:    d.AudioArchive.MaxFiles,
			MaxSizeMB:   d.AudioArchive.MaxSizeMB,
			StoragePath: d.AudioArchive.StoragePath,
		},
		FrameService: agent.FrameServiceConfig{
			KeepStreamOn: d.FrameService.KeepStreamOn,
		},
		QuickCapture: agent.QuickCaptureConfig{
			Enabled:         boolPtr(d.QuickCapture.Enabled),
			GPIOPin:         d.QuickCapture.GPIOPin,
			ScreenMemoryTTL: d.QuickCapture.ScreenMemoryTTL,
		},
		Storage: storage,
		VoiceNotifications: agent.VoiceNotificationsConfig{
			Enabled:       d.VoiceNotifications.Enabled,
			MaxPending:    d.VoiceNotifications.MaxPending,
			RetentionDays: d.VoiceNotifications.RetentionDays,
			ResponseTail: agent.VoiceNotificationResponseTailConfig{
				Enabled:      d.VoiceNotifications.ResponseTail.Enabled,
				MaxItems:     d.VoiceNotifications.ResponseTail.MaxItems,
				MaxTextChars: d.VoiceNotifications.ResponseTail.MaxTextChars,
			},
			Expiration: agent.VoiceNotificationExpirationConfig{
				DefaultTTLSeconds: d.VoiceNotifications.Expiration.DefaultTTLSeconds,
				CodeTTLSeconds:    d.VoiceNotifications.Expiration.CodeTTLSeconds,
			},
		},
		Log: agent.LogConfig{
			LLMHTTPRetentionDays: d.Log.LLMHTTPRetentionDays,
			Level:                d.Log.Level,
		},
		OTA: agent.OTAConfig{
			GitHubProxyURL: d.OTA.GitHubProxyURL,
		},
		HID: agent.HIDConfig{
			KeyboardDevice:        d.HID.KeyboardDevice,
			KeyboardLayout:        d.HID.KeyboardLayout,
			MouseDevice:           d.HID.MouseDevice,
			AndroidKeyboardDevice: d.HID.AndroidKeyboardDevice,
			FrameSocket:           d.HID.FrameSocket,
			PointerMode:           d.HID.PointerMode,
			InputBackend:          d.HID.InputBackend,
		},
		Device: agent.DeviceConfig{
			Backend:    d.Device.Backend,
			DeviceType: d.Device.DeviceType,
		},
		Search: agent.SearchConfig{
			Provider: d.Search.Provider,
			APIKey:   searchKey,
		},
		Telemetry: agent.TelemetryConfig{
			Enabled:           boolPtr(d.Telemetry.Enabled),
			Provider:          d.Telemetry.Provider,
			BaseURL:           d.Telemetry.BaseURL,
			PublicKey:         d.Telemetry.PublicKey,
			SecretKey:         d.Telemetry.SecretKey,
			UploadScreenshots: boolPtr(d.Telemetry.UploadScreenshots),
			UploadTimeoutSec:  d.Telemetry.UploadTimeoutSec,
			MaxRetry:          d.Telemetry.MaxRetry,
			Tags:              d.Telemetry.Tags,
			Environment:       d.Telemetry.Environment,
		},
		LiveActivity: agent.LiveActivityConfig{
			Enabled: d.LiveActivity.Enabled,
		},
		TerminationPolicy:          d.TerminationPolicy,
		Locale:                     d.Agent.Locale,
		Timezone:                   d.Agent.Timezone,
		Prompt:                     d.Agent.Prompt,
		ContextPruneThreshold:      d.Agent.ContextPruneThreshold,
		ContextCompactionThreshold: d.Agent.ContextCompactionThreshold,
		InputMode:                  d.Agent.InputMode,
		VADBackend:                 d.Agent.VADBackend,
		VADModelPath:               d.Agent.VADModelPath,
		VADHelperPath:              d.Agent.VADHelperPath,
		VADSpeechThreshold:         d.Agent.VADSpeechThreshold,
		SilenceMs:                  d.Agent.SilenceMs,
		MinSpeechMs:                d.Agent.MinSpeechMs,
		VoiceFollowupEnabled:       boolPtr(d.Agent.VoiceFollowupEnabled),
		VoiceFollowupTimeoutMs:     d.Agent.VoiceFollowupTimeoutMs,
		VoiceFirstTurnTimeoutMs:    d.Agent.VoiceFirstTurnTimeoutMs,
		VoiceMaxTurns:              d.Agent.VoiceMaxTurns,
		VoiceInterruptOnWakeup:     boolPtr(d.Agent.VoiceInterruptOnWakeup),
		GPIOWakeupEnabled:          boolPtr(d.Agent.GPIOWakeupEnabled),
		VoiceStreamingTTSEnabled:   boolPtr(d.Agent.VoiceStreamingTTSEnabled),
		VoiceToolCallSpeech:        boolPtr(d.Agent.VoiceToolCallSpeech),
		VoiceProgressSpeechEnabled: boolPtr(d.Agent.VoiceProgressSpeechEnabled),
		VoiceMaxResponseTokens:     d.Agent.VoiceMaxResponseTokens,
		MaxIterations:              d.Agent.MaxIterations,
		ScreenshotKeepN:            d.Agent.ScreenshotKeepN,
		ScreenshotPruneInterval:    d.Agent.ScreenshotPruneInterval,
		ScreenStableTimeoutMs:      d.Agent.ScreenStableTimeoutMs,
		ScreenStableMs:             d.Agent.ScreenStableMs,
		ScreenStableDiffThreshold:  d.Agent.ScreenStableDiffThreshold,
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func modelProvidersFromConfig(providers map[string]agent.ModelProvider) map[string]ModelProvider {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string]ModelProvider, len(providers))
	for name, provider := range providers {
		result[name] = ModelProvider{
			Type:      provider.Type,
			HasAPIKey: strings.TrimSpace(provider.APIKey) != "",
			BaseURL:   provider.BaseURL,
		}
	}
	return result
}

func (d Config) modelProvidersToAgentConfig() map[string]agent.ModelProvider {
	if len(d.ModelProviders) == 0 {
		return nil
	}
	result := make(map[string]agent.ModelProvider, len(d.ModelProviders))
	for name, provider := range d.ModelProviders {
		result[name] = agent.ModelProvider{
			Type:    provider.Type,
			APIKey:  provider.APIKey,
			BaseURL: provider.BaseURL,
		}
		if result[name].APIKey == "" && provider.HasAPIKey {
			result[name] = agent.ModelProvider{Type: provider.Type, APIKey: hasAPIKeyPlaceholder, BaseURL: provider.BaseURL}
		}
	}
	return result
}

func ttsProvidersFromConfig(providers map[string]agent.TTSProvider) map[string]TTSProvider {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string]TTSProvider, len(providers))
	for name, provider := range providers {
		result[name] = TTSProvider{
			Type:        provider.Type,
			HasAPIKey:   strings.TrimSpace(provider.APIKey) != "",
			Model:       provider.Model,
			VoiceID:     provider.VoiceID,
			Emotion:     provider.Emotion,
			ReferenceID: provider.ReferenceID,
		}
	}
	return result
}

func (d Config) ttsProvidersToAgentConfig() map[string]agent.TTSProvider {
	if len(d.TTSProviders) == 0 {
		return nil
	}
	result := make(map[string]agent.TTSProvider, len(d.TTSProviders))
	for name, provider := range d.TTSProviders {
		mapped := agent.TTSProvider{
			Type:        provider.Type,
			APIKey:      provider.APIKey,
			Model:       provider.Model,
			VoiceID:     provider.VoiceID,
			Emotion:     provider.Emotion,
			ReferenceID: provider.ReferenceID,
		}
		if mapped.APIKey == "" && provider.HasAPIKey {
			mapped.APIKey = hasAPIKeyPlaceholder
		}
		result[name] = mapped
	}
	return result
}

func sttProvidersFromConfig(providers map[string]agent.STTProvider) map[string]STTProvider {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string]STTProvider, len(providers))
	for name, provider := range providers {
		result[name] = STTProvider{
			Type:            provider.Type,
			HasAPIKey:       strings.TrimSpace(provider.APIKey) != "",
			Model:           provider.Model,
			BaseURL:         provider.BaseURL,
			AppID:           provider.AppID,
			HasSecretID:     strings.TrimSpace(provider.SecretID) != "",
			HasSecretKey:    strings.TrimSpace(provider.SecretKey) != "",
			Region:          provider.Region,
			EngineModelType: provider.EngineModelType,
		}
	}
	return result
}

func (d Config) sttProvidersToAgentConfig() map[string]agent.STTProvider {
	if len(d.STTProviders) == 0 {
		return nil
	}
	result := make(map[string]agent.STTProvider, len(d.STTProviders))
	for name, provider := range d.STTProviders {
		mapped := agent.STTProvider{
			Type:            provider.Type,
			APIKey:          provider.APIKey,
			Model:           provider.Model,
			BaseURL:         provider.BaseURL,
			AppID:           provider.AppID,
			SecretID:        provider.SecretID,
			SecretKey:       provider.SecretKey,
			Region:          provider.Region,
			EngineModelType: provider.EngineModelType,
		}
		if mapped.APIKey == "" && provider.HasAPIKey {
			mapped.APIKey = hasAPIKeyPlaceholder
		}
		if mapped.SecretID == "" && provider.HasSecretID {
			mapped.SecretID = hasAPIKeyPlaceholder
		}
		if mapped.SecretKey == "" && provider.HasSecretKey {
			mapped.SecretKey = hasAPIKeyPlaceholder
		}
		result[name] = mapped
	}
	return result
}

func voiceModelProvidersFromConfig(providers map[string]agent.VoiceModelProvider) map[string]VoiceModelProvider {
	if len(providers) == 0 {
		return nil
	}
	result := make(map[string]VoiceModelProvider, len(providers))
	for name, provider := range providers {
		result[name] = VoiceModelProvider{
			Type:                   provider.Type,
			UpstreamProvider:       provider.UpstreamProvider,
			AgentID:                provider.AgentID,
			HasAPIKey:              strings.TrimSpace(provider.APIKey) != "",
			Model:                  provider.Model,
			WorkspaceID:            provider.WorkspaceID,
			Region:                 provider.Region,
			AuthMode:               provider.AuthMode,
			ProjectID:              provider.ProjectID,
			Location:               provider.Location,
			Endpoint:               provider.Endpoint,
			BaseURL:                provider.BaseURL,
			RealtimeProtocol:       provider.RealtimeProtocol,
			ThinkingLevel:          provider.ThinkingLevel,
			Voice:                  provider.Voice,
			TurnDetection:          provider.TurnDetection,
			TurnDetectionThreshold: provider.TurnDetectionThreshold,
			TurnDetectionSilenceMs: provider.TurnDetectionSilenceMs,
		}
	}
	return result
}

func (d Config) voiceModelProvidersToAgentConfig() map[string]agent.VoiceModelProvider {
	if len(d.VoiceModelProviders) == 0 {
		return nil
	}
	result := make(map[string]agent.VoiceModelProvider, len(d.VoiceModelProviders))
	for name, provider := range d.VoiceModelProviders {
		mapped := agent.VoiceModelProvider{
			Type:                   provider.Type,
			UpstreamProvider:       provider.UpstreamProvider,
			AgentID:                provider.AgentID,
			APIKey:                 provider.APIKey,
			Model:                  provider.Model,
			WorkspaceID:            provider.WorkspaceID,
			Region:                 provider.Region,
			AuthMode:               provider.AuthMode,
			ProjectID:              provider.ProjectID,
			Location:               provider.Location,
			Endpoint:               provider.Endpoint,
			BaseURL:                provider.BaseURL,
			RealtimeProtocol:       provider.RealtimeProtocol,
			ThinkingLevel:          provider.ThinkingLevel,
			Voice:                  provider.Voice,
			TurnDetection:          provider.TurnDetection,
			TurnDetectionThreshold: provider.TurnDetectionThreshold,
			TurnDetectionSilenceMs: provider.TurnDetectionSilenceMs,
		}
		if mapped.APIKey == "" && provider.HasAPIKey {
			mapped.APIKey = hasAPIKeyPlaceholder
		}
		result[name] = mapped
	}
	return result
}

func FromAgentConfig(cfg agent.Config) Config {
	audioArchive := cfg.AudioArchive
	migrateStartFreePct, migrateStopFreePct := cfg.Storage.MigrateWatermarksOrDefault()

	return Config{
		ModelProviders:      modelProvidersFromConfig(cfg.ModelProviders),
		TTSProviders:        ttsProvidersFromConfig(cfg.TTSProviders),
		STTProviders:        sttProvidersFromConfig(cfg.STTProviders),
		VoiceModelProviders: voiceModelProvidersFromConfig(cfg.VoiceModelProviders),
		Model: Model{
			Provider:                          cfg.Model.Provider,
			Model:                             cfg.Model.Model,
			APIMode:                           cfg.Model.APIMode,
			ResponsesContextManagement:        cfg.Model.ResponsesContextManagement,
			ResponsesCompactThreshold:         cfg.Model.ResponsesCompactThreshold,
			ResponsesContextEditTrigger:       cfg.Model.ResponsesContextEditTrigger,
			ResponsesContextEditKeep:          cfg.Model.ResponsesContextEditKeep,
			ResponsesContextEditClearThinking: cfg.Model.ResponsesContextEditClearThinking,
			ResponsesTruncation:               cfg.Model.ResponsesTruncation,
			ResponsesInclude:                  append([]string(nil), cfg.Model.ResponsesInclude...),
			ReasoningEffort:                   cfg.Model.ReasoningEffort,
			ReasoningBudgetTokens:             cfg.Model.ReasoningBudgetTokens,
			Temperature:                       cfg.Model.Temperature,
			MaxResponseTokens:                 cfg.Model.MaxResponseTokens,
			LogRawHTTP:                        cfg.Model.LogRawHTTP,
			ContextWindow:                     cfg.Model.ContextWindow,
			ModelMaxOutputTokens:              cfg.Model.ModelMaxOutputTokens,
		},
		TTS: TTS{
			Provider:    cfg.TTS.Provider,
			Model:       cfg.TTS.Model,
			VoiceID:     cfg.TTS.VoiceID,
			ReferenceID: cfg.TTS.ReferenceID,
			Emotion:     cfg.TTS.Emotion,
			Speed:       cfg.TTS.Speed,
		},
		STT: STT{
			Provider:        cfg.STT.Provider,
			Language:        cfg.STT.Language,
			Model:           cfg.STT.Model,
			BaseURL:         cfg.STT.BaseURL,
			AppID:           cfg.STT.AppID,
			Region:          cfg.STT.Region,
			EngineModelType: cfg.STT.EngineModelType,
		},
		Audio: Audio{
			Socket:     cfg.Audio.SocketOrDefault(),
			SampleRate: cfg.Audio.SampleRateOrDefault(),
			Channels:   cfg.Audio.ChannelsOrDefault(),
			BitWidth:   cfg.Audio.BitWidthOrDefault(),
			Backend:    cfg.Audio.BackendOrDefault(),
		},
		VoiceModel: VoiceModel{
			Provider:               cfg.VoiceModel.Provider,
			UpstreamProvider:       cfg.VoiceModel.UpstreamProvider,
			AgentID:                cfg.VoiceModel.AgentID,
			HasAPIKey:              strings.TrimSpace(cfg.VoiceModel.APIKey) != "",
			Model:                  cfg.VoiceModel.Model,
			WorkspaceID:            cfg.VoiceModel.WorkspaceID,
			Region:                 cfg.VoiceModel.Region,
			AuthMode:               cfg.VoiceModel.AuthMode,
			ProjectID:              cfg.VoiceModel.ProjectID,
			Location:               cfg.VoiceModel.Location,
			Endpoint:               cfg.VoiceModel.Endpoint,
			BaseURL:                cfg.VoiceModel.BaseURL,
			RealtimeProtocol:       cfg.VoiceModel.RealtimeProtocol,
			Voice:                  cfg.VoiceModel.Voice,
			EnableSpeechEmotion:    cfg.VoiceModel.EnableSpeechEmotion,
			InputAudioFormat:       cfg.VoiceModel.InputAudioFormat,
			OutputAudioFormat:      cfg.VoiceModel.OutputAudioFormat,
			TurnDetection:          cfg.VoiceModel.TurnDetection,
			TurnDetectionThreshold: cfg.VoiceModel.TurnDetectionThreshold,
			TurnDetectionSilenceMs: cfg.VoiceModel.TurnDetectionSilenceMs,
			UseBackendAgent:        cfg.VoiceModel.UseBackendAgent,
		},
		AudioArchive: AudioArchive{
			Enabled:     audioArchive.Enabled,
			MaxFiles:    audioArchive.MaxFilesOrDefault(),
			MaxSizeMB:   audioArchive.MaxSizeMBOrDefault(),
			StoragePath: audioArchive.StoragePathOrDefault(),
		},
		FrameService: FrameService{
			KeepStreamOn: cfg.FrameService.KeepStreamOn,
		},
		QuickCapture: QuickCapture{
			Enabled:         cfg.QuickCapture.EnabledOrDefault(),
			GPIOPin:         cfg.QuickCapture.GPIOPin,
			ScreenMemoryTTL: cfg.QuickCapture.ScreenMemoryTTLOrDefault(),
		},
		Storage: Storage{
			MonitorEnabled:       cfg.Storage.MonitorEnabled,
			MountPoint:           cfg.Storage.MountPointOrDefault(),
			Device:               cfg.Storage.DeviceOrDefault(),
			MinCardFreeMB:        cfg.Storage.MinCardFreeMBOrDefault(),
			MigrateStartFreePct:  migrateStartFreePct,
			MigrateStopFreePct:   migrateStopFreePct,
			RootPath:             cfg.Storage.RootPath,
			CheckIntervalSeconds: cfg.Storage.CheckIntervalSeconds,
			WarningThresholdMB:   cfg.Storage.WarningThresholdMB,
			CriticalThresholdMB:  cfg.Storage.CriticalThresholdMB,
			EmergencyThresholdMB: cfg.Storage.EmergencyThresholdMB,
			RecoveryHysteresisMB: cfg.Storage.RecoveryHysteresisMB,
			DegradedMode: StorageDegradedMode{
				DisableLLMHTTPLog:     cfg.Storage.DegradedMode.DisableLLMHTTPLog,
				DisableAudioArchive:   cfg.Storage.DegradedMode.DisableAudioArchive,
				DisableSessionArchive: cfg.Storage.DegradedMode.DisableSessionArchive,
				MaxAgentLogMB:         cfg.Storage.DegradedMode.MaxAgentLogMB,
			},
			Cleanup: StorageCleanup{
				Enabled:                          cfg.Storage.Cleanup.Enabled,
				LLMHTTPLogRetentionDays:          cfg.Storage.Cleanup.LLMHTTPLogRetentionDays,
				AudioArchiveRetentionDays:        cfg.Storage.Cleanup.AudioArchiveRetentionDays,
				SessionArchiveRetentionDays:      cfg.Storage.Cleanup.SessionArchiveRetentionDays,
				NotificationContextRetentionDays: cfg.Storage.Cleanup.NotificationContextRetentionDays,
				CleanupRetryIntervalSeconds:      cfg.Storage.Cleanup.CleanupRetryIntervalSeconds,
			},
		},
		Device: Device{
			Backend:    cfg.Device.BackendOrDefault(),
			DeviceType: cfg.DeviceTypeOrDefault(),
		},
		VoiceNotifications: VoiceNotifications{
			Enabled:       cfg.VoiceNotifications.Enabled,
			MaxPending:    cfg.VoiceNotifications.MaxPendingOrDefault(),
			RetentionDays: cfg.VoiceNotifications.RetentionDaysOrDefault(),
			ResponseTail: VoiceNotificationResponseTail{
				Enabled:      cfg.VoiceNotifications.ResponseTail.Enabled,
				MaxItems:     cfg.VoiceNotifications.ResponseTail.MaxItems,
				MaxTextChars: cfg.VoiceNotifications.ResponseTail.MaxTextCharsOrDefault(),
			},
			Expiration: VoiceNotificationExpiration{
				DefaultTTLSeconds: cfg.VoiceNotifications.Expiration.DefaultTTLSeconds,
				CodeTTLSeconds:    cfg.VoiceNotifications.Expiration.CodeTTLSeconds,
			},
		},
		Log: Log{
			LLMHTTPRetentionDays: cfg.Log.LLMHTTPRetentionDaysOrDefault(),
			Level:                cfg.Log.LevelOrDefault(),
		},
		OTA: OTA{
			GitHubProxyURL: cfg.OTA.GitHubProxyURLOrDefault(),
		},
		HID: HID{
			KeyboardDevice:        cfg.HID.KeyboardDeviceOrDefault(),
			KeyboardLayout:        cfg.HID.KeyboardLayoutOrDefault(),
			MouseDevice:           cfg.HID.MouseDeviceOrDefault(),
			AndroidKeyboardDevice: cfg.HID.AndroidKeyboardDeviceOrDefault(),
			FrameSocket:           cfg.HID.FrameSocketOrDefault(),
			PointerMode:           cfg.PointerModeOrDefault(),
			InputBackend:          cfg.HID.InputBackendOrDefault(),
		},
		Search: Search{
			Provider:  cfg.Search.ProviderOrDefault(),
			HasAPIKey: strings.TrimSpace(cfg.Search.APIKey) != "",
		},
		Telemetry: Telemetry{
			Enabled:           cfg.Telemetry.EnabledOrDefault(),
			Provider:          cfg.Telemetry.ProviderOrDefault(),
			BaseURL:           cfg.Telemetry.BaseURL,
			PublicKey:         cfg.Telemetry.PublicKey,
			SecretKey:         cfg.Telemetry.SecretKey,
			UploadScreenshots: cfg.Telemetry.UploadScreenshotsOrDefault(),
			UploadTimeoutSec:  int(cfg.Telemetry.UploadTimeoutOrDefault().Seconds()),
			MaxRetry:          cfg.Telemetry.MaxRetryOrDefault(),
			Tags:              cfg.Telemetry.Tags,
			Environment:       cfg.Telemetry.EnvironmentOrDefault(),
		},
		LiveActivity: LiveActivity{
			Enabled: boolPtr(cfg.LiveActivity.EnabledOrDefault()),
		},
		TerminationPolicy: cfg.TerminationPolicyOrDefault(),
		Agent: Agent{
			Locale:                     cfg.LocaleOrDefault(),
			Timezone:                   cfg.TimezoneOrDefault(),
			Prompt:                     cfg.Prompt,
			ContextPruneThreshold:      cfg.ContextPruneThreshold,
			ContextCompactionThreshold: cfg.ContextCompactionThreshold,
			InputMode:                  cfg.InputModeOrDefault(),
			VADBackend:                 cfg.VADBackendOrDefault(),
			VADModelPath:               cfg.VADModelPath,
			VADHelperPath:              cfg.VADHelperPath,
			VADSpeechThreshold:         cfg.VADSpeechThreshold,
			SilenceMs:                  cfg.SilenceMs,
			MinSpeechMs:                cfg.MinSpeechMs,
			VoiceFollowupEnabled:       cfg.VoiceFollowupEnabledOrDefault(),
			VoiceFollowupTimeoutMs:     int(cfg.VoiceFollowupTimeoutOrDefault().Milliseconds()),
			VoiceFirstTurnTimeoutMs:    int(cfg.VoiceFirstTurnTimeoutOrDefault().Milliseconds()),
			VoiceMaxTurns:              cfg.VoiceMaxTurns,
			VoiceInterruptOnWakeup:     cfg.VoiceInterruptOnWakeupOrDefault(),
			GPIOWakeupEnabled:          cfg.GPIOWakeupEnabledOrDefault(),
			VoiceStreamingTTSEnabled:   cfg.VoiceStreamingTTSEnabledOrDefault(),
			VoiceToolCallSpeech:        cfg.VoiceToolCallSpeechOrDefault(),
			VoiceProgressSpeechEnabled: cfg.VoiceProgressSpeechEnabledOrDefault(),
			VoiceMaxResponseTokens:     cfg.VoiceMaxResponseTokensOrDefault(),
			MaxIterations:              cfg.MaxIterations,
			ScreenshotKeepN:            cfg.ScreenshotKeepN,
			ScreenshotPruneInterval:    cfg.ScreenshotPruneInterval,
			ScreenStableTimeoutMs:      cfg.ScreenStableTimeoutMs,
			ScreenStableMs:             cfg.ScreenStableMs,
			ScreenStableDiffThreshold:  cfg.ScreenStableDiffThreshold,
		},
	}
}

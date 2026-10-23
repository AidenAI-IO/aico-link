package agent

import "aico-agent/internal/agent/realtimevoice"

import "aico-agent/internal/agent/constants"

const (
	localeSimplifiedChinese = "zh-CN"
	localeEnglishUS         = "en-US"
	defaultLocale           = localeEnglishUS
	defaultInstruction      = "Keep the tone natural, concise, and suitable for TTS playback. " +
		"Use tools whenever reading or changing phone, external-device, or service state; combine multiple tools when needed. " +
		"After every visual observation or input-tool result, inspect the latest screen to verify the previous action, focus, and navigation state before continuing; do not blindly repeat the same click, gesture, or key. " +
		"When opening apps or finding contacts, settings, products, or page content on the phone, prefer system search, in-app search, or visible search fields instead of relying on repeated scrolling. " +
		"Treat requests to place phone calls as phone-automation tasks; do not claim they are impossible merely because there is no dedicated dial tool."
	DefaultRealtimeVoiceCoreInstructions = "You are Aico, the AI agent running on Aico Link hardware, with realtime voice capabilities. The official website is https://aico.link. Focus on natural conversation. " +
		"Reply briefly and clearly in the configured response language. Use get_current_time only when the exact current date or time matters. " +
		"Use recall_memory only when saved user preferences or facts are relevant. " +
		"Use save_memory whenever the user asks you to remember something, and for stable preferences, rules, or procedures you observe; do not claim you remembered something before it returns. Use forget_memory when the user asks you to forget something. " +
		"Use recall_session_chunks when the user refers to earlier conversation you cannot see in your visible context, since only the most recent turns remain visible. " +
		"Use audio_volume to change or read your own speaking volume; do not delegate that as device work. " +
		"Use end_conversation when the user is finished talking or asks to stop the voice interaction, for example when they say goodbye, tell you they need nothing else, ask you to stop listening or stop the service, or ask you to go idle, enter standby, sleep, or wait for the next wakeup. Treat these phrases as conversation-control requests, not as device or backend service-operation requests. Say a short farewell in the same response. Do not use it while the user is still engaged, and do not announce that you are going to standby as though it were a device operation. "
	DefaultRealtimeVoiceBackendInstructions = "For requests that require device operation, external actions, or longer multi-step work, use the available work-management tools and respond as the single assistant responsible for the request. " +
		"Before creating background work, call query_agent_task with no task_id and continue the task that already covers the request instead of starting a duplicate. " +
		"When the user changes the goal of existing background work, use update_agent_task with the complete updated goal instead of cancelling it and creating replacement work. Use cancel_agent_task only when the user wants the work stopped; it also clears a finished result that has not started delivery. " +
		"Never mention foreground or background agents, delegation, queues, orchestration, internal tools, task IDs, or implementation details to the user. Do not say that another agent or process is handling the work; speak naturally in the first person as Aico. " +
		"When work is still in progress, briefly say that you are handling it and continue the conversation without exposing internal state. When work finishes, report the outcome as your own work. " +
		"If you cannot directly and reliably answer or complete a request with the realtime conversation tools, use the work-management tool to have the device-capable assistant handle it before saying that you cannot. This includes device state, screen or page content, apps, images, photos, external actions, information that requires observation or lookup, and any longer or multi-step operation. Never refuse merely because the realtime voice session lacks direct access; delegate the request and report the result as your own work. Do not delegate simple conversation, current-time requests, or relevant memory lookups that you can answer directly. " +
		"If a user action is required, clearly explain what the user should do on the device and ask them to tell you when it is complete, without mentioning why an internal handoff is needed."
	DefaultRealtimeVoiceInstructions         = DefaultRealtimeVoiceCoreInstructions + DefaultRealtimeVoiceBackendInstructions
	DefaultRealtimeToolExecutionInstructions = DefaultRealtimeVoiceCoreInstructions + "For device operation, screen inspection, external actions, or information that requires lookup, use the available tools and continue the request yourself; do not delegate to or mention a backend agent. "
	defaultModelProvider                     = "openrouter"
	defaultModelName                         = "bytedance-seed/seed-2.0-lite"
	defaultModelTemperature                  = 0.2
	// 8192 leaves room for hidden reasoning tokens while keeping a bounded
	// default cost ceiling. Voice replies use their separate 300-token default.
	defaultModelMaxResponseTokens = 8192
	// minReasoningBudgetTokens is Anthropic's documented floor for
	// thinking.budget_tokens; other budget-style APIs are at or below it.
	minReasoningBudgetTokens               = 1024
	defaultModelLogRawHTTP                 = true
	defaultModelReasoningEffort            = ""
	defaultTTSProvider                     = "minimax-cn"
	defaultTTSVoiceID                      = "male-qn-qingse"
	defaultTTSEmotion                      = "happy"
	defaultTTSSpeed                        = 1.0
	defaultSTTProvider                     = "openai-whisper"
	defaultSTTModel                        = "whisper-1"
	defaultSTTLanguage                     = "zh"
	tencentASRProvider                     = "tencent-asr"
	legacyTencentProvider                  = "tencent"
	legacyTencentASRProvider               = "tencent_asr"
	defaultTencentASRRegion                = "ap-shanghai"
	defaultTencentASREngineModel           = "16k_zh"
	defaultAudioSocket                     = "/run/audio_service/audio_service.sock"
	defaultAudioSampleRate                 = 16000
	defaultAudioChannels                   = 1
	defaultAudioBitWidth                   = 16
	defaultAudioArchiveStoragePath         = "/userdata/audio"
	defaultAudioArchiveMaxFiles            = 500
	defaultAudioArchiveMaxSizeMB           = 100
	defaultLLMHTTPLogRetentionDays         = 7
	defaultLogLevel                        = "info"
	defaultNotificationMemoryRetentionDays = 14
	defaultStorageMountPoint               = "/mnt/sdcard"
	defaultStorageDevice                   = "auto"
	defaultStorageMinCardFreeMB            = 64
	// Migration watermarks: start when eMMC free space drops below 10%,
	// stop once it is back at or above 50%.
	defaultStorageMigrateStartFreePct = 10
	defaultStorageMigrateStopFreePct  = 50
	defaultKeyboardDevice             = "/dev/hidg0"
	defaultMouseDevice                = "/dev/hidg1"
	defaultAndroidKeyboardDevice      = "/dev/hidg2"
	defaultFrameServiceSocket         = "/run/frame_service/frame_service.sock"
	defaultPointerMode                = "absolute"
	defaultDeviceType                 = "iOS"
	defaultInputBackend               = "hid"
	// defaultInputMode is empty on purpose: speech providers are opt-in (see
	// applyRuntimeOptionalProviderDefaults), so a device that never configured
	// voice must not inherit a mode whose providers are required. An empty mode
	// means "voice not configured" and is skipped by Config.Validate; the
	// effective mode is resolved by InputModeOrDefault.
	defaultInputMode               = ""
	defaultSilenceMs               = 550
	defaultMinSpeechMs             = 300
	defaultVoiceFollowupTimeoutMs  = 5000
	defaultVoiceFirstTurnTimeoutMs = 10000
	defaultVoiceMaxTurns           = 0
	defaultVoiceMaxResponseTokens  = 300
	defaultMaxIterations           = -1
	defaultVoiceModelProvider      = realtimevoice.ProviderQwen
	defaultVoiceModelModel         = realtimevoice.DefaultQwenRealtimeModel
	defaultVoiceModelVoice         = realtimevoice.DefaultQwenRealtimeVoice
	defaultVoiceModelTurnDetection = "server_vad"
	// defaultContextCompactionThreshold is the fraction of the usable model
	// input budget at which conversation compaction summarizes the transcript.
	defaultContextCompactionThreshold = 0.8
	// defaultContextPruneThreshold is the fraction of the usable model input
	// budget at which deterministic pruning of stale state and older tool
	// exchanges runs. It sits below the compaction threshold so the cheap
	// deterministic pass gets a chance to free tokens before the LLM summary.
	defaultContextPruneThreshold = 0.5
	// GPIO 3 (physical pin 38) is the Quick Capture button on Luckfox Pico Zero.
	// Zero disables the trigger; see QuickCaptureConfig.Validate.
	defaultQuickCaptureGPIOPin = 3

	defaultTelemetryProvider    = "langfuse"
	defaultTelemetryTimeoutSec  = 30
	defaultTelemetryMaxRetry    = 2
	defaultTelemetryEnvironment = "default"
)

func defaultBoolPtr(value bool) *bool {
	v := value
	return &v
}

func DefaultConfig() Config {
	return Config{
		Timezone: defaultTimezone,
		Model: ModelConfig{
			Provider: defaultModelProvider,
			Model:    defaultModelName,
			// Temperature is intentionally left unset here; the effective value is
			// resolved at load time (see applyModelTemperatureDefault). Native
			// Gemini models without a known default stay unset; other models fall
			// back to defaultModelTemperature.
			MaxResponseTokens: defaultModelMaxResponseTokens,
			LogRawHTTP:        defaultModelLogRawHTTP,
			ReasoningEffort:   defaultModelReasoningEffort,
		},
		TTS: TTSConfig{
			Provider: defaultTTSProvider,
			VoiceID:  defaultTTSVoiceID,
			Emotion:  defaultTTSEmotion,
			Speed:    defaultTTSSpeed,
		},
		STT: STTConfig{
			Provider: defaultSTTProvider,
			Model:    defaultSTTModel,
			Language: defaultSTTLanguage,
		},
		Audio: AudioConfig{
			Socket:     defaultAudioSocket,
			SampleRate: defaultAudioSampleRate,
			Channels:   defaultAudioChannels,
			BitWidth:   defaultAudioBitWidth,
			Backend:    AudioBackendAuto,
		},
		VoiceModel: VoiceModelConfig{
			Provider:          defaultVoiceModelProvider,
			Model:             defaultVoiceModelModel,
			Voice:             defaultVoiceModelVoice,
			InputAudioFormat:  "pcm",
			OutputAudioFormat: "pcm",
			TurnDetection:     defaultVoiceModelTurnDetection,
		},
		AudioArchive: AudioArchiveConfig{
			Enabled:     true,
			MaxFiles:    defaultAudioArchiveMaxFiles,
			MaxSizeMB:   defaultAudioArchiveMaxSizeMB,
			StoragePath: defaultAudioArchiveStoragePath,
		},
		FrameService: FrameServiceConfig{
			KeepStreamOn: false,
		},
		QuickCapture: QuickCaptureConfig{
			Enabled:         defaultBoolPtr(true),
			GPIOPin:         defaultQuickCaptureGPIOPin,
			ScreenMemoryTTL: DefaultScreenMemoryTTL,
		},
		VoiceNotifications: VoiceNotificationsConfig{
			Enabled:       defaultBoolPtr(true),
			MaxPending:    8,
			RetentionDays: defaultNotificationMemoryRetentionDays,
			ResponseTail: VoiceNotificationResponseTailConfig{
				Enabled:      defaultBoolPtr(true),
				MaxItems:     1,
				MaxTextChars: 40,
			},
			Expiration: VoiceNotificationExpirationConfig{
				DefaultTTLSeconds: 0,
				CodeTTLSeconds: map[string]int{
					"storage": 900,
				},
			},
		},
		Storage: StorageConfig{
			MountPoint:           defaultStorageMountPoint,
			Device:               defaultStorageDevice,
			MinCardFreeMB:        defaultStorageMinCardFreeMB,
			MigrateStartFreePct:  defaultStorageMigrateStartFreePct,
			MigrateStopFreePct:   defaultStorageMigrateStopFreePct,
			MonitorEnabled:       true,
			RootPath:             "/userdata",
			CheckIntervalSeconds: 300,
			WarningThresholdMB:   50,
			CriticalThresholdMB:  10,
			EmergencyThresholdMB: 5,
			RecoveryHysteresisMB: 5,
			DegradedMode: StorageDegradedModeConfig{
				DisableLLMHTTPLog:     true,
				DisableAudioArchive:   true,
				DisableSessionArchive: true,
				MaxAgentLogMB:         1,
			},
			Cleanup: StorageCleanupConfig{
				Enabled:                          true,
				LLMHTTPLogRetentionDays:          []int{7, 3, 1, 0},
				AudioArchiveRetentionDays:        []int{30, 7, 0},
				SessionArchiveRetentionDays:      []int{30},
				NotificationContextRetentionDays: []int{14, 7, 1, 0},
				CleanupRetryIntervalSeconds:      60,
			},
		},
		Log: LogConfig{
			LLMHTTPRetentionDays: defaultLLMHTTPLogRetentionDays,
			Level:                defaultLogLevel,
		},
		HID: HIDConfig{
			KeyboardDevice:        defaultKeyboardDevice,
			KeyboardLayout:        defaultKeyboardLayout,
			MouseDevice:           defaultMouseDevice,
			AndroidKeyboardDevice: defaultAndroidKeyboardDevice,
			FrameSocket:           defaultFrameServiceSocket,
			PointerMode:           defaultPointerMode,
			InputBackend:          defaultInputBackend,
		},
		Device: DeviceConfig{
			DeviceType: defaultDeviceType,
		},
		Search: SearchConfig{
			Provider: searchProviderDuckDuckGo,
		},
		Telemetry: TelemetryConfig{
			Enabled:           defaultBoolPtr(false),
			Provider:          defaultTelemetryProvider,
			UploadScreenshots: defaultBoolPtr(true),
			UploadTimeoutSec:  defaultTelemetryTimeoutSec,
			MaxRetry:          defaultTelemetryMaxRetry,
			Tags:              []string{},
			Environment:       defaultTelemetryEnvironment,
		},
		Locale:                     defaultLocale,
		Instruction:                defaultInstruction,
		InputMode:                  defaultInputMode,
		VADBackend:                 defaultVADBackend,
		VADModelPath:               defaultVADModelPath,
		VADHelperPath:              defaultVADHelperPath,
		VADSpeechThreshold:         defaultVADSpeechThreshold,
		SilenceMs:                  defaultSilenceMs,
		MinSpeechMs:                defaultMinSpeechMs,
		VoiceFollowupEnabled:       defaultBoolPtr(false),
		VoiceFollowupTimeoutMs:     defaultVoiceFollowupTimeoutMs,
		VoiceFirstTurnTimeoutMs:    defaultVoiceFirstTurnTimeoutMs,
		VoiceMaxTurns:              defaultVoiceMaxTurns,
		VoiceInterruptOnWakeup:     defaultBoolPtr(true),
		VoiceStreamingTTSEnabled:   defaultBoolPtr(true),
		VoiceToolCallSpeech:        defaultBoolPtr(true),
		VoiceProgressSpeechEnabled: defaultBoolPtr(true),
		VoiceMaxResponseTokens:     defaultVoiceMaxResponseTokens,
		MaxIterations:              defaultMaxIterations,
		TerminationPolicy:          DefaultTerminationPolicyConfig(),
		ContextCompactionThreshold: defaultContextCompactionThreshold,
		ContextPruneThreshold:      defaultContextPruneThreshold,
		ScreenshotKeepN:            constants.DefaultScreenshotKeepN,
		ScreenshotPruneInterval:    constants.DefaultScreenshotPruneInterval,
		ScreenStableTimeoutMs:      defaultStableWaitTimeoutMs,
		ScreenStableMs:             defaultStableDurationMs,
		ScreenStableDiffThreshold:  defaultDiffThreshold,
	}
}

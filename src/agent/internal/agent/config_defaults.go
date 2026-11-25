package agent

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/realtimevoice"
)

const (
	localeSimplifiedChinese = "zh-CN"
	localeEnglishUS         = "en-US"
	defaultLocale           = localeEnglishUS
	defaultInstruction      = "Keep the tone natural, concise, and suitable for TTS playback. " +
		"Use tools whenever reading or changing phone, external-device, or service state; combine multiple tools when needed. " +
		"After every visual observation or input-tool result, inspect the latest screen to verify the previous action, focus, and navigation state before continuing; do not blindly repeat the same click, gesture, or key. " +
		"When opening apps or finding contacts, settings, products, or page content on the phone, prefer system search, in-app search, or visible search fields instead of relying on repeated scrolling. " +
		"Treat requests to place phone calls as phone-automation tasks; do not claim they are impossible merely because there is no dedicated dial tool. " +
		"Use wait_for_wakeup when the user is finished talking or asks to stop the voice interaction, for example when they say goodbye, tell you they need nothing else, ask you to stop listening or stop the service, or ask you to go idle, enter standby, sleep, or wait for the next wakeup. Treat these as conversation-control requests, not device or backend service-operation requests. Say a short farewell in the same response and call wait_for_wakeup in that turn; a spoken farewell alone does not end the session. Do not use it while the user is still engaged, and do not announce standby as though it were a device operation."
	// The user talks to one assistant, Aico. The realtime prompts describe
	// what Aico can do, never how the work is split between models.
	DefaultRealtimeVoiceCoreInstructions = "You are Aico, the voice assistant running on Aico Link hardware; the official website is https://aico.link. You talk with the user in real time, and you can see and operate the phone and other devices connected to Aico Link: read the screen, open apps, tap and type, check device state, look things up, and carry out multi-step tasks. Answer questions about what you can do from these abilities. " +
		"Reply briefly and naturally in the configured response language, in plain spoken sentences: no lists, Markdown, or technical details. Use get_current_time only when the exact current date or time matters. " +
		"Use recall_memory only when saved user preferences or facts are relevant. " +
		"Until a result shows the actual outcome, only say that you are on it; do not claim success or failure. A tool result that only says work was accepted, queued, or started is not outcome evidence. A tool error is evidence that the work could not start or complete as requested; say so instead of continuing to say you are handling it. " +
		personalMemoryAdmissionGuidance + " Use forget_memory when the user asks you to forget something. " +
		"Use recall_session_chunks when the user refers to earlier conversation you cannot see in your visible context, since only the most recent turns remain visible. " +
		"Use audio_volume to change or read your own speaking volume, not the phone's. " +
		"Use end_conversation when the user is finished or asks you to stop listening, stop the service, go idle, enter standby, sleep, or wait for the next wakeup, or says goodbye, 退下, 退一下, or 再见. These are conversation-control requests, not device operations. Say a short farewell in the same response, and do not use it while the user is still engaged. "
	DefaultRealtimeVoiceBackendInstructions = "You act on the phone and devices through create_agent_task. Use it for anything that needs the screen, apps, device state, external actions, lookups, or several steps, and tell the user you are on it. Saying you will do something does nothing by itself: call the tool in the same response, including when the user asks to retry work that already finished or failed. Never say you cannot see or do something before trying it this way. Answer directly only for conversation, the time, and memory. " +
		"If the user repeats or rephrases a request that is still under way, do not start it again; say you are on it, or check it with query_agent_task. When the user changes that same request, use update_agent_task with the complete new goal. Never use update_agent_task to add an unrelated request to existing work; a new request gets its own create_agent_task, even while other work runs. Use cancel_agent_task only when the user wants the work stopped, and confirm the stop only when status is cancelled. " +
		"Only work with status completed, failed, or cancelled shows the outcome, whether a work update or query_agent_task reports it; queued, running, and cancelling do not. Then tell the user the result in a sentence or two as your own work; if it failed or was cancelled, say so and mention any steps already done. If the user must do something on the device, say what and ask them to tell you when it is done. " +
		"The user sees a single assistant: never mention tasks, task IDs, agents, background work, queues, or how the work was carried out."
	DefaultRealtimeVoiceInstructions         = DefaultRealtimeVoiceCoreInstructions + DefaultRealtimeVoiceBackendInstructions
	DefaultRealtimeToolExecutionInstructions = DefaultRealtimeVoiceCoreInstructions + "For device operation, screen inspection, external actions, or information that requires lookup, use the available tools and continue the request yourself; do not delegate to or mention a backend agent. Until a direct tool returns, do not claim device or external state has changed. "
	defaultModelProvider                     = "openrouter"
	defaultModelName                         = "bytedance-seed/seed-2.0-lite"
	defaultModelTemperature                  = 0.2
	// 0 sends no output limit, so each provider applies its model default
	// (native Anthropic, which requires max_tokens, uses the model's advertised
	// output limit instead).
	defaultModelMaxResponseTokens = 0
	// minReasoningBudgetTokens is Anthropic's documented floor for
	// thinking.budget_tokens; other budget-style APIs are at or below it.
	minReasoningBudgetTokens               = 1024
	defaultModelLogRawHTTP                 = true
	defaultModelReasoningEffort            = ""
	defaultTTSProvider                     = "ekho"
	defaultTTSVoiceID                      = ""
	defaultTTSEmotion                      = ""
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
	defaultPointerMode                = "touchpad"
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
	// defaultContextPruneStateLimit also triggers that cleanup once the
	// context holds more <state> messages with a screenshot. The token trigger
	// cannot catch screenshot growth: a screenshot is a few hundred tokens but
	// 50-100 KB of base64 resent in every request, so on a 145 MB board the
	// request outgrew memory long before the token trigger (about 293k tokens
	// for deepseek-flash) fired. 6 kept board latency and swap at the level of
	// 4 while pruning a third less often.
	defaultContextPruneStateLimit = 6
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

// DefaultConfig returns the built-in configuration that runtime loading starts
// from before applying agent.toml. Values set in the file override these
// defaults, including an explicit voice_model.use_backend_agent = false.
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
			UseBackendAgent:   true,
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
			KeyboardLayout:        mnk.DefaultKeyboardLayout,
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
		VoiceFollowupEnabled:       defaultBoolPtr(true),
		VoiceFollowupTimeoutMs:     defaultVoiceFollowupTimeoutMs,
		VoiceFirstTurnTimeoutMs:    defaultVoiceFirstTurnTimeoutMs,
		VoiceMaxTurns:              defaultVoiceMaxTurns,
		VoiceInterruptOnWakeup:     defaultBoolPtr(true),
		VoiceSpeakerGateEnabled:    defaultBoolPtr(false),
		VoiceSpeakerThreshold:      defaultSpeakerThreshold,
		VoiceSpeakerToolsDir:       defaultSpeakerToolsDir,
		VoiceSpeakerModelDir:       defaultSpeakerModelDir,
		VoiceSpeakerStoreDir:       defaultSpeakerStoreDir,
		VoiceSpeakerMaxProfiles:    defaultSpeakerMaxProfiles,
		TapWakeupEnabled:           defaultBoolPtr(true),
		MCPEnabled:                 defaultBoolPtr(true),
		UIGroundingEnabled:         defaultBoolPtr(true),
		MCPShellEnabled:            defaultBoolPtr(false),
		VoiceStreamingTTSEnabled:   defaultBoolPtr(true),
		VoiceToolCallSpeech:        defaultBoolPtr(true),
		VoiceProgressSpeechEnabled: defaultBoolPtr(true),
		MaxIterations:              defaultMaxIterations,
		TerminationPolicy:          DefaultTerminationPolicyConfig(),
		ContextCompactionThreshold: defaultContextCompactionThreshold,
		ContextPruneThreshold:      defaultContextPruneThreshold,
		ContextPruneStateLimit:     defaultContextPruneStateLimit,
		ScreenStableTimeoutMs:      defaultStableWaitTimeoutMs,
		ScreenStableMs:             defaultStableDurationMs,
		ScreenStableDiffThreshold:  defaultDiffThreshold,
	}
}

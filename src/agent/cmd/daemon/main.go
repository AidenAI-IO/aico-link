package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/backup"
	"aiden-agent/internal/configupdate"
	"aiden-agent/internal/configweb"
	"aiden-agent/internal/logging"
	"aiden-agent/internal/wifiproxy"
)

const (
	wakeupListenTimeout            = 10 * time.Second
	wakeupDebounceInterval         = 500 * time.Millisecond
	defaultVoiceSteerListenTimeout = 45 * time.Second
	// While dialog TTS is actually playing, require only a short sustained
	// speech window so near-field talk can force-stop playback quickly.
	ttsBargeInNeedFrames                    = 5  // ~160ms @ 32ms Silero frames
	thinkBargeInNeed                        = 10 // ~320ms without active TTS
	voiceWakeupInterruptedCorrectionContext = "Voice interruption: the user pressed the physical wakeup button " +
		"while the previous voice turn was still running. The previous task was interrupted and did NOT complete. " +
		"Based on the user's new input, judge the relationship to the interrupted task:\n" +
		"- If the new input replaces the old task (e.g. \"don't do that\", \"do B instead\") → only execute the new request.\n" +
		"- If the new input adds to or reorders (e.g. \"then do B\", \"do B first\") → execute the new request first, then briefly ask whether to resume the interrupted task.\n" +
		"- If ambiguous → briefly confirm the user's intent before acting."
)

var voiceTurnCancelWaitTimeout = 2 * time.Second
var wakeupDebounceNow = time.Now

var wakeupGPIOPins = []int{33, 32}

func main() {
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	// Check if a subcommand is provided
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "config-check":
			os.Exit(RunConfigCheck(os.Args[2:]))
		case "config-meta":
			os.Exit(runConfigMeta(os.Args[2:]))
		case "config":
			os.Exit(runConfig(os.Args[2:]))
		case "config-update":
			os.Exit(runConfigUpdate(os.Args[2:]))
		case "config-test":
			os.Exit(runConfigTest(os.Args[2:]))
		case "config-web":
			os.Exit(configweb.Run(os.Args[2:]))
		case "backup-recover":
			os.Exit(backup.RunRecover(os.Args[2:], os.Stdout, os.Stderr))
		case "wifi-proxy":
			os.Exit(wifiproxy.Run(os.Args[2:]))
		case "wifi-region-apply":
			os.Exit(runWiFiRegionApply(os.Args[2:]))
		}
	}

	// Default: run as daemon
	var (
		dataDir                   = flag.String("dir", "", "path to the agent data directory holding agent.toml, skills, memory, cache and logs (required)")
		addr                      = flag.String("addr", "0.0.0.0:8080", "HTTP server address")
		deviceType                = registerDeviceTypeFlag(flag.CommandLine)
		environmentBridgeMode     = flag.Bool("environment-bridge-mode", false, "Enable environment bridge screen and input providers")
		environmentBridgeEndpoint = flag.String("environment-bridge-endpoint", "", "Environment bridge endpoint (e.g., http://192.168.50.123:8080)")
		benchmarkTaskID           = flag.String("benchmark-task-id", "", "Benchmark task id to include on environment bridge requests for task routing")
		benchmarkHeadless         = flag.Bool("benchmark-headless", false, "Hide unavailable device tools in a benchmark container without an environment bridge")
		benchmarkMockTools        = flag.Bool("benchmark-mock-tools", false, "Use scripted device tools for benchmark policy fixtures (requires benchmark token and environment bridge)")
		benchmarkTokenFile        = flag.String("benchmark-token-file", "", "Path to benchmark API bearer token file. Enables benchmark-only mutation endpoints.")
	)
	flag.Parse()
	environmentPath := os.Getenv("AIDEN_SYSTEM_ENV")
	if environmentPath == "" {
		environmentPath = "/userdata/system/env"
	}
	environmentRevision, environmentErr := agent.SystemEnvironmentRevision(environmentPath)

	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "data_directory_missing")
		os.Exit(1)
	}

	cfg, err := agent.LoadRuntimeConfigFromDir(dir)
	if err != nil {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "config_load_failed",
			logging.Field{Key: "dir", Value: dir},
			logging.Field{Key: "error", Value: err},
		)
		os.Exit(1)
	}
	if err := applyDeviceTypeOverride(&cfg, *deviceType); err != nil {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "device_type_override_invalid",
			logging.Field{Key: "device_type", Value: strings.TrimSpace(*deviceType)},
			logging.Field{Key: "error", Value: err},
		)
		os.Exit(1)
	}
	if err := agent.ApplyTimezone(cfg); err != nil {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "timezone_apply_failed",
			logging.Field{Key: "timezone", Value: cfg.TimezoneOrDefault()},
			logging.Field{Key: "error", Value: err},
		)
		os.Exit(1)
	}

	// Apply CLI flags to override config file
	if *environmentBridgeMode {
		cfg.EnvironmentBridge.Enabled = true
		if *environmentBridgeEndpoint != "" {
			cfg.EnvironmentBridge.Endpoint = *environmentBridgeEndpoint
		}
		if cfg.EnvironmentBridge.Endpoint == "" {
			_ = logging.LogEvent(logging.Error, "agent", "startup", "environment_bridge_endpoint_missing")
			os.Exit(1)
		}
		cfg.EnvironmentBridge.BenchmarkTaskID = strings.TrimSpace(*benchmarkTaskID)
	}
	if strings.TrimSpace(*benchmarkTokenFile) != "" {
		data, err := os.ReadFile(strings.TrimSpace(*benchmarkTokenFile))
		if err != nil {
			_ = logging.LogEvent(logging.Error, "agent", "startup", "benchmark_token_read_failed",
				logging.Field{Key: "path", Value: strings.TrimSpace(*benchmarkTokenFile)},
				logging.Field{Key: "error", Value: err},
			)
			os.Exit(1)
		}
		cfg.Benchmark.Token = strings.TrimSpace(string(data))
		if cfg.Benchmark.Token == "" {
			_ = logging.LogEvent(logging.Error, "agent", "startup", "benchmark_token_empty")
			os.Exit(1)
		}
	}
	if *benchmarkHeadless {
		if cfg.Benchmark.Token == "" || cfg.EnvironmentBridge.Enabled {
			fmt.Fprintln(os.Stderr, "benchmark-headless requires benchmark-token-file and no environment bridge")
			os.Exit(1)
		}
		cfg.Benchmark.Headless = true
	}
	if *benchmarkMockTools {
		if cfg.Benchmark.Token == "" || !cfg.EnvironmentBridge.Enabled || cfg.EnvironmentBridge.Endpoint == "" {
			fmt.Fprintln(os.Stderr, "benchmark-mock-tools requires benchmark-token-file and environment-bridge-mode with an endpoint")
			os.Exit(1)
		}
		cfg.Benchmark.MockTools = true
	}
	proxyConfig := agent.ProxyConfigFromEnvironment()
	if err := proxyConfig.Validate(); err != nil {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "proxy_environment_invalid",
			logging.Field{Key: "error", Value: err},
		)
		os.Exit(1)
	}
	cfg.SkillMergeModel = agent.NewLLMSkillMergeModel(agent.NewModelManager(cfg.Model, proxyConfig))
	persistedConfig := cfg
	cfg, err = configForRunningUSB(cfg)
	if err != nil {
		logging.Errorf("agent", "input", "USB boot settings failed: %v", err)
		exitCode = 1
		return
	}

	runtime, err := agent.NewRuntime(cfg)
	if err != nil {
		_ = logging.LogEvent(logging.Error, "agent", "startup", "runtime_create_failed",
			logging.Field{Key: "error", Value: err},
		)
		os.Exit(1)
	}
	defer runtime.Close()
	runtime.InitializeConfigApplication(persistedConfig)
	runtime.RegisterConfigTools(configupdate.NewService())
	if environmentErr == nil {
		runtime.SetInitialEnvironmentRevision(environmentRevision)
	}
	if err := runtime.StartStorageMonitor(); err != nil {
		logging.Warnf("agent", "storage_monitor", "startup check failed: %v", err)
	}
	if err := runtime.PrimeScreenMappingOnStartup(context.Background()); err != nil {
		logging.Warnf("agent", "init", "screen mapping prime failed: %v", err)
	}

	// HTTP server runs in all input modes so the web UI is available even
	// during voice (audio/stt) interactions.
	server := agent.NewServer(runtime, *addr)
	defer server.Close()
	inputs := newInputLifecycle(runtime, server)
	if err := inputs.Start(cfg); err != nil {
		logging.Errorf("agent", "input", "startup failed: %v", err)
		exitCode = 1
		return
	}
	defer func() { inputs.StopForShutdown(); runtime.StopConfigReloads(); inputs.Close() }()
	runtime.SetConfigPreparer(inputs.Prepare)

	_ = logging.LogEvent(logging.Info, "agent", "startup", "daemon_starting",
		logging.Field{Key: "addr", Value: *addr},
		logging.Field{Key: "dir", Value: dir},
	)
	if cfg.EnvironmentBridge.Enabled {
		fields := []logging.Field{
			{Key: "endpoint", Value: cfg.EnvironmentBridge.Endpoint},
		}
		if cfg.EnvironmentBridge.BenchmarkTaskID != "" {
			fields = append(fields, logging.Field{Key: "benchmark_task_id", Value: cfg.EnvironmentBridge.BenchmarkTaskID})
		}
		_ = logging.LogEvent(logging.Info, "agent", "environment_bridge", "bridge_enabled", fields...)
	}
	if _, port, err := net.SplitHostPort(*addr); err == nil && port != "" {
		_ = logging.LogEvent(logging.Info, "agent", "http", "web_ui_available",
			logging.Field{Key: "url", Value: "http://localhost:" + port},
		)
		if cfg.HID.InputBackendADB() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := agent.EnsureADBReverse(ctx, port, port); err != nil {
				logging.Warnf("agent", "adb", "reverse tcp:%s -> tcp:%s setup failed: %v", port, port, err)
			} else {
				logging.Infof("agent", "adb", "reverse tcp:%s -> tcp:%s configured for companion app desktop bridge", port, port)
			}
			cancel()
		}
	}
	_ = logging.LogEvent(logging.Info, "agent", "startup", "log_directory_configured",
		logging.Field{Key: "path", Value: filepath.Join(dir, "log")},
	)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start()
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serverErr:
		if err != nil {
			logging.Errorf("agent", "server", "stopped: %v", err)
			exitCode = 1
		}
	case <-signals:
	}

}

func registerDeviceTypeFlag(fs *flag.FlagSet) *string {
	return fs.String("device-type", "", "Override device.device_type for this daemon process (iOS, Android, macOS, windows, or linux)")
}

func applyDeviceTypeOverride(cfg *agent.Config, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return cfg.OverrideDeviceType(value)
}

type audioDialogRunner interface {
	StartRecording() error
	StopRecording() error
	FinalizeAndRotateSTT() error
	RecordingActive() bool
	ReadRecordChunk(timeoutMs uint32) (*agent.AudioChunkResult, error)
	ReadRecordChunkMonitor(timeoutMs uint32) (*agent.AudioChunkResult, error)
	ProcessVADFrame(samples []int16) ([]int16, error)
	ProcessUtterance(ctx context.Context, utterance []int16, runtime *agent.Runtime) error
	PrepareTurnInput(utterance []int16) (agent.TurnInput, error)
	RunVoiceTurn(ctx context.Context, input agent.TurnInput, utterance []int16, runtime *agent.Runtime) (agent.RunResult, error)
	RunVoiceTurnWithContext(ctx context.Context, input agent.TurnInput, utterance []int16, runtime *agent.Runtime, turnContext agent.VoiceTurnContext) (agent.RunResult, error)
	QueueSteer(input agent.TurnInput) bool
	BeginSteerInterrupt() bool
	ResumeSteerInterrupt() bool
	WaitForVoiceRunIdle(ctx context.Context) bool
	ForceResetVoiceRun()
	InterruptOutput()
	CanSpeakFinalText() bool
	Speak(ctx context.Context, text string, interrupt <-chan struct{}) error
	SpeakFinal(ctx context.Context, text string, interrupt <-chan struct{}) error
	TTSOutputActive() bool
	PlayListenReadyCue()
	FlushVAD() []int16
	FinishPendingUtterance(pending []int16) []int16
	ResetVAD()
	VADFrameSamples() int
	VADSpeaking() bool
}

type wakeupWatcher interface {
	Start() error
	Stop()
}

type wakeupWatcherFactory func(pin int, callback func()) (wakeupWatcher, error)

func newGPIOWatcher(pin int, callback func()) (wakeupWatcher, error) {
	return agent.NewGPIOWatcher(pin, callback)
}

type quickCaptureTrigger interface {
	TriggerQuickCapture() error
}

func startQuickCaptureGPIOWatcher(cfg agent.Config, trigger quickCaptureTrigger, newWatcher wakeupWatcherFactory) (wakeupWatcher, error) {
	pin := cfg.QuickCapture.GPIOPin
	if !cfg.QuickCapture.EnabledOrDefault() || pin == 0 {
		return nil, nil
	}
	watcher, err := newWatcher(pin, func() {
		err := trigger.TriggerQuickCapture()
		switch {
		case err == nil:
			logging.Infof("agent", "quick_capture", "GPIO %d triggered capture", pin)
		case errors.Is(err, agent.ErrQuickCaptureBusy):
			logging.Warnf("agent", "quick_capture", "GPIO %d ignored: capture already in progress", pin)
		default:
			logging.Errorf("agent", "quick_capture", "GPIO %d trigger failed: %v", pin, err)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("create GPIO %d watcher: %w", pin, err)
	}
	if err := watcher.Start(); err != nil {
		watcher.Stop()
		return nil, fmt.Errorf("start GPIO %d watcher: %w", pin, err)
	}
	return watcher, nil
}

func wakeupGPIOPinsLabel() string {
	return "GPIO 33/GPIO 32"
}

func startWakeupWatchers(newWatcher wakeupWatcherFactory, callback func()) ([]wakeupWatcher, error) {
	return startWakeupWatchersWithDebounce(newWatcher, callback, wakeupDebounceInterval, wakeupDebounceNow)
}

// startWakeupWatchersIfEnabled registers GPIO 32/33 listeners when tap wakeup
// is enabled. When disabled it returns nil watchers so voice loops keep waiting
// on reload/signal (and realtime /api/chat) without opening sessions from hardware.
func startWakeupWatchersIfEnabled(cfg agent.Config, newWatcher wakeupWatcherFactory, callback func()) ([]wakeupWatcher, error) {
	if !cfg.TapWakeupEnabledOrDefault() {
		logging.Infof("agent", "wakeup", "Tap wakeup disabled (tap_wakeup_enabled=false); ignoring tap button on %s", wakeupGPIOPinsLabel())
		return nil, nil
	}
	return startWakeupWatchers(newWatcher, callback)
}

type wakeupDebouncer struct {
	mu       sync.Mutex
	interval time.Duration
	now      func() time.Time
	last     time.Time
}

func newWakeupDebouncer(interval time.Duration, now func() time.Time) *wakeupDebouncer {
	if now == nil {
		now = time.Now
	}
	return &wakeupDebouncer{
		interval: interval,
		now:      now,
	}
}

func (d *wakeupDebouncer) allow() bool {
	if d == nil || d.interval <= 0 {
		return true
	}

	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.last.IsZero() {
		elapsed := now.Sub(d.last)
		if elapsed >= 0 && elapsed < d.interval {
			return false
		}
	}
	d.last = now
	return true
}

func startWakeupWatchersWithDebounce(newWatcher wakeupWatcherFactory, callback func(), debounceInterval time.Duration, now func() time.Time) ([]wakeupWatcher, error) {
	watchers := make([]wakeupWatcher, 0, len(wakeupGPIOPins))
	debouncer := newWakeupDebouncer(debounceInterval, now)
	for _, pin := range wakeupGPIOPins {
		pin := pin
		watcher, err := newWatcher(pin, func() {
			if !debouncer.allow() {
				logging.Warnf("agent", "wakeup", "GPIO %d wakeup event ignored by %s debounce", pin, debounceInterval)
				return
			}
			logging.Infof("agent", "wakeup", "GPIO %d wakeup event", pin)
			callback()
		})
		if err != nil {
			stopWakeupWatchers(watchers)
			return nil, fmt.Errorf("create GPIO %d watcher: %w", pin, err)
		}
		if err := watcher.Start(); err != nil {
			stopWakeupWatchers(append(watchers, watcher))
			return nil, fmt.Errorf("start GPIO %d watcher: %w", pin, err)
		}
		watchers = append(watchers, watcher)
	}
	return watchers, nil
}

func stopWakeupWatchers(watchers []wakeupWatcher) {
	for i := len(watchers) - 1; i >= 0; i-- {
		watchers[i].Stop()
	}
}

type vadDebugReporter interface {
	VADDebugState() agent.VADDebugState
}

func formatVADDebugLog(state agent.VADDebugState) string {
	msg := fmt.Sprintf("[vad] probability=%.3f threshold=%.3f speaking=%v speech_frames=%d silence_frames=%d",
		state.Probability,
		state.Threshold,
		state.Speaking,
		state.SpeechFrames,
		state.SilenceFrames,
	)
	if state.LastError != "" {
		msg += fmt.Sprintf(" last_error=%q", state.LastError)
	}
	return msg
}

type voiceEvent int

const (
	voiceEventWakeup voiceEvent = iota + 1
)

type pendingVoiceTurn struct {
	input       agent.TurnInput
	utterance   []int16
	turnContext agent.VoiceTurnContext
}

type voiceTurnResult struct {
	interrupted            bool
	waitForWakeupRequested bool
	// requeueWakeup is set when a GPIO tap closed a wait_for_wakeup turn. The
	// tap already meant "start again"; re-signal so the outer loop opens the
	// next session without requiring a second press.
	requeueWakeup   bool
	exit            bool
	nextTurn        *pendingVoiceTurn
	followUpContext agent.VoiceTurnContext
}

func runWakeupMode(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, sigChan chan os.Signal, newWatcher wakeupWatcherFactory, reload ...<-chan struct{}) {
	// Classic final UX is a Realtime-like continuous session. Single-turn STT
	// remains available only when voice_followup_enabled is explicitly false.
	if cfg.InputModeOrDefault() == "stt" && !cfg.VoiceFollowupEnabledOrDefault() {
		runSingleTurnSTTWakeupMode(cfg, dialog, runtime, sigChan, newWatcher, reload...)
		return
	}
	if cfg.InputModeOrDefault() != "stt" {
		runLegacyWakeupMode(cfg, dialog, runtime, sigChan, newWatcher, reload...)
		return
	}

	if cfg.TapWakeupEnabledOrDefault() {
		logging.Infof("agent", "ready", "Starting GPIO wakeup listeners on %s...", wakeupGPIOPinsLabel())
	}

	events := make(chan voiceEvent, 1)

	watchers, err := startWakeupWatchersIfEnabled(cfg, newWatcher, func() {
		dialog.InterruptOutput()
		signalVoiceWakeupEvent(events)
	})
	if err != nil {
		logging.Errorf("agent", "daemon", "Failed to start GPIO wakeup listeners: %v", err)
		return
	}
	defer stopWakeupWatchers(watchers)

	logging.Infof("agent", "ready", "Waiting for wakeup event (%s)... Ctrl+C to quit", wakeupReadyLabel(cfg))

	for {
		if reloadRequested(reload) {
			return
		}
		select {
		case <-reloadStop(reload):
			return
		case <-sigChan:
			logging.Infof("agent", "exit", "Stopped.")
			return
		case <-events:
			logging.Infof("agent", "wakeup", "GPIO wakeup triggered, opening voice session...")
			if exit := runVoiceSession(cfg, dialog, runtime, sigChan, events); exit {
				logging.Infof("agent", "exit", "Stopped.")
				return
			}
			logging.Infof("agent", "ready", "Waiting for wakeup event...")
		}
	}
}

func runLegacyWakeupMode(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, sigChan chan os.Signal, newWatcher wakeupWatcherFactory, reload ...<-chan struct{}) {
	if cfg.TapWakeupEnabledOrDefault() {
		logging.Infof("agent", "ready", "Starting GPIO wakeup listeners on %s...", wakeupGPIOPinsLabel())
	}

	ctx := context.Background()
	wakeupEvents := make(chan struct{}, 1)

	watchers, err := startWakeupWatchersIfEnabled(cfg, newWatcher, func() {
		dialog.InterruptOutput()
		signalWakeupEvent(wakeupEvents)
	})
	if err != nil {
		logging.Errorf("agent", "daemon", "Failed to start GPIO wakeup listeners: %v", err)
		return
	}
	defer stopWakeupWatchers(watchers)

	logging.Infof("agent", "ready", "Waiting for wakeup event (%s)... Ctrl+C to quit", wakeupReadyLabel(cfg))

	pendingWakeup := false
	for {
		if reloadRequested(reload) {
			return
		}
		if !pendingWakeup {
			select {
			case <-reloadStop(reload):
				return
			case <-sigChan:
				if dialog != nil {
					dialog.StopRecording()
				}
				logging.Infof("agent", "exit", "Stopped.")
				return
			case <-wakeupEvents:
				logging.Infof("agent", "wakeup", "GPIO wakeup triggered, starting to listen...")
			}
		} else {
			pendingWakeup = false
		}

		// Start recording
		logging.Infof("agent", "listen", "Recording audio...")
		if err := dialog.StartRecording(); err != nil {
			logging.Errorf("agent", "daemon", "Failed to start recording: %v", err)
			continue
		}

		dialog.ResetVAD()
		exit, interrupted := processAudioUntilUtteranceWithWakeupInterrupt(
			dialog,
			runtime,
			ctx,
			sigChan,
			wakeupEvents,
			cfg.VoiceInterruptOnWakeupOrDefault(),
			wakeupListenTimeout,
		)
		if exit {
			dialog.StopRecording()
			logging.Infof("agent", "exit", "Stopped.")
			return
		}

		// Stop recording
		dialog.StopRecording()

		if interrupted {
			pendingWakeup = true
			continue
		}

		logging.Infof("agent", "ready", "Waiting for next wakeup event...")
	}
}

func wakeupReadyLabel(cfg agent.Config) string {
	if cfg.TapWakeupEnabledOrDefault() {
		return wakeupGPIOPinsLabel()
	}
	return "tap wakeup disabled"
}

func runSingleTurnSTTWakeupMode(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, sigChan chan os.Signal, newWatcher wakeupWatcherFactory, reload ...<-chan struct{}) {
	if cfg.TapWakeupEnabledOrDefault() {
		logging.Infof("agent", "ready", "Starting GPIO wakeup listeners on %s...", wakeupGPIOPinsLabel())
	}

	events := make(chan voiceEvent, 1)

	watchers, err := startWakeupWatchersIfEnabled(cfg, newWatcher, func() {
		dialog.InterruptOutput()
		signalVoiceWakeupEvent(events)
	})
	if err != nil {
		logging.Errorf("agent", "daemon", "Failed to start GPIO wakeup listeners: %v", err)
		return
	}
	defer stopWakeupWatchers(watchers)

	logging.Infof("agent", "ready", "Waiting for wakeup event (%s)... Ctrl+C to quit", wakeupReadyLabel(cfg))

	var nextTurn *pendingVoiceTurn
	var pendingTurnContext agent.VoiceTurnContext
	pendingWakeup := false
	for {
		if reloadRequested(reload) {
			return
		}
		if nextTurn == nil {
			if !pendingWakeup {
				select {
				case <-reloadStop(reload):
					return
				case <-sigChan:
					if dialog != nil {
						dialog.StopRecording()
					}
					logging.Infof("agent", "exit", "Stopped.")
					return
				case <-events:
					logging.Infof("agent", "wakeup", "GPIO wakeup triggered, starting to listen...")
				}
			} else {
				pendingWakeup = false
			}

			utterance, exit := listenOneUtterance(dialog, sigChan, events, wakeupListenTimeout)
			if exit {
				logging.Infof("agent", "exit", "Stopped.")
				return
			}
			if len(utterance) == 0 {
				logging.Infof("agent", "listen", "no utterance captured after wakeup")
				logging.Infof("agent", "ready", "Waiting for next wakeup event...")
				pendingTurnContext = agent.VoiceTurnContext{}
				continue
			}
			input, err := dialog.PrepareTurnInput(utterance)
			if err != nil {
				logging.Errorf("agent", "daemon", "prepare turn input failed: %v", err)
				logging.Infof("agent", "ready", "Waiting for next wakeup event...")
				pendingTurnContext = agent.VoiceTurnContext{}
				continue
			}
			nextTurn = &pendingVoiceTurn{
				input:       input,
				utterance:   append([]int16(nil), utterance...),
				turnContext: pendingTurnContext,
			}
			pendingTurnContext = agent.VoiceTurnContext{}
		}

		turn := nextTurn
		nextTurn = nil
		result := runVoiceTurnWithInputContext(cfg, dialog, runtime, turn.input, turn.utterance, sigChan, events, turn.turnContext, true)
		if result.exit {
			logging.Infof("agent", "exit", "Stopped.")
			return
		}
		if result.nextTurn != nil {
			nextTurn = result.nextTurn
			logging.Infof("agent", "session", "turn interrupted, running captured steering input")
			continue
		}
		if result.interrupted {
			pendingWakeup = true
			pendingTurnContext = interruptedFollowUpContext(result)
			logging.Infof("agent", "session", "playback interrupted, starting a new wakeup turn")
			continue
		}
		logging.Infof("agent", "ready", "Waiting for next wakeup event...")
	}
}

func signalVoiceWakeupEvent(events chan<- voiceEvent) {
	select {
	case events <- voiceEventWakeup:
	default:
		logging.Warnf("agent", "wakeup", "pending wakeup already queued, coalescing duplicate GPIO wakeup event")
	}
}

func signalWakeupEvent(wakeupEvents chan<- struct{}) {
	select {
	case wakeupEvents <- struct{}{}:
	default:
		logging.Warnf("agent", "wakeup", "pending wakeup already queued, coalescing duplicate GPIO wakeup event")
	}
}

func runVoiceSession(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, sigChan chan os.Signal, events chan voiceEvent) bool {
	maxTurns := cfg.VoiceMaxTurns
	turns := 0
	firstTurn := true
	// Follow-up listen tip: instant fire-and-forget only after a normal reply.
	// Never after barge-in (user is already talking) and never on first turn
	// (StartRecording already cued). Do not wait for TTS idle before listening.
	playReadyCue := false
	var nextTurn *pendingVoiceTurn
	var pendingTurnContext agent.VoiceTurnContext

	// Classic final UX: keep the mic open for the whole session (Realtime-like shell).
	if err := dialog.StartRecording(); err != nil {
		logging.Errorf("agent", "daemon", "Failed to start session recording: %v", err)
		return false
	}
	defer func() {
		if err := dialog.StopRecording(); err != nil {
			logging.Warnf("agent", "session", "stop session recording: %v", err)
		}
	}()

	for {
		var input agent.TurnInput
		var utterance []int16
		var turnContext agent.VoiceTurnContext

		if nextTurn != nil {
			input = nextTurn.input
			utterance = nextTurn.utterance
			turnContext = nextTurn.turnContext
			nextTurn = nil
			playReadyCue = false
		} else {
			if maxTurns > 0 && turns >= maxTurns {
				logging.Infof("agent", "session", "max turns reached (%d), closing voice session")
				return false
			}

			timeout := cfg.VoiceFollowupTimeoutOrDefault()
			if firstTurn {
				timeout = cfg.VoiceFirstTurnTimeoutOrDefault()
			}

			var exit bool
			utterance, exit = listenOneUtteranceInSession(dialog, sigChan, events, timeout, playReadyCue)
			playReadyCue = false
			if exit {
				return true
			}
			if len(utterance) == 0 {
				logging.Infof("agent", "session", "listen timeout, closing voice session")
				return false
			}

			if err := dialog.FinalizeAndRotateSTT(); err != nil {
				logging.Warnf("agent", "stt", "finalize/rotate streaming STT: %v", err)
			}

			var err error
			input, err = dialog.PrepareTurnInput(utterance)
			if err != nil {
				logging.Errorf("agent", "daemon", "prepare turn input failed: %v", err)
				firstTurn = false
				dialog.ResetVAD()
				continue
			}
			turnContext = pendingTurnContext
			pendingTurnContext = agent.VoiceTurnContext{}
			// Clear Silero LSTM/state and drain any mic backlog queued while
			// STT finalize/rotate ran, so agent-send prompt echo cannot look
			// like a barge-in continuation.
			dialog.ResetVAD()
			for i := 0; i < 8; i++ {
				chunk, err := dialog.ReadRecordChunkMonitor(20)
				if err != nil || chunk == nil || len(chunk.PCM) == 0 {
					break
				}
			}
		}

		interruptOnWakeup := cfg.VoiceInterruptOnWakeupOrDefault()
		result := runVoiceTurnWithInputContext(cfg, dialog, runtime, input, utterance, sigChan, events, turnContext, interruptOnWakeup)
		if result.exit {
			return true
		}
		firstTurn = false
		if result.waitForWakeupRequested {
			logging.Infof("agent", "session", "agent requested wakeup wait, closing voice session")
			if result.requeueWakeup {
				logging.Infof("agent", "session", "GPIO tap during goodbye; requeueing wakeup for next session")
				signalVoiceWakeupEvent(events)
			}
			return false
		}
		if result.nextTurn != nil {
			nextTurn = result.nextTurn
			logging.Infof("agent", "session", "turn interrupted, running captured steering input")
			continue
		}
		if result.interrupted {
			pendingTurnContext = interruptedFollowUpContext(result)
			// Keep Silero buffers when barge-in already marked speaking so the
			// follow-up listen can finish the same utterance. No ready cue:
			// interrupt already stopped playback; user is mid-utterance.
			if !dialog.VADSpeaking() {
				dialog.ResetVAD()
			}
			playReadyCue = false
			logging.Infof("agent", "session", "turn interrupted, listening for follow-up")
			continue
		}
		// Do not wait for leftover TTS and do not play a follow-up ready cue:
		// barge-in covers mid-playback speech; a post-TTS drip made users wait
		// for the agent to finish before they felt allowed to talk.
		dialog.ResetVAD()
		playReadyCue = false
		turns++
	}
}

func runVoiceTurn(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, utterance []int16, sigChan chan os.Signal, events <-chan voiceEvent) voiceTurnResult {
	input, err := dialog.PrepareTurnInput(utterance)
	if err != nil {
		logging.Errorf("agent", "daemon", "prepare turn input failed: %v", err)
		return voiceTurnResult{}
	}

	return runVoiceTurnWithInput(cfg, dialog, runtime, input, utterance, sigChan, events)
}

func runVoiceTurnWithInput(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, input agent.TurnInput, utterance []int16, sigChan chan os.Signal, events <-chan voiceEvent) voiceTurnResult {
	return runVoiceTurnWithInputContext(cfg, dialog, runtime, input, utterance, sigChan, events, agent.VoiceTurnContext{}, cfg.VoiceInterruptOnWakeupOrDefault())
}

func runVoiceTurnWithInputContext(cfg agent.Config, dialog audioDialogRunner, runtime *agent.Runtime, input agent.TurnInput, utterance []int16, sigChan chan os.Signal, events <-chan voiceEvent, turnContext agent.VoiceTurnContext, interruptOnWakeup bool) voiceTurnResult {
	turnCtx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result agent.RunResult
		err    error
	}, 1)
	go func() {
		result, err := dialog.RunVoiceTurnWithContext(turnCtx, input, utterance, runtime, turnContext)
		resultCh <- struct {
			result agent.RunResult
			err    error
		}{result: result, err: err}
	}()

	var result agent.RunResult
	var turnErr error
	thinkFrameSamples := dialog.VADFrameSamples()
	thinkPending := make([]int16, 0, thinkFrameSamples*4)
	var thinkPendingByte bool
	var thinkOddByte byte
	thinkSpeechFrames := 0
	// Mode B: user speech must barge-in during thinking too. Arm after the
	// agent-send prompt (~1.1s) and require sustained speech so tip-tone echo
	// cannot cancel the turn in ~96ms. During TTS, require louder+longer
	// speech so speaker echo does not count but near-field talk still can.
	thinkBargeInArmedAt := time.Now().Add(1200 * time.Millisecond)
	ttsWasActive := dialog.TTSOutputActive()

thinking:
	for {
		select {
		case <-sigChan:
			cancel()
			_ = waitForTurnCancellation(dialog, resultCh)
			return voiceTurnResult{exit: true}
		case <-events:
			if interruptOnWakeup {
				logging.Infof("agent", "interrupt", "GPIO wakeup during active turn, immediately canceling")
				dialog.InterruptOutput()
				cancel()
				canceledInfo := waitForTurnCancellation(dialog, resultCh)
				return voiceTurnResultAfterCancel(input, canceledInfo, true)
			}
			logging.Warnf("agent", "interrupt", "wakeup received during thinking, ignoring because wakeup interrupt is disabled")
		case turnResult := <-resultCh:
			cancel()
			if turnResult.err != nil {
				logging.Errorf("agent", "daemon", "%v", turnResult.err)
				turnErr = turnResult.err
				result = turnResult.result
				break thinking
			}
			result = turnResult.result
			break thinking
		default:
			if !dialog.RecordingActive() || thinkFrameSamples <= 0 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			chunk, err := dialog.ReadRecordChunkMonitor(50)
			if err != nil || chunk == nil || len(chunk.PCM) == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			ttsActive := dialog.TTSOutputActive()
			if ttsActive != ttsWasActive {
				dialog.ResetVAD()
				thinkPending = thinkPending[:0]
				thinkPendingByte = false
				thinkSpeechFrames = 0
				if ttsActive {
					// Brief ignore for TTS start transient; keep short so the
					// user can force-stop playback almost immediately.
					thinkBargeInArmedAt = time.Now().Add(250 * time.Millisecond)
				} else {
					// Hold briefly after TTS ends so trailing echo cannot cancel.
					thinkBargeInArmedAt = time.Now().Add(500 * time.Millisecond)
				}
				ttsWasActive = ttsActive
			}
			if time.Now().Before(thinkBargeInArmedAt) {
				continue
			}
			agent.AppendPCM16Samples(chunk.PCM, &thinkPending, &thinkPendingByte, &thinkOddByte)
			for len(thinkPending) >= thinkFrameSamples {
				frame := append([]int16(nil), thinkPending[:thinkFrameSamples]...)
				thinkPending = thinkPending[thinkFrameSamples:]
				if countVoiceBargeInFrame(dialog, frame, ttsActive, &thinkSpeechFrames) {
					logging.Infof("agent", "interrupt", "voice barge-in during thinking, canceling turn")
					dialog.InterruptOutput()
					cancel()
					canceledInfo := waitForTurnCancellation(dialog, resultCh)
					return voiceTurnResultAfterCancel(input, canceledInfo, false)
				}
			}
		}
	}

	if turnErr == nil && result.WaitForWakeupRequested {
		return voiceTurnResult{waitForWakeupRequested: true}
	}

	speechText := result.SpokenTextForConfig(cfg)
	if result.SpeechStreamed {
		return voiceTurnResult{}
	}
	prepared := runtime.PrepareSpokenText(context.Background(), agent.SpokenTextInput{
		ResponseText:   speechText,
		TurnFailure:    result.TurnFailure,
		TailAppendable: turnErr == nil && !result.SpeechStreamed && dialog.CanSpeakFinalText(),
	})
	if prepared.Text == "" {
		return voiceTurnResult{}
	}

	// Keep the session mic open during TTS so Silero can barge-in (Realtime-like).
	dialog.ResetVAD()
	speakCtx, cancelSpeak := context.WithCancel(context.Background())
	speakCh := make(chan error, 1)
	go func() {
		speakCh <- dialog.SpeakFinal(speakCtx, prepared.Text, nil)
	}()

	frameSamples := dialog.VADFrameSamples()
	vadPending := make([]int16, 0, frameSamples*4)
	var speakPendingByte bool
	var speakOddByte byte
	bargeInSpeechFrames := 0
	bargeInArmedAt := time.Now().Add(250 * time.Millisecond)

speaking:
	for {
		select {
		case <-sigChan:
			cancelSpeak()
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, waitForSpeakCancel(speakCh))
			return voiceTurnResult{exit: true}
		case <-events:
			if interruptOnWakeup {
				logging.Infof("agent", "interrupt", "wakeup received during speaking, stopping playback")
				dialog.InterruptOutput()
				cancelSpeak()
				runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, waitForSpeakCancel(speakCh))
				return voiceTurnResult{
					interrupted:     true,
					followUpContext: buildInterruptedContextWithResult(input, result),
				}
			}
		case err := <-speakCh:
			cancelSpeak()
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
			if err != nil && !errors.Is(err, context.Canceled) {
				logging.Errorf("agent", "daemon", "speak failed: %v", err)
			}
			break speaking
		default:
			if !dialog.RecordingActive() || frameSamples <= 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			chunk, err := dialog.ReadRecordChunkMonitor(50)
			if err != nil || chunk == nil || len(chunk.PCM) == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Drain during the grace window so the record queue stays healthy,
			// but ignore Silero until TTS has been playing briefly.
			if time.Now().Before(bargeInArmedAt) {
				continue
			}
			agent.AppendPCM16Samples(chunk.PCM, &vadPending, &speakPendingByte, &speakOddByte)
			for len(vadPending) >= frameSamples {
				frame := append([]int16(nil), vadPending[:frameSamples]...)
				vadPending = vadPending[frameSamples:]
				if countVoiceBargeInFrame(dialog, frame, true, &bargeInSpeechFrames) {
					logging.Infof("agent", "interrupt", "voice barge-in during speaking, stopping playback")
					dialog.InterruptOutput()
					cancelSpeak()
					runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, waitForSpeakCancel(speakCh))
					return voiceTurnResult{
						interrupted:     true,
						followUpContext: buildInterruptedContextWithResult(input, result),
					}
				}
			}
		}
	}

	return voiceTurnResult{}
}

func interruptedFollowUpContext(result voiceTurnResult) agent.VoiceTurnContext {
	if result.followUpContext != (agent.VoiceTurnContext{}) {
		return result.followUpContext
	}
	return interruptedVoiceCorrectionContext()
}

func captureVoiceSteer(cfg agent.Config, dialog audioDialogRunner, sigChan chan os.Signal, events <-chan voiceEvent) (*pendingVoiceTurn, bool) {
	logging.Infof("agent", "steer", "wakeup received during thinking, listening for steering input")
	dialog.InterruptOutput()
	utterance, exit := listenOneUtterance(dialog, sigChan, events, voiceSteerListenTimeoutForConfig(cfg))
	if exit {
		return nil, true
	}
	if len(utterance) == 0 {
		logging.Infof("agent", "steer", "no steering utterance captured")
		return nil, false
	}
	input, err := dialog.PrepareTurnInput(utterance)
	if err != nil {
		logging.Errorf("agent", "steer", "prepare steer input failed: %v", err)
		return nil, false
	}
	return &pendingVoiceTurn{
		input:       input,
		utterance:   append([]int16(nil), utterance...),
		turnContext: interruptedVoiceCorrectionContext(),
	}, false
}

func voiceSteerListenTimeoutForConfig(_ agent.Config) time.Duration {
	return defaultVoiceSteerListenTimeout
}

func interruptedVoiceCorrectionContext() agent.VoiceTurnContext {
	return agent.VoiceTurnContext{
		RuntimeContext: voiceWakeupInterruptedCorrectionContext,
	}
}

// buildInterruptedContext constructs context for an interrupted turn during thinking/execution phase.
// It preserves the user's input and any partial execution history from the canceled turn.
func buildInterruptedContext(originalInput agent.TurnInput, canceledInfo canceledTurnInfo) agent.VoiceTurnContext {
	var contextParts []string

	// Base interruption message
	contextParts = append(contextParts, voiceWakeupInterruptedCorrectionContext)

	// Add the user's original input
	userInput := strings.TrimSpace(originalInput.InputText)
	if userInput == "" && originalInput.Transcript != "" {
		userInput = strings.TrimSpace(originalInput.Transcript)
	}
	if userInput != "" {
		contextParts = append(contextParts, fmt.Sprintf("\nThe interrupted turn's user input was: \"%s\"", userInput))
	}

	// Add episode information if available for history lookup
	if canceledInfo.episodeID != "" {
		contextParts = append(contextParts, fmt.Sprintf("\nInterrupted episode ID: %s (you can inspect this episode for partial execution history)", canceledInfo.episodeID))
	}

	// Add error information if the turn failed
	if canceledInfo.err != nil && !errors.Is(canceledInfo.err, context.Canceled) {
		contextParts = append(contextParts, fmt.Sprintf("\nThe interrupted turn encountered an error: %v", canceledInfo.err))
	}

	return agent.VoiceTurnContext{
		RuntimeContext: strings.Join(contextParts, ""),
	}
}

// buildInterruptedContextWithResult constructs context for an interrupted turn during speaking phase.
// At this point the turn has fully completed, so we have the complete result.
func buildInterruptedContextWithResult(originalInput agent.TurnInput, result agent.RunResult) agent.VoiceTurnContext {
	var contextParts []string

	// Base interruption message
	contextParts = append(contextParts, voiceWakeupInterruptedCorrectionContext)

	// Add the user's original input
	userInput := strings.TrimSpace(originalInput.InputText)
	if userInput == "" && originalInput.Transcript != "" {
		userInput = strings.TrimSpace(originalInput.Transcript)
	}
	if userInput != "" {
		contextParts = append(contextParts, fmt.Sprintf("\nThe interrupted turn's user input was: \"%s\"", userInput))
	}

	// Add the agent's output that was interrupted during playback
	agentOutput := strings.TrimSpace(result.Output)
	if agentOutput != "" {
		contextParts = append(contextParts, fmt.Sprintf("\nThe agent's response (interrupted during playback) was: \"%s\"", agentOutput))
	}

	// Add episode information for full history
	if result.EpisodeID != "" {
		contextParts = append(contextParts, fmt.Sprintf("\nCompleted episode ID: %s (contains full execution history)", result.EpisodeID))
	}

	return agent.VoiceTurnContext{
		RuntimeContext: strings.Join(contextParts, ""),
	}
}

type canceledTurnInfo struct {
	result    agent.RunResult
	err       error
	episodeID string
}

// voiceTurnResultAfterCancel prefers an already-requested wait_for_wakeup over
// treating the cancel as a follow-up interrupt, except voice barge-in after
// goodbye should keep the session open for the new utterance. A GPIO tap during
// goodbye both closes and requeues wakeup so one press starts the next session.
func voiceTurnResultAfterCancel(input agent.TurnInput, canceledInfo canceledTurnInfo, fromGPIO bool) voiceTurnResult {
	if canceledInfo.err == nil && canceledInfo.result.WaitForWakeupRequested {
		if fromGPIO {
			logging.Infof("agent", "session", "GPIO tap during wait_for_wakeup goodbye, closing and requeueing wakeup")
			return voiceTurnResult{waitForWakeupRequested: true, requeueWakeup: true}
		}
		logging.Infof("agent", "session", "voice barge-in during wait_for_wakeup goodbye, staying in session for follow-up")
		return voiceTurnResult{
			interrupted:     true,
			followUpContext: buildInterruptedContext(input, canceledInfo),
		}
	}
	return voiceTurnResult{
		interrupted:     true,
		followUpContext: buildInterruptedContext(input, canceledInfo),
	}
}

// countVoiceBargeInFrame updates consecutive speech frames and reports whether
// barge-in should fire. During TTS playback use a slightly longer sustain so
// tip/echo blips are ignored; still interrupt on real near-field talk.
func countVoiceBargeInFrame(dialog audioDialogRunner, frame []int16, ttsActive bool, speechFrames *int) bool {
	_, _ = dialog.ProcessVADFrame(frame)
	need := thinkBargeInNeed
	if ttsActive {
		need = ttsBargeInNeedFrames
	}
	if dialog.VADSpeaking() {
		*speechFrames++
	} else {
		*speechFrames = 0
	}
	return *speechFrames >= need
}

func waitForTurnCancellation(dialog audioDialogRunner, resultCh <-chan struct {
	result agent.RunResult
	err    error
}) canceledTurnInfo {
	select {
	case turnResult := <-resultCh:
		return canceledTurnInfo{
			result:    turnResult.result,
			err:       turnResult.err,
			episodeID: turnResult.result.EpisodeID,
		}
	case <-time.After(voiceTurnCancelWaitTimeout):
		logging.Warnf("agent", "interrupt", "current turn did not finish after signal cancellation; waiting for voice run to go idle")
		idleCtx, cancel := context.WithTimeout(context.Background(), voiceTurnCancelWaitTimeout)
		defer cancel()
		if dialog.WaitForVoiceRunIdle(idleCtx) {
			return canceledTurnInfo{}
		}
		logging.Warnf("agent", "interrupt", "voice run still active after cancellation wait; forcing reset")
		dialog.ForceResetVoiceRun()
		return canceledTurnInfo{}
	}
}

func waitForSpeakCancel(speakCh <-chan error) error {
	select {
	case err := <-speakCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logging.Errorf("agent", "daemon", "speak failed after signal cancellation: %v", err)
		}
		return err
	case <-time.After(voiceTurnCancelWaitTimeout):
		logging.Warnf("agent", "interrupt", "speech did not finish after signal cancellation; exiting")
		return context.Canceled
	}
}

func listenOneUtterance(dialog audioDialogRunner, sigChan chan os.Signal, events <-chan voiceEvent, listenTimeout time.Duration) ([]int16, bool) {
	logging.Infof("agent", "listen", "Recording audio...")
	if err := dialog.StartRecording(); err != nil {
		logging.Errorf("agent", "daemon", "Failed to start recording: %v", err)
		return nil, false
	}
	defer func() {
		if err := dialog.StopRecording(); err != nil {
			logging.Warnf("agent", "listen", "stop recording after listen: %v", err)
		}
	}()
	dialog.ResetVAD()
	utterance, exit := captureUtteranceWithTimeout(dialog, sigChan, events, listenTimeout)
	if !exit {
		drainWakeupsWhileListening(events)
	}
	return utterance, exit
}

// waitForDialogTTSIdle drains the mic until Speak/SpeakFinal outputs finish.
// GPIO and loud near-field speech can barge-in; soft speaker echo should not.
func waitForDialogTTSIdle(dialog audioDialogRunner, sigChan chan os.Signal, events <-chan voiceEvent, interruptOnWakeup bool) voiceTurnResult {
	if dialog == nil || !dialog.TTSOutputActive() {
		return voiceTurnResult{}
	}
	logging.Infof("agent", "session", "waiting for streaming TTS to finish before follow-up listen")
	frameSamples := dialog.VADFrameSamples()
	pending := make([]int16, 0, frameSamples*4)
	var pendingByte bool
	var oddByte byte
	speechFrames := 0
	armedAt := time.Now().Add(500 * time.Millisecond)
	for dialog.TTSOutputActive() {
		select {
		case <-sigChan:
			dialog.InterruptOutput()
			return voiceTurnResult{exit: true}
		case <-events:
			if interruptOnWakeup {
				logging.Infof("agent", "interrupt", "GPIO wakeup while waiting for TTS idle, interrupting playback")
				dialog.InterruptOutput()
				return voiceTurnResult{interrupted: true}
			}
		default:
			if !dialog.RecordingActive() || frameSamples <= 0 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			chunk, err := dialog.ReadRecordChunkMonitor(50)
			if err != nil || chunk == nil || len(chunk.PCM) == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if time.Now().Before(armedAt) {
				continue
			}
			agent.AppendPCM16Samples(chunk.PCM, &pending, &pendingByte, &oddByte)
			for len(pending) >= frameSamples {
				frame := append([]int16(nil), pending[:frameSamples]...)
				pending = pending[frameSamples:]
				if countVoiceBargeInFrame(dialog, frame, true, &speechFrames) {
					logging.Infof("agent", "interrupt", "voice barge-in while waiting for TTS idle, interrupting playback")
					dialog.InterruptOutput()
					return voiceTurnResult{interrupted: true}
				}
			}
		}
	}
	// Brief settle so room echo does not look like the start of follow-up speech.
	settleUntil := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(settleUntil) {
		select {
		case <-sigChan:
			return voiceTurnResult{exit: true}
		case <-events:
			if interruptOnWakeup {
				return voiceTurnResult{interrupted: true}
			}
		default:
			if dialog.RecordingActive() {
				_, _ = dialog.ReadRecordChunkMonitor(50)
			} else {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	return voiceTurnResult{}
}

// listenOneUtteranceInSession captures one utterance while the session mic stays open.
// playReadyCue should be true only for follow-up turns; the first turn already
// gets StartRecording's tip and must not double-beep on wakeup.
func listenOneUtteranceInSession(dialog audioDialogRunner, sigChan chan os.Signal, events <-chan voiceEvent, listenTimeout time.Duration, playReadyCue bool) ([]int16, bool) {
	logging.Infof("agent", "listen", "Recording audio...")
	if !dialog.RecordingActive() {
		if err := dialog.StartRecording(); err != nil {
			logging.Errorf("agent", "daemon", "Failed to start recording: %v", err)
			return nil, false
		}
	}
	// Barge-in may already have Silero in the speaking state with buffered audio.
	if !dialog.VADSpeaking() {
		dialog.ResetVAD()
		// Follow-up turns keep the mic open, so StartRecording's tip never
		// replays. Cue "you can speak" here once TTS is idle; skip when
		// barge-in already has speech in progress so we do not beep over the user.
		if playReadyCue && !dialog.TTSOutputActive() {
			dialog.PlayListenReadyCue()
		}
	}
	utterance, exit := captureUtteranceWithTimeout(dialog, sigChan, events, listenTimeout)
	if !exit {
		drainWakeupsWhileListening(events)
	}
	return utterance, exit
}

func drainWakeupsWhileListening(events <-chan voiceEvent) {
	for {
		select {
		case <-events:
			ignoreWakeupWhileListening()
		default:
			return
		}
	}
}

func ignoreWakeupWhileListening() {
	logging.Warnf("agent", "listen", "duplicate wakeup received while listening, ignoring")
}

func captureUtteranceWithTimeout(dialog audioDialogRunner, sigChan chan os.Signal, events <-chan voiceEvent, listenTimeout time.Duration) ([]int16, bool) {
	frameSamples := dialog.VADFrameSamples()
	if frameSamples <= 0 {
		logging.Errorf("agent", "listen", "invalid VAD frame size: %d", frameSamples)
		return nil, false
	}

	vadPending := make([]int16, 0, frameSamples*10)
	captured := make([]int16, 0, frameSamples*100)
	var hasPendingByte bool
	var pendingByte byte
	startedAt := time.Now()
	nextVADLogAt := startedAt.Add(time.Second)
	hasVADState := false
	speechDetected := false

	for {
		select {
		case <-sigChan:
			return nil, true
		case <-events:
			if speechDetected && len(captured) > 0 {
				logging.Infof("agent", "listen", "wakeup pressed during recording, returning captured audio")
				utterance := dialog.FinishPendingUtterance(vadPending)
				if len(utterance) > 0 {
					return utterance, false
				}
				return captured, false
			}
			ignoreWakeupWhileListening()
			continue
		default:
		}

		if listenTimeout > 0 && time.Since(startedAt) >= listenTimeout {
			logging.Infof("agent", "listen", "No complete utterance within %s", listenTimeout)
			if len(captured) > 0 && (!hasVADState || speechDetected) {
				// Prefer a VAD-flushed utterance over dumping the entire
				// listen window into STT (15s blobs time out / hang the turn).
				if utterance := dialog.FinishPendingUtterance(vadPending); len(utterance) > 0 {
					logging.Infof("agent", "listen", "Flushing %d samples after VAD timeout", len(utterance))
					return utterance, false
				}
				const maxTimeoutSamples = 16000 * 6 // 6s @ 16kHz
				if len(captured) > maxTimeoutSamples {
					captured = captured[len(captured)-maxTimeoutSamples:]
				}
				logging.Infof("agent", "listen", "Returning %d buffered samples after VAD timeout", len(captured))
				return captured, false
			}
			if len(captured) > 0 {
				logging.Infof("agent", "listen", "Discarding %d buffered samples without detected speech", len(captured))
			}
			return nil, false
		}

		chunk, err := dialog.ReadRecordChunk(200)
		if err != nil {
			logging.Errorf("agent", "listen", "read_record_chunk error: %v", err)
			return nil, false
		}

		select {
		case <-sigChan:
			return nil, true
		case <-events:
			if speechDetected && len(captured) > 0 {
				logging.Infof("agent", "listen", "wakeup pressed during recording, returning captured audio")
				utterance := dialog.FinishPendingUtterance(vadPending)
				if len(utterance) > 0 {
					return utterance, false
				}
				return captured, false
			}
			ignoreWakeupWhileListening()
		default:
		}

		if chunk == nil {
			continue
		}

		if chunk.EndOfStream {
			logging.Infof("agent", "listen", "record session closed by service")
			return nil, false
		}

		if len(chunk.PCM) == 0 {
			continue
		}

		pendingStart := len(vadPending)
		agent.AppendPCM16Samples(chunk.PCM, &vadPending, &hasPendingByte, &pendingByte)
		if len(vadPending) > pendingStart {
			captured = append(captured, vadPending[pendingStart:]...)
		}

		consumed := 0
		for consumed+frameSamples <= len(vadPending) {
			utterance, err := dialog.ProcessVADFrame(vadPending[consumed : consumed+frameSamples])
			consumed += frameSamples
			if err != nil {
				// Recover: Silero helper blips must not close the whole session.
				logging.Errorf("agent", "vad", "RKNN processing failed, resetting VAD and continuing: %v", err)
				dialog.ResetVAD()
				vadPending = vadPending[:0]
				hasPendingByte = false
				pendingByte = 0
				break
			}
			if reporter, ok := dialog.(vadDebugReporter); ok {
				state := reporter.VADDebugState()
				hasVADState = true
				if state.Speaking || state.SpeechFrames > 0 {
					speechDetected = true
				}
				if time.Now().After(nextVADLogAt) {
					logging.Debugf("agent", "daemon", "%s", formatVADDebugLog(state))
					nextVADLogAt = time.Now().Add(time.Second)
				}
			}
			if utterance != nil {
				select {
				case <-sigChan:
					return nil, true
				case <-events:
					ignoreWakeupWhileListening()
				default:
				}
				logging.Infof("agent", "utterance", "VAD detected end of speech")
				return utterance, false
			}
		}

		if consumed > 0 {
			vadPending = vadPending[consumed:]
		}
	}
}

func processAudioUntilUtterance(dialog audioDialogRunner, runtime *agent.Runtime, ctx context.Context, sigChan chan os.Signal) bool {
	return processAudioUntilUtteranceWithTimeout(dialog, runtime, ctx, sigChan, wakeupListenTimeout)
}

func processAudioUntilUtteranceWithTimeout(dialog audioDialogRunner, runtime *agent.Runtime, ctx context.Context, sigChan chan os.Signal, listenTimeout time.Duration) bool {
	// Wakeup interruption is disabled when wakeupEvents is nil, so interrupted is always false here.
	exit, _ := processAudioUntilUtteranceWithWakeupInterrupt(dialog, runtime, ctx, sigChan, nil, false, listenTimeout)
	return exit
}

func processAudioUntilUtteranceWithWakeupInterrupt(
	dialog audioDialogRunner,
	runtime *agent.Runtime,
	ctx context.Context,
	sigChan chan os.Signal,
	wakeupEvents <-chan struct{},
	interruptOnWakeup bool,
	listenTimeout time.Duration,
) (bool, bool) {
	frameSamples := dialog.VADFrameSamples()
	if frameSamples <= 0 {
		logging.Errorf("agent", "listen", "invalid VAD frame size: %d", frameSamples)
		return false, false
	}

	vadPending := make([]int16, 0, frameSamples*10)
	captured := make([]int16, 0, frameSamples*100)
	var hasPendingByte bool
	var pendingByte byte
	startedAt := time.Now()
	nextVADLogAt := startedAt.Add(time.Second)

	for {
		// Read audio chunk
		select {
		case <-sigChan:
			return true, false
		default:
		}
		drainWakeupEventsWhileListening(wakeupEvents)

		if listenTimeout > 0 && time.Since(startedAt) >= listenTimeout {
			logging.Infof("agent", "listen", "No complete utterance within %s, stopping recording", listenTimeout)
			if len(captured) > 0 {
				logging.Infof("agent", "listen", "Sending %d buffered samples after VAD timeout", len(captured))
				if err := dialog.StopRecording(); err != nil {
					logging.Warnf("agent", "listen", "stop recording before timeout utterance: %v", err)
				}
				exit, interrupted := processUtteranceWithWakeupInterrupt(dialog, runtime, ctx, captured, sigChan, wakeupEvents, interruptOnWakeup)
				if exit || interrupted {
					return exit, interrupted
				}
			}
			return false, false
		}

		chunk, err := dialog.ReadRecordChunk(200)
		if err != nil {
			logging.Errorf("agent", "listen", "read_record_chunk error: %v", err)
			return false, false
		}

		if chunk == nil {
			continue
		}

		if chunk.EndOfStream {
			logging.Infof("agent", "listen", "record session closed by service")
			return false, false
		}

		if len(chunk.PCM) == 0 {
			continue
		}

		// Convert PCM bytes to int16 samples
		pendingStart := len(vadPending)
		agent.AppendPCM16Samples(chunk.PCM, &vadPending, &hasPendingByte, &pendingByte)
		if len(vadPending) > pendingStart {
			captured = append(captured, vadPending[pendingStart:]...)
		}

		// Feed VAD in 32ms Silero frames.
		consumed := 0
		for consumed+frameSamples <= len(vadPending) {
			utterance, err := dialog.ProcessVADFrame(vadPending[consumed : consumed+frameSamples])
			consumed += frameSamples
			if err != nil {
				logging.Errorf("agent", "vad", "RKNN processing failed: %v", err)
				return false, false
			}
			if reporter, ok := dialog.(vadDebugReporter); ok && time.Now().After(nextVADLogAt) {
				state := reporter.VADDebugState()
				logging.Debugf("agent", "daemon", "%s", formatVADDebugLog(state))
				nextVADLogAt = time.Now().Add(time.Second)
			}
			if utterance != nil {
				logging.Infof("agent", "utterance", "VAD detected end of speech")
				if err := dialog.StopRecording(); err != nil {
					logging.Warnf("agent", "listen", "stop recording before utterance processing: %v", err)
				}
				return processUtteranceWithWakeupInterrupt(dialog, runtime, ctx, utterance, sigChan, wakeupEvents, interruptOnWakeup)
			}
		}

		if consumed > 0 {
			vadPending = vadPending[consumed:]
		}
	}
}

func drainWakeupEventsWhileListening(wakeupEvents <-chan struct{}) {
	if wakeupEvents == nil {
		return
	}
	drainedCount := 0
	for {
		select {
		case <-wakeupEvents:
			drainedCount++
		default:
			if drainedCount > 0 {
				logging.Warnf("agent", "listen", "%d duplicate wakeup(s) received while listening, ignoring", drainedCount)
			}
			return
		}
	}
}

func processUtteranceWithWakeupInterrupt(
	dialog audioDialogRunner,
	runtime *agent.Runtime,
	ctx context.Context,
	utterance []int16,
	sigChan chan os.Signal,
	wakeupEvents <-chan struct{},
	interruptOnWakeup bool,
) (bool, bool) {
	if wakeupEvents == nil {
		if err := dialog.ProcessUtterance(ctx, utterance, runtime); err != nil {
			logging.Errorf("agent", "daemon", "%v", err)
		}
		return false, false
	}

	turnCtx, cancel := context.WithCancel(ctx)
	resultCh := make(chan error, 1)
	go func() {
		resultCh <- dialog.ProcessUtterance(turnCtx, utterance, runtime)
	}()

	for {
		select {
		case <-sigChan:
			cancel()
			waitForLegacyUtteranceCancel(resultCh)
			return true, false
		case <-wakeupEvents:
			if interruptOnWakeup {
				logging.Infof("agent", "interrupt", "wakeup received during legacy turn, canceling current turn")
				cancel()
				waitForLegacyUtteranceCancel(resultCh)
				return false, true
			}
			logging.Warnf("agent", "interrupt", "wakeup received during legacy turn, ignoring because interrupt is disabled")
		case err := <-resultCh:
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				logging.Errorf("agent", "daemon", "%v", err)
			}
			return false, false
		}
	}
}

func waitForLegacyUtteranceCancel(resultCh <-chan error) {
	select {
	case err := <-resultCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logging.Errorf("agent", "daemon", "%v", err)
		}
	case <-time.After(voiceTurnCancelWaitTimeout):
		logging.Warnf("agent", "interrupt", "legacy turn did not finish after cancellation; continuing")
	}
}

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/realtimevoice"
	"aico-agent/internal/agent/tts"
	"aico-agent/internal/agenttask"
	"aico-agent/internal/logging"
	streamcontract "aico-agent/internal/stream"

	"github.com/google/uuid"
	langtools "github.com/tmc/langchaingo/tools"
)

const (
	realtimeAudioReadTimeoutMs        = 200
	realtimeConnectTimeout            = 15 * time.Second
	realtimeNotificationPoll          = 500 * time.Millisecond
	realtimeNotificationDrain         = 30 * time.Second
	realtimeSleepDrain                = 30 * time.Second
	realtimePlaybackKeepAlive         = 10 * time.Second
	realtimeToolCallTimeout           = 30 * time.Second
	realtimeTaskResultDebounce        = 500 * time.Millisecond
	realtimeContextReplayTurns        = 10
	realtimeClientTurnEndpointSilence = 800 * time.Millisecond
	realtimeClientTurnSpeechThreshold = 500
	maxRetiredRealtimeResponseIDs     = 64
)

var errRealtimeShutdown = errors.New("realtime shutdown requested")

type realtimeProviderFailure struct {
	err error
}

func (e *realtimeProviderFailure) Error() string { return e.err.Error() }
func (e *realtimeProviderFailure) Unwrap() error { return e.err }

func markRealtimeProviderFailure(err error) error {
	if err == nil {
		return nil
	}
	return &realtimeProviderFailure{err: err}
}

func shouldAnnounceRealtimeSessionFailure(err error) bool {
	var providerFailure *realtimeProviderFailure
	return errors.As(err, &providerFailure)
}

type realtimeChatCommand struct {
	ctx     context.Context
	request agent.RealtimeChatRequest
	events  chan agent.RealtimeChatEvent
}

type realtimeToolResult struct {
	call   realtimevoice.Event
	output string
}

// realtimeSemanticEmitter is the observer-side adapter for native realtime
// events. The daemon remains the owner of stale-response admission and output
// invalidation; this adapter only mirrors accepted events into the shared
// semantic contract. PCM stays on the playback/audio path.
type realtimeSemanticEmitter struct {
	sink           streamcontract.EventSink
	sequence       uint64
	outputEpoch    uint64
	responseEpoch  map[string]uint64
	conversationID string
	sessionEpoch   uint64
}

func newRealtimeSemanticEmitter(sink streamcontract.EventSink) *realtimeSemanticEmitter {
	if sink == nil {
		return nil
	}
	return &realtimeSemanticEmitter{
		sink:           sink,
		outputEpoch:    1,
		responseEpoch:  make(map[string]uint64),
		conversationID: "agent",
		sessionEpoch:   1,
	}
}

func (e *realtimeSemanticEmitter) nextResponseEpoch() {
	if e == nil {
		return
	}
	e.outputEpoch++
	if e.outputEpoch == 0 {
		e.outputEpoch = 1
	}
}

func (e *realtimeSemanticEmitter) invalidateOutput() {
	e.nextResponseEpoch()
}

func (e *realtimeSemanticEmitter) beginResponse(responseID string) {
	if e == nil {
		return
	}
	if responseID != "" {
		e.responseEpoch[responseID] = e.outputEpoch
	}
}

func (e *realtimeSemanticEmitter) epochFor(source realtimevoice.Event) uint64 {
	if e == nil {
		return 0
	}
	switch source.Kind {
	case realtimevoice.EventTranscriptDelta, realtimevoice.EventTranscriptFinal,
		realtimevoice.EventToolCall, realtimevoice.EventToolCallCancelled,
		realtimevoice.EventResponseStarted:
		if source.Role != "assistant" && source.Kind != realtimevoice.EventToolCall && source.Kind != realtimevoice.EventToolCallCancelled && source.Kind != realtimevoice.EventResponseStarted {
			return 0
		}
		if source.ResponseID != "" {
			if epoch, ok := e.responseEpoch[source.ResponseID]; ok {
				return epoch
			}
		}
		return e.outputEpoch
	default:
		return 0
	}
}

func (e *realtimeSemanticEmitter) emit(source realtimevoice.Event, outputEpoch ...uint64) {
	if e == nil || e.sink == nil {
		return
	}
	nextSequence := e.sequence + 1
	if nextSequence == 0 {
		nextSequence = 1
	}
	epoch := uint64(0)
	if len(outputEpoch) > 0 {
		epoch = outputEpoch[0]
	} else {
		epoch = e.epochFor(source)
	}
	identity := streamcontract.EventIdentity{
		EventID:        uuid.NewString(),
		Sequence:       nextSequence,
		ConversationID: e.conversationID,
		SessionEpoch:   e.sessionEpoch,
		ResponseID:     source.ResponseID,
		ItemID:         source.ItemID,
		OutputEpoch:    epoch,
		CallID:         source.CallID,
	}
	if bridged, ok := realtimevoice.BridgeEvent(identity, e.outputEpoch, source); ok {
		e.sequence = nextSequence
		_ = e.sink.Emit(context.Background(), bridged)
	}
}

func (e *realtimeSemanticEmitter) emitError(err error) {
	if e == nil || err == nil {
		return
	}
	e.emit(realtimevoice.Event{Kind: realtimevoice.EventError, Error: err})
}

type realtimeToolTracker struct {
	pending      map[string]int
	seen         map[string]bool
	responseDone map[string]bool
}

func newRealtimeToolTracker() *realtimeToolTracker {
	return &realtimeToolTracker{
		pending:      make(map[string]int),
		seen:         make(map[string]bool),
		responseDone: make(map[string]bool),
	}
}

func (t *realtimeToolTracker) start(responseID string) {
	t.seen[responseID] = true
	t.pending[responseID]++
}

// complete returns true when response.done was already received and this was
// the final pending tool result, so the caller should create the continuation.
func (t *realtimeToolTracker) complete(responseID string) bool {
	if t.pending[responseID] > 0 {
		t.pending[responseID]--
	}
	if t.pending[responseID] != 0 || !t.responseDone[responseID] {
		return false
	}
	t.clear(responseID)
	return true
}

// done returns true when a tool-bearing response can continue immediately.
// A false return means either no tool was seen or pending tool work remains.
func (t *realtimeToolTracker) done(responseID string) (hasTools bool, continueNow bool) {
	if !t.seen[responseID] {
		return false, false
	}
	if t.pending[responseID] > 0 {
		t.responseDone[responseID] = true
		return true, false
	}
	t.clear(responseID)
	return true, true
}

func (t *realtimeToolTracker) clear(responseID string) {
	if responseID == "" {
		clear(t.pending)
		clear(t.seen)
		clear(t.responseDone)
		return
	}
	delete(t.pending, responseID)
	delete(t.seen, responseID)
	delete(t.responseDone, responseID)
}

// realtimeSleepState carries an end_conversation request through the farewell
// response and its playback drain. Standby is deferred until the goodbye has
// actually been spoken, and abandoned entirely if the user re-engages first.
type realtimeSleepState struct {
	requested bool
	drain     <-chan error
	cancel    context.CancelFunc
}

func (s *realtimeSleepState) request() { s.requested = true }

func (s *realtimeSleepState) pending() bool { return s.requested }

func (s *realtimeSleepState) shouldDrain() bool { return s.requested && s.drain == nil }

func (s *realtimeSleepState) startDrain(drain <-chan error, cancel context.CancelFunc) {
	s.drain = drain
	s.cancel = cancel
}

func (s *realtimeSleepState) draining() <-chan error { return s.drain }

func (s *realtimeSleepState) abandon() bool {
	if !s.requested {
		return false
	}
	s.requested = false
	s.drain = nil
	s.stopDrain()
	return true
}

func (s *realtimeSleepState) stopDrain() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

type realtimeChatAdmission uint8

const (
	realtimeChatStart realtimeChatAdmission = iota
	realtimeChatQueueAfterResponse
	realtimeChatRejectBusy
)

type realtimeChatAdmissionState struct {
	activeChat           bool
	queuedChat           bool
	responseActive       bool
	turnBlocked          bool
	localSpeechActive    bool
	notificationActive   bool
	notificationDraining bool
	standbyPending       bool
}

func (s realtimeChatAdmissionState) admission() realtimeChatAdmission {
	if s.activeChat || s.queuedChat || s.notificationActive || s.notificationDraining {
		return realtimeChatRejectBusy
	}
	if s.localSpeechActive && !s.responseActive && !s.turnBlocked {
		return realtimeChatQueueAfterResponse
	}
	if s.standbyPending {
		if s.responseActive {
			return realtimeChatQueueAfterResponse
		}
		if !s.turnBlocked {
			return realtimeChatStart
		}
	}
	if s.turnBlocked {
		return realtimeChatRejectBusy
	}
	return realtimeChatStart
}

type realtimeChatBridge struct {
	commands chan realtimeChatCommand
	wakeup   func()

	mu     sync.RWMutex
	active bool
	done   chan struct{}
}

func newRealtimeChatBridge(wakeups ...func()) *realtimeChatBridge {
	bridge := &realtimeChatBridge{commands: make(chan realtimeChatCommand, 8)}
	if len(wakeups) > 0 {
		bridge.wakeup = wakeups[0]
	}
	return bridge
}

func (b *realtimeChatBridge) Handle(ctx context.Context, request agent.RealtimeChatRequest) (<-chan agent.RealtimeChatEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	events := make(chan agent.RealtimeChatEvent, 16)
	b.mu.RLock()
	active, done := b.active, b.done
	b.mu.RUnlock()
	command := realtimeChatCommand{ctx: ctx, request: request, events: events}
	var sessionDone <-chan struct{}
	if active {
		sessionDone = done
	}
	select {
	case b.commands <- command:
		if !active && b.wakeup != nil {
			b.wakeup()
		}
		return events, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sessionDone:
		return nil, fmt.Errorf("realtime voice session is unavailable")
	}
}

func (b *realtimeChatBridge) activate() {
	b.mu.Lock()
	b.active = true
	b.done = make(chan struct{})
	b.mu.Unlock()
}

func (b *realtimeChatBridge) deactivate() {
	b.mu.Lock()
	if b.active && b.done != nil {
		close(b.done)
	}
	b.active = false
	b.done = nil
	b.mu.Unlock()
}

func (b *realtimeChatBridge) failQueued(message string) {
	for {
		select {
		case command := <-b.commands:
			sendRealtimeChatEvent(command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventError, Error: message})
			close(command.events)
		default:
			return
		}
	}
}

func sendRealtimeChatEvent(command realtimeChatCommand, event agent.RealtimeChatEvent) bool {
	select {
	case command.events <- event:
		return true
	case <-command.ctx.Done():
		return false
	}
}

func chatBridgeCommands(bridge *realtimeChatBridge) <-chan realtimeChatCommand {
	if bridge == nil {
		return nil
	}
	return bridge.commands
}

func chatCommandDone(command *realtimeChatCommand) <-chan struct{} {
	if command == nil || command.ctx == nil {
		return nil
	}
	return command.ctx.Done()
}

// relayRealtimeSessionEvents marks user re-engagement before forwarding its
// event, so farewell drain completion cannot win a select race and close the
// session while speech is already waiting to be handled.
func relayRealtimeSessionEvents(ctx context.Context, source <-chan realtimevoice.Event) (<-chan realtimevoice.Event, <-chan struct{}) {
	events := make(chan realtimevoice.Event, 16)
	reengagement := make(chan struct{}, 1)
	go func() {
		defer close(events)
		// Track source ordering before the event loop consumes it, so a delayed
		// transcript refining an active response cannot win the farewell drain.
		providerResponseActive := false
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-source:
				if !ok {
					return
				}
				switch event.Kind {
				case realtimevoice.EventResponseStarted:
					providerResponseActive = true
				case realtimevoice.EventResponseDone, realtimevoice.EventResponseCancelled:
					providerResponseActive = false
				}
				if event.Kind == realtimevoice.EventSpeechStarted || event.Kind == realtimevoice.EventInterruption ||
					(!providerResponseActive && (event.Kind == realtimevoice.EventTranscriptDelta || event.Kind == realtimevoice.EventTranscriptFinal) && event.Role == "user") {
					select {
					case reengagement <- struct{}{}:
					default:
					}
				}
				select {
				case events <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return events, reengagement
}

func consumeRealtimeReengagement(reengagement <-chan struct{}) bool {
	select {
	case <-reengagement:
		return true
	default:
		return false
	}
}

func abandonSleepForPendingRealtimeReengagement(sleep *realtimeSleepState, reengagement <-chan struct{}) bool {
	if !consumeRealtimeReengagement(reengagement) {
		return false
	}
	return sleep.abandon()
}

func runRealtimeWakeupMode(cfg agent.Config, sigChan chan os.Signal, newWatcher voiceWakeupWatcherFactory) {
	runRealtimeWakeupModeWithServer(cfg, sigChan, nil, nil, nil, newWatcher)
}

// runRealtimeWakeupModeWithServer owns the realtime voice path. The legacy wakeup
// runners remain separate so they can be restored without changing this path.
func runRealtimeWakeupModeWithServer(cfg agent.Config, sigChan chan os.Signal, server *agent.Server, runtime *agent.Runtime, tasks *agenttask.Manager, newWatcher voiceWakeupWatcherFactory, reload ...<-chan struct{}) {
	events := make(chan struct{}, 1)
	bridge := newRealtimeChatBridge(func() {
		signalWakeupEvent(events)
	})
	voiceNotificationTicker := time.NewTicker(realtimeNotificationPoll)
	defer voiceNotificationTicker.Stop()
	var notificationFallbackCancel context.CancelFunc
	var notificationFallbackDone chan struct{}
	var failureAnnouncementCancel context.CancelFunc
	var failureAnnouncementDone chan struct{}
	failureAnnouncementClipPlayed := false
	pendingActivation := false
	// The prerecorded clip only announces that speech is unavailable; it does
	// not carry the notification text, so the notification stays pending on
	// purpose. Latch it to once per standby period, otherwise the notification
	// poll replays the same clip every realtimeNotificationPoll.
	clipPlayed := false
	clipLatched := false
	collectNotificationFallback := func() {
		if notificationFallbackDone == nil {
			return
		}
		<-notificationFallbackDone
		clipLatched = clipLatched || clipPlayed
		notificationFallbackCancel = nil
		notificationFallbackDone = nil
	}
	stopNotificationFallback := func() {
		if notificationFallbackCancel != nil {
			notificationFallbackCancel()
		}
		collectNotificationFallback()
	}
	defer stopNotificationFallback()
	collectFailureAnnouncement := func() {
		if failureAnnouncementDone == nil {
			return
		}
		<-failureAnnouncementDone
		clipLatched = clipLatched || failureAnnouncementClipPlayed
		failureAnnouncementCancel = nil
		failureAnnouncementDone = nil
	}
	stopFailureAnnouncement := func() {
		if failureAnnouncementCancel != nil {
			failureAnnouncementCancel()
		}
		collectFailureAnnouncement()
	}
	defer stopFailureAnnouncement()
	startFailureAnnouncement := func(sessionErr error) {
		if server == nil || !shouldAnnounceRealtimeSessionFailure(sessionErr) || failureAnnouncementDone != nil {
			return
		}
		failureCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		failureAnnouncementCancel = cancel
		failureAnnouncementDone = done
		failureAnnouncementClipPlayed = false
		go func() {
			defer close(done)
			failureAnnouncementClipPlayed = announceRealtimeSessionFailure(failureCtx, server, sessionErr)
		}()
	}
	failureAnnouncementFinished := func() <-chan struct{} {
		if failureAnnouncementDone == nil {
			return nil
		}
		return failureAnnouncementDone
	}
	startNotificationFallback := func() {
		if server == nil || runtime == nil || notificationFallbackDone != nil || failureAnnouncementDone != nil {
			return
		}
		allowClip := !clipLatched
		if !server.CanSpeakVoiceNotification(allowClip) {
			return
		}
		fallbackCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		notificationFallbackCancel = cancel
		notificationFallbackDone = done
		clipPlayed = false
		go func() {
			defer close(done)
			clipPlayed = deliverPendingVoiceNotification(fallbackCtx, runtime, allowClip, server.SpeakVoiceNotification)
		}()
	}
	notificationFallbackFinished := func() <-chan struct{} {
		if notificationFallbackDone == nil {
			return nil
		}
		return notificationFallbackDone
	}
	if server != nil {
		server.SetRealtimeChatHandler(bridge.Handle)
		defer server.SetRealtimeChatHandler(nil)
	}
	logging.Infof("agent", "ready", "Starting realtime MCU wakeup listener...")
	watchers, err := startWakeupWatchers(newWatcher, func() {
		signalWakeupEvent(events)
	})
	if err != nil {
		logging.Warnf("agent", "realtime", "MCU wakeup unavailable: %v; /api/chat activation remains enabled", err)
	} else {
		defer stopWakeupWatchers(watchers)
	}

	logging.Infof("agent", "ready", "Waiting for realtime activation (/api/chat or %s)... Ctrl+C to quit", wakeupReadyLabel(cfg))
	var taskWake taskWakeState
	for {
		if reloadRequested(reload) {
			bridge.failQueued("voice configuration changed; retry the request")
			return
		}
		select {
		case <-reloadStop(reload):
			bridge.failQueued("voice configuration changed; retry the request")
			return
		case <-sigChan:
			stopNotificationFallback()
			stopFailureAnnouncement()
			logging.Infof("agent", "exit", "Stopped.")
			return
		case <-voiceNotificationTicker.C:
			startNotificationFallback()
		case <-notificationFallbackFinished():
			collectNotificationFallback()
		case <-failureAnnouncementFinished():
			collectFailureAnnouncement()
			if pendingActivation {
				pendingActivation = false
				signalWakeupEvent(events)
			}
		case <-agentTaskWakeNotifications(tasks):
			pending, seq := agentTaskTerminalState(tasks)
			if len(pending) == 0 {
				// The batch was already delivered by a session, so the wake
				// signal is stale and must not open an empty session.
				continue
			}
			if !taskWake.shouldActivate(seq) {
				// A restored update from a previous session. The manager keeps
				// it pending for the next session instead of activating in a
				// loop.
				continue
			}
			if failureAnnouncementCancel != nil {
				// Do not cut the failure announcement short for a task update;
				// activating after it exits keeps the user informed about both.
				pendingActivation = true
				continue
			}
			logging.Infof("agent", "realtime", "Background task update ready, activating realtime voice model...")
			signalWakeupEvent(events)
		case <-events:
			// Realtime is the preferred consumer. Stop idle standalone speech
			// before opening the realtime audio session.
			stopNotificationFallback()
			if failureAnnouncementCancel != nil {
				// Do not block the wakeup loop waiting for a provider that is
				// already being canceled. Keep the activation queued and start
				// the session after the failure announcement exits.
				failureAnnouncementCancel()
				pendingActivation = true
				continue
			}
			logging.Infof("agent", "realtime", "Activation requested, connecting realtime voice model...")
			rotated := false
			// The session is about to consume every terminal update available
			// now. Record that watermark before it runs: an update produced
			// while it is connected and dropped by its teardown is then still
			// announced by a later activation, while an update it delivered is
			// no longer pending and cannot open an empty session.
			taskWake.observeSession(agentTaskTerminalSequence(tasks))
			var semanticSink streamcontract.EventSink
			if server != nil {
				semanticSink = server.RealtimeEventSink()
			}
			sessionErr := runRealtimeSessionWithSink(cfg, sigChan, runtime, tasks, semanticSink, bridge)
			if errors.Is(sessionErr, errRealtimeShutdown) {
				return
			} else if errors.Is(sessionErr, realtimevoice.ErrSessionRotated) {
				// The provider capped session lifetime rather than failing, so the
				// conversation continues in a fresh session instead of dropping back
				// to idle. Queued chat requests are kept: they remain answerable
				// after the handover.
				rotated = true
				logging.Warnf("agent", "realtime", "session rotated by provider, reconnecting: %v", sessionErr)
			} else if sessionErr != nil {
				logging.Errorf("agent", "realtime", "session ended: %v", sessionErr)
				bridge.failQueued(sessionErr.Error())
				// The failed Realtime session cannot voice its own failure. Start
				// the standalone announcement without blocking activation handling;
				// a new activation cancels this speech first.
				startFailureAnnouncement(sessionErr)
			}
			drainRealtimeWakeups(events, failureAnnouncementDone != nil)
			// An API request can arrive while the previous session is tearing
			// down. Preserve that activation after clearing stale wake events.
			if rotated || chatBridgeHasPending(bridge) {
				signalWakeupEvent(events)
			}
			if !rotated {
				// A new standby period may face a different TTS state, and the
				// user has had a chance to hear the clip already, so re-arm it.
				clipLatched = false
				logging.Infof("agent", "ready", "Waiting for realtime activation...")
			}
		}
	}
}

func drainRealtimeWakeups(events <-chan struct{}, failureAnnouncementPending bool) {
	// A wakeup queued while failure speech starts is a new activation. Leave
	// it for the event loop to cancel the announcement and set pendingActivation.
	// The channel already coalesces ASR/API wakeups into one pending event.
	if failureAnnouncementPending {
		return
	}
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

func realtimeProviderType(cfg agent.Config) string {
	provider := strings.ToLower(strings.TrimSpace(cfg.VoiceModel.Provider))
	if record, ok := cfg.VoiceModelProviders[provider]; ok && strings.TrimSpace(record.Type) != "" {
		provider = strings.ToLower(strings.TrimSpace(record.Type))
	}
	if provider == "" {
		return "qwen"
	}
	return provider
}

func realtimeProviderSessionConfig(cfg agent.Config, runtime *agent.Runtime) realtimevoice.SessionConfig {
	return realtimeProviderSessionConfigWithTools(cfg, runtime, nil)
}

func realtimeProviderSessionConfigWithTools(cfg agent.Config, runtime *agent.Runtime, runtimeTools []langtools.Tool) realtimevoice.SessionConfig {
	usesBackendAgent := cfg.VoiceModel.UseBackendAgent
	voice := cfg.VoiceModel.Voice
	inputFormat := cfg.VoiceModel.InputAudioFormat
	if inputFormat == "" {
		inputFormat = "pcm"
	}
	outputFormat := cfg.VoiceModel.OutputAudioFormat
	if outputFormat == "" {
		outputFormat = "pcm"
	}
	turnType := cfg.VoiceModel.TurnDetection
	turnThreshold := cfg.VoiceModel.TurnDetectionThreshold
	turnSilenceMs := cfg.VoiceModel.TurnDetectionSilenceMs
	if realtimeProviderType(cfg) != realtimevoice.ProviderQwen {
		turnType = ""
		turnThreshold = nil
		turnSilenceMs = 0
	} else if turnType == "" {
		turnType = "server_vad"
	}
	baseInstructions := agent.DefaultRealtimeToolExecutionInstructions
	if usesBackendAgent {
		baseInstructions = agent.DefaultRealtimeVoiceInstructions
	}
	instructionParts := []string{baseInstructions}
	if prompt := strings.TrimSpace(cfg.Prompt); prompt != "" {
		instructionParts = append(instructionParts, prompt)
	}
	instructionParts = append(instructionParts, agent.ResponseLanguageGuidance(cfg.LocaleOrDefault()))
	instructions := strings.TrimSpace(strings.Join(instructionParts, "\n\n"))
	enableEmotion := cfg.VoiceModel.EnableSpeechEmotion
	if enableEmotion == nil {
		v := true
		enableEmotion = &v
	}
	return realtimevoice.SessionConfig{APIKey: cfg.VoiceModel.APIKey, Model: cfg.VoiceModel.Model, Voice: voice, Instructions: instructions,
		InputAudioFormat: inputFormat, OutputAudioFormat: outputFormat, MaxHistoryTurns: realtimeContextReplayTurns,
		TurnDetection: turnType, TurnDetectionThresh: turnThreshold, TurnDetectionSilenceMs: turnSilenceMs,
		EnableSpeechEmotion: enableEmotion, ThinkingLevel: cfg.VoiceModel.ThinkingLevel,
		Tools: realtimeVoiceToolDefinitionsWithTools(cfg, runtime, runtimeTools)}
}

// realtimeClientTurnEndpoint tracks a local speech turn when a provider
// requires client-side endpointing. The provider contract exposes commit()
// but does not expose a server VAD stream, so a small energy gate is needed
// to delimit turns on the hardware.
type realtimeClientTurnEndpoint struct {
	silenceDuration time.Duration
	speechActive    bool
	lastSpeechAt    time.Time
}

func newRealtimeClientTurnEndpoint(silenceMs int) *realtimeClientTurnEndpoint {
	silence := realtimeClientTurnEndpointSilence
	if silenceMs > 0 {
		silence = time.Duration(silenceMs) * time.Millisecond
	}
	return &realtimeClientTurnEndpoint{silenceDuration: silence}
}

func (e *realtimeClientTurnEndpoint) Observe(pcm []byte, now time.Time) bool {
	if e == nil || pcm16MeanAbs(pcm) < realtimeClientTurnSpeechThreshold {
		return false
	}
	e.speechActive = true
	e.lastSpeechAt = now
	return true
}

func (e *realtimeClientTurnEndpoint) Due(now time.Time) bool {
	return e != nil && e.speechActive && !e.lastSpeechAt.IsZero() && now.Sub(e.lastSpeechAt) >= e.silenceDuration
}

func (e *realtimeClientTurnEndpoint) Reset() {
	if e == nil {
		return
	}
	e.speechActive = false
	e.lastSpeechAt = time.Time{}
}

func realtimeAdmissionSpeechActive(endpoint *realtimeClientTurnEndpoint) bool {
	return endpoint != nil && endpoint.speechActive
}

// shouldTrackRealtimeAdmissionSpeech prevents the local energy gate from
// racing an explicit text response during session startup. The first audio
// chunk can be microphone startup noise (or audio already buffered before the
// chat command is selected); treating it as a new turn retires the pending
// anonymous response before Gemini emits response.created. Providers with
// server-authoritative turn detection still use the gate as a transient
// injection guard, but only provider events may promote audio into an input
// turn.
func shouldTrackRealtimeAdmissionSpeech(state *realtimeTurnState, chatPending bool, capabilities realtimevoice.Capabilities) bool {
	if state == nil || state.responseRequestPending {
		return false
	}
	if chatPending && !capabilities.ServerAuthoritativeTurnDetection {
		return false
	}
	if capabilities.ServerAuthoritativeInterruption && (state.responseActive || state.responseTerminalPending) {
		return false
	}
	return true
}

// observeRealtimeAdmissionSpeech updates the local energy endpoint without
// claiming provider-owned turns. Server-authoritative providers use the
// endpoint only to prevent text injection while speech is still in progress.
func observeRealtimeAdmissionSpeech(endpoint *realtimeClientTurnEndpoint, state *realtimeTurnState, capabilities realtimevoice.Capabilities, pcm []byte, now time.Time, clearAfterSilence bool) (started, stopped bool) {
	if endpoint == nil || state == nil {
		return false, false
	}
	wasActive := endpoint.speechActive
	endpoint.Observe(pcm, now)
	started = !wasActive && endpoint.speechActive
	if started && !capabilities.ServerAuthoritativeTurnDetection {
		state.speechStarted()
	}
	if !clearAfterSilence || !endpoint.speechActive || !endpoint.Due(now) {
		return started, false
	}
	endpoint.Reset()
	if !capabilities.ServerAuthoritativeTurnDetection {
		state.localSpeechStopped()
	}
	return started, true
}

func pcm16MeanAbs(pcm []byte) int {
	const sampleBytes = 2
	if len(pcm) < sampleBytes {
		return 0
	}
	var total uint64
	samples := len(pcm) / sampleBytes
	for i := 0; i < samples; i++ {
		value := int64(int16(binary.LittleEndian.Uint16(pcm[i*sampleBytes:])))
		if value < 0 {
			value = -value
		}
		total += uint64(value)
	}
	return int(total / uint64(samples))
}

const (
	realtimeCurrentTimeTool        = "get_current_time"
	realtimeRecallTool             = "recall_memory"
	realtimeSaveMemoryTool         = "save_memory"
	realtimeForgetMemoryTool       = "forget_memory"
	realtimeRecallChunksTool       = "recall_session_chunks"
	realtimeAudioVolumeTool        = "audio_volume"
	realtimeCreateTaskTool         = "create_agent_task"
	realtimeCancelTaskTool         = "cancel_agent_task"
	realtimeUpdateTaskTool         = "update_agent_task"
	realtimeQueryTaskTool          = "query_agent_task"
	realtimeResponseUserActionTool = "response_user_action"
	realtimeEndConversationTool    = "end_conversation"
	realtimeWaitForWakeupTool      = "wait_for_wakeup"
	realtimeRequestUserActionTool  = "request_user_action"
)

var realtimeDelegatedTools = []string{
	realtimeRecallTool,
	realtimeSaveMemoryTool,
	realtimeForgetMemoryTool,
	realtimeRecallChunksTool,
	realtimeAudioVolumeTool,
}

var realtimeBackendCommunicationRuntimeTools = map[string]struct{}{
	realtimeRequestUserActionTool: {},
	realtimeWaitForWakeupTool:     {},
}

func realtimeVoiceToolDefinitions(cfg agent.Config, runtime *agent.Runtime) []realtimevoice.Tool {
	return realtimeVoiceToolDefinitionsWithTools(cfg, runtime, nil)
}

func realtimeVoiceToolDefinitionsWithTools(cfg agent.Config, runtime *agent.Runtime, runtimeTools []langtools.Tool) []realtimevoice.Tool {
	tools := []realtimevoice.Tool{
		realtimeVoiceToolDefinition(
			realtimeCurrentTimeTool,
			"Get the current local date, time, timezone, and UTC offset.",
			map[string]any{"type": "object", "properties": map[string]any{}},
		),
		realtimeDelegatedVoiceToolDefinition(
			agent.NewRecallMemoryTool(nil),
			"Recall saved long-term user preferences, rules, facts, procedures, or profile information.",
		),
		realtimeDelegatedVoiceToolDefinition(
			agent.NewSaveMemoryTool(nil),
			"Save a long-term memory for future recall. Mandatory when the user asks you to remember or save something; also use for stable preferences, rules, or procedures you observe. Do not tell the user you remembered it until this tool returns status=saved or status=ignored for a duplicate. Use profile for durable user facts such as name, location, or timezone; preference or rule for future defaults and must/must-not behavior; procedure for how-to steps; fact for stable info worth recalling later.",
		),
		realtimeDelegatedVoiceToolDefinition(
			agent.NewForgetMemoryTool(nil),
			"Delete a saved memory when the user asks you to forget it. First call recall_memory to find the memory id, then pass that id here. Never expose the id to the user.",
		),
		realtimeDelegatedVoiceToolDefinition(
			agent.NewRecallSessionChunksTool(nil),
			"Recall compressed history from earlier in this conversation and from prior conversations. Only the most recent turns stay in your visible context; older turns are compressed and invisible until recalled. Call this whenever the user refers to something discussed earlier that you cannot see, including when they claim something was or was not said. Pass known chunk ids as chunk_ids, topic keywords as tags, or empty tags for the most recent history. For saved preferences, rules, or facts, use recall_memory instead.",
		),
		realtimeDelegatedVoiceToolDefinition(
			agent.NewAudioVolumeTool(""),
			"Get or set your own speaking volume. Omit volume to read the current value. This controls your playback volume only, not the phone's system volume.",
		),
		realtimeVoiceToolDefinition(
			realtimeEndConversationTool,
			"End the voice conversation and return to standby when the user is done or asks to stop the voice interaction. Use this for goodbye, stop listening, stop the service, go idle, standby, sleep, or wait for the next wakeup; these are conversation-control requests, not device or backend service-operation requests. Say a short farewell in the same response; the microphone stays open until you finish speaking. The user can start a new conversation at any time, and you keep your memory and history. Work you are already handling continues, and when it finishes the device comes back on its own to report the outcome, so you may briefly say that you will let them know.",
			map[string]any{"type": "object", "properties": map[string]any{}},
		),
	}
	usesBackendAgent := cfg.VoiceModel.UseBackendAgent
	if !usesBackendAgent {
		if runtime != nil {
			if runtimeTools == nil {
				runtimeTools = runtime.AvailableTools()
			}
			for _, tool := range runtimeTools {
				if tool == nil {
					continue
				}
				if _, excluded := realtimeBackendCommunicationRuntimeTools[tool.Name()]; excluded {
					continue
				}
				if containsRealtimeVoiceTool(tools, tool.Name()) {
					continue
				}
				spec := agent.NewToolSpec(tool)
				tools = append(tools, realtimeVoiceToolDefinition(spec.Name, spec.Description, spec.LLMSchema()))
			}
		}
	}

	if usesBackendAgent {
		tools = append(tools,
			realtimeVoiceToolDefinition(
				realtimeCreateTaskTool,
				"Handle any request you cannot directly and reliably answer or complete with the realtime conversation tools, including device state, visual inspection, external actions, lookups, or longer multi-step work. Call query_agent_task with no task_id first and continue the task that already covers the request instead of creating a duplicate. Present the work to the user as your own responsibility.",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task": map[string]any{"type": "string", "description": "A self-contained description of the work you will handle."},
					},
					"required": []string{"task"},
				},
			),
			realtimeVoiceToolDefinition(
				realtimeCancelTaskTool,
				"Cancel work that you previously started when the user asks you to stop it. If the work already finished but its result has not started delivery, this clears that stale result notification.",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "string"},
					},
					"required": []string{"task_id"},
				},
			),
			realtimeVoiceToolDefinition(
				realtimeUpdateTaskTool,
				"Replace the complete goal of work that is queued, running, or waiting for user action. Use this when the user changes an existing request instead of cancelling it and creating another task.",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "string"},
						"task":    map[string]any{"type": "string", "description": "The complete updated goal, including all requirements that still apply."},
					},
					"required": []string{"task_id", "task"},
				},
			),
			realtimeVoiceToolDefinition(
				realtimeQueryTaskTool,
				"Check the status and result of work you are handling. Pass the task_id you were given, or omit it to list every outstanding task: work still in flight, and finished work whose result you have not been told about yet.",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "string", "description": "Task to report. Omit to list every outstanding task."},
					},
				},
			),
			realtimeVoiceToolDefinition(
				realtimeResponseUserActionTool,
				"Continue work after the user completed a requested device action. Use the internal task reference and pass a concise description of what the user did; never expose the internal reference to the user.",
				map[string]any{"type": "object", "properties": map[string]any{"task_id": map[string]any{"type": "string"}, "user_message": map[string]any{"type": "string"}}, "required": []string{"task_id", "user_message"}},
			),
		)
	}

	return tools
}

func containsRealtimeVoiceTool(tools []realtimevoice.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func realtimeVoiceToolDefinition(name, description string, parameters map[string]any) realtimevoice.Tool {
	encoded, err := json.Marshal(parameters)
	if err != nil {
		encoded = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return realtimevoice.Tool{Name: name, Description: description, Parameters: encoded}
}

func realtimeDelegatedVoiceToolDefinition(tool langtools.Tool, description string) realtimevoice.Tool {
	spec := agent.NewToolSpec(tool)
	return realtimeVoiceToolDefinition(spec.Name, description, spec.LLMSchema())
}

type realtimeVoiceToolExecutor struct {
	delegated map[string]langtools.Tool
	now       func() time.Time
	tasks     *agenttask.Manager
}

func newRealtimeVoiceToolExecutor(runtime *agent.Runtime, tasks *agenttask.Manager, availableTools ...[]langtools.Tool) realtimeVoiceToolExecutor {
	executor := realtimeVoiceToolExecutor{now: time.Now, tasks: tasks}
	if runtime == nil {
		return executor
	}
	var available []langtools.Tool
	if len(availableTools) > 0 && availableTools[0] != nil {
		available = availableTools[0]
	} else {
		available = make([]langtools.Tool, 0, len(realtimeDelegatedTools))
		for _, name := range realtimeDelegatedTools {
			if tool, ok := runtime.Tool(name); ok {
				available = append(available, tool)
			}
		}
	}
	executor.delegated = make(map[string]langtools.Tool, len(available))
	for _, tool := range available {
		if tool == nil {
			continue
		}
		if _, excluded := realtimeBackendCommunicationRuntimeTools[tool.Name()]; excluded {
			continue
		}
		executor.delegated[tool.Name()] = tool
	}
	return executor
}

func (e realtimeVoiceToolExecutor) call(ctx context.Context, name, arguments string) string {
	switch name {
	case realtimeCurrentTimeTool:
		now := e.now()
		zone, offsetSeconds := now.Zone()
		return realtimeToolJSON(map[string]any{
			"datetime":           now.Format(time.RFC3339),
			"timezone":           zone,
			"utc_offset_seconds": offsetSeconds,
		})
	case realtimeRecallTool, realtimeSaveMemoryTool, realtimeForgetMemoryTool, realtimeRecallChunksTool, realtimeAudioVolumeTool:
		tool := e.delegated[name]
		if tool == nil {
			return realtimeToolJSON(map[string]any{"error": fmt.Sprintf("%s is unavailable", name)})
		}
		output, err := tool.Call(ctx, arguments)
		if err != nil {
			return realtimeToolJSON(map[string]any{"error": err.Error()})
		}
		return output
	case realtimeEndConversationTool:
		// The session loop owns teardown after the farewell finishes playing.
		return realtimeToolJSON(map[string]any{"status": "ending"})
	case realtimeCreateTaskTool:
		var input struct {
			Task string `json:"task"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return realtimeToolJSON(map[string]any{"error": "invalid task arguments"})
		}
		if e.tasks == nil {
			return realtimeToolJSON(map[string]any{"error": "background agent is unavailable"})
		}
		task, err := e.tasks.Create(input.Task)
		if err != nil {
			return realtimeToolJSON(map[string]any{"error": err.Error()})
		}
		return realtimeToolJSON(task)
	case realtimeUpdateTaskTool:
		var input struct {
			TaskID string `json:"task_id"`
			Task   string `json:"task"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return realtimeToolJSON(map[string]any{"error": "invalid task arguments"})
		}
		if e.tasks == nil {
			return realtimeToolJSON(map[string]any{"error": "background agent is unavailable"})
		}
		task, err := e.tasks.Update(input.TaskID, input.Task)
		if err != nil {
			return realtimeToolJSON(map[string]any{"error": err.Error()})
		}
		return realtimeToolJSON(task)
	case realtimeCancelTaskTool, realtimeQueryTaskTool:
		var input struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return realtimeToolJSON(map[string]any{"error": "invalid task arguments"})
		}
		if e.tasks == nil {
			return realtimeToolJSON(map[string]any{"error": "background agent is unavailable"})
		}
		if name == realtimeCancelTaskTool {
			task, notificationCleared, err := e.tasks.Cancel(input.TaskID)
			if err != nil {
				return realtimeToolJSON(map[string]any{"error": err.Error()})
			}
			return realtimeToolJSON(map[string]any{
				"status":                       task.Status,
				"pending_notification_cleared": notificationCleared,
			})
		}
		// No task_id asks for everything outstanding, which is how the foreground
		// checks for work it already started before creating more.
		if strings.TrimSpace(input.TaskID) == "" {
			return realtimeTaskListJSON(e.tasks.Outstanding())
		}
		task, ok := e.tasks.Query(input.TaskID)
		if !ok {
			return realtimeToolJSON(map[string]any{"error": "agent task not found"})
		}
		return realtimeTaskListJSON([]agenttask.Task{task})
	case realtimeResponseUserActionTool:
		var input struct {
			TaskID      string `json:"task_id"`
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			return realtimeToolJSON(map[string]any{"error": "invalid task arguments"})
		}
		if e.tasks == nil {
			return realtimeToolJSON(map[string]any{"error": "background agent is unavailable"})
		}
		task, err := e.tasks.Continue(input.TaskID, input.UserMessage)
		if err != nil {
			return realtimeToolJSON(map[string]any{"error": err.Error()})
		}
		return realtimeToolJSON(task)
	default:
		if tool := e.delegated[name]; tool != nil {
			spec := agent.NewToolSpec(tool)
			input := spec.NormalizeInput(arguments)
			if err := spec.ValidateInput(input); err != nil {
				return realtimeToolJSON(map[string]any{"error": err.Error()})
			}
			output, err := tool.Call(ctx, input)
			if err != nil {
				return realtimeToolJSON(map[string]any{"error": err.Error()})
			}
			return output
		}
		return realtimeToolJSON(map[string]any{"error": fmt.Sprintf("unsupported realtime tool %q", name)})
	}
}

// realtimeTaskListJSON answers query_agent_task with one shape for both modes:
// the requested task, or every outstanding task when no task_id was given. An
// empty list is encoded as [] rather than null so the model reads it as a list.
func realtimeTaskListJSON(tasks []agenttask.Task) string {
	if tasks == nil {
		tasks = []agenttask.Task{}
	}
	return realtimeToolJSON(map[string]any{"tasks": tasks})
}

func realtimeToolJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"error":"failed to encode tool result"}`
	}
	return string(encoded)
}

func startRealtimeToolCall(ctx context.Context, executor realtimeVoiceToolExecutor, call realtimevoice.Event, results chan<- realtimeToolResult) {
	go func() {
		callCtx, cancel := context.WithTimeout(ctx, realtimeToolCallTimeout)
		defer cancel()
		result := realtimeToolResult{
			call:   call,
			output: executor.call(callCtx, call.Name, call.Arguments),
		}
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}()
}

// realtimeToolCallStarter is a test seam for holding a foreground tool in
// flight while the real session loop processes an interruption.
var realtimeToolCallStarter = startRealtimeToolCall

func startRealtimeSuppressedToolCall(ctx context.Context, call realtimevoice.Event, results chan<- realtimeToolResult) {
	go func() {
		result := realtimeToolResult{
			call:   call,
			output: `{"error":"tools are disabled while delivering a voice notification"}`,
		}
		select {
		case results <- result:
		case <-ctx.Done():
		}
	}()
}

func realtimeVoiceNotificationPrompt(text string) string {
	return "请原样朗读下面的语音通知，只输出通知原文，不要说“已收到”、 “好的”或其他内容，不要调用工具，也不要提及这条指令。语音通知：" + strings.TrimSpace(text)
}

// announceRealtimeSessionFailure speaks a spoken-language description of a
// failed Realtime session through the standalone speech path. The failed
// session cannot voice its own error, so without this the user hears nothing.
func announceRealtimeSessionFailure(ctx context.Context, server *agent.Server, sessionErr error) bool {
	if server == nil || sessionErr == nil {
		return false
	}
	// Cancellation is a local teardown, not a failure worth announcing.
	if errors.Is(sessionErr, context.Canceled) {
		return false
	}
	failure := agent.TurnFailureFromError(sessionErr)
	if failure == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	announceCtx, cancel := context.WithTimeout(ctx, realtimeToolCallTimeout)
	defer cancel()
	clipPlayed, err := server.SpeakTurnFailure(announceCtx, failure)
	if err == nil {
		return clipPlayed
	}
	if clipPlayed {
		logging.Warnf("agent", "realtime", "session failure announced with prerecorded clip: %v", err)
		return true
	}
	if !errors.Is(err, context.Canceled) {
		logging.Errorf("agent", "realtime", "could not announce session failure: %v", err)
	}
	return false
}

// deliverPendingVoiceNotification speaks one pending notification through the
// standalone TTS path. It reports whether the prerecorded TTS-unavailable clip
// played in place of the notification text, which leaves the notification
// pending for a later speech path.
func deliverPendingVoiceNotification(ctx context.Context, runtime *agent.Runtime, allowFallbackClip bool, speaker func(context.Context, string, bool) (bool, error)) bool {
	if runtime == nil || speaker == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := runtime.PrepareVoiceNotification(ctx)
	if prepared.DeliveryToken == "" || strings.TrimSpace(prepared.Text) == "" {
		return false
	}
	speakCtx, cancel := context.WithTimeout(ctx, realtimeToolCallTimeout)
	defer cancel()
	clipPlayed, err := speaker(speakCtx, prepared.Text, allowFallbackClip)
	runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
	if err != nil {
		if clipPlayed {
			logging.Warnf("agent", "voice_notification", "standalone TTS unavailable, played prerecorded clip; notification stays pending: %v", err)
		} else {
			logging.Errorf("agent", "voice_notification", "standalone TTS fallback failed: %v", err)
		}
	}
	return clipPlayed
}

func realtimePlaybackOutputFormat(cfg agent.Config) agent.AudioFormat {
	return agent.AudioFormat{
		SampleRate: uint32(cfg.Audio.SampleRateOrDefault()),
		Channels:   uint32(cfg.Audio.ChannelsOrDefault()),
		BitWidth:   uint32(cfg.Audio.BitWidthOrDefault()),
	}
}

func realtimeAgentAudioFormat(format realtimevoice.AudioFormat) agent.AudioFormat {
	return agent.AudioFormat{SampleRate: uint32(format.SampleRate), Channels: uint32(format.Channels), BitWidth: uint32(format.BitDepth)}
}

func realtimeVoiceDeviceFormat(cfg agent.Config) realtimevoice.AudioFormat {
	return realtimevoice.AudioFormat{
		Encoding:   "pcm_s16le",
		SampleRate: int(cfg.Audio.SampleRateOrDefault()),
		Channels:   int(cfg.Audio.ChannelsOrDefault()),
		BitDepth:   int(cfg.Audio.BitWidthOrDefault()),
	}
}

func realtimeProviderAudioFormat(format realtimevoice.AudioFormat, legacyRate, defaultRate int) agent.AudioFormat {
	return realtimeAgentAudioFormat(format.OrDefault(positiveRate(legacyRate, defaultRate)))
}

func positiveRate(primary, fallback int) int {
	if primary > 0 {
		return primary
	}
	return fallback
}

func runRealtimeSession(cfg agent.Config, sigChan chan os.Signal, runtime *agent.Runtime, tasks *agenttask.Manager, chatBridges ...*realtimeChatBridge) error {
	return runRealtimeSessionWithSink(cfg, sigChan, runtime, tasks, nil, chatBridges...)
}

func runRealtimeSessionWithRegistry(cfg agent.Config, sigChan chan os.Signal, runtime *agent.Runtime, tasks *agenttask.Manager, registry *realtimevoice.ProviderRegistry, chatBridges ...*realtimeChatBridge) (returnErr error) {
	return runRealtimeSessionWithRegistryAndSink(cfg, sigChan, runtime, tasks, registry, nil, realtimeResponseIdleTimeout, chatBridges...)
}

func runRealtimeSessionWithSink(cfg agent.Config, sigChan chan os.Signal, runtime *agent.Runtime, tasks *agenttask.Manager, sink streamcontract.EventSink, chatBridges ...*realtimeChatBridge) error {
	return runRealtimeSessionWithRegistryAndSink(cfg, sigChan, runtime, tasks, realtimevoice.DefaultProviderRegistry(), sink, realtimeResponseIdleTimeout, chatBridges...)
}

func runRealtimeSessionWithIdleTimeout(cfg agent.Config, sigChan chan os.Signal, runtime *agent.Runtime, tasks *agenttask.Manager, registry *realtimevoice.ProviderRegistry, idleTimeout time.Duration, chatBridges ...*realtimeChatBridge) (returnErr error) {
	return runRealtimeSessionWithRegistryAndSink(cfg, sigChan, runtime, tasks, registry, nil, idleTimeout, chatBridges...)
}

func runRealtimeSessionWithRegistryAndSink(cfg agent.Config, sigChan chan os.Signal, runtime *agent.Runtime, tasks *agenttask.Manager, registry *realtimevoice.ProviderRegistry, sink streamcontract.EventSink, idleTimeout time.Duration, chatBridges ...*realtimeChatBridge) (returnErr error) {
	var chatBridge *realtimeChatBridge
	if len(chatBridges) > 0 {
		chatBridge = chatBridges[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runtimeTools []langtools.Tool
	if !cfg.VoiceModel.UseBackendAgent && runtime != nil {
		runtimeTools = runtime.AvailableTools()
	}
	sessionConfig := realtimeProviderSessionConfigWithTools(cfg, runtime, runtimeTools)
	if runtime != nil {
		if err := runtime.PrepareUserContext(sessionConfig.Instructions); err != nil {
			return err
		}
	}
	if runtime != nil {
		unregisterReset := runtime.RegisterUserContextResetHook(cancel)
		defer unregisterReset()
	}
	userContext, err := agent.InitializeContextManager(
		sessionConfig.Instructions,
		agentpath.UserContextManagerSessionFolder(cfg.ConfigDir),
		nil,
	)
	if err != nil {
		return fmt.Errorf("initialize realtime user context: %w", err)
	}

	providerName := realtimeProviderType(cfg)
	logging.Infof("agent", "realtime", "Connecting: provider=%s model=%s region=%s workspace_configured=%t endpoint_override=%t",
		providerName, cfg.VoiceModel.Model, cfg.VoiceModel.Region, cfg.VoiceModel.WorkspaceID != "", cfg.VoiceModel.Endpoint != "")
	connectCtx, connectCancel := context.WithTimeout(ctx, realtimeConnectTimeout)
	session, err := registry.Open(connectCtx, providerName, realtimevoice.ProviderConfig{Endpoint: cfg.VoiceModel.Endpoint, BaseURL: cfg.VoiceModel.BaseURL, AgentID: cfg.VoiceModel.AgentID, UpstreamProvider: cfg.VoiceModel.UpstreamProvider, WorkspaceID: cfg.VoiceModel.WorkspaceID, Region: cfg.VoiceModel.Region, AuthMode: cfg.VoiceModel.AuthMode, ProjectID: cfg.VoiceModel.ProjectID, Location: cfg.VoiceModel.Location, RealtimeProtocol: cfg.VoiceModel.RealtimeProtocol}, sessionConfig, realtimevoice.DeviceMediaConfig{Input: realtimeVoiceDeviceFormat(cfg), Output: realtimeVoiceDeviceFormat(cfg)})
	connectCancel()
	if err != nil {
		return markRealtimeProviderFailure(fmt.Errorf("connect realtime voice model: %w", err))
	}
	defer session.Close()
	info := session.Info()
	semanticEmitter := newRealtimeSemanticEmitter(sink)
	if semanticEmitter != nil {
		// Open returns only after the managed session has negotiated its media
		// contract, so publish readiness even for providers that do not expose a
		// separate ready frame on their event channel.
		semanticEmitter.emit(realtimevoice.Event{Kind: realtimevoice.EventReady, SessionID: info.ID})
	}
	defer func() {
		if semanticEmitter != nil {
			semanticEmitter.emit(realtimevoice.Event{Kind: realtimevoice.EventClosed})
		}
	}()
	turnCommitter := session.TurnCommitter
	responseInterrupter := session.ResponseInterrupter
	canInterrupt := responseInterrupter != nil
	toolResultSender := session.ToolResultSender
	canSendToolResult := toolResultSender != nil
	textSession := session.TextSession
	supportsText := textSession != nil
	contextReplayer := session.ContextReplayer
	canReplayContext := contextReplayer != nil
	if supportsText && canReplayContext {
		if err := replayRealtimeContext(ctx, contextReplayer, userContext); err != nil {
			return markRealtimeProviderFailure(fmt.Errorf("restore realtime user context: %w", err))
		}
	}
	logging.Infof("agent", "realtime", "Session ready: id=%s input_rate=%d output_rate=%d provider_input_rate=%d provider_output_rate=%d text_input=%t", info.ID, info.InputSampleRate, info.OutputSampleRate, info.ProviderInputAudioFormat.SampleRate, info.ProviderOutputAudioFormat.SampleRate, supportsText)
	if chatBridge != nil {
		chatBridge.activate()
		defer func() {
			chatBridge.deactivate()
			if shouldFailQueuedRealtimeChat(returnErr) {
				chatBridge.failQueued("realtime voice session ended")
			}
		}()
	}

	audioService := agent.NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	recordingAudio := agent.NewConfiguredAudioRecordingBackend(cfg, audioService, nil)
	playbackAudio := realtimePlaybackAudioAdapter{
		backend: agent.NewConfiguredAudioPlaybackBackend(cfg, audioService, nil),
	}
	inputFormat := realtimeAgentAudioFormat(info.InputFormatOrDefault(16000))
	recording, err := recordingAudio.StartRecording(inputFormat)
	if err != nil {
		return fmt.Errorf("start realtime recording: %w", err)
	}
	defer func() { _ = recordingAudio.StopRecording(recording.SessionID) }()
	logging.Infof("agent", "realtime", "Microphone opened: session_id=%d format=pcm/%d/%d/%d", recording.SessionID, inputFormat.SampleRate, inputFormat.Channels, inputFormat.BitWidth)

	chunks := make(chan []byte, 8)
	readErrs := make(chan error, 1)
	go streamRealtimeAudio(ctx, recordingAudio, recording.SessionID, chunks, readErrs)

	realtimeOutputFormat := realtimeAgentAudioFormat(info.OutputFormatOrDefault(24000))
	outputFormat := realtimePlaybackOutputFormat(cfg)
	if outputFormat.SampleRate == 0 {
		outputFormat = realtimeOutputFormat
	}
	logging.Infof("agent", "realtime", "Playback format: source=pcm/%d/%d/%d target=pcm/%d/%d/%d resample=%t",
		realtimeOutputFormat.SampleRate,
		realtimeOutputFormat.Channels,
		realtimeOutputFormat.BitWidth,
		outputFormat.SampleRate,
		outputFormat.Channels,
		outputFormat.BitWidth,
		uint32(info.ProviderOutputAudioFormat.SampleRate) != outputFormat.SampleRate,
	)
	playback := realtimePlaybackState{finalizeResponses: cfg.AudioBackendOrDefault() == agent.AudioBackendLocal}
	if err := playback.open(playbackAudio, outputFormat); err != nil {
		return fmt.Errorf("open realtime playback: %w", err)
	}
	logging.Infof("agent", "realtime", "Speaker opened: session_id=%d", playback.session.SessionID)
	defer func() { _ = playback.stop(playbackAudio) }()
	playbackKeepAlive := time.NewTicker(realtimePlaybackKeepAlive)
	defer playbackKeepAlive.Stop()
	var clientTurnEndpoint *realtimeClientTurnEndpoint
	var clientTurnCommitTimer *time.Timer
	var clientTurnCommitTimerCh <-chan time.Time
	if info.Capabilities.ClientSideTurnDetection {
		clientTurnEndpoint = newRealtimeClientTurnEndpoint(cfg.VoiceModel.TurnDetectionSilenceMs)
	}
	// Gemini Live and other providers may run server VAD without reporting the
	// input speech boundaries. Keep a local energy gate for task admission so a
	// background update cannot be injected in the gap before the first transcript
	// arrives. This gate does not send provider commit events unless the provider
	// explicitly requires client-side endpointing.
	admissionTurnEndpoint := clientTurnEndpoint
	if admissionTurnEndpoint == nil && !info.Capabilities.EmitsSpeechEvents {
		admissionTurnEndpoint = newRealtimeClientTurnEndpoint(cfg.VoiceModel.TurnDetectionSilenceMs)
	}
	stopClientTurnCommitTimer := func() {
		if clientTurnCommitTimer == nil {
			clientTurnCommitTimerCh = nil
			return
		}
		if !clientTurnCommitTimer.Stop() {
			select {
			case <-clientTurnCommitTimer.C:
			default:
			}
		}
		clientTurnCommitTimerCh = nil
	}
	defer stopClientTurnCommitTimer()
	armClientTurnCommitTimer := func() {
		if clientTurnEndpoint == nil || !clientTurnEndpoint.speechActive {
			return
		}
		delay := clientTurnEndpoint.silenceDuration
		if !clientTurnEndpoint.lastSpeechAt.IsZero() {
			delay = clientTurnEndpoint.silenceDuration - time.Since(clientTurnEndpoint.lastSpeechAt)
			if delay < 0 {
				delay = 0
			}
		}
		if clientTurnCommitTimer == nil {
			clientTurnCommitTimer = time.NewTimer(delay)
		} else {
			if !clientTurnCommitTimer.Stop() {
				select {
				case <-clientTurnCommitTimer.C:
				default:
				}
			}
			clientTurnCommitTimer.Reset(delay)
		}
		clientTurnCommitTimerCh = clientTurnCommitTimer.C
	}
	commitClientTurn := func() error {
		if clientTurnEndpoint == nil || !clientTurnEndpoint.speechActive {
			return nil
		}
		stopClientTurnCommitTimer()
		if err := turnCommitter.Commit(ctx); err != nil {
			return markRealtimeProviderFailure(fmt.Errorf("commit realtime input turn: %w", err))
		}
		clientTurnEndpoint.Reset()
		return nil
	}
	firstAudioChunk := true
	var activeChat *realtimeChatCommand
	var queuedChat *realtimeChatCommand
	cancelPending := false
	var cancelAckTimer *time.Timer
	var cancelAckDeadline <-chan time.Time
	stopCancelAckTimer := func() {
		if cancelAckTimer != nil {
			if !cancelAckTimer.Stop() {
				select {
				case <-cancelAckTimer.C:
				default:
				}
			}
		}
		cancelAckDeadline = nil
	}
	defer stopCancelAckTimer()
	startCancelAckTimer := func() {
		provider := session.InterruptionAckTimeoutProvider
		if provider == nil {
			return
		}
		timeout := provider.InterruptionAckTimeout()
		if timeout <= 0 {
			return
		}
		stopCancelAckTimer()
		if cancelAckTimer == nil {
			cancelAckTimer = time.NewTimer(timeout)
		} else {
			cancelAckTimer.Reset(timeout)
		}
		cancelAckDeadline = cancelAckTimer.C
	}
	defer func() {
		failActiveRealtimeChat(activeChat, returnErr)
		failActiveRealtimeChat(queuedChat, returnErr)
	}()
	var chatMode string
	var chatText strings.Builder
	var chatTranscript strings.Builder
	var responseText strings.Builder
	var responseTranscript strings.Builder
	assistantPersisted := false
	var realtimeResponseUsage messages.Usage
	hasRealtimeResponseUsage := false
	turnState := realtimeTurnState{}
	watchdog := realtimeResponseWatchdog{timeout: idleTimeout}
	defer watchdog.stop()
	type foregroundTool struct {
		responseID string
		cancel     context.CancelFunc
	}
	foregroundTools := make(map[string]foregroundTool)
	cancelForegroundTools := func(responseID string) {
		for id, tool := range foregroundTools {
			if responseID == "" || tool.responseID == responseID {
				tool.cancel()
				delete(foregroundTools, id)
			}
		}
	}
	defer cancelForegroundTools("")
	requestResponse := func() {
		if semanticEmitter != nil {
			semanticEmitter.nextResponseEpoch()
		}
		turnState.responseRequested()
		watchdog.progress()
	}
	defer func() {
		logging.Infof("agent", "realtime", "Session released: active_request_id=%s response_id=%s occupied_ms=%d error=%v",
			realtimeChatRequestID(activeChat), turnState.responseID, watchdog.age().Milliseconds(), returnErr)
	}()
	voiceNotificationTicker := time.NewTicker(realtimeNotificationPoll)
	defer voiceNotificationTicker.Stop()
	activeNotificationToken := ""
	activeNotificationResponseID := ""
	activeNotificationAudioWritten := false
	responseSuppressed := false
	suppressedNotificationResponsePending := false
	suppressedNotificationResponseIDs := make(map[string]struct{})
	var notificationDrainDone <-chan error
	var notificationDrainCancel context.CancelFunc
	var sleep realtimeSleepState
	defer func() {
		if notificationDrainCancel != nil {
			notificationDrainCancel()
		}
		sleep.stopDrain()
	}()
	defer func() {
		if activeNotificationToken != "" && runtime != nil {
			runtime.ReportSpokenTextDelivery(activeNotificationToken, context.Canceled)
		}
	}()
	toolExecutor := newRealtimeVoiceToolExecutor(runtime, tasks, runtimeTools)
	toolResults := make(chan realtimeToolResult, 16)
	toolTracker := newRealtimeToolTracker()
	var pendingTaskUpdates []agenttask.Task
	var taskDebounceTimer *time.Timer
	var taskDebounce <-chan time.Time
	taskUpdatesReady := false
	defer func() {
		if taskDebounceTimer != nil {
			taskDebounceTimer.Stop()
		}
		if tasks != nil && len(pendingTaskUpdates) > 0 {
			var terminal, actions []agenttask.Task
			for _, task := range pendingTaskUpdates {
				// Only a running task can be waiting for a user action; a
				// terminal snapshot is always a result update.
				if task.Status == agenttask.StatusRunning && task.PendingUserAction != nil {
					actions = append(actions, task)
				} else {
					terminal = append(terminal, task)
				}
			}
			tasks.RestoreTerminalTasks(terminal)
			tasks.RestoreUserActionTasks(actions)
			logging.Infof("agent", "realtime", "Task updates returned to the queue: terminal=%d user_action=%d", len(terminal), len(actions))
		}
		if tasks != nil {
			tasks.RestoreUserActionTasks(tasks.PendingUserActionTasks())
		}
	}()
	tryInjectTaskUpdates := func() error {
		if !taskUpdatesReady || len(pendingTaskUpdates) == 0 || !turnState.canInjectResponse() || realtimeAdmissionSpeechActive(admissionTurnEndpoint) || activeNotificationToken != "" || notificationDrainDone != nil || activeChat != nil || sleep.pending() || chatBridgeHasPending(chatBridge) {
			return nil
		}
		if !supportsText {
			return nil
		}
		delivering := pendingTaskUpdates
		if tasks != nil {
			delivering = tasks.BeginTaskUpdateDelivery(pendingTaskUpdates)
		}
		pendingTaskUpdates = delivering
		if len(delivering) == 0 {
			taskUpdatesReady = false
			return nil
		}
		message := formatRealtimeTaskUpdates(delivering)
		if err := appendRealtimeNoticeMessage(userContext, message); err != nil {
			return fmt.Errorf("persist task update in realtime context: %w", err)
		}
		if err := textSession.SendText(ctx, message); err != nil {
			return markRealtimeProviderFailure(fmt.Errorf("inject background task update: %w", err))
		}
		if err := textSession.CreateResponse(ctx); err != nil {
			return markRealtimeProviderFailure(fmt.Errorf("respond to background task update: %w", err))
		}
		if tasks != nil {
			tasks.CompleteTaskUpdateDelivery(delivering)
		}
		pendingTaskUpdates = nil
		taskUpdatesReady = false
		requestResponse()
		return nil
	}
	tryInjectVoiceNotification := func() error {
		if runtime == nil || !supportsText || activeNotificationToken != "" || notificationDrainDone != nil || !turnState.canInjectResponse() || realtimeAdmissionSpeechActive(admissionTurnEndpoint) || activeChat != nil || sleep.pending() || chatBridgeHasPending(chatBridge) {
			return nil
		}
		prepared := runtime.PrepareVoiceNotification(ctx)
		if prepared.DeliveryToken == "" || strings.TrimSpace(prepared.Text) == "" {
			return nil
		}
		if err := textSession.SendText(ctx, realtimeVoiceNotificationPrompt(prepared.Text)); err != nil {
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
			return markRealtimeProviderFailure(fmt.Errorf("inject realtime voice notification: %w", err))
		}
		if err := textSession.CreateResponse(ctx); err != nil {
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
			return markRealtimeProviderFailure(fmt.Errorf("respond to realtime voice notification: %w", err))
		}
		activeNotificationToken = prepared.DeliveryToken
		activeNotificationResponseID = ""
		activeNotificationAudioWritten = false
		requestResponse()
		return nil
	}
	startChat := func(command realtimeChatCommand) error {
		activeChat = &command
		watchdog.stop()
		logging.Infof("agent", "realtime", "Chat started: request_id=%s", command.request.RequestID)
		chatMode = ""
		chatText.Reset()
		chatTranscript.Reset()
		if err := appendRealtimeUserMessage(userContext, command.request.Message); err != nil {
			return fmt.Errorf("persist realtime chat: %w", err)
		}
		if err := textSession.SendText(command.ctx, command.request.Message); err != nil {
			return fmt.Errorf("send realtime chat: %w", err)
		}
		requestResponse()
		if err := textSession.CreateResponse(command.ctx); err != nil {
			// The write may have partially reached the provider. Only closing
			// the session prevents a delayed answer from owning the next chat.
			return fmt.Errorf("create realtime chat response: %w", err)
		}
		return nil
	}
	tryStartQueuedChat := func() error {
		if queuedChat == nil || realtimeAdmissionSpeechActive(admissionTurnEndpoint) {
			return nil
		}
		if !turnState.canInjectResponse() {
			// Providers without speech boundary events still use the local gate
			// as an admission guard. Once that synthetic gate has gone quiet, a
			// queued chat must retain the pre-existing terminal-event behavior;
			// there is no provider transcript or client-side commit to wait for.
			if info.Capabilities.EmitsSpeechEvents || clientTurnEndpoint != nil || turnState.inputSpeechActive || turnState.inputTurnTranscriptSeen || turnState.responseActive || turnState.responseTerminalPending {
				return nil
			}
		}
		command := *queuedChat
		queuedChat = nil
		return startChat(command)
	}
	sessionErrors := session.Errors()
	// The event stream is the authoritative completion signal. Providers may
	// close Done before the final buffered transcript or response event is read.
	sessionEvents, realtimeReengagement := relayRealtimeSessionEvents(ctx, session.Events())
	observeProviderUserTranscript := func() {
		consumeRealtimeReengagement(realtimeReengagement)
		if turnState.userTranscriptObserved() && sleep.abandon() {
			logging.Infof("agent", "realtime", "Standby canceled by provider user transcript")
		}
	}
	for {
		watchdog.setBusy(cancelPending || activeChat != nil || turnState.responseActive || turnState.responseRequestPending || turnState.responseTerminalPending || len(foregroundTools) > 0 ||
			(turnState.inputTurnPending && !turnState.inputSpeechActive))
		select {
		case <-watchdog.deadline:
			logging.Errorf("agent", "realtime", "Response timeout: active_request_id=%s response_id=%s occupied_ms=%d idle_ms=%d active_chat=%t response_active=%t input_pending=%t tools=%d",
				realtimeChatRequestID(activeChat), turnState.responseID, watchdog.age().Milliseconds(), time.Since(watchdog.lastProgress).Milliseconds(),
				activeChat != nil, turnState.responseActive, turnState.inputTurnPending, len(foregroundTools))
			return fmt.Errorf("realtime response timed out: no progress for %s; session closed", idleTimeout)
		case <-cancelAckDeadline:
			return fmt.Errorf("realtime interruption acknowledgement timed out; session closed")
		case <-ctx.Done():
			return nil
		case <-sigChan:
			return errRealtimeShutdown
		case err, ok := <-sessionErrors:
			if !ok {
				sessionErrors = nil
				continue
			}
			if err != nil {
				if semanticEmitter != nil {
					semanticEmitter.emitError(err)
				}
				return markRealtimeProviderFailure(err)
			}
		case err := <-readErrs:
			if err != nil {
				if semanticEmitter != nil {
					semanticEmitter.emitError(err)
				}
				return err
			}
		case <-voiceNotificationTicker.C:
			if err := tryInjectVoiceNotification(); err != nil {
				return err
			}
		case err := <-sleep.draining():
			sleep.stopDrain()
			if abandonSleepForPendingRealtimeReengagement(&sleep, realtimeReengagement) {
				logging.Infof("agent", "realtime", "Standby canceled by pending user re-engagement")
				continue
			}
			if err != nil {
				logging.Warnf("agent", "realtime", "farewell playback drain: %v", err)
			}
			logging.Infof("agent", "realtime", "Conversation ended by request, returning to standby")
			return nil
		case err := <-notificationDrainDone:
			notificationDrainDone = nil
			if notificationDrainCancel != nil {
				notificationDrainCancel()
				notificationDrainCancel = nil
			}
			if activeNotificationToken == "" {
				continue
			}
			if err != nil {
				runtime.ReportSpokenTextDelivery(activeNotificationToken, err)
				if stopErr := playback.stop(playbackAudio); stopErr != nil {
					return fmt.Errorf("stop failed realtime notification playback: %w", stopErr)
				}
			} else {
				runtime.ReportSpokenTextDelivery(activeNotificationToken, nil)
			}
			activeNotificationToken = ""
			activeNotificationResponseID = ""
			activeNotificationAudioWritten = false
			if err := tryInjectVoiceNotification(); err != nil {
				return err
			}
			if err := tryInjectTaskUpdates(); err != nil {
				return err
			}
		case result := <-toolResults:
			call := result.call
			tool, pending := foregroundTools[call.CallID]
			if !pending {
				// A canceled tool may race its result into the channel. Do not
				// send it to the provider or renew the next response's deadline.
				continue
			}
			delete(foregroundTools, call.CallID)
			tool.cancel()
			if !canSendToolResult {
				return fmt.Errorf("realtime provider %s cannot send tool results", providerName)
			}
			if err := toolResultSender.SendToolResult(ctx, call.CallID, result.output); err != nil {
				return markRealtimeProviderFailure(fmt.Errorf("send realtime tool result: %w", err))
			}
			watchdog.progress()
			if activeNotificationToken == "" && !suppressedNotificationResponsePending && !hasSuppressedNotificationResponse(suppressedNotificationResponseIDs, call.ResponseID) {
				if err := appendRealtimeToolExecution(userContext, call, result.output); err != nil {
					return fmt.Errorf("persist realtime tool call and result: %w", err)
				}
				if call.Name == realtimeEndConversationTool {
					sleep.request()
				}
			}
			if info.Capabilities.ExplicitToolContinuation && toolTracker.complete(call.ResponseID) {
				requestResponse()
				if err := textSession.CreateResponse(ctx); err != nil {
					return markRealtimeProviderFailure(fmt.Errorf("continue realtime response after tool call: %w", err))
				}
			}
		case pcm, ok := <-chunks:
			if !ok {
				if err := commitClientTurn(); err != nil {
					return err
				}
				return nil
			}
			if err := session.SendAudio(ctx, pcm); err != nil {
				return markRealtimeProviderFailure(err)
			}
			if admissionTurnEndpoint != nil && shouldTrackRealtimeAdmissionSpeech(&turnState, realtimeChatPending(chatBridge, queuedChat), info.Capabilities) {
				now := time.Now()
				started, stopped := observeRealtimeAdmissionSpeech(admissionTurnEndpoint, &turnState, info.Capabilities, pcm, now, clientTurnEndpoint == nil)
				if started && !info.Capabilities.ServerAuthoritativeTurnDetection {
					if sleep.abandon() {
						logging.Infof("agent", "realtime", "Standby canceled by local user speech")
					}
				}
				if clientTurnEndpoint != nil && admissionTurnEndpoint.speechActive {
					armClientTurnCommitTimer()
				}
				if clientTurnEndpoint == nil && stopped {
					if err := tryStartQueuedChat(); err != nil {
						return err
					}
					if err := tryInjectTaskUpdates(); err != nil {
						return err
					}
				}
			}
			if firstAudioChunk {
				firstAudioChunk = false
				logging.Infof("agent", "realtime", "Audio streaming started: first_chunk_bytes=%d", len(pcm))
			}
		case <-playbackKeepAlive.C:
			// audio_service reaps sessions after 30 seconds without a write.
			// A zero-length non-final write keeps the speaker hardware open
			// without adding audible silence to the playback queue.
			if err := playback.keepAlive(playbackAudio, outputFormat); err != nil {
				return fmt.Errorf("keep realtime playback alive: %w", err)
			}
		case <-agentTaskNotifications(tasks):
			pendingTaskUpdates = append(pendingTaskUpdates, tasks.DrainTerminalTasks()...)
			taskUpdatesReady = false
			if taskDebounceTimer == nil {
				taskDebounceTimer = time.NewTimer(realtimeTaskResultDebounce)
			} else {
				if !taskDebounceTimer.Stop() {
					select {
					case <-taskDebounceTimer.C:
					default:
					}
				}
				taskDebounceTimer.Reset(realtimeTaskResultDebounce)
			}
			taskDebounce = taskDebounceTimer.C
		case <-agentTaskUserActionNotifications(tasks):
			for _, task := range tasks.DrainUserActionTasks() {
				pendingTaskUpdates = append(pendingTaskUpdates, task)
			}
			// User-action requests should be announced as soon as the
			// foreground is idle. The normal guards in tryInjectTaskUpdates
			// defer delivery while speech or a response is active.
			taskUpdatesReady = true
			if err := tryInjectTaskUpdates(); err != nil {
				return err
			}
		case <-taskDebounce:
			taskDebounce = nil
			taskUpdatesReady = true
			if err := tryInjectTaskUpdates(); err != nil {
				return err
			}
		case <-clientTurnCommitTimerCh:
			if clientTurnEndpoint != nil && clientTurnEndpoint.Due(time.Now()) {
				if err := commitClientTurn(); err != nil {
					return err
				}
				turnState.localSpeechStopped()
				if err := tryInjectTaskUpdates(); err != nil {
					return err
				}
			} else if clientTurnEndpoint != nil && clientTurnEndpoint.speechActive {
				armClientTurnCommitTimer()
			}
		case command := <-chatBridgeCommands(chatBridge):
			if !supportsText {
				sendRealtimeChatEvent(command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventError, Error: fmt.Sprintf("realtime provider %s does not support text injection", providerName)})
				close(command.events)
				continue
			}
			admission := (realtimeChatAdmissionState{
				activeChat:           activeChat != nil,
				queuedChat:           queuedChat != nil,
				responseActive:       turnState.responseActive,
				turnBlocked:          !turnState.canInjectResponse(),
				localSpeechActive:    realtimeAdmissionSpeechActive(admissionTurnEndpoint),
				notificationActive:   activeNotificationToken != "",
				notificationDraining: notificationDrainDone != nil,
				standbyPending:       sleep.pending(),
			}).admission()
			logging.Infof("agent", "realtime", "Chat admission: request_id=%s active_request_id=%s response_id=%s occupied_ms=%d active_chat=%t queued_chat=%t response_active=%t terminal_pending=%t can_inject=%t input_speech=%t input_pending=%t local_speech=%t notification=%t draining=%t standby=%t admission=%d",
				command.request.RequestID, realtimeChatRequestID(activeChat), turnState.responseID, watchdog.age().Milliseconds(),
				activeChat != nil, queuedChat != nil, turnState.responseActive, turnState.responseTerminalPending, turnState.canInjectResponse(),
				turnState.inputSpeechActive, turnState.inputTurnPending, realtimeAdmissionSpeechActive(admissionTurnEndpoint), activeNotificationToken != "", notificationDrainDone != nil, sleep.pending(), admission)
			if admission == realtimeChatRejectBusy {
				logging.Warnf("agent", "realtime", "Chat rejected busy: request_id=%s active_request_id=%s", command.request.RequestID, realtimeChatRequestID(activeChat))
				sendRealtimeChatEvent(command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventError, Error: "realtime response is busy"})
				close(command.events)
				continue
			}
			if sleep.abandon() {
				logging.Infof("agent", "realtime", "Standby canceled by text request")
			}
			if admission == realtimeChatQueueAfterResponse {
				queuedChat = &command
				continue
			}
			if err := startChat(command); err != nil {
				return err
			}
		case <-chatCommandDone(queuedChat):
			sendRealtimeChatEvent(*queuedChat, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventError, Error: "request canceled"})
			close(queuedChat.events)
			queuedChat = nil
		case <-chatCommandDone(activeChat):
			if !canInterrupt {
				return fmt.Errorf("realtime provider %s cannot cancel response; session closed: %w", providerName, context.Canceled)
			}
			if err := responseInterrupter.Interrupt(ctx, playback.responseInterruption(outputFormat)); err != nil {
				return fmt.Errorf("cancel realtime response; session closed: %w", err)
			}
			if semanticEmitter != nil {
				semanticEmitter.invalidateOutput()
			}
			if err := playback.interrupt(playbackAudio, outputFormat); err != nil {
				return fmt.Errorf("stop canceled realtime playback: %w", err)
			}
			logging.Infof("agent", "realtime", "Chat canceled: request_id=%s response_id=%s; awaiting terminal acknowledgement", realtimeChatRequestID(activeChat), turnState.responseID)
			cancelPending = true
			startCancelAckTimer()
			cancelForegroundTools(turnState.responseID)
			turnState.responseInterrupted()
			turnState.responseTerminalPending = true
			watchdog.progress()
			close(activeChat.events)
			activeChat = nil
		case event, ok := <-sessionEvents:
			if !ok {
				if err := realtimeSessionEventClosureError(sessionErrors); err != nil {
					if semanticEmitter != nil {
						semanticEmitter.emitError(err)
					}
					return err
				}
				return nil
			}
			// Late output must neither resurrect a canceled request nor keep its
			// acknowledgement deadline alive. Input events remain observable.
			if cancelPending && (event.Kind == realtimevoice.EventResponseStarted || event.Kind == realtimevoice.EventAudio || event.Kind == realtimevoice.EventToolCall ||
				((event.Kind == realtimevoice.EventTranscriptDelta || event.Kind == realtimevoice.EventTranscriptFinal) && event.Role == "assistant")) {
				continue
			}
			switch event.Kind {
			case realtimevoice.EventReady:
				// Providers may emit a ready frame after Open; negotiated rates
				// are already available in Session.Info(). The synthetic ready
				// event above keeps the lifecycle observable for providers that omit
				// this frame, so do not publish a duplicate here.
			case realtimevoice.EventSpeechStarted:
				consumeRealtimeReengagement(realtimeReengagement)
				if semanticEmitter != nil {
					semanticEmitter.invalidateOutput()
				}
				turnState.speechStarted()
				if sleep.abandon() {
					logging.Infof("agent", "realtime", "Standby canceled by new user speech")
				}
				if activeNotificationToken != "" {
					if notificationDrainDone == nil {
						markSuppressedRealtimeNotificationResponse(activeNotificationResponseID, &suppressedNotificationResponsePending, suppressedNotificationResponseIDs)
					}
					runtime.ReportSpokenTextDelivery(activeNotificationToken, context.Canceled)
					if notificationDrainCancel != nil {
						notificationDrainCancel()
						notificationDrainCancel = nil
					}
					notificationDrainDone = nil
					activeNotificationToken = ""
					activeNotificationResponseID = ""
					activeNotificationAudioWritten = false
				}
				logging.Infof("agent", "realtime", "Input speech started: provider=%s session_id=%s item_id=%s audio_start_ms=%d; interrupting local playback", providerName, info.ID, event.ItemID, event.AudioStartMS)
				if canInterrupt {
					interruption := playback.responseInterruption(outputFormat)
					interruption.ServerDetected = providerName == realtimevoice.ProviderQwen
					if err := interruptRealtimeResponse(ctx, turnState.responseActive, responseInterrupter, interruption); err != nil {
						return markRealtimeProviderFailure(fmt.Errorf("interrupt realtime provider response: %w", err))
					}
				}
				// Clear the queued response immediately, then reopen the speaker
				// session so mic and speaker remain active for the next turn.
				if err := playback.interrupt(playbackAudio, outputFormat); err != nil {
					return fmt.Errorf("interrupt realtime playback: %w", err)
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventSpeechStopped:
				turnState.speechStopped(event.Status)
				logging.Infof("agent", "realtime", "Input speech stopped: provider=%s session_id=%s item_id=%s audio_end_ms=%d status=%s", providerName, info.ID, event.ItemID, event.AudioEndMS, event.Status)
				if event.Status == "turn_invalid" {
					if err := restoreRealtimePlaybackAfterInvalidTurn(&turnState, &playback, playbackAudio, outputFormat); err != nil {
						return fmt.Errorf("restore realtime playback after invalid turn: %w", err)
					}
				}
				if clientTurnEndpoint != nil {
					if err := commitClientTurn(); err != nil {
						return err
					}
				}
				if err := tryStartQueuedChat(); err != nil {
					return err
				}
				if err := tryInjectTaskUpdates(); err != nil {
					return err
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventInputCommitted:
				// Server VAD already owns the commit. This event is diagnostic only
				// and must not commit the buffer or change response admission again.
				logging.Infof("agent", "realtime", "Input audio committed: provider=%s session_id=%s item_id=%s previous_item_id=%s", providerName, info.ID, event.ItemID, event.PreviousItemID)
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventTranscriptFinal:
				if event.Role == "user" {
					observeProviderUserTranscript()
					logging.Infof("agent", "realtime", "User transcript: provider=%s session_id=%s item_id=%s sequence=%d status=completed transcript_len=%d", providerName, info.ID, event.ItemID, event.Sequence, len([]rune(strings.TrimSpace(event.Text))))
					if err := appendRealtimeUserMessage(userContext, event.Text); err != nil {
						return fmt.Errorf("persist realtime user transcript: %w", err)
					}
					if semanticEmitter != nil {
						semanticEmitter.emit(event)
					}
				} else if event.Role == "assistant" {
					if !turnState.acceptsResponseEvent(event.ResponseID) {
						logging.Warnf("agent", "realtime", "Ignoring stale assistant transcript: response_id=%s active_response_id=%s", event.ResponseID, turnState.responseID)
						continue
					}
					if event.Text != "" {
						watchdog.progress()
					}
					if recordRealtimeFinalTranscript(event.Text, &responseTranscript, &chatText, &chatTranscript) && activeChat != nil {
						chatMode = "transcript"
						sendRealtimeChatEvent(*activeChat, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDelta, Delta: event.Text})
					}
					if semanticEmitter != nil {
						semanticEmitter.emit(event)
					}
				}
			case realtimevoice.EventResponseStarted:
				if !turnState.responseStarted(event.ResponseID) {
					logging.Warnf("agent", "realtime", "Ignoring stale response.created: response_id=%s input_turn=%d request_turn=%d", event.ResponseID, turnState.inputTurnSequence, turnState.responseRequestTurn)
					continue
				}
				if semanticEmitter != nil {
					semanticEmitter.beginResponse(event.ResponseID)
					semanticEmitter.emit(event, semanticEmitter.outputEpoch)
				}
				if info.Capabilities.ServerAuthoritativeTurnDetection {
					admissionTurnEndpoint.Reset()
				}
				logging.Infof("agent", "realtime", "Response created: provider=%s session_id=%s request_id=%s response_id=%s", providerName, info.ID, realtimeChatRequestID(activeChat), event.ResponseID)
				responseSuppressed = bindSuppressedRealtimeNotificationResponse(event.ResponseID, &suppressedNotificationResponsePending, suppressedNotificationResponseIDs)
				if activeNotificationToken != "" && activeNotificationResponseID == "" && !responseSuppressed {
					activeNotificationResponseID = event.ResponseID
				}
				responseText.Reset()
				responseTranscript.Reset()
				assistantPersisted = false
				// Ignore any late deltas from the interrupted response until the
				// server explicitly starts the next one. The speaker session stays
				// open across responses.
				if err := playback.beginResponse(playbackAudio, outputFormat); err != nil {
					return fmt.Errorf("begin realtime playback response: %w", err)
				}
			case realtimevoice.EventTranscriptDelta:
				if event.Role != "assistant" {
					if event.Role == "user" {
						observeProviderUserTranscript()
						if semanticEmitter != nil {
							semanticEmitter.emit(event)
						}
					}
					continue
				}
				if !turnState.acceptsResponseEvent(event.ResponseID) {
					logging.Warnf("agent", "realtime", "Ignoring stale assistant transcript delta: response_id=%s active_response_id=%s", event.ResponseID, turnState.responseID)
					continue
				}
				if event.Text != "" {
					watchdog.progress()
				}
				if !supportsText && (!turnState.responseActive || playback.suppressDeltas) {
					if err := ensureImplicitRealtimeResponse(&turnState.responseActive, &assistantPersisted, &responseText, &responseTranscript, &playback, playbackAudio, outputFormat); err != nil {
						return err
					}
					turnState.responseOutputObserved(event.ResponseID)
				}
				if event.TextSource == "text" {
					responseText.WriteString(event.Text)
					if activeChat != nil {
						if chatMode == "" {
							chatMode = "text"
						}
						if chatMode == "text" && event.Text != "" {
							chatText.WriteString(event.Text)
							sendRealtimeChatEvent(*activeChat, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDelta, Delta: event.Text})
						}
					}
				} else {
					responseTranscript.WriteString(event.Text)
					if activeChat != nil {
						if chatMode == "" {
							chatMode = "transcript"
						}
						if chatMode == "transcript" && event.Text != "" {
							chatTranscript.WriteString(event.Text)
							sendRealtimeChatEvent(*activeChat, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDelta, Delta: event.Text})
						}
					}
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventTranscriptFailed:
				logging.Errorf("agent", "realtime", "User transcript: provider=%s session_id=%s item_id=%s status=failed error=%q", providerName, info.ID, event.ItemID, event.Error)
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventAudio:
				if !turnState.acceptsResponseEvent(event.ResponseID) {
					logging.Warnf("agent", "realtime", "Ignoring stale response audio: response_id=%s active_response_id=%s", event.ResponseID, turnState.responseID)
					continue
				}
				if responseSuppressed || suppressedNotificationResponsePending || hasSuppressedNotificationResponse(suppressedNotificationResponseIDs, event.ResponseID) {
					continue
				}
				if !supportsText {
					if err := ensureImplicitRealtimeResponse(&turnState.responseActive, &assistantPersisted, &responseText, &responseTranscript, &playback, playbackAudio, outputFormat); err != nil {
						return err
					}
					turnState.responseOutputObserved(event.ResponseID)
				}
				pcm := event.PCM
				if len(pcm) > 0 {
					watchdog.progress()
				}
				if err := playback.appendItem(playbackAudio, outputFormat, event.ItemID, pcm); err != nil {
					return err
				}
				if activeNotificationToken != "" {
					activeNotificationAudioWritten = activeNotificationAudioWritten || len(pcm) > 0
				}
			case realtimevoice.EventToolCall:
				if !turnState.acceptsResponseEvent(event.ResponseID) {
					logging.Warnf("agent", "realtime", "Ignoring stale response tool call: response_id=%s active_response_id=%s", event.ResponseID, turnState.responseID)
					continue
				}
				logging.Infof("agent", "realtime", "Tool call: name=%s provider=%s session_id=%s request_id=%s response_id=%s item_id=%s call_id=%s", event.Name, providerName, info.ID, realtimeChatRequestID(activeChat), event.ResponseID, event.ItemID, event.CallID)
				toolCtx, cancelTool := context.WithCancel(ctx)
				foregroundTools[event.CallID] = foregroundTool{responseID: event.ResponseID, cancel: cancelTool}
				watchdog.progress()
				if info.Capabilities.ExplicitToolContinuation {
					toolTracker.start(event.ResponseID)
				}
				if activeNotificationToken != "" || suppressedNotificationResponsePending || hasSuppressedNotificationResponse(suppressedNotificationResponseIDs, event.ResponseID) {
					startRealtimeSuppressedToolCall(toolCtx, event, toolResults)
				} else {
					realtimeToolCallStarter(toolCtx, toolExecutor, event, toolResults)
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventToolCallCancelled:
				if !turnState.acceptsResponseEvent(event.ResponseID) {
					continue
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
				tool, pending := foregroundTools[event.CallID]
				if !pending {
					continue
				}
				tool.cancel()
				delete(foregroundTools, event.CallID)
				// A tool-level cancellation does not interrupt the response or
				// discard other tools that the provider is still waiting for.
				if info.Capabilities.ExplicitToolContinuation && toolTracker.complete(tool.responseID) {
					requestResponse()
					if err := textSession.CreateResponse(ctx); err != nil {
						return fmt.Errorf("continue realtime response after tool cancellation: %w", err)
					}
				}
			case realtimevoice.EventUsage:
				realtimeResponseUsage.TotalTokens += event.Usage.TotalTokens
				realtimeResponseUsage.InputTokens += event.Usage.InputTokens
				realtimeResponseUsage.OutputTokens += event.Usage.OutputTokens
				hasRealtimeResponseUsage = true
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventResponseDone, realtimevoice.EventResponseCancelled:
				if !turnState.responseFinished(event.ResponseID) {
					logging.Warnf("agent", "realtime", "Ignoring stale terminal response event: response_id=%s active_response_id=%s", event.ResponseID, turnState.responseID)
					continue
				}
				if semanticEmitter != nil {
					// Terminal events describe the response outcome even when the
					// response was invalidated by a user interruption. They are
					// lifecycle/control events, so do not attach the old output epoch.
					semanticEmitter.emit(event, 0)
				}
				wasCanceled := cancelPending || event.Kind == realtimevoice.EventResponseCancelled
				cancelPending = false
				stopCancelAckTimer()
				if wasCanceled {
					cancelForegroundTools(event.ResponseID)
					if event.ResponseID != "" {
						toolTracker.clear(event.ResponseID)
					}
				}
				logging.Infof("agent", "realtime", "Response terminal: provider=%s session_id=%s request_id=%s response_id=%s kind=%s status=%s occupied_ms=%d",
					providerName, info.ID, realtimeChatRequestID(activeChat), event.ResponseID, event.Kind, event.Status, watchdog.age().Milliseconds())
				if suppressedNotificationResponsePending && event.ResponseID != "" {
					bindSuppressedRealtimeNotificationResponse(event.ResponseID, &suppressedNotificationResponsePending, suppressedNotificationResponseIDs)
				}
				suppressedResponse := hasSuppressedNotificationResponse(suppressedNotificationResponseIDs, event.ResponseID)
				notificationResponse := activeNotificationToken != "" && (activeNotificationResponseID == "" || activeNotificationResponseID == event.ResponseID)
				if suppressedResponse {
					delete(suppressedNotificationResponseIDs, event.ResponseID)
					if event.ResponseID != "" {
						toolTracker.clear(event.ResponseID)
					}
				}
				if event.Usage.TotalTokens > 0 || event.Usage.InputTokens > 0 || event.Usage.OutputTokens > 0 {
					realtimeResponseUsage.TotalTokens += event.Usage.TotalTokens
					realtimeResponseUsage.InputTokens += event.Usage.InputTokens
					realtimeResponseUsage.OutputTokens += event.Usage.OutputTokens
					hasRealtimeResponseUsage = true
				}
				if !suppressedResponse && !notificationResponse {
					if err := playback.finishResponse(playbackAudio); err != nil {
						return fmt.Errorf("finish realtime playback response: %w", err)
					}
				}
				if !suppressedResponse && !notificationResponse && !wasCanceled {
					if hasTools, continueNow := toolTracker.done(event.ResponseID); hasTools {
						if !continueNow {
							continue
						}
						requestResponse()
						if !supportsText {
							continue
						}
						if err := textSession.CreateResponse(ctx); err != nil {
							return markRealtimeProviderFailure(fmt.Errorf("continue realtime response after tool call: %w", err))
						}
						continue
					}
				}
				if !notificationResponse && !suppressedResponse && !assistantPersisted && !wasCanceled && (event.Status == "" || event.Status == "completed") {
					assistantText := strings.TrimSpace(responseText.String())
					if assistantText == "" {
						assistantText = strings.TrimSpace(responseTranscript.String())
					}
					if assistantText != "" {
						var usage *messages.Usage
						if hasRealtimeResponseUsage {
							usageCopy := realtimeResponseUsage
							usage = &usageCopy
						}
						if err := appendRealtimeAssistantMessage(userContext, assistantText, usage); err != nil {
							return fmt.Errorf("persist realtime assistant response: %w", err)
						}
					}
				}
				realtimeResponseUsage = messages.Usage{}
				hasRealtimeResponseUsage = false
				assistantPersisted = false
				if activeChat != nil {
					logging.Infof("agent", "realtime", "Chat completed: request_id=%s occupied_ms=%d", activeChat.request.RequestID, watchdog.age().Milliseconds())
					content := chatText.String()
					if content == "" {
						content = chatTranscript.String()
					}
					sendRealtimeChatEvent(*activeChat, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventDone, Response: content})
					close(activeChat.events)
					activeChat = nil
				}
				if err := tryStartQueuedChat(); err != nil {
					return err
				}
				if err := tryInjectTaskUpdates(); err != nil {
					return err
				}
				if notificationResponse {
					var deliveryErr error
					if event.Kind == realtimevoice.EventResponseCancelled || (event.Status != "" && event.Status != "completed") {
						deliveryErr = fmt.Errorf("realtime notification response ended with status %q", event.Status)
					} else if !activeNotificationAudioWritten {
						deliveryErr = errors.New("realtime notification response produced no audio")
					}
					if deliveryErr == nil {
						if err := playback.finishResponse(playbackAudio, true); err != nil {
							deliveryErr = fmt.Errorf("finish realtime notification playback: %w", err)
						}
					}
					if deliveryErr != nil {
						runtime.ReportSpokenTextDelivery(activeNotificationToken, deliveryErr)
						if stopErr := playback.stop(playbackAudio); stopErr != nil {
							return fmt.Errorf("stop failed realtime notification playback: %w", stopErr)
						}
						activeNotificationToken = ""
						activeNotificationResponseID = ""
						activeNotificationAudioWritten = false
					} else {
						drainCtx, cancel := context.WithTimeout(ctx, realtimeNotificationDrain)
						notificationDrainCancel = cancel
						doneCh := make(chan error, 1)
						notificationDrainDone = doneCh
						go func() { doneCh <- playbackAudio.WaitForPlaybackDrain(drainCtx) }()
					}
				}
				if sleep.shouldDrain() && activeNotificationToken == "" && notificationDrainDone == nil {
					if err := playback.finishResponse(playbackAudio, true); err != nil {
						logging.Warnf("agent", "realtime", "finalize farewell playback: %v", err)
						logging.Infof("agent", "realtime", "Conversation ended by request, returning to standby")
						return nil
					}
					drainCtx, cancel := context.WithTimeout(ctx, realtimeSleepDrain)
					doneCh := make(chan error, 1)
					sleep.startDrain(doneCh, cancel)
					go func() { doneCh <- playbackAudio.WaitForPlaybackDrain(drainCtx) }()
				}
				responseSuppressed = false
				if err := tryInjectVoiceNotification(); err != nil {
					return err
				}
				if err := tryInjectTaskUpdates(); err != nil {
					return err
				}
			case realtimevoice.EventInterruption:
				consumeRealtimeReengagement(realtimeReengagement)
				sleep.abandon()
				turnState.responseInterrupted()
				if semanticEmitter != nil {
					semanticEmitter.invalidateOutput()
				}
				cancelForegroundTools(event.ResponseID)
				// Use the current response ID if the event didn't provide one
				responseIDForCleanup := event.ResponseID
				if responseIDForCleanup == "" {
					responseIDForCleanup = turnState.responseID
				}
				if responseIDForCleanup != "" {
					toolTracker.clear(responseIDForCleanup)
				}
				if err := playback.interrupt(playbackAudio, outputFormat); err != nil {
					return err
				}
				if semanticEmitter != nil {
					semanticEmitter.emit(event)
				}
			case realtimevoice.EventError:
				if event.Error != nil {
					if semanticEmitter != nil {
						semanticEmitter.emit(event)
					}
					return markRealtimeProviderFailure(event.Error)
				}
			}
		}
	}
}

func shouldFailQueuedRealtimeChat(err error) bool {
	return !errors.Is(err, realtimevoice.ErrSessionRotated)
}

func failActiveRealtimeChat(command *realtimeChatCommand, err error) {
	if command == nil {
		return
	}
	message := "realtime voice session ended"
	if err != nil {
		message = err.Error()
	}
	sendRealtimeChatEvent(*command, agent.RealtimeChatEvent{Type: agent.RealtimeChatEventError, Error: message})
	close(command.events)
}

func realtimeSessionEventClosureError(errs <-chan error) error {
	err := realtimeSessionTerminationError(errs)
	if err == nil {
		return nil
	}
	return markRealtimeProviderFailure(err)
}

func realtimeSessionTerminationError(errs <-chan error) error {
	if errs == nil {
		return nil
	}
	select {
	case err, ok := <-errs:
		if ok && err != nil {
			return err
		}
	default:
	}
	return nil
}

type realtimeTurnState struct {
	responseActive          bool
	responseTerminalPending bool
	responseID              string
	responseRequestPending  bool
	responseRequestTurn     uint64
	anonymousResponseStale  bool
	retiredResponseIDs      map[string]struct{}
	retiredResponseOrder    []string
	inputSpeechActive       bool
	inputTurnPending        bool
	// Keep interrupted output suppressed independently of input admission.
	bargedInResponseID      string
	bargedInAnonymous       bool
	inputTurnSequence       uint64
	inputTurnTranscriptSeen bool
}

func (s *realtimeTurnState) canInjectResponse() bool {
	return !s.responseActive && !s.responseTerminalPending && !s.inputSpeechActive && !s.inputTurnPending
}

func (s *realtimeTurnState) responseRequested() {
	s.responseTerminalPending = false
	s.responseRequestPending = true
	s.responseRequestTurn = s.inputTurnSequence
	s.anonymousResponseStale = false
	s.retireResponseID(s.responseID)
	s.responseActive = true
	s.responseID = ""
}

func (s *realtimeTurnState) speechStarted() {
	if !s.inputSpeechActive {
		s.inputTurnSequence++
	}
	s.inputSpeechActive = true
	s.inputTurnPending = true
	if s.responseActive || s.responseTerminalPending {
		if s.responseID != "" {
			s.bargedInResponseID = s.responseID
		} else {
			s.bargedInAnonymous = true
		}
	}
	s.inputTurnTranscriptSeen = false
}

// speechStopped releases input admission; an invalid turn also restores interrupted output.
func (s *realtimeTurnState) speechStopped(status string) {
	s.inputSpeechActive = false
	s.inputTurnPending = false
	if status == "turn_invalid" {
		// The provider rejected this speech as a turn, so roll back the
		// sequence and leave any response that was interrupted eligible to
		// resume.
		if s.inputTurnSequence > 0 {
			s.inputTurnSequence--
		}
		s.bargedInResponseID = ""
		s.bargedInAnonymous = false
	}
}

func (s *realtimeTurnState) localSpeechStopped() {
	s.inputSpeechActive = false
	// If a response already started while the local gate was active, that
	// response consumed the input turn. Do not create a second pending turn when
	// the trailing silence timer fires.
	if !s.responseActive {
		s.inputTurnPending = true
	}
}

func (s *realtimeTurnState) userTranscriptObserved() bool {
	if s.responseActive && !s.inputTurnPending {
		// A provider may finish refining the input transcript after it has
		// already started the answer. That refinement belongs to the turn that
		// produced the current response, not to a new pending user turn.
		return false
	}
	// A transcript is the first provider-neutral marker of a new turn for
	// adapters whose response events do not carry IDs (Gemini Live).
	s.anonymousResponseStale = false
	s.inputTurnTranscriptSeen = true
	if !s.inputTurnPending {
		s.inputTurnSequence++
	}
	s.inputTurnPending = true
	return true
}

// responseStarted returns false when the event belongs to a response request
// that was superseded by a newer speech turn. The caller must discard that
// response's output and wait for the response created for the current turn.
func (s *realtimeTurnState) responseStarted(responseID string) bool {
	if responseID != "" && responseID == s.responseID && !s.responseRequestPending {
		// A duplicate response.created for an already identified response must
		// not consume a newer input turn that arrived while this response was
		// producing output.
		return false
	}
	staleRequest := s.responseRequestPending && s.responseRequestTurn < s.inputTurnSequence
	// Gemini Live does not attach a response ID to model turns. Once a
	// transcript for the new input turn has arrived, an anonymous response is
	// the provider's answer to that turn rather than the delayed old request.
	if staleRequest && responseID == "" && s.inputTurnTranscriptSeen {
		staleRequest = false
	}
	s.responseRequestPending = false
	s.responseTerminalPending = false
	if staleRequest {
		s.retireResponseID(responseID)
		s.anonymousResponseStale = responseID == ""
		s.responseActive = false
		s.responseID = ""
		return false
	}
	s.retireResponseID(s.bargedInResponseID)
	s.responseActive = true
	s.responseID = responseID
	s.bargedInResponseID = ""
	s.bargedInAnonymous = false
	s.anonymousResponseStale = false
	s.inputTurnPending = false
	return true
}

func (s *realtimeTurnState) responseOutputObserved(responseID string) {
	s.responseActive = true
	if responseID != "" {
		s.responseID = responseID
	}
	s.inputTurnPending = false
}

func (s *realtimeTurnState) acceptsResponseEvent(responseID string) bool {
	if responseID == "" {
		return !s.anonymousResponseStale && !s.bargedInAnonymous
	}
	if s.isRetiredResponseID(responseID) {
		return false
	}
	if s.bargedInResponseID == responseID {
		return false
	}
	if s.responseID != "" && s.responseID != responseID {
		return false
	}
	// Once a new input turn has started, output carrying the previous response
	// ID is late output from the interrupted answer. It must not leak into chat
	// text or the assistant transcript while playback is suppressed.
	if s.responseID == responseID && s.inputTurnPending {
		return false
	}
	return true
}

func (s *realtimeTurnState) responseFinished(responseID string) bool {
	if responseID == "" && s.anonymousResponseStale {
		return false
	}
	if s.isRetiredResponseID(responseID) {
		return false
	}
	if responseID == "" && !s.responseActive && !s.responseTerminalPending && s.responseID == "" && s.inputTurnPending {
		// Gemini can complete an input turn without emitting a separate
		// response.created event when no model output was produced.
		s.inputTurnPending = false
		return true
	}
	// An interrupted response may still deliver its terminal event. Keep that
	// event eligible for the old response cleanup until another response is
	// requested, which retires the old ID above.
	if !s.responseActive && !s.responseTerminalPending && s.responseID == "" {
		return false
	}
	if s.responseID != "" && responseID != "" && responseID != s.responseID {
		return false
	}
	s.retireResponseID(s.responseID)
	s.retireResponseID(responseID)
	s.responseActive = false
	s.responseTerminalPending = false
	s.responseRequestPending = false
	s.responseID = ""
	return true
}

func (s *realtimeTurnState) responseInterrupted() {
	if s.responseID != "" {
		s.bargedInResponseID = s.responseID
	} else if s.responseActive || s.responseTerminalPending {
		s.bargedInAnonymous = true
	}
	s.responseTerminalPending = s.responseTerminalPending || s.responseActive || s.responseID != ""
	s.responseActive = false
}

func (s *realtimeTurnState) isRetiredResponseID(responseID string) bool {
	if responseID == "" || len(s.retiredResponseIDs) == 0 {
		return false
	}
	_, ok := s.retiredResponseIDs[responseID]
	return ok
}

func (s *realtimeTurnState) retireResponseID(responseID string) {
	if responseID == "" {
		return
	}
	if s.retiredResponseIDs == nil {
		s.retiredResponseIDs = make(map[string]struct{})
	}
	if _, exists := s.retiredResponseIDs[responseID]; exists {
		return
	}
	if len(s.retiredResponseOrder) >= maxRetiredRealtimeResponseIDs {
		oldest := s.retiredResponseOrder[0]
		delete(s.retiredResponseIDs, oldest)
		s.retiredResponseOrder = s.retiredResponseOrder[1:]
	}
	s.retiredResponseIDs[responseID] = struct{}{}
	s.retiredResponseOrder = append(s.retiredResponseOrder, responseID)
}

func interruptRealtimeResponse(ctx context.Context, responseActive bool, interrupter realtimevoice.ResponseInterrupter, interruption realtimevoice.ResponseInterruption) error {
	if !responseActive || interrupter == nil {
		return nil
	}
	return interrupter.Interrupt(ctx, interruption)
}

// restoreRealtimePlaybackAfterInvalidTurn resumes an answer that server VAD
// kept alive after rejecting a short interruption as turn_invalid. The daemon
// has already stopped and suppressed playback on speech_started, so the next
// audio delta must reopen the low-latency playback session.
func restoreRealtimePlaybackAfterInvalidTurn(state *realtimeTurnState, playback *realtimePlaybackState, audio realtimePlaybackAudio, format agent.AudioFormat) error {
	if state == nil || playback == nil || !state.responseActive || !playback.suppressDeltas {
		return nil
	}
	return playback.beginResponse(audio, format)
}

func ensureImplicitRealtimeResponse(
	active *bool,
	assistantPersisted *bool,
	responseText *strings.Builder,
	responseTranscript *strings.Builder,
	playback *realtimePlaybackState,
	audio realtimePlaybackAudio,
	format agent.AudioFormat,
) error {
	if active == nil || assistantPersisted == nil || responseText == nil || responseTranscript == nil || playback == nil {
		return errors.New("realtime response state is unavailable")
	}
	if *active && !playback.suppressDeltas {
		return nil
	}
	*active = true
	*assistantPersisted = false
	responseText.Reset()
	responseTranscript.Reset()
	if err := playback.beginResponse(audio, format); err != nil {
		return err
	}
	return nil
}

func recordRealtimeFinalTranscript(text string, responseTranscript, chatText, chatTranscript *strings.Builder) bool {
	if responseTranscript == nil || chatText == nil || chatTranscript == nil {
		return false
	}
	if responseTranscript.Len() == 0 {
		responseTranscript.WriteString(text)
	}
	if chatText.Len() == 0 && chatTranscript.Len() == 0 && strings.TrimSpace(text) != "" {
		chatTranscript.WriteString(text)
		return true
	}
	return false
}

func agentTaskNotifications(tasks *agenttask.Manager) <-chan struct{} {
	if tasks == nil {
		return nil
	}
	return tasks.TerminalNotifications()
}

func agentTaskWakeNotifications(tasks *agenttask.Manager) <-chan struct{} {
	if tasks == nil {
		return nil
	}
	return tasks.WakeNotifications()
}

func agentTaskTerminalSequence(tasks *agenttask.Manager) uint64 {
	if tasks == nil {
		return 0
	}
	return tasks.TerminalSequence()
}

func agentTaskTerminalState(tasks *agenttask.Manager) ([]agenttask.Task, uint64) {
	if tasks == nil {
		return nil, 0
	}
	return tasks.TerminalState()
}

// taskWakeState decides whether a terminal task update should activate the
// foreground from standby. The watermark is the manager's terminal sequence,
// so a batch is announced once: a session that could not deliver an update
// restores it without re-activating in a loop, and the next user activation
// still picks the update up from the manager.
type taskWakeState struct {
	lastSeq uint64
}

func (s *taskWakeState) shouldActivate(seq uint64) bool {
	if seq == 0 || seq <= s.lastSeq {
		return false
	}
	s.lastSeq = seq
	return true
}

// observeSession records the terminal updates a session is about to consume,
// whether it goes on to deliver them or restores them on teardown.
func (s *taskWakeState) observeSession(seq uint64) {
	if seq > s.lastSeq {
		s.lastSeq = seq
	}
}

func agentTaskUserActionNotifications(tasks *agenttask.Manager) <-chan struct{} {
	if tasks == nil {
		return nil
	}
	return tasks.UserActionNotifications()
}

func chatBridgeHasPending(bridge *realtimeChatBridge) bool {
	return bridge != nil && len(bridge.commands) > 0
}

// realtimeChatPending reports whether an explicit /api/chat request is waiting
// to start: either still unread on the bridge, or already selected off the
// bridge and parked in queuedChat until the active response completes.
func realtimeChatPending(bridge *realtimeChatBridge, queued *realtimeChatCommand) bool {
	return chatBridgeHasPending(bridge) || queued != nil
}

func hasSuppressedNotificationResponse(responses map[string]struct{}, responseID string) bool {
	if responseID == "" {
		return false
	}
	_, ok := responses[responseID]
	return ok
}

func markSuppressedRealtimeNotificationResponse(responseID string, pending *bool, responses map[string]struct{}) {
	if responseID != "" {
		responses[responseID] = struct{}{}
		return
	}
	if pending != nil {
		*pending = true
	}
}

func bindSuppressedRealtimeNotificationResponse(responseID string, pending *bool, responses map[string]struct{}) bool {
	if pending != nil && *pending && responseID != "" {
		responses[responseID] = struct{}{}
		*pending = false
		return true
	}
	return hasSuppressedNotificationResponse(responses, responseID)
}

func formatRealtimeTaskUpdates(tasks []agenttask.Task) string {
	var output strings.Builder
	output.WriteString("Private work-status update for the assistant. Present these outcomes naturally as your own work; never mention agents, background execution, queues, orchestration, tools, or task IDs. For pending_user_action, explain what the user must do on the device, ask them to say when it is complete, and then continue using the internal response action:\n")
	for _, task := range tasks {
		fmt.Fprintf(&output, "- task_id=%s status=%s task=%q", task.ID, task.Status, limitRealtimeTaskField(task.Prompt, 300))
		if task.Result != "" {
			fmt.Fprintf(&output, " result=%q", limitRealtimeTaskField(task.Result, 2000))
		}
		if task.Error != "" {
			fmt.Fprintf(&output, " error=%q", limitRealtimeTaskField(task.Error, 500))
		}
		if task.PendingUserAction != nil {
			fmt.Fprintf(&output, " pending_user_action={reason:%q details:%q suggested_action:%q}",
				task.PendingUserAction.Reason,
				task.PendingUserAction.Details,
				task.PendingUserAction.SuggestedAction)
		}
		output.WriteByte('\n')
	}
	return strings.TrimSpace(output.String())
}

func appendRealtimeUserMessage(manager *contextmanager.ContextManager, content string) error {
	content = strings.TrimSpace(content)
	if manager == nil || content == "" {
		return nil
	}
	return manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: content})
}

func appendRealtimeNoticeMessage(manager *contextmanager.ContextManager, content string) error {
	content = strings.TrimSpace(content)
	if manager == nil || content == "" {
		return nil
	}
	return manager.AppendMessage(messages.Message{Role: messages.MessageRoleNotice, Content: content})
}

func appendRealtimeAssistantMessage(manager *contextmanager.ContextManager, content string, usage *messages.Usage) error {
	content = strings.TrimSpace(content)
	if manager == nil || content == "" {
		return nil
	}
	return manager.AppendMessage(messages.Message{Role: messages.MessageRoleAssistant, Content: content, Usage: usage})
}

// appendRealtimeToolExecution persists the protocol pair only after the tool
// has produced an output, so an interrupted call cannot remain in user context.
func appendRealtimeToolExecution(manager *contextmanager.ContextManager, call realtimevoice.Event, output string) error {
	if manager == nil {
		return nil
	}
	return manager.AppendMessages([]messages.Message{
		{
			Role: messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{
				ID:        strings.TrimSpace(call.CallID),
				Name:      strings.TrimSpace(call.Name),
				Arguments: strings.TrimSpace(call.Arguments),
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: strings.TrimSpace(call.CallID),
				Name:       strings.TrimSpace(call.Name),
				Content:    output,
			}},
		},
	})
}

func replayRealtimeContext(ctx context.Context, session realtimevoice.ContextReplayer, manager *contextmanager.ContextManager) error {
	if session == nil || manager == nil {
		return nil
	}
	var items []realtimevoice.ContextItem
	for _, message := range recentRealtimeContextMessages(manager.MessageListDump().Messages, realtimeContextReplayTurns) {
		content := strings.TrimSpace(message.Content)
		if message.Role == messages.MessageRoleSystem {
			continue
		}
		if message.Role == messages.MessageRoleToolCall {
			for _, call := range message.ToolCalls {
				items = append(items, realtimevoice.ContextItem{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: call.Arguments})
			}
			continue
		}
		if message.Role == messages.MessageRoleToolResult {
			for _, result := range message.ToolResults {
				items = append(items, realtimevoice.ContextItem{Type: "function_call_output", CallID: result.ToolCallID, Output: result.Content})
			}
			continue
		}
		if content == "" {
			continue
		}
		role := "user"
		if message.Role == messages.MessageRoleAssistant {
			role = "assistant"
		}
		items = append(items, realtimevoice.ContextItem{Type: "message", Role: role, Content: content})
	}
	return session.ReplayContext(ctx, items)
}

func recentRealtimeContextMessages(all []messages.Message, maxTurns int) []messages.Message {
	// A turn starts at a user message and owns every following assistant,
	// tool-call, and tool-result message until the next user message. Slice only
	// at these boundaries so a tool call can never be replayed without its
	// matching result (or vice versa).
	if len(all) == 0 || maxTurns <= 0 {
		return nil
	}
	userCount := 0
	for _, message := range all {
		if message.Role == messages.MessageRoleUser {
			userCount++
		}
	}
	if userCount <= maxTurns {
		return all
	}
	cutoff := userCount - maxTurns
	seen := 0
	start := len(all)
	for i, message := range all {
		if message.Role != messages.MessageRoleUser {
			continue
		}
		if seen == cutoff {
			start = i
			break
		}
		seen++
	}
	return all[start:]
}

// rotateRealtimeContextForReplay makes the durable session match the context
// restored into a new provider session. Keeping the shortened context under
// the old session ID would violate the invariant that one session identifies
// one conversation context.
func rotateRealtimeContextForReplay(manager *contextmanager.ContextManager, maxTurns int) (*contextmanager.ContextManager, error) {
	if manager == nil {
		return nil, nil
	}
	all := manager.MessageListDump().Messages
	recent := recentRealtimeContextMessages(all, maxTurns)
	if len(recent) == len(all) {
		return manager, nil
	}
	parentSessionID := manager.GetSessionID()
	retained := make([]messages.Message, 0, len(recent)+1)
	if len(all) > 0 && all[0].Role == messages.MessageRoleSystem {
		retained = append(retained, all[0])
	}
	retained = append(retained, recent...)
	revision, err := contextmanager.NewContextManagerRevisionFromMessageList(manager, retained)
	if err != nil {
		return nil, err
	}
	if err := manager.Activate(revision); err != nil {
		return nil, err
	}
	logging.Warnf("agent", "realtime", "Rotated user context after replay truncation: parent=%s session=%s retained_messages=%d",
		parentSessionID, revision.GetSessionID(), len(retained))
	return manager, nil
}

func limitRealtimeTaskField(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "…"
}

type realtimePlaybackAudio interface {
	StartPlayback(agent.AudioFormat) (*agent.PlaybackStartResult, error)
	WritePlayChunk(uint64, []byte, bool) error
	StopPlayback(uint64) error
}

type realtimePlaybackAudioAdapter struct {
	backend tts.AudioServiceBackend
}

type realtimePlaybackDrainWaiter interface {
	WaitForPlaybackDrain(context.Context) error
}

func (a realtimePlaybackAudioAdapter) StartPlayback(format agent.AudioFormat) (*agent.PlaybackStartResult, error) {
	if a.backend == nil {
		return nil, errors.New("realtime playback backend is unavailable")
	}
	sessionID, err := a.backend.StartPlayback(tts.AudioFormat{
		SampleRate: int(format.SampleRate),
		Channels:   int(format.Channels),
		BitWidth:   int(format.BitWidth),
	})
	if err != nil {
		return nil, err
	}
	return &agent.PlaybackStartResult{SessionID: sessionID}, nil
}

func (a realtimePlaybackAudioAdapter) WritePlayChunk(sessionID uint64, data []byte, final bool) error {
	return a.backend.WritePlayChunk(sessionID, data, final)
}

func (a realtimePlaybackAudioAdapter) StopPlayback(sessionID uint64) error {
	err := a.backend.StopPlayback(sessionID)
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "session_not_found") || strings.Contains(message, "session not found") {
		return nil
	}
	return err
}

func (a realtimePlaybackAudioAdapter) WaitForPlaybackDrain(ctx context.Context) error {
	if waiter, ok := a.backend.(realtimePlaybackDrainWaiter); ok {
		return waiter.WaitForPlaybackDrain(ctx)
	}
	return errors.New("realtime playback backend cannot confirm drain")
}

type realtimePlaybackState struct {
	session        *agent.PlaybackStartResult
	suppressDeltas bool
	finalized      bool
	itemID         string
	submittedBytes int64
	itemStartedAt  time.Time
	now            func() time.Time
	// Host players consume a finalized WAV response, while audio_service keeps
	// one low-latency PCM session open across responses.
	finalizeResponses bool
}

func (p *realtimePlaybackState) open(audio realtimePlaybackAudio, format agent.AudioFormat) error {
	if p.session != nil {
		return nil
	}
	session, err := audio.StartPlayback(format)
	if err != nil {
		return err
	}
	p.session = session
	return nil
}

func (p *realtimePlaybackState) append(audio realtimePlaybackAudio, format agent.AudioFormat, pcm []byte) error {
	return p.appendItem(audio, format, "", pcm)
}

func (p *realtimePlaybackState) appendItem(audio realtimePlaybackAudio, format agent.AudioFormat, itemID string, pcm []byte) error {
	if p.suppressDeltas || len(pcm) == 0 {
		return nil
	}
	if err := p.open(audio, format); err != nil {
		return err
	}
	if err := audio.WritePlayChunk(p.session.SessionID, pcm, false); err != nil {
		return err
	}
	if itemID != "" && p.itemID != itemID {
		p.itemID = itemID
		p.submittedBytes = 0
		p.itemStartedAt = p.currentTime()
	} else if itemID != "" && p.itemStartedAt.IsZero() {
		p.itemStartedAt = p.currentTime()
	}
	p.submittedBytes += int64(len(pcm))
	return nil
}

func (p *realtimePlaybackState) responseInterruption(format agent.AudioFormat) realtimevoice.ResponseInterruption {
	bytesPerSecond := int64(format.SampleRate) * int64(format.Channels) * int64(format.BitWidth) / 8
	submittedDuration := time.Duration(0)
	if bytesPerSecond > 0 {
		submittedDuration = time.Duration(p.submittedBytes) * time.Second / time.Duration(bytesPerSecond)
	}
	playedDuration := submittedDuration
	if !p.itemStartedAt.IsZero() {
		playedDuration = p.currentTime().Sub(p.itemStartedAt)
		if playedDuration < 0 {
			playedDuration = 0
		}
		if playedDuration > submittedDuration {
			playedDuration = submittedDuration
		}
	}
	return realtimevoice.ResponseInterruption{ItemID: p.itemID, AudioEndMS: int(playedDuration / time.Millisecond)}
}

func (p *realtimePlaybackState) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *realtimePlaybackState) keepAlive(audio realtimePlaybackAudio, format agent.AudioFormat) error {
	if p.finalizeResponses {
		return nil
	}
	if err := p.open(audio, format); err != nil {
		return err
	}
	return audio.WritePlayChunk(p.session.SessionID, nil, false)
}

func (p *realtimePlaybackState) interrupt(audio realtimePlaybackAudio, format agent.AudioFormat) error {
	p.suppressDeltas = true
	p.finalized = false
	if err := p.stop(audio); err != nil {
		return err
	}
	p.itemID = ""
	p.submittedBytes = 0
	p.itemStartedAt = time.Time{}
	return p.open(audio, format)
}

func (p *realtimePlaybackState) beginResponse(audio realtimePlaybackAudio, format agent.AudioFormat) error {
	p.suppressDeltas = false
	p.itemID = ""
	p.submittedBytes = 0
	p.itemStartedAt = time.Time{}
	if p.finalized {
		if err := p.stop(audio); err != nil {
			return err
		}
		p.finalized = false
	}
	return p.open(audio, format)
}

func (p *realtimePlaybackState) finishResponse(audio realtimePlaybackAudio, force ...bool) error {
	finalize := p.finalizeResponses
	if len(force) > 0 && force[0] {
		finalize = true
	}
	if !finalize || p.session == nil || p.finalized {
		return nil
	}
	if err := audio.WritePlayChunk(p.session.SessionID, nil, true); err != nil {
		return err
	}
	p.finalized = true
	return nil
}

func (p *realtimePlaybackState) stop(audio realtimePlaybackAudio) error {
	if p.session == nil {
		return nil
	}
	sessionID := p.session.SessionID
	p.session = nil
	p.finalized = false
	p.itemID = ""
	p.submittedBytes = 0
	p.itemStartedAt = time.Time{}
	return audio.StopPlayback(sessionID)
}

type realtimeRecordingAudio interface {
	ReadRecordChunk(uint64, uint32) (*agent.AudioChunkResult, error)
}

type realtimeRecordChunkReader interface {
	Read(uint32) (*agent.AudioChunkResult, error)
	Close() error
}

type realtimeRecordChunkReaderOpener interface {
	OpenRecordChunkReader(uint64) (*agent.AudioRecordChunkReader, error)
}

func streamRealtimeAudio(ctx context.Context, audio realtimeRecordingAudio, sessionID uint64, chunks chan<- []byte, errs chan<- error) {
	defer close(chunks)
	var reader realtimeRecordChunkReader = realtimeRecordingBackendReader{audio: audio, sessionID: sessionID}
	if opener, ok := audio.(realtimeRecordChunkReaderOpener); ok {
		serviceReader, err := opener.OpenRecordChunkReader(sessionID)
		if err != nil {
			select {
			case errs <- err:
			case <-ctx.Done():
			}
			return
		}
		reader = serviceReader
	}
	defer reader.Close()
	for {
		chunk, err := reader.Read(realtimeAudioReadTimeoutMs)
		if err != nil {
			select {
			case errs <- err:
			case <-ctx.Done():
			}
			return
		}
		if chunk.EndOfStream {
			return
		}
		if len(chunk.PCM) == 0 {
			continue
		}
		select {
		case chunks <- chunk.PCM:
		case <-ctx.Done():
			return
		}
	}
}

type realtimeRecordingBackendReader struct {
	audio     realtimeRecordingAudio
	sessionID uint64
}

func (r realtimeRecordingBackendReader) Read(timeoutMs uint32) (*agent.AudioChunkResult, error) {
	return r.audio.ReadRecordChunk(r.sessionID, timeoutMs)
}

func (realtimeRecordingBackendReader) Close() error { return nil }

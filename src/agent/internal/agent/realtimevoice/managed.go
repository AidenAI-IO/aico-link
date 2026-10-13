package realtimevoice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"aiden-agent/internal/agent/tts"

	"github.com/gorilla/websocket"
)

// ErrUnsupportedCapability is returned when a caller invokes an operation
// that the selected provider does not implement. Callers should normally use
// SessionInfo.Capabilities to avoid unsupported operations.
var ErrUnsupportedCapability = errors.New("realtime voice capability is unsupported")

// ErrSessionRotated reports that the provider ended a healthy session on its
// own schedule rather than failing. Gemini Live caps session lifetime and
// announces the cutoff ahead of time, so a caller that wants continuous
// conversation should open a new session instead of treating this as a fault.
// Wrap it with errors.Is to distinguish rotation from a real error.
var ErrSessionRotated = errors.New("realtime voice session was rotated by the provider")

// ErrSessionIdleTimeout reports that the provider closed the session because no
// response was generated for its idle window. This is normal session
// management, not a fault, so callers should announce it as an idle timeout
// rather than a service outage. Wrap it with errors.Is to distinguish an idle
// close from a real error.
var ErrSessionIdleTimeout = errors.New("realtime voice session was closed after an idle timeout")

// isSessionIdleCloseMessage reports whether a server-sent reason describes an
// idle close. Gemini Live and Qwen Omni Realtime both word it the same way and
// neither offers a machine-readable signal, so the wording is the only seam
// available. Keep this provider-neutral: the text comes from the server, and a
// provider reaching it through either seam below gets the same classification.
func isSessionIdleCloseMessage(reason string) bool {
	normalized := strings.ToLower(reason)
	if !strings.Contains(normalized, "session") || !strings.Contains(normalized, "closed") {
		return false
	}
	return strings.Contains(normalized, "no response was generated")
}

// classifyServerErrorMessage wraps a provider's JSON error-frame message with
// ErrSessionIdleTimeout when it describes an idle close. An unrecognized
// message stays a plain error and keeps its existing handling.
func classifyServerErrorMessage(message string) error {
	if isSessionIdleCloseMessage(message) {
		return fmt.Errorf("%w: %s", ErrSessionIdleTimeout, message)
	}
	return errors.New(message)
}

// classifyTransportError wraps a websocket read error with
// ErrSessionIdleTimeout when the peer's close reason describes an idle close.
// An idle close arrives as a close frame, which never reaches a provider's
// event translation, so this covers every provider at the transport seam.
func classifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || !isSessionIdleCloseMessage(closeErr.Text) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrSessionIdleTimeout, err)
}

// DeviceMediaConfig is the PCM contract between Aiden's audio device and the
// managed realtime session. Provider-native formats stay behind this seam.
type DeviceMediaConfig struct {
	Input  AudioFormat
	Output AudioFormat
}

// Conversation is Aiden's provider-neutral realtime session plus the optional
// operations actually implemented by the selected provider. Optional fields
// remain nil when unavailable so callers cannot mistake a forwarding shim for
// real protocol support.
type Conversation struct {
	Session
	TurnCommitter                  TurnCommitter
	ResponseInterrupter            ResponseInterrupter
	InterruptionAckTimeoutProvider InterruptionAckTimeoutProvider
	ToolResultSender               ToolResultSender
	TextSession                    TextSession
	ContextReplayer                ContextReplayer
}

// Open constructs the selected provider adapter and wraps it with Aiden's
// media and capability normalization.
func (r *ProviderRegistry) Open(
	ctx context.Context,
	name string,
	providerConfig ProviderConfig,
	sessionConfig SessionConfig,
	media DeviceMediaConfig,
) (*Conversation, error) {
	provider, err := r.New(name, providerConfig)
	if err != nil {
		return nil, err
	}
	raw, err := provider.Open(ctx, sessionConfig)
	if err != nil {
		return nil, err
	}
	managed, err := newManagedSession(raw, media)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	conversation := newConversation(managed, raw)
	capabilities := conversation.Info().Capabilities
	if capabilities.ClientSideTurnDetection && !capabilities.CanCommitInputTurn {
		_ = conversation.Close()
		return nil, fmt.Errorf("realtime provider %s requires client-side turn detection without input commit support", name)
	}
	if len(sessionConfig.Tools) > 0 && !capabilities.CanSendToolResult {
		_ = conversation.Close()
		return nil, fmt.Errorf("realtime provider %s cannot send configured tool results", name)
	}
	if capabilities.ExplicitToolContinuation && !capabilities.CanSendText {
		_ = conversation.Close()
		return nil, fmt.Errorf("realtime provider %s requires explicit response continuation without text response support", name)
	}
	return conversation, nil
}

func newConversation(managed *managedSession, raw Session) *Conversation {
	conversation := &Conversation{Session: managed}
	conversation.TurnCommitter, _ = raw.(TurnCommitter)
	conversation.ResponseInterrupter, _ = raw.(ResponseInterrupter)
	conversation.InterruptionAckTimeoutProvider, _ = raw.(InterruptionAckTimeoutProvider)
	conversation.ToolResultSender, _ = raw.(ToolResultSender)
	conversation.TextSession, _ = raw.(TextSession)
	conversation.ContextReplayer, _ = raw.(ContextReplayer)
	return conversation
}

type managedSession struct {
	raw             Session
	info            SessionInfo
	events          chan Event
	stop            chan struct{}
	closeOnce       sync.Once
	closeErr        error
	infoMu          sync.RWMutex
	inputMu         sync.Mutex
	inputResampler  *tts.PCM16MonoResampler
	outputResampler *tts.PCM16MonoResampler
	outputSpec      *pcmResamplerSpec
}

type pcmResamplerSpec struct {
	source AudioFormat
	target AudioFormat
}

func newManagedSession(raw Session, media DeviceMediaConfig) (*managedSession, error) {
	if raw == nil {
		return nil, errors.New("realtime voice provider returned a nil session")
	}
	rawInfo := raw.Info()
	providerInput := normalizedPCMFormat(rawInfo.InputFormatOrDefault(16000))
	providerOutput := normalizedPCMFormat(rawInfo.OutputFormatOrDefault(24000))
	deviceInput := requestedDeviceFormat(media.Input, providerInput)
	deviceOutput := requestedDeviceFormat(media.Output, providerOutput)
	inputSpec, err := mediaResamplerSpec("input", deviceInput, providerInput)
	if err != nil {
		return nil, err
	}
	outputSpec, err := mediaResamplerSpec("output", providerOutput, deviceOutput)
	if err != nil {
		return nil, err
	}

	capabilities := rawInfo.Capabilities
	_, capabilities.CanCommitInputTurn = raw.(TurnCommitter)
	_, capabilities.CanInterruptResponse = raw.(ResponseInterrupter)
	_, capabilities.CanSendToolResult = raw.(ToolResultSender)
	_, capabilities.CanSendText = raw.(TextSession)
	_, capabilities.CanReplayContext = raw.(ContextReplayer)
	info := rawInfo
	info.InputAudioFormat = deviceInput
	info.OutputAudioFormat = deviceOutput
	info.InputSampleRate = deviceInput.SampleRate
	info.OutputSampleRate = deviceOutput.SampleRate
	info.ProviderInputAudioFormat = providerInput
	info.ProviderOutputAudioFormat = providerOutput
	info.Capabilities = capabilities

	sourceEvents := raw.Events()
	eventBuffer := cap(sourceEvents)
	if eventBuffer < 1 {
		eventBuffer = 64
	}
	s := &managedSession{
		raw:        raw,
		info:       info,
		events:     make(chan Event, eventBuffer),
		stop:       make(chan struct{}),
		outputSpec: outputSpec,
	}
	if inputSpec != nil {
		s.inputResampler = inputSpec.newResampler()
	}
	if outputSpec != nil {
		s.outputResampler = outputSpec.newResampler()
	}
	go s.forwardEvents(sourceEvents)
	return s, nil
}

func normalizedPCMFormat(format AudioFormat) AudioFormat {
	if strings.TrimSpace(format.Encoding) == "" {
		format.Encoding = "pcm_s16le"
	}
	return format
}

func requestedDeviceFormat(requested, provider AudioFormat) AudioFormat {
	if strings.TrimSpace(requested.Encoding) == "" {
		requested.Encoding = provider.Encoding
	}
	if requested.SampleRate <= 0 {
		requested.SampleRate = provider.SampleRate
	}
	if requested.Channels <= 0 {
		requested.Channels = provider.Channels
	}
	if requested.BitDepth <= 0 {
		requested.BitDepth = provider.BitDepth
	}
	return normalizedPCMFormat(requested)
}

func mediaResamplerSpec(direction string, source, target AudioFormat) (*pcmResamplerSpec, error) {
	if !pcm16Mono(source) || !pcm16Mono(target) {
		return nil, fmt.Errorf("realtime %s media conversion only supports mono PCM16: source=%s target=%s", direction, formatLabel(source), formatLabel(target))
	}
	if source.SampleRate == target.SampleRate {
		return nil, nil
	}
	return &pcmResamplerSpec{source: source, target: target}, nil
}

func pcm16Mono(format AudioFormat) bool {
	if format.SampleRate <= 0 || format.Channels != 1 || format.BitDepth != 16 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(format.Encoding)) {
	case "", "pcm", "pcm16", "pcm_s16le", "audio/pcm":
		return true
	default:
		return false
	}
}

func formatLabel(format AudioFormat) string {
	return fmt.Sprintf("%s/%d/%d/%d", format.Encoding, format.SampleRate, format.Channels, format.BitDepth)
}

func (s *pcmResamplerSpec) newResampler() *tts.PCM16MonoResampler {
	return tts.NewPCM16MonoResampler(s.source.SampleRate, s.target.SampleRate)
}

func (s *managedSession) Info() SessionInfo {
	s.infoMu.RLock()
	defer s.infoMu.RUnlock()
	return s.info
}
func (s *managedSession) Events() <-chan Event  { return s.events }
func (s *managedSession) Errors() <-chan error  { return s.raw.Errors() }
func (s *managedSession) Done() <-chan struct{} { return s.raw.Done() }
func (s *managedSession) SendAudio(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	s.inputMu.Lock()
	if s.inputResampler != nil {
		pcm = s.inputResampler.Write(pcm)
	}
	s.inputMu.Unlock()
	if len(pcm) == 0 {
		return nil
	}
	return s.raw.SendAudio(ctx, pcm)
}

func (s *managedSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.closeErr = s.raw.Close()
	})
	return s.closeErr
}

func (s *managedSession) forwardEvents(source <-chan Event) {
	defer close(s.events)
	for event := range source {
		if event.Kind == EventResponseStarted && s.outputSpec != nil {
			s.outputResampler = s.outputSpec.newResampler()
		}
		if event.Kind == EventAudio {
			if err := s.refreshOutputResampler(); err != nil {
				if !s.forwardEvent(Event{Kind: EventError, Error: err}) {
					return
				}
				continue
			}
			if s.outputResampler != nil {
				event.PCM = s.outputResampler.Write(event.PCM)
				if len(event.PCM) == 0 {
					continue
				}
			}
		}
		if !s.forwardEvent(event) {
			return
		}
	}
}

// refreshOutputResampler observes provider-side format renegotiation. Gemini
// announces its actual PCM rate in the first audio MIME type, after the
// managed session has already been created; rebuilding here keeps playback
// speed and pitch correct without changing the device-side format contract.
func (s *managedSession) refreshOutputResampler() error {
	if s == nil || s.raw == nil {
		return nil
	}
	rawInfo := s.raw.Info()
	providerOutput := normalizedPCMFormat(rawInfo.OutputFormatOrDefault(24000))
	s.infoMu.RLock()
	deviceOutput := s.info.OutputAudioFormat
	previousProviderOutput := s.info.ProviderOutputAudioFormat
	s.infoMu.RUnlock()
	if providerOutput == previousProviderOutput {
		return nil
	}
	spec, err := mediaResamplerSpec("output", providerOutput, deviceOutput)
	if err != nil {
		return err
	}
	if s.outputSpec == nil && spec == nil {
		return nil
	}
	if s.outputSpec != nil && spec != nil && s.outputSpec.source == spec.source && s.outputSpec.target == spec.target {
		return nil
	}
	s.outputSpec = spec
	s.infoMu.Lock()
	s.info.ProviderOutputAudioFormat = providerOutput
	s.info.OutputSampleRate = deviceOutput.SampleRate
	s.infoMu.Unlock()
	if spec == nil {
		s.outputResampler = nil
	} else {
		s.outputResampler = spec.newResampler()
	}
	return nil
}

func (s *managedSession) forwardEvent(event Event) bool {
	select {
	case s.events <- event:
		return true
	case <-s.stop:
		return false
	}
}

var _ Session = (*managedSession)(nil)

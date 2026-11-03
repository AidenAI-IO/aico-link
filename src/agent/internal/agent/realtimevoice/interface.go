package realtimevoice

import (
	"context"
	"encoding/json"
	"time"
)

// Provider opens provider-neutral realtime voice sessions. Implementations
// hide provider wire formats, authentication, and transport details.
type Provider interface {
	Open(context.Context, SessionConfig) (Session, error)
}

type SessionConfig struct {
	APIKey string
	// SessionID is an optional control-plane identifier. Provider-direct brokers such as Speko mint it before opening the media connection.
	SessionID              string
	Model                  string
	Voice                  string
	Instructions           string
	InputAudioFormat       string
	OutputAudioFormat      string
	InputSampleRate        int
	OutputSampleRate       int
	MaxHistoryTurns        int
	TurnDetection          string
	TurnDetectionThresh    *float64
	TurnDetectionSilenceMs int
	EnableSpeechEmotion    *bool
	// ThinkingLevel sets thinkingConfig; only models that support thinking use it.
	ThinkingLevel string
	Tools         []Tool
}

// AudioFormat describes the media exchanged with a realtime provider. The
// legacy sample-rate fields on SessionInfo remain for compatibility; new code
// should use these complete formats so channel count and sample width are not
// implicit daemon assumptions.
type AudioFormat struct {
	Encoding   string
	SampleRate int
	Channels   int
	BitDepth   int
}

// OrDefault fills the fields that the daemon needs in order to open an audio
// device. Providers can leave a field unspecified when their wire contract
// has a well-known default; callers should use the negotiated sample rate
// before falling back to a local default.
func (f AudioFormat) OrDefault(sampleRate int) AudioFormat {
	if f.SampleRate <= 0 {
		f.SampleRate = sampleRate
	}
	if f.Channels <= 0 {
		f.Channels = 1
	}
	if f.BitDepth <= 0 {
		f.BitDepth = 16
	}
	return f
}

func normalizedSequence(primary, alternate uint64) uint64 {
	if primary != 0 {
		return primary
	}
	return alternate
}

// turnDetectionConfig keeps optional VAD values out of provider payloads
// when the user did not configure them. Providers may still encode a disabled
// detector using their own wire-level representation.
func turnDetectionConfig(cfg SessionConfig) map[string]any {
	if cfg.TurnDetection == "" || cfg.TurnDetection == "disabled" {
		return nil
	}
	settings := map[string]any{"type": cfg.TurnDetection}
	if cfg.TurnDetectionThresh != nil {
		settings["threshold"] = *cfg.TurnDetectionThresh
	}
	if cfg.TurnDetectionSilenceMs > 0 {
		settings["silence_duration_ms"] = cfg.TurnDetectionSilenceMs
	}
	return settings
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type Capabilities struct {
	// EmitsSpeechEvents means the provider emits EventSpeechStarted and
	// EventSpeechStopped for input activity. Providers such as Gemini Live use
	// server VAD without exposing those boundaries, so callers may need a local
	// activity gate for admission decisions.
	EmitsSpeechEvents bool
	// ClientSideTurnDetection means the caller must detect the end of an
	// input turn and invoke TurnCommitter. Provider-side VAD remains the
	// default.
	ClientSideTurnDetection bool
	// ExplicitToolContinuation means the caller must invoke CreateResponse
	// after all results for a tool-bearing response have been sent.
	ExplicitToolContinuation bool
	// ServerAuthoritativeInterruption means response output must remain active
	// until the provider reports an interruption. A local microphone energy
	// gate must not classify anonymous response output as stale on its own.
	ServerAuthoritativeInterruption bool
	// ServerAuthoritativeTurnDetection means the provider alone decides whether
	// buffered input became a user turn. The caller must not promote local audio
	// energy into a pending input turn for admission decisions.
	ServerAuthoritativeTurnDetection bool
	CanCommitInputTurn               bool
	CanInterruptResponse             bool
	CanSendToolResult                bool
	CanSendText                      bool
	CanReplayContext                 bool
}

type SessionInfo struct {
	ID                string
	InputSampleRate   int
	OutputSampleRate  int
	InputAudioFormat  AudioFormat
	OutputAudioFormat AudioFormat
	// Provider formats describe the native media contract hidden by an Aico
	// managed session. They equal the public formats on raw provider sessions.
	ProviderInputAudioFormat  AudioFormat
	ProviderOutputAudioFormat AudioFormat
	Capabilities              Capabilities
}

// InputFormatOrDefault returns the negotiated input format, falling back to
// the legacy sample-rate field and finally to defaultRate. The legacy fields
// are intentionally read here so callers do not have to know both interface
// generations.
func (i SessionInfo) InputFormatOrDefault(defaultRate int) AudioFormat {
	format := i.InputAudioFormat
	if format.SampleRate <= 0 {
		format.SampleRate = i.InputSampleRate
	}
	return format.OrDefault(defaultRate)
}

// OutputFormatOrDefault is the output-side counterpart to
// InputFormatOrDefault.
func (i SessionInfo) OutputFormatOrDefault(defaultRate int) AudioFormat {
	format := i.OutputAudioFormat
	if format.SampleRate <= 0 {
		format.SampleRate = i.OutputSampleRate
	}
	return format.OrDefault(defaultRate)
}

// newPCM16SessionInfo keeps the legacy rate fields and negotiated formats
// consistent for providers whose wire contract is signed 16-bit PCM.
func newPCM16SessionInfo(id string, inputRate, outputRate int, capabilities Capabilities) SessionInfo {
	input := (AudioFormat{Encoding: "pcm_s16le", SampleRate: inputRate}).OrDefault(16000)
	output := (AudioFormat{Encoding: "pcm_s16le", SampleRate: outputRate}).OrDefault(24000)
	return SessionInfo{
		ID: id, InputSampleRate: input.SampleRate, OutputSampleRate: output.SampleRate,
		InputAudioFormat: input, OutputAudioFormat: output,
		ProviderInputAudioFormat: input, ProviderOutputAudioFormat: output,
		Capabilities: capabilities,
	}
}

// contextItemPayload builds the `item` object for a conversation.item.create
// event, omitting fields the item does not carry. Empty values must be absent
// rather than sent as "": xAI validates `item.role` against an enum and rejects
// the whole event when a function_call item carries an empty role, which ends
// the session as soon as history replay starts.
func contextItemPayload(item ContextItem) map[string]any {
	payload := make(map[string]any, 6)
	for key, value := range map[string]string{
		"type":      item.Type,
		"role":      item.Role,
		"call_id":   item.CallID,
		"name":      item.Name,
		"arguments": item.Arguments,
		"output":    item.Output,
	} {
		if value != "" {
			payload[key] = value
		}
	}
	if item.Type == "message" {
		contentType := "input_text"
		if item.Role == "assistant" {
			contentType = "output_text"
		}
		payload["content"] = []map[string]string{{"type": contentType, "text": item.Content}}
	}
	return payload
}

type Session interface {
	Info() SessionInfo
	Events() <-chan Event
	Errors() <-chan error
	Done() <-chan struct{}
	SendAudio(context.Context, []byte) error
	Close() error
}

// TurnCommitter is an optional session capability for providers that accept a
// client-side end-of-turn marker. It is intentionally separate from Session:
// a provider may use server VAD and have no meaningful Commit operation.
type TurnCommitter interface {
	Commit(context.Context) error
}

// ResponseInterrupter is an optional session capability. Providers that
// perform interruption entirely on the server should not implement it.
type ResponseInterrupter interface {
	Interrupt(context.Context, ResponseInterruption) error
}

// InterruptionAckTimeoutProvider lets a provider bound how long its session
// can safely wait for the terminal acknowledgement after Interrupt succeeds.
// Providers without this capability continue using the daemon's response
// watchdog.
type InterruptionAckTimeoutProvider interface {
	InterruptionAckTimeout() time.Duration
}

// ResponseInterruption identifies how much of the current assistant audio was
// submitted for playback. Providers that keep server-side conversation audio
// can use it to remove the unheard tail after a barge-in.
type ResponseInterruption struct {
	ItemID     string
	AudioEndMS int
	// ServerDetected is set when the provider's VAD reported the barge-in.
	// Adapters use it only where that provider's protocol defines additional
	// server-side interruption behavior.
	ServerDetected bool
}

// ToolResultSender is an optional session capability for sessions that expose
// tool calls and accept their results over the same connection.
type ToolResultSender interface {
	SendToolResult(context.Context, string, string) error
}

// TextSession is an optional capability for injecting text and explicitly
// starting a response. Provider-direct brokers may return the native session
// capability when the selected upstream supports text injection; callers must
// feature-detect it.
type TextSession interface {
	Session
	SendText(context.Context, string) error
	CreateResponse(context.Context) error
}

// ContextReplayer is independent from TextSession because a provider may
// accept text injection while using a server-managed conversation history.
// Implementations should preserve item order and return the first write error.
type ContextReplayer interface {
	ReplayContext(context.Context, []ContextItem) error
}

type ContextItem struct {
	Role      string
	Content   string
	Type      string
	CallID    string
	Name      string
	Arguments string
	Output    string
}

type EventKind string

const (
	EventReady             EventKind = "ready"
	EventAudio             EventKind = "audio"
	EventSpeechStarted     EventKind = "speech_started"
	EventSpeechStopped     EventKind = "speech_stopped"
	EventInputCommitted    EventKind = "input_committed"
	EventTranscriptDelta   EventKind = "transcript_delta"
	EventTranscriptFinal   EventKind = "transcript_final"
	EventTranscriptFailed  EventKind = "transcript_failed"
	EventResponseStarted   EventKind = "response_started"
	EventResponseDone      EventKind = "response_done"
	EventResponseCancelled EventKind = "response_cancelled"
	EventInterruption      EventKind = "interruption"
	EventToolCall          EventKind = "tool_call"
	// EventToolCallCancelled cancels only the invocation identified by CallID.
	EventToolCallCancelled EventKind = "tool_call_cancelled"
	EventUsage             EventKind = "usage"
	EventError             EventKind = "error"
	EventClosed            EventKind = "closed"
)

type Event struct {
	Kind           EventKind
	SessionID      string
	ResponseID     string
	ItemID         string
	PreviousItemID string
	Sequence       uint64
	CallID         string
	Name           string
	Arguments      string
	Text           string
	TextSource     string
	PCM            []byte
	Role           string
	Final          bool
	At             string
	Status         string
	AudioStartMS   int
	AudioEndMS     int
	Error          error
	Usage          Usage
}

type Usage struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
}

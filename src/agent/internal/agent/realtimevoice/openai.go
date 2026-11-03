package realtimevoice

import (
	"aico-agent/internal/logging"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

const (
	DefaultOpenAIRealtimeEndpoint        = "wss://api.openai.com/v1/realtime"
	DefaultOpenAIRealtimeModel           = "gpt-realtime"
	DefaultOpenAIInputTranscriptionModel = "gpt-4o-mini-transcribe"
)

// OpenAIProvider is the native OpenAI Realtime adapter. Endpoint is optional
// and is primarily useful for a compatible gateway or protocol tests.
type OpenAIProvider struct {
	Endpoint         string
	RealtimeProtocol string
	Dialer           *websocket.Dialer
	EventBuffer      int
}

func (p OpenAIProvider) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("openai realtime: APIKey is required")
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultOpenAIRealtimeModel
	}
	endpoint, err := p.endpoint(model)
	if err != nil {
		return nil, err
	}
	d := p.Dialer
	if d == nil {
		d = websocket.DefaultDialer
	}
	header := make(map[string][]string, 1)
	header["Authorization"] = []string{bearer(cfg.APIKey)}
	conn, resp, err := d.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("openai realtime websocket connect: %w", err)
	}
	inputRate := cfg.InputSampleRate
	if inputRate <= 0 {
		inputRate = 24000
	}
	outputRate := cfg.OutputSampleRate
	if outputRate <= 0 {
		outputRate = 24000
	}
	transport := newJSONWebSocketTransport(conn, "openai realtime", p.EventBuffer)
	s := &openAISession{
		jsonRealtimeSession: newJSONRealtimeSession(transport, newPCM16SessionInfo(cfg.SessionID, inputRate, outputRate, Capabilities{EmitsSpeechEvents: true, ExplicitToolContinuation: true})),
		realtimeProtocol:    p.RealtimeProtocol,
	}
	transport.start(func(body []byte) []Event {
		event, ok := translateOpenAIEvent(body)
		if !ok {
			return nil
		}
		if event.Kind == EventReady {
			logOpenAITranscriptionAcknowledgement(body, s.Info().ID)
		}
		return []Event{event}
	})

	if err := s.waitReady(ctx, 1); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.writeJSON(ctx, buildOpenAISessionUpdateForProtocol(cfg, model, p.RealtimeProtocol)); err != nil {
		_ = s.Close()
		return nil, err
	}
	logging.Infof("agent", "realtime", "OpenAI transcription requested: session_id=%s protocol=%s model=%s transcription_model=%s language=auto", s.Info().ID, normalizeRealtimeProtocol(p.RealtimeProtocol), model, DefaultOpenAIInputTranscriptionModel)
	if err := s.waitReady(ctx, 1); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (p OpenAIProvider) endpoint(model string) (string, error) {
	raw := strings.TrimSpace(p.Endpoint)
	if raw == "" {
		raw = DefaultOpenAIRealtimeEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("openai realtime: parse endpoint: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("openai realtime: endpoint must use ws:// or https://, got %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/realtime"
	}
	q := u.Query()
	q.Set("model", model)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type openAISessionUpdate struct {
	Type    string                `json:"type"`
	Session openAISessionSettings `json:"session"`
}

type openAISessionSettings struct {
	Type             string              `json:"type,omitempty"`
	Model            string              `json:"model,omitempty"`
	OutputModalities []string            `json:"output_modalities,omitempty"`
	Instructions     string              `json:"instructions,omitempty"`
	Audio            openAIAudioSettings `json:"audio"`
	Tools            []openAITool        `json:"tools,omitempty"`
}

type openAIAudioSettings struct {
	Input  openAIAudioInput  `json:"input"`
	Output openAIAudioOutput `json:"output"`
}

type openAIAudioInput struct {
	Format        openAIAudioFormat         `json:"format"`
	Transcription openAITranscriptionConfig `json:"transcription"`
	TurnDetection any                       `json:"turn_detection,omitempty"`
}

type openAITranscriptionConfig struct {
	Model    string `json:"model"`
	Language string `json:"language,omitempty"`
}

type openAIAudioOutput struct {
	Format openAIAudioFormat `json:"format"`
	Voice  string            `json:"voice,omitempty"`
}

type openAIAudioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type openAITool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildOpenAISessionUpdateForProtocol(cfg SessionConfig, model, protocol string) any {
	if normalizeRealtimeProtocol(protocol) == "legacy" {
		return buildOpenAILegacySessionUpdate(cfg)
	}
	return buildOpenAISessionUpdate(cfg, model)
}

func normalizeRealtimeProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "beta", "legacy":
		return "legacy"
	default:
		return "ga"
	}
}

type openAILegacySessionUpdate struct {
	Type    string                      `json:"type"`
	Session openAILegacySessionSettings `json:"session"`
}

type openAILegacySessionSettings struct {
	Modalities              []string                   `json:"modalities,omitempty"`
	Instructions            string                     `json:"instructions,omitempty"`
	Voice                   string                     `json:"voice,omitempty"`
	InputAudioFormat        string                     `json:"input_audio_format,omitempty"`
	OutputAudioFormat       string                     `json:"output_audio_format,omitempty"`
	InputAudioTranscription *openAITranscriptionConfig `json:"input_audio_transcription,omitempty"`
	TurnDetection           map[string]any             `json:"turn_detection,omitempty"`
	Tools                   []openAITool               `json:"tools,omitempty"`
}

func buildOpenAILegacySessionUpdate(cfg SessionConfig) openAILegacySessionUpdate {
	settings := openAILegacySessionSettings{
		Modalities: []string{"audio", "text"}, Instructions: cfg.Instructions,
		Voice: cfg.Voice, InputAudioFormat: "pcm16", OutputAudioFormat: "pcm16",
		InputAudioTranscription: &openAITranscriptionConfig{Model: DefaultOpenAIInputTranscriptionModel},
		TurnDetection:           turnDetectionConfig(cfg),
	}
	for _, tool := range cfg.Tools {
		settings.Tools = append(settings.Tools, openAITool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return openAILegacySessionUpdate{Type: "session.update", Session: settings}
}

func buildOpenAISessionUpdate(cfg SessionConfig, model string) openAISessionUpdate {
	inputRate := cfg.InputSampleRate
	if inputRate <= 0 {
		inputRate = 24000
	}
	outputRate := cfg.OutputSampleRate
	if outputRate <= 0 {
		outputRate = 24000
	}
	settings := openAISessionSettings{
		Type:             "realtime",
		Model:            model,
		OutputModalities: []string{"audio"},
		Instructions:     cfg.Instructions,
		Audio: openAIAudioSettings{
			Input: openAIAudioInput{
				Format:        openAIAudioFormat{Type: "audio/pcm", Rate: inputRate},
				Transcription: openAITranscriptionConfig{Model: DefaultOpenAIInputTranscriptionModel},
			},
			Output: openAIAudioOutput{Format: openAIAudioFormat{Type: "audio/pcm", Rate: outputRate}, Voice: cfg.Voice},
		},
	}
	if turnDetection := turnDetectionConfig(cfg); turnDetection != nil {
		settings.Audio.Input.TurnDetection = turnDetection
	}
	for _, tool := range cfg.Tools {
		settings.Tools = append(settings.Tools, openAITool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return openAISessionUpdate{Type: "session.update", Session: settings}
}

type openAISession struct {
	*jsonRealtimeSession
	realtimeProtocol string
}

func (s *openAISession) ReplayContext(ctx context.Context, items []ContextItem) error {
	for _, item := range items {
		if err := s.writeJSON(ctx, map[string]any{
			"type": "conversation.item.create",
			"item": openAIContextItemPayload(item, s.realtimeProtocol),
		}); err != nil {
			return err
		}
	}
	return nil
}

func openAIContextItemPayload(item ContextItem, protocol string) map[string]any {
	item.CallID = openAIReplayCallID(item.CallID)
	payload := contextItemPayload(item)
	if normalizeRealtimeProtocol(protocol) == "legacy" && item.Type == "message" && item.Role == "assistant" {
		// OpenAI GA renamed assistant message content from text to output_text.
		// Legacy/Beta-compatible gateways such as MixRoute still reject
		// output_text and require the original text content type.
		payload["content"] = []map[string]string{{"type": "text", "text": item.Content}}
	}
	return payload
}

// openAIReplayCallID converts provider-native IDs into an OpenAI-safe wire ID
// when replaying persisted tool history. The mapping is deterministic so a
// function_call and its function_call_output retain the same correlation ID.
// Live tool results keep the provider-issued ID unchanged.
func openAIReplayCallID(callID string) string {
	const maxLength = 32
	if len(callID) <= maxLength {
		return callID
	}
	digest := sha256.Sum256([]byte(callID))
	return fmt.Sprintf("call_%x", digest)[:maxLength]
}

func (s *openAISession) Interrupt(ctx context.Context, interruption ResponseInterruption) error {
	if err := s.cancelResponse(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(interruption.ItemID) == "" {
		return nil
	}
	if interruption.AudioEndMS < 0 {
		interruption.AudioEndMS = 0
	}
	return s.writeJSON(ctx, map[string]any{
		"type":          "conversation.item.truncate",
		"item_id":       interruption.ItemID,
		"content_index": 0,
		"audio_end_ms":  interruption.AudioEndMS,
	})
}

type openAITranscriptionAcknowledgement struct {
	SessionID string
	Model     string
	Language  string
	Present   bool
}

func parseOpenAITranscriptionAcknowledgement(body []byte, fallbackSessionID string) (openAITranscriptionAcknowledgement, bool) {
	var frame struct {
		Type    string `json:"type"`
		Session struct {
			ID                      string                     `json:"id"`
			InputAudioTranscription *openAITranscriptionConfig `json:"input_audio_transcription"`
			Audio                   struct {
				Input struct {
					Transcription *openAITranscriptionConfig `json:"transcription"`
				} `json:"input"`
			} `json:"audio"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &frame); err != nil || frame.Type != "session.updated" {
		return openAITranscriptionAcknowledgement{}, false
	}
	if frame.Session.ID == "" {
		frame.Session.ID = fallbackSessionID
	}
	transcription := frame.Session.Audio.Input.Transcription
	if transcription == nil {
		transcription = frame.Session.InputAudioTranscription
	}
	ack := openAITranscriptionAcknowledgement{SessionID: frame.Session.ID, Present: transcription != nil}
	if transcription != nil {
		ack.Model = transcription.Model
		ack.Language = transcription.Language
	}
	return ack, true
}

func logOpenAITranscriptionAcknowledgement(body []byte, fallbackSessionID string) {
	ack, ok := parseOpenAITranscriptionAcknowledgement(body, fallbackSessionID)
	if !ok {
		return
	}
	if !ack.Present {
		logging.Warnf("agent", "realtime", "OpenAI transcription acknowledgement absent: session_id=%s", ack.SessionID)
		return
	}
	logging.Infof("agent", "realtime", "OpenAI transcription acknowledged: session_id=%s transcription_model=%s language=%q", ack.SessionID, ack.Model, ack.Language)
}

func translateOpenAIEvent(body []byte) (Event, bool) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Event{Kind: EventError, Error: fmt.Errorf("openai realtime: decode event: %w", err)}, true
	}
	// The GA Realtime API renamed assistant output events to response.output_*;
	// retain the beta aliases so gateways and older models remain compatible.
	switch envelope.Type {
	case "response.output_text.delta":
		envelope.Type = "response.text.delta"
	case "response.output_audio_transcript.delta":
		envelope.Type = "response.audio_transcript.delta"
	case "response.output_audio_transcript.done":
		envelope.Type = "response.audio_transcript.done"
	case "response.output_audio.delta":
		envelope.Type = "response.audio.delta"
	}
	switch envelope.Type {
	case "session.created", "session.updated":
		var event struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventReady, SessionID: event.Session.ID}, true
	case "input_audio_buffer.speech_started":
		var event struct {
			ItemID       string `json:"item_id"`
			AudioStartMS int    `json:"audio_start_ms"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventSpeechStarted, ItemID: event.ItemID, AudioStartMS: event.AudioStartMS}, true
	case "input_audio_buffer.speech_stopped":
		var event struct {
			ItemID     string `json:"item_id"`
			AudioEndMS int    `json:"audio_end_ms"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventSpeechStopped, ItemID: event.ItemID, AudioEndMS: event.AudioEndMS}, true
	case "input_audio_buffer.committed":
		var event struct {
			ItemID         string `json:"item_id"`
			PreviousItemID string `json:"previous_item_id"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventInputCommitted, ItemID: event.ItemID, PreviousItemID: event.PreviousItemID}, true
	case "conversation.item.input_audio_transcription.delta":
		var event struct {
			Delta          string `json:"delta"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptDelta, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), Role: "user", Text: event.Delta, TextSource: "audio"}, true
	case "conversation.item.input_audio_transcription.completed":
		var event struct {
			Transcript     string `json:"transcript"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptFinal, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), Role: "user", Text: event.Transcript, TextSource: "audio", Final: true}, true
	case "conversation.item.input_audio_transcription.failed":
		var event struct {
			ItemID string `json:"item_id"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		err := errors.New(event.Error.Message)
		if event.Error.Code != "" {
			err = fmt.Errorf("%s: %w", event.Error.Code, err)
		}
		return Event{Kind: EventTranscriptFailed, ItemID: event.ItemID, Role: "user", TextSource: "audio", Error: err}, true
	case "response.created":
		var event struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventResponseStarted, ResponseID: event.Response.ID}, true
	case "response.text.delta":
		var event struct {
			Delta          string `json:"delta"`
			ResponseID     string `json:"response_id"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptDelta, ResponseID: event.ResponseID, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), Role: "assistant", Text: event.Delta, TextSource: "text"}, true
	case "response.audio_transcript.delta":
		var event struct {
			Delta          string `json:"delta"`
			ResponseID     string `json:"response_id"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptDelta, ResponseID: event.ResponseID, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), Role: "assistant", Text: event.Delta, TextSource: "audio"}, true
	case "response.audio_transcript.done":
		var event struct {
			Transcript     string `json:"transcript"`
			ResponseID     string `json:"response_id"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptFinal, ResponseID: event.ResponseID, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), Role: "assistant", Text: event.Transcript, TextSource: "audio", Final: true}, true
	case "response.audio.delta":
		var event struct {
			Delta          string `json:"delta"`
			ResponseID     string `json:"response_id"`
			ItemID         string `json:"item_id"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		pcm, err := base64.StdEncoding.DecodeString(event.Delta)
		if err != nil {
			return Event{Kind: EventError, Error: fmt.Errorf("openai realtime: decode audio delta: %w", err)}, true
		}
		return Event{Kind: EventAudio, ResponseID: event.ResponseID, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), PCM: pcm}, true
	case "response.function_call_arguments.done":
		var event struct {
			ResponseID     string `json:"response_id"`
			ItemID         string `json:"item_id"`
			CallID         string `json:"call_id"`
			Name           string `json:"name"`
			Arguments      string `json:"arguments"`
			Sequence       uint64 `json:"sequence"`
			SequenceNumber uint64 `json:"sequence_number"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventToolCall, ResponseID: event.ResponseID, ItemID: event.ItemID, Sequence: normalizedSequence(event.Sequence, event.SequenceNumber), CallID: event.CallID, Name: event.Name, Arguments: event.Arguments}, true
	case "response.done":
		var event struct {
			Response struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Usage  *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		var usage Usage
		if event.Response.Usage != nil {
			usage = Usage{InputTokens: event.Response.Usage.InputTokens, OutputTokens: event.Response.Usage.OutputTokens, TotalTokens: event.Response.Usage.TotalTokens}
		}
		return terminalResponseEvent("openai", event.Response.ID, event.Response.Status, usage), true
	case "conversation.item.truncated", "response.cancelled":
		return Event{Kind: EventInterruption, At: "assistant"}, true
	case "error":
		var event struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventError, Error: errors.New(event.Error.Message)}, true
	default:
		return Event{}, false
	}
}

var _ Provider = OpenAIProvider{}
var _ TextSession = (*openAISession)(nil)

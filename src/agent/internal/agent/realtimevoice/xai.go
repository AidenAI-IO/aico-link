package realtimevoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	DefaultXAIRealtimeEndpoint = "wss://api.x.ai/v1/realtime"
	DefaultXAIRealtimeModel    = "grok-voice-latest"
)

// XAIProvider is the native XAI Realtime adapter. Endpoint is optional
// and is primarily useful for a compatible gateway or protocol tests.
type XAIProvider struct {
	Endpoint    string
	Dialer      *websocket.Dialer
	EventBuffer int
	// AuthSubprotocol is used for Speko-delegated xAI credentials. Native xAI API keys use the Authorization bearer header.
	AuthSubprotocol       bool
	AuthSubprotocolPrefix string
}

func (p XAIProvider) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("xai realtime: APIKey is required")
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultXAIRealtimeModel
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
	if p.AuthSubprotocol {
		dialer := *d
		prefix := p.AuthSubprotocolPrefix
		if prefix == "" {
			prefix = "xai-client-secret."
		}
		dialer.Subprotocols = append(append([]string(nil), dialer.Subprotocols...), prefix+cfg.APIKey)
		conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return nil, fmt.Errorf("xai realtime websocket connect: %w", err)
		}
		return p.openSession(ctx, cfg, model, conn)
	}
	header["Authorization"] = []string{bearer(cfg.APIKey)}
	conn, resp, err := d.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("xai realtime websocket connect: %w", err)
	}
	return p.openSession(ctx, cfg, model, conn)
}

func (p XAIProvider) openSession(ctx context.Context, cfg SessionConfig, model string, conn *websocket.Conn) (Session, error) {
	inputRate := cfg.InputSampleRate
	if inputRate <= 0 {
		inputRate = 24000
	}
	outputRate := cfg.OutputSampleRate
	if outputRate <= 0 {
		outputRate = 24000
	}
	transport := newJSONWebSocketTransport(conn, "xai realtime", p.EventBuffer)
	s := &xAISession{
		jsonRealtimeSession: newJSONRealtimeSession(transport, newPCM16SessionInfo(cfg.SessionID, inputRate, outputRate, Capabilities{EmitsSpeechEvents: true, ExplicitToolContinuation: true})),
		transcripts:         make(map[string]string),
	}
	transport.start(func(body []byte) []Event {
		event, ok := s.translateXAIEventForSession(body)
		if !ok {
			return nil
		}
		return []Event{event}
	})

	if err := s.waitReady(ctx, 1); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.writeJSON(ctx, buildXAISessionUpdate(cfg, model)); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.waitReady(ctx, 1); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (p XAIProvider) endpoint(model string) (string, error) {
	raw := strings.TrimSpace(p.Endpoint)
	if raw == "" {
		raw = DefaultXAIRealtimeEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("xai realtime: parse endpoint: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("xai realtime: endpoint must use ws:// or https://, got %q", u.Scheme)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/realtime"
	}
	q := u.Query()
	q.Set("model", model)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type xAISessionUpdate struct {
	Type    string             `json:"type"`
	Session xAISessionSettings `json:"session"`
}

type xAISessionSettings struct {
	Type             string           `json:"type,omitempty"`
	Model            string           `json:"model,omitempty"`
	OutputModalities []string         `json:"output_modalities,omitempty"`
	Instructions     string           `json:"instructions,omitempty"`
	Voice            string           `json:"voice,omitempty"`
	TurnDetection    any              `json:"turn_detection,omitempty"`
	Audio            xAIAudioSettings `json:"audio"`
	Tools            []xAITool        `json:"tools,omitempty"`
}

type xAIAudioSettings struct {
	Input  xAIAudioInput  `json:"input"`
	Output xAIAudioOutput `json:"output"`
}

type xAIAudioInput struct {
	Format        xAIAudioFormat    `json:"format"`
	Transcription *xAITranscription `json:"transcription,omitempty"`
}

type xAITranscription struct {
	Model string `json:"model,omitempty"`
}

type xAIAudioOutput struct {
	Format xAIAudioFormat `json:"format"`
}

type xAIAudioFormat struct {
	Type string `json:"type"`
	Rate int    `json:"rate"`
}

type xAITool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildXAISessionUpdate(cfg SessionConfig, model string) xAISessionUpdate {
	inputRate := cfg.InputSampleRate
	if inputRate <= 0 {
		inputRate = 24000
	}
	outputRate := cfg.OutputSampleRate
	if outputRate <= 0 {
		outputRate = 24000
	}
	settings := xAISessionSettings{
		Type:             "realtime",
		Model:            model,
		OutputModalities: []string{"audio"},
		Instructions:     cfg.Instructions,
		Voice:            cfg.Voice,
		Audio: xAIAudioSettings{
			Input:  xAIAudioInput{Format: xAIAudioFormat{Type: "audio/pcm", Rate: inputRate}, Transcription: &xAITranscription{Model: "grok-transcribe"}},
			Output: xAIAudioOutput{Format: xAIAudioFormat{Type: "audio/pcm", Rate: outputRate}},
		},
	}
	if cfg.TurnDetection == "disabled" {
		settings.TurnDetection = json.RawMessage("null")
	} else if turnDetection := turnDetectionConfig(cfg); turnDetection != nil {
		settings.TurnDetection = turnDetection
	}
	for _, tool := range cfg.Tools {
		settings.Tools = append(settings.Tools, xAITool{Type: "function", Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return xAISessionUpdate{Type: "session.update", Session: settings}
}

type xAISession struct {
	*jsonRealtimeSession
	transcriptMu sync.Mutex
	transcripts  map[string]string
}

func (s *xAISession) Interrupt(ctx context.Context, _ ResponseInterruption) error {
	return s.cancelResponse(ctx)
}
func (s *xAISession) CreateResponse(ctx context.Context) error {
	return s.writeJSON(ctx, map[string]any{
		"type": "response.create",
		"response": map[string]any{
			"output_modalities": []string{"audio"},
			"metadata":          map[string]string{"client_event_id": fmt.Sprintf("aico-%d", time.Now().UnixNano())},
		},
	})
}
func translateXAIEventBase(body []byte) (Event, bool) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Event{Kind: EventError, Error: fmt.Errorf("xai realtime: decode event: %w", err)}, true
	}
	switch envelope.Type {
	case "session.created", "session.updated", "conversation.created":
		var event struct {
			Session struct {
				ID string `json:"id"`
			} `json:"session"`
			Conversation struct {
				ID string `json:"id"`
			} `json:"conversation"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		id := event.Session.ID
		if id == "" {
			id = event.Conversation.ID
		}
		return Event{Kind: EventReady, SessionID: id}, true
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
		var event xAIInputTranscription
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		// A non-terminal status means the transcript is still being refined.
		// The session-scoped translator turns those into deltas; drop them here
		// so this stateless path can never promote one to a final transcript.
		if event.Status != "" && event.Status != "completed" {
			return Event{}, false
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
			return Event{Kind: EventError, Error: fmt.Errorf("xai realtime: decode audio delta: %w", err)}, true
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
		return terminalResponseEvent("xai", event.Response.ID, event.Response.Status, usage), true
	case "conversation.item.truncated", "response.cancelled":
		return Event{Kind: EventInterruption, At: "assistant"}, true
	case "error":
		var event struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
				Param   string `json:"param"`
				EventID string `json:"event_id"`
			} `json:"error"`
		}
		raw := compactXAIEvent(body)
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		message := strings.TrimSpace(event.Error.Message)
		if message == "" {
			message = "xAI realtime server error"
		}
		if raw != "" {
			message += " (raw=" + raw + ")"
		}
		var details []string
		if event.Error.Type != "" {
			details = append(details, "type="+event.Error.Type)
		}
		if event.Error.Code != "" {
			details = append(details, "code="+event.Error.Code)
		}
		if event.Error.Param != "" {
			details = append(details, "param="+event.Error.Param)
		}
		if event.Error.EventID != "" {
			details = append(details, "event_id="+event.Error.EventID)
		}
		if len(details) > 0 {
			message += " (" + strings.Join(details, ", ") + ")"
		}
		return Event{Kind: EventError, Error: errors.New(message)}, true
	default:
		return Event{}, false
	}
}

func compactXAIEvent(body []byte) string {
	const maxBytes = 2048
	if len(body) > maxBytes {
		body = body[:maxBytes]
	}
	return strings.ReplaceAll(strings.ReplaceAll(string(body), "\n", ""), "\r", "")
}

// xAIInputTranscription is the shared shape of the .updated and .completed
// input-transcription events. Status is only populated on .completed.
type xAIInputTranscription struct {
	ItemID         string `json:"item_id"`
	Transcript     string `json:"transcript"`
	Status         string `json:"status"`
	Sequence       uint64 `json:"sequence"`
	SequenceNumber uint64 `json:"sequence_number"`
}

// itemKey scopes accumulated transcript state. xAI has been observed to omit
// item_id on some frames, so those share one bucket rather than each resetting
// an empty key.
func (t xAIInputTranscription) itemKey() string {
	if t.ItemID == "" {
		return "__default__"
	}
	return t.ItemID
}

// cumulativeUserTranscriptDelta converts a cumulative transcript into an
// incremental delta. xAI's .updated event carries the full transcript so far
// and may revise earlier text, so a shorter or diverging transcript yields no
// delta instead of re-sending text the caller already has.
func (s *xAISession) cumulativeUserTranscriptDelta(frame xAIInputTranscription) (Event, bool) {
	key := frame.itemKey()
	s.transcriptMu.Lock()
	previous := s.transcripts[key]
	s.transcripts[key] = frame.Transcript
	s.transcriptMu.Unlock()
	delta := frame.Transcript
	if previous != "" {
		if strings.HasPrefix(frame.Transcript, previous) {
			delta = frame.Transcript[len(previous):]
		} else {
			delta = ""
		}
	}
	if delta == "" {
		return Event{}, false
	}
	return Event{Kind: EventTranscriptDelta, ItemID: frame.ItemID, Sequence: normalizedSequence(frame.Sequence, frame.SequenceNumber), Role: "user", Text: delta, TextSource: "audio"}, true
}

func (s *xAISession) translateXAIEventForSession(body []byte) (Event, bool) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Event{Kind: EventError, Error: fmt.Errorf("xai realtime: decode event: %w", err)}, true
	}
	if envelope.Type == "conversation.item.input_audio_transcription.updated" {
		var update xAIInputTranscription
		if err := json.Unmarshal(body, &update); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return s.cumulativeUserTranscriptDelta(update)
	}
	if envelope.Type == "conversation.item.input_audio_transcription.completed" {
		var completed xAIInputTranscription
		if err := json.Unmarshal(body, &completed); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		// xAI re-emits .completed for the same item while the transcript is
		// still being refined, marking the non-final ones status=in_progress.
		// Its docs describe .completed as the single terminal event and never
		// mention status, so the field is an undocumented narrowing: trust it
		// when present and fall back to the documented contract when absent.
		// Treating every .completed as terminal appends one user message per
		// refinement, which is what duplicated voice turns in history were.
		if completed.Status != "" && completed.Status != "completed" {
			return s.cumulativeUserTranscriptDelta(completed)
		}
		s.transcriptMu.Lock()
		delete(s.transcripts, completed.itemKey())
		s.transcriptMu.Unlock()
	}
	aliases := map[string]string{
		"response.output_audio.delta":            "response.audio.delta",
		"response.output_audio_transcript.delta": "response.audio_transcript.delta",
		"response.output_audio_transcript.done":  "response.audio_transcript.done",
		"response.output_text.delta":             "response.text.delta",
	}
	if replacement, ok := aliases[envelope.Type]; ok {
		var event map[string]any
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		event["type"] = replacement
		body, _ = json.Marshal(event)
	}
	return translateXAIEventBase(body)
}

var _ Provider = XAIProvider{}
var _ TextSession = (*xAISession)(nil)

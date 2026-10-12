package realtimevoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
)

const (
	DefaultQwenRealtimeModel = "qwen-audio-3.0-realtime-plus"
	DefaultQwenRealtimeVoice = "longanqian"
)

type QwenProvider struct {
	WorkspaceID string
	Region      string
	Endpoint    string
	Dialer      *websocket.Dialer
	EventBuffer int
}

func (p QwenProvider) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("qwen realtime: APIKey is required")
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultQwenRealtimeModel
	}
	endpoint, err := p.endpoint(model)
	if err != nil {
		return nil, err
	}
	header := make(http.Header, 2)
	header.Set("Authorization", bearer(cfg.APIKey))
	if p.WorkspaceID != "" {
		header.Set("X-DashScope-WorkSpace", p.WorkspaceID)
	}
	dialer := p.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("qwen realtime websocket connect: %w", err)
	}
	transport := newJSONWebSocketTransport(conn, "qwen realtime", p.EventBuffer)
	session := &qwenSession{
		jsonRealtimeSession: newJSONRealtimeSession(transport, newPCM16SessionInfo(cfg.SessionID, 16000, 24000, Capabilities{EmitsSpeechEvents: true, ExplicitToolContinuation: true})),
	}
	transport.start(func(body []byte) []Event {
		event, ok := translateQwenEvent(body)
		if !ok {
			return nil
		}
		return []Event{event}
	})
	if err := session.waitReady(ctx, 1); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.writeJSON(ctx, buildQwenSessionUpdate(cfg)); err != nil {
		_ = session.Close()
		return nil, err
	}
	if err := session.waitReady(ctx, 1); err != nil {
		_ = session.Close()
		return nil, err
	}
	return session, nil
}

func (p QwenProvider) endpoint(model string) (string, error) {
	raw := strings.TrimSpace(p.Endpoint)
	if raw == "" {
		host := "dashscope.aliyuncs.com"
		switch p.Region {
		case "cn-beijing":
			if p.WorkspaceID != "" {
				host = p.WorkspaceID + ".cn-beijing.maas.aliyuncs.com"
			}
		case "ap-southeast-1":
			if p.WorkspaceID != "" {
				host = p.WorkspaceID + ".ap-southeast-1.maas.aliyuncs.com"
			} else {
				host = "dashscope-intl.aliyuncs.com"
			}
		case "":
		default:
			return "", fmt.Errorf("qwen realtime: unsupported region %q", p.Region)
		}
		raw = "wss://" + host + "/api-ws/v1/realtime"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("qwen realtime: parse endpoint: %w", err)
	}
	query := u.Query()
	query.Set("model", model)
	u.RawQuery = query.Encode()
	return u.String(), nil
}

type qwenSessionUpdate struct {
	Type    string              `json:"type"`
	Session qwenSessionSettings `json:"session"`
}

type qwenSessionSettings struct {
	Modalities          []string           `json:"modalities,omitempty"`
	Voice               string             `json:"voice,omitempty"`
	EnableSpeechEmotion *bool              `json:"enable_speech_emotion,omitempty"`
	Instructions        string             `json:"instructions,omitempty"`
	InputAudioFormat    string             `json:"input_audio_format,omitempty"`
	OutputAudioFormat   string             `json:"output_audio_format,omitempty"`
	MaxHistoryTurns     int                `json:"max_history_turns,omitempty"`
	Tools               []qwenTool         `json:"tools,omitempty"`
	TurnDetection       *qwenTurnDetection `json:"turn_detection,omitempty"`
}

type qwenTurnDetection struct {
	Type              string   `json:"type,omitempty"`
	Threshold         *float64 `json:"threshold,omitempty"`
	SilenceDurationMS int      `json:"silence_duration_ms,omitempty"`
}

type qwenTool struct {
	Type     string                 `json:"type"`
	Function qwenFunctionDefinition `json:"function"`
}

type qwenFunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func buildQwenSessionUpdate(cfg SessionConfig) qwenSessionUpdate {
	settings := qwenSessionSettings{
		Modalities:          []string{"audio", "text"},
		Voice:               cfg.Voice,
		EnableSpeechEmotion: cfg.EnableSpeechEmotion,
		Instructions:        cfg.Instructions,
		InputAudioFormat:    cfg.InputAudioFormat,
		OutputAudioFormat:   cfg.OutputAudioFormat,
		MaxHistoryTurns:     cfg.MaxHistoryTurns,
		Tools:               make([]qwenTool, 0, len(cfg.Tools)),
	}
	if settings.Voice == "" {
		settings.Voice = DefaultQwenRealtimeVoice
	}
	if cfg.TurnDetection != "" {
		settings.TurnDetection = &qwenTurnDetection{
			Type:              cfg.TurnDetection,
			Threshold:         cfg.TurnDetectionThresh,
			SilenceDurationMS: cfg.TurnDetectionSilenceMs,
		}
	}
	for _, tool := range cfg.Tools {
		settings.Tools = append(settings.Tools, qwenTool{
			Type: "function",
			Function: qwenFunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
			},
		})
	}
	return qwenSessionUpdate{Type: "session.update", Session: settings}
}

type qwenSession struct {
	*jsonRealtimeSession
}

func (s *qwenSession) Interrupt(ctx context.Context, interruption ResponseInterruption) error {
	if interruption.ServerDetected {
		return nil
	}
	return s.cancelResponse(ctx)
}

func translateQwenEvent(body []byte) (Event, bool) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Event{Kind: EventError, Error: fmt.Errorf("qwen realtime: decode event: %w", err)}, true
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
			Reason     string `json:"reason"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventSpeechStopped, ItemID: event.ItemID, AudioEndMS: event.AudioEndMS, Status: event.Reason}, true
	case "input_audio_buffer.committed":
		var event struct {
			ItemID         string `json:"item_id"`
			PreviousItemID string `json:"previous_item_id"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventInputCommitted, ItemID: event.ItemID, PreviousItemID: event.PreviousItemID}, true
	case "conversation.item.input_audio_transcription.completed":
		var event struct {
			ItemID     string `json:"item_id"`
			Transcript string `json:"transcript"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptFinal, ItemID: event.ItemID, Role: "user", Text: event.Transcript, TextSource: "audio", Final: true}, true
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
	case "response.text.delta", "response.audio_transcript.delta", "response.audio.delta":
		var event struct {
			ResponseID string `json:"response_id"`
			ItemID     string `json:"item_id"`
			Delta      string `json:"delta"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		if envelope.Type == "response.audio.delta" {
			pcm, err := base64.StdEncoding.DecodeString(event.Delta)
			if err != nil {
				return Event{Kind: EventError, Error: err}, true
			}
			return Event{Kind: EventAudio, ResponseID: event.ResponseID, ItemID: event.ItemID, PCM: pcm}, true
		}
		textSource := "text"
		if envelope.Type == "response.audio_transcript.delta" {
			textSource = "audio"
		}
		return Event{Kind: EventTranscriptDelta, ResponseID: event.ResponseID, ItemID: event.ItemID, Role: "assistant", Text: event.Delta, TextSource: textSource}, true
	case "response.audio_transcript.done":
		var event struct {
			ResponseID string `json:"response_id"`
			ItemID     string `json:"item_id"`
			Transcript string `json:"transcript"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventTranscriptFinal, ResponseID: event.ResponseID, ItemID: event.ItemID, Role: "assistant", Text: event.Transcript, TextSource: "audio", Final: true}, true
	case "response.function_call_arguments.done":
		var event struct {
			ResponseID string `json:"response_id"`
			ItemID     string `json:"item_id"`
			CallID     string `json:"call_id"`
			Name       string `json:"name"`
			Arguments  string `json:"arguments"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		return Event{Kind: EventToolCall, ResponseID: event.ResponseID, ItemID: event.ItemID, CallID: event.CallID, Name: event.Name, Arguments: event.Arguments}, true
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
		return terminalResponseEvent("qwen", event.Response.ID, event.Response.Status, usage), true
	case "error":
		var event struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
				Param   string `json:"param"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return Event{Kind: EventError, Error: err}, true
		}
		if isQwenNoActiveResponseCancelError(event.Error.Type, event.Error.Param, event.Error.Message) {
			return Event{}, false
		}
		return Event{Kind: EventError, Error: classifyServerErrorMessage(event.Error.Message)}, true
	default:
		return Event{}, false
	}
}

func isQwenNoActiveResponseCancelError(errorType, param, message string) bool {
	if !strings.EqualFold(strings.TrimSpace(errorType), "invalid_request_error") {
		return false
	}
	if param = strings.TrimSpace(param); param != "" && param != "response.cancel" {
		return false
	}
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(message)), ".")
	return normalized == "conversation has no active response"
}

var _ Provider = QwenProvider{}
var _ TextSession = (*qwenSession)(nil)

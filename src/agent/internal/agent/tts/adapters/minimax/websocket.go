package minimax

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/logging"
)

const (
	wsEndpoint    = "wss://api.minimax.io/ws/v1/t2a_v2"
	wsEndpointCN  = "wss://api.minimaxi.com/ws/v1/t2a_v2"
	wsConnTimeout = 10 * time.Second
)

// WebSocketAdapter implements TTSProvider using Minimax WebSocket API.
type WebSocketAdapter struct {
	cfg commonConfig
}

var _ tts.TTSProvider = (*WebSocketAdapter)(nil)

// NewWebSocket creates a Minimax WebSocket provider.
func NewWebSocket(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	provider := normalizeProvider(cfg.Provider)
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("%s: api_key is required", provider)
	}
	endpoint, err := defaultEndpointForProvider(provider)
	if err != nil {
		return nil, err
	}
	cfg.Provider = provider
	return &WebSocketAdapter{cfg: parseConfig(cfg, endpoint)}, nil
}

func (a *WebSocketAdapter) Name() string                   { return a.cfg.provider }
func (a *WebSocketAdapter) Capabilities() tts.Capabilities { return capabilities() }
func (a *WebSocketAdapter) Close() error                   { return nil }

func (a *WebSocketAdapter) BeginStream(ctx context.Context, sink tts.AudioSink) (tts.StreamSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return &wsSession{
		ctx:  ctx,
		cfg:  a.cfg,
		sink: sink,
		buf:  &sentenceBuffer{},
	}, nil
}

// wsSession buffers text until sentence boundaries, then performs a full
// task_start → task_continue → receive audio → task_finish cycle per sentence.
type wsSession struct {
	ctx  context.Context
	cfg  commonConfig
	sink tts.AudioSink
	buf  *sentenceBuffer

	errMu   sync.Mutex
	lastErr error
}

func (s *wsSession) WriteText(text string) error {
	chunks := s.buf.Write(text)
	for _, chunk := range chunks {
		if err := s.synthesizeChunk(chunk); err != nil {
			s.recordErr(err)
			return err
		}
	}
	return nil
}

func (s *wsSession) Flush() error {
	if rest := s.buf.Flush(); rest != "" {
		if err := s.synthesizeChunk(rest); err != nil {
			s.recordErr(err)
			return err
		}
	}
	return nil
}

// ResetBuffer discards any text buffered but not yet synthesized. This is used
// to drop residual content streamed during a turn that ultimately returned a
// tool call rather than a streamed speech, so it cannot leak into the next turn.
func (s *wsSession) ResetBuffer() {
	if s.buf != nil {
		s.buf.Reset()
	}
}

func (s *wsSession) Close() error {
	if err := s.Flush(); err != nil {
		return err
	}
	if err := s.sink.Drain(s.ctx); err != nil {
		s.recordErr(err)
	}
	return s.Err()
}

func (s *wsSession) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

func (s *wsSession) recordErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

// synthesizeChunk performs one full WebSocket TTS cycle for a sentence.
func (s *wsSession) synthesizeChunk(text string) error {
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	format := s.sink.Format()
	sampleRate := format.SampleRate
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}
	channels := format.Channels
	if channels <= 0 {
		channels = defaultChannels
	}

	conn, err := s.dial()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// task_start
	startMsg := map[string]any{
		"event": "task_start",
		"model": s.cfg.model,
		"voice_setting": map[string]any{
			"voice_id": s.cfg.voiceID,
			"speed":    s.cfg.speed,
			"vol":      1.0,
			"pitch":    0,
			"emotion":  s.cfg.emotion,
		},
		"audio_setting": map[string]any{
			"sample_rate": sampleRate,
			"format":      "pcm",
			"channel":     channels,
		},
	}
	if err := conn.WriteJSON(startMsg); err != nil {
		return fmt.Errorf("write task_start: %w", err)
	}
	var startResp map[string]any
	if err := readJSONWithContext(s.ctx, conn, &startResp); err != nil {
		return fmt.Errorf("read task_started: %w", err)
	}
	if event, _ := startResp["event"].(string); event != "task_started" {
		return fmt.Errorf("unexpected: %v", startResp)
	}

	// task_continue
	if err := conn.WriteJSON(map[string]any{"event": "task_continue", "text": text}); err != nil {
		return fmt.Errorf("write task_continue: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"event": "task_finish"}); err != nil {
		return fmt.Errorf("write task_finish: %w", err)
	}

	// receive audio
	wroteAudio := false
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}
		var resp map[string]any
		if err := readJSONWithContext(s.ctx, conn, &resp); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) && wroteAudio {
				logging.Infof("agent", "tts", "%s: synthesized %d chars", s.cfg.provider, len(text))
				return nil
			}
			return fmt.Errorf("read audio: %w", err)
		}
		if event, _ := resp["event"].(string); event != "" {
			switch event {
			case "task_finished":
				logging.Infof("agent", "tts", "%s: synthesized %d chars", s.cfg.provider, len(text))
				return nil
			case "task_failed":
				return fmt.Errorf("task failed: %s", minimaxResponseMessage(resp))
			}
		}
		if data, ok := resp["data"].(map[string]any); ok {
			if audioHex, ok := data["audio"].(string); ok && audioHex != "" {
				pcm, err := hex.DecodeString(audioHex)
				if err != nil {
					return fmt.Errorf("decode audio hex: %w", err)
				}
				if len(pcm) > 0 {
					if err := s.sink.WritePCM(pcm); err != nil {
						return err
					}
					wroteAudio = true
				}
			}
		}
	}
}

func (s *wsSession) dial() (*websocket.Conn, error) {
	dialer, err := websocketDialerForConfig(s.cfg)
	if err != nil {
		return nil, err
	}
	header := map[string][]string{
		"Authorization": {"Bearer " + s.cfg.apiKey},
	}
	connCtx, cancel := context.WithTimeout(s.ctx, wsConnTimeout)
	defer cancel()

	conn, _, err := dialer.DialContext(connCtx, s.cfg.endpoint, header)
	if err != nil {
		return nil, err
	}
	// Wait for connected_success
	var connResp map[string]any
	if err := readJSONWithContext(s.ctx, conn, &connResp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read connected: %w", err)
	}
	if event, _ := connResp["event"].(string); event != "connected_success" {
		conn.Close()
		return nil, fmt.Errorf("unexpected connect response: %v", connResp)
	}
	return conn, nil
}

func minimaxResponseMessage(resp map[string]any) string {
	if base, ok := resp["base_resp"].(map[string]any); ok {
		if msg, ok := base["status_msg"].(string); ok && msg != "" {
			return msg
		}
		if msg, ok := base["message"].(string); ok && msg != "" {
			return msg
		}
	}
	if msg, ok := resp["message"].(string); ok && msg != "" {
		return msg
	}
	return fmt.Sprint(resp)
}

func readJSONWithContext(ctx context.Context, conn *websocket.Conn, v any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	err := conn.ReadJSON(v)
	close(done)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

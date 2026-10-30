// Package alicloud implements a TTS adapter for Alibaba Cloud's
// Qwen-TTS-Realtime API.
//
// The API uses an OpenAI-Realtime-style JSON-over-WebSocket protocol:
//   - Connect with Authorization: Bearer <api_key>
//   - Server sends `session.created`
//   - Client sends `session.update` to configure (voice, sample_rate, mode)
//   - Client streams text via repeated `input_text_buffer.append` events
//   - Server emits audio chunks via `response.audio.delta` (base64 PCM)
//   - Client sends `session.finish` to flush and close
//
// Domestic endpoint (no proxy required from mainland China):
//
//	wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=<model>
package alicloud

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/logging"
)

const (
	ProviderName      = "alicloud"
	defaultEndpoint   = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	defaultModel      = "qwen-tts-realtime"
	defaultSampleRate = 24000
	defaultVoice      = "Cherry"
	connectTimeout    = 10 * time.Second
)

func init() {
	tts.Register(ProviderName, New)
}

// Adapter is the Qwen-TTS-Realtime provider.
type Adapter struct {
	apiKey   string
	model    string
	voice    string
	endpoint string
	speed    float64
	language string
}

var _ tts.TTSProvider = (*Adapter)(nil)

// New constructs the adapter from the unified config.
func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("alicloud: api_key is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	model := defaultModel
	if m, ok := cfg.Extra["model"].(string); ok && m != "" {
		model = m
	}
	voice := cfg.Voice
	if voice == "" {
		voice = defaultVoice
	}
	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}
	language := "Auto"
	if l, ok := cfg.Extra["language_type"].(string); ok && l != "" {
		language = l
	} else if cfg.Language != "" {
		language = mapBCP47ToQwenLanguage(cfg.Language)
	}
	return &Adapter{
		apiKey:   cfg.APIKey,
		model:    model,
		voice:    voice,
		endpoint: endpoint,
		speed:    speed,
		language: language,
	}, nil
}

func (a *Adapter) Name() string { return ProviderName }

func (a *Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportsContextContinuation: false,
		SupportedSampleRates:        []int{24000}, // Qwen-TTS-Realtime fixed
		RegionRestricted:            false,
		TextGranularity:             tts.TextGranularityIncremental,
		SupportsIncrementalAudio:    true,
		SupportsFlush:               true,
		SupportsCancellation:        true,
	}
}

func (a *Adapter) Close() error { return nil }

func (a *Adapter) BeginStream(ctx context.Context, sink tts.AudioSink) (tts.StreamSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	s := &session{
		ctx:      ctx,
		conn:     conn,
		sink:     sink,
		readDone: make(chan error, 1),
	}

	// Wait for session.created from server.
	if err := s.waitForSessionCreated(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session.created: %w", err)
	}

	// Send session.update with our configuration.
	if err := s.sendSessionUpdate(a.model, a.voice, a.language, a.speed); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session.update: %w", err)
	}

	go s.readLoop()
	return s, nil
}

func (a *Adapter) dial(ctx context.Context) (*websocket.Conn, error) {
	u, err := url.Parse(a.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	q := u.Query()
	q.Set("model", a.model)
	u.RawQuery = q.Encode()

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		HandshakeTimeout: connectTimeout,
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.apiKey)

	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	conn, _, err := dialer.DialContext(connCtx, u.String(), header)
	if err != nil {
		return nil, err
	}
	logging.Infof("agent", "tts", "alicloud: connected")
	return conn, nil
}

// session is one Qwen-TTS-Realtime streaming session.
type session struct {
	ctx  context.Context
	conn *websocket.Conn
	sink tts.AudioSink

	writeMu    sync.Mutex
	readDone   chan error
	responseMu sync.Mutex
	pending    *pendingResponse

	closeOnce sync.Once
	errMu     sync.Mutex
	lastErr   error
}

type pendingResponse struct {
	done chan struct{}
	err  error
	once sync.Once
}

// waitForSessionCreated reads the initial server event before sending anything.
func (s *session) waitForSessionCreated() error {
	_, raw, err := s.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	if t, _ := ev["type"].(string); t != "session.created" {
		return fmt.Errorf("unexpected first event: %v", ev)
	}
	return nil
}

func (s *session) sendSessionUpdate(model, voice, language string, speed float64) error {
	sessionCfg := map[string]any{
		"voice":           voice,
		"mode":            "server_commit", // server decides synthesis timing
		"language_type":   language,
		"response_format": "pcm",
		"sample_rate":     defaultSampleRate,
	}
	// speech_rate is supported by Qwen3-TTS series only; harmless to include
	// for compatible models. The Qwen-TTS-Realtime base series silently ignores it.
	if speed != 1.0 {
		sessionCfg["speech_rate"] = speed
	}
	return s.writeEvent(map[string]any{
		"event_id": newEventID(),
		"type":     "session.update",
		"session":  sessionCfg,
	})
}

// WriteText pushes a text fragment. In server_commit mode, the server buffers
// and decides when to start synthesis.
func (s *session) WriteText(text string) error {
	if text == "" {
		return nil
	}
	if err := s.waitForPendingResponse(); err != nil {
		return err
	}
	return s.writeEvent(map[string]any{
		"event_id": newEventID(),
		"type":     "input_text_buffer.append",
		"text":     text,
	})
}

// Flush forces immediate synthesis of the current buffer.
func (s *session) Flush() error {
	if err := s.waitForPendingResponse(); err != nil {
		return err
	}
	pending := &pendingResponse{done: make(chan struct{})}
	s.responseMu.Lock()
	s.pending = pending
	s.responseMu.Unlock()
	err := s.writeEvent(map[string]any{
		"event_id": newEventID(),
		"type":     "input_text_buffer.commit",
	})
	if err != nil {
		s.completePendingResponse(err)
	}
	return err
}

func (s *session) writeEvent(ev map[string]any) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return tts.ErrSessionClosed
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *session) readLoop() {
	for {
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			s.finishRead(err)
			return
		}
		var ev map[string]any
		if err := json.Unmarshal(raw, &ev); err != nil {
			s.finishRead(fmt.Errorf("unmarshal: %w", err))
			return
		}
		t, _ := ev["type"].(string)
		switch t {
		case "response.audio.delta":
			// {"type":"response.audio.delta","delta":"<base64 pcm>"}
			deltaStr, _ := ev["delta"].(string)
			if deltaStr == "" {
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(deltaStr)
			if err != nil {
				s.finishRead(fmt.Errorf("decode audio: %w", err))
				return
			}
			if len(pcm) > 0 {
				if err := s.sink.WritePCM(pcm); err != nil {
					s.finishRead(fmt.Errorf("write pcm: %w", err))
					return
				}
			}
		case "response.audio.done":
			// A response may be followed by another commit in the same session.
		case "response.done":
			s.completePendingResponse(nil)
		case "session.finished":
			s.completePendingResponse(nil)
			s.readDone <- nil
			return
		case "error":
			msg, _ := ev["error"].(map[string]any)
			s.finishRead(fmt.Errorf("server error: %v", msg))
			return
		case "session.updated", "input_text_buffer.committed",
			"response.created", "response.audio_transcript.delta",
			"response.audio_transcript.done":
			// informational, ignore
		}
	}
}

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		if err := s.waitForPendingResponse(); err != nil {
			s.recordErr(err)
		}
		// Tell server no more text is coming; it flushes and closes.
		if err := s.writeEvent(map[string]any{
			"event_id": newEventID(),
			"type":     "session.finish",
		}); err != nil {
			s.recordErr(fmt.Errorf("session.finish: %w", err))
		}

		// Wait for reader to drain remaining audio.
		if err := s.waitReadDone(); err != nil {
			s.recordErr(err)
		}

		// Wait for playback to drain.
		if err := s.sink.Drain(context.Background()); err != nil {
			s.recordErr(fmt.Errorf("drain: %w", err))
		}

		s.writeMu.Lock()
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
		}
		s.writeMu.Unlock()
	})
	return s.Err()
}

func (s *session) Abort() error {
	s.closeOnce.Do(func() {
		if err := s.sink.Stop(); err != nil {
			s.recordErr(fmt.Errorf("abort stop: %w", err))
		}

		s.writeMu.Lock()
		if s.conn != nil {
			if err := s.conn.Close(); err != nil {
				s.recordErr(fmt.Errorf("abort close: %w", err))
			}
			s.conn = nil
		}
		s.writeMu.Unlock()

		// Wait for reader to exit (will error out on closed connection).
		_ = s.waitReadDone()
	})
	return s.Err()
}

func (s *session) waitForPendingResponse() error {
	s.responseMu.Lock()
	pending := s.pending
	s.responseMu.Unlock()
	if pending == nil {
		return nil
	}
	select {
	case <-pending.done:
		s.responseMu.Lock()
		if s.pending == pending {
			s.pending = nil
		}
		s.responseMu.Unlock()
		return pending.err
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *session) completePendingResponse(err error) {
	s.responseMu.Lock()
	pending := s.pending
	s.responseMu.Unlock()
	if pending == nil {
		return
	}
	pending.once.Do(func() {
		pending.err = err
		close(pending.done)
	})
}

func (s *session) finishRead(err error) {
	s.completePendingResponse(err)
	s.readDone <- err
}

func (s *session) waitReadDone() error {
	waitCtx, cancel := context.WithTimeout(s.ctx, connectTimeout)
	defer cancel()
	select {
	case err := <-s.readDone:
		return err
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

func (s *session) recordErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

func newEventID() string {
	return "event_" + uuid.NewString()
}

// mapBCP47ToQwenLanguage converts BCP-47 codes to Qwen language_type values.
func mapBCP47ToQwenLanguage(bcp47 string) string {
	switch bcp47 {
	case "zh-CN", "zh", "zh-TW", "zh-HK":
		return "Chinese"
	case "en", "en-US", "en-GB":
		return "English"
	case "ja", "ja-JP":
		return "Japanese"
	case "ko", "ko-KR":
		return "Korean"
	case "fr", "fr-FR":
		return "French"
	case "de", "de-DE":
		return "German"
	case "es", "es-ES":
		return "Spanish"
	case "it", "it-IT":
		return "Italian"
	case "pt", "pt-BR", "pt-PT":
		return "Portuguese"
	case "ru", "ru-RU":
		return "Russian"
	default:
		return "Auto"
	}
}

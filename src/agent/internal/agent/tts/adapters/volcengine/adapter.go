// Package volcengine implements a TTS adapter for Volcengine's
// WebSocket bidirectional streaming V3 API.
package volcengine

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/logging"
)

const (
	ProviderName      = "volcengine"
	defaultEndpoint   = "wss://openspeech.bytedance.com/api/v3/tts/bidirection"
	defaultResourceID = "seed-tts-2.0"
	defaultSpeaker    = "zh_female_vv_uranus_bigtts"
	connectTimeout    = 10 * time.Second
	synthesisTimeout  = 60 * time.Second
	defaultUserID     = "aico-agent"
	statusCodeOK      = 20000000
)

func init() {
	tts.Register(ProviderName, New)
}

type Adapter struct {
	apiKey     string
	resourceID string
	speaker    string
	emotion    string
	endpoint   string
	proxy      tts.ProxyConfig
	speed      float64
}

var _ tts.TTSProvider = (*Adapter)(nil)

func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("volcengine: api_key is required")
	}
	resourceID := defaultResourceID
	if model, ok := cfg.Extra["model"].(string); ok && strings.TrimSpace(model) != "" {
		resourceID = strings.TrimSpace(model)
	}
	speaker := strings.TrimSpace(cfg.Voice)
	if speaker == "" {
		speaker = defaultSpeaker
	}
	emotion, _ := cfg.Extra["emotion"].(string)
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}
	return &Adapter{
		apiKey:     cfg.APIKey,
		resourceID: resourceID,
		speaker:    speaker,
		emotion:    emotion,
		endpoint:   endpoint,
		proxy:      cfg.Proxy,
		speed:      speed,
	}, nil
}

func (a *Adapter) Name() string { return ProviderName }

func (a *Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportedSampleRates:     []int{8000, 16000, 22050, 24000, 32000, 44100, 48000},
		TextGranularity:          tts.TextGranularityIncremental,
		SupportsIncrementalAudio: true,
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
		ctx:       ctx,
		conn:      conn,
		sink:      sink,
		sessionID: uuid.NewString(),
		readDone:  make(chan error, 1),
	}
	if err := s.sendEvent(eventStartConnection, "", []byte("{}")); err != nil {
		conn.Close()
		return nil, fmt.Errorf("start connection: %w", err)
	}
	if err := s.waitForEvent(ctx, eventConnectionStarted); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connection started: %w", err)
	}
	if err := s.sendEvent(eventStartSession, s.sessionID, a.startSessionPayload(sink.Format())); err != nil {
		conn.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}
	if err := s.waitForEvent(ctx, eventSessionStarted); err != nil {
		conn.Close()
		return nil, fmt.Errorf("session started: %w", err)
	}
	go s.readLoop()
	return s, nil
}

func (a *Adapter) dial(ctx context.Context) (*websocket.Conn, error) {
	dialer, err := a.websocketDialer()
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("X-Api-Key", a.apiKey)
	header.Set("X-Api-Resource-Id", a.resourceID)
	header.Set("X-Api-Connect-Id", uuid.NewString())
	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, resp, err := dialer.DialContext(connCtx, a.endpoint, header)
	if err != nil {
		return nil, err
	}
	if resp != nil && resp.Header.Get("X-Tt-Logid") != "" {
		logging.Infof("agent", "tts", "volcengine: connected logid=%s", resp.Header.Get("X-Tt-Logid"))
	} else {
		logging.Infof("agent", "tts", "volcengine: connected")
	}
	return conn, nil
}

func (a *Adapter) websocketDialer() (websocket.Dialer, error) {
	dialer := websocket.Dialer{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, HandshakeTimeout: connectTimeout}
	err := a.proxy.ConfigureWebSocketDialer(&dialer)
	return dialer, err
}

func (a *Adapter) startSessionPayload(format tts.AudioFormat) []byte {
	audioParams := map[string]any{
		"format":      "pcm",
		"sample_rate": format.SampleRate,
	}
	if a.emotion != "" {
		audioParams["emotion"] = a.emotion
	}
	if a.speed != 1.0 {
		audioParams["speech_rate"] = int((a.speed - 1.0) * 100)
	}
	payload, _ := json.Marshal(map[string]any{
		"user": map[string]any{"uid": defaultUserID},
		"req_params": map[string]any{
			"speaker":      a.speaker,
			"audio_params": audioParams,
		},
	})
	return payload
}

type session struct {
	ctx       context.Context
	conn      *websocket.Conn
	sink      tts.AudioSink
	sessionID string

	writeMu  sync.Mutex
	readDone chan error

	closeOnce sync.Once
	errMu     sync.Mutex
	lastErr   error
}

func (s *session) WriteText(text string) error {
	if text == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"req_params": map[string]any{"text": text}})
	if err != nil {
		return err
	}
	return s.sendEvent(eventTaskRequest, s.sessionID, payload)
}

func (s *session) Flush() error { return nil }

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		if err := s.sendEvent(eventFinishSession, s.sessionID, []byte("{}")); err != nil {
			s.recordErr(fmt.Errorf("finish session: %w", err))
		}
		finishCtx, finishCancel := context.WithTimeout(s.ctx, synthesisTimeout)
		if err := s.waitReadDone(finishCtx); err != nil {
			s.recordErr(err)
		}
		finishCancel()
		if err := s.sink.Drain(s.ctx); err != nil {
			s.recordErr(fmt.Errorf("drain: %w", err))
		}
		_ = s.sendEvent(eventFinishConnection, "", []byte("{}"))
		closeCtx, cancel := context.WithTimeout(s.ctx, connectTimeout)
		defer cancel()
		_ = s.waitForEvent(closeCtx, eventConnectionFinished)
		s.closeConn()
	})
	return s.Err()
}

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

func (s *session) sendEvent(event int32, sessionID string, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return tts.ErrSessionClosed
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, encodeClientEvent(event, sessionID, payload))
}

func (s *session) waitForEvent(ctx context.Context, want int32) error {
	_, raw, err := s.readMessage(ctx)
	if err != nil {
		return err
	}
	msg, err := parseServerFrame(raw)
	if err != nil {
		return err
	}
	if msg.messageType == messageTypeError {
		return fmt.Errorf("server error %d: %s", msg.errorCode, string(msg.payload))
	}
	if msg.event != want {
		return fmt.Errorf("unexpected event %d, want %d: %s", msg.event, want, string(msg.payload))
	}
	return validateStatus(msg)
}

func (s *session) readLoop() {
	for {
		_, raw, err := s.readMessage(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				err = s.ctx.Err()
			}
			s.readDone <- err
			return
		}
		msg, err := parseServerFrame(raw)
		if err != nil {
			s.readDone <- err
			return
		}
		switch msg.messageType {
		case messageTypeError:
			s.readDone <- fmt.Errorf("server error %d: %s", msg.errorCode, string(msg.payload))
			return
		case messageTypeAudioOnlyResponse:
			if len(msg.payload) > 0 {
				if err := s.sink.WritePCM(msg.payload); err != nil {
					s.readDone <- fmt.Errorf("write pcm: %w", err)
					return
				}
			}
		case messageTypeFullServerResponse:
			if msg.event == eventSessionFinished {
				s.readDone <- validateStatus(msg)
				return
			}
			if msg.event == eventSessionFailed || msg.event == eventConnectionFailed {
				s.readDone <- fmt.Errorf("server failed: %s", string(msg.payload))
				return
			}
		}
	}
}

func (s *session) readMessage(ctx context.Context) (int, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	conn := s.conn
	if conn == nil {
		return 0, nil, tts.ErrSessionClosed
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-done:
		}
	}()
	messageType, raw, err := conn.ReadMessage()
	close(done)
	if ctx.Err() != nil {
		return messageType, raw, ctx.Err()
	}
	return messageType, raw, err
}

func (s *session) waitReadDone(ctx context.Context) error {
	select {
	case err := <-s.readDone:
		return err
	case <-ctx.Done():
		s.closeConn()
		return ctx.Err()
	}
}

func (s *session) closeConn() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
}

func validateStatus(msg serverMessage) error {
	if len(msg.payload) == 0 || string(msg.payload) == "{}" {
		return nil
	}
	var status struct {
		StatusCode int    `json:"status_code"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(msg.payload, &status); err != nil || status.StatusCode == 0 || status.StatusCode == statusCodeOK {
		return nil
	}
	return fmt.Errorf("server status %d: %s", status.StatusCode, status.Message)
}

func (s *session) recordErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

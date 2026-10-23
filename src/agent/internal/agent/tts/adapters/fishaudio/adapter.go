// Package fishaudio implements a TTS adapter for Fish Audio's WebSocket API.
//
// Fish Audio supports true incremental text streaming: each WriteText pushes
// a text event over the WebSocket immediately, and the server decides when
// to begin synthesis based on its internal buffering policy.
package fishaudio

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"
	"golang.org/x/net/proxy"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/logging"
)

const (
	ProviderName     = "fish-audio"
	defaultEndpoint  = "wss://api.fish.audio/v1/tts/live"
	defaultModel     = "s2-pro"
	connectTimeout   = 10 * time.Second
	synthesisTimeout = 60 * time.Second
)

func init() {
	tts.Register(ProviderName, New)
}

// Adapter is the Fish Audio provider.
type Adapter struct {
	apiKey      string
	referenceID string
	model       string
	endpoint    string
	proxyURL    string
	speed       float64
}

// Compile-time check.
var _ tts.TTSProvider = (*Adapter)(nil)

// New constructs a Fish Audio provider from the unified config.
func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("fish-audio: api_key is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	// Fish Audio uses reference_id exclusively. Do not read cfg.Voice to avoid
	// parameter pollution from other providers (e.g., Minimax voice_id). An
	// empty reference_id uses the same built-in demo voice shown by Config Web.
	referenceID := tts.DefaultFishAudioReferenceID
	if extra, ok := cfg.Extra["reference_id"].(string); ok && extra != "" {
		referenceID = extra
	}
	model := defaultModel
	if extra, ok := cfg.Extra["model"].(string); ok && extra != "" {
		model = extra
	}
	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}
	return &Adapter{
		apiKey:      cfg.APIKey,
		referenceID: referenceID,
		model:       model,
		endpoint:    endpoint,
		proxyURL:    cfg.Proxy.AllProxy,
		speed:       speed,
	}, nil
}

func (a *Adapter) Name() string { return ProviderName }

func (a *Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportsContextContinuation: false,
		SupportedSampleRates:        []int{16000, 24000, 44100},
		RegionRestricted:            true,
	}
}

func (a *Adapter) Close() error { return nil }

// BeginStream opens a fresh WebSocket connection for the session.
// One connection per session keeps things simple and avoids head-of-line
// blocking when multiple chats run concurrently.
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

	if err := s.sendStart(a.referenceID, sink.Format(), a.speed); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send start: %w", err)
	}

	go s.readLoop()
	return s, nil
}

func (a *Adapter) dial(ctx context.Context) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		HandshakeTimeout: connectTimeout,
	}
	if err := configureProxy(&dialer, a.proxyURL); err != nil {
		return nil, err
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+a.apiKey)
	header.Set("model", a.model)

	connCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	conn, _, err := dialer.DialContext(connCtx, a.endpoint, header)
	if err != nil {
		return nil, err
	}
	logging.Infof("agent", "tts", "fish-audio: connected")
	return conn, nil
}

func configureProxy(dialer *websocket.Dialer, proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy: %w", err)
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return fmt.Errorf("socks5: %w", err)
		}
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.(proxy.ContextDialer).DialContext(ctx, network, addr)
		}
	case "http", "https":
		dialer.Proxy = func(req *http.Request) (*url.URL, error) { return u, nil }
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
	return nil
}

// session is one Fish Audio streaming session.
type session struct {
	ctx  context.Context
	conn *websocket.Conn
	sink tts.AudioSink

	writeMu sync.Mutex

	readDone chan error

	closeOnce sync.Once
	lastErr   error
	errMu     sync.Mutex
}

func (s *session) sendStart(referenceID string, format tts.AudioFormat, speed float64) error {
	request := map[string]any{
		"text":                         "",
		"chunk_length":                 200,
		"format":                       "pcm",
		"sample_rate":                  format.SampleRate,
		"mp3_bitrate":                  128,
		"opus_bitrate":                 32,
		"references":                   []any{},
		"normalize":                    true,
		"latency":                      "balanced",
		"top_p":                        0.7,
		"temperature":                  0.7,
		"max_new_tokens":               1024,
		"repetition_penalty":           1.2,
		"min_chunk_length":             50,
		"condition_on_previous_chunks": true,
		"early_stop_threshold":         1.0,
	}
	if referenceID != "" {
		request["reference_id"] = referenceID
	}
	if speed != 1.0 {
		request["prosody"] = map[string]any{"speed": speed, "volume": 0.0}
	}
	msg := map[string]any{
		"event":   "start",
		"request": request,
	}
	return s.writeMsg(msg)
}

func (s *session) WriteText(text string) error {
	if text == "" {
		return nil
	}
	return s.writeMsg(map[string]any{"event": "text", "text": text})
}

func (s *session) Flush() error {
	return s.writeMsg(map[string]any{"event": "flush"})
}

func (s *session) writeMsg(msg map[string]any) error {
	data, err := msgpack.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.conn == nil {
		return tts.ErrSessionClosed
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (s *session) readLoop() {
	for {
		conn := s.conn
		if conn == nil {
			s.readDone <- tts.ErrSessionClosed
			return
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if s.ctx.Err() != nil {
				err = s.ctx.Err()
			}
			s.readDone <- err
			return
		}
		var resp map[string]any
		if err := msgpack.Unmarshal(raw, &resp); err != nil {
			s.readDone <- fmt.Errorf("unmarshal: %w", err)
			return
		}
		event, _ := resp["event"].(string)
		switch event {
		case "audio":
			if pcm, _ := resp["audio"].([]byte); len(pcm) > 0 {
				if err := s.sink.WritePCM(pcm); err != nil {
					s.readDone <- fmt.Errorf("write pcm: %w", err)
					return
				}
			}
		case "finish":
			s.readDone <- nil
			return
		case "error":
			msg, _ := resp["message"].(string)
			s.readDone <- fmt.Errorf("server: %s", msg)
			return
		case "log":
			// server-side log, ignore
		}
	}
}

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		// Flush so server emits remaining audio.
		if err := s.Flush(); err != nil {
			s.recordErr(fmt.Errorf("flush: %w", err))
		}
		// Stop tells the server to send a final "finish" event and end the stream.
		_ = s.writeMsg(map[string]any{"event": "stop"})

		// Wait for the read loop to drain.
		waitCtx, cancel := context.WithTimeout(s.ctx, synthesisTimeout)
		if err := s.waitReadDone(waitCtx); err != nil {
			s.recordErr(err)
		}
		cancel()

		// Drain the playback buffer (waits until audio fully plays).
		if err := s.sink.Drain(s.ctx); err != nil {
			s.recordErr(fmt.Errorf("drain: %w", err))
		}

		s.closeConn()
	})
	return s.Err()
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

func (s *session) recordErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

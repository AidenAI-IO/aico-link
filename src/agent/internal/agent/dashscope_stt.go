package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/stream"
)

const (
	defaultDashScopeEndpoint    = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	defaultDashScopeModel       = "qwen3-asr-flash-realtime"
	dashScopeConnTimeout        = 15 * time.Second
	dashScopePacketMillis       = 100
	dashScopeOneShotReadTimeout = 30 * time.Second
)

// DashScopeRealtimeSTT implements STT using Alibaba DashScope's real-time
// WebSocket API (qwen3-asr-flash-realtime). It supports both streaming upload
// and one-shot TranscribeWAV (which opens a WebSocket, sends all audio, then closes).
type DashScopeRealtimeSTT struct {
	apiKey   string
	model    string
	endpoint string
	language string
	proxy    ProxyConfig
}

// NewDashScopeRealtimeSTT creates a new DashScope real-time STT client.
func NewDashScopeRealtimeSTT(apiKey, model, endpoint, language string) *DashScopeRealtimeSTT {
	if model == "" || !isDashScopeModel(model) {
		model = defaultDashScopeModel
	}
	if endpoint == "" {
		endpoint = defaultDashScopeEndpoint
	}
	return &DashScopeRealtimeSTT{
		apiKey:   apiKey,
		model:    model,
		endpoint: endpoint,
		language: language,
	}
}

// Capabilities reports that this provider supports streaming upload.
func (s *DashScopeRealtimeSTT) Capabilities() STTCapabilities {
	return STTCapabilities{
		SupportsStreamingUpload:       true,
		SupportsInterimResults:        true,
		SupportsSegmentFinals:         true,
		SupportsProviderEndpointing:   true,
		SupportsStreamingCancellation: true,
	}
}

// TranscribeWAV performs one-shot transcription by opening a WebSocket session,
// sending all audio at once, and waiting for the final result.
func (s *DashScopeRealtimeSTT) TranscribeWAV(wavData []byte) (string, error) {
	pcm, sampleRate, err := extractPCMFromWAV(wavData)
	if err != nil {
		return "", fmt.Errorf("dashscope stt: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), dashScopeOneShotReadTimeout)
	defer cancel()

	uploader, err := s.newUploader(ctx, STTStreamConfig{
		SampleRate: sampleRate,
		Channels:   1,
		BitWidth:   16,
	})
	if err != nil {
		return "", fmt.Errorf("dashscope stt one-shot: %w", err)
	}

	if err := uploader.UploadPCM(pcm); err != nil {
		_ = uploader.Close()
		return "", fmt.Errorf("dashscope stt one-shot upload: %w", err)
	}

	transcript, err := uploader.Finalize()
	if err != nil {
		return "", fmt.Errorf("dashscope stt one-shot finalize: %w", err)
	}
	return transcript, nil
}

// NewStreamingUploader creates a new streaming STT session over WebSocket.
func (s *DashScopeRealtimeSTT) NewStreamingUploader(ctx context.Context, cfg STTStreamConfig) (STTStreamUploader, error) {
	return s.newUploader(ctx, cfg)
}

// isDashScopeModel returns true if the model name is a valid DashScope ASR model.
func isDashScopeModel(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "qwen") || strings.Contains(m, "paraformer")
}

func (s *DashScopeRealtimeSTT) buildEndpointURL() string {
	base := strings.TrimRight(s.endpoint, "/")
	if strings.Contains(base, "?") {
		return base + "&model=" + s.model
	}
	return base + "?model=" + s.model
}

func (s *DashScopeRealtimeSTT) newUploader(ctx context.Context, cfg STTStreamConfig) (*dashScopeUploader, error) {
	if s.apiKey == "" {
		return nil, fmt.Errorf("dashscope STT: api_key is required")
	}
	if cfg.SampleRate <= 0 {
		return nil, fmt.Errorf("dashscope STT: sample_rate must be > 0")
	}
	if cfg.Channels != 1 {
		return nil, fmt.Errorf("dashscope STT: only mono audio supported, got %d channels", cfg.Channels)
	}
	if cfg.BitWidth != 16 {
		return nil, fmt.Errorf("dashscope STT: only 16-bit PCM supported, got %d", cfg.BitWidth)
	}

	dialer, err := newProxyWebSocketDialer(s.proxy, dashScopeConnTimeout)
	if err != nil {
		return nil, fmt.Errorf("dashscope STT: configure websocket proxy: %w", err)
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+s.apiKey)

	endpoint := s.buildEndpointURL()
	conn, _, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return nil, fmt.Errorf("dashscope STT: dial websocket: %w", err)
	}

	packetBytes := cfg.SampleRate * cfg.Channels * (cfg.BitWidth / 8) * dashScopePacketMillis / 1000
	if packetBytes <= 0 {
		return nil, fmt.Errorf("dashscope STT: invalid sample rate %d produces zero-byte packets", cfg.SampleRate)
	}

	uploader := &dashScopeUploader{
		conn:        conn,
		packetBytes: packetBytes,
		done:        make(chan struct{}),
		ready:       make(chan struct{}),
		ctx:         ctx,
		events:      make(chan stream.TranscriptEvent, 32),
	}

	go uploader.readLoop()

	// Send session.update to configure the recognition session (manual turn detection).
	session := dashScopeSession{
		Modalities:       []string{"text"},
		InputAudioFormat: "pcm",
		SampleRate:       cfg.SampleRate,
		TurnDetection:    json.RawMessage("null"),
	}
	if s.language != "" {
		switch s.language {
		case "zh":
			session.Language = "zh-CN"
		case "en":
			session.Language = "en-US"
		default:
			session.Language = s.language
		}
	}

	sessionUpdate := dashScopeMessage{
		EventID: "evt_" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Type:    "session.update",
		Session: &session,
	}

	if err := conn.WriteJSON(sessionUpdate); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dashscope STT: send session.update: %w", err)
	}

	// Wait for session.created or session.updated before sending audio.
	select {
	case <-uploader.ready:
	case <-uploader.done:
		return nil, fmt.Errorf("dashscope STT: session setup failed: %v", uploader.err)
	case <-ctx.Done():
		_ = conn.Close()
		return nil, fmt.Errorf("dashscope STT: context cancelled waiting for session ready")
	}

	return uploader, nil
}

// extractPCMFromWAV strips the WAV header and returns raw PCM data and sample rate.
func extractPCMFromWAV(wavData []byte) ([]byte, int, error) {
	if len(wavData) < 44 {
		return nil, 0, fmt.Errorf("WAV data too short (%d bytes)", len(wavData))
	}
	if string(wavData[0:4]) != "RIFF" || string(wavData[8:12]) != "WAVE" {
		return nil, 0, fmt.Errorf("not a valid WAV file")
	}

	// Parse fmt chunk to validate format
	fmtOffset := 12
	for fmtOffset+8 <= len(wavData) {
		chunkID := string(wavData[fmtOffset : fmtOffset+4])
		chunkSize := int(wavData[fmtOffset+4]) | int(wavData[fmtOffset+5])<<8 | int(wavData[fmtOffset+6])<<16 | int(wavData[fmtOffset+7])<<24
		if chunkID == "fmt " {
			if fmtOffset+8+16 > len(wavData) {
				return nil, 0, fmt.Errorf("WAV fmt chunk too short")
			}
			audioFormat := int(wavData[fmtOffset+8]) | int(wavData[fmtOffset+9])<<8
			numChannels := int(wavData[fmtOffset+10]) | int(wavData[fmtOffset+11])<<8
			sampleRate := int(wavData[fmtOffset+12]) | int(wavData[fmtOffset+13])<<8 | int(wavData[fmtOffset+14])<<16 | int(wavData[fmtOffset+15])<<24
			bitsPerSample := int(wavData[fmtOffset+22]) | int(wavData[fmtOffset+23])<<8

			// Validate format: must be PCM (format 1), mono (1 channel), 16-bit
			if audioFormat != 1 {
				return nil, 0, fmt.Errorf("WAV must be PCM format (got format %d)", audioFormat)
			}
			if numChannels != 1 {
				return nil, 0, fmt.Errorf("WAV must be mono (got %d channels)", numChannels)
			}
			if bitsPerSample != 16 {
				return nil, 0, fmt.Errorf("WAV must be 16-bit (got %d bits)", bitsPerSample)
			}

			// Find "data" chunk
			offset := 12
			for offset+8 <= len(wavData) {
				chunkID := string(wavData[offset : offset+4])
				chunkSize := int(wavData[offset+4]) | int(wavData[offset+5])<<8 | int(wavData[offset+6])<<16 | int(wavData[offset+7])<<24
				if chunkID == "data" {
					dataStart := offset + 8
					dataEnd := dataStart + chunkSize
					if dataEnd > len(wavData) {
						dataEnd = len(wavData)
					}
					return wavData[dataStart:dataEnd], sampleRate, nil
				}
				offset += 8 + chunkSize
			}
			return nil, 0, fmt.Errorf("WAV data chunk not found")
		}
		fmtOffset += 8 + chunkSize
	}
	return nil, 0, fmt.Errorf("WAV fmt chunk not found")
}

// --- DashScope protocol message types ---

type dashScopeMessage struct {
	EventID string            `json:"event_id"`
	Type    string            `json:"type"`
	Session *dashScopeSession `json:"session,omitempty"`
	Audio   string            `json:"audio,omitempty"`
}

type dashScopeSession struct {
	Modalities       []string        `json:"modalities"`
	InputAudioFormat string          `json:"input_audio_format"`
	SampleRate       int             `json:"sample_rate"`
	Language         string          `json:"language,omitempty"`
	TurnDetection    json.RawMessage `json:"turn_detection,omitempty"`
}

type dashScopeServerEvent struct {
	EventID    string                 `json:"event_id"`
	Type       string                 `json:"type"`
	ItemID     string                 `json:"item_id,omitempty"`
	Transcript string                 `json:"transcript,omitempty"`
	Text       string                 `json:"text,omitempty"`
	Error      *dashScopeErrorPayload `json:"error,omitempty"`
}

type dashScopeErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

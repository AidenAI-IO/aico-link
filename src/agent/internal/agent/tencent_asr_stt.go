package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/stream"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	tencentASRHost                   = "asr.tencentcloudapi.com"
	tencentASRService                = "asr"
	tencentASRAction                 = "SentenceRecognition"
	tencentASRVersion                = "2019-06-14"
	tencentASRAlgorithm              = "TC3-HMAC-SHA256"
	tencentRealtimeASRHost           = "asr.cloud.tencent.com"
	tencentRealtimeASRPathPrefix     = "/asr/v2/"
	tencentRealtimeASRVoiceFormatPCM = "1"
	tencentRealtimeASRPacketMillis   = 200
	tencentRealtimeASREndMessage     = `{"type":"end"}`
	tencentRealtimeASRURLTTL         = 24 * time.Hour
)

var tencentRealtimeASRConnTimeout = 15 * time.Second

// TencentASRSTT implements Tencent Cloud STT using both sentence recognition
// and realtime WebSocket streaming when app_id is configured.
type TencentASRSTT struct {
	secretID        string
	secretKey       string
	appID           string
	region          string
	engineModelType string
	language        string
	httpClient      *http.Client
	proxy           ProxyConfig
	apiURL          string // optional override for tests
	realtimeURL     string // optional override for tests
}

// NewTencentASRSTT creates a Tencent ASR STT client.
func NewTencentASRSTT(secretID, secretKey, appID, region, engineModelType, language string, httpClients ...*http.Client) *TencentASRSTT {
	if region == "" {
		region = defaultTencentASRRegion
	}
	engineModelType = normalizeTencentASREngineModelType(language, engineModelType)

	httpClient := http.DefaultClient
	if len(httpClients) > 0 && httpClients[0] != nil {
		httpClient = httpClients[0]
	}
	return &TencentASRSTT{
		secretID:        strings.TrimSpace(secretID),
		secretKey:       strings.TrimSpace(secretKey),
		appID:           strings.TrimSpace(appID),
		region:          region,
		engineModelType: engineModelType,
		language:        strings.ToLower(strings.TrimSpace(language)),
		httpClient:      httpClient,
	}
}

func normalizeTencentASREngineModelType(language, engineModelType string) string {
	language = strings.ToLower(strings.TrimSpace(language))
	switch language {
	case "zh":
		return "16k_zh"
	case "en":
		return "16k_en"
	default:
		if strings.TrimSpace(engineModelType) == "" {
			return defaultTencentASREngineModel
		}
		return strings.TrimSpace(engineModelType)
	}
}

func (s *TencentASRSTT) Capabilities() STTCapabilities {
	return STTCapabilities{
		SupportsStreamingUpload:       s.appID != "",
		SupportsInterimResults:        s.appID != "",
		SupportsSegmentFinals:         s.appID != "",
		SupportsProviderEndpointing:   s.appID != "",
		SupportsStreamingCancellation: s.appID != "",
	}
}

func (s *TencentASRSTT) TranscribeWAV(wavData []byte) (string, error) {
	if len(wavData) == 0 {
		return "", fmt.Errorf("empty wav data")
	}
	if s.secretID == "" || s.secretKey == "" {
		return "", fmt.Errorf("tencent ASR: secret_id and secret_key are required")
	}

	payload, err := json.Marshal(tencentASRRequest{
		EngSerViceType: s.engineModelType,
		SourceType:     1,
		VoiceFormat:    "wav",
		Data:           base64.StdEncoding.EncodeToString(wavData),
		DataLen:        len(wavData),
	})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	timestamp := time.Now().Unix()
	authorization, err := buildTencentTC3Authorization(s.secretID, s.secretKey, string(payload), timestamp)
	if err != nil {
		return "", err
	}

	apiURL := s.apiURL
	if apiURL == "" {
		apiURL = "https://" + tencentASRHost
	}
	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Host = tencentASRHost
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("X-TC-Action", tencentASRAction)
	req.Header.Set("X-TC-Version", tencentASRVersion)
	req.Header.Set("X-TC-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("X-TC-Region", s.region)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var parsed tencentASRResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if parsed.Response.Error != nil {
		msg := strings.TrimSpace(parsed.Response.Error.Message)
		if msg == "" {
			msg = strings.TrimSpace(parsed.Response.Error.Code)
		}
		return "", fmt.Errorf("tencent ASR API error: %s", msg)
	}
	text := strings.TrimSpace(parsed.Response.Result)
	if text == "" {
		return "", fmt.Errorf("tencent ASR returned empty transcript")
	}
	return text, nil
}

func (s *TencentASRSTT) NewStreamingUploader(ctx context.Context, cfg STTStreamConfig) (STTStreamUploader, error) {
	if !s.Capabilities().SupportsStreamingUpload {
		return nil, fmt.Errorf("tencent ASR realtime upload requires stt.app_id")
	}
	if s.secretID == "" || s.secretKey == "" {
		return nil, fmt.Errorf("tencent ASR: secret_id and secret_key are required")
	}
	if cfg.SampleRate <= 0 {
		return nil, fmt.Errorf("streaming STT requires sample_rate > 0")
	}
	if cfg.Channels != 1 {
		return nil, fmt.Errorf("streaming STT requires mono PCM, got %d channel(s)", cfg.Channels)
	}
	if cfg.BitWidth != 16 {
		return nil, fmt.Errorf("streaming STT requires 16-bit PCM, got %d", cfg.BitWidth)
	}

	endpoint, err := s.buildRealtimeEndpoint(cfg)
	if err != nil {
		return nil, err
	}

	dialer, err := newProxyWebSocketDialer(s.proxy, tencentRealtimeASRConnTimeout)
	if err != nil {
		return nil, fmt.Errorf("configure websocket proxy: %w", err)
	}

	conn, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("dial realtime ASR websocket: %w", err)
	}

	uploader := &tencentASRStreamingUploader{
		conn:        conn,
		packetBytes: packetBytesForAudioFormat(cfg),
		done:        make(chan struct{}),
		events:      make(chan stream.TranscriptEvent, 32),
		segments:    make(map[int]string),
		revisions:   make(map[int]uint64),
		finalized:   make(map[int]bool),
	}

	initial, err := uploader.readServerMessage()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := uploader.handleMessage(initial); err != nil {
		_ = conn.Close()
		return nil, err
	}

	go uploader.readLoop()
	return uploader, nil
}

func (s *TencentASRSTT) buildRealtimeEndpoint(cfg STTStreamConfig) (string, error) {
	timestamp := time.Now().Unix()
	expired := timestamp + int64(tencentRealtimeASRURLTTL/time.Second)
	baseURL := strings.TrimRight(s.realtimeURL, "/")
	if baseURL == "" {
		baseURL = "wss://" + tencentRealtimeASRHost
	}

	// Generate 16-character voice_id from UUID
	voiceID := strings.ReplaceAll(uuid.New().String(), "-", "")[:16]

	params := map[string]string{
		"engine_model_type": s.engineModelType,
		"expired":           strconv.FormatInt(expired, 10),
		"nonce":             strconv.FormatInt(time.Now().UnixNano(), 10),
		"secretid":          s.secretID,
		"timestamp":         strconv.FormatInt(timestamp, 10),
		"voice_format":      tencentRealtimeASRVoiceFormatPCM,
		"voice_id":          voiceID,
	}
	if cfg.SampleRate == 8000 {
		params["input_sample_rate"] = "8000"
	}

	signedQuery := encodeTencentRealtimeQuery(params)
	resource := tencentRealtimeASRHost + tencentRealtimeASRPathPrefix + url.PathEscape(s.appID) + "?" + signedQuery
	signature := base64.StdEncoding.EncodeToString(hmacSHA1([]byte(s.secretKey), resource))
	params["signature"] = signature

	return baseURL + tencentRealtimeASRPathPrefix + url.PathEscape(s.appID) + "?" + encodeTencentRealtimeQuery(params), nil
}

func encodeTencentRealtimeQuery(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	values := url.Values{}
	for _, key := range keys {
		values.Set(key, params[key])
	}
	return values.Encode()
}

func packetBytesForAudioFormat(cfg STTStreamConfig) int {
	bytesPerSecond := cfg.SampleRate * cfg.Channels * (cfg.BitWidth / 8)
	if bytesPerSecond <= 0 {
		return 0
	}
	return bytesPerSecond * tencentRealtimeASRPacketMillis / 1000
}

type tencentASRStreamingUploader struct {
	conn        *websocket.Conn
	packetBytes int

	mu           sync.Mutex
	pending      []byte
	done         chan struct{}
	ended        bool
	finished     bool
	transcript   string
	err          error
	segments     map[int]string
	revisions    map[int]uint64
	finalized    map[int]bool
	maxIndex     int
	events       chan stream.TranscriptEvent
	eventsMu     sync.Mutex
	eventsClosed bool
}

// TranscriptEvents returns revisioned hypotheses, segment finals, and the
// turn commit emitted by the realtime provider. The channel is closed when
// the session finishes or is closed. Events are best-effort and bounded so a
// consumer that only uses Finalize cannot stall audio/network processing.
func (u *tencentASRStreamingUploader) TranscriptEvents() <-chan stream.TranscriptEvent {
	if u == nil {
		return nil
	}
	return u.events
}

func (u *tencentASRStreamingUploader) emitTranscript(event stream.TranscriptEvent) {
	if u == nil || u.events == nil {
		return
	}
	if err := event.Validate(); err != nil {
		return
	}
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	if u.eventsClosed {
		return
	}
	select {
	case u.events <- event:
	default:
		// Keep the latest provider state flowing. A bounded channel prevents
		// transcript observers from blocking the websocket reader.
		select {
		case <-u.events:
		default:
		}
		select {
		case u.events <- event:
		default:
		}
	}
}

func (u *tencentASRStreamingUploader) closeTranscriptEvents() {
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	if !u.eventsClosed && u.events != nil {
		u.eventsClosed = true
		close(u.events)
	}
}

func (u *tencentASRStreamingUploader) UploadPCM(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}

	u.mu.Lock()
	if u.ended {
		u.mu.Unlock()
		return fmt.Errorf("stream already finalized")
	}
	if u.packetBytes <= 0 {
		u.mu.Unlock()
		return fmt.Errorf("invalid packet size: audio format produces zero-byte packets (bit_width must be >= 8)")
	}
	u.pending = append(u.pending, pcm...)
	chunks := make([][]byte, 0)
	for len(u.pending) >= u.packetBytes {
		chunk := append([]byte(nil), u.pending[:u.packetBytes]...)
		u.pending = u.pending[u.packetBytes:]
		chunks = append(chunks, chunk)
	}
	u.mu.Unlock()

	for _, chunk := range chunks {
		if err := u.conn.WriteMessage(websocket.BinaryMessage, chunk); err != nil {
			u.finish("", fmt.Errorf("write audio chunk: %w", err))
			_, readErr := u.readResult()
			return readErr
		}
	}
	return nil
}

func (u *tencentASRStreamingUploader) Finalize() (string, error) {
	u.mu.Lock()
	if u.finished {
		transcript, err := u.transcript, u.err
		u.mu.Unlock()
		return transcript, err
	}
	if !u.ended {
		u.ended = true
	}
	pending := append([]byte(nil), u.pending...)
	u.pending = nil
	u.mu.Unlock()

	if len(pending) > 0 {
		if err := u.conn.WriteMessage(websocket.BinaryMessage, pending); err != nil {
			u.finish("", fmt.Errorf("write final audio chunk: %w", err))
			return u.readResult()
		}
	}
	if err := u.conn.WriteMessage(websocket.TextMessage, []byte(tencentRealtimeASREndMessage)); err != nil {
		u.finish("", fmt.Errorf("write end message: %w", err))
		return u.readResult()
	}
	if err := u.conn.SetReadDeadline(time.Now().Add(tencentRealtimeASRConnTimeout)); err != nil {
		u.finish("", fmt.Errorf("set realtime ASR read deadline: %w", err))
		return u.readResult()
	}

	<-u.done
	_ = u.conn.Close()
	return u.readResult()
}

func (u *tencentASRStreamingUploader) Close() error {
	if u == nil || u.conn == nil {
		return nil
	}
	_ = u.conn.Close()
	return nil
}

func (u *tencentASRStreamingUploader) readResult() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.transcript, u.err
}

func (u *tencentASRStreamingUploader) readLoop() {
	for {
		msg, err := u.readServerMessage()
		if err != nil {
			u.finish("", err)
			return
		}
		if err := u.handleMessage(msg); err != nil {
			u.finish("", err)
			return
		}
		if msg.Final != 0 {
			return
		}
	}
}

func (u *tencentASRStreamingUploader) readServerMessage() (tencentASRRealtimeServerMessage, error) {
	_, payload, err := u.conn.ReadMessage()
	if err != nil {
		return tencentASRRealtimeServerMessage{}, fmt.Errorf("read realtime ASR message: %w", err)
	}

	var msg tencentASRRealtimeServerMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return tencentASRRealtimeServerMessage{}, fmt.Errorf("decode realtime ASR message: %w", err)
	}
	return msg, nil
}

func (u *tencentASRStreamingUploader) handleMessage(msg tencentASRRealtimeServerMessage) error {
	if msg.Code != 0 {
		return fmt.Errorf("tencent realtime ASR error %d: %s", msg.Code, strings.TrimSpace(msg.Message))
	}

	if msg.Result != nil {
		index := msg.Result.Index
		if index < 0 {
			index = 0
		}
		text := msg.Result.VoiceTextStr
		u.mu.Lock()
		if u.finalized[index] {
			u.mu.Unlock()
		} else {
			u.revisions[index]++
			revision := u.revisions[index]
			u.segments[index] = text
			if index > u.maxIndex {
				u.maxIndex = index
			}
			segmentID := fmt.Sprintf("tencent-%d", index)
			segmentFinal := msg.Result.SliceType >= 2
			if segmentFinal {
				u.finalized[index] = true
			}
			u.mu.Unlock()

			if segmentFinal {
				u.emitTranscript(stream.NewTranscriptSegmentFinal(segmentID, revision, text))
			} else {
				u.emitTranscript(stream.NewTranscriptHypothesis(segmentID, revision, text))
			}
		}
	}

	if msg.Final != 0 {
		transcript := u.joinSegments()
		if strings.TrimSpace(transcript) != "" {
			u.mu.Lock()
			segments := make([]stream.TranscriptSegment, 0, len(u.segments))
			for i := 0; i <= u.maxIndex; i++ {
				if text, ok := u.segments[i]; ok && strings.TrimSpace(text) != "" {
					segments = append(segments, stream.TranscriptSegment{
						SegmentID: fmt.Sprintf("tencent-%d", i),
						Revision:  u.revisions[i],
						Text:      text,
					})
				}
			}
			u.mu.Unlock()
			if len(segments) > 0 {
				u.emitTranscript(stream.NewTurnCommit(segments, transcript))
			}
		}
		u.finish(transcript, nil)
	}
	return nil
}

func (u *tencentASRStreamingUploader) joinSegments() string {
	u.mu.Lock()
	defer u.mu.Unlock()

	parts := make([]string, 0, u.maxIndex+1)
	for i := 0; i <= u.maxIndex; i++ {
		if part := strings.TrimSpace(u.segments[i]); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func (u *tencentASRStreamingUploader) finish(transcript string, err error) {
	u.mu.Lock()
	if u.finished {
		u.mu.Unlock()
		return
	}
	if strings.TrimSpace(transcript) == "" {
		transcript = u.transcript
	}
	u.transcript = strings.TrimSpace(transcript)
	if err != nil {
		u.err = err
	} else if u.transcript == "" {
		u.err = fmt.Errorf("tencent realtime ASR returned empty transcript")
	}
	u.finished = true
	close(u.done)
	u.mu.Unlock()
	u.closeTranscriptEvents()
}

type tencentASRRequest struct {
	EngSerViceType string `json:"EngSerViceType"`
	SourceType     int    `json:"SourceType"`
	VoiceFormat    string `json:"VoiceFormat"`
	Data           string `json:"Data"`
	DataLen        int    `json:"DataLen"`
}

type tencentASRResponse struct {
	Response struct {
		Result string `json:"Result"`
		Error  *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error"`
	} `json:"Response"`
}

type tencentASRRealtimeServerMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	VoiceID string `json:"voice_id"`
	Final   int    `json:"final"`
	Result  *struct {
		SliceType    int    `json:"slice_type"`
		Index        int    `json:"index"`
		VoiceTextStr string `json:"voice_text_str"`
	} `json:"result"`
}

func buildTencentTC3Authorization(secretID, secretKey, payload string, timestamp int64) (string, error) {
	if secretID == "" || secretKey == "" {
		return "", fmt.Errorf("tencent ASR: missing credentials")
	}

	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")
	ts := fmt.Sprintf("%d", timestamp)

	canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:" + tencentASRHost + "\n"
	signedHeaders := "content-type;host"
	hashedPayload := sha256Hex(payload)
	canonicalRequest := strings.Join([]string{
		"POST",
		"/",
		"",
		canonicalHeaders,
		signedHeaders,
		hashedPayload,
	}, "\n")

	credentialScope := date + "/" + tencentASRService + "/tc3_request"
	stringToSign := strings.Join([]string{
		tencentASRAlgorithm,
		ts,
		credentialScope,
		sha256Hex(canonicalRequest),
	}, "\n")

	secretDate := hmacSHA256([]byte("TC3"+secretKey), date)
	secretService := hmacSHA256(secretDate, tencentASRService)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	return fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		tencentASRAlgorithm,
		secretID,
		credentialScope,
		signedHeaders,
		signature,
	), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(msg))
	return mac.Sum(nil)
}

func hmacSHA1(key []byte, msg string) []byte {
	mac := hmac.New(sha1.New, key)
	_, _ = mac.Write([]byte(msg))
	return mac.Sum(nil)
}

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"aico-agent/internal/providerretry"
)

const defaultOpenRouterSTTBaseURL = "https://openrouter.ai/api/v1"
const defaultOpenRouterSTTModel = "openai/whisper-large-v3"

// OpenRouterSTT implements STT using OpenRouter's JSON + base64 audio API.
type OpenRouterSTT struct {
	apiKey     string
	model      string
	baseURL    string
	language   string
	httpClient *http.Client
}

// NewOpenRouterSTT creates a new OpenRouter STT client.
func NewOpenRouterSTT(apiKey, model, baseURL, language string, httpClient *http.Client) *OpenRouterSTT {
	if baseURL == "" {
		baseURL = defaultOpenRouterSTTBaseURL
	}
	if model == "" {
		model = defaultOpenRouterSTTModel
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	} else if httpClient.Timeout == 0 {
		httpClient.Timeout = 30 * time.Second
	}
	return &OpenRouterSTT{
		apiKey:     apiKey,
		model:      model,
		baseURL:    baseURL,
		language:   language,
		httpClient: httpClient,
	}
}

// TranscribeWAV transcribes WAV audio data to text via OpenRouter.
func (s *OpenRouterSTT) TranscribeWAV(wavData []byte) (string, error) {
	return s.TranscribeWAVWithContext(context.Background(), wavData)
}

// TranscribeWAVWithContext transcribes WAV audio data with context for timeout/cancellation.
func (s *OpenRouterSTT) TranscribeWAVWithContext(ctx context.Context, wavData []byte) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return providerretry.Do(ctx, providerretry.VoicePolicy(), func(ctx context.Context) (string, error) { return s.transcribeWAVAttempt(ctx, wavData) })
}

func (s *OpenRouterSTT) transcribeWAVAttempt(ctx context.Context, wavData []byte) (string, error) {
	reqBody := openRouterSTTRequest{
		Model: s.model,
		InputAudio: openRouterInputAudio{
			Data:   base64.StdEncoding.EncodeToString(wavData),
			Format: "wav",
		},
	}
	if s.language != "" {
		reqBody.Language = s.language
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/audio/transcriptions", s.baseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", providerretry.ResponseError(resp, body)
	}

	var result openRouterSTTResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Text, nil
}

func (s *OpenRouterSTT) Capabilities() STTCapabilities {
	return STTCapabilities{}
}

func (s *OpenRouterSTT) NewStreamingUploader(_ context.Context, _ STTStreamConfig) (STTStreamUploader, error) {
	return nil, fmt.Errorf("provider %q does not support streaming upload", "openrouter")
}

type openRouterSTTRequest struct {
	Model      string               `json:"model"`
	InputAudio openRouterInputAudio `json:"input_audio"`
	Language   string               `json:"language,omitempty"`
}

type openRouterInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type openRouterSTTResponse struct {
	Text string `json:"text"`
}

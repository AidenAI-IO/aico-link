package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	defaultGoogleCloudSTTEndpoint = "https://speech.googleapis.com/v1/speech:recognize"
)

// GoogleCloudSTT implements STT using Google Cloud Speech-to-Text REST API with API Key authentication.
type GoogleCloudSTT struct {
	endpoint   string
	language   string
	model      string
	apiKey     string
	httpClient *http.Client
}

// NewGoogleCloudSTT creates a new Google Cloud STT client using API Key authentication.
func NewGoogleCloudSTT(apiKey, endpoint, language, model string, httpClient *http.Client) (*GoogleCloudSTT, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("google cloud STT: api_key is required")
	}
	if endpoint == "" {
		endpoint = defaultGoogleCloudSTTEndpoint
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &GoogleCloudSTT{
		endpoint:   endpoint,
		language:   language,
		model:      model,
		apiKey:     apiKey,
		httpClient: httpClient,
	}, nil
}

// Capabilities reports that this provider does not support streaming upload.
func (s *GoogleCloudSTT) Capabilities() STTCapabilities {
	return STTCapabilities{SupportsStreamingUpload: false}
}

// TranscribeWAV transcribes WAV audio data to text via Google Cloud Speech-to-Text API.
func (s *GoogleCloudSTT) TranscribeWAV(wavData []byte) (string, error) {
	return s.TranscribeWAVWithContext(context.Background(), wavData)
}

// TranscribeWAVWithContext transcribes WAV audio while honoring cancellation
// and the caller's deadline. The provider's 30-second limit remains an upper
// bound when the caller supplies a longer-lived context.
func (s *GoogleCloudSTT) TranscribeWAVWithContext(ctx context.Context, wavData []byte) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Extract PCM from WAV
	pcm, sampleRate, err := extractPCMFromWAV(wavData)
	if err != nil {
		return "", fmt.Errorf("google cloud STT: %w", err)
	}

	// Build request payload
	reqBody := googleCloudSTTRequest{
		Config: googleCloudSTTConfig{
			Encoding:        "LINEAR16",
			SampleRateHertz: sampleRate,
			LanguageCode:    s.resolveLanguageCode(),
			Model:           s.model,
		},
		Audio: googleCloudSTTAudio{
			Content: base64.StdEncoding.EncodeToString(pcm),
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("google cloud STT: marshal request: %w", err)
	}

	// Create HTTP request with timeout
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", s.endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return "", fmt.Errorf("google cloud STT: create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", s.apiKey)

	// Send request
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("google cloud STT: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("google cloud STT: API error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var result googleCloudSTTResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("google cloud STT: decode response: %w", err)
	}

	// Extract and concatenate transcripts from all results
	if len(result.Results) == 0 {
		return "", fmt.Errorf("google cloud STT: empty transcript")
	}

	var transcript strings.Builder
	for i, res := range result.Results {
		if len(res.Alternatives) > 0 {
			if i > 0 {
				transcript.WriteString(" ")
			}
			transcript.WriteString(res.Alternatives[0].Transcript)
		}
	}

	finalTranscript := transcript.String()
	if finalTranscript == "" {
		return "", fmt.Errorf("google cloud STT: empty transcript")
	}

	return finalTranscript, nil
}

// NewStreamingUploader returns an error as this provider does not support streaming.
func (s *GoogleCloudSTT) NewStreamingUploader(_ context.Context, _ STTStreamConfig) (STTStreamUploader, error) {
	return nil, fmt.Errorf("provider %q does not support streaming upload", "google-cloud")
}

func (s *GoogleCloudSTT) resolveLanguageCode() string {
	if s.language == "" {
		return "en-US"
	}
	// Convert simple language codes to Google Cloud format
	switch s.language {
	case "zh":
		return "zh-CN"
	case "en":
		return "en-US"
	default:
		return s.language
	}
}

// --- Google Cloud Speech-to-Text protocol message types ---

type googleCloudSTTRequest struct {
	Config googleCloudSTTConfig `json:"config"`
	Audio  googleCloudSTTAudio  `json:"audio"`
}

type googleCloudSTTConfig struct {
	Encoding        string `json:"encoding"`
	SampleRateHertz int    `json:"sampleRateHertz"`
	LanguageCode    string `json:"languageCode"`
	Model           string `json:"model,omitempty"`
}

type googleCloudSTTAudio struct {
	Content string `json:"content"` // base64 encoded audio
}

type googleCloudSTTResponse struct {
	Results []googleCloudSTTResult `json:"results"`
}

type googleCloudSTTResult struct {
	Alternatives []googleCloudSTTAlternative `json:"alternatives"`
}

type googleCloudSTTAlternative struct {
	Transcript string  `json:"transcript"`
	Confidence float64 `json:"confidence"`
}

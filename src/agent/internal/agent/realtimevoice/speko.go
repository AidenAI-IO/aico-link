package realtimevoice

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aico-agent/internal/providerretry"

	"github.com/gorilla/websocket"
)

const DefaultSpekoBaseURL = "https://api.speko.dev"

const defaultSpekoLeaseRefreshMargin = 30 * time.Second

// SpekoProvider mints a provider-direct S2S entitlement. Speko is deliberately
// absent from the media path: after minting, the selected native adapter opens
// the provider connection with the short-lived delegated credential.
type SpekoProvider struct {
	HTTPClient         *http.Client
	BaseURL            string
	AgentID            string
	UpstreamProvider   string
	Dialer             *websocket.Dialer
	EventBuffer        int
	IdempotencyKey     string
	LeaseRefreshMargin time.Duration
}

type spekoSessionCreate struct {
	Mode    string         `json:"mode"`
	AgentID string         `json:"agentId,omitempty"`
	S2S     spekoS2SConfig `json:"s2s"`
}

type spekoS2SConfig struct {
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	Voice            string `json:"voice,omitempty"`
	SystemPrompt     string `json:"systemPrompt,omitempty"`
	InputSampleRate  int    `json:"inputSampleRate,omitempty"`
	OutputSampleRate int    `json:"outputSampleRate,omitempty"`
	Tools            []Tool `json:"tools,omitempty"`
}

// spekoMintResponse intentionally keeps Credential opaque. Speko has used
// different credential object shapes as provider authentication evolved; the
// token extractor below accepts the documented token/value/apiKey variants
// without exposing those wire details to the shared session interface.
type spekoMintResponse struct {
	Mode              string          `json:"mode"`
	Transport         string          `json:"transport"`
	SessionID         string          `json:"sessionId"`
	SessionIDSnake    string          `json:"session_id"`
	AttemptID         string          `json:"attemptId"`
	Model             string          `json:"model"`
	Provider          string          `json:"provider"`
	Adapter           string          `json:"adapter"`
	ProviderTransport string          `json:"providerTransport"`
	Endpoint          string          `json:"endpoint"`
	ProviderEndpoint  string          `json:"providerEndpoint"`
	Credential        json.RawMessage `json:"credential"`
	InputSampleRate   int             `json:"inputSampleRate"`
	OutputSampleRate  int             `json:"outputSampleRate"`
	ExpiresAt         string          `json:"expiresAt"`
	Reservation       struct {
		LeaseExpiresAt string `json:"leaseExpiresAt"`
		Billing        struct {
			RenewalURL string `json:"renewalUrl"`
		} `json:"billing"`
	} `json:"reservation"`
}

func (p SpekoProvider) Open(ctx context.Context, cfg SessionConfig) (Session, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("speko: APIKey is required")
	}
	upstream, err := normalizeSpekoProvider(p.UpstreamProvider)
	if err != nil {
		return nil, err
	}
	if upstream == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("speko: upstream provider and model are required; automatic routing is disabled because it may select an unsupported WebRTC route")
	}
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		base = DefaultSpekoBaseURL
	}
	hc := p.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	body, err := json.Marshal(spekoSessionCreate{
		Mode: "s2s", AgentID: p.AgentID,
		S2S: spekoS2SConfig{
			Provider: upstream, Model: cfg.Model, Voice: cfg.Voice,
			SystemPrompt:     cfg.Instructions,
			InputSampleRate:  requestedRate(cfg.InputSampleRate, cfg.InputAudioFormat),
			OutputSampleRate: requestedRate(cfg.OutputSampleRate, cfg.OutputAudioFormat),
			Tools:            cfg.Tools,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("speko session mint request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("speko session mint request: %w", err)
	}
	req.Header.Set("Authorization", bearer(cfg.APIKey))
	req.Header.Set("Content-Type", "application/json")
	idempotencyKey := strings.TrimSpace(p.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey, err = spekoIdempotencyKey()
		if err != nil {
			return nil, fmt.Errorf("speko session mint: generate idempotency key: %w", err)
		}
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := providerretry.HTTP(ctx, hc, req)
	if err != nil {
		return nil, fmt.Errorf("speko session mint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("speko session mint: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var minted spekoMintResponse
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		return nil, fmt.Errorf("speko session mint response: %w", err)
	}
	if err := validateSpekoMintResponseForBaseURL(minted, upstream, base); err != nil {
		return nil, err
	}
	token, err := spekoCredentialToken(minted.Credential)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = strings.TrimSpace(minted.Model)
	}
	selectedProvider, err := normalizeSpekoProvider(minted.Provider)
	if err != nil {
		return nil, fmt.Errorf("speko: minted provider: %w", err)
	}
	if upstream != "" && selectedProvider != upstream {
		return nil, fmt.Errorf("speko: minted provider %q does not match requested upstream %q", selectedProvider, upstream)
	}
	endpoint := strings.TrimSpace(minted.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(minted.ProviderEndpoint)
	}
	if endpoint == "" {
		return nil, errors.New("speko session mint response missing provider endpoint")
	}
	sessionID := minted.SessionID
	if sessionID == "" {
		sessionID = minted.SessionIDSnake
	}
	inputRate, outputRate := spekoRates(selectedProvider, minted.InputSampleRate, minted.OutputSampleRate, cfg.InputSampleRate, cfg.OutputSampleRate)
	if !supportedSpekoSampleRate(inputRate) {
		return nil, fmt.Errorf("speko session mint response has unsupported input sample rate %d", inputRate)
	}
	if !supportedSpekoSampleRate(outputRate) {
		return nil, fmt.Errorf("speko session mint response has unsupported output sample rate %d", outputRate)
	}
	cfg.APIKey = token
	cfg.SessionID = sessionID
	cfg.InputSampleRate = inputRate
	cfg.OutputSampleRate = outputRate
	cfg.Model = strings.TrimSpace(cfg.Model)
	leaseExpiry, err := spekoLeaseExpiry(minted)
	if err != nil {
		return nil, err
	}
	leaseMargin := p.LeaseRefreshMargin
	if leaseMargin <= 0 {
		leaseMargin = defaultSpekoLeaseRefreshMargin
	}
	switch selectedProvider {
	case "google":
		native, err := (GeminiProvider{Endpoint: endpoint, Dialer: p.Dialer, EventBuffer: p.EventBuffer, DelegatedCredential: true}).Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return newLeasedGeminiSession(native, leaseExpiry, leaseMargin), nil
	case "xai":
		native, err := (XAIProvider{Endpoint: endpoint, Dialer: p.Dialer, EventBuffer: p.EventBuffer, AuthSubprotocol: true, AuthSubprotocolPrefix: "xai-client-secret."}).Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return newLeasedXAISession(native, leaseExpiry, leaseMargin), nil
	default:
		return nil, fmt.Errorf("speko: unsupported upstream provider %q", upstream)
	}
}

func validateSpekoMintResponse(minted spekoMintResponse, upstream string) error {
	return validateSpekoMintResponseWithLoopback(minted, upstream, false)
}

func validateSpekoMintResponseForBaseURL(minted spekoMintResponse, upstream, baseURL string) error {
	return validateSpekoMintResponseWithLoopback(minted, upstream, spekoBaseURLIsLoopback(baseURL))
}

func validateSpekoMintResponseWithLoopback(minted spekoMintResponse, upstream string, allowLoopback bool) error {
	if minted.Mode != "s2s" {
		return fmt.Errorf("speko: session mode %q is not s2s", minted.Mode)
	}
	if minted.Transport != "provider_direct" {
		return fmt.Errorf("speko: session transport %q is not provider_direct", minted.Transport)
	}
	if strings.TrimSpace(minted.SessionID) == "" && strings.TrimSpace(minted.SessionIDSnake) == "" {
		return errors.New("speko session mint response missing sessionId")
	}
	responseProvider, err := normalizeSpekoProvider(minted.Provider)
	if err != nil {
		return fmt.Errorf("speko: minted provider: %w", err)
	}
	if responseProvider == "" {
		return errors.New("speko: session mint response missing provider")
	}
	if upstream != "" && responseProvider != upstream {
		return fmt.Errorf("speko: minted provider %q does not match requested upstream %q", responseProvider, upstream)
	}
	expectedAdapter := map[string]string{"google": "google.live.v1", "xai": "xai.realtime.v1"}[responseProvider]
	if strings.TrimSpace(minted.Adapter) != expectedAdapter {
		return fmt.Errorf("speko: adapter %q does not match %s", minted.Adapter, expectedAdapter)
	}
	transport := strings.ToLower(strings.TrimSpace(minted.ProviderTransport))
	if transport != "websocket" && transport != "webrtc" {
		return fmt.Errorf("speko: %s provider-direct transport %q is not recognized", responseProvider, transport)
	}
	if transport == "webrtc" {
		return fmt.Errorf("speko: %s is routed over WebRTC, which this adapter cannot speak; select a Speko upstream served over WebSocket", responseProvider)
	}
	endpoint := strings.TrimSpace(minted.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(minted.ProviderEndpoint)
	}
	if err := validateSpekoEndpoint(endpoint, responseProvider, allowLoopback); err != nil {
		return err
	}
	if _, err := spekoCredentialExpiry(minted.Credential); err != nil {
		return err
	}
	if strings.TrimSpace(minted.ExpiresAt) == "" {
		return errors.New("speko session mint response missing expiresAt")
	}
	if err := validateSpekoExpiry(minted.ExpiresAt, "expiresAt"); err != nil {
		return err
	}
	if strings.TrimSpace(minted.Reservation.LeaseExpiresAt) == "" {
		return errors.New("speko session mint response missing reservation.leaseExpiresAt")
	}
	if err := validateSpekoExpiry(minted.Reservation.LeaseExpiresAt, "reservation.leaseExpiresAt"); err != nil {
		return err
	}
	if minted.InputSampleRate <= 0 || !supportedSpekoSampleRate(minted.InputSampleRate) {
		return fmt.Errorf("speko session mint response has unsupported input sample rate %d", minted.InputSampleRate)
	}
	if minted.OutputSampleRate <= 0 || !supportedSpekoSampleRate(minted.OutputSampleRate) {
		return fmt.Errorf("speko session mint response has unsupported output sample rate %d", minted.OutputSampleRate)
	}
	if strings.TrimSpace(minted.Model) == "" {
		return errors.New("speko session mint response missing model")
	}
	return nil
}

func validateSpekoEndpoint(raw, provider string, allowLoopback bool) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("speko session mint response missing provider endpoint")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("speko: invalid provider endpoint %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	loopback := isSpekoLoopbackHost(host)
	if scheme != "wss" && !(allowLoopback && loopback && scheme == "ws") {
		return fmt.Errorf("speko: %s endpoint must use wss://", provider)
	}
	if port := u.Port(); port != "" && !(allowLoopback && loopback) && port != "443" {
		return fmt.Errorf("speko: %s endpoint must use the default TLS port", provider)
	}
	if loopback {
		if !allowLoopback {
			return fmt.Errorf("speko: loopback provider endpoint %q is only allowed with a loopback Speko base URL", u.Hostname())
		}
	} else {
		allowedHost := false
		for _, suffix := range map[string][]string{
			"google": {"googleapis.com"},
			"xai":    {"x.ai"},
		}[provider] {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				allowedHost = true
				break
			}
		}
		if !allowedHost {
			return fmt.Errorf("speko: provider endpoint host %q is not allowlisted for %s", u.Hostname(), provider)
		}
	}
	allowedPaths := map[string]map[string]bool{
		"google": {
			"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent":            true,
			"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContentConstrained": true,
		},
		"xai": {"/v1/realtime": true},
	}[provider]
	if !allowedPaths[u.EscapedPath()] {
		return fmt.Errorf("speko: provider endpoint path %q is not allowlisted for %s", u.EscapedPath(), provider)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return fmt.Errorf("speko: invalid provider endpoint query: %w", err)
	}
	for key := range query {
		if provider != "xai" || key != "model" {
			return fmt.Errorf("speko: provider endpoint query parameter %q is not allowlisted for %s", key, provider)
		}
	}
	return nil
}

func spekoBaseURLIsLoopback(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && isSpekoLoopbackHost(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")))
}

func isSpekoLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func spekoCredentialExpiry(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", errors.New("speko session mint response missing credential")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("speko session mint response credential: %w", err)
	}
	if object, ok := value.(map[string]any); ok {
		if kind, ok := object["kind"].(string); ok && strings.TrimSpace(kind) != "" && strings.ToLower(strings.TrimSpace(kind)) != "bearer" {
			return "", fmt.Errorf("speko session mint response credential kind %q is not bearer", kind)
		}
		for _, key := range []string{"expiresAt", "expires_at"} {
			if expires, ok := object[key].(string); ok && strings.TrimSpace(expires) != "" {
				if err := validateSpekoExpiry(expires, "credential expiresAt"); err != nil {
					return "", err
				}
				return expires, nil
			}
		}
	}
	return "", errors.New("speko session mint response credential missing expiresAt")
}

func validateSpekoExpiry(raw, field string) error {
	expires, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("speko session mint response %s: %w", field, err)
	}
	if !expires.After(time.Now()) {
		return fmt.Errorf("speko session mint response %s is expired", field)
	}
	return nil
}

func spekoLeaseExpiry(minted spekoMintResponse) (time.Time, error) {
	sessionExpiry, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(minted.ExpiresAt))
	if err != nil {
		return time.Time{}, fmt.Errorf("speko session mint response expiresAt: %w", err)
	}
	credentialRaw, err := spekoCredentialExpiry(minted.Credential)
	if err != nil {
		return time.Time{}, err
	}
	credentialExpiry, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(credentialRaw))
	if err != nil {
		return time.Time{}, fmt.Errorf("speko session mint response credential expiresAt: %w", err)
	}
	leaseExpiry, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(minted.Reservation.LeaseExpiresAt))
	if err != nil {
		return time.Time{}, fmt.Errorf("speko session mint response reservation.leaseExpiresAt: %w", err)
	}
	earliest := sessionExpiry
	for _, candidate := range []time.Time{credentialExpiry, leaseExpiry} {
		if candidate.Before(earliest) {
			earliest = candidate
		}
	}
	return earliest, nil
}

func normalizeSpekoProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "":
		return "", nil
	case "google", "gemini":
		return "google", nil
	case "xai":
		return "xai", nil
	default:
		return "", fmt.Errorf("speko: unsupported upstream provider %q (want google or xai)", provider)
	}
}

func spekoRates(provider string, input, output, requestedInput, requestedOutput int) (int, int) {
	if input <= 0 {
		input = requestedInput
	}
	if output <= 0 {
		output = requestedOutput
	}
	if input <= 0 {
		if provider == "xai" {
			input = 24000
		} else {
			input = 16000
		}
	}
	if output <= 0 {
		output = 24000
	}
	return input, output
}

func supportedSpekoSampleRate(rate int) bool { return rate == 16000 || rate == 24000 }

func spekoIdempotencyKey() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func requestedRate(rate int, format string) int {
	if rate == 16000 || rate == 24000 {
		return rate
	}
	if strings.Contains(strings.ToLower(format), "16") {
		return 16000
	}
	if strings.Contains(strings.ToLower(format), "24") {
		return 24000
	}
	return 0
}

func spekoCredentialToken(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", errors.New("speko session mint response missing credential")
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("speko session mint response credential: %w", err)
	}
	if token := findSpekoToken(value); token != "" {
		return token, nil
	}
	return "", errors.New("speko session mint response missing credential token")
}

func findSpekoToken(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"token", "accessToken", "access_token", "apiKey", "api_key", "key", "value"} {
		if token := findSpekoToken(object[key]); token != "" {
			return token
		}
	}
	for _, key := range []string{"credential", "providerCredential", "provider_credential", "auth"} {
		if token := findSpekoToken(object[key]); token != "" {
			return token
		}
	}
	return ""
}

func bearer(key string) string {
	key = strings.TrimSpace(key)
	if strings.HasPrefix(strings.ToLower(key), "bearer ") {
		return key
	}
	return "Bearer " + key
}

func buffer(n int) int {
	if n <= 0 {
		return 64
	}
	return n
}

func normalClose(err error) bool {
	return websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, websocket.ErrCloseSent)
}

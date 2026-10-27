package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRetryTransportRetriesEOFAndReplaysBody(t *testing.T) {
	attempts := 0
	var bodies []string
	transport := &retryTransport{
		maxRetries:     2,
		retryDelayBase: 0,
		wrapped: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("ReadAll body: %v", err)
			}
			bodies = append(bodies, string(body))
			if attempts == 1 {
				return nil, io.EOF
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    req,
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewBufferString(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(bodies) != 2 || bodies[0] != `{"model":"test"}` || bodies[1] != `{"model":"test"}` {
		t.Fatalf("request bodies were not replayed: %#v", bodies)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRetryTransportDoesNotRetryCanceledContext(t *testing.T) {
	attempts := 0
	transport := &retryTransport{
		maxRetries:     2,
		retryDelayBase: 0,
		wrapped: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			return nil, context.Canceled
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewBufferString(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected context canceled error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestLLMHTTPClientsDoNotUseClientTimeout(t *testing.T) {
	manager := NewModelManager(ModelConfig{}, ProxyConfig{})
	clients := map[string]*http.Client{
		"retry client":               manager.buildContext().HTTPClient,
		"ollama client":              manager.buildContext().OllamaHTTPClient,
		"OpenAI-compatible fallback": newOpenAICompatibleModel("https://example.test", "model", "", nil).(*openAICompatibleModel).httpClient,
		"Responses fallback":         newResponsesModel("https://example.test", "model", "", nil, responsesModelOptions{}).(*responsesModel).httpClient,
		"Anthropic fallback":         newAnthropicModel("https://example.test", "model", "", nil).(*anthropicModel).httpClient,
	}
	for name, client := range clients {
		if client.Timeout != 0 {
			t.Fatalf("%s timeout = %s, want no client-wide timeout", name, client.Timeout)
		}
	}
}

type requestContextRecordingModel struct {
	ctx context.Context
}

func (m *requestContextRecordingModel) Call(ctx context.Context, _ string, _ ...llms.CallOption) (string, error) {
	m.ctx = ctx
	return "ok", nil
}

func (m *requestContextRecordingModel) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.ctx = ctx
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "ok"}}}, nil
}

func TestModelManagerAppliesTimeoutOnlyToNonStreamingRequests(t *testing.T) {
	model := &requestContextRecordingModel{}
	manager := NewModelManager(ModelConfig{}, ProxyConfig{})
	manager.model = model

	if _, err := manager.GenerateContent(context.Background(), nil); err != nil {
		t.Fatalf("non-streaming GenerateContent: %v", err)
	}
	deadline, ok := model.ctx.Deadline()
	if !ok {
		t.Fatal("non-streaming request has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 29*time.Second || remaining > 30*time.Second {
		t.Fatalf("deadline remaining = %s, want approximately 30s", remaining)
	}

	if _, err := manager.GenerateContent(context.Background(), nil, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil })); err != nil {
		t.Fatalf("streaming GenerateContent: %v", err)
	}
	if _, ok := model.ctx.Deadline(); ok {
		t.Fatal("streaming request received a default deadline")
	}
}

func TestRetryTransportRetriesTooManyRequests(t *testing.T) {
	attempts := 0
	transport := &retryTransport{
		maxRetries:     2,
		retryDelayBase: 0,
		wrapped: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Body:       io.NopCloser(strings.NewReader(`rate limited`)),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    req,
			}, nil
		}),
	}

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewBufferString(`{"model":"test"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned error: %v", err)
	}
	defer resp.Body.Close()

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestBuildKimiProvidersResolveBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		baseURL     string
		wantBaseURL string
	}{
		{"kimi default global", "kimi", "", moonshotGlobalBaseURL},
		{"kimi-cn default cn", "kimi-cn", "", moonshotCNBaseURL},
		{"kimi case insensitive", "Kimi", "", moonshotGlobalBaseURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{
				Provider: tt.provider,
				Model:    "kimi-k3",
				APIKey:   "test-key",
				BaseURL:  tt.baseURL,
			}, ProxyConfig{})

			model, err := mgr.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			compatible, ok := model.(*openAICompatibleModel)
			if !ok {
				t.Fatalf("model type = %T, want *openAICompatibleModel", model)
			}
			if compatible.baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", compatible.baseURL, tt.wantBaseURL)
			}
		})
	}
}

func TestBuildKimiProvidersRejectResponsesMode(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-cn"} {
		t.Run(provider, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{
				Provider: provider,
				Model:    "kimi-k3",
				APIKey:   "test-key",
				APIMode:  "responses",
			}, ProxyConfig{})

			_, err := mgr.build()
			if err == nil || !strings.Contains(err.Error(), "Chat Completions, not /responses") {
				t.Fatalf("build error = %v, want Kimi Responses compatibility error", err)
			}
		})
	}
}

func TestBuildVolcengineProviderResolvesBaseURL(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		baseURL     string
		wantBaseURL string
	}{
		{"volcengine default ark", "volcengine", "", arkBeijingBaseURL},
		{"volcengine case insensitive", "Volcengine", "", arkBeijingBaseURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{
				Provider: tt.provider,
				Model:    "doubao-seed-2-1-pro-260628",
				APIKey:   "test-key",
				BaseURL:  tt.baseURL,
			}, ProxyConfig{})

			model, err := mgr.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			compatible, ok := model.(*openAICompatibleModel)
			if !ok {
				t.Fatalf("model type = %T, want *openAICompatibleModel", model)
			}
			if compatible.baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", compatible.baseURL, tt.wantBaseURL)
			}
			// The OpenRouter-only nested reasoning object must stay off for Ark:
			// the Ark endpoint only accepts the standard reasoning_effort field.
			if compatible.dialect != compatibleDialectOpenAI {
				t.Errorf("dialect = %q, want the generic OpenAI shape for the volcengine provider", compatible.dialect)
			}
		})
	}
}

func TestBuildDeepSeekProviderResolvesBaseURL(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider: "deepseek",
		Model:    "deepseek-flash",
		APIKey:   "test-key",
	}, ProxyConfig{})

	model, err := mgr.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	compatible, ok := model.(*openAICompatibleModel)
	if !ok {
		t.Fatalf("model type = %T, want *openAICompatibleModel", model)
	}
	if compatible.baseURL != deepseekBaseURL {
		t.Errorf("baseURL = %q, want %q", compatible.baseURL, deepseekBaseURL)
	}
}

func TestBuildOpenRouterEnablesNestedReasoning(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider: "openrouter",
		Model:    "google/gemini-3.5-flash",
		APIKey:   "test-key",
	}, ProxyConfig{})

	model, err := mgr.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	compatible, ok := model.(*openAICompatibleModel)
	if !ok {
		t.Fatalf("model type = %T, want *openAICompatibleModel", model)
	}
	if compatible.dialect != compatibleDialectOpenRouter {
		t.Errorf("dialect = %q, want openrouter", compatible.dialect)
	}
}

func TestBuildOpenAICompatibleResponsesMode(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider: "openai",
		Model:    "gpt-5",
		APIKey:   "test-key",
		APIMode:  "responses",
	}, ProxyConfig{})

	model, err := mgr.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, ok := model.(*responsesModel); !ok {
		t.Fatalf("model type = %T, want *responsesModel", model)
	}
}

func TestBuildGeminiInteractionsModes(t *testing.T) {
	for _, tt := range []struct {
		name        string
		apiMode     string
		providerCtx bool
	}{
		{name: "local", apiMode: "interactions"},
		{name: "stateful", apiMode: "interactions_stateful", providerCtx: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash", APIKey: "test-key", APIMode: tt.apiMode}, ProxyConfig{})
			model, err := mgr.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			interactions, ok := model.(*interactionsModel)
			if !ok || interactions.providerManagedContext != tt.providerCtx {
				t.Fatalf("model = %#v, want interactions provider_context=%v", model, tt.providerCtx)
			}
			if interactions.baseURL != "https://generativelanguage.googleapis.com/v1beta" {
				t.Fatalf("base URL = %q", interactions.baseURL)
			}
		})
	}
}

// An unset api_mode must resolve to stateless Interactions. The
// OpenAI-compatible endpoint cannot replay thought signatures, so falling back
// to it would break multi-turn tool calls with HTTP 400.
func TestBuildGeminiDefaultsToStatelessInteractions(t *testing.T) {
	mgr := NewModelManager(ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash", APIKey: "test-key"}, ProxyConfig{})
	model, err := mgr.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	interactions, ok := model.(*interactionsModel)
	if !ok {
		t.Fatalf("model type = %T, want *interactionsModel", model)
	}
	if interactions.baseURL != "https://generativelanguage.googleapis.com/v1beta" {
		t.Fatalf("base URL = %q", interactions.baseURL)
	}
	if interactions.providerManagedContext {
		t.Fatal("unset api_mode must not enable provider-managed context")
	}
	if interactions.token != "test-key" {
		t.Fatal("token was not passed to the interactions transport")
	}
}

func TestBuildGeminiRejectsCompatibleAndResponsesModes(t *testing.T) {
	for _, apiMode := range []string{"chat_completions", "responses", "responses_stateful", "bogus"} {
		mgr := NewModelManager(ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash", APIKey: "test-key", APIMode: apiMode}, ProxyConfig{})
		if _, err := mgr.build(); err == nil {
			t.Fatalf("api_mode=%s built a model, want an error", apiMode)
		}
	}
}

// Gemini exposes exactly one endpoint and one wire shape here, including when
// api_mode is unset, so the config UI never advertises a compatible endpoint.
func TestGeminiModelAPIEndpointAndShapeAreAlwaysInteractions(t *testing.T) {
	for _, apiMode := range []string{"", "interactions", "interactions_stateful"} {
		if got := modelAPIEndpoint("gemini", ""); got != "https://generativelanguage.googleapis.com/v1beta" {
			t.Fatalf("endpoint = %q", got)
		}
		if got := modelAPIShape("gemini", apiMode); got != "interactions" {
			t.Fatalf("api_mode=%q shape = %q, want interactions", apiMode, got)
		}
	}
}

func TestBuildOpenAICompatibleResponsesStatefulMode(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider: "openai",
		Model:    "gpt-5",
		APIKey:   "test-key",
		APIMode:  "responses_stateful",
	}, ProxyConfig{})

	model, err := mgr.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	responses, ok := model.(*responsesModel)
	if !ok || !responses.providerManagedContext {
		t.Fatalf("model = %#v, want provider-managed Responses model", model)
	}
}

func TestBuildOpenRouterResponsesModes(t *testing.T) {
	t.Run("stateless", func(t *testing.T) {
		mgr := NewModelManager(ModelConfig{
			Provider: "openrouter",
			Model:    "openai/gpt-5.5",
			APIKey:   "test-key",
			APIMode:  "responses",
		}, ProxyConfig{})

		model, err := mgr.build()
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		responses, ok := model.(*responsesModel)
		if !ok || responses.providerManagedContext || responses.dialect != responsesDialectOpenRouter {
			t.Fatalf("model = %#v, want stateless OpenRouter Responses model", model)
		}
	})

	t.Run("stateful", func(t *testing.T) {
		mgr := NewModelManager(ModelConfig{
			Provider: "openrouter",
			Model:    "openai/gpt-5.5",
			APIKey:   "test-key",
			APIMode:  "responses_stateful",
		}, ProxyConfig{})

		_, err := mgr.build()
		if err == nil || !strings.Contains(err.Error(), "stateless") {
			t.Fatalf("build error = %v, want stateless-only error", err)
		}
	})
}

func TestBuildResponsesModeUsesCustomOpenAICompatibleEndpoint(t *testing.T) {
	for _, provider := range []string{"openai", "volcengine"} {
		t.Run(provider, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{
				Provider: provider,
				Model:    "custom-model",
				APIKey:   "test-key",
				BaseURL:  "https://gateway.example.test/v1",
				APIMode:  "responses",
			}, ProxyConfig{})

			model, err := mgr.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			responses, ok := model.(*responsesModel)
			if !ok {
				t.Fatalf("model type = %T, want *responsesModel", model)
			}
			if responses.baseURL != "https://gateway.example.test/v1" {
				t.Fatalf("base URL = %q", responses.baseURL)
			}
		})
	}
}

func TestBuildOpenAIResponsesRecognizesKnownCompatibleHosts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		baseURL string
		want    responsesDialect
	}{
		{name: "OpenRouter", baseURL: "https://openrouter.ai/api/v1", want: responsesDialectOpenRouter},
		{name: "Volcengine Ark", baseURL: "https://ark.cn-beijing.volces.com/api/v3", want: responsesDialectVolcengine},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{
				Provider: "openai",
				Model:    "test-model",
				APIKey:   "test-key",
				BaseURL:  tt.baseURL,
				APIMode:  "responses",
			}, ProxyConfig{})

			model, err := mgr.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			responses, ok := model.(*responsesModel)
			if !ok || responses.dialect != tt.want {
				t.Fatalf("model = %#v, want dialect %q", model, tt.want)
			}
		})
	}
}

func TestBuildOpenAIStatefulResponsesRejectsOpenRouterHost(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider: "openai",
		Model:    "openai/gpt-5.5",
		APIKey:   "test-key",
		BaseURL:  "https://openrouter.ai/api/v1",
		APIMode:  "responses_stateful",
	}, ProxyConfig{})

	_, err := mgr.build()
	if err == nil || !strings.Contains(err.Error(), "stateless") {
		t.Fatalf("build error = %v, want stateless-only error", err)
	}
}

func TestNativeTransportsExplainResponsesModeRequirement(t *testing.T) {
	for _, provider := range []string{"anthropic", "ollama"} {
		t.Run(provider, func(t *testing.T) {
			mgr := NewModelManager(ModelConfig{Provider: provider, Model: "test", APIKey: "test-key", APIMode: "responses"}, ProxyConfig{})
			_, err := mgr.build()
			if err == nil || !strings.Contains(err.Error(), "OpenAI-compatible /responses endpoint") {
				t.Fatalf("build error = %v", err)
			}
		})
	}
}

func TestBuildOpenRouterMissingAPIKeyError(t *testing.T) {
	t.Run("named environment reference", func(t *testing.T) {
		const tokenEnv = "AICO_TEST_MISSING_OPENROUTER_KEY"
		t.Setenv(tokenEnv, "")
		mgr := NewModelManager(ModelConfig{
			Provider: "openrouter",
			Model:    "google/gemini-3.5-flash",
			APIKey:   "$" + tokenEnv,
		}, ProxyConfig{})

		_, err := mgr.build()
		if err == nil {
			t.Fatal("build succeeded without an API key")
		}
		want := "missing the OpenRouter API key, set it in the " + tokenEnv + " environment variable"
		if err.Error() != want {
			t.Fatalf("build error = %q, want %q", err, want)
		}
	})

	t.Run("provider record without api key", func(t *testing.T) {
		mgr := NewModelManager(ModelConfig{
			Provider: "openrouter",
			Model:    "google/gemini-3.5-flash",
		}, ProxyConfig{})

		_, err := mgr.build()
		if err == nil {
			t.Fatal("build succeeded without an API key")
		}
		const want = "missing the OpenRouter API key, set api_key on the provider record"
		if err.Error() != want {
			t.Fatalf("build error = %q, want %q", err, want)
		}
	})
}

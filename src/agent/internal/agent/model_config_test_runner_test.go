package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunModelProviderTestUsesRuntimeProviderResolution(t *testing.T) {
	t.Setenv("AICO_MODEL_CONFIG_TEST_KEY", "model-config-test-secret")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer model-config-test-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "hello"},
			}},
		})
	}))
	defer server.Close()

	cfg := Config{
		ModelProviders: map[string]ModelProvider{
			"work": {
				Type:    "openai",
				APIKey:  "$AICO_MODEL_CONFIG_TEST_KEY",
				BaseURL: server.URL + "/v1",
			},
		},
	}
	result, err := RunModelProviderTest(context.Background(), cfg, ModelProviderTestRequest{
		Provider: "work",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatalf("RunModelProviderTest() error = %v", err)
	}
	if result.Provider != "openai" || result.Model != "test-model" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunModelProviderTestReportsInvalidProviderRecord(t *testing.T) {
	_, err := RunModelProviderTest(context.Background(), Config{
		ModelProviders: map[string]ModelProvider{"broken": {}},
	}, ModelProviderTestRequest{Provider: "broken", Model: "test-model"})
	if err == nil || !strings.Contains(err.Error(), "has no provider type specified") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunModelProviderTestRejectsInvalidAPIMode(t *testing.T) {
	_, err := RunModelProviderTest(context.Background(), Config{}, ModelProviderTestRequest{
		Provider: "openai",
		Model:    "test-model",
		APIMode:  "invalid",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid model.api_mode") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunModelProviderTestSendsEffectiveSamplingValues(t *testing.T) {
	tests := []struct {
		name                string
		request             ModelProviderTestRequest
		wantTemperature     float64
		wantReasoningEffort string
	}{
		{
			name: "unset values use model defaults",
			request: ModelProviderTestRequest{
				Provider: "openai",
				Model:    "kimi-k3",
			},
			wantTemperature:     1,
			wantReasoningEffort: "low",
		},
		{
			name: "explicit zero and reasoning value reach provider",
			request: ModelProviderTestRequest{
				Provider:        "openai",
				Model:           "kimi-k3",
				Temperature:     floatPtr(0),
				ReasoningEffort: "none",
			},
			wantTemperature:     0,
			wantReasoningEffort: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured struct {
				Temperature     *float64 `json:"temperature"`
				ReasoningEffort string   `json:"reasoning_effort"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
			}))
			defer server.Close()

			cfg := Config{
				ModelProviders: map[string]ModelProvider{
					"test-openai": {Type: "openai", APIKey: "test-key", BaseURL: server.URL + "/v1"},
				},
			}
			tt.request.Provider = "test-openai"
			if _, err := RunModelProviderTest(context.Background(), cfg, tt.request); err != nil {
				t.Fatalf("RunModelProviderTest() error = %v", err)
			}
			if captured.Temperature == nil || *captured.Temperature != tt.wantTemperature {
				t.Fatalf("request temperature = %v, want %v", captured.Temperature, tt.wantTemperature)
			}
			if captured.ReasoningEffort != tt.wantReasoningEffort {
				t.Fatalf("request reasoning_effort = %q, want %q", captured.ReasoningEffort, tt.wantReasoningEffort)
			}
		})
	}
}

func TestApplyModelProviderTestRequestSamplingSemantics(t *testing.T) {
	tests := []struct {
		name                string
		request             ModelProviderTestRequest
		wantTemperature     float64
		wantReasoningEffort string
	}{
		{
			name:                "unset clears stored values and uses Kimi defaults",
			request:             ModelProviderTestRequest{Provider: "openai", Model: "kimi-k3"},
			wantTemperature:     1,
			wantReasoningEffort: "low",
		},
		{
			name: "explicit zero remains set",
			request: ModelProviderTestRequest{
				Provider:        "openai",
				Model:           "kimi-k3",
				Temperature:     floatPtr(0),
				ReasoningEffort: "none",
			},
			wantTemperature:     0,
			wantReasoningEffort: "none",
		},
		{
			name: "explicit nonzero values remain set",
			request: ModelProviderTestRequest{
				Provider:        "openai",
				Model:           "kimi-k3",
				Temperature:     floatPtr(0.7),
				ReasoningEffort: "medium",
			},
			wantTemperature:     0.7,
			wantReasoningEffort: "medium",
		},
		{
			name: "blank reasoning is unset",
			request: ModelProviderTestRequest{
				Provider:        "openai",
				Model:           "kimi-k3",
				ReasoningEffort: "  \t",
			},
			wantTemperature:     1,
			wantReasoningEffort: "low",
		},
		{
			name:                "unknown model uses global temperature and auto reasoning",
			request:             ModelProviderTestRequest{Provider: "openai", Model: "custom-model"},
			wantTemperature:     defaultModelTemperature,
			wantReasoningEffort: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storedTemperature := 0.2
			cfg := Config{Model: ModelConfig{
				Provider:        "openai",
				Model:           "gpt-4o",
				Temperature:     &storedTemperature,
				ReasoningEffort: "high",
			}}

			if err := applyModelProviderTestRequest(&cfg, tt.request); err != nil {
				t.Fatalf("applyModelProviderTestRequest() error = %v", err)
			}
			if cfg.Model.Temperature == nil || *cfg.Model.Temperature != tt.wantTemperature {
				t.Fatalf("temperature = %v, want %v", cfg.Model.Temperature, tt.wantTemperature)
			}
			if cfg.Model.ReasoningEffort != tt.wantReasoningEffort {
				t.Fatalf("reasoning_effort = %q, want %q", cfg.Model.ReasoningEffort, tt.wantReasoningEffort)
			}
		})
	}
}

func TestApplyModelProviderTestRequestBaseURLFollowsProvider(t *testing.T) {
	cfg := Config{
		ModelProviders: map[string]ModelProvider{
			"work": {Type: "openai", BaseURL: "https://provider.example.com/v1"},
		},
		Model: ModelConfig{
			Provider: "openai",
			Model:    "gpt-4o",
			BaseURL:  "https://stale.example.com/v1",
		},
	}

	if err := applyModelProviderTestRequest(&cfg, ModelProviderTestRequest{
		Provider: "work",
		Model:    "gpt-4o",
	}); err != nil {
		t.Fatalf("applyModelProviderTestRequest() error = %v", err)
	}
	if cfg.Model.BaseURL != "https://provider.example.com/v1" {
		t.Fatalf("base_url = %q, want provider value", cfg.Model.BaseURL)
	}

	if err := applyModelProviderTestRequest(&cfg, ModelProviderTestRequest{
		Provider: "openai",
		Model:    "gpt-4o",
	}); err != nil {
		t.Fatalf("applyModelProviderTestRequest() direct error = %v", err)
	}
	if cfg.Model.BaseURL != "" {
		t.Fatalf("direct provider base_url = %q, want empty", cfg.Model.BaseURL)
	}
}

func TestApplyModelProviderTestRequestCopiesExplicitTemperature(t *testing.T) {
	requestedTemperature := 0.7
	req := ModelProviderTestRequest{
		Provider:    "openai",
		Model:       "kimi-k3",
		Temperature: &requestedTemperature,
	}
	cfg := Config{}

	if err := applyModelProviderTestRequest(&cfg, req); err != nil {
		t.Fatalf("applyModelProviderTestRequest() error = %v", err)
	}
	requestedTemperature = 0.1
	if cfg.Model.Temperature == req.Temperature {
		t.Fatal("temperature pointer aliases request storage")
	}
	if cfg.Model.Temperature == nil || *cfg.Model.Temperature != 0.7 {
		t.Fatalf("temperature = %v, want copied value 0.7", cfg.Model.Temperature)
	}
}

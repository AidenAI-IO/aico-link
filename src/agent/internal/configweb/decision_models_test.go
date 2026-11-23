package configweb

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/decision"
)

type decisionCatalogTransport func(*http.Request) (*http.Response, error)

func (f decisionCatalogTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestDecisionModelsCatalogFiltersNativeEndpoints(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	server.decisionModelsClient = &http.Client{Transport: decisionCatalogTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "openrouter.ai" || req.URL.Path != "/api/v1/models" || req.URL.Query().Get("output_modalities") != "decisions" || req.URL.Query().Get("order") != "top-weekly" {
			t.Errorf("catalog URL = %s", req.URL)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("public catalog request included a saved API key")
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[
			{"id":"vendor/new-decision","name":"New Decision","architecture":{"output_modalities":["decisions"]},"endpoints":["/api/alpha/decisions"]},
			{"id":"vendor/extractor","architecture":{"output_modalities":["decisions"]},"endpoints":["/api/alpha/extractions"]},
			{"id":"vendor/chat","architecture":{"output_modalities":["text"]},"endpoints":["/api/v1/chat/completions"]},
			{"id":"vendor/second-decision","architecture":{"output_modalities":["decisions"]},"endpoints":["/api/alpha/decisions","/api/alpha/extractions"]}
		]}`))}, nil
	})}
	get := func(provider string) *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/models?provider="+provider+"&output_modalities=decisions", nil))
		return resp
	}
	resp := get("openrouter")
	var payload struct {
		Models []decision.ModelInfo `json:"models"`
	}
	if resp.Code != http.StatusOK || json.Unmarshal(resp.Body.Bytes(), &payload) != nil {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if len(payload.Models) != 2 || payload.Models[0].ID != "vendor/new-decision" || payload.Models[1].ID != "vendor/second-decision" {
		t.Fatalf("filtered catalog changed order or included non-decision models: %+v", payload.Models)
	}
	status = http.StatusServiceUnavailable
	if resp = get("openrouter"); resp.Code != http.StatusBadGateway {
		t.Fatalf("upstream error was hidden: status=%d body=%s", resp.Code, resp.Body.String())
	}
	if resp = get("openai"); resp.Code != http.StatusBadRequest {
		t.Fatalf("unsupported catalog provider accepted: status=%d", resp.Code)
	}
}

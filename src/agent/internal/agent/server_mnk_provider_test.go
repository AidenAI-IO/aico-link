package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agent/mnk"
)

func TestHandleProviderMNKUnavailableWithoutProvider(t *testing.T) {
	server := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/providers/mnk", strings.NewReader(`{"operation":"click"}`))
	rec := httptest.NewRecorder()
	server.handleProviderMNK(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	var payload mnk.MNKErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if payload.Error != "mnk provider not configured" {
		t.Fatalf("error = %q", payload.Error)
	}
}

func TestHandleProviderMNKRoutesThroughHandler(t *testing.T) {
	mock := mnk.NewMockProvider()
	server := &Server{
		runtime: &Runtime{
			tools: &ToolSet{mnkProvider: mock},
		},
	}

	body := `{"operation":"keypress","keypress":{"keys":["enter"]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/providers/mnk", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.handleProviderMNK(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := mock.KeypressCalls(); len(got) != 1 || len(got[0].Keys) != 1 || got[0].Keys[0] != "enter" {
		t.Fatalf("unexpected keypresses: %#v", got)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/providers/mnk", server.handleProviderMNK)
	req = httptest.NewRequest(http.MethodGet, "/api/providers/mnk", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

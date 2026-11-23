package configweb

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPClientConfigWebProxy(t *testing.T) {
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			suffix := ""
			if method == "PATCH" || method == "DELETE" {
				suffix = "/demo"
			}
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.URL.Path != "/api/internal/mcp-servers"+suffix || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected forwarded request: %s %s", r.Method, r.URL)
				}
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"enabled":false}` {
					t.Errorf("body altered: %s", data)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"servers":[]}`))
			}))
			defer h.Close()
			for _, local := range []bool{false, true} {
				s := &Server{options: Options{AgentHTTPBaseURL: h.URL, LocalAgent: local}}
				r := httptest.NewRequest(method, "http://127.0.0.1/api/mcp/servers"+suffix, strings.NewReader(`{"enabled":false}`))
				w := httptest.NewRecorder()
				s.serveAPI(w, r)
				if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("proxy: %d %s", w.Code, w.Body)
				}
			}
		})
	}
	s := &Server{options: Options{AgentHTTPBaseURL: "http://192.0.2.1:8080"}}
	w := httptest.NewRecorder()
	s.handleMCPServers(w, httptest.NewRequest("GET", "http://localhost/api/mcp/servers", nil), "")
	if w.Code != 503 {
		t.Fatal("management proxy allowed non-loopback destination")
	}
}

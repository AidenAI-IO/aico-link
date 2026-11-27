package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDebugUTGSnapshot(t *testing.T) {
	rt := &Runtime{config: Config{ConfigDir: t.TempDir()}}
	handler := (&Server{runtime: rt}).Handler()
	get := func() (*httptest.ResponseRecorder, debugUTGSnapshot) {
		t.Helper()
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/debug/utg", nil))
		var view debugUTGSnapshot
		if r.Code == http.StatusOK {
			if err := json.Unmarshal(r.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
		}
		return r, view
	}
	r, view := get()
	if r.Code != http.StatusOK || !view.Enabled || !view.Available || view.Nodes == nil || view.Edges == nil || view.Tips == nil || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected empty snapshot: %d %s", r.Code, r.Body.String())
	}
	graph := skillUTG{
		Nodes: map[string]*utgNode{"phone_home": {Label: "phone_home", Samples: []utgFingerprint{{Hash: 99}}}},
		Edges: []utgEdge{{From: "phone_home", To: "phone_settings", Intent: "nav", Successes: 2}},
	}
	if err := saveSkillUTG(skillUTGPath(rt.config, view.Platform), graph); err != nil {
		t.Fatal(err)
	}
	r, view = get()
	if r.Code != http.StatusOK || len(view.Nodes) != 1 || view.Nodes[0].Samples != 1 || len(view.Edges) != 1 || view.Edges[0].Successes != 2 || strings.Contains(r.Body.String(), `"hash"`) || strings.Contains(r.Body.String(), `"gray"`) {
		t.Fatalf("expected metadata without visual fingerprints: %d %s", r.Code, r.Body.String())
	}
	rt.config.EnvironmentBridge.Endpoint = "http://bridge.invalid"
	r, view = get()
	if r.Code != http.StatusOK || !view.Available || len(view.Nodes) != 1 || view.Nodes[0].Label != "phone_home" || len(view.Edges) != 1 {
		t.Fatalf("bridge should expose the saved graph for the configured profile and platform: %d %s", r.Code, r.Body.String())
	}
	post := httptest.NewRecorder()
	handler.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "/api/debug/utg", nil))
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET" {
		t.Fatalf("UTG API must be read-only: %d", post.Code)
	}
}

func TestDebugRoutes(t *testing.T) {
	handler := (&Server{}).Handler()
	for _, path := range []string{"/debug", "/coordinate-debug", "/coordinate-debug.html"} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/debug" {
			if r.Code != http.StatusOK || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("debug response: %d %v", r.Code, r.Header())
			}
		} else if r.Code != http.StatusNotFound || r.Header().Get("Location") != "" {
			t.Fatalf("removed page %s: %d %v", path, r.Code, r.Header())
		}
	}
}

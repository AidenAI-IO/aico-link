package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestIPhoneMirroringRestoresConfiguredRoute(t *testing.T) {
	for _, scenario := range []string{"restored", "other_bridge", "permission", "window", "busy", "wrong_task", "bad_frame", "stale_frame", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			var calls []string
			var jpegBytes bytes.Buffer
			if err := jpeg.Encode(&jpegBytes, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil); err != nil {
				t.Fatal(err)
			}
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.Header.Get("benchmark-task-id") != "configured-task" {
					t.Error("launch-time task ID was not forwarded")
				}
				var data any
				switch r.URL.Path {
				case "/api/concurrent":
					kind := "iphone_mirroring"
					if scenario == "other_bridge" {
						kind = "desktop"
					}
					if scenario == "redirect" {
						http.Redirect(w, r, "/unexpected", http.StatusFound)
						return
					}
					data = map[string]any{"bridge_type": kind}
				case "/health":
					data = map[string]any{"bridge_type": "iphone_mirroring", "window_visible": scenario != "window", "screen_recording": true, "accessibility": scenario != "permission"}
				case "/api/setup":
					body, _ := io.ReadAll(r.Body)
					if string(body) != `{}` {
						t.Error("reconnect must not reset or forward browser parameters")
					}
					if scenario == "busy" {
						w.WriteHeader(429)
						_, _ = w.Write([]byte(`{"ok":false}`))
						return
					}
					task := "configured-task"
					if scenario == "wrong_task" {
						task = "different-task"
					}
					data = map[string]any{"task_id": task, "reset": false}
				case "/api/providers/screenshot":
					encoded := base64.StdEncoding.EncodeToString(jpegBytes.Bytes())
					if scenario == "bad_frame" {
						encoded = "not a jpeg"
					}
					data = map[string]any{"image": encoded, "meta": map[string]any{"width": 2, "height": 3, "stale": scenario == "stale_frame"}, "capture_info": map[string]any{"capture_backend": "iphone-mirroring"}}
				default:
					t.Errorf("unexpected bridge operation %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
			}))
			defer bridge.Close()
			s := &Server{runtime: &Runtime{config: Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: bridge.URL, BenchmarkTaskID: "configured-task"}}}}
			request := func(method string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, "http://127.0.0.1:8080/api/internal/iphone-mirroring", strings.NewReader(`{"endpoint":"http://attacker.example","task_id":"wrong","reset":true}`))
				r.RemoteAddr = "127.0.0.1:4321"
				w := httptest.NewRecorder()
				s.handleInternalIPhoneMirroring(w, r)
				return w
			}
			read := request(http.MethodGet)
			if !reflect.DeepEqual(calls, []string{"GET /api/concurrent"}) {
				t.Fatalf("status read mutated the route: %v", calls)
			}
			if scenario == "other_bridge" && !strings.Contains(read.Body.String(), `"supported":false`) {
				t.Fatal("unrelated bridge exposed reconnect")
			}
			calls = nil
			w := request(http.MethodPost)
			if scenario == "restored" {
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"screenshot_available":true`) {
					t.Fatalf("restore failed: %d %s", w.Code, w.Body)
				}
				want := []string{"GET /api/concurrent", "GET /health", "POST /api/setup", "POST /api/providers/screenshot"}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("calls=%v", calls)
				}
				if strings.Contains(w.Body.String(), "image") || strings.Contains(w.Body.String(), "configured-task") {
					t.Fatal("response exposed frame or route identity")
				}
			} else {
				if w.Code < 400 || strings.Contains(w.Body.String(), `"restored":true`) {
					t.Fatalf("failure reported success: %d %s", w.Code, w.Body)
				}
				if scenario == "other_bridge" || scenario == "permission" || scenario == "window" || scenario == "redirect" {
					for _, call := range calls {
						if strings.HasPrefix(call, "POST ") {
							t.Fatalf("preflight failure changed route: %v", calls)
						}
					}
				}
				if scenario == "busy" && (w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"session_busy"`)) {
					t.Fatal("session conflict was not preserved")
				}
			}
		})
	}
}

func TestIPhoneMirroringRejectsUnsafeRequests(t *testing.T) {
	for _, test := range []struct{ remote, host, origin string }{
		{"192.168.1.2:20", "127.0.0.1:8080", ""},
		{"127.0.0.1:20", "attacker.example", ""},
		{"127.0.0.1:20", "127.0.0.1:8080", "http://attacker.example"},
	} {
		s := &Server{runtime: &Runtime{config: Config{}}}
		r := httptest.NewRequest(http.MethodPost, "http://"+test.host+"/api/internal/iphone-mirroring", nil)
		r.RemoteAddr = test.remote
		r.Header.Set("Origin", test.origin)
		w := httptest.NewRecorder()
		s.handleInternalIPhoneMirroring(w, r)
		if w.Code < 400 {
			t.Fatalf("accepted request from %s %s", test.remote, test.origin)
		}
	}
	for _, endpoint := range []string{"http://example.com", "http://user:secret@localhost:8897", "http://127.0.0.1:8897?url=other"} {
		s := &Server{runtime: &Runtime{config: Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: endpoint, BenchmarkTaskID: "task"}}}}
		r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/internal/iphone-mirroring", nil)
		r.RemoteAddr = "127.0.0.1:20"
		w := httptest.NewRecorder()
		s.handleInternalIPhoneMirroring(w, r)
		if w.Code != 409 {
			t.Fatalf("accepted non-local endpoint %s", endpoint)
		}
	}
}

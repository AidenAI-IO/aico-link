package configweb

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSpeakerProxyForwardsStatusProfilesAndCapture(t *testing.T) {
	var statusCalls, captureCalls, deleteCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/speaker/status":
			statusCalls.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"gate_enabled": false, "profile_count": 1})
		case r.Method == http.MethodGet && r.URL.Path == "/api/speaker/profiles":
			writeJSON(w, http.StatusOK, map[string]any{"profiles": []map[string]any{{"id": "owner"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/speaker/enroll/capture":
			captureCalls.Add(1)
			if r.URL.Query().Get("id") != "owner" {
				t.Fatalf("id = %q", r.URL.Query().Get("id"))
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": "owner", "status": "enrolled"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/speaker/profiles/owner":
			deleteCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected upstream %s %s", r.Method, r.URL.Path)
		}
	}))
	defer upstream.Close()

	options := testOptions(t)
	options.AgentHTTPBaseURL = upstream.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	statusResp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(statusResp, httptest.NewRequest(http.MethodGet, "/api/speaker/status", nil))
	if statusResp.Code != http.StatusOK || !strings.Contains(statusResp.Body.String(), `"profile_count":1`) {
		t.Fatalf("status=%d body=%s", statusResp.Code, statusResp.Body.String())
	}

	captureResp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(captureResp, httptest.NewRequest(http.MethodPost, "/api/speaker/enroll/capture?id=owner", nil))
	if captureResp.Code != http.StatusOK || !strings.Contains(captureResp.Body.String(), `"enrolled"`) {
		t.Fatalf("capture status=%d body=%s", captureResp.Code, captureResp.Body.String())
	}

	deleteResp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(deleteResp, httptest.NewRequest(http.MethodDelete, "/api/speaker/profiles/owner", nil))
	if deleteResp.Code != http.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete status=%d body=%s", deleteResp.Code, body)
	}

	if statusCalls.Load() != 1 || captureCalls.Load() != 1 || deleteCalls.Load() != 1 {
		t.Fatalf("calls status=%d capture=%d delete=%d", statusCalls.Load(), captureCalls.Load(), deleteCalls.Load())
	}
}

func TestSpeakerProxyForwardsLargeEnrollUpload(t *testing.T) {
	const uploadBytes = 128 * 1024 // above the generic 64 KiB API body cap
	var gotLen atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/speaker/enroll" {
			t.Fatalf("unexpected upstream %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		gotLen.Store(int64(len(body)))
		writeJSON(w, http.StatusOK, map[string]any{"id": "owner", "status": "enrolled"})
	}))
	defer upstream.Close()

	options := testOptions(t)
	options.AgentHTTPBaseURL = upstream.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte{0x01, 0x00}, uploadBytes/2)
	req := httptest.NewRequest(http.MethodPost, "/api/speaker/enroll?id=owner", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if gotLen.Load() != int64(uploadBytes) {
		t.Fatalf("upstream body len=%d, want %d (must not truncate at 64KiB)", gotLen.Load(), uploadBytes)
	}
}

func TestSpeakerProxyRejectsCrossOriginMutations(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	options := testOptions(t)
	options.AgentHTTPBaseURL = upstream.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://device.local/api/speaker/enroll/capture?id=owner", nil)
	req.Header.Set("Origin", "https://attacker.example")
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aico-agent/internal/netproxy"
)

type mirroringBridgeResponse struct {
	OK   bool            `json:"ok"`
	Data json.RawMessage `json:"data"`
}

// Only the running Agent knows the launch-time bridge endpoint and task ID.
// Neither value is accepted from the browser or inferred from the device type.
func (s *Server) handleInternalIPhoneMirroring(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := r.Host
	if value, _, err := net.SplitHostPort(host); err == nil {
		host = value
	}
	if !isLoopbackRequest(r) || !netproxy.IsLoopbackHost(host) {
		http.NotFound(w, r)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "http" || !strings.EqualFold(u.Host, r.Host) {
			writeMirroringError(w, http.StatusForbidden, "origin_rejected")
			return
		}
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		writeMirroringError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.runtime == nil {
		writeMirroringError(w, http.StatusServiceUnavailable, "agent_unavailable")
		return
	}
	bridge := s.runtime.ConfigSnapshot().EnvironmentBridge
	endpoint, err := url.Parse(strings.TrimRight(strings.TrimSpace(bridge.Endpoint), "/"))
	eligible := err == nil && bridge.Enabled && endpoint.Scheme == "http" &&
		netproxy.IsLoopbackHost(endpoint.Hostname()) && endpoint.User == nil && endpoint.RawQuery == "" && endpoint.Fragment == "" &&
		strings.TrimSpace(bridge.BenchmarkTaskID) != ""
	if !eligible {
		if r.Method == http.MethodGet {
			writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "supported": false})
		} else {
			writeMirroringError(w, http.StatusConflict, "not_iphone_mirroring")
		}
		return
	}
	if r.Method == http.MethodPost {
		// A second click must not start a competing setup/capture sequence.
		if !s.mirroringReconnectMu.TryLock() {
			writeMirroringError(w, http.StatusConflict, "reconnect_busy")
			return
		}
		defer s.mirroringReconnectMu.Unlock()
	}
	timeout := 3 * time.Second
	if r.Method == http.MethodPost {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	bridge.Endpoint = endpoint.String()
	data, _, err := requestMirroringBridge(ctx, bridge, http.MethodGet, "/api/concurrent", 64*1024)
	var identity struct {
		BridgeType string `json:"bridge_type"`
	}
	if err != nil || json.Unmarshal(data, &identity) != nil {
		writeMirroringError(w, http.StatusServiceUnavailable, "bridge_unavailable")
		return
	}
	if identity.BridgeType != "iphone_mirroring" {
		if r.Method == http.MethodGet {
			writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "supported": false})
		} else {
			writeMirroringError(w, http.StatusConflict, "not_iphone_mirroring")
		}
		return
	}
	if r.Method == http.MethodGet {
		writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "supported": true})
		return
	}

	// Health is read-only. It checks the window and capture permission before
	// setup claims a route, and exposes Accessibility separately for input.
	data, _, err = requestMirroringBridge(ctx, bridge, http.MethodGet, "/health", 64*1024)
	var health struct {
		BridgeType      string `json:"bridge_type"`
		WindowVisible   bool   `json:"window_visible"`
		ScreenRecording bool   `json:"screen_recording"`
		Accessibility   bool   `json:"accessibility"`
	}
	if err != nil || json.Unmarshal(data, &health) != nil || health.BridgeType != "iphone_mirroring" || !health.WindowVisible || !health.ScreenRecording {
		writeMirroringError(w, http.StatusServiceUnavailable, "mirror_unavailable")
		return
	}
	if !health.Accessibility {
		writeMirroringError(w, http.StatusServiceUnavailable, "input_permission_required")
		return
	}
	data, status, err := requestMirroringBridge(ctx, bridge, http.MethodPost, "/api/setup", 64*1024)
	if err != nil {
		if status == http.StatusTooManyRequests || status == http.StatusConflict {
			writeMirroringError(w, http.StatusConflict, "session_busy")
		} else {
			writeMirroringError(w, http.StatusServiceUnavailable, "setup_failed")
		}
		return
	}
	var setup struct {
		TaskID string `json:"task_id"`
		Reset  bool   `json:"reset"`
	}
	if json.Unmarshal(data, &setup) != nil || setup.TaskID != strings.TrimSpace(bridge.BenchmarkTaskID) || setup.Reset {
		writeMirroringError(w, http.StatusBadGateway, "setup_failed")
		return
	}
	data, _, err = requestMirroringBridge(ctx, bridge, http.MethodPost, "/api/providers/screenshot", 16*1024*1024)
	var frame struct {
		Image string `json:"image"`
		Meta  struct {
			Width, Height int
			Stale         bool
		} `json:"meta"`
		Capture struct {
			Backend string `json:"capture_backend"`
		} `json:"capture_info"`
	}
	if err != nil || json.Unmarshal(data, &frame) != nil || frame.Meta.Stale || frame.Capture.Backend != "iphone-mirroring" {
		writeMirroringError(w, http.StatusBadGateway, "capture_failed")
		return
	}
	image, err := base64.StdEncoding.DecodeString(frame.Image)
	if err != nil {
		writeMirroringError(w, http.StatusBadGateway, "capture_failed")
		return
	}
	size, err := jpeg.DecodeConfig(bytes.NewReader(image))
	if err != nil || size.Width <= 0 || size.Height <= 0 || size.Width != frame.Meta.Width || size.Height != frame.Meta.Height {
		writeMirroringError(w, http.StatusBadGateway, "capture_failed")
		return
	}
	// Capturing a frame proves the Agent route works, not that Apple's UI has
	// dismissed a phone-unlock or reconnect dialog. Do not claim the latter.
	writeAgentJSON(w, http.StatusOK, map[string]any{"ok": true, "supported": true, "restored": true, "screenshot_available": true})
}

func requestMirroringBridge(ctx context.Context, bridge EnvironmentBridgeConfig, method, path string, limit int64) (json.RawMessage, int, error) {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, bridge.Endpoint+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("benchmark-task-id", strings.TrimSpace(bridge.BenchmarkTaskID))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	var envelope mirroringBridgeResponse
	if err != nil || int64(len(data)) > limit || response.StatusCode != http.StatusOK || json.Unmarshal(data, &envelope) != nil || !envelope.OK {
		return nil, response.StatusCode, fmt.Errorf("bridge request failed")
	}
	return envelope.Data, response.StatusCode, nil
}

func writeMirroringError(w http.ResponseWriter, status int, code string) {
	writeAgentJSON(w, status, map[string]any{"ok": false, "error": "iPhone Mirroring connection could not be restored", "code": code})
}

package configweb

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Proxy only the running Agent's fixed internal endpoint; browser input can
// never select a different bridge, task, or reset mode.
func (s *Server) handleLocalIPhoneMirroring(w http.ResponseWriter, r *http.Request) {
	if !s.options.LocalAgent {
		http.NotFound(w, r)
		return
	}
	base, err := s.agentBaseURL()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent unavailable")
		return
	}
	base.Path = "/api/internal/iphone-mirroring"
	base.RawQuery = ""
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base.String(), strings.NewReader(`{}`))
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent unavailable")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "code": "agent_unavailable", "error": "local Agent unavailable"})
		return
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	var result struct {
		OK                  bool   `json:"ok"`
		Supported           bool   `json:"supported"`
		Restored            bool   `json:"restored"`
		ScreenshotAvailable bool   `json:"screenshot_available"`
		Code                string `json:"code,omitempty"`
		Error               string `json:"error,omitempty"`
	}
	if err != nil || len(data) > maxRequestBodySize || json.Unmarshal(data, &result) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "code": "agent_unavailable", "error": "local Agent connection status unavailable"})
		return
	}
	writeJSON(w, response.StatusCode, result)
}

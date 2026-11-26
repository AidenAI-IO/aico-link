package configweb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleMemoryReset exposes the product-level reset action from Config Web.
// The memory implementation lives in the Agent process, so Config Web proxies
// the request to the Agent's existing clear-all endpoint and returns its result
// without exposing the Agent service topology to the browser.
func (s *Server) handleMemoryReset(w http.ResponseWriter, r *http.Request) {
	if !s.proxyAgentReset(w, r, "/api/clear-all", "memory reset") {
		return
	}
	if err := s.scheduleAgentRestart(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "memory was reset, but Agent restart failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agent_restart_scheduled": true})
}

// handleConversationReset clears the current conversation history only, via
// the Agent's /api/clear. Memory is kept and the Agent keeps running.
func (s *Server) handleConversationReset(w http.ResponseWriter, r *http.Request) {
	if !s.proxyAgentReset(w, r, "/api/clear", "conversation reset") {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// proxyAgentReset POSTs to an Agent clear endpoint. It writes the error
// response itself and reports whether the Agent accepted the request.
func (s *Server) proxyAgentReset(w http.ResponseWriter, r *http.Request, agentPath, action string) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if !isSameOriginBrowserRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}

	base, err := s.agentBaseURL()
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return false
	}
	target, err := url.Parse(strings.TrimRight(base.String(), "/") + agentPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent "+action+" endpoint is invalid")
		return false
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), nil)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "create agent "+action+" request: "+err.Error())
		return false
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		s.invalidateAgentEndpoint(base)
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("agent %s request failed: %v", action, err))
		return false
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	if readErr != nil || len(body) > maxRequestBodySize || !json.Valid(body) {
		writeJSONError(w, http.StatusBadGateway, "agent "+action+" returned invalid JSON")
		return false
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		message, _ := payload["error"].(string)
		if message == "" {
			message = fmt.Sprintf("agent %s failed (HTTP %d)", action, response.StatusCode)
		}
		writeJSONError(w, response.StatusCode, message)
		return false
	}
	return true
}

func isSameOriginBrowserRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aico-agent/internal/agent"
)

func (s *Server) agentBaseURL() (*url.URL, error) {
	baseURL := strings.TrimSpace(s.options.AgentHTTPBaseURL)
	if baseURL == "" {
		// Share discovery across concurrent pollers, but periodically rediscover
		// custom ports even if the old endpoint remains reachable.
		s.agentEndpointMu.Lock()
		defer s.agentEndpointMu.Unlock()
		if s.agentEndpointURL != "" && time.Now().Before(s.agentEndpointExpires) {
			return url.Parse(s.agentEndpointURL)
		}
		status := s.queryAgentStatus()
		host, _ := status["port_host"].(string)
		port, _ := status["port"].(int)
		if host == "" {
			host = "127.0.0.1"
		}
		if port < 1 || port > 65535 {
			port = 8080
		}
		baseURL = fmt.Sprintf("http://%s", net.JoinHostPort(host, strconv.Itoa(port)))
		s.agentEndpointURL = baseURL
		s.agentEndpointExpires = time.Now().Add(30 * time.Second)
	}
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("agent HTTP endpoint is invalid")
	}
	return base, nil
}

// Failed requests invalidate discovery for the next request. Never replay a
// mutation automatically: the Agent may already have accepted it.
func (s *Server) invalidateAgentEndpoint(base *url.URL) {
	s.agentEndpointMu.Lock()
	defer s.agentEndpointMu.Unlock()
	cached, err := url.Parse(s.agentEndpointURL)
	if err == nil && cached.Host == base.Host {
		s.agentEndpointURL = ""
	}
}

// reloadAgentConfig asks the independent Agent process to re-read its config.
// Config Web owns persistence; Agent owns runtime application.
func (s *Server) reloadAgentConfig(ctx context.Context, revision uint64) (map[string]any, error) {
	body, err := json.Marshal(map[string]any{"revision": revision})
	if err != nil {
		return nil, err
	}
	base, err := s.agentBaseURL()
	if err != nil {
		return nil, err
	}
	target, _ := url.Parse("/api/internal/config/reload")
	base.Path = strings.TrimRight(base.Path, "/") + target.Path
	base.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		s.invalidateAgentEndpoint(base)
		return nil, fmt.Errorf("agent config reload request failed: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("agent config reload response failed: %w", err)
	}
	var payload map[string]any
	if len(data) > maxRequestBodySize || json.Unmarshal(data, &payload) != nil || payload == nil {
		return nil, fmt.Errorf("agent config reload returned invalid JSON")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || payload["ok"] != true || payload["applied"] != false || payload["pending"] != true {
		message, _ := payload["error"].(string)
		if message == "" {
			message = fmt.Sprintf("agent config reload failed (HTTP %d)", response.StatusCode)
		}
		return payload, errors.New(message)
	}
	return payload, nil
}

// fetchAgentApplicationStatus reads the Agent's loopback application status.
func (s *Server) fetchAgentApplicationStatus(ctx context.Context) ([]byte, int, error) {
	base, err := s.agentBaseURL()
	if err != nil {
		return nil, 0, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/internal/config/reload"
	base.RawQuery = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		s.invalidateAgentEndpoint(base)
		return nil, 0, fmt.Errorf("agent configuration status unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBodySize+1))
	if err != nil || len(data) > maxRequestBodySize || !json.Valid(data) {
		return nil, 0, fmt.Errorf("invalid agent configuration status")
	}
	if response.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("agent configuration status unavailable (HTTP %d)", response.StatusCode)
	}
	return data, response.StatusCode, nil
}

// handleConfigApplication exposes runtime application state without secrets.
func (s *Server) handleConfigApplication(w http.ResponseWriter, r *http.Request) {
	data, statusCode, fetchErr := s.fetchAgentApplicationStatus(r.Context())
	var live agent.ConfigApplyStatus
	if fetchErr == nil {
		fetchErr = json.Unmarshal(data, &live)
	}
	// Disk state must remain observable even when the Agent is unavailable or
	// has not been asked to apply an external edit. Keep runtime revisions intact.
	persistedRevision := agent.ConfigFileRevision(s.options.AgentConfigPath)
	respond := func(code int) {
		writeJSON(w, code, struct {
			agent.ConfigApplyStatus
			PersistedRevision uint64 `json:"persisted_revision"`
		}{live, persistedRevision})
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	// Re-read local state after the HTTP fetch: a save may have finished while
	// the status request was in flight. Service failures cannot be cleared by
	// an Agent restart; only a matching applied file revision clears reload errors.
	if fetchErr == nil && s.configApplyErrorRevision != 0 && live.Revision == s.configApplyErrorRevision &&
		(live.State == "applied" || live.State == "reboot_required") {
		s.configApplyError = ""
		s.configApplyErrorRevision = 0
	}
	if s.configSavePending {
		live.State, live.Applied, live.Pending, live.Error = "pending", false, true, ""
		respond(http.StatusOK)
		return
	}
	if s.configApplyError != "" {
		live.State, live.Applied, live.Pending, live.Error = "failed", false, false, s.configApplyError
		respond(http.StatusOK)
		return
	}
	if fetchErr != nil {
		live.State, live.Error = "unavailable", fetchErr.Error()
		respond(http.StatusServiceUnavailable)
		return
	}
	respond(statusCode)
}

func (s *Server) finishConfigSave() {
	s.configMu.Lock()
	s.configSavePending = false
	s.configMu.Unlock()
}

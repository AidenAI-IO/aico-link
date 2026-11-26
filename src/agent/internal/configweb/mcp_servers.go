package configweb

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"aico-agent/internal/mcpclient"
	"aico-agent/internal/netproxy"
)

func (s *Server) handleMCPServers(w http.ResponseWriter, r *http.Request, name string) {
	base, err := s.agentBaseURL()
	if err != nil || !netproxy.IsLoopbackHost(base.Hostname()) {
		writeJSONError(w, 503, "Agent must be reachable over loopback to manage MCP servers")
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, mcpclient.MaxConfigBytes+1))
	if err != nil || len(data) > mcpclient.MaxConfigBytes {
		writeJSONError(w, 413, "MCP configuration exceeds 64 KiB")
		return
	}
	base.Path = "/api/internal/mcp-servers"
	if name != "" {
		base.Path += "/" + name
	}
	base.RawQuery = ""
	base.Fragment = ""
	base.RawPath = ""
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base.String(), bytes.NewReader(data))
	if err != nil {
		writeJSONError(w, 500, "cannot create MCP management request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		s.invalidateAgentEndpoint(base)
		writeJSONError(w, 503, "Agent unavailable; start Agent and retry")
		return
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(result) > 1024*1024 {
		writeJSONError(w, 502, "invalid MCP management response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(result)
}

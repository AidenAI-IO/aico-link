package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aico-agent/internal/mcpclient"
	"aico-agent/internal/netproxy"
)

// Configuration can start executables on the Agent host. Only the authenticated
// Config Web API proxies to this loopback-only endpoint; MCP tokens never reach it.
func (s *Server) handleInternalMCPServers(w http.ResponseWriter, r *http.Request) {
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
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			http.Error(w, "origin rejected", http.StatusForbidden)
			return
		}
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		http.Error(w, "origin rejected", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if s.runtime == nil || s.runtime.mcpClients == nil {
		writeAgentJSON(w, 503, map[string]any{"ok": false, "error": "MCP installation requires an Agent configuration directory"})
		return
	}
	m := s.runtime.mcpClients
	name := strings.TrimPrefix(r.URL.Path, "/api/internal/mcp-servers")
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	var err error
	switch {
	case name == "" && r.Method == http.MethodGet:
	case name == "" && r.Method == http.MethodPost:
		var doc mcpclient.Document
		doc, err = mcpclient.Decode(r.Body)
		if err == nil {
			err = m.Install(ctx, doc)
		}
	case strings.HasPrefix(name, "/") && len(name) > 1 && !strings.Contains(name[1:], "/") && (r.Method == http.MethodDelete || r.Method == http.MethodPatch):
		if r.Method == http.MethodDelete {
			err = m.Change(ctx, name[1:], nil)
		} else {
			var patch struct {
				Enabled *bool `json:"enabled"`
			}
			d := json.NewDecoder(io.LimitReader(r.Body, 1025))
			d.DisallowUnknownFields()
			if d.Decode(&patch) != nil || patch.Enabled == nil || d.Decode(new(any)) != io.EOF {
				writeAgentJSON(w, 400, map[string]any{"ok": false, "error": "expected {\"enabled\": true|false}"})
				return
			}
			err = m.Change(ctx, name[1:], patch.Enabled)
		}
	default:
		http.Error(w, "method or path not supported", http.StatusMethodNotAllowed)
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeAgentJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeAgentJSON(w, 200, map[string]any{"ok": true, "servers": m.Status()})
}

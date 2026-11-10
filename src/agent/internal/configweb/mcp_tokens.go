package configweb

import (
	"errors"
	"net/http"
	"time"

	"aico-agent/internal/lanaccess"
	"aico-agent/internal/logging"
)

type mcpTokenRequest struct {
	Name string `json:"name"`
}

// mcpTokenManager reports whether r may list, create, or revoke MCP access
// tokens: a USB client, or a LAN client signed in with the device password.
// A token itself never grants this; it only reaches /mcp on the Agent.
func (s *Server) mcpTokenManager(r *http.Request) bool {
	return s.lanAccessRequestFromUSB(r) || lanaccess.IsAuthenticated(r)
}

func (s *Server) handleMCPTokensList(w http.ResponseWriter, r *http.Request) {
	if !s.mcpTokenManager(r) {
		writeJSONError(w, http.StatusForbidden, "MCP access tokens can be managed only over USB or after signing in")
		return
	}
	tokens, err := s.lanAccess.MCPTokens()
	if err != nil {
		logging.Errorf("config_web", "mcp_tokens", "read MCP access tokens: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to read MCP access tokens")
		return
	}
	if tokens == nil {
		tokens = []lanaccess.MCPToken{}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"tokens":     tokens,
		"lan_access": s.lanAccess.Enabled(),
		"max_tokens": lanaccess.MaxMCPTokens,
	})
}

// handleMCPTokenCreate returns the new token's secret once. It is not stored
// and cannot be shown again.
func (s *Server) handleMCPTokenCreate(w http.ResponseWriter, r *http.Request) {
	if !s.mcpTokenManager(r) {
		writeJSONError(w, http.StatusForbidden, "MCP access tokens can be managed only over USB or after signing in")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	var request mcpTokenRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	token, secret, err := s.lanAccess.CreateMCPToken(request.Name, time.Now())
	switch {
	case errors.Is(err, lanaccess.ErrMCPTokenName), errors.Is(err, lanaccess.ErrMCPTokenLimit):
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		logging.Errorf("config_web", "mcp_tokens", "create MCP access token: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to create the MCP access token")
		return
	}
	logging.Infof("config_web", "mcp_tokens", "MCP access token %s created", token.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "token": token, "secret": secret})
}

func (s *Server) handleMCPTokenRevoke(w http.ResponseWriter, r *http.Request, id string) {
	if !s.mcpTokenManager(r) {
		writeJSONError(w, http.StatusForbidden, "MCP access tokens can be managed only over USB or after signing in")
		return
	}
	if !sameOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "cross-origin request rejected")
		return
	}
	switch err := s.lanAccess.RevokeMCPToken(id); {
	case errors.Is(err, lanaccess.ErrMCPTokenNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		logging.Errorf("config_web", "mcp_tokens", "revoke MCP access token %s: %v", id, err)
		writeJSONError(w, http.StatusInternalServerError, "failed to revoke the MCP access token")
		return
	}
	logging.Infof("config_web", "mcp_tokens", "MCP access token %s revoked", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

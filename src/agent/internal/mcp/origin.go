package mcp

import (
	"net/http"
	"net/url"
	"strings"
)

// validateOrigin enforces the Streamable HTTP Origin check.
// Missing Origin is allowed (non-browser MCP clients / curl).
// When present, Origin must match the request Host (scheme-insensitive host:port).
func validateOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	reqHost := strings.TrimSpace(r.Host)
	if reqHost == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, reqHost)
}

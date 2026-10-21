package mcp

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// validateOrigin enforces the Streamable HTTP Origin check for browser clients.
// Missing Origin is allowed (non-browser MCP host apps / curl).
//
// When Origin is present it must:
//  1. Parse as an absolute URL with a host
//  2. Use a non-rebindable hostname (IP literal or localhost)
//  3. Match the request Host (scheme-insensitive host:port)
//
// Matching Origin to Host alone is not DNS-rebinding protection: after rebinding,
// both headers carry the attacker's name. Requiring an IP / localhost hostname
// blocks that case. Clients should address the Agent by IP (or localhost).
func validateOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if !originHostnameAllowed(parsed.Hostname()) {
		return false
	}
	reqHost := strings.TrimSpace(r.Host)
	if reqHost == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, reqHost)
}

func originHostnameAllowed(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	return net.ParseIP(host) != nil
}

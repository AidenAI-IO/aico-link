package lanaccess

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// CrossSiteReason reports why Guard refuses r as a browser request made on
// behalf of another site, or "" when r may proceed.
//
// A USB client can pair for a session without the password, and with no
// password set every reachable client is admitted, but the phone or computer
// at the other end of the cable also runs a browser. A page it loads can send
// requests to 192.168.42.1, and those arrive on the USB socket like the app's
// own. Native clients (the app, MCP hosts, curl) send no
// Origin and no Sec-Fetch-Site, so these checks only affect browsers:
//
//   - Sec-Fetch-Site: cross-site is a request started by another site, except
//     a top-level GET navigation (a link to a shared file followed from a chat
//     page): it carries no body, and the page that started it cannot read the
//     response. Frames are still refused.
//   - An Origin must name the host the request was sent to, by IP address or
//     localhost. "null" (sandboxed frames, file pages) is refused.
//   - The Host must be an IP address or localhost. A DNS rebinding page reaches
//     the board under the attacker's own name, so its requests are same-origin
//     and carry no cross-site marker; only the name in Host gives it away.
//     Address the board by IP.
func CrossSiteReason(r *http.Request) string {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") && !topLevelNavigation(r) {
		return "cross_site_request"
	}
	if !nonRebindableHost(hostOnly(r.Host)) {
		return "host_not_allowed"
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return ""
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "origin_not_allowed"
	}
	if !nonRebindableHost(parsed.Hostname()) || !strings.EqualFold(parsed.Host, strings.TrimSpace(r.Host)) {
		return "origin_not_allowed"
	}
	return ""
}

func hostOnly(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return strings.Trim(hostport, "[]")
}

func nonRebindableHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil
}

func topLevelNavigation(r *http.Request) bool {
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Mode")), "navigate") &&
		strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Dest")), "document")
}

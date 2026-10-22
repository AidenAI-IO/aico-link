package netproxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"
)

var proxyAssignmentFragments = []string{
	"http_proxy=", "HTTP_PROXY=",
	"https_proxy=", "HTTPS_PROXY=",
	"all_proxy=", "ALL_PROXY=",
}

func Parse(raw string, allowedSchemes ...string) (*url.URL, error) {
	if raw == "" {
		return nil, fmt.Errorf("expected absolute proxy URL, for example http://127.0.0.1:7890")
	}
	if hasProxyWhitespace(raw) {
		return nil, fmt.Errorf("proxy URL contains whitespace; use one export assignment per line")
	}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("expected absolute proxy URL, for example http://127.0.0.1:7890")
	}
	if hasEmbeddedProxyAssignment(trimmed) {
		return nil, fmt.Errorf("proxy URL contains another proxy assignment; use one export assignment per line")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" || u.Host == "" || u.Hostname() == "" {
		return nil, fmt.Errorf("expected absolute proxy URL, for example http://127.0.0.1:7890")
	}
	if strings.Contains(strings.TrimPrefix(strings.ToLower(trimmed), u.Scheme+"://"), "://") {
		return nil, fmt.Errorf("duplicate scheme in proxy URL")
	}

	scheme := strings.ToLower(u.Scheme)
	if len(allowedSchemes) == 0 {
		allowedSchemes = []string{"http", "https", "socks5", "socks5h"}
	}
	for _, allowed := range allowedSchemes {
		if scheme == strings.ToLower(strings.TrimSpace(allowed)) {
			return u, nil
		}
	}
	return nil, fmt.Errorf("unsupported proxy URL scheme %q", u.Scheme)
}

func Validate(raw string, allowedSchemes ...string) error {
	if raw == "" {
		return nil
	}
	_, err := Parse(raw, allowedSchemes...)
	return err
}

func EnvForRequest(req *http.Request) (string, string) {
	if req == nil || req.URL == nil {
		return "", ""
	}
	switch strings.ToLower(req.URL.Scheme) {
	case "http":
		return firstEnv("HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy")
	case "https":
		return firstEnv("HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy")
	default:
		return firstEnv("ALL_PROXY", "all_proxy")
	}
}

func ProxyFromEnvironment(req *http.Request, allowedSchemes ...string) (*url.URL, error) {
	name, raw := EnvForRequest(req)
	if raw == "" {
		return nil, nil
	}
	if req != nil && req.URL != nil {
		if IsLoopbackHost(req.URL.Hostname()) {
			return nil, nil
		}
		if Bypass(req.URL.Hostname(), req.URL.Port(), NoProxyFromEnvironment()) {
			return nil, nil
		}
	}
	proxyURL, err := Parse(raw, allowedSchemes...)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy %s=%q: %w", name, raw, err)
	}
	return proxyURL, nil
}

func NoProxyFromEnvironment() string {
	_, value := firstEnv("NO_PROXY", "no_proxy")
	return value
}

func firstEnv(keys ...string) (string, string) {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return key, value
		}
	}
	return "", ""
}

func hasProxyWhitespace(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

func hasEmbeddedProxyAssignment(value string) bool {
	lower := strings.ToLower(value)
	for _, fragment := range proxyAssignmentFragments {
		if pos := strings.Index(lower, strings.ToLower(fragment)); pos > 0 {
			return true
		}
	}
	return false
}

// IsLoopbackHost reports whether host is localhost or a loopback IP, which
// never goes through a proxy.
func IsLoopbackHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func Bypass(host, port, noProxy string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return false
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}

	for _, rawRule := range strings.FieldsFunc(noProxy, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		rule := strings.TrimSpace(strings.ToLower(rawRule))
		if rule == "" {
			continue
		}
		if rule == "*" {
			return true
		}
		if strings.Contains(rule, "/") {
			if ip := net.ParseIP(host); ip != nil {
				if _, network, err := net.ParseCIDR(rule); err == nil && network.Contains(ip) {
					return true
				}
			}
			continue
		}
		if ruleIP := net.ParseIP(rule); ruleIP != nil {
			if hostIP := net.ParseIP(host); hostIP != nil && hostIP.Equal(ruleIP) {
				return true
			}
			continue
		}
		if strings.Contains(rule, ":") && !strings.HasPrefix(rule, ".") {
			if strings.EqualFold(hostPort, rule) {
				return true
			}
			continue
		}
		if strings.HasPrefix(rule, "*.") {
			base := strings.TrimPrefix(rule, "*.")
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		rule = strings.TrimPrefix(rule, ".")
		if host == rule || strings.HasSuffix(host, "."+rule) {
			return true
		}
	}
	return false
}

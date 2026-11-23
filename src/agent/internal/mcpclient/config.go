// Package mcpclient manages user-installed MCP tool servers.
package mcpclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const MaxConfigBytes = 64 * 1024
const maxServers = 16

var serverName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

// Config accepts the common MCP client JSON format. Commands run on the Agent
// host, directly (without a shell). Dependencies must already be available there.
type Config struct {
	Type     string            `json:"type,omitempty"`
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type Document struct {
	Servers map[string]Config `json:"mcpServers"`
}

func Decode(r io.Reader) (Document, error) {
	return decode(r, false)
}

func decode(r io.Reader, allowEmpty bool) (Document, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigBytes+1))
	if err != nil || len(data) > MaxConfigBytes {
		return Document{}, fmt.Errorf("MCP configuration exceeds 64 KiB or cannot be read")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var doc Document
	if err := d.Decode(&doc); err != nil {
		return doc, fmt.Errorf("invalid MCP configuration; expected {\"mcpServers\": {\"name\": {\"command\": ..., \"args\": [...]}}}")
	}
	if d.Decode(new(any)) != io.EOF {
		return doc, fmt.Errorf("expected a single JSON object")
	}
	if doc.Servers == nil || (!allowEmpty && len(doc.Servers) == 0) || len(doc.Servers) > maxServers {
		return doc, fmt.Errorf("mcpServers must contain 1 to 16 servers")
	}
	for name, cfg := range doc.Servers {
		if err := cfg.validate(name); err != nil {
			return doc, fmt.Errorf("%s: %w", name, err)
		}
	}
	return doc, nil
}

func (c Config) transport() string {
	if c.Type != "" {
		return c.Type
	}
	if c.URL != "" {
		return "http"
	}
	return "stdio"
}

func (c Config) validate(name string) error {
	if !serverName.MatchString(name) {
		return fmt.Errorf("server name must be 1-32 letters, digits, underscores or hyphens")
	}
	switch c.transport() {
	case "stdio":
		if strings.TrimSpace(c.Command) == "" || c.URL != "" || len(c.Headers) != 0 {
			return fmt.Errorf("stdio requires command and accepts args/env, not url/headers")
		}
		for key, value := range c.Env {
			if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
				return fmt.Errorf("invalid environment variable")
			}
		}
	case "http", "streamable-http":
		u, err := url.Parse(c.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("HTTP requires an http(s) URL without userinfo or fragment")
		}
		if c.Command != "" || len(c.Args) != 0 || len(c.Env) != 0 {
			return fmt.Errorf("HTTP accepts url/headers, not command/args/env")
		}
		for key, value := range c.Headers {
			if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("invalid HTTP header")
			}
			switch strings.ToLower(key) {
			case "host", "content-type", "accept", "connection", "content-length", "mcp-session-id", "mcp-protocol-version", "mcp-method", "mcp-name":
				return fmt.Errorf("MCP transport headers cannot be overridden")
			}
		}
	default:
		return fmt.Errorf("supported transports: stdio and http (Streamable HTTP)")
	}
	return nil
}

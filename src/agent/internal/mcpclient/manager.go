package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const connectTimeout = 30 * time.Second
const callTimeout = 60 * time.Second
const maxTools = 256

type Status struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Enabled   bool     `json:"enabled"`
	State     string   `json:"state"`
	Error     string   `json:"error,omitempty"`
	Tools     []string `json:"tools"`
}

type entry struct {
	config  Config
	status  Status
	session *mcp.ClientSession
	tools   []*Tool
}

// Manager owns sessions and the private configuration file. Mutations are
// serialized; model calls and status reads do not wait on installation/network IO.
type Manager struct {
	op      sync.Mutex
	mu      sync.RWMutex
	path    string
	entries map[string]*entry
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	started bool
}

func New(path string) (*Manager, error) {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{path: path, entries: map[string]*entry{}, ctx: ctx, cancel: cancel}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot open MCP configuration")
	}
	defer f.Close()
	// An empty map is valid on disk after the last server is removed.
	doc, err := decode(f, true)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid saved MCP configuration")
	}
	for name, cfg := range doc.Servers {
		m.entries[name] = newEntry(name, cfg)
	}
	return m, nil
}

func newEntry(name string, cfg Config) *entry {
	state := "disconnected"
	if cfg.Disabled {
		state = "disabled"
	}
	return &entry{config: cfg, status: Status{Name: name, Transport: cfg.transport(), Enabled: !cfg.Disabled, State: state, Tools: []string{}}}
}

// Start reconnects saved servers without delaying Agent startup.
func (m *Manager) Start() {
	m.op.Lock()
	defer m.op.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true
	for _, e := range m.entries {
		if m.ctx.Err() != nil {
			return
		}
		if e.config.Disabled || e.session != nil {
			continue
		}
		connected, err := m.connect(m.ctx, e.status.Name, e.config)
		m.mu.Lock()
		if err != nil {
			e.status.State, e.status.Error = "error", err.Error()
		} else {
			m.entries[e.status.Name] = connected
		}
		m.mu.Unlock()
		if err == nil {
			m.watch(connected)
		}
	}
}

func (m *Manager) Status() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Status, 0, len(m.entries))
	for _, e := range m.entries {
		s := e.status
		s.Tools = append([]string{}, s.Tools...)
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (m *Manager) Tools() []*Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []*Tool
	for _, e := range m.entries {
		result = append(result, e.tools...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name() < result[j].Name() })
	return result
}

// Install validates and connects every imported server before saving anything.
// Existing names are rejected so pasted configuration cannot replace secrets.
func (m *Manager) Install(ctx context.Context, doc Document) error {
	if !m.op.TryLock() {
		return fmt.Errorf("MCP installation or reconnection is in progress; retry shortly")
	}
	defer m.op.Unlock()
	if m.closed {
		return fmt.Errorf("MCP manager is closed")
	}
	if len(doc.Servers) == 0 || len(m.entries)+len(doc.Servers) > maxServers {
		return fmt.Errorf("install 1 to 16 servers in total")
	}
	for name, cfg := range doc.Servers {
		if err := cfg.validate(name); err != nil {
			return err
		}
		if _, ok := m.entries[name]; ok {
			return fmt.Errorf("server %s already installed; remove it before replacing", name)
		}
	}
	added := map[string]*entry{}
	committed := false
	defer func() {
		if !committed {
			for _, e := range added {
				closeEntry(e)
			}
		}
	}()
	for name, cfg := range doc.Servers {
		e := newEntry(name, cfg)
		if !cfg.Disabled {
			var err error
			e, err = m.connect(ctx, name, cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		added[name] = e
	}
	next := m.copyEntries()
	for name, e := range added {
		next[name] = e
	}
	if err := m.save(next); err != nil {
		return err
	}
	m.mu.Lock()
	m.entries = next
	m.mu.Unlock()
	committed = true
	for _, e := range added {
		m.watch(e)
	}
	return nil
}

// Change enables/reconnects, disables or removes a server, then publishes its
// new catalog atomically. Previously captured tool handles check session identity.
func (m *Manager) Change(ctx context.Context, name string, enabled *bool) error {
	if !m.op.TryLock() {
		return fmt.Errorf("MCP installation or reconnection is in progress; retry shortly")
	}
	defer m.op.Unlock()
	if m.closed {
		return fmt.Errorf("MCP manager is closed")
	}
	old, ok := m.entries[name]
	if !ok {
		return os.ErrNotExist
	}
	next := m.copyEntries()
	var replacement *entry
	if enabled == nil {
		delete(next, name)
	} else {
		cfg := old.config
		cfg.Disabled = !*enabled
		replacement = newEntry(name, cfg)
		if *enabled {
			var err error
			replacement, err = m.connect(ctx, name, cfg)
			if err != nil {
				return err
			}
		}
		next[name] = replacement
	}
	if err := m.save(next); err != nil {
		closeEntry(replacement)
		return err
	}
	m.mu.Lock()
	m.entries = next
	m.mu.Unlock()
	closeEntry(old)
	m.watch(replacement)
	return nil
}

func (m *Manager) copyEntries() map[string]*entry {
	next := make(map[string]*entry, len(m.entries))
	for name, e := range m.entries {
		next[name] = e
	}
	return next
}

func (m *Manager) save(entries map[string]*entry) error {
	doc := Document{Servers: map[string]Config{}}
	for name, e := range entries {
		doc.Servers[name] = e.config
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil || len(data) > MaxConfigBytes {
		return fmt.Errorf("saved MCP configuration would exceed 64 KiB")
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return fmt.Errorf("cannot create MCP configuration directory")
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".mcp-*.json")
	if err != nil {
		return fmt.Errorf("cannot write MCP configuration")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), m.path)
	}
	if err != nil {
		return fmt.Errorf("cannot save MCP configuration")
	}
	return nil
}

type headerTransport struct {
	headers map[string]string
	base    http.RoundTripper
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header = r.Header.Clone()
	for key, value := range t.headers {
		r.Header.Set(key, value)
	}
	return t.base.RoundTrip(r)
}

func (m *Manager) connect(ctx context.Context, name string, cfg Config) (*entry, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	defer cancel()
	var transport mcp.Transport
	switch cfg.transport() {
	case "stdio":
		command := exec.Command(cfg.Command, cfg.Args...)
		if command.Err != nil {
			return nil, fmt.Errorf("command unavailable on Agent host; install its runtime or use an absolute executable path")
		}
		command.Env = os.Environ()
		for key, value := range cfg.Env {
			command.Env = append(command.Env, key+"="+value)
		}
		// Protocol stdout belongs to the SDK; stderr must not leak credentials
		// into application logs or management responses.
		command.Stderr = io.Discard
		transport = &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}
	default:
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: &http.Client{
			Transport:     headerTransport{headers: cfg.Headers, base: http.DefaultTransport},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}, MaxRetries: -1}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "aico-agent", Version: "1.0.0"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("MCP handshake failed; check transport, credentials and server availability")
	}
	e := newEntry(name, cfg)
	e.session = session
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			closeEntry(e)
			return nil, fmt.Errorf("MCP tool discovery failed")
		}
		if len(e.tools) >= maxTools {
			closeEntry(e)
			return nil, fmt.Errorf("server exceeds 256 tools")
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if tool.Name == "" || !ok || schema["type"] != "object" {
			closeEntry(e)
			return nil, fmt.Errorf("server returned an invalid tool schema")
		}
		wrapped := &Tool{manager: m, server: name, remote: tool.Name, session: session, description: tool.Description, schema: schema}
		e.tools = append(e.tools, wrapped)
		e.status.Tools = append(e.status.Tools, wrapped.Name())
	}
	if len(e.tools) == 0 {
		closeEntry(e)
		return nil, fmt.Errorf("server exposes no tools; resource-only and prompt-only servers are not supported")
	}
	e.status.State = "connected"
	return e, nil
}

func closeEntry(e *entry) {
	if e != nil && e.session != nil {
		_ = e.session.Close()
	}
}

func (m *Manager) watch(e *entry) {
	if e == nil || e.session == nil {
		return
	}
	go func() {
		_ = e.session.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.entries[e.status.Name] == e {
			e.status.State, e.status.Error, e.tools = "error", "Connection closed; reconnect this server", nil
		}
	}()
}
func (m *Manager) Close() {
	m.cancel()
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	old := m.entries
	m.entries = map[string]*entry{}
	m.closed = true
	m.mu.Unlock()
	for _, e := range old {
		closeEntry(e)
	}
}

// Tool implements the Agent's typed tool interface without losing nested
// JSON schemas or MCP's structured content and isError semantics.
type Tool struct {
	manager                     *Manager
	server, remote, description string
	session                     *mcp.ClientSession
	schema                      map[string]any
}

func (t *Tool) Name() string {
	hash := sha256.Sum256([]byte(t.server + "\x00" + t.remote))
	label := serverNameLabel(t.remote)
	if len(label) > 10 {
		label = label[:10]
	}
	return fmt.Sprintf("mcp_%s_%s_%x", t.server, label, hash[:8])
}
func serverNameLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}
func (t *Tool) Description() string {
	return fmt.Sprintf("MCP %s / %s: %s", t.server, t.remote, t.description)
}
func (t *Tool) ArgsSchema() map[string]any  { return t.schema }
func (t *Tool) PreserveJSONArguments() bool { return true }
func (t *Tool) Call(ctx context.Context, input string) (string, error) {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &args) != nil || args == nil {
		return "", fmt.Errorf("MCP tool arguments must be a JSON object")
	}
	t.manager.mu.RLock()
	e := t.manager.entries[t.server]
	active := e != nil && e.session == t.session && !e.config.Disabled
	t.manager.mu.RUnlock()
	if !active {
		return "", fmt.Errorf("MCP server was disabled, removed or reconnected")
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	result, err := t.session.CallTool(ctx, &mcp.CallToolParams{Name: t.remote, Arguments: args})
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, mcp.ErrConnectionClosed) {
			t.manager.mu.Lock()
			if t.manager.entries[t.server] == e {
				e.status.State, e.status.Error, e.tools = "error", "Connection closed; reconnect this server", nil
			}
			t.manager.mu.Unlock()
		}
		return "", fmt.Errorf("MCP call failed; check connection or reconnect the server")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("invalid MCP tool result")
	}
	if result.IsError {
		return string(data), errors.New(string(data))
	}
	return string(data), nil
}

// Package usbuplink owns the opt-in USB Internet policy and short-lived host
// sessions. Only the switch is persistent; leases use Go's monotonic clock.
package usbuplink

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	DefaultSettingsPath = "/userdata/system/usb-internet.json"
	DefaultSocketPath   = "/run/wifi_proxy/usb-internet/control.sock"
	APIPath             = "/api/network/usb-internet"
	LeaseDuration       = 30 * time.Second
	ProxyPort           = "18081"
)

var tokenPattern = regexp.MustCompile(`^[a-f0-9]{32,64}$`)

type Request struct {
	Enabled    *bool  `json:"enabled,omitempty"`
	ID         string `json:"session_id,omitempty"`
	Host       string `json:"host,omitempty"`
	Peer       string `json:"peer,omitempty"` // Set by Config Web, never by a remote caller.
	Password   string `json:"password,omitempty"`
	UnixTimeMS int64  `json:"unix_time_ms,omitempty"`
}

type Status struct {
	Enabled   bool   `json:"enabled"`
	Active    bool   `json:"active"`
	Host      string `json:"host,omitempty"`
	ExpiresIn int    `json:"expires_in_seconds"`
}

type Manager struct {
	mu      sync.Mutex
	path    string
	enabled bool
	lease   *Lease
	now     func() time.Time
}

// Lease is immutable apart from expiry and connection ownership under mu.
// Revocation closes even established CONNECT/streaming connections.
type Lease struct {
	ID          string
	Host        string
	Peer        string
	Upstream    *url.URL
	mu          sync.Mutex
	expires     time.Time
	ctx         context.Context
	cancel      context.CancelFunc
	connections map[*leasedConn]struct{}
}

func New(path string) (*Manager, error) {
	m := &Manager{path: path, now: time.Now}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	var settings struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, err
	}
	m.enabled = settings.Enabled
	return m, nil
}

func (m *Manager) currentLocked() *Lease {
	if m.lease != nil {
		m.lease.mu.Lock()
		expired := !m.now().Before(m.lease.expires)
		m.lease.mu.Unlock()
		if !m.enabled || expired {
			m.lease.revoke()
			m.lease = nil
		}
	}
	return m.lease
}

func (m *Manager) Current() *Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentLocked()
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Enabled: m.enabled}
	if lease := m.currentLocked(); lease != nil {
		lease.mu.Lock()
		s.Active, s.Host = true, lease.Host
		s.ExpiresIn = int(lease.expires.Sub(m.now()).Seconds())
		lease.mu.Unlock()
	}
	return s
}

func (m *Manager) SetEnabled(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled == enabled {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".usb-internet-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	data, _ := json.Marshal(struct {
		Enabled bool `json:"enabled"`
	}{enabled})
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	m.enabled = enabled
	m.currentLocked()
	return nil
}

func (m *Manager) Register(request Request) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled {
		return http.StatusForbidden, errors.New("USB Internet is disabled")
	}
	if !tokenPattern.MatchString(request.ID) {
		return http.StatusBadRequest, errors.New("invalid session ID")
	}
	ip := net.ParseIP(request.Peer)
	if ip == nil {
		return http.StatusBadRequest, errors.New("invalid USB peer")
	}
	upstream := &url.URL{Scheme: "socks5", Host: net.JoinHostPort(ip.String(), ProxyPort)}
	switch request.Host {
	case "mac":
		if !ip.IsLoopback() {
			return http.StatusForbidden, errors.New("Mac sessions require the local SSH tunnel")
		}
	case "android":
		if ip.IsLoopback() || !tokenPattern.MatchString(request.Password) {
			return http.StatusBadRequest, errors.New("Android sessions require a USB peer and temporary credentials")
		}
		upstream.User = url.UserPassword("aico", request.Password)
	default:
		return http.StatusBadRequest, errors.New("unsupported host")
	}
	if existing := m.currentLocked(); existing != nil {
		if existing.ID != request.ID || existing.Peer != request.Peer || existing.Upstream.String() != upstream.String() {
			return http.StatusConflict, errors.New("another USB Internet session is active")
		}
		existing.mu.Lock()
		existing.expires = m.now().Add(LeaseDuration)
		existing.mu.Unlock()
		return http.StatusOK, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.lease = &Lease{ID: request.ID, Host: request.Host, Peer: request.Peer, Upstream: upstream,
		expires: m.now().Add(LeaseDuration), ctx: ctx, cancel: cancel, connections: make(map[*leasedConn]struct{})}
	return http.StatusOK, nil
}

func (m *Manager) Release(id, peer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.currentLocked()
	if lease == nil {
		return true
	}
	if lease.ID != id || lease.Peer != peer {
		return false
	}
	lease.revoke()
	m.lease = nil
	return true
}

func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lease != nil {
		m.lease.revoke()
		m.lease = nil
	}
}

func (l *Lease) revoke() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancel()
	for conn := range l.connections {
		_ = conn.Conn.Close()
	}
	l.connections = make(map[*leasedConn]struct{})
}

// Dial interrupts an in-progress connect on revoke and refuses to attach a
// socket if the lease expired or was disabled while the connect completed.
func (l *Lease) Dial(ctx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	defer func() { stop(); cancel() }()
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil || !time.Now().Before(l.expires) {
		conn.Close()
		return nil, errors.New("USB Internet session ended")
	}
	wrapped := &leasedConn{Conn: conn, lease: l}
	l.connections[wrapped] = struct{}{}
	return wrapped, nil
}

type leasedConn struct {
	net.Conn
	lease *Lease
}

func (c *leasedConn) Close() error {
	c.lease.mu.Lock()
	delete(c.lease.connections, c)
	c.lease.mu.Unlock()
	return c.Conn.Close()
}
func (c *leasedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

// ServeHTTP is served only on a root-owned Unix socket. Config Web enforces
// USB ingress and same-origin policy before forwarding requests here.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == APIPath {
		json.NewEncoder(w).Encode(m.Status())
		return
	}
	var request Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	switch {
	case r.Method == http.MethodPut && r.URL.Path == APIPath:
		if request.Enabled == nil {
			http.Error(w, "enabled is required", http.StatusBadRequest)
			return
		}
		if err := m.SetEnabled(*request.Enabled); err != nil {
			http.Error(w, "cannot save USB Internet setting", http.StatusInternalServerError)
			return
		}
	case r.Method == http.MethodPut && r.URL.Path == APIPath+"/session":
		if code, err := m.Register(request); err != nil {
			http.Error(w, err.Error(), code)
			return
		}
	case r.Method == http.MethodDelete && r.URL.Path == APIPath+"/session":
		if !m.Release(request.ID, request.Peer) {
			http.Error(w, "session owner mismatch", http.StatusConflict)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(m.Status())
}

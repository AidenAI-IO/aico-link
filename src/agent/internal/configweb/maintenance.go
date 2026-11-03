package configweb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// maintenanceController is deliberately separate from the HTTP job store. A
// single lock covers backup, restore, OTA and destructive storage operations,
// including operations started by another Config Web process.
type maintenanceController struct {
	mu         sync.Mutex
	path       string
	file       *os.File
	operation  string
	jobID      string
	phase      string
	startedAt  time.Time
	cancelable bool
}

type maintenanceLease struct {
	owner *maintenanceController
	once  sync.Once
}

type maintenanceSnapshot struct {
	Operation  string    `json:"operation"`
	JobID      string    `json:"job_id"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"started_at"`
	Cancelable bool      `json:"cancelable"`
}

func newMaintenanceController(path string) *maintenanceController {
	return &maintenanceController{path: path}
}

func (m *maintenanceController) begin(operation, jobID string, cancelable bool) (*maintenanceLease, error) {
	if m == nil {
		return nil, errors.New("maintenance controller is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file != nil {
		return nil, &maintenanceBusyError{Snapshot: maintenanceSnapshot{
			Operation: m.operation, JobID: m.jobID, Phase: m.phase,
			StartedAt: m.startedAt, Cancelable: m.cancelable,
		}}
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return nil, fmt.Errorf("create maintenance lock directory: %w", err)
	}
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open maintenance lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, &maintenanceBusyError{}
		}
		return nil, fmt.Errorf("lock maintenance file: %w", err)
	}
	m.file = f
	m.operation, m.jobID, m.phase = operation, jobID, "starting"
	m.startedAt, m.cancelable = time.Now().UTC(), cancelable
	return &maintenanceLease{owner: m}, nil
}

func (l *maintenanceLease) Update(phase string) {
	if l == nil || l.owner == nil {
		return
	}
	l.owner.mu.Lock()
	if l.owner.file != nil {
		l.owner.phase = strings.TrimSpace(phase)
	}
	l.owner.mu.Unlock()
}

func (l *maintenanceLease) Release() {
	if l == nil || l.owner == nil {
		return
	}
	l.once.Do(func() {
		m := l.owner
		m.mu.Lock()
		f := m.file
		m.file = nil
		m.operation, m.jobID, m.phase = "", "", ""
		m.startedAt = time.Time{}
		m.cancelable = false
		m.mu.Unlock()
		if f != nil {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		}
	})
}

func (m *maintenanceController) snapshot() (maintenanceSnapshot, bool) {
	if m == nil {
		return maintenanceSnapshot{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.file == nil {
		return maintenanceSnapshot{}, false
	}
	return maintenanceSnapshot{Operation: m.operation, JobID: m.jobID, Phase: m.phase,
		StartedAt: m.startedAt, Cancelable: m.cancelable}, true
}

func (m *maintenanceController) active() bool {
	_, ok := m.snapshot()
	return ok
}

type maintenanceBusyError struct{ Snapshot maintenanceSnapshot }

func (e *maintenanceBusyError) Error() string { return "maintenance operation is already in progress" }

type serviceController interface {
	Active(context.Context, string) (bool, error)
	Stop(context.Context, string) error
	Start(context.Context, string) error
}

type systemdServiceController struct{ binary string }

func (c *systemdServiceController) run(ctx context.Context, action, unit string) error {
	if strings.TrimSpace(c.binary) == "" {
		return errors.New("systemctl binary is empty")
	}
	result := runCommandContext(ctx, 60*time.Second, nil, nil, c.binary, action, unit)
	if result.ExitCode != 0 {
		return fmt.Errorf("systemctl %s %s failed: %s", action, unit, strings.TrimSpace(string(result.Output)))
	}
	return nil
}

func (c *systemdServiceController) Active(ctx context.Context, unit string) (bool, error) {
	if strings.TrimSpace(c.binary) == "" {
		return false, errors.New("systemctl binary is empty")
	}
	result := runCommandContext(ctx, 5*time.Second, nil, nil, c.binary, "is-active", "--quiet", unit)
	if result.TimedOut {
		return false, context.DeadlineExceeded
	}
	if result.ExitCode == 0 {
		return true, nil
	}
	if result.ExitCode == 3 || result.ExitCode == 4 {
		return false, nil
	}
	return false, fmt.Errorf("systemctl is-active %s failed", unit)
}

func (c *systemdServiceController) Stop(ctx context.Context, unit string) error {
	return c.run(ctx, "stop", unit)
}

func (c *systemdServiceController) Start(ctx context.Context, unit string) error {
	return c.run(ctx, "start", unit)
}

// maintenanceSessionStore implements the USB-local capability handshake. Only
// hashes of opaque values are retained, so a process dump cannot directly
// replay a bearer token or CSRF value.
type maintenanceSessionStore struct {
	mu        sync.Mutex
	address   net.IP
	network   *net.IPNet
	session   *maintenanceSession
	jobTokens map[string]jobTransferToken
	maxAge    time.Duration
	// busy reports whether a maintenance job currently runs.  While it does,
	// the session that started it cannot be taken over by another client;
	// otherwise a new client (a fresh CLI invocation, a reloaded page on a
	// different browser) replaces the idle session instead of waiting for it
	// to expire.
	busy func() bool
}

type maintenanceSession struct {
	tokenHash [sha256.Size]byte
	csrfHash  [sha256.Size]byte
	token     string
	csrf      string
	expiresAt time.Time
}

type jobTransferToken struct {
	jobID     string
	operation string
	tokenHash [sha256.Size]byte
	expiresAt time.Time
}

func newMaintenanceSessionStore(address, subnet string) *maintenanceSessionStore {
	store := &maintenanceSessionStore{jobTokens: make(map[string]jobTransferToken), maxAge: time.Hour}
	store.address = net.ParseIP(strings.TrimSpace(address))
	_, store.network, _ = net.ParseCIDR(strings.TrimSpace(subnet))
	return store
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashToken(value string) [sha256.Size]byte { return sha256.Sum256([]byte(value)) }

func (s *maintenanceSessionStore) create(r *http.Request) (token, csrf string, expires time.Time, err error) {
	if !isUSBRequest(r, s.address, s.network) {
		return "", "", time.Time{}, &backupAPIError{Code: "usb_required", Status: http.StatusForbidden, Message: "connect through the USB ECM link"}
	}
	if !sameOrigin(r) {
		return "", "", time.Time{}, &backupAPIError{Code: "cross_origin_rejected", Status: http.StatusForbidden, Message: "cross-origin maintenance request rejected"}
	}
	client := strings.TrimSpace(r.Header.Get("X-Aico-Client"))
	if client == "" || len(client) > 128 {
		return "", "", time.Time{}, &backupAPIError{Code: "invalid_client", Status: http.StatusBadRequest, Message: "X-Aico-Client is required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if s.session != nil && now.Before(s.session.expiresAt) {
		if cookie, cookieErr := r.Cookie("aico_maintenance"); cookieErr == nil && hashToken(cookie.Value) == s.session.tokenHash {
			return s.session.token, s.session.csrf, s.session.expiresAt, nil
		}
		if s.busy != nil && s.busy() {
			return "", "", time.Time{}, &backupAPIError{Code: "maintenance_session_busy", Status: http.StatusConflict, Message: "a maintenance session is already active", Retryable: true}
		}
	}
	token, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	csrf, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	expires = now.Add(s.maxAge)
	s.session = &maintenanceSession{tokenHash: hashToken(token), csrfHash: hashToken(csrf), token: token, csrf: csrf, expiresAt: expires}
	return token, csrf, expires, nil
}

// authorizeStatus admits read-only status requests that carry no secrets:
// the caller only has to arrive over the USB link from the same origin.
func (s *maintenanceSessionStore) authorizeStatus(r *http.Request) bool {
	return s != nil && isUSBRequest(r, s.address, s.network) && sameOrigin(r)
}

func (s *maintenanceSessionStore) authorize(r *http.Request, mutate bool) bool {
	if s == nil || !isUSBRequest(r, s.address, s.network) || !sameOrigin(r) {
		return false
	}
	if mutate {
		if _, err := uuid.Parse(strings.TrimSpace(r.Header.Get("X-Aico-Request-ID"))); err != nil {
			return false
		}
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	bearer := strings.HasPrefix(authorization, "Bearer ")
	value := ""
	if bearer {
		value = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	} else {
		if cookie, err := r.Cookie("aico_maintenance"); err == nil {
			value = cookie.Value
		}
	}
	if value == "" {
		return false
	}
	hash := hashToken(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil || time.Now().UTC().After(s.session.expiresAt) || hash != s.session.tokenHash {
		return false
	}
	if mutate && !bearer {
		csrf := hashToken(strings.TrimSpace(r.Header.Get("X-Aico-CSRF-Token")))
		if csrf != s.session.csrfHash {
			return false
		}
	}
	return true
}

func (s *maintenanceSessionStore) issueJobToken(jobID, operation string) (string, time.Time, error) {
	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	s.mu.Lock()
	s.jobTokens[jobID] = jobTransferToken{jobID: jobID, operation: operation, tokenHash: hashToken(token), expiresAt: expires}
	s.mu.Unlock()
	return token, expires, nil
}

func (s *maintenanceSessionStore) authorizeJob(r *http.Request, jobID, operation string, mutate bool) bool {
	if s.authorize(r, mutate) {
		return true
	}
	if !isUSBRequest(r, s.address, s.network) || !sameOrigin(r) {
		return false
	}
	value := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if value == "" {
		return false
	}
	hash := hashToken(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	token, ok := s.jobTokens[jobID]
	if !ok || token.operation != operation || time.Now().UTC().After(token.expiresAt) || token.tokenHash != hash {
		return false
	}
	return !mutate
}

func (s *maintenanceSessionStore) revokeJobToken(jobID string) {
	s.mu.Lock()
	delete(s.jobTokens, jobID)
	s.mu.Unlock()
}

func isUSBRequest(r *http.Request, address net.IP, network *net.IPNet) bool {
	if r == nil {
		return false
	}
	if usb, known := r.Context().Value(usbIngressContextKey{}).(bool); known && !usb {
		return false
	}
	remoteHost, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		remoteHost = strings.TrimSpace(r.RemoteAddr)
	}
	remote := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remote == nil || network == nil || !network.Contains(remote) {
		return false
	}
	local := ""
	if value := r.Context().Value(http.LocalAddrContextKey); value != nil {
		if addr, ok := value.(net.Addr); ok {
			local, _, _ = net.SplitHostPort(addr.String())
		}
	}
	if local != "" && net.ParseIP(local) != nil && !net.ParseIP(local).IsUnspecified() && !net.ParseIP(local).Equal(address) {
		return false
	}
	return true
}

func sameOrigin(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("Origin"))
	if value == "" {
		value = strings.TrimSpace(r.Header.Get("Referer"))
	}
	if value == "" {
		return true
	}
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

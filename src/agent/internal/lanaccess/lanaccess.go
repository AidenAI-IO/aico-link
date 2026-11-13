// Package lanaccess guards the board's services with two independent
// settings: the LAN access switch and the device password.
//
//   - The LAN access switch decides who can reach the services at all. While
//     it is off, only the USB ECM link (a socket bound to the USB interface)
//     and the board itself are answered; while it is on, LAN clients such as
//     those on Wi-Fi are answered too.
//   - The device password decides whether a client must sign in. With no
//     password, every client that can reach the services is admitted. With
//     one, every client except the board itself needs a session token,
//     whether it arrives over the LAN or over USB. A LAN client signs in with
//     the password; a USB client is paired silently, because holding the
//     cable proves ownership (see auth.go).
//
// The password is also the password of the aico login account used by the
// terminal and SSH. Without one the account is locked: nobody can log in to
// a shell until the owner sets a password. Clearing the password returns to
// that state and leaves the switch alone.
package lanaccess

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Default state files. They live on userdata so they survive OTA.
const (
	DefaultEnabledPath  = "/userdata/system/lan-access-enabled"
	DefaultPasswordPath = "/userdata/system/device-password"
)

const (
	MinPasswordLength = 6
	// bcrypt ignores everything after 72 bytes; reject longer input instead
	// of silently truncating it.
	MaxPasswordLength = 72
	bcryptCost        = 10
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
)

// State is the LAN access switch, the device password hash, and the session
// signing key, all kept as files so the Agent and Config Web processes see
// the same state without coordinating.
type State struct {
	EnabledPath   string
	PasswordPath  string
	SecretPath    string
	MCPTokensPath string

	secretMu sync.Mutex
	mcpMu    sync.Mutex
}

// NewState uses the default path for each empty argument. MCP access tokens
// are kept next to the device password.
func NewState(enabledPath, passwordPath, secretPath string) *State {
	if strings.TrimSpace(enabledPath) == "" {
		enabledPath = DefaultEnabledPath
	}
	if strings.TrimSpace(passwordPath) == "" {
		passwordPath = DefaultPasswordPath
	}
	if strings.TrimSpace(secretPath) == "" {
		secretPath = DefaultSecretPath
	}
	return &State{
		EnabledPath:   enabledPath,
		PasswordPath:  passwordPath,
		SecretPath:    secretPath,
		MCPTokensPath: filepath.Join(filepath.Dir(passwordPath), filepath.Base(DefaultMCPTokensPath)),
	}
}

// Enabled reports whether LAN access is on. Any error other than a missing
// marker counts as off, so a damaged userdata fails closed.
func (s *State) Enabled() bool {
	info, err := os.Stat(s.EnabledPath)
	return err == nil && info.Mode().IsRegular()
}

// PasswordSet reports whether a device password is stored, and so whether
// clients must sign in. Only a missing or empty file means no password; a
// file that cannot be read counts as set, so a damaged userdata fails closed.
func (s *State) PasswordSet() bool {
	data, err := os.ReadFile(s.PasswordPath)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	return err != nil || strings.TrimSpace(string(data)) != ""
}

// SetEnabled turns LAN access on or off. It does not touch the password or
// sessions: the switch only decides whether LAN clients are answered.
func (s *State) SetEnabled(enabled bool) error {
	if !enabled {
		if err := os.Remove(s.EnabledPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeFileAtomic(s.EnabledPath, []byte("1\n"))
}

// ClearPassword removes the device password, as if none had ever been set,
// and revokes every session. The LAN access switch and MCP access tokens are
// separate settings and are kept.
func (s *State) ClearPassword() error {
	if err := s.revokeSessions(); err != nil {
		return err
	}
	if err := os.Remove(s.PasswordPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SetPassword stores the bcrypt hash of a new device password and signs
// every client out, over the LAN and over USB alike. Sessions are revoked
// before the password changes, so a failure can never leave the new password
// in place with sessions signed under the old one.
func (s *State) SetPassword(password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.revokeSessions(); err != nil {
		return err
	}
	return writeFileAtomic(s.PasswordPath, []byte(hash+"\n"))
}

// ValidatePassword enforces the length rules for a new password.
func ValidatePassword(password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if len(password) > MaxPasswordLength {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword returns a bcrypt hash. libxcrypt accepts the same string, so it
// is also written to /etc/shadow as the aico account password.
func HashPassword(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type usbConnContextKey struct{}

type admittedContextKey struct{}

// IsAdmitted reports whether Guard admitted r as a client: it carried a valid
// session, or no device password is set. Requests from the board itself are
// served without this mark; see IsLocal.
func IsAdmitted(r *http.Request) bool {
	admitted, _ := r.Context().Value(admittedContextKey{}).(bool)
	return admitted
}

type usbListener struct{ net.Listener }

type usbConn struct{ net.Conn }

func (l usbListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return usbConn{conn}, nil
}

// USBListener wraps a listener whose connections can only come from the USB
// link, such as one bound with SO_BINDTODEVICE.
func USBListener(listener net.Listener) net.Listener { return usbListener{listener} }

// ConnContext is an http.Server ConnContext hook that records whether the
// connection came from a USBListener.
func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	// An HTTPS server hands over its TLS wrapper; the USB marker is on the
	// connection underneath.
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn = tlsConn.NetConn()
	}
	_, usb := conn.(usbConn)
	return WithUSB(ctx, usb)
}

// WithUSB records whether a connection arrived on the USB link.
func WithUSB(ctx context.Context, usb bool) context.Context {
	return context.WithValue(ctx, usbConnContextKey{}, usb)
}

// IsUSB reports whether a request arrived on the USB link. Such a request
// always reaches the services and may pair for a session, but it still needs
// that session while a device password is set.
func IsUSB(r *http.Request) bool {
	usb, _ := r.Context().Value(usbConnContextKey{}).(bool)
	return usb
}

// IsLocal reports whether a request came from the board itself, which is
// always served without credentials. Linux drops packets that arrive on a
// real interface with a loopback or local source address, so neither can be
// forged from the network.
func IsLocal(r *http.Request) bool {
	remote, ok := addrIP(r.RemoteAddr)
	if !ok {
		return false
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil {
		return false
	}
	localIP, ok := addrIP(local.String())
	if !ok {
		return false
	}
	return remote == localIP || (remote.IsLoopback() && localIP.IsLoopback())
}

func addrIP(address string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		host = strings.TrimSpace(address)
	}
	ip, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

const disabledMessage = "LAN access is off. Connect over USB and turn it on in Settings > Advanced > LAN access."

const disabledPage = `<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Aico Link</title></head>` +
	`<body style="font-family:system-ui,sans-serif;max-width:32rem;margin:15vh auto;padding:0 16px;line-height:1.5">` +
	`<h1 style="font-size:1.25rem">LAN access is off / 局域网访问未开启</h1>` +
	`<p>Connect over USB and turn it on in Settings &gt; Advanced &gt; LAN access.</p>` +
	`<p>请通过 USB 连接设备，在 设置 &gt; 高级设置 &gt; 局域网访问 中开启。</p></body></html>`

// Guard applies the two settings to every request. Browser requests made on
// behalf of another site are refused first, wherever they come from (see
// CrossSiteReason). Then:
//
//   - /mcp needs an MCP access token from every client and nothing else, so
//     it works over Wi-Fi with LAN access off (see serveMCP).
//   - LAN requests are refused while LAN access is off.
//   - The board itself is served without credentials.
//   - Everyone else is admitted when no device password is set, and needs a
//     session token when one is. Guard serves the login and pairing
//     endpoints itself.
func Guard(state *State, next http.Handler) http.Handler {
	auth := &authHandler{state: state, limiter: newLoginLimiter(), now: time.Now}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reason := CrossSiteReason(r); reason != "" {
			rejectCrossSite(w, reason)
			return
		}
		if r.URL.Path == MCPPath {
			serveMCP(state, w, r, next)
			return
		}
		local := IsLocal(r)
		if !local && !IsUSB(r) && !state.Enabled() {
			rejectLANDisabled(w, r)
			return
		}
		switch {
		case isAuthPath(r.URL.Path):
			auth.serveLogin(w, r)
		case local:
			next.ServeHTTP(w, r)
		case !state.PasswordSet() || sessionValid(state, w, r, auth.now()):
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), admittedContextKey{}, true)))
		default:
			rejectUnauthenticated(w, r)
		}
	})
}

// serveMCP admits /mcp requests that carry an MCP access token, from any
// client, whether LAN access is on or off. Creating a token, which needs an
// admitted client, is the user's consent to MCP; the token reaches
// nothing but /mcp. A session token does not count, and neither does arriving
// over USB or from the board.
func serveMCP(state *State, w http.ResponseWriter, r *http.Request, next http.Handler) {
	if !mcpTokenValid(state, r) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("WWW-Authenticate", `Bearer realm="aico-mcp"`)
		writeAuthJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":      false,
			"error":   "mcp_token_required",
			"message": "Create an MCP access token in Settings > Advanced > MCP and send it as Authorization: Bearer <token>.",
		})
		return
	}
	next.ServeHTTP(w, r)
}

func rejectLANDisabled(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, disabledPage)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"error":   "lan_access_disabled",
		"message": disabledMessage,
	})
}

func rejectCrossSite(w http.ResponseWriter, reason string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      false,
		"error":   reason,
		"message": "Requests from other sites are refused. Open the board by its IP address.",
	})
}

// ErrListenStopped is returned by RetryListen when stop closes first.
var ErrListenStopped = errors.New("listener retry stopped")

// RetryListen calls listen until it succeeds or stop is closed. The USB
// gadget can come up after the service starts, so the device-bound socket is
// retried quickly at first and then polled slowly.
func RetryListen(listen func() (net.Listener, error), stop <-chan struct{}, onError func(int, error)) (net.Listener, error) {
	return retryListen(listen, stop, usbRetryDelay, onError)
}

// usbRetryDelay maps a failed attempt count to the delay before the next try.
func usbRetryDelay(attempt int) time.Duration {
	if attempt <= 30 {
		return 2 * time.Second
	}
	return 15 * time.Second
}

// retryListen is RetryListen with an injectable delay schedule; onError sees
// each failed attempt number before the wait.
func retryListen(listen func() (net.Listener, error), stop <-chan struct{}, delay func(int) time.Duration, onError func(int, error)) (net.Listener, error) {
	for attempt := 1; ; attempt++ {
		listener, err := listen()
		if err == nil {
			return listener, nil
		}
		if onError != nil {
			onError(attempt, err)
		}
		timer := time.NewTimer(delay(attempt))
		select {
		case <-stop:
			timer.Stop()
			return nil, ErrListenStopped
		case <-timer.C:
		}
	}
}

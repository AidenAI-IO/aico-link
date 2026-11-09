// Package lanaccess controls whether the board's developer services can be
// reached from networks other than the USB link.
//
// Requests that arrive on the USB ECM link (through a socket bound to the USB
// interface) or from the board itself are always trusted: only someone with
// the cable, or a process already on the board, can make them. Every other
// request, such as one from Wi-Fi, is refused until LAN access is turned on
// over USB. Turning it on requires a device password, which becomes the
// password of the aico login account used by the terminal and SSH.
package lanaccess

import (
	"context"
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
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Default state files. They live on userdata so they survive OTA.
const (
	DefaultEnabledPath  = "/userdata/system/lan-access-enabled"
	DefaultPasswordPath = "/userdata/system/device-password"
)

const (
	MinPasswordLength = 8
	// bcrypt ignores everything after 72 bytes; reject longer input instead
	// of silently truncating it.
	MaxPasswordLength = 72
	bcryptCost        = 10
)

var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", MaxPasswordLength)
	ErrPasswordRequired = errors.New("set a device password before turning on LAN access")
)

// State is the LAN access switch and the device password hash, both kept as
// files so the Agent and Config Web processes see the same state without
// coordinating.
type State struct {
	EnabledPath  string
	PasswordPath string
}

func NewState(enabledPath, passwordPath string) *State {
	if strings.TrimSpace(enabledPath) == "" {
		enabledPath = DefaultEnabledPath
	}
	if strings.TrimSpace(passwordPath) == "" {
		passwordPath = DefaultPasswordPath
	}
	return &State{EnabledPath: enabledPath, PasswordPath: passwordPath}
}

// Enabled reports whether LAN access is on. Any error other than a missing
// marker counts as off, so a damaged userdata fails closed.
func (s *State) Enabled() bool {
	info, err := os.Stat(s.EnabledPath)
	return err == nil && info.Mode().IsRegular()
}

// PasswordSet reports whether a device password hash is stored.
func (s *State) PasswordSet() bool {
	data, err := os.ReadFile(s.PasswordPath)
	return err == nil && strings.TrimSpace(string(data)) != ""
}

// SetEnabled turns LAN access on or off. Turning it on requires a stored
// device password. Turning it off keeps the password.
func (s *State) SetEnabled(enabled bool) error {
	if !enabled {
		if err := os.Remove(s.EnabledPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if !s.PasswordSet() {
		return ErrPasswordRequired
	}
	return writeFileAtomic(s.EnabledPath, []byte("1\n"))
}

// SetPassword stores the bcrypt hash of a new device password.
func (s *State) SetPassword(password string) error {
	hash, err := HashPassword(password)
	if err != nil {
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

type trustedConnContextKey struct{}

type trustedListener struct{ net.Listener }

type trustedConn struct{ net.Conn }

func (l trustedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return trustedConn{conn}, nil
}

// TrustListener wraps a listener whose connections can only come from the USB
// link, such as one bound with SO_BINDTODEVICE.
func TrustListener(listener net.Listener) net.Listener { return trustedListener{listener} }

// ConnContext is an http.Server ConnContext hook that records whether the
// connection came from a TrustListener.
func ConnContext(ctx context.Context, conn net.Conn) context.Context {
	_, trusted := conn.(trustedConn)
	return WithTrusted(ctx, trusted)
}

// WithTrusted records the ingress trust decision on a context.
func WithTrusted(ctx context.Context, trusted bool) context.Context {
	return context.WithValue(ctx, trustedConnContextKey{}, trusted)
}

// IsTrusted reports whether a request came through a trusted listener or from
// the board itself. Linux drops packets that arrive on a real interface with
// a loopback or local source address, so neither can be forged from the
// network.
func IsTrusted(r *http.Request) bool {
	if trusted, _ := r.Context().Value(trustedConnContextKey{}).(bool); trusted {
		return true
	}
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

// Guard refuses requests from outside the USB link while LAN access is off.
func Guard(state *State, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsTrusted(r) || state.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
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

package lanaccess

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newState(t *testing.T) *State {
	t.Helper()
	root := filepath.Join(t.TempDir(), "system")
	return NewState(filepath.Join(root, "lan-access-enabled"), filepath.Join(root, "device-password"), filepath.Join(root, "lan-session-secret"))
}

func newRequest(remote, local string, trusted bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://board/api/tools", nil)
	r.RemoteAddr = remote
	ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 8080})
	if trusted {
		ctx = WithTrusted(ctx, true)
	}
	return r.WithContext(ctx)
}

func serve(h http.Handler, r *http.Request) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func guarded(state *State) http.Handler {
	return Guard(state, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

func TestGuardRefusesLANUntilEnabled(t *testing.T) {
	state := newState(t)
	h := guarded(state)
	lan := func() *http.Request { return newRequest("192.168.50.20:5000", "192.168.50.76", false) }
	// A LAN host that routes to the USB address still arrives on the
	// wildcard socket, so it is not trusted.
	routedToUSB := func() *http.Request { return newRequest("192.168.50.20:5000", "192.168.42.1", false) }

	for name, r := range map[string]*http.Request{
		"usb":       newRequest("192.168.42.100:5000", "192.168.42.1", true),
		"loopback":  newRequest("127.0.0.1:5000", "127.0.0.1", false),
		"same host": newRequest("192.168.50.76:5000", "192.168.50.76", false),
	} {
		if code := serve(h, r); code != http.StatusNoContent {
			t.Fatalf("%s request while off: %d", name, code)
		}
	}
	if code := serve(h, lan()); code != http.StatusForbidden {
		t.Fatalf("LAN request while off: %d", code)
	}
	if code := serve(h, routedToUSB()); code != http.StatusForbidden {
		t.Fatalf("LAN request routed to the USB address while off: %d", code)
	}

	if err := state.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if code := serve(h, lan()); code != http.StatusForbidden {
		t.Fatalf("a password alone must not open LAN access: %d", code)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if code := serve(h, lan()); code != http.StatusUnauthorized {
		t.Fatalf("LAN request without a session while on: %d", code)
	}
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	withToken := lan()
	withToken.Header.Set("Authorization", "Bearer "+token)
	if code := serve(h, withToken); code != http.StatusNoContent {
		t.Fatalf("LAN request with a session while on: %d", code)
	}
	if err := state.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if code := serve(h, lan()); code != http.StatusForbidden {
		t.Fatalf("LAN request after turning off: %d", code)
	}
	if !state.PasswordSet() {
		t.Fatal("turning LAN access off must keep the password")
	}
}

func TestEnablingRequiresPassword(t *testing.T) {
	state := newState(t)
	if err := state.SetEnabled(true); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("enable without password: %v", err)
	}
	if state.Enabled() {
		t.Fatal("enabled without a password")
	}
	if err := state.SetEnabled(false); err != nil {
		t.Fatalf("disabling an already-off switch: %v", err)
	}
}

func TestSetPasswordStoresRootOnlyBcryptHash(t *testing.T) {
	state := newState(t)
	if err := state.SetPassword("short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password: %v", err)
	}
	if err := state.SetPassword(strings.Repeat("a", MaxPasswordLength+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password: %v", err)
	}
	if err := state.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(state.PasswordPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode = %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(state.PasswordPath)
	if !strings.HasPrefix(string(data), "$2") || strings.Contains(string(data), "correct horse") {
		t.Fatalf("password file is not a bcrypt hash: %q", data)
	}
}

func TestEnabledFailsClosedOnNonRegularMarker(t *testing.T) {
	state := newState(t)
	if err := os.MkdirAll(state.EnabledPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if state.Enabled() {
		t.Fatal("a directory at the marker path must not count as enabled")
	}
}

func TestTrustListenerMarksConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	trusted := TrustListener(listener)
	defer trusted.Close()
	go func() {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err == nil {
			conn.Close()
		}
	}()
	conn, err := trusted.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := ConnContext(context.Background(), conn)
	r := httptest.NewRequest(http.MethodGet, "http://board/", nil).WithContext(ctx)
	r.RemoteAddr = "192.168.50.20:5000"
	if !IsTrusted(r) {
		t.Fatal("connection from a TrustListener is not trusted")
	}
}

func TestRetryListenRecoversAfterTransientFailures(t *testing.T) {
	attempts := 0
	var seen []int
	listener, err := retryListen(func() (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("no such device")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}, make(chan struct{}), func(int) time.Duration { return time.Millisecond }, func(attempt int, _ error) {
		seen = append(seen, attempt)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if attempts != 3 || len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("attempts=%d seen=%v", attempts, seen)
	}
}

func TestRetryListenStopsOnShutdown(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	_, err := retryListen(func() (net.Listener, error) {
		return nil, errors.New("no such device")
	}, stop, func(int) time.Duration { return time.Hour }, nil)
	if !errors.Is(err, ErrListenStopped) {
		t.Fatalf("err = %v", err)
	}
}

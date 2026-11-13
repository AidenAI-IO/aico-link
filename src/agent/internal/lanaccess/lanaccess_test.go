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

func newRequest(remote, local string, usb bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "http://"+local+"/api/tools", nil)
	r.RemoteAddr = remote
	ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(local), Port: 8080})
	if usb {
		ctx = WithUSB(ctx, true)
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

// The switch decides who is answered, the password decides who must sign
// in, and the two are independent.
func TestGuardAppliesSwitchAndPasswordIndependently(t *testing.T) {
	clients := map[string]func() *http.Request{
		"usb":      func() *http.Request { return newRequest("192.168.42.100:5000", "192.168.42.1", true) },
		"lan":      func() *http.Request { return newRequest("192.168.50.20:5000", "192.168.50.76", false) },
		"loopback": func() *http.Request { return newRequest("127.0.0.1:5000", "127.0.0.1", false) },
		// A LAN host that routes to the USB address still arrives on the
		// wildcard socket, so it is not a USB client.
		"lan routed to usb": func() *http.Request { return newRequest("192.168.50.20:5000", "192.168.42.1", false) },
	}
	for _, tc := range []struct {
		enabled, password bool
		want              map[string]int
	}{
		{false, false, map[string]int{"usb": 204, "lan": 403, "loopback": 204, "lan routed to usb": 403}},
		{true, false, map[string]int{"usb": 204, "lan": 204, "loopback": 204, "lan routed to usb": 204}},
		{false, true, map[string]int{"usb": 401, "lan": 403, "loopback": 204, "lan routed to usb": 403}},
		{true, true, map[string]int{"usb": 401, "lan": 401, "loopback": 204, "lan routed to usb": 401}},
	} {
		state := newState(t)
		if tc.password {
			if err := state.SetPassword("luckfox"); err != nil {
				t.Fatal(err)
			}
		}
		if err := state.SetEnabled(tc.enabled); err != nil {
			t.Fatal(err)
		}
		h := guarded(state)
		for name, client := range clients {
			if code := serve(h, client()); code != tc.want[name] {
				t.Errorf("enabled=%v password=%v %s: %d, want %d", tc.enabled, tc.password, name, code, tc.want[name])
			}
		}
		if !tc.password {
			continue
		}
		token, _, err := state.IssueToken(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for name, client := range clients {
			r := client()
			r.Header.Set("Authorization", "Bearer "+token)
			want := 204
			if tc.want[name] == 403 {
				want = 403
			}
			if code := serve(h, r); code != want {
				t.Errorf("enabled=%v %s with a session: %d, want %d", tc.enabled, name, code, want)
			}
		}
	}
}

func TestSwitchKeepsPasswordAndSessions(t *testing.T) {
	state := newState(t)
	if err := state.SetEnabled(true); err != nil {
		t.Fatalf("LAN access must turn on without a password: %v", err)
	}
	if err := state.SetPassword("luckfox"); err != nil {
		t.Fatal(err)
	}
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !state.PasswordSet() {
		t.Fatal("the switch removed the password")
	}
	if _, err := state.VerifyToken(token, time.Now()); err != nil {
		t.Fatalf("the switch signed clients out: %v", err)
	}
}

func TestClearPasswordKeepsSwitchAndMCPTokens(t *testing.T) {
	state := newState(t)
	if err := state.SetPassword("luckfox"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := state.CreateMCPToken("laptop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := state.ClearPassword(); err != nil {
		t.Fatal(err)
	}
	if !state.Enabled() || state.PasswordSet() {
		t.Fatalf("after clearing: enabled=%v password=%v", state.Enabled(), state.PasswordSet())
	}
	if _, err := state.VerifyToken(token, time.Now()); err == nil {
		t.Fatal("a session survived clearing the password")
	}
	if !state.VerifyMCPToken(secret) {
		t.Fatal("clearing the password revoked an MCP access token")
	}
}

func TestPasswordSetFailsClosedOnUnreadableFile(t *testing.T) {
	state := newState(t)
	if err := os.MkdirAll(state.PasswordPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if !state.PasswordSet() {
		t.Fatal("an unreadable password file must count as set")
	}
}

func TestSetPasswordStoresRootOnlyBcryptHash(t *testing.T) {
	state := newState(t)
	if err := state.SetPassword("12345"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("short password: %v", err)
	}
	if err := state.SetPassword(strings.Repeat("a", MaxPasswordLength+1)); !errors.Is(err, ErrPasswordTooLong) {
		t.Fatalf("long password: %v", err)
	}
	if err := state.SetPassword("123456"); err != nil {
		t.Fatalf("six characters: %v", err)
	}
	info, err := os.Stat(state.PasswordPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("password file mode = %v", info.Mode().Perm())
	}
	data, _ := os.ReadFile(state.PasswordPath)
	if !strings.HasPrefix(string(data), "$2") || strings.Contains(string(data), "123456") {
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

func TestUSBListenerMarksConnections(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	usb := USBListener(listener)
	defer usb.Close()
	go func() {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err == nil {
			conn.Close()
		}
	}()
	conn, err := usb.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx := ConnContext(context.Background(), conn)
	r := httptest.NewRequest(http.MethodGet, "http://board/", nil).WithContext(ctx)
	r.RemoteAddr = "192.168.50.20:5000"
	if !IsUSB(r) {
		t.Fatal("connection from a USBListener is not marked as USB")
	}
}

// An HTTPS server hands ConnContext its TLS wrapper. A USB listener's marker
// must be seen through it, and an ordinary TLS listener must not gain it.
func TestConnContextSeesUSBThroughTLS(t *testing.T) {
	for _, usb := range []bool{true, false} {
		var seen bool
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "192.168.50.10:50000"
			seen = IsUSB(r)
		}))
		if usb {
			srv.Listener = USBListener(srv.Listener)
		}
		srv.Config.ConnContext = ConnContext
		srv.StartTLS()
		resp, err := srv.Client().Get(srv.URL)
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if seen != usb {
			t.Fatalf("TLS request on a listener with usb=%t: IsUSB=%t", usb, seen)
		}
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

package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func newLANAccessServer(t *testing.T) (*Server, string) {
	t.Helper()
	options := testOptions(t)
	root := t.TempDir()
	options.LANAccessPath = filepath.Join(root, "system", "lan-access-enabled")
	options.DevicePasswordPath = filepath.Join(root, "system", "device-password")
	options.SessionSecretPath = filepath.Join(root, "system", "lan-session-secret")
	marker := filepath.Join(root, "synced")
	script := filepath.Join(root, "sync")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho run >>'"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options.LANAccessSync = script
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	return server, marker
}

func callLANAccess(t *testing.T, server *Server, method, body string, usb bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1/api/security/lan-access", bytes.NewBufferString(body))
	req.RemoteAddr = "127.0.0.2:1234"
	req = req.WithContext(context.WithValue(req.Context(), usbIngressContextKey{}, usb))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	decoded := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded
}

func TestLANAccessStatusDefaultsOff(t *testing.T) {
	server, _ := newLANAccessServer(t)
	code, body := callLANAccess(t, server, http.MethodGet, "", false)
	if code != http.StatusOK || body["enabled"] != false || body["password_set"] != false || body["via_usb"] != false {
		t.Fatalf("status: %d %v", code, body)
	}
	_, body = callLANAccess(t, server, http.MethodGet, "", true)
	if body["via_usb"] != true {
		t.Fatalf("USB status: %v", body)
	}
}

// With a guard, only admitted requests reach the handler; without one, the
// handler itself admits USB requests only.
func TestLANAccessChangesNeedAnAdmittedClient(t *testing.T) {
	server, marker := newLANAccessServer(t)
	code, _ := callLANAccess(t, server, http.MethodPut, `{"enabled":true,"password":"correct horse"}`, false)
	if code != http.StatusForbidden {
		t.Fatalf("non-USB update without the guard: %d", code)
	}
	if server.lanAccess.Enabled() || server.lanAccess.PasswordSet() {
		t.Fatal("state changed from a refused request")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("login sync ran for a refused request")
	}
	if code, _ := callClearPassword(t, server, `{}`, false); code != http.StatusForbidden {
		t.Fatalf("non-USB clear without the guard: %d", code)
	}
}

func TestLANAccessSwitchAndPasswordAreIndependent(t *testing.T) {
	server, marker := newLANAccessServer(t)

	code, body := callLANAccess(t, server, http.MethodPut, `{"enabled":true}`, true)
	if code != http.StatusOK || body["enabled"] != true || body["password_set"] != false {
		t.Fatalf("enable without a password: %d %v", code, body)
	}
	code, _ = callLANAccess(t, server, http.MethodPut, `{"password":"12345"}`, true)
	if code != http.StatusBadRequest || server.lanAccess.PasswordSet() {
		t.Fatalf("five-character password: %d", code)
	}
	code, body = callLANAccess(t, server, http.MethodPut, `{"password":"luckfox"}`, true)
	if code != http.StatusOK || body["enabled"] != true || body["password_set"] != true || body["system_synced"] != true {
		t.Fatalf("set password: %d %v", code, body)
	}
	code, body = callLANAccess(t, server, http.MethodPut, `{"enabled":false}`, true)
	if code != http.StatusOK || body["enabled"] != false || body["password_set"] != true {
		t.Fatalf("disable: %d %v", code, body)
	}
	// Every change is applied to SSH, not only password changes.
	runs, _ := os.ReadFile(marker)
	if got := bytes.Count(runs, []byte("run")); got != 3 {
		t.Fatalf("system sync ran %d times; want once per change", got)
	}
}

// Clearing the password needs the current one from every client, USB
// included, and leaves LAN access alone.
func TestClearPasswordNeedsCurrentPasswordAndKeepsSwitch(t *testing.T) {
	server, marker := newLANAccessServer(t)
	if code, _ := callLANAccess(t, server, http.MethodPut, `{"enabled":true,"password":"luckfox"}`, true); code != http.StatusOK {
		t.Fatalf("setup: %d", code)
	}
	_ = os.Remove(marker)
	for _, body := range []string{`{}`, `{"current_password":"wrong"}`} {
		if code, _ := callClearPassword(t, server, body, true); code != http.StatusForbidden || !server.lanAccess.PasswordSet() {
			t.Fatalf("clear with %s: %d", body, code)
		}
	}
	code, body := callClearPassword(t, server, `{"current_password":"luckfox"}`, true)
	if code != http.StatusOK || body["enabled"] != true || body["password_set"] != false || body["system_synced"] != true {
		t.Fatalf("clear: %d %v", code, body)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("clearing was not applied to SSH and the login account")
	}
}

// Wrong current passwords count against the same lockout as sign-in, so a
// session cannot be used to guess the password.
func TestCurrentPasswordGuessesAreThrottled(t *testing.T) {
	server, _ := newLANAccessServer(t)
	if code, _ := callLANAccess(t, server, http.MethodPut, `{"password":"luckfox"}`, true); code != http.StatusOK {
		t.Fatalf("setup: %d", code)
	}
	code := 0
	for i := 0; i < 10 && code != http.StatusTooManyRequests; i++ {
		code, _ = callClearPassword(t, server, `{"current_password":"wrong"}`, true)
	}
	if code != http.StatusTooManyRequests {
		t.Fatalf("repeated wrong current passwords were never throttled: %d", code)
	}
	if code, _ := callClearPassword(t, server, `{"current_password":"luckfox"}`, true); code != http.StatusTooManyRequests || !server.lanAccess.PasswordSet() {
		t.Fatalf("the right password must also wait out the lockout: %d", code)
	}
}

func callClearPassword(t *testing.T, server *Server, body string, usb bool) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/security/password", bytes.NewBufferString(body))
	req.RemoteAddr = "127.0.0.2:1234"
	req = req.WithContext(context.WithValue(req.Context(), usbIngressContextKey{}, usb))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	decoded := map[string]any{}
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded
}

func TestLANAccessReportsLoginSyncFailure(t *testing.T) {
	server, _ := newLANAccessServer(t)
	server.options.LANAccessSync = "/bin/false"
	code, body := callLANAccess(t, server, http.MethodPut, `{"password":"correct horse"}`, true)
	if code != http.StatusOK || body["system_synced"] != false || body["password_set"] != true {
		t.Fatalf("sync failure: %d %v", code, body)
	}
}

func newGuardedServer(t *testing.T) *Server {
	t.Helper()
	options := testOptions(t)
	root := t.TempDir()
	options.LANGuard = true
	options.USBAddress = "192.168.42.1"
	options.USBSubnet = "192.168.42.0/24"
	options.LANAccessPath = filepath.Join(root, "lan-access-enabled")
	options.DevicePasswordPath = filepath.Join(root, "device-password")
	options.SessionSecretPath = filepath.Join(root, "lan-session-secret")
	options.LANAccessSync = ""
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func guardedCall(server *Server, method, path, body, bearer string, usb bool, headers map[string]string) *httptest.ResponseRecorder {
	host, remote := "192.168.50.76", "192.168.50.20:5000"
	if usb {
		host, remote = "192.168.42.1", "192.168.42.100:5000"
	}
	req := httptest.NewRequest(method, "http://"+host+path, bytes.NewBufferString(body))
	req.RemoteAddr = remote
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	ctx := context.WithValue(lanaccessContext(req, usb), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(host), Port: 80})
	response := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(response, req.WithContext(ctx))
	return response
}

func TestLANGuardGatesConfigWeb(t *testing.T) {
	server := newGuardedServer(t)
	const api = "/api/security/lan-access"
	if response := guardedCall(server, http.MethodGet, api, "", "", false, nil); response.Code != http.StatusForbidden {
		t.Fatalf("LAN request while off: %d", response.Code)
	}
	if response := guardedCall(server, http.MethodGet, api, "", "", false, map[string]string{"Accept": "text/html"}); response.Code != http.StatusForbidden || !bytes.Contains(response.Body.Bytes(), []byte("<html")) {
		t.Fatalf("browser request while off: %d %q", response.Code, response.Body.String())
	}
	// No password: USB clients need no credentials and can set one.
	if response := guardedCall(server, http.MethodPut, api, `{"password":"luckfox","enabled":true}`, "", true, nil); response.Code != http.StatusOK {
		t.Fatalf("USB update without a password: %d %s", response.Code, response.Body.String())
	}
	// With a password, USB clients need a session too; the app pairs.
	if response := guardedCall(server, http.MethodGet, api, "", "", true, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("USB request without a session: %d", response.Code)
	}
	if response := guardedCall(server, http.MethodGet, api, "", "", false, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("LAN request without a session: %d", response.Code)
	}
	tokenFrom := func(response *httptest.ResponseRecorder) string {
		var body struct {
			Token string `json:"token"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Token == "" {
			t.Fatalf("no token: %d %s", response.Code, response.Body.String())
		}
		return body.Token
	}
	paired := tokenFrom(guardedCall(server, http.MethodPost, "/api/auth/pair", "", "", true, nil))
	signedIn := tokenFrom(guardedCall(server, http.MethodPost, "/api/auth/login", `{"password":"luckfox"}`, "", false, nil))

	// The paired app reaches the settings over the LAN as well.
	if response := guardedCall(server, http.MethodGet, api, "", paired, false, nil); response.Code != http.StatusOK {
		t.Fatalf("paired token over the LAN: %d", response.Code)
	}
	// Replacing the password needs the current one, from the paired app too.
	for name, bearer := range map[string]string{"LAN": signedIn, "USB": paired} {
		if response := guardedCall(server, http.MethodPut, api, `{"password":"another"}`, bearer, name == "USB", nil); response.Code != http.StatusForbidden {
			t.Fatalf("%s password change without the current password: %d", name, response.Code)
		}
	}
	if !server.lanAccess.CheckPassword("luckfox") {
		t.Fatal("password changed without the current one")
	}
	if response := guardedCall(server, http.MethodPut, api, `{"password":"another","current_password":"luckfox"}`, signedIn, false, nil); response.Code != http.StatusOK {
		t.Fatalf("LAN password change with the current password: %d %s", response.Code, response.Body.String())
	}
	// That signed everyone out, the app included; it pairs again.
	if response := guardedCall(server, http.MethodGet, api, "", paired, true, nil); response.Code != http.StatusUnauthorized {
		t.Fatalf("paired token after a password change: %d", response.Code)
	}
	paired = tokenFrom(guardedCall(server, http.MethodPost, "/api/auth/pair", "", "", true, nil))
	if response := guardedCall(server, http.MethodDelete, "/api/security/password", `{"current_password":"another"}`, paired, true, nil); response.Code != http.StatusOK {
		t.Fatalf("clear from the paired app: %d %s", response.Code, response.Body.String())
	}
	if server.lanAccess.PasswordSet() || !server.lanAccess.Enabled() {
		t.Fatal("clearing must remove the password and keep LAN access on")
	}
	if response := guardedCall(server, http.MethodGet, api, "", "", false, nil); response.Code != http.StatusOK {
		t.Fatalf("LAN request after clearing: %d", response.Code)
	}
}

// lanaccessContext reproduces what the listeners' ConnContext records.
func lanaccessContext(req *http.Request, usb bool) context.Context {
	return configHTTPServer("", nil, usb).ConnContext(req.Context(), nil)
}

func TestLANAccessSyncSurvivesClientDisconnect(t *testing.T) {
	server, marker := newLANAccessServer(t)
	script := filepath.Join(t.TempDir(), "slow-sync")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.3\necho run >>'"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	server.options.LANAccessSync = script
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone when the helper starts
	if err := server.syncLANAccess(ctx); err != nil {
		t.Fatalf("sync after client disconnect: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("helper was stopped by the caller's cancellation")
	}
}

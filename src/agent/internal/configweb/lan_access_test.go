package configweb

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestLANAccessCanOnlyChangeOverUSB(t *testing.T) {
	server, marker := newLANAccessServer(t)
	code, _ := callLANAccess(t, server, http.MethodPut, `{"enabled":true,"password":"correct horse"}`, false)
	if code != http.StatusForbidden {
		t.Fatalf("non-USB update: %d", code)
	}
	if server.lanAccess.Enabled() || server.lanAccess.PasswordSet() {
		t.Fatal("state changed from a non-USB request")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("login sync ran for a refused request")
	}
}

func TestLANAccessEnableFlow(t *testing.T) {
	server, marker := newLANAccessServer(t)

	code, _ := callLANAccess(t, server, http.MethodPut, `{"enabled":true}`, true)
	if code != http.StatusBadRequest || server.lanAccess.Enabled() {
		t.Fatalf("enable without password: %d enabled=%v", code, server.lanAccess.Enabled())
	}
	code, _ = callLANAccess(t, server, http.MethodPut, `{"enabled":true,"password":"short"}`, true)
	if code != http.StatusBadRequest || server.lanAccess.Enabled() || server.lanAccess.PasswordSet() {
		t.Fatalf("short password: %d", code)
	}

	code, body := callLANAccess(t, server, http.MethodPut, `{"enabled":true,"password":"correct horse"}`, true)
	if code != http.StatusOK || body["enabled"] != true || body["password_set"] != true || body["system_synced"] != true {
		t.Fatalf("enable with password: %d %v", code, body)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("login sync helper did not run")
	}

	code, body = callLANAccess(t, server, http.MethodPut, `{"enabled":false}`, true)
	if code != http.StatusOK || body["enabled"] != false || body["password_set"] != true {
		t.Fatalf("disable: %d %v", code, body)
	}
	// The kept password lets the switch come back on without asking again.
	code, body = callLANAccess(t, server, http.MethodPut, `{"enabled":true}`, true)
	if code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("re-enable: %d %v", code, body)
	}
	// Every change is applied to SSH, not only password changes.
	runs, _ := os.ReadFile(marker)
	if got := bytes.Count(runs, []byte("run")); got != 3 {
		t.Fatalf("system sync ran %d times; want once per change", got)
	}
}

func TestLANAccessReportsLoginSyncFailure(t *testing.T) {
	server, _ := newLANAccessServer(t)
	server.options.LANAccessSync = "/bin/false"
	code, body := callLANAccess(t, server, http.MethodPut, `{"password":"correct horse"}`, true)
	if code != http.StatusOK || body["system_synced"] != false || body["password_set"] != true {
		t.Fatalf("sync failure: %d %v", code, body)
	}
}

func TestLANGuardGatesConfigWebOnLAN(t *testing.T) {
	options := testOptions(t)
	root := t.TempDir()
	options.LANGuard = true
	options.LANAccessPath = filepath.Join(root, "lan-access-enabled")
	options.DevicePasswordPath = filepath.Join(root, "device-password")
	options.LANAccessSync = ""
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	get := func(usb bool, accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://192.168.50.76/api/security/lan-access", nil)
		req.RemoteAddr = "192.168.50.20:5000"
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		response := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(response, req.WithContext(lanaccessContext(req, usb)))
		return response
	}
	if response := get(false, ""); response.Code != http.StatusForbidden {
		t.Fatalf("LAN request while off: %d", response.Code)
	}
	if response := get(false, "text/html"); response.Code != http.StatusForbidden || !bytes.Contains(response.Body.Bytes(), []byte("<html")) {
		t.Fatalf("browser request while off: %d %q", response.Code, response.Body.String())
	}
	if response := get(true, ""); response.Code != http.StatusOK {
		t.Fatalf("USB request while off: %d", response.Code)
	}
	if err := server.lanAccess.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := server.lanAccess.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if response := get(false, ""); response.Code != http.StatusOK {
		t.Fatalf("LAN request while on: %d", response.Code)
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

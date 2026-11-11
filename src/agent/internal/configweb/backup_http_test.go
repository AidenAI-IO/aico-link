package configweb

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aico-agent/internal/lanaccess"
)

func TestMaintenanceSessionRejectsNonUSBIngress(t *testing.T) {
	server := newRestoreTestServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/sessions", nil)
	req.RemoteAddr = "127.0.0.2:1234"
	req.Header.Set("X-Aico-Client", "config-web/1.0")
	req = req.WithContext(context.WithValue(req.Context(), usbIngressContextKey{}, false))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHTTPMaintenanceCookieAcceptsUUIDv4Fallback(t *testing.T) {
	server := newRestoreTestServer(t, t.TempDir())
	if err := os.WriteFile(server.options.HardwareIDPath, []byte("board-http-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/maintenance/sessions", nil)
	req.RemoteAddr = "127.0.0.2:1234"
	req.Header.Set("X-Aico-Client", "config-web/1.0")
	session := httptest.NewRecorder()
	server.ServeHTTP(session, req)
	if session.Code != http.StatusOK {
		t.Fatalf("session=%d %s", session.Code, session.Body.String())
	}
	csrf, _ := decodeJSON(t, session)["csrf_token"].(string)
	cookies := session.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("maintenance cookie missing")
	}
	for _, id := range []string{"1710000000000-abcd", "00000000-0000-4000-8000-000000000001"} {
		req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/backup/jobs", bytes.NewBufferString(`{"format_version":1,"protection":{"mode":"none"}}`))
		req.RemoteAddr = "127.0.0.2:1234"
		req.AddCookie(cookies[0])
		req.Header.Set("X-Aico-Request-ID", id)
		req.Header.Set("X-Aico-CSRF-Token", csrf)
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		want := http.StatusAccepted
		if id == "1710000000000-abcd" {
			want = http.StatusUnauthorized
		}
		if resp.Code != want {
			t.Fatalf("id=%s status=%d want=%d body=%s", id, resp.Code, want, resp.Body.String())
		}
	}
	server.backupJobs.cancelAll()
}

// A LAN client that the LAN guard admitted with a device-password session may
// open a maintenance session; the same LAN request without one may not.
func TestMaintenanceSessionAdmitsSignedInLANClient(t *testing.T) {
	server := newRestoreTestServer(t, t.TempDir())
	if err := os.WriteFile(server.options.HardwareIDPath, []byte("board-lan-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://192.168.50.76/api/maintenance/sessions", nil)
		req.RemoteAddr = "192.168.50.20:5000"
		req.Header.Set("X-Aico-Client", "config-web/1.0")
		return req.WithContext(context.WithValue(req.Context(), usbIngressContextKey{}, false))
	}

	response := httptest.NewRecorder()
	server.ServeHTTP(response, newRequest())
	if response.Code != http.StatusForbidden {
		t.Fatalf("LAN without a session: status=%d body=%s", response.Code, response.Body.String())
	}

	state := lanaccess.NewState(
		filepath.Join(t.TempDir(), "enabled"),
		filepath.Join(t.TempDir(), "password"),
		filepath.Join(t.TempDir(), "secret"),
	)
	if err := state.SetPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := newRequest()
	req.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	lanaccess.Guard(state, server).ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("signed-in LAN client: status=%d body=%s", response.Code, response.Body.String())
	}
}

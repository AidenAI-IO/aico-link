package configweb

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
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

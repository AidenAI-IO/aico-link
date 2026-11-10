package configweb

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/usbuplink"
)

func TestUSBInternetIngressToggleAndHostSessions(t *testing.T) {
	manager, err := usbuplink.New(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	control := httptest.NewServer(manager)
	defer control.Close()
	options := testOptions(t)
	options.USBSubnet = "192.168.42.0/24"
	options.USBAddress = "192.168.42.1"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	server.usbInternetClient = &http.Client{Transport: rewriteUSBControl{target: control.URL}}
	clockChanges := 0
	server.usbInternetSetTime = func(context.Context, time.Time) error { clockChanges++; return nil }
	call := func(method, path, body, peer string, usb bool, origin string) (int, map[string]any) {
		r := httptest.NewRequest(method, "http://192.168.42.1"+path, bytes.NewBufferString(body))
		r.RemoteAddr = peer
		r.Header.Set("Origin", origin)
		r = r.WithContext(context.WithValue(r.Context(), usbIngressContextKey{}, usb))
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		value := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &value)
		return w.Code, value
	}
	path := usbuplink.APIPath
	if code, state := call("GET", path, "", "192.168.42.100:123", true, ""); code != 200 || state["enabled"] != false {
		t.Fatal(code, state)
	}
	for _, client := range []struct {
		peer   string
		usb    bool
		origin string
	}{{"10.0.0.5:123", false, ""}, {"192.168.42.100:123", false, ""}, {"192.168.42.100:123", true, "http://evil.example"}} {
		if code, _ := call("PUT", path, `{"enabled":true}`, client.peer, client.usb, client.origin); code != 403 {
			t.Fatal("unauthorized toggle", code)
		}
	}
	request := `{"session_id":"` + strings.Repeat("a", 32) + `","host":"android","peer":"10.0.0.5","password":"` + strings.Repeat("b", 32) + `","unix_time_ms":1704067200000}`
	if code, _ := call("PUT", path+"/session", request, "192.168.42.100:123", true, ""); code != 403 || clockChanges != 0 {
		t.Fatal("disabled session or clock accepted", code)
	}
	if code, _ := call("PUT", path, `{"enabled":true}`, "192.168.42.100:123", true, ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := call("PUT", path+"/session", request, "192.168.42.100:123", true, ""); code != 200 {
		t.Fatal(code)
	}
	if manager.Current().Peer != "192.168.42.100" || clockChanges != 1 {
		t.Fatal("caller selected peer or clock not synchronized")
	}
	if code, _ := call("PUT", path, `{"enabled":false}`, "192.168.42.100:123", true, ""); code != 200 || manager.Current() != nil {
		t.Fatal("disable did not revoke", code)
	}
	call("PUT", path, `{"enabled":true}`, "192.168.42.100:123", true, "")
	mac := `{"session_id":"` + strings.Repeat("c", 32) + `","host":"mac"}`
	if code, _ := call("PUT", path+"/session", mac, "192.168.42.100:123", true, ""); code != 403 {
		t.Fatal("remote registered loopback proxy", code)
	}
	if code, _ := call("PUT", path+"/session", mac, "127.0.0.1:123", false, ""); code != 200 {
		t.Fatal("SSH session refused", code)
	}
}

type rewriteUSBControl struct{ target string }

func (r rewriteUSBControl) RoundTrip(request *http.Request) (*http.Response, error) {
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, r.target+request.URL.Path, request.Body)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(forward)
}

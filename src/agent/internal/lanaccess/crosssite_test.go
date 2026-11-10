package lanaccess

import (
	"net/http"
	"testing"
	"time"
)

func TestGuardRefusesCrossSiteBrowserRequestsOverUSB(t *testing.T) {
	h := guarded(newState(t))
	usb := func(host string, headers map[string]string) *http.Request {
		r := newRequest("192.168.42.100:5000", "192.168.42.1", true)
		r.Method = http.MethodPost
		r.Host = host
		for key, value := range headers {
			r.Header.Set(key, value)
		}
		return r
	}
	get := func(headers map[string]string) *http.Request {
		r := usb("192.168.42.1:8080", headers)
		r.Method = http.MethodGet
		return r
	}
	for name, tc := range map[string]struct {
		r    *http.Request
		want int
	}{
		"native app":           {usb("192.168.42.1:8080", nil), http.StatusNoContent},
		"same-origin page":     {usb("192.168.42.1:8080", map[string]string{"Origin": "http://192.168.42.1:8080", "Sec-Fetch-Site": "same-origin"}), http.StatusNoContent},
		"localhost page":       {usb("localhost:8080", map[string]string{"Origin": "http://localhost:8080"}), http.StatusNoContent},
		"cross-site page":      {usb("192.168.42.1:8080", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		"cross-site, no fetch": {usb("192.168.42.1:8080", map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		"marker only":          {usb("192.168.42.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		"sandboxed frame":      {usb("192.168.42.1:8080", map[string]string{"Origin": "null"}), http.StatusForbidden},
		"other port":           {usb("192.168.42.1:8080", map[string]string{"Origin": "http://192.168.42.1"}), http.StatusForbidden},
		"DNS rebinding":        {usb("rebind.evil.example:8080", map[string]string{"Origin": "http://rebind.evil.example:8080", "Sec-Fetch-Site": "same-origin"}), http.StatusForbidden},
		"rebinding, no Origin": {usb("rebind.evil.example:8080", nil), http.StatusForbidden},
		"link followed":        {get(map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}), http.StatusNoContent},
		"framed":               {get(map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}), http.StatusForbidden},
		"cross-site GET fetch": {get(map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "empty"}), http.StatusForbidden},
		"POST as navigation":   {usb("192.168.42.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}), http.StatusForbidden},
	} {
		if code := serve(h, tc.r); code != tc.want {
			t.Errorf("%s: %d, want %d", name, code, tc.want)
		}
	}
}

func TestGuardRefusesCrossSiteBeforeSessionCheck(t *testing.T) {
	state := enabledState(t)
	token, _, err := state.IssueToken(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := lanRequest(http.MethodPost, "/api/chat", "")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Origin", "https://evil.example")
	if code := serve(guarded(state), r); code != http.StatusForbidden {
		t.Fatalf("cross-site request with a session: %d", code)
	}
}

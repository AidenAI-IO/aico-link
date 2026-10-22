package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aiden-agent/internal/mcu"
)

func TestMCUHTTPRoutesAndRestriction(t *testing.T) {
	var captured mcu.Request
	calls := 0
	s := &Server{mcuRequest: func(ctx context.Context, r mcu.Request) (mcu.Response, error) {
		calls++
		captured = r
		return mcu.Response{Status: "OK"}, nil
	}}
	for _, tc := range []struct{ method, path, body, op string }{{"GET", "/api/mcu/state", "", "get_state"}, {"GET", "/api/mcu/events?since=3&generation=g1&wait_ms=10", "", "events_since"}, {"PUT", "/api/mcu/wakeup-timer", `{"delay_s":10,"generation":"g1","request_id":"a"}`, "set_wakeup_timer"}, {"DELETE", "/api/mcu/wakeup-timer", `{"generation":"g1","request_id":"b"}`, "cancel_wakeup_timer"}, {"PUT", "/api/mcu/leds/white", `{"mode":"auto","generation":"g1","request_id":"c"}`, "set_led"}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.handleMCU(w, r)
		if w.Code != 200 || captured.Op != tc.op {
			t.Fatalf("%s: %d %s %+v", tc.path, w.Code, w.Body.String(), captured)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable hardware state")
		}
	}
	before := calls
	r := httptest.NewRequest("GET", "/api/mcu/state", nil)
	r.RemoteAddr = "192.168.50.10:1234"
	w := httptest.NewRecorder()
	s.handleMCU(w, r)
	if w.Code != 403 || calls != before {
		t.Fatal("LAN request accepted")
	}
}

// Exercise the real UDS client: a service can receive a complete mutation and
// disconnect (or send a corrupt reply) after the hardware has already acted.
func TestMCUHTTPLostResponsePreservesUnknownOutcome(t *testing.T) {
	operations := []struct {
		method, path, body, op string
		mutating               bool
	}{
		{"PUT", "/api/mcu/wakeup-timer", `{"delay_s":120,"generation":"g","request_id":"set"}`, "set_wakeup_timer", true},
		{"DELETE", "/api/mcu/wakeup-timer", `{"generation":"g","request_id":"cancel"}`, "cancel_wakeup_timer", true},
		{"PUT", "/api/mcu/leds/white", `{"mode":"on","lease_s":10,"generation":"g","request_id":"led"}`, "set_led", true},
		{"GET", "/api/mcu/battery", "", "get_battery", false},
	}
	failures := []struct {
		name, payload, status string
		code                  int
	}{
		{"eof", "", "SERVICE_UNAVAILABLE", 503},
		{"invalid_envelope", "", "SERVICE_UNAVAILABLE", 503},
		{"invalid_json", "{", "PROTOCOL_ERROR", 502},
		{"missing_status", "{}", "PROTOCOL_ERROR", 502},
		{"timeout", "", "TIMEOUT", 504},
	}
	for _, op := range operations {
		for _, failure := range failures {
			t.Run(op.op+"/"+failure.name, func(t *testing.T) {
				dir, err := os.MkdirTemp("/tmp", "mcu-http-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				path := filepath.Join(dir, "mcu.sock")
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				accepted := make(chan mcu.Request, 1)
				go func() {
					var received mcu.Request
					defer func() { accepted <- received }()
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					var prefix [12]byte
					if _, err := io.ReadFull(conn, prefix[:]); err != nil {
						return
					}
					size := binary.LittleEndian.Uint32(prefix[:4])
					if size == 0 || size > 16*1024 {
						return
					}
					data := make([]byte, size)
					if _, err := io.ReadFull(conn, data); err != nil {
						return
					}
					if err := json.Unmarshal(data, &received); err != nil {
						return
					}
					switch failure.name {
					case "eof":
						return
					case "timeout":
						_, _ = conn.Read(prefix[:]) // Client deadline closes the socket.
					case "invalid_envelope":
						_, _ = conn.Write(make([]byte, 12))
					default:
						wire := make([]byte, 12+len(failure.payload))
						binary.LittleEndian.PutUint32(wire, uint32(len(failure.payload)))
						copy(wire[12:], failure.payload)
						_, _ = conn.Write(wire)
					}
				}()
				t.Setenv("AIDEN_MCU_SERVICE_SOCKET", path)
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				request := httptest.NewRequest(op.method, op.path, strings.NewReader(op.body)).WithContext(ctx)
				request.RemoteAddr = "127.0.0.1:1234"
				recorder := httptest.NewRecorder()
				(&Server{}).handleMCU(recorder, request)
				if received := <-accepted; received.Op != op.op {
					t.Fatalf("complete request was not received: %+v", received)
				}
				var response mcu.Response
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if recorder.Code != failure.code || response.Status != failure.status || response.UnknownOutcome != op.mutating {
					t.Fatalf("HTTP %d: %+v", recorder.Code, response)
				}
			})
		}
	}
}

func TestMCUHTTPConnectionFailureDoesNotClaimUnknownOutcome(t *testing.T) {
	t.Setenv("AIDEN_MCU_SERVICE_SOCKET", filepath.Join(t.TempDir(), "missing.sock"))
	request := httptest.NewRequest("PUT", "/api/mcu/wakeup-timer", strings.NewReader(`{"delay_s":120,"generation":"g","request_id":"set"}`))
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	(&Server{}).handleMCU(recorder, request)
	var response mcu.Response
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != 503 || response.Status != "SERVICE_UNAVAILABLE" || response.UnknownOutcome {
		t.Fatalf("HTTP %d: %+v", recorder.Code, response)
	}
}
func TestMCUHTTPRejectsInvalidAndMapsErrors(t *testing.T) {
	s := &Server{mcuRequest: func(context.Context, mcu.Request) (mcu.Response, error) {
		return mcu.Response{}, &mcu.APIError{Status: "UNSUPPORTED", Message: "old MCU"}
	}}
	for _, tc := range []struct {
		method, path, body string
		code               int
	}{{"GET", "/api/mcu/leds/red", "", 501}, {"GET", "/api/mcu/events?wait_ms=26000", "", 400}, {"POST", "/api/mcu/state", "", 405}, {"PUT", "/api/mcu/leds/white", `{"op":"set_wakeup_timer"}`, 400}, {"PUT", "/api/mcu/wakeup-timer", `{"delay_s":1,"unknown":true}`, 400}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.handleMCU(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
}

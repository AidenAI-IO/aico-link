package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/ble"
)

func TestBluetoothHTTPStatus(t *testing.T) {
	server := &Server{
		bleStatusRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			return ble.RuntimeStatus{DeviceName: "Aico-1234", BackendAvailable: true}, nil
		},
	}
	recorder := httptest.NewRecorder()
	server.handleBluetoothStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/bluetooth/status", nil))
	if recorder.Code != http.StatusOK || !containsAll(recorder.Body.String(), `"ok":true`, `"device_name":"Aico-1234"`) {
		t.Fatalf("status code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBluetoothHTTPPairingActions(t *testing.T) {
	server := &Server{
		blePairingStartRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			return ble.RuntimeStatus{PairingOpen: true}, nil
		},
	}

	recorder := httptest.NewRecorder()
	server.handleBluetoothPairingStart(recorder, bluetoothControlRequest(http.MethodPost))
	if recorder.Code != http.StatusOK || !containsAll(recorder.Body.String(), `"pairing_open":true`) {
		t.Fatalf("start code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBluetoothHTTPDisconnectsPhysicalLink(t *testing.T) {
	server := &Server{
		bleDisconnectRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			return ble.RuntimeStatus{Paired: true, Connected: false, ANCSSubscribed: false}, nil
		},
	}

	recorder := httptest.NewRecorder()
	server.handleBluetoothDisconnect(recorder, bluetoothControlRequestForPath(
		http.MethodPost,
		"/api/bluetooth/disconnect",
	))
	if recorder.Code != http.StatusOK || !containsAll(
		recorder.Body.String(),
		`"paired":true`,
		`"connected":false`,
		`"ancs_subscribed":false`,
	) {
		t.Fatalf("disconnect code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBluetoothUSBReenumerationWakeIsLoopbackOnly(t *testing.T) {
	wakeReasons := []string{}
	server := &Server{
		bleWakeRequest: func(_ context.Context, reason string) error {
			wakeReasons = append(wakeReasons, reason)
			return nil
		},
	}

	allowed := httptest.NewRequest(http.MethodPost, "/api/bluetooth/wake/usb-reenumeration", nil)
	allowed.RemoteAddr = "127.0.0.1:12345"
	allowedRecorder := httptest.NewRecorder()
	server.handleBluetoothUSBReenumerationWake(allowedRecorder, allowed)
	if allowedRecorder.Code != http.StatusOK {
		t.Fatalf("loopback status = %d body=%s", allowedRecorder.Code, allowedRecorder.Body.String())
	}
	if len(wakeReasons) != 1 || wakeReasons[0] != "usb_reenumeration" {
		t.Fatalf("wake reasons = %v, want [usb_reenumeration]", wakeReasons)
	}

	denied := httptest.NewRequest(http.MethodPost, "/api/bluetooth/wake/usb-reenumeration", nil)
	denied.RemoteAddr = "192.168.42.2:12345"
	deniedRecorder := httptest.NewRecorder()
	server.handleBluetoothUSBReenumerationWake(deniedRecorder, denied)
	if deniedRecorder.Code != http.StatusForbidden {
		t.Fatalf("USB client status = %d body=%s", deniedRecorder.Code, deniedRecorder.Body.String())
	}
	if len(wakeReasons) != 1 {
		t.Fatalf("denied request emitted wake reasons: %v", wakeReasons)
	}
}

func TestBluetoothHTTPResetsStaleBond(t *testing.T) {
	server := &Server{
		blePairingForgetRequest: func(context.Context, string) (ble.ForgetResult, error) {
			return ble.ForgetResult{
				Removed:   1,
				Bluetooth: ble.RuntimeStatus{Paired: false, Connected: false},
			}, nil
		},
	}

	recorder := httptest.NewRecorder()
	server.handleBluetoothPairingReset(recorder, bluetoothControlRequestForPath(
		http.MethodPost,
		"/api/bluetooth/pairing/reset",
	))
	if recorder.Code != http.StatusOK || !containsAll(
		recorder.Body.String(),
		`"removed":1`,
		`"paired":false`,
		`"connected":false`,
	) {
		t.Fatalf("pairing reset code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPhoneNotificationEventsPublishesAndroidBatch(t *testing.T) {
	var gotPhoneID string
	var gotEvents []ble.NotificationEvent
	server := &Server{
		bleNotifyRequest: func(
			_ context.Context,
			_ string,
			phoneID string,
			events []ble.NotificationEvent,
		) (ble.NotificationPublishResult, error) {
			gotPhoneID = phoneID
			gotEvents = events
			return ble.NotificationPublishResult{Accepted: 1, LastID: "17"}, nil
		},
	}
	body := []byte(`{"phone_id":"android-1","events":[{"source_id":"key","source_event_id":"event-1","event":"added","app_identifier":"com.example"}]}`)
	request := bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	recorder := httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusOK || !containsAll(
		recorder.Body.String(),
		`"ok":true`,
		`"accepted":1`,
		`"last_id":"17"`,
	) {
		t.Fatalf("publish code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if gotPhoneID != "android-1" || len(gotEvents) != 1 || gotEvents[0].SourceEventID != "event-1" {
		t.Fatalf("unexpected publish request phone=%q events=%#v", gotPhoneID, gotEvents)
	}
}

func TestPhoneNotificationEventsNotifiesSharedMemoryWorker(t *testing.T) {
	worker := newMemoryWorkerWithProcessors(nil, time.Hour)
	defer worker.Stop()
	plane := &FilesystemMemoryPlane{memoryWorker: worker}
	server := &Server{
		runtime: &Runtime{memoryPlane: plane},
		bleNotifyRequest: func(context.Context, string, string, []ble.NotificationEvent) (ble.NotificationPublishResult, error) {
			return ble.NotificationPublishResult{Accepted: 1, LastID: "1"}, nil
		},
	}
	body := `{"phone_id":"android-1","events":[{"source_id":"key","source_event_id":"event-1","event":"added","app_identifier":"com.example"}]}`
	request := bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("publish code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !worker.pending || worker.timer == nil {
		t.Fatal("accepted notification did not wake the shared MemoryWorker idle timer")
	}
}

func TestPhoneNotificationEventsRejectsInvalidAndNonUSBRequests(t *testing.T) {
	called := false
	server := &Server{
		bleNotifyRequest: func(
			context.Context,
			string,
			string,
			[]ble.NotificationEvent,
		) (ble.NotificationPublishResult, error) {
			called = true
			return ble.NotificationPublishResult{}, nil
		},
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/api/phone-notifications/events",
		strings.NewReader(`{"phone_id":"android-1","events":[]}`),
	)
	request.RemoteAddr = "192.168.50.140:12345"
	recorder := httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("external publish code=%d called=%v", recorder.Code, called)
	}

	request = bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(strings.NewReader(`{"phone_id":`))
	recorder = httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusBadRequest || called {
		t.Fatalf("invalid publish code=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestPhoneNotificationEventsAcceptsMaximumValidatedBatch(t *testing.T) {
	requestPayload := maximumPhoneNotificationRequest()
	body, err := json.Marshal(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 64*1024 {
		t.Fatalf("maximum notification batch did not exercise the old limit: %d", len(body))
	}
	if len(body) > maxPhoneNotificationRequestBytes {
		t.Fatalf("maximum notification batch exceeds HTTP limit: %d", len(body))
	}
	body = append(body, bytes.Repeat([]byte(" "), maxPhoneNotificationRequestBytes-len(body))...)
	if len(body) != maxPhoneNotificationRequestBytes {
		t.Fatalf("boundary notification request length=%d", len(body))
	}

	socketDir, err := os.MkdirTemp("/tmp", "aico-ble-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Errorf("remove BLE socket directory: %v", err)
		}
	})
	socketPath := filepath.Join(socketDir, "ble.sock")
	udsServer := ble.NewUDSServer(socketPath, ble.NewService(16))
	if err := udsServer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := udsServer.Close(); err != nil {
			t.Errorf("close BLE UDS server: %v", err)
		}
	})
	server := &Server{bleSocketPath: socketPath}
	request := bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusOK || !containsAll(recorder.Body.String(), `"accepted":8`) {
		t.Fatalf("maximum publish code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPhoneNotificationEventsRejectsUDSOversizedEncoding(t *testing.T) {
	prefix := `{"phone_id":"`
	suffix := `","events":[{}]}`
	body := prefix + strings.Repeat("&", maxPhoneNotificationRequestBytes-len(prefix)-len(suffix)) + suffix
	if len(body) != maxPhoneNotificationRequestBytes {
		t.Fatalf("boundary request length=%d", len(body))
	}

	called := false
	server := &Server{
		bleNotifyRequest: func(
			context.Context,
			string,
			string,
			[]ble.NotificationEvent,
		) (ble.NotificationPublishResult, error) {
			called = true
			return ble.NotificationPublishResult{}, nil
		},
	}
	request := bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(strings.NewReader(body))
	recorder := httptest.NewRecorder()
	server.handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusBadRequest || called || !strings.Contains(recorder.Body.String(), "exceeds BLE UDS frame limit") {
		t.Fatalf("UDS-oversized publish code=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestPhoneNotificationEventsRejectsOversizedBody(t *testing.T) {
	body := `{"phone_id":"` + strings.Repeat("x", maxPhoneNotificationRequestBytes+1) + `","events":[]}`
	request := bluetoothControlRequestForPath(http.MethodPost, "/api/phone-notifications/events")
	request.Body = io.NopCloser(strings.NewReader(body))
	recorder := httptest.NewRecorder()
	(&Server{}).handlePhoneNotificationEvents(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized publish code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBluetoothHTTPMapsPairingConflict(t *testing.T) {
	server := &Server{
		blePairingStartRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			return ble.RuntimeStatus{}, &ble.RequestError{Status: "FAILED_PRECONDITION", Message: "already paired"}
		},
	}
	recorder := httptest.NewRecorder()
	server.handleBluetoothPairingStart(recorder, bluetoothControlRequest(http.MethodPost))
	if recorder.Code != http.StatusConflict || !containsAll(recorder.Body.String(), "already paired") {
		t.Fatalf("conflict code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBluetoothHTTPPairingStartRejectsNonUSBRequest(t *testing.T) {
	called := false
	server := &Server{
		blePairingStartRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			called = true
			return ble.RuntimeStatus{}, nil
		},
	}
	request := httptest.NewRequest(http.MethodPost, "/api/bluetooth/pairing/start", nil)
	request.RemoteAddr = "192.168.50.140:12345"
	request = request.WithContext(context.WithValue(
		request.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("192.168.50.10"), Port: 8080},
	))
	recorder := httptest.NewRecorder()
	server.handleBluetoothPairingStart(recorder, request)
	if recorder.Code != http.StatusForbidden || called {
		t.Fatalf("external pairing request code=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestConfiguredBLEServiceSocketPath(t *testing.T) {
	t.Setenv("AICO_BLE_SERVICE_SOCKET", "/tmp/custom-ble.sock")
	if got := configuredBLEServiceSocketPath(); got != "/tmp/custom-ble.sock" {
		t.Fatalf("configuredBLEServiceSocketPath() = %q", got)
	}
}

func TestBluetoothHTTPMethodAndUnavailableErrors(t *testing.T) {
	server := &Server{
		bleStatusRequest: func(context.Context, string) (ble.RuntimeStatus, error) {
			return ble.RuntimeStatus{}, errors.New("socket unavailable")
		},
	}
	recorder := httptest.NewRecorder()
	server.handleBluetoothStatus(recorder, httptest.NewRequest(http.MethodPost, "/api/bluetooth/status", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	server.handleBluetoothStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/bluetooth/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func containsAll(value string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(value, want) {
			return false
		}
	}
	return true
}

func bluetoothControlRequest(method string) *http.Request {
	return bluetoothControlRequestForPath(method, "/api/bluetooth/pairing/start")
}

func bluetoothControlRequestForPath(method, path string) *http.Request {
	request := httptest.NewRequest(method, path, nil)
	request.RemoteAddr = "192.168.42.100:12345"
	return request.WithContext(context.WithValue(
		request.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("192.168.42.1"), Port: 8080},
	))
}

func maximumPhoneNotificationRequest() phoneNotificationRequest {
	fill := func(length int) string { return strings.Repeat(`"`, length) }
	events := make([]ble.NotificationEvent, 8)
	for index := range events {
		flags := make([]string, 16)
		for flagIndex := range flags {
			flags[flagIndex] = fill(64)
		}
		events[index] = ble.NotificationEvent{
			SourceID:      fill(512),
			SourceEventID: fill(127) + string(rune('0'+index)),
			Event:         "added",
			Flags:         flags,
			AppIdentifier: fill(255),
			Title:         fill(512),
			Subtitle:      fill(512),
			Message:       fill(4096),
			Category:      fill(128),
			Date:          fill(64),
		}
	}
	return phoneNotificationRequest{PhoneID: fill(128), Events: events}
}

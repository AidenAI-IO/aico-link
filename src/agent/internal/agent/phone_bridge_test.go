package agent

import (
	"aico-agent/internal/ble"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhoneBridgeUpdateStateDoesNotExposeUnknownValues(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	state := bridge.UpdateState()
	for _, key := range []string{"app_connected", "app_pip_enabled", "app_fgs_enabled"} {
		if got := state[key]; got != "false" {
			t.Errorf("%s = %q, want false", key, got)
		}
	}
	for _, key := range []string{"app_state", "app_platform"} {
		if got := state[key]; got != "" {
			t.Errorf("%s = %q, want empty", key, got)
		}
	}
	for key, value := range state {
		if value == "unknown" {
			t.Errorf("%s exposes unknown value", key)
		}
	}
}

func TestPhoneBridgeUsesConfiguredPlatformUntilAppReportsOne(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	bridge.SetConfiguredPlatform("ios")
	if got := bridge.getStatus().Platform; got != "ios" {
		t.Fatalf("configured platform = %q, want ios", got)
	}

	bridge.mu.Lock()
	bridge.platform = "android"
	bridge.mu.Unlock()
	if got := bridge.getStatus().Platform; got != "android" {
		t.Fatalf("reported platform = %q, want android", got)
	}
}

func TestPhoneBridgeBLECapabilitiesCachesConcurrentStatusRequests(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	var calls atomic.Int32
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		calls.Add(1)
		time.Sleep(10 * time.Millisecond)
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
		}, nil
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := bridge.bleCapabilities(context.Background()); !got.Wake {
				t.Errorf("cached capabilities = %#v, want Wake", got)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent BLE status calls = %d, want 1", got)
	}

	bridge.bleCapabilityMu.Lock()
	bridge.bleCapabilityAt = time.Now().Add(-phoneBridgeBLECacheTTL - time.Millisecond)
	bridge.bleCapabilityMu.Unlock()
	if got := bridge.bleCapabilities(context.Background()); !got.Wake {
		t.Fatalf("refreshed capabilities = %#v, want Wake", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("BLE status calls after expiry = %d, want 2", got)
	}
}

func TestPhoneBridgeUpdateStateKeepsPiPBackgroundModeAcrossAppStateRefreshGaps(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.pipBridgeEnabled = true
	bridge.pipBridgeSeen = true
	bridge.mu.Unlock()
	if got := bridge.UpdateState()["app_pip_enabled"]; got != "true" {
		t.Fatalf("fresh app_pip_enabled = %q, want true", got)
	}

	bridge.mu.Lock()
	bridge.appStateAt = time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second)
	bridge.mu.Unlock()
	if got := bridge.UpdateState()["app_pip_enabled"]; got != "true" {
		t.Fatalf("stale app_pip_enabled = %q, want true while PiP remains enabled", got)
	}

	bridge.mu.Lock()
	bridge.platform = "android"
	bridge.appStateAt = time.Now()
	bridge.fgsBridgeEnabled = true
	bridge.fgsBridgeSeen = true
	bridge.fgsBridgeAt = time.Now()
	bridge.mu.Unlock()
	if got := bridge.UpdateState()["app_fgs_enabled"]; got != "true" {
		t.Fatalf("fresh app_fgs_enabled = %q, want true", got)
	}

	bridge.mu.Lock()
	bridge.fgsBridgeAt = time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second)
	bridge.mu.Unlock()
	if got := bridge.UpdateState()["app_fgs_enabled"]; got != "false" {
		t.Fatalf("stale app_fgs_enabled = %q, want false", got)
	}
}

func TestPhoneBridgeHandlesEnvironmentEvent(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	bridge.mu.Lock()
	bridge.connected = true
	bridge.platform = "ios"
	bridge.pendingCmds["phone_environment"] = make(chan BridgeCommandResponse, 1)
	bridge.mu.Unlock()

	handled := bridge.handleAppEvent(BridgeCommandResponse{
		ID:     "phone_environment",
		Method: "phone_environment",
		Data: json.RawMessage(`{
			"captured_at":"2026-06-01T02:03:05Z",
			"system_name":"iOS",
			"system_version":"18.5",
			"locale":"zh-Hans-CN",
			"screen":{"width_pixels":1200,"height_pixels":2608},
			"system_apps":[
				{"name":"Camera","available":true,"category":"system","availability_source":"builtin"}
			],
			"third_party_apps":[
				{"name":"WeChat","available":true,"category":"third_party","availability_source":"can_open_url"},
				{"name":"Douyin","available":false,"category":"third_party","availability_source":"can_open_url"}
			],
			"available_apps":[
				{"name":"WeChat","available":true},
				{"name":"Douyin","available":false}
			]
		}`),
	})
	if !handled {
		t.Fatal("environment event was not handled")
	}

	status := bridge.getStatus()
	if status.Environment == nil {
		t.Fatal("expected environment in status")
	}
	if status.Environment.Platform != "ios" {
		t.Fatalf("environment platform = %q, want ios", status.Environment.Platform)
	}
	if status.Environment.SystemName != "iOS" {
		t.Fatalf("environment system_name = %q, want iOS", status.Environment.SystemName)
	}
	if !hasPhoneScreenDimensions(status.Environment.Screen) {
		t.Fatalf("live environment screen = %+v, want dimensions without requiring phone_id", status.Environment.Screen)
	}
	if got := len(status.Environment.SystemApps); got != 1 {
		t.Fatalf("system app count = %d, want 1", got)
	}
	if got := len(status.Environment.ThirdPartyApps); got != 2 {
		t.Fatalf("third-party app count = %d, want 2", got)
	}
	if status.EnvironmentUpdatedAt == nil {
		t.Fatal("expected environment_updated_at")
	}

	bridge.mu.Lock()
	_, pendingStillExists := bridge.pendingCmds["phone_environment"]
	bridge.mu.Unlock()
	if !pendingStillExists {
		t.Fatal("environment event should not consume pending command responses")
	}
}

func TestPhoneBridgeHandlesAppStateEvent(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	handled := bridge.handleAppEvent(BridgeCommandResponse{
		ID:     "phone_app_state",
		Method: "phone_app_state",
		Data:   json.RawMessage(`{"app_state":"background","reported_at":"2026-06-01T02:03:06Z","return_entry":"dynamic_island","return_entry_available":true}`),
	})
	if !handled {
		t.Fatal("app state event was not handled")
	}

	status := bridge.getStatus()
	if status.AppState != "background" {
		t.Fatalf("app_state = %q, want background", status.AppState)
	}
	if status.AppStateUpdatedAt == nil {
		t.Fatal("expected app_state_updated_at")
	}
	if got := status.AppStateUpdatedAt.UTC().Format(time.RFC3339); got != "2026-06-01T02:03:06Z" {
		t.Fatalf("app_state_updated_at = %q, want reported_at", got)
	}
	if status.ReturnEntry != "dynamic_island" {
		t.Fatalf("return_entry = %q, want dynamic_island", status.ReturnEntry)
	}
	if status.ReturnEntryAvailable == nil || !*status.ReturnEntryAvailable {
		t.Fatalf("return_entry_available = %#v, want true", status.ReturnEntryAvailable)
	}
}

func TestBridgeCommandResponseJSONRoundTripStructuredError(t *testing.T) {
	orig := BridgeCommandResponse{
		ID:    "cmd-1",
		Error: NewToolError(CodeBridgeTimeout, "command timeout"),
	}
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var got BridgeCommandResponse
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || got.Error.Code != CodeBridgeTimeout {
		t.Errorf("Error not preserved: %+v", got.Error)
	}
	if got.Error.Category != CategoryTransient {
		t.Errorf("Category not preserved: %+v", got.Error)
	}
}

func TestBridgeCommandResponseSuccessOmitsError(t *testing.T) {
	resp := BridgeCommandResponse{ID: "cmd-2", Data: json.RawMessage(`{}`)}
	data, _ := json.Marshal(resp)
	if got := string(data); strings.Contains(got, `"error"`) {
		t.Errorf("success response must omit error field; got %s", got)
	}
	if resp.Error != nil {
		t.Errorf("success response must have nil Error")
	}
}

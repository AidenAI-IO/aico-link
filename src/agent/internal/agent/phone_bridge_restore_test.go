package agent

import (
	"aico-agent/internal/ble"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPhoneBridgeRestorerReturnsForegroundFromDynamicIsland(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.returnEntry = "dynamic_island"
	bridge.returnEntrySeen = true
	bridge.returnEntryOK = true
	bridge.mu.Unlock()

	restorer := NewPhoneBridgeRestorer(bridge, nil)
	restorer.waitTimeout = time.Second
	tapped := false
	restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
		tapped = true
		go func() {
			time.Sleep(10 * time.Millisecond)
			bridge.mu.Lock()
			bridge.connected = true
			bridge.platform = "ios"
			bridge.appState = "active"
			bridge.returnEntry = "none"
			bridge.returnEntrySeen = true
			bridge.returnEntryOK = false
			bridge.lastHeartbeatAt = time.Now()
			bridge.mu.Unlock()
		}()
		return nil
	}

	restored, err := restorer.EnsureForeground(context.Background())
	if err != nil {
		t.Fatalf("EnsureForeground() error = %v", err)
	}
	if !restored {
		t.Fatal("EnsureForeground() restored = false, want true")
	}
	if !tapped {
		t.Fatal("return entry was not tapped")
	}
	if !phoneBridgeReadyForCommand(bridge.getStatus()) {
		t.Fatalf("bridge status not ready after restore: %+v", bridge.getStatus())
	}
}

func TestPhoneBridgeRestorerDoesNotTapWithoutReturnEntry(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.returnEntry = "none"
	bridge.returnEntryOK = false
	bridge.mu.Unlock()

	restorer := NewPhoneBridgeRestorer(bridge, nil)
	restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
		t.Fatal("tapReturnEntry should not be called without a return entry")
		return nil
	}

	restored, err := restorer.EnsureForeground(context.Background())
	if err == nil {
		t.Fatal("EnsureForeground() error = nil, want missing return entry error")
	}
	if restored {
		t.Fatal("EnsureForeground() restored = true, want false")
	}
	if !strings.Contains(err.Error(), "no supported Dynamic Island return entry") {
		t.Fatalf("EnsureForeground() error = %v, want return entry message", err)
	}
}

func TestPhoneBridgeRestorerDoesNotTapLockScreenLiveActivity(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.returnEntry = "live_activity"
	bridge.returnEntrySeen = true
	bridge.returnEntryOK = true
	bridge.mu.Unlock()

	restorer := NewPhoneBridgeRestorer(bridge, nil)
	restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
		t.Fatal("tapReturnEntry should not be called for lock-screen Live Activity")
		return nil
	}

	restored, err := restorer.EnsureForeground(context.Background())
	if err == nil {
		t.Fatal("EnsureForeground() error = nil, want unsupported return entry error")
	}
	if restored {
		t.Fatal("EnsureForeground() restored = true, want false")
	}
	if !strings.Contains(err.Error(), "no supported Dynamic Island return entry") {
		t.Fatalf("EnsureForeground() error = %v, want unsupported Dynamic Island message", err)
	}
}

func TestPhoneBridgeReadyForCommandRejectsBackgroundIOS(t *testing.T) {
	if phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "ios", AppState: "background"}) {
		t.Fatal("background iOS bridge should not be considered ready for foreground command")
	}
	if !phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "ios", AppState: "active"}) {
		t.Fatal("active iOS bridge should be ready")
	}
	if phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "android", AppState: "background"}) {
		t.Fatal("background Android bridge should not be considered ready for foreground command")
	}
	if phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "android", AppState: "background"}, "open_app") {
		t.Fatal("background Android bridge should not be ready for open_app")
	}
	if !phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "android", AppState: "background"}, "clipboard_read") {
		t.Fatal("background-safe Android command should preserve connected-ready behavior")
	}
	if !phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "android", AppState: "active"}, "open_app") {
		t.Fatal("active Android bridge should be ready for open_app")
	}
	if !phoneBridgeReadyForCommand(PhoneBridgeStatus{Connected: true, Platform: "unknown", AppState: "background"}) {
		t.Fatal("connected non-iOS/non-Android bridge should preserve previous ready behavior")
	}
}

func TestPhoneBridgeBackgroundSafeCommandTypesCoverAllDataTools(t *testing.T) {
	for _, commandType := range []string{
		"clipboard_read",
		"clipboard_write",
		"calendar_create",
		"calendar_query",
		"calendar_delete",
		"contacts_query",
		"contacts_create",
		"contacts_update",
		"notification_send",
	} {
		if !phoneBridgeBackgroundSafeCommandType(commandType) {
			t.Errorf("%s must be background-safe", commandType)
		}
	}
	if phoneBridgeBackgroundSafeCommandType("open_app") {
		t.Fatal("open_app must not be background-safe")
	}
}

func TestPhoneBridgeCanUsePiPBackgroundOnlyForSafeDataCommands(t *testing.T) {
	enabled := true
	status := PhoneBridgeStatus{
		Platform:          "ios",
		AppState:          "background",
		AppStateUpdatedAt: ptrTime(time.Now()),
		PipBridgeEnabled:  &enabled,
	}
	if phoneBridgeCanUsePiPBackground(status, "open_app") {
		t.Fatal("open_app must not be allowed in iOS PiP background bridge mode")
	}

	status.AppState = "active"
	if phoneBridgeCanUsePiPBackground(status, "clipboard_read") {
		t.Fatal("foreground app should use WebSocket path, not PiP background queue")
	}

	disabled := false
	status.AppState = "background"
	status.PipBridgeEnabled = &disabled
	if phoneBridgeCanUsePiPBackground(status, "clipboard_read") {
		t.Fatal("disabled PiP bridge mode should not allow background queue")
	}

	status.PipBridgeEnabled = &enabled
	status.AppStateUpdatedAt = ptrTime(time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second))
	if !phoneBridgeCanUsePiPBackground(status, "clipboard_read") {
		t.Fatal("enabled PiP bridge should allow background queue despite stale app-state timestamp")
	}
}

func TestPhoneBridgeCanUseFGSBackgroundOnlyForSafeDataCommands(t *testing.T) {
	enabled := true
	status := PhoneBridgeStatus{
		Platform:           "android",
		AppState:           "background",
		AppStateUpdatedAt:  ptrTime(time.Now()),
		FgsBridgeEnabled:   &enabled,
		FgsBridgeUpdatedAt: ptrTime(time.Now()),
	}
	if phoneBridgeCanUseFGSBackground(status, "open_app") {
		t.Fatal("open_app must not be allowed in Android FGS background bridge mode")
	}

	status.AppState = "active"
	if phoneBridgeCanUseFGSBackground(status, "clipboard_read") {
		t.Fatal("foreground app should use WebSocket path, not FGS background queue")
	}

	disabled := false
	status.AppState = "background"
	status.FgsBridgeEnabled = &disabled
	if phoneBridgeCanUseFGSBackground(status, "clipboard_read") {
		t.Fatal("disabled FGS bridge mode should not allow background queue")
	}

	status.Platform = "ios"
	status.FgsBridgeEnabled = &enabled
	if phoneBridgeCanUseFGSBackground(status, "clipboard_read") {
		t.Fatal("FGS bridge mode should only apply to Android")
	}

	status.Platform = "android"
	status.FgsBridgeUpdatedAt = ptrTime(time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second))
	if phoneBridgeCanUseFGSBackground(status, "clipboard_read") {
		t.Fatal("stale FGS bridge status should not allow background queue")
	}
}

func TestPhoneBridgeCanUseBLEBackgroundOnlyForIOSDataCommands(t *testing.T) {
	status := PhoneBridgeStatus{Platform: "ios", AppState: "background"}
	for _, commandType := range []string{"calendar_query", "contacts_query", "contacts_create", "notification_send"} {
		if !phoneBridgeCanUseBLEBackground(status, commandType) {
			t.Fatalf("background iOS %s command should allow BLE wake routing", commandType)
		}
	}
	for _, commandType := range []string{"clipboard_read", "clipboard_write", "contacts_update", "open_app"} {
		if phoneBridgeCanUseBLEBackground(status, commandType) {
			t.Fatalf("%s must not use BLE background routing", commandType)
		}
	}
	status.Platform = "android"
	if phoneBridgeCanUseBLEBackground(status, "calendar_query") {
		t.Fatal("Android command must not use iOS BLE background routing")
	}
	status.Platform = "ios"
	status.AppState = "active"
	if phoneBridgeCanUseBLEBackground(status, "calendar_query") {
		t.Fatal("active iOS app should use the foreground WebSocket")
	}
}

func TestSendRoutedBridgeCommandUsesBLEWakeQueue(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.phoneID = "ios-phone"
	bridge.appState = "background"
	bridge.mu.Unlock()

	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
		}, nil
	}
	wakeCalled := make(chan struct{}, 1)
	bridge.bleWake = func(context.Context, string) error {
		wakeCalled <- struct{}{}
		return nil
	}

	go func() {
		for {
			commands := bridge.queue.PollForPhone("ios", "ios-phone", 10)
			if len(commands) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			if err := bridge.queue.SubmitResult(BridgeCommandResponse{
				ID:     commands[0].ID,
				Method: "calendar_query",
				Data:   json.RawMessage(`{"events":[]}`),
			}); err != nil {
				t.Errorf("SubmitResult() error = %v", err)
			}
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, restored, err := sendRoutedBridgeCommand(ctx, bridge, nil, BridgeCommand{
		ID:        "ble_calendar",
		Type:      "calendar_query",
		TimeoutMs: 1000,
	})
	if err != nil {
		t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
	}
	if restored {
		t.Fatal("BLE background route must not restore the app foreground")
	}
	if response.Error != nil || response.Method != "calendar_query" {
		t.Fatalf("response = %#v", response)
	}
	select {
	case <-wakeCalled:
	case <-time.After(time.Second):
		t.Fatal("BLE wake was not sent after queueing the command")
	}
}

func TestSendRoutedBridgeCommandUsesConfiguredPlatformBeforeFirstAppPoll(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.SetConfiguredPlatform("ios")
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
		}, nil
	}
	bridge.bleWake = func(context.Context, string) error { return nil }

	go func() {
		for {
			commands := bridge.queue.PollForPhone("ios", "", 10)
			if len(commands) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			_ = bridge.queue.SubmitResult(BridgeCommandResponse{
				ID:     commands[0].ID,
				Method: "calendar_query",
				Data:   json.RawMessage(`{"events":[]}`),
			})
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, _, err := sendRoutedBridgeCommand(ctx, bridge, nil, BridgeCommand{
		ID:        "cold_start_calendar",
		Type:      "calendar_query",
		TimeoutMs: 1000,
	})
	if err != nil {
		t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
	}
	if response.Error != nil || response.Method != "calendar_query" {
		t.Fatalf("response = %#v", response)
	}
}

func TestSendRoutedBridgeCommandRejectsUnsupportedBLEWakeCommands(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.mu.Unlock()
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
		}, nil
	}

	for _, commandType := range []string{"clipboard_read", "clipboard_write", "contacts_update"} {
		t.Run(commandType, func(t *testing.T) {
			response, restored, err := sendRoutedBridgeCommand(context.Background(), bridge, nil, BridgeCommand{
				ID:   "unsupported_" + commandType,
				Type: commandType,
			})
			if err != nil {
				t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
			}
			if restored {
				t.Fatal("unsupported BLE command must not report foreground restoration")
			}
			if response.Error == nil || response.Error.Code != CodeAppBackgrounded {
				t.Fatalf("response error = %#v, want app_backgrounded", response.Error)
			}
			if response.Error.Details["transport"] != "ios_ble_wake" {
				t.Fatalf("response details = %#v", response.Error.Details)
			}
			if commands := bridge.queue.PollForPhone("ios", "", 10); len(commands) != 0 {
				t.Fatalf("unsupported BLE command was queued: %#v", commands)
			}
		})
	}
}

func TestPhoneBridgeCannotRestoreFromDynamicIslandWhenPiPBackgroundEnabled(t *testing.T) {
	enabled := true
	available := true
	status := PhoneBridgeStatus{
		Platform:             "ios",
		AppState:             "background",
		ReturnEntry:          "dynamic_island",
		ReturnEntryAvailable: &available,
		PipBridgeEnabled:     &enabled,
	}
	if phoneBridgeCanRestoreFromReturnEntry(status) {
		t.Fatal("PiP background bridge mode hides Dynamic Island and must block restore")
	}

	enabled = false
	if !phoneBridgeCanRestoreFromReturnEntry(status) {
		t.Fatal("visible Dynamic Island entry should allow restore when PiP bridge mode is disabled")
	}
}

func TestSendRoutedBridgeCommandChoosesDeliveryPath(t *testing.T) {
	t.Run("foreground websocket", func(t *testing.T) {
		bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
			if cmd.Type != "clipboard_read" {
				t.Errorf("command type = %q, want clipboard_read", cmd.Type)
			}
			return BridgeCommandResponse{Method: "foreground"}
		})

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		resp, restored, err := sendRoutedBridgeCommand(ctx, bridge, nil, BridgeCommand{
			ID:        "route_foreground",
			Type:      "clipboard_read",
			TimeoutMs: 1000,
		})
		if err != nil {
			t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
		}
		if restored {
			t.Fatal("sendRoutedBridgeCommand() restored = true, want false")
		}
		if resp.Method != "foreground" {
			t.Fatalf("response method = %q, want foreground", resp.Method)
		}
	})

	t.Run("pip background queue", func(t *testing.T) {
		bridge := newPhoneBridgeForTest()
		t.Cleanup(func() { bridge.queue.Stop() })
		bridge.mu.Lock()
		bridge.platform = "ios"
		bridge.appState = "background"
		bridge.appStateAt = time.Now()
		bridge.pipBridgeEnabled = true
		bridge.pipBridgeSeen = true
		bridge.mu.Unlock()

		tapCalled := false
		restorer := NewPhoneBridgeRestorer(bridge, nil)
		restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
			tapCalled = true
			return nil
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			commands := bridge.queue.PollForPhone("ios", "", 10)
			if len(commands) != 1 {
				t.Errorf("expected one queued command, got %d", len(commands))
				return
			}
			if commands[0].Type != "clipboard_read" {
				t.Errorf("queued command type = %q, want clipboard_read", commands[0].Type)
				return
			}
			if err := bridge.queue.SubmitResult(BridgeCommandResponse{
				ID:     commands[0].ID,
				Method: "queued",
			}); err != nil {
				t.Errorf("SubmitResult() error = %v", err)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		resp, restored, err := sendRoutedBridgeCommand(ctx, bridge, restorer, BridgeCommand{
			ID:        "route_queued",
			Type:      "clipboard_read",
			TimeoutMs: 1000,
		})
		if err != nil {
			t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
		}
		if restored {
			t.Fatal("sendRoutedBridgeCommand() restored = true, want false")
		}
		if tapCalled {
			t.Fatal("return entry tap should not be used for PiP background queue")
		}
		if resp.Method != "queued" {
			t.Fatalf("response method = %q, want queued", resp.Method)
		}
	})

	t.Run("android fgs background queue", func(t *testing.T) {
		bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
			t.Errorf("websocket should not receive command in Android FGS background mode: %+v", cmd)
			return BridgeCommandResponse{Method: "unexpected_websocket"}
		})
		bridge.mu.Lock()
		bridge.platform = "android"
		bridge.appState = "background"
		bridge.appStateAt = time.Now()
		bridge.fgsBridgeEnabled = true
		bridge.fgsBridgeSeen = true
		bridge.fgsBridgeAt = time.Now()
		bridge.mu.Unlock()

		tapCalled := false
		restorer := NewPhoneBridgeRestorer(bridge, nil)
		restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
			tapCalled = true
			return nil
		}
		go func() {
			time.Sleep(10 * time.Millisecond)
			commands := bridge.queue.PollForPhone("android", "", 10)
			if len(commands) != 1 {
				t.Errorf("expected one queued command, got %d", len(commands))
				return
			}
			if commands[0].Type != "clipboard_read" {
				t.Errorf("queued command type = %q, want clipboard_read", commands[0].Type)
				return
			}
			if err := bridge.queue.SubmitResult(BridgeCommandResponse{
				ID:     commands[0].ID,
				Method: "queued_android_fgs",
			}); err != nil {
				t.Errorf("SubmitResult() error = %v", err)
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		resp, restored, err := sendRoutedBridgeCommand(ctx, bridge, restorer, BridgeCommand{
			ID:        "route_android_fgs",
			Type:      "clipboard_read",
			TimeoutMs: 1000,
		})
		if err != nil {
			t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
		}
		if restored {
			t.Fatal("sendRoutedBridgeCommand() restored = true, want false")
		}
		if tapCalled {
			t.Fatal("return entry tap should not be used for Android FGS background queue")
		}
		if resp.Method != "queued_android_fgs" {
			t.Fatalf("response method = %q, want queued_android_fgs", resp.Method)
		}
	})

	t.Run("restore then websocket", func(t *testing.T) {
		bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
			if cmd.Type != "clipboard_read" {
				t.Errorf("command type = %q, want clipboard_read", cmd.Type)
			}
			return BridgeCommandResponse{Method: "restored"}
		})
		bridge.mu.Lock()
		bridge.platform = "ios"
		bridge.appState = "background"
		bridge.returnEntry = "dynamic_island"
		bridge.returnEntrySeen = true
		bridge.returnEntryOK = true
		bridge.mu.Unlock()

		tapCalled := false
		restorer := NewPhoneBridgeRestorer(bridge, nil)
		restorer.waitTimeout = time.Second
		restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
			tapCalled = true
			bridge.mu.Lock()
			bridge.appState = "active"
			bridge.returnEntry = "none"
			bridge.returnEntrySeen = true
			bridge.returnEntryOK = false
			bridge.lastHeartbeatAt = time.Now()
			bridge.mu.Unlock()
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		resp, restored, err := sendRoutedBridgeCommand(ctx, bridge, restorer, BridgeCommand{
			ID:        "route_restored",
			Type:      "clipboard_read",
			TimeoutMs: 1000,
		})
		if err != nil {
			t.Fatalf("sendRoutedBridgeCommand() error = %v", err)
		}
		if !restored {
			t.Fatal("sendRoutedBridgeCommand() restored = false, want true")
		}
		if !tapCalled {
			t.Fatal("return entry tap was not used before routed command")
		}
		if resp.Method != "restored" {
			t.Fatalf("response method = %q, want restored", resp.Method)
		}
	})
}

func ptrTime(value time.Time) *time.Time {
	return &value
}

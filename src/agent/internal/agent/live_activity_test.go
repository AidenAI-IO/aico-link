package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLiveActivityManagerLifecycle(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	if manager == nil {
		t.Fatal("NewLiveActivityManager() = nil")
	}

	state := manager.StartTask("req-1", "Open Settings and enable Wi-Fi")
	if state == nil || state.Status != LiveActivityStatusRunning {
		t.Fatalf("StartTask() state = %#v", state)
	}
	if state.Progress <= 0 || !state.CanStop || state.Phase != LiveActivityPhasePlanning || state.CurrentAction != "process" || state.ToolStatus != "processing" {
		t.Fatalf("unexpected initial state: %#v", state)
	}

	state = manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "screenshot",
		Content:   "Checking the current screen",
		Timestamp: time.Now(),
	})
	if state == nil || state.LastToolName != "screenshot" || !strings.Contains(state.CurrentStep, "Checking") {
		t.Fatalf("unexpected tool update state: %#v", state)
	}

	state = manager.CompleteTask("req-1", "Done")
	if state == nil || state.Status != LiveActivityStatusCompleted || state.ToolStatus != "succeeded" || state.CanStop || state.Progress != 1 {
		t.Fatalf("unexpected completion state: %#v", state)
	}

}

func TestLiveActivityManagerDisablingCancelsAllRunningTasks(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-1", "First task")
	manager.StartTask("req-2", "Second task")

	manager.Reconfigure(LiveActivityConfig{Enabled: new(bool)}, "en-US")

	for _, requestID := range []string{"req-1", "req-2"} {
		state := manager.Snapshot(requestID)
		if state == nil || state.Status != LiveActivityStatusCanceled || state.ToolStatus != "" {
			t.Fatalf("state[%s] = %#v, want canceled", requestID, state)
		}
	}
}

func TestLiveActivityManagerSummarizesAgentSteps(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-1", "Book a table")

	state := manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      "role_output",
		Role:      "agent",
		Content:   `{"plan":["Open Maps","Search restaurant"]}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "Planning: Open Maps" {
		t.Fatalf("agent step = %#v, want Open Maps", state)
	}

	state = manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "open_app",
		ToolInput: `{"app":"Maps"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "Opening Maps" || state.CurrentApp != "Maps" || state.Phase != LiveActivityPhaseActing || state.RequiresApp {
		t.Fatalf("open_app step = %#v, want targeted app step and current app", state)
	}

	state = manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "screenshot",
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "Checking the screen" {
		t.Fatalf("screenshot step = %#v, want Checking the screen", state)
	}
}

func TestLiveActivityManagerLocalizesTextByLocale(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "zh-CN", newTestLogger())
	if manager == nil {
		t.Fatal("NewLiveActivityManager() = nil")
	}
	manager.StartTask("req-zh", "预订餐厅")

	state := manager.UpdateFromRunEvent("req-zh", RunEvent{
		Type:      "role_output",
		Role:      "agent",
		Content:   `{"plan":["打开地图"]}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "规划：打开地图" {
		t.Fatalf("zh plan step = %q, want 规划：打开地图", state.CurrentStep)
	}

	state = manager.UpdateFromRunEvent("req-zh", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "screenshot",
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "正在查看屏幕" {
		t.Fatalf("zh screenshot step = %q, want 正在查看屏幕", state.CurrentStep)
	}

	state = manager.UpdateFromRunEvent("req-zh", RunEvent{
		Type:     "tool_result",
		ToolName: "screenshot",
	})
	if state == nil || state.CurrentStep != "已查看屏幕" {
		t.Fatalf("zh screenshot result step = %q, want 已查看屏幕", state.CurrentStep)
	}

	state = manager.UpdateFromRunEvent("req-zh", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "open_url",
		ToolInput: `{"url":"https://example.com"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "正在打开https://example.com" {
		t.Fatalf("zh open_url step = %q, want 正在打开https://example.com", state.CurrentStep)
	}

	manager.Reconfigure(LiveActivityConfig{}, "en-US")
	state = manager.UpdateFromRunEvent("req-zh", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "screenshot",
		Timestamp: time.Now(),
	})
	if state == nil || state.CurrentStep != "Checking the screen" {
		t.Fatalf("en screenshot step after reconfigure = %q, want Checking the screen", state.CurrentStep)
	}
}

func TestLiveActivityManagerPublishesToolAndThinkingDetails(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-details", "Open Settings")

	state := manager.UpdateFromRunEvent("req-details", RunEvent{
		Type:             "role_output",
		Role:             "agent",
		Content:          `{"plan":["Open Settings"]}`,
		ReasoningContent: "Checking the current screen state",
		Timestamp:        time.Now(),
	})
	if state == nil || state.ToolStatus != "processing" || state.CurrentAction != "process" {
		t.Fatalf("role output details = %#v", state)
	}

	state = manager.UpdateFromRunEvent("req-details", RunEvent{
		Type:             runEventReasoningDelta,
		ReasoningContent: "Checking the current screen state",
		Timestamp:        time.Now(),
	})
	if state == nil || state.ToolStatus != "thinking" || state.CurrentAction != "think" {
		t.Fatalf("thinking details = %#v", state)
	}

	startedAt := time.Now().Add(-2 * time.Second)
	state = manager.UpdateFromRunEvent("req-details", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "screenshot",
		Content:   "Checking the current screen",
		Timestamp: startedAt,
	})
	if state == nil || state.ToolStatus != "running" || state.ToolStartedAt == nil || !state.ToolStartedAt.Equal(startedAt) {
		t.Fatalf("tool start details = %#v", state)
	}

	state = manager.UpdateFromRunEvent("req-details", RunEvent{
		Type:       runEventToolProgress,
		ToolStatus: "running",
		Content:    "Still checking the current screen",
		Timestamp:  startedAt.Add(5 * time.Second),
	})
	if state == nil || state.ToolStartedAt == nil || !state.ToolStartedAt.Equal(startedAt) {
		t.Fatalf("tool progress reset start time: %#v", state)
	}

	state = manager.UpdateFromRunEvent("req-details", RunEvent{
		Type:      "tool_result",
		ToolName:  "screenshot",
		Content:   `{"summary":"Current screen captured"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.ToolStatus != "succeeded" || state.ToolStartedAt != nil {
		t.Fatalf("tool result details = %#v", state)
	}
}

func TestLiveActivityOpenURLUsesSchemeSpecificStatus(t *testing.T) {
	tests := []struct {
		name string
		url  string
		app  string
		step string
	}{
		{name: "web", url: "https://example.com", app: "Browser", step: "Opening https://example.com"},
		{name: "sms", url: "sms:+15551234567?body=hello", app: "Messages", step: "Opening message composer"},
		{name: "email", url: "mailto:user@example.com?subject=hello", app: "Mail", step: "Opening email composer"},
		{name: "phone", url: "tel:+15551234567", app: "Phone", step: "Opening phone"},
	}

	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	text := manager.getLiveActivityText()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := RunEvent{ToolName: toolOpenURL, ToolInput: jsonString(map[string]string{"url": tt.url})}
			status := liveActivityToolCallStatus(event, text)
			if status.app != tt.app || status.step != tt.step {
				t.Fatalf("status = %#v, want app=%q step=%q", status, tt.app, tt.step)
			}
			if got := liveActivityAppFromToolCall(event, text); got != tt.app {
				t.Fatalf("liveActivityAppFromToolCall() = %q, want %q", got, tt.app)
			}
		})
	}
	if got := liveActivityToolResultStep(toolOpenURL, text); got != "Link opened" {
		t.Fatalf("liveActivityToolResultStep(open_url) = %q, want Link opened", got)
	}
}

func TestLiveActivityManagerNeedsAppWhenBridgeUnavailable(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-1", "Read clipboard")

	state := manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      runEventToolCall,
		ToolName:  "bridge_clipboard",
		ToolInput: `{"action":"read"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.Status != LiveActivityStatusRunning || state.Phase != LiveActivityPhasePhoneBridge || !state.RequiresApp {
		t.Fatalf("clipboard call state = %#v, want phone bridge state requiring app", state)
	}
	if state.CurrentStep != "Reading clipboard" {
		t.Fatalf("clipboard call step = %q, want Reading clipboard", state.CurrentStep)
	}

	state = manager.UpdateFromRunEvent("req-1", RunEvent{
		Type:      "tool_result",
		ToolName:  "bridge_clipboard",
		ToolInput: `{"action":"read"}`,
		Content:   `{"ok":false,"error":"phone bridge not connected"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.Status != LiveActivityStatusNeedsApp || state.Phase != LiveActivityPhaseWaitingApp || !state.RequiresApp {
		t.Fatalf("bridge unavailable state = %#v, want needs_app", state)
	}
	if state.CurrentStep != "Open the companion app to continue" || state.ShowsProgress {
		t.Fatalf("bridge unavailable display = %#v, want open companion app without progress", state)
	}
}

func TestLiveActivityManagerKeepsHumanHandoffVisible(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-handoff", "Complete login")

	state := manager.UpdateFromRunEvent("req-handoff", RunEvent{
		Type:      runEventToolCall,
		ToolName:  toolUserActionStep,
		ToolInput: `{"reason":"verification_code","details":"A verification code is required","suggested_action":"请在手机上输入验证码"}`,
		Content:   "Waiting for the user to complete verification",
		Timestamp: time.Now(),
	})
	if state == nil || state.Status != LiveActivityStatusNeedsApp || state.Phase != LiveActivityPhaseWaitingUser {
		t.Fatalf("handoff call state = %#v, want needs_app waiting_user", state)
	}
	if state.CurrentStep != "请在手机上输入验证码" || state.CurrentAction != "request_user_input" || state.ShowsProgress {
		t.Fatalf("handoff call display = %#v, want takeover instructions without progress", state)
	}

	state = manager.UpdateFromRunEvent("req-handoff", RunEvent{
		Type:      "tool_result",
		ToolName:  toolUserActionStep,
		Content:   `{"status":"HUMAN_HANDOFF_REQUESTED","reason":"verification_code","details":"A verification code is required","suggested_action":"请在手机上输入验证码"}`,
		Timestamp: time.Now(),
	})
	if state == nil || state.Status != LiveActivityStatusNeedsApp || state.Phase != LiveActivityPhaseWaitingUser || state.CurrentStep != "请在手机上输入验证码" {
		t.Fatalf("handoff result state = %#v, want persistent takeover instructions", state)
	}

	state = manager.CompleteTask("req-handoff", "请在手机上输入验证码")
	if state == nil || state.Status != LiveActivityStatusNeedsApp || state.Phase != LiveActivityPhaseWaitingUser {
		t.Fatalf("handoff completion state = %#v, want paused needs_app", state)
	}
	if state.CanStop || state.ShowsProgress || state.EndedAt != nil {
		t.Fatalf("handoff completion lifecycle = %#v, want paused non-terminal state", state)
	}
}

func TestLiveActivityManagerSnapshotActive(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-1", "First task")
	manager.StartTask("req-2", "Second task")

	active := manager.SnapshotActive()
	if active == nil || active.RequestID != "req-2" {
		t.Fatalf("active state = %#v, want req-2", active)
	}

	manager.CompleteTask("req-2", "Done")
	active = manager.SnapshotActive()
	if active == nil || active.RequestID != "req-2" || active.Status != LiveActivityStatusCompleted {
		t.Fatalf("active terminal state = %#v, want completed req-2", active)
	}
}

func TestLiveActivityManagerSnapshotActiveForPhone(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-phone-a", "First phone", "phone-a")
	manager.StartTask("req-phone-b", "Second phone", "phone-b")

	active := manager.SnapshotActiveForPhone("phone-a")
	if active == nil || active.RequestID != "req-phone-a" || active.PhoneID != "phone-a" {
		t.Fatalf("active state for phone-a = %#v, want req-phone-a", active)
	}

	active = manager.SnapshotActiveForPhone("phone-b")
	if active == nil || active.RequestID != "req-phone-b" || active.PhoneID != "phone-b" {
		t.Fatalf("active state for phone-b = %#v, want req-phone-b", active)
	}

	if active := manager.SnapshotActiveForPhone("phone-missing"); active != nil {
		t.Fatalf("active state for missing phone = %#v, want nil", active)
	}
}

func TestLiveActivityManagerSnapshotActiveForPhoneIncludesUnscopedLocalTask(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	manager.StartTask("req-local", "Hardware initiated task")

	active := manager.SnapshotActiveForPhone("phone-a")
	if active == nil || active.RequestID != "req-local" || active.PhoneID != "" {
		t.Fatalf("active unscoped state = %#v, want req-local", active)
	}
}

func TestLiveActivityManagerCoalescesLocalBLEWake(t *testing.T) {
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	reasons := make(chan string, 4)
	manager.SetLocalUpdateNotifier(func(ctx context.Context, reason string) error {
		reasons <- reason
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	manager.StartTask("req-1", "Open Settings")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("local BLE notifier did not start")
	}
	for i := 0; i < 10; i++ {
		manager.UpdateFromRunEvent("req-1", RunEvent{
			Type:      runEventToolCall,
			ToolName:  "screenshot",
			Timestamp: time.Now(),
		})
	}
	manager.CompleteTask("req-1", "Done")
	close(release)

	deadline := time.After(2 * time.Second)
	for len(reasons) < 2 {
		select {
		case <-deadline:
			t.Fatalf("local BLE wake count = %d, want coalesced trailing update", len(reasons))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if first, second := <-reasons, <-reasons; first != "live_activity" || second != "live_activity" {
		t.Fatalf("local BLE reasons = %q, %q", first, second)
	}
	time.Sleep(100 * time.Millisecond)
	if extra := len(reasons); extra != 0 {
		t.Fatalf("local BLE notifier emitted %d uncoalesced extra wake(s)", extra)
	}
}

func TestServerLiveActivityRegistrationRouteRemoved(t *testing.T) {
	server := &Server{logger: newTestLogger()}
	req := httptest.NewRequest(http.MethodPost, "/api/live-activity/registrations", nil)
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("registration status = %d body=%s, want 404", rec.Code, rec.Body.String())
	}
}

func TestServerLiveActivityStatus(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}
	server.liveActivity.StartTask("req-1", "Do a task")

	statusReq := liveActivityLoopbackRequest("/api/live-activity/status?request_id=%20req-1%20")
	statusRec := httptest.NewRecorder()
	server.handleLiveActivityStatus(statusRec, statusReq)
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", statusRec.Code, statusRec.Body.String())
	}
	var statusResp struct {
		Status       string            `json:"status"`
		LiveActivity LiveActivityState `json:"live_activity"`
	}
	if err := json.NewDecoder(statusRec.Body).Decode(&statusResp); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if statusResp.Status != "ok" || statusResp.LiveActivity.RequestID != "req-1" {
		t.Fatalf("unexpected status response: %#v", statusResp)
	}
}

func TestServerLiveActivityCurrent(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}
	server.liveActivity.StartTask("req-1", "Do a task")

	req := liveActivityLoopbackRequest("/api/live-activity/current")
	rec := httptest.NewRecorder()
	server.handleLiveActivityCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string            `json:"status"`
		LiveActivity LiveActivityState `json:"live_activity"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode current response: %v", err)
	}
	if resp.Status != "ok" || resp.LiveActivity.RequestID != "req-1" || resp.LiveActivity.Phase != LiveActivityPhasePlanning {
		t.Fatalf("unexpected current response: %#v", resp)
	}
}

func TestServerBridgeStatusWithoutBridge(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		runtime: &Runtime{
			config: Config{
				Device: DeviceConfig{DeviceType: "Android"},
			},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/status", nil)
	rec := httptest.NewRecorder()

	server.handleBridgeStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["board_id"]; ok {
		t.Fatalf("board_id must not be exposed: %#v", payload)
	}
	if payload["device_type"] != "Android" {
		t.Fatalf("device_type = %q, want Android", payload["device_type"])
	}
	if payload["pointer_mode"] != "touchscreen" {
		t.Fatalf("pointer_mode = %q, want touchscreen", payload["pointer_mode"])
	}
	if _, ok := payload["target_platform"]; ok {
		t.Fatalf("target_platform must not be exposed: %#v", payload)
	}
	if connected, _ := payload["connected"].(bool); connected {
		t.Fatalf("connected = true, want false")
	}
}

func TestServerLiveActivityCurrentFiltersPhoneID(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}
	server.liveActivity.StartTask("req-phone-a", "Do a task", "phone-a")
	server.liveActivity.StartTask("req-phone-b", "Do another task", "phone-b")

	req := liveActivityLoopbackRequest("/api/live-activity/current?phone_id=phone-a")
	rec := httptest.NewRecorder()
	server.handleLiveActivityCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status       string            `json:"status"`
		LiveActivity LiveActivityState `json:"live_activity"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode current response: %v", err)
	}
	if resp.Status != "ok" || resp.LiveActivity.RequestID != "req-phone-a" || resp.LiveActivity.PhoneID != "phone-a" {
		t.Fatalf("unexpected filtered current response: %#v", resp)
	}

	missingReq := liveActivityLoopbackRequest("/api/live-activity/current?phone_id=phone-missing")
	missingRec := httptest.NewRecorder()
	server.handleLiveActivityCurrent(missingRec, missingReq)
	if missingRec.Code != http.StatusOK {
		t.Fatalf("missing status code = %d body=%s", missingRec.Code, missingRec.Body.String())
	}
	var missingResp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(missingRec.Body).Decode(&missingResp); err != nil {
		t.Fatalf("decode missing current response: %v", err)
	}
	if missingResp.Status != "not_found" {
		t.Fatalf("unexpected missing current response: %#v", missingResp)
	}
}

func TestServerLiveActivityPhoneIDPreference(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.phoneID = "bridge-phone"
	server := &Server{logger: newTestLogger(), bridge: bridge}
	if got := server.liveActivityPhoneID(ChatRequest{PhoneID: "request-phone"}); got != "request-phone" {
		t.Fatalf("liveActivityPhoneID request = %q, want request-phone", got)
	}
	if got := server.liveActivityPhoneID(ChatRequest{}); got != "bridge-phone" {
		t.Fatalf("liveActivityPhoneID bridge = %q, want bridge-phone", got)
	}
}

func TestServerLiveActivityCurrentEmpty(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}

	req := liveActivityLoopbackRequest("/api/live-activity/current")
	rec := httptest.NewRecorder()
	server.handleLiveActivityCurrent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode current empty response: %v", err)
	}
	if resp.Status != "not_found" {
		t.Fatalf("unexpected current empty response: %#v", resp)
	}
}

func TestServerLiveActivityStatusRequiresRequestID(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}
	req := liveActivityLoopbackRequest("/api/live-activity/status")
	rec := httptest.NewRecorder()

	server.handleLiveActivityStatus(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d body=%s, want 400", rec.Code, rec.Body.String())
	}
}

func TestServerLiveActivityEndpointsRequireUSB(t *testing.T) {
	server := &Server{logger: newTestLogger(), liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())}
	server.liveActivity.StartTask("req-1", "Do a task")

	usbRequest := httptest.NewRequest(http.MethodGet, "/api/live-activity/current", nil)
	usbRequest.RemoteAddr = "192.168.42.2:12345"
	usbRequest = usbRequest.WithContext(context.WithValue(
		usbRequest.Context(),
		http.LocalAddrContextKey,
		&net.TCPAddr{IP: net.ParseIP("192.168.42.1"), Port: 8080},
	))
	usbRecorder := httptest.NewRecorder()
	server.handleLiveActivityCurrent(usbRecorder, usbRequest)
	if usbRecorder.Code != http.StatusOK {
		t.Fatalf("USB current status = %d body=%s, want 200", usbRecorder.Code, usbRecorder.Body.String())
	}

	for _, target := range []string{
		"/api/live-activity/current",
		"/api/live-activity/status?request_id=req-1",
	} {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.RemoteAddr = "192.168.50.2:12345"
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("non-USB %s status = %d body=%s, want 403", target, recorder.Code, recorder.Body.String())
		}
	}
}

func liveActivityLoopbackRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "127.0.0.1:12345"
	return request
}

func TestChatResultIncludesLiveActivityState(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger()),
		pendingResults: map[string]*chatPendingResult{
			"req-1": {
				messages: []Message{},
				history:  []Message{},
			},
		},
	}
	server.liveActivity.StartTask("req-1", "Do a task")

	req := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id=req-1", nil)
	rec := httptest.NewRecorder()
	server.handleChatResult(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode chat result: %v", err)
	}
	if resp.Status != "running" || resp.LiveActivity == nil || resp.LiveActivity.RequestID != "req-1" {
		t.Fatalf("unexpected chat result: %#v", resp)
	}
}

func TestChatResultIncludesTerminalLiveActivityState(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger()),
		pendingResults: map[string]*chatPendingResult{
			"req-1": {
				messages: []Message{{
					Type:      "assistant",
					RequestID: "req-1",
					Content:   "Done",
					Timestamp: time.Now(),
				}},
				history: []Message{{
					Type:      "assistant",
					RequestID: "req-1",
					Content:   "Done",
					Timestamp: time.Now(),
				}},
				done: true,
			},
		},
	}
	server.liveActivity.StartTask("req-1", "Do a task")
	server.liveActivity.CompleteTask("req-1", "Done")

	req := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id=req-1", nil)
	rec := httptest.NewRecorder()
	server.handleChatResult(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode chat result: %v", err)
	}
	if resp.Status != "complete" || resp.LiveActivity == nil || resp.LiveActivity.Status != LiveActivityStatusCompleted {
		t.Fatalf("unexpected terminal chat result: %#v", resp)
	}
}

func TestChatResultErrorIncludesQueuedMessages(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger()),
		pendingResults: map[string]*chatPendingResult{
			"req-1": {
				messages: []Message{{
					Type:      "episode_status",
					RequestID: "req-1",
					Content:   `{"status":"failed"}`,
					Timestamp: time.Now(),
				}},
				err: "boom",
			},
		},
	}
	server.liveActivity.StartTask("req-1", "Do a task")
	server.liveActivity.FailTask("req-1", "boom")

	req := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id=req-1", nil)
	rec := httptest.NewRecorder()
	server.handleChatResult(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ChatResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode chat result: %v", err)
	}
	if resp.Status != "error" || resp.Error != "boom" || len(resp.Messages) != 1 {
		t.Fatalf("unexpected error chat result: %#v", resp)
	}
}

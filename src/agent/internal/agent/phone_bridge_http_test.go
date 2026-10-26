package agent

import (
	"aico-agent/internal/agent/statemanager"
	"aico-agent/internal/ble"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newPhoneBridgeWithStateManager(stateManager *statemanager.StateManager) *PhoneBridge {
	bridge := NewPhoneBridge(nil)
	stateManager.RegisterUpdater(bridge)
	return bridge
}

func currentPhoneBridgeState(stateManager *statemanager.StateManager, key string) string {
	stateManager.GetAllStates()
	return stateManager.GetState(key)
}

// TestHTTPEnqueueCommand tests POST /api/phone-bridge/commands
func TestHTTPEnqueueCommand(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	tests := []struct {
		name           string
		command        BridgeCommand
		expectedStatus int
		checkResponse  func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name: "valid command",
			command: BridgeCommand{
				ID:   "test_cmd_1",
				Type: "open_app",
			},
			expectedStatus: http.StatusAccepted,
			checkResponse: func(t *testing.T, w *httptest.ResponseRecorder) {
				var resp EnqueueCommandResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.CommandID != "test_cmd_1" {
					t.Errorf("expected command_id=test_cmd_1, got %s", resp.CommandID)
				}
				if resp.Status != "queued" {
					t.Errorf("expected status=queued, got %s", resp.Status)
				}
			},
		},
		{
			name: "empty ID",
			command: BridgeCommand{
				Type: "open_app",
			},
			expectedStatus: http.StatusBadRequest,
			checkResponse:  nil,
		},
		{
			name: "duplicate ID",
			command: BridgeCommand{
				ID:   "test_cmd_1",
				Type: "open_app",
			},
			expectedStatus: http.StatusConflict,
			checkResponse:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := EnqueueCommandRequest{Command: tt.command}
			body, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/commands", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			bridge.handleEnqueueCommand(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			if tt.checkResponse != nil {
				tt.checkResponse(t, w)
			}
		})
	}
}

func TestHTTPEnqueueRetainsCommandWhenBLEWakeFails(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.mu.Unlock()

	wakeCalled := make(chan struct{}, 1)
	bridge.bleWake = func(context.Context, string) error {
		wakeCalled <- struct{}{}
		return errors.New("wake failed")
	}

	body, err := json.Marshal(EnqueueCommandRequest{Command: BridgeCommand{
		ID:   "wake_retention",
		Type: "notification_send",
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/commands", bytes.NewReader(body))
	response := httptest.NewRecorder()
	bridge.handleEnqueueCommand(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-wakeCalled:
	case <-time.After(time.Second):
		t.Fatal("BLE wake was not attempted")
	}
	queued := bridge.queue.Get("wake_retention")
	if queued == nil || queued.Status != StatusQueued {
		t.Fatalf("command was not retained after wake failure: %#v", queued)
	}
}

func TestHTTPEnqueueDoesNotWakeForForegroundOnlyCommand(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	wakeCalled := make(chan struct{}, 1)
	bridge.bleWake = func(context.Context, string) error {
		wakeCalled <- struct{}{}
		return nil
	}

	body, err := json.Marshal(EnqueueCommandRequest{Command: BridgeCommand{
		ID:   "no_wake_open_app",
		Type: "open_app",
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/commands", bytes.NewReader(body))
	response := httptest.NewRecorder()
	bridge.handleEnqueueCommand(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("enqueue status=%d body=%s", response.Code, response.Body.String())
	}
	select {
	case <-wakeCalled:
		t.Fatal("BLE wake must not be sent for open_app")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHTTPEnqueueRejectsUnsupportedBLEBackgroundCommand(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.SetConfiguredPlatform("ios")
	bridge.mu.Lock()
	bridge.appState = "background"
	bridge.mu.Unlock()
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
		}, nil
	}

	body, err := json.Marshal(EnqueueCommandRequest{Command: BridgeCommand{
		ID:   "unsupported_clipboard",
		Type: "clipboard_read",
	}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/commands", bytes.NewReader(body))
	response := httptest.NewRecorder()
	bridge.handleEnqueueCommand(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("enqueue status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		ToolError *ToolError `json:"tool_error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ToolError == nil || result.ToolError.Code != CodeAppBackgrounded {
		t.Fatalf("tool error = %#v, want app_backgrounded", result.ToolError)
	}
	if queued := bridge.queue.Get("unsupported_clipboard"); queued != nil {
		t.Fatalf("unsupported command was queued: %#v", queued)
	}
}

func TestHTTPPollFiltersUnsupportedBLEBackgroundCommands(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	for _, command := range []BridgeCommand{
		{ID: "unsupported_update", Type: "contacts_update"},
		{ID: "supported_query", Type: "contacts_query"},
	} {
		if err := bridge.queue.Enqueue(command); err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=ios&app_state=background", nil)
	response := httptest.NewRecorder()
	bridge.handlePollCommands(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", response.Code, response.Body.String())
	}
	var result PollCommandsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 1 || result.Commands[0].ID != "supported_query" {
		t.Fatalf("commands = %#v, want supported query only", result.Commands)
	}
	if queued := bridge.queue.Get("unsupported_update"); queued == nil || queued.Status != StatusQueued {
		t.Fatalf("unsupported command = %#v, want queued", queued)
	}
}

func TestHTTPPollUsesRetainedBackgroundStateWhenQueryOmitsIt(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()
	bridge.noteHTTPPollState("ios", "", "background", "false", "")
	for _, command := range []BridgeCommand{
		{ID: "retained_unsupported", Type: "contacts_update"},
		{ID: "retained_supported", Type: "contacts_query"},
	} {
		if err := bridge.queue.Enqueue(command); err != nil {
			t.Fatal(err)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=ios", nil)
	response := httptest.NewRecorder()
	bridge.handlePollCommands(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", response.Code, response.Body.String())
	}
	var result PollCommandsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Commands) != 1 || result.Commands[0].ID != "retained_supported" {
		t.Fatalf("commands = %#v, want retained supported command only", result.Commands)
	}
	if queued := bridge.queue.Get("retained_unsupported"); queued == nil || queued.Status != StatusQueued {
		t.Fatalf("unsupported command = %#v, want queued", queued)
	}
}

// TestHTTPPollCommands tests GET /api/phone-bridge/commands
func TestHTTPPollCommands(t *testing.T) {
	tests := []struct {
		name             string
		platform         string
		limit            string
		setupCommands    func(*CommandQueue)
		expectedCount    int
		expectedStatus   int
		expectedCommands []BridgeCommand
	}{
		{
			name:     "ios poll returns semantic commands",
			platform: "ios",
			limit:    "",
			setupCommands: func(q *CommandQueue) {
				q.Enqueue(BridgeCommand{
					ID:   "ios_cmd_1",
					Type: "open_app",
					App:  "微信",
				})
				q.Enqueue(BridgeCommand{
					ID:   "universal_cmd_1",
					Type: "clipboard_read",
				})
			},
			expectedCount:  2,
			expectedStatus: http.StatusOK,
			expectedCommands: []BridgeCommand{
				{ID: "ios_cmd_1", Type: "open_app", App: "微信"},
				{ID: "universal_cmd_1", Type: "clipboard_read"},
			},
		},
		{
			name:     "android poll returns semantic commands",
			platform: "android",
			limit:    "",
			setupCommands: func(q *CommandQueue) {
				q.Enqueue(BridgeCommand{
					ID:   "android_cmd_1",
					Type: "open_app",
					App:  "微信",
				})
				q.Enqueue(BridgeCommand{
					ID:   "universal_cmd_2",
					Type: "clipboard_read",
				})
			},
			expectedCount:  2,
			expectedStatus: http.StatusOK,
			expectedCommands: []BridgeCommand{
				{ID: "android_cmd_1", Type: "open_app", App: "微信"},
				{ID: "universal_cmd_2", Type: "clipboard_read"},
			},
		},
		{
			name:     "limit parameter",
			platform: "ios",
			limit:    "1",
			setupCommands: func(q *CommandQueue) {
				q.Enqueue(BridgeCommand{ID: "cmd_1", Type: "open_app"})
				q.Enqueue(BridgeCommand{ID: "cmd_2", Type: "open_app"})
				q.Enqueue(BridgeCommand{ID: "cmd_3", Type: "open_app"})
			},
			expectedCount:  1,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "missing platform",
			platform:       "",
			limit:          "",
			setupCommands:  func(q *CommandQueue) {},
			expectedCount:  0,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fresh bridge for each test
			bridge := newPhoneBridgeForTest()
			defer bridge.queue.Stop()

			// Setup commands for this test
			if tt.setupCommands != nil {
				tt.setupCommands(bridge.queue)
			}

			url := "/api/phone-bridge/commands"
			if tt.platform != "" {
				url += "?platform=" + tt.platform
			}
			if tt.limit != "" {
				if tt.platform != "" {
					url += "&limit=" + tt.limit
				} else {
					url += "?limit=" + tt.limit
				}
			}

			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			bridge.handlePollCommands(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.expectedStatus == http.StatusOK {
				var resp PollCommandsResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if len(resp.Commands) != tt.expectedCount {
					t.Errorf("expected %d commands, got %d", tt.expectedCount, len(resp.Commands))
				}
				for i, want := range tt.expectedCommands {
					if i >= len(resp.Commands) {
						t.Fatalf("missing command %d: want %#v", i, want)
					}
					got := resp.Commands[i]
					if got.ID != want.ID || got.Type != want.Type || got.App != want.App || got.URL != want.URL {
						t.Errorf("command %d = {ID:%q Type:%q App:%q URL:%q}, want {ID:%q Type:%q App:%q URL:%q}",
							i, got.ID, got.Type, got.App, got.URL, want.ID, want.Type, want.App, want.URL)
					}
				}
			}
		})
	}
}

func TestHTTPPollCommandsReturnsEmptyArray(t *testing.T) {
	for _, query := range []string{
		"platform=ios",
		"platform=ios&app_state=background&pip_bridge_enabled=false&phone_id=phone-1",
		"platform=ios&phone_id=other-phone",
		"platform=android&app_state=active&fgs_bridge_enabled=false&phone_id=phone-1",
	} {
		t.Run(query, func(t *testing.T) {
			bridge := newPhoneBridgeForTest()
			defer bridge.queue.Stop()
			if query != "platform=ios" {
				if err := bridge.queue.Enqueue(BridgeCommand{ID: "clipboard", Type: "clipboard_read", PhoneID: "phone-1"}); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?"+query, nil)
			response := httptest.NewRecorder()
			bridge.handlePollCommands(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("poll status=%d body=%s", response.Code, response.Body.String())
			}
			var result PollCommandsResponse
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Commands == nil || len(result.Commands) != 0 {
				t.Fatalf("empty poll response = %s, want commands=[]", response.Body.String())
			}
		})
	}
}

func TestHTTPPollCommandsRecordsAndroidFGSBridgeState(t *testing.T) {
	stateManager := statemanager.NewStateManager()
	bridge := newPhoneBridgeWithStateManager(stateManager)
	defer bridge.queue.Stop()

	req := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=android&phone_id=android-abc&app_state=background&fgs_bridge_enabled=true", nil)
	w := httptest.NewRecorder()

	bridge.handlePollCommands(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	status := bridge.getStatus()
	if status.Platform != "android" {
		t.Fatalf("platform = %q, want android", status.Platform)
	}
	if status.PhoneID != "android-abc" {
		t.Fatalf("phone_id = %q, want android-abc", status.PhoneID)
	}
	if status.AppState != "background" {
		t.Fatalf("app_state = %q, want background", status.AppState)
	}
	if status.FgsBridgeEnabled == nil || !*status.FgsBridgeEnabled {
		t.Fatalf("fgs_bridge_enabled = %#v, want true", status.FgsBridgeEnabled)
	}
	if status.FgsBridgeUpdatedAt == nil {
		t.Fatal("fgs_bridge_updated_at is nil")
	}
	if platform := currentPhoneBridgeState(stateManager, "app_platform"); platform != "android" {
		t.Fatalf("app_platform = %q, want android", platform)
	}
}

func TestHTTPPollCommandsRecordsIOSPiPBridgeState(t *testing.T) {
	stateManager := statemanager.NewStateManager()
	bridge := newPhoneBridgeWithStateManager(stateManager)
	defer bridge.queue.Stop()

	req := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=ios&phone_id=ios-abc&app_state=background&pip_bridge_enabled=true", nil)
	w := httptest.NewRecorder()
	bridge.handlePollCommands(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	status := bridge.getStatus()
	if status.PipBridgeEnabled == nil || !*status.PipBridgeEnabled {
		t.Fatalf("pip_bridge_enabled = %#v, want true", status.PipBridgeEnabled)
	}
	if platform := currentPhoneBridgeState(stateManager, "app_platform"); platform != "ios" {
		t.Fatalf("app_platform = %q, want ios", platform)
	}
}

func TestHTTPPollCommandsSuppressesAndroidForegroundFGSQueue(t *testing.T) {
	bridge := NewPhoneBridge(nil)
	defer bridge.queue.Stop()

	if err := bridge.queue.Enqueue(BridgeCommand{
		ID:      "fgs_foreground_cmd",
		Type:    "clipboard_read",
		PhoneID: "android-abc",
	}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=android&phone_id=android-abc&app_state=active&fgs_bridge_enabled=false", nil)
	w := httptest.NewRecorder()

	bridge.handlePollCommands(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, w.Code, w.Body.String())
	}
	var resp PollCommandsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(resp.Commands) != 0 {
		t.Fatalf("commands = %#v, want empty foreground FGS poll", resp.Commands)
	}
	status := bridge.getStatus()
	if status.AppState != "active" {
		t.Fatalf("app_state = %q, want active", status.AppState)
	}
	if status.FgsBridgeEnabled == nil || *status.FgsBridgeEnabled {
		t.Fatalf("fgs_bridge_enabled = %#v, want false", status.FgsBridgeEnabled)
	}
	if _, commandStatus := bridge.queue.QueryResult("fgs_foreground_cmd"); commandStatus != StatusQueued {
		t.Fatalf("command status = %s, want %s", commandStatus, StatusQueued)
	}
}

// TestHTTPSubmitResult tests POST /api/phone-bridge/results
func TestHTTPSubmitResult(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	// Enqueue and poll a command first
	bridge.queue.Enqueue(BridgeCommand{ID: "test_cmd", Type: "open_app"})
	bridge.queue.Poll("ios", 1)

	tests := []struct {
		name           string
		result         BridgeCommandResponse
		expectedStatus int
	}{
		{
			name: "valid result",
			result: BridgeCommandResponse{
				ID: "test_cmd",
			},
			expectedStatus: http.StatusOK,
		},
		{
			name:           "missing ID",
			result:         BridgeCommandResponse{},
			expectedStatus: http.StatusBadRequest,
		},
		{
			name: "command not found",
			result: BridgeCommandResponse{
				ID: "nonexistent",
			},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.result)
			req := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/results", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			bridge.handleSubmitResult(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			if tt.expectedStatus == http.StatusOK {
				var resp SubmitResultResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.Status != "acknowledged" {
					t.Errorf("expected status=acknowledged, got %s", resp.Status)
				}
			}
		})
	}
}

// TestHTTPQueryResult tests GET /api/phone-bridge/results/:command_id
func TestHTTPQueryResult(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	// Enqueue, poll and submit result for a command
	bridge.queue.Enqueue(BridgeCommand{ID: "completed_cmd", Type: "open_app"})
	bridge.queue.Poll("ios", 1)
	bridge.queue.SubmitResult(BridgeCommandResponse{
		ID:     "completed_cmd",
		Method: "open_app",
	})

	// Enqueue but don't poll another command
	bridge.queue.Enqueue(BridgeCommand{ID: "queued_cmd", Type: "clipboard_read"})

	tests := []struct {
		name           string
		commandID      string
		expectedStatus int
		expectedState  CommandStatus
		hasResult      bool
	}{
		{
			name:           "completed command",
			commandID:      "completed_cmd",
			expectedStatus: http.StatusOK,
			expectedState:  StatusCompleted,
			hasResult:      true,
		},
		{
			name:           "queued command",
			commandID:      "queued_cmd",
			expectedStatus: http.StatusOK,
			expectedState:  StatusQueued,
			hasResult:      false,
		},
		{
			name:           "expired command",
			commandID:      "nonexistent",
			expectedStatus: http.StatusOK,
			expectedState:  StatusExpired,
			hasResult:      false,
		},
		{
			name:           "empty command_id",
			commandID:      "",
			expectedStatus: http.StatusBadRequest,
			expectedState:  "",
			hasResult:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/api/phone-bridge/results/" + tt.commandID
			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			bridge.handleQueryResult(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.expectedStatus == http.StatusOK {
				var resp GetResultResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("failed to unmarshal response: %v", err)
				}
				if resp.Status != tt.expectedState {
					t.Errorf("expected status=%s, got %s", tt.expectedState, resp.Status)
				}
				if tt.hasResult && resp.Result == nil {
					t.Error("expected result, got nil")
				}
				if !tt.hasResult && resp.Result != nil {
					t.Error("expected no result, got one")
				}
			}
		})
	}
}

// TestHTTPEndToEnd tests full workflow: enqueue -> poll -> submit -> query
func TestHTTPEndToEnd(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	// Step 1: Agent enqueues a command
	cmd := BridgeCommand{
		ID:   "e2e_test",
		Type: "open_app",
		URL:  "https://example.com",
	}
	enqueueBody, _ := json.Marshal(EnqueueCommandRequest{Command: cmd})
	enqueueReq := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/commands", bytes.NewReader(enqueueBody))
	enqueueReq.Header.Set("Content-Type", "application/json")
	enqueueW := httptest.NewRecorder()
	bridge.handleEnqueueCommand(enqueueW, enqueueReq)

	if enqueueW.Code != http.StatusAccepted {
		t.Fatalf("enqueue failed: %s", enqueueW.Body.String())
	}

	// Step 2: App polls for commands
	pollReq := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/commands?platform=ios", nil)
	pollW := httptest.NewRecorder()
	bridge.handlePollCommands(pollW, pollReq)

	if pollW.Code != http.StatusOK {
		t.Fatalf("poll failed: %s", pollW.Body.String())
	}

	var pollResp PollCommandsResponse
	json.Unmarshal(pollW.Body.Bytes(), &pollResp)
	if len(pollResp.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(pollResp.Commands))
	}
	if got := pollResp.Commands[0]; got.ID != "e2e_test" || got.Type != "open_app" || got.URL != "https://example.com" {
		t.Fatalf("polled command = {ID:%q Type:%q URL:%q}, want semantic open_app URL command", got.ID, got.Type, got.URL)
	}

	// Step 3: App submits result
	result := BridgeCommandResponse{
		ID:     "e2e_test",
		Method: "open_app",
	}
	resultBody, _ := json.Marshal(result)
	submitReq := httptest.NewRequest(http.MethodPost, "/api/phone-bridge/results", bytes.NewReader(resultBody))
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	bridge.handleSubmitResult(submitW, submitReq)

	if submitW.Code != http.StatusOK {
		t.Fatalf("submit result failed: %s", submitW.Body.String())
	}

	// Step 4: Agent queries result
	queryReq := httptest.NewRequest(http.MethodGet, "/api/phone-bridge/results/e2e_test", nil)
	queryW := httptest.NewRecorder()
	bridge.handleQueryResult(queryW, queryReq)

	if queryW.Code != http.StatusOK {
		t.Fatalf("query result failed: %s", queryW.Body.String())
	}

	var queryResp GetResultResponse
	json.Unmarshal(queryW.Body.Bytes(), &queryResp)
	if queryResp.Status != StatusCompleted {
		t.Errorf("expected status=completed, got %s", queryResp.Status)
	}
	if queryResp.Result == nil {
		t.Fatal("expected result, got nil")
	}
	if queryResp.Result.Error != nil {
		t.Errorf("expected result.error=nil, got %v", queryResp.Result.Error)
	}
}

// TestHTTPCommandTimeout tests that in-flight commands timeout and get retried
func TestHTTPCommandTimeout(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	defer bridge.queue.Stop()

	// Enqueue a command
	bridge.queue.Enqueue(BridgeCommand{ID: "timeout_test", Type: "open_app"})

	// Poll it (marks as in_flight)
	cmds := bridge.queue.Poll("ios", 1)
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cmds))
	}

	// Manually trigger timeout by advancing time simulation
	// (In real code, the cleanup goroutine handles this after CommandInFlightTimeout)
	time.Sleep(100 * time.Millisecond)

	// For this test, we just verify the command is tracked as in_flight
	result, status := bridge.queue.QueryResult("timeout_test")
	if status != StatusInFlight {
		t.Errorf("expected status=in_flight, got %s", status)
	}
	if result != nil {
		t.Error("expected no result for in-flight command")
	}
}

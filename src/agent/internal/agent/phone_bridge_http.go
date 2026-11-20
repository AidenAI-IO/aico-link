package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const phoneBridgeProxyMaxBodyBytes = 1 << 20 // 1 MiB

// EnqueueCommandRequest is the request body for POST /api/phone-bridge/commands
type EnqueueCommandRequest struct {
	Command BridgeCommand `json:"command"`
}

// EnqueueCommandResponse is returned by POST /api/phone-bridge/commands
type EnqueueCommandResponse struct {
	CommandID string `json:"command_id"`
	Status    string `json:"status"`
	QueuedAt  string `json:"queued_at"`
}

// PollCommandsResponse is returned by GET /api/phone-bridge/commands
type PollCommandsResponse struct {
	Commands   []BridgeCommand `json:"commands"`
	ServerTime string          `json:"server_time"`
}

// SubmitResultRequest is the request body for POST /api/phone-bridge/results
type SubmitResultRequest struct {
	BridgeCommandResponse
}

// SubmitResultResponse is returned by POST /api/phone-bridge/results
type SubmitResultResponse struct {
	Status string `json:"status"`
}

// GetResultResponse is returned by GET /api/phone-bridge/results/:command_id
type GetResultResponse struct {
	CommandID   string                 `json:"command_id"`
	Status      CommandStatus          `json:"status"`
	Result      *BridgeCommandResponse `json:"result,omitempty"`
	CompletedAt *string                `json:"completed_at,omitempty"`
}

// handleEnqueueCommand handles POST /api/phone-bridge/commands
// Enqueues a command from the agent to be picked up by the app.
// Returns 202 Accepted immediately, actual execution happens async.
func (pb *PhoneBridge) handleEnqueueCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Proxy mode: forward to remote agent
	if pb.proxyMode {
		pb.proxyHTTPRequest(w, r, "/api/phone-bridge/commands")
		return
	}

	var req EnqueueCommandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: enqueue command decode failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"Invalid request: %v"}`, err), http.StatusBadRequest)
		return
	}

	if req.Command.ID == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"command ID must not be empty"}`, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Command.PhoneID) == "" {
		req.Command.PhoneID = pb.currentPhoneID()
	}
	status := pb.getStatus()
	if phoneBridgeUnsupportedBLEBackgroundCommand(status, req.Command.Type) {
		capabilityCtx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		bleWakeAvailable := pb.bleWakeAvailable(capabilityCtx)
		cancel()
		if toolErr := phoneBridgeUnsupportedBLEBackgroundError(status, req.Command.Type, bleWakeAvailable); toolErr != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error":      toolErr.Message,
				"tool_error": toolErr,
			})
			return
		}
	}

	if err := pb.queue.Enqueue(req.Command); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: enqueue command failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, ErrCommandExists) {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
		} else {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		}
		return
	}
	pb.notifyBLEWake(req.Command)

	cmd := pb.queue.Get(req.Command.ID)
	if cmd == nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"command not found after enqueue"}`, http.StatusInternalServerError)
		return
	}

	if pb.logger != nil {
		pb.logger.Info("phone-bridge: command enqueued: id=%s type=%s", req.Command.ID, req.Command.Type)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(EnqueueCommandResponse{
		CommandID: req.Command.ID,
		Status:    "queued",
		QueuedAt:  cmd.QueuedAt.UTC().Format("2006-01-02T15:04:05Z"),
	})
}

// handleCancelCommand handles DELETE /api/phone-bridge/commands/:command_id.
// It lets a synchronous proxy withdraw a queued command after its caller has
// stopped waiting, preventing a late background execution.
func (pb *PhoneBridge) handleCancelCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	commandID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/phone-bridge/commands/"))
	if commandID == "" || strings.Contains(commandID, "/") || strings.Contains(commandID, "\\") ||
		commandID == "." || commandID == ".." {
		http.Error(w, `{"error":"Invalid command ID"}`, http.StatusBadRequest)
		return
	}
	if pb.proxyMode {
		pb.proxyHTTPRequest(w, r, "/api/phone-bridge/commands/"+url.PathEscape(commandID))
		return
	}
	if pb.queue == nil || !pb.queue.Cancel(commandID) {
		http.Error(w, `{"error":"command not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "canceled", "command_id": commandID})
}

// handlePollCommands handles GET /api/phone-bridge/commands?platform=ios&limit=10
// Returns commands waiting to be executed by the app.
// Atomically marks returned commands as in_flight.
func (pb *PhoneBridge) handlePollCommands(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Proxy mode: forward to remote agent
	if pb.proxyMode {
		pb.proxyHTTPRequest(w, r, "/api/phone-bridge/commands")
		return
	}

	platform := r.URL.Query().Get("platform")
	if platform == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"platform parameter required"}`, http.StatusBadRequest)
		return
	}

	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 50 {
			limit = l
		}
	}

	phoneID := strings.TrimSpace(r.URL.Query().Get("phone_id"))
	pb.noteHTTPPollState(
		platform,
		phoneID,
		r.URL.Query().Get("app_state"),
		r.URL.Query().Get("pip_bridge_enabled"),
		r.URL.Query().Get("fgs_bridge_enabled"),
	)
	pb.noteHTTPPollOrientation(r.URL.Query().Get("device_orientation"))

	status := pb.getStatus()
	appState, appStateOK := normalizeAppState(r.URL.Query().Get("app_state"))
	if !appStateOK {
		appState = status.AppState
	}
	pipEnabled, pipEnabledOK := parseOptionalBoolQuery(r.URL.Query().Get("pip_bridge_enabled"))
	if !pipEnabledOK && status.PipBridgeEnabled != nil {
		pipEnabled = *status.PipBridgeEnabled
	}
	fgsEnabled, fgsEnabledOK := parseOptionalBoolQuery(r.URL.Query().Get("fgs_bridge_enabled"))
	if !fgsEnabledOK && status.FgsBridgeEnabled != nil {
		fgsEnabled = *status.FgsBridgeEnabled
	}

	var commands []BridgeCommand
	if !shouldSuppressHTTPCommandPoll(platform, appState, fgsEnabled) {
		commands = pb.queue.PollForPhoneMatching(platform, phoneID, limit, func(cmd BridgeCommand) bool {
			return phoneBridgeHTTPPollCommandAllowed(platform, appState, pipEnabled, fgsEnabled, cmd.Type)
		})
	}
	if commands == nil {
		commands = []BridgeCommand{}
	}

	if pb.logger != nil && len(commands) > 0 {
		var cmdIDs []string
		for _, cmd := range commands {
			cmdIDs = append(cmdIDs, cmd.ID)
		}
		pb.logger.Info("phone-bridge: polled %d commands: %s", len(commands), strings.Join(cmdIDs, ", "))
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(PollCommandsResponse{
		Commands:   commands,
		ServerTime: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
}

func shouldSuppressHTTPCommandPoll(platform, appState string, fgsBridgeEnabled bool) bool {
	if strings.TrimSpace(strings.ToLower(platform)) != "android" {
		return false
	}
	return strings.TrimSpace(strings.ToLower(appState)) == "active" && !fgsBridgeEnabled
}

func (pb *PhoneBridge) noteHTTPPollState(platform, phoneID, appState, pipBridgeEnabled, fgsBridgeEnabled string) {
	platform = strings.TrimSpace(platform)
	phoneID = strings.TrimSpace(phoneID)
	appState, appStateOK := normalizeAppState(appState)
	enabled, enabledOK := parseOptionalBoolQuery(pipBridgeEnabled)
	fgsEnabled, fgsEnabledOK := parseOptionalBoolQuery(fgsBridgeEnabled)
	now := time.Now()

	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	pb.updateHIDConnectionPhoneLocked(phoneID)
	if platform != "" {
		pb.platform = platform
	}
	if phoneID != "" || !pb.connected {
		pb.phoneID = phoneID
	}
	if appStateOK {
		pb.appState = appState
		pb.appStateAt = now
	}
	if enabledOK {
		pb.pipBridgeEnabled = enabled
		pb.pipBridgeSeen = true
	}
	if fgsEnabledOK {
		pb.fgsBridgeEnabled = fgsEnabled
		pb.fgsBridgeSeen = true
		pb.fgsBridgeAt = now
	}
	pb.mu.Unlock()
	pb.notifyEnvironmentObserver()
}

// noteHTTPPollOrientation keeps the phone orientation a background app sends
// with each command poll, since it is not on the WebSocket then.
func (pb *PhoneBridge) noteHTTPPollOrientation(value string) {
	orientation, ok := normalizeDeviceOrientation(value)
	if !ok {
		return
	}
	pb.mu.Lock()
	changed := pb.noteDeviceOrientationLocked(orientation, time.Now())
	pb.mu.Unlock()
	if changed && pb.logger != nil {
		pb.logger.Info("phone-bridge: device orientation updated (%s) via http poll", orientation)
	}
}

func parseOptionalBoolQuery(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true, true
	case "false", "0", "no":
		return false, true
	default:
		return false, false
	}
}

// handleSubmitResult handles POST /api/phone-bridge/results
// Receives execution result from the app and stores it for agent retrieval.
func (pb *PhoneBridge) handleSubmitResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Proxy mode: forward to remote agent
	if pb.proxyMode {
		pb.proxyHTTPRequest(w, r, "/api/phone-bridge/results")
		return
	}

	var req SubmitResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: submit result decode failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, fmt.Sprintf(`{"error":"Invalid request: %v"}`, err), http.StatusBadRequest)
		return
	}

	if req.ID == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"result ID must not be empty"}`, http.StatusBadRequest)
		return
	}

	if err := pb.queue.SubmitResult(req.BridgeCommandResponse); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: submit result failed: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadRequest)
		}
		return
	}

	if pb.logger != nil {
		ok := req.Error == nil
		pb.logger.Info("phone-bridge: result received: id=%s ok=%t", req.ID, ok)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(SubmitResultResponse{
		Status: "acknowledged",
	})
}

// handleQueryResult handles GET /api/phone-bridge/results/:command_id
// Returns the current status and result (if completed) for a command.
func (pb *PhoneBridge) handleQueryResult(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"Method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Proxy mode: forward to remote agent
	if pb.proxyMode {
		// Extract and validate command ID to prevent path traversal
		path := strings.TrimPrefix(r.URL.Path, "/api/phone-bridge/results/")
		commandID := strings.TrimSpace(path)

		// Reject path separators and dot segments
		if commandID == "" || strings.Contains(commandID, "/") || strings.Contains(commandID, "\\") ||
			commandID == "." || commandID == ".." {
			http.Error(w, `{"error":"Invalid command ID"}`, http.StatusBadRequest)
			return
		}

		// Build safe path with validated ID
		safePath := "/api/phone-bridge/results/" + url.PathEscape(commandID)
		pb.proxyHTTPRequest(w, r, safePath)
		return
	}

	// Extract command_id from path: /api/phone-bridge/results/:command_id
	path := strings.TrimPrefix(r.URL.Path, "/api/phone-bridge/results/")
	commandID := strings.TrimSpace(path)
	if commandID == "" {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"command_id required"}`, http.StatusBadRequest)
		return
	}

	result, status := pb.queue.QueryResult(commandID)

	resp := GetResultResponse{
		CommandID: commandID,
		Status:    status,
	}

	if result != nil {
		resp.Result = &result.Response
		completedAt := result.CompletedAt.UTC().Format("2006-01-02T15:04:05Z")
		resp.CompletedAt = &completedAt
	}

	if pb.logger != nil {
		pb.logger.Info("phone-bridge: query result: id=%s status=%s", commandID, status)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// proxyHTTPRequest forwards an HTTP request to the remote agent in proxy mode.
func (pb *PhoneBridge) proxyHTTPRequest(w http.ResponseWriter, r *http.Request, path string) {
	// Build remote URL
	remoteURL := pb.proxyEndpoint + path
	if r.URL.RawQuery != "" {
		remoteURL = remoteURL + "?" + r.URL.RawQuery
	}

	// Forward the request body without buffering it in memory
	var body io.Reader
	if r.Body != nil {
		body = http.MaxBytesReader(w, r.Body, phoneBridgeProxyMaxBodyBytes)
	}

	// Create proxied request
	req, err := http.NewRequestWithContext(r.Context(), r.Method, remoteURL, body)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: create request failed: %v", err)
		}
		http.Error(w, fmt.Sprintf(`{"error":"create request: %v"}`, err), http.StatusBadGateway)
		return
	}

	// Copy relevant headers
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if pb.proxyTaskID != "" {
		req.Header.Set("benchmark-task-id", pb.proxyTaskID)
	}

	// Send request
	resp, err := pb.proxyClient.Do(req)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: send request failed: %v", err)
		}
		http.Error(w, fmt.Sprintf(`{"error":"send request: %v"}`, err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, phoneBridgeProxyMaxBodyBytes+1))
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"read response: %v"}`, err), http.StatusBadGateway)
		return
	}
	if int64(len(responseBody)) > phoneBridgeProxyMaxBodyBytes {
		http.Error(w, `{"error":"proxy response exceeds 1 MiB limit"}`, http.StatusBadGateway)
		return
	}

	// Copy response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

// sendProxyQueuedCommand uses the environment bridge's HTTP queue. The App
// continues polling and submitting results to the device-side Agent while the
// benchmark daemon observes a synchronous tool call.
func (pb *PhoneBridge) sendProxyQueuedCommand(ctx context.Context, cmd BridgeCommand) (BridgeCommandResponse, error) {
	status := pb.getProxyStatus()
	if strings.TrimSpace(cmd.PhoneID) == "" {
		cmd.PhoneID = status.PhoneID
	}
	body, err := json.Marshal(EnqueueCommandRequest{Command: cmd})
	if err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeCommandMarshalFailed, fmt.Sprintf("marshal queued command: %v", err)),
		}, nil
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		pb.proxyEndpoint+"/api/phone-bridge/commands",
		strings.NewReader(string(body)),
	)
	if err != nil {
		return BridgeCommandResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	pb.setProxyTaskHeader(req)
	resp, err := pb.proxyClient.Do(req)
	if err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeNotConnected, fmt.Sprintf("enqueue relayed command: %v", err)),
		}, nil
	}
	responseBody, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return BridgeCommandResponse{}, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeToolExecutionFailed, fmt.Sprintf("enqueue relayed command returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(responseBody)))),
		}, nil
	}

	timeout := 10 * time.Second
	if cmd.TimeoutMs > 0 {
		timeout = time.Duration(cmd.TimeoutMs) * time.Millisecond
		if timeout < 10*time.Second {
			timeout = 10 * time.Second
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for waitCtx.Err() == nil {
		result, done, err := pb.queryProxyQueuedResult(waitCtx, cmd.ID)
		if err != nil {
			if isTransientProxyTransportError(err) {
				select {
				case <-waitCtx.Done():
					goto timeout
				case <-ticker.C:
					continue
				}
			}
			return BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeToolExecutionFailed, err.Error()),
			}, nil
		}
		if done {
			return result, nil
		}
		select {
		case <-waitCtx.Done():
			goto timeout
		case <-ticker.C:
		}
	}

timeout:
	cancelCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	cancelErr := pb.cancelProxyQueuedCommand(cancelCtx, cmd.ID)
	cancel()
	if ctx.Err() != nil {
		return BridgeCommandResponse{}, ctx.Err()
	}
	if cancelErr != nil && pb.logger != nil {
		pb.logger.Warn("phone-bridge-proxy: cancel timed-out command %s failed: %v", cmd.ID, cancelErr)
	}
	return BridgeCommandResponse{
		ID:    cmd.ID,
		Error: NewToolError(CodeBridgeTimeout, "queued relayed command timeout"),
	}, nil
}

// queryProxyQueuedResult polls one remote queue result and distinguishes
// incomplete results from terminal protocol failures.
func (pb *PhoneBridge) queryProxyQueuedResult(ctx context.Context, commandID string) (BridgeCommandResponse, bool, error) {
	remoteURL := pb.proxyEndpoint + "/api/phone-bridge/results/" + url.PathEscape(commandID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return BridgeCommandResponse{}, false, err
	}
	pb.setProxyTaskHeader(req)
	resp, err := pb.proxyClient.Do(req)
	if err != nil {
		return BridgeCommandResponse{}, false, &proxyTransportError{
			err: fmt.Errorf("query relayed command result: %w", err),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return BridgeCommandResponse{}, false, fmt.Errorf(
			"query relayed command result returned HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	var result GetResultResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return BridgeCommandResponse{}, false, fmt.Errorf("decode relayed command result: %w", err)
	}
	switch result.Status {
	case StatusCompleted:
		if result.Result == nil {
			return BridgeCommandResponse{}, false, fmt.Errorf("completed relayed command has no result")
		}
		return *result.Result, true, nil
	case StatusExpired:
		return BridgeCommandResponse{
			ID:    commandID,
			Error: NewToolError(CodeBridgeTimeout, "queued command expired before result"),
		}, true, nil
	default:
		return BridgeCommandResponse{}, false, nil
	}
}

// setProxyTaskHeader scopes proxy HTTP requests to the active benchmark task.
func (pb *PhoneBridge) setProxyTaskHeader(req *http.Request) {
	if pb.proxyTaskID != "" {
		req.Header.Set("benchmark-task-id", pb.proxyTaskID)
	}
}

// proxyTransportError marks an HTTP client transport failure as retryable while
// keeping protocol and payload failures terminal.
type proxyTransportError struct {
	err error
}

func (e *proxyTransportError) Error() string { return e.err.Error() }
func (e *proxyTransportError) Unwrap() error { return e.err }

func isTransientProxyTransportError(err error) bool {
	var transportErr *proxyTransportError
	if !errors.As(err, &transportErr) || errors.Is(err, context.Canceled) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

// cancelProxyQueuedCommand asks the environment bridge to remove a command
// whose synchronous caller stopped waiting.
func (pb *PhoneBridge) cancelProxyQueuedCommand(ctx context.Context, commandID string) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		pb.proxyEndpoint+"/api/phone-bridge/commands/"+url.PathEscape(commandID),
		nil,
	)
	if err != nil {
		return err
	}
	pb.setProxyTaskHeader(req)
	resp, err := pb.proxyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel relayed command returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

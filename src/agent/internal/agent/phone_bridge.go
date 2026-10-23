package agent

import (
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/ble"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	// heartbeatTimeout matches the protocol contract: a connection that goes
	// this long without a heartbeat is considered dead. The app sends one every
	// 10-15s, so 60s tolerates a few missed beats before we tear down.
	heartbeatTimeout = 60 * time.Second
	// heartbeatCheckInterval is how often the monitor re-checks liveness.
	heartbeatCheckInterval = 15 * time.Second
	// phoneBridgeBLECacheTTL avoids repeated UDS status requests while keeping
	// runtime tool availability responsive to BLE connection changes.
	phoneBridgeBLECacheTTL = 1500 * time.Millisecond
	// phoneBridgeBenchmarkRelayQuery distinguishes a benchmark daemon from the
	// companion app on the shared WebSocket endpoint. The daemon dials the
	// device-side environment bridge; that bridge executes each command over its
	// existing App connection and returns the App response on this socket.
	phoneBridgeBenchmarkRelayQuery = "benchmark_relay"
)

type BridgeCommand struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	PhoneID   string `json:"phone_id,omitempty"`
	App       string `json:"app,omitempty"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	// Payload carries command-specific JSON (clipboard text, calendar event,
	// query window, etc.). open_app uses semantic top-level fields.
	Payload json.RawMessage `json:"payload,omitempty"`
}

type BridgeCommandResponse struct {
	ID     string          `json:"id"`
	Method string          `json:"method,omitempty"`
	Error  *ToolError      `json:"error,omitempty"` // nil ⇔ success
	Data   json.RawMessage `json:"data,omitempty"`
}

type PhoneEnvironment struct {
	CapturedAt       string                 `json:"captured_at,omitempty"`
	Source           string                 `json:"source,omitempty"`
	Platform         string                 `json:"platform,omitempty"`
	SystemName       string                 `json:"system_name,omitempty"`
	SystemVersion    string                 `json:"system_version,omitempty"`
	IsTablet         *bool                  `json:"is_tablet,omitempty"`
	Locale           string                 `json:"locale,omitempty"`
	Language         string                 `json:"language,omitempty"`
	Region           string                 `json:"region,omitempty"`
	TimeZone         string                 `json:"time_zone,omitempty"`
	UTCOffsetMinutes *int                   `json:"utc_offset_minutes,omitempty"`
	UTCOffset        string                 `json:"utc_offset,omitempty"`
	Uses24HourClock  *bool                  `json:"uses_24_hour_clock,omitempty"`
	Manufacturer     string                 `json:"manufacturer,omitempty"`
	Brand            string                 `json:"brand,omitempty"`
	Model            string                 `json:"model,omitempty"`
	DeviceName       string                 `json:"device_name,omitempty"`
	Screen           screen.PhoneScreenInfo `json:"screen,omitempty"`
	Battery          PhoneBatteryInfo       `json:"battery,omitempty"`
	SystemApps       []AvailableAppInfo     `json:"system_apps,omitempty"`
	ThirdPartyApps   []AvailableAppInfo     `json:"third_party_apps,omitempty"`
	AvailableApps    []AvailableAppInfo     `json:"available_apps,omitempty"`
}

type PhoneBatteryInfo struct {
	Level    *float64 `json:"level,omitempty"`
	Charging *bool    `json:"charging,omitempty"`
	State    string   `json:"state,omitempty"`
}

type AvailableAppInfo struct {
	Name               string `json:"name"`
	Available          bool   `json:"available"`
	Category           string `json:"category,omitempty"`
	AvailabilitySource string `json:"availability_source,omitempty"`
	IOSURL             string `json:"ios_url,omitempty"`
	AndroidPackage     string `json:"android_package,omitempty"`
	Note               string `json:"note,omitempty"`
	Error              string `json:"error,omitempty"`
}

type PhoneBridgeStatus struct {
	Connected            bool              `json:"connected"`
	Platform             string            `json:"platform,omitempty"`
	DeviceType           string            `json:"device_type,omitempty"`
	PointerMode          string            `json:"pointer_mode,omitempty"`
	PhoneID              string            `json:"phone_id,omitempty"`
	HIDConnectionID      string            `json:"hid_connection_id,omitempty"`
	LastHeartbeatAt      *time.Time        `json:"last_heartbeat_at,omitempty"`
	AppState             string            `json:"app_state,omitempty"`
	AppStateUpdatedAt    *time.Time        `json:"app_state_updated_at,omitempty"`
	ReturnEntry          string            `json:"return_entry,omitempty"`
	ReturnEntryAvailable *bool             `json:"return_entry_available,omitempty"`
	PipBridgeEnabled     *bool             `json:"pip_bridge_enabled,omitempty"`
	FgsBridgeEnabled     *bool             `json:"fgs_bridge_enabled,omitempty"`
	FgsBridgeUpdatedAt   *time.Time        `json:"fgs_bridge_updated_at,omitempty"`
	Environment          *PhoneEnvironment `json:"environment,omitempty"`
	EnvironmentUpdatedAt *time.Time        `json:"environment_updated_at,omitempty"`
}

type PhoneBridge struct {
	mu                     sync.Mutex
	conn                   *websocket.Conn
	connected              bool
	platform               string
	configuredPlatform     string
	phoneID                string
	lastHeartbeatAt        time.Time
	appState               string
	appStateAt             time.Time
	returnEntry            string
	returnEntryOK          bool
	returnEntrySeen        bool
	pipBridgeEnabled       bool
	pipBridgeSeen          bool
	fgsBridgeEnabled       bool
	fgsBridgeSeen          bool
	fgsBridgeAt            time.Time
	environment            *PhoneEnvironment
	environmentAt          time.Time
	hidConnectionState     func() (connected, known bool)
	hidConnected           bool
	hidConnectionKnown     bool
	hidConnectionID        string
	hidConnectionSeq       uint64
	hidConnectionPhone     string
	hidConnectionCheckedAt time.Time
	hidMonitorEnabled      bool
	hidMonitorMu           sync.Mutex
	hidMonitorRunning      bool
	hidMonitorStop         chan struct{}
	hidMonitorStopOnce     sync.Once
	closeOnce              sync.Once

	screenCache         screen.PhoneScreenInfo
	screenCachePhoneID  string
	screenCacheHIDID    string
	screenCacheAt       time.Time
	environmentObserver func(*PhoneEnvironment)
	environmentNotifyMu sync.Mutex

	clipboardText      string
	clipboardAt        time.Time
	pendingCmds        map[string]chan BridgeCommandResponse
	logger             *Logger
	done               chan struct{}
	queue              *CommandQueue // HTTP queue for background-compatible commands
	bleWake            func(context.Context, string) error
	bleStatus          func(context.Context) (ble.RuntimeStatus, error)
	bleCapabilityMu    sync.Mutex
	bleCapabilityCache phoneBridgeBLECapabilities
	bleCapabilityAt    time.Time

	// Proxy mode fields
	proxyMode     bool
	proxyEndpoint string
	proxyTaskID   string
	proxyClient   *http.Client
}

func NewPhoneBridge(logger *Logger) *PhoneBridge {
	return &PhoneBridge{
		pendingCmds:        make(map[string]chan BridgeCommandResponse),
		logger:             logger,
		queue:              NewCommandQueue(logger),
		bleWake:            defaultBLEWake,
		bleStatus:          defaultBLEStatus,
		hidConnectionState: readHIDConnectionState,
		hidMonitorEnabled:  true,
		hidMonitorStop:     make(chan struct{}),
		proxyClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// NewPhoneBridgeProxy creates a PhoneBridge in proxy mode that forwards to a remote agent.
func NewPhoneBridgeProxy(endpoint, taskID string, logger *Logger) *PhoneBridge {
	pb := NewPhoneBridge(logger)
	pb.proxyMode = true
	pb.proxyEndpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	pb.proxyTaskID = strings.TrimSpace(taskID)
	return pb
}

// Close stops background Phone Bridge monitoring. It is safe to call more than
// once, including when runtime shutdown races with test cleanup.
func (pb *PhoneBridge) Close() {
	if pb == nil {
		return
	}
	pb.hidMonitorMu.Lock()
	stop := pb.hidMonitorStop
	if stop == nil {
		stop = make(chan struct{})
		pb.hidMonitorStop = stop
	}
	pb.hidMonitorMu.Unlock()
	pb.closeOnce.Do(func() {
		pb.hidMonitorStopOnce.Do(func() {
			close(stop)
		})
	})
}

// SetEnvironmentObserver keeps consumers such as screenshot cropping in sync
// with either the live phone environment or a same-phone screen-size fallback.
func (pb *PhoneBridge) SetEnvironmentObserver(observer func(*PhoneEnvironment)) {
	if pb == nil {
		return
	}
	pb.ensureHIDConnectionMonitor()
	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	pb.environmentObserver = observer
	pb.mu.Unlock()
	pb.notifyEnvironmentObserver()
}

// SetConfiguredPlatform supplies the device_type-derived platform used before
// the companion app has had a chance to report its platform over WebSocket or
// HTTP polling. A live app report always takes precedence.
func (pb *PhoneBridge) SetConfiguredPlatform(platform string) {
	if pb == nil {
		return
	}
	platform = phoneBridgePlatform(PhoneBridgeStatus{Platform: platform})
	if platform != "ios" && platform != "android" {
		platform = ""
	}
	pb.mu.Lock()
	pb.configuredPlatform = platform
	pb.mu.Unlock()
}

func defaultBLEWake(ctx context.Context, reason string) error {
	result, err := ble.RequestWake(ctx, configuredBLEServiceSocketPath(), reason)
	if err != nil {
		return err
	}
	if !result.Delivered {
		return errors.New("BLE wake has no active subscriber")
	}
	return nil
}

func defaultBLEStatus(ctx context.Context) (ble.RuntimeStatus, error) {
	return ble.RequestStatus(ctx, configuredBLEServiceSocketPath())
}

type phoneBridgeBLECapabilities struct {
	Wake              bool
	NotificationQuery bool
}

func (pb *PhoneBridge) bleCapabilities(ctx context.Context) phoneBridgeBLECapabilities {
	if pb == nil || pb.bleStatus == nil {
		return phoneBridgeBLECapabilities{}
	}
	pb.bleCapabilityMu.Lock()
	defer pb.bleCapabilityMu.Unlock()
	if !pb.bleCapabilityAt.IsZero() && time.Since(pb.bleCapabilityAt) < phoneBridgeBLECacheTTL {
		return pb.bleCapabilityCache
	}
	status, err := pb.bleStatus(ctx)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Warn("phone-bridge: BLE capability status unavailable: %v", err)
		}
		pb.bleCapabilityCache = phoneBridgeBLECapabilities{}
		pb.bleCapabilityAt = time.Now()
		return pb.bleCapabilityCache
	}
	pb.bleCapabilityCache = phoneBridgeBLECapabilities{
		Wake:              status.BackendAvailable && status.Connected && status.WakeSubscriber,
		NotificationQuery: status.EventCount > 0 || (status.BackendAvailable && status.Connected),
	}
	pb.bleCapabilityAt = time.Now()
	return pb.bleCapabilityCache
}

func (pb *PhoneBridge) bleWakeAvailable(ctx context.Context) bool {
	return pb.bleCapabilities(ctx).Wake
}

func (pb *PhoneBridge) notifyBLEWake(cmd BridgeCommand) {
	if pb == nil || pb.bleWake == nil || !phoneBridgeShouldNotifyBLEWake(pb.getStatus(), cmd.Type) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := pb.bleWake(ctx, "phone_bridge"); err != nil && pb.logger != nil {
			pb.logger.Warn("phone-bridge: BLE wake unavailable: %v", err)
		}
	}()
}

func (pb *PhoneBridge) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get(phoneBridgeBenchmarkRelayQuery) == "1" {
		pb.handleBenchmarkWebSocketRelay(w, r)
		return
	}

	// Proxy mode: forward WebSocket to remote agent
	if pb.proxyMode {
		pb.handleWebSocketProxy(w, r)
		return
	}

	// Local mode: handle WebSocket directly
	// Leave origin verification on (the library default): a request with an
	// empty Origin header — i.e. the native companion app — is accepted, while
	// a cross-origin browser handshake is rejected. This blocks cross-site
	// WebSocket hijacking, where a malicious page would otherwise connect and
	// impersonate the app by sending BridgeCommandResponse messages.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: websocket accept failed: %v", err)
		}
		return
	}

	platform := r.URL.Query().Get("platform")
	phoneID := strings.TrimSpace(r.URL.Query().Get("phone_id"))

	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	if pb.conn != nil {
		oldConn := pb.conn
		pb.mu.Unlock()
		oldConn.Close(websocket.StatusGoingAway, "new connection")
		pb.mu.Lock()
	}
	pb.conn = conn
	pb.connected = true
	pb.platform = platform
	pb.updateHIDConnectionPhoneLocked(phoneID)
	pb.phoneID = phoneID
	pb.lastHeartbeatAt = time.Now()
	pb.environment = nil
	pb.environmentAt = time.Time{}
	pb.done = make(chan struct{})
	done := pb.done
	pb.mu.Unlock()
	pb.notifyEnvironmentObserver()

	if pb.logger != nil {
		pb.logger.Info("phone-bridge: client connected (platform=%s phone_id=%s)", platform, phoneID)
	}

	go pb.monitorHeartbeat(conn, done)
	pb.readLoop(conn, done)
}

// monitorHeartbeat tears down a connection that stops sending heartbeats. The
// readLoop's blocking conn.Read never returns on a half-open socket, so without
// this the bridge would keep reporting connected:true while every command
// silently times out. Closing the conn here unblocks readLoop, which performs
// the actual cleanup in its deferred handler.
func (pb *PhoneBridge) monitorHeartbeat(conn *websocket.Conn, done chan struct{}) {
	ticker := time.NewTicker(heartbeatCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			pb.mu.Lock()
			stale := pb.conn == conn && time.Since(pb.lastHeartbeatAt) > heartbeatTimeout
			pb.mu.Unlock()
			if stale {
				if pb.logger != nil {
					pb.logger.Error("phone-bridge: heartbeat timeout, closing connection")
				}
				conn.Close(websocket.StatusGoingAway, "heartbeat timeout")
				return
			}
		}
	}
}

func (pb *PhoneBridge) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer func() {
		cleared := false
		pb.mu.Lock()
		if pb.conn == conn {
			pb.conn = nil
			pb.connected = false
			pb.phoneID = ""
			pb.environment = nil
			pb.environmentAt = time.Time{}
			cleared = true
			for id, ch := range pb.pendingCmds {
				close(ch)
				delete(pb.pendingCmds, id)
			}
		}
		pb.mu.Unlock()
		if cleared {
			pb.notifyEnvironmentObserver()
		}
		close(done)
		conn.Close(websocket.StatusNormalClosure, "")
		if pb.logger != nil {
			pb.logger.Info("phone-bridge: client disconnected")
		}
	}()

	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			if pb.logger != nil && websocket.CloseStatus(err) == -1 {
				pb.logger.Error("phone-bridge: read error: %v", err)
			}
			return
		}

		var resp BridgeCommandResponse
		if err := json.Unmarshal(data, &resp); err != nil {
			if pb.logger != nil {
				pb.logger.Error("phone-bridge: invalid message: %v", err)
			}
			continue
		}

		if pb.handleAppEvent(resp) {
			continue
		}

		// Echo heartbeat/ping back to the app so it can reset its missedPongs counter.
		// Without this, the app forces a reconnect every HEARTBEAT_INTERVAL * MAX_MISSED_PONGS.
		isHeartbeat := resp.ID == "heartbeat" || resp.ID == "ping"
		if isHeartbeat {
			if echoData, err := json.Marshal(resp); err == nil {
				_ = conn.Write(context.Background(), websocket.MessageText, echoData)
			}
		}

		pb.mu.Lock()
		if isHeartbeat {
			pb.lastHeartbeatAt = time.Now()
		}

		if ch, ok := pb.pendingCmds[resp.ID]; ok {
			select {
			case ch <- resp:
			default:
			}
			delete(pb.pendingCmds, resp.ID)
		}
		pb.mu.Unlock()
	}
}

func (pb *PhoneBridge) handleAppEvent(resp BridgeCommandResponse) bool {
	switch {
	case resp.ID == "phone_environment" || resp.Method == "phone_environment":
		return pb.handleEnvironmentEvent(resp)
	case resp.ID == "phone_app_state" || resp.Method == "phone_app_state":
		return pb.handleAppStateEvent(resp)
	default:
		return false
	}
}

func (pb *PhoneBridge) handleEnvironmentEvent(resp BridgeCommandResponse) bool {
	if resp.Error != nil {
		if pb.logger != nil {
			pb.logger.Warn("phone-bridge: environment report failed: code=%s msg=%s",
				resp.Error.Code, resp.Error.Message)
		}
		return true
	}
	if len(resp.Data) == 0 {
		if pb.logger != nil {
			pb.logger.Warn("phone-bridge: environment report missing data")
		}
		return true
	}

	var env PhoneEnvironment
	if err := json.Unmarshal(resp.Data, &env); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: decode environment report failed: %v", err)
		}
		return true
	}

	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	if strings.TrimSpace(env.Platform) == "" {
		env.Platform = pb.platform
	}
	pb.environment = &env
	pb.environmentAt = time.Now()
	pb.lastHeartbeatAt = pb.environmentAt
	if hasPhoneScreenDimensions(env.Screen) &&
		(pb.phoneID != "" || (pb.hidConnectionKnown && pb.hidConnected)) {
		pb.screenCache = env.Screen
		pb.screenCachePhoneID = pb.phoneID
		pb.screenCacheHIDID = pb.hidConnectionID
		pb.screenCacheAt = pb.environmentAt
	}
	pb.mu.Unlock()
	pb.notifyEnvironmentObserver()

	if pb.logger != nil {
		pb.logger.Info("phone-bridge: environment updated (platform=%s)", env.Platform)
	}
	return true
}

func (pb *PhoneBridge) handleAppStateEvent(resp BridgeCommandResponse) bool {
	if resp.Error != nil {
		if pb.logger != nil {
			pb.logger.Warn("phone-bridge: app state report failed: code=%s msg=%s",
				resp.Error.Code, resp.Error.Message)
		}
		return true
	}
	var payload struct {
		AppState             string `json:"app_state"`
		ReportedAt           string `json:"reported_at,omitempty"`
		ReturnEntry          string `json:"return_entry,omitempty"`
		ReturnEntryAvailable *bool  `json:"return_entry_available,omitempty"`
		PipBridgeEnabled     *bool  `json:"pip_bridge_enabled,omitempty"`
	}
	if err := json.Unmarshal(resp.Data, &payload); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge: decode app state report failed: %v", err)
		}
		return true
	}
	appState, ok := normalizeAppState(payload.AppState)
	if !ok {
		if pb.logger != nil {
			pb.logger.Warn("phone-bridge: ignoring unknown app_state=%q", payload.AppState)
		}
		return true
	}
	appStateAt := time.Now()
	if ts := strings.TrimSpace(payload.ReportedAt); ts != "" {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			appStateAt = parsed
		} else if pb.logger != nil {
			pb.logger.Warn("phone-bridge: invalid reported_at=%q: %v", payload.ReportedAt, err)
		}
	}
	pb.mu.Lock()
	pb.appState = appState
	pb.appStateAt = appStateAt
	if payload.ReturnEntryAvailable != nil {
		pb.returnEntryOK = *payload.ReturnEntryAvailable
		pb.returnEntrySeen = true
	}
	pb.returnEntry = strings.ToLower(strings.TrimSpace(payload.ReturnEntry))
	if payload.PipBridgeEnabled != nil {
		pb.pipBridgeEnabled = *payload.PipBridgeEnabled
		pb.pipBridgeSeen = true
	}
	pb.lastHeartbeatAt = pb.appStateAt
	pb.mu.Unlock()

	if pb.logger != nil {
		pb.logger.Info("phone-bridge: app state updated (%s)", appState)
	}
	return true
}

func (pb *PhoneBridge) SendQueuedCommand(ctx context.Context, cmd BridgeCommand) (BridgeCommandResponse, error) {
	if cmd.ID == "" {
		return BridgeCommandResponse{}, fmt.Errorf("command ID must not be empty")
	}
	if pb.proxyMode {
		return pb.sendProxyQueuedCommand(ctx, cmd)
	}
	if pb.queue == nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeNotConnected, "phone bridge command queue is not available"),
		}, nil
	}
	if strings.TrimSpace(cmd.PhoneID) == "" {
		cmd.PhoneID = pb.currentPhoneID()
	}
	if err := pb.queue.Enqueue(cmd); err != nil {
		if errors.Is(err, ErrCommandExists) {
			return BridgeCommandResponse{
				ID: cmd.ID,
				Error: NewToolErrorWithDetails(CodeCommandIDCollision,
					fmt.Sprintf("duplicate command ID %q is already queued or has a retained result", cmd.ID),
					map[string]any{"command_id": cmd.ID}),
			}, nil
		}
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeToolExecutionFailed, fmt.Sprintf("enqueue command: %v", err)),
		}, nil
	}
	pb.notifyBLEWake(cmd)

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

	for {
		result, status := pb.queue.QueryResult(cmd.ID)
		switch status {
		case StatusCompleted:
			if result != nil {
				return result.Response, nil
			}
		case StatusExpired:
			return BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeBridgeTimeout, "queued command expired before result"),
			}, nil
		}

		select {
		case <-waitCtx.Done():
			if result, status := pb.queue.QueryResult(cmd.ID); status == StatusCompleted && result != nil {
				return result.Response, nil
			}
			pb.queue.Cancel(cmd.ID)
			if ctx.Err() != nil {
				return BridgeCommandResponse{}, ctx.Err()
			}
			return BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeBridgeTimeout, "queued command timeout"),
			}, nil
		case <-ticker.C:
		}
	}
}

func (pb *PhoneBridge) SendCommand(ctx context.Context, cmd BridgeCommand) (BridgeCommandResponse, error) {
	if cmd.ID == "" {
		return BridgeCommandResponse{}, fmt.Errorf("command ID must not be empty")
	}
	if pb.proxyMode {
		return pb.sendProxyWebSocketCommand(ctx, cmd)
	}
	pb.mu.Lock()
	if !pb.connected || pb.conn == nil {
		pb.mu.Unlock()
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeNotConnected, "phone bridge not connected"),
		}, nil
	}
	if _, exists := pb.pendingCmds[cmd.ID]; exists {
		pb.mu.Unlock()
		return BridgeCommandResponse{
			ID: cmd.ID,
			Error: NewToolErrorWithDetails(CodeCommandIDCollision,
				fmt.Sprintf("duplicate command ID %q already in flight", cmd.ID),
				map[string]any{"command_id": cmd.ID}),
		}, nil
	}
	ch := make(chan BridgeCommandResponse, 1)
	pb.pendingCmds[cmd.ID] = ch
	defer func() {
		pb.mu.Lock()
		delete(pb.pendingCmds, cmd.ID)
		pb.mu.Unlock()
	}()
	conn := pb.conn
	if strings.TrimSpace(cmd.PhoneID) == "" {
		cmd.PhoneID = pb.phoneID
	}
	pb.mu.Unlock()

	data, err := json.Marshal(cmd)
	if err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeCommandMarshalFailed, fmt.Sprintf("marshal command: %v", err)),
		}, nil
	}

	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeWriteFailed, fmt.Sprintf("write command: %v", err)),
		}, nil
	}

	timeout := 5 * time.Second
	if cmd.TimeoutMs > 0 {
		timeout = time.Duration(cmd.TimeoutMs) * time.Millisecond
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeBridgeConnectionClosed, "connection closed before response"),
			}, nil
		}
		return resp, nil
	case <-time.After(timeout):
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeTimeout, "command timeout"),
		}, nil
	case <-ctx.Done():
		return BridgeCommandResponse{}, ctx.Err()
	}
}

func (pb *PhoneBridge) Connected() bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.connected
}

func (pb *PhoneBridge) NoteClipboardWrite(text string) {
	if pb == nil {
		return
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.clipboardText = text
	pb.clipboardAt = time.Now()
}

func (pb *PhoneBridge) ClipboardRecentlyContains(text string, maxAge time.Duration) bool {
	if pb == nil || maxAge <= 0 {
		return false
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if pb.clipboardAt.IsZero() || time.Since(pb.clipboardAt) > maxAge {
		return false
	}
	return strings.TrimSpace(pb.clipboardText) == strings.TrimSpace(text)
}

func (pb *PhoneBridge) ClipboardRecentlyEquals(text string, maxAge time.Duration) bool {
	if pb == nil || maxAge <= 0 {
		return false
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if pb.clipboardAt.IsZero() || time.Since(pb.clipboardAt) > maxAge {
		return false
	}
	return pb.clipboardText == text
}

func (pb *PhoneBridge) currentPhoneID() string {
	if pb == nil {
		return ""
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return strings.TrimSpace(pb.phoneID)
}

func (pb *PhoneBridge) UpdateState() map[string]string {
	ret := make(map[string]string)
	if pb == nil {
		return ret
	}

	// Serialize snapshot publication so an older status callback cannot publish
	// after a newer HTTP poll or WebSocket event. The freshness expiry callback
	// uses the same path.
	status := pb.getStatus()
	// update stateManager with the new status
	// if connected, set app_connected to true
	if status.Connected {
		ret["app_connected"] = "true"
	} else {
		ret["app_connected"] = "false"
	}

	// Empty values overwrite stale state-manager entries and are omitted from
	// the model-facing <state> message by the state hook.
	// Clear app_state when disconnected and stale (no recent HTTP polling either).
	// This prevents showing "connecting" state when the phone is physically unplugged.
	// When app goes to background normally, HTTP polling keeps appStateAt fresh.
	appState := strings.TrimSpace(status.AppState)
	if !status.Connected && appState != "" {
		if status.AppStateUpdatedAt == nil || time.Since(*status.AppStateUpdatedAt) > phoneBridgeBackgroundStateMaxAge {
			appState = ""
		}
	}
	ret["app_state"] = appState

	// Expose the transport modes reported by the companion app. PiP mode is an
	// explicit iOS capability signal and remains usable for background-safe data
	// commands even when ordinary app-state timestamps stop refreshing.
	ret["app_pip_enabled"] = fmt.Sprintf("%t", phoneBridgeCanUsePiPBackground(status, "clipboard_read"))
	ret["app_fgs_enabled"] = fmt.Sprintf("%t", phoneBridgeCanUseFGSBackground(status, "clipboard_read"))

	platform := strings.ToLower(strings.TrimSpace(status.Platform))
	if platform == "ios" || platform == "android" {
		ret["app_platform"] = platform
	} else {
		ret["app_platform"] = ""
	}

	return ret
}

func (pb *PhoneBridge) getStatus() PhoneBridgeStatus {
	// Proxy mode: fetch status from remote agent
	if pb.proxyMode {
		return pb.getProxyStatus()
	}

	pb.refreshHIDConnection()
	pb.mu.Lock()
	defer pb.mu.Unlock()
	platform := pb.platform
	if strings.TrimSpace(platform) == "" {
		platform = pb.configuredPlatform
	}
	status := PhoneBridgeStatus{
		Connected:       pb.connected,
		Platform:        platform,
		PhoneID:         pb.phoneID,
		HIDConnectionID: pb.hidConnectionID,
	}
	if pb.connected && !pb.lastHeartbeatAt.IsZero() {
		t := pb.lastHeartbeatAt
		status.LastHeartbeatAt = &t
	}
	if pb.appState != "" {
		status.AppState = pb.appState
		if !pb.appStateAt.IsZero() {
			t := pb.appStateAt
			status.AppStateUpdatedAt = &t
		}
	}
	if pb.returnEntrySeen {
		v := pb.returnEntryOK
		status.ReturnEntryAvailable = &v
	}
	if pb.returnEntry != "" {
		status.ReturnEntry = pb.returnEntry
	}
	if pb.pipBridgeSeen {
		enabled := pb.pipBridgeEnabled
		status.PipBridgeEnabled = &enabled
	}
	if pb.fgsBridgeSeen {
		enabled := pb.fgsBridgeEnabled
		status.FgsBridgeEnabled = &enabled
		if !pb.fgsBridgeAt.IsZero() {
			t := pb.fgsBridgeAt
			status.FgsBridgeUpdatedAt = &t
		}
	}
	if env, updatedAt := pb.environmentForStatusLocked(); env != nil {
		status.Environment = env
		if !updatedAt.IsZero() {
			t := updatedAt
			status.EnvironmentUpdatedAt = &t
		}
	}
	return status
}

func (pb *PhoneBridge) environmentForStatusLocked() (*PhoneEnvironment, time.Time) {
	if pb.connected && pb.environment != nil {
		env := clonePhoneEnvironment(*pb.environment)
		if !hasPhoneScreenDimensions(env.Screen) && pb.hasMatchingScreenCacheLocked() {
			env.Screen = pb.screenCache
			env.Source = "phone-bridge-screen-cache"
		}
		return &env, pb.environmentAt
	}
	if !pb.hasMatchingScreenCacheLocked() {
		return nil, time.Time{}
	}
	platform := pb.platform
	if strings.TrimSpace(platform) == "" {
		platform = pb.configuredPlatform
	}
	env := PhoneEnvironment{
		Source:   "phone-bridge-screen-cache",
		Platform: platform,
		Screen:   pb.screenCache,
	}
	return &env, pb.screenCacheAt
}

func (pb *PhoneBridge) hasMatchingScreenCacheLocked() bool {
	if pb.hidConnectionKnown {
		matchesPhone := pb.screenCachePhoneID == "" ||
			(pb.hidConnectionPhone != "" && pb.hidConnectionPhone == pb.screenCachePhoneID)
		return matchesPhone && pb.hidConnected &&
			pb.hidConnectionID != "" &&
			pb.hidConnectionID == pb.screenCacheHIDID &&
			hasPhoneScreenDimensions(pb.screenCache)
	}
	return pb.phoneID != "" &&
		pb.phoneID == pb.screenCachePhoneID &&
		hasPhoneScreenDimensions(pb.screenCache)
}

// ScreenCacheScopeID identifies the physical HID session that owns cached
// phone screen dimensions. Desktop builds without a UDC sysfs view retain the
// previous phone_id scope.
func (pb *PhoneBridge) ScreenCacheScopeID() string {
	if pb == nil {
		return ""
	}
	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if pb.hidConnectionKnown {
		if pb.hidConnected {
			return pb.hidConnectionID
		}
		return ""
	}
	if phoneID := strings.TrimSpace(pb.phoneID); phoneID != "" {
		return "phone:" + phoneID
	}
	return ""
}

func (pb *PhoneBridge) refreshHIDConnection() {
	pb.refreshHIDConnectionWithForce(false)
}

func (pb *PhoneBridge) refreshHIDConnectionNow() {
	pb.refreshHIDConnectionWithForce(true)
}

func (pb *PhoneBridge) refreshHIDConnectionWithForce(force bool) {
	if pb == nil {
		return
	}
	pb.mu.Lock()
	state := pb.hidConnectionState
	if !force && !pb.hidConnectionCheckedAt.IsZero() &&
		time.Since(pb.hidConnectionCheckedAt) < phoneBridgeHIDConnectionPollInterval {
		pb.mu.Unlock()
		return
	}
	pb.mu.Unlock()
	if state == nil {
		return
	}
	connected, known := state()
	pb.mu.Lock()
	pb.hidConnectionCheckedAt = time.Now()
	pb.applyHIDConnectionStateLocked(connected, known)
	pb.mu.Unlock()
}

func (pb *PhoneBridge) applyHIDConnectionStateLocked(connected, known bool) {
	if !known {
		return
	}
	pb.hidConnectionKnown = true
	if connected {
		if pb.hidConnected && pb.hidConnectionID != "" {
			return
		}
		pb.hidConnected = true
		pb.hidConnectionSeq++
		pb.hidConnectionID = fmt.Sprintf("hid-%d-%d", time.Now().UnixNano(), pb.hidConnectionSeq)
		return
	}
	if !pb.hidConnected && pb.hidConnectionID == "" && pb.screenCacheHIDID == "" {
		return
	}
	pb.hidConnected = false
	pb.hidConnectionID = ""
	pb.hidConnectionPhone = ""
	pb.environment = nil
	pb.environmentAt = time.Time{}
	pb.screenCache = screen.PhoneScreenInfo{}
	pb.screenCachePhoneID = ""
	pb.screenCacheHIDID = ""
	pb.screenCacheAt = time.Time{}
}

// HIDConnectionPhoneID returns the phone last identified on the current USB HID
// connection. Unlike the Phone Bridge phone_id it outlives a WebSocket
// disconnect and clears only when HID disconnects.
func (pb *PhoneBridge) HIDConnectionPhoneID() string {
	if pb == nil {
		return ""
	}
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.hidConnectionPhone
}

func (pb *PhoneBridge) updateHIDConnectionPhoneLocked(phoneID string) {
	phoneID = strings.TrimSpace(phoneID)
	if phoneID == "" || !pb.hidConnectionKnown || !pb.hidConnected {
		return
	}
	if pb.hidConnectionPhone != "" && pb.hidConnectionPhone != phoneID {
		pb.screenCache = screen.PhoneScreenInfo{}
		pb.screenCachePhoneID = ""
		pb.screenCacheHIDID = ""
		pb.screenCacheAt = time.Time{}
	}
	pb.hidConnectionPhone = phoneID
}

func hasPhoneScreenDimensions(info screen.PhoneScreenInfo) bool {
	switch {
	case info.WidthPixels != nil && info.HeightPixels != nil && *info.WidthPixels > 0 && *info.HeightPixels > 0:
		return true
	case info.NativeWidthPixels != nil && info.NativeHeightPixels != nil && *info.NativeWidthPixels > 0 && *info.NativeHeightPixels > 0:
		return true
	case info.Width != nil && info.Height != nil && *info.Width > 0 && *info.Height > 0:
		return true
	default:
		return false
	}
}

func notifyPhoneEnvironmentObserver(observer func(*PhoneEnvironment), env *PhoneEnvironment) {
	if observer == nil {
		return
	}
	if env == nil || !hasPhoneScreenDimensions(env.Screen) {
		observer(nil)
		return
	}
	copyEnv := clonePhoneEnvironment(*env)
	observer(&copyEnv)
}

func (pb *PhoneBridge) notifyEnvironmentObserver() {
	if pb == nil {
		return
	}
	pb.ensureHIDConnectionMonitor()
	pb.refreshHIDConnectionNow()
	pb.environmentNotifyMu.Lock()
	defer pb.environmentNotifyMu.Unlock()

	pb.mu.Lock()
	observer := pb.environmentObserver
	env, _ := pb.environmentForStatusLocked()
	pb.mu.Unlock()
	notifyPhoneEnvironmentObserver(observer, env)
}

func clonePhoneEnvironment(env PhoneEnvironment) PhoneEnvironment {
	if len(env.SystemApps) > 0 {
		env.SystemApps = append([]AvailableAppInfo(nil), env.SystemApps...)
	}
	if len(env.ThirdPartyApps) > 0 {
		env.ThirdPartyApps = append([]AvailableAppInfo(nil), env.ThirdPartyApps...)
	}
	if len(env.AvailableApps) > 0 {
		env.AvailableApps = append([]AvailableAppInfo(nil), env.AvailableApps...)
	}
	return env
}

// ApplyBenchmarkStatus replaces the runtime Phone Bridge state for benchmark-only
// daemon workers. The caller is responsible for restricting access to a
// benchmark-authenticated endpoint. Freshness timestamps are set to now so
// PiP/FGS policy tests remain deterministic for the following task.
func (pb *PhoneBridge) ApplyBenchmarkStatus(status PhoneBridgeStatus) error {
	if pb == nil {
		return fmt.Errorf("phone bridge is not configured")
	}
	platform := strings.ToLower(strings.TrimSpace(status.Platform))
	if platform != "" && platform != "ios" && platform != "android" {
		return fmt.Errorf("unsupported platform %q", status.Platform)
	}
	appState := ""
	if strings.TrimSpace(status.AppState) != "" {
		var ok bool
		appState, ok = normalizeAppState(status.AppState)
		if !ok {
			return fmt.Errorf("unsupported app_state %q", status.AppState)
		}
	}
	now := time.Now()

	pb.refreshHIDConnectionNow()
	pb.mu.Lock()
	pb.updateHIDConnectionPhoneLocked(status.PhoneID)
	pb.connected = status.Connected
	pb.platform = platform
	pb.phoneID = strings.TrimSpace(status.PhoneID)
	if status.Connected {
		pb.lastHeartbeatAt = now
	} else {
		pb.lastHeartbeatAt = time.Time{}
	}
	pb.appState = appState
	if appState != "" {
		pb.appStateAt = now
	} else {
		pb.appStateAt = time.Time{}
	}
	pb.returnEntry = strings.ToLower(strings.TrimSpace(status.ReturnEntry))
	pb.returnEntrySeen = status.ReturnEntryAvailable != nil
	if status.ReturnEntryAvailable != nil {
		pb.returnEntryOK = *status.ReturnEntryAvailable
	} else {
		pb.returnEntryOK = false
	}
	pb.pipBridgeSeen = status.PipBridgeEnabled != nil
	if status.PipBridgeEnabled != nil {
		pb.pipBridgeEnabled = *status.PipBridgeEnabled
	} else {
		pb.pipBridgeEnabled = false
	}
	pb.fgsBridgeSeen = status.FgsBridgeEnabled != nil
	if status.FgsBridgeEnabled != nil {
		pb.fgsBridgeEnabled = *status.FgsBridgeEnabled
		pb.fgsBridgeAt = now
	} else {
		pb.fgsBridgeEnabled = false
		pb.fgsBridgeAt = time.Time{}
	}
	if status.Environment != nil {
		env := clonePhoneEnvironment(*status.Environment)
		if strings.TrimSpace(env.Platform) == "" {
			env.Platform = platform
		}
		pb.environment = &env
		pb.environmentAt = now
		if hasPhoneScreenDimensions(env.Screen) &&
			(pb.phoneID != "" || (pb.hidConnectionKnown && pb.hidConnected)) {
			pb.screenCache = env.Screen
			pb.screenCachePhoneID = pb.phoneID
			pb.screenCacheHIDID = pb.hidConnectionID
			pb.screenCacheAt = now
		}
	} else {
		pb.environment = nil
		pb.environmentAt = time.Time{}
	}
	pb.mu.Unlock()
	pb.notifyEnvironmentObserver()

	return nil
}

// handleBenchmarkWebSocketRelay serves an outbound connection from a benchmark
// daemon. The device-side Agent remains the sole owner of the App WebSocket;
// SendCommand multiplexes relay commands onto that connection and readLoop
// delivers the matching App responses back here.
func (pb *PhoneBridge) handleBenchmarkWebSocketRelay(w http.ResponseWriter, r *http.Request) {
	if pb.proxyMode {
		http.Error(w, "nested phone bridge proxy is not supported", http.StatusBadGateway)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-relay: accept benchmark connection failed: %v", err)
		}
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	taskID := strings.TrimSpace(r.Header.Get("benchmark-task-id"))
	if pb.logger != nil {
		pb.logger.Info("phone-bridge-relay: benchmark connected (task_id=%s)", taskID)
		defer pb.logger.Info("phone-bridge-relay: benchmark disconnected (task_id=%s)", taskID)
	}

	for {
		msgType, data, err := conn.Read(r.Context())
		if err != nil {
			if websocket.CloseStatus(err) == -1 && pb.logger != nil {
				pb.logger.Error("phone-bridge-relay: benchmark read failed: %v", err)
			}
			return
		}

		var cmd BridgeCommand
		if err := json.Unmarshal(data, &cmd); err != nil {
			resp := BridgeCommandResponse{
				Error: NewToolError(CodeInvalidArguments, fmt.Sprintf("decode relayed phone bridge command: %v", err)),
			}
			if writeErr := writeBenchmarkRelayResponse(r.Context(), conn, msgType, resp); writeErr != nil {
				return
			}
			continue
		}

		resp, sendErr := pb.SendCommand(r.Context(), cmd)
		if sendErr != nil {
			resp = BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeToolExecutionFailed, sendErr.Error()),
			}
		}
		if err := writeBenchmarkRelayResponse(r.Context(), conn, msgType, resp); err != nil {
			if pb.logger != nil {
				pb.logger.Error("phone-bridge-relay: benchmark write failed: %v", err)
			}
			return
		}
	}
}

func writeBenchmarkRelayResponse(ctx context.Context, conn *websocket.Conn, msgType websocket.MessageType, resp BridgeCommandResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	return conn.Write(ctx, msgType, data)
}

// sendProxyWebSocketCommand dials the device-side environment bridge. That
// bridge owns the App WebSocket and relays this command and its response, so the
// benchmark daemon observes the same command semantics as a directly connected
// Agent without requiring an inbound connection from the device.
func (pb *PhoneBridge) sendProxyWebSocketCommand(ctx context.Context, cmd BridgeCommand) (BridgeCommandResponse, error) {
	remoteURL := pb.proxyEndpoint + "/api/phone-bridge"
	if strings.HasPrefix(remoteURL, "http://") {
		remoteURL = "ws://" + strings.TrimPrefix(remoteURL, "http://")
	} else if strings.HasPrefix(remoteURL, "https://") {
		remoteURL = "wss://" + strings.TrimPrefix(remoteURL, "https://")
	}
	params := url.Values{}
	params.Set(phoneBridgeBenchmarkRelayQuery, "1")
	remoteURL += "?" + params.Encode()

	headers := http.Header{}
	if pb.proxyTaskID != "" {
		headers.Set("benchmark-task-id", pb.proxyTaskID)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, _, err := websocket.Dial(dialCtx, remoteURL, &websocket.DialOptions{HTTPHeader: headers})
	cancel()
	if err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeNotConnected, fmt.Sprintf("connect phone bridge relay: %v", err)),
		}, nil
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	data, err := json.Marshal(cmd)
	if err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeCommandMarshalFailed, fmt.Sprintf("marshal command: %v", err)),
		}, nil
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeWriteFailed, fmt.Sprintf("write relayed command: %v", err)),
		}, nil
	}

	timeout := 5 * time.Second
	if cmd.TimeoutMs > 0 {
		timeout = time.Duration(cmd.TimeoutMs) * time.Millisecond
	}
	readCtx, readCancel := context.WithTimeout(ctx, timeout)
	defer readCancel()
	_, responseData, err := conn.Read(readCtx)
	if err != nil {
		if readCtx.Err() != nil && ctx.Err() == nil {
			return BridgeCommandResponse{
				ID:    cmd.ID,
				Error: NewToolError(CodeBridgeTimeout, "relayed command timeout"),
			}, nil
		}
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeBridgeConnectionClosed, fmt.Sprintf("read relayed command response: %v", err)),
		}, nil
	}

	var resp BridgeCommandResponse
	if err := json.Unmarshal(responseData, &resp); err != nil {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeToolExecutionFailed, fmt.Sprintf("decode relayed command response: %v", err)),
		}, nil
	}
	return resp, nil
}

// handleWebSocketProxy forwards WebSocket connection to remote agent in proxy mode.
func (pb *PhoneBridge) handleWebSocketProxy(w http.ResponseWriter, r *http.Request) {
	// Accept connection from aiden app
	appConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: accept app connection failed: %v", err)
		}
		return
	}

	platform := r.URL.Query().Get("platform")
	phoneID := r.URL.Query().Get("phone_id")

	if pb.logger != nil {
		pb.logger.Info("phone-bridge-proxy: app connected (platform=%s phone_id=%s)", platform, phoneID)
	}

	// Build remote agent WebSocket URL
	remoteURL := pb.proxyEndpoint + "/api/phone-bridge"
	if strings.HasPrefix(remoteURL, "http://") {
		remoteURL = "ws://" + strings.TrimPrefix(remoteURL, "http://")
	} else if strings.HasPrefix(remoteURL, "https://") {
		remoteURL = "wss://" + strings.TrimPrefix(remoteURL, "https://")
	}

	// Add query parameters
	if platform != "" || phoneID != "" {
		params := url.Values{}
		if platform != "" {
			params.Set("platform", platform)
		}
		if phoneID != "" {
			params.Set("phone_id", phoneID)
		}
		remoteURL = remoteURL + "?" + params.Encode()
	}

	// Connect to remote agent
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	agentConn, _, err := websocket.Dial(ctx, remoteURL, nil)
	cancel()
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: connect to remote agent failed: %v", err)
		}
		appConn.Close(websocket.StatusInternalError, "failed to connect to remote agent")
		return
	}

	if pb.logger != nil {
		pb.logger.Info("phone-bridge-proxy: connected to remote agent %s", remoteURL)
	}

	// Bidirectional message forwarding
	var wg sync.WaitGroup
	wg.Add(2)

	proxyCtx, proxyCancel := context.WithCancel(context.Background())
	defer proxyCancel()
	var closeOnce sync.Once
	closeConnections := func() {
		closeOnce.Do(func() {
			_ = appConn.Close(websocket.StatusNormalClosure, "")
			_ = agentConn.Close(websocket.StatusNormalClosure, "")
		})
	}

	// Forward: app -> agent
	go func() {
		defer wg.Done()
		pb.forwardWebSocketMessages(proxyCtx, "app->agent", appConn, agentConn, closeConnections)
		proxyCancel()
	}()

	// Forward: agent -> app
	go func() {
		defer wg.Done()
		pb.forwardWebSocketMessages(proxyCtx, "agent->app", agentConn, appConn, closeConnections)
		proxyCancel()
	}()

	wg.Wait()
	closeConnections()

	if pb.logger != nil {
		pb.logger.Info("phone-bridge-proxy: connection closed")
	}
}

func (pb *PhoneBridge) forwardWebSocketMessages(
	ctx context.Context,
	direction string,
	from, to *websocket.Conn,
	closeConnections func(),
) {
	defer closeConnections()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Read message from source
		msgType, data, err := from.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == -1 && pb.logger != nil {
				pb.logger.Error("phone-bridge-proxy: [%s] read error: %v", direction, err)
			}
			return
		}

		// Forward message to destination
		if err := to.Write(ctx, msgType, data); err != nil {
			if pb.logger != nil {
				pb.logger.Error("phone-bridge-proxy: [%s] write error: %v", direction, err)
			}
			return
		}

		if pb.logger != nil {
			pb.logger.Info("phone-bridge-proxy: [%s] forwarded %d bytes (type=%v)", direction, len(data), msgType)
		}
	}
}

// getProxyStatus fetches status from remote agent in proxy mode.
func (pb *PhoneBridge) getProxyStatus() PhoneBridgeStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pb.proxyEndpoint+"/api/phone-bridge/status", nil)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: create status request failed: %v", err)
		}
		return PhoneBridgeStatus{Connected: false}
	}

	if pb.proxyTaskID != "" {
		req.Header.Set("benchmark-task-id", pb.proxyTaskID)
	}

	resp, err := pb.proxyClient.Do(req)
	if err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: status request failed: %v", err)
		}
		return PhoneBridgeStatus{Connected: false}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: status request returned %d", resp.StatusCode)
		}
		return PhoneBridgeStatus{Connected: false}
	}

	var status PhoneBridgeStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		if pb.logger != nil {
			pb.logger.Error("phone-bridge-proxy: decode status failed: %v", err)
		}
		return PhoneBridgeStatus{Connected: false}
	}

	return status
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var adbUSBIPv4Pattern = regexp.MustCompile(`(?:^|[[:space:]])inet[[:space:]]+(192\.168\.42\.(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9]))(?:/[0-9]{1,2})?(?:[[:space:]]|$)`)

type androidADBController interface {
	Status(ctx context.Context, appUSBIP string) AndroidADBStatusResponse
	Pair(ctx context.Context, req AndroidADBPairRequest) (AndroidADBPairResponse, error)
}

type AndroidADBFrameStatus struct {
	Available            bool    `json:"available"`
	State                string  `json:"state,omitempty"`
	CaptureMode          string  `json:"capture_mode,omitempty"`
	LatestSeq            uint64  `json:"latest_seq,omitempty"`
	FrameAgeMs           uint64  `json:"frame_age_ms,omitempty"`
	RingBufferSize       uint32  `json:"ring_buffer_size,omitempty"`
	RingBufferUsed       uint32  `json:"ring_buffer_used,omitempty"`
	ConsecutiveFailures  uint32  `json:"consecutive_failures,omitempty"`
	LastError            string  `json:"last_error,omitempty"`
	LastRecoveryTs       uint64  `json:"last_recovery_ts,omitempty"`
	AvgFrameServeLatency float64 `json:"avg_frame_serve_latency_ms,omitempty"`
	AvgCaptureCopyMs     float64 `json:"avg_capture_copy_latency_ms,omitempty"`
	Error                string  `json:"error,omitempty"`
}

type AndroidADBDeviceStatus struct {
	Serial string   `json:"serial"`
	State  string   `json:"state"`
	USBIPs []string `json:"usb_ips,omitempty"`
	Match  bool     `json:"match"`
	Error  string   `json:"error,omitempty"`
}

type AndroidADBConnectionStatus struct {
	AppUSBIP           string                   `json:"app_usb_ip,omitempty"`
	HasConnectedDevice bool                     `json:"has_connected_device"`
	MatchedDevice      bool                     `json:"matched_device"`
	MatchedSerial      string                   `json:"matched_serial,omitempty"`
	Devices            []AndroidADBDeviceStatus `json:"devices,omitempty"`
	Error              string                   `json:"error,omitempty"`
}

type AndroidADBStatusResponse struct {
	OK           bool                       `json:"ok"`
	PairRequired bool                       `json:"pair_required"`
	Reason       string                     `json:"reason,omitempty"`
	Frame        AndroidADBFrameStatus      `json:"frame"`
	ADB          AndroidADBConnectionStatus `json:"adb"`
}

type AndroidADBPairRequest struct {
	PairHost string `json:"pair_host"`
	PairPort string `json:"pair_port"`
	PairCode string `json:"pair_code"`
	AppUSBIP string `json:"app_usb_ip,omitempty"`
}

type AndroidADBPairResponse struct {
	OK         bool                     `json:"ok"`
	Error      string                   `json:"error,omitempty"`
	PairOutput string                   `json:"pair_output,omitempty"`
	Status     AndroidADBStatusResponse `json:"status"`
}

type androidADBManager struct {
	logger      *Logger
	frameHealth func(context.Context) (*FrameHealthResult, error)
	runADB      func(context.Context, ...string) (string, error)
	pairMu      sync.Mutex
}

type normalizedAndroidADBPairRequest struct {
	PairHost string
	PairPort string
	PairCode string
	AppUSBIP string
}

type androidADBRequestError struct {
	statusCode int
	message    string
}

func (e *androidADBRequestError) Error() string {
	return e.message
}

func NewAndroidADBManager(frameSocket string, logger *Logger) *androidADBManager {
	frameClient := NewFrameServiceClient(frameSocket)
	return &androidADBManager{
		logger:      logger,
		frameHealth: func(context.Context) (*FrameHealthResult, error) { return frameClient.Health() },
		runADB:      defaultAndroidADBRunner,
	}
}

func defaultAndroidADBRunner(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("adb %s failed: %s", strings.Join(args, " "), text)
		}
		return text, fmt.Errorf("adb %s failed: %w", strings.Join(args, " "), err)
	}
	return text, nil
}

func (m *androidADBManager) Status(ctx context.Context, appUSBIP string) AndroidADBStatusResponse {
	resp := AndroidADBStatusResponse{
		OK:    true,
		Frame: m.frameStatus(ctx),
		ADB:   m.adbStatus(ctx, appUSBIP),
	}
	resp.PairRequired = !resp.Frame.Available && !resp.ADB.MatchedDevice
	if resp.PairRequired {
		resp.Reason = "frame_capture_unavailable_and_no_matched_adb_device"
	}
	return resp
}

func (m *androidADBManager) Pair(ctx context.Context, rawReq AndroidADBPairRequest) (AndroidADBPairResponse, error) {
	req, err := normalizeAndroidADBPairRequest(rawReq)
	if err != nil {
		return AndroidADBPairResponse{
			OK:    false,
			Error: err.Error(),
			Status: AndroidADBStatusResponse{
				OK: true,
			},
		}, &androidADBRequestError{statusCode: http.StatusBadRequest, message: err.Error()}
	}

	m.pairMu.Lock()
	defer m.pairMu.Unlock()

	result := AndroidADBPairResponse{}
	pairTarget := joinHostPort(req.PairHost, req.PairPort)

	pairCtx, cancelPair := context.WithTimeout(ctx, 15*time.Second)
	pairOutput, pairErr := m.runADB(pairCtx, "pair", pairTarget, req.PairCode)
	cancelPair()
	result.PairOutput = pairOutput
	if pairErr != nil {
		result.Status = m.Status(ctx, req.AppUSBIP)
		result.Error = "adb pair failed: " + firstNonEmptyString([]string{pairOutput, pairErr.Error()})
		return result, &androidADBRequestError{statusCode: http.StatusBadGateway, message: result.Error}
	}

	result.Status = m.Status(ctx, req.AppUSBIP)
	result.OK = true
	return result, nil
}

func (m *androidADBManager) frameStatus(ctx context.Context) AndroidADBFrameStatus {
	health, err := m.frameHealth(ctx)
	if err != nil {
		return AndroidADBFrameStatus{
			Available: false,
			Error:     err.Error(),
		}
	}
	status := AndroidADBFrameStatus{
		State:                health.State,
		CaptureMode:          health.CaptureMode,
		LatestSeq:            health.LatestSeq,
		FrameAgeMs:           health.FrameAgeMs,
		RingBufferSize:       health.RingBufferSize,
		RingBufferUsed:       health.RingBufferUsed,
		ConsecutiveFailures:  health.ConsecutiveFailures,
		LastError:            health.LastError,
		LastRecoveryTs:       health.LastRecoveryTs,
		AvgFrameServeLatency: health.AvgFrameServeLatencyMs,
		AvgCaptureCopyMs:     health.AvgCaptureCopyLatencyMs,
	}
	status.Available = frameCaptureAvailable(health)
	return status
}

func frameCaptureAvailable(health *FrameHealthResult) bool {
	if health == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(health.State), "RUNNING") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(health.CaptureMode), "on_demand") {
		return true
	}
	return health.LatestSeq > 0
}

func (m *androidADBManager) adbStatus(ctx context.Context, appUSBIP string) AndroidADBConnectionStatus {
	status := AndroidADBConnectionStatus{
		AppUSBIP: strings.TrimSpace(appUSBIP),
	}

	devicesCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	output, err := m.runADB(devicesCtx, "devices")
	cancel()
	if err != nil {
		status.Error = err.Error()
		return status
	}

	for _, entry := range parseADBDevices(output) {
		device := AndroidADBDeviceStatus{
			Serial: entry.Serial,
			State:  entry.State,
		}
		if entry.State != "device" {
			status.Devices = append(status.Devices, device)
			continue
		}

		status.HasConnectedDevice = true
		shellCtx, cancelShell := context.WithTimeout(ctx, 5*time.Second)
		ipOutput, shellErr := m.runADB(shellCtx, "-s", entry.Serial, "shell", "ip", "addr")
		cancelShell()
		if shellErr != nil {
			device.Error = shellErr.Error()
			status.Devices = append(status.Devices, device)
			continue
		}

		device.USBIPs = extractUSBIPv4s(ipOutput)
		if status.AppUSBIP != "" && stringSliceContains(device.USBIPs, status.AppUSBIP) {
			device.Match = true
			status.MatchedDevice = true
			status.MatchedSerial = entry.Serial
		}
		status.Devices = append(status.Devices, device)
	}

	return status
}

func normalizeAndroidADBPairRequest(req AndroidADBPairRequest) (normalizedAndroidADBPairRequest, error) {
	normalized := normalizedAndroidADBPairRequest{
		PairHost: normalizeHost(req.PairHost),
		PairPort: strings.TrimSpace(req.PairPort),
		PairCode: strings.TrimSpace(req.PairCode),
		AppUSBIP: strings.TrimSpace(req.AppUSBIP),
	}

	if normalized.PairHost == "" {
		return normalized, fmt.Errorf("pair_host is required")
	}
	if err := validateAndroidADBPairHost(normalized.PairHost); err != nil {
		return normalized, err
	}
	if err := validatePort("pair_port", normalized.PairPort); err != nil {
		return normalized, err
	}
	if normalized.PairCode == "" {
		return normalized, fmt.Errorf("pair_code is required")
	}
	return normalized, nil
}

func validateAndroidADBPairHost(host string) error {
	ip := net.ParseIP(normalizeHost(host))
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("pair_host must be a private IPv4 address shown by Android wireless debugging")
	}
	if !ip.IsPrivate() {
		return fmt.Errorf("pair_host must be in a private IPv4 range shown by Android wireless debugging")
	}
	return nil
}

func validatePort(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%s must be a valid TCP port", name)
	}
	return nil
}

func normalizeHost(host string) string {
	return strings.Trim(strings.TrimSpace(host), "[]")
}

func joinHostPort(host, port string) string {
	return net.JoinHostPort(normalizeHost(host), strings.TrimSpace(port))
}

func parseADBDevices(output string) []AndroidADBDeviceStatus {
	lines := strings.Split(output, "\n")
	devices := make([]AndroidADBDeviceStatus, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices attached") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devices = append(devices, AndroidADBDeviceStatus{
			Serial: fields[0],
			State:  fields[1],
		})
	}
	return devices
}

func extractUSBIPv4s(output string) []string {
	lines := strings.Split(output, "\n")
	matches := make([]string, 0, len(lines))
	for _, line := range lines {
		submatches := adbUSBIPv4Pattern.FindStringSubmatch(line)
		if len(submatches) < 2 {
			continue
		}
		matches = append(matches, submatches[1])
	}
	return uniqueStrings(matches)
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func stringSliceContains(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.TrimSpace(value) == target {
			return true
		}
	}
	return false
}

func androidADBStatusCode(err error) int {
	var requestErr *androidADBRequestError
	if err != nil && errors.As(err, &requestErr) {
		return requestErr.statusCode
	}
	if err != nil {
		return http.StatusBadGateway
	}
	return http.StatusOK
}

func (s *Server) handleAndroidADBStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.androidADBSnapshot() == nil {
		http.Error(w, `{"ok":false,"error":"android adb is not initialized"}`, http.StatusServiceUnavailable)
		return
	}

	status := s.androidADBSnapshot().Status(r.Context(), strings.TrimSpace(r.URL.Query().Get("app_usb_ip")))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleAndroidADBPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.androidADBSnapshot() == nil {
		http.Error(w, `{"ok":false,"error":"android adb is not initialized"}`, http.StatusServiceUnavailable)
		return
	}

	var req AndroidADBPairRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"ok":false,"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	result, err := s.androidADBSnapshot().Pair(r.Context(), req)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(androidADBStatusCode(err))
	json.NewEncoder(w).Encode(result)
}

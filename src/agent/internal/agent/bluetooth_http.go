package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"aiden-agent/internal/ble"
	"aiden-agent/internal/logging"
)

const defaultBLEServiceSocketPath = "/run/ble_service/ble_service.sock"

// Keep this above the largest valid eight-event batch after JSON escaping.
const maxPhoneNotificationRequestBytes = 256 * 1024

type bluetoothStatusResponse struct {
	OK        bool              `json:"ok"`
	Bluetooth ble.RuntimeStatus `json:"bluetooth"`
	Removed   int               `json:"removed,omitempty"`
	Error     string            `json:"error,omitempty"`
}

type phoneNotificationRequest struct {
	PhoneID string                  `json:"phone_id"`
	Events  []ble.NotificationEvent `json:"events"`
}

type phoneNotificationResponse struct {
	OK         bool   `json:"ok"`
	Accepted   int    `json:"accepted,omitempty"`
	Duplicates int    `json:"duplicates,omitempty"`
	LastID     string `json:"last_id,omitempty"`
	Error      string `json:"error,omitempty"`
}

type bluetoothWakeResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) handleBluetoothStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeBluetoothError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	status, err := s.bluetoothStatusRequest()(ctx, s.bluetoothServiceSocketPath())
	if err != nil {
		writeBluetoothRequestError(w, err)
		return
	}
	writeBluetoothJSON(w, http.StatusOK, bluetoothStatusResponse{OK: true, Bluetooth: status})
}

func (s *Server) handleBluetoothPairingStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBluetoothError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		writeBluetoothError(w, http.StatusForbidden, "Bluetooth pairing control is available only over USB")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	status, err := s.bluetoothPairingStartRequest()(ctx, s.bluetoothServiceSocketPath())
	if err != nil {
		logging.Warnf("agent", "bluetooth", "Bluetooth pairing start failed: %v", err)
		writeBluetoothRequestError(w, err)
		return
	}
	logBluetoothControl("pairing_start", status, -1)
	writeBluetoothJSON(w, http.StatusOK, bluetoothStatusResponse{OK: true, Bluetooth: status})
}

func (s *Server) handleBluetoothPairingReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBluetoothError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		writeBluetoothError(w, http.StatusForbidden, "Bluetooth pairing reset is available only over USB")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	result, err := s.bluetoothPairingForgetRequest()(ctx, s.bluetoothServiceSocketPath())
	if err != nil {
		logging.Warnf("agent", "bluetooth", "Bluetooth pairing reset failed: %v", err)
		writeBluetoothRequestError(w, err)
		return
	}
	logBluetoothControl("pairing_reset", result.Bluetooth, result.Removed)
	writeBluetoothJSON(w, http.StatusOK, bluetoothStatusResponse{
		OK:        true,
		Bluetooth: result.Bluetooth,
		Removed:   result.Removed,
	})
}

func (s *Server) handleBluetoothDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBluetoothError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		writeBluetoothError(w, http.StatusForbidden, "Bluetooth disconnect control is available only over USB")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	status, err := s.bluetoothDisconnectRequest()(ctx, s.bluetoothServiceSocketPath())
	if err != nil {
		logging.Warnf("agent", "bluetooth", "Bluetooth disconnect failed: %v", err)
		writeBluetoothRequestError(w, err)
		return
	}
	logBluetoothControl("disconnect", status, -1)
	writeBluetoothJSON(w, http.StatusOK, bluetoothStatusResponse{OK: true, Bluetooth: status})
}

// logBluetoothControl records App-initiated pairing control. These requests
// decide whether the board accepts a new bond, so they must be visible when a
// phone fails to connect. A negative removed count means "not applicable".
func logBluetoothControl(action string, status ble.RuntimeStatus, removed int) {
	removedField := ""
	if removed >= 0 {
		removedField = fmt.Sprintf(" removed=%d", removed)
	}
	logging.Infof(
		"agent", "bluetooth",
		"Bluetooth %s pairing_open=%t bonded=%d connected=%t wake_subscriber=%t%s",
		action, status.PairingOpen, status.BondedDeviceCount, status.Connected, status.WakeSubscriber, removedField,
	)
}

func (s *Server) handleBluetoothUSBReenumerationWake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeBluetoothWakeJSON(w, http.StatusMethodNotAllowed, bluetoothWakeResponse{
			Error: "method not allowed",
		})
		return
	}
	remoteIP, ok := addressIP(r.RemoteAddr)
	if !ok || !remoteIP.IsLoopback() {
		writeBluetoothWakeJSON(w, http.StatusForbidden, bluetoothWakeResponse{
			Error: "USB re-enumeration wake is available only on board loopback",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	wake := s.bleWakeRequest
	if wake == nil {
		wake = defaultBLEWake
	}
	if err := wake(ctx, "usb_reenumeration"); err != nil {
		writeBluetoothWakeJSON(w, http.StatusServiceUnavailable, bluetoothWakeResponse{
			Error: err.Error(),
		})
		return
	}
	writeBluetoothWakeJSON(w, http.StatusOK, bluetoothWakeResponse{OK: true})
}

func (s *Server) handlePhoneNotificationEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePhoneNotificationJSON(w, http.StatusMethodNotAllowed, phoneNotificationResponse{
			OK:    false,
			Error: "method not allowed",
		})
		return
	}
	if !bluetoothControlRequestAllowed(r) {
		writePhoneNotificationJSON(w, http.StatusForbidden, phoneNotificationResponse{
			OK:    false,
			Error: "phone notification ingestion is available only over USB",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPhoneNotificationRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request phoneNotificationRequest
	if err := decoder.Decode(&request); err != nil {
		writePhoneNotificationJSON(w, http.StatusBadRequest, phoneNotificationResponse{
			OK:    false,
			Error: "invalid notification request: " + err.Error(),
		})
		return
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writePhoneNotificationJSON(w, http.StatusBadRequest, phoneNotificationResponse{
			OK:    false,
			Error: "invalid notification request: multiple JSON values",
		})
		return
	}
	if err := ble.ValidateNotificationPublishRequestFrame(request.PhoneID, request.Events); err != nil {
		writePhoneNotificationJSON(w, http.StatusBadRequest, phoneNotificationResponse{
			OK:    false,
			Error: "invalid notification request: " + err.Error(),
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()
	result, err := s.bluetoothNotificationPublishRequest()(
		ctx,
		s.bluetoothServiceSocketPath(),
		request.PhoneID,
		request.Events,
	)
	if err != nil {
		statusCode := http.StatusServiceUnavailable
		var requestErr *ble.RequestError
		if errors.As(err, &requestErr) && requestErr.Status == "INVALID_ARGUMENT" {
			statusCode = http.StatusBadRequest
		}
		writePhoneNotificationJSON(w, statusCode, phoneNotificationResponse{OK: false, Error: err.Error()})
		return
	}
	writePhoneNotificationJSON(w, http.StatusOK, phoneNotificationResponse{
		OK:         true,
		Accepted:   result.Accepted,
		Duplicates: result.Duplicates,
		LastID:     result.LastID,
	})
	if result.Accepted > 0 && s.runtime != nil {
		if plane, ok := s.runtime.MemoryPlane().(*FilesystemMemoryPlane); ok {
			plane.NotifyMemory()
		}
	}
}

func (s *Server) bluetoothServiceSocketPath() string {
	if path := strings.TrimSpace(s.bleSocketPath); path != "" {
		return path
	}
	return configuredBLEServiceSocketPath()
}

func configuredBLEServiceSocketPath() string {
	if path := strings.TrimSpace(os.Getenv("AIDEN_BLE_SERVICE_SOCKET")); path != "" {
		return path
	}
	return defaultBLEServiceSocketPath
}

func bluetoothControlRequestAllowed(r *http.Request) bool {
	remoteIP, ok := addressIP(r.RemoteAddr)
	if !ok {
		return false
	}
	if remoteIP.IsLoopback() {
		return true
	}
	localAddr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || localAddr == nil {
		return false
	}
	localIP, ok := addressIP(localAddr.String())
	if !ok {
		return false
	}
	usbPrefix := netip.MustParsePrefix("192.168.42.0/24")
	return localIP == netip.MustParseAddr("192.168.42.1") && usbPrefix.Contains(remoteIP)
}

func addressIP(address string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return netip.Addr{}, false
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return parsed.Unmap(), true
}

func (s *Server) bluetoothStatusRequest() func(context.Context, string) (ble.RuntimeStatus, error) {
	if s.bleStatusRequest != nil {
		return s.bleStatusRequest
	}
	return ble.RequestStatus
}

func (s *Server) bluetoothPairingStartRequest() func(context.Context, string) (ble.RuntimeStatus, error) {
	if s.blePairingStartRequest != nil {
		return s.blePairingStartRequest
	}
	return ble.RequestPairingStart
}

func (s *Server) bluetoothPairingForgetRequest() func(context.Context, string) (ble.ForgetResult, error) {
	if s.blePairingForgetRequest != nil {
		return s.blePairingForgetRequest
	}
	return ble.RequestPairingForget
}

func (s *Server) bluetoothDisconnectRequest() func(context.Context, string) (ble.RuntimeStatus, error) {
	if s.bleDisconnectRequest != nil {
		return s.bleDisconnectRequest
	}
	return ble.RequestDisconnect
}

func (s *Server) bluetoothNotificationPublishRequest() func(
	context.Context,
	string,
	string,
	[]ble.NotificationEvent,
) (ble.NotificationPublishResult, error) {
	if s.bleNotifyRequest != nil {
		return s.bleNotifyRequest
	}
	return ble.RequestPublishNotifications
}

func writeBluetoothRequestError(w http.ResponseWriter, err error) {
	statusCode := http.StatusServiceUnavailable
	var requestErr *ble.RequestError
	if errors.As(err, &requestErr) {
		switch requestErr.Status {
		case "FAILED_PRECONDITION":
			statusCode = http.StatusConflict
		case "INVALID_ARGUMENT":
			statusCode = http.StatusBadRequest
		case "SERVICE_UNAVAILABLE":
			statusCode = http.StatusServiceUnavailable
		default:
			statusCode = http.StatusBadGateway
		}
	}
	writeBluetoothError(w, statusCode, err.Error())
}

func writeBluetoothError(w http.ResponseWriter, statusCode int, message string) {
	writeBluetoothJSON(w, statusCode, bluetoothStatusResponse{OK: false, Error: message})
}

func writeBluetoothJSON(w http.ResponseWriter, statusCode int, payload bluetoothStatusResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeBluetoothWakeJSON(w http.ResponseWriter, statusCode int, payload bluetoothWakeResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func writePhoneNotificationJSON(w http.ResponseWriter, statusCode int, payload phoneNotificationResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

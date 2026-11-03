package configweb

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	if !isSameOriginBrowserRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if _, err := exec.LookPath("reboot"); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "reboot command not found")
		return
	}
	cmd := exec.Command("/bin/sh", "-c", "sync; sleep 1; reboot")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "failed to schedule reboot: "+err.Error())
		return
	}
	go func() { _ = cmd.Wait() }()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "reboot scheduled", "reboot_scheduled": true})
}

// handleAgentRestart carries out the agent_restart apply level. Config Web is a
// separate process, so the page that asked stays up while the Agent restarts.
// handleAgentRestart, like the reboot and reset endpoints, is a simple POST a
// page on another origin could send, so it refuses cross-site requests.
func (s *Server) handleAgentRestart(w http.ResponseWriter, r *http.Request) {
	if !isSameOriginBrowserRequest(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err := s.scheduleAgentRestart(); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "agent_restart_scheduled": true})
}

const usbReenumerateScript = `set -eu
UDC="$(ls /sys/class/udc 2>/dev/null | head -n 1)"
GADGET_DIR=/sys/kernel/config/usb_gadget/aico_hid
if [ -z "$UDC" ] || [ ! -d "$GADGET_DIR" ]; then
  echo 'Error: UDC device or gadget directory not found' >&2
  exit 1
fi
if ! echo "" > "$GADGET_DIR/UDC" 2>/dev/null; then
  echo 'Error: Failed to unbind UDC' >&2
  exit 2
fi
sleep 1
if ! echo "$UDC" > "$GADGET_DIR/UDC" 2>/dev/null; then
  echo 'Error: Failed to rebind UDC' >&2
  exit 3
fi
echo 'USB re-enumeration completed successfully'
`

func (s *Server) handleUSBReenumerate(w http.ResponseWriter, _ *http.Request) {
	result := runCommand(5*time.Second, nil, []byte(usbReenumerateScript), "sh", "-s")
	if result.ExitCode != 0 {
		message := strings.TrimSpace(string(result.Output))
		if message == "" {
			message = fmt.Sprintf("USB re-enumeration failed with exit code %d", result.ExitCode)
		}
		writeJSONError(w, 500, message)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "message": "USB re-enumeration triggered successfully"})
}

func (s *Server) prepareOTAUpdateLog() (int64, error) {
	if err := os.MkdirAll(filepath.Dir(s.options.OTAUpdateLogPath), 0o755); err != nil {
		return 0, err
	}
	if info, err := os.Stat(s.options.OTAUpdateLogPath); err == nil && info.Size() > 100*1024 {
		data, err := tailFile(s.options.OTAUpdateLogPath, 100*1024)
		if err != nil {
			return 0, err
		}
		if err := atomicWriteFile(s.options.OTAUpdateLogPath, data, 0o600); err != nil {
			return 0, err
		}
	}
	file, err := os.OpenFile(s.options.OTAUpdateLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *Server) acquireOTALock() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(s.options.OTAUpdateLockPath), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(s.options.OTAUpdateLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("ota update already running: %w", err)
		}
		return nil, err
	}
	return file, nil
}

func (s *Server) otaUpdateRunning() bool {
	if _, err := os.Stat(s.options.HybridUpdateMarkerPath); err == nil {
		return true
	}
	file, err := os.OpenFile(s.options.OTAUpdateLockPath, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK || err == syscall.EAGAIN
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false
}

func (s *Server) handleOTAUpdate(w http.ResponseWriter, _ *http.Request) {
	lock, err := s.acquireOTALock()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			status = http.StatusConflict
		}
		writeJSONError(w, status, err.Error())
		return
	}
	defer lock.Close()
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if _, err := os.Stat(s.options.HybridUpdateMarkerPath); err == nil {
		writeJSONError(w, http.StatusConflict, "ota update already running")
		return
	} else if !os.IsNotExist(err) {
		writeJSONError(w, 500, err.Error())
		return
	}
	startSize, err := s.prepareOTAUpdateLog()
	if err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	// The marker is persistent on userdata.  The systemd worker owns the
	// process and can therefore continue after a package restart or an OTA
	// reboot, while Config Web remains a short-lived API front end.
	if err := atomicWriteFile(s.options.HybridUpdateMarkerPath, []byte("{\"phase\":\"requested\"}\n"), 0o600); err != nil {
		writeJSONError(w, 500, "failed to queue hybrid update: "+err.Error())
		return
	}
	logFile, err := os.OpenFile(s.options.OTAUpdateLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = os.Remove(s.options.HybridUpdateMarkerPath)
		writeJSONError(w, 500, err.Error())
		return
	}
	defer logFile.Close()
	if _, err := fmt.Fprintf(logFile, "%s [INFO][config_web][ota] message=\"hybrid: start\"\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
		_ = os.Remove(s.options.HybridUpdateMarkerPath)
		writeJSONError(w, 500, err.Error())
		return
	}
	result := runCommand(10*time.Second, nil, nil, s.options.SystemctlBinary, "start", "--no-block", s.options.HybridUpdateService)
	if result.ExitCode != 0 {
		_, _ = fmt.Fprintf(logFile, "%s\n%s [ERROR][config_web][ota] update_exited exit_code=%d\n", result.Output, time.Now().UTC().Format(time.RFC3339), result.ExitCode)
		_ = os.Remove(s.options.HybridUpdateMarkerPath)
		writeJSONError(w, http.StatusServiceUnavailable, "failed to start hybrid update: "+strings.TrimSpace(string(result.Output)))
		return
	}
	taskID := fmt.Sprintf("ota-%d", time.Now().UnixNano())
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "task_id": taskID, "status": "running", "ota_update_started": true,
		"message": "hybrid update started", "ota_log_start_size_bytes": startSize,
	})
}

func (s *Server) handleOTALogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "ota_update_running": s.otaUpdateRunning(), "hybrid_update_phase": s.hybridUpdatePhase(), "ota_log": readLogSnapshot(s.options.OTAUpdateLogPath, 128*1024, 96*1024), "ota_health_log": readLogSnapshot(s.options.OTAHealthLogPath, 128*1024, 96*1024)})
}

func (s *Server) hybridUpdatePhase() string {
	var marker struct {
		Phase  string `json:"phase"`
		BootID string `json:"boot_id"`
	}
	data, err := readFileLimited(s.options.HybridUpdateMarkerPath, 4096)
	if err != nil || json.Unmarshal(data, &marker) != nil {
		return ""
	}
	if marker.Phase == "post-ota" {
		bootID, err := os.ReadFile(s.options.BootIDPath)
		if err == nil && strings.TrimSpace(string(bootID)) != "" && strings.TrimSpace(string(bootID)) != marker.BootID {
			return "waiting-health"
		}
	}
	return marker.Phase
}

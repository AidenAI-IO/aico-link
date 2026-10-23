package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/ble"
)

type SelfCheckConfig struct {
	RequiredWiFi        bool
	RequiredPhoneBridge bool
	RequiredHDMI        bool
	RequiredUDC         bool
	CommandTimeout      time.Duration
	FrameCLI            string
	AudioCLI            string
	Curl                string
	ConfigWebURL        string
	BLESocketPath       string
	PhoneBridgeURL      string
}

type SelfCheckItem struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
type SelfCheckReport struct {
	StartedAt  time.Time                `json:"started_at"`
	FinishedAt time.Time                `json:"finished_at"`
	Items      map[string]SelfCheckItem `json:"items"`
	Passed     int                      `json:"passed"`
	Warnings   int                      `json:"warnings"`
	Failures   int                      `json:"failures"`
}

func (r SelfCheckReport) Fatal() bool { return r.Failures > 0 }
func DefaultSelfCheckConfig() SelfCheckConfig {
	return SelfCheckConfig{CommandTimeout: 4 * time.Second}
}

// RunSelfCheck only performs read-only probes. External conditions such as a
// missing HDMI source, phone, Wi-Fi AP, or USB host are warnings by default.
func RunSelfCheck(ctx context.Context, cfg SelfCheckConfig) SelfCheckReport {
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = 4 * time.Second
	}
	r := SelfCheckReport{StartedAt: time.Now().UTC(), Items: map[string]SelfCheckItem{}}
	add := func(name, status, detail string) {
		r.Items[name] = SelfCheckItem{status, detail}
		switch status {
		case "pass":
			r.Passed++
		case "warn":
			r.Warnings++
		default:
			r.Failures++
		}
	}
	pathCheck := func(name, path string, required bool) {
		if _, e := os.Stat(path); e == nil {
			add(name, "pass", path)
		} else if required {
			add(name, "fail", e.Error())
		} else {
			add(name, "warn", "not present")
		}
	}
	pathCheck("ota_storage", "/userdata/ota", true)
	pathCheck("agent_config", "/userdata/agent/agent.toml", true)
	pathCheck("hid_keyboard", "/dev/hidg0", true)
	pathCheck("hid_pointer", "/dev/hidg1", true)
	pathCheck("hid_consumer", "/dev/hidg2", false)
	command := func(name string, required bool, bin string, args ...string) {
		c, cancel := context.WithTimeout(ctx, cfg.CommandTimeout)
		defer cancel()
		out, e := exec.CommandContext(c, bin, args...).CombinedOutput()
		if e == nil {
			add(name, "pass", strings.TrimSpace(string(out)))
			return
		}
		st := "warn"
		if required {
			st = "fail"
		}
		d := strings.TrimSpace(string(out))
		if d == "" {
			d = e.Error()
		}
		add(name, st, d)
	}
	frameCLI := cfg.FrameCLI
	if frameCLI == "" {
		frameCLI = "/usr/lib/aico/frame_service_cli"
	}
	audioCLI := cfg.AudioCLI
	if audioCLI == "" {
		audioCLI = "/usr/lib/aico/audio_service_cli"
	}
	curlBin := cfg.Curl
	if curlBin == "" {
		curlBin = "curl"
	}
	configWebURL := cfg.ConfigWebURL
	if configWebURL == "" {
		configWebURL = "http://127.0.0.1/api/device/status"
	}
	phoneBridgeURL := cfg.PhoneBridgeURL
	if phoneBridgeURL == "" {
		phoneBridgeURL = "http://127.0.0.1:8080/api/phone-bridge/status"
	}
	bleSocketPath := cfg.BLESocketPath
	if bleSocketPath == "" {
		bleSocketPath = "/run/ble_service/ble_service.sock"
	}
	command("frame_service", true, frameCLI, "--socket", "/run/frame_service/frame_service.sock", "health")
	command("audio_service", true, audioCLI, "--socket", "/run/audio_service/audio_service.sock", "health")
	// Agent startup is optional during OTA validation. A bad persisted Agent
	// configuration must not turn an otherwise usable update into a rollback.
	command("agent_http", false, curlBin, "--fail", "--silent", "--max-time", "3", "http://127.0.0.1:8080/health")
	// Config Web is the recovery entry point when the Agent cannot start, so it
	// must remain available for the update to be considered healthy.
	command("config_web", true, curlBin, "--fail", "--silent", "--max-time", "3", configWebURL)
	bleCtx, bleCancel := context.WithTimeout(ctx, cfg.CommandTimeout)
	bleStatus, bleErr := ble.RequestStatus(bleCtx, bleSocketPath)
	bleCancel()
	if bleErr != nil {
		add("ble_service", "warn", bleErr.Error())
	} else if !bleStatus.BackendAvailable {
		add("ble_service", "warn", "BLE backend unavailable")
	} else {
		detail, _ := json.Marshal(bleStatus)
		add("ble_service", "pass", string(detail))
	}
	if _, e := os.Stat("/sys/class/net/usb0"); e == nil {
		command("usb_ecm", cfg.RequiredUDC, "sh", "-c", "ip addr show usb0 | grep -q '192.168.42.1'")
	} else if cfg.RequiredUDC {
		add("usb_ecm", "fail", "usb0 not enumerated")
	} else {
		add("usb_ecm", "warn", "usb0 not enumerated")
	}
	if cfg.RequiredWiFi {
		command("wifi_uplink", true, "sh", "-c", "ip route | grep -q default")
	} else {
		add("wifi_uplink", "warn", "optional; not required")
	}
	phoneBody, phoneErr := runSelfCheckCommand(ctx, cfg.CommandTimeout, curlBin, "--fail", "--silent", "--max-time", "3", phoneBridgeURL)
	if phoneErr != nil {
		status := "warn"
		if cfg.RequiredPhoneBridge {
			status = "fail"
		}
		add("phone_bridge", status, phoneErr.Error())
	} else {
		status, detail := classifyPhoneBridgeStatus(phoneBody, cfg.RequiredPhoneBridge)
		add("phone_bridge", status, detail)
	}
	if cfg.RequiredHDMI {
		command("hdmi_signal", true, frameCLI, "--socket", "/run/frame_service/frame_service.sock", "latest-frame", "--out", "/tmp/ota-self-check-frame.raw")
	} else {
		add("hdmi_signal", "warn", "optional; HDMI source not required")
	}
	r.FinishedAt = time.Now().UTC()
	return r
}

func classifyPhoneBridgeStatus(body string, required bool) (string, string) {
	var phone struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal([]byte(body), &phone); err != nil {
		status := "warn"
		if required {
			status = "fail"
		}
		return status, fmt.Sprintf("invalid status response: %v", err)
	}
	if !phone.Connected {
		if required {
			return "fail", "Phone Bridge is not connected"
		}
		return "warn", "Phone Bridge is not connected"
	}
	return "pass", strings.TrimSpace(body)
}

func runSelfCheckCommand(parent context.Context, timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("%s", detail)
	}
	return string(out), nil
}

func SaveSelfCheckReport(path string, report SelfCheckReport) error {
	if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
		return e
	}
	b, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		return e
	}
	tmp := path + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(append(b, '\n')); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(tmp, path); e != nil {
		return e
	}
	return fsyncDirFor(path)
}

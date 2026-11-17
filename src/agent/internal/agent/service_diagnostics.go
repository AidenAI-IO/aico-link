package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDiagnosticsDir        = "/userdata/diagnostics"
	defaultDiagnosticsEventsFile = "service-events.jsonl"
	diagnosticsEventsLimit       = 50
)

type coreServiceSpec struct {
	Unit  string `json:"unit"`
	Label string `json:"label"`
}

var coreServiceSpecs = []coreServiceSpec{
	{Unit: "aico-agent.service", Label: "Agent"},
	{Unit: "aico-frame.service", Label: "Frame"},
	{Unit: "aico-audio.service", Label: "Audio"},
	{Unit: "aico-ble.service", Label: "BLE"},
	{Unit: "aico-config-web.service", Label: "Config Web"},
	{Unit: "aico-mcu-event.service", Label: "MCU"},
	{Unit: "aico-wifi-proxy.service", Label: "Wi-Fi Proxy"},
	{Unit: "aico-ttyd.service", Label: "Terminal"},
}

type serviceProcSample struct {
	PID         int    `json:"pid"`
	CPUTotal    uint64 `json:"cpu_total"`
	MemRSSBytes uint64 `json:"mem_rss_bytes"`
}

type coreServiceStatus struct {
	Unit        string             `json:"unit"`
	Label       string             `json:"label"`
	ActiveState string             `json:"active_state"`
	SubState    string             `json:"sub_state"`
	Result      string             `json:"result"`
	NRestarts   int                `json:"n_restarts"`
	MainPID     int                `json:"main_pid"`
	Severity    string             `json:"severity"`
	Proc        *serviceProcSample `json:"proc,omitempty"`
	LoadError   string             `json:"load_error,omitempty"`
}

type serviceDiagnosticsSnapshot struct {
	SampledAtMS int64               `json:"sampled_at_ms"`
	Services    []coreServiceStatus `json:"services"`
}

type serviceIncidentEvent map[string]any

func diagnosticsDir() string {
	if value := strings.TrimSpace(os.Getenv("AICO_DIAGNOSTICS_DIR")); value != "" {
		return value
	}
	return defaultDiagnosticsDir
}

func diagnosticsEventsPath() string {
	if value := strings.TrimSpace(os.Getenv("AICO_DIAGNOSTICS_EVENTS")); value != "" {
		return value
	}
	return filepath.Join(diagnosticsDir(), defaultDiagnosticsEventsFile)
}

func classifyServiceSeverity(activeState, result string, nRestarts int) string {
	switch {
	case activeState == "failed",
		result == "signal",
		result == "core-dump",
		result == "oom-kill",
		result == "start-limit-hit":
		return "failed"
	case activeState == "activating", activeState == "deactivating":
		return "degraded"
	case nRestarts >= 3 && activeState == "active":
		return "degraded"
	case activeState == "active" || activeState == "inactive":
		return "ok"
	default:
		if result != "" && result != "success" {
			return "degraded"
		}
		return "ok"
	}
}

func readProcSample(pid int) *serviceProcSample {
	if pid < 2 {
		return nil
	}
	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	statusData, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil
	}
	// /proc/pid/stat: after comm in parentheses, fields are space-separated.
	start := bytes.LastIndexByte(statData, ')')
	if start < 0 || start+2 >= len(statData) {
		return nil
	}
	fields := strings.Fields(string(statData[start+2:]))
	// utime=12 stime=13 in the post-comm field list (1-based field 14/15 overall).
	if len(fields) < 13 {
		return nil
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	var rssKB uint64
	for _, line := range strings.Split(string(statusData), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				rssKB, _ = strconv.ParseUint(parts[1], 10, 64)
			}
			break
		}
	}
	return &serviceProcSample{
		PID:         pid,
		CPUTotal:    utime + stime,
		MemRSSBytes: rssKB * 1024,
	}
}

func parseSystemctlShowBlocks(output string) map[string]map[string]string {
	result := map[string]map[string]string{}
	current := map[string]string{}
	flush := func() {
		id := current["Id"]
		if id == "" {
			id = current["Names"]
		}
		if id != "" {
			result[id] = current
		}
		current = map[string]string{}
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		current[key] = value
	}
	flush()
	return result
}

var systemctlShow = func(units ...string) (string, error) {
	args := append([]string{"show"}, units...)
	args = append(args,
		"-p", "Id",
		"-p", "ActiveState",
		"-p", "SubState",
		"-p", "Result",
		"-p", "NRestarts",
		"-p", "MainPID",
	)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), fmt.Errorf("systemctl show timed out")
	}
	if err != nil && stdout.Len() == 0 {
		return "", fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func collectCoreServiceStatuses() serviceDiagnosticsSnapshot {
	snapshot := serviceDiagnosticsSnapshot{
		SampledAtMS: time.Now().UnixMilli(),
		Services:    make([]coreServiceStatus, 0, len(coreServiceSpecs)),
	}
	units := make([]string, 0, len(coreServiceSpecs))
	for _, spec := range coreServiceSpecs {
		units = append(units, spec.Unit)
	}
	raw, err := systemctlShow(units...)
	blocks := map[string]map[string]string{}
	loadErr := ""
	if err != nil {
		loadErr = err.Error()
	} else {
		blocks = parseSystemctlShowBlocks(raw)
	}
	for _, spec := range coreServiceSpecs {
		status := coreServiceStatus{
			Unit:      spec.Unit,
			Label:     spec.Label,
			Severity:  "ok",
			LoadError: loadErr,
		}
		if block, ok := blocks[spec.Unit]; ok {
			status.ActiveState = block["ActiveState"]
			status.SubState = block["SubState"]
			status.Result = block["Result"]
			status.NRestarts, _ = strconv.Atoi(block["NRestarts"])
			status.MainPID, _ = strconv.Atoi(block["MainPID"])
			status.Severity = classifyServiceSeverity(status.ActiveState, status.Result, status.NRestarts)
			status.Proc = readProcSample(status.MainPID)
			status.LoadError = ""
		} else if loadErr == "" {
			status.ActiveState = "unknown"
			status.Severity = "degraded"
			status.LoadError = "unit status unavailable"
		}
		snapshot.Services = append(snapshot.Services, status)
	}
	return snapshot
}

func readServiceIncidentEvents(limit int, unitFilter string) ([]serviceIncidentEvent, error) {
	if limit <= 0 {
		limit = diagnosticsEventsLimit
	}
	unitFilter = strings.TrimSpace(unitFilter)
	if unitFilter != "" && !strings.HasSuffix(unitFilter, ".service") {
		unitFilter += ".service"
	}
	path := diagnosticsEventsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []serviceIncidentEvent{}, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	events := make([]serviceIncidentEvent, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(events) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var event serviceIncidentEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if unitFilter != "" {
			unit, _ := event["unit"].(string)
			if unit != unitFilter {
				continue
			}
		}
		events = append(events, event)
	}
	return events, nil
}

func readServiceIncidentLogTail(unit string, maxBytes int64) (string, error) {
	unit = strings.TrimSpace(unit)
	if unit == "" || strings.Contains(unit, "/") || strings.Contains(unit, "..") {
		return "", fmt.Errorf("invalid unit")
	}
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	allowed := false
	for _, spec := range coreServiceSpecs {
		if spec.Unit == unit {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("unit not monitored")
	}
	if maxBytes <= 0 || maxBytes > 64*1024 {
		maxBytes = 32 * 1024
	}
	path := filepath.Join(diagnosticsDir(), "last", unit)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if int64(len(data)) > maxBytes {
		data = data[int64(len(data))-maxBytes:]
	}
	return string(data), nil
}

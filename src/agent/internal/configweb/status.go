package configweb

import (
	"aico-agent/internal/agent"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type logSnapshot struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"size_bytes"`
	Truncated bool   `json:"truncated"`
	Log       string `json:"log"`
	Error     string `json:"error"`
}

func readLogSnapshot(path string, readSize, displaySize int64) logSnapshot {
	snapshot := logSnapshot{Path: path}
	file, err := os.Open(path)
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	defer file.Close()
	snapshot.Exists = true
	info, err := file.Stat()
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	snapshot.SizeBytes = info.Size()
	offset := int64(0)
	if info.Size() > readSize {
		offset = info.Size() - readSize
		snapshot.Truncated = true
	}
	if _, err := file.Seek(offset, 0); err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	data, err := io.ReadAll(io.LimitReader(file, readSize))
	if err != nil {
		snapshot.Error = err.Error()
		return snapshot
	}
	if offset > 0 {
		if newline := strings.IndexByte(string(data), '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	text := strings.TrimRight(string(data), "\r\n")
	if int64(len(text)) > displaySize {
		snapshot.Truncated = true
		text = text[len(text)-int(displaySize):]
		if newline := strings.IndexByte(text, '\n'); newline >= 0 && newline+1 < len(text) {
			text = text[newline+1:]
		}
	}
	snapshot.Log = text
	return snapshot
}

func (s *Server) agentLogPath() string {
	return filepath.Join(s.options.ConfigDir(), "log", "agent.log")
}

func (s *Server) handleAgentLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"agent_log": readLogSnapshot(s.agentLogPath(), 64*1024, 48*1024),
	})
}

func (s *Server) queryAgentStatus() map[string]any {
	status := map[string]any{
		"state": "unknown", "watchdog_running": false, "watchdog_pid": 0,
		"process_running": false, "pid": 0, "addr": ":8080",
		"port_host": "127.0.0.1", "port": 8080, "public_port": 8080,
		"port_reachable": false, "port_detail": "", "detail": "", "startup_error": "",
	}
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AICO_PUBLIC_AGENT_WEB_PORT"))); err == nil && value > 0 && value <= 65535 {
		status["public_port"] = value
	}
	if _, err := os.Stat(s.options.AgentInitScript); err != nil {
		detail := "agent init script not found: " + s.options.AgentInitScript
		status["detail"], status["startup_error"] = detail, detail
	} else {
		result := runCommand(1500*time.Millisecond, os.Environ(), nil, s.options.AgentInitScript, "status")
		detail := strings.TrimRight(string(result.Output), "\r\n")
		status["detail"] = detail
		for _, line := range strings.Split(detail, "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "watchdog=running"):
				status["watchdog_running"] = true
				status["watchdog_pid"] = parseStatusPID(line)
			case strings.HasPrefix(line, "agent=running"):
				status["process_running"] = true
				status["pid"] = parseStatusPID(line)
				if addr := parseStatusValue(line, "addr"); addr != "" {
					status["addr"] = addr
					host, port := parseAgentAddr(addr)
					status["port_host"], status["port"] = host, port
					if os.Getenv("AICO_PUBLIC_AGENT_WEB_PORT") == "" {
						status["public_port"] = port
					}
				}
			case strings.HasPrefix(line, "agent=stopped"):
				status["state"] = "stopped"
			}
		}
		if result.TimedOut {
			status["startup_error"] = detail
		}
	}
	host, _ := status["port_host"].(string)
	port, _ := status["port"].(int)
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 700*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		status["port_reachable"] = true
		status["port_detail"] = "connected"
	} else {
		status["port_detail"] = err.Error()
	}
	if status["process_running"] == true {
		if status["port_reachable"] == true {
			status["state"] = "running"
		} else {
			status["state"] = "port_unreachable"
		}
	} else if status["watchdog_running"] == true {
		recent := readLogSnapshot(s.agentLogPath(), 16*1024, 4096).Log
		status["startup_error"] = recent
		lower := strings.ToLower(recent)
		if strings.Contains(lower, "error") || strings.Contains(lower, "failed") ||
			strings.Contains(lower, "not found") || strings.Contains(lower, "panic") || strings.Contains(lower, "exited with status") {
			status["state"] = "error"
		} else {
			status["state"] = "starting"
		}
	}
	return status
}

func parseStatusPID(line string) int {
	value := parseStatusValue(line, "pid")
	pid, _ := strconv.Atoi(value)
	if pid < 1 || pid > 999999 {
		return 0
	}
	return pid
}

func parseStatusValue(line, key string) string {
	prefix := key + "="
	position := strings.Index(line, prefix)
	if position < 0 {
		return ""
	}
	value := line[position+len(prefix):]
	if end := strings.IndexAny(value, " \t\r\n"); end >= 0 {
		value = value[:end]
	}
	return value
}

func parseAgentAddr(addr string) (string, int) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			host, portText = "127.0.0.1", strings.TrimPrefix(addr, ":")
		} else {
			return "127.0.0.1", 8080
		}
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		port = 8080
	}
	return host, port
}

func (s *Server) handleAgentStatus(w http.ResponseWriter, _ *http.Request) {
	status := s.queryAgentStatus()
	deviceType := s.deviceType()
	usb := map[string]any{
		"keyboard": pathExists("/dev/hidg0"),
		"pointer":  pathExists("/dev/hidg1"),
		"ecm":      pathExists("/sys/class/net/usb0"),
	}
	capabilities := map[string]any{
		"wifi": true, "ota": true, "storage": true,
		"usb_hid": usb["keyboard"] == true || usb["pointer"] == true,
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "agent_status": status,
		"device_type": deviceType, "firmware": s.firmwareInfo(),
		"usb_hid": usb, "capabilities": capabilities,
	})
}

// deviceType reads the resolved configuration directly so the device status
// resource can report the same effective type as the Agent runtime without
// depending on the status text emitted by an init script.
func (s *Server) deviceType() string {
	// Recovery mode is exactly when the status page matters most, so read the
	// persisted value without requiring the Agent's semantic validation.
	cfg, err := agent.LoadResolvedConfigForUpdate(s.options.AgentConfigPath)
	if err != nil {
		return ""
	}
	return cfg.DeviceTypeOrDefault()
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func emptyFirmwareInfo() map[string]any {
	return map[string]any{
		"version": "", "build_time": "", "phase": "", "health_status": "", "health_error": "",
		"current_version": "", "current_build_time": "", "target_version": "", "target_build_time": "",
		"previous_version": "", "previous_build_time": "", "running_slot": "", "target_slot": "",
		"components": map[string]string{"boot": "", "rootfs": ""},
	}
}

func firmwareComponentVersions(state map[string]any, slot string) map[string]string {
	result := map[string]string{"boot": "", "rootfs": ""}
	slots, _ := state["slots"].(map[string]any)
	slotState, _ := slots[slot].(map[string]any)
	partitions, _ := slotState["partitions"].(map[string]any)
	for name := range result {
		partition, _ := partitions[name].(map[string]any)
		result[name] = jsonString(partition, "version")
	}
	return result
}

func (s *Server) firmwareInfo() map[string]any {
	data, err := readFileLimited(s.options.OTAStatePath, maxAgentConfigSize)
	if os.IsNotExist(err) {
		return s.factoryFirmwareInfo()
	}
	if err != nil {
		return emptyFirmwareInfo()
	}
	var state map[string]any
	if json.Unmarshal(data, &state) != nil {
		return emptyFirmwareInfo()
	}
	currentVersion := jsonString(state, "current_version")
	currentBuild := jsonString(state, "current_build_time")
	targetVersion := jsonString(state, "target_version")
	targetBuild := jsonString(state, "target_build_time")
	phase := jsonString(state, "phase")
	lastError := jsonString(state, "last_error")
	targetSlot := normalizeSlot(state["target_slot"])
	runningSlot := ""
	if cmdline, err := readFileLimited(s.options.CmdlinePath, 16*1024); err == nil {
		runningSlot = currentSlot(string(cmdline))
	}
	componentSlot := runningSlot
	if componentSlot == "" {
		componentSlot = normalizeSlot(state["active_slot"])
	}
	components := firmwareComponentVersions(state, componentSlot)
	showTarget := targetVersion != "" && (phase == "pending-reboot" || phase == "health") && runningSlot != "" && runningSlot == targetSlot
	version, build := currentVersion, currentBuild
	previousVersion, previousBuild := "", ""
	if showTarget {
		version, build = targetVersion, targetBuild
		previousVersion, previousBuild = currentVersion, currentBuild
	}
	health := ""
	switch {
	case phase == "health" && lastError != "":
		health = "failed"
	case phase == "pending-reboot" && showTarget:
		health = "pending"
	case phase == "committed":
		health = "success"
	case phase == "rolled-back":
		health = "rolled_back"
	}
	healthError := ""
	if health == "failed" {
		healthError = lastError
	}
	return map[string]any{
		"version": version, "build_time": build, "phase": phase,
		"current_version": currentVersion, "current_build_time": currentBuild,
		"target_version": targetVersion, "target_build_time": targetBuild,
		"previous_version": previousVersion, "previous_build_time": previousBuild,
		"running_slot": runningSlot, "target_slot": targetSlot,
		"health_status": health, "health_error": healthError,
		"components": components,
	}
}

// A fresh factory image has no OTA transaction state yet. Read its shipped
// baseline without creating state or invoking the updater from a status GET.
// Existing (including unreadable or malformed) state must never be replaced
// with a potentially stale factory version.
func (s *Server) factoryFirmwareInfo() map[string]any {
	info := emptyFirmwareInfo()
	data, err := readFileLimited(s.options.OTAConfigPath, maxAgentConfigSize)
	if err != nil {
		return info
	}
	var config map[string]any
	if json.Unmarshal(data, &config) != nil {
		return info
	}
	version := strings.TrimSpace(jsonString(config, "factory_version"))
	if version == "" {
		return info
	}
	build := strings.TrimSpace(jsonString(config, "factory_build_time"))
	info["version"], info["current_version"] = version, version
	info["build_time"], info["current_build_time"] = build, build
	info["phase"] = "factory"
	// Both factory slots contain the boot/rootfs baseline from the same release.
	info["components"] = map[string]string{"boot": version, "rootfs": version}
	if cmdline, err := readFileLimited(s.options.CmdlinePath, 16*1024); err == nil {
		info["running_slot"] = currentSlot(string(cmdline))
	}
	return info
}

func jsonString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func normalizeSlot(value any) string {
	switch typed := value.(type) {
	case float64:
		if typed == 0 {
			return "a"
		}
		if typed == 1 {
			return "b"
		}
	case string:
		normalized := strings.ToLower(strings.TrimSpace(typed))
		if normalized == "a" || normalized == "_a" || normalized == "slot_a" || normalized == "0" {
			return "a"
		}
		if normalized == "b" || normalized == "_b" || normalized == "slot_b" || normalized == "1" {
			return "b"
		}
	}
	return ""
}

func currentSlot(cmdline string) string {
	rootSlot := ""
	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "aico.slot_suffix=") {
			if slot := normalizeSlot(strings.TrimPrefix(field, "aico.slot_suffix=")); slot != "" {
				return slot
			}
		}
		if strings.HasPrefix(field, "root=") {
			value := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(field, "root=")))
			switch {
			case value == "partlabel=rootfs_a", value == "rootfs_a", value == "/dev/mmcblk1p7", strings.HasSuffix(value, "/rootfs_a"):
				rootSlot = "a"
			case value == "partlabel=rootfs_b", value == "rootfs_b", value == "/dev/mmcblk1p8", strings.HasSuffix(value, "/rootfs_b"):
				rootSlot = "b"
			}
		}
	}
	return rootSlot
}

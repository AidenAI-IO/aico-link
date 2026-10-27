package configweb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (s *Server) llmLogDir() string {
	return filepath.Join(s.options.ConfigDir(), "log")
}

func validLLMLogName(name string) bool {
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." ||
		!strings.HasPrefix(name, "llm-http-") || !strings.HasSuffix(name, ".log") || len(name) <= len("llm-http-.log") {
		return false
	}
	for _, value := range []byte(name) {
		if value < 0x20 || value == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) handleLLMLogs(w http.ResponseWriter, _ *http.Request) {
	type logFile struct {
		Name      string `json:"name"`
		SizeBytes int64  `json:"size_bytes"`
		MTime     int64  `json:"mtime"`
	}
	files := []logFile{}
	entries, err := os.ReadDir(s.llmLogDir())
	if err == nil {
		for _, entry := range entries {
			if entry.Type()&os.ModeSymlink != 0 || !validLLMLogName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			files = append(files, logFile{Name: entry.Name(), SizeBytes: info.Size(), MTime: info.ModTime().Unix()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	writeJSON(w, 200, map[string]any{"ok": true, "files": files})
}

func decodedLogSegment(encoded string) (string, bool) {
	name, err := url.PathUnescape(encoded)
	return name, err == nil && validLLMLogName(name)
}

func openRegularNoSymlink(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, os.ErrNotExist
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err == nil {
			err = os.ErrNotExist
		}
		return nil, nil, err
	}
	return file, info, nil
}

func (s *Server) handleLLMLogExportName(w http.ResponseWriter, encodedName string) {
	name, ok := decodedLogSegment(encodedName)
	if !ok {
		writeJSONError(w, 400, "invalid log file name")
		return
	}
	file, info, err := openRegularNoSymlink(filepath.Join(s.llmLogDir(), name))
	if err != nil {
		if os.IsNotExist(err) {
			writeJSONError(w, 404, "log file not found")
		} else {
			writeJSONError(w, 500, "failed to open log file: "+err.Error())
		}
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	_, _ = io.Copy(w, file)
}

func (s *Server) handleLLMLogImportName(w http.ResponseWriter, r *http.Request, encodedName string) {
	name, ok := decodedLogSegment(encodedName)
	if !ok {
		writeJSONError(w, 400, "invalid log file name")
		return
	}
	if err := os.MkdirAll(s.llmLogDir(), 0o755); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	temporary, err := os.CreateTemp(s.llmLogDir(), "llm-log-upload-*")
	if err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	body := http.MaxBytesReader(w, r.Body, maxRequestBodySize)
	size, copyErr := io.Copy(temporary, body)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	var maxErr *http.MaxBytesError
	if errors.As(copyErr, &maxErr) {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "log file is too large")
		return
	}
	if copyErr != nil || syncErr != nil || closeErr != nil {
		if copyErr == nil {
			copyErr = syncErr
		}
		if copyErr == nil {
			copyErr = closeErr
		}
		writeJSONError(w, 500, "failed to import log file: "+copyErr.Error())
		return
	}
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	if err := os.Rename(temporaryPath, filepath.Join(s.llmLogDir(), name)); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": name, "size_bytes": size, "message": "llm log imported"})
}

func readStorageState(path string) map[string]string {
	data, err := readFileLimited(path, 16*1024)
	if err != nil || len(data) == 0 {
		return nil
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}
func stateBool(values map[string]string, key string) bool     { return values[key] == "1" }
func stateString(values map[string]string, key string) string { return values[key] }
func stateInt(values map[string]string, key string, fallback int) int {
	value, err := strconv.Atoi(values[key])
	if err != nil || value < 0 || value > 1000000 {
		return fallback
	}
	return value
}
func stateInt64(values map[string]string, key string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(values[key]), 10, 64)
	if err != nil || value < 0 {
		return 0
	}
	return value
}

func (s *Server) storageStatusValue() map[string]any {
	if storage := s.currentStorage(); storage != nil {
		if status := s.storageStatusResponse(storage.Status()); status != nil {
			return status
		}
	}
	values := readStorageState(s.options.StorageStatePath)
	jobStatus := stateString(values, "FORMAT_STATUS")
	if jobStatus == "" {
		jobStatus = "idle"
	}
	migrationStatus := stateString(values, "MIGRATE_STATUS")
	if migrationStatus == "" {
		migrationStatus = "idle"
	}
	mode := stateInt(values, "EFFECTIVE_MODE", 1)
	if mode != 1 && mode != 2 {
		mode = 1
	}
	mountPoint := stateString(values, "SD_MOUNTPOINT")
	if mountPoint == "" {
		mountPoint = "/mnt/sdcard"
	}
	status := map[string]any{
		"effective_mode": mode,
		"card": map[string]any{
			"present":     stateBool(values, "SD_PRESENT"),
			"mounted":     stateBool(values, "SD_MOUNTED"),
			"device":      stateString(values, "SD_DEVICE"),
			"total_bytes": stateInt64(values, "SD_TOTAL_BYTES"),
			"free_bytes":  stateInt64(values, "SD_FREE_BYTES"),
			"reason":      stateString(values, "REASON"),
		},
		"mount_point": mountPoint,
		"format_job": map[string]any{
			"status": jobStatus,
			"fs":     stateString(values, "FORMAT_FS"),
			"auto":   stateBool(values, "FORMAT_AUTO"),
			"error":  stateString(values, "FORMAT_ERROR"),
		},
		"migration": map[string]any{
			"status":      migrationStatus,
			"detail":      stateString(values, "MIGRATE_DETAIL"),
			"error":       stateString(values, "MIGRATE_ERROR"),
			"moved_files": stateInt(values, "MIGRATE_MOVED_FILES", 0),
			"moved_bytes": stateInt64(values, "MIGRATE_MOVED_BYTES"),
		},
	}
	status["internal"] = filesystemSpaceValue(filepath.Dir(s.options.AgentConfigPath))
	return status
}

func tailFile(path string, limit int64) ([]byte, error) {
	file, info, err := openRegularNoSymlink(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	offset := int64(0)
	truncated := false
	if info.Size() > limit {
		offset = info.Size() - limit
		truncated = true
	}
	if _, err := file.Seek(offset, 0); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return nil, err
	}
	if truncated {
		return append([]byte(fmt.Sprintf("# truncated: copied latest %d of %d bytes from %s\n", limit, info.Size(), path)), data...), nil
	}
	return data, nil
}

func (s *Server) latestEpisodeYAML() string {
	best := ""
	var bestTime int64
	root := filepath.Join(s.options.ConfigDir(), "memory", "episodes")
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "episode.yaml" || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && (best == "" || info.ModTime().Unix() > bestTime || (info.ModTime().Unix() == bestTime && path > best)) {
			best = path
			bestTime = info.ModTime().Unix()
		}
		return nil
	})
	return best
}
func (s *Server) latestLLMLog() string {
	entries, _ := os.ReadDir(s.llmLogDir())
	best := ""
	var bestTime int64
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !validLLMLogName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() && (best == "" || info.ModTime().Unix() > bestTime || (info.ModTime().Unix() == bestTime && entry.Name() > filepath.Base(best))) {
			best = filepath.Join(s.llmLogDir(), entry.Name())
			bestTime = info.ModTime().Unix()
		}
	}
	return best
}

const (
	supportLogTailLimit = 1024 * 1024
	supportDmesgLimit   = 1024 * 1024
)

type supportArchiveFile struct {
	name string
	data []byte
}

// These are the files written by the systemd services in the device image.
// Keep this list explicit rather than archiving /var/log wholesale: the latter
// can contain unrelated data and can grow without a useful bound.
func (s *Server) supportServiceLogs() []struct {
	name string
	path string
} {
	return []struct {
		name string
		path string
	}{
		{"services/frame_service.log", "/var/log/frame_service/frame_service.log"},
		{"services/audio_service.log", "/var/log/audio_service/audio_service.log"},
		{"services/ble_service.log", "/var/log/ble_service/ble_service.log"},
		{"services/adb-startup.log", "/var/log/adb/adb-startup.log"},
		{"services/ota.log", s.options.OTAHealthLogPath},
		{"services/ota-recovery.log", "/var/log/ota/ota-recovery.log"},
		{"services/ttyd.log", "/var/log/ttyd/ttyd.log"},
		{"services/aico-hciattach.log", "/var/log/aico-hciattach.log"},
		{"services/bluetoothd.log", "/var/log/bluetoothd/bluetoothd.log"},
		{"services/wifi-driver.log", "/var/log/wifi_driver/wifi_driver.log"},
		{"services/wpa-supplicant.log", "/var/log/wpa_supplicant/wlan0.log"},
		{"services/wlan-guard.log", "/var/log/wlan_guard/wlan_guard.log"},
		{"services/wifi-proxy.log", "/var/log/wifi_proxy/wifi_proxy.log"},
		{"services/boot-timeline.log", "/var/log/aico_boot_timeline.log"},
		{"services/ota-update.log", s.options.OTAUpdateLogPath},
	}
}

func unavailableSupportFile(name, source string, err error) supportArchiveFile {
	message := "Log unavailable\nsource: " + source
	if err != nil {
		message += "\ncopy_error: " + err.Error()
	}
	message += "\n"
	return supportArchiveFile{name: name, data: []byte(message)}
}

func tailBytes(data []byte, limit int64, source string) []byte {
	if int64(len(data)) <= limit {
		return data
	}
	marker := []byte(fmt.Sprintf("# truncated: copied latest %d of %d bytes from %s\n", limit, len(data), source))
	return append(marker, data[len(data)-int(limit):]...)
}

func (s *Server) supportDmesg() supportArchiveFile {
	binary := strings.TrimSpace(s.options.DmesgBinary)
	if binary == "" {
		binary = "dmesg"
	}
	result := runDmesgCommand(10*time.Second, binary)
	if result.ExitCode != 0 {
		err := fmt.Errorf("command exited with status %d", result.ExitCode)
		if result.TimedOut {
			err = context.DeadlineExceeded
		}
		if output := strings.TrimSpace(string(result.Output)); output != "" {
			err = fmt.Errorf("%w: %s", err, output)
		}
		return unavailableSupportFile("dmesg.log", binary, err)
	}
	return supportArchiveFile{name: "dmesg.log", data: tailBytes(result.Output, supportDmesgLimit, binary)}
}

func runDmesgCommand(timeout time.Duration, binary string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	output, err := cmd.CombinedOutput()
	result := commandResult{Output: output, ExitCode: 0}
	if ctx.Err() != nil {
		result.ExitCode = -1
		result.TimedOut = true
		return result
	}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
	} else {
		result.ExitCode = 127
		if len(output) == 0 {
			result.Output = []byte(err.Error())
		}
	}
	return result
}

func (s *Server) handleSupportLogsExport(w http.ResponseWriter, _ *http.Request) {
	files := []supportArchiveFile{}
	episode := s.latestEpisodeYAML()
	if episode == "" {
		files = append(files, supportArchiveFile{"langfuse.yaml", []byte("Langfuse episode data unavailable\nNo episode.yaml files found under " + filepath.Join(s.options.ConfigDir(), "memory", "episodes") + ".\n")})
	} else if data, err := tailFile(episode, 1024*1024); err == nil {
		files = append(files, supportArchiveFile{"langfuse.yaml", data})
	} else {
		files = append(files, unavailableSupportFile("langfuse.yaml", episode, err))
	}
	agentPath := s.agentLogPath()
	if data, err := tailFile(agentPath, 1024*1024); err == nil {
		files = append(files, supportArchiveFile{"agent.log", data})
	} else {
		files = append(files, unavailableSupportFile("agent.log", agentPath, err))
	}
	llm := s.latestLLMLog()
	llmName := "http.log"
	if llm != "" {
		llmName = filepath.Base(llm)
	}
	if data, err := tailFile(llm, 4*1024*1024); llm != "" && err == nil {
		files = append(files, supportArchiveFile{llmName, data})
	} else if llm == "" {
		files = append(files, unavailableSupportFile(llmName, s.llmLogDir(), fmt.Errorf("no llm-http-*.log files found")))
	} else {
		files = append(files, unavailableSupportFile(llmName, llm, err))
	}
	for _, service := range s.supportServiceLogs() {
		if data, err := tailFile(service.path, supportLogTailLimit); err == nil {
			files = append(files, supportArchiveFile{name: service.name, data: data})
		} else {
			files = append(files, unavailableSupportFile(service.name, service.path, err))
		}
	}
	files = append(files, s.supportDmesg())
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		header := &tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.data))}
		if err := tw.WriteHeader(header); err != nil {
			writeJSONError(w, 500, err.Error())
			return
		}
		if _, err := tw.Write(file.data); err != nil {
			writeJSONError(w, 500, err.Error())
			return
		}
	}
	if err := tw.Close(); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	if err := gz.Close(); err != nil {
		writeJSONError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.Itoa(output.Len()))
	_, _ = w.Write(output.Bytes())
}

package configweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/logging"
	"github.com/BurntSushi/toml"
)

type storageController interface {
	Status() agent.StorageStatus
	Reconfigure(agent.StorageConfig) error
	SafeEject() error
	StartFormat(fs, confirm string) error
	AcquireSnapshotLease(context.Context) (agent.StorageSnapshotLease, error)
	Stop()
}

// storageConfigForHardware returns the SD-card settings Config Web may hand to the
// manager, which is the only process allowed to mount, format, eject, or migrate the
// card. Recovery mode starts the portal from a config the Agent runtime rejected, and
// that rejection can name a storage rule, so fall back to the built-in defaults
// instead of driving the card with settings Config.Validate() refuses.
func storageConfigForHardware(cfg agent.StorageConfig) agent.StorageConfig {
	if err := cfg.MonitorConfig().Validate(); err != nil {
		logging.Warnf("config_web", "config_web", "Agent storage settings are invalid; using defaults until repaired: %v", err)
		return agent.DefaultConfig().Storage
	}
	return cfg
}

func (s *Server) initializeStorageManager() error {
	cfg, err := agent.LoadResolvedConfigForUpdate(s.options.AgentConfigPath)
	if err != nil {
		return fmt.Errorf("load Agent config: %w", err)
	}
	manager := agent.NewStorageManagerWithStatePath(storageConfigForHardware(cfg.Storage), s.options.StorageStatePath, nil)
	s.storageMu.Lock()
	if s.storage != nil {
		s.storageMu.Unlock()
		return nil
	}
	manager.Start()
	s.storage = manager
	s.storageMu.Unlock()
	return nil
}

func (s *Server) currentStorage() storageController {
	s.storageMu.RLock()
	defer s.storageMu.RUnlock()
	return s.storage
}

func (s *Server) reconfigureStorage() error {
	cfg, err := agent.LoadResolvedConfigForUpdate(s.options.AgentConfigPath)
	if err != nil {
		return fmt.Errorf("load Agent config: %w", err)
	}
	storage := s.currentStorage()
	if storage == nil {
		return s.initializeStorageManager()
	}
	if err := storage.Reconfigure(storageConfigForHardware(cfg.Storage)); err != nil {
		return fmt.Errorf("apply storage config: %w", err)
	}
	return nil
}

func (s *Server) handleStorageStatus(w http.ResponseWriter, _ *http.Request) {
	storage := s.currentStorage()
	if storage == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "storage manager unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.storageStatusResponse(storage.Status()))
}

func (s *Server) handleStorageEject(w http.ResponseWriter, _ *http.Request) {
	storage := s.currentStorage()
	if storage == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "storage manager unavailable")
		return
	}
	if err := storage.SafeEject(); err != nil {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.storageStatusResponse(storage.Status()))
}

func (s *Server) handleStorageFormat(w http.ResponseWriter, r *http.Request) {
	storage := s.currentStorage()
	if storage == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "storage manager unavailable")
		return
	}
	var request struct {
		FS      string `json:"fs"`
		Confirm string `json:"confirm"`
	}
	if !readJSONBody(w, r, &request) {
		return
	}
	if err := storage.StartFormat(request.FS, request.Confirm); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.storageStatusResponse(storage.Status()))
}

func (s *Server) handleConfigBackupExport(w http.ResponseWriter, _ *http.Request) {
	data, err := readFileLimited(s.options.AgentConfigPath, maxAgentConfigSize)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent configuration is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/toml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="aiden-config-%s.toml"`, time.Now().Format("20060102-150405")))
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) handleConfigBackupImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentConfigSize)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "configuration backup is too large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "configuration backup could not be read")
		return
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		writeJSONError(w, http.StatusBadRequest, "configuration backup is empty")
		return
	}

	s.configSaveMu.Lock()
	defer s.configSaveMu.Unlock()
	target, mode, err := resolveAgentConfigTarget(s.options.AgentConfigPath)
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "agent configuration path is unavailable")
		return
	}
	current, currentErr := agent.LoadResolvedConfig(target)
	candidate, err := validateAgentConfigBackup(filepath.Dir(target), data, mode)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid configuration backup: "+err.Error())
		return
	}
	if err := atomicWriteFile(target, data, mode); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "configuration backup could not be saved")
		return
	}
	if directory, openErr := os.Open(filepath.Dir(target)); openErr == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}

	revision := configDataRevision(data)
	s.configMu.Lock()
	s.configSavePending = true
	if currentErr != nil || current.FrameService != candidate.FrameService {
		s.frameApplyPending = true
	}
	if currentErr != nil || !reflect.DeepEqual(current.Storage, candidate.Storage) {
		s.storageApplyPending = true
	}
	s.configMu.Unlock()
	defer s.finishConfigSave()
	if err := s.applyConfigServices(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "persisted": true, "applied": false, "pending": false,
			"revision": revision, "error": err.Error(),
		})
		return
	}
	payload, err := s.reloadAgentConfig(r.Context(), revision)
	if err != nil {
		s.configMu.Lock()
		s.configApplyError = err.Error()
		s.configApplyErrorRevision = revision
		s.configMu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "persisted": true, "applied": false, "pending": false,
			"revision": revision, "error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "persisted": true, "applied": payload["applied"],
		"pending": payload["pending"], "state": payload["state"],
		"reboot_required": payload["reboot_required"], "revision": revision,
	})
}

func resolveAgentConfigTarget(path string) (string, os.FileMode, error) {
	if _, err := os.Lstat(path); err == nil {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", 0, err
		}
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = fmt.Errorf("configuration target is not a regular file")
			}
			return "", 0, err
		}
		return resolved, info.Mode().Perm(), nil
	} else if !os.IsNotExist(err) {
		return "", 0, err
	}
	resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", 0, err
	}
	return filepath.Join(resolvedDir, filepath.Base(path)), 0o640, nil
}

func validateAgentConfigBackup(dir string, data []byte, mode os.FileMode) (agent.Config, error) {
	if err := validateCanonicalConfigBackup(data); err != nil {
		return agent.Config{}, err
	}
	temporary, err := os.CreateTemp(dir, ".agent-config-import-*.toml")
	if err != nil {
		return agent.Config{}, err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return agent.Config{}, err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return agent.Config{}, err
	}
	if err := temporary.Close(); err != nil {
		return agent.Config{}, err
	}
	candidate, err := agent.LoadResolvedConfig(temporaryPath)
	if err != nil {
		return agent.Config{}, err
	}
	if err := candidate.ValidateVoiceProviders(); err != nil {
		return agent.Config{}, err
	}
	return candidate, nil
}

func validateCanonicalConfigBackup(data []byte) error {
	var root map[string]any
	if _, err := toml.Decode(string(data), &root); err != nil {
		return err
	}
	if len(root) == 0 {
		return fmt.Errorf("backup must contain grouped configuration values")
	}
	allowedRoots := map[string]bool{
		"basic_settings": true, "conversation_settings": true, "model_settings": true,
		"voice_settings": true, "memory_settings": true, "storage_settings": true,
		"advanced_settings": true,
	}
	for name, value := range root {
		if !allowedRoots[name] {
			return fmt.Errorf("unsupported top-level table %q; use the grouped configuration schema", name)
		}
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("top-level key %q must be a table", name)
		}
	}

	checks := []struct {
		path    []string
		allowed []string
	}{
		{[]string{"basic_settings"}, []string{"language_timezone", "device"}},
		{[]string{"basic_settings", "device"}, []string{"hid"}},
		{[]string{"conversation_settings"}, []string{"agent", "search", "termination_policy"}},
		{[]string{"model_settings"}, []string{"model", "providers", "decision"}},
		{[]string{"voice_settings"}, []string{"mode", "realtime", "classic"}},
		{[]string{"voice_settings", "realtime"}, []string{"providers"}},
		{[]string{"voice_settings", "classic"}, []string{"runtime", "stt", "tts", "audio", "audio_archive"}},
		{[]string{"voice_settings", "classic", "stt"}, []string{"providers"}},
		{[]string{"voice_settings", "classic", "tts"}, []string{"providers"}},
		{[]string{"memory_settings"}, []string{"screen", "notification"}},
		{[]string{"memory_settings", "notification"}, []string{"response_tail", "expiration"}},
		{[]string{"memory_settings", "notification", "expiration"}, []string{"code_ttl_seconds"}},
		{[]string{"storage_settings"}, []string{"storage"}},
		{[]string{"storage_settings", "storage"}, []string{"degraded_mode", "cleanup"}},
		{[]string{"advanced_settings"}, []string{"log", "hardware", "runtime"}},
		{[]string{"advanced_settings", "hardware"}, []string{"hid", "frame_service"}},
		{[]string{"advanced_settings", "runtime"}, []string{"live_activity", "telemetry", "ota"}},
	}
	for _, check := range checks {
		if err := rejectUnknownChildTables(root, check.path, check.allowed); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnknownChildTables(root map[string]any, path, allowed []string) error {
	table := root
	for _, segment := range path {
		value, exists := table[segment]
		if !exists {
			return nil
		}
		next, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be a table", strings.Join(path, "."))
		}
		table = next
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	for name, value := range table {
		if _, isTable := value.(map[string]any); isTable && !allowedSet[name] {
			return fmt.Errorf("unsupported table %q", strings.Join(append(append([]string{}, path...), name), "."))
		}
	}
	return nil
}

func configDataRevision(data []byte) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write(data)
	return hash.Sum64()
}

func storageStatusMap(status agent.StorageStatus) map[string]any {
	data, err := json.Marshal(status)
	if err != nil {
		return nil
	}
	var value map[string]any
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	return value
}

func (s *Server) storageStatusResponse(status agent.StorageStatus) map[string]any {
	value := storageStatusMap(status)
	if value == nil {
		value = map[string]any{}
	}
	value["internal"] = filesystemSpaceValue(filepath.Dir(s.options.AgentConfigPath))
	return value
}

func filesystemSpaceValue(path string) map[string]any {
	value := map[string]any{
		"available":   false,
		"total_bytes": int64(0),
		"free_bytes":  int64(0),
	}
	var status syscall.Statfs_t
	if err := syscall.Statfs(path, &status); err != nil {
		return value
	}
	blockSize := int64(status.Bsize)
	value["available"] = true
	value["total_bytes"] = int64(status.Blocks) * blockSize
	value["free_bytes"] = int64(status.Bavail) * blockSize
	return value
}

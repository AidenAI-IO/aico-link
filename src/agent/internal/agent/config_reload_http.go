package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type configReloadRequest struct {
	Revision uint64 `json:"revision"`
}

// ConfigRevision is the revision of agent.toml contents that Config Web and
// configupdate report and the reload endpoint checks against the file on disk.
func ConfigRevision(data []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(data)
	return h.Sum64()
}

// ConfigFileRevision returns ConfigRevision of the file at path, or 0 when it
// cannot be read.
func ConfigFileRevision(path string) uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return ConfigRevision(data)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleInternalConfigReload is intentionally not part of the public Agent
// API. Config Web persists agent.toml and calls this loopback-only endpoint to
// make the new revision visible to the running runtime.
func (s *Server) handleInternalConfigReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !isLoopbackRequest(r) {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if s.runtime == nil {
		writeAgentJSONError(w, http.StatusServiceUnavailable, "runtime unavailable")
		return
	}
	if r.Method == http.MethodGet {
		writeAgentJSON(w, http.StatusOK, s.runtime.ConfigApplyStatus())
		return
	}
	var request configReloadRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		// A caller that sent a revision but a malformed body must not be told
		// the revision was missing; the decoder reason names the real problem.
		writeAgentJSONError(w, http.StatusBadRequest, "invalid reload request: "+err.Error())
		return
	}
	// The body must hold exactly one object. A second decoded value means the
	// caller sent more than one document; any other error is the decoder's own
	// reason. Neither may be reported as a missing revision.
	err := decoder.Decode(&struct{}{})
	if err == nil {
		writeAgentJSONError(w, http.StatusBadRequest, "invalid reload request: body must contain a single JSON object")
		return
	}
	if err != io.EOF {
		writeAgentJSONError(w, http.StatusBadRequest, "invalid reload request: "+err.Error())
		return
	}
	if request.Revision == 0 {
		writeAgentJSONError(w, http.StatusBadRequest, "reload request requires a nonzero revision")
		return
	}
	status, err := s.runtime.ReloadPersistedConfig(request.Revision)
	switch {
	case errors.Is(err, ErrConfigRevisionStale):
		writeAgentJSONError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeAgentJSONError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeAgentJSON(w, http.StatusAccepted, map[string]any{
		"ok": status.Error == "", "applied": false, "pending": status.Pending,
		"persisted": true, "revision": request.Revision, "state": status.State, "error": status.Error,
		"reboot_required": status.RebootRequired,
		"runtime_id":      s.runtime.ConfigApplyStatus().RuntimeID,
	})
}

// ErrConfigRevisionStale reports that agent.toml no longer holds the revision a
// caller asked to apply. A newer save superseded it and will apply on its own.
var ErrConfigRevisionStale = errors.New("stale config revision")

// ReloadPersistedConfig queues the persisted agent.toml for runtime
// application, provided the file still holds revision. It returns before the
// config applies: the running task keeps its config until its boundary.
func (r *Runtime) ReloadPersistedConfig(revision uint64) (ConfigApplyStatus, error) {
	current := r.ConfigSnapshot()
	configPath := filepath.Join(current.ConfigDir, "agent.toml")
	onDisk := ConfigFileRevision(configPath)
	if onDisk == 0 {
		return ConfigApplyStatus{}, fmt.Errorf("config file unavailable")
	}
	if revision != onDisk {
		return ConfigApplyStatus{}, fmt.Errorf("%w %d (current %d)", ErrConfigRevisionStale, revision, onDisk)
	}
	cfg, err := LoadRuntimeConfigFromDir(current.ConfigDir)
	if err != nil {
		return ConfigApplyStatus{}, fmt.Errorf("reload config: %w", err)
	}
	if ConfigFileRevision(configPath) != revision {
		return ConfigApplyStatus{}, fmt.Errorf("%w: config changed while loading; retry latest revision", ErrConfigRevisionStale)
	}
	// Preserve process-only fields; the persisted config cannot overwrite CLI overrides.
	cfg.SkillMergeModel = current.SkillMergeModel
	cfg.EnvironmentBridge = current.EnvironmentBridge
	cfg.Benchmark = current.Benchmark
	cfg.ForceSimpleLoop = current.ForceSimpleLoop
	if current.DeviceTypeOverride != "" {
		_ = cfg.OverrideDeviceType(current.DeviceTypeOverride)
	}
	return r.QueueConfig(cfg, revision), nil
}

func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAgentJSONError(w http.ResponseWriter, status int, message string) {
	writeAgentJSON(w, status, map[string]any{"ok": false, "applied": false, "error": message})
}

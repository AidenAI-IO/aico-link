package configweb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/backup"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	backupJobCreated    = "created"
	backupJobReady      = "ready"
	backupJobQuiescing  = "quiescing"
	backupJobStreaming  = "streaming"
	backupJobResuming   = "resuming_services"
	backupJobCompleted  = "completed"
	backupJobCancelled  = "cancelled"
	backupJobFailed     = "failed"
	maxBackupPassphrase = 4096

	// backupReadyTimeout is how long a created job keeps its derived key while
	// the user is still in the browser's save dialog.  backupStallTimeout
	// cancels a stream that makes no progress (a stalled download manager)
	// and backupTotalTimeout caps the whole stream.
	backupReadyTimeout = 5 * time.Minute
	backupStallTimeout = 120 * time.Second
	backupTotalTimeout = 4 * time.Hour
)

type backupAPIError struct {
	Code      string
	Status    int
	Message   string
	Retryable bool
	Details   map[string]any
}

func (e *backupAPIError) Error() string { return e.Message }

type backupJobStore struct {
	server *Server
	root   string
	mu     sync.Mutex
	jobs   map[string]*backupJob
}

type backupJob struct {
	mu             sync.Mutex
	id             string
	createdAt      time.Time
	startedAt      time.Time
	finishedAt     time.Time
	lastProgress   time.Time
	state          string
	phase          string
	mode           backup.Mode
	components     []backup.ComponentID
	plan           *backup.Plan
	material       *backup.KeyMaterial
	transferExpiry time.Time
	estimatedBytes int64
	filesProcessed int
	bytesRead      int64
	errorCode      string
	errorMessage   string
	cancel         context.CancelFunc
	done           chan struct{}
}

type backupCreateRequest struct {
	FormatVersion int         `json:"format_version"`
	Mode          backup.Mode `json:"mode"`
	Components    []string    `json:"components"`
	Protection    struct {
		Mode       string `json:"mode"`
		Passphrase string `json:"passphrase"`
	} `json:"protection"`
}

func newBackupJobStore(server *Server, root string) *backupJobStore {
	return &backupJobStore{server: server, root: root, jobs: make(map[string]*backupJob)}
}

func (s *backupJobStore) cancelAll() {
	s.mu.Lock()
	jobs := make([]*backupJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.Unlock()
	for _, job := range jobs {
		s.server.cancelBackupJob(job, "server_shutdown", "backup interrupted by service shutdown")
	}
}

func (s *backupJobStore) get(id string) (*backupJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *backupJobStore) put(job *backupJob) error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	s.jobs[job.id] = job
	s.mu.Unlock()
	return nil
}

func (s *backupJobStore) remove(id string) {
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
}

func (s *backupJobStore) expireReady(id string) {
	job, ok := s.get(id)
	if !ok {
		return
	}
	job.mu.Lock()
	if job.state != backupJobReady {
		job.mu.Unlock()
		return
	}
	job.mu.Unlock()
	s.server.cancelBackupJob(job, "job_expired", "backup download was not started before the key expired")
}

func (s *backupJobStore) persist(job *backupJob) {
	if s.root == "" || job == nil {
		return
	}
	job.mu.Lock()
	payload := job.publicMapLocked()
	id := job.id
	job.mu.Unlock()
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(s.root, 0o700)
	tmp := filepath.Join(s.root, "."+id+".tmp")
	path := filepath.Join(s.root, id+".json")
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
		if dir, err := os.Open(s.root); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
}

func (j *backupJob) publicMapLocked() map[string]any {
	result := map[string]any{
		"job_id": j.id, "operation": "backup", "state": j.state, "phase": j.phase,
		"mode": j.mode, "components": j.components,
		"files_processed": j.filesProcessed, "bytes_read": j.bytesRead,
		"estimated_bytes": j.estimatedBytes, "created_at": j.createdAt,
		"started_at": j.startedAt, "finished_at": j.finishedAt,
		"cancelable": !isTerminalBackupState(j.state),
	}
	if j.errorCode != "" {
		result["error"] = map[string]any{"code": j.errorCode, "message": j.errorMessage}
	} else {
		result["error"] = nil
	}
	return result
}

func (j *backupJob) publicMap() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.publicMapLocked()
}

func (s *Server) requireSession(w http.ResponseWriter, r *http.Request, mutate bool) bool {
	if s.maintenanceSessions == nil || !s.maintenanceSessions.authorize(r, mutate) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "maintenance_session_required"})
		return false
	}
	return true
}

func (s *Server) writeMaintenanceLocked(w http.ResponseWriter) {
	payload := map[string]any{"ok": false, "error": "maintenance_in_progress", "retryable": true}
	if snapshot, ok := s.maintenance.snapshot(); ok {
		payload["operation"], payload["job_id"], payload["phase"] = snapshot.Operation, snapshot.JobID, snapshot.Phase
	}
	writeJSON(w, http.StatusLocked, payload)
}

func (s *Server) handleMaintenanceSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	token, csrf, expiry, err := s.maintenanceSessions.create(r)
	if err != nil {
		s.writeBackupError(w, err)
		return
	}
	hardware := s.readHardwareID()
	w.Header().Set("Set-Cookie", "aico_maintenance="+token+"; Path=/api; HttpOnly; SameSite=Strict")
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "csrf_token": csrf, "expires_at": expiry,
		"device": map[string]any{"hardware_id": hardware, "firmware_version": agent.FirmwareVersion()},
	})
}

// handleMaintenanceCurrent lets a reloaded page re-attach to the running job.
// It never returns passphrases, keys or tokens.
func (s *Server) handleMaintenanceCurrent(w http.ResponseWriter, r *http.Request) {
	if !s.maintenanceSessions.authorizeStatus(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "usb_required"})
		return
	}
	snapshot, ok := s.maintenance.snapshot()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	payload := map[string]any{
		"operation": snapshot.Operation, "job_id": snapshot.JobID, "phase": snapshot.Phase,
		"started_at": snapshot.StartedAt, "cancelable": snapshot.Cancelable,
	}
	switch snapshot.Operation {
	case "backup":
		if job, found := s.backupJobs.get(snapshot.JobID); found {
			payload["job"] = job.publicMap()
		}
	case "restore":
		if job, found := s.restoreJobs.get(snapshot.JobID); found {
			payload["job"] = job.publicMap()
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) backupRoots() backup.Roots {
	return backup.Roots{Userdata: s.options.BackupUserdataRoot, SD: s.options.BackupSDRoot}
}

func (s *Server) handleBackupCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r, false) {
		return
	}
	status := agent.StorageStatus{}
	if storage := s.currentStorage(); storage != nil {
		status = storage.Status()
	}
	sdAvailable := status.Card.Present && status.Card.Mounted
	roots := s.backupRoots()
	components := make([]map[string]any, 0)
	for _, definition := range backup.ComponentDefinitions() {
		available := !definition.RequiresSD || sdAvailable
		item := map[string]any{
			"id": definition.ID, "available": available,
			"default_selected": available,
			"same_device_only": definition.SameDeviceOnly,
			"sensitive":        definition.Sensitive, "advanced": definition.Advanced,
			"schema_version": definition.SchemaVersion,
		}
		if available {
			item["estimated_size"] = estimateBackupDefinitions([]backup.ComponentDefinition{definition}, roots)
		}
		components = append(components, item)
	}
	s.configMu.Lock()
	recovery := s.restoreRecovery
	s.configMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"format_versions": []int{backup.FormatVersion}, "components": components,
		"sd": map[string]any{"present": status.Card.Present, "mounted": status.Card.Mounted,
			"uuid": s.sdFilesystemUUID(status), "device": status.Card.Device},
		"device": map[string]any{"hardware_id": s.readHardwareID(), "firmware_version": agent.FirmwareVersion(),
			"identity_available": s.readHardwareID() != ""},
		"maintenance_busy": s.maintenance.active(),
		"last_recovery":    recovery,
	})
}

// sdFilesystemUUID reads the mounted card's UUID without taking a lease; a
// short-lived lease would needlessly interrupt background migration.
func (s *Server) sdFilesystemUUID(status agent.StorageStatus) string {
	if !status.Card.Present || !status.Card.Mounted || strings.TrimSpace(status.Card.Device) == "" {
		return ""
	}
	result := runCommand(5*time.Second, nil, nil, "blkid", "-s", "UUID", "-o", "value", status.Card.Device)
	if result.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(string(result.Output))
}

func (s *Server) handleCreateBackupJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r, true) {
		return
	}
	var request backupCreateRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	if request.FormatVersion == 0 {
		request.FormatVersion = backup.FormatVersion
	}
	if request.FormatVersion != backup.FormatVersion {
		s.writeBackupError(w, &backupAPIError{Code: "unsupported_format", Status: http.StatusBadRequest, Message: "unsupported backup format version"})
		return
	}
	if request.Mode == "" {
		request.Mode = backup.ModeSameDevice
	}
	if request.Mode != backup.ModeSameDevice {
		s.writeBackupError(w, &backupAPIError{Code: "unsupported_mode", Status: http.StatusBadRequest, Message: "backups use same_device mode"})
		return
	}
	if len(request.Components) != 0 {
		s.writeBackupError(w, &backupAPIError{Code: "custom_selection_unsupported", Status: http.StatusBadRequest, Message: "all available components are backed up automatically"})
		return
	}
	if (request.Protection.Mode != "" && request.Protection.Mode != "none") || request.Protection.Passphrase != "" {
		s.writeBackupError(w, &backupAPIError{Code: "unsupported_protection", Status: http.StatusBadRequest, Message: "backups do not use password protection"})
		return
	}
	if s.maintenance.active() {
		s.writeMaintenanceLocked(w)
		return
	}
	storage := s.currentStorage()
	status := agent.StorageStatus{}
	if storage != nil {
		status = storage.Status()
	}
	sdAvailable := status.Card.Present && status.Card.Mounted
	selected := backup.DefaultComponents(backup.ModeSameDevice, sdAvailable)
	definitions, err := backup.ValidateSelection(request.Mode, selected, sdAvailable)
	if err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "planning_failed", Status: http.StatusBadRequest, Message: sanitizeBackupError(err)})
		return
	}
	if containsComponent(selected, backup.ComponentDeviceIdentity) && s.readHardwareID() == "" {
		s.writeBackupError(w, &backupAPIError{Code: "source_device_unavailable", Status: http.StatusConflict, Message: "same-device identity backup requires an immutable hardware identifier"})
		return
	}
	createdAt := time.Now().UTC()
	material := backup.NewPlainMaterial(createdAt)
	id := uuid.NewString()
	job := &backupJob{
		id: id, createdAt: createdAt, state: backupJobReady, phase: "ready", mode: request.Mode,
		components: selected, material: material, done: make(chan struct{}),
		estimatedBytes: estimateBackupDefinitions(definitions, s.backupRoots()),
	}
	if err := s.backupJobs.put(job); err != nil {
		material.Destroy()
		s.writeBackupError(w, err)
		return
	}
	transfer, expiry, err := s.maintenanceSessions.issueJobToken(id, "backup")
	if err != nil {
		material.Destroy()
		s.backupJobs.remove(id)
		s.writeBackupError(w, err)
		return
	}
	job.mu.Lock()
	job.transferExpiry = expiry
	job.mu.Unlock()
	s.backupJobs.persist(job)
	time.AfterFunc(backupReadyTimeout, func() { s.backupJobs.expireReady(id) })
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": id, "state": job.state, "archive_url": "/api/backup/jobs/" + id + "/archive",
		"suggested_filename": backupFilename(createdAt), "estimated_bytes": job.estimatedBytes,
		"transfer_token": transfer, "transfer_expires_at": expiry,
	})
}

func backupFilename(at time.Time) string {
	return "aico-backup-" + at.Local().Format("20060102-150405") + backup.ArchiveSuffix
}

func (s *Server) handleBackupJob(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.backupJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "backup job not found")
		return
	}
	if r.Method == http.MethodDelete {
		if !s.maintenanceSessions.authorizeJob(r, id, "backup", true) {
			writeJSONError(w, http.StatusUnauthorized, "maintenance_session_required")
			return
		}
		s.cancelBackupJob(job, "cancelled", "backup cancelled")
		writeJSON(w, http.StatusAccepted, job.publicMap())
		return
	}
	if !s.maintenanceSessions.authorizeStatus(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "usb_required"})
		return
	}
	writeJSON(w, http.StatusOK, job.publicMap())
}

// cancelBackupJob cancels a running stream or drops a ready job's key.  A
// running stream finishes through handleBackupArchive's deferred cleanup.
func (s *Server) cancelBackupJob(job *backupJob, code, message string) {
	job.mu.Lock()
	cancel := job.cancel
	terminal := isTerminalBackupState(job.state)
	if !terminal {
		job.errorCode, job.errorMessage = code, message
		if cancel == nil {
			job.state, job.phase, job.finishedAt = backupJobCancelled, code, time.Now().UTC()
			if job.material != nil {
				job.material.Destroy()
				job.material = nil
			}
		}
	}
	job.mu.Unlock()
	if terminal {
		return
	}
	if cancel != nil {
		cancel()
		return
	}
	s.backupJobs.persist(job)
	s.maintenanceSessions.revokeJobToken(job.id)
}

func (s *Server) handleBackupArchive(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.backupJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "backup job not found")
		return
	}
	if !s.maintenanceSessions.authorizeJob(r, id, "backup", false) {
		writeJSONError(w, http.StatusUnauthorized, "maintenance_session_required")
		return
	}
	job.mu.Lock()
	if job.state != backupJobReady {
		payload := job.publicMapLocked()
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, payload)
		return
	}
	job.state, job.phase, job.startedAt = backupJobQuiescing, "acquiring_maintenance", time.Now().UTC()
	job.lastProgress = job.startedAt
	ctx, cancel := context.WithTimeout(r.Context(), backupTotalTimeout)
	job.cancel = cancel
	job.mu.Unlock()
	s.backupJobs.persist(job)
	defer cancel()
	lease, err := s.maintenance.begin("backup", id, true)
	if err != nil {
		s.finishBackupJob(job, backupJobFailed, "maintenance_in_progress", err.Error())
		s.writeBackupError(w, err)
		return
	}
	defer func() {
		lease.Release()
		s.maintenanceSessions.revokeJobToken(id)
		s.startDeferredRestartIfIdle()
	}()
	lease.Update("quiescing")
	var sdLease agent.StorageSnapshotLease
	if hasSDComponent(job.components) {
		storage := s.currentStorage()
		if storage == nil {
			s.finishBackupJob(job, backupJobFailed, "sd_missing", "storage manager unavailable")
			writeJSONError(w, http.StatusServiceUnavailable, "storage manager unavailable")
			return
		}
		sdLease, err = storage.AcquireSnapshotLease(ctx)
		if err != nil {
			s.finishBackupJob(job, backupJobFailed, "sd_missing", sanitizeBackupError(err))
			s.writeBackupError(w, &backupAPIError{Code: "sd_missing", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
			return
		}
		defer sdLease.Release()
	}
	services, err := s.quiesceUnits(ctx, backupQuiesceUnits(job.components), nil)
	if err != nil {
		s.finishBackupJob(job, backupJobFailed, "service_quiesce_failed", err.Error())
		s.writeBackupError(w, &backupAPIError{Code: "service_quiesce_failed", Status: http.StatusServiceUnavailable, Message: "unable to stop data-writing services"})
		return
	}
	streamSucceeded := false
	defer func() {
		job.mu.Lock()
		terminal := isTerminalBackupState(job.state)
		if !terminal {
			job.state, job.phase = backupJobResuming, "resuming_services"
		}
		job.mu.Unlock()
		if !terminal {
			s.backupJobs.persist(job)
		}
		resumeErr := s.resumeServices(context.Background(), services)
		if streamSucceeded && resumeErr == nil {
			s.finishBackupJob(job, backupJobCompleted, "", "")
		} else if resumeErr != nil && !terminal {
			s.finishBackupJob(job, backupJobFailed, "service_resume_failed", sanitizeBackupError(resumeErr))
		}
	}()
	storageIdentity := backup.StorageIdentity{}
	if storage := s.currentStorage(); storage != nil {
		status := storage.Status()
		storageIdentity.SDPresent = status.Card.Present && status.Card.Mounted
		storageIdentity.SDDevice = status.Card.Device
	}
	if sdLease != nil {
		if err := sdLease.Validate(ctx); err != nil {
			s.finishBackupJob(job, backupJobFailed, "sd_changed", sanitizeBackupError(err))
			writeJSONError(w, http.StatusConflict, "SD card changed before backup")
			return
		}
		snapshot := sdLease.Snapshot()
		storageIdentity.SDUUID = snapshot.FilesystemUUID
		storageIdentity.SDMountID = snapshot.MountID
	}
	// Plan only after the writers are stopped so the manifest describes a
	// consistent view of every component.
	replanned, err := backup.NewPlanner(s.backupRoots()).Plan(ctx, backup.PlanOptions{
		Mode: job.mode, Components: job.components,
		Source: backup.SourceIdentity{HardwareID: s.readHardwareID(), MachineID: s.readMachineID(),
			FirmwareVersion: agent.FirmwareVersion(), FirmwareBuild: agent.AgentBuildVersion(),
			ActiveSlot: s.activeSlot(), Architecture: runtime.GOARCH},
		Storage: storageIdentity, CreatedAt: job.createdAt, BackupID: job.id,
	})
	if err != nil {
		s.finishBackupJob(job, backupJobFailed, "planning_failed", sanitizeBackupError(err))
		writeJSONError(w, http.StatusConflict, "backup sources changed before streaming")
		return
	}
	job.mu.Lock()
	job.plan = replanned
	job.estimatedBytes = manifestExpandedSize(replanned.Manifest)
	job.mu.Unlock()
	if err := syncBackupRoots(s.options.BackupUserdataRoot, s.options.BackupSDRoot, hasSDComponent(job.components)); err != nil {
		s.finishBackupJob(job, backupJobFailed, "sync_failed", err.Error())
		writeJSONError(w, http.StatusServiceUnavailable, "could not synchronize persistent data")
		return
	}
	lease.Update("streaming")
	job.mu.Lock()
	job.state, job.phase = backupJobStreaming, "manifest"
	job.lastProgress = time.Now().UTC()
	job.mu.Unlock()
	s.backupJobs.persist(job)
	w.Header().Set("Content-Type", backup.ArchiveMIMEType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+backupFilename(job.createdAt)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if controller := http.NewResponseController(w); controller != nil {
		_ = controller.SetWriteDeadline(time.Time{})
	}
	stallStop := make(chan struct{})
	defer close(stallStop)
	go s.backupStallWatchdog(job, cancel, stallStop)
	var snapshotErr error
	lastSnapshotCheck := time.Now()
	lastPersist := time.Time{}
	err = backup.WriteArchive(ctx, w, job.plan, job.material, func(progress backup.Progress) {
		now := time.Now()
		job.mu.Lock()
		job.phase = string(progress.Component)
		job.filesProcessed = progress.FilesProcessed
		job.bytesRead = progress.BytesRead
		job.lastProgress = now.UTC()
		job.mu.Unlock()
		if now.Sub(lastPersist) >= time.Second {
			lastPersist = now
			s.backupJobs.persist(job)
		}
		if sdLease != nil && snapshotErr == nil && ctx.Err() == nil && now.Sub(lastSnapshotCheck) >= time.Second {
			lastSnapshotCheck = now
			if validateErr := sdLease.Validate(ctx); validateErr != nil {
				snapshotErr = validateErr
				cancel()
			}
		}
	})
	if err == nil && sdLease != nil {
		snapshotErr = sdLease.Validate(ctx)
	}
	if snapshotErr != nil {
		err = &backupAPIError{Code: "sd_changed", Status: http.StatusConflict, Message: sanitizeBackupError(snapshotErr)}
	}
	if err != nil {
		code := backup.ErrorCode(err)
		var apiErr *backupAPIError
		if errors.As(err, &apiErr) {
			code = apiErr.Code
		}
		job.mu.Lock()
		requested := job.errorCode
		job.mu.Unlock()
		state := backupJobFailed
		switch {
		case requested == "cancelled" || requested == "server_shutdown":
			code, state = requested, backupJobCancelled
		case requested != "":
			code = requested
		case code == "" && errors.Is(ctx.Err(), context.DeadlineExceeded):
			code = "stream_stalled"
		case code == "" && ctx.Err() != nil:
			code = "upload_interrupted"
		case code == "":
			code = "stream_failed"
		}
		s.finishBackupJob(job, state, code, sanitizeBackupError(err))
		return
	}
	streamSucceeded = true
}

// backupStallWatchdog cancels the stream when the HTTP response makes no
// progress for backupStallTimeout, which is what a stuck download manager or
// a half-open USB link looks like from the device.
func (s *Server) backupStallWatchdog(job *backupJob, cancel context.CancelFunc, stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		job.mu.Lock()
		stalled := time.Since(job.lastProgress) > backupStallTimeout
		if stalled && job.errorCode == "" {
			job.errorCode, job.errorMessage = "stream_stalled", "backup stream made no progress"
		}
		job.mu.Unlock()
		if stalled {
			cancel()
			return
		}
	}
}

func (s *Server) finishBackupJob(job *backupJob, state, code, message string) {
	job.mu.Lock()
	if !isTerminalBackupState(job.state) {
		job.state, job.phase, job.finishedAt = state, state, time.Now().UTC()
		// A cancel or stall recorded earlier wins over the generic stream
		// error the archive writer reports afterwards.
		if job.errorCode == "" || code == "" {
			job.errorCode, job.errorMessage = code, message
		}
	}
	job.cancel = nil
	if isTerminalBackupState(job.state) && job.material != nil {
		job.material.Destroy()
		job.material = nil
	}
	job.mu.Unlock()
	s.backupJobs.persist(job)
	select {
	case <-job.done:
	default:
		close(job.done)
	}
}

func isTerminalBackupState(state string) bool {
	return state == backupJobCompleted || state == backupJobCancelled || state == backupJobFailed
}

func hasSDComponent(ids []backup.ComponentID) bool {
	for _, id := range ids {
		if id == backup.ComponentSDManagedAudio || id == backup.ComponentSDUserFiles {
			return true
		}
	}
	return false
}

func syncBackupRoots(userdata, sd string, includeSD bool) error {
	for _, root := range []string{userdata, sd} {
		if root == sd && !includeSD {
			continue
		}
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = unix.Syncfs(fd)
		_ = unix.Close(fd)
		if err != nil {
			return err
		}
	}
	return nil
}

// Service orchestration --------------------------------------------------

type quiescedService struct {
	unit      string
	wasActive bool
}

// serviceStopOrder is the order writers are stopped; services are restarted
// in the reverse of their position in serviceStartOrder.
var serviceStartOrder = []string{
	"aico-environment.service",
	"bluetooth.service",
	"aico-ble.service",
	"wpa_supplicant@wlan0.service",
	"aico-wifi-proxy.service",
	"aico-audio.service",
	"aico-agent.service",
	"ssh.service",
	"aico-ttyd.service",
}

func backupQuiesceUnits(components []backup.ComponentID) []string {
	var units []string
	if containsComponent(components, backup.ComponentUserHome) {
		units = append(units, "aico-ttyd.service", "ssh.service")
	}
	if containsComponent(components, backup.ComponentDeviceIdentity) {
		units = append(units, "aico-ble.service", "bluetooth.service")
	}
	return append(units, "aico-audio.service", "aico-agent.service")
}

func restoreQuiesceUnits(components []backup.ComponentID) []string {
	var units []string
	if containsComponent(components, backup.ComponentUserHome) || containsComponent(components, backup.ComponentDeviceIdentity) {
		units = append(units, "aico-ttyd.service", "ssh.service")
	}
	if containsComponent(components, backup.ComponentNetwork) {
		units = append(units, "aico-wifi-proxy.service", "wpa_supplicant@wlan0.service")
	}
	if containsComponent(components, backup.ComponentDeviceIdentity) {
		units = append(units, "aico-ble.service", "bluetooth.service")
	}
	return append(units, "aico-audio.service", "aico-agent.service")
}

// quiesceUnits records each unit's active state and stops the active ones in
// order.  Units listed in already were stopped earlier by the same job and
// are skipped so their original state is not lost.
func (s *Server) quiesceUnits(ctx context.Context, units []string, already []quiescedService) ([]quiescedService, error) {
	if s.services == nil {
		return nil, nil
	}
	skip := make(map[string]bool, len(already))
	for _, service := range already {
		skip[service.unit] = true
	}
	result := make([]quiescedService, 0, len(units))
	for _, unit := range units {
		if skip[unit] {
			continue
		}
		active, err := s.services.Active(ctx, unit)
		if err != nil {
			_ = s.resumeServices(context.Background(), result)
			return nil, err
		}
		result = append(result, quiescedService{unit: unit, wasActive: active})
		if active {
			if err := s.services.Stop(ctx, unit); err != nil {
				_ = s.resumeServices(context.Background(), result)
				return nil, err
			}
		}
	}
	return result, nil
}

// resumeServices restarts only the services that were active before
// maintenance, in dependency order.
func (s *Server) resumeServices(ctx context.Context, services []quiescedService) error {
	if s.services == nil {
		return nil
	}
	var first error
	byUnit := make(map[string]bool, len(services))
	for _, service := range services {
		if service.wasActive {
			byUnit[service.unit] = true
		}
	}
	for _, unit := range serviceStartOrder {
		if byUnit[unit] {
			if err := s.services.Start(ctx, unit); err != nil && first == nil {
				first = err
			}
			delete(byUnit, unit)
		}
	}
	for unit := range byUnit {
		if err := s.services.Start(ctx, unit); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// resumeRestoredServices additionally regenerates the strict service
// environment when its source was restored, before the Agent starts.
func (s *Server) resumeRestoredServices(ctx context.Context, services []quiescedService, components []backup.ComponentID) error {
	if s.services != nil && (containsComponent(components, backup.ComponentSystemEnvironment) || containsComponent(components, backup.ComponentAgentConfig)) {
		services = append([]quiescedService{{unit: "aico-environment.service", wasActive: true}}, services...)
	}
	return s.resumeServices(ctx, services)
}

func containsComponent(ids []backup.ComponentID, id backup.ComponentID) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func manifestExpandedSize(manifest backup.Manifest) int64 {
	var total int64
	for _, component := range manifest.Components {
		total += component.ExpandedSize
	}
	return total
}

func estimateBackupDefinitions(definitions []backup.ComponentDefinition, roots backup.Roots) int64 {
	var total int64
	for _, definition := range definitions {
		for _, source := range definition.Sources(roots) {
			info, err := os.Lstat(source.Path)
			if err != nil {
				continue
			}
			if info.Mode().IsRegular() {
				total += info.Size()
				continue
			}
			if !info.IsDir() {
				continue
			}
			_ = filepath.WalkDir(source.Path, func(_ string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil || entry == nil || entry.Type()&os.ModeSymlink != 0 {
					return nil
				}
				if entry.Type().IsRegular() {
					if item, err := entry.Info(); err == nil {
						total += item.Size()
					}
				}
				return nil
			})
		}
	}
	return total
}

func (s *Server) readHardwareID() string {
	data, err := os.ReadFile(s.options.HardwareIDPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(data), "\x00"))
}

func (s *Server) readMachineID() string {
	data, err := os.ReadFile(filepath.Join(s.options.BackupUserdataRoot, "system/machine-id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (s *Server) activeSlot() string {
	data, err := os.ReadFile(s.options.CmdlinePath)
	if err != nil {
		return ""
	}
	return currentSlot(string(data))
}

// sanitizeBackupError bounds error text; callers must never pass passphrases,
// tokens or file contents into error messages.
func sanitizeBackupError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 256 {
		value = value[:256]
	}
	return value
}

func zeroString(value *string) {
	if value == nil {
		return
	}
	bytes := []byte(*value)
	for i := range bytes {
		bytes[i] = 0
	}
	*value = ""
}

func (s *Server) writeBackupError(w http.ResponseWriter, err error) {
	var busy *maintenanceBusyError
	if errors.As(err, &busy) {
		payload := map[string]any{"ok": false, "error": "maintenance_in_progress", "message": busy.Error(), "retryable": true}
		if busy.Snapshot.JobID != "" {
			payload["operation"], payload["job_id"], payload["phase"] = busy.Snapshot.Operation, busy.Snapshot.JobID, busy.Snapshot.Phase
		}
		writeJSON(w, http.StatusLocked, payload)
		return
	}
	var apiErr *backupAPIError
	if errors.As(err, &apiErr) {
		details := map[string]any{"ok": false, "error": apiErr.Code, "message": apiErr.Message, "retryable": apiErr.Retryable}
		for key, value := range apiErr.Details {
			details[key] = value
		}
		writeJSON(w, apiErr.Status, details)
		return
	}
	if code := backup.ErrorCode(err); code != "" {
		status := http.StatusBadRequest
		switch code {
		case "wrong_passphrase":
			status = http.StatusUnauthorized
		case "insufficient_space", "sd_missing", "sd_changed", "commit_failed":
			status = http.StatusConflict
		case "rollback_failed":
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": code, "message": sanitizeBackupError(err), "retryable": false})
		return
	}
	writeJSONError(w, http.StatusInternalServerError, sanitizeBackupError(err))
}

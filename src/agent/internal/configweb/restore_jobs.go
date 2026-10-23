package configweb

// The restore implementation intentionally keeps the encrypted archive out of
// device storage.  A one-slot input queue feeds backup.ArchiveStream, which
// lets the parser stop after manifest.json and resume only after the client has
// submitted an explicit storage/component plan.
//
// Commit, rollback and boot-time recovery share backup.Transaction so a power
// loss at any point is resumed by the same code that produced the log.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/backup"
	"golang.org/x/sys/unix"
)

const (
	restoreJobCreated         = "created"
	restoreJobIngest          = "restore_ingest"
	restoreJobReadingManifest = "reading_manifest"
	restoreJobAwaitingPlan    = "awaiting_plan"
	restoreJobUploading       = "uploading_and_staging"
	restoreJobValidating      = "validating"
	restoreJobPrepared        = "prepared"
	restoreJobQuiescing       = "quiescing"
	restoreJobCommitting      = "committing"
	restoreJobPostProcessing  = "post_processing"
	restoreJobResuming        = "resuming_services"
	restoreJobRollingBack     = "rolling_back"
	restoreJobCompleted       = "completed"
	restoreJobRebootRequired  = "reboot_required"
	restoreJobCancelled       = "cancelled"
	restoreJobFailed          = "failed"
	restoreJobRollbackFailed  = "rollback_failed"

	maxRestoreArchiveSize int64 = 16 << 30
	restoreSafetyBytes    int64 = 64 << 20

	// restoreStallTimeout bounds the gap between two upload blocks while the
	// device is waiting for data; restoreIdleTimeout bounds how long a job may
	// sit in a client-driven state (awaiting_plan, validating, prepared) and
	// restoreTotalTimeout caps the whole job.  All three release maintenance
	// mode and restart the services stopped for ingest.
	restoreStallTimeout = 120 * time.Second
	restoreIdleTimeout  = 30 * time.Minute
	restoreTotalTimeout = 4 * time.Hour
	restoreRebootDelay  = 3 * time.Second
)

type restoreCreateRequest struct {
	FormatVersion  int                  `json:"format_version"`
	ArchiveSize    int64                `json:"archive_size"`
	ArchiveSHA256  string               `json:"archive_sha256"`
	PublicHeader   backup.PublicHeader  `json:"public_header"`
	ConflictPolicy string               `json:"conflict_policy"`
	Protection     restoreProtectionReq `json:"protection"`
}

type restoreProtectionReq struct {
	Mode       string `json:"mode"`
	Passphrase string `json:"passphrase"`
}

type restorePlanRequest struct {
	Components       []string `json:"components"`
	SDStrategy       string   `json:"sd_strategy"`
	ConflictPolicy   string   `json:"conflict_policy"`
	ConfirmConflicts bool     `json:"confirm_conflicts"`
	ConfirmIdentity  bool     `json:"confirm_identity"`
}

type restoreValidateRequest struct {
	ArchiveSHA256 string `json:"archive_sha256"`
}

type restoreApplyRequest struct {
	PlanDigest string `json:"plan_digest"`
	Confirm    string `json:"confirm"`
}

type restoreJobStore struct {
	server *Server
	root   string
	mu     sync.Mutex
	jobs   map[string]*restoreJob
}

type restoreJob struct {
	mu sync.Mutex

	id             string
	createdAt      time.Time
	startedAt      time.Time
	finishedAt     time.Time
	lastActivity   time.Time
	state          string
	phase          string
	archiveSize    int64
	expectedHash   string
	archiveHash    hash.Hash
	receivedBytes  int64
	stagedBytes    int64
	filesProcessed int
	warnings       []string

	header   backup.PublicHeader
	material *backup.KeyMaterial

	input              *restoreChunkReader
	parserCtx          context.Context
	parserCancel       context.CancelFunc
	parserErr          error
	manifest           backup.Manifest
	manifestOK         bool
	stream             *backup.ArchiveStream
	manifestReady      chan struct{}
	planReady          chan struct{}
	inputDone          chan struct{}
	planSet            bool
	inputClosed        bool
	receivedChunkIndex int64

	selected   map[backup.ComponentID]bool
	sdStrategy string
	planDigest string
	units      map[string]*backup.TransactionUnit

	identityProvisioned    bool
	identityRebootRequired bool
	transactionState       string

	maintenance    *maintenanceLease
	sdLease        agent.StorageSnapshotLease
	services       []quiescedService
	cancel         context.CancelFunc
	done           chan struct{}
	transferExpiry time.Time
}

// restoreChunkReader is the one-slot queue between HTTP upload handlers and
// the parser goroutine.  Each queued block carries a channel that is closed
// once the parser has consumed every byte of it, which is what the chunk
// response waits for so the client never runs ahead of staging.
type restoreChunkReader struct {
	chunks   chan *restoreChunk
	closedCh chan struct{}
	mu       sync.Mutex
	closed   bool
	err      error
	current  *restoreChunk
}

type restoreChunk struct {
	data     []byte
	consumed chan struct{}
	off      int
}

func newRestoreChunkReader() *restoreChunkReader {
	return &restoreChunkReader{chunks: make(chan *restoreChunk, 1), closedCh: make(chan struct{})}
}

// Write queues data and returns a channel that is closed when the parser has
// consumed the whole block.  The caller owns data and must not reuse it.
func (r *restoreChunkReader) Write(ctx context.Context, data []byte) (<-chan struct{}, error) {
	item := &restoreChunk{data: data, consumed: make(chan struct{})}
	if len(data) == 0 {
		close(item.consumed)
		return item.consumed, nil
	}
	r.mu.Lock()
	if r.closed {
		err := r.err
		r.mu.Unlock()
		if err == nil {
			return nil, io.EOF
		}
		return nil, err
	}
	r.mu.Unlock()
	select {
	case r.chunks <- item:
		return item.consumed, nil
	case <-r.closedCh:
		r.mu.Lock()
		err := r.err
		r.mu.Unlock()
		if err == nil {
			return nil, io.EOF
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *restoreChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		r.mu.Lock()
		current := r.current
		r.mu.Unlock()
		if current != nil {
			count := copy(p, current.data[current.off:])
			current.off += count
			if current.off == len(current.data) {
				r.mu.Lock()
				r.current = nil
				r.mu.Unlock()
				close(current.consumed)
			}
			return count, nil
		}
		var item *restoreChunk
		// Prefer already queued data over the close notification.  This matters
		// for the final upload block: CloseWithError may run immediately after
		// the block was queued.
		select {
		case item = <-r.chunks:
		default:
			select {
			case item = <-r.chunks:
			case <-r.closedCh:
				select {
				case item = <-r.chunks:
				default:
				}
				if item == nil {
					r.mu.Lock()
					err := r.err
					r.mu.Unlock()
					if err == nil {
						return 0, io.EOF
					}
					return 0, err
				}
			}
		}
		r.mu.Lock()
		r.current = item
		r.mu.Unlock()
	}
}

func (r *restoreChunkReader) CloseWithError(err error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed, r.err = true, err
	close(r.closedCh)
	r.mu.Unlock()
}

func newRestoreJobStore(server *Server, root string) *restoreJobStore {
	return &restoreJobStore{server: server, root: root, jobs: make(map[string]*restoreJob)}
}

func (s *restoreJobStore) get(id string) (*restoreJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	return job, ok
}

func (s *restoreJobStore) put(job *restoreJob) error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	s.jobs[job.id] = job
	s.mu.Unlock()
	return nil
}

func (s *restoreJobStore) cancelAll() {
	s.mu.Lock()
	jobs := make([]*restoreJob, 0, len(s.jobs))
	for _, job := range s.jobs {
		jobs = append(jobs, job)
	}
	s.mu.Unlock()
	for _, job := range jobs {
		s.server.abortRestore(job, "server_shutdown", errors.New("restore interrupted by service shutdown"))
	}
}

func (s *restoreJobStore) persist(job *restoreJob) {
	if job == nil || s.root == "" {
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

func (j *restoreJob) publicMap() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.publicMapLocked()
}

func (j *restoreJob) publicMapLocked() map[string]any {
	result := map[string]any{
		"job_id": j.id, "operation": "restore", "state": j.state, "phase": j.phase,
		"archive_size": j.archiveSize, "received_bytes": j.receivedBytes,
		"staged_bytes": j.stagedBytes, "files_processed": j.filesProcessed,
		"created_at": j.createdAt, "started_at": j.startedAt, "finished_at": j.finishedAt,
		"plan_digest": j.planDigest, "cancelable": !isTerminalRestoreState(j.state) && !restoreCommitInProgress(j.state),
		"reboot_required": j.identityRebootRequired,
	}
	if len(j.warnings) > 0 {
		result["warnings"] = append([]string(nil), j.warnings...)
	}
	if j.manifestOK {
		result["manifest"] = map[string]any{
			"backup_id": j.manifest.BackupID, "created_at": j.manifest.CreatedAt,
			"mode": j.manifest.Mode, "source": j.manifest.Source,
			"storage": j.manifest.Storage, "components": j.manifest.Components,
			"conflicts": manifestConflictCount(j.manifest),
		}
	}
	if j.parserErr != nil {
		code := backup.ErrorCode(j.parserErr)
		if code == "" {
			code = "restore_failed"
		}
		result["error"] = map[string]any{"code": code, "message": sanitizeBackupError(j.parserErr)}
	} else {
		result["error"] = nil
	}
	return result
}

func manifestConflictCount(manifest backup.Manifest) int {
	count := 0
	for _, file := range manifest.Files {
		if file.Conflict {
			count++
		}
	}
	return count
}

func (s *Server) handleCreateRestoreJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireSession(w, r, true) {
		return
	}
	var request restoreCreateRequest
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
	if request.ArchiveSize <= 0 || request.ArchiveSize > maxRestoreArchiveSize {
		s.writeBackupError(w, &backupAPIError{Code: "archive_size_invalid", Status: http.StatusBadRequest, Message: "archive_size is outside the supported range"})
		return
	}
	if request.PublicHeader.Protection.Algorithm != "sha256-chunked" && (request.Protection.Mode != "passphrase" || len(request.Protection.Passphrase) < 8 || len(request.Protection.Passphrase) > maxBackupPassphrase) {
		s.writeBackupError(w, &backupAPIError{Code: "wrong_passphrase", Status: http.StatusBadRequest, Message: "a passphrase of 8 to 4096 bytes is required"})
		return
	}
	if request.PublicHeader.Protection.Algorithm == "sha256-chunked" && ((request.Protection.Mode != "" && request.Protection.Mode != "none") || request.Protection.Passphrase != "") {
		s.writeBackupError(w, &backupAPIError{Code: "unsupported_protection", Status: http.StatusBadRequest, Message: "this archive does not use a passphrase"})
		return
	}
	// Validate the untrusted public header (including KDF limits) before any
	// Argon2 memory is allocated.
	if err := request.PublicHeader.Validate(); err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "unsupported_format", Status: http.StatusBadRequest, Message: sanitizeBackupError(err)})
		return
	}
	expectedHash := strings.ToLower(strings.TrimSpace(request.ArchiveSHA256))
	if expectedHash != "" && !isHexDigest(expectedHash) {
		s.writeBackupError(w, &backupAPIError{Code: "archive_hash_invalid", Status: http.StatusBadRequest, Message: "archive_sha256 must be a SHA-256 digest"})
		return
	}
	if s.maintenance.active() {
		s.writeMaintenanceLocked(w)
		return
	}
	// The archive may mostly contain SD data and is never stored on userdata.
	// Reserve only safety space here; manifest planning checks each target.
	if err := checkFreeSpace(s.options.BackupUserdataRoot, 0); err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "insufficient_space", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	material, err := backup.DeriveKeyMaterial(request.PublicHeader, []byte(request.Protection.Passphrase))
	zeroString(&request.Protection.Passphrase)
	if err != nil {
		s.writeBackupError(w, err)
		return
	}
	id := newRestoreID()
	now := time.Now().UTC()
	job := &restoreJob{
		id: id, createdAt: now, lastActivity: now, state: restoreJobCreated, phase: "created",
		archiveSize: request.ArchiveSize, expectedHash: expectedHash,
		header: request.PublicHeader, material: material, done: make(chan struct{}),
		manifestReady: make(chan struct{}), planReady: make(chan struct{}), inputDone: make(chan struct{}),
		units: make(map[string]*backup.TransactionUnit), archiveHash: sha256.New(),
	}
	if err := s.restoreJobs.put(job); err != nil {
		material.Destroy()
		s.writeBackupError(w, err)
		return
	}
	transfer, expiry, err := s.maintenanceSessions.issueJobToken(id, "restore")
	if err != nil {
		material.Destroy()
		s.writeBackupError(w, err)
		return
	}
	job.mu.Lock()
	job.transferExpiry = expiry
	job.mu.Unlock()
	s.restoreJobs.persist(job)
	// A job that never receives its first chunk must not keep the derived key
	// alive indefinitely.
	time.AfterFunc(restoreIdleTimeout, func() {
		job.mu.Lock()
		stale := job.state == restoreJobCreated
		job.mu.Unlock()
		if stale {
			s.abortRestore(job, "job_expired", errors.New("restore upload was not started"))
		}
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id": id, "state": restoreJobCreated, "chunk_size": backup.UploadChunkSize(request.PublicHeader.Protection.ChunkSize),
		"transfer_token": transfer, "transfer_expires_at": expiry,
	})
}

func newRestoreID() string {
	suffix, err := randomToken()
	if err != nil {
		suffix = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("r%d-%s", time.Now().UnixNano(), suffix[:16])
}

func isHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s *Server) handleRestoreJob(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.restoreJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "restore job not found")
		return
	}
	if r.Method == http.MethodDelete {
		if !s.authorizeRestore(w, r, id, true, false) {
			return
		}
		job.mu.Lock()
		state := job.state
		job.mu.Unlock()
		if restoreCommitInProgress(state) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_committing", "state": state, "message": "restore is already committing"})
			return
		}
		s.abortRestore(job, "cancelled", errors.New("restore cancelled"))
		writeJSON(w, http.StatusAccepted, job.publicMap())
		return
	}
	// Status is read-only and carries no secrets: any USB same-origin client
	// (a reloaded page while a CLI holds the session) may poll it.
	if !s.maintenanceSessions.authorizeStatus(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "usb_required"})
		return
	}
	writeJSON(w, http.StatusOK, job.publicMap())
}

func (s *Server) authorizeRestore(w http.ResponseWriter, r *http.Request, id string, mutate, transfer bool) bool {
	if transfer && s.maintenanceSessions.authorizeJob(r, id, "restore", false) {
		return true
	}
	if s.maintenanceSessions == nil || !s.maintenanceSessions.authorize(r, mutate) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "maintenance_session_required"})
		return false
	}
	return true
}

func (s *Server) handleRestoreChunk(w http.ResponseWriter, r *http.Request, suffix string) {
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 {
		writeJSONError(w, http.StatusBadRequest, "invalid restore chunk path")
		return
	}
	index, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || index < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid restore chunk index")
		return
	}
	id := parts[0]
	job, ok := s.restoreJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "restore job not found")
		return
	}
	if !s.authorizeRestore(w, r, id, true, true) {
		return
	}
	job.mu.Lock()
	if job.state == restoreJobCreated {
		if err := s.beginRestoreIngestLocked(job); err != nil {
			job.mu.Unlock()
			s.writeBackupError(w, err)
			return
		}
	}
	switch job.state {
	case restoreJobIngest, restoreJobReadingManifest, restoreJobUploading:
	case restoreJobAwaitingPlan:
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_plan_required", "state": restoreJobAwaitingPlan})
		return
	default:
		state := job.state
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_not_receiving", "state": state})
		return
	}
	if index != job.receivedChunkIndex {
		want := job.receivedChunkIndex
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "chunk_out_of_order", "expected_index": want})
		return
	}
	chunkSize := backup.UploadChunkSize(job.header.Protection.ChunkSize)
	if r.ContentLength < 0 || r.ContentLength > int64(chunkSize)+1 {
		job.mu.Unlock()
		writeJSONError(w, http.StatusRequestEntityTooLarge, "restore chunk is too large")
		return
	}
	remaining := job.archiveSize - job.receivedBytes
	job.mu.Unlock()

	// Chunk requests are long-lived: reading a full block over a slow link and
	// waiting for the parser to consume it can both exceed the server-wide
	// 65 s timeouts.  Bound the read by the job's stall limit and let the job
	// watchdog (stall/idle/total) govern the response instead of a fixed
	// write deadline, as the backup archive stream already does.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(restoreStallTimeout))
	_ = controller.SetWriteDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, int64(chunkSize)+1)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "restore chunk could not be read")
		return
	}
	if len(data) == 0 || len(data) > chunkSize {
		writeJSONError(w, http.StatusBadRequest, "restore chunk has an invalid size")
		return
	}
	if int64(len(data)) > remaining {
		writeJSONError(w, http.StatusBadRequest, "restore upload exceeds archive_size")
		return
	}
	wantHash := strings.TrimSpace(strings.ToLower(r.Header.Get("X-Aico-Chunk-SHA256")))
	digest := sha256.Sum256(data)
	if wantHash == "" || wantHash != hex.EncodeToString(digest[:]) {
		writeJSONError(w, http.StatusBadRequest, "restore chunk hash mismatch")
		return
	}
	if err := s.validateRestoreSDLease(job, r.Context()); err != nil {
		s.failRestore(job, "sd_changed", err)
		s.writeBackupError(w, &backupAPIError{Code: "sd_changed", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	if err := s.checkRestoreSpaceReserve(job); err != nil {
		s.failRestore(job, "insufficient_space", err)
		s.writeBackupError(w, &backupAPIError{Code: "insufficient_space", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	job.mu.Lock()
	input := job.input
	if job.archiveHash != nil {
		_, _ = job.archiveHash.Write(data)
	}
	job.mu.Unlock()
	if input == nil {
		writeJSONError(w, http.StatusConflict, "restore input is unavailable")
		return
	}
	consumed, err := input.Write(r.Context(), data)
	if err != nil {
		s.failRestore(job, "ingest_failed", err)
		s.writeBackupError(w, err)
		return
	}
	job.mu.Lock()
	job.receivedBytes += int64(len(data))
	job.receivedChunkIndex++
	job.lastActivity = time.Now().UTC()
	planSet := job.planSet
	final := job.receivedBytes == job.archiveSize
	if final {
		job.inputClosed = true
		input.CloseWithError(nil)
	}
	job.mu.Unlock()

	// Do not answer until the device has made progress on this block: either
	// the parser consumed it (send the next one), paused at manifest.json
	// (submit a plan), or finished staging the last block (call validate).
	var manifestWait <-chan struct{}
	if !planSet {
		manifestWait = job.manifestReady
	}
	wait := consumed
	if final && planSet {
		wait = job.inputDone
	}
	select {
	case <-wait:
	case <-manifestWait:
	case <-job.done:
	case <-r.Context().Done():
		return
	}
	job.mu.Lock()
	parserErr, terminal := job.parserErr, isTerminalRestoreState(job.state)
	job.mu.Unlock()
	if terminal && parserErr != nil {
		// Typically a wrong passphrase on the very first block: fail early with
		// the stable error code instead of a 202 that hides it in the payload.
		s.writeBackupError(w, parserErr)
		return
	}
	s.restoreJobs.persist(job)
	writeJSON(w, http.StatusAccepted, job.publicMap())
}

// checkRestoreSpaceReserve aborts ingest before staging can push userdata or
// the SD card below the reserve; cleaners must never make room for a restore.
func (s *Server) checkRestoreSpaceReserve(job *restoreJob) error {
	job.mu.Lock()
	units := job.units
	job.mu.Unlock()
	roots := map[string]string{s.options.BackupUserdataRoot: backup.LayerUserdata}
	for _, unit := range units {
		if unit.Layer == backup.LayerSD {
			roots[s.options.BackupSDRoot] = backup.LayerSD
		}
	}
	for root := range roots {
		if err := checkFreeSpace(root, 0); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) beginRestoreIngestLocked(job *restoreJob) error {
	lease, err := s.maintenance.begin("restore", job.id, true)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	job.maintenance, job.cancel, job.parserCtx, job.parserCancel = lease, cancel, ctx, cancel
	job.state, job.phase, job.startedAt = restoreJobIngest, "ingest", time.Now().UTC()
	job.lastActivity = job.startedAt
	job.input = newRestoreChunkReader()
	lease.Update("restore_ingest")
	// The Agent owns the storage monitor and its emergency cleaners.  Without a
	// storage-protection lease the only verifiable way to keep cleaners away
	// from the staging tree is to stop the Agent for the whole ingest.
	stopped, err := s.quiesceUnits(ctx, ingestQuiesceUnits(), nil)
	if err != nil {
		lease.Release()
		cancel()
		job.maintenance, job.cancel, job.parserCtx, job.parserCancel, job.input = nil, nil, nil, nil, nil
		job.state, job.phase = restoreJobCreated, "created"
		return &backupAPIError{Code: "service_quiesce_failed", Status: http.StatusServiceUnavailable, Message: "unable to stop the Agent before restore ingest"}
	}
	job.services = append(job.services, stopped...)
	go s.restoreParseLoop(job)
	go s.restoreWatchdog(job)
	return nil
}

func ingestQuiesceUnits() []string {
	return []string{"aico-agent.service"}
}

// restoreWatchdog enforces stall, idle and total time limits for one job.
func (s *Server) restoreWatchdog(job *restoreJob) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-job.done:
			return
		case <-ticker.C:
		}
		job.mu.Lock()
		state := job.state
		idle := time.Since(job.lastActivity)
		total := time.Since(job.startedAt)
		job.mu.Unlock()
		var reason string
		switch state {
		case restoreJobIngest, restoreJobReadingManifest, restoreJobUploading:
			if idle > restoreStallTimeout {
				reason = "restore upload stalled"
			}
		case restoreJobAwaitingPlan, restoreJobValidating, restoreJobPrepared:
			if idle > restoreIdleTimeout {
				reason = "restore was not continued by the client"
			}
		}
		if reason == "" && total > restoreTotalTimeout && !isTerminalRestoreState(state) && !restoreCommitInProgress(state) {
			reason = "restore exceeded the maximum job duration"
		}
		if reason != "" {
			s.abortRestore(job, "stream_stalled", errors.New(reason))
			return
		}
	}
}

func (s *Server) restoreParseLoop(job *restoreJob) {
	job.mu.Lock()
	input, material, ctx := job.input, job.material, job.parserCtx
	job.state, job.phase = restoreJobReadingManifest, "reading_manifest"
	job.mu.Unlock()
	stream, err := backup.NewArchiveStream(input, material)
	if err != nil {
		s.failRestore(job, backup.ErrorCode(err), err)
		return
	}
	manifest, err := stream.ReadManifest()
	if err != nil {
		s.failRestore(job, backup.ErrorCode(err), err)
		return
	}
	job.mu.Lock()
	job.stream, job.manifest, job.manifestOK = stream, manifest, true
	job.state, job.phase = restoreJobAwaitingPlan, restoreJobAwaitingPlan
	job.lastActivity = time.Now().UTC()
	closeOnce(job.manifestReady)
	job.mu.Unlock()
	s.restoreJobs.persist(job)

	select {
	case <-job.planReady:
	case <-ctx.Done():
		s.failRestore(job, "cancelled", ctx.Err())
		return
	}
	job.mu.Lock()
	if isTerminalRestoreState(job.state) {
		job.mu.Unlock()
		return
	}
	job.state, job.phase = restoreJobUploading, "staging"
	job.mu.Unlock()
	s.restoreJobs.persist(job)

	for {
		if err := s.validateRestoreSDLease(job, ctx); err != nil {
			s.failRestore(job, "sd_changed", err)
			return
		}
		entry, nextErr := stream.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			s.failRestore(job, backup.ErrorCode(nextErr), nextErr)
			return
		}
		job.mu.Lock()
		selected := job.selected[entry.Manifest.Component]
		job.mu.Unlock()
		if !selected {
			// Unselected entries are still authenticated and consumed, but never
			// written to staging.
			if err := stream.CopyCurrentEntry(ctx, io.Discard); err != nil {
				s.failRestore(job, backup.ErrorCode(err), err)
				return
			}
			continue
		}
		if err := s.stageRestoreEntry(job, stream, entry); err != nil {
			code := backup.ErrorCode(err)
			if code == "" {
				code = "staging_failed"
			}
			s.failRestore(job, code, err)
			return
		}
		job.mu.Lock()
		job.filesProcessed++
		job.phase = string(entry.Manifest.Component)
		job.mu.Unlock()
	}
	if err := stream.Finish(); err != nil {
		s.failRestore(job, backup.ErrorCode(err), err)
		return
	}
	if err := s.validateRestoreStaging(job); err != nil {
		code := backup.ErrorCode(err)
		if code == "" {
			code = "validation_failed"
		}
		s.failRestore(job, code, err)
		return
	}
	job.mu.Lock()
	job.state, job.phase = restoreJobValidating, "awaiting_validate"
	job.lastActivity = time.Now().UTC()
	closeOnce(job.inputDone)
	job.mu.Unlock()
	s.restoreJobs.persist(job)
}

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (s *Server) handleRestorePlan(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.restoreJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "restore job not found")
		return
	}
	if !s.authorizeRestore(w, r, id, true, false) {
		return
	}
	var request restorePlanRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	job.mu.Lock()
	if job.parserErr != nil {
		err := job.parserErr
		job.mu.Unlock()
		s.writeBackupError(w, err)
		return
	}
	if !job.manifestOK || job.state != restoreJobAwaitingPlan || job.planSet {
		state := job.state
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_plan_unavailable", "state": state})
		return
	}
	manifest := job.manifest
	job.lastActivity = time.Now().UTC()
	job.mu.Unlock()

	selected, strategy, err := validateRestorePlan(s, manifest, request)
	if err != nil {
		s.writeBackupError(w, err)
		return
	}
	var sdLease agent.StorageSnapshotLease
	leaseAttached := false
	if restoreNeedsSDLease(selected, strategy) {
		sdLease, err = s.acquireRestoreSDLease(r.Context(), manifest, strategy)
		if err != nil {
			s.writeBackupError(w, err)
			return
		}
		defer func() {
			if !leaseAttached {
				sdLease.Release()
			}
		}()
	}
	// Build the restore plan while the SD mount identity is pinned.
	units, err := s.buildRestoreUnits(job.id, manifest, selected, strategy)
	if err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "planning_failed", Status: http.StatusBadRequest, Message: sanitizeBackupError(err)})
		return
	}
	if err := preflightRestoreSpace(s, manifest, selected, strategy); err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "insufficient_space", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	componentList := make([]backup.ComponentID, 0, len(selected))
	for id := range selected {
		componentList = append(componentList, id)
	}
	sort.Slice(componentList, func(i, j int) bool { return componentList[i] < componentList[j] })
	digestInput := restorePlanDigestInput{BackupID: manifest.BackupID, Components: componentList, SDStrategy: strategy, Units: units}
	digestData, _ := json.Marshal(digestInput)
	digest := sha256.Sum256(digestData)
	warnings := restorePlanWarnings(manifest, selected, strategy)
	job.mu.Lock()
	// Planning runs without the job lock. Cancellation or another accepted
	// plan must not be overwritten when this request reacquires it.
	if job.state != restoreJobAwaitingPlan || job.planSet {
		state := job.state
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_plan_unavailable", "state": state})
		return
	}
	job.selected, job.sdStrategy, job.units, job.planDigest = selected, strategy, units, hex.EncodeToString(digest[:])
	job.sdLease = sdLease
	job.warnings = warnings
	job.planSet = true
	// The next chunk can arrive before the parser wakes on planReady. Publish
	// the receiving state with the accepted plan before returning success.
	job.state, job.phase = restoreJobUploading, "staging"
	closeOnce(job.planReady)
	inputClosed := job.inputClosed
	job.mu.Unlock()
	if sdLease != nil {
		leaseAttached = true
	}
	if inputClosed {
		// The whole archive already arrived: staging finishes without further
		// uploads, so report the validating state directly.  Staging the
		// remaining frames is bounded by the job watchdog, not the server
		// write timeout.
		if controller := http.NewResponseController(w); controller != nil {
			_ = controller.SetWriteDeadline(time.Time{})
		}
		select {
		case <-job.inputDone:
		case <-job.done:
		case <-r.Context().Done():
			return
		}
	}
	s.restoreJobs.persist(job)
	payload := job.publicMap()
	payload["ok"] = true
	payload["components"] = componentList
	payload["sd_strategy"] = strategy
	writeJSON(w, http.StatusOK, payload)
}

type restorePlanDigestInput struct {
	BackupID   string                             `json:"backup_id"`
	Components []backup.ComponentID               `json:"components"`
	SDStrategy string                             `json:"sd_strategy"`
	Units      map[string]*backup.TransactionUnit `json:"units"`
}

func restorePlanWarnings(manifest backup.Manifest, selected map[backup.ComponentID]bool, strategy string) []string {
	var warnings []string
	if selected[backup.ComponentSDManagedAudio] && strategy == "emmc_fallback" {
		warnings = append(warnings, "SD audio will be restored to userdata; retention limits may evict media on first start")
	}
	if !selected[backup.ComponentAgentConfig] {
		for _, id := range []backup.ComponentID{backup.ComponentAudioArchive, backup.ComponentSDManagedAudio} {
			if selected[id] {
				warnings = append(warnings, "media is restored without agent_config; the current retention policy applies on first start")
				break
			}
		}
	}
	if count := manifestConflictCount(manifest); count > 0 {
		warnings = append(warnings, fmt.Sprintf("%d audio files differ between userdata and SD copies; both copies are restored", count))
	}
	return warnings
}

func validateRestorePlan(s *Server, manifest backup.Manifest, request restorePlanRequest) (map[backup.ComponentID]bool, string, error) {
	known := make(map[backup.ComponentID]bool)
	for _, component := range manifest.Components {
		known[component.ID] = true
	}
	selected := make(map[backup.ComponentID]bool)
	if len(request.Components) == 0 {
		for id := range known {
			selected[id] = true
		}
	} else {
		for _, value := range request.Components {
			id := backup.ComponentID(strings.TrimSpace(value))
			if !known[id] {
				return nil, "", &backupAPIError{Code: "component_not_in_archive", Status: http.StatusBadRequest, Message: "component is not present in the archive"}
			}
			selected[id] = true
		}
	}
	if len(selected) == 0 {
		return nil, "", &backupAPIError{Code: "planning_failed", Status: http.StatusBadRequest, Message: "at least one component must be selected"}
	}
	strategy := strings.TrimSpace(request.SDStrategy)
	if strategy == "" {
		strategy = "require_match"
	}
	if strategy != "require_match" && strategy != "allow_different" && strategy != "emmc_fallback" && strategy != "skip" {
		return nil, "", &backupAPIError{Code: "sd_strategy_invalid", Status: http.StatusBadRequest, Message: "unsupported SD restore strategy"}
	}
	status := agent.StorageStatus{}
	if storage := s.currentStorage(); storage != nil {
		status = storage.Status()
	}
	sdAvailable := status.Card.Present && status.Card.Mounted
	for _, id := range []backup.ComponentID{backup.ComponentSDManagedAudio, backup.ComponentSDUserFiles} {
		if !selected[id] {
			continue
		}
		if strategy == "skip" {
			delete(selected, id)
			continue
		}
		if strategy == "emmc_fallback" {
			if id == backup.ComponentSDUserFiles {
				return nil, "", &backupAPIError{Code: "sd_fallback_unsupported", Status: http.StatusBadRequest, Message: "SD user files cannot be redirected to userdata"}
			}
			continue
		}
		if !sdAvailable {
			return nil, "", &backupAPIError{Code: "sd_missing", Status: http.StatusConflict, Message: "the archive contains SD data but no mounted SD card is available"}
		}
	}
	if selected[backup.ComponentDeviceIdentity] {
		if manifest.Mode != backup.ModeSameDevice {
			return nil, "", &backupAPIError{Code: "identity_not_portable", Status: http.StatusConflict, Message: "device identity is only valid for same-device backups"}
		}
		// A pending OTA boot means the current slot has not confirmed health;
		// changing machine-id now would fail that check and roll the slot back.
		if _, err := os.Stat(filepath.Join(s.options.BackupUserdataRoot, "ota/pending_boot.json")); err == nil {
			return nil, "", &backupAPIError{Code: "ota_pending_boot", Status: http.StatusConflict, Message: "device identity restore is blocked while an OTA boot is pending"}
		}
		hardware := s.readHardwareID()
		switch {
		case hardware == "" || manifest.Source.HardwareID == "":
			if !request.ConfirmIdentity {
				return nil, "", &backupAPIError{Code: "identity_confirmation_required", Status: http.StatusConflict, Message: "the device cannot verify that this backup belongs to it; confirm identity restore explicitly"}
			}
		case hardware != manifest.Source.HardwareID:
			return nil, "", &backupAPIError{Code: "source_device_mismatch", Status: http.StatusConflict, Message: "backup belongs to a different device"}
		}
	}
	for _, file := range manifest.Files {
		if file.Conflict && selected[file.Component] && !request.ConfirmConflicts {
			return nil, "", &backupAPIError{Code: "audio_conflict", Status: http.StatusConflict, Message: "conflicting audio files require explicit confirmation"}
		}
	}
	return selected, strategy, nil
}

func restoreNeedsSDLease(selected map[backup.ComponentID]bool, strategy string) bool {
	if strategy == "skip" || strategy == "emmc_fallback" {
		return false
	}
	return selected[backup.ComponentSDManagedAudio] || selected[backup.ComponentSDUserFiles]
}

func (s *Server) acquireRestoreSDLease(ctx context.Context, manifest backup.Manifest, strategy string) (agent.StorageSnapshotLease, error) {
	storage := s.currentStorage()
	if storage == nil {
		return nil, &backupAPIError{Code: "sd_missing", Status: http.StatusServiceUnavailable, Message: "storage manager unavailable"}
	}
	lease, err := storage.AcquireSnapshotLease(ctx)
	if err != nil {
		return nil, &backupAPIError{Code: "sd_missing", Status: http.StatusConflict, Message: sanitizeBackupError(err)}
	}
	snapshot := lease.Snapshot()
	if strategy == "require_match" && manifest.Storage.SDUUID != "" && snapshot.FilesystemUUID != manifest.Storage.SDUUID {
		lease.Release()
		return nil, &backupAPIError{Code: "sd_uuid_mismatch", Status: http.StatusConflict, Message: "the mounted SD card does not match the backup"}
	}
	if err := lease.Validate(ctx); err != nil {
		lease.Release()
		return nil, &backupAPIError{Code: "sd_changed", Status: http.StatusConflict, Message: sanitizeBackupError(err)}
	}
	return lease, nil
}

func (s *Server) validateRestoreSDLease(job *restoreJob, ctx context.Context) error {
	job.mu.Lock()
	lease := job.sdLease
	job.mu.Unlock()
	if lease == nil {
		return nil
	}
	return lease.Validate(ctx)
}

func preflightRestoreSpace(s *Server, manifest backup.Manifest, selected map[backup.ComponentID]bool, strategy string) error {
	var userdataBytes, sdBytes int64
	for _, component := range manifest.Components {
		if !selected[component.ID] {
			continue
		}
		if component.ID == backup.ComponentSDManagedAudio && strategy == "emmc_fallback" {
			userdataBytes += component.ExpandedSize
		} else if component.ID == backup.ComponentSDManagedAudio || component.ID == backup.ComponentSDUserFiles {
			sdBytes += component.ExpandedSize
		} else {
			userdataBytes += component.ExpandedSize
		}
	}
	if err := checkFreeSpace(s.options.BackupUserdataRoot, userdataBytes); err != nil {
		return err
	}
	if sdBytes > 0 {
		if err := checkFreeSpace(s.options.BackupSDRoot, sdBytes); err != nil {
			return err
		}
	}
	return nil
}

func checkFreeSpace(path string, required int64) error {
	if required < 0 {
		required = 0
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return fmt.Errorf("inspect restore filesystem space at %s: %w", path, err)
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	if free < required+restoreSafetyBytes {
		return fmt.Errorf("not enough free space at %s (need %d bytes, have %d)", path, required+restoreSafetyBytes, free)
	}
	return nil
}

type restoreTargetSpec struct {
	layer     string
	rootRel   string
	within    string
	unit      string
	directory bool
	file      bool
}

// restoreTargetSpecFor is the single mapping table between archive paths and
// the persistent layout.  Archive paths remain stable even when the target
// firmware changes its on-disk parent directories.
func restoreTargetSpecFor(file backup.FileManifest, strategy string) (restoreTargetSpec, error) {
	id, p := file.Component, filepath.ToSlash(file.Path)
	spec := restoreTargetSpec{layer: backup.LayerUserdata}
	switch id {
	case backup.ComponentAgentConfig:
		if p != "agent.toml" {
			return spec, fmt.Errorf("unexpected agent_config path %q", p)
		}
		spec.rootRel, spec.unit, spec.file = "agent/agent.toml", "agent-config", true
	case backup.ComponentSystemEnvironment:
		if p != "env" {
			return spec, fmt.Errorf("unexpected system_environment path %q", p)
		}
		spec.rootRel, spec.unit, spec.file = "system/env", "system-environment", true
	case backup.ComponentNetwork:
		switch p {
		case "wpa_supplicant-wlan0.conf":
			spec.rootRel, spec.unit = "debian/wifi/wpa_supplicant-wlan0.conf", "network-wpa"
		case "wifi-proxies.json":
			spec.rootRel, spec.unit = "system/wifi-proxies.json", "network-proxy"
		case "wifi-region.json":
			spec.rootRel, spec.unit = "system/wifi-region.json", "network-region"
		default:
			return spec, fmt.Errorf("unexpected network path %q", p)
		}
		spec.file = true
	case backup.ComponentOTASettings:
		if p != "settings.json" {
			return spec, fmt.Errorf("unexpected ota_settings path %q", p)
		}
		// The staged file is merged into the target firmware's config.json
		// before commit; it must never replace factory/runtime OTA fields.
		spec.rootRel, spec.unit, spec.file = "debian/ota/config.json", "ota-settings", true
	case backup.ComponentAgentSkills:
		if p == "skills" || strings.HasPrefix(p, "skills/") {
			spec.rootRel, spec.unit, spec.directory = "agent/skills", "agent-skills", true
			spec.within = strings.TrimPrefix(p, "skills")
		} else if p == "skill-state" || strings.HasPrefix(p, "skill-state/") {
			spec.rootRel, spec.unit, spec.directory = "agent/skill-state", "agent-skill-state", true
			spec.within = strings.TrimPrefix(p, "skill-state")
		} else {
			return spec, fmt.Errorf("unexpected agent_skills path %q", p)
		}
	case backup.ComponentAgentMemory:
		spec.rootRel, spec.unit, spec.directory = "agent/memory", "agent-memory", true
		spec.within = p
	case backup.ComponentAgentSessions:
		spec.rootRel, spec.unit, spec.directory = "agent/sessions", "agent-sessions", true
		spec.within = p
	case backup.ComponentUserHome:
		spec.rootRel, spec.unit, spec.directory = "userhome", "user-home", true
		spec.within = p
	case backup.ComponentPreferences:
		if p != "audio_service/playback_volume" {
			return spec, fmt.Errorf("unexpected preferences path %q", p)
		}
		spec.rootRel, spec.unit, spec.file = "audio_service/playback_volume", "preferences", true
	case backup.ComponentDeviceIdentity:
		switch {
		case p == "machine-id":
			spec.rootRel, spec.unit, spec.file = "system/machine-id", "identity-machine", true
		case p == "ssh" || strings.HasPrefix(p, "ssh/"):
			spec.rootRel, spec.unit, spec.directory = "system/ssh", "identity-ssh", true
			spec.within = strings.TrimPrefix(p, "ssh")
		case p == "bluetooth" || strings.HasPrefix(p, "bluetooth/"):
			spec.rootRel, spec.unit, spec.directory = "ble_service/bluetooth", "identity-bluetooth", true
			spec.within = strings.TrimPrefix(p, "bluetooth")
		default:
			return spec, fmt.Errorf("unexpected device_identity path %q", p)
		}
	case backup.ComponentAudioArchive:
		spec.rootRel, spec.unit, spec.directory, spec.within = "audio", "audio-archive", true, p
	case backup.ComponentSDManagedAudio:
		if strategy == "emmc_fallback" {
			spec.layer, spec.rootRel, spec.unit = backup.LayerUserdata, "audio", "sd-audio-fallback"
		} else {
			spec.layer, spec.rootRel, spec.unit = backup.LayerSD, "aico/audio", "sd-managed-audio"
		}
		spec.directory, spec.within = true, p
	case backup.ComponentSDUserFiles:
		if p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") ||
			p != path.Clean(p) || strings.ContainsAny(p, "\\\x00") {
			return spec, fmt.Errorf("SD user file path %q must be a normalized relative path", p)
		}
		// Exchanging a managed ancestor would replace restored audio or the
		// live transaction itself. Share backup's FAT alias boundary.
		if backup.IsReservedSDUserPath(p) {
			return spec, fmt.Errorf("SD user files cannot restore reserved path %q; exclude sd_user_files from this restore", p)
		}
		spec.layer, spec.unit, spec.file = backup.LayerSD, "sd-user-"+shortRestoreKey(p), true
		spec.rootRel = p
	case backup.ComponentPythonEnvironment:
		spec.rootRel, spec.unit, spec.directory, spec.within = "agent/python", "python-environment", true, p
	case backup.ComponentDiagnostics:
		if p == "agent" || strings.HasPrefix(p, "agent/") {
			spec.rootRel, spec.unit, spec.directory, spec.within = "agent/log", "diagnostics-agent", true, strings.TrimPrefix(p, "agent")
		} else if p == "system" || strings.HasPrefix(p, "system/") {
			spec.rootRel, spec.unit, spec.directory, spec.within = "log", "diagnostics-system", true, strings.TrimPrefix(p, "system")
		} else {
			return spec, fmt.Errorf("unexpected diagnostics path %q", p)
		}
	default:
		return spec, fmt.Errorf("unsupported restore component %s", id)
	}
	if spec.within != "" {
		spec.within = strings.TrimPrefix(filepath.ToSlash(spec.within), "/")
	}
	return spec, nil
}

func shortRestoreKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:6])
}

func (s *Server) restoreLayerRoot(layer string) string {
	if layer == backup.LayerSD {
		return s.options.BackupSDRoot
	}
	return s.options.BackupUserdataRoot
}

// bindMountFor returns the mount point and remount script for units whose
// target is the source of a bind mount.  Empty options disable the handling,
// which is what host tests use.
func (s *Server) bindMountFor(unit string) (mountPoint, script string) {
	switch unit {
	case "user-home":
		if s.options.RootHomeMountPoint != "" && s.options.RootHomeScript != "" {
			return s.options.RootHomeMountPoint, s.options.RootHomeScript
		}
	case "identity-bluetooth":
		if s.options.BluetoothStateMountPoint != "" && s.options.BluetoothStateScript != "" {
			return s.options.BluetoothStateMountPoint, s.options.BluetoothStateScript
		}
	}
	return "", ""
}

func (s *Server) buildRestoreUnits(jobID string, manifest backup.Manifest, selected map[backup.ComponentID]bool, strategy string) (map[string]*backup.TransactionUnit, error) {
	units := make(map[string]*backup.TransactionUnit)
	for _, file := range manifest.Files {
		if !selected[file.Component] {
			continue
		}
		spec, err := restoreTargetSpecFor(file, strategy)
		if err != nil {
			return nil, err
		}
		root := s.restoreLayerRoot(spec.layer)
		target := filepath.Join(root, filepath.FromSlash(spec.rootRel))
		key := spec.layer + ":" + spec.unit
		if prior, exists := units[key]; exists {
			if prior.Target != target {
				return nil, fmt.Errorf("restore target collision for %s", file.Path)
			}
			continue
		}
		mountPoint, script := s.bindMountFor(spec.unit)
		units[key] = &backup.TransactionUnit{
			Key: key, Component: file.Component, Layer: spec.layer, Target: target,
			Staged:    filepath.Join(backup.TransactionDir(root, jobID), "new", spec.unit),
			Old:       filepath.Join(backup.TransactionDir(root, jobID), "old", spec.unit),
			Directory: spec.directory, BindMount: mountPoint, RemountScript: script,
		}
	}
	return units, nil
}

func (s *Server) stageRestoreEntry(job *restoreJob, stream *backup.ArchiveStream, entry backup.ArchiveEntry) error {
	job.mu.Lock()
	strategy := job.sdStrategy
	selected := job.selected[entry.Manifest.Component]
	ctx := job.parserCtx
	job.mu.Unlock()
	if !selected {
		return stream.CopyCurrentEntry(ctx, io.Discard)
	}
	spec, err := restoreTargetSpecFor(entry.Manifest, strategy)
	if err != nil {
		return err
	}
	root := s.restoreLayerRoot(spec.layer)
	stageRoot := filepath.Join(backup.TransactionDir(root, job.id), "new", spec.unit)
	stagePath := stageRoot
	if spec.within != "" {
		stagePath = filepath.Join(stageRoot, filepath.FromSlash(spec.within))
	}
	if err := backup.EnsureParent(stagePath); err != nil {
		return err
	}
	switch entry.Manifest.Type {
	case backup.FileTypeDirectory:
		if err := ensureRestoreDirectory(stagePath, parseRestoreMode(entry.Manifest.Mode)); err != nil {
			return err
		}
		// Directories and symlinks carry no body, but the stream still has to
		// mark the entry as consumed.
		return stream.CopyCurrentEntry(ctx, nil)
	case backup.FileTypeSymlink:
		if err := validateRestoreLink(stagePath, entry.Manifest.LinkTarget, stageRoot); err != nil {
			return err
		}
		if _, err := os.Lstat(stagePath); err == nil {
			return fmt.Errorf("staging path already exists: %s", stagePath)
		}
		if err := os.Symlink(filepath.FromSlash(entry.Manifest.LinkTarget), stagePath); err != nil {
			return err
		}
		return stream.CopyCurrentEntry(ctx, nil)
	case backup.FileTypeRegular:
		file, err := os.OpenFile(stagePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, parseRestoreMode(entry.Manifest.Mode))
		if err != nil {
			return err
		}
		if err := stream.CopyCurrentEntry(ctx, file); err != nil {
			_ = file.Close()
			_ = os.Remove(stagePath)
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		job.mu.Lock()
		job.stagedBytes += entry.Manifest.Size
		job.lastActivity = time.Now().UTC()
		job.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("unsupported restore entry type %s", entry.Manifest.Type)
	}
}

func parseRestoreMode(value string) os.FileMode {
	var mode uint32
	_, _ = fmt.Sscanf(value, "%o", &mode)
	return os.FileMode(mode & 0o777)
}

func ensureRestoreDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode.Perm()); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe restore directory %s", path)
	}
	return os.Chmod(path, mode.Perm())
}

func validateRestoreLink(path, target, root string) error {
	if target == "" || filepath.IsAbs(target) || strings.Contains(target, "\\") || strings.ContainsRune(target, 0) {
		return fmt.Errorf("unsafe restore symlink")
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(target)))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("restore symlink escapes component")
	}
	return nil
}

func (s *Server) handleRestoreValidate(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.restoreJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "restore job not found")
		return
	}
	if !s.authorizeRestore(w, r, id, true, false) {
		return
	}
	var request restoreValidateRequest
	if r.ContentLength != 0 && !readJSONBody(w, r, &request) {
		return
	}
	job.mu.Lock()
	state, parserErr, expectedHash, received, archiveSize := job.state, job.parserErr, job.expectedHash, job.receivedBytes, job.archiveSize
	actualHash := ""
	if job.archiveHash != nil {
		actualHash = hex.EncodeToString(job.archiveHash.Sum(nil))
	}
	planSet, planDigest := job.planSet, job.planDigest
	job.lastActivity = time.Now().UTC()
	job.mu.Unlock()
	if parserErr != nil {
		s.writeBackupError(w, parserErr)
		return
	}
	if state != restoreJobValidating {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_not_ready", "state": state})
		return
	}
	if received != archiveSize {
		writeJSONError(w, http.StatusConflict, "restore archive is incomplete")
		return
	}
	if !planSet || planDigest == "" {
		writeJSONError(w, http.StatusConflict, "restore plan is required")
		return
	}
	if err := s.validateRestoreSDLease(job, r.Context()); err != nil {
		s.failRestore(job, "sd_changed", err)
		s.writeBackupError(w, &backupAPIError{Code: "sd_changed", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	// Optional outer SHA-256 cross-check from the create request or from a
	// client that streamed the digest while uploading.
	submitted := strings.ToLower(strings.TrimSpace(request.ArchiveSHA256))
	if submitted != "" && !isHexDigest(submitted) {
		writeJSONError(w, http.StatusBadRequest, "archive_sha256 must be a SHA-256 digest")
		return
	}
	for _, want := range []string{expectedHash, submitted} {
		if want != "" && want != actualHash {
			s.failRestore(job, "hash_mismatch", errors.New("archive hash mismatch"))
			s.writeBackupError(w, &backupAPIError{Code: "hash_mismatch", Status: http.StatusBadRequest, Message: "archive hash mismatch"})
			return
		}
	}
	if err := syncRestoreRoots(s, job); err != nil {
		s.writeBackupError(w, &backupAPIError{Code: "staging_sync_failed", Status: http.StatusServiceUnavailable, Message: sanitizeBackupError(err)})
		return
	}
	job.mu.Lock()
	job.state, job.phase = restoreJobPrepared, "prepared"
	job.transactionState = backup.TransactionPrepared
	job.mu.Unlock()
	if err := s.persistRestoreTransaction(job); err != nil {
		s.failRestore(job, "staging_sync_failed", err)
		s.writeBackupError(w, &backupAPIError{Code: "staging_sync_failed", Status: http.StatusServiceUnavailable, Message: sanitizeBackupError(err)})
		return
	}
	s.restoreJobs.persist(job)
	payload := job.publicMap()
	payload["ok"] = true
	payload["archive_sha256"] = actualHash
	writeJSON(w, http.StatusOK, payload)
}

func syncRestoreRoots(s *Server, job *restoreJob) error {
	roots := map[string]bool{backup.LayerUserdata: true}
	job.mu.Lock()
	for _, unit := range job.units {
		roots[unit.Layer] = true
	}
	job.mu.Unlock()
	for layer := range roots {
		root := s.restoreLayerRoot(layer)
		fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			if layer == backup.LayerSD {
				return err
			}
			continue
		}
		err = unix.Syncfs(fd)
		_ = unix.Close(fd)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) handleRestoreApply(w http.ResponseWriter, r *http.Request, id string) {
	job, ok := s.restoreJobs.get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "restore job not found")
		return
	}
	if !s.authorizeRestore(w, r, id, true, false) {
		return
	}
	var request restoreApplyRequest
	if !readJSONBody(w, r, &request) {
		return
	}
	job.mu.Lock()
	if job.state != restoreJobPrepared {
		state := job.state
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "restore_not_prepared", "state": state})
		return
	}
	if request.PlanDigest == "" || request.PlanDigest != job.planDigest {
		job.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "plan_digest_mismatch", "message": "restore plan digest does not match"})
		return
	}
	if strings.ToUpper(strings.TrimSpace(request.Confirm)) != "RESTORE" {
		job.mu.Unlock()
		writeJSONError(w, http.StatusBadRequest, "restore confirmation is required")
		return
	}
	job.state, job.phase = restoreJobQuiescing, "quiescing"
	job.lastActivity = time.Now().UTC()
	// Commit, identity provisioning and service restarts can exceed the
	// server's global write timeout; the client polls status meanwhile.
	if controller := http.NewResponseController(w); controller != nil {
		_ = controller.SetWriteDeadline(time.Time{})
	}
	components := make([]backup.ComponentID, 0, len(job.selected))
	for id := range job.selected {
		components = append(components, id)
	}
	sort.Slice(components, func(i, j int) bool { return components[i] < components[j] })
	lease := job.maintenance
	already := job.services
	job.mu.Unlock()
	s.restoreJobs.persist(job)
	if lease == nil {
		// Only reachable if ingest never ran; keep the invariant that apply
		// always holds the global lock.
		var leaseErr error
		lease, leaseErr = s.maintenance.begin("restore", id, false)
		if leaseErr != nil {
			s.failRestore(job, "maintenance_in_progress", leaseErr)
			s.writeBackupError(w, leaseErr)
			return
		}
		job.mu.Lock()
		job.maintenance = lease
		job.mu.Unlock()
	}
	lease.Update("quiescing")
	if err := s.validateRestoreSDLease(job, r.Context()); err != nil {
		s.failRestore(job, "sd_changed", err)
		s.writeBackupError(w, &backupAPIError{Code: "sd_changed", Status: http.StatusConflict, Message: sanitizeBackupError(err)})
		return
	}
	// The apply phase must not be tied to the client connection: once commit
	// starts it runs to completion or rollback regardless of the browser.
	ctx := context.Background()
	stopped, err := s.quiesceUnits(ctx, restoreQuiesceUnits(components), already)
	if err != nil {
		s.failRestore(job, "service_quiesce_failed", err)
		s.writeBackupError(w, &backupAPIError{Code: "service_quiesce_failed", Status: http.StatusServiceUnavailable, Message: "unable to stop data-writing services"})
		return
	}
	job.mu.Lock()
	job.services = append(job.services, stopped...)
	job.state, job.phase = restoreJobCommitting, "committing"
	job.transactionState = backup.TransactionCommitting
	job.mu.Unlock()
	lease.Update("committing")
	s.restoreJobs.persist(job)
	if err := s.commitRestore(job, ctx); err != nil {
		rollbackErr := s.rollbackRestore(job)
		if rollbackErr != nil {
			s.finishRestoreJob(job, restoreJobRollbackFailed, "rollback_failed", sanitizeBackupError(fmt.Errorf("%v; rollback: %w", err, rollbackErr)))
			s.writeBackupError(w, &backupAPIError{Code: "rollback_failed", Status: http.StatusInternalServerError, Message: "restore failed and rollback could not be completed"})
			return
		}
		s.finishRestoreJob(job, restoreJobFailed, "commit_failed", sanitizeBackupError(err))
		s.writeBackupError(w, &backupAPIError{Code: "commit_failed", Status: http.StatusInternalServerError, Message: sanitizeBackupError(err)})
		return
	}

	job.mu.Lock()
	job.state, job.phase = restoreJobPostProcessing, "post_processing"
	identity := job.selected[backup.ComponentDeviceIdentity]
	job.mu.Unlock()
	lease.Update("post_processing")
	s.restoreJobs.persist(job)
	if identity {
		reboot, err := s.provisionRestoredIdentity(ctx)
		if err != nil {
			// The data is already committed and durable; provisioning is
			// retried by aico-machine-id.service on the next boot.
			s.addRestoreWarning(job, "identity provisioning did not complete: "+sanitizeBackupError(err)+"; it is retried at next boot")
			reboot = true
		}
		job.mu.Lock()
		job.identityProvisioned, job.identityRebootRequired = err == nil, reboot
		job.mu.Unlock()
	}
	job.mu.Lock()
	job.transactionState = backup.TransactionCommitted
	job.mu.Unlock()
	if err := s.persistRestoreTransaction(job); err != nil {
		s.addRestoreWarning(job, "transaction log update failed: "+sanitizeBackupError(err))
	}

	job.mu.Lock()
	job.state, job.phase = restoreJobResuming, "resuming_services"
	sdLease := job.sdLease
	job.sdLease = nil
	services := job.services
	job.services = nil
	rebootRequired := job.identityRebootRequired
	job.mu.Unlock()
	lease.Update("resuming_services")
	s.restoreJobs.persist(job)
	// Release the SD lease before services start so storage reconfiguration
	// and migration are possible again.
	if sdLease != nil {
		sdLease.Release()
	}
	if err := s.resumeRestoredServices(ctx, services, components); err != nil {
		s.addRestoreWarning(job, "service restart failed: "+sanitizeBackupError(err))
	}
	s.applyRestoredConfig(job, components)
	// Every service is running from the restored data and the bind mounts
	// have been verified; rollback copies are no longer needed.
	s.cleanupRestoreTransaction(job, true)
	if rebootRequired {
		s.finishRestoreJob(job, restoreJobRebootRequired, "", "")
		s.scheduleRestoreReboot(job)
	} else {
		s.finishRestoreJob(job, restoreJobCompleted, "", "")
	}
	writeJSON(w, http.StatusAccepted, job.publicMap())
}

func (s *Server) addRestoreWarning(job *restoreJob, message string) {
	job.mu.Lock()
	job.warnings = append(job.warnings, message)
	job.mu.Unlock()
	logConfigWebError("restore " + job.id + ": " + message)
}

// provisionRestoredIdentity re-runs the OTA identity provisioning after a
// machine-id restore.  It reports whether a reboot is required.
func (s *Server) provisionRestoredIdentity(ctx context.Context) (bool, error) {
	return s.identityProvisioner().ProvisionIdentity(ctx)
}

func (s *Server) identityProvisioner() backup.OTAIdentityProvisioner {
	return backup.OTAIdentityProvisioner{Binary: s.options.OTABinary, ConfigPath: s.options.OTAConfigPath}
}

// scheduleRestoreReboot reboots the device shortly after the apply response
// has been written so the client sees reboot_required first.
func (s *Server) scheduleRestoreReboot(job *restoreJob) {
	binary := strings.TrimSpace(s.options.SystemctlBinary)
	if binary == "" {
		return
	}
	time.AfterFunc(restoreRebootDelay, func() {
		result := runCommand(30*time.Second, nil, nil, binary, "reboot", "--no-block")
		if result.ExitCode != 0 {
			s.addRestoreWarning(job, "automatic reboot could not be scheduled; reboot the device manually")
			s.restoreJobs.persist(job)
		}
	})
}

// applyRestoredConfig re-applies the frame/storage side of a restored Agent
// TOML with the same internal logic the TOML import endpoint uses.
func (s *Server) applyRestoredConfig(job *restoreJob, components []backup.ComponentID) {
	if !containsComponent(components, backup.ComponentAgentConfig) {
		return
	}
	s.configMu.Lock()
	s.frameApplyPending = true
	s.storageApplyPending = true
	s.configMu.Unlock()
	if err := s.applyConfigServices(); err != nil {
		s.addRestoreWarning(job, "configuration apply: "+sanitizeBackupError(err))
	}
}

func (s *Server) commitRestore(job *restoreJob, ctx context.Context) error {
	job.mu.Lock()
	units := make([]*backup.TransactionUnit, 0, len(job.units))
	for _, unit := range job.units {
		units = append(units, unit)
	}
	job.mu.Unlock()
	backup.SortUnits(units)
	if err := prepareOTAStagedFiles(s, job); err != nil {
		return err
	}
	checkpoint := func() error { return s.persistRestoreTransaction(job) }
	if err := checkpoint(); err != nil {
		return err
	}
	for _, unit := range units {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.validateRestoreSDLease(job, ctx); err != nil {
			return err
		}
		if err := backup.CommitUnit(unit, s.mounts, checkpoint); err != nil {
			return fmt.Errorf("commit %s: %w", unit.Key, err)
		}
	}
	return syncRestoreRoots(s, job)
}

func (s *Server) rollbackRestore(job *restoreJob) error {
	job.mu.Lock()
	units := make([]*backup.TransactionUnit, 0, len(job.units))
	for _, unit := range job.units {
		units = append(units, unit)
	}
	job.state, job.phase = restoreJobRollingBack, "rolling_back"
	job.transactionState = backup.TransactionRollingBack
	job.mu.Unlock()
	backup.SortUnits(units)
	checkpoint := func() error { return s.persistRestoreTransaction(job) }
	if err := checkpoint(); err != nil {
		return err
	}
	var first error
	for index := len(units) - 1; index >= 0; index-- {
		if err := backup.RollbackUnit(units[index], s.mounts, checkpoint); err != nil && first == nil {
			first = fmt.Errorf("rollback %s: %w", units[index].Key, err)
		}
	}
	if first != nil {
		return first
	}
	job.mu.Lock()
	job.transactionState = backup.TransactionRolledBack
	job.mu.Unlock()
	return checkpoint()
}

func (s *Server) validateRestoreStaging(job *restoreJob) error {
	job.mu.Lock()
	units := make([]*backup.TransactionUnit, 0, len(job.units))
	for _, unit := range job.units {
		units = append(units, unit)
	}
	manifest := job.manifest
	selected := job.selected
	strategy := job.sdStrategy
	job.mu.Unlock()
	for _, file := range manifest.Files {
		if !selected[file.Component] {
			continue
		}
		spec, err := restoreTargetSpecFor(file, strategy)
		if err != nil {
			return err
		}
		root := s.restoreLayerRoot(spec.layer)
		stageRoot := filepath.Join(backup.TransactionDir(root, job.id), "new", spec.unit)
		stagePath := stageRoot
		if spec.within != "" {
			stagePath = filepath.Join(stageRoot, filepath.FromSlash(spec.within))
		}
		info, err := os.Lstat(stagePath)
		if err != nil {
			return fmt.Errorf("staged entry missing for %s: %w", file.Path, err)
		}
		switch file.Type {
		case backup.FileTypeRegular:
			if !info.Mode().IsRegular() || info.Size() != file.Size {
				return fmt.Errorf("staged regular file metadata mismatch for %s", file.Path)
			}
		case backup.FileTypeDirectory:
			if !info.IsDir() {
				return fmt.Errorf("staged directory metadata mismatch for %s", file.Path)
			}
		case backup.FileTypeSymlink:
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("staged symlink metadata mismatch for %s", file.Path)
			}
		}
	}
	// Validate the Agent TOML with exactly the same schema path used by the
	// existing import endpoint before it can be committed.
	for _, unit := range units {
		if unit.Component != backup.ComponentAgentConfig {
			continue
		}
		data, err := os.ReadFile(unit.Staged)
		if err != nil {
			return err
		}
		if err := validateCanonicalConfigBackup(data); err != nil {
			return fmt.Errorf("agent configuration validation: %w", err)
		}
		if _, err := validateAgentConfigBackup(filepath.Dir(unit.Staged), data, 0o640); err != nil {
			return fmt.Errorf("agent configuration validation: %w", err)
		}
	}
	return nil
}

// persistRestoreTransaction writes one transaction copy per filesystem that
// takes part in the restore.
func (s *Server) persistRestoreTransaction(job *restoreJob) error {
	job.mu.Lock()
	id, state, phase := job.id, job.transactionState, job.phase
	identitySelected := job.selected[backup.ComponentDeviceIdentity]
	identityProvisioned, identityRebootRequired := job.identityProvisioned, job.identityRebootRequired
	lease := job.sdLease
	groups := map[string][]*backup.TransactionUnit{}
	for _, unit := range job.units {
		layer := unit.Layer
		if layer == "" {
			layer = backup.LayerUserdata
		}
		groups[layer] = append(groups[layer], unit)
	}
	job.mu.Unlock()
	if state == "" {
		state = backup.TransactionPrepared
	}
	required := make([]string, 0, 2)
	for _, layer := range []string{backup.LayerUserdata, backup.LayerSD} {
		if len(groups[layer]) > 0 {
			required = append(required, layer)
		}
	}
	for _, layer := range required {
		if layer != backup.LayerUserdata && layer != backup.LayerSD {
			return fmt.Errorf("unsupported restore transaction layer %q", layer)
		}
		if layer == backup.LayerSD {
			if lease == nil {
				return errors.New("SD restore transaction requires an active mount lease")
			}
			if err := lease.Validate(context.Background()); err != nil {
				return fmt.Errorf("validate SD restore transaction lease: %w", err)
			}
		}
		units := groups[layer]
		backup.SortUnits(units)
		txn := backup.Transaction{
			JobID: id, Layer: layer, RequiredLayers: required, State: state, Phase: phase,
			IdentitySelected: identitySelected, IdentityProvisioned: identityProvisioned,
			IdentityRebootRequired: identityRebootRequired, Units: units,
		}
		if err := backup.WriteTransaction(s.restoreLayerRoot(layer), txn); err != nil {
			return err
		}
	}
	return nil
}

// prepareOTAStagedFiles merges the whitelisted OTA user fields into a copy of
// the target firmware's config.json so the commit is a plain file exchange.
func prepareOTAStagedFiles(s *Server, job *restoreJob) error {
	job.mu.Lock()
	unit := job.units[backup.LayerUserdata+":ota-settings"]
	job.mu.Unlock()
	if unit == nil {
		return nil
	}
	data, err := os.ReadFile(unit.Staged)
	if err != nil {
		return err
	}
	var incoming map[string]any
	if err := json.Unmarshal(data, &incoming); err != nil {
		return err
	}
	existing := map[string]any{}
	if old, err := os.ReadFile(unit.Target); err == nil {
		if err := json.Unmarshal(old, &existing); err != nil {
			return fmt.Errorf("current OTA configuration is not valid JSON: %w", err)
		}
	}
	for _, key := range []string{"manifest_url", "github_proxy_url", "github_token"} {
		if value, ok := incoming[key].(string); ok && strings.TrimSpace(value) != "" {
			existing[key] = value
		}
	}
	if value, ok := incoming["public_key"].(map[string]any); ok {
		existing["public_key"] = value
	}
	merged, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	merged = append(merged, '\n')
	return os.WriteFile(unit.Staged, merged, 0o600)
}

// abortRestore cancels a job from outside its handlers (client cancel, server
// shutdown, watchdog) and releases everything the job holds.
func (s *Server) abortRestore(job *restoreJob, code string, err error) {
	state := restoreJobFailed
	if code == "cancelled" || code == "server_shutdown" || code == "job_expired" {
		state = restoreJobCancelled
	}
	s.finishRestoreJobWithPolicy(job, state, code, sanitizeBackupError(err), true)
}

func (s *Server) failRestore(job *restoreJob, code string, err error) {
	if err == nil {
		err = errors.New(code)
	}
	if code == "" {
		code = "restore_failed"
	}
	job.mu.Lock()
	if isTerminalRestoreState(job.state) {
		job.mu.Unlock()
		return
	}
	job.parserErr = err
	parserCancel := job.parserCancel
	input := job.input
	job.mu.Unlock()
	if parserCancel != nil {
		parserCancel()
	}
	if input != nil {
		input.CloseWithError(err)
	}
	s.finishRestoreJob(job, restoreJobFailed, code, sanitizeBackupError(err))
}

// finishRestoreJob moves the job to a terminal state exactly once and releases
// the SD lease, maintenance lock, key material, transfer token and services.
func (s *Server) finishRestoreJob(job *restoreJob, state, code, message string) {
	s.finishRestoreJobWithPolicy(job, state, code, message, false)
}

func restoreCommitInProgress(state string) bool {
	return state == restoreJobQuiescing || state == restoreJobCommitting || state == restoreJobPostProcessing || state == restoreJobResuming || state == restoreJobRollingBack
}

func (s *Server) finishRestoreJobWithPolicy(job *restoreJob, state, code, message string, abortOnly bool) {
	job.mu.Lock()
	// Check and claim the terminal state under the same lock as apply's
	// prepared -> committing transition. Shutdown must not release a lease
	// or restart writers underneath an in-flight commit or rollback.
	if isTerminalRestoreState(job.state) || (abortOnly && restoreCommitInProgress(job.state)) {
		job.mu.Unlock()
		return
	}
	job.state, job.phase, job.finishedAt = state, state, time.Now().UTC()
	if code != "" {
		job.parserErr = &backup.Error{Code: code, Message: message}
	}
	maintenance, sdLease, cancel, material := job.maintenance, job.sdLease, job.cancel, job.material
	parserCancel, input := job.parserCancel, job.input
	services := job.services
	job.cancel, job.material, job.parserCancel, job.input = nil, nil, nil, nil
	// A failed rollback leaves a live filesystem in an indeterminate state.
	// Hold maintenance and the SD snapshot lease, and keep writers stopped,
	// until a reboot lets the mandatory recovery gate retry the transaction.
	rollbackFailed := state == restoreJobRollbackFailed
	if !rollbackFailed {
		job.maintenance, job.sdLease, job.services = nil, nil, nil
	}
	job.mu.Unlock()
	if parserCancel != nil {
		parserCancel()
	}
	if input != nil {
		input.CloseWithError(context.Canceled)
	}
	if cancel != nil {
		cancel()
	}
	if state == restoreJobCompleted || state == restoreJobRebootRequired {
		s.cleanupRestoreTransaction(job, true)
	} else if state == restoreJobRollbackFailed {
		// Keep new/ and old/ for manual inspection; the transaction log stays
		// in rolling_back so the boot recovery retries the rollback.
	} else {
		s.cleanupRestoreStaging(job)
	}
	if sdLease != nil && !rollbackFailed {
		sdLease.Release()
	}
	if len(services) > 0 && !rollbackFailed {
		if err := s.resumeServices(context.Background(), services); err != nil {
			logConfigWebError("restore " + job.id + ": resume services: " + err.Error())
		}
	}
	if material != nil {
		material.Destroy()
	}
	if maintenance != nil && !rollbackFailed {
		maintenance.Release()
	}
	if s.maintenanceSessions != nil {
		s.maintenanceSessions.revokeJobToken(job.id)
	}
	if s.restoreJobs != nil {
		s.restoreJobs.persist(job)
	}
	closeOnce(job.done)
	s.startDeferredRestartIfIdle()
}

func (s *Server) cleanupRestoreTransaction(job *restoreJob, removeOld bool) {
	for _, root := range []string{s.options.BackupUserdataRoot, s.options.BackupSDRoot} {
		if err := backup.CleanupTransaction(root, job.id, removeOld); err != nil {
			logConfigWebError("restore " + job.id + ": cleanup below " + root + ": " + err.Error())
		}
	}
}

// cleanupRestoreStaging removes staging for a job that never started to
// commit.  The transaction log (if any) is removed as well because a prepared
// transaction must never be resumed at boot.
func (s *Server) cleanupRestoreStaging(job *restoreJob) {
	job.mu.Lock()
	state := job.transactionState
	job.mu.Unlock()
	if state == backup.TransactionCommitting || state == backup.TransactionRollingBack {
		return
	}
	s.cleanupRestoreTransaction(job, true)
}

func isTerminalRestoreState(state string) bool {
	return state == restoreJobCompleted || state == restoreJobRebootRequired || state == restoreJobCancelled || state == restoreJobFailed || state == restoreJobRollbackFailed
}

// recoverRestoreTransactions sweeps leftover transactions when Config Web
// starts: orphaned staging from a crashed ingest is removed, and a commit or
// rollback interrupted by a Config Web crash is finished.
func (s *Server) recoverRestoreTransactions() {
	if s.options.BackupUserdataRoot == "" {
		return
	}
	results, err := backup.RecoverTransactions(context.Background(), backup.RecoveryOptions{
		Roots:    backup.Roots{Userdata: s.options.BackupUserdataRoot, SD: s.options.BackupSDRoot},
		Mounts:   s.mounts,
		Identity: s.identityProvisioner(),
		Logf:     func(format string, args ...any) { logConfigWebError(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		logConfigWebError("restore recovery: " + err.Error())
	}
	s.configMu.Lock()
	s.restoreRecovery = results
	s.configMu.Unlock()
}

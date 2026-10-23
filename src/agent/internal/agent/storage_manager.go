package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"aico-agent/internal/logging"
)

// StorageMode reports which storage tiers are currently available. The mode
// is derived purely from card availability: a usable card enables the SD tier,
// while no usable card leaves eMMC as the only tier. New data is always written
// to eMMC; when the SD tier is available, older data migrates there as eMMC
// crosses the configured free-space watermarks. There is no user preference.
type StorageMode int

const (
	// StorageModeEMMCOnly is the fallback when no usable card is mounted.
	StorageModeEMMCOnly StorageMode = 1
	// StorageModeDual makes both eMMC and SD available to governed data.
	StorageModeDual StorageMode = 2
)

// StorageDataClass identifies a governed application-data category.
type StorageDataClass string

const (
	StorageClassAudio StorageDataClass = "audio"
	StorageClassLogs  StorageDataClass = "logs"
)

// storageSDSubdir is the dedicated subtree on the card; the card's own
// content outside it is never touched.
const storageSDSubdir = "aico"

// StorageFormatConfirmToken must be sent by clients to confirm a destructive format.
const StorageFormatConfirmToken = "format-sd-card"

// Filesystems the format job can create. FAT32 is the default for PC/phone
// interoperability; ext4 is journaled and has on-device fsck support; exFAT
// keeps PC/phone interoperability on large cards without FAT32's 4 GB
// file-size limit.
const (
	StorageFormatFAT32 = "fat32"
	StorageFormatExt4  = "ext4"
	StorageFormatExFAT = "exfat"
)

// Format job states surfaced through Status and the state mirror.
const (
	StorageFormatIdle    = "idle"
	StorageFormatRunning = "running"
	StorageFormatSuccess = "success"
	StorageFormatFailed  = "failed"
)

// storageVolumeLabel is stamped on freshly formatted cards.
const storageVolumeLabel = "AICO"

// Migration tuning. Only immutable, closed files are moved: the minimum age
// guards against touching a file that is still being written.
const (
	migrationMinFileAge    = 60 * time.Second
	migrationFilePause     = 100 * time.Millisecond // throttle between files
	migrationRetryCooldown = 10 * time.Minute       // after failed/exhausted runs
	migrationPartialSuffix = ".aico-partial"        // in-flight copy marker on SD
)

// storageStateFileName mirrors runtime state for external consumers.
const defaultStorageStatePath = "/run/aico/storage.state"

// StorageCardStatus describes the SD card as last observed.
type StorageCardStatus struct {
	Present    bool   `json:"present"`
	Mounted    bool   `json:"mounted"`
	Device     string `json:"device,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	FreeBytes  int64  `json:"free_bytes,omitempty"`
	// Reason explains why a present card is not usable (mount/validation failure).
	Reason string `json:"reason,omitempty"`
}

// StorageFormatJob tracks the asynchronous card-format task. Formatting
// rewrites the whole card: a fresh MBR with one partition plus a new
// filesystem, so it runs as a background job with polled status.
type StorageFormatJob struct {
	Status string `json:"status"` // idle | running | success | failed
	FS     string `json:"fs,omitempty"`
	Error  string `json:"error,omitempty"`
	// Auto marks a job started by blank-card detection rather than a user.
	Auto       bool      `json:"auto,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// StorageMigrationJob tracks the background eMMC→SD migration of older
// governed data, triggered by the eMMC free-space watermarks.
type StorageMigrationJob struct {
	Status string `json:"status"` // idle | running | success | failed
	// Detail is a human-readable summary of how the last run ended.
	Detail     string    `json:"detail,omitempty"`
	Error      string    `json:"error,omitempty"`
	MovedFiles int       `json:"moved_files,omitempty"`
	MovedBytes int64     `json:"moved_bytes,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// StorageStatus is the API-facing snapshot of the storage subsystem.
type StorageStatus struct {
	EffectiveMode StorageMode         `json:"effective_mode"`
	Card          StorageCardStatus   `json:"card"`
	MountPoint    string              `json:"mount_point"`
	FormatJob     StorageFormatJob    `json:"format_job"`
	Migration     StorageMigrationJob `json:"migration"`
	LastWarning   string              `json:"last_warning,omitempty"`
}

// StorageSnapshot identifies the exact mounted card covered by a backup or
// restore operation. MountID detects an unmount/remount even when the same
// device node and filesystem UUID reappear.
type StorageSnapshot struct {
	DevicePath     string `json:"device_path"`
	MountPoint     string `json:"mount_point"`
	FilesystemUUID string `json:"filesystem_uuid"`
	MountID        string `json:"mount_id"`
}

// StorageSnapshotLease prevents migration, formatting, eject and storage
// reconfiguration while a caller reads or stages SD-card data.
type StorageSnapshotLease interface {
	Snapshot() StorageSnapshot
	Validate(context.Context) error
	Release()
}

// StorageEvent notifies subscribers that the effective mode changed.
type StorageEvent struct {
	EffectiveMode StorageMode `json:"effective_mode"`
}

// storageSysOps abstracts the platform operations so the state machine is
// testable on the host.
type storageSysOps interface {
	// CardDevice reports the block device to use and whether the card is present.
	CardDevice() (dev string, present bool)
	// Prepare runs fsck, mounts dev at mountPoint read-write, and probe-writes.
	Prepare(dev, mountPoint string) error
	// Unmount detaches mountPoint; lazy detach is used for forced removal.
	// ErrStorageFlush means detachment succeeded after a failed flush.
	Unmount(mountPoint string, lazy bool) error
	// IsMounted reports whether mountPoint currently has a filesystem mounted.
	IsMounted(mountPoint string) bool
	// Healthy reports whether the mounted filesystem still responds.
	Healthy(mountPoint string) bool
	// VerifyWritable verifies a synced write and readback on the mounted card.
	VerifyWritable(mountPoint string) error
	// SpaceInfo returns free/total bytes for the filesystem containing path.
	SpaceInfo(path string) (free, total int64, err error)
	// FormatDisk rewrites the whole card: fresh MBR with one partition plus
	// a new filesystem of the given type. Destructive. Returns the partition
	// device node to mount.
	FormatDisk(fs string) (string, error)
	// CardIsBlank reports whether the card carries no recognizable content
	// at all: no kernel-visible partitions, no blkid signature (filesystem /
	// RAID / crypto / partition table), and no MBR/GPT magic in the first
	// sectors. Anything unverifiable must be reported as NOT blank.
	CardIsBlank() bool
}

// StorageManager owns SD card mounting, mode derivation, and per-class
// directory resolution. It is the only writer of the preference file and the
// runtime state mirror.
type StorageManager struct {
	cfg          StorageConfig
	logger       *Logger
	ops          storageSysOps
	statePath    string // "" disables the state mirror
	mirrorOnly   bool   // read state written by the Config Web owner; never touch hardware
	emmcRoot     string
	pollInterval time.Duration

	mu              sync.Mutex
	card            StorageCardStatus
	lastEffective   StorageMode
	ejected         bool // safe-eject latch; cleared when the card is removed
	mountFailed     bool // avoid retrying a failing mount every tick
	presenceRaw     bool
	presenceCount   int
	subscribers     []chan StorageEvent
	stateWriteErr   bool             // log the mirror-write failure only once
	formatJob       StorageFormatJob // async format task state
	autoFormatDone  bool             // one auto-format attempt per insertion
	cardOperation   string           // exclusive mount or health I/O running off m.mu
	lastHealthProbe time.Time
	lastWarning     string // most recent flush warning, retained across reconfiguration

	migration        StorageMigrationJob
	migrationCancel  chan struct{} // non-nil while a migration run is active
	migrationDone    chan struct{} // closed when the migration worker exits
	migrationRetryAt time.Time     // cooldown after a failed/exhausted run
	snapshotLease    *storageSnapshotLease
	snapshotSequence uint64

	stopOnce sync.Once
	stop     chan struct{}
}

// storageEMMCRootEnv overrides the eMMC data root. This is a test hook used
// by tests/board/test_storage_migration.sh to point the watermark migrator
// at a small loop-mounted filesystem instead of the real 3 GB /userdata;
// production deployments never set it.
const storageEMMCRootEnv = "AICO_STORAGE_EMMC_ROOT"

// NewStorageManager builds a manager with real platform operations.
func NewStorageManager(cfg StorageConfig, logger *Logger) *StorageManager {
	return NewStorageManagerWithStatePath(cfg, defaultStorageStatePath, logger)
}

// NewStorageManagerWithStatePath builds the hardware-owning storage manager
// and mirrors its state at statePath for read-only consumers such as Agent.
func NewStorageManagerWithStatePath(cfg StorageConfig, statePath string, logger *Logger) *StorageManager {
	emmcRoot := "/userdata"
	if override := strings.TrimSpace(os.Getenv(storageEMMCRootEnv)); override != "" {
		emmcRoot = override
		if logger != nil {
			logger.Warn("[storage] %s=%s overrides the eMMC data root (test hook; unset it for production use)",
				storageEMMCRootEnv, override)
		}
	}
	return newStorageManagerWithOps(cfg, &realStorageOps{device: cfg.DeviceOrDefault()}, statePath, emmcRoot, logger)
}

// NewStorageStateView builds a read-only view for processes that need storage
// routing but must not mount, format, eject, or migrate the card. Config Web is
// the sole hardware owner and publishes the state mirror consumed here.
func NewStorageStateView(cfg StorageConfig, statePath string, logger *Logger) *StorageManager {
	m := NewStorageManagerWithStatePath(cfg, statePath, logger)
	m.ops = nil
	m.mirrorOnly = true
	return m
}

func newStorageManagerWithOps(cfg StorageConfig, ops storageSysOps, statePath, emmcRoot string, logger *Logger) *StorageManager {
	return &StorageManager{
		cfg:           cfg,
		logger:        logger,
		ops:           ops,
		statePath:     statePath,
		emmcRoot:      emmcRoot,
		pollInterval:  2 * time.Second,
		lastEffective: StorageModeEMMCOnly,
		formatJob:     StorageFormatJob{Status: StorageFormatIdle},
		migration:     StorageMigrationJob{Status: StorageFormatIdle},
		stop:          make(chan struct{}),
	}
}

// Start launches the polling loop. Safe to call once.
func (m *StorageManager) Start() {
	if m == nil || m.mirrorOnly || m.ops == nil {
		return
	}
	go func() {
		m.tick()
		ticker := time.NewTicker(m.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
}

// Stop terminates the polling loop.
func (m *StorageManager) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() { close(m.stop) })
}

// Reconfigure applies persisted storage settings without replacing the
// hardware-owning manager. Replacing the manager would briefly leave two
// pollers able to mount or format the same card.
func (m *StorageManager) Reconfigure(cfg StorageConfig) error {
	if m == nil || m.mirrorOnly || m.ops == nil {
		return fmt.Errorf("storage hardware is owned by Config Web")
	}
	m.mu.Lock()
	if m.snapshotLease != nil {
		m.mu.Unlock()
		return fmt.Errorf("storage snapshot lease is active")
	}
	m.mu.Unlock()
	for {
		if err := m.cancelMigrationAndWait("storage reconfiguration"); err != nil {
			return err
		}
		m.mu.Lock()
		// A poll tick may have started a new migration between the completed
		// cancellation and this lock acquisition. Cancel that run as well so
		// the configuration cannot change underneath a migration worker.
		if m.migrationDone == nil {
			break
		}
		m.mu.Unlock()
	}
	if m.snapshotLease != nil {
		m.mu.Unlock()
		return fmt.Errorf("storage snapshot lease is active")
	}

	oldMountPoint := m.cfg.MountPointOrDefault()
	oldDevice := m.cfg.DeviceOrDefault()
	newMountPoint := cfg.MountPointOrDefault()
	newDevice := cfg.DeviceOrDefault()
	identityChanged := oldMountPoint != newMountPoint || oldDevice != newDevice
	if identityChanged && m.formatJob.Status == StorageFormatRunning {
		m.mu.Unlock()
		return fmt.Errorf("cannot change the storage device or mount point while a format job is running")
	}
	if identityChanged && m.cardOperation != "" {
		operation := m.cardOperation
		m.mu.Unlock()
		return fmt.Errorf("cannot change the storage device or mount point while %s is in progress", operation)
	}
	if identityChanged && m.card.Mounted {
		if err := m.unmountLocked(false, ""); err != nil && !errors.Is(err, ErrStorageFlush) {
			m.mu.Unlock()
			return fmt.Errorf("cannot apply storage config: %w", err)
		}
	}

	m.cfg = cfg
	if identityChanged {
		if configurable, ok := m.ops.(interface{ setDevice(string) }); ok {
			configurable.setDevice(newDevice)
		}
		m.card = StorageCardStatus{}
		m.ejected = false
		m.mountFailed = false
		m.presenceRaw = false
		m.presenceCount = 0
		m.autoFormatDone = false
		m.finishTransitionLocked()
	} else {
		m.writeStateFileLocked()
	}
	m.mu.Unlock()

	// Reconcile promptly instead of waiting for the next polling interval.
	m.tick()
	return nil
}

// deriveEffectiveMode is the single rule from which all boot and hot-plug
// behavior follows.
func deriveEffectiveMode(cardMounted bool) StorageMode {
	if !cardMounted {
		return StorageModeEMMCOnly
	}
	return StorageModeDual
}

// EffectiveMode returns the currently derived mode.
func (m *StorageManager) EffectiveMode() StorageMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshStateMirrorLocked()
	return deriveEffectiveMode(m.card.Mounted)
}

// Status returns an API-facing snapshot.
func (m *StorageManager) Status() StorageStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshStateMirrorLocked()
	return StorageStatus{
		EffectiveMode: deriveEffectiveMode(m.card.Mounted),
		Card:          m.card,
		MountPoint:    m.cfg.MountPointOrDefault(),
		FormatJob:     m.formatJob,
		Migration:     m.migration,
		LastWarning:   m.lastWarning,
	}
}

// SafeEject syncs and unmounts the card so it can be pulled. The card is not
// remounted until it is physically removed and reinserted.
func (m *StorageManager) SafeEject() error {
	if m == nil || m.mirrorOnly {
		return fmt.Errorf("storage hardware is owned by Config Web")
	}
	m.mu.Lock()
	if m.snapshotLease != nil {
		m.mu.Unlock()
		return fmt.Errorf("storage snapshot lease is active")
	}
	m.mu.Unlock()
	// Ejecting invalidates the migration target; stop the worker first.
	if err := m.cancelMigrationAndWait("eject"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.formatJob.Status == StorageFormatRunning {
		return fmt.Errorf("cannot eject while a format job is running")
	}
	if m.snapshotLease != nil {
		return fmt.Errorf("storage snapshot lease is active")
	}
	// A mount attempt in flight has not set m.card.Mounted yet, so reporting
	// "no mounted card" here would be wrong and the mount would land right
	// after the eject. Make the caller retry once the attempt settles.
	if m.cardOperation != "" {
		return fmt.Errorf("%s is in progress; retry shortly", m.cardOperation)
	}
	if !m.card.Mounted {
		return fmt.Errorf("no mounted card to eject")
	}
	err := m.unmountLocked(false, "")
	if err != nil && !errors.Is(err, ErrStorageFlush) {
		return fmt.Errorf("cannot eject: %w (close any shell or process using %s and retry)", err, m.cfg.MountPointOrDefault())
	}
	m.ejected = true
	m.finishTransitionLocked()
	if err != nil {
		return fmt.Errorf("card was detached, but pending writes may not have been saved: %w", err)
	}
	return nil
}

// StartFormat launches the asynchronous format job: fresh MBR with one
// partition plus a new filesystem. Erases the entire card, including any
// partitions not created by this device. confirm must equal
// StorageFormatConfirmToken. Progress is polled via Status().FormatJob.
// After a successful format the card is mounted and dual storage resumes.
func (m *StorageManager) StartFormat(fs, confirm string) error {
	if m == nil || m.mirrorOnly {
		return fmt.Errorf("storage hardware is owned by Config Web")
	}
	m.mu.Lock()
	if m.snapshotLease != nil {
		m.mu.Unlock()
		return fmt.Errorf("storage snapshot lease is active")
	}
	m.mu.Unlock()
	if confirm != StorageFormatConfirmToken {
		return fmt.Errorf("format not confirmed")
	}
	if fs == "" {
		fs = StorageFormatFAT32
	}
	if fs != StorageFormatFAT32 && fs != StorageFormatExt4 && fs != StorageFormatExFAT {
		return fmt.Errorf("unsupported filesystem %q (want %s, %s or %s)", fs, StorageFormatFAT32, StorageFormatExt4, StorageFormatExFAT)
	}
	// Formatting owns the card exclusively; stop a running migration first.
	if err := m.cancelMigrationAndWait("format"); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.formatJob.Status == StorageFormatRunning {
		return fmt.Errorf("a format job is already running")
	}
	if m.snapshotLease != nil {
		return fmt.Errorf("storage snapshot lease is active")
	}
	// A mount attempt holds the card with m.mu released, and m.card.Mounted is
	// still false until it lands — so without this the unmount guard below is
	// skipped and mkfs would run concurrently with mount. Mount attempts are
	// short and the caller can retry.
	if m.cardOperation != "" {
		return fmt.Errorf("%s is in progress; retry shortly", m.cardOperation)
	}
	if _, present := m.ops.CardDevice(); !present {
		return fmt.Errorf("no card present")
	}
	if m.card.Mounted {
		if err := m.unmountLocked(false, ""); err != nil && !errors.Is(err, ErrStorageFlush) {
			return fmt.Errorf("cannot format: %w (close any shell or process using %s and retry)", err, m.cfg.MountPointOrDefault())
		}
	}
	m.formatJob = StorageFormatJob{Status: StorageFormatRunning, FS: fs, StartedAt: time.Now()}
	m.writeStateFileLocked()
	go m.runFormatJob(fs)
	return nil
}

// runFormatJob executes the destructive format outside the manager lock so
// status polling stays responsive; the running-job flag keeps the poll loop
// and other operations away from the card meanwhile.
func (m *StorageManager) runFormatJob(fs string) {
	_, err := m.ops.FormatDisk(fs)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.formatJob.FinishedAt = time.Now()
		m.formatJob.Status = StorageFormatFailed
		m.formatJob.Error = err.Error()
		m.card.Reason = fmt.Sprintf("format failed: %v", err)
		m.logf("[storage] format (%s) failed: %v", fs, err)
		m.finishTransitionLocked()
		return
	}
	m.card.Reason = ""
	m.ejected = false
	m.mountFailed = false
	// Keep the job running until the post-format mount attempt has landed.
	// tryMountLocked releases m.mu while Prepare runs, so publishing success
	// first would let observers see a completed job with an unmounted card and
	// let callers tear down resources that the mount attempt is still using.
	m.tryMountLocked()
	m.formatJob.FinishedAt = time.Now()
	if !m.card.Mounted {
		reason := m.card.Reason
		if reason == "" {
			reason = "card unavailable after format"
			m.card.Reason = reason
		}
		m.formatJob.Status = StorageFormatFailed
		m.formatJob.Error = fmt.Sprintf("post-format mount failed: %s", reason)
		m.finishTransitionLocked()
		return
	}
	m.formatJob.Status = StorageFormatSuccess
	m.finishTransitionLocked()
}

// ----- eMMC → SD migration --------------------------------------------------

// maybeStartMigrationLocked launches the background migration when a card is
// mounted and eMMC free space fell below the start watermark. Caller holds m.mu.
func (m *StorageManager) maybeStartMigrationLocked() {
	if !m.card.Mounted ||
		m.card.FreeBytes < int64(m.cfg.MinCardFreeMBOrDefault())*1024*1024 ||
		m.snapshotLease != nil ||
		m.formatJob.Status == StorageFormatRunning ||
		m.migration.Status == StorageFormatRunning ||
		storageRestorePending(m.cfg.MountPointOrDefault()) {
		return
	}
	if !m.migrationRetryAt.IsZero() && time.Now().Before(m.migrationRetryAt) {
		return
	}
	startPct, stopPct := m.cfg.MigrateWatermarksOrDefault()
	freePct, err := m.emmcFreePct()
	if err != nil {
		m.warnf("[storage] migration trigger: cannot read eMMC space at %s: %v", m.emmcRoot, err)
		return
	}
	if freePct >= float64(startPct) {
		return
	}
	m.migration = StorageMigrationJob{Status: StorageFormatRunning, StartedAt: time.Now()}
	m.migrationCancel = make(chan struct{})
	m.migrationDone = make(chan struct{})
	m.writeStateFileLocked()
	m.logf("[storage] eMMC free space %.1f%% is below the %d%% watermark; migrating older data to SD until %d%%",
		freePct, startPct, stopPct)
	go m.runMigration(m.migrationCancel, m.migrationDone, stopPct)
}

type storageSnapshotIdentityOps interface {
	SnapshotIdentity(ctx context.Context, device, mountPoint string) (filesystemUUID, mountID string, err error)
}

type storageSnapshotLease struct {
	manager  *StorageManager
	snapshot StorageSnapshot
	sequence uint64
	once     sync.Once
}

func (l *storageSnapshotLease) Snapshot() StorageSnapshot {
	if l == nil {
		return StorageSnapshot{}
	}
	return l.snapshot
}

func (l *storageSnapshotLease) Validate(ctx context.Context) error {
	if l == nil || l.manager == nil {
		return fmt.Errorf("storage snapshot lease is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m := l.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshotLease != l || l.sequence != m.snapshotSequence {
		return fmt.Errorf("storage snapshot lease is no longer active")
	}
	if !m.card.Mounted || m.card.Device != l.snapshot.DevicePath ||
		m.cfg.MountPointOrDefault() != l.snapshot.MountPoint ||
		!m.ops.Healthy(l.snapshot.MountPoint) {
		return fmt.Errorf("SD card changed or became unavailable during snapshot")
	}
	identityOps, ok := m.ops.(storageSnapshotIdentityOps)
	if !ok {
		return fmt.Errorf("storage backend cannot verify snapshot identity")
	}
	uuid, mountID, err := identityOps.SnapshotIdentity(ctx, l.snapshot.DevicePath, l.snapshot.MountPoint)
	if err != nil {
		return fmt.Errorf("verify SD snapshot identity: %w", err)
	}
	if uuid != l.snapshot.FilesystemUUID || mountID != l.snapshot.MountID {
		return fmt.Errorf("SD card filesystem or mount changed during snapshot")
	}
	return nil
}

func (l *storageSnapshotLease) Release() {
	if l == nil || l.manager == nil {
		return
	}
	l.once.Do(func() {
		m := l.manager
		m.mu.Lock()
		if m.snapshotLease == l {
			m.snapshotLease = nil
			m.writeStateFileLocked()
		}
		m.mu.Unlock()
		// Reconcile immediately so a migration delayed by the lease need not wait
		// for the next polling interval. Run it synchronously: a detached
		// goroutine could still be writing the state file after the caller
		// (or a test's temporary directory) is gone.
		select {
		case <-m.stop:
			return
		default:
		}
		m.tick()
	})
}

// AcquireSnapshotLease stops any active migration and pins the current SD
// mount identity until Release. It never mounts a missing card implicitly.
func (m *StorageManager) AcquireSnapshotLease(ctx context.Context) (StorageSnapshotLease, error) {
	if m == nil || m.mirrorOnly || m.ops == nil {
		return nil, fmt.Errorf("storage hardware is owned by Config Web")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := m.cancelMigrationAndWait("snapshot lease"); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.migrationDone == nil {
			break
		}
		m.mu.Unlock()
	}
	defer m.mu.Unlock()
	if m.snapshotLease != nil {
		return nil, fmt.Errorf("storage snapshot lease is already active")
	}
	if m.formatJob.Status == StorageFormatRunning {
		return nil, fmt.Errorf("cannot snapshot while a format job is running")
	}
	if m.cardOperation != "" {
		return nil, fmt.Errorf("cannot snapshot while %s is in progress", m.cardOperation)
	}
	if !m.card.Present || !m.card.Mounted || !m.ops.Healthy(m.cfg.MountPointOrDefault()) {
		return nil, fmt.Errorf("no readable and writable SD card is mounted")
	}
	identityOps, ok := m.ops.(storageSnapshotIdentityOps)
	if !ok {
		return nil, fmt.Errorf("storage backend cannot identify snapshots")
	}
	mountPoint := m.cfg.MountPointOrDefault()
	uuid, mountID, err := identityOps.SnapshotIdentity(ctx, m.card.Device, mountPoint)
	if err != nil {
		return nil, fmt.Errorf("identify SD snapshot: %w", err)
	}
	if strings.TrimSpace(uuid) == "" || strings.TrimSpace(mountID) == "" {
		return nil, fmt.Errorf("SD snapshot UUID or mount ID is unavailable")
	}
	m.snapshotSequence++
	lease := &storageSnapshotLease{
		manager: m,
		snapshot: StorageSnapshot{
			DevicePath: m.card.Device, MountPoint: mountPoint,
			FilesystemUUID: uuid, MountID: mountID,
		},
		sequence: m.snapshotSequence,
	}
	m.snapshotLease = lease
	m.writeStateFileLocked()
	return lease, nil
}

func (m *StorageManager) emmcFreePct() (float64, error) {
	free, total, err := m.ops.SpaceInfo(m.emmcRoot)
	if err != nil {
		return 0, err
	}
	if total <= 0 {
		return 0, fmt.Errorf("filesystem total size reported as %d", total)
	}
	return float64(free) / float64(total) * 100, nil
}

// requestMigrationCancelLocked signals the migration worker to stop after the
// current file. Caller holds m.mu.
func (m *StorageManager) requestMigrationCancelLocked(reason string) {
	if m.migrationCancel == nil {
		return
	}
	m.logf("[storage] canceling migration: %s", reason)
	close(m.migrationCancel)
	m.migrationCancel = nil
}

// cancelMigrationAndWait stops a running migration and waits for the worker
// to exit, so callers (format, eject) get exclusive card access. Must be
// called without m.mu held.
func (m *StorageManager) cancelMigrationAndWait(reason string) error {
	m.mu.Lock()
	m.requestMigrationCancelLocked(reason)
	done := m.migrationDone
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-time.After(15 * time.Second):
		return fmt.Errorf("migration is still stopping; retry %s shortly", reason)
	}
}

// runMigration moves the oldest migratable files from eMMC to the SD card
// until the stop watermark is reached, no eligible files remain, or the run
// is canceled. Every failure path logs with full context.
func (m *StorageManager) runMigration(cancel <-chan struct{}, done chan<- struct{}, stopPct int) {
	defer close(done)

	movedFiles := 0
	var movedBytes int64
	finish := func(status, detail, errText string, cooldown bool) {
		m.mu.Lock()
		m.migration.Status = status
		m.migration.Detail = detail
		m.migration.Error = errText
		m.migration.MovedFiles = movedFiles
		m.migration.MovedBytes = movedBytes
		m.migration.FinishedAt = time.Now()
		m.migrationCancel = nil
		m.migrationDone = nil
		if cooldown {
			m.migrationRetryAt = time.Now().Add(migrationRetryCooldown)
		}
		m.writeStateFileLocked()
		m.mu.Unlock()
		if errText != "" {
			m.warnf("[storage] migration finished: %s (%s); moved %d files / %d bytes", detail, errText, movedFiles, movedBytes)
		} else {
			m.logf("[storage] migration finished: %s; moved %d files / %d bytes", detail, movedFiles, movedBytes)
		}
	}

	srcDir := m.emmcClassDir(StorageClassAudio)
	m.mu.Lock()
	dstDir := m.sdClassDir(m.cfg.MountPointOrDefault(), StorageClassAudio)
	m.mu.Unlock()
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		finish(StorageFormatFailed, "cannot create SD target directory "+dstDir, err.Error(), true)
		return
	}
	m.cleanStalePartials(dstDir)

	// Snapshot the candidates once, oldest first. Files created after this
	// point are by definition the newest and are never migration targets in
	// this run.
	candidates, err := m.listMigratable(srcDir)
	if err != nil {
		finish(StorageFormatFailed, "cannot list "+srcDir, err.Error(), true)
		return
	}
	if len(candidates) == 0 {
		finish(StorageFormatSuccess,
			"no migratable files (only system data or files still in use occupy eMMC); free space remains below the watermark",
			"", true)
		return
	}

	failedFiles := 0
	for _, candidate := range candidates {
		select {
		case <-cancel:
			finish(StorageFormatSuccess, "canceled", "", false)
			return
		default:
		}

		freePct, err := m.emmcFreePct()
		if err != nil {
			finish(StorageFormatFailed, "cannot read eMMC space during migration", err.Error(), true)
			return
		}
		if freePct >= float64(stopPct) {
			finish(StorageFormatSuccess, fmt.Sprintf("reached the %d%% stop watermark", stopPct), "", false)
			return
		}
		m.mu.Lock()
		mounted := m.card.Mounted
		m.mu.Unlock()
		if !mounted {
			finish(StorageFormatFailed, "card no longer mounted", "migration aborted", true)
			return
		}

		size, err := m.migrateOneFile(filepath.Join(srcDir, candidate), filepath.Join(dstDir, candidate))
		if err != nil {
			if storageCapacityError(err) {
				finish(StorageFormatFailed, "SD card has insufficient free space", err.Error(), true)
				return
			}
			failedFiles++
			m.warnf("[storage] migrate %s -> %s failed: %v", filepath.Join(srcDir, candidate), dstDir, err)
			continue // a single bad file must not stall the whole run
		}
		movedFiles++
		movedBytes += size
		m.logf("[storage] migrated %s (%d bytes) to SD", candidate, size)
		m.mu.Lock()
		m.migration.MovedFiles = movedFiles
		m.migration.MovedBytes = movedBytes
		m.writeStateFileLocked()
		m.mu.Unlock()

		// Throttle so recordings and system IO are not starved.
		select {
		case <-cancel:
			finish(StorageFormatSuccess, "canceled", "", false)
			return
		case <-time.After(migrationFilePause):
		}
	}

	// Candidates exhausted: re-check where that left us.
	detail := "all migratable files moved; free space remains below the stop watermark"
	cooldown := true
	if freePct, err := m.emmcFreePct(); err == nil && freePct >= float64(stopPct) {
		detail = fmt.Sprintf("reached the %d%% stop watermark", stopPct)
		cooldown = false
	}
	errText := ""
	status := StorageFormatSuccess
	if failedFiles > 0 {
		status = StorageFormatFailed
		errText = fmt.Sprintf("%d files failed to migrate", failedFiles)
		cooldown = true
	}
	finish(status, detail, errText, cooldown)
}

// listMigratable returns basenames of regular files in dir older than
// migrationMinFileAge, oldest first.
func (m *StorageManager) listMigratable(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type fileAge struct {
		name    string
		modTime time.Time
	}
	cutoff := time.Now().Add(-migrationMinFileAge)
	var files []fileAge
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			m.warnf("[storage] migration: stat %s: %v", filepath.Join(dir, entry.Name()), err)
			continue
		}
		if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		files = append(files, fileAge{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
	}
	return names, nil
}

// migrateOneFile is the idempotent per-file transaction:
// copy to a .aico-partial temp name → fsync → verify size → rename →
// fsync destination dir → delete source. A crash at any point leaves either both copies (resolved by
// the destination-exists check on the next run and by eMMC-first ReadRoots)
// or the migrated file only.
func (m *StorageManager) migrateOneFile(src, dst string) (int64, error) {
	info, err := os.Stat(src)
	if err != nil {
		return 0, fmt.Errorf("stat source: %w", err)
	}
	if dstInfo, err := os.Stat(dst); err == nil {
		if dstInfo.Size() == info.Size() {
			// Left over from an interrupted run: the copy completed but the
			// source was not deleted. The interrupted run may not have made
			// its commit rename durable, so sync the directory before
			// finishing the transaction.
			if err := syncDir(filepath.Dir(dst)); err != nil {
				return 0, fmt.Errorf("sync destination directory: %w", err)
			}
			if err := os.Remove(src); err != nil {
				return 0, fmt.Errorf("destination already migrated but removing source failed: %w", err)
			}
			m.logf("[storage] migration: %s already on SD (same size); removed eMMC copy", filepath.Base(src))
			return info.Size(), nil
		}
		return 0, fmt.Errorf("destination exists with different size (src %d, dst %d); refusing to overwrite", info.Size(), dstInfo.Size())
	}

	free, _, err := m.ops.SpaceInfo(filepath.Dir(dst))
	if err != nil {
		return 0, fmt.Errorf("check SD free space: %w", err)
	}
	m.mu.Lock()
	reserve := int64(m.cfg.MinCardFreeMBOrDefault()) * 1024 * 1024
	m.mu.Unlock()
	if free < reserve || info.Size() > free-reserve {
		return 0, fmt.Errorf("SD has %d bytes free; copying %d bytes would cross the %d-byte reserve: %w", free, info.Size(), reserve, syscall.ENOSPC)
	}

	partial := dst + migrationPartialSuffix
	if err := copyFileSync(src, partial); err != nil {
		if rmErr := os.Remove(partial); rmErr != nil && !os.IsNotExist(rmErr) {
			m.warnf("[storage] migration: cleanup of %s failed: %v", partial, rmErr)
		}
		return 0, err
	}
	partialInfo, err := os.Stat(partial)
	if err != nil {
		return 0, fmt.Errorf("stat copied file: %w", err)
	}
	if partialInfo.Size() != info.Size() {
		if rmErr := os.Remove(partial); rmErr != nil {
			m.warnf("[storage] migration: cleanup of %s failed: %v", partial, rmErr)
		}
		return 0, fmt.Errorf("size mismatch after copy (src %d, copy %d)", info.Size(), partialInfo.Size())
	}
	// The rename is the commit point: only then does the file appear under
	// its real name on the SD card.
	if err := os.Rename(partial, dst); err != nil {
		if rmErr := os.Remove(partial); rmErr != nil {
			m.warnf("[storage] migration: cleanup of %s failed: %v", partial, rmErr)
		}
		return 0, fmt.Errorf("commit rename: %w", err)
	}
	// The rename is durable only once the destination directory's metadata
	// reaches the card. Source and destination live on different filesystems
	// with independently ordered journals, so without this the eMMC unlink
	// below can survive a power cut that the SD-side rename does not —
	// losing the file. On failure keep the source; both copies coexisting is
	// the safe state the next run resolves.
	if err := syncDir(filepath.Dir(dst)); err != nil {
		return 0, fmt.Errorf("sync destination directory: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return 0, fmt.Errorf("copied to SD but removing eMMC source failed: %w", err)
	}
	return info.Size(), nil
}

// cleanStalePartials removes leftover in-flight copies from interrupted runs.
func (m *StorageManager) cleanStalePartials(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			m.warnf("[storage] migration: cannot scan %s for stale partial files: %v", dir, err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), migrationPartialSuffix) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := os.Remove(path); err != nil {
			m.warnf("[storage] migration: cannot remove stale partial %s: %v", path, err)
		} else {
			m.logf("[storage] migration: removed stale partial %s", path)
		}
	}
}

// syncDir fsyncs a directory so renames and creates inside it are durable.
// FAT32, exFAT, and ext4 (the filesystems StartFormat offers) all support
// fsync on a directory handle.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create copy: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy data: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return fmt.Errorf("sync copy: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close copy: %w", err)
	}
	return nil
}

// Subscribe returns a channel that receives an event whenever the effective
// mode changes. Events are dropped rather than blocking the manager.
func (m *StorageManager) Subscribe() <-chan StorageEvent {
	ch := make(chan StorageEvent, 4)
	m.mu.Lock()
	m.subscribers = append(m.subscribers, ch)
	m.mu.Unlock()
	return ch
}

// ResolveDir returns the directory new data of the given class should be
// written to, creating it if needed. New data always lands on eMMC (fast,
// reliable, unaffected by card removal); the background migrator moves
// older files to the SD card when eMMC runs low (see runMigration).
func (m *StorageManager) ResolveDir(class StorageDataClass) (string, error) {
	dir := m.emmcClassDir(class)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// ReadRoots returns every root that may hold data of the given class, eMMC
// first. Callers merge their views across all roots; during the migration
// crash window a file may briefly exist on both stores, and the eMMC copy is
// authoritative until the source is deleted, so first-hit readers resolve
// duplicates correctly.
func (m *StorageManager) ReadRoots(class StorageDataClass) []string {
	m.mu.Lock()
	m.refreshStateMirrorLocked()
	mounted := m.card.Mounted
	mountPoint := m.cfg.MountPointOrDefault()
	m.mu.Unlock()

	roots := []string{m.emmcClassDir(class)}
	if mounted {
		roots = append(roots, m.sdClassDir(mountPoint, class))
	}
	return roots
}

// CleanupRoots returns the roots whose contents are subject to the
// audio-archive retention limits. With a mounted card, eMMC space is managed
// by watermark migration, so the limits apply to the SD side only (the final
// eviction tier). Without a card the limits keep the eMMC store bounded as
// before — note that this can mass-evict files that migration had allowed to
// accumulate, which the archive cleanup logs.
func (m *StorageManager) CleanupRoots(class StorageDataClass) []string {
	m.mu.Lock()
	m.refreshStateMirrorLocked()
	mounted := m.card.Mounted
	mountPoint := m.cfg.MountPointOrDefault()
	m.mu.Unlock()

	if mounted {
		if storageRestorePending(mountPoint) {
			return nil
		}
		return []string{m.sdClassDir(mountPoint, class)}
	}
	return []string{m.emmcClassDir(class)}
}

func (m *StorageManager) sdClassDir(mountPoint string, class StorageDataClass) string {
	return filepath.Join(mountPoint, storageSDSubdir, string(class))
}

// A restarted manager may mount a card before restore recovery runs. Do not
// migrate or evict files while a transaction is pending; its checksummed
// target and staging paths must remain unchanged.
func storageRestorePending(mountPoint string) bool {
	entries, err := os.ReadDir(filepath.Join(mountPoint, ".aico-restore"))
	return (err != nil && !os.IsNotExist(err)) || len(entries) != 0
}

func (m *StorageManager) emmcClassDir(class StorageDataClass) string {
	switch class {
	case StorageClassAudio:
		return filepath.Join(m.emmcRoot, "audio")
	case StorageClassLogs:
		return filepath.Join(m.emmcRoot, "agent", "log")
	default:
		return filepath.Join(m.emmcRoot, string(class))
	}
}

func (m *StorageManager) refreshStateMirrorLocked() {
	if m == nil || !m.mirrorOnly {
		return
	}
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		m.card = StorageCardStatus{}
		m.lastWarning = ""
		m.formatJob = StorageFormatJob{Status: StorageFormatIdle}
		m.migration = StorageMigrationJob{Status: StorageFormatIdle}
		return
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			values[key] = value
		}
	}
	parseInt64 := func(key string) int64 {
		value, parseErr := strconv.ParseInt(strings.TrimSpace(values[key]), 10, 64)
		if parseErr != nil || value < 0 {
			return 0
		}
		return value
	}
	parseInt := func(key string) int {
		value, parseErr := strconv.Atoi(strings.TrimSpace(values[key]))
		if parseErr != nil || value < 0 {
			return 0
		}
		return value
	}
	m.card = StorageCardStatus{
		Present:    values["SD_PRESENT"] == "1",
		Mounted:    values["SD_MOUNTED"] == "1",
		Device:     values["SD_DEVICE"],
		TotalBytes: parseInt64("SD_TOTAL_BYTES"),
		FreeBytes:  parseInt64("SD_FREE_BYTES"),
		Reason:     values["REASON"],
	}
	m.lastWarning = values["LAST_WARNING"]
	if mountPoint := strings.TrimSpace(values["SD_MOUNTPOINT"]); mountPoint != "" {
		m.cfg.MountPoint = mountPoint
	}
	formatStatus := values["FORMAT_STATUS"]
	if formatStatus == "" {
		formatStatus = StorageFormatIdle
	}
	m.formatJob = StorageFormatJob{
		Status: formatStatus,
		FS:     values["FORMAT_FS"],
		Auto:   values["FORMAT_AUTO"] == "1",
		Error:  values["FORMAT_ERROR"],
	}
	migrationStatus := values["MIGRATE_STATUS"]
	if migrationStatus == "" {
		migrationStatus = StorageFormatIdle
	}
	m.migration = StorageMigrationJob{
		Status:     migrationStatus,
		Detail:     values["MIGRATE_DETAIL"],
		Error:      values["MIGRATE_ERROR"],
		MovedFiles: parseInt("MIGRATE_MOVED_FILES"),
		MovedBytes: parseInt64("MIGRATE_MOVED_BYTES"),
	}
}

// tick is one poll cycle: debounce presence, then reconcile mount state.
func (m *StorageManager) tick() {
	if m == nil || m.mirrorOnly || m.ops == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	dev, present := m.ops.CardDevice()

	// A running format owns the card exclusively; skip reconciliation.
	if m.formatJob.Status == StorageFormatRunning {
		return
	}
	// Mount or health I/O is in flight with m.mu released. Its result will
	// update state; a second tick must not touch the same device meanwhile.
	if m.cardOperation != "" {
		return
	}

	// Debounce: require two consecutive observations before accepting a
	// presence change (contact bounce on physical slots is common).
	if present != m.card.Present {
		if present == m.presenceRaw {
			m.presenceCount++
		} else {
			m.presenceRaw = present
			m.presenceCount = 1
		}
		if m.presenceCount < 2 {
			return
		}
	} else {
		m.presenceRaw = present
		m.presenceCount = 0
	}

	switch {
	case present && !m.card.Present:
		m.card.Present = true
		m.card.Device = dev
		m.card.Reason = ""
		m.ejected = false
		m.mountFailed = false
		m.autoFormatDone = false
		m.lastHealthProbe = time.Time{}
		m.tryMountLocked()
	case !present && m.card.Present:
		m.requestMigrationCancelLocked("card removed")
		if m.card.Mounted {
			m.unmountLocked(true, "card removed")
		}
		m.card = StorageCardStatus{}
		m.ejected = false
		m.mountFailed = false
		m.autoFormatDone = false
	case present && m.card.Mounted:
		mountPoint := m.cfg.MountPointOrDefault()
		probeDue := time.Since(m.lastHealthProbe) >= 30*time.Second
		m.cardOperation = "a card health check"
		m.mu.Unlock()
		healthy := m.ops.Healthy(mountPoint)
		var probeErr error
		if healthy && probeDue {
			probeErr = m.ops.VerifyWritable(mountPoint)
			healthy = probeErr == nil || storageCapacityError(probeErr)
		}
		free, total, spaceErr := m.ops.SpaceInfo(mountPoint)
		m.mu.Lock()
		m.cardOperation = ""
		if probeDue {
			m.lastHealthProbe = time.Now()
		}
		if !healthy {
			m.requestMigrationCancelLocked("card unresponsive")
			reason := "card unresponsive or read-only"
			if probeErr != nil {
				reason = fmt.Sprintf("card write/sync probe failed: %v", probeErr)
			}
			m.unmountLocked(true, reason)
			m.mountFailed = true
		} else {
			if spaceErr == nil {
				m.card.FreeBytes, m.card.TotalBytes = free, total
			}
			if storageCapacityError(probeErr) {
				m.card.Reason = fmt.Sprintf("card capacity exhausted: %v", probeErr)
				m.requestMigrationCancelLocked("card capacity exhausted")
				m.migrationRetryAt = time.Now().Add(migrationRetryCooldown)
			} else if probeDue && strings.HasPrefix(m.card.Reason, "card capacity exhausted:") {
				m.card.Reason = ""
			}
		}
	case present && !m.card.Mounted:
		// Card sitting idle: mount unless safe-ejected or known bad.
		if !m.ejected && !m.mountFailed {
			m.tryMountLocked()
		}
	}
	m.maybeStartMigrationLocked()
	m.finishTransitionLocked()
}

// tryMountLocked runs the usability gate and mounts. Caller holds m.mu.
//
// ops.Prepare shells out to fsck and mount, so running it
// under m.mu would freeze Status/EffectiveMode/ReadRoots/CleanupRoots for the
// duration. It is instead released across the Prepare call — guarded by the
// m.cardOperation guard (checked in tick) so no second tick launches a concurrent
// Prepare — and re-acquired to land the result, following the same
// lock-release pattern as runFormatJob and runMigration.
func (m *StorageManager) tryMountLocked() {
	dev, present := m.ops.CardDevice()
	if !present {
		return
	}
	mountPoint := m.cfg.MountPointOrDefault()
	m.card.Present = true
	m.card.Device = dev

	m.cardOperation = "a mount attempt"
	m.mu.Unlock()
	err := m.ops.Prepare(dev, mountPoint)
	blank := err != nil && !errors.Is(err, errStorageIO) && !errors.Is(err, syscall.EIO) && !errors.Is(err, context.DeadlineExceeded) && m.ops.CardIsBlank()
	m.mu.Lock()
	m.cardOperation = ""

	// The card may have been ejected or removed while Prepare ran with the
	// lock released; the removal path already reset m.card, so discard this
	// stale result rather than resurrecting a gone card.
	if m.ejected || !m.card.Present || m.card.Device != dev {
		if err == nil {
			// A mount succeeded for a card no longer wanted here; detach it so
			// it does not linger. Best effort — the card is likely already gone.
			_ = m.ops.Unmount(mountPoint, true)
		}
		m.logf("[storage] discarding mount result for %s: card state changed during mount", dev)
		return
	}

	if err != nil {
		m.card.Mounted = false
		m.card.Reason = err.Error()
		m.mountFailed = true
		// A truly blank card (no partitions, no data signature of any kind)
		// is auto-formatted FAT32 and mounted. Cards with unreadable but
		// recognizable content are never touched — the user decides in the
		// portal. One attempt per insertion.
		if !m.autoFormatDone && m.formatJob.Status != StorageFormatRunning && blank {
			m.autoFormatDone = true
			m.card.Reason = "blank card detected; formatting as FAT32"
			m.formatJob = StorageFormatJob{Status: StorageFormatRunning, FS: StorageFormatFAT32, Auto: true, StartedAt: time.Now()}
			m.writeStateFileLocked()
			m.logf("[storage] card %s is blank; auto-formatting as FAT32", dev)
			go m.runFormatJob(StorageFormatFAT32)
			return
		}
		m.warnf("[storage] card %s failed the usability gate: %v", dev, err)
		return
	}
	m.card.Mounted = true
	m.card.Reason = ""
	m.mountFailed = false
	m.refreshSpaceLocked()
	free := int64(m.cfg.MinCardFreeMBOrDefault()) * 1024 * 1024
	if m.card.FreeBytes < free {
		reason := fmt.Sprintf("card has %d bytes free, below the %d MB minimum", m.card.FreeBytes, m.cfg.MinCardFreeMBOrDefault())
		// If the unmount fails the card simply stays mounted (and usable);
		// only the failed unmount keeps it from being rejected.
		_ = m.unmountLocked(false, reason)
		m.mountFailed = true
		m.logf("[storage] %s", reason)
		return
	}
	m.logf("[storage] mounted %s at %s (%d bytes free)", dev, mountPoint, m.card.FreeBytes)
}

// unmountLocked detaches the card. Caller holds m.mu.
//
// On a non-lazy unmount failure the card is STILL mounted (e.g. a shell has
// its cwd on the mount point), so the in-memory state keeps saying mounted
// and an error is returned — marking it unmounted here would desynchronize
// state from reality and send the poll loop into mount/EBUSY churn. Lazy
// unmounts are used when the card is physically gone; their failures cannot
// leave a working mount behind, so state is cleared regardless. ErrStorageFlush
// reports successful detachment after a failed flush; clear state and let the
// caller decide whether that warning permits its operation.
func (m *StorageManager) unmountLocked(lazy bool, reason string) error {
	mountPoint := m.cfg.MountPointOrDefault()
	var flushWarning error
	if err := m.ops.Unmount(mountPoint, lazy); err != nil {
		if !lazy && errors.Is(err, ErrStorageFlush) {
			flushWarning = err
			m.lastWarning = fmt.Sprintf("Card was detached, but pending writes may not have been saved: %v", err)
			if reason == "" {
				reason = err.Error()
			}
			m.warnf("[storage] card detached after flush failure: %v", err)
		} else if !lazy {
			m.card.Reason = fmt.Sprintf("unmount failed: %v", err)
			m.warnf("[storage] unmount %s failed; card stays mounted: %v", mountPoint, err)
			return err
		} else {
			m.logf("[storage] lazy unmount %s: %v", mountPoint, err)
		}
	}
	m.card.Mounted = false
	m.card.TotalBytes = 0
	m.card.FreeBytes = 0
	if reason != "" {
		m.card.Reason = reason
		m.logf("[storage] card detached: %s", reason)
	}
	return flushWarning
}

func (m *StorageManager) refreshSpaceLocked() {
	free, total, err := m.ops.SpaceInfo(m.cfg.MountPointOrDefault())
	if err == nil {
		m.card.FreeBytes = free
		m.card.TotalBytes = total
	}
}

// finishTransitionLocked re-derives the effective mode and, on change,
// notifies subscribers and refreshes the state mirror. Caller holds m.mu.
func (m *StorageManager) finishTransitionLocked() {
	effective := deriveEffectiveMode(m.card.Mounted)
	if effective == m.lastEffective {
		m.writeStateFileLocked()
		return
	}
	m.lastEffective = effective
	m.logf("[storage] effective mode -> %d (card mounted %t)", effective, m.card.Mounted)
	for _, ch := range m.subscribers {
		select {
		case ch <- StorageEvent{EffectiveMode: effective}:
		default:
		}
	}
	m.writeStateFileLocked()
}

// writeStateFileLocked mirrors state for external processes. Best effort:
// on hosts without the runtime dir it logs once and stays silent afterwards.
// Caller holds m.mu.
func (m *StorageManager) writeStateFileLocked() {
	if m.statePath == "" {
		return
	}
	var b strings.Builder
	writeKV := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }
	bool01 := func(v bool) string {
		if v {
			return "1"
		}
		return "0"
	}
	writeKV("SD_PRESENT", bool01(m.card.Present))
	writeKV("SD_MOUNTED", bool01(m.card.Mounted))
	writeKV("SD_DEVICE", m.card.Device)
	writeKV("SD_MOUNTPOINT", m.cfg.MountPointOrDefault())
	writeKV("EFFECTIVE_MODE", fmt.Sprintf("%d", deriveEffectiveMode(m.card.Mounted)))
	writeKV("SD_TOTAL_BYTES", fmt.Sprintf("%d", m.card.TotalBytes))
	writeKV("SD_FREE_BYTES", fmt.Sprintf("%d", m.card.FreeBytes))
	writeKV("REASON", strings.ReplaceAll(m.card.Reason, "\n", " "))
	writeKV("LAST_WARNING", strings.ReplaceAll(m.lastWarning, "\n", " "))
	writeKV("FORMAT_STATUS", m.formatJob.Status)
	writeKV("FORMAT_FS", m.formatJob.FS)
	writeKV("FORMAT_AUTO", bool01(m.formatJob.Auto))
	writeKV("FORMAT_ERROR", strings.ReplaceAll(m.formatJob.Error, "\n", " "))
	writeKV("MIGRATE_STATUS", m.migration.Status)
	writeKV("MIGRATE_DETAIL", strings.ReplaceAll(m.migration.Detail, "\n", " "))
	writeKV("MIGRATE_ERROR", strings.ReplaceAll(m.migration.Error, "\n", " "))
	writeKV("MIGRATE_MOVED_FILES", fmt.Sprintf("%d", m.migration.MovedFiles))
	writeKV("MIGRATE_MOVED_BYTES", fmt.Sprintf("%d", m.migration.MovedBytes))
	writeKV("SNAPSHOT_LEASE", bool01(m.snapshotLease != nil))

	tmp := m.statePath + ".tmp"
	err := os.MkdirAll(filepath.Dir(m.statePath), 0o755)
	if err == nil {
		if err = os.WriteFile(tmp, []byte(b.String()), 0o644); err == nil {
			err = os.Rename(tmp, m.statePath)
		}
	}
	if err != nil && !m.stateWriteErr {
		m.stateWriteErr = true
		m.logf("[storage] cannot mirror state to %s (logged once): %v", m.statePath, err)
	}
}

func (m *StorageManager) logf(format string, args ...interface{}) {
	if m.logger != nil {
		m.logger.Info(format, args...)
	} else {
		logging.Infof("config_web", "storage", format, args...)
	}
}

func (m *StorageManager) warnf(format string, args ...interface{}) {
	if m.logger != nil {
		m.logger.Warn(format, args...)
	} else {
		logging.Warnf("config_web", "storage", format, args...)
	}
}

// realStorageOps implements storageSysOps against the device.
type realStorageOps struct {
	mu     sync.RWMutex
	device string // "auto" or an explicit block device base name
}

func (o *realStorageOps) currentDevice() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.device == "auto" || o.device == "" {
		return discoverSDCard("/sys/block")
	}
	return o.device
}

// MMC numbering differs between boards. The card type, unlike the removable
// flag, reliably separates an SD card from eMMC and SDIO Wi-Fi. Fail closed if
// more than one SD card is present; an explicit device is then required.
func discoverSDCard(sysBlock string) string {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return ""
	}
	device := ""
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "mmcblk") {
			continue
		}
		cardType, err := os.ReadFile(filepath.Join(sysBlock, entry.Name(), "device", "type"))
		if err != nil || strings.TrimSpace(string(cardType)) != "SD" {
			continue
		}
		if device != "" {
			return ""
		}
		device = entry.Name()
	}
	return device
}

func (o *realStorageOps) setDevice(device string) {
	o.mu.Lock()
	o.device = device
	o.mu.Unlock()
}

// CardDevice checks /sys/block for the controller and prefers the first
// partition; cards formatted without a partition table use the whole device.
func (o *realStorageOps) CardDevice() (string, bool) {
	device := o.currentDevice()
	if device == "" {
		return "", false
	}
	sysPath := filepath.Join("/sys/block", device)
	if _, err := os.Stat(sysPath); err != nil {
		return "", false
	}
	part := device + "p1"
	if _, err := os.Stat(filepath.Join(sysPath, part)); err == nil {
		return "/dev/" + part, true
	}
	return "/dev/" + device, true
}

// CardIsBlank reports whether the card carries no recognizable content.
// The check is deliberately conservative: any partition, any signature blkid
// recognizes (filesystem, RAID, crypto, partition table), any MBR/GPT magic,
// or any failure to verify means NOT blank — we only ever auto-format a card
// we could positively identify as empty.
func (o *realStorageOps) CardIsBlank() bool {
	device := o.currentDevice()
	if device == "" {
		return false
	}
	sysPath := filepath.Join("/sys/block", device)
	if _, err := os.Stat(filepath.Join(sysPath, device+"p1")); err == nil {
		return false // kernel sees a partition table
	}
	dev := "/dev/" + device
	if !storageSignatureAbsent(dev) {
		return false // a signature exists, or probing could not verify absence
	}
	// Double-check raw contents before declaring the card blank.
	f, err := os.Open(dev)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 1024)
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return bytes.Equal(buf, make([]byte, len(buf)))
}

var errStorageIO = errors.New("card I/O failure")

// ErrStorageFlush means the card was detached, but pending writes may not have
// been saved. Callers should report a completed eject with a data-loss warning.
var ErrStorageFlush = errors.New("card detached after flush failure")

// Kill checker subprocesses too: fsck starts a filesystem-specific child.
func runStorageCommand(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("%s timed out after %s: %w", name, timeout, ctx.Err())
	}
	return out, err
}

func storageIOError(out []byte, err error) bool {
	message := strings.ToLower(string(out))
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.EIO) ||
		strings.Contains(message, "input/output error") || strings.Contains(message, "i/o error")
}

func storageCapacityError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// Resolve the filesystem-specific checker directly so a missing helper is
// distinguishable from a checker that ran and reported filesystem errors.
func checkStorageFilesystem(dev string) error {
	fsType, _ := probeBlkid(dev)
	if fsType == "" {
		return nil // mount/probe remains the gate when the type is unknown
	}
	out, err := runStorageCommand(60*time.Second, "fsck."+fsType, "-p", dev)
	if storageIOError(out, err) {
		return fmt.Errorf("fsck %s: %w: %v: %s", dev, errStorageIO, err, strings.TrimSpace(string(out)))
	}
	if err == nil || errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		// FAT's checker uses 2 for usage errors and 3 for internal errors.
		// With -p, 1 also covers changes successfully written to the card.
		maxStatus := 3 // fsck bitmask: corrected errors / reboot recommended
		if fsType == "vfat" || fsType == "fat" || fsType == "msdos" {
			maxStatus = 1
		}
		if status := exit.ExitCode(); status >= 0 && status <= maxStatus {
			return nil
		}
	}
	return fmt.Errorf("fsck %s: %w: %s", dev, err, strings.TrimSpace(string(out)))
}

func storageWriteProbe(mountPoint string) error {
	f, err := os.CreateTemp(mountPoint, ".aico-write-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	data := []byte("aico-storage-probe\n")
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write probe: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync probe: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	readback := make([]byte, len(data))
	if _, err := io.ReadFull(f, readback); err != nil {
		return fmt.Errorf("read probe: %w", err)
	}
	if !bytes.Equal(readback, data) {
		return fmt.Errorf("probe readback mismatch: %w", errStorageIO)
	}
	return f.Close()
}

// Prepare checks the filesystem, mounts read-write, and verifies a synced
// probe write and readback. Never formats.
func (o *realStorageOps) Prepare(dev, mountPoint string) error {
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	// Adopt an existing mount (e.g. left behind by a previous agent run that
	// the watchdog restarted) instead of mounting on top of it — a second
	// mount of the same device fails with EBUSY. The probe write below still
	// validates the adopted mount.
	if o.IsMounted(mountPoint) {
		if source, _ := storageMount(mountPoint); !sameStorageDevice(source, dev) {
			return fmt.Errorf("mount point %s belongs to %s, expected %s", mountPoint, source, dev)
		}
		if err := storageWriteProbe(mountPoint); err != nil {
			return fmt.Errorf("existing mount at %s is not writable: %w", mountPoint, err)
		}
		return nil
	}
	if err := checkStorageFilesystem(dev); err != nil {
		return err
	}
	// ext4 gets a tighter journal commit; other filesystems reject those
	// options, so retry with the portable set.
	if out, err := runStorageCommand(15*time.Second, "mount", "-o", "noatime,errors=remount-ro,commit=2", dev, mountPoint); err != nil {
		if storageIOError(out, err) {
			return fmt.Errorf("mount %s: %w: %v: %s", dev, errStorageIO, err, strings.TrimSpace(string(out)))
		}
		if out2, err2 := runStorageCommand(15*time.Second, "mount", "-o", "noatime", dev, mountPoint); err2 != nil {
			if storageIOError(out2, err2) {
				return fmt.Errorf("mount %s: %w: %v: %s", dev, errStorageIO, err2, strings.TrimSpace(string(out2)))
			}
			// An unmountable card usually means a filesystem this kernel has
			// no driver for (APFS, HFS+, NTFS, ...) or a partition table with
			// every partition deleted (Windows "delete volume"). Name the
			// actual situation and point at the fix instead of surfacing a
			// bare "Invalid argument".
			fsType, ptType := probeBlkid(dev)
			if fsType != "" && !kernelSupportsFilesystem(fsType) {
				return fmt.Errorf("unsupported filesystem %q on %s; format the card (Settings page) as FAT32/exFAT/ext4 to use it on this device", fsType, dev)
			}
			if fsType == "" && ptType != "" {
				return fmt.Errorf("card has a %s partition table but no usable partition (volume deleted?); format the card (Settings page) to use it on this device", ptType)
			}
			return fmt.Errorf("mount %s: %v: %s / %v: %s", dev, err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
		}
	}
	if err := storageWriteProbe(mountPoint); err != nil {
		_, _ = runStorageCommand(15*time.Second, "umount", mountPoint)
		return fmt.Errorf("card is not writable: %w", err)
	}
	return nil
}

func (o *realStorageOps) Unmount(mountPoint string, lazy bool) error {
	if !o.IsMounted(mountPoint) {
		return nil
	}
	var flushErr error
	if !lazy {
		if out, err := runStorageCommand(15*time.Second, "sync", "-f", mountPoint); err != nil {
			flushErr = fmt.Errorf("sync %s: %w: %s", mountPoint, err, strings.TrimSpace(string(out)))
			// Pending writeback errors must not prevent eject or recovery by
			// formatting. A successful umount still detaches the filesystem.
			logging.Warnf("config_web", "storage", "sync %s failed; attempting unmount: %v: %s", mountPoint, err, strings.TrimSpace(string(out)))
		}
	}
	args := []string{mountPoint}
	if lazy {
		args = []string{"-l", mountPoint}
	}
	if out, err := runStorageCommand(15*time.Second, "umount", args...); err != nil {
		return fmt.Errorf("umount %s: %w: %s", mountPoint, err, strings.TrimSpace(string(out)))
	}
	if flushErr != nil {
		return fmt.Errorf("%w: %w", ErrStorageFlush, flushErr)
	}
	return nil
}

func (o *realStorageOps) IsMounted(mountPoint string) bool {
	source, _ := storageMount(mountPoint)
	return source != ""
}

func storageMount(mountPoint string) (source, options string) {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 4 && unescapeMountInfoField(fields[1]) == mountPoint {
			return unescapeMountInfoField(fields[0]), fields[3]
		}
	}
	return "", ""
}

func sameStorageDevice(source, device string) bool {
	if source == device {
		return true
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return false
	}
	deviceInfo, err := os.Stat(device)
	return err == nil && os.SameFile(sourceInfo, deviceInfo)
}

func (o *realStorageOps) Healthy(mountPoint string) bool {
	source, options := storageMount(mountPoint)
	if source == "" || !strings.Contains(","+options+",", ",rw,") {
		return false
	}
	var st syscall.Statfs_t
	return syscall.Statfs(mountPoint, &st) == nil && st.Flags&1 == 0 // ST_RDONLY
}

func (o *realStorageOps) VerifyWritable(mountPoint string) error {
	return storageWriteProbe(mountPoint)
}

func (o *realStorageOps) SnapshotIdentity(ctx context.Context, device, mountPoint string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "blkid", "-s", "UUID", "-o", "value", device).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("blkid %s: %w: %s", device, err, strings.TrimSpace(string(output)))
	}
	uuid := strings.TrimSpace(string(output))
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", "", err
	}
	cleanMount := filepath.Clean(mountPoint)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || filepath.Clean(unescapeMountInfoField(fields[4])) != cleanMount {
			continue
		}
		return uuid, fields[0] + ":" + fields[2], nil
	}
	return "", "", fmt.Errorf("mount point %s is absent from mountinfo", mountPoint)
}

func unescapeMountInfoField(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func (o *realStorageOps) SpaceInfo(path string) (int64, int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := int64(st.Bsize)
	return int64(st.Bavail) * bsize, int64(st.Blocks) * bsize, nil
}

// mbrPartitionStartLBA aligns the single partition to 4 MB (SD erase blocks).
const mbrPartitionStartLBA = 8192

// blkrrpartIoctl asks the kernel to re-read the partition table (BLKRRPART).
const blkrrpartIoctl = 0x125F

// buildMBRSector builds a 512-byte MBR with one partition spanning the card
// from mbrPartitionStartLBA to the end. Pure function so the partition math
// is host-testable without touching a block device.
func buildMBRSector(totalSectors uint64, partType byte) ([]byte, error) {
	if totalSectors <= mbrPartitionStartLBA+2048 {
		return nil, fmt.Errorf("device too small: %d sectors", totalSectors)
	}
	numSectors := totalSectors - mbrPartitionStartLBA
	if numSectors > 0xFFFFFFFF {
		numSectors = 0xFFFFFFFF // MBR limit (~2 TiB); clamp rather than fail
	}
	sector := make([]byte, 512)
	entry := sector[446:462]
	entry[0] = 0x00                                 // not bootable
	entry[1], entry[2], entry[3] = 0xFE, 0xFF, 0xFF // CHS begin: LBA marker
	entry[4] = partType                             // filesystem type
	entry[5], entry[6], entry[7] = 0xFE, 0xFF, 0xFF // CHS end: LBA marker
	putLE32(entry[8:12], mbrPartitionStartLBA)      // first LBA
	putLE32(entry[12:16], uint32(numSectors))       // sector count
	sector[510], sector[511] = 0x55, 0xAA           // boot signature
	return sector, nil
}

func putLE32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

// mbrPartitionType maps a format filesystem to its MBR partition type byte.
func mbrPartitionType(fs string) (byte, error) {
	switch fs {
	case StorageFormatFAT32:
		return 0x0C, nil // W95 FAT32 (LBA)
	case StorageFormatExt4:
		return 0x83, nil // Linux
	case StorageFormatExFAT:
		return 0x07, nil // HPFS/NTFS/exFAT
	default:
		return 0, fmt.Errorf("unsupported filesystem %q", fs)
	}
}

// FormatDisk rewrites the MBR with a single partition and creates the
// filesystem on it. Erases the entire card.
func (o *realStorageOps) FormatDisk(fs string) (string, error) {
	device := o.currentDevice()
	partType, err := mbrPartitionType(fs)
	if err != nil {
		return "", err
	}
	// Resolve mkfs before opening the card or rewriting its partition table.
	tool, args, err := storageFormatCommand(fs)
	if err != nil {
		return "", err
	}
	if device == "" {
		return "", fmt.Errorf("no unambiguous SD card is present")
	}
	// Keep every step on the same device if discovery changes during the job.
	o = &realStorageOps{device: device}
	totalSectors, err := o.deviceSectors()
	if err != nil {
		return "", err
	}
	sector, err := buildMBRSector(totalSectors, partType)
	if err != nil {
		return "", err
	}

	base := "/dev/" + device

	// BLKRRPART fails EBUSY while ANY partition of the device is still held:
	// umount returning does not mean the kernel has released the bdev (journal
	// commits and page-cache writeback are asynchronous), and udev's blkid
	// probe may briefly open the partition after change events. Make sure no
	// partition is mounted anymore, flush, and give stragglers a moment.
	if err := o.waitNoMounts(5 * time.Second); err != nil {
		return "", err
	}
	if out, err := runStorageCommand(15*time.Second, "sync"); err != nil {
		return "", fmt.Errorf("sync before format: %w: %s", err, strings.TrimSpace(string(out)))
	}
	time.Sleep(300 * time.Millisecond)

	f, err := os.OpenFile(base, os.O_WRONLY, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", base, err)
	}
	if _, err := f.WriteAt(sector, 0); err != nil {
		f.Close()
		return "", fmt.Errorf("write MBR to %s: %w", base, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("sync %s: %w", base, err)
	}
	// Ask the kernel to re-read the partition table. Retry with backoff for
	// up to ~10 s: transient holders (writeback, udev blkid) normally clear
	// within a couple of seconds.
	var rrErr syscall.Errno
	delay := 200 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, _, rrErr = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkrrpartIoctl, 0)
		if rrErr == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
	f.Close()
	if rrErr != 0 {
		return "", fmt.Errorf("re-read partition table on %s: %v (holders: %s)", base, rrErr, o.deviceHolders())
	}

	part := base + "p1"
	if err := waitForPath(part, 10*time.Second); err != nil {
		return "", fmt.Errorf("partition %s did not appear: %w", part, err)
	}

	if out, err := runStorageCommand(5*time.Minute, tool, append(args, part)...); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", tool, part, err, strings.TrimSpace(string(out)))
	}
	return part, nil
}

func storageFormatCommand(fs string) (string, []string, error) {
	var tool string
	var args []string
	switch fs {
	case StorageFormatFAT32:
		tool, args = "mkfs.vfat", []string{"-F", "32", "-n", storageVolumeLabel}
	case StorageFormatExt4:
		tool, args = "mkfs.ext4", []string{"-F", "-L", storageVolumeLabel}
	case StorageFormatExFAT:
		tool, args = "mkfs.exfat", []string{"-L", storageVolumeLabel}
	default:
		return "", nil, fmt.Errorf("unsupported filesystem %q", fs)
	}
	path, err := exec.LookPath(tool)
	if err != nil {
		return "", nil, fmt.Errorf("required formatting tool %s is unavailable: %w", tool, err)
	}
	return path, args, nil
}

// blkidCandidates lists probe binaries in preference order. The device PATH
// resolves plain "blkid" to /sbin/blkid, a busybox applet that knows few
// signatures (prints nothing for APFS/NTFS, exits 0) and ignores -p; the
// full util-linux blkid at /usr/sbin/blkid recognizes them and supports
// low-level probing, so it must be tried first.
var blkidCandidates = []string{"/usr/sbin/blkid", "/sbin/blkid", "blkid"}

func storageSignatureAbsent(dev string) bool {
	for _, bin := range blkidCandidates {
		out, err := runStorageCommand(3*time.Second, bin, "-p", dev)
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		return blkidReportsNoSignature(out, err)
	}
	return false
}

func blkidReportsNoSignature(out []byte, err error) bool {
	// util-linux low-level probing exits 2 when it found no signature. Exit
	// 0 with empty output is not evidence (some BusyBox probes behave so).
	var exit *exec.ExitError
	return len(bytes.TrimSpace(out)) == 0 && errors.As(err, &exit) && exit.ExitCode() == 2
}

// probeBlkid asks blkid what the device carries: a filesystem signature
// (TYPE) and/or a partition table (PTTYPE, e.g. a card whose volumes were
// deleted on a PC). Plain blkid may print nothing for a bare partition
// table, so low-level probing (-p) is also tried. Empty strings mean no
// probe found anything.
func probeBlkid(dev string) (fsType, ptType string) {
	for _, bin := range blkidCandidates {
		for _, args := range [][]string{{dev}, {"-p", dev}} {
			out, err := runStorageCommand(3*time.Second, bin, args...)
			if err != nil {
				continue
			}
			if fs, pt := parseBlkidLine(string(out)); fs != "" || pt != "" {
				return fs, pt
			}
		}
	}
	return "", ""
}

// parseBlkidLine extracts TYPE and PTTYPE values from a blkid output line
// like `/dev/mmcblk2p1: UUID="..." TYPE="apfs"` or
// `/dev/mmcblk2: PTUUID="..." PTTYPE="dos"`.
func parseBlkidLine(line string) (fsType, ptType string) {
	return blkidValue(line, ` TYPE="`), blkidValue(line, ` PTTYPE="`)
}

func blkidValue(line, marker string) string {
	start := strings.Index(line, marker)
	if start < 0 {
		return ""
	}
	rest := line[start+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// kernelSupportsFilesystem reports whether the running kernel can mount the
// given filesystem type, per /proc/filesystems. Unknown on read failure —
// err on the side of NOT claiming "unsupported" so the raw mount error
// (which may have a different cause) is preserved.
func kernelSupportsFilesystem(fsType string) bool {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[len(fields)-1] == fsType {
			return true
		}
	}
	return false
}

// waitNoMounts blocks until no partition of the card appears in the mount
// table (a lazy umount detaches asynchronously), or fails naming the mount.
func (o *realStorageOps) waitNoMounts(timeout time.Duration) error {
	prefix := "/dev/" + o.currentDevice()
	deadline := time.Now().Add(timeout)
	for {
		mounted := ""
		if data, err := os.ReadFile("/proc/self/mounts"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) >= 2 && strings.HasPrefix(fields[0], prefix) {
					mounted = fields[0] + " at " + fields[1]
					break
				}
			}
		}
		if mounted == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cannot format: %s is still mounted", mounted)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// deviceHolders reports what still references the card when BLKRRPART fails,
// so the error message names the culprit instead of a bare EBUSY: lingering
// mounts of any partition, and processes holding the device nodes open.
func (o *realStorageOps) deviceHolders() string {
	device := o.currentDevice()
	var findings []string

	// Any partition of the card still mounted?
	if data, err := os.ReadFile("/proc/self/mounts"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[0], "/dev/"+device) {
				findings = append(findings, fmt.Sprintf("%s mounted at %s", fields[0], fields[1]))
			}
		}
	}

	// Any process with an open fd on the device or a partition? (busybox has
	// no fuser/lsof; scan /proc/*/fd directly.)
	prefix := "/dev/" + device
	if procs, err := os.ReadDir("/proc"); err == nil {
		for _, proc := range procs {
			pid := proc.Name()
			if pid[0] < '0' || pid[0] > '9' {
				continue
			}
			fdDir := filepath.Join("/proc", pid, "fd")
			fds, err := os.ReadDir(fdDir)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
				if err != nil || !strings.HasPrefix(target, prefix) {
					continue
				}
				comm, _ := os.ReadFile(filepath.Join("/proc", pid, "comm"))
				findings = append(findings, fmt.Sprintf("pid %s (%s) holds %s", pid, strings.TrimSpace(string(comm)), target))
				break
			}
		}
	}

	if len(findings) == 0 {
		return "none visible (transient kernel reference, e.g. writeback)"
	}
	return strings.Join(findings, "; ")
}

// deviceSectors reads the card size in 512-byte sectors from sysfs.
func (o *realStorageOps) deviceSectors() (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/sys/block", o.currentDevice(), "size"))
	if err != nil {
		return 0, fmt.Errorf("read device size: %w", err)
	}
	var sectors uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &sectors); err != nil {
		return 0, fmt.Errorf("parse device size %q: %w", strings.TrimSpace(string(data)), err)
	}
	return sectors, nil
}

func waitForPath(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *StorageManager) reconfigureStateView(cfg StorageConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mirrorOnly {
		m.cfg = cfg
	}
}

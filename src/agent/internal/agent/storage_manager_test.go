package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStorageOps struct {
	present    bool
	dev        string
	prepareErr error
	healthy    bool
	probeErr   error
	// prepareBlock, when non-nil, makes Prepare block until it is closed. Used
	// to prove the read endpoints stay responsive while a mount is in flight.
	// prepareEntered is closed by Prepare once it has begun blocking.
	prepareBlock   chan struct{}
	prepareEntered chan struct{}
	prepareOnce    sync.Once

	free       int64
	total      int64
	spaceErr   error
	formatErr  error
	unmountErr error
	blank      bool

	remountErr     error
	nextPrepareErr error
	// spaceFn, when set, answers SpaceInfo per path (used by migration
	// tests to model eMMC free space changing as files move).
	spaceFn func(path string) (int64, int64, error)

	mounted          bool
	prepares         int
	probes           int
	unmounts         int
	lazyUnmnts       int
	configuredDevice string
	lastUnmountPoint string
	formats          int
	lastFormatFS     string
	snapshotUUID     string
	snapshotMountID  string
	snapshotErr      error
	snapshotContext  context.Context
}

func (f *fakeStorageOps) CardDevice() (string, bool) {
	if !f.present {
		return "", false
	}
	dev := f.dev
	if dev == "" {
		dev = "/dev/mmcblk2p1"
	}
	return dev, true
}

func (f *fakeStorageOps) setDevice(device string) { f.configuredDevice = device }

func (f *fakeStorageOps) Prepare(dev, mountPoint string) error {
	f.prepares++
	if f.prepareBlock != nil {
		f.prepareOnce.Do(func() { close(f.prepareEntered) })
		<-f.prepareBlock
	}
	if f.nextPrepareErr != nil {
		err := f.nextPrepareErr
		f.nextPrepareErr = nil
		return err
	}
	if f.prepareErr != nil {
		return f.prepareErr
	}
	f.mounted = true
	return nil
}

func (f *fakeStorageOps) Unmount(mountPoint string, lazy bool) error {
	f.unmounts++
	f.lastUnmountPoint = mountPoint
	if lazy {
		f.lazyUnmnts++
	}
	if f.unmountErr != nil && !lazy && !errors.Is(f.unmountErr, ErrStorageFlush) {
		return f.unmountErr // mount point busy: the mount survives
	}
	f.mounted = false
	if !lazy {
		return f.unmountErr
	}
	return nil
}

func (f *fakeStorageOps) IsMounted(mountPoint string) bool { return f.mounted }
func (f *fakeStorageOps) Healthy(mountPoint string) bool   { return f.healthy }

func (f *fakeStorageOps) VerifyWritable(string) error {
	f.probes++
	return f.probeErr
}

func (f *fakeStorageOps) SpaceInfo(path string) (int64, int64, error) {
	if f.spaceErr != nil {
		return 0, 0, f.spaceErr
	}
	if f.spaceFn != nil {
		return f.spaceFn(path)
	}
	return f.free, f.total, nil
}

func (f *fakeStorageOps) FormatDisk(fs string) (string, error) {
	f.formats++
	f.lastFormatFS = fs
	if f.formatErr != nil {
		return "", f.formatErr
	}
	// A freshly formatted card is no longer blank and mounts cleanly.
	f.blank = false
	f.prepareErr = nil
	f.nextPrepareErr = f.remountErr
	f.remountErr = nil
	return "/dev/mmcblk2p1", nil
}

func (f *fakeStorageOps) CardIsBlank() bool { return f.blank }

func (f *fakeStorageOps) SnapshotIdentity(ctx context.Context, _, _ string) (string, string, error) {
	f.snapshotContext = ctx
	if f.snapshotErr != nil {
		return "", "", f.snapshotErr
	}
	uuid := f.snapshotUUID
	if uuid == "" {
		uuid = "test-sd-uuid"
	}
	mountID := f.snapshotMountID
	if mountID == "" {
		mountID = "42:179:1"
	}
	return uuid, mountID, nil
}

func TestStorageSnapshotLeasePinsCardAndBlocksMutations(t *testing.T) {
	ops := &fakeStorageOps{present: true, mounted: true, healthy: true, free: 1 << 30, total: 1 << 31}
	m := newTestStorageManager(t, ops)
	m.card = StorageCardStatus{Present: true, Mounted: true, Device: "/dev/mmcblk2p1"}
	m.lastEffective = StorageModeDual

	type snapshotContextKey struct{}
	acquireCtx := context.WithValue(context.Background(), snapshotContextKey{}, "acquire")
	lease, err := m.AcquireSnapshotLease(acquireCtx)
	if err != nil {
		t.Fatal(err)
	}
	if ops.snapshotContext != acquireCtx {
		t.Fatal("snapshot acquisition did not propagate its context")
	}
	snapshot := lease.Snapshot()
	if snapshot.DevicePath != "/dev/mmcblk2p1" || snapshot.FilesystemUUID != "test-sd-uuid" || snapshot.MountID == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	validateCtx := context.WithValue(context.Background(), snapshotContextKey{}, "validate")
	if err := lease.Validate(validateCtx); err != nil {
		t.Fatalf("fresh lease validation = %v", err)
	}
	if ops.snapshotContext != validateCtx {
		t.Fatal("snapshot validation did not propagate its context")
	}
	if err := m.SafeEject(); err == nil || !strings.Contains(err.Error(), "snapshot lease") {
		t.Fatalf("SafeEject during lease = %v", err)
	}
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err == nil || !strings.Contains(err.Error(), "snapshot lease") {
		t.Fatalf("StartFormat during lease = %v", err)
	}
	changed := m.cfg
	changed.MinCardFreeMB++
	if err := m.Reconfigure(changed); err == nil || !strings.Contains(err.Error(), "snapshot lease") {
		t.Fatalf("Reconfigure during lease = %v", err)
	}

	ops.snapshotMountID = "99:179:1"
	if err := lease.Validate(context.Background()); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed mount validation = %v", err)
	}
	ops.snapshotMountID = "42:179:1"
	lease.Release()
	lease.Release()
	if err := m.SafeEject(); err != nil {
		t.Fatalf("SafeEject after release = %v", err)
	}
}

func TestStorageSnapshotLeaseRejectsMissingOrBusyCard(t *testing.T) {
	ops := &fakeStorageOps{healthy: true}
	m := newTestStorageManager(t, ops)
	if _, err := m.AcquireSnapshotLease(context.Background()); err == nil {
		t.Fatal("lease acquired without a mounted card")
	}
	m.card = StorageCardStatus{Present: true, Mounted: true, Device: "/dev/mmcblk2p1"}
	ops.present = true
	ops.mounted = true
	m.formatJob.Status = StorageFormatRunning
	if _, err := m.AcquireSnapshotLease(context.Background()); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("lease during format = %v", err)
	}
}

func newTestStorageManager(t *testing.T, ops *fakeStorageOps) *StorageManager {
	return newTestStorageManagerWithWatermarks(t, ops, 0, 0)
}

// newTestStorageManagerWithWatermarks lets migration tests pin the start/stop
// percentages (0 = use the built-in defaults) so they do not depend on the
// production default values.
func newTestStorageManagerWithWatermarks(t *testing.T, ops *fakeStorageOps, startPct, stopPct int) *StorageManager {
	t.Helper()
	dir := t.TempDir()
	cfg := StorageConfig{
		MountPoint:          filepath.Join(dir, "sdcard"),
		MigrateStartFreePct: startPct,
		MigrateStopFreePct:  stopPct,
	}
	m := newStorageManagerWithOps(cfg, ops, filepath.Join(dir, "storage.state"), filepath.Join(dir, "emmc"), nil)
	return m
}

// tickN runs n poll cycles (debounce needs two to accept a presence change).
func tickN(m *StorageManager, n int) {
	for i := 0; i < n; i++ {
		m.tick()
	}
}

func TestDeriveEffectiveMode(t *testing.T) {
	if got := deriveEffectiveMode(false); got != StorageModeEMMCOnly {
		t.Fatalf("deriveEffectiveMode(false) = %d, want eMMC-only", got)
	}
	if got := deriveEffectiveMode(true); got != StorageModeDual {
		t.Fatalf("deriveEffectiveMode(true) = %d, want dual", got)
	}
}

func TestStorageManagerReconfigureKeepsSingleHardwareOwner(t *testing.T) {
	ops := &fakeStorageOps{mounted: true}
	m := newTestStorageManager(t, ops)
	oldMountPoint := m.cfg.MountPointOrDefault()
	m.card = StorageCardStatus{Present: true, Mounted: true, Device: "/dev/mmcblk2p1"}
	m.lastEffective = StorageModeDual
	events := m.Subscribe()

	newMountPoint := filepath.Join(t.TempDir(), "new-card")
	newConfig := m.cfg
	newConfig.MountPoint = newMountPoint
	newConfig.Device = "mmcblk9"
	newConfig.MinCardFreeMB = 128
	if err := m.Reconfigure(newConfig); err != nil {
		t.Fatal(err)
	}
	status := m.Status()
	if status.MountPoint != newMountPoint || status.Card.Present || status.Card.Mounted {
		t.Fatalf("status after reconfigure=%+v", status)
	}
	if ops.unmounts != 1 || ops.lastUnmountPoint != oldMountPoint {
		t.Fatalf("unmounts=%d mount_point=%q, want old mount %q", ops.unmounts, ops.lastUnmountPoint, oldMountPoint)
	}
	if ops.configuredDevice != "mmcblk9" {
		t.Fatalf("configured device=%q", ops.configuredDevice)
	}
	select {
	case event := <-events:
		if event.EffectiveMode != StorageModeEMMCOnly {
			t.Fatalf("effective mode event=%d", event.EffectiveMode)
		}
	default:
		t.Fatal("storage fallback event was not published")
	}
}

func TestStorageManagerReconfigureKeepsOldConfigWhenUnmountFails(t *testing.T) {
	ops := &fakeStorageOps{mounted: true, unmountErr: errors.New("busy")}
	m := newTestStorageManager(t, ops)
	oldMountPoint := m.cfg.MountPointOrDefault()
	m.card = StorageCardStatus{Present: true, Mounted: true, Device: "/dev/mmcblk2p1"}

	newConfig := m.cfg
	newConfig.MountPoint = filepath.Join(t.TempDir(), "new-card")
	newConfig.Device = "mmcblk9"
	if err := m.Reconfigure(newConfig); err == nil {
		t.Fatal("Reconfigure succeeded after the old mount refused to detach")
	}
	if got := m.Status().MountPoint; got != oldMountPoint {
		t.Fatalf("mount point=%q, want unchanged %q", got, oldMountPoint)
	}
	if ops.configuredDevice != "" {
		t.Fatalf("device changed to %q after failed unmount", ops.configuredDevice)
	}
}

func TestStorageManagerMountsOnInsertWithDebounce(t *testing.T) {
	ops := &fakeStorageOps{free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)

	tickN(m, 2)
	if m.EffectiveMode() != StorageModeEMMCOnly {
		t.Fatalf("effective mode without card = %d, want eMMC-only", m.EffectiveMode())
	}

	ops.present = true
	m.tick()
	if got := m.Status(); got.Card.Present {
		t.Fatal("presence accepted after a single observation; debounce missing")
	}
	m.tick()
	status := m.Status()
	if !status.Card.Present || !status.Card.Mounted {
		t.Fatalf("card not mounted after debounced insert: %+v", status.Card)
	}
	if status.EffectiveMode != StorageModeDual {
		t.Fatalf("effective mode = %d, want dual", status.EffectiveMode)
	}
}

func TestStorageManagerReadEndpointsStayResponsiveDuringMount(t *testing.T) {
	ops := &fakeStorageOps{
		present:        true,
		free:           1 << 30,
		total:          1 << 31,
		healthy:        true,
		prepareBlock:   make(chan struct{}),
		prepareEntered: make(chan struct{}),
	}
	m := newTestStorageManager(t, ops)
	m.card.Present = true // pre-accept presence so the first tick mounts

	// Run the mounting tick in the background; it blocks inside ops.Prepare.
	tickDone := make(chan struct{})
	go func() { defer close(tickDone); m.tick() }()

	select {
	case <-ops.prepareEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare was never reached")
	}

	// Prepare is blocking with the mount in flight. Every read endpoint must
	// still return promptly instead of waiting on m.mu.
	endpoints := map[string]func(){
		"Status":        func() { m.Status() },
		"EffectiveMode": func() { m.EffectiveMode() },
		"ReadRoots":     func() { m.ReadRoots(StorageClassAudio) },
		"CleanupRoots":  func() { m.CleanupRoots(StorageClassAudio) },
	}
	for name, call := range endpoints {
		done := make(chan struct{})
		go func() { defer close(done); call() }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			close(ops.prepareBlock)
			t.Fatalf("%s blocked while a mount was in flight", name)
		}
	}

	// Let the mount finish and confirm it lands normally.
	close(ops.prepareBlock)
	<-tickDone
	if status := m.Status(); !status.Card.Mounted {
		t.Fatalf("card not mounted after Prepare unblocked: %+v", status.Card)
	}
}

// A format must never run concurrently with an in-flight mount: during the
// ops.Prepare window m.card.Mounted is still false, so without the m.cardOperation
// guard StartFormat would skip its unmount step and hand mkfs a device that
// mount is actively working on.
func TestStorageManagerFormatRejectedWhileMountInFlight(t *testing.T) {
	ops := &fakeStorageOps{
		present:        true,
		free:           1 << 30,
		total:          1 << 31,
		healthy:        true,
		prepareBlock:   make(chan struct{}),
		prepareEntered: make(chan struct{}),
	}
	m := newTestStorageManager(t, ops)
	m.card.Present = true // pre-accept presence so the first tick mounts

	tickDone := make(chan struct{})
	go func() { defer close(tickDone); m.tick() }()

	select {
	case <-ops.prepareEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Prepare was never reached")
	}

	err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken)
	if err == nil {
		close(ops.prepareBlock)
		t.Fatal("StartFormat() succeeded while a mount was in flight; mkfs would race mount")
	}
	if !strings.Contains(err.Error(), "mount attempt is in progress") {
		close(ops.prepareBlock)
		t.Fatalf("StartFormat() error = %v, want an in-progress mount rejection", err)
	}
	if ops.formats != 0 {
		close(ops.prepareBlock)
		t.Fatalf("FormatDisk called %d times during an in-flight mount, want 0", ops.formats)
	}

	// SafeEject must not report "no mounted card" during the same window.
	if err := m.SafeEject(); err == nil || !strings.Contains(err.Error(), "mount attempt is in progress") {
		close(ops.prepareBlock)
		t.Fatalf("SafeEject() error = %v, want an in-progress mount rejection", err)
	}

	// Once the mount lands, a format is accepted again.
	close(ops.prepareBlock)
	<-tickDone
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
		t.Fatalf("StartFormat() after the mount settled = %v, want success", err)
	}
	// Drain the async job so it cannot outlive the test's temp dir.
	if job := waitForFormatJob(t, m); job.Status != StorageFormatSuccess {
		t.Fatalf("format job = %+v, want success", job)
	}
}

func TestStorageManagerRemovalFallsBackToEMMCOnly(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	events := m.Subscribe()
	tickN(m, 2)
	if m.EffectiveMode() != StorageModeDual {
		t.Fatalf("setup: effective mode = %d, want dual", m.EffectiveMode())
	}
	<-events // insertion event

	ops.present = false
	tickN(m, 2)
	if m.EffectiveMode() != StorageModeEMMCOnly {
		t.Fatalf("effective mode after removal = %d, want eMMC-only", m.EffectiveMode())
	}
	if ops.lazyUnmnts != 1 {
		t.Fatalf("lazy unmounts = %d, want 1 (forced removal path)", ops.lazyUnmnts)
	}
	select {
	case ev := <-events:
		if ev.EffectiveMode != StorageModeEMMCOnly {
			t.Fatalf("event mode = %d, want eMMC-only", ev.EffectiveMode)
		}
	default:
		t.Fatal("no event delivered on removal")
	}
}

func TestStorageManagerUnhealthyMountTreatedAsRemoval(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	ops.healthy = false
	m.tick()
	status := m.Status()
	if status.Card.Mounted {
		t.Fatal("card still mounted after failed health check")
	}
	if status.EffectiveMode != StorageModeEMMCOnly {
		t.Fatalf("effective mode = %d, want eMMC-only", status.EffectiveMode)
	}
}

func TestStorageManagerSafeEjectLatch(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)
	if err := m.SafeEject(); err != nil {
		t.Fatal(err)
	}
	if m.Status().Card.Mounted {
		t.Fatal("card still mounted after eject")
	}

	// The card stays in the slot; further ticks must not remount it.
	prepares := ops.prepares
	tickN(m, 3)
	if ops.prepares != prepares {
		t.Fatal("card remounted while eject latch active")
	}

	// Physical removal clears the latch; reinsertion mounts again.
	ops.present = false
	tickN(m, 2)
	ops.present = true
	tickN(m, 2)
	if !m.Status().Card.Mounted {
		t.Fatal("card not remounted after removal and reinsertion")
	}
}

func TestStorageManagerMountFailureRecordsReasonAndDoesNotRetryEveryTick(t *testing.T) {
	ops := &fakeStorageOps{present: true, prepareErr: errors.New("bad superblock")}
	m := newTestStorageManager(t, ops)
	tickN(m, 4)
	status := m.Status()
	if status.Card.Mounted {
		t.Fatal("unusable card reported as mounted")
	}
	if !strings.Contains(status.Card.Reason, "bad superblock") {
		t.Fatalf("reason = %q, want the gate failure", status.Card.Reason)
	}
	if status.EffectiveMode != StorageModeEMMCOnly {
		t.Fatalf("effective mode = %d, want eMMC-only", status.EffectiveMode)
	}
	if ops.prepares != 1 {
		t.Fatalf("prepares = %d, want 1: failed mounts must not retry every tick", ops.prepares)
	}
}

func TestStorageManagerResolveDirAlwaysEMMC(t *testing.T) {
	// New data always lands on eMMC, card or no card; the migrator moves
	// older files to SD when eMMC runs low.
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	want := filepath.Join(m.emmcRoot, "audio")
	dir, err := m.ResolveDir(StorageClassAudio)
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Fatalf("ResolveDir with card = %s, want %s", dir, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("resolved dir not created: %v", err)
	}

	ops.present = false
	tickN(m, 2)
	if dir, err = m.ResolveDir(StorageClassAudio); err != nil || dir != want {
		t.Fatalf("ResolveDir without card = %s (%v), want %s", dir, err, want)
	}
}

func TestStorageManagerReadRootsMergesEMMCFirst(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	roots := m.ReadRoots(StorageClassAudio)
	if len(roots) != 2 {
		t.Fatalf("roots = %v, want eMMC + SD", roots)
	}
	// eMMC first: during the migration crash window the eMMC copy is
	// authoritative, so first-hit readers must see it before the SD copy.
	if roots[0] != filepath.Join(m.emmcRoot, "audio") {
		t.Fatalf("first root = %s, want the eMMC dir", roots[0])
	}
	if roots[1] != filepath.Join(m.cfg.MountPointOrDefault(), storageSDSubdir, "audio") {
		t.Fatalf("second root = %s, want the SD dir", roots[1])
	}

	ops.present = false
	tickN(m, 2)
	roots = m.ReadRoots(StorageClassAudio)
	if len(roots) != 1 || roots[0] != filepath.Join(m.emmcRoot, "audio") {
		t.Fatalf("roots without card = %v, want only eMMC", roots)
	}
}

// waitForFormatJob polls until the async format job leaves the running state.
func waitForFormatJob(t *testing.T, m *StorageManager) StorageFormatJob {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job := m.Status().FormatJob
		if job.Status != StorageFormatRunning && job.Status != StorageFormatIdle {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("format job did not finish: %+v", job)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStorageManagerFormatRequiresConfirmation(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	if err := m.StartFormat(StorageFormatFAT32, "yes"); err == nil {
		t.Fatal("format without the confirm token succeeded")
	}
	if err := m.StartFormat("ntfs", StorageFormatConfirmToken); err == nil {
		t.Fatal("format with unsupported filesystem succeeded")
	}
	if ops.formats != 0 {
		t.Fatal("format executed without valid request")
	}

	if err := m.StartFormat("", StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess {
		t.Fatalf("job = %+v, want success", job)
	}
	if ops.lastFormatFS != StorageFormatFAT32 {
		t.Fatalf("format fs = %q, want fat32 default", ops.lastFormatFS)
	}
	if ops.formats != 1 {
		t.Fatalf("formats = %d, want 1", ops.formats)
	}
	if !m.Status().Card.Mounted {
		t.Fatal("card not remounted after format")
	}
}

func TestStorageManagerFormatExt4MountsAfterSuccess(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	if err := m.StartFormat(StorageFormatExt4, StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess {
		t.Fatalf("job = %+v, want success", job)
	}
	if ops.lastFormatFS != StorageFormatExt4 {
		t.Fatalf("format fs = %q, want ext4", ops.lastFormatFS)
	}
	if status := m.Status(); !status.Card.Mounted || status.EffectiveMode != StorageModeDual {
		t.Fatalf("status = %+v, want mounted dual storage after format", status)
	}
}

func TestStorageManagerFormatRemainsRunningUntilRemountFinishes(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	// Block only the post-format Prepare call so the state exposed during the
	// remount is deterministic.
	ops.prepareBlock = make(chan struct{})
	ops.prepareEntered = make(chan struct{})
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ops.prepareEntered:
	case <-time.After(time.Second):
		t.Fatal("post-format mount did not start")
	}

	if status := m.Status(); status.FormatJob.Status != StorageFormatRunning {
		t.Fatalf("format job published completion before remount finished: %+v", status)
	}

	close(ops.prepareBlock)
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess {
		t.Fatalf("job = %+v, want success", job)
	}
	if status := m.Status(); !status.Card.Mounted || status.EffectiveMode != StorageModeDual {
		t.Fatalf("status = %+v, want mounted dual storage after completed job", status)
	}
}

func TestStorageManagerFormatExFATMountsAfterSuccess(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	if err := m.StartFormat(StorageFormatExFAT, StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess {
		t.Fatalf("job = %+v, want success", job)
	}
	if ops.lastFormatFS != StorageFormatExFAT {
		t.Fatalf("format fs = %q, want exfat", ops.lastFormatFS)
	}
	if status := m.Status(); !status.Card.Mounted || status.EffectiveMode != StorageModeDual {
		t.Fatalf("status = %+v, want mounted dual storage after format", status)
	}
}

func TestStorageManagerFormatFailureReported(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true, formatErr: errors.New("card yanked")}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatFailed || !strings.Contains(job.Error, "card yanked") {
		t.Fatalf("job = %+v, want failure with reason", job)
	}
	if m.Status().Card.Mounted {
		t.Fatal("card mounted after failed format")
	}
}

func TestStorageManagerPostFormatMountFailureReported(t *testing.T) {
	ops := &fakeStorageOps{
		present: true, free: 1 << 30, total: 1 << 31, healthy: true,
		remountErr: errors.New("mount: device unavailable"),
	}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
		t.Fatal(err)
	}
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatFailed || !strings.Contains(job.Error, "mount: device unavailable") {
		t.Fatalf("job = %+v, want post-format mount failure", job)
	}
	status := m.Status()
	if status.Card.Mounted {
		t.Fatal("card reported mounted after post-format mount failure")
	}
	if !strings.Contains(status.Card.Reason, "mount: device unavailable") {
		t.Fatalf("card reason = %q, want mount failure", status.Card.Reason)
	}

	// The injected remount failure is transient. A physical reinsertion should
	// get a fresh mount attempt rather than inheriting the consumed error.
	ops.present = false
	tickN(m, 2)
	ops.present = true
	tickN(m, 2)
	if status := m.Status(); !status.Card.Mounted {
		t.Fatalf("card did not recover after reinsertion: %+v", status.Card)
	}
}

func TestStorageManagerRejectsActionsDuringFormat(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	// Freeze the job in the running state without spawning the goroutine.
	m.mu.Lock()
	m.formatJob = StorageFormatJob{Status: StorageFormatRunning, FS: StorageFormatFAT32}
	m.mu.Unlock()

	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err == nil {
		t.Fatal("second format accepted while one is running")
	}
	if err := m.SafeEject(); err == nil {
		t.Fatal("eject accepted while formatting")
	}
	prepares := ops.prepares
	tickN(m, 3)
	if ops.prepares != prepares {
		t.Fatal("poll loop touched the card while formatting")
	}
}

func TestStorageManagerAutoFormatsBlankCard(t *testing.T) {
	ops := &fakeStorageOps{
		present: true, healthy: true, free: 1 << 30, total: 1 << 31,
		prepareErr: errors.New("mount: Invalid argument"), blank: true,
	}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess {
		t.Fatalf("auto-format job = %+v, want success", job)
	}
	if !job.Auto {
		t.Fatal("job not marked as auto-triggered")
	}
	if job.FS != StorageFormatFAT32 {
		t.Fatalf("auto-format fs = %q, want fat32", job.FS)
	}
	if ops.formats != 1 {
		t.Fatalf("formats = %d, want 1", ops.formats)
	}
	if status := m.Status(); !status.Card.Mounted || status.EffectiveMode != StorageModeDual {
		t.Fatalf("card not mounted after auto-format: %+v", status)
	}
}

func TestStorageManagerNeverAutoFormatsUnreadableCard(t *testing.T) {
	// A card that fails to mount but carries recognizable content (blank=false,
	// e.g. NTFS or an encrypted volume) must never be formatted.
	ops := &fakeStorageOps{
		present: true, healthy: true,
		prepareErr: errors.New("mount: Invalid argument"), blank: false,
	}
	m := newTestStorageManager(t, ops)
	tickN(m, 4)

	if ops.formats != 0 {
		t.Fatalf("formats = %d, want 0: unreadable card was auto-formatted", ops.formats)
	}
	status := m.Status()
	if status.Card.Mounted || status.EffectiveMode != StorageModeEMMCOnly {
		t.Fatalf("status = %+v, want rejected card on eMMC-only", status)
	}
	if !strings.Contains(status.Card.Reason, "Invalid argument") {
		t.Fatalf("reason = %q, want the mount failure", status.Card.Reason)
	}
}

func TestStorageManagerAutoFormatOncePerInsertion(t *testing.T) {
	ops := &fakeStorageOps{
		present: true, healthy: true, free: 1 << 30, total: 1 << 31,
		prepareErr: errors.New("no filesystem"), blank: true,
		formatErr: errors.New("card yanked"),
	}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)
	job := waitForFormatJob(t, m)
	if job.Status != StorageFormatFailed {
		t.Fatalf("job = %+v, want failure", job)
	}
	tickN(m, 4)
	if ops.formats != 1 {
		t.Fatalf("formats = %d, want 1: failed auto-format must not retry in place", ops.formats)
	}

	// Removal and reinsertion earns a fresh attempt.
	ops.present = false
	tickN(m, 2)
	ops.present = true
	ops.formatErr = nil
	tickN(m, 2)
	job = waitForFormatJob(t, m)
	if job.Status != StorageFormatSuccess || ops.formats != 2 {
		t.Fatalf("job = %+v formats = %d, want success on reinsertion", job, ops.formats)
	}
}

// waitForMigration polls until the migration job leaves the running state.
func waitForMigration(t *testing.T, m *StorageManager) StorageMigrationJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		job := m.Status().Migration
		if job.Status != StorageFormatRunning && job.Status != StorageFormatIdle {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("migration did not finish: %+v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeAgedFile creates a file and backdates its mtime so it is migratable.
func writeAgedFile(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// dirBytes sums the sizes of regular files directly inside dir.
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// migrationSpaceFn models a 1000-byte eMMC filesystem whose free space grows
// as files leave srcDir: free = base + (initial - remaining). The card side
// reports plenty of space.
func migrationSpaceFn(t *testing.T, m *StorageManager, srcDir string, base, initial int64) func(string) (int64, int64, error) {
	return func(path string) (int64, int64, error) {
		if path == m.emmcRoot {
			return base + (initial - dirBytes(t, srcDir)), 1000, nil
		}
		return 1 << 30, 1 << 31, nil
	}
}

func TestStorageManagerMigratesOldestFilesUntilStopWatermark(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true}
	// Pin watermarks (start 10 / stop 30) so the arithmetic below is
	// independent of the production defaults.
	m := newTestStorageManagerWithWatermarks(t, ops, 10, 30)
	srcDir := filepath.Join(m.emmcRoot, "audio")

	// Four aged 100-byte files (a oldest ... d newest) plus one fresh file.
	// eMMC free starts at 40/1000 (4% < 10% start watermark) and gains 100
	// per migrated file; after a, b, c it is 340/1000 (34% ≥ 30% stop).
	writeAgedFile(t, filepath.Join(srcDir, "a.wav"), 100, 4*time.Hour)
	writeAgedFile(t, filepath.Join(srcDir, "b.wav"), 100, 3*time.Hour)
	writeAgedFile(t, filepath.Join(srcDir, "c.wav"), 100, 2*time.Hour)
	writeAgedFile(t, filepath.Join(srcDir, "d.wav"), 100, 1*time.Hour)
	writeAgedFile(t, filepath.Join(srcDir, "fresh.wav"), 100, 0)
	ops.spaceFn = migrationSpaceFn(t, m, srcDir, 40, 500) // 40/1000 free initially

	tickN(m, 2)
	job := waitForMigration(t, m)

	if job.Status != StorageFormatSuccess {
		t.Fatalf("migration job = %+v, want success", job)
	}
	if !strings.Contains(job.Detail, "stop watermark") {
		t.Fatalf("detail = %q, want stop-watermark completion", job.Detail)
	}
	if job.MovedFiles != 3 || job.MovedBytes != 300 {
		t.Fatalf("moved = %d files / %d bytes, want 3/300", job.MovedFiles, job.MovedBytes)
	}
	sdDir := filepath.Join(m.cfg.MountPointOrDefault(), storageSDSubdir, "audio")
	for _, name := range []string{"a.wav", "b.wav", "c.wav"} {
		if _, err := os.Stat(filepath.Join(sdDir, name)); err != nil {
			t.Fatalf("oldest file %s not migrated to SD: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(srcDir, name)); !os.IsNotExist(err) {
			t.Fatalf("migrated file %s still on eMMC", name)
		}
	}
	for _, name := range []string{"d.wav", "fresh.wav"} {
		if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
			t.Fatalf("file %s should have stayed on eMMC: %v", name, err)
		}
	}
}

func TestStorageManagerMigrationDedupesAndSkipsConflicts(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true}
	m := newTestStorageManager(t, ops)
	srcDir := filepath.Join(m.emmcRoot, "audio")
	sdDir := filepath.Join(m.cfg.MountPointOrDefault(), storageSDSubdir, "audio")

	writeAgedFile(t, filepath.Join(srcDir, "same.wav"), 100, 3*time.Hour)
	writeAgedFile(t, filepath.Join(srcDir, "conflict.wav"), 100, 2*time.Hour)
	// same.wav already fully copied by an interrupted run; conflict.wav
	// exists on SD with a different size and must never be overwritten.
	writeAgedFile(t, filepath.Join(sdDir, "same.wav"), 100, 3*time.Hour)
	writeAgedFile(t, filepath.Join(sdDir, "conflict.wav"), 42, 2*time.Hour)
	// A stale in-flight copy from a crashed run must be swept.
	writeAgedFile(t, filepath.Join(sdDir, "x.wav"+migrationPartialSuffix), 10, time.Hour)
	// Watermark can never be satisfied: free space stays below stop.
	ops.spaceFn = func(path string) (int64, int64, error) {
		if path == m.emmcRoot {
			return 50, 1000, nil
		}
		return 1 << 30, 1 << 31, nil
	}

	tickN(m, 2)
	job := waitForMigration(t, m)

	if job.Status != StorageFormatFailed || !strings.Contains(job.Error, "1 files failed") {
		t.Fatalf("job = %+v, want failure reporting the conflict", job)
	}
	// The interrupted transaction was completed: source gone, SD copy kept.
	if _, err := os.Stat(filepath.Join(srcDir, "same.wav")); !os.IsNotExist(err) {
		t.Fatal("deduped file still on eMMC")
	}
	// The conflicting file was left untouched on both sides.
	if _, err := os.Stat(filepath.Join(srcDir, "conflict.wav")); err != nil {
		t.Fatalf("conflicting source removed: %v", err)
	}
	if info, err := os.Stat(filepath.Join(sdDir, "conflict.wav")); err != nil || info.Size() != 42 {
		t.Fatalf("conflicting SD copy modified: %v", err)
	}
	// Stale partial swept.
	if _, err := os.Stat(filepath.Join(sdDir, "x.wav"+migrationPartialSuffix)); !os.IsNotExist(err) {
		t.Fatal("stale partial not removed")
	}
	// Failed runs enter the retry cooldown instead of retriggering each tick.
	m.mu.Lock()
	retryAt := m.migrationRetryAt
	m.mu.Unlock()
	if retryAt.IsZero() || !retryAt.After(time.Now()) {
		t.Fatalf("retry cooldown not set after failed run: %v", retryAt)
	}
}

func TestStorageManagerMigrationExhaustionWarnsAndCoolsDown(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true}
	m := newTestStorageManager(t, ops)
	// eMMC below the start watermark but the audio dir is empty: nothing to
	// migrate, so the run must end with a diagnostic and a cooldown.
	ops.spaceFn = func(path string) (int64, int64, error) {
		if path == m.emmcRoot {
			return 50, 1000, nil
		}
		return 1 << 30, 1 << 31, nil
	}

	tickN(m, 2)
	job := waitForMigration(t, m)
	if job.Status != StorageFormatSuccess || !strings.Contains(job.Detail, "no migratable files") {
		t.Fatalf("job = %+v, want no-candidates diagnostic", job)
	}
	prev := job.FinishedAt
	tickN(m, 3)
	if got := m.Status().Migration.FinishedAt; !got.Equal(prev) {
		t.Fatal("migration retriggered during the cooldown window")
	}
}

func TestStorageManagerNoMigrationAboveWatermark(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true, free: 1 << 30, total: 1 << 31}
	m := newTestStorageManager(t, ops)
	srcDir := filepath.Join(m.emmcRoot, "audio")
	writeAgedFile(t, filepath.Join(srcDir, "a.wav"), 100, time.Hour)

	tickN(m, 3)
	if job := m.Status().Migration; job.Status != StorageFormatIdle {
		t.Fatalf("migration ran with 50%% free space: %+v", job)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "a.wav")); err != nil {
		t.Fatalf("file moved despite healthy free space: %v", err)
	}
}

func TestStorageManagerCleanupRoots(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	roots := m.CleanupRoots(StorageClassAudio)
	sdDir := filepath.Join(m.cfg.MountPointOrDefault(), storageSDSubdir, "audio")
	if len(roots) != 1 || roots[0] != sdDir {
		t.Fatalf("cleanup roots with card = %v, want only the SD dir", roots)
	}

	ops.present = false
	tickN(m, 2)
	roots = m.CleanupRoots(StorageClassAudio)
	if len(roots) != 1 || roots[0] != filepath.Join(m.emmcRoot, "audio") {
		t.Fatalf("cleanup roots without card = %v, want only the eMMC dir", roots)
	}
}

func TestStorageManagerBusyUnmountKeepsStateMounted(t *testing.T) {
	// A busy mount point (e.g. a shell cwd on the card) makes non-lazy
	// unmounts fail. Format and eject must then refuse with a clear error
	// while the in-memory state keeps matching reality: still mounted, still
	// dual storage, no mount/EBUSY churn from the poll loop.
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)
	if !m.Status().Card.Mounted {
		t.Fatal("setup: card not mounted")
	}

	ops.unmountErr = errors.New("umount: target is busy")

	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("StartFormat = %v, want busy-unmount refusal", err)
	}
	if m.Status().FormatJob.Status == StorageFormatRunning {
		t.Fatal("format job started despite failed unmount")
	}
	if err := m.SafeEject(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("SafeEject = %v, want busy-unmount refusal", err)
	}

	status := m.Status()
	if !status.Card.Mounted {
		t.Fatal("state says unmounted while the mount survived (desync)")
	}
	if status.EffectiveMode != StorageModeDual {
		t.Fatalf("effective mode = %d, want dual (card is still usable)", status.EffectiveMode)
	}
	// The poll loop must not try to re-mount over the surviving mount.
	prepares := ops.prepares
	tickN(m, 3)
	if ops.prepares != prepares {
		t.Fatal("poll loop attempted to mount over an existing mount")
	}

	// Once the holder goes away, format proceeds normally.
	ops.unmountErr = nil
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
		t.Fatalf("StartFormat after holder released = %v", err)
	}
	if job := waitForFormatJob(t, m); job.Status != StorageFormatSuccess {
		t.Fatalf("job = %+v, want success", job)
	}
}

func TestParseBlkidLine(t *testing.T) {
	tests := []struct {
		line   string
		wantFS string
		wantPT string
	}{
		{`/dev/mmcblk2p1: UUID="0a1b2c3d" TYPE="apfs" PARTUUID="deadbeef-01"`, "apfs", ""},
		{`/dev/mmcblk2p1: LABEL="AICO" UUID="ABCD-1234" TYPE="vfat"`, "vfat", ""},
		{`/dev/mmcblk2p1: TYPE="hfsplus"`, "hfsplus", ""},
		// Whole-device probe of a card whose volumes were deleted on a PC:
		// a partition table signature but no filesystem.
		{`/dev/mmcblk2: PTUUID="1c6a2f34" PTTYPE="dos"`, "", "dos"},
		{`/dev/mmcblk2: PTUUID="a3b2c1d0-..." PTTYPE="gpt"`, "", "gpt"},
		// PTTYPE must not be mistaken for TYPE and vice versa.
		{`/dev/mmcblk2p1: TYPE="vfat" PTTYPE="dos"`, "vfat", "dos"},
		{`/dev/mmcblk2p1: PARTUUID="deadbeef-01"`, "", ""},
		{``, "", ""},
	}
	for _, tt := range tests {
		gotFS, gotPT := parseBlkidLine(tt.line)
		if gotFS != tt.wantFS || gotPT != tt.wantPT {
			t.Fatalf("parseBlkidLine(%q) = (%q, %q), want (%q, %q)", tt.line, gotFS, gotPT, tt.wantFS, tt.wantPT)
		}
	}
}

func TestNewStorageManagerEMMCRootEnvOverride(t *testing.T) {
	m := NewStorageManager(StorageConfig{}, nil)
	m.Stop()
	if m.emmcRoot != "/userdata" {
		t.Fatalf("default emmc root = %q, want /userdata", m.emmcRoot)
	}

	t.Setenv(storageEMMCRootEnv, "/tmp/stmig/emmc")
	m = NewStorageManager(StorageConfig{}, nil)
	m.Stop()
	if m.emmcRoot != "/tmp/stmig/emmc" {
		t.Fatalf("overridden emmc root = %q, want /tmp/stmig/emmc", m.emmcRoot)
	}
}

func TestBuildMBRSector(t *testing.T) {
	const totalSectors = 62333952 // ~29.7 GiB card
	sector, err := buildMBRSector(totalSectors, 0x0C)
	if err != nil {
		t.Fatal(err)
	}
	if len(sector) != 512 {
		t.Fatalf("sector length = %d, want 512", len(sector))
	}
	if sector[510] != 0x55 || sector[511] != 0xAA {
		t.Fatal("missing MBR boot signature")
	}
	entry := sector[446:462]
	if entry[0] != 0x00 {
		t.Fatal("partition marked bootable")
	}
	if entry[4] != 0x0C {
		t.Fatalf("partition type = %#x, want 0x0C", entry[4])
	}
	start := uint32(entry[8]) | uint32(entry[9])<<8 | uint32(entry[10])<<16 | uint32(entry[11])<<24
	count := uint32(entry[12]) | uint32(entry[13])<<8 | uint32(entry[14])<<16 | uint32(entry[15])<<24
	if start != mbrPartitionStartLBA {
		t.Fatalf("partition start = %d, want %d", start, mbrPartitionStartLBA)
	}
	if uint64(count) != totalSectors-mbrPartitionStartLBA {
		t.Fatalf("partition sectors = %d, want %d", count, totalSectors-mbrPartitionStartLBA)
	}
	// Bytes 2-445 stay zero so no stale bootstrap code survives.
	for i := 0; i < 446; i++ {
		if sector[i] != 0 {
			t.Fatalf("bootstrap area byte %d = %#x, want 0", i, sector[i])
		}
	}

	if _, err := buildMBRSector(4096, 0x0C); err == nil {
		t.Fatal("tiny device accepted")
	}
	if _, err := mbrPartitionType("ntfs"); err == nil {
		t.Fatal("unsupported fs accepted for partition type")
	}
}

func TestStorageManagerStateFileMirrorsStatus(t *testing.T) {
	ops := &fakeStorageOps{present: true, free: 1 << 30, total: 1 << 31, healthy: true}
	m := newTestStorageManager(t, ops)
	tickN(m, 2)

	data, err := os.ReadFile(m.statePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{"SD_MOUNTED=1", "EFFECTIVE_MODE=2", "SD_MOUNTPOINT=" + m.cfg.MountPointOrDefault()} {
		if !strings.Contains(content, want) {
			t.Fatalf("state file missing %q:\n%s", want, content)
		}
	}
}

func TestStorageStateViewReadsConfigWebMirrorWithoutStartingHardwarePoller(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "storage.state")
	if err := os.WriteFile(statePath, []byte("SD_PRESENT=1\nSD_MOUNTED=1\nSD_DEVICE=/dev/mmcblk2p1\nSD_MOUNTPOINT=/media/card\nSD_TOTAL_BYTES=100\nSD_FREE_BYTES=40\nFORMAT_STATUS=idle\nMIGRATE_STATUS=success\nMIGRATE_MOVED_FILES=3\nMIGRATE_MOVED_BYTES=64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view := NewStorageStateView(StorageConfig{}, statePath, nil)
	view.Start()
	status := view.Status()
	if !status.Card.Present || !status.Card.Mounted || status.MountPoint != "/media/card" || status.Migration.MovedFiles != 3 {
		t.Fatalf("status=%+v", status)
	}
	roots := view.ReadRoots(StorageClassAudio)
	if len(roots) != 2 || roots[1] != "/media/card/aico/audio" {
		t.Fatalf("roots=%v", roots)
	}
	view.Stop()
}

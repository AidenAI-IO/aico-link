package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"aico-agent/internal/logging"
)

func TestDiscoverSDCard(t *testing.T) {
	for _, cardName := range []string{"mmcblk1", "mmcblk2"} {
		t.Run(cardName, func(t *testing.T) {
			root := t.TempDir()
			add := func(name, cardType string) {
				t.Helper()
				dir := filepath.Join(root, name, "device")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "type"), []byte(cardType+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			add("mmcblk0", "MMC")
			add("mmcblk0boot0", "MMC")
			add("mmcblk9", "SDIO")
			if got := discoverSDCard(root); got != "" {
				t.Fatalf("discovered non-SD device %q", got)
			}
			add(cardName, "SD")
			if got := discoverSDCard(root); got != cardName {
				t.Fatalf("got %q, want %q", got, cardName)
			}
			add("mmcblk8", "SD")
			if got := discoverSDCard(root); got != "" {
				t.Fatalf("ambiguous cards selected %q", got)
			}
		})
	}
}

func TestStorageDeviceDefaultsAndOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("[storage_settings.storage]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadResolvedConfigForUpdate(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Storage.DeviceOrDefault(); got != "auto" {
		t.Fatalf("default device = %q", got)
	}
	if err := os.WriteFile(path, []byte("[storage_settings.storage]\ndevice = \"mmcblk2\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadResolvedConfigForUpdate(path)
	if err != nil {
		t.Fatal(err)
	}
	ops := &realStorageOps{device: cfg.Storage.DeviceOrDefault()}
	if got := ops.currentDevice(); got != "mmcblk2" {
		t.Fatalf("explicit device = %q", got)
	}
}

func TestFormatDiskRequiresToolBeforeDeviceAccess(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ops := &realStorageOps{device: "nonexistent-test-card"}
	for _, fs := range []string{StorageFormatFAT32, StorageFormatExt4, StorageFormatExFAT} {
		if _, err := ops.FormatDisk(fs); err == nil || !strings.Contains(err.Error(), "formatting tool") {
			t.Fatalf("%s: expected tool preflight before device access, got %v", fs, err)
		}
	}
}

func TestStorageFormatCommandForcesFAT32(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for _, tc := range []struct {
		fs, tool string
		args     []string
	}{
		{StorageFormatFAT32, "mkfs.vfat", []string{"-F", "32", "-n", "AICO"}},
		{StorageFormatExt4, "mkfs.ext4", []string{"-F", "-L", "AICO"}},
		{StorageFormatExFAT, "mkfs.exfat", []string{"-L", "AICO"}},
	} {
		path := filepath.Join(dir, tc.tool)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		tool, args, err := storageFormatCommand(tc.fs)
		if err != nil || tool != path || !reflect.DeepEqual(args, tc.args) {
			t.Fatalf("%s: %q %v, %v", tc.fs, tool, args, err)
		}
	}
}

func TestStorageWriteProbePreservesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".aico-write-probe")
	if err := os.WriteFile(path, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storageWriteProbe(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "user data" {
		t.Fatalf("existing file changed: %q, %v", data, err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("probe files leaked: %v, %v", files, err)
	}
	if err := storageWriteProbe(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("probe accepted unavailable storage")
	}
}

func TestStorageCommandDeadlineKillsChildren(t *testing.T) {
	start := time.Now()
	_, err := runStorageCommand(100*time.Millisecond, "/bin/sh", "-c", "sleep 30 & wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("command or child outlived deadline: %s", elapsed)
	}
}

func TestBlankCardSignatureCheckFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		command string
		absent  bool
	}{
		{"exit 2", true},
		{"exit 0", false},
		{"exit 8", false},
		{"echo 'Input/output error' >&2; exit 2", false},
		{"echo 'TYPE=filesystem'; exit 0", false},
	} {
		out, err := runStorageCommand(time.Second, "/bin/sh", "-c", tc.command)
		if got := blkidReportsNoSignature(out, err); got != tc.absent {
			t.Fatalf("%q: absent=%t, want %t", tc.command, got, tc.absent)
		}
	}
	if blkidReportsNoSignature(nil, context.DeadlineExceeded) || blkidReportsNoSignature(nil, os.ErrNotExist) {
		t.Fatal("unverified signature check accepted")
	}
}

func TestStoragePreparePreservesIOFailure(t *testing.T) {
	dir := storageCommandFixture(t, map[string]string{
		"fsck.vfat": "echo 'Input/output error' >&2\nexit 8",
		"mount":     "echo 'MOUNT MUST NOT RUN' > \"$AICO_STORAGE_TEST_TRACE\"\nexit 1",
	})
	trace := filepath.Join(dir, "commands")
	t.Setenv("AICO_STORAGE_TEST_TRACE", trace)
	err := (&realStorageOps{}).Prepare("/dev/nonexistent-test-card", filepath.Join(dir, "mountpoint"))
	if !errors.Is(err, errStorageIO) || !strings.Contains(err.Error(), "Input/output error") || strings.Contains(err.Error(), "format the card") {
		t.Fatalf("I/O failure lost or replaced by format advice: %v", err)
	}
	if _, err := os.Stat(trace); !os.IsNotExist(err) {
		t.Fatalf("mount ran after checker I/O failure: %v", err)
	}
}

// Fake only the commands under test; no test mounts or unmounts a filesystem.
func storageCommandFixture(t *testing.T, commands map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if _, ok := commands["blkid"]; !ok {
		commands["blkid"] = `echo '/dev/test: TYPE="vfat"'`
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	previous := blkidCandidates
	blkidCandidates = []string{filepath.Join(dir, "blkid")}
	t.Cleanup(func() { blkidCandidates = previous })
	return dir
}

func TestStoragePrepareCheckerExitStatus(t *testing.T) {
	for _, fsType := range []string{"vfat", "ext4", "exfat"} {
		for _, status := range []int{0, 1, 2, 3, 4, 5, 8, 9, 16, 32, 128} {
			t.Run(fmt.Sprintf("%s/%d", fsType, status), func(t *testing.T) {
				dir := storageCommandFixture(t, map[string]string{
					"blkid":          fmt.Sprintf(`echo '/dev/test: TYPE="%s"'`, fsType),
					"fsck." + fsType: fmt.Sprintf("echo checker >> \"$AICO_STORAGE_TEST_TRACE\"\nexit %d", status),
					"mount":          "echo mount >> \"$AICO_STORAGE_TEST_TRACE\"",
				})
				trace := filepath.Join(dir, "commands")
				t.Setenv("AICO_STORAGE_TEST_TRACE", trace)
				err := (&realStorageOps{}).Prepare("/dev/test", filepath.Join(dir, "mountpoint"))
				wantMounted := status <= 3 && (fsType != "vfat" || status <= 1)
				if (err == nil) != wantMounted {
					t.Fatalf("fsck exit %d: Prepare = %v", status, err)
				}
				commands, readErr := os.ReadFile(trace)
				if readErr != nil || !strings.Contains(string(commands), "checker\n") || strings.Contains(string(commands), "mount\n") != wantMounted {
					t.Fatalf("fsck exit %d: commands = %q, %v", status, commands, readErr)
				}
			})
		}
	}
}

func TestStoragePrepareFallsBackWithoutChecker(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-filesystem=%t", unknown), func(t *testing.T) {
			commands := map[string]string{"mount": "echo mount >> \"$AICO_STORAGE_TEST_TRACE\""}
			if unknown {
				commands["blkid"] = "exit 2"
			}
			dir := storageCommandFixture(t, commands)
			trace := filepath.Join(dir, "commands")
			t.Setenv("AICO_STORAGE_TEST_TRACE", trace)
			ops := &realStorageOps{}
			if err := ops.Prepare("/dev/test", filepath.Join(dir, "mountpoint")); err != nil {
				t.Fatalf("missing checker rejected the mount/probe gate: %v", err)
			}
			if out, err := os.ReadFile(trace); err != nil || string(out) != "mount\n" {
				t.Fatalf("mount was not attempted: %q, %v", out, err)
			}
			// Falling back still requires a successful mount, even without fsck.
			if err := os.WriteFile(filepath.Join(dir, "mount"), []byte("#!/bin/sh\necho 'mount rejected' >&2\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := ops.Prepare("/dev/test", filepath.Join(dir, "mountpoint")); err == nil || !strings.Contains(err.Error(), "mount rejected") {
				t.Fatalf("missing checker bypassed failed mount: %v", err)
			}
		})
	}
}

func TestStorageUnmountContinuesAfterSyncFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lazy        bool
		unmountExit int
	}{
		{name: "eject-or-format"},
		{name: "busy-mount", unmountExit: 1},
		{name: "lazy", lazy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := storageCommandFixture(t, map[string]string{
				"sync":   "echo sync >> \"$AICO_STORAGE_TEST_TRACE\"\necho 'Input/output error' >&2\nexit 1",
				"umount": fmt.Sprintf("echo umount >> \"$AICO_STORAGE_TEST_TRACE\"\necho 'target busy' >&2\nexit %d", tc.unmountExit),
			})
			trace := filepath.Join(dir, "commands")
			t.Setenv("AICO_STORAGE_TEST_TRACE", trace)
			var output bytes.Buffer
			defer logging.SetOutput(&output)()
			ops := &realStorageOps{}
			if !ops.IsMounted("/") {
				t.Fatal("test requires the Linux root mount")
			}
			err := ops.Unmount("/", tc.lazy)
			if (err != nil) != (!tc.lazy || tc.unmountExit != 0) {
				t.Fatalf("Unmount = %v, lazy=%t, umount exit=%d", err, tc.lazy, tc.unmountExit)
			}
			if tc.unmountExit != 0 && (!strings.Contains(err.Error(), "target busy") || errors.Is(err, ErrStorageFlush)) {
				t.Fatalf("umount failure lost: %v", err)
			}
			if !tc.lazy && tc.unmountExit == 0 && (!errors.Is(err, ErrStorageFlush) || !strings.Contains(err.Error(), "Input/output error")) {
				t.Fatalf("successful detach lost flush warning: %v", err)
			}
			want := "umount\n"
			if !tc.lazy {
				want = "sync\n" + want
				if !strings.Contains(output.String(), "Input/output error") {
					t.Fatalf("sync failure was not logged: %s", output.String())
				}
			}
			if out, err := os.ReadFile(trace); err != nil || string(out) != want {
				t.Fatalf("commands = %q, %v, want %q", out, err, want)
			}
		})
	}
}

func TestStorageFlushFailureAllowsRecoveryButWarnsOnEject(t *testing.T) {
	for _, action := range []string{"eject", "format", "reconfigure"} {
		t.Run(action, func(t *testing.T) {
			ops := &fakeStorageOps{present: true, healthy: true, free: 1 << 30, total: 1 << 31}
			m := newTestStorageManager(t, ops)
			tickN(m, 2)
			ops.unmountErr = fmt.Errorf("%w: %w", ErrStorageFlush, syscall.EIO)
			switch action {
			case "eject":
				err := m.SafeEject()
				if !errors.Is(err, ErrStorageFlush) || !strings.Contains(err.Error(), "pending writes") || strings.Contains(err.Error(), "close any shell") || strings.Count(err.Error(), "input/output error") != 1 {
					t.Fatalf("SafeEject hid or misreported the failed flush: %v", err)
				}
				if status := m.Status(); status.Card.Mounted || status.EffectiveMode != StorageModeEMMCOnly || !m.ejected {
					t.Fatalf("detachment after failed flush did not update state: %+v", status)
				}
				tickN(m, 2)
				if ops.prepares != 1 {
					t.Fatal("card remounted after eject with failed flush")
				}
			case "format":
				if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err != nil {
					t.Fatalf("flush error blocked formatting after detach: %v", err)
				}
				if job := waitForFormatJob(t, m); job.Status != StorageFormatSuccess || ops.formats != 1 {
					t.Fatalf("format recovery failed: %+v", job)
				}
			case "reconfigure":
				cfg := m.cfg
				cfg.MountPoint = filepath.Join(t.TempDir(), "new-mount")
				if err := m.Reconfigure(cfg); err != nil {
					t.Fatalf("flush warning blocked reconfiguration after detach: %v", err)
				}
			}
			// Keep the warning visible even after a new mount or configuration
			// replaces the card status, including the Agent's read-only mirror.
			tickN(m, 2)
			view := NewStorageStateView(m.cfg, m.statePath, nil)
			for _, status := range []StorageStatus{m.Status(), view.Status()} {
				if !strings.Contains(status.LastWarning, "pending writes") || !strings.Contains(status.LastWarning, "input/output error") {
					t.Fatalf("flush warning was lost from status: %+v", status)
				}
			}
		})
	}
}

func TestStorageMountDeviceAliases(t *testing.T) {
	dir := t.TempDir()
	device := filepath.Join(dir, "device")
	other := filepath.Join(dir, "other")
	alias := filepath.Join(dir, "by-uuid")
	for _, path := range []string{device, other} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(device, alias); err != nil {
		t.Fatal(err)
	}
	if !sameStorageDevice(alias, device) || !sameStorageDevice(device, alias) || sameStorageDevice(other, device) || sameStorageDevice(filepath.Join(dir, "missing"), device) {
		t.Fatal("mount source identity did not distinguish aliases from other devices")
	}
}

func TestStorageHealthCapacityFailureKeepsCardMounted(t *testing.T) {
	for _, failure := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT} {
		t.Run(failure.Error(), func(t *testing.T) {
			ops := &fakeStorageOps{present: true, healthy: true, free: 1 << 30, total: 1 << 31}
			m := newTestStorageManager(t, ops)
			tickN(m, 2)
			ops.probeErr = fmt.Errorf("write probe: %w", failure)
			if failure == syscall.ENOSPC {
				ops.free = 0
			}
			m.tick()
			status := m.Status()
			if !status.Card.Mounted || status.EffectiveMode != StorageModeDual || m.mountFailed || ops.unmounts != 0 || ops.probes != 1 {
				t.Fatalf("capacity failure detached a healthy card: %+v, probes=%d", status, ops.probes)
			}
			if status.Card.FreeBytes != ops.free || !strings.Contains(status.Card.Reason, "capacity exhausted") {
				t.Fatalf("capacity was not reported: %+v", status.Card)
			}
			ops.probeErr = nil
			ops.free = 1 << 30
			m.lastHealthProbe = time.Now().Add(-time.Minute)
			m.tick()
			if status := m.Status(); !status.Card.Mounted || status.Card.Reason != "" || ops.prepares != 1 || ops.probes != 2 {
				t.Fatalf("capacity recovery required reinsertion: %+v", status)
			}
		})
	}
}

func TestStorageMigrationPreservesCardReserve(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true}
	m := newTestStorageManager(t, ops)
	srcDir := filepath.Join(m.emmcRoot, "audio")
	dstDir := filepath.Join(m.cfg.MountPointOrDefault(), storageSDSubdir, "audio")
	for i, name := range []string{"a.wav", "b.wav", "c.wav"} {
		writeAgedFile(t, filepath.Join(srcDir, name), 100, time.Duration(3-i)*time.Hour)
	}
	reserve := int64(m.cfg.MinCardFreeMBOrDefault()) * 1024 * 1024
	ops.spaceFn = func(path string) (int64, int64, error) {
		if path == m.emmcRoot {
			return 50, 1000, nil // eMMC stays below the start/stop watermarks
		}
		return reserve + 150 - dirBytes(t, dstDir), 1 << 30, nil
	}
	tickN(m, 2)
	job := waitForMigration(t, m)
	if job.Status != StorageFormatFailed || job.MovedFiles != 1 || job.MovedBytes != 100 || !strings.Contains(job.Detail, "insufficient free space") {
		t.Fatalf("migration did not stop before crossing the SD reserve: %+v", job)
	}
	for _, name := range []string{"b.wav", "c.wav"} {
		if _, err := os.Stat(filepath.Join(srcDir, name)); err != nil {
			t.Fatalf("unmigrated source %s lost: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dstDir, name)); !os.IsNotExist(err) {
			t.Fatalf("copy %s created without enough SD space: %v", name, err)
		}
	}
	tickN(m, 2)
	if status := m.Status(); !status.Card.Mounted || !status.Migration.FinishedAt.Equal(job.FinishedAt) {
		t.Fatalf("reserve exhaustion detached card or immediately retried: %+v", status)
	}
}

func TestStorageNeverAutoFormatsAfterIOFailure(t *testing.T) {
	for _, prepareErr := range []error{errStorageIO, syscall.EIO, context.DeadlineExceeded} {
		ops := &fakeStorageOps{present: true, blank: true, prepareErr: prepareErr}
		manager := newStorageManagerWithOps(StorageConfig{}, ops, "", t.TempDir(), nil)
		manager.tick()
		manager.tick()
		if status := manager.Status(); status.FormatJob.Status == StorageFormatRunning || ops.formats != 0 {
			t.Fatalf("auto-format after %v: %+v", prepareErr, status)
		}
	}
}

func TestStorageLogsWithoutAgentLogger(t *testing.T) {
	var output bytes.Buffer
	defer logging.SetOutput(&output)()
	manager := &StorageManager{}
	manager.logf("card detected")
	manager.warnf("card failed")
	if !strings.Contains(output.String(), "[INFO][config_web][storage]") || !strings.Contains(output.String(), "[WARN][config_web][storage]") {
		t.Fatalf("storage logs missing: %s", output.String())
	}
}

type blockedStorageHealth struct {
	fakeStorageOps
	entered chan struct{}
	unblock chan struct{}
}

func (o *blockedStorageHealth) VerifyWritable(string) error {
	close(o.entered)
	<-o.unblock
	return syscall.EIO
}

func TestStorageHealthProbeKeepsStatusResponsive(t *testing.T) {
	ops := &blockedStorageHealth{
		fakeStorageOps: fakeStorageOps{present: true, mounted: true, healthy: true},
		entered:        make(chan struct{}), unblock: make(chan struct{}),
	}
	m := newStorageManagerWithOps(StorageConfig{}, ops, "", t.TempDir(), nil)
	m.card = StorageCardStatus{Present: true, Mounted: true, Device: "/dev/mmcblk2p1"}
	tickDone := make(chan struct{})
	go func() { defer close(tickDone); m.tick() }()
	select {
	case <-ops.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("health probe did not start")
	}
	statusDone := make(chan struct{})
	go func() { m.Status(); close(statusDone) }()
	select {
	case <-statusDone:
	case <-time.After(2 * time.Second):
		close(ops.unblock)
		t.Fatal("Status blocked during health I/O")
	}
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err == nil || !strings.Contains(err.Error(), "card health check") {
		close(ops.unblock)
		t.Fatal("format raced health probe")
	}
	close(ops.unblock)
	<-tickDone
	status := m.Status()
	if status.Card.Mounted || !strings.Contains(status.Card.Reason, "input/output error") {
		t.Fatalf("failed health probe accepted: %+v", status)
	}
	prepares := ops.prepares
	m.tick()
	if ops.prepares != prepares {
		t.Fatal("failed card remounted without reinsertion")
	}
}

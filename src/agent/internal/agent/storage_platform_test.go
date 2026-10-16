package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"aiden-agent/internal/logging"
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
		{StorageFormatFAT32, "mkfs.vfat", []string{"-F", "32", "-n", "AIDEN"}},
		{StorageFormatExt4, "mkfs.ext4", []string{"-F", "-L", "AIDEN"}},
		{StorageFormatExFAT, "mkfs.exfat", []string{"-L", "AIDEN"}},
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
	path := filepath.Join(dir, ".aiden-write-probe")
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
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "fsck"), []byte("#!/bin/sh\necho 'Input/output error' >&2\nexit 8\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mount"), []byte("#!/bin/sh\necho 'MOUNT MUST NOT RUN' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&realStorageOps{}).Prepare("/dev/nonexistent-test-card", filepath.Join(dir, "mountpoint"))
	if !errors.Is(err, errStorageIO) || !strings.Contains(err.Error(), "Input/output error") || strings.Contains(err.Error(), "format the card") {
		t.Fatalf("I/O failure lost or replaced by format advice: %v", err)
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
	if err := m.StartFormat(StorageFormatFAT32, StorageFormatConfirmToken); err == nil {
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

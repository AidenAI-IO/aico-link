package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageManagerPreservesPendingRestoreLayout(t *testing.T) {
	ops := &fakeStorageOps{present: true, healthy: true}
	m := newTestStorageManagerWithWatermarks(t, ops, 10, 30)
	mountPoint := m.cfg.MountPointOrDefault()
	journal := filepath.Join(mountPoint, ".aico-restore", "pending", "transaction.json")
	writeAgedFile(t, journal, 123, time.Hour)
	srcDir := m.emmcClassDir(StorageClassAudio)
	writeAgedFile(t, filepath.Join(srcDir, "clip.wav"), 300, time.Hour)
	ops.spaceFn = migrationSpaceFn(t, m, srcDir, 40, 300)
	tickN(m, 3)
	if job := m.Status().Migration; job.Status != StorageFormatIdle {
		t.Fatalf("migration interfered with pending restore: %+v", job)
	}
	if got := m.CleanupRoots(StorageClassAudio); len(got) != 0 {
		t.Fatalf("cleanup exposed pending restore targets: %v", got)
	}
	if info, err := os.Stat(journal); err != nil || info.Size() != 123 {
		t.Fatalf("mount changed restore journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountPoint, "aico")); !os.IsNotExist(err) {
		t.Fatalf("mount created managed tree during recovery: %v", err)
	}
	if err := os.RemoveAll(filepath.Dir(journal)); err != nil {
		t.Fatal(err)
	}
	tickN(m, 1)
	if job := waitForMigration(t, m); job.Status != StorageFormatSuccess || job.MovedFiles != 1 {
		t.Fatalf("migration did not resume after recovery: %+v", job)
	}
}

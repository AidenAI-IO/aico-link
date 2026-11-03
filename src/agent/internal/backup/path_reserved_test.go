package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReservedSDUserRootsIncludeFATAliases(t *testing.T) {
	for _, value := range []string{
		"aico", "AICO/audio/clip.wav",
		".AICO-RESTORE.../job/new/file",
		"AICO-R~1/job/new/file", "aico-~12/job/new/file",
		"AICO-R~1.../job/new/file", "AICO-R~1.DIR/job/new/file",
		"AICO-RES/job/new/file", "NOTES~1/file",
	} {
		if !IsReservedSDUserPath(value) {
			t.Errorf("reserved FAT alias accepted: %q", value)
		}
	}
	for _, value := range []string{
		"aico-notes/file", ".aico-restored/file",
		"notes/aico/file", "notes/.aico-restore/file", "notes/AICO-R~1/file",
		"notes~daily/file", "notes~1234/file", "aico-r~1extra/file", "aico-r~1.long/file",
	} {
		if IsReservedSDUserPath(value) {
			t.Errorf("unreserved user path rejected: %q", value)
		}
	}
}

func TestSDUserBackupExcludesFATAliasesWithoutChangingFiles(t *testing.T) {
	for _, rootName := range []string{
		"AICO", ".AICO-RESTORE...",
		"AICO-R~1", "AICO-RES", "NOTES~1",
	} {
		t.Run(rootName, func(t *testing.T) {
			roots := testRoots(t)
			reserved := filepath.Join(roots.SD, rootName, "sentinel.txt")
			writeTestFile(t, reserved, "preserve", 0o600)
			writeTestFile(t, filepath.Join(roots.SD, "notes", "readme.txt"), "user notes", 0o600)
			plan, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{
				Mode: ModePortable, Components: []ComponentID{ComponentSDUserFiles}, Storage: StorageIdentity{SDPresent: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Entries) != 2 {
				t.Fatalf("managed alias entered user backup: %+v", plan.Entries)
			}
			for _, entry := range plan.Entries {
				if entry.Manifest.Path != "notes" && entry.Manifest.Path != "notes/readme.txt" {
					t.Fatalf("managed alias entered user backup: %+v", entry)
				}
			}
			if data, err := os.ReadFile(reserved); err != nil || string(data) != "preserve" {
				t.Fatalf("backup changed reserved source: %q, %v", data, err)
			}
		})
	}
}

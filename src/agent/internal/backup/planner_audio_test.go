package backup

import (
	"context"
	"path/filepath"
	"testing"
)

func TestManagedAudioBackupExcludesPartialsAndDeduplicatesEMMC(t *testing.T) {
	roots := testRoots(t)
	writeTestFile(t, filepath.Join(roots.Userdata, "audio/clip.wav"), "recording", 0o600)
	writeTestFile(t, filepath.Join(roots.SD, "aico/audio/clip.wav"), "recording", 0o600)
	writeTestFile(t, filepath.Join(roots.SD, "aico/audio/nested/new.wav.aico-partial"), "incomplete", 0o600)
	plan, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentAudioArchive, ComponentSDManagedAudio},
		Storage: StorageIdentity{SDPresent: true, SDUUID: "test-card"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := regularAudioEntries(plan)
	if len(entries) != 1 || entries[0].Manifest.Path != "clip.wav" || entries[0].Manifest.Component != ComponentAudioArchive {
		t.Fatalf("partial filtering or eMMC deduplication failed: %+v", entries)
	}
}

func TestSDUserBackupExcludesManagedRoots(t *testing.T) {
	roots := testRoots(t)
	for _, name := range []string{"aico/audio/clip.wav", ".aico-restore/job/transaction.json", "notes/readme.txt"} {
		writeTestFile(t, filepath.Join(roots.SD, name), "preserve", 0o600)
	}
	plan, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentSDUserFiles}, Storage: StorageIdentity{SDPresent: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range plan.Entries {
		if entry.Manifest.Path != "notes" && entry.Manifest.Path != "notes/readme.txt" {
			t.Fatalf("user backup included managed path: %+v", entry)
		}
	}
	if len(plan.Entries) != 2 {
		t.Fatalf("user files missing: %+v", plan.Entries)
	}
}

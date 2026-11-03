package configweb

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/backup"
)

func TestRestorePlanRejectsSDUserPathsOverlappingManagedRoots(t *testing.T) {
	for _, archivePath := range []string{
		"aico", "aico/audio/clip.wav", "AICO/audio/clip.wav",
		"aico.", "aico./audio/clip.wav", "AICO.../audio/clip.wav",
		".aico-restore", ".aico-restore/job/new/file", ".AICO-RESTORE/job/new/file",
		".aico-restore.", ".AICO-RESTORE.../job/new/file",
		"AICO-R~1/job/new/file", "aico-~12/job/new/file",
		"AICO-R~1.../job/new/file", "AICO-R~1.DIR/job/new/file", "NOTES~1/file",
		"AICO-RES/job/new/file",
	} {
		t.Run(archivePath, func(t *testing.T) {
			root := t.TempDir()
			server := &Server{options: Options{
				BackupUserdataRoot: filepath.Join(root, "userdata"),
				BackupSDRoot:       filepath.Join(root, "sd"),
			}}
			sentinels := []string{"aico/audio/current.wav", ".aico-restore/existing/transaction.json"}
			for _, relative := range sentinels {
				filename := filepath.Join(server.options.BackupSDRoot, relative)
				if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte("preserve"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A user-files directory exchange must never replace managed audio
			// or the transaction staging directory.
			manifest := backup.Manifest{Files: []backup.FileManifest{
				{Component: backup.ComponentSDManagedAudio, Path: "clip.wav", Type: backup.FileTypeRegular},
				{Component: backup.ComponentSDUserFiles, Path: archivePath, Type: backup.FileTypeDirectory},
			}}
			selected := map[backup.ComponentID]bool{backup.ComponentSDManagedAudio: true, backup.ComponentSDUserFiles: true}
			units, err := server.buildRestoreUnits("reserved-path-test", manifest, selected, "sd")
			if err == nil || !strings.Contains(err.Error(), "reserved path") || units != nil {
				t.Fatalf("reserved-path preflight = %+v, %v", units, err)
			}
			for _, relative := range sentinels {
				data, err := os.ReadFile(filepath.Join(server.options.BackupSDRoot, relative))
				if err != nil || string(data) != "preserve" {
					t.Fatalf("preflight changed %s: %q, %v", relative, data, err)
				}
			}
			if _, err := os.Stat(backup.TransactionDir(server.options.BackupSDRoot, "reserved-path-test")); !os.IsNotExist(err) {
				t.Fatalf("preflight created staging: %v", err)
			}
		})
	}
}

func TestRestoreSDUserPathsRequireExactNormalizedRootMatches(t *testing.T) {
	for _, archivePath := range []string{".", "", "../aico", "./aico/audio", "notes/../aico/audio", "aico//audio", "/aico/audio"} {
		if _, err := restoreTargetSpecFor(backup.FileManifest{Component: backup.ComponentSDUserFiles, Path: archivePath}, "sd"); err == nil {
			t.Errorf("noncanonical SD user path accepted: %q", archivePath)
		}
	}
	for _, archivePath := range []string{"aico-notes/file", "aico-notes./file", ".aico-restored/file", "notes/aico/file", "notes/.aico-restore/file"} {
		spec, err := restoreTargetSpecFor(backup.FileManifest{Component: backup.ComponentSDUserFiles, Path: archivePath}, "sd")
		if err != nil || spec.rootRel != archivePath || spec.layer != backup.LayerSD {
			t.Errorf("unreserved SD user path %q = %+v, %v", archivePath, spec, err)
		}
	}
}

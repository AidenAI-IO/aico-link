package configweb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aico-agent/internal/backup"
)

func TestManagedAudioRestoreUsesAicoCardLayout(t *testing.T) {
	roots := backup.Roots{Userdata: t.TempDir(), SD: t.TempDir()}
	server := &Server{options: Options{BackupUserdataRoot: roots.Userdata, BackupSDRoot: roots.SD}}
	manifest := backup.Manifest{Files: []backup.FileManifest{{Component: backup.ComponentSDManagedAudio, Path: "session/clip.wav", Type: backup.FileTypeRegular}}}
	selected := map[backup.ComponentID]bool{backup.ComponentSDManagedAudio: true}
	units, err := server.buildRestoreUnits("audio-restore", manifest, selected, "sd")
	if err != nil {
		t.Fatal(err)
	}
	unit := units["sd:sd-managed-audio"]
	if unit == nil || unit.Target != filepath.Join(roots.SD, "aico", "audio") {
		t.Fatalf("restore target = %+v", unit)
	}
	staged := filepath.Join(unit.Staged, "session/clip.wav")
	if err := os.MkdirAll(filepath.Dir(staged), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("restored recording"), 0o600); err != nil {
		t.Fatal(err)
	}
	txn := backup.Transaction{JobID: "audio-restore", Layer: backup.LayerSD, RequiredLayers: []string{backup.LayerSD}, State: backup.TransactionCommitting, Units: []*backup.TransactionUnit{unit}}
	if err := txn.ValidatePaths(roots.SD); err != nil {
		t.Fatal(err)
	}
	if err := backup.WriteTransaction(roots.SD, txn); err != nil {
		t.Fatal(err)
	}
	results, err := backup.RecoverTransactions(context.Background(), backup.RecoveryOptions{Roots: roots})
	if err != nil || len(results) != 1 || results[0].Result != backup.RecoveryCommitted {
		t.Fatalf("restore recovery = %+v, %v", results, err)
	}
	if data, err := os.ReadFile(filepath.Join(unit.Target, "session/clip.wav")); err != nil || string(data) != "restored recording" {
		t.Fatalf("restored recording = %q, %v", data, err)
	}
	if _, err := os.Stat(backup.TransactionDir(roots.SD, "audio-restore")); !os.IsNotExist(err) {
		t.Fatalf("completed journal not cleaned: %v", err)
	}
	fallback, err := server.buildRestoreUnits("fallback", manifest, selected, "emmc_fallback")
	if err != nil || fallback["userdata:sd-audio-fallback"].Target != filepath.Join(roots.Userdata, "audio") {
		t.Fatalf("eMMC fallback = %+v, %v", fallback, err)
	}
}

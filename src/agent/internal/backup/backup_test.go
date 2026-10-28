package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func writeTestFile(t *testing.T, name, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func testRoots(t *testing.T) Roots {
	t.Helper()
	base := t.TempDir()
	return Roots{Userdata: filepath.Join(base, "userdata"), SD: filepath.Join(base, "sd")}
}

func TestArchiveRoundTripAndAuthentication(t *testing.T) {
	roots := testRoots(t)
	writeTestFile(t, filepath.Join(roots.Userdata, "agent/agent.toml"), "[basic_settings]\n", 0o600)
	writeTestFile(t, filepath.Join(roots.Userdata, "agent/memory/profile.txt"), "remember me\n", 0o640)
	if err := os.Symlink("profile.txt", filepath.Join(roots.Userdata, "agent/memory/current")); err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	planner := NewPlanner(roots)
	plan, err := planner.Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentAgentConfig, ComponentAgentMemory},
		Source:    SourceIdentity{MachineID: "must-not-leak", HardwareID: "must-not-leak", FirmwareVersion: "1.2.3"},
		CreatedAt: createdAt, BackupID: "backup-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.Source.MachineID != "" || plan.Manifest.Source.HardwareID != "" {
		t.Fatalf("portable manifest leaked device identity: %+v", plan.Manifest.Source)
	}

	passphrase := []byte("correct horse battery staple")
	material, err := NewKeyMaterial(passphrase, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	var archive bytes.Buffer
	if err := WriteArchive(context.Background(), &archive, plan, material, nil); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyArchive(context.Background(), bytes.NewReader(archive.Bytes()), passphrase)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.BackupID != "backup-test" || len(result.Manifest.Files) != 3 {
		t.Fatalf("unexpected verified manifest: %+v", result.Manifest)
	}

	if _, err := VerifyArchive(context.Background(), bytes.NewReader(archive.Bytes()), []byte("incorrect passphrase")); err == nil {
		t.Fatal("wrong passphrase unexpectedly verified")
	}
	truncated := archive.Bytes()[:archive.Len()-1]
	if _, err := VerifyArchive(context.Background(), bytes.NewReader(truncated), passphrase); err == nil || ErrorCode(err) != "archive_truncated" {
		t.Fatalf("truncated archive error = %v (%s)", err, ErrorCode(err))
	}
	tampered := append([]byte(nil), archive.Bytes()...)
	tampered[len(tampered)-20] ^= 0x80
	if _, err := VerifyArchive(context.Background(), bytes.NewReader(tampered), passphrase); err == nil {
		t.Fatal("tampered archive unexpectedly verified")
	}
}

func TestArchiveStreamWalksEveryEntryTypeAndDetectsWrongPassphrase(t *testing.T) {
	roots := testRoots(t)
	writeTestFile(t, filepath.Join(roots.Userdata, "agent/memory/long_term/profile.md"), "remember me\n", 0o600)
	if err := os.Symlink("long_term/profile.md", filepath.Join(roots.Userdata, "agent/memory/current")); err != nil {
		t.Fatal(err)
	}
	plan, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{Mode: ModePortable, Components: []ComponentID{ComponentAgentMemory}})
	if err != nil {
		t.Fatal(err)
	}
	passphrase := []byte("correct horse battery staple")
	material, err := NewKeyMaterial(passphrase, plan.Manifest.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	var archive bytes.Buffer
	if err := WriteArchive(context.Background(), &archive, plan, material, nil); err != nil {
		t.Fatal(err)
	}
	header, _, err := ReadPublicHeader(bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	restoreMaterial, err := DeriveKeyMaterial(header, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer restoreMaterial.Destroy()
	stream, err := NewArchiveStream(bytes.NewReader(archive.Bytes()), restoreMaterial)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := stream.ReadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 3 {
		t.Fatalf("manifest files = %d", len(manifest.Files))
	}
	var types []FileType
	for {
		entry, err := stream.Next(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, entry.Manifest.Type)
		var body bytes.Buffer
		if entry.Manifest.Type == FileTypeRegular {
			err = stream.CopyCurrentEntry(context.Background(), &body)
		} else {
			err = stream.CopyCurrentEntry(context.Background(), nil)
		}
		if err != nil {
			t.Fatalf("consume %s: %v", entry.Manifest.Path, err)
		}
		if entry.Manifest.Type == FileTypeRegular && body.String() != "remember me\n" {
			t.Fatalf("body = %q", body.String())
		}
	}
	if err := stream.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(types) != 3 {
		t.Fatalf("entry types = %v", types)
	}

	wrong, err := DeriveKeyMaterial(header, []byte("not the right passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Destroy()
	if _, err := NewArchiveStream(bytes.NewReader(archive.Bytes()), wrong); err == nil || ErrorCode(err) != "wrong_passphrase" {
		t.Fatalf("wrong passphrase stream error = %v (%s)", err, ErrorCode(err))
	}
}

func TestPlannerRejectsIdentityInPortableAndRequiresHardwareID(t *testing.T) {
	roots := testRoots(t)
	writeTestFile(t, filepath.Join(roots.Userdata, "system/machine-id"), strings.Repeat("a", 32), 0o600)
	planner := NewPlanner(roots)
	_, err := planner.Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentDeviceIdentity},
	})
	if err == nil || !strings.Contains(err.Error(), "portable") {
		t.Fatalf("portable device identity error = %v", err)
	}
	_, err = planner.Plan(context.Background(), PlanOptions{
		Mode: ModeSameDevice, Components: []ComponentID{ComponentDeviceIdentity},
	})
	if err == nil || !strings.Contains(err.Error(), "hardware_id") {
		t.Fatalf("missing hardware ID error = %v", err)
	}
}

func TestPlannerRejectsEscapingSymlinkAndSpecialFile(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, memory string)
	}{
		{name: "escaping symlink", setup: func(t *testing.T, memory string) {
			if err := os.Symlink("../secret", filepath.Join(memory, "escape")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "fifo", setup: func(t *testing.T, memory string) {
			if err := unix.Mkfifo(filepath.Join(memory, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := testRoots(t)
			memory := filepath.Join(roots.Userdata, "agent/memory")
			if err := os.MkdirAll(memory, 0o755); err != nil {
				t.Fatal(err)
			}
			test.setup(t, memory)
			_, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{
				Mode: ModePortable, Components: []ComponentID{ComponentAgentMemory},
			})
			if err == nil {
				t.Fatal("unsafe entry unexpectedly accepted")
			}
		})
	}
}

func TestOTASettingsAreWhitelistedAndCredentialPathsAreBounded(t *testing.T) {
	roots := testRoots(t)
	tokenPath := filepath.Join(roots.Userdata, "debian/ota/credentials/github-token")
	writeTestFile(t, tokenPath, "secret-token\n", 0o600)
	configPath := filepath.Join(roots.Userdata, "debian/ota/config.json")
	config := map[string]any{
		"manifest_url":             "https://example.test/manifest.json",
		"github_proxy_url":         "https://proxy.test/",
		"github_token_path":        tokenPath,
		"factory_version":          "must-not-copy",
		"factory_partition_hashes": map[string]string{"rootfs": "must-not-copy"},
		"state_dir":                "/userdata/ota/state",
	}
	data, _ := json.Marshal(config)
	writeTestFile(t, configPath, string(data), 0o600)
	plan, err := NewPlanner(roots).Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentOTASettings},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Entries) != 1 {
		t.Fatalf("OTA entries = %d, want 1", len(plan.Entries))
	}
	text := string(plan.Entries[0].Generated)
	for _, forbidden := range []string{"factory_version", "factory_partition_hashes", "state_dir", "must-not-copy", tokenPath} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("OTA backup leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "secret-token") || !strings.Contains(text, "manifest_url") {
		t.Fatalf("OTA user settings missing: %s", text)
	}

	config["github_token_path"] = filepath.Join(t.TempDir(), "outside-token")
	data, _ = json.Marshal(config)
	writeTestFile(t, configPath, string(data), 0o600)
	plan, err = NewPlanner(roots).Plan(context.Background(), PlanOptions{
		Mode: ModePortable, Components: []ComponentID{ComponentOTASettings},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan.Entries[0].Generated), "outside-token") {
		t.Fatal("unapproved credential path was serialized")
	}
}

func TestAudioCopiesAreDeduplicatedAndConflictsMarked(t *testing.T) {
	roots := testRoots(t)
	emmc := filepath.Join(roots.Userdata, "audio/clip.wav")
	sd := filepath.Join(roots.SD, "aico/audio/clip.wav")
	writeTestFile(t, emmc, "same", 0o600)
	writeTestFile(t, sd, "same", 0o600)
	options := PlanOptions{
		Mode:       ModePortable,
		Components: []ComponentID{ComponentAudioArchive, ComponentSDManagedAudio},
		Storage:    StorageIdentity{SDPresent: true, SDUUID: "sd-test"},
	}
	plan, err := NewPlanner(roots).Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	regular := regularAudioEntries(plan)
	if len(regular) != 1 || regular[0].Manifest.Component != ComponentAudioArchive {
		t.Fatalf("deduplicated entries = %+v", regular)
	}

	writeTestFile(t, sd, "different", 0o600)
	plan, err = NewPlanner(roots).Plan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	regular = regularAudioEntries(plan)
	if len(regular) != 2 || !regular[0].Manifest.Conflict || !regular[1].Manifest.Conflict {
		t.Fatalf("conflicting entries = %+v", regular)
	}
}

func regularAudioEntries(plan *Plan) []PlannedEntry {
	var result []PlannedEntry
	for _, entry := range plan.Entries {
		if entry.Manifest.Type == FileTypeRegular &&
			(entry.Manifest.Component == ComponentAudioArchive || entry.Manifest.Component == ComponentSDManagedAudio) {
			result = append(result, entry)
		}
	}
	return result
}

func TestManifestRejectsTamperedSummaryAndUnsafePath(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: 1, BackupID: "x", CreatedAt: time.Now(), Mode: ModePortable,
		Components: []ComponentManifest{{ID: ComponentAgentMemory, SchemaVersion: 1, FileCount: 1, ExpandedSize: 1, SHA256: strings.Repeat("0", 64)}},
		Files:      []FileManifest{{Component: ComponentAgentMemory, Path: "../escape", Type: FileTypeRegular, Mode: "0600", Size: 1, SHA256: strings.Repeat("0", 64)}},
	}
	if err := manifest.Validate(); err == nil {
		t.Fatal("unsafe manifest path unexpectedly accepted")
	}
	manifest.Files[0].Path = "safe"
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered component digest error = %v", err)
	}
}

func TestKDFLimitsAreCheckedBeforeDerivation(t *testing.T) {
	header := PublicHeader{
		Format: FormatName, Version: FormatVersion, CreatedAt: time.Now(),
		Protection: Protection{
			Algorithm: "xchacha20-poly1305-chunked", KDF: "argon2id",
			Salt: "AAAAAAAAAAAAAAAAAAAAAA==", NoncePrefix: "AAAAAAAAAAAAAAAAAAAAAA==",
			MemoryKiB: 1024 * 1024, Iterations: 3, Parallelism: 1, ChunkSize: DefaultChunkSize,
		},
	}
	_, err := DeriveKeyMaterial(header, []byte("long-enough-passphrase"))
	if err == nil || ErrorCode(err) != "unsupported_format" {
		t.Fatalf("oversized KDF error = %v (%s)", err, ErrorCode(err))
	}
}

func TestErrorCodeUnwraps(t *testing.T) {
	err := errorf("test_code", errors.New("cause"), "message")
	if ErrorCode(err) != "test_code" {
		t.Fatalf("ErrorCode = %q", ErrorCode(err))
	}
}

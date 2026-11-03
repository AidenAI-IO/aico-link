package ota

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonalizationSidecarRoundTripChecksumAndPublicStateIsolation(t *testing.T) {
	dir := t.TempDir()
	sidecarPath := filepath.Join(dir, "debian", "ota", "personalization-v1.json")
	statePath := filepath.Join(dir, "ota", "state.json")
	sidecar := PersonalizationSidecar{
		TransactionID:   "0123456789abcdef",
		TargetVersion:   "20260521-120000-abcdef0",
		TargetBuildTime: "2026-05-21T12:00:00Z",
		Slots: map[string]RootFSPersonalization{
			"b": {
				ArtifactSHA256:           testHashA,
				PersonalizationSchema:    PersonalizationSchemaVersion,
				EffectivePartitionSHA256: testHashB,
				HashedBytes:              16 << 20,
			},
		},
	}
	if err := SavePersonalizationSidecar(sidecarPath, sidecar); err != nil {
		t.Fatalf("SavePersonalizationSidecar() error = %v", err)
	}
	before, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("ReadFile(sidecar) error = %v", err)
	}
	loaded, err := LoadPersonalizationSidecar(sidecarPath)
	if err != nil {
		t.Fatalf("LoadPersonalizationSidecar() error = %v", err)
	}
	if loaded.ChecksumSHA256 == "" || loaded.TransactionID != sidecar.TransactionID {
		t.Fatalf("loaded sidecar = %+v", loaded)
	}
	if loaded.Slots["b"] != sidecar.Slots["b"] {
		t.Fatalf("loaded slot B = %+v, want %+v", loaded.Slots["b"], sidecar.Slots["b"])
	}

	// Simulate an old Buildroot Agent loading and rewriting only the public
	// state file. The Debian sidecar must remain byte-for-byte independent.
	if err := SaveState(statePath, NewFactoryState("factory", "2026-05-21T10:00:00Z", uniformFactoryPartitionHashes(testHashA))); err != nil {
		t.Fatalf("SaveState(factory) error = %v", err)
	}
	publicState, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	publicState.Phase = "legacy-rewrite"
	if err := SaveState(statePath, publicState); err != nil {
		t.Fatalf("SaveState(legacy rewrite) error = %v", err)
	}
	after, err := os.ReadFile(sidecarPath)
	if err != nil {
		t.Fatalf("ReadFile(sidecar after public rewrite) error = %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("public state rewrite modified the Debian personalization sidecar")
	}
}

func TestPersonalizationSidecarRejectsChecksumTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "personalization-v1.json")
	sidecar := PersonalizationSidecar{
		TransactionID:   "0123456789abcdef",
		TargetVersion:   "v2",
		TargetBuildTime: "2026-05-21T12:00:00Z",
		Slots: map[string]RootFSPersonalization{
			"a": {
				ArtifactSHA256:           testHashA,
				PersonalizationSchema:    PersonalizationSchemaVersion,
				EffectivePartitionSHA256: testHashB,
				HashedBytes:              4096,
			},
		},
	}
	if err := SavePersonalizationSidecar(path, sidecar); err != nil {
		t.Fatalf("SavePersonalizationSidecar() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	tampered := strings.Replace(string(data), testHashB, testHashC, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatalf("WriteFile(tampered) error = %v", err)
	}
	if _, err := LoadPersonalizationSidecar(path); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("LoadPersonalizationSidecar() error = %v, want checksum failure", err)
	}
}

func TestPersonalizeExt4MachineIDAndHashEffectiveImage(t *testing.T) {
	imagePath := newGenericExt4RootFS(t, "")
	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("Stat(image) error = %v", err)
	}
	genericHash, err := hashFilePrefix(imagePath, info.Size())
	if err != nil {
		t.Fatalf("hashFilePrefix(generic) error = %v", err)
	}
	machineID := "0123456789abcdef0123456789abcdef"
	debugfs, e2fsck := requireE2fsprogs(t, "debugfs"), requireE2fsprogs(t, "e2fsck")
	effectiveHash, err := personalizeExt4MachineID(
		imagePath,
		machineID,
		info.Size(),
		debugfs,
		e2fsck,
		runPersonalizationCommand,
	)
	if err != nil {
		t.Fatalf("personalizeExt4MachineID() error = %v", err)
	}
	if effectiveHash == genericHash {
		t.Fatalf("effective hash = generic hash %s after personalization", effectiveHash)
	}
	cat, err := runPersonalizationCommand(debugfs, "-R", "cat /etc/machine-id", imagePath)
	if err != nil {
		t.Fatalf("debugfs cat error = %v stderr=%s", err, cat.Stderr)
	}
	if string(cat.Stdout) != machineID+"\n" {
		t.Fatalf("personalized machine-id = %q", cat.Stdout)
	}
	if _, err := runPersonalizationCommand(e2fsck, "-f", "-n", imagePath); err != nil {
		t.Fatalf("post-personalization e2fsck error = %v", err)
	}
}

func TestPersonalizeExt4RejectsNonGenericMachineID(t *testing.T) {
	imagePath := newGenericExt4RootFS(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")
	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("Stat(image) error = %v", err)
	}
	_, err = personalizeExt4MachineID(
		imagePath,
		"0123456789abcdef0123456789abcdef",
		info.Size(),
		requireE2fsprogs(t, "debugfs"),
		requireE2fsprogs(t, "e2fsck"),
		runPersonalizationCommand,
	)
	if err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("personalizeExt4MachineID() error = %v, want non-generic rejection", err)
	}
}

func TestDebianUpdaterPersonalizesInactiveRootFSAndCommitsSidecarBeforeActivation(t *testing.T) {
	env := newUpdaterTestEnv(t)
	genericImagePath := newGenericExt4RootFS(t, "")
	genericRootFS, err := os.ReadFile(genericImagePath)
	if err != nil {
		t.Fatalf("ReadFile(generic rootfs) error = %v", err)
	}
	assets := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": genericRootFS,
	}
	manifest := env.signedManifest(assets, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": genericRootFS,
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	env.config.DebianMode = true
	env.config.DebugfsPath = requireE2fsprogs(t, "debugfs")
	env.config.E2fsckPath = requireE2fsprogs(t, "e2fsck")
	env.config.MachineIDPath = filepath.Join(t.TempDir(), "machine-id")
	env.config.PersonalizationPath = filepath.Join(t.TempDir(), "debian", "ota", "personalization-v1.json")
	machineID := "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(env.config.MachineIDPath, []byte(machineID+"\n"), 0o444); err != nil {
		t.Fatalf("WriteFile(persistent machine-id) error = %v", err)
	}

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v", result)
	}
	pendingData, err := os.ReadFile(filepath.Join(env.stateDir, "pending_boot.json"))
	if err != nil {
		t.Fatalf("ReadFile(pending_boot.json) error = %v", err)
	}
	var pending PendingBoot
	if err := json.Unmarshal(pendingData, &pending); err != nil {
		t.Fatalf("Unmarshal(pending boot) error = %v", err)
	}
	sidecar, err := LoadPersonalizationSidecar(env.config.PersonalizationPath)
	if err != nil {
		t.Fatalf("LoadPersonalizationSidecar() error = %v", err)
	}
	if sidecar.TransactionID != pending.Nonce {
		t.Fatalf("sidecar transaction_id = %q, pending nonce = %q", sidecar.TransactionID, pending.Nonce)
	}
	record := sidecar.Slots["b"]
	genericHash := testSHA256Hex(genericRootFS)
	if record.ArtifactSHA256 != genericHash {
		t.Fatalf("sidecar artifact hash = %s, want %s", record.ArtifactSHA256, genericHash)
	}
	if record.EffectivePartitionSHA256 == genericHash || record.HashedBytes != int64(len(genericRootFS)) {
		t.Fatalf("sidecar effective record = %+v", record)
	}
	cat, err := runPersonalizationCommand(requireE2fsprogs(t, "debugfs"), "-R", "cat /etc/machine-id", filepath.Join(env.blockDir, "rootfs_b"))
	if err != nil {
		t.Fatalf("debugfs cat personalized target error = %v stderr=%s", err, cat.Stderr)
	}
	if string(cat.Stdout) != machineID+"\n" {
		t.Fatalf("target machine-id = %q", cat.Stdout)
	}
	stateData, err := os.ReadFile(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("ReadFile(state.json) error = %v", err)
	}
	for _, forbidden := range []string{"artifact_sha256", "personalization_schema", "effective_partition_sha256", "hashed_bytes"} {
		if strings.Contains(string(stateData), forbidden) {
			t.Fatalf("public state contains Debian-only field %q", forbidden)
		}
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.PendingBootNonce != pending.Nonce || state.DownloadedHashes["rootfs"] != genericHash {
		t.Fatalf("public state transaction/artifact association = %+v", state)
	}
}

func newGenericExt4RootFS(t *testing.T, machineID string) string {
	t.Helper()
	mke2fs := requireE2fsprogs(t, "mke2fs")
	debugfs := requireE2fsprogs(t, "debugfs")
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "rootfs.img")
	f, err := os.OpenFile(imagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(image) error = %v", err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		_ = f.Close()
		t.Fatalf("Truncate(image) error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(image) error = %v", err)
	}
	if output, err := runPersonalizationCommand(mke2fs, "-q", "-t", "ext4", "-F", imagePath); err != nil {
		t.Fatalf("mke2fs error = %v stderr=%s", err, output.Stderr)
	}
	if output, err := runPersonalizationCommand(debugfs, "-w", "-R", "mkdir /etc", imagePath); err != nil {
		t.Fatalf("debugfs mkdir error = %v stderr=%s", err, output.Stderr)
	}
	machineIDSource := filepath.Join(dir, "machine-id")
	if err := os.WriteFile(machineIDSource, []byte(machineID), 0o644); err != nil {
		t.Fatalf("WriteFile(machine-id) error = %v", err)
	}
	command := "write " + machineIDSource + " /etc/machine-id"
	if output, err := runPersonalizationCommand(debugfs, "-w", "-R", command, imagePath); err != nil {
		t.Fatalf("debugfs write error = %v stderr=%s", err, output.Stderr)
	}
	return imagePath
}

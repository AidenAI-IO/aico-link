package ota

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	PersonalizationSchemaVersion      = 1
	DefaultPersistentMachineIDPath    = "/userdata/system/machine-id"
	DefaultPersonalizationSidecarPath = "/userdata/debian/ota/personalization-v1.json"
	DefaultDebugfsPath                = "/usr/sbin/debugfs"
	DefaultE2fsckPath                 = "/usr/sbin/e2fsck"
)

var (
	machineIDRE                  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	debugfsSizeRE                = regexp.MustCompile(`(?m)^User:.*\bSize:\s+([0-9]+)\s*$`)
	personalizationTransactionRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type PersonalizationSidecar struct {
	SchemaVersion   int                              `json:"schema_version"`
	TransactionID   string                           `json:"transaction_id"`
	TargetVersion   string                           `json:"target_version"`
	TargetBuildTime string                           `json:"target_build_time"`
	Slots           map[string]RootFSPersonalization `json:"slots"`
	ChecksumSHA256  string                           `json:"checksum_sha256"`
}

type RootFSPersonalization struct {
	ArtifactSHA256           string `json:"artifact_sha256"`
	PersonalizationSchema    int    `json:"personalization_schema"`
	EffectivePartitionSHA256 string `json:"effective_partition_sha256"`
	HashedBytes              int64  `json:"hashed_bytes"`
}

type commandOutput struct {
	Stdout []byte
	Stderr []byte
}

type personalizationCommandRunner func(name string, args ...string) (commandOutput, error)

func runPersonalizationCommand(name string, args ...string) (commandOutput, error) {
	cmd := exec.Command(name, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

func LoadPersonalizationSidecar(path string) (PersonalizationSidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PersonalizationSidecar{}, err
	}
	var sidecar PersonalizationSidecar
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return PersonalizationSidecar{}, err
	}
	if err := sidecar.Validate(); err != nil {
		return PersonalizationSidecar{}, err
	}
	return sidecar, nil
}

func SavePersonalizationSidecar(path string, sidecar PersonalizationSidecar) error {
	sidecar.SchemaVersion = PersonalizationSchemaVersion
	sidecar.ChecksumSHA256 = ""
	checksum, err := personalizationChecksum(sidecar)
	if err != nil {
		return err
	}
	sidecar.ChecksumSHA256 = checksum
	if err := sidecar.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sidecar, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, writeErr = f.Write(data); writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return fsyncDirFor(path)
}

func (s PersonalizationSidecar) Validate() error {
	if s.SchemaVersion != PersonalizationSchemaVersion {
		return fmt.Errorf("personalization schema_version %d, want %d", s.SchemaVersion, PersonalizationSchemaVersion)
	}
	if !personalizationTransactionRE.MatchString(s.TransactionID) {
		return fmt.Errorf("invalid personalization transaction_id %q", s.TransactionID)
	}
	if !manifestVersionRE.MatchString(s.TargetVersion) {
		return fmt.Errorf("invalid personalization target_version %q", s.TargetVersion)
	}
	if _, err := time.Parse(time.RFC3339, s.TargetBuildTime); err != nil {
		return fmt.Errorf("invalid personalization target_build_time: %w", err)
	}
	if len(s.Slots) == 0 {
		return errors.New("personalization sidecar has no slots")
	}
	for slot, record := range s.Slots {
		if slot != "a" && slot != "b" {
			return fmt.Errorf("invalid personalization slot %q", slot)
		}
		if err := validateSHA256Hex("personalization artifact_sha256", record.ArtifactSHA256); err != nil {
			return err
		}
		if record.PersonalizationSchema != PersonalizationSchemaVersion {
			return fmt.Errorf("slot %s personalization_schema %d, want %d", slot, record.PersonalizationSchema, PersonalizationSchemaVersion)
		}
		if err := validateSHA256Hex("personalization effective_partition_sha256", record.EffectivePartitionSHA256); err != nil {
			return err
		}
		if record.HashedBytes <= 0 {
			return fmt.Errorf("slot %s personalization hashed_bytes must be positive", slot)
		}
	}
	if err := validateSHA256Hex("personalization checksum_sha256", s.ChecksumSHA256); err != nil {
		return err
	}
	want, err := personalizationChecksum(s)
	if err != nil {
		return err
	}
	if s.ChecksumSHA256 != want {
		return fmt.Errorf("personalization checksum %s, want %s", s.ChecksumSHA256, want)
	}
	return nil
}

func personalizationChecksum(sidecar PersonalizationSidecar) (string, error) {
	sidecar.ChecksumSHA256 = ""
	data, err := json.Marshal(sidecar)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func readPersistentMachineID(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("persistent machine-id %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("persistent machine-id %s is group/world writable", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	machineID := strings.TrimSpace(string(data))
	if !machineIDRE.MatchString(machineID) {
		return "", fmt.Errorf("persistent machine-id %s is not 32 lowercase hexadecimal characters", path)
	}
	return machineID, nil
}

func personalizeExt4MachineID(blockPath string, machineID string, hashedBytes int64, debugfsPath string, e2fsckPath string, run personalizationCommandRunner) (string, error) {
	if !machineIDRE.MatchString(machineID) {
		return "", errors.New("machine-id is not 32 lowercase hexadecimal characters")
	}
	if hashedBytes <= 0 {
		return "", errors.New("personalization hash length must be positive")
	}
	if run == nil {
		run = runPersonalizationCommand
	}

	existing, err := readExt4MachineID(blockPath, debugfsPath, run)
	if err != nil {
		return "", fmt.Errorf("inspect generic rootfs machine-id: %w", err)
	}
	if existing != "" {
		return "", errors.New("generic rootfs /etc/machine-id is not empty")
	}
	if err := writeExt4MachineID(blockPath, machineID, debugfsPath, e2fsckPath, run); err != nil {
		return "", err
	}
	return hashFilePrefix(blockPath, hashedBytes)
}

func readExt4MachineID(blockPath string, debugfsPath string, run personalizationCommandRunner) (string, error) {
	if run == nil {
		run = runPersonalizationCommand
	}
	stat, err := run(debugfsPath, "-R", "stat /etc/machine-id", blockPath)
	if err != nil {
		return "", commandFailure("inspect rootfs machine-id", stat, err)
	}
	if err := rejectDebugfsErrors(stat); err != nil {
		return "", fmt.Errorf("inspect rootfs machine-id: %w", err)
	}
	match := debugfsSizeRE.FindSubmatch(stat.Stdout)
	if len(match) != 2 {
		return "", errors.New("inspect rootfs machine-id: debugfs did not report a file size")
	}
	if string(match[1]) == "0" {
		return "", nil
	}
	cat, err := run(debugfsPath, "-R", "cat /etc/machine-id", blockPath)
	if err != nil {
		return "", commandFailure("read rootfs machine-id", cat, err)
	}
	if err := rejectDebugfsErrors(cat); err != nil {
		return "", fmt.Errorf("read rootfs machine-id: %w", err)
	}
	machineID := strings.TrimSpace(string(cat.Stdout))
	if !machineIDRE.MatchString(machineID) {
		return "", fmt.Errorf("rootfs machine-id is not 32 lowercase hexadecimal characters: %q", cat.Stdout)
	}
	return machineID, nil
}

func writeExt4MachineID(blockPath string, machineID string, debugfsPath string, e2fsckPath string, run personalizationCommandRunner) error {
	if !machineIDRE.MatchString(machineID) {
		return errors.New("machine-id is not 32 lowercase hexadecimal characters")
	}
	if run == nil {
		run = runPersonalizationCommand
	}

	tmpDir, err := os.MkdirTemp("", "aico-ota-personalize-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	machineIDSource := filepath.Join(tmpDir, "machine-id")
	f, err := os.OpenFile(machineIDSource, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, writeErr = io.WriteString(f, machineID+"\n"); writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}

	for _, command := range []string{
		"rm /etc/machine-id",
		"write " + machineIDSource + " /etc/machine-id",
		"set_inode_field /etc/machine-id mode 0100444",
	} {
		output, err := run(debugfsPath, "-w", "-R", command, blockPath)
		if err != nil {
			return commandFailure("personalize rootfs machine-id", output, err)
		}
		if err := rejectDebugfsErrors(output); err != nil {
			return fmt.Errorf("personalize rootfs machine-id: %w", err)
		}
	}

	fsck, fsckErr := run(e2fsckPath, "-f", "-p", blockPath)
	if fsckErr != nil && !isE2fsckCorrected(fsckErr) {
		return commandFailure("fsck personalized rootfs", fsck, fsckErr)
	}

	readback, err := readExt4MachineID(blockPath, debugfsPath, run)
	if err != nil {
		return fmt.Errorf("read personalized rootfs machine-id: %w", err)
	}
	if readback != machineID {
		return fmt.Errorf("personalized rootfs machine-id readback is %q", readback)
	}
	return nil
}

func rejectDebugfsErrors(output commandOutput) error {
	combined := strings.ToLower(string(output.Stdout) + "\n" + string(output.Stderr))
	for _, marker := range []string{
		"file not found",
		"ext2 file already exists",
		"filesystem opened read/only",
		"command not found",
		"while opening",
	} {
		if strings.Contains(combined, marker) {
			return fmt.Errorf("debugfs reported %q", marker)
		}
	}
	return nil
}

func commandFailure(action string, output commandOutput, err error) error {
	detail := strings.TrimSpace(string(output.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(output.Stdout))
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}

func isE2fsckCorrected(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

func hashFilePrefix(path string, length int64) (string, error) {
	if length <= 0 {
		return "", errors.New("hash length must be positive")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	written, err := io.CopyN(h, f, length)
	if err != nil {
		return "", fmt.Errorf("hash %s: read %d of %d bytes: %w", path, written, length, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

package ota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"aico-agent/internal/logging"
)

const (
	FactoryIdentitySchemaVersion     = 1
	DefaultFactoryIdentityPath       = "/userdata/debian/ota/factory-identity-v1.json"
	DefaultSystemdMachineIDSetupPath = "/usr/bin/systemd-machine-id-setup"
)

type FactoryIdentityResult struct {
	MachineID            string `json:"machine_id"`
	ActiveSlot           string `json:"active_slot"`
	InactiveSlot         string `json:"inactive_slot"`
	PersistentCreated    bool   `json:"persistent_created"`
	InactivePersonalized bool   `json:"inactive_personalized"`
	RebootRequired       bool   `json:"reboot_required"`
}

type FactoryIdentityMarker struct {
	SchemaVersion    int    `json:"schema_version"`
	MachineID        string `json:"machine_id"`
	SlotsProvisioned bool   `json:"slots_provisioned"`
	RebootBootID     string `json:"reboot_boot_id,omitempty"`
	ChecksumSHA256   string `json:"checksum_sha256"`
}

func LoadFactoryIdentityMarker(path string) (FactoryIdentityMarker, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FactoryIdentityMarker{}, err
	}
	var marker FactoryIdentityMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return FactoryIdentityMarker{}, err
	}
	if err := marker.Validate(); err != nil {
		return FactoryIdentityMarker{}, err
	}
	return marker, nil
}

func SaveFactoryIdentityMarker(path string, marker FactoryIdentityMarker) error {
	marker.SchemaVersion = FactoryIdentitySchemaVersion
	marker.ChecksumSHA256 = ""
	checksum, err := factoryIdentityChecksum(marker)
	if err != nil {
		return err
	}
	marker.ChecksumSHA256 = checksum
	if err := marker.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(marker, "", "  ")
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

func (m FactoryIdentityMarker) Validate() error {
	if m.SchemaVersion != FactoryIdentitySchemaVersion {
		return fmt.Errorf("factory identity schema_version %d, want %d", m.SchemaVersion, FactoryIdentitySchemaVersion)
	}
	if !machineIDRE.MatchString(m.MachineID) {
		return errors.New("factory identity machine-id is not 32 lowercase hexadecimal characters")
	}
	if !m.SlotsProvisioned {
		return errors.New("factory identity slots_provisioned is false")
	}
	if err := validateSHA256Hex("factory identity checksum_sha256", m.ChecksumSHA256); err != nil {
		return err
	}
	want, err := factoryIdentityChecksum(m)
	if err != nil {
		return err
	}
	if m.ChecksumSHA256 != want {
		return fmt.Errorf("factory identity checksum %s, want %s", m.ChecksumSHA256, want)
	}
	return nil
}

func factoryIdentityChecksum(marker FactoryIdentityMarker) (string, error) {
	marker.ChecksumSHA256 = ""
	data, err := json.Marshal(marker)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (u *Updater) ProvisionFactoryIdentity() (FactoryIdentityResult, error) {
	if !u.config.DebianMode {
		return FactoryIdentityResult{}, errors.New("factory identity provisioning requires the Debian OTA configuration path")
	}
	if u.config.DryRun {
		return FactoryIdentityResult{}, errors.New("factory identity provisioning does not support dry-run")
	}
	if err := u.ensureStorageReady(); err != nil {
		return FactoryIdentityResult{}, err
	}
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		return FactoryIdentityResult{}, err
	}
	defer unlock()

	active, ok, err := u.currentSlot()
	if err != nil {
		return FactoryIdentityResult{}, err
	}
	if !ok {
		return FactoryIdentityResult{}, errors.New("aico.slot_suffix missing from cmdline")
	}
	rootSlot, rootOK, err := u.currentRootSlot()
	if err != nil {
		return FactoryIdentityResult{}, err
	}
	if !rootOK {
		return FactoryIdentityResult{}, errors.New("rootfs slot missing from cmdline")
	}
	if rootSlot != active {
		return FactoryIdentityResult{}, fmt.Errorf("running slot %s does not match rootfs slot %s", slotLogName(active), slotLogName(rootSlot))
	}
	inactive := inactiveSlot(active)
	activeName, _ := slotName(active)
	inactiveName, _ := slotName(inactive)
	result := FactoryIdentityResult{ActiveSlot: activeName, InactiveSlot: inactiveName}

	machineID, created, err := ensurePersistentMachineID(u.config.MachineIDPath, u.config.RuntimeMachineIDPath)
	if err != nil {
		return FactoryIdentityResult{}, err
	}
	result.MachineID = machineID
	result.PersistentCreated = created

	bootID := u.bootID()
	if bootID == "" {
		return FactoryIdentityResult{}, errors.New("current boot ID is empty")
	}
	marker, markerErr := LoadFactoryIdentityMarker(u.config.FactoryIdentityPath)
	markerValid := markerErr == nil && marker.MachineID == machineID
	if markerErr != nil && !os.IsNotExist(markerErr) {
		logging.Warnf("ota", "updater", "ota factory identity: ignoring invalid marker: %v", markerErr)
	}
	if !markerValid {
		inactivePath := filepath.Join(u.blockDirForAccess(), "rootfs_"+inactiveName)
		changed, err := ensureExt4MachineID(
			inactivePath,
			machineID,
			u.config.DebugfsPath,
			u.config.E2fsckPath,
			u.runCommand,
		)
		if err != nil {
			return FactoryIdentityResult{}, fmt.Errorf("provision inactive rootfs slot %s: %w", inactiveName, err)
		}
		result.InactivePersonalized = changed
		marker = FactoryIdentityMarker{
			SchemaVersion:    FactoryIdentitySchemaVersion,
			MachineID:        machineID,
			SlotsProvisioned: true,
		}
	}

	runtimeMachineID, err := readPersistentMachineID(u.config.RuntimeMachineIDPath)
	if err != nil {
		return FactoryIdentityResult{}, fmt.Errorf("read runtime machine-id: %w", err)
	}
	runtimeStaged := false
	if runtimeMachineID != machineID {
		marker.RebootBootID = bootID
		if err := SaveFactoryIdentityMarker(u.config.FactoryIdentityPath, marker); err != nil {
			return FactoryIdentityResult{}, fmt.Errorf("persist factory identity reboot transaction: %w", err)
		}
		if err := writeRuntimeMachineID(u.config.RuntimeMachineIDPath, machineID); err != nil {
			return FactoryIdentityResult{}, fmt.Errorf("stage persistent machine-id on active rootfs: %w", err)
		}
		runtimeStaged = true
	}

	activePath := filepath.Join(u.blockDirForAccess(), "rootfs_"+activeName)
	activeMachineID, err := readExt4MachineID(activePath, u.config.DebugfsPath, u.runCommand)
	if err != nil {
		return FactoryIdentityResult{}, fmt.Errorf("inspect active rootfs slot %s: %w", activeName, err)
	}
	if activeMachineID != machineID {
		if marker.RebootBootID == "" && runtimeMachineID != machineID {
			marker.RebootBootID = bootID
		}
		if !runtimeStaged {
			if err := writeRuntimeMachineID(u.config.RuntimeMachineIDPath, machineID); err != nil {
				return FactoryIdentityResult{}, fmt.Errorf("write active rootfs machine-id: %w", err)
			}
		}
		output, commitErr := u.runCommand(u.config.MachineIDSetupPath, "--commit")
		if commitErr != nil {
			return FactoryIdentityResult{}, commandFailure("commit active rootfs machine-id", output, commitErr)
		}
		activeMachineID, err = readExt4MachineID(activePath, u.config.DebugfsPath, u.runCommand)
		if err != nil {
			return FactoryIdentityResult{}, fmt.Errorf("verify active rootfs slot %s: %w", activeName, err)
		}
		if activeMachineID != machineID {
			return FactoryIdentityResult{}, fmt.Errorf("active rootfs slot %s machine-id %q does not match persistent machine-id", activeName, activeMachineID)
		}
	}

	if marker.RebootBootID != "" && marker.RebootBootID != bootID {
		marker.RebootBootID = ""
	}
	result.RebootRequired = marker.RebootBootID == bootID
	if err := SaveFactoryIdentityMarker(u.config.FactoryIdentityPath, marker); err != nil {
		return FactoryIdentityResult{}, fmt.Errorf("save factory identity marker: %w", err)
	}
	logging.Infof("ota", "updater",
		"ota factory identity: active_slot=%s inactive_slot=%s persistent_created=%t inactive_personalized=%t reboot_required=%t",
		activeName,
		inactiveName,
		result.PersistentCreated,
		result.InactivePersonalized,
		result.RebootRequired,
	)
	return result, nil
}

func ensurePersistentMachineID(path string, runtimePath string) (string, bool, error) {
	machineID, err := readPersistentMachineID(path)
	if err == nil {
		return machineID, false, nil
	}
	if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("load persistent machine-id: %w", err)
	}
	runtimeMachineID, err := readPersistentMachineID(runtimePath)
	if err != nil {
		return "", false, fmt.Errorf("load runtime machine-id for persistence: %w", err)
	}
	if err := writeMachineIDAtomic(path, runtimeMachineID); err != nil {
		return "", false, fmt.Errorf("persist runtime machine-id: %w", err)
	}
	return runtimeMachineID, true, nil
}

func writeMachineIDAtomic(path string, machineID string) error {
	if !machineIDRE.MatchString(machineID) {
		return errors.New("machine-id is not 32 lowercase hexadecimal characters")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o444)
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
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return fsyncDirFor(path)
}

func writeRuntimeMachineID(path string, machineID string) error {
	if !machineIDRE.MatchString(machineID) {
		return errors.New("machine-id is not 32 lowercase hexadecimal characters")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("runtime machine-id %s is not a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	writeErr := error(nil)
	if _, writeErr = io.WriteString(f, machineID+"\n"); writeErr == nil {
		writeErr = f.Chmod(0o444)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func ensureExt4MachineID(blockPath string, machineID string, debugfsPath string, e2fsckPath string, run personalizationCommandRunner) (bool, error) {
	existing, err := readExt4MachineID(blockPath, debugfsPath, run)
	if err != nil {
		return false, err
	}
	if existing == machineID {
		return false, nil
	}
	if err := writeExt4MachineID(blockPath, machineID, debugfsPath, e2fsckPath, run); err != nil {
		return false, err
	}
	return true, nil
}

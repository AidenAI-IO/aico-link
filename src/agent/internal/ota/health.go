package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type PendingBoot struct {
	TargetSlot      string `json:"target_slot"`
	TargetVersion   string `json:"target_version"`
	TargetBuildTime string `json:"target_build_time"`
	Nonce           string `json:"nonce"`
}

type HealthMarker struct {
	Slot      string `json:"slot"`
	Version   string `json:"version"`
	BuildTime string `json:"build_time"`
	Nonce     string `json:"nonce"`
	BootID    string `json:"boot_id"`
}

func DeleteStaleHealthMarker(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func WritePendingBoot(path string, pending PendingBoot) error {
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
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

func ValidateHealthMarker(path string, pending PendingBoot, currentBootID string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var marker HealthMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return err
	}
	if marker.Slot != pending.TargetSlot {
		return fmt.Errorf("health slot %q, want %q", marker.Slot, pending.TargetSlot)
	}
	if marker.Version != pending.TargetVersion {
		return fmt.Errorf("health version %q, want %q", marker.Version, pending.TargetVersion)
	}
	if marker.BuildTime != pending.TargetBuildTime {
		return fmt.Errorf("health build_time %q, want %q", marker.BuildTime, pending.TargetBuildTime)
	}
	if marker.Nonce != pending.Nonce {
		return fmt.Errorf("health nonce %q, want %q", marker.Nonce, pending.Nonce)
	}
	if marker.BootID != currentBootID {
		return fmt.Errorf("health boot_id %q, want %q", marker.BootID, currentBootID)
	}
	return nil
}

func WriteHealthMarker(path string, pending PendingBoot, slot string, bootID string) error {
	if slot == "" {
		return fmt.Errorf("health slot is empty")
	}
	if bootID == "" {
		return fmt.Errorf("health boot_id is empty")
	}
	marker := HealthMarker{
		Slot:      slot,
		Version:   pending.TargetVersion,
		BuildTime: pending.TargetBuildTime,
		Nonce:     pending.Nonce,
		BootID:    bootID,
	}
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

func WriteHealthMarkerIfPending(pendingPath string, markerPath string) (bool, error) {
	return writeHealthMarkerIfPending(pendingPath, markerPath, currentSlotFromProcCmdline, currentRootSlotFromProcCmdline, currentBootID)
}

// MarkHealthIfPending is the transaction-bound marker entry point used by the
// independent Debian local-health aggregator. It validates public OTA state,
// the running boot/rootfs slot, the current boot ID, and Debian identity
// personalization before allowing the marker to be written.
func (u *Updater) MarkHealthIfPending() (bool, error) {
	if err := u.ensureStorageReady(); err != nil {
		return false, err
	}
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		return false, err
	}
	defer unlock()

	data, err := os.ReadFile(u.pendingPath())
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var pending PendingBoot
	if err := json.Unmarshal(data, &pending); err != nil {
		return false, err
	}
	state, err := u.loadState()
	if err != nil {
		return false, err
	}
	if state.Phase != "pending-reboot" {
		return false, fmt.Errorf("public OTA state phase %q, want pending-reboot", state.Phase)
	}
	if state.PendingBootNonce != pending.Nonce {
		return false, fmt.Errorf("public OTA transaction %q does not match pending nonce %q", state.PendingBootNonce, pending.Nonce)
	}
	if state.TargetVersion != pending.TargetVersion || state.TargetBuildTime != pending.TargetBuildTime {
		return false, fmt.Errorf("public OTA target version/build does not match pending boot")
	}
	targetSlot, err := parseSlotName(pending.TargetSlot)
	if err != nil {
		return false, err
	}
	if state.TargetSlot != targetSlot {
		return false, fmt.Errorf("public OTA target slot %s does not match pending target %s", slotLogName(state.TargetSlot), pending.TargetSlot)
	}
	if u.config.DebianMode {
		if err := u.validateDebianPendingIdentity(pending); err != nil {
			return false, err
		}
	}
	return writeHealthMarkerIfPending(
		u.pendingPath(),
		u.healthPath(),
		u.currentSlot,
		u.currentRootSlot,
		u.bootID,
	)
}

func (u *Updater) validateDebianPendingIdentity(pending PendingBoot) error {
	persistentMachineID, err := readPersistentMachineID(u.config.MachineIDPath)
	if err != nil {
		return fmt.Errorf("validate persistent machine-id: %w", err)
	}
	runtimeMachineID, err := readPersistentMachineID(u.config.RuntimeMachineIDPath)
	if err != nil {
		return fmt.Errorf("validate runtime machine-id: %w", err)
	}
	if runtimeMachineID != persistentMachineID {
		return fmt.Errorf("runtime machine-id does not match persistent machine-id")
	}
	sidecar, err := LoadPersonalizationSidecar(u.config.PersonalizationPath)
	if err != nil {
		return fmt.Errorf("validate personalization sidecar: %w", err)
	}
	if sidecar.TransactionID != pending.Nonce {
		return fmt.Errorf("personalization transaction %q does not match pending nonce %q", sidecar.TransactionID, pending.Nonce)
	}
	if sidecar.TargetVersion != pending.TargetVersion || sidecar.TargetBuildTime != pending.TargetBuildTime {
		return fmt.Errorf("personalization target version/build does not match pending boot")
	}
	if _, ok := sidecar.Slots[pending.TargetSlot]; !ok {
		return fmt.Errorf("personalization sidecar has no target slot %s record", pending.TargetSlot)
	}
	return nil
}

func writeHealthMarkerIfPending(pendingPath string, markerPath string, currentSlot func() (Slot, bool, error), currentRootSlot func() (Slot, bool, error), bootID func() string) (bool, error) {
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var pending PendingBoot
	if err := json.Unmarshal(data, &pending); err != nil {
		return false, err
	}
	running, ok, err := currentSlot()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("aico.slot_suffix missing from cmdline")
	}
	runningName, err := slotName(running)
	if err != nil {
		return false, err
	}
	if runningName != pending.TargetSlot {
		return false, fmt.Errorf("running slot %s does not match pending target %s", runningName, pending.TargetSlot)
	}
	rootSlot, ok, err := currentRootSlot()
	if err != nil {
		return false, err
	}
	if !ok {
		return false, fmt.Errorf("rootfs slot missing from cmdline")
	}
	rootName, err := slotName(rootSlot)
	if err != nil {
		return false, err
	}
	if rootName != pending.TargetSlot {
		return false, fmt.Errorf("running rootfs slot %s does not match pending target %s", rootName, pending.TargetSlot)
	}
	return true, WriteHealthMarker(markerPath, pending, runningName, bootID())
}

func WaitForHealth(ctx context.Context, markerPath string, pending PendingBoot, currentBootID string, timeout time.Duration, interval time.Duration, reboot func() error) error {
	if interval <= 0 {
		return fmt.Errorf("health interval must be positive")
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if err := ValidateHealthMarker(markerPath, pending, currentBootID); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if reboot != nil {
				if err := reboot(); err != nil {
					return err
				}
			}
			return fmt.Errorf("health timeout waiting for %s", markerPath)
		case <-ticker.C:
		}
	}
}

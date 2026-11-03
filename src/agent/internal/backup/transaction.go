package backup

// The restore transaction is the one on-disk contract shared by the Config Web
// restore endpoint and the boot-time recovery service. Every state change is
// persisted before the next irreversible filesystem step so that a power loss
// at any point can be resumed or rolled back idempotently.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	TransactionFormatVersion = 1
	TransactionDirName       = ".aico-restore"
	TransactionFileName      = "transaction.json"

	// Transaction states persisted on disk. Only committing and rolling_back
	// are continued at boot; anything else is disposable staging that must
	// never overwrite user data.
	TransactionPrepared    = "prepared"
	TransactionCommitting  = "committing"
	TransactionCommitted   = "committed"
	TransactionRollingBack = "rolling_back"
	TransactionRolledBack  = "rolled_back"

	LayerUserdata = "userdata"
	LayerSD       = "sd"
)

// TransactionUnit is a single same-filesystem exchange: a directory or a
// single file that is swapped with a staged replacement via rename.
//
// BindMount is set for components whose target directory is the source of a
// bind mount (for example /userdata/userhome -> /root). Those units must be
// unmounted before the exchange and re-mounted afterwards, because the mount
// follows the inode rather than the path.
type TransactionUnit struct {
	Key           string      `json:"key"`
	Component     ComponentID `json:"component"`
	Layer         string      `json:"layer"`
	Target        string      `json:"target"`
	Staged        string      `json:"staged"`
	Old           string      `json:"old"`
	Directory     bool        `json:"directory"`
	BindMount     string      `json:"bind_mount,omitempty"`
	RemountScript string      `json:"remount_script,omitempty"`

	HadOriginal  bool `json:"had_original,omitempty"`
	Unmounted    bool `json:"unmounted,omitempty"`
	OldMoved     bool `json:"old_moved,omitempty"`
	NewInstalled bool `json:"new_installed,omitempty"`
	Remounted    bool `json:"remounted,omitempty"`
	Committed    bool `json:"committed,omitempty"`
}

// Transaction is written below every filesystem that takes part in a restore
// (Layer names which one this copy describes). RequiredLayers lists every
// layer so a userdata copy can tell that an absent SD card still owes work.
type Transaction struct {
	FormatVersion          int                `json:"format_version"`
	JobID                  string             `json:"job_id"`
	Layer                  string             `json:"layer"`
	RequiredLayers         []string           `json:"required_layers"`
	State                  string             `json:"state"`
	Phase                  string             `json:"phase"`
	UpdatedAt              time.Time          `json:"updated_at"`
	IdentitySelected       bool               `json:"identity_selected,omitempty"`
	IdentityProvisioned    bool               `json:"identity_provisioned,omitempty"`
	IdentityRebootRequired bool               `json:"identity_reboot_required,omitempty"`
	Units                  []*TransactionUnit `json:"units"`
}

// transactionEnvelope wraps the encoded transaction with a checksum so a
// torn or corrupted log fails closed instead of moving arbitrary paths.
type transactionEnvelope struct {
	FormatVersion     int    `json:"format_version"`
	TransactionBase64 string `json:"transaction_base64"`
	SHA256            string `json:"sha256"`
}

func TransactionDir(root, jobID string) string {
	return filepath.Join(root, TransactionDirName, jobID)
}

func TransactionPath(root, jobID string) string {
	return filepath.Join(TransactionDir(root, jobID), TransactionFileName)
}

// WriteTransaction persists txn below root using temp file, fsync, rename and
// parent-directory fsync so the log is either the old or the new version.
func WriteTransaction(root string, txn Transaction) error {
	if strings.TrimSpace(txn.JobID) == "" || strings.ContainsAny(txn.JobID, "/\\") {
		return fmt.Errorf("invalid transaction job id")
	}
	txn.FormatVersion = TransactionFormatVersion
	txn.UpdatedAt = time.Now().UTC()
	encoded, err := json.Marshal(txn)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	envelope := transactionEnvelope{
		FormatVersion:     TransactionFormatVersion,
		TransactionBase64: base64.StdEncoding.EncodeToString(encoded),
		SHA256:            hex.EncodeToString(digest[:]),
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	dir := TransactionDir(root, txn.JobID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+TransactionFileName+".tmp")
	path := filepath.Join(dir, TransactionFileName)
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncParent(path)
}

// ReadTransaction loads and checksum-verifies a transaction log.
func ReadTransaction(path string) (Transaction, error) {
	var txn Transaction
	data, err := os.ReadFile(path)
	if err != nil {
		return txn, err
	}
	var envelope transactionEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return txn, fmt.Errorf("transaction envelope is invalid: %w", err)
	}
	if envelope.FormatVersion != TransactionFormatVersion {
		return txn, fmt.Errorf("unsupported transaction format version %d", envelope.FormatVersion)
	}
	encoded, err := base64.StdEncoding.DecodeString(envelope.TransactionBase64)
	if err != nil {
		return txn, fmt.Errorf("transaction payload is invalid: %w", err)
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != strings.ToLower(strings.TrimSpace(envelope.SHA256)) {
		return txn, fmt.Errorf("transaction checksum mismatch")
	}
	if err := json.Unmarshal(encoded, &txn); err != nil {
		return txn, fmt.Errorf("transaction is invalid: %w", err)
	}
	if txn.FormatVersion != TransactionFormatVersion {
		return txn, fmt.Errorf("unsupported transaction version %d", txn.FormatVersion)
	}
	if strings.TrimSpace(txn.JobID) == "" || strings.ContainsAny(txn.JobID, "/\\") {
		return txn, fmt.Errorf("transaction job id is invalid")
	}
	return txn, nil
}

// ValidatePaths rejects any unit whose paths do not stay below root. A damaged
// log must never turn the recovery service into an arbitrary file mover.
func (t Transaction) ValidatePaths(root string) error {
	root = filepath.Clean(root)
	for _, unit := range t.Units {
		if unit == nil {
			return fmt.Errorf("transaction contains an empty unit")
		}
		stagingRoot := TransactionDir(root, t.JobID)
		if err := pathBelow(unit.Staged, filepath.Join(stagingRoot, "new")); err != nil {
			return fmt.Errorf("unit %s staged: %w", unit.Key, err)
		}
		if err := pathBelow(unit.Old, filepath.Join(stagingRoot, "old")); err != nil {
			return fmt.Errorf("unit %s old: %w", unit.Key, err)
		}
		if err := pathBelow(unit.Target, root); err != nil {
			return fmt.Errorf("unit %s target: %w", unit.Key, err)
		}
		if filepath.Clean(unit.Target) == root || strings.HasPrefix(filepath.Clean(unit.Target), stagingRoot) {
			return fmt.Errorf("unit %s target %q is not a valid restore destination", unit.Key, unit.Target)
		}
		if unit.BindMount != "" && (!filepath.IsAbs(unit.BindMount) || filepath.Clean(unit.BindMount) != unit.BindMount) {
			return fmt.Errorf("unit %s bind mount path is invalid", unit.Key)
		}
		if unit.RemountScript != "" && !filepath.IsAbs(unit.RemountScript) {
			return fmt.Errorf("unit %s remount script path is invalid", unit.Key)
		}
	}
	return nil
}

func pathBelow(value, root string) error {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return fmt.Errorf("%q is not an absolute clean path", value)
	}
	rel, err := filepath.Rel(root, value)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%q is outside %q", value, root)
	}
	return nil
}

// MountController abstracts the bind-mount operations needed for user_home
// and BlueZ state so host tests can run without root.
type MountController interface {
	IsMountPoint(path string) (bool, error)
	Unmount(path string) error
	Remount(script string) error
}

// SystemMountController drives findmnt, umount and the overlay remount
// scripts on the device.
type SystemMountController struct{}

func (SystemMountController) IsMountPoint(path string) (bool, error) {
	cmd := exec.Command("findmnt", "-nr", "--mountpoint", path)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output)) != "", nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("findmnt %s: %w", path, err)
}

func (SystemMountController) Unmount(path string) error {
	output, err := exec.Command("umount", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (SystemMountController) Remount(script string) error {
	output, err := exec.Command(script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(script), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// SameInode reports whether two paths resolve to the same device and inode,
// which is how a bind mount is tied to its source directory.
func SameInode(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, err
	}
	leftStat, leftOK := leftInfo.Sys().(*syscall.Stat_t)
	rightStat, rightOK := rightInfo.Sys().(*syscall.Stat_t)
	if !leftOK || !rightOK {
		return false, fmt.Errorf("inode information is unavailable")
	}
	return leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino, nil
}

// SyncParent fsyncs the directory containing path.
func SyncParent(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func lexists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// EnsureParent creates missing parent directories of path without following
// symlinks, and rejects any existing parent that is not a real directory.
func EnsureParent(path string) error {
	parent := filepath.Clean(filepath.Dir(path))
	if !filepath.IsAbs(parent) {
		return fmt.Errorf("restore path %q is not absolute", path)
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(parent, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe restore parent %s", current)
		}
	}
	return nil
}

func unmountForExchange(unit *TransactionUnit, mounts MountController, expectedSource string) error {
	if unit.BindMount == "" {
		return nil
	}
	if mounts == nil {
		return fmt.Errorf("unit %s requires a mount controller", unit.Key)
	}
	mounted, err := mounts.IsMountPoint(unit.BindMount)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if exists, err := lexists(expectedSource); err == nil && exists {
		same, err := SameInode(unit.BindMount, expectedSource)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%s is not bound to %s; refusing to exchange", unit.BindMount, expectedSource)
		}
	}
	return mounts.Unmount(unit.BindMount)
}

func remountAndVerify(unit *TransactionUnit, mounts MountController, expectedSource string) error {
	if unit.BindMount == "" {
		return nil
	}
	if mounts == nil {
		return fmt.Errorf("unit %s requires a mount controller", unit.Key)
	}
	if unit.RemountScript == "" {
		return fmt.Errorf("unit %s has no remount script", unit.Key)
	}
	if err := mounts.Remount(unit.RemountScript); err != nil {
		return err
	}
	mounted, err := mounts.IsMountPoint(unit.BindMount)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("%s is not mounted after remount", unit.BindMount)
	}
	same, err := SameInode(unit.BindMount, expectedSource)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%s does not reference %s after remount", unit.BindMount, expectedSource)
	}
	return nil
}

// CommitUnit performs (or resumes) one exchange. checkpoint is invoked after
// every persisted flag change so rollback never depends on in-memory state.
func CommitUnit(unit *TransactionUnit, mounts MountController, checkpoint func() error) error {
	if unit == nil || unit.Committed {
		return nil
	}
	if checkpoint == nil {
		checkpoint = func() error { return nil }
	}
	if err := EnsureParent(unit.Target); err != nil {
		return err
	}
	if err := EnsureParent(unit.Old); err != nil {
		return err
	}
	// A rename may be durable even when its following checkpoint was lost.
	oldExists, err := lexists(unit.Old)
	if err != nil {
		return err
	}
	targetExists, err := lexists(unit.Target)
	if err != nil {
		return err
	}
	stagedExists, err := lexists(unit.Staged)
	if err != nil {
		return err
	}
	if !unit.OldMoved && oldExists {
		if targetExists && stagedExists {
			return fmt.Errorf("ambiguous exchange paths for %s; preserving all copies", unit.Key)
		}
		unit.HadOriginal, unit.OldMoved = true, true
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if !unit.NewInstalled && !stagedExists && targetExists && (unit.OldMoved || unit.Unmounted) {
		unit.NewInstalled = true
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if !unit.Unmounted {
		// The mount still references the live target (or the already moved
		// original when resuming after a crash between the two renames).
		expected := unit.Target
		if unit.OldMoved {
			expected = unit.Old
		}
		if unit.NewInstalled {
			expected = unit.Target
		}
		if err := unmountForExchange(unit, mounts, expected); err != nil {
			return err
		}
		unit.Unmounted = true
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if !unit.OldMoved {
		exists, err := lexists(unit.Target)
		if err != nil {
			return err
		}
		if exists {
			unit.HadOriginal = true
			if err := os.Rename(unit.Target, unit.Old); err != nil {
				return err
			}
			unit.OldMoved = true
			if err := SyncParent(unit.Old); err != nil {
				return err
			}
			if err := SyncParent(unit.Target); err != nil {
				return err
			}
			if err := checkpoint(); err != nil {
				return err
			}
		}
	}
	if !unit.NewInstalled {
		exists, err := lexists(unit.Staged)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("staged restore path is missing for %s", unit.Key)
		}
		if err := os.Rename(unit.Staged, unit.Target); err != nil {
			return err
		}
		unit.NewInstalled = true
		if err := SyncParent(unit.Target); err != nil {
			return err
		}
		if err := SyncParent(unit.Staged); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if unit.BindMount != "" && !unit.Remounted {
		if err := remountAndVerify(unit, mounts, unit.Target); err != nil {
			return err
		}
		unit.Remounted = true
		if err := checkpoint(); err != nil {
			return err
		}
	}
	unit.Committed = true
	return checkpoint()
}

// RollbackUnit reverses whatever CommitUnit already did for unit. The staged
// tree is moved back below new/ rather than deleted so that a failed rollback
// never destroys the only remaining copy of either version.
func RollbackUnit(unit *TransactionUnit, mounts MountController, checkpoint func() error) (returnErr error) {
	if unit == nil {
		return nil
	}
	if checkpoint == nil {
		checkpoint = func() error { return nil }
	}
	oldExists, err := lexists(unit.Old)
	if err != nil {
		return err
	}
	targetExists, err := lexists(unit.Target)
	if err != nil {
		return err
	}
	stagedExists, err := lexists(unit.Staged)
	if err != nil {
		return err
	}
	reconciled := false
	if oldExists && !unit.OldMoved {
		unit.HadOriginal, unit.OldMoved = true, true
		reconciled = true
	}
	if oldExists && targetExists && !stagedExists && !unit.NewInstalled {
		unit.NewInstalled = true
		reconciled = true
	}
	// Resume either rollback rename when it happened before its checkpoint.
	if unit.NewInstalled && !targetExists && stagedExists {
		unit.NewInstalled = false
		reconciled = true
	}
	if unit.OldMoved && !oldExists && targetExists && stagedExists {
		unit.NewInstalled, unit.OldMoved = false, false
		reconciled = true
	}
	if reconciled {
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if !unit.Unmounted && !unit.OldMoved && !unit.NewInstalled {
		unit.Committed = false
		return nil
	}
	if unit.BindMount != "" {
		// Same fail-closed rule as the commit path: a bind-mount unit can only
		// be exchanged when the mount can be dropped and re-established.
		if mounts == nil {
			return fmt.Errorf("unit %s requires a mount controller", unit.Key)
		}
		mounted, err := mounts.IsMountPoint(unit.BindMount)
		if err != nil {
			return err
		}
		if mounted {
			if err := mounts.Unmount(unit.BindMount); err != nil {
				return err
			}
		}
		unit.Unmounted = true
		unit.Remounted = false
		// If rollback fails before replacing the live source, rebind it for
		// observability. Keep the error and transaction for the recovery gate;
		// writers must remain stopped until the whole rollback succeeds.
		defer func() {
			if returnErr == nil {
				return
			}
			if exists, err := lexists(unit.Target); err != nil || !exists {
				return
			}
			if err := remountAndVerify(unit, mounts, unit.Target); err != nil {
				returnErr = errors.Join(returnErr, err)
			}
		}()
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if unit.NewInstalled {
		if err := EnsureParent(unit.Staged); err != nil {
			return err
		}
		if exists, err := lexists(unit.Staged); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("rollback staging already exists for %s; preserving both copies", unit.Key)
		}
		if err := os.Rename(unit.Target, unit.Staged); err != nil {
			return err
		}
		unit.NewInstalled = false
		if err := SyncParent(unit.Target); err != nil {
			return err
		}
		if err := SyncParent(unit.Staged); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if unit.OldMoved {
		exists, err := lexists(unit.Old)
		if err != nil {
			return err
		}
		if exists {
			if err := EnsureParent(unit.Target); err != nil {
				return err
			}
			if err := os.Rename(unit.Old, unit.Target); err != nil {
				return err
			}
		} else if exists, err := lexists(unit.Target); err != nil || !exists {
			return fmt.Errorf("original restore path is missing for %s", unit.Key)
		}
		unit.OldMoved = false
		if err := SyncParent(unit.Target); err != nil {
			return err
		}
		if err := SyncParent(unit.Old); err != nil {
			return err
		}
		if err := checkpoint(); err != nil {
			return err
		}
	}
	if unit.BindMount != "" && unit.Unmounted {
		if exists, _ := lexists(unit.Target); exists {
			if err := remountAndVerify(unit, mounts, unit.Target); err != nil {
				return err
			}
		}
		unit.Unmounted = false
		if err := checkpoint(); err != nil {
			return err
		}
	}
	unit.Committed = false
	return checkpoint()
}

// SortUnits orders units by the protocol commit order, then by key.
func SortUnits(units []*TransactionUnit) {
	order := make(map[ComponentID]int, len(CommitOrder))
	for index, id := range CommitOrder {
		order[id] = index
	}
	rank := func(id ComponentID) int {
		if value, ok := order[id]; ok {
			return value
		}
		return len(CommitOrder) + 1
	}
	sort.SliceStable(units, func(i, j int) bool {
		left, right := rank(units[i].Component), rank(units[j].Component)
		if left != right {
			return left < right
		}
		return units[i].Key < units[j].Key
	})
}

// IdentityProvisioner finalizes a restored machine identity after commit. It
// reports whether the device must reboot to complete provisioning.
type IdentityProvisioner interface {
	ProvisionIdentity(ctx context.Context) (rebootRequired bool, err error)
}

// RecoveryOptions configures a boot-time or startup transaction sweep.
type RecoveryOptions struct {
	Roots    Roots
	Mounts   MountController
	Identity IdentityProvisioner
	Logf     func(format string, args ...any)
}

// RecoveryResult describes what happened to one transaction directory.
type RecoveryResult struct {
	Root           string `json:"root"`
	JobID          string `json:"job_id"`
	Result         string `json:"result"`
	Error          string `json:"error,omitempty"`
	RebootRequired bool   `json:"reboot_required,omitempty"`
	MissingLayer   string `json:"missing_layer,omitempty"`
}

const (
	RecoveryDiscarded  = "discarded"
	RecoveryCommitted  = "committed"
	RecoveryRolledBack = "rolled_back"
	RecoveryFailed     = "failed"
	RecoveryWaiting    = "waiting"
	RecoveryCleaned    = "cleaned"
)

// RecoverTransactions scans every persistent root for restore transaction
// directories and finishes, rolls back or discards them. It is idempotent and
// safe to run on every boot and on every Config Web start.
//
// The sweep runs in two passes: every layer copy is first driven to a terminal
// state, then rollback copies are removed only once every required layer is
// terminal. A transaction whose SD half is absent stays in place with its
// rollback copies so it can finish when the card returns.
func RecoverTransactions(ctx context.Context, options RecoveryOptions) ([]RecoveryResult, error) {
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	type layerRoot struct{ name, root string }
	layers := []layerRoot{{LayerUserdata, options.Roots.Userdata}, {LayerSD, options.Roots.SD}}
	type pending struct {
		layer, root string
		txn         Transaction
		index       int
	}
	var results []RecoveryResult
	var terminal []pending
	states := make(map[string]map[string]string)
	var firstErr error
	record := func(result RecoveryResult) int {
		results = append(results, result)
		if result.Result == RecoveryFailed && firstErr == nil {
			firstErr = errors.New(result.Error)
		}
		return len(results) - 1
	}
	for _, layer := range layers {
		if strings.TrimSpace(layer.root) == "" {
			continue
		}
		base := filepath.Join(layer.root, TransactionDirName)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			if !entry.IsDir() {
				_ = os.Remove(filepath.Join(base, entry.Name()))
				continue
			}
			txn, result, done := advanceTransaction(ctx, options, layer.name, layer.root, entry.Name(), logf)
			if states[entry.Name()] == nil {
				states[entry.Name()] = make(map[string]string)
			}
			states[entry.Name()][layer.name] = txn.State
			index := record(result)
			if !done {
				terminal = append(terminal, pending{layer: layer.name, root: layer.root, txn: txn, index: index})
			}
		}
	}
	for _, item := range terminal {
		result := &results[item.index]
		if missing := missingLayer(options.Roots, item.txn, item.layer, states[item.txn.JobID]); missing != "" {
			logf("restore %s: %s on %s, waiting for layer %s", item.txn.JobID, item.txn.State, item.root, missing)
			result.Result, result.MissingLayer = RecoveryWaiting, missing
			continue
		}
		if err := CleanupTransaction(item.root, item.txn.JobID, true); err != nil {
			result.Result, result.Error = RecoveryFailed, err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		logf("restore %s: %s on %s", item.txn.JobID, item.txn.State, item.root)
	}
	return results, firstErr
}

// advanceTransaction drives one layer copy to a terminal state. done reports
// that no second pass is needed (failures, discards and orphans).
func advanceTransaction(ctx context.Context, options RecoveryOptions, layer, root, jobID string, logf func(string, ...any)) (Transaction, RecoveryResult, bool) {
	result := RecoveryResult{Root: root, JobID: jobID}
	dir := TransactionDir(root, jobID)
	path := filepath.Join(dir, TransactionFileName)
	txn, err := ReadTransaction(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Staging without a transaction log is an interrupted ingest; the
		// formal data was never touched, so the directory is simply removed.
		if err := os.RemoveAll(dir); err != nil {
			result.Result, result.Error = RecoveryFailed, err.Error()
			return txn, result, true
		}
		logf("restore %s: discarded orphaned staging below %s", jobID, root)
		result.Result = RecoveryDiscarded
		return txn, result, true
	}
	if err != nil {
		result.Result, result.Error = RecoveryFailed, err.Error()
		return txn, result, true
	}
	if txn.Layer != layer || txn.JobID != jobID {
		result.Result, result.Error = RecoveryFailed, "transaction layer or job id does not match its location"
		return txn, result, true
	}
	if err := txn.ValidatePaths(root); err != nil {
		result.Result, result.Error = RecoveryFailed, err.Error()
		return txn, result, true
	}
	checkpoint := func() error { return WriteTransaction(root, txn) }
	units := append([]*TransactionUnit(nil), txn.Units...)
	SortUnits(units)
	switch txn.State {
	case TransactionCommitting:
		for _, unit := range units {
			if err := CommitUnit(unit, options.Mounts, checkpoint); err != nil {
				logf("restore %s: commit of %s failed: %v", jobID, unit.Key, err)
				result.Result, result.Error = RecoveryFailed, fmt.Sprintf("commit %s: %v", unit.Key, err)
				return txn, result, true
			}
		}
		if layer == LayerUserdata && txn.IdentitySelected && !txn.IdentityProvisioned {
			if options.Identity == nil {
				result.Result, result.Error = RecoveryFailed, "identity provisioning is unavailable"
				return txn, result, true
			}
			reboot, err := options.Identity.ProvisionIdentity(ctx)
			if err != nil {
				result.Result, result.Error = RecoveryFailed, "identity provisioning: "+err.Error()
				return txn, result, true
			}
			txn.IdentityProvisioned, txn.IdentityRebootRequired = true, reboot
			if err := checkpoint(); err != nil {
				result.Result, result.Error = RecoveryFailed, err.Error()
				return txn, result, true
			}
		}
		txn.State, txn.Phase = TransactionCommitted, TransactionCommitted
		if err := checkpoint(); err != nil {
			result.Result, result.Error = RecoveryFailed, err.Error()
			return txn, result, true
		}
		result.Result, result.RebootRequired = RecoveryCommitted, txn.IdentityRebootRequired
		return txn, result, false
	case TransactionCommitted:
		result.Result, result.RebootRequired = RecoveryCommitted, txn.IdentityRebootRequired
		return txn, result, false
	case TransactionRollingBack:
		for index := len(units) - 1; index >= 0; index-- {
			if err := RollbackUnit(units[index], options.Mounts, checkpoint); err != nil {
				logf("restore %s: rollback of %s failed: %v", jobID, units[index].Key, err)
				result.Result, result.Error = RecoveryFailed, fmt.Sprintf("rollback %s: %v", units[index].Key, err)
				return txn, result, true
			}
		}
		txn.State, txn.Phase = TransactionRolledBack, TransactionRolledBack
		if err := checkpoint(); err != nil {
			result.Result, result.Error = RecoveryFailed, err.Error()
			return txn, result, true
		}
		result.Result = RecoveryRolledBack
		return txn, result, false
	case TransactionRolledBack:
		result.Result = RecoveryRolledBack
		return txn, result, false
	default:
		// prepared or unknown: nothing was committed, staging is disposable.
		if err := CleanupTransaction(root, jobID, true); err != nil {
			result.Result, result.Error = RecoveryFailed, err.Error()
			return txn, result, true
		}
		logf("restore %s: discarded %s transaction on %s", jobID, txn.State, root)
		result.Result = RecoveryDiscarded
		return txn, result, true
	}
}

// missingLayer returns the first required layer that has not reached a
// terminal state in this sweep, or "" when rollback copies may be removed.
func missingLayer(roots Roots, txn Transaction, current string, seen map[string]string) string {
	for _, required := range txn.RequiredLayers {
		if required == current {
			continue
		}
		if state, ok := seen[required]; ok {
			if state == TransactionCommitted || state == TransactionRolledBack {
				continue
			}
			return required
		}
		root := roots.Userdata
		if required == LayerSD {
			root = roots.SD
		}
		// No copy of this job on the other layer: either that layer already
		// finished and cleaned itself (its .aico-restore directory exists),
		// or the filesystem is not mounted at all.
		if _, err := os.Stat(filepath.Join(root, TransactionDirName)); err != nil {
			return required
		}
	}
	return ""
}

// CleanupTransaction removes staging (and, when removeOld is set, rollback
// copies and the log) for a job below root.
func CleanupTransaction(root, jobID string, removeOld bool) error {
	dir := TransactionDir(root, jobID)
	if err := os.RemoveAll(filepath.Join(dir, "new")); err != nil {
		return err
	}
	if !removeOld {
		return nil
	}
	if err := os.RemoveAll(filepath.Join(dir, "old")); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, TransactionFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(dir, "."+TransactionFileName+".tmp"))
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

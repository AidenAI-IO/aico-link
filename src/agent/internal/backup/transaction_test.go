package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMounts models a bind mount as "mount point path -> source path" and
// records the operations performed so tests can assert their order.
type fakeMounts struct {
	bound      map[string]string
	scripts    map[string]func() error
	ops        []string
	unmountErr error
}

func newFakeMounts() *fakeMounts {
	return &fakeMounts{bound: make(map[string]string), scripts: make(map[string]func() error)}
}

func (f *fakeMounts) IsMountPoint(path string) (bool, error) {
	_, ok := f.bound[path]
	return ok, nil
}

func (f *fakeMounts) Unmount(path string) error {
	f.ops = append(f.ops, "umount "+path)
	if f.unmountErr != nil {
		return f.unmountErr
	}
	delete(f.bound, path)
	return nil
}

func (f *fakeMounts) Remount(script string) error {
	f.ops = append(f.ops, "remount "+script)
	if fn, ok := f.scripts[script]; ok {
		return fn()
	}
	return errors.New("unknown remount script")
}

// bind simulates a bind mount on a single filesystem by making the mount
// point a symlink to the source; SameInode then agrees with a real bind.
func (f *fakeMounts) bind(mountPoint, source string) error {
	_ = os.Remove(mountPoint)
	if err := os.Symlink(source, mountPoint); err != nil {
		return err
	}
	f.bound[mountPoint] = source
	return nil
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestTransactionRoundTripDetectsTampering(t *testing.T) {
	root := t.TempDir()
	txn := Transaction{JobID: "job-1", Layer: LayerUserdata, RequiredLayers: []string{LayerUserdata}, State: TransactionCommitting,
		Units: []*TransactionUnit{{Key: "userdata:agent-memory", Component: ComponentAgentMemory, Layer: LayerUserdata,
			Target: filepath.Join(root, "agent/memory"), Staged: filepath.Join(TransactionDir(root, "job-1"), "new/agent-memory"),
			Old: filepath.Join(TransactionDir(root, "job-1"), "old/agent-memory"), Directory: true}}}
	if err := WriteTransaction(root, txn); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadTransaction(TransactionPath(root, "job-1"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != TransactionCommitting || len(loaded.Units) != 1 || loaded.Units[0].Key != "userdata:agent-memory" {
		t.Fatalf("loaded = %+v", loaded)
	}
	if err := loaded.ValidatePaths(root); err != nil {
		t.Fatal(err)
	}
	data := mustRead(t, TransactionPath(root, "job-1"))
	tampered := strings.Replace(data, `"sha256": "`, `"sha256": "0`, 1)
	if err := os.WriteFile(TransactionPath(root, "job-1"), []byte(tampered[:len(tampered)-1]), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTransaction(TransactionPath(root, "job-1")); err == nil {
		t.Fatal("tampered transaction unexpectedly loaded")
	}

	escaping := txn
	escaping.Units = []*TransactionUnit{{Key: "x", Target: filepath.Join(root, "../outside"), Staged: txn.Units[0].Staged, Old: txn.Units[0].Old}}
	if err := escaping.ValidatePaths(root); err == nil {
		t.Fatal("escaping target unexpectedly validated")
	}
	rootTarget := txn
	rootTarget.Units = []*TransactionUnit{{Key: "x", Target: root, Staged: txn.Units[0].Staged, Old: txn.Units[0].Old}}
	if err := rootTarget.ValidatePaths(root); err == nil {
		t.Fatal("filesystem root target unexpectedly validated")
	}
}

func TestCommitUnitExchangesAndRollbackRestores(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:agent-memory", Component: ComponentAgentMemory, Layer: LayerUserdata,
		Target: filepath.Join(root, "agent/memory"), Staged: filepath.Join(TransactionDir(root, "j"), "new/agent-memory"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/agent-memory"), Directory: true}
	mustWrite(t, filepath.Join(unit.Target, "profile.md"), "old")
	mustWrite(t, filepath.Join(unit.Staged, "profile.md"), "new")
	checkpoints := 0
	if err := CommitUnit(unit, nil, func() error { checkpoints++; return nil }); err != nil {
		t.Fatal(err)
	}
	if !unit.Committed || !unit.HadOriginal || !unit.OldMoved || !unit.NewInstalled || checkpoints < 3 {
		t.Fatalf("unit after commit = %+v (checkpoints %d)", unit, checkpoints)
	}
	if got := mustRead(t, filepath.Join(unit.Target, "profile.md")); got != "new" {
		t.Fatalf("target = %q", got)
	}
	if got := mustRead(t, filepath.Join(unit.Old, "profile.md")); got != "old" {
		t.Fatalf("old = %q", got)
	}
	// Committing again is a no-op.
	if err := CommitUnit(unit, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := RollbackUnit(unit, nil, nil); err != nil {
		t.Fatal(err)
	}
	if unit.Committed || unit.OldMoved || unit.NewInstalled {
		t.Fatalf("unit after rollback = %+v", unit)
	}
	if got := mustRead(t, filepath.Join(unit.Target, "profile.md")); got != "old" {
		t.Fatalf("target after rollback = %q", got)
	}
	if got := mustRead(t, filepath.Join(unit.Staged, "profile.md")); got != "new" {
		t.Fatalf("staged after rollback = %q", got)
	}
}

func TestCommitUnitResumesAfterCrashBetweenRenames(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:agent-config", Component: ComponentAgentConfig, Layer: LayerUserdata,
		Target: filepath.Join(root, "agent/agent.toml"), Staged: filepath.Join(TransactionDir(root, "j"), "new/agent-config"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/agent-config")}
	mustWrite(t, unit.Old, "old")
	mustWrite(t, unit.Staged, "new")
	// Simulate: original already moved to old/, crash before installing new.
	unit.HadOriginal, unit.OldMoved, unit.Unmounted = true, true, true
	if err := CommitUnit(unit, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, unit.Target); got != "new" || !unit.Committed {
		t.Fatalf("resumed commit target=%q unit=%+v", got, unit)
	}
}

func TestCommitAdoptsUncheckpointedOldRenameAndRollbackRestores(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:agent-config", Component: ComponentAgentConfig, Layer: LayerUserdata,
		Target: filepath.Join(root, "agent/agent.toml"), Staged: filepath.Join(TransactionDir(root, "j"), "new/agent-config"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/agent-config")}
	// The rename reached disk but its state checkpoint did not.
	mustWrite(t, unit.Old, "old")
	mustWrite(t, unit.Staged, "new")
	if err := CommitUnit(unit, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !unit.HadOriginal || !unit.OldMoved || mustRead(t, unit.Target) != "new" {
		t.Fatalf("uncheckpointed rename was not adopted: %+v", unit)
	}
	if err := RollbackUnit(unit, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, unit.Target); got != "old" {
		t.Fatalf("rollback target = %q", got)
	}
}

// Power loss after the rollback moved the new version back to new/ but
// before that checkpoint persisted: the log still says NewInstalled, Target is
// absent, and both copies sit in old/ and new/.  Resuming must restore the
// original instead of failing on the already-completed rename.
func TestRollbackResumesAfterUncheckpointedStagedRename(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:agent-memory", Component: ComponentAgentMemory, Layer: LayerUserdata,
		Target: filepath.Join(root, "agent/memory"), Staged: filepath.Join(TransactionDir(root, "j"), "new/agent-memory"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/agent-memory"), Directory: true,
		HadOriginal: true, Unmounted: true, OldMoved: true, NewInstalled: true}
	mustWrite(t, filepath.Join(unit.Old, "profile.md"), "old")
	mustWrite(t, filepath.Join(unit.Staged, "profile.md"), "new")
	checkpoints := 0
	if err := RollbackUnit(unit, nil, func() error { checkpoints++; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(unit.Target, "profile.md")); got != "old" {
		t.Fatalf("target after resumed rollback = %q", got)
	}
	if got := mustRead(t, filepath.Join(unit.Staged, "profile.md")); got != "new" {
		t.Fatalf("staged copy after resumed rollback = %q", got)
	}
	if unit.NewInstalled || unit.OldMoved || unit.Committed || checkpoints == 0 {
		t.Fatalf("unit after resumed rollback = %+v (checkpoints %d)", unit, checkpoints)
	}
	if _, err := os.Stat(unit.Old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old copy still present after rollback: %v", err)
	}
}

func TestRollbackPreservesAllCopiesWhenStagingIsOccupied(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:user-home", Target: filepath.Join(root, "userhome"),
		Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"), Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"),
		HadOriginal: true, OldMoved: true, NewInstalled: true}
	mustWrite(t, filepath.Join(unit.Target, "value"), "new")
	mustWrite(t, filepath.Join(unit.Old, "value"), "old")
	mustWrite(t, filepath.Join(unit.Staged, "value"), "collision")
	if err := RollbackUnit(unit, nil, nil); err == nil {
		t.Fatal("rollback replaced occupied staging")
	}
	for path, want := range map[string]string{unit.Target: "new", unit.Old: "old", unit.Staged: "collision"} {
		if got := mustRead(t, filepath.Join(path, "value")); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestRollbackRemountsLiveSourceWhenCheckpointFails(t *testing.T) {
	root := t.TempDir()
	mounts := newFakeMounts()
	mountPoint := filepath.Join(t.TempDir(), "root")
	target := filepath.Join(root, "userhome")
	unit := &TransactionUnit{Key: "userdata:user-home", Target: target,
		Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"), Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"),
		BindMount: mountPoint, RemountScript: "/usr/lib/aico/aico-root-home",
		HadOriginal: true, OldMoved: true, NewInstalled: true, Unmounted: true, Remounted: true, Committed: true}
	mustWrite(t, filepath.Join(unit.Target, "value"), "new")
	mustWrite(t, filepath.Join(unit.Old, "value"), "old")
	if err := mounts.bind(mountPoint, target); err != nil {
		t.Fatal(err)
	}
	mounts.scripts[unit.RemountScript] = func() error { return mounts.bind(mountPoint, target) }
	checkpointErr := errors.New("checkpoint failed")
	if err := RollbackUnit(unit, mounts, func() error { return checkpointErr }); !errors.Is(err, checkpointErr) {
		t.Fatalf("rollback error = %v, want checkpoint failure", err)
	}
	if source, ok := mounts.bound[mountPoint]; !ok || source != target {
		t.Fatalf("live source was not remounted after rollback failure: %q, %v", source, ok)
	}
	if got := mustRead(t, filepath.Join(unit.Target, "value")); got != "new" {
		t.Fatalf("target changed before durable rollback checkpoint: %q", got)
	}
}

func TestBindMountRollbackRequiresMountController(t *testing.T) {
	root := t.TempDir()
	unit := &TransactionUnit{Key: "userdata:user-home", Component: ComponentUserHome, Layer: LayerUserdata,
		Target: filepath.Join(root, "userhome"), Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"), Directory: true,
		BindMount: filepath.Join(t.TempDir(), "root"), RemountScript: "/usr/lib/aico/aico-root-home",
		HadOriginal: true, OldMoved: true, NewInstalled: true}
	mustWrite(t, filepath.Join(unit.Target, ".bashrc"), "new")
	mustWrite(t, filepath.Join(unit.Old, ".bashrc"), "old")
	if err := RollbackUnit(unit, nil, nil); err == nil || !strings.Contains(err.Error(), "mount controller") {
		t.Fatalf("rollback without mount controller = %v", err)
	}
	if got := mustRead(t, filepath.Join(unit.Target, ".bashrc")); got != "new" {
		t.Fatalf("target was exchanged without a mount controller: %q", got)
	}
	if !unit.NewInstalled || !unit.OldMoved {
		t.Fatalf("unit mutated without a mount controller: %+v", unit)
	}
}

func TestBindMountUnitUnmountsExchangesAndRemounts(t *testing.T) {
	root := t.TempDir()
	mounts := newFakeMounts()
	mountPoint := filepath.Join(t.TempDir(), "root")
	target := filepath.Join(root, "userhome")
	mustWrite(t, filepath.Join(target, ".bashrc"), "old")
	if err := mounts.bind(mountPoint, target); err != nil {
		t.Fatal(err)
	}
	unit := &TransactionUnit{Key: "userdata:user-home", Component: ComponentUserHome, Layer: LayerUserdata,
		Target: target, Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"), Directory: true,
		BindMount: mountPoint, RemountScript: "/usr/lib/aico/aico-root-home"}
	mustWrite(t, filepath.Join(unit.Staged, ".bashrc"), "new")
	mounts.scripts[unit.RemountScript] = func() error { return mounts.bind(mountPoint, target) }

	if err := CommitUnit(unit, mounts, nil); err != nil {
		t.Fatal(err)
	}
	if !unit.Unmounted || !unit.Remounted || !unit.Committed {
		t.Fatalf("unit = %+v", unit)
	}
	if got := mustRead(t, filepath.Join(mountPoint, ".bashrc")); got != "new" {
		t.Fatalf("mount point shows %q after commit", got)
	}
	if len(mounts.ops) != 2 || mounts.ops[0] != "umount "+mountPoint || mounts.ops[1] != "remount "+unit.RemountScript {
		t.Fatalf("mount ops = %v", mounts.ops)
	}

	if err := RollbackUnit(unit, mounts, nil); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(mountPoint, ".bashrc")); got != "old" {
		t.Fatalf("mount point shows %q after rollback", got)
	}
	if unit.Unmounted || unit.Remounted || unit.Committed {
		t.Fatalf("unit after rollback = %+v", unit)
	}
}

func TestBindMountUnmountFailureLeavesTargetUntouched(t *testing.T) {
	root := t.TempDir()
	mounts := newFakeMounts()
	mounts.unmountErr = errors.New("target is busy")
	mountPoint := filepath.Join(t.TempDir(), "root")
	target := filepath.Join(root, "userhome")
	mustWrite(t, filepath.Join(target, ".bashrc"), "old")
	if err := mounts.bind(mountPoint, target); err != nil {
		t.Fatal(err)
	}
	unit := &TransactionUnit{Key: "userdata:user-home", Component: ComponentUserHome, Layer: LayerUserdata,
		Target: target, Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"), Directory: true,
		BindMount: mountPoint, RemountScript: "/usr/lib/aico/aico-root-home"}
	mustWrite(t, filepath.Join(unit.Staged, ".bashrc"), "new")
	if err := CommitUnit(unit, mounts, nil); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("commit with busy mount = %v", err)
	}
	if unit.OldMoved || unit.NewInstalled || unit.Unmounted {
		t.Fatalf("unit mutated after unmount failure: %+v", unit)
	}
	if got := mustRead(t, filepath.Join(target, ".bashrc")); got != "old" {
		t.Fatalf("target changed: %q", got)
	}
	if _, err := os.Stat(unit.Old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback directory unexpectedly created: %v", err)
	}
}

func TestBindMountRefusesForeignSource(t *testing.T) {
	root := t.TempDir()
	mounts := newFakeMounts()
	mountPoint := filepath.Join(t.TempDir(), "root")
	target := filepath.Join(root, "userhome")
	other := filepath.Join(root, "elsewhere")
	mustWrite(t, filepath.Join(target, ".bashrc"), "old")
	mustWrite(t, filepath.Join(other, ".bashrc"), "foreign")
	if err := mounts.bind(mountPoint, other); err != nil {
		t.Fatal(err)
	}
	unit := &TransactionUnit{Key: "userdata:user-home", Component: ComponentUserHome, Layer: LayerUserdata,
		Target: target, Staged: filepath.Join(TransactionDir(root, "j"), "new/user-home"),
		Old: filepath.Join(TransactionDir(root, "j"), "old/user-home"), Directory: true,
		BindMount: mountPoint, RemountScript: "/usr/lib/aico/aico-root-home"}
	mustWrite(t, filepath.Join(unit.Staged, ".bashrc"), "new")
	if err := CommitUnit(unit, mounts, nil); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("commit with foreign mount source = %v", err)
	}
	if len(mounts.ops) != 0 {
		t.Fatalf("mount ops = %v", mounts.ops)
	}
}

type fakeProvisioner struct {
	calls  int
	reboot bool
	err    error
}

func (p *fakeProvisioner) ProvisionIdentity(context.Context) (bool, error) {
	p.calls++
	return p.reboot, p.err
}

func TestRecoverTransactionsFinishesCommitAndDiscardsOrphans(t *testing.T) {
	roots := Roots{Userdata: t.TempDir(), SD: t.TempDir()}
	jobID := "job-commit"
	target := filepath.Join(roots.Userdata, "agent/memory")
	staged := filepath.Join(TransactionDir(roots.Userdata, jobID), "new/agent-memory")
	old := filepath.Join(TransactionDir(roots.Userdata, jobID), "old/agent-memory")
	mustWrite(t, filepath.Join(target, "profile.md"), "old")
	mustWrite(t, filepath.Join(staged, "profile.md"), "new")
	txn := Transaction{JobID: jobID, Layer: LayerUserdata, RequiredLayers: []string{LayerUserdata}, State: TransactionCommitting,
		IdentitySelected: true,
		Units: []*TransactionUnit{{Key: "userdata:agent-memory", Component: ComponentAgentMemory, Layer: LayerUserdata,
			Target: target, Staged: staged, Old: old, Directory: true}}}
	if err := WriteTransaction(roots.Userdata, txn); err != nil {
		t.Fatal(err)
	}
	// An interrupted ingest leaves staging without a transaction log.
	orphan := filepath.Join(TransactionDir(roots.Userdata, "job-orphan"), "new/agent-sessions/chat.json")
	mustWrite(t, orphan, "{}")
	// A log for a job that never reached commit is disposable staging.
	prepared := Transaction{JobID: "job-prepared", Layer: LayerUserdata, RequiredLayers: []string{LayerUserdata}, State: TransactionPrepared}
	if err := WriteTransaction(roots.Userdata, prepared); err != nil {
		t.Fatal(err)
	}

	provisioner := &fakeProvisioner{reboot: true}
	results, err := RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots, Mounts: newFakeMounts(), Identity: provisioner})
	if err != nil {
		t.Fatalf("recover: %v (%+v)", err, results)
	}
	byJob := make(map[string]RecoveryResult)
	for _, result := range results {
		byJob[result.JobID] = result
	}
	if byJob[jobID].Result != RecoveryCommitted || !byJob[jobID].RebootRequired {
		t.Fatalf("commit result = %+v", byJob[jobID])
	}
	if byJob["job-orphan"].Result != RecoveryDiscarded || byJob["job-prepared"].Result != RecoveryDiscarded {
		t.Fatalf("results = %+v", byJob)
	}
	if provisioner.calls != 1 {
		t.Fatalf("identity provisioner calls = %d", provisioner.calls)
	}
	if got := mustRead(t, filepath.Join(target, "profile.md")); got != "new" {
		t.Fatalf("target after recovery = %q", got)
	}
	for _, name := range []string{jobID, "job-orphan", "job-prepared"} {
		if _, err := os.Stat(TransactionDir(roots.Userdata, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transaction dir %s still exists: %v", name, err)
		}
	}
	// A second sweep is a no-op.
	again, err := RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots, Mounts: newFakeMounts(), Identity: provisioner})
	if err != nil || len(again) != 0 || provisioner.calls != 1 {
		t.Fatalf("second sweep = %+v err=%v calls=%d", again, err, provisioner.calls)
	}
}

func TestRecoverTransactionsRollsBackAndWaitsForMissingSD(t *testing.T) {
	roots := Roots{Userdata: t.TempDir(), SD: t.TempDir()}
	jobID := "job-rollback"
	target := filepath.Join(roots.Userdata, "agent/memory")
	staged := filepath.Join(TransactionDir(roots.Userdata, jobID), "new/agent-memory")
	old := filepath.Join(TransactionDir(roots.Userdata, jobID), "old/agent-memory")
	mustWrite(t, filepath.Join(target, "profile.md"), "new")
	mustWrite(t, filepath.Join(old, "profile.md"), "old")
	unit := &TransactionUnit{Key: "userdata:agent-memory", Component: ComponentAgentMemory, Layer: LayerUserdata,
		Target: target, Staged: staged, Old: old, Directory: true, HadOriginal: true, Unmounted: true, OldMoved: true, NewInstalled: true}
	txn := Transaction{JobID: jobID, Layer: LayerUserdata, RequiredLayers: []string{LayerUserdata}, State: TransactionRollingBack, Units: []*TransactionUnit{unit}}
	if err := WriteTransaction(roots.Userdata, txn); err != nil {
		t.Fatal(err)
	}
	results, err := RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots, Mounts: newFakeMounts()})
	if err != nil || len(results) != 1 || results[0].Result != RecoveryRolledBack {
		t.Fatalf("rollback results = %+v err=%v", results, err)
	}
	if got := mustRead(t, filepath.Join(target, "profile.md")); got != "old" {
		t.Fatalf("target after rollback = %q", got)
	}

	// A committed userdata half whose SD half is absent must keep its rollback
	// copies until the card returns.
	waiting := "job-waiting"
	mustWrite(t, filepath.Join(TransactionDir(roots.Userdata, waiting), "old/agent-memory/profile.md"), "old")
	txn = Transaction{JobID: waiting, Layer: LayerUserdata, RequiredLayers: []string{LayerUserdata, LayerSD}, State: TransactionCommitted}
	if err := WriteTransaction(roots.Userdata, txn); err != nil {
		t.Fatal(err)
	}
	results, err = RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots, Mounts: newFakeMounts()})
	if err != nil || len(results) != 1 || results[0].Result != RecoveryWaiting || results[0].MissingLayer != LayerSD {
		t.Fatalf("waiting results = %+v err=%v", results, err)
	}
	if _, err := os.Stat(TransactionPath(roots.Userdata, waiting)); err != nil {
		t.Fatalf("waiting transaction was removed: %v", err)
	}
	// Once the SD half shows up and completes, both halves are cleaned.
	sdTxn := Transaction{JobID: waiting, Layer: LayerSD, RequiredLayers: []string{LayerUserdata, LayerSD}, State: TransactionCommitted}
	if err := WriteTransaction(roots.SD, sdTxn); err != nil {
		t.Fatal(err)
	}
	results, err = RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots, Mounts: newFakeMounts()})
	if err != nil || len(results) != 2 {
		t.Fatalf("final results = %+v err=%v", results, err)
	}
	for _, result := range results {
		if result.Result != RecoveryCommitted {
			t.Fatalf("final results = %+v", results)
		}
	}
	for _, root := range []string{roots.Userdata, roots.SD} {
		if _, err := os.Stat(TransactionDir(root, waiting)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("transaction dir below %s still exists: %v", root, err)
		}
	}
}

func TestRecoverTransactionsFailsClosedOnDamagedLog(t *testing.T) {
	roots := Roots{Userdata: t.TempDir(), SD: ""}
	jobID := "job-damaged"
	if err := os.MkdirAll(TransactionDir(roots.Userdata, jobID), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, TransactionPath(roots.Userdata, jobID), "{not json")
	results, err := RecoverTransactions(context.Background(), RecoveryOptions{Roots: roots})
	if err == nil || len(results) != 1 || results[0].Result != RecoveryFailed {
		t.Fatalf("damaged results = %+v err=%v", results, err)
	}
	if _, statErr := os.Stat(TransactionPath(roots.Userdata, jobID)); statErr != nil {
		t.Fatalf("damaged log was removed: %v", statErr)
	}
}

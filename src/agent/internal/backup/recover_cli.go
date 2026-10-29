package backup

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// RecoveryStatus is the document written for Config Web and operators after
// a boot-time sweep.
type RecoveryStatus struct {
	UpdatedAt time.Time        `json:"updated_at"`
	OK        bool             `json:"ok"`
	Results   []RecoveryResult `json:"results"`
}

// RunRecover implements `agent backup-recover`, the oneshot executed by
// aico-backup-recover.service before the Agent starts.  It exits non-zero
// (and leaves the failure marker) only when formal data may be inconsistent.
func RunRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup-recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	roots := DefaultRoots()
	fs.StringVar(&roots.Userdata, "userdata-root", roots.Userdata, "userdata root")
	fs.StringVar(&roots.SD, "sd-root", roots.SD, "SD-card root")
	statusPath := fs.String("status", "/run/aico/backup/recovery.json", "status document path")
	failedMarker := fs.String("failed-marker", "/run/aico/backup/recovery.failed", "marker created when formal data may be inconsistent")
	otaBinary := fs.String("ota-binary", "/usr/lib/aico/ota", "OTA binary used for identity provisioning")
	otaConfig := fs.String("ota-config", "/userdata/debian/ota/config.json", "OTA configuration path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := roots.Validate(); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	logf := func(format string, values ...any) { fmt.Fprintf(stderr, "backup-recover: "+format+"\n", values...) }
	results, err := RecoverTransactions(context.Background(), RecoveryOptions{
		Roots:    roots,
		Mounts:   SystemMountController{},
		Identity: OTAIdentityProvisioner{Binary: *otaBinary, ConfigPath: *otaConfig},
		Logf:     logf,
	})
	status := RecoveryStatus{UpdatedAt: time.Now().UTC(), OK: err == nil, Results: results}
	if results == nil {
		status.Results = []RecoveryResult{}
	}
	if writeErr := writeRecoveryStatus(*statusPath, status); writeErr != nil {
		logf("write status: %v", writeErr)
	}
	if err != nil {
		if *failedMarker != "" {
			_ = os.MkdirAll(filepath.Dir(*failedMarker), 0o755)
			_ = os.WriteFile(*failedMarker, []byte(err.Error()+"\n"), 0o644)
		}
		logf("recovery failed: %v", err)
		return 1
	}
	if *failedMarker != "" {
		_ = os.Remove(*failedMarker)
	}
	for _, result := range results {
		if result.RebootRequired {
			// aico-machine-id.service runs provision-identity again and reboots
			// itself when required; report it so the log explains the reboot.
			logf("restore %s: identity provisioning requests a reboot", result.JobID)
		}
	}
	encoded, _ := json.Marshal(status)
	fmt.Fprintln(stdout, string(encoded))
	return 0
}

func writeRecoveryStatus(path string, status RecoveryStatus) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return SyncParent(path)
}

//go:build unix

package configupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockConfigUpdates serializes read-modify-write cycles on one config file
// across processes: Config Web, the config CLI, and the Agent's own config
// tool all persist agent.toml through Update.
func lockConfigUpdates(resolvedPath string) (func(), error) {
	lockPath := filepath.Join(filepath.Dir(resolvedPath), "."+filepath.Base(resolvedPath)+".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open config lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock config: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

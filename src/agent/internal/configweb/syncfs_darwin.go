package configweb

import "fmt"

// Board backup/restore durability depends on Linux syncfs. A desktop daemon
// must not claim the same guarantee by substituting a single-file fsync.
func syncFilesystem(fd int) error {
	return fmt.Errorf("board filesystem backup/restore requires Linux syncfs; unavailable on macOS")
}

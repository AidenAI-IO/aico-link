package backup

import "golang.org/x/sys/unix"

// O_EVTONLY opens metadata without reading a FIFO or device. O_SYMLINK opens
// the final symlink itself; openSourcePath still rejects parent symlinks.
const sourceMetadataOpenFlags = unix.O_EVTONLY | unix.O_SYMLINK | unix.O_NONBLOCK

func sourceOpenFlags(flags int) int {
	if flags&unix.O_SYMLINK != 0 {
		// Darwin rejects O_NOFOLLOW even with O_SYMLINK. O_SYMLINK itself
		// requests a descriptor for the link, rather than its destination.
		return flags | unix.O_CLOEXEC
	}
	return flags | unix.O_NOFOLLOW | unix.O_CLOEXEC
}

package backup

import "golang.org/x/sys/unix"

const sourceMetadataOpenFlags = unix.O_PATH

func sourceOpenFlags(flags int) int { return flags | unix.O_NOFOLLOW | unix.O_CLOEXEC }

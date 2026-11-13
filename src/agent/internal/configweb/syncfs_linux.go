package configweb

import "golang.org/x/sys/unix"

func syncFilesystem(fd int) error { return unix.Syncfs(fd) }

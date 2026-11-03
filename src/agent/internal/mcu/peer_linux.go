//go:build linux

package mcu

import (
	"net"

	"golang.org/x/sys/unix"
)

// Check the kernel-provided peer identity, never a request-supplied identity.
func poweroffPeerIsRoot(c net.Conn) bool {
	conn, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	err = raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		allowed = err == nil && cred.Uid == 0
	})
	return err == nil && allowed
}

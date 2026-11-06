//go:build linux

package lanaccess

import (
	"context"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// ListenOnDevice listens on address with SO_REUSEPORT, so it can share a port
// with a wildcard listener, and binds it to device when one is given. The
// kernel then delivers only packets that entered through that interface, and
// prefers this socket over the wildcard one for them.
func ListenOnDevice(network, address, device string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, connection syscall.RawConn) error {
		var socketErr error
		if err := connection.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			if socketErr == nil && device != "" {
				socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	return config.Listen(context.Background(), network, address)
}

//go:build !linux

package lanaccess

import (
	"context"
	"fmt"
	"net"
)

// ListenOnDevice binds a plain listener on hosts without SO_BINDTODEVICE.
// Device binding is refused so a host build can never mark a wildcard socket
// as the trusted USB ingress.
func ListenOnDevice(network, address, device string) (net.Listener, error) {
	if device != "" {
		return nil, fmt.Errorf("binding to interface %q is only supported on Linux", device)
	}
	var config net.ListenConfig
	return config.Listen(context.Background(), network, address)
}

//go:build !linux

package mcu

import "net"

func poweroffPeerIsRoot(net.Conn) bool { return false }

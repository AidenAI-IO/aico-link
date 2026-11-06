package agent

import (
	"errors"
	"net"
	"net/http"
	"strings"

	"aico-agent/internal/lanaccess"
)

// USBIngress names the USB link whose connections bypass the LAN guard.
type USBIngress struct {
	Interface string // e.g. usb0
	Address   string // the board's address on that link, e.g. 192.168.42.1
}

// EnableLANGuard refuses requests from outside the USB link and the board
// while LAN access is off. Call before Start.
func (s *Server) EnableLANGuard(state *lanaccess.State, usb USBIngress) {
	s.lanAccess = state
	s.usbIngress = USBIngress{Interface: strings.TrimSpace(usb.Interface), Address: strings.TrimSpace(usb.Address)}
}

// listenMain binds the main listener. With the guard on it is IPv4 only, so
// IPv6 cannot be used to reach the Agent around the USB socket, and it sets
// SO_REUSEPORT so the interface-bound USB socket can share the port.
func (s *Server) listenMain() (net.Listener, error) {
	if s.lanAccess == nil || s.usbIngress.Interface == "" {
		return net.Listen("tcp", s.addr)
	}
	return lanaccess.ListenOnDevice("tcp4", s.addr, "")
}

// startUSBListener serves srv on a socket bound to the USB interface. Only
// its connections are trusted; until it is bound, USB clients reach the main
// listener and are treated like any other LAN client.
func (s *Server) startUSBListener(srv *http.Server) {
	if s.lanAccess == nil || s.usbIngress.Interface == "" || s.usbIngress.Address == "" {
		return
	}
	_, port, err := net.SplitHostPort(s.addr)
	if err != nil {
		return
	}
	address := net.JoinHostPort(s.usbIngress.Address, port)
	stop := make(chan struct{})
	s.httpMu.Lock()
	s.usbListenStop = stop
	s.httpMu.Unlock()
	go func() {
		listener, err := lanaccess.RetryListen(func() (net.Listener, error) {
			return lanaccess.ListenOnDevice("tcp4", address, s.usbIngress.Interface)
		}, stop, func(attempt int, err error) {
			if attempt == 1 && s.logger != nil {
				s.logger.Warn("USB listener %s (%s) unavailable, retrying: %v", address, s.usbIngress.Interface, err)
			}
		})
		if err != nil {
			return
		}
		s.httpMu.Lock()
		if s.closing.Load() {
			s.httpMu.Unlock()
			_ = listener.Close()
			return
		}
		s.usbListener = listener
		s.httpMu.Unlock()
		if s.logger != nil {
			s.logger.Info("USB listener ready on %s (%s)", address, s.usbIngress.Interface)
		}
		if err := srv.Serve(lanaccess.TrustListener(listener)); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && s.logger != nil {
			s.logger.Warn("USB listener stopped: %v", err)
		}
	}()
}

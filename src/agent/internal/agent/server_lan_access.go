package agent

import (
	"crypto/tls"
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
	return s.listenShare(s.addr)
}

func (s *Server) listenShare(addr string) (net.Listener, error) {
	if s.lanAccess == nil || s.usbIngress.Interface == "" {
		return net.Listen("tcp", addr)
	}
	return lanaccess.ListenOnDevice("tcp4", addr, "")
}

// startUSBListener serves srv on a socket bound to the USB interface. Only
// its connections are trusted; until it is bound, USB clients reach the main
// listener and are treated like any other LAN client. A non-nil tlsConfig
// serves HTTPS. It is passed explicitly because net/http fills in
// srv.TLSConfig on a plain server when it starts serving.
func (s *Server) startUSBListener(srv *http.Server, tlsConfig *tls.Config) {
	if s.lanAccess == nil || s.usbIngress.Interface == "" || s.usbIngress.Address == "" {
		return
	}
	_, port, err := net.SplitHostPort(srv.Addr)
	if err != nil {
		return
	}
	address := net.JoinHostPort(s.usbIngress.Address, port)
	stop := make(chan struct{})
	s.httpMu.Lock()
	s.usbListenStops = append(s.usbListenStops, stop)
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
		s.usbListeners = append(s.usbListeners, listener)
		s.httpMu.Unlock()
		if s.logger != nil {
			s.logger.Info("USB listener ready on %s (%s)", address, s.usbIngress.Interface)
		}
		trusted := lanaccess.TrustListener(listener)
		if tlsConfig != nil {
			// The trust marker must wrap the raw connection: the TLS server
			// wraps each accepted conn, and ConnContext sees the inner one.
			trusted = tls.NewListener(trusted, tlsConfig)
		}
		if err := srv.Serve(trusted); err != nil &&
			!errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && s.logger != nil {
			s.logger.Warn("USB listener stopped: %v", err)
		}
	}()
}

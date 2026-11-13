package agent

import (
	"errors"
	"net"
	"net/http"
	"strconv"

	"aico-agent/internal/lanaccess"
	"aico-agent/internal/localtls"
)

// EnableHTTPS serves the Web UI and API over HTTPS on addr as well, for
// browser features that need a secure page, such as Web UI voice. Certificates
// come from the board's local authority in tlsDir, which Config Web shares.
// Call before Start.
func (s *Server) EnableHTTPS(addr, tlsDir string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if _, httpPort, err := net.SplitHostPort(s.addr); err == nil && port == httpPort {
		// With the LAN guard on, listeners share ports, so this would bind
		// and then split connections between HTTP and HTTPS.
		return errors.New("the HTTPS port must differ from the HTTP port")
	}
	authority, err := localtls.Load(tlsDir)
	if err != nil {
		return err
	}
	s.httpsAddr = addr
	s.httpsAuthority = authority
	return nil
}

// HTTPSPort returns the HTTPS port while its listener is serving, or 0. The
// Web UI sends users there for microphone access, so a port that failed to
// bind must not be reported.
func (s *Server) HTTPSPort() int {
	if s == nil || s.httpsAuthority == nil || !s.httpsServing.Load() {
		return 0
	}
	_, port, err := net.SplitHostPort(s.httpsAddr)
	if err != nil {
		return 0
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return value
}

// startHTTPS binds the HTTPS listeners. A failure leaves plain HTTP serving
// and is only logged: HTTPS adds browser features but is not required.
func (s *Server) startHTTPS(handler http.Handler) {
	if s.httpsAuthority == nil {
		return
	}
	srv := &http.Server{
		Addr:              s.httpsAddr,
		Handler:           handler,
		ReadHeaderTimeout: agentHTTPReadHeaderTimeout,
		ReadTimeout:       agentHTTPReadTimeout,
		IdleTimeout:       agentHTTPIdleTimeout,
		ConnContext:       lanaccess.ConnContext,
	}
	s.httpsAuthority.Configure(srv)
	listener, err := s.listenShare(s.httpsAddr)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("HTTPS listener unavailable on %s: %v", s.httpsAddr, err)
		}
		return
	}
	s.httpMu.Lock()
	if s.closing.Load() {
		s.httpMu.Unlock()
		_ = listener.Close()
		return
	}
	s.httpsServer = srv
	s.httpMu.Unlock()
	if s.logger != nil {
		s.logger.Info("Starting HTTPS server on %s", s.httpsAddr)
	}
	s.startUSBListener(srv, true)
	s.httpsServing.Store(true)
	go func() {
		err := srv.ServeTLS(listener, "", "")
		s.httpsServing.Store(false)
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && s.logger != nil {
			s.logger.Warn("HTTPS listener stopped: %v", err)
		}
	}()
}

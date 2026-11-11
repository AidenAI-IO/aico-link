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
	authority, err := localtls.Load(tlsDir)
	if err != nil {
		return err
	}
	s.httpsAddr = addr
	s.httpsAuthority = authority
	return nil
}

// HTTPSPort returns the configured HTTPS port, or 0 when HTTPS is off.
func (s *Server) HTTPSPort() int {
	if s == nil || s.httpsAuthority == nil {
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
		TLSConfig:         s.httpsAuthority.TLSConfig(),
		ReadHeaderTimeout: agentHTTPReadHeaderTimeout,
		ReadTimeout:       agentHTTPReadTimeout,
		IdleTimeout:       agentHTTPIdleTimeout,
		ConnContext:       lanaccess.ConnContext,
	}
	localtls.DisableHTTP2(srv)
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
	s.startUSBListener(srv, srv.TLSConfig)
	go func() {
		err := srv.ServeTLS(listener, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && s.logger != nil {
			s.logger.Warn("HTTPS listener stopped: %v", err)
		}
	}()
}

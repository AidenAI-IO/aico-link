package agent

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"aico-agent/internal/lanaccess"
)

// The HTTPS listener exists for browser features that require a secure
// context, chiefly microphone access for Web UI realtime voice. The board has
// no stable public name, so the agent keeps a local certificate authority in
// its data directory and issues a short-lived certificate for whichever local
// address a client connected to: 192.168.42.1 over USB, a Wi-Fi address, or
// localhost. A browser that has not trusted the authority shows a certificate
// warning once; after the user accepts it the page is still a secure context.
//
// The authority is name-constrained to private and loopback addresses and to
// localhost, so trusting it cannot be used to impersonate a public site.
const (
	httpsCAFileName    = "ca.pem"
	httpsCAKeyFileName = "ca-key.pem"
	// macOS rejects TLS server certificates valid for more than 825 days even
	// under a user-trusted root; stay well inside that and the 398-day limit
	// browsers enforce.
	httpsLeafLifetime = 180 * 24 * time.Hour
	// Renew a cached leaf this long before it expires.
	httpsLeafRenewal = 7 * 24 * time.Hour
	// CA validity is fixed rather than relative to the clock: the board may
	// boot with an unsynchronized clock, and a CA minted then must still be
	// valid once time is corrected.
	httpsCAPath = "/tls/ca.pem"
)

var (
	httpsCANotBefore = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	httpsCANotAfter  = time.Date(2049, 12, 31, 0, 0, 0, 0, time.UTC)
)

// httpsCertificates issues server certificates from the local authority.
type httpsCertificates struct {
	caCert *x509.Certificate
	caPEM  []byte
	caKey  *ecdsa.PrivateKey
	now    func() time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// loadHTTPSCertificates loads the local authority from dir, creating it on
// first use.
func loadHTTPSCertificates(dir string) (*httpsCertificates, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create TLS directory: %w", err)
	}
	certPath := filepath.Join(dir, httpsCAFileName)
	keyPath := filepath.Join(dir, httpsCAKeyFileName)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if errors.Is(certErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist) {
		var err error
		certPEM, keyPEM, err = createHTTPSAuthority()
		if err != nil {
			return nil, err
		}
		// Write the key first: a certificate without its key is unusable,
		// while a lone key is replaced on the next start.
		if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
			return nil, fmt.Errorf("write TLS authority key: %w", err)
		}
		if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
			return nil, fmt.Errorf("write TLS authority: %w", err)
		}
	} else if certErr != nil {
		return nil, fmt.Errorf("read TLS authority: %w", certErr)
	} else if keyErr != nil {
		return nil, fmt.Errorf("read TLS authority key: %w", keyErr)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load TLS authority: %w", err)
	}
	caCert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse TLS authority: %w", err)
	}
	caKey, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !caCert.IsCA {
		return nil, errors.New("TLS authority must be an ECDSA certificate authority")
	}
	return &httpsCertificates{caCert: caCert, caPEM: certPEM, caKey: caKey, now: time.Now, leaves: make(map[string]*tls.Certificate)}, nil
}

func createHTTPSAuthority() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate TLS authority key: %w", err)
	}
	serial, err := httpsSerialNumber()
	if err != nil {
		return nil, nil, err
	}
	name := "Aico Link Local CA"
	if host, _ := os.Hostname(); host != "" {
		name += " (" + host + ")"
	}
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: name, Organization: []string{"Aico Link"}},
		NotBefore:                   httpsCANotBefore,
		NotAfter:                    httpsCANotAfter,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
		PermittedIPRanges:           httpsPermittedIPRanges(),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create TLS authority: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode TLS authority key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// httpsPermittedIPRanges are the addresses a board can be reached at:
// loopback, private IPv4, link-local, and IPv6 unique-local/link-local.
func httpsPermittedIPRanges() []*net.IPNet {
	var ranges []*net.IPNet
	for _, cidr := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
		"::1/128", "fc00::/7", "fe80::/10",
	} {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			ranges = append(ranges, network)
		}
	}
	return ranges
}

func httpsSerialNumber() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}

// GetCertificate implements tls.Config.GetCertificate. Clients that connect by
// IP address send no server name, so the certificate is chosen by the local
// address the connection arrived on.
func (c *httpsCertificates) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	ip := net.IPv4(127, 0, 0, 1)
	if hello.Conn != nil {
		if addr, ok := hello.Conn.LocalAddr().(*net.TCPAddr); ok && addr.IP != nil && !addr.IP.IsUnspecified() {
			ip = addr.IP
		}
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	key := ip.String()
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf := c.leaves[key]; leaf != nil && leaf.Leaf != nil &&
		now.After(leaf.Leaf.NotBefore) && now.Add(httpsLeafRenewal).Before(leaf.Leaf.NotAfter) {
		return leaf, nil
	}
	leaf, err := c.issue(ip, now)
	if err != nil {
		return nil, err
	}
	c.leaves[key] = leaf
	return leaf, nil
}

func (c *httpsCertificates) issue(ip net.IP, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := httpsSerialNumber()
	if err != nil {
		return nil, err
	}
	ips := []net.IP{ip}
	if !ip.IsLoopback() {
		ips = append(ips, net.IPv4(127, 0, 0, 1).To4())
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: ip.String(), Organization: []string{"Aico Link"}},
		// Allow for a client clock running behind the board's.
		NotBefore:   now.Add(-24 * time.Hour),
		NotAfter:    now.Add(httpsLeafLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.caCert, &key.PublicKey, c.caKey)
	if err != nil {
		return nil, fmt.Errorf("issue TLS certificate for %s: %w", ip, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, c.caCert.Raw}, PrivateKey: key, Leaf: leaf}, nil
}

// tlsConfig pins HTTP/1.1. The voice socket and the event stream rely on
// HTTP/1.1 connection hijacking and flushing, which HTTP/2 does not provide,
// and both HTTPS listeners must negotiate the same protocol.
func (c *httpsCertificates) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: c.GetCertificate, NextProtos: []string{"http/1.1"}}
}

// handleTLSAuthority serves the public certificate of the local authority so
// a user can install it and open the HTTPS Web UI without a warning.
func (c *httpsCertificates) handleTLSAuthority(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="aico-link-ca.pem"`)
	_, _ = w.Write(c.caPEM)
}

// EnableHTTPS serves the Web UI and API over HTTPS on addr as well, with
// certificates from the local authority kept in tlsDir. Call before Start.
func (s *Server) EnableHTTPS(addr, tlsDir string) error {
	certificates, err := loadHTTPSCertificates(tlsDir)
	if err != nil {
		return err
	}
	s.httpsAddr = addr
	s.httpsCertificates = certificates
	return nil
}

// HTTPSPort returns the configured HTTPS port, or 0 when HTTPS is off.
func (s *Server) HTTPSPort() int {
	if s == nil || s.httpsCertificates == nil {
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
	if s.httpsCertificates == nil {
		return
	}
	srv := &http.Server{
		Addr:      s.httpsAddr,
		Handler:   handler,
		TLSConfig: s.httpsCertificates.tlsConfig(),
		// A non-nil empty map keeps net/http from enabling HTTP/2 on ServeTLS.
		TLSNextProto:      map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ReadHeaderTimeout: agentHTTPReadHeaderTimeout,
		ReadTimeout:       agentHTTPReadTimeout,
		IdleTimeout:       agentHTTPIdleTimeout,
		ConnContext:       lanaccess.ConnContext,
	}
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
	s.startUSBListener(srv)
	go func() {
		err := srv.ServeTLS(listener, "", "")
		if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) && s.logger != nil {
			s.logger.Warn("HTTPS listener stopped: %v", err)
		}
	}()
}

// Package localtls issues HTTPS certificates for the board's local web
// services from a certificate authority kept on the board.
//
// Browsers expose features such as the microphone only to secure pages, and
// the board has no public name, so it cannot use a publicly trusted
// certificate. The authority lives in user data and is shared by every local
// service (Config Web, the Agent Web UI), so a user who trusts it once trusts
// them all. A user who does not trust it accepts each address's browser
// warning instead; certificates are stored per address and reused across
// restarts so that choice keeps applying.
//
// The authority is name-constrained to loopback, private and link-local
// addresses and to localhost, so trusting it cannot make a browser accept a
// certificate for a public site.
package localtls

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
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// DefaultDir is shared by the Agent and Config Web.
	DefaultDir = "/userdata/agent/tls"
	// AuthorityPath serves the public authority certificate.
	AuthorityPath = "/tls/ca.pem"

	caFileName    = "ca.pem"
	caKeyFileName = "ca-key.pem"
	lockFileName  = ".lock"
	leafDirName   = "certs"

	// macOS rejects TLS server certificates valid for more than 825 days even
	// under a user-trusted root, and browsers cap public certificates at 398
	// days; stay inside both.
	leafLifetime = 397 * 24 * time.Hour
	// Replace a stored certificate this long before it expires.
	leafRenewal = 30 * 24 * time.Hour
)

// The authority's validity is fixed rather than relative to the clock: the
// board may create it before its clock is synchronized.
var (
	caNotBefore = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	caNotAfter  = time.Date(2049, 12, 31, 0, 0, 0, 0, time.UTC)
)

// Authority issues server certificates for local addresses.
type Authority struct {
	dir    string
	cert   *x509.Certificate
	pem    []byte
	key    *ecdsa.PrivateKey
	now    func() time.Time
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// Load opens the authority in dir, creating it on first use. Several
// processes may call Load on the same directory concurrently.
func Load(dir string) (*Authority, error) {
	if err := os.MkdirAll(filepath.Join(dir, leafDirName), 0o700); err != nil {
		return nil, fmt.Errorf("create TLS directory: %w", err)
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	certPath := filepath.Join(dir, caFileName)
	keyPath := filepath.Join(dir, caKeyFileName)
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case errors.Is(certErr, os.ErrNotExist) || errors.Is(keyErr, os.ErrNotExist):
		// A missing half cannot be recovered; replace both.
		certPEM, keyPEM, err = createAuthority()
		if err != nil {
			return nil, err
		}
		if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
			return nil, fmt.Errorf("write TLS authority key: %w", err)
		}
		if err := writeFileAtomic(certPath, certPEM, 0o644); err != nil {
			return nil, fmt.Errorf("write TLS authority: %w", err)
		}
		// Certificates from a previous authority no longer verify.
		_ = clearDir(filepath.Join(dir, leafDirName))
	case certErr != nil:
		return nil, fmt.Errorf("read TLS authority: %w", certErr)
	case keyErr != nil:
		return nil, fmt.Errorf("read TLS authority key: %w", keyErr)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load TLS authority: %w", err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse TLS authority: %w", err)
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !cert.IsCA {
		return nil, errors.New("TLS authority must be an ECDSA certificate authority")
	}
	return &Authority{dir: dir, cert: cert, pem: certPEM, key: key, now: time.Now, leaves: make(map[string]*tls.Certificate)}, nil
}

func lockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, lockFileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open TLS lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock TLS directory: %w", err)
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		file.Close()
	}, nil
}

func createAuthority() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate TLS authority key: %w", err)
	}
	serial, err := serialNumber()
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
		NotBefore:                   caNotBefore,
		NotAfter:                    caNotAfter,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid:       true,
		IsCA:                        true,
		MaxPathLenZero:              true,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"localhost"},
		PermittedIPRanges:           permittedIPRanges(),
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

// permittedIPRanges are the addresses a board can be reached at: loopback,
// private IPv4, link-local, and IPv6 unique-local and link-local.
func permittedIPRanges() []*net.IPNet {
	var ranges []*net.IPNet
	for _, cidr := range []string{
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16",
		"::1/128", "fc00::/7", "fe80::/10",
	} {
		if _, network, err := net.ParseCIDR(cidr); err == nil {
			ranges = append(ranges, network)
		}
	}
	return ranges
}

func serialNumber() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}

// PEM returns the public authority certificate.
func (a *Authority) PEM() []byte { return append([]byte(nil), a.pem...) }

// Certificate returns the authority certificate.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// TLSConfig returns a server configuration that presents a certificate for
// the address each connection arrived on. It pins HTTP/1.1: the Agent's event
// stream and voice socket rely on HTTP/1.1 flushing and connection hijacking.
func (a *Authority) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: a.GetCertificate, NextProtos: []string{"http/1.1"}}
}

// GetCertificate implements tls.Config.GetCertificate. Clients that connect by
// IP address send no server name, so the certificate is chosen by the local
// address the connection arrived on.
func (a *Authority) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	ip := net.IPv4(127, 0, 0, 1)
	if hello != nil && hello.Conn != nil {
		if addr, ok := hello.Conn.LocalAddr().(*net.TCPAddr); ok && addr.IP != nil && !addr.IP.IsUnspecified() {
			ip = addr.IP
		}
	}
	return a.CertificateFor(ip)
}

// CertificateFor returns the certificate for ip, reusing the stored one while
// it remains valid so a browser's decision about it keeps applying.
func (a *Authority) CertificateFor(ip net.IP) (*tls.Certificate, error) {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	name := ip.String()
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if leaf := a.leaves[name]; usable(leaf, now) {
		return leaf, nil
	}
	path := filepath.Join(a.dir, leafDirName, strings.NewReplacer(":", "_", "%", "_").Replace(name)+".pem")
	// Another service may have stored a certificate for this address.
	unlock, err := lockDir(a.dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if leaf, err := a.readLeaf(path); err == nil && usable(leaf, now) && leaf.Leaf.VerifyHostname(name) == nil {
		a.leaves[name] = leaf
		return leaf, nil
	}
	leaf, keyPEM, err := a.issue(ip, now)
	if err != nil {
		return nil, err
	}
	data := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]}), keyPEM...)
	// A certificate that cannot be stored still serves this run.
	_ = writeFileAtomic(path, data, 0o600)
	a.leaves[name] = leaf
	return leaf, nil
}

func usable(leaf *tls.Certificate, now time.Time) bool {
	return leaf != nil && leaf.Leaf != nil && now.After(leaf.Leaf.NotBefore) && now.Add(leafRenewal).Before(leaf.Leaf.NotAfter)
}

func (a *Authority) readLeaf(path string) (*tls.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if err := leaf.CheckSignatureFrom(a.cert); err != nil {
		return nil, err
	}
	pair.Certificate = append(pair.Certificate, a.cert.Raw)
	pair.Leaf = leaf
	return &pair, nil
}

func (a *Authority) issue(ip net.IP, now time.Time) (*tls.Certificate, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate TLS key: %w", err)
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, nil, err
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
		NotAfter:    now.Add(leafLifetime),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, nil, fmt.Errorf("issue TLS certificate for %s: %w", ip, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &tls.Certificate{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key, Leaf: leaf}, keyPEM, nil
}

// ServeAuthority serves the public authority certificate so a user can trust
// it and open the board's HTTPS pages without a warning.
func (a *Authority) ServeAuthority(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="aico-link-ca.pem"`)
	_, _ = w.Write(a.pem)
}

// DisableHTTP2 keeps net/http from negotiating HTTP/2 on a server that serves
// TLS itself, matching TLSConfig.
func DisableHTTP2(server *http.Server) {
	server.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
	return nil
}

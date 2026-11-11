package agent

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"

	"aico-agent/internal/lanaccess"
)

// A client that trusts the board's local authority must accept the HTTPS
// listener when it connects by IP address, and the authority must survive a
// restart so a browser that trusted it keeps trusting the board.
func TestHTTPSCertificatesServeTrustedIPCertificate(t *testing.T) {
	dir := t.TempDir()
	certificates, err := loadHTTPSCertificates(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadHTTPSCertificates(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.caCert.Equal(certificates.caCert) {
		t.Fatal("restart replaced the TLS authority; trusted browsers would see a warning again")
	}

	var trusted bool
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Serve like startUSBListener does: trust marker under the TLS layer.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trusted = lanaccess.IsTrusted(r)
			w.WriteHeader(http.StatusNoContent)
		}),
		ConnContext: lanaccess.ConnContext,
	}
	go func() { _ = srv.Serve(tls.NewListener(lanaccess.TrustListener(listener), reloaded.tlsConfig())) }()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certificates.caPEM)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	// Connecting by IP sends no server name, as a browser at 192.168.42.1 does.
	resp, err := client.Get("https://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("HTTPS request with the board authority trusted: %v", err)
	}
	resp.Body.Close()
	if !trusted {
		t.Fatal("a request on a trusted (USB) listener lost its trust over TLS")
	}

	// The authority's name constraints keep it from vouching for public sites.
	public, err := certificates.issue(net.ParseIP("8.8.8.8"), certificates.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := public.Leaf.Verify(x509.VerifyOptions{Roots: pool}); err == nil {
		t.Fatal("a certificate for a public address verified under the board authority")
	}
}

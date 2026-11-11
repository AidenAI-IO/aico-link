package localtls

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"testing"

	"aico-agent/internal/lanaccess"
)

// A browser that trusts the board authority must accept a connection by IP
// address; a browser that accepted the warning instead must see the same
// certificate after a restart; and the authority must not vouch for public
// addresses.
func TestAuthorityServesStableTrustedIPCertificates(t *testing.T) {
	dir := t.TempDir()
	first, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	firstLeaf, err := first.CertificateFor(net.ParseIP("192.168.42.1"))
	if err != nil {
		t.Fatal(err)
	}

	restarted, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.Certificate().Equal(first.Certificate()) {
		t.Fatal("restart replaced the authority; trusted browsers would see a warning again")
	}
	again, err := restarted.CertificateFor(net.ParseIP("192.168.42.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Leaf.Equal(firstLeaf.Leaf) {
		t.Fatal("restart issued a new certificate; browsers that accepted the warning would be asked again")
	}

	var trusted bool
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Serve as the USB listeners do: trust marker under the TLS layer.
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trusted = lanaccess.IsTrusted(r)
			w.WriteHeader(http.StatusNoContent)
		}),
		ConnContext: lanaccess.ConnContext,
	}
	DisableHTTP2(srv)
	go func() { _ = srv.Serve(tls.NewListener(lanaccess.TrustListener(listener), restarted.TLSConfig())) }()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(first.PEM())
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

	public, _, err := restarted.issue(net.ParseIP("8.8.8.8"), restarted.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := public.Leaf.Verify(x509.VerifyOptions{Roots: pool}); err == nil {
		t.Fatal("a certificate for a public address verified under the board authority")
	}
}

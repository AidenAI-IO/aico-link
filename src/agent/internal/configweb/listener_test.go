package configweb

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"
)

// The wildcard portal must come up even when the USB interface does not
// exist yet; the USB maintenance socket keeps retrying in the background and
// stops cleanly on shutdown.
func TestListenAndServeStartsWithoutUSBInterface(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	options := testOptions(t)
	options.BindAddress = "0.0.0.0"
	options.Port = port
	options.USBInterface = "aico-no-such-if0"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	if server.usbHTTP == nil {
		t.Fatal("wildcard bind must create the USB maintenance server")
	}
	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wildcard listener never came up: %v", dialErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ListenAndServe = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after shutdown")
	}
	select {
	case <-server.usbRetryStop:
	default:
		t.Fatal("USB retry loop was not stopped on shutdown")
	}
}

func itoa(value int) string { return strconv.Itoa(value) }

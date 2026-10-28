package configweb

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestRetryListenRecoversAfterTransientFailures(t *testing.T) {
	attempts := 0
	var seen []int
	listener, err := retryListen(func() (net.Listener, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("no such device")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}, make(chan struct{}), func(int) time.Duration { return time.Millisecond }, func(attempt int, _ error) {
		seen = append(seen, attempt)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if attempts != 3 || len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("attempts=%d seen=%v", attempts, seen)
	}
}

func TestRetryListenStopsOnShutdown(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	_, err := retryListen(func() (net.Listener, error) {
		return nil, errors.New("no such device")
	}, stop, func(int) time.Duration { return time.Hour }, nil)
	if !errors.Is(err, errListenRetryStopped) {
		t.Fatalf("err = %v", err)
	}
}

func TestUSBListenRetryScheduleBacksOff(t *testing.T) {
	if usbListenRetrySchedule(1) != 2*time.Second || usbListenRetrySchedule(30) != 2*time.Second || usbListenRetrySchedule(31) != 15*time.Second {
		t.Fatal("unexpected retry schedule")
	}
}

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

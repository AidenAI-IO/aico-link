package configweb

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"aico-agent/internal/lanaccess"
)

type usbIngressContextKey struct{}

func configHTTPServer(address string, handler http.Handler, usb bool) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 65 * time.Second,
		WriteTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return lanaccess.WithTrusted(context.WithValue(ctx, usbIngressContextKey{}, usb), usb)
		},
	}
}

func listenConfigWeb(address, device string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, connection syscall.RawConn) error {
		var socketErr error
		if err := connection.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			if socketErr == nil && device != "" {
				socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, device)
			}
		}); err != nil {
			return err
		}
		return socketErr
	}}
	return config.Listen(context.Background(), "tcp4", address)
}

// usbListenRetrySchedule returns the delay before the next bind attempt:
// quick retries while the USB gadget is still being configured at boot, then
// a slow poll so a late or re-plugged interface is still picked up.
func usbListenRetrySchedule(attempt int) time.Duration {
	if attempt <= 30 {
		return 2 * time.Second
	}
	return 15 * time.Second
}

var errListenRetryStopped = errors.New("listener retry stopped")

// retryListen calls listen until it succeeds or stop is closed.  schedule maps
// the failed attempt count to the delay before the next try; onError is
// invoked after every failed attempt with that count.
func retryListen(listen func() (net.Listener, error), stop <-chan struct{}, schedule func(int) time.Duration, onError func(int, error)) (net.Listener, error) {
	for attempt := 1; ; attempt++ {
		listener, err := listen()
		if err == nil {
			return listener, nil
		}
		if onError != nil {
			onError(attempt, err)
		}
		timer := time.NewTimer(schedule(attempt))
		select {
		case <-stop:
			timer.Stop()
			return nil, errListenRetryStopped
		case <-timer.C:
		}
	}
}

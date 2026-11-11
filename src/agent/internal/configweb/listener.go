package configweb

import (
	"context"
	"net"
	"net/http"
	"time"

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

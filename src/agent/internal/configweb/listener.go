package configweb

import (
	"context"
	"net"
	"net/http"
	"time"

	"aico-agent/internal/lanaccess"
	"aico-agent/internal/localtls"
)

type usbIngressContextKey struct{}

func configHTTPServer(address string, handler http.Handler, usb bool) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 65 * time.Second,
		WriteTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second,
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			return lanaccess.WithUSB(context.WithValue(ctx, usbIngressContextKey{}, usb), usb)
		},
	}
}

// configHTTPSServer serves the portal over TLS with certificates from the
// board's local authority, with the same USB ingress marking as HTTP.
func configHTTPSServer(address string, handler http.Handler, usb bool, authority *localtls.Authority) *http.Server {
	server := configHTTPServer(address, handler, usb)
	authority.Configure(server)
	return server
}

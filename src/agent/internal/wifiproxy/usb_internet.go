package wifiproxy

import (
	"context"
	"net"
	"net/http"
	"net/url"

	"aico-agent/internal/netproxy"
	"aico-agent/internal/usbuplink"
)

const usbNoProxy = "localhost,127.0.0.1,::1,192.168.42.0/24"

type proxyRoute struct {
	upstream *url.URL
	lease    *usbuplink.Lease
}

func (s *Server) routeForHost(host, port, scheme string) (proxyRoute, error) {
	if s.usbInternet != nil {
		if lease := s.usbInternet.Current(); lease != nil {
			if netproxy.IsLoopbackHost(host) || netproxy.Bypass(host, port, usbNoProxy) {
				return proxyRoute{}, nil
			}
			return proxyRoute{upstream: lease.Upstream, lease: lease}, nil
		}
	}
	upstream, err := s.legacyUpstreamForHost(host, port, scheme)
	return proxyRoute{upstream: upstream}, err
}

func (r proxyRoute) dial(ctx context.Context, target string) (net.Conn, error) {
	connect := func(ctx context.Context) (net.Conn, error) { return dialTarget(ctx, target, r.upstream) }
	if r.lease != nil {
		return r.lease.Dial(ctx, connect)
	}
	return connect(ctx)
}

func (s *Server) transportForRoute(route proxyRoute) (*http.Transport, error) {
	if route.lease == nil {
		return s.transportFor(route.upstream)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usbTransportLease == route.lease {
		return s.usbTransport, nil
	}
	if s.usbTransport != nil {
		s.usbTransport.CloseIdleConnections()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 8
	transport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) { return route.dial(ctx, address) }
	s.usbTransportLease, s.usbTransport = route.lease, transport
	return transport, nil
}

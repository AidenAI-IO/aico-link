package wifiproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/logging"
	"aiden-agent/internal/netproxy"
	xproxy "golang.org/x/net/proxy"
)

const defaultPollInterval = 2 * time.Second

type Upstreams struct {
	HTTPProxy  string
	HTTPSProxy string
	AllProxy   string
	NoProxy    string
	NoProxySet bool
}

func UpstreamsFromEnvironment() (Upstreams, error) {
	httpProxy := firstNonEmptyEnvironment("HTTP_PROXY", "http_proxy")
	httpsProxy := firstNonEmptyEnvironment("HTTPS_PROXY", "https_proxy")
	allProxy := firstNonEmptyEnvironment("ALL_PROXY", "all_proxy")
	noProxy, noProxySet := firstEnvironment("NO_PROXY", "no_proxy")
	if !noProxySet {
		noProxy = DefaultNoProxy
	}
	normalizedNoProxy, err := NormalizeNoProxy(noProxy)
	if err != nil {
		return Upstreams{}, fmt.Errorf("NO_PROXY: %w", err)
	}
	return Upstreams{
		HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, AllProxy: allProxy,
		NoProxy: normalizedNoProxy, NoProxySet: noProxySet,
	}, nil
}

func firstNonEmptyEnvironment(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

func firstEnvironment(names ...string) (string, bool) {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			return value, true
		}
	}
	return "", false
}

func (u Upstreams) Validate() error {
	if _, err := NormalizeNoProxy(u.NoProxy); err != nil {
		return fmt.Errorf("NO_PROXY: %w", err)
	}
	for name, raw := range map[string]string{
		"HTTP_PROXY": u.HTTPProxy, "HTTPS_PROXY": u.HTTPSProxy, "ALL_PROXY": u.AllProxy,
	} {
		if raw == "" {
			continue
		}
		if _, err := parseUpstream(raw); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

type Server struct {
	listenAddress   string
	configPath      string
	environmentPath string
	wifiInterface   string
	pollInterval    time.Duration
	fallback        Upstreams

	mu          sync.RWMutex
	config      Config
	currentSSID string
	transports  map[string]*http.Transport

	httpServer *http.Server
	cancel     context.CancelFunc
}

func NewServer(listenAddress, configPath, wifiInterface string, fallback Upstreams) (*Server, error) {
	if strings.TrimSpace(listenAddress) == "" {
		listenAddress = DefaultListenAddress
	}
	if err := validateListenAddress(listenAddress); err != nil {
		return nil, err
	}
	if strings.TrimSpace(configPath) == "" {
		configPath = DefaultConfigPath
	}
	if strings.TrimSpace(wifiInterface) == "" {
		wifiInterface = "wlan0"
	}
	if err := fallback.Validate(); err != nil {
		return nil, fmt.Errorf("fallback proxy: %w", err)
	}
	fallback.NoProxy, _ = NormalizeNoProxy(fallback.NoProxy)
	config, err := Load(configPath)
	if err != nil {
		return nil, err
	}
	server := &Server{
		listenAddress:   listenAddress,
		configPath:      configPath,
		environmentPath: DefaultEnvironmentPath,
		wifiInterface:   wifiInterface,
		pollInterval:    defaultPollInterval,
		fallback:        fallback,
		config:          config,
		transports:      make(map[string]*http.Transport),
	}
	server.httpServer = &http.Server{
		Addr:              listenAddress,
		Handler:           server,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	return server, nil
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid Wi-Fi proxy listen address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid Wi-Fi proxy listen port %q", portText)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("Wi-Fi proxy must listen on a loopback address")
	}
	return nil
}

func (s *Server) ListenAndServe() error {
	listener, err := net.Listen("tcp", s.listenAddress)
	if err != nil {
		return err
	}
	return s.Serve(listener)
}

func (s *Server) Serve(listener net.Listener) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	_ = s.Refresh(ctx)
	go s.monitor(ctx)
	logging.Infof("wifi_proxy", "wifi_proxy", "listening on %s", listener.Addr())
	protocol := newProtocolListener(listener, s)
	defer protocol.Close()
	err := s.httpServer.Serve(protocol)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	for _, transport := range s.transports {
		transport.CloseIdleConnections()
	}
	s.mu.Unlock()
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) monitor(ctx context.Context) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Refresh(ctx); err != nil {
				logging.Warnf("wifi_proxy", "wifi_proxy", "refresh failed: %v", err)
			}
		}
	}
}

func (s *Server) Refresh(ctx context.Context) error {
	config, configErr := Load(s.configPath)
	ssid, ssidErr := currentSSID(ctx, s.wifiInterface)
	s.mu.Lock()
	if configErr == nil {
		s.config = config
	}
	// Keep the last completed SSID while Wi-Fi is briefly reassociating. This
	// avoids switching the local proxy protocol to the system fallback and back
	// again during a normal roam or configuration update.
	if ssidErr == nil && ssid != "" {
		s.currentSSID = ssid
	}
	s.mu.Unlock()
	if err := s.writeLocalProxyEnvironment(); err != nil {
		return err
	}
	if configErr != nil {
		return configErr
	}
	return ssidErr
}

// SetEnvironmentPath overrides the generated local-proxy environment path.
// It is primarily useful for host tests; production uses DefaultEnvironmentPath.
func (s *Server) SetEnvironmentPath(path string) {
	s.mu.Lock()
	s.environmentPath = strings.TrimSpace(path)
	s.mu.Unlock()
}

// protocolListener classifies connections concurrently. A serial classifier
// in Accept would let an idle client block the HTTP server from accepting all
// other clients while it waited for the first byte.
type protocolListener struct {
	net.Listener
	server   *Server
	httpConn chan net.Conn
	done     chan struct{}
	doneOnce sync.Once
}

func newProtocolListener(listener net.Listener, server *Server) *protocolListener {
	classified := &protocolListener{
		Listener: listener,
		server:   server,
		httpConn: make(chan net.Conn),
		done:     make(chan struct{}),
	}
	go classified.acceptLoop()
	return classified
}

func (l *protocolListener) acceptLoop() {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			l.doneOnce.Do(func() { close(l.done) })
			return
		}
		go l.classify(connection)
	}
}

func (l *protocolListener) classify(connection net.Conn) {
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(connection)
	prefix, err := reader.Peek(1)
	_ = connection.SetReadDeadline(time.Time{})
	if err != nil {
		connection.Close()
		return
	}
	buffered := &bufferedConn{Conn: connection, reader: reader}
	if prefix[0] == 0x05 {
		go l.server.handleSOCKS5(buffered)
		return
	}
	select {
	case l.httpConn <- buffered:
	case <-l.done:
		buffered.Close()
	}
}

func (l *protocolListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.httpConn:
		return connection, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *protocolListener) Close() error {
	l.doneOnce.Do(func() { close(l.done) })
	return l.Listener.Close()
}

func (s *Server) handleSOCKS5(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(15 * time.Second))

	var greeting [2]byte
	if _, err := io.ReadFull(client, greeting[:]); err != nil || greeting[0] != 0x05 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	noAuthentication := false
	for _, method := range methods {
		if method == 0x00 {
			noAuthentication = true
			break
		}
	}
	if !noAuthentication {
		_, _ = client.Write([]byte{0x05, 0xff})
		return
	}
	if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
		return
	}

	var requestHeader [4]byte
	if _, err := io.ReadFull(client, requestHeader[:]); err != nil || requestHeader[0] != 0x05 {
		return
	}
	if requestHeader[1] != 0x01 {
		writeSOCKS5Reply(client, 0x07)
		return
	}
	targetHost, err := readSOCKS5Host(client, requestHeader[3])
	if err != nil {
		writeSOCKS5Reply(client, 0x08)
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(client, portBytes[:]); err != nil {
		return
	}
	targetPort := int(binary.BigEndian.Uint16(portBytes[:]))
	targetAddress := net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort))
	// SOCKS5 does not carry an application-layer scheme. Use the generic
	// fallback order (ALL_PROXY, then HTTPS_PROXY, then HTTP_PROXY) instead of
	// guessing from the destination port.
	upstream, err := s.genericFallbackUpstream(targetAddress)
	if err != nil {
		writeSOCKS5Reply(client, 0x01)
		return
	}
	remote, err := dialTarget(context.Background(), targetAddress, upstream)
	if err != nil {
		writeSOCKS5Reply(client, 0x05)
		return
	}
	if err := writeSOCKS5Reply(client, 0x00); err != nil {
		remote.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	tunnel(client, remote)
}

func readSOCKS5Host(reader io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 0x01:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	case 0x03:
		var size [1]byte
		if _, err := io.ReadFull(reader, size[:]); err != nil || size[0] == 0 {
			return "", errors.New("invalid SOCKS5 domain")
		}
		address := make([]byte, int(size[0]))
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return string(address), nil
	case 0x04:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	default:
		return "", errors.New("unsupported SOCKS5 address type")
	}
}

func writeSOCKS5Reply(writer io.Writer, status byte) error {
	_, err := writer.Write([]byte{0x05, status, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	return err
}

func (s *Server) writeLocalProxyEnvironment() error {
	s.mu.RLock()
	path := s.environmentPath
	address := s.listenAddress
	ssid := s.currentSSID
	network, configured := s.config.Networks[ssid]
	fallback := s.fallback
	s.mu.RUnlock()
	if path == "" {
		return nil
	}

	httpScheme, httpsScheme, allScheme := fallbackLocalSchemes(fallback)
	noProxy := fallback.NoProxy
	noProxySet := fallback.NoProxySet
	if configured {
		switch network.Mode {
		case ModeDirect:
			httpScheme, httpsScheme, allScheme = "http", "http", "http"
		case ModeProxy:
			scheme := localSchemeForUpstream(network.ProxyURL)
			httpScheme, httpsScheme, allScheme = scheme, scheme, scheme
			noProxy = network.NoProxy
			noProxySet = network.NoProxySet
		}
	}
	normalizedNoProxy, err := NormalizeNoProxy(noProxy)
	if err != nil {
		return fmt.Errorf("NO_PROXY: %w", err)
	}
	noProxyFlag := "0"
	if noProxySet {
		noProxyFlag = "1"
	}
	contents := []byte(fmt.Sprintf(
		"HTTP_PROXY=%s://%s\nHTTPS_PROXY=%s://%s\nALL_PROXY=%s://%s\nhttp_proxy=%s://%s\nhttps_proxy=%s://%s\nall_proxy=%s://%s\nNO_PROXY=%s\nno_proxy=%s\nAIDEN_WIFI_PROXY_NO_PROXY_SET=%s\n",
		httpScheme, address, httpsScheme, address, allScheme, address,
		httpScheme, address, httpsScheme, address, allScheme, address,
		shellQuote(normalizedNoProxy), shellQuote(normalizedNoProxy), noProxyFlag,
	))
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, contents) {
		// Repair files left by versions that restricted login shells to root.
		// Avoid chmod on every refresh: metadata changes can wake the Agent's
		// systemd path watcher even when the proxy route did not change.
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o644 {
			return os.Chmod(path, 0o644)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create local proxy environment directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".proxy-env.tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	// This public file contains only the local relay address and NO_PROXY.
	// Upstream credentials remain in the separate, private Wi-Fi config.
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func shellQuote(value string) string {
	return "'" + value + "'"
}

func fallbackLocalSchemes(fallback Upstreams) (string, string, string) {
	httpRaw := fallback.HTTPProxy
	if httpRaw == "" {
		httpRaw = fallback.AllProxy
	}
	httpsRaw := fallback.HTTPSProxy
	if httpsRaw == "" {
		httpsRaw = fallback.AllProxy
	}
	allRaw := fallback.AllProxy
	if allRaw == "" {
		allRaw = fallback.HTTPSProxy
	}
	if allRaw == "" {
		allRaw = fallback.HTTPProxy
	}
	return localSchemeForUpstream(httpRaw), localSchemeForUpstream(httpsRaw), localSchemeForUpstream(allRaw)
}

func localSchemeForUpstream(raw string) string {
	parsed, err := parseUpstream(raw)
	if err == nil && (strings.EqualFold(parsed.Scheme, "socks5") || strings.EqualFold(parsed.Scheme, "socks5h")) {
		// socks5h is the same SOCKS5 wire protocol; the h tells clients such
		// as curl to send the hostname through the tunnel instead of relying
		// on the board's local DNS resolver.
		return "socks5h"
	}
	return "http"
}

func currentSSID(ctx context.Context, wifiInterface string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandCtx, "wpa_cli", "-i", wifiInterface, "status").Output()
	if err != nil {
		return "", err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	if values["wpa_state"] != "COMPLETED" {
		return "", nil
	}
	return values["ssid"], nil
}

func (s *Server) SetCurrentSSID(ssid string) {
	s.mu.Lock()
	s.currentSSID = ssid
	s.mu.Unlock()
}

func (s *Server) upstreamFor(target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, nil
	}
	return s.upstreamForHost(target.Hostname(), target.Port(), target.Scheme)
}

// genericFallbackUpstream picks the upstream for a SOCKS5 client, which names
// only a host and port.
func (s *Server) genericFallbackUpstream(targetAddress string) (*url.URL, error) {
	host, port := splitTargetAddress(targetAddress)
	return s.upstreamForHost(host, port, "socks5")
}

// upstreamForHost returns the proxy for host on the current Wi-Fi network, or
// nil to connect directly. scheme selects the fallback proxy variable when the
// network has no saved setting.
func (s *Server) upstreamForHost(host, port, scheme string) (*url.URL, error) {
	if netproxy.IsLoopbackHost(host) {
		return nil, nil
	}
	s.mu.RLock()
	ssid := s.currentSSID
	network, configured := s.config.Networks[ssid]
	fallback := s.fallback
	s.mu.RUnlock()

	if configured {
		switch network.Mode {
		case ModeDirect:
			return nil, nil
		case ModeProxy:
			if netproxy.Bypass(host, port, network.NoProxy) {
				return nil, nil
			}
			return parseUpstream(network.ProxyURL)
		}
	}
	if netproxy.Bypass(host, port, fallback.NoProxy) {
		return nil, nil
	}
	raw := fallbackProxyForScheme(fallback, scheme)
	if raw == "" {
		return nil, nil
	}
	return parseUpstream(raw)
}

func fallbackProxyForScheme(fallback Upstreams, scheme string) string {
	raw := fallback.AllProxy
	switch strings.ToLower(scheme) {
	case "http", "ws":
		if fallback.HTTPProxy != "" {
			raw = fallback.HTTPProxy
		}
	case "https", "wss":
		if fallback.HTTPSProxy != "" {
			raw = fallback.HTTPSProxy
		}
	default:
		if raw == "" {
			raw = fallback.HTTPSProxy
		}
		if raw == "" {
			raw = fallback.HTTPProxy
		}
	}
	return raw
}

func splitTargetAddress(address string) (string, string) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return strings.Trim(address, "[]"), ""
	}
	return host, port
}

func (s *Server) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodConnect {
		s.handleConnect(w, request)
		return
	}
	s.handleHTTP(w, request)
}

func (s *Server) handleHTTP(w http.ResponseWriter, request *http.Request) {
	target := request.URL
	if target == nil || target.Host == "" {
		http.Error(w, "absolute target URL required", http.StatusBadRequest)
		return
	}
	outgoing := request.Clone(request.Context())
	outgoing.RequestURI = ""
	outgoing.Header = request.Header.Clone()
	if outgoing.URL.Scheme == "ws" {
		clonedURL := *outgoing.URL
		clonedURL.Scheme = "http"
		outgoing.URL = &clonedURL
	}
	upgrade := strings.EqualFold(request.Header.Get("Connection"), "upgrade") || request.Header.Get("Upgrade") != ""
	if !upgrade {
		removeHopHeaders(outgoing.Header)
	}
	upstream, err := s.upstreamFor(target)
	if err != nil {
		http.Error(w, "invalid upstream proxy", http.StatusBadGateway)
		return
	}
	transport, err := s.transportFor(upstream)
	if err != nil {
		http.Error(w, "configure upstream proxy", http.StatusBadGateway)
		return
	}
	response, err := transport.RoundTrip(outgoing)
	if err != nil {
		http.Error(w, "proxy request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		s.handleUpgrade(w, response)
		return
	}
	defer response.Body.Close()
	removeHopHeaders(response.Header)
	copyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (s *Server) handleConnect(w http.ResponseWriter, request *http.Request) {
	targetAddress, err := canonicalTargetAddress(request.Host, "443")
	if err != nil {
		http.Error(w, "invalid CONNECT target", http.StatusBadRequest)
		return
	}
	targetURL := &url.URL{Scheme: "https", Host: targetAddress}
	upstream, err := s.upstreamFor(targetURL)
	if err != nil {
		http.Error(w, "invalid upstream proxy", http.StatusBadGateway)
		return
	}
	remote, err := dialTarget(request.Context(), targetAddress, upstream)
	if err != nil {
		http.Error(w, "proxy connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		remote.Close()
		http.Error(w, "connection hijacking unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		remote.Close()
		return
	}
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		client.Close()
		remote.Close()
		return
	}
	if err := buffered.Flush(); err != nil {
		client.Close()
		remote.Close()
		return
	}
	tunnel(&bufferedConn{Conn: client, reader: buffered.Reader}, remote)
}

func (s *Server) handleUpgrade(w http.ResponseWriter, response *http.Response) {
	upstream, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		response.Body.Close()
		http.Error(w, "upstream upgrade unavailable", http.StatusBadGateway)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "connection hijacking unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	fmt.Fprintf(buffered, "HTTP/1.1 %s\r\n", response.Status)
	_ = response.Header.Write(buffered)
	_, _ = buffered.WriteString("\r\n")
	if err := buffered.Flush(); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	tunnelReadWriter(&bufferedConn{Conn: client, reader: buffered.Reader}, upstream)
}

func (s *Server) transportFor(upstream *url.URL) (*http.Transport, error) {
	key := "direct"
	if upstream != nil {
		key = upstream.String()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if transport := s.transports[key]; transport != nil {
		return transport, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 8
	if upstream != nil {
		switch strings.ToLower(upstream.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(upstream)
		case "socks5", "socks5h":
			dialer, err := xproxy.FromURL(upstream, &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second})
			if err != nil {
				return nil, err
			}
			transport.DialContext = contextDialer(dialer)
		default:
			return nil, fmt.Errorf("unsupported upstream proxy scheme %q", upstream.Scheme)
		}
	}
	s.transports[key] = transport
	return transport, nil
}

func contextDialer(dialer xproxy.Dialer) func(context.Context, string, string) (net.Conn, error) {
	if contextAware, ok := dialer.(xproxy.ContextDialer); ok {
		return contextAware.DialContext
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		resultCh := make(chan result, 1)
		go func() {
			conn, err := dialer.Dial(network, address)
			resultCh <- result{conn: conn, err: err}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case value := <-resultCh:
			return value.conn, value.err
		}
	}
}

func dialTarget(ctx context.Context, targetAddress string, upstream *url.URL) (net.Conn, error) {
	if upstream == nil {
		return (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", targetAddress)
	}
	switch strings.ToLower(upstream.Scheme) {
	case "socks5", "socks5h":
		dialer, err := xproxy.FromURL(upstream, &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second})
		if err != nil {
			return nil, err
		}
		return contextDialer(dialer)(ctx, "tcp", targetAddress)
	case "http", "https":
		return dialHTTPProxy(ctx, targetAddress, upstream)
	default:
		return nil, fmt.Errorf("unsupported upstream proxy scheme %q", upstream.Scheme)
	}
}

func dialHTTPProxy(ctx context.Context, targetAddress string, upstream *url.URL) (net.Conn, error) {
	proxyAddress, err := canonicalTargetAddress(upstream.Host, map[bool]string{true: "443", false: "80"}[strings.EqualFold(upstream.Scheme, "https")])
	if err != nil {
		return nil, err
	}
	var connection net.Conn
	if strings.EqualFold(upstream.Scheme, "https") {
		dialer := &tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second},
			Config:    &tls.Config{MinVersion: tls.VersionTLS12, ServerName: upstream.Hostname()},
		}
		connection, err = dialer.DialContext(ctx, "tcp", proxyAddress)
	} else {
		connection, err = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", proxyAddress)
	}
	if err != nil {
		return nil, err
	}
	connectRequest := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{},
		Host:   targetAddress,
		Header: make(http.Header),
	}
	if upstream.User != nil {
		password, _ := upstream.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(upstream.User.Username() + ":" + password))
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := connectRequest.Write(connection); err != nil {
		connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, connectRequest)
	if err != nil {
		connection.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		connection.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT returned %s", response.Status)
	}
	return &bufferedConn{Conn: connection, reader: reader}, nil
}

func canonicalTargetAddress(address, defaultPort string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("target address is empty")
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		if host == "" || port == "" {
			return "", errors.New("target host or port is empty")
		}
		return net.JoinHostPort(host, port), nil
	}
	host := strings.Trim(address, "[]")
	if host == "" || strings.Contains(host, "/") {
		return "", errors.New("invalid target host")
	}
	return net.JoinHostPort(host, defaultPort), nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}

func tunnel(left, right net.Conn) {
	tunnelReadWriter(left, right)
}

func tunnelReadWriter(left, right io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(right, left)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(left, right)
		done <- struct{}{}
	}()
	<-done
	left.Close()
	right.Close()
	<-done
}

func removeHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

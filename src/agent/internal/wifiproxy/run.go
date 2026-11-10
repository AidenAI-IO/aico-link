package wifiproxy

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aico-agent/internal/logging"
	"aico-agent/internal/usbuplink"
)

func Run(args []string) int {
	flags := flag.NewFlagSet("wifi-proxy", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	listenAddress := flags.String("listen", DefaultListenAddress, "loopback HTTP/SOCKS5 proxy listen address")
	configPath := flags.String("config", DefaultConfigPath, "per-Wi-Fi proxy configuration path")
	environmentPath := flags.String("environment", DefaultEnvironmentPath, "generated local proxy environment path")
	wifiInterface := flags.String("wifi-interface", "wlan0", "Wi-Fi interface used to resolve the active SSID")
	usbSettings := flags.String("usb-internet-settings", usbuplink.DefaultSettingsPath, "USB Internet switch file")
	usbSocket := flags.String("usb-internet-socket", usbuplink.DefaultSocketPath, "local USB Internet control socket")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", flags.Args())
		return 1
	}
	upstreams, err := UpstreamsFromEnvironment()
	if err != nil {
		logging.Errorf("wifi_proxy", "run", "wifi-proxy: invalid environment: %v", err)
		return 1
	}
	server, err := NewServer(*listenAddress, *configPath, *wifiInterface, upstreams)
	if err != nil {
		logging.Errorf("wifi_proxy", "run", "wifi-proxy: %v", err)
		return 1
	}
	server.SetEnvironmentPath(*environmentPath)
	stopUSB := server.startUSBInternet(*usbSettings, *usbSocket)
	defer stopUSB()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-errCh:
		if err != nil {
			logging.Errorf("wifi_proxy", "run", "wifi-proxy: %v", err)
			return 1
		}
		return 0
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logging.Errorf("wifi_proxy", "run", "wifi-proxy shutdown: %v", err)
			return 1
		}
		return 0
	}
}

// startUSBInternet isolates optional USB setup and serving failures from the
// core proxy. Call it before serving requests, and release its resources at exit.
func (s *Server) startUSBInternet(settingsPath, socketPath string) func() {
	manager, err := usbuplink.New(settingsPath)
	if err != nil {
		logging.Errorf("wifi_proxy", "run", "USB Internet disabled: settings: %v", err)
		return func() {}
	}
	listener, err := usbuplink.Listen(socketPath)
	if err != nil {
		manager.Close()
		logging.Errorf("wifi_proxy", "run", "USB Internet disabled: control socket: %v", err)
		return func() {}
	}
	s.usbInternet = manager
	ctx, cancel := context.WithCancel(context.Background())
	control := &http.Server{Handler: manager, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				manager.Current()
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer cancel()
		defer manager.Close()
		if err := control.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logging.Errorf("wifi_proxy", "run", "USB Internet control stopped: %v", err)
		}
	}()
	return func() {
		cancel()
		_ = control.Close()
		_ = listener.Close()
		manager.Close()
	}
}

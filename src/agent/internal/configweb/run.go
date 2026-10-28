package configweb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"aico-agent/internal/logging"
)

// Run serves the device configuration portal until SIGINT or SIGTERM.
func Run(args []string) int {
	options := DefaultOptions()
	fs := flag.NewFlagSet("config-web", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&options.BindAddress, "bind", options.BindAddress, "HTTP bind address")
	fs.IntVar(&options.Port, "port", options.Port, "HTTP port")
	fs.StringVar(&options.AgentConfigPath, "config", options.AgentConfigPath, "agent TOML path")
	fs.StringVar(&options.WiFiConfigPath, "wifi-config", options.WiFiConfigPath, "wpa_supplicant config path")
	fs.StringVar(&options.WiFiConfigEnvironmentPath, "wifi-config-environment", options.WiFiConfigEnvironmentPath, "runtime wpa_supplicant config environment path")
	fs.StringVar(&options.WiFiRegionStatePath, "wifi-region-state", options.WiFiRegionStatePath, "Wi-Fi regulatory domain provenance sidecar path")
	fs.BoolVar(&options.WiFiRegionAutoApplyBeacon, "wifi-region-auto-apply-beacon", options.WiFiRegionAutoApplyBeacon, "apply a country agreed by at least three distinct access points without asking")
	fs.StringVar(&options.WiFiInterface, "wifi-interface", options.WiFiInterface, "Wi-Fi interface")
	fs.StringVar(&options.WiFiBackend, "wifi-backend", options.WiFiBackend, "Wi-Fi backend (legacy or systemd-networkd)")
	fs.StringVar(&options.OTAStatePath, "ota-state", options.OTAStatePath, "OTA state JSON path")
	fs.StringVar(&options.CmdlinePath, "cmdline", options.CmdlinePath, "kernel command line path")
	fs.StringVar(&options.SystemEnvPath, "system-env", options.SystemEnvPath, "system environment file path")
	fs.StringVar(&options.WiFiProxyConfigPath, "wifi-proxy-config", options.WiFiProxyConfigPath, "per-Wi-Fi proxy configuration path")
	fs.StringVar(&options.LocalProxyAddress, "local-proxy-address", options.LocalProxyAddress, "fixed loopback proxy address used by managed commands")
	fs.StringVar(&options.LocalProxyEnvironmentPath, "local-proxy-environment", options.LocalProxyEnvironmentPath, "generated local proxy environment path")
	fs.StringVar(&options.StorageStatePath, "storage-state", options.StorageStatePath, "storage state path")
	fs.StringVar(&options.WebRoot, "web-root", options.WebRoot, "config web static asset root")
	fs.StringVar(&options.BackupUserdataRoot, "backup-userdata-root", options.BackupUserdataRoot, "userdata root used by backup and restore")
	fs.StringVar(&options.BackupSDRoot, "backup-sd-root", options.BackupSDRoot, "SD-card root used by backup and restore")
	fs.StringVar(&options.MaintenanceLockPath, "maintenance-lock", options.MaintenanceLockPath, "backup and restore maintenance lock path")
	fs.StringVar(&options.BackupJobStateDir, "backup-job-state-dir", options.BackupJobStateDir, "runtime backup job state directory")
	fs.StringVar(&options.USBAddress, "usb-address", options.USBAddress, "device address on the USB ECM link")
	fs.StringVar(&options.USBSubnet, "usb-subnet", options.USBSubnet, "USB ECM client subnet")
	fs.StringVar(&options.USBInterface, "usb-interface", options.USBInterface, "USB ECM ingress interface for maintenance")
	fs.StringVar(&options.HardwareIDPath, "hardware-id-path", options.HardwareIDPath, "immutable hardware identifier path")
	fs.StringVar(&options.OTAConfigPath, "ota-config", options.OTAConfigPath, "OTA configuration path for factory version information and identity provisioning after restore")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected arguments: %v\n", fs.Args())
		return 1
	}

	server, err := NewServer(options)
	if err != nil {
		logging.Errorf("config_web", "run", "config-web: %v", err)
		return 1
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-errCh:
		if err != nil {
			logging.Errorf("config_web", "run", "config-web: %v", err)
			return 1
		}
		return 0
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logging.Errorf("config_web", "run", "config-web shutdown: %v", err)
			return 1
		}
		return 0
	}
}

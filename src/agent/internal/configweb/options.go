package configweb

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"aiden-agent/internal/netproxy"
	"aiden-agent/internal/wifiproxy"
)

const (
	defaultAgentConfigPath           = "/userdata/agent/agent.toml"
	defaultWiFiConfigPath            = "/userdata/wpa_supplicant.conf"
	defaultWiFiConfigEnvironmentPath = "/run/aiden/wpa_supplicant-config.env"
	defaultWiFiRegionStatePath       = "/userdata/system/wifi-region.json"
	defaultSystemEnvPath             = "/userdata/system/env"
	defaultWebRoot                   = "/usr/share/aiden/config-web"
	defaultMaintenanceLockPath       = "/run/aiden/backup.lock"
	defaultBackupJobStateDir         = "/run/aiden/backup/jobs"
	defaultUSBAddress                = "192.168.42.1"
	defaultUSBSubnet                 = "192.168.42.0/24"
)

// Options contains the filesystem and process integration points used by the
// configuration portal. Paths are flags so host tests can run without device
// files, while production keeps the established locations.
type Options struct {
	BindAddress               string
	Port                      int
	AgentConfigPath           string
	WiFiConfigPath            string
	WiFiConfigEnvironmentPath string
	WiFiInterface             string
	WiFiBackend               string
	WiFiRegionStatePath       string
	WiFiRegionAutoApplyBeacon bool
	OTAStatePath              string
	CmdlinePath               string
	SystemEnvPath             string
	WiFiProxyConfigPath       string
	LocalProxyAddress         string
	LocalProxyEnvironmentPath string
	StorageStatePath          string
	WebRoot                   string
	BackupUserdataRoot        string
	BackupSDRoot              string
	MaintenanceLockPath       string
	BackupJobStateDir         string
	USBAddress                string
	USBSubnet                 string
	USBInterface              string
	HardwareIDPath            string
	SystemctlBinary           string
	RootHomeMountPoint        string
	RootHomeScript            string
	BluetoothStateMountPoint  string
	BluetoothStateScript      string
	OTAConfigPath             string

	AgentBinary            string
	AgentInitScript        string
	FrameServiceInitScript string
	WiFiProxyInitScript    string
	AgentHTTPBaseURL       string
	OTABinary              string
	HybridUpdateService    string
	HybridUpdateMarkerPath string
	BootIDPath             string
	EnvRunBinary           string
	OTAUpdateLockPath      string
	OTAUpdateLogPath       string
	OTAHealthLogPath       string
	DmesgBinary            string
}

func DefaultOptions() Options {
	agentBinary := strings.TrimSpace(os.Getenv("AIDEN_AGENT_BIN"))
	if agentBinary == "" {
		if executable, err := os.Executable(); err == nil {
			agentBinary = executable
		} else {
			agentBinary = "/usr/lib/aiden/agent"
		}
	}
	return Options{
		BindAddress:               "0.0.0.0",
		Port:                      80,
		AgentConfigPath:           defaultAgentConfigPath,
		WiFiRegionStatePath:       defaultWiFiRegionStatePath,
		WiFiConfigPath:            defaultWiFiConfigPath,
		WiFiConfigEnvironmentPath: defaultWiFiConfigEnvironmentPath,
		WiFiInterface:             "wlan0",
		WiFiBackend:               "legacy",
		OTAStatePath:              "/userdata/ota/state.json",
		CmdlinePath:               "/proc/cmdline",
		SystemEnvPath:             defaultSystemEnvPath,
		WiFiProxyConfigPath:       wifiproxy.DefaultConfigPath,
		LocalProxyAddress:         wifiproxy.DefaultListenAddress,
		LocalProxyEnvironmentPath: envOrDefault("AIDEN_WIFI_PROXY_ENVIRONMENT", wifiproxy.DefaultEnvironmentPath),
		StorageStatePath:          "/run/aiden/storage.state",
		WebRoot:                   defaultWebRoot,
		BackupUserdataRoot:        "/userdata",
		BackupSDRoot:              "/mnt/sdcard",
		MaintenanceLockPath:       defaultMaintenanceLockPath,
		BackupJobStateDir:         defaultBackupJobStateDir,
		USBAddress:                defaultUSBAddress,
		USBSubnet:                 defaultUSBSubnet,
		USBInterface:              "usb0",
		HardwareIDPath:            envOrDefault("AIDEN_HARDWARE_ID_PATH", "/sys/firmware/devicetree/base/serial-number"),
		SystemctlBinary:           envOrDefault("AIDEN_SYSTEMCTL_BIN", "systemctl"),
		RootHomeMountPoint:        "/root",
		RootHomeScript:            envOrDefault("AIDEN_ROOT_HOME_SCRIPT", "/usr/lib/aiden/aiden-root-home"),
		BluetoothStateMountPoint:  "/var/lib/bluetooth",
		BluetoothStateScript:      envOrDefault("AIDEN_BLUETOOTH_STATE_SCRIPT", "/usr/lib/aiden/aiden-bluetooth-state"),
		OTAConfigPath:             envOrDefault("AIDEN_OTA_CONFIG", "/userdata/debian/ota/config.json"),
		AgentBinary:               agentBinary,
		AgentInitScript:           envOrDefault("AIDEN_AGENT_INIT_SCRIPT", "/usr/lib/aiden/aiden-agent-control"),
		FrameServiceInitScript:    envOrDefault("AIDEN_FRAME_SERVICE_INIT_SCRIPT", "/usr/lib/aiden/aiden-frame-control"),
		WiFiProxyInitScript:       envOrDefault("AIDEN_WIFI_PROXY_INIT_SCRIPT", "/usr/lib/aiden/aiden-wifi-proxy-control"),
		// Leave the Agent HTTP target empty by default so the portal follows the
		// address reported by the agent control helper. AIDEN_AGENT_HTTP_BASE_URL
		// remains an explicit override for development and tests.
		AgentHTTPBaseURL:       strings.TrimSpace(os.Getenv("AIDEN_AGENT_HTTP_BASE_URL")),
		OTABinary:              envOrDefault("AIDEN_OTA_BIN", "/usr/lib/aiden/ota"),
		HybridUpdateService:    envOrDefault("AIDEN_HYBRID_UPDATE_SERVICE", "aiden-hybrid-update.service"),
		HybridUpdateMarkerPath: envOrDefault("AIDEN_HYBRID_UPDATE_MARKER", "/userdata/ota/hybrid-update.pending"),
		BootIDPath:             envOrDefault("AIDEN_BOOT_ID_PATH", "/proc/sys/kernel/random/boot_id"),
		EnvRunBinary:           envOrDefault("AIDEN_ENV_RUN_BIN", "/usr/lib/aiden/aiden-managed-env-run"),
		OTAUpdateLockPath:      envOrDefault("AIDEN_CONFIG_WEB_OTA_UPDATE_LOCK", "/tmp/config_web_ota_update.lock"),
		OTAUpdateLogPath:       envOrDefault("AIDEN_CONFIG_WEB_OTA_UPDATE_LOG", "/userdata/ota/config_web_ota_update.log"),
		OTAHealthLogPath:       envOrDefault("AIDEN_CONFIG_WEB_OTA_HEALTH_LOG", "/var/log/ota/ota.log"),
		DmesgBinary:            envOrDefault("AIDEN_CONFIG_WEB_DMESG_BIN", "dmesg"),
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (o Options) Addr() string {
	return net.JoinHostPort(o.BindAddress, strconv.Itoa(o.Port))
}

func (o Options) Validate() error {
	switch o.BindAddress {
	case "0.0.0.0", "127.0.0.1", "192.168.42.1":
	default:
		return fmt.Errorf("unsupported bind address %q", o.BindAddress)
	}
	if o.Port < 1 || o.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if o.WiFiBackend != "legacy" && o.WiFiBackend != "systemd-networkd" {
		return fmt.Errorf("unsupported Wi-Fi backend %q", o.WiFiBackend)
	}
	for name, value := range map[string]string{
		"config":                  o.AgentConfigPath,
		"wifi-config":             o.WiFiConfigPath,
		"wifi-config-environment": o.WiFiConfigEnvironmentPath,
		"wifi-interface":          o.WiFiInterface,
		"ota-state":               o.OTAStatePath,
		"cmdline":                 o.CmdlinePath,
		"system-env":              o.SystemEnvPath,
		"wifi-proxy-config":       o.WiFiProxyConfigPath,
		"local-proxy-environment": o.LocalProxyEnvironmentPath,
		"storage-state":           o.StorageStatePath,
		"web-root":                o.WebRoot,
		"backup-userdata-root":    o.BackupUserdataRoot,
		"backup-sd-root":          o.BackupSDRoot,
		"maintenance-lock":        o.MaintenanceLockPath,
		"backup-job-state-dir":    o.BackupJobStateDir,
		"usb-address":             o.USBAddress,
		"usb-subnet":              o.USBSubnet,
		"systemctl":               o.SystemctlBinary,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("--%s must not be empty", name)
		}
	}
	usbAddress := net.ParseIP(strings.TrimSpace(o.USBAddress))
	if usbAddress == nil {
		return fmt.Errorf("invalid --usb-address %q", o.USBAddress)
	}
	_, usbNetwork, err := net.ParseCIDR(strings.TrimSpace(o.USBSubnet))
	if err != nil {
		return fmt.Errorf("invalid --usb-subnet: %w", err)
	}
	if !usbNetwork.Contains(usbAddress) {
		return fmt.Errorf("--usb-address must belong to --usb-subnet")
	}
	for name, value := range map[string]string{
		"backup-userdata-root": o.BackupUserdataRoot,
		"backup-sd-root":       o.BackupSDRoot,
		"maintenance-lock":     o.MaintenanceLockPath,
		"backup-job-state-dir": o.BackupJobStateDir,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("--%s must be an absolute clean path", name)
		}
	}
	if o.LocalProxyAddress != "" {
		host, portText, err := net.SplitHostPort(o.LocalProxyAddress)
		if err != nil {
			return fmt.Errorf("invalid --local-proxy-address: %w", err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid --local-proxy-address port %q", portText)
		}
		if !netproxy.IsLoopbackHost(host) {
			return fmt.Errorf("--local-proxy-address must use a loopback host")
		}
	}
	return nil
}

func (o Options) ConfigDir() string {
	return filepath.Dir(o.AgentConfigPath)
}

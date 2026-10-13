package wifiproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"aiden-agent/internal/netproxy"
)

const (
	DefaultListenAddress   = "127.0.0.1:18080"
	DefaultConfigPath      = "/userdata/system/wifi-proxies.json"
	DefaultEnvironmentPath = "/run/wifi_proxy/proxy-env"
	DefaultNoProxy         = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7,fe80::/10"
	legacyDefaultNoProxy   = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
	ConfigVersion          = 1
	maxNoProxyLength       = 4096
)

type Mode string

const (
	ModeSystem Mode = "system"
	ModeDirect Mode = "direct"
	ModeProxy  Mode = "proxy"
)

type Network struct {
	Mode       Mode   `json:"mode"`
	ProxyURL   string `json:"proxy_url,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
	NoProxySet bool   `json:"no_proxy_set,omitempty"`
}

type Config struct {
	Version  int                `json:"version"`
	Networks map[string]Network `json:"networks"`
}

func EmptyConfig() Config {
	return Config{Version: ConfigVersion, Networks: make(map[string]Network)}
}

// ValidateSSID rejects empty or padded network names so persisted keys match
// the exact SSID reported by the Wi-Fi runtime.
func ValidateSSID(ssid string) error {
	if strings.TrimSpace(ssid) == "" {
		return errors.New("Wi-Fi SSID must not be empty")
	}
	if strings.TrimSpace(ssid) != ssid {
		return errors.New("Wi-Fi SSID must not have surrounding whitespace")
	}
	return nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return EmptyConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	if len(data) > 64*1024 {
		return Config{}, errors.New("Wi-Fi proxy config exceeds 65536 bytes")
	}
	config := EmptyConfig()
	if err := json.Unmarshal(data, &config); err != nil {
		return Config{}, fmt.Errorf("parse Wi-Fi proxy config: %w", err)
	}
	if config.Version != ConfigVersion {
		return Config{}, fmt.Errorf("unsupported Wi-Fi proxy config version %d", config.Version)
	}
	if config.Networks == nil {
		config.Networks = make(map[string]Network)
	}
	for ssid, network := range config.Networks {
		if err := ValidateSSID(ssid); err != nil {
			return Config{}, fmt.Errorf("Wi-Fi proxy config contains invalid SSID %q: %w", ssid, err)
		}
		normalized, err := NormalizeNetwork(network)
		if err != nil {
			return Config{}, fmt.Errorf("Wi-Fi %q proxy: %w", ssid, err)
		}
		if normalized.Mode == ModeSystem {
			delete(config.Networks, ssid)
		} else {
			config.Networks[ssid] = normalized
		}
	}
	return config, nil
}

func Save(path string, config Config) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("Wi-Fi proxy config path is empty")
	}
	normalized := EmptyConfig()
	for ssid, network := range config.Networks {
		if err := ValidateSSID(ssid); err != nil {
			return fmt.Errorf("Wi-Fi proxy config contains invalid SSID %q: %w", ssid, err)
		}
		value, err := NormalizeNetwork(network)
		if err != nil {
			return fmt.Errorf("Wi-Fi %q proxy: %w", ssid, err)
		}
		if value.Mode != ModeSystem {
			normalized.Networks[ssid] = value
		}
	}
	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func NormalizeNetwork(network Network) (Network, error) {
	network.Mode = Mode(strings.ToLower(strings.TrimSpace(string(network.Mode))))
	switch network.Mode {
	case "", ModeSystem:
		return Network{Mode: ModeSystem}, nil
	case ModeDirect:
		return Network{Mode: ModeDirect}, nil
	case ModeProxy:
		parsed, err := netproxy.Parse(network.ProxyURL, "http", "https", "socks5", "socks5h")
		if err != nil {
			return Network{}, err
		}
		if parsed.Path != "" && parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Network{}, errors.New("proxy URL must not include a path, query, or fragment")
		}
		noProxy, err := NormalizeNoProxy(network.NoProxy)
		if err != nil {
			return Network{}, err
		}
		noProxySet := network.NoProxySet
		if !noProxySet && (noProxy == "" || noProxy == legacyDefaultNoProxy) {
			noProxy = DefaultNoProxy
		}
		return Network{Mode: ModeProxy, ProxyURL: parsed.String(), NoProxy: noProxy, NoProxySet: noProxySet}, nil
	default:
		return Network{}, fmt.Errorf("unsupported mode %q", network.Mode)
	}
}

func NormalizeNoProxy(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > maxNoProxyLength {
		return "", fmt.Errorf("NO_PROXY exceeds %d bytes", maxNoProxyLength)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("NO_PROXY must be a single line")
	}
	if strings.ContainsAny(value, ";&|<>()`$\\'\"") {
		return "", errors.New("NO_PROXY contains unsupported shell syntax")
	}
	return value, nil
}

func RedactedURL(raw string) string {
	parsed, err := netproxy.Parse(raw, "http", "https", "socks5", "socks5h")
	if err != nil {
		return ""
	}
	return parsed.Redacted()
}

// redactedPassword is the placeholder url.URL.Redacted writes in place of a
// password, and so what a client echoes back when it did not change one.
const redactedPassword = "xxxxx"

// RestoreRedactedPassword returns submitted with its password taken from saved
// when the client sent back the redacted form RedactedURL produced: the same
// user with the placeholder password. Clients only ever see the redacted URL,
// so editing its host or port must not wipe the stored password. Anything else
// is returned unchanged.
func RestoreRedactedPassword(submitted, saved string) string {
	next, err := netproxy.Parse(submitted, "http", "https", "socks5", "socks5h")
	if err != nil || next.User == nil {
		return submitted
	}
	password, hasPassword := next.User.Password()
	if !hasPassword || password != redactedPassword {
		return submitted
	}
	previous, err := netproxy.Parse(saved, "http", "https", "socks5", "socks5h")
	if err != nil || previous.User == nil || previous.User.Username() != next.User.Username() {
		return submitted
	}
	savedPassword, ok := previous.User.Password()
	if !ok {
		return submitted
	}
	next.User = url.UserPassword(next.User.Username(), savedPassword)
	return next.String()
}

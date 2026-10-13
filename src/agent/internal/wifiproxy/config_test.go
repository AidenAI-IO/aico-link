package wifiproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRoundTripAndRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi-proxies.json")
	config := EmptyConfig()
	config.Networks["Office"] = Network{
		Mode: ModeProxy, ProxyURL: "http://alice:secret@proxy.example:7890",
		NoProxy: "localhost,.example.com",
	}
	config.Networks["Home"] = Network{Mode: ModeDirect}
	config.Networks["Inherited"] = Network{Mode: ModeSystem, NoProxy: "ignored.example"}
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Networks) != 2 || loaded.Networks["Home"].Mode != ModeDirect {
		t.Fatalf("loaded config=%#v", loaded)
	}
	if _, ok := loaded.Networks["Inherited"]; ok {
		t.Fatalf("system-default network retained Wi-Fi-specific settings: %#v", loaded.Networks["Inherited"])
	}
	if got := RedactedURL(loaded.Networks["Office"].ProxyURL); !strings.Contains(got, "alice:xxxxx@") || strings.Contains(got, "secret") {
		t.Fatalf("redacted URL=%q", got)
	}
	if got := loaded.Networks["Office"].NoProxy; got != "localhost,.example.com" {
		t.Fatalf("NO_PROXY=%q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o, want 600", info.Mode().Perm())
	}
}

func TestConfigRejectsSSIDWithSurroundingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi-proxies.json")
	config := EmptyConfig()
	config.Networks[" Office "] = Network{Mode: ModeDirect}
	if err := Save(path, config); err == nil {
		t.Fatal("Save accepted an SSID with surrounding whitespace")
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"networks":{" Office ":{"mode":"direct"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an SSID with surrounding whitespace")
	}
}

func TestNormalizeNoProxyRejectsMultilineAndOversizedValues(t *testing.T) {
	for _, value := range []string{"example.com\ninternal.example", strings.Repeat("x", maxNoProxyLength+1), "$(touch /tmp/pwned)"} {
		if _, err := NormalizeNoProxy(value); err == nil {
			t.Fatalf("NormalizeNoProxy accepted %q", value[:min(len(value), 32)])
		}
	}
}

func TestConfigRejectsInvalidProxy(t *testing.T) {
	for _, network := range []Network{
		{Mode: "unknown"},
		{Mode: ModeProxy, ProxyURL: "proxy.example:7890"},
		{Mode: ModeProxy, ProxyURL: "ftp://proxy.example:21"},
	} {
		if _, err := NormalizeNetwork(network); err == nil {
			t.Fatalf("NormalizeNetwork(%#v) accepted invalid config", network)
		}
	}
}

func TestNormalizeNetworkDefaultsNoProxyWhenEmpty(t *testing.T) {
	network, err := NormalizeNetwork(Network{Mode: ModeProxy, ProxyURL: "http://proxy.example:7890"})
	if err != nil {
		t.Fatal(err)
	}
	if network.NoProxy != DefaultNoProxy || network.NoProxySet {
		t.Fatalf("omitted NO_PROXY produced %#v", network)
	}

	network, err = NormalizeNetwork(Network{
		Mode: ModeProxy, ProxyURL: "http://proxy.example:7890", NoProxySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.NoProxy != "" || !network.NoProxySet {
		t.Fatalf("explicit empty NO_PROXY produced %#v", network)
	}
}

func TestConfigRoundTripPersistsDefaultNoProxy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi-proxies.json")
	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: "http://proxy.example:7890"}
	if err := Save(path, config); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Networks["Office"].NoProxy; got != DefaultNoProxy {
		t.Fatalf("NO_PROXY=%q, want %q", got, DefaultNoProxy)
	}
}

func TestNormalizeNetworkUpgradesLegacyDefaultNoProxy(t *testing.T) {
	network, err := NormalizeNetwork(Network{
		Mode: ModeProxy, ProxyURL: "http://proxy.example:7890", NoProxy: legacyDefaultNoProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.NoProxy != DefaultNoProxy || network.NoProxySet {
		t.Fatalf("legacy default NO_PROXY produced %#v", network)
	}

	network, err = NormalizeNetwork(Network{
		Mode: ModeProxy, ProxyURL: "http://proxy.example:7890", NoProxy: legacyDefaultNoProxy, NoProxySet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if network.NoProxy != legacyDefaultNoProxy || !network.NoProxySet {
		t.Fatalf("explicit legacy NO_PROXY produced %#v", network)
	}
}

func TestRestoreRedactedPassword(t *testing.T) {
	saved := "http://alice:secret@proxy.example:7890"
	cases := []struct {
		name, submitted, want string
	}{
		{"unchanged redacted form", "http://alice:xxxxx@proxy.example:7890", saved},
		{"new port keeps the password", "http://alice:xxxxx@proxy.example:8080", "http://alice:secret@proxy.example:8080"},
		{"a typed password wins", "http://alice:newpass@proxy.example:7890", "http://alice:newpass@proxy.example:7890"},
		{"another user is not given alice's password", "http://bob:xxxxx@proxy.example:7890", "http://bob:xxxxx@proxy.example:7890"},
		{"no credentials", "http://proxy.example:7890", "http://proxy.example:7890"},
	}
	for _, tc := range cases {
		if got := RestoreRedactedPassword(tc.submitted, saved); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := RestoreRedactedPassword("http://alice:xxxxx@proxy.example:7890", "http://proxy.example:7890"); got != "http://alice:xxxxx@proxy.example:7890" {
		t.Errorf("saved URL without a password must not invent one: %q", got)
	}
	if got := RedactedURL("http://proxy.example:7890"); got != "http://proxy.example:7890" {
		t.Errorf("a proxy without credentials is shown as is: %q", got)
	}
}

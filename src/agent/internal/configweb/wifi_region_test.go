package configweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aico-agent/internal/wifiregion"
)

type fakeWiFiRegionDriver struct {
	mu           sync.Mutex
	country      string
	applied      []string
	err          error
	staleCountry string
	staleReads   int
	onApply      func(string)
}

func (driver *fakeWiFiRegionDriver) Apply(_ context.Context, _ string, country string) error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.applied = append(driver.applied, country)
	if driver.err != nil {
		return driver.err
	}
	driver.country = country
	if driver.onApply != nil {
		driver.onApply(country)
	}
	return nil
}

func (driver *fakeWiFiRegionDriver) Read(ctx context.Context, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if driver.staleReads > 0 {
		driver.staleReads--
		return driver.staleCountry, nil
	}
	return driver.country, nil
}

func TestSetWiFiRadioCountryWaitsForDriverReadback(t *testing.T) {
	server := &Server{
		options: Options{WiFiInterface: "wlan0"},
		wifiRegionDriver: &fakeWiFiRegionDriver{
			country: "CN", staleCountry: "CN", staleReads: 2,
		},
	}
	if err := server.setWiFiRadioCountry(context.Background(), "US"); err != nil {
		t.Fatal(err)
	}
}

const iwScanFixture = `BSS aa:bb:cc:dd:ee:00(on wlan0)
	SSID: Office
	Country: DE	Environment: Indoor/Outdoor
BSS aa:bb:cc:dd:ee:01(on wlan0)
	SSID: Office-Guest
	Country: DE	Environment: Indoor/Outdoor
BSS 11:22:33:44:55:66(on wlan0)
	SSID: Cafe
	Country: DE	Environment: Indoor/Outdoor
BSS 77:88:99:aa:bb:cc(on wlan0)
	SSID: Neighbour
	Country: AT	Environment: Indoor/Outdoor
BSS de:ad:be:ef:00:11(on wlan0)
	SSID: NoCountry
`

func TestParseWiFiScanOutputCountsRadiosNotBSSes(t *testing.T) {
	scan := parseWiFiScanOutput(iwScanFixture)
	// The two Office BSSes share a radio, so together they cast one vote.
	if got := scan.Countries["DE"]; got != 2 {
		t.Fatalf("DE votes=%d, want 2 (one per radio, not per BSS)", got)
	}
	if got := scan.Countries["AT"]; got != 1 {
		t.Fatalf("AT votes=%d, want 1", got)
	}
	if len(scan.SSIDs) != 5 {
		t.Fatalf("SSIDs=%v, want all five networks", scan.SSIDs)
	}
}

func TestParseWiFiScanOutputDiscardsUnusableCountryCodes(t *testing.T) {
	scan := parseWiFiScanOutput(`BSS aa:bb:cc:dd:ee:00(on wlan0)
	Country: 00	Environment: Indoor/Outdoor
BSS aa:bb:cc:dd:ee:10(on wlan0)
	Country: ZZ	Environment: Indoor/Outdoor
BSS aa:bb:cc:dd:ee:20(on wlan0)
	Country: XX
`)
	if len(scan.Countries) != 0 {
		t.Fatalf("Countries=%v, want the world domain and unknown codes discarded", scan.Countries)
	}
}

func TestParseWiFiScanOutputIgnoresCountryWithoutABSSHeader(t *testing.T) {
	scan := parseWiFiScanOutput("Country: DE\tEnvironment: Indoor/Outdoor\n")
	if len(scan.Countries) != 0 {
		t.Fatalf("Countries=%v, want no vote without an owning radio", scan.Countries)
	}
}

func TestWiFiConnectRejectsUnsupportedCountryBeforeStartingTheTask(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(
		http.MethodPut, "/api/network/wifi/connection",
		strings.NewReader(`{"ssid":"test-network","psk":"secret","country":"USA"}`)))
	// A 202 here would mean the task was already created and the caller could
	// never be told the country was rejected.
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.Code, resp.Body.String())
	}
	server.wifiMu.Lock()
	job := server.wifiJob
	server.wifiMu.Unlock()
	if job != nil {
		t.Fatal("a rejected request must not create a background task")
	}
}

func TestWiFiConnectRejectsWorldCountryBeforeStartingTheTask(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut,
		"/api/network/wifi/connection", strings.NewReader(`{"ssid":"test-network","country":"00"}`)))
	if resp.Code != http.StatusBadRequest || server.wifiJob != nil {
		t.Fatalf("status=%d job=%v body=%s", resp.Code, server.wifiJob, resp.Body.String())
	}
}

func TestWiFiRegionUpdateIsTheOnlySourceOfUserProvenance(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(
		http.MethodPut, "/api/network/wifi/region", strings.NewReader(`{"country":"ZZ"}`)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 for an unsupported country", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(
		http.MethodPut, "/api/network/wifi/region", strings.NewReader(`{"country":"00"}`)))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want the permissive world domain rejected as a choice", resp.Code)
	}
}

func TestWiFiRegionResolveOffersCandidatesWithoutApplyingThem(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(
		http.MethodPost, "/api/network/wifi/region/resolve",
		strings.NewReader(`{"browser_timezone":"Europe/Berlin"}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Current    map[string]any   `json:"current"`
		Candidates []map[string]any `json:"candidates"`
		Options    []string         `json:"options"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Current["country"] != "CN" {
		t.Fatalf("current=%v, want the configured CN untouched", payload.Current)
	}
	if len(payload.Candidates) == 0 || payload.Candidates[0]["country"] != "DE" {
		t.Fatalf("candidates=%v, want DE suggested from the browser timezone", payload.Candidates)
	}
	if len(payload.Options) == 0 {
		t.Fatal("options must list the selectable countries")
	}
	// Resolving must not touch the authoritative file.
	data, err := os.ReadFile(options.WiFiConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "country=CN") {
		t.Fatalf("configuration changed by a read-only resolve: %q", data)
	}
}

func TestWiFiRegionStateIsReconciledAgainstTheConfigurationFile(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sidecar left behind by a restore, disagreeing with the live file.
	if err := wifiregion.SaveState(options.WiFiRegionStatePath, wifiregion.State{
		Country: "DE", Source: wifiregion.SourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	state := server.loadWiFiRegionState("CN")
	if state.Country != "CN" || state.Source != wifiregion.SourceUnknown {
		t.Fatalf("state=%+v, want the configuration file to win with provenance downgraded", state)
	}
	data, err := os.ReadFile(options.WiFiConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "country=CN") {
		t.Fatalf("a stale sidecar rewrote the authoritative configuration: %q", data)
	}
}

func TestUnreadableWiFiRegionStateDoesNotBecomeExistingChoice(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.WiFiRegionStatePath, []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	state := server.loadWiFiRegionState("CN")
	if state.Country != "CN" || state.Source != wifiregion.SourceUnknown {
		t.Fatalf("state=%+v, want CN/unknown after a corrupt sidecar", state)
	}
	data, err := os.ReadFile(options.WiFiRegionStatePath)
	if err != nil || string(data) != "{invalid" {
		t.Fatalf("corrupt sidecar unexpectedly changed: data=%q err=%v", data, err)
	}
}

func TestStaleRegionStateCannotAutoApplyBeacon(t *testing.T) {
	options := testOptions(t)
	options.WiFiRegionAutoApplyBeacon = true
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wifiregion.SaveState(options.WiFiRegionStatePath, wifiregion.State{
		Country: "DE", Source: wifiregion.SourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	server.recordWiFiCountryVotes(map[string]int{"US": 5})
	decision := server.resolveWiFiRegion("CN", "")
	if decision.Country != "CN" || decision.Source != wifiregion.SourceUnknown {
		t.Fatalf("decision=%+v, want authoritative CN kept for manual selection", decision)
	}
}

func TestConnectionCountryCannotOverrideUserChoice(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wifiregion.SaveState(options.WiFiRegionStatePath, wifiregion.State{
		Country: "CN", Source: wifiregion.SourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	result := server.runWiFiConnection(context.Background(), wifiConnectionRequest{SSID: "test", Country: "US"})
	if result["ok"] != false || !strings.Contains(result["error"].(string), "region endpoint") {
		t.Fatalf("connection override result=%v", result)
	}
	state, err := wifiregion.LoadState(options.WiFiRegionStatePath)
	if err != nil || state.Country != "CN" || state.Source != wifiregion.SourceUser {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestRegionUpdateAfterClientDisconnectAppliesAndPersists(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for name, script := range map[string]string{
		"ip":        "#!/bin/sh\nexit 0\n",
		"wpa_cli":   "#!/bin/sh\nprintf '%s\\n' 'wpa_state=DISCONNECTED'\n",
		"systemctl": "#!/bin/sh\n[ \"$1\" = restart ] && exit 0\nexit 1\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	driver := &fakeWiFiRegionDriver{country: "CN"}
	server.wifiRegionDriver = driver
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPut, "/api/network/wifi/region", strings.NewReader(`{"country":"US"}`)).WithContext(ctx)
	cancel()
	response := httptest.NewRecorder()
	server.handleWiFiRegionUpdate(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want a completed update", response.Code, response.Body.String())
	}
	config, err := loadWiFiConfig(options.WiFiConfigPath)
	if err != nil || config.Country != "US" || len(config.Networks) != 0 {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	state, err := wifiregion.LoadState(options.WiFiRegionStatePath)
	if err != nil || state.Country != "US" || state.Source != wifiregion.SourceUser {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err := os.Stat(options.WiFiConfigPath + ".region-candidate"); err != nil {
		t.Fatalf("running candidate removed: %v", err)
	}
}

func TestNoNetworkApplyReportsSelectorCleanupFailure(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	binDir := t.TempDir()
	t.Setenv("AICO_TEST_WIFI_SELECTOR", options.WiFiConfigEnvironmentPath)
	for name, script := range map[string]string{
		"ip": "#!/bin/sh\nexit 0\n",
		"systemctl": `#!/bin/sh
set -eu
/bin/rm -f "$AICO_TEST_WIFI_SELECTOR"
/bin/mkdir "$AICO_TEST_WIFI_SELECTOR"
/bin/touch "$AICO_TEST_WIFI_SELECTOR/stale"
`,
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	t.Cleanup(func() { _ = os.RemoveAll(options.WiFiConfigEnvironmentPath) })
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	result := server.applyWiFiWithConnectivity(context.Background(), options.WiFiConfigPath, true, false)
	if result.ExitCode == 0 || !strings.Contains(string(result.Output), "remove runtime wpa_supplicant config selector") {
		t.Fatalf("result=%+v", result)
	}
}

func TestRegionUpdateRollsBackRadioWhenCandidateRestartFails(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	t.Setenv("AICO_TEST_ORIGINAL_WIFI_CONFIG", options.WiFiConfigPath)
	t.Setenv("AICO_TEST_WIFI_SELECTOR", options.WiFiConfigEnvironmentPath)
	for name, script := range map[string]string{
		"ip":      "#!/bin/sh\nexit 0\n",
		"wpa_cli": "#!/bin/sh\nprintf '%s\\n' 'wpa_state=DISCONNECTED'\n",
		"systemctl": `#!/bin/sh
/usr/bin/grep -Fqx "AICO_WPA_SUPPLICANT_CONFIG=\"$AICO_TEST_ORIGINAL_WIFI_CONFIG\"" "$AICO_TEST_WIFI_SELECTOR"
`,
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	driver := &fakeWiFiRegionDriver{country: "CN"}
	server.wifiRegionDriver = driver
	result := server.applyWiFiRegion(context.Background(), "US")
	if result["ok"] != false {
		t.Fatalf("result=%v, want candidate failure", result)
	}
	config, err := loadWiFiConfig(options.WiFiConfigPath)
	if err != nil || config.Country != "CN" || driver.country != "CN" {
		t.Fatalf("config=%+v driver=%+v err=%v", config, driver, err)
	}
	if strings.Join(driver.applied, ",") != "US,CN" {
		t.Fatalf("driver applications=%v", driver.applied)
	}
}

func TestFirstWiFiConnectionTimeoutRestoresCountryAndEmptyConfiguration(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	options.WiFiRegionAutoApplyBeacon = true
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wifiregion.SaveState(options.WiFiRegionStatePath, wifiregion.State{
		Country: "CN", Source: wifiregion.SourceFactory,
	}); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	t.Setenv("AICO_TEST_ORIGINAL_WIFI_CONFIG", options.WiFiConfigPath)
	t.Setenv("AICO_TEST_WIFI_CANDIDATE", options.WiFiConfigPath+".candidate")
	t.Setenv("AICO_TEST_WIFI_SELECTOR", options.WiFiConfigEnvironmentPath)
	for name, script := range map[string]string{
		"ip":      "#!/bin/sh\nexit 0\n",
		"wpa_cli": "#!/bin/sh\nprintf '%s\\n' 'wpa_state=DISCONNECTED'\n",
		"systemctl": `#!/bin/sh
set -eu
[ "${1:-}" = restart ]
[ "${2:-}" = wpa_supplicant@wlan0.service ]
if /usr/bin/grep -Fqx "AICO_WPA_SUPPLICANT_CONFIG=\"$AICO_TEST_WIFI_CANDIDATE\"" "$AICO_TEST_WIFI_SELECTOR"; then
  exit 1
fi
/usr/bin/grep -Fqx "AICO_WPA_SUPPLICANT_CONFIG=\"$AICO_TEST_ORIGINAL_WIFI_CONFIG\"" "$AICO_TEST_WIFI_SELECTOR"
`,
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	driver := &fakeWiFiRegionDriver{country: "CN", onApply: func(country string) {
		if country == "US" {
			cancel()
		}
	}}
	server.wifiRegionDriver = driver
	server.recordWiFiCountryVotes(map[string]int{"US": 3})
	result := server.runWiFiConnection(ctx, wifiConnectionRequest{SSID: "missing-network"})
	if result["ok"] != false {
		t.Fatalf("result=%v, want a canceled candidate", result)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("candidate context error = %v", ctx.Err())
	}
	rollback, ok := result["wifi_rollback"].(map[string]any)
	if !ok || rollback["ok"] != true {
		t.Fatalf("rollback=%v", result["wifi_rollback"])
	}
	config, err := loadWiFiConfig(options.WiFiConfigPath)
	if err != nil || config.Country != "CN" || len(config.Networks) != 0 {
		t.Fatalf("restored configuration=%+v err=%v", config, err)
	}
	if driver.country != "CN" || strings.Join(driver.applied, ",") != "US,CN" {
		t.Fatalf("driver=%+v", driver)
	}
	state, err := wifiregion.LoadState(options.WiFiRegionStatePath)
	if err != nil || state.Country != "CN" || state.Source != wifiregion.SourceFactory {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestResolveWiFiRegionRespectsTheAutoApplySwitch(t *testing.T) {
	// A freshly provisioned device: the migration wrote both the default country
	// and the marker saying it is only a default. Without the marker the device
	// would read as a pre-existing deployment, which is deliberately never
	// re-decided, and the switch would not be exercised at all.
	newServer := func(t *testing.T, autoApply bool, configCountry string) *Server {
		t.Helper()
		options := testOptions(t)
		options.WiFiRegionAutoApplyBeacon = autoApply
		if configCountry != "" {
			if err := os.WriteFile(options.WiFiConfigPath, []byte("country="+configCountry+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := wifiregion.SaveState(options.WiFiRegionStatePath, wifiregion.State{
				Country: configCountry, Source: wifiregion.SourceFactory,
			}); err != nil {
				t.Fatal(err)
			}
		}
		server, err := NewServer(options)
		if err != nil {
			t.Fatal(err)
		}
		server.recordWiFiCountryVotes(map[string]int{"DE": 5})
		return server
	}

	t.Run("a strong beacon majority is ignored while the switch is off", func(t *testing.T) {
		server := newServer(t, false, "CN")
		got := server.resolveWiFiRegion("CN", "")
		if got.Country != "CN" {
			t.Fatalf("resolveWiFiRegion()=%+v, want the configured CN kept", got)
		}
	})

	t.Run("with no configured country the switch still holds", func(t *testing.T) {
		// Falling through to the beacon value here would apply a suggestion the
		// operator explicitly disabled.
		server := newServer(t, false, "")
		got := server.resolveWiFiRegion("", "")
		if got.Country != wifiregion.FactoryCountry || got.Source != wifiregion.SourceFactory {
			t.Fatalf("resolveWiFiRegion()=%+v, want the labelled factory default", got)
		}
	})

	t.Run("the switch lets a strong majority through", func(t *testing.T) {
		server := newServer(t, true, "CN")
		got := server.resolveWiFiRegion("CN", "")
		if got.Country != "DE" || got.Source != wifiregion.SourceBeacon {
			t.Fatalf("resolveWiFiRegion()=%+v, want DE/beacon", got)
		}
	})

	t.Run("an existing deployment is never re-decided, switch or not", func(t *testing.T) {
		options := testOptions(t)
		options.WiFiRegionAutoApplyBeacon = true
		// A configuration file with no sidecar: a device that predates this
		// feature, whose country was set deliberately.
		if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(options)
		if err != nil {
			t.Fatal(err)
		}
		server.recordWiFiCountryVotes(map[string]int{"DE": 5})
		got := server.resolveWiFiRegion("CN", "")
		if got.Country != "CN" || got.Source != wifiregion.SourceExisting {
			t.Fatalf("resolveWiFiRegion()=%+v, want CN/existing", got)
		}
	})

	t.Run("a user choice outranks the switch", func(t *testing.T) {
		server := newServer(t, true, "JP")
		if err := wifiregion.SaveState(server.options.WiFiRegionStatePath, wifiregion.State{
			Country: "JP", Source: wifiregion.SourceUser,
		}); err != nil {
			t.Fatal(err)
		}
		got := server.resolveWiFiRegion("JP", "")
		if got.Country != "JP" || got.Source != wifiregion.SourceUser {
			t.Fatalf("resolveWiFiRegion()=%+v, want the user's JP kept", got)
		}
	})
}

// A supplicant that never starts is an apply failure. The association branch
// that follows must not relabel it as "could not join this network".
func TestApplyWiFiReportsSupplicantStartFailureAsApplyFailed(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	if err := os.WriteFile(options.WiFiConfigPath, []byte("country=CN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	for name, script := range map[string]string{
		"ip":        "#!/bin/sh\nexit 0\n",
		"systemctl": "#!/bin/sh\nexit 5\n",
	} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	result := server.applyWiFi(context.Background(), options.WiFiConfigPath, true)
	if result.ExitCode == 0 || result.FailureReason != wifiFailureApply {
		t.Fatalf("exit=%d reason=%q, want apply_failed", result.ExitCode, result.FailureReason)
	}
}

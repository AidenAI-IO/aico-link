package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/ota"
)

func TestSplitCommandAndFlagsAllowsGlobalFlagsBeforeCommand(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--config", "test.json", "verify-manifest", "manifest.json", "--state-dir", "state"})
	if command != "verify-manifest" {
		t.Fatalf("command = %q, want verify-manifest", command)
	}
	want := []string{"--config", "test.json", "manifest.json", "--state-dir", "state"}
	if len(rest) != len(want) {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("rest = %#v, want %#v", rest, want)
		}
	}
}

func TestSplitCommandAndFlagsDefaultsToHealth(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--config", "test.json"})
	if command != "health" {
		t.Fatalf("command = %q, want health", command)
	}
	if len(rest) != 2 || rest[0] != "--config" || rest[1] != "test.json" {
		t.Fatalf("rest = %#v", rest)
	}
}

func TestDefaultConfigPathUsesDebianPersistentLayout(t *testing.T) {
	if ota.DefaultOTAConfigPath != "/userdata/debian/ota/config.json" {
		t.Fatalf("DefaultOTAConfigPath = %q, want Debian persistent config", ota.DefaultOTAConfigPath)
	}
}

func TestSplitCommandAndFlagsConsumesFlagValuesBeforeCommand(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--manifest-url", "https://example.com/manifest.json", "update"})
	if command != "update" {
		t.Fatalf("command = %q, want update", command)
	}
	want := []string{"--manifest-url", "https://example.com/manifest.json"}
	if len(rest) != len(want) || rest[0] != want[0] || rest[1] != want[1] {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
	config, err := parseConfigFlags(rest)
	if err != nil {
		t.Fatalf("parseConfigFlags() error = %v", err)
	}
	if config.ManifestURL != "https://example.com/manifest.json" {
		t.Fatalf("manifest_url = %q, want https://example.com/manifest.json", config.ManifestURL)
	}
}

func TestSplitCommandAndFlagsKeepsCheckNowAlias(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--manifest-url", "https://example.com/manifest.json", "check-now"})
	if command != "check-now" {
		t.Fatalf("command = %q, want check-now", command)
	}
	want := []string{"--manifest-url", "https://example.com/manifest.json"}
	if len(rest) != len(want) || rest[0] != want[0] || rest[1] != want[1] {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
}

func TestSplitCommandAndFlagsSupportsHealthCommand(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--state-dir", "state", "health"})
	if command != "health" {
		t.Fatalf("command = %q, want health", command)
	}
	want := []string{"--state-dir", "state"}
	if len(rest) != len(want) || rest[0] != want[0] || rest[1] != want[1] {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
}

func TestSplitCommandAndFlagsSupportsMarkHealthCommand(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--config", "debian.json", "mark-health"})
	if command != "mark-health" {
		t.Fatalf("command = %q, want mark-health", command)
	}
	want := []string{"--config", "debian.json"}
	if len(rest) != len(want) || rest[0] != want[0] || rest[1] != want[1] {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
}

func TestSplitCommandAndFlagsSupportsProvisionIdentityCommand(t *testing.T) {
	command, rest := splitCommandAndFlags([]string{"--config", ota.DefaultDebianOTAConfigPath, "provision-identity"})
	if command != "provision-identity" {
		t.Fatalf("command = %q, want provision-identity", command)
	}
	want := []string{"--config", ota.DefaultDebianOTAConfigPath}
	if len(rest) != len(want) || rest[0] != want[0] || rest[1] != want[1] {
		t.Fatalf("rest = %#v, want %#v", rest, want)
	}
}

func TestRunRejectsExtraPositionalsForNonManifestCommands(t *testing.T) {
	for _, args := range [][]string{
		{"health", "extra"},
		{"mark-health", "extra"},
		{"provision-identity", "extra"},
		{"update", "extra", "--dry-run"},
		{"check-now", "extra", "--dry-run"},
		{"status", "extra"},
	} {
		err := run(args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("run(%#v) error = %v, want usage", args, err)
		}
	}
}

func TestDefaultRebootIsRealUnlessDryRun(t *testing.T) {
	reboot := rebootForConfig(ota.UpdaterConfig{})
	if reboot == nil {
		t.Fatalf("rebootForConfig(default) = nil, want real reboot function")
	}
	dryRunReboot := rebootForConfig(ota.UpdaterConfig{DryRun: true})
	if dryRunReboot == nil {
		t.Fatalf("rebootForConfig(dry-run) = nil")
	}
	if err := dryRunReboot(); err != nil {
		t.Fatalf("dry-run reboot error = %v", err)
	}
}

func TestSelfCheckCommandOutcomes(t *testing.T) {
	for _, tt := range []struct {
		name            string
		agentStatus     string
		configWebStatus string
		passed          int
		warnings        int
		failures        int
		wantError       string
	}{
		{"healthy", "pass", "pass", 2, 0, 0, ""},
		{"agent only failure", "warn", "pass", 1, 1, 0, ""},
		{"config web failure", "pass", "fail", 1, 0, 1, "self-check failed: 1 required check(s)"},
		{"both unavailable", "warn", "fail", 0, 1, 1, "self-check failed: 1 required check(s)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			stateDir := filepath.Join(dir, "state")
			configPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			startedAt := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
			// Probe classification is covered by internal/ota tests. Here the
			// report isolates the command's success/error and reporting contract.
			wantReport := ota.SelfCheckReport{
				StartedAt:  startedAt,
				FinishedAt: startedAt.Add(time.Second),
				Items: map[string]ota.SelfCheckItem{
					"agent_http": {Status: tt.agentStatus},
					"config_web": {Status: tt.configWebStatus},
				},
				Passed: tt.passed, Warnings: tt.warnings, Failures: tt.failures,
			}
			probeCalls := 0
			var out bytes.Buffer
			err := runWithDependencies([]string{"self-check", "--config", configPath, "--state-dir", stateDir}, &out,
				func(config *ota.UpdaterConfig) { config.StorageMountPoint = stateDir },
				func(ctx context.Context, config ota.SelfCheckConfig) ota.SelfCheckReport {
					probeCalls++
					if err := ctx.Err(); err != nil {
						t.Fatalf("probe context: %v", err)
					}
					if config != ota.DefaultSelfCheckConfig() {
						t.Fatalf("probe config = %+v, want defaults", config)
					}
					return wantReport
				})
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("self-check returned an error: %v", err)
				}
			} else if err == nil || err.Error() != tt.wantError {
				t.Fatalf("self-check error = %v, want %q", err, tt.wantError)
			}
			if probeCalls != 1 {
				t.Fatalf("probe calls = %d, want 1", probeCalls)
			}
			persisted, err := os.ReadFile(filepath.Join(stateDir, "health", "current.json"))
			if err != nil {
				t.Fatalf("read self-check report: %v", err)
			}
			for name, data := range map[string][]byte{"stdout": out.Bytes(), "persisted": persisted} {
				var report ota.SelfCheckReport
				if err := json.Unmarshal(data, &report); err != nil {
					t.Fatalf("decode %s report: %v", name, err)
				}
				if !reflect.DeepEqual(report, wantReport) {
					t.Fatalf("%s report = %+v, want %+v", name, report, wantReport)
				}
			}
		})
	}
}

func TestUpdateRunsManualCheckWhenNoUpdate(t *testing.T) {
	fixture := newNoUpdateFixture(t)

	var out bytes.Buffer
	err := runWithConfig([]string{
		"update",
		"--config", fixture.configPath,
		"--misc", fixture.miscPath,
		"--manifest-url", fixture.manifestURL,
		"--public-key", fixture.keyPath,
	}, &out, fixture.configureStorage)
	if err != nil {
		t.Fatalf("run(update) error = %v", err)
	}
	if !strings.Contains(out.String(), `"NoUpdate":true`) {
		t.Fatalf("output = %q, want no update", out.String())
	}
}

func TestCheckReturnsAvailabilityWithoutStartingUpdate(t *testing.T) {
	fixture := newNoUpdateFixture(t)
	var out bytes.Buffer
	err := runWithConfig([]string{"check", "--config", fixture.configPath,
		"--manifest-url", fixture.manifestURL, "--public-key", fixture.keyPath}, &out, fixture.configureStorage)
	if err != nil {
		t.Fatal(err)
	}
	var result ota.CheckResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Available || result.Version == "" {
		t.Fatalf("result=%+v", result)
	}
}

func TestUpdateReturnsManualCheckFailure(t *testing.T) {
	fixture := newNoUpdateFixture(t)
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not a manifest"))
	}))
	t.Cleanup(badServer.Close)

	err := runWithConfig([]string{
		"update",
		"--config", fixture.configPath,
		"--misc", fixture.miscPath,
		"--manifest-url", badServer.URL + "/manifest.json",
		"--public-key", fixture.keyPath,
	}, &bytes.Buffer{}, fixture.configureStorage)
	if err == nil {
		t.Fatalf("run(update) error = nil, want manifest failure")
	}
}

type noUpdateFixture struct {
	configPath    string
	mountInfoPath string
	storageDevice string
	stateDir      string
	miscPath      string
	keyPath       string
	manifestURL   string
}

func (f noUpdateFixture) configureStorage(config *ota.UpdaterConfig) {
	config.StateDir = f.stateDir
	config.DownloadDir = filepath.Join(f.stateDir, "downloads")
	config.UpdateLockPath = filepath.Join(f.stateDir, ota.DefaultOTAUpdateLockName)
	config.StorageMountPoint = f.stateDir
	config.StorageDevicePath = f.storageDevice
	config.StorageFilesystem = "ext4"
	config.MountInfoPath = f.mountInfoPath
}

func newNoUpdateFixture(t *testing.T) noUpdateFixture {
	t.Helper()

	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	configPath := filepath.Join(tmp, "config.json")
	mountInfoPath := filepath.Join(tmp, "mountinfo")
	storageDevicePath := filepath.Join(tmp, "ota-device")
	miscPath := filepath.Join(tmp, "misc.img")
	keyPath := filepath.Join(tmp, "ota_pubkey.pem")
	version := "20260521-120000-abcdef0"
	buildTime := "2026-05-21T12:00:00Z"

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey() error = %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatalf("WriteFile(pubkey) error = %v", err)
	}
	if err := ota.CreateMiscImage(miscPath, ota.DefaultMiscSize); err != nil {
		t.Fatalf("CreateMiscImage() error = %v", err)
	}
	state := ota.State{
		LastCommittedVersion:   version,
		LastCommittedBuildTime: buildTime,
		Slots:                  map[string]ota.SlotPartitionInfo{},
	}
	if err := ota.SaveState(filepath.Join(stateDir, "state.json"), state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
	if err := os.WriteFile(storageDevicePath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(storage device) error = %v", err)
	}
	mountInfo := fmt.Sprintf("36 25 179:10 / %s rw,relatime - ext4 %s rw\n", stateDir, storageDevicePath)
	if err := os.WriteFile(mountInfoPath, []byte(mountInfo), 0o644); err != nil {
		t.Fatalf("WriteFile(mountinfo) error = %v", err)
	}
	configBytes, err := json.Marshal(ota.UpdaterConfig{})
	if err != nil {
		t.Fatalf("Marshal(config) error = %v", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	manifest := ota.Manifest{
		SchemaVersion: 2,
		Channel:       "stable",
		Version:       version,
		BuildTime:     buildTime,
		Parts: []ota.ManifestPart{{
			Name:   "boot",
			AssetA: &ota.ManifestAsset{Name: "boot_a.img", Size: 1, SHA256: strings.Repeat("a", 64)},
			AssetB: &ota.ManifestAsset{Name: "boot_b.img", Size: 1, SHA256: strings.Repeat("b", 64)},
		}},
		Signature: ota.ManifestSignature{Algorithm: "ed25519"},
	}
	canonical, err := ota.CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature.Value = hex.EncodeToString(ed25519.Sign(priv, canonical))
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/manifest.json" {
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
		_, _ = w.Write(manifestBytes)
	}))
	t.Cleanup(server.Close)

	return noUpdateFixture{
		configPath:    configPath,
		mountInfoPath: mountInfoPath,
		storageDevice: storageDevicePath,
		stateDir:      stateDir,
		miscPath:      miscPath,
		keyPath:       keyPath,
		manifestURL:   server.URL + "/manifest.json",
	}
}

func TestStateDirOverrideDoesNotDisableDedicatedMountValidation(t *testing.T) {
	err := run([]string{
		"status",
		"--config", filepath.Join(t.TempDir(), "missing.json"),
		"--state-dir", t.TempDir(),
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "state_dir must be inside the dedicated OTA storage mount") {
		t.Fatalf("run(status) error = %v, want dedicated storage validation", err)
	}
}

func TestParseConfigFlagsRejectsInvalidUpdaterConfig(t *testing.T) {
	err := run([]string{"status", "--config", filepath.Join(t.TempDir(), "missing.json"), "--switch-tries", "16"}, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("run(status) error = nil, want validation error")
	}
}

func TestParseConfigFlagsRejectsSwitchTriesOverflow(t *testing.T) {
	_, err := parseConfigFlags([]string{"--config", filepath.Join(t.TempDir(), "missing.json"), "--switch-tries", "256"})
	if err == nil || !strings.Contains(err.Error(), "switch-tries") {
		t.Fatalf("parseConfigFlags() error = %v, want switch-tries rejection", err)
	}
}

func TestVerifyManifestSupportsRemoteURL(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	manifest := ota.Manifest{
		SchemaVersion: 2,
		Channel:       "stable",
		Version:       "20260521-120000-abcdef0",
		BuildTime:     "2026-05-21T12:00:00Z",
		Parts: []ota.ManifestPart{{
			Name:   "boot",
			AssetA: &ota.ManifestAsset{Name: "boot_a.img", Size: 1, SHA256: strings.Repeat("a", 64)},
			AssetB: &ota.ManifestAsset{Name: "boot_b.img", Size: 1, SHA256: strings.Repeat("b", 64)},
		}},
		Signature: ota.ManifestSignature{Algorithm: "ed25519"},
	}
	canonical, err := ota.CanonicalManifestJSON(manifest)
	if err != nil {
		t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature.Value = hex.EncodeToString(ed25519.Sign(priv, canonical))
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal(manifest) error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(manifestBytes)
	}))
	t.Cleanup(server.Close)
	keyPath := filepath.Join(t.TempDir(), "ota_pubkey.pem")
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey() error = %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatalf("WriteFile(pubkey) error = %v", err)
	}

	var out bytes.Buffer
	if err := run([]string{"verify-manifest", server.URL, "--public-key", keyPath}, &out); err != nil {
		t.Fatalf("run(verify-manifest URL) error = %v", err)
	}
	if !strings.Contains(out.String(), "manifest ok") {
		t.Fatalf("output = %q, want manifest ok", out.String())
	}
}

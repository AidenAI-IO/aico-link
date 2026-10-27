package ota

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aico-agent/internal/logging"
)

func TestUpdaterHappyPathDownloadsWritesSwitchesAndReboots(t *testing.T) {
	env := newUpdaterTestEnv(t)
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB || result.Version != env.version {
		t.Fatalf("CheckOnce() = %+v", result)
	}
	if env.reboots != 1 {
		t.Fatalf("reboots = %d, want 1", env.reboots)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "boot-b-v2")
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "rootfs-v2")
	assertFileContent(t, filepath.Join(env.blockDir, "boot_a"), "old-boot-a")

	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	active, ok := ab.ActiveSlot()
	if !ok || active != SlotB || ab.Slots[SlotB].TriesRemaining != env.config.SwitchTries {
		t.Fatalf("misc after update active=%v ok=%v slotB=%+v", active, ok, ab.Slots[SlotB])
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "health.ok")); !os.IsNotExist(err) {
		t.Fatalf("stale health marker still exists: %v", err)
	}
	pendingBytes, err := os.ReadFile(filepath.Join(env.stateDir, "pending_boot.json"))
	if err != nil {
		t.Fatalf("pending_boot.json missing: %v", err)
	}
	var pending PendingBoot
	if err := json.Unmarshal(pendingBytes, &pending); err != nil {
		t.Fatalf("pending boot JSON error = %v", err)
	}
	if pending.TargetSlot != "b" || pending.TargetVersion != env.version || pending.Nonce == "" {
		t.Fatalf("pending boot = %+v", pending)
	}
}

func TestUpdaterDownloadsTarGzAssetsAndWritesExtractedImages(t *testing.T) {
	env := newUpdaterTestEnv(t)
	bootImage := []byte("boot-b-v2")
	rootfsImage := []byte("rootfs-v2")
	bootArchive := testTarGzImage(t, "boot_b.img", bootImage)
	rootfsArchive := testTarGzImage(t, "rootfs.img", rootfsImage)
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": bootImage,
		"rootfs.img": rootfsImage,
	}, func(m *Manifest) {
		m.Parts[0].AssetB = testCompressedManifestAsset("boot_b.img.tar.gz", bootArchive, bootImage)
		m.Parts[1].Asset = testCompressedManifestAsset("rootfs.img.tar.gz", rootfsArchive, rootfsImage)
	})
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img.tar.gz": bootArchive,
		"rootfs.img.tar.gz": rootfsArchive,
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v, want update to slot B", result)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), string(bootImage))
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), string(rootfsImage))
	for _, name := range []string{"boot_b.img.tar.gz", "rootfs.img.tar.gz"} {
		if _, err := os.Stat(filepath.Join(env.downloadDir, name)); !os.IsNotExist(err) {
			t.Fatalf("verified archive cache %s was not deleted: %v", name, err)
		}
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got := state.DownloadedHashes["boot"]; got != testSHA256Hex(bootImage) {
		t.Fatalf("state.DownloadedHashes[boot] = %s, want extracted image hash", got)
	}
	if got := state.DownloadedHashes["rootfs"]; got != testSHA256Hex(rootfsImage) {
		t.Fatalf("state.DownloadedHashes[rootfs] = %s, want extracted image hash", got)
	}
}

func TestUpdaterRejectsTarGzImageSHA256MismatchDuringWrite(t *testing.T) {
	env := newUpdaterTestEnv(t)
	bootImage := []byte("boot-b-v2")
	bootArchive := testTarGzImage(t, "boot_b.img", bootImage)
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": bootImage,
		"boot_b.img": bootImage,
		"rootfs.img": []byte("rootfs-v2"),
	}, func(m *Manifest) {
		m.Parts[0].AssetB = testCompressedManifestAsset("boot_b.img.tar.gz", bootArchive, bootImage)
		m.Parts[0].AssetB.ImageSHA256 = strings.Repeat("d", 64)
	})
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img.tar.gz": bootArchive,
		"rootfs.img":        []byte("rootfs-v2"),
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "image_sha256") {
		t.Fatalf("CheckOnce() error = %v, want image_sha256 mismatch", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), string(bootImage))
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "old-rootfs-b")
	ab, readErr := readMiscFile(env.miscPath)
	if readErr != nil {
		t.Fatalf("readMiscFile() error = %v", readErr)
	}
	if ab.Bootable(SlotB) {
		t.Fatalf("target slot is bootable after image hash mismatch: %+v", ab.Slots[SlotB])
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterDryRunRejectsTarGzImageSHA256MismatchBeforeWriting(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.config.DryRun = true
	bootImage := []byte("boot-b-v2")
	bootArchive := testTarGzImage(t, "boot_b.img", bootImage)
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": bootImage,
		"boot_b.img": bootImage,
		"rootfs.img": []byte("rootfs-v2"),
	}, func(m *Manifest) {
		m.Parts[0].AssetB = testCompressedManifestAsset("boot_b.img.tar.gz", bootArchive, bootImage)
		m.Parts[0].AssetB.ImageSHA256 = strings.Repeat("d", 64)
	})
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img.tar.gz": bootArchive,
		"rootfs.img":        []byte("rootfs-v2"),
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "image_sha256") {
		t.Fatalf("CheckOnce() error = %v, want image_sha256 mismatch", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
	ab, readErr := readMiscFile(env.miscPath)
	if readErr != nil {
		t.Fatalf("readMiscFile() error = %v", readErr)
	}
	if !ab.Bootable(SlotA) || ab.Bootable(SlotB) {
		t.Fatalf("misc changed during dry-run: slot_a=%+v slot_b=%+v", ab.Slots[SlotA], ab.Slots[SlotB])
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestDefaultHTTPTimeoutAllowsLargeImageDownloads(t *testing.T) {
	if DefaultHTTPDownloadLimit < time.Hour {
		t.Fatalf("DefaultHTTPDownloadLimit = %s, want at least 1h", DefaultHTTPDownloadLimit)
	}
	if DefaultHTTPRequestLimit != 30*time.Minute {
		t.Fatalf("DefaultHTTPRequestLimit = %s, want 30m", DefaultHTTPRequestLimit)
	}
	if DefaultHTTPDownloadLimit <= DefaultHTTPRequestLimit {
		t.Fatalf("DefaultHTTPDownloadLimit = %s, want more than metadata limit %s",
			DefaultHTTPDownloadLimit, DefaultHTTPRequestLimit)
	}
}

func TestUpdaterDownloadTimeoutFallsBackToDownloadLimit(t *testing.T) {
	updater, err := NewUpdater(UpdaterConfig{}, nil)
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	// NewUpdater normalizes an unset HTTPTimeout to the metadata default, so
	// the download budget must not silently inherit it.
	if got := updater.downloadTimeout(); got != DefaultHTTPDownloadLimit {
		t.Fatalf("downloadTimeout() = %s, want %s", got, DefaultHTTPDownloadLimit)
	}
	if got := updater.httpTimeout(); got != DefaultHTTPRequestLimit {
		t.Fatalf("httpTimeout() = %s, want %s", got, DefaultHTTPRequestLimit)
	}
}

func TestUpdaterExplicitHTTPTimeoutAppliesToDownloads(t *testing.T) {
	updater, err := NewUpdater(UpdaterConfig{HTTPTimeout: 90 * time.Second}, nil)
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	if got := updater.downloadTimeout(); got != 90*time.Second {
		t.Fatalf("downloadTimeout() = %s, want 90s", got)
	}
	if got := updater.httpTimeout(); got != 90*time.Second {
		t.Fatalf("httpTimeout() = %s, want 90s", got)
	}
}

func TestUpdaterDownloadTimeoutHonorsSecondsFields(t *testing.T) {
	// http_timeout_seconds alone must still govern downloads.
	updater, err := NewUpdater(UpdaterConfig{HTTPTimeoutSecs: 45}, nil)
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	if got := updater.downloadTimeout(); got != 45*time.Second {
		t.Fatalf("downloadTimeout() = %s, want 45s", got)
	}

	// download_timeout_seconds wins over http_timeout_seconds for downloads.
	updater, err = NewUpdater(UpdaterConfig{HTTPTimeoutSecs: 45, DownloadTimeoutSecs: 600}, nil)
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	if got := updater.downloadTimeout(); got != 600*time.Second {
		t.Fatalf("downloadTimeout() = %s, want 600s", got)
	}
	if got := updater.httpTimeout(); got != 45*time.Second {
		t.Fatalf("httpTimeout() = %s, want 45s", got)
	}
}

func TestDescribeTimeoutNamesTheBudget(t *testing.T) {
	err := describeTimeout(context.Background(), context.DeadlineExceeded, "download of rootfs.img", time.Hour)
	if err == nil {
		t.Fatal("describeTimeout() = nil, want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("describeTimeout() lost DeadlineExceeded: %v", err)
	}
	for _, want := range []string{"timed out", "download of rootfs.img", "1h0m0s"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("describeTimeout() = %q, want it to contain %q", err, want)
		}
	}
}

func TestDescribeTimeoutLeavesNonTimeoutErrorsAlone(t *testing.T) {
	sentinel := errors.New("download status 404")
	if got := describeTimeout(context.Background(), sentinel, "download", time.Hour); got != sentinel {
		t.Fatalf("describeTimeout() = %v, want the original error", got)
	}
	if got := describeTimeout(context.Background(), nil, "download", time.Hour); got != nil {
		t.Fatalf("describeTimeout(nil) = %v, want nil", got)
	}
}

func TestDescribeTimeoutDoesNotBlameTimeoutOnCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	// A shutdown that cancels the parent surfaces DeadlineExceeded from the
	// child context, but calling that a timeout would send the reader off to
	// raise a limit that was never reached.
	err := describeTimeout(parent, context.DeadlineExceeded, "download", time.Hour)
	if strings.Contains(err.Error(), "timed out") {
		t.Fatalf("describeTimeout() = %q, want no timeout claim for a cancelled parent", err)
	}
}

func TestDefaultHTTPResponseHeaderTimeoutIsShort(t *testing.T) {
	if DefaultHTTPResponseHeaderTimeout != 30*time.Second {
		t.Fatalf("DefaultHTTPResponseHeaderTimeout = %s, want 30s", DefaultHTTPResponseHeaderTimeout)
	}
}

func TestOTAHTTPClientUsesDefaultResponseHeaderTimeout(t *testing.T) {
	transport, ok := newOTAHTTPClient().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("newOTAHTTPClient transport = %T, want *http.Transport", newOTAHTTPClient().Transport)
	}
	if transport.ResponseHeaderTimeout != DefaultHTTPResponseHeaderTimeout {
		t.Fatalf("ResponseHeaderTimeout = %s, want %s", transport.ResponseHeaderTimeout, DefaultHTTPResponseHeaderTimeout)
	}
}

func TestUpdaterUsesPerRequestHTTPTimeout(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.config.HTTPTimeout = 120 * time.Millisecond
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}, nil)
	assets := map[string][]byte{
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			var release struct {
				Assets []githubAsset `json:"assets"`
			}
			release.Assets = append(release.Assets, githubAsset{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"})
			for name := range assets {
				release.Assets = append(release.Assets, githubAsset{Name: name, BrowserDownloadURL: "http://" + r.Host + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if b, ok := assets[name]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	if _, err := env.updater().CheckOnce(context.Background()); err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
}

func TestNewUpdaterNormalizesNonPositiveHTTPTimeout(t *testing.T) {
	updater, err := NewUpdater(UpdaterConfig{HTTPTimeout: -time.Second}, nil)
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	if updater.config.HTTPTimeout != DefaultHTTPRequestLimit {
		t.Fatalf("HTTPTimeout = %s, want %s", updater.config.HTTPTimeout, DefaultHTTPRequestLimit)
	}
}

func TestUpdaterNoUpdateReturnsNoop(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.LastCommittedVersion = env.version
	env.state.LastCommittedBuildTime = env.buildTime
	env.saveState(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.NoUpdate || result.Updated {
		t.Fatalf("CheckOnce() = %+v, want no update", result)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterCheckOnceRejectsConcurrentUpdateLock(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.LastCommittedVersion = env.version
	env.state.LastCommittedBuildTime = env.buildTime
	env.saveState(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	lockFile, err := os.OpenFile(filepath.Join(env.stateDir, "update.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(lock) error = %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	})
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("Flock(lock) error = %v", err)
	}

	_, err = env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ota update already running") {
		t.Fatalf("CheckOnce() error = %v, want ota update already running", err)
	}
}

func TestProcessPendingHealthOnceDoesNotCheckForUpdates(t *testing.T) {
	env := newUpdaterTestEnv(t)
	requests := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requests <- r.URL.Path:
		default:
		}
		http.Error(w, "unexpected automatic OTA check", http.StatusTeapot)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	select {
	case err := <-processPendingHealthOnceAsync(env.updater(), context.Background()):
		if err != nil {
			t.Fatalf("ProcessPendingHealthOnce() error = %v", err)
		}
	case path := <-requests:
		t.Fatalf("ProcessPendingHealthOnce made automatic OTA request to %s", path)
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessPendingHealthOnce did not exit after one health pass")
	}
}

func processPendingHealthOnceAsync(updater *Updater, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- updater.ProcessPendingHealthOnce(ctx)
	}()
	return done
}

func TestProcessPendingHealthOnceReturnsContextErrorWhenHealthWaitIsCanceled(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "a", TargetVersion: "v2", TargetBuildTime: "2026-05-21T12:00:00Z", Nonce: "nonce"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	env.config.HealthTimeout = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := processPendingHealthOnceAsync(env.updater(), ctx)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessPendingHealthOnce() error = %v, want context canceled", err)
	}
}

func TestUpdaterSkipsAssetDownloadWhenCachedFileMatchesSHA256(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assetBytes := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	manifest := env.signedManifest(assetBytes, nil)
	if err := os.MkdirAll(env.downloadDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(downloadDir) error = %v", err)
	}
	for _, name := range []string{"boot_b.img", "rootfs.img"} {
		if err := os.WriteFile(filepath.Join(env.downloadDir, name), assetBytes[name], 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}

	assetRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			var release struct {
				Assets []githubAsset `json:"assets"`
			}
			release.Assets = append(release.Assets, githubAsset{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"})
			for _, name := range []string{"boot_b.img", "rootfs.img"} {
				release.Assets = append(release.Assets, githubAsset{Name: name, BrowserDownloadURL: "http://" + r.Host + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			assetRequests++
			http.Error(w, "asset download should be skipped", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v", result)
	}
	if assetRequests != 0 {
		t.Fatalf("assetRequests = %d, want 0", assetRequests)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "boot-b-v2")
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "rootfs-v2")
}

func TestUpdaterSkipsDownloadAndWriteWhenTargetPartitionHashMatches(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assetBytes := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	rootfsHash := testSHA256Hex(assetBytes["rootfs.img"])
	if err := os.WriteFile(filepath.Join(env.blockDir, "rootfs_b"), assetBytes["rootfs.img"], 0o644); err != nil {
		t.Fatalf("WriteFile(rootfs_b) error = %v", err)
	}
	env.state.Slots["b"] = SlotPartitionInfo{Partitions: map[string]PartitionVersion{
		"boot":   {Version: "factory", Hash: testHashA},
		"rootfs": {Version: "previous", Hash: rootfsHash},
	}}
	env.saveState(t)
	manifest := env.signedManifest(assetBytes, nil)

	rootfsRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			var release struct {
				Assets []githubAsset `json:"assets"`
			}
			release.Assets = append(release.Assets, githubAsset{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"})
			for _, name := range []string{"boot_b.img", "rootfs.img"} {
				release.Assets = append(release.Assets, githubAsset{Name: name, BrowserDownloadURL: "http://" + r.Host + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if name == "rootfs.img" {
			rootfsRequests++
			http.Error(w, "matching partition should not be downloaded", http.StatusInternalServerError)
			return
		}
		if b, ok := assetBytes[name]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v", result)
	}
	if rootfsRequests != 0 {
		t.Fatalf("rootfsRequests = %d, want 0", rootfsRequests)
	}
	if _, err := os.Stat(filepath.Join(env.downloadDir, "rootfs.img")); !os.IsNotExist(err) {
		t.Fatalf("rootfs.img cache exists after partition skip: %v", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "boot-b-v2")
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "rootfs-v2")

	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if got := state.DownloadedHashes["rootfs"]; got != rootfsHash {
		t.Fatalf("DownloadedHashes[rootfs] = %s, want %s", got, rootfsHash)
	}
	pendingBytes, err := os.ReadFile(filepath.Join(env.stateDir, "pending_boot.json"))
	if err != nil {
		t.Fatalf("ReadFile(pending_boot.json) error = %v", err)
	}
	var pending PendingBoot
	if err := json.Unmarshal(pendingBytes, &pending); err != nil {
		t.Fatalf("Unmarshal(PendingBoot) error = %v", err)
	}
	marker := HealthMarker{Slot: "b", Version: env.version, BuildTime: env.buildTime, Nonce: pending.Nonce, BootID: currentBootID()}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("Marshal(HealthMarker) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.stateDir, "health.ok"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(health.ok) error = %v", err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotB, true, nil }
	if err := updater.ProcessPendingHealth(context.Background()); err != nil {
		t.Fatalf("ProcessPendingHealth() error = %v", err)
	}
	state, err = LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState(committed) error = %v", err)
	}
	if got := state.Slots["b"].Partitions["rootfs"]; got.Version != env.version || got.Hash != rootfsHash {
		t.Fatalf("committed rootfs partition = %+v, want %s/%s", got, env.version, rootfsHash)
	}
}

func TestUpdaterInitializesMissingStateFromFactoryConfig(t *testing.T) {
	env := newUpdaterTestEnv(t)
	statePath := filepath.Join(env.stateDir, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("Remove(state.json) error = %v", err)
	}
	env.config.FactoryVersion = "factory-1"
	env.config.FactoryBuildTime = "2026-05-21T10:00:00Z"
	env.config.FactoryPartitionHashes = map[string]map[string]string{
		"a": {"boot": testHashA, "rootfs": testHashC},
		"b": {"boot": testHashB, "rootfs": testHashA},
	}

	state, err := env.updater().loadState()
	if err != nil {
		t.Fatalf("loadState() error = %v", err)
	}
	if state.LastCommittedVersion != env.config.FactoryVersion || state.LastCommittedBuildTime != env.config.FactoryBuildTime {
		t.Fatalf("factory state version/build_time = %q/%q", state.LastCommittedVersion, state.LastCommittedBuildTime)
	}
	for _, slot := range []string{"a", "b"} {
		for part, wantHash := range env.config.FactoryPartitionHashes[slot] {
			got := state.Slots[slot].Partitions[part]
			if got.Version != env.config.FactoryVersion || got.Hash != wantHash {
				t.Fatalf("state.Slots[%s].Partitions[%s] = %+v, want hash %s", slot, part, got, wantHash)
			}
		}
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("factory state was not persisted: %v", err)
	}
}

func TestUpdaterRejectsDowngradeOnFreshDeviceWithFactoryConfig(t *testing.T) {
	env := newUpdaterTestEnv(t)
	if err := os.Remove(filepath.Join(env.stateDir, "state.json")); err != nil {
		t.Fatalf("Remove(state.json) error = %v", err)
	}
	env.config.FactoryVersion = "factory-2"
	env.config.FactoryBuildTime = "2026-05-21T12:00:00Z"
	env.config.FactoryPartitionHashes = uniformFactoryPartitionHashes(testHashA)
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v1"),
		"boot_b.img": []byte("boot-b-v1"),
		"rootfs.img": []byte("rootfs-v1"),
	}, func(m *Manifest) {
		m.Version = "factory-1"
		m.BuildTime = "2026-05-21T11:00:00Z"
	})
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img": []byte("boot-b-v1"),
		"rootfs.img": []byte("rootfs-v1"),
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reject downgrade") {
		t.Fatalf("CheckOnce() error = %v, want reject downgrade", err)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestNewUpdaterRejectsInvalidConfig(t *testing.T) {
	updater, err := NewUpdater(UpdaterConfig{SwitchTries: MaxTries + 1}, nil)
	if err == nil {
		t.Fatalf("NewUpdater() error = nil, want switch_tries validation error")
	}
	if updater != nil {
		t.Fatalf("NewUpdater() updater = %#v, want nil", updater)
	}
}

func TestUpdaterRejectsOversizedRemoteManifest(t *testing.T) {
	env := newUpdaterTestEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", MaxRemoteManifestBytes+1)))
	}))
	t.Cleanup(server.Close)

	_, err := env.updater().VerifyManifestFile(server.URL)
	if err == nil || !strings.Contains(err.Error(), "manifest too large") {
		t.Fatalf("VerifyManifestFile() error = %v, want manifest too large", err)
	}
}

func TestCurrentSlotFromCmdlineReadsAicoSlotSuffix(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdline string
		want    Slot
		wantOK  bool
		wantErr string
	}{
		{name: "slot a", cmdline: "console=ttyFIQ0 aico.slot_suffix=_a root=/dev/mmcblk0", want: SlotA, wantOK: true},
		{name: "slot b", cmdline: "rootwait aico.slot_suffix=_b", want: SlotB, wantOK: true},
		{name: "missing", cmdline: "console=ttyFIQ0", wantOK: false},
		{name: "invalid", cmdline: "aico.slot_suffix=_c", wantErr: "invalid slot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := currentSlotFromCmdline(tc.cmdline)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("currentSlotFromCmdline() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("currentSlotFromCmdline() error = %v", err)
			}
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("currentSlotFromCmdline() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRootSlotFromCmdlineReadsRunningRootfs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cmdline string
		want    Slot
		wantOK  bool
		wantErr string
	}{
		{name: "partlabel a", cmdline: "rootwait root=PARTLABEL=rootfs_a", want: SlotA, wantOK: true},
		{name: "partlabel b", cmdline: "root=PARTLABEL=rootfs_b aico.slot_suffix=_b", want: SlotB, wantOK: true},
		{name: "by name a", cmdline: "root=/dev/block/by-name/rootfs_a", want: SlotA, wantOK: true},
		{name: "by name b", cmdline: "root=/dev/block/by-name/rootfs_b", want: SlotB, wantOK: true},
		{name: "numbered p7 a", cmdline: "aico.slot_suffix=_b root=/dev/mmcblk0p7", want: SlotA, wantOK: true},
		{name: "numbered p8 b", cmdline: "root=/dev/mmcblk0p8", want: SlotB, wantOK: true},
		{name: "missing", cmdline: "console=ttyFIQ0", wantOK: false},
		{name: "unsupported", cmdline: "root=/dev/mmcblk0p2", wantErr: "unsupported root device"},
		{name: "old rootfs a is now userdata", cmdline: "root=/dev/mmcblk0p9", wantErr: "unsupported root device"},
		{name: "old rootfs b is now ota", cmdline: "root=/dev/mmcblk0p10", wantErr: "unsupported root device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := rootSlotFromCmdline(tc.cmdline)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("rootSlotFromCmdline() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("rootSlotFromCmdline() error = %v", err)
			}
			if ok != tc.wantOK || (ok && got != tc.want) {
				t.Fatalf("rootSlotFromCmdline() = %v, %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestUpdaterRejectsOversizedAssetWithDefaultPartitionSizes(t *testing.T) {
	env := newUpdaterTestEnv(t)
	bootBody := []byte("boot-b-v2")
	assets := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": bootBody,
		"rootfs.img": []byte("rootfs-v2"),
	}
	manifest := env.signedManifest(assets, func(m *Manifest) {
		m.Parts[0].AssetB.Size = DefaultBootPartitionSize + 1
	})
	assetDownloads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			release := struct {
				Assets []githubAsset `json:"assets"`
			}{Assets: []githubAsset{
				{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"},
				{Name: "boot_b.img", BrowserDownloadURL: "http://" + r.Host + "/assets/boot_b.img"},
			}}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		if r.URL.Path == "/assets/boot_b.img" {
			assetDownloads++
			_, _ = w.Write(bootBody)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than partition") {
		t.Fatalf("CheckOnce() error = %v, want default partition size rejection", err)
	}
	if assetDownloads != 0 {
		t.Fatalf("assetDownloads = %d, want 0", assetDownloads)
	}
	if _, err := os.Stat(filepath.Join(env.downloadDir, "boot_b.img")); !os.IsNotExist(err) {
		t.Fatalf("download file exists after pre-download rejection: %v", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterRejectsBadSignature(t *testing.T) {
	env := newUpdaterTestEnv(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	manifest = []byte(strings.Replace(string(manifest), env.version, "20260521-130000-tamper", 1))
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("CheckOnce() error = %v, want signature failure", err)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterRejectsHashMismatchBeforeWriting(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assets := map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}
	manifest := env.signedManifest(assets, func(m *Manifest) { m.Parts[0].AssetB.SHA256 = testHashC })
	server := env.releaseServer(t, manifest, assets)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("CheckOnce() error = %v, want hash failure", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterUsesGitHubTokenForManifestAndImageDownloads(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.config.GitHubToken = "secret-token"
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.authReleaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, "Bearer secret-token")
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated {
		t.Fatalf("CheckOnce() = %+v, want update", result)
	}
}

func TestUpdaterLogsVisibleCheckProgress(t *testing.T) {
	env := newUpdaterTestEnv(t)
	var logs bytes.Buffer
	restoreOutput := logging.SetOutput(&logs)
	defer restoreOutput()
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated {
		t.Fatalf("CheckOnce() = %+v, want update", result)
	}

	output := logs.String()
	for _, want := range []string{
		"ota check: start",
		"ota release: fetching",
		"ota manifest: verified version=20260521-120000-abcdef0",
		"ota download: boot_b.img start size=9 B",
		"ota download: boot_b.img complete 9 B/9 B",
		"ota verify: boot_b.img sha256 ok",
		"ota write: boot -> boot_b start",
		"ota write: boot -> boot_b complete",
		"ota reboot: requested after switching to slot b",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("logs missing %q:\n%s", want, output)
		}
	}
	// Progress is emitted at explicit severity through the shared logging
	// output, so every captured line carries the structured record prefix.
	if !strings.Contains(output, "[INFO][ota][updater] log_message message=") {
		t.Fatalf("logs missing explicit INFO/ota/updater record prefix:\n%s", output)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if !logging.IsStructuredLine(line) {
			t.Fatalf("log line %q is not a structured record:\n%s", line, output)
		}
	}
}

func TestUpdaterRejectsStaleTargetSlotForSelectiveUpdate(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.Slots["b"] = SlotPartitionInfo{Partitions: map[string]PartitionVersion{
		"boot":   {Version: "factory", Hash: testHashA},
		"rootfs": {Version: "older", Hash: testHashB},
	}}
	env.saveState(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2")}, func(m *Manifest) {
		m.Parts = []ManifestPart{m.Parts[0]}
		m.Parts[0].RequiresPartitions = []string{"rootfs=factory:" + testHashA}
	})
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "selective update") {
		t.Fatalf("CheckOnce() error = %v, want selective update failure", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
}

func TestUpdaterInvalidatesTargetSlotMetadataBeforePartialWriteFailure(t *testing.T) {
	env := newUpdaterTestEnv(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	if err := os.Remove(filepath.Join(env.blockDir, "rootfs_b")); err != nil {
		t.Fatalf("Remove(rootfs_b) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(env.blockDir, "rootfs_b"), 0o755); err != nil {
		t.Fatalf("Mkdir(rootfs_b) error = %v", err)
	}

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil {
		t.Fatalf("CheckOnce() error = nil, want write failure")
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if got := ab.Slots[SlotB]; got != (SlotData{Priority: 0, TriesRemaining: 0, SuccessfulBoot: false}) {
		t.Fatalf("misc target slot after interrupted write = %+v, want unbootable", got)
	}
	if ab.Bootable(SlotB) {
		t.Fatalf("misc target slot is bootable after interrupted write: %+v", ab.Slots[SlotB])
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	selective := env.manifestValue(map[string][]byte{"boot_a.img": []byte("boot-a-v3"), "boot_b.img": []byte("boot-b-v3")}, func(m *Manifest) {
		m.Version = "20260521-130000-abcdef1"
		m.BuildTime = "2026-05-21T13:00:00Z"
		m.Parts = []ManifestPart{m.Parts[0]}
		m.Parts[0].RequiresPartitions = []string{"rootfs=factory:" + testHashA}
	})
	if err := state.ValidateSelectiveUpdate(selective, SlotB); err == nil {
		t.Fatalf("ValidateSelectiveUpdate() error = nil, want invalidated target slot metadata")
	}
}

func TestUpdaterRejectsActiveSlotWrite(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.config.TargetSlotOverride = "a"
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active slot") {
		t.Fatalf("CheckOnce() error = %v, want active slot write rejection", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_a"), "old-boot-a")
}

func TestUpdaterUsesRunningSlotToProtectWritesWhenMiscPrefersOtherSlot(t *testing.T) {
	env := newUpdaterTestEnv(t)
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if err := ab.SetActive(SlotB, 2, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	env.config.TargetSlotOverride = "a"
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, true, nil }

	_, err = updater.CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "active slot") {
		t.Fatalf("CheckOnce() error = %v, want running active slot write rejection", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_a"), "old-boot-a")
}

func TestUpdaterDryRunDoesNotWritePartitionsOrSwitchMisc(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.config.DryRun = true
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v", result)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "old-rootfs-b")
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	active, ok := ab.ActiveSlot()
	if !ok || active != SlotA {
		t.Fatalf("active slot after dry-run = %v ok=%v, want A", active, ok)
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_boot.json exists after dry-run: %v", err)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

func TestUpdaterRejectsSameBuildTimeDifferentVersion(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.LastCommittedVersion = "20260521-110000-older"
	env.state.LastCommittedBuildTime = env.buildTime
	env.saveState(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("CheckOnce() error = %v, want downgrade rejection", err)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "old-boot-b")
}

func TestUpdaterProcessesPendingHealthTimeoutAndReboots(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if err := ab.SetActive(SlotB, 2, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	env.config.HealthTimeout = time.Millisecond
	env.config.HealthPollInterval = time.Millisecond

	err = env.updater().ProcessPendingHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health timeout") {
		t.Fatalf("ProcessPendingHealth() error = %v, want timeout", err)
	}
	if env.reboots != 1 {
		t.Fatalf("reboots = %d, want 1", env.reboots)
	}
}

func TestUpdaterDoesNotClearPendingHealthWhenRunningSlotUnknown(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	env.config.HealthTimeout = time.Millisecond
	env.config.HealthPollInterval = time.Millisecond
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, false, nil }

	err := updater.ProcessPendingHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "health timeout") {
		t.Fatalf("ProcessPendingHealth() error = %v, want timeout", err)
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); err != nil {
		t.Fatalf("pending_boot.json missing after unknown running slot: %v", err)
	}
}

func TestUpdaterCommitsPendingHealthWhenMiscPrefersOldSlotButRunningTarget(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	// Simulate the final try after SPL has decremented B to zero: misc now prefers
	// old slot A, but Linux is still running the just-booted target slot B.
	ab.Slots[SlotA] = SlotData{Priority: MaxPriority - 1, SuccessfulBoot: true}
	ab.Slots[SlotB] = SlotData{Priority: MaxPriority}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	env.state.TargetVersion = env.version
	env.state.TargetBuildTime = env.buildTime
	env.state.TargetSlot = SlotB
	env.state.DownloadedHashes = map[string]string{"boot": testHashB}
	env.saveState(t)
	marker := HealthMarker{Slot: "b", Version: env.version, BuildTime: env.buildTime, Nonce: "nonce-1", BootID: currentBootID()}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("Marshal(HealthMarker) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.stateDir, "health.ok"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(health.ok) error = %v", err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotB, true, nil }

	if err := updater.ProcessPendingHealth(context.Background()); err != nil {
		t.Fatalf("ProcessPendingHealth() error = %v", err)
	}
	ab, err = readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if !ab.Slots[SlotB].SuccessfulBoot || ab.Slots[SlotB].TriesRemaining != 0 {
		t.Fatalf("slot B after health = %+v, want successful with zero tries", ab.Slots[SlotB])
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_boot.json still exists after commit: %v", err)
	}
}

func TestUpdaterClearsPendingHealthAfterRollbackToOldSlot(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	env.state.Phase = "pending-reboot"
	env.state.TargetVersion = env.version
	env.state.TargetBuildTime = env.buildTime
	env.state.TargetSlot = SlotB
	env.state.PendingBootNonce = pending.Nonce
	env.state.PendingBootID = "boot-before-rollback"
	env.state.PendingTargetSlot = &SlotPartitionInfo{Partitions: map[string]PartitionVersion{"boot": {Version: "previous", Hash: testHashA}}}
	env.saveState(t)
	env.config.HealthTimeout = time.Millisecond
	env.config.HealthPollInterval = time.Millisecond
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, true, nil }

	if err := updater.ProcessPendingHealth(context.Background()); err != nil {
		t.Fatalf("ProcessPendingHealth() error = %v", err)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_boot.json still exists after rollback: %v", err)
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.Phase != "rolled-back" || state.TargetVersion != "" || state.TargetBuildTime != "" || state.PendingBootNonce != "" || state.PendingBootID != "" || state.PendingTargetSlot != nil {
		t.Fatalf("state after rollback = %+v, want pending fields cleared", state)
	}
	if state.ActiveSlot != SlotA || state.TargetSlot != SlotA {
		t.Fatalf("state slots after rollback active=%d target=%d, want A/A", state.ActiveSlot, state.TargetSlot)
	}
}

func TestRecoverPendingDataReconcilesCompletedRollback(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.Phase = "rollback-requested"
	env.state.ActiveSlot = SlotB
	env.state.TargetSlot = SlotA
	env.state.CurrentVersion = env.version
	env.state.CurrentBuildTime = env.buildTime
	env.state.Slots["a"] = SlotPartitionInfo{Partitions: map[string]PartitionVersion{
		"boot": {Version: "old-version", Hash: testHashA},
	}}
	env.state.SlotBuildTimes = map[string]string{"a": "old-build", "b": env.buildTime}
	env.saveState(t)
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, true, nil }
	if err := updater.RecoverPendingData(); err != nil {
		t.Fatalf("RecoverPendingData() error = %v", err)
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "rolled-back" || state.ActiveSlot != SlotA || state.TargetSlot != SlotA || state.CurrentVersion != "old-version" || state.CurrentBuildTime != "old-build" {
		t.Fatalf("reconciled state = %+v", state)
	}
}

func TestRollbackRestoreFailureKeepsSnapshotAndPendingState(t *testing.T) {
	env := newUpdaterTestEnv(t)
	snapshot, dataRoot, files := makeRestoreFixture(t)
	defer setProtectedDataRoot(dataRoot)()
	if err := os.Remove(filepath.Join(snapshot, "userdata", files[2])); err != nil {
		t.Fatal(err)
	}
	env.state.Phase = "pending-reboot"
	env.state.TargetSlot = SlotB
	env.state.DataSnapshotPath = snapshot
	env.saveState(t)
	pendingPath := filepath.Join(env.stateDir, "pending_boot.json")
	if err := WritePendingBoot(pendingPath, PendingBoot{TargetSlot: "b"}); err != nil {
		t.Fatal(err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, true, nil }
	for _, recover := range []func() error{
		updater.RecoverPendingData,
		func() error { return updater.ProcessPendingHealth(context.Background()) },
	} {
		if err := recover(); err == nil {
			t.Fatal("incomplete snapshot recovery succeeded")
		}
		state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if state.DataSnapshotPath != snapshot || state.Phase != "pending-reboot" {
			t.Fatalf("lost recovery state: %+v", state)
		}
		if _, err := os.Stat(pendingPath); err != nil {
			t.Fatalf("lost pending boot: %v", err)
		}
		for _, rel := range files {
			assertFileContent(t, filepath.Join(dataRoot, rel), "new:"+rel)
		}
	}
}

func TestSaveSnapshotStateFailureCleansOnlyUnpublishedSnapshot(t *testing.T) {
	for _, afterRename := range []bool{false, true} {
		t.Run(fmt.Sprintf("after-rename=%t", afterRename), func(t *testing.T) {
			env := newUpdaterTestEnv(t)
			snapshot := filepath.Join(env.stateDir, "transactions/pre-test-v2")
			writeSnapshotTestFile(t, filepath.Join(snapshot, "manifest.json"), `{"files":[]}`)
			env.state.DataSnapshotPath = snapshot
			if afterRename {
				original := syncDir
				syncDir = func(*os.File) error { return errors.New("sync failed") }
				t.Cleanup(func() { syncDir = original })
			} else {
				if err := os.Mkdir(filepath.Join(env.stateDir, "state.json.tmp"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := env.updater().saveSnapshotState(env.state); err == nil {
				t.Fatal("state write unexpectedly succeeded")
			}
			_, err := os.Stat(snapshot)
			if afterRename && err != nil {
				t.Fatalf("published snapshot lost after fsync failure: %v", err)
			}
			if !afterRename && !os.IsNotExist(err) {
				t.Fatalf("unpublished snapshot not removed: %v", err)
			}
		})
	}
}

func TestSaveSnapshotStateWithoutSnapshotTouchesNoDirectory(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.DataSnapshotPath = ""
	// No state file on disk, so the read-back reports os.IsNotExist: the branch
	// that would otherwise clean up an unpublished snapshot.
	if err := os.Remove(filepath.Join(env.stateDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	// Force the state write to fail after the temporary file is opened.
	if err := os.Mkdir(filepath.Join(env.stateDir, "state.json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	var synced []string
	original := syncDir
	syncDir = func(f *os.File) error {
		synced = append(synced, f.Name())
		return original(f)
	}
	t.Cleanup(func() { syncDir = original })

	if err := env.updater().saveSnapshotState(env.state); err == nil {
		t.Fatal("state write unexpectedly succeeded")
	}
	for _, path := range synced {
		if path == "." {
			t.Fatalf("empty snapshot path fsynced the working directory: %v", synced)
		}
	}
}

func TestRecoverPendingDataReconcilesAbandonedRollback(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.Phase = "rollback-requested"
	env.state.ActiveSlot = SlotB
	env.state.TargetSlot = SlotA
	env.state.CurrentVersion = env.version
	env.state.Slots["b"] = SlotPartitionInfo{Partitions: map[string]PartitionVersion{
		"boot": {Version: "current-version", Hash: testHashB},
	}}
	env.saveState(t)
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ab.SetActive(SlotB, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatal(err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotB, true, nil }
	if err := updater.RecoverPendingData(); err != nil {
		t.Fatalf("RecoverPendingData() error = %v", err)
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "committed" || state.ActiveSlot != SlotB || state.TargetSlot != SlotB || state.CurrentVersion != "current-version" {
		t.Fatalf("reconciled abandoned state = %+v", state)
	}
}

func TestUpdaterDoesNotClearPendingWhenMiscStillPrefersTargetButRunningOldSlot(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if err := ab.SetActive(SlotB, 2, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotA, true, nil }

	err = updater.ProcessPendingHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pending target slot") {
		t.Fatalf("ProcessPendingHealth() error = %v, want pending target mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); err != nil {
		t.Fatalf("pending_boot.json missing after reboot mismatch: %v", err)
	}
}

func TestUpdaterSelectiveRootFSCommitPreservesCompatibleOmittedTargetPartitions(t *testing.T) {
	env := newUpdaterTestEnv(t)
	env.state.Slots["b"] = SlotPartitionInfo{Partitions: map[string]PartitionVersion{
		"boot":   {Version: "factory", Hash: testHashA},
		"rootfs": {Version: "factory", Hash: testHashA},
	}}
	env.saveState(t)
	manifest := env.signedManifest(map[string][]byte{"rootfs.img": []byte("rootfs-b-v2")}, func(m *Manifest) {
		m.Parts = []ManifestPart{m.Parts[0]}
		m.Parts[0].RequiresPartitions = []string{"boot=factory:" + testHashA}
	})
	server := env.releaseServer(t, manifest, map[string][]byte{"rootfs.img": []byte("rootfs-b-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated || result.TargetSlot != SlotB {
		t.Fatalf("CheckOnce() = %+v, want update to slot B", result)
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if err := ab.SetActive(SlotB, 1, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime}
	pendingBytes, err := os.ReadFile(filepath.Join(env.stateDir, "pending_boot.json"))
	if err != nil {
		t.Fatalf("ReadFile(pending_boot.json) error = %v", err)
	}
	if err := json.Unmarshal(pendingBytes, &pending); err != nil {
		t.Fatalf("Unmarshal(pending) error = %v", err)
	}
	marker := HealthMarker{Slot: "b", Version: env.version, BuildTime: env.buildTime, Nonce: pending.Nonce, BootID: currentBootID()}
	markerBytes, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("Marshal(HealthMarker) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.stateDir, "health.ok"), markerBytes, 0o644); err != nil {
		t.Fatalf("WriteFile(health.ok) error = %v", err)
	}
	updater := env.updater()
	updater.currentSlot = func() (Slot, bool, error) { return SlotB, true, nil }

	if err := updater.ProcessPendingHealth(context.Background()); err != nil {
		t.Fatalf("ProcessPendingHealth() error = %v", err)
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	parts := state.Slots["b"].Partitions
	if got := parts["boot"]; got.Version != "factory" || got.Hash != testHashA {
		t.Fatalf("boot partition = %+v, want preserved factory", got)
	}
	rootfsHash := sha256.Sum256([]byte("rootfs-b-v2"))
	if got := parts["rootfs"]; got.Version != env.version || got.Hash != hex.EncodeToString(rootfsHash[:]) {
		t.Fatalf("rootfs partition = %+v, want committed update", got)
	}
}

func TestUpdaterProcessesPendingHealthSuccessDuringWindow(t *testing.T) {
	env := newUpdaterTestEnv(t)
	pending := PendingBoot{TargetSlot: "b", TargetVersion: env.version, TargetBuildTime: env.buildTime, Nonce: "nonce-1"}
	if err := WritePendingBoot(filepath.Join(env.stateDir, "pending_boot.json"), pending); err != nil {
		t.Fatalf("WritePendingBoot() error = %v", err)
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if err := ab.SetActive(SlotB, 2, false); err != nil {
		t.Fatalf("SetActive() error = %v", err)
	}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}
	env.state.TargetVersion = env.version
	env.state.TargetBuildTime = env.buildTime
	env.state.TargetSlot = SlotB
	env.state.DownloadedHashes = map[string]string{"boot": testHashA, "rootfs": testHashB}
	env.saveState(t)
	env.config.HealthTimeout = 200 * time.Millisecond
	env.config.HealthPollInterval = time.Millisecond

	go func() {
		time.Sleep(10 * time.Millisecond)
		marker := HealthMarker{Slot: "b", Version: env.version, BuildTime: env.buildTime, Nonce: "nonce-1", BootID: currentBootID()}
		data, _ := json.Marshal(marker)
		_ = os.WriteFile(filepath.Join(env.stateDir, "health.ok"), data, 0o644)
	}()

	if err := env.updater().ProcessPendingHealth(context.Background()); err != nil {
		t.Fatalf("ProcessPendingHealth() error = %v", err)
	}
	ab, err = readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	if !ab.Slots[SlotB].SuccessfulBoot || ab.Slots[SlotB].TriesRemaining != 0 {
		t.Fatalf("slot B after health = %+v, want successful with zero tries", ab.Slots[SlotB])
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_boot.json still exists: %v", err)
	}
	state, err := LoadState(filepath.Join(env.stateDir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if state.LastCommittedVersion != env.version || state.Slots["b"].Partitions["boot"].Hash != testHashA {
		t.Fatalf("state after health = %+v", state)
	}
}

func TestUpdaterCleansPendingBootWhenMiscSwitchFails(t *testing.T) {
	env := newUpdaterTestEnv(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("boot-a-v2"), "boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")}, nil)
	server := env.releaseServer(t, manifest, map[string][]byte{"boot_b.img": []byte("boot-b-v2"), "rootfs.img": []byte("rootfs-v2")})
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	updater := env.updater()
	updater.writeABData = func(ABData) error { return fmt.Errorf("forced misc write failure") }

	_, err := updater.CheckOnce(context.Background())
	if err == nil {
		t.Fatalf("CheckOnce() error = nil, want misc write failure")
	}
	if _, err := os.Stat(filepath.Join(env.stateDir, "pending_boot.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_boot.json still exists after misc failure: %v", err)
	}
	if env.reboots != 0 {
		t.Fatalf("reboots = %d, want 0", env.reboots)
	}
}

type updaterTestEnv struct {
	t           *testing.T
	stateDir    string
	downloadDir string
	blockDir    string
	miscPath    string
	pub         ed25519.PublicKey
	priv        ed25519.PrivateKey
	config      UpdaterConfig
	state       State
	version     string
	buildTime   string
	reboots     int
}

func newUpdaterTestEnv(t *testing.T) *updaterTestEnv {
	t.Helper()
	tmp := t.TempDir()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	env := &updaterTestEnv{
		t:           t,
		stateDir:    filepath.Join(tmp, "state"),
		downloadDir: filepath.Join(tmp, "downloads"),
		blockDir:    filepath.Join(tmp, "block"),
		miscPath:    filepath.Join(tmp, "misc.img"),
		pub:         pub,
		priv:        priv,
		version:     "20260521-120000-abcdef0",
		buildTime:   "2026-05-21T12:00:00Z",
	}
	if err := os.MkdirAll(env.blockDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(blockDir) error = %v", err)
	}
	for name, content := range map[string]string{
		"boot_a":   "old-boot-a",
		"boot_b":   "old-boot-b",
		"rootfs_a": "old-rootfs-a",
		"rootfs_b": "old-rootfs-b",
	} {
		if err := os.WriteFile(filepath.Join(env.blockDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	if err := CreateMiscImage(env.miscPath, DefaultMiscSize); err != nil {
		t.Fatalf("CreateMiscImage() error = %v", err)
	}
	env.state = NewFactoryState("factory", "2026-05-21T10:00:00Z", uniformFactoryPartitionHashes(testHashA))
	env.state.ActiveSlot = SlotA
	env.saveState(t)
	if err := os.WriteFile(filepath.Join(env.stateDir, "health.ok"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile(health.ok) error = %v", err)
	}
	storageDevicePath := filepath.Join(tmp, "ota-device")
	if err := os.WriteFile(storageDevicePath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(ota-device) error = %v", err)
	}
	mountInfoPath := filepath.Join(tmp, "mountinfo")
	mountInfo := fmt.Sprintf("36 25 179:12 / %s rw,relatime - ext4 %s rw\n", tmp, storageDevicePath)
	if err := os.WriteFile(mountInfoPath, []byte(mountInfo), 0o644); err != nil {
		t.Fatalf("WriteFile(mountinfo) error = %v", err)
	}
	env.config = UpdaterConfig{
		StateDir:                  env.stateDir,
		DownloadDir:               env.downloadDir,
		StorageMountPoint:         tmp,
		StorageDevicePath:         storageDevicePath,
		StorageFilesystem:         "ext4",
		MountInfoPath:             mountInfoPath,
		DownloadSafetyMarginBytes: 64,
		MiscPath:                  env.miscPath,
		BlockDir:                  env.blockDir,
		PublicKey:                 env.pub,
		SwitchTries:               3,
		HealthTimeout:             10 * time.Millisecond,
		HealthPollInterval:        time.Millisecond,
	}
	return env
}

func (e *updaterTestEnv) saveState(t *testing.T) {
	t.Helper()
	if err := SaveState(filepath.Join(e.stateDir, "state.json"), e.state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}
}

func (e *updaterTestEnv) updater() *Updater {
	updater, err := NewUpdater(e.config, func() error {
		e.reboots++
		return nil
	})
	if err != nil {
		e.t.Fatalf("NewUpdater() error = %v", err)
	}
	return updater
}

func (e *updaterTestEnv) signedManifest(assetBytes map[string][]byte, mutate func(*Manifest)) []byte {
	e.t.Helper()
	manifest := e.manifestValue(assetBytes, mutate)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		e.t.Fatalf("Marshal(manifest) error = %v", err)
	}
	return encoded
}

func (e *updaterTestEnv) manifestValue(assetBytes map[string][]byte, mutate func(*Manifest)) Manifest {
	e.t.Helper()
	asset := func(name string) *ManifestAsset {
		b, ok := assetBytes[name]
		if !ok {
			return nil
		}
		sum := sha256.Sum256(b)
		return &ManifestAsset{Name: name, Size: int64(len(b)), SHA256: hex.EncodeToString(sum[:])}
	}
	manifest := Manifest{
		SchemaVersion: 2,
		Channel:       "stable",
		Version:       e.version,
		BuildTime:     e.buildTime,
		Parts: []ManifestPart{
			{Name: "boot", AssetA: asset("boot_a.img"), AssetB: asset("boot_b.img")},
			{Name: "rootfs", Asset: asset("rootfs.img")},
		},
		Signature: ManifestSignature{Algorithm: "ed25519"},
	}
	filtered := manifest.Parts[:0]
	for _, part := range manifest.Parts {
		if part.Name == "boot" && (part.AssetA != nil || part.AssetB != nil) || part.Name != "boot" && (part.Asset != nil || part.AssetA != nil || part.AssetB != nil) {
			filtered = append(filtered, part)
		}
	}
	manifest.Parts = filtered
	if mutate != nil {
		mutate(&manifest)
	}
	canonical, err := CanonicalManifestJSON(manifest)
	if err != nil {
		e.t.Fatalf("CanonicalManifestJSON() error = %v", err)
	}
	manifest.Signature.Value = hex.EncodeToString(ed25519.Sign(e.priv, canonical))
	return manifest
}

func testManifestAsset(name string, body []byte) *ManifestAsset {
	return &ManifestAsset{Name: name, Size: int64(len(body)), SHA256: testSHA256Hex(body)}
}

func testCompressedManifestAsset(name string, archive []byte, image []byte) *ManifestAsset {
	asset := testManifestAsset(name, archive)
	asset.ImageSHA256 = testSHA256Hex(image)
	return asset
}

func testSHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func testTarGzImage(t *testing.T, imageName string, image []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: imageName, Mode: 0o644, Size: int64(len(image))}); err != nil {
		t.Fatalf("WriteHeader(%s) error = %v", imageName, err)
	}
	if _, err := tw.Write(image); err != nil {
		t.Fatalf("Write(%s) error = %v", imageName, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close(tar) error = %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("Close(gzip) error = %v", err)
	}
	return buf.Bytes()
}

func (e *updaterTestEnv) releaseServer(t *testing.T, manifest []byte, assets map[string][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			var release struct {
				Assets []githubAsset `json:"assets"`
			}
			release.Assets = append(release.Assets, githubAsset{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"})
			for name := range assets {
				release.Assets = append(release.Assets, githubAsset{Name: name, BrowserDownloadURL: "http://" + r.Host + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if b, ok := assets[name]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func (e *updaterTestEnv) authReleaseServer(t *testing.T, manifest []byte, assets map[string][]byte, wantAuth string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/repos/AidenAI-IO/aico-link/releases/latest") {
			var release struct {
				Assets []githubAsset `json:"assets"`
			}
			release.Assets = append(release.Assets, githubAsset{Name: "manifest.json", BrowserDownloadURL: "http://" + r.Host + "/assets/manifest.json"})
			for name := range assets {
				release.Assets = append(release.Assets, githubAsset{Name: name, BrowserDownloadURL: "http://" + r.Host + "/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if r.URL.Path == "/assets/manifest.json" {
			_, _ = w.Write(manifest)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if b, ok := assets[name]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func assertFileContent(t *testing.T, path string, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("ReadFile(%s) = %q, want %q", path, got, want)
	}
}

func readMiscFile(path string) (ABData, error) {
	f, err := os.Open(path)
	if err != nil {
		return ABData{}, err
	}
	defer f.Close()
	return ReadABData(f)
}

func writeMiscFile(path string, ab ABData) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return WriteABDataAt(f, ab)
}

func TestDeriveAssetURLFromManifestURL(t *testing.T) {
	tests := []struct {
		name        string
		manifestURL string
		assetName   string
		want        string
		wantErr     bool
	}{
		{
			name:        "GitHub release download URL",
			manifestURL: "https://github.com/owner/repo/releases/download/v1.2.3/manifest.json",
			assetName:   "boot_a.img",
			want:        "https://github.com/owner/repo/releases/download/v1.2.3/boot_a.img",
			wantErr:     false,
		},
		{
			name:        "Custom CDN with query params",
			manifestURL: "https://cdn.example.com/firmware/v1.0.0/manifest.json?token=abc",
			assetName:   "rootfs.img",
			want:        "https://cdn.example.com/firmware/v1.0.0/rootfs.img",
			wantErr:     false,
		},
		{
			name:        "Nested path",
			manifestURL: "https://example.com/releases/2024/06/manifest.json",
			assetName:   "rootfs_b.img",
			want:        "https://example.com/releases/2024/06/rootfs_b.img",
			wantErr:     false,
		},
		{
			name:        "URL with fragment",
			manifestURL: "https://example.com/path/manifest.json#section",
			assetName:   "boot_b.img",
			want:        "https://example.com/path/boot_b.img",
			wantErr:     false,
		},
		{
			name:        "Malformed URL",
			manifestURL: "://invalid",
			assetName:   "boot_a.img",
			want:        "",
			wantErr:     true,
		},
		{
			name:        "No directory component",
			manifestURL: "https://example.com/manifest.json",
			assetName:   "boot_a.img",
			want:        "https://example.com/boot_a.img",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deriveAssetURL(tt.manifestURL, tt.assetName)
			if (err != nil) != tt.wantErr {
				t.Errorf("deriveAssetURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("deriveAssetURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpdaterUsesManifestURLToDeriveAssetURLs(t *testing.T) {
	env := newUpdaterTestEnv(t)

	// Create manifest without asset URLs.
	// Set a dev channel to verify the channel check is skipped in manifest-url mode.
	manifest := env.signedManifest(map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}, func(m *Manifest) {
		m.Channel = "dev-feat-ota-open-sources"
	})

	// Serve manifest and assets from same base URL
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/20240604-abc123/manifest.json":
			w.Write(manifest)
		case "/releases/20240604-abc123/boot_b.img":
			w.Write([]byte("boot-b-v2"))
		case "/releases/20240604-abc123/rootfs.img":
			w.Write([]byte("rootfs-v2"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	// Use ManifestURL mode: a dev-channel manifest should install successfully
	// because there's no channel check when using direct manifest URLs.
	env.config.ManifestURL = server.URL + "/releases/20240604-abc123/manifest.json"

	result, err := env.updater().CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated {
		t.Fatalf("CheckOnce() Updated = false, want true")
	}

	// Verify assets were downloaded and written
	assertFileContent(t, filepath.Join(env.blockDir, "boot_b"), "boot-b-v2")
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "rootfs-v2")
}

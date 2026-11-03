package ota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestUpdaterRejectsNegativeDownloadSafetyMargin(t *testing.T) {
	storageDir := t.TempDir()
	_, err := NewUpdater(UpdaterConfig{
		StateDir:                  storageDir,
		StorageMountPoint:         storageDir,
		DownloadSafetyMarginBytes: -1,
	}, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "download_safety_margin_bytes must be non-negative") {
		t.Fatalf("NewUpdater() error = %v, want safety margin validation", err)
	}
}

func TestUpdaterRejectsStateDirectoryOutsideDedicatedStorageByDefault(t *testing.T) {
	_, err := NewUpdater(UpdaterConfig{StateDir: t.TempDir()}, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "state_dir must be inside the dedicated OTA storage mount") {
		t.Fatalf("NewUpdater() error = %v, want fixed dedicated storage validation", err)
	}
}

func TestUpdaterIgnoresStorageIdentityFromJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{
		"storage_mount_point":"/userdata",
		"storage_device_path":"/dev/block/by-name/userdata",
		"storage_filesystem":"xfs"
	}`)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	config, err := LoadUpdaterConfig(configPath)
	if err != nil {
		t.Fatalf("LoadUpdaterConfig() error = %v", err)
	}
	updater, err := NewUpdater(config, func() error { return nil })
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}
	if updater.config.StorageMountPoint != DefaultOTAStorageMountPoint ||
		updater.config.StorageDevicePath != DefaultOTAStorageDevicePath ||
		updater.config.StorageFilesystem != DefaultOTAStorageFilesystem {
		t.Fatalf("storage identity = %q %q %q, want fixed production defaults",
			updater.config.StorageMountPoint,
			updater.config.StorageDevicePath,
			updater.config.StorageFilesystem,
		)
	}
}

func TestDefaultUpdaterUsesPartlabelPaths(t *testing.T) {
	config := DefaultUpdaterConfig()
	if config.StorageDevicePath != "/dev/disk/by-partlabel/ota" {
		t.Fatalf("StorageDevicePath = %q, want by-partlabel OTA path", config.StorageDevicePath)
	}
	if config.MiscPath != "/dev/disk/by-partlabel/misc" {
		t.Fatalf("MiscPath = %q, want by-partlabel misc path", config.MiscPath)
	}
	if config.BlockDir != "/dev/disk/by-partlabel" {
		t.Fatalf("BlockDir = %q, want by-partlabel directory", config.BlockDir)
	}
}

func TestPreferExistingPathRetainsLegacyCompatibility(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "by-partlabel", "ota")
	legacy := filepath.Join(dir, "by-name", "ota")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatalf("MkdirAll(legacy) error = %v", err)
	}
	if err := os.WriteFile(legacy, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(legacy) error = %v", err)
	}
	if got := preferExistingPath(primary, legacy); got != legacy {
		t.Fatalf("preferExistingPath() = %q, want legacy %q", got, legacy)
	}
	if err := os.MkdirAll(filepath.Dir(primary), 0o755); err != nil {
		t.Fatalf("MkdirAll(primary) error = %v", err)
	}
	if err := os.WriteFile(primary, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(primary) error = %v", err)
	}
	if got := preferExistingPath(primary, legacy); got != primary {
		t.Fatalf("preferExistingPath() = %q, want primary %q", got, primary)
	}
}

func TestMountPointIsActiveRequiresExpectedDeviceFilesystemAndRoot(t *testing.T) {
	tmp := t.TempDir()
	devicePath := filepath.Join(tmp, "ota-device")
	deviceAlias := filepath.Join(tmp, "ota-device-alias")
	if err := os.WriteFile(devicePath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(device) error = %v", err)
	}
	if err := os.Symlink(devicePath, deviceAlias); err != nil {
		t.Fatalf("Symlink(device) error = %v", err)
	}

	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "expected mount",
			line: fmt.Sprintf("36 25 179:12 / /userdata/ota rw,relatime - ext4 %s rw\n", deviceAlias),
			want: true,
		},
		{
			name: "wrong device",
			line: "36 25 179:12 / /userdata/ota rw,relatime - ext4 /dev/mmcblk0p99 rw\n",
		},
		{
			name: "wrong filesystem",
			line: fmt.Sprintf("36 25 0:42 / /userdata/ota rw,relatime - tmpfs %s rw\n", devicePath),
		},
		{
			name: "bind mount subtree",
			line: fmt.Sprintf("36 25 179:12 /downloads /userdata/ota rw,relatime - ext4 %s rw\n", devicePath),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mountInfoPath := filepath.Join(t.TempDir(), "mountinfo")
			if err := os.WriteFile(mountInfoPath, []byte(test.line), 0o644); err != nil {
				t.Fatalf("WriteFile(mountinfo) error = %v", err)
			}
			got, err := mountPointIsActive(mountInfoPath, "/userdata/ota", devicePath, "ext4")
			if err != nil {
				t.Fatalf("mountPointIsActive() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("mountPointIsActive() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestUpdaterFailsClosedWhenDedicatedStorageIsNotMounted(t *testing.T) {
	storageDir := t.TempDir()
	mountInfoPath := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfoPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(mountinfo) error = %v", err)
	}
	lockPath := filepath.Join(storageDir, DefaultOTAUpdateLockName)
	updater, err := NewUpdater(UpdaterConfig{
		StateDir:          storageDir,
		DownloadDir:       filepath.Join(storageDir, "downloads"),
		StorageMountPoint: storageDir,
		MountInfoPath:     mountInfoPath,
		UpdateLockPath:    lockPath,
	}, func() error { return nil })
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}

	_, err = updater.CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dedicated OTA storage is not mounted") {
		t.Fatalf("CheckOnce() error = %v, want dedicated mount failure", err)
	}
	if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
		t.Fatalf("OTA lock was created on the underlying filesystem: %v", statErr)
	}
}

func TestUpdaterHealthAndStatusFailClosedWhenDedicatedStorageIsNotMounted(t *testing.T) {
	storageDir := t.TempDir()
	mountInfoPath := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfoPath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(mountinfo) error = %v", err)
	}
	updater, err := NewUpdater(UpdaterConfig{
		StateDir:          storageDir,
		DownloadDir:       filepath.Join(storageDir, "downloads"),
		StorageMountPoint: storageDir,
		MountInfoPath:     mountInfoPath,
	}, func() error { return nil })
	if err != nil {
		t.Fatalf("NewUpdater() error = %v", err)
	}

	if err := updater.ProcessPendingHealthOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "dedicated OTA storage is not mounted") {
		t.Fatalf("ProcessPendingHealthOnce() error = %v, want dedicated mount failure", err)
	}
	if _, _, err := updater.Status(); err == nil || !strings.Contains(err.Error(), "dedicated OTA storage is not mounted") {
		t.Fatalf("Status() error = %v, want dedicated mount failure", err)
	}
}

func TestUpdaterRejectsDownloadDirectoryOutsideDedicatedStorage(t *testing.T) {
	storageDir := t.TempDir()
	_, err := NewUpdater(UpdaterConfig{
		StateDir:          storageDir,
		DownloadDir:       filepath.Join(t.TempDir(), "downloads"),
		StorageMountPoint: storageDir,
	}, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "download_dir must be inside the dedicated OTA storage mount") {
		t.Fatalf("NewUpdater() error = %v, want dedicated storage path validation", err)
	}
}

func TestUpdaterRejectsUpdateLockOutsideDedicatedStorage(t *testing.T) {
	storageDir := t.TempDir()
	_, err := NewUpdater(UpdaterConfig{
		StateDir:          storageDir,
		DownloadDir:       filepath.Join(storageDir, "downloads"),
		StorageMountPoint: storageDir,
		UpdateLockPath:    filepath.Join(t.TempDir(), "update.lock"),
	}, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "update_lock_path must be inside the dedicated OTA storage mount") {
		t.Fatalf("NewUpdater() error = %v, want dedicated lock path validation", err)
	}
}

func TestUpdaterChecksActualCapacityBeforeAssetDownload(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assets := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	manifest := env.signedManifest(assets, nil)
	var assetRequests atomic.Int32
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
		assetRequests.Add(1)
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if body, ok := assets[name]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	env.config.DownloadSafetyMarginBytes = 64
	updater := env.updater()
	updater.availableBytes = func(string) (int64, error) { return 64, nil }

	_, err := updater.CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "insufficient OTA download space") {
		t.Fatalf("CheckOnce() error = %v, want actual capacity rejection", err)
	}
	if got := assetRequests.Load(); got != 0 {
		t.Fatalf("asset requests = %d, want 0 before capacity succeeds", got)
	}
	state, loadErr := LoadState(filepath.Join(env.stateDir, "state.json"))
	if loadErr != nil {
		t.Fatalf("LoadState() error = %v", loadErr)
	}
	if state.Phase != "space" {
		t.Fatalf("state phase = %q, want space", state.Phase)
	}
}

func TestUpdaterCapacityCreditsResumablePartialDownload(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assets := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	manifest := env.signedManifest(assets, nil)
	if err := os.MkdirAll(env.downloadDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(downloadDir) error = %v", err)
	}
	partial := assets["boot_b.img"][:5]
	if err := os.WriteFile(filepath.Join(env.downloadDir, "boot_b.img.part"), partial, 0o644); err != nil {
		t.Fatalf("WriteFile(partial) error = %v", err)
	}

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
		body, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if name == "boot_b.img" && r.Header.Get("Range") == "bytes=5-" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(body)-1, len(body)))
			w.Header().Set("Content-Length", fmt.Sprint(len(body)-5))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[5:])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	env.config.DownloadSafetyMarginBytes = 64
	remaining := int64(len(assets["boot_b.img"]) - len(partial) + len(assets["rootfs.img"]))
	updater := env.updater()
	updater.availableBytes = func(string) (int64, error) { return remaining + 64, nil }

	result, err := updater.CheckOnce(context.Background())
	if err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	if !result.Updated {
		t.Fatalf("CheckOnce() = %+v, want update", result)
	}
}

func TestUpdaterStreamsOneVerifiedAssetAtATimeWithinLargestAssetBudget(t *testing.T) {
	env := newUpdaterTestEnv(t)
	assets := map[string][]byte{
		"boot_a.img": []byte("boot-a-v2"),
		"boot_b.img": []byte("boot-b-v2"),
		"rootfs.img": []byte("rootfs-v2"),
	}
	manifest := env.signedManifest(assets, nil)
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		t.Fatalf("readMiscFile() error = %v", err)
	}
	ab.Slots[SlotB] = SlotData{Priority: MaxPriority - 1, SuccessfulBoot: true}
	if err := writeMiscFile(env.miscPath, ab); err != nil {
		t.Fatalf("writeMiscFile() error = %v", err)
	}

	var invariantMu sync.Mutex
	var invariantErr string
	setInvariantErr := func(format string, args ...any) {
		invariantMu.Lock()
		defer invariantMu.Unlock()
		if invariantErr == "" {
			invariantErr = fmt.Sprintf(format, args...)
		}
	}
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
		switch name {
		case "boot_b.img":
			current, readErr := readMiscFile(env.miscPath)
			if readErr != nil || !current.Bootable(SlotB) {
				setInvariantErr("target slot was invalidated before the first asset was downloaded: misc=%+v err=%v", current.Slots[SlotB], readErr)
			}
		case "rootfs.img":
			assertStreamedPartComplete(env, "boot_b", "boot-b-v2", "boot_b.img", setInvariantErr)
		default:
			http.NotFound(w, r)
			return
		}
		body := assets[name]
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	env.config.DownloadSafetyMarginBytes = 64
	largest := int64(0)
	total := int64(0)
	for _, name := range []string{"boot_b.img", "rootfs.img"} {
		size := int64(len(assets[name]))
		total += size
		if size > largest {
			largest = size
		}
	}
	if largest >= total {
		t.Fatalf("test assets do not exercise streaming budget: largest=%d total=%d", largest, total)
	}
	updater := env.updater()
	updater.availableBytes = func(string) (int64, error) { return largest + env.config.DownloadSafetyMarginBytes, nil }
	if _, err := updater.CheckOnce(context.Background()); err != nil {
		t.Fatalf("CheckOnce() error = %v", err)
	}
	invariantMu.Lock()
	gotInvariantErr := invariantErr
	invariantMu.Unlock()
	if gotInvariantErr != "" {
		t.Fatal(gotInvariantErr)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "rootfs_b"), "rootfs-v2")
	if _, err := os.Stat(filepath.Join(env.downloadDir, "rootfs.img")); !os.IsNotExist(err) {
		t.Fatalf("rootfs cache still exists after streaming update: %v", err)
	}
}

func assertStreamedPartComplete(env *updaterTestEnv, blockName string, wantContent string, cacheName string, fail func(string, ...any)) {
	content, err := os.ReadFile(filepath.Join(env.blockDir, blockName))
	if err != nil || string(content) != wantContent {
		fail("streamed partition %s = %q err=%v, want %q", blockName, content, err, wantContent)
		return
	}
	if _, err := os.Stat(filepath.Join(env.downloadDir, cacheName)); !os.IsNotExist(err) {
		fail("streamed cache %s still exists before the next asset download: %v", cacheName, err)
		return
	}
	ab, err := readMiscFile(env.miscPath)
	if err != nil {
		fail("read misc after streaming %s: %v", blockName, err)
		return
	}
	if ab.Bootable(SlotB) || ab.Slots[SlotB] != (SlotData{}) {
		fail("target slot after streaming %s = %+v, want unbootable", blockName, ab.Slots[SlotB])
	}
}

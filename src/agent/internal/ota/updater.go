package ota

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"aico-agent/internal/logging"
	"aico-agent/internal/netproxy"
)

const (
	DefaultDebianOTAConfigPath          = "/userdata/debian/ota/config.json"
	DefaultOTAConfigPath                = DefaultDebianOTAConfigPath
	DefaultOTAStateDir                  = "/userdata/ota"
	DefaultOTAStorageMountPoint         = "/userdata/ota"
	DefaultOTAStorageDevicePath         = "/dev/disk/by-partlabel/ota"
	DefaultOTAMiscPath                  = "/dev/disk/by-partlabel/misc"
	DefaultOTABlockDir                  = "/dev/disk/by-partlabel"
	LegacyOTAStorageDevicePath          = "/dev/block/by-name/ota"
	LegacyOTAMiscPath                   = "/dev/block/by-name/misc"
	LegacyOTABlockDir                   = "/dev/block/by-name"
	DefaultOTAStorageFilesystem         = "ext4"
	DefaultOTAMountInfoPath             = "/proc/self/mountinfo"
	DefaultOTAUpdateLockName            = "update.lock"
	DefaultOTADownloadSafetyMarginBytes = 16 << 20
	DefaultReleaseURL                   = "https://api.github.com/repos/AidenAI-IO/aico-link/releases/latest"
	MaxRemoteManifestBytes              = 1 << 20
	// DefaultHTTPRequestLimit bounds metadata requests (release JSON, manifest).
	// Those are small and fast; 30m is already generous.
	DefaultHTTPRequestLimit = 30 * time.Minute
	// DefaultHTTPDownloadLimit bounds a single image download. Images are
	// hundreds of MB and the device may be on a slow link, so this gets a
	// larger budget than metadata requests. An explicit HTTPTimeout overrides
	// both -- this is only the default when none is configured.
	DefaultHTTPDownloadLimit         = time.Hour
	DefaultHTTPResponseHeaderTimeout = 30 * time.Second
)

var ErrUpdateAlreadyRunning = errors.New("ota update already running")

type UpdaterConfig struct {
	ExpectedVersion           string                       `json:"-"`
	ExpectedBuildTime         string                       `json:"-"`
	ConfigPath                string                       `json:"-"`
	StateDir                  string                       `json:"state_dir,omitempty"`
	DownloadDir               string                       `json:"download_dir,omitempty"`
	StorageMountPoint         string                       `json:"-"` // Fixed in production; tests may override before NewUpdater.
	StorageDevicePath         string                       `json:"-"` // Fixed in production; tests may override before NewUpdater.
	StorageFilesystem         string                       `json:"-"` // Fixed in production; tests may override before NewUpdater.
	MountInfoPath             string                       `json:"-"`
	DownloadSafetyMarginBytes int64                        `json:"download_safety_margin_bytes,omitempty"`
	UpdateLockPath            string                       `json:"update_lock_path,omitempty"`
	MiscPath                  string                       `json:"misc_path,omitempty"`
	BlockDir                  string                       `json:"block_dir,omitempty"`
	ManifestURL               string                       `json:"manifest_url,omitempty"`
	Repo                      string                       `json:"repo,omitempty"`
	Channel                   string                       `json:"channel,omitempty"`
	ReleaseURL                string                       `json:"-"` // Test override for default release URL
	PublicKeyPath             string                       `json:"public_key_path,omitempty"`
	PublicKey                 ed25519.PublicKey            `json:"-"`
	FactoryVersion            string                       `json:"factory_version,omitempty"`
	FactoryBuildTime          string                       `json:"factory_build_time,omitempty"`
	FactoryPartitionHashes    map[string]map[string]string `json:"factory_partition_hashes,omitempty"`
	PartitionSizes            map[string]int64             `json:"partition_sizes,omitempty"`
	GitHubToken               string                       `json:"github_token,omitempty"`
	GitHubTokenPath           string                       `json:"github_token_path,omitempty"`
	GitHubProxyURL            string                       `json:"github_proxy_url,omitempty"`
	SwitchTries               uint8                        `json:"switch_tries,omitempty"`
	HealthTimeout             time.Duration                `json:"-"`
	HealthTimeoutSecs         int                          `json:"health_timeout_seconds,omitempty"`
	HealthPollInterval        time.Duration                `json:"-"`
	HTTPTimeout               time.Duration                `json:"-"`
	HTTPTimeoutSecs           int                          `json:"http_timeout_seconds,omitempty"`
	DownloadTimeout           time.Duration                `json:"-"`
	DownloadTimeoutSecs       int                          `json:"download_timeout_seconds,omitempty"`
	DryRun                    bool                         `json:"dry_run,omitempty"`
	TargetSlotOverride        string                       `json:"target_slot_override,omitempty"`
	DebianMode                bool                         `json:"-"`
	MachineIDPath             string                       `json:"-"`
	RuntimeMachineIDPath      string                       `json:"-"`
	PersonalizationPath       string                       `json:"-"`
	FactoryIdentityPath       string                       `json:"-"`
	DebugfsPath               string                       `json:"-"`
	E2fsckPath                string                       `json:"-"`
	MachineIDSetupPath        string                       `json:"-"`
}

type UpdateResult struct {
	Updated    bool
	NoUpdate   bool
	Version    string
	TargetSlot Slot
}

type Updater struct {
	config          UpdaterConfig
	reboot          func() error
	writeABData     func(ABData) error
	currentSlot     func() (Slot, bool, error)
	currentRootSlot func() (Slot, bool, error)
	bootID          func() string
	availableBytes  func(string) (int64, error)
	runCommand      personalizationCommandRunner
}

func LoadUpdaterConfig(path string) (UpdaterConfig, error) {
	if path == "" {
		path = DefaultOTAConfigPath
	}
	config := UpdaterConfig{
		ConfigPath: path,
		DebianMode: filepath.Clean(path) == filepath.Clean(DefaultDebianOTAConfigPath),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return normalizeUpdaterConfig(config)
		}
		return UpdaterConfig{}, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return UpdaterConfig{}, err
	}
	config.ConfigPath = path
	return normalizeUpdaterConfig(config)
}

func DefaultUpdaterConfig() UpdaterConfig {
	config, _ := normalizeUpdaterConfig(UpdaterConfig{})
	return config
}

func normalizeUpdaterConfig(config UpdaterConfig) (UpdaterConfig, error) {
	// Captured before the defaults below overwrite HTTPTimeout, so the download
	// budget can tell "operator asked for this" from "nobody set anything".
	httpTimeoutWasExplicit := config.HTTPTimeout > 0 || config.HTTPTimeoutSecs > 0
	if config.StateDir == "" {
		config.StateDir = DefaultOTAStateDir
	}
	if config.DownloadDir == "" {
		config.DownloadDir = filepath.Join(config.StateDir, "downloads")
	}
	if config.StorageMountPoint == "" {
		config.StorageMountPoint = DefaultOTAStorageMountPoint
	}
	if config.StorageDevicePath == "" {
		config.StorageDevicePath = DefaultOTAStorageDevicePath
	}
	if config.StorageFilesystem == "" {
		config.StorageFilesystem = DefaultOTAStorageFilesystem
	}
	if config.MountInfoPath == "" {
		config.MountInfoPath = DefaultOTAMountInfoPath
	}
	if !pathIsWithin(config.StorageMountPoint, config.StateDir) {
		return UpdaterConfig{}, fmt.Errorf("state_dir must be inside the dedicated OTA storage mount")
	}
	if !pathIsWithin(config.StorageMountPoint, config.DownloadDir) {
		return UpdaterConfig{}, fmt.Errorf("download_dir must be inside the dedicated OTA storage mount")
	}
	if config.DownloadSafetyMarginBytes == 0 {
		config.DownloadSafetyMarginBytes = DefaultOTADownloadSafetyMarginBytes
	}
	if config.DownloadSafetyMarginBytes < 0 {
		return UpdaterConfig{}, fmt.Errorf("download_safety_margin_bytes must be non-negative")
	}
	if config.UpdateLockPath == "" {
		config.UpdateLockPath = filepath.Join(config.StateDir, DefaultOTAUpdateLockName)
	}
	if !pathIsWithin(config.StorageMountPoint, config.UpdateLockPath) {
		return UpdaterConfig{}, fmt.Errorf("update_lock_path must be inside the dedicated OTA storage mount")
	}
	if config.MiscPath == "" {
		config.MiscPath = DefaultOTAMiscPath
	}
	if config.BlockDir == "" {
		config.BlockDir = DefaultOTABlockDir
	}
	if config.PublicKeyPath == "" {
		config.PublicKeyPath = "/usr/share/keyrings/aico-ota.pem"
	}
	if config.MachineIDPath == "" {
		config.MachineIDPath = DefaultPersistentMachineIDPath
	}
	if config.RuntimeMachineIDPath == "" {
		config.RuntimeMachineIDPath = "/etc/machine-id"
	}
	if config.PersonalizationPath == "" {
		config.PersonalizationPath = DefaultPersonalizationSidecarPath
	}
	if config.FactoryIdentityPath == "" {
		config.FactoryIdentityPath = DefaultFactoryIdentityPath
	}
	if config.DebugfsPath == "" {
		config.DebugfsPath = DefaultDebugfsPath
	}
	if config.E2fsckPath == "" {
		config.E2fsckPath = DefaultE2fsckPath
	}
	if config.MachineIDSetupPath == "" {
		config.MachineIDSetupPath = DefaultSystemdMachineIDSetupPath
	}
	if config.GitHubTokenPath == "" {
		config.GitHubTokenPath = filepath.Join(config.StateDir, "gh_token")
	}
	if config.SwitchTries == 0 {
		config.SwitchTries = 3
	}
	if config.HealthTimeout == 0 {
		if config.HealthTimeoutSecs > 0 {
			config.HealthTimeout = time.Duration(config.HealthTimeoutSecs) * time.Second
		} else {
			config.HealthTimeout = 5 * time.Minute
		}
	}
	if config.HealthPollInterval == 0 {
		config.HealthPollInterval = time.Second
	}
	if config.HTTPTimeout <= 0 {
		if config.HTTPTimeoutSecs > 0 {
			config.HTTPTimeout = time.Duration(config.HTTPTimeoutSecs) * time.Second
		} else {
			config.HTTPTimeout = DefaultHTTPRequestLimit
		}
	}
	// Resolve the download budget after HTTPTimeout so an operator who sets only
	// HTTPTimeout still gets that value applied to downloads. This has to read
	// the caller's intent rather than the normalized field, which is why it
	// checks *Secs and the pre-normalization duration instead of the result
	// above -- by now an unset HTTPTimeout looks identical to an explicit 30m.
	if config.DownloadTimeout <= 0 {
		switch {
		case config.DownloadTimeoutSecs > 0:
			config.DownloadTimeout = time.Duration(config.DownloadTimeoutSecs) * time.Second
		case httpTimeoutWasExplicit:
			config.DownloadTimeout = config.HTTPTimeout
		default:
			config.DownloadTimeout = DefaultHTTPDownloadLimit
		}
	}
	if config.SwitchTries > MaxTries {
		return UpdaterConfig{}, fmt.Errorf("switch_tries %d exceeds %d", config.SwitchTries, MaxTries)
	}
	return config, nil
}

func NewUpdater(config UpdaterConfig, reboot func() error) (*Updater, error) {
	config, err := normalizeUpdaterConfig(config)
	if err != nil {
		return nil, err
	}
	u := &Updater{
		config:          config,
		reboot:          reboot,
		currentSlot:     currentSlotFromProcCmdline,
		currentRootSlot: currentRootSlotFromProcCmdline,
		bootID:          currentBootID,
		availableBytes:  filesystemAvailableBytes,
		runCommand:      runPersonalizationCommand,
	}
	u.writeABData = u.writeABDataFile
	return u, nil
}

func (u *Updater) CheckOnce(ctx context.Context) (UpdateResult, error) {
	if err := u.ensureStorageReady(); err != nil {
		logging.Errorf("ota", "updater", "ota check: %v", err)
		return UpdateResult{}, err
	}
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		logging.Errorf("ota", "updater", "ota check: %v", err)
		return UpdateResult{}, err
	}
	defer unlock()
	return u.checkOnceLocked(ctx)
}

// checkOnceLocked performs one update while the caller holds the update lock.
// It verifies archives before writing, then verifies streamed images and eMMC readback before activation.
func (u *Updater) checkOnceLocked(ctx context.Context) (UpdateResult, error) {
	logging.Infof("ota", "updater", "ota check: start")
	if err := u.ProcessPendingHealth(ctx); err != nil {
		u.recordError("health", err)
		return UpdateResult{}, err
	}

	state, err := u.loadState()
	if err != nil {
		return UpdateResult{}, err
	}
	ab, err := u.readABData()
	if err != nil {
		u.recordError("read-misc", err)
		return UpdateResult{}, err
	}
	miscActive, ok := ab.ActiveSlot()
	if !ok {
		err := errors.New("misc has no bootable active slot")
		u.recordError("read-misc", err)
		return UpdateResult{}, err
	}
	active := miscActive
	if u.currentSlot != nil {
		running, runningOK, err := u.currentSlot()
		if err != nil {
			u.recordError("current-slot", err)
			return UpdateResult{}, err
		}
		if runningOK {
			active = running
		}
	}
	target := inactiveSlot(active)
	if u.config.TargetSlotOverride != "" {
		target, err = parseSlotName(u.config.TargetSlotOverride)
		if err != nil {
			u.recordError("target-slot", err)
			return UpdateResult{}, err
		}
	}
	logging.Infof("ota", "updater", "ota check: active_slot=%s target_slot=%s", slotLogName(active), slotLogName(target))

	manifest, assetsByName, err := u.fetchUpdateManifest(ctx)
	if err != nil {
		u.recordError("manifest", err)
		return UpdateResult{}, err
	}
	if (u.config.ExpectedVersion != "" && manifest.Version != u.config.ExpectedVersion) ||
		(u.config.ExpectedBuildTime != "" && manifest.BuildTime != u.config.ExpectedBuildTime) {
		err := fmt.Errorf("OTA release changed since availability check; check again")
		u.recordError("manifest", err)
		return UpdateResult{}, err
	}
	token := u.githubToken()
	if err := state.RejectDowngrade(manifest); err != nil {
		u.recordError("policy", err)
		return UpdateResult{}, err
	}
	if isNoUpdate(state, manifest) {
		logging.Infof("ota", "updater", "ota check: no update version=%s build_time=%s", manifest.Version, manifest.BuildTime)
		return UpdateResult{NoUpdate: true, Version: manifest.Version, TargetSlot: target}, nil
	}
	if err := state.ValidateSelectiveUpdate(manifest, target); err != nil {
		err = fmt.Errorf("selective update rejected: %w", err)
		u.recordError("policy", err)
		return UpdateResult{}, err
	}

	selectedAssets := map[string]ManifestAsset{}
	for _, part := range manifest.Parts {
		asset, err := ResolveAsset(part, target)
		if err != nil {
			u.recordError("asset", err)
			return UpdateResult{}, err
		}
		if err := u.validateAssetFitsPartition(part.Name, target, asset); err != nil {
			u.recordError("asset", err)
			return UpdateResult{}, err
		}
		selectedAssets[part.Name] = asset
	}
	plan, err := u.buildDownloadPlan(selectedAssets, state, target)
	if err != nil {
		u.recordError("cleanup", err)
		return UpdateResult{}, err
	}

	if err := u.cleanupOldDownloadCache(plan); err != nil {
		u.recordError("cleanup", err)
		return UpdateResult{}, err
	}
	if err := u.ensureDownloadCapacity(plan); err != nil {
		u.recordError("space", err)
		return UpdateResult{}, err
	}

	if target == active {
		err := fmt.Errorf("refusing to write active slot %s", slotLogName(active))
		u.recordError("target-slot", err)
		return UpdateResult{}, err
	}
	transactionID, err := generateNonce()
	if err != nil {
		u.recordError("transaction", err)
		return UpdateResult{}, err
	}

	statePrepared := false
	prepareState := func() error {
		if statePrepared {
			return nil
		}
		// Debian writes and frees one archive at a time. Publish the protected
		// snapshot before the first target invalidation or partition write.
		snapshotPath, err := SnapshotProtectedData(u.config.StateDir, manifest.Version)
		if err != nil {
			return fmt.Errorf("snapshot protected data: %w", err)
		}
		state.DataSnapshotPath = snapshotPath
		if state.SlotBuildTimes == nil {
			state.SlotBuildTimes = map[string]string{}
		}
		state.SlotBuildTimes[targetNameForState(target)] = manifest.BuildTime
		state.Phase = "writing"
		state.TargetVersion = manifest.Version
		state.TargetBuildTime = manifest.BuildTime
		state.ActiveSlot = active
		state.TargetSlot = target
		state.PendingBootNonce = transactionID
		state.DownloadedAssets = map[string]string{}
		state.DownloadedHashes = map[string]string{}
		for partName, planned := range plan.assets {
			if planned.targetMatches {
				state.DownloadedHashes[partName] = partitionSHA256ForAsset(planned.asset)
			}
		}
		if state.Slots == nil {
			state.Slots = map[string]SlotPartitionInfo{}
		}
		targetKey := targetNameForState(target)
		snapshot := copySlotPartitionInfo(state.Slots[targetKey])
		if len(snapshot.Partitions) > 0 {
			state.PendingTargetSlot = &snapshot
		} else {
			state.PendingTargetSlot = nil
		}
		delete(state.Slots, targetKey)
		if err := u.saveSnapshotState(state); err != nil {
			return err
		}
		statePrepared = true
		return nil
	}

	writer := PartitionWriter{BlockDir: u.blockDirForAccess(), ActiveSlot: active, PartitionSizes: u.partitionSizes()}
	targetInvalidated := false
	for _, part := range manifest.Parts {
		planned := plan.assets[part.Name]
		asset := planned.asset
		if planned.targetMatches {
			logging.Infof("ota", "updater", "ota partition: %s skipped; target slot %s hash matches manifest", part.Name, slotLogName(target))
			continue
		}

		var assetURL string
		var assetToken string
		if asset.URL != "" {
			assetURL = asset.URL
			logging.Infof("ota", "updater", "ota asset: %s using direct URL from manifest", asset.Name)
			if isGitHubURL(assetURL) {
				assetToken = token
			}
		} else if u.config.ManifestURL != "" {
			assetURL, err = deriveAssetURL(u.config.ManifestURL, asset.Name)
			if err != nil {
				u.recordError("asset", err)
				return UpdateResult{}, err
			}
			logging.Infof("ota", "updater", "ota asset: %s using URL derived from manifest URL", asset.Name)
			if isGitHubURL(assetURL) {
				assetToken = token
			}
		} else {
			assetURL, err = requiredAssetURL(assetsByName, asset.Name)
			if err != nil {
				u.recordError("asset", err)
				return UpdateResult{}, err
			}
			logging.Infof("ota", "updater", "ota asset: %s using URL from release API", asset.Name)
			assetToken = token
		}

		dst := planned.path
		if planned.archiveVerified {
			logging.Infof("ota", "updater", "ota download: %s skipped; cached archive already verified dst=%s", asset.Name, dst)
		} else {
			if err := os.MkdirAll(u.config.DownloadDir, 0o755); err != nil {
				u.recordError("download", err)
				return UpdateResult{}, err
			}
			logging.Infof("ota", "updater", "ota download: %s start size=%s url=%s dst=%s", asset.Name, formatBytes(asset.Size), sanitizeURLForLog(assetURL), dst)
			if err := u.downloadFileWithToken(ctx, assetURL, dst, asset.Size, assetToken); err != nil {
				u.recordError("download", err)
				return UpdateResult{}, err
			}
			if err := VerifyFile(dst, asset.Size, asset.SHA256); err != nil {
				err = u.discardInvalidDownload(dst, err)
				u.recordError("verify", err)
				return UpdateResult{}, err
			}
			logging.Infof("ota", "updater", "ota verify: %s sha256 ok", asset.Name)
		}

		if u.config.DryRun {
			if err := u.verifyDownloadedImage(dst, asset); err != nil {
				err = u.discardInvalidDownload(dst, err)
				u.recordError("verify", err)
				return UpdateResult{}, err
			}
			continue
		}
		if err := prepareState(); err != nil {
			u.recordError("state", err)
			return UpdateResult{}, err
		}
		if !targetInvalidated {
			if err := ab.MarkUnbootable(target); err != nil {
				u.recordError("misc", err)
				return UpdateResult{}, err
			}
			if err := u.writeABData(ab); err != nil {
				u.recordError("misc", err)
				return UpdateResult{}, err
			}
			targetInvalidated = true
			logging.Infof("ota", "updater", "ota misc: target slot %s marked unbootable before partition writes", slotLogName(target))
		}

		blockName, err := writer.ResolveBlockName(part.Name, target)
		if err != nil {
			u.recordError("write", err)
			return UpdateResult{}, err
		}
		logging.Infof("ota", "updater", "ota write: %s -> %s start image=%s", part.Name, blockName, dst)
		expectedImageSHA256 := partitionSHA256ForAsset(asset)
		imageSize, err := writer.writePartWithProgressAndVerify(part.Name, target, dst, expectedImageSHA256, u.logWriteProgress)
		if err != nil {
			if errors.Is(err, errPartitionImageSHA256Mismatch) {
				field := "sha256"
				if asset.ImageSHA256 != "" {
					field = "image_sha256"
				}
				err = fmt.Errorf("%s %s: %w", asset.Name, field, err)
				err = u.discardInvalidDownload(dst, err)
				u.recordError("verify", err)
				return UpdateResult{}, err
			}
			u.recordError("write", err)
			return UpdateResult{}, err
		}
		logging.Infof("ota", "updater", "ota verify: %s image sha256 ok during write", asset.Name)
		if err := writer.verifyPartWithSize(part.Name, target, imageSize, expectedImageSHA256); err != nil {
			u.recordError("readback", err)
			return UpdateResult{}, err
		}
		state.DownloadedHashes[part.Name] = expectedImageSHA256
		logging.Infof("ota", "updater", "ota readback: %s -> %s sha256 ok", part.Name, blockName)
		if u.config.DebianMode && part.Name == "rootfs" {
			record, err := u.personalizeRootFS(writer, target, imageSize, asset)
			if err != nil {
				u.recordError("personalization", err)
				return UpdateResult{}, err
			}
			if err := u.commitPersonalizationRecord(transactionID, manifest, target, record); err != nil {
				u.recordError("personalization", err)
				return UpdateResult{}, err
			}
			logging.Infof("ota", "updater", "ota personalization: rootfs slot %s machine-id applied effective_sha256=%s", slotLogName(target), record.EffectivePartitionSHA256)
		}
		if err := u.deleteDownloadCache(dst); err != nil {
			u.recordError("cleanup", err)
			return UpdateResult{}, err
		}
	}
	if u.config.DryRun {
		logging.Infof("ota", "updater", "ota check: dry-run complete version=%s target_slot=%s", manifest.Version, slotLogName(target))
		return UpdateResult{Updated: true, Version: manifest.Version, TargetSlot: target}, nil
	}
	if err := prepareState(); err != nil {
		u.recordError("state", err)
		return UpdateResult{}, err
	}
	if err := DeleteStaleHealthMarker(u.healthPath()); err != nil {
		u.recordError("health", err)
		return UpdateResult{}, err
	}
	targetName, _ := slotName(target)
	pending := PendingBoot{TargetSlot: targetName, TargetVersion: manifest.Version, TargetBuildTime: manifest.BuildTime, Nonce: transactionID}
	if err := WritePendingBoot(u.pendingPath(), pending); err != nil {
		u.recordError("pending", err)
		return UpdateResult{}, err
	}
	state.Phase = "pending-reboot"
	state.PendingBootNonce = transactionID
	if err := SaveState(u.statePath(), state); err != nil {
		u.recordError("state", err)
		return UpdateResult{}, err
	}
	if err := ab.SetActive(target, u.config.SwitchTries, false); err != nil {
		_ = os.Remove(u.pendingPath())
		u.recordError("misc", err)
		return UpdateResult{}, err
	}
	if err := u.writeABData(ab); err != nil {
		_ = os.Remove(u.pendingPath())
		u.recordError("misc", err)
		return UpdateResult{}, err
	}
	logging.Infof("ota", "updater", "ota misc: switched active slot to %s tries=%d", slotLogName(target), u.config.SwitchTries)
	if u.reboot != nil {
		logging.Infof("ota", "updater", "ota reboot: requested after switching to slot %s", slotLogName(target))
		if err := u.reboot(); err != nil {
			u.recordError("reboot", err)
			return UpdateResult{}, err
		}
	}
	return UpdateResult{Updated: true, Version: manifest.Version, TargetSlot: target}, nil
}

func (u *Updater) acquireUpdateLock() (func(), error) {
	lockPath := u.config.UpdateLockPath
	if lockPath == "" {
		lockPath = filepath.Join(u.config.StateDir, DefaultOTAUpdateLockName)
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("create ota update lock dir: %w", err)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open ota update lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrUpdateAlreadyRunning, lockPath)
		}
		return nil, fmt.Errorf("lock ota update: %w", err)
	}
	if err := lockFile.Truncate(0); err == nil {
		_, _ = lockFile.Seek(0, 0)
		_, _ = fmt.Fprintf(lockFile, "%d\n", os.Getpid())
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

// verifyCachedArchive checks only the downloaded artifact size and digest.
// Extracted image validation is deferred to dry-run verification or the streamed write.
func (u *Updater) verifyCachedArchive(path string, asset ManifestAsset) error {
	return VerifyFile(path, asset.Size, asset.SHA256)
}

func (u *Updater) discardInvalidDownload(path string, verifyErr error) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; remove invalid download %s: %v", verifyErr, filepath.Base(path), err)
	}
	logging.Warnf("ota", "updater", "ota download: removed invalid cached file %s", filepath.Base(path))
	return verifyErr
}

func (u *Updater) deleteDownloadCache(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove verified OTA cache %s: %w", filepath.Base(path), err)
	}
	if err := fsyncDirFor(path); err != nil {
		return fmt.Errorf("sync OTA cache directory after removing %s: %w", filepath.Base(path), err)
	}
	logging.Infof("ota", "updater", "ota cleanup: removed verified cache %s", filepath.Base(path))
	return nil
}

// personalizeRootFS applies the persistent machine ID to the written rootfs
// and records its effective digest over imageSize bytes.
func (u *Updater) personalizeRootFS(writer PartitionWriter, target Slot, imageSize int64, asset ManifestAsset) (RootFSPersonalization, error) {
	machineID, err := readPersistentMachineID(u.config.MachineIDPath)
	if err != nil {
		return RootFSPersonalization{}, fmt.Errorf("load persistent machine-id: %w", err)
	}
	blockName, err := writer.ResolveBlockName("rootfs", target)
	if err != nil {
		return RootFSPersonalization{}, err
	}
	blockPath := filepath.Join(writer.BlockDir, blockName)
	effectiveHash, err := personalizeExt4MachineID(
		blockPath,
		machineID,
		imageSize,
		u.config.DebugfsPath,
		u.config.E2fsckPath,
		u.runCommand,
	)
	if err != nil {
		return RootFSPersonalization{}, err
	}
	return RootFSPersonalization{
		ArtifactSHA256:           partitionSHA256ForAsset(asset),
		PersonalizationSchema:    PersonalizationSchemaVersion,
		EffectivePartitionSHA256: effectiveHash,
		HashedBytes:              imageSize,
	}, nil
}

func (u *Updater) commitPersonalizationRecord(transactionID string, manifest Manifest, target Slot, record RootFSPersonalization) error {
	sidecar, err := LoadPersonalizationSidecar(u.config.PersonalizationPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("load personalization sidecar: %w", err)
		}
		sidecar = PersonalizationSidecar{Slots: map[string]RootFSPersonalization{}}
	}
	if sidecar.Slots == nil {
		sidecar.Slots = map[string]RootFSPersonalization{}
	}
	targetName, err := slotName(target)
	if err != nil {
		return err
	}
	sidecar.SchemaVersion = PersonalizationSchemaVersion
	sidecar.TransactionID = transactionID
	sidecar.TargetVersion = manifest.Version
	sidecar.TargetBuildTime = manifest.BuildTime
	sidecar.Slots[targetName] = record
	return SavePersonalizationSidecar(u.config.PersonalizationPath, sidecar)
}

func targetPartitionHashMatches(state State, target Slot, partName string, asset ManifestAsset) bool {
	targetName, err := slotName(target)
	if err != nil {
		return false
	}
	slotState, ok := state.Slots[targetName]
	if !ok {
		return false
	}
	local, ok := slotState.Partitions[partName]
	return ok && local.Version != "" && local.Hash == partitionSHA256ForAsset(asset)
}

func (u *Updater) verifyDownloadedImage(path string, asset ManifestAsset) error {
	if asset.ImageSHA256 == "" {
		return nil
	}
	if err := verifyPartitionImage(path, asset.ImageSHA256); err != nil {
		return fmt.Errorf("%s image_sha256: %w", asset.Name, err)
	}
	logging.Infof("ota", "updater", "ota verify: %s image_sha256 ok", asset.Name)
	return nil
}

func (u *Updater) ProcessPendingHealth(ctx context.Context) error {
	if err := u.recoverABCommit(); err != nil {
		return err
	}
	data, err := os.ReadFile(u.pendingPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var pending PendingBoot
	if err := json.Unmarshal(data, &pending); err != nil {
		return err
	}
	logging.Infof("ota", "updater", "ota health: pending slot=%s version=%s timeout=%s", pending.TargetSlot, pending.TargetVersion, u.config.HealthTimeout)
	pendingSlot, err := parseSlotName(pending.TargetSlot)
	if err != nil {
		return err
	}
	if u.currentSlot != nil {
		running, ok, err := u.currentSlot()
		if err != nil {
			return err
		}
		if ok && running != pendingSlot {
			ab, err := u.readABData()
			if err != nil {
				return err
			}
			if miscActive, activeOK := ab.ActiveSlot(); activeOK && miscActive == pendingSlot {
				runningName, _ := slotName(running)
				return fmt.Errorf("pending target slot %s is selected in misc but running slot is %s", pending.TargetSlot, runningName)
			}
			return u.clearPendingAfterRollback(running)
		}
	}
	bootID := u.bootID()
	if err := ValidateHealthMarker(u.healthPath(), pending, bootID); err == nil {
		logging.Infof("ota", "updater", "ota health: marker valid, committing slot=%s", pending.TargetSlot)
		return u.commitPendingHealth(pending)
	}
	logging.Infof("ota", "updater", "ota health: waiting for marker path=%s", u.healthPath())
	if err := WaitForHealth(ctx, u.healthPath(), pending, bootID, u.config.HealthTimeout, u.config.HealthPollInterval, u.reboot); err != nil {
		return err
	}
	logging.Infof("ota", "updater", "ota health: marker received, committing slot=%s", pending.TargetSlot)
	return u.commitPendingHealth(pending)
}

func (u *Updater) clearPendingAfterRollback(running Slot) error {
	state, err := u.loadState()
	if err != nil {
		return err
	}
	if state.DataSnapshotPath != "" {
		if err := RestoreProtectedData(state.DataSnapshotPath); err != nil {
			state.LastError = fmt.Sprintf("%s; data restore: %v", state.LastError, err)
			_ = SaveState(u.statePath(), state)
			return err
		}
	}
	state.Phase = "rolled-back"
	state.ActiveSlot = running
	state.TargetSlot = running
	state.TargetVersion = ""
	state.TargetBuildTime = ""
	state.PendingBootNonce = ""
	state.PendingBootID = ""
	state.PendingTargetSlot = nil
	state.DataSnapshotPath = ""
	if err := SaveState(u.statePath(), state); err != nil {
		return err
	}
	if err := os.Remove(u.pendingPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// saveSnapshotState publishes state and, when publication fails, removes the
// snapshot only after reading back the state file proves nothing references it.
// A snapshot path recorded on disk (this one or another transaction's) is left
// alone: rollback and recovery still need it, and pruning bounds what leaks.
func (u *Updater) saveSnapshotState(state State) error {
	err := SaveState(u.statePath(), state)
	if err == nil {
		return nil
	}
	if state.DataSnapshotPath == "" {
		return err
	}
	saved, readErr := LoadState(u.statePath())
	if (readErr == nil && saved.DataSnapshotPath != state.DataSnapshotPath) || os.IsNotExist(readErr) {
		if removeErr := os.RemoveAll(state.DataSnapshotPath); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		// Make the removal durable in the parent transactions directory.
		return errors.Join(err, fsyncDirFor(state.DataSnapshotPath))
	}
	return err
}

func (u *Updater) commitPendingHealth(pending PendingBoot) error {
	slot, err := parseSlotName(pending.TargetSlot)
	if err != nil {
		return err
	}
	ab, err := u.readABData()
	if err != nil {
		return err
	}
	if err := u.prepareABCommit(ab); err != nil {
		return err
	}
	if err := ab.MarkSuccessful(slot); err != nil {
		return err
	}
	if err := u.writeABData(ab); err != nil {
		return err
	}
	if err := u.verifyABCommit(ab); err != nil {
		return err
	}
	state, err := u.loadState()
	if err != nil {
		return err
	}
	state.Phase = "committed"
	state.CurrentVersion = pending.TargetVersion
	state.CurrentBuildTime = pending.TargetBuildTime
	state.LastCommittedVersion = pending.TargetVersion
	state.LastCommittedBuildTime = pending.TargetBuildTime
	state.ActiveSlot = slot
	state.TargetVersion = ""
	state.TargetBuildTime = ""
	slotKey, err := slotName(slot)
	if err != nil {
		return err
	}
	if state.Slots == nil {
		state.Slots = map[string]SlotPartitionInfo{}
	}
	slotState := SlotPartitionInfo{}
	if state.PendingTargetSlot != nil {
		slotState = copySlotPartitionInfo(*state.PendingTargetSlot)
	}
	state.PendingBootNonce = ""
	state.PendingBootID = ""
	state.PendingTargetSlot = nil
	state.LastError = ""
	if len(slotState.Partitions) == 0 {
		slotState = state.Slots[slotKey]
	}
	if slotState.Partitions == nil {
		slotState.Partitions = map[string]PartitionVersion{}
	}
	for part, hash := range state.DownloadedHashes {
		slotState.Partitions[part] = PartitionVersion{Version: pending.TargetVersion, Hash: hash}
	}
	state.Slots[slotKey] = slotState
	if err := SaveState(u.statePath(), state); err != nil {
		return err
	}
	if err := os.Remove(u.pendingPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := fsyncDirFor(u.pendingPath()); err != nil {
		return err
	}
	return u.clearABCommit()
}

func (u *Updater) ProcessPendingHealthOnce(ctx context.Context) error {
	logging.Infof("ota", "updater", "ota health: processing pending boot")
	if err := u.ensureStorageReady(); err != nil {
		logging.Errorf("ota", "updater", "ota health: %v", err)
		return err
	}
	if err := u.processPendingHealthWithLock(ctx); err != nil {
		logging.Errorf("ota", "updater", "ota health: %v", err)
		if !errors.Is(err, ErrUpdateAlreadyRunning) {
			u.recordError("health", err)
		}
		return err
	}
	logging.Infof("ota", "updater", "ota health: complete")
	return nil
}

func (u *Updater) processPendingHealthWithLock(ctx context.Context) error {
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	return u.ProcessPendingHealth(ctx)
}

func (u *Updater) Status() (State, ABData, error) {
	if err := u.ensureStorageReady(); err != nil {
		return State{}, ABData{}, err
	}
	state, stateErr := u.loadState()
	ab, abErr := u.readABData()
	if stateErr != nil && !os.IsNotExist(stateErr) {
		return State{}, ABData{}, stateErr
	}
	if abErr != nil {
		return state, ABData{}, abErr
	}
	return state, ab, nil
}

// Rollback selects the last successful slot and reboots into it. It is also
// used by the self-check supervisor for failures discovered after boot.
func (u *Updater) Rollback(reason string) error {
	if err := u.ensureStorageReady(); err != nil {
		return err
	}
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := u.recoverABCommit(); err != nil {
		return err
	}
	state, err := u.loadState()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if state.DataSnapshotPath != "" {
		if err := RestoreProtectedData(state.DataSnapshotPath); err != nil {
			state.LastError = fmt.Sprintf("%s; data restore: %v", strings.TrimSpace(reason), err)
			_ = SaveState(u.statePath(), state)
			return err
		}
	}
	ab, err := u.readABData()
	if err != nil {
		return err
	}
	active, ok := ab.ActiveSlot()
	if !ok {
		return errors.New("misc has no bootable active slot")
	}
	previous := SlotA
	if active == SlotA {
		previous = SlotB
	}
	if !ab.Slots[previous].SuccessfulBoot {
		return fmt.Errorf("no successful previous slot available (active=%s)", slotLogName(active))
	}
	// The old slot was already the last committed slot. Make it successful
	// immediately so a rollback does not create a second probation boot with no
	// pending health marker.
	if err := ab.SetActive(previous, 0, true); err != nil {
		return err
	}
	if err := u.writeABData(ab); err != nil {
		return err
	}
	state.Phase = "rollback-requested"
	state.LastError = strings.TrimSpace(reason)
	state.ActiveSlot = active
	state.TargetSlot = previous
	if err := SaveState(u.statePath(), state); err != nil {
		return err
	}
	_ = os.Remove(u.pendingPath())
	_ = os.Remove(u.healthPath())
	if u.reboot != nil {
		return u.reboot()
	}
	return nil
}

// RecoverPendingData is safe to run during early boot. It completes the
// configuration side of an interrupted rollback without touching append-only
// user data.
func (u *Updater) RecoverPendingData() error {
	if err := u.ensureStorageReady(); err != nil {
		return err
	}
	unlock, err := u.acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := u.recoverABCommit(); err != nil {
		return err
	}
	// First boot must not require a factory baseline or identity migration.
	// There is nothing to recover until a transaction state has been saved.
	state, err := LoadState(u.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	running, runningOK, err := u.currentSlot()
	if err != nil {
		return err
	}
	ab, err := u.readABData()
	if err != nil {
		return err
	}
	miscActive, miscOK := ab.ActiveSlot()
	if err := pruneProtectedSnapshots(u.config.StateDir, state.DataSnapshotPath); err != nil {
		return err
	}
	if state.Phase == "pending-reboot" && runningOK && running != state.TargetSlot && miscOK && miscActive == running && ab.Slots[running].SuccessfulBoot {
		return u.clearPendingAfterRollback(running)
	}
	if state.Phase == "rollback-requested" && runningOK {
		if miscOK && miscActive == state.TargetSlot && running == state.TargetSlot && ab.Slots[running].SuccessfulBoot {
			if state.DataSnapshotPath != "" {
				if err := RestoreProtectedData(state.DataSnapshotPath); err != nil {
					return err
				}
				state.DataSnapshotPath = ""
			}
			reconcileRollbackState(&state, running, true)
			return SaveState(u.statePath(), state)
		}
		if miscOK && miscActive == state.ActiveSlot && running == state.ActiveSlot && ab.Slots[running].SuccessfulBoot {
			reconcileRollbackState(&state, running, false)
			return SaveState(u.statePath(), state)
		}
	}
	return nil
}

func reconcileRollbackState(state *State, running Slot, completed bool) {
	name, err := slotName(running)
	if err != nil {
		return
	}
	state.ActiveSlot = running
	state.TargetSlot = running
	state.TargetVersion = ""
	state.TargetBuildTime = ""
	state.PendingBootNonce = ""
	state.PendingBootID = ""
	state.PendingTargetSlot = nil
	if completed {
		state.Phase = "rolled-back"
	} else {
		state.Phase = "committed"
	}
	state.CurrentVersion = ""
	state.CurrentBuildTime = ""
	if slot, ok := state.Slots[name]; ok {
		if part, ok := slot.Partitions["boot"]; ok && part.Version != "" {
			state.CurrentVersion = part.Version
		}
	}
	if state.SlotBuildTimes != nil {
		if buildTime := state.SlotBuildTimes[name]; buildTime != "" {
			state.CurrentBuildTime = buildTime
		}
	}
	state.LastCommittedVersion = state.CurrentVersion
	state.LastCommittedBuildTime = state.CurrentBuildTime
}

func (u *Updater) VerifyManifestFile(path string) (Manifest, error) {
	parent := context.Background()
	ctx, cancel := u.httpContext(parent)
	defer cancel()
	data, err := readLocalOrRemoteManifest(ctx, path)
	if err != nil {
		return Manifest{}, describeTimeout(parent, err, "manifest read", u.httpTimeout())
	}
	key, err := u.publicKey()
	if err != nil {
		return Manifest{}, err
	}
	return VerifyManifestJSON(data, key)
}

func (u *Updater) httpContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, u.httpTimeout())
}

func (u *Updater) httpTimeout() time.Duration {
	if u.config.HTTPTimeout > 0 {
		return u.config.HTTPTimeout
	}
	return DefaultHTTPRequestLimit
}

// downloadContext bounds a single image download. It reuses an explicitly
// configured HTTPTimeout when present so operators keep one knob, and otherwise
// falls back to the larger download-specific default.
func (u *Updater) downloadContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, u.downloadTimeout())
}

func (u *Updater) downloadTimeout() time.Duration {
	if u.config.DownloadTimeout > 0 {
		return u.config.DownloadTimeout
	}
	return DefaultHTTPDownloadLimit
}

// describeTimeout turns a bare "context deadline exceeded" into something that
// names the budget that was blown. Without this the OTA log and state.json
// last_error both just say "context deadline exceeded", which reads like a
// generic network fault and gives no hint that raising the timeout is the fix.
// A parent cancellation (shutdown, SIGTERM) is reported as such instead, since
// that is not a timeout and must not point the reader at the timeout setting.
func describeTimeout(parent context.Context, err error, what string, budget time.Duration) error {
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if parent.Err() != nil {
		return err
	}
	return fmt.Errorf("%s timed out after %s: %w", what, budget, err)
}

func readLocalOrRemoteManifest(ctx context.Context, path string) ([]byte, error) {
	parsed, err := url.Parse(path)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return fetchBytesWithTokenLimit(ctx, path, "", MaxRemoteManifestBytes)
	}
	return os.ReadFile(path)
}

func (u *Updater) loadState() (State, error) {
	state, err := LoadState(u.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return u.initializeFactoryState()
		}
		return State{}, err
	}
	if state.Slots == nil {
		state.Slots = map[string]SlotPartitionInfo{}
	}
	return state, nil
}

func (u *Updater) initializeFactoryState() (State, error) {
	if u.config.FactoryVersion == "" || u.config.FactoryBuildTime == "" || len(u.config.FactoryPartitionHashes) == 0 {
		return State{}, errors.New("missing OTA state: factory_version, factory_build_time, and factory_partition_hashes are required")
	}
	if err := validateFactoryPartitionHashes(u.config.FactoryPartitionHashes); err != nil {
		return State{}, fmt.Errorf("invalid OTA factory state: %w", err)
	}
	state := NewFactoryState(u.config.FactoryVersion, u.config.FactoryBuildTime, u.config.FactoryPartitionHashes)
	if err := SaveState(u.statePath(), state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (u *Updater) readABData() (ABData, error) {
	f, err := os.Open(u.miscPathForAccess())
	if err != nil {
		return ABData{}, err
	}
	defer f.Close()
	return ReadABData(f)
}

func (u *Updater) writeABDataFile(ab ABData) error {
	f, err := os.OpenFile(u.miscPathForAccess(), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	writeErr := WriteABDataAt(f, ab)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func (u *Updater) miscPathForAccess() string {
	if u.config.MiscPath == DefaultOTAMiscPath {
		return preferExistingPath(DefaultOTAMiscPath, LegacyOTAMiscPath)
	}
	return u.config.MiscPath
}

func (u *Updater) blockDirForAccess() string {
	if u.config.BlockDir == DefaultOTABlockDir {
		return preferExistingPath(DefaultOTABlockDir, LegacyOTABlockDir)
	}
	return u.config.BlockDir
}

func (u *Updater) storageDevicePathForAccess() string {
	if u.config.StorageDevicePath == DefaultOTAStorageDevicePath {
		return preferExistingPath(DefaultOTAStorageDevicePath, LegacyOTAStorageDevicePath)
	}
	return u.config.StorageDevicePath
}

func preferExistingPath(primary string, legacy string) string {
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return primary
}

func (u *Updater) publicKey() (ed25519.PublicKey, error) {
	if len(u.config.PublicKey) == ed25519.PublicKeySize {
		return u.config.PublicKey, nil
	}
	data, err := os.ReadFile(u.config.PublicKeyPath)
	if err != nil {
		return nil, err
	}
	return ParseEd25519PublicKeyPEM(data)
}

func sanitizeURLForLog(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "<malformed-url>"
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func deriveAssetURL(manifestURL, assetName string) (string, error) {
	parsed, err := url.Parse(manifestURL)
	if err != nil {
		return "", fmt.Errorf("invalid manifest URL: %w", err)
	}

	// Extract directory from manifest path
	// Example: /repos/owner/repo/releases/download/tag/manifest.json
	//       -> /repos/owner/repo/releases/download/tag/
	lastSlash := strings.LastIndex(parsed.Path, "/")
	if lastSlash < 0 {
		return "", fmt.Errorf("manifest URL has no directory component: %s", manifestURL)
	}
	baseDir := parsed.Path[:lastSlash+1]

	// Construct asset URL by replacing manifest filename with asset name
	parsed.Path = baseDir + assetName
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String(), nil
}

func (u *Updater) validateAssetFitsPartition(partName string, target Slot, asset ManifestAsset) error {
	if isCompressedImageAssetName(asset.Name) {
		return nil
	}
	blockName := partitionBlockName(partName, target)
	if max, ok := u.partitionSizes()[blockName]; ok && asset.Size > max {
		return fmt.Errorf("asset %s size %d is larger than partition %s size %d", asset.Name, asset.Size, blockName, max)
	}
	return nil
}

func partitionBlockName(partName string, target Slot) string {
	if target == SlotA {
		return partName + "_a"
	}
	return partName + "_b"
}

func (u *Updater) partitionSizes() map[string]int64 {
	sizes := map[string]int64{}
	for name, size := range DefaultProductionPartitionSizes {
		sizes[name] = size
	}
	for name, size := range u.config.PartitionSizes {
		sizes[name] = size
	}
	return sizes
}

func (u *Updater) fetchLatestReleaseAssets(parent context.Context, releaseURL string, token string) (map[string]string, error) {
	ctx, cancel := u.httpContext(parent)
	defer cancel()
	if managedChannel(u.config.Channel) {
		assets, err := FetchChannelReleaseAssets(ctx, releaseURL, u.config.Channel, token, u.config.GitHubProxyURL)
		return assets, describeTimeout(parent, err, "channel release metadata request", u.httpTimeout())
	}
	assets, err := FetchLatestReleaseAssetsWithProxy(ctx, releaseURL, token, u.config.GitHubProxyURL)
	return assets, describeTimeout(parent, err, "release metadata request", u.httpTimeout())
}

func (u *Updater) fetchBytesWithTokenLimit(parent context.Context, url string, token string, limit int64) ([]byte, error) {
	ctx, cancel := u.httpContext(parent)
	defer cancel()

	// Apply GitHub proxy if configured
	fetchURL := ApplyGitHubProxy(url, u.config.GitHubProxyURL)
	if fetchURL != url {
		_ = logging.LogEvent(logging.Info, "ota", "manifest", "proxy_enabled")
	}

	data, err := fetchBytesWithTokenLimit(ctx, fetchURL, token, limit)
	return data, describeTimeout(parent, err, "manifest request", u.httpTimeout())
}

func (u *Updater) downloadFileWithToken(parent context.Context, url string, dst string, expectedSize int64, token string) error {
	ctx, cancel := u.downloadContext(parent)
	defer cancel()
	err := DownloadFileWithOptions(ctx, url, dst, expectedSize, DownloadOptions{
		BearerToken:    token,
		GitHubProxyURL: u.config.GitHubProxyURL,
		Progress:       u.logDownloadProgress,
	})
	return describeTimeout(parent, err, "download of "+filepath.Base(dst), u.downloadTimeout())
}

func (u *Updater) githubToken() string {
	if u.config.GitHubToken != "" {
		return u.config.GitHubToken
	}
	data, err := os.ReadFile(u.config.GitHubTokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (u *Updater) statePath() string   { return filepath.Join(u.config.StateDir, "state.json") }
func (u *Updater) pendingPath() string { return filepath.Join(u.config.StateDir, "pending_boot.json") }
func (u *Updater) healthPath() string  { return filepath.Join(u.config.StateDir, "health.ok") }

func (u *Updater) recordError(phase string, err error) {
	state, loadErr := u.loadState()
	if loadErr != nil {
		return
	}
	state.Phase = phase
	state.LastError = err.Error()
	_ = SaveState(u.statePath(), state)
}

// cleanupOldDownloadCache keeps only verified assets and resumable partials
// needed for the selected target slot that are still part of the active plan.
func (u *Updater) cleanupOldDownloadCache(plan downloadPlan) error {
	downloadDir := u.config.DownloadDir
	if downloadDir == "" {
		return nil
	}

	keepFiles := make(map[string]bool)
	for _, planned := range plan.assets {
		if planned.archiveVerified {
			keepFiles[planned.asset.Name] = true
		}
		if planned.partialPresent {
			keepFiles[planned.asset.Name+".part"] = true
		}
	}

	// Read download directory
	entries, err := os.ReadDir(downloadDir)
	if os.IsNotExist(err) {
		// Directory doesn't exist, nothing to clean
		return nil
	}
	if err != nil {
		return fmt.Errorf("read download dir: %w", err)
	}

	// Check each file
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if keepFiles[name] {
			continue
		}

		// Delete old file
		path := filepath.Join(downloadDir, name)
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale OTA cache %s: %w", name, err)
		}
		logging.Infof("ota", "updater", "ota cleanup: removed old file %s", name)
	}

	return nil
}

var defaultOTAHTTPClient = newOTAHTTPClient()

func newOTAHTTPClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Transport: http.DefaultTransport}
	}
	clone := transport.Clone()
	clone.ResponseHeaderTimeout = DefaultHTTPResponseHeaderTimeout
	clone.Proxy = otaProxyFromEnvironment
	return &http.Client{Transport: clone}
}

func otaProxyFromEnvironment(req *http.Request) (*url.URL, error) {
	return netproxy.ProxyFromEnvironment(req, "http", "https", "socks5", "socks5h")
}

func (u *Updater) logDownloadProgress(progress DownloadProgress) {
	name := filepath.Base(progress.Path)
	amount := formatDownloadAmount(progress.Bytes, progress.Total)
	if progress.Complete {
		logging.Infof("ota", "updater", "ota download: %s complete %s", name, amount)
		return
	}
	resume := ""
	if progress.ResumedFrom > 0 {
		resume = fmt.Sprintf(" resumed_from=%s", formatBytes(progress.ResumedFrom))
	}
	percent := ""
	if progress.Total > 0 {
		percent = fmt.Sprintf(" (%d%%)", progress.Bytes*100/progress.Total)
	}
	logging.Infof("ota", "updater", "ota download: %s progress %s%s%s", name, amount, percent, resume)
}

func (u *Updater) logWriteProgress(progress WriteProgress) {
	amount := formatDownloadAmount(progress.Bytes, progress.Total)
	if progress.Complete {
		logging.Infof("ota", "updater", "ota write: %s -> %s complete %s", progress.Part, progress.BlockName, amount)
		return
	}
	percent := ""
	if progress.Total > 0 {
		percent = fmt.Sprintf(" (%d%%)", progress.Bytes*100/progress.Total)
	}
	logging.Infof("ota", "updater", "ota write: %s -> %s progress %s%s", progress.Part, progress.BlockName, amount, percent)
}

func formatDownloadAmount(bytes int64, total int64) string {
	if total >= 0 {
		return fmt.Sprintf("%s/%s", formatBytes(bytes), formatBytes(total))
	}
	return formatBytes(bytes)
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		return "unknown"
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, unit := range []string{"KiB", "MiB", "GiB"} {
		value /= 1024
		if value < 1024 || unit == "GiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return fmt.Sprintf("%.1f GiB", value)
}

func slotLogName(slot Slot) string {
	name, err := slotName(slot)
	if err != nil {
		return fmt.Sprint(slot)
	}
	return name
}

func logValue(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func requiredAssetURL(assets map[string]string, name string) (string, error) {
	url, ok := assets[name]
	if !ok || url == "" {
		return "", fmt.Errorf("missing required release asset %s", name)
	}
	return url, nil
}

func fetchBytesWithTokenLimit(ctx context.Context, url string, bearerToken string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := defaultOTAHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch %s status %d", url, resp.StatusCode)
	}
	if limit < 0 {
		return io.ReadAll(resp.Body)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("manifest too large: exceeds %d bytes", limit)
	}
	return data, nil
}

func targetNameForState(slot Slot) string {
	name, _ := slotName(slot)
	return name
}

func isNoUpdate(state State, manifest Manifest) bool {
	return state.LastCommittedVersion != "" &&
		state.LastCommittedBuildTime != "" &&
		manifest.Version == state.LastCommittedVersion &&
		manifest.BuildTime == state.LastCommittedBuildTime
}

func inactiveSlot(active Slot) Slot {
	if active == SlotA {
		return SlotB
	}
	return SlotA
}

func parseSlotName(name string) (Slot, error) {
	switch strings.TrimPrefix(strings.ToLower(name), "_") {
	case "a":
		return SlotA, nil
	case "b":
		return SlotB, nil
	default:
		return SlotA, fmt.Errorf("invalid slot %q", name)
	}
}

func currentSlotFromProcCmdline() (Slot, bool, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		if os.IsNotExist(err) {
			return SlotA, false, nil
		}
		return SlotA, false, err
	}
	return currentSlotFromCmdline(string(data))
}

func currentRootSlotFromProcCmdline() (Slot, bool, error) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		if os.IsNotExist(err) {
			return SlotA, false, nil
		}
		return SlotA, false, err
	}
	return rootSlotFromCmdline(string(data))
}

func currentSlotFromCmdline(cmdline string) (Slot, bool, error) {
	for _, field := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(field, "aico.slot_suffix=")
		if !ok {
			continue
		}
		slot, err := parseSlotName(value)
		if err != nil {
			return SlotA, false, err
		}
		return slot, true, nil
	}
	return SlotA, false, nil
}

func rootSlotFromCmdline(cmdline string) (Slot, bool, error) {
	for _, field := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(field, "root=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.ToLower(value), "\"'")
		switch {
		case value == "partlabel=rootfs_a" || value == "rootfs_a" || strings.HasSuffix(value, "/rootfs_a") || value == "/dev/mmcblk1p7":
			return SlotA, true, nil
		case value == "partlabel=rootfs_b" || value == "rootfs_b" || strings.HasSuffix(value, "/rootfs_b") || value == "/dev/mmcblk1p8":
			return SlotB, true, nil
		default:
			return SlotA, false, fmt.Errorf("unsupported root device %q", value)
		}
	}
	return SlotA, false, nil
}

func copySlotPartitionInfo(info SlotPartitionInfo) SlotPartitionInfo {
	if len(info.Partitions) == 0 {
		return SlotPartitionInfo{}
	}
	parts := make(map[string]PartitionVersion, len(info.Partitions))
	for name, version := range info.Partitions {
		parts[name] = version
	}
	return SlotPartitionInfo{Partitions: parts}
}

func generateNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func currentBootID() string {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// fetchUpdateManifest is shared by availability checks and installation. Only
// signed, channel-compatible manifests may influence an update decision.
func (u *Updater) fetchUpdateManifest(ctx context.Context) (Manifest, map[string]string, error) {
	var assetsByName map[string]string
	var manifestBytes []byte
	token := u.githubToken()
	var err error

	if u.config.ManifestURL != "" {
		logging.Infof("ota", "updater", "ota manifest: downloading from direct URL %s", sanitizeURLForLog(u.config.ManifestURL))
		directToken := ""
		if isGitHubURL(u.config.ManifestURL) {
			directToken = token
		}
		manifestBytes, err = u.fetchBytesWithTokenLimit(ctx, u.config.ManifestURL, directToken, MaxRemoteManifestBytes)
		if err != nil {
			return Manifest{}, nil, err
		}
	} else {
		releaseURL, endpointErr := releaseEndpoint(u.config)
		if endpointErr != nil {
			return Manifest{}, nil, endpointErr
		}
		logging.Infof("ota", "updater", "ota release: fetching %s", releaseURL)
		assetsByName, err = u.fetchLatestReleaseAssets(ctx, releaseURL, token)
		if err != nil {
			return Manifest{}, nil, err
		}
		logging.Infof("ota", "updater", "ota release: found %d assets", len(assetsByName))
		manifestURL, err := requiredAssetURL(assetsByName, "manifest.json")
		if err != nil {
			return Manifest{}, nil, err
		}
		logging.Infof("ota", "updater", "ota manifest: downloading manifest.json from %s", manifestURL)
		manifestBytes, err = u.fetchBytesWithTokenLimit(ctx, manifestURL, token, MaxRemoteManifestBytes)
		if err != nil {
			return Manifest{}, nil, err
		}
	}
	publicKey, err := u.publicKey()
	if err != nil {
		return Manifest{}, nil, err
	}
	manifest, err := VerifyManifestJSON(manifestBytes, publicKey)
	if err != nil {
		return Manifest{}, nil, err
	}
	if err := requireManifestChannel(u.config.Channel, manifest); err != nil {
		return Manifest{}, nil, err
	}
	if u.config.DebianMode {
		if err := requireAtomicProductionManifest(manifest); err != nil {
			return Manifest{}, nil, err
		}
	}
	logging.Infof("ota", "updater", "ota manifest: verified version=%s channel=%s build_time=%s parts=%d", manifest.Version, logValue(manifest.Channel, "<unset>"), manifest.BuildTime, len(manifest.Parts))
	return manifest, assetsByName, nil
}

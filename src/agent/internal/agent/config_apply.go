package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// ConfigPrepareFunc drains input sessions and prepares replacement components.
// finish(true) publishes them after Runtime commits; finish(false) resumes the
// old components on failure. It must not mutate the active config itself.
type ConfigPrepareFunc func(context.Context, Config) (finish func(bool), err error)

type ConfigApplyStatus struct {
	EnvironmentRevision string `json:"environment_revision,omitempty"`
	Revision            uint64 `json:"revision"`
	AppliedRevision     uint64 `json:"applied_revision"`
	State               string `json:"state"`
	Applied             bool   `json:"applied"`
	Pending             bool   `json:"pending"`
	RebootRequired      bool   `json:"reboot_required"`
	Error               string `json:"error,omitempty"`
	// RuntimeID identifies the Agent process reporting this status.
	RuntimeID string `json:"runtime_id,omitempty"`
}

func (r *Runtime) SetConfigPreparer(prepare ConfigPrepareFunc) {
	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()
	r.configPrepare = prepare
}

func (r *Runtime) toolSnapshot() *ToolSet {
	if r == nil {
		return nil
	}
	r.configMu.RLock()
	defer r.configMu.RUnlock()
	return r.tools
}

func (r *Runtime) ConfigApplyStatus() ConfigApplyStatus {
	r.configStatusMu.Lock()
	status := r.configStatus
	r.configStatusMu.Unlock()
	// runtimeID is assigned once at construction and never mutated; attach it
	// on read so every report identifies the answering process.
	status.RuntimeID = r.runtimeID
	return status
}

// InitializeConfigApplication records the persisted revision at daemon startup,
// including USB changes that still await a device reboot.
func (r *Runtime) InitializeConfigApplication(persisted Config) {
	revision := configFileRevision(filepath.Join(persisted.ConfigDir, "agent.toml"))
	r.configStatusMu.Lock()
	defer r.configStatusMu.Unlock()
	r.configStatus = ConfigApplyStatus{Revision: revision, AppliedRevision: revision, State: "applied", Applied: true}
	if configRequiresReboot(r.ConfigSnapshot(), persisted) {
		r.configStatus.RebootRequired = true
		r.configStatus.Applied = false
		r.configStatus.AppliedRevision = 0
		r.configStatus.State = "reboot_required"
	}
}

// QueueConfig coalesces saves while a voice session or tool operation drains.
// The HTTP request never has to outlive the running user task.
func (r *Runtime) QueueConfig(cfg Config, revision uint64) ConfigApplyStatus {
	r.configStatusMu.Lock()
	defer r.configStatusMu.Unlock()
	if r.configClosed {
		return ConfigApplyStatus{State: "failed", Error: "runtime is closing"}
	}
	r.configPending = &cfg
	r.configSequence++
	r.configStatus.Revision = revision
	r.configStatus.State = "pending"
	r.configStatus.Pending = true
	r.configStatus.Applied = false
	r.configStatus.Error = ""
	r.configStatus.RebootRequired = configRequiresReboot(r.ConfigSnapshot(), cfg)
	if !r.configWorkerRunning {
		r.configWorkerRunning = true
		r.configWorkerWG.Add(1)
		go r.applyConfigWorker()
	}
	return r.configStatus
}

// applyConfigWorker drains queued configuration snapshots one at a time and
// records the outcome of each application.
func (r *Runtime) applyConfigWorker() {
	defer r.configWorkerWG.Done()
	for {
		r.configStatusMu.Lock()
		if r.configPending == nil || r.configClosed {
			r.configWorkerRunning = false
			r.configStatusMu.Unlock()
			return
		}
		cfg, revision, sequence := *r.configPending, r.configStatus.Revision, r.configSequence
		r.configPending = nil
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		r.configWorkerCancel = cancel
		r.configStatusMu.Unlock()
		started := time.Now()
		err := r.applyConfig(ctx, cfg)
		elapsed := time.Since(started)
		cancel()
		r.configStatusMu.Lock()
		r.configWorkerCancel = nil
		// Resolve the reboot outcome once, for the snapshot this iteration
		// applied. A queued revision may publish its own status as soon as the
		// lock is released, so the value must not be re-read for the log.
		rebootRequired := configRequiresReboot(r.ConfigSnapshot(), cfg)
		if err == nil && !rebootRequired {
			r.configStatus.AppliedRevision = revision
		}
		if r.configSequence == sequence {
			r.configStatus.Pending = false
			r.configStatus.Applied = err == nil
			r.configStatus.State = "applied"
			if err != nil {
				r.configStatus.State = "failed"
				r.configStatus.Error = err.Error()
			}
			r.configStatus.RebootRequired = rebootRequired
			if err == nil && rebootRequired {
				r.configStatus.State = "reboot_required"
				r.configStatus.Applied = false
			}
		}
		r.configStatusMu.Unlock()
		r.logConfigApply(revision, err, elapsed, rebootRequired)
	}
}

// logConfigApply records the outcome of an online configuration application.
// Without it agent.log stays silent about successful reloads, so "the setting
// did not take effect" cannot be told apart from "the save never arrived".
// rebootRequired describes the revision being logged, never a newer queue
// entry, so each event stays consistent with its own revision.
func (r *Runtime) logConfigApply(revision uint64, applyErr error, elapsed time.Duration, rebootRequired bool) {
	if r == nil || r.logger == nil {
		return
	}
	if applyErr != nil {
		r.logger.WarnEvent("runtime", "config_apply_failed",
			LogField{Key: "revision", Value: revision},
			LogField{Key: "duration_ms", Value: elapsed.Milliseconds()},
			LogField{Key: "error", Value: applyErr},
		)
		return
	}
	r.logger.InfoEvent("runtime", "config_applied",
		LogField{Key: "revision", Value: revision},
		LogField{Key: "duration_ms", Value: elapsed.Milliseconds()},
		LogField{Key: "reboot_required", Value: rebootRequired},
	)
}

func configRequiresReboot(current, next Config) bool {
	level, _ := ConfigApplyLevel(current, next)
	return level == ApplyReboot
}

// ApplyConfigSnapshot is synchronous for embedders. HTTP callers use QueueConfig.
func (r *Runtime) ApplyConfigSnapshot(cfg Config) error {
	return r.applyConfig(context.Background(), cfg)
}

func (r *Runtime) applyConfig(ctx context.Context, cfg Config) error {
	if r == nil {
		return fmt.Errorf("runtime unavailable")
	}
	r.configReloadMu.Lock()
	defer r.configReloadMu.Unlock()
	current := r.ConfigSnapshot()
	if current.ConfigDir != cfg.ConfigDir {
		return fmt.Errorf("config directory cannot be reloaded")
	}
	// Keep the host USB session consistent until reboot; unrelated fields can apply.
	if current.PointerModeOrDefault() != cfg.PointerModeOrDefault() {
		cfg.Device.DeviceType = current.Device.DeviceType
		cfg.HID.PointerMode = current.HID.PointerMode
	}
	if current.HID.KeyboardLayoutOrDefault() != cfg.HID.KeyboardLayoutOrDefault() {
		cfg.HID.KeyboardLayout = current.HID.KeyboardLayout
	}
	if reflect.DeepEqual(current, cfg) {
		return nil
	}
	var finish func(bool)
	if r.configPrepare != nil {
		var err error
		finish, err = r.configPrepare(ctx, cfg)
		if err != nil {
			return err
		}
	}
	committed := false
	if finish != nil {
		defer func() {
			if !committed {
				finish(false)
			}
		}()
	}
	unlock, err := r.lockRun(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	r.configOperations.Lock()
	defer r.configOperations.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if current.TimezoneOrDefault() != cfg.TimezoneOrDefault() {
		previousTimezone, hadPreviousTimezone := os.LookupEnv("TZ")
		if err := ApplyTimezone(cfg); err != nil {
			return fmt.Errorf("configure timezone: %w", err)
		}
		defer func() {
			if committed {
				return
			}
			var rollbackErr error
			if hadPreviousTimezone {
				rollbackErr = os.Setenv("TZ", previousTimezone)
			} else {
				rollbackErr = os.Unsetenv("TZ")
			}
			if rollbackErr != nil && r.logger != nil {
				r.logger.Warn("roll back timezone environment: %v", rollbackErr)
			}
		}()
	}
	oldTools := r.toolSnapshot()
	tools := oldTools
	hardwareChanged := !reflect.DeepEqual(current.HID, cfg.HID) || current.Device != cfg.Device || current.Audio != cfg.Audio || current.Search != cfg.Search || current.ScreenStableDefaults() != cfg.ScreenStableDefaults()
	if hardwareChanged && oldTools != nil {
		tools = NewBuiltinToolSetFromConfig(cfg, ProxyConfigFromEnvironment(), WithScreenState(r.screenState), WithWaitForWakeupController(r.waitForWakeup), WithScreenStableDefaults(cfg.ScreenStableDefaults()), WithShellTemporaryDirectory(managedPythonTmp))
		defer func() {
			if !committed {
				tools.closeInputDevices()
			}
		}()
		tools.RegisterEnterTextTool(r.models, r.deviceTypeFromState)
		tools.SetRuntimeDeviceTypeFn(r.deviceTypeFromState)
		tools.SetDecisionConfigFn(r.decisionConfig)
		// Preserve registered memory/skill tools and their live state.
		for name, tool := range oldTools.tools {
			if _, exists := tools.tools[name]; !exists {
				tools.tools[name] = tool
			}
		}
		// These tools have no reloadable hardware dependencies; shell in
		// particular owns background sessions that must survive a HID change.
		for _, name := range []string{"shell", "weather", "wikipedia", "web_scraper", "request_user_action"} {
			if tool, ok := oldTools.tools[name]; ok {
				tools.tools[name] = tool
			}
		}
	}
	var modelCommit func()
	if !reflect.DeepEqual(current.Model, cfg.Model) {
		if manager, ok := r.models.(*ModelManager); ok {
			modelCommit, err = manager.prepareReplacement(cfg.Model, cfg.ConfigDir)
			if err != nil {
				return fmt.Errorf("configure model: %w", err)
			}
		}
	}
	if hardwareChanged && oldTools != nil {
		if guard, ok := oldTools.mnkProvider.(interface{ CheckConfigReload() error }); ok {
			if err := guard.CheckConfigReload(); err != nil {
				return err
			}
		}
	}
	if !reflect.DeepEqual(buildTTSProviderConfig(current), buildTTSProviderConfig(cfg)) {
		manager := r.ttsProviderManager()
		if cfg.TTS.Provider == "" {
			err = manager.Disable()
		} else {
			err = manager.SwitchTo(buildTTSProviderConfig(cfg))
		}
		if err != nil {
			return fmt.Errorf("configure TTS: %w", err)
		}
	}
	if modelCommit != nil {
		modelCommit()
	}
	if !reflect.DeepEqual(current.Model, cfg.Model) {
		if merge, ok := current.SkillMergeModel.(*LLMSkillMergeModel); ok {
			if manager, ok := merge.model.(*ModelManager); ok && manager != r.models {
				manager.replaceConfig(cfg.Model, cfg.ConfigDir)
			}
		}
	}
	if r.storageMonitor != nil && (!reflect.DeepEqual(current.Storage, cfg.Storage) || current.AudioArchive != cfg.AudioArchive || !reflect.DeepEqual(current.VoiceNotifications, cfg.VoiceNotifications)) {
		monitorConfig, cleaners := runtimeStorageMonitorParts(cfg)
		r.storageMonitor.Reconfigure(monitorConfig, cleaners)
	}
	if r.storage != nil {
		r.storage.reconfigureStateView(cfg.Storage)
	}
	if r.voiceNotifications != nil {
		r.voiceNotifications.Reconfigure(cfg.VoiceNotifications, resolvedVoiceNotificationLocale(cfg))
	}
	if current.Log != cfg.Log {
		if cfg.ConfigDir != "" {
			if err := cleanupOldLogFiles(filepath.Join(cfg.ConfigDir, "log"), time.Now(), cfg.Log.LLMHTTPRetentionDaysOrDefault()); err != nil && r.logger != nil {
				r.logger.Warn("apply log retention: %v", err)
			}
		}
		if r.logger != nil {
			r.logger.SetLevel(cfg.Log.LevelOrDefault())
		}
	}
	if tools != oldTools && r.phoneBridge != nil {
		tools.RegisterPhoneBridge(r.phoneBridge)
	}
	r.configMu.Lock()
	r.config = cfg
	r.tools = tools
	r.configMu.Unlock()
	if current.HID != cfg.HID || current.Device != cfg.Device {
		r.screenState.InvalidateCapture()
	}
	if !reflect.DeepEqual(current.SkillsDirs, cfg.SkillsDirs) {
		r.MarkSkillsDirty()
	}
	if r.phoneBridge != nil {
		r.phoneBridge.SetConfiguredPlatform(cfg.DevicePlatformOrDefault())
	}
	if tools != oldTools && oldTools != nil {
		oldTools.closeInputDevices()
	}
	// InitializeContextManager rotates the append-only session when its system
	// prompt changes. Model/provider changes also drop provider-specific chaining.
	if current.Locale != cfg.Locale || current.TimezoneOrDefault() != cfg.TimezoneOrDefault() || current.Instruction != cfg.Instruction || current.Prompt != cfg.Prompt || current.Model.Provider != cfg.Model.Provider || current.Model.Model != cfg.Model.Model || current.Model.APIMode != cfg.Model.APIMode || current.Model.BaseURL != cfg.Model.BaseURL {
		r.configContextRotate.Store(true)
		r.configUserContextRotate.Store(true)
	}
	if !reflect.DeepEqual(current.VoiceModel, cfg.VoiceModel) {
		r.configUserContextRotate.Store(true)
	}
	committed = true
	if finish != nil {
		finish(true)
	}
	return nil
}

// StopConfigReloads cancels an application waiting for an idle input session.
// Call before stopping the input lifecycle, whose preparation hook may be active.
func (r *Runtime) StopConfigReloads() {
	r.configStatusMu.Lock()
	r.configClosed = true
	r.configPending = nil
	if r.configWorkerCancel != nil {
		r.configWorkerCancel()
	}
	r.configStatusMu.Unlock()
	r.configWorkerWG.Wait()
}

// PrepareUserContext preserves old realtime transcripts when the provider or
// system prompt changes and prevents replaying incompatible continuation data.
func (r *Runtime) PrepareUserContext(systemPrompt string) error {
	if r == nil || !r.configUserContextRotate.Load() {
		return nil
	}
	if err := r.rotateUserContext(systemPrompt); err != nil {
		return err
	}
	r.configUserContextRotate.Store(false)
	return nil
}

package main

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/logging"
	"aiden-agent/internal/mcu"
)

// inputLifecycle owns the input listeners and voice loop, independently of the
// HTTP server. A reload drains the current session before replacing the loop.
type inputLifecycle struct {
	mu              sync.Mutex
	dialogMu        sync.Mutex
	dialog          *agent.AudioDialog
	runtime         *agent.Runtime
	server          *agent.Server
	cfg             agent.Config
	stop            chan struct{}
	shutdown        chan os.Signal
	done            chan struct{}
	quick           wakeupWatcher
	closed          bool
	shutdownClosed  bool
	quiescing       atomic.Bool
	newDialog       func(agent.Config) (*agent.AudioDialog, error)
	runVoice        func(agent.Config, *agent.AudioDialog, chan os.Signal, <-chan struct{})
	newWatcher      wakeupWatcherFactory
	newVoiceWatcher voiceWakeupWatcherFactory
}

func newInputLifecycle(runtime *agent.Runtime, server *agent.Server) *inputLifecycle {
	source := newMCUWakeupSource(mcu.Client{Socket: strings.TrimSpace(os.Getenv("AIDEN_MCU_SERVICE_SOCKET"))}, defaultMCUVoiceCursorPath)
	c := &inputLifecycle{runtime: runtime, server: server, newWatcher: newGPIOWatcher, newVoiceWatcher: source.newWatcher}
	runtime.RegisterPreemptHook(func() {
		c.dialogMu.Lock()
		defer c.dialogMu.Unlock()
		if c.dialog != nil {
			c.dialog.InterruptOutput()
			if c.dialog.RecordingActive() {
				_ = c.dialog.StopRecording()
			}
		}
	})
	return c
}

func voiceConfig(cfg agent.Config) agent.Config {
	// Runtime replaces the ordinary model at the next Agent task boundary.
	// Including it here would wait for an unrelated, potentially long-lived
	// voice session before the new provider can serve any task.
	return agent.Config{
		HID: cfg.HID, Device: cfg.Device, Search: cfg.Search,
		ScreenStableTimeoutMs: cfg.ScreenStableTimeoutMs, ScreenStableMs: cfg.ScreenStableMs, ScreenStableDiffThreshold: cfg.ScreenStableDiffThreshold,
		InputMode: cfg.InputMode, Audio: cfg.Audio, STT: cfg.STT, TTS: cfg.TTS, VoiceModel: cfg.VoiceModel,
		Locale: cfg.Locale, Timezone: cfg.Timezone, Instruction: cfg.Instruction, Prompt: cfg.Prompt,
		AudioArchive: cfg.AudioArchive, VADBackend: cfg.VADBackend, VADModelPath: cfg.VADModelPath, VADHelperPath: cfg.VADHelperPath,
		VADSpeechThreshold: cfg.VADSpeechThreshold, SilenceMs: cfg.SilenceMs, MinSpeechMs: cfg.MinSpeechMs,
		VoiceFollowupEnabled: cfg.VoiceFollowupEnabled, VoiceFollowupTimeoutMs: cfg.VoiceFollowupTimeoutMs,
		VoiceFirstTurnTimeoutMs: cfg.VoiceFirstTurnTimeoutMs, VoiceMaxTurns: cfg.VoiceMaxTurns,
		VoiceInterruptOnWakeup: cfg.VoiceInterruptOnWakeup, VoiceStreamingTTSEnabled: cfg.VoiceStreamingTTSEnabled,
		VoiceToolCallSpeech: cfg.VoiceToolCallSpeech, VoiceProgressSpeechEnabled: cfg.VoiceProgressSpeechEnabled,
		VoiceMaxResponseTokens: cfg.VoiceMaxResponseTokens,
	}
}

func activeVoiceConfig(cfg agent.Config) agent.Config {
	active := voiceConfig(cfg)
	active.InputMode = cfg.InputModeOrDefault()
	switch active.InputMode {
	case "stt":
		active.VoiceModel = agent.VoiceModelConfig{}
	case "realtime":
		// These clients are published independently by Server and Runtime.
		// Editing them must not wait for a live realtime conversation to end.
		active.STT = agent.STTConfig{}
		active.TTS = agent.TTSConfig{}
	default:
		active.STT = agent.STTConfig{}
		active.TTS = agent.TTSConfig{}
		active.VoiceModel = agent.VoiceModelConfig{}
	}
	return active
}

func (c *inputLifecycle) buildDialog(cfg agent.Config) (*agent.AudioDialog, error) {
	if c.newDialog != nil {
		return c.newDialog(cfg)
	}
	if cfg.InputModeOrDefault() != "stt" {
		return nil, nil
	}
	dialog, err := agent.NewAudioDialogWithConfig(c.runtime, cfg)
	if err != nil {
		return nil, err
	}
	dialog.SetMessagePublisher(c.server.BroadcastMessage)
	dialog.SetStorageManager(c.runtime.Storage())
	dialog.SetStorageMonitor(c.runtime.StorageMonitor())
	return dialog, nil
}

func (c *inputLifecycle) startVoice(cfg agent.Config, dialog *agent.AudioDialog) {
	c.cfg = cfg
	c.stop = make(chan struct{})
	c.shutdown = make(chan os.Signal)
	c.shutdownClosed = false
	c.done = make(chan struct{})
	stop, shutdown, done := c.stop, c.shutdown, c.done
	c.dialogMu.Lock()
	c.dialog = dialog
	c.dialogMu.Unlock()
	go func() {
		defer close(done)
		if c.runVoice != nil {
			c.runVoice(cfg, dialog, shutdown, stop)
			return
		}
		switch cfg.InputModeOrDefault() {
		case "stt":
			runWakeupMode(cfg, dialog, c.runtime, shutdown, c.newVoiceWatcher, stop)
		case "realtime":
			tasks := newRealtimeAgentTaskManager(cfg, c.runtime)
			if tasks != nil {
				defer tasks.Close()
			}
			runRealtimeWakeupModeWithServer(cfg, shutdown, c.server, c.runtime, tasks, c.newVoiceWatcher, stop)
		default:
			select {
			case <-stop:
			case <-shutdown:
			}
		}
	}()
}

func (c *inputLifecycle) Start(cfg agent.Config) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	dialog, err := c.buildDialog(cfg)
	if err != nil {
		return err
	}
	quick, err := startQuickCaptureGPIOWatcher(cfg, c.server, c.newWatcher)
	if err != nil {
		logging.Warnf("agent", "quick_capture", "GPIO trigger disabled: %v", err)
	}
	c.quick = quick
	c.startVoice(cfg, dialog)
	return nil
}

func (c *inputLifecycle) Prepare(ctx context.Context, cfg agent.Config) (func(bool), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("input controller is closing")
	}
	serverFinish, err := c.server.PrepareConfig(ctx, cfg)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	old := c.cfg
	c.dialogMu.Lock()
	oldDialog := c.dialog
	c.dialogMu.Unlock()
	voiceChanged := !reflect.DeepEqual(activeVoiceConfig(old), activeVoiceConfig(cfg))
	quickChanged := old.QuickCapture.GPIOPin != cfg.QuickCapture.GPIOPin || old.QuickCapture.EnabledOrDefault() != cfg.QuickCapture.EnabledOrDefault()
	if voiceChanged {
		close(c.stop)
		select {
		case <-c.done:
		case <-ctx.Done():
			c.shutdownVoice()
			<-c.done
			if !c.quiescing.Load() {
				// Timeout rollback: the runtime keeps serving, so the drained
				// loop must resume. During shutdown the loop stays stopped and
				// Close finishes tearing it down.
				c.startVoice(old, oldDialog)
			}
			serverFinish(false)
			c.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	var dialog *agent.AudioDialog
	var commitInput func()
	if voiceChanged {
		dialog, err = c.buildDialog(cfg)
		if err == nil {
			commitInput, err = dialog.PrepareInputReplacement(oldDialog)
		}
	}
	var quick wakeupWatcher
	var quickReady atomic.Bool
	if err == nil && quickChanged {
		// Reserve the new GPIO before committing, but ignore events until the
		// runtime snapshot and clients have been published.
		quick, err = startQuickCaptureGPIOWatcher(cfg, c.server, func(pin int, callback func()) (wakeupWatcher, error) {
			return c.newWatcher(pin, func() {
				if quickReady.Load() {
					callback()
				}
			})
		})
	}
	restore := func() {
		if dialog != nil {
			_ = dialog.Close()
		}
		if quick != nil {
			quick.Stop()
		}
		if voiceChanged {
			// The drained dialog still owns its original VAD and clients. Keep
			// it intact until commit so rollback never needs to initialize again.
			c.startVoice(old, oldDialog)
		}
	}
	if err != nil {
		serverFinish(false)
		restore()
		c.mu.Unlock()
		return nil, fmt.Errorf("prepare input components: %w", err)
	}
	return func(commit bool) {
		defer c.mu.Unlock()
		serverFinish(commit)
		if !commit {
			restore()
			return
		}
		if quickChanged {
			if c.quick != nil {
				c.quick.Stop()
			}
			c.quick = quick
			quickReady.Store(true)
		}
		if voiceChanged {
			c.dialogMu.Lock()
			c.dialog = nil
			if commitInput != nil {
				commitInput()
			}
			c.dialogMu.Unlock()
			if oldDialog != nil {
				_ = oldDialog.Close()
			}
			c.startVoice(cfg, dialog)
		} else {
			c.cfg = cfg
		}
	}, nil
}

// StopForShutdown marks the lifecycle as closing so a configuration
// preparation cancelled by Runtime.StopConfigReloads leaves the drained voice
// loop stopped instead of restarting it. Call before StopConfigReloads.
func (c *inputLifecycle) StopForShutdown() {
	c.quiescing.Store(true)
}

func (c *inputLifecycle) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.quick != nil {
		c.quick.Stop()
	}
	if c.shutdown != nil {
		c.shutdownVoice()
		<-c.done
	}
	c.dialogMu.Lock()
	dialog := c.dialog
	c.dialog = nil
	c.dialogMu.Unlock()
	if dialog != nil {
		_ = dialog.Close()
	}
}

func reloadStop(channels []<-chan struct{}) <-chan struct{} {
	if len(channels) > 0 {
		return channels[0]
	}
	return nil
}
func reloadRequested(channels []<-chan struct{}) bool {
	select {
	case <-reloadStop(channels):
		return true
	default:
		return false
	}
}

func (c *inputLifecycle) shutdownVoice() {
	if c.shutdown != nil && !c.shutdownClosed {
		close(c.shutdown)
		c.shutdownClosed = true
	}
}

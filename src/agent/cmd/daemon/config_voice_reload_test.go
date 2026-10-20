package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"aiden-agent/internal/agent"
)

func TestActiveVoiceConfigTracksGPIOWakeupEnabled(t *testing.T) {
	base := agent.DefaultConfig()
	disabled := false
	changed := base
	changed.GPIOWakeupEnabled = &disabled
	if reflect.DeepEqual(activeVoiceConfig(base), activeVoiceConfig(changed)) {
		t.Fatal("gpio_wakeup_enabled change did not change active voice lifecycle")
	}
}

func TestActiveVoiceConfigIgnoresInactiveProviderSettings(t *testing.T) {
	base := agent.DefaultConfig()
	base.InputMode = "realtime"
	changed := base
	changed.STT.Provider = "openai-whisper"
	changed.TTS.Provider = "fish-audio"
	if !reflect.DeepEqual(activeVoiceConfig(base), activeVoiceConfig(changed)) {
		t.Fatal("inactive STT/TTS settings changed realtime voice lifecycle")
	}

	base.InputMode = "stt"
	changed = base
	changed.VoiceModel.Provider = "gemini"
	if !reflect.DeepEqual(activeVoiceConfig(base), activeVoiceConfig(changed)) {
		t.Fatal("inactive realtime settings changed STT voice lifecycle")
	}

	changed.InputMode = "realtime"
	if reflect.DeepEqual(activeVoiceConfig(base), activeVoiceConfig(changed)) {
		t.Fatal("input mode change did not change active voice lifecycle")
	}
}

// Startup leaves VAD lazy. A provider-only save must not add a new hardware
// prerequisite, even when the driver is unavailable before the first recording.
func TestInputLifecycleProviderReloadDoesNotInitializeUnchangedVAD(t *testing.T) {
	for _, field := range []string{"tts", "stt", "realtime"} {
		t.Run(field, func(t *testing.T) {
			cfg := agent.DefaultConfig()
			cfg.ConfigDir = t.TempDir()
			cfg.Model.Provider, cfg.Model.Model = "fake", "fake"
			cfg.InputMode, cfg.STT.Provider, cfg.TTS.Provider = "stt", "", ""
			cfg.QuickCapture.Enabled = new(bool)
			cfg.VADHelperPath = filepath.Join(t.TempDir(), "vad-unavailable")
			if err := os.WriteFile(cfg.VADHelperPath, []byte("#!/bin/sh\nprintf 'ERR encoder rknn_init failed: -1\\n'\n"), 0755); err != nil {
				t.Fatal(err)
			}
			r, err := agent.NewRuntime(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			s := agent.NewServer(r, ":0")
			defer s.Close()
			c := newInputLifecycle(r, s)
			c.runVoice = func(_ agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, stop <-chan struct{}) {
				select {
				case <-stop:
				case <-shutdown:
				}
			}
			if err := c.Start(cfg); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			r.SetConfigPreparer(c.Prepare)
			next := cfg
			switch field {
			case "tts":
				next.TTS.Provider, next.TTS.APIKey = "fish-audio", "test-key"
			case "stt":
				next.STT.Provider, next.STT.APIKey = "openai", "test-key"
			case "realtime":
				next.VoiceModel.Provider, next.VoiceModel.APIKey = "gemini", "test-key"
			}
			if err := r.ApplyConfigSnapshot(next); err != nil {
				t.Fatalf("provider-only reload attempted VAD initialization: %v", err)
			}
			if got := r.ConfigSnapshot(); got.TTS != next.TTS || got.STT != next.STT || got.VoiceModel != next.VoiceModel {
				t.Fatal("new provider not published")
			}
		})
	}
}

func TestInputLifecycleProviderReloadPreservesWorkingVAD(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			cfg := agent.DefaultConfig()
			cfg.ConfigDir = t.TempDir()
			cfg.Model.Provider, cfg.Model.Model = "fake", "fake"
			cfg.InputMode, cfg.STT.Provider, cfg.TTS.Provider = "stt", "", ""
			cfg.QuickCapture.Enabled = new(bool)
			cfg.VADHelperPath = filepath.Join(t.TempDir(), "vad-helper")
			t.Setenv("AIDEN_TEST_VAD_STARTED", filepath.Join(t.TempDir(), "started"))
			// A second helper cannot start: post-reload inference must use the
			// existing process, even after the old or staged dialog is closed.
			const script = `#!/bin/sh
if [ -e "$AIDEN_TEST_VAD_STARTED" ]; then
  printf 'ERR helper started again\n'
  exit 1
fi
printf 'started\n' > "$AIDEN_TEST_VAD_STARTED"
printf 'READY\n'
while op=$(dd bs=1 count=1 2>/dev/null); do
  case "$op" in
    F) dd bs=1 count=1024 of=/dev/null 2>/dev/null; printf 'P 0.9\n' ;;
    R) printf 'OK\n' ;;
    Q|"") exit 0 ;;
    *) exit 1 ;;
  esac
done
`
			if err := os.WriteFile(cfg.VADHelperPath, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			r, err := agent.NewRuntime(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			s := agent.NewServer(r, ":0")
			defer s.Close()
			c := newInputLifecycle(r, s)
			c.runVoice = func(_ agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, stop <-chan struct{}) {
				select {
				case <-stop:
				case <-shutdown:
				}
			}
			if err := c.Start(cfg); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			r.SetConfigPreparer(c.Prepare)
			previous := c.dialog
			frame := make([]int16, previous.VADFrameSamples())
			if _, err := previous.ProcessVADFrame(frame); err != nil {
				t.Fatalf("start original VAD: %v", err)
			}
			next := cfg
			next.STT.Provider, next.STT.APIKey = "openai", "test-key"
			if commit {
				if err := r.ApplyConfigSnapshot(next); err != nil {
					t.Fatal(err)
				}
				if c.dialog == previous {
					t.Fatal("provider change did not replace the dialog")
				}
			} else {
				finish, err := c.Prepare(context.Background(), next)
				if err != nil {
					t.Fatal(err)
				}
				finish(false)
				if c.dialog != previous || c.cfg.STT != cfg.STT {
					t.Fatal("rollback did not restore the original dialog and provider")
				}
			}
			c.dialog.ResetVAD()
			if state := c.dialog.VADDebugState(); state.LastError != "" || state.SpeechFrames != 0 {
				t.Fatalf("reset after reload: %+v", state)
			}
			if _, err := c.dialog.ProcessVADFrame(frame); err != nil {
				t.Fatalf("VAD cannot process audio after reload: %v", err)
			}
			if state := c.dialog.VADDebugState(); state.Probability != 0.9 || !state.Speaking || state.SpeechFrames != 1 {
				t.Fatalf("unexpected VAD state after reload: %+v", state)
			}
		})
	}
}

func TestInputLifecycleInactiveProviderKeepsVoiceLoop(t *testing.T) {
	for _, mode := range []string{"stt", "realtime"} {
		t.Run(mode, func(t *testing.T) {
			cfg := agent.DefaultConfig()
			cfg.ConfigDir = t.TempDir()
			cfg.Model.Provider, cfg.Model.Model = "fake", "fake"
			cfg.InputMode, cfg.TTS.Provider = mode, ""
			cfg.QuickCapture.Enabled = new(bool)
			r, err := agent.NewRuntime(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			s := agent.NewServer(r, ":0")
			defer s.Close()
			c := newInputLifecycle(r, s)
			c.newDialog = func(agent.Config) (*agent.AudioDialog, error) { return nil, nil }
			c.runVoice = func(_ agent.Config, _ *agent.AudioDialog, shutdown chan os.Signal, _ <-chan struct{}) { <-shutdown }
			if err := c.Start(cfg); err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			stop := c.stop
			next := cfg
			if mode == "stt" {
				next.VoiceModel.Provider = "gemini"
			} else {
				next.TTS.APIKey = "replacement-key"
				next.STT.Language = "en"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			finish, err := c.Prepare(ctx, next)
			if err != nil {
				t.Fatalf("inactive provider blocked on active conversation: %v", err)
			}
			finish(true)
			select {
			case <-stop:
				t.Fatal("inactive provider drained voice loop")
			default:
			}
		})
	}
}

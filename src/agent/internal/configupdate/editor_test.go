package configupdate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"aiden-agent/internal/agent"
)

// selfConfigFixture configures classic voice providers, as a provisioned
// device does; without them the resolved TTS and STT sections are empty.
const selfConfigFixture = `[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.stt]
provider = "speech"

[voice_settings.classic.tts]
provider = "voice"

[voice_settings.classic.stt.providers.speech]
type = "openai-whisper"
api_key = "stt-secret"

[voice_settings.classic.tts.providers.voice]
type = "alicloud"
api_key = "tts-secret"
`

// Every setting the Agent may change on its own must be writable through the
// Config Web patch shape and apply live; a reboot-level or unknown path in
// the allowlist would make config_update promise something it cannot keep.
func TestSelfConfigSettingsAreWritableAndLive(t *testing.T) {
	samples := map[string]any{
		"tts.speed":                       1.2,
		"stt.language":                    "en",
		"agent.locale":                    "zh-CN",
		"agent.timezone":                  "Asia/Tokyo",
		"agent.silence_ms":                1500,
		"agent.voice_followup_enabled":    false,
		"agent.voice_followup_timeout_ms": 9000,
		"agent.voice_max_turns":           3,
		"agent.voice_interrupt_on_wakeup": false,
		"agent.voice_tool_call_speech":    false,
		"voice_notifications.enabled":     false,
		"live_activity.enabled":           false,
	}
	// config_update reads the resolved input mode from the same snapshot to
	// tell which settings the current mode ignores.
	modePath := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(modePath, []byte(selfConfigFixture), 0o640); err != nil {
		t.Fatal(err)
	}
	modeSnapshot, err := NewService().Snapshot(modePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupJSON(t, modeSnapshot, "agent.input_mode"); string(got) != `"stt"` {
		t.Fatalf("snapshot agent.input_mode = %s, want \"stt\"", got)
	}
	for _, setting := range agent.SelfConfigSettingPaths() {
		sample, ok := samples[setting]
		if !ok {
			t.Errorf("no sample value for allowlisted setting %s", setting)
			continue
		}
		t.Run(setting, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			if err := os.WriteFile(path, []byte(selfConfigFixture), 0o640); err != nil {
				t.Fatal(err)
			}
			patch := nestSetting(t, setting, sample)
			edit, err := NewService().Edit(path, patch, true)
			if err != nil {
				t.Fatalf("Edit(%s) error = %v", patch, err)
			}
			if len(edit.ChangedPaths) == 0 {
				t.Fatalf("Edit(%s) changed nothing", patch)
			}
			if edit.Apply != agent.ApplyLive {
				t.Fatalf("Edit(%s) apply = %q, want live", patch, edit.Apply)
			}
			snapshot, err := NewService().Snapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(sample)
			if got := lookupJSON(t, snapshot, setting); string(got) != string(want) {
				t.Fatalf("snapshot %s = %s, want %s", setting, got, want)
			}
		})
	}
}

func TestEditDryRunLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(selfConfigFixture), 0o640); err != nil {
		t.Fatal(err)
	}
	edit, err := NewService().Edit(path, json.RawMessage(`{"config":{"tts":{"speed":0.8}}}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := lookupJSON(t, edit.Config, "tts.speed"); string(got) != "0.8" {
		t.Fatalf("candidate tts.speed = %s, want 0.8", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != selfConfigFixture {
		t.Fatalf("dry run wrote %q", data)
	}
}

// Config Web and the Agent's config tool save from separate processes; a
// save must merge onto the latest file, never overwrite a concurrent one.
func TestConcurrentEditsKeepEverySave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(selfConfigFixture), 0o640); err != nil {
		t.Fatal(err)
	}
	patches := map[string]any{
		"tts.speed":                    1.3,
		"agent.timezone":               "Asia/Tokyo",
		"agent.silence_ms":             1700,
		"agent.voice_max_turns":        4,
		"agent.voice_tool_call_speech": false,
		"live_activity.enabled":        false,
		"voice_notifications.enabled":  false,
	}
	var wg sync.WaitGroup
	for setting, value := range patches {
		wg.Add(1)
		go func(setting string, value any) {
			defer wg.Done()
			if _, err := NewService().Edit(path, nestSetting(t, setting, value), true); err != nil {
				t.Errorf("Edit(%s) error = %v", setting, err)
			}
		}(setting, value)
	}
	wg.Wait()
	snapshot, err := NewService().Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	for setting, value := range patches {
		want, _ := json.Marshal(value)
		if got := lookupJSON(t, snapshot, setting); string(got) != string(want) {
			t.Errorf("%s = %s, want %s; a concurrent save was lost", setting, got, want)
		}
	}
}

func nestSetting(t *testing.T, path string, value any) json.RawMessage {
	t.Helper()
	var node any = value
	parts := splitDotted(path)
	for i := len(parts) - 1; i >= 0; i-- {
		node = map[string]any{parts[i]: node}
	}
	data, err := json.Marshal(map[string]any{"config": node})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func lookupJSON(t *testing.T, data json.RawMessage, path string) json.RawMessage {
	t.Helper()
	current := data
	for _, part := range splitDotted(path) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(current, &fields); err != nil {
			t.Fatalf("lookup %s: %v", path, err)
		}
		current = fields[part]
	}
	return current
}

func splitDotted(path string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			parts = append(parts, path[start:i])
			start = i + 1
		}
	}
	return append(parts, path[start:])
}

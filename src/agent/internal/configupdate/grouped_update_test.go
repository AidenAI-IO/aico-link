package configupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateWritesTapWakeupEnabledToClassicRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `[model_settings.providers.local]
type = "openai"
api_key = "secret"

[model_settings.model]
provider = "local"
model = "gpt-old"

[voice_settings.classic.stt]
provider = "openai-whisper"
api_key = "key"

[voice_settings.classic.tts]
provider = "minimax-cn"
api_key = "key"

[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.runtime]
tap_wakeup_enabled = true
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"tap_wakeup_enabled":false}}}`))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.OK {
		t.Fatalf("Update() OK = false error=%q", result.Error)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if !strings.Contains(text, "[voice_settings.classic.runtime]") || !strings.Contains(text, "tap_wakeup_enabled = false") {
		t.Fatalf("tap_wakeup_enabled missing from classic.runtime: %s", text)
	}
	if strings.Contains(text, "[conversation_settings.agent]") && strings.Contains(text, "tap_wakeup_enabled") {
		t.Fatalf("tap_wakeup_enabled written under conversation_settings.agent: %s", text)
	}
}

func TestUpdateWritesGroupedTOMLPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `[model_settings.providers.local]
type = "openai"
api_key = "secret"

[model_settings.model]
provider = "local"
model = "gpt-old"

[voice_settings.classic.stt]
provider = "openai-whisper"
api_key = "key"

[voice_settings.classic.tts]
provider = "minimax-cn"
api_key = "key"

[voice_settings.mode]
input_mode = "stt"
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"model":{"model":"gpt-new"}}}`))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.OK {
		t.Fatalf("Update() OK = false, want true")
	}
	// ChangedPaths includes both the explicit change and legacy provider sync paths
	foundModelChange := false
	for _, p := range result.ChangedPaths {
		if p == "model_settings.model.model" {
			foundModelChange = true
			break
		}
	}
	if !foundModelChange {
		t.Fatalf("model_settings.model.model not in ChangedPaths: %v", result.ChangedPaths)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if !strings.Contains(text, "[model_settings.model]") || !strings.Contains(text, `model = "gpt-new"`) {
		t.Fatalf("grouped model section missing from %s", text)
	}
	if strings.Contains(text, "[model]\n") {
		t.Fatalf("legacy model section was written: %s", text)
	}
}

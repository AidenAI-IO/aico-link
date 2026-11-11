package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultConfigEnablesBackendAgent pins realtime delegation to the backend
// agent as the default when use_backend_agent is omitted.
func TestDefaultConfigEnablesBackendAgent(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.VoiceModel.UseBackendAgent {
		t.Fatal("DefaultConfig().VoiceModel.UseBackendAgent = false, want true")
	}
}

func TestUseBackendAgentEffectiveIgnoresMossWithoutRewriting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        Config
		want       bool
		keepStored bool
	}{
		{
			name: "gemini keeps stored true",
			cfg:  Config{VoiceModel: VoiceModelConfig{Provider: "gemini", UseBackendAgent: true}},
			want: true,
		},
		{
			name:       "moss bare provider forces false",
			cfg:        Config{VoiceModel: VoiceModelConfig{Provider: "moss", UseBackendAgent: true}},
			want:       false,
			keepStored: true,
		},
		{
			name: "moss named provider forces false",
			cfg: Config{
				VoiceModel:          VoiceModelConfig{Provider: "moss-main", UseBackendAgent: true},
				VoiceModelProviders: map[string]VoiceModelProvider{"moss-main": {Type: "moss"}},
			},
			want:       false,
			keepStored: true,
		},
		{
			name: "stored false stays false",
			cfg:  Config{VoiceModel: VoiceModelConfig{Provider: "gemini", UseBackendAgent: false}},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.UseBackendAgentEffective(); got != tc.want {
				t.Fatalf("UseBackendAgentEffective() = %v, want %v", got, tc.want)
			}
			if tc.keepStored && !tc.cfg.VoiceModel.UseBackendAgent {
				t.Fatal("stored UseBackendAgent was rewritten; want preserved true")
			}
		})
	}
}

func TestLoadRuntimeConfigBackendAgentDefaultFollowsModelCredential(t *testing.T) {
	t.Setenv("BACKEND_AGENT_TEST_MODEL_KEY", "")
	for _, tc := range []struct {
		name     string
		apiKey   string
		explicit string
		want     bool
	}{
		{name: "credential present", apiKey: "model-secret", want: true},
		{name: "credential missing", apiKey: "$BACKEND_AGENT_TEST_MODEL_KEY", want: false},
		{name: "explicit true kept without credential", apiKey: "$BACKEND_AGENT_TEST_MODEL_KEY", explicit: "true", want: true},
		{name: "explicit false kept with credential", apiKey: "model-secret", explicit: "false", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `
[model_settings.providers.openrouter-main]
type = "openrouter"
api_key = "` + tc.apiKey + `"

[model_settings.model]
provider = "openrouter-main"
model = "test/model"

[voice_settings.realtime]
provider = "gemini"
api_key = "realtime-secret"
`
			if tc.explicit != "" {
				body += "use_backend_agent = " + tc.explicit + "\n"
			}
			path := filepath.Join(t.TempDir(), "agent.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig: %v", err)
			}
			if cfg.VoiceModel.UseBackendAgent != tc.want {
				t.Fatalf("use_backend_agent = %v, want %v", cfg.VoiceModel.UseBackendAgent, tc.want)
			}
		})
	}
}

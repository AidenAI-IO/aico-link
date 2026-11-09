package agent

import "testing"

func TestDefaultConfigEnablesBackendAgent(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.VoiceModel.UseBackendAgent {
		t.Fatal("DefaultConfig().VoiceModel.UseBackendAgent = false, want true")
	}
}

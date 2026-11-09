package agent

import "testing"

// TestDefaultConfigEnablesBackendAgent pins realtime delegation to the backend
// agent as the default when use_backend_agent is omitted.
func TestDefaultConfigEnablesBackendAgent(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.VoiceModel.UseBackendAgent {
		t.Fatal("DefaultConfig().VoiceModel.UseBackendAgent = false, want true")
	}
}

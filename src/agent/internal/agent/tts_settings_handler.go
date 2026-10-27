package agent

import (
	"encoding/json"
	"net/http"

	"aico-agent/internal/agent/tts"
)

// TTSSettingsRequest is the body for POST /api/settings/tts.
type TTSSettingsRequest struct {
	Provider    string  `json:"provider"`
	APIKey      string  `json:"api_key,omitempty"`
	Voice       string  `json:"voice,omitempty"`
	Emotion     string  `json:"emotion,omitempty"`
	ReferenceID string  `json:"reference_id,omitempty"`
	Speed       float64 `json:"speed,omitempty"`
}

// TTSSettingsResponse is the body for GET /api/settings/tts.
type TTSSettingsResponse struct {
	Provider     string           `json:"provider"`
	Capabilities tts.Capabilities `json:"capabilities"`
	Available    []string         `json:"available_providers"`
}

// handleTTSSettings handles GET (current settings) and POST (switch provider).
func (s *Server) handleTTSSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.respondTTSSettings(w)
	case http.MethodPost:
		s.handleTTSSwitch(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) respondTTSSettings(w http.ResponseWriter) {
	resp := TTSSettingsResponse{Available: availableTTSProviderNames(s.runtime.ConfigSnapshot())}
	manager := s.currentTTSManager()
	if manager != nil {
		resp.Provider = manager.Current()
		resp.Capabilities = manager.Holder().Capabilities()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTTSSwitch(w http.ResponseWriter, r *http.Request) {
	var req TTSSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		http.Error(w, "provider is required", http.StatusBadRequest)
		return
	}

	// Resolve the config for the *target* provider, so per-provider credentials
	// in agent.toml are picked up automatically.
	cfg := s.runtime.ConfigSnapshot()
	ttsCfg := buildTTSProviderConfigFor(cfg, req.Provider)

	// Request body fields override the resolved config.
	if req.APIKey != "" {
		ttsCfg.APIKey = req.APIKey
	}
	if req.Voice != "" {
		ttsCfg.Voice = req.Voice
	}
	if req.Speed > 0 {
		ttsCfg.SpeedRatio = req.Speed
	}
	if ttsCfg.Extra == nil {
		ttsCfg.Extra = map[string]any{}
	}
	if req.Emotion != "" {
		ttsCfg.Extra["emotion"] = req.Emotion
	}
	if req.ReferenceID != "" {
		ttsCfg.Extra["reference_id"] = req.ReferenceID
	}

	manager := s.ttsProviderManager()
	if manager == nil {
		http.Error(w, "TTS manager is unavailable", http.StatusInternalServerError)
		return
	}
	if err := manager.SwitchTo(ttsCfg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.respondTTSSettings(w)
}

// handleTTSProviders returns the list of available provider names.
func (s *Server) handleTTSProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"providers": availableTTSProviderNames(s.runtime.ConfigSnapshot()),
	})
}

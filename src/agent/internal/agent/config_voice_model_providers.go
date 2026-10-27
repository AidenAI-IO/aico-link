package agent

import (
	"aico-agent/internal/agent/realtimevoice"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// VoiceModelProvider is one [voice_model_providers.<name>] record. The flat
// [voice_model] section keeps the selected record name and session-wide
// behavior; credentials and provider-specific routing stay on the record.
type VoiceModelProvider struct {
	Type                   string   `toml:"type"`
	UpstreamProvider       string   `toml:"upstream_provider,omitempty"`
	AgentID                string   `toml:"agent_id,omitempty"`
	APIKey                 string   `toml:"api_key,omitempty"`
	Model                  string   `toml:"model,omitempty"`
	WorkspaceID            string   `toml:"workspace_id,omitempty"`
	Region                 string   `toml:"region,omitempty"`
	AuthMode               string   `toml:"auth_mode,omitempty"`
	ProjectID              string   `toml:"project_id,omitempty"`
	Location               string   `toml:"location,omitempty"`
	Endpoint               string   `toml:"endpoint,omitempty"`
	BaseURL                string   `toml:"base_url,omitempty"`
	RealtimeProtocol       string   `toml:"realtime_protocol,omitempty"`
	ThinkingLevel          string   `toml:"thinking_level,omitempty"`
	Voice                  string   `toml:"voice,omitempty"`
	TurnDetection          string   `toml:"turn_detection,omitempty"`
	TurnDetectionThreshold *float64 `toml:"turn_detection_threshold,omitempty"`
	TurnDetectionSilenceMs int      `toml:"turn_detection_silence_ms,omitempty"`
}

func normalizeVoiceModelProviderType(providerType string) string {
	return strings.ToLower(strings.TrimSpace(providerType))
}

func isKnownVoiceModelProviderType(providerType string) bool {
	return realtimevoice.IsProvider(providerType)
}

func defaultVoiceModelProviderRecord(providerType string) VoiceModelProvider {
	providerType = normalizeVoiceModelProviderType(providerType)
	record := VoiceModelProvider{Type: providerType}
	if providerType == defaultVoiceModelProvider {
		record.Model = defaultVoiceModelModel
		record.Voice = defaultVoiceModelVoice
		record.TurnDetection = defaultVoiceModelTurnDetection
	}
	return record
}

// migrateLegacyVoiceModelProvider turns the old flat [voice_model] provider
// settings into one named record. Only fields present in the source TOML are
// copied, except canonical Qwen defaults used to seed its first record.
func migrateLegacyVoiceModelProvider(cfg *Config, metadata toml.MetaData) {
	if cfg == nil {
		return
	}
	ref := strings.TrimSpace(cfg.VoiceModel.Provider)
	if ref == "" {
		return
	}

	record, exists := cfg.VoiceModelProviders[ref]
	if !exists {
		providerType := normalizeVoiceModelProviderType(ref)
		if !isKnownVoiceModelProviderType(providerType) {
			return
		}
		record = defaultVoiceModelProviderRecord(providerType)
	}
	copyDefinedLegacyVoiceModelFields(&record, cfg.VoiceModel, metadata)
	if cfg.VoiceModelProviders == nil {
		cfg.VoiceModelProviders = make(map[string]VoiceModelProvider)
	}
	cfg.VoiceModelProviders[ref] = record
	clearVoiceModelProviderFields(&cfg.VoiceModel)
}

func copyDefinedLegacyVoiceModelFields(record *VoiceModelProvider, legacy VoiceModelConfig, metadata toml.MetaData) {
	if metadata.IsDefined("voice_model", "upstream_provider") {
		record.UpstreamProvider = legacy.UpstreamProvider
	}
	if metadata.IsDefined("voice_model", "agent_id") {
		record.AgentID = legacy.AgentID
	}
	if metadata.IsDefined("voice_model", "api_key") {
		record.APIKey = legacy.APIKey
	}
	if metadata.IsDefined("voice_model", "model") {
		record.Model = legacy.Model
	}
	if metadata.IsDefined("voice_model", "workspace_id") {
		record.WorkspaceID = legacy.WorkspaceID
	}
	if metadata.IsDefined("voice_model", "region") {
		record.Region = legacy.Region
	}
	if metadata.IsDefined("voice_model", "auth_mode") {
		record.AuthMode = legacy.AuthMode
	}
	if metadata.IsDefined("voice_model", "project_id") {
		record.ProjectID = legacy.ProjectID
	}
	if metadata.IsDefined("voice_model", "location") {
		record.Location = legacy.Location
	}
	if metadata.IsDefined("voice_model", "endpoint") {
		record.Endpoint = legacy.Endpoint
	}
	if metadata.IsDefined("voice_model", "base_url") {
		record.BaseURL = legacy.BaseURL
	}
	if metadata.IsDefined("voice_model", "realtime_protocol") {
		record.RealtimeProtocol = legacy.RealtimeProtocol
	}
	if metadata.IsDefined("voice_model", "voice") {
		record.Voice = legacy.Voice
	}
	if normalizeVoiceModelProviderType(record.Type) != realtimevoice.ProviderQwen {
		return
	}
	if metadata.IsDefined("voice_model", "turn_detection") {
		record.TurnDetection = legacy.TurnDetection
	}
	if metadata.IsDefined("voice_model", "turn_detection_threshold") {
		record.TurnDetectionThreshold = legacy.TurnDetectionThreshold
	}
	if metadata.IsDefined("voice_model", "turn_detection_silence_ms") {
		record.TurnDetectionSilenceMs = legacy.TurnDetectionSilenceMs
	}
}

func clearVoiceModelProviderFields(config *VoiceModelConfig) {
	config.UpstreamProvider = ""
	config.AgentID = ""
	config.APIKey = ""
	config.Model = ""
	config.WorkspaceID = ""
	config.Region = ""
	config.AuthMode = ""
	config.ProjectID = ""
	config.Location = ""
	config.Endpoint = ""
	config.BaseURL = ""
	config.RealtimeProtocol = ""
	config.Voice = ""
	config.TurnDetection = ""
	config.TurnDetectionThreshold = nil
	config.TurnDetectionSilenceMs = 0
}

// resolveVoiceModelProvider expands the selected record into the legacy flat
// runtime shape consumed by the realtime adapter seam.
func resolveVoiceModelProvider(cfg *Config) {
	if cfg == nil {
		return
	}
	ref := strings.TrimSpace(cfg.VoiceModel.Provider)
	record, ok := cfg.VoiceModelProviders[ref]
	if ok {
		providerType := normalizeVoiceModelProviderType(record.Type)
		if providerType == "" {
			return
		}
		cfg.VoiceModel.Provider = providerType
		cfg.VoiceModel.ActiveProviderRecord = ref
		fillVoiceModelProviderFields(&cfg.VoiceModel, record)
	} else {
		cfg.VoiceModel.Provider = normalizeVoiceModelProviderType(ref)
	}
	applyVoiceModelTypeDefaults(&cfg.VoiceModel)
}

func fillVoiceModelProviderFields(config *VoiceModelConfig, record VoiceModelProvider) {
	if config.UpstreamProvider == "" {
		config.UpstreamProvider = record.UpstreamProvider
	}
	if config.AgentID == "" {
		config.AgentID = record.AgentID
	}
	if config.APIKey == "" {
		config.APIKey = record.APIKey
	}
	if config.Model == "" {
		config.Model = record.Model
	}
	if config.WorkspaceID == "" {
		config.WorkspaceID = record.WorkspaceID
	}
	if config.Region == "" {
		config.Region = record.Region
	}
	if config.AuthMode == "" {
		config.AuthMode = record.AuthMode
	}
	if config.ProjectID == "" {
		config.ProjectID = record.ProjectID
	}
	if config.Location == "" {
		config.Location = record.Location
	}
	if config.Endpoint == "" {
		config.Endpoint = record.Endpoint
	}
	if config.BaseURL == "" {
		config.BaseURL = record.BaseURL
	}
	if config.RealtimeProtocol == "" {
		config.RealtimeProtocol = record.RealtimeProtocol
	}
	if config.ThinkingLevel == "" {
		config.ThinkingLevel = record.ThinkingLevel
	}
	if config.Voice == "" {
		config.Voice = record.Voice
	}
	if normalizeVoiceModelProviderType(record.Type) != realtimevoice.ProviderQwen {
		config.TurnDetection = ""
		config.TurnDetectionThreshold = nil
		config.TurnDetectionSilenceMs = 0
		return
	}
	if config.TurnDetection == "" {
		config.TurnDetection = record.TurnDetection
	}
	if config.TurnDetectionThreshold == nil {
		config.TurnDetectionThreshold = record.TurnDetectionThreshold
	}
	if config.TurnDetectionSilenceMs == 0 {
		config.TurnDetectionSilenceMs = record.TurnDetectionSilenceMs
	}
}

func applyVoiceModelTypeDefaults(config *VoiceModelConfig) {
	if config == nil || normalizeVoiceModelProviderType(config.Provider) != defaultVoiceModelProvider {
		return
	}
	if config.Model == "" {
		config.Model = defaultVoiceModelModel
	}
	if config.Voice == "" {
		config.Voice = defaultVoiceModelVoice
	}
	if config.TurnDetection == "" {
		config.TurnDetection = defaultVoiceModelTurnDetection
	}
}

func validateVoiceModelProviderRecords(cfg Config) error {
	names := make([]string, 0, len(cfg.VoiceModelProviders))
	for name := range cfg.VoiceModelProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("voice_model_providers: record name cannot be empty")
		}
	}

	ref := strings.TrimSpace(cfg.VoiceModel.Provider)
	if ref == "" {
		return nil
	}
	if record, ok := cfg.VoiceModelProviders[ref]; ok {
		providerType := normalizeVoiceModelProviderType(record.Type)
		if providerType == "" {
			return fmt.Errorf("voice_model_providers.%s: provider type is required", ref)
		}
		if !isKnownVoiceModelProviderType(providerType) {
			return fmt.Errorf("voice_model_providers.%s: unsupported provider type %q", ref, record.Type)
		}
		if err := validateVoiceModelRealtimeProtocol(record.RealtimeProtocol, providerType, fmt.Sprintf("voice_model_providers.%s.realtime_protocol", ref)); err != nil {
			return err
		}
		providerFieldPrefix := fmt.Sprintf("voice_model_providers.%s", ref)
		if providerType == realtimevoice.ProviderQwen {
			if err := validateQwenTurnDetection(record.TurnDetection, record.TurnDetectionThreshold, record.TurnDetectionSilenceMs, providerFieldPrefix); err != nil {
				return err
			}
		} else if record.TurnDetection != "" || record.TurnDetectionThreshold != nil || record.TurnDetectionSilenceMs != 0 {
			return fmt.Errorf("%s.turn_detection settings are only supported for provider=qwen", providerFieldPrefix)
		}
		return nil
	}
	if !isKnownVoiceModelProviderType(ref) {
		return fmt.Errorf("voice_model.provider %q is neither a [voice_model_providers] record nor a known realtime provider type", ref)
	}
	return nil
}

func validateQwenTurnDetection(turnType string, threshold *float64, silenceMs int, fieldPrefix string) error {
	if turnType != "" && turnType != "server_vad" && turnType != "smart_turn" {
		return fmt.Errorf("%s.turn_detection: unsupported type %q", fieldPrefix, turnType)
	}
	if silenceMs < 0 {
		return fmt.Errorf("%s.turn_detection_silence_ms must be >= 0", fieldPrefix)
	}
	if turnType == "smart_turn" {
		return nil
	}
	if threshold != nil && (math.IsNaN(*threshold) || math.IsInf(*threshold, 0) || *threshold < -1 || *threshold > 1) {
		return fmt.Errorf("%s.turn_detection_threshold must be a finite value between -1 and 1", fieldPrefix)
	}
	if silenceMs != 0 && (silenceMs < 200 || silenceMs > 6000) {
		return fmt.Errorf("%s.turn_detection_silence_ms must be 0 or between 200 and 6000", fieldPrefix)
	}
	return nil
}

func validateVoiceModelRealtimeProtocol(protocol, providerType, field string) error {
	normalized := strings.ToLower(strings.TrimSpace(protocol))
	switch normalized {
	case "", "ga", "legacy", "beta":
	default:
		return fmt.Errorf("%s: unsupported protocol %q (expected ga or legacy)", field, protocol)
	}
	if normalized != "" && normalizeVoiceModelProviderType(providerType) != "openai" {
		return fmt.Errorf("%s is only supported for provider=openai", field)
	}
	return nil
}

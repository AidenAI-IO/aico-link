package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"aiden-agent/internal/agent/tts"
)

// TTSProvider is one [tts_providers.<name>] record: a provider type plus the
// credentials and voice settings that only mean anything for that type. [tts]
// references one by name, so several stay configured at once and switching is a
// one-line change -- the same shape [model_providers.<name>] gives [model].
//
// speed is deliberately absent. It is a listening preference that should not
// change when the voice changes, so it stays global on [tts].
type TTSProvider struct {
	Type        string `toml:"type"` // Provider type: minimax, fish-audio, ...
	APIKey      string `toml:"api_key,omitempty"`
	Model       string `toml:"model,omitempty"`
	VoiceID     string `toml:"voice_id,omitempty"`
	Emotion     string `toml:"emotion,omitempty"`
	ReferenceID string `toml:"reference_id,omitempty"`
}

// STTProvider is one [stt_providers.<name>] record. language stays on [stt]: it
// is a user preference that holds regardless of which provider transcribes.
type STTProvider struct {
	Type            string `toml:"type"` // Provider type: openai-whisper, tencent-asr, ...
	APIKey          string `toml:"api_key,omitempty"`
	Model           string `toml:"model,omitempty"`
	BaseURL         string `toml:"base_url,omitempty"`
	AppID           string `toml:"app_id,omitempty"`
	SecretID        string `toml:"secret_id,omitempty"`
	SecretKey       string `toml:"secret_key,omitempty"`
	Region          string `toml:"region,omitempty"`
	EngineModelType string `toml:"engine_model_type,omitempty"`
}

// isKnownTTSProviderType and isKnownSTTProviderType stay separate from
// isKnownProviderType on purpose. A single merged whitelist would accept
// [model] provider = "minimax", which passes validation and only fails much
// later when the model client is built.
func isKnownTTSProviderType(providerType string) bool {
	return tts.HasProvider(normalizeTTSProvider(providerType))
}

func isKnownSTTProviderType(providerType string) bool {
	_, ok := lookupSTTProviderDefinition(providerType)
	return ok
}

// resolveTTSProvider expands a [tts] provider reference into the effective
// config, mirroring resolveModelProvider. A name that matches a record wins; a
// bare provider type is left alone for backward compatibility. Fields already
// set on [tts] override what the record carries, so a flat override still works.
//
// Resolution is deliberately mechanical and never fails. Voice is optional at
// runtime: a TTS init failure is a logger.Warn and the agent still starts, so a
// stale provider name must not stop the device from booting. An unresolvable
// reference is left in place for tts.New() to report by name. Strict checking
// lives in ValidateVoiceProviders, which the config page runs on save.
func resolveTTSProvider(cfg *Config) {
	if cfg == nil {
		return
	}
	ref := strings.TrimSpace(cfg.TTS.Provider)
	if ref == "" {
		return
	}

	record, ok := cfg.TTSProviders[ref]
	if !ok {
		// Not a reference: a bare provider type, or a name that no longer
		// resolves. normalizeTTSProvider folds the minimax-ws alias and lowers
		// the case; it must not run before the lookup above, or a record named
		// with capitals would stop matching itself.
		cfg.TTS.Provider = normalizeTTSProvider(ref)
		return
	}

	providerType := strings.TrimSpace(record.Type)
	if providerType == "" {
		// Leave the reference in place. Blanking it here would read downstream
		// as "TTS disabled" rather than "TTS misconfigured".
		return
	}

	cfg.TTS.Provider = normalizeTTSProvider(providerType)
	// Remember which record this was, so speak-time resolution cannot pick a
	// different record of the same type. See TTSConfig.ActiveProviderRecord.
	cfg.TTS.ActiveProviderRecord = ref
	if cfg.TTS.APIKey == "" {
		cfg.TTS.APIKey = resolveProviderAPIKey(record.APIKey)
	}
	if cfg.TTS.Model == "" {
		cfg.TTS.Model = record.Model
	}
	if cfg.TTS.VoiceID == "" {
		cfg.TTS.VoiceID = record.VoiceID
	}
	if cfg.TTS.Emotion == "" {
		cfg.TTS.Emotion = record.Emotion
	}
	if cfg.TTS.ReferenceID == "" {
		cfg.TTS.ReferenceID = record.ReferenceID
	}
}

// resolveSTTProvider is resolveTTSProvider for [stt], with the same non-fatal
// contract.
func resolveSTTProvider(cfg *Config) {
	if cfg == nil {
		return
	}
	ref := strings.TrimSpace(cfg.STT.Provider)
	if ref == "" {
		return
	}

	record, ok := cfg.STTProviders[ref]
	if !ok {
		return
	}

	providerType := strings.TrimSpace(record.Type)
	if providerType == "" {
		return
	}

	cfg.STT.Provider = providerType
	if cfg.STT.APIKey == "" {
		cfg.STT.APIKey = resolveProviderAPIKey(record.APIKey)
	}
	if cfg.STT.Model == "" {
		cfg.STT.Model = record.Model
	}
	if cfg.STT.BaseURL == "" {
		cfg.STT.BaseURL = record.BaseURL
	}
	if cfg.STT.AppID == "" {
		cfg.STT.AppID = record.AppID
	}
	if cfg.STT.SecretID == "" {
		cfg.STT.SecretID = record.SecretID
	}
	if cfg.STT.SecretKey == "" {
		cfg.STT.SecretKey = record.SecretKey
	}
	if cfg.STT.Region == "" {
		cfg.STT.Region = record.Region
	}
	if cfg.STT.EngineModelType == "" {
		cfg.STT.EngineModelType = record.EngineModelType
	}
}

// resolveProviderAPIKey resolves the single provider credential syntax used by
// agent.toml and Config Web. A leading $ means the rest is an environment
// variable name; every other value is a literal API key.
func resolveProviderAPIKey(apiKey string) string {
	env, isEnvironmentReference := providerAPIKeyEnv(apiKey)
	if !isEnvironmentReference {
		return apiKey
	}
	if env == "" {
		return ""
	}
	return os.Getenv(env)
}

// ProviderAPIKeyConfigured reports whether a provider api_key yields a
// credential at runtime. A "$NAME" reference counts only when the environment
// variable is set: the shipped template references unset variables, and a
// settings page must not report those keys as filled in.
func ProviderAPIKeyConfigured(apiKey string) bool {
	return strings.TrimSpace(resolveProviderAPIKey(apiKey)) != ""
}

func providerAPIKeyEnv(apiKey string) (string, bool) {
	trimmed := strings.TrimSpace(apiKey)
	if !strings.HasPrefix(trimmed, "$") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, "$")), true
}

// ValidateVoiceProviders is the strict pass over the voice provider records. It
// is called from the config page's save path, not from boot: a save that stores
// a dangling reference silently loses voice on the next restart, so it must be
// rejected while the user is still looking at the form.
//
// Only the referenced record is checked strictly. An unreferenced record is
// user data parked for later and must not block unrelated config changes.
func (c Config) ValidateVoiceProviders() error {
	if err := validateVoiceModelProviderRecords(c); err != nil {
		return err
	}
	names := make([]string, 0, len(c.TTSProviders))
	for name := range c.TTSProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tts_providers: record name cannot be empty")
		}
	}

	sttNames := make([]string, 0, len(c.STTProviders))
	for name := range c.STTProviders {
		sttNames = append(sttNames, name)
	}
	sort.Strings(sttNames)
	for _, name := range sttNames {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("stt_providers: record name cannot be empty")
		}
	}

	if ref := strings.TrimSpace(c.TTS.Provider); ref != "" {
		if record, ok := c.TTSProviders[ref]; ok {
			providerType := strings.TrimSpace(record.Type)
			if providerType == "" {
				return fmt.Errorf("tts_providers.%s: provider type is required", ref)
			}
			if !isKnownTTSProviderType(providerType) {
				return fmt.Errorf("tts_providers.%s: unsupported provider type %q", ref, providerType)
			}
		} else if !isKnownTTSProviderType(ref) {
			return fmt.Errorf("tts.provider %q is neither a [tts_providers] record nor a known TTS provider type", ref)
		}
	}

	if ref := strings.TrimSpace(c.STT.Provider); ref != "" {
		if record, ok := c.STTProviders[ref]; ok {
			providerType := strings.TrimSpace(record.Type)
			if providerType == "" {
				return fmt.Errorf("stt_providers.%s: provider type is required", ref)
			}
			if !isKnownSTTProviderType(providerType) {
				return fmt.Errorf("stt_providers.%s: unsupported provider type %q", ref, providerType)
			}
		} else if !isKnownSTTProviderType(ref) {
			return fmt.Errorf("stt.provider %q is neither an [stt_providers] record nor a known STT provider type", ref)
		}
	}

	return nil
}

// migrateLegacyVoiceProviders upgrades flat realtime/TTS/STT credentials into
// named records so the config page sees one shape.
//
// For a mixed config, an explicitly populated flat field keeps its historical
// runtime precedence and overwrites the referenced record during migration.
// Fields inherited only from DefaultConfig are cleared without being copied.
func migrateLegacyVoiceProviders(cfg *Config, metadata toml.MetaData) {
	if cfg == nil {
		return
	}
	migrateLegacyVoiceModelProvider(cfg, metadata)
	migrateLegacyTTSFlatFields(cfg, metadata)
	migrateLegacySTTFlatFields(cfg, metadata)
}

// migrateLegacyTTSFlatFields turns a bare [tts] provider type plus its flat
// credentials into a record named after the type. Only a known type migrates: a
// name that already resolves to a record is a reference, and an unknown string
// has no adapter to belong to.
//
// Only fields the file actually set are copied. Reading them off cfg would also
// pick up DefaultConfig's values (the default provider keeps them, since
// clearDefaultTTSProviderFields skips it) and freeze today's default voice into
// the user's config file, where a later change to that default could not reach.
func migrateLegacyTTSFlatFields(cfg *Config, metadata toml.MetaData) {
	// The file must have declared a provider: both loaders clear an undeclared
	// [tts] before reaching here, and an absent file keeps the defaults, so this
	// gate keeps DefaultConfig's provider from minting a phantom minimax-cn record
	// for a device that only ever configured a model.
	if !metadata.IsDefined("tts", "provider") {
		return
	}
	provider := normalizeTTSProvider(cfg.TTS.Provider)
	if provider == "" {
		return
	}
	if record, isRef := cfg.TTSProviders[cfg.TTS.Provider]; isRef {
		// Flat fields historically override inherited record values at runtime.
		// Preserve explicitly configured non-empty overrides while collapsing
		// both sources into the record. Clear every provider-specific flat field,
		// including values inherited from DefaultConfig, so editing never bakes a
		// default voice into [tts] or leaves a second source behind.
		if metadata.IsDefined("tts", "api_key") && cfg.TTS.APIKey != "" {
			record.APIKey = cfg.TTS.APIKey
		}
		if metadata.IsDefined("tts", "model") && cfg.TTS.Model != "" {
			record.Model = cfg.TTS.Model
		}
		if metadata.IsDefined("tts", "voice_id") && cfg.TTS.VoiceID != "" {
			record.VoiceID = cfg.TTS.VoiceID
		}
		if metadata.IsDefined("tts", "emotion") && cfg.TTS.Emotion != "" {
			record.Emotion = cfg.TTS.Emotion
		}
		if metadata.IsDefined("tts", "reference_id") && cfg.TTS.ReferenceID != "" {
			record.ReferenceID = cfg.TTS.ReferenceID
		}
		cfg.TTS.APIKey = ""
		cfg.TTS.Model = ""
		cfg.TTS.VoiceID = ""
		cfg.TTS.Emotion = ""
		cfg.TTS.ReferenceID = ""
		cfg.TTSProviders[cfg.TTS.Provider] = record
		return
	}
	if !isKnownTTSProviderType(provider) {
		return
	}
	if cfg.TTSProviders == nil {
		cfg.TTSProviders = map[string]TTSProvider{}
	}
	if _, exists := cfg.TTSProviders[provider]; exists {
		return
	}

	// Move exactly the fields the file set, and clear exactly those. A field
	// either lives on the record or stays flat, never both -- that is the "one
	// editor per credential" invariant: two fields for one key would disagree the
	// moment either is edited, and would write both shapes back to agent.toml.
	//
	// The symmetry matters. Reading a field off cfg unconditionally would also
	// pick up DefaultConfig's value (the default provider keeps them, since
	// clearDefaultTTSProviderFields skips it) and freeze today's default voice
	// into the user's file. Clearing unconditionally would instead DISCARD such a
	// default, because resolve would then refill from a record that never carried
	// it. Neither the copy nor the clear may be unconditional.
	//
	// speed is not here at all: it is global by design.
	record := TTSProvider{Type: provider}
	if metadata.IsDefined("tts", "api_key") {
		record.APIKey = cfg.TTS.APIKey
		cfg.TTS.APIKey = ""
	}
	if metadata.IsDefined("tts", "model") {
		record.Model = cfg.TTS.Model
		cfg.TTS.Model = ""
	}
	if metadata.IsDefined("tts", "voice_id") {
		record.VoiceID = cfg.TTS.VoiceID
		cfg.TTS.VoiceID = ""
	}
	if metadata.IsDefined("tts", "emotion") {
		record.Emotion = cfg.TTS.Emotion
		cfg.TTS.Emotion = ""
	}
	if metadata.IsDefined("tts", "reference_id") {
		record.ReferenceID = cfg.TTS.ReferenceID
		cfg.TTS.ReferenceID = ""
	}
	cfg.TTSProviders[provider] = record
}

func migrateLegacySTTFlatFields(cfg *Config, metadata toml.MetaData) {
	// Same gate as TTS: the file must have declared a provider, or DefaultConfig's
	// would mint a phantom record for a config that never configured speech.
	if !metadata.IsDefined("stt", "provider") {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.STT.Provider))
	if provider == "" {
		return
	}
	if record, isRef := cfg.STTProviders[cfg.STT.Provider]; isRef {
		if metadata.IsDefined("stt", "api_key") && cfg.STT.APIKey != "" {
			record.APIKey = cfg.STT.APIKey
		}
		if metadata.IsDefined("stt", "model") && cfg.STT.Model != "" {
			record.Model = cfg.STT.Model
		}
		if metadata.IsDefined("stt", "base_url") && cfg.STT.BaseURL != "" {
			record.BaseURL = cfg.STT.BaseURL
		}
		if metadata.IsDefined("stt", "app_id") && cfg.STT.AppID != "" {
			record.AppID = cfg.STT.AppID
		}
		if metadata.IsDefined("stt", "secret_id") && cfg.STT.SecretID != "" {
			record.SecretID = cfg.STT.SecretID
		}
		if metadata.IsDefined("stt", "secret_key") && cfg.STT.SecretKey != "" {
			record.SecretKey = cfg.STT.SecretKey
		}
		if metadata.IsDefined("stt", "region") && cfg.STT.Region != "" {
			record.Region = cfg.STT.Region
		}
		if metadata.IsDefined("stt", "engine_model_type") && cfg.STT.EngineModelType != "" {
			record.EngineModelType = cfg.STT.EngineModelType
		}
		cfg.STT.APIKey = ""
		cfg.STT.Model = ""
		cfg.STT.BaseURL = ""
		cfg.STT.AppID = ""
		cfg.STT.SecretID = ""
		cfg.STT.SecretKey = ""
		cfg.STT.Region = ""
		cfg.STT.EngineModelType = ""
		cfg.STTProviders[cfg.STT.Provider] = record
		return
	}
	if !isKnownSTTProviderType(provider) {
		return
	}
	if cfg.STTProviders == nil {
		cfg.STTProviders = map[string]STTProvider{}
	}
	if _, exists := cfg.STTProviders[provider]; exists {
		return
	}

	// Same copy/clear symmetry as TTS above. language is not here at all: it
	// holds regardless of which provider transcribes, so it stays global.
	record := STTProvider{Type: provider}
	if metadata.IsDefined("stt", "api_key") {
		record.APIKey = cfg.STT.APIKey
		cfg.STT.APIKey = ""
	}
	if metadata.IsDefined("stt", "model") {
		record.Model = cfg.STT.Model
		cfg.STT.Model = ""
	}
	if metadata.IsDefined("stt", "base_url") {
		record.BaseURL = cfg.STT.BaseURL
		cfg.STT.BaseURL = ""
	}
	if metadata.IsDefined("stt", "app_id") {
		record.AppID = cfg.STT.AppID
		cfg.STT.AppID = ""
	}
	if metadata.IsDefined("stt", "secret_id") {
		record.SecretID = cfg.STT.SecretID
		cfg.STT.SecretID = ""
	}
	if metadata.IsDefined("stt", "secret_key") {
		record.SecretKey = cfg.STT.SecretKey
		cfg.STT.SecretKey = ""
	}
	if metadata.IsDefined("stt", "region") {
		record.Region = cfg.STT.Region
		cfg.STT.Region = ""
	}
	if metadata.IsDefined("stt", "engine_model_type") {
		record.EngineModelType = cfg.STT.EngineModelType
		cfg.STT.EngineModelType = ""
	}
	cfg.STTProviders[provider] = record
}

// availableTTSProviderNames lists what a client may switch to: the registered
// adapter types plus every configured record name.
//
// Types alone are not enough once several records share one type -- two accounts
// of one service is the whole point of named records, and a switch by type
// cannot say which account to use. Record names make that addressable while the
// types stay listed so a client that switches by type keeps working.
func availableTTSProviderNames(cfg Config) []string {
	seen := map[string]bool{}
	names := make([]string, 0, len(cfg.TTSProviders)+8)
	for _, name := range tts.AvailableProviders() {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	for name := range cfg.TTSProviders {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// lookupTTSProviderRecord resolves a name-or-type to a record, which is what the
// runtime switch API needs: the phone passes a provider type, while the config
// page passes a record name.
//
// Resolution order:
//  1. Exact record name. The config page switches by name, and an explicit name
//     must always win -- including over the active record of the same type.
//  2. The active record, when its type matches. This is what keeps speak-time
//     resolution on the record the reference actually named; a bare type scan
//     would otherwise pick whichever same-type record sorts first.
//  3. Any record of a matching type, in sorted name order so the pick is stable.
func lookupTTSProviderRecord(cfg Config, providerRef string) (TTSProvider, bool) {
	ref := strings.TrimSpace(providerRef)
	if ref == "" {
		return TTSProvider{}, false
	}
	if record, ok := cfg.TTSProviders[ref]; ok {
		return record, true
	}

	target := normalizeTTSProvider(ref)

	if active := strings.TrimSpace(cfg.TTS.ActiveProviderRecord); active != "" {
		if record, ok := cfg.TTSProviders[active]; ok &&
			normalizeTTSProvider(record.Type) == target {
			return record, true
		}
	}

	names := make([]string, 0, len(cfg.TTSProviders))
	for name := range cfg.TTSProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if normalizeTTSProvider(cfg.TTSProviders[name].Type) == target {
			return cfg.TTSProviders[name], true
		}
	}
	return TTSProvider{}, false
}

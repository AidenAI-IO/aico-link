package agent

import (
	"fmt"
	"strings"
	"sync"

	"aico-agent/internal/logging"
)

// SpeakerGate decides whether a Classic utterance belongs to an enrolled speaker.
// When Enabled is false the gate is a no-op (everyone allowed).
type SpeakerGate struct {
	Enabled   bool
	Threshold float64
	Embedder  SpeakerEmbedder
	Store     *SpeakerProfileStore

	mu sync.Mutex
}

type SpeakerGateDecision struct {
	Allowed   bool
	Score     float64
	Threshold float64
	Reason    string
}

func NewSpeakerGateFromConfig(cfg Config) *SpeakerGate {
	toolsDir := strings.TrimSpace(cfg.VoiceSpeakerToolsDir)
	if toolsDir == "" {
		toolsDir = defaultSpeakerToolsDir
	}
	modelDir := strings.TrimSpace(cfg.VoiceSpeakerModelDir)
	if modelDir == "" {
		modelDir = defaultSpeakerModelDir
	}
	storeDir := strings.TrimSpace(cfg.VoiceSpeakerStoreDir)
	if storeDir == "" {
		storeDir = defaultSpeakerStoreDir
	}
	return &SpeakerGate{
		Enabled:   cfg.VoiceSpeakerGateEnabledOrDefault(),
		Threshold: cfg.VoiceSpeakerThresholdOrDefault(),
		Embedder:  NewNativeSpeakerEmbedder(toolsDir, modelDir),
		Store:     NewSpeakerProfileStore(storeDir, cfg.VoiceSpeakerMaxProfilesOrDefault()),
	}
}

// SyncLiveSettings refreshes enablement and threshold from the current Agent
// config. Voice sessions keep a long-lived gate; without this, config-web
// threshold edits would stay stuck on the values captured at session start.
func (g *SpeakerGate) SyncLiveSettings(cfg Config) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Enabled = cfg.VoiceSpeakerGateEnabledOrDefault()
	g.Threshold = cfg.VoiceSpeakerThresholdOrDefault()
}

func (g *SpeakerGate) Status() (enabled bool, count int, threshold float64, err error) {
	if g == nil {
		return false, 0, 0, nil
	}
	enabled = g.Enabled
	threshold = g.Threshold
	if g.Store != nil {
		count, err = g.Store.Count()
	}
	return enabled, count, threshold, err
}

func (g *SpeakerGate) Enroll(id string, pcm []int16) error {
	if g == nil || g.Embedder == nil || g.Store == nil {
		return fmt.Errorf("speaker gate is not configured")
	}
	embedding, err := enrollSpeakerEmbedding(g.Embedder, pcm)
	if err != nil {
		return err
	}
	return g.Store.Save(NormalizeSpeakerProfileID(id), embedding)
}

func (g *SpeakerGate) Delete(id string) error {
	if g == nil || g.Store == nil {
		return fmt.Errorf("speaker store unavailable")
	}
	return g.Store.Delete(id)
}

func (g *SpeakerGate) List() ([]SpeakerProfileMeta, error) {
	if g == nil || g.Store == nil {
		return nil, nil
	}
	return g.Store.List()
}

// AllowUtterance returns whether the utterance may enter the Classic dialog path.
func (g *SpeakerGate) AllowUtterance(pcm []int16) SpeakerGateDecision {
	if g == nil || !g.Enabled {
		threshold := 0.0
		if g != nil {
			threshold = g.Threshold
		}
		return SpeakerGateDecision{Allowed: true, Threshold: threshold, Reason: "gate_disabled"}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	threshold := g.Threshold

	profiles, err := g.Store.LoadAll()
	if err != nil {
		logging.Warnf("agent", "speaker", "load profiles failed: %v", err)
		return SpeakerGateDecision{Allowed: false, Threshold: threshold, Reason: "store_error"}
	}
	if len(profiles) == 0 {
		return SpeakerGateDecision{Allowed: false, Threshold: threshold, Reason: "no_profiles"}
	}
	if g.Embedder == nil {
		return SpeakerGateDecision{Allowed: false, Threshold: threshold, Reason: "no_embedder"}
	}
	vec, err := g.Embedder.Embed(pcm)
	if err != nil {
		logging.Warnf("agent", "speaker", "embed failed: %v", err)
		return SpeakerGateDecision{Allowed: false, Threshold: threshold, Reason: "embed_error"}
	}
	best := -1.0
	for _, profile := range profiles {
		score := cosineSpeakerSimilarity(vec, profile)
		if score > best {
			best = score
		}
	}
	if best >= threshold {
		return SpeakerGateDecision{Allowed: true, Score: best, Threshold: threshold, Reason: "matched"}
	}
	return SpeakerGateDecision{Allowed: false, Score: best, Threshold: threshold, Reason: "below_threshold"}
}

func NormalizeSpeakerProfileID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "speaker"
	}
	return id
}

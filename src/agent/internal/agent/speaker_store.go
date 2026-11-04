package agent

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

var speakerProfileIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type SpeakerProfileMeta struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

type SpeakerProfileStore struct {
	dir         string
	maxProfiles int
	mu          sync.Mutex
}

var (
	sharedSpeakerStoresMu sync.Mutex
	sharedSpeakerStores   = map[string]*SpeakerProfileStore{}
)

// SharedSpeakerProfileStore returns the process-wide store for dir so HTTP and
// the Classic voice session share one lock over the same on-disk profiles.
func SharedSpeakerProfileStore(dir string, maxProfiles int) *SpeakerProfileStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = defaultSpeakerStoreDir
	}
	sharedSpeakerStoresMu.Lock()
	defer sharedSpeakerStoresMu.Unlock()
	if existing, ok := sharedSpeakerStores[dir]; ok {
		return existing
	}
	store := NewSpeakerProfileStore(dir, maxProfiles)
	sharedSpeakerStores[dir] = store
	return store
}

func NewSpeakerProfileStore(dir string, maxProfiles int) *SpeakerProfileStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = defaultSpeakerStoreDir
	}
	// Classic is single-owner: ignore any requested multi-profile budget.
	_ = maxProfiles
	return &SpeakerProfileStore{dir: dir, maxProfiles: defaultSpeakerMaxProfiles}
}

func (s *SpeakerProfileStore) Dir() string { return s.dir }

func (s *SpeakerProfileStore) MaxProfiles() int { return defaultSpeakerMaxProfiles }

func (s *SpeakerProfileStore) EnsureReady() error {
	return os.MkdirAll(s.dir, 0o700)
}

func (s *SpeakerProfileStore) List() ([]SpeakerProfileMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metas, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	owner, ok := selectOwnerMeta(metas)
	if !ok {
		return nil, nil
	}
	// Read path never deletes: surface the single logical owner only.
	return []SpeakerProfileMeta{owner}, nil
}

func (s *SpeakerProfileStore) Count() (int, error) {
	list, err := s.List()
	return len(list), err
}

func (s *SpeakerProfileStore) Save(id string, embedding []float32) error {
	id = NormalizeSpeakerProfileID(id)
	if !speakerProfileIDPattern.MatchString(id) {
		return fmt.Errorf("invalid speaker profile id %q", id)
	}
	if len(embedding) != speakerEmbeddingDim {
		return fmt.Errorf("embedding must have %d dims", speakerEmbeddingDim)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.EnsureReady(); err != nil {
		return err
	}

	raw := make([]byte, speakerEmbeddingDim*4)
	for i, v := range embedding {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	meta := SpeakerProfileMeta{ID: id, CreatedAt: time.Now().UTC()}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal speaker meta: %w", err)
	}

	embedPath := s.embedPath(id)
	metaPath := s.metaPath(id)
	embedTmp := embedPath + ".tmp"
	metaTmp := metaPath + ".tmp"

	// Keep the previous embedding so a failed meta commit can roll the voice back.
	prevEmbed, prevErr := os.ReadFile(embedPath)
	if prevErr != nil && !os.IsNotExist(prevErr) {
		return prevErr
	}
	hadPrev := prevErr == nil

	if err := os.WriteFile(embedTmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(metaTmp, metaRaw, 0o600); err != nil {
		_ = os.Remove(embedTmp)
		return err
	}
	if err := os.Rename(embedTmp, embedPath); err != nil {
		_ = os.Remove(embedTmp)
		_ = os.Remove(metaTmp)
		return err
	}
	if err := os.Rename(metaTmp, metaPath); err != nil {
		_ = os.Remove(metaTmp)
		if hadPrev {
			if restoreErr := os.WriteFile(embedPath, prevEmbed, 0o600); restoreErr != nil {
				return fmt.Errorf("commit speaker meta: %w (restore embed: %v)", err, restoreErr)
			}
		} else if restoreErr := os.Remove(embedPath); restoreErr != nil && !os.IsNotExist(restoreErr) {
			return fmt.Errorf("commit speaker meta: %w (remove embed: %v)", err, restoreErr)
		}
		return fmt.Errorf("commit speaker meta: %w", err)
	}
	// Only after the canonical owner is durable, drop legacy extras.
	if err := s.removeOthersLocked(id); err != nil {
		logging.Warnf("agent", "speaker", "cleanup legacy profiles after save: %v", err)
	}
	return nil
}

func (s *SpeakerProfileStore) Delete(id string) error {
	id = strings.TrimSpace(id)
	if !speakerProfileIDPattern.MatchString(id) {
		return fmt.Errorf("invalid speaker profile id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	metas, err := s.listLocked()
	if err != nil {
		return err
	}
	owner, ok := selectOwnerMeta(metas)
	if !ok {
		return fmt.Errorf("speaker profile %q not found", id)
	}
	// Single-owner product: deleting the logical owner (by canonical id or the
	// id List currently surfaces) clears every on-disk profile so a stale
	// legacy file cannot resurrect on the next List/LoadOwner.
	if id != owner.ID && id != speakerOwnerProfileID {
		return fmt.Errorf("speaker profile %q not found", id)
	}
	var first error
	foundEmbed := false
	for _, meta := range metas {
		_ = os.Remove(s.metaPath(meta.ID))
		removeErr := os.Remove(s.embedPath(meta.ID))
		if removeErr == nil {
			foundEmbed = true
			continue
		}
		if os.IsNotExist(removeErr) {
			continue
		}
		if first == nil {
			first = fmt.Errorf("remove embed %s: %w", meta.ID, removeErr)
		}
	}
	if !foundEmbed {
		return fmt.Errorf("speaker profile %q not found", id)
	}
	return first
}

// LoadOwner returns the single enrolled owner embedding, if any.
// It never deletes on-disk profiles; Save consolidates to the canonical id.
func (s *SpeakerProfileStore) LoadOwner() ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metas, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	owner, ok := selectOwnerMeta(metas)
	if !ok {
		return nil, nil
	}
	return s.readEmbeddingLocked(owner.ID)
}

func (s *SpeakerProfileStore) LoadAll() ([][]float32, error) {
	owner, err := s.LoadOwner()
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, nil
	}
	return [][]float32{owner}, nil
}

func (s *SpeakerProfileStore) listLocked() ([]SpeakerProfileMeta, error) {
	if err := s.EnsureReady(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []SpeakerProfileMeta
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".f32") || entry.IsDir() {
			continue
		}
		id := strings.TrimSuffix(name, ".f32")
		info, err := entry.Info()
		created := time.Time{}
		if err == nil {
			created = info.ModTime().UTC()
		}
		meta := SpeakerProfileMeta{ID: id, CreatedAt: created}
		if raw, err := os.ReadFile(s.metaPath(id)); err == nil {
			_ = json.Unmarshal(raw, &meta)
			if meta.ID == "" {
				meta.ID = id
			}
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// selectOwnerMeta picks the single logical owner without mutating disk.
// Prefer the newest profile by CreatedAt so a later re-enroll under a legacy
// id (speaker3) wins over a stale canonical speaker trial recording.
func selectOwnerMeta(metas []SpeakerProfileMeta) (SpeakerProfileMeta, bool) {
	if len(metas) == 0 {
		return SpeakerProfileMeta{}, false
	}
	return metas[len(metas)-1], true
}

func (s *SpeakerProfileStore) removeOthersLocked(keepID string) error {
	metas, err := s.listLocked()
	if err != nil {
		return err
	}
	var first error
	for _, meta := range metas {
		if meta.ID == keepID {
			continue
		}
		if err := os.Remove(s.metaPath(meta.ID)); err != nil && !os.IsNotExist(err) && first == nil {
			first = fmt.Errorf("remove meta %s: %w", meta.ID, err)
		}
		if err := os.Remove(s.embedPath(meta.ID)); err != nil && !os.IsNotExist(err) && first == nil {
			first = fmt.Errorf("remove embed %s: %w", meta.ID, err)
		}
	}
	return first
}

func (s *SpeakerProfileStore) readEmbeddingLocked(id string) ([]float32, error) {
	raw, err := os.ReadFile(s.embedPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(raw) != speakerEmbeddingDim*4 {
		return nil, nil
	}
	vec := make([]float32, speakerEmbeddingDim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return normalizeSpeakerEmbedding(vec)
}

func (s *SpeakerProfileStore) embedPath(id string) string {
	return filepath.Join(s.dir, id+".f32")
}

func (s *SpeakerProfileStore) metaPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

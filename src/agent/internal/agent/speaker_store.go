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
	if err := s.pruneToSingleLocked(""); err != nil {
		return nil, err
	}
	return s.listLocked()
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
	// Single owner: drop every prior profile before writing the new one.
	if err := s.deleteAllLocked(); err != nil {
		return err
	}

	raw := make([]byte, speakerEmbeddingDim*4)
	for i, v := range embedding {
		binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(v))
	}
	embedPath := s.embedPath(id)
	tmp := embedPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, embedPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	meta := SpeakerProfileMeta{ID: id, CreatedAt: time.Now().UTC()}
	metaRaw, _ := json.Marshal(meta)
	_ = os.WriteFile(s.metaPath(id), metaRaw, 0o600)
	return nil
}

func (s *SpeakerProfileStore) Delete(id string) error {
	id = strings.TrimSpace(id)
	if !speakerProfileIDPattern.MatchString(id) {
		return fmt.Errorf("invalid speaker profile id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Remove(s.metaPath(id))
	err := os.Remove(s.embedPath(id))
	if os.IsNotExist(err) {
		return fmt.Errorf("speaker profile %q not found", id)
	}
	return err
}

// LoadOwner returns the single enrolled owner embedding, if any.
func (s *SpeakerProfileStore) LoadOwner() ([]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.pruneToSingleLocked(""); err != nil {
		return nil, err
	}
	metas, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, nil
	}
	return s.readEmbeddingLocked(metas[0].ID)
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

// pruneToSingleLocked keeps at most one profile. Prefer keepID, then the
// canonical owner id, then the newest profile. Used so boards that previously
// enrolled multiple speakers collapse to single-owner matching.
func (s *SpeakerProfileStore) pruneToSingleLocked(keepID string) error {
	metas, err := s.listLocked()
	if err != nil {
		return err
	}
	if len(metas) <= 1 {
		return nil
	}
	keepID = strings.TrimSpace(keepID)
	keepIdx := -1
	if keepID != "" {
		for i, meta := range metas {
			if meta.ID == keepID {
				keepIdx = i
				break
			}
		}
	}
	if keepIdx < 0 {
		for i, meta := range metas {
			if meta.ID == speakerOwnerProfileID {
				keepIdx = i
				break
			}
		}
	}
	if keepIdx < 0 {
		keepIdx = len(metas) - 1 // newest after CreatedAt ascending sort
	}
	for i, meta := range metas {
		if i == keepIdx {
			continue
		}
		_ = os.Remove(s.metaPath(meta.ID))
		_ = os.Remove(s.embedPath(meta.ID))
	}
	return nil
}

func (s *SpeakerProfileStore) deleteAllLocked() error {
	metas, err := s.listLocked()
	if err != nil {
		return err
	}
	for _, meta := range metas {
		_ = os.Remove(s.metaPath(meta.ID))
		_ = os.Remove(s.embedPath(meta.ID))
	}
	return nil
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

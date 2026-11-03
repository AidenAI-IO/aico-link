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
	if maxProfiles <= 0 {
		maxProfiles = defaultSpeakerMaxProfiles
	}
	return &SpeakerProfileStore{dir: dir, maxProfiles: maxProfiles}
}

func (s *SpeakerProfileStore) Dir() string { return s.dir }

func (s *SpeakerProfileStore) MaxProfiles() int { return s.maxProfiles }

func (s *SpeakerProfileStore) EnsureReady() error {
	return os.MkdirAll(s.dir, 0o700)
}

func (s *SpeakerProfileStore) List() ([]SpeakerProfileMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

func (s *SpeakerProfileStore) Count() (int, error) {
	list, err := s.List()
	return len(list), err
}

func (s *SpeakerProfileStore) Save(id string, embedding []float32) error {
	id = strings.TrimSpace(id)
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
	existing, err := s.listLocked()
	if err != nil {
		return err
	}
	replacing := false
	for _, meta := range existing {
		if meta.ID == id {
			replacing = true
			break
		}
	}
	if !replacing && len(existing) >= s.maxProfiles {
		return fmt.Errorf("speaker profile limit reached (%d)", s.maxProfiles)
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

func (s *SpeakerProfileStore) LoadAll() ([][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	metas, err := s.listLocked()
	if err != nil {
		return nil, err
	}
	var out [][]float32
	for _, meta := range metas {
		raw, err := os.ReadFile(s.embedPath(meta.ID))
		if err != nil {
			continue
		}
		if len(raw) != speakerEmbeddingDim*4 {
			continue
		}
		vec := make([]float32, speakerEmbeddingDim)
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		normed, err := normalizeSpeakerEmbedding(vec)
		if err != nil {
			continue
		}
		out = append(out, normed)
	}
	return out, nil
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

func (s *SpeakerProfileStore) embedPath(id string) string {
	return filepath.Join(s.dir, id+".f32")
}

func (s *SpeakerProfileStore) metaPath(id string) string {
	return filepath.Join(s.dir, id+".json")
}

package contextmanager

import (
	"aico-agent/internal/agent/session"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	artifactMetadataMaxBytes = 64 * 1024

	artifactDefaultTTL   = 7 * 24 * time.Hour
	artifactSensitiveTTL = time.Hour
)

// Keep the public context-manager names stable while the data types belong to
// the session package.
type ArtifactMetadata = session.ArtifactMetadata
type ArtifactFile = session.ArtifactFile

const (
	ArtifactSingleMaxBytes  = session.ArtifactSingleMaxBytes
	ArtifactSessionMaxBytes = session.ArtifactSessionMaxBytes
)

var (
	ErrArtifactTooLarge    = session.ErrArtifactTooLarge
	ErrArtifactSessionFull = session.ErrArtifactSessionFull

	// artifactFilesystemMu coordinates stores with the cleaner, which operates
	// through a separate object and therefore cannot use artifactStore.mu.
	artifactFilesystemMu sync.RWMutex
)

type artifactStore struct {
	root string
	mu   sync.Mutex
}

func (s *artifactStore) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

// ArtifactPathRecoverable reports whether an artifact data path still has a
// readable, unexpired metadata pair. Missing, malformed, or expired metadata
// fails closed so compaction does not keep advertising an unusable path.
func ArtifactPathRecoverable(dataPath string, now time.Time) bool {
	artifactFilesystemMu.RLock()
	defer artifactFilesystemMu.RUnlock()

	dataPath = strings.TrimSpace(dataPath)
	if dataPath == "" || !strings.HasSuffix(dataPath, ".data") {
		return false
	}
	info, err := os.Stat(dataPath)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	metadataPath := strings.TrimSuffix(dataPath, ".data") + ".json"
	pathInfo, err := os.Lstat(metadataPath)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() > artifactMetadataMaxBytes {
		return false
	}
	file, err := os.Open(metadataPath)
	if err != nil {
		return false
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil || !openInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openInfo) || openInfo.Size() > artifactMetadataMaxBytes {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, artifactMetadataMaxBytes+1))
	if err != nil || len(data) > artifactMetadataMaxBytes {
		return false
	}
	var metadata ArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return false
	}
	if metadata.ExpiresAt.IsZero() {
		return false
	}
	return metadata.ExpiresAt.After(now.UTC())
}

func newArtifactStore(sessionFolder, sessionID string) (*artifactStore, error) {
	root, err := artifactStoreRoot(sessionFolder, sessionID)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create artifact directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure artifact directory: %w", err)
	}
	return &artifactStore{root: root}, nil
}

func artifactStoreRoot(sessionFolder, sessionID string) (string, error) {
	sessionID, err := validateArtifactSessionID(sessionID)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(filepath.Join(sessionDataDir(sessionFolder, sessionID), "tool-results"))
	if err != nil {
		return "", fmt.Errorf("resolve artifact directory: %w", err)
	}
	return root, nil
}

func validateArtifactSessionID(sessionID string) (string, error) {
	trimmed := strings.TrimSpace(sessionID)
	if trimmed == "" || trimmed == "." || trimmed == ".." || filepath.Base(trimmed) != trimmed || strings.ContainsAny(trimmed, `/\\`) {
		return "", fmt.Errorf("invalid artifact session ID")
	}
	return trimmed, nil
}

func (s *artifactStore) store(mimeType string, data []byte, metadata ArtifactMetadata) (ArtifactFile, error) {
	if s == nil || strings.TrimSpace(s.root) == "" {
		return ArtifactFile{}, fmt.Errorf("artifact store is closed")
	}
	if len(data) > ArtifactSingleMaxBytes {
		return ArtifactFile{}, ErrArtifactTooLarge
	}

	artifactFilesystemMu.Lock()
	defer artifactFilesystemMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()

	hash := sha256.Sum256(data)
	metadata.MIMEType = strings.TrimSpace(mimeType)
	if metadata.MIMEType == "" {
		metadata.MIMEType = "text/plain"
	}
	metadata.Size = int64(len(data))
	metadata.SHA256 = hex.EncodeToString(hash[:])
	metadata.CreatedAt = time.Now().UTC()
	ttl := artifactDefaultTTL
	if metadata.Sensitive {
		ttl = artifactSensitiveTTL
	}
	if metadata.ExpiresAt.IsZero() {
		metadata.ExpiresAt = metadata.CreatedAt.Add(ttl)
	}
	metadata.Complete = true
	if existing, found, err := findReusableArtifact(s.root, metadata); err != nil {
		return ArtifactFile{}, err
	} else if found {
		return existing, nil
	}

	metadataData, err := json.Marshal(metadata)
	if err != nil {
		return ArtifactFile{}, fmt.Errorf("marshal artifact metadata: %w", err)
	}
	used, err := artifactSessionBytes(s.root)
	if err != nil {
		return ArtifactFile{}, err
	}
	if used+int64(len(data))+int64(len(metadataData)) > ArtifactSessionMaxBytes {
		return ArtifactFile{}, ErrArtifactSessionFull
	}

	id := "tr_" + uuid.NewString()
	dataPath := filepath.Join(s.root, id+".data")
	if err := writeArtifactFileAtomically(dataPath, data); err != nil {
		return ArtifactFile{}, err
	}
	if err := writeArtifactFileAtomically(filepath.Join(s.root, id+".json"), metadataData); err != nil {
		_ = os.Remove(dataPath)
		return ArtifactFile{}, err
	}

	return ArtifactFile{
		Path:     dataPath,
		Size:     metadata.Size,
		SHA256:   metadata.SHA256,
		Complete: true,
	}, nil
}

// StoreArtifact implements session.ArtifactStore while keeping the concrete
// filesystem store private to the storage layer.
func (s *artifactStore) StoreArtifact(mimeType string, data []byte, metadata session.ArtifactMetadata) (session.ArtifactFile, error) {
	return s.store(mimeType, data, metadata)
}

func findReusableArtifact(root string, requested ArtifactMetadata) (ArtifactFile, bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return ArtifactFile{}, false, fmt.Errorf("list artifact directory for deduplication: %w", err)
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		metadataPath := filepath.Join(root, entry.Name())
		existing, ok := readArtifactMetadataFile(metadataPath)
		if !ok || !existing.Complete || !existing.ExpiresAt.After(now) {
			continue
		}
		if existing.Size != requested.Size ||
			!strings.EqualFold(existing.SHA256, requested.SHA256) ||
			strings.TrimSpace(existing.MIMEType) != requested.MIMEType ||
			existing.Sensitive != requested.Sensitive {
			continue
		}
		dataPath := strings.TrimSuffix(metadataPath, ".json") + ".data"
		if !artifactDataMatches(dataPath, existing.Size, requested.SHA256) {
			continue
		}
		if requested.ExpiresAt.After(existing.ExpiresAt) {
			existing.ExpiresAt = requested.ExpiresAt
			metadataData, marshalErr := json.Marshal(existing)
			if marshalErr != nil {
				return ArtifactFile{}, false, fmt.Errorf("marshal reused artifact metadata: %w", marshalErr)
			}
			if writeErr := writeArtifactFileAtomically(metadataPath, metadataData); writeErr != nil {
				return ArtifactFile{}, false, writeErr
			}
		}
		return ArtifactFile{
			Path:     dataPath,
			Size:     existing.Size,
			SHA256:   existing.SHA256,
			Complete: true,
		}, true, nil
	}
	return ArtifactFile{}, false, nil
}

func readArtifactMetadataFile(path string) (ArtifactMetadata, bool) {
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() > artifactMetadataMaxBytes {
		return ArtifactMetadata{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return ArtifactMetadata{}, false
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil || !openInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openInfo) || openInfo.Size() > artifactMetadataMaxBytes {
		return ArtifactMetadata{}, false
	}
	data, err := io.ReadAll(io.LimitReader(file, artifactMetadataMaxBytes+1))
	if err != nil || len(data) > artifactMetadataMaxBytes {
		return ArtifactMetadata{}, false
	}
	var metadata ArtifactMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return ArtifactMetadata{}, false
	}
	return metadata, true
}

func artifactDataMatches(path string, expectedSize int64, expectedSHA256 string) bool {
	pathInfo, err := os.Lstat(path)
	if err != nil || !pathInfo.Mode().IsRegular() || pathInfo.Size() != expectedSize {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil || !openInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openInfo) || openInfo.Size() != expectedSize {
		return false
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return false
	}
	return strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedSHA256)
}

func artifactSessionBytes(root string) (int64, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("list artifact directory: %w", err)
	}
	var total int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, fmt.Errorf("stat artifact: %w", err)
		}
		total += info.Size()
	}
	return total, nil
}

func writeArtifactFileAtomically(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".artifact-*")
	if err != nil {
		return fmt.Errorf("create artifact temp file: %w", err)
	}
	tempPath := file.Name()
	defer os.Remove(tempPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("chmod artifact temp file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write artifact temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync artifact temp file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close artifact temp file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install artifact file: %w", err)
	}
	return nil
}

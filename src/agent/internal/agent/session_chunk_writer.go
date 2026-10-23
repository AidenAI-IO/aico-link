package agent

import (
	"aiden-agent/internal/agent/messages"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// SessionChunkWriter writes conversation chunks to session directories.
//
// Chunks are the only record of conversation spans that compaction removed from
// the live transcript, so the writer also derives the tags and entities that
// recall_session_chunks searches on.
type SessionChunkWriter struct {
	sessionFolder string
	extraction    MemoryExtractionConfig
}

// pendingSessionChunk stages a speculative summary's history in memory until
// the caller has validated and activated the corresponding context revision.
type pendingSessionChunk struct {
	sessionID string
	messages  []messages.Message
	summary   string
}

func (p *pendingSessionChunk) WriteChunk(ctx context.Context, sessionID string, msgs []messages.Message, summary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.sessionID = sessionID
	p.messages = messages.CloneList(msgs)
	p.summary = summary
	return nil
}

func (p *pendingSessionChunk) persist(ctx context.Context, writer *SessionChunkWriter) error {
	if p.sessionID == "" {
		return nil
	}
	return writer.WriteChunk(ctx, p.sessionID, p.messages, p.summary)
}

// NewSessionChunkWriter creates a chunk writer for the given session folder.
func NewSessionChunkWriter(sessionFolder string, extraction ...MemoryExtractionConfig) *SessionChunkWriter {
	cfg := DefaultMemoryExtractionConfig()
	if len(extraction) > 0 {
		cfg = normalizeMemoryExtractionConfig(extraction[0])
	}
	return &SessionChunkWriter{sessionFolder: sessionFolder, extraction: cfg}
}

// chunkSearchTerms derives the tags and entities recall matches on from the
// compacted messages. It reuses the same deterministic extraction the Episode
// path uses, so a chunk is searchable by the vocabulary configured in
// memory/extraction.yaml.
func (w *SessionChunkWriter) chunkSearchTerms(msgs []messages.Message, summary string) (tags, entities []string) {
	var text strings.Builder
	text.WriteString(summary)
	for _, msg := range msgs {
		text.WriteByte('\n')
		text.WriteString(msg.Content)
	}
	content := text.String()
	return dedupeStrings(w.extraction.extractTagsFromText(content)),
		dedupeStrings(w.extraction.extractEntitiesFromText(content))
}

func dedupeStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WriteChunk writes a chunk of messages with their summary to the session's chunks directory.
func (w *SessionChunkWriter) WriteChunk(ctx context.Context, sessionID string, msgs []messages.Message, summary string) error {
	// A nil writer is wrapped in a non-nil ChunkWriter interface by the caller,
	// so guard the receiver here rather than relying on an interface nil check.
	if w == nil || w.sessionFolder == "" || sessionID == "" {
		return nil
	}
	if len(msgs) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// All chunks go into a shared flat directory
	chunksDir := filepath.Join(w.sessionFolder, "chunks")
	if err := os.MkdirAll(chunksDir, 0o755); err != nil {
		return fmt.Errorf("create chunks directory: %w", err)
	}

	// Appending to the index is a read-modify-write, so serialize it on the
	// index itself. The lock cannot live on SessionChunkWriter: compaction builds
	// a fresh writer per run, so a per-instance mutex would hand concurrent
	// compactions distinct locks and serialize nothing. flock is held per open
	// file description, so this also covers two goroutines in this process, not
	// just a second process sharing the config dir.
	fl := NewFileLock(chunksDir)
	if err := fl.Lock(defaultLockTimeout); err != nil {
		return fmt.Errorf("lock chunk index: %w", err)
	}
	defer fl.Unlock()

	// Generate chunk ID
	chunkID := "chunk_" + uuid.New().String()

	// Write chunk messages to JSONL
	chunkFile := filepath.Join(chunksDir, chunkID+".jsonl")
	file, err := os.Create(chunkFile)
	if err != nil {
		return fmt.Errorf("create chunk file: %w", err)
	}
	var chunkComplete bool
	defer func() {
		file.Close()
		// If context was canceled or another error prevented us from updating the
		// index, remove the orphaned chunk file: recall expects every .jsonl file
		// to have a matching index entry, and a partial file could be corrupt.
		if !chunkComplete {
			os.Remove(chunkFile)
		}
	}()

	for _, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message: %w", err)
		}
		if _, err := file.Write(data); err != nil {
			return fmt.Errorf("write message: %w", err)
		}
		if _, err := file.WriteString("\n"); err != nil {
			return fmt.Errorf("write newline: %w", err)
		}
	}

	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync chunk file: %w", err)
	}

	// Check context before the index update: if canceled here, the deferred
	// cleanup removes the chunk file we just wrote, preserving the invariant
	// that every .jsonl has an index entry.
	if err := ctx.Err(); err != nil {
		return err
	}

	// Update index
	indexPath := filepath.Join(chunksDir, "index.yaml")
	index, err := loadChunkIndexFromPath(indexPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("load chunk index: %w", err)
	}

	// Calculate checksum
	checksum := calculateChunkChecksum(msgs)

	// Derive the searchable terms now: recall filters on these fields, so a
	// chunk written without them would be reachable only by chunk_id.
	tags, entities := w.chunkSearchTerms(msgs, summary)

	// Create index entry with session_id and created_at
	now := time.Now().UTC().Format(time.RFC3339)
	entry := chunkIndexEntry{
		ID:         chunkID,
		SessionID:  sessionID,
		File:       chunkID + ".jsonl",
		Status:     "active",
		Summary:    summary,
		Tags:       tags,
		Entities:   entities,
		EventCount: len(msgs),
		Checksum:   checksum,
		CreatedAt:  now,
	}

	index.Version = 1
	index.UpdatedAt = now
	index.Chunks = append(index.Chunks, entry)

	// Write index as YAML
	indexData, err := yaml.Marshal(index)
	if err != nil {
		return fmt.Errorf("marshal chunk index: %w", err)
	}

	if err := writeFileAtomic(indexPath, indexData, 0o644); err != nil {
		return fmt.Errorf("write chunk index: %w", err)
	}

	// Mark complete so the deferred cleanup doesn't remove the file.
	chunkComplete = true
	return nil
}

func calculateChunkChecksum(msgs []messages.Message) string {
	h := sha256.New()
	for _, msg := range msgs {
		h.Write([]byte(msg.Role))
		h.Write([]byte(msg.Content))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func loadChunkIndexFromPath(path string) (chunkIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return chunkIndex{Version: 1, Chunks: []chunkIndexEntry{}}, nil
		}
		return chunkIndex{}, err
	}

	var index chunkIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return chunkIndex{}, fmt.Errorf("unmarshal chunk index: %w", err)
	}

	return index, nil
}

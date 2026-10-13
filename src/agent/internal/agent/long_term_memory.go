package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ProfileFn synthesizes a user profile from a list of memory entries.
// Each entry has Type (profile/rule/preference) and Content.
type ProfileFn func(ctx context.Context, entries []ProfileEntry) string

type ProfileEntry struct {
	Type    string
	Content string
}

type LongTermMemoryStore struct {
	rootDir          string
	lifecycleDir     string
	profileFn        ProfileFn
	profileDebouncer *ProfileDebouncer
	writeMu          *sync.Mutex
	cacheMu          sync.Mutex
	indexCache       longTermIndexCache
	parsedCache      parsedMemoryCache
}

var longTermMemoryStoreMutexes sync.Map

func longTermMemoryStoreMutex(rootDir string) *sync.Mutex {
	key := filepath.Clean(rootDir)
	mutex, _ := longTermMemoryStoreMutexes.LoadOrStore(key, &sync.Mutex{})
	return mutex.(*sync.Mutex)
}

type longTermIndexCache struct {
	signature memoryFileSignature
	index     memoryIndex
	valid     bool
}

const memoryIndexVersion = 2

type LongTermMemoryOption func(*LongTermMemoryStore)

func WithLifecycleDir(dir string) LongTermMemoryOption {
	return func(s *LongTermMemoryStore) { s.lifecycleDir = dir }
}

func WithStoreProfileFn(fn ProfileFn) LongTermMemoryOption {
	return func(s *LongTermMemoryStore) { s.profileFn = fn }
}

func withParsedCacheCapacity(capacity int) LongTermMemoryOption {
	return func(s *LongTermMemoryStore) { s.parsedCache.init(capacity) }
}

// MemoryTypeScreenSnapshot is the long-term memory type for a Screen Memory:
// what was on the controlled device's screen at one moment, recorded by a
// deliberate button press.
//
// It is its own type rather than a fact so that it can be retrieved as a group,
// is excluded from the User Profile by isProfileRelevantType, and is exempt
// from the outcome feedback that applies to inferred memories.
const MemoryTypeScreenSnapshot = "screen_snapshot"

// MemorySourceTypeScreenCapture marks a Screen Memory's provenance.
const MemorySourceTypeScreenCapture = "screen_capture"

func isScreenMemoryType(memoryType string) bool {
	return memoryType == MemoryTypeScreenSnapshot
}

type MemorySourceRef struct {
	Type     string   `yaml:"type" json:"type"`
	ID       string   `yaml:"id" json:"id"`
	EventIDs []string `yaml:"event_ids,omitempty" json:"event_ids,omitempty"`
}

type MemoryItem struct {
	ID               string            `yaml:"id"`
	Type             string            `yaml:"type"`
	Status           string            `yaml:"status"`
	Revision         int               `yaml:"revision,omitempty"`
	Priority         int               `yaml:"priority"`
	Confidence       float64           `yaml:"confidence"`
	Tags             []string          `yaml:"tags,omitempty"`
	Entities         []string          `yaml:"entities,omitempty"`
	TimeScope        string            `yaml:"time_scope"`
	CreatedAt        string            `yaml:"created_at"`
	UpdatedAt        string            `yaml:"updated_at"`
	TTL              string            `yaml:"ttl,omitempty"`
	ExpiresAt        string            `yaml:"expires_at,omitempty"`
	Applicability    map[string]string `yaml:"applicability,omitempty"`
	SourceRefs       []MemorySourceRef `yaml:"source_refs,omitempty"`
	EvidenceRefs     []MemorySourceRef `yaml:"evidence_refs,omitempty"`
	LastValidatedAt  string            `yaml:"last_validated_at,omitempty"`
	SuccessCount     int               `yaml:"success_count,omitempty"`
	FailureCount     int               `yaml:"failure_count,omitempty"`
	ConflictsWith    []string          `yaml:"conflicts_with,omitempty"`
	Supersedes       string            `yaml:"supersedes,omitempty"`
	SupersededBy     string            `yaml:"superseded_by,omitempty"`
	Traceability     string            `yaml:"traceability"`
	Title            string            `yaml:"-"`
	Content          string            `yaml:"-"`
	EvidenceExcerpts []string          `yaml:"-"`
}

type MemoryQuery struct {
	Tags     []string
	Entities []string
	Types    []string
	Limit    int
}

type MemoryResult struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Status        string            `json:"status"`
	Revision      int               `json:"revision,omitempty"`
	Title         string            `json:"title"`
	Summary       string            `json:"summary"`
	Content       string            `json:"content"`
	Priority      int               `json:"priority"`
	Confidence    float64           `json:"confidence"`
	Tags          []string          `json:"tags,omitempty"`
	Entities      []string          `json:"entities,omitempty"`
	FilePath      string            `json:"file_path"`
	MemoryScope   string            `json:"memory_scope,omitempty"`
	ExpiresAt     string            `json:"expires_at,omitempty"`
	Applicability map[string]string `json:"applicability,omitempty"`
	SourceRefs    []MemorySourceRef `json:"source_refs,omitempty"`
	EvidenceRefs  []MemorySourceRef `json:"evidence_refs,omitempty"`
}

type memoryIndex struct {
	Version   int                `yaml:"version"`
	UpdatedAt string             `yaml:"updated_at"`
	Memories  []memoryIndexEntry `yaml:"memories"`
}

type memoryIndexEntry struct {
	ID         string   `yaml:"id"`
	File       string   `yaml:"file"`
	Type       string   `yaml:"type"`
	Status     string   `yaml:"status"`
	ExpiresAt  string   `yaml:"expires_at,omitempty"`
	Priority   int      `yaml:"priority"`
	Confidence float64  `yaml:"confidence"`
	Tags       []string `yaml:"tags,omitempty"`
	Entities   []string `yaml:"entities,omitempty"`
	Summary    string   `yaml:"summary"`
}

type scoredMemoryResult struct {
	Result MemoryResult
	Score  int
}

func NewLongTermMemoryStore(rootDir string, opts ...LongTermMemoryOption) *LongTermMemoryStore {
	s := &LongTermMemoryStore{rootDir: rootDir, writeMu: longTermMemoryStoreMutex(rootDir)}
	s.parsedCache.init(defaultParsedMemoryCacheCapacity)
	for _, opt := range opts {
		opt(s)
	}
	if s.lifecycleDir == "" {
		s.lifecycleDir = filepath.Join(rootDir, "..", "lifecycle")
	}
	return s
}

func (s *LongTermMemoryStore) RootDir() string {
	return s.rootDir
}

func (s *LongTermMemoryStore) AddMemory(ctx context.Context, item MemoryItem) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.addMemoryLocked(ctx, item)
}

func (s *LongTermMemoryStore) addMemoryLocked(ctx context.Context, item MemoryItem) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	if strings.TrimSpace(item.Content) == "" {
		return "", errors.New("memory content is required")
	}
	if len(item.EvidenceExcerpts) == 0 {
		return "", errors.New("memory evidence excerpt is required")
	}
	item = normalizeMemoryItem(item, time.Now().UTC())
	path := s.memoryPath(item.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create memory directory: %w", err)
	}
	if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(item)), 0o644); err != nil {
		return "", fmt.Errorf("write memory %q: %w", item.ID, err)
	}
	s.invalidateParsedMemoryCache(path)
	if err := s.rebuildIndexLocked(ctx); err != nil {
		return "", err
	}
	return item.ID, nil
}

func (s *LongTermMemoryStore) Search(ctx context.Context, query MemoryQuery) ([]MemoryResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.searchLocked(ctx, query)
}

func (s *LongTermMemoryStore) searchAll(ctx context.Context) ([]MemoryResult, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	index, err := s.loadIndexLocked(ctx)
	if err != nil {
		return nil, err
	}
	return s.searchLocked(ctx, MemoryQuery{Limit: len(index.Memories)})
}

func (s *LongTermMemoryStore) searchLocked(ctx context.Context, query MemoryQuery) ([]MemoryResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	index, err := s.loadIndexLocked(ctx)
	if err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 5
	}

	matches := make([]scoredMemoryResult, 0)
	matchAll := memoryQueryIsEmpty(query)
	now := time.Now().UTC()
	var expiredPaths []string
	for _, entry := range index.Memories {
		if entry.Status != "active" {
			continue
		}
		// Failure lessons are now owned by Episode Memory consolidation.
		// Keep legacy long-term files for traceability, but do not recall them.
		if entry.Type == "failure" {
			continue
		}
		parsed, path, expired, err := s.resolveActiveEntry(entry, now)
		if expired {
			expiredPaths = append(expiredPaths, path)
			continue
		}
		if err != nil {
			continue
		}
		score := scoreMemoryEntry(query, entry, parsed)
		if score == 0 && !matchAll {
			continue
		}
		matches = append(matches, scoredMemoryResult{Score: score, Result: MemoryResult{
			ID:            entry.ID,
			Type:          entry.Type,
			Status:        entry.Status,
			Revision:      parsed.Item.Revision,
			Title:         parsed.Title,
			Summary:       entry.Summary,
			Content:       parsed.Content,
			Priority:      entry.Priority,
			Confidence:    entry.Confidence,
			Tags:          append([]string(nil), entry.Tags...),
			Entities:      append([]string(nil), entry.Entities...),
			FilePath:      path,
			ExpiresAt:     firstNonEmpty(parsed.Item.ExpiresAt, entry.ExpiresAt),
			Applicability: cloneStringMap(parsed.Item.Applicability),
			SourceRefs:    append([]MemorySourceRef(nil), parsed.Item.SourceRefs...),
			EvidenceRefs:  append([]MemorySourceRef(nil), parsed.Item.EvidenceRefs...),
		}})
	}
	if len(expiredPaths) > 0 {
		s.invalidateParsedMemoryCache(expiredPaths...)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Score != matches[j].Score {
			return matches[i].Score > matches[j].Score
		}
		if matches[i].Result.Priority != matches[j].Result.Priority {
			return matches[i].Result.Priority > matches[j].Result.Priority
		}
		if matches[i].Result.Confidence != matches[j].Result.Confidence {
			return matches[i].Result.Confidence > matches[j].Result.Confidence
		}
		iScreen := isScreenMemoryType(matches[i].Result.Type)
		jScreen := isScreenMemoryType(matches[j].Result.Type)
		if iScreen != jScreen {
			return iScreen
		}
		// Screen Memory answers "the one I just saved", so tied snapshots sort
		// newest-first. Other memory types preserve their existing ID order.
		if iScreen {
			return matches[i].Result.ID > matches[j].Result.ID
		}
		return matches[i].Result.ID < matches[j].Result.ID
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	results := make([]MemoryResult, 0, len(matches))
	for _, match := range matches {
		results = append(results, match.Result)
	}
	return results, nil
}

// resolveActiveEntry loads an index entry and reports whether its backing memory has expired.
func (s *LongTermMemoryStore) resolveActiveEntry(entry memoryIndexEntry, now time.Time) (parsedMemoryMarkdown, string, bool, error) {
	path := filepath.Join(s.rootDir, entry.File)
	if memoryExpiresAtPassed(entry.ExpiresAt, now) {
		return parsedMemoryMarkdown{}, path, true, nil
	}
	parsed, err := s.readMemoryMarkdownCached(path)
	if err != nil {
		return parsedMemoryMarkdown{}, path, false, err
	}
	if memoryItemExpired(parsed.Item, now) {
		return parsedMemoryMarkdown{}, path, true, nil
	}
	return parsed, path, false, nil
}

func (s *LongTermMemoryStore) Forget(ctx context.Context, id string, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.forgetLocked(ctx, id, reason)
}

func (s *LongTermMemoryStore) forgetLocked(ctx context.Context, id string, reason string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	path := s.memoryPath(id)
	parsed, err := readMemoryMarkdown(path)
	if err != nil {
		return err
	}
	parsed.Item.Status = "deleted"
	parsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
		return fmt.Errorf("write forgotten memory %q: %w", id, err)
	}
	s.invalidateParsedMemoryCache(path)
	if err := s.appendTombstone(id, reason); err != nil {
		return err
	}
	return s.rebuildIndexLocked(ctx)
}

func (s *LongTermMemoryStore) SupersedeMemory(ctx context.Context, oldID string, newItem MemoryItem) (string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.supersedeMemoryLocked(ctx, oldID, newItem)
}

func (s *LongTermMemoryStore) supersedeMemoryLocked(ctx context.Context, oldID string, newItem MemoryItem) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	oldPath := s.memoryPath(oldID)
	oldParsed, err := readMemoryMarkdown(oldPath)
	if err != nil {
		return "", fmt.Errorf("read old memory %q for supersede: %w", oldID, err)
	}

	newItem = normalizeMemoryItem(newItem, time.Now().UTC())
	newItem.Supersedes = oldID

	oldParsed.Item.Status = "replaced"
	oldParsed.Item.SupersededBy = newItem.ID
	oldParsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeFileAtomic(oldPath, []byte(formatMemoryMarkdown(oldParsed.Item)), 0o644); err != nil {
		return "", fmt.Errorf("update superseded memory %q: %w", oldID, err)
	}
	s.invalidateParsedMemoryCache(oldPath)

	newPath := s.memoryPath(newItem.ID)
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return "", fmt.Errorf("create memory directory: %w", err)
	}
	if err := writeFileAtomic(newPath, []byte(formatMemoryMarkdown(newItem)), 0o644); err != nil {
		return "", fmt.Errorf("write superseding memory %q: %w", newItem.ID, err)
	}
	s.invalidateParsedMemoryCache(newPath)
	if err := s.rebuildIndexLocked(ctx); err != nil {
		return "", err
	}
	return newItem.ID, nil
}

func (s *LongTermMemoryStore) MarkConflict(ctx context.Context, aID string, bID string, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.markConflictLocked(ctx, aID, bID, reason)
}

func (s *LongTermMemoryStore) markConflictLocked(ctx context.Context, aID string, bID string, reason string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	for _, id := range []string{aID, bID} {
		path := s.memoryPath(id)
		parsed, err := readMemoryMarkdown(path)
		if err != nil {
			continue
		}
		if parsed.Item.Status == "active" {
			parsed.Item.Status = "conflicted"
			parsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
				return fmt.Errorf("mark memory %q conflicted: %w", id, err)
			}
			s.invalidateParsedMemoryCache(path)
		}
	}
	return s.rebuildIndexLocked(ctx)
}

func (s *LongTermMemoryStore) UpdateMemory(ctx context.Context, id string, update func(*MemoryItem)) error {
	return s.UpdateMemories(ctx, []string{id}, update)
}

func (s *LongTermMemoryStore) UpdateMemories(ctx context.Context, ids []string, update func(*MemoryItem)) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.updateMemoriesLocked(ctx, ids, update)
}

func (s *LongTermMemoryStore) updateMemoriesLocked(ctx context.Context, ids []string, update func(*MemoryItem)) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if update == nil {
		return nil
	}
	updated := false
	for _, id := range uniqueNonEmpty(ids) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		path := s.memoryPath(id)
		parsed, err := readMemoryMarkdown(path)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		update(&parsed.Item)
		parsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if parsed.Item.Title == "" {
			parsed.Item.Title = parsed.Title
		}
		if parsed.Item.Content == "" {
			parsed.Item.Content = parsed.Content
		}
		if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err != nil {
			return err
		}
		s.invalidateParsedMemoryCache(path)
		updated = true
	}
	if !updated {
		return nil
	}
	return s.rebuildIndexLocked(ctx)
}

func (s *LongTermMemoryStore) DecideAction(ctx context.Context, candidate MemoryItem) (string, string, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.decideActionLocked(ctx, candidate)
}

func (s *LongTermMemoryStore) decideActionLocked(ctx context.Context, candidate MemoryItem) (string, string, error) {
	results, err := s.searchLocked(ctx, MemoryQuery{
		Tags:     candidate.Tags,
		Entities: candidate.Entities,
		Types:    []string{candidate.Type},
		Limit:    10,
	})
	if err != nil {
		return "add", "", err
	}
	for _, existing := range results {
		if hasOverlappingSourceEvents(candidate, existing) {
			return "ignore", existing.ID, nil
		}
		candidateContent := strings.TrimSpace(candidate.Content)
		existingContent := strings.TrimSpace(existing.Content)
		if candidateContent == existingContent {
			return "ignore", existing.ID, nil
		}
		if strings.Contains(existingContent, candidateContent) {
			return "ignore", existing.ID, nil
		}
		if strings.Contains(candidateContent, existingContent) {
			return "supersede", existing.ID, nil
		}
		if candidate.Type == existing.Type && hasOverlappingEntities(candidate.Entities, existing.Entities) && hasOverlappingTags(candidate.Tags, existing.Tags) {
			return "supersede", existing.ID, nil
		}
	}
	return "add", "", nil
}

func hasOverlappingSourceEvents(candidate MemoryItem, existing MemoryResult) bool {
	if len(candidate.SourceRefs) == 0 {
		return false
	}
	existingPath := existing.FilePath
	if existingPath == "" {
		return false
	}
	parsed, err := readMemoryMarkdown(existingPath)
	if err != nil {
		return false
	}
	for _, cRef := range candidate.SourceRefs {
		for _, eRef := range parsed.Item.SourceRefs {
			if cRef.ID == eRef.ID {
				for _, cEvt := range cRef.EventIDs {
					for _, eEvt := range eRef.EventIDs {
						if cEvt == eEvt {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func hasOverlappingEntities(a []string, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, ae := range a {
		for _, be := range b {
			if strings.EqualFold(ae, be) {
				return true
			}
		}
	}
	return false
}

func hasOverlappingTags(a []string, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	for _, at := range a {
		for _, bt := range b {
			if strings.EqualFold(at, bt) {
				return true
			}
		}
	}
	return false
}

func (s *LongTermMemoryStore) RegenerateProfileMD(ctx context.Context) error {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.writeMu.Lock()
		entries, unreadableEntries, indexSignature, err := s.profileEntriesLocked(ctx)
		if err != nil {
			s.writeMu.Unlock()
			return err
		}
		if len(entries) == 0 {
			if unreadableEntries > 0 {
				s.writeMu.Unlock()
				return nil
			}
			profilePath := filepath.Join(s.rootDir, "profile.md")
			err := os.Remove(profilePath)
			s.writeMu.Unlock()
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove empty long-term profile: %w", err)
			}
			return nil
		}
		s.writeMu.Unlock()

		var profileContent string
		if s.profileFn != nil {
			profileContent = s.profileFn(ctx, entries)
		}
		if profileContent == "" {
			profileContent = fallbackProfile(entries)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		s.writeMu.Lock()
		currentSignature, exists, err := memoryFileSignatureForPath(s.indexPath())
		if err != nil {
			s.writeMu.Unlock()
			return fmt.Errorf("stat memory index before profile write: %w", err)
		}
		if !exists || currentSignature != indexSignature {
			s.writeMu.Unlock()
			continue
		}
		err = writeFileAtomic(filepath.Join(s.rootDir, "profile.md"), []byte(profileContent), 0o644)
		s.writeMu.Unlock()
		return err
	}
	return errors.New("long-term memory changed repeatedly during profile rebuild")
}

func (s *LongTermMemoryStore) profileEntriesLocked(ctx context.Context) ([]ProfileEntry, int, memoryFileSignature, error) {
	index, err := s.loadIndexLocked(ctx)
	if err != nil {
		return nil, 0, memoryFileSignature{}, err
	}
	indexSignature, _, err := memoryFileSignatureForPath(s.indexPath())
	if err != nil {
		return nil, 0, memoryFileSignature{}, fmt.Errorf("stat memory index for profile: %w", err)
	}
	var entries []ProfileEntry
	now := time.Now().UTC()
	var expiredPaths []string
	unreadableEntries := 0
	for _, entry := range index.Memories {
		if entry.Status != "active" || !isProfileRelevantType(entry.Type) {
			continue
		}
		parsed, path, expired, err := s.resolveActiveEntry(entry, now)
		if expired {
			expiredPaths = append(expiredPaths, path)
			continue
		}
		if err != nil {
			unreadableEntries++
			continue
		}
		entries = append(entries, ProfileEntry{Type: entry.Type, Content: strings.TrimSpace(parsed.Content)})
	}
	if len(expiredPaths) > 0 {
		s.invalidateParsedMemoryCache(expiredPaths...)
	}
	return entries, unreadableEntries, indexSignature, nil
}

func (s *LongTermMemoryStore) RequestProfileRebuild() {
	if s.profileDebouncer != nil {
		s.profileDebouncer.RequestRebuild()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.RegenerateProfileMD(ctx)
}

func (s *LongTermMemoryStore) setProfileDebouncer(debouncer *ProfileDebouncer) {
	s.profileDebouncer = debouncer
}

func isProfileRelevantType(t string) bool {
	switch t {
	case "profile", "rule", "preference":
		return true
	}
	return false
}

func fallbackProfile(entries []ProfileEntry) string {
	var b strings.Builder
	b.WriteString("# User Profile\n\n")
	b.WriteString(fmt.Sprintf("updated_at: %s\n\n", time.Now().UTC().Format(time.RFC3339)))
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("## [%s]\n\n%s\n\n", e.Type, e.Content))
	}
	return b.String()
}

func (s *LongTermMemoryStore) RebuildIndex(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.rebuildIndexLocked(ctx)
}

func (s *LongTermMemoryStore) rebuildIndexLocked(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	entries, err := os.ReadDir(s.memoriesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return s.writeIndex(memoryIndex{Version: memoryIndexVersion, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)})
		}
		return fmt.Errorf("read memories directory: %w", err)
	}
	index := memoryIndex{
		Version:   memoryIndexVersion,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Memories:  make([]memoryIndexEntry, 0, len(entries)),
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(s.memoriesDir(), entry.Name())
		parsed, err := readMemoryMarkdown(path)
		if err != nil {
			continue
		}
		index.Memories = append(index.Memories, memoryIndexEntry{
			ID:         parsed.Item.ID,
			File:       filepath.ToSlash(filepath.Join("memories", entry.Name())),
			Type:       parsed.Item.Type,
			Status:     parsed.Item.Status,
			ExpiresAt:  parsed.Item.ExpiresAt,
			Priority:   parsed.Item.Priority,
			Confidence: parsed.Item.Confidence,
			Tags:       append([]string(nil), parsed.Item.Tags...),
			Entities:   append([]string(nil), parsed.Item.Entities...),
			Summary:    firstSentence(parsed.Content),
		})
	}
	sort.SliceStable(index.Memories, func(i, j int) bool {
		if index.Memories[i].Priority == index.Memories[j].Priority {
			return index.Memories[i].ID < index.Memories[j].ID
		}
		return index.Memories[i].Priority > index.Memories[j].Priority
	})
	return s.writeIndex(index)
}

func (s *LongTermMemoryStore) memoriesDir() string {
	return filepath.Join(s.rootDir, "memories")
}

func (s *LongTermMemoryStore) indexPath() string {
	return filepath.Join(s.rootDir, "index.yaml")
}

func (s *LongTermMemoryStore) memoryPath(id string) string {
	return filepath.Join(s.memoriesDir(), safeMemoryFileName(id))
}

func (s *LongTermMemoryStore) loadIndex(ctx context.Context) (memoryIndex, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.loadIndexLocked(ctx)
}

func (s *LongTermMemoryStore) loadIndexLocked(ctx context.Context) (memoryIndex, error) {
	signature, ok, err := memoryFileSignatureForPath(s.indexPath())
	if err != nil {
		return memoryIndex{}, fmt.Errorf("stat memory index: %w", err)
	}
	if ok {
		if index, hit := s.cachedIndex(signature); hit {
			if index.Version >= memoryIndexVersion {
				return index, nil
			}
			if err := s.rebuildIndexLocked(ctx); err != nil {
				return memoryIndex{}, err
			}
			return s.loadIndexLocked(ctx)
		}
	}
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if !os.IsNotExist(err) {
			return memoryIndex{}, fmt.Errorf("read memory index: %w", err)
		}
		if err := s.rebuildIndexLocked(ctx); err != nil {
			return memoryIndex{}, err
		}
		data, err = os.ReadFile(s.indexPath())
		if err != nil {
			return memoryIndex{}, fmt.Errorf("read rebuilt memory index: %w", err)
		}
		signature, ok, err = memoryFileSignatureForPath(s.indexPath())
		if err != nil {
			return memoryIndex{}, fmt.Errorf("stat rebuilt memory index: %w", err)
		}
	}
	var index memoryIndex
	if err := yaml.Unmarshal(data, &index); err != nil {
		return memoryIndex{}, fmt.Errorf("decode memory index: %w", err)
	}
	if index.Version < memoryIndexVersion {
		if err := s.rebuildIndexLocked(ctx); err != nil {
			return memoryIndex{}, err
		}
		return s.loadIndexLocked(ctx)
	}
	if ok {
		s.storeIndexCache(signature, index)
	}
	return cloneMemoryIndex(index), nil
}

func (s *LongTermMemoryStore) writeIndex(index memoryIndex) error {
	data, err := yaml.Marshal(index)
	if err != nil {
		return fmt.Errorf("encode memory index: %w", err)
	}
	if err := os.MkdirAll(s.rootDir, 0o755); err != nil {
		return fmt.Errorf("create long-term memory directory: %w", err)
	}
	if err := writeFileAtomic(s.indexPath(), data, 0o644); err != nil {
		return err
	}
	s.invalidateIndexCache()
	return nil
}

func (s *LongTermMemoryStore) cachedIndex(signature memoryFileSignature) (memoryIndex, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.indexCache.valid || s.indexCache.signature != signature {
		return memoryIndex{}, false
	}
	return cloneMemoryIndex(s.indexCache.index), true
}

func (s *LongTermMemoryStore) storeIndexCache(signature memoryFileSignature, index memoryIndex) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.indexCache = longTermIndexCache{signature: signature, index: cloneMemoryIndex(index), valid: true}
}

func (s *LongTermMemoryStore) invalidateIndexCache() {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.indexCache = longTermIndexCache{}
}

func (s *LongTermMemoryStore) readMemoryMarkdownCached(path string) (parsedMemoryMarkdown, error) {
	signature, ok, err := memoryFileSignatureForPath(path)
	if err != nil {
		return parsedMemoryMarkdown{}, fmt.Errorf("stat memory markdown %q: %w", path, err)
	}
	if ok {
		s.cacheMu.Lock()
		if parsed, hit := s.parsedCache.get(path, signature); hit {
			s.cacheMu.Unlock()
			return parsed, nil
		}
		s.cacheMu.Unlock()
	}
	parsed, err := readMemoryMarkdown(path)
	if err != nil {
		return parsedMemoryMarkdown{}, err
	}
	if ok && !memoryItemExpired(parsed.Item, time.Now().UTC()) {
		s.cacheMu.Lock()
		s.parsedCache.put(path, signature, parsed)
		s.cacheMu.Unlock()
	}
	return cloneParsedMemoryMarkdown(parsed), nil
}

func (s *LongTermMemoryStore) invalidateParsedMemoryCache(paths ...string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	s.parsedCache.evict(paths...)
}

func cloneMemoryIndex(index memoryIndex) memoryIndex {
	index.Memories = cloneMemoryIndexEntries(index.Memories)
	return index
}

func cloneMemoryIndexEntries(entries []memoryIndexEntry) []memoryIndexEntry {
	if len(entries) == 0 {
		return nil
	}
	cloned := make([]memoryIndexEntry, len(entries))
	for i, entry := range entries {
		cloned[i] = entry
		cloned[i].Tags = append([]string(nil), entry.Tags...)
		cloned[i].Entities = append([]string(nil), entry.Entities...)
	}
	return cloned
}

func cloneParsedMemoryMarkdown(parsed parsedMemoryMarkdown) parsedMemoryMarkdown {
	parsed.Item = cloneMemoryItem(parsed.Item)
	return parsed
}

func cloneMemoryItem(item MemoryItem) MemoryItem {
	item.Tags = append([]string(nil), item.Tags...)
	item.Entities = append([]string(nil), item.Entities...)
	item.Applicability = cloneStringMap(item.Applicability)
	item.SourceRefs = cloneMemorySourceRefs(item.SourceRefs)
	item.EvidenceRefs = cloneMemorySourceRefs(item.EvidenceRefs)
	item.ConflictsWith = append([]string(nil), item.ConflictsWith...)
	item.EvidenceExcerpts = append([]string(nil), item.EvidenceExcerpts...)
	return item
}

func (s *LongTermMemoryStore) appendTombstone(id string, reason string) error {
	if err := os.MkdirAll(s.lifecycleDir, 0o755); err != nil {
		return fmt.Errorf("create lifecycle directory: %w", err)
	}
	path := filepath.Join(s.lifecycleDir, "tombstones.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open tombstones: %w", err)
	}
	defer file.Close()

	record := map[string]any{
		"id":     "tomb_" + strconvTimeID(time.Now().UTC()),
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"reason": reason,
		"ref": map[string]string{
			"type": "memory",
			"id":   id,
		},
	}
	if err := json.NewEncoder(file).Encode(record); err != nil {
		return fmt.Errorf("append tombstone: %w", err)
	}
	return nil
}

type parsedMemoryMarkdown struct {
	Item    MemoryItem
	Title   string
	Content string
}

func normalizeMemoryItem(item MemoryItem, now time.Time) MemoryItem {
	if item.ID == "" {
		item.ID = "mem_" + strconvTimeID(now)
	}
	if item.Type == "" {
		item.Type = "fact"
	}
	if item.Status == "" {
		item.Status = "active"
	}
	if item.Revision <= 0 {
		item.Revision = 1
	}
	if item.TimeScope == "" {
		item.TimeScope = "long_term"
	}
	if item.CreatedAt == "" {
		item.CreatedAt = now.Format(time.RFC3339Nano)
	}
	item.UpdatedAt = now.Format(time.RFC3339Nano)
	if item.Traceability == "" {
		item.Traceability = "full"
	}
	if item.ExpiresAt == "" {
		item.ExpiresAt = ttlExpiresAt(now, item.TTL)
	}
	if len(item.EvidenceRefs) == 0 && len(item.SourceRefs) > 0 {
		item.EvidenceRefs = append([]MemorySourceRef(nil), item.SourceRefs...)
	}
	if item.Title == "" {
		item.Title = item.ID
	}
	return item
}

func formatMemoryMarkdown(item MemoryItem) string {
	frontmatter := item
	data, _ := yaml.Marshal(frontmatter)
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.Write(data)
	builder.WriteString("---\n\n")
	builder.WriteString("# ")
	builder.WriteString(item.Title)
	builder.WriteString("\n\n## 内容\n\n")
	builder.WriteString(strings.TrimSpace(item.Content))
	builder.WriteString("\n\n## 证据摘录\n\n")
	for _, excerpt := range item.EvidenceExcerpts {
		builder.WriteString("- ")
		builder.WriteString(strings.TrimSpace(excerpt))
		builder.WriteString("\n")
	}
	return builder.String()
}

func readMemoryMarkdown(path string) (parsedMemoryMarkdown, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return parsedMemoryMarkdown{}, fmt.Errorf("read memory markdown %q: %w", path, err)
	}
	parts := bytes.SplitN(data, []byte("---\n"), 3)
	if len(parts) != 3 || len(parts[0]) != 0 {
		return parsedMemoryMarkdown{}, fmt.Errorf("memory markdown %q missing frontmatter", path)
	}
	var item MemoryItem
	if err := yaml.Unmarshal(parts[1], &item); err != nil {
		return parsedMemoryMarkdown{}, fmt.Errorf("decode memory frontmatter %q: %w", path, err)
	}
	title, content, evidence := parseMemoryBody(string(parts[2]))
	item.Title = title
	item.Content = content
	item.EvidenceExcerpts = evidence
	return parsedMemoryMarkdown{Item: item, Title: title, Content: content}, nil
}

func parseMemoryBody(body string) (string, string, []string) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	title := ""
	var contentLines []string
	var fallbackLines []string
	var evidence []string
	inContent := false
	inEvidence := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if title == "" && strings.HasPrefix(line, "# ") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			continue
		}
		if trimmed == "## 内容" {
			inContent = true
			inEvidence = false
			continue
		}
		if trimmed == "## 证据摘录" {
			inContent = false
			inEvidence = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			inContent = false
			inEvidence = false
		}
		if inContent {
			contentLines = append(contentLines, line)
		}
		if inEvidence {
			if strings.HasPrefix(trimmed, "- ") {
				evidence = append(evidence, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			}
		}
		if title != "" && !inContent && !inEvidence && trimmed != "" && !strings.HasPrefix(trimmed, "## ") {
			fallbackLines = append(fallbackLines, line)
		}
	}
	content := strings.TrimSpace(strings.Join(contentLines, "\n"))
	if content == "" {
		content = strings.TrimSpace(strings.Join(fallbackLines, "\n"))
	}
	return title, content, evidence
}

func matchesAny(queryValues []string, candidateValues []string) bool {
	if len(queryValues) == 0 {
		return true
	}
	for _, queryValue := range queryValues {
		for _, candidateValue := range candidateValues {
			if strings.EqualFold(queryValue, candidateValue) {
				return true
			}
		}
	}
	return false
}

func memoryQueryIsEmpty(query MemoryQuery) bool {
	return !memoryQueryHasTopicalTerms(query) && len(query.Types) == 0
}

func memoryQueryHasTopicalTerms(query MemoryQuery) bool {
	for _, tag := range query.Tags {
		if normalizeMemorySearchTerm(tag) != "" {
			return true
		}
	}
	return len(nonGenericMemoryEntities(query.Entities)) > 0
}

func scoreMemoryEntry(query MemoryQuery, entry memoryIndexEntry, parsed parsedMemoryMarkdown) int {
	typeScore := 0
	if matchesAny(query.Types, []string{entry.Type}) && len(query.Types) > 0 {
		typeScore = 3
	}
	haystacks := []string{parsed.Title, entry.Summary, parsed.Content}
	topicScore := scoreMemoryQueryValues(query.Tags, entry.Tags, entry.Entities, haystacks)
	topicScore += scoreMemoryQueryValues(nonGenericMemoryEntities(query.Entities), entry.Entities, entry.Tags, haystacks)
	if topicScore == 0 && memoryQueryHasTopicalTerms(query) {
		return 0
	}
	return typeScore + topicScore
}

func scoreMemoryQueryValues(queryValues, candidateValues, crossFieldValues, haystacks []string) int {
	score := 0
	for _, queryValue := range queryValues {
		queryTerm := normalizeMemorySearchTerm(queryValue)
		if queryTerm == "" {
			continue
		}
		metadataScore := scoreMemoryMetadataTerm(queryTerm, candidateValues)
		if metadataScore == 0 {
			// Models may classify the same topic as a tag when saving and an
			// entity when recalling. Prefer matches in the original field.
			metadataScore = scoreMemoryMetadataTerm(queryTerm, crossFieldValues) / 2
		}
		score += metadataScore
		for _, haystack := range haystacks {
			if strings.Contains(normalizeMemorySearchTerm(haystack), queryTerm) {
				score += 4
				break
			}
		}
	}
	return score
}

func scoreMemoryMetadataTerm(queryTerm string, values []string) int {
	score := 0
	for _, value := range values {
		term := normalizeMemorySearchTerm(value)
		if term == "" {
			continue
		}
		if queryTerm == term {
			return 10
		}
		if memorySearchTermContains(queryTerm, term) {
			score = 6
		}
	}
	return score
}

func nonGenericMemoryEntities(entities []string) []string {
	filtered := make([]string, 0, len(entities))
	for _, entity := range entities {
		switch normalizeMemorySearchTerm(entity) {
		case "", "user", "me", "my", "myself", "you":
			continue
		default:
			filtered = append(filtered, entity)
		}
	}
	return filtered
}

func normalizeMemorySearchTerm(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func memorySearchTermContains(a string, b string) bool {
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

func firstSentence(content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return ""
	}
	for _, sep := range []string{"。", ".", "\n"} {
		if idx := strings.Index(content, sep); idx >= 0 {
			return strings.TrimSpace(content[:idx+len(sep)])
		}
	}
	return content
}

func safeMemoryFileName(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = "memory"
	}
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		switch r {
		case '-', '_', '.':
			return r
		default:
			return '_'
		}
	}, id)
	return safe + ".md"
}

func strconvTimeID(ts time.Time) string {
	var buf [4]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("%d_%s", ts.UnixNano(), hex.EncodeToString(buf[:]))
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type RetentionPolicy struct {
	EventCompactAfter   string `yaml:"event_compact_after"`
	ChunkNoRefTTL       string `yaml:"chunk_no_ref_ttl"`
	ChunkReferencedKeep string `yaml:"chunk_referenced_keep"`
	MemoryDeletedTTL    string `yaml:"memory_deleted_ttl"`
	TombstoneTTL        string `yaml:"tombstone_ttl"`
	GCInterval          string `yaml:"gc_interval"`
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		EventCompactAfter:   "7d",
		ChunkNoRefTTL:       "30d",
		ChunkReferencedKeep: "forever",
		MemoryDeletedTTL:    "90d",
		TombstoneTTL:        "30d",
		GCInterval:          "24h",
	}
}

type LifecycleManager struct {
	rootDir   string
	retention RetentionPolicy
}

func NewLifecycleManager(rootDir string) *LifecycleManager {
	lm := &LifecycleManager{rootDir: rootDir}
	lm.retention = lm.loadRetention()
	return lm
}

func (lm *LifecycleManager) loadRetention() RetentionPolicy {
	policy := DefaultRetentionPolicy()
	path := filepath.Join(lm.rootDir, "lifecycle", "retention.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return policy
	}
	_ = yaml.Unmarshal(data, &policy)
	return policy
}

func (lm *LifecycleManager) EnsureRetentionFile() error {
	dir := filepath.Join(lm.rootDir, "lifecycle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create lifecycle directory: %w", err)
	}
	path := filepath.Join(dir, "retention.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	data, err := yaml.Marshal(lm.retention)
	if err != nil {
		return fmt.Errorf("marshal retention policy: %w", err)
	}
	return writeFileAtomic(path, data, 0o644)
}

type VerifyReport struct {
	IndexRebuilt        bool
	EpisodeIndexRebuilt bool
	OrphanedIndexItems  int
	OrphanedEpisodes    int
	ExpiredMemories     int
	PrunedEpisodeTraces int
	StaleTraceability   int
	ProfileFixups       int
}

func (lm *LifecycleManager) Verify(ctx context.Context) (VerifyReport, error) {
	var report VerifyReport
	longTermDir := filepath.Join(lm.rootDir, "long_term")
	store := NewLongTermMemoryStore(longTermDir, WithLifecycleDir(filepath.Join(lm.rootDir, "lifecycle")))
	episodeStore := NewTaskEpisodeStore(filepath.Join(lm.rootDir, "episodes"))

	indexPath := store.indexPath()
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := store.RebuildIndex(ctx); err != nil {
				return report, err
			}
			report.IndexRebuilt = true
		} else {
			return report, fmt.Errorf("read index for verify: %w", err)
		}
	}

	if len(data) > 0 {
		var index memoryIndex
		if err := yaml.Unmarshal(data, &index); err != nil {
			if err := store.RebuildIndex(ctx); err != nil {
				return report, err
			}
			report.IndexRebuilt = true
		} else {
			needsRebuild := false
			for _, entry := range index.Memories {
				memPath := filepath.Join(longTermDir, entry.File)
				if _, err := os.Stat(memPath); os.IsNotExist(err) {
					report.OrphanedIndexItems++
					needsRebuild = true
				}
			}
			if needsRebuild {
				if err := store.RebuildIndex(ctx); err != nil {
					return report, err
				}
				report.IndexRebuilt = true
			}
		}
	}

	if episodeReport, err := lm.verifyEpisodeIndex(ctx, episodeStore); err != nil {
		return report, err
	} else {
		report.EpisodeIndexRebuilt = episodeReport.EpisodeIndexRebuilt
		report.OrphanedEpisodes = episodeReport.OrphanedEpisodes
	}

	memoryReport, err := lm.verifyLongTermMemoryFiles(ctx, store)
	if err != nil {
		return report, err
	}
	report.ExpiredMemories = memoryReport.ExpiredMemories
	report.StaleTraceability = memoryReport.StaleTraceability
	report.IndexRebuilt = report.IndexRebuilt || memoryReport.IndexRebuilt

	pruned, err := lm.pruneEpisodeTraces(ctx, store, episodeStore)
	if err != nil {
		return report, err
	}
	report.PrunedEpisodeTraces = pruned

	profilePath := filepath.Join(longTermDir, "profile.md")
	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		if err := store.RegenerateProfileMD(ctx); err == nil {
			report.ProfileFixups++
		}
	}

	return report, nil
}

func (lm *LifecycleManager) verifyLongTermMemoryFiles(ctx context.Context, store *LongTermMemoryStore) (VerifyReport, error) {
	var report VerifyReport
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	entries, err := os.ReadDir(store.memoriesDir())
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	chunksDir := filepath.Join(lm.rootDir, "session", "chunks")
	now := time.Now().UTC()
	memoriesChanged := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(store.memoriesDir(), entry.Name())
		parsed, err := readMemoryMarkdown(path)
		if err != nil {
			continue
		}
		if parsed.Item.Status == "active" && memoryItemExpired(parsed.Item, now) {
			parsed.Item.Status = "expired"
			parsed.Item.UpdatedAt = now.Format(time.RFC3339Nano)
			if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err == nil {
				store.invalidateParsedMemoryCache(path)
				report.ExpiredMemories++
				memoriesChanged = true
			}
			continue
		}
		if parsed.Item.Traceability == "excerpt_only" {
			continue
		}
		for _, ref := range parsed.Item.SourceRefs {
			if ref.Type != "chunk" {
				continue
			}
			chunkPath := filepath.Join(chunksDir, ref.ID+".jsonl")
			if _, err := os.Stat(chunkPath); os.IsNotExist(err) {
				parsed.Item.Traceability = "excerpt_only"
				parsed.Item.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				if err := writeFileAtomic(path, []byte(formatMemoryMarkdown(parsed.Item)), 0o644); err == nil {
					store.invalidateParsedMemoryCache(path)
					report.StaleTraceability++
					memoriesChanged = true
				}
				break
			}
		}
	}
	if memoriesChanged {
		if err := store.rebuildIndexLocked(ctx); err != nil {
			return report, err
		}
		report.IndexRebuilt = true
	}
	return report, nil
}

func (lm *LifecycleManager) verifyEpisodeIndex(ctx context.Context, store *TaskEpisodeStore) (VerifyReport, error) {
	report, needsRebuild, err := checkEpisodeIndex(store)
	if err != nil {
		return report, err
	}
	if needsRebuild {
		if err := store.RebuildIndex(ctx); err != nil {
			return report, err
		}
		report.EpisodeIndexRebuilt = true
	}
	return report, nil
}

// checkEpisodeIndex clears references to missing event files and reports
// whether the index must be rebuilt. It holds the store lock from load to
// write, so an entry appended in between is not dropped; the caller runs
// RebuildIndex afterwards, since it takes the same lock.
func checkEpisodeIndex(store *TaskEpisodeStore) (VerifyReport, bool, error) {
	var report VerifyReport
	defer store.lock()()
	if _, err := os.Stat(store.indexPath()); os.IsNotExist(err) && !store.indexLogExists() {
		return report, true, nil
	} else if err != nil && !os.IsNotExist(err) {
		return report, false, err
	}
	index, err := store.loadIndex()
	if err != nil {
		return report, true, nil
	}
	needsRebuild := false
	indexChanged := false
	for i := range index.Episodes {
		entry := &index.Episodes[i]
		metaPath := filepath.Join(store.rootDir, entry.File)
		if _, err := os.Stat(metaPath); os.IsNotExist(err) {
			report.OrphanedEpisodes++
			needsRebuild = true
			continue
		}
		if entry.EventsFile != "" {
			eventsPath := filepath.Join(store.rootDir, entry.EventsFile)
			if _, err := os.Stat(eventsPath); os.IsNotExist(err) {
				entry.EventsFile = ""
				indexChanged = true
			}
		}
	}
	if needsRebuild {
		return report, true, nil
	}
	if indexChanged {
		index.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := store.writeIndex(index); err != nil {
			return report, false, err
		}
		report.EpisodeIndexRebuilt = true
	}
	return report, false, nil
}

func (lm *LifecycleManager) pruneEpisodeTraces(ctx context.Context, longTerm *LongTermMemoryStore, episodes *TaskEpisodeStore) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	compactAfter, ok := parseRetentionDuration(lm.retention.EventCompactAfter)
	if !ok || compactAfter <= 0 || episodes == nil || episodes.rootDir == "" {
		return 0, nil
	}
	defer episodes.lock()()
	index, err := episodes.loadIndex()
	if err != nil {
		return 0, err
	}
	if len(index.Episodes) == 0 {
		return 0, nil
	}
	referenced, err := activeEpisodeRefs(longTerm, NewDeviceMemoryStore(filepath.Join(lm.rootDir, "device")), time.Now().UTC())
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().Add(-compactAfter)
	pruned := 0
	indexChanged := false
	for i := range index.Episodes {
		entry := &index.Episodes[i]
		if referenced[entry.ID] {
			continue
		}
		episodeTime, ok := parseLifecycleTimestamp(firstNonEmptyString([]string{entry.EndedAt, entry.StartedAt}))
		if !ok || !episodeTime.Before(cutoff) {
			continue
		}
		episodeDir := filepath.Dir(filepath.Join(episodes.rootDir, entry.File))
		eventsPath := filepath.Join(episodeDir, "events.jsonl")
		if entry.EventsFile != "" {
			eventsPath = filepath.Join(episodes.rootDir, entry.EventsFile)
		}
		prunedThisEpisode := false
		if removed, err := removeFileIfExists(eventsPath); err != nil {
			return pruned, err
		} else if removed {
			prunedThisEpisode = true
		}
		artifactsDir := filepath.Join(episodeDir, "artifacts")
		if removed, err := removeDirIfExists(artifactsDir); err != nil {
			return pruned, err
		} else if removed {
			prunedThisEpisode = true
		}
		if prunedThisEpisode {
			pruned++
			if entry.EventsFile != "" {
				entry.EventsFile = ""
				indexChanged = true
			}
		}
	}
	if indexChanged {
		index.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := episodes.writeIndex(index); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

func activeEpisodeRefs(store *LongTermMemoryStore, device *DeviceMemoryStore, now time.Time) (map[string]bool, error) {
	refs := map[string]bool{}
	if store != nil {
		store.writeMu.Lock()
		defer store.writeMu.Unlock()
		entries, err := os.ReadDir(store.memoriesDir())
		if err != nil {
			if !os.IsNotExist(err) {
				return refs, err
			}
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			parsed, err := readMemoryMarkdown(filepath.Join(store.memoriesDir(), entry.Name()))
			if err != nil {
				continue
			}
			if parsed.Item.Status != "active" || memoryItemExpired(parsed.Item, now) {
				continue
			}
			addEpisodeRefs(refs, append(append([]MemorySourceRef(nil), parsed.Item.SourceRefs...), parsed.Item.EvidenceRefs...))
		}
	}

	if device != nil {
		items, err := device.readAll()
		if err != nil {
			return refs, err
		}
		for _, item := range items {
			if item.Status != "active" || memoryExpiresAtPassed(item.ExpiresAt, now) {
				continue
			}
			addEpisodeRefs(refs, item.EvidenceRefs)
		}
	}
	return refs, nil
}

func addEpisodeRefs(refs map[string]bool, sourceRefs []MemorySourceRef) {
	for _, ref := range sourceRefs {
		if ref.Type == "episode" && strings.TrimSpace(ref.ID) != "" {
			refs[ref.ID] = true
		}
	}
}

func parseLifecycleTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ts, true
	}
	if ts, err := time.Parse(time.RFC3339, value); err == nil {
		return ts, true
	}
	return time.Time{}, false
}

func removeFileIfExists(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	return true, nil
}

func removeDirIfExists(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return false, nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.RemoveAll(path); err != nil {
		return false, err
	}
	return true, nil
}

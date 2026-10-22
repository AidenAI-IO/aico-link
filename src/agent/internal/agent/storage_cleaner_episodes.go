package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Task episodes are execution evidence. What later runs need from them is
// distilled into Device Memory by Episode Memory within minutes, and the
// runtime only reads recent or running episodes back, so old ones are deleted
// unless a memory still cites them as evidence.
const (
	defaultEpisodeRetention  = 30 * 24 * time.Hour
	defaultEpisodeMaxEntries = 1000
)

// EpisodeRetentionCleaner deletes whole episode directories, with their index
// entries, that ended more than maxAge ago, and the oldest ones beyond
// maxEntries. Running episodes and episodes cited by an active Long-Term or
// Device Memory are always kept. Directories missing from the index are
// removed once they are older than maxAge.
type EpisodeRetentionCleaner struct {
	memoryDir  string
	maxAge     time.Duration
	maxEntries int
	priority   int
	now        func() time.Time
}

func NewEpisodeRetentionCleaner(memoryDir string, priority int) *EpisodeRetentionCleaner {
	return &EpisodeRetentionCleaner{
		memoryDir:  memoryDir,
		maxAge:     defaultEpisodeRetention,
		maxEntries: defaultEpisodeMaxEntries,
		priority:   priority,
		now:        time.Now,
	}
}

func (c *EpisodeRetentionCleaner) Name() string  { return "episode_retention" }
func (c *EpisodeRetentionCleaner) Priority() int { return c.priority }

func (c *EpisodeRetentionCleaner) store() *TaskEpisodeStore {
	return NewTaskEpisodeStore(filepath.Join(c.memoryDir, "episodes"))
}

func (c *EpisodeRetentionCleaner) EstimateReclaimable(ctx context.Context) (uint64, error) {
	if c == nil || strings.TrimSpace(c.memoryDir) == "" {
		return 0, nil
	}
	store := c.store()
	unlock := store.lock()
	defer unlock()
	plan, err := c.plan(ctx, store)
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, dir := range plan.dirs {
		total += directoryBytes(dir)
	}
	return total, nil
}

func (c *EpisodeRetentionCleaner) Clean(ctx context.Context) (uint64, error) {
	if c == nil || strings.TrimSpace(c.memoryDir) == "" {
		return 0, nil
	}
	store := c.store()
	unlock := store.lock()
	defer unlock()
	plan, err := c.plan(ctx, store)
	if err != nil || len(plan.dirs) == 0 {
		return 0, err
	}
	var freed uint64
	for _, dir := range plan.dirs {
		if err := ctx.Err(); err != nil {
			return freed, err
		}
		size := directoryBytes(dir)
		if err := os.RemoveAll(dir); err != nil {
			return freed, fmt.Errorf("remove episode %s: %w", filepath.Base(dir), err)
		}
		freed += size
	}
	if plan.indexChanged {
		plan.index.UpdatedAt = c.now().UTC().Format(time.RFC3339Nano)
		if err := store.writeIndex(plan.index); err != nil {
			return freed, err
		}
	}
	return freed, nil
}

type episodeRetentionPlan struct {
	// index is the index without the deleted entries.
	index        episodeIndex
	indexChanged bool
	dirs         []string
}

// plan picks what to delete. The caller holds the store lock.
func (c *EpisodeRetentionCleaner) plan(ctx context.Context, store *TaskEpisodeStore) (episodeRetentionPlan, error) {
	index, err := store.loadIndex()
	if err != nil {
		return episodeRetentionPlan{}, err
	}
	now := c.now().UTC()
	referenced, err := activeEpisodeRefs(
		NewLongTermMemoryStore(filepath.Join(c.memoryDir, "long_term")),
		NewDeviceMemoryStore(filepath.Join(c.memoryDir, "device")),
		now,
	)
	if err != nil {
		return episodeRetentionPlan{}, err
	}
	cutoff := now.Add(-c.maxAge)

	entries := append([]episodeIndexEntry(nil), index.Episodes...)
	sort.SliceStable(entries, func(i, j int) bool {
		return episodeEntryTime(entries[i]).After(episodeEntryTime(entries[j]))
	})
	var plan episodeRetentionPlan
	indexed := map[string]bool{}
	kept := make([]episodeIndexEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return episodeRetentionPlan{}, err
		}
		dir, inside := store.entryDir(entry)
		if inside {
			indexed[dir] = true
		}
		deletable := inside && entry.Status != "running" && !referenced[entry.ID]
		at := episodeEntryTime(entry)
		tooOld := !at.IsZero() && at.Before(cutoff)
		if deletable && (tooOld || len(kept) >= c.maxEntries) {
			plan.dirs = append(plan.dirs, dir)
			plan.indexChanged = true
			continue
		}
		kept = append(kept, entry)
	}

	orphans, err := c.orphanDirs(ctx, store.rootDir, indexed, referenced, cutoff)
	if err != nil {
		return episodeRetentionPlan{}, err
	}
	plan.dirs = append(plan.dirs, orphans...)
	plan.index = index
	plan.index.Episodes = kept
	return plan, nil
}

// orphanDirs returns episode directories that no index entry points at and
// that were last modified before cutoff.
func (c *EpisodeRetentionCleaner) orphanDirs(ctx context.Context, root string, indexed, referenced map[string]bool, cutoff time.Time) ([]string, error) {
	years, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var orphans []string
	for _, year := range years {
		if !year.IsDir() {
			continue
		}
		episodes, err := os.ReadDir(filepath.Join(root, year.Name()))
		if err != nil {
			return nil, err
		}
		for _, episode := range episodes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !episode.IsDir() || !strings.HasPrefix(episode.Name(), "ep_") || referenced[episode.Name()] {
				continue
			}
			dir := filepath.Join(root, year.Name(), episode.Name())
			if indexed[dir] {
				continue
			}
			info, err := episode.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			orphans = append(orphans, dir)
		}
	}
	return orphans, nil
}

// entryDir returns the episode directory of an index entry, and false when the
// entry's path would leave the episodes directory.
func (s *TaskEpisodeStore) entryDir(entry episodeIndexEntry) (string, bool) {
	if strings.TrimSpace(entry.File) == "" {
		return "", false
	}
	dir := filepath.Dir(filepath.Join(s.rootDir, filepath.FromSlash(entry.File)))
	rel, err := filepath.Rel(s.rootDir, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return dir, true
}

func episodeEntryTime(entry episodeIndexEntry) time.Time {
	at, _ := parseLifecycleTimestamp(firstNonEmptyString([]string{entry.EndedAt, entry.StartedAt}))
	return at
}

func directoryBytes(dir string) uint64 {
	var total uint64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

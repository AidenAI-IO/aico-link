package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"aico-agent/internal/logging"
)

// SessionArchiveCleaner cleans up old session archives
type SessionArchiveCleaner struct {
	archiveDir   string
	retentionAge time.Duration
	maxCount     int
	priority     int
}

// NewSessionArchiveCleaner creates a new session archive cleaner
func NewSessionArchiveCleaner(archiveDir string, retentionDays int, maxCount int, priority int) *SessionArchiveCleaner {
	return &SessionArchiveCleaner{
		archiveDir:   archiveDir,
		retentionAge: time.Duration(retentionDays) * 24 * time.Hour,
		maxCount:     maxCount,
		priority:     priority,
	}
}

func (c *SessionArchiveCleaner) Name() string {
	if c.retentionAge > 0 {
		return fmt.Sprintf("session_archive_%dd", int(c.retentionAge.Hours()/24))
	}
	return fmt.Sprintf("session_archive_keep_%d", c.maxCount)
}

func (c *SessionArchiveCleaner) Priority() int {
	return c.priority
}

type sessionArchiveInfo struct {
	path    string
	modTime time.Time
	size    int64
}

func (c *SessionArchiveCleaner) collectArchives() ([]sessionArchiveInfo, error) {
	if c.archiveDir == "" {
		return nil, nil
	}

	entries, err := os.ReadDir(c.archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read archive directory: %w", err)
	}

	var archives []sessionArchiveInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Calculate directory size
		dirPath := filepath.Join(c.archiveDir, entry.Name())
		size, err := calculateDirSize(dirPath)
		if err != nil {
			_ = logging.LogEvent(logging.Warn, "agent", "storage_cleanup", "session_archive_size_failed",
				logging.Field{Key: "archive", Value: entry.Name()},
				logging.Field{Key: "error", Value: err},
			)
			size = 0
		}

		archives = append(archives, sessionArchiveInfo{
			path:    dirPath,
			modTime: info.ModTime(),
			size:    size,
		})
	}

	// Sort by modification time (oldest first)
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].modTime.Before(archives[j].modTime)
	})

	return archives, nil
}

func calculateDirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (c *SessionArchiveCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	archives, err := c.collectArchives()
	if err != nil {
		return 0, err
	}

	var total uint64
	if c.retentionAge == 0 && c.maxCount == 0 {
		for _, archive := range archives {
			total += uint64(archive.size)
		}
		return total, nil
	}
	now := time.Now()
	cutoff := now.Add(-c.retentionAge)

	for i, archive := range archives {
		// If retention age is set, check age
		if c.retentionAge > 0 && archive.modTime.Before(cutoff) {
			total += uint64(archive.size)
			continue
		}

		// If max count is set, count archives beyond the limit
		if c.maxCount > 0 && i < len(archives)-c.maxCount {
			total += uint64(archive.size)
		}
	}

	return total, nil
}

func (c *SessionArchiveCleaner) Clean(ctx context.Context) (uint64, error) {
	if c.retentionAge == 0 && c.maxCount == 0 {
		return c.cleanAll(ctx)
	}
	archives, err := c.collectArchives()
	if err != nil {
		return 0, err
	}

	if len(archives) == 0 {
		return 0, nil
	}

	var totalFreed uint64
	var deletedCount int
	now := time.Now()
	cutoff := now.Add(-c.retentionAge)

	// Delete archives based on retention age
	if c.retentionAge > 0 {
		for _, archive := range archives {
			if !archive.modTime.Before(cutoff) {
				break // Archives are sorted, so we can stop
			}

			if err := os.RemoveAll(archive.path); err != nil {
				return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(archive.path), err)
			}

			totalFreed += uint64(archive.size)
			deletedCount++
		}
	}

	// Delete archives beyond max count
	if c.maxCount > 0 {
		// Re-collect archives after age-based deletion
		archives, err = c.collectArchives()
		if err != nil {
			return totalFreed, err
		}

		for len(archives) > c.maxCount {
			oldest := archives[0]
			if err := os.RemoveAll(oldest.path); err != nil {
				return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(oldest.path), err)
			}

			totalFreed += uint64(oldest.size)
			deletedCount++
			archives = archives[1:]
		}
	}

	if deletedCount > 0 {
		_ = logging.LogEvent(logging.Info, "agent", "storage_cleanup", "session_archives_deleted",
			logging.Field{Key: "archives", Value: deletedCount},
			logging.Field{Key: "freed_bytes", Value: totalFreed},
		)
	}

	return totalFreed, nil
}

func (c *SessionArchiveCleaner) ForceClean(ctx context.Context) (uint64, error) {
	forced := *c
	forced.retentionAge = 0
	forced.maxCount = 0
	return forced.cleanAll(ctx)
}

func (c *SessionArchiveCleaner) cleanAll(context.Context) (uint64, error) {
	archives, err := c.collectArchives()
	if err != nil {
		return 0, err
	}
	var totalFreed uint64
	for _, archive := range archives {
		if err := os.RemoveAll(archive.path); err != nil {
			return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(archive.path), err)
		}
		totalFreed += uint64(archive.size)
	}
	return totalFreed, nil
}

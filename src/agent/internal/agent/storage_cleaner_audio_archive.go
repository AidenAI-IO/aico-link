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

// AudioArchiveCleaner cleans up old audio archive files
type AudioArchiveCleaner struct {
	archiveDir   string
	retentionAge time.Duration
	maxFiles     int
	priority     int
}

// NewAudioArchiveCleaner creates a new audio archive cleaner
func NewAudioArchiveCleaner(archiveDir string, retentionDays int, maxFiles int, priority int) *AudioArchiveCleaner {
	return &AudioArchiveCleaner{
		archiveDir:   archiveDir,
		retentionAge: time.Duration(retentionDays) * 24 * time.Hour,
		maxFiles:     maxFiles,
		priority:     priority,
	}
}

func (c *AudioArchiveCleaner) Name() string {
	if c.retentionAge > 0 {
		return fmt.Sprintf("audio_archive_%dd", int(c.retentionAge.Hours()/24))
	}
	return fmt.Sprintf("audio_archive_keep_%d", c.maxFiles)
}

func (c *AudioArchiveCleaner) Priority() int {
	return c.priority
}

type audioFileInfo struct {
	path    string
	modTime time.Time
	size    int64
}

func (c *AudioArchiveCleaner) collectFiles() ([]audioFileInfo, error) {
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

	var files []audioFileInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".wav" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		files = append(files, audioFileInfo{
			path:    filepath.Join(c.archiveDir, entry.Name()),
			modTime: info.ModTime(),
			size:    info.Size(),
		})
	}

	// Sort by modification time (oldest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	return files, nil
}

func (c *AudioArchiveCleaner) EstimateReclaimable(context.Context) (uint64, error) {
	files, err := c.collectFiles()
	if err != nil {
		return 0, err
	}

	var total uint64
	if c.retentionAge == 0 && c.maxFiles == 0 {
		for _, file := range files {
			total += uint64(file.size)
		}
		return total, nil
	}
	now := time.Now()
	cutoff := now.Add(-c.retentionAge)

	excessCount := 0
	if c.maxFiles > 0 && len(files) > c.maxFiles {
		excessCount = len(files) - c.maxFiles
	}
	for i, file := range files {
		deleteByAge := c.retentionAge > 0 && file.modTime.Before(cutoff)
		deleteByCount := i < excessCount
		if deleteByAge || deleteByCount {
			total += uint64(file.size)
		}
	}

	return total, nil
}

func (c *AudioArchiveCleaner) Clean(context.Context) (uint64, error) {
	files, err := c.collectFiles()
	if err != nil {
		return 0, err
	}

	if len(files) == 0 {
		return 0, nil
	}

	var totalFreed uint64
	var deletedCount int
	if c.retentionAge == 0 && c.maxFiles == 0 {
		for _, file := range files {
			if err := os.Remove(file.path); err != nil {
				return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(file.path), err)
			}
			totalFreed += uint64(file.size)
			deletedCount++
		}
		if deletedCount > 0 {
			logAudioCleanupResult(deletedCount, totalFreed)
		}
		return totalFreed, nil
	}
	now := time.Now()
	cutoff := now.Add(-c.retentionAge)

	// Delete files based on retention age
	if c.retentionAge > 0 {
		for _, file := range files {
			if !file.modTime.Before(cutoff) {
				break // Files are sorted, so we can stop
			}

			if err := os.Remove(file.path); err != nil {
				return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(file.path), err)
			}

			totalFreed += uint64(file.size)
			deletedCount++
		}
	}

	// Delete files beyond max count
	if c.maxFiles > 0 && len(files) > c.maxFiles {
		// Re-collect files after age-based deletion
		files, err = c.collectFiles()
		if err != nil {
			return totalFreed, err
		}

		for len(files) > c.maxFiles {
			oldest := files[0]
			if err := os.Remove(oldest.path); err != nil {
				return totalFreed, fmt.Errorf("remove %s: %w", filepath.Base(oldest.path), err)
			}

			totalFreed += uint64(oldest.size)
			deletedCount++
			files = files[1:]
		}
	}

	if deletedCount > 0 {
		logAudioCleanupResult(deletedCount, totalFreed)
	}

	return totalFreed, nil
}

func logAudioCleanupResult(deletedCount int, totalFreed uint64) {
	_ = logging.LogEvent(logging.Info, "agent", "storage_cleanup", "audio_files_deleted",
		logging.Field{Key: "files", Value: deletedCount},
		logging.Field{Key: "freed_bytes", Value: totalFreed},
	)
}

func (c *AudioArchiveCleaner) ForceClean(ctx context.Context) (uint64, error) {
	forced := *c
	forced.retentionAge = 0
	forced.maxFiles = 0
	return forced.Clean(ctx)
}

package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// sharedFilesStateLimit bounds the shared_files state entry, which rides
	// on every user turn.
	sharedFilesStateLimit = 20
	// sharedFilesStateMaxScan bounds the walk so a large share cannot slow
	// down every turn; files past it are reported as unlisted.
	sharedFilesStateMaxScan = 2000
)

// sharedFilesStateUpdater reports the files in the shared directory, newest
// first, so the model knows what the user uploaded and what earlier turns
// produced without listing the directory first.
type sharedFilesStateUpdater struct {
	snapshot func() Config
}

func (u *sharedFilesStateUpdater) UpdateState() map[string]string {
	timezone := defaultTimezone
	if u != nil && u.snapshot != nil {
		timezone = u.snapshot().TimezoneOrDefault()
	}
	return map[string]string{"shared_files": sharedFilesState(timezone)}
}

type sharedFileStateEntry struct {
	rel      string
	size     int64
	modified time.Time
}

// sharedFilesState lists regular files below the shared directory, newest
// first, one per line. Hidden entries and symbolic links are skipped, as in
// GET /api/files. It returns "" when there are none, so the entry is omitted.
func sharedFilesState(timezone string) string {
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		return ""
	}
	defer root.Close()

	var entries []sharedFileStateEntry
	truncated := false
	_ = fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if !validSharedName(entry.Name()) || entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		if len(entries) >= sharedFilesStateMaxScan {
			truncated = true
			return fs.SkipAll
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, sharedFileStateEntry{rel: rel, size: info.Size(), modified: info.ModTime()})
		return nil
	})
	if len(entries) == 0 {
		return ""
	}

	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].modified.Equal(entries[j].modified) {
			return entries[i].modified.After(entries[j].modified)
		}
		return entries[i].rel < entries[j].rel
	})
	sharedUploadsMu.Lock()
	uploads := readSharedUploads(root)
	sharedUploadsMu.Unlock()

	shown := entries
	if len(shown) > sharedFilesStateLimit {
		shown = shown[:sharedFilesStateLimit]
	}
	var b strings.Builder
	count := fmt.Sprint(len(entries))
	if truncated {
		count = "over " + count
	}
	fmt.Fprintf(&b, "%s in %s, newest first", count, sharedFilesDir())
	for _, entry := range shown {
		fmt.Fprintf(&b, "\n- %s (%s, shared by %s, modified %s)",
			filepath.Join(sharedFilesDir(), filepath.FromSlash(entry.rel)),
			formatSharedFileSize(entry.size),
			sharedFileSource(uploads, path.Clean(entry.rel)),
			configuredTime(entry.modified, timezone).Format("2006-01-02 15:04"))
	}
	if len(entries) > len(shown) {
		fmt.Fprintf(&b, "\n- (older files not listed; use shell to list %s)", sharedFilesDir())
	}
	return b.String()
}

func formatSharedFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value, suffix := float64(size)/unit, "KB"
	for _, next := range []string{"MB", "GB"} {
		if value < unit {
			break
		}
		value, suffix = value/unit, next
	}
	return fmt.Sprintf("%.1f %s", value, suffix)
}

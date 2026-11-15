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
	// sharedFilesStateMaxVisits bounds the directory entries one state
	// refresh may visit, files and directories alike, so a large share
	// cannot slow down every turn.
	sharedFilesStateMaxVisits = 5000
	// sharedFilesStateReadBatch bounds how many entries a directory read
	// returns at once, so one huge directory is not loaded in full.
	sharedFilesStateReadBatch = 256
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

type sharedDirStateEntry struct {
	rel      string
	modified time.Time
}

// newerSharedFile orders files newest first, then by path for a stable list.
func newerSharedFile(a, b sharedFileStateEntry) bool {
	if !a.modified.Equal(b.modified) {
		return a.modified.After(b.modified)
	}
	return a.rel < b.rel
}

// sharedFilesState lists regular files below the shared directory, newest
// first, one per line. Hidden entries and symbolic links are skipped, as in
// GET /api/files. It returns "" when there are none, so the entry is omitted.
//
// The scan visits at most sharedFilesStateMaxVisits entries. Directories are
// read in batches and visited most recently modified first, since adding a
// file updates its directory's modification time; when the budget runs out,
// the list says it covers only the entries checked.
func sharedFilesState(timezone string) string {
	root, err := os.OpenRoot(sharedFilesDir())
	if err != nil {
		return ""
	}
	defer root.Close()

	var newest []sharedFileStateEntry
	files, visits := 0, 0
	complete := true
	keep := func(entry sharedFileStateEntry) {
		files++
		index := sort.Search(len(newest), func(i int) bool { return newerSharedFile(entry, newest[i]) })
		if index >= sharedFilesStateLimit {
			return
		}
		newest = append(newest, sharedFileStateEntry{})
		copy(newest[index+1:], newest[index:])
		newest[index] = entry
		if len(newest) > sharedFilesStateLimit {
			newest = newest[:sharedFilesStateLimit]
		}
	}

	pending := []sharedDirStateEntry{{rel: "."}}
scan:
	for len(pending) > 0 {
		sort.Slice(pending, func(i, j int) bool { return pending[i].modified.After(pending[j].modified) })
		dir := pending[0]
		pending = pending[1:]
		handle, err := root.Open(dir.rel)
		if err != nil {
			continue
		}
		for {
			batch, err := handle.ReadDir(sharedFilesStateReadBatch)
			for _, entry := range batch {
				if visits >= sharedFilesStateMaxVisits {
					complete = false
					handle.Close()
					break scan
				}
				visits++
				if !validSharedName(entry.Name()) || entry.Type()&fs.ModeSymlink != 0 {
					continue
				}
				rel := path.Join(dir.rel, entry.Name())
				info, infoErr := entry.Info()
				if infoErr != nil {
					continue
				}
				switch {
				case entry.IsDir():
					pending = append(pending, sharedDirStateEntry{rel: rel, modified: info.ModTime()})
				case entry.Type().IsRegular():
					keep(sharedFileStateEntry{rel: rel, size: info.Size(), modified: info.ModTime()})
				}
			}
			if err != nil || len(batch) == 0 {
				break
			}
		}
		handle.Close()
	}
	if len(pending) > 0 {
		complete = false
	}
	if len(newest) == 0 && complete {
		return ""
	}

	sharedUploadsMu.Lock()
	uploads := readSharedUploads(root)
	sharedUploadsMu.Unlock()

	var b strings.Builder
	if complete {
		fmt.Fprintf(&b, "%d in %s, newest first", files, sharedFilesDir())
	} else {
		fmt.Fprintf(&b, "%s is too large to scan in full; newest of the %d files checked", sharedFilesDir(), files)
	}
	for _, entry := range newest {
		fmt.Fprintf(&b, "\n- %s (%s, shared by %s, modified %s)",
			filepath.Join(sharedFilesDir(), filepath.FromSlash(entry.rel)),
			formatSharedFileSize(entry.size),
			sharedFileSource(uploads, entry.rel),
			configuredTime(entry.modified, timezone).Format("2006-01-02 15:04"))
	}
	switch {
	case !complete:
		fmt.Fprintf(&b, "\n- (other files were not checked; use shell to list %s)", sharedFilesDir())
	case files > len(newest):
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

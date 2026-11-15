package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSharedFilesStateListsNewestFilesWithSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", dir)
	if got := sharedFilesState("UTC"); got != "" {
		t.Fatalf("empty share state = %q, want omitted", got)
	}

	base := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	write := func(rel string, size int, age time.Duration) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, base.Add(-age), base.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("report.csv", 42, time.Hour)
	write("outputs/summary.pdf", 2048, time.Minute)
	write(".uploads.json", 0, 0)
	write(".hidden/secret.txt", 1, 0)
	if err := os.WriteFile(filepath.Join(dir, ".uploads.json"), []byte(`["report.csv"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "report.csv"), filepath.Join(dir, "link.csv")); err != nil {
		t.Fatal(err)
	}

	want := "2 in " + dir + ", newest first" +
		"\n- " + filepath.Join(dir, "outputs", "summary.pdf") + " (2.0 KB, shared by aico, modified 2026-11-15 15:59)" +
		"\n- " + filepath.Join(dir, "report.csv") + " (42 B, shared by user, modified 2026-11-15 15:00)"
	if got := sharedFilesState("Asia/Shanghai"); got != want {
		t.Fatalf("shared files state =\n%s\nwant\n%s", got, want)
	}

	for i := range sharedFilesStateLimit {
		write(fmt.Sprintf("bulk/%02d.txt", i), 1, time.Duration(i+2)*time.Hour)
	}
	got := sharedFilesState("UTC")
	if lines := strings.Split(got, "\n"); len(lines) != sharedFilesStateLimit+2 ||
		!strings.HasPrefix(got, fmt.Sprintf("%d in ", sharedFilesStateLimit+2)) ||
		lines[len(lines)-1] != "- (older files not listed; use shell to list "+dir+")" {
		t.Fatalf("capped shared files state =\n%s", got)
	}
}

// The scan budget must not hide a new upload behind a large, older directory
// that sorts first, and a scan cut short must say it is partial.
func TestSharedFilesStateBudgetPrefersRecentDirectories(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", dir)
	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	archive := filepath.Join(dir, "2025")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range sharedFilesStateMaxVisits + 10 {
		file := filepath.Join(archive, fmt.Sprintf("%05d.txt", i))
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(archive, old, old); err != nil {
		t.Fatal(err)
	}
	upload := filepath.Join(dir, "2026", "new.png")
	if err := os.MkdirAll(filepath.Dir(upload), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upload, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := sharedFilesState("UTC")
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], "too large to scan in full") || !strings.HasPrefix(lines[1], "- "+upload+" ") ||
		lines[len(lines)-1] != "- (other files were not checked; use shell to list "+dir+")" {
		t.Fatalf("budgeted shared files state =\n%s", got)
	}
}

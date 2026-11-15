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

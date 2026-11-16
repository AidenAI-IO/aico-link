package backup

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSourceLstatDoesNotFollowLinksOrReadSpecialFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{{"file", 0}, {"link", os.ModeSymlink}, {"fifo", os.ModeNamedPipe}} {
		info, err := sourceLstat(filepath.Join(root, item.name))
		if err != nil || info.Mode().Type() != item.mode {
			t.Fatalf("lstat %s: info=%v, err=%v", item.name, info, err)
		}
	}
	if err := os.Symlink(root, filepath.Join(root, "parent-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceLstat(filepath.Join(root, "parent-link", "file")); err == nil {
		t.Fatal("source metadata lookup followed an intermediate symlink")
	}
}

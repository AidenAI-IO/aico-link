package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Open each parent relative to its already-open directory descriptor. The
// trusted starting descriptor is /; neither intermediate nor final symlinks
// can redirect a content read outside the registered source path.
func openSourceParent(value string) (*os.File, string, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
		return nil, "", fmt.Errorf("source path is not absolute and clean: %q", value)
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	parts := strings.Split(strings.TrimPrefix(value, "/"), "/")
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, "", fmt.Errorf("open source parent %s: %w", value, openErr)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), filepath.Dir(value)), parts[len(parts)-1], nil
}

func openSourcePath(value string, flags int) (*os.File, error) {
	parent, name, err := openSourceParent(value)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), name, sourceOpenFlags(flags), 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), value), nil
}

func sourceLstat(value string) (fs.FileInfo, error) {
	file, err := openSourcePath(value, sourceMetadataOpenFlags)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func sourceReadlink(value string) (string, error) {
	parent, name, err := openSourceParent(value)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	for size := 256; size <= 64*1024; size *= 2 {
		buffer := make([]byte, size)
		n, err := unix.Readlinkat(int(parent.Fd()), name, buffer)
		if err != nil {
			return "", err
		}
		if n < len(buffer) {
			return string(buffer[:n]), nil
		}
	}
	return "", fmt.Errorf("source symlink is too long: %s", value)
}

func readSourceSettings(value string) ([]byte, error) {
	file, err := openSourcePath(value, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLogicalSettingsFileSize {
		return nil, fmt.Errorf("source settings is not a bounded regular file: %s", value)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLogicalSettingsFileSize+1))
	if len(data) > maxLogicalSettingsFileSize {
		return nil, fmt.Errorf("source settings grew beyond the size limit: %s", value)
	}
	return data, err
}

func walkSourceDirectory(ctx context.Context, value string, visit fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := sourceLstat(value)
	if err != nil {
		return err
	}
	if err := visit(value, fs.FileInfoToDirEntry(info), nil); err != nil {
		if err == filepath.SkipDir {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	directory, err := openSourcePath(value, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return err
	}
	opened, err := directory.Stat()
	if err != nil || !sameFingerprint(fingerprint(info), fingerprint(opened)) {
		_ = directory.Close()
		return fmt.Errorf("source directory changed while planning: %s", value)
	}
	entries, err := directory.ReadDir(-1)
	_ = directory.Close()
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := walkSourceDirectory(ctx, filepath.Join(value, entry.Name()), visit); err != nil {
			return err
		}
	}
	return nil
}

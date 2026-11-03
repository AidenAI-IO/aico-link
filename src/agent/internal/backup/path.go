package backup

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// IsReservedSDUserPath applies the same managed-root boundary to backup and
// restore, including FAT case folding and trailing-dot aliases. Top-level
// DOS 8.3-shaped names with a tilde and numeric suffix are conservatively
// reserved: their long-name target cannot be determined from an archive.
func IsReservedSDUserPath(value string) bool {
	root := strings.ToLower(strings.TrimRight(strings.SplitN(value, "/", 2)[0], "."))
	switch root {
	case "aico", ".aico-restore", "aico-res":
		// The last name is the journal alias created by VFAT nonumtail.
		return true
	}
	stem, extension, hasExtension := strings.Cut(root, ".")
	if len(stem) > 8 || (hasExtension && (len(extension) > 3 || strings.ContainsAny(extension, ".~"))) {
		return false
	}
	prefix, suffix, hasTilde := strings.Cut(stem, "~")
	if !hasTilde || prefix == "" || len(prefix) > 6 || suffix == "" {
		return false
	}
	for _, digit := range suffix {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func validateArchivePath(value string) error {
	if value == "" || value == "." {
		return fmt.Errorf("archive path is empty")
	}
	if strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return fmt.Errorf("archive path contains an invalid character")
	}
	if strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return fmt.Errorf("archive path %q is not a clean relative path", value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("archive path %q contains an invalid segment", value)
		}
	}
	return nil
}

func validRelativeSymlink(linkPath, target string) bool {
	if target == "" || strings.ContainsRune(target, '\x00') || strings.Contains(target, "\\") || path.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(linkPath), target))
	return resolved != "." && resolved != ".." && !strings.HasPrefix(resolved, "../")
}

func excludedPath(relative string, patterns []string) bool {
	relative = filepath.ToSlash(relative)
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(pattern)
		if strings.HasPrefix(pattern, "**/") {
			remainder := strings.TrimPrefix(pattern, "**/")
			for suffix := relative; ; {
				if matched, _ := path.Match(remainder, suffix); matched {
					return true
				}
				index := strings.IndexByte(suffix, '/')
				if index < 0 {
					break
				}
				suffix = suffix[index+1:]
			}
		}
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if relative == prefix || strings.HasPrefix(relative, prefix+"/") {
				return true
			}
		}
		if matched, _ := path.Match(pattern, relative); matched {
			return true
		}
	}
	return false
}

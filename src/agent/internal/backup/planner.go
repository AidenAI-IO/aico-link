package backup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const maxLogicalSettingsFileSize = 1024 * 1024

type PlanOptions struct {
	Mode       Mode
	Components []ComponentID
	Source     SourceIdentity
	Storage    StorageIdentity
	CreatedAt  time.Time
	BackupID   string
}

type fileFingerprint struct {
	Device    uint64
	Inode     uint64
	Size      int64
	Mode      fs.FileMode
	MtimeNano int64
}

type PlannedEntry struct {
	Manifest    FileManifest
	SourcePath  string
	Generated   []byte
	fingerprint fileFingerprint
}

type Plan struct {
	Manifest Manifest
	Entries  []PlannedEntry
}

type Planner struct {
	Roots Roots
	Now   func() time.Time
}

func NewPlanner(roots Roots) *Planner {
	return &Planner{Roots: roots, Now: time.Now}
}

func (p *Planner) Plan(ctx context.Context, options PlanOptions) (*Plan, error) {
	if err := p.Roots.Validate(); err != nil {
		return nil, err
	}
	definitions, err := ValidateSelection(options.Mode, options.Components, options.Storage.SDPresent)
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if definition.ID == ComponentDeviceIdentity && strings.TrimSpace(options.Source.HardwareID) == "" {
			return nil, fmt.Errorf("same-device identity backup requires an immutable hardware_id")
		}
	}
	if options.Mode == ModePortable {
		options.Source.MachineID = ""
		options.Source.HardwareID = ""
	}
	createdAt := options.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = p.Now().UTC()
	}
	backupID := strings.TrimSpace(options.BackupID)
	if backupID == "" {
		backupID = uuid.NewString()
	}
	plan := &Plan{Manifest: Manifest{
		SchemaVersion: 1,
		BackupID:      backupID,
		CreatedAt:     createdAt,
		Source:        options.Source,
		Mode:          options.Mode,
		Storage:       options.Storage,
	}}

	for _, definition := range definitions {
		before := len(plan.Entries)
		for _, source := range definition.Sources(p.Roots) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			var entries []PlannedEntry
			switch source.Kind {
			case SourceFile:
				entries, err = p.planFile(ctx, definition.ID, source)
			case SourceDirectory:
				entries, err = p.planDirectory(ctx, definition.ID, source)
			case SourceOTASettings:
				entries, err = p.planOTASettings(ctx, definition.ID, source)
			default:
				err = fmt.Errorf("unsupported source kind %d", source.Kind)
			}
			if err != nil {
				return nil, fmt.Errorf("plan component %s: %w", definition.ID, err)
			}
			plan.Entries = append(plan.Entries, entries...)
		}
		if len(plan.Entries) == before {
			continue
		}
	}

	deduplicateAudioEntries(plan.Entries)
	plan.Entries = compactEntries(plan.Entries)
	sort.Slice(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].Manifest.Component != plan.Entries[j].Manifest.Component {
			return plan.Entries[i].Manifest.Component < plan.Entries[j].Manifest.Component
		}
		return plan.Entries[i].Manifest.Path < plan.Entries[j].Manifest.Path
	})
	plan.rebuildManifest(definitions)
	if err := plan.Manifest.Validate(); err != nil {
		return nil, fmt.Errorf("generated manifest is invalid: %w", err)
	}
	return plan, nil
}

func (p *Planner) planFile(ctx context.Context, component ComponentID, source SourceSpec) ([]PlannedEntry, error) {
	entry, err := inspectPath(ctx, component, source.Path, source.ArchivePath, source.SD)
	if errors.Is(err, fs.ErrNotExist) && source.Optional {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry.Manifest.Type == FileTypeDirectory {
		return nil, fmt.Errorf("expected regular file at %s", source.Path)
	}
	return []PlannedEntry{entry}, nil
}

func (p *Planner) planDirectory(ctx context.Context, component ComponentID, source SourceSpec) ([]PlannedEntry, error) {
	info, err := sourceLstat(source.Path)
	if errors.Is(err, fs.ErrNotExist) && source.Optional {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("expected directory at %s", source.Path)
	}
	var result []PlannedEntry
	err = walkSourceDirectory(ctx, source.Path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(source.Path, current)
		if err != nil {
			return err
		}
		if relative == "." {
			if source.ArchivePath == "" {
				return nil
			}
			relative = ""
		}
		archivePath := filepath.ToSlash(filepath.Join(source.ArchivePath, relative))
		if excludedPath(archivePath, source.Exclude) || (component == ComponentSDUserFiles && IsReservedSDUserPath(archivePath)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		planned, err := inspectPath(ctx, component, current, archivePath, source.SD)
		if err != nil {
			return err
		}
		result = append(result, planned)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func inspectPath(ctx context.Context, component ComponentID, sourcePath, archivePath string, sd bool) (PlannedEntry, error) {
	if err := ctx.Err(); err != nil {
		return PlannedEntry{}, err
	}
	if err := validateArchivePath(filepath.ToSlash(archivePath)); err != nil {
		return PlannedEntry{}, err
	}
	info, err := sourceLstat(sourcePath)
	if err != nil {
		return PlannedEntry{}, err
	}
	manifest := FileManifest{
		Component:     component,
		Path:          filepath.ToSlash(archivePath),
		Mode:          fmt.Sprintf("%04o", info.Mode().Perm()),
		MtimeUnixNano: info.ModTime().UnixNano(),
	}
	if sd {
		manifest.StorageLayer = "sd"
	} else {
		manifest.StorageLayer = "userdata"
	}
	planned := PlannedEntry{Manifest: manifest, SourcePath: sourcePath, fingerprint: fingerprint(info)}
	switch {
	case info.Mode().IsRegular():
		digest, size, stable, err := hashRegularFile(ctx, sourcePath, info)
		if err != nil {
			return PlannedEntry{}, err
		}
		if !stable {
			info, err = sourceLstat(sourcePath)
			if err != nil {
				return PlannedEntry{}, err
			}
			digest, size, stable, err = hashRegularFile(ctx, sourcePath, info)
			if err != nil {
				return PlannedEntry{}, err
			}
			if !stable {
				return PlannedEntry{}, fmt.Errorf("file changed while planning: %s", sourcePath)
			}
			planned.fingerprint = fingerprint(info)
		}
		planned.Manifest.Type = FileTypeRegular
		planned.Manifest.Size = size
		planned.Manifest.SHA256 = digest
	case info.IsDir():
		planned.Manifest.Type = FileTypeDirectory
		planned.Manifest.Mode = fmt.Sprintf("%04o", info.Mode().Perm()&0o777)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := sourceReadlink(sourcePath)
		if err != nil {
			return PlannedEntry{}, err
		}
		if !validRelativeSymlink(planned.Manifest.Path, filepath.ToSlash(target)) {
			return PlannedEntry{}, fmt.Errorf("symlink %s points outside its component", sourcePath)
		}
		planned.Manifest.Type = FileTypeSymlink
		planned.Manifest.LinkTarget = filepath.ToSlash(target)
		planned.Manifest.Mode = "0777"
	default:
		return PlannedEntry{}, fmt.Errorf("unsupported special file %s (%s)", sourcePath, info.Mode())
	}
	return planned, nil
}

func hashRegularFile(ctx context.Context, path string, before fs.FileInfo) (string, int64, bool, error) {
	file, err := openSourcePath(path, unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return "", 0, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", 0, false, err
	}
	if !sameFingerprint(fingerprint(before), fingerprint(opened)) || !opened.Mode().IsRegular() {
		return "", 0, false, nil
	}
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, false, err
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			size += int64(count)
			_, _ = hash.Write(buffer[:count])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, false, readErr
		}
	}
	after, err := sourceLstat(path)
	if err != nil {
		return "", 0, false, err
	}
	stable := size == opened.Size() && sameFingerprint(fingerprint(opened), fingerprint(after))
	return hex.EncodeToString(hash.Sum(nil)), size, stable, nil
}

func fingerprint(info fs.FileInfo) fileFingerprint {
	result := fileFingerprint{Size: info.Size(), Mode: info.Mode(), MtimeNano: info.ModTime().UnixNano()}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		result.Device = uint64(stat.Dev)
		result.Inode = stat.Ino
	}
	return result
}

func sameFingerprint(left, right fileFingerprint) bool {
	return left == right
}

func (p *Planner) planOTASettings(ctx context.Context, component ComponentID, source SourceSpec) ([]PlannedEntry, error) {
	data, err := readSourceSettings(source.Path)
	if errors.Is(err, fs.ErrNotExist) && source.Optional {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxLogicalSettingsFileSize {
		return nil, fmt.Errorf("OTA configuration is too large")
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse OTA configuration: %w", err)
	}
	settings := make(map[string]any)
	for _, key := range []string{"manifest_url", "github_proxy_url", "github_token"} {
		if value, ok := config[key].(string); ok && strings.TrimSpace(value) != "" {
			settings[key] = value
		}
	}
	if tokenPath, ok := config["github_token_path"].(string); ok && strings.TrimSpace(tokenPath) != "" {
		if value, err := p.readApprovedOTAFile(tokenPath); err == nil && len(value) > 0 {
			settings["github_token"] = strings.TrimSpace(string(value))
		}
	}
	if keyPath, ok := config["public_key_path"].(string); ok && strings.TrimSpace(keyPath) != "" {
		if value, err := p.readApprovedOTAFile(keyPath); err == nil && len(value) > 0 {
			settings["public_key"] = map[string]any{
				"filename":   filepath.Base(keyPath),
				"pem_base64": base64.StdEncoding.EncodeToString(value),
			}
		}
	}
	if len(settings) == 0 {
		return nil, nil
	}
	generated, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	generated = append(generated, '\n')
	digest := sha256.Sum256(generated)
	entry := PlannedEntry{
		Manifest: FileManifest{
			Component: component, Path: source.ArchivePath, Type: FileTypeRegular,
			Mode: "0600", Size: int64(len(generated)), SHA256: hex.EncodeToString(digest[:]),
			StorageLayer: "userdata",
		},
		Generated: generated,
	}
	return []PlannedEntry{entry}, ctx.Err()
}

func (p *Planner) readApprovedOTAFile(value string) ([]byte, error) {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return nil, fmt.Errorf("OTA credential path is not an absolute clean path")
	}
	approved := []string{
		filepath.Join(p.Roots.Userdata, "debian/ota"),
		filepath.Join(p.Roots.Userdata, "system"),
		filepath.Join(p.Roots.Userdata, "agent"),
	}
	allowed := false
	for _, root := range approved {
		relative, err := filepath.Rel(root, value)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("OTA credential path is outside approved persistent roots")
	}
	info, err := sourceLstat(value)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxLogicalSettingsFileSize {
		return nil, fmt.Errorf("OTA credential is not an approved regular file")
	}
	return readSourceSettings(value)
}

func deduplicateAudioEntries(entries []PlannedEntry) {
	emmc := make(map[string]int)
	for index := range entries {
		entry := &entries[index]
		if entry.Manifest.Component == ComponentAudioArchive && entry.Manifest.Type == FileTypeRegular {
			emmc[entry.Manifest.Path] = index
		}
	}
	for index := range entries {
		entry := &entries[index]
		if entry.Manifest.Component != ComponentSDManagedAudio || entry.Manifest.Type != FileTypeRegular {
			continue
		}
		emmcIndex, ok := emmc[entry.Manifest.Path]
		if !ok {
			continue
		}
		if entries[emmcIndex].Manifest.Size == entry.Manifest.Size && entries[emmcIndex].Manifest.SHA256 == entry.Manifest.SHA256 {
			entry.Manifest.Component = ""
			continue
		}
		entries[emmcIndex].Manifest.Conflict = true
		entry.Manifest.Conflict = true
	}
}

func compactEntries(entries []PlannedEntry) []PlannedEntry {
	result := entries[:0]
	for _, entry := range entries {
		if entry.Manifest.Component != "" {
			result = append(result, entry)
		}
	}
	return result
}

func (p *Plan) rebuildManifest(definitions []ComponentDefinition) {
	p.Manifest.Files = p.Manifest.Files[:0]
	p.Manifest.Components = p.Manifest.Components[:0]
	definitionByID := make(map[ComponentID]ComponentDefinition, len(definitions))
	for _, definition := range definitions {
		definitionByID[definition.ID] = definition
	}
	grouped := make(map[ComponentID][]FileManifest)
	for _, entry := range p.Entries {
		p.Manifest.Files = append(p.Manifest.Files, entry.Manifest)
		grouped[entry.Manifest.Component] = append(grouped[entry.Manifest.Component], entry.Manifest)
	}
	for _, definition := range definitions {
		files := grouped[definition.ID]
		if len(files) == 0 {
			continue
		}
		var expanded int64
		for _, file := range files {
			if file.Type == FileTypeRegular {
				expanded += file.Size
			}
		}
		p.Manifest.Components = append(p.Manifest.Components, ComponentManifest{
			ID: definition.ID, SchemaVersion: definition.SchemaVersion,
			FileCount: len(files), ExpandedSize: expanded,
			SHA256: componentDigest(files), Sensitive: definition.Sensitive,
		})
	}
	p.Manifest.Normalize()
}

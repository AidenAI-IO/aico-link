package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	FormatName      = "aico-backup"
	FormatVersion   = 1
	ArchiveMIMEType = "application/vnd.aico.backup"
	ArchiveSuffix   = ".aico-backup"

	DefaultChunkSize   = 1024 * 1024
	DefaultMemoryKiB   = 32 * 1024
	DefaultIterations  = 3
	DefaultParallelism = 1

	MaxManifestSize = 8 * 1024 * 1024

	// MaxUploadChunkSize bounds one restore upload block.  Upload blocks are
	// larger than encrypted frames so the public header, the first frame and
	// therefore manifest.json normally arrive in block 0.
	MaxUploadChunkSize = 4 * 1024 * 1024
)

// UploadChunkSize returns the restore upload block size for an archive whose
// encrypted frames are frameSize bytes.
func UploadChunkSize(frameSize int) int {
	size := frameSize * 4
	if size > MaxUploadChunkSize {
		size = MaxUploadChunkSize
	}
	if size < frameSize {
		size = frameSize
	}
	return size
}

type Mode string

const (
	ModeSameDevice Mode = "same_device"
	ModePortable   Mode = "portable"
)

func (m Mode) Validate() error {
	switch m {
	case ModeSameDevice, ModePortable:
		return nil
	default:
		return fmt.Errorf("unsupported backup mode %q", m)
	}
}

type ComponentID string

const (
	ComponentAgentConfig       ComponentID = "agent_config"
	ComponentSystemEnvironment ComponentID = "system_environment"
	ComponentNetwork           ComponentID = "network"
	ComponentOTASettings       ComponentID = "ota_settings"
	ComponentAgentSkills       ComponentID = "agent_skills"
	ComponentAgentMemory       ComponentID = "agent_memory"
	ComponentAgentSessions     ComponentID = "agent_sessions"
	ComponentUserHome          ComponentID = "user_home"
	ComponentPreferences       ComponentID = "preferences"
	ComponentDeviceIdentity    ComponentID = "device_identity"
	ComponentAudioArchive      ComponentID = "audio_archive"
	ComponentSDManagedAudio    ComponentID = "sd_managed_audio"
	ComponentSDUserFiles       ComponentID = "sd_user_files"
	ComponentPythonEnvironment ComponentID = "python_environment"
	ComponentDiagnostics       ComponentID = "diagnostics"
)

var CommitOrder = []ComponentID{
	ComponentAgentConfig,
	ComponentSystemEnvironment,
	ComponentNetwork,
	ComponentOTASettings,
	ComponentAgentSkills,
	ComponentAgentMemory,
	ComponentAgentSessions,
	ComponentUserHome,
	ComponentPreferences,
	ComponentDeviceIdentity,
	ComponentAudioArchive,
	ComponentSDManagedAudio,
	ComponentSDUserFiles,
	ComponentPythonEnvironment,
	ComponentDiagnostics,
}

type PublicHeader struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
	Protection Protection `json:"protection"`
}

type Protection struct {
	Algorithm   string `json:"algorithm"`
	KDF         string `json:"kdf"`
	Salt        string `json:"salt,omitempty"`
	NoncePrefix string `json:"nonce_prefix,omitempty"`
	MemoryKiB   uint32 `json:"memory_kib,omitempty"`
	Iterations  uint32 `json:"iterations,omitempty"`
	Parallelism uint8  `json:"parallelism,omitempty"`
	ChunkSize   int    `json:"chunk_size"`
}

func (h PublicHeader) Validate() error {
	if h.Format != FormatName {
		return fmt.Errorf("unsupported backup format %q", h.Format)
	}
	if h.Version != FormatVersion {
		return fmt.Errorf("unsupported backup format version %d", h.Version)
	}
	if h.CreatedAt.IsZero() {
		return fmt.Errorf("backup creation time is missing")
	}
	return h.Protection.Validate()
}

func (p Protection) Validate() error {
	if p.ChunkSize < 64*1024 || p.ChunkSize > 4*1024*1024 {
		return fmt.Errorf("chunk_size must be between 65536 and 4194304")
	}
	if p.Algorithm == "sha256-chunked" {
		if p.KDF != "none" || p.Salt != "" || p.NoncePrefix != "" || p.MemoryKiB != 0 || p.Iterations != 0 || p.Parallelism != 0 {
			return fmt.Errorf("unencrypted backups must not contain key derivation parameters")
		}
		return nil
	}
	if p.Algorithm != "xchacha20-poly1305-chunked" {
		return fmt.Errorf("unsupported protection algorithm %q", p.Algorithm)
	}
	if p.KDF != "argon2id" {
		return fmt.Errorf("unsupported key derivation function %q", p.KDF)
	}
	if p.MemoryKiB < 8*1024 || p.MemoryKiB > 64*1024 {
		return fmt.Errorf("argon2 memory_kib must be between 8192 and 65536")
	}
	if p.Iterations < 1 || p.Iterations > 6 {
		return fmt.Errorf("argon2 iterations must be between 1 and 6")
	}
	if p.Parallelism < 1 || p.Parallelism > 4 {
		return fmt.Errorf("argon2 parallelism must be between 1 and 4")
	}
	if p.ChunkSize < 64*1024 || p.ChunkSize > 4*1024*1024 {
		return fmt.Errorf("encrypted chunk_size must be between 65536 and 4194304")
	}
	return nil
}

type Manifest struct {
	SchemaVersion int                 `json:"schema_version"`
	BackupID      string              `json:"backup_id"`
	CreatedAt     time.Time           `json:"created_at"`
	Source        SourceIdentity      `json:"source"`
	Mode          Mode                `json:"mode"`
	Components    []ComponentManifest `json:"components"`
	Storage       StorageIdentity     `json:"storage"`
	Files         []FileManifest      `json:"files"`
}

type SourceIdentity struct {
	HardwareID      string `json:"hardware_id,omitempty"`
	MachineID       string `json:"machine_id,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	FirmwareBuild   string `json:"firmware_build,omitempty"`
	ActiveSlot      string `json:"active_slot,omitempty"`
	Architecture    string `json:"architecture,omitempty"`
	PythonVersion   string `json:"python_version,omitempty"`
	LibcABI         string `json:"libc_abi,omitempty"`
}

type StorageIdentity struct {
	UserdataUUID string `json:"userdata_uuid,omitempty"`
	SDPresent    bool   `json:"sd_present"`
	SDUUID       string `json:"sd_uuid,omitempty"`
	SDDevice     string `json:"sd_device,omitempty"`
	SDMountID    string `json:"sd_mount_id,omitempty"`
}

type ComponentManifest struct {
	ID            ComponentID `json:"id"`
	SchemaVersion int         `json:"schema_version"`
	FileCount     int         `json:"file_count"`
	ExpandedSize  int64       `json:"expanded_size"`
	SHA256        string      `json:"sha256"`
	Sensitive     bool        `json:"sensitive,omitempty"`
}

type FileType string

const (
	FileTypeRegular   FileType = "regular"
	FileTypeDirectory FileType = "directory"
	FileTypeSymlink   FileType = "symlink"
)

type FileManifest struct {
	Component     ComponentID `json:"component"`
	Path          string      `json:"path"`
	Type          FileType    `json:"type"`
	Mode          string      `json:"mode"`
	Size          int64       `json:"size,omitempty"`
	MtimeUnixNano int64       `json:"mtime_unix_nano"`
	SHA256        string      `json:"sha256,omitempty"`
	LinkTarget    string      `json:"link_target,omitempty"`
	StorageLayer  string      `json:"storage_layer,omitempty"`
	Conflict      bool        `json:"conflict,omitempty"`
}

func (m *Manifest) Normalize() {
	sort.Slice(m.Components, func(i, j int) bool { return m.Components[i].ID < m.Components[j].ID })
	sort.Slice(m.Files, func(i, j int) bool {
		if m.Files[i].Component != m.Files[j].Component {
			return m.Files[i].Component < m.Files[j].Component
		}
		return m.Files[i].Path < m.Files[j].Path
	})
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported manifest schema version %d", m.SchemaVersion)
	}
	if strings.TrimSpace(m.BackupID) == "" {
		return fmt.Errorf("backup_id is missing")
	}
	if m.CreatedAt.IsZero() {
		return fmt.Errorf("manifest created_at is missing")
	}
	if err := m.Mode.Validate(); err != nil {
		return err
	}
	known := DefinitionsByID()
	componentSeen := make(map[ComponentID]bool, len(m.Components))
	for _, component := range m.Components {
		definition, ok := known[component.ID]
		if !ok {
			return fmt.Errorf("unknown component %q", component.ID)
		}
		if componentSeen[component.ID] {
			return fmt.Errorf("duplicate component %q", component.ID)
		}
		componentSeen[component.ID] = true
		if component.SchemaVersion != definition.SchemaVersion {
			return fmt.Errorf("unsupported schema version %d for component %s", component.SchemaVersion, component.ID)
		}
		if component.FileCount < 0 || component.ExpandedSize < 0 {
			return fmt.Errorf("invalid summary for component %s", component.ID)
		}
		if !validSHA256(component.SHA256) {
			return fmt.Errorf("invalid digest for component %s", component.ID)
		}
	}
	if m.Mode == ModePortable && componentSeen[ComponentDeviceIdentity] {
		return fmt.Errorf("portable backups must not contain device_identity")
	}
	fileSeen := make(map[string]bool, len(m.Files))
	componentCounts := make(map[ComponentID]int)
	componentSizes := make(map[ComponentID]int64)
	componentFiles := make(map[ComponentID][]FileManifest)
	for _, file := range m.Files {
		if !componentSeen[file.Component] {
			return fmt.Errorf("file %s belongs to undeclared component %s", file.Path, file.Component)
		}
		if err := validateArchivePath(file.Path); err != nil {
			return fmt.Errorf("component %s path: %w", file.Component, err)
		}
		key := string(file.Component) + "\x00" + file.Path
		if fileSeen[key] {
			return fmt.Errorf("duplicate archive entry for %s/%s", file.Component, file.Path)
		}
		fileSeen[key] = true
		if !validMode(file.Mode) {
			return fmt.Errorf("invalid mode %q for %s/%s", file.Mode, file.Component, file.Path)
		}
		switch file.Type {
		case FileTypeRegular:
			if file.Size < 0 || !validSHA256(file.SHA256) || file.LinkTarget != "" {
				return fmt.Errorf("invalid regular file metadata for %s/%s", file.Component, file.Path)
			}
			componentSizes[file.Component] += file.Size
		case FileTypeDirectory:
			if file.Size != 0 || file.SHA256 != "" || file.LinkTarget != "" {
				return fmt.Errorf("invalid directory metadata for %s/%s", file.Component, file.Path)
			}
		case FileTypeSymlink:
			if file.Size != 0 || file.SHA256 != "" || !validRelativeSymlink(file.Path, file.LinkTarget) {
				return fmt.Errorf("invalid symlink metadata for %s/%s", file.Component, file.Path)
			}
		default:
			return fmt.Errorf("unsupported file type %q", file.Type)
		}
		componentCounts[file.Component]++
		componentFiles[file.Component] = append(componentFiles[file.Component], file)
	}
	for _, component := range m.Components {
		if component.FileCount != componentCounts[component.ID] {
			return fmt.Errorf("component %s file_count mismatch", component.ID)
		}
		if component.ExpandedSize != componentSizes[component.ID] {
			return fmt.Errorf("component %s expanded_size mismatch", component.ID)
		}
		if component.SHA256 != componentDigest(componentFiles[component.ID]) {
			return fmt.Errorf("component %s digest mismatch", component.ID)
		}
	}
	return nil
}

func componentDigest(files []FileManifest) string {
	files = append([]FileManifest(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	hash := sha256.New()
	for _, file := range files {
		encoded, _ := json.Marshal(file)
		_, _ = hash.Write(encoded)
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (m Manifest) MarshalCanonical() ([]byte, error) {
	m.Normalize()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(m, "", "  ")
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func validMode(value string) bool {
	if len(value) != 4 {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '7' {
			return false
		}
	}
	mode := 0
	_, _ = fmt.Sscanf(value, "%o", &mode)
	return mode&0o6000 == 0
}

package agent

import (
	"aico-agent/internal/logging"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var manifestMu sync.Mutex
var skillFileMu sync.Mutex

type SkillSyncOptions struct {
	ConfigDir        string
	BundledSkillsDir string
	MergeModel       SkillMergeModel
	Quiet            bool
}

type SkillSyncReport struct {
	Copied        []string
	Updated       []string
	KeptUser      []string
	DeletedByUser []string
	Stale         []string
	MergeNeeded   []SkillMergeJob
}

type BundledManifest struct {
	Version   int                      `json:"version"`
	UpdatedAt string                   `json:"updated_at"`
	Skills    map[string]ManifestEntry `json:"skills"`
}

type ManifestEntry struct {
	OriginHash         string `json:"origin_hash"`
	EffectiveHash      string `json:"effective_hash,omitempty"`
	Status             string `json:"status"`
	BasePath           string `json:"base_path,omitempty"`
	LastSyncedAt       string `json:"last_synced_at,omitempty"`
	LastMergedAt       string `json:"last_merged_at,omitempty"`
	LastMergedKey      string `json:"last_merged_key,omitempty"`
	LastMergeFailedAt  string `json:"last_merge_failed_at,omitempty"`
	LastFailedMergeKey string `json:"last_failed_merge_key,omitempty"`
	LastMergeError     string `json:"last_merge_error,omitempty"`
}

const (
	StatusSynced        = "synced"
	StatusUserModified  = "user_modified"
	StatusMerged        = "merged"
	StatusDeletedByUser = "deleted_by_user"
)

func SyncBundledSkills(ctx context.Context, opts SkillSyncOptions) (*SkillSyncReport, error) {
	if opts.BundledSkillsDir == "" || opts.ConfigDir == "" {
		return &SkillSyncReport{}, nil
	}

	userSkillsDir := filepath.Join(opts.ConfigDir, "skills")
	stateDir := filepath.Join(opts.ConfigDir, "skill-state")
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	if err := os.MkdirAll(userSkillsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create user skills dir: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create skill-state dir: %w", err)
	}

	manifest := loadManifest(manifestPath)
	bundled := discoverBundledSkills(opts.BundledSkillsDir)
	report := &SkillSyncReport{}

	for name, bundledPath := range bundled {
		syncOneSkill(name, bundledPath, userSkillsDir, stateDir, manifest, report)
	}

	cleanStaleManifestEntries(manifest, bundled, report)
	manifest.UpdatedAt = time.Now().Format(time.RFC3339)
	saveManifest(manifestPath, manifest)

	if !opts.Quiet {
		logSyncReport(report)
	}
	return report, nil
}

func syncOneSkill(name, bundledPath, userSkillsDir, stateDir string, manifest *BundledManifest, report *SkillSyncReport) {
	userDir := filepath.Join(userSkillsDir, name)
	userPath := filepath.Join(userDir, "SKILL.md")
	bundledContent, err := os.ReadFile(bundledPath)
	if err != nil {
		logging.Errorf("agent", "skill_sync", "read bundled %s: %v", name, err)
		return
	}
	bundledHash := hashContent(bundledContent)

	entry, hasManifest := manifest.Skills[name]
	userExists := fileExists(userPath)

	switch {
	case hasManifest && entry.Status == StatusDeletedByUser:
		report.DeletedByUser = append(report.DeletedByUser, name)

	case !userExists && !hasManifest:
		copyBundledToUser(name, bundledContent, userDir, userPath, bundledHash, stateDir, manifest, report)

	case !userExists && hasManifest:
		report.DeletedByUser = append(report.DeletedByUser, name)
		manifest.Skills[name] = ManifestEntry{
			OriginHash:         entry.OriginHash,
			EffectiveHash:      entry.EffectiveHash,
			Status:             StatusDeletedByUser,
			BasePath:           entry.BasePath,
			LastSyncedAt:       entry.LastSyncedAt,
			LastMergedAt:       entry.LastMergedAt,
			LastMergedKey:      entry.LastMergedKey,
			LastMergeFailedAt:  entry.LastMergeFailedAt,
			LastFailedMergeKey: entry.LastFailedMergeKey,
			LastMergeError:     entry.LastMergeError,
		}

	case userExists && !hasManifest:
		// §9.7: same name but no manifest — need two-way merge
		userContent, err := os.ReadFile(userPath)
		if err != nil {
			if os.IsNotExist(err) {
				report.KeptUser = append(report.KeptUser, name)
				return
			}
			logging.Errorf("agent", "skill_sync", "read user %s for two-way merge: %v", name, err)
			return
		}
		if len(userContent) == 0 {
			report.KeptUser = append(report.KeptUser, name)
			return
		}
		localHash := hashContent(userContent)
		mergeKey := computeMergeKey(MergeTwoWay, name, "", bundledHash, localHash)
		report.MergeNeeded = append(report.MergeNeeded, SkillMergeJob{
			SkillName:    name,
			Mode:         MergeTwoWay,
			Upstream:     string(bundledContent),
			Local:        string(userContent),
			LocalHash:    localHash,
			UpstreamHash: bundledHash,
			MergeKey:     mergeKey,
			UserPath:     userPath,
			BasePath:     basePathForSkill(stateDir, name),
			StateDir:     stateDir,
		})

	case userExists && hasManifest:
		syncExistingSkill(name, userPath, bundledContent, bundledHash, entry, stateDir, manifest, report)
	}
}

func syncExistingSkill(name, userPath string, bundledContent []byte, bundledHash string, entry ManifestEntry, stateDir string, manifest *BundledManifest, report *SkillSyncReport) {
	userContent, err := os.ReadFile(userPath)
	if err != nil {
		logging.Errorf("agent", "skill_sync", "read user %s: %v", name, err)
		return
	}
	userHash := hashContent(userContent)

	effectiveHash := entry.EffectiveHash
	if effectiveHash == "" && entry.OriginHash != "" {
		effectiveHash = entry.OriginHash
	}

	if entry.OriginHash == "" {
		queueTwoWayMergeForConflict(name, userPath, userContent, bundledContent, bundledHash, entry, stateDir, manifest, report)
		return
	}

	userMatchesOrigin := userHash == entry.OriginHash
	userMatchesEffective := effectiveHash != "" && userHash == effectiveHash
	localDiffersFromBaseline := userHash != entry.OriginHash
	bundledUpdated := bundledHash != entry.OriginHash
	basePath := basePathForSkill(stateDir, name)

	switch {
	case userMatchesEffective && !bundledUpdated:
		entry.EffectiveHash = effectiveHash
		manifest.Skills[name] = entry
	case userMatchesOrigin && bundledUpdated:
		if err := os.WriteFile(userPath, bundledContent, 0o644); err != nil {
			logging.Errorf("agent", "skill_sync", "update %s: %v", name, err)
			return
		}
		saveBase(basePath, bundledContent)
		manifest.Skills[name] = ManifestEntry{
			OriginHash:    bundledHash,
			EffectiveHash: bundledHash,
			Status:        StatusSynced,
			BasePath:      basePath,
			LastSyncedAt:  time.Now().Format(time.RFC3339),
		}
		report.Updated = append(report.Updated, name)
	case localDiffersFromBaseline && !bundledUpdated:
		manifest.Skills[name] = ManifestEntry{
			OriginHash:         entry.OriginHash,
			EffectiveHash:      effectiveHash,
			Status:             StatusUserModified,
			BasePath:           entry.BasePath,
			LastSyncedAt:       entry.LastSyncedAt,
			LastMergedAt:       entry.LastMergedAt,
			LastMergedKey:      entry.LastMergedKey,
			LastMergeFailedAt:  entry.LastMergeFailedAt,
			LastFailedMergeKey: entry.LastFailedMergeKey,
			LastMergeError:     entry.LastMergeError,
		}
		report.KeptUser = append(report.KeptUser, name)
	case localDiffersFromBaseline && bundledUpdated:
		baseContent := ""
		if entry.BasePath != "" {
			if data, err := os.ReadFile(entry.BasePath); err == nil {
				baseContent = string(data)
			}
		}
		mode := MergeThreeWay
		baseHash := entry.OriginHash
		if baseContent == "" {
			mode = MergeTwoWay
			baseHash = ""
		}
		mergeKey := computeMergeKey(mode, name, baseHash, bundledHash, userHash)
		if entry.LastFailedMergeKey == mergeKey {
			report.KeptUser = append(report.KeptUser, name)
		} else {
			report.MergeNeeded = append(report.MergeNeeded, SkillMergeJob{
				SkillName:    name,
				Mode:         mode,
				Base:         baseContent,
				Upstream:     string(bundledContent),
				Local:        string(userContent),
				LocalHash:    userHash,
				UpstreamHash: bundledHash,
				BaseHash:     baseHash,
				MergeKey:     mergeKey,
				UserPath:     userPath,
				BasePath:     basePath,
				StateDir:     stateDir,
			})
		}
		manifest.Skills[name] = ManifestEntry{
			OriginHash:         entry.OriginHash,
			EffectiveHash:      effectiveHash,
			Status:             StatusUserModified,
			BasePath:           entry.BasePath,
			LastSyncedAt:       entry.LastSyncedAt,
			LastMergedAt:       entry.LastMergedAt,
			LastMergedKey:      entry.LastMergedKey,
			LastMergeFailedAt:  entry.LastMergeFailedAt,
			LastFailedMergeKey: entry.LastFailedMergeKey,
			LastMergeError:     entry.LastMergeError,
		}
	}
}

func queueTwoWayMergeForConflict(name, userPath string, userContent, bundledContent []byte, bundledHash string, entry ManifestEntry, stateDir string, manifest *BundledManifest, report *SkillSyncReport) {
	localHash := hashContent(userContent)
	mergeKey := computeMergeKey(MergeTwoWay, name, "", bundledHash, localHash)
	if entry.LastFailedMergeKey == mergeKey {
		report.KeptUser = append(report.KeptUser, name)
	} else {
		report.MergeNeeded = append(report.MergeNeeded, SkillMergeJob{
			SkillName:    name,
			Mode:         MergeTwoWay,
			Upstream:     string(bundledContent),
			Local:        string(userContent),
			LocalHash:    localHash,
			UpstreamHash: bundledHash,
			MergeKey:     mergeKey,
			UserPath:     userPath,
			BasePath:     basePathForSkill(stateDir, name),
			StateDir:     stateDir,
		})
	}
	entry.Status = StatusUserModified
	manifest.Skills[name] = entry
}

func copyBundledToUser(name string, content []byte, userDir, userPath, bundledHash, stateDir string, manifest *BundledManifest, report *SkillSyncReport) {
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		logging.Errorf("agent", "skill_sync", "mkdir %s: %v", userDir, err)
		return
	}
	if err := os.WriteFile(userPath, content, 0o644); err != nil {
		logging.Errorf("agent", "skill_sync", "write %s: %v", userPath, err)
		return
	}
	basePath := basePathForSkill(stateDir, name)
	saveBase(basePath, content)
	manifest.Skills[name] = ManifestEntry{
		OriginHash:    bundledHash,
		EffectiveHash: bundledHash,
		Status:        StatusSynced,
		BasePath:      basePath,
		LastSyncedAt:  time.Now().Format(time.RFC3339),
	}
	report.Copied = append(report.Copied, name)
}

func discoverBundledSkills(bundledDir string) map[string]string {
	result := make(map[string]string)
	entries, err := os.ReadDir(bundledDir)
	if err != nil {
		return result
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillPath := filepath.Join(bundledDir, e.Name(), "SKILL.md")
		if fileExists(skillPath) {
			result[e.Name()] = skillPath
		}
	}
	return result
}

func cleanStaleManifestEntries(manifest *BundledManifest, bundled map[string]string, report *SkillSyncReport) {
	for name := range manifest.Skills {
		if _, ok := bundled[name]; !ok {
			report.Stale = append(report.Stale, name)
			delete(manifest.Skills, name)
		}
	}
}

func loadManifest(path string) *BundledManifest {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	m := &BundledManifest{Version: 1, Skills: make(map[string]ManifestEntry)}
	data, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if err := json.Unmarshal(data, m); err != nil {
		return &BundledManifest{Version: 1, Skills: make(map[string]ManifestEntry)}
	}
	if m.Skills == nil {
		m.Skills = make(map[string]ManifestEntry)
	}
	for name, entry := range m.Skills {
		if entry.EffectiveHash == "" && entry.OriginHash != "" {
			entry.EffectiveHash = entry.OriginHash
			m.Skills[name] = entry
		}
	}
	return m
}

func saveManifest(path string, m *BundledManifest) {
	manifestMu.Lock()
	defer manifestMu.Unlock()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		logging.Errorf("agent", "skill_sync", "marshal manifest: %v", err)
		return
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		logging.Errorf("agent", "skill_sync", "write manifest: %v", err)
	}
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func logSyncReport(report *SkillSyncReport) {
	if len(report.Copied) > 0 {
		logging.Infof("agent", "skill_sync", "copied %d bundled skill(s): %s", len(report.Copied), strings.Join(report.Copied, ", "))
	}
	if len(report.Updated) > 0 {
		logging.Infof("agent", "skill_sync", "updated %d skill(s): %s", len(report.Updated), strings.Join(report.Updated, ", "))
	}
	if len(report.KeptUser) > 0 {
		logging.Infof("agent", "skill_sync", "kept user version for %d skill(s): %s", len(report.KeptUser), strings.Join(report.KeptUser, ", "))
	}
}

func basePathForSkill(stateDir, name string) string {
	return filepath.Join(stateDir, "bases", name, "SKILL.md")
}

func saveBase(basePath string, content []byte) error {
	if err := writeFileAtomic(basePath, content, 0o644); err != nil {
		logging.Errorf("agent", "skill_sync", "write base: %v", err)
		return err
	}
	return nil
}

func RestoreBundledSkill(configDir, bundledSkillsDir, name string) error {
	bundledPath := filepath.Join(bundledSkillsDir, name, "SKILL.md")
	content, err := os.ReadFile(bundledPath)
	if err != nil {
		return fmt.Errorf("read bundled skill %q: %w", name, err)
	}

	userPath := filepath.Join(configDir, "skills", name, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userPath), 0o755); err != nil {
		return fmt.Errorf("create skill dir: %w", err)
	}
	if err := os.WriteFile(userPath, content, 0o644); err != nil {
		return fmt.Errorf("write user skill: %w", err)
	}

	stateDir := filepath.Join(configDir, "skill-state")
	basePath := basePathForSkill(stateDir, name)
	saveBase(basePath, content)

	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")
	manifest := loadManifest(manifestPath)
	manifest.Skills[name] = ManifestEntry{
		OriginHash:    hashContent(content),
		EffectiveHash: hashContent(content),
		Status:        StatusSynced,
		BasePath:      basePath,
		LastSyncedAt:  time.Now().Format(time.RFC3339),
	}
	manifest.UpdatedAt = time.Now().Format(time.RFC3339)
	saveManifest(manifestPath, manifest)
	return nil
}

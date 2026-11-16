package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeSKILL(t *testing.T, dir, name, content string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSKILL(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

const testSkillA = `---
name: alpha
description: Alpha skill
---

Do alpha things.
`

const testSkillAv2 = `---
name: alpha
description: Alpha skill v2
---

Do alpha things better.
`

const testSkillB = `---
name: beta
description: Beta skill
---

Do beta things.
`

func TestSyncBundledSkills_NewSkillCopied(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	report, err := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir:        configDir,
		BundledSkillsDir: bundledDir,
		Quiet:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Copied) != 1 || report.Copied[0] != "alpha" {
		t.Fatalf("expected copied=[alpha], got %v", report.Copied)
	}

	got := readSKILL(t, filepath.Join(configDir, "skills"), "alpha")
	if got != testSkillA {
		t.Fatalf("user skill content mismatch")
	}

	manifest := loadManifest(filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	entry := manifest.Skills["alpha"]
	if entry.Status != StatusSynced {
		t.Fatalf("expected status synced, got %s", entry.Status)
	}
	if entry.OriginHash == "" {
		t.Fatal("expected non-empty origin_hash")
	}
	if entry.EffectiveHash != entry.OriginHash {
		t.Fatalf("expected effective_hash to match origin_hash, got %q vs %q", entry.EffectiveHash, entry.OriginHash)
	}
}

func TestSaveManifestCreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", ".bundled_manifest.json")
	saveManifest(path, &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "sha256:test", Status: StatusSynced},
	}})
	manifest := loadManifest(path)
	if manifest.Skills["alpha"].OriginHash != "sha256:test" {
		t.Fatalf("expected manifest written through missing parent dirs, got %+v", manifest.Skills["alpha"])
	}
}

func TestSyncBundledSkills_UpdateUnmodified(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	// Bundled updates
	writeSKILL(t, bundledDir, "alpha", testSkillAv2)
	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	if len(report.Updated) != 1 {
		t.Fatalf("expected updated=[alpha], got %v", report.Updated)
	}
	got := readSKILL(t, filepath.Join(configDir, "skills"), "alpha")
	if got != testSkillAv2 {
		t.Fatalf("expected v2 content")
	}

	manifest := loadManifest(filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	entry := manifest.Skills["alpha"]
	if entry.EffectiveHash != entry.OriginHash {
		t.Fatalf("expected effective_hash to match updated origin_hash, got %q vs %q", entry.EffectiveHash, entry.OriginHash)
	}
}

func TestSyncBundledSkills_KeepsMergedEffectiveCopyWhenBundledUnchanged(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	mergedContent := "---\nname: alpha\ndescription: merged\n---\n\nMerged steps.\n"
	writeSKILL(t, filepath.Join(configDir, "skills"), "alpha", mergedContent)
	manifestPath := filepath.Join(configDir, "skill-state", ".bundled_manifest.json")
	manifest := loadManifest(manifestPath)
	entry := manifest.Skills["alpha"]
	entry.Status = StatusMerged
	entry.EffectiveHash = hashContent([]byte(mergedContent))
	manifest.Skills["alpha"] = entry
	saveManifest(manifestPath, manifest)

	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})
	if len(report.KeptUser) != 0 || len(report.MergeNeeded) != 0 {
		t.Fatalf("expected merged effective copy to be kept without user_modified handling, kept=%v merge=%v", report.KeptUser, report.MergeNeeded)
	}

	manifest = loadManifest(manifestPath)
	if manifest.Skills["alpha"].Status != StatusMerged {
		t.Fatalf("expected merged status to remain stable, got %s", manifest.Skills["alpha"].Status)
	}
	if got := readSKILL(t, filepath.Join(configDir, "skills"), "alpha"); got != mergedContent {
		t.Fatal("merged effective copy was modified")
	}
}

func TestSyncBundledSkills_TwoWayFallbackUsesEmptyBaseHash(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	userModified := "---\nname: alpha\ndescription: custom\n---\n\nCustom.\n"
	writeSKILL(t, filepath.Join(configDir, "skills"), "alpha", userModified)
	basePath := filepath.Join(configDir, "skill-state", "bases", "alpha", "SKILL.md")
	if err := os.Remove(basePath); err != nil {
		t.Fatal(err)
	}
	writeSKILL(t, bundledDir, "alpha", testSkillAv2)

	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})
	if len(report.MergeNeeded) != 1 {
		t.Fatalf("expected one merge job, got %v", report.MergeNeeded)
	}
	job := report.MergeNeeded[0]
	if job.Mode != MergeTwoWay {
		t.Fatalf("expected two-way fallback, got %s", job.Mode)
	}
	if job.BaseHash != "" {
		t.Fatalf("expected empty base hash for two-way fallback, got %q", job.BaseHash)
	}
	expectedKey := computeMergeKey(MergeTwoWay, "alpha", "", hashContent([]byte(testSkillAv2)), hashContent([]byte(userModified)))
	if job.MergeKey != expectedKey {
		t.Fatalf("expected merge key %s, got %s", expectedKey, job.MergeKey)
	}
}

func TestSyncBundledSkills_KeepUserModified(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	// User modifies the skill
	userModified := "---\nname: alpha\ndescription: My alpha\n---\n\nCustom.\n"
	writeSKILL(t, filepath.Join(configDir, "skills"), "alpha", userModified)

	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	if len(report.KeptUser) != 1 {
		t.Fatalf("expected kept_user=[alpha], got %v", report.KeptUser)
	}
	got := readSKILL(t, filepath.Join(configDir, "skills"), "alpha")
	if got != userModified {
		t.Fatalf("user modification was overwritten")
	}
}

func TestSyncBundledSkills_DeletedByUser(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	// User deletes the skill
	os.RemoveAll(filepath.Join(configDir, "skills", "alpha"))

	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	if len(report.DeletedByUser) != 1 {
		t.Fatalf("expected deleted_by_user=[alpha], got %v", report.DeletedByUser)
	}
	// Should not re-copy
	if fileExists(filepath.Join(configDir, "skills", "alpha", "SKILL.md")) {
		t.Fatal("deleted skill was re-copied")
	}
}

func TestSyncBundledSkills_StaleManifestCleaned(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)
	writeSKILL(t, bundledDir, "beta", testSkillB)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	// Remove beta from bundled (simulating firmware removing a skill)
	os.RemoveAll(filepath.Join(bundledDir, "beta"))

	report, _ := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	if len(report.Stale) != 1 || report.Stale[0] != "beta" {
		t.Fatalf("expected stale=[beta], got %v", report.Stale)
	}
	manifest := loadManifest(filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	if _, ok := manifest.Skills["beta"]; ok {
		t.Fatal("stale entry not cleaned from manifest")
	}
}

func TestSyncBundledSkills_BaseSavedOnCopy(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	basePath := filepath.Join(configDir, "skill-state", "bases", "alpha", "SKILL.md")
	data, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("base not saved: %v", err)
	}
	if string(data) != testSkillA {
		t.Fatal("base content mismatch")
	}

	manifest := loadManifest(filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	if manifest.Skills["alpha"].BasePath == "" {
		t.Fatal("manifest missing base_path")
	}
}

func TestSyncBundledSkills_BaseUpdatedOnAutoUpdate(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	writeSKILL(t, bundledDir, "alpha", testSkillAv2)
	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	basePath := filepath.Join(configDir, "skill-state", "bases", "alpha", "SKILL.md")
	data, err := os.ReadFile(basePath)
	if err != nil {
		t.Fatalf("base not updated: %v", err)
	}
	if string(data) != testSkillAv2 {
		t.Fatal("base should contain v2 after auto-update")
	}
}

func TestSyncBundledSkills_TwoWayReadErrorNotKeptUser(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)
	blockedPath := filepath.Join(configDir, "skills", "alpha", "SKILL.md")
	if err := os.MkdirAll(blockedPath, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir:        configDir,
		BundledSkillsDir: bundledDir,
		Quiet:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.KeptUser) != 0 {
		t.Fatalf("expected read error not to be treated as kept user, got %v", report.KeptUser)
	}
	if len(report.MergeNeeded) != 0 {
		t.Fatalf("expected read error not to queue merge, got %v", report.MergeNeeded)
	}
}

func TestRestoreBundledSkill(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})

	// User modifies
	userModified := "---\nname: alpha\ndescription: custom\n---\n\nCustom.\n"
	writeSKILL(t, filepath.Join(configDir, "skills"), "alpha", userModified)

	// Restore
	if err := RestoreBundledSkill(configDir, bundledDir, "alpha"); err != nil {
		t.Fatal(err)
	}

	got := readSKILL(t, filepath.Join(configDir, "skills"), "alpha")
	if got != testSkillA {
		t.Fatal("restore did not reset to bundled version")
	}

	manifest := loadManifest(filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	if manifest.Skills["alpha"].Status != StatusSynced {
		t.Fatalf("expected synced after restore, got %s", manifest.Skills["alpha"].Status)
	}
	if manifest.Skills["alpha"].EffectiveHash != manifest.Skills["alpha"].OriginHash {
		t.Fatalf("expected restore effective_hash to match origin_hash")
	}
}

func TestSyncBundledSkills_SupportFiles(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	userDir := filepath.Join(configDir, "skills", "alpha")
	writeFile := func(dir, rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	readUser := func(rel string) (string, bool) {
		data, err := os.ReadFile(filepath.Join(userDir, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			return "", false
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(data), true
	}
	sync := func() {
		t.Helper()
		if _, err := SyncBundledSkills(context.Background(), SkillSyncOptions{ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true}); err != nil {
			t.Fatal(err)
		}
	}

	writeSKILL(t, bundledDir, "alpha", testSkillA)
	bundledSkill := filepath.Join(bundledDir, "alpha")
	writeFile(bundledSkill, "references/guide.md", "guide v1")
	writeFile(bundledSkill, "scripts/tool.py", "tool v1")
	writeFile(bundledSkill, "references/dropped.md", "dropped v1")
	writeFile(bundledSkill, "references/edited.md", "edited v1")
	writeFile(bundledSkill, "references/deleted.md", "deleted v1")
	sync()
	for rel, want := range map[string]string{"references/guide.md": "guide v1", "scripts/tool.py": "tool v1", "references/dropped.md": "dropped v1"} {
		if got, _ := readUser(rel); got != want {
			t.Fatalf("first sync %s = %q, want %q", rel, got, want)
		}
	}

	writeFile(userDir, "references/edited.md", "user edit")
	if err := os.Remove(filepath.Join(userDir, "references", "deleted.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(bundledSkill, "references/guide.md", "guide v2")
	writeFile(bundledSkill, "references/edited.md", "edited v2")
	writeFile(bundledSkill, "references/deleted.md", "deleted v2")
	writeFile(bundledSkill, "assets/new.txt", "new v1")
	if err := os.Remove(filepath.Join(bundledSkill, "references", "dropped.md")); err != nil {
		t.Fatal(err)
	}
	sync()

	cases := []struct {
		rel    string
		want   string
		exists bool
	}{
		{"references/guide.md", "guide v2", true},
		{"scripts/tool.py", "tool v1", true},
		{"assets/new.txt", "new v1", true},
		{"references/edited.md", "user edit", true},
		{"references/deleted.md", "", false},
		{"references/dropped.md", "", false},
	}
	for _, tc := range cases {
		got, exists := readUser(tc.rel)
		if exists != tc.exists || got != tc.want {
			t.Fatalf("second sync %s = %q (exists=%t), want %q (exists=%t)", tc.rel, got, exists, tc.want, tc.exists)
		}
	}

	if err := RestoreBundledSkill(configDir, bundledDir, "alpha"); err != nil {
		t.Fatal(err)
	}
	if got, _ := readUser("references/edited.md"); got != "edited v2" {
		t.Fatalf("restore edited.md = %q, want bundled copy", got)
	}
	if got, _ := readUser("references/deleted.md"); got != "deleted v2" {
		t.Fatalf("restore deleted.md = %q, want bundled copy", got)
	}
}

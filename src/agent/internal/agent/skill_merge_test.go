package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type mockMergeModel struct {
	result *SkillMergeResult
	err    error
}

func (m *mockMergeModel) MergeSkill(_ context.Context, _ SkillMergeInput) (*SkillMergeResult, error) {
	return m.result, m.err
}

func TestMergeWorker_SuccessfulMerge(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "skill-state")
	os.MkdirAll(stateDir, 0o755)
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	userSkillDir := filepath.Join(configDir, "skills", "alpha")
	os.MkdirAll(userSkillDir, 0o755)
	localContent := "---\nname: alpha\ndescription: user version\n---\n\nUser steps.\n"
	os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(localContent), 0o644)

	mergedContent := "---\nname: alpha\ndescription: merged version\n---\n\nMerged steps.\n"
	model := &mockMergeModel{
		result: &SkillMergeResult{
			Status:        "merged",
			MergedSkillMD: mergedContent,
			Summary:       "merged ok",
		},
	}

	upstreamContent := "---\nname: alpha\ndescription: upstream\n---\n\nUpstream.\n"
	job := SkillMergeJob{
		SkillName:    "alpha",
		Mode:         MergeThreeWay,
		Base:         localContent,
		Upstream:     upstreamContent,
		Local:        localContent,
		LocalHash:    hashContent([]byte(localContent)),
		UpstreamHash: hashContent([]byte(upstreamContent)),
		MergeKey:     "test-key",
		UserPath:     filepath.Join(userSkillDir, "SKILL.md"),
		BasePath:     filepath.Join(stateDir, "bases", "alpha", "SKILL.md"),
		StateDir:     stateDir,
	}

	// Save initial manifest
	manifest := &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "old", Status: StatusUserModified},
	}}
	saveManifest(manifestPath, manifest)

	worker := NewMergeWorker(model, manifestPath)
	worker.Enqueue([]SkillMergeJob{job})
	worker.Start(context.Background())
	worker.Wait()

	got, err := os.ReadFile(filepath.Join(userSkillDir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != mergedContent {
		t.Fatalf("expected merged content, got %q", string(got))
	}

	m := loadManifest(manifestPath)
	if m.Skills["alpha"].Status != StatusMerged {
		t.Fatalf("expected merged status, got %s", m.Skills["alpha"].Status)
	}
	if m.Skills["alpha"].EffectiveHash != hashContent([]byte(mergedContent)) {
		t.Fatalf("expected effective_hash for merged content, got %s", m.Skills["alpha"].EffectiveHash)
	}
	if m.Skills["alpha"].LastMergedKey != "test-key" {
		t.Fatalf("expected last_merged_key=test-key, got %s", m.Skills["alpha"].LastMergedKey)
	}
	if m.Skills["alpha"].LastFailedMergeKey != "" {
		t.Fatalf("expected last_failed_merge_key cleared, got %s", m.Skills["alpha"].LastFailedMergeKey)
	}
}

func TestMergeWorker_CallsSuccessCallback(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "skill-state")
	os.MkdirAll(stateDir, 0o755)
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	userSkillDir := filepath.Join(configDir, "skills", "alpha")
	os.MkdirAll(userSkillDir, 0o755)
	localContent := "---\nname: alpha\ndescription: user version\n---\n\nUser steps.\n"
	os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(localContent), 0o644)

	mergedContent := "---\nname: alpha\ndescription: merged version\n---\n\nMerged steps.\n"
	upstreamContent := "---\nname: alpha\ndescription: upstream\n---\n\nUpstream.\n"
	job := SkillMergeJob{
		SkillName:    "alpha",
		Mode:         MergeThreeWay,
		Upstream:     upstreamContent,
		Local:        localContent,
		LocalHash:    hashContent([]byte(localContent)),
		UpstreamHash: hashContent([]byte(upstreamContent)),
		MergeKey:     "callback-key",
		UserPath:     filepath.Join(userSkillDir, "SKILL.md"),
		BasePath:     filepath.Join(stateDir, "bases", "alpha", "SKILL.md"),
		StateDir:     stateDir,
	}
	saveManifest(manifestPath, &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "old", Status: StatusUserModified},
	}})

	called := false
	worker := NewMergeWorker(&mockMergeModel{result: &SkillMergeResult{Status: "merged", MergedSkillMD: mergedContent}}, manifestPath)
	worker.onSuccess = func(got SkillMergeJob) {
		called = true
		if got.SkillName != "alpha" {
			t.Fatalf("unexpected callback job %q", got.SkillName)
		}
	}
	worker.Enqueue([]SkillMergeJob{job})
	worker.Start(context.Background())
	worker.Wait()

	if !called {
		t.Fatal("expected merge success callback to run")
	}
}

func TestMergeWorker_BaseWriteFailureRecordsFailure(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "skill-state")
	os.MkdirAll(stateDir, 0o755)
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	userSkillDir := filepath.Join(configDir, "skills", "alpha")
	os.MkdirAll(userSkillDir, 0o755)
	localContent := "---\nname: alpha\ndescription: user version\n---\n\nUser steps.\n"
	os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(localContent), 0o644)

	mergedContent := "---\nname: alpha\ndescription: merged version\n---\n\nMerged steps.\n"
	upstreamContent := "---\nname: alpha\ndescription: upstream\n---\n\nUpstream.\n"
	blockedBasePath := filepath.Join(stateDir, "base-blocker")
	if err := os.WriteFile(blockedBasePath, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := SkillMergeJob{
		SkillName:    "alpha",
		Mode:         MergeThreeWay,
		Upstream:     upstreamContent,
		Local:        localContent,
		LocalHash:    hashContent([]byte(localContent)),
		UpstreamHash: hashContent([]byte(upstreamContent)),
		MergeKey:     "base-fail-key",
		UserPath:     filepath.Join(userSkillDir, "SKILL.md"),
		BasePath:     filepath.Join(blockedBasePath, "alpha", "SKILL.md"),
		StateDir:     stateDir,
	}
	saveManifest(manifestPath, &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "old", Status: StatusUserModified},
	}})

	worker := NewMergeWorker(&mockMergeModel{result: &SkillMergeResult{Status: "merged", MergedSkillMD: mergedContent}}, manifestPath)
	worker.Enqueue([]SkillMergeJob{job})
	worker.Start(context.Background())
	worker.Wait()

	manifest := loadManifest(manifestPath)
	entry := manifest.Skills["alpha"]
	if entry.Status == StatusMerged {
		t.Fatalf("expected base write failure not to record merged status")
	}
	if entry.LastFailedMergeKey != "base-fail-key" {
		t.Fatalf("expected base write failure to record last_failed_merge_key, got %q", entry.LastFailedMergeKey)
	}
}

func TestMergeWorker_FailedMerge(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "skill-state")
	os.MkdirAll(stateDir, 0o755)
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	userSkillDir := filepath.Join(configDir, "skills", "alpha")
	os.MkdirAll(userSkillDir, 0o755)
	localContent := "---\nname: alpha\ndescription: user\n---\n\nUser.\n"
	os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(localContent), 0o644)

	model := &mockMergeModel{
		result: &SkillMergeResult{Status: "failed", Summary: "conflict"},
	}

	manifest := &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "old", Status: StatusUserModified},
	}}
	saveManifest(manifestPath, manifest)

	job := SkillMergeJob{
		SkillName: "alpha",
		Mode:      MergeThreeWay,
		Local:     localContent,
		LocalHash: hashContent([]byte(localContent)),
		MergeKey:  "fail-key",
		UserPath:  filepath.Join(userSkillDir, "SKILL.md"),
		StateDir:  stateDir,
	}

	worker := NewMergeWorker(model, manifestPath)
	worker.Enqueue([]SkillMergeJob{job})
	worker.Start(context.Background())
	worker.Wait()

	// User file should be unchanged
	got, _ := os.ReadFile(filepath.Join(userSkillDir, "SKILL.md"))
	if string(got) != localContent {
		t.Fatal("user file was modified on failed merge")
	}

	m := loadManifest(manifestPath)
	if m.Skills["alpha"].LastFailedMergeKey != "fail-key" {
		t.Fatalf("expected last_failed_merge_key=fail-key, got %s", m.Skills["alpha"].LastFailedMergeKey)
	}
}

func TestMergeWorker_StopBeforeStartReturns(t *testing.T) {
	worker := NewMergeWorker(&mockMergeModel{}, filepath.Join(t.TempDir(), "manifest.json"))
	done := make(chan struct{})
	go func() {
		worker.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Stop blocked before Start")
	}
}

func TestMergeResultOKRejectsUnknownAllowedTool(t *testing.T) {
	result := &SkillMergeResult{
		Status: "merged",
		MergedSkillMD: `---
name: alpha
description: Alpha
metadata:
  allowed_tools: [not_a_real_tool]
---

Do alpha.
`,
	}
	if mergeResultOK(result, "alpha") {
		t.Fatal("expected unknown allowed_tools entry to fail validation")
	}
}

func TestMergeResultOKAcceptsKnownAllowedTool(t *testing.T) {
	result := &SkillMergeResult{
		Status: "merged",
		MergedSkillMD: `---
name: alpha
description: Alpha
metadata:
  allowed_tools: [screenshot, skill_read]
---

Do alpha.
`,
	}
	if !mergeResultOK(result, "alpha") {
		t.Fatal("expected known allowed_tools entries to pass validation")
	}
}

func TestMergeResultOKAcceptsDelegateAllowedTool(t *testing.T) {
	result := &SkillMergeResult{
		Status: "merged",
		MergedSkillMD: `---
name: alpha
description: Alpha
metadata:
  allowed_tools: [shell, delegate_researcher]
---

Do alpha.
`,
	}
	if !mergeResultOK(result, "alpha") {
		t.Fatal("expected delegate_* allowed_tools entries to pass validation")
	}
}

func TestBundledSkillsReferenceKnownAllowedTools(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	skillsDir := filepath.Join(filepath.Dir(file), "..", "..", "config", "skills")
	index, err := LoadSkillsFromDirs([]string{skillsDir})
	if err != nil {
		t.Fatal(err)
	}
	for _, skill := range index.All() {
		if !registeredToolsExist(skill.AllowedTools) {
			t.Fatalf("bundled skill %q references unknown allowed_tools %v", skill.Name, skill.AllowedTools)
		}
	}
}

func TestMergeWorker_SkipsWhenLocalChanged(t *testing.T) {
	configDir := t.TempDir()
	stateDir := filepath.Join(configDir, "skill-state")
	os.MkdirAll(stateDir, 0o755)
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")

	userSkillDir := filepath.Join(configDir, "skills", "alpha")
	os.MkdirAll(userSkillDir, 0o755)
	originalContent := "---\nname: alpha\ndescription: original\n---\n\nOriginal.\n"
	os.WriteFile(filepath.Join(userSkillDir, "SKILL.md"), []byte(originalContent), 0o644)

	mergedContent := "---\nname: alpha\ndescription: merged\n---\n\nMerged.\n"
	model := &mockMergeModel{
		result: &SkillMergeResult{Status: "merged", MergedSkillMD: mergedContent},
	}

	// Use a WRONG local hash to simulate the file changing during merge
	job := SkillMergeJob{
		SkillName: "alpha",
		Mode:      MergeThreeWay,
		Local:     originalContent,
		LocalHash: "sha256:wrong-hash",
		MergeKey:  "stale-key",
		UserPath:  filepath.Join(userSkillDir, "SKILL.md"),
		StateDir:  stateDir,
	}

	manifest := &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {OriginHash: "old", Status: StatusUserModified},
	}}
	saveManifest(manifestPath, manifest)

	worker := NewMergeWorker(model, manifestPath)
	worker.Enqueue([]SkillMergeJob{job})
	worker.Start(context.Background())
	worker.Wait()

	got, _ := os.ReadFile(filepath.Join(userSkillDir, "SKILL.md"))
	if string(got) != originalContent {
		t.Fatal("file was overwritten despite local hash mismatch")
	}
	m := loadManifest(manifestPath)
	if m.Skills["alpha"].LastFailedMergeKey != "stale-key" {
		t.Fatalf("expected local change to record last_failed_merge_key=stale-key, got %s", m.Skills["alpha"].LastFailedMergeKey)
	}
}

func TestMergeWorker_TwoWayFailurePreventsSameInputRetry(t *testing.T) {
	configDir := t.TempDir()
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)
	userSkillsDir := filepath.Join(configDir, "skills")
	userContent := "---\nname: alpha\ndescription: user\n---\n\nUser local.\n"
	writeSKILL(t, userSkillsDir, "alpha", userContent)

	report, err := SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MergeNeeded) != 1 {
		t.Fatalf("expected initial two-way merge job, got %v", report.MergeNeeded)
	}

	stateDir := filepath.Join(configDir, "skill-state")
	manifestPath := filepath.Join(stateDir, ".bundled_manifest.json")
	worker := NewMergeWorker(&mockMergeModel{result: &SkillMergeResult{Status: "failed", Summary: "conflict"}}, manifestPath)
	worker.Enqueue(report.MergeNeeded)
	worker.Start(context.Background())
	worker.Wait()

	report, err = SyncBundledSkills(context.Background(), SkillSyncOptions{
		ConfigDir: configDir, BundledSkillsDir: bundledDir, Quiet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MergeNeeded) != 0 {
		t.Fatalf("expected same failed inputs not to retry, got %v", report.MergeNeeded)
	}
}

func TestSkillListTool(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	writeSKILL(t, dir, "beta", testSkillB)

	tool := NewSkillListTool(dir)
	result, err := tool.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alpha") || !strings.Contains(result, "beta") {
		t.Fatalf("expected both skills in list, got %s", result)
	}

	// Filter
	result, _ = tool.Call(context.Background(), "alpha")
	if !strings.Contains(result, "alpha") {
		t.Fatal("filter should return alpha")
	}
	if strings.Contains(result, "beta") {
		t.Fatal("filter should not return beta")
	}
}

func TestSkillListToolFiltersByDeviceType(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "generic", "---\nname: generic\ndescription: Generic skill\n---\n\nUse anywhere.\n")
	writeSKILL(t, dir, "android-only", "---\nname: android-only\ndescription: Android skill\nmetadata:\n  device_types: [Android]\n---\n\nUse on Android.\n")
	writeSKILL(t, dir, "ios-only", "---\nname: ios-only\ndescription: iOS skill\nmetadata:\n  device_types: [iOS]\n---\n\nUse on iOS.\n")

	tool := NewSkillListTool(dir)
	tool.SetDeviceTypeFunc(func() string { return "Android" })
	result, err := tool.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "generic") || !strings.Contains(result, "android-only") {
		t.Fatalf("expected generic and Android skills, got %s", result)
	}
	if strings.Contains(result, "ios-only") {
		t.Fatalf("expected iOS skill to be filtered, got %s", result)
	}

	result, err = tool.Call(context.Background(), `{"include_incompatible":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "ios-only") {
		t.Fatalf("expected include_incompatible to include iOS skill, got %s", result)
	}
}

func TestSkillListToolFiltersArchivedByDefault(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	writeSKILL(t, skillsDir, "beta", testSkillB)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"beta": {State: SkillUsageStateArchived},
	})

	tool := NewSkillListTool(skillsDir, usagePath)
	result, err := tool.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alpha") || strings.Contains(result, "beta") {
		t.Fatalf("expected default list to hide archived beta, got %s", result)
	}

	result, err = tool.Call(context.Background(), `{"include_archived":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alpha") || !strings.Contains(result, "beta") {
		t.Fatalf("expected include_archived list to include both skills, got %s", result)
	}

	result, err = tool.Call(context.Background(), `{"state":"archived","include_archived":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, "alpha") || !strings.Contains(result, "beta") {
		t.Fatalf("expected archived state filter to include only beta, got %s", result)
	}
}

func TestSkillMarkUsedToolUpdatesUsage(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)

	tool := NewSkillMarkUsedTool(skillsDir, usagePath)
	if _, err := tool.Call(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].UseCount != 1 || usage["alpha"].LastUsedAt == "" {
		t.Fatalf("expected use_count update, got %+v", usage["alpha"])
	}
}

func TestSkillMarkUsedToolRestoresArchivedSkill(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {State: SkillUsageStateArchived},
	})

	tool := NewSkillMarkUsedTool(skillsDir, usagePath)
	if _, err := tool.Call(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateActive || usage["alpha"].StateChangedAt == "" {
		t.Fatalf("expected use to restore archived skill to active, got %+v", usage["alpha"])
	}
}

func TestSkillListToolAutoMarksAgentSkillStale(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testAgentCreatedSkill("alpha"))
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {
			State:      SkillUsageStateActive,
			LastUsedAt: time.Now().Add(-91 * 24 * time.Hour).Format(time.RFC3339),
		},
	})

	tool := NewSkillListTool(skillsDir, usagePath)
	result, err := tool.Call(context.Background(), `{"state":"stale","include_archived":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alpha") || !strings.Contains(result, SkillUsageStateStale) {
		t.Fatalf("expected stale alpha in list, got %s", result)
	}
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateStale || usage["alpha"].StateChangedAt == "" {
		t.Fatalf("expected usage state stale, got %+v", usage["alpha"])
	}
}

func TestSkillListToolAutoArchivesOldAgentSkill(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testAgentCreatedSkill("alpha"))
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {
			State:      SkillUsageStateStale,
			LastUsedAt: time.Now().Add(-181 * 24 * time.Hour).Format(time.RFC3339),
		},
	})

	tool := NewSkillListTool(skillsDir, usagePath)
	defaultResult, err := tool.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(defaultResult, "alpha") {
		t.Fatalf("expected archived alpha to be hidden by default, got %s", defaultResult)
	}
	archivedResult, err := tool.Call(context.Background(), `{"state":"archived","include_archived":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archivedResult, "alpha") || !strings.Contains(archivedResult, SkillUsageStateArchived) {
		t.Fatalf("expected archived alpha in explicit list, got %s", archivedResult)
	}
}

func TestSkillListToolArchivesByLastActivityNotStateChange(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testAgentCreatedSkill("alpha"))
	now := time.Date(2026, 5, 28, 8, 0, 0, 0, time.UTC)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {
			State:          SkillUsageStateStale,
			LastUsedAt:     now.Add(-181 * 24 * time.Hour).Format(time.RFC3339),
			StateChangedAt: now.Add(-time.Hour).Format(time.RFC3339),
		},
	})

	applyAutomaticSkillLifecycle(skillsDir, usagePath, now)
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateArchived {
		t.Fatalf("expected archive based on last activity, got %+v", usage["alpha"])
	}
}

func TestAutomaticSkillLifecycleSkipsWhenRecentlyEvaluated(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testAgentCreatedSkill("alpha"))
	now := time.Date(2026, 5, 28, 8, 0, 0, 0, time.UTC)
	oldUsedAt := now.Add(-91 * 24 * time.Hour).Format(time.RFC3339)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {State: SkillUsageStateActive, LastUsedAt: oldUsedAt},
	})

	applyAutomaticSkillLifecycle(skillsDir, usagePath, now)
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateStale {
		t.Fatalf("expected first evaluation to mark stale, got %+v", usage["alpha"])
	}

	// Simulate a state reset shortly after the scan; the next skill_list should
	// not rescan all skills until the throttle window expires.
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {State: SkillUsageStateActive, LastUsedAt: oldUsedAt},
	})
	applyAutomaticSkillLifecycle(skillsDir, usagePath, now.Add(time.Hour))
	usage = loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateActive {
		t.Fatalf("expected lifecycle scan to be throttled, got %+v", usage["alpha"])
	}
}

func TestAutomaticSkillLifecycleRunsAfterThrottleWindow(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testAgentCreatedSkill("alpha"))
	now := time.Date(2026, 5, 28, 8, 0, 0, 0, time.UTC)
	oldUsedAt := now.Add(-91 * 24 * time.Hour).Format(time.RFC3339)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {State: SkillUsageStateActive, LastUsedAt: oldUsedAt},
	})

	applyAutomaticSkillLifecycle(skillsDir, usagePath, now)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {State: SkillUsageStateActive, LastUsedAt: oldUsedAt},
	})
	applyAutomaticSkillLifecycle(skillsDir, usagePath, now.Add(25*time.Hour))
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateStale {
		t.Fatalf("expected lifecycle scan after throttle window, got %+v", usage["alpha"])
	}
}

func TestSkillListToolAutoLifecycleIgnoresNonAgentSkill(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	saveSkillUsage(usagePath, map[string]SkillUsageEntry{
		"alpha": {
			State:      SkillUsageStateActive,
			LastUsedAt: time.Now().Add(-181 * 24 * time.Hour).Format(time.RFC3339),
		},
	})

	tool := NewSkillListTool(skillsDir, usagePath)
	result, err := tool.Call(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "alpha") || !strings.Contains(result, SkillUsageStateActive) {
		t.Fatalf("expected non-agent alpha to stay active and visible, got %s", result)
	}
}

func testAgentCreatedSkill(name string) string {
	return "---\nname: " + name + "\ndescription: Agent-created skill\nsource: agent\ncreated_by: agent\n---\n\nDo agent things.\n"
}

func TestSkillReadToolReadsLinkedFiles(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	refDir := filepath.Join(dir, "alpha", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "notes.md"), []byte("reference notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSkillReadTool(dir)

	mainContent, err := tool.Call(context.Background(), `{"name":"alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mainContent, "Linked files available") || !strings.Contains(mainContent, "references/notes.md") {
		t.Fatalf("expected main SKILL.md read to list linked files, got:\n%s", mainContent)
	}

	got, err := tool.Call(context.Background(), `{"name":"alpha","file_path":"references/notes.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != "reference notes" {
		t.Fatalf("unexpected linked file content: %q", got)
	}

	if _, err := tool.Call(context.Background(), `{"name":"alpha","file_path":"../secret"}`); err == nil {
		t.Fatalf("expected traversal path to be rejected")
	}
}

func TestSkillReadToolRejectsIncompatibleDeviceTypeByDefault(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "ios-only", "---\nname: ios-only\ndescription: iOS skill\nmetadata:\n  device_types: [iOS]\n---\n\nUse on iOS.\n")
	tool := NewSkillReadTool(dir)
	tool.SetDeviceTypeFunc(func() string { return "Android" })

	_, err := tool.Call(context.Background(), `{"name":"ios-only"}`)
	if err == nil {
		t.Fatal("expected incompatible skill_read to fail")
	}
	if !strings.Contains(err.Error(), "current device_type is Android") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := tool.Call(context.Background(), `{"name":"ios-only","include_incompatible":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Use on iOS.") {
		t.Fatalf("expected include_incompatible to read skill, got %s", got)
	}
}

func TestSkillReadToolRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	refDir := filepath.Join(dir, "alpha", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outsidePath := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(outsidePath, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(refDir, "secret.md")); err != nil {
		t.Fatal(err)
	}
	tool := NewSkillReadTool(dir)

	if _, err := tool.Call(context.Background(), `{"name":"alpha","file_path":"references/secret.md"}`); err == nil {
		t.Fatalf("expected symlink escape to be rejected")
	}
}

func TestSkillReadToolRejectsSkillDirectorySymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	externalDir := filepath.Join(dir, "external-alpha")
	if err := os.MkdirAll(externalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalDir, "SKILL.md"), []byte(testSkillA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalDir, filepath.Join(skillsDir, "alpha")); err != nil {
		t.Fatal(err)
	}
	tool := NewSkillReadTool(skillsDir)

	if _, err := tool.Call(context.Background(), `{"name":"alpha"}`); err == nil {
		t.Fatalf("expected skill directory symlink escape to be rejected")
	}
}

func TestSkillReadToolRejectsLargeAndNonUTF8Files(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	refDir := filepath.Join(dir, "alpha", "references")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", maxSkillReadBytes+1)
	if err := os.WriteFile(filepath.Join(refDir, "large.md"), []byte(large), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(refDir, "binary.md"), []byte{0xff, 0xfe, 0xfd}, 0o644); err != nil {
		t.Fatal(err)
	}
	tool := NewSkillReadTool(dir)
	if _, err := tool.Call(context.Background(), `{"name":"alpha","file_path":"references/large.md"}`); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("expected too large error, got %v", err)
	}
	if _, err := tool.Call(context.Background(), `{"name":"alpha","file_path":"references/binary.md"}`); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("expected UTF-8 error, got %v", err)
	}
}

// TestSkillManageActionsDocumentedInSchema verifies the per-action field
// contract (previously spelled out in the description) is carried by ArgsSchema,
// which is where the input shape now lives after the description was trimmed.
func TestSkillManageActionsDocumentedInSchema(t *testing.T) {
	schema := NewSkillManageTool(t.TempDir(), "").ArgsSchema()
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("skill_manage schema missing properties: %v", schema)
	}
	for _, field := range []string{"action", "name", "content", "old_string", "new_string", "file_path", "file_content", "source_url", "reason"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("skill_manage schema missing field %q: %v", field, props)
		}
	}
	actionEnum, _ := props["action"].(map[string]any)["enum"].([]string)
	wantActions := []string{
		"create", "edit", "patch", "install", "delete",
		"write_file", "remove_file", "mark_stale",
		"archive", "restore_archive",
	}
	if !slices.Equal(actionEnum, wantActions) {
		t.Fatalf("skill_manage action enum = %v, want %v", actionEnum, wantActions)
	}
}

func TestSkillManageToolEmptyInputExplainsJSONContract(t *testing.T) {
	_, err := NewSkillManageTool(t.TempDir(), "").Call(context.Background(), "")
	if err == nil {
		t.Fatal("expected empty skill_manage input to fail")
	}
	for _, want := range []string{
		"skill_manage input must be a JSON object",
		`{"action":"patch"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("empty input error missing %q: %v", want, err)
		}
	}
}

func TestBundledDeviceOperatorHasNoLegacyChildSkills(t *testing.T) {
	skillsDir := filepath.Join("..", "..", "config", "skills")
	for _, childName := range []string{"app-switching", "frame-service-recovery", "scroll-and-picker", "text-entry"} {
		if fileExists(filepath.Join(skillsDir, childName, "SKILL.md")) {
			t.Fatalf("bundled child skill %q should be folded into device-operator", childName)
		}
	}
}

func TestBundledDeviceOperatorAllowedToolsCoverEmbeddedPlaybooks(t *testing.T) {
	skillsDir := filepath.Join("..", "..", "config", "skills")
	index, err := LoadSkillsFromDirs([]string{skillsDir})
	if err != nil {
		t.Fatal(err)
	}
	device, ok := index.Get("device-operator")
	if !ok {
		t.Fatal("device-operator skill not found")
	}
	deviceTools := map[string]struct{}{}
	for _, tool := range device.AllowedTools {
		deviceTools[tool] = struct{}{}
	}
	for _, tool := range []string{
		"screenshot",
		"wait_for_stable_screen",
		"quick_action",
		"touch_gesture",
		"mouse_move",
		"mouse_scroll",
		"keyboard_tap",
		"enter_text",
		"open_app",
		"open_url",
		"request_user_action",
		"recall_memory",
		"save_memory",
		"shell",
	} {
		if _, ok := deviceTools[tool]; !ok {
			t.Fatalf("device-operator allowed_tools missing %q required by embedded playbooks", tool)
		}
	}
}

func TestSkillManageTool_CreateAndPatch(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")

	// Create
	content := "---\nname: foo\ndescription: Foo skill\n---\n\nDo foo.\n"
	input := `{"action":"create","name":"foo","content":"---\nname: foo\ndescription: Foo skill\n---\n\nDo foo.\n","reason":"test"}`
	result, err := tool.Call(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Created skill "foo"`,
		"bytes=50",
		"lines=6",
		"sha256=65b5d4a35a9ec5a724b0feaa9e02012daf3e8bf07fad80a598b2cb361f486b02",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("create result missing %q: %s", want, result)
		}
	}

	got, _ := os.ReadFile(filepath.Join(dir, "foo", "SKILL.md"))
	if string(got) != content {
		t.Fatalf("content mismatch: %q", string(got))
	}

	// Patch
	patchInput := `{"action":"patch","name":"foo","old_string":"Do foo.","new_string":"Do foo better.","reason":"improve"}`
	result, err = tool.Call(context.Background(), patchInput)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Patched skill "foo"`,
		"previous_bytes=50",
		"changed_lines=6-6",
		"bytes=57",
		"lines=6",
		"sha256=5a5b4520ed0300abcdf82f1433834636cc83deaedd07e5200a65edde34e374de",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("patch result missing %q: %s", want, result)
		}
	}

	got, _ = os.ReadFile(filepath.Join(dir, "foo", "SKILL.md"))
	if !strings.Contains(string(got), "Do foo better.") {
		t.Fatal("patch not applied")
	}
}

func TestSkillManageTool_CreateAndEditRejectUnknownAllowedTool(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	invalid := "---\nname: invalid\ndescription: Invalid skill\nmetadata:\n  allowed_tools: [not_a_real_tool]\n---\n\nDo invalid work.\n"
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"create","name":"invalid","content":%q}`, invalid)); err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("create should reject unknown allowed tool, got %v", err)
	}

	valid := "---\nname: alpha\ndescription: Alpha skill\n---\n\nDo alpha work.\n"
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"create","name":"alpha","content":%q}`, valid)); err != nil {
		t.Fatal(err)
	}
	invalidEdit := strings.ReplaceAll(invalid, "name: invalid", "name: alpha")
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"edit","name":"alpha","content":%q}`, invalidEdit)); err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("edit should reject unknown allowed tool, got %v", err)
	}
	if got := readSKILL(t, dir, "alpha"); got != valid {
		t.Fatalf("rejected edit changed skill: %q", got)
	}
	invalidPatch := `{"action":"patch","name":"alpha","old_string":"description: Alpha skill","new_string":"description: Alpha skill\nmetadata:\n  allowed_tools: [not_a_real_tool]"}`
	if _, err := tool.Call(context.Background(), invalidPatch); err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("patch should reject unknown allowed tool, got %v", err)
	}
	if got := readSKILL(t, dir, "alpha"); got != valid {
		t.Fatalf("rejected patch changed skill: %q", got)
	}
}

func TestSkillManageTool_CreateAllowsRegisteredTools(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	content := "---\nname: valid\ndescription: Valid skill\nmetadata:\n  allowed_tools: [bridge_calendar, recall_device_memory, inspect_episode, wait_for_wakeup]\n---\n\nDo valid work.\n"
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"create","name":"valid","content":%q}`, content)); err != nil {
		t.Fatalf("create rejected registered allowed tools: %v", err)
	}
}

func TestSkillManageTool_CreateRejectsAgentHiddenTool(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	content := "---\nname: hidden\ndescription: Hidden tool skill\nmetadata:\n  allowed_tools: [search_launch_app]\n---\n\nDo hidden work.\n"
	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"create","name":"hidden","content":%q}`, content)); err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("create should reject agent-hidden tool, got %v", err)
	}
}

func TestSkillManageTool_ActionResultsExposeVerificationDetails(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	manifestPath := filepath.Join(configDir, "skill-state", ".bundled_manifest.json")
	tool := NewSkillManageTool(skillsDir, manifestPath)
	content := "---\nname: alpha\ndescription: Alpha skill\n---\n\nDo alpha.\n"

	if _, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"create","name":"alpha","content":%q}`, content)); err != nil {
		t.Fatal(err)
	}

	writeResult, err := tool.Call(context.Background(), `{"action":"write_file","name":"alpha","file_path":"references/info.md","file_content":"notes"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"created=true", "bytes=5", "lines=1", "sha256="} {
		if !strings.Contains(writeResult, want) {
			t.Fatalf("write_file result missing %q: %s", want, writeResult)
		}
	}

	removeResult, err := tool.Call(context.Background(), `{"action":"remove_file","name":"alpha","file_path":"references/info.md"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(removeResult, `Removed references/info.md from skill "alpha"`) {
		t.Fatalf("unexpected remove_file result: %s", removeResult)
	}

	for _, tc := range []struct {
		action string
	}{
		{action: "mark_stale"},
		{action: "archive"},
		{action: "restore_archive"},
	} {
		result, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":%q,"name":"alpha"}`, tc.action))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result, `skill "alpha"`) {
			t.Fatalf("unexpected %s result: %s", tc.action, result)
		}
	}

	deleteResult, err := tool.Call(context.Background(), `{"action":"delete","name":"alpha"}`)
	if err != nil {
		t.Fatal(err)
	}
	if deleteResult != `Deleted skill "alpha"` {
		t.Fatalf("unexpected delete result: %s", deleteResult)
	}
}

func TestSkillManageTool_InstallFromURL(t *testing.T) {
	content := "---\nname: remote\ndescription: Remote skill\n---\n\nDo remote work.\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/SKILL.md" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(server.Client())
	out, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"install","source_url":%q,"reason":"test"}`, server.URL+"/SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256([]byte(content))
	for _, want := range []string{
		`Installed skill "remote"`,
		"total_bytes=64",
		"bytes=64",
		"lines=6",
		"sha256=" + hex.EncodeToString(wantHash[:]),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("install result missing %q: %s", want, out)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "remote", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("installed content mismatch: %q", string(got))
	}
}

func TestSkillManageTool_InstallInvalidContentLeavesNoSkill(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a skill"))
	}))
	defer server.Close()

	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(server.Client())
	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"install","source_url":%q}`, server.URL+"/SKILL.md"))
	if err == nil || !strings.Contains(err.Error(), "invalid SKILL.md") {
		t.Fatalf("expected invalid SKILL.md error, got %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr == nil && len(entries) != 0 {
		t.Fatalf("invalid install left files behind: %+v", entries)
	}
}

func TestSkillManageTool_InstallGitHubDirectoryWithSupportingFiles(t *testing.T) {
	content := "---\nname: remote\ndescription: Remote skill\n---\n\nRead references/guide.md.\n"
	metadata := `{"files":[` +
		`{"name":"/skills/remote/SKILL.md","size":77},` +
		`{"name":"/skills/remote/references/guide.md","size":15},` +
		`{"name":"/skills/other/SKILL.md","size":68},` +
		`{"name":"/skills/remote/README.md","size":7}` +
		`]}`
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch {
		case req.URL.Host == "data.jsdelivr.com":
			body = metadata
		case req.URL.Path == "/gh/acme/skills@main/skills/remote/SKILL.md":
			body = content
		case req.URL.Path == "/gh/acme/skills@main/skills/remote/references/guide.md":
			body = "Use the guide.\n"
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header), Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}

	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(client)
	out, err := tool.Call(context.Background(), `{"action":"install","source_url":"https://github.com/acme/skills/tree/main/skills/remote"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "files=2") {
		t.Fatalf("install result should report two files: %s", out)
	}
	gotSkill, err := os.ReadFile(filepath.Join(dir, "remote", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotSkill) != content {
		t.Fatalf("SKILL.md mismatch: %q", string(gotSkill))
	}
	gotGuide, err := os.ReadFile(filepath.Join(dir, "remote", "references", "guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotGuide) != "Use the guide.\n" {
		t.Fatalf("guide mismatch: %q", string(gotGuide))
	}
	if _, err := os.Stat(filepath.Join(dir, "remote", "README.md")); !os.IsNotExist(err) {
		t.Fatalf("unsupported README.md should not be installed, stat err=%v", err)
	}
}

func TestSkillManageTool_InstallDownloadFailureKeepsExistingSkills(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Body:       io.NopCloser(bytes.NewBufferString("upstream unavailable")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})})

	_, err := tool.Call(context.Background(), `{"action":"install","source_url":"https://example.test/SKILL.md"}`)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("expected download failure, got %v", err)
	}
	if strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("download failure exposed response body: %v", err)
	}
	if got := readSKILL(t, dir, "alpha"); got != testSkillA {
		t.Fatalf("existing skill changed after failed install: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "alpha" {
		t.Fatalf("failed install left partial entries: %+v", entries)
	}
}

func TestSkillInstallClientRejectsPrivateTargetWhenProxyConfigured(t *testing.T) {
	originalLookup := lookupSkillInstallHostIPs
	lookupSkillInstallHostIPs = func(host string) ([]net.IP, error) {
		if host != "internal.invalid" {
			t.Fatalf("lookup host = %q, want internal.invalid", host)
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	defer func() { lookupSkillInstallHostIPs = originalLookup }()

	client := newSkillInstallHTTPClient(ProxyConfig{HTTPProxy: "http://127.0.0.1:8888"})
	_, err := client.Get("http://internal.invalid/SKILL.md")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("proxied private target should be rejected before proxy transport, got %v", err)
	}
}

func TestSkillInstallClientPinsGenericProxyTarget(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxyURL, err := url.Parse("http://127.0.0.1:8888")
	if err != nil {
		t.Fatal(err)
	}
	transport.Proxy = http.ProxyURL(proxyURL)
	req, err := http.NewRequest(http.MethodGet, "https://user-controlled.invalid/SKILL.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	pinnedReq, next, err := pinSkillInstallProxyTarget(req, transport, net.ParseIP("93.184.216.34"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pinnedReq.URL.Host, "93.184.216.34:443"; got != want {
		t.Fatalf("pinned URL host = %q, want %q", got, want)
	}
	if got, want := pinnedReq.Host, "user-controlled.invalid"; got != want {
		t.Fatalf("request Host = %q, want %q", got, want)
	}
	pinnedTransport, ok := next.(*http.Transport)
	if !ok {
		t.Fatalf("pinned transport = %T, want *http.Transport", next)
	}
	if pinnedTransport.TLSClientConfig == nil {
		t.Fatal("pinned transport has no TLS config")
	}
	if got := pinnedTransport.TLSClientConfig.ServerName; got != "user-controlled.invalid" {
		t.Fatalf("TLS ServerName = %q, want user-controlled.invalid", got)
	}
	proxyURL, err = pinnedTransport.Proxy(pinnedReq)
	if err != nil {
		t.Fatal(err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:8888" {
		t.Fatalf("generic public source proxy = %v", proxyURL)
	}
}

func TestSkillInstallClientRejectsPrivateRedirectTarget(t *testing.T) {
	originalLookup := lookupSkillInstallHostIPs
	lookupSkillInstallHostIPs = func(host string) ([]net.IP, error) {
		switch host {
		case "public.invalid":
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		case "internal.invalid":
			return []net.IP{net.ParseIP("10.0.0.8")}, nil
		default:
			t.Fatalf("unexpected lookup host %q", host)
			return nil, nil
		}
	}
	defer func() { lookupSkillInstallHostIPs = originalLookup }()

	requests := 0
	client := &http.Client{Transport: skillInstallTargetGuard{next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://internal.invalid/SKILL.md"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}}
	_, err := client.Get("http://public.invalid/SKILL.md")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("private redirect target should be rejected, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("transport requests = %d, want only the public request", requests)
	}
}

func TestSkillInstallClientRejectsDNSRebindingAtDialTime(t *testing.T) {
	for _, env := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(env, "")
	}
	originalLookup := lookupSkillInstallHostIPs
	lookupCalls := 0
	lookupSkillInstallHostIPs = func(host string) ([]net.IP, error) {
		if host != "rebind.invalid" {
			t.Fatalf("lookup host = %q, want rebind.invalid", host)
		}
		lookupCalls++
		if lookupCalls == 1 {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	defer func() { lookupSkillInstallHostIPs = originalLookup }()

	_, err := newSkillInstallHTTPClient(ProxyConfig{}).Get("http://rebind.invalid/SKILL.md")
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("DNS rebinding should be rejected at dial time, got %v", err)
	}
	if lookupCalls < 2 {
		t.Fatalf("lookup calls = %d, want request-time and dial-time validation", lookupCalls)
	}
}

func TestSkillManageTool_InstallRejectsUnknownAllowedTool(t *testing.T) {
	content := "---\nname: remote\ndescription: Remote skill\nmetadata:\n  allowed_tools: [not_a_real_tool]\n---\n\nDo remote work.\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(server.Client())
	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"install","source_url":%q}`, server.URL+"/SKILL.md"))
	if err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("expected allowed_tools validation error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "remote")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid install should not create a skill, stat err=%v", statErr)
	}
}

func TestSkillManageTool_InstallRejectsAgentHiddenTool(t *testing.T) {
	content := "---\nname: remote-hidden\ndescription: Hidden tool skill\nmetadata:\n  allowed_tools: [search_launch_app]\n---\n\nDo hidden work.\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = io.WriteString(w, content)
	}))
	defer server.Close()

	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")
	tool.SetHTTPClient(server.Client())
	_, err := tool.Call(context.Background(), fmt.Sprintf(`{"action":"install","source_url":%q}`, server.URL+"/SKILL.md"))
	if err == nil || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("install should reject agent-hidden tool, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "remote-hidden")); !os.IsNotExist(statErr) {
		t.Fatalf("rejected install left skill directory, stat error = %v", statErr)
	}
}

func TestNormalizeSkillInstallURLGitHubTree(t *testing.T) {
	got, err := normalizeSkillInstallURL("https://github.com/zerob13/skills/tree/master/skills/ai-slop-taste")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://raw.githubusercontent.com/zerob13/skills/master/skills/ai-slop-taste/SKILL.md"
	if got != want {
		t.Fatalf("normalized URL = %q, want %q", got, want)
	}
}

func TestSkillManageTool_InstallQAGitHubURL(t *testing.T) {
	if os.Getenv("AICO_E2E_SKILL_INSTALL") != "1" {
		t.Skip("set AICO_E2E_SKILL_INSTALL=1 to run the live GitHub install check")
	}

	dir := t.TempDir()
	out, err := NewSkillManageTool(dir, "").Call(context.Background(), `{"action":"install","source_url":"https://github.com/zerob13/skills/tree/master/skills/ai-slop-taste"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Installed skill "ai-slop-taste"`) {
		t.Fatalf("unexpected install result: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ai-slop-taste", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("bytes=%d", len(got)),
		fmt.Sprintf("lines=%d", lineCount(got)),
		"sha256=" + strings.TrimPrefix(hashContent(got), "sha256:"),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("install result missing %q: %s", want, out)
		}
	}
}

func TestSkillManageTool_PatchAllowsEmptyReplacement(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: alpha\ndescription: Alpha skill\n---\n\nKeep this instruction.\n\nRemove this instruction.\n"
	writeSKILL(t, dir, "alpha", content)
	tool := NewSkillManageTool(dir, "")

	if _, err := tool.Call(context.Background(), `{"action":"patch","name":"alpha","old_string":"Remove this instruction.","new_string":"","reason":"remove obsolete instruction"}`); err != nil {
		t.Fatal(err)
	}
	got := readSKILL(t, dir, "alpha")
	if strings.Contains(got, "Remove this instruction.") || !strings.Contains(got, "Keep this instruction.") {
		t.Fatalf("expected patch to remove text, got %q", got)
	}
}

func TestSkillManageTool_WriteFileAllowsEmptyContent(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	tool := NewSkillManageTool(dir, "")

	if _, err := tool.Call(context.Background(), `{"action":"write_file","name":"alpha","file_path":"references/empty.md","file_content":"","reason":"create placeholder"}`); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "alpha", "references", "empty.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty supporting file, got %q", string(data))
	}
}

func TestSkillManageTool_SupportingFileChangesMarkManifestModified(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	manifestPath := filepath.Join(configDir, "skill-state", ".bundled_manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	originHash := hashContent([]byte(testSkillA))
	saveManifest(manifestPath, &BundledManifest{Version: 1, Skills: map[string]ManifestEntry{
		"alpha": {
			OriginHash:         originHash,
			EffectiveHash:      originHash,
			Status:             StatusMerged,
			LastFailedMergeKey: "failed-key",
			LastMergeError:     "old failure",
		},
	}})

	tool := NewSkillManageTool(skillsDir, manifestPath)
	input := `{"action":"write_file","name":"alpha","file_path":"references/info.md","file_content":"notes","reason":"test"}`
	if _, err := tool.Call(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	manifest := loadManifest(manifestPath)
	entry := manifest.Skills["alpha"]
	if entry.Status != StatusUserModified {
		t.Fatalf("expected user_modified after supporting file write, got %s", entry.Status)
	}
	if entry.OriginHash != originHash || entry.EffectiveHash != originHash {
		t.Fatal("supporting file write should not change origin/effective hashes")
	}
	if entry.LastFailedMergeKey != "failed-key" || entry.LastMergeError != "old failure" {
		t.Fatalf("expected supporting file write to preserve failed merge fields, got key=%q error=%q", entry.LastFailedMergeKey, entry.LastMergeError)
	}

	removeInput := `{"action":"remove_file","name":"alpha","file_path":"references/info.md","reason":"test"}`
	if _, err := tool.Call(context.Background(), removeInput); err != nil {
		t.Fatal(err)
	}
	manifest = loadManifest(manifestPath)
	if manifest.Skills["alpha"].Status != StatusUserModified {
		t.Fatalf("expected user_modified after supporting file remove, got %s", manifest.Skills["alpha"].Status)
	}
}

func TestSkillManageTool_SupportingFileRequiresExistingSkill(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillManageTool(dir, "")

	_, err := tool.Call(context.Background(), `{"action":"write_file","name":"missing","file_path":"references/info.md","file_content":"notes","reason":"test"}`)
	if err == nil {
		t.Fatal("expected write_file for missing skill to fail")
	}
	if fileExists(filepath.Join(dir, "missing", "references", "info.md")) {
		t.Fatal("write_file created supporting file for missing skill")
	}

	_, err = tool.Call(context.Background(), `{"action":"remove_file","name":"missing","file_path":"references/info.md","reason":"test"}`)
	if err == nil {
		t.Fatal("expected remove_file for missing skill to fail")
	}
}

func TestSkillManageTool_LifecycleStateActions(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	manifestPath := filepath.Join(configDir, "skill-state", ".bundled_manifest.json")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	tool := NewSkillManageTool(skillsDir, manifestPath)

	if _, err := tool.Call(context.Background(), `{"action":"mark_stale","name":"alpha","reason":"old"}`); err != nil {
		t.Fatal(err)
	}
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateStale || usage["alpha"].StateChangedAt == "" {
		t.Fatalf("expected stale state, got %+v", usage["alpha"])
	}

	if _, err := tool.Call(context.Background(), `{"action":"archive","name":"alpha","reason":"old"}`); err != nil {
		t.Fatal(err)
	}
	usage = loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateArchived {
		t.Fatalf("expected archived state, got %+v", usage["alpha"])
	}

	if _, err := tool.Call(context.Background(), `{"action":"restore_archive","name":"alpha","reason":"needed"}`); err != nil {
		t.Fatal(err)
	}
	usage = loadSkillUsage(usagePath)
	if usage["alpha"].State != SkillUsageStateActive {
		t.Fatalf("expected active state, got %+v", usage["alpha"])
	}
}

func TestSkillReadAndManageUpdateUsage(t *testing.T) {
	configDir := t.TempDir()
	skillsDir := filepath.Join(configDir, "skills")
	manifestPath := filepath.Join(configDir, "skill-state", ".bundled_manifest.json")
	usagePath := filepath.Join(configDir, "skill-state", "usage.json")
	writeSKILL(t, skillsDir, "alpha", testSkillA)

	readTool := NewSkillReadTool(skillsDir, usagePath)
	if _, err := readTool.Call(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	usage := loadSkillUsage(usagePath)
	if usage["alpha"].ViewCount != 1 || usage["alpha"].LastViewedAt == "" {
		t.Fatalf("expected skill_read usage update, got %+v", usage["alpha"])
	}

	manageTool := NewSkillManageTool(skillsDir, manifestPath)
	patchInput := `{"action":"patch","name":"alpha","old_string":"Do alpha things.","new_string":"Do alpha things safely.","reason":"test"}`
	if _, err := manageTool.Call(context.Background(), patchInput); err != nil {
		t.Fatal(err)
	}
	usage = loadSkillUsage(usagePath)
	if usage["alpha"].ModifyCount != 1 || usage["alpha"].LastModifiedAt == "" {
		t.Fatalf("expected skill_manage usage update, got %+v", usage["alpha"])
	}
}

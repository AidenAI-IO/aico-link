package agent

import (
	"aico-agent/internal/logging"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type SkillUsageEntry struct {
	ViewCount      int    `json:"view_count,omitempty"`
	UseCount       int    `json:"use_count,omitempty"`
	ModifyCount    int    `json:"modify_count,omitempty"`
	LastViewedAt   string `json:"last_viewed_at,omitempty"`
	LastUsedAt     string `json:"last_used_at,omitempty"`
	LastModifiedAt string `json:"last_modified_at,omitempty"`
	State          string `json:"state,omitempty"`
	StateChangedAt string `json:"state_changed_at,omitempty"`
}

const (
	SkillUsageStateActive   = "active"
	SkillUsageStateStale    = "stale"
	SkillUsageStateArchived = "archived"

	SkillLifecycleStaleAfterDays   = 90
	SkillLifecycleArchiveAfterDays = 180
	SkillLifecycleScanInterval     = 24 * time.Hour
)

type skillLifecycleScanState struct {
	LastEvaluatedAt string `json:"last_evaluated_at,omitempty"`
}

var usageMu sync.Mutex

func loadSkillUsage(path string) map[string]SkillUsageEntry {
	usageMu.Lock()
	defer usageMu.Unlock()
	return readSkillUsage(path)
}

func recordSkillViewed(path, name string) {
	updateSkillUsage(path, name, true, false, false)
}

func recordSkillModified(path, name string) {
	updateSkillUsage(path, name, false, false, true)
}

func recordSkillUsed(path, name string) {
	updateSkillUsage(path, name, false, true, false)
}

func updateSkillUsage(path, name string, viewed, used, modified bool) {
	if path == "" || name == "" {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()

	usage := readSkillUsage(path)
	entry := usage[name]
	now := time.Now().Format(time.RFC3339)
	if viewed {
		entry.ViewCount++
		entry.LastViewedAt = now
	}
	if used {
		entry.UseCount++
		entry.LastUsedAt = now
		if entry.State == SkillUsageStateStale || entry.State == SkillUsageStateArchived {
			entry.State = SkillUsageStateActive
			entry.StateChangedAt = now
		}
	}
	if modified {
		entry.ModifyCount++
		entry.LastModifiedAt = now
	}
	usage[name] = entry
	saveSkillUsage(path, usage)
}

func applyAutomaticSkillLifecycle(skillsDir, usagePath string, now time.Time) {
	if skillsDir == "" || usagePath == "" {
		return
	}

	usageMu.Lock()
	defer usageMu.Unlock()
	if shouldSkipAutomaticSkillLifecycle(usagePath, now) {
		return
	}

	usage := readSkillUsage(usagePath)
	changed := false
	for name, entry := range usage {
		if !shouldAutoManageSkill(filepath.Join(skillsDir, name, "SKILL.md")) {
			continue
		}
		reference, ok := skillLifecycleReferenceTime(entry)
		if !ok {
			continue
		}
		age := now.Sub(reference)
		newState := normalizeSkillUsageState(entry.State)
		if age >= time.Duration(SkillLifecycleArchiveAfterDays)*24*time.Hour {
			newState = SkillUsageStateArchived
		} else if age >= time.Duration(SkillLifecycleStaleAfterDays)*24*time.Hour && newState == SkillUsageStateActive {
			newState = SkillUsageStateStale
		}
		if newState != normalizeSkillUsageState(entry.State) {
			entry.State = newState
			entry.StateChangedAt = now.Format(time.RFC3339)
			usage[name] = entry
			changed = true
		}
	}
	if changed {
		saveSkillUsage(usagePath, usage)
	}
	saveSkillLifecycleScanState(usagePath, skillLifecycleScanState{LastEvaluatedAt: now.Format(time.RFC3339)})
}

func shouldSkipAutomaticSkillLifecycle(usagePath string, now time.Time) bool {
	state := loadSkillLifecycleScanState(usagePath)
	last, ok := latestRFC3339Time(state.LastEvaluatedAt)
	return ok && now.Sub(last) < SkillLifecycleScanInterval
}

func loadSkillLifecycleScanState(usagePath string) skillLifecycleScanState {
	var state skillLifecycleScanState
	data, err := os.ReadFile(skillLifecycleScanStatePath(usagePath))
	if err != nil {
		return state
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return skillLifecycleScanState{}
	}
	return state
}

func saveSkillLifecycleScanState(usagePath string, state skillLifecycleScanState) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		logging.Warnf("agent", "skill_usage", "marshal lifecycle scan state: %v", err)
		return
	}
	if err := writeFileAtomic(skillLifecycleScanStatePath(usagePath), data, 0o644); err != nil {
		logging.Warnf("agent", "skill_usage", "write lifecycle scan state: %v", err)
	}
}

func skillLifecycleScanStatePath(usagePath string) string {
	return filepath.Join(filepath.Dir(usagePath), "lifecycle_scan.json")
}

func shouldAutoManageSkill(skillPath string) bool {
	skill, err := loadSkillMetadata(skillPath)
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(skill.Source), "agent") ||
		strings.EqualFold(strings.TrimSpace(skill.CreatedBy), "agent")
}

func skillLifecycleReferenceTime(entry SkillUsageEntry) (time.Time, bool) {
	return latestRFC3339Time(entry.LastUsedAt, entry.LastModifiedAt, entry.LastViewedAt)
}

func latestRFC3339Time(values ...string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			continue
		}
		if !found || parsed.After(latest) {
			latest = parsed
			found = true
		}
	}
	return latest, found
}

func setSkillUsageState(path, name, state string) {
	if path == "" || name == "" {
		return
	}
	usageMu.Lock()
	defer usageMu.Unlock()

	usage := readSkillUsage(path)
	entry := usage[name]
	entry.State = normalizeSkillUsageState(state)
	entry.StateChangedAt = time.Now().Format(time.RFC3339)
	usage[name] = entry
	saveSkillUsage(path, usage)
}

func normalizeSkillUsageState(state string) string {
	switch state {
	case SkillUsageStateStale, SkillUsageStateArchived:
		return state
	default:
		return SkillUsageStateActive
	}
}

func readSkillUsage(path string) map[string]SkillUsageEntry {
	usage := make(map[string]SkillUsageEntry)
	data, err := os.ReadFile(path)
	if err != nil {
		return usage
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		return make(map[string]SkillUsageEntry)
	}
	if usage == nil {
		return make(map[string]SkillUsageEntry)
	}
	return usage
}

func saveSkillUsage(path string, usage map[string]SkillUsageEntry) {
	data, err := json.MarshalIndent(usage, "", "  ")
	if err != nil {
		logging.Warnf("agent", "skill_usage", "marshal usage: %v", err)
		return
	}
	if err := writeFileAtomic(path, data, 0o644); err != nil {
		logging.Warnf("agent", "skill_usage", "write usage: %v", err)
	}
}

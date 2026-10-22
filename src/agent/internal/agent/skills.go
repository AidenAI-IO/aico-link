package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

const (
	maxSkillCatalogEntries          = 30
	maxSkillCatalogDescriptionRunes = 160
)

type SkillManager struct {
	index           *SkillIndex
	mu              sync.RWMutex
	activatedSkills map[string]*SkillDefinition
	usagePath       string
	deviceTypeFn    func() string
}

type ResolvedSkills struct {
	Names               []string
	PreferredModel      string
	Instructions        []string
	AllowedTools        map[string]struct{}
	AllowedChildren     map[string]struct{}
	HasToolRestriction  bool
	HasChildRestriction bool
	manager             *SkillManager
}

func (m *SkillManager) SetUsagePath(path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usagePath = strings.TrimSpace(path)
}

func (m *SkillManager) SetDeviceTypeFunc(fn func() string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deviceTypeFn = fn
}

func NewSkillManager(index *SkillIndex) *SkillManager {
	return &SkillManager{
		index:           index,
		activatedSkills: make(map[string]*SkillDefinition),
	}
}

// GetIndex returns the skill index for discovery
func (m *SkillManager) GetIndex() *SkillIndex {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.index
}

func (m *SkillManager) ReplaceIndex(index *SkillIndex) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.index = index
	for name := range m.activatedSkills {
		if skill, ok := index.Get(name); ok {
			m.activatedSkills[name] = skill
		} else {
			delete(m.activatedSkills, name)
		}
	}
}

func (m *SkillManager) Snapshot() *SkillManager {
	m.mu.RLock()
	defer m.mu.RUnlock()

	activated := make(map[string]*SkillDefinition, len(m.activatedSkills))
	for name, skill := range m.activatedSkills {
		activated[name] = skill
	}
	return &SkillManager{
		index:           m.index,
		activatedSkills: activated,
		usagePath:       m.usagePath,
		deviceTypeFn:    m.deviceTypeFn,
	}
}

// Activate loads the full instructions for a skill by name
func (m *SkillManager) Activate(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if already activated
	if _, ok := m.activatedSkills[name]; ok {
		return nil
	}

	// Get skill from index
	skill, ok := m.index.Get(name)
	if !ok {
		return fmt.Errorf("unknown skill %q", name)
	}
	deviceType := m.deviceTypeLocked()
	if !skillSupportsDeviceType(skill, deviceType) {
		return fmt.Errorf("skill %q supports device_type %s, current device_type is %s", name, formatSkillDeviceTypes(skill.DeviceTypes), currentDeviceTypeForMessage(deviceType))
	}

	// Mark as activated
	m.activatedSkills[name] = skill
	return nil
}

// Resolve merges all activated skills into a single ResolvedSkills
func (m *SkillManager) Resolve(names []string) (ResolvedSkills, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	resolved := ResolvedSkills{
		Names:           uniqueNonEmpty(names),
		AllowedTools:    map[string]struct{}{},
		AllowedChildren: map[string]struct{}{},
		manager:         m,
	}

	for _, name := range resolved.Names {
		skill, ok := m.activatedSkills[name]
		if !ok {
			return ResolvedSkills{}, fmt.Errorf("skill %q not activated", name)
		}
		deviceType := m.deviceTypeLocked()
		if !skillSupportsDeviceType(skill, deviceType) {
			return ResolvedSkills{}, fmt.Errorf("skill %q supports device_type %s, current device_type is %s", name, formatSkillDeviceTypes(skill.DeviceTypes), currentDeviceTypeForMessage(deviceType))
		}

		if text := strings.TrimSpace(skill.Instructions); text != "" {
			resolved.Instructions = append(resolved.Instructions, fmt.Sprintf("[%s] %s", name, text))
		}

		if skill.PreferredModel != "" {
			resolved.PreferredModel = skill.PreferredModel
		}

		if len(skill.AllowedTools) > 0 {
			resolved.HasToolRestriction = true
			for _, tool := range skill.AllowedTools {
				resolved.AllowedTools[tool] = struct{}{}
			}
		}

		if len(skill.AllowedChildren) > 0 {
			resolved.HasChildRestriction = true
			for _, child := range skill.AllowedChildren {
				resolved.AllowedChildren[child] = struct{}{}
			}
		}
	}

	return resolved, nil
}

// GetActivatedSkills returns all currently activated skills
func (m *SkillManager) GetActivatedSkills() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.activatedSkills))
	for name := range m.activatedSkills {
		names = append(names, name)
	}
	return names
}

func (r ResolvedSkills) CatalogSummary() string {
	if r.manager == nil {
		return "No skills available."
	}
	return r.manager.CatalogSummary()
}

func (m *SkillManager) CatalogSummary() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.index == nil || len(m.index.skills) == 0 {
		return "No skills available."
	}
	usage := map[string]SkillUsageEntry{}
	if m.usagePath != "" {
		usage = loadSkillUsage(m.usagePath)
	}
	names := m.index.Names()
	sort.Strings(names)
	deviceType := m.deviceTypeLocked()
	lines := make([]string, 0, min(maxSkillCatalogEntries, len(names)))
	hidden := 0
	for _, name := range names {
		if normalizeSkillUsageState(usage[name].State) == SkillUsageStateArchived {
			continue
		}
		skill, ok := m.index.Get(name)
		if !ok {
			continue
		}
		if !skillSupportsDeviceType(skill, deviceType) {
			continue
		}
		if len(lines) >= maxSkillCatalogEntries {
			hidden++
			continue
		}
		desc := truncateForLog(strings.TrimSpace(skill.Description), maxSkillCatalogDescriptionRunes)
		if desc == "" {
			desc = "(no description)"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", name, desc))
	}
	if len(lines) == 0 {
		return "No skills available."
	}
	if hidden > 0 {
		lines = append(lines, fmt.Sprintf("... %d more skills hidden. Use skill_list to search.", hidden))
	}
	return strings.Join(lines, "\n")
}

func (m *SkillManager) deviceTypeLocked() string {
	if m == nil || m.deviceTypeFn == nil {
		return ""
	}
	return m.deviceTypeFn()
}

func skillSupportsDeviceType(skill *SkillDefinition, deviceType string) bool {
	if skill == nil || len(skill.DeviceTypes) == 0 {
		return true
	}
	if strings.TrimSpace(deviceType) == "" {
		return true
	}
	normalized, ok := normalizeDeviceType(deviceType)
	if !ok {
		return false
	}
	for _, supported := range skill.DeviceTypes {
		if supported == normalized {
			return true
		}
	}
	return false
}

func formatSkillDeviceTypes(deviceTypes []string) string {
	if len(deviceTypes) == 0 {
		return "all"
	}
	return strings.Join(deviceTypes, ", ")
}

func currentDeviceTypeForMessage(deviceType string) string {
	if normalized, ok := normalizeDeviceType(deviceType); ok {
		return normalized
	}
	if strings.TrimSpace(deviceType) != "" {
		return strings.TrimSpace(deviceType)
	}
	return "unknown"
}

func (r ResolvedSkills) CombinedInstructions() string {
	if len(r.Instructions) == 0 {
		return ""
	}
	return strings.Join(r.Instructions, "\n")
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

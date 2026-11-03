package agent

import (
	"aico-agent/internal/logging"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillMetadata represents the frontmatter of a SKILL.md file
type SkillMetadata struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description"`
	Source      string                 `yaml:"source,omitempty"`
	CreatedBy   string                 `yaml:"created_by,omitempty"`
	Metadata    map[string]interface{} `yaml:"metadata,omitempty"`
}

// SkillDefinition represents a complete skill loaded from SKILL.md
type SkillDefinition struct {
	Name            string
	Description     string
	Instructions    string
	PreferredModel  string
	AllowedTools    []string
	AllowedChildren []string
	DeviceTypes     []string
	Source          string
	CreatedBy       string
	FilePath        string
}

// SkillIndex holds the discovery-phase metadata for all skills
type SkillIndex struct {
	skills map[string]*SkillDefinition
}

// NewSkillIndex creates an empty skill index
func NewSkillIndex() *SkillIndex {
	return &SkillIndex{
		skills: make(map[string]*SkillDefinition),
	}
}

// LoadSkillsFromDirs scans the given directories for SKILL.md files
// and builds an index with name + description only (progressive disclosure phase 1)
func LoadSkillsFromDirs(dirs []string) (*SkillIndex, error) {
	index := NewSkillIndex()

	for _, dir := range dirs {
		if err := index.scanDirectory(dir); err != nil {
			return nil, fmt.Errorf("scan directory %q: %w", dir, err)
		}
	}

	return index, nil
}

// scanDirectory walks through a directory and loads all SKILL.md files
func (idx *SkillIndex) scanDirectory(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Only process SKILL.md files (case-insensitive)
		if strings.ToUpper(filepath.Base(path)) != "SKILL.MD" {
			return nil
		}

		skill, err := loadSkillMetadata(path)
		if err != nil {
			logging.Warnf("agent", "skill_loader", "skipping %q: %v", path, err)
			return nil
		}

		if _, exists := idx.skills[skill.Name]; exists {
			logging.Warnf("agent", "skill_loader", "duplicate skill name %q in %q, keeping first", skill.Name, path)
			return nil
		}

		idx.skills[skill.Name] = skill
		return nil
	})
}

// loadSkillMetadata reads a SKILL.md file and extracts metadata from frontmatter
func loadSkillMetadata(path string) (*SkillDefinition, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Parse frontmatter and body
	frontmatter, body, err := parseFrontmatter(string(content))
	if err != nil {
		return nil, err
	}

	var meta SkillMetadata
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	if meta.Name == "" {
		return nil, fmt.Errorf("skill name is required in frontmatter")
	}
	if meta.Description == "" {
		return nil, fmt.Errorf("skill description is required in frontmatter")
	}

	return skillDefinitionFromMetadata(meta, body, path)
}

// parseFrontmatter splits a markdown document into YAML frontmatter and body.
// Expects the document to start with "---\n", followed by YAML, then "---\n".
func parseFrontmatter(content string) (frontmatter, body string, err error) {
	content = strings.TrimLeft(content, "")
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", "", fmt.Errorf("missing frontmatter delimiter")
	}

	// Skip the opening delimiter
	rest := content
	if strings.HasPrefix(rest, "---\r\n") {
		rest = rest[5:]
	} else {
		rest = rest[4:]
	}

	// Find the closing delimiter
	endIdx := strings.Index(rest, "\n---\n")
	endLen := 5
	if endIdx < 0 {
		endIdx = strings.Index(rest, "\n---\r\n")
		endLen = 6
	}
	if endIdx < 0 {
		return "", "", fmt.Errorf("missing closing frontmatter delimiter")
	}

	frontmatter = rest[:endIdx]
	body = rest[endIdx+endLen:]
	return frontmatter, body, nil
}

func interfaceSliceToStringSlice(in []interface{}, fieldName string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
			continue
		}
		logging.Warnf("agent", "skill_loader", "ignoring non-string %s value %v (type %T)", fieldName, v, v)
	}
	return out
}

func metadataStringList(value interface{}, fieldName string) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{typed}, nil
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, v := range typed {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("metadata.%s must contain only strings, got %T", fieldName, v)
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return append([]string{}, typed...), nil
	default:
		return nil, fmt.Errorf("metadata.%s must be a string or list of strings, got %T", fieldName, value)
	}
}

func normalizeSkillDeviceTypes(value interface{}) ([]string, error) {
	values, err := metadataStringList(value, "device_types")
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("metadata.device_types must not contain empty values")
		}
		deviceType, ok := normalizeDeviceType(value)
		if !ok {
			return nil, fmt.Errorf("invalid metadata.device_types value %q (expected iOS, Android, macOS, windows, or linux)", value)
		}
		if _, ok := seen[deviceType]; ok {
			continue
		}
		seen[deviceType] = struct{}{}
		out = append(out, deviceType)
	}
	return out, nil
}

func skillDefinitionFromMetadata(meta SkillMetadata, body, path string) (*SkillDefinition, error) {
	skill := &SkillDefinition{
		Name:         meta.Name,
		Description:  meta.Description,
		Instructions: strings.TrimSpace(body),
		Source:       meta.Source,
		CreatedBy:    meta.CreatedBy,
		FilePath:     path,
	}

	if meta.Metadata != nil {
		if skill.Source == "" {
			if source, ok := meta.Metadata["source"].(string); ok {
				skill.Source = source
			}
		}
		if skill.CreatedBy == "" {
			if createdBy, ok := meta.Metadata["created_by"].(string); ok {
				skill.CreatedBy = createdBy
			}
		}
		if model, ok := meta.Metadata["preferred_model"].(string); ok {
			skill.PreferredModel = model
		}
		if tools, ok := meta.Metadata["allowed_tools"].([]interface{}); ok {
			skill.AllowedTools = interfaceSliceToStringSlice(tools, "allowed_tools")
		}
		if children, ok := meta.Metadata["allowed_children"].([]interface{}); ok {
			skill.AllowedChildren = interfaceSliceToStringSlice(children, "allowed_children")
		}
		if value, ok := meta.Metadata["device_types"]; ok {
			deviceTypes, err := normalizeSkillDeviceTypes(value)
			if err != nil {
				return nil, err
			}
			skill.DeviceTypes = deviceTypes
		}
	}

	return skill, nil
}

// Get returns the skill definition for a given name
func (idx *SkillIndex) Get(name string) (*SkillDefinition, bool) {
	skill, ok := idx.skills[name]
	return skill, ok
}

// parseSkillFromContent parses a SKILL.md from raw content without reading from disk.
func parseSkillFromContent(content string) (*SkillDefinition, error) {
	frontmatter, body, err := parseFrontmatter(content)
	if err != nil {
		return nil, err
	}

	var meta SkillMetadata
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	if meta.Name == "" {
		return nil, fmt.Errorf("skill name is required in frontmatter")
	}
	if meta.Description == "" {
		return nil, fmt.Errorf("skill description is required in frontmatter")
	}

	return skillDefinitionFromMetadata(meta, body, "")
}

// Names returns all registered skill names
func (idx *SkillIndex) Names() []string {
	names := make([]string, 0, len(idx.skills))
	for name := range idx.skills {
		names = append(names, name)
	}
	return names
}

// All returns all skill definitions
func (idx *SkillIndex) All() map[string]*SkillDefinition {
	return idx.skills
}

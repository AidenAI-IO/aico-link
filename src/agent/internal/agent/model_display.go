package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	_ "embed"

	"aico-agent/internal/agent/model"
	"gopkg.in/yaml.v3"
)

//go:embed model_catalog.yaml
var embeddedModelCatalogYAML []byte

// ModelDisplayInfo represents UI display information for a model.
// The ID serves as both the model identifier and the display name to reduce
// cognitive overhead. Only descriptions are localized.
type ModelDisplayInfo struct {
	ID           string            `json:"id"`           // Model ID, also used as display name
	Descriptions map[string]string `json:"descriptions"` // locale -> description (localeEnglishUS, localeSimplifiedChinese)
	Recommended  bool              `json:"recommended"`  // Whether this model is recommended for the provider
}

// GetDescription returns the localized description for the given locale.
// Falls back to English if the requested locale is not available.
func (m ModelDisplayInfo) GetDescription(locale string) string {
	if desc, ok := m.Descriptions[locale]; ok && desc != "" {
		return desc
	}
	if desc, ok := m.Descriptions[localeEnglishUS]; ok {
		return desc
	}
	return ""
}

// LocalizedModelInfo represents a model's display information after localization.
type LocalizedModelInfo struct {
	ID          string           `json:"id"`
	Description string           `json:"description"`
	Recommended bool             `json:"recommended"`
	Spec        *model.ModelSpec `json:"spec,omitempty"`
}

// Localized returns the model information localized for the given locale.
func (m ModelDisplayInfo) Localized(locale string) LocalizedModelInfo {
	var spec *model.ModelSpec
	if resolved, ok := LookupModelSpec("", m.ID); ok {
		resolved.Provider = ""
		resolved.Name = m.ID
		spec = &resolved
	}
	return LocalizedModelInfo{
		ID:          m.ID,
		Description: m.GetDescription(locale),
		Recommended: m.Recommended,
		Spec:        spec,
	}
}

type modelCatalogFile struct {
	Providers map[string]modelCatalogProvider `yaml:"providers"`
}

type modelCatalogProvider struct {
	Default string              `yaml:"default"`
	Models  []modelCatalogModel `yaml:"models"`
}

type modelCatalogModel struct {
	ID           string            `yaml:"id"`
	Recommended  bool              `yaml:"recommended"`
	Descriptions map[string]string `yaml:"descriptions"`
}

type providerModelCatalog struct {
	Default string
	Models  []ModelDisplayInfo
}

var (
	modelCatalogOnce sync.Once
	modelCatalogData map[string]providerModelCatalog
	modelCatalogErr  error
)

func loadModelCatalog() (map[string]providerModelCatalog, error) {
	modelCatalogOnce.Do(func() {
		modelCatalogData, modelCatalogErr = parseModelCatalogYAML(embeddedModelCatalogYAML)
	})
	return modelCatalogData, modelCatalogErr
}

func parseModelCatalogYAML(raw []byte) (map[string]providerModelCatalog, error) {
	var file modelCatalogFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse model catalog: %w", err)
	}
	if len(file.Providers) == 0 {
		return nil, fmt.Errorf("model catalog has no providers")
	}
	out := make(map[string]providerModelCatalog, len(file.Providers))
	for provider, entry := range file.Providers {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			return nil, fmt.Errorf("model catalog has an empty provider key")
		}
		if len(entry.Models) == 0 {
			return nil, fmt.Errorf("model catalog provider %q has no models", key)
		}
		seen := make(map[string]struct{}, len(entry.Models))
		models := make([]ModelDisplayInfo, 0, len(entry.Models))
		var hasRecommended bool
		for _, item := range entry.Models {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				return nil, fmt.Errorf("model catalog provider %q has a model with empty id", key)
			}
			if _, ok := seen[id]; ok {
				return nil, fmt.Errorf("model catalog provider %q has duplicate model %q", key, id)
			}
			seen[id] = struct{}{}
			if item.Recommended {
				hasRecommended = true
			}
			models = append(models, ModelDisplayInfo{
				ID:           id,
				Recommended:  item.Recommended,
				Descriptions: item.Descriptions,
			})
		}
		if !hasRecommended {
			return nil, fmt.Errorf("model catalog provider %q has no recommended model", key)
		}
		def := strings.TrimSpace(entry.Default)
		if def == "" {
			return nil, fmt.Errorf("model catalog provider %q is missing default", key)
		}
		if _, ok := seen[def]; !ok {
			return nil, fmt.Errorf("model catalog provider %q default %q is not in models", key, def)
		}
		out[key] = providerModelCatalog{Default: def, Models: models}
	}
	return out, nil
}

func providerCatalog(providerType string) (providerModelCatalog, bool) {
	catalog, err := loadModelCatalog()
	if err != nil || catalog == nil {
		return providerModelCatalog{}, false
	}
	entry, ok := catalog[strings.ToLower(strings.TrimSpace(providerType))]
	return entry, ok
}

// GetDisplayModelsForProvider returns the list of models available for the given provider.
// Recommended models are ordered first, with the provider default at the front.
// Returns an empty slice if the provider is not found.
func GetDisplayModelsForProvider(providerType string) []ModelDisplayInfo {
	entry, ok := providerCatalog(providerType)
	if !ok {
		return []ModelDisplayInfo{}
	}
	models := append([]ModelDisplayInfo(nil), entry.Models...)
	sortProviderModels(models, entry.Default)
	return models
}

// GetDefaultModelForProvider returns the catalog default model id for a provider.
func GetDefaultModelForProvider(providerType string) string {
	entry, ok := providerCatalog(providerType)
	if !ok {
		return ""
	}
	return entry.Default
}

func sortProviderModels(models []ModelDisplayInfo, defaultID string) {
	rank := func(m ModelDisplayInfo) int {
		if m.ID == defaultID {
			return 0
		}
		if m.Recommended {
			return 1
		}
		return 2
	}
	sort.SliceStable(models, func(i, j int) bool {
		ri, rj := rank(models[i]), rank(models[j])
		if ri != rj {
			return ri < rj
		}
		return false
	})
}

// GetLocalizedModelsForProvider returns the localized model list for the given provider and locale.
func GetLocalizedModelsForProvider(providerType, locale string) []LocalizedModelInfo {
	models := GetDisplayModelsForProvider(providerType)
	result := make([]LocalizedModelInfo, len(models))
	for i, m := range models {
		result[i] = m.Localized(locale)
		// Some registry entries only exist under a provider-qualified key (for
		// example openai/gpt-4o). Resolve again with the selected provider so the
		// config UI receives explicit reasoning support, including "unsupported".
		if resolved, ok := LookupModelSpec(providerType, m.ID); ok {
			resolved.Provider = normalizedProviderName(providerType)
			resolved.Name = m.ID
			result[i].Spec = &resolved
		}
		if result[i].Spec != nil {
			result[i].Spec.Provider = normalizedProviderName(providerType)
			result[i].Spec.API = modelAPIEndpoint(providerType, "")
			result[i].Spec.APIShape = modelAPIShape(providerType, "")
		}
	}
	return result
}

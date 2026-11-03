package configweb

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/agent"
)

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	if provider == "" {
		writeJSONError(w, http.StatusBadRequest, "provider parameter is required")
		return
	}
	runtimeConfig, configErr := agent.LoadRuntimeConfig(s.options.AgentConfigPath)
	locale := strings.TrimSpace(r.URL.Query().Get("locale"))
	if locale == "" {
		locale = "en-US"
		if configErr == nil {
			locale = runtimeConfig.LocaleOrDefault()
		}
	}
	response := map[string]any{
		"provider": provider,
		"models":   agent.GetLocalizedModelsForProvider(provider, locale),
	}
	if modelName := strings.TrimSpace(r.URL.Query().Get("model")); modelName != "" {
		spec, _ := agent.LookupModelSpec(provider, modelName)
		if configErr == nil {
			manager := agent.NewModelManager(runtimeConfig.Model, agent.ProxyConfigFromEnvironment(),
				agent.WithProviderModelMetadataCachePath(filepath.Join(filepath.Dir(s.options.AgentConfigPath), "cache", "provider_model_metadata.json")))
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			spec = manager.SpecForModel(ctx, provider, modelName)
			cancel()
		}
		if spec.Provider != "" || spec.Name != "" || spec.Reasoning != nil || spec.ContextWindow > 0 || spec.MaxOutput > 0 {
			response["spec"] = spec
		}
	}
	writeJSON(w, http.StatusOK, response)
}

package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
)

const OpenRouterModelsURL = "https://openrouter.ai/api/v1/models?output_modalities=decisions&order=top-weekly"

// ModelInfo is a native Decisions model from OpenRouter's public catalog.
type ModelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListOpenRouterModels fetches native Decisions models without sending a key.
// Extractions-only entries share the decisions modality but cannot answer the
// workflow questions, so they are excluded. Preserve the catalog's order.
func ListOpenRouterModels(ctx context.Context, client *http.Client) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenRouter model catalog returned %d", resp.StatusCode)
	}
	var catalog struct {
		Data []struct {
			ModelInfo
			Architecture struct {
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			Endpoints []string `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return nil, err
	}
	models := make([]ModelInfo, 0, len(catalog.Data))
	for _, entry := range catalog.Data {
		if entry.ID != "" && slices.Contains(entry.Architecture.OutputModalities, "decisions") &&
			slices.Contains(entry.Endpoints, "/api/alpha/decisions") {
			models = append(models, entry.ModelInfo)
		}
	}
	return models, nil
}

package configupdate

import (
	"encoding/json"
	"fmt"

	"aico-agent/internal/agent"
)

// Snapshot returns the resolved config at path in the Config Web JSON shape,
// so the Agent's config tools read the same values the settings page shows.
func (s *Service) Snapshot(path string) (json.RawMessage, error) {
	cfg, err := agent.LoadResolvedConfigForUpdate(path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return json.Marshal(FromAgentConfig(cfg))
}

// Edit implements agent.ConfigEditor on top of Plan and Update, so the Agent's
// own changes pass the same validation and atomic write as Config Web saves.
func (s *Service) Edit(path string, patch json.RawMessage, commit bool) (agent.ConfigEdit, error) {
	result, err := s.update(path, patch, commit)
	if err != nil {
		return agent.ConfigEdit{}, err
	}
	config, err := json.Marshal(result.Config)
	if err != nil {
		return agent.ConfigEdit{}, err
	}
	return agent.ConfigEdit{
		Config:       config,
		ChangedPaths: result.ChangedPaths,
		Apply:        result.Apply,
		ApplyReasons: result.ApplyReasons,
		Revision:     result.Revision,
	}, nil
}

var _ agent.ConfigEditor = (*Service)(nil)

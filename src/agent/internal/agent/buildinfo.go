package agent

import (
	"encoding/json"
	"os"
	"strings"
)

const defaultOTAStatePath = "/userdata/ota/state.json"

var (
	buildCommit  = "unknown"
	buildVersion = "dev"
)

func AgentCommitID() string {
	if v := strings.TrimSpace(buildCommit); v != "" {
		return v
	}
	return "unknown"
}

func AgentBuildVersion() string {
	if v := strings.TrimSpace(buildVersion); v != "" {
		return v
	}
	return "dev"
}

func FirmwareVersion() string {
	return readOTAStateField(otaStatePath(), "current_version")
}

func otaStatePath() string {
	if v := strings.TrimSpace(os.Getenv("AICO_OTA_STATE_PATH")); v != "" {
		return v
	}
	return defaultOTAStatePath
}

func readOTAStateField(path, field string) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	var state map[string]json.RawMessage
	if err := json.Unmarshal(data, &state); err != nil {
		return ""
	}
	raw, ok := state[field]
	if !ok || len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func telemetryModelInfo(cfg Config) (provider, model string) {
	return strings.TrimSpace(cfg.Model.Provider), strings.TrimSpace(cfg.Model.Model)
}

func telemetryVersionInfo() (commitID, buildVersion, firmwareVersion string) {
	commitID = AgentCommitID()
	buildVersion = AgentBuildVersion()
	firmwareVersion = FirmwareVersion()
	if firmwareVersion == "" {
		return commitID, buildVersion, firmwareVersion
	}
	if commitID == "" || commitID == "unknown" {
		if idx := strings.LastIndex(firmwareVersion, "-"); idx >= 0 && idx+1 < len(firmwareVersion) {
			suffix := firmwareVersion[idx+1:]
			if len(suffix) >= 7 && len(suffix) <= 40 {
				commitID = suffix
			}
		}
	}
	return commitID, buildVersion, firmwareVersion
}

func enrichEpisodeTelemetry(episode *TaskEpisode, cfg Config) {
	if episode == nil {
		return
	}
	if episode.Extra == nil {
		episode.Extra = map[string]interface{}{}
	}
	provider, model := telemetryModelInfo(cfg)
	if provider != "" {
		episode.Extra["model_provider"] = provider
	}
	if model != "" {
		episode.Extra["model_name"] = model
	}
	if provider != "" && model != "" {
		episode.Extra["model"] = provider + "/" + model
	}
	commitID, buildVersion, firmwareVersion := telemetryVersionInfo()
	if commitID != "" && commitID != "unknown" {
		episode.Extra["agent_commit"] = commitID
	}
	if buildVersion != "" && buildVersion != "dev" {
		episode.Extra["agent_build"] = buildVersion
	}
	if firmwareVersion != "" {
		episode.Extra["firmware_version"] = firmwareVersion
	}
}

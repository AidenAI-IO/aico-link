package main

import "aico-agent/internal/configupdate"

type configUpdateResult = configupdate.Result

const hasAPIKeyPlaceholder = "***"

func updateConfigFile(path string, patch []byte) (configUpdateResult, error) {
	return configupdate.NewService().Update(path, patch)
}

func boolPtr(value bool) *bool {
	return &value
}

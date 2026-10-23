package agent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
)

func withTestConfigDir(t *testing.T, cfg Config) Config {
	t.Helper()
	if strings.TrimSpace(cfg.ConfigDir) == "" {
		cfg.ConfigDir = ensureTestConfigDir(t, t.TempDir())
	} else {
		cfg.ConfigDir = ensureTestConfigDir(t, cfg.ConfigDir)
	}
	return cfg
}

func ensureTestConfigDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(agentpath.ContextManagerSessionFolder(dir), 0o755); err != nil {
		t.Fatalf("MkdirAll sessions dir: %v", err)
	}
	return dir
}

func freshNewContextManager(systemPrompt, userInput string, attachments []InputAttachment, sessionFolder string) (*contextmanager.ContextManager, error) {
	manager, err := contextmanager.NewContextManager(sessionFolder, systemPrompt)
	if err != nil {
		return nil, err
	}
	if err := manager.AppendMessage(userMessageFromInput(manager, userInput, attachments)); err != nil {
		return nil, err
	}
	return manager, nil
}

func messageText(messages []llms.MessageContent) string {
	var builder strings.Builder
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if text, ok := part.(llms.TextContent); ok {
				builder.WriteString(text.Text)
				builder.WriteByte('\n')
			}
		}
	}
	return builder.String()
}

func intPtr(v int) *int { return &v }

func testScreenshotObservationStep(tool string, data []byte) schema.AgentStep {
	observation, _ := json.Marshal(postActionScreenshotResult{
		screenshotResult: screenshotResult{
			Width:  320,
			Height: 240,
			Format: "jpeg",
			Size:   len(data),
			Data:   base64.StdEncoding.EncodeToString(data),
		},
	})
	return schema.AgentStep{
		Action:      schema.AgentAction{Tool: tool},
		Observation: string(observation),
	}
}

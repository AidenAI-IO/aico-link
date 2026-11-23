package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/schema"
)

func TestEpisodeRecorderRecordsStructuredToolError(t *testing.T) {
	recorder := NewEpisodeRecorder(MemoryRetrieveRequest{}, MemoryContext{})
	action := schema.AgentAction{Tool: "non_sensitive_tool", ToolInput: `{"action":"query"}`}
	step := schema.AgentStep{Action: action, Observation: "contacts permission denied"}
	toolErr := NewToolError(CodePermissionDenied, "contacts permission denied")

	recorder.RecordExecution(ToolCallExecutionResult{
		Call:   ToolCall{Action: action},
		Step:   step,
		Result: ToolResult{Output: toolErr.Message, Error: toolErr},
	})
	episode := recorder.Finish("", nil, nil, nil, nil)

	for _, event := range episode.Events {
		if event.Type != "tool_result" {
			continue
		}
		if event.ToolError == nil || event.ToolError.Code != CodePermissionDenied {
			t.Fatalf("TaskEpisodeEvent.ToolError = %+v, want permission_denied", event.ToolError)
		}
		if !event.IsError {
			t.Fatalf("TaskEpisodeEvent.IsError = false, want true")
		}
		if event.ToolError.Message != "contacts permission denied" {
			t.Fatalf("TaskEpisodeEvent.ToolError.Message = %q", event.ToolError.Message)
		}
		return
	}
	t.Fatal("missing tool_result event")
}

func TestEpisodeRecorderDoesNotPersistSensitiveToolResultContent(t *testing.T) {
	for _, toolName := range []string{toolBridgeClipboard, "shell"} {
		t.Run(toolName, func(t *testing.T) { testEpisodeRedactsSensitiveToolResult(t, toolName) })
	}
}

func testEpisodeRedactsSensitiveToolResult(t *testing.T, toolName string) {
	t.Helper()
	root := t.TempDir()
	recorder := NewPersistentEpisodeRecorder(
		MemoryRetrieveRequest{Input: "read clipboard"},
		MemoryContext{},
		NewTaskEpisodeStore(root),
	)
	secret := "SENSITIVE_EPISODE_MARKER_8f2d"
	action := schema.AgentAction{Tool: toolName, ToolInput: `{"action":"read"}`}
	var deviceCalls []deviceCLIToolEvidence
	if toolName == "shell" {
		action.ToolInput = `{"command":"device-cli app clipboard read"}`
		deviceCalls = []deviceCLIToolEvidence{{Tool: toolBridgeClipboard, Input: map[string]any{"action": "read"}}}
	}
	recorder.RecordExecution(ToolCallExecutionResult{
		Call: ToolCall{
			Spec:   ToolSpec{Name: toolName},
			Action: action,
		},
		Step: schema.AgentStep{Action: action, Observation: secret},
		Result: ToolResult{
			Output:      secret,
			deviceCalls: deviceCalls,
			Summary:     secret,
			Error: NewToolErrorWithDetails(
				CodeToolExecutionFailed,
				secret,
				map[string]any{"raw": secret},
			),
		},
	})
	episode := recorder.Finish("", nil, nil, nil, nil)

	foundRedactedResult := false
	for _, event := range episode.Events {
		if event.Type != "tool_result" {
			continue
		}
		foundRedactedResult = true
		if event.Content != sensitiveToolResultRedaction || event.RawObservation != "" {
			t.Fatalf("sensitive tool result event = %#v", event)
		}
		if event.ToolError == nil || event.ToolError.Message != sensitiveToolResultRedaction || len(event.ToolError.Details) != 0 {
			t.Fatalf("sensitive tool error = %#v", event.ToolError)
		}
	}
	if !foundRedactedResult {
		t.Fatal("missing sensitive tool_result event")
	}

	filesScanned := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		filesScanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), secret) {
			t.Fatalf("sensitive marker persisted in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk episode store: %v", err)
	}
	if filesScanned == 0 {
		t.Fatal("expected persisted episode files")
	}
}

func TestMaterializeEventArtifactUsesExistingReference(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	dir := filepath.Join(t.TempDir(), "episode")
	ref := "artifacts/step_002.jpeg"
	event := TaskEpisodeEvent{
		RawObservation: `{"width":320,"height":240,"format":"jpeg","size":10,"screenshot_ref":"` + ref + `"}`,
	}

	if err := store.materializeEventArtifact(dir, &event, 1); err != nil {
		t.Fatalf("materialize referenced artifact: %v", err)
	}
	if event.ScreenshotRef != ref {
		t.Fatalf("screenshot_ref = %q, want %q", event.ScreenshotRef, ref)
	}
	if !strings.Contains(event.Observation, `"screenshot_ref":"`+ref+`"`) {
		t.Fatalf("compact observation is missing reference: %s", event.Observation)
	}
}

func TestReadEpisodeEventsRepairsNULTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	valid := "{\"event_id\":\"one\",\"type\":\"tool_call\"}\n{\"event_id\":\"two\",\"type\":\"tool_result\"}\n"
	data := append([]byte(valid), make([]byte, 128)...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	events, err := readEpisodeEvents(path)
	if err != nil {
		t.Fatalf("readEpisodeEvents() error = %v", err)
	}
	if len(events) != 2 || events[0].EventID != "one" || events[1].EventID != "two" {
		t.Fatalf("events = %#v", events)
	}
	repaired, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired events: %v", err)
	}
	if string(repaired) != valid {
		t.Fatalf("repaired events = %q, want %q", repaired, valid)
	}
}

func TestReadEpisodeEventsRejectsNULBeforeNonzeroData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := []byte("{\"event_id\":\"one\",\"type\":\"tool_call\"}\n\x00corrupt\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	if _, err := readEpisodeEvents(path); err == nil || !strings.Contains(err.Error(), "invalid NUL byte") {
		t.Fatalf("readEpisodeEvents() error = %v, want invalid NUL byte", err)
	}
}

func TestReadEpisodeEventsRejectsNULLineBeforeLaterRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	data := []byte("{\"event_id\":\"one\",\"type\":\"tool_call\"}\n\x00\x00\n{\"event_id\":\"two\",\"type\":\"tool_result\"}\n")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}

	if _, err := readEpisodeEvents(path); err == nil || !strings.Contains(err.Error(), "invalid data after truncated episode event tail") {
		t.Fatalf("readEpisodeEvents() error = %v, want invalid trailing data", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events after rejection: %v", err)
	}
	if string(unchanged) != string(data) {
		t.Fatalf("events were rewritten after rejected repair: %q", unchanged)
	}
}

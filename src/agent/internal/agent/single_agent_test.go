package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestSingleAgentProfileDoesNotBuildDelegatedRoles(t *testing.T) {
	index := NewSkillIndex()
	index.skills["ui"] = &SkillDefinition{Name: "ui", Description: "Inspect first"}
	profile := buildProfile(
		AgentConfig{Instruction: "base", Prompt: "extra"},
		NewSkillManager(index),
		[]langtools.Tool{&stubTool{name: "screenshot", description: "Capture screen."}},
		agentRoleRules(),
	)

	for _, want := range []string{"base", "extra", "- ui: Inspect first"} {
		if !strings.Contains(profile.SystemPrompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, profile.SystemPrompt)
		}
	}
	for _, unexpected := range []string{"enter_plan_mode", "commit_plan", "cancel_plan"} {
		if strings.Contains(profile.SystemPrompt, unexpected) {
			t.Fatalf("agent prompt should not contain old delegated role wording %q:\n%s", unexpected, profile.SystemPrompt)
		}
	}
}

func TestSingleAgentRuntimeUsesAgentRoleAndDirectTools(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("audio_volume", `{"__arg1":"{\"volume\":3}"}`, "Volume set to 3."),
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get or set audio playback volume.",
		output:      `{"volume":3}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use direct tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"audio_volume": tool}},
		NewSkillIndex(),
	)

	var events []RunEvent
	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "set volume to 3",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "Volume set to 3." {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(model.tools) != 2 {
		t.Fatalf("expected tool call turn and final turn, got %d model calls", len(model.tools))
	}
	if !singleAgentLLMToolsContain(model.tools[0], "audio_volume") {
		t.Fatalf("agent did not receive audio_volume tool: %#v", model.tools[0])
	}
	for _, name := range []string{"enter_plan_mode", "commit_plan", "cancel_plan", "finish_step", "abort_step"} {
		if singleAgentLLMToolsContain(model.tools[0], name) {
			t.Fatalf("single-agent turn exposed delegated meta tool %q: %#v", name, model.tools[0])
		}
	}
	if prompt := messageText(model.messages[0]); strings.Contains(prompt, "Planner runtime context") || strings.Contains(prompt, "force_simple_loop") {
		t.Fatalf("agent prompt leaked old runtime context:\n%s", prompt)
	}
	for _, event := range events {
		if event.Type == "role_output" && event.Role != "agent" {
			t.Fatalf("role_output role = %q, want %q", event.Role, "agent")
		}
	}
}

func TestSingleAgentDoesNotRunDefaultFinalVerifierReview(t *testing.T) {
	model := &scriptedModel{responses: roleToolResponses("screenshot", `{"__arg1":"{}"}`, "screen checked")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"screenshot": &stubTool{
				name:        "screenshot",
				description: "Capture screen.",
				visual:      true,
				output:      `{"format":"jpeg","width":1,"height":1,"size":1}`,
			},
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "check screen"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "screen checked" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model calls = %d, want tool turn plus final answer only", model.callCount)
	}
}

func TestSingleAgentOpenAppRoutesInternallyWhenBridgeDisconnected(t *testing.T) {
	model := &routedOpenAppModel{}
	events := make([]string, 0, 4)
	bridge := NewPhoneBridge(nil)
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.returnEntry = "dynamic_island"
	bridge.returnEntrySeen = true
	bridge.returnEntryOK = true
	bridge.mu.Unlock()
	stateScreenshot := &stubTool{
		name:        "screenshot",
		description: "Capture the current phone screen.",
		visual:      true,
	}
	stateScreenshot.callFn = func(context.Context, string) (string, error) {
		events = append(events, "state screenshot")
		return `{"format":"jpeg","width":1,"height":1,"size":1,"data":"YQ=="}`, nil
	}
	postActionScreenshot := &stubTool{name: "screenshot"}
	postActionScreenshot.callFn = func(context.Context, string) (string, error) {
		if len(postActionScreenshot.inputs) == 1 {
			events = append(events, "pre-action baseline")
		} else {
			events = append(events, "post-action final")
		}
		return `{"format":"jpeg","width":1,"height":1,"size":1,"data":"YQ=="}`, nil
	}
	searchLaunch := &stubTool{
		name:        "search_launch_app",
		description: "Open an app through visible system search.",
	}
	searchLaunch.callFn = func(context.Context, string) (string, error) {
		events = append(events, "routed open_app")
		return `{"ok":true,"opened":true}`, nil
	}
	toolSet := &ToolSet{
		phoneBridge: bridge,
		tools: map[string]langtools.Tool{
			"open_app":            newPostActionScreenshotTool(NewOpenAppTool(bridge, nil, searchLaunch), postActionScreenshot, 0),
			"request_user_action": NewHumanHandoffTool(),
			"screenshot":          stateScreenshot,
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Operate the connected phone."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		toolSet,
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "打开小红书"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "已打开小红书。" {
		t.Fatalf("Output = %q, want routed completion", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model calls = %d, want open_app plus completion", model.callCount)
	}
	if len(stateScreenshot.inputs) != 1 || len(postActionScreenshot.inputs) != 2 || len(searchLaunch.inputs) != 1 {
		t.Fatalf("routed calls: state_screenshot=%v post_action_screenshot=%v search_launch_app=%v", stateScreenshot.inputs, postActionScreenshot.inputs, searchLaunch.inputs)
	}
	if want := []string{"state screenshot", "pre-action baseline", "routed open_app", "post-action final"}; !slices.Equal(events, want) {
		t.Fatalf("routed call order = %#v, want %#v", events, want)
	}
	var searchInput map[string]any
	if err := json.Unmarshal([]byte(searchLaunch.inputs[0]), &searchInput); err != nil {
		t.Fatalf("decode search_launch_app input: %v", err)
	}
	if searchInput["app"] != "小红书" {
		t.Fatalf("search_launch_app app = %#v, want 小红书", searchInput["app"])
	}
	if _, ok := searchInput["platform"]; ok {
		t.Fatalf("search_launch_app input = %#v, want platform omitted", searchInput)
	}
}

type routedOpenAppModel struct {
	callCount int
}

func (m *routedOpenAppModel) GenerateContent(_ context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.callCount++
	switch m.callCount {
	case 1:
		return toolCallResponse("call_open", "open_app", `{"app":"小红书","platform":"ios"}`), nil
	case 2:
		if modelMessagesContainSuccessfulToolResult(messages, "open_app") && modelMessagesContainToolScreenshotObservation(messages, "open_app") {
			return contentResponse("已打开小红书。"), nil
		}
	}
	return contentResponse("未完成。"), nil
}

func (m *routedOpenAppModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

func modelMessagesContainSuccessfulToolResult(messages []llms.MessageContent, name string) bool {
	for _, message := range messages {
		for _, part := range message.Parts {
			result, ok := part.(llms.ToolCallResponse)
			if !ok || result.Name != name {
				continue
			}
			if strings.Contains(result.Content, `"ok":true`) || strings.Contains(result.Content, `\"ok\":true`) {
				return true
			}
		}
	}
	return false
}

func modelMessagesContainToolScreenshotObservation(messages []llms.MessageContent, name string) bool {
	caption := "screenshot observation returned by the " + name + " tool"
	for _, message := range messages {
		hasCaption := false
		hasImage := false
		for _, part := range message.Parts {
			switch typed := part.(type) {
			case llms.TextContent:
				hasCaption = hasCaption || strings.Contains(typed.Text, caption)
			case llms.BinaryContent:
				hasImage = len(typed.Data) > 0
			case llms.ImageURLContent:
				hasImage = strings.TrimSpace(typed.URL) != ""
			}
		}
		if hasCaption && hasImage {
			return true
		}
	}
	return false
}

func singleAgentLLMToolsContain(tools []llms.Tool, name string) bool {
	for _, tool := range tools {
		if tool.Function != nil && tool.Function.Name == name {
			return true
		}
	}
	return false
}

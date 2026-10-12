package agent

import (
	"aiden-agent/internal/agent/messages"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/compactor"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/model"
	speechtext "aiden-agent/internal/agent/speech"
	"aiden-agent/internal/agent/tokencounter"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	fakellm "github.com/tmc/langchaingo/llms/fake"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestEffectiveMaxIterationsDefaultsAndUnlimited(t *testing.T) {
	if got := effectiveMaxIterations(-1); got != math.MaxInt {
		t.Fatalf("effectiveMaxIterations(-1) = %d, want math.MaxInt", got)
	}
	if got := effectiveMaxIterations(0); got != math.MaxInt {
		t.Fatalf("effectiveMaxIterations(0) = %d, want math.MaxInt", got)
	}
	if got := effectiveMaxIterations(10); got != 10 {
		t.Fatalf("effectiveMaxIterations(10) = %d, want 10", got)
	}
}

func TestRuntimeRejectsBackendRunWhenBackendAgentDisabled(t *testing.T) {
	runtime := &Runtime{config: Config{
		InputMode:  "realtime",
		VoiceModel: VoiceModelConfig{},
	}}

	_, err := runtime.Run(context.Background(), RunRequest{Input: "handled by realtime session"})
	if err == nil || !strings.Contains(err.Error(), "backend agent is disabled for this realtime session") {
		t.Fatalf("Run() error = %v, want backend-disabled realtime guard", err)
	}
}

func TestRuntimeDoesNotRegisterArtifactReadTool(t *testing.T) {
	toolSet := &ToolSet{tools: map[string]langtools.Tool{}}
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: t.TempDir()},
		nil,
		nil,
		toolSet,
		NewSkillIndex(),
	)
	if runtime == nil {
		t.Fatal("NewRuntimeWithDeps() returned nil")
	}
	if registered, ok := toolSet.Get("artifact_read"); ok {
		t.Fatalf("artifact_read tool is still registered: %T", registered)
	}
}

func TestRuntimeUsesConfiguredTerminationPolicy(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		{
			Choices: []*llms.ContentChoice{{
				ToolCalls: []llms.ToolCall{{ID: "invalid-tool-call"}},
			}},
		},
		contentResponse("should not be reached"),
	}}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:             ModelConfig{Provider: "fake"},
			Instruction:       "Answer directly.",
			TerminationPolicy: TerminationPolicyConfig{ParseFailureLimit: 1},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "test configured parse limit"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(result.Output, string(StopReasonParseFailure)) {
		t.Fatalf("output = %q, want configured parse-failure termination", result.Output)
	}
	if model.callCount != 1 {
		t.Fatalf("model call count = %d, want 1", model.callCount)
	}
}

type testModelResolver struct {
	model llms.Model
	err   error
	calls int
	spec  model.ModelSpec
}

func (r *testModelResolver) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.model.GenerateContent(ctx, messages, options...)
}

func (r *testModelResolver) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	return r.model.Call(ctx, prompt, options...)
}

func (r *testModelResolver) CallOptions() []chains.ChainCallOption {
	return nil
}

func (r *testModelResolver) Spec() model.ModelSpec {
	if r.spec.ContextWindow > 0 || r.spec.MaxOutput > 0 {
		return r.spec
	}
	if provider, ok := r.model.(interface{ Spec() model.ModelSpec }); ok {
		if spec := provider.Spec(); spec.ContextWindow > 0 || spec.MaxOutput > 0 {
			return spec
		}
	}
	return model.ModelSpec{
		Provider:      "fake",
		Name:          "test",
		ContextWindow: 1_000_000,
	}
}

func TestRuntimeRun(t *testing.T) {
	cfg := Config{
		Model:       ModelConfig{Provider: "fake"},
		Instruction: "Answer directly.",
	}
	cfg = withTestConfigDir(t, cfg)

	resolver := &testModelResolver{
		model: &scriptedModel{responses: roleDirectResponses("completed")},
	}

	runtime := NewRuntimeWithDeps(cfg, resolver, NewMemoryManager(""), NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}), NewSkillIndex())
	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "completed" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
}

func TestRuntimeParseFallbackDoesNotPersistInterruptNotice(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1},
		&testModelResolver{model: failingGenerateModel{err: fmt.Errorf("%w: raw model response", agents.ErrUnableToParseOutput)}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	defer runtime.Close()

	result, err := runtime.Run(context.Background(), RunRequest{Input: "parse fallback"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "raw model response" {
		t.Fatalf("Run() output = %q, want raw model response", result.Output)
	}
	for _, message := range runtime.contextManager.CloneMessageList() {
		if message.Role == messages.MessageRoleNotice && strings.HasPrefix(message.Content, "Interrupt [") {
			t.Fatalf("recoverable parse fallback persisted interruption notice: %q", message.Content)
		}
	}
	if _, err := os.Stat(filepath.Join(runtime.contextManager.GetSessionFolder(), pendingBackendRunFile)); !os.IsNotExist(err) {
		t.Fatalf("pending run journal remains after parse fallback: %v", err)
	}
}

func TestRuntimeRecordsOnDemandDeviceMemoryRecallThroughAgentToolCall(t *testing.T) {
	ctx := context.Background()
	configDir := ensureTestConfigDir(t, t.TempDir())
	memoryDir := filepath.Join(configDir, "memory")
	deviceStore := NewDeviceMemoryStore(filepath.Join(memoryDir, "device"))
	if _, err := deviceStore.Upsert(ctx, DeviceMemoryItem{
		ID:         "devmem_qa_notes_guard",
		Type:       "failure",
		Status:     "active",
		Title:      "Verify edited title before Save",
		Content:    "Before clicking Save in QA Notes, verify that the title field shows the new value.",
		Summary:    "Verify the new QA Notes title before Save.",
		DeviceID:   defaultMemoryDeviceID,
		Tags:       []string{legacyReflectionFailureTag, "save-action"},
		Entities:   []string{"QA Notes", "Save button"},
		Confidence: 0.8,
		Priority:   80,
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	if _, err := deviceStore.Upsert(ctx, DeviceMemoryItem{
		ID:         "devmem_unrelated_procedure",
		Type:       "procedure",
		Status:     "active",
		Title:      "Unrelated controller procedure",
		Content:    "Use shell to inspect the controller clock before answering a time question.",
		DeviceID:   defaultMemoryDeviceID,
		Confidence: 0.8,
		Priority:   70,
		Steps:      []ProcedureStep{{Tool: "shell"}},
	}); err != nil {
		t.Fatalf("Upsert unrelated procedure error = %v", err)
	}

	model := &scriptedModel{responses: roleDefaultToolResponses(
		"recall_device_memory",
		`{"terms":["QA Notes","Save"],"types":["failure"],"device_id":"default","limit":5}`,
		"Verify the new title before saving.",
	)}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 3,
		},
		&testModelResolver{model: model},
		NewMemoryManager(memoryDir),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	t.Cleanup(func() { _ = runtime.Close() })

	result, err := runtime.Run(ctx, RunRequest{Input: "在 QA Notes 点击 Save 前，如何避免保存旧标题？"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.toolChoices) < 2 {
		t.Fatalf("tool choices = %#v, want recall turn and answer turn", model.toolChoices)
	}
	for i, choice := range model.toolChoices[:2] {
		if choice != nil {
			t.Fatalf("tool choice %d = %#v, want model-controlled tool selection", i, choice)
		}
	}
	if len(model.messages) < 2 {
		t.Fatalf("model messages = %#v, want post-recall turn", model.messages)
	}
	contextArgs := ""
	for _, message := range model.messages[1] {
		for _, part := range message.Parts {
			call, ok := part.(llms.ToolCall)
			if !ok || call.FunctionCall == nil || call.FunctionCall.Name != "recall_device_memory" {
				continue
			}
			contextArgs = call.FunctionCall.Arguments
		}
	}
	if !strings.Contains(contextArgs, `"types":["failure"]`) || !strings.Contains(contextArgs, "QA Notes") || strings.Contains(contextArgs, "devmem_qa_notes_guard") {
		t.Fatalf("post-recall context arguments = %q, want the model-provided query unchanged", contextArgs)
	}

	episode, err := NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes")).Get(ctx, result.EpisodeID)
	if err != nil {
		t.Fatalf("Get episode: %v", err)
	}
	if !slices.Equal(episode.RetrievedMemoryRefs, []string{"devmem_qa_notes_guard"}) {
		t.Fatalf("retrieved_memory_refs = %#v, want only relevant device memory", episode.RetrievedMemoryRefs)
	}
	foundRecall := false
	for _, event := range episode.Events {
		if event.Type == runEventToolCall && event.ToolName == "recall_device_memory" {
			foundRecall = true
			if !strings.Contains(event.ToolInput, `"types":["failure"]`) || !strings.Contains(event.ToolInput, "QA Notes") || strings.Contains(event.ToolInput, "devmem_qa_notes_guard") {
				t.Fatalf("recall_device_memory tool input = %q, want the model-provided query unchanged", event.ToolInput)
			}
			break
		}
	}
	if !foundRecall {
		t.Fatalf("episode missing recall_device_memory tool call: %#v", episode.Events)
	}
}

func TestRuntimeDoesNotForceDeviceMemoryRecallForRelevantQuestion(t *testing.T) {
	ctx := context.Background()
	configDir := ensureTestConfigDir(t, t.TempDir())
	memoryDir := filepath.Join(configDir, "memory")
	deviceStore := NewDeviceMemoryStore(filepath.Join(memoryDir, "device"))
	if _, err := deviceStore.Upsert(ctx, DeviceMemoryItem{
		ID:         "devmem_qa_notes_guard",
		Type:       "failure",
		Status:     "active",
		Title:      "Verify edited title before Save",
		Content:    "Before clicking Save in QA Notes, verify that the title field shows the new value.",
		DeviceID:   defaultMemoryDeviceID,
		Tags:       []string{legacyReflectionFailureTag, "save-action"},
		Entities:   []string{"QA Notes", "Save button"},
		Confidence: 0.8,
		Priority:   80,
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	model := &scriptedModel{responses: roleDirectResponses("Use Preview before returning to Edit and saving.")}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{model: model},
		NewMemoryManager(memoryDir),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	t.Cleanup(func() { _ = runtime.Close() })

	if _, err := runtime.Run(ctx, RunRequest{Input: "在 QA Notes 点击 Save 前，如何避免保存旧标题？"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.toolChoices) != 1 || model.toolChoices[0] != nil {
		t.Fatalf("tool choices = %#v, want relevant question to remain model-controlled", model.toolChoices)
	}
}

func TestRuntimeIOSKeyboardIsolationCoalescesModifierActionsForWholeRun(t *testing.T) {
	events := []string{}
	controller := newTestIOSKeyboardIsolationController(&events)
	runtime := &Runtime{tools: &ToolSet{iosKeyboardIsolation: controller}}

	result, err := runtime.withIOSKeyboardIsolationRun(context.Background(), func(runCtx context.Context) (RunResult, error) {
		if err := controller.withKeyboard(runCtx, true, func() error {
			events = append(events, "first-shortcut")
			return nil
		}); err != nil {
			return RunResult{}, err
		}
		if err := controller.withBatch(runCtx, func(compositeCtx context.Context) error {
			return controller.withKeyboard(compositeCtx, true, func() error {
				events = append(events, "second-shortcut")
				return nil
			})
		}); err != nil {
			return RunResult{}, err
		}
		return RunResult{Output: "done"}, nil
	})
	if err != nil {
		t.Fatalf("withIOSKeyboardIsolationRun() error = %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("output = %q, want done", result.Output)
	}
	if want := []string{"isolate", "first-shortcut", "second-shortcut", "restore"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeIOSKeyboardIsolationDoesNotSwitchWithoutModifier(t *testing.T) {
	events := []string{}
	controller := newTestIOSKeyboardIsolationController(&events)
	runtime := &Runtime{tools: &ToolSet{iosKeyboardIsolation: controller}}

	_, err := runtime.withIOSKeyboardIsolationRun(context.Background(), func(runCtx context.Context) (RunResult, error) {
		return RunResult{}, controller.withKeyboard(runCtx, false, func() error {
			events = append(events, "plain-key")
			return nil
		})
	})
	if err != nil {
		t.Fatalf("withIOSKeyboardIsolationRun() error = %v", err)
	}
	if want := []string{"plain-key"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeIOSKeyboardIsolationRestoresWhenRunIsCanceled(t *testing.T) {
	events := []string{}
	controller := newCancellationSensitiveIOSKeyboardIsolationController(&events)
	runtime := &Runtime{tools: &ToolSet{iosKeyboardIsolation: controller}}
	ctx, cancel := context.WithCancel(context.Background())

	_, err := runtime.withIOSKeyboardIsolationRun(ctx, func(runCtx context.Context) (RunResult, error) {
		if err := controller.withKeyboard(runCtx, true, func() error {
			events = append(events, "shortcut")
			cancel()
			return nil
		}); err != nil {
			return RunResult{}, err
		}
		return RunResult{}, runCtx.Err()
	})
	if errors.Is(err, errIOSKeyboardRestoreUsedCanceledContext) {
		t.Fatalf("withIOSKeyboardIsolationRun() restore used canceled context: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("withIOSKeyboardIsolationRun() error = %v, want context canceled", err)
	}
	if want := []string{"isolate", "shortcut", "restore"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestRuntimeRunMarksMainAgentModelCallsForRawHTTPLog(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("completed")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Answer directly.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.rawHTTPLogEnabled) != 1 {
		t.Fatalf("expected one model call, got %d", len(model.rawHTTPLogEnabled))
	}
	if !model.rawHTTPLogEnabled[0] {
		t.Fatal("main agent model call was not marked for raw HTTP logging")
	}
}

func TestRuntimeRunExportsFailedTraceWhenModelBuildFails(t *testing.T) {
	exportedCh := make(chan struct{}, 1)
	var otlpBody []byte
	var scoreBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			otlpBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &scoreBody); err != nil {
				t.Errorf("decode score body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			select {
			case exportedCh <- struct{}{}:
			default:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	configDir := ensureTestConfigDir(t, t.TempDir())
	buildErr := errors.New("model unavailable")
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir: configDir,
			Telemetry: TelemetryConfig{
				Enabled:          boolPtr(true),
				BaseURL:          server.URL,
				PublicKey:        "pk-test",
				SecretKey:        "sk-test",
				UploadTimeoutSec: 1,
			},
		},
		&testModelResolver{err: buildErr},
		NewMemoryManager(filepath.Join(configDir, "memory")),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	_, err := runtime.Run(context.Background(), RunRequest{Input: "turn that should be traced"})
	if !errors.Is(err, buildErr) {
		t.Fatalf("Run() error = %v, want %v", err, buildErr)
	}

	select {
	case <-exportedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected Langfuse export for failed chat")
	}

	spans := decodeOTLPSpans(t, otlpBody)
	root := otlpSpanByName(t, spans, langfuseRunSpanName)
	wantInput, _ := json.Marshal("turn that should be traced")
	if got := root.attributeString(t, "langfuse.observation.input"); got != string(wantInput) {
		t.Fatalf("trace input = %s, want original user input", got)
	}
	if got := root.attributeString(t, "langfuse.trace.metadata.failure_reason"); got != buildErr.Error() {
		t.Fatalf("failure_reason = %q, want %q", got, buildErr.Error())
	}
	if scoreBody["value"] != float64(0) {
		t.Fatalf("score value = %v, want 0", scoreBody["value"])
	}
}

func TestRuntimeRunWaitForWakeupTerminatesRoleLoop(t *testing.T) {
	const wakeupMessage = "The current agent run is ending now, and the voice interaction will wait for the next wakeup event."
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("wait_1", "wait_for_wakeup", `{"reason":"user asked"}`),
		toolCallResponse("wait_2", "wait_for_wakeup", `{"reason":"still awake"}`),
	}}
	controller := NewWaitForWakeupController()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools.", MaxIterations: 2}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"wait_for_wakeup": NewWaitForWakeupTool(controller),
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "go to sleep"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.WaitForWakeupRequested {
		t.Fatal("WaitForWakeupRequested = false, want true")
	}
	if result.WaitForWakeupReason != "user asked" {
		t.Fatalf("WaitForWakeupReason = %q, want user asked", result.WaitForWakeupReason)
	}
	if !result.SleepRequested {
		t.Fatal("SleepRequested = false, want deprecated alias to mirror WaitForWakeupRequested")
	}
	if result.SleepReason != "user asked" {
		t.Fatalf("SleepReason = %q, want deprecated alias to mirror WaitForWakeupReason", result.SleepReason)
	}
	if result.Output != wakeupMessage {
		t.Fatalf("Output = %q, want wait-for-wakeup observation message", result.Output)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal RunResult: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("Unmarshal RunResult JSON: %v", err)
	}
	if payload["wait_for_wakeup_requested"] != true || payload["sleep_requested"] != true {
		t.Fatalf("RunResult JSON missing wakeup aliases: %s", encoded)
	}
	if payload["wait_for_wakeup_reason"] != "user asked" || payload["sleep_reason"] != "user asked" {
		t.Fatalf("RunResult JSON missing wakeup reason aliases: %s", encoded)
	}
	if model.callCount != 1 {
		t.Fatalf("model call count = %d, want role loop to stop after wait_for_wakeup", model.callCount)
	}
}

func TestRuntimeRunInjectsDeviceTypeStateIntoTools(t *testing.T) {
	model := &scriptedModel{responses: roleToolResponses("enter_text", `{"text":"hello","focus":{"x":500,"y":500}}`, "done")}
	tool := &deviceTypeCaptureTool{
		stubTool: stubTool{
			name:        "enter_text",
			description: "Enter text.",
			output:      `{"ok":true}`,
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
			Device:      DeviceConfig{DeviceType: "Android"},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"enter_text": tool}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:             "enter text",
		DeviceEnvironment: &PhoneEnvironment{Platform: "ios"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("output = %q, want final answer", result.Output)
	}
	if len(tool.deviceTypes) != 1 || tool.deviceTypes[0] != "Android" {
		t.Fatalf("runtime device_type = %v, want [Android]", tool.deviceTypes)
	}
}

func TestRuntimeRunDoesNotUsePhoneEnvironmentPlatformForPointerModeMismatch(t *testing.T) {
	tests := []struct {
		name       string
		platform   string
		deviceType string
	}{
		{
			name:       "android absolute",
			platform:   "android",
			deviceType: "iOS",
		},
		{
			name:       "ios touchscreen",
			platform:   "ios",
			deviceType: "Android",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedModel{responses: roleToolResponses("touch_gesture", `{"type":"tap","point":{"x":500,"y":500}}`, "continued with device_type state")}
			tool := &stubTool{
				name:        "touch_gesture",
				description: "Touch.",
				output:      `{"screen_changed":false}`,
			}
			runtime := NewRuntimeWithDeps(
				withTestConfigDir(t, Config{
					Model:       ModelConfig{Provider: "fake"},
					Instruction: "Use tools.",
					Device:      DeviceConfig{DeviceType: tc.deviceType},
				}),
				&testModelResolver{model: model},
				NewMemoryManager(""),
				&ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool}},
				NewSkillIndex(),
			)

			result, err := runtime.Run(context.Background(), RunRequest{
				Input:             "tap the screen",
				DeviceEnvironment: &PhoneEnvironment{Platform: tc.platform},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Output != "continued with device_type state" {
				t.Fatalf("output = %q, want model final answer after using device_type state", result.Output)
			}
			if model.callCount != 2 {
				t.Fatalf("model call count = %d, want second model call", model.callCount)
			}
			if len(tool.inputs) != 1 {
				t.Fatalf("touch_gesture calls = %d, want 1", len(tool.inputs))
			}
		})
	}
}

func TestRuntimeRunDoesNotStopMismatchContentFromPhoneEnvironmentPlatform(t *testing.T) {
	stopMessage := `touch_gesture produced no visible screen change, and the connected platform is Android while [device].device_type derives hid.pointer_mode="absolute". Stop operation here because the touch mode likely does not match the target. Please switch [device].device_type to "Android", restart the agent, and retry.`
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponseWithContent("call_1", "touch_gesture", `{"type":"long_press","point":{"x":500,"y":500}}`, stopMessage),
		contentResponse("continued with device_type state"),
	}}
	tool := &stubTool{
		name:        "touch_gesture",
		description: "Touch.",
		output:      `{"screen_changed":false}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
			Device:      DeviceConfig{DeviceType: "iOS"},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:             "tap the screen",
		DeviceEnvironment: &PhoneEnvironment{Platform: "android"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "continued with device_type state" {
		t.Fatalf("output = %q, want model final answer after using device_type state", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model call count = %d, want second model call", model.callCount)
	}
	if len(tool.inputs) != 1 {
		t.Fatalf("touch_gesture calls = %d, want 1", len(tool.inputs))
	}
}

func TestRuntimeRunDoesNotStopBracketedMismatchContentFromPhoneEnvironmentPlatform(t *testing.T) {
	stopMessage := `touch_gesture produced no visible screen change. The connected platform is Android but [device].device_type is configured for iOS. Stop operation here because the touch mode likely does not match the target.`
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponseWithContent("call_1", "touch_gesture", `{"type":"tap","point":{"x":500,"y":500}}`, stopMessage),
		contentResponse("continued with device_type state"),
	}}
	tool := &stubTool{
		name:        "touch_gesture",
		description: "Touch.",
		output:      `{"screen_changed":false}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
			Device:      DeviceConfig{DeviceType: "iOS"},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:             "tap the screen",
		DeviceEnvironment: &PhoneEnvironment{Platform: "android"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "continued with device_type state" {
		t.Fatalf("output = %q, want model final answer after using device_type state", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model call count = %d, want second model call", model.callCount)
	}
	if len(tool.inputs) != 1 {
		t.Fatalf("touch_gesture calls = %d, want 1", len(tool.inputs))
	}
}

func TestRuntimeRunDoesNotStopPointerModeMismatchContentForOtherTool(t *testing.T) {
	stopMessage := `touch_gesture produced no visible screen change, and the connected platform is Android while [device].device_type derives hid.pointer_mode="absolute". Stop operation here because the touch mode likely does not match the target. Please switch [device].device_type to "Android", restart the agent, and retry.`
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponseWithContent("call_1", "screenshot", `{}`, stopMessage),
		contentResponse("continued after screenshot"),
	}}
	tool := &stubTool{
		name:        "screenshot",
		description: "Screenshot.",
		output:      `{"ok":true}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
			Device:      DeviceConfig{DeviceType: "iOS"},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"screenshot": tool}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:             "take a screenshot",
		DeviceEnvironment: &PhoneEnvironment{Platform: "android"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "continued after screenshot" {
		t.Fatalf("output = %q, want model final answer", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model call count = %d, want continue to second model call", model.callCount)
	}
	if len(tool.inputs) != 2 {
		t.Fatalf("screenshot calls = %d, want automatic state capture plus explicit tool call", len(tool.inputs))
	}
}

func TestRuntimeRunContinuesTouchGestureWhenPointerModeMatches(t *testing.T) {
	model := &scriptedModel{responses: roleToolResponses("touch_gesture", `{"type":"tap","point":{"x":500,"y":500}}`, "verified after inspection")}
	tool := &stubTool{
		name:        "touch_gesture",
		description: "Touch.",
		output:      `{"screen_changed":false}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
			Device:      DeviceConfig{DeviceType: "Android"},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:             "tap the screen",
		DeviceEnvironment: &PhoneEnvironment{Platform: "android"},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "verified after inspection" {
		t.Fatalf("output = %q, want model final answer", result.Output)
	}
	if model.callCount != 2 {
		t.Fatalf("model call count = %d, want continue to second model call", model.callCount)
	}
	if len(tool.inputs) != 1 {
		t.Fatalf("touch_gesture calls = %d, want 1", len(tool.inputs))
	}
}

func TestRuntimeRunWaitForWakeupAppendsToolResultBeforeFinishing(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("wait_1", "wait_for_wakeup", `{"reason":"user asked"}`),
		contentResponse("resumed"),
	}}
	controller := NewWaitForWakeupController()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"wait_for_wakeup": NewWaitForWakeupTool(controller),
		}},
		NewSkillIndex(),
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "go to sleep"}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if _, err := runtime.Run(context.Background(), RunRequest{Input: "continue"}); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if len(model.messages) < 2 {
		t.Fatalf("model calls = %d, want second run planner prompt", len(model.messages))
	}

	var foundToolCall, foundToolResponse bool
	for _, msg := range model.messages[1] {
		for _, part := range msg.Parts {
			switch typed := part.(type) {
			case llms.ToolCall:
				if msg.Role == llms.ChatMessageTypeAI &&
					typed.ID == "wait_1" &&
					typed.FunctionCall != nil &&
					typed.FunctionCall.Name == "wait_for_wakeup" {
					foundToolCall = true
				}
			case llms.ToolCallResponse:
				if msg.Role == llms.ChatMessageTypeTool &&
					typed.ToolCallID == "wait_1" &&
					strings.Contains(typed.Content, "wait_for_wakeup_requested") {
					foundToolResponse = true
				}
			}
		}
	}
	if !foundToolCall || !foundToolResponse {
		t.Fatalf("second run prompt missing paired wait_for_wakeup scratchpad: found call=%v response=%v messages=%#v",
			foundToolCall, foundToolResponse, model.messages[1])
	}
}

func TestRuntimeRunWaitForWakeupDoesNotStream(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("wait_1", "wait_for_wakeup", `{"reason":"user asked"}`),
	}}
	controller := NewWaitForWakeupController()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"wait_for_wakeup": NewWaitForWakeupTool(controller),
		}},
		NewSkillIndex(),
	)

	var stream strings.Builder
	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "go to sleep",
		StreamWriter: &stream,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.WaitForWakeupRequested {
		t.Fatal("WaitForWakeupRequested = false, want true")
	}
	if stream.String() != "" {
		t.Fatalf("stream = %q, want no spoken final answer for wait_for_wakeup", stream.String())
	}
}

func TestRuntimeRunInjectsCurrentDateIntoPlannerPrompt(t *testing.T) {
	originalNow := promptNow
	promptNow = func() time.Time {
		return time.Date(2026, time.June, 15, 8, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() { promptNow = originalNow })

	model := &scriptedModel{responses: roleDirectResponses("completed")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.messages) == 0 || len(model.messages[0]) == 0 {
		t.Fatalf("expected model to receive planner prompt")
	}
	systemPrompt := messageText(model.messages[0][:1])
	want := "Current date: 2026-06-15 (Monday, timezone: UTC)"
	if !strings.Contains(systemPrompt, want) {
		t.Fatalf("planner system prompt missing current date %q:\n%s", want, systemPrompt)
	}
}

func TestRuntimeRunAllowsNilMemoryManager(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: model},
		nil,
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
}

func TestRuntimeRunContinuesWhenPersistedMemoryCannotLoad(t *testing.T) {
	memoryDir := filepath.Join(t.TempDir(), "memory")
	if err := os.MkdirAll(filepath.Join(memoryDir, "session"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memoryDir, "session", "events.jsonl"), []byte("{not-json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	manager := NewMemoryManager(memoryDir)
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: model},
		manager,
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	t.Cleanup(func() { _ = runtime.Close() })

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
}

func TestRuntimeRunUsesSessionManager(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			contentResponseWithInfo("Old answer.", map[string]any{"prompt_tokens": 321}),
		},
	}
	manager := &recordingSessionManager{
		beginResult: SessionBeginResult{},
		result:      SessionCommitResult{},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.sessionManager = manager

	result, err := runtime.Run(context.Background(), RunRequest{Input: "original request"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "Old answer." {
		t.Fatalf("output = %q, want \"Old answer.\"", result.Output)
	}

	if manager.beginCalls != 1 {
		t.Fatalf("expected one session begin, got %d", manager.beginCalls)
	}
	if manager.beginReq.Input != "original request" {
		t.Fatalf("begin input = %q, want original request", manager.beginReq.Input)
	}
	if manager.beginReq.CurrentHints != (CurrentEnvironmentHints{}) {
		t.Fatalf("unexpected begin hints: %#v", manager.beginReq.CurrentHints)
	}
	if manager.commitCalls != 1 {
		t.Fatalf("expected one session commit, got %d", manager.commitCalls)
	}
	if manager.commitReq.AgentName != "default" {
		t.Fatalf("agent name = %q, want default", manager.commitReq.AgentName)
	}
	if manager.commitReq.Input != "original request" || manager.commitReq.Output != "Old answer." {
		t.Fatalf("unexpected commit request: %#v", manager.commitReq)
	}
	if len(manager.commitReq.Steers) != 0 {
		t.Fatalf("unexpected commit steers: %#v", manager.commitReq.Steers)
	}
	if manager.commitReq.Metrics == nil || manager.commitReq.Metrics.LastPromptTokens != 321 {
		t.Fatalf("commit metrics missing prompt tokens: %#v", manager.commitReq.Metrics)
	}
}

func TestRuntimeRunContinuesWhenSessionBeginFails(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	manager := &recordingSessionManager{beginErr: errors.New("session append failed")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.sessionManager = manager

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
	if manager.beginCalls != 1 {
		t.Fatalf("begin calls = %d, want 1", manager.beginCalls)
	}
}

func TestRuntimeRunReturnsOutputWhenSessionCommitFails(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	manager := &recordingSessionManager{err: errors.New("session commit failed")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.sessionManager = manager

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
	if manager.commitCalls != 1 {
		t.Fatalf("commit calls = %d, want 1", manager.commitCalls)
	}
}

type recordingSessionManager struct {
	beginCalls  int
	beginReq    SessionBeginRequest
	beginResult SessionBeginResult
	beginErr    error
	commitCalls int
	commitReq   SessionCommitRequest
	result      SessionCommitResult
	err         error
}

func (m *recordingSessionManager) BeginRun(ctx context.Context, req SessionBeginRequest) (SessionBeginResult, error) {
	m.beginCalls++
	m.beginReq = req
	if m.beginErr != nil {
		return SessionBeginResult{}, m.beginErr
	}
	return m.beginResult, nil
}

func (m *recordingSessionManager) CommitRun(ctx context.Context, req SessionCommitRequest) (SessionCommitResult, error) {
	m.commitCalls++
	m.commitReq = req
	if m.err != nil {
		return SessionCommitResult{}, m.err
	}
	return m.result, nil
}

type blockingEpisodeMaintenancePlane struct {
	traceCommitted     atomic.Bool
	maintenanceStarted atomic.Bool
	released           atomic.Bool
	started            chan struct{}
	release            chan struct{}
}

func newBlockingEpisodeMaintenancePlane() *blockingEpisodeMaintenancePlane {
	return &blockingEpisodeMaintenancePlane{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (p *blockingEpisodeMaintenancePlane) NewEpisodeRecorder(req MemoryRetrieveRequest, retrieved MemoryContext) *EpisodeRecorder {
	return NewEpisodeRecorder(req, retrieved)
}

func (p *blockingEpisodeMaintenancePlane) CommitEpisode(context.Context, TaskEpisode) error {
	return errors.New("sync CommitEpisode should not be called")
}

func (p *blockingEpisodeMaintenancePlane) commitEpisodeTrace(context.Context, TaskEpisode) error {
	p.traceCommitted.Store(true)
	return nil
}

func (p *blockingEpisodeMaintenancePlane) commitEpisodeMaintenance(ctx context.Context, episode TaskEpisode) {
	if p.maintenanceStarted.CompareAndSwap(false, true) {
		close(p.started)
	}
	select {
	case <-ctx.Done():
	case <-p.release:
	}
}

func (p *blockingEpisodeMaintenancePlane) releaseMaintenance() {
	if p.released.CompareAndSwap(false, true) {
		close(p.release)
	}
}

type gatedEpisodeTracePlane struct {
	*blockingEpisodeMaintenancePlane
	traceStarted chan struct{}
	traceRelease chan struct{}
}

func newGatedEpisodeTracePlane() *gatedEpisodeTracePlane {
	return &gatedEpisodeTracePlane{
		blockingEpisodeMaintenancePlane: newBlockingEpisodeMaintenancePlane(),
		traceStarted:                    make(chan struct{}),
		traceRelease:                    make(chan struct{}),
	}
}

func (p *gatedEpisodeTracePlane) commitEpisodeTrace(ctx context.Context, episode TaskEpisode) error {
	close(p.traceStarted)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.traceRelease:
		p.traceCommitted.Store(true)
		return nil
	}
}

func TestRuntimeRunAsyncEpisodeMaintenanceDoesNotBlock(t *testing.T) {
	plane := newBlockingEpisodeMaintenancePlane()
	defer plane.releaseMaintenance()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: &scriptedModel{responses: roleDirectResponses("ok")}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane

	runReturned := make(chan struct{})
	var runErr error
	go func() {
		_, runErr = runtime.Run(context.Background(), RunRequest{
			Input:                   "hello",
			AsyncEpisodeMaintenance: true,
		})
		close(runReturned)
	}()

	// Async maintenance blocks in commitEpisodeMaintenance until the test
	// releases it. Wait for the worker to start before asserting that Run()
	// returned: Run() persists the session with fsync first, and that I/O can
	// take hundreds of milliseconds on IO-contended CI runners. Anchoring on the
	// worker start keeps the assertion independent of that disk latency.
	select {
	case <-plane.started:
	case <-runReturned:
		// Run() got back before the maintenance goroutine was scheduled, which
		// is still non-blocking. Make sure the worker starts afterwards.
		select {
		case <-plane.started:
		case <-time.After(time.Second):
			t.Fatal("async episode maintenance did not start")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("async episode maintenance did not start")
	}

	// Maintenance is running and blocked here, so Run() must not wait for it.
	select {
	case <-runReturned:
	case <-time.After(time.Second):
		t.Fatal("Run() blocked on async episode maintenance")
	}
	if runErr != nil {
		t.Fatalf("Run() error = %v", runErr)
	}
	if !plane.traceCommitted.Load() {
		t.Fatal("episode trace was not committed before Run returned")
	}
}

func TestRuntimeClosePreventsLateAsyncEpisodeMaintenanceRegistration(t *testing.T) {
	plane := newGatedEpisodeTracePlane()
	defer plane.releaseMaintenance()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: &scriptedModel{responses: roleDirectResponses("ok")}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane

	runDone := make(chan error, 1)
	go func() {
		_, err := runtime.Run(context.Background(), RunRequest{
			Input:                   "hello",
			AsyncEpisodeMaintenance: true,
		})
		runDone <- err
	}()
	// Run() persists the session (fsync) before it reaches the trace commit, so
	// allow the same generous IO headroom as the non-blocking test above.
	select {
	case <-plane.traceStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("episode trace commit did not start")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- runtime.Close()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() waited for episode maintenance that was not registered")
	}

	close(plane.traceRelease)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not return after episode trace commit completed")
	}
	select {
	case <-plane.started:
		t.Fatal("async episode maintenance started after Runtime.Close()")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRuntimeCloseWaitsForAsyncEpisodeMaintenance(t *testing.T) {
	plane := newBlockingEpisodeMaintenancePlane()
	defer plane.releaseMaintenance()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: &scriptedModel{responses: roleDirectResponses("ok")}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane

	if _, err := runtime.Run(context.Background(), RunRequest{
		Input:                   "hello",
		AsyncEpisodeMaintenance: true,
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-plane.started:
	case <-time.After(time.Second):
		t.Fatal("async episode maintenance did not start")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- runtime.Close()
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close() returned before async episode maintenance completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	plane.releaseMaintenance()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not return after async episode maintenance completed")
	}
}

func TestAsyncEpisodeMaintenanceCloseAndWaitCancelsWorkOnTimeout(t *testing.T) {
	var maintenance asyncRuntimeWork
	maintenanceCtx, started := maintenance.begin()
	if !started {
		t.Fatal("begin() = false, want true")
	}
	workCanceled := make(chan struct{})
	go func() {
		defer maintenance.done()
		<-maintenanceCtx.Done()
		close(workCanceled)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := maintenance.closeAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeAndWait() error = %v, want deadline exceeded", err)
	}
	select {
	case <-workCanceled:
	case <-time.After(time.Second):
		t.Fatal("closeAndWait() did not cancel active maintenance")
	}
}

type capturingEpisodePlane struct {
	episode TaskEpisode
}

func (p *capturingEpisodePlane) NewEpisodeRecorder(req MemoryRetrieveRequest, retrieved MemoryContext) *EpisodeRecorder {
	return NewEpisodeRecorder(req, retrieved)
}

func (p *capturingEpisodePlane) CommitEpisode(_ context.Context, episode TaskEpisode) error {
	p.episode = episode
	return nil
}

func TestRuntimeRunCommitsTimingEventsBeforeEpisodeCommit(t *testing.T) {
	plane := &capturingEpisodePlane{}
	model := &scriptedModel{responses: roleToolResponses("echo", `{"__arg1":"hello"}`, "ok")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools.", MaxIterations: 3}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"echo": &stubTool{name: "echo", description: "Echo.", output: "tool output"},
		}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane

	result, err := runtime.Run(context.Background(), RunRequest{Input: "use echo"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}

	var (
		hasSessionBegin  bool
		startByIteration = map[int]int{}
		endByIteration   = map[int]int{}
	)
	for index, event := range plane.episode.Events {
		switch event.Type {
		case runEventSessionBegin:
			hasSessionBegin = true
		case runEventIterationStart:
			startByIteration[eventMetadataInt(event, "iteration")] = index
		case runEventIterationEnd:
			endByIteration[eventMetadataInt(event, "iteration")] = index
		}
	}
	if !hasSessionBegin {
		t.Fatal("committed episode missing session_begin event")
	}
	if _, ok := startByIteration[1]; !ok {
		t.Fatalf("committed episode missing iteration 1 start: %#v", plane.episode.Events)
	}
	if _, ok := endByIteration[1]; !ok {
		t.Fatalf("committed episode missing iteration 1 end: %#v", plane.episode.Events)
	}
	if _, ok := startByIteration[2]; !ok {
		t.Fatalf("committed episode missing iteration 2 start: %#v", plane.episode.Events)
	}
	if _, ok := endByIteration[2]; !ok {
		t.Fatalf("committed episode missing iteration 2 end: %#v", plane.episode.Events)
	}
	if endByIteration[1] > startByIteration[2] {
		t.Fatalf("iteration 1 end index = %d, want before iteration 2 start index %d", endByIteration[1], startByIteration[2])
	}

	episodeStart := parseEpisodeTime(plane.episode.StartedAt, time.Time{})
	if episodeStart.IsZero() {
		t.Fatalf("committed episode missing StartedAt: %#v", plane.episode)
	}
	for _, event := range plane.episode.Events {
		switch event.Type {
		case runEventSessionBegin:
			eventTime := parseEpisodeTime(event.Ts, time.Time{})
			if eventTime.Before(episodeStart) {
				t.Fatalf("%s event time %s is before episode start %s", event.Type, eventTime.Format(time.RFC3339Nano), episodeStart.Format(time.RFC3339Nano))
			}
		}
	}
}

func TestRuntimeRunCommitsTurnTelemetryEvents(t *testing.T) {
	plane := &capturingEpisodePlane{}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane

	startedAt := time.Now().UTC().Add(-2 * time.Second)
	durationMs := int64(321)
	telemetryEvent := TaskEpisodeEvent{
		Type:       runEventSTTTranscription,
		Ts:         startedAt.Format(time.RFC3339Nano),
		Content:    "hello from voice",
		DurationMs: &durationMs,
		Metadata: map[string]interface{}{
			"provider":          "qwen-asr",
			"fallback_one_shot": false,
		},
	}

	_, err := runtime.Run(context.Background(), RunRequest{
		Input: "hello from voice",
		Turn: TurnInput{
			InputText:       "hello from voice",
			Modality:        TurnModalitySTT,
			Transcript:      "hello from voice",
			TelemetryEvents: []TaskEpisodeEvent{telemetryEvent},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(plane.episode.Events) == 0 || plane.episode.Events[0].Type != runEventSTTTranscription {
		t.Fatalf("first episode event = %#v, want STT telemetry first", plane.episode.Events)
	}
	if plane.episode.Events[0].Content != "hello from voice" {
		t.Fatalf("STT event content = %q", plane.episode.Events[0].Content)
	}
	episodeStart := parseEpisodeTime(plane.episode.StartedAt, time.Time{})
	if episodeStart.After(startedAt) {
		t.Fatalf("episode start = %s, want no later than STT start %s", episodeStart, startedAt)
	}
}

func eventMetadataInt(event TaskEpisodeEvent, key string) int {
	if event.Metadata == nil {
		return 0
	}
	switch value := event.Metadata[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func TestContextCompactionEventsUseDistinctTelemetryType(t *testing.T) {
	conversationEvent := contextCompactionEvent(compactor.CompactionStats{
		TokensBefore:                100,
		TokensAfter:                 40,
		ConversationSummaryRequired: true,
	}, true, nil, "threshold")
	if conversationEvent.Type != runEventConversationCompaction {
		t.Fatalf("conversation compaction event type = %q, want %q", conversationEvent.Type, runEventConversationCompaction)
	}

	historicalEvent := historicalPruneEvent(compactor.HistoricalPruneStats{
		HistoricalStatesDropped:        1,
		HistoricalToolResultsPruned:    2,
		CurrentTurnStatesDropped:       3,
		CurrentTurnToolExchangesPruned: 4,
	}, true, nil, "threshold")
	if historicalEvent.Type != runEventHistoricalContextPrune {
		t.Fatalf("historical prune event type = %q, want %q", historicalEvent.Type, runEventHistoricalContextPrune)
	}
	if conversationEvent.Type == historicalEvent.Type {
		t.Fatal("conversation compaction and historical prune events must use distinct types")
	}
	if got := eventMetadataInt(historicalEvent, "current_turn_states_dropped"); got != 3 {
		t.Fatalf("current-turn states metadata = %d, want 3", got)
	}
	if got := eventMetadataInt(historicalEvent, "current_turn_tool_exchanges_pruned"); got != 4 {
		t.Fatalf("current-turn tool exchanges metadata = %d, want 4", got)
	}
}

func TestRuntimeRunAttachesPendingSteerToNextToolCall(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses(
			"echo", `{"__arg1":"original action"}`,
			"Adjusted based on feedback.",
		),
	}
	tool := &stubTool{
		name:        "echo",
		description: "Echo.",
		output:      "tool output",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"echo": tool}},
		NewSkillIndex(),
	)

	var steerCalls int32
	var events []RunEvent
	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "do the original action",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
		SteerProvider: func(ctx context.Context) (RunSteerMessage, bool) {
			if atomic.AddInt32(&steerCalls, 1) != 1 {
				return RunSteerMessage{}, false
			}
			return RunSteerMessage{
				ID:        "steer-1",
				RequestID: "req-1",
				Content:   "Use the updated instruction instead.",
				Timestamp: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
			}, true
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "Adjusted based on feedback." {
		t.Fatalf("output = %q, want steer-adjusted response", result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "original action" {
		t.Fatalf("tool inputs = %#v, want single original action", tool.inputs)
	}

	if steerCalls == 0 {
		t.Fatal("SteerProvider was not polled")
	}
	toolResult, ok := firstRunEventOfType(events, "tool_result")
	if !ok {
		t.Fatalf("missing tool_result event: %#v", events)
	}
	if toolResult.Content != "tool output" || toolResult.IsError {
		t.Fatalf("unexpected tool result: %#v", toolResult)
	}
	// Second LLM call should include steer content
	if len(model.messages) < 2 {
		t.Fatalf("expected second LLM call after steer, got %#v", model.messages)
	}
	if !runtimeModelCallContains(model.messages[1], "Use the updated instruction instead.") {
		t.Fatalf("second LLM call missing steer: %#v", model.messages[1])
	}
	if !runtimeModelCallToolResponseContains(model.messages[1], "tool output") {
		t.Fatalf("second LLM call missing tool result: %#v", model.messages[1])
	}
}

func TestRuntimeRunSteerInterruptDoesNotPauseAfterNonCancelableTool(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("slow", `{"__arg1":"original action"}`, "Changed course."),
	}
	toolStarted := make(chan struct{})
	releaseTool := make(chan struct{})
	tool := &blockingTool{
		name:          "slow",
		description:   "Slow tool.",
		output:        "tool output",
		started:       toolStarted,
		release:       releaseTool,
		interruptSeen: make(chan struct{}),
		ignoreCancel:  true,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"slow": tool}},
		NewSkillIndex(),
	)

	interruptCh := make(chan struct{})
	waitCalled := make(chan struct{})
	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, err := runtime.Run(context.Background(), RunRequest{
			Input: "do the original action",
			SteerInterrupt: func() <-chan struct{} {
				return interruptCh
			},
			SteerWaiter: func(ctx context.Context) (RunSteerMessage, bool, error) {
				close(waitCalled)
				return RunSteerMessage{ID: "steer-1", Content: "Use the updated instruction instead."}, true, nil
			},
		})
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-toolStarted:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	close(interruptCh)
	select {
	case <-tool.interruptSeen:
	case <-time.After(time.Second):
		t.Fatal("tool context was not canceled after steer interrupt")
	}
	close(releaseTool)

	var runResult struct {
		result RunResult
		err    error
	}
	select {
	case runResult = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("runtime did not finish after interrupted non-cancelable tool returned")
	}
	select {
	case <-waitCalled:
		t.Fatal("SteerWaiter was called by context-manager loop")
	default:
	}
	if runResult.err != nil {
		t.Fatalf("Run() error = %v", runResult.err)
	}
	if runResult.result.Output != "Changed course." {
		t.Fatalf("output = %q, want Changed course.", runResult.result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "original action" {
		t.Fatalf("tool inputs = %#v, want only original action", tool.inputs)
	}
	if len(model.messages) < 2 {
		t.Fatalf("expected second model call after tool result, got %#v", model.messages)
	}
	if runtimeModelCallContains(model.messages[1], "Use the updated instruction instead.") {
		t.Fatalf("second model call unexpectedly contains steer message: %#v", model.messages[1])
	}
}

func TestRuntimeRunSteerInterruptWaitsThenContinuesWithoutInput(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("slow", `{"__arg1":"original action"}`, "Original done."),
	}
	toolStarted := make(chan struct{})
	toolCanceled := make(chan struct{})
	tool := &blockingTool{
		name:        "slow",
		description: "Slow tool.",
		output:      "tool output",
		started:     toolStarted,
		canceled:    toolCanceled,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"slow": tool}},
		NewSkillIndex(),
	)

	interruptCh := make(chan struct{})
	waitCalled := make(chan struct{})
	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, err := runtime.Run(context.Background(), RunRequest{
			Input: "do the original action",
			SteerInterrupt: func() <-chan struct{} {
				return interruptCh
			},
			SteerWaiter: func(ctx context.Context) (RunSteerMessage, bool, error) {
				close(waitCalled)
				return RunSteerMessage{}, false, nil
			},
		})
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-toolStarted:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	close(interruptCh)
	select {
	case <-toolCanceled:
	case <-time.After(time.Second):
		t.Fatal("tool context was not canceled after steer interrupt")
	}

	var runResult struct {
		result RunResult
		err    error
	}
	select {
	case runResult = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("runtime did not return after cancelable tool was canceled")
	}
	if runResult.err != nil {
		t.Fatalf("Run() error = %v", runResult.err)
	}
	if runResult.result.Output != "Original done." {
		t.Fatalf("output = %q, want Original done.", runResult.result.Output)
	}
	select {
	case <-waitCalled:
		// Expected: the canceled tool waits briefly for steering capture.
	default:
		t.Fatal("SteerWaiter was not called after tool cancellation")
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "original action" {
		t.Fatalf("tool inputs = %#v, want only original action", tool.inputs)
	}
}

func TestRuntimeRunCanceledToolDoesNotPoisonNextRunToolHistory(t *testing.T) {
	model := &strictToolPairModel{}
	toolStarted := make(chan struct{})
	toolCanceled := make(chan struct{})
	tool := &blockingTool{
		name:        "slow",
		description: "Slow tool.",
		output:      "tool output",
		started:     toolStarted,
		canceled:    toolCanceled,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"slow": tool}},
		NewSkillIndex(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := runtime.Run(ctx, RunRequest{Input: "do the original action"})
		firstDone <- err
	}()

	select {
	case <-toolStarted:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	cancel()
	select {
	case <-toolCanceled:
	case <-time.After(time.Second):
		t.Fatal("tool context was not canceled")
	}
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run() error = %v, want context canceled", err)
	}

	requireInterruptNotices(t, runtime.contextManager, "canceled")
	result, err := runtime.Run(context.Background(), RunRequest{Input: "continue"})
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	requireInterruptNotices(t, runtime.contextManager, "canceled")
	if result.Output != "continued" {
		t.Fatalf("second Run() output = %q, want continued", result.Output)
	}
}

type strictToolPairModel struct {
	calls atomic.Int64
}

func (m *strictToolPairModel) GenerateContent(_ context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	if m.calls.Add(1) == 1 {
		return toolCallResponseWithContent("call_1", "slow", `{"__arg1":"original action"}`, ""), nil
	}
	if err := validateStrictToolMessageSequence(messages); err != nil {
		return nil, err
	}
	return contentResponse("continued"), nil
}

func (m *strictToolPairModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

func validateStrictToolMessageSequence(messages []llms.MessageContent) error {
	pending := map[string]struct{}{}
	for i, message := range messages {
		if len(pending) > 0 && message.Role != llms.ChatMessageTypeTool && message.Role != llms.ChatMessageTypeFunction {
			return fmt.Errorf("message %d arrived before tool responses for %v", i, pending)
		}
		switch message.Role {
		case llms.ChatMessageTypeAI:
			for _, part := range message.Parts {
				if call, ok := part.(llms.ToolCall); ok {
					pending[strings.TrimSpace(call.ID)] = struct{}{}
				}
			}
		case llms.ChatMessageTypeTool, llms.ChatMessageTypeFunction:
			if len(pending) == 0 {
				return fmt.Errorf("message %d contains an orphan tool result", i)
			}
			for _, part := range message.Parts {
				response, ok := part.(llms.ToolCallResponse)
				if !ok {
					continue
				}
				id := strings.TrimSpace(response.ToolCallID)
				if _, ok := pending[id]; !ok {
					return fmt.Errorf("message %d responds to unknown tool call %q", i, id)
				}
				delete(pending, id)
			}
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("missing tool responses for %v", pending)
	}
	return nil
}

func TestRuntimeRunConsumesPendingSteerBeforeFinalAnswer(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			contentResponse("Old answer."),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	var steerCalls int32
	var events []RunEvent
	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "answer the old request",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
		SteerProvider: func(ctx context.Context) (RunSteerMessage, bool) {
			if atomic.AddInt32(&steerCalls, 1) != 1 {
				return RunSteerMessage{}, false
			}
			return RunSteerMessage{
				ID:        "steer-1",
				RequestID: "req-1",
				Content:   "Actually change direction before answering.",
				Timestamp: time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC),
			}, true
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "Old answer." {
		t.Fatalf("output = %q, want Old answer.", result.Output)
	}
	if steerCalls == 0 {
		t.Fatal("SteerProvider was not polled")
	}
	if steerEvent, ok := firstRunEventOfType(events, "steer"); !ok || steerEvent.Content != "Actually change direction before answering." {
		t.Fatalf("missing steer event: %#v", events)
	}
	if len(model.messages) != 1 {
		t.Fatalf("model calls = %d, want direct final answer in one call", len(model.messages))
	}
}

func TestRuntimeRunPublishesReasoningWithFinalAssistantOutput(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{{
		Choices: []*llms.ContentChoice{{Content: "final answer", ReasoningContent: "short plan"}},
	}}}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	t.Cleanup(func() { _ = runtime.Close() })

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "hi",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	var final []RunEvent
	for _, event := range events {
		if event.Type == "assistant_output" {
			final = append(final, event)
		}
	}
	if len(final) != 1 {
		t.Fatalf("assistant_output events = %d, want 1; events=%#v", len(final), events)
	}
	if final[0].Content != "final answer" || final[0].ReasoningContent != "short plan" {
		t.Fatalf("assistant_output = %#v, want final answer plus reasoning", final[0])
	}
}

func TestRuntimeRunDoesNotMarkLocalValidationErrorAsTurnFailure(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{})
	if err == nil {
		t.Fatal("Run() error = nil, want input validation error")
	}
	if result.TurnFailure != nil {
		t.Fatalf("local validation error TurnFailure = %#v, want nil", result.TurnFailure)
	}
}

func TestRuntimeRunIncludesAvailableSkillCatalog(t *testing.T) {
	index := NewSkillIndex()
	index.skills["planner"] = &SkillDefinition{
		Name:         "planner",
		Description:  "Plan before acting",
		Instructions: "Make a plan.",
	}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		index,
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !runtimeModelCallContains(model.messages[0], "## Available skills") {
		t.Fatalf("run missing available skills heading")
	}
	if !runtimeModelCallContains(model.messages[0], "- planner: Plan before acting") {
		t.Fatalf("run missing planner skill catalog entry")
	}
	if runtimeModelCallContains(model.messages[0], "[planner] Make a plan.") {
		t.Fatalf("inactive skill instructions should not be injected")
	}
}

func TestRuntimeRunOmitsArchivedSkillsFromAvailableCatalog(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	skillsDir := filepath.Join(configDir, "skills")
	writeSKILL(t, skillsDir, "alpha", testSkillA)
	writeSKILL(t, skillsDir, "beta", testSkillB)
	saveSkillUsage(filepath.Join(configDir, "skill-state", "usage.json"), map[string]SkillUsageEntry{
		"beta": {State: SkillUsageStateArchived},
	})
	index, err := LoadSkillsFromDirs([]string{skillsDir})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, SkillsDirs: []string{skillsDir}, Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1},
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		index,
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !runtimeModelCallContains(model.messages[0], "- alpha: Alpha skill") {
		t.Fatalf("run missing active alpha skill catalog entry")
	}
	if runtimeModelCallContains(model.messages[0], "- beta: Beta skill") {
		t.Fatalf("run included archived beta skill catalog entry")
	}
}

func TestRuntimeRunFiltersAvailableSkillCatalogByDeviceType(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	skillsDir := filepath.Join(configDir, "skills")
	writeSKILL(t, skillsDir, "android-only", "---\nname: android-only\ndescription: Android skill\nmetadata:\n  device_types: [Android]\n---\n\nUse Android.\n")
	writeSKILL(t, skillsDir, "ios-only", "---\nname: ios-only\ndescription: iOS skill\nmetadata:\n  device_types: [iOS]\n---\n\nUse iOS.\n")
	index, err := LoadSkillsFromDirs([]string{skillsDir})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, SkillsDirs: []string{skillsDir}, Device: DeviceConfig{DeviceType: "Android"}, Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly.", MaxIterations: 1},
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		index,
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !runtimeModelCallContains(model.messages[0], "- android-only: Android skill") {
		t.Fatalf("run missing Android skill catalog entry")
	}
	if runtimeModelCallContains(model.messages[0], "- ios-only: iOS skill") {
		t.Fatalf("run included incompatible iOS skill catalog entry")
	}
}

func TestToolDescriptorsIncludeSkillToolMetadata(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	tools := &ToolSet{tools: map[string]langtools.Tool{}}
	tools.RegisterSkillTools(filepath.Join(configDir, "skills"), filepath.Join(configDir, "skill-state", ".bundled_manifest.json"))
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, tools, NewSkillIndex())

	for _, name := range []string{"skill_list", "skill_read"} {
		desc, ok := runtime.ToolDescriptorByName(name)
		if !ok {
			t.Fatalf("expected descriptor for %s", name)
		}
		if desc.Category != "skills" {
			t.Fatalf("%s category = %q, want skills", name, desc.Category)
		}
		if desc.InputMode != toolInputModeJSON {
			t.Fatalf("%s input mode = %q, want json", name, desc.InputMode)
		}
		if strings.TrimSpace(desc.ExampleInput) == "" {
			t.Fatalf("%s missing example input", name)
		}
	}
	if _, ok := runtime.ToolDescriptorByName("skill_mark_used"); ok {
		t.Fatal("skill_mark_used should remain hidden from the HTTP catalog")
	}
}

func TestToolDescriptorsIncludeMemoryToolMetadata(t *testing.T) {
	tools := &ToolSet{tools: map[string]langtools.Tool{}}
	tools.RegisterMemoryTools(t.TempDir(), filepath.Join(t.TempDir(), "memory"), nil)
	runtime := NewRuntimeWithDeps(Config{}, nil, nil, tools, NewSkillIndex())

	for _, name := range []string{"recall_device_memory", "inspect_episode"} {
		desc, ok := runtime.ToolDescriptorByName(name)
		if !ok {
			t.Fatalf("expected descriptor for %s", name)
		}
		if desc.Category != "memory" {
			t.Fatalf("%s category = %q, want memory", name, desc.Category)
		}
		if desc.InputMode != toolInputModeJSON {
			t.Fatalf("%s input mode = %q, want json", name, desc.InputMode)
		}
		if strings.TrimSpace(desc.ExampleInput) == "" {
			t.Fatalf("%s missing example input", name)
		}
	}
}

func TestSkillCatalogSummaryLimitsEntriesAndDescriptionLength(t *testing.T) {
	index := NewSkillIndex()
	longDesc := strings.Repeat("长", maxSkillCatalogDescriptionRunes+10)
	for i := 0; i < maxSkillCatalogEntries+2; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		index.skills[name] = &SkillDefinition{Name: name, Description: longDesc}
	}
	manager := NewSkillManager(index)
	catalog := manager.CatalogSummary()
	if strings.Count(catalog, "- skill-") != maxSkillCatalogEntries {
		t.Fatalf("expected %d catalog entries, got catalog:\n%s", maxSkillCatalogEntries, catalog)
	}
	if !strings.Contains(catalog, "more skills hidden. Use skill_list to search") {
		t.Fatalf("expected hidden skills hint, got:\n%s", catalog)
	}
	if strings.Contains(catalog, strings.Repeat("长", maxSkillCatalogDescriptionRunes+1)) {
		t.Fatalf("expected long descriptions to be truncated")
	}
}

func TestSkillManagerRejectsIncompatibleDeviceTypeActivation(t *testing.T) {
	index := NewSkillIndex()
	index.skills["ios-only"] = &SkillDefinition{Name: "ios-only", Description: "iOS skill", DeviceTypes: []string{"iOS"}}
	manager := NewSkillManager(index)
	manager.SetDeviceTypeFunc(func() string { return "Android" })

	err := manager.Activate(context.Background(), "ios-only")
	if err == nil {
		t.Fatal("expected incompatible activation to fail")
	}
	if !strings.Contains(err.Error(), "current device_type is Android") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRuntimeRunSnapshotUnaffectedByConcurrentReload(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	skillsDir := filepath.Join(configDir, "skills")
	v1 := "---\nname: alpha\ndescription: Alpha\n---\n\nUse alpha v1.\n"
	v2 := "---\nname: alpha\ndescription: Alpha\n---\n\nUse alpha v2.\n"
	writeSKILL(t, skillsDir, "alpha", v1)
	index, err := LoadSkillsFromDirs([]string{skillsDir})
	if err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, SkillsDirs: []string{skillsDir}},
		&testModelResolver{model: fakellm.NewFakeLLM([]string{"unused"})},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		index,
	)

	if err := runtime.reloadSkillsIfDirty(); err != nil {
		t.Fatal(err)
	}
	runSkills := runtime.skills.Snapshot()
	if err := runSkills.Activate(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}

	writeSKILL(t, skillsDir, "alpha", v2)
	runtime.MarkSkillsDirty()
	if err := runtime.reloadSkillsIfDirty(); err != nil {
		t.Fatal(err)
	}

	resolved, err := runSkills.Resolve([]string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resolved.CombinedInstructions(), "Use alpha v1.") {
		t.Fatalf("in-progress run snapshot lost v1 instructions: %s", resolved.CombinedInstructions())
	}
	if strings.Contains(resolved.CombinedInstructions(), "Use alpha v2.") {
		t.Fatalf("in-progress run snapshot saw reloaded v2 instructions: %s", resolved.CombinedInstructions())
	}
}

func runtimeModelCallContains(messages []llms.MessageContent, want string) bool {
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if text, ok := part.(llms.TextContent); ok && strings.Contains(text.Text, want) {
				return true
			}
		}
	}
	return false
}

func runtimeModelCallToolResponseContains(messages []llms.MessageContent, want string) bool {
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if response, ok := part.(llms.ToolCallResponse); ok && strings.Contains(response.Content, want) {
				return true
			}
		}
	}
	return false
}

func runtimeLastMessageText(messages []llms.MessageContent) (llms.ChatMessageType, string, bool) {
	if len(messages) == 0 {
		return "", "", false
	}
	last := messages[len(messages)-1]
	var builder strings.Builder
	for _, part := range last.Parts {
		if text, ok := part.(llms.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return last.Role, builder.String(), true
}

type staticTool struct {
	name   string
	output string
}

func (t *staticTool) Name() string { return t.name }

func (t *staticTool) Description() string { return "static test tool" }

func (t *staticTool) Call(context.Context, string) (string, error) { return t.output, nil }

type failingGenerateModel struct {
	err error
}

type runtimeContextOverflowRecoveryModel struct {
	callCount int
}

func (m *runtimeContextOverflowRecoveryModel) GenerateContent(_ context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.callCount++
	switch m.callCount {
	case 1:
		return nil, newProviderHTTPError(http.StatusBadRequest, []byte(
			`{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`,
		))
	case 2:
		return contentResponse("compacted summary"), nil
	default:
		return contentResponse("ok after compaction"), nil
	}
}

func (m *runtimeContextOverflowRecoveryModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

func (m *runtimeContextOverflowRecoveryModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "context-overflow-recovery", ContextWindow: 1_000_000}
}

func (m *runtimeContextOverflowRecoveryModel) CallOptions() []chains.ChainCallOption { return nil }

func (m failingGenerateModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return nil, errors.New("generate failed")
}

func (m failingGenerateModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	return "", errors.New("call failed")
}

// scriptedModel replays a fixed sequence of responses and records what it was
// asked, so tests can assert on the prompt, tools, and streaming mode.
type scriptedModel struct {
	responses         []*llms.ContentResponse
	streamChunks      [][]string
	callCount         int
	sawStreaming      []bool
	messages          [][]llms.MessageContent
	tools             [][]llms.Tool
	toolChoices       []any
	rawHTTPLogEnabled []bool
}

func (m *scriptedModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	var callOptions llms.CallOptions
	for _, option := range options {
		option(&callOptions)
	}
	m.sawStreaming = append(m.sawStreaming, callOptions.StreamingFunc != nil)
	m.messages = append(m.messages, messages)
	m.tools = append(m.tools, callOptions.Tools)
	m.toolChoices = append(m.toolChoices, callOptions.ToolChoice)
	m.rawHTTPLogEnabled = append(m.rawHTTPLogEnabled, rawHTTPLogEnabled(ctx))

	if (callOptions.StreamingFunc != nil || callOptions.StreamingReasoningFunc != nil) && m.callCount < len(m.responses) {
		if callOptions.StreamingReasoningFunc != nil {
			reasoning := m.responses[m.callCount].Choices[0].ReasoningContent
			if reasoning != "" {
				if err := callOptions.StreamingReasoningFunc(ctx, []byte(reasoning), nil); err != nil {
					return nil, err
				}
			}
		}
		if callOptions.StreamingFunc != nil && m.streamChunks != nil && m.callCount < len(m.streamChunks) {
			for _, chunk := range m.streamChunks[m.callCount] {
				if err := callOptions.StreamingFunc(ctx, []byte(chunk)); err != nil {
					return nil, err
				}
			}
		} else if callOptions.StreamingFunc != nil {
			content := m.responses[m.callCount].Choices[0].Content
			if content != "" {
				if err := callOptions.StreamingFunc(ctx, []byte("chunk:"+content)); err != nil {
					return nil, err
				}
			}
		}
	}

	if m.callCount >= len(m.responses) {
		t := &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: ""}}}
		return t, nil
	}

	response := m.responses[m.callCount]
	m.callCount++
	return response, nil
}

func (m *scriptedModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

func (m *scriptedModel) Spec() model.ModelSpec {
	return model.ModelSpec{
		Provider:      "fake",
		Name:          "scripted",
		ContextWindow: 1_000_000,
	}
}

func (m *scriptedModel) CallOptions() []chains.ChainCallOption { return nil }

func contentResponse(content string) *llms.ContentResponse {
	return contentResponseWithInfo(content, nil)
}

func TestRuntimeRunCompactsWithoutLogger(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	// This fixture must comfortably exceed the 8192-token model's ~80% input
	// threshold to force compaction.
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "Answer directly."},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("user ", 1600)},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("assistant ", 1600)},
		{
			Role: messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{
				ID:        "call_1",
				Name:      "echo",
				Arguments: `{"input":"` + strings.Repeat("x", 4000) + `"}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "call_1",
				Name:       "echo",
				Content:    strings.Repeat("result ", 1600),
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "recent tail"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}

	llmModel := &scriptedModel{
		responses: []*llms.ContentResponse{
			contentResponse("compacted summary"),
			contentResponse("ok"),
		},
	}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{
			model: llmModel,
			spec:  model.ModelSpec{ContextWindow: 8_192},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
	if len(llmModel.messages) != 2 {
		t.Fatalf("model call count = %d, want 2 (summary + planner)", len(llmModel.messages))
	}
}

func TestRuntimeRunCompactionTriggerIncludesToolSchema(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "Answer directly."},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("u", 8_000)},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("a", 8_000)},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("q", 4_000)},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("r", 400)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	largeSchemaTool := &structuredStubTool{
		// Cross the summary threshold while leaving room for the protected
		// head/tail messages under the hard input budget after summarization.
		stubTool: stubTool{name: "large_schema", description: strings.Repeat("schema ", 800)},
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": strings.Repeat("field ", 500)},
			},
		},
	}
	llmModel := &scriptedModel{responses: []*llms.ContentResponse{
		contentResponse("compacted summary"),
		contentResponse("compacted summary after retry"),
		contentResponse("ok"),
	}}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{
			model: llmModel,
			spec:  model.ModelSpec{ContextWindow: 8_192},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{largeSchemaTool.Name(): largeSchemaTool}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want planner response after schema-aware compaction", result.Output)
	}
	if len(llmModel.messages) != 3 {
		t.Fatalf("model call count = %d, want two summaries + planner", len(llmModel.messages))
	}
}

func TestRuntimeRunPrunesHistoricalStateWithProviderCompaction(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "Answer directly."},
		{Role: messages.MessageRoleState, Content: strings.Repeat("stale-device-state ", 2_000)},
		{Role: messages.MessageRoleUser, Content: "old question"},
		{Role: messages.MessageRoleAssistant, Content: "old answer"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	originalSessionID := manager.GetSessionID()

	llmModel := &scriptedModel{responses: []*llms.ContentResponse{contentResponse("ok")}}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir: configDir,
			// A deliberately low fraction so the oversized state message is well
			// past the prune trigger regardless of the model's budget.
			ContextPruneThreshold: 0.1,
			Model: ModelConfig{
				Provider:                   "openai",
				APIMode:                    "responses",
				ResponsesContextManagement: "compaction",
			},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{
			model: llmModel,
			spec:  model.ModelSpec{ContextWindow: 8_192},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "new question"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok" {
		t.Fatalf("output = %q, want ok", result.Output)
	}
	if llmModel.callCount != 1 {
		t.Fatalf("model call count = %d, want 1 planner call without local summary", llmModel.callCount)
	}
	if runtime.contextManager == nil || runtime.contextManager.GetSessionID() == originalSessionID {
		t.Fatal("runtime did not switch to the state-pruned context revision")
	}
	for _, message := range runtime.contextManager.CloneMessageList() {
		if message.Role == messages.MessageRoleState || strings.Contains(message.Content, "stale-device-state") {
			t.Fatalf("pruned context retained historical state: %#v", message)
		}
	}
}

func TestRuntimeRunPrunesContextDuringSingleLongToolRun(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	responses := make([]*llms.ContentResponse, 0, 7)
	for i := 1; i <= 6; i++ {
		responses = append(responses, toolCallResponse(
			fmt.Sprintf("call_%d", i),
			"echo",
			fmt.Sprintf(`{"input":"step-%d-%s"}`, i, strings.Repeat("x", 2_400)),
		))
	}
	responses = append(responses, contentResponse("long task complete"))
	llmModel := &scriptedModel{responses: responses}
	tool := &stubTool{
		name:        "echo",
		description: "Echo input.",
		output:      strings.Repeat("bounded tool result ", 20),
	}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:             configDir,
			ContextPruneThreshold: 0.2,
			Model:                 ModelConfig{Provider: "fake", MaxResponseTokens: 256},
			Instruction:           "Complete every requested step.",
			MaxIterations:         7,
		},
		&testModelResolver{
			model: llmModel,
			spec: model.ModelSpec{
				Provider:      "fake",
				Name:          "long-tool-run",
				ContextWindow: 12_000,
				MaxOutput:     256,
			},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"echo": tool}},
		NewSkillIndex(),
	)
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "run all six steps"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "long task complete" || llmModel.callCount != 7 {
		t.Fatalf("output/calls = %q/%d, want long task complete/7", result.Output, llmModel.callCount)
	}
	if runtime.contextManager == nil || runtime.contextManager.GetParentSessionID() == "" {
		t.Fatal("runtime did not switch to a pruned context revision during the run")
	}
	loaded, err := contextmanager.LoadContextManagerFromCurrentSession(agentpath.ContextManagerSessionFolder(configDir))
	if err != nil {
		t.Fatalf("LoadContextManagerFromCurrentSession() error = %v", err)
	}
	if loaded.GetSessionID() != runtime.contextManager.GetSessionID() {
		t.Fatalf("current session = %q, want %q", loaded.GetSessionID(), runtime.contextManager.GetSessionID())
	}

	finalRequest := llmModel.messages[len(llmModel.messages)-1]
	argumentsByID := make(map[string]string)
	resultsByID := make(map[string]string)
	for _, message := range finalRequest {
		for _, part := range message.Parts {
			switch part := part.(type) {
			case llms.ToolCall:
				if part.FunctionCall != nil {
					argumentsByID[part.ID] = part.FunctionCall.Arguments
				}
			case llms.ToolCallResponse:
				resultsByID[part.ToolCallID] = part.Content
			}
		}
	}
	if _, callRetained := argumentsByID["call_1"]; callRetained {
		t.Fatalf("oldest tool call was retained before final request: %q", argumentsByID["call_1"])
	}
	if _, resultRetained := resultsByID["call_1"]; resultRetained {
		t.Fatalf("oldest tool result was retained before final request: %q", resultsByID["call_1"])
	}
	for i := 4; i <= 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		if !strings.Contains(argumentsByID[id], fmt.Sprintf("step-%d-", i)) {
			t.Fatalf("recent tool exchange %s was unexpectedly pruned: %q", id, argumentsByID[id])
		}
	}
}

func TestRuntimeRunPrunesProtectedExchangesForHardInputBudget(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	responses := make([]*llms.ContentResponse, 0, 4)
	for i := 1; i <= 3; i++ {
		responses = append(responses, toolCallResponse(
			fmt.Sprintf("call_%d", i),
			"echo",
			fmt.Sprintf(`{"input":"step-%d-%s"}`, i, strings.Repeat("x", 5_000)),
		))
	}
	responses = append(responses, contentResponse("hard budget handled"))
	llmModel := &scriptedModel{responses: responses}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:             configDir,
			ContextPruneThreshold: 0.8,
			Model:                 ModelConfig{Provider: "fake", MaxResponseTokens: 256},
			Instruction:           "Complete every requested step.",
			MaxIterations:         4,
		},
		&testModelResolver{
			model: llmModel,
			spec: model.ModelSpec{
				Provider:      "fake",
				Name:          "hard-budget-tool-run",
				ContextWindow: 5_000,
				MaxOutput:     256,
			},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"echo": &stubTool{name: "echo", description: "Echo input.", output: "ok"}}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "run all three steps"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "hard budget handled" || llmModel.callCount != 4 {
		t.Fatalf("output/calls = %q/%d, want hard budget handled/4", result.Output, llmModel.callCount)
	}
	finalRequest := llmModel.messages[len(llmModel.messages)-1]
	fullArguments := 0
	for _, message := range finalRequest {
		for _, part := range message.Parts {
			call, ok := part.(llms.ToolCall)
			if ok && call.FunctionCall != nil && strings.Contains(call.FunctionCall.Arguments, strings.Repeat("x", 1_000)) {
				fullArguments++
			}
		}
	}
	if fullArguments >= 3 {
		t.Fatal("hard-budget pruning preserved all three oversized recent exchanges")
	}
	var finalOptions llms.CallOptions
	finalOptions.Tools = llmModel.tools[len(llmModel.tools)-1]
	requestTokens := tokencounter.EstimateLLMMessageListTokens(finalRequest) + tokencounter.EstimateToolSchemaTokens(finalOptions)
	if usable := toolResultUsableInputBudget(5_000, 256); requestTokens > usable {
		t.Fatalf("final request tokens = %d, want <= usable input budget %d", requestTokens, usable)
	}
}

func TestRuntimeRunStopsLocallyWhenHardInputBudgetCannotFit(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("s", 8_000)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	llmModel := &scriptedModel{responses: []*llms.ContentResponse{contentResponse("must not be called")}}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:             configDir,
			ContextPruneThreshold: 0.8,
			Model:                 ModelConfig{Provider: "fake", MaxResponseTokens: 256},
			Instruction:           "Answer directly.",
			MaxIterations:         1,
		},
		&testModelResolver{
			model: llmModel,
			spec:  model.ModelSpec{Provider: "fake", Name: "hard-budget-local-stop", ContextWindow: 5_000, MaxOutput: 256},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	_, err = runtime.Run(context.Background(), RunRequest{Input: strings.Repeat("u", 8_000)})
	if err == nil || !strings.Contains(err.Error(), "context remains over usable input budget after pruning") {
		t.Fatalf("Run() error = %v, want local hard-budget error", err)
	}
	if llmModel.callCount != 0 {
		t.Fatalf("model call count = %d, want 0 for a request that cannot fit locally", llmModel.callCount)
	}
}

func TestRuntimeRunRejectsHardInputBudgetFromMeasuredUsage(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	manager, err := contextmanager.NewContextManagerFromMessageList(
		agentpath.ContextManagerSessionFolder(configDir), []messages.Message{
			{Role: messages.MessageRoleSystem, Content: "system"},
			{Role: messages.MessageRoleUser, Content: "hello"},
			{
				Role: messages.MessageRoleAssistant, Content: "ok",
				Usage: &messages.Usage{InputTokens: 9_000, OutputTokens: 100, TotalTokens: 9_100},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	llmModel := &scriptedModel{responses: []*llms.ContentResponse{contentResponse("must not be called")}}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir: configDir, ContextPruneThreshold: 0.8, MaxIterations: 1,
			Model: ModelConfig{
				Provider: "openai", APIMode: "responses", ResponsesContextManagement: "compaction", MaxResponseTokens: 256,
			},
		},
		&testModelResolver{model: llmModel, spec: model.ModelSpec{ContextWindow: 5_000, MaxOutput: 256}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil
	_, err = runtime.Run(context.Background(), RunRequest{Input: "continue"})
	if err == nil || !strings.Contains(err.Error(), "context remains over usable input budget") {
		t.Fatalf("Run() error = %v, want measured-usage budget rejection", err)
	}
	if llmModel.callCount != 0 {
		t.Fatalf("model call count = %d, want 0", llmModel.callCount)
	}
}

func TestRuntimeRunSummarizesBeforeRejectingHardInputBudget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		summary         string
		wantErr         string
		chunkWriteFails bool
	}{
		{name: "fits", summary: "old work summarized"},
		{name: "chunk persistence fails", summary: "old work summarized", chunkWriteFails: true},
		{name: "summary fails", wantErr: "empty summary"},
		{name: "summary still too large", summary: strings.Repeat("s", 16_000), wantErr: "context remains over usable input budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir := ensureTestConfigDir(t, t.TempDir())
			sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
			manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
				{Role: messages.MessageRoleSystem, Content: "system"},
				{Role: messages.MessageRoleState, Content: "expired state"},
				{Role: messages.MessageRoleUser, Content: "old goal"},
				{Role: messages.MessageRoleAssistant, Content: strings.Repeat("h", 14_000)},
				{Role: messages.MessageRoleUser, Content: "recent goal"},
				{Role: messages.MessageRoleAssistant, Content: "recent tail"},
			})
			if err != nil {
				t.Fatal(err)
			}
			originalSessionID := manager.GetSessionID()
			if err := manager.SwitchSession(manager.GetSessionID()); err != nil {
				t.Fatalf("SwitchSession() error = %v", err)
			}
			if tc.chunkWriteFails {
				// A file where the chunk directory belongs makes persistence fail
				// without preventing creation or activation of the context revision.
				if err := os.WriteFile(filepath.Join(sessionFolder, "chunks"), []byte("blocked"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			llmModel := &scriptedModel{responses: []*llms.ContentResponse{
				contentResponse(tc.summary), contentResponse("done after recovery"),
			}}
			runtime := NewRuntimeWithDeps(
				Config{
					ConfigDir: configDir, ContextPruneThreshold: 0.79, MaxIterations: 1,
					Model: ModelConfig{
						Provider: "openai", APIMode: "responses", ResponsesContextManagement: "compaction", MaxResponseTokens: 256,
					},
				},
				&testModelResolver{model: llmModel, spec: model.ModelSpec{ContextWindow: 6_000, MaxOutput: 256}},
				NewMemoryManager(""),
				&ToolSet{tools: map[string]langtools.Tool{
					"echo": &stubTool{name: "echo", description: strings.Repeat("schema ", 900)},
				}},
				NewSkillIndex(),
			)
			runtime.contextManager = manager
			runtime.logger = nil
			result, err := runtime.Run(context.Background(), RunRequest{Input: "continue"})
			loaded, loadErr := contextmanager.LoadContextManagerFromCurrentSession(sessionFolder)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			index, indexErr := loadChunkIndexFromPath(filepath.Join(sessionFolder, "chunks", "index.yaml"))
			if indexErr != nil && !tc.chunkWriteFails {
				t.Fatal(indexErr)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Run() error = %v, want %q", err, tc.wantErr)
				}
				if runtime.contextManager != manager || loaded.GetSessionID() != manager.GetSessionID() {
					t.Fatal("failed budget preparation switched the active session")
				}
				if llmModel.callCount != 1 || len(llmModel.tools[0]) != 0 {
					t.Fatal("failed budget preparation should only call the summary model")
				}
				if len(index.Chunks) != 0 {
					t.Fatalf("rejected budget recovery persisted %d chunks", len(index.Chunks))
				}
				if tc.name == "summary still too large" {
					llmModel.responses[1] = contentResponse(tc.summary)
					_, retryErr := runtime.Run(context.Background(), RunRequest{Input: "retry"})
					if retryErr == nil || !strings.Contains(retryErr.Error(), tc.wantErr) {
						t.Fatalf("retry error = %v, want budget rejection", retryErr)
					}
					retryIndex, err := loadChunkIndexFromPath(filepath.Join(sessionFolder, "chunks", "index.yaml"))
					if err != nil || len(retryIndex.Chunks) != 0 {
						t.Fatalf("rejected retry persisted chunks: count=%d error=%v", len(retryIndex.Chunks), err)
					}
				}
				return
			}
			if err != nil || result.Output != "done after recovery" || llmModel.callCount != 2 {
				t.Fatalf("Run() = %q, %v, calls=%d; want summary then successful request", result.Output, err, llmModel.callCount)
			}
			if len(llmModel.tools[0]) != 0 || len(llmModel.tools[1]) == 0 {
				t.Fatal("expected summary before the tool-enabled model request")
			}
			if runtime.contextManager != manager || loaded.GetSessionID() != runtime.contextManager.GetSessionID() || loaded.GetSessionID() == originalSessionID {
				t.Fatal("successful recovery did not activate the prepared revision on the existing manager")
			}
			if !tc.chunkWriteFails {
				if len(index.Chunks) != 1 || index.Chunks[0].Summary != tc.summary || index.Chunks[0].EventCount != 1 {
					t.Fatalf("accepted recovery chunks = %#v, want one summarized history span", index.Chunks)
				}
				data, readErr := os.ReadFile(filepath.Join(sessionFolder, "chunks", index.Chunks[0].File))
				if readErr != nil || !strings.Contains(string(data), strings.Repeat("h", 14_000)) {
					t.Fatalf("accepted chunk did not retain the original history: %v", readErr)
				}
			}
			if requestTokens := estimateActivePromptTokens(runtime.contextManager, []llms.CallOption{llms.WithTools(llmModel.tools[1])}); requestTokens > toolResultUsableInputBudget(6_000, 256) {
				t.Fatalf("recovered request still exceeds budget: %d", requestTokens)
			}
		})
	}
}

func TestRuntimeRunCompactsAndRetriesWhenProviderRejectsContextLength(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "Answer directly."},
		{Role: messages.MessageRoleUser, Content: "old question one"},
		{Role: messages.MessageRoleAssistant, Content: "old answer one"},
		{Role: messages.MessageRoleUser, Content: "old question two"},
		{Role: messages.MessageRoleAssistant, Content: "old answer two"},
		{Role: messages.MessageRoleUser, Content: "old question three"},
		{Role: messages.MessageRoleAssistant, Content: "old answer three"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	originalSessionID := manager.GetSessionID()

	llmModel := &runtimeContextOverflowRecoveryModel{}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{
			model: llmModel,
			spec:  model.ModelSpec{ContextWindow: 1_000_000},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil

	result, err := runtime.Run(context.Background(), RunRequest{Input: "new question"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "ok after compaction" {
		t.Fatalf("output = %q, want ok after compaction", result.Output)
	}
	if llmModel.callCount != 3 {
		t.Fatalf("model call count = %d, want 3 (rejected planner + summary + retried planner)", llmModel.callCount)
	}
	if runtime.contextManager == nil || runtime.contextManager.GetSessionID() == originalSessionID {
		t.Fatal("runtime did not switch to the compacted context session")
	}
	loaded, err := contextmanager.LoadContextManagerFromCurrentSession(sessionFolder)
	if err != nil {
		t.Fatalf("LoadContextManagerFromCurrentSession() error = %v", err)
	}
	if loaded.GetSessionID() != runtime.contextManager.GetSessionID() {
		t.Fatalf("current session = %q, want %q", loaded.GetSessionID(), runtime.contextManager.GetSessionID())
	}
}

func contentResponseWithInfo(content string, info map[string]any) *llms.ContentResponse {
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content:        content,
			GenerationInfo: info,
		}},
	}
}

func plannerResponse(nextStep string, plan ...string) *llms.ContentResponse {
	if len(plan) == 0 {
		plan = []string{nextStep}
	}
	payload, _ := json.Marshal(map[string]any{
		"objective":           "test objective",
		"completion_criteria": []string{"test request is satisfied"},
		"plan":                plan,
		"next_step":           nextStep,
		"reason":              "test plan",
	})
	return contentResponse(string(payload))
}

func verifierFinishResponse(finalAnswer string) *llms.ContentResponse {
	return verifierFinishResponseWithInfo(finalAnswer, nil)
}

func verifierFinishResponseWithInfo(finalAnswer string, info map[string]any) *llms.ContentResponse {
	return contentResponseWithInfo(verifierFinishJSON(finalAnswer), info)
}

func verifierFinishJSON(finalAnswer string) string {
	payload, _ := json.Marshal(map[string]any{
		"can_finish":   true,
		"final_answer": finalAnswer,
		"reason":       "test verified",
	})
	return string(payload)
}

func structuredVerifierFinishResponse(speechText, output string) *llms.ContentResponse {
	return contentResponse(structuredVerifierFinishJSON(speechText, output))
}

func structuredVerifierFinishJSON(speechText, output string) string {
	payload, _ := json.Marshal(map[string]any{
		"can_finish":   true,
		"speech":       speechText,
		"text":         output,
		"final_answer": output,
		"reason":       "test verified",
	})
	return string(payload)
}

func verifierContinueResponse(reason string) *llms.ContentResponse {
	return contentResponse(verifierContinueJSON(reason))
}

func verifierContinueJSON(reason string) string {
	payload, _ := json.Marshal(map[string]any{
		"can_finish":   false,
		"needs_replan": true,
		"reason":       reason,
	})
	return string(payload)
}

func verifierStepContinueResponse(reason string) *llms.ContentResponse {
	payload, _ := json.Marshal(map[string]any{
		"can_finish":   false,
		"needs_replan": false,
		"reason":       reason,
	})
	return contentResponse(string(payload))
}

func toolCallResponse(id, name, arguments string) *llms.ContentResponse {
	return toolCallResponseWithContent(id, name, arguments, "")
}

func toolCallResponseWithContent(id, name, arguments, content string) *llms.ContentResponse {
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: content,
			ToolCalls: []llms.ToolCall{{
				ID:   id,
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      name,
					Arguments: arguments,
				},
			}},
		}},
	}
}

func multiToolCallResponse(calls ...llms.ToolCall) *llms.ContentResponse {
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: calls,
		}},
	}
}

func roleDirectResponses(finalAnswer string) []*llms.ContentResponse {
	return []*llms.ContentResponse{contentResponse(finalAnswer)}
}

func roleDefaultToolResponses(toolName, arguments, finalAnswer string) []*llms.ContentResponse {
	return []*llms.ContentResponse{
		toolCallResponse("call_1", toolName, arguments),
		contentResponse(finalAnswer),
	}
}

func roleToolResponses(toolName, arguments, finalAnswer string) []*llms.ContentResponse {
	return roleDefaultToolResponses(toolName, arguments, finalAnswer)
}

func roleReviewedToolResponses(toolName, arguments, finalAnswer string) []*llms.ContentResponse {
	responses := roleToolResponses(toolName, arguments, finalAnswer)
	return append(responses, verifierFinishResponse(finalAnswer))
}

func firstRunEventOfType(events []RunEvent, eventType string) (RunEvent, bool) {
	for _, event := range events {
		if event.Type == eventType {
			return event, true
		}
	}
	return RunEvent{}, false
}

func TestRuntimeCallbackPropagatesToolErrorToEventsAndMessages(t *testing.T) {
	toolErr := NewToolErrorWithDetails(CodePermissionDenied, "contacts permission denied", map[string]any{"scope": "contacts"})
	var gotRunEvent RunEvent
	handler := &runtimeCallbackHandler{
		episodeID: "ep-1",
		runtimeID: "runtime-1",
		requestID: "req-1",
		runID:     "run-1",
		eventHandler: func(event RunEvent) {
			gotRunEvent = event
		},
	}
	call := ToolCall{
		Spec: ToolSpec{Name: "bridge_contacts"},
		Action: schema.AgentAction{
			Tool:      "bridge_contacts",
			ToolID:    "call-1",
			ToolInput: `{"action":"query"}`,
		},
		Input: `{"action":"query"}`,
	}

	handler.HandleToolCallResult(context.Background(), call, ToolResult{Output: toolErr.Message, Error: toolErr})

	if gotRunEvent.ToolError == nil || gotRunEvent.ToolError.Code != CodePermissionDenied {
		t.Fatalf("RunEvent.ToolError = %+v, want permission_denied", gotRunEvent.ToolError)
	}
	if gotRunEvent.ToolError.Details["scope"] != "contacts" {
		t.Fatalf("RunEvent.ToolError.Details = %+v, want scope=contacts", gotRunEvent.ToolError.Details)
	}
	message := messageFromRunEvent(gotRunEvent, "", "req-1")
	if message.ToolError == nil || message.ToolError.Code != CodePermissionDenied {
		t.Fatalf("Message.ToolError = %+v, want permission_denied", message.ToolError)
	}
	if message.ToolError.Details["scope"] != "contacts" {
		t.Fatalf("Message.ToolError.Details = %+v, want scope=contacts", message.ToolError.Details)
	}
	if gotRunEvent.Content != toolErr.Message || message.Content != toolErr.Message {
		t.Fatalf("error message content mismatch: run=%q message=%q want=%q", gotRunEvent.Content, message.Content, toolErr.Message)
	}
}

func runEventsOfType(events []RunEvent, eventType string) []RunEvent {
	var matching []RunEvent
	for _, event := range events {
		if event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

func taskEpisodeEventsOfType(events []TaskEpisodeEvent, eventType string) []TaskEpisodeEvent {
	var matching []TaskEpisodeEvent
	for _, event := range events {
		if event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

type stubTool struct {
	name        string
	description string
	output      string
	err         error
	visual      bool
	inputs      []string
	callFn      func(context.Context, string) (string, error)
}

func (t *stubTool) Name() string { return t.name }

func (t *stubTool) Description() string { return t.description }

func (t *stubTool) ReturnsVisualObservation() bool { return t.visual }

func (t *stubTool) Call(ctx context.Context, input string) (string, error) {
	t.inputs = append(t.inputs, input)
	if t.callFn != nil {
		return t.callFn(ctx, input)
	}
	if t.err != nil {
		return "", t.err
	}
	return t.output, nil
}

type deviceTypeCaptureTool struct {
	stubTool
	deviceTypeFn func() string
	deviceTypes  []string
}

func (t *deviceTypeCaptureTool) SetDeviceTypeFunc(fn func() string) {
	t.deviceTypeFn = fn
}

func (t *deviceTypeCaptureTool) Call(ctx context.Context, input string) (string, error) {
	if t.deviceTypeFn != nil {
		t.deviceTypes = append(t.deviceTypes, t.deviceTypeFn())
	} else {
		t.deviceTypes = append(t.deviceTypes, "")
	}
	return t.stubTool.Call(ctx, input)
}

type blockingTool struct {
	name          string
	description   string
	output        string
	inputs        []string
	started       chan struct{}
	release       chan struct{}
	canceled      chan struct{}
	interruptSeen chan struct{}
	ignoreCancel  bool
}

func (t *blockingTool) Name() string { return t.name }

func (t *blockingTool) Description() string { return t.description }

func (t *blockingTool) Call(ctx context.Context, input string) (string, error) {
	t.inputs = append(t.inputs, input)
	if t.started != nil {
		close(t.started)
	}
	if t.ignoreCancel {
		if t.interruptSeen != nil {
			go func() {
				<-ctx.Done()
				close(t.interruptSeen)
			}()
		}
		if t.release != nil {
			<-t.release
		}
		return t.output, nil
	}
	select {
	case <-ctx.Done():
		if t.canceled != nil {
			close(t.canceled)
		}
		return "", ctx.Err()
	case <-t.release:
		return t.output, nil
	}
}

func TestRuntimeRunOpenRouterUsesToolsWithoutStreaming(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("audio_volume", `{"__arg1":"{}"}`, "The current audio volume is 42."),
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "openrouter"},
			Instruction: "Use tools when external state is requested.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "当前音量是多少？"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "The current audio volume is 42." {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "{}" {
		t.Fatalf("unexpected tool inputs: %#v", tool.inputs)
	}
	if len(model.sawStreaming) != 2 || model.sawStreaming[0] || model.sawStreaming[1] {
		t.Fatalf("expected non-streaming default-mode planner calls, got %#v", model.sawStreaming)
	}
}

func TestRuntimeRunFakeProviderUsesFunctionAgentToolCalls(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("audio_volume", `{"__arg1":"{}"}`, "The current audio volume is 42."),
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when external state is requested.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "当前音量是多少？"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "The current audio volume is 42." {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "{}" {
		t.Fatalf("unexpected tool inputs: %#v", tool.inputs)
	}
}

func TestRuntimeRunRestoresPlannerToolCallsIntoNextRunPrompt(t *testing.T) {
	model := &scriptedModel{
		responses: append(
			roleToolResponses("echo", `{"__arg1":"{}"}`, "first run done"),
			contentResponse("second run done"),
		),
	}
	tool := &stubTool{
		name:        "echo",
		description: "Echo test tool.",
		output:      "echo result",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when needed.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(t.TempDir()),
		&ToolSet{tools: map[string]langtools.Tool{
			"echo": tool,
		}},
		NewSkillIndex(),
	)
	t.Cleanup(func() { _ = runtime.Close() })

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "call echo"}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if _, err := runtime.Run(context.Background(), RunRequest{Input: "continue"}); err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if len(model.messages) < 3 {
		t.Fatalf("model calls = %d, want second run planner prompt", len(model.messages))
	}

	secondRunPrompt := model.messages[2]
	var foundToolCall, foundToolResponse bool
	for _, msg := range secondRunPrompt {
		for _, part := range msg.Parts {
			switch typed := part.(type) {
			case llms.ToolCall:
				if msg.Role == llms.ChatMessageTypeAI &&
					typed.ID == "call_1" &&
					typed.FunctionCall != nil &&
					typed.FunctionCall.Name == "echo" {
					foundToolCall = true
				}
			case llms.ToolCallResponse:
				if msg.Role == llms.ChatMessageTypeTool &&
					typed.ToolCallID == "call_1" &&
					strings.Contains(typed.Content, "echo result") {
					foundToolResponse = true
				}
			}
		}
	}
	if !foundToolCall || !foundToolResponse {
		t.Fatalf("second run planner prompt missing persisted tool scratchpad: found call=%v response=%v messages=%#v",
			foundToolCall, foundToolResponse, secondRunPrompt)
	}
}

func TestRuntimeRunExecutesOnlyFirstToolCallAndKeepsModelToolCallMessage(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			multiToolCallResponse(
				llms.ToolCall{
					ID:   "call_1",
					Type: "function",
					FunctionCall: &llms.FunctionCall{
						Name:      "slow_a",
						Arguments: `{"__arg1":"{}"}`,
					},
				},
				llms.ToolCall{
					ID:   "call_2",
					Type: "function",
					FunctionCall: &llms.FunctionCall{
						Name:      "slow_b",
						Arguments: `{"__arg1":"{}"}`,
					},
				},
			),
			contentResponse("done"),
		},
	}
	toolA := &stubTool{name: "slow_a", description: "First tool.", output: `{"ok":true}`}
	toolB := &stubTool{name: "slow_b", description: "Second tool.", output: `{"ok":true}`}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"slow_a": toolA,
			"slow_b": toolB,
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "run both"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(toolA.inputs) != 1 || toolA.inputs[0] != "{}" {
		t.Fatalf("first tool inputs = %#v, want one empty JSON call", toolA.inputs)
	}
	if len(toolB.inputs) != 0 {
		t.Fatalf("second tool inputs = %#v, want no calls", toolB.inputs)
	}
	if len(model.messages) < 2 {
		t.Fatalf("model calls = %d, want at least 2", len(model.messages))
	}
	var toolCallNames []string
	for _, msg := range model.messages[1] {
		for _, part := range msg.Parts {
			toolCall, ok := part.(llms.ToolCall)
			if !ok || toolCall.FunctionCall == nil {
				continue
			}
			toolCallNames = append(toolCallNames, toolCall.FunctionCall.Name)
		}
	}
	if !slices.Equal(toolCallNames, []string{"slow_a"}) {
		t.Fatalf("scratchpad tool calls = %#v, want only first model tool call", toolCallNames)
	}
}

func TestRuntimeRunFeedsToolErrorsBackToModel(t *testing.T) {
	model := &scriptedModel{
		responses: roleReviewedToolResponses("screenshot", `{"__arg1":"{}"}`, "屏幕暂时获取失败，frame service 正在恢复。"),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"screenshot": &stubTool{
				name:        "screenshot",
				description: "Capture a screenshot from the connected display.",
				visual:      true,
				err:         errors.New("frame service: SERVICE_RECOVERING"),
			},
		}},
		NewSkillIndex(),
	)

	var events []RunEvent
	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "看看屏幕",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "屏幕暂时获取失败，frame service 正在恢复。" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(model.messages) < 2 {
		t.Fatalf("expected follow-up planner call with tool observation, got %d calls", len(model.messages))
	}
	var toolObservation string
	for _, msg := range model.messages[1] {
		if msg.Role != llms.ChatMessageTypeTool {
			continue
		}
		if len(msg.Parts) == 1 {
			if part, ok := msg.Parts[0].(llms.ToolCallResponse); ok {
				toolObservation = part.Content
			}
		}
	}
	if !strings.Contains(toolObservation, "frame service: SERVICE_RECOVERING") || strings.Contains(toolObservation, "error:") {
		t.Fatalf("unexpected tool observation: %q", toolObservation)
	}
	toolResult, ok := firstRunEventOfType(events, "tool_result")
	if !ok || !toolResult.IsError {
		t.Fatalf("expected error tool_result event, got %#v", events)
	}
	if toolResult.ToolError == nil || toolResult.ToolError.Code != CodeToolExecutionFailed || toolResult.ToolError.Message != toolObservation {
		t.Fatalf("tool_result ToolError = %+v, want execution failure matching observation %q", toolResult.ToolError, toolObservation)
	}
}
func TestRuntimeRunStripsLegacyToolInputWithoutDerivingContent(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("audio_volume", `{"__arg1":"{}","description":"我先读取当前音量。","speech":"读取音量。"}`, "The current audio volume is 42."),
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when external state is requested.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	var events []RunEvent
	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "当前音量是多少？",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "The current audio volume is 42." {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "{}" {
		t.Fatalf("unexpected tool inputs: %#v", tool.inputs)
	}
	toolCall, ok := firstRunEventOfType(events, runEventToolCall)
	if !ok {
		t.Fatalf("expected tool_call event, got %#v", events)
	}
	if toolCall.Content != "" {
		t.Fatalf("tool_call content = %q, want empty without assistant content", toolCall.Content)
	}
	if toolCall.ToolInput != "{}" {
		t.Fatalf("tool_call event input = %q, want stripped input", toolCall.ToolInput)
	}
}

func TestRuntimeRunEmitsToolContentForToolCallSpeech(t *testing.T) {
	speech := "读取音量。"
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, speech),
			contentResponse("The current audio volume is 42."),
		},
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	toolSpeechEnabled := true
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:               ModelConfig{Provider: "fake"},
			Instruction:         "Use tools when external state is requested.",
			VoiceToolCallSpeech: &toolSpeechEnabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "当前音量是多少？",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	toolCall, ok := firstRunEventOfType(events, runEventToolCall)
	if !ok {
		t.Fatalf("expected tool_call event, got %#v", events)
	}
	if toolCall.Content != speech {
		t.Fatalf("tool_call content = %q, want assistant content", toolCall.Content)
	}
}

func TestRuntimeLogsPreserveThinkStartTagInToolCallContent(t *testing.T) {
	thinkingContent := "<think>\n需要查当前时间。\n</think>"
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "shell", `{"command":"date"}`, thinkingContent),
			contentResponse("已完成。"),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when external state is requested.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"shell": &stubTool{name: "shell", description: "Run controller commands.", output: "now"},
		}},
		NewSkillIndex(),
	)
	var logs bytes.Buffer
	runtime.logger = &Logger{logger: log.New(&logs, "", 0)}

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "现在几点了？"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	logText := logs.String()
	if !strings.Contains(logText, "Role output: role=agent") {
		t.Fatalf("missing agent role output log:\n%s", logText)
	}
	if !strings.Contains(logText, "Tool call: name=shell") {
		t.Fatalf("missing shell tool call log:\n%s", logText)
	}
	if strings.Count(logText, "<think>") < 2 {
		t.Fatalf("logs lost think start tag:\n%s", logText)
	}
	if !strings.Contains(logText, "</think>") {
		t.Fatalf("log lost think end tag:\n%s", logText)
	}
}

func TestRuntimeRunDoesNotDeriveToolContentFromDescriptionArgument(t *testing.T) {
	description := "我先读取当前音量并检查当前播放设备、音量状态、静音状态、输出通道以及系统返回结果是否一致。然后继续回答。"
	model := &scriptedModel{
		responses: roleToolResponses("audio_volume", fmt.Sprintf(`{"__arg1":"{}","description":%q}`, description), "The current audio volume is 42."),
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	toolSpeechEnabled := true
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:               ModelConfig{Provider: "fake"},
			Instruction:         "Use tools when external state is requested.",
			VoiceToolCallSpeech: &toolSpeechEnabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "当前音量是多少？",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	toolCall, ok := firstRunEventOfType(events, runEventToolCall)
	if !ok {
		t.Fatalf("expected tool_call event, got %#v", events)
	}
	if toolCall.Content != "" {
		t.Fatalf("tool_call content = %q, want empty without assistant content", toolCall.Content)
	}
}
func TestRuntimeDirectAnswerDoesNotGenerateTodo(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("done")}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "hello",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if todos := runEventsOfType(events, "todo_update"); len(todos) != 0 {
		t.Fatalf("direct answer emitted todo_update events: %#v", todos)
	}
	if closed := runEventsOfType(events, "todo_closed"); len(closed) != 0 {
		t.Fatalf("direct answer emitted todo_closed events: %#v", closed)
	}
}

func TestRuntimeSimpleLoopDoesNotGenerateImplicitTodo(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("call_1", "screenshot", `{"__arg1":"{}"}`),
			toolCallResponse("call_2", "shell", `{"command":"date"}`),
			contentResponse("done"),
			verifierFinishResponse("done"),
		},
	}
	screenshot := &stubTool{name: "screenshot", description: "Capture screen.", output: "screen"}
	shell := &stubTool{name: "shell", description: "Run controller commands.", output: "now"}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"screenshot": screenshot,
			"shell":      shell,
		}},
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "look",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	todos := runEventsOfType(events, "todo_update")
	if len(todos) != 0 {
		t.Fatalf("simple loop emitted implicit todo_update events: %#v", todos)
	}
	if closed := runEventsOfType(events, "todo_closed"); len(closed) != 0 {
		t.Fatalf("simple loop emitted implicit todo_closed events: %#v", closed)
	}
	if len(screenshot.inputs) != 2 || len(shell.inputs) != 1 {
		t.Fatalf("expected simple tools to execute without todo, screenshot=%#v shell=%#v", screenshot.inputs, shell.inputs)
	}
}

func TestRuntimeForceSimpleLoopDoesNotGenerateTodo(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("call_1", "shell", `{"command":"date"}`),
			contentResponse("done"),
		},
	}
	shell := &stubTool{name: "shell", description: "Run controller commands.", output: "now"}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools.", ForceSimpleLoop: true}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"shell": shell}},
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "search",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	todos := runEventsOfType(events, "todo_update")
	if len(todos) != 0 {
		t.Fatalf("single-agent loop emitted todo_update events: %#v", todos)
	}
	if closed := runEventsOfType(events, "todo_closed"); len(closed) != 0 {
		t.Fatalf("single-agent loop emitted todo_closed events: %#v", closed)
	}
	if len(shell.inputs) != 1 {
		t.Fatalf("expected single-agent tool to execute without todo, inputs=%#v", shell.inputs)
	}
}

func TestRuntimeForceSimpleLoopRejectsLegacySetTodoTool(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("todo_1", "set_todo", `{"__arg1":"{\"items\":[\"inspect state\"]}"}`),
			contentResponse("done"),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools.", ForceSimpleLoop: true}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	var events []RunEvent
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: "complex single-agent task",
		EventHandler: func(event RunEvent) {
			events = append(events, event)
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	toolCall, ok := firstRunEventOfType(events, runEventToolCall)
	if !ok || toolCall.ToolName != "set_todo" || toolCall.IsError {
		t.Fatalf("legacy set_todo should be recorded as a normal tool_call event, got %#v", events)
	}
	toolResult, ok := firstRunEventOfType(events, "tool_result")
	if !ok || !toolResult.IsError || toolResult.ToolName != "set_todo" {
		t.Fatalf("legacy set_todo should fail in tool_result, got %#v", events)
	}
	if toolResult.ToolError == nil || toolResult.ToolError.Code != CodeToolNotFound {
		t.Fatalf("legacy set_todo ToolError = %#v, want tool_not_found", toolResult.ToolError)
	}
	if todos := runEventsOfType(events, "todo_update"); len(todos) != 0 {
		t.Fatalf("legacy set_todo emitted todo_update events: %#v", todos)
	}
	if closed := runEventsOfType(events, "todo_closed"); len(closed) != 0 {
		t.Fatalf("legacy set_todo emitted todo_closed events: %#v", closed)
	}
}

func TestRuntimeSimpleLoopDoesNotInjectLegacyTodoReminderAfterSeveralToolCalls(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("call_1", "web_search", `{"__arg1":"one"}`),
			toolCallResponse("call_2", "web_search", `{"__arg1":"two"}`),
			toolCallResponse("call_3", "web_search", `{"__arg1":"three"}`),
			contentResponse("done"),
		},
	}
	webSearch := &stubTool{name: "web_search", description: "Search web.", output: "result"}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Use tools.", ForceSimpleLoop: true}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"web_search": webSearch}},
		NewSkillIndex(),
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "complex single-agent task"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.messages) < 4 {
		t.Fatalf("expected fourth model call after three tools, got %d", len(model.messages))
	}
	for i, messages := range model.messages {
		prompt := messageText(messages)
		if strings.Contains(prompt, "Todo reminder") || strings.Contains(prompt, "call set_todo") {
			t.Fatalf("model call %d leaked legacy todo reminder runtime state:\n%s", i, prompt)
		}
	}
}

func TestRuntimeCallbackRemovesPendingActionWithNormalizedToolInput(t *testing.T) {
	handler := &runtimeCallbackHandler{}
	handler.pushPendingAction(schema.AgentAction{
		Tool:      "audio_volume",
		ToolInput: "{}\nObservation:",
	})

	handler.removePendingAction("AUDIO_VOLUME", "{}")

	if action, ok := handler.popPendingAction(); ok {
		t.Fatalf("pending action was not removed: %#v", action)
	}
}

func TestRuntimeRunOpenRouterStreamsWhenWriterIsProvided(t *testing.T) {
	model := &scriptedModel{
		responses: roleDirectResponses("completed"),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "openrouter"},
			Instruction: "Answer directly.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var stream bytes.Buffer
	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "hello",
		StreamWriter: &stream,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "completed" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(model.sawStreaming) != 1 || !model.sawStreaming[0] {
		t.Fatalf("expected planner call to use provider streaming, got %#v", model.sawStreaming)
	}
	if stream.String() != "chunk:completed" {
		t.Fatalf("stream = %q, want streamed model chunk", stream.String())
	}
}

func TestRuntimeRunDoesNotCallFinalSteerProviderForDirectFinalAnswer(t *testing.T) {
	model := &scriptedModel{
		responses:    roleDirectResponses("final answer"),
		streamChunks: [][]string{{}},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:           ModelConfig{Provider: "fake"},
			Instruction:     "Answer directly.",
			ForceSimpleLoop: true,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var finalSteerCalls int32
	var stream bytes.Buffer
	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "hello",
		StreamWriter: &stream,
		FinalSteerProvider: func(ctx context.Context) (RunSteerMessage, bool) {
			atomic.AddInt32(&finalSteerCalls, 1)
			return RunSteerMessage{}, false
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "final answer" {
		t.Fatalf("Output = %q, want final answer", result.Output)
	}
	if finalSteerCalls != 0 {
		t.Fatalf("FinalSteerProvider calls = %d, want 0 for context-manager loop", finalSteerCalls)
	}
	if stream.String() != "" {
		t.Fatalf("stream = %q, want empty", stream.String())
	}
}
func TestRuntimeRunStreamsToolCapableCallsWhenWriterIsProvided(t *testing.T) {
	toolSpeech := "我先读取当前音量。"
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, toolSpeech),
			contentResponse("The current audio volume is 42."),
		},
		streamChunks: [][]string{
			{toolSpeech},
			{"The current audio volume is 42."},
		},
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:           ModelConfig{Provider: "openrouter"},
			Instruction:     "Use tools when external state is requested.",
			ForceSimpleLoop: true,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)

	var stream bytes.Buffer
	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "当前音量是多少？",
		StreamWriter: &stream,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Output != "The current audio volume is 42." {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "{}" {
		t.Fatalf("unexpected tool inputs: %#v", tool.inputs)
	}
	if got, want := model.sawStreaming, []bool{true, true}; !slices.Equal(got, want) {
		t.Fatalf("expected tool-capable model calls to use provider streaming, got %#v", got)
	}
	if stream.String() != "我先读取当前音量。The current audio volume is 42." {
		t.Fatalf("stream = %q, want streamed model chunks", stream.String())
	}
}
func TestRuntimeRunKeyboardToolFeedsPostActionScreenshotImage(t *testing.T) {
	jpegBytes := []byte("keyboard-post-action-jpeg")
	model := &scriptedModel{
		responses: roleReviewedToolResponses("keyboard_tap", `{"keys":["enter"]}`, "The keyboard action updated the UI."),
	}
	tool := &stubTool{
		name:        "keyboard_tap",
		description: "Press and release keyboard keys.",
		visual:      true,
		output: `{"action_output":"ok","screen_changed":false,"screen_stable":true,"stable_wait_ms":250,"width":800,"height":600,"format":"jpeg","size":25,"data":"` +
			base64.StdEncoding.EncodeToString(jpegBytes) + `"}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "openrouter"},
			Instruction: "Use input tools when needed.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"keyboard_tap": tool,
		}},
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "press enter"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "The keyboard action updated the UI." {
		t.Fatalf("unexpected output: %q", result.Output)
	}

	var foundToolResponse, foundImage, foundStateNote bool
	for _, msg := range model.messages[1] {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case llms.ToolCallResponse:
				if p.ToolCallID == "call_1" {
					foundToolResponse = true
					if p.Content != "ok" {
						t.Fatalf("keyboard tool response = %q, want original action output", p.Content)
					}
					if strings.Contains(p.Content, base64.StdEncoding.EncodeToString(jpegBytes)) {
						t.Fatalf("keyboard tool response should not inline screenshot payload: %q", p.Content)
					}
				}
			case llms.BinaryContent:
				if p.MIMEType == "image/jpeg" && string(p.Data) == string(jpegBytes) {
					foundImage = true
				}
			case llms.TextContent:
				if strings.Contains(p.Text, "No meaningful visible UI change was detected between the pre-action baseline and the final settled screenshot") {
					foundStateNote = true
				}
			}
		}
	}
	if !foundToolResponse {
		t.Fatalf("expected keyboard tool response in second model call")
	}
	if !foundImage {
		t.Fatalf("expected keyboard post-action screenshot image in second model call")
	}
	if !foundStateNote {
		t.Fatalf("expected keyboard post-action status in the following state message")
	}
}

func TestRuntimeCallbackHandlerCapturesUsageMetrics(t *testing.T) {
	metrics := &RunMetrics{}
	handler := &runtimeCallbackHandler{metrics: metrics}

	handler.HandleLLMGenerateContentEnd(context.Background(), &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			GenerationInfo: map[string]any{
				"prompt_tokens":     12,
				"completion_tokens": 34,
				"total_tokens":      46,
			},
		}},
	})

	if metrics.PromptTokens != 12 || metrics.CompletionTokens != 34 || metrics.TotalTokens != 46 {
		t.Fatalf("unexpected metrics: %#v", metrics)
	}
}

func TestRuntimeRunCapturesUsageMetricsFromDirectModelCall(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			contentResponseWithInfo("completed", map[string]any{
				"prompt_tokens":     600,
				"completion_tokens": 40,
				"total_tokens":      640,
			}),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Metrics == nil || result.Metrics.PromptTokens != 600 || result.Metrics.CompletionTokens != 40 || result.Metrics.TotalTokens != 640 {
		t.Fatalf("unexpected metrics: %#v", result.Metrics)
	}
}

func TestRuntimeCloseStopsStoragePoller(t *testing.T) {
	rt, err := NewRuntime(Config{
		ConfigDir:     ensureTestConfigDir(t, t.TempDir()),
		Model:         ModelConfig{Provider: "fake"},
		Instruction:   "Answer directly.",
		SkillsDirs:    []string{},
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case <-rt.storage.stop:
	default:
		t.Fatal("Close() did not stop the storage poller")
	}
}

func TestNewRuntimeLoadsBundledSkillsSeededOnFirstStartup(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	bundledDir := t.TempDir()
	writeSKILL(t, bundledDir, "alpha", testSkillA)

	runtime, err := NewRuntime(Config{
		ConfigDir:        configDir,
		BundledSkillsDir: bundledDir,
		Model:            ModelConfig{Provider: "fake"},
		Instruction:      "Answer directly.",
		SkillsDirs:       []string{},
		MaxIterations:    1,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer runtime.Close()

	if _, ok := runtime.skills.GetIndex().Get("alpha"); !ok {
		t.Fatalf("expected runtime to load skill copied during startup sync")
	}
}

func numericExtraValue(v interface{}) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case int32:
		return int64(n)
	case uint64:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

// Every chunk returned by recall came from a span already compacted out of the
// live transcript, so all of them count as consulting compressed history.
func TestSessionRecallTelemetryCountsEveryRecalledChunk(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   int64
	}{
		{
			name:   "chunks from several sessions",
			output: `{"results":[{"chunk_id":"chunk_001","source":"session_abc"},{"chunk_id":"chunk_002","source":"session_def"}]}`,
			want:   2,
		},
		{
			name:   "no matching chunks",
			output: `{"results":[]}`,
			want:   0,
		},
		{
			name:   "unparseable output is not counted",
			output: `not json`,
			want:   0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &atomic.Int64{}
			tool := &sessionRecallTelemetryTool{
				inner:   &staticTool{name: "recall_session_chunks", output: tc.output},
				counter: counter,
			}
			if _, err := tool.Call(context.Background(), `{}`); err != nil {
				t.Fatalf("Call() error = %v", err)
			}
			if got := counter.Load(); got != tc.want {
				t.Fatalf("recall count = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRuntimeRegistersMemoryRecallToolsWhenConfigDirSet(t *testing.T) {
	runtime, err := NewRuntime(Config{
		ConfigDir:     t.TempDir(),
		Model:         ModelConfig{Provider: "fake"},
		Instruction:   "Answer directly.",
		SkillsDirs:    []string{},
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer runtime.Close()

	if _, ok := runtime.tools.Get("recall_session_chunks"); !ok {
		t.Fatalf("expected runtime to register recall_session_chunks")
	}
	if _, ok := runtime.tools.Get("recall_memory"); !ok {
		t.Fatalf("expected runtime to register recall_memory")
	}
}

func TestRuntimeRunOmitsMemoryFilesFromSystemPrompt(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	summary := "SESSION SUMMARY SENTINEL"
	profile := "PROFILE SENTINEL"

	sessionDir := filepath.Join(configDir, "memory", "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "summary.md"), []byte(summary), 0o644); err != nil {
		t.Fatalf("WriteFile summary.md: %v", err)
	}

	longTermDir := filepath.Join(configDir, "memory", "long_term")
	if err := os.MkdirAll(longTermDir, 0o755); err != nil {
		t.Fatalf("MkdirAll long_term: %v", err)
	}
	if err := os.WriteFile(filepath.Join(longTermDir, "profile.md"), []byte(profile), 0o644); err != nil {
		t.Fatalf("WriteFile profile.md: %v", err)
	}

	model := &scriptedModel{
		responses: roleDirectResponses("ok"),
	}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:     configDir,
			Model:         ModelConfig{Provider: "fake"},
			Instruction:   "Answer directly.",
			MaxIterations: 1,
		},
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.messages) != 1 || len(model.messages[0]) == 0 {
		t.Fatalf("expected one default-mode planner call with messages, got %#v", model.messages)
	}

	systemMessage := model.messages[0][0]
	if systemMessage.Role != llms.ChatMessageTypeSystem {
		t.Fatalf("expected first message to be system, got %q", systemMessage.Role)
	}
	var systemText strings.Builder
	for _, part := range systemMessage.Parts {
		text, ok := part.(llms.TextContent)
		if ok {
			systemText.WriteString(text.Text)
		}
	}
	for _, unexpected := range []string{summary, profile} {
		if strings.Contains(systemText.String(), unexpected) {
			t.Fatalf("system message should not include memory file %q:\n%s", unexpected, systemText.String())
		}
	}
}

func TestRuntimeRunPlacesSystemPromptBeforeCurrentUserMessage(t *testing.T) {
	model := &scriptedModel{
		responses: roleDirectResponses("processed"),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use context when answering.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	const userText = "REAL_USER_REQUEST_MARKER"
	if _, err := runtime.Run(context.Background(), RunRequest{
		Input: userText,
		Attachments: []InputAttachment{
			{
				Kind:     AttachmentKindImage,
				Name:     "photo.png",
				MIMEType: "image/png",
				Data:     []byte{0x89, 0x50, 0x4e, 0x47},
			},
		},
		DeviceEnvironment: &PhoneEnvironment{
			Platform:      "ios",
			SystemName:    "iOS",
			SystemVersion: "18.0",
		},
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.messages) != 1 {
		t.Fatalf("expected one model call, got %d", len(model.messages))
	}

	messages := model.messages[0]
	if len(messages) < 2 {
		t.Fatalf("expected system and current user messages, got %#v", messages)
	}
	if messages[0].Role != llms.ChatMessageTypeSystem {
		t.Fatalf("first message role = %q, want system", messages[0].Role)
	}
	userMessage := messages[len(messages)-1]
	if userMessage.Role != llms.ChatMessageTypeHuman {
		t.Fatalf("current user message role = %q, want human", userMessage.Role)
	}
	userPrompt := messageText([]llms.MessageContent{userMessage})
	if !strings.Contains(userPrompt, userText) || !strings.Contains(userPrompt, "Attached content") || !strings.Contains(userPrompt, "photo.png") {
		t.Fatalf("current user message missing attachment-aware prompt: %q", userPrompt)
	}
	var userHasImageURL, userHasImageBinary bool
	for _, part := range userMessage.Parts {
		switch typed := part.(type) {
		case llms.ImageURLContent:
			userHasImageURL = true
		case llms.BinaryContent:
			if typed.MIMEType == "image/png" && string(typed.Data) == string([]byte{0x89, 0x50, 0x4e, 0x47}) {
				userHasImageBinary = true
			}
		}
	}
	if userHasImageURL {
		t.Fatalf("current user message unexpectedly retained separate image URL part: %#v", userMessage.Parts)
	}
	if !userHasImageBinary {
		t.Fatalf("current user message missing image binary part: %#v", userMessage.Parts)
	}
}

func TestRuntimeRunIncludesUserAttachments(t *testing.T) {
	model := &scriptedModel{
		responses: roleDirectResponses("processed"),
	}

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "openrouter"},
			Instruction: "Use the provided media when answering.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "Describe the uploaded media.",
		Attachments: []InputAttachment{
			{
				Kind:     AttachmentKindImage,
				Name:     "photo.png",
				MIMEType: "image/png",
				Data:     []byte{0x89, 0x50, 0x4e, 0x47},
			},
			{
				Kind:     AttachmentKindAudio,
				Name:     "note.wav",
				MIMEType: "audio/wav",
				Data:     []byte{0x52, 0x49, 0x46, 0x46},
			},
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "processed" {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if len(model.messages) != 1 {
		t.Fatalf("expected 1 default-mode planner call, got %d", len(model.messages))
	}

	lastCall := model.messages[0]
	if len(lastCall) < 2 {
		t.Fatalf("expected messages in model call")
	}
	userMessage := lastCall[len(lastCall)-1]
	if userMessage.Role != llms.ChatMessageTypeHuman {
		t.Fatalf("expected raw user message to be human, got %q", userMessage.Role)
	}

	var textContent string
	var imageURL string
	var imageBinary, audioBinary bool
	for _, part := range userMessage.Parts {
		switch p := part.(type) {
		case llms.TextContent:
			textContent = p.Text
		case llms.ImageURLContent:
			imageURL = p.URL
		case llms.BinaryContent:
			if p.MIMEType == "image/png" && string(p.Data) == string([]byte{0x89, 0x50, 0x4e, 0x47}) {
				imageBinary = true
			}
			if p.MIMEType == "audio/wav" && string(p.Data) == string([]byte{0x52, 0x49, 0x46, 0x46}) {
				audioBinary = true
			}
		}
	}

	for _, expected := range []string{"Describe the uploaded media.", "Attached content", "photo.png", "note.wav"} {
		if !strings.Contains(textContent, expected) {
			t.Fatalf("agent user message text missing %q: %q", expected, textContent)
		}
	}
	if strings.Contains(textContent, "data:image/png;base64,") {
		t.Fatalf("image attachment should not be kept inline in text: %q", textContent)
	}
	if imageURL != "" {
		t.Fatalf("image attachment should be folded into text by context manager bridge, got %q", imageURL)
	}
	if !imageBinary || !audioBinary {
		t.Fatalf("missing binary attachment parts: image=%v audio=%v parts=%#v", imageBinary, audioBinary, userMessage.Parts)
	}
}

func TestRuntimeClearMemoryRemovesPersistedSession(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	runtime, err := NewRuntime(Config{
		ConfigDir:     configDir,
		Model:         ModelConfig{Provider: "fake"},
		Instruction:   "Answer directly.",
		SkillsDirs:    []string{},
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer runtime.Close()
	if err := os.MkdirAll(agentpath.ContextManagerSessionFolder(configDir), 0o755); err != nil {
		t.Fatalf("MkdirAll sessions dir: %v", err)
	}

	runtime.models = &testModelResolver{
		model: &scriptedModel{responses: roleDirectResponses("first")},
	}

	if _, err := runtime.Run(context.Background(), RunRequest{Input: "hello"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	oldBackendSessionID := runtime.ContextDump().SessionID

	if err := runtime.ClearMemory(context.Background()); err != nil {
		t.Fatalf("ClearMemory() error = %v", err)
	}

	backendFolder := agentpath.ContextManagerSessionFolder(configDir)
	oldBackendSessionPath := filepath.Join(backendFolder, oldBackendSessionID+".jsonl")
	if _, err := os.Stat(oldBackendSessionPath); !os.IsNotExist(err) {
		t.Fatalf("old backend context session file should be removed, stat err = %v", err)
	}
	newBackendSession := runtime.ContextDump()
	if newBackendSession.SessionID == "" || newBackendSession.SessionID == oldBackendSessionID {
		t.Fatalf("backend context session was not replaced: old=%q new=%q", oldBackendSessionID, newBackendSession.SessionID)
	}
	if newBackendSession.ParentSessionID != "" {
		t.Fatalf("cleared backend context parent session = %q, want root session", newBackendSession.ParentSessionID)
	}
}

func TestRuntimeInitialRotateCreatesOneContextSession(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, Instruction: "system"},
		nil,
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	defer runtime.Close()

	if err := runtime.rotateContext(); err != nil {
		t.Fatalf("rotateContext() error = %v", err)
	}
	files, err := filepath.Glob(filepath.Join(agentpath.ContextManagerSessionFolder(configDir), "*.jsonl"))
	if err != nil {
		t.Fatalf("glob context sessions: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("initial rotate created %d transcript sessions, want 1: %#v", len(files), files)
	}
}

func TestRuntimeClearMemoryReplacesAndRemovesUserContextSession(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	runtime, err := NewRuntime(Config{
		ConfigDir: configDir,
		Model:     ModelConfig{Provider: "fake"},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer runtime.Close()

	folder := agentpath.UserContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManager(folder, "realtime system prompt")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	oldSessionID := manager.GetSessionID()
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "old user context"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	if err := runtime.ClearMemory(context.Background()); err != nil {
		t.Fatalf("ClearMemory() error = %v", err)
	}
	rotated, err := contextmanager.LoadContextManagerFromCurrentSession(folder)
	if err != nil {
		t.Fatalf("load rotated user context: %v", err)
	}
	if rotated.GetSessionID() == oldSessionID {
		t.Fatalf("user context session was not rotated: %q", oldSessionID)
	}
	oldUserSessionPath := filepath.Join(folder, oldSessionID+".jsonl")
	if _, err := os.Stat(oldUserSessionPath); !os.IsNotExist(err) {
		t.Fatalf("old user context session file should be removed, stat err = %v", err)
	}
	if rotated.GetParentSessionID() != "" {
		t.Fatalf("cleared user context parent session = %q, want root session", rotated.GetParentSessionID())
	}
	rotatedMessages := rotated.MessageListDump().Messages
	if len(rotatedMessages) != 1 || rotatedMessages[0].Role != messages.MessageRoleSystem || rotatedMessages[0].Content != "realtime system prompt" {
		t.Fatalf("rotated user context = %#v, want only preserved system prompt", rotatedMessages)
	}
}

func TestRuntimeClearMemoryPreservesSessionsWhenUserContextCannotLoad(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	runtime, err := NewRuntime(Config{
		ConfigDir: configDir,
		Model:     ModelConfig{Provider: "fake"},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer runtime.Close()

	userFolder := agentpath.UserContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManager(userFolder, "realtime system prompt")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	userSessionPath := filepath.Join(userFolder, manager.GetSessionID()+".jsonl")
	if err := os.WriteFile(userSessionPath, []byte("{invalid json\n"), 0o644); err != nil {
		t.Fatalf("corrupt user context session: %v", err)
	}
	backendFolder := agentpath.ContextManagerSessionFolder(configDir)
	backendManager, err := contextmanager.NewContextManager(backendFolder, "backend system prompt")
	if err != nil {
		t.Fatalf("create backend context session: %v", err)
	}
	backendSessionPath := filepath.Join(backendFolder, backendManager.GetSessionID()+".jsonl")

	if err := runtime.ClearMemory(context.Background()); err == nil {
		t.Fatal("ClearMemory() error = nil, want user context load error")
	}
	for _, path := range []string{userSessionPath, backendSessionPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("session should remain after user context load failure, stat %s: %v", path, err)
		}
	}
}

func TestRuntimeClearMemoryResetsActiveUserContext(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	reset := make(chan struct{}, 1)
	unregister := runtime.RegisterUserContextResetHook(func() { reset <- struct{}{} })
	defer unregister()

	if err := runtime.ClearMemory(context.Background()); err != nil {
		t.Fatalf("ClearMemory() error = %v", err)
	}
	select {
	case <-reset:
	default:
		t.Fatal("active user context reset hook was not called")
	}
}

func TestRuntimePreemptCancelsActiveRun(t *testing.T) {
	t.Parallel()

	// Model that blocks until context is canceled.
	model := &blockingFirstCallModel{
		firstCallStarted: make(chan struct{}),
		releaseFirstCall: make(chan struct{}),
		responses:        []*llms.ContentResponse{contentResponse("first"), contentResponse("second")},
	}

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	// Start first run in background.
	firstDone := make(chan struct{})
	var firstErr error
	go func() {
		defer close(firstDone)
		_, firstErr = runtime.Run(context.Background(), RunRequest{Input: "first task"})
	}()

	// Wait for first run to actually start the model call.
	select {
	case <-model.firstCallStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start model call")
	}

	// Start second run — should preempt the first.
	secondResult, secondErr := runtime.Run(context.Background(), RunRequest{Input: "second task"})

	// First run should have been canceled.
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not finish after preemption")
	}
	if firstErr == nil {
		t.Fatal("expected first run to return error after preemption")
	}

	// Second run should succeed.
	if secondErr != nil {
		t.Fatalf("second run error = %v", secondErr)
	}
	if secondResult.Output != "second" {
		t.Fatalf("second run output = %q, want 'second'", secondResult.Output)
	}

	requireInterruptNotices(t, runtime.contextManager, "preempted")

	// WasPreempted should report true.
	if !runtime.WasPreempted(5 * time.Second) {
		t.Fatal("WasPreempted should return true after preemption")
	}
}

func TestRuntimePreemptHooksAreCalled(t *testing.T) {
	t.Parallel()

	model := &blockingFirstCallModel{
		firstCallStarted: make(chan struct{}),
		releaseFirstCall: make(chan struct{}),
		responses:        []*llms.ContentResponse{contentResponse("first"), contentResponse("second")},
	}

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var hookCalled atomic.Int32
	runtime.RegisterPreemptHook(func() {
		hookCalled.Add(1)
	})

	// Start first run.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		runtime.Run(context.Background(), RunRequest{Input: "first"})
	}()

	select {
	case <-model.firstCallStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}

	// Second run triggers preemption.
	runtime.Run(context.Background(), RunRequest{Input: "second"})

	if hookCalled.Load() == 0 {
		t.Fatal("preempt hook was not called")
	}

	// Wait for first run to complete.
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not complete")
	}
}

func TestRuntimeOnRunActiveRunsAfterStartupPreemption(t *testing.T) {
	model := &scriptedModel{responses: []*llms.ContentResponse{contentResponse("done")}}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var order []string
	runtime.RegisterPreemptHook(func() {
		order = append(order, "preempt")
	})

	result, err := runtime.Run(context.Background(), RunRequest{
		Input: "test callback order",
		OnRunActive: func(context.Context) {
			order = append(order, "active")
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != "done" {
		t.Fatalf("Output = %q, want done", result.Output)
	}
	if !reflect.DeepEqual(order, []string{"preempt", "active"}) {
		t.Fatalf("callback order = %#v, want preempt then active", order)
	}
}

func TestRuntimeFinalizesStreamingStateAtEachLLMResponseBoundary(t *testing.T) {
	first := "<tts>Retrying.</tts>Malformed tool response."
	final := "<tts>Completed.</tts>Completed."
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			{Choices: []*llms.ContentChoice{{
				Content:   first,
				ToolCalls: []llms.ToolCall{{ID: "malformed"}},
			}}},
			contentResponse(final),
		},
		streamChunks: [][]string{{first}, {final}},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var speech strings.Builder
	writer := speechtext.NewStreamWriter(&speech)
	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "retry malformed output",
		StreamWriter: writer,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Output != final {
		t.Fatalf("Output = %q, want %q", result.Output, final)
	}
	if got := speech.String(); got != "Retrying.Completed." {
		t.Fatalf("streamed speech = %q, want both response-level TTS blocks", got)
	}
	if emitted, ok := writer.ConsumeFinishedResponse(); !ok || !emitted {
		t.Fatalf("final response emission = (%v, %v), want (true, true)", emitted, ok)
	}
}

func TestRuntimePreemptHookPanicDoesNotStopOtherHooks(t *testing.T) {
	t.Parallel()

	model := &blockingFirstCallModel{
		firstCallStarted: make(chan struct{}),
		releaseFirstCall: make(chan struct{}),
		responses:        []*llms.ContentResponse{contentResponse("first"), contentResponse("second")},
	}

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var hook1Called atomic.Int32
	var hook2Called atomic.Int32
	var hook3Called atomic.Int32

	// Register hooks: first and third work normally, second panics.
	runtime.RegisterPreemptHook(func() {
		hook1Called.Add(1)
	})
	runtime.RegisterPreemptHook(func() {
		hook2Called.Add(1)
		panic("intentional test panic")
	})
	runtime.RegisterPreemptHook(func() {
		hook3Called.Add(1)
	})

	// Start first run.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		runtime.Run(context.Background(), RunRequest{Input: "first"})
	}()

	select {
	case <-model.firstCallStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not start")
	}

	// Second run triggers preemption with panicking hook.
	runtime.Run(context.Background(), RunRequest{Input: "second"})

	// All hooks should be called despite the panic in hook 2.
	if hook1Called.Load() == 0 {
		t.Error("hook 1 was not called")
	}
	if hook2Called.Load() == 0 {
		t.Error("hook 2 was not called (should have been called before panic)")
	}
	if hook3Called.Load() == 0 {
		t.Error("hook 3 was not called (panic in hook 2 should not stop subsequent hooks)")
	}

	// Wait for first run to complete.
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not complete")
	}
}

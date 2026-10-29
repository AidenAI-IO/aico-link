package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/realtimevoice"
	"aiden-agent/internal/agenttask"
	"aiden-agent/internal/logging"

	langtools "github.com/tmc/langchaingo/tools"
)

func TestDaemonDeviceTypeFlag(t *testing.T) {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	deviceType := registerDeviceTypeFlag(fs)
	if err := fs.Parse([]string{"--device-type", "android"}); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if *deviceType != "android" {
		t.Fatalf("device type flag = %q, want android", *deviceType)
	}
}

func TestRealtimeSessionConfigUsesAgentPrompt(t *testing.T) {
	emotion := false
	cfg := agent.Config{
		Prompt: "speak naturally",
		VoiceModel: agent.VoiceModelConfig{
			Provider:               realtimevoice.ProviderQwen,
			Voice:                  "custom-voice",
			EnableSpeechEmotion:    &emotion,
			InputAudioFormat:       "pcm",
			OutputAudioFormat:      "pcm",
			TurnDetection:          "smart_turn",
			TurnDetectionThreshold: floatPtr(0.2),
			TurnDetectionSilenceMs: 900,
		},
	}
	got := realtimeProviderSessionConfig(cfg, nil)
	wantInstructionsPrefix := agent.DefaultRealtimeToolExecutionInstructions + "\n\n" + cfg.Prompt
	if got.Voice != "custom-voice" || !strings.HasPrefix(got.Instructions, wantInstructionsPrefix) || got.TurnDetection != "smart_turn" {
		t.Fatalf("unexpected realtime session config: %+v", got)
	}
	if got.TurnDetectionSilenceMs != 900 || got.TurnDetectionThresh == nil || *got.TurnDetectionThresh != 0.2 {
		t.Fatalf("unexpected turn detection config: %+v", got.TurnDetection)
	}
	if got.EnableSpeechEmotion == nil || *got.EnableSpeechEmotion {
		t.Fatalf("enable_speech_emotion = %#v, want false", got.EnableSpeechEmotion)
	}
	wantTools := []string{
		"get_current_time", "recall_memory", "save_memory", "forget_memory",
		"recall_session_chunks", "audio_volume", "end_conversation",
	}
	if len(got.Tools) != len(wantTools) {
		t.Fatalf("realtime tools = %#v, want %v", got.Tools, wantTools)
	}
	for i, want := range wantTools {
		if got.Tools[i].Name != want {
			t.Fatalf("realtime tool[%d] = %q, want %q", i, got.Tools[i].Name, want)
		}
	}
}

func TestRealtimeSessionConfigSwitchesBackendCommunicationTools(t *testing.T) {
	backendTools := map[string]bool{
		realtimeCreateTaskTool:         true,
		realtimeCancelTaskTool:         true,
		realtimeUpdateTaskTool:         true,
		realtimeQueryTaskTool:          true,
		realtimeResponseUserActionTool: true,
	}
	backendDisabledConfigs := []agent.Config{
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live-extended-thinking"}},
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live"}},
	}
	for _, cfg := range backendDisabledConfigs {
		got := realtimeProviderSessionConfig(cfg, nil)
		for _, tool := range got.Tools {
			if backendTools[tool.Name] {
				t.Fatalf("backend-disabled setup exposed communication tool %q", tool.Name)
			}
		}
		if strings.Contains(got.Instructions, "query_agent_task") {
			t.Fatalf("backend-disabled instructions still reference communication tools: %s", got.Instructions)
		}
	}

	retainedConfigs := []struct {
		name string
		cfg  agent.Config
	}{
		{name: "thinking model", cfg: agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live-extended-thinking", UseBackendAgent: true}}},
		{name: "regular 3.8 live", cfg: agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live", UseBackendAgent: true}}},
		{
			name: "named provider",
			cfg: agent.Config{
				VoiceModel:          agent.VoiceModelConfig{Provider: "gemini-main", UseBackendAgent: true},
				VoiceModelProviders: map[string]agent.VoiceModelProvider{"gemini-main": {Type: "gemini", Model: "models/gemini-3.8-live-extended-thinking"}},
			},
		},
	}
	for _, tc := range retainedConfigs {
		t.Run(tc.name, func(t *testing.T) {
			got := realtimeProviderSessionConfig(tc.cfg, nil)
			toolNames := make(map[string]bool, len(got.Tools))
			for _, tool := range got.Tools {
				toolNames[tool.Name] = true
			}
			for name := range backendTools {
				if !toolNames[name] {
					t.Fatalf("backend-agent mode lost tool %q", name)
				}
			}
			if !strings.Contains(got.Instructions, "query_agent_task") {
				t.Fatalf("backend-agent mode lost backend instructions: %s", got.Instructions)
			}
		})
	}
}

func TestRealtimeSessionConfigTransfersRuntimeToolsWhenBackendDisabled(t *testing.T) {
	toolSet := agent.NewBuiltinToolSet(
		agent.HIDConfig{},
		agent.AudioConfig{},
		agent.SearchConfig{},
		agent.ProxyConfig{},
		agent.WithWaitForWakeupController(agent.NewWaitForWakeupController()),
	)
	runtime := agent.NewRuntimeWithDeps(agent.Config{}, nil, nil, toolSet, agent.NewSkillIndex())
	defer runtime.Close()

	backendDisabledConfigs := []agent.Config{
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live-extended-thinking"}},
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live"}},
	}
	for _, cfg := range backendDisabledConfigs {
		t.Run(cfg.VoiceModel.Model, func(t *testing.T) {
			got := realtimeProviderSessionConfig(cfg, runtime)
			definitions := make(map[string]realtimevoice.Tool, len(got.Tools))
			counts := make(map[string]int, len(got.Tools))
			for _, definition := range got.Tools {
				definitions[definition.Name] = definition
				counts[definition.Name]++
			}
			for _, name := range []string{"weather", "web_search", "screenshot"} {
				if _, ok := definitions[name]; !ok {
					t.Fatalf("backend-disabled setup missing transferred runtime tool %q", name)
				}
			}
			for _, name := range []string{realtimeCreateTaskTool, realtimeCancelTaskTool, realtimeUpdateTaskTool, realtimeQueryTaskTool, realtimeResponseUserActionTool, "request_user_action", "wait_for_wakeup"} {
				if _, ok := definitions[name]; ok {
					t.Fatalf("backend-disabled setup exposed control tool %q", name)
				}
			}
			if counts[realtimeAudioVolumeTool] != 1 {
				t.Fatalf("audio_volume declaration count = %d, want 1", counts[realtimeAudioVolumeTool])
			}
			if cfg.VoiceModel.Model == "gemini-3.8-live-extended-thinking" {
				weather, ok := runtime.Tool("weather")
				if !ok {
					t.Fatal("runtime is missing weather tool")
				}
				wantSchema, err := json.Marshal(agent.NewToolSpec(weather).LLMSchema())
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(definitions["weather"].Parameters, wantSchema) {
					t.Fatalf("weather schema = %s, want %s", definitions["weather"].Parameters, wantSchema)
				}
			}
		})
	}

	for _, model := range []string{"gemini-3.8-live-extended-thinking", "gemini-3.8-live"} {
		retained := realtimeProviderSessionConfig(agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: model, UseBackendAgent: true}}, runtime)
		retainedNames := make(map[string]bool, len(retained.Tools))
		for _, definition := range retained.Tools {
			retainedNames[definition.Name] = true
		}
		for _, name := range []string{"weather", "web_search", "screenshot"} {
			if retainedNames[name] {
				t.Fatalf("backend-agent mode unexpectedly exposed runtime tool %q for model %q", name, model)
			}
		}
		for _, name := range []string{realtimeCreateTaskTool, realtimeCancelTaskTool, realtimeUpdateTaskTool, realtimeQueryTaskTool, realtimeResponseUserActionTool} {
			if !retainedNames[name] {
				t.Fatalf("backend-agent mode lost communication tool %q for model %q", name, model)
			}
		}
	}
}

func TestRealtimeAgentTaskManagerFollowsUseBackendAgent(t *testing.T) {
	for _, cfg := range []agent.Config{
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live-extended-thinking"}},
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live"}},
	} {
		if manager := newRealtimeAgentTaskManager(cfg, nil); manager != nil {
			manager.Close()
			t.Fatal("backend-disabled configuration unexpectedly created a backend-agent task manager")
		}
	}

	for _, cfg := range []agent.Config{
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live", UseBackendAgent: true}},
		{VoiceModel: agent.VoiceModelConfig{Provider: "gemini", Model: "gemini-3.8-live-extended-thinking", UseBackendAgent: true}},
		{
			VoiceModel:          agent.VoiceModelConfig{Provider: "gemini-main", UseBackendAgent: true},
			VoiceModelProviders: map[string]agent.VoiceModelProvider{"gemini-main": {Type: "gemini", Model: "gemini-3.8-live-extended-thinking"}},
		},
	} {
		manager := newRealtimeAgentTaskManager(cfg, nil)
		if manager == nil {
			t.Fatalf("backend-agent configuration did not create its task manager: %+v", cfg.VoiceModel)
		}
		manager.Close()
	}
}

func TestRealtimeSessionConfigOnlyAppliesTurnDetectionToQwen(t *testing.T) {
	threshold := 0.2
	for _, provider := range []string{"openai", "gemini", "xai", "speko"} {
		t.Run(provider, func(t *testing.T) {
			cfg := agent.Config{VoiceModel: agent.VoiceModelConfig{
				Provider: provider, TurnDetection: "smart_turn",
				TurnDetectionThreshold: &threshold, TurnDetectionSilenceMs: 900,
			}}
			got := realtimeProviderSessionConfig(cfg, nil)
			if got.TurnDetection != "" || got.TurnDetectionThresh != nil || got.TurnDetectionSilenceMs != 0 {
				t.Fatalf("%s received Qwen turn detection settings: %+v", provider, got)
			}
		})
	}

	got := realtimeProviderSessionConfig(agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: realtimevoice.ProviderQwen}}, nil)
	if got.TurnDetection != "server_vad" {
		t.Fatalf("Qwen default turn detection = %q, want server_vad", got.TurnDetection)
	}
}

func TestRealtimeDelegatedToolSchemasMatchRuntimeTools(t *testing.T) {
	canonical := []langtools.Tool{
		agent.NewRecallMemoryTool(nil),
		agent.NewSaveMemoryTool(nil),
		agent.NewForgetMemoryTool(nil),
		agent.NewRecallSessionChunksTool(nil),
		agent.NewAudioVolumeTool(""),
	}
	definitions := make(map[string]realtimevoice.Tool)
	for _, definition := range realtimeVoiceToolDefinitions(agent.Config{}, nil) {
		definitions[definition.Name] = definition
	}
	for _, tool := range canonical {
		t.Run(tool.Name(), func(t *testing.T) {
			definition, ok := definitions[tool.Name()]
			if !ok {
				t.Fatalf("realtime tool definition missing %q", tool.Name())
			}
			want, err := json.Marshal(agent.NewToolSpec(tool).LLMSchema())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(definition.Parameters, want) {
				t.Fatalf("realtime schema for %s = %s, want %s", tool.Name(), definition.Parameters, want)
			}
		})
	}
}

func TestRealtimeSessionConfigUsesDedicatedDefaultInstructions(t *testing.T) {
	cfg := agent.Config{Instruction: "legacy phone automation prompt", Prompt: "Use concise replies."}
	got := realtimeProviderSessionConfig(cfg, nil)
	wantPrefix := agent.DefaultRealtimeToolExecutionInstructions + "\n\n" + cfg.Prompt
	if !strings.HasPrefix(got.Instructions, wantPrefix) {
		t.Fatalf("instructions = %q, want direct-tool default followed by configured prompt and shared guidance", got.Instructions)
	}
	if strings.Contains(got.Instructions, cfg.Instruction) {
		t.Fatal("realtime session reused the legacy agent instruction")
	}
}

func TestRealtimeSessionConfigIncludesConfiguredResponseLanguage(t *testing.T) {
	for _, tc := range []struct {
		locale, language string
	}{
		{locale: "zh-CN", language: "Simplified Chinese"},
		{locale: "en-US", language: "English"},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			cfg := agent.Config{Locale: tc.locale, Prompt: "speak naturally"}
			got := realtimeProviderSessionConfig(cfg, nil)
			if !strings.Contains(got.Instructions, "The configured response locale is "+tc.locale+".") {
				t.Fatalf("realtime instructions missing configured locale: %q", got.Instructions)
			}
			if !strings.Contains(got.Instructions, "Always respond in "+tc.language) {
				t.Fatalf("realtime instructions missing fixed response language: %q", got.Instructions)
			}
		})
	}
}

func TestRealtimeSessionConfigPreservesSpekoAutomaticVoice(t *testing.T) {
	cfg := agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: "speko"}}
	got := realtimeProviderSessionConfig(cfg, nil)
	if got.Voice != "" {
		t.Fatalf("Speko voice = %q, want empty automatic selection", got.Voice)
	}
}

func TestRealtimeSessionConfigResolvesNamedSpekoProviderForDefaults(t *testing.T) {
	cfg := agent.Config{
		VoiceModel:          agent.VoiceModelConfig{Provider: "speko-main"},
		VoiceModelProviders: map[string]agent.VoiceModelProvider{"speko-main": {Type: "speko"}},
	}
	got := realtimeProviderSessionConfig(cfg, nil)
	if got.Voice != "" {
		t.Fatalf("named Speko voice = %q, want empty automatic selection", got.Voice)
	}
}

func TestRealtimeSessionConfigLeavesProviderNativeRatesToAdapters(t *testing.T) {
	cases := []struct {
		provider, upstream string
	}{
		{provider: "qwen"},
		{provider: "gemini"},
		{provider: "openai"},
		{provider: "xai"},
		{provider: "speko", upstream: "google"},
		{provider: "speko", upstream: "xai"},
		{provider: "speko"},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.upstream, func(t *testing.T) {
			cfg := agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: tc.provider, UpstreamProvider: tc.upstream}}
			got := realtimeProviderSessionConfig(cfg, nil)
			if got.InputSampleRate != 0 || got.OutputSampleRate != 0 {
				t.Fatalf("provider rates = %d/%d, want adapter-owned defaults", got.InputSampleRate, got.OutputSampleRate)
			}
		})
	}
}

func TestRealtimeSessionConfigDoesNotApplyQwenVoiceToNativeProviders(t *testing.T) {
	for _, provider := range []string{"openai", "gemini", "xai"} {
		t.Run(provider, func(t *testing.T) {
			cfg := agent.Config{VoiceModel: agent.VoiceModelConfig{Provider: provider}}
			got := realtimeProviderSessionConfig(cfg, nil)
			if got.Voice != "" {
				t.Fatalf("voice = %q, want native provider default", got.Voice)
			}
		})
	}
}

func TestRealtimeClientTurnEndpointDetectsSilenceAfterSpeech(t *testing.T) {
	endpoint := newRealtimeClientTurnEndpoint(100)
	speech := make([]byte, 640)
	for i := 0; i < len(speech); i += 2 {
		binary.LittleEndian.PutUint16(speech[i:], uint16(1000))
	}
	start := time.Unix(0, 0)
	if !endpoint.Observe(speech, start) {
		t.Fatal("speech PCM was not detected")
	}
	if endpoint.Due(start.Add(99 * time.Millisecond)) {
		t.Fatal("endpoint became due before the configured silence duration")
	}
	if !endpoint.Due(start.Add(100 * time.Millisecond)) {
		t.Fatal("endpoint did not become due after the configured silence duration")
	}
	endpoint.Reset()
	if endpoint.Due(start.Add(time.Second)) {
		t.Fatal("reset endpoint remained active")
	}
}

func TestRealtimeInstructionsRouteVisualRequestsToDeviceWork(t *testing.T) {
	instructions := agent.DefaultRealtimeVoiceInstructions
	for _, phrase := range []string{"cannot directly and reliably answer", "screen or page content", "Never refuse merely because"} {
		if !strings.Contains(instructions, phrase) {
			t.Fatalf("realtime instructions missing visual-routing guidance %q: %s", phrase, instructions)
		}
	}
}

func TestRealtimeCurrentTimeTool(t *testing.T) {
	want := time.Date(2026, time.August, 20, 14, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	executor := realtimeVoiceToolExecutor{now: func() time.Time { return want }}
	output := executor.call(context.Background(), realtimeCurrentTimeTool, `{}`)
	var got struct {
		Datetime         string `json:"datetime"`
		Timezone         string `json:"timezone"`
		UTCOffsetSeconds int    `json:"utc_offset_seconds"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if got.Datetime != "2026-08-20T14:30:00+08:00" || got.Timezone != "CST" || got.UTCOffsetSeconds != 8*60*60 {
		t.Fatalf("current time output = %+v", got)
	}
}

func TestRealtimeRecallToolDelegatesToRuntimeTool(t *testing.T) {
	recall := &fakeRealtimeTool{output: `{"results":[{"content":"likes concise replies"}]}`}
	executor := realtimeVoiceToolExecutor{
		delegated: map[string]langtools.Tool{realtimeRecallTool: recall},
		now:       time.Now,
	}
	input := `{"tags":["preference"],"limit":3}`
	if got := executor.call(context.Background(), realtimeRecallTool, input); got != recall.output {
		t.Fatalf("recall output = %q, want %q", got, recall.output)
	}
	if recall.input != input {
		t.Fatalf("recall input = %q, want %q", recall.input, input)
	}
}

func TestRealtimeDelegatedToolsReuseRuntimeImplementations(t *testing.T) {
	for _, name := range []string{
		realtimeSaveMemoryTool,
		realtimeForgetMemoryTool,
		realtimeRecallChunksTool,
		realtimeAudioVolumeTool,
	} {
		t.Run(name, func(t *testing.T) {
			tool := &fakeRealtimeTool{output: `{"status":"saved"}`}
			executor := realtimeVoiceToolExecutor{
				delegated: map[string]langtools.Tool{name: tool},
				now:       time.Now,
			}
			input := `{"content":"prefers metric units"}`
			if got := executor.call(context.Background(), name, input); got != tool.output {
				t.Fatalf("%s output = %q, want %q", name, got, tool.output)
			}
			if tool.input != input {
				t.Fatalf("%s input = %q, want %q", name, tool.input, input)
			}
		})
	}
}

func TestRealtimeRuntimeToolCallsReuseUnderlyingTool(t *testing.T) {
	tool := &fakeRealtimeTool{name: "weather", output: `{"location":"Guangzhou"}`}
	executor := realtimeVoiceToolExecutor{
		delegated: map[string]langtools.Tool{"weather": tool},
		now:       time.Now,
	}
	input := `{"location":"Guangzhou"}`
	if got := executor.call(context.Background(), "weather", input); got != tool.output {
		t.Fatalf("weather output = %q, want %q", got, tool.output)
	}
	if tool.input != input {
		t.Fatalf("weather input = %q, want %q", tool.input, input)
	}
}

func TestRealtimeDelegatedToolReportsUnavailableWithoutRuntime(t *testing.T) {
	executor := realtimeVoiceToolExecutor{now: time.Now}
	for _, name := range realtimeDelegatedTools {
		output := executor.call(context.Background(), name, `{}`)
		var got struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(output), &got); err != nil {
			t.Fatalf("decode %s output: %v", name, err)
		}
		if !strings.Contains(got.Error, name) {
			t.Fatalf("%s error = %q, want it to name the tool", name, got.Error)
		}
	}
}

func TestRealtimeEndConversationToolDefersTeardownToSessionLoop(t *testing.T) {
	executor := realtimeVoiceToolExecutor{now: time.Now}
	output := executor.call(context.Background(), realtimeEndConversationTool, `{}`)
	var got struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	// The tool must succeed without a runtime or task manager: the session
	// loop performs the teardown after the farewell finishes speaking.
	if got.Status != "ending" || got.Error != "" {
		t.Fatalf("end_conversation output = %+v", got)
	}
}

func TestRealtimeInstructionsCoverMemoryWritesAndStandby(t *testing.T) {
	instructions := agent.DefaultRealtimeVoiceInstructions
	for _, phrase := range []string{"save_memory", "forget_memory", "recall_session_chunks", "audio_volume", "end_conversation", "query_agent_task", "update_agent_task", "stop listening", "stop the service", "go idle", "wait for the next wakeup", "conversation-control requests"} {
		if !strings.Contains(instructions, phrase) {
			t.Fatalf("realtime instructions missing guidance for %q: %s", phrase, instructions)
		}
	}
}

func TestRealtimeAgentTaskToolsAreNonBlocking(t *testing.T) {
	runner := &fakeBackgroundTaskRunner{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	manager := agenttask.NewManager(runner)
	defer manager.Close()
	executor := realtimeVoiceToolExecutor{tasks: manager, now: time.Now}

	startedAt := time.Now()
	output := executor.call(context.Background(), realtimeCreateTaskTool, `{"task":"open settings"}`)
	if time.Since(startedAt) > 100*time.Millisecond {
		t.Fatal("create_agent_task blocked on background execution")
	}
	var created agenttask.Task
	if err := json.Unmarshal([]byte(output), &created); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if created.Status != agenttask.StatusQueued || created.ID == "" {
		t.Fatalf("created task = %+v", created)
	}

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}
	running := decodeRealtimeTaskQuery(t, executor.call(context.Background(), realtimeQueryTaskTool, fmt.Sprintf(`{"task_id":%q}`, created.ID)))
	if len(running) != 1 || running[0].Status != agenttask.StatusRunning {
		t.Fatalf("query output = %+v, want the running task", running)
	}
	updatedOutput := executor.call(context.Background(), realtimeUpdateTaskTool, fmt.Sprintf(`{"task_id":%q,"task":"open display settings"}`, created.ID))
	var updated agenttask.Task
	if err := json.Unmarshal([]byte(updatedOutput), &updated); err != nil {
		t.Fatalf("decode updated task: %v", err)
	}
	if updated.Prompt != "open display settings" || updated.Status != agenttask.StatusRunning {
		t.Fatalf("updated task = %+v", updated)
	}
	// Omitting task_id must surface the same work, so the foreground can check
	// what it already started before creating more.
	outstanding := decodeRealtimeTaskQuery(t, executor.call(context.Background(), realtimeQueryTaskTool, `{}`))
	if len(outstanding) != 1 || outstanding[0].ID != created.ID {
		t.Fatalf("query-all output = %+v, want the running task", outstanding)
	}
	cancelled := executor.call(context.Background(), realtimeCancelTaskTool, fmt.Sprintf(`{"task_id":%q}`, created.ID))
	var cancelling agenttask.Task
	if err := json.Unmarshal([]byte(cancelled), &cancelling); err != nil || cancelling.Status != agenttask.StatusCancelling {
		t.Fatalf("cancel output = %s, error = %v", cancelled, err)
	}
}

func TestRealtimeQueryTaskWithoutTaskIDListsOutstandingWork(t *testing.T) {
	runner := &fakeBackgroundTaskRunner{started: make(chan string, 1), release: make(chan struct{})}
	manager := agenttask.NewManager(runner)
	defer manager.Close()
	executor := realtimeVoiceToolExecutor{tasks: manager, now: time.Now}

	// With nothing outstanding the result is an empty list, not null: the model
	// reads both modes of the tool as a list of tasks.
	if got := executor.call(context.Background(), realtimeQueryTaskTool, `{}`); got != `{"tasks":[]}` {
		t.Fatalf("query-all output = %s, want an empty task list", got)
	}
	created, err := manager.Create("open settings")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("background task did not start")
	}
	outstanding := decodeRealtimeTaskQuery(t, executor.call(context.Background(), realtimeQueryTaskTool, `{}`))
	if len(outstanding) != 1 || outstanding[0].ID != created.ID {
		t.Fatalf("query-all output = %+v, want %s", outstanding, created.ID)
	}

	// A task id that no longer exists stays a distinct error rather than an
	// empty list, so the foreground does not read it as "no work in flight".
	var missing struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(executor.call(context.Background(), realtimeQueryTaskTool, `{"task_id":"task_missing"}`)), &missing); err != nil {
		t.Fatalf("decode query output: %v", err)
	}
	if missing.Error != "agent task not found" {
		t.Fatalf("query error = %q, want agent task not found", missing.Error)
	}
}

func TestRealtimeCancelCompletedTaskSuppressesAndRedactsPendingResult(t *testing.T) {
	manager := agenttask.NewManager(staticTaskRunner{result: "stale private result"})
	defer manager.Close()
	executor := realtimeVoiceToolExecutor{tasks: manager, now: time.Now}
	task, err := manager.Create("old request")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if current, ok := manager.Query(task.ID); ok && current.Status == agenttask.StatusCompleted {
			break
		}
		time.Sleep(time.Millisecond)
	}
	claimed := manager.DrainTerminalTasks()
	if len(claimed) != 1 {
		t.Fatalf("claimed terminal tasks = %+v", claimed)
	}
	output := executor.call(context.Background(), realtimeCancelTaskTool, fmt.Sprintf(`{"task_id":%q}`, task.ID))
	if strings.Contains(output, "stale private result") {
		t.Fatalf("cancel exposed the suppressed result to the realtime model: %s", output)
	}
	var got struct {
		Status                     agenttask.Status `json:"status"`
		PendingNotificationCleared bool             `json:"pending_notification_cleared"`
		Error                      string           `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode cancel result: %v", err)
	}
	if got.Status != agenttask.StatusCompleted || !got.PendingNotificationCleared || got.Error != "" {
		t.Fatalf("cancel result = %+v", got)
	}
	if deliverable := manager.BeginTaskUpdateDelivery(claimed); len(deliverable) != 0 {
		t.Fatalf("suppressed result remained deliverable: %+v", deliverable)
	}
}

func TestRealtimeTaskCreationRequiresOutstandingWorkCheck(t *testing.T) {
	const rule = "query_agent_task with no task_id"
	if !strings.Contains(agent.DefaultRealtimeVoiceInstructions, rule) {
		t.Fatalf("realtime instructions missing the outstanding-work check: %s", agent.DefaultRealtimeVoiceInstructions)
	}
	definitions := make(map[string]realtimevoice.Tool)
	for _, definition := range realtimeVoiceToolDefinitions(agent.Config{VoiceModel: agent.VoiceModelConfig{UseBackendAgent: true}}, nil) {
		definitions[definition.Name] = definition
	}
	create, ok := definitions[realtimeCreateTaskTool]
	if !ok || !strings.Contains(create.Description, rule) {
		t.Fatalf("%s must require the check before creating work: %s", realtimeCreateTaskTool, create.Description)
	}
	// The rule is only followable while query_agent_task accepts an omitted
	// task_id, so the schema and the instructions have to move together.
	query, ok := definitions[realtimeQueryTaskTool]
	if !ok {
		t.Fatalf("missing %s definition", realtimeQueryTaskTool)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(query.Parameters, &schema); err != nil {
		t.Fatalf("decode %s schema: %v", realtimeQueryTaskTool, err)
	}
	if _, ok := schema.Properties["task_id"]; !ok {
		t.Fatalf("%s schema has no task_id property: %s", realtimeQueryTaskTool, query.Parameters)
	}
	for _, name := range schema.Required {
		if name == "task_id" {
			t.Fatalf("%s still requires task_id, so the outstanding-work check cannot run: %s", realtimeQueryTaskTool, query.Parameters)
		}
	}
}

func TestRealtimeUpdateTaskRequiresCompleteGoal(t *testing.T) {
	definitions := make(map[string]realtimevoice.Tool)
	for _, definition := range realtimeVoiceToolDefinitions(agent.Config{VoiceModel: agent.VoiceModelConfig{UseBackendAgent: true}}, nil) {
		definitions[definition.Name] = definition
	}
	update, ok := definitions[realtimeUpdateTaskTool]
	if !ok {
		t.Fatalf("missing %s definition", realtimeUpdateTaskTool)
	}
	if !strings.Contains(update.Description, "complete goal") || !strings.Contains(update.Description, "instead of cancelling") {
		t.Fatalf("update description does not explain replacement semantics: %s", update.Description)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(update.Parameters, &schema); err != nil {
		t.Fatalf("decode %s schema: %v", realtimeUpdateTaskTool, err)
	}
	for _, required := range []string{"task_id", "task"} {
		if _, ok := schema.Properties[required]; !ok || !slices.Contains(schema.Required, required) {
			t.Fatalf("%s schema must require %q: %s", realtimeUpdateTaskTool, required, update.Parameters)
		}
	}
}

func decodeRealtimeTaskQuery(t *testing.T, output string) []agenttask.Task {
	t.Helper()
	var got struct {
		Tasks []agenttask.Task `json:"tasks"`
		Error string           `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatalf("decode query output %s: %v", output, err)
	}
	if got.Error != "" {
		t.Fatalf("query output carries an error: %s", output)
	}
	return got.Tasks
}

func TestFormatRealtimeTaskUpdatesAggregatesResults(t *testing.T) {
	message := formatRealtimeTaskUpdates([]agenttask.Task{
		{ID: "task_1", Prompt: "first", Status: agenttask.StatusCompleted, Result: "done"},
		{ID: "task_2", Prompt: "second", Status: agenttask.StatusFailed, Error: "boom"},
	})
	for _, value := range []string{"task_1", "task_2", "done", "boom"} {
		if !strings.Contains(message, value) {
			t.Fatalf("aggregated message missing %q: %s", value, message)
		}
	}
}

func TestRealtimeToolCallAndResultPersistInUserContext(t *testing.T) {
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	call := realtimevoice.Event{Kind: realtimevoice.EventToolCall, CallID: "call_1", Name: realtimeCreateTaskTool, Arguments: `{"task":"打开微信"}`}
	if err := appendRealtimeToolExecution(manager, call, `{"id":"task_1","status":"queued"}`); err != nil {
		t.Fatal(err)
	}
	got := manager.MessageListDump().Messages
	if len(got) != 3 || got[1].Role != messages.MessageRoleToolCall || got[2].Role != messages.MessageRoleToolResult {
		t.Fatalf("messages = %+v", got)
	}
	if got[1].ToolCalls[0].Name != realtimeCreateTaskTool || got[2].ToolResults[0].ToolCallID != "call_1" {
		t.Fatalf("tool messages = %+v", got[1:])
	}
}

func TestRealtimeNoticePersistsAsNoticeInUserContext(t *testing.T) {
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendRealtimeNoticeMessage(manager, "background task completed"); err != nil {
		t.Fatal(err)
	}
	got := manager.MessageListDump().Messages
	if len(got) != 2 || got[1].Role != messages.MessageRoleNotice || got[1].Content != "background task completed" {
		t.Fatalf("messages = %+v, want notice role", got)
	}
}

func TestRecentRealtimeContextMessagesKeepsLatestTenUserTurns(t *testing.T) {
	var all []messages.Message
	for i := 1; i <= 12; i++ {
		all = append(all,
			messages.Message{Role: messages.MessageRoleUser, Content: fmt.Sprintf("user-%d", i)},
			messages.Message{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: fmt.Sprintf("call-%d", i)}}},
			messages.Message{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: fmt.Sprintf("call-%d", i)}}},
			messages.Message{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("assistant-%d", i)},
		)
	}
	got := recentRealtimeContextMessages(all, 10)
	if len(got) != 40 || got[0].Content != "user-3" || got[len(got)-1].Content != "assistant-12" {
		t.Fatalf("recent messages = %+v", got)
	}
	for i := 0; i < len(got); i++ {
		if got[i].Role != messages.MessageRoleToolCall {
			continue
		}
		if i+1 >= len(got) || got[i+1].Role != messages.MessageRoleToolResult ||
			got[i].ToolCalls[0].ID != got[i+1].ToolResults[0].ToolCallID {
			t.Fatalf("tool exchange was split: %+v", got[i:])
		}
	}
}

func TestRotateRealtimeContextForReplayStartsRevisionWhenTruncated(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := contextmanager.NewContextManager(sessionFolder, "system")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := manager.AppendMessages([]messages.Message{
			{Role: messages.MessageRoleUser, Content: fmt.Sprintf("user-%d", i)},
			{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("assistant-%d", i)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	originalSessionID := manager.GetSessionID()

	rotated, err := rotateRealtimeContextForReplay(manager, 2)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.GetSessionID() == originalSessionID {
		t.Fatal("truncated context reused the original session")
	}
	if rotated != manager {
		t.Fatal("context manager pointer changed during replay rotation")
	}
	dump := rotated.MessageListDump()
	if dump.ParentSessionID != originalSessionID {
		t.Fatalf("parent session = %q, want %q", dump.ParentSessionID, originalSessionID)
	}
	if len(dump.Messages) != 5 ||
		dump.Messages[0].Role != messages.MessageRoleSystem ||
		dump.Messages[1].Content != "user-2" ||
		dump.Messages[4].Content != "assistant-3" {
		t.Fatalf("rotated messages = %+v", dump.Messages)
	}
	current, err := contextmanager.LoadContextManagerFromCurrentSession(sessionFolder)
	if err != nil {
		t.Fatal(err)
	}
	if current.GetSessionID() != rotated.GetSessionID() {
		t.Fatalf("current session = %q, want %q", current.GetSessionID(), rotated.GetSessionID())
	}
}

func TestRotateRealtimeContextForReplayReusesSessionWithoutTruncation(t *testing.T) {
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	got, err := rotateRealtimeContextForReplay(manager, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetSessionID() != manager.GetSessionID() {
		t.Fatal("untruncated context started a new session")
	}
}

type fakeBackgroundTaskRunner struct {
	started chan string
	release chan struct{}
}

func (r *fakeBackgroundTaskRunner) Run(ctx context.Context, prompt string) (string, error) {
	r.started <- prompt
	select {
	case <-r.release:
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type fakeRealtimeTool struct {
	name   string
	input  string
	output string
}

func (t *fakeRealtimeTool) Name() string {
	if t.name != "" {
		return t.name
	}
	return realtimeRecallTool
}
func (t *fakeRealtimeTool) Description() string { return "fake recall" }
func (t *fakeRealtimeTool) Call(_ context.Context, input string) (string, error) {
	t.input = input
	return t.output, nil
}

func floatPtr(value float64) *float64 { return &value }

func TestApplyDeviceTypeOverrideTakesPrecedenceOverConfig(t *testing.T) {
	cfg := agent.Config{
		Device: agent.DeviceConfig{DeviceType: "iOS"},
		HID:    agent.HIDConfig{PointerMode: "absolute"},
	}
	if err := applyDeviceTypeOverride(&cfg, "android"); err != nil {
		t.Fatalf("applyDeviceTypeOverride() error = %v", err)
	}
	if cfg.Device.DeviceType != "Android" || cfg.HID.PointerMode != "touchscreen" {
		t.Fatalf("config = %+v, want Android command-line override", cfg)
	}
}

type fakeAudioDialog struct {
	chunks               []*agent.AudioChunkResult
	chunkBatches         [][]*agent.AudioChunkResult
	frameSamples         int
	frames               [][]int16
	vad                  *agent.AudioVAD
	utterance            []int16
	utterancesToReturn   [][]int16
	onStart              func(*fakeAudioDialog)
	onUtterance          func()
	processUtterance     func(context.Context, []int16, *agent.Runtime) error
	utterances           [][]int16
	prepareTurnInput     func([]int16) (agent.TurnInput, error)
	runTurn              func(context.Context) (agent.RunResult, error)
	runVoiceTurn         func(context.Context, agent.TurnInput, []int16) (agent.RunResult, error)
	beginSteerInterrupt  func() bool
	resumeSteerInterrupt func() bool
	waitForVoiceRunIdle  func(context.Context) bool
	queueSteer           func(agent.TurnInput) bool
	interruptOutput      func()
	speak                func(context.Context) error
	persistVoiceTurn     func(agent.TurnInput, agent.RunResult, []int16)
	persistedTurns       []persistedVoiceTurn
	queuedSteers         []agent.TurnInput
	steerInterrupts      int
	steerResumes         int
	vadErr               error
	onRead               func(*fakeAudioDialog, *agent.AudioChunkResult)
	spoken               []string
	speechUnavailable    bool
	voiceTurnContexts    []agent.VoiceTurnContext
	repeatEmpty          bool
	readDelay            time.Duration
	ops                  []string
	starts               int
	stops                int
	resets               int
	recordingActive      bool
}

func taggedReply(text string) string {
	return text + "\n<tts>" + text + "</tts>"
}

type persistedVoiceTurn struct {
	input     agent.TurnInput
	result    agent.RunResult
	utterance []int16
}

func (d *fakeAudioDialog) StartRecording() error {
	if d.recordingActive {
		return nil
	}
	d.ops = append(d.ops, "start")
	d.starts++
	d.recordingActive = true
	if d.onStart != nil {
		d.onStart(d)
	}
	if len(d.chunkBatches) > 0 {
		d.chunks = d.chunkBatches[0]
		d.chunkBatches = d.chunkBatches[1:]
	}
	return nil
}

func (d *fakeAudioDialog) FinalizeAndRotateSTT() error {
	return nil
}

func (d *fakeAudioDialog) StopRecording() error {
	d.ops = append(d.ops, "stop")
	d.stops++
	d.recordingActive = false
	return nil
}

func (d *fakeAudioDialog) RecordingActive() bool {
	return d.recordingActive
}

func (d *fakeAudioDialog) ReadRecordChunkMonitor(timeoutMs uint32) (*agent.AudioChunkResult, error) {
	return d.ReadRecordChunk(timeoutMs)
}

func (d *fakeAudioDialog) ReadRecordChunk(timeoutMs uint32) (*agent.AudioChunkResult, error) {
	if d.readDelay > 0 {
		time.Sleep(d.readDelay)
	}
	if len(d.chunks) == 0 {
		if d.repeatEmpty {
			return &agent.AudioChunkResult{}, nil
		}
		return &agent.AudioChunkResult{EndOfStream: true}, nil
	}
	chunk := d.chunks[0]
	d.chunks = d.chunks[1:]
	if d.onRead != nil {
		d.onRead(d, chunk)
	}
	return chunk, nil
}

func (d *fakeAudioDialog) ProcessVADFrame(samples []int16) ([]int16, error) {
	copied := append([]int16(nil), samples...)
	d.frames = append(d.frames, copied)
	if d.vadErr != nil {
		return nil, d.vadErr
	}
	if d.vad != nil {
		return d.vad.Process(samples)
	}
	if len(d.utterancesToReturn) > 0 {
		utterance := d.utterancesToReturn[0]
		d.utterancesToReturn = d.utterancesToReturn[1:]
		return utterance, nil
	}
	if d.utterance != nil {
		utterance := d.utterance
		d.utterance = nil
		return utterance, nil
	}
	return nil, nil
}

func (d *fakeAudioDialog) ProcessUtterance(ctx context.Context, utterance []int16, runtime *agent.Runtime) error {
	d.utterances = append(d.utterances, append([]int16(nil), utterance...))
	if d.onUtterance != nil {
		d.onUtterance()
	}
	if d.processUtterance != nil {
		return d.processUtterance(ctx, utterance, runtime)
	}
	return nil
}

func (d *fakeAudioDialog) PrepareTurnInput(utterance []int16) (agent.TurnInput, error) {
	d.utterances = append(d.utterances, append([]int16(nil), utterance...))
	if d.onUtterance != nil {
		d.onUtterance()
	}
	if d.prepareTurnInput != nil {
		return d.prepareTurnInput(utterance)
	}
	return agent.TurnInput{InputText: "voice"}, nil
}

func (d *fakeAudioDialog) RunAgentTurn(ctx context.Context, input agent.TurnInput, runtime *agent.Runtime) (agent.RunResult, error) {
	if d.runTurn != nil {
		return d.runTurn(ctx)
	}
	return agent.RunResult{Output: taggedReply("reply")}, nil
}

func (d *fakeAudioDialog) RunVoiceTurn(ctx context.Context, input agent.TurnInput, utterance []int16, runtime *agent.Runtime) (agent.RunResult, error) {
	return d.RunVoiceTurnWithContext(ctx, input, utterance, runtime, agent.VoiceTurnContext{})
}

func (d *fakeAudioDialog) RunVoiceTurnWithContext(ctx context.Context, input agent.TurnInput, utterance []int16, runtime *agent.Runtime, turnContext agent.VoiceTurnContext) (agent.RunResult, error) {
	d.voiceTurnContexts = append(d.voiceTurnContexts, turnContext)
	if d.runVoiceTurn != nil {
		return d.runVoiceTurn(ctx, input, utterance)
	}
	if d.persistVoiceTurn != nil {
		d.persistVoiceTurn(input, agent.RunResult{}, utterance)
	}
	result, err := d.RunAgentTurn(ctx, input, runtime)
	if err != nil {
		return result, err
	}
	d.persistedTurns = append(d.persistedTurns, persistedVoiceTurn{
		input:     input,
		result:    result,
		utterance: append([]int16(nil), utterance...),
	})
	if d.persistVoiceTurn != nil {
		d.persistVoiceTurn(agent.TurnInput{}, result, nil)
	}
	return result, nil
}

func (d *fakeAudioDialog) QueueSteer(input agent.TurnInput) bool {
	ok := true
	if d.queueSteer != nil {
		ok = d.queueSteer(input)
	}
	if ok {
		d.queuedSteers = append(d.queuedSteers, input)
	}
	return ok
}

func (d *fakeAudioDialog) BeginSteerInterrupt() bool {
	d.ops = append(d.ops, "begin_steer_interrupt")
	d.steerInterrupts++
	if d.beginSteerInterrupt != nil {
		return d.beginSteerInterrupt()
	}
	return true
}

func (d *fakeAudioDialog) ResumeSteerInterrupt() bool {
	d.ops = append(d.ops, "resume_steer_interrupt")
	d.steerResumes++
	if d.resumeSteerInterrupt != nil {
		return d.resumeSteerInterrupt()
	}
	return true
}

func (d *fakeAudioDialog) WaitForVoiceRunIdle(ctx context.Context) bool {
	if d.waitForVoiceRunIdle != nil {
		return d.waitForVoiceRunIdle(ctx)
	}
	return true
}

func (d *fakeAudioDialog) ForceResetVoiceRun() {
	d.ops = append(d.ops, "force_reset_voice_run")
}

func (d *fakeAudioDialog) InterruptOutput() {
	d.ops = append(d.ops, "interrupt_output")
	if d.interruptOutput != nil {
		d.interruptOutput()
	}
}

func (d *fakeAudioDialog) CanSpeakFinalText() bool {
	return !d.speechUnavailable
}

func (d *fakeAudioDialog) PersistVoiceTurn(input agent.TurnInput, result agent.RunResult, utterance []int16) {
	d.persistedTurns = append(d.persistedTurns, persistedVoiceTurn{
		input:     input,
		result:    result,
		utterance: append([]int16(nil), utterance...),
	})
	if d.persistVoiceTurn != nil {
		d.persistVoiceTurn(input, result, utterance)
	}
}

func (d *fakeAudioDialog) Speak(ctx context.Context, text string, interrupt <-chan struct{}) error {
	d.ops = append(d.ops, "speak:"+text)
	d.spoken = append(d.spoken, text)
	if d.speak != nil {
		return d.speak(ctx)
	}
	return nil
}

func (d *fakeAudioDialog) SpeakFinal(ctx context.Context, text string, interrupt <-chan struct{}) error {
	return d.Speak(ctx, text, interrupt)
}

func (d *fakeAudioDialog) TTSOutputActive() bool {
	return false
}

func (d *fakeAudioDialog) PlayListenReadyCue() {
	d.addOp("listen_ready_cue")
}

func (d *fakeAudioDialog) FlushVAD() []int16 {
	if d.vad != nil {
		return d.vad.Flush()
	}
	return nil
}

func (d *fakeAudioDialog) FinishPendingUtterance(pending []int16) []int16 {
	frameSamples := d.VADFrameSamples()
	consumed := 0
	for consumed+frameSamples <= len(pending) {
		if _, err := d.ProcessVADFrame(pending[consumed : consumed+frameSamples]); err != nil {
			break
		}
		consumed += frameSamples
	}
	tail := pending[consumed:]
	utterance := d.FlushVAD()
	if len(tail) == 0 {
		return utterance
	}
	if len(utterance) == 0 {
		return append([]int16(nil), tail...)
	}
	return append(utterance, tail...)
}

func (d *fakeAudioDialog) ResetVAD() {
	d.ops = append(d.ops, "reset")
	d.resets++
	if d.vad != nil {
		_ = d.vad.Reset()
	}
}

func (d *fakeAudioDialog) VADSpeaking() bool {
	return false
}

func (d *fakeAudioDialog) VADFrameSamples() int {
	if d.vad != nil {
		return d.vad.FrameSamples()
	}
	return d.frameSamples
}

func (d *fakeAudioDialog) VADDebugState() agent.VADDebugState {
	if d.vad != nil {
		return d.vad.DebugState()
	}
	return agent.VADDebugState{}
}

func opsHasPrefix(ops, prefix []string) bool {
	if len(ops) < len(prefix) {
		return false
	}
	for i, want := range prefix {
		if ops[i] != want {
			return false
		}
	}
	return true
}

type fakeWakeupWatcher struct {
	callback     func()
	fireOnStart  bool
	firedOnStart bool
	started      bool
	stopped      bool
	startCount   int
	stopCount    int
}

func withWakeupDebounceClock(t *testing.T, start time.Time) func(time.Duration) {
	t.Helper()
	var mu sync.Mutex
	current := start
	originalNow := wakeupDebounceNow
	wakeupDebounceNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	t.Cleanup(func() {
		wakeupDebounceNow = originalNow
	})
	return func(delta time.Duration) {
		mu.Lock()
		current = current.Add(delta)
		mu.Unlock()
	}
}

func newTestAudioVAD(t *testing.T, probabilities []float64) *agent.AudioVAD {
	t.Helper()
	vad, err := agent.NewAudioVADWithScorer(agent.AudioVADConfig{
		SampleRate:      16000,
		SilenceMs:       90,
		MinSpeechMs:     60,
		SpeechThreshold: 0.5,
	}, &testVADScorer{probabilities: probabilities})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}
	return vad
}

type testVADScorer struct {
	probabilities []float64
	index         int
}

func (s *testVADScorer) Score(samples []int16) (float64, error) {
	if len(s.probabilities) == 0 {
		return 0, nil
	}
	if s.index >= len(s.probabilities) {
		return s.probabilities[len(s.probabilities)-1], nil
	}
	probability := s.probabilities[s.index]
	s.index++
	return probability, nil
}

func (s *testVADScorer) Reset() error {
	s.index = 0
	return nil
}

func (s *testVADScorer) Close() error {
	return nil
}

func (w *fakeWakeupWatcher) Start() error {
	w.started = true
	w.startCount++
	if w.fireOnStart && !w.firedOnStart {
		w.firedOnStart = true
		w.callback()
	}
	return nil
}

func (w *fakeWakeupWatcher) Stop() {
	w.stopped = true
	w.stopCount++
}

func TestStartWakeupWatchersIfEnabledSkipsWhenDisabled(t *testing.T) {
	disabled := false
	factoryCalls := 0
	watchers, err := startWakeupWatchersIfEnabled(agent.Config{TapWakeupEnabled: &disabled}, func(pin int, callback func()) (wakeupWatcher, error) {
		factoryCalls++
		return &fakeWakeupWatcher{callback: callback}, nil
	}, func() {})
	if err != nil {
		t.Fatalf("startWakeupWatchersIfEnabled() error = %v", err)
	}
	if watchers != nil {
		t.Fatalf("watchers = %#v, want nil when tap wakeup disabled", watchers)
	}
	if factoryCalls != 0 {
		t.Fatalf("factoryCalls = %d, want 0", factoryCalls)
	}
}

func TestStartWakeupWatchersIfEnabledRegistersWhenEnabled(t *testing.T) {
	enabled := true
	watchersByPin := map[int]*fakeWakeupWatcher{}
	watchers, err := startWakeupWatchersIfEnabled(agent.Config{TapWakeupEnabled: &enabled}, func(pin int, callback func()) (wakeupWatcher, error) {
		watcher := &fakeWakeupWatcher{callback: callback}
		watchersByPin[pin] = watcher
		return watcher, nil
	}, func() {})
	if err != nil {
		t.Fatalf("startWakeupWatchersIfEnabled() error = %v", err)
	}
	defer stopWakeupWatchers(watchers)
	if len(watchers) != 2 || watchersByPin[32] == nil || watchersByPin[33] == nil {
		t.Fatalf("watchers pins = %#v, want GPIO 32 and 33", watchersByPin)
	}
}

func TestStartWakeupWatchersRegistersGPIO32And33Callbacks(t *testing.T) {
	advanceWakeup := withWakeupDebounceClock(t, time.Unix(0, 0))
	watchersByPin := map[int]*fakeWakeupWatcher{}
	eventCount := 0

	watchers, err := startWakeupWatchers(func(pin int, callback func()) (wakeupWatcher, error) {
		watcher := &fakeWakeupWatcher{callback: callback}
		watchersByPin[pin] = watcher
		return watcher, nil
	}, func() {
		eventCount++
	})
	if err != nil {
		t.Fatalf("startWakeupWatchers() error = %v", err)
	}
	defer stopWakeupWatchers(watchers)

	for _, pin := range []int{33, 32} {
		watcher := watchersByPin[pin]
		if watcher == nil {
			t.Fatalf("missing watcher for GPIO %d", pin)
		}
		if !watcher.started {
			t.Fatalf("GPIO %d watcher was not started", pin)
		}
		watcher.callback()
		advanceWakeup(wakeupDebounceInterval + time.Millisecond)
	}

	if eventCount != 2 {
		t.Fatalf("event count = %d, want one wakeup event per GPIO callback", eventCount)
	}
}

func TestStartWakeupWatchersDebouncesImmediateGPIOBurst(t *testing.T) {
	withWakeupDebounceClock(t, time.Unix(0, 0))
	watchersByPin := map[int]*fakeWakeupWatcher{}
	eventCount := 0

	watchers, err := startWakeupWatchers(func(pin int, callback func()) (wakeupWatcher, error) {
		watcher := &fakeWakeupWatcher{callback: callback}
		watchersByPin[pin] = watcher
		return watcher, nil
	}, func() {
		eventCount++
	})
	if err != nil {
		t.Fatalf("startWakeupWatchers() error = %v", err)
	}
	defer stopWakeupWatchers(watchers)

	watchersByPin[33].callback()
	watchersByPin[32].callback()
	watchersByPin[33].callback()

	if eventCount != 1 {
		t.Fatalf("event count = %d, want one wakeup event after immediate GPIO burst", eventCount)
	}
}

type fakeQuickCaptureTrigger struct {
	calls int
	err   error
}

func (t *fakeQuickCaptureTrigger) TriggerQuickCapture() error {
	t.calls++
	return t.err
}

func TestStartQuickCaptureGPIOWatcherUsesConfiguredSparePin(t *testing.T) {
	trigger := &fakeQuickCaptureTrigger{}
	var configuredPin int
	var watcher *fakeWakeupWatcher

	got, err := startQuickCaptureGPIOWatcher(agent.Config{
		QuickCapture: agent.QuickCaptureConfig{GPIOPin: 3},
	}, trigger, func(pin int, callback func()) (wakeupWatcher, error) {
		configuredPin = pin
		watcher = &fakeWakeupWatcher{callback: callback}
		return watcher, nil
	})
	if err != nil {
		t.Fatalf("startQuickCaptureGPIOWatcher() error = %v", err)
	}
	defer got.Stop()

	if configuredPin != 3 || watcher == nil || !watcher.started {
		t.Fatalf("watcher pin=%d watcher=%+v, want started GPIO 3", configuredPin, watcher)
	}
	watcher.callback()
	if trigger.calls != 1 {
		t.Fatalf("trigger calls = %d, want 1", trigger.calls)
	}
	if got == nil {
		t.Fatal("watcher = nil, want configured watcher")
	}
}

func TestStartQuickCaptureGPIOWatcherDisabledByDefault(t *testing.T) {
	called := false
	watcher, err := startQuickCaptureGPIOWatcher(agent.Config{}, &fakeQuickCaptureTrigger{}, func(pin int, callback func()) (wakeupWatcher, error) {
		called = true
		return nil, nil
	})
	if err != nil {
		t.Fatalf("startQuickCaptureGPIOWatcher() error = %v", err)
	}
	if watcher != nil || called {
		t.Fatalf("watcher=%v factory_called=%v, want disabled", watcher, called)
	}
}

func TestProcessAudioUntilUtteranceUsesConfiguredFrameSizeAndCarriesOddPCMByte(t *testing.T) {
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16Bytes(100)[:1]},
			{PCM: pcm16BytesFromSamples(100, 200)[1:]},
		},
		utterance: []int16{100, 200},
	}

	if exit := processAudioUntilUtterance(dialog, nil, context.Background(), make(chan os.Signal, 1)); exit {
		t.Fatal("processAudioUntilUtterance() returned exit=true")
	}
	if len(dialog.frames) != 1 {
		t.Fatalf("expected one VAD frame, got %d", len(dialog.frames))
	}
	if got := dialog.frames[0]; len(got) != 2 || got[0] != 100 || got[1] != 200 {
		t.Fatalf("unexpected VAD frame: %#v", got)
	}
	if dialog.stops != 1 {
		t.Fatalf("dialog stops = %d, want 1 before utterance processing", dialog.stops)
	}
}

func TestProcessAudioUntilUtteranceStopsAfterListenTimeout(t *testing.T) {
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		repeatEmpty:  true,
	}

	if exit := processAudioUntilUtteranceWithTimeout(dialog, nil, context.Background(), make(chan os.Signal, 1), time.Millisecond); exit {
		t.Fatal("processAudioUntilUtteranceWithTimeout() returned exit=true")
	}
	if len(dialog.frames) != 0 {
		t.Fatalf("expected no VAD frames for empty chunks, got %d", len(dialog.frames))
	}
}

func TestProcessAudioUntilUtteranceSendsBufferedAudioAfterVADTimeout(t *testing.T) {
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(100, 200, 300, 400)},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
	}

	if exit := processAudioUntilUtteranceWithTimeout(dialog, nil, context.Background(), make(chan os.Signal, 1), time.Millisecond); exit {
		t.Fatal("processAudioUntilUtteranceWithTimeout() returned exit=true")
	}
	if len(dialog.utterances) != 1 {
		t.Fatalf("utterances = %d, want 1", len(dialog.utterances))
	}
	if got := dialog.utterances[0]; len(got) != 4 || got[0] != 100 || got[1] != 200 || got[2] != 300 || got[3] != 400 {
		t.Fatalf("unexpected fallback utterance: %#v", got)
	}
	if dialog.stops != 1 {
		t.Fatalf("dialog stops = %d, want 1 before timeout utterance processing", dialog.stops)
	}
}

func TestDrainWakeupEventsWhileListeningLogsDrainedCount(t *testing.T) {
	wakeupEvents := make(chan struct{}, 3)
	wakeupEvents <- struct{}{}
	wakeupEvents <- struct{}{}
	wakeupEvents <- struct{}{}

	var logBuf bytes.Buffer
	restore := logging.SetOutput(&logBuf)
	t.Cleanup(restore)

	drainWakeupEventsWhileListening(wakeupEvents)

	if got := logBuf.String(); !strings.Contains(got, "3 duplicate wakeup(s) received while listening, ignoring") {
		t.Fatalf("drain log = %q, want drained count", got)
	}
}

func TestFormatVADDebugLogOmitsEmptyLastError(t *testing.T) {
	got := formatVADDebugLog(agent.VADDebugState{
		Probability:   0.1234,
		Threshold:     0.5,
		Speaking:      false,
		SpeechFrames:  0,
		SilenceFrames: 0,
	})

	if strings.Contains(got, "last_error=") {
		t.Fatalf("log = %q, want last_error omitted when empty", got)
	}
	want := "[vad] probability=0.123 threshold=0.500 speaking=false speech_frames=0 silence_frames=0"
	if got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestFormatVADDebugLogIncludesLastErrorWhenPresent(t *testing.T) {
	got := formatVADDebugLog(agent.VADDebugState{
		Probability:   0.5,
		Threshold:     0.5,
		Speaking:      true,
		SpeechFrames:  3,
		SilenceFrames: 1,
		LastError:     "model failed",
	})

	want := `[vad] probability=0.500 threshold=0.500 speaking=true speech_frames=3 silence_frames=1 last_error="model failed"`
	if got != want {
		t.Fatalf("log = %q, want %q", got, want)
	}
}

func TestSignalWakeupEventCoalescesPendingEvents(t *testing.T) {
	wakeupEvents := make(chan struct{}, 1)

	signalWakeupEvent(wakeupEvents)
	signalWakeupEvent(wakeupEvents)
	signalWakeupEvent(wakeupEvents)

	if got := len(wakeupEvents); got != 1 {
		t.Fatalf("pending wakeup events = %d, want 1", got)
	}
}

func TestSignalVoiceWakeupEventCoalescesPendingEvents(t *testing.T) {
	events := make(chan voiceEvent, 1)

	signalVoiceWakeupEvent(events)
	signalVoiceWakeupEvent(events)
	signalVoiceWakeupEvent(events)

	if got := len(events); got != 1 {
		t.Fatalf("pending voice events = %d, want 1", got)
	}
	if got := <-events; got != voiceEventWakeup {
		t.Fatalf("voice event = %v, want wakeup", got)
	}
}

func TestRunWakeupModeProcessesTriggeredAudioAndStopsOnSignal(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(300, 400)},
		},
		utterance: []int16{300, 400},
		onUtterance: func() {
			sigChan <- syscall.SIGTERM
		},
	}
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	var watcherPins []int

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcherPins = append(watcherPins, pin)
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after signal")
	}

	if !watcher.started || !watcher.stopped {
		t.Fatalf("watcher lifecycle started=%v stopped=%v", watcher.started, watcher.stopped)
	}
	if len(watcherPins) != 2 || watcherPins[0] != 33 || watcherPins[1] != 32 {
		t.Fatalf("watcher pins = %#v, want [33 32]", watcherPins)
	}
	if dialog.starts != 1 || dialog.stops == 0 || dialog.resets != 1 {
		t.Fatalf("dialog lifecycle starts=%d stops=%d resets=%d", dialog.starts, dialog.stops, dialog.resets)
	}
	if len(dialog.frames) != 1 || dialog.frames[0][0] != 300 || dialog.frames[0][1] != 400 {
		t.Fatalf("unexpected VAD frames: %#v", dialog.frames)
	}
}

func TestRunWakeupModeLegacyStartsRecordingWithoutWakeupAck(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(300, 400)},
		},
		utterance: []int16{300, 400},
		onUtterance: func() {
			sigChan <- syscall.SIGTERM
		},
	}
	watcher := &fakeWakeupWatcher{fireOnStart: true}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after signal")
	}

	if len(dialog.spoken) != 0 {
		t.Fatalf("spoken texts = %#v, want no wakeup acknowledgement", dialog.spoken)
	}
	wantPrefix := []string{"interrupt_output", "start", "reset"}
	if !opsHasPrefix(dialog.ops, wantPrefix) {
		t.Fatalf("dialog ops = %#v, want prefix %#v without wakeup acknowledgement", dialog.ops, wantPrefix)
	}
}

func TestRunWakeupModeStopsRecordingAfterSpeechThenBiasedSilence(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	vad := newTestAudioVAD(t, []float64{0.9, 0.9, 0.1, 0.1, 0.1})
	samples := append([]int16{}, alternatingSamples(vad.FrameSamples()*2, 700, 1000)...)
	samples = append(samples, constantSamples(vad.FrameSamples()*3, 700)...)

	dialog := &fakeAudioDialog{
		vad: vad,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(samples...)},
		},
		onUtterance: func() {
			sigChan <- syscall.SIGTERM
		},
	}
	watcher := &fakeWakeupWatcher{fireOnStart: true}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after VAD utterance")
	}

	if dialog.starts != 1 {
		t.Fatalf("dialog starts = %d, want 1", dialog.starts)
	}
	if dialog.stops == 0 {
		t.Fatal("expected wakeup mode to stop recording after VAD utterance")
	}
	if len(dialog.utterances) != 1 {
		t.Fatalf("utterances = %d, want 1", len(dialog.utterances))
	}
	if len(dialog.frames) != 5 {
		t.Fatalf("VAD frames = %d, want 5", len(dialog.frames))
	}
	if got, want := len(dialog.utterances[0]), len(samples); got != want {
		t.Fatalf("utterance samples = %d, want %d", got, want)
	}
	if !watcher.started || !watcher.stopped {
		t.Fatalf("watcher lifecycle started=%v stopped=%v", watcher.started, watcher.stopped)
	}
}

func TestRunWakeupModeBuffersSpeechDetectedByRKNNAfterInitialNonSpeech(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	vad := newTestAudioVAD(t, append(append(make([]float64, 10), 0.9, 0.9), 0.1, 0.1, 0.1))
	noise := alternatingSamples(vad.FrameSamples()*10, 700, 35)
	speech := alternatingSamples(vad.FrameSamples()*2, 700, 160)
	silence := constantSamples(vad.FrameSamples()*3, 700)
	samples := append([]int16{}, noise...)
	samples = append(samples, speech...)
	samples = append(samples, silence...)

	dialog := &fakeAudioDialog{
		vad: vad,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(samples...)},
		},
		onUtterance: func() {
			sigChan <- syscall.SIGTERM
		},
	}
	watcher := &fakeWakeupWatcher{fireOnStart: true}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after RKNN-detected utterance")
	}

	if dialog.starts != 1 || dialog.stops == 0 {
		t.Fatalf("dialog lifecycle starts=%d stops=%d", dialog.starts, dialog.stops)
	}
	if len(dialog.utterances) != 1 {
		t.Fatalf("utterances = %d, want 1", len(dialog.utterances))
	}
	if got, want := len(dialog.utterances[0]), len(speech)+len(silence); got != want {
		t.Fatalf("utterance samples = %d, want %d", got, want)
	}
	if !watcher.started || !watcher.stopped {
		t.Fatalf("watcher lifecycle started=%v stopped=%v", watcher.started, watcher.stopped)
	}
}

func TestRunWakeupModeStartsNewRecordingForNextWakeup(t *testing.T) {
	advanceWakeup := withWakeupDebounceClock(t, time.Unix(0, 0))
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	voiceSessionDisabled := false
	var dialog *fakeAudioDialog
	dialog = &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{{PCM: pcm16BytesFromSamples(300, 400)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
			{300, 400},
		},
		onUtterance: func() {
			if len(dialog.utterances) == 2 {
				sigChan <- syscall.SIGTERM
			} else if watcher.callback != nil {
				go func() {
					time.Sleep(10 * time.Millisecond)
					advanceWakeup(wakeupDebounceInterval + time.Millisecond)
					watcher.callback()
				}()
			}
		},
	}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{VoiceFollowupEnabled: &voiceSessionDisabled}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not process two wakeups")
	}

	if dialog.starts != 2 {
		t.Fatalf("dialog starts = %d, want 2", dialog.starts)
	}
	if dialog.stops < 2 {
		t.Fatalf("dialog stops = %d, want at least 2", dialog.stops)
	}
	if len(dialog.utterances) != 2 {
		t.Fatalf("utterances = %d, want 2", len(dialog.utterances))
	}
}

func TestRunWakeupModeSTTWakeupKeepsLegacySingleTurnBehavior(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(100, 200)},
		},
		utterance: []int16{100, 200},
		onUtterance: func() {
			go func() {
				time.Sleep(10 * time.Millisecond)
				sigChan <- syscall.SIGTERM
			}()
		},
	}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{InputMode: "stt", STT: agent.STTConfig{Provider: "openai-whisper"}}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after signal")
	}

	if dialog.starts != 1 {
		t.Fatalf("dialog starts = %d, want 1 without STT voice session", dialog.starts)
	}
	if len(dialog.utterances) != 1 {
		t.Fatalf("utterances = %d, want 1", len(dialog.utterances))
	}
}

func TestRunWakeupModeVoiceSessionProcessesFollowupWithoutSecondWakeup(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	var dialog *fakeAudioDialog
	dialog = &fakeAudioDialog{
		frameSamples: 2,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(100, 200)},
			{PCM: pcm16BytesFromSamples(300, 400)},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
			{300, 400},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
		speak: func(ctx context.Context) error {
			if len(dialog.spoken) == 2 {
				sigChan <- syscall.SIGTERM
			}
			return nil
		},
	}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{InputMode: "stt", VoiceFollowupEnabled: boolPtr(true), VoiceFollowupTimeoutMs: 100}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not process follow-up utterance")
	}

	if dialog.starts != 2 {
		t.Fatalf("dialog starts = %d, want one recording per voice turn", dialog.starts)
	}
	if len(dialog.utterances) != 2 {
		t.Fatalf("utterances = %d, want 2", len(dialog.utterances))
	}
	if len(dialog.spoken) != 2 {
		t.Fatalf("spoken replies = %d, want 2 agent replies without wakeup acknowledgement", len(dialog.spoken))
	}
	if dialog.spoken[0] != "reply" || dialog.spoken[1] != "reply" {
		t.Fatalf("spoken texts = %#v, want only agent replies", dialog.spoken)
	}
}

func TestRunWakeupModeVoiceSessionStartsRecordingWithoutWakeupAck(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	var dialog *fakeAudioDialog
	dialog = &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		speak: func(ctx context.Context) error {
			if len(dialog.spoken) == 1 {
				go func() {
					time.Sleep(10 * time.Millisecond)
					sigChan <- syscall.SIGTERM
				}()
			}
			return nil
		},
	}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{InputMode: "stt", VoiceFollowupEnabled: boolPtr(true), VoiceMaxTurns: 1}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after signal")
	}

	wantPrefix := []string{"interrupt_output", "start", "reset"}
	if !opsHasPrefix(dialog.ops, wantPrefix) {
		t.Fatalf("dialog ops = %#v, want prefix %#v without wakeup acknowledgement", dialog.ops, wantPrefix)
	}
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "reply" {
		t.Fatalf("spoken texts = %#v, want only the agent reply", dialog.spoken)
	}
}

func TestRunVoiceSessionClosesAfterFollowupTimeout(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
	}

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 5}, dialog, nil, sigChan, make(chan voiceEvent, 1))
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	if dialog.starts != 2 {
		t.Fatalf("dialog starts = %d, want one recording for initial turn and one follow-up listen", dialog.starts)
	}
	if len(dialog.utterances) != 1 {
		t.Fatalf("utterances = %d, want 1", len(dialog.utterances))
	}
}

func TestRunVoiceSessionPersistsCompletedTurn(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
		prepareTurnInput: func(utterance []int16) (agent.TurnInput, error) {
			return agent.TurnInput{InputText: "现在几点了？", Transcript: "现在几点了？"}, nil
		},
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			return agent.RunResult{EpisodeID: "ep_voice", Output: "现在是凌晨 1 点 42 分。"}, nil
		},
	}

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 5}, dialog, nil, sigChan, make(chan voiceEvent, 1))
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	if len(dialog.persistedTurns) != 1 {
		t.Fatalf("persisted turns = %d, want 1", len(dialog.persistedTurns))
	}
	turn := dialog.persistedTurns[0]
	if turn.input.Transcript != "现在几点了？" {
		t.Fatalf("persisted transcript = %q, want voice transcript", turn.input.Transcript)
	}
	if turn.result.EpisodeID != "ep_voice" || turn.result.Output != "现在是凌晨 1 点 42 分。" {
		t.Fatalf("persisted result = %#v, want completed voice result", turn.result)
	}
	if len(turn.utterance) != 2 || turn.utterance[0] != 100 || turn.utterance[1] != 200 {
		t.Fatalf("persisted utterance = %#v, want captured samples", turn.utterance)
	}
}

func TestRunVoiceSessionClosesWhenAgentRequestsWaitForWakeup(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			return agent.RunResult{Output: "我先待命，等下次唤醒。", WaitForWakeupRequested: true}, nil
		},
	}

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 100}, dialog, nil, sigChan, make(chan voiceEvent, 1))
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	if dialog.starts != 1 {
		t.Fatalf("dialog starts = %d, want one recording for the single voice turn", dialog.starts)
	}
	if len(dialog.spoken) != 0 {
		t.Fatalf("spoken = %#v, want no final reply after wait-for-wakeup request", dialog.spoken)
	}
}

func TestRunVoiceSessionDoesNotRecordWhileSpeaking(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
	}
	spokeWhileRecording := false
	dialog.speak = func(ctx context.Context) error {
		if dialog.recordingActive {
			spokeWhileRecording = true
		}
		return nil
	}

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 5}, dialog, nil, sigChan, make(chan voiceEvent, 1))
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	if spokeWhileRecording {
		t.Fatal("dialog was recording while speaking")
	}
	if dialog.starts != 2 {
		t.Fatalf("dialog starts = %d, want recording once before speech and once after speech", dialog.starts)
	}
	wantOps := []string{"start", "reset", "stop", "speak:reply", "start", "reset", "stop"}
	if len(dialog.ops) != len(wantOps) {
		t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
	}
	for i, want := range wantOps {
		if dialog.ops[i] != want {
			t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
		}
	}
}

func TestRunVoiceTurnAppendsAndConfirmsVoiceNotificationTail(t *testing.T) {
	config := agent.DefaultConfig()
	config.Locale = "zh-CN"
	runtime := agent.NewRuntimeWithDeps(config, nil, nil, nil, agent.NewSkillIndex())
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), agent.VoiceNotificationEvent{
		Code: "storage", Severity: agent.SeverityWarning, State: agent.VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	dialog := &fakeAudioDialog{}

	runVoiceTurnWithInputContext(
		agent.Config{},
		dialog,
		runtime,
		agent.TurnInput{InputText: "voice"},
		nil,
		make(chan os.Signal, 1),
		make(chan voiceEvent, 1),
		agent.VoiceTurnContext{},
		true,
	)
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "reply。另外提醒一下，设备存储空间不足。" {
		t.Fatalf("spoken texts = %#v", dialog.spoken)
	}

	dialog.spoken = nil
	runVoiceTurnWithInputContext(
		agent.Config{},
		dialog,
		runtime,
		agent.TurnInput{InputText: "voice"},
		nil,
		make(chan os.Signal, 1),
		make(chan voiceEvent, 1),
		agent.VoiceTurnContext{},
		true,
	)
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "reply" {
		t.Fatalf("completed tail repeated in same active cycle: %#v", dialog.spoken)
	}
}

func TestRunVoiceTurnKeepsVoiceNotificationPendingWhenSpeechIsUnavailable(t *testing.T) {
	runtime := agent.NewRuntimeWithDeps(agent.DefaultConfig(), nil, nil, nil, agent.NewSkillIndex())
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), agent.VoiceNotificationEvent{
		Code: "storage", Severity: agent.SeverityWarning, State: agent.VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	dialog := &fakeAudioDialog{speechUnavailable: true}

	runVoiceTurnWithInputContext(
		agent.Config{},
		dialog,
		runtime,
		agent.TurnInput{InputText: "voice"},
		nil,
		make(chan os.Signal, 1),
		make(chan voiceEvent, 1),
		agent.VoiceTurnContext{},
		true,
	)
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "reply" {
		t.Fatalf("speech-unavailable turn consumed tail: %#v", dialog.spoken)
	}

	prepared := runtime.PrepareSpokenText(context.Background(), agent.SpokenTextInput{ResponseText: "next reply", TailAppendable: true})
	if prepared.Mode != agent.SpokenTextModeTail {
		t.Fatalf("pending reminder mode = %q, want %q", prepared.Mode, agent.SpokenTextModeTail)
	}
}

func TestRunVoiceTurnSpeaksReplacementForFinalLLMFailure(t *testing.T) {
	config := agent.DefaultConfig()
	config.Locale = "zh-CN"
	runtime := agent.NewRuntimeWithDeps(config, nil, nil, nil, agent.NewSkillIndex())
	dialog := &fakeAudioDialog{
		runTurn: func(context.Context) (agent.RunResult, error) {
			return agent.RunResult{TurnFailure: &agent.TurnFailure{Code: agent.TurnFailureNetworkUnavailable}}, errors.New("dial tcp: network is unreachable")
		},
	}

	runVoiceTurnWithInputContext(
		agent.Config{},
		dialog,
		runtime,
		agent.TurnInput{InputText: "voice"},
		nil,
		make(chan os.Signal, 1),
		make(chan voiceEvent, 1),
		agent.VoiceTurnContext{},
		true,
	)
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "当前网络不可用，暂时无法完成这个请求。" {
		t.Fatalf("spoken replacement = %#v", dialog.spoken)
	}
}

func TestRunVoiceTurnDoesNotSpeakReplacementForNonLLMFailure(t *testing.T) {
	runtime := agent.NewRuntimeWithDeps(agent.DefaultConfig(), nil, nil, nil, agent.NewSkillIndex())
	dialog := &fakeAudioDialog{
		runTurn: func(context.Context) (agent.RunResult, error) {
			return agent.RunResult{}, errors.New("reload skills: invalid manifest")
		},
	}

	runVoiceTurnWithInputContext(
		agent.Config{},
		dialog,
		runtime,
		agent.TurnInput{InputText: "voice"},
		nil,
		make(chan os.Signal, 1),
		make(chan voiceEvent, 1),
		agent.VoiceTurnContext{},
		true,
	)
	if len(dialog.spoken) != 0 {
		t.Fatalf("non-LLM failure spoke replacement: %#v", dialog.spoken)
	}
}

func TestRunVoiceTurnSignalWaitsForThinkingGoroutine(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	runStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	dialog := &fakeAudioDialog{
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			close(runStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return agent.RunResult{}, ctx.Err()
		},
	}

	go func() {
		<-runStarted
		sigChan <- syscall.SIGTERM
	}()

	result := runVoiceTurn(agent.Config{}, dialog, nil, []int16{1}, sigChan, events)
	if result.interrupted || result.waitForWakeupRequested || !result.exit {
		t.Fatalf("runVoiceTurn = interrupted:%v wait:%v exit:%v, want false false true", result.interrupted, result.waitForWakeupRequested, result.exit)
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("RunVoiceTurn goroutine was not finished before signal return")
	}
}

func TestRunVoiceTurnWakeupDuringThinkingImmediatelyCancelsRun(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	runStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	interruptOutputCalls := 0
	dialog := &fakeAudioDialog{
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			close(runStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return agent.RunResult{}, ctx.Err()
		},
		interruptOutput: func() {
			interruptOutputCalls++
		},
	}

	go func() {
		<-runStarted
		events <- voiceEventWakeup
	}()

	result := runVoiceTurn(agent.Config{}, dialog, nil, []int16{1}, sigChan, events)
	if !result.interrupted || result.exit || result.waitForWakeupRequested || result.nextTurn != nil {
		t.Fatalf("runVoiceTurn = interrupted:%v exit:%v wait:%v nextTurn:%v, want true false false nil",
			result.interrupted, result.exit, result.waitForWakeupRequested, result.nextTurn)
	}
	// Verify the context includes interruption marker and original user input
	followUpContext := result.followUpContext.RuntimeContext
	if !strings.Contains(followUpContext, "Voice interruption") {
		t.Errorf("follow-up context missing interruption marker: %s", followUpContext)
	}
	if !strings.Contains(followUpContext, "voice") {
		t.Errorf("follow-up context missing original user input: %s", followUpContext)
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("RunAgentTurn goroutine was not canceled before result returned")
	}
	if interruptOutputCalls != 1 {
		t.Fatalf("InterruptOutput calls = %d, want 1", interruptOutputCalls)
	}
	if len(dialog.queuedSteers) != 0 {
		t.Fatalf("queued steers = %#v, want none for GPIO hard interrupt during thinking", dialog.queuedSteers)
	}
	if dialog.steerInterrupts != 0 {
		t.Fatalf("steer interrupts = %d, want 0 for GPIO hard interrupt during thinking", dialog.steerInterrupts)
	}
}

func TestRunVoiceTurnPersistsUserBeforeRunEvents(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	historyOrder := make([]string, 0, 3)
	dialog := &fakeAudioDialog{
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			historyOrder = append(historyOrder, "role_output")
			return agent.RunResult{EpisodeID: "ep-voice", Output: "reply"}, nil
		},
		persistVoiceTurn: func(input agent.TurnInput, result agent.RunResult, utterance []int16) {
			if input.InputText != "" || input.Transcript != "" {
				historyOrder = append(historyOrder, "user")
			}
			if result.Output != "" {
				historyOrder = append(historyOrder, "assistant")
			}
		},
	}

	result := runVoiceTurnWithInput(
		agent.Config{},
		dialog,
		nil,
		agent.TurnInput{InputText: "voice command"},
		[]int16{1, 2, 3},
		sigChan,
		events,
	)

	if result.interrupted || result.waitForWakeupRequested || result.exit {
		t.Fatalf("runVoiceTurnWithInput = interrupted:%v wait:%v exit:%v, want false false false", result.interrupted, result.waitForWakeupRequested, result.exit)
	}
	want := []string{"user", "role_output", "assistant"}
	if len(historyOrder) != len(want) {
		t.Fatalf("history order = %#v, want %#v", historyOrder, want)
	}
	for i := range want {
		if historyOrder[i] != want[i] {
			t.Fatalf("history order = %#v, want %#v", historyOrder, want)
		}
	}
}

func TestRunVoiceTurnSignalWaitsForSpeakingGoroutine(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	speakStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	dialog := &fakeAudioDialog{
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			return agent.RunResult{Output: taggedReply("reply")}, nil
		},
		speak: func(ctx context.Context) error {
			close(speakStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return ctx.Err()
		},
	}

	go func() {
		<-speakStarted
		sigChan <- syscall.SIGTERM
	}()

	result := runVoiceTurn(agent.Config{}, dialog, nil, []int16{1}, sigChan, events)
	if result.interrupted || result.waitForWakeupRequested || !result.exit {
		t.Fatalf("runVoiceTurn = interrupted:%v wait:%v exit:%v, want false false true", result.interrupted, result.waitForWakeupRequested, result.exit)
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("Speak goroutine was not finished before signal return")
	}
}

func TestCaptureUtteranceTimeoutDiscardsSilenceWhenVADNeverDetectedSpeech(t *testing.T) {
	vad := newTestAudioVAD(t, []float64{0.1, 0.1, 0.1, 0.1})
	dialog := &fakeAudioDialog{
		vad: vad,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(constantSamples(vad.FrameSamples()*4, 20)...)},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
	}

	utterance, exit := captureUtteranceWithTimeout(dialog, make(chan os.Signal, 1), make(chan voiceEvent, 1), time.Millisecond)
	if exit {
		t.Fatal("captureUtteranceWithTimeout returned exit=true")
	}
	if len(utterance) != 0 {
		t.Fatalf("utterance samples = %d, want 0 for pure silence timeout", len(utterance))
	}
}

func TestCaptureUtteranceTimeoutReturnsBufferedSpeechWhenVADStarted(t *testing.T) {
	vad := newTestAudioVAD(t, []float64{0.9, 0.9, 0.9})
	speech := alternatingSamples(vad.FrameSamples()*3, 700, 1000)
	dialog := &fakeAudioDialog{
		vad: vad,
		chunks: []*agent.AudioChunkResult{
			{PCM: pcm16BytesFromSamples(speech...)},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
	}

	utterance, exit := captureUtteranceWithTimeout(dialog, make(chan os.Signal, 1), make(chan voiceEvent, 1), time.Millisecond)
	if exit {
		t.Fatal("captureUtteranceWithTimeout returned exit=true")
	}
	if len(utterance) == 0 {
		t.Fatal("expected buffered speech after timeout")
	}
}

func TestListenOneUtteranceIgnoresWakeupWhileAlreadyListening(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	firstRead := true
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		onRead: func(d *fakeAudioDialog, chunk *agent.AudioChunkResult) {
			if firstRead {
				firstRead = false
				events <- voiceEventWakeup
			}
		},
	}

	utterance, exit := listenOneUtterance(dialog, sigChan, events, time.Second)
	if exit {
		t.Fatal("listenOneUtterance returned exit=true")
	}
	if got, want := utterance, []int16{100, 200}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("utterance = %#v, want original recording samples %#v", got, want)
	}
	if len(dialog.frames) != 1 || dialog.frames[0][0] != 100 || dialog.frames[0][1] != 200 {
		t.Fatalf("VAD frames = %#v, want original recording samples", dialog.frames)
	}
	wantOps := []string{"start", "reset", "stop"}
	if len(dialog.ops) != len(wantOps) {
		t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
	}
	for i, want := range wantOps {
		if dialog.ops[i] != want {
			t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
		}
	}
}

func TestRunVoiceSessionDrainsBurstWakeupsWhileAlreadyListening(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 8)
	firstRead := true
	turnStarted := make(chan struct{})
	allowTurnResult := make(chan struct{})
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
		onRead: func(d *fakeAudioDialog, chunk *agent.AudioChunkResult) {
			if firstRead {
				firstRead = false
				for i := 0; i < 4; i++ {
					events <- voiceEventWakeup
				}
			}
		},
		runTurn: func(ctx context.Context) (agent.RunResult, error) {
			close(turnStarted)
			select {
			case <-ctx.Done():
				return agent.RunResult{}, ctx.Err()
			case <-allowTurnResult:
				return agent.RunResult{Output: taggedReply("reply")}, nil
			}
		},
	}
	go func() {
		<-turnStarted
		time.Sleep(10 * time.Millisecond)
		close(allowTurnResult)
	}()

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 5}, dialog, nil, sigChan, events)
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	if len(dialog.spoken) != 1 || dialog.spoken[0] != "reply" {
		t.Fatalf("spoken texts = %#v, want reply after draining listen-time wakeups", dialog.spoken)
	}
}

func TestVoiceSteerListenTimeoutIgnoresFollowupTimeout(t *testing.T) {
	cfg := agent.Config{VoiceFollowupTimeoutMs: 5}
	if got := voiceSteerListenTimeoutForConfig(cfg); got != 45*time.Second {
		t.Fatalf("voiceSteerListenTimeoutForConfig() = %s, want 45s", got)
	}
}

func TestCaptureVoiceSteerInterruptsOutputBeforeRecording(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(300, 400)}},
		},
		utterancesToReturn: [][]int16{
			{300, 400},
		},
		prepareTurnInput: func(utterance []int16) (agent.TurnInput, error) {
			return agent.TurnInput{InputText: "change course", Transcript: "change course"}, nil
		},
	}

	nextTurn, exit := captureVoiceSteer(agent.Config{VoiceFollowupTimeoutMs: 50}, dialog, sigChan, events)
	if exit {
		t.Fatal("captureVoiceSteer returned exit=true")
	}

	wantOps := []string{"interrupt_output", "start", "reset", "stop"}
	if len(dialog.ops) != len(wantOps) {
		t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
	}
	for i, want := range wantOps {
		if dialog.ops[i] != want {
			t.Fatalf("dialog ops = %#v, want %#v", dialog.ops, wantOps)
		}
	}
	if nextTurn == nil || nextTurn.input.InputText != "change course" {
		t.Fatalf("next turn = %#v, want change course", nextTurn)
	}
	if len(nextTurn.utterance) != 2 || nextTurn.utterance[0] != 300 || nextTurn.utterance[1] != 400 {
		t.Fatalf("next turn utterance = %#v, want captured samples", nextTurn.utterance)
	}
	if len(dialog.queuedSteers) != 0 {
		t.Fatalf("queued steers = %#v, want daemon wakeup interrupt to bypass QueueSteer", dialog.queuedSteers)
	}
}

func TestRunVoiceSessionWakeupInterruptsSpeaking(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 2)
	speakStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	speakCalls := 0
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
		},
		repeatEmpty: true,
		readDelay:   time.Millisecond,
		speak: func(ctx context.Context) error {
			speakCalls++
			if speakCalls > 1 {
				return nil
			}
			close(speakStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return ctx.Err()
		},
	}

	go func() {
		<-speakStarted
		events <- voiceEventWakeup
	}()

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 5}, dialog, nil, sigChan, events)
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("speaking context was not canceled")
	}
	if len(dialog.spoken) != 1 {
		t.Fatalf("spoken texts = %d, want only interrupted reply without wakeup acknowledgement", len(dialog.spoken))
	}
	if dialog.starts != 2 {
		t.Fatalf("dialog starts = %d, want recording restarted immediately after wakeup interrupt", dialog.starts)
	}
}

func TestRunVoiceSessionSpeakingInterruptMarksNextUtteranceCorrection(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	events := make(chan voiceEvent, 1)
	speakStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	speakCalls := 0
	dialog := &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{{PCM: pcm16BytesFromSamples(300, 400)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
			{300, 400},
		},
		prepareTurnInput: func(utterance []int16) (agent.TurnInput, error) {
			if len(utterance) > 0 && utterance[0] == 300 {
				return agent.TurnInput{InputText: "改成明天。", Transcript: "改成明天。"}, nil
			}
			return agent.TurnInput{InputText: "订今天的票。", Transcript: "订今天的票。"}, nil
		},
		speak: func(ctx context.Context) error {
			speakCalls++
			if speakCalls > 1 {
				return nil
			}
			close(speakStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return ctx.Err()
		},
	}

	go func() {
		<-speakStarted
		events <- voiceEventWakeup
	}()

	exit := runVoiceSession(agent.Config{VoiceFollowupTimeoutMs: 50, VoiceMaxTurns: 1}, dialog, nil, sigChan, events)
	if exit {
		t.Fatal("runVoiceSession returned exit=true")
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("speaking context was not canceled")
	}
	if len(dialog.voiceTurnContexts) != 2 {
		t.Fatalf("voice turn contexts = %#v, want original turn and correction follow-up", dialog.voiceTurnContexts)
	}
	if dialog.voiceTurnContexts[0] != (agent.VoiceTurnContext{}) {
		t.Fatalf("first voice turn context = %#v, want ordinary new task", dialog.voiceTurnContexts[0])
	}
	// Verify the second turn has interruption context with preserved user input and agent response
	secondContext := dialog.voiceTurnContexts[1].RuntimeContext
	if !strings.Contains(secondContext, "Voice interruption") {
		t.Errorf("second turn context missing interruption marker: %s", secondContext)
	}
	if !strings.Contains(secondContext, "订今天的票") && !strings.Contains(secondContext, "总结这段内容") {
		t.Errorf("second turn context missing original user input: %s", secondContext)
	}
	if !strings.Contains(secondContext, "reply") {
		t.Errorf("second turn context missing agent response: %s", secondContext)
	}
}

func TestRunWakeupModeSTTSpeakingInterruptMarksNextTurnCorrection(t *testing.T) {
	advanceWakeup := withWakeupDebounceClock(t, time.Unix(0, 0))
	sigChan := make(chan os.Signal, 1)
	watcher := &fakeWakeupWatcher{fireOnStart: true}
	speakStarted := make(chan struct{})
	ctxCanceled := make(chan struct{})
	voiceSessionDisabled := false
	speakCalls := 0
	var dialog *fakeAudioDialog
	dialog = &fakeAudioDialog{
		frameSamples: 2,
		chunkBatches: [][]*agent.AudioChunkResult{
			{{PCM: pcm16BytesFromSamples(100, 200)}},
			{{PCM: pcm16BytesFromSamples(300, 400)}},
		},
		utterancesToReturn: [][]int16{
			{100, 200},
			{300, 400},
		},
		prepareTurnInput: func(utterance []int16) (agent.TurnInput, error) {
			if len(utterance) > 0 && utterance[0] == 300 {
				return agent.TurnInput{InputText: "短一点。", Transcript: "短一点。"}, nil
			}
			return agent.TurnInput{InputText: "总结这段内容。", Transcript: "总结这段内容。"}, nil
		},
		speak: func(ctx context.Context) error {
			speakCalls++
			if speakCalls > 1 {
				sigChan <- syscall.SIGTERM
				return nil
			}
			close(speakStarted)
			<-ctx.Done()
			close(ctxCanceled)
			return ctx.Err()
		},
	}

	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{InputMode: "stt", VoiceFollowupEnabled: &voiceSessionDisabled, VoiceFollowupTimeoutMs: 50}, dialog, nil, sigChan, func(pin int, callback func()) (wakeupWatcher, error) {
			watcher.callback = callback
			return watcher, nil
		})
		close(done)
	}()

	select {
	case <-speakStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not start speaking")
	}
	advanceWakeup(wakeupDebounceInterval + time.Millisecond)
	watcher.callback()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWakeupMode did not stop after correction turn")
	}
	select {
	case <-ctxCanceled:
	default:
		t.Fatal("speaking context was not canceled")
	}
	if len(dialog.voiceTurnContexts) != 2 {
		t.Fatalf("voice turn contexts = %#v, want original turn and correction follow-up", dialog.voiceTurnContexts)
	}
	if dialog.voiceTurnContexts[0] != (agent.VoiceTurnContext{}) {
		t.Fatalf("first voice turn context = %#v, want ordinary new task", dialog.voiceTurnContexts[0])
	}
	// Verify the second turn has interruption context with preserved user input and agent response
	secondContext := dialog.voiceTurnContexts[1].RuntimeContext
	if !strings.Contains(secondContext, "Voice interruption") {
		t.Errorf("second turn context missing interruption marker: %s", secondContext)
	}
	if !strings.Contains(secondContext, "订今天的票") && !strings.Contains(secondContext, "总结这段内容") {
		t.Errorf("second turn context missing original user input: %s", secondContext)
	}
	if !strings.Contains(secondContext, "reply") {
		t.Errorf("second turn context missing agent response: %s", secondContext)
	}
}

func pcm16Bytes(sample int16) []byte {
	return pcm16BytesFromSamples(sample)
}

func pcm16BytesFromSamples(samples ...int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(sample))
	}
	return out
}

func constantSamples(samples int, value int16) []int16 {
	out := make([]int16, samples)
	for i := range out {
		out[i] = value
	}
	return out
}

func alternatingSamples(samples int, center, amplitude int16) []int16 {
	out := make([]int16, samples)
	for i := range out {
		if i%2 == 0 {
			out[i] = center + amplitude
		} else {
			out[i] = center - amplitude
		}
	}
	return out
}

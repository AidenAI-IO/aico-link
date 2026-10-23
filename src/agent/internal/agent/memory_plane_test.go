package agent

import (
	"aiden-agent/internal/agent/screen"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	langtools "github.com/tmc/langchaingo/tools"
)

func TestPersistentEpisodeRecorderWritesIncrementalEventsAndMarksInterrupted(t *testing.T) {
	ctx := context.Background()
	store := NewTaskEpisodeStore(filepath.Join(t.TempDir(), "episodes"))
	recorder := NewPersistentEpisodeRecorder(MemoryRetrieveRequest{
		Input:     "打开设置",
		EpisodeID: "ep_incremental",
	}, MemoryContext{}, store)

	if err := recorder.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	recorder.append(TaskEpisodeEvent{
		Type:      "planner_decision",
		Role:      "agent",
		Objective: "打开设置",
		Plan:      []string{"打开设置"},
		NextStep:  "点击设置",
	})

	eventPaths, err := filepath.Glob(filepath.Join(store.rootDir, "*", "ep_incremental", "events.jsonl"))
	if err != nil || len(eventPaths) != 1 {
		t.Fatalf("episode events glob paths=%#v err=%v", eventPaths, err)
	}
	data, err := os.ReadFile(eventPaths[0])
	if err != nil {
		t.Fatalf("read incremental events: %v", err)
	}
	if !strings.Contains(string(data), `"planner_decision"`) {
		t.Fatalf("incremental event not persisted:\n%s", data)
	}

	marked, err := store.MarkRunningEpisodesInterrupted(ctx, "restart")
	if err != nil {
		t.Fatalf("MarkRunningEpisodesInterrupted() error = %v", err)
	}
	if marked != 1 {
		t.Fatalf("marked = %d, want 1", marked)
	}
	episode, err := store.Get(ctx, "ep_incremental")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if episode.Status != "interrupted" || episode.Outcome.FailureReason != "restart" {
		t.Fatalf("episode not marked interrupted: %#v", episode)
	}
	if len(episode.Events) != 1 || episode.Events[0].Type != "planner_decision" {
		t.Fatalf("unexpected persisted events: %#v", episode.Events)
	}
}

func TestMemoryPlaneStartsBothProcessorsBeforeSharedWorker(t *testing.T) {
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	if err := plane.StartMemoryWorker(&episodeMemoryScriptedModel{}); err != nil {
		t.Fatalf("StartMemoryWorker() error = %v", err)
	}
	defer plane.StopEpisodeMemory()
	defer plane.StopNotificationMemory()

	plane.memoryWorkerMu.RLock()
	worker := plane.memoryWorker
	episode := plane.episodeProcessor
	notification := plane.notificationProcessor
	plane.memoryWorkerMu.RUnlock()
	if worker == nil || episode == nil || notification == nil {
		t.Fatalf("worker=%p episode=%p notification=%p, want all initialized", worker, episode, notification)
	}
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if !worker.started || len(worker.processors) != 2 || worker.processors[0] != episode || worker.processors[1] != notification {
		t.Fatalf("started=%v processors=%#v, want [episode notification]", worker.started, worker.processors)
	}
}

func TestTaskEpisodeIndexSummaryOmitsScreenshotBase64(t *testing.T) {
	ctx := context.Background()
	memoryDir := filepath.Join(t.TempDir(), "memory")
	store := NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes"))
	image := base64.StdEncoding.EncodeToString([]byte("jpeg bytes"))

	if _, err := store.AddEpisode(ctx, TaskEpisode{
		ID:        "ep_screenshot_summary",
		Status:    "active",
		StartedAt: "2026-06-02T00:00:00Z",
		EndedAt:   "2026-06-02T00:00:10Z",
		UserGoal:  "截图任务",
		Outcome: TaskEpisodeOutcome{
			Success:    true,
			FinalState: `{"width":447,"height":972,"format":"jpeg","size":29392,"data":"` + image + `"}`,
		},
		Events: []TaskEpisodeEvent{
			{
				EventID:        "evt_tool",
				Type:           runEventToolCall,
				ToolName:       "screenshot",
				RawObservation: "",
			},
			{
				EventID:        "evt_result",
				Type:           "tool_result",
				ToolName:       "screenshot",
				Observation:    `{"width":447,"height":972,"format":"jpeg","size":29392,"data":"` + image + `"}`,
				RawObservation: `{"width":447,"height":972,"format":"jpeg","size":29392,"data":"` + image + `"}`,
			},
		},
	}); err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}

	for _, rel := range []string{
		filepath.Join("episodes", "index-log.jsonl"),
		filepath.Join("episodes", "2026", "ep_screenshot_summary", "episode.yaml"),
		filepath.Join("episodes", "2026", "ep_screenshot_summary", "events.jsonl"),
	} {
		data, err := os.ReadFile(filepath.Join(memoryDir, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(string(data), `"data"`) || strings.Contains(string(data), image) {
			t.Fatalf("%s should not contain screenshot base64:\n%s", rel, data)
		}
	}
}

func TestRuntimeRetrieveUsesAutomaticScreenHints(t *testing.T) {
	ctx := context.Background()
	configDir := ensureTestConfigDir(t, t.TempDir())
	memoryDir := filepath.Join(configDir, "memory")
	longTerm := NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"))
	for _, item := range []MemoryItem{
		{
			ID:               "mem_matching_context",
			Type:             "procedure",
			Priority:         80,
			Confidence:       0.9,
			Tags:             []string{"登录"},
			Entities:         []string{"微信App"},
			Title:            "matching context",
			Content:          "This procedure applies to 640x1200.",
			Applicability:    map[string]string{"screen": "640x1200"},
			EvidenceExcerpts: []string{"matched evidence"},
		},
		{
			ID:               "mem_wrong_screen_runtime",
			Type:             "procedure",
			Priority:         100,
			Confidence:       0.9,
			Tags:             []string{"登录"},
			Entities:         []string{"微信App"},
			Title:            "wrong screen",
			Content:          "This should not apply to 640x1200.",
			Applicability:    map[string]string{"screen": "320x240"},
			EvidenceExcerpts: []string{"wrong screen evidence"},
		},
	} {
		if _, err := longTerm.AddMemory(ctx, item); err != nil {
			t.Fatalf("AddMemory(%s): %v", item.ID, err)
		}
	}

	screen := &screen.ScreenState{}
	screen.Update(640, 1200)
	model := &scriptedModel{responses: roleDirectResponses("done")}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:   configDir,
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Answer directly.",
		},
		&testModelResolver{model: model},
		NewMemoryManager(memoryDir),
		&ToolSet{tools: map[string]langtools.Tool{}, screen: screen},
		NewSkillIndex(),
	)
	if _, err := runtime.Run(ctx, RunRequest{Input: "登录微信App"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	plannerPrompt := messageText(model.messages[0])
	for _, unexpected := range []string{"mem_matching_context", "mem_wrong_screen_runtime"} {
		if strings.Contains(plannerPrompt, unexpected) {
			t.Fatalf("planner prompt should not contain %s:\n%s", unexpected, plannerPrompt)
		}
	}
}

func TestLongTermMemorySearchSkipsExpiredMemory(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_expired",
		Type:             "procedure",
		Priority:         90,
		Tags:             []string{"登录"},
		Title:            "过期流程",
		Content:          "旧流程。",
		EvidenceExcerpts: []string{"旧证据"},
		ExpiresAt:        "2000-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("AddMemory expired: %v", err)
	}
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "mem_active",
		Type:             "procedure",
		Priority:         80,
		Tags:             []string{"登录"},
		Title:            "有效流程",
		Content:          "新流程。",
		EvidenceExcerpts: []string{"新证据"},
		TTL:              "1d",
	}); err != nil {
		t.Fatalf("AddMemory active: %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"登录"}, Limit: 10})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 || results[0].ID != "mem_active" {
		t.Fatalf("expected only active result, got %#v", results)
	}
}

func TestMemoryPlaneDoesNotRecordNormalizedCoordinatesAsCalibration(t *testing.T) {
	ctx := context.Background()
	memoryDir := filepath.Join(t.TempDir(), "memory")
	plane := NewFilesystemMemoryPlane(memoryDir, DefaultMemoryExtractionConfig(), nil)

	if err := plane.CommitEpisode(ctx, TaskEpisode{
		ID:        "ep_normalized_coordinates",
		Status:    "active",
		StartedAt: "2026-06-02T00:00:00Z",
		EndedAt:   "2026-06-02T00:00:10Z",
		UserGoal:  "Tap the visible control",
		Outcome:   TaskEpisodeOutcome{Success: true},
		Events: []TaskEpisodeEvent{
			{
				EventID:   "evt_touch",
				Type:      runEventToolCall,
				ToolName:  "touch_gesture",
				ToolInput: `{"type":"tap","point":{"x":500,"y":500}}`,
			},
		},
	}); err != nil {
		t.Fatalf("CommitEpisode() error = %v", err)
	}

	if item, found, err := plane.device.Get(ctx, "cal_normalized_coordinates"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if found {
		t.Fatalf("normalized coordinates are the fixed contract, not a calibration: %#v", item)
	}
}

func TestDeviceMemoryIgnoresRetiredCoordinateModeCalibration(t *testing.T) {
	ctx := context.Background()
	store := NewDeviceMemoryStore(filepath.Join(t.TempDir(), "device"))
	if _, err := store.Upsert(ctx, DeviceMemoryItem{
		ID:      "cal_normalized_coordinates",
		Type:    "calibration",
		Status:  "active",
		Title:   "Prefer normalized coordinates",
		Content: "Keep preferring normalized coordinates unless calibration contradicts it.",
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	if item, found, err := store.Get(ctx, "cal_normalized_coordinates"); err != nil {
		t.Fatalf("Get() error = %v", err)
	} else if found {
		t.Fatalf("retired coordinate mode calibration must not be loaded: %#v", item)
	}
}

func TestCommitEpisodeDoesNotUpdateRecalledMemoryFromUnverifiedOutcome(t *testing.T) {
	ctx := context.Background()
	memoryDir := filepath.Join(t.TempDir(), "memory")
	longTerm := NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"))
	if _, err := longTerm.AddMemory(ctx, MemoryItem{
		ID:               "mem_proc",
		Type:             "procedure",
		Status:           "active",
		Priority:         80,
		Confidence:       0.8,
		Tags:             []string{"登录"},
		Entities:         []string{"微信App"},
		Title:            "旧流程",
		Content:          "使用旧流程。",
		EvidenceExcerpts: []string{"旧流程曾成功。"},
		TTL:              "45d",
	}); err != nil {
		t.Fatalf("AddMemory procedure: %v", err)
	}
	device := NewDeviceMemoryStore(filepath.Join(memoryDir, "device"))
	if _, err := device.Upsert(ctx, DeviceMemoryItem{
		ID:         "dev_proc",
		Type:       "procedure",
		Status:     "active",
		Title:      "设备旧流程",
		Content:    "设备上的旧流程。",
		DeviceID:   "default",
		Tags:       []string{"登录"},
		Entities:   []string{"微信App"},
		Confidence: 0.8,
		TTL:        "45d",
	}); err != nil {
		t.Fatalf("Upsert device procedure: %v", err)
	}

	plane := NewFilesystemMemoryPlane(memoryDir, DefaultMemoryExtractionConfig(), nil)
	for _, episodeID := range []string{"ep_fail_1", "ep_fail_2"} {
		if err := plane.CommitEpisode(ctx, TaskEpisode{
			ID:                  episodeID,
			Status:              "active",
			StartedAt:           "2026-06-01T00:00:00Z",
			EndedAt:             "2026-06-01T00:00:10Z",
			UserGoal:            "登录微信App",
			Tags:                []string{"登录"},
			Entities:            []string{"微信App"},
			RetrievedMemoryRefs: []string{"mem_proc", "dev_proc"},
			Outcome:             TaskEpisodeOutcome{Success: false, FailureReason: "旧流程失败"},
			Events: []TaskEpisodeEvent{
				{EventID: episodeID + "_tool", Type: runEventToolCall, ToolName: "echo"},
			},
		}); err != nil {
			t.Fatalf("CommitEpisode(%s): %v", episodeID, err)
		}
	}
	proc, err := readMemoryMarkdown(longTerm.memoryPath("mem_proc"))
	if err != nil {
		t.Fatalf("read mem_proc: %v", err)
	}
	if proc.Item.Status != "active" || proc.Item.FailureCount != 0 || proc.Item.Confidence != 0.8 {
		t.Fatalf("CommitEpisode changed long-term memory from unverified outcome: %#v", proc.Item)
	}
	devHits, err := device.Search(ctx, DeviceMemoryQuery{Terms: []string{"设备旧流程"}, Limit: 10})
	if err != nil {
		t.Fatalf("device search: %v", err)
	}
	if len(devHits) == 0 || devHits[0].ID != "dev_proc" || devHits[0].Type != "procedure" {
		t.Fatalf("CommitEpisode changed device memory from unverified outcome: %#v", devHits)
	}

	if _, err := longTerm.AddMemory(ctx, MemoryItem{
		ID:               "mem_fail_fresh",
		Type:             "failure",
		Status:           "active",
		Priority:         80,
		Confidence:       0.8,
		Tags:             []string{"登录"},
		Entities:         []string{"微信App"},
		Title:            "旧失败模式",
		Content:          "这个目标会失败。",
		EvidenceExcerpts: []string{"旧失败证据。"},
	}); err != nil {
		t.Fatalf("AddMemory fresh failure: %v", err)
	}

	if err := plane.CommitEpisode(ctx, TaskEpisode{
		ID:                  "ep_success",
		Status:              "active",
		StartedAt:           "2026-06-01T00:01:00Z",
		EndedAt:             "2026-06-01T00:01:10Z",
		UserGoal:            "登录微信App",
		Tags:                []string{"登录"},
		Entities:            []string{"微信App"},
		RetrievedMemoryRefs: []string{"mem_fail_fresh"},
		Outcome:             TaskEpisodeOutcome{Success: true, VerifierReason: "成功完成"},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_success_tool", Type: runEventToolCall, ToolName: "echo"},
		},
	}); err != nil {
		t.Fatalf("CommitEpisode success: %v", err)
	}
	fail, err := readMemoryMarkdown(longTerm.memoryPath("mem_fail_fresh"))
	if err != nil {
		t.Fatalf("read mem_fail_fresh: %v", err)
	}
	if fail.Item.Status != "active" || fail.Item.SuccessCount != 0 || len(fail.Item.ConflictsWith) != 0 {
		t.Fatalf("CommitEpisode changed recalled failure memory from unverified success: %#v", fail.Item)
	}
}

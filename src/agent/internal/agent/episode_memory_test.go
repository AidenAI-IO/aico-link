package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

type panickingEpisodeMemoryBatchProcessor struct{}

func (panickingEpisodeMemoryBatchProcessor) Initialize() error { return nil }
func (panickingEpisodeMemoryBatchProcessor) NextRunAt(context.Context) (time.Time, error) {
	return time.Time{}, nil
}
func (panickingEpisodeMemoryBatchProcessor) ProcessBatch(context.Context, func() bool) (MemoryBatchResult, error) {
	panic("batch failed")
}
func (panickingEpisodeMemoryBatchProcessor) logBatchError(error) {}

func TestEpisodeMemoryWorkerCleansUpPanickingBatch(t *testing.T) {
	worker := newMemoryWorker(panickingEpisodeMemoryBatchProcessor{}, defaultMemoryWorkerIdleDelay)
	worker.mu.Lock()
	batchCtx, cancel := worker.startBatchLocked(context.Background())
	worker.mu.Unlock()

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("executeBatch() did not propagate panic")
			}
		}()
		_, _ = worker.executeBatch(batchCtx, cancel)
	}()

	if batchCtx.Err() != context.Canceled {
		t.Fatalf("batch context error = %v, want canceled", batchCtx.Err())
	}
	done := make(chan struct{})
	go func() {
		worker.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker wait group leaked after panic")
	}
	worker.mu.Lock()
	running := worker.running
	worker.mu.Unlock()
	if running {
		t.Fatal("worker remained busy after panic")
	}
}

func TestProcessEpisodeMemoryNowContinuesAcrossBoundedBatches(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	proposal := episodeMemoryProposal{EpisodeAssessment: episodeMemoryAssessment{GoalResult: episodeGoalAchieved, Reason: "The app opened.", EvidenceRefs: []string{"result"}}}
	firstResults := make([]episodeMemoryBatchResult, 0, episodeMemoryBatchLimit)
	for index := 0; index < episodeMemoryBatchLimit; index++ {
		firstResults = append(firstResults, episodeMemoryBatchResult{EpisodeID: fmt.Sprintf("ep_batch_%02d", index), Proposal: proposal})
	}
	firstResponse, _ := json.Marshal(episodeMemoryBatchResponse{Results: firstResults})
	secondResponse, _ := json.Marshal(episodeMemoryBatchResponse{Results: []episodeMemoryBatchResult{{EpisodeID: fmt.Sprintf("ep_batch_%02d", episodeMemoryBatchLimit), Proposal: proposal}}})
	model := &episodeMemoryScriptedModel{responses: []string{string(firstResponse), string(secondResponse)}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for index := 0; index < episodeMemoryBatchLimit+1; index++ {
		episodeID := fmt.Sprintf("ep_batch_%02d", index)
		endedAt := time.Date(2026, 8, 14, 0, 0, index+1, 0, time.UTC)
		if _, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
			ID: episodeID, Status: "active",
			StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano),
			EndedAt:   endedAt.Format(time.RFC3339Nano),
			UserGoal:  "Open Settings",
			Events: []TaskEpisodeEvent{
				{EventID: "call", Type: runEventToolCall, ToolName: "open_app"},
				{EventID: "result", Type: "tool_result", ToolName: "open_app", Observation: "Settings opened"},
			},
		}); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episodeID, err)
		}
	}
	worker := newMemoryWorker(processor, defaultMemoryWorkerIdleDelay)
	plane.memoryWorker = worker
	plane.episodeProcessor = processor
	status, _, err := plane.ProcessEpisodeMemoryNow(ctx, fmt.Sprintf("ep_batch_%02d", episodeMemoryBatchLimit))
	if err != nil {
		t.Fatalf("ProcessEpisodeMemoryNow() error = %v", err)
	}
	if status.Status != episodeMemoryStatusDone {
		t.Fatalf("status = %q, want done", status.Status)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls = %d, want two bounded batch calls", got)
	}
}

func TestEpisodeMemoryProcessorSeparatesDifferentRetryAttempts(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{
		`{"results":[{"episode_id":"ep_attempt_zero","proposal":{"episode_assessment":{"goal_result":"achieved","reason":"First attempt completed.","evidence_refs":["ep_attempt_zero_result"]},"candidates":[]}}]}`,
		`{"results":[{"episode_id":"ep_attempt_two","proposal":{"episode_assessment":{"goal_result":"achieved","reason":"Retry completed.","evidence_refs":["ep_attempt_two_result"]},"candidates":[]}}]}`,
	}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := func(id string) TaskEpisode {
		return TaskEpisode{
			ID: id, Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:01:00Z", UserGoal: "Complete task",
			Events: []TaskEpisodeEvent{
				{EventID: id + "_call", Type: runEventToolCall, ToolName: "open_app"},
				{EventID: id + "_result", Type: "tool_result", ToolName: "open_app", Observation: "Task completed"},
			},
		}
	}
	works := []episodeMemoryWork{
		{episode: episode("ep_attempt_zero"), needsModel: true, status: episodeMemoryEpisodeStatus{AttemptCount: 0}},
		{episode: episode("ep_attempt_two"), needsModel: true, status: episodeMemoryEpisodeStatus{AttemptCount: 2}},
	}
	state := &episodeMemoryStateFile{Episodes: map[string]episodeMemoryEpisodeStatus{}}
	result := &MemoryBatchResult{}
	if stopped, err := processor.extractEpisodeMemoryWork(ctx, state, works, result); err != nil || stopped {
		t.Fatalf("extractEpisodeMemoryWork() = stopped %v, error %v", stopped, err)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls = %d, want one call per retry attempt", got)
	}
	if got, want := model.batchMaxTokens(0), episodeMemoryBatchTokenBudget(1, 0); got != want {
		t.Fatalf("attempt-zero budget = %d, want %d", got, want)
	}
	if got, want := model.batchMaxTokens(1), episodeMemoryBatchTokenBudget(1, 2); got != want {
		t.Fatalf("attempt-two budget = %d, want %d", got, want)
	}
}

func TestEpisodeMemoryProcessorDoesNotMarkLaterAttemptGroupsProcessing(t *testing.T) {
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	inner := &episodeMemoryScriptedModel{}
	model := &episodeMemoryBlockingModel{inner: inner, started: make(chan struct{}), release: make(chan struct{})}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := func(id string) TaskEpisode {
		return TaskEpisode{ID: id, Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:01:00Z"}
	}
	laterOriginal := episodeMemoryEpisodeStatus{Status: episodeMemoryStatusRetry, ExtractorVersion: episodeMemoryExtractorVersion, AttemptCount: 2}
	if err := processor.state.SetEpisode("ep_later_group", laterOriginal); err != nil {
		t.Fatalf("SetEpisode(later group) error = %v", err)
	}
	works := []episodeMemoryWork{
		{
			episode:    episode("ep_first_group"),
			needsModel: true,
			status: episodeMemoryEpisodeStatus{
				Status: episodeMemoryStatusProcessing, ExtractorVersion: episodeMemoryExtractorVersion, AttemptCount: 0,
			},
		},
		{
			episode:        episode("ep_later_group"),
			needsModel:     true,
			originalStatus: laterOriginal,
			status: episodeMemoryEpisodeStatus{
				Status: episodeMemoryStatusProcessing, ExtractorVersion: episodeMemoryExtractorVersion, AttemptCount: 2,
			},
		},
	}
	state := &episodeMemoryStateFile{Episodes: map[string]episodeMemoryEpisodeStatus{
		episodeMemoryStateKey("ep_later_group", episodeMemoryExtractorVersion): laterOriginal,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := processor.extractEpisodeMemoryWork(ctx, state, works, &MemoryBatchResult{})
		done <- err
	}()
	<-model.started

	snapshot, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if got := snapshot.Episodes[episodeMemoryStateKey("ep_first_group", episodeMemoryExtractorVersion)].Status; got != episodeMemoryStatusProcessing {
		t.Fatalf("first group status = %q, want processing", got)
	}
	if got := snapshot.Episodes[episodeMemoryStateKey("ep_later_group", episodeMemoryExtractorVersion)]; got != laterOriginal {
		t.Fatalf("later group status = %#v, want unchanged %#v", got, laterOriginal)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("extractEpisodeMemoryWork() error = %v", err)
	}
	snapshot, err = processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot(after cancel) error = %v", err)
	}
	if _, ok := snapshot.Episodes[episodeMemoryStateKey("ep_first_group", episodeMemoryExtractorVersion)]; ok {
		t.Fatal("canceled first group was not restored to its empty original state")
	}
	if got := snapshot.Episodes[episodeMemoryStateKey("ep_later_group", episodeMemoryExtractorVersion)]; got != laterOriginal {
		t.Fatalf("later group status after cancel = %#v, want unchanged %#v", got, laterOriginal)
	}
}

func TestEpisodeMemoryProcessorRejectsNegativeRetryBatchLimit(t *testing.T) {
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	processor := newEpisodeMemoryProcessor(plane, &episodeMemoryScriptedModel{})
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{ID: "ep_invalid_retry_limit", Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:01:00Z"}
	state := &episodeMemoryStateFile{Episodes: map[string]episodeMemoryEpisodeStatus{
		episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion): {
			Status:          episodeMemoryStatusRetry,
			RetryBatchLimit: -1,
		},
	}}
	_, _, err := processor.collectEpisodeMemoryWork(state, []TaskEpisode{episode}, 3, nil)
	if !errors.Is(err, errEpisodeMemoryInvalidRetryLimit) {
		t.Fatalf("collectEpisodeMemoryWork() error = %v, want errEpisodeMemoryInvalidRetryLimit", err)
	}
}

func TestEpisodeMemoryProcessorBatchesEligibleEpisodesIntoOneModelCall(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {
      "episode_id":"ep_batch_first",
      "proposal":{"episode_assessment":{"goal_result":"achieved","reason":"Settings opened.","evidence_refs":["ep_batch_first_result"]},"candidates":[]}
    },
    {
      "episode_id":"ep_batch_second",
      "proposal":{"episode_assessment":{"goal_result":"achieved","reason":"Clock opened.","evidence_refs":["ep_batch_second_result"]},"candidates":[]}
    }
  ]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for index, app := range []string{"Settings", "Clock"} {
		episodeID := []string{"ep_batch_first", "ep_batch_second"}[index]
		endedAt := time.Date(2026, 8, 14, 0, 0, index+1, 0, time.UTC)
		if _, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
			ID: episodeID, Status: "active",
			StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano),
			EndedAt:   endedAt.Format(time.RFC3339Nano),
			UserGoal:  "Open " + app,
			Events: []TaskEpisodeEvent{
				{EventID: episodeID + "_call", Type: runEventToolCall, ToolName: "open_app"},
				{EventID: episodeID + "_result", Type: "tool_result", ToolName: "open_app", Observation: app + " opened"},
			},
		}); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episodeID, err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one call for the Episode batch", got)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	for _, episodeID := range []string{"ep_batch_first", "ep_batch_second"} {
		if status := state.Episodes[episodeMemoryStateKey(episodeID, episodeMemoryExtractorVersion)]; status.Status != episodeMemoryStatusDone {
			t.Fatalf("status for %s = %#v, want done", episodeID, status)
		}
	}
}

func TestEpisodeMemoryProcessorKeepsValidBatchResultsWhenOneProposalIsInvalid(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"episode_id":"ep_valid","proposal":{"episode_assessment":{"goal_result":"achieved","reason":"Settings opened.","evidence_refs":["ep_valid_result"]},"candidates":[]}},
    {"episode_id":"ep_invalid","proposal":{"episode_assessment":{"goal_result":"achieved","reason":"Missing direct evidence.","evidence_refs":[]},"candidates":[]}}
  ]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatal(err)
	}
	for index, episodeID := range []string{"ep_valid", "ep_invalid"} {
		endedAt := time.Date(2026, 8, 14, 1, 0, index, 0, time.UTC)
		if _, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
			ID: episodeID, Status: "active", StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano), EndedAt: endedAt.Format(time.RFC3339Nano), UserGoal: "Open Settings",
			Events: []TaskEpisodeEvent{{EventID: episodeID + "_call", Type: runEventToolCall, ToolName: "open_app"}, {EventID: episodeID + "_result", Type: "tool_result", ToolName: "open_app", Observation: "Settings opened"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatal(err)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Episodes[episodeMemoryStateKey("ep_valid", episodeMemoryExtractorVersion)].Status; got != episodeMemoryStatusDone {
		t.Fatalf("valid status = %q, want done", got)
	}
	if got := state.Episodes[episodeMemoryStateKey("ep_invalid", episodeMemoryExtractorVersion)].Status; got != episodeMemoryStatusIgnored {
		t.Fatalf("invalid status = %q, want ignored", got)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one batch call", got)
	}
}

func TestProcessEpisodeMemoryNowWaitsForBusyWorker(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	inner := &episodeMemoryScriptedModel{responses: []string{`{
	  "episode_assessment":{"goal_result":"achieved","reason":"The app opened.","evidence_refs":["result"]},
	  "candidates":[]
	}`}}
	model := &episodeMemoryBlockingModel{inner: inner, started: make(chan struct{}), release: make(chan struct{})}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episodeID := "ep_busy_process_now"
	if _, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
		ID: episodeID, Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:00:01Z",
		UserGoal: "Open Settings",
		Events: []TaskEpisodeEvent{
			{EventID: "call", Type: runEventToolCall, ToolName: "open_app"},
			{EventID: "result", Type: "tool_result", ToolName: "open_app", Observation: "Settings opened"},
		},
	}); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	worker := newMemoryWorker(processor, defaultMemoryWorkerIdleDelay)
	plane.memoryWorker = worker
	plane.episodeProcessor = processor
	backgroundDone := make(chan error, 1)
	go func() {
		_, err := worker.ProcessNow(ctx)
		backgroundDone <- err
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("background batch did not start")
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(model.release)
	}()
	status, _, err := plane.ProcessEpisodeMemoryNow(ctx, episodeID)
	if err != nil {
		t.Fatalf("ProcessEpisodeMemoryNow() error = %v", err)
	}
	if status.Status != episodeMemoryStatusDone {
		t.Fatalf("status = %q, want done", status.Status)
	}
	if err := <-backgroundDone; err != nil {
		t.Fatalf("background ProcessNow() error = %v", err)
	}
}

func TestEpisodeMemoryProcessorPrefiltersNoiseAndProcessesSuccessfulDeviceEpisode(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The tool result confirms the requested app opened.","evidence_refs":["ep_success_result"]},
  "candidates":[]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	for _, episode := range []TaskEpisode{
		{
			ID: "ep_greeting", Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:00:01Z",
			UserGoal: "你好", Outcome: TaskEpisodeOutcome{Success: true, FinalAnswer: "你好！"},
			Events: []TaskEpisodeEvent{{EventID: "ep_greeting_answer", Type: "assistant_output", Content: "你好！"}},
		},
		{
			ID: "ep_memory_only", Status: "active", StartedAt: "2026-08-14T00:00:02Z", EndedAt: "2026-08-14T00:00:03Z",
			UserGoal: "记住我喜欢深色模式", Outcome: TaskEpisodeOutcome{Success: true},
			Events: []TaskEpisodeEvent{
				{EventID: "ep_memory_call", Type: runEventToolCall, ToolName: "save_memory", ToolInput: `{}`},
				{EventID: "ep_memory_result", Type: "tool_result", ToolName: "save_memory", Content: "saved"},
			},
		},
		{
			ID: "ep_success", Status: "active", StartedAt: "2026-08-14T00:00:04Z", EndedAt: "2026-08-14T00:00:05Z",
			UserGoal: "打开设置", Outcome: TaskEpisodeOutcome{Success: true, FinalState: "Settings is visible"},
			Events: []TaskEpisodeEvent{
				{EventID: "ep_success_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
				{EventID: "ep_success_result", Type: "tool_result", ToolName: "launch_app", Content: "opened Settings", Observation: "Settings is visible"},
			},
		},
	} {
		if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episode.ID, err)
		}
	}

	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want 1 for only the successful device Episode", got)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if state.Episodes[episodeMemoryStateKey("ep_greeting", episodeMemoryExtractorVersion)].Status != episodeMemoryStatusIgnored || state.Episodes[episodeMemoryStateKey("ep_memory_only", episodeMemoryExtractorVersion)].Status != episodeMemoryStatusIgnored {
		t.Fatalf("noise statuses = %#v, want ignored", state.Episodes)
	}
	if state.Episodes[episodeMemoryStateKey("ep_success", episodeMemoryExtractorVersion)].Status != episodeMemoryStatusDone {
		t.Fatalf("successful device Episode status = %#v, want done", state.Episodes[episodeMemoryStateKey("ep_success", episodeMemoryExtractorVersion)])
	}
}

func TestEpisodeMemoryProcessorCreatesMultipleTypedMemoriesWithOneModelCall(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"Settings opened and the display page became visible.","evidence_refs":["ep_extract_result"]},
  "candidates":[
    {
      "lesson_key":"open_display_settings",
      "type":"procedure",
      "action":"create",
      "memory_revision":1,
      "unresolved_conflict":false,
      "situation":"When the user wants the display settings on this device",
      "guidance":"Open Settings, then select Display",
      "expected_effect":"The Display settings page is visible",
      "scope":{"device_id":"device_a","app_name":"Settings","goal_pattern":"open display settings"},
      "tags":["settings","display"],
      "evidence_refs":["ep_extract_call","ep_extract_result","ep_extract_display_call","ep_extract_display_result"]
    },
    {
      "lesson_key":"settings_display_location",
      "type":"fact",
      "action":"create",
      "unresolved_conflict":false,
      "situation":"In the Settings app on this device",
      "guidance":"Use the Display entry to reach display controls",
      "expected_effect":"Display controls can be found without searching",
      "scope":{"device_id":"device_a","app_name":"Settings"},
      "tags":["settings","display-entry"],
      "evidence_refs":["ep_extract_result"]
    }
  ]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_extract", Status: "active", StartedAt: "2026-08-14T01:00:00Z", EndedAt: "2026-08-14T01:00:05Z",
		UserGoal: "打开显示设置", DeviceScope: map[string]string{"device_id": "device_a"},
		Outcome: TaskEpisodeOutcome{Success: true, FinalState: "Display settings visible"},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_extract_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
			{EventID: "ep_extract_result", Type: "tool_result", ToolName: "launch_app", Content: "Display settings visible", Observation: "Display settings visible"},
			{EventID: "ep_extract_display_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":500,"y":500}}`},
			{EventID: "ep_extract_display_result", Type: "tool_result", ToolName: "touch_gesture", Content: "Display page opened", Observation: "Display controls visible"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want 1", got)
	}
	items, err := plane.device.readAll()
	if err != nil {
		t.Fatalf("readAll() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("device memories = %#v, want 2", items)
	}
	types := map[string]bool{}
	for _, item := range items {
		types[item.Type] = true
		if item.Status != "active" || item.Revision != 1 || item.LessonKey == "" {
			t.Fatalf("created memory = %#v, want active revision 1 with lesson key", item)
		}
		if !hasEpisodeEvidence(item.EvidenceRefs, episode.ID) {
			t.Fatalf("created memory missing Episode evidence: %#v", item)
		}
	}
	if !types["procedure"] || !types["fact"] {
		t.Fatalf("memory types = %#v, want procedure and fact", types)
	}
}

func TestEpisodeMemoryProcessorUpdatesRevisionAndQuarantinesUnresolvedConflict(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	if _, err := plane.device.Upsert(ctx, DeviceMemoryItem{
		ID: "devmem_settings_fact", Type: "fact", Status: "active", Revision: 1,
		ExtractorVersion: episodeMemoryExtractorVersion, LessonKey: "settings_display_location",
		Title: "Display is under Settings", Summary: "Open Display from Settings",
		Content:  "Situation: In Settings\nGuidance: Open Display\nExpected effect: Display controls are visible",
		Priority: 10, Confidence: 0.2,
		DeviceID: "device_a", AppName: "Settings", Tags: []string{episodeMemoryTag, "settings", "display"},
		Applicability: map[string]string{"device_id": "device_a", "app_name": "Settings"},
		EvidenceRefs:  []MemorySourceRef{{Type: "episode", ID: "ep_original", EventIDs: []string{"evt_original"}}},
	}); err != nil {
		t.Fatalf("Upsert(existing) error = %v", err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"episode_id":"ep_update","proposal":{
  "episode_assessment":{"goal_result":"achieved","reason":"The Display entry opened the controls.","evidence_refs":["ep_update_result"]},
  "candidates":[{
    "lesson_key":"settings_display_location_update","type":"fact","action":"update","memory_id":"devmem_settings_fact","memory_revision":1,
	    "unresolved_conflict":false,"situation":"In Settings on device A","guidance":"Open the Display entry from the main list","expected_effect":"Display controls are visible","confidence":0.84,
    "scope":{"device_id":"device_a","app_name":"Settings","page_name":"main"},"tags":["settings","display"],
    "evidence_refs":["ep_update_result"]
  }]
}},
    {"episode_id":"ep_conflict","proposal":{
  "episode_assessment":{"goal_result":"unknown","reason":"The same scope now shows a different location and there is not enough evidence to condition it.","evidence_refs":["ep_conflict_result"]},
  "candidates":[{
    "lesson_key":"settings_display_location_conflict","type":"fact","action":"update","memory_id":"devmem_settings_fact","memory_revision":1,
    "unresolved_conflict":true,"conflict_reason":"The same Settings scope showed an incompatible location without a distinguishing precondition.",
    "situation":"In Settings on device A","guidance":"Do not rely on one fixed Display location until the differing UI states can be distinguished","expected_effect":"The agent avoids following an unsafe location rule",
    "scope":{"device_id":"device_a","app_name":"Settings"},"tags":["settings","display"],
    "evidence_refs":["ep_conflict_result"]
  }]
}}
  ]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for index, episode := range []TaskEpisode{
		{
			ID: "ep_update", Status: "active", StartedAt: "2026-08-14T02:00:00Z", EndedAt: "2026-08-14T02:00:05Z",
			UserGoal: "打开设置里的显示", DeviceScope: map[string]string{"device_id": "device_a"}, Outcome: TaskEpisodeOutcome{Success: true},
			Events: []TaskEpisodeEvent{
				{EventID: "ep_update_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":500,"y":500}}`},
				{EventID: "ep_update_result", Type: "tool_result", ToolName: "touch_gesture", Content: "Display controls visible"},
			},
		},
		{
			ID: "ep_conflict", Status: "active", StartedAt: "2026-08-14T03:00:00Z", EndedAt: "2026-08-14T03:00:05Z",
			UserGoal: "打开设置里的显示", DeviceScope: map[string]string{"device_id": "device_a"}, Outcome: TaskEpisodeOutcome{Success: false},
			Events: []TaskEpisodeEvent{
				{EventID: "ep_conflict_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":500,"y":500}}`},
				{EventID: "ep_conflict_result", Type: "tool_result", ToolName: "touch_gesture", Content: "A different Settings section opened"},
			},
		},
	} {
		if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
			t.Fatalf("AddEpisode(%d) error = %v", index, err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one for both Episodes", got)
	}
	updated, found, err := plane.device.Get(ctx, "devmem_settings_fact")
	if err != nil || !found {
		t.Fatalf("Get(first batch) found=%v error=%v", found, err)
	}
	if updated.Status != "active" || updated.Revision != 2 || updated.Priority != 60 || updated.Confidence != 0.84 {
		t.Fatalf("first batch memory = %#v, want active revision 2", updated)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if status := state.Episodes[episodeMemoryStateKey("ep_conflict", episodeMemoryExtractorVersion)]; status.Status != episodeMemoryStatusRetry || status.Proposal != nil {
		t.Fatalf("conflicting batch status = %#v, want retry without stale proposal", status)
	}
	model.appendResponses(`{
  "episode_assessment":{"goal_result":"unknown","reason":"The same scope now shows a different location and there is not enough evidence to condition it.","evidence_refs":["ep_conflict_result"]},
  "candidates":[{
    "lesson_key":"settings_display_location_conflict","type":"fact","action":"update","memory_id":"devmem_settings_fact","memory_revision":2,
    "unresolved_conflict":true,"conflict_reason":"The same Settings scope showed an incompatible location without a distinguishing precondition.",
	    "situation":"In Settings on device A","guidance":"Do not rely on one fixed Display location until the differing UI states can be distinguished","expected_effect":"The agent avoids following an unsafe location rule","confidence":0.62,
    "scope":{"device_id":"device_a","app_name":"Settings"},"tags":["settings","display"],
    "evidence_refs":["ep_conflict_result"]
  }]
}`)
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch(retry) error = %v", err)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls after revision collision = %d, want one batch call plus one focused retry", got)
	}
	updated, found, err = plane.device.Get(ctx, "devmem_settings_fact")
	if err != nil || !found {
		t.Fatalf("Get(updated) found=%v error=%v", found, err)
	}
	if updated.Status != "disputed" || updated.Revision != 3 || len(updated.RevisionHistory) != 2 || updated.Priority != 60 || updated.Confidence != 0.62 {
		t.Fatalf("updated memory = %#v, want disputed revision 3 with two prior revisions", updated)
	}
	if distinctEpisodeEvidenceCount(updated.EvidenceRefs) != 3 {
		t.Fatalf("evidence refs = %#v, want original plus two updates", updated.EvidenceRefs)
	}
	hits, err := plane.device.Search(ctx, DeviceMemoryQuery{Terms: []string{"Display"}, Limit: 8})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("normal Recall returned disputed memory: %#v", hits)
	}
}

func TestEpisodeMemoryProcessorResumesPersistedProposalWithoutCallingModelAgain(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_resume", Status: "active", StartedAt: "2026-08-14T04:00:00Z", EndedAt: "2026-08-14T04:00:05Z",
		UserGoal: "查看当前屏幕尺寸", DeviceScope: map[string]string{"device_id": "device_a"}, Outcome: TaskEpisodeOutcome{Success: true},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_resume_call", Type: runEventToolCall, ToolName: "screenshot", ToolInput: `{}`},
			{EventID: "ep_resume_result", Type: "tool_result", ToolName: "screenshot", Observation: `{"width":1080,"height":1920}`},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	proposal := episodeMemoryProposal{
		EpisodeAssessment: episodeMemoryAssessment{GoalResult: "achieved", Reason: "The screenshot result reports the dimensions.", EvidenceRefs: []string{"ep_resume_result"}},
		Candidates: []episodeMemoryCandidate{{
			LessonKey: "screen_dimensions", Type: "fact", Action: "create", Retention: episodeMemoryRetentionDurable,
			Situation: "When operating device A", Guidance: "Use a 1080x1920 screen model", ExpectedEffect: "Coordinates are interpreted against the observed screen size",
			Scope: map[string]string{"device_id": "device_a", "screen": "1080x1920"}, Tags: []string{"screen", "dimensions"}, EvidenceRefs: []string{"ep_resume_result"},
		}},
	}
	if err := processor.state.SetEpisode(episode.ID, episodeMemoryEpisodeStatus{
		Status: episodeMemoryStatusProposed, ExtractorVersion: episodeMemoryExtractorVersion, Proposal: &proposal,
	}); err != nil {
		t.Fatalf("SetEpisode(proposed) error = %v", err)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 0 {
		t.Fatalf("model calls = %d, want 0 when resuming proposed output", got)
	}
	items, err := plane.device.readAll()
	if err != nil {
		t.Fatalf("readAll() error = %v", err)
	}
	if len(items) != 1 || items[0].LessonKey != "screen_dimensions" {
		t.Fatalf("resumed memories = %#v, want persisted proposal applied once", items)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch(second) error = %v", err)
	}
	items, err = plane.device.readAll()
	if err != nil || len(items) != 1 {
		t.Fatalf("second pass memories = %#v error=%v, want no duplicate", items, err)
	}
}

func TestEpisodeMemoryProcessorDoesNotCarryErrorIntoNextPersistedProposal(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	failedEpisode := TaskEpisode{
		ID: "ep_extract_error", Status: "active", StartedAt: "2026-08-14T04:10:00Z", EndedAt: "2026-08-14T04:10:05Z",
		UserGoal: "打开设置", DeviceScope: map[string]string{"device_id": "device_a"}, Outcome: TaskEpisodeOutcome{Success: false},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_extract_error_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
			{EventID: "ep_extract_error_result", Type: "tool_result", ToolName: "launch_app", Content: "request failed"},
		},
	}
	resumedEpisode := TaskEpisode{
		ID: "ep_resume_after_error", Status: "active", StartedAt: "2026-08-14T04:11:00Z", EndedAt: "2026-08-14T04:11:05Z",
		UserGoal: "查看当前屏幕尺寸", DeviceScope: map[string]string{"device_id": "device_a"}, Outcome: TaskEpisodeOutcome{Success: true},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_resume_after_error_call", Type: runEventToolCall, ToolName: "screenshot", ToolInput: `{}`},
			{EventID: "ep_resume_after_error_result", Type: "tool_result", ToolName: "screenshot", Observation: `{"width":1080,"height":1920}`},
		},
	}
	for _, episode := range []TaskEpisode{failedEpisode, resumedEpisode} {
		if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episode.ID, err)
		}
	}
	proposal := episodeMemoryProposal{
		EpisodeAssessment: episodeMemoryAssessment{GoalResult: "achieved", Reason: "The screenshot reports the dimensions.", EvidenceRefs: []string{"ep_resume_after_error_result"}},
		Candidates: []episodeMemoryCandidate{{
			LessonKey: "screen_dimensions_after_error", Type: "fact", Action: "create", Retention: episodeMemoryRetentionDurable,
			Situation: "When operating device A", Guidance: "Use a 1080x1920 screen model", ExpectedEffect: "Coordinates use the observed dimensions",
			Scope: map[string]string{"device_id": "device_a", "screen": "1080x1920"}, Tags: []string{"screen", "dimensions"}, EvidenceRefs: []string{"ep_resume_after_error_result"},
		}},
	}
	if err := processor.state.SetEpisode(resumedEpisode.ID, episodeMemoryEpisodeStatus{
		Status: episodeMemoryStatusProposed, ExtractorVersion: episodeMemoryExtractorVersion, Proposal: &proposal,
	}); err != nil {
		t.Fatalf("SetEpisode(proposed) error = %v", err)
	}

	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want only the failing extraction call", got)
	}
	items, err := plane.device.readAll()
	if err != nil {
		t.Fatalf("readAll() error = %v", err)
	}
	if len(items) != 1 || items[0].LessonKey != "screen_dimensions_after_error" {
		t.Fatalf("memories = %#v, want persisted proposal applied after prior error", items)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if status := state.Episodes[episodeMemoryStateKey(failedEpisode.ID, episodeMemoryExtractorVersion)]; status.Status != episodeMemoryStatusIgnored {
		t.Fatalf("failed extraction status = %#v, want ignored", status)
	}
	if status := state.Episodes[episodeMemoryStateKey(resumedEpisode.ID, episodeMemoryExtractorVersion)]; status.Status != episodeMemoryStatusDone {
		t.Fatalf("persisted proposal status = %#v, want done", status)
	}
}

func TestEpisodeMemoryProcessorUsesGoalResultToRejectFalseSuccessProcedure(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"not_achieved","reason":"The user said the requested message was not sent.","evidence_refs":["ep_false_success_steer"]},
  "candidates":[
    {
      "lesson_key":"send_message_path","type":"procedure","action":"create","unresolved_conflict":false,
      "situation":"When sending a message","guidance":"Tap Send after entering text","expected_effect":"The requested message is sent",
      "scope":{"device_id":"device_a","app_name":"Messages","goal_pattern":"send message"},"tags":["messages"],
      "evidence_refs":["ep_false_success_call","ep_false_success_result"]
    },
    {
      "lesson_key":"verify_message_sent","type":"failure","action":"create","unresolved_conflict":false,
      "situation":"After attempting to send a message","guidance":"Verify the requested message appears as sent before reporting completion","expected_effect":"The agent does not claim success when the message was not sent",
      "scope":{"device_id":"device_a","app_name":"Messages"},"tags":["messages","verify-before-finish"],
      "evidence_refs":["ep_false_success_result","ep_false_success_steer"]
    }
  ]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_false_success", Status: "active", StartedAt: "2026-08-14T05:00:00Z", EndedAt: "2026-08-14T05:00:05Z",
		UserGoal: "发送消息", DeviceScope: map[string]string{"device_id": "device_a"},
		Outcome: TaskEpisodeOutcome{Success: true, FinalAnswer: "已发送"},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_false_success_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":900,"y":900}}`},
			{EventID: "ep_false_success_result", Type: "tool_result", ToolName: "touch_gesture", Content: "tap completed"},
			{EventID: "ep_false_success_steer", Type: "steer", Content: "并没有发送出去"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	items, err := plane.device.readAll()
	if err != nil {
		t.Fatalf("readAll() error = %v", err)
	}
	if len(items) != 1 || items[0].Type != "failure" || items[0].LessonKey != "verify_message_sent" {
		t.Fatalf("memories = %#v, want only reusable failure guard", items)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if assessment := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)].Assessment; assessment == nil || assessment.GoalResult != "not_achieved" {
		t.Fatalf("persisted assessment = %#v, want not_achieved audit result", assessment)
	}
}

func TestEpisodeMemoryProcessorAcceptsNonCanceledStructuredErrorWithoutPairedDeviceCall(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"not_achieved","reason":"The tool returned a structured execution error.","evidence_refs":["ep_error_result"]},
  "candidates":[]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for _, episode := range []TaskEpisode{
		{
			ID: "ep_error", Status: "active", StartedAt: "2026-08-14T06:00:00Z", EndedAt: "2026-08-14T06:00:01Z", UserGoal: "打开应用",
			Events: []TaskEpisodeEvent{{EventID: "ep_error_result", Type: "tool_result", IsError: true, ToolError: NewToolError(CodeToolExecutionFailed, "launch failed")}},
		},
		{
			ID: "ep_canceled_only", Status: "active", StartedAt: "2026-08-14T06:00:02Z", EndedAt: "2026-08-14T06:00:03Z", UserGoal: "打开应用",
			Events: []TaskEpisodeEvent{{EventID: "ep_canceled_result", Type: "tool_result", IsError: true, ToolError: NewToolError(CodeCanceled, "canceled")}},
		},
	} {
		if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episode.ID, err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want only the non-canceled structured error", got)
	}
}

func TestEpisodeMemoryModelInputUsesDirectEvidenceWithoutVerifierState(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"unknown","reason":"The final visible state is insufficient.","evidence_refs":[]},
  "candidates":[]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	episode := TaskEpisode{
		ID: "ep_direct_evidence", Status: "active", StartedAt: "2026-08-14T07:00:00Z", EndedAt: "2026-08-14T07:00:01Z", UserGoal: "检查当前页面",
		Outcome: TaskEpisodeOutcome{Success: true, VerifierReason: "SECRET_VERIFIER_REASON"},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_direct_call", Type: runEventToolCall, ToolName: "screenshot"},
			{
				EventID: "ep_direct_result", Type: "tool_result", ToolName: "screenshot",
				ObservedState:  &observedWorldState{AppName: "SECRET_OBSERVED_APP", PageName: "SECRET_OBSERVED_PAGE"},
				RawObservation: `{"width":10,"height":10,"format":"jpeg","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg")) + `"}`,
			},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	stored, err := plane.episodes.Get(ctx, episode.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, errs, err := processor.proposeEpisodeBatch(ctx, []TaskEpisode{stored}, 0); err != nil {
		t.Fatalf("proposeEpisodeBatch() error = %v", err)
	} else if errs[stored.ID] != nil {
		t.Fatalf("proposeEpisodeBatch() proposal error = %v", errs[stored.ID])
	}
	prompt := model.firstCallText()
	for _, forbidden := range []string{"SECRET_VERIFIER_REASON", "SECRET_OBSERVED_APP", "SECRET_OBSERVED_PAGE", "verifier_reason", "observed_state"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("Episode Memory model input contains %q:\n%s", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, "For action=create, omit memory_id and memory_revision.") {
		t.Fatal("Episode Memory model input does not distinguish create fields from update fields")
	}
	if strings.Contains(prompt, `"memory_revision": 1`) {
		t.Fatal("Episode Memory create schema still presents memory_revision as a required field")
	}
	if !strings.Contains(prompt, "must emit at least one candidate") {
		t.Fatal("Episode Memory model input does not require retention of directly verified reusable lessons")
	}
	if !strings.Contains(prompt, episodeMemoryBuiltInGuidanceRule) {
		t.Fatal("Episode Memory model input does not exclude guidance the tools already give the Agent")
	}
	if !model.firstCallHasImage() {
		t.Fatal("Episode Memory model input did not attach the persisted screenshot")
	}
}

func TestEpisodeMemoryWorkerCancelsBackgroundModelWhenForegroundTaskStarts(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	inner := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"unknown","reason":"No final visible proof was recorded.","evidence_refs":[]},
  "candidates":[]
}`}}
	model := &episodeMemoryBlockingModel{inner: inner, started: make(chan struct{}), release: make(chan struct{})}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID: "ep_preempt", Status: "active", StartedAt: "2026-08-14T08:00:00Z", EndedAt: "2026-08-14T08:00:01Z", UserGoal: "截图",
		Events: []TaskEpisodeEvent{
			{EventID: "ep_preempt_call", Type: runEventToolCall, ToolName: "screenshot"},
			{EventID: "ep_preempt_result", Type: "tool_result", ToolName: "screenshot", Observation: "screen captured"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	worker := newMemoryWorker(processor, defaultMemoryWorkerIdleDelay)
	worker.idleDelay = 10 * time.Millisecond
	if err := worker.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer worker.Stop()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("Episode Memory model call did not start")
	}
	worker.TaskStarted()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, err := processor.state.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot() error = %v", err)
		}
		status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]
		if status.Status != episodeMemoryStatusProcessing {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls := inner.callCount(); calls != 0 {
		t.Fatalf("completed model calls while foreground task started = %d, want 0", calls)
	}
	worker.TaskFinished()
	deadline = time.Now().Add(time.Second)
	for inner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := inner.callCount(); calls != 1 {
		t.Fatalf("model calls after foreground task finished = %d, want resumed Episode once", calls)
	}
}

func TestEpisodeMemoryProcessorRequeuesProposalWhenMemoryRevisionChanged(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	if _, err := plane.device.Upsert(ctx, DeviceMemoryItem{
		ID: "devmem_revision", Type: "fact", Status: "active", Revision: 2, LessonKey: "existing",
		Title: "Settings location", Summary: "Open Display", Content: "Display is in Settings", DeviceID: "device_a",
		Tags: []string{episodeMemoryTag, "settings"}, Applicability: map[string]string{"device_id": "device_a", "app_name": "Settings"},
	}); err != nil {
		t.Fatalf("Upsert(existing) error = %v", err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The tool result confirms the current location.","evidence_refs":["ep_revision_result"]},
  "candidates":[{
    "lesson_key":"refresh_revision","type":"fact","action":"update","memory_id":"devmem_revision","memory_revision":2,
    "unresolved_conflict":false,"situation":"In Settings on device A","guidance":"Open Display from the main list","expected_effect":"Display controls become visible",
    "scope":{"device_id":"device_a","app_name":"Settings"},"tags":["settings"],"evidence_refs":["ep_revision_result"]
  }]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_revision", Status: "active", StartedAt: "2026-08-14T09:00:00Z", EndedAt: "2026-08-14T09:00:01Z", UserGoal: "打开设置显示",
		DeviceScope: map[string]string{"device_id": "device_a"}, RetrievedMemoryRefs: []string{"devmem_revision"}, Outcome: TaskEpisodeOutcome{Success: true},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_revision_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":500,"y":500}}`},
			{EventID: "ep_revision_result", Type: "tool_result", ToolName: "touch_gesture", Content: "Display controls visible"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	stale := episodeMemoryProposal{
		EpisodeAssessment: episodeMemoryAssessment{GoalResult: "achieved", Reason: "stale proposal", EvidenceRefs: []string{"ep_revision_result"}},
		ExistingRevisions: map[string]int{"devmem_revision": 1},
		Candidates: []episodeMemoryCandidate{{
			LessonKey: "stale_revision", Type: "fact", Action: "update", Retention: episodeMemoryRetentionDurable, MemoryID: "devmem_revision", MemoryRevision: 1,
			Situation: "In Settings", Guidance: "Open Display", ExpectedEffect: "Display opens",
			Scope: map[string]string{"device_id": "device_a", "app_name": "Settings"}, EvidenceRefs: []string{"ep_revision_result"},
		}},
	}
	if err := processor.state.SetEpisode(episode.ID, episodeMemoryEpisodeStatus{
		Status: episodeMemoryStatusProposed, ExtractorVersion: episodeMemoryExtractorVersion, Proposal: &stale,
	}); err != nil {
		t.Fatalf("SetEpisode(stale proposed) error = %v", err)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch(stale) error = %v", err)
	}
	if got := model.callCount(); got != 0 {
		t.Fatalf("model calls for stale persisted proposal = %d, want 0 before requeue", got)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]; status.Status != episodeMemoryStatusRetry || status.Proposal != nil {
		t.Fatalf("stale proposal state = %#v, want retry without old proposal", status)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch(requeued) error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls after requeue = %d, want 1 fresh extraction", got)
	}
	updated, found, err := plane.device.Get(ctx, "devmem_revision")
	if err != nil || !found || updated.Revision != 3 {
		t.Fatalf("updated memory found=%v error=%v item=%#v, want revision 3", found, err, updated)
	}
}

func TestMemoryPlaneNotifiesEpisodeMemoryWorkerForSuccessfulEpisode(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The app-open result confirms the goal.","evidence_refs":["ep_notify_result"]},
  "candidates":[]
}`}}
	if err := plane.StartEpisodeMemory(model); err != nil {
		t.Fatalf("StartEpisodeMemory() error = %v", err)
	}
	defer plane.StopEpisodeMemory()
	plane.memoryWorkerMu.RLock()
	worker := plane.memoryWorker
	plane.memoryWorkerMu.RUnlock()
	worker.mu.Lock()
	worker.idleDelay = 10 * time.Millisecond
	worker.mu.Unlock()
	now := time.Now().UTC()
	if err := plane.CommitEpisode(ctx, TaskEpisode{
		ID: "ep_notify", Status: "active", StartedAt: now.Format(time.RFC3339Nano), EndedAt: now.Add(time.Millisecond).Format(time.RFC3339Nano),
		UserGoal: "打开设置", Outcome: TaskEpisodeOutcome{Success: true},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_notify_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
			{EventID: "ep_notify_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
		},
	}); err != nil {
		t.Fatalf("CommitEpisode() error = %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for model.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want successful Episode notification to trigger extraction", got)
	}
}

func TestEpisodeMemoryAssessmentRejectsIndirectEvidence(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The action was attempted.","evidence_refs":["ep_indirect_call"]},
  "candidates":[]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	episode := TaskEpisode{
		ID: "ep_indirect", Status: "active", StartedAt: "2026-08-14T10:00:00Z", EndedAt: "2026-08-14T10:00:01Z", UserGoal: "打开设置",
		Events: []TaskEpisodeEvent{
			{EventID: "ep_indirect_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
			{EventID: "ep_indirect_result", Type: "tool_result", ToolName: "launch_app", Content: "request accepted"},
		},
	}
	if _, errs, err := processor.proposeEpisodeBatch(ctx, []TaskEpisode{episode}, 0); err != nil || errs[episode.ID] == nil || !strings.Contains(errs[episode.ID].Error(), "requires direct evidence") {
		t.Fatalf("proposeEpisodeBatch() error = %v proposal error = %v, want direct-evidence rejection", err, errs[episode.ID])
	}
}

func TestFailureCandidateMustReferenceNotAchievedEvidence(t *testing.T) {
	episode := TaskEpisode{Events: []TaskEpisodeEvent{
		{EventID: "call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
		{EventID: "result", Type: "tool_result", ToolName: "launch_app", Content: "The requested page did not open"},
	}}
	assessment := episodeMemoryAssessment{
		GoalResult:   episodeGoalNotAchieved,
		Reason:       "The result shows the requested page did not open.",
		EvidenceRefs: []string{"result"},
	}
	candidate := episodeMemoryCandidate{
		LessonKey:      "check_page_opened",
		Type:           episodeMemoryTypeFailure,
		Action:         episodeMemoryActionCreate,
		Retention:      episodeMemoryRetentionDurable,
		Situation:      "After launching Settings",
		Guidance:       "Check that the requested page is visible before continuing",
		ExpectedEffect: "The agent stops when the requested page did not open",
		Scope:          map[string]string{"app_name": "Settings"},
		EvidenceRefs:   []string{"call"},
	}

	if _, ok := validateEpisodeMemoryCandidate(episode, assessment, candidate, map[string]bool{}); ok {
		t.Fatal("failure Candidate citing only an action was accepted")
	}
	candidate.EvidenceRefs = []string{"call", "result"}
	if _, ok := validateEpisodeMemoryCandidate(episode, assessment, candidate, map[string]bool{}); !ok {
		t.Fatal("failure Candidate citing the not-achieved result was rejected")
	}
}

func TestEpisodeMemoryCandidateUsesModelConfidenceWithExplicitDefault(t *testing.T) {
	episode := TaskEpisode{Events: []TaskEpisodeEvent{{
		EventID: "result", Type: "tool_result", ToolName: "touch_gesture", Observation: "Display controls are visible",
	}}}
	candidate := episodeMemoryCandidate{
		LessonKey: "settings_display", Type: episodeMemoryTypeFact, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "In Settings", Guidance: "Open Display",
		ExpectedEffect: "Display controls are visible", Scope: map[string]string{"app_name": "Settings"},
		EvidenceRefs: []string{"result"},
	}
	validated, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{})
	if !ok || validated.Confidence == nil || *validated.Confidence != 0.7 {
		t.Fatalf("validated=%#v ok=%v, want omitted confidence default 0.7", validated, ok)
	}
	candidate.Confidence = episodeMemoryConfidencePointer(0.88)
	validated, ok = validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{})
	if !ok || validated.Confidence == nil || *validated.Confidence != 0.88 {
		t.Fatalf("validated=%#v ok=%v, want model confidence 0.88", validated, ok)
	}
	candidate.Confidence = episodeMemoryConfidencePointer(1.01)
	if _, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{}); ok {
		t.Fatal("candidate with confidence above 1 was accepted")
	}
	candidate.Confidence = episodeMemoryConfidencePointer(0)
	if _, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{}); ok {
		t.Fatal("candidate with explicit zero confidence was accepted")
	}
	var decoded episodeMemoryCandidate
	if err := json.Unmarshal([]byte(`{"confidence":0}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Confidence == nil || *decoded.Confidence != 0 {
		t.Fatalf("decoded confidence=%v, want explicit zero preserved", decoded.Confidence)
	}
	if err := json.Unmarshal([]byte(`{"confidence":null}`), &decoded); err == nil {
		t.Fatal("explicit null confidence was accepted as omitted")
	}
}

func TestEpisodeMemoryCandidatePreservesExplicitVersionScope(t *testing.T) {
	episode := TaskEpisode{
		DeviceScope: map[string]string{
			"device_id":   "device_a",
			"app_name":    "QA Notes",
			"app_version": "7",
			"page_name":   "Note editor",
		},
		Events: []TaskEpisodeEvent{
			{EventID: "result", Type: "tool_result", ToolName: "touch_gesture", Observation: "The title was saved."},
		},
	}
	candidate := episodeMemoryCandidate{
		LessonKey:      "qa_notes_save",
		Type:           episodeMemoryTypeFact,
		Action:         episodeMemoryActionCreate,
		Retention:      episodeMemoryRetentionDurable,
		Situation:      "When saving an edited title in QA Notes",
		Guidance:       "Use the verified save flow",
		ExpectedEffect: "The title remains saved",
		Scope: map[string]string{
			"app_name":     "QA Notes",
			"precondition": "app_version=7; title edited",
		},
		EvidenceRefs: []string{"result"},
	}

	validated, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{})
	if !ok {
		t.Fatal("candidate with an omitted explicit version boundary was rejected")
	}
	for key, want := range map[string]string{"device_id": "device_a", "app_name": "QA Notes", "app_version": "7", "page_name": "Note editor"} {
		if got := validated.Scope[key]; !strings.EqualFold(got, want) {
			t.Fatalf("validated scope[%q] = %q, want %q; scope=%#v", key, got, want, validated.Scope)
		}
	}
}

func TestEpisodeMemoryCandidateRejectsConflictingVersionScope(t *testing.T) {
	episode := TaskEpisode{
		DeviceScope: map[string]string{"app_name": "QA Notes", "app_version": "7"},
		Events: []TaskEpisodeEvent{
			{EventID: "result", Type: "tool_result", ToolName: "touch_gesture", Observation: "The title was saved."},
		},
	}
	candidate := episodeMemoryCandidate{
		LessonKey:      "qa_notes_save_wrong_version",
		Type:           episodeMemoryTypeFact,
		Action:         episodeMemoryActionCreate,
		Retention:      episodeMemoryRetentionDurable,
		Situation:      "When saving an edited title in QA Notes",
		Guidance:       "Use the verified save flow",
		ExpectedEffect: "The title remains saved",
		Scope:          map[string]string{"app_name": "QA Notes", "app_version": "8"},
		EvidenceRefs:   []string{"result"},
	}
	if _, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{}); ok {
		t.Fatal("candidate with a conflicting app_version was accepted")
	}
}

func TestRetainedEpisodeMemoryCandidateUsesRewrittenScope(t *testing.T) {
	original := []episodeMemoryCandidate{{
		LessonKey: "qa_notes_save", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "old", Guidance: "old", ExpectedEffect: "old",
		Scope: map[string]string{"app_name": "QA Notes"}, EvidenceRefs: []string{"result"},
	}}
	audit := episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
		LessonKey: "qa_notes_save", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
		Reason: "verified", SensitiveValues: []string{}, Rewrite: &episodeMemoryRetentionRewrite{
			Situation: "new", Guidance: "new", ExpectedEffect: "new",
			Scope: map[string]string{"app_name": "QA Notes", "app_version": "7"}, EvidenceRefs: []string{"result"},
		},
	}}}
	retained := retainedEpisodeMemoryCandidates(original, audit)
	if len(retained) != 1 || retained[0].Scope["app_version"] != "7" {
		t.Fatalf("retained candidate scope = %#v, want rewritten app_version=7", retained)
	}
}

func TestRetainedEpisodeMemoryCandidatePreservesScopeOmittedByRewrite(t *testing.T) {
	original := []episodeMemoryCandidate{{
		LessonKey: "qa_notes_save", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "old", Guidance: "old", ExpectedEffect: "old",
		Scope: map[string]string{"app_name": "QA Notes", "app_version": "7", "goal_pattern": "persist title"}, EvidenceRefs: []string{"result"},
	}}
	audit := episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
		LessonKey: "qa_notes_save", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
		Reason: "verified", SensitiveValues: []string{}, Rewrite: &episodeMemoryRetentionRewrite{
			Situation: "new", Guidance: "new", ExpectedEffect: "new",
			Scope: map[string]string{"precondition": "title edited"}, EvidenceRefs: []string{"result"},
		},
	}}}
	retained := retainedEpisodeMemoryCandidates(original, audit)
	if len(retained) != 1 {
		t.Fatalf("retained candidates = %#v, want one", retained)
	}
	for key, want := range map[string]string{"app_name": "QA Notes", "app_version": "7", "goal_pattern": "persist title", "precondition": "title edited"} {
		if got := retained[0].Scope[key]; got != want {
			t.Fatalf("retained scope[%q] = %q, want %q; scope=%#v", key, got, want, retained[0].Scope)
		}
	}
}

func TestRetainedEpisodeMemoryCandidateRejectsSensitiveValueLeftInRewrite(t *testing.T) {
	original := []episodeMemoryCandidate{{
		LessonKey: "verification_flow", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "challenge", Guidance: "enter the observed value", ExpectedEffect: "sign-in succeeds",
		Scope: map[string]string{"app_name": "Auth"}, EvidenceRefs: []string{"result"},
	}}
	audit := episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
		LessonKey: "verification_flow", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
		Reason: "the workflow is reusable", SensitiveValues: []string{"913204"}, Rewrite: &episodeMemoryRetentionRewrite{
			Situation: "During a one-time sign-in challenge", Guidance: "Enter 913204", ExpectedEffect: "The challenge completes",
			Scope: map[string]string{"app_name": "Auth"}, EvidenceRefs: []string{"result"},
		},
	}}}
	if retained := retainedEpisodeMemoryCandidates(original, audit); len(retained) != 0 {
		t.Fatalf("retained candidates = %#v, want sensitive rewrite discarded", retained)
	}
}

func TestRetainedEpisodeMemoryCandidateAcceptsGeneralizedSensitiveWorkflow(t *testing.T) {
	original := []episodeMemoryCandidate{{
		LessonKey: "verification_flow", Type: episodeMemoryTypeProcedure, Action: episodeMemoryActionCreate,
		Retention: episodeMemoryRetentionDurable, Situation: "challenge", Guidance: "enter the observed value", ExpectedEffect: "sign-in succeeds",
		Scope: map[string]string{"app_name": "Auth"}, EvidenceRefs: []string{"result"},
	}}
	audit := episodeMemoryRetentionAudit{Reviews: []episodeMemoryRetentionReview{{
		LessonKey: "verification_flow", Decision: episodeMemoryRetentionDecisionRetain, Retention: episodeMemoryRetentionDurable,
		Reason: "the generalized workflow is reusable", SensitiveValues: []string{"913204"}, Rewrite: &episodeMemoryRetentionRewrite{
			Situation: "During a one-time sign-in challenge", Guidance: "Enter the current challenge value shown for this session", ExpectedEffect: "The challenge completes",
			Scope: map[string]string{"app_name": "Auth"}, EvidenceRefs: []string{"result"},
		},
	}}}
	if retained := retainedEpisodeMemoryCandidates(original, audit); len(retained) != 1 {
		t.Fatalf("retained candidates = %#v, want generalized workflow retained", retained)
	}
}

func TestEpisodeMemoryProcedureStepsRedactAuditedSensitiveValues(t *testing.T) {
	episode := TaskEpisode{
		Entities: []string{"Auth", "913204"},
		Events: []TaskEpisodeEvent{
			{EventID: "call", Type: runEventToolCall, ToolName: "enter_text", ToolInput: `{"text":"913204"}`, Content: "Enter 913204"},
			{EventID: "result", Type: "tool_result", ToolName: "enter_text", Observation: "913204 was accepted"},
		},
	}
	steps := episodeMemoryProcedureSteps(episode, []string{"call", "result"}, []string{"913204"})
	if len(steps) != 1 {
		t.Fatalf("steps = %#v, want one", steps)
	}
	encoded, _ := json.Marshal(steps)
	if strings.Contains(string(encoded), "913204") {
		t.Fatalf("steps retained audited sensitive value: %s", encoded)
	}
	entities := redactEpisodeMemorySensitiveStrings(episode.Entities, []string{"913204"})
	if encoded, _ := json.Marshal(entities); strings.Contains(string(encoded), "913204") {
		t.Fatalf("entities retained audited sensitive value: %s", encoded)
	}
}

func TestNavigationCandidateLinksResultEvidenceToToolCalls(t *testing.T) {
	episode := TaskEpisode{Events: []TaskEpisodeEvent{
		{EventID: "ethernet_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","target":"Ethernet"}`},
		{EventID: "ethernet_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "The Ethernet page is visible."},
		{EventID: "interface_call", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","target":"Aico HID+ECM"}`},
		{EventID: "interface_result", Type: "tool_result", ToolName: "touch_gesture", Observation: "The IPv4 details page is visible."},
	}}
	candidate := episodeMemoryCandidate{
		LessonKey:      "ios_aico_ethernet_path",
		Type:           episodeMemoryTypeNavigation,
		Action:         episodeMemoryActionCreate,
		Retention:      episodeMemoryRetentionDurable,
		MemoryRevision: 1,
		Situation:      "When opening the Aico USB Ethernet details on iOS",
		Guidance:       "Open Ethernet, then Aico HID+ECM",
		ExpectedEffect: "The IPv4 details page is visible",
		Scope:          map[string]string{"app_name": "Settings"},
		EvidenceRefs:   []string{"ethernet_result", "interface_result"},
	}

	validated, ok := validateEpisodeMemoryCandidate(episode, episodeMemoryAssessment{GoalResult: episodeGoalAchieved}, candidate, map[string]bool{})
	if !ok {
		t.Fatal("navigation Candidate citing paired tool results was rejected")
	}
	if validated.MemoryRevision != 0 {
		t.Fatalf("create memory_revision = %d, want 0", validated.MemoryRevision)
	}
	for _, want := range []string{"ethernet_result", "interface_result", "ethernet_call", "interface_call"} {
		found := false
		for _, ref := range validated.EvidenceRefs {
			if ref == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("evidence refs = %#v, missing %q", validated.EvidenceRefs, want)
		}
	}
}

func TestEpisodeMemoryProcessorProcessesInterruptedEpisodeWithDeviceEvidence(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"unknown","reason":"The task was interrupted after a device action, before final proof.","evidence_refs":[]},
  "candidates":[]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for _, episode := range []TaskEpisode{
		{
			ID: "ep_interrupted_after_action", Status: "interrupted", StartedAt: "2026-08-14T10:10:00Z", EndedAt: "2026-08-14T10:10:01Z", UserGoal: "打开设置",
			Events: []TaskEpisodeEvent{
				{EventID: "ep_interrupted_call", Type: runEventToolCall, ToolName: "launch_app", ToolInput: `{"app":"Settings"}`},
				{EventID: "ep_interrupted_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
			},
		},
		{
			ID: "ep_interrupted_before_action", Status: "interrupted", StartedAt: "2026-08-14T10:10:02Z", EndedAt: "2026-08-14T10:10:03Z", UserGoal: "打开设置",
			Events: []TaskEpisodeEvent{{EventID: "ep_interrupted_cancel", Type: "tool_result", IsError: true, ToolError: NewToolError(CodeCanceled, "canceled")}},
		},
	} {
		if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episode.ID, err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want only interrupted Episode with effective device evidence", got)
	}
}

func TestEpisodeMemoryExtractionFailureIsNotRetried(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{
		`not-json`,
		`{"episode_assessment":{"goal_result":"unknown","reason":"unused","evidence_refs":[]},"candidates":[]}`,
	}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_bad_proposal", Status: "active", StartedAt: "2026-08-14T10:20:00Z", EndedAt: "2026-08-14T10:20:01Z", UserGoal: "打开设置",
		Events: []TaskEpisodeEvent{
			{EventID: "ep_bad_call", Type: runEventToolCall, ToolName: "launch_app"},
			{EventID: "ep_bad_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	for pass := 0; pass < 2; pass++ {
		if _, err := processor.ProcessBatch(ctx, nil); err != nil {
			t.Fatalf("ProcessBatch(%d) error = %v", pass, err)
		}
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one extraction attempt", got)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]
	if status.Status != episodeMemoryStatusIgnored || status.AttemptCount != 1 {
		t.Fatalf("state = %#v, want one terminal ignored attempt", status)
	}
}

// Truncation is the one extraction failure that is retried: unlike malformed
// output, it is caused by the output budget rather than by the model finishing
// with garbage, so a retry with more headroom can succeed. Board logs showed
// truncated batches being discarded on the first attempt.
func TestEpisodeMemoryTruncatedBatchIsRetriedWithLargerBudget(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	partial := `{"results":[{"episode_id":"ep_truncated","proposal":{"candidates":[{"lesson_key":"k`
	model := &episodeMemoryScriptedModel{responses: []string{partial}, stopReason: "length"}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_truncated", Status: "active", StartedAt: "2026-08-14T10:30:00Z", EndedAt: "2026-08-14T10:30:01Z", UserGoal: "打开设置",
		Events: []TaskEpisodeEvent{
			{EventID: "ep_trunc_call", Type: runEventToolCall, ToolName: "launch_app"},
			{EventID: "ep_trunc_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	firstPass := time.Date(2026, 8, 14, 11, 0, 0, 0, time.UTC)
	processor.now = func() time.Time { return firstPass }
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	stateKey := episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if status := state.Episodes[stateKey]; status.Status != episodeMemoryStatusRetry || status.AttemptCount != 1 {
		t.Fatalf("state = %#v, want a scheduled retry after truncation", status)
	}

	// Let the next response finish normally, past the retry delay.
	model.setStopReason("")
	model.appendResponses(`{"episode_assessment":{"goal_result":"unknown","reason":"no durable lesson","evidence_refs":[]},"candidates":[]}`)
	processor.now = func() time.Time { return firstPass.Add(episodeMemoryRetryDelay + time.Minute) }
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() second pass error = %v", err)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls = %d, want a second extraction attempt", got)
	}
	// The retry must ask for more room, or it would replay the same failure.
	first, second := model.batchMaxTokens(0), model.batchMaxTokens(1)
	if first != episodeMemoryBatchTokensPerEpisode {
		t.Fatalf("first attempt max_tokens = %d, want %d", first, episodeMemoryBatchTokensPerEpisode)
	}
	if second <= first {
		t.Fatalf("retry max_tokens = %d, want more than the first attempt's %d", second, first)
	}
	if state, err = processor.state.Snapshot(); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if status := state.Episodes[stateKey]; status.Status == episodeMemoryStatusIgnored {
		t.Fatalf("state = %#v, want the retry to not be discarded", status)
	}
}

func TestEpisodeMemoryTruncatedBatchRetriesEpisodesIndividually(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	partial := `{"results":[{"episode_id":"ep_split_0","proposal":{"candidates":[{"lesson_key":"k`
	model := &episodeMemoryScriptedModel{responses: []string{partial}, stopReason: "length"}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	for index := 0; index < episodeMemoryBatchLimit; index++ {
		endedAt := time.Date(2026, 8, 14, 12, 0, index+1, 0, time.UTC)
		episodeID := fmt.Sprintf("ep_split_%d", index)
		if _, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
			ID: episodeID, Status: "active", StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano),
			EndedAt: endedAt.Format(time.RFC3339Nano), UserGoal: "打开设置",
			Events: []TaskEpisodeEvent{
				{EventID: episodeID + "_call", Type: runEventToolCall, ToolName: "launch_app"},
				{EventID: episodeID + "_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
			},
		}); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episodeID, err)
		}
	}
	firstPass := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	processor.now = func() time.Time { return firstPass }
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch(first) error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("first-pass model calls = %d, want one full-batch call", got)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() after first pass: %v", err)
	}
	for index := 0; index < episodeMemoryBatchLimit; index++ {
		key := episodeMemoryStateKey(fmt.Sprintf("ep_split_%d", index), episodeMemoryExtractorVersion)
		status := state.Episodes[key]
		if status.Status != episodeMemoryStatusRetry || status.RetryBatchLimit != 1 {
			t.Fatalf("status[%s] = %#v, want retry_batch_limit=1", key, status)
		}
	}

	model.setStopReason("")
	for index := 0; index < episodeMemoryBatchLimit; index++ {
		model.appendResponses(`{"episode_assessment":{"goal_result":"unknown","reason":"no durable lesson","evidence_refs":[]},"candidates":[]}`)
	}
	for pass := 0; pass < episodeMemoryBatchLimit; pass++ {
		processor.now = func() time.Time {
			return firstPass.Add(time.Duration(pass+1) * (episodeMemoryRetryDelay + time.Minute))
		}
		result, err := processor.ProcessBatch(ctx, nil)
		if err != nil {
			t.Fatalf("ProcessBatch(retry %d) error = %v", pass+1, err)
		}
		if pass < episodeMemoryBatchLimit-1 && !result.HasPending {
			t.Fatalf("retry %d result = %#v, want pending episodes", pass+1, result)
		}
	}
	if got := model.callCount(); got != 1+episodeMemoryBatchLimit {
		t.Fatalf("total model calls = %d, want one batch plus %d single-episode retries", got, episodeMemoryBatchLimit)
	}
	if first, second := model.batchMaxTokens(0), model.batchMaxTokens(1); first != episodeMemoryBatchTokensPerEpisode*episodeMemoryBatchLimit || second <= episodeMemoryBatchTokensPerEpisode {
		t.Fatalf("batch budgets = first %d, first retry %d; want %d then a larger single-episode retry", first, second, episodeMemoryBatchTokensPerEpisode*episodeMemoryBatchLimit)
	}
	for index := 1; index <= episodeMemoryBatchLimit; index++ {
		if got := model.batchMaxTokens(index); got != memoryMergeTokenBudget(episodeMemoryBatchTokensPerEpisode, 1, episodeMemoryBatchMaxTokens, 1) {
			t.Fatalf("single retry %d max_tokens = %d, want %d", index, got, memoryMergeTokenBudget(episodeMemoryBatchTokensPerEpisode, 1, episodeMemoryBatchMaxTokens, 1))
		}
	}
}

func TestEpisodeMemoryOmissionReviewTruncationUsesLargerRetryBudget(t *testing.T) {
	partial := `{"episode_assessment":{"goal_result":"achieved","reason":"partial`
	model := &episodeMemoryScriptedModel{responses: []string{partial, partial}, stopReason: "length"}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	processor := newEpisodeMemoryProcessor(plane, model)
	episode := TaskEpisode{ID: "ep_omission_truncated"}
	parts := []llms.ContentPart{llms.TextPart("Review this episode for an omitted memory.")}

	for attempt := 0; attempt < 2; attempt++ {
		_, err := processor.generateEpisodeMemoryProposal(context.Background(), episode, nil, parts, attempt)
		if !errors.Is(err, errMemoryMergeTruncated) {
			t.Fatalf("attempt %d error = %v, want errMemoryMergeTruncated", attempt, err)
		}
	}
	first, second := model.batchMaxTokens(0), model.batchMaxTokens(1)
	if first != episodeMemoryBatchTokenBudget(1, 0) || second != episodeMemoryBatchTokenBudget(1, 1) || second <= first {
		t.Fatalf("omission review budgets = (%d, %d), want (%d, %d)", first, second, episodeMemoryBatchTokenBudget(1, 0), episodeMemoryBatchTokenBudget(1, 1))
	}
}

func TestEpisodeMemoryRetentionAuditTruncationUsesLargerRetryBudget(t *testing.T) {
	partial := `{"reviews":[{"lesson_key":"safe_path","decision":"retain"`
	model := &episodeMemoryScriptedModel{auditResponses: []string{partial, partial}, stopReason: "max_tokens"}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	processor := newEpisodeMemoryProcessor(plane, model)
	episode := TaskEpisode{ID: "ep_audit_truncated"}
	proposal := episodeMemoryProposal{Candidates: []episodeMemoryCandidate{{LessonKey: "safe_path"}}}

	for attempt := 0; attempt < 2; attempt++ {
		_, err := processor.generateEpisodeMemoryRetentionAudit(context.Background(), episode, proposal, nil, attempt)
		if !errors.Is(err, errMemoryMergeTruncated) {
			t.Fatalf("attempt %d error = %v, want errMemoryMergeTruncated", attempt, err)
		}
	}
	first, second := model.auditMaxTokens(0), model.auditMaxTokens(1)
	if first != episodeMemoryRetentionAuditTokenBudget(0) || second != episodeMemoryRetentionAuditTokenBudget(1) || second <= first {
		t.Fatalf("retention audit budgets = (%d, %d), want (%d, %d)", first, second, episodeMemoryRetentionAuditTokenBudget(0), episodeMemoryRetentionAuditTokenBudget(1))
	}
}

func TestEpisodeMemoryFailureLogDoesNotPersistResponseBody(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{logger: log.New(&output, "", 0)}
	processor := &episodeMemoryProcessor{plane: &FilesystemMemoryPlane{logger: logger}}
	raw := `{"sensitive_values":["otp-123456"],"说明":"不要记录"}`

	processor.logEpisodeMemoryResponseFailure("retention audit truncated", raw)

	logged := output.String()
	if strings.Contains(logged, "otp-123456") || strings.Contains(logged, "不要记录") {
		t.Fatalf("failure log persisted model response body: %q", logged)
	}
	for _, want := range []string{
		fmt.Sprintf("response_bytes=%d", len(raw)),
		fmt.Sprintf("response_runes=%d", len([]rune(raw))),
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("failure log = %q, want %q", logged, want)
		}
	}
}

func TestEpisodeMemoryErrorPersistenceSanitizesProviderText(t *testing.T) {
	var output bytes.Buffer
	logger := &Logger{logger: log.New(&output, "", 0)}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), logger)
	processor := newEpisodeMemoryProcessor(plane, &episodeMemoryScriptedModel{})
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	endedAt := time.Date(2026, 8, 14, 10, 0, 1, 0, time.UTC)
	episode := TaskEpisode{ID: "ep_sanitized_error", Status: "active", StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano), EndedAt: endedAt.Format(time.RFC3339Nano)}
	work := &episodeMemoryWork{
		episode:        episode,
		originalStatus: episodeMemoryEpisodeStatus{},
		status:         episodeMemoryEpisodeStatus{},
	}
	result := &MemoryBatchResult{}
	providerErr := fmt.Errorf("provider response contains otp-123456")
	cause := fmt.Errorf("%w: %w", errEpisodeMemoryRetentionAudit, providerErr)
	if err := processor.retryEpisodeMemoryWork(&episodeMemoryStateFile{Episodes: map[string]episodeMemoryEpisodeStatus{}}, work, cause, result); err != nil {
		t.Fatalf("retryEpisodeMemoryWork() error = %v", err)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]
	if status.LastError != errEpisodeMemoryRetentionAudit.Error() {
		t.Fatalf("LastError = %q, want stable retention-audit error", status.LastError)
	}
	if strings.Contains(status.LastError, "otp-123456") || strings.Contains(output.String(), "otp-123456") {
		t.Fatalf("provider error text was persisted: state=%q log=%q", status.LastError, output.String())
	}

	truncatedCause := fmt.Errorf("%w: %w", errEpisodeMemoryRetentionAudit, fmt.Errorf("%w: provider response contains otp-654321", errMemoryMergeTruncated))
	safeErr := safeEpisodeMemoryError(truncatedCause)
	if !errors.Is(safeErr, errEpisodeMemoryRetentionAudit) || !errors.Is(safeErr, errMemoryMergeTruncated) {
		t.Fatalf("safe error = %v, want retention and truncation sentinels", safeErr)
	}
	if strings.Contains(safeErr.Error(), "otp-654321") {
		t.Fatalf("safe error retained provider text: %q", safeErr)
	}
}

func TestEpisodeMemoryProposalRetryStopsAtMaximumAttempts(t *testing.T) {
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	processor := newEpisodeMemoryProcessor(plane, &episodeMemoryScriptedModel{})
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	endedAt := time.Date(2026, 8, 14, 10, 0, 1, 0, time.UTC)
	episode := TaskEpisode{ID: "ep_retry_limit", Status: "active", StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano), EndedAt: endedAt.Format(time.RFC3339Nano), UserGoal: "retry a proposal"}
	work := &episodeMemoryWork{
		episode:        episode,
		originalStatus: episodeMemoryEpisodeStatus{AttemptCount: episodeMemoryMaxAttempts - 1},
		status:         episodeMemoryEpisodeStatus{AttemptCount: episodeMemoryMaxAttempts - 1},
	}
	result := &MemoryBatchResult{}
	if err := processor.retryEpisodeMemoryWork(&episodeMemoryStateFile{Episodes: map[string]episodeMemoryEpisodeStatus{}}, work, errors.New("retention audit failed"), result); err != nil {
		t.Fatalf("retryEpisodeMemoryWork() error = %v", err)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	status := state.Episodes[episodeMemoryStateKey(episode.ID, episodeMemoryExtractorVersion)]
	if status.Status != episodeMemoryStatusIgnored || status.AttemptCount != episodeMemoryMaxAttempts {
		t.Fatalf("state = %#v, want terminal ignored at max attempts", status)
	}
	if result.HasPending {
		t.Fatal("retry at maximum attempts unexpectedly scheduled pending work")
	}
}

func TestEpisodeMemoryProcedureUpdatePreservesExistingSteps(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	oldSteps := []ProcedureStep{{Tool: "launch_app", Text: "Settings", OutcomeNote: "Settings opened"}}
	if _, err := plane.device.Upsert(ctx, DeviceMemoryItem{
		ID: "devmem_procedure", Type: "procedure", Status: deviceMemoryStatusActive, Revision: 1,
		Title: "Open display settings", Summary: "Open Settings", Content: "old merged procedure", DeviceID: "device_a",
		Tags: []string{episodeMemoryTag}, Applicability: map[string]string{"device_id": "device_a", "app_name": "Settings"}, Steps: oldSteps,
	}); err != nil {
		t.Fatalf("Upsert(existing) error = %v", err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The final tool result shows the Display controls.","evidence_refs":["ep_proc_result_2"]},
  "candidates":[{
    "lesson_key":"merge_display_path","type":"procedure","action":"update","memory_id":"devmem_procedure","memory_revision":1,
    "unresolved_conflict":false,"situation":"When opening display settings","guidance":"Open Settings and select Display","expected_effect":"Display controls are visible",
    "scope":{"device_id":"device_a","app_name":"Settings"},"tags":["display"],
    "evidence_refs":["ep_proc_call_1","ep_proc_result_1","ep_proc_call_2","ep_proc_result_2"]
  }]
}`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	episode := TaskEpisode{
		ID: "ep_proc_update", Status: "active", StartedAt: "2026-08-14T10:30:00Z", EndedAt: "2026-08-14T10:30:05Z", UserGoal: "打开显示设置",
		DeviceScope: map[string]string{"device_id": "device_a"}, RetrievedMemoryRefs: []string{"devmem_procedure"},
		Events: []TaskEpisodeEvent{
			{EventID: "ep_proc_call_1", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":100,"y":200}}`},
			{EventID: "ep_proc_result_1", Type: "tool_result", ToolName: "touch_gesture", Content: "Settings list visible"},
			{EventID: "ep_proc_call_2", Type: runEventToolCall, ToolName: "touch_gesture", ToolInput: `{"type":"tap","point":{"x":100,"y":400}}`},
			{EventID: "ep_proc_result_2", Type: "tool_result", ToolName: "touch_gesture", Content: "Display controls visible"},
		},
	}
	if _, err := plane.episodes.AddEpisode(ctx, episode); err != nil {
		t.Fatalf("AddEpisode() error = %v", err)
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	updated, found, err := plane.device.Get(ctx, "devmem_procedure")
	if err != nil || !found {
		t.Fatalf("Get(updated) found=%v error=%v", found, err)
	}
	if len(updated.Steps) != 3 || updated.Steps[0] != oldSteps[0] {
		t.Fatalf("updated steps = %#v, want old step plus two new evidenced steps", updated.Steps)
	}
	if len(updated.RevisionHistory) != 1 || len(updated.RevisionHistory[0].Steps) != 1 {
		t.Fatalf("revision history = %#v, want prior step snapshot", updated.RevisionHistory)
	}
}

func TestSearchEpisodeMemoryCandidatesPrioritizesPreferredAndSameScope(t *testing.T) {
	ctx := context.Background()
	store := NewDeviceMemoryStore(t.TempDir())
	items := []DeviceMemoryItem{
		{ID: "preferred", Type: "fact", Status: deviceMemoryStatusActive, DeviceID: "device_a", AppName: "Other", Title: "preferred"},
		{ID: "same_active", Type: "fact", Status: deviceMemoryStatusActive, DeviceID: "device_a", AppName: "Settings", Title: "same active"},
		{ID: "same_disputed", Type: "fact", Status: deviceMemoryStatusDisputed, DeviceID: "device_a", AppName: "Settings", Title: "same disputed"},
		{ID: "other_related", Type: "fact", Status: deviceMemoryStatusActive, DeviceID: "device_a", AppName: "Other", Content: "needle"},
	}
	for _, item := range items {
		item.Applicability = map[string]string{"device_id": item.DeviceID, "app_name": item.AppName}
		if _, err := store.Upsert(ctx, item); err != nil {
			t.Fatalf("Upsert(%s) error = %v", item.ID, err)
		}
	}
	got, err := store.SearchEpisodeMemoryCandidates(ctx, EpisodeMemoryCandidateQuery{
		Terms: []string{"needle"}, PreferredIDs: []string{"preferred"}, DeviceID: "device_a",
		Scope: map[string]string{"device_id": "device_a", "app_name": "Settings"}, Limit: 4, CharBudget: 12000,
	})
	if err != nil {
		t.Fatalf("SearchEpisodeMemoryCandidates() error = %v", err)
	}
	want := []string{"preferred", "same_active", "same_disputed", "other_related"}
	if len(got) != len(want) {
		t.Fatalf("candidate IDs = %#v, want %v", got, want)
	}
	for index, id := range want {
		if got[index].ID != id {
			t.Fatalf("candidate[%d] = %s, want %s; all=%#v", index, got[index].ID, id, got)
		}
	}
}

func TestEpisodeMemoryCreatePreservesExplicitCreateAction(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	if _, err := plane.device.Upsert(ctx, DeviceMemoryItem{
		ID: "existing_scope", Type: "fact", Status: deviceMemoryStatusActive, Title: "Existing conclusion", Summary: "Use the existing rule",
		DeviceID: "device_a", AppName: "Settings", Applicability: map[string]string{"device_id": "device_a", "app_name": "Settings"},
		Tags: []string{episodeMemoryTag},
	}); err != nil {
		t.Fatalf("Upsert(existing) error = %v", err)
	}
	processor := newEpisodeMemoryProcessor(plane, &episodeMemoryScriptedModel{})
	episode := TaskEpisode{ID: "ep_collision", DeviceScope: map[string]string{"device_id": "device_a"}}
	candidate := episodeMemoryCandidate{
		LessonKey: "different_wording", Type: episodeMemoryTypeFact, Action: episodeMemoryActionCreate,
		Situation: "Different conclusion", Guidance: "Use a different rule", ExpectedEffect: "Different effect",
		Scope: map[string]string{"device_id": "device_a", "app_name": "Settings"}, EvidenceRefs: []string{"evt"}, Confidence: episodeMemoryConfidencePointer(0.86),
	}
	id, err := processor.createMemory(ctx, episode, candidate)
	if err != nil {
		t.Fatalf("createMemory() error = %v", err)
	}
	if id == "existing_scope" || !strings.HasPrefix(id, "devmem_") {
		t.Fatalf("createMemory() id = %q, want a new deterministic memory", id)
	}
	created, found, err := plane.device.Get(ctx, id)
	if err != nil || !found || created.Priority != 60 || created.Confidence != 0.86 {
		t.Fatalf("created memory=%#v found=%v err=%v, want code priority 60 and model confidence 0.86", created, found, err)
	}
	items, err := plane.device.readAll()
	if err != nil || len(items) != 2 {
		t.Fatalf("memories = %#v error=%v, want explicit create to retain both records", items, err)
	}
}

func TestLimitDeviceMemoryRecallHonorsBudget(t *testing.T) {
	results := make([]MemoryHit, 0, 5)
	for index := 0; index < 5; index++ {
		results = append(results, MemoryHit{
			ID: "memory_" + string(rune('a'+index)), Type: "procedure", Title: strings.Repeat("title", 40),
			Summary: strings.Repeat("summary", 80), Content: strings.Repeat("content", 300),
			Steps: []ProcedureStep{{Tool: "touch_gesture", Description: strings.Repeat("step", 80)}},
		})
	}
	limited := limitDeviceMemoryRecall(results, 4800)
	encoded, err := json.Marshal(map[string]any{"results": limited})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if len(encoded) > 4800 {
		t.Fatalf("recall payload bytes = %d, want <= 4800", len(encoded))
	}
	if len(limited) == 0 || len(limited) > 5 {
		t.Fatalf("limited results = %d, want 1..5", len(limited))
	}
}

func TestProcessEpisodeMemoryNowDrainsScheduledRetry(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhausted=%t", fail), func(t *testing.T) {
			ctx := context.Background()
			plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
			proposal := episodeMemoryProposal{EpisodeAssessment: episodeMemoryAssessment{GoalResult: episodeGoalAchieved, Reason: "Settings opened", EvidenceRefs: []string{"result"}}}
			body, _ := json.Marshal(episodeMemoryBatchResponse{Results: []episodeMemoryBatchResult{{EpisodeID: "ep_pending", Proposal: proposal}}})
			model := &episodeMemoryScriptedModel{responses: []string{string(body)}}
			if fail {
				model.responses = []string{`{"results":[`, `{"results":[`}
				model.stopReason = "length"
			}
			processor := newEpisodeMemoryProcessor(plane, model)
			processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
			if err := processor.Initialize(); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"ep_pending", "ep_unrelated"} {
				_, err := plane.episodes.AddEpisode(ctx, TaskEpisode{
					ID: id, Status: "active", StartedAt: "2026-08-14T00:00:00Z", EndedAt: "2026-08-14T00:00:01Z", UserGoal: "Open Settings",
					Events: []TaskEpisodeEvent{{EventID: "call", Type: runEventToolCall, ToolName: "open_app"}, {EventID: "result", Type: "tool_result", ToolName: "open_app", Observation: "Settings opened"}},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := processor.state.SetEpisode(id, episodeMemoryEpisodeStatus{
					Status: episodeMemoryStatusRetry, ExtractorVersion: episodeMemoryExtractorVersion,
					RetryAt: time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano), AttemptCount: 1,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// Ordinary maintenance must continue respecting the retry backoff.
			if _, err := processor.ProcessBatch(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if model.callCount() != 0 {
				t.Fatal("background retry ran early")
			}
			plane.memoryWorker = newMemoryWorker(processor, defaultMemoryWorkerIdleDelay)
			plane.episodeProcessor = processor
			status, _, err := plane.ProcessEpisodeMemoryNow(ctx, "ep_pending")
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if status.Status != episodeMemoryStatusIgnored || status.AttemptCount != episodeMemoryMaxAttempts || model.callCount() != 2 || status.LastError == "" {
					t.Fatalf("unbounded or hidden failure: status=%#v calls=%d", status, model.callCount())
				}
			} else if status.Status != episodeMemoryStatusDone || model.callCount() != 1 {
				t.Fatalf("status=%#v calls=%d", status, model.callCount())
			}
			state, err := processor.state.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			if got := state.Episodes[episodeMemoryStateKey("ep_unrelated", episodeMemoryExtractorVersion)]; got.Status != episodeMemoryStatusRetry || got.AttemptCount != 1 {
				t.Fatalf("unrelated retry changed: %#v", got)
			}
		})
	}
}

func TestEpisodeMemoryRetentionAuditDiscardsBuiltInToolGuidance(t *testing.T) {
	prompt := buildEpisodeMemoryRetentionAuditPrompt(`{"episode_id":"ep_1"}`, []episodeMemoryCandidate{{LessonKey: "retry_enter_text"}})
	// A failed enter_text already returns "Inspect the latest screen and correct
	// or clear the existing field text before retrying enter_text."; a memory
	// that repeats it adds nothing the Agent does not already receive.
	if !strings.Contains(prompt, episodeMemoryBuiltInGuidanceRule) {
		t.Fatalf("retention audit does not discard built-in tool guidance:\n%s", prompt)
	}
}

func TestEpisodeMemoryOmissionReviewDiscountsBuiltInToolGuidance(t *testing.T) {
	model := &episodeMemoryScriptedModel{responses: []string{`{"episode_assessment":{"goal_result":"unknown","reason":"no proof","evidence_refs":[]},"candidates":[]}`}}
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	processor := newEpisodeMemoryProcessor(plane, model)

	if _, err := processor.reviewEpisodeMemoryOmission(context.Background(), TaskEpisode{ID: "ep_omission_rule"}, episodeMemoryProposal{}, nil, 0); err != nil {
		t.Fatalf("reviewEpisodeMemoryOmission: %v", err)
	}
	var review string
	for _, part := range strings.Split(model.firstCallText(), "\n") {
		if strings.HasPrefix(part, "Review this first-pass assessment once") {
			review = part
		}
	}
	if !strings.Contains(review, episodeMemoryBuiltInGuidanceRule) {
		t.Fatalf("omission review does not discount built-in tool guidance:\n%s", review)
	}
}

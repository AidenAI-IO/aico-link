package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agenttask"
)

type fakeAgentTasks struct {
	tasks        []agenttask.Task
	cancelled    string
	cancelledAll bool
	reorder      error
}

func (f *fakeAgentTasks) Active() []agenttask.Task { return f.tasks }
func (f *fakeAgentTasks) Query(id string) (agenttask.Task, bool) {
	for _, task := range f.tasks {
		if task.ID == id {
			return task, true
		}
	}
	return agenttask.Task{}, false
}
func (f *fakeAgentTasks) Timeline(string) (agenttask.Timeline, bool) {
	return agenttask.Timeline{Steps: []agenttask.Step{{Kind: "tool", Title: "screenshot"}}}, true
}
func (f *fakeAgentTasks) Cancel(id string) (agenttask.Task, bool, error) {
	f.cancelled = id
	return agenttask.Task{ID: id, Status: agenttask.StatusCancelled}, false, nil
}
func (f *fakeAgentTasks) Reorder([]string) error { return f.reorder }
func (f *fakeAgentTasks) CancelAll() []agenttask.Task {
	f.cancelledAll = true
	return f.tasks
}

func TestAgentTasksHTTP(t *testing.T) {
	server := &Server{}
	serve := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		server.handleAgentTasks(recorder, httptest.NewRequest(method, target, strings.NewReader(body)))
		return recorder
	}

	if got := serve(http.MethodGet, "/api/agent-tasks", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"enabled":false,"tasks":[]`) {
		t.Fatalf("without a backend agent: %d %s", got.Code, got.Body)
	}
	if got := serve(http.MethodPut, "/api/agent-tasks/order", `{"task_ids":[]}`); got.Code != http.StatusServiceUnavailable {
		t.Fatalf("reorder without a backend agent: %d", got.Code)
	}

	tasks := &fakeAgentTasks{tasks: []agenttask.Task{{ID: "task_1", Prompt: "check screen", Status: agenttask.StatusRunning}}}
	server.SetAgentTasks(tasks)
	if got := serve(http.MethodGet, "/api/agent-tasks/task_1", ""); got.Code != 200 || !strings.Contains(got.Body.String(), `"title":"screenshot"`) {
		t.Fatalf("detail: %d %s", got.Code, got.Body)
	}
	if got := serve(http.MethodGet, "/api/agent-tasks/missing", ""); got.Code != http.StatusNotFound {
		t.Fatalf("missing detail: %d", got.Code)
	}
	if got := serve(http.MethodDelete, "/api/agent-tasks/task_1", ""); got.Code != 200 || tasks.cancelled != "task_1" {
		t.Fatalf("cancel: %d %q", got.Code, tasks.cancelled)
	}
	tasks.reorder = agenttask.ErrStaleOrder
	if got := serve(http.MethodPut, "/api/agent-tasks/order", `{"task_ids":["task_2"]}`); got.Code != http.StatusConflict {
		t.Fatalf("stale reorder: %d", got.Code)
	}
	if got := serve(http.MethodPost, "/api/agent-tasks", ""); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST list: %d", got.Code)
	}
}

func TestTaskTimelineStepHidesPrivateToolOutput(t *testing.T) {
	step, ok := TaskTimelineStep(RunEvent{Type: "tool_result", ToolCallID: "c1", ToolName: toolBridgeContacts, Content: "Alice +61 400 000 000"})
	if !ok || step.ID != "c1" || step.Status != "done" || step.Detail != "" {
		t.Fatalf("private tool step = %+v", step)
	}
	step, _ = TaskTimelineStep(RunEvent{Type: "tool_result", ToolCallID: "c2", ToolName: toolBridgeContacts, IsError: true,
		Content: "Alice +61 400 000 000", ToolError: &ToolError{Message: "lookup failed for Alice"}})
	if !step.IsError || step.Status != "failed" || step.Detail != "" {
		t.Fatalf("failed private tool step = %+v", step)
	}
	step, _ = TaskTimelineStep(RunEvent{Type: "tool_result", ToolName: "shell", IsError: true, ToolError: &ToolError{Message: "exit 1"}})
	if !step.IsError || step.Detail != "exit 1" {
		t.Fatalf("failed tool step = %+v", step)
	}
	if _, ok := TaskTimelineStep(RunEvent{Type: "reasoning_delta"}); ok {
		t.Fatal("reasoning deltas must not enter the timeline")
	}
}

// A background task drives the Live Activity like a chat run: its tool steps
// become the current step, the task ID keys it, and the stop button on it
// cancels the task.
func TestBackgroundTaskActivity(t *testing.T) {
	server := &Server{logger: newTestLogger()}
	server.liveActivity = NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	activity := server.BackgroundTaskActivity()

	activity.Start("task_1", "Order a fruit tea")
	activity.Event("task_1", RunEvent{Type: runEventToolCall, ToolName: "screenshot"})
	state := server.liveActivity.SnapshotActive()
	if state == nil || state.RequestID != "task_1" || state.TaskTitle != "Order a fruit tea" || state.CurrentStep != "Checking the screen" {
		t.Fatalf("running task = %#v", state)
	}

	tasks := &fakeAgentTasks{tasks: []agenttask.Task{{ID: "task_1", Status: agenttask.StatusRunning}}}
	server.SetAgentTasks(tasks)
	recorder := httptest.NewRecorder()
	server.handleChatCancel(recorder, httptest.NewRequest(http.MethodPost, "/api/chat/cancel", strings.NewReader(`{"request_id":"task_1"}`)))
	if tasks.cancelled != "task_1" || !strings.Contains(recorder.Body.String(), `"status":"canceled"`) {
		t.Fatalf("cancel from the Live Activity: cancelled=%q body=%s", tasks.cancelled, recorder.Body)
	}

	for _, tc := range []struct {
		name     string
		err      error
		canceled bool
		want     string
	}{
		{name: "completed", want: LiveActivityStatusCompleted},
		{name: "failed", err: errors.New("model unavailable"), want: LiveActivityStatusFailed},
		{name: "canceled", err: context.Canceled, canceled: true, want: LiveActivityStatusCanceled},
	} {
		activity.Start(tc.name, "goal")
		activity.Finish(tc.name, "done", tc.err, tc.canceled)
		if got := server.liveActivity.Snapshot(tc.name); got == nil || got.Status != tc.want {
			t.Fatalf("%s: state = %#v, want %s", tc.name, got, tc.want)
		}
	}

	// Without a Live Activity there is nothing to report to.
	var disabled *BackgroundTaskActivity
	disabled.Start("task_2", "goal")
	disabled.Event("task_2", RunEvent{Type: runEventToolCall, ToolName: "screenshot"})
	disabled.Finish("task_2", "", nil, false)
	if (&Server{}).BackgroundTaskActivity() != nil {
		t.Fatal("a server without a Live Activity returned a reporter")
	}
}

// Stopping voice mode with tasks=1 also stops the background work; without it
// the tasks keep running, as before.
func TestRealtimeVoiceStopCancelsBackgroundTasks(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"/api/realtime/voice/stop", false},
		{"/api/realtime/voice/stop?tasks=1", true},
	} {
		server := &Server{logger: newTestLogger()}
		tasks := &fakeAgentTasks{tasks: []agenttask.Task{{ID: "task_1", Status: agenttask.StatusRunning}}}
		server.SetAgentTasks(tasks)
		recorder := httptest.NewRecorder()
		server.handleRealtimeVoiceStop(recorder, httptest.NewRequest(http.MethodPost, tc.target, nil))
		if recorder.Code != http.StatusOK || tasks.cancelledAll != tc.want {
			t.Fatalf("%s: code=%d cancelledAll=%t, want %t", tc.target, recorder.Code, tasks.cancelledAll, tc.want)
		}
	}
}

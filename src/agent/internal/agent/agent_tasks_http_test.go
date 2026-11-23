package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
func (f *fakeAgentTasks) CancelUnfinished(id string) (agenttask.Task, error) {
	if task, ok := f.Query(id); !ok || task.CompletedAt != nil {
		return agenttask.Task{}, errors.New("no unfinished agent task")
	}
	f.cancelled = id
	return agenttask.Task{ID: id, Status: agenttask.StatusCancelling}, nil
}
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

// scriptedTaskRunner hands each run to the test, which drives it through its
// context and decides when it returns.
type scriptedTaskRunner struct {
	runs chan scriptedRun
}

type scriptedRun struct {
	ctx    context.Context
	prompt string
	done   chan string
}

func (r *scriptedTaskRunner) Run(ctx context.Context, prompt string) (string, error) {
	run := scriptedRun{ctx: ctx, prompt: prompt, done: make(chan string)}
	r.runs <- run
	select {
	case result := <-run.done:
		return result, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *scriptedTaskRunner) next(t *testing.T) scriptedRun {
	t.Helper()
	select {
	case run := <-r.runs:
		return run
	case <-time.After(2 * time.Second):
		t.Fatal("the task did not run")
		return scriptedRun{}
	}
}

func waitLiveActivity(t *testing.T, server *Server, id string, ok func(*LiveActivityState) bool) *LiveActivityState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state := server.liveActivity.Snapshot(id)
		if state != nil && ok(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("Live Activity %s = %#v", id, state)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A background task drives the Live Activity like a chat run, for the task's
// whole lifecycle: a rerun after a goal update keeps one running activity
// under the task's title, and a task cancelled while it waits for the
// user settles the activity even though no run is active.
func TestBackgroundTaskActivity(t *testing.T) {
	server := &Server{logger: newTestLogger()}
	server.liveActivity = NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger())
	activity := server.BackgroundTaskActivity()
	runner := &scriptedTaskRunner{runs: make(chan scriptedRun, 1)}
	manager := agenttask.NewManager(runner)
	defer manager.Close()
	manager.SetLifecycleObserver(activity.Observe)

	task, err := manager.Create("Order a fruit tea")
	if err != nil {
		t.Fatal(err)
	}
	first := runner.next(t)
	activity.Event(task.ID, RunEvent{Type: runEventToolCall, ToolName: "screenshot"})
	if state := server.liveActivity.Snapshot(task.ID); state == nil || state.TaskTitle != "Order a fruit tea" || state.CurrentStep != "Checking the screen" {
		t.Fatalf("running task = %#v", state)
	}

	// A goal update the run did not take reruns the task; the activity must
	// neither settle in between nor take the rerun prompt as its title.
	if _, err := manager.Update(task.ID, "Order a mango tea"); err != nil {
		t.Fatal(err)
	}
	first.done <- "ordered fruit tea"
	second := runner.next(t)
	if state := server.liveActivity.Snapshot(task.ID); state == nil || state.EndedAt != nil || state.TaskTitle != "Order a fruit tea" {
		t.Fatalf("rerun task = %#v", state)
	}

	// The run asks the user to take over and returns; cancelling then has no
	// run to report through.
	agenttask.UserActionHandlerFromContext(second.ctx)(agenttask.UserAction{Reason: "pay"})
	activity.Event(task.ID, RunEvent{Type: "tool_result", ToolName: toolUserActionStep, Content: "Confirm the payment"})
	second.done <- ""
	waitLiveActivity(t, server, task.ID, func(s *LiveActivityState) bool { return s.Status == LiveActivityStatusNeedsApp })
	if _, _, err := manager.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	waitLiveActivity(t, server, task.ID, func(s *LiveActivityState) bool { return s.Status == LiveActivityStatusCanceled })

	// Without a Live Activity there is nothing to report to.
	var disabled *BackgroundTaskActivity
	disabled.Observe(agenttask.Task{ID: "task_2", Prompt: "goal"}, "running")
	disabled.Event("task_2", RunEvent{Type: runEventToolCall, ToolName: "screenshot"})
	if (&Server{}).BackgroundTaskActivity() != nil {
		t.Fatal("a server without a Live Activity returned a reporter")
	}
}

// The Live Activity's stop button cancels a running background task. A
// finished one is left alone so its result is still announced, and a chat run
// registered under the same ID is cancelled instead of the task.
func TestChatCancelStopsBackgroundTask(t *testing.T) {
	finished := time.Now()
	for _, tc := range []struct {
		name    string
		task    agenttask.Task
		chatRun bool
		want    string
		status  string
	}{
		{"running", agenttask.Task{ID: "task_1", Status: agenttask.StatusRunning}, false, "task_1", "canceled"},
		{"finished", agenttask.Task{ID: "task_1", Status: agenttask.StatusCompleted, CompletedAt: &finished}, false, "", "not_running"},
		{"chat run with the same ID", agenttask.Task{ID: "task_1", Status: agenttask.StatusRunning}, true, "", "canceled"},
	} {
		server := &Server{logger: newTestLogger()}
		tasks := &fakeAgentTasks{tasks: []agenttask.Task{tc.task}}
		server.SetAgentTasks(tasks)
		runCancelled := false
		if tc.chatRun {
			server.activeRuns = map[string]context.CancelFunc{"task_1": func() { runCancelled = true }}
		}
		recorder := httptest.NewRecorder()
		server.handleChatCancel(recorder, httptest.NewRequest(http.MethodPost, "/api/chat/cancel", strings.NewReader(`{"request_id":"task_1"}`)))
		var resp ChatCancelResponse
		if err := json.NewDecoder(recorder.Body).Decode(&resp); err != nil || recorder.Code != http.StatusOK {
			t.Fatalf("%s: code=%d err=%v", tc.name, recorder.Code, err)
		}
		if tasks.cancelled != tc.want || runCancelled != tc.chatRun || resp.Status != tc.status {
			t.Fatalf("%s: task cancelled=%q run cancelled=%t status=%q", tc.name, tasks.cancelled, runCancelled, resp.Status)
		}
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

package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agenttask"
)

type fakeAgentTasks struct {
	tasks     []agenttask.Task
	cancelled string
	reorder   error
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
	step, _ = TaskTimelineStep(RunEvent{Type: "tool_result", ToolName: "shell", IsError: true, ToolError: &ToolError{Message: "exit 1"}})
	if !step.IsError || step.Detail != "exit 1" {
		t.Fatalf("failed tool step = %+v", step)
	}
	if _, ok := TaskTimelineStep(RunEvent{Type: "reasoning_delta"}); ok {
		t.Fatal("reasoning deltas must not enter the timeline")
	}
}

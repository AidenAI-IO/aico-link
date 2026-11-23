package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"aico-agent/internal/agenttask"
)

// AgentTaskController is the background task manager as seen by the HTTP API.
// The daemon installs it only while realtime mode runs a backend agent.
type AgentTaskController interface {
	Active() []agenttask.Task
	CancelAll() []agenttask.Task
	CancelUnfinished(taskID string) (agenttask.Task, error)
	Query(taskID string) (agenttask.Task, bool)
	Timeline(taskID string) (agenttask.Timeline, bool)
	Cancel(taskID string) (agenttask.Task, bool, error)
	Reorder(taskIDs []string) error
}

type agentTaskRegistry struct {
	mu         sync.RWMutex
	controller AgentTaskController
}

// SetAgentTasks installs the background task manager, or removes it with nil.
func (s *Server) SetAgentTasks(controller AgentTaskController) {
	s.agentTasks.mu.Lock()
	s.agentTasks.controller = controller
	s.agentTasks.mu.Unlock()
}

func (s *Server) agentTaskController() AgentTaskController {
	s.agentTasks.mu.RLock()
	defer s.agentTasks.mu.RUnlock()
	return s.agentTasks.controller
}

type agentTaskListResponse struct {
	Enabled bool             `json:"enabled"`
	Tasks   []agenttask.Task `json:"tasks"`
}

type agentTaskDetailResponse struct {
	Task     agenttask.Task     `json:"task"`
	Timeline agenttask.Timeline `json:"timeline"`
}

// handleAgentTasks serves the background task queue:
//
//	GET  /api/agent-tasks                 unfinished tasks in execution order
//	PUT  /api/agent-tasks/order           {"task_ids": [...]} reorders queued tasks
//	GET  /api/agent-tasks/{id}            one task with its recent timeline
//	DELETE /api/agent-tasks/{id}          cancels queued or running work
func (s *Server) handleAgentTasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tasks := s.agentTaskController()
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent-tasks"), "/")
	switch {
	case rest == "":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response := agentTaskListResponse{Enabled: tasks != nil, Tasks: []agenttask.Task{}}
		if tasks != nil {
			response.Tasks = append(response.Tasks, tasks.Active()...)
		}
		writeJSON(w, response)
	case rest == "order":
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if tasks == nil {
			http.Error(w, "Background agent is not running", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			TaskIDs []string `json:"task_ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
		if err := tasks.Reorder(request.TaskIDs); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, agenttask.ErrStaleOrder) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, agentTaskListResponse{Enabled: true, Tasks: tasks.Active()})
	case !strings.Contains(rest, "/"):
		if tasks == nil {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			task, ok := tasks.Query(rest)
			if !ok {
				http.NotFound(w, r)
				return
			}
			timeline, _ := tasks.Timeline(rest)
			writeJSON(w, agentTaskDetailResponse{Task: task, Timeline: timeline})
		case http.MethodDelete:
			task, _, err := tasks.Cancel(rest)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, task)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	default:
		http.NotFound(w, r)
	}
}

// TaskTimelineStep converts a backend run event into a task timeline step.
// Tool steps share the tool call ID so a result replaces its call. Output of
// tools that read private phone data is never copied into the timeline, even
// when the call failed.
func TaskTimelineStep(event RunEvent) (agenttask.Step, bool) {
	step := agenttask.Step{At: event.Timestamp, ID: event.ToolCallID}
	switch event.Type {
	case runEventToolCall:
		step.Kind, step.Title, step.Status = "tool", event.ToolName, "running"
		step.Detail = event.ToolInput
	case "tool_result":
		step.Kind, step.Title, step.Status = "tool", event.ToolName, "done"
		step.Detail = event.Content
		if event.IsError || event.ToolError != nil {
			step.Status, step.IsError = "failed", true
			if event.ToolError != nil && event.ToolError.Message != "" {
				step.Detail = event.ToolError.Message
			}
		}
	case "assistant_output":
		if strings.TrimSpace(event.Content) == "" {
			return agenttask.Step{}, false
		}
		step.ID, step.Kind, step.Title, step.Detail = "", "message", "Agent", event.Content
	default:
		return agenttask.Step{}, false
	}
	// A failed result still shows as failed through Status and IsError; its
	// text can carry the same private data as a successful one.
	if step.Kind == "tool" && step.Status != "running" && PrivateToolOutput(event.ToolName) {
		step.Detail = ""
	}
	return step, true
}

// PrivateToolOutput reports whether a tool's output holds private phone data
// (clipboard, calendar, contacts, notifications) that observers such as task
// timelines and cancelled-run summaries must not copy.
func PrivateToolOutput(toolName string) bool {
	return toolResultIsSensitive(toolName)
}

// BackgroundTaskActivity reports a background agent task's progress on the
// phone's Live Activity, as a chat run's progress is. The task ID is the
// Live Activity request ID, so the steps (checking the screen, tapping,
// opening an app) use the same wording as a chat run. It is nil when the
// Live Activity is disabled.
type BackgroundTaskActivity struct {
	server *Server
}

// BackgroundTaskActivity returns the reporter for background tasks, or nil
// when the Live Activity is disabled.
func (s *Server) BackgroundTaskActivity() *BackgroundTaskActivity {
	if s == nil || s.liveActivity == nil {
		return nil
	}
	return &BackgroundTaskActivity{server: s}
}

// Observe follows the task manager's lifecycle. A task can run the agent
// several times (after a goal update or a human handoff), so the Live Activity
// starts on the first "running" and settles only on "terminal", including for
// a task cancelled while it waits for the user and no run is active.
func (a *BackgroundTaskActivity) Observe(task agenttask.Task, phase string) {
	if a == nil || task.ID == "" {
		return
	}
	live := a.server.liveActivity
	switch phase {
	case "running":
		if state := live.Snapshot(task.ID); state != nil && state.EndedAt == nil {
			return
		}
		live.StartTask(task.ID, task.Prompt, a.server.liveActivityPhoneID(ChatRequest{}))
	case "terminal":
		switch task.Status {
		case agenttask.StatusCompleted:
			live.completeTask(task.ID, task.Result)
		case agenttask.StatusCancelled:
			live.CancelTask(task.ID)
		case agenttask.StatusFailed:
			live.FailTask(task.ID, task.Error)
		}
	}
}

// Event advances the current step from one of the task's run events.
func (a *BackgroundTaskActivity) Event(taskID string, event RunEvent) {
	if a == nil || taskID == "" {
		return
	}
	a.server.liveActivity.UpdateFromRunEvent(taskID, event)
}

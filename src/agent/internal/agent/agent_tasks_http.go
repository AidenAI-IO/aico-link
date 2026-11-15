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
// tools that read private phone data is never copied into the timeline.
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
	if step.Kind == "tool" && toolResultIsSensitive(event.ToolName) && step.Status != "running" && !step.IsError {
		step.Detail = ""
	}
	return step, true
}

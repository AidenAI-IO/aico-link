package agent

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agenttask"
	"github.com/tmc/langchaingo/llms"
)

func TestAgentTaskHistoryAvailabilityExcludesActiveTasks(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	tasks := &fakeAgentTasks{tasks: []agenttask.Task{{ID: "task_one", Status: agenttask.StatusRunning}}}
	server := &Server{episodeStore: store}
	server.SetAgentTasks(tasks)

	check := func(want bool) {
		t.Helper()
		response := httptest.NewRecorder()
		server.handleAgentTasks(response, httptest.NewRequest(http.MethodGet, "/api/agent-tasks", nil))
		var got agentTaskListResponse
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || got.HasHistory == nil || *got.HasHistory != want {
			t.Fatalf("has_history should be %t, got %d %s", want, response.Code, response.Body)
		}
	}
	check(false)
	episode := TaskEpisode{
		ID: "ep_one", TaskID: tasks.tasks[0].ID, UserGoal: "Check the weather",
		StartedAt: "2026-11-20T02:40:00Z", EndedAt: "2026-11-20T02:40:02Z",
		Outcome: TaskEpisodeOutcome{Success: true},
	}
	if _, err := store.AddEpisode(context.Background(), episode); err != nil {
		t.Fatal(err)
	}
	check(false)
	tasks.tasks = nil
	check(true)
}

func TestAgentTaskHistoryGroupsExecutionsAndProjectsToolFailures(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	created := "2026-11-20T02:45:47Z"
	longResult := strings.TrimSpace(strings.Repeat("Weather app result\n", 60))
	first := TaskEpisode{
		ID: "ep_first", TaskID: "task_weather", TaskCreatedAt: created,
		UserGoal: "Check the weather", StartedAt: "2026-11-20T02:45:57Z", EndedAt: "2026-11-20T02:46:00Z",
		Outcome: TaskEpisodeOutcome{Success: true},
		Extra: map[string]interface{}{
			"model_name": "gpt-6-luna", "model_provider_host": "apibest.ai", "total_duration_ms": 3000,
			"prompt_tokens": 14000, "cached_prompt_tokens": 14000, "completion_tokens": 0,
			"official_price_estimate_usd": 0.001,
		},
	}
	resumed := TaskEpisode{
		ID: "ep_resumed", TaskID: first.TaskID, TaskCreatedAt: created,
		UserGoal: "Continue", StartedAt: "2026-11-20T02:46:10Z", EndedAt: "2026-11-20T02:46:30Z",
		Outcome: TaskEpisodeOutcome{Success: true},
		Extra: map[string]interface{}{
			"model_name": "gpt-6-luna", "model_provider_host": "apibest.ai", "total_duration_ms": 20000,
			"prompt_tokens": 60156, "cached_prompt_tokens": 57552, "completion_tokens": 71,
		},
		Events: []TaskEpisodeEvent{
			{Type: "tool_result", ToolName: toolBridgeContacts, Content: "Private contact payload"},
			{Type: "tool_result", ToolName: "open_app", Content: longResult},
			{Type: runEventToolCall, ToolName: "weather", ToolInput: "Private tool argument"},
			{Type: "tool_result", ToolName: "weather", Ts: "2026-11-20T02:46:22Z", IsError: true,
				ToolError: NewToolError(CodeToolExecutionFailed, "Get https://weather.example/private?secret=secret: net/http: TLS handshake timeout")},
		},
	}
	active := TaskEpisode{
		ID: "ep_waiting", TaskID: "task_waiting", UserGoal: "Waiting for a user",
		StartedAt: "2026-11-20T02:41:00Z", EndedAt: "2026-11-20T02:42:00Z",
		Outcome: TaskEpisodeOutcome{Success: true},
	}
	legacy := TaskEpisode{
		ID: "ep_legacy", UserGoal: "An older run", StartedAt: "2026-11-19T02:45:57Z", EndedAt: "2026-11-19T02:46:00Z",
		Outcome: TaskEpisodeOutcome{FailureReason: "context canceled"},
	}
	for _, episode := range []TaskEpisode{first, resumed, active, legacy} {
		if _, err := store.AddEpisode(context.Background(), episode); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{episodeStore: NewTaskEpisodeStore(store.rootDir)}
	server.SetAgentTasks(&fakeAgentTasks{tasks: []agenttask.Task{{ID: active.TaskID, Status: agenttask.StatusRunning}}})
	get := func(path string, target interface{}) string {
		t.Helper()
		response := httptest.NewRecorder()
		server.handleTaskHistory(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, response.Code, response.Body)
		}
		if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
			t.Fatal(err)
		}
		return response.Body.String()
	}
	var page taskHistoryPage
	get("/api/agent-tasks/history", &page)
	if len(page.Tasks) != 2 || page.Tasks[1].ID != legacy.ID || page.Tasks[1].Cost != nil {
		t.Fatalf("unexpected history: %+v", page.Tasks)
	}
	row := page.Tasks[0]
	if row.ID != first.TaskID || row.Prompt != first.UserGoal || row.Status != "tool_error" ||
		row.DurationMS != 43000 || row.Tokens == nil || row.Tokens.Total != 74227 ||
		row.Cost != nil {
		t.Fatalf("unexpected task summary: %+v", row)
	}
	var detail taskHistoryDetail
	body := get("/api/agent-tasks/history/"+row.ID, &detail)
	if detail.ExecutionMS != 23000 || detail.Error != "TLS handshake timeout" ||
		len(detail.Steps) != 3 || detail.Steps[0].Detail != "" || detail.Steps[1].Detail != longResult ||
		detail.Steps[2].Status != "failed" || detail.Steps[2].Tool != "weather" ||
		detail.Steps[2].Detail != "TLS handshake timeout" {
		t.Fatalf("unexpected task detail: %+v", detail)
	}
	for _, private := range []string{"Private contact payload", "Private tool argument", "weather.example"} {
		if strings.Contains(body, private) {
			t.Fatalf("history exposed %q in %s", private, body)
		}
	}
}

func TestAgentTaskHistoryUsesTelemetryCostFallback(t *testing.T) {
	provider := map[string]any{"cost_usd": 0.0, "estimated_cost": 1.0}
	estimate := map[string]any{"estimated_cost": 0.125}
	for _, tc := range []struct {
		name  string
		calls []map[string]any
		want  *taskHistoryCost
	}{
		{"provider first", []map[string]any{provider}, &taskHistoryCost{Source: "provider"}},
		{"telemetry estimate", []map[string]any{estimate}, &taskHistoryCost{USD: 0.125, Estimated: true, Source: "telemetry"}},
		{"mixed", []map[string]any{provider, estimate, nil}, &taskHistoryCost{USD: 0.125, Estimated: true, Source: "mixed", Partial: true}},
		{"unavailable", []map[string]any{{"total_tokens": 12000}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := &RunMetrics{}
			for _, info := range tc.calls {
				res := &llms.ContentResponse{Choices: []*llms.ContentChoice{{GenerationInfo: info}}}
				recordModelCallMetrics(metrics, res, time.Now())
			}
			episode := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "task"}, MemoryContext{}).
				Finish("", metrics, nil, nil, nil)
			got, ok := taskHistoryEpisodeCost(episode)
			if tc.want == nil {
				if ok {
					t.Fatalf("cost = %+v, want unavailable", got)
				}
				return
			}
			if !ok || got != *tc.want {
				t.Fatalf("cost = %+v (%t), want %+v", got, ok, *tc.want)
			}
			if got.Estimated && metrics.CostUSD != nil {
				t.Fatal("telemetry estimates must not appear as provider billing")
			}
		})
	}
	if got := telemetryCostDetailsFromMap(provider)["total"]; math.Abs(got) > 0 {
		t.Fatalf("Langfuse telemetry replaced the provider's zero cost: %v", got)
	}
}

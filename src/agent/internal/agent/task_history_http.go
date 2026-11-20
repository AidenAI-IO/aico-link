package agent

import (
	"errors"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aico-agent/internal/agenttask"
	"aico-agent/internal/util"
)

const (
	taskHistoryPageSize = 20
	taskHistoryMaxSteps = 80
)

type taskHistoryTokens struct {
	Input       int `json:"input"`
	CachedInput int `json:"cached_input"`
	Output      int `json:"output"`
	Total       int `json:"total"`
}

type taskHistoryCost struct {
	USD       float64 `json:"usd"`
	Estimated bool    `json:"estimated"`
	Source    string  `json:"source"`
	Partial   bool    `json:"partial,omitempty"`
}

type taskHistoryRow struct {
	ID         string             `json:"id"`
	Prompt     string             `json:"prompt"`
	Status     string             `json:"status"`
	StartedAt  time.Time          `json:"started_at"`
	DurationMS int64              `json:"duration_ms"`
	Tokens     *taskHistoryTokens `json:"tokens,omitempty"`
	Cost       *taskHistoryCost   `json:"cost,omitempty"`
}

type taskHistoryPage struct {
	Tasks      []taskHistoryRow `json:"tasks"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type taskHistoryStep struct {
	At         string `json:"at"`
	Tool       string `json:"tool"`
	Status     string `json:"status"`
	DurationMS *int64 `json:"duration_ms,omitempty"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type taskHistoryDetail struct {
	taskHistoryRow
	ExecutionMS int64             `json:"execution_ms"`
	Models      []string          `json:"models,omitempty"`
	Providers   []string          `json:"providers,omitempty"`
	Pricing     []string          `json:"pricing,omitempty"`
	Result      string            `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	Steps       []taskHistoryStep `json:"steps"`
	Omitted     int               `json:"omitted,omitempty"`
}

type taskHistoryGroup struct {
	id      string
	entries []episodeIndexEntry // Newest execution first.
}

func (s *Server) handleTaskHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/agent-tasks/history"), "/")
	if strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	page := taskHistoryPage{Tasks: []taskHistoryRow{}}
	store := s.episodeStore
	if store == nil {
		if id != "" {
			http.NotFound(w, r)
		} else {
			writeJSON(w, page)
		}
		return
	}
	index, err := store.loadIndex()
	if err != nil {
		http.Error(w, "Task history is unavailable", http.StatusInternalServerError)
		return
	}
	var active map[string]bool
	if tasks := s.agentTaskController(); tasks != nil {
		active = taskHistoryActiveIDs(tasks.Active())
	}
	groups := groupTaskHistory(index, active)
	var catalog map[string]modelsDevProvider
	if s.runtime != nil {
		if manager, ok := s.runtime.models.(*ModelManager); ok {
			catalog = manager.cachedModelsDevCatalog()
		}
	}
	if id != "" {
		for _, group := range groups {
			if group.id != id {
				continue
			}
			detail, err := store.readTaskHistory(group, true, catalog)
			switch {
			case errors.Is(err, os.ErrNotExist):
				http.NotFound(w, r)
			case err != nil:
				http.Error(w, "Task details are unavailable", http.StatusInternalServerError)
			default:
				writeJSON(w, detail)
			}
			return
		}
		http.NotFound(w, r)
		return
	}
	cursor := r.URL.Query().Get("before")
	for _, group := range groups {
		if cursor != "" {
			if group.id == cursor {
				cursor = ""
			}
			continue
		}
		if len(page.Tasks) == taskHistoryPageSize {
			page.NextCursor = page.Tasks[len(page.Tasks)-1].ID
			break
		}
		detail, err := store.readTaskHistory(group, false, catalog)
		if errors.Is(err, os.ErrNotExist) {
			continue // Retention can remove an entry after we read the index.
		}
		if err != nil {
			http.Error(w, "Task history is unavailable", http.StatusInternalServerError)
			return
		}
		page.Tasks = append(page.Tasks, detail.taskHistoryRow)
	}
	writeJSON(w, page)
}

func taskHistoryActiveIDs(tasks []agenttask.Task) map[string]bool {
	if len(tasks) == 0 {
		return nil
	}
	active := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		active[task.ID] = true
	}
	return active
}

func hasTaskHistory(index episodeIndex, active map[string]bool) bool {
	for range taskHistoryEntries(index, active) {
		return true
	}
	return false
}

func taskHistoryEntries(index episodeIndex, active map[string]bool) iter.Seq2[string, episodeIndexEntry] {
	return func(yield func(string, episodeIndexEntry) bool) {
		eligible := make(map[string]bool)
		for _, entry := range index.Episodes {
			id := firstNonEmptyString([]string{entry.TaskID, entry.ID})
			if id == "" || active[id] {
				continue
			}
			visible, known := eligible[id]
			if !known {
				visible = entry.Status != "running" && entry.EndedAt != "" && strings.TrimSpace(entry.UserGoal) != ""
				eligible[id] = visible
			}
			if visible && !yield(id, entry) {
				return
			}
		}
	}
}

func groupTaskHistory(index episodeIndex, active map[string]bool) []taskHistoryGroup {
	positions := make(map[string]int)
	groups := make([]taskHistoryGroup, 0)
	for id, entry := range taskHistoryEntries(index, active) {
		if position, found := positions[id]; found {
			groups[position].entries = append(groups[position].entries, entry)
			continue
		}
		positions[id] = len(groups)
		groups = append(groups, taskHistoryGroup{id: id, entries: []episodeIndexEntry{entry}})
	}
	return groups
}

func (s *TaskEpisodeStore) readTaskHistory(group taskHistoryGroup, withSteps bool, catalog map[string]modelsDevProvider) (taskHistoryDetail, error) {
	detail := taskHistoryDetail{taskHistoryRow: taskHistoryRow{ID: group.id}, Steps: []taskHistoryStep{}}
	var endedAt time.Time
	var hasExecution bool
	var tokens taskHistoryTokens
	partialCost := false
	for i := len(group.entries) - 1; i >= 0; i-- {
		entry := group.entries[i]
		episode, err := s.readEpisodeMetadata(entry)
		if errors.Is(err, os.ErrNotExist) {
			partialCost = true
			continue
		}
		if err != nil {
			return detail, err
		}
		start, _ := time.Parse(time.RFC3339Nano, episode.StartedAt)
		end, _ := time.Parse(time.RFC3339Nano, episode.EndedAt)
		if !hasExecution {
			detail.Prompt = truncateForLog(strings.TrimSpace(episode.UserGoal), episodeIndexTextLimit)
			detail.StartedAt = start
			if created, err := time.Parse(time.RFC3339Nano, episode.TaskCreatedAt); err == nil {
				detail.StartedAt = created
			}
			hasExecution = true
		}
		if end.After(endedAt) {
			endedAt = end
		}
		if elapsed, ok := util.UsageMetricInt(episode.Extra["total_duration_ms"]); ok {
			detail.ExecutionMS += int64(max(elapsed, 0))
		} else if end.After(start) && !start.IsZero() {
			detail.ExecutionMS += end.Sub(start).Milliseconds()
		}
		usage := taskHistoryRecordedTokens(episode.Extra)
		tokens.Input += usage.Input
		tokens.CachedInput += usage.CachedInput
		tokens.Output += usage.Output
		tokens.Total += usage.Total
		model := extraString(episode.Extra, "model_name")
		cost, hasCost := taskHistoryEpisodeCost(episode)
		if !hasCost {
			if estimate, ok := estimateHistoricalCatalogCost(episode, catalog); ok {
				cost = taskHistoryCost{USD: estimate.usd, Source: "catalog", Estimated: true, Partial: estimate.partial}
				hasCost = true
				if withSteps {
					detail.Pricing = append(detail.Pricing, estimate.rate)
				}
			}
		}
		if hasCost {
			if detail.Cost == nil {
				detail.Cost = &cost
			} else {
				detail.Cost.USD += cost.USD
				detail.Cost.Estimated = detail.Cost.Estimated || cost.Estimated
				if detail.Cost.Source != cost.Source {
					detail.Cost.Source = "mixed"
				}
			}
			partialCost = partialCost || cost.Partial
		} else {
			partialCost = true
		}
		detail.Status, detail.Error = taskHistoryOutcome(episode)
		// Historical summaries predate the structured tool outcome. Consult
		// their last execution trace when necessary so the list matches detail.
		_, outcomeTracked := episode.Extra["last_tool_error"]
		legacyOutcome := i == 0 && !outcomeTracked && (detail.Status == "completed" || detail.Error == "Task failed.")
		if entry.EventsFile != "" && (withSteps || legacyOutcome) {
			if events, err := readEpisodeEvents(filepath.Join(s.rootDir, entry.EventsFile)); err == nil {
				if legacyOutcome {
					if reason := lastHistoryToolError(events); reason != "" {
						if detail.Status == "completed" {
							detail.Status = "tool_error"
						}
						detail.Error = reason
					}
				}
				if withSteps {
					detail.appendSteps(events)
				}
			}
		}
		if withSteps {
			detail.Pricing = append(detail.Pricing, strings.Split(extraString(episode.Extra, "history_catalog_rates"), "\n")...)
			detail.Models = append(detail.Models, model)
			provider := extraString(episode.Extra, "model_provider_host")
			if provider == "" {
				provider = extraString(episode.Extra, "model_provider")
				if provider == "openai" {
					provider = "OpenAI-compatible"
				}
			}
			detail.Providers = append(detail.Providers, provider)
			detail.Result = truncateForLog(strings.TrimSpace(episode.Outcome.FinalAnswer), 1200)
		}
	}
	if !hasExecution {
		return detail, os.ErrNotExist
	}
	if !detail.StartedAt.IsZero() && endedAt.After(detail.StartedAt) {
		detail.DurationMS = endedAt.Sub(detail.StartedAt).Milliseconds()
	}
	if tokens.Total > 0 {
		detail.Tokens = &tokens
	}
	if detail.Cost != nil {
		detail.Cost.Partial = partialCost
	}
	detail.Models = uniqueNonEmpty(detail.Models)
	detail.Providers = uniqueNonEmpty(detail.Providers)
	detail.Pricing = uniqueNonEmpty(detail.Pricing)
	return detail, nil
}

func taskHistoryEpisodeCost(episode TaskEpisode) (taskHistoryCost, bool) {
	if amount, ok := providerCostUSD(episode.Extra); ok {
		return taskHistoryCost{USD: amount, Source: "provider"}, true
	}
	if amount, ok := costMetricFloat(episode.Extra["history_cost_usd"]); ok {
		calls, _ := util.UsageMetricInt(episode.Extra["llm_calls"])
		priced, _ := util.UsageMetricInt(episode.Extra["history_cost_calls"])
		reported, _ := util.UsageMetricInt(episode.Extra["cost_reported_calls"])
		catalog, _ := util.UsageMetricInt(episode.Extra["history_catalog_calls"])
		cost := taskHistoryCost{USD: amount, Source: "provider", Partial: priced < calls}
		cost.Estimated = reported < priced
		switch {
		case catalog > 0 && catalog == priced:
			cost.Source = "catalog"
		case catalog > 0 || (reported > 0 && reported < priced):
			cost.Source = "mixed"
		case cost.Estimated:
			cost.Source = "telemetry"
		}
		return cost, true
	}
	// Older episodes may carry the same cost fields the exporter used.
	amount, exists := episodeCostDetails(episode)["total"]
	amount, valid := costMetricFloat(amount)
	return taskHistoryCost{USD: amount, Estimated: true, Source: "telemetry"}, exists && valid
}

func taskHistoryRecordedTokens(extra map[string]interface{}) taskHistoryTokens {
	input, _ := util.UsageMetricInt(extra["prompt_tokens"])
	cached, _ := util.UsageMetricInt(extra["cached_prompt_tokens"])
	output, _ := util.UsageMetricInt(extra["completion_tokens"])
	total, _ := util.UsageMetricInt(extra["total_tokens"])
	return taskHistoryTokens{
		Input: max(0, input), CachedInput: min(max(0, cached), max(0, input)),
		Output: max(0, output), Total: max(total, max(0, input)+max(0, output)),
	}
}

func taskHistoryOutcome(episode TaskEpisode) (status, reason string) {
	failure := episode.Outcome.FailureReason
	if episode.Status == "interrupted" {
		return "interrupted", "Agent restarted before the task finished."
	}
	if cancelled, _ := episode.Extra["run_cancelled"].(bool); cancelled || failure == "context canceled" {
		return "cancelled", "Execution was cancelled."
	}
	if !episode.Outcome.Success {
		lastToolError := extraString(episode.Extra, "last_tool_error")
		return "failed", historyErrorSummary(failure, firstNonEmptyString([]string{lastToolError, "Task failed."}))
	}
	if reason := extraString(episode.Extra, "last_tool_error"); reason != "" {
		return "tool_error", reason
	}
	return "completed", ""
}

func historyErrorSummary(message, fallback string) string {
	message = strings.ToLower(message)
	switch {
	case strings.Contains(message, "tls handshake timeout"):
		return "TLS handshake timeout"
	case strings.Contains(message, "frame service: timeout"):
		return "Frame service timeout"
	case strings.Contains(message, "upstream_unavailable"), strings.Contains(message, "http 503"), strings.Contains(message, "status 503"):
		return "Model upstream unavailable (HTTP 503)"
	case strings.Contains(message, "context deadline exceeded"):
		return "Deadline exceeded"
	default:
		return fallback
	}
}

func historyToolError(event TaskEpisodeEvent) string {
	if !event.IsError && event.ToolError == nil {
		return ""
	}
	if PrivateToolOutput(event.ToolName) {
		return "Tool failed"
	}
	if event.ToolError != nil {
		code := "Tool failed"
		if _, known := codeRegistry[event.ToolError.Code]; known {
			code = strings.ReplaceAll(event.ToolError.Code, "_", " ")
		}
		return historyErrorSummary(event.ToolError.Message, code)
	}
	return historyErrorSummary(event.Content, "Tool failed")
}

func lastHistoryToolError(events []TaskEpisodeEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == "tool_result" {
			return historyToolError(events[i])
		}
	}
	return ""
}

func (d *taskHistoryDetail) appendSteps(events []TaskEpisodeEvent) {
	for _, event := range events {
		step := taskHistoryStep{At: event.Ts, Tool: event.ToolName, DurationMS: event.DurationMs}
		if step.Tool == "" {
			continue
		}
		switch event.Type {
		case runEventToolCall:
			step.Status = "running"
		case "tool_result":
			step.Status = "done"
			if step.Error = historyToolError(event); step.Error != "" {
				step.Status = "failed"
			}
			if !PrivateToolOutput(step.Tool) {
				step.Detail = step.Error
				if step.Status == "done" {
					step.Detail = strings.TrimSpace(event.Content)
					if step.Tool == "screenshot" && event.ScreenshotRef != "" && event.Observation != "" {
						step.Detail = event.Observation
					}
				}
			}
		default:
			continue
		}
		last := len(d.Steps) - 1
		if step.Status != "running" && last >= 0 && d.Steps[last].Status == "running" && d.Steps[last].Tool == step.Tool {
			d.Steps[last] = step
			continue
		}
		d.Steps = append(d.Steps, step)
	}
	if omitted := len(d.Steps) - taskHistoryMaxSteps; omitted > 0 {
		d.Omitted += omitted
		d.Steps = append([]taskHistoryStep(nil), d.Steps[omitted:]...)
	}
}

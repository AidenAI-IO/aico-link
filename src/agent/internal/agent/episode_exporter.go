package agent

import (
	"aico-agent/internal/agent/langfuse"
	"aico-agent/internal/util"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	langfuseBatchSize          = 20
	langfuseTraceIngestReserve = 5 * time.Second
	langfuseTraceName          = "aico-episode"
)

type langfuseClient = langfuse.Client

type langfuseIterationWindow struct {
	Index int
	ID    string
	Start time.Time
	End   time.Time
}

type langfuseToolPair struct {
	CallObservationID   string
	ResultObservationID string
	CallEventID         string
	ResultEventID       string
	ResultTime          time.Time
	ResultDurationMs    *int64
	HasCall             bool
	HasResult           bool
}

type EpisodeExporter struct {
	cfg    TelemetryConfig
	client *langfuseClient
	logger *Logger
}

func NewEpisodeExporter(cfg TelemetryConfig, logger *Logger) *EpisodeExporter {
	return &EpisodeExporter{
		cfg: cfg,
		client: langfuse.NewClient(langfuse.Config{
			BaseURL:       cfg.BaseURL,
			PublicKey:     cfg.PublicKey,
			SecretKey:     cfg.SecretKey,
			UploadTimeout: cfg.UploadTimeoutOrDefault(),
		}),
		logger: logger,
	}
}

func (e *EpisodeExporter) ExportEpisodeDir(ctx context.Context, episodeDir string, episode TaskEpisode, promptCalls ...[]telemetryPromptCall) error {
	if e == nil || !e.cfg.EnabledOrDefault() {
		return nil
	}
	if !e.client.Configured() {
		return fmt.Errorf("langfuse credentials or base_url missing")
	}
	metaPath := filepath.Join(episodeDir, "episode.yaml")
	eventsPath := filepath.Join(episodeDir, "events.jsonl")
	if _, err := os.Stat(metaPath); err != nil {
		return fmt.Errorf("episode metadata missing: %w", err)
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return fmt.Errorf("read episode metadata: %w", err)
	}
	var stored TaskEpisode
	if err := yaml.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode episode metadata: %w", err)
	}
	events, err := readEpisodeEvents(eventsPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read episode events: %w", err)
	}
	stored.Events = events
	stored = mergeEpisodeForExport(stored, episode)
	var prompts []telemetryPromptCall
	if len(promptCalls) > 0 {
		prompts = promptCalls[0]
	}
	spans, score, err := e.buildLangfuseSpans(ctx, stored, episodeDir, prompts)
	if err != nil {
		return err
	}
	if err := e.exportSpansWithRetry(ctx, spans); err != nil {
		return err
	}
	return e.createScoreWithRetry(ctx, score)
}

func (e *EpisodeExporter) exportSpansWithRetry(ctx context.Context, spans []langfuse.Span) error {
	if len(spans) == 0 {
		return nil
	}
	maxRetry := e.cfg.MaxRetryOrDefault()
	pending := spans
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		lastErr = nil
		for start := 0; start < len(pending); start += langfuseBatchSize {
			end := start + langfuseBatchSize
			if end > len(pending) {
				end = len(pending)
			}
			if err := e.client.ExportSpans(ctx, pending[start:end]); err != nil {
				lastErr = err
				// Spans are immutable once ingested, so only the chunks that were
				// not accepted are retried.
				pending = pending[start:]
				break
			}
		}
		if lastErr == nil {
			return nil
		}
		var rejected *langfuse.RejectedSpansError
		if errors.As(lastErr, &rejected) {
			return lastErr
		}
	}
	return lastErr
}

func (e *EpisodeExporter) createScoreWithRetry(ctx context.Context, score langfuse.Score) error {
	if strings.TrimSpace(score.Name) == "" {
		return nil
	}
	maxRetry := e.cfg.MaxRetryOrDefault()
	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		if err := e.client.CreateScore(ctx, score); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func mergeEpisodeForExport(stored TaskEpisode, supplied TaskEpisode) TaskEpisode {
	if strings.TrimSpace(stored.ID) == "" {
		stored.ID = supplied.ID
	}
	if strings.TrimSpace(stored.Status) == "" {
		stored.Status = supplied.Status
	}
	if strings.TrimSpace(stored.StartedAt) == "" {
		stored.StartedAt = supplied.StartedAt
	}
	if strings.TrimSpace(stored.EndedAt) == "" {
		stored.EndedAt = supplied.EndedAt
	}
	if strings.TrimSpace(stored.UserGoal) == "" {
		stored.UserGoal = supplied.UserGoal
	}
	if len(stored.NormalizedGoal) == 0 {
		stored.NormalizedGoal = cloneStringMap(supplied.NormalizedGoal)
	}
	if len(stored.DeviceScope) == 0 {
		stored.DeviceScope = cloneStringMap(supplied.DeviceScope)
	}
	if len(stored.Tags) == 0 {
		stored.Tags = append([]string(nil), supplied.Tags...)
	} else if len(supplied.Tags) > 0 {
		stored.Tags = uniqueNonEmpty(append(stored.Tags, supplied.Tags...))
	}
	if len(stored.Entities) == 0 {
		stored.Entities = append([]string(nil), supplied.Entities...)
	} else if len(supplied.Entities) > 0 {
		stored.Entities = uniqueNonEmpty(append(stored.Entities, supplied.Entities...))
	}
	if len(stored.Events) == 0 {
		stored.Events = append([]TaskEpisodeEvent(nil), supplied.Events...)
	}
	if stored.Extra == nil && supplied.Extra != nil {
		stored.Extra = map[string]interface{}{}
	}
	for key, value := range supplied.Extra {
		stored.Extra[key] = value
	}
	return stored
}

func (e *EpisodeExporter) uploadPromptMedia(ctx context.Context, traceID string, call telemetryPromptCall) telemetryPromptCall {
	for _, media := range call.Media {
		if !e.cfg.UploadScreenshotsOrDefault() {
			replaceTelemetryMediaPlaceholder(call.Input, media.Placeholder, "[media omitted: upload disabled]")
			continue
		}
		replacement := "[media omitted: upload unavailable]"
		uploadCtx, cancel, ok := langfuseScreenshotUploadContext(ctx, e.cfg.UploadTimeoutOrDefault())
		if ok {
			mediaID, err := e.client.UploadMedia(uploadCtx, traceID, call.ID, media.ContentType, media.Data, "input")
			cancel()
			if err != nil {
				if e.logger != nil {
					e.logger.Warn("[telemetry] prompt media upload failed (%s, %d bytes): %v", media.ContentType, len(media.Data), err)
				}
			} else if mediaID != "" {
				replacement = langfuse.MediaToken(media.ContentType, mediaID)
			}
		}
		replaceTelemetryMediaPlaceholder(call.Input, media.Placeholder, replacement)
	}
	call.Media = nil
	return call
}

func replaceTelemetryMediaPlaceholder(value interface{}, placeholder, replacement string) {
	switch typed := value.(type) {
	case []map[string]interface{}:
		for _, item := range typed {
			replaceTelemetryMediaPlaceholder(item, placeholder, replacement)
		}
	case []interface{}:
		for _, item := range typed {
			replaceTelemetryMediaPlaceholder(item, placeholder, replacement)
		}
	case map[string]interface{}:
		for key, item := range typed {
			if text, ok := item.(string); ok && text == placeholder {
				typed[key] = replacement
				continue
			}
			replaceTelemetryMediaPlaceholder(item, placeholder, replacement)
		}
	}
}

func langfuseScreenshotUploadContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return nil, nil, false
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= langfuseTraceIngestReserve {
			return nil, nil, false
		}
		if available := remaining - langfuseTraceIngestReserve; available < timeout {
			timeout = available
		}
	}
	if timeout <= 0 {
		return nil, nil, false
	}
	screenshotCtx, cancel := context.WithTimeout(ctx, timeout)
	return screenshotCtx, cancel, true
}

func episodeTokenUsage(episode TaskEpisode) (promptTokens, completionTokens, totalTokens int, ok bool) {
	if episode.Extra == nil {
		return 0, 0, 0, false
	}
	if v, found := util.UsageMetricInt(episode.Extra["prompt_tokens"]); found {
		promptTokens = v
	}
	if v, found := util.UsageMetricInt(episode.Extra["completion_tokens"]); found {
		completionTokens = v
	}
	if v, found := util.UsageMetricInt(episode.Extra["total_tokens"]); found {
		totalTokens = v
	}
	if totalTokens == 0 && (promptTokens > 0 || completionTokens > 0) {
		totalTokens = promptTokens + completionTokens
	}
	return promptTokens, completionTokens, totalTokens, promptTokens > 0 || completionTokens > 0 || totalTokens > 0
}

func langfuseToolParentSpan(iterationSpanID, phaseSpanID string) string {
	if iterationSpanID != "" {
		return iterationSpanID
	}
	if phaseSpanID != "" {
		return phaseSpanID
	}
	return iterationSpanID
}

func metadataDurationMS(metadata map[string]interface{}, key string) (int64, bool) {
	if len(metadata) == 0 {
		return 0, false
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return 0, false
	}
	var duration int64
	switch v := value.(type) {
	case int:
		duration = int64(v)
	case int8:
		duration = int64(v)
	case int16:
		duration = int64(v)
	case int32:
		duration = int64(v)
	case int64:
		duration = v
	case uint:
		duration = int64(v)
	case uint8:
		duration = int64(v)
	case uint16:
		duration = int64(v)
	case uint32:
		duration = int64(v)
	case uint64:
		if v > uint64(1<<63-1) {
			return 0, false
		}
		duration = int64(v)
	case float32:
		duration = int64(v)
	case float64:
		duration = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			floatValue, floatErr := v.Float64()
			if floatErr != nil {
				return 0, false
			}
			parsed = int64(floatValue)
		}
		duration = parsed
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			floatValue, floatErr := strconv.ParseFloat(trimmed, 64)
			if floatErr != nil {
				return 0, false
			}
			parsed = int64(floatValue)
		}
		duration = parsed
	default:
		return 0, false
	}
	if duration < 0 {
		return 0, false
	}
	return duration, true
}

func metadataString(metadata map[string]interface{}, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	value, ok := metadata[key]
	if !ok || value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func langfusePhaseWindows(events []TaskEpisodeEvent, startTime, endTime time.Time) []langfuseIterationWindow {
	windows := []langfuseIterationWindow{{
		Index: 1,
		ID:    telemetryObservationID(),
		Start: startTime,
		End:   endTime,
	}}
	for _, event := range events {
		if event.Type != "loop_phase" {
			continue
		}
		eventTime := parseEpisodeTime(event.Ts, startTime)
		if len(windows) > 0 {
			windows[len(windows)-1].End = eventTime
		}
		windows = append(windows, langfuseIterationWindow{
			Index: len(windows) + 1,
			ID:    telemetryObservationID(),
			Start: eventTime,
			End:   endTime,
		})
	}
	for i := range windows {
		if windows[i].End.IsZero() {
			windows[i].End = endTime
		}
		if windows[i].End.Before(windows[i].Start) {
			windows[i].End = windows[i].Start
		}
	}
	return windows
}

func langfuseIterationWindows(events []TaskEpisodeEvent, startTime, endTime time.Time) []langfuseIterationWindow {
	if langfuseHasIterationStartEvents(events) {
		return langfuseIterationTimingWindows(events, startTime, endTime)
	}
	return langfusePlannerIterationWindows(events, startTime, endTime)
}

func langfusePlannerIterationWindows(events []TaskEpisodeEvent, startTime, endTime time.Time) []langfuseIterationWindow {
	windows := []langfuseIterationWindow{}
	for _, event := range events {
		if event.Type != "planner_decision" {
			continue
		}
		eventTime := parseEpisodeTime(event.Ts, startTime)
		if len(windows) > 0 {
			windows[len(windows)-1].End = eventTime
		}
		windows = append(windows, langfuseIterationWindow{
			Index: len(windows) + 1,
			ID:    telemetryObservationID(),
			Start: eventTime,
			End:   endTime,
		})
	}
	for i := range windows {
		if windows[i].End.IsZero() {
			windows[i].End = endTime
		}
		if windows[i].End.Before(windows[i].Start) {
			windows[i].End = windows[i].Start
		}
	}
	return windows
}

func langfuseIterationTimingWindows(events []TaskEpisodeEvent, startTime, endTime time.Time) []langfuseIterationWindow {
	windows := []langfuseIterationWindow{}
	for _, event := range events {
		eventTime := parseEpisodeTime(event.Ts, startTime)
		switch event.Type {
		case runEventIterationStart:
			if len(windows) > 0 && windows[len(windows)-1].End.IsZero() {
				windows[len(windows)-1].End = eventTime
			}
			windows = append(windows, langfuseIterationWindow{
				Index: taskEpisodeEventIterationIndex(event, len(windows)+1),
				ID:    telemetryObservationID(),
				Start: eventTime,
			})
		case runEventIterationEnd:
			if len(windows) == 0 {
				continue
			}
			match := -1
			if index := taskEpisodeEventIterationIndex(event, 0); index > 0 {
				for i := len(windows) - 1; i >= 0; i-- {
					if windows[i].Index == index {
						match = i
						break
					}
				}
			}
			if match < 0 {
				match = len(windows) - 1
			}
			windows[match].End = eventTime
		}
	}
	for i := range windows {
		if windows[i].End.IsZero() {
			windows[i].End = endTime
		}
		if windows[i].End.Before(windows[i].Start) {
			windows[i].End = windows[i].Start
		}
	}
	return windows
}

func langfuseHasIterationTimingEvents(events []TaskEpisodeEvent) bool {
	for _, event := range events {
		if event.Type == runEventIterationStart || event.Type == runEventIterationEnd {
			return true
		}
	}
	return false
}

func langfuseHasIterationStartEvents(events []TaskEpisodeEvent) bool {
	for _, event := range events {
		if event.Type == runEventIterationStart {
			return true
		}
	}
	return false
}

func iterationWindowForIndex(windows []langfuseIterationWindow, index int, fallback time.Time) langfuseIterationWindow {
	if index > 0 && index <= len(windows) {
		return windows[index-1]
	}
	return langfuseIterationWindow{Index: index, ID: telemetryObservationID(), Start: fallback, End: fallback}
}

func iterationWindowForEvent(windows []langfuseIterationWindow, eventTime time.Time) (langfuseIterationWindow, bool) {
	for i := len(windows) - 1; i >= 0; i-- {
		window := windows[i]
		if timeWithinWindow(eventTime, window) {
			return window, true
		}
	}
	return langfuseIterationWindow{}, false
}

func taskEpisodeEventIterationIndex(event TaskEpisodeEvent, fallback int) int {
	if event.Metadata != nil {
		if value, ok := util.UsageMetricInt(event.Metadata["iteration"]); ok && value > 0 {
			return value
		}
	}
	return fallback
}

type pendingLangfuseToolCall struct {
	Index         int
	ToolName      string
	EventID       string
	ObservationID string
}

func langfuseToolPairs(events []TaskEpisodeEvent, startTime time.Time) (map[int]langfuseToolPair, map[int]langfuseToolPair) {
	byCall := map[int]langfuseToolPair{}
	byResult := map[int]langfuseToolPair{}
	pending := []pendingLangfuseToolCall{}
	for index, event := range events {
		switch event.Type {
		case runEventToolCall:
			pending = append(pending, pendingLangfuseToolCall{
				Index:         index,
				ToolName:      strings.TrimSpace(event.ToolName),
				EventID:       event.EventID,
				ObservationID: telemetryObservationID(),
			})
		case "tool_result":
			resultID := telemetryObservationID()
			matchIndex := -1
			toolName := strings.TrimSpace(event.ToolName)
			for i, call := range pending {
				if call.ToolName == toolName {
					matchIndex = i
					break
				}
			}
			if matchIndex < 0 && len(pending) > 0 {
				matchIndex = 0
			}
			if matchIndex >= 0 {
				call := pending[matchIndex]
				pending = append(pending[:matchIndex], pending[matchIndex+1:]...)
				pair := langfuseToolPair{
					CallObservationID:   call.ObservationID,
					ResultObservationID: resultID,
					CallEventID:         call.EventID,
					ResultEventID:       event.EventID,
					ResultTime:          parseEpisodeTime(event.Ts, startTime),
					ResultDurationMs:    cloneInt64Ptr(event.DurationMs),
					HasCall:             true,
					HasResult:           true,
				}
				byCall[call.Index] = pair
				byResult[index] = pair
			} else {
				byResult[index] = langfuseToolPair{
					ResultObservationID: resultID,
					ResultEventID:       event.EventID,
					ResultTime:          parseEpisodeTime(event.Ts, startTime),
					ResultDurationMs:    cloneInt64Ptr(event.DurationMs),
					HasResult:           true,
				}
			}
		}
	}
	for _, call := range pending {
		byCall[call.Index] = langfuseToolPair{
			CallObservationID: call.ObservationID,
			CallEventID:       call.EventID,
			HasCall:           true,
		}
	}
	return byCall, byResult
}

func cloneInt64Ptr(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func promptParentObservationID(call telemetryPromptCall, iterations, phases []langfuseIterationWindow) string {
	if id := promptParentFromWindows(call, iterations); id != "" {
		return id
	}
	return promptParentFromWindows(call, phases)
}

func promptParentFromWindows(call telemetryPromptCall, windows []langfuseIterationWindow) string {
	if len(windows) == 0 {
		return ""
	}
	startedAt := call.StartedAt.UTC()
	endedAt := call.EndedAt.UTC()
	if endedAt.IsZero() {
		endedAt = startedAt
	}
	for _, window := range windows {
		if timeWithinWindow(startedAt, window) || timeWithinWindow(endedAt, window) {
			return window.ID
		}
	}
	best := windows[0]
	bestDistance := promptWindowDistance(endedAt, best)
	for _, window := range windows[1:] {
		if distance := promptWindowDistance(endedAt, window); distance < bestDistance {
			best = window
			bestDistance = distance
		}
	}
	return best.ID
}

func timeWithinWindow(ts time.Time, window langfuseIterationWindow) bool {
	if ts.IsZero() {
		return false
	}
	return (ts.Equal(window.Start) || ts.After(window.Start)) && (ts.Equal(window.End) || ts.Before(window.End))
}

func promptWindowDistance(ts time.Time, window langfuseIterationWindow) time.Duration {
	if ts.IsZero() {
		return 0
	}
	if timeWithinWindow(ts, window) {
		return 0
	}
	if ts.Before(window.Start) {
		return window.Start.Sub(ts)
	}
	return ts.Sub(window.End)
}

func successScoreComment(episode TaskEpisode) string {
	if episode.Outcome.Success {
		return firstNonEmptyString([]string{episode.Outcome.VerifierReason, "task completed"})
	}
	return firstNonEmptyString([]string{episode.Outcome.FailureReason, episode.Outcome.VerifierReason, "task failed"})
}

func traceUserID(episode TaskEpisode) string {
	if userID := extraString(episode.Extra, "user_id"); userID != "" {
		return userID
	}
	if episode.DeviceScope != nil {
		return strings.TrimSpace(episode.DeviceScope["device_id"])
	}
	return ""
}

func traceRuntimeID(episode TaskEpisode) string {
	if runtimeID := extraString(episode.Extra, "runtime_id"); runtimeID != "" {
		return runtimeID
	}
	if legacySessionID := extraString(episode.Extra, "session_id"); legacySessionID != "" {
		return legacySessionID
	}
	return extraString(episode.Extra, "telemetry_session_id")
}

func episodeModelParameters(episode TaskEpisode) map[string]interface{} {
	if episode.Extra == nil {
		return nil
	}
	if raw, ok := episode.Extra["model_parameters"]; ok {
		if params := normalizeModelParameters(raw); len(params) > 0 {
			return params
		}
	}
	params := map[string]interface{}{}
	if v, ok := costMetricFloat(episode.Extra["temperature"]); ok && v != 0 {
		params["temperature"] = v
	}
	if v, ok := util.UsageMetricInt(episode.Extra["max_tokens"]); ok && v > 0 {
		params["max_tokens"] = v
	}
	if len(params) == 0 {
		return nil
	}
	return params
}

func normalizeModelParameters(raw interface{}) map[string]interface{} {
	switch typed := raw.(type) {
	case map[string]interface{}:
		return typed
	case map[string]string:
		out := make(map[string]interface{}, len(typed))
		for key, value := range typed {
			out[key] = value
		}
		return out
	default:
		return nil
	}
}

func episodeCostDetails(episode TaskEpisode) map[string]float64 {
	if episode.Extra == nil {
		return nil
	}
	if raw, ok := episode.Extra["cost_details"]; ok {
		if costs := normalizeCostDetails(raw); len(costs) > 0 {
			return costs
		}
	}
	return telemetryCostDetailsFromMap(episode.Extra)
}

func normalizeCostDetails(raw interface{}) map[string]float64 {
	switch typed := raw.(type) {
	case map[string]float64:
		return typed
	case map[string]interface{}:
		out := map[string]float64{}
		for key, value := range typed {
			if v, ok := costMetricFloat(value); ok {
				out[key] = v
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func (e *EpisodeExporter) uploadScreenshot(ctx context.Context, traceID, observationID, episodeDir, screenshotRef string) (string, error) {
	path := filepath.Join(episodeDir, filepath.FromSlash(screenshotRef))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	contentType := screenshotContentType(path)
	mediaID, err := e.client.UploadMedia(ctx, traceID, observationID, contentType, data, "output")
	if err != nil {
		return "", err
	}
	return langfuse.MediaToken(contentType, mediaID), nil
}

func (e *EpisodeExporter) traceTags(episode TaskEpisode) []string {
	tags := append([]string(nil), e.cfg.Tags...)
	tags = append(tags, episode.Tags...)
	if status := strings.TrimSpace(episode.Status); status != "" {
		tags = append(tags, "status:"+status)
		if status == "interrupted" {
			tags = append(tags, "interrupted")
		}
	}
	if model := extraString(episode.Extra, "model"); model != "" {
		tags = append(tags, "model:"+model)
	}
	if episode.Outcome.Success {
		tags = append(tags, "success")
	} else {
		tags = append(tags, "failure")
	}
	tags = append(tags, episodeLoopTags(episode.Events)...)
	return uniqueNonEmpty(tags)
}

func (e *EpisodeExporter) traceMetadata(episode TaskEpisode, prompts []telemetryPromptCall) map[string]interface{} {
	meta := map[string]interface{}{
		"episode_id":       episode.ID,
		"status":           episode.Status,
		"started_at":       episode.StartedAt,
		"ended_at":         episode.EndedAt,
		"normalized_goal":  episode.NormalizedGoal,
		"device_scope":     episode.DeviceScope,
		"failure_reason":   episode.Outcome.FailureReason,
		"verifier_reason":  episode.Outcome.VerifierReason,
		"final_state":      episode.Outcome.FinalState,
		"entities":         episode.Entities,
		"reusable_lessons": episode.ReusableLessons,
		"failure_causes":   episode.FailureCauses,
	}
	if len(episode.RetrievedMemoryRefs) > 0 {
		meta["retrieved_memory_refs"] = episode.RetrievedMemoryRefs
	}
	for key, value := range episodeDerivedMetrics(episode.Events) {
		meta[key] = value
	}

	// Add LLM call statistics by role
	if len(prompts) > 0 {
		byRole := make(map[string][]int64)
		for _, call := range prompts {
			role := strings.TrimSpace(call.Role)
			if role == "" {
				role = "unknown"
			}
			duration := call.EndedAt.Sub(call.StartedAt).Milliseconds()
			byRole[role] = append(byRole[role], duration)
		}

		for role, durations := range byRole {
			if len(durations) == 0 {
				continue
			}
			sorted := make([]int64, len(durations))
			copy(sorted, durations)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

			meta[role+"_call_count"] = len(durations)
			meta[role+"_call_ms_avg"] = avgInt64(durations)
			meta[role+"_call_ms_p50"] = percentileInt64(sorted, 0.5)
			meta[role+"_call_ms_p95"] = percentileInt64(sorted, 0.95)
		}
	}

	// Add prompt cache hit rate from episode.Extra
	if episode.Extra != nil {
		if promptTokens, ok := util.UsageMetricInt(episode.Extra["prompt_tokens"]); ok && promptTokens > 0 {
			if cachedTokens, ok := util.UsageMetricInt(episode.Extra["cached_prompt_tokens"]); ok && cachedTokens > 0 {
				meta["prompt_cache_hit_rate"] = float64(cachedTokens) / float64(promptTokens)
				meta["cached_prompt_tokens"] = cachedTokens
			}
		}
	}

	if episode.Extra != nil {
		for key, value := range episode.Extra {
			meta[key] = value
		}
	}
	return meta
}

func episodeDerivedMetrics(events []TaskEpisodeEvent) map[string]interface{} {
	metrics := map[string]interface{}{}
	toolCounts := map[string]int{}
	var toolLatencies []int64
	var startTime time.Time
	if len(events) > 0 {
		startTime = parseEpisodeTime(events[0].Ts, time.Now().UTC())
	}
	toolPairsByCall, _ := langfuseToolPairs(events, startTime)
	phaseTransitions := []string{}
	finalPhase := "default"
	for index, event := range events {
		switch event.Type {
		case "loop_phase":
			metrics["loop_phase_count"] = intMetric(metrics, "loop_phase_count") + 1
			phase := strings.TrimSpace(event.Content)
			reason := strings.TrimSpace(event.Reason)
			if phase != "" {
				finalPhase = phase
				if reason != "" {
					phaseTransitions = append(phaseTransitions, phase+":"+reason)
				} else {
					phaseTransitions = append(phaseTransitions, phase)
				}
			}
			switch reason {
			case "enter_plan_mode":
				metrics["enter_plan_mode_count"] = intMetric(metrics, "enter_plan_mode_count") + 1
			case "commit_plan":
				metrics["commit_plan_count"] = intMetric(metrics, "commit_plan_count") + 1
			case "cancel_plan":
				metrics["cancel_plan_count"] = intMetric(metrics, "cancel_plan_count") + 1
			case "plan_exhausted":
				metrics["plan_exhausted_count"] = intMetric(metrics, "plan_exhausted_count") + 1
			}
		case "default_finish":
			metrics["default_finish"] = true
			metrics["loop_mode"] = "default"
		case "planner_decision":
			metrics["iteration_count"] = intMetric(metrics, "iteration_count") + 1
			metrics["loop_mode"] = "committed"
		case "candidate_answer":
			metrics["candidate_answer_count"] = intMetric(metrics, "candidate_answer_count") + 1
		case runEventToolCall:
			metrics["tool_call_count"] = intMetric(metrics, "tool_call_count") + 1
			if strings.EqualFold(strings.TrimSpace(event.Role), "agent") {
				metrics["planner_tool_call_count"] = intMetric(metrics, "planner_tool_call_count") + 1
			} else if strings.EqualFold(strings.TrimSpace(event.Role), "executor") {
				metrics["executor_tool_call_count"] = intMetric(metrics, "executor_tool_call_count") + 1
			}
			toolName := strings.TrimSpace(event.ToolName)
			if toolName == "" {
				toolName = "tool"
			}
			toolCounts[toolName]++
			if pair, ok := toolPairsByCall[index]; ok && pair.HasResult {
				if latency, ok := toolPairLatencyMs(event, pair, startTime); ok {
					toolLatencies = append(toolLatencies, latency)
				}
			}
		case "tool_result":
			metrics["tool_result_count"] = intMetric(metrics, "tool_result_count") + 1
			if event.IsError {
				metrics["tool_error_count"] = intMetric(metrics, "tool_error_count") + 1
			}
			if strings.TrimSpace(event.ScreenshotRef) != "" {
				metrics["screenshot_count"] = intMetric(metrics, "screenshot_count") + 1
			}
		case "verifier_decision":
			metrics["verifier_decision_count"] = intMetric(metrics, "verifier_decision_count") + 1
			if event.NeedsReplan {
				metrics["replan_count"] = intMetric(metrics, "replan_count") + 1
			}
		}
	}
	if len(toolCounts) > 0 {
		metrics["tool_counts"] = toolCounts
	}
	if len(toolLatencies) > 0 {
		var total int64
		max := toolLatencies[0]
		for _, value := range toolLatencies {
			total += value
			if value > max {
				max = value
			}
		}
		metrics["tool_latency_ms_avg"] = float64(total) / float64(len(toolLatencies))
		metrics["tool_latency_ms_max"] = max

		// Add percentiles
		sorted := make([]int64, len(toolLatencies))
		copy(sorted, toolLatencies)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		metrics["tool_latency_ms_p50"] = percentileInt64(sorted, 0.5)
		metrics["tool_latency_ms_p95"] = percentileInt64(sorted, 0.95)
		metrics["tool_latency_ms_p99"] = percentileInt64(sorted, 0.99)
	}

	// Add tool latency by type
	toolLatenciesByType := make(map[string][]int64)
	for index, event := range events {
		if event.Type == runEventToolCall {
			if pair, ok := toolPairsByCall[index]; ok && pair.HasResult {
				toolName := strings.TrimSpace(event.ToolName)
				if toolName == "" {
					toolName = "unknown"
				}
				if latency, ok := toolPairLatencyMs(event, pair, startTime); ok {
					toolLatenciesByType[toolName] = append(toolLatenciesByType[toolName], latency)
				}
			}
		}
	}
	if len(toolLatenciesByType) > 0 {
		toolStats := make(map[string]interface{})
		for toolName, latencies := range toolLatenciesByType {
			sorted := make([]int64, len(latencies))
			copy(sorted, latencies)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

			toolStats[toolName] = map[string]interface{}{
				"count": len(latencies),
				"avg":   avgInt64(latencies),
				"p50":   percentileInt64(sorted, 0.5),
				"p95":   percentileInt64(sorted, 0.95),
				"max":   sorted[len(sorted)-1],
			}
		}
		metrics["tool_latency_by_type"] = toolStats
	}

	// Add memory retrieve timing
	for _, event := range events {
		if event.Type == runEventMemoryRetrieve && event.DurationMs != nil {
			metrics["memory_retrieve_ms"] = *event.DurationMs
			break
		}
	}

	// Add session begin timing
	for _, event := range events {
		if event.Type == runEventSessionBegin && event.DurationMs != nil {
			metrics["session_begin_ms"] = *event.DurationMs
			break
		}
	}

	// Add iteration timing statistics
	var iterationDurations []int64
	for _, event := range events {
		if event.Type == runEventIterationEnd && event.DurationMs != nil {
			iterationDurations = append(iterationDurations, *event.DurationMs)
		}
	}
	if len(iterationDurations) > 0 {
		sorted := make([]int64, len(iterationDurations))
		copy(sorted, iterationDurations)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

		metrics["iteration_durations_ms"] = iterationDurations
		metrics["iteration_ms_avg"] = avgInt64(iterationDurations)
		metrics["iteration_ms_p50"] = percentileInt64(sorted, 0.5)
		metrics["iteration_ms_p95"] = percentileInt64(sorted, 0.95)
		metrics["iteration_ms_p99"] = percentileInt64(sorted, 0.99)
	}

	if len(phaseTransitions) > 0 {
		metrics["phase_transitions"] = phaseTransitions
	}
	metrics["final_phase"] = finalPhase
	return metrics
}

func toolPairLatencyMs(callEvent TaskEpisodeEvent, pair langfuseToolPair, startTime time.Time) (int64, bool) {
	if pair.ResultDurationMs != nil && *pair.ResultDurationMs >= 0 {
		return *pair.ResultDurationMs, true
	}
	start := parseEpisodeTime(callEvent.Ts, startTime)
	if pair.ResultTime.Before(start) {
		return 0, false
	}
	return pair.ResultTime.Sub(start).Milliseconds(), true
}

func episodeLoopTags(events []TaskEpisodeEvent) []string {
	tags := []string{}
	hasDefaultFinish := false
	hasCommit := false
	hasReplan := false
	for _, event := range events {
		switch event.Type {
		case "default_finish":
			hasDefaultFinish = true
		case "planner_decision":
			hasCommit = true
		case "loop_phase":
			switch strings.TrimSpace(event.Reason) {
			case "enter_plan_mode":
				tags = append(tags, "loop:plan")
			case "commit_plan":
				tags = append(tags, "loop:execution")
			case "cancel_plan":
				tags = append(tags, "loop:cancelled")
			case "plan_exhausted":
				tags = append(tags, "loop:exhausted")
			}
		case "verifier_decision":
			if event.NeedsReplan {
				hasReplan = true
			}
		}
	}
	if hasDefaultFinish {
		tags = append(tags, "loop:default_finish")
	}
	if hasCommit {
		tags = append(tags, "loop:committed")
	}
	if hasReplan {
		tags = append(tags, "loop:replan")
	}
	return tags
}

func intMetric(values map[string]interface{}, key string) int {
	if raw, ok := values[key]; ok {
		if v, ok := raw.(int); ok {
			return v
		}
	}
	return 0
}

func traceReleaseFromEpisode(episode TaskEpisode) string {
	if v := extraString(episode.Extra, "agent_commit"); v != "" {
		return v
	}
	return extraString(episode.Extra, "firmware_version")
}

func traceVersionFromEpisode(episode TaskEpisode) string {
	if v := extraString(episode.Extra, "agent_build"); v != "" {
		return v
	}
	return extraString(episode.Extra, "firmware_version")
}

func extraString(extra map[string]interface{}, key string) string {
	if extra == nil {
		return ""
	}
	raw, ok := extra[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func parseEpisodeTime(raw string, fallback time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC()
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC()
	}
	return fallback
}

func eventObjectiveInput(event TaskEpisodeEvent) map[string]interface{} {
	return map[string]interface{}{
		"objective":           event.Objective,
		"completion_criteria": event.CompletionCriteria,
		"observed_state":      event.ObservedState,
	}
}

func plannerOutput(event TaskEpisodeEvent) map[string]interface{} {
	return map[string]interface{}{
		"plan":      event.Plan,
		"next_step": event.NextStep,
		"reason":    event.Reason,
	}
}

func toolCallInput(event TaskEpisodeEvent) map[string]interface{} {
	input := map[string]interface{}{
		"tool_name": event.ToolName,
	}
	if strings.TrimSpace(event.ToolInput) != "" {
		input["tool_input"] = event.ToolInput
	}
	if strings.TrimSpace(event.Content) != "" {
		input["content"] = event.Content
	}
	return input
}

func verifierOutput(event TaskEpisodeEvent) map[string]interface{} {
	return map[string]interface{}{
		"content":        event.Content,
		"can_finish":     event.CanFinish,
		"needs_replan":   event.NeedsReplan,
		"reason":         event.Reason,
		"observed_state": event.ObservedState,
	}
}

func screenshotContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func avgInt64(values []int64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum int64
	for _, v := range values {
		sum += v
	}
	return float64(sum) / float64(len(values))
}

func percentileInt64(sortedValues []int64, p float64) int64 {
	if len(sortedValues) == 0 {
		return 0
	}
	if p <= 0 {
		return sortedValues[0]
	}
	if p >= 1 {
		return sortedValues[len(sortedValues)-1]
	}
	index := int(float64(len(sortedValues)-1) * p)
	return sortedValues[index]
}

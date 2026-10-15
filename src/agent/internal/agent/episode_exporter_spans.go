package agent

// Mapping from committed task episodes to Langfuse observations.
//
// Every observation is assembled here with its final timings and attributes and
// exported once as a complete OTLP span: Langfuse treats ingested spans as
// immutable, so create/update pairs are not used.

import (
	"aiden-agent/internal/agent/langfuse"
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Telemetry roles tag the model calls made during a run so captured
// generations can be named after the operation that produced them.
const (
	telemetryRoleAgent      = "agent"
	telemetryRoleCompaction = "compaction"
)

const (
	langfuseRunSpanName       = "agent-run"
	langfuseIterationSpanName = "agent-iteration"
)

// telemetryGenerationNames maps a model call's role to the generation name
// shown in Langfuse. Names describe a stable operation, never a single call, so
// evaluators and dashboards keep matching across runs.
var telemetryGenerationNames = map[string]string{
	telemetryRoleAgent:      "agent-response",
	telemetryRoleCompaction: "summarize-context",
}

func telemetryGenerationName(role string) string {
	if name, ok := telemetryGenerationNames[strings.TrimSpace(role)]; ok {
		return name
	}
	return "llm-response"
}

func telemetryToolSpanName(toolName string) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "tool"
	}
	return toolName
}

func langfuseEventLevel(event TaskEpisodeEvent) string {
	if event.IsError {
		return langfuse.LevelError
	}
	return ""
}

// buildLangfuseSpans turns a committed episode into the observations of one
// trace plus the run's outcome score.
func (e *EpisodeExporter) buildLangfuseSpans(ctx context.Context, episode TaskEpisode, episodeDir string, promptCalls ...[]telemetryPromptCall) ([]langfuse.Span, langfuse.Score, error) {
	startTime := parseEpisodeTime(episode.StartedAt, time.Now().UTC())
	endTime := parseEpisodeTime(episode.EndedAt, startTime)
	version := traceVersionFromEpisode(episode)
	iterations := langfuseIterationWindows(episode.Events, startTime, endTime)
	toolPairsByCall, toolPairsByResult := langfuseToolPairs(episode.Events, startTime)

	var prompts []telemetryPromptCall
	if len(promptCalls) > 0 {
		prompts = promptCalls[0]
	}

	traceID := telemetryTraceID(episode.ID)
	builder := newTelemetrySpanBuilder(traceID, langfuse.TraceContext{
		Name:        langfuseTraceName,
		UserID:      traceUserID(episode),
		SessionID:   traceRuntimeID(episode),
		Release:     traceReleaseFromEpisode(episode),
		Version:     version,
		Environment: e.cfg.EnvironmentOrDefault(),
		Tags:        e.traceTags(episode),
		Metadata:    e.traceMetadata(episode, prompts),
	})

	// The root observation carries the overall goal and answer; Langfuse derives
	// the trace input and output from it.
	runSpanID := builder.add(langfuse.Span{
		Name:      langfuseRunSpanName,
		Type:      langfuse.TypeAgent,
		StartTime: startTime,
		EndTime:   endTime,
		Input:     episode.UserGoal,
		Output:    episode.Outcome.FinalAnswer,
	})

	iterationSpanID := ""
	iterationIndex := 0
	phaseSpanID := ""
	phaseWindowIndex := 0
	currentPhase := "default"
	phaseWindows := langfusePhaseWindows(episode.Events, startTime, endTime)
	iterationSpansCreated := map[string]bool{}
	hasIterationTiming := langfuseHasIterationTimingEvents(episode.Events)

	openPhaseSpan := func(phase string, spanStart time.Time, spanEnd time.Time, metadata map[string]interface{}) {
		if phaseSpanID != "" {
			builder.end(phaseSpanID, spanStart)
		}
		phase = strings.TrimSpace(phase)
		if phase == "" {
			phase = "unknown"
		}
		currentPhase = phase
		if phaseWindowIndex < len(phaseWindows) {
			phaseSpanID = phaseWindows[phaseWindowIndex].ID
			if spanEnd.IsZero() {
				spanEnd = phaseWindows[phaseWindowIndex].End
			}
		} else {
			phaseSpanID = telemetryObservationID()
			if spanEnd.IsZero() {
				spanEnd = endTime
			}
		}
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
		metadata["phase"] = phase
		builder.add(langfuse.Span{
			SpanID:       phaseSpanID,
			ParentSpanID: runSpanID,
			Name:         "phase/" + phase,
			Type:         langfuse.TypeSpan,
			StartTime:    spanStart,
			EndTime:      spanEnd,
			Metadata:     metadata,
		})
		phaseWindowIndex++
	}
	openPhaseSpan("default", startTime, time.Time{}, nil)

	openIterationSpan := func(iteration langfuseIterationWindow, metadata map[string]interface{}) {
		if iteration.ID == "" || iterationSpansCreated[iteration.ID] {
			return
		}
		if metadata == nil {
			metadata = map[string]interface{}{}
		}
		metadata["iteration"] = iteration.Index
		builder.add(langfuse.Span{
			SpanID:       iteration.ID,
			ParentSpanID: firstNonEmptyString([]string{phaseSpanID, runSpanID}),
			Name:         langfuseIterationSpanName,
			Type:         langfuse.TypeSpan,
			StartTime:    iteration.Start,
			EndTime:      iteration.End,
			Metadata:     metadata,
		})
		iterationSpansCreated[iteration.ID] = true
	}

	appendTimedVoiceSpan := func(event TaskEpisodeEvent, eventTime time.Time, spanName string) {
		duration := int64(0)
		if event.DurationMs != nil && *event.DurationMs >= 0 {
			duration = *event.DurationMs
		}
		builder.add(langfuse.Span{
			ParentSpanID: firstNonEmptyString([]string{phaseSpanID, runSpanID}),
			Name:         spanName,
			Type:         langfuse.TypeSpan,
			StartTime:    eventTime,
			EndTime:      eventTime.Add(time.Duration(duration) * time.Millisecond),
			Output:       event.Content,
			Metadata:     telemetryEventMetadata(event, map[string]interface{}{"phase": currentPhase}),
			Level:        langfuseEventLevel(event),
			StatusMsg:    strings.TrimSpace(event.Reason),
		})
	}

	appendSTTDetailSpans := func(event TaskEpisodeEvent, eventTime time.Time, parentDurationMs int64, parentID string) {
		if parentID == "" {
			return
		}
		parentEnd := eventTime.Add(time.Duration(parentDurationMs) * time.Millisecond)
		if parentEnd.Before(eventTime) {
			parentEnd = eventTime
		}
		oneShotMS, hasOneShotMS := metadataDurationMS(event.Metadata, "one_shot_ms")
		oneShotErr := metadataString(event.Metadata, "one_shot_error")
		oneShotStart := parentEnd
		if hasOneShotMS {
			oneShotStart = parentEnd.Add(-time.Duration(oneShotMS) * time.Millisecond)
			if oneShotStart.Before(eventTime) {
				oneShotStart = eventTime
			}
		}
		finalizeMS, hasFinalizeMS := metadataDurationMS(event.Metadata, "streaming_finalize_ms")
		finalizeErr := metadataString(event.Metadata, "streaming_finalize_error")
		finalizeStart := oneShotStart
		if !hasOneShotMS {
			finalizeStart = parentEnd
		}
		if hasFinalizeMS {
			finalizeStart = finalizeStart.Add(-time.Duration(finalizeMS) * time.Millisecond)
			if finalizeStart.Before(eventTime) {
				finalizeStart = eventTime
			}
		}
		audioCaptureEnd := finalizeStart
		if !hasFinalizeMS && !hasOneShotMS {
			audioCaptureEnd = parentEnd
		}

		appendChild := func(name string, spanStart, spanEnd time.Time, extra map[string]interface{}, statusMessage string) {
			if spanEnd.Before(spanStart) {
				spanEnd = spanStart
			}
			metadata := telemetryEventMetadata(event, map[string]interface{}{"phase": currentPhase})
			for key, value := range extra {
				metadata[key] = value
			}
			span := langfuse.Span{
				ParentSpanID: parentID,
				Name:         name,
				Type:         langfuse.TypeSpan,
				StartTime:    spanStart,
				EndTime:      spanEnd,
				Metadata:     metadata,
				StatusMsg:    statusMessage,
			}
			if event.Content != "" {
				span.Output = event.Content
			}
			if statusMessage != "" {
				span.Level = langfuse.LevelError
			}
			builder.add(span)
		}

		if audioDurationMS, ok := metadataDurationMS(event.Metadata, "audio_duration_ms"); ok {
			spanEnd := audioCaptureEnd
			if spanEnd.Before(eventTime) || spanEnd.After(parentEnd) {
				spanEnd = parentEnd
			}
			spanStart := spanEnd.Add(-time.Duration(audioDurationMS) * time.Millisecond)
			if spanStart.Before(eventTime) {
				spanStart = eventTime
			}
			if spanStart.After(eventTime) {
				residualMS := spanStart.Sub(eventTime).Milliseconds()
				appendChild("stt/listening_overhead", eventTime, spanStart, map[string]interface{}{
					"step":        "listening_overhead",
					"duration_ms": residualMS,
					"estimated":   true,
				}, "")
			}
			appendChild("stt/audio_capture", spanStart, spanEnd, map[string]interface{}{
				"step":        "audio_capture",
				"duration_ms": audioDurationMS,
			}, "")
		}

		if readyMS, ok := metadataDurationMS(event.Metadata, "streaming_ready_ms"); ok || metadataString(event.Metadata, "streaming_unavailable_error") != "" {
			spanEnd := eventTime
			if ok {
				spanEnd = eventTime.Add(time.Duration(readyMS) * time.Millisecond)
				if spanEnd.After(parentEnd) {
					spanEnd = parentEnd
				}
			}
			appendChild("stt/streaming_setup", eventTime, spanEnd, map[string]interface{}{
				"step":        "streaming_setup",
				"duration_ms": readyMS,
			}, metadataString(event.Metadata, "streaming_unavailable_error"))
		}

		if hasFinalizeMS || finalizeErr != "" {
			spanEnd := parentEnd
			if hasOneShotMS {
				spanEnd = oneShotStart
			}
			spanStart := spanEnd
			if hasFinalizeMS {
				spanStart = spanEnd.Add(-time.Duration(finalizeMS) * time.Millisecond)
				if spanStart.Before(eventTime) {
					spanStart = eventTime
				}
			}
			appendChild("stt/streaming_finalize", spanStart, spanEnd, map[string]interface{}{
				"step":        "streaming_finalize",
				"duration_ms": finalizeMS,
			}, finalizeErr)
		}

		if hasOneShotMS || oneShotErr != "" {
			spanStart := oneShotStart
			if !hasOneShotMS {
				spanStart = parentEnd
			}
			appendChild("stt/one_shot", spanStart, parentEnd, map[string]interface{}{
				"step":        "one_shot",
				"duration_ms": oneShotMS,
			}, oneShotErr)
		}

		if uploadErr := metadataString(event.Metadata, "streaming_upload_error"); uploadErr != "" {
			appendChild("stt/streaming_upload", eventTime, eventTime, map[string]interface{}{
				"step": "streaming_upload",
			}, uploadErr)
		}
	}

	for eventIndex, event := range episode.Events {
		eventTime := parseEpisodeTime(event.Ts, startTime)
		switch event.Type {
		case "loop_phase":
			openPhaseSpan(event.Content, eventTime, time.Time{}, telemetryEventMetadata(event, map[string]interface{}{
				"reason":     strings.TrimSpace(event.Reason),
				"event_type": event.Type,
			}))

		case "default_finish":
			builder.add(langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{iterationSpanID, phaseSpanID, runSpanID}),
				Name:         "agent/default_finish",
				Type:         langfuse.TypeSpan,
				StartTime:    eventTime,
				EndTime:      eventTime,
				Output:       event.Content,
				Metadata:     telemetryEventMetadata(event, nil),
			})

		case runEventMemoryRetrieve:
			duration := int64(0)
			if event.DurationMs != nil {
				duration = *event.DurationMs
			}
			builder.add(langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{phaseSpanID, runSpanID}),
				Name:         "memory/retrieve",
				Type:         langfuse.TypeRetriever,
				StartTime:    eventTime,
				EndTime:      eventTime.Add(time.Duration(duration) * time.Millisecond),
				Metadata:     event.Metadata,
			})

		case runEventSessionBegin:
			duration := int64(0)
			if event.DurationMs != nil {
				duration = *event.DurationMs
			}
			builder.add(langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{phaseSpanID, runSpanID}),
				Name:         "session/begin",
				Type:         langfuse.TypeSpan,
				StartTime:    eventTime,
				EndTime:      eventTime.Add(time.Duration(duration) * time.Millisecond),
				Metadata:     event.Metadata,
			})

		case runEventSTTTranscription:
			duration := int64(0)
			if event.DurationMs != nil && *event.DurationMs >= 0 {
				duration = *event.DurationMs
			}
			sttSpanID := builder.add(langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{phaseSpanID, runSpanID}),
				Name:         "stt/transcription",
				Type:         langfuse.TypeSpan,
				StartTime:    eventTime,
				EndTime:      eventTime.Add(time.Duration(duration) * time.Millisecond),
				Output:       event.Content,
				Metadata:     telemetryEventMetadata(event, map[string]interface{}{"phase": currentPhase}),
				Level:        langfuseEventLevel(event),
				StatusMsg:    strings.TrimSpace(event.Reason),
			})
			appendSTTDetailSpans(event, eventTime, duration, sttSpanID)

		case runEventVoicePromptSound:
			appendTimedVoiceSpan(event, eventTime, "voice/prompt_sound_agent_send")

		case runEventTTSStreamPreopen:
			appendTimedVoiceSpan(event, eventTime, "voice/preopen_tts_stream")

		case runEventIterationStart:
			iteration, ok := iterationWindowForEvent(iterations, eventTime)
			if !ok {
				continue
			}
			iterationSpanID = iteration.ID
			iterationIndex = iteration.Index
			openIterationSpan(iteration, telemetryEventMetadata(event, map[string]interface{}{
				"event_type": event.Type,
			}))

		case runEventIterationEnd:
			if iteration, ok := iterationWindowForEvent(iterations, eventTime); ok && iteration.ID == iterationSpanID {
				iterationSpanID = ""
			}

		case "planner_decision":
			if hasIterationTiming {
				iteration, ok := iterationWindowForEvent(iterations, eventTime)
				if !ok {
					iteration = iterationWindowForIndex(iterations, iterationIndex, eventTime)
				}
				iterationSpanID = iteration.ID
				iterationIndex = iteration.Index
				openIterationSpan(iteration, telemetryEventMetadata(event, map[string]interface{}{
					"event_type": event.Type,
				}))
			} else {
				iterationIndex++
				iteration := iterationWindowForIndex(iterations, iterationIndex, eventTime)
				iterationSpanID = iteration.ID
				openIterationSpan(iteration, telemetryEventMetadata(event, map[string]interface{}{
					"event_type": event.Type,
				}))
			}

			builder.add(langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{iterationSpanID, runSpanID}),
				Name:         "planner",
				Type:         langfuse.TypeSpan,
				StartTime:    eventTime,
				EndTime:      eventTime,
				Input:        eventObjectiveInput(event),
				Output:       plannerOutput(event),
				Metadata:     telemetryEventMetadata(event, map[string]interface{}{"reason": event.Reason}),
			})

		case "candidate_answer":
			if iterationSpanID == "" {
				continue
			}
			builder.add(langfuse.Span{
				ParentSpanID: iterationSpanID,
				Name:         "candidate_answer",
				Type:         langfuse.TypeEvent,
				StartTime:    eventTime,
				EndTime:      eventTime,
				Output:       event.Content,
				Metadata:     telemetryEventMetadata(event, nil),
			})

		case runEventToolCall:
			parentID := langfuseToolParentSpan(iterationSpanID, phaseSpanID)
			if parentID == "" {
				parentID = runSpanID
			}
			pair, paired := toolPairsByCall[eventIndex]
			spanStart := eventTime
			end := eventTime
			if paired && pair.HasResult {
				end = pair.ResultTime
				if pair.ResultDurationMs != nil && *pair.ResultDurationMs >= 0 {
					spanStart = end.Add(-time.Duration(*pair.ResultDurationMs) * time.Millisecond)
				}
			}
			metadata := telemetryEventMetadata(event, map[string]interface{}{
				"tool_name":       event.ToolName,
				"has_tool_result": paired && pair.HasResult,
				"phase":           currentPhase,
			})
			if paired && pair.ResultEventID != "" {
				metadata["result_event_id"] = pair.ResultEventID
			}
			if paired && pair.ResultDurationMs != nil {
				metadata["duration_ms"] = *pair.ResultDurationMs
			}
			builder.add(langfuse.Span{
				SpanID:       pair.CallObservationID,
				ParentSpanID: parentID,
				Name:         telemetryToolSpanName(event.ToolName),
				Type:         langfuse.TypeTool,
				StartTime:    spanStart,
				EndTime:      end,
				Input:        toolCallInput(event),
				Metadata:     metadata,
			})

		case "tool_result":
			pair, paired := toolPairsByResult[eventIndex]
			var output interface{} = event.Content
			if e.cfg.UploadScreenshotsOrDefault() && strings.TrimSpace(event.ScreenshotRef) != "" {
				screenshotCtx, cancel, ok := langfuseScreenshotUploadContext(ctx, e.cfg.UploadTimeoutOrDefault())
				if ok {
					mediaRef, err := e.uploadScreenshot(screenshotCtx, traceID, pair.CallObservationID, episodeDir, event.ScreenshotRef)
					cancel()
					if err != nil && e.logger != nil {
						e.logger.Warn("[telemetry] screenshot upload failed (%s): %v", event.ScreenshotRef, err)
					} else if mediaRef != "" {
						output = map[string]interface{}{
							"observation": event.Content,
							"screenshot":  mediaRef,
						}
					}
				}
			}
			metadata := telemetryEventMetadata(event, map[string]interface{}{
				"tool_name": event.ToolName,
				"is_error":  event.IsError,
				"phase":     currentPhase,
			})
			if paired && pair.CallEventID != "" {
				metadata["tool_call_event_id"] = pair.CallEventID
			}
			if event.DurationMs != nil {
				metadata["duration_ms"] = *event.DurationMs
			}
			statusMessage := ""
			level := ""
			if event.IsError {
				statusMessage = truncateForLog(event.Content, 500)
				level = langfuse.LevelError
			}
			// The result completes the tool observation that requested it. A result
			// whose call is missing from the episode becomes its own observation.
			if paired && pair.HasCall && pair.CallObservationID != "" && builder.update(pair.CallObservationID, func(span *langfuse.Span) {
				span.Output = output
				span.EndTime = eventTime
				if span.Metadata == nil {
					span.Metadata = map[string]interface{}{}
				}
				for key, value := range metadata {
					span.Metadata[key] = value
				}
				if level != "" {
					span.Level = level
					span.StatusMsg = statusMessage
				}
			}) {
				continue
			}
			builder.add(langfuse.Span{
				SpanID:       pair.ResultObservationID,
				ParentSpanID: firstNonEmptyString([]string{iterationSpanID, phaseSpanID, runSpanID}),
				Name:         telemetryToolSpanName(event.ToolName),
				Type:         langfuse.TypeTool,
				StartTime:    eventTime,
				EndTime:      eventTime,
				Output:       output,
				Metadata:     metadata,
				Level:        level,
				StatusMsg:    statusMessage,
			})

		case "verifier_decision":
			span := langfuse.Span{
				ParentSpanID: firstNonEmptyString([]string{iterationSpanID, runSpanID}),
				Name:         "verifier",
				Type:         langfuse.TypeSpan,
				StartTime:    eventTime,
				EndTime:      eventTime,
				Output:       verifierOutput(event),
				Metadata: telemetryEventMetadata(event, map[string]interface{}{
					"can_finish":   event.CanFinish,
					"needs_replan": event.NeedsReplan,
					"reason":       event.Reason,
				}),
			}
			builder.add(span)
			if event.CanFinish != nil && *event.CanFinish {
				iterationSpanID = ""
			}
		}
	}
	if phaseSpanID != "" {
		builder.end(phaseSpanID, endTime)
	}

	if len(prompts) > 0 {
		for index := range prompts {
			if prompts[index].ID == "" {
				prompts[index].ID = telemetryObservationID()
			}
			prompts[index] = e.uploadPromptMedia(ctx, traceID, prompts[index])
			call := prompts[index]
			parentID := promptParentObservationID(call, iterations, phaseWindows)
			if parentID == "" {
				parentID = runSpanID
			}
			builder.add(e.promptGenerationSpan(episode, call, index, parentID))
		}
	} else if usageSpan := e.traceUsageGenerationSpan(episode, startTime, endTime, runSpanID); usageSpan != nil {
		builder.add(*usageSpan)
	}

	return builder.spansList(), e.successScore(episode, traceID), nil
}

func (e *EpisodeExporter) promptGenerationSpan(episode TaskEpisode, call telemetryPromptCall, index int, parentObservationID string) langfuse.Span {
	if call.EndedAt.IsZero() {
		call.EndedAt = call.StartedAt
	}
	metadata := map[string]interface{}{
		"role":         call.Role,
		"prompt_index": index + 1,
	}
	for key, value := range call.Metadata {
		metadata[key] = value
	}
	if inputTokens, ok := call.UsageDetails["input"]; ok && inputTokens > 0 {
		if cachedTokens, ok := call.UsageDetails["cached"]; ok && cachedTokens > 0 {
			metadata["cache_hit_rate"] = float64(cachedTokens) / float64(inputTokens)
		}
	}
	modelParameters := call.ModelParameters
	if len(modelParameters) == 0 {
		modelParameters = episodeModelParameters(episode)
	}
	span := langfuse.Span{
		SpanID:          call.ID,
		ParentSpanID:    parentObservationID,
		Name:            telemetryGenerationName(call.Role),
		Type:            langfuse.TypeGeneration,
		StartTime:       call.StartedAt,
		EndTime:         call.EndedAt,
		Input:           call.Input,
		Output:          call.Output,
		Metadata:        metadata,
		ModelParameters: modelParameters,
		Usage:           call.UsageDetails,
		Cost:            call.CostDetails,
		Model:           extraString(episode.Extra, "model"),
		StatusMsg:       call.Error,
	}
	if call.Error != "" {
		span.Level = langfuse.LevelError
	}
	if completionStart, ok := telemetryCompletionStart(call); ok {
		span.CompletionStartTime = completionStart
	}
	return span
}

// telemetryCompletionStart derives the time the model started streaming content
// from the provider's time-to-first-token metric, when reported.
func telemetryCompletionStart(call telemetryPromptCall) (time.Time, bool) {
	timeToFirstContentMS, ok := metadataDurationMS(call.Metadata, "llm_time_to_first_content_ms")
	if !ok || timeToFirstContentMS <= 0 {
		return time.Time{}, false
	}
	start := call.StartedAt.Add(time.Duration(timeToFirstContentMS) * time.Millisecond)
	if start.After(call.EndedAt) {
		return time.Time{}, false
	}
	return start, true
}

// traceUsageGenerationSpan reports episode-level token usage when individual
// model calls were not captured. The aggregate is marked as such in metadata so
// it is not mistaken for a single model invocation.
func (e *EpisodeExporter) traceUsageGenerationSpan(episode TaskEpisode, startTime, endTime time.Time, parentID string) *langfuse.Span {
	promptTokens, completionTokens, totalTokens, ok := episodeTokenUsage(episode)
	if !ok {
		return nil
	}
	return &langfuse.Span{
		ParentSpanID:    parentID,
		Name:            "aiden-run-usage",
		Type:            langfuse.TypeGeneration,
		StartTime:       startTime,
		EndTime:         endTime,
		Input:           episode.UserGoal,
		Output:          episode.Outcome.FinalAnswer,
		Model:           extraString(episode.Extra, "model"),
		ModelParameters: episodeModelParameters(episode),
		Usage: map[string]int{
			"input":  promptTokens,
			"output": completionTokens,
			"total":  totalTokens,
		},
		Cost:     episodeCostDetails(episode),
		Metadata: map[string]interface{}{"usage_source": "episode.extra"},
	}
}

func (e *EpisodeExporter) successScore(episode TaskEpisode, traceID string) langfuse.Score {
	score := langfuse.Score{
		ID:          uuid.NewString(),
		TraceID:     traceID,
		Name:        "success",
		DataType:    "BOOLEAN",
		Comment:     successScoreComment(episode),
		Environment: e.cfg.EnvironmentOrDefault(),
		Metadata: map[string]interface{}{
			"failure_reason":  episode.Outcome.FailureReason,
			"verifier_reason": episode.Outcome.VerifierReason,
			"final_state":     episode.Outcome.FinalState,
		},
	}
	if episode.Outcome.Success {
		score.Value = 1
	}
	return score
}

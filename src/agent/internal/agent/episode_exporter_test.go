package agent

import (
	"aico-agent/internal/agent/langfuse"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTelemetryConfigDefaults(t *testing.T) {
	cfg := TelemetryConfig{}
	if cfg.EnabledOrDefault() {
		t.Fatal("EnabledOrDefault() = true, want false")
	}
	if cfg.ProviderOrDefault() != "langfuse" {
		t.Fatalf("ProviderOrDefault() = %q, want langfuse", cfg.ProviderOrDefault())
	}
	if !cfg.UploadScreenshotsOrDefault() {
		t.Fatal("UploadScreenshotsOrDefault() = false, want true")
	}
	if cfg.UploadTimeoutOrDefault() != 30*time.Second {
		t.Fatalf("UploadTimeoutOrDefault() = %s, want 30s", cfg.UploadTimeoutOrDefault())
	}
	if cfg.MaxRetryOrDefault() != 2 {
		t.Fatalf("MaxRetryOrDefault() = %d, want 2", cfg.MaxRetryOrDefault())
	}
}

func TestConfigValidateTelemetryRequiresBaseURL(t *testing.T) {
	enabled := true
	cfg := Config{
		Model: ModelConfig{Provider: "fake"},
		Telemetry: TelemetryConfig{
			Enabled: &enabled,
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "telemetry.base_url") {
		t.Fatalf("Validate() = %v, want telemetry.base_url error", err)
	}
}

func TestConfigValidateTelemetryRequiresKeys(t *testing.T) {
	enabled := true
	cfg := Config{
		Model: ModelConfig{Provider: "fake"},
		Telemetry: TelemetryConfig{
			Enabled: &enabled,
			BaseURL: "http://langfuse.test",
		},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "telemetry.public_key") {
		t.Fatalf("Validate() = %v, want telemetry.public_key error", err)
	}

	cfg.Telemetry.PublicKey = "pk-test"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "telemetry.secret_key") {
		t.Fatalf("Validate() = %v, want telemetry.secret_key error", err)
	}
}

func TestBuildLangfuseSpansAddsTraceIdentityAndFailureScore(t *testing.T) {
	start := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:          "ep_failure_001",
		StartedAt:   start.Format(time.RFC3339Nano),
		EndedAt:     start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:    "打开设置",
		DeviceScope: map[string]string{"device_id": "device-a"},
		Outcome: TaskEpisodeOutcome{
			Success:       false,
			FailureReason: "verifier rejected completion",
		},
		Extra: map[string]interface{}{
			"runtime_id": "runtime-a",
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, score, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}

	root := singleLangfuseSpan(t, spans, langfuseRunSpanName)
	if root.Type != langfuse.TypeAgent {
		t.Fatalf("root type = %q, want agent", root.Type)
	}
	if root.Trace.Name != langfuseTraceName {
		t.Fatalf("trace name = %q, want %q", root.Trace.Name, langfuseTraceName)
	}
	if root.Trace.UserID != "device-a" {
		t.Fatalf("trace userId = %q, want device-a", root.Trace.UserID)
	}
	if root.Trace.SessionID != "runtime-a" {
		t.Fatalf("trace sessionId = %q, want runtime-a", root.Trace.SessionID)
	}
	if root.Input != "打开设置" {
		t.Fatalf("root input = %#v, want user goal", root.Input)
	}
	if root.TraceID != telemetryTraceID(episode.ID) {
		t.Fatalf("root trace id = %q, want %q", root.TraceID, telemetryTraceID(episode.ID))
	}
	for _, span := range spans {
		if span.TraceID != root.TraceID {
			t.Fatalf("span %s trace id = %q, want %q", span.Name, span.TraceID, root.TraceID)
		}
		if span.Trace.UserID != "device-a" || span.Trace.SessionID != "runtime-a" {
			t.Fatalf("span %s missing trace context: %#v", span.Name, span.Trace)
		}
	}

	if score.Value != 0 {
		t.Fatalf("failure score value = %v, want 0", score.Value)
	}
	if score.Comment != "verifier rejected completion" {
		t.Fatalf("failure score comment = %q", score.Comment)
	}
	if score.Name != "success" || score.DataType != "BOOLEAN" {
		t.Fatalf("score = %#v, want success BOOLEAN", score)
	}
	if score.TraceID != telemetryTraceID(episode.ID) {
		t.Fatalf("score trace id = %q, want %q", score.TraceID, telemetryTraceID(episode.ID))
	}
}

func TestBuildLangfuseSpansUsesCapturedPromptsForGenerations(t *testing.T) {
	start := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_prompt_capture",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:  "debug prompt capture",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
		Extra: map[string]interface{}{
			"prompt_tokens":     999,
			"completion_tokens": 111,
			"total_tokens":      1110,
			"model":             "openrouter/test-model",
		},
	}
	promptCalls := []telemetryPromptCall{
		{
			ID:        "1111111111111111",
			Role:      "agent",
			StartedAt: start,
			EndedAt:   start.Add(100 * time.Millisecond),
			Input: []map[string]interface{}{
				{
					"role": "system",
					"parts": []map[string]interface{}{
						{"type": "text", "text": "complete planner system prompt"},
					},
				},
				{
					"role": "human",
					"parts": []map[string]interface{}{
						{"type": "text", "text": "complete user prompt"},
					},
				},
			},
			Output: map[string]interface{}{
				"choices": []map[string]interface{}{{"content": "planner output"}},
			},
			UsageDetails: map[string]int{"input": 10, "output": 2, "total": 12},
			CostDetails:  map[string]float64{"total": 0.0012},
			ModelParameters: map[string]interface{}{
				"temperature": 0.2,
				"max_tokens":  128,
			},
			Metadata: map[string]interface{}{
				"tools_count":                  1,
				"llm_time_to_first_content_ms": int64(40),
				"tool_schemas": []map[string]interface{}{
					{
						"type": "function",
						"function": map[string]interface{}{
							"name":        "echo",
							"description": "Echo text.",
							"parameters": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"value": map[string]interface{}{"type": "string"},
								},
								"required": []string{"value"},
							},
						},
					},
				},
			},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir(), promptCalls)
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}

	generation := singleLangfuseSpan(t, spans, "agent-response")
	if generation.Type != langfuse.TypeGeneration {
		t.Fatalf("generation type = %q, want generation", generation.Type)
	}
	if generation.Model != "openrouter/test-model" {
		t.Fatalf("generation model = %q, want openrouter/test-model", generation.Model)
	}
	if _, ok := langfuseSpanByName(spans, "agent_prompt_1"); ok {
		t.Fatal("generation still uses a per-call index name")
	}
	if _, ok := langfuseSpanByName(spans, "aico-run-usage"); ok {
		t.Fatal("aggregate fallback generation emitted despite captured prompts")
	}

	if !generation.CompletionStartTime.Equal(start.Add(40 * time.Millisecond)) {
		t.Fatalf("completion start = %s, want start + 40ms", generation.CompletionStartTime)
	}

	input, ok := generation.Input.([]map[string]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("generation input = %#v, want 2 messages", generation.Input)
	}
	parts, ok := input[0]["parts"].([]map[string]interface{})
	if !ok || len(parts) != 1 || parts[0]["text"] != "complete planner system prompt" {
		t.Fatalf("captured prompt part = %#v", input[0]["parts"])
	}

	if generation.Usage["input"] != 10 || generation.Usage["output"] != 2 || generation.Usage["total"] != 12 {
		t.Fatalf("usage details = %#v, want 10/2/12", generation.Usage)
	}
	if generation.Cost["total"] != 0.0012 {
		t.Fatalf("cost details = %#v, want total cost", generation.Cost)
	}
	if generation.ModelParameters["temperature"] != 0.2 || generation.ModelParameters["max_tokens"] != 128 {
		t.Fatalf("model parameters = %#v, want temperature/max_tokens", generation.ModelParameters)
	}
	if _, ok := generation.ModelParameters["tools_count"]; ok {
		t.Fatalf("model parameters = %#v, did not expect tools_count", generation.ModelParameters)
	}

	metadata := generation.Metadata
	if metadata["role"] != "agent" || metadata["prompt_index"] != 1 {
		t.Fatalf("generation metadata = %#v, want role/prompt_index", metadata)
	}
	if metadata["tools_count"] != 1 {
		t.Fatalf("generation metadata = %#v, want tools_count=1", metadata)
	}
	tools, ok := metadata["tool_schemas"].([]map[string]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("generation metadata.tool_schemas = %#v, want one tool definition", metadata["tool_schemas"])
	}
	function, ok := tools[0]["function"].(map[string]interface{})
	if !ok || function["name"] != "echo" {
		t.Fatalf("tool function = %#v, want echo", tools[0]["function"])
	}
	parameters, ok := function["parameters"].(map[string]interface{})
	if !ok || parameters["type"] != "object" {
		t.Fatalf("tool parameters = %#v, want object schema", function["parameters"])
	}
}

func TestBuildLangfuseSpansExportsSTTTranscriptionSpans(t *testing.T) {
	start := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	durationMs := int64(9000)
	episode := TaskEpisode{
		ID:        "ep_stt_span",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(2 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "打开天气",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
		Events: []TaskEpisodeEvent{
			{
				EventID:    "evt_stt",
				Ts:         start.Add(100 * time.Millisecond).Format(time.RFC3339Nano),
				Type:       runEventSTTTranscription,
				Role:       "system",
				Content:    "打开天气",
				DurationMs: &durationMs,
				Metadata: map[string]interface{}{
					"provider":                  "qwen-asr",
					"model":                     "qwen3-asr-flash-realtime",
					"audio_duration_ms":         int64(1200),
					"streaming_supported":       true,
					"streaming_ready":           true,
					"streaming_ready_ms":        int64(320),
					"streaming_finalize_ms":     int64(90),
					"used_streaming_transcript": true,
					"fallback_one_shot":         true,
					"one_shot_ms":               int64(750),
				},
			},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}

	sttSpan := singleLangfuseSpan(t, spans, "stt/transcription")
	if sttSpan.Output != "打开天气" {
		t.Fatalf("STT output = %#v, want transcript", sttSpan.Output)
	}
	if sttSpan.Metadata["event_id"] != "evt_stt" || sttSpan.Metadata["provider"] != "qwen-asr" || sttSpan.Metadata["fallback_one_shot"] != true {
		t.Fatalf("STT metadata = %#v", sttSpan.Metadata)
	}
	for _, spanName := range []string{"stt/listening_overhead", "stt/audio_capture", "stt/streaming_setup", "stt/streaming_finalize", "stt/one_shot"} {
		detail := singleLangfuseSpan(t, spans, spanName)
		if detail.ParentSpanID != sttSpan.SpanID {
			t.Fatalf("%s parent = %q, want %q", spanName, detail.ParentSpanID, sttSpan.SpanID)
		}
		if detail.Metadata["event_id"] != "evt_stt" {
			t.Fatalf("%s metadata = %#v", spanName, detail.Metadata)
		}
	}
}

func TestBuildLangfuseSpansExportsVoicePreRunSpans(t *testing.T) {
	start := time.Date(2026, 7, 8, 6, 18, 19, 0, time.UTC)
	promptDurationMs := promptSoundDurationMS(promptSoundAgentSend)
	preopenDurationMs := int64(120)
	episode := TaskEpisode{
		ID:        "ep_voice_prerun",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(6 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "天气怎么样",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
		Events: []TaskEpisodeEvent{
			{
				EventID:    "evt_prompt",
				Ts:         start.Add(3500 * time.Millisecond).Format(time.RFC3339Nano),
				Type:       runEventVoicePromptSound,
				Role:       "system",
				Content:    "agent send",
				DurationMs: &promptDurationMs,
				Metadata: map[string]interface{}{
					"prompt":                "agent_send",
					"wait":                  false,
					"async":                 true,
					"cue_audio_duration_ms": promptDurationMs,
					"dispatch_duration_ms":  int64(0),
					"success":               true,
				},
			},
			{
				EventID:    "evt_preopen",
				Ts:         start.Add(5450 * time.Millisecond).Format(time.RFC3339Nano),
				Type:       runEventTTSStreamPreopen,
				Role:       "system",
				Content:    "preopen TTS stream",
				DurationMs: &preopenDurationMs,
				Metadata: map[string]interface{}{
					"provider": "alicloud",
					"success":  true,
				},
			},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}

	promptSpan := singleLangfuseSpan(t, spans, "voice/prompt_sound_agent_send")
	if promptSpan.Output != "agent send" {
		t.Fatalf("prompt span output = %#v", promptSpan.Output)
	}
	if promptSpan.Metadata["prompt"] != "agent_send" || promptSpan.Metadata["success"] != true || promptSpan.Metadata["async"] != true {
		t.Fatalf("prompt metadata = %#v", promptSpan.Metadata)
	}
	if promptSpan.StartTime.Equal(promptSpan.EndTime) {
		t.Fatalf("prompt span has zero duration: %#v", promptSpan)
	}

	preopenSpan := singleLangfuseSpan(t, spans, "voice/preopen_tts_stream")
	if preopenSpan.Metadata["provider"] != "alicloud" || preopenSpan.Metadata["success"] != true {
		t.Fatalf("preopen metadata = %#v", preopenSpan.Metadata)
	}
}

func TestBuildLangfuseSpansUploadsCapturedPromptMedia(t *testing.T) {
	var mediaRequest langfuse.MediaCreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/public/media" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &mediaRequest); err != nil {
			t.Fatalf("decode media request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"mediaId":"prompt-media-1","uploadUrl":null}`))
	}))
	defer server.Close()

	start := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	image := []byte("prompt-jpeg-bytes")
	promptMedia := newTelemetryPromptMedia("image/jpeg", image)
	callID := "1111111111111111"
	promptCalls := []telemetryPromptCall{{
		ID:        callID,
		Role:      "agent",
		StartedAt: start,
		EndedAt:   start.Add(time.Millisecond),
		Input: []map[string]interface{}{{
			"role": "human",
			"parts": []map[string]interface{}{{
				"type":      "binary",
				"mime_type": "image/jpeg",
				"size":      len(image),
				"data":      promptMedia.Placeholder,
			}},
		}},
		Media: []telemetryPromptMedia{promptMedia},
	}}
	episode := TaskEpisode{
		ID:        "ep_prompt_media",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:  "inspect screenshot",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
	}
	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:   boolPtr(true),
		BaseURL:   server.URL,
		PublicKey: "pk-test",
		SecretKey: "sk-test",
	}, nil)

	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir(), promptCalls)
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}
	if len(promptCalls[0].Media) != 0 {
		t.Fatalf("prompt media retained after upload: %d item(s)", len(promptCalls[0].Media))
	}
	if mediaRequest.ObservationID != callID {
		t.Fatalf("media observationId = %q, want %q", mediaRequest.ObservationID, callID)
	}
	if mediaRequest.Field != "input" {
		t.Fatalf("media field = %q, want input", mediaRequest.Field)
	}

	generation := singleLangfuseSpan(t, spans, "agent-response")
	encoded, err := json.Marshal(generation.Input)
	if err != nil {
		t.Fatalf("marshal generation input: %v", err)
	}
	if strings.Contains(string(encoded), base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("generation input contains inline base64: %s", encoded)
	}
	if !strings.Contains(string(encoded), "id=prompt-media-1") {
		t.Fatalf("generation input missing media token: %s", encoded)
	}
}

func TestBuildLangfuseSpansOmitsPromptImagesWhenScreenshotUploadDisabled(t *testing.T) {
	var mediaRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediaRequests++
		http.Error(w, "media upload must be disabled", http.StatusInternalServerError)
	}))
	defer server.Close()

	start := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	image := []byte("prompt-jpeg-bytes")
	promptMedia := newTelemetryPromptMedia("image/jpeg", image)
	pdf := []byte("%PDF-1.7 prompt bytes")
	pdfMedia := newTelemetryPromptMedia("application/pdf", pdf)
	promptCalls := []telemetryPromptCall{{
		ID:        "1111111111111111",
		Role:      "agent",
		StartedAt: start,
		EndedAt:   start.Add(time.Millisecond),
		Input: []map[string]interface{}{{
			"role": "human",
			"parts": []map[string]interface{}{{
				"type":      "binary",
				"mime_type": "image/jpeg",
				"size":      len(image),
				"data":      promptMedia.Placeholder,
			}, {
				"type":      "binary",
				"mime_type": "application/pdf",
				"size":      len(pdf),
				"data":      pdfMedia.Placeholder,
			}},
		}},
		Media: []telemetryPromptMedia{promptMedia, pdfMedia},
	}}
	uploadScreenshots := false
	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:           boolPtr(true),
		BaseURL:           server.URL,
		PublicKey:         "pk-test",
		SecretKey:         "sk-test",
		UploadScreenshots: &uploadScreenshots,
	}, nil)
	episode := TaskEpisode{
		ID:        "ep_prompt_media_disabled",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:  "inspect screenshot",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
	}

	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir(), promptCalls)
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}
	if len(promptCalls[0].Media) != 0 {
		t.Fatalf("prompt media retained when upload is disabled: %d item(s)", len(promptCalls[0].Media))
	}
	if mediaRequests != 0 {
		t.Fatalf("media API requests = %d, want 0", mediaRequests)
	}
	generation := singleLangfuseSpan(t, spans, "agent-response")
	encoded, err := json.Marshal(generation.Input)
	if err != nil {
		t.Fatalf("marshal generation input: %v", err)
	}
	body := string(encoded)
	if strings.Contains(body, promptMedia.Placeholder) {
		t.Fatalf("generation input retained media placeholder: %s", body)
	}
	if strings.Contains(body, pdfMedia.Placeholder) {
		t.Fatalf("generation input retained non-image media placeholder: %s", body)
	}
	if strings.Contains(body, base64.StdEncoding.EncodeToString(image)) {
		t.Fatalf("generation input contains inline base64: %s", body)
	}
	if strings.Contains(body, base64.StdEncoding.EncodeToString(pdf)) {
		t.Fatalf("generation input contains inline non-image base64: %s", body)
	}
	if !strings.Contains(body, "[media omitted: upload disabled]") {
		t.Fatalf("generation input missing disabled placeholder: %s", body)
	}
}

func TestExportEpisodeDirUploadsToLangfuse(t *testing.T) {
	var otlpCalls int
	var scoreCalls int
	var mediaObservationID string
	var otlpBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			otlpCalls++
			if got := r.Header.Get("x-langfuse-ingestion-version"); got != "4" {
				t.Errorf("ingestion version header = %q, want 4", got)
			}
			user, pass, ok := r.BasicAuth()
			if !ok || user != "pk-test" || pass != "sk-test" {
				t.Errorf("unexpected auth: ok=%v user=%q pass=%q", ok, user, pass)
			}
			otlpBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			scoreCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/media":
			body, _ := io.ReadAll(r.Body)
			var req langfuse.MediaCreateRequest
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("decode media create request: %v", err)
			}
			mediaObservationID = req.ObservationID
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"mediaId":"media-123","uploadUrl":null}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	episodeDir := t.TempDir()
	start := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_export_test",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(5 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "打开时钟",
		Outcome: TaskEpisodeOutcome{
			Success:     true,
			FinalAnswer: "done",
		},
	}
	writeEpisodeFixture(t, episodeDir, episode, []TaskEpisodeEvent{
		{
			EventID:  "evt1",
			Ts:       start.Format(time.RFC3339Nano),
			Type:     "planner_decision",
			NextStep: "打开时钟应用",
			Plan:     []string{"打开时钟应用"},
		},
		{
			EventID:   "evt2",
			Ts:        start.Add(time.Second).Format(time.RFC3339Nano),
			Type:      runEventToolCall,
			ToolName:  "screenshot",
			ToolInput: `{}`,
			Content:   "截图",
		},
		{
			EventID:       "evt3",
			Ts:            start.Add(2 * time.Second).Format(time.RFC3339Nano),
			Type:          "tool_result",
			ToolName:      "screenshot",
			Content:       `{"action_output":"ok"}`,
			ScreenshotRef: "artifacts/step_002.jpeg",
		},
	})
	artifactsDir := filepath.Join(episodeDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "step_002.jpeg"), []byte("fakejpeg"), 0o644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}

	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:           boolPtr(true),
		BaseURL:           server.URL,
		PublicKey:         "pk-test",
		SecretKey:         "sk-test",
		UploadScreenshots: boolPtr(true),
		MaxRetry:          0,
	}, nil)
	if err := exporter.ExportEpisodeDir(context.Background(), episodeDir, episode); err != nil {
		t.Fatalf("ExportEpisodeDir() error = %v", err)
	}
	if otlpCalls != 1 {
		t.Fatalf("otlp calls = %d, want 1", otlpCalls)
	}
	if scoreCalls != 1 {
		t.Fatalf("score calls = %d, want 1", scoreCalls)
	}
	if strings.TrimSpace(mediaObservationID) == "" {
		t.Fatal("expected media create request to include observationId")
	}

	spans := decodeOTLPSpans(t, otlpBody)
	tool := otlpSpanByName(t, spans, "screenshot")
	if mediaObservationID != tool.SpanID {
		t.Fatalf("media observationId = %q, want tool span id %q", mediaObservationID, tool.SpanID)
	}
	if got := tool.attributeString(t, "langfuse.observation.type"); got != langfuse.TypeTool {
		t.Fatalf("tool observation type = %q, want tool", got)
	}
	output := tool.jsonObjectAttribute(t, "langfuse.observation.output")
	screenshot, _ := output["screenshot"].(string)
	if !strings.Contains(screenshot, "id=media-123") {
		t.Fatalf("tool output screenshot = %q, want media token", screenshot)
	}
}

func TestExportEpisodeDirExportsTraceWhenScreenshotUploadWouldExhaustDeadline(t *testing.T) {
	var otlpCalls int
	var mediaCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			otlpCalls++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/media":
			mediaCalls++
			time.Sleep(200 * time.Millisecond)
			http.Error(w, "media upload should have been skipped", http.StatusGatewayTimeout)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	episodeDir := t.TempDir()
	start := time.Date(2026, 6, 23, 9, 30, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_export_deadline_test",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(5 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "失败也要上传 trace",
		Outcome: TaskEpisodeOutcome{
			Success:       false,
			FailureReason: "agent restarted before the task episode completed",
		},
	}
	writeEpisodeFixture(t, episodeDir, episode, []TaskEpisodeEvent{
		{
			EventID:   "evt1",
			Ts:        start.Format(time.RFC3339Nano),
			Type:      runEventToolCall,
			ToolName:  "screenshot",
			ToolInput: `{}`,
			Content:   "截图",
		},
		{
			EventID:       "evt2",
			Ts:            start.Add(time.Second).Format(time.RFC3339Nano),
			Type:          "tool_result",
			ToolName:      "screenshot",
			Content:       `{"format":"jpeg","size":100}`,
			ScreenshotRef: "artifacts/step_002.jpeg",
		},
	})
	artifactsDir := filepath.Join(episodeDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		t.Fatalf("mkdir artifacts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "step_002.jpeg"), []byte("fakejpeg"), 0o644); err != nil {
		t.Fatalf("write screenshot: %v", err)
	}

	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:           boolPtr(true),
		BaseURL:           server.URL,
		PublicKey:         "pk-test",
		SecretKey:         "sk-test",
		UploadScreenshots: boolPtr(true),
		MaxRetry:          0,
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := exporter.ExportEpisodeDir(ctx, episodeDir, episode); err != nil {
		t.Fatalf("ExportEpisodeDir() error = %v", err)
	}
	if otlpCalls == 0 {
		t.Fatal("expected otlp request even when screenshot upload cannot fit within export deadline")
	}
	if mediaCalls != 0 {
		t.Fatalf("mediaCalls = %d, want screenshot upload skipped to preserve trace ingestion budget", mediaCalls)
	}
}

func TestLangfuseScreenshotUploadContextUsesConfiguredTimeout(t *testing.T) {
	screenshotCtx, cancel, ok := langfuseScreenshotUploadContext(context.Background(), 120*time.Millisecond)
	if !ok {
		t.Fatal("langfuseScreenshotUploadContext() ok = false, want true")
	}
	defer cancel()
	deadline, ok := screenshotCtx.Deadline()
	if !ok {
		t.Fatal("screenshot context missing deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 120*time.Millisecond {
		t.Fatalf("screenshot context timeout = %s, want <= 120ms", remaining)
	}
}

func TestLangfuseScreenshotUploadContextReservesTraceIngestionBudget(t *testing.T) {
	parentCtx, parentCancel := context.WithTimeout(context.Background(), 2*langfuseTraceIngestReserve)
	defer parentCancel()
	parentDeadline, ok := parentCtx.Deadline()
	if !ok {
		t.Fatal("parent context missing deadline")
	}

	screenshotCtx, cancel, ok := langfuseScreenshotUploadContext(parentCtx, 30*time.Second)
	if !ok {
		t.Fatal("langfuseScreenshotUploadContext() ok = false, want true")
	}
	defer cancel()
	deadline, ok := screenshotCtx.Deadline()
	if !ok {
		t.Fatal("screenshot context missing deadline")
	}
	if deadline.After(parentDeadline.Add(-langfuseTraceIngestReserve + 100*time.Millisecond)) {
		t.Fatalf("screenshot deadline = %s, want trace ingestion reserve before parent deadline %s", deadline, parentDeadline)
	}
}

func TestRuntimeStartupExportsInterruptedEpisodeToLangfuse(t *testing.T) {
	ctx := context.Background()
	configDir := t.TempDir()
	memoryDir := filepath.Join(configDir, "memory")
	store := NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes"))
	recorder := NewPersistentEpisodeRecorder(MemoryRetrieveRequest{
		Input:     "打开设置",
		EpisodeID: "ep_langfuse_interrupted",
	}, MemoryContext{}, store)

	if err := recorder.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	recorder.append(TaskEpisodeEvent{
		Type:      "planner_decision",
		Role:      "agent",
		Objective: "打开设置",
		Plan:      []string{"打开设置"},
		NextStep:  "点击设置",
	})

	scoreCh := make(chan map[string]interface{}, 1)
	var otlpBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			user, pass, ok := r.BasicAuth()
			if !ok || user != "pk-test" || pass != "sk-test" {
				t.Errorf("unexpected auth: ok=%v user=%q pass=%q", ok, user, pass)
			}
			otlpBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			body, _ := io.ReadAll(r.Body)
			var scoreBody map[string]interface{}
			if err := json.Unmarshal(body, &scoreBody); err != nil {
				t.Errorf("decode score body: %v", err)
			}
			select {
			case scoreCh <- scoreBody:
			default:
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	NewRuntimeWithDeps(
		Config{
			ConfigDir: configDir,
			Model: ModelConfig{
				Provider: "fake",
				Model:    "test-model",
			},
			Telemetry: TelemetryConfig{
				Enabled:   boolPtr(true),
				BaseURL:   server.URL,
				PublicKey: "pk-test",
				SecretKey: "sk-test",
				MaxRetry:  0,
			},
		},
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(memoryDir),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	var scoreBody map[string]interface{}
	select {
	case scoreBody = <-scoreCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for langfuse export")
	}

	spans := decodeOTLPSpans(t, otlpBody)
	root := otlpSpanByName(t, spans, langfuseRunSpanName)
	if got := root.attributeString(t, "langfuse.trace.metadata.episode_id"); got != "ep_langfuse_interrupted" {
		t.Fatalf("metadata.episode_id = %q", got)
	}
	if got := root.attributeString(t, "langfuse.trace.metadata.status"); got != "interrupted" {
		t.Fatalf("metadata.status = %q", got)
	}
	if got := root.attributeString(t, "langfuse.trace.metadata.failure_reason"); got != "agent restarted before the task episode completed" {
		t.Fatalf("metadata.failure_reason = %q", got)
	}
	if got := root.attributeString(t, "langfuse.trace.metadata.model"); got != "fake/test-model" {
		t.Fatalf("metadata.model = %q", got)
	}
	if got := root.attributeString(t, "langfuse.trace.metadata.interruption_source"); got != "agent_restart" {
		t.Fatalf("metadata.interruption_source = %q", got)
	}
	if scoreBody["value"] != float64(0) {
		t.Fatalf("score value = %v, want 0", scoreBody["value"])
	}

	tags, ok := root.attribute("langfuse.trace.tags")
	if !ok || tags.ArrayValue == nil {
		t.Fatalf("trace tags = %#v, want array", tags)
	}
	for _, want := range []string{"interrupted", "status:interrupted", "failure"} {
		if !otlpArrayContains(tags.ArrayValue, want) {
			t.Fatalf("trace tags missing %q: %#v", want, tags.ArrayValue.Values)
		}
	}
}

func TestBuildLangfuseSpansMapsDefaultModePlannerTools(t *testing.T) {
	start := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_default_001",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(3 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "echo test",
		Outcome: TaskEpisodeOutcome{
			Success:     true,
			FinalAnswer: "done",
		},
		Events: []TaskEpisodeEvent{
			{
				EventID:   "evt_tool",
				Ts:        start.Add(time.Second).Format(time.RFC3339Nano),
				Type:      runEventToolCall,
				Role:      "agent",
				ToolName:  "echo",
				ToolInput: `{"__arg1":"ok"}`,
			},
			{
				EventID:  "evt_result",
				Ts:       start.Add(2 * time.Second).Format(time.RFC3339Nano),
				Type:     "tool_result",
				Role:     "agent",
				ToolName: "echo",
				Content:  "ok",
			},
			{
				EventID: "evt_finish",
				Ts:      start.Add(3 * time.Second).Format(time.RFC3339Nano),
				Type:    "default_finish",
				Role:    "agent",
				Content: "done",
			},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}

	names := langfuseSpansByName(spans)
	if len(names["phase/default"]) != 1 {
		t.Fatalf("phase/default count = %d, want 1; names=%v", len(names["phase/default"]), langfuseSpanNames(spans))
	}
	if len(names["echo"]) != 1 {
		t.Fatalf("echo count = %d, want 1; names=%v", len(names["echo"]), langfuseSpanNames(spans))
	}
	if len(names["tool_result/echo"]) != 0 {
		t.Fatalf("tool result emitted its own observation: names=%v", langfuseSpanNames(spans))
	}
	if len(names["agent/default_finish"]) != 1 {
		t.Fatalf("agent/default_finish count = %d, want 1; names=%v", len(names["agent/default_finish"]), langfuseSpanNames(spans))
	}

	tool := names["echo"][0]
	if tool.Type != langfuse.TypeTool || tool.Input == nil || tool.Output != "ok" {
		t.Fatalf("tool span = %#v, want paired tool observation with input and output", tool)
	}

	root := singleLangfuseSpan(t, spans, langfuseRunSpanName)
	if root.Trace.Metadata["default_finish"] != true {
		t.Fatalf("trace metadata default_finish = %#v, want true", root.Trace.Metadata["default_finish"])
	}
	if root.Trace.Metadata["loop_mode"] != "default" {
		t.Fatalf("trace metadata loop_mode = %#v, want default", root.Trace.Metadata["loop_mode"])
	}
}

func TestBuildLangfuseSpansUsesIterationTimingSpanAsToolParent(t *testing.T) {
	start := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	toolDuration := int64(100)
	episode := TaskEpisode{
		ID:        "ep_iteration_default",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:  "echo test",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
		Events: []TaskEpisodeEvent{
			{EventID: "evt_iter_start", Ts: start.Format(time.RFC3339Nano), Type: runEventIterationStart, Metadata: map[string]interface{}{"iteration": 1}},
			{EventID: "evt_tool", Ts: start.Add(100 * time.Millisecond).Format(time.RFC3339Nano), Type: runEventToolCall, Role: "agent", ToolName: "echo", ToolInput: `{"__arg1":"ok"}`},
			{EventID: "evt_result", Ts: start.Add(200 * time.Millisecond).Format(time.RFC3339Nano), Type: "tool_result", Role: "agent", ToolName: "echo", Content: "ok", DurationMs: &toolDuration},
			{EventID: "evt_finish", Ts: start.Add(300 * time.Millisecond).Format(time.RFC3339Nano), Type: "default_finish", Role: "agent", Content: "done"},
			{EventID: "evt_iter_end", Ts: start.Add(400 * time.Millisecond).Format(time.RFC3339Nano), Type: runEventIterationEnd, DurationMs: int64Ptr(400), Metadata: map[string]interface{}{"iteration": 1}},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}
	iteration := singleLangfuseSpan(t, spans, langfuseIterationSpanName)
	if iteration.Metadata["iteration"] != 1 {
		t.Fatalf("iteration metadata = %#v, want iteration 1", iteration.Metadata)
	}
	tool := singleLangfuseSpan(t, spans, "echo")
	finish := singleLangfuseSpan(t, spans, "agent/default_finish")
	if tool.ParentSpanID != iteration.SpanID {
		t.Fatalf("tool parent = %q, want iteration id %q", tool.ParentSpanID, iteration.SpanID)
	}
	if finish.ParentSpanID != iteration.SpanID {
		t.Fatalf("finish parent = %q, want iteration id %q", finish.ParentSpanID, iteration.SpanID)
	}
}

func TestBuildLangfuseSpansMarksFailedToolResult(t *testing.T) {
	start := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_tool_error",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(time.Second).Format(time.RFC3339Nano),
		UserGoal:  "echo test",
		Outcome:   TaskEpisodeOutcome{Success: false, FailureReason: "tool failed"},
		Events: []TaskEpisodeEvent{
			{EventID: "evt_tool", Ts: start.Add(100 * time.Millisecond).Format(time.RFC3339Nano), Type: runEventToolCall, Role: "agent", ToolName: "echo", ToolInput: `{"__arg1":"hi"}`},
			{EventID: "evt_result", Ts: start.Add(200 * time.Millisecond).Format(time.RFC3339Nano), Type: "tool_result", Role: "agent", ToolName: "echo", Content: "boom", IsError: true},
		},
	}

	exporter := NewEpisodeExporter(TelemetryConfig{Enabled: boolPtr(true), BaseURL: "http://langfuse.test"}, nil)
	spans, _, err := exporter.buildLangfuseSpans(context.Background(), episode, t.TempDir())
	if err != nil {
		t.Fatalf("buildLangfuseSpans() error = %v", err)
	}
	if len(langfuseSpansByName(spans)["echo"]) != 1 {
		t.Fatalf("echo span count = %d, want 1; names=%v", len(langfuseSpansByName(spans)["echo"]), langfuseSpanNames(spans))
	}
	tool := singleLangfuseSpan(t, spans, "echo")
	if tool.Type != langfuse.TypeTool {
		t.Fatalf("tool type = %q, want tool", tool.Type)
	}
	if tool.Input == nil {
		t.Fatal("tool observation missing input")
	}
	if tool.Output != "boom" {
		t.Fatalf("tool output = %#v, want failure content", tool.Output)
	}
	if tool.Level != langfuse.LevelError {
		t.Fatalf("tool level = %q, want ERROR", tool.Level)
	}
	if tool.StatusMsg != "boom" {
		t.Fatalf("tool status message = %q, want boom", tool.StatusMsg)
	}
}

func TestExportEpisodeDirOTLPPayloadCarriesTraceContext(t *testing.T) {
	var otlpBody []byte
	var scoreCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			if got := r.Header.Get("x-langfuse-ingestion-version"); got != "4" {
				t.Errorf("ingestion version header = %q, want 4", got)
			}
			if user, pass, ok := r.BasicAuth(); !ok || user != "pk-test" || pass != "sk-test" {
				t.Errorf("unexpected auth: ok=%v user=%q pass=%q", ok, user, pass)
			}
			otlpBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			scoreCalls++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	start := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:          "ep_otlp_wire",
		StartedAt:   start.Format(time.RFC3339Nano),
		EndedAt:     start.Add(10 * time.Second).Format(time.RFC3339Nano),
		UserGoal:    "wire the exporter",
		Tags:        []string{"episode-tag"},
		DeviceScope: map[string]string{"device_id": "device-wire"},
		Outcome:     TaskEpisodeOutcome{Success: true, FinalAnswer: "wired"},
		Extra: map[string]interface{}{
			"runtime_id":        "runtime-wire",
			"agent_commit":      "abc1234",
			"agent_build":       "20260701-build",
			"model":             "openrouter/wire-model",
			"prompt_tokens":     3,
			"completion_tokens": 1,
			"total_tokens":      4,
			"model_parameters":  map[string]interface{}{"temperature": 0.1},
			"cost_details":      map[string]float64{"total": 0.0003},
		},
	}
	episodeDir := t.TempDir()
	writeEpisodeFixture(t, episodeDir, episode, []TaskEpisodeEvent{
		{EventID: "evt_mem", Ts: start.Add(100 * time.Millisecond).Format(time.RFC3339Nano), Type: runEventMemoryRetrieve, DurationMs: int64Ptr(30), Metadata: map[string]interface{}{"query": "wire"}},
		{EventID: "evt_plan", Ts: start.Add(200 * time.Millisecond).Format(time.RFC3339Nano), Type: "planner_decision", Role: "agent", Objective: "wire", Plan: []string{"wire"}, NextStep: "wire"},
		{EventID: "evt_tool_call", Ts: start.Add(300 * time.Millisecond).Format(time.RFC3339Nano), Type: runEventToolCall, Role: "agent", ToolName: "echo", ToolInput: `{"__arg1":"hi"}`},
		{EventID: "evt_candidate", Ts: start.Add(350 * time.Millisecond).Format(time.RFC3339Nano), Type: "candidate_answer", Role: "agent", Content: "candidate"},
		{EventID: "evt_tool_result", Ts: start.Add(400 * time.Millisecond).Format(time.RFC3339Nano), Type: "tool_result", Role: "agent", ToolName: "echo", Content: "hi"},
		{EventID: "evt_finish", Ts: start.Add(500 * time.Millisecond).Format(time.RFC3339Nano), Type: "default_finish", Role: "agent", Content: "wired"},
	})
	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:     boolPtr(true),
		BaseURL:     server.URL,
		PublicKey:   "pk-test",
		SecretKey:   "sk-test",
		Environment: "wire-env",
		Tags:        []string{"cfg-tag"},
		MaxRetry:    0,
	}, nil)
	if err := exporter.ExportEpisodeDir(context.Background(), episodeDir, episode); err != nil {
		t.Fatalf("ExportEpisodeDir() error = %v", err)
	}
	if scoreCalls != 1 {
		t.Fatalf("score calls = %d, want 1", scoreCalls)
	}

	spans := decodeOTLPSpans(t, otlpBody)
	ids := map[string]bool{}
	for _, span := range spans {
		if len(span.TraceID) != 32 || !isDashlessHex(span.TraceID) {
			t.Fatalf("span %s trace id = %q, want 32 dashless hex chars", span.Name, span.TraceID)
		}
		if len(span.SpanID) != 16 || !isDashlessHex(span.SpanID) {
			t.Fatalf("span %s span id = %q, want 16 dashless hex chars", span.Name, span.SpanID)
		}
		ids[span.SpanID] = true
	}
	for _, span := range spans {
		for _, key := range []string{
			"langfuse.trace.name",
			"langfuse.user.id",
			"langfuse.session.id",
			"langfuse.release",
			"langfuse.version",
			"langfuse.environment",
			"langfuse.trace.tags",
			"langfuse.trace.metadata.episode_id",
		} {
			if _, ok := span.attribute(key); !ok {
				t.Fatalf("span %s missing trace context attribute %s", span.Name, key)
			}
		}
		if span.Name == langfuseRunSpanName {
			if span.ParentSpanID != "" {
				t.Fatalf("root span parent = %q, want none", span.ParentSpanID)
			}
			continue
		}
		if span.ParentSpanID == "" {
			t.Fatalf("child span %s missing parentSpanId", span.Name)
		}
		if !ids[span.ParentSpanID] {
			t.Fatalf("span %s parent %q is not an exported span", span.Name, span.ParentSpanID)
		}
	}

	first := spans[0]
	if got := first.attributeString(t, "langfuse.trace.name"); got != langfuseTraceName {
		t.Fatalf("trace name = %q, want %q", got, langfuseTraceName)
	}
	if got := first.attributeString(t, "langfuse.user.id"); got != "device-wire" {
		t.Fatalf("user id = %q, want device-wire", got)
	}
	if got := first.attributeString(t, "langfuse.session.id"); got != "runtime-wire" {
		t.Fatalf("session id = %q, want runtime-wire", got)
	}
	if got := first.attributeString(t, "langfuse.release"); got != "abc1234" {
		t.Fatalf("release = %q, want abc1234", got)
	}
	if got := first.attributeString(t, "langfuse.version"); got != "20260701-build" {
		t.Fatalf("version = %q, want 20260701-build", got)
	}
	if got := first.attributeString(t, "langfuse.environment"); got != "wire-env" {
		t.Fatalf("environment = %q, want wire-env", got)
	}
	tags, ok := first.attribute("langfuse.trace.tags")
	if !ok || tags.ArrayValue == nil {
		t.Fatalf("trace tags = %#v, want array", tags)
	}
	for _, want := range []string{"cfg-tag", "episode-tag"} {
		if !otlpArrayContains(tags.ArrayValue, want) {
			t.Fatalf("trace tags missing %q: %#v", want, tags.ArrayValue.Values)
		}
	}

	typeOf := func(name string) string {
		return otlpSpanByName(t, spans, name).attributeString(t, "langfuse.observation.type")
	}
	for name, want := range map[string]string{
		langfuseRunSpanName: "agent",
		"phase/default":     "span",
		"memory/retrieve":   "retriever",
		"echo":              "tool",
		"candidate_answer":  "event",
		"aico-run-usage":    "generation",
	} {
		if got := typeOf(name); got != want {
			t.Fatalf("%s observation type = %q, want %q", name, got, want)
		}
	}

	// The aggregate usage generation stands in for the model calls when none
	// were captured; its structured attributes are JSON-encoded strings.
	generation := otlpSpanByName(t, spans, "aico-run-usage")
	if got := generation.attributeString(t, "langfuse.observation.model.name"); got != "openrouter/wire-model" {
		t.Fatalf("generation model name = %q, want openrouter/wire-model", got)
	}
	parameters := generation.jsonObjectAttribute(t, "langfuse.observation.model.parameters")
	if parameters["temperature"] != 0.1 {
		t.Fatalf("generation model parameters = %#v", parameters)
	}
	usage := generation.jsonObjectAttribute(t, "langfuse.observation.usage_details")
	if usage["input"] != float64(3) || usage["output"] != float64(1) || usage["total"] != float64(4) {
		t.Fatalf("generation usage details = %#v, want 3/1/4", usage)
	}
	cost := generation.jsonObjectAttribute(t, "langfuse.observation.cost_details")
	if cost["total"] != 0.0003 {
		t.Fatalf("generation cost details = %#v", cost)
	}
}

// TestExportEpisodeDirOTLPPayloadCarriesGenerationAttributes checks the wire
// attributes of a captured model call: a stable name plus the model, usage,
// cost, and completion-start attributes Langfuse reads.
func TestExportEpisodeDirOTLPPayloadCarriesGenerationAttributes(t *testing.T) {
	var otlpBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/otel/v1/traces":
			otlpBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/public/scores":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	start := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	episode := TaskEpisode{
		ID:        "ep_otlp_generation",
		StartedAt: start.Format(time.RFC3339Nano),
		EndedAt:   start.Add(2 * time.Second).Format(time.RFC3339Nano),
		UserGoal:  "capture a generation",
		Outcome:   TaskEpisodeOutcome{Success: true, FinalAnswer: "done"},
		Extra:     map[string]interface{}{"model": "openrouter/wire-model"},
	}
	episodeDir := t.TempDir()
	writeEpisodeFixture(t, episodeDir, episode, []TaskEpisodeEvent{
		{EventID: "evt_plan", Ts: start.Add(100 * time.Millisecond).Format(time.RFC3339Nano), Type: "planner_decision", Role: "agent", Objective: "capture", Plan: []string{"capture"}, NextStep: "capture"},
	})
	promptCalls := []telemetryPromptCall{{
		ID:              "2222222222222222",
		Role:            "agent",
		StartedAt:       start.Add(150 * time.Millisecond),
		EndedAt:         start.Add(190 * time.Millisecond),
		Input:           []map[string]interface{}{{"role": "human", "parts": []map[string]interface{}{{"type": "text", "text": "hi"}}}},
		Output:          map[string]interface{}{"choices": []map[string]interface{}{{"content": "hi"}}},
		UsageDetails:    map[string]int{"input": 3, "output": 1, "total": 4},
		CostDetails:     map[string]float64{"total": 0.0003},
		ModelParameters: map[string]interface{}{"temperature": 0.1},
		Metadata:        map[string]interface{}{"llm_time_to_first_content_ms": int64(10)},
	}}

	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:   boolPtr(true),
		BaseURL:   server.URL,
		PublicKey: "pk-test",
		SecretKey: "sk-test",
		MaxRetry:  0,
	}, nil)
	if err := exporter.ExportEpisodeDir(context.Background(), episodeDir, episode, promptCalls); err != nil {
		t.Fatalf("ExportEpisodeDir() error = %v", err)
	}

	generation := otlpSpanByName(t, decodeOTLPSpans(t, otlpBody), "agent-response")
	if got := generation.attributeString(t, "langfuse.observation.type"); got != "generation" {
		t.Fatalf("generation type = %q, want generation", got)
	}
	if got := generation.attributeString(t, "langfuse.observation.model.name"); got != "openrouter/wire-model" {
		t.Fatalf("generation model name = %q, want openrouter/wire-model", got)
	}
	parameters := generation.jsonObjectAttribute(t, "langfuse.observation.model.parameters")
	if parameters["temperature"] != 0.1 {
		t.Fatalf("generation model parameters = %#v", parameters)
	}
	usage := generation.jsonObjectAttribute(t, "langfuse.observation.usage_details")
	if usage["input"] != float64(3) || usage["output"] != float64(1) || usage["total"] != float64(4) {
		t.Fatalf("generation usage details = %#v, want 3/1/4", usage)
	}
	cost := generation.jsonObjectAttribute(t, "langfuse.observation.cost_details")
	if cost["total"] != 0.0003 {
		t.Fatalf("generation cost details = %#v", cost)
	}
	wantCompletionStart := langfuse.RFC3339(start.Add(150 * time.Millisecond).Add(10 * time.Millisecond))
	if got := generation.attributeString(t, "langfuse.observation.completion_start_time"); got != wantCompletionStart {
		t.Fatalf("completion start time = %q, want %q", got, wantCompletionStart)
	}
}

// TestExportSpansWithRetryResendsOnlyFailedChunk drives more spans than fit in
// one batch and fails the second chunk once. The first chunk is accepted and
// must not be re-sent; only the failed chunk is retried.
func TestExportSpansWithRetryResendsOnlyFailedChunk(t *testing.T) {
	spans := make([]langfuse.Span, 0, langfuseBatchSize+5)
	for i := 0; i < langfuseBatchSize+5; i++ {
		spans = append(spans, langfuse.Span{
			TraceID: strings.Repeat("a", 32),
			SpanID:  fmt.Sprintf("%016x", i),
			Name:    fmt.Sprintf("span-%d", i),
		})
	}

	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		request := len(bodies)
		mu.Unlock()
		// The first chunk is accepted; the second chunk fails on its first send
		// and is retried by the exporter.
		if request == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:   boolPtr(true),
		BaseURL:   server.URL,
		PublicKey: "pk-test",
		SecretKey: "sk-test",
	}, nil)
	if err := exporter.exportSpansWithRetry(context.Background(), spans); err != nil {
		t.Fatalf("exportSpansWithRetry() error = %v", err)
	}

	mu.Lock()
	captured := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(captured) != 3 {
		t.Fatalf("otlp request count = %d, want 3", len(captured))
	}

	seen := map[string]int{}
	for _, body := range captured {
		for _, span := range decodeOTLPSpans(t, body) {
			seen[span.SpanID]++
		}
	}
	for i, span := range spans {
		want := 1
		if i >= langfuseBatchSize {
			// The failing chunk is retried, so its spans are sent twice.
			want = 2
		}
		if seen[span.SpanID] != want {
			t.Fatalf("span %s sent %d time(s), want %d", span.Name, seen[span.SpanID], want)
		}
	}
}

// TestExportSpansWithRetryDoesNotRetryRejectedSpans checks that a partial
// success response surfaces an error without re-sending the batch: Langfuse
// already ingested the accepted spans, so a retry would duplicate them.
func TestExportSpansWithRetryDoesNotRetryRejectedSpans(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":1,"errorMessage":"bad span"}}`))
	}))
	defer server.Close()

	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:   boolPtr(true),
		BaseURL:   server.URL,
		PublicKey: "pk-test",
		SecretKey: "sk-test",
	}, nil)
	err := exporter.exportSpansWithRetry(context.Background(), []langfuse.Span{{
		TraceID: strings.Repeat("a", 32),
		SpanID:  strings.Repeat("b", 16),
		Name:    "agent-run",
	}})
	var rejected *langfuse.RejectedSpansError
	if !errors.As(err, &rejected) {
		t.Fatalf("exportSpansWithRetry() error = %v, want RejectedSpansError", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("otlp request count = %d, want 1 (rejected spans must not be retried)", requests)
	}
}

// writeEpisodeFixture writes the on-disk episode metadata and events an
// ExportEpisodeDir call reads before exporting.
func writeEpisodeFixture(t *testing.T, dir string, episode TaskEpisode, events []TaskEpisodeEvent) {
	t.Helper()
	metadata := "id: " + episode.ID + "\n" +
		"status: " + episode.Status + "\n" +
		"started_at: \"" + episode.StartedAt + "\"\n" +
		"ended_at: \"" + episode.EndedAt + "\"\n" +
		"user_goal: " + episode.UserGoal + "\n" +
		"outcome:\n" +
		"  success: " + boolString(episode.Outcome.Success) + "\n" +
		"  final_answer: " + episode.Outcome.FinalAnswer + "\n" +
		"  failure_reason: " + episode.Outcome.FailureReason + "\n"
	if err := os.WriteFile(filepath.Join(dir, "episode.yaml"), []byte(metadata), 0o644); err != nil {
		t.Fatalf("write episode.yaml: %v", err)
	}
	if err := writeEpisodeEventsJSONL(filepath.Join(dir, "events.jsonl"), events); err != nil {
		t.Fatalf("write events.jsonl: %v", err)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func langfuseSpansByName(spans []langfuse.Span) map[string][]langfuse.Span {
	byName := map[string][]langfuse.Span{}
	for _, span := range spans {
		if span.Name == "" {
			continue
		}
		byName[span.Name] = append(byName[span.Name], span)
	}
	return byName
}

func langfuseSpanNames(spans []langfuse.Span) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.Name)
	}
	return names
}

func langfuseSpanByName(spans []langfuse.Span, name string) (langfuse.Span, bool) {
	matches := langfuseSpansByName(spans)[name]
	if len(matches) == 0 {
		return langfuse.Span{}, false
	}
	return matches[0], true
}

func singleLangfuseSpan(t *testing.T, spans []langfuse.Span, name string) langfuse.Span {
	t.Helper()
	matches := langfuseSpansByName(spans)[name]
	if len(matches) != 1 {
		t.Fatalf("%s span count = %d, want 1; spans=%v", name, len(matches), langfuseSpanNames(spans))
	}
	return matches[0]
}

func int64Ptr(value int64) *int64 {
	return &value
}

// otlpSpanPayload and friends decode the OTLP/HTTP JSON body the exporter posts
// so tests can assert on the exact wire representation rather than on the Go
// values that produced it.
type otlpSpanPayload struct {
	TraceID           string                 `json:"traceId"`
	SpanID            string                 `json:"spanId"`
	ParentSpanID      string                 `json:"parentSpanId"`
	Name              string                 `json:"name"`
	Kind              int                    `json:"kind"`
	StartTimeUnixNano string                 `json:"startTimeUnixNano"`
	EndTimeUnixNano   string                 `json:"endTimeUnixNano"`
	Attributes        []otlpAttributePayload `json:"attributes"`
	Status            *otlpStatusPayload     `json:"status"`
}

type otlpAttributePayload struct {
	Key   string           `json:"key"`
	Value otlpValuePayload `json:"value"`
}

type otlpValuePayload struct {
	StringValue string `json:"stringValue"`
	BoolValue   *bool  `json:"boolValue"`
	// Langfuse drops observations whose intValue arrives as a string, so the
	// exporter sends integers as JSON numbers.
	IntValue    *int64            `json:"intValue"`
	DoubleValue *float64          `json:"doubleValue"`
	ArrayValue  *otlpArrayPayload `json:"arrayValue"`
}

type otlpArrayPayload struct {
	Values []otlpValuePayload `json:"values"`
}

type otlpStatusPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type otlpRequestPayload struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []otlpAttributePayload `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Spans []otlpSpanPayload `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

func decodeOTLPSpans(t *testing.T, body []byte) []otlpSpanPayload {
	t.Helper()
	var payload otlpRequestPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode otlp body: %v", err)
	}
	var spans []otlpSpanPayload
	for _, resource := range payload.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			spans = append(spans, scope.Spans...)
		}
	}
	if len(spans) == 0 {
		t.Fatalf("otlp body contained no spans: %s", strings.TrimSpace(string(body)))
	}
	return spans
}

func otlpSpanByName(t *testing.T, spans []otlpSpanPayload, name string) otlpSpanPayload {
	t.Helper()
	var matches []otlpSpanPayload
	for _, span := range spans {
		if span.Name == name {
			matches = append(matches, span)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("%s otlp span count = %d, want 1", name, len(matches))
	}
	return matches[0]
}

func (s otlpSpanPayload) attribute(key string) (otlpValuePayload, bool) {
	for _, attribute := range s.Attributes {
		if attribute.Key == key {
			return attribute.Value, true
		}
	}
	return otlpValuePayload{}, false
}

func (s otlpSpanPayload) attributeString(t *testing.T, key string) string {
	t.Helper()
	value, ok := s.attribute(key)
	if !ok {
		t.Fatalf("%s missing attribute %s", s.Name, key)
	}
	return value.StringValue
}

func (s otlpSpanPayload) jsonObjectAttribute(t *testing.T, key string) map[string]interface{} {
	t.Helper()
	raw := s.attributeString(t, key)
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("%s attribute %s = %q, want JSON object: %v", s.Name, key, raw, err)
	}
	return decoded
}

func otlpArrayContains(array *otlpArrayPayload, want string) bool {
	if array == nil {
		return false
	}
	for _, value := range array.Values {
		if value.StringValue == want {
			return true
		}
	}
	return false
}

func isDashlessHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

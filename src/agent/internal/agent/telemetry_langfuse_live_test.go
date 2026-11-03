package agent

// Opt-in end-to-end check of the episode telemetry against a real Langfuse
// deployment. It exports a synthetic episode through the production exporter and
// then reads the trace back through the Langfuse API, so it verifies ingestion,
// observation types, hierarchy, trace context propagation, and scores rather
// than only the payloads this repository builds.
//
// Run against a local stack (`deploy/langfuse`):
//
//	AICO_LANGFUSE_LIVE=1 \
//	LANGFUSE_BASE_URL=http://localhost:3000 \
//	LANGFUSE_PUBLIC_KEY=pk-lf-... LANGFUSE_SECRET_KEY=sk-lf-... \
//	go test ./internal/agent/ -run TestLangfuseLiveEpisodeExport -v -count=1
import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const langfuseLiveSkipMessage = "set AICO_LANGFUSE_LIVE=1 with LANGFUSE_BASE_URL, LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY to verify against a real Langfuse"

func TestLangfuseLiveEpisodeExport(t *testing.T) {
	if os.Getenv("AICO_LANGFUSE_LIVE") != "1" {
		t.Skip(langfuseLiveSkipMessage)
	}
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("LANGFUSE_BASE_URL")), "/")
	publicKey := strings.TrimSpace(os.Getenv("LANGFUSE_PUBLIC_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("LANGFUSE_SECRET_KEY"))
	if baseURL == "" || publicKey == "" || secretKey == "" {
		t.Fatal("requires LANGFUSE_BASE_URL, LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY")
	}

	episode, episodeDir, promptCalls := langfuseLiveEpisodeFixture(t)
	exporter := NewEpisodeExporter(TelemetryConfig{
		Enabled:           boolPtr(true),
		BaseURL:           baseURL,
		PublicKey:         publicKey,
		SecretKey:         secretKey,
		UploadTimeoutSec:  30,
		UploadScreenshots: boolPtr(true),
		Environment:       "verification",
		Tags:              []string{"aico-live-verification"},
	}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := exporter.ExportEpisodeDir(ctx, episodeDir, episode, promptCalls); err != nil {
		t.Fatalf("ExportEpisodeDir() error = %v", err)
	}

	traceID := telemetryTraceID(episode.ID)
	client := langfuseLiveClient{t: t, baseURL: baseURL, publicKey: publicKey, secretKey: secretKey}
	trace := client.waitForTrace(ctx, traceID)
	t.Logf("trace %s: %s/project/%s/traces/%s", traceID, baseURL, client.projectID(ctx), traceID)

	if got := langfuseLiveString(trace, "name"); got != langfuseTraceName {
		t.Errorf("trace name = %q, want %q", got, langfuseTraceName)
	}
	if got := langfuseLiveString(trace, "userId"); got != "device-live-verify" {
		t.Errorf("trace userId = %q, want device-live-verify", got)
	}
	if got := langfuseLiveString(trace, "sessionId"); got != "runtime-live-verify" {
		t.Errorf("trace sessionId = %q, want runtime-live-verify", got)
	}
	if got := langfuseLiveString(trace, "environment"); got != "verification" {
		t.Errorf("trace environment = %q, want verification", got)
	}
	if got := langfuseLiveString(trace, "version"); got != "live-verify-build" {
		t.Errorf("trace version = %q, want live-verify-build", got)
	}
	if got := langfuseLiveString(trace, "release"); got != "live-verify-commit" {
		t.Errorf("trace release = %q, want live-verify-commit", got)
	}
	if tags := langfuseLiveStrings(trace["tags"]); !langfuseLiveContains(tags, "aico-live-verification") || !langfuseLiveContains(tags, "success") {
		t.Errorf("trace tags = %v, want aico-live-verification and success", tags)
	}
	metadata := langfuseLiveMap(trace["metadata"])
	if metadata["tool_call_count"] != float64(1) {
		t.Errorf("trace metadata tool_call_count = %v, want 1", metadata["tool_call_count"])
	}
	if input := langfuseLiveString(trace, "input"); input != "打开设置并截图" {
		t.Errorf("trace input = %q, want the user goal", input)
	}
	if output := langfuseLiveString(trace, "output"); output != "已打开设置" {
		t.Errorf("trace output = %q, want the final answer", output)
	}

	observations := langfuseLiveObservations(trace)
	byName := map[string]map[string]interface{}{}
	for _, observation := range observations {
		byName[langfuseLiveString(observation, "name")] = observation
	}

	root, ok := byName[langfuseRunSpanName]
	if !ok {
		t.Fatalf("missing %s root observation; got %v", langfuseRunSpanName, langfuseLiveNames(observations))
	}
	if got := langfuseLiveString(root, "type"); got != "AGENT" {
		t.Errorf("root observation type = %q, want AGENT", got)
	}
	if parent := langfuseLiveString(root, "parentObservationId"); parent != "" {
		t.Errorf("root observation parent = %q, want empty", parent)
	}
	rootID := langfuseLiveString(root, "id")

	tool, ok := byName["screenshot"]
	if !ok {
		t.Fatalf("missing screenshot tool observation; got %v", langfuseLiveNames(observations))
	}
	if got := langfuseLiveString(tool, "type"); got != "TOOL" {
		t.Errorf("tool observation type = %q, want TOOL", got)
	}
	if parent := langfuseLiveString(tool, "parentObservationId"); parent == "" || parent == rootID {
		t.Errorf("tool observation parent = %q, want the iteration observation", parent)
	}
	if input := langfuseLiveJSON(tool["input"]); !strings.Contains(input, "jpeg") {
		t.Errorf("tool observation input = %q, want the call arguments", input)
	}
	if output := langfuseLiveJSON(tool["output"]); !strings.Contains(output, "langfuseMedia") {
		t.Errorf("tool observation output = %q, want the screenshot media reference", output)
	}
	if _, exists := byName["tool_result/screenshot"]; exists {
		t.Error("tool results must not be duplicated as a separate observation")
	}

	generation, ok := byName["agent-response"]
	if !ok {
		t.Fatalf("missing agent-response generation; got %v", langfuseLiveNames(observations))
	}
	if got := langfuseLiveString(generation, "type"); got != "GENERATION" {
		t.Errorf("generation type = %q, want GENERATION", got)
	}
	if got := langfuseLiveString(generation, "model"); got != "openrouter/google/gemini-3.5-flash" {
		t.Errorf("generation model = %q, want the configured model", got)
	}
	usage := langfuseLiveMap(generation["usageDetails"])
	if usage["input"] != float64(120) || usage["output"] != float64(30) {
		t.Errorf("generation usageDetails = %v, want 120/30", usage)
	}
	generationMetadata := langfuseLiveMap(generation["metadata"])
	if generationMetadata["role"] != "agent" {
		t.Errorf("generation metadata role = %v, want agent", generationMetadata["role"])
	}
	// Trace context is propagated to every observation. The v3 read API only
	// exposes environment and version per observation; user and session IDs are
	// trace-level fields there, while v4 exposes them on every observation too.
	if got := langfuseLiveString(generation, "environment"); got != "verification" {
		t.Errorf("generation environment = %q, want verification (trace context must be on every observation)", got)
	}
	if got := langfuseLiveString(generation, "version"); got != "live-verify-build" {
		t.Errorf("generation version = %q, want live-verify-build (trace context must be on every observation)", got)
	}
	if got := langfuseLiveString(tool, "environment"); got != "verification" {
		t.Errorf("tool observation environment = %q, want verification", got)
	}

	if _, ok := byName["memory/retrieve"]; !ok {
		t.Errorf("missing memory/retrieve observation; got %v", langfuseLiveNames(observations))
	}

	scores := client.waitForScores(ctx, traceID, "success")
	found := false
	for _, score := range scores {
		if langfuseLiveString(score, "name") != "success" {
			continue
		}
		found = true
		if score["value"] != float64(1) {
			t.Errorf("success score value = %v, want 1", score["value"])
		}
	}
	if !found {
		t.Errorf("missing success score, got %v", scores)
	}
}

// langfuseLiveEpisodeFixture writes an episode to disk and returns it together
// with the captured model calls that belong to the run.
func langfuseLiveEpisodeFixture(t *testing.T) (TaskEpisode, string, []telemetryPromptCall) {
	t.Helper()
	start := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	episodeDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(episodeDir, "artifacts"), 0o755); err != nil {
		t.Fatalf("create artifacts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(episodeDir, "artifacts", "step_1.jpeg"), []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatalf("write screenshot artifact: %v", err)
	}

	toolDuration := int64(400)
	iterationDuration := int64(1500)
	episode := TaskEpisode{
		ID:          "8f1c0f4e-6c2f-4a2e-9f1e-2f6b0f0d3a11",
		Status:      "completed",
		StartedAt:   start.Format(time.RFC3339Nano),
		EndedAt:     start.Add(4 * time.Second).Format(time.RFC3339Nano),
		UserGoal:    "打开设置并截图",
		DeviceScope: map[string]string{"device_id": "device-live-verify"},
		Outcome: TaskEpisodeOutcome{
			Success:        true,
			FinalAnswer:    "已打开设置",
			VerifierReason: "settings page visible",
		},
		Events: []TaskEpisodeEvent{
			{
				EventID:    "evt_session",
				Ts:         start.Format(time.RFC3339Nano),
				Type:       runEventSessionBegin,
				DurationMs: langfuseLiveInt64Ptr(12),
			},
			{
				EventID:    "evt_memory",
				Ts:         start.Add(20 * time.Millisecond).Format(time.RFC3339Nano),
				Type:       runEventMemoryRetrieve,
				DurationMs: langfuseLiveInt64Ptr(35),
				Metadata:   map[string]interface{}{"chunks": 2},
			},
			{
				EventID:  "evt_iter_start",
				Ts:       start.Add(100 * time.Millisecond).Format(time.RFC3339Nano),
				Type:     runEventIterationStart,
				Metadata: map[string]interface{}{"iteration": 1},
			},
			{
				EventID:   "evt_tool_call",
				Ts:        start.Add(300 * time.Millisecond).Format(time.RFC3339Nano),
				Type:      runEventToolCall,
				ToolName:  "screenshot",
				ToolInput: `{"format":"jpeg"}`,
				Role:      "agent",
			},
			{
				EventID:       "evt_tool_result",
				Ts:            start.Add(700 * time.Millisecond).Format(time.RFC3339Nano),
				Type:          "tool_result",
				ToolName:      "screenshot",
				Content:       "screenshot captured",
				ScreenshotRef: "artifacts/step_1.jpeg",
				DurationMs:    &toolDuration,
			},
			{
				EventID:    "evt_iter_end",
				Ts:         start.Add(1500 * time.Millisecond).Format(time.RFC3339Nano),
				Type:       runEventIterationEnd,
				DurationMs: &iterationDuration,
			},
			{
				EventID:   "evt_verifier",
				Ts:        start.Add(1600 * time.Millisecond).Format(time.RFC3339Nano),
				Type:      "verifier_decision",
				CanFinish: boolPtr(true),
				Reason:    "settings page visible",
				Content:   "done",
			},
			{
				EventID: "evt_finish",
				Ts:      start.Add(1700 * time.Millisecond).Format(time.RFC3339Nano),
				Type:    "default_finish",
				Content: "已打开设置",
			},
		},
		Extra: map[string]interface{}{
			"runtime_id":        "runtime-live-verify",
			"model":             "openrouter/google/gemini-3.5-flash",
			"agent_build":       "live-verify-build",
			"agent_commit":      "live-verify-commit",
			"prompt_tokens":     120,
			"completion_tokens": 30,
			"total_tokens":      150,
		},
	}

	meta, err := yaml.Marshal(episode)
	if err != nil {
		t.Fatalf("marshal episode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(episodeDir, "episode.yaml"), meta, 0o644); err != nil {
		t.Fatalf("write episode.yaml: %v", err)
	}
	eventsFile, err := os.Create(filepath.Join(episodeDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("create events.jsonl: %v", err)
	}
	for _, event := range episode.Events {
		line, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if _, err := eventsFile.Write(append(line, '\n')); err != nil {
			t.Fatalf("write event: %v", err)
		}
	}
	if err := eventsFile.Close(); err != nil {
		t.Fatalf("close events.jsonl: %v", err)
	}

	promptCalls := []telemetryPromptCall{
		{
			ID:        "2f5a0f2f4a3e4a3e",
			Role:      telemetryRoleAgent,
			StartedAt: start.Add(150 * time.Millisecond),
			EndedAt:   start.Add(250 * time.Millisecond),
			Input: []map[string]interface{}{
				{"role": "system", "parts": []map[string]interface{}{{"type": "text", "text": "You operate a phone."}}},
				{"role": "human", "parts": []map[string]interface{}{{"type": "text", "text": "打开设置并截图"}}},
			},
			Output:          map[string]interface{}{"choices": []map[string]interface{}{{"content": "tapping settings"}}},
			UsageDetails:    map[string]int{"input": 120, "output": 30, "total": 150, "cached": 60},
			CostDetails:     map[string]float64{"total": 0.00042},
			ModelParameters: map[string]interface{}{"temperature": 0.2, "max_tokens": 512},
			Metadata:        map[string]interface{}{"llm_time_to_first_content_ms": int64(180), "tools_count": 4},
		},
	}
	return episode, episodeDir, promptCalls
}

type langfuseLiveClient struct {
	t         *testing.T
	baseURL   string
	publicKey string
	secretKey string
}

func (c langfuseLiveClient) projectID(ctx context.Context) string {
	body, status, err := c.get(ctx, "/api/public/projects")
	if err != nil || status != http.StatusOK {
		return ""
	}
	projects, _ := body["data"].([]interface{})
	if len(projects) == 0 {
		return ""
	}
	return langfuseLiveString(langfuseLiveMap(projects[0]), "id")
}

func (c langfuseLiveClient) waitForTrace(ctx context.Context, traceID string) map[string]interface{} {
	deadline := time.Now().Add(45 * time.Second)
	var lastStatus int
	var lastErr error
	for time.Now().Before(deadline) {
		body, status, err := c.get(ctx, "/api/public/traces/"+traceID)
		lastStatus, lastErr = status, err
		if err == nil && status == http.StatusOK {
			if observations, ok := body["observations"].([]interface{}); ok && len(observations) > 0 {
				return body
			}
		}
		select {
		case <-ctx.Done():
			c.t.Fatalf("waiting for trace %s: %v", traceID, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	c.t.Fatalf("trace %s did not appear (last status %d, err %v)", traceID, lastStatus, lastErr)
	return nil
}

// waitForScores polls the scores API until the trace's score is visible. Older
// self-hosted deployments ignore the traceId filter, so results are matched on
// the returned traceId instead.
func (c langfuseLiveClient) waitForScores(ctx context.Context, traceID, name string) []map[string]interface{} {
	deadline := time.Now().Add(30 * time.Second)
	var last []map[string]interface{}
	for time.Now().Before(deadline) {
		last = c.scores(ctx, traceID, name)
		if len(last) > 0 {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(2 * time.Second):
		}
	}
	return last
}

func (c langfuseLiveClient) scores(ctx context.Context, traceID, name string) []map[string]interface{} {
	body, status, err := c.get(ctx, "/api/public/scores?name="+name+"&limit=100")
	if err != nil || status != http.StatusOK {
		return nil
	}
	items, _ := body["data"].([]interface{})
	scores := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		score := langfuseLiveMap(item)
		if langfuseLiveString(score, "traceId") == traceID {
			scores = append(scores, score)
		}
	}
	return scores
}

func (c langfuseLiveClient) get(ctx context.Context, path string) (map[string]interface{}, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.SetBasicAuth(c.publicKey, c.secretKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func langfuseLiveObservations(trace map[string]interface{}) []map[string]interface{} {
	items, _ := trace["observations"].([]interface{})
	observations := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		observations = append(observations, langfuseLiveMap(item))
	}
	return observations
}

func langfuseLiveNames(observations []map[string]interface{}) []string {
	names := make([]string, 0, len(observations))
	for _, observation := range observations {
		names = append(names, langfuseLiveString(observation, "name"))
	}
	return names
}

func langfuseLiveMap(value interface{}) map[string]interface{} {
	if typed, ok := value.(map[string]interface{}); ok {
		return typed
	}
	return map[string]interface{}{}
}

// langfuseLiveJSON renders an observation field as JSON so assertions can look
// for content inside structured input and output values.
func langfuseLiveJSON(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func langfuseLiveInt64Ptr(value int64) *int64 {
	return &value
}

func langfuseLiveString(values map[string]interface{}, key string) string {
	if value, ok := values[key].(string); ok {
		return value
	}
	return ""
}

func langfuseLiveStrings(value interface{}) []string {
	items, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func langfuseLiveContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

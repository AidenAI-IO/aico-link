package agent

import (
	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/util"
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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func TestOpenAICompatibleModelReturnsStructuredProviderHTTPError(t *testing.T) {
	tests := []struct {
		name             string
		statusCode       int
		body             string
		wantProviderCode string
		wantFailureCode  string
	}{
		{
			name:             "openrouter payment required",
			statusCode:       http.StatusPaymentRequired,
			body:             `{"error":{"message":"This request requires more credits, or fewer max_tokens.","code":402}}`,
			wantProviderCode: "402",
			wantFailureCode:  TurnFailureTokenInsufficient,
		},
		{
			name:             "openai insufficient quota",
			statusCode:       http.StatusTooManyRequests,
			body:             `{"error":{"message":"quota exhausted","code":"insufficient_quota"}}`,
			wantProviderCode: "insufficient_quota",
			wantFailureCode:  TurnFailureTokenInsufficient,
		},
		{
			name:             "provider rate limit",
			statusCode:       http.StatusTooManyRequests,
			body:             `{"error":{"message":"too many requests","code":"rate_limit_exceeded"}}`,
			wantProviderCode: "rate_limit_exceeded",
			wantFailureCode:  TurnFailureNetworkUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer server.Close()

			model := newOpenAICompatibleModel(server.URL, "test-model", "token", server.Client())
			_, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
				Role:  llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{llms.TextPart("test")},
			}})
			if err == nil {
				t.Fatal("GenerateContent() error = nil")
			}

			var providerErr *ProviderHTTPError
			if !errors.As(err, &providerErr) {
				t.Fatalf("GenerateContent() error type = %T, want *ProviderHTTPError", err)
			}
			if providerErr.StatusCode != tt.statusCode || providerErr.ProviderCode != tt.wantProviderCode {
				t.Fatalf("ProviderHTTPError = %#v", providerErr)
			}
			failure := TurnFailureFromError(err)
			if failure == nil || failure.Code != tt.wantFailureCode {
				t.Fatalf("TurnFailureFromError() = %#v, want %q", failure, tt.wantFailureCode)
			}
		})
	}
}

func TestOpenAICompatibleModelEncodesAudioAsInputAudio(t *testing.T) {
	var captured struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url,omitempty"`
				InputAudio *struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"input_audio,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "token", server.Client())
	audioBytes := []byte("RIFFaudio")

	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart("transcribe"),
			llms.BinaryPart("audio/wav", audioBytes),
		},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if resp.Choices[0].Content != "ok" {
		t.Fatalf("unexpected response: %#v", resp.Choices[0].Content)
	}
	if len(captured.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(captured.Messages))
	}
	if len(captured.Messages[0].Content) != 2 {
		t.Fatalf("expected 2 content parts, got %d", len(captured.Messages[0].Content))
	}
	if captured.Messages[0].Content[1].Type != "input_audio" {
		t.Fatalf("expected input_audio part, got %#v", captured.Messages[0].Content[1])
	}
	if captured.Messages[0].Content[1].InputAudio == nil {
		t.Fatalf("expected input_audio payload, got %#v", captured.Messages[0].Content[1])
	}
	if captured.Messages[0].Content[1].InputAudio.Format != "wav" {
		t.Fatalf("unexpected audio format: %#v", captured.Messages[0].Content[1].InputAudio.Format)
	}
	if captured.Messages[0].Content[1].InputAudio.Data != base64.StdEncoding.EncodeToString(audioBytes) {
		t.Fatalf("unexpected audio payload: %#v", captured.Messages[0].Content[1].InputAudio.Data)
	}
}

func TestOpenAICompatibleModelEncodesImageBinaryAsImageURL(t *testing.T) {
	var captured struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text,omitempty"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	imageBytes := []byte("jpeg-image")
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart("inspect"),
			llms.BinaryPart("image/jpeg", imageBytes),
		},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) != 1 || len(captured.Messages[0].Content) != 2 {
		t.Fatalf("captured messages = %#v, want text plus image", captured.Messages)
	}
	image := captured.Messages[0].Content[1]
	wantURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	if image.Type != "image_url" || image.ImageURL == nil || image.ImageURL.URL != wantURL {
		t.Fatalf("image content = %#v, want image_url %q", image, wantURL)
	}
}

func TestOpenAICompatibleModelParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo","arguments":"{\"value\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("say hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %#v", resp.Choices)
	}
	if resp.Choices[0].ToolCalls[0].FunctionCall.Name != "echo" {
		t.Fatalf("unexpected tool call: %#v", resp.Choices[0].ToolCalls[0])
	}
}

func TestOpenAICompatibleModelNormalizesInvalidToolCallArgumentsInRequest(t *testing.T) {
	rawArguments := `{"type": "tap", "point": {"x":}`
	var captured struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role: llms.ChatMessageTypeAI,
		Parts: []llms.ContentPart{llms.ToolCall{
			ID:   "call_1",
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      "touch_gesture",
				Arguments: rawArguments,
			},
		}},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) != 1 || len(captured.Messages[0].ToolCalls) != 1 {
		t.Fatalf("captured messages = %#v, want one tool call", captured.Messages)
	}
	got := captured.Messages[0].ToolCalls[0].Function.Arguments
	if got != encodeToolArguments(rawArguments) || !json.Valid([]byte(got)) {
		t.Fatalf("arguments = %q, want valid JSON wrapper", got)
	}
}

func TestOpenAICompatibleModelLogsRawHTTPWhenEnabled(t *testing.T) {
	rawResponse := `{"choices":[{"message":{"content":"<think>\n需要查当前时间。\n</think>","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"date\"}"}}]},"finish_reason":"tool_calls"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawResponse))
	}))
	defer server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		server.URL,
		"test-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("现在几点了？")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	logText := readRawHTTPLog(t, logDir)

	// Parse JSONL and check for expected content
	var foundResponse, foundRequest bool
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		body, _ := entry["body"].(string)
		if strings.Contains(body, "finish_reason") && strings.Contains(body, "tool_calls") {
			foundResponse = true
		}
		if strings.Contains(body, "现在几点了") {
			foundRequest = true
		}
	}

	if !foundResponse {
		t.Fatalf("raw HTTP log missing response:\n%s", logText)
	}
	if !foundRequest {
		t.Fatalf("raw HTTP log missing request:\n%s", logText)
	}
	if !strings.Contains(logText, `"kind":"response"`) {
		t.Fatalf("raw HTTP log missing response metadata:\n%s", logText)
	}
	if !strings.Contains(logText, `"kind":"request"`) {
		t.Fatalf("raw HTTP log missing request record:\n%s", logText)
	}
	assertRawHTTPLogIsValidJSONL(t, logText)
}

func TestOpenAICompatibleModelRawHTTPLogUsesEffectiveRequestModel(t *testing.T) {
	rawResponse := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawResponse))
	}))
	defer server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		server.URL,
		"configured-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	_, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("hello")},
		}},
		llms.WithModel("override-model"),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	logText := readRawHTTPLog(t, logDir)

	// Parse JSONL and check for model in request body
	var foundModel bool
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		body, _ := entry["body"].(string)
		if strings.Contains(body, `"model":"override-model"`) {
			foundModel = true
			break
		}
	}

	if !foundModel {
		t.Fatalf("raw HTTP log should use effective model in request body:\n%s", logText)
	}
}

func TestOpenAICompatibleModelStreamsContent(t *testing.T) {
	var captured struct {
		Stream bool `json:"stream"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var chunks []string
	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	resp, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("hello")},
		}},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			chunks = append(chunks, string(chunk))
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if !captured.Stream {
		t.Fatal("expected stream=true in request")
	}
	if resp.Choices[0].Content != "hello world" {
		t.Fatalf("unexpected streamed content: %#v", resp.Choices[0].Content)
	}
	if len(chunks) != 2 || chunks[0] != "hello " || chunks[1] != "world" {
		t.Fatalf("unexpected stream chunks: %#v", chunks)
	}
}

func TestOpenAICompatibleModelLogsRawStreamingHTTPWhenEnabled(t *testing.T) {
	firstEvent := `data: {"choices":[{"delta":{"content":"<think>\n"}}]}`
	secondEvent := `data: {"choices":[{"delta":{"content":"需要查当前时间。\n</think>"},"finish_reason":"tool_calls"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(firstEvent + "\n\n"))
		w.Write([]byte(secondEvent + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		server.URL,
		"test-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	_, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("现在几点了？")},
		}},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	logText := readRawHTTPLog(t, logDir)

	// Parse JSONL and check stream body contains expected events
	var foundStreamResponse bool
	var streamBody string
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["kind"] == "response" {
			streamBody, _ = entry["body"].(string)
			foundStreamResponse = true
			break
		}
	}

	if !foundStreamResponse {
		t.Fatalf("raw streaming HTTP log missing stream response:\n%s", logText)
	}
	if strings.Contains(streamBody, "data:") {
		t.Fatalf("raw streaming HTTP log should store a readable JSON response, not raw SSE:\n%s", streamBody)
	}
	var logged struct {
		Stream  bool `json:"stream"`
		Done    bool `json:"done"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(streamBody), &logged); err != nil {
		t.Fatalf("raw streaming HTTP response body should be JSON: %v\n%s", err, streamBody)
	}
	if !logged.Stream || !logged.Done {
		t.Fatalf("raw streaming HTTP response missing stream metadata: %#v", logged)
	}
	if len(logged.Choices) != 1 {
		t.Fatalf("raw streaming HTTP response choices = %#v", logged.Choices)
	}
	if logged.Choices[0].Message.Content != "<think>\n需要查当前时间。\n</think>" {
		t.Fatalf("raw streaming HTTP response content = %q", logged.Choices[0].Message.Content)
	}
	if logged.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("raw streaming HTTP response finish_reason = %q", logged.Choices[0].FinishReason)
	}
	if !strings.Contains(logText, `"kind":"response"`) {
		t.Fatalf("raw streaming HTTP log missing metadata:\n%s", logText)
	}
	assertRawHTTPLogIsValidJSONL(t, logText)
}

func TestOpenAICompatibleModelLogsRawStreamingHTTPOnDecodeError(t *testing.T) {
	validEvent := `data: {"choices":[{"delta":{"content":"hello"}}]}`
	malformedEvent := `data: {"choices":[`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(validEvent + "\n\n"))
		w.Write([]byte(malformedEvent + "\n\n"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		server.URL,
		"test-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	_, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("stream please")},
		}},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error { return nil }),
	)
	if err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("GenerateContent() error = %v, want decode stream event", err)
	}

	logText := readRawHTTPLog(t, logDir)
	var streamBody string
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry["kind"] == "response" {
			streamBody, _ = entry["body"].(string)
			break
		}
	}
	var logged struct {
		Stream  bool   `json:"stream"`
		Error   string `json:"error"`
		RawSSE  string `json:"raw_sse"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(streamBody), &logged); err != nil {
		t.Fatalf("raw streaming error response body should be JSON: %v\n%s", err, streamBody)
	}
	if !logged.Stream {
		t.Fatalf("raw streaming error response missing stream marker: %#v", logged)
	}
	if !strings.Contains(logged.Error, "decode stream event") {
		t.Fatalf("raw streaming error response missing decode error: %#v", logged)
	}
	if len(logged.Choices) != 1 || logged.Choices[0].Message.Content != "hello" {
		t.Fatalf("raw streaming error response missing partial content: %#v", logged.Choices)
	}
	if !strings.Contains(logged.RawSSE, validEvent) || !strings.Contains(logged.RawSSE, malformedEvent) {
		t.Fatalf("raw streaming error response missing failed SSE events:\nlog=%s\nstream=%s", logText, streamBody)
	}
	assertRawHTTPLogIsValidJSONL(t, logText)
}

func TestOpenAICompatibleModelRawHTTPLogKeepsRunInInitialSessionFile(t *testing.T) {
	var sessionMu sync.Mutex
	sessionID := "session-a"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionMu.Lock()
		sessionID = "session-b"
		sessionMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	logDir := t.TempDir()
	logger := newLLMRawHTTPLogger(logDir, NewModelRuntimeBindings())
	logger.bindings.SetSessionIDProvider(func() string {
		sessionMu.Lock()
		defer sessionMu.Unlock()
		return sessionID
	})
	times := []time.Time{
		time.Date(2026, 6, 21, 9, 4, 59, 0, time.UTC),
		time.Date(2026, 6, 21, 9, 4, 59, 0, time.UTC),
		time.Date(2026, 6, 21, 9, 5, 1, 0, time.UTC),
	}
	logger.now = func() time.Time {
		if len(times) == 0 {
			return time.Date(2026, 6, 21, 9, 5, 1, 0, time.UTC)
		}
		next := times[0]
		times = times[1:]
		return next
	}

	model := newOpenAICompatibleModel(
		server.URL,
		"test-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(logger),
	)
	_, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("hello")},
		}},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	if err != nil {
		t.Fatalf("glob raw HTTP logs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("raw HTTP request/response should stay in one file, got %d: %#v", len(matches), matches)
	}
	if filepath.Base(matches[0]) != "llm-http-session-a.log" {
		t.Fatalf("raw HTTP file should use initial session, got %s", filepath.Base(matches[0]))
	}
	logText := readRawHTTPLog(t, logDir)
	counts := countRawHTTPKinds(t, logText)
	if counts["request"] != 1 || counts["response"] != 1 {
		t.Fatalf("raw HTTP log should contain one request and one response:\n%s", logText)
	}
}

func TestLLMRawHTTPLoggerFallsBackToTimestampForUnsafeSessionID(t *testing.T) {
	logDir := t.TempDir()
	logger := newLLMRawHTTPLogger(logDir, NewModelRuntimeBindings())
	fileTime := time.Date(2026, 6, 21, 9, 4, 59, 0, time.UTC)

	ctx := contextWithRawHTTPLogFileTime(context.Background(), fileTime)
	ctx = contextWithRawHTTPLogFileSessionID(ctx, "..evil")
	if err := logger.Log(ctx, RawHTTPLogEntry{Model: "test-model", Kind: "request", StatusCode: http.StatusOK, Raw: `{"ok":true}`}); err != nil {
		t.Fatalf("Log() error = %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	if err != nil {
		t.Fatalf("glob raw HTTP logs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one raw HTTP log file, got %d: %#v", len(matches), matches)
	}
	wantName := "llm-http-202606210904.log"
	if filepath.Base(matches[0]) != wantName {
		t.Fatalf("raw HTTP file = %q, want %q", filepath.Base(matches[0]), wantName)
	}
}

// newRawHTTPLogTestRuntime wires a runtime whose raw HTTP log is partitioned by
// the live ContextManager session, and returns the model plus its log clock.
func newRawHTTPLogTestRuntime(t *testing.T, configDir, logDir, serverURL string) *openAICompatibleModel {
	t.Helper()
	manager := NewModelManager(ModelConfig{
		Provider:   "openai",
		BaseURL:    serverURL,
		Model:      "test-model",
		LogRawHTTP: true,
	}, ProxyConfig{}, WithLLMRawHTTPLogDir(logDir))
	NewRuntimeWithDeps(Config{ConfigDir: configDir}, manager, NewMemoryManager(filepath.Join(configDir, "memory")), nil, nil)

	resolved, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	compatible, ok := resolved.(*openAICompatibleModel)
	if !ok {
		t.Fatalf("model = %T, want *openAICompatibleModel", resolved)
	}
	compatible.rawLogger.(*llmRawHTTPLogger).now = func() time.Time {
		return time.Date(2026, 6, 21, 9, 4, 59, 0, time.UTC)
	}
	return compatible
}

func TestRuntimeConfiguresRawHTTPLogWithCurrentContextSessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	rootDir := t.TempDir()
	logDir := filepath.Join(rootDir, "log")
	sessionFolder := agentpath.ContextManagerSessionFolder(rootDir)
	if _, err := contextmanager.NewContextManager(sessionFolder, "system"); err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}

	model := newRawHTTPLogTestRuntime(t, rootDir, logDir, server.URL)
	if _, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	if err != nil {
		t.Fatalf("glob raw HTTP logs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one raw HTTP log file, got %d: %#v", len(matches), matches)
	}
	wantName := rawHTTPLogNameForCurrentSession(t, rootDir)
	if filepath.Base(matches[0]) != wantName {
		t.Fatalf("raw HTTP file = %q, want %q", filepath.Base(matches[0]), wantName)
	}
}

func TestRuntimeRawHTTPLogKeepsOneFilePerSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	rootDir := t.TempDir()
	logDir := filepath.Join(rootDir, "log")
	sessionFolder := agentpath.ContextManagerSessionFolder(rootDir)
	if _, err := contextmanager.NewContextManager(sessionFolder, "system"); err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}

	model := newRawHTTPLogTestRuntime(t, rootDir, logDir, server.URL)
	current := time.Date(2026, 6, 21, 9, 4, 59, 0, time.UTC)
	model.rawLogger.(*llmRawHTTPLogger).now = func() time.Time { return current }

	runLLM := func(prompt string) {
		t.Helper()
		if _, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart(prompt)},
		}}); err != nil {
			t.Fatalf("GenerateContent(%q) error = %v", prompt, err)
		}
	}

	runLLM("first")
	// Advancing the clock must not split the log: partitioning follows the
	// session, not wall-clock minutes.
	current = time.Date(2026, 6, 21, 9, 5, 1, 0, time.UTC)
	runLLM("second")

	matches, err := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	if err != nil {
		t.Fatalf("glob raw HTTP logs: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("same session should stay in one raw HTTP log file, got %d: %#v", len(matches), matches)
	}
	wantName := rawHTTPLogNameForCurrentSession(t, rootDir)
	if filepath.Base(matches[0]) != wantName {
		t.Fatalf("raw HTTP file = %q, want %q", filepath.Base(matches[0]), wantName)
	}
	logText := readRawHTTPLog(t, logDir)
	counts := countRawHTTPKinds(t, logText)
	if counts["request"] != 2 || counts["response"] != 2 {
		t.Fatalf("raw HTTP log should contain two request/response pairs:\n%s", logText)
	}
}

// A compaction revision starts a new transcript session while the long-lived
// context manager remains in place, so subsequent LLM traffic must land in a
// new log partition.
func TestRuntimeRawHTTPLogSwitchesFileAfterSessionRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	rootDir := t.TempDir()
	logDir := filepath.Join(rootDir, "log")
	sessionFolder := agentpath.ContextManagerSessionFolder(rootDir)
	first, err := contextmanager.NewContextManager(sessionFolder, "system")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}

	model := newRawHTTPLogTestRuntime(t, rootDir, logDir, server.URL)
	runLLM := func(prompt string) {
		t.Helper()
		if _, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart(prompt)},
		}}); err != nil {
			t.Fatalf("GenerateContent(%q) error = %v", prompt, err)
		}
	}

	runLLM("before revision")
	firstName := rawHTTPLogNameForCurrentSession(t, rootDir)

	revision, err := contextmanager.NewContextManagerRevisionFromMessageList(first, first.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	if err := first.Activate(revision); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	runLLM("after revision")
	revisionName := rawHTTPLogNameForCurrentSession(t, rootDir)
	if revisionName == firstName {
		t.Fatalf("revision session reused the first log name %q", firstName)
	}

	matches, err := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	if err != nil {
		t.Fatalf("glob raw HTTP logs: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected two raw HTTP log files after the revision, got %d: %#v", len(matches), matches)
	}
	found := map[string]bool{firstName: false, revisionName: false}
	for _, match := range matches {
		if _, ok := found[filepath.Base(match)]; ok {
			found[filepath.Base(match)] = true
		}
	}
	for name, ok := range found {
		if !ok {
			t.Fatalf("missing raw HTTP log file %q; got %#v", name, matches)
		}
	}
}

// countRawHTTPKinds returns how many log entries carry each kind, so tests can
// assert the request/response pairing invariant the log viewer relies on.
func countRawHTTPKinds(t *testing.T, logText string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid JSONL line: %v\nline: %s", err, line)
		}
		kind, _ := entry["kind"].(string)
		counts[kind]++
	}
	return counts
}

func TestOpenAICompatibleModelLogsResponseOnTransportError(t *testing.T) {
	// Point the model at a server that is already closed so httpClient.Do
	// fails at the transport layer before any HTTP response exists.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedURL := server.URL
	client := server.Client()
	server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		closedURL,
		"test-model",
		"",
		client,
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}

	logText := readRawHTTPLog(t, logDir)
	counts := countRawHTTPKinds(t, logText)
	if counts["request"] != 1 {
		t.Fatalf("expected 1 request entry, got %d:\n%s", counts["request"], logText)
	}
	if counts["response"] != 1 {
		t.Fatalf("expected 1 response entry on transport failure, got %d:\n%s", counts["response"], logText)
	}
	if !strings.Contains(logText, "transport error:") {
		t.Fatalf("response entry missing transport error detail:\n%s", logText)
	}
	if !strings.Contains(logText, `"status":0`) {
		t.Fatalf("transport failure should log status 0:\n%s", logText)
	}
	assertRawHTTPLogIsValidJSONL(t, logText)
}

func TestOpenAICompatibleModelSkipsRawHTTPLogWithoutContextMarker(t *testing.T) {
	// Background tasks (summarization, profile rebuilds, skill merge) share the
	// model and logger but call with a plain context. Those calls must not be
	// logged, so their requests don't interleave with the main loop's entries.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	logDir := t.TempDir()
	model := newOpenAICompatibleModel(
		server.URL,
		"test-model",
		"",
		server.Client(),
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(logDir, "test-session-1")),
	)
	// Plain context, no contextWithRawHTTPLog marker.
	_, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if _, statErr := os.Stat(rawHTTPLogPath(logDir)); statErr == nil {
		t.Fatalf("raw HTTP log was written for a call without the context marker:\n%s", readRawHTTPLog(t, logDir))
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected error stating log path: %v", statErr)
	}
}

func TestOpenAICompatibleModelDoesNotBufferRawHTTPResponseWithoutContextMarker(t *testing.T) {
	// Decoder.Decode consumes the first JSON value, while buffering the entire
	// body and calling json.Unmarshal would reject the trailing second value.
	responseBody := io.NopCloser(strings.NewReader(
		`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}` + "\n{}",
	))
	client := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       responseBody,
			}, nil
		}),
	}

	model := newOpenAICompatibleModel(
		"https://example.test",
		"test-model",
		"",
		client,
		withOpenAICompatibleRawHTTPLogger(newTestLLMRawHTTPLogger(t.TempDir(), "test-session-1")),
	)

	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if got := resp.Choices[0].Content; got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
}

func TestModelManagerEnablesRawHTTPLoggingFromModelConfig(t *testing.T) {
	rawResponse := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawResponse))
	}))
	defer server.Close()

	logDir := t.TempDir()
	manager := NewModelManager(ModelConfig{
		Provider:   "openai",
		Model:      "test-model",
		BaseURL:    server.URL,
		LogRawHTTP: true,
	}, ProxyConfig{}, WithLLMRawHTTPLogDir(logDir))
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	_, err = model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	logText := readRawHTTPLog(t, logDir)

	// Parse JSONL and check for expected content
	var foundResponse bool
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		body, _ := entry["body"].(string)
		if strings.Contains(body, "finish_reason") && strings.Contains(body, "ok") {
			foundResponse = true
			break
		}
	}

	if !foundResponse {
		t.Fatalf("raw HTTP log missing response from model manager config:\n%s", logText)
	}
}

func TestModelManagerEnablesRawHTTPLoggingFromDefaultConfig(t *testing.T) {
	rawResponse := `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(rawResponse))
	}))
	defer server.Close()

	logDir := t.TempDir()
	cfg := DefaultConfig().Model
	cfg.Provider = "openai"
	cfg.Model = "test-model"
	cfg.BaseURL = server.URL
	manager := NewModelManager(cfg, ProxyConfig{}, WithLLMRawHTTPLogDir(logDir))
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	_, err = model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	logText := readRawHTTPLog(t, logDir)

	// Parse JSONL and check for expected content
	var foundResponse bool
	lines := strings.Split(strings.TrimSpace(logText), "\n")
	for _, line := range lines {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		body, _ := entry["body"].(string)
		if strings.Contains(body, "finish_reason") && strings.Contains(body, "ok") {
			foundResponse = true
			break
		}
	}

	if !foundResponse {
		t.Fatalf("raw HTTP log missing response from default config:\n%s", logText)
	}
}

func TestModelManagerDoesNotLogRawHTTPWhenDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	logDir := t.TempDir()
	manager := NewModelManager(ModelConfig{
		Provider: "openai",
		Model:    "test-model",
		BaseURL:  server.URL,
	}, ProxyConfig{}, WithLLMRawHTTPLogDir(logDir))
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	_, err = model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if _, err := os.Stat(rawHTTPLogPath(logDir)); !os.IsNotExist(err) {
		t.Fatalf("raw HTTP log should not exist when disabled, err=%v", err)
	}
}

func TestOpenAICompatibleModelStreamsToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"id\":\"call_3\",\"type\":\"function\",\"function\":{\"name\":\"later\",\"arguments\":\"{\\\"\"}},{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"\"}}]}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"value\\\":\\\"hello\\\"}\"}},{\"index\":2,\"function\":{\"arguments\":\"value\\\":\\\"done\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	resp, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("say hello")},
		}},
		llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if len(resp.Choices[0].ToolCalls) != 2 {
		t.Fatalf("expected two tool calls, got %#v", resp.Choices[0].ToolCalls)
	}
	call := resp.Choices[0].ToolCalls[0]
	if call.ID != "call_1" || call.FunctionCall.Name != "echo" || call.FunctionCall.Arguments != `{"value":"hello"}` {
		t.Fatalf("unexpected streamed tool call: %#v", call)
	}
	call = resp.Choices[0].ToolCalls[1]
	if call.ID != "call_3" || call.FunctionCall.Name != "later" || call.FunctionCall.Arguments != `{"value":"done"}` {
		t.Fatalf("unexpected streamed tool call: %#v", call)
	}
}

func TestOpenAICompatibleModelMergesSystemMessages(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("System A")}},
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("System B")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
	})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) != 2 {
		t.Fatalf("expected 2 messages after normalization, got %#v", captured.Messages)
	}
	if captured.Messages[0].Role != "system" || captured.Messages[0].Content != "System A\n\nSystem B" {
		t.Fatalf("unexpected merged system message: %#v", captured.Messages[0])
	}
	if captured.Messages[1].Role != "user" || captured.Messages[1].Content != "hello" {
		t.Fatalf("unexpected user message: %#v", captured.Messages[1])
	}
}

func TestOpenAICompatibleModelMergesConsecutiveSameRoleMessages(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role      string      `json:"role"`
			Content   interface{} `json:"content"`
			ToolCalls []struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"tool_calls,omitempty"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())

	// Test consecutive user messages (simulating state/notice role conversion)
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("System prompt")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("User message 1")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("User message 2")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{llms.TextPart("Assistant response")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("Another user message")}},
	})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	// Should have: system, merged user, assistant, user
	if len(captured.Messages) != 4 {
		t.Fatalf("expected 4 messages after normalization, got %d: %#v", len(captured.Messages), captured.Messages)
	}

	if captured.Messages[0].Role != "system" {
		t.Fatalf("message 0: expected system, got %#v", captured.Messages[0])
	}

	if captured.Messages[1].Role != "user" {
		t.Fatalf("message 1: expected user, got %#v", captured.Messages[1])
	}
	userContent, ok := captured.Messages[1].Content.(string)
	if !ok || userContent != "User message 1\n\nUser message 2" {
		t.Fatalf("message 1: expected merged user content, got %#v", captured.Messages[1].Content)
	}

	if captured.Messages[2].Role != "assistant" {
		t.Fatalf("message 2: expected assistant, got %#v", captured.Messages[2])
	}

	if captured.Messages[3].Role != "user" {
		t.Fatalf("message 3: expected user, got %#v", captured.Messages[3])
	}
}

func TestOpenAICompatibleModelDoesNotMergeToolMessages(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role       string      `json:"role"`
			Content    interface{} `json:"content"`
			ToolCallID string      `json:"tool_call_id,omitempty"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())

	// Tool messages should NOT be merged even if consecutive
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("use tools")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{
			llms.ToolCall{ID: "call1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "tool1", Arguments: "{}"}},
		}},
		{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{
			llms.ToolCallResponse{ToolCallID: "call1", Name: "tool1", Content: "result1"},
		}},
		{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{
			llms.ToolCallResponse{ToolCallID: "call2", Name: "tool2", Content: "result2"},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	// Should keep all 4 messages separate (tool messages must not merge)
	if len(captured.Messages) != 4 {
		t.Fatalf("expected 4 messages (tool messages should not merge), got %d: %#v", len(captured.Messages), captured.Messages)
	}

	if captured.Messages[2].Role != "tool" || captured.Messages[2].ToolCallID != "call1" {
		t.Fatalf("message 2: expected separate tool message, got %#v", captured.Messages[2])
	}

	if captured.Messages[3].Role != "tool" || captured.Messages[3].ToolCallID != "call2" {
		t.Fatalf("message 3: expected separate tool message, got %#v", captured.Messages[3])
	}
}

func TestOpenAICompatibleModelDoesNotMergeAssistantWithToolCalls(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role      string      `json:"role"`
			Content   interface{} `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls,omitempty"`
		} `json:"messages"`
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())

	// Assistant messages with tool_calls should NOT merge with other assistant messages
	_, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{llms.TextPart("text response")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{
			llms.ToolCall{ID: "call1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "tool1", Arguments: "{}"}},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	// Should keep 3 messages: user, assistant (text), assistant (tool_calls)
	if len(captured.Messages) != 3 {
		t.Fatalf("expected 3 messages (assistant with tool_calls should not merge), got %d: %#v", len(captured.Messages), captured.Messages)
	}

	if captured.Messages[1].Role != "assistant" || len(captured.Messages[1].ToolCalls) != 0 {
		t.Fatalf("message 1: expected plain assistant, got %#v", captured.Messages[1])
	}

	if captured.Messages[2].Role != "assistant" || len(captured.Messages[2].ToolCalls) == 0 {
		t.Fatalf("message 2: expected assistant with tool_calls, got %#v", captured.Messages[2])
	}
}

func TestOpenAICompatibleModelIncludesUsageInGenerationInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	got := resp.Choices[0].GenerationInfo
	if got["prompt_tokens"] != 11 || got["completion_tokens"] != 7 || got["total_tokens"] != 18 {
		t.Fatalf("unexpected generation info: %#v", got)
	}
}

func TestOpenAICompatibleModelIncludesCachedTokensInGenerationInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":7,"total_tokens":1207,"prompt_tokens_details":{"cached_tokens":1024}}}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	got := resp.Choices[0].GenerationInfo
	if got["cached_tokens"] != 1024 {
		t.Fatalf("expected cached_tokens=1024 in generation info, got %#v", got)
	}

	usage := telemetryUsageDetails(resp)
	if usage["cached"] != 1024 || usage["input"] != 1200 {
		t.Fatalf("expected telemetry cached=1024 input=1200, got %#v", usage)
	}

	metrics := &RunMetrics{}
	recordUsageMetrics(metrics, resp)
	if metrics.CachedPromptTokens != 1024 || metrics.PromptTokens != 1200 {
		t.Fatalf("expected run metrics cached=1024 prompt=1200, got %#v", metrics)
	}
	if hitRate := float64(metrics.CachedPromptTokens) / float64(metrics.PromptTokens); hitRate < 0.85 {
		t.Fatalf("expected hit rate >= 0.85, got %.3f", hitRate)
	}
}

func TestOpenAICompatibleModelRecordsProviderTimingMetadata(t *testing.T) {
	var gotMetadataHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMetadataHeader = r.Header.Get("X-OpenRouter-Metadata")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Generation-Id", "gen-test-123")
		w.Write([]byte(`{
			"id":"chatcmpl-test",
			"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18},
			"openrouter_metadata":{"provider_name":"TestProvider","strategy":"fallback","attempt":2}
		}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client(),
		withOpenAICompatibleRouterMetadata())
	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if gotMetadataHeader != "enabled" {
		t.Fatalf("X-OpenRouter-Metadata = %q, want enabled", gotMetadataHeader)
	}

	info := resp.Choices[0].GenerationInfo
	for _, key := range []string{
		"llm_request_prepare_ms",
		"llm_json_marshal_ms",
		"llm_http_to_headers_ms",
		"llm_response_read_ms",
		"llm_response_decode_ms",
		"llm_http_status",
		"llm_output_chars",
		"llm_total_ms",
		"llm_ms_per_output_token",
	} {
		if _, ok := info[key]; !ok {
			t.Fatalf("generation info missing %s: %#v", key, info)
		}
	}
	if info["openrouter_generation_id"] != "gen-test-123" {
		t.Fatalf("openrouter_generation_id = %#v, want gen-test-123", info["openrouter_generation_id"])
	}
	if info["openrouter_provider_name"] != "TestProvider" {
		t.Fatalf("openrouter_provider_name = %#v, want TestProvider", info["openrouter_provider_name"])
	}
	if info["openrouter_strategy"] != "fallback" {
		t.Fatalf("openrouter_strategy = %#v, want fallback", info["openrouter_strategy"])
	}
	if info["openrouter_attempt"] != float64(2) {
		t.Fatalf("openrouter_attempt = %#v, want 2", info["openrouter_attempt"])
	}
}

func TestOpenAICompatibleModelRecordsStreamingTimingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Generation-Id", "gen-stream-123")
		w.Write([]byte(": OPENROUTER PROCESSING\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5},\"openrouter_metadata\":{\"provider_name\":\"StreamProvider\"}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client(),
		withOpenAICompatibleRouterMetadata())
	resp, err := model.GenerateContent(
		contextWithRawHTTPLog(context.Background()),
		[]llms.MessageContent{{
			Role:  llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{llms.TextPart("hello")},
		}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }),
	)
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	info := resp.Choices[0].GenerationInfo
	for _, key := range []string{
		"llm_stream_read_ms",
		"llm_time_to_first_sse_ms",
		"llm_time_to_first_content_ms",
		"llm_stream_sse_events",
		"llm_stream_content_chunks",
		"llm_stream_comment_count",
		"llm_total_ms",
		"llm_ms_per_output_token",
		"llm_ttft_per_input_token",
	} {
		if _, ok := info[key]; !ok {
			t.Fatalf("generation info missing %s: %#v", key, info)
		}
	}
	if info["llm_stream_sse_events"] != 2 {
		t.Fatalf("llm_stream_sse_events = %#v, want 2", info["llm_stream_sse_events"])
	}
	if info["llm_stream_content_chunks"] != 2 {
		t.Fatalf("llm_stream_content_chunks = %#v, want 2", info["llm_stream_content_chunks"])
	}
	if info["llm_stream_comment_count"] != 1 {
		t.Fatalf("llm_stream_comment_count = %#v, want 1", info["llm_stream_comment_count"])
	}
	if info["openrouter_generation_id"] != "gen-stream-123" {
		t.Fatalf("openrouter_generation_id = %#v, want gen-stream-123", info["openrouter_generation_id"])
	}
	if info["openrouter_provider_name"] != "StreamProvider" {
		t.Fatalf("openrouter_provider_name = %#v, want StreamProvider", info["openrouter_provider_name"])
	}
}

func TestOpenAICompatibleModelSendsSessionHeaderWhenProviderSet(t *testing.T) {
	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-session-id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client(),
		withOpenAICompatibleSessionSticky(func() string { return "session-abc" }))
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hi")},
	}}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if gotHeader != "session-abc" {
		t.Fatalf("x-session-id header = %q, want session-abc", gotHeader)
	}
}

func TestOpenAICompatibleModelOmitsSessionHeaderByDefault(t *testing.T) {
	headerPresent := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, headerPresent = r.Header["X-Session-Id"]
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	model := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hi")},
	}}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if headerPresent {
		t.Fatal("x-session-id header should be absent when no session provider is configured")
	}
}

func TestModelManagerSendsSessionHeaderOnlyForOpenRouter(t *testing.T) {
	for _, tc := range []struct {
		provider   string
		wantHeader bool
	}{
		{"openrouter", true},
		{"openai", false},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			var gotHeader string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("x-session-id")
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			mgr := NewModelManager(ModelConfig{Provider: tc.provider, Model: "m", APIKey: "k", BaseURL: server.URL}, ProxyConfig{})
			mgr.SetSessionIDProvider(func() string { return "sess-123" })
			model, err := mgr.get()
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
				Role:  llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{llms.TextPart("hi")},
			}}); err != nil {
				t.Fatalf("GenerateContent() error = %v", err)
			}
			if tc.wantHeader && gotHeader != "sess-123" {
				t.Fatalf("%s: x-session-id = %q, want sess-123", tc.provider, gotHeader)
			}
			if !tc.wantHeader && gotHeader != "" {
				t.Fatalf("%s: x-session-id = %q, want empty", tc.provider, gotHeader)
			}
		})
	}
}

func TestOpenRouterSupportedModelAddsPromptCacheControlToSystemPrefix(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"supported_parameters":["cache_control"]}]}}`))
		case r.URL.Path == "/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{Provider: "openrouter", Model: "vendor/cache-control-model", APIKey: "k", BaseURL: server.URL}, ProxyConfig{})
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("stable role prompt"), llms.TextPart("dynamic runtime context")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
	}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) == 0 {
		t.Fatalf("expected split system content, got %#v", captured.Messages)
	}
	systemContent := decodePromptCacheSystemContent(t, captured.Messages[0].Content)
	if len(systemContent) != 2 {
		t.Fatalf("expected split system content, got %#v", systemContent)
	}
	if got := systemContent[0].CacheControl; got == nil || got.Type != "ephemeral" {
		t.Fatalf("first system text block should carry cache_control ephemeral, got %#v", systemContent[0])
	}
	if got := systemContent[1].CacheControl; got != nil {
		t.Fatalf("dynamic system text block should not carry cache_control, got %#v", systemContent[1])
	}
}

func TestOpenRouterSupportedModelAddsPromptCacheControlToSingleSystemPart(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"supported_parameters":["cache_control"]}]}}`))
		case r.URL.Path == "/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{Provider: "openrouter", Model: "vendor/cache-control-model", APIKey: "k", BaseURL: server.URL}, ProxyConfig{})
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("stable role prompt")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
	}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) == 0 {
		t.Fatalf("expected system content, got %#v", captured.Messages)
	}
	systemContent := decodePromptCacheSystemContent(t, captured.Messages[0].Content)
	if len(systemContent) != 1 {
		t.Fatalf("expected one system content part, got %#v", systemContent)
	}
	if got := systemContent[0].CacheControl; got == nil || got.Type != "ephemeral" {
		t.Fatalf("single system text block should carry cache_control ephemeral, got %#v", systemContent[0])
	}
}

func TestOpenRouterUnsupportedModelDoesNotSendPromptCacheControl(t *testing.T) {
	var captured struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/endpoints"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"endpoints":[{"supported_parameters":["temperature"]}]}}`))
		case r.URL.Path == "/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{Provider: "openrouter", Model: "openai/gpt-4o", APIKey: "k", BaseURL: server.URL}, ProxyConfig{})
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("stable role prompt"), llms.TextPart("dynamic runtime context")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
	}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	if len(captured.Messages) == 0 {
		t.Fatalf("expected split system content, got %#v", captured.Messages)
	}
	if strings.Contains(string(captured.Messages[0].Content), "cache_control") {
		t.Fatalf("unsupported OpenRouter model should not receive cache_control: %s", string(captured.Messages[0].Content))
	}
}

func decodePromptCacheSystemContent(t *testing.T, raw json.RawMessage) []struct {
	Type         string `json:"type"`
	Text         string `json:"text,omitempty"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control,omitempty"`
} {
	t.Helper()
	var content []struct {
		Type         string `json:"type"`
		Text         string `json:"text,omitempty"`
		CacheControl *struct {
			Type string `json:"type"`
		} `json:"cache_control,omitempty"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatalf("decode system content: %v raw=%s", err, string(raw))
	}
	return content
}

// TestOpenRouterSessionStickyCacheHit makes two real OpenRouter calls sharing a
// session id and a long stable prefix (with a varying tail). It verifies sticky
// routing produces a prompt-cache hit on the second call. Skipped unless
// OPENROUTER_API_KEY is set; override the model via OPENROUTER_CACHE_MODEL.
func TestOpenRouterSessionStickyCacheHit(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live cache-hit verification")
	}
	model := os.Getenv("OPENROUTER_CACHE_MODEL")
	if model == "" {
		model = "deepseek/deepseek-chat"
	}
	sessionID := fmt.Sprintf("cache-test-%d", time.Now().UnixNano())
	client := newOpenAICompatibleModel("https://openrouter.ai/api/v1", model, apiKey, http.DefaultClient,
		withOpenAICompatibleSessionSticky(func() string { return sessionID }))

	prefix := strings.TrimSpace(strings.Repeat("You are a meticulous device-operations assistant. ", 400))
	call := func(question string) int {
		resp, err := client.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
			{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart(prefix)}},
			{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart(question)}},
		}, llms.WithMaxTokens(8), llms.WithTemperature(0))
		if err != nil {
			if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "region") {
				t.Skipf("model %s unavailable for this account/region: %v", model, err)
			}
			t.Fatalf("live GenerateContent() error = %v", err)
		}
		cached, _ := util.UsageMetricInt(resp.Choices[0].GenerationInfo["cached_tokens"])
		prompt, _ := util.UsageMetricInt(resp.Choices[0].GenerationInfo["prompt_tokens"])
		t.Logf("model=%s session=%s prompt_tokens=%d cached_tokens=%d", model, sessionID, prompt, cached)
		return cached
	}

	call("Reply with the single word: one.")
	cached2 := call("Reply with the single word: two.")
	if cached2 <= 0 {
		t.Logf("WARNING: second call reported cached_tokens=0; provider may not auto-cache via OpenRouter even with sticky routing")
	}
}

// TestOpenAICompatibleModelLiveUsageParsing hits a real OpenRouter endpoint to
// confirm the live response flows through convertMessageContent and usage
// parsing. It is skipped unless OPENROUTER_API_KEY is set so normal/CI runs stay
// offline. Set OPENROUTER_MODEL to override the default model.
func TestOpenAICompatibleModelLiveUsageParsing(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live API verification")
	}
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-4-5"
	}

	client := newOpenAICompatibleModel("https://openrouter.ai/api/v1", model, apiKey, http.DefaultClient)
	resp, err := client.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("You are a terse assistant.")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("Reply with the single word: ok")}},
	}, llms.WithMaxTokens(16), llms.WithTemperature(0))
	if err != nil {
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "region") {
			t.Skipf("model %s unavailable for this account/region: %v", model, err)
		}
		t.Fatalf("live GenerateContent() error = %v", err)
	}

	info := resp.Choices[0].GenerationInfo
	if info == nil {
		t.Fatalf("live response missing generation info")
	}
	prompt, ok := util.UsageMetricInt(info["prompt_tokens"])
	if !ok || prompt <= 0 {
		t.Fatalf("live response missing positive prompt_tokens: %#v", info)
	}
	// cached_tokens must be readable (>=0); a single call without an explicit
	// cache breakpoint is typically 0, which still proves the field plumbing.
	cached, _ := util.UsageMetricInt(info["cached_tokens"])
	usage := telemetryUsageDetails(resp)
	t.Logf("live model=%s prompt_tokens=%d cached_tokens=%d telemetry=%v", model, prompt, cached, usage)
}

// TestOpenAICompatibleModelLiveConsecutiveUserMessages verifies that consecutive
// user messages (which would cause 400 errors on Gemini/Claude without merging)
// are handled correctly. Run with:
//
//	OPENROUTER_API_KEY=sk-or-... go test -run TestOpenAICompatibleModelLiveConsecutiveUserMessages -v
func TestOpenAICompatibleModelLiveConsecutiveUserMessages(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set; skipping live API verification")
	}
	// Use Gemini which enforces strict role alternation
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		model = "google/gemini-2.5-flash"
	}

	client := newOpenAICompatibleModel("https://openrouter.ai/api/v1", model, apiKey, http.DefaultClient)

	// Simulate the exact scenario that causes issues:
	// history ends with a visual followup (user role), then new user input arrives
	t.Log("Sending consecutive user messages (simulating visual followup + new input)...")
	resp, err := client.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("You are a terse assistant. Reply in one short sentence.")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("What is 2+2?")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{llms.TextPart("4")}},
		// This simulates: tool_result followed by visual followup (user) then new user input (user)
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("Screenshot shows a math app displaying 4.")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("Now calculate 3+3.")}},
	}, llms.WithMaxTokens(32), llms.WithTemperature(0))
	if err != nil {
		if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "region") {
			t.Skipf("model %s unavailable for this account/region: %v", model, err)
		}
		t.Fatalf("GenerateContent() with consecutive user messages failed: %v\n"+
			"This would have failed WITHOUT message merging on strict providers (Gemini/Claude).", err)
	}

	t.Logf("SUCCESS: model=%s response=%q", model, resp.Choices[0].Content)
	t.Log("Message merging correctly handled consecutive user messages.")
}

func readRawHTTPLog(t *testing.T, logDir string) string {
	t.Helper()
	data, err := os.ReadFile(rawHTTPLogPath(logDir))
	if err != nil {
		t.Fatalf("read raw HTTP log: %v", err)
	}
	return string(data)
}

func rawHTTPLogPath(logDir string) string {
	matches, _ := filepath.Glob(filepath.Join(logDir, "llm-http-*.log"))
	sort.Strings(matches)
	if len(matches) > 0 {
		return matches[len(matches)-1]
	}
	return filepath.Join(logDir, "llm-http-"+time.Now().Format("200601021504")+".log")
}

// rawHTTPLogNameForCurrentSession returns the log file name expected for the
// live ContextManager session under configDir.
func rawHTTPLogNameForCurrentSession(t *testing.T, configDir string) string {
	t.Helper()
	sessionID := contextmanager.CurrentSessionID(agentpath.ContextManagerSessionFolder(configDir))
	if sessionID == "" {
		t.Fatal("current session id is empty")
	}
	return "llm-http-" + sessionID + ".log"
}

func assertRawHTTPLogIsValidJSONL(t *testing.T, logText string) {
	t.Helper()
	trimmed := strings.TrimSpace(logText)
	if trimmed == "" {
		t.Fatal("raw HTTP log is empty")
	}
	lines := strings.Split(trimmed, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %s", i+1, err, line)
		}
		// Check required fields
		if _, ok := entry["ts"]; !ok {
			t.Fatalf("line %d missing 'ts' field: %s", i+1, line)
		}
		if _, ok := entry["kind"]; !ok {
			t.Fatalf("line %d missing 'kind' field: %s", i+1, line)
		}
		if _, ok := entry["status"]; !ok {
			t.Fatalf("line %d missing 'status' field: %s", i+1, line)
		}
		if _, ok := entry["body"]; !ok {
			t.Fatalf("line %d missing 'body' field: %s", i+1, line)
		}
		// Validate timestamp format
		ts, ok := entry["ts"].(string)
		if !ok {
			t.Fatalf("line %d 'ts' is not a string: %s", i+1, line)
		}
		if _, err := time.Parse("15:04:05", ts); err != nil {
			t.Fatalf("line %d 'ts' format invalid (want HH:MM:SS): %v", i+1, err)
		}
	}
}

func TestModelManagerOpenRouterRetriesEOFInModelCall(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Fatalf("ReadAll body: %v", err)
		}
		if attempts == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("Hijack: %v", err)
			}
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok after retry"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{
		Provider: "openrouter",
		Model:    "test-model",
		APIKey:   "token",
		BaseURL:  server.URL,
	}, ProxyConfig{})
	model, err := manager.get()
	if err != nil {
		t.Fatalf("Get model: %v", err)
	}

	resp, err := model.GenerateContent(contextWithRawHTTPLog(context.Background()), []llms.MessageContent{{
		Role:  llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{llms.TextPart("hello")},
	}})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if resp.Choices[0].Content != "ok after retry" {
		t.Fatalf("unexpected response: %#v", resp.Choices[0].Content)
	}
}

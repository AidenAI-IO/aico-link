package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"

	"aiden-agent/internal/agent/agentpath"
	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/agent/screenprovider"
	speechtext "aiden-agent/internal/agent/speech"
	ttsmodule "aiden-agent/internal/agent/tts"
)

type stubSTTClient struct {
	transcript         string
	transcribeErr      error
	inputs             [][]byte
	supportsStreaming  bool
	streamConfigs      []STTStreamConfig
	streamUploader     *stubSTTStreamUploader
	streamUploaderErr  error
	streamUploaderUsed int
}

type stubSTTStreamUploader struct {
	transcript  string
	finalizeErr error
	closeErr    error
	writes      [][]byte
	finalized   bool
	closed      bool
}

type mappingStateMutatingTool struct {
	name        string
	description string
	output      string
	screen      *screen.ScreenState
	inputs      []string
}

func (t *mappingStateMutatingTool) Name() string { return t.name }

func (t *mappingStateMutatingTool) Description() string { return t.description }

func (t *mappingStateMutatingTool) Call(_ context.Context, input string) (string, error) {
	t.inputs = append(t.inputs, input)
	if t.screen != nil {
		t.screen.UpdateActiveArea(320, 240, screen.ScreenActiveArea{})
	}
	return t.output, nil
}

func (s *stubSTTClient) TranscribeWAV(wavData []byte) (string, error) {
	s.inputs = append(s.inputs, append([]byte(nil), wavData...))
	if s.transcribeErr != nil {
		return "", s.transcribeErr
	}
	return s.transcript, nil
}

func (s *stubSTTClient) Capabilities() STTCapabilities {
	return STTCapabilities{SupportsStreamingUpload: s.supportsStreaming}
}

func (s *stubSTTClient) NewStreamingUploader(_ context.Context, cfg STTStreamConfig) (STTStreamUploader, error) {
	s.streamConfigs = append(s.streamConfigs, cfg)
	if !s.supportsStreaming {
		return nil, fmt.Errorf("streaming upload is not supported")
	}
	if s.streamUploaderErr != nil {
		return nil, s.streamUploaderErr
	}
	if s.streamUploader == nil {
		s.streamUploader = &stubSTTStreamUploader{}
	}
	s.streamUploaderUsed++
	return s.streamUploader, nil
}

func TestStubSTTClientNewStreamingUploaderRequiresStreamingCapability(t *testing.T) {
	client := &stubSTTClient{}

	_, err := client.NewStreamingUploader(context.Background(), STTStreamConfig{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err == nil {
		t.Fatal("expected streaming capability error")
	}
}

func (s *stubSTTStreamUploader) UploadPCM(pcm []byte) error {
	s.writes = append(s.writes, append([]byte(nil), pcm...))
	return nil
}

func (s *stubSTTStreamUploader) Finalize() (string, error) {
	s.finalized = true
	if s.finalizeErr != nil {
		return "", s.finalizeErr
	}
	return s.transcript, nil
}

func (s *stubSTTStreamUploader) Close() error {
	s.closed = true
	return s.closeErr
}

func firstMessageOfType(messages []Message, messageType string) (Message, bool) {
	for _, message := range messages {
		if message.Type == messageType {
			return message, true
		}
	}
	return Message{}, false
}

func TestChatResultPollingPreservesReasoningReset(t *testing.T) {
	const requestID = "reasoning-reset"
	pending := &chatPendingResult{}
	for _, event := range []RunEvent{
		{Type: runEventReasoningDelta, ReasoningContent: "stale thought"},
		{Type: runEventReasoningReset},
		{Type: runEventReasoningDelta, ReasoningContent: "replacement thought"},
	} {
		pending.messages = append(pending.messages, pollingMessageFromReasoningEvent(event, "episode-1", requestID))
	}
	server := &Server{pendingResults: map[string]*chatPendingResult{requestID: pending}}

	req := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
	rec := httptest.NewRecorder()
	server.handleChatResult(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll status = %d body=%s", rec.Code, rec.Body.String())
	}

	var response ChatResultResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	wantTypes := []string{"assistant_reasoning_delta", "assistant_reasoning_reset", "assistant_reasoning_delta"}
	if len(response.Messages) != len(wantTypes) {
		t.Fatalf("poll messages = %#v, want %d reasoning events", response.Messages, len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if response.Messages[index].Type != wantType {
			t.Fatalf("poll message %d type = %q, want %q", index, response.Messages[index].Type, wantType)
		}
	}
	if response.Messages[1].ReasoningContent != "" {
		t.Fatalf("reset reasoning content = %q, want empty reset marker", response.Messages[1].ReasoningContent)
	}
}

func TestServerHandleChatReturnsToolHistory(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, "Let me read the current volume."),
			contentResponse("The current audio volume is 42."),
		},
	}
	tool := &stubTool{
		name:        "audio_volume",
		description: "Get the current audio playback volume.",
		output:      `{"volume":42}`,
	}
	streamingDisabled := false
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when external state is requested.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": tool,
		}},
		NewSkillIndex(),
	)
	defer runtime.Close()
	server := newServerForTest(runtime)

	body := bytes.NewBufferString(`{"message":"What is the current volume?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}

	// Poll for result
	var resp ChatResultResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.Status != "complete" {
		t.Fatalf("result never completed: status=%q", resp.Status)
	}
	if resp.Response != "The current audio volume is 42." {
		t.Fatalf("unexpected response: %q", resp.Response)
	}
	if len(resp.History) < 4 {
		t.Fatalf("expected at least 4 history entries for single-agent tool flow, got %d", len(resp.History))
	}

	user, ok := firstMessageOfType(resp.History, "user")
	if !ok || user.Content != "What is the current volume?" {
		t.Fatalf("unexpected user history message: %#v", resp.History)
	}
	toolCall, ok := firstMessageOfType(resp.History, runEventToolCall)
	if !ok || toolCall.ToolName != "audio_volume" || !strings.Contains(toolCall.ToolInput, "{}") {
		t.Fatalf("unexpected tool_call message: %#v", resp.History)
	}
	if toolCall.Content != "Let me read the current volume." {
		t.Fatalf("unexpected tool_call content: %#v", toolCall)
	}
	toolResult, ok := firstMessageOfType(resp.History, "tool_result")
	if !ok || toolResult.ToolName != "audio_volume" || toolResult.Content != `{"volume":42}` {
		t.Fatalf("unexpected tool_result message: %#v", resp.History)
	}
	assistant, ok := firstMessageOfType(resp.History, "assistant")
	if !ok || assistant.Content != "The current audio volume is 42." {
		t.Fatalf("unexpected assistant message: %#v", resp.History)
	}
}

func TestServerContextHistoryReturnsPersistedToolResultContent(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("call_1", "shell", `{"command":"read config.py"}`),
			contentResponse("Done"),
		},
	}
	tool := &stubTool{
		name:        "shell",
		description: "Run a shell command.",
		output:      strings.Repeat(" ", 4001),
	}
	streamingDisabled := false
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when requested.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"shell": tool}},
		NewSkillIndex(),
	)
	if plane, ok := runtime.memoryPlane.(*FilesystemMemoryPlane); ok {
		plane.StopEpisodeMemory()
	}
	// This test exercises persisted chat history only. Disable the unrelated
	// asynchronous Episode maintenance so TempDir cleanup cannot race its writes.
	runtime.memoryPlane = nil
	defer runtime.Close()
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"Read the large config"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d body=%s", rec.Code, rec.Body.String())
	}
	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}

	var result ChatResultResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+startResp["request_id"], nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code == http.StatusOK {
			if err := json.NewDecoder(resultRec.Body).Decode(&result); err != nil {
				t.Fatalf("decode result response: %v", err)
			}
			if result.Status == "complete" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Status != "complete" {
		t.Fatalf("result never completed: status=%q", result.Status)
	}
	waitForServerRequestFinished(t, server, startResp["request_id"])

	const want = 4001
	resultToolMessage, ok := firstMessageOfType(result.History, "tool_result")
	if !ok || len(resultToolMessage.Content) != want {
		t.Fatalf("/api/chat/result tool result length = %d, want %d", len(resultToolMessage.Content), want)
	}

	reloaded := newServerForTest(runtime)
	historyReq := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	historyRec := httptest.NewRecorder()
	reloaded.handleHistory(historyRec, historyReq)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", historyRec.Code, historyRec.Body.String())
	}
	var history []Message
	if err := json.NewDecoder(historyRec.Body).Decode(&history); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	historyToolMessage, ok := firstMessageOfType(history, "tool_result")
	if !ok || len(historyToolMessage.Content) != want {
		t.Fatalf("/api/history tool result length = %d, want %d", len(historyToolMessage.Content), want)
	}
}

func TestServerPersistsContextBackedChatHistory(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	memoryDir := filepath.Join(configDir, "memory")
	model := &scriptedModel{
		responses: roleDirectResponses("Completed"),
	}
	streamingDisabled := false
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:                configDir,
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Answer directly.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
			VoiceToolCallSpeech:      &streamingDisabled,
		},
		&testModelResolver{model: model},
		NewMemoryManager(memoryDir),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"Do a task"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}

	// Poll for result
	var resp ChatResultResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.Status != "complete" {
		t.Fatalf("result never completed: status=%q", resp.Status)
	}
	user, ok := firstMessageOfType(resp.History, "user")
	if !ok || user.Content != "Do a task" {
		t.Fatalf("context history missing user message: %#v", resp.History)
	}
	assistant, ok := firstMessageOfType(resp.History, "assistant")
	if !ok || assistant.Content != "Completed" {
		t.Fatalf("context history missing assistant message: %#v", resp.History)
	}

	reloaded := newServerForTest(runtime)
	historyReq := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	historyRec := httptest.NewRecorder()
	reloaded.handleHistory(historyRec, historyReq)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("unexpected history status: %d body=%s", historyRec.Code, historyRec.Body.String())
	}
	var restored []Message
	if err := json.NewDecoder(historyRec.Body).Decode(&restored); err != nil {
		t.Fatalf("decode restored history: %v", err)
	}
	restoredAssistant, ok := firstMessageOfType(restored, "assistant")
	if !ok || restoredAssistant.Content != "Completed" {
		t.Fatalf("restored context missing assistant response: %#v", restored)
	}
}

func TestServerHandleChatStreamsToolAndAssistantMessages(t *testing.T) {
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponse("call_1", "audio_volume", `{"__arg1":"{}","description":"Let me read the current volume."}`),
			&llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "The current audio volume is 42.", ReasoningContent: "read the volume"}}},
		},
	}
	configDir, err := os.MkdirTemp("", "aiden-server-chat-stream-*")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	configDir = ensureTestConfigDir(t, configDir)
	t.Cleanup(func() { _ = os.RemoveAll(configDir) })
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:   configDir,
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when external state is requested.",
		},
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	defer runtime.Close()
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"What is the current volume?"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("X-Aiden-Stream", "ndjson")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("Content-Type = %q, want application/x-ndjson", contentType)
	}

	var events []ChatStreamEvent
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		var event ChatStreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode stream event %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}

	var sawToolCall, sawToolResult, sawAssistant, sawDone, sawReasoning bool
	reasoningIndex, assistantIndex := -1, -1
	for eventIndex, event := range events {
		if event.Type == "assistant_reasoning_delta" && event.ReasoningContent == "read the volume" {
			sawReasoning = true
			reasoningIndex = eventIndex
		}
		if event.Type == "message" && event.Message != nil {
			switch event.Message.Type {
			case runEventToolCall:
				sawToolCall = event.Message.ToolName == "audio_volume"
			case "tool_result":
				sawToolResult = event.Message.ToolName == "audio_volume" && event.Message.Content == `{"volume":42}`
			case "assistant":
				sawAssistant = event.Message.Content == "The current audio volume is 42." && event.Message.ReasoningContent == "read the volume"
				if assistantIndex < 0 {
					assistantIndex = eventIndex
				}
			}
		}
		if event.Type == "done" && event.Response == "The current audio volume is 42." {
			sawDone = true
		}
	}
	if !sawToolCall || !sawToolResult || !sawAssistant || !sawDone || !sawReasoning || reasoningIndex >= assistantIndex {
		t.Fatalf("missing expected stream events: tool_call=%v tool_result=%v assistant=%v reasoning=%v done=%v events=%#v",
			sawToolCall, sawToolResult, sawAssistant, sawReasoning, sawDone, events)
	}
}

func TestServerHandleChatStreamsLeadingToolSpeechWithoutDuplicatePlayback(t *testing.T) {
	const requestID = "streaming-tts-cleanup"
	toolSpeech := true
	toolContent := "<tts>Check volume.</tts>\nChecking volume."
	finalContent := "<tts>Current volume is 42.</tts>\nCurrent volume is 42."
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, toolContent),
			contentResponse(finalContent),
		},
		streamChunks: [][]string{
			{toolContent},
			{finalContent},
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when external state is requested.",
			VoiceStreamingTTSEnabled: boolPtr(true),
			VoiceToolCallSpeech:      &toolSpeech,
			Audio:                    AudioConfig{SampleRate: 16000},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	defer runtime.Close()
	server := newServerForTest(runtime)
	provider := &flushRecordingTTSProvider{name: "server-provider"}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))
	time.Sleep(30 * time.Millisecond)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"What is the current volume?","request_id":"`+requestID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("X-Aiden-Stream", "ndjson")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	waitForFlushProviderTextCount(t, provider, 2)
	time.Sleep(100 * time.Millisecond)
	texts := provider.texts()
	if len(texts) != 2 {
		writes, flushCalls := provider.activity()
		t.Fatalf("tool-event and final TTS should each play once, got %#v writes=%#v flush_calls=%d", texts, writes, flushCalls)
	}
	if countString(texts, "Check volume.") != 1 {
		t.Fatalf("tool TTS count = %d, want 1: %#v", countString(texts, "Check volume."), texts)
	}
	if countString(texts, "Current volume is 42.") != 1 {
		t.Fatalf("final TTS count = %d, want 1: %#v", countString(texts, "Current volume is 42."), texts)
	}
	if texts[0] != "Check volume." || texts[1] != "Current volume is 42." {
		t.Fatalf("TTS playback order = %#v, want tool progress before final response", texts)
	}
	if outputs := server.snapshotActiveOutputs(requestID); len(outputs) != 0 {
		t.Fatalf("active TTS outputs after streaming response = %d, want 0", len(outputs))
	}
}

func TestServerAsyncChatUnregistersStreamingTTSOutput(t *testing.T) {
	const requestID = "async-tts-cleanup"
	output := "<tts>Done.</tts>\nDone."
	model := &scriptedModel{
		responses:    roleDirectResponses(output),
		streamChunks: [][]string{{output}},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Answer directly.",
			VoiceStreamingTTSEnabled: boolPtr(true),
			Audio:                    AudioConfig{SampleRate: 16000},
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	defer runtime.Close()
	server := newServerForTest(runtime)
	server.ttsManager = ttsmodule.NewProviderManager(&recordingTTSProvider{name: "server-provider"}, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"finish it","request_id":"`+requestID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleChat status = %d body=%s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code == http.StatusOK {
			var result ChatResultResponse
			if err := json.NewDecoder(resultRec.Body).Decode(&result); err == nil && result.Status == "complete" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForServerRequestFinished(t, server, requestID)
	if outputs := server.snapshotActiveOutputs(requestID); len(outputs) != 0 {
		t.Fatalf("active TTS outputs after async response = %d, want 0", len(outputs))
	}
}

type failingStreamWriter struct {
	err error
}

func (w failingStreamWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestStreamFanoutWriterReturnsInputLengthWhenLaterWriterFails(t *testing.T) {
	writeErr := errors.New("fanout write failed")
	var first strings.Builder
	fanout := newStreamFanoutWriter(&first, failingStreamWriter{err: writeErr})

	n, err := fanout.Write([]byte("chunk"))

	if !errors.Is(err, writeErr) {
		t.Fatalf("Write() error = %v, want %v", err, writeErr)
	}
	if n != len("chunk") {
		t.Fatalf("Write() bytes = %d, want %d", n, len("chunk"))
	}
	if first.String() != "chunk" {
		t.Fatalf("first writer received %q, want chunk", first.String())
	}
}

func TestStreamFanoutWriterResetsAssistantDeltaDraftBeforeFallback(t *testing.T) {
	writeErr := errors.New("fanout write failed")
	rec := httptest.NewRecorder()
	stream, ok := newChatStreamWriter(rec)
	if !ok {
		t.Fatal("httptest recorder must support streaming")
	}
	fanout := newStreamFanoutWriter(
		newChatAssistantStreamWriter(stream, "episode-reset", "request-reset"),
		failingStreamWriter{err: writeErr},
	)

	if _, err := fanout.Write([]byte("Partial")); !errors.Is(err, writeErr) {
		t.Fatalf("first Write() error = %v, want %v", err, writeErr)
	}
	resetStreamWriterState(fanout)
	if _, err := fanout.Write([]byte("Complete answer.")); !errors.Is(err, writeErr) {
		t.Fatalf("fallback Write() error = %v, want %v", err, writeErr)
	}

	var events []ChatStreamEvent
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for scanner.Scan() {
		var event ChatStreamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode stream event %q: %v", scanner.Text(), err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}

	resetIndex := -1
	for i, event := range events {
		if event.Type == "assistant_delta_reset" {
			if resetIndex >= 0 {
				t.Fatalf("expected one assistant_delta_reset, events=%#v", events)
			}
			resetIndex = i
		}
	}
	if resetIndex <= 0 || resetIndex >= len(events)-1 {
		t.Fatalf("assistant_delta_reset index = %d, want between deltas: %#v", resetIndex, events)
	}

	var beforeReset strings.Builder
	var afterReset strings.Builder
	for i, event := range events {
		if event.Type != "assistant_delta" {
			continue
		}
		if i < resetIndex {
			beforeReset.WriteString(event.Delta)
		} else {
			afterReset.WriteString(event.Delta)
		}
	}
	if beforeReset.String() != "Partial" {
		t.Fatalf("delta before reset = %q, want Partial", beforeReset.String())
	}
	if afterReset.String() != "Complete answer." {
		t.Fatalf("delta after reset = %q, want Complete answer.", afterReset.String())
	}
}

func TestStreamFanoutWriterReportsAnyChildEmission(t *testing.T) {
	var webDelta strings.Builder
	var speech strings.Builder

	fanout := newStreamFanoutWriter(
		&webDelta,
		speechtext.NewStreamWriter(&speech),
	)
	tracker, ok := fanout.(streamOutputTracker)
	if !ok {
		t.Fatal("fanout writer must track stream emission")
	}

	if _, err := fanout.Write([]byte("<tts>Short answer.</tts>\nComplete answer.")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if webDelta.String() != "<tts>Short answer.</tts>\nComplete answer." {
		t.Fatalf("web delta stream = %q", webDelta.String())
	}
	if speech.String() != "Short answer." {
		t.Fatalf("speech stream = %q, want Short answer.", speech.String())
	}
	if !tracker.StreamEmitted() {
		t.Fatal("fanout should report emitted when any child stream emitted")
	}
}

func TestServerEventStreamAllowsRunEventMessages(t *testing.T) {
	for _, messageType := range []string{
		"user",
		"assistant",
		runEventToolCall,
		"tool_result",
	} {
		if !shouldStreamEventMessage(Message{Type: messageType}) {
			t.Fatalf("message type %q should be streamed to web clients", messageType)
		}
	}
	for _, messageType := range []string{"role_output", "episode_status", "todo_update", "todo_closed"} {
		if shouldStreamEventMessage(Message{Type: messageType}) {
			t.Fatalf("message type %q should not be streamed to web clients", messageType)
		}
	}
}

func TestHandleCoordinateDebugTap(t *testing.T) {
	currentScreen := &screen.ScreenState{}
	currentScreen.UpdateActiveArea(1280, 720, screen.ScreenActiveArea{X: 0, Y: 72, Width: 1280, Height: 576, Valid: true})
	tool := &mappingStateMutatingTool{
		name:        "touch_gesture",
		description: "Touch gesture tool.",
		output:      `{"width":1280,"height":576,"format":"jpeg","size":4,"data":"ZmFrZQ==","action_output":"ok"}`,
		screen:      currentScreen,
	}
	toolSet := &ToolSet{
		tools: map[string]langtools.Tool{
			"touch_gesture": tool,
		},
		screen: currentScreen,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		toolSet,
		NewSkillIndex(),
	)
	bridge := newTestPhoneBridge(t)
	bridge.hidConnectionState = func() (bool, bool) { return false, false }
	bridge.hidMonitorEnabled = false
	runtime.phoneBridge = bridge
	server := newServerForTest(runtime)
	if err := bridge.ApplyBenchmarkStatus(PhoneBridgeStatus{
		Connected: true,
		Platform:  "ios",
		PhoneID:   "coordinate-debug-phone",
		Environment: &PhoneEnvironment{Screen: screen.PhoneScreenInfo{
			NativeWidthPixels:  intPtr(1179),
			NativeHeightPixels: intPtr(2556),
		}},
	}); err != nil {
		t.Fatalf("ApplyBenchmarkStatus() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":123,"y":456,"type":"double_tap"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleCoordinateDebugTap(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(tool.inputs) != 1 {
		t.Fatalf("touch_gesture call count = %d, want 1", len(tool.inputs))
	}

	var input map[string]any
	if err := json.Unmarshal([]byte(tool.inputs[0]), &input); err != nil {
		t.Fatalf("decode tool input: %v", err)
	}
	if got := input["type"]; got != "double_tap" {
		t.Fatalf("gesture type = %#v, want double_tap", got)
	}
	if _, exists := input["coord_space"]; exists {
		t.Fatalf("coordinate debug input must not include coord_space: %#v", input)
	}
	point, ok := input["point"].(map[string]any)
	if !ok {
		t.Fatalf("point missing or invalid: %#v", input["point"])
	}
	if point["x"] != float64(123) || point["y"] != float64(456) {
		t.Fatalf("point = %#v, want x=123 y=456", point)
	}

	var resp coordinateDebugTapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.ActionType != "double_tap" {
		t.Fatalf("unexpected response: %#v", resp)
	}
	if resp.Screenshot == nil || resp.Screenshot.Width != 1280 || resp.Screenshot.Height != 576 || resp.Screenshot.Data != "ZmFrZQ==" {
		t.Fatalf("unexpected screenshot payload: %#v", resp.Screenshot)
	}
	if resp.Screenshot.SourceWidth != 1280 || resp.Screenshot.SourceHeight != 720 {
		t.Fatalf("unexpected screenshot source dimensions: %#v", resp.Screenshot)
	}
	if resp.Screenshot.SourceActiveArea == nil || *resp.Screenshot.SourceActiveArea != (screen.ScreenActiveArea{X: 0, Y: 72, Width: 1280, Height: 576, Valid: true}) {
		t.Fatalf("unexpected screenshot active area: %#v", resp.Screenshot.SourceActiveArea)
	}
	if resp.Screenshot.OriginalScreenWidthPixels == nil || *resp.Screenshot.OriginalScreenWidthPixels != 1179 {
		t.Fatalf("unexpected original screen width: %#v", resp.Screenshot.OriginalScreenWidthPixels)
	}
	if resp.Screenshot.OriginalScreenHeightPixels == nil || *resp.Screenshot.OriginalScreenHeightPixels != 2556 {
		t.Fatalf("unexpected original screen height: %#v", resp.Screenshot.OriginalScreenHeightPixels)
	}
	width, height, active, _, ok := toolSet.screen.ActiveAreaWithAge()
	if !ok || width != 1280 || height != 720 {
		t.Fatalf("screen state dimensions = %dx%d ok=%v, want 1280x720 true", width, height, ok)
	}
	if active != (screen.ScreenActiveArea{X: 0, Y: 72, Width: 1280, Height: 576, Valid: true}) {
		t.Fatalf("screen state active area = %+v", active)
	}
}

func TestHandleCoordinateDebugTapRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	server := newServerForTest(&Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{}}})
	for _, body := range []string{
		`{"x":123,"y":456,"coord_space":"pixel"}`,
		`{"x":123,"y":456} {"x":1,"y":2}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		server.handleCoordinateDebugTap(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q status = %d, want %d; response=%s", body, rec.Code, http.StatusBadRequest, rec.Body.String())
		}
	}
}

func TestHandleCoordinateDebugTapMapsStructuredToolErrorStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toolErr    *ToolError
		wantStatus int
	}{
		{name: "invalid input", toolErr: NewToolError(CodeInvalidArguments, "bad tap"), wantStatus: http.StatusBadRequest},
		{name: "module unavailable", toolErr: NewToolError(CodeModuleUnavailable, "hid unavailable"), wantStatus: http.StatusServiceUnavailable},
		{name: "execution failed", toolErr: NewToolError(CodeToolExecutionFailed, "hid write failed"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toolSet := &ToolSet{tools: map[string]langtools.Tool{
				"touch_gesture": &contextToolErrorStub{name: "touch_gesture", toolErr: tc.toolErr},
			}}
			runtime := NewRuntimeWithDeps(
				withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
				&testModelResolver{},
				NewMemoryManager(""),
				toolSet,
				NewSkillIndex(),
			)
			server := newServerForTest(runtime)

			req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":123,"y":456}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			server.handleCoordinateDebugTap(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.toolErr.Message) {
				t.Fatalf("body = %s, want message %q", rec.Body.String(), tc.toolErr.Message)
			}
		})
	}
}

func TestServerHandleChatStreamBroadcastsRequestID(t *testing.T) {
	model := &scriptedModel{
		responses: roleDirectResponses("Hello!"),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Answer directly.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)
	messages := server.eventBroadcaster.Subscribe()
	defer server.eventBroadcaster.Unsubscribe(messages)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"hello","request_id":"web-req-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("X-Aiden-Stream", "ndjson")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var broadcast []Message
	for len(messages) > 0 {
		broadcast = append(broadcast, <-messages)
	}
	user, ok := firstMessageOfType(broadcast, "user")
	if !ok {
		t.Fatalf("missing user broadcast: %#v", broadcast)
	}
	if user.RequestID != "web-req-1" {
		t.Fatalf("user request_id = %q, want web-req-1", user.RequestID)
	}
	assistant, ok := firstMessageOfType(broadcast, "assistant")
	if !ok {
		t.Fatalf("missing assistant broadcast: %#v", broadcast)
	}
	if assistant.RequestID != "web-req-1" {
		t.Fatalf("assistant request_id = %q, want web-req-1", assistant.RequestID)
	}
	state := server.liveActivity.Snapshot("web-req-1")
	if state == nil || state.Status != LiveActivityStatusCompleted {
		t.Fatalf("stream live activity state = %#v, want completed", state)
	}
}

func TestHandleCoordinateDebugTapRejectsInvalidType(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":100,"y":200,"type":"swipe"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleCoordinateDebugTap(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); !strings.Contains(body, fmt.Sprintf("%q", "unsupported tap type")) {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func TestHandleCoordinateDebugTapRejectsNonJSONContentType(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":100,"y":200,"type":"tap"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	server.handleCoordinateDebugTap(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); !strings.Contains(body, fmt.Sprintf("%q", "Content-Type must be application/json")) {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func postProviderScreenshot(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/screenshot", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleProviderScreenshot(rec, req)
	return rec
}

func decodeProviderScreenshot(t *testing.T, rec *httptest.ResponseRecorder) (meta map[string]any, capture map[string]any, image []byte) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var payload struct {
		OK   bool `json:"ok"`
		Data struct {
			Meta        map[string]any `json:"meta"`
			CaptureInfo map[string]any `json:"capture_info"`
			Image       string         `json:"image"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode provider screenshot: %v body=%s", err, rec.Body.String())
	}
	if !payload.OK {
		t.Fatalf("provider screenshot failed: %s", rec.Body.String())
	}
	image, err := base64.StdEncoding.DecodeString(payload.Data.Image)
	if err != nil {
		t.Fatalf("decode provider image: %v", err)
	}
	return payload.Data.Meta, payload.Data.CaptureInfo, image
}

func TestHandleProviderScreenshotCanDisableBlackBarCropping(t *testing.T) {
	frameSocket := startFakeFrameServiceSocket(t, func(req map[string]any) (string, []byte) {
		method, _ := req["method"].(string)
		if method == "health" {
			return `{"type":"response","method":"health","status":"OK","state":"RUNNING","latest_seq":1,"frame_age_ms":10}`, nil
		}
		if method != "latest_frame" {
			t.Fatalf("unexpected method: %#v", req["method"])
		}
		if format, _ := req["format"].(string); format != "jpeg" {
			t.Fatalf("expected jpeg format request when crop_black=false, got %#v", req["format"])
		}
		if cropBlack, _ := req["crop_black"].(bool); cropBlack {
			t.Fatalf("crop_black = %#v, want false", req["crop_black"])
		}
		jpegData, err := encodeJPEG([]byte{255, 255, 255, 0, 0, 0}, 2, 1, screenshotJPEGQuality)
		if err != nil {
			t.Fatalf("encode jpeg fixture: %v", err)
		}
		header := fmt.Sprintf(`{"type":"response","method":"latest_frame","status":"OK","frame":{"seq":1,"width":2,"height":1,"pixel_format":"jpeg","stride":0,"bytes":%d,"stale":false}}`, len(jpegData))
		return header, jpegData
	})

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			HID:   HIDConfig{FrameSocket: frameSocket},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	rec := postProviderScreenshot(t, server, `{"format":"jpeg","quality":80,"crop_black":false,"minimal_width":0}`)
	meta, _, imageBytes := decodeProviderScreenshot(t, rec)
	if width, _ := meta["width"].(float64); width != 2 {
		t.Fatalf("meta.width = %#v, want 2", meta["width"])
	}
	if height, _ := meta["height"].(float64); height != 1 {
		t.Fatalf("meta.height = %#v, want 1", meta["height"])
	}

	img, err := jpeg.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatalf("decode response jpeg: %v", err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("decoded bounds = %v, want 2x1", bounds)
	}
}

func TestHandleProviderScreenshotReturnsCropMetadata(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 9))
	for y := 0; y < 9; y++ {
		for x := 5; x < 10; x++ {
			img.Set(x, y, color.RGBA{R: 240, G: 240, B: 240, A: 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode jpeg fixture: %v", err)
	}
	jpegData := jpegBuf.Bytes()

	frameSocket := startFakeFrameServiceSocket(t, func(req map[string]any) (string, []byte) {
		if method, _ := req["method"].(string); method == "health" {
			return `{"type":"response","method":"health","status":"OK","state":"RUNNING","latest_seq":1,"frame_age_ms":10}`, nil
		}
		if format, _ := req["format"].(string); format != "jpeg" {
			t.Fatalf("expected jpeg format request, got %#v", req["format"])
		}
		if cropBlack, _ := req["crop_black"].(bool); !cropBlack {
			t.Fatalf("crop_black = %#v, want true", req["crop_black"])
		}
		header := fmt.Sprintf(`{"type":"response","method":"latest_frame","status":"OK","frame":{"seq":1,"width":5,"height":9,"source_width":16,"source_height":9,"crop_x":5,"crop_y":0,"crop_width":5,"crop_height":9,"pixel_format":"jpeg","stride":0,"bytes":%d,"stale":false}}`, len(jpegData))
		return header, jpegData
	})

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			HID:   HIDConfig{FrameSocket: frameSocket},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{FrameSocket: frameSocket}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	rec := postProviderScreenshot(t, server, `{"format":"jpeg","quality":80,"crop_black":true,"minimal_width":0}`)
	meta, _, _ := decodeProviderScreenshot(t, rec)
	if width, _ := meta["width"].(float64); width != 5 {
		t.Fatalf("meta.width = %#v, want 5", meta["width"])
	}
	if sourceWidth, _ := meta["source_width"].(float64); sourceWidth != 16 {
		t.Fatalf("meta.source_width = %#v, want 16", meta["source_width"])
	}
	if cropX, _ := meta["crop_x"].(float64); cropX != 5 {
		t.Fatalf("meta.crop_x = %#v, want 5", meta["crop_x"])
	}
}

func TestFrameServiceClientSendsRawCropBlackOption(t *testing.T) {
	frameSocket := startFakeFrameServiceSocket(t, func(req map[string]any) (string, []byte) {
		if format, _ := req["format"].(string); format != "raw" {
			t.Fatalf("format = %#v, want raw", req["format"])
		}
		if cropBlack, _ := req["crop_black"].(bool); !cropBlack {
			t.Fatalf("crop_black = %#v, want true", req["crop_black"])
		}
		if minimalWidth, _ := req["minimal_width"].(float64); minimalWidth != 608 {
			t.Fatalf("minimal_width = %#v, want 608", req["minimal_width"])
		}
		header := `{"type":"response","method":"latest_frame","status":"OK","frame":{"seq":1,"width":2,"height":1,"source_width":4,"source_height":1,"crop_x":2,"crop_y":0,"crop_width":2,"crop_height":1,"pixel_format":"uyvy","stride":4,"bytes":4,"stale":false}}`
		return header, []byte{128, 235, 128, 235}
	})

	client := NewFrameServiceClient(frameSocket)
	meta, data, err := client.LatestFrameWithFormat("raw", 0, true, screenprovider.CropHint{MinimalWidth: 608})
	if err != nil {
		t.Fatalf("LatestFrameWithFormat() error = %v", err)
	}
	if meta.Width != 2 || meta.SourceWidth != 4 || meta.CropX != 2 || meta.CropWidth != 2 {
		t.Fatalf("unexpected raw crop metadata: %#v", meta)
	}
	if len(data) != 4 {
		t.Fatalf("raw payload length = %d, want 4", len(data))
	}
}

func TestHandleProviderScreenshotIncludesADBDeviceWhenFallbackUsed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script uses /bin/sh")
	}

	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}

	tmpDir := t.TempDir()
	pngPath := filepath.Join(tmpDir, "screen.png")
	if err := os.WriteFile(pngPath, pngBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", pngPath, err)
	}

	adbPath := filepath.Join(tmpDir, "adb")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"devices\" ] && [ \"$2\" = \"-l\" ]; then\n" +
		"  printf 'List of devices attached\\nserial123\\tdevice product:panther model:Pixel_7_Pro device:panther transport_id:1\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"-s\" ] && [ \"$2\" = \"serial123\" ] && [ \"$3\" = \"exec-out\" ] && [ \"$4\" = \"screencap\" ] && [ \"$5\" = \"-p\" ]; then\n" +
		"  cat \"$AIDEN_TEST_PNG\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"unexpected args: $@\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(adbPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", adbPath, err)
	}

	t.Setenv("AIDEN_ADB_PATH", adbPath)
	t.Setenv("AIDEN_TEST_PNG", pngPath)
	t.Setenv("AIDEN_ADB_SERIAL", "")
	t.Setenv("ANDROID_SERIAL", "")
	defer clearAutoConfiguredADBSerial("serial123")

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			HID:   HIDConfig{FrameSocket: filepath.Join(tmpDir, "missing-frame.sock")},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	rec := postProviderScreenshot(t, server, `{"format":"jpeg","quality":80,"crop_black":true,"minimal_width":0}`)
	_, capture, imageBytes := decodeProviderScreenshot(t, rec)
	if backend, _ := capture["capture_backend"].(string); backend != "adb" {
		t.Fatalf("capture_backend = %#v, want adb", capture["capture_backend"])
	}
	device, _ := capture["adb_device"].(map[string]any)
	if device == nil {
		t.Fatal("expected adb_device in capture_info")
	}
	if serial, _ := device["serial"].(string); serial != "serial123" {
		t.Fatalf("adb serial = %#v, want serial123", device["serial"])
	}
	if name, _ := device["name"].(string); name != "Pixel 7 Pro" {
		t.Fatalf("adb name = %#v, want Pixel 7 Pro", device["name"])
	}
	if state, _ := device["state"].(string); state != "device" {
		t.Fatalf("adb state = %#v, want device", device["state"])
	}

	decoded, err := jpeg.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		t.Fatalf("jpeg.Decode() error = %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("decoded jpeg bounds = %v, want 2x1", bounds)
	}
}

func TestHandleCoordinateDebugTapRecapturesUncroppedScreenshot(t *testing.T) {
	frameSocket := startFakeFrameServiceSocket(t, func(req map[string]any) (string, []byte) {
		if method, _ := req["method"].(string); method == "health" {
			return `{"type":"response","method":"health","status":"OK","state":"RUNNING","latest_seq":2,"frame_age_ms":10}`, nil
		}
		if format, _ := req["format"].(string); format != "jpeg" {
			t.Fatalf("expected jpeg format request when crop_black_bars=false, got %#v", req["format"])
		}
		if cropBlack, _ := req["crop_black"].(bool); cropBlack {
			t.Fatalf("crop_black = %#v, want false", req["crop_black"])
		}
		jpegData, err := encodeJPEG([]byte{255, 255, 255, 0, 0, 0}, 2, 1, screenshotJPEGQuality)
		if err != nil {
			t.Fatalf("encode jpeg fixture: %v", err)
		}
		header := fmt.Sprintf(`{"type":"response","method":"latest_frame","status":"OK","frame":{"seq":2,"width":2,"height":1,"pixel_format":"jpeg","stride":0,"bytes":%d,"stale":false}}`, len(jpegData))
		return header, jpegData
	})
	tool := &stubTool{
		name:        "touch_gesture",
		description: "Touch gesture tool.",
		output:      `{"width":1,"height":1,"format":"jpeg","size":4,"data":"ZmFrZQ==","action_output":"ok"}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			HID:   HIDConfig{FrameSocket: frameSocket},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"touch_gesture": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":123,"y":456,"type":"tap","crop_black_bars":false}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleCoordinateDebugTap(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp coordinateDebugTapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Screenshot == nil {
		t.Fatal("expected screenshot in response")
	}
	if resp.Screenshot.Width != 2 || resp.Screenshot.Height != 1 {
		t.Fatalf("unexpected screenshot dimensions: %#v", resp.Screenshot)
	}
	if resp.Screenshot.Data == "ZmFrZQ==" {
		t.Fatalf("expected recaptured screenshot instead of tool screenshot: %#v", resp.Screenshot)
	}

	jpegBytes, err := base64.StdEncoding.DecodeString(resp.Screenshot.Data)
	if err != nil {
		t.Fatalf("decode screenshot base64: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("decode screenshot jpeg: %v", err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("decoded bounds = %v, want 2x1", bounds)
	}
}

func TestHandleCoordinateDebugTapRecapturesScreenshotWhenMappingUnavailable(t *testing.T) {
	rgb := make([]byte, 5*9*3)
	for i := range rgb {
		rgb[i] = 255
	}
	jpegData, err := encodeJPEG(rgb, 5, 9, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	frameSocket := startFakeFrameServiceSocket(t, func(req map[string]any) (string, []byte) {
		if method, _ := req["method"].(string); method == "health" {
			return `{"type":"response","method":"health","status":"OK","state":"RUNNING","latest_seq":2,"frame_age_ms":10}`, nil
		}
		if format, _ := req["format"].(string); format != "jpeg" {
			t.Fatalf("expected jpeg format request when remapping cropped screenshot, got %#v", req["format"])
		}
		header := fmt.Sprintf(`{"type":"response","method":"latest_frame","status":"OK","frame":{"seq":2,"width":5,"height":9,"source_width":16,"source_height":9,"crop_x":5,"crop_y":0,"crop_width":5,"crop_height":9,"pixel_format":"jpeg","stride":0,"bytes":%d,"stale":false}}`, len(jpegData))
		return header, jpegData
	})
	tool := &stubTool{
		name:        "touch_gesture",
		description: "Touch gesture tool.",
		output:      `{"width":1,"height":1,"format":"jpeg","size":4,"data":"ZmFrZQ==","action_output":"ok"}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			HID:   HIDConfig{FrameSocket: frameSocket},
		}),
		&testModelResolver{},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"touch_gesture": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodPost, "/api/coordinate-debug/tap", bytes.NewBufferString(`{"x":123,"y":456,"type":"tap","crop_black_bars":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleCoordinateDebugTap(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp coordinateDebugTapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Screenshot == nil {
		t.Fatal("expected screenshot in response")
	}
	if resp.Screenshot.Data == "ZmFrZQ==" {
		t.Fatalf("expected recaptured screenshot instead of tool screenshot: %#v", resp.Screenshot)
	}
	if resp.Screenshot.Width != 5 || resp.Screenshot.Height != 9 {
		t.Fatalf("unexpected screenshot dimensions: %#v", resp.Screenshot)
	}
	if resp.Screenshot.SourceWidth != 16 || resp.Screenshot.SourceHeight != 9 {
		t.Fatalf("unexpected screenshot source dimensions: %#v", resp.Screenshot)
	}
	want := &screen.ScreenActiveArea{X: 5, Y: 0, Width: 5, Height: 9, Valid: true}
	if resp.Screenshot.SourceActiveArea == nil || *resp.Screenshot.SourceActiveArea != *want {
		t.Fatalf("unexpected screenshot active area: %#v", resp.Screenshot.SourceActiveArea)
	}
}

func TestServerSpeakToolContentUsesTTS(t *testing.T) {
	provider := &recordingTTSProvider{name: "server-provider"}
	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{SampleRate: 16000}}),
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
	}

	server.speakToolContent(context.Background(), " Let me read the current volume. ")

	if got := provider.texts(); len(got) != 1 || got[0] != "Let me read the current volume." {
		t.Fatalf("unexpected TTS texts: %#v", got)
	}
}

func TestServerAsyncChatAppendsVoiceNotificationOnlyToFinalSpeech(t *testing.T) {
	streamingDisabled := false
	model := &scriptedModel{responses: roleDirectResponses("Done.\n<tts>Done.</tts>")}
	cfg := DefaultConfig()
	cfg.Model = ModelConfig{Provider: "fake"}
	cfg.Instruction = "Answer directly."
	cfg.Locale = "zh-CN"
	cfg.VoiceStreamingTTSEnabled = &streamingDisabled
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, cfg),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime.Close() error = %v", err)
		}
	})
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), VoiceNotificationEvent{
		Code: "storage", Severity: SeverityWarning, State: VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	server := newServerForTest(runtime)
	provider := &recordingTTSProvider{name: "server-provider"}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"finish it"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleChat status = %d body=%s", rec.Code, rec.Body.String())
	}
	var start map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&start); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := start["request_id"]
	if requestID == "" {
		t.Fatal("missing request_id")
	}

	deadline := time.Now().Add(2 * time.Second)
	var result ChatResultResponse
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code == http.StatusOK {
			result = ChatResultResponse{}
			if err := json.NewDecoder(resultRec.Body).Decode(&result); err == nil && result.Status == "complete" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if result.Status != "complete" {
		t.Fatalf("chat result = %#v", result)
	}
	if strings.Contains(result.Response, "存储空间") {
		t.Fatalf("display response was changed by voice notification: %q", result.Response)
	}
	waitForProviderTextCount(t, provider, 1)
	if got := provider.texts(); len(got) != 1 || got[0] != "Done.另外提醒一下，设备存储空间不足。" {
		t.Fatalf("spoken texts = %#v", got)
	}
	waitForServerRequestFinished(t, server, requestID)
}

func TestServerAsyncChatSpeaksReplacementForFinalLLMFailure(t *testing.T) {
	streamingDisabled := false
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Answer directly.",
			Locale:                   "zh-CN",
			VoiceStreamingTTSEnabled: &streamingDisabled,
		}),
		&testModelResolver{model: failingGenerateModel{err: errors.New("dial tcp: network is unreachable")}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime.Close() error = %v", err)
		}
	})
	server := newServerForTest(runtime)
	provider := &recordingTTSProvider{name: "server-provider"}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"finish it"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChat(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleChat status = %d body=%s", rec.Code, rec.Body.String())
	}
	var start map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&start); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := start["request_id"]
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code == http.StatusOK {
			var result ChatResultResponse
			if err := json.NewDecoder(resultRec.Body).Decode(&result); err == nil && result.Status == "error" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForProviderTextCount(t, provider, 1)
	if got := provider.texts(); len(got) != 1 || got[0] != "当前网络不可用，暂时无法完成这个请求。" {
		t.Fatalf("spoken replacement = %#v", got)
	}
	waitForServerRequestFinished(t, server, requestID)
}

func waitForServerRequestFinished(t *testing.T, server *Server, requestID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.activeRunsMu.Lock()
		_, active := server.activeRuns[requestID]
		server.activeRunsMu.Unlock()
		if !active {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("request %q was still active after final speech", requestID)
}

func TestServerHandleChatDoesNotWaitForToolContentTTSWhenEnabled(t *testing.T) {
	speech := "Read volume."
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, "Reading volume.\n<tts>"+speech+"</tts>"),
			contentResponse("The current audio volume is 42."),
		},
	}
	streamingDisabled := false
	toolSpeechEnabled := true
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when external state is requested.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
			VoiceToolCallSpeech:      &toolSpeechEnabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	defer runtime.Close()
	server := newServerForTest(runtime)
	provider := &blockingTTSProvider{started: make(chan struct{}), blockText: speech}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient("/tmp/audio.sock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"What is the current volume?"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		server.handleChat(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("handleChat waited for tool content TTS")
	}
	select {
	case <-provider.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tool content TTS was not started")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var resp ChatResultResponse
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServerHandleChatDoesNotSpeakWaitForWakeup(t *testing.T) {
	toolSpeechEnabled := true
	streamingEnabled := true
	toolContent := "Going idle.\n<tts>Standby mode.</tts>"
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("wait_1", "wait_for_wakeup", `{"reason":"user asked"}`, toolContent),
		},
		streamChunks: [][]string{{toolContent}},
	}
	controller := NewWaitForWakeupController()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools.",
			VoiceStreamingTTSEnabled: &streamingEnabled,
			VoiceToolCallSpeech:      &toolSpeechEnabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"wait_for_wakeup": NewWaitForWakeupTool(controller),
		}},
		NewSkillIndex(),
	)
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime.Close() error = %v", err)
		}
	})
	server := newServerForTest(runtime)
	provider := &recordingTTSProvider{name: "server-provider"}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"go to sleep"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}
	assertNoProviderTextWithin(t, provider, 200*time.Millisecond)
	waitForServerRequestFinished(t, server, requestID)
}

func TestServerHandleChatSkipsToolContentTTSWhenDisabled(t *testing.T) {
	toolContent := "Reading volume.\n<tts>Let me read the current volume.</tts>"
	finalContent := "<tts>The current audio volume is 42.</tts>\nThe current audio volume is 42."
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, toolContent),
			contentResponse(finalContent),
		},
		streamChunks: [][]string{{toolContent}, {finalContent}},
	}
	streamingEnabled := true
	toolSpeechDisabled := false
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when external state is requested.",
			VoiceStreamingTTSEnabled: &streamingEnabled,
			VoiceToolCallSpeech:      &toolSpeechDisabled,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("runtime.Close() error = %v", err)
		}
	})
	server := newServerForTest(runtime)
	provider := &recordingTTSProvider{name: "server-provider"}
	server.ttsManager = ttsmodule.NewProviderManager(provider, nil)
	server.audioClient = NewAudioServiceClient(startTTSPlaybackAudioSocket(t))

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"What is the current volume?"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}

	completed := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			t.Fatalf("unexpected result status: %d body=%s", resultRec.Code, resultRec.Body.String())
		}
		var resp ChatResultResponse
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			completed = true
			break
		}
		if resp.Status == "error" {
			t.Fatalf("chat request failed: %s", resp.Error)
		}
		if resp.Status == "running" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !completed {
		t.Fatalf("chat request did not complete before deadline: request_id=%s", requestID)
	}
	waitForServerRequestFinished(t, server, requestID)
	waitForProviderTextCount(t, provider, 1)
	if got := provider.texts(); len(got) != 1 || got[0] != "The current audio volume is 42." {
		t.Fatalf("spoken texts = %#v, want final answer only", got)
	}
}

func TestServerShouldSpeakToolCallRequiresTTSTag(t *testing.T) {
	toolSpeechEnabled := true
	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{VoiceToolCallSpeech: &toolSpeechEnabled, Model: ModelConfig{Provider: "fake"}}),
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
	}

	if server.shouldSpeakToolCall(RunEvent{Type: runEventToolCall, ToolName: "recall_memory", Content: "I will check your preferences first."}) {
		t.Fatal("plain recall_memory tool content should not be spoken")
	}
	if server.shouldSpeakToolCall(RunEvent{Type: runEventToolCall, ToolName: "screenshot", Content: "I will inspect the screen first."}) {
		t.Fatal("plain screenshot tool content should not be spoken")
	}
	if server.shouldSpeakToolCall(RunEvent{Type: runEventToolCall, ToolName: "audio_volume", Content: "Check the current volume."}) {
		t.Fatal("plain audio_volume tool content should not be spoken")
	}
	if !server.shouldSpeakToolCall(RunEvent{Type: runEventToolCall, ToolName: "audio_volume", Content: "Checking.\n<tts>Check the current volume.</tts>"}) {
		t.Fatal("tagged audio_volume tool content should be spoken")
	}
}

func TestServerHandleChatCancelCancelsActiveRun(t *testing.T) {
	server := &Server{
		logger:             newTestLogger(),
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	server.registerActiveRun("req-1", cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":" req-1 "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChatCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("active run was not canceled")
	}

	var resp ChatCancelResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "canceled" || resp.RequestID != "req-1" {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	if !server.isRequestTerminated("req-1") {
		t.Fatal("active request was not marked terminated")
	}
	server.unregisterActiveRun("req-1")
	if server.isRequestTerminated("req-1") {
		t.Fatal("termination marker remained after active run cleanup")
	}
}

func TestServerHandleChatCancelUnknownRequestDoesNotRetainTermination(t *testing.T) {
	server := &Server{
		logger:             newTestLogger(),
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"req-unknown"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChatCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChatCancelResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "not_running" || resp.RequestID != "req-unknown" {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	if server.isRequestTerminated("req-unknown") {
		t.Fatal("unknown request left a termination marker")
	}
}

func markRequestTerminatedForTest(server *Server, requestID string) {
	server.terminatedRequestsMu.Lock()
	defer server.terminatedRequestsMu.Unlock()
	server.markRequestTerminatedLocked(requestID)
}

func TestServerUnregisterActiveRunClearsTermination(t *testing.T) {
	server := &Server{
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	if !server.registerActiveRun("req-finished", cancel) {
		t.Fatal("registerActiveRun() failed")
	}
	markRequestTerminatedForTest(server, "req-finished")
	server.unregisterActiveRun("req-finished")

	if server.isRequestTerminated("req-finished") {
		t.Fatal("finished request left a termination marker")
	}
}

func TestServerUnregisterActiveOutputClearsTermination(t *testing.T) {
	server := &Server{terminatedRequests: make(map[string]struct{})}
	output := newActiveTTSOutput(nil)
	unregister := server.registerActiveOutput("req-output-finished", output)
	markRequestTerminatedForTest(server, "req-output-finished")
	unregister()

	if server.isRequestTerminated("req-output-finished") {
		t.Fatal("finished output left a termination marker")
	}
}

func TestServerRegisterActiveOutputRejectsTerminatedRequest(t *testing.T) {
	requestID := "req-output-after-cancel"
	server := &Server{terminatedRequests: make(map[string]struct{})}
	markRequestTerminatedForTest(server, requestID)

	outputCtx, cancelOutput := context.WithCancel(context.Background())
	output := newActiveTTSOutput(cancelOutput)
	unregister := server.registerActiveOutput(requestID, output)
	defer unregister()

	select {
	case <-outputCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("terminated request output was not interrupted")
	}
	if outputs := server.snapshotActiveOutputs(requestID); len(outputs) != 0 {
		t.Fatalf("terminated request registered %d active outputs, want 0", len(outputs))
	}
}

func TestServerTerminationMarkerWaitsForAllRequestOwnedWork(t *testing.T) {
	requestID := "req-multiple-resources"
	server := &Server{
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	if !server.registerActiveRun(requestID, cancel) {
		t.Fatal("registerActiveRun() failed")
	}
	output := newActiveTTSOutput(nil)
	unregisterOutput := server.registerActiveOutput(requestID, output)
	markRequestTerminatedForTest(server, requestID)

	server.unregisterActiveRun(requestID)
	if !server.isRequestTerminated(requestID) {
		t.Fatal("termination marker was cleared while output was still active")
	}

	unregisterOutput()
	if server.isRequestTerminated(requestID) {
		t.Fatal("termination marker remained after all request work finished")
	}
}

func TestServerHandleChatCancelConcurrentRequests(t *testing.T) {
	const requestCount = 256

	server := &Server{activeRuns: make(map[string]context.CancelFunc)}
	requestIDs := make([]string, requestCount)
	runDone := make([]<-chan struct{}, requestCount)
	for i := range requestCount {
		requestID := fmt.Sprintf("req-concurrent-cancel-%d", i)
		ctx, cancel := context.WithCancel(context.Background())
		if !server.registerActiveRun(requestID, cancel) {
			t.Fatalf("registerActiveRun(%q) failed", requestID)
		}
		requestIDs[i] = requestID
		runDone[i] = ctx.Done()
	}

	start := make(chan struct{})
	errs := make(chan error, requestCount)
	var wg sync.WaitGroup
	for _, requestID := range requestIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			req := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"`+requestID+`"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			server.handleChatCancel(rec, req)
			if rec.Code != http.StatusOK {
				errs <- fmt.Errorf("cancel %q returned status %d: %s", requestID, rec.Code, rec.Body.String())
				return
			}
			var resp ChatCancelResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				errs <- fmt.Errorf("decode cancel %q response: %w", requestID, err)
				return
			}
			if resp.Status != "canceled" || resp.RequestID != requestID {
				errs <- fmt.Errorf("cancel %q response = %#v", requestID, resp)
				return
			}
			server.unregisterActiveRun(requestID)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	for i, done := range runDone {
		select {
		case <-done:
		default:
			t.Errorf("request %q was not canceled", requestIDs[i])
		}
	}
	server.terminatedRequestsMu.RLock()
	defer server.terminatedRequestsMu.RUnlock()
	if server.terminatedRequests != nil {
		t.Fatalf("termination marker map retained %d entries", len(server.terminatedRequests))
	}
}

func TestServerReusedRequestTerminationSurvivesPreviousCleanup(t *testing.T) {
	const requestID = "req-reused-during-cleanup"

	server := &Server{activeRuns: make(map[string]context.CancelFunc)}
	if !server.registerActiveRun(requestID, func() {}) {
		t.Fatal("registerActiveRun() failed for previous lifecycle")
	}
	if !server.cancelActiveRun(requestID) {
		t.Fatal("cancelActiveRun() failed for previous lifecycle")
	}

	// Hold output inspection so the previous lifecycle cleanup overlaps the
	// registration and cancellation of the reused request ID.
	server.activeOutputsMu.Lock()
	previousCleanupDone := make(chan struct{})
	go func() {
		server.unregisterActiveRun(requestID)
		close(previousCleanupDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		server.activeRunsMu.Lock()
		_, active := server.activeRuns[requestID]
		server.activeRunsMu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			server.activeOutputsMu.Unlock()
			t.Fatal("previous lifecycle did not release its active run")
		}
		time.Sleep(time.Millisecond)
	}
	// Give cleanup time to reach the intentionally blocked output check.
	time.Sleep(10 * time.Millisecond)

	registered := make(chan bool, 1)
	go func() {
		registered <- server.registerActiveRun(requestID, func() {})
	}()
	newLifecycleCanceled := false
	select {
	case ok := <-registered:
		if !ok {
			server.activeOutputsMu.Unlock()
			t.Fatal("registerActiveRun() failed for reused lifecycle")
		}
		if !server.cancelActiveRun(requestID) {
			server.activeOutputsMu.Unlock()
			t.Fatal("cancelActiveRun() failed for reused lifecycle")
		}
		newLifecycleCanceled = true
	case <-time.After(20 * time.Millisecond):
		// The current lock ordering blocks registration until cleanup finishes.
	}
	server.activeOutputsMu.Unlock()

	select {
	case <-previousCleanupDone:
	case <-time.After(time.Second):
		t.Fatal("previous lifecycle cleanup did not finish")
	}
	if !newLifecycleCanceled {
		select {
		case ok := <-registered:
			if !ok {
				t.Fatal("registerActiveRun() failed for reused lifecycle")
			}
		case <-time.After(time.Second):
			t.Fatal("reused lifecycle registration did not finish")
		}
		if !server.cancelActiveRun(requestID) {
			t.Fatal("cancelActiveRun() failed for reused lifecycle")
		}
	}

	if !server.isRequestTerminated(requestID) {
		t.Fatal("previous lifecycle cleanup removed the reused lifecycle marker")
	}
	server.unregisterActiveRun(requestID)
}

func TestServerReleasesTerminationMarkerStorageAfterBurst(t *testing.T) {
	const requestCount = 10_000

	server := &Server{activeRuns: make(map[string]context.CancelFunc)}
	requestIDs := make([]string, requestCount)
	for i := range requestCount {
		requestID := fmt.Sprintf("req-marker-burst-%d", i)
		if !server.registerActiveRun(requestID, func() {}) {
			t.Fatalf("registerActiveRun(%q) failed", requestID)
		}
		if !server.cancelActiveRun(requestID) {
			t.Fatalf("cancelActiveRun(%q) failed", requestID)
		}
		requestIDs[i] = requestID
	}

	server.terminatedRequestsMu.RLock()
	markerCount := len(server.terminatedRequests)
	server.terminatedRequestsMu.RUnlock()
	if markerCount != requestCount {
		t.Fatalf("termination marker count = %d, want %d", markerCount, requestCount)
	}

	for _, requestID := range requestIDs {
		server.unregisterActiveRun(requestID)
	}
	server.terminatedRequestsMu.RLock()
	defer server.terminatedRequestsMu.RUnlock()
	if server.terminatedRequests != nil {
		t.Fatalf("termination marker map retained %d entries after burst cleanup", len(server.terminatedRequests))
	}
}

func TestServerCloseClearsTerminationMarkers(t *testing.T) {
	server := &Server{
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !server.registerActiveRun("req-closed", cancel) {
		t.Fatal("registerActiveRun() failed")
	}
	markRequestTerminatedForTest(server, "req-closed")

	server.Close()

	server.terminatedRequestsMu.Lock()
	count := len(server.terminatedRequests)
	server.terminatedRequestsMu.Unlock()
	if count != 0 {
		t.Fatalf("termination marker count after Close() = %d, want 0", count)
	}
}

func TestServerCloseCancelsActiveRunsAndOutputs(t *testing.T) {
	server := &Server{activeRuns: make(map[string]context.CancelFunc)}
	runCtx, cancelRun := context.WithCancel(context.Background())
	server.registerActiveRun("req-close", cancelRun)

	outputCtx, cancelOutput := context.WithCancel(context.Background())
	output := newActiveTTSOutput(cancelOutput)
	unregisterOutput := server.registerActiveOutput("req-close", output)
	defer unregisterOutput()

	server.Close()

	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Server.Close() did not cancel the active run")
	}
	select {
	case <-outputCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Server.Close() did not interrupt the active output")
	}

	lateRunCtx, cancelLateRun := context.WithCancel(context.Background())
	defer cancelLateRun()
	if server.registerActiveRun("req-after-close", cancelLateRun) {
		t.Fatal("Server accepted an active run after Close()")
	}
	select {
	case <-lateRunCtx.Done():
		t.Fatal("rejected late run was unexpectedly canceled by Server")
	default:
	}

	lateOutputCtx, cancelLateOutput := context.WithCancel(context.Background())
	lateOutput := newActiveTTSOutput(cancelLateOutput)
	cleanupLateOutput := server.registerActiveOutput("req-after-close", lateOutput)
	defer cleanupLateOutput()
	select {
	case <-lateOutputCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Server did not interrupt an output registered after Close()")
	}
}

func TestServerBroadcastsStreamingReasoningForVoiceRuns(t *testing.T) {
	server := &Server{eventBroadcaster: NewEventBroadcaster()}
	events := server.eventBroadcaster.Subscribe()
	defer server.eventBroadcaster.Unsubscribe(events)

	server.BroadcastMessage(Message{
		Type:             "assistant",
		Status:           "streaming",
		Source:           "voice",
		RequestID:        "voice-request",
		ReasoningContent: "first thought",
	})

	select {
	case message := <-events:
		if message.ReasoningContent != "first thought" || message.RequestID != "voice-request" {
			t.Fatalf("broadcast message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("streaming voice reasoning was not broadcast")
	}
}

func TestServerCloseStopsAcceptingAndDrainsActiveHTTPHandlers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve HTTP address: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release HTTP address: %v", err)
	}

	server := &Server{
		addr:             addr,
		eventBroadcaster: NewEventBroadcaster(),
		activeRuns:       make(map[string]context.CancelFunc),
	}
	startDone := make(chan error, 1)
	go func() {
		startDone <- server.Start()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", addr, 20*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP server did not start: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/api/events")
	if err != nil {
		t.Fatalf("open SSE request: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read SSE response: %v", err)
	}

	closeDone := make(chan struct{})
	go func() {
		server.Close()
		close(closeDone)
	}()

	returnedBeforeHandlerDrained := false
	select {
	case <-closeDone:
		returnedBeforeHandlerDrained = true
	case <-time.After(50 * time.Millisecond):
	}

	if err := resp.Body.Close(); err != nil {
		t.Errorf("close SSE response: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Close() did not finish after the active handler drained")
	}
	if returnedBeforeHandlerDrained {
		t.Error("Server.Close() returned before the active HTTP handler drained")
	}

	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Server.Start() after Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Server.Start() did not return after Close()")
	}

	conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("HTTP server still accepted connections after Close()")
	}
}

func TestServerHandleChatCancelEndsDanglingLiveActivity(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		activeRuns:   make(map[string]context.CancelFunc),
		liveActivity: NewLiveActivityManager(LiveActivityConfig{}, "en-US", newTestLogger()),
	}
	server.liveActivity.StartTask("req-1", "External run")

	req := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"req-1"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChatCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChatCancelResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "canceled" || resp.RequestID != "req-1" {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	state := server.liveActivity.Snapshot("req-1")
	if state == nil || state.Status != LiveActivityStatusCanceled || state.CanStop {
		t.Fatalf("live activity state = %#v, want canceled", state)
	}
}

func TestServerHandleChatCancelStopsRequestScopedTTSPlayback(t *testing.T) {
	requestID := "req-stop-1"
	audioOps := &recordedAudioOps{}
	provider := newInterruptibleAudioTTSProvider("server-provider", 48000, true)
	server := &Server{logger: newTestLogger(),
		activeRuns:  make(map[string]context.CancelFunc),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun(requestID, cancel)
	done := make(chan error, 1)
	go func() {
		done <- server.speakTextForRequest(ctx, requestID, "final answer", 0)
	}()
	waitForTestSignal(t, provider.firstWriteDone(), "final TTS playback to start")
	deadline := time.Now().Add(500 * time.Millisecond)
	for audioOps.countOp("start_playback") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("final TTS playback never opened a playback session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"`+requestID+`"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleChatCancel(cancelRec, cancelReq)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("unexpected cancel status: %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	var resp ChatCancelResponse
	if err := json.NewDecoder(cancelRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if resp.Status != "canceled" || resp.RequestID != requestID {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	if got := audioOps.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want 1", got)
	}
	if got := audioOps.finalChunkCountAfterFirstStop(); got != 0 {
		t.Fatalf("final write_play_chunk count after stop = %d, want 0", got)
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("speakTextForRequest() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("request-scoped TTS did not stop after cancel")
	}
}

func TestServerHandleChatCancelStopsCompletedAsyncRequestTTSPlayback(t *testing.T) {
	requestID := "req-stop-async-complete"
	audioOps := &recordedAudioOps{}
	provider := newInterruptibleAudioTTSProvider("server-provider", 48000, true)
	server := &Server{logger: newTestLogger(),
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
		ttsManager:         ttsmodule.NewProviderManager(provider, nil),
		audioClient:        NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun(requestID, cancel)

	playbackDone := make(chan error, 1)
	go func() {
		playbackDone <- server.speakTextForRequest(ctx, requestID, "completed async final answer", 0)
	}()

	waitForTestSignal(t, provider.firstWriteDone(), "completed async final TTS playback to start")
	deadline := time.Now().Add(500 * time.Millisecond)
	for audioOps.countOp("start_playback") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("completed async final TTS playback never opened a playback session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	server.unregisterActiveRun(requestID)

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"`+requestID+`"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleChatCancel(cancelRec, cancelReq)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("unexpected cancel status: %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	var resp ChatCancelResponse
	if err := json.NewDecoder(cancelRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if resp.Status != "canceled" || resp.RequestID != requestID {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	if got := audioOps.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want 1", got)
	}
	if got := audioOps.finalChunkCountAfterFirstStop(); got != 0 {
		t.Fatalf("final write_play_chunk count after stop = %d, want 0", got)
	}
	select {
	case err := <-playbackDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("speakTextForRequest() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("completed async final request-scoped TTS did not stop after cancel")
	}
}

func TestSpeakTextForRequestRefusesTerminatedRequest(t *testing.T) {
	requestID := "req-terminated"
	audioOps := &recordedAudioOps{}
	provider := newInterruptibleAudioTTSProvider("server-provider", 48000, true)
	server := &Server{logger: newTestLogger(),
		activeRuns:         make(map[string]context.CancelFunc),
		terminatedRequests: make(map[string]struct{}),
		ttsManager:         ttsmodule.NewProviderManager(provider, nil),
		audioClient:        NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
	}
	markRequestTerminatedForTest(server, requestID)

	err := server.speakTextForRequest(context.Background(), requestID, "should not play", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("speakTextForRequest() error = %v, want context canceled", err)
	}
	if got := audioOps.countOp("start_playback"); got != 0 {
		t.Fatalf("start_playback count = %d, want 0", got)
	}
}

func TestServerHandleChatCancelStopsRequestScopedStreamingTTSPlayback(t *testing.T) {
	requestID := "req-stop-streaming"
	audioOps := &recordedAudioOps{}
	provider := newInterruptibleAudioTTSProvider("server-provider", 48000, true)
	server := &Server{logger: newTestLogger(),
		activeRuns:  make(map[string]context.CancelFunc),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun(requestID, cancel)

	stream, err := beginManagedTTSStream(ctx, server.ttsManager, server.currentTTSPlaybackBackend(), Config{})
	if err != nil {
		t.Fatalf("beginManagedTTSStream() error = %v", err)
	}
	output := newActiveTTSOutput(nil)
	output.setStream(stream)
	unregisterOutput := server.registerActiveOutput(requestID, output)
	defer unregisterOutput()

	writeDone := make(chan error, 1)
	go func() {
		_, writeErr := speechtext.NewStreamWriter(stream).Write([]byte("<tts>streaming final answer</tts>"))
		writeDone <- writeErr
	}()

	waitForTestSignal(t, provider.firstWriteDone(), "request-scoped streaming TTS playback to start")
	deadline := time.Now().Add(500 * time.Millisecond)
	for audioOps.countOp("start_playback") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("streaming TTS playback never opened a playback session")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/cancel", bytes.NewBufferString(`{"request_id":"`+requestID+`"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleChatCancel(cancelRec, cancelReq)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("unexpected cancel status: %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	var resp ChatCancelResponse
	if err := json.NewDecoder(cancelRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if resp.Status != "canceled" || resp.RequestID != requestID {
		t.Fatalf("unexpected cancel response: %#v", resp)
	}
	if got := audioOps.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want 1", got)
	}
	if got := audioOps.finalChunkCountAfterFirstStop(); got != 0 {
		t.Fatalf("final write_play_chunk count after stop = %d, want 0", got)
	}
	select {
	case err := <-writeDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("stream Write() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("request-scoped streaming TTS did not stop after cancel")
	}
	if err := stream.closeAndWait(); err != nil {
		t.Fatalf("closeAndWait() error = %v", err)
	}
}

func TestServerHandleChatSteerQueuesAndCancelsPendingMessage(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		activeRuns:    make(map[string]context.CancelFunc),
		pendingSteers: make(map[string]pendingSteerMessage),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun("req-1", cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/chat/steer", bytes.NewBufferString(`{"request_id":" req-1 ","message":" change direction "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChatSteer(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp ChatSteerResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != "pending" || resp.RequestID != "req-1" || resp.Steer == nil || resp.Steer.Content != "change direction" {
		t.Fatalf("unexpected steer response: %#v", resp)
	}
	steer, ok := server.consumePendingSteer("req-1")
	if !ok || steer.Content != "change direction" {
		t.Fatalf("unexpected consumed steer: %#v ok=%v", steer, ok)
	}
	if _, ok := server.consumePendingSteer("req-1"); ok {
		t.Fatal("pending steer was not consumed exactly once")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/chat/steer", bytes.NewBufferString(`{"request_id":"req-1","message":"cancel this"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	server.handleChatSteer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected requeue status: %d body=%s", rec.Code, rec.Body.String())
	}

	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/steer/cancel", bytes.NewBufferString(`{"request_id":"req-1"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleChatSteerCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("unexpected cancel status: %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelResp ChatSteerResponse
	if err := json.NewDecoder(cancelRec.Body).Decode(&cancelResp); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	if cancelResp.Status != "canceled" || cancelResp.RequestID != "req-1" {
		t.Fatalf("unexpected cancel response: %#v", cancelResp)
	}
	if _, ok := server.consumePendingSteer("req-1"); ok {
		t.Fatal("canceled steer was still pending")
	}
}

func TestServerSteerSignalRemainsObservableUntilSteerIsConsumed(t *testing.T) {
	server := &Server{
		logger:        newTestLogger(),
		activeRuns:    make(map[string]context.CancelFunc),
		pendingSteers: make(map[string]pendingSteerMessage),
		steerSignals:  make(map[string]chan struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun("req-1", cancel)
	server.resetSteerSignal("req-1")

	req := httptest.NewRequest(http.MethodPost, "/api/chat/steer", bytes.NewBufferString(`{"request_id":"req-1","message":"change direction"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleChatSteer(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	lateSubscriber := server.steerSignalChannel("req-1")
	if lateSubscriber == nil {
		t.Fatal("late interrupt subscriber received nil after steer was signaled")
	}
	select {
	case <-lateSubscriber:
		// A subscriber installed after the HTTP request must still observe it.
	default:
		t.Fatal("late interrupt subscriber did not observe queued steer")
	}
}

func TestServerCancelPendingSteerRearmsInterruptForNextSteer(t *testing.T) {
	server := &Server{
		logger:        newTestLogger(),
		activeRuns:    make(map[string]context.CancelFunc),
		pendingSteers: make(map[string]pendingSteerMessage),
		steerSignals:  make(map[string]chan struct{}),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun("req-1", cancel)
	server.resetSteerSignal("req-1")

	queue := func(message string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/chat/steer", bytes.NewBufferString(`{"request_id":"req-1","message":"`+message+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.handleChatSteer(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("queue steer status: %d body=%s", rec.Code, rec.Body.String())
		}
	}

	queue("first")
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/chat/steer/cancel", bytes.NewBufferString(`{"request_id":"req-1"}`))
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelRec := httptest.NewRecorder()
	server.handleChatSteerCancel(cancelRec, cancelReq)
	if cancelRec.Code != http.StatusOK {
		t.Fatalf("cancel steer status: %d body=%s", cancelRec.Code, cancelRec.Body.String())
	}

	nextInterrupt := server.steerSignalChannel("req-1")
	if nextInterrupt == nil {
		t.Fatal("canceling a pending steer did not rearm the interrupt channel")
	}
	select {
	case <-nextInterrupt:
		t.Fatal("rearmed interrupt channel was already closed")
	default:
	}

	queue("second")
	select {
	case <-nextInterrupt:
		// The next steer must still interrupt the active run.
	default:
		t.Fatal("second steer did not close the rearmed interrupt channel")
	}
}

func TestServerStaleSignalDoesNotCloseRearmedChannelWithoutPendingSteer(t *testing.T) {
	server := &Server{
		pendingSteers: make(map[string]pendingSteerMessage),
		steerSignals:  make(map[string]chan struct{}),
	}
	server.resetSteerSignal("req-1")
	server.pendingSteers["req-1"] = pendingSteerMessage{RequestID: "req-1", Content: "first"}
	if !server.cancelPendingSteer("req-1") {
		t.Fatal("cancelPendingSteer() = false, want true")
	}

	// Model a delayed signal from the canceled setPendingSteer call.
	server.signalSteer("req-1")
	interrupt := server.steerSignalChannel("req-1")
	if interrupt == nil {
		t.Fatal("rearmed interrupt channel is nil")
	}
	select {
	case <-interrupt:
		t.Fatal("stale signal closed channel without a pending steer")
	default:
	}
}

func TestServerResetSignalKeepsQueuedSteerObservable(t *testing.T) {
	server := &Server{
		pendingSteers: make(map[string]pendingSteerMessage),
		steerSignals:  make(map[string]chan struct{}),
	}
	server.pendingSteers["req-1"] = pendingSteerMessage{RequestID: "req-1", Content: "second"}

	// Model a reset racing after a second steer has already been queued.
	server.resetSteerSignal("req-1")
	interrupt := server.steerSignalChannel("req-1")
	if interrupt == nil {
		t.Fatal("reset interrupt channel is nil")
	}
	select {
	case <-interrupt:
		// A queued steer must remain observable after any reset.
	default:
		t.Fatal("reset signal lost an already queued steer")
	}
}

func TestServerHandleChatSteerRejectsNonRunningRequest(t *testing.T) {
	server := &Server{logger: newTestLogger(), activeRuns: make(map[string]context.CancelFunc)}
	req := httptest.NewRequest(http.MethodPost, "/api/chat/steer", bytes.NewBufferString(`{"request_id":"req-1","message":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChatSteer(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWebUISteerModeControlsArePresent(t *testing.T) {
	chatScript := readWebUIResource(t, "scripts/chat.js")
	index := readWebUIResource(t, "index.html")
	for _, want := range []string{
		"/api/chat/steer",
		"/api/chat/steer/cancel",
		"async function submitSteerMessage()",
	} {
		if !strings.Contains(chatScript, want) {
			t.Fatalf("web UI missing %q", want)
		}
	}
	if !strings.Contains(index, "id=\"pendingSteer\"") {
		t.Fatal("web UI missing pending steer controls")
	}
	if !strings.Contains(readWebUIResource(t, "scripts/messages.js"), "sendBtn.textContent = currentChatRequestId ? 'Steer' : 'Send';") {
		t.Fatal("web UI missing steer composer state")
	}
}

func TestWebUIClearMenuUsesConfirmedSessionActions(t *testing.T) {
	index := readWebUIResource(t, "index.html")
	chatScript := readWebUIResource(t, "scripts/chat.js")

	for _, want := range []string{
		`id="clearMenu"`,
		"Clear Session",
		"Clear Session &amp; Memory",
		`onclick="clearSession()"`,
		`onclick="clearSessionAndMemory()"`,
	} {
		if !strings.Contains(index, want) {
			t.Errorf("web UI clear menu missing %q", want)
		}
	}
	for _, want := range []string{
		"async function clearSession()",
		"async function clearSessionAndMemory()",
		"if (!confirm(",
		"fetch('/api/clear', { method: 'POST' })",
		"fetch('/api/clear-all', { method: 'POST' })",
	} {
		if !strings.Contains(chatScript, want) {
			t.Errorf("web UI clear action missing %q", want)
		}
	}
	for _, unwanted := range []string{">New Chat</button>", ">Reset Memory</button>"} {
		if strings.Contains(index, unwanted) {
			t.Errorf("web UI still contains old action %q", unwanted)
		}
	}
}

func TestWebUIIsEmbeddedFromStaticResource(t *testing.T) {
	want, err := os.ReadFile("web_ui/index.html")
	if err != nil {
		t.Fatalf("read web UI static resource: %v", err)
	}
	if webUI != string(want) {
		t.Fatal("embedded web UI differs from web_ui/index.html")
	}

	for _, asset := range []string{
		"/web-ui/styles.css",
		"/web-ui/scripts/state.js",
		"/web-ui/scripts/events.js",
		"/web-ui/scripts/tools.js",
		"/web-ui/scripts/chat.js",
		"/web-ui/scripts/messages.js",
		"/web-ui/scripts/attachments.js",
		"/web-ui/scripts/tool_messages.js",
		"/web-ui/scripts/bootstrap.js",
	} {
		if !strings.Contains(webUI, asset) {
			t.Errorf("web UI index missing asset %q", asset)
		}
	}
}

func TestServerServesEmbeddedWebUIAssets(t *testing.T) {
	server := &Server{logger: newTestLogger(), bridge: NewPhoneBridge(newTestLogger())}
	handler := server.Handler()
	for _, test := range []struct {
		path        string
		contentType string
		content     string
	}{
		{path: "/web-ui/styles.css", contentType: "text/css", content: ":root {"},
		{path: "/web-ui/scripts/bootstrap.js", contentType: "text/javascript", content: "loadHistory();"},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s: unexpected status: %d body=%s", test.path, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, test.contentType) {
			t.Fatalf("GET %s: unexpected content type: %q", test.path, got)
		}
		if !strings.Contains(recorder.Body.String(), test.content) {
			t.Fatalf("GET %s: embedded content is missing", test.path)
		}
	}
}

func TestWebUIBootstrapDoesNotReferenceRemovedStoragePanel(t *testing.T) {
	index := readWebUIResource(t, "index.html")
	for _, unwanted := range []string{
		"scripts/storage.js",
		"TOOLS · TERMINAL · STORAGE",
	} {
		if strings.Contains(index, unwanted) {
			t.Fatalf("web UI index still references removed storage panel %q", unwanted)
		}
	}

	bootstrapScript := readWebUIResource(t, "scripts/bootstrap.js")
	for _, unwanted := range []string{
		"loadStorageStatus()",
		"setInterval(loadStorageStatus",
	} {
		if strings.Contains(bootstrapScript, unwanted) {
			t.Fatalf("web UI bootstrap still references removed storage function %q", unwanted)
		}
	}
	for _, required := range []string{
		"connectSSE();",
		"configureTerminal();",
		"toolSelectEl.addEventListener('change', syncSelectedTool);",
	} {
		if !strings.Contains(bootstrapScript, required) {
			t.Fatalf("web UI bootstrap missing required initialization %q", required)
		}
	}
}

func TestTerminalReverseProxyPreservesPublicHostAndRewritesFrameHeaders(t *testing.T) {
	var gotHost, gotForwardedHost, gotForwardedProto, gotForwardedPrefix string
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotForwardedHost = r.Header.Get("X-Forwarded-Host")
		gotForwardedProto = r.Header.Get("X-Forwarded-Proto")
		gotForwardedPrefix = r.Header.Get("X-Forwarded-Prefix")
		w.Header().Set("X-Frame-Options", "sameorigin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src ws://"+r.Host)
		w.Header().Set("Location", upstream.URL+"/")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream URL: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://device.example:8080/webtty/", nil)
	newTerminalReverseProxyForTarget(target).ServeHTTP(recorder, request)

	if gotHost != "device.example:8080" {
		t.Fatalf("upstream Host = %q, want device.example:8080", gotHost)
	}
	if gotForwardedHost != "device.example:8080" || gotForwardedProto != "http" || gotForwardedPrefix != "/webtty" {
		t.Fatalf("forwarded headers = host %q proto %q prefix %q", gotForwardedHost, gotForwardedProto, gotForwardedPrefix)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "" {
		t.Fatalf("X-Frame-Options = %q, want removed", got)
	}
	if got := recorder.Header().Get("Content-Security-Policy"); got != "default-src 'self'; connect-src ws://device.example:8080; frame-ancestors 'self'" {
		t.Fatalf("Content-Security-Policy = %q, want public WebSocket host plus same-origin framing", got)
	}
	if got := recorder.Header().Get("Location"); got != "/webtty/" {
		t.Fatalf("Location = %q, want /webtty/", got)
	}
}

func TestWebUIImagePasteControlsArePresent(t *testing.T) {
	attachmentsScript := readWebUIResource(t, "scripts/attachments.js")
	stateScript := readWebUIResource(t, "scripts/state.js")
	bootstrapScript := readWebUIResource(t, "scripts/bootstrap.js")
	for _, want := range []string{
		"async function handleComposerPaste(event)",
		"await addImageFiles(files, 'pasted');",
		"Only 4 images can be attached.",
	} {
		if !strings.Contains(attachmentsScript, want) {
			t.Fatalf("web UI missing %q", want)
		}
	}
	if !strings.Contains(stateScript, "const maxDraftImageAttachments = 4;") {
		t.Fatal("web UI missing image attachment limit")
	}
	if !strings.Contains(bootstrapScript, "inputEl.addEventListener('paste', handleComposerPaste);") {
		t.Fatal("web UI missing image paste listener")
	}
}

func TestWebUIUsesContextRequestIDsForToolMessageIdentity(t *testing.T) {
	chatScript := readWebUIResource(t, "scripts/chat.js")
	for _, want := range []string{
		"type === 'tool_call' || type === 'tool_result'",
		"'request', requestId, type",
		"msg.tool_name || '', msg.tool_input || '', content",
	} {
		if !strings.Contains(chatScript, want) {
			t.Fatalf("web UI tool message identity missing %q", want)
		}
	}
}

func readWebUIResource(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(webUIFiles, name)
	if err != nil {
		t.Fatalf("read embedded web UI resource %q: %v", name, err)
	}
	return string(data)
}

func TestServerHandleChatAsyncRejectsDuplicateRequestID(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		activeRuns:     make(map[string]context.CancelFunc),
		pendingResults: map[string]*chatPendingResult{"req-1": {}},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"hello","request_id":" req-1 "}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerHandleChatStreamRejectsDuplicateRequestID(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		activeRuns: make(map[string]context.CancelFunc),
	}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	server.registerActiveRun("req-1", cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewBufferString(`{"message":"hello","request_id":" req-1 "}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("X-Aiden-Stream", "ndjson")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerSpeakToolContentUsesCallerContext(t *testing.T) {
	provider := &blockingTTSProvider{started: make(chan struct{}), blockText: "Let me read the current volume."}
	server := &Server{logger: newTestLogger(),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient("/tmp/audio.sock"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.speakToolContent(ctx, "Let me read the current volume.")
		close(done)
	}()

	select {
	case <-provider.started:
	case <-time.After(500 * time.Millisecond):
		cancel()
		t.Fatal("tool content TTS was not started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("tool content TTS did not stop after caller context cancellation")
	}
}

type cancelAwareModel struct {
	started chan struct{}
	seen    chan error
	once    sync.Once
}

func (m *cancelAwareModel) GenerateContent(ctx context.Context, _ []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.once.Do(func() {
		if m.started != nil {
			close(m.started)
		}
	})
	<-ctx.Done()
	err := ctx.Err()
	m.seen <- err
	return nil, err
}

func (m *cancelAwareModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

type blockingTTSProvider struct {
	started   chan struct{}
	blockText string
	once      sync.Once
}

func (p *blockingTTSProvider) Name() string { return "blocking" }

func (p *blockingTTSProvider) Capabilities() ttsmodule.Capabilities {
	return ttsmodule.Capabilities{SupportedSampleRates: []int{16000}}
}

func (p *blockingTTSProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	return &blockingTTSSession{ctx: ctx, provider: p}, nil
}

func (p *blockingTTSProvider) Close() error { return nil }

type blockingTTSSession struct {
	ctx      context.Context
	provider *blockingTTSProvider
}

func (s *blockingTTSSession) WriteText(text string) error {
	if text == s.provider.blockText {
		s.provider.once.Do(func() { close(s.provider.started) })
	}
	return nil
}

func (s *blockingTTSSession) Flush() error { return nil }

func (s *blockingTTSSession) Close() error {
	<-s.ctx.Done()
	return s.ctx.Err()
}

func (s *blockingTTSSession) Err() error {
	return s.ctx.Err()
}

func TestServerHistoryEndpointIncludesToolMessages(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	manager, err := InitializeContextManager("system", agentpath.ContextManagerSessionFolder(runtime.config.ConfigDir), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleToolCall, Usage: &messages.Usage{InputTokens: 19, OutputTokens: 10, TotalTokens: 29}, ToolCalls: []messages.ToolCall{{ID: "call", Name: "screenshot", Arguments: "{}"}}},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "call", Name: "screenshot", Content: `{"width":100}`}}},
	} {
		if err := manager.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	runtime.contextManager = manager
	server := &Server{logger: newTestLogger(), runtime: runtime}

	req := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	rec := httptest.NewRecorder()
	server.handleHistory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var history []Message
	if err := json.NewDecoder(rec.Body).Decode(&history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	if history[1].Type != runEventToolCall || history[2].Type != "tool_result" {
		t.Fatalf("unexpected history payload: %#v", history)
	}
	if history[1].Usage == nil || history[1].Usage.InputTokens != 19 || history[1].Usage.OutputTokens != 10 || history[1].Usage.TotalTokens != 29 {
		t.Fatalf("history usage = %#v, want normalized token usage", history[1].Usage)
	}
}

func TestServerContextDumpEndpointReturnsPlannerMessages(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
	}
	sessionFolder := t.TempDir()
	sessionID := "test-session"
	if err := os.MkdirAll(sessionFolder, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionFolder, ".current_session"), []byte(sessionID), 0o644); err != nil {
		t.Fatalf("WriteFile(.current_session) error = %v", err)
	}
	manager, err := contextmanager.LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleUser,
		Content: "hello planner",
	}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	server.runtime.contextManager = manager

	req := httptest.NewRequest(http.MethodGet, "/api/context", nil)
	rec := httptest.NewRecorder()
	server.handleContext(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var dump ContextResponse
	if err := json.NewDecoder(rec.Body).Decode(&dump); err != nil {
		t.Fatalf("decode context dump: %v", err)
	}
	if dump.Backend.SessionID == "" {
		t.Fatal("expected backend session_id in context dump")
	}
	if len(dump.Backend.Messages) != 1 || dump.Backend.Messages[0].Content != "hello planner" {
		t.Fatalf("unexpected context dump payload: %#v", dump.Backend)
	}
}

func TestServerLoadsPersistedBackendContextBeforeFirstRun(t *testing.T) {
	configDir := t.TempDir()
	sessionFolder := agentpath.ContextManagerSessionFolder(configDir)
	manager, err := contextmanager.NewContextManager(sessionFolder, "persisted system prompt")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	for _, message := range []messages.Message{
		{Role: messages.MessageRoleUser, Content: "persisted question"},
		{Role: messages.MessageRoleAssistant, Content: "persisted answer"},
	} {
		if err := manager.AppendMessage(message); err != nil {
			t.Fatalf("AppendMessage() error = %v", err)
		}
	}

	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, Model: ModelConfig{Provider: "fake"}},
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	if runtime.contextManager != nil {
		t.Fatal("backend context should remain lazy before the first run")
	}
	server := &Server{logger: newTestLogger(), runtime: runtime}

	contextReq := httptest.NewRequest(http.MethodGet, "/api/context", nil)
	contextRec := httptest.NewRecorder()
	server.handleContext(contextRec, contextReq)
	var contextDump ContextResponse
	if err := json.NewDecoder(contextRec.Body).Decode(&contextDump); err != nil {
		t.Fatalf("decode context response: %v", err)
	}
	if contextDump.Backend.SessionID != manager.GetSessionID() || len(contextDump.Backend.Messages) != 3 {
		t.Fatalf("backend context = %#v, want persisted session before first run", contextDump.Backend)
	}

	historyReq := httptest.NewRequest(http.MethodGet, "/api/history", nil)
	historyRec := httptest.NewRecorder()
	server.handleHistory(historyRec, historyReq)
	var history []Message
	if err := json.NewDecoder(historyRec.Body).Decode(&history); err != nil {
		t.Fatalf("decode history response: %v", err)
	}
	if len(history) != 2 || history[0].Content != "persisted question" || history[1].Content != "persisted answer" {
		t.Fatalf("history = %#v, want persisted conversation before first run", history)
	}
	if runtime.contextManager != nil {
		t.Fatal("read-only context endpoints should not initialize the live context manager")
	}
}

func TestWebMessageFromContextMessagePreservesNoticeType(t *testing.T) {
	message, ok := webMessageFromContextMessage(messages.Message{
		Role:    messages.MessageRoleNotice,
		Content: "change strategy",
	}, "backend")
	if !ok {
		t.Fatal("webMessageFromContextMessage() rejected notice message")
	}
	if message.Type != "notice" || message.Role != "notice" {
		t.Fatalf("notice message = %#v, want notice type and role", message)
	}
}

func TestWebMessageFromContextMessagePreservesUsage(t *testing.T) {
	usage := &messages.Usage{InputTokens: 336, OutputTokens: 41, TotalTokens: 377}
	message, ok := webMessageFromContextMessage(messages.Message{
		Role:             messages.MessageRoleAssistant,
		Content:          "done",
		ReasoningContent: "plan",
		Usage:            usage,
	}, "backend")
	if !ok {
		t.Fatal("webMessageFromContextMessage() rejected assistant message")
	}
	if message.Usage == nil || *message.Usage != *usage {
		t.Fatalf("usage = %#v, want %#v", message.Usage, usage)
	}
	if message.Usage == usage {
		t.Fatal("web message usage should not share the context message pointer")
	}
	if message.ReasoningContent != "plan" {
		t.Fatalf("reasoning_content = %q, want plan", message.ReasoningContent)
	}
}

func TestServerContextAttachmentEndpointServesRegisteredAttachment(t *testing.T) {
	config := Config{Model: ModelConfig{Provider: "fake"}}
	server := &Server{logger: newTestLogger(), runtime: NewRuntimeWithDeps(
		withTestConfigDir(t, config),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)}
	folder := agentpath.UserContextManagerSessionFolder(server.runtime.config.ConfigDir)
	manager, err := contextmanager.NewContextManager(folder, "system")
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := manager.StoreAttachment("image/png", []byte("png-data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Attachments: []messages.Attachment{attachment}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/context/attachment?role=user&attachment="+url.QueryEscape(filepath.Base(attachment.FilePath)), nil)
	rec := httptest.NewRecorder()
	server.handleContextAttachment(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Body.String() != "png-data" {
		t.Fatalf("attachment response: status=%d type=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestServerContextAttachmentEndpointHidesInternalErrors(t *testing.T) {
	config := Config{Model: ModelConfig{Provider: "fake"}}
	server := &Server{logger: newTestLogger(), runtime: NewRuntimeWithDeps(
		withTestConfigDir(t, config),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)}
	rec := httptest.NewRecorder()
	server.handleContextAttachment(rec, httptest.NewRequest(http.MethodGet, "/api/context/attachment?role=user&attachment=missing.png", nil))
	if rec.Code != http.StatusNotFound || rec.Body.String() != "attachment not found\n" {
		t.Fatalf("attachment error response: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestContextPageUsesSafeAttachmentHandlers(t *testing.T) {
	page := readWebUIResource(t, "context.html")
	for _, want := range []string{"data-attachment=", "event.target.closest('.attachment')", "String(a.file_path || '')"} {
		if !strings.Contains(page, want) {
			t.Fatalf("context page missing %q", want)
		}
	}
	if strings.Contains(page, `onclick="openAttachment(`) {
		t.Fatal("context page still embeds attachment values in an inline JavaScript handler")
	}
}

func TestWebUIShowsMessageTokenUsage(t *testing.T) {
	messagesScript := readWebUIResource(t, "scripts/messages.js")
	styles := readWebUIResource(t, "styles.css")
	contextPage := readWebUIResource(t, "context.html")
	for _, want := range []string{"function renderMessageUsage(usage)", "usage.input_tokens", "usage.output_tokens", "usage.total_tokens"} {
		if !strings.Contains(messagesScript, want) {
			t.Fatalf("message renderer missing %q", want)
		}
	}
	for _, want := range []string{".message-footer", ".message-usage", ".message-usage-item"} {
		if !strings.Contains(styles, want) {
			t.Fatalf("web UI styles missing %q", want)
		}
	}
	if !strings.Contains(contextPage, "m.usage.input_tokens") || !strings.Contains(contextPage, "m.usage.total_tokens") {
		t.Fatal("context page does not render token usage")
	}
	if strings.Index(messagesScript, "footer.appendChild(timeDiv)") > strings.Index(messagesScript, "footer.appendChild(usageDiv)") {
		t.Fatal("message footer must render time before usage")
	}
	if !strings.Contains(styles, "margin-right: auto;") || !strings.Contains(styles, "margin-left: auto;") {
		t.Fatal("message footer must pin time left and usage right")
	}
}

func TestServerHandleClearRemovesRuntimeMemory(t *testing.T) {
	configDir := t.TempDir()
	memoryManager := NewMemoryManager(configDir)
	// ClearMemory rotates the user context, so verify a context exists to rotate.
	userFolder := agentpath.UserContextManagerSessionFolder(configDir)
	if _, err := contextmanager.NewContextManager(userFolder, "system"); err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	oldSessionID := contextmanager.CurrentSessionID(userFolder)
	if oldSessionID == "" {
		t.Fatal("expected a current session before clear")
	}

	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{ConfigDir: configDir, Model: ModelConfig{Provider: "fake"}}),
			&testModelResolver{model: &scriptedModel{}},
			memoryManager,
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/clear", nil)
	rec := httptest.NewRecorder()
	server.handleClear(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	newSessionID := contextmanager.CurrentSessionID(userFolder)
	if newSessionID == "" || newSessionID == oldSessionID {
		t.Fatalf("user context session not rotated: old=%q new=%q", oldSessionID, newSessionID)
	}
}

func TestServerHandleSetupReturnsSuccess(t *testing.T) {
	server := &Server{logger: newTestLogger()}

	req := httptest.NewRequest(http.MethodPost, "/api/setup", nil)
	rec := httptest.NewRecorder()
	server.handleSetup(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK   bool `json:"ok"`
		Data struct {
			Setup bool `json:"setup"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK {
		t.Fatalf("expected ok response: %#v", got)
	}
	if got.Data.Setup {
		t.Fatalf("expected setup=false for Go agent no-op response")
	}
}

func TestServerHandleHealthReportsBridgePlatform(t *testing.T) {
	server := &Server{logger: newTestLogger()}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	server.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK   bool `json:"ok"`
		Data struct {
			Status     string `json:"status"`
			BridgeType string `json:"bridge_type"`
			Platform   string `json:"platform"`
			DeviceType string `json:"device_type"`
			Concurrent int    `json:"concurrent"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK || got.Data.Status != "ok" || got.Data.BridgeType != "go-agent" {
		t.Fatalf("unexpected health envelope: %#v", got)
	}
	if got.Data.Platform != "ios" || got.Data.Concurrent != 1 {
		t.Fatalf("unexpected health data: %#v", got)
	}
	if got.Data.DeviceType != defaultDeviceType {
		t.Fatalf("unexpected device type: %#v", got)
	}
}

func TestServerHandleHealthIsRoutedOnTheBridgePath(t *testing.T) {
	server := &Server{logger: newTestLogger()}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /health to be routed, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerHandleHealthRejectsNonGet(t *testing.T) {
	server := &Server{logger: newTestLogger()}

	rec := httptest.NewRecorder()
	server.handleHealth(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
}

func TestServerHandleConcurrentReturnsSingleCapacity(t *testing.T) {
	server := &Server{logger: newTestLogger()}

	req := httptest.NewRequest(http.MethodGet, "/api/concurrent", nil)
	rec := httptest.NewRecorder()
	server.handleConcurrent(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK   bool `json:"ok"`
		Data struct {
			BridgeType string `json:"bridge_type"`
			Concurrent int    `json:"concurrent"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK || got.Data.BridgeType != "go-agent" || got.Data.Concurrent != 1 {
		t.Fatalf("unexpected concurrent response: %#v", got)
	}
}

func TestServerHandleSkillsReloadMarksDirty(t *testing.T) {
	storageDir := t.TempDir()
	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(storageDir),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
	}

	if server.runtime.skillsDirty {
		t.Fatalf("expected skillsDirty=false initially")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/skills/reload", nil)
	rec := httptest.NewRecorder()
	server.handleSkillsReload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if !server.runtime.skillsDirty {
		t.Fatalf("expected skillsDirty=true after reload request")
	}
}

func TestServerHandleSkillsReloadRejectsGet(t *testing.T) {
	server := &Server{logger: newTestLogger(),
		runtime: NewRuntimeWithDeps(
			withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(t.TempDir()),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/skills/reload", nil)
	rec := httptest.NewRecorder()
	server.handleSkillsReload(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestServerHandleChatWithAudioAttachmentUsesSTT(t *testing.T) {
	stt := &stubSTTClient{transcript: "Hello, please summarize this"}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Answer directly.",
		}),
		&testModelResolver{model: &scriptedModel{responses: roleDirectResponses("Completed")}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := &Server{logger: newTestLogger(),
		runtime:        runtime,
		sttClient:      stt,
		pendingResults: make(map[string]*chatPendingResult),
		activeRuns:     make(map[string]context.CancelFunc),
	}

	payload, err := json.Marshal(ChatRequest{
		Attachments: []MessageAttachment{{
			Kind:     AttachmentKindAudio,
			Name:     "recording.wav",
			MIMEType: "audio/wav",
			Data:     base64.StdEncoding.EncodeToString([]byte("RIFFtest")),
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}

	// STT should have been called during input processing
	if len(stt.inputs) != 1 {
		t.Fatalf("expected 1 STT invocation, got %d", len(stt.inputs))
	}

	// Poll for result
	var resp ChatResultResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.Status != "complete" {
		t.Fatalf("result never completed: status=%q", resp.Status)
	}
	if resp.Response != "Completed" {
		t.Fatalf("unexpected response: %q", resp.Response)
	}
	if len(resp.History) < 2 {
		t.Fatalf("expected at least 2 history entries for default-mode direct finish, got %d", len(resp.History))
	}
	user, ok := firstMessageOfType(resp.History, "user")
	if !ok || user.Content != "Hello, please summarize this" {
		t.Fatalf("expected transcript as user content, got %#v", resp.History)
	}
	assistant, ok := firstMessageOfType(resp.History, "assistant")
	if !ok || assistant.Content != "Completed" {
		t.Fatalf("unexpected assistant message: %#v", resp.History)
	}
}

func TestDecodeMessageAttachmentsLimitsImages(t *testing.T) {
	payloads := make([]MessageAttachment, maxChatImageAttachments+1)
	for i := range payloads {
		payloads[i] = MessageAttachment{
			Kind:     AttachmentKindImage,
			Name:     fmt.Sprintf("image-%d.png", i+1),
			MIMEType: "image/png",
			Data:     base64.StdEncoding.EncodeToString([]byte{byte(i + 1)}),
		}
	}

	if decoded, history, err := decodeMessageAttachments(payloads[:maxChatImageAttachments]); err != nil {
		t.Fatalf("decodeMessageAttachments(%d images) error = %v", maxChatImageAttachments, err)
	} else if len(decoded) != maxChatImageAttachments || len(history) != maxChatImageAttachments {
		t.Fatalf("decoded=%d history=%d, want %d", len(decoded), len(history), maxChatImageAttachments)
	}

	_, _, err := decodeMessageAttachments(payloads)
	if err == nil || !strings.Contains(err.Error(), "at most 4 image attachments") {
		t.Fatalf("decodeMessageAttachments(%d images) error = %v, want image limit", len(payloads), err)
	}
}

func TestDecodeMessageAttachmentsCountsImageMIMEWithFileKind(t *testing.T) {
	payloads := make([]MessageAttachment, maxChatImageAttachments+1)
	for i := range payloads {
		payloads[i] = MessageAttachment{
			Kind:     "file",
			Name:     fmt.Sprintf("image-%d.png", i+1),
			MIMEType: "image/png",
			Data:     base64.StdEncoding.EncodeToString([]byte{byte(i + 1)}),
		}
	}

	decoded, history, err := decodeMessageAttachments(payloads[:maxChatImageAttachments])
	if err != nil {
		t.Fatalf("decodeMessageAttachments(%d file-kind images) error = %v", maxChatImageAttachments, err)
	}
	if decoded[0].Kind != AttachmentKindImage || history[0].Kind != AttachmentKindImage {
		t.Fatalf("file-kind image was not normalized: decoded=%q history=%q", decoded[0].Kind, history[0].Kind)
	}

	_, _, err = decodeMessageAttachments(payloads)
	if err == nil || !strings.Contains(err.Error(), "at most 4 image attachments") {
		t.Fatalf("decodeMessageAttachments(%d file-kind images) error = %v, want image limit", len(payloads), err)
	}
}

func TestDecodeMessageAttachmentsPreservesImageDimensions(t *testing.T) {
	payloads := []MessageAttachment{{
		Kind:     AttachmentKindImage,
		MIMEType: "image/jpeg",
		Width:    447,
		Height:   972,
		Data:     base64.StdEncoding.EncodeToString([]byte("jpeg")),
	}}

	decoded, history, err := decodeMessageAttachments(payloads)
	if err != nil {
		t.Fatalf("decodeMessageAttachments() error = %v", err)
	}
	if decoded[0].Width != 447 || decoded[0].Height != 972 {
		t.Fatalf("decoded dimensions = %dx%d", decoded[0].Width, decoded[0].Height)
	}
	if history[0].Width != 447 || history[0].Height != 972 {
		t.Fatalf("history dimensions = %dx%d", history[0].Width, history[0].Height)
	}
}

func TestServerWebAudioInputModeNeverFallsBackToRemovedAudio(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		stt  STTClient
		want string
	}{
		{
			name: "explicit stt",
			cfg:  Config{InputMode: " stt "},
			want: TurnModalitySTT,
		},
		{
			name: "unconfigured mode with stt client",
			cfg:  Config{},
			stt:  &stubSTTClient{},
			want: TurnModalitySTT,
		},
		{
			name: "unconfigured mode without stt client",
			cfg:  Config{},
			want: TurnModalityText,
		},
		{
			name: "realtime browser channel stays text",
			cfg:  Config{InputMode: " realtime "},
			want: TurnModalityText,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{logger: newTestLogger(),
				runtime:   &Runtime{config: tt.cfg},
				sttClient: tt.stt,
			}
			got := server.webAudioInputMode()
			if got != tt.want {
				t.Fatalf("webAudioInputMode() = %q, want %q", got, tt.want)
			}
			if got == TurnModalityAudio {
				t.Fatalf("webAudioInputMode() returned removed audio mode")
			}
		})
	}
}

func TestServerHandleChatUsesAttachmentTranscriptWithoutRetranscribing(t *testing.T) {
	model := &scriptedModel{
		responses: roleDirectResponses("Completed"),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use transcript directly.",
			InputMode:   "stt",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	stt := &stubSTTClient{transcript: "should not be called"}
	server := &Server{logger: newTestLogger(),
		runtime:        runtime,
		sttClient:      stt,
		pendingResults: make(map[string]*chatPendingResult),
		activeRuns:     make(map[string]context.CancelFunc),
	}

	payload, err := json.Marshal(ChatRequest{
		Attachments: []MessageAttachment{{
			Kind:       AttachmentKindAudio,
			Name:       "recording.wav",
			MIMEType:   "audio/wav",
			Data:       base64.StdEncoding.EncodeToString([]byte("RIFFtest")),
			Transcript: "directly reused transcript",
		}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleChat(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var startResp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	requestID := startResp["request_id"]
	if requestID == "" {
		t.Fatalf("missing request_id in response: %#v", startResp)
	}

	// Should not have called STT since transcript was provided
	if len(stt.inputs) != 0 {
		t.Fatalf("expected attachment transcript to skip TranscribeWAV, got %d calls", len(stt.inputs))
	}

	// Poll for result
	var resp ChatResultResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resultReq := httptest.NewRequest(http.MethodGet, "/api/chat/result?request_id="+requestID, nil)
		resultRec := httptest.NewRecorder()
		server.handleChatResult(resultRec, resultReq)
		if resultRec.Code != http.StatusOK {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err := json.NewDecoder(resultRec.Body).Decode(&resp); err != nil {
			t.Fatalf("decode result response: %v", err)
		}
		if resp.Status == "complete" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.Status != "complete" {
		t.Fatalf("result never completed: status=%q", resp.Status)
	}
	user, ok := firstMessageOfType(resp.History, "user")
	if !ok || user.Content != "directly reused transcript" {
		t.Fatalf("expected transcript as user content, got %#v", resp.History)
	}
}

func TestServerSTTConfigTestLiveSessionUsesStreamingTranscript(t *testing.T) {
	sentChunk := false
	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "start_recording":
			return audioResponse{Status: "OK", SessionID: stringUint64(41)}, nil
		case "read_record_chunk":
			timeoutMs := int(req.TimeoutMs)
			if timeoutMs <= 0 {
				t.Fatalf("unexpected timeout_ms: %d", timeoutMs)
			}
			if !sentChunk {
				sentChunk = true
				return audioResponse{Status: "OK"}, []byte{1, 0, 2, 0, 3, 0, 4, 0}
			}
			return audioResponse{Status: "OK", EndOfStream: true}, nil
		case "stop_recording":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			Audio: AudioConfig{
				Socket:     socketPath,
				SampleRate: 16000,
				Channels:   1,
				BitWidth:   16,
			},
		}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	stt := &stubSTTClient{
		supportsStreaming: true,
		streamUploader:    &stubSTTStreamUploader{transcript: "streaming upload result"},
	}
	previousFactory := newSTTClientFromConfigForLiveTest
	newSTTClientFromConfigForLiveTest = func(cfg Config) (STTClient, error) {
		if cfg.STT.Provider != "tencent-asr" {
			t.Fatalf("provider = %q, want tencent-asr", cfg.STT.Provider)
		}
		if cfg.Audio.Socket != socketPath {
			t.Fatalf("audio socket = %q, want %q", cfg.Audio.Socket, socketPath)
		}
		return stt, nil
	}
	defer func() {
		newSTTClientFromConfigForLiveTest = previousFactory
	}()

	startBody := `{"stt_values":{"provider":"tencent-asr","app_id":"app-1","secret_id":"id","secret_key":"key","region":"ap-shanghai","engine_model_type":"16k_zh"},"audio_values":{"socket":"` + socketPath + `","sample_rate":16000,"channels":1,"bit_width":16}}`
	startReq := httptest.NewRequest(http.MethodPost, "/api/config-test/stt/start", strings.NewReader(startBody))
	startReq.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	server.handleSTTConfigTestStart(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("unexpected start status: %d body=%s", startRec.Code, startRec.Body.String())
	}

	stopReq := httptest.NewRequest(http.MethodPost, "/api/config-test/stt/stop", nil)
	stopRec := httptest.NewRecorder()
	server.handleSTTConfigTestStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("unexpected stop status: %d body=%s", stopRec.Code, stopRec.Body.String())
	}

	var resp struct {
		OK         bool   `json:"ok"`
		Transcript string `json:"transcript"`
		Results    []struct {
			Check  string `json:"check"`
			Passed bool   `json:"passed"`
			Detail string `json:"detail"`
		} `json:"results"`
	}
	if err := json.NewDecoder(stopRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected ok=true, got %#v", resp)
	}
	if resp.Transcript != "streaming upload result" {
		t.Fatalf("transcript = %q, want streaming upload result", resp.Transcript)
	}
	if len(stt.inputs) != 0 {
		t.Fatalf("expected streaming transcript to skip TranscribeWAV, got %d calls", len(stt.inputs))
	}
	if stt.streamUploaderUsed != 1 {
		t.Fatalf("stream uploader begin count = %d, want 1", stt.streamUploaderUsed)
	}
	if stt.streamUploader == nil || len(stt.streamUploader.writes) != 1 {
		t.Fatalf("stream uploader writes = %#v, want one PCM write", stt.streamUploader)
	}
	if len(resp.Results) != 1 || !strings.Contains(resp.Results[0].Detail, "streaming") {
		t.Fatalf("unexpected results: %#v", resp.Results)
	}
}

func TestServerSTTConfigTestLiveSessionFallsBackToOneShot(t *testing.T) {
	sentChunk := false
	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "start_recording":
			return audioResponse{Status: "OK", SessionID: stringUint64(42)}, nil
		case "read_record_chunk":
			if !sentChunk {
				sentChunk = true
				return audioResponse{Status: "OK"}, []byte{10, 0, 11, 0, 12, 0, 13, 0}
			}
			return audioResponse{Status: "OK", EndOfStream: true}, nil
		case "stop_recording":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})

	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			Audio: AudioConfig{
				Socket:     socketPath,
				SampleRate: 16000,
				Channels:   1,
				BitWidth:   16,
			},
		}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	stt := &stubSTTClient{transcript: "one-shot result"}
	previousFactory := newSTTClientFromConfigForLiveTest
	newSTTClientFromConfigForLiveTest = func(cfg Config) (STTClient, error) {
		if cfg.STT.Provider != "openai-whisper" {
			t.Fatalf("provider = %q, want openai-whisper", cfg.STT.Provider)
		}
		return stt, nil
	}
	defer func() {
		newSTTClientFromConfigForLiveTest = previousFactory
	}()

	startBody := `{"stt_values":{"provider":"openai-whisper","api_key":"sk-test","model":"whisper-1","base_url":"http://127.0.0.1:9"},"audio_values":{"socket":"` + socketPath + `","sample_rate":16000,"channels":1,"bit_width":16}}`
	startReq := httptest.NewRequest(http.MethodPost, "/api/config-test/stt/start", strings.NewReader(startBody))
	startReq.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	server.handleSTTConfigTestStart(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("unexpected start status: %d body=%s", startRec.Code, startRec.Body.String())
	}

	stopReq := httptest.NewRequest(http.MethodPost, "/api/config-test/stt/stop", nil)
	stopRec := httptest.NewRecorder()
	server.handleSTTConfigTestStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("unexpected stop status: %d body=%s", stopRec.Code, stopRec.Body.String())
	}

	var resp struct {
		OK         bool   `json:"ok"`
		Transcript string `json:"transcript"`
		Results    []struct {
			Check  string `json:"check"`
			Passed bool   `json:"passed"`
			Detail string `json:"detail"`
		} `json:"results"`
	}
	if err := json.NewDecoder(stopRec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode stop response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected ok=true, got %#v", resp)
	}
	if resp.Transcript != "one-shot result" {
		t.Fatalf("transcript = %q, want one-shot result", resp.Transcript)
	}
	if len(stt.inputs) != 1 {
		t.Fatalf("TranscribeWAV calls = %d, want 1", len(stt.inputs))
	}
	if stt.streamUploaderUsed != 0 {
		t.Fatalf("stream uploader begin count = %d, want 0", stt.streamUploaderUsed)
	}
	if len(resp.Results) != 1 || !strings.Contains(resp.Results[0].Detail, "one-shot") {
		t.Fatalf("unexpected results: %#v", resp.Results)
	}
}

func TestServerToolCatalogEndpoint(t *testing.T) {
	tool := &stubTool{
		name:        "shell",
		description: "Run shell commands.",
		output:      "ok",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"shell": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	rec := httptest.NewRecorder()

	server.handleToolCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ToolCatalogResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var shell ToolDescriptor
	for _, descriptor := range resp.Tools {
		if descriptor.Name == "shell" {
			shell = descriptor
			break
		}
	}
	if shell.Name == "" {
		t.Fatalf("expected shell in tool catalog: %#v", resp.Tools)
	}
	if shell.HTTP.Path != "/api/tools/shell" {
		t.Fatalf("unexpected tool path: %#v", shell.HTTP)
	}
	if shell.ExampleInput != `{"command":"pwd"}` {
		t.Fatalf("unexpected example input: %q", shell.ExampleInput)
	}
}

func TestServerToolInvokeEndpointAcceptsStructuredJSON(t *testing.T) {
	tool := &stubTool{
		name:        "shell",
		description: "Run shell commands.",
		output:      `{"status":"ok"}`,
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"shell": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	body := bytes.NewBufferString(`{"input":{"command":"pwd"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tools/shell", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleToolInvoke(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != `{"command":"pwd"}` {
		t.Fatalf("unexpected tool input: %#v", tool.inputs)
	}

	var resp ToolInvokeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Tool.Name != "shell" || resp.RawInput != `{"command":"pwd"}` || resp.Output != `{"status":"ok"}` || resp.IsError {
		t.Fatalf("unexpected response: %#v", resp)
	}
}

func TestServerToolInvokeContinuesAfterClientDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	tool := &stubTool{
		name:        "open_app",
		description: "Open an app.",
		callFn: func(ctx context.Context, _ string) (string, error) {
			close(started)
			select {
			case <-release:
				return "ok", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{"open_app": tool}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/tools/open_app", bytes.NewBufferString(`{"input":{"app":"WeChat"}}`)).WithContext(requestCtx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.handleToolInvoke(rec, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("tool invocation did not start")
	}
	cancelRequest()
	select {
	case <-done:
		t.Fatal("tool invocation stopped when the client request was canceled")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tool invocation did not finish after release")
	}

	var resp ToolInvokeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.IsError || resp.Output != "ok" {
		t.Fatalf("response = %#v, want successful detached execution", resp)
	}
}

func TestHTTPToolExecutionSurvivesClientDisconnectForHIDTools(t *testing.T) {
	for _, toolName := range []string{
		"keyboard_tap",
		"quick_action",
		"open_app",
		"enter_text",
		"mouse_move",
		"mouse_scroll",
		"touch_gesture",
	} {
		t.Run(toolName, func(t *testing.T) {
			if !httpToolExecutionSurvivesClientDisconnect(toolName) {
				t.Fatalf("httpToolExecutionSurvivesClientDisconnect(%q) = false, want true", toolName)
			}
		})
	}
	if httpToolExecutionSurvivesClientDisconnect("screenshot") {
		t.Fatal("screenshot should keep normal client-cancellation behavior")
	}
}

func TestServerToolInvokeUsesUnifiedExecutionAndNormalizesInput(t *testing.T) {
	tool := &stubTool{
		name:        "shell",
		description: "Run shell commands.",
		output:      "ok",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"shell": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	body := bytes.NewBufferString(`{"raw_input":"{\"command\":\"pwd\"}\nObservation:"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tools/shell", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleToolInvoke(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != `{"command":"pwd"}` {
		t.Fatalf("unexpected tool input: %#v", tool.inputs)
	}

	var resp ToolInvokeResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.RawInput != `{"command":"pwd"}` {
		t.Fatalf("raw input = %q, want normalized input", resp.RawInput)
	}
}

func TestServerDoesNotExposeActivateSkillOverHTTP(t *testing.T) {
	index := NewSkillIndex()
	index.skills["planner"] = &SkillDefinition{
		Name:         "planner",
		Description:  "Planning skill",
		Instructions: "Plan before acting.",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		index,
	)
	server := newServerForTest(runtime)

	catalogReq := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	catalogRec := httptest.NewRecorder()
	server.handleToolCatalog(catalogRec, catalogReq)
	if catalogRec.Code != http.StatusOK {
		t.Fatalf("unexpected catalog status: %d body=%s", catalogRec.Code, catalogRec.Body.String())
	}
	if bytes.Contains(catalogRec.Body.Bytes(), []byte("activate_skill")) {
		t.Fatalf("activate_skill should not be advertised over HTTP: %s", catalogRec.Body.String())
	}

	body := bytes.NewBufferString(`{"input":"planner"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/tools/activate_skill", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleToolInvoke(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected activate_skill HTTP invoke to be blocked, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestServerDoesNotExposeSkillManageOverHTTP(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"skill_manage": NewSkillManageTool(t.TempDir(), ""),
			"skill_list":   NewSkillListTool(t.TempDir()),
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	catalogReq := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	catalogRec := httptest.NewRecorder()
	server.handleToolCatalog(catalogRec, catalogReq)
	if catalogRec.Code != http.StatusOK {
		t.Fatalf("unexpected catalog status: %d body=%s", catalogRec.Code, catalogRec.Body.String())
	}
	if bytes.Contains(catalogRec.Body.Bytes(), []byte("skill_manage")) {
		t.Fatalf("skill_manage should not be advertised over HTTP: %s", catalogRec.Body.String())
	}
	if !bytes.Contains(catalogRec.Body.Bytes(), []byte("skill_list")) {
		t.Fatalf("expected skill_list to remain exposed: %s", catalogRec.Body.String())
	}

	invokeReq := httptest.NewRequest(http.MethodPost, "/api/tools/skill_manage", bytes.NewBufferString(`{"raw_input":"{\"action\":\"list\"}"}`))
	invokeRec := httptest.NewRecorder()
	server.handleToolInvoke(invokeRec, invokeReq)
	if invokeRec.Code != http.StatusNotFound {
		t.Fatalf("expected skill_manage HTTP invoke to be hidden, got %d body=%s", invokeRec.Code, invokeRec.Body.String())
	}
}

func TestServerToolSkillsEndpointReturnsGeneratedSkills(t *testing.T) {
	tool := &stubTool{
		name:        "shell",
		description: "Run shell commands.",
		output:      "ok",
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"shell": tool,
		}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)

	req := httptest.NewRequest(http.MethodGet, "https://device.example/api/tool-skills", nil)
	req.Header.Set("X-Forwarded-Host", "203.0.113.57:8080")
	req.Header.Set("X-Forwarded-Proto", "http")
	rec := httptest.NewRecorder()

	server.handleToolSkills(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ToolSkillsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Skills) != 1 {
		t.Fatalf("expected exactly 1 generated skill, got %d", len(resp.Skills))
	}

	skill := resp.Skills[0]
	if skill.Name != "aiden-http-tool-suite" {
		t.Fatalf("unexpected skill name: %#v", skill)
	}
	hasShell := false
	for _, name := range skill.ToolNames {
		if name == "shell" {
			hasShell = true
			break
		}
	}
	if !hasShell {
		t.Fatalf("unexpected tool list: %#v", skill.ToolNames)
	}
	if !bytes.Contains([]byte(skill.Markdown), []byte("/api/tools/{tool_name}")) {
		t.Fatalf("unexpected skill markdown: %q", skill.Markdown)
	}
	if !bytes.Contains([]byte(skill.Markdown), []byte("http://203.0.113.57:8080")) {
		t.Fatalf("expected forwarded base URL in markdown: %q", skill.Markdown)
	}
	if !bytes.Contains([]byte(skill.Markdown), []byte("NO_PROXY")) {
		t.Fatalf("expected proxy guidance in markdown: %q", skill.Markdown)
	}
}

func TestRequestBaseURLPrefersForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://local.invalid/api/tool-skills", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Forwarded-Proto", "https, http")
	req.Header.Set("X-Forwarded-Host", "device.example:8443, proxy.local")

	got := requestBaseURL(req)
	if got != "https://device.example:8443" {
		t.Fatalf("requestBaseURL = %q, want %q", got, "https://device.example:8443")
	}
}

func TestWebUIRedactsScreenshotBase64Payloads(t *testing.T) {
	toolMessagesScript := readWebUIResource(t, "scripts/tool_messages.js")
	required := []string{
		"redactToolPayloadForDisplay(JSON.parse(value))",
		"clone.data = '[base64 screenshot omitted: ' + byteLabel + ']'",
		"function isScreenshotPayload(value)",
	}
	for _, snippet := range required {
		if !strings.Contains(toolMessagesScript, snippet) {
			t.Fatalf("webUI missing screenshot redaction snippet %q", snippet)
		}
	}
	if strings.Contains(toolMessagesScript, "toolName !== 'screenshot'") {
		t.Fatalf("webUI still limits screenshot parsing to only the screenshot tool")
	}
}

func startFakeAudioServiceSocket(t *testing.T, handler func(audioRequest) (audioResponse, []byte)) string {
	t.Helper()

	socketDir, err := os.MkdirTemp("/tmp", "aiden-audio-test-*")
	if err != nil {
		t.Fatalf("create fake audio socket dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(socketDir)
	})

	socketPath := filepath.Join(socketDir, "audio.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on fake audio socket: %v", err)
	}
	t.Cleanup(func() {
		listener.Close()
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()

				msg, err := readUdsMessage(conn)
				if err != nil {
					t.Errorf("fake audio read request: %v", err)
					return
				}

				var req audioRequest
				if err := json.Unmarshal([]byte(msg.HeaderJSON), &req); err != nil {
					t.Errorf("fake audio decode request: %v", err)
					return
				}

				resp, payload := handler(req)
				respHeader, err := json.Marshal(resp)
				if err != nil {
					t.Errorf("fake audio encode response: %v", err)
					return
				}
				if err := writeUdsMessage(conn, udsMessage{HeaderJSON: string(respHeader), Payload: payload}); err != nil {
					t.Errorf("fake audio write response: %v", err)
					return
				}
			}()
		}
	}()

	return socketPath
}

func startFakeFrameServiceSocket(t *testing.T, handler func(map[string]any) (string, []byte)) string {
	t.Helper()

	socketDir, err := os.MkdirTemp("/tmp", "aiden-frame-test-*")
	if err != nil {
		t.Fatalf("create fake frame socket dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(socketDir)
	})

	socketPath := filepath.Join(socketDir, "frame.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on fake frame socket: %v", err)
	}
	t.Cleanup(func() {
		listener.Close()
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()

				header, payload, err := readUDSMessage(conn)
				if err != nil {
					t.Errorf("fake frame read request: %v", err)
					return
				}
				if len(payload) != 0 {
					t.Errorf("fake frame expected empty request payload, got %d bytes", len(payload))
					return
				}

				var req map[string]any
				if err := json.Unmarshal(header, &req); err != nil {
					t.Errorf("fake frame decode request: %v", err)
					return
				}

				respHeader, respPayload := handler(req)
				if err := writeUDSMessage(conn, []byte(respHeader), respPayload); err != nil {
					t.Errorf("fake frame write response: %v", err)
					return
				}
			}()
		}
	}()

	return socketPath
}

func newBenchmarkSeedMemoryServer(t *testing.T) (*Server, string) {
	return newBenchmarkSeedMemoryServerWithModel(t, &scriptedModel{})
}

func newBenchmarkSeedMemoryServerWithModel(t *testing.T, model *scriptedModel) (*Server, string) {
	t.Helper()
	configDir := ensureTestConfigDir(t, t.TempDir())
	streamingDisabled := false
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:                configDir,
			Model:                    ModelConfig{Provider: "fake"},
			Benchmark:                BenchmarkConfig{Token: "test-benchmark-token"},
			Instruction:              "Answer directly.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
			VoiceToolCallSpeech:      &streamingDisabled,
		},
		&testModelResolver{model: model},
		NewMemoryManager(filepath.Join(configDir, "memory")),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	server := newServerForTest(runtime)
	t.Cleanup(func() { server.bridge.queue.Stop() })
	return server, configDir
}

func TestHandleBenchmarkSeedMemorySucceeds(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServer(t)
	body := `{"id":"personamem_test_seed_1","type":"preference","title":"Test seed","content":"Seeded fixture content.","tags":["t1"],"priority":80}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "seeded" || resp["id"] != "personamem_test_seed_1" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	memPath := filepath.Join(configDir, "memory", "long_term", "memories", "personamem_test_seed_1.md")
	data, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read seeded memory file: %v", err)
	}
	if !strings.Contains(string(data), "Seeded fixture content.") {
		t.Fatalf("memory file missing content, got: %s", string(data))
	}
	if !strings.Contains(string(data), "id: personamem_test_seed_1") {
		t.Fatalf("memory file missing fixed id, got: %s", string(data))
	}
}

func TestHandleBenchmarkSeedSessionChunkSucceeds(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServer(t)
	body := `{
		"session_id":"benchmark_expense_session",
		"summary":"蓝海报销App 对话详情：核销码为 ZX-91-ALPHA，项目代号为 海鸥计划。",
		"messages":[
			{"role":"user","content":"刚才蓝海报销App里核销码和项目代号是什么？"},
			{"role":"assistant","content":"核销码是 ZX-91-ALPHA，项目代号是 海鸥计划。"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_session_chunk", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedSessionChunk(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "seeded" || resp["session_id"] != "benchmark_expense_session" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	tool := NewRecallSessionChunksTool(NewMultiSessionChunkStore(agentpath.ContextManagerSessionFolder(configDir)))
	for _, query := range []string{
		`{"limit":1}`,
		`{"entities":["蓝海报销App"],"limit":1}`,
		`{"tags":["蓝海报销"],"limit":1}`,
	} {
		out, err := tool.Call(context.Background(), query)
		if err != nil {
			t.Fatalf("recall seeded chunk: %v", err)
		}
		var recalled struct {
			Results []ChunkRecallResult `json:"results"`
		}
		if err := json.Unmarshal([]byte(out), &recalled); err != nil {
			t.Fatal(err)
		}
		if len(recalled.Results) != 1 || recalled.Results[0].SessionID != "benchmark_expense_session" ||
			recalled.Results[0].Summary != "蓝海报销App 对话详情：核销码为 ZX-91-ALPHA，项目代号为 海鸥计划。" {
			t.Fatalf("query %s: seeded conversation not recalled: %s", query, out)
		}
	}
}

func TestHandleBenchmarkSeedSessionChunkRejectsOversizedBody(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	oversizedSummary := strings.Repeat("x", benchmarkSeedSessionChunkMaxBodyBytes)
	body := fmt.Sprintf(`{"session_id":"benchmark_expense_session","summary":%q,"messages":[{"role":"user","content":"hello"}]}`, oversizedSummary)
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_session_chunk", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedSessionChunk(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("expected oversized-body error, got: %s", rec.Body.String())
	}
}

func TestHandleBenchmarkSeedNotificationWritesDurableFixture(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServerWithModel(t, &scriptedModel{responses: []*llms.ContentResponse{
		contentResponse(`{"results":[{"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}}]}`),
	}})
	body := `{"events":[{"source":"android","source_id":"delivery-1","source_event_id":"delivery-event-1","device_id":"benchmark-device","notification_uid":101,"event":"added","app_identifier":"com.delivery","title":"包裹更新","message":"包裹明天送达","received_at":"2026-08-21T00:01:00Z"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_notification", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedNotification(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Status     string   `json:"status"`
		ContextIDs []string `json:"context_ids"`
		EventCount int      `json:"event_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Status != "seeded" || response.EventCount != 1 || len(response.ContextIDs) != 1 || response.ContextIDs[0] != "1" {
		t.Fatalf("unexpected response: %#v", response)
	}
	rawPath := filepath.Join(configDir, "memory", "notifications", "events", "2026-08-21.jsonl")
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		t.Fatalf("read seeded notification fixture: %v", err)
	}
	if !strings.Contains(string(raw), `"message":"包裹明天送达"`) {
		t.Fatalf("raw fixture missing original message: %s", raw)
	}
	processReq := httptest.NewRequest(http.MethodPost, "/api/benchmark/notification-memory/process", bytes.NewBufferString(`{}`))
	processReq.Header.Set("Content-Type", "application/json")
	processReq.Header.Set("Authorization", "Bearer test-benchmark-token")
	processRec := httptest.NewRecorder()
	server.handleBenchmarkProcessNotificationMemory(processRec, processReq)
	if processRec.Code != http.StatusOK {
		t.Fatalf("unexpected process status: %d body=%s", processRec.Code, processRec.Body.String())
	}
	var processResponse struct {
		MemoryCursor string `json:"memory_cursor"`
	}
	if err := json.NewDecoder(processRec.Body).Decode(&processResponse); err != nil {
		t.Fatalf("decode process response: %v", err)
	}
	if processResponse.MemoryCursor != "1" {
		t.Fatalf("process cursor = %q, want 1", processResponse.MemoryCursor)
	}
}

func TestHandleBenchmarkProcessNotificationMemoryIsolatesInvalidProposal(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServerWithModel(t, &scriptedModel{responses: []*llms.ContentResponse{
		contentResponse(`{"results":[{"context_id":"1","proposal":{"actions":[{"action":"add","scope":"temporary","type":"not-a-memory-type","content":"Package arrives tomorrow"}]}}]}`),
	}})
	seedBody := `{"events":[{"source":"android","source_id":"delivery-invalid","source_event_id":"delivery-invalid-event","device_id":"benchmark-device","notification_uid":110,"event":"added","app_identifier":"com.delivery","title":"Package update","message":"Package arrives tomorrow","received_at":"2026-08-21T00:01:00Z"}]}`
	seedReq := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_notification", bytes.NewBufferString(seedBody))
	seedReq.Header.Set("Authorization", "Bearer test-benchmark-token")
	seedRec := httptest.NewRecorder()
	server.handleBenchmarkSeedNotification(seedRec, seedReq)
	if seedRec.Code != http.StatusOK {
		t.Fatalf("seed status=%d body=%s", seedRec.Code, seedRec.Body.String())
	}

	processReq := httptest.NewRequest(http.MethodPost, "/api/benchmark/notification-memory/process", bytes.NewBufferString(`{}`))
	processReq.Header.Set("Authorization", "Bearer test-benchmark-token")
	processRec := httptest.NewRecorder()
	server.handleBenchmarkProcessNotificationMemory(processRec, processReq)

	if processRec.Code != http.StatusOK {
		t.Fatalf("process status=%d body=%s, want 200", processRec.Code, processRec.Body.String())
	}
	var response benchmarkProcessNotificationMemoryResponse
	if err := json.NewDecoder(processRec.Body).Decode(&response); err != nil {
		t.Fatalf("decode process response: %v", err)
	}
	if response.MemoryCursor != "1" || len(response.MemoryIDs) != 0 {
		t.Fatalf("process response=%#v, want cursor 1 with no memory", response)
	}
}

func TestHandleBenchmarkSeedNotificationRetryReturnsOriginalContextID(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	body := `{"events":[{"source":"android","source_id":"delivery-retry","source_event_id":"delivery-retry-event","device_id":"benchmark-device","notification_uid":102,"event":"added","app_identifier":"com.delivery","title":"Package update","message":"Package arrives tomorrow","received_at":"2026-08-21T00:02:00Z"}]}`
	seed := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_notification", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-benchmark-token")
		rec := httptest.NewRecorder()
		server.handleBenchmarkSeedNotification(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
		}
		var response map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return response
	}

	first := seed()
	retried := seed()
	if !reflect.DeepEqual(first["context_ids"], retried["context_ids"]) || retried["event_count"] != float64(1) {
		t.Fatalf("first=%#v retried=%#v", first, retried)
	}
}

func TestHandleBenchmarkSeedMemoryCanSeedDeviceFixture(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServer(t)
	body := `{"store":"device","id":"legacy_device_fixture","type":"procedure","title":"Legacy procedure","content":"Preview, then Edit, then Save.","tags":["qa-notes","save"],"entities":["QA Notes","Preview","Edit","Save"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["status"] != "seeded" || resp["id"] != "legacy_device_fixture" || resp["store"] != "device" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	files, err := filepath.Glob(filepath.Join(configDir, "memory", "device", "procedures", "*.yaml"))
	if err != nil {
		t.Fatalf("glob device fixture: %v", err)
	}
	if len(files) != 1 || !strings.Contains(files[0], "legacy_device_fixture") {
		t.Fatalf("device fixture files = %#v", files)
	}
}

func TestHandleBenchmarkSeedMemoryCanSeedTemporaryFixture(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServer(t)
	body := `{"store":"temporary","id":"tmp_benchmark_delivery","type":"fact","title":"Delivery","content":"Package arrives today.","tags":["notification","com.delivery"],"source_refs":[{"type":"notification","id":"fixture-old","event_ids":["old-event"]}],"expires_at":"2099-01-01T00:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedMemory(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	memPath := filepath.Join(configDir, "memory", "temporary", "memories", "tmp_benchmark_delivery.md")
	parsed, err := readMemoryMarkdown(memPath)
	if err != nil {
		t.Fatalf("read temporary fixture: %v", err)
	}
	if parsed.Item.TimeScope != "temporary" || len(parsed.Item.SourceRefs) != 1 {
		t.Fatalf("temporary fixture = %#v", parsed.Item)
	}
}

func TestHandleBenchmarkSeedMemoryRequiresBenchmarkToken(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	body := `{"id":"personamem_test_seed_1","content":"Seeded fixture content."}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedMemory(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBenchmarkSeedEpisodePersistsCompletedEpisode(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	handler := server.Handler()
	body := `{
		"id":"ep_benchmark_title_save",
		"user_goal":"Change the QA Notes title and verify it persists.",
		"device_scope":{"device_id":"benchmark-device","app_name":"QA Notes","app_version":"7"},
		"outcome":{"success":true,"final_state":"The changed title is visible after reopening the note."},
		"events":[
			{"event_id":"save_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"tap\",\"target\":\"Save\"}"},
			{"event_id":"save_result","type":"tool_result","tool_name":"touch_gesture","observation":"The title reverted after direct Save."}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_episode", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected seed status: %d body=%s", rec.Code, rec.Body.String())
	}
	getReq := httptest.NewRequest(http.MethodGet, "/api/episodes/ep_benchmark_title_save", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("unexpected get status: %d body=%s", getRec.Code, getRec.Body.String())
	}
	var response EpisodeResponse
	if err := json.NewDecoder(getRec.Body).Decode(&response); err != nil {
		t.Fatalf("decode episode response: %v", err)
	}
	if response.Episode.ID != "ep_benchmark_title_save" || response.Episode.UserGoal != "Change the QA Notes title and verify it persists." {
		t.Fatalf("unexpected persisted Episode: %#v", response.Episode)
	}
	if response.Episode.EndedAt == "" || response.Episode.Status == "running" {
		t.Fatalf("seeded Episode was not completed: %#v", response.Episode)
	}
}

func TestBenchmarkSeedEpisodeRequiresBenchmarkToken(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_episode", bytes.NewBufferString(`{"id":"ep_unauthorized","user_goal":"test","outcome":{"success":true}}`))
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBenchmarkSeedEpisodeRejectsUnknownAndTrailingJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: `{"id":"ep_unknown","user_goal":"test","unknown":true}`},
		{name: "trailing object", body: `{"id":"ep_trailing","user_goal":"test"}{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newBenchmarkSeedMemoryServer(t)
			req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_episode", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer test-benchmark-token")
			rec := httptest.NewRecorder()
			server.handleBenchmarkSeedEpisode(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestBenchmarkProcessEpisodeMemoryConsolidatesSeededEpisode(t *testing.T) {
	configDir := ensureTestConfigDir(t, t.TempDir())
	streamingDisabled := false
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "episode_assessment":{"goal_result":"achieved","reason":"The final tool result confirms the changed title persisted after reopening.","evidence_refs":["verify_result"]},
  "candidates":[{
    "lesson_key":"qa_notes_v7_title_save_handshake",
    "type":"procedure",
    "action":"create",
	"retention":"durable",
    "unresolved_conflict":false,
    "situation":"When saving an edited title in QA Notes build 7",
    "guidance":"Switch to Preview, return to Edit, and then tap Save",
    "expected_effect":"The changed title remains visible after reopening the note",
    "scope":{"device_id":"benchmark-device","app_name":"QA Notes","app_version":"7","goal_pattern":"persist edited note title"},
    "tags":["qa-notes","title","save"],
    "evidence_refs":["preview_call","preview_result","edit_call","edit_result","save_call","save_result","verify_call","verify_result"]
  }]
}`}}
	runtime := NewRuntimeWithDeps(
		Config{
			ConfigDir:                configDir,
			Model:                    ModelConfig{Provider: "fake"},
			Benchmark:                BenchmarkConfig{Token: "test-benchmark-token"},
			Instruction:              "Answer directly.",
			VoiceStreamingTTSEnabled: &streamingDisabled,
			VoiceToolCallSpeech:      &streamingDisabled,
		},
		model,
		NewMemoryManager(filepath.Join(configDir, "memory")),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	t.Cleanup(func() { runtime.Close() })
	server := newServerForTest(runtime)
	t.Cleanup(func() { server.bridge.queue.Stop() })
	handler := server.Handler()
	episodeBody := `{
		"id":"ep_benchmark_title_save",
		"user_goal":"Change the QA Notes title and verify it persists.",
		"device_scope":{"device_id":"benchmark-device","app_name":"QA Notes","app_version":"7"},
		"outcome":{"success":true,"final_state":"The changed title is visible after reopening the note."},
		"events":[
			{"event_id":"direct_save_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"tap\",\"target\":\"Save\"}"},
			{"event_id":"direct_save_result","type":"tool_result","tool_name":"touch_gesture","observation":"The title reverted after direct Save."},
			{"event_id":"preview_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"tap\",\"target\":\"Preview\"}"},
			{"event_id":"preview_result","type":"tool_result","tool_name":"touch_gesture","observation":"Preview mode is visible."},
			{"event_id":"edit_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"tap\",\"target\":\"Edit\"}"},
			{"event_id":"edit_result","type":"tool_result","tool_name":"touch_gesture","observation":"Edit mode is visible again."},
			{"event_id":"save_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"tap\",\"target\":\"Save\"}"},
			{"event_id":"save_result","type":"tool_result","tool_name":"touch_gesture","observation":"Save completed after the Preview and Edit transition."},
			{"event_id":"verify_call","type":"tool_call","tool_name":"touch_gesture","tool_input":"{\"type\":\"reopen_note\"}"},
			{"event_id":"verify_result","type":"tool_result","tool_name":"touch_gesture","observation":"The changed title remains visible after reopening."}
		]
	}`
	seedReq := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_episode", bytes.NewBufferString(episodeBody))
	seedReq.Header.Set("Authorization", "Bearer test-benchmark-token")
	seedRec := httptest.NewRecorder()
	handler.ServeHTTP(seedRec, seedReq)
	if seedRec.Code != http.StatusOK {
		t.Fatalf("seed status: %d body=%s", seedRec.Code, seedRec.Body.String())
	}

	processReq := httptest.NewRequest(http.MethodPost, "/api/benchmark/episode-memory/process", bytes.NewBufferString(`{"episode_id":"ep_benchmark_title_save"}`))
	processReq.Header.Set("Authorization", "Bearer test-benchmark-token")
	processRec := httptest.NewRecorder()
	handler.ServeHTTP(processRec, processReq)

	if processRec.Code != http.StatusOK {
		t.Fatalf("process status: %d body=%s", processRec.Code, processRec.Body.String())
	}
	var response struct {
		Status     string                  `json:"status"`
		Assessment episodeMemoryAssessment `json:"assessment"`
		MemoryIDs  []string                `json:"memory_ids"`
	}
	if err := json.NewDecoder(processRec.Body).Decode(&response); err != nil {
		t.Fatalf("decode process response: %v", err)
	}
	if response.Status != string(episodeMemoryStatusDone) {
		t.Fatalf("status = %q, want done", response.Status)
	}
	if response.Assessment.GoalResult != episodeGoalAchieved {
		t.Fatalf("assessment = %#v, want achieved", response.Assessment)
	}
	if len(response.MemoryIDs) != 1 {
		t.Fatalf("memory_ids = %#v, want one extracted memory", response.MemoryIDs)
	}
}

func TestHandleBenchmarkPhoneBridgeStateAppliesIOSPiPPolicy(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	body := `{
		"connected": false,
		"platform": "ios",
		"phone_id": "benchmark-ios-pip",
		"app_state": "background",
		"return_entry": "dynamic_island",
		"return_entry_available": true,
		"pip_bridge_enabled": true,
		"fgs_bridge_enabled": false
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/benchmark/phone_bridge_state", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkPhoneBridgeState(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	status := server.bridge.getStatus()
	if status.Platform != "ios" || status.AppState != "background" {
		t.Fatalf("unexpected bridge status: %+v", status)
	}
	if status.AppStateUpdatedAt == nil || status.PipBridgeEnabled == nil || !*status.PipBridgeEnabled {
		t.Fatalf("PiP benchmark status was not made fresh: %+v", status)
	}
}

func TestHandleBenchmarkPhoneBridgeStateRequiresBenchmarkToken(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/benchmark/phone_bridge_state",
		bytes.NewBufferString(`{"platform":"ios","app_state":"background"}`),
	)
	rec := httptest.NewRecorder()

	server.handleBenchmarkPhoneBridgeState(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBenchmarkPhoneBridgeStateRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "platform", body: `{"platform":"windows","app_state":"active"}`},
		{name: "app state", body: `{"platform":"ios","app_state":"suspended"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newBenchmarkSeedMemoryServer(t)
			req := httptest.NewRequest(
				http.MethodPost,
				"/api/benchmark/phone_bridge_state",
				bytes.NewBufferString(tc.body),
			)
			req.Header.Set("Authorization", "Bearer test-benchmark-token")
			rec := httptest.NewRecorder()

			server.handleBenchmarkPhoneBridgeState(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleBenchmarkSeedMemoryOverwritesSameID(t *testing.T) {
	server, configDir := newBenchmarkSeedMemoryServer(t)
	mkReq := func(content string) *http.Request {
		body := fmt.Sprintf(`{"id":"personamem_overwrite_1","type":"fact","content":%q}`, content)
		req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-benchmark-token")
		return req
	}

	rec := httptest.NewRecorder()
	server.handleBenchmarkSeedMemory(rec, mkReq("first content"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first seed status: %d body=%s", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	server.handleBenchmarkSeedMemory(rec2, mkReq("second content"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second seed status: %d body=%s", rec2.Code, rec2.Body.String())
	}

	memPath := filepath.Join(configDir, "memory", "long_term", "memories", "personamem_overwrite_1.md")
	data, err := os.ReadFile(memPath)
	if err != nil {
		t.Fatalf("read seeded memory file: %v", err)
	}
	if !strings.Contains(string(data), "second content") {
		t.Fatalf("expected overwrite to second content, got: %s", string(data))
	}
	if strings.Contains(string(data), "first content") {
		t.Fatalf("first content should be overwritten, still present: %s", string(data))
	}
}

func TestHandleBenchmarkSeedMemoryRejectsMissingFields(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing id", `{"content":"x"}`},
		{"missing content", `{"id":"x"}`},
		{"blank id", `{"id":" ","content":"x"}`},
		{"blank content", `{"id":"x","content":"  "}`},
		{"malformed json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/benchmark/seed_memory", bytes.NewBufferString(tc.body))
			req.Header.Set("Authorization", "Bearer test-benchmark-token")
			rec := httptest.NewRecorder()
			server.handleBenchmarkSeedMemory(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleBenchmarkSeedMemoryRejectsUnknownFields(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/benchmark/seed_memory",
		bytes.NewBufferString(`{"id":"seed","content":"content","evidnce":["typo"]}`),
	)
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()

	server.handleBenchmarkSeedMemory(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown field") {
		t.Fatalf("expected strict 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleBenchmarkSeedMemoryRejectsNonPost(t *testing.T) {
	server, _ := newBenchmarkSeedMemoryServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/benchmark/seed_memory", nil)
	req.Header.Set("Authorization", "Bearer test-benchmark-token")
	rec := httptest.NewRecorder()
	server.handleBenchmarkSeedMemory(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d body=%s", rec.Code, rec.Body.String())
	}
}

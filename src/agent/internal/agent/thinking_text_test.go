package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
)

func TestNormalizeTaggedThinkingTextSupportsOrphanClosingTag(t *testing.T) {
	visible, reasoning, found := normalizeTaggedThinkingText("internal plan\n</think>\n<tts>1 加 1 等于 2。</tts>\n1 加 1 等于 2。")
	if !found {
		t.Fatal("normalizeTaggedThinkingText found = false, want true")
	}
	if reasoning != "internal plan" {
		t.Fatalf("reasoning = %q, want %q", reasoning, "internal plan")
	}
	if visible != "<tts>1 加 1 等于 2。</tts>\n1 加 1 等于 2。" {
		t.Fatalf("visible = %q", visible)
	}
}

func TestNormalizeTaggedThinkingTextPreservesUnclosedThinking(t *testing.T) {
	visible, reasoning, found := normalizeTaggedThinkingText("before <think>unfinished")
	if !found {
		t.Fatal("normalizeTaggedThinkingText found = false, want true")
	}
	if visible != "before <think>unfinished" {
		t.Fatalf("visible = %q, want original unclosed text", visible)
	}
	if reasoning != "" {
		t.Fatalf("reasoning = %q, want empty", reasoning)
	}
}

func TestTaggedThinkingStreamPreservesUnclosedThinking(t *testing.T) {
	var visible, reasoning strings.Builder
	stream := newTaggedThinkingStream(func(chunk []byte) error {
		visible.Write(chunk)
		return nil
	}, func(chunk []byte) error {
		reasoning.Write(chunk)
		return nil
	})
	if err := stream.Write([]byte("before <think>unfinished")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if visible.Len() != 7 || reasoning.Len() != 0 {
		t.Fatalf("before Finish: visible=%q reasoning=%q", visible.String(), reasoning.String())
	}
	if err := stream.Finish(""); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := visible.String(); got != "before <think>unfinished" {
		t.Fatalf("visible = %q, want original unclosed text", got)
	}
	if reasoning.Len() != 0 {
		t.Fatalf("reasoning = %q, want empty", reasoning.String())
	}
}

func TestTaggedThinkingStreamRoutesTruncatedThinkingToReasoning(t *testing.T) {
	var visible, reasoning strings.Builder
	stream := newTaggedThinkingStream(func(chunk []byte) error {
		visible.Write(chunk)
		return nil
	}, func(chunk []byte) error {
		reasoning.Write(chunk)
		return nil
	})
	if err := stream.Write([]byte("before <think>unfinished")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := stream.Finish("length"); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := visible.String(); got != "before " {
		t.Fatalf("visible = %q, want only text before the unclosed block", got)
	}
	if got := reasoning.String(); got != "unfinished" {
		t.Fatalf("reasoning = %q, want the unclosed block", got)
	}
}

func TestTaggedThinkingStreamForwardsVisibleTextWithConfiguredEffort(t *testing.T) {
	var visible strings.Builder
	stream := newTaggedThinkingStream(func(chunk []byte) error {
		visible.Write(chunk)
		return nil
	}, func([]byte) error { return nil })
	for _, chunk := range []string{"Hello ", "there", "!"} {
		if err := stream.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write(%q) failed: %v", chunk, err)
		}
	}
	if got := visible.String(); got != "Hello there" {
		t.Fatalf("visible before Finish = %q, want incremental visible text", got)
	}
	if err := stream.Finish(""); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := visible.String(); got != "Hello there!" {
		t.Fatalf("visible after Finish = %q", got)
	}
}

func TestTaggedThinkingStreamDoesNotFlushOpenThinkingAsVisible(t *testing.T) {
	var visible, reasoning strings.Builder
	stream := newTaggedThinkingStream(func(chunk []byte) error {
		visible.Write(chunk)
		return nil
	}, func(chunk []byte) error {
		reasoning.Write(chunk)
		return nil
	})
	if err := stream.Write([]byte("<think>secret plan</thi")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := stream.FlushVisible(); err != nil {
		t.Fatalf("FlushVisible failed: %v", err)
	}
	if got := visible.String(); got != "" {
		t.Fatalf("visible after FlushVisible = %q, want empty", got)
	}
	if got := reasoning.String(); got != "" {
		t.Fatalf("reasoning after FlushVisible = %q, want pending", got)
	}
	if err := stream.Write([]byte("nk>visible")); err != nil {
		t.Fatalf("Write close failed: %v", err)
	}
	if err := stream.Finish(""); err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	if got := reasoning.String(); got != "secret plan" {
		t.Fatalf("reasoning = %q, want secret plan", got)
	}
	if got := visible.String(); got != "visible" {
		t.Fatalf("visible = %q, want visible", got)
	}
}

func TestFunctionAgentParseOutputRemovesTaggedThinkingFromFinalAnswer(t *testing.T) {
	agent := &FunctionAgent{OutputKey: agentLoopOutputKey}
	_, finish, err := agent.ParseOutput(&llms.ContentResponse{Choices: []*llms.ContentChoice{{
		Content: "internal plan\n</think>\nfinal answer",
	}}})
	if err != nil {
		t.Fatalf("ParseOutput failed: %v", err)
	}
	if finish == nil {
		t.Fatal("finish = nil, want final answer")
	}
	if got := finish.ReturnValues[agentLoopOutputKey]; got != "final answer" {
		t.Fatalf("final answer = %#v", got)
	}
}

func TestOpenAICompatibleReasoningContentInContentStreamsVisibleText(t *testing.T) {
	streamEvents := []string{
		`data: {"id":"1","choices":[{"delta":{"content":"internal plan"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":"\n</think>\n<tts>answer"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":".</tts>"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":" visible"}}]}`,
		`data: {"id":"1","choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range streamEvents {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}))
	defer server.Close()
	model := newOpenAICompatibleModel(server.URL, "test-model", "token", server.Client(), withOpenAICompatibleReasoningEffort("medium"))
	var chunks []string
	var reasoningChunks []string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "test")}, llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
		chunks = append(chunks, string(chunk))
		return nil
	}), llms.WithStreamingReasoningFunc(func(_ context.Context, chunk, _ []byte) error {
		reasoningChunks = append(reasoningChunks, string(chunk))
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent failed: %v", err)
	}
	if got := resp.Choices[0].Content; got != "<tts>answer.</tts> visible" {
		t.Fatalf("content = %q", got)
	}
	if got := resp.Choices[0].ReasoningContent; got != "internal plan" {
		t.Fatalf("reasoning content = %q", got)
	}
	if strings.Join(chunks, "") != "<tts>answer.</tts> visible" {
		t.Fatalf("streamed content = %q", strings.Join(chunks, ""))
	}
	if len(chunks) < 2 {
		t.Fatalf("streamed chunks = %#v, want incremental visible output", chunks)
	}
	if got := strings.TrimSpace(strings.Join(reasoningChunks, "")); got != "internal plan" {
		t.Fatalf("streamed reasoning = %q, want internal plan", got)
	}
}

func TestOpenAICompatibleDedicatedReasoningStillStreamsContent(t *testing.T) {
	streamEvents := []string{
		`data: {"id":"1","choices":[{"delta":{"reasoning_content":"private"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":"first"}}]}`,
		`data: {"id":"1","choices":[{"delta":{"content":" second"}}]}`,
		`data: {"id":"1","choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range streamEvents {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}))
	defer server.Close()
	model := newOpenAICompatibleModel(server.URL, "test-model", "token", server.Client(), withOpenAICompatibleReasoningEffort("medium"))
	var chunks []string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "test")}, llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
		chunks = append(chunks, string(chunk))
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent failed: %v", err)
	}
	if got := strings.Join(chunks, ""); got != "first second" {
		t.Fatalf("streamed content = %q", got)
	}
	if len(chunks) != 2 {
		t.Fatalf("streamed chunks = %#v, want 2", chunks)
	}
	if got := resp.Choices[0].ReasoningContent; got != "private" {
		t.Fatalf("reasoning content = %q", got)
	}
}

func TestOpenAICompatibleReasoningCallbackWithoutContentStream(t *testing.T) {
	streamEvents := []string{
		`data: {"id":"1","choices":[{"delta":{"content":"<think>private</think>visible"}}]}`,
		`data: {"id":"1","choices":[{"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range streamEvents {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}))
	defer server.Close()
	model := newOpenAICompatibleModel(server.URL, "test-model", "token", server.Client(), withOpenAICompatibleReasoningEffort("medium"))
	var reasoningChunks []string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "test")}, llms.WithStreamingReasoningFunc(func(_ context.Context, chunk, _ []byte) error {
		reasoningChunks = append(reasoningChunks, string(chunk))
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent failed: %v", err)
	}
	if got := resp.Choices[0].Content; got != "visible" {
		t.Fatalf("content = %q, want visible", got)
	}
	if got := strings.Join(reasoningChunks, ""); got != "private" {
		t.Fatalf("streamed reasoning = %q, want private", got)
	}
}

package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
)

type replayedMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

// replayAnthropicMessages sends contextMessages through the native Claude path
// and returns the request messages the provider actually received.
func replayAnthropicMessages(t *testing.T, contextMessages []messages.Message) []replayedMessage {
	t.Helper()
	var captured struct {
		Messages []replayedMessage `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		_, _ = w.Write([]byte(anthropicThinkingProbeOK))
	}))
	defer server.Close()

	m := newAnthropicModel(server.URL, "claude-sonnet-4-6", "tok", server.Client(),
		withAnthropicReasoningEffort("low"))
	contextModel, ok := m.(interface {
		GenerateContentFromMessageList(context.Context, []messages.Message, ...llms.CallOption) (*llms.ContentResponse, error)
	})
	if !ok {
		t.Fatalf("model = %T does not preserve message-list metadata", m)
	}
	if _, err := contextModel.GenerateContentFromMessageList(context.Background(), contextMessages); err != nil {
		t.Fatalf("GenerateContentFromMessageList() error = %v", err)
	}
	return captured.Messages
}

func signedThinking(text, signature string) []json.RawMessage {
	return []json.RawMessage{json.RawMessage(
		`{"type":"thinking","thinking":"` + text + `","signature":"` + signature + `"}`)}
}

// assistantSignatures returns the signature of every thinking block on each
// assistant message, keyed by the assistant's position in the request.
func assistantSignatures(t *testing.T, replayed []replayedMessage) [][]string {
	t.Helper()
	var result [][]string
	for _, message := range replayed {
		if message.Role != "assistant" {
			continue
		}
		signatures := []string{}
		for _, block := range message.Content {
			if block["type"] == "thinking" {
				signature, _ := block["signature"].(string)
				signatures = append(signatures, signature)
			}
		}
		result = append(result, signatures)
	}
	return result
}

func TestAnthropicReplayKeepsSignatureOnItsOwnTurnAcrossDroppedTurns(t *testing.T) {
	// The empty assistant turn has no parts, so the converter drops it. Pairing
	// by assistant ordinal would shift sig-B onto the wrong turn, which the API
	// rejects. RuntimeContext appends assistant messages that can be empty, so
	// this is a reachable state, not a synthetic one.
	replayed := replayAnthropicMessages(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: ""},
		{Role: messages.MessageRoleUser, Content: "again"},
		{Role: messages.MessageRoleAssistant, Content: "real answer",
			AnthropicThinkingBlocks: signedThinking("real", "sig-B")},
		{Role: messages.MessageRoleUser, Content: "more"},
	})

	signatures := assistantSignatures(t, replayed)
	if len(signatures) != 1 {
		t.Fatalf("assistant messages = %d, want 1 after the empty turn is dropped: %+v", len(signatures), replayed)
	}
	if len(signatures[0]) != 1 || signatures[0][0] != "sig-B" {
		t.Fatalf("signatures = %v, want sig-B on the surviving turn", signatures[0])
	}
}

func TestAnthropicReplayDropsSignatureOfDroppedTurn(t *testing.T) {
	// The signed turn itself is dropped for having no content. Its signature
	// must not migrate to a later assistant turn it did not belong to.
	replayed := replayAnthropicMessages(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: "",
			AnthropicThinkingBlocks: signedThinking("orphan", "sig-A")},
		{Role: messages.MessageRoleUser, Content: "again"},
		{Role: messages.MessageRoleAssistant, Content: "second answer"},
		{Role: messages.MessageRoleUser, Content: "more"},
	})

	for _, signatures := range assistantSignatures(t, replayed) {
		for _, signature := range signatures {
			if signature == "sig-A" {
				t.Fatalf("sig-A was replayed on a turn that did not produce it: %+v", replayed)
			}
		}
	}
}

func TestAnthropicReplayKeepsThinkingFirstWhenAssistantTurnsMerge(t *testing.T) {
	// Two consecutive assistant turns merge into one request message. Anthropic
	// requires every thinking block to precede the visible content.
	replayed := replayAnthropicMessages(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: "first",
			AnthropicThinkingBlocks: signedThinking("t1", "sig-1")},
		{Role: messages.MessageRoleAssistant, Content: "second",
			AnthropicThinkingBlocks: signedThinking("t2", "sig-2")},
		{Role: messages.MessageRoleUser, Content: "more"},
	})

	var assistant *replayedMessage
	for i := range replayed {
		if replayed[i].Role == "assistant" {
			if assistant != nil {
				t.Fatalf("expected the assistant turns to merge into one message: %+v", replayed)
			}
			assistant = &replayed[i]
		}
	}
	if assistant == nil {
		t.Fatalf("no assistant message in %+v", replayed)
	}

	seenNonThinking := false
	var order []string
	for _, block := range assistant.Content {
		kind, _ := block["type"].(string)
		order = append(order, kind)
		if kind == "thinking" {
			if seenNonThinking {
				t.Fatalf("thinking block follows non-thinking content, order = %v", order)
			}
			continue
		}
		seenNonThinking = true
	}
	if len(order) != 4 {
		t.Fatalf("content blocks = %v, want both thinking blocks and both texts", order)
	}
	// Relative order of the signatures must survive the hoist.
	signatures := assistantSignatures(t, replayed)[0]
	if len(signatures) != 2 || signatures[0] != "sig-1" || signatures[1] != "sig-2" {
		t.Fatalf("signatures = %v, want [sig-1 sig-2] in original order", signatures)
	}
}

func TestAnthropicReplaySkipsUnsignedThinkingBlocks(t *testing.T) {
	// A thinking block without a signature cannot be replayed; sending it back
	// is an API error, so it must be filtered rather than forwarded.
	replayed := replayAnthropicMessages(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: "answer",
			AnthropicThinkingBlocks: []json.RawMessage{
				json.RawMessage(`{"type":"thinking","thinking":"unsigned"}`),
				json.RawMessage(`{"type":"thinking","thinking":"signed","signature":"sig-ok"}`),
			}},
		{Role: messages.MessageRoleUser, Content: "more"},
	})

	signatures := assistantSignatures(t, replayed)
	if len(signatures) != 1 || len(signatures[0]) != 1 || signatures[0][0] != "sig-ok" {
		t.Fatalf("signatures = %v, want only the signed block replayed", signatures)
	}
}

func TestAnthropicReplayPreservesToolCallTurnSignature(t *testing.T) {
	// The tool-call turn is the case that motivated persisting signatures at
	// all: Claude requires the signature back when a tool result follows.
	var captured struct {
		Messages []replayedMessage `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		_, _ = w.Write([]byte(anthropicThinkingProbeOK))
	}))
	defer server.Close()

	m := newAnthropicModel(server.URL, "claude-sonnet-4-6", "tok", server.Client(),
		withAnthropicReasoningEffort("low"))
	contextModel := m.(interface {
		GenerateContentFromMessageList(context.Context, []messages.Message, ...llms.CallOption) (*llms.ContentResponse, error)
	})
	_, err := contextModel.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleToolCall,
			ToolCalls:               []messages.ToolCall{{ID: "call", Name: "echo", Arguments: "{}"}},
			AnthropicThinkingBlocks: signedThinking("planning", "sig-tool")},
		{Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{ToolCallID: "call", Name: "echo", Content: "ok"}}},
		{Role: messages.MessageRoleUser, Content: "continue"},
	}, llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "echo"}}}))
	if err != nil {
		t.Fatalf("GenerateContentFromMessageList() error = %v", err)
	}

	signatures := assistantSignatures(t, captured.Messages)
	if len(signatures) != 1 || len(signatures[0]) != 1 || signatures[0][0] != "sig-tool" {
		t.Fatalf("signatures = %v, want sig-tool replayed on the tool-call turn", signatures)
	}
	for _, message := range captured.Messages {
		if message.Role != "assistant" {
			continue
		}
		if kind, _ := message.Content[0]["type"].(string); kind != "thinking" {
			t.Fatalf("first content block = %q, want thinking to lead the turn", kind)
		}
	}
}

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelpkg "aico-agent/internal/agent/model"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
)

// memoryMergeRealModel adapts a concrete llms.Model to the model.Model the merge
// engine takes, so a test can drive the engine through the real HTTP decoder
// rather than a hand-built ContentResponse.
type memoryMergeRealModel struct {
	llms.Model
}

func (memoryMergeRealModel) CallOptions() []chains.ChainCallOption { return nil }
func (memoryMergeRealModel) Spec() modelpkg.ModelSpec              { return modelpkg.ModelSpec{} }

// memoryMergeProbeRequest is a minimal request whose only interesting property
// is the model response, so tests can focus on how Extract classifies it.
func memoryMergeProbeRequest(maxTokens int) MemoryMergeRequest {
	return MemoryMergeRequest{
		Search: func(context.Context) ([]MemoryMergeReference, error) { return nil, nil },
		BuildMessages: func([]MemoryMergeReference) ([]llms.MessageContent, error) {
			return []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "extract")}, nil
		},
		MaxTokens: maxTokens,
	}
}

func TestMemoryMergeEngineRunsSearchAndModelInOrder(t *testing.T) {
	model := &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"ignore","scope":"temporary"}]}`}}
	engine := NewMemoryMergeEngine(model)
	var searched bool
	refs, raw, err := engine.Extract(context.Background(), MemoryMergeRequest{
		Search: func(context.Context) ([]MemoryMergeReference, error) {
			searched = true
			return []MemoryMergeReference{{Scope: "temporary", ID: "tmp_1", Content: "old"}}, nil
		},
		BuildMessages: func(refs []MemoryMergeReference) ([]llms.MessageContent, error) {
			if len(refs) != 1 || refs[0].ID != "tmp_1" {
				t.Fatalf("references=%#v", refs)
			}
			return []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("raw + " + refs[0].Content)}}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !searched || len(refs) != 1 || refs[0].ID != "tmp_1" || raw == "" {
		t.Fatalf("searched=%v refs=%#v raw=%q", searched, refs, raw)
	}
}

// A cap applied to a per-item product used to give a full batch a smaller
// per-item share than a single item, which is how batched episodes were
// squeezed into truncation.
func TestMemoryMergeTokenBudgetKeepsPerItemShareAsBatchGrows(t *testing.T) {
	single := memoryMergeTokenBudget(episodeMemoryBatchTokensPerEpisode, 1, episodeMemoryBatchMaxTokens, 0)
	if single != episodeMemoryBatchTokensPerEpisode {
		t.Fatalf("single-episode budget = %d, want %d", single, episodeMemoryBatchTokensPerEpisode)
	}
	for count := 2; count <= episodeMemoryBatchLimit; count++ {
		budget := memoryMergeTokenBudget(episodeMemoryBatchTokensPerEpisode, count, episodeMemoryBatchMaxTokens, 0)
		if share := budget / count; share < single {
			t.Fatalf("count=%d budget=%d per-episode share=%d, want at least %d", count, budget, share, single)
		}
	}
}

func TestMemoryMergeTokenBudgetGrowsWithAttemptUpToCap(t *testing.T) {
	const perItem, ceiling = 2000, 9000
	base := memoryMergeTokenBudget(perItem, 1, ceiling, 0)
	if base != perItem {
		t.Fatalf("attempt 0 budget = %d, want %d", base, perItem)
	}
	previous := base
	for attempt := 1; attempt <= 3; attempt++ {
		budget := memoryMergeTokenBudget(perItem, 1, ceiling, attempt)
		if budget <= previous {
			t.Fatalf("attempt %d budget = %d, want more than attempt %d budget %d", attempt, budget, attempt-1, previous)
		}
		previous = budget
	}
	if saturated := memoryMergeTokenBudget(perItem, 1, ceiling, 50); saturated != ceiling {
		t.Fatalf("saturated budget = %d, want the ceiling %d", saturated, ceiling)
	}
	if guarded := memoryMergeTokenBudget(perItem, 0, ceiling, -1); guarded != perItem {
		t.Fatalf("guarded budget = %d, want %d for a degenerate batch and attempt", guarded, perItem)
	}
}

// A budget-exhausted response is incomplete JSON. Reporting it as truncation
// rather than letting it reach the parser is what makes it retryable instead of
// surfacing as an opaque "unexpected end of JSON input".
func TestMemoryMergeExtractReportsTruncationWithPartialContent(t *testing.T) {
	partial := `{"results":[{"episode_id":"ep_1","proposal":{"candidates":[{"lesson_key":"k`
	model := &episodeMemoryScriptedModel{responses: []string{partial}, stopReason: "length"}
	_, raw, err := NewMemoryMergeEngine(model).Extract(context.Background(), memoryMergeProbeRequest(2600))
	if !errors.Is(err, errMemoryMergeTruncated) {
		t.Fatalf("Extract() error = %v, want errMemoryMergeTruncated", err)
	}
	// The partial body comes back so the caller can log where output stopped.
	if !strings.Contains(raw, "lesson_key") {
		t.Fatalf("Extract() raw = %q, want the partial body for diagnostics", raw)
	}
	for _, want := range []string{"stop_reason=length", "max_tokens=2600"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Extract() error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestMemoryMergeExtractClassifiesFinishedAndEmptyResponses(t *testing.T) {
	finished := &episodeMemoryScriptedModel{responses: []string{`{"results":[]}`}}
	if _, raw, err := NewMemoryMergeEngine(finished).Extract(context.Background(), memoryMergeProbeRequest(2600)); err != nil {
		t.Fatalf("Extract() on a finished response error = %v", err)
	} else if !strings.Contains(raw, "results") {
		t.Fatalf("Extract() raw = %q, want the finished body", raw)
	}
	empty := &episodeMemoryScriptedModel{responses: []string{"   "}}
	if _, _, err := NewMemoryMergeEngine(empty).Extract(context.Background(), memoryMergeProbeRequest(2600)); !errors.Is(err, errMemoryMergeEmpty) {
		t.Fatalf("Extract() error = %v, want errMemoryMergeEmpty", err)
	}
}

// Truncation must win over the empty check: a response that spent its whole
// budget on reasoning arrives with no content and stop_reason=length, and is a
// budget failure rather than a model that chose to say nothing.
func TestMemoryMergeExtractPrefersTruncationOverEmptyContent(t *testing.T) {
	model := &episodeMemoryScriptedModel{responses: []string{"   "}, stopReason: "max_tokens"}
	if _, _, err := NewMemoryMergeEngine(model).Extract(context.Background(), memoryMergeProbeRequest(2600)); !errors.Is(err, errMemoryMergeTruncated) {
		t.Fatalf("Extract() error = %v, want errMemoryMergeTruncated", err)
	}
}

// The tests above set StopReason on a fake, which only proves the engine reacts
// to the field. This one drives the real HTTP decoder with the wire shape the
// board actually received, so the whole chain is covered: provider
// finish_reason -> decoded StopReason -> truncation error. Without it, a decoder
// that dropped finish_reason would leave every other test passing while the fix
// silently did nothing on device.
func TestMemoryMergeExtractDetectsTruncationThroughRealDecoder(t *testing.T) {
	// Truncated mid-object, exactly as a budget-exhausted proposal arrives.
	const partial = `{"results":[{"episode_id":"ep_1","proposal":{"candidates":[{"lesson_key":"k`
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": partial},
			"finish_reason": "length",
		}},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	raw := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	_, content, err := NewMemoryMergeEngine(memoryMergeRealModel{Model: raw}).
		Extract(context.Background(), memoryMergeProbeRequest(2600))
	if !errors.Is(err, errMemoryMergeTruncated) {
		t.Fatalf("Extract() error = %v, want errMemoryMergeTruncated through the real decoder", err)
	}
	if !strings.Contains(content, "lesson_key") {
		t.Fatalf("Extract() content = %q, want the partial body for diagnostics", content)
	}
	// Confirm the old behavior is what we escaped: this body reaching the parser
	// is precisely the "unexpected end of JSON input" seen in the board logs.
	var probe map[string]any
	if parseErr := json.Unmarshal([]byte(content), &probe); parseErr == nil {
		t.Fatal("partial body parsed cleanly; it no longer reproduces the logged failure")
	} else if !strings.Contains(parseErr.Error(), "unexpected end of JSON input") {
		t.Fatalf("partial body parse error = %v, want the logged unexpected-end-of-input failure", parseErr)
	}
}

// A normally-finished response must not be mistaken for truncation, or every
// successful extraction would be retried until the attempt limit discarded it.
func TestMemoryMergeExtractAcceptsFinishedResponseThroughRealDecoder(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": `{"results":[]}`},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	raw := newOpenAICompatibleModel(server.URL, "test-model", "", server.Client())
	_, content, err := NewMemoryMergeEngine(memoryMergeRealModel{Model: raw}).
		Extract(context.Background(), memoryMergeProbeRequest(2600))
	if err != nil {
		t.Fatalf("Extract() on a finished response error = %v", err)
	}
	if content != `{"results":[]}` {
		t.Fatalf("Extract() content = %q, want the finished body", content)
	}
}

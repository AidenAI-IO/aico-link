package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"aico-agent/internal/agent/screen"

	"github.com/tmc/langchaingo/llms"
)

// ScreenMemoryOptions configures the screen memory pipeline.
type ScreenMemoryOptions struct {
	// TTL is the retention period for a captured memory, e.g. "90d", or
	// "forever" to disable expiry.
	TTL string
}

// screenMemoryVisionModel is the vision call the pipeline needs. It matches
// llms.Model, so a resolved model.Model satisfies it directly.
type screenMemoryVisionModel interface {
	GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error)
}

// ScreenMemoryPipeline runs frame capture → vision extraction → store write.
//
// It deliberately bypasses the agent loop: no RunRequest, no dialog history, no
// tool calls. A capture must not disturb an agent task already in flight, and
// must not depend on one being idle.
type ScreenMemoryPipeline struct {
	frameClient screenshotFrameClient
	screenState *screen.ScreenState
	model       screenMemoryVisionModel
	store       *LongTermMemoryStore
	options     ScreenMemoryOptions
}

// NewScreenMemoryPipeline builds the pipeline. screenState may be nil; it is
// only read to help resolve the active area, never written.
func NewScreenMemoryPipeline(
	frameClient screenshotFrameClient,
	screenState *screen.ScreenState,
	model screenMemoryVisionModel,
	store *LongTermMemoryStore,
	options ScreenMemoryOptions,
) *ScreenMemoryPipeline {
	return &ScreenMemoryPipeline{
		frameClient: frameClient,
		screenState: screenState,
		model:       model,
		store:       store,
		options:     options,
	}
}

// screenMemoryExtraction is the JSON the vision model is asked to return.
type screenMemoryExtraction struct {
	Summary  string   `json:"summary"`
	KeyText  []string `json:"key_text"`
	Tags     []string `json:"tags"`
	Entities []string `json:"entities"`
}

const screenMemoryVisionPrompt = `Describe what is on this screen in one sentence, then extract the details worth retrieving later.

Return only JSON:
{
  "summary": "one sentence describing the screen",
  "key_text": ["exact strings: tracking numbers, addresses, amounts, dates, names"],
  "tags": ["topic keywords for retrieval"],
  "entities": ["named things: apps, people, services"]
}

Copy key_text verbatim from the screen, including digits and punctuation — these are the values the user will ask about later. Write summary and tags in the language shown on screen.`

// Capture runs the pipeline and returns the id of the memory it wrote.
//
// Either a capture is written and retrievable, or nothing is written and an
// error is returned. There is no partial state: a memory holding no askable
// content is worse than none, because it reports success to the user.
func (p *ScreenMemoryPipeline) Capture(ctx context.Context) (string, error) {
	if p == nil {
		return "", fmt.Errorf("screen memory pipeline not configured")
	}
	if p.store == nil {
		return "", fmt.Errorf("long-term memory store not configured")
	}
	if p.model == nil {
		return "", fmt.Errorf("vision model not configured")
	}

	frame, err := captureActiveAreaFrame(p.frameClient, p.screenState, true)
	if err != nil {
		return "", fmt.Errorf("capture screen: %w", err)
	}

	extraction, err := p.extract(ctx, frame.Data)
	if err != nil {
		return "", err
	}

	return p.writeMemory(ctx, extraction)
}

// extract runs the vision call and parses its response.
func (p *ScreenMemoryPipeline) extract(ctx context.Context, jpegData []byte) (*screenMemoryExtraction, error) {
	messages := []llms.MessageContent{{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart(screenMemoryVisionPrompt),
			llms.ImageURLPart("data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegData)),
		},
	}}

	resp, err := p.model.GenerateContent(ctx, messages)
	if err != nil {
		return nil, fmt.Errorf("vision call: %w", err)
	}
	if resp == nil || len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Content) == "" {
		return nil, fmt.Errorf("vision call returned no content")
	}

	var extraction screenMemoryExtraction
	raw := stripJSONCodeFence(resp.Choices[0].Content)
	if err := json.Unmarshal([]byte(raw), &extraction); err != nil {
		return nil, fmt.Errorf("parse vision response: %w", err)
	}

	// A capture with neither a summary nor any key text holds nothing the user
	// could ask about, so it is a failure rather than an empty success.
	if strings.TrimSpace(extraction.Summary) == "" && len(nonEmptyStrings(extraction.KeyText)) == 0 {
		return nil, fmt.Errorf("vision response had no summary or key text")
	}
	return &extraction, nil
}

// writeMemory builds the MemoryItem and writes it to the long-term store.
func (p *ScreenMemoryPipeline) writeMemory(ctx context.Context, extraction *screenMemoryExtraction) (string, error) {
	summary := strings.TrimSpace(extraction.Summary)
	keyText := nonEmptyStrings(extraction.KeyText)

	// Key text goes into the body verbatim so retrieval can match the exact
	// value the user asks about, such as a tracking number.
	var content strings.Builder
	if summary != "" {
		content.WriteString(summary)
	}
	if len(keyText) > 0 {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		for _, text := range keyText {
			content.WriteString("- ")
			content.WriteString(text)
			content.WriteString("\n")
		}
	}

	title := summary
	if title == "" {
		title = keyText[0]
	}

	// This is a deliberate user capture, but the vision extraction is still
	// probabilistic, so it should not enter recall at absolute confidence.
	excerpts := keyText
	if len(excerpts) == 0 {
		excerpts = []string{summary}
	}

	item := MemoryItem{
		Type:             MemoryTypeScreenSnapshot,
		Title:            title,
		Content:          content.String(),
		Tags:             nonEmptyStrings(extraction.Tags),
		Entities:         nonEmptyStrings(extraction.Entities),
		Confidence:       0.9,
		TimeScope:        "long_term",
		TTL:              p.options.TTL,
		EvidenceExcerpts: excerpts,
		SourceRefs: []MemorySourceRef{{
			Type: MemorySourceTypeScreenCapture,
		}},
	}

	id, err := p.store.AddMemory(ctx, item)
	if err != nil {
		return "", fmt.Errorf("write screen memory: %w", err)
	}
	return id, nil
}

func nonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

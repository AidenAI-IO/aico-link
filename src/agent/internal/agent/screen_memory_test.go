package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/agent/screenprovider"
	"github.com/tmc/langchaingo/llms"
)

// fakeScreenFrameClient returns a canned JPEG frame.
type fakeScreenFrameClient struct {
	meta  *frameMetadata
	data  []byte
	err   error
	calls int
}

func (f *fakeScreenFrameClient) LatestFrameWithFormat(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, screenCaptureInfo{}, f.err
	}
	return f.meta, f.data, screenCaptureInfo{Backend: "fake"}, nil
}

// fakeVisionModel returns canned JSON, or an error.
type fakeVisionModel struct {
	response string
	err      error
	calls    int
	lastMsgs []llms.MessageContent
}

func (f *fakeVisionModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	f.calls++
	f.lastMsgs = messages
	if f.err != nil {
		return nil, f.err
	}
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{Content: f.response}},
	}, nil
}

func newTestScreenMemoryPipeline(t *testing.T, frames screenshotFrameClient, model screenMemoryVisionModel) (*ScreenMemoryPipeline, *LongTermMemoryStore) {
	t.Helper()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	p := NewScreenMemoryPipeline(frames, nil, model, store, ScreenMemoryOptions{TTL: "90d"})
	return p, store
}

func validFrameClient(t *testing.T) *fakeScreenFrameClient {
	t.Helper()
	return &fakeScreenFrameClient{
		meta: &frameMetadata{Width: 400, Height: 800, PixelFormat: "jpeg"},
		data: uniformScreenMemoryJPEG(t, 400, 800),
	}
}

const validVisionJSON = `{
  "summary": "WeChat chat window showing a SF Express tracking number",
  "key_text": ["SF1234567890", "预计明天送达"],
  "tags": ["快递", "物流"],
  "entities": ["微信", "顺丰"]
}`

func TestScreenMemoryPipelineWritesRetrievableMemory(t *testing.T) {
	ctx := context.Background()
	model := &fakeVisionModel{response: validVisionJSON}
	p, store := newTestScreenMemoryPipeline(t, validFrameClient(t), model)

	id, err := p.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	if id == "" {
		t.Fatal("Capture() returned an empty id")
	}
	if model.calls != 1 {
		t.Fatalf("vision model calls = %d, want exactly 1", model.calls)
	}

	results, err := store.Search(ctx, MemoryQuery{Types: []string{MemoryTypeScreenSnapshot}})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("search returned %d results, want 1", len(results))
	}
	got := results[0]

	if got.Type != MemoryTypeScreenSnapshot {
		t.Fatalf("type = %q, want %q", got.Type, MemoryTypeScreenSnapshot)
	}
	if got.Confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9", got.Confidence)
	}
	// Key text must survive into the body verbatim, or the specific value the
	// user will ask about is unanswerable.
	wantContent := "WeChat chat window showing a SF Express tracking number\n\n- SF1234567890\n- 预计明天送达"
	if got.Content != wantContent {
		t.Fatalf("content = %q, want %q", got.Content, wantContent)
	}
}

func TestScreenMemoryPipelineSetsTTLAndProvenance(t *testing.T) {
	ctx := context.Background()
	p, store := newTestScreenMemoryPipeline(t, validFrameClient(t), &fakeVisionModel{response: validVisionJSON})

	id, err := p.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	results, err := store.Search(ctx, MemoryQuery{Types: []string{MemoryTypeScreenSnapshot}})
	if err != nil || len(results) != 1 {
		t.Fatalf("Search() error = %v, results = %d", err, len(results))
	}
	parsed, err := readMemoryMarkdown(results[0].FilePath)
	if err != nil {
		t.Fatalf("readMemoryMarkdown(%q) error = %v", results[0].FilePath, err)
	}
	if parsed.Item.ExpiresAt == "" {
		t.Fatal("expires_at is empty: a 90d TTL must produce an expiry, the only reclamation path")
	}
	if parsed.Item.TTL != "90d" {
		t.Fatalf("ttl = %q, want %q", parsed.Item.TTL, "90d")
	}
	var sawProvenance bool
	for _, ref := range results[0].SourceRefs {
		if ref.Type == MemorySourceTypeScreenCapture {
			sawProvenance = true
		}
	}
	if !sawProvenance {
		t.Fatalf("no %q source ref on %s", MemorySourceTypeScreenCapture, id)
	}
}

func TestScreenMemoryPipelineSendsImageToModel(t *testing.T) {
	ctx := context.Background()
	model := &fakeVisionModel{response: validVisionJSON}
	p, _ := newTestScreenMemoryPipeline(t, validFrameClient(t), model)

	if _, err := p.Capture(ctx); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	var sawImage bool
	for _, msg := range model.lastMsgs {
		for _, part := range msg.Parts {
			if _, ok := part.(llms.ImageURLContent); ok {
				sawImage = true
			}
		}
	}
	if !sawImage {
		t.Fatal("no image part was sent to the vision model")
	}
}

func TestScreenMemoryPipelineWritesNothingOnFailure(t *testing.T) {
	// The invariant: a Screen Memory either exists and is retrievable, or was
	// never written. No half-saved entries that hold nothing askable.
	tests := []struct {
		name   string
		frames screenshotFrameClient
		model  screenMemoryVisionModel
	}{
		{
			name:   "stale frame",
			frames: &fakeScreenFrameClient{meta: &frameMetadata{Width: 400, Height: 800, PixelFormat: "jpeg", Stale: true}},
			model:  &fakeVisionModel{response: validVisionJSON},
		},
		{
			name:   "frame service unavailable",
			frames: &fakeScreenFrameClient{err: fmt.Errorf("dial unix: no such file")},
			model:  &fakeVisionModel{response: validVisionJSON},
		},
		{
			name:   "vision call fails",
			frames: nil, // filled in below
			model:  &fakeVisionModel{err: fmt.Errorf("network unreachable")},
		},
		{
			name:   "unparseable model output",
			frames: nil,
			model:  &fakeVisionModel{response: "I'm sorry, I can't help with that."},
		},
		{
			name:   "empty summary and key text",
			frames: nil,
			model:  &fakeVisionModel{response: `{"summary":"","key_text":[],"tags":[],"entities":[]}`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			frames := tc.frames
			if frames == nil {
				frames = validFrameClient(t)
			}
			p, store := newTestScreenMemoryPipeline(t, frames, tc.model)

			if _, err := p.Capture(ctx); err == nil {
				t.Fatal("Capture() succeeded, want an error")
			}

			results, err := store.Search(ctx, MemoryQuery{Types: []string{MemoryTypeScreenSnapshot}})
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if len(results) != 0 {
				t.Fatalf("failed capture wrote %d memories, want 0", len(results))
			}
		})
	}
}

func TestScreenMemoryPipelineToleratesFencedJSON(t *testing.T) {
	// Models often wrap JSON in a code fence even in JSON mode.
	ctx := context.Background()
	fenced := "```json\n" + validVisionJSON + "\n```"
	p, store := newTestScreenMemoryPipeline(t, validFrameClient(t), &fakeVisionModel{response: fenced})

	if _, err := p.Capture(ctx); err != nil {
		t.Fatalf("Capture() with fenced JSON error = %v", err)
	}
	results, _ := store.Search(ctx, MemoryQuery{Types: []string{MemoryTypeScreenSnapshot}})
	if len(results) != 1 {
		t.Fatalf("fenced JSON produced %d memories, want 1", len(results))
	}
}

func TestScreenMemoryPipelineAcceptsSummaryWithoutKeyText(t *testing.T) {
	// Not every screen has extractable specifics; a summary alone is still worth
	// keeping.
	ctx := context.Background()
	resp := `{"summary":"Home screen with no notable content","key_text":[],"tags":["主屏"],"entities":[]}`
	p, store := newTestScreenMemoryPipeline(t, validFrameClient(t), &fakeVisionModel{response: resp})

	if _, err := p.Capture(ctx); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	results, _ := store.Search(ctx, MemoryQuery{Types: []string{MemoryTypeScreenSnapshot}})
	if len(results) != 1 {
		t.Fatalf("summary-only capture produced %d memories, want 1", len(results))
	}
}

func TestScreenMemoryPipelineDoesNotTouchSharedScreenState(t *testing.T) {
	// Quick Capture must not perturb the coordinate mapping an in-flight agent
	// task depends on.
	ctx := context.Background()
	p, _ := newTestScreenMemoryPipeline(t, validFrameClient(t), &fakeVisionModel{response: validVisionJSON})

	if _, err := p.Capture(ctx); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	// The pipeline was constructed with a nil screen state; reaching it would
	// have panicked. Reaching here means it never wrote.
}

func TestScreenMemoryPipelineBase64EncodesFrame(t *testing.T) {
	ctx := context.Background()
	model := &fakeVisionModel{response: validVisionJSON}
	frames := validFrameClient(t)
	p, _ := newTestScreenMemoryPipeline(t, frames, model)

	if _, err := p.Capture(ctx); err != nil {
		t.Fatalf("Capture() error = %v", err)
	}

	for _, msg := range model.lastMsgs {
		for _, part := range msg.Parts {
			img, ok := part.(llms.ImageURLContent)
			if !ok {
				continue
			}
			const prefix = "data:image/jpeg;base64,"
			if !strings.HasPrefix(img.URL, prefix) {
				t.Fatalf("image URL prefix = %q, want %q", img.URL[:min(len(img.URL), 40)], prefix)
			}
			if _, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(img.URL, prefix)); err != nil {
				t.Fatalf("image payload is not valid base64: %v", err)
			}
			return
		}
	}
	t.Fatal("no image part found")
}

func uniformScreenMemoryJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, width, height))
	for index := range img.Pix {
		img.Pix[index] = 24
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

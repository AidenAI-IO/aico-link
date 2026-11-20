package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/agent/screen"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func writeTestPNG(t *testing.T, path string, width, height int, fill color.Color) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadImageToolIsBackendOnly(t *testing.T) {
	tool := NewReadImageTool()
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{tool.Name(): tool}}}}
	if !NewToolSpec(tool).AgentExposed {
		t.Fatal("backend must retain read_image")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tools/read_image", strings.NewReader(`{"input":{"path":"/tmp/photo.jpg"}}`))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("HTTP read_image status=%d", w.Code)
	}
}

func TestReadImageToolReturnsImageObservation(t *testing.T) {
	share := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", share)
	small := filepath.Join(t.TempDir(), "small.png")
	writeTestPNG(t, small, 40, 20, color.NRGBA{R: 200, A: 255})
	large := filepath.Join(share, "photos", "large.png")
	// Fully transparent: scaling must flatten it onto white, not black.
	writeTestPNG(t, large, 3200, 1600, color.NRGBA{})

	tool := NewReadImageTool()
	call := func(path string) readImageResult {
		t.Helper()
		out, err := tool.Call(context.Background(), `{"path":"`+path+`"}`)
		if err != nil {
			t.Fatal(err)
		}
		var result readImageResult
		if err := json.Unmarshal([]byte(out), &result); err != nil || result.Data == "" {
			t.Fatalf("read_image %s = %s", path, out)
		}
		return result
	}

	if got := call(small); got.Width != 40 || got.Height != 20 || got.Format != "png" || got.Path != small {
		t.Fatalf("small image result = %+v", got.screenshotResult)
	}

	got := call(large)
	if got.Width != readImageMaxEdge || got.Height != readImageMaxEdge/2 || got.Format != "jpeg" ||
		got.SourceWidth != 3200 || got.SourceHeight != 1600 {
		t.Fatalf("large image result = %+v", got.screenshotResult)
	}
	data, _ := base64.StdEncoding.DecodeString(got.Data)
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, _ := decoded.At(10, 10).RGBA(); r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Fatalf("transparent pixel = %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}

	// The loop attaches the image with a file caption and keeps only a short
	// summary in the transcript.
	agent := &FunctionAgent{Tools: []langtools.Tool{tool}}
	observation, _ := tool.Call(context.Background(), `{"path":"`+large+`"}`)
	content, followups := agent.observationMessagesForStep(schema.AgentStep{Action: schema.AgentAction{Tool: toolReadImage}, Observation: observation})
	if strings.Contains(content, got.Data[:32]) || !strings.Contains(content, `"width":1568`) {
		t.Fatalf("transcript tool result = %s", content)
	}
	if len(followups) != 1 || len(followups[0].Parts) != 2 {
		t.Fatalf("followups = %#v", followups)
	}
	caption, _ := followups[0].Parts[0].(llms.TextContent)
	if !strings.Contains(caption.Text, "the file "+large) || !strings.Contains(caption.Text, "not the connected display") ||
		!strings.Contains(caption.Text, "3200x1600") {
		t.Fatalf("caption = %q", caption.Text)
	}

	// The file is not the display: touch coordinates must not follow its size.
	state := &screen.ScreenState{}
	newScreenToolResultObserver(state)(context.Background(), ToolCall{Spec: NewToolSpec(tool)}, ToolResult{Output: observation})
	if _, _, ok := state.ScreenshotDimensions(); ok {
		t.Fatal("read_image result was recorded as the display screenshot")
	}
}

func TestReadImageToolRejectsInvalidPathsAndNonImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadImageTool()
	for name, input := range map[string]string{
		"missing path": `{}`,
		"relative":     `{"path":"sample.png"}`,
		"directory":    `{"path":"` + dir + `"}`,
		"absent":       `{"path":"` + filepath.Join(dir, "absent.png") + `"}`,
		"not image":    `{"path":"` + filepath.Join(dir, "notes.txt") + `"}`,
	} {
		ctx, _ := WithToolError(context.Background())
		out, err := tool.Call(ctx, input)
		if err != nil {
			t.Fatalf("%s: unexpected error %v", name, err)
		}
		if toolErr := ToolErrorFromContext(ctx); toolErr == nil || toolErr.Code != CodeInvalidArguments || strings.Contains(out, `"data"`) {
			t.Fatalf("%s: result = %s, error = %+v", name, out, toolErr)
		}
	}
}

// A PNG cut off after its header passes DecodeConfig; it must still be refused.
func TestEncodeImageForModelRejectsHeaderOnlyPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 40, 20))); err != nil {
		t.Fatal(err)
	}
	headerOnly := buf.Bytes()[:33] // signature + IHDR chunk
	if _, _, err := image.DecodeConfig(bytes.NewReader(headerOnly)); err != nil {
		t.Fatalf("fixture header does not parse: %v", err)
	}
	if _, err := encodeImageForModel(headerOnly); err == nil {
		t.Fatal("header-only PNG was accepted")
	}
}

// A GIF whose first frame covers part of the canvas keeps its margins.
func TestEncodeImageForModelKeepsGIFCanvas(t *testing.T) {
	palette := color.Palette{color.Black, color.NRGBA{R: 255, A: 255}}
	frame := image.NewPaletted(image.Rect(40, 10, 60, 90), palette)
	for y := 10; y < 90; y++ {
		for x := 40; x < 60; x++ {
			frame.SetColorIndex(x, y, 1)
		}
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{
		Image: []*image.Paletted{frame}, Delay: []int{0},
		Config: image.Config{Width: 100, Height: 100, ColorModel: palette},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := encodeImageForModel(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if result.Width != 100 || result.Height != 100 {
		t.Fatalf("size = %dx%d, want 100x100", result.Width, result.Height)
	}
	data, _ := base64.StdEncoding.DecodeString(result.Data)
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if r, g, b, _ := img.At(10, 50).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("margin pixel = %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
	if r, g, _, _ := img.At(50, 50).RGBA(); r>>8 < 200 || g>>8 > 60 {
		t.Fatalf("frame pixel = %d,%d, want red", r>>8, g>>8)
	}
}

package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestReadImageToolReturnsImageObservation(t *testing.T) {
	share := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", share)
	small := filepath.Join(share, "small.png")
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
}

func TestReadImageToolRejectsPathsOutsideShareAndNonImages(t *testing.T) {
	share := t.TempDir()
	t.Setenv("AICO_SHARE_DIR", share)
	outside := filepath.Join(t.TempDir(), "outside.png")
	writeTestPNG(t, outside, 4, 4, color.Black)
	if err := os.Symlink(outside, filepath.Join(share, "link.png")); err != nil {
		t.Fatal(err)
	}
	writeTestPNG(t, filepath.Join(share, ".hidden.png"), 4, 4, color.Black)
	if err := os.WriteFile(filepath.Join(share, "notes.txt"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadImageTool()
	for name, input := range map[string]string{
		"missing path": `{}`,
		"outside":      `{"path":"` + outside + `"}`,
		"traversal":    `{"path":"` + share + `/../x.png"}`,
		"symlink":      `{"path":"` + filepath.Join(share, "link.png") + `"}`,
		"hidden":       `{"path":"` + filepath.Join(share, ".hidden.png") + `"}`,
		"absent":       `{"path":"` + filepath.Join(share, "absent.png") + `"}`,
		"not image":    `{"path":"` + filepath.Join(share, "notes.txt") + `"}`,
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

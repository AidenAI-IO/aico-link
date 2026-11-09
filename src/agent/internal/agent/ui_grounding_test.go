package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"aico-agent/internal/agent/screen"

	langtools "github.com/tmc/langchaingo/tools"
)

type fakeUIGrounder struct {
	boxes  []uiElementBox
	err    error
	width  int
	height int
}

func (g *fakeUIGrounder) Detect(_ context.Context, rgb []byte, width, height int) ([]uiElementBox, error) {
	if len(rgb) != width*height*3 {
		return nil, fmt.Errorf("rgb has %d bytes for %dx%d", len(rgb), width, height)
	}
	g.width, g.height = width, height
	return g.boxes, g.err
}

func TestScreenshotToolGrounding(t *testing.T) {
	const width, height = 60, 120
	pixels := bytes.Repeat([]byte{240, 240, 240}, width*height)
	jpegData, err := encodeJPEG(pixels, width, height, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	newTool := func(grounder uiGrounder) (*ScreenshotTool, *screen.ScreenState) {
		state := &screen.ScreenState{}
		client := &fakeScreenshotFrameClient{
			meta: frameMetadata{Width: width, Height: height, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
			data: jpegData,
		}
		return &ScreenshotTool{client: client, screen: state, grounder: grounder, deviceTypeFn: func() string { return "macos" }}, state
	}
	// decode returns the result, the grounding the model reads in the tool
	// message, and the image.
	decode := func(t *testing.T, output string) (postActionScreenshotResult, screenshotGrounding, []byte) {
		t.Helper()
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("output %q: %v", output, err)
		}
		if result.ActionOutput != "ok" {
			t.Fatalf("action_output = %q, want ok with grounding in its own field", result.ActionOutput)
		}
		content, ok := compactScreenshotObservation("screenshot", output)
		if !ok {
			t.Fatalf("output %q is not a screenshot observation", output)
		}
		var message struct {
			ActionOutput string               `json:"action_output"`
			Grounding    *screenshotGrounding `json:"grounding"`
		}
		if err := json.Unmarshal([]byte(content), &message); err != nil || message.ActionOutput != "ok" || message.Grounding == nil {
			t.Fatalf("tool message %q, %v; want action_output ok and grounding", content, err)
		}
		image, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("decode data: %v", err)
		}
		return result, *message.Grounding, image
	}

	t.Run("marks control-sized elements in screenshot pixels", func(t *testing.T) {
		grounder := &fakeUIGrounder{boxes: []uiElementBox{
			{X1: 10, Y1: 20, X2: 50, Y2: 40, Score: 0.9},
			// Covers most of the screen: a page, not a tap target.
			{X1: 0, Y1: 0, X2: 60, Y2: 100, Score: 0.8},
			{X1: 0.4, Y1: 100.2, X2: 8.6, Y2: 119.9, Score: 0.5},
		}}
		tool, state := newTool(grounder)
		output, err := tool.Call(context.Background(), `{"grounding":true}`)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		result, grounded, image := decode(t, output)
		if grounder.width != width || grounder.height != height {
			t.Fatalf("detector saw %dx%d, want the %dx%d screenshot", grounder.width, grounder.height, width, height)
		}
		// Normalized 0-1000, the units touch_gesture takes natively:
		// pixel / (size-1) * 1000 on a 60x120 screenshot.
		want := []uiGroundingElement{
			{ID: 1, Center: [2]float64{508.5, 252.1}, Box: [4]float64{169.5, 168.1, 847.5, 336.1}},
			{ID: 2, Center: [2]float64{84.7, 924.4}, Box: [4]float64{0, 840.3, 152.5, 1000}},
		}
		if grounded.Error != "" || fmt.Sprint(grounded.Elements) != fmt.Sprint(want) {
			t.Fatalf("grounding = %+v, want elements %+v", grounded, want)
		}
		if bytes.Equal(image, jpegData) || result.Size != len(image) {
			t.Fatalf("image was not replaced by the marked screenshot (size %d, %d bytes)", result.Size, len(image))
		}
		marked, err := jpeg.Decode(bytes.NewReader(image))
		if err != nil || marked.Bounds().Dx() != width || marked.Bounds().Dy() != height {
			t.Fatalf("marked image = %v, %v; want %dx%d JPEG", marked.Bounds(), err, width, height)
		}
		// The shell export and visual coordinates read the image the model saw.
		latest, _, _, _, ok := state.LatestScreenshot(0)
		if !ok || !bytes.Equal(latest, image) {
			t.Fatal("screen state does not hold the marked screenshot")
		}
	})

	t.Run("failure keeps the plain screenshot", func(t *testing.T) {
		tool, _ := newTool(&fakeUIGrounder{err: errors.New("npu busy")})
		output, err := tool.Call(context.Background(), `{"grounding":true}`)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		_, grounded, image := decode(t, output)
		if grounded.Error != "npu busy" || len(grounded.Elements) != 0 || !bytes.Equal(image, jpegData) {
			t.Fatalf("grounding = %+v, want plain screenshot with the error", grounded)
		}
	})

	t.Run("default leaves the screenshot unmarked", func(t *testing.T) {
		grounder := &fakeUIGrounder{}
		tool, _ := newTool(grounder)
		output, err := tool.Call(context.Background(), `{}`)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || result.ActionOutput != "ok" || result.Grounding != nil || grounder.width != 0 {
			t.Fatalf("result = %+v, %v; want plain ok without detection", result, err)
		}
		if content, _ := compactScreenshotObservation("screenshot", output); content != "ok" {
			t.Fatalf("tool message = %q, want ok", content)
		}
	})
}

func TestHelperUIGrounderProtocol(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	script := fmt.Sprintf("#!/bin/sh\nAICO_UI_GROUNDING_HELPER=1 exec %q -test.run='^TestUIGroundingHelperProcess$'\n", os.Args[0])
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	grounder := newHelperUIGrounder(helper, dir)
	defer grounder.Close()

	rgb := make([]byte, 4*2*3)
	for request := 1; request <= 2; request++ {
		boxes, err := grounder.Detect(context.Background(), rgb, 4, 2)
		if err != nil {
			t.Fatalf("Detect() #%d error = %v", request, err)
		}
		// The fake helper reports its request count, proving the process stays resident.
		want := []uiElementBox{{X1: 0, Y1: 0, X2: 4, Y2: 2, Score: float64(request)}}
		if fmt.Sprint(boxes) != fmt.Sprint(want) {
			t.Fatalf("Detect() #%d = %+v, want %+v", request, boxes, want)
		}
	}
	if _, err := grounder.Detect(context.Background(), rgb, 3, 2); err == nil {
		t.Fatal("Detect() accepted RGB data that does not match the size")
	}
}

// TestUIGroundingHelperProcess is the fake helper started by TestHelperUIGrounderProtocol.
func TestUIGroundingHelperProcess(t *testing.T) {
	if os.Getenv("AICO_UI_GROUNDING_HELPER") != "1" {
		t.Skip("helper process only")
	}
	in := bufio.NewReader(os.Stdin)
	fmt.Println("READY 960 960")
	for requests := 1; ; requests++ {
		command, err := in.ReadByte()
		if err != nil || command == 'Q' {
			os.Exit(0)
		}
		var size [2]uint32
		if command != 'D' || binary.Read(in, binary.LittleEndian, &size) != nil {
			fmt.Println("ERR bad request")
			os.Exit(1)
		}
		if _, err := io.CopyN(io.Discard, in, int64(size[0])*int64(size[1])*3); err != nil {
			os.Exit(1)
		}
		fmt.Printf("OK 1 1.0\n0 0 %d %d %d\n", size[0], size[1], requests)
	}
}

// Under the visual coordinate protocol, touch_gesture takes screenshot pixels,
// so the wrapped screenshot tool must report grounding in the same pixels.
func TestVisualCoordinatesGroundingUsesTouchGestureUnits(t *testing.T) {
	const width, height = 60, 120
	jpegData, err := encodeJPEG(bytes.Repeat([]byte{240, 240, 240}, width*height), width, height, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	state := &screen.ScreenState{}
	screenshot := &ScreenshotTool{
		client: &fakeScreenshotFrameClient{
			meta: frameMetadata{Width: width, Height: height, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
			data: jpegData,
		},
		screen:       state,
		grounder:     &fakeUIGrounder{boxes: []uiElementBox{{X1: 10, Y1: 20, X2: 50, Y2: 40, Score: 0.9}}},
		deviceTypeFn: func() string { return "macos" },
	}
	touch := &visualRecordingTool{name: "touch_gesture"}
	frames := newVisualCoordinates(state)
	tools := frames.wrap([]langtools.Tool{screenshot, touch})

	output, err := tools[0].Call(context.Background(), `{"grounding":true}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(output), &result); err != nil || result.Grounding == nil || len(result.Grounding.Elements) != 1 {
		t.Fatalf("output %q, %v; want one grounded element", output, err)
	}
	element := result.Grounding.Elements[0]
	if element.Center != [2]float64{30, 30} || element.Box != [4]float64{10, 20, 50, 40} {
		t.Fatalf("element = %+v, want pixel center (30,30) and box (10,20,50,40)", element)
	}
	if result.Data == "" || result.ActionOutput != "ok" {
		t.Fatalf("result lost its image or action output: %+v", result.screenshotResult)
	}

	// The reported center goes into touch_gesture unchanged and reaches the
	// device as the same normalized point the plain screenshot tool reports.
	input, _ := json.Marshal(map[string]any{"type": "tap", "point": map[string]any{"x": element.Center[0], "y": element.Center[1]}})
	if _, err := tools[1].Call(context.Background(), string(input)); err != nil {
		t.Fatalf("touch_gesture Call() error = %v", err)
	}
	if len(touch.inputs) != 1 {
		t.Fatalf("touch_gesture inputs = %v", touch.inputs)
	}
	point, _ := touch.inputs[0]["point"].(map[string]any)
	x, _ := point["x"].(float64)
	y, _ := point["y"].(float64)
	if math.Abs(x-508.5) > 0.1 || math.Abs(y-252.1) > 0.1 {
		t.Fatalf("device point = %v, want the normalized element center (508.5, 252.1)", point)
	}
}

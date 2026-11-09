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
	"os"
	"path/filepath"
	"testing"

	"aico-agent/internal/agent/screen"
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
	decode := func(t *testing.T, output string) (postActionScreenshotResult, groundedScreenshotOutput, []byte) {
		t.Helper()
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("output %q: %v", output, err)
		}
		var grounded groundedScreenshotOutput
		if err := json.Unmarshal([]byte(result.ActionOutput), &grounded); err != nil {
			t.Fatalf("action_output %q: %v", result.ActionOutput, err)
		}
		image, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("decode data: %v", err)
		}
		return result, grounded, image
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
		want := []uiGroundingElement{
			{ID: 1, Center: [2]int{30, 30}, Box: [4]int{10, 20, 50, 40}},
			{ID: 2, Center: [2]int{5, 110}, Box: [4]int{0, 100, 9, 120}},
		}
		if !grounded.OK || grounded.GroundingError != "" || fmt.Sprint(grounded.Elements) != fmt.Sprint(want) {
			t.Fatalf("grounded output = %+v, want elements %+v", grounded, want)
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
		if grounded.GroundingError != "npu busy" || len(grounded.Elements) != 0 || !bytes.Equal(image, jpegData) {
			t.Fatalf("grounded output = %+v, want plain screenshot with the error", grounded)
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
		if err := json.Unmarshal([]byte(output), &result); err != nil || result.ActionOutput != "ok" || grounder.width != 0 {
			t.Fatalf("result = %+v, %v; want plain ok without detection", result, err)
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

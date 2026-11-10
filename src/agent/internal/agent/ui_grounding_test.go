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
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/screen"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
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
	decode := func(t *testing.T, output string) (postActionScreenshotResult, screenshotGrounding, []byte) {
		t.Helper()
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil || result.Grounding == nil {
			t.Fatalf("output %q, %v; want a grounding field", output, err)
		}
		requireGroundingInStateMessage(t, "screenshot", output, *result.Grounding)
		image, err := base64.StdEncoding.DecodeString(result.Data)
		if err != nil {
			t.Fatalf("decode data: %v", err)
		}
		return result, *result.Grounding, image
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
		toolContent, followups := (&FunctionAgent{Tools: []langtools.Tool{tool}}).observationMessagesForStep(schema.AgentStep{
			Action: schema.AgentAction{Tool: "screenshot"}, Observation: output,
		})
		if toolContent != "ok" || len(followups) != 1 || strings.Contains(followupText(followups[0]), "UI grounding") {
			t.Fatalf("tool content %q, followups %v; want ok and a plain screenshot", toolContent, followups)
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

// While UI Grounding is on the helper is loaded ahead of the first request
// and is never idled out; turning the setting off releases it.
func TestHelperUIGrounderResidency(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	script := fmt.Sprintf("#!/bin/sh\nAICO_UI_GROUNDING_HELPER=1 exec %q -test.run='^TestUIGroundingHelperProcess$'\n", os.Args[0])
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	grounder := newHelperUIGrounder(helper, dir)
	grounder.idleTimeout = 20 * time.Millisecond
	defer grounder.Close()
	running := func() bool {
		grounder.mu.Lock()
		defer grounder.mu.Unlock()
		return grounder.cmd != nil
	}
	waitFor := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for running() != want {
			if time.Now().After(deadline) {
				t.Fatalf("helper running = %v, want %v", !want, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	grounder.SetResident(true)
	waitFor(true) // preloaded without any request
	if _, err := grounder.Detect(context.Background(), make([]byte, 4*2*3), 4, 2); err != nil {
		t.Fatalf("Detect() error = %v", err)
	}
	time.Sleep(10 * grounder.idleTimeout)
	if !running() {
		t.Fatal("resident helper exited after the idle timeout")
	}

	grounder.SetResident(false)
	waitFor(false) // released when the setting is turned off

	// On demand again: an explicit request starts it, and it idles out.
	if _, err := grounder.Detect(context.Background(), make([]byte, 4*2*3), 4, 2); err != nil {
		t.Fatalf("on-demand Detect() error = %v", err)
	}
	waitFor(false)
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

	// A grounded post-action observation coming back through the wrapped
	// touch_gesture is converted to pixels as well.
	normalized := `{"width":60,"height":120,"format":"jpeg","size":1,"data":"YQ==","action_output":"ok",` +
		`"grounding":{"elements":[{"id":1,"center":[508.5,252.1],"box":[169.5,168.1,847.5,336.1]}]}}`
	gesture := frames.wrap([]langtools.Tool{&stubTool{name: "touch_gesture", output: normalized}})[0]
	output, err = gesture.Call(context.Background(), string(input))
	if err != nil {
		t.Fatalf("wrapped touch_gesture Call() error = %v", err)
	}
	var posted postActionScreenshotResult
	if err := json.Unmarshal([]byte(output), &posted); err != nil || posted.Grounding == nil || len(posted.Grounding.Elements) != 1 {
		t.Fatalf("post-action output %q, %v; want one grounded element", output, err)
	}
	if got := posted.Grounding.Elements[0]; got.Center != [2]float64{30, 30} || got.Box != [4]float64{10, 20, 50, 40} {
		t.Fatalf("post-action element = %+v, want pixel center (30,30) and box (10,20,50,40)", got)
	}
}

// requireGroundingInStateMessage checks what the model reads for a grounded
// observation: the tool result keeps the tool's own output, and the element
// list travels in the state message that carries the image.
func requireGroundingInStateMessage(t *testing.T, toolName, output string, grounding screenshotGrounding) {
	t.Helper()
	tool := &stubTool{name: toolName, visual: true}
	toolContent, followups := (&FunctionAgent{Tools: []langtools.Tool{tool}}).observationMessagesForStep(schema.AgentStep{
		Action: schema.AgentAction{Tool: toolName}, Observation: output,
	})
	if strings.Contains(toolContent, "elements") || strings.Contains(toolContent, "grounding") {
		t.Fatalf("tool content %q carries grounding; want it in the state message", toolContent)
	}
	if len(followups) != 1 {
		t.Fatalf("followups = %v, want one state message with the image", followups)
	}
	text := followupText(followups[0])
	want, _ := json.Marshal(grounding)
	if grounding.Error != "" {
		want = []byte(grounding.Error)
	}
	if !strings.Contains(text, string(want)) {
		t.Fatalf("state message %q does not carry grounding %s", text, want)
	}
}

func followupText(message llms.MessageContent) string {
	var text strings.Builder
	for _, part := range message.Parts {
		if part, ok := part.(llms.TextContent); ok {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

// Every post-action screenshot (stable-wait and fixed-delay tools alike)
// follows the live UI Grounding setting, and grounding goes into the state
// message while the tool result keeps its own output.
func TestPostActionScreenshotFollowsUIGroundingSetting(t *testing.T) {
	const width, height = 60, 120
	jpegData, err := encodeJPEG(bytes.Repeat([]byte{240, 240, 240}, width*height), width, height, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	newScreenshot := func(grounder uiGrounder) *ScreenshotTool {
		return &ScreenshotTool{
			client: &fakeScreenshotFrameClient{
				meta: frameMetadata{Width: width, Height: height, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
				data: jpegData,
			},
			screen:       &screen.ScreenState{},
			grounder:     grounder,
			deviceTypeFn: func() string { return "macos" },
		}
	}
	wait := &stubTool{name: "wait_for_stable_screen", output: `{"ok":true,"stable":true,"elapsed_ms":5}`}
	boxes := []uiElementBox{{X1: 10, Y1: 20, X2: 50, Y2: 40, Score: 0.9}}
	enabled := true
	setting := func() bool { return enabled }
	call := func(t *testing.T, tool langtools.Tool, input string) postActionScreenshotResult {
		t.Helper()
		tool.(*postActionScreenshotTool).SetUIGroundingFunc(setting)
		output, err := tool.Call(context.Background(), input)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("decode %q: %v", output, err)
		}
		if result.ActionOutput != "ok" {
			t.Fatalf("action_output = %q, want the tool's own output", result.ActionOutput)
		}
		if result.Grounding != nil {
			requireGroundingInStateMessage(t, tool.Name(), output, *result.Grounding)
		}
		return result
	}
	tools := map[string]func(uiGrounder) langtools.Tool{
		"stable wait (touch_gesture)": func(g uiGrounder) langtools.Tool {
			return newPostActionStableScreenshotTool(&stubTool{name: "touch_gesture", output: "ok"}, wait, newScreenshot(g), 0, ScreenStableDefaults{})
		},
		"fixed delay (enter_text)": func(g uiGrounder) langtools.Tool {
			return newPostActionScreenshotTool(&stubTool{name: "enter_text", output: "ok"}, newScreenshot(g), 0)
		},
	}
	for name, build := range tools {
		t.Run(name, func(t *testing.T) {
			enabled = true
			on := &fakeUIGrounder{boxes: boxes}
			if result := call(t, build(on), `{"type":"tap","point":{"x":500,"y":250}}`); result.Grounding == nil || len(result.Grounding.Elements) != 1 || on.width != width {
				t.Fatalf("setting on: grounding = %+v, want one element", result.Grounding)
			}
			enabled = false
			off := &fakeUIGrounder{boxes: boxes}
			if result := call(t, build(off), `{"type":"tap","point":{"x":500,"y":250}}`); result.Grounding != nil || off.width != 0 {
				t.Fatalf("setting off: grounding = %+v, detector ran = %v", result.Grounding, off.width != 0)
			}
			// A device without the detector stays plain and silent.
			enabled = true
			if result := call(t, build(nil), `{"type":"tap","point":{"x":500,"y":250}}`); result.Grounding != nil {
				t.Fatalf("no detector: grounding = %+v, want none", result.Grounding)
			}
		})
	}
}

// The screenshot attached to a user turn follows the live UI Grounding
// setting, and its element list goes into the state message text.
func TestStateHookGroundsUserTurnScreenshotWhenEnabled(t *testing.T) {
	const width, height = 60, 120
	jpegData, err := encodeJPEG(bytes.Repeat([]byte{240, 240, 240}, width*height), width, height, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			grounder := &fakeUIGrounder{boxes: []uiElementBox{{X1: 10, Y1: 20, X2: 50, Y2: 40, Score: 0.9}}}
			screenshot := &ScreenshotTool{
				client: &fakeScreenshotFrameClient{
					meta: frameMetadata{Width: width, Height: height, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
					data: jpegData,
				},
				screen:       &screen.ScreenState{},
				grounder:     grounder,
				deviceTypeFn: func() string { return "macos" },
			}
			tools := &ToolSet{tools: map[string]langtools.Tool{"screenshot": screenshot}}
			// The runtime installs the live ui_grounding_enabled setting.
			runtime := NewRuntimeWithDeps(Config{UIGroundingEnabled: &enabled}, nil, nil, tools, NewSkillIndex())
			manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime.contextManager = manager
			manager.AddAppendMessageHook(runtime.getStateHook())
			if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
				t.Fatalf("AppendMessage() error = %v", err)
			}
			state := manager.MessageListDump().Messages[0]
			if state.Role != messages.MessageRoleState || len(state.Attachments) != 1 {
				t.Fatalf("state message = %#v, want one screenshot attachment", state)
			}
			grounded := strings.Contains(state.Content, "UI grounding:") && strings.Contains(state.Content, `"center":[508.5,252.1]`)
			if grounded != enabled || (grounder.width != 0) != enabled {
				t.Fatalf("state content %q, detector ran = %v; want grounding = %v", state.Content, grounder.width != 0, enabled)
			}
		})
	}
}

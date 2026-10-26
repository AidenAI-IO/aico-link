package agent

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"slices"
	"strings"
	"testing"
)

func TestScreenStableDefaultsResolved(t *testing.T) {
	t.Parallel()

	defaults := ScreenStableDefaults{}.Resolved()
	if defaults.TimeoutMs != defaultStableWaitTimeoutMs {
		t.Fatalf("TimeoutMs = %d, want %d", defaults.TimeoutMs, defaultStableWaitTimeoutMs)
	}
	if defaults.StableMs != defaultStableDurationMs {
		t.Fatalf("StableMs = %d, want %d", defaults.StableMs, defaultStableDurationMs)
	}

	custom := ScreenStableDefaults{TimeoutMs: 8000, StableMs: 9000}.Resolved()
	if custom.TimeoutMs != 8000 {
		t.Fatalf("TimeoutMs = %d, want 8000", custom.TimeoutMs)
	}
	if custom.StableMs != 8000 {
		t.Fatalf("StableMs = %d, want clamped to 8000", custom.StableMs)
	}
}

func TestScreenStableDefaultsInputJSON(t *testing.T) {
	t.Parallel()

	got := ScreenStableDefaults{TimeoutMs: 4500, StableMs: 600, DiffThreshold: 2.5}.InputJSON()
	want := `{"timeout_ms":4500,"stable_ms":600,"diff_threshold":2.5}`
	if got != want {
		t.Fatalf("InputJSON() = %q, want %q", got, want)
	}
}

func TestWaitStableScreenToolReturnsScreenshotObservationJSON(t *testing.T) {
	rawFrame := []byte{128, 128, 128, 128, 128, 128}
	jpegData, err := encodeJPEG([]byte{
		255, 255, 255, 255, 255, 255,
		255, 255, 255, 255, 255, 255,
	}, 2, 2, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	screen := &screen.ScreenState{}
	client := &fakeWaitStableFrameClient{
		rawFrames: []fakeWaitStableFrame{
			{meta: frameMetadata{Seq: 1, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrame},
			{meta: frameMetadata{Seq: 2, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrame},
		},
		jpegData: jpegData,
		jpegMeta: frameMetadata{Seq: 99, Width: 2, Height: 2, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
	}
	tool := &WaitStableScreenTool{
		client:   client,
		defaults: ScreenStableDefaults{TimeoutMs: 50, StableMs: 1, DiffThreshold: 2},
		screen:   screen,
	}

	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("Call() returned error output: %s", out)
	}

	var result waitStableScreenObservationResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid wait screenshot JSON: %v", err)
	}
	if !result.OK || !result.Stable {
		t.Fatalf("stable result = ok:%v stable:%v, want true/true", result.OK, result.Stable)
	}
	if result.ScreenChanged == nil || *result.ScreenChanged {
		t.Fatalf("ScreenChanged = %#v, want false", result.ScreenChanged)
	}
	if result.ScreenStable == nil || !*result.ScreenStable {
		t.Fatalf("ScreenStable = %#v, want true", result.ScreenStable)
	}
	if result.StableWaitMs == nil || *result.StableWaitMs != result.ElapsedMs {
		t.Fatalf("StableWaitMs = %#v, elapsed_ms=%d", result.StableWaitMs, result.ElapsedMs)
	}
	var toolResult map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result.ActionOutput), &toolResult); err != nil {
		t.Fatalf("action_output is not valid stable-screen result JSON: %v", err)
	}
	if _, ok := toolResult["data"]; ok {
		t.Fatalf("action_output contains screenshot data: %s", result.ActionOutput)
	}
	var stableResult waitStableScreenResult
	if err := json.Unmarshal([]byte(result.ActionOutput), &stableResult); err != nil {
		t.Fatalf("decode action_output: %v", err)
	}
	if !stableResult.OK || !stableResult.Stable || stableResult.ElapsedMs != result.ElapsedMs {
		t.Fatalf("action_output stable result = %#v, want top-level stable result", stableResult)
	}
	if result.Width != 2 || result.Height != 2 || result.Format != "jpeg" || result.Size != len(jpegData) {
		t.Fatalf("unexpected screenshot metadata: %#v", result.screenshotResult)
	}
	if result.Data != base64.StdEncoding.EncodeToString(jpegData) {
		t.Fatalf("unexpected screenshot data: %q", result.Data)
	}
	if client.jpegCalls != 1 {
		t.Fatalf("jpegCalls = %d, want 1", client.jpegCalls)
	}
	if width, height, _, ok := screen.DimensionsWithAge(); !ok || width != 2 || height != 2 {
		t.Fatalf("screen dimensions = %dx%d ok=%v, want 2x2 true", width, height, ok)
	}
	latest, latestWidth, latestHeight, _, ok := screen.LatestScreenshot(screenDimensionsStaleAfter)
	if !ok || latestWidth != 2 || latestHeight != 2 || !bytes.Equal(latest, jpegData) {
		t.Fatalf("latest screenshot = %dx%d bytes=%d ok=%v, want 2x2 bytes=%d true", latestWidth, latestHeight, len(latest), ok, len(jpegData))
	}
	visual, ok := any(tool).(visualObservationTool)
	if !ok || !visual.ReturnsVisualObservation() {
		t.Fatalf("wait_for_stable_screen must be a visual observation tool")
	}
}

func TestWaitStableScreenToolPropagatesContextCancellation(t *testing.T) {
	rawFrame := []byte{128, 128, 128, 128, 128, 128}
	client := &fakeWaitStableFrameClient{
		rawFrames: []fakeWaitStableFrame{
			{meta: frameMetadata{Seq: 1, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrame},
		},
	}
	tool := &WaitStableScreenTool{
		client:   client,
		defaults: ScreenStableDefaults{TimeoutMs: 1000, StableMs: 500, DiffThreshold: 2},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Call(ctx, `{}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context.Canceled", err)
	}
}

func TestWaitStableScreenToolWaitReportsScreenChangedWhenFramesDiffer(t *testing.T) {
	rawFrameA := []byte{128, 128, 128, 128, 128, 128}
	rawFrameB := []byte{140, 140, 140, 140, 128, 128}
	client := &fakeWaitStableFrameClient{
		rawFrames: []fakeWaitStableFrame{
			{meta: frameMetadata{Seq: 1, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrameA},
			{meta: frameMetadata{Seq: 2, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrameB},
		},
	}
	tool := &WaitStableScreenTool{
		client:   client,
		defaults: ScreenStableDefaults{TimeoutMs: 50, StableMs: 20, DiffThreshold: 2},
	}

	result, err := tool.wait(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("wait() error = %v", err)
	}
	if result.ScreenChanged == nil || !*result.ScreenChanged {
		t.Fatalf("ScreenChanged = %#v, want true", result.ScreenChanged)
	}
	if result.LastDiff == nil || *result.LastDiff <= 0 {
		t.Fatalf("LastDiff = %#v, want > 0", result.LastDiff)
	}
}

func TestDragStartUsesInternalStableWaitBeforeReturningScreenshot(t *testing.T) {
	events := make([]string, 0, 4)
	action := &stubTool{name: "touch_gesture"}
	action.callFn = func(context.Context, string) (string, error) {
		events = append(events, "action")
		return "ok", nil
	}
	waitStable := &fakeInternalWaitTool{
		result: waitStableScreenResult{OK: true, Stable: true, ElapsedMs: 12},
		onWait: func() {
			events = append(events, "stable wait")
		},
	}
	screenshot := &stubTool{
		name: "screenshot",
	}
	screenshot.callFn = func(context.Context, string) (string, error) {
		if len(screenshot.inputs) == 1 {
			events = append(events, "pre-action baseline")
		} else {
			events = append(events, "post-stable final")
		}
		return `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`, nil
	}
	tool := newPostActionStableScreenshotTool(action, waitStable, screenshot, 0, ScreenStableDefaults{TimeoutMs: 50, StableMs: 1, DiffThreshold: 2})

	out, err := tool.Call(context.Background(), `{"type":"drag_start","point":{"x":400,"y":500}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("Call() returned error output: %s", out)
	}
	if waitStable.callCount != 0 {
		t.Fatalf("wait tool Call count = %d, want 0", waitStable.callCount)
	}
	if waitStable.waitCount != 1 {
		t.Fatalf("wait count = %d, want 1", waitStable.waitCount)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ScreenStable == nil || !*result.ScreenStable {
		t.Fatalf("drag_start ScreenStable = %#v, want true", result.ScreenStable)
	}
	if len(screenshot.inputs) != 2 {
		t.Fatalf("post-action screenshot inputs = %#v, want baseline and final captures", screenshot.inputs)
	}
	if want := []string{"pre-action baseline", "action", "stable wait", "post-stable final"}; !slices.Equal(events, want) {
		t.Fatalf("post-action call order = %#v, want %#v", events, want)
	}
}

func TestDragStartUnstableWaitReturnsToStartAndReleasesContact(t *testing.T) {
	provider := &dragRecoveryProvider{}
	action := &TouchGestureTool{mnkProvider: provider}
	waitStable := &fakeInternalWaitTool{
		result: waitStableScreenResult{OK: true, Stable: false, ElapsedMs: 50},
	}
	screenshot := &stubTool{
		name:   "screenshot",
		output: `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`,
	}
	tool := newPostActionStableScreenshotTool(action, waitStable, screenshot, 0, ScreenStableDefaults{TimeoutMs: 50, StableMs: 1, DiffThreshold: 2})

	out, err := tool.Call(context.Background(), `{"type":"drag_start","point":{"x":400,"y":500}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ScreenStable == nil || *result.ScreenStable {
		t.Fatalf("ScreenStable = %#v, want false", result.ScreenStable)
	}
	if provider.dragActive {
		t.Fatal("unstable drag_start left contact active")
	}
	if provider.releaseX != 400 || provider.releaseY != 500 {
		t.Fatalf("unstable recovery released at (%v,%v), want original point (400,500)", provider.releaseX, provider.releaseY)
	}

	// A second start succeeds only if the unstable first drag was released.
	if out, err := action.Call(context.Background(), `{"type":"drag_start","point":{"x":600,"y":500}}`); err != nil || out != "ok" {
		t.Fatalf("second drag_start = %q, %v; unstable recovery left contact active", out, err)
	}
	if out, err := action.Call(context.Background(), `{"type":"drag_release","point":{"x":600,"y":500}}`); err != nil || out != "ok" {
		t.Fatalf("cleanup drag_release = %q, %v", out, err)
	}
}

type dragRecoveryProvider struct {
	mnk.Provider
	dragActive bool
	releaseX   float64
	releaseY   float64
}

func (p *dragRecoveryProvider) DragStart(context.Context, float64, float64, string) error {
	if p.dragActive {
		return errors.New("drag already active")
	}
	p.dragActive = true
	return nil
}

func (p *dragRecoveryProvider) DragRelease(_ context.Context, x, y float64) error {
	if !p.dragActive {
		return errors.New("drag is not active")
	}
	p.releaseX, p.releaseY = x, y
	p.dragActive = false
	return nil
}

func TestWaitStableScreenToolUsesJPEGSourceMetadataForSharedScreenState(t *testing.T) {
	rawFrame := []byte{128, 128, 128, 128, 128, 128}
	jpegData, err := encodeJPEG([]byte{
		255, 255, 255, 255, 255, 255,
		255, 255, 255, 255, 255, 255,
	}, 2, 2, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	screenState := &screen.ScreenState{}
	client := &fakeWaitStableFrameClient{
		rawFrames: []fakeWaitStableFrame{
			{meta: frameMetadata{Seq: 1, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrame},
			{meta: frameMetadata{Seq: 2, Width: 2, Height: 2, PixelFormat: "nv12"}, data: rawFrame},
		},
		jpegData: jpegData,
		jpegMeta: frameMetadata{
			Seq:          99,
			Width:        2,
			Height:       2,
			SourceWidth:  16,
			SourceHeight: 9,
			CropX:        5,
			CropY:        0,
			CropWidth:    5,
			CropHeight:   9,
			PixelFormat:  "jpeg",
			Bytes:        uint64(len(jpegData)),
		},
	}
	tool := &WaitStableScreenTool{
		client:   client,
		defaults: ScreenStableDefaults{TimeoutMs: 50, StableMs: 1, DiffThreshold: 2},
		screen:   screenState,
	}

	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("Call() returned error output: %s", out)
	}

	var result waitStableScreenObservationResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid wait screenshot JSON: %v", err)
	}
	want := screen.ScreenActiveArea{X: 5, Y: 0, Width: 5, Height: 9, Valid: true}
	if result.SourceWidth != 16 || result.SourceHeight != 9 || result.ActiveArea == nil || *result.ActiveArea != want || result.ActiveWidth != want.Width || result.ActiveHeight != want.Height {
		t.Fatalf("unexpected source mapping metadata: %#v", result.screenshotResult)
	}
	width, height, active, _, ok := screenState.ActiveAreaWithAge()
	if !ok || width != 16 || height != 9 {
		t.Fatalf("screen dimensions = %dx%d ok=%v, want 16x9 true", width, height, ok)
	}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestWaitStableScreenToolCropsDetectedActiveAreaForModelObservation(t *testing.T) {
	rawFrame := make([]byte, 8*4*3/2)
	for i := range rawFrame {
		rawFrame[i] = 128
	}
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := 0; y < 4; y++ {
		for x := 2; x < 6; x++ {
			img.Set(x, y, color.RGBA{255, 255, 255, 255})
		}
	}
	var src []byte
	for y := 0; y < 4; y++ {
		for x := 0; x < 8; x++ {
			r, _, _, _ := img.At(x, y).RGBA()
			src = append(src, byte(r>>8), byte(r>>8), byte(r>>8))
		}
	}
	fullJPEGData, err := encodeJPEG(src, 8, 4, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}

	screenState := &screen.ScreenState{}
	client := &fakeWaitStableFrameClient{
		rawFrames: []fakeWaitStableFrame{
			{meta: frameMetadata{Seq: 1, Width: 8, Height: 4, PixelFormat: "nv12"}, data: rawFrame},
			{meta: frameMetadata{Seq: 2, Width: 8, Height: 4, PixelFormat: "nv12"}, data: rawFrame},
		},
		jpegData: fullJPEGData,
		jpegMeta: frameMetadata{Seq: 99, Width: 8, Height: 4, PixelFormat: "jpeg", Bytes: uint64(len(fullJPEGData))},
	}
	tool := &WaitStableScreenTool{
		client:   client,
		defaults: ScreenStableDefaults{TimeoutMs: 50, StableMs: 1, DiffThreshold: 2},
		screen:   screenState,
	}

	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	var result waitStableScreenObservationResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid wait screenshot JSON: %v", err)
	}
	if result.Width != 4 || result.Height != 4 {
		t.Fatalf("cropped screenshot dimensions = %dx%d, want 4x4", result.Width, result.Height)
	}
	want := screen.ScreenActiveArea{X: 2, Y: 0, Width: 4, Height: 4, Valid: true}
	if result.SourceWidth != 8 || result.SourceHeight != 4 || result.ActiveArea == nil || *result.ActiveArea != want || result.ActiveWidth != want.Width || result.ActiveHeight != want.Height {
		t.Fatalf("unexpected cropped observation mapping metadata: %#v", result.screenshotResult)
	}
	if result.Data == base64.StdEncoding.EncodeToString(fullJPEGData) {
		t.Fatal("expected cropped screenshot bytes, got original full-frame JPEG")
	}
	imageBytes, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	if result.Size != len(imageBytes) {
		t.Fatalf("result size = %d, want %d", result.Size, len(imageBytes))
	}
	decoded, err := jpegDecodeConfig(imageBytes)
	if err != nil {
		t.Fatalf("jpegDecodeConfig() error = %v", err)
	}
	if decoded.Width != 4 || decoded.Height != 4 {
		t.Fatalf("decoded cropped jpeg = %dx%d, want 4x4", decoded.Width, decoded.Height)
	}
	width, height, active, _, ok := screenState.ActiveAreaWithAge()
	if !ok || width != 8 || height != 4 {
		t.Fatalf("screen dimensions = %dx%d ok=%v, want 8x4 true", width, height, ok)
	}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func jpegDecodeConfig(data []byte) (image.Config, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	return config, err
}

type fakeWaitStableFrame struct {
	meta frameMetadata
	data []byte
}

type fakeWaitStableFrameClient struct {
	rawFrames []fakeWaitStableFrame
	rawCalls  int
	jpegData  []byte
	jpegCalls int
	jpegMeta  frameMetadata
}

func (c *fakeWaitStableFrameClient) LatestFrameWithFormat(format string, quality int, _ bool, _ screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	if format != "jpeg" {
		return nil, nil, screenCaptureInfo{}, fmt.Errorf("unexpected format %q", format)
	}
	if quality != screenshotJPEGQuality {
		return nil, nil, screenCaptureInfo{}, fmt.Errorf("quality = %d, want %d", quality, screenshotJPEGQuality)
	}
	if len(c.rawFrames) > 0 && (len(c.jpegData) == 0 || c.rawCalls < len(c.rawFrames)) {
		index := c.rawCalls
		if index >= len(c.rawFrames) {
			index = len(c.rawFrames) - 1
		}
		c.rawCalls++
		frame := c.rawFrames[index]
		meta := frame.meta
		return &meta, append([]byte(nil), frame.data...), screenCaptureInfo{}, nil
	}
	c.jpegCalls++
	meta := c.jpegMeta
	if meta.Width == 0 {
		meta = frameMetadata{
			Seq:         99,
			Width:       2,
			Height:      2,
			PixelFormat: "jpeg",
			Bytes:       uint64(len(c.jpegData)),
		}
	}
	return &meta, append([]byte(nil), c.jpegData...), screenCaptureInfo{}, nil
}

type fakeInternalWaitTool struct {
	result    waitStableScreenResult
	err       error
	callCount int
	waitCount int
	onWait    func()
}

func (t *fakeInternalWaitTool) Name() string { return "wait_for_stable_screen" }

func (t *fakeInternalWaitTool) Description() string { return "fake wait" }

func (t *fakeInternalWaitTool) Call(context.Context, string) (string, error) {
	t.callCount++
	return "error: Call should not be used by post-action wrapper", nil
}

func (t *fakeInternalWaitTool) wait(context.Context, string) (waitStableScreenResult, error) {
	t.waitCount++
	if t.onWait != nil {
		t.onWait()
	}
	return t.result, t.err
}

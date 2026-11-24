package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/color"
	"image/jpeg"
	"strconv"
	"strings"
	"testing"
)

func TestStripScreenshotDataPreservesActionOutputAndStableScreenStatus(t *testing.T) {
	content := `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ==","action_output":" completed ","screen_stable":false,"stable_wait_ms":0,"screen_changed":true,"last_diff":1.25}`

	stripped := stripScreenshotData(content)

	if strings.Contains(stripped, "ZmFrZQ==") || strings.Contains(stripped, `"data"`) {
		t.Fatalf("screenshot data was not stripped: %s", stripped)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(stripped), &result); err != nil {
		t.Fatalf("decode stripped result: %v", err)
	}
	if result.Width != 320 || result.Height != 240 || result.Format != "jpeg" || result.Size != 4 {
		t.Fatalf("screenshot metadata = %#v", result)
	}
	if result.ActionOutput != " completed " {
		t.Fatalf("ActionOutput = %q, want original output", result.ActionOutput)
	}
	if result.ScreenStable == nil || *result.ScreenStable {
		t.Fatalf("ScreenStable = %#v, want false", result.ScreenStable)
	}
	if result.StableWaitMs == nil || *result.StableWaitMs != 0 {
		t.Fatalf("StableWaitMs = %#v, want 0", result.StableWaitMs)
	}
	if result.ScreenChanged == nil || !*result.ScreenChanged {
		t.Fatalf("ScreenChanged = %#v, want true", result.ScreenChanged)
	}
	if result.LastDiff == nil || *result.LastDiff != 1.25 {
		t.Fatalf("LastDiff = %#v, want 1.25", result.LastDiff)
	}
	if strings.Contains(stripped, "gesture_marker") {
		t.Fatalf("marker metadata leaked into stripped result: %s", stripped)
	}
}

func TestParseTouchGesturePostMarker(t *testing.T) {
	for _, gestureType := range []string{"tap", "double_tap", "long_press"} {
		marker, ok := parseTouchGesturePostMarker(`{"type":"` + gestureType + `","point":{"x":125,"y":875}}`)
		if !ok || marker.Type != gestureType || marker.X != 125 || marker.Y != 875 {
			t.Fatalf("%s marker = %#v, %v", gestureType, marker, ok)
		}
	}
	for _, input := range []string{
		`{"type":"tap","point":["125","875"]}`,
		`{"type":"tap","point":{"x":"125","y":"875"}}`,
	} {
		marker, ok := parseTouchGesturePostMarker(input)
		if !ok || marker.Type != "tap" || marker.X != 125 || marker.Y != 875 {
			t.Fatalf("compatibility point marker for %s = %#v, %v", input, marker, ok)
		}
	}
	for _, input := range []string{
		`{"type":"swipe","point":{"x":125,"y":875}}`,
		`{"type":"home","point":{"x":125,"y":875}}`,
		`{"type":"tap","point":{"x":null,"y":875}}`,
		`{"type":"tap","point":{"x":125,"y":null}}`,
		`{"type":"tap","point":[null,875]}`,
		`{"type":"tap","point":[125,null]}`,
		`{"type":"tap"}`,
		`{"type":"tap","point":{"x":125}}`,
		`{"type":"tap","point":{"y":875}}`,
		`{"type":"tap","point":{"x":-1,"y":875}}`,
		`{"type":"tap","point":{"x":125,"y":1001}}`,
	} {
		if marker, ok := parseTouchGesturePostMarker(input); ok {
			t.Fatalf("unexpected marker for %s: %#v", input, marker)
		}
	}
}

func TestPostActionTouchGestureMarksReturnedScreenshot(t *testing.T) {
	raw := solidJPEG(t, 120, 80, color.RGBA{R: 24, G: 48, B: 72, A: 255})
	rawData := base64.StdEncoding.EncodeToString(raw)
	action := &stubTool{name: "touch_gesture", output: "ok"}
	screenshot := &stubTool{name: "screenshot", output: `{"width":120,"height":80,"format":"jpeg","size":` +
		strconv.Itoa(len(raw)) + `,"data":"` + rawData + `"}`}
	tool := newPostActionScreenshotTool(action, screenshot, 0)

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":250,"y":750}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	marked, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		t.Fatalf("decode marked screenshot: %v", err)
	}
	if bytes.Equal(marked, raw) {
		t.Fatal("returned screenshot did not contain the touch marker")
	}
	if result.Size != len(marked) {
		t.Fatalf("result size = %d, want marked image size %d", result.Size, len(marked))
	}
	decoded, err := jpeg.Decode(bytes.NewReader(marked))
	if err != nil {
		t.Fatalf("decode marked JPEG: %v", err)
	}
	if got := color.RGBAModel.Convert(decoded.At(30, 60)).(color.RGBA); got.R > 100 || got.G > 100 || got.B > 100 {
		t.Fatalf("marker center should remain hollow, got %#v", got)
	}
}

func TestPostActionTouchGestureDoesNotMarkSwipe(t *testing.T) {
	raw := solidJPEG(t, 120, 80, color.RGBA{R: 24, G: 48, B: 72, A: 255})
	rawData := base64.StdEncoding.EncodeToString(raw)
	action := &stubTool{name: "touch_gesture", output: "ok"}
	screenshot := &stubTool{name: "screenshot", output: `{"width":120,"height":80,"format":"jpeg","size":` +
		strconv.Itoa(len(raw)) + `,"data":"` + rawData + `"}`}
	tool := newPostActionScreenshotTool(action, screenshot, 0)

	out, err := tool.Call(context.Background(), `{"type":"swipe","point":{"x":250,"y":750}}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Data != rawData {
		t.Fatalf("swipe screenshot was marked: %q", result.Data)
	}
}

// A task stopped while the baseline screenshot is captured must not go on to
// send the action.
func TestPostActionSendsNoActionAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	action := &stubTool{name: "touch_gesture", output: "ok"}
	screenshot := &stubTool{name: "screenshot", callFn: func(context.Context, string) (string, error) {
		cancel()
		return `{"width":1,"height":1}`, nil
	}}
	tool := newPostActionScreenshotTool(action, screenshot, 0)

	if _, err := tool.Call(ctx, `{"type":"tap","point":{"x":250,"y":750}}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want context.Canceled", err)
	}
	if len(action.inputs) != 0 {
		t.Fatalf("action ran after cancel: %v", action.inputs)
	}
}

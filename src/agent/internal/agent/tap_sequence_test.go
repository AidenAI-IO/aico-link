package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
)

func tapSequenceTestFrame(fill byte, changed image.Rectangle) lumaFrame {
	return tapSequenceTestFrameSized(400, 800, fill, changed)
}

func tapSequenceTestFrameSized(width, height int, fill byte, changed image.Rectangle) lumaFrame {
	frame := lumaFrame{width: width, height: height, stride: width, y: make([]byte, width*height)}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if (image.Point{X: x, Y: y}).In(changed) {
				frame.y[y*width+x] = 255 - fill
			} else {
				frame.y[y*width+x] = fill
			}
		}
	}
	return frame
}

const tapSequenceTestBase byte = 40

func TestTapSequenceGuardAllowsSmallChangesAndStopsOnLayoutChange(t *testing.T) {
	baseline := tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{})
	// A digit-sized change in the display area is ordinary sequence progress.
	digit := tapSequenceTestFrame(tapSequenceTestBase, image.Rect(180, 100, 220, 140))
	// Most of the screen changing is a navigation, sheet, or dimmed dialog.
	dialog := tapSequenceTestFrame(tapSequenceTestBase, image.Rect(0, 100, 400, 800))

	frames := []lumaFrame{digit, dialog}
	guard := newTapSequenceGuard(func(context.Context) (lumaFrame, error) {
		frame := frames[0]
		frames = frames[1:]
		return frame, nil
	}, nil, baseline)

	if stop, reason, err := guard(context.Background(), 1); err != nil || stop {
		t.Fatalf("digit change: stop=%v reason=%q err=%v, want continue", stop, reason, err)
	}
	stop, reason, err := guard(context.Background(), 2)
	if err != nil || !stop {
		t.Fatalf("layout change: stop=%v err=%v, want stop", stop, err)
	}
	if !strings.Contains(reason, "after tap 2") || !strings.Contains(reason, "remaining taps were not sent") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestTapSequenceGuardTreatsResizedFrameAsLayoutChange(t *testing.T) {
	baseline := tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{})
	resized := tapSequenceTestFrameSized(800, 400, tapSequenceTestBase, image.Rectangle{})
	guard := newTapSequenceGuard(func(context.Context) (lumaFrame, error) { return resized, nil }, nil, baseline)
	if stop, _, err := guard(context.Background(), 1); err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop for a resized frame", stop, err)
	}
}

func TestTouchGestureToolTapSequenceStopsWhenLayoutChanges(t *testing.T) {
	provider := mnk.NewMockProvider()
	baseline := tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{})
	frames := []lumaFrame{
		baseline,
		tapSequenceTestFrame(tapSequenceTestBase, image.Rect(180, 100, 220, 140)),
		tapSequenceTestFrame(tapSequenceTestBase, image.Rect(0, 100, 400, 800)),
	}
	tool := &TouchGestureTool{mnkProvider: provider, captureFrame: func(context.Context) (lumaFrame, error) {
		frame := frames[0]
		frames = frames[1:]
		return frame, nil
	}}

	ctx, _ := WithToolError(context.Background())
	output, err := tool.Call(ctx, `{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":200,"y":200},{"x":300,"y":300},{"x":400,"y":400}],"interval_ms":80}`)
	if err != nil || ToolErrorFromContext(ctx) != nil {
		t.Fatalf("output=%q err=%v tool_error=%v", output, err, ToolErrorFromContext(ctx))
	}
	if clicks := provider.ClickCalls(); len(clicks) != 2 {
		t.Fatalf("clicks = %#v, want two taps before the layout change", clicks)
	}
	var result mnk.TapSequenceResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output %q: %v", output, err)
	}
	if result.Requested != 4 || result.Executed != 2 || !result.StoppedEarly {
		t.Fatalf("result = %#v", result)
	}
}

func TestTouchGestureToolTapSequenceSendsNothingWithoutBaseline(t *testing.T) {
	provider := mnk.NewMockProvider()
	tool := &TouchGestureTool{mnkProvider: provider, captureFrame: func(context.Context) (lumaFrame, error) {
		return lumaFrame{}, errors.New("frame service down")
	}}

	ctx, _ := WithToolError(context.Background())
	if _, err := tool.Call(ctx, `{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":200,"y":200}]}`); err != nil {
		t.Fatal(err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || !strings.Contains(te.Message, "no tap was sent") {
		t.Fatalf("tool error = %#v", te)
	}
	if clicks := provider.ClickCalls(); len(clicks) != 0 {
		t.Fatalf("clicks = %#v, want none", clicks)
	}

	// The check is opt-out: without it the sequence runs without a baseline.
	ctx, _ = WithToolError(context.Background())
	if _, err := tool.Call(ctx, `{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":200,"y":200}],"interval_ms":80,"stop_on_screen_change":false}`); err != nil || ToolErrorFromContext(ctx) != nil {
		t.Fatalf("err=%v tool_error=%v", err, ToolErrorFromContext(ctx))
	}
	if clicks := provider.ClickCalls(); len(clicks) != 2 {
		t.Fatalf("clicks = %#v, want two", clicks)
	}
}

// The phone mirror fills only a pillarboxed strip of the HDMI frame. A change
// that covers the whole phone screen must still stop the sequence, so the
// comparison is limited to the active area instead of diluted by black bars.
func TestTapSequenceGuardComparesInsidePhoneActiveArea(t *testing.T) {
	const frameW, frameH = 1920, 1080
	phone := image.Rect(730, 0, 1190, 1080)
	state := &screen.ScreenState{}
	state.UpdateActiveArea(frameW, frameH, screen.ScreenActiveArea{X: phone.Min.X, Y: phone.Min.Y, Width: phone.Dx(), Height: phone.Dy(), Valid: true})

	baseline := tapSequenceTestFrameSized(frameW, frameH, 0, image.Rectangle{})
	phoneChanged := tapSequenceTestFrameSized(frameW, frameH, 0, phone)

	if ratio, _ := tapSequenceFrameChange(baseline, phoneChanged, image.Rect(0, 0, frameW, frameH)); ratio > tapSequenceLayoutChangeRatio {
		t.Fatalf("whole-frame ratio = %.3f; the regression this test guards needs it diluted below the threshold", ratio)
	}
	guard := newTapSequenceGuard(func(context.Context) (lumaFrame, error) { return phoneChanged, nil }, state, baseline)
	if stop, _, err := guard(context.Background(), 1); err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop for a full phone-screen change", stop, err)
	}
}

func TestFrameLumaReadsRawAndEncodedFrames(t *testing.T) {
	nv12 := append(bytes.Repeat([]byte{7}, 4*2), bytes.Repeat([]byte{128}, 4)...)
	frame, err := frameLuma(&frameMetadata{Width: 4, Height: 2, Stride: 4, PixelFormat: "nv12"}, nv12)
	if err != nil || frame.width != 4 || frame.height != 2 || frame.y[7] != 7 {
		t.Fatalf("nv12 frame = %+v err=%v", frame, err)
	}
	uyvy := []byte{128, 10, 128, 20, 128, 30, 128, 40}
	frame, err = frameLuma(&frameMetadata{Width: 2, Height: 2, PixelFormat: "uyvy"}, uyvy)
	if err != nil || !bytes.Equal(frame.y, []byte{10, 20, 30, 40}) {
		t.Fatalf("uyvy frame = %+v err=%v", frame, err)
	}
	var encoded bytes.Buffer
	img := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 200
	}
	if err := jpeg.Encode(&encoded, img, nil); err != nil {
		t.Fatal(err)
	}
	frame, err = frameLuma(&frameMetadata{Width: 8, Height: 8, PixelFormat: "jpeg"}, encoded.Bytes())
	if err != nil || frame.width != 8 || frame.y[0] < 190 {
		t.Fatalf("jpeg frame = %+v err=%v", frame, err)
	}
	if _, err := frameLuma(&frameMetadata{Width: 4, Height: 2, Stride: 4, PixelFormat: "nv12"}, nv12[:3]); err == nil {
		t.Fatal("short nv12 frame was accepted")
	}
}

func TestValidateTouchGestureFieldsTapSequence(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`{"type":"tap_sequence","points":[{"x":1,"y":2},{"x":3,"y":4}]}`, ""},
		{`{"type":"tap_sequence"}`, "points is required for tap_sequence"},
		{`{"type":"tap_sequence","points":{"x":1,"y":2}}`, "points must be an array"},
		{`{"type":"tap_sequence","points":[{"x":1,"y":2},{"x":3}]}`, "points[1] must be a JSON object"},
	} {
		err := validateTouchGestureFields(tc.input)
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.input, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "no touch action was executed") {
			t.Fatalf("%s: err = %v, want %q", tc.input, err, tc.want)
		}
	}
}

func TestParseTapSequencePostMarkersOnlyMarksExecutedTaps(t *testing.T) {
	input := `{"type":"tap_sequence","points":[{"x":100,"y":200},{"x":300,"y":400},{"x":500,"y":600}]}`
	markers := parseTapSequencePostMarkers(input, `{"requested":3,"executed":2,"stopped_early":true}`)
	if len(markers) != 2 || markers[0].X != 100 || markers[1].Y != 400 {
		t.Fatalf("markers = %#v, want the two executed taps", markers)
	}
	if markers := parseTapSequencePostMarkers(`{"type":"tap","point":{"x":1,"y":1}}`, "ok"); markers != nil {
		t.Fatalf("plain tap produced sequence markers: %#v", markers)
	}
}

func TestVisualCoordinatesConvertTapSequencePoints(t *testing.T) {
	args := map[string]any{}
	if err := json.Unmarshal([]byte(`{"type":"tap_sequence","points":[{"x":0,"y":0},{"x":100,"y":200}]}`), &args); err != nil {
		t.Fatal(err)
	}
	if err := convertVisualArguments(args, visualFrame{width: 101, height: 201}); err != nil {
		t.Fatal(err)
	}
	last := args["points"].([]any)[1].(map[string]any)
	if last["x"] != float64(1000) || last["y"] != float64(1000) {
		t.Fatalf("converted point = %#v", last)
	}
}

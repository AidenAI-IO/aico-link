package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// tapSequenceKeypadFrame draws a bright glyph at each key of a 3x4 keypad on a
// dark screen, offset by (dx, dy), so shifts and covers change real content.
func tapSequenceKeypadFrame(dx, dy int, covered ...image.Rectangle) lumaFrame {
	frame := tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{})
	for _, key := range tapSequenceTestKeys {
		cx, cy := int(key.X/1000*399)+dx, int(key.Y/1000*799)+dy
		for y := cy - 6; y <= cy+6; y++ {
			for x := cx - 3; x <= cx+3; x++ {
				frame.y[y*frame.stride+x] = 230
			}
		}
	}
	for _, rect := range covered {
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				frame.y[y*frame.stride+x] = 120
			}
		}
	}
	return frame
}

var tapSequenceTestKeys = []normalizedPoint{
	{X: 250, Y: 450}, {X: 500, Y: 450}, {X: 750, Y: 450},
	{X: 250, Y: 600}, {X: 500, Y: 600}, {X: 750, Y: 600},
	{X: 250, Y: 750}, {X: 500, Y: 750}, {X: 750, Y: 750},
}

const tapSequenceTestBase byte = 40

func tapSequenceFrames(frames ...lumaFrame) frameCapturer {
	return func(context.Context) (lumaFrame, error) {
		frame := frames[0]
		frames = frames[1:]
		return frame, nil
	}
}

// tapSequenceTestPoints taps keys 1, 2, 2, 6, 9 of tapSequenceTestKeys.
var tapSequenceTestPoints = []normalizedPoint{tapSequenceTestKeys[0], tapSequenceTestKeys[1], tapSequenceTestKeys[1], tapSequenceTestKeys[5], tapSequenceTestKeys[8]}

// tapSequencePressed brightens rect by 15, about how much the pointer and
// press highlight change a just-tapped key on the board's raw frames.
func tapSequencePressed(frame lumaFrame, rect image.Rectangle) lumaFrame {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			frame.y[y*frame.stride+x] = byte(min(255, int(frame.y[y*frame.stride+x])+15))
		}
	}
	return frame
}

func TestTapSequenceGuardIgnoresChangesAwayFromRemainingTargets(t *testing.T) {
	baseline := tapSequenceKeypadFrame(0, 0)
	// The number display filling a third of the screen and the just-tapped key
	// lighting up are both expected while entering digits; the pressed key is
	// tapped again next, so it is still compared.
	display := image.Rect(0, 80, 400, 330)
	frame := tapSequencePressed(tapSequenceKeypadFrame(0, 0, display), image.Rect(180, 340, 220, 380))
	guard := newTapSequenceGuard(tapSequenceFrames(frame), nil, baseline, tapSequenceTestPoints, nil)
	if stop, reason, err := guard(context.Background(), 2); err != nil || stop {
		t.Fatalf("stop=%v reason=%q err=%v, want continue", stop, reason, err)
	}
}

// Pressing one key several times leaves no other target to watch, so the
// repeated key itself must be checked from the first tap on.
func TestTapSequenceGuardChecksARepeatedKeyFromTheFirstTap(t *testing.T) {
	baseline := tapSequenceKeypadFrame(0, 0)
	key := tapSequenceTestKeys[4]
	points := []normalizedPoint{key, key, key, key}
	popup := image.Rect(170, 440, 230, 510)
	guard := newTapSequenceGuard(tapSequenceFrames(tapSequenceKeypadFrame(0, 0, popup)), nil, baseline, points, nil)
	stop, reason, err := guard(context.Background(), 1)
	if err != nil || !stop || !strings.Contains(reason, "target 2 changed after tap 1") {
		t.Fatalf("stop=%v reason=%q err=%v, want the repeated key checked after the first tap", stop, reason, err)
	}
}

func TestTapSequenceGuardStopsWhenARemainingTargetIsCovered(t *testing.T) {
	baseline := tapSequenceKeypadFrame(0, 0)
	// A small menu over one key is far below any whole-screen ratio.
	menu := image.Rect(270, 460, 330, 500)
	guard := newTapSequenceGuard(tapSequenceFrames(tapSequenceKeypadFrame(0, 0, menu)), nil, baseline, tapSequenceTestPoints, nil)
	stop, reason, err := guard(context.Background(), 2)
	if err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop", stop, err)
	}
	if !strings.Contains(reason, "target 4 changed after tap 2") || !strings.Contains(reason, "remaining taps were not sent") {
		t.Fatalf("reason = %q", reason)
	}
}

// A target change the pixel check sees goes to the decision model with the
// baseline and the changed frame. A pass continues only when a fresh frame
// still matches the judged one, and that frame becomes the new baseline;
// anything else stops, and a whole-screen change never asks.
func TestTapSequenceGuardConfirmerDecidesTargetChanges(t *testing.T) {
	menu := image.Rect(270, 460, 330, 500)
	sheet := image.Rect(0, 0, 400, 500)
	keypad := tapSequenceKeypadFrame(0, 0)
	dimmed := tapSequenceKeypadFrame(0, 0, menu)
	cases := []struct {
		name      string
		frames    []lumaFrame
		points    []normalizedPoint
		answer    stepGateResult
		wantStops []bool
		wantAsks  int
	}{
		{
			name: "confirmed change continues and becomes the baseline",
			// Check 1 sees the change and rechecks a fresh frame; check 2
			// sees the same accepted change and must not ask again.
			frames:    []lumaFrame{dimmed, dimmed, dimmed},
			points:    tapSequenceTestPoints,
			answer:    stepGateResult{Checked: true, Pass: true},
			wantStops: []bool{false, false},
			wantAsks:  1,
		},
		{name: "screen moving during the check stops", frames: []lumaFrame{dimmed, keypad}, points: tapSequenceTestPoints,
			answer: stepGateResult{Checked: true, Pass: true}, wantStops: []bool{true}, wantAsks: 1},
		{name: "rejected change stops", frames: []lumaFrame{dimmed}, points: tapSequenceTestPoints,
			answer: stepGateResult{Checked: true}, wantStops: []bool{true}, wantAsks: 1},
		{name: "unchecked change stops", frames: []lumaFrame{dimmed}, points: tapSequenceTestPoints,
			wantStops: []bool{true}, wantAsks: 1},
		{name: "whole-screen change never asks", frames: []lumaFrame{tapSequenceKeypadFrame(0, 0, sheet)},
			points: []normalizedPoint{tapSequenceTestKeys[0], tapSequenceTestKeys[8]}, answer: stepGateResult{Checked: true, Pass: true},
			wantStops: []bool{true}, wantAsks: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asks := 0
			confirm := func(_ context.Context, before, after lumaFrame, _ image.Rectangle, _ []normalizedPoint) stepGateResult {
				asks++
				if &before.y[0] != &keypad.y[0] || &after.y[0] != &dimmed.y[0] {
					t.Fatalf("confirmer did not get the baseline and the changed frame")
				}
				return tc.answer
			}
			guard := newTapSequenceGuard(tapSequenceFrames(tc.frames...), nil, keypad, tc.points, confirm)
			for i, want := range tc.wantStops {
				if stop, reason, err := guard(context.Background(), 1+i); err != nil || stop != want {
					t.Fatalf("check %d: stop=%v reason=%q err=%v, want stop=%v", i+1, stop, reason, err, want)
				}
			}
			if asks != tc.wantAsks {
				t.Fatalf("decision model asked %d times, want %d", asks, tc.wantAsks)
			}
		})
	}
}

// The tap_sequence check sends the decision model both frames and passes only
// on a confident "yes"; a slow decision model is cut off and leaves the stop.
func TestTapSequenceConfirmerAsksWithBothFrames(t *testing.T) {
	keypad := tapSequenceKeypadFrame(0, 0)
	dimmed := tapSequenceKeypadFrame(0, 0, image.Rect(270, 460, 330, 500))
	region := image.Rect(0, 0, keypad.width, keypad.height)
	for _, tc := range []struct {
		name     string
		noul     float64
		wantPass bool
	}{
		{name: "same controls", noul: 0.95, wantPass: true},
		{name: "different controls", noul: 0.05},
		{name: "unsure", noul: 0.6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newDecisionServer(t, fmt.Sprintf(`{"answers":{"met":{"type":"noul","noul":%g}}}`, tc.noul))
			confirm := newTapSequenceConfirmer(testDecisionAdvisor(server))
			got := confirm(context.Background(), keypad, dimmed, region, tapSequenceTestPoints[2:])
			if got.Pass != tc.wantPass || !got.Checked {
				t.Fatalf("result = %+v, want pass=%v", got, tc.wantPass)
			}
			server.mu.Lock()
			assets, _ := server.requests[0]["assets"].(map[string]any)
			server.mu.Unlock()
			if len(assets) != 2 {
				t.Fatalf("request carried %d images, want the baseline and the current frame", len(assets))
			}
		})
	}

	previous := stepGateTimeout
	stepGateTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stepGateTimeout = previous })
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	advisor := &decisionAdvisor{}
	advisor.setConfig(func() DecisionModelConfig {
		return DecisionModelConfig{Enabled: true, APIKey: "ts-key", BaseURL: slow.URL}
	})
	started := time.Now()
	got := newTapSequenceConfirmer(advisor)(context.Background(), keypad, dimmed, region, tapSequenceTestPoints[2:])
	if got.Pass || got.Checked || time.Since(started) > 2*time.Second {
		t.Fatalf("slow decision model: result=%+v after %v, want unchecked within the timeout", got, time.Since(started))
	}
}

func TestTapSequenceGuardToleratesOnePixelLayoutNudge(t *testing.T) {
	baseline := tapSequenceKeypadFrame(0, 0)
	guard := newTapSequenceGuard(tapSequenceFrames(tapSequenceKeypadFrame(1, 2)), nil, baseline, tapSequenceTestPoints, nil)
	if stop, reason, err := guard(context.Background(), 1); err != nil || stop {
		t.Fatalf("stop=%v reason=%q err=%v, want continue", stop, reason, err)
	}
	guard = newTapSequenceGuard(tapSequenceFrames(tapSequenceKeypadFrame(0, 12)), nil, baseline, tapSequenceTestPoints, nil)
	if stop, _, err := guard(context.Background(), 1); err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop when keys moved", stop, err)
	}
}

func TestTapSequenceGuardStopsWhenMostOfTheScreenChanges(t *testing.T) {
	baseline := tapSequenceKeypadFrame(0, 0)
	// Remaining targets sit below the changed area, so only the backstop sees it.
	sheet := image.Rect(0, 0, 400, 500)
	guard := newTapSequenceGuard(tapSequenceFrames(tapSequenceKeypadFrame(0, 0, sheet)), nil, baseline, []normalizedPoint{tapSequenceTestKeys[0], tapSequenceTestKeys[8]}, nil)
	stop, reason, err := guard(context.Background(), 1)
	if err != nil || !stop || !strings.Contains(reason, "screen layout changed after tap 1") {
		t.Fatalf("stop=%v reason=%q err=%v, want backstop stop", stop, reason, err)
	}
}

func TestTapSequenceGuardTreatsResizedFrameAsLayoutChange(t *testing.T) {
	baseline := tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{})
	resized := tapSequenceTestFrameSized(800, 400, tapSequenceTestBase, image.Rectangle{})
	guard := newTapSequenceGuard(tapSequenceFrames(resized), nil, baseline, tapSequenceTestPoints, nil)
	if stop, _, err := guard(context.Background(), 1); err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop for a resized frame", stop, err)
	}
}

// The phone mirror fills only a pillarboxed strip of the HDMI frame. Targets
// are normalized to the phone screen, so their patches must be located inside
// the active area, and a whole-phone change must not be diluted by black bars.
func TestTapSequenceGuardComparesInsidePhoneActiveArea(t *testing.T) {
	const frameW, frameH = 1920, 1080
	phone := image.Rect(100, 0, 560, 1080)
	state := &screen.ScreenState{}
	state.UpdateActiveArea(frameW, frameH, screen.ScreenActiveArea{X: phone.Min.X, Y: phone.Min.Y, Width: phone.Dx(), Height: phone.Dy(), Valid: true})
	baseline := tapSequenceTestFrameSized(frameW, frameH, 0, image.Rectangle{})
	points := []normalizedPoint{{X: 100, Y: 100}, {X: 500, Y: 500}}

	// Point (500,500) of the phone is frame pixel (330,539), not the frame center.
	covered := tapSequenceTestFrameSized(frameW, frameH, 0, image.Rect(310, 520, 350, 560))
	guard := newTapSequenceGuard(tapSequenceFrames(covered), state, baseline, points, nil)
	if stop, reason, err := guard(context.Background(), 1); err != nil || !stop || !strings.Contains(reason, "target 2") {
		t.Fatalf("stop=%v reason=%q err=%v, want the target located inside the phone", stop, reason, err)
	}

	if ratio, _ := tapSequenceFrameChange(baseline, tapSequenceTestFrameSized(frameW, frameH, 0, phone), image.Rect(0, 0, frameW, frameH)); ratio > tapSequenceLayoutChangeRatio {
		t.Fatalf("whole-frame ratio = %.3f; the regression this test guards needs it diluted below the threshold", ratio)
	}
	guard = newTapSequenceGuard(tapSequenceFrames(tapSequenceTestFrameSized(frameW, frameH, 0, phone)), state, baseline, []normalizedPoint{{X: 100, Y: 100}, {X: 120, Y: 120}}, nil)
	if stop, _, err := guard(context.Background(), 1); err != nil || !stop {
		t.Fatalf("stop=%v err=%v, want stop for a full phone-screen change", stop, err)
	}
}

func TestTouchGestureToolTapSequenceStopsWhenATargetIsCovered(t *testing.T) {
	for _, touchscreen := range []bool{false, true} {
		provider := mnk.NewMockProvider()
		menu := image.Rect(270, 460, 330, 500)
		tool := &TouchGestureTool{mnkProvider: provider, touchscreen: touchscreen, captureFrame: tapSequenceFrames(
			tapSequenceKeypadFrame(0, 0),
			tapSequenceKeypadFrame(0, 0),
			tapSequenceKeypadFrame(0, 0, menu),
		)}

		ctx, _ := WithToolError(context.Background())
		output, err := tool.Call(ctx, `{"type":"tap_sequence","points":[{"x":250,"y":450},{"x":500,"y":450},{"x":500,"y":450},{"x":750,"y":600}],"interval_ms":80}`)
		if err != nil || ToolErrorFromContext(ctx) != nil {
			t.Fatalf("output=%q err=%v tool_error=%v", output, err, ToolErrorFromContext(ctx))
		}
		if clicks := provider.ClickCalls(); len(clicks) != 2 {
			t.Fatalf("touchscreen=%v clicks = %#v, want two taps before the menu covered a target", touchscreen, clicks)
		}
		var result mnk.TapSequenceResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("output %q: %v", output, err)
		}
		if result.Requested != 4 || result.Executed != 2 || !result.StoppedEarly {
			t.Fatalf("result = %#v", result)
		}
		// Absolute pointers are parked on the first target before the baseline;
		// a touchscreen has no pointer to park.
		moves := provider.MoveCalls()
		if touchscreen && len(moves) != 0 || !touchscreen && (len(moves) != 1 || moves[0].X != 250 || moves[0].Y != 450) {
			t.Fatalf("touchscreen=%v moves = %#v", touchscreen, moves)
		}
	}
}

func TestTouchGestureToolTapSequenceRejectsInvalidInputBeforeTouchingTheDevice(t *testing.T) {
	for _, input := range []string{
		`{"type":"tap_sequence","points":[{"x":100,"y":100}]}`,
		`{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":200,"y":200}],"interval_ms":10}`,
		`{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":2000,"y":200}]}`,
	} {
		provider := mnk.NewMockProvider()
		captures := 0
		tool := &TouchGestureTool{mnkProvider: provider, captureFrame: func(context.Context) (lumaFrame, error) {
			captures++
			return tapSequenceTestFrame(tapSequenceTestBase, image.Rectangle{}), nil
		}}
		ctx, _ := WithToolError(context.Background())
		if _, err := tool.Call(ctx, input); err != nil {
			t.Fatal(err)
		}
		if te := ToolErrorFromContext(ctx); te == nil || te.Code != CodeInvalidArguments {
			t.Fatalf("%s: tool error = %#v, want invalid_arguments", input, te)
		}
		if moves, clicks := provider.MoveCalls(), provider.ClickCalls(); len(moves) != 0 || len(clicks) != 0 || captures != 0 {
			t.Fatalf("%s: moves=%d clicks=%d captures=%d, want no device operation", input, len(moves), len(clicks), captures)
		}
	}
}

func TestTouchGestureToolTapSequenceSendsNothingWithoutBaseline(t *testing.T) {
	provider := mnk.NewMockProvider()
	tool := &TouchGestureTool{mnkProvider: provider, touchscreen: true, captureFrame: func(context.Context) (lumaFrame, error) {
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

	// Asking to skip the check does not bypass it.
	ctx, _ = WithToolError(context.Background())
	if _, err := tool.Call(ctx, `{"type":"tap_sequence","points":[{"x":100,"y":100},{"x":200,"y":200}],"interval_ms":80,"stop_on_screen_change":false}`); err != nil {
		t.Fatal(err)
	}
	if te := ToolErrorFromContext(ctx); te == nil || !strings.Contains(te.Message, "no tap was sent") {
		t.Fatalf("tool error = %#v, want the missing baseline to block the sequence", te)
	}
	if clicks := provider.ClickCalls(); len(clicks) != 0 {
		t.Fatalf("clicks = %#v, want none", clicks)
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

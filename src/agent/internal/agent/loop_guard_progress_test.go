package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
)

// progressTestObservation is a 200x400 screenshot observation with each rect
// painted in a contrasting color.
func progressTestObservation(t *testing.T, rects ...image.Rectangle) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 200, 400))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 32, G: 48, B: 64, A: 255}}, image.Point{}, draw.Src)
	for _, rect := range rects {
		draw.Draw(img, rect.Intersect(img.Bounds()), &image.Uniform{C: color.RGBA{R: 240, G: 224, B: 208, A: 255}}, image.Point{}, draw.Src)
	}
	var data bytes.Buffer
	if err := jpeg.Encode(&data, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(screenshotResult{Width: 200, Height: 400, Format: "jpeg", Size: data.Len(), Data: base64.StdEncoding.EncodeToString(data.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func runPostActionWithScreens(t *testing.T, pointer *pointerTracker, input, before, after string) postActionScreenshotResult {
	t.Helper()
	return runPostActionToolWithScreens(t, "touch_gesture", pointer, input, before, after)
}

func runPostActionToolWithScreens(t *testing.T, toolName string, pointer *pointerTracker, input, before, after string) postActionScreenshotResult {
	t.Helper()
	screenshot := &stubTool{name: "screenshot"}
	screenshot.callFn = func(context.Context, string) (string, error) {
		if len(screenshot.inputs) == 1 {
			return before, nil
		}
		return after, nil
	}
	tool := newPostActionScreenshotTool(&stubTool{name: toolName, output: "ok"}, screenshot, 0).(*postActionScreenshotTool).withPointerTracker(pointer)
	out, err := tool.Call(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || result.ScreenChanged == nil {
		t.Fatalf("result = %s err=%v, want screen_changed", out, err)
	}
	return result
}

// An absolute pointer leaving one spot and arriving at another changes the
// screen even when nothing was hit; that alone is not progress. The squares
// are larger than the real pointer so that, untracked, they exceed the
// progress threshold.
func TestPostActionScreenChangedIgnoresAbsolutePointerMovement(t *testing.T) {
	before := progressTestObservation(t, image.Rect(40, 90, 60, 110))   // pointer at (250,250)
	after := progressTestObservation(t, image.Rect(139, 229, 159, 249)) // pointer at (750,600)
	move := `{"x":750,"y":600}`

	if result := runPostActionToolWithScreens(t, "mouse_move", nil, move, before, after); !*result.ScreenChanged {
		t.Fatal("without pointer tracking the fixture must exceed the progress threshold")
	}

	pointer := &pointerTracker{}
	pointer.set(&normalizedPoint{X: 250, Y: 250})
	if result := runPostActionToolWithScreens(t, "mouse_move", pointer, move, before, after); *result.ScreenChanged {
		t.Fatal("pointer movement alone was reported as a screen change")
	}
	if last := pointer.current(); last == nil || last.X != 750 || last.Y != 600 {
		t.Fatalf("pointer = %#v, want the move target", last)
	}

	// A change away from the pointer is still progress.
	changed := progressTestObservation(t, image.Rect(139, 229, 159, 249), image.Rect(20, 300, 180, 340))
	if result := runPostActionWithScreens(t, pointer, `{"type":"tap","point":{"x":750,"y":600}}`, after, changed); !*result.ScreenChanged {
		t.Fatal("a change outside the pointer areas was not reported")
	}
}

// Feedback drawn around the touch point (a selection ring, a placed piece,
// a confirm mark) falls inside the pointer exclusion square. The pointer disc
// itself stays near the centre, so a tap is still credited when the ring
// around it changes, while the pointer arriving alone is not.
func TestPostActionScreenChangedSeesTapFeedbackAtThePointer(t *testing.T) {
	// 200x400 fixture: the tap at (500,500) lands on pixel (100,200); the
	// ring is 6 to 10 pixels out, the pointer disc fits inside 5.
	pointerDisc := image.Rect(96, 196, 105, 205)
	feedback := image.Rect(90, 190, 111, 211)
	tap := `{"type":"tap","point":{"x":500,"y":500}}`

	pointer := &pointerTracker{}
	pointer.set(&normalizedPoint{X: 500, Y: 500})
	before := progressTestObservation(t, pointerDisc)
	if result := runPostActionWithScreens(t, pointer, tap, before, progressTestObservation(t, pointerDisc)); *result.ScreenChanged {
		t.Fatal("a tap that changed nothing was reported as a screen change")
	}

	pointer.set(&normalizedPoint{X: 250, Y: 250})
	if result := runPostActionWithScreens(t, pointer, tap, progressTestObservation(t), before); *result.ScreenChanged {
		t.Fatal("the pointer arriving at the tap point was reported as a screen change")
	}

	pointer.set(&normalizedPoint{X: 500, Y: 500})
	if result := runPostActionWithScreens(t, pointer, tap, before, progressTestObservation(t, feedback)); !*result.ScreenChanged {
		t.Fatal("feedback around the tap point was not reported")
	}

	// mouse_move touches nothing, and a swipe's feedback is not centred on
	// one point, so neither gets the tap-point check.
	pointer.set(&normalizedPoint{X: 500, Y: 500})
	if result := runPostActionWithScreens(t, pointer, `{"type":"swipe","start":{"x":500,"y":500},"end":{"x":500,"y":500}}`, before, progressTestObservation(t, feedback)); *result.ScreenChanged {
		t.Fatal("a swipe was credited by the tap-point check")
	}
}

func TestPointerFinalPointFollowsPointerActions(t *testing.T) {
	for _, tc := range []struct {
		tool, input, output string
		want                *normalizedPoint
	}{
		{"touch_gesture", `{"type":"long_press","point":{"x":10,"y":20}}`, "", &normalizedPoint{X: 10, Y: 20}},
		{"touch_gesture", `{"type":"swipe","start":{"x":500,"y":900},"end":{"x":500,"y":300}}`, "", &normalizedPoint{X: 500, Y: 300}},
		{"touch_gesture", `{"type":"swipe","start":{"x":500,"y":900},"direction":"up"}`, "", &normalizedPoint{X: 500, Y: 0}},
		{"touch_gesture", `{"type":"swipe","start":{"x":500,"y":900},"direction":"left","duration_ms":200}`, "", &normalizedPoint{X: 0, Y: 900}},
		{"touch_gesture", `{"type":"drag_start","point":{"x":400,"y":300}}`, "", &normalizedPoint{X: 400, Y: 500}},
		{"mouse_move", `{"x":30,"y":40}`, "", &normalizedPoint{X: 30, Y: 40}},
		{"touch_gesture", `{"actions":[{"action":"touch_down","point":{"x":1,"y":1}},{"action":"touch_up"}]}`, "", nil},
		{"touch_gesture", `{"type":"tap_sequence","points":[{"x":1,"y":2},{"x":3,"y":4},{"x":5,"y":6}]}`, `{"requested":3,"executed":2,"stopped_early":true}`, &normalizedPoint{X: 3, Y: 4}},
	} {
		got := pointerFinalPoint(tc.tool, tc.input, tc.output)
		if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
			t.Fatalf("%s %s: got %#v, want %#v", tc.tool, tc.input, got, tc.want)
		}
	}
}

// The guard only catches obvious loops: the same UI action repeated while the
// screen stays identical. Different actions on an unchanged screen, such as
// placing pieces or filling a form whose small changes a block grid can miss,
// are left to the model.
func TestTerminationPolicyIgnoresDifferentActionsOnUnchangedScreen(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	for i := 0; i < 20; i++ {
		input := fmt.Sprintf(`{"type":"tap","point":{"x":%d,"y":500}}`, 100+i*10)
		if decision := policy.AfterToolCall("touch_gesture", input, screen, false); decision.Stop || decision.Notice != "" {
			t.Fatalf("tap %d: decision=%#v, want no intervention", i+1, decision)
		}
		if _, allowed := policy.BeforeToolCall("touch_gesture", input); !allowed {
			t.Fatalf("tap %d was blocked", i+1)
		}
	}
}

func TestTerminationPolicyStopsIdenticalUIActionOnIdenticalScreen(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	tap := `{"type":"tap","point":{"x":500,"y":500}}`
	if decision := policy.AfterToolCall("touch_gesture", tap, screen, false); decision.Stop || decision.Notice != "" {
		t.Fatalf("first tap: %#v", decision)
	}
	// Observations and other tools between the repeats do not hide the loop.
	policy.AfterToolCall("screenshot", `{}`, screen, false)
	policy.AfterToolCall("shell", `{"command":"sleep 5; echo ok"}`, "ok", false)
	decision := policy.AfterToolCall("touch_gesture", tap, screen, false)
	if decision.Stop || decision.Notice == "" {
		t.Fatalf("second identical tap: %#v, want a notice", decision)
	}
	policy.AfterToolCall("wait_for_stable_screen", `{}`, screen, false)
	decision = policy.AfterToolCall("touch_gesture", tap, screen, false)
	if !decision.Stop || decision.Reason != StopReasonLoopDetected {
		t.Fatalf("third identical tap: %#v, want a loop stop", decision)
	}
}

// A repeated action that keeps changing the screen (incrementing a counter,
// deleting characters) is not a loop.
func TestTerminationPolicyAllowsRepeatedActionThatChangesScreen(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	tap := `{"type":"tap","point":{"x":500,"y":500}}`
	for i := 0; i < 10; i++ {
		screen := progressTestObservation(t, image.Rect(10, 100, 20+i*8, 110))
		if decision := policy.AfterToolCall("touch_gesture", tap, screen, false); decision.Stop || decision.Notice != "" {
			t.Fatalf("tap %d: decision=%#v, want no intervention", i+1, decision)
		}
	}
}

// The guard never blocks UI tools, so a model can always act on what it sees.
func TestTerminationPolicyNeverRestrictsUITools(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	for i := 0; i < 5; i++ {
		policy.AfterToolCall("screenshot", `{}`, screen, false)
	}
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":1,"y":1}}`); !allowed {
		t.Fatal("a UI action was blocked")
	}
}

// Identical screenshots repeated with nothing in between are still a loop.
func TestTerminationPolicyStopsRepeatedIdenticalObservations(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	var decision TerminationDecision
	for i := 0; i < 10 && !decision.Stop; i++ {
		decision = policy.AfterToolCall("screenshot", `{}`, screen, false)
	}
	if !decision.Stop {
		t.Fatal("repeated identical screenshots never stopped the run")
	}
	fresh := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	for i := 0; i < 10; i++ {
		changed := progressTestObservation(t, image.Rect(10, 100, 20+i*8, 110))
		if d := fresh.AfterToolCall("screenshot", `{}`, changed, false); d.Stop {
			t.Fatalf("screenshot %d of a changing screen stopped the run", i+1)
		}
	}
}

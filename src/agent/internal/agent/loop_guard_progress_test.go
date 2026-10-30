package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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
	screenshot := &stubTool{name: "screenshot"}
	screenshot.callFn = func(context.Context, string) (string, error) {
		if len(screenshot.inputs) == 1 {
			return before, nil
		}
		return after, nil
	}
	tool := newPostActionScreenshotTool(&stubTool{name: "touch_gesture", output: "ok"}, screenshot, 0).(*postActionScreenshotTool).withPointerTracker(pointer)
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
// screen even when the tap hit nothing; that alone is not progress.
func TestPostActionScreenChangedIgnoresAbsolutePointerMovement(t *testing.T) {
	before := progressTestObservation(t, image.Rect(40, 90, 60, 110))   // pointer at (250,250)
	after := progressTestObservation(t, image.Rect(139, 229, 159, 249)) // pointer at (750,600)
	tap := `{"type":"tap","point":{"x":750,"y":600}}`

	if result := runPostActionWithScreens(t, nil, tap, before, after); !*result.ScreenChanged {
		t.Fatal("without pointer tracking the fixture must exceed the progress threshold")
	}

	pointer := &pointerTracker{}
	pointer.set(&normalizedPoint{X: 250, Y: 250})
	if result := runPostActionWithScreens(t, pointer, tap, before, after); *result.ScreenChanged {
		t.Fatal("pointer movement alone was reported as a screen change")
	}
	if last := pointer.current(); last == nil || last.X != 750 || last.Y != 600 {
		t.Fatalf("pointer = %#v, want the tap point", last)
	}

	// A change away from the pointer is still progress.
	changed := progressTestObservation(t, image.Rect(139, 229, 159, 249), image.Rect(20, 300, 180, 340))
	if result := runPostActionWithScreens(t, pointer, `{"type":"tap","point":{"x":750,"y":600}}`, after, changed); !*result.ScreenChanged {
		t.Fatal("a change outside the pointer areas was not reported")
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

// Repeating a tap whose effect is too small to register (one more digit in a
// field) must not restrict actions on the second attempt: screenshot results
// share one hash, so "same result" would only repeat the signature check.
func TestTerminationPolicyRepeatedTapWithoutVisibleChangeOnlyNotices(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	first := progressTestObservation(t)
	second := progressTestObservation(t, image.Rect(20, 100, 120, 200))
	policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":250,"y":320}}`, first, false)
	policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":750,"y":320}}`, second, false)
	decision := policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":750,"y":320}}`, second, false)

	if decision.Stop || policy.tier != TierSoftNotice {
		t.Fatalf("decision=%#v tier=%d score=%d, want a soft notice", decision, policy.tier, policy.stallScore)
	}
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":250,"y":440}}`); !allowed {
		t.Fatal("next action was blocked after one unchanged repeat")
	}
}

func restrictWithUnchangedActions(t *testing.T, policy *TerminationPolicy, screen string) {
	t.Helper()
	for _, direction := range []string{"up", "down", "left", "right"} {
		if policy.tier >= TierRestrictTools {
			return
		}
		policy.AfterToolCall("touch_gesture", `{"type":"swipe","start":{"x":500,"y":500},"direction":"`+direction+`"}`, screen, false)
	}
	if policy.tier != TierRestrictTools {
		t.Fatalf("tier = %d score=%d, want restrict tier", policy.tier, policy.stallScore)
	}
}

// The restriction tells the model to diagnose with a fresh observation; doing
// so allows one more action, and a second stall without progress keeps the
// restriction until something changes.
func TestTerminationPolicyObservationAllowsOneActionUnderRestriction(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	restrictWithUnchangedActions(t, policy, screen)
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":1,"y":1}}`); allowed {
		t.Fatal("restricted policy allowed an action")
	}

	if decision := policy.AfterToolCall("screenshot", `{}`, screen, false); decision.Stop {
		t.Fatalf("observation stopped the run: %#v", decision)
	}
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":1,"y":1}}`); !allowed {
		t.Fatal("action still blocked after a fresh observation")
	}

	policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":1,"y":1}}`, screen, false)
	policy.AfterToolCall("screenshot", `{}`, screen, false)
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":2,"y":2}}`); allowed {
		t.Fatal("a second observation lifted the restriction again without progress")
	}

	// Progress restores the allowance for a later stall.
	policy.AfterToolCall("screenshot", `{}`, progressTestObservation(t, image.Rect(20, 100, 120, 200)), false)
	restrictWithUnchangedActions(t, policy, progressTestObservation(t, image.Rect(20, 100, 120, 200)))
	policy.AfterToolCall("screenshot", `{}`, progressTestObservation(t, image.Rect(20, 100, 120, 200)), false)
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":3,"y":3}}`); !allowed {
		t.Fatal("observation did not lift a restriction after progress reset it")
	}
}

func TestTerminationPolicyPrefersReportedScreenChanged(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	restrictWithUnchangedActions(t, policy, screen)

	var payload map[string]any
	if err := json.Unmarshal([]byte(screen), &payload); err != nil {
		t.Fatal(err)
	}
	payload["screen_changed"] = true
	reported, _ := json.Marshal(payload)
	policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":9,"y":9}}`, string(reported), false)
	if policy.tier != TierNone || policy.stallScore != 0 {
		t.Fatalf("tier=%d score=%d, want progress from screen_changed=true on an identical image", policy.tier, policy.stallScore)
	}
}

// wait_for_stable_screen reports screen_changed when any frame differed while
// waiting, which is not progress; under restriction it only earns the single
// probation an observation gives.
func TestTerminationPolicyIgnoresScreenChangedFromObservationTools(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	restrictWithUnchangedActions(t, policy, screen)

	var payload map[string]any
	if err := json.Unmarshal([]byte(screen), &payload); err != nil {
		t.Fatal(err)
	}
	payload["screen_changed"] = true
	waitResult, _ := json.Marshal(payload)
	policy.AfterToolCall("wait_for_stable_screen", `{}`, string(waitResult), false)
	if policy.stallScore == 0 || policy.tier != TierSoftNotice {
		t.Fatalf("tier=%d score=%d, want only the observation allowance, not progress", policy.tier, policy.stallScore)
	}
	policy.AfterToolCall("touch_gesture", `{"type":"tap","point":{"x":7,"y":7}}`, screen, false)
	policy.AfterToolCall("wait_for_stable_screen", `{}`, string(waitResult), false)
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":8,"y":8}}`); allowed {
		t.Fatal("a second wait_for_stable_screen lifted the restriction")
	}
}

// Repeated screenshots reach the restriction on their own; the observation
// that earns the allowance repeats the previous call too, and its repeat
// score must not take the allowance back.
func TestTerminationPolicyRepeatedObservationKeepsItsAllowance(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	screen := progressTestObservation(t)
	for i := 0; i < 10 && policy.tier < TierRestrictTools; i++ {
		policy.AfterToolCall("screenshot", `{}`, screen, false)
	}
	if policy.tier != TierRestrictTools {
		t.Fatalf("tier = %d score=%d, want repeated screenshots to restrict", policy.tier, policy.stallScore)
	}
	policy.AfterToolCall("screenshot", `{}`, screen, false)
	if _, allowed := policy.BeforeToolCall("touch_gesture", `{"type":"tap","point":{"x":1,"y":1}}`); !allowed {
		t.Fatalf("tier=%d score=%d: the observation did not allow one action", policy.tier, policy.stallScore)
	}
}

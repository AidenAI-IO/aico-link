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
	"strings"
	"testing"
)

func TestTerminationPolicyRepeatWithoutProgressTerminates(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	observation := terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})
	for i := 0; i < 2; i++ {
		if decision := policy.AfterToolCall("touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`, observation, false); decision.Stop {
			t.Fatalf("iteration %d: unexpected stop: %#v", i+1, decision)
		}
	}
	decision := policy.AfterToolCall("touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`, observation, false)
	if !decision.Stop || decision.Reason != StopReasonLoopDetected {
		t.Fatalf("expected loop termination, got %#v", decision)
	}
}

func TestTerminationPolicyParseFailureLimit(t *testing.T) {
	policy := NewTerminationPolicy(TerminationPolicyConfig{ParseFailureLimit: 2})
	if decision := policy.RecordParseFailure(); decision.Stop {
		t.Fatalf("first parse failure should not stop: %#v", decision)
	}
	decision := policy.RecordParseFailure()
	if !decision.Stop || decision.Reason != StopReasonParseFailure {
		t.Fatalf("second parse failure should stop: %#v", decision)
	}
}

func TestTerminationPolicySameResultRequiresSameSignature(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	for _, input := range []string{`{"q":"a"}`, `{"q":"b"}`, `{"q":"c"}`} {
		decision := policy.AfterToolCall("web_search", input, `{"ok":true,"value":"same"}`, false)
		if decision.Stop || policy.sameResultStreak != 1 {
			t.Fatalf("input %s should not count as a repeated action: %#v, streak=%d", input, decision, policy.sameResultStreak)
		}
	}
}

func TestTerminationPolicyDetectsRotatingArtifactRecoveryPaths(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	observation := "same recovered artifact content"
	paths := []string{
		"/userdata/agent/sessions/backend/s_1/tool-results/tr_11111111-1111-1111-1111-111111111111.data",
		"/userdata/agent/sessions/backend/s_1/tool-results/tr_22222222-2222-2222-2222-222222222222.data",
		"/userdata/agent/sessions/backend/s_1/tool-results/tr_33333333-3333-3333-3333-333333333333.data",
	}
	for index, path := range paths {
		input := `{"command":"cat ` + path + `"}`
		decision := policy.AfterToolCall("shell", input, observation, false)
		if index < len(paths)-1 && decision.Stop {
			t.Fatalf("artifact read %d stopped too early: %#v", index+1, decision)
		}
		if index == len(paths)-1 && (!decision.Stop || decision.Reason != StopReasonLoopDetected) {
			t.Fatalf("rotating artifact reads were not terminated: %#v", decision)
		}
	}
}

func TestTerminationPolicyDoesNotCollapseOrdinaryShellPaths(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	for _, path := range []string{"/tmp/a", "/tmp/b", "/tmp/c"} {
		decision := policy.AfterToolCall("shell", `{"command":"cat `+path+`"}`, "same output", false)
		if decision.Stop || policy.sameResultStreak != 1 {
			t.Fatalf("ordinary shell path %q counted as the same action: %#v, streak=%d", path, decision, policy.sameResultStreak)
		}
	}
}

func TestTerminationPolicyNormalizesOnlyArtifactPathTokens(t *testing.T) {
	artifactPath := "/userdata/agent/sessions/backend/tool-results/tr_result.data"
	first := `{"command":"grep -F tr_a.data ` + artifactPath + `"}`
	second := `{"command":"grep -F tr_b.data ` + artifactPath + `"}`
	if toolCallSignature("shell", first) == toolCallSignature("shell", second) {
		t.Fatal("distinct non-artifact command arguments were collapsed")
	}

	for _, command := range []string{
		"grep -F tr_a.data /tmp/tool-results/nested/tr_result.data",
		"grep -F tr_a.data " + artifactPath + ".bak",
		"grep -F tr_a.data " + artifactPath + "-suffix",
	} {
		input := `{"command":"` + command + `"}`
		if normalized := normalizeArtifactRecoveryInput(input); normalized != input {
			t.Fatalf("non-direct or non-boundary artifact token was normalized: %q -> %q", input, normalized)
		}
	}
}

func TestTerminationPolicyExternalCancel(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decision := policy.CheckBeforeIteration(ctx, 1, 10)
	if !decision.Stop || decision.Reason != StopReasonExternal || !strings.Contains(decision.Message, "canceled") {
		t.Fatalf("expected external stop, got %#v", decision)
	}
}

func terminationPolicyScreenshotObservation(t *testing.T, width, height int, changed image.Rectangle) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: color.RGBA{R: 32, G: 48, B: 64, A: 255}}, image.Point{}, draw.Src)
	if !changed.Empty() {
		draw.Draw(img, changed.Intersect(img.Bounds()), &image.Uniform{C: color.RGBA{R: 240, G: 224, B: 208, A: 255}}, image.Point{}, draw.Src)
	}

	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode test screenshot: %v", err)
	}
	payload := struct {
		Width  int    `json:"width"`
		Height int    `json:"height"`
		Format string `json:"format"`
		Size   int    `json:"size"`
		Data   string `json:"data"`
	}{
		Width:  width,
		Height: height,
		Format: "jpeg",
		Size:   jpegData.Len(),
		Data:   base64.StdEncoding.EncodeToString(jpegData.Bytes()),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal test screenshot observation: %v", err)
	}
	return string(encoded)
}

func fillScreenshotProgressGridCell(img draw.Image, bounds image.Rectangle, index int, fill color.Color) {
	row := index / screenshotProgressGridColumns
	column := index % screenshotProgressGridColumns
	x0 := bounds.Min.X + bounds.Dx()*column/screenshotProgressGridColumns
	x1 := bounds.Min.X + bounds.Dx()*(column+1)/screenshotProgressGridColumns
	y0 := bounds.Min.Y + bounds.Dy()*row/screenshotProgressGridRows
	y1 := bounds.Min.Y + bounds.Dy()*(row+1)/screenshotProgressGridRows
	draw.Draw(img, image.Rect(x0, y0, x1, y1), &image.Uniform{C: fill}, image.Point{}, draw.Src)
}

func TestTerminationPolicyDetectsAlternatingArtifactQueries(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	observation := "Error: exit status 1\nStderr:\nKeyError: 'content'"
	for i := 0; i < 12; i++ {
		command := fmt.Sprintf("python3 -c \"import json; data=json.load(open('/userdata/tool-results/tr_original.data')); print(data['results'][0]['content'][:%d])\"", 1000*(i+1))
		if i%2 == 1 {
			command = fmt.Sprintf("cat /userdata/tool-results/tr_copy%d.data", i)
		}
		input, _ := json.Marshal(map[string]string{"command": command})
		decision := policy.AfterToolCall("shell", string(input), observation, i%2 == 0)
		if decision.Stop {
			if decision.Reason != StopReasonLoopDetected {
				t.Fatalf("unexpected stop: %+v", decision)
			}
			return
		}
	}
	t.Fatal("alternating artifact queries returning the same error did not stop")
}

func TestTerminationPolicyAllowsArtifactReadProgress(t *testing.T) {
	policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
	for i := 0; i < 12; i++ {
		input := fmt.Sprintf(`{"command":"fq -r '.summary[%d:%d]' /userdata/tool-results/tr_original.data"}`, i*100, (i+1)*100)
		if decision := policy.AfterToolCall("shell", input, fmt.Sprintf("page %d", i), false); decision.Stop {
			t.Fatalf("new page stopped: %+v", decision)
		}
	}
}

func TestTerminationPolicyResetsArtifactRecoveryStreak(t *testing.T) {
	for _, reset := range []string{"steer", "other tool", "new output"} {
		t.Run(reset, func(t *testing.T) {
			policy := NewTerminationPolicy(DefaultTerminationPolicyConfig())
			read := func(i int) TerminationDecision {
				input := fmt.Sprintf(`{"command":"fq '.summary[0:%d]' /userdata/tool-results/tr_original.data"}`, i)
				return policy.AfterToolCall("shell", input, "same result", false)
			}
			read(1)
			read(2)
			switch reset {
			case "steer":
				policy.ResetForSteer()
			case "other tool":
				policy.AfterToolCall("web_search", `{"q":"next"}`, "same result", false)
			case "new output":
				policy.AfterToolCall("shell", `{"command":"fq '.summary[100:200]' /userdata/tool-results/tr_original.data"}`, "new page", false)
			}
			if decision := read(3); decision.Stop {
				t.Fatalf("stale recovery streak survived %s: %+v", reset, decision)
			}
		})
	}
}

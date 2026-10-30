package agent

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/logging"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"strings"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
)

const postActionScreenshotDelay = time.Second
const postActionCompletedDetail = "action_completed"

type postActionScreenshotResult struct {
	screenshotResult
	ActionOutput  string   `json:"action_output"`
	ScreenStable  *bool    `json:"screen_stable,omitempty"`
	StableWaitMs  *int64   `json:"stable_wait_ms,omitempty"`
	ScreenChanged *bool    `json:"screen_changed,omitempty"`
	LastDiff      *float64 `json:"last_diff,omitempty"`
}

type touchGesturePostMarkerInfo struct {
	Type string  `json:"type"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
}

// stripScreenshotData removes the base64 payload while retaining metadata used
// by session memory, logs, and tool consumers.
func stripScreenshotData(content string) string {
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(content), &result); err != nil || result.Data == "" {
		return content
	}
	format := strings.TrimSpace(result.Format)
	if format == "" {
		format = "jpeg"
	}
	compact := map[string]interface{}{
		"width":  result.Width,
		"height": result.Height,
		"format": format,
		"size":   result.Size,
	}
	if strings.TrimSpace(result.ActionOutput) != "" {
		compact["action_output"] = result.ActionOutput
	}
	if result.ScreenStable != nil {
		compact["screen_stable"] = *result.ScreenStable
	}
	if result.StableWaitMs != nil {
		compact["stable_wait_ms"] = *result.StableWaitMs
	}
	if result.ScreenChanged != nil {
		compact["screen_changed"] = *result.ScreenChanged
	}
	if result.LastDiff != nil {
		compact["last_diff"] = *result.LastDiff
	}
	data, err := json.Marshal(compact)
	if err != nil {
		return content
	}
	return string(data)
}

type postActionScreenshotTool struct {
	inner      langtools.Tool
	waitStable langtools.Tool
	screenshot langtools.Tool
	delay      time.Duration
	waitInput  string
	defaults   ScreenStableDefaults
	// pointer is set for pointer tools in absolute mouse mode; the pointer's
	// own movement is excluded from screen_changed.
	pointer *pointerTracker
}

type stableScreenWaiter interface {
	wait(context.Context, string) (waitStableScreenResult, error)
}

type unstableScreenRecoverer interface {
	recoverAfterUnstableScreen(context.Context, string) error
}

func newPostActionScreenshotTool(inner langtools.Tool, screenshot langtools.Tool, delay time.Duration) langtools.Tool {
	return newPostActionStableScreenshotTool(inner, nil, screenshot, delay, ScreenStableDefaults{})
}

func newPostActionStableScreenshotTool(inner langtools.Tool, waitStable langtools.Tool, screenshot langtools.Tool, delay time.Duration, defaults ScreenStableDefaults) langtools.Tool {
	return &postActionScreenshotTool{
		inner:      inner,
		waitStable: waitStable,
		screenshot: screenshot,
		delay:      delay,
		waitInput:  defaults.InputJSON(),
		defaults:   defaults,
	}
}

func (t *postActionScreenshotTool) Name() string {
	return t.inner.Name()
}

func (t *postActionScreenshotTool) Description() string {
	if t.waitStable != nil {
		return t.inner.Description() + " On successful execution, captures a pre-action baseline, waits for the screen to become stable (or until the configured timeout), and returns the final screenshot observation. screen_changed compares the pre-action baseline with the final stable screenshot using meaningful structural change detection; it ignores the top status area and minor image noise. When screen_changed=false and the action was expected to change the UI, do not assume success and inspect the screenshot before answering or repeating. screen_stable=false means the screen was still changing (for example during video playback) but the screenshot was still captured."
	}
	return fmt.Sprintf(
		"%s Captures a pre-action baseline and, on successful execution, waits %s before returning the final screenshot observation. screen_changed reports meaningful structural change between those screenshots while ignoring the top status area and minor image noise.",
		t.inner.Description(),
		t.delay,
	)
}

func (t *postActionScreenshotTool) ReturnsVisualObservation() bool {
	return true
}

func (t *postActionScreenshotTool) ArgsSchema() map[string]any {
	structured, ok := t.inner.(structuredInputTool)
	if !ok {
		return nil
	}
	return structured.ArgsSchema()
}

func (t *postActionScreenshotTool) Call(ctx context.Context, input string) (string, error) {
	touchscreenRCALogf("post_action start inner=%q input_len=%d", t.inner.Name(), len(input))
	baselineImage := t.captureBaseline(ctx)
	pointerBefore := t.pointer.current()
	actionOutput, err := t.inner.Call(ctx, input)
	if err != nil {
		touchscreenRCALogf("post_action inner error inner=%q err_type=%T", t.inner.Name(), err)
		return "", err
	}
	touchscreenRCALogf("post_action inner completed inner=%q output_len=%d", t.inner.Name(), len(actionOutput))
	if te := ToolErrorFromContext(ctx); te != nil {
		touchscreenRCALogf("post_action inner tool_error inner=%q code=%q category=%q", t.inner.Name(), te.Code, te.Category)
		return toolErrorString(te), nil
	}
	if legacyToolOutputLooksLikeError(actionOutput) {
		trimmed := strings.TrimSpace(actionOutput)
		te := NewToolError(CodeToolExecutionFailed, strings.TrimSpace(trimmed[len("error:"):]))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	pointerAfter := pointerFinalPoint(t.inner.Name(), input, actionOutput)
	t.pointer.set(pointerAfter)

	reportToolProgress(ctx, ToolProgress{
		Status:  "verifying",
		Content: "Action sent; waiting for the screen to stabilize",
	})
	var waitResult waitStableScreenResult
	if t.waitStable != nil {
		touchscreenRCALogf("post_action wait_stable start inner=%q", t.inner.Name())
		if waiter, ok := t.waitStable.(stableScreenWaiter); ok {
			waitResult, err = waiter.wait(ctx, t.waitInput)
			if err != nil {
				touchscreenRCALogf("post_action wait_stable error inner=%q err_type=%T", t.inner.Name(), err)
				return postActionErrorResultf(ctx, postActionErrorCode(err), "%s completed with output %q, but stable-screen wait failed: %v", t.inner.Name(), actionOutput, err), nil
			}
		} else {
			waitOutput, err := t.waitStable.Call(ctx, t.waitInput)
			if err != nil {
				touchscreenRCALogf("post_action wait_stable call error inner=%q err_type=%T", t.inner.Name(), err)
				return postActionErrorResultf(ctx, postActionErrorCode(err), "%s completed with output %q, but stable-screen wait failed: %v", t.inner.Name(), actionOutput, err), nil
			}
			if te := ToolErrorFromContext(ctx); te != nil {
				return wrapPostActionSubtoolError(ctx, te, "%s completed with output %q, but stable-screen wait failed: %s", t.inner.Name(), actionOutput, te.Message), nil
			}
			if legacyToolOutputLooksLikeError(waitOutput) {
				return postActionErrorResultf(ctx, CodeToolExecutionFailed, "%s completed with output %q, but stable-screen wait failed: %s", t.inner.Name(), actionOutput, waitOutput), nil
			}
			if err := json.Unmarshal([]byte(waitOutput), &waitResult); err != nil {
				return postActionErrorResultf(ctx, CodeToolExecutionFailed, "%s completed with output %q, but stable-screen wait returned invalid JSON: %v", t.inner.Name(), actionOutput, err), nil
			}
			if !waitResult.OK {
				return postActionErrorResultf(ctx, CodeToolExecutionFailed, "%s completed with output %q, but stable-screen wait failed: %s", t.inner.Name(), actionOutput, waitOutput), nil
			}
		}
		resolved := t.defaults.Resolved()
		logging.Infof("agent", "tools_post_action_screenshot", "stable-screen: tool=%s timeout_ms=%d elapsed_ms=%d stable=%v", t.inner.Name(), resolved.TimeoutMs, waitResult.ElapsedMs, waitResult.Stable)
		if !waitResult.OK {
			return postActionErrorResultf(ctx, CodeToolExecutionFailed, "%s completed with output %q, but stable-screen wait failed", t.inner.Name(), actionOutput), nil
		}
		if !waitResult.Stable {
			if recoverer, ok := t.inner.(unstableScreenRecoverer); ok {
				if err := recoverer.recoverAfterUnstableScreen(ctx, input); err != nil {
					return postActionErrorResultf(ctx, postActionErrorCode(err), "%s completed with output %q, but unstable-screen recovery failed: %v", t.inner.Name(), actionOutput, err), nil
				}
			}
		}
		touchscreenRCALogf(
			"post_action wait_stable completed inner=%q stable=%v elapsed_ms=%d screen_changed=%v last_diff=%s",
			t.inner.Name(),
			waitResult.Stable,
			waitResult.ElapsedMs,
			waitResult.ScreenChanged != nil && *waitResult.ScreenChanged,
			formatTouchscreenRCAFloatPtr(waitResult.LastDiff),
		)
	} else if err := sleepWithContext(ctx, t.delay); err != nil {
		return postActionErrorResultf(ctx, postActionErrorCode(err), "%s completed with output %q, but post-action delay failed: %v", t.inner.Name(), actionOutput, err), nil
	}

	touchscreenRCALogf("post_action screenshot start inner=%q", t.inner.Name())
	reportToolProgress(ctx, ToolProgress{
		Status:  "verifying",
		Content: "Reading the screen after the action",
	})
	screenshotOutput, err := t.screenshot.Call(ctx, "{}")
	if err != nil {
		touchscreenRCALogf("post_action screenshot error inner=%q err_type=%T", t.inner.Name(), err)
		return postActionErrorResultf(ctx, postActionErrorCode(err), "%s completed with output %q, but post-action screenshot failed: %v", t.inner.Name(), actionOutput, err), nil
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		return wrapPostActionSubtoolError(ctx, te, "%s completed with output %q, but post-action screenshot failed: %s", t.inner.Name(), actionOutput, te.Message), nil
	}

	var result screenshotResult
	if err := json.Unmarshal([]byte(screenshotOutput), &result); err != nil {
		return postActionErrorResultf(ctx, CodeToolExecutionFailed, "%s completed with output %q, but post-action screenshot was invalid: %v", t.inner.Name(), actionOutput, err), nil
	}
	touchscreenRCALogf("post_action screenshot completed inner=%q display=%dx%d capture_backend=%q size=%d", t.inner.Name(), result.Width, result.Height, result.CaptureBackend, result.Size)

	payload := postActionScreenshotResult{
		screenshotResult: result,
		ActionOutput:     actionOutput,
	}
	if finalImage, _ := extractScreenshotImage(screenshotOutput); baselineImage != nil && finalImage != nil {
		var excluded []image.Rectangle
		if t.pointer != nil {
			excluded = pointerExclusionRects(finalImage.Bounds(), pointerBefore, pointerAfter)
		}
		if ratio, comparable := screenshotProgressChangeRatio(baselineImage, finalImage, excluded); comparable {
			changed := ratio > screenshotProgressChangedBlockThreshold
			payload.ScreenChanged = &changed
			// The changed-block share near the threshold is what decides
			// whether small UI changes count; keep it for later tuning.
			logging.Infof("agent", "tools_post_action_screenshot", "progress_compare: tool=%s screen_changed=%v changed_ratio=%.4f changed_blocks=%.0f pointer_excluded=%d", t.inner.Name(), changed, ratio, ratio*screenshotProgressGridColumns*screenshotProgressGridRows, len(excluded))
		}
	}
	if t.waitStable != nil {
		stable := waitResult.Stable
		elapsed := waitResult.ElapsedMs
		payload.ScreenStable = &stable
		payload.StableWaitMs = &elapsed
		payload.LastDiff = waitResult.LastDiff
	}
	if t.inner.Name() == "touch_gesture" {
		if marker, ok := parseTouchGesturePostMarker(input); ok {
			applyTouchGesturePostMarkers(&payload.screenshotResult, []touchGesturePostMarkerInfo{marker})
		} else if markers := parseTapSequencePostMarkers(input, actionOutput); len(markers) > 0 {
			applyTouchGesturePostMarkers(&payload.screenshotResult, markers)
		}
	}

	out, _ := json.Marshal(payload)
	return string(out), nil
}

func parseTouchGesturePostMarker(input string) (touchGesturePostMarkerInfo, bool) {
	var args struct {
		Type  string          `json:"type"`
		Point json.RawMessage `json:"point"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || len(args.Point) == 0 || string(args.Point) == "null" {
		return touchGesturePostMarkerInfo{}, false
	}
	// The public schema asks for an object, while the low-level pointer parser
	// also accepts [x,y] as a compatibility form. Keep marker parsing aligned
	// with both accepted forms and require both coordinates when the point is
	// present so an invalid action cannot receive a misleading marker.
	trimmedPoint := strings.TrimSpace(string(args.Point))
	if strings.HasPrefix(trimmedPoint, "[") {
		var coordinates []json.RawMessage
		if err := json.Unmarshal(args.Point, &coordinates); err != nil || len(coordinates) != 2 {
			return touchGesturePostMarkerInfo{}, false
		}
		if !validTouchMarkerCoordinateJSON(coordinates[0]) || !validTouchMarkerCoordinateJSON(coordinates[1]) {
			return touchGesturePostMarkerInfo{}, false
		}
	} else if strings.HasPrefix(trimmedPoint, "{") {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(args.Point, &object); err != nil || object["x"] == nil || object["y"] == nil {
			return touchGesturePostMarkerInfo{}, false
		}
		if !validTouchMarkerCoordinateJSON(object["x"]) || !validTouchMarkerCoordinateJSON(object["y"]) {
			return touchGesturePostMarkerInfo{}, false
		}
	} else {
		return touchGesturePostMarkerInfo{}, false
	}
	var point pointerPoint
	if err := json.Unmarshal(args.Point, &point); err != nil {
		return touchGesturePostMarkerInfo{}, false
	}
	marker := touchGesturePostMarkerInfo{
		Type: strings.ToLower(strings.TrimSpace(args.Type)),
		X:    point.X.Float64(),
		Y:    point.Y.Float64(),
	}
	if !validTouchGesturePostMarker(marker) {
		return touchGesturePostMarkerInfo{}, false
	}
	return marker, true
}

// parseTapSequencePostMarkers marks the taps a tap_sequence actually sent, as
// reported by its action output, so stopped-early points are not drawn.
func parseTapSequencePostMarkers(input, actionOutput string) []touchGesturePostMarkerInfo {
	var args struct {
		Type   string         `json:"type"`
		Points []pointerPoint `json:"points"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "tap_sequence") {
		return nil
	}
	var result mnk.TapSequenceResult
	if err := json.Unmarshal([]byte(actionOutput), &result); err != nil {
		return nil
	}
	executed := min(result.Executed, len(args.Points))
	markers := make([]touchGesturePostMarkerInfo, 0, executed)
	for _, point := range args.Points[:executed] {
		marker := touchGesturePostMarkerInfo{Type: "tap", X: point.X.Float64(), Y: point.Y.Float64()}
		if validTouchGesturePostMarker(marker) {
			markers = append(markers, marker)
		}
	}
	return markers
}

func validTouchMarkerCoordinateJSON(value json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(value))
	return trimmed != "" && trimmed != "null"
}

func validTouchGesturePostMarker(marker touchGesturePostMarkerInfo) bool {
	switch marker.Type {
	case "tap", "double_tap", "long_press":
	default:
		return false
	}
	return finiteNormalizedCoordinate(marker.X) && finiteNormalizedCoordinate(marker.Y)
}

func finiteNormalizedCoordinate(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1000
}

func applyTouchGesturePostMarkers(result *screenshotResult, markers []touchGesturePostMarkerInfo) {
	if result == nil || strings.TrimSpace(result.Data) == "" {
		return
	}
	format, ok := normalizeScreenshotFormat(result.Format)
	if !ok || format != "jpeg" {
		return
	}
	raw, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil {
		return
	}
	marked, err := drawTouchGesturePostMarkers(raw, markers)
	if err != nil {
		return
	}
	result.Data = base64.StdEncoding.EncodeToString(marked)
	result.Size = len(marked)
}

func drawTouchGesturePostMarkers(jpegData []byte, markers []touchGesturePostMarkerInfo) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, fmt.Errorf("decode screenshot image: %w", err)
	}
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid screenshot dimensions %dx%d", width, height)
	}

	marked := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(marked, marked.Bounds(), img, bounds.Min, draw.Src)
	for _, marker := range markers {
		x := int(math.Round(marker.X / 1000 * float64(max(width-1, 0))))
		y := int(math.Round(marker.Y / 1000 * float64(max(height-1, 0))))
		drawTouchMarker(marked, x, y)
	}

	var output bytes.Buffer
	if err := jpeg.Encode(&output, marked, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encode marked screenshot image: %w", err)
	}
	return output.Bytes(), nil
}

func drawTouchMarker(img *image.RGBA, x, y int) {
	if img == nil {
		return
	}
	minDimension := min(img.Bounds().Dx(), img.Bounds().Dy())
	radius := max(10, min(24, minDimension/28))
	outer := radius + 3
	drawMarkerRing(img, x, y, outer, 3, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	drawMarkerRing(img, x, y, radius, 3, color.RGBA{R: 255, G: 32, B: 32, A: 255})
}

func drawMarkerRing(img *image.RGBA, centerX, centerY, radius, thickness int, c color.Color) {
	outerSquared := radius * radius
	innerRadius := max(radius-thickness, 0)
	innerSquared := innerRadius * innerRadius
	for y := centerY - radius; y <= centerY+radius; y++ {
		for x := centerX - radius; x <= centerX+radius; x++ {
			dx := x - centerX
			dy := y - centerY
			distanceSquared := dx*dx + dy*dy
			if distanceSquared <= outerSquared && distanceSquared >= innerSquared && image.Pt(x, y).In(img.Bounds()) {
				img.Set(x, y, c)
			}
		}
	}
}

func (t *postActionScreenshotTool) captureBaseline(ctx context.Context) image.Image {
	baselineCtx, _ := WithToolError(ctx)
	touchscreenRCALogf("post_action baseline start inner=%q", t.inner.Name())
	output, err := t.screenshot.Call(baselineCtx, "{}")
	if err != nil {
		touchscreenRCALogf("post_action baseline unavailable inner=%q err_type=%T", t.inner.Name(), err)
		return nil
	}
	if te := ToolErrorFromContext(baselineCtx); te != nil {
		touchscreenRCALogf("post_action baseline unavailable inner=%q code=%q category=%q", t.inner.Name(), te.Code, te.Category)
		return nil
	}
	img, _ := extractScreenshotImage(output)
	if img == nil {
		touchscreenRCALogf("post_action baseline unavailable inner=%q reason=invalid_screenshot", t.inner.Name())
		return nil
	}
	touchscreenRCALogf("post_action baseline completed inner=%q", t.inner.Name())
	return img
}

func wrapPostActionSubtoolError(ctx context.Context, te *ToolError, format string, args ...any) string {
	details := make(map[string]any, len(te.Details)+1)
	for key, value := range te.Details {
		details[key] = value
	}
	details[postActionCompletedDetail] = true
	wrapped := NewToolErrorWithDetails(te.Code, fmt.Sprintf(format, args...), details)
	SetToolError(ctx, wrapped)
	return toolErrorString(wrapped)
}

func postActionErrorResultf(ctx context.Context, code string, format string, args ...any) string {
	toolErr := NewToolErrorWithDetails(code, fmt.Sprintf(format, args...), map[string]any{postActionCompletedDetail: true})
	SetToolError(ctx, toolErr)
	return toolErrorString(toolErr)
}

func postActionErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return CodeDeadlineExceeded
	default:
		return CodeToolExecutionFailed
	}
}

// withPointerTracker shares an absolute-mode pointer tracker with this tool.
func (t *postActionScreenshotTool) withPointerTracker(pointer *pointerTracker) *postActionScreenshotTool {
	t.pointer = pointer
	return t
}

func (t *postActionScreenshotTool) SetDeviceTypeFunc(fn func() string) {
	type deviceTypeConfigurable interface {
		SetDeviceTypeFunc(func() string)
	}
	if tool, ok := t.inner.(deviceTypeConfigurable); ok {
		tool.SetDeviceTypeFunc(fn)
	}
}

package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	defaultStableWaitTimeoutMs = 2000
	defaultStableDurationMs    = 250
	defaultDiffThreshold       = 6.0
	stableWaitPollInterval     = 200 * time.Millisecond
)

type ScreenStableDefaults struct {
	TimeoutMs     int
	StableMs      int
	DiffThreshold float64
}

func (d ScreenStableDefaults) Resolved() ScreenStableDefaults {
	timeout := d.TimeoutMs
	if timeout <= 0 {
		timeout = defaultStableWaitTimeoutMs
	}
	stable := d.StableMs
	if stable <= 0 {
		stable = defaultStableDurationMs
	}
	if stable > timeout {
		stable = timeout
	}
	diffThreshold := d.DiffThreshold
	if diffThreshold <= 0 {
		diffThreshold = defaultDiffThreshold
	}
	return ScreenStableDefaults{
		TimeoutMs:     timeout,
		StableMs:      stable,
		DiffThreshold: diffThreshold,
	}
}

func (d ScreenStableDefaults) InputJSON() string {
	resolved := d.Resolved()
	return fmt.Sprintf(
		`{"timeout_ms":%d,"stable_ms":%d,"diff_threshold":%g}`,
		resolved.TimeoutMs,
		resolved.StableMs,
		resolved.DiffThreshold,
	)
}

type WaitStableScreenTool struct {
	client       screenprovider.Provider
	defaults     ScreenStableDefaults
	screen       *screen.ScreenState
	deviceTypeFn func() string
}

func (t *WaitStableScreenTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *WaitStableScreenTool) cropBlack() bool {
	if t == nil {
		return cropBlackForDeviceType(nil)
	}
	return cropBlackForDeviceType(t.deviceTypeFn)
}

type waitStableScreenResult struct {
	OK            bool     `json:"ok"`
	Stable        bool     `json:"stable"`
	ElapsedMs     int64    `json:"elapsed_ms"`
	ScreenChanged *bool    `json:"screen_changed,omitempty"`
	LastDiff      *float64 `json:"last_diff,omitempty"`
}

type waitStableScreenObservationResult struct {
	screenshotResult
	ActionOutput  string   `json:"action_output"`
	OK            bool     `json:"ok"`
	Stable        bool     `json:"stable"`
	ElapsedMs     int64    `json:"elapsed_ms"`
	ScreenStable  *bool    `json:"screen_stable,omitempty"`
	StableWaitMs  *int64   `json:"stable_wait_ms,omitempty"`
	ScreenChanged *bool    `json:"screen_changed,omitempty"`
	LastDiff      *float64 `json:"last_diff,omitempty"`
}

func NewWaitStableScreenTool(provider screenprovider.Provider, defaults ScreenStableDefaults, screens ...*screen.ScreenState) *WaitStableScreenTool {
	var screen *screen.ScreenState
	if len(screens) > 0 {
		screen = screens[0]
	}
	return &WaitStableScreenTool{
		client:   provider,
		defaults: defaults,
		screen:   screen,
	}
}

func (t *WaitStableScreenTool) Name() string { return "wait_for_stable_screen" }

func (t *WaitStableScreenTool) ReturnsVisualObservation() bool { return true }

func (t *WaitStableScreenTool) DynamicExampleInput() string {
	return "{}"
}

func (t *WaitStableScreenTool) Description() string {
	resolved := t.defaults.Resolved()
	return fmt.Sprintf(
		`Wait until the connected display stops changing before judging UI result. `+
			`Use only while operating a visible target UI, after a UI action or known UI transition that may animate, navigate, or load; do not call for text-only reasoning, arithmetic, comparison, or memory lookup. `+
			`Input: {} (empty JSON object). Configured timeouts: timeout_ms=%d, stable_ms=%d, diff_threshold=%g. `+
			`The screen is stable when consecutive frames stay below diff_threshold for stable_ms. `+
			`Returns {"ok":true,"stable":true/false,"elapsed_ms":N,"screen_changed":true/false,...} and provides the screenshot as a visual observation. `+
			`screen_changed reports whether any frame-to-frame motion was observed during this wait window; it is not a before/after action-success signal. Inspect the returned screenshot to judge the preceding action. `+
			`stable=false means the wait timed out while the screen was still changing (for example video playback); that is not an error and the screenshot is still captured as a best-effort observation.`,
		resolved.TimeoutMs,
		resolved.StableMs,
		resolved.DiffThreshold,
	)
}

func (t *WaitStableScreenTool) ArgsSchema() map[string]any {
	// No parameters - tool uses configured timeouts only
	return objectArgsSchema(map[string]any{})
}

func (t *WaitStableScreenTool) Call(ctx context.Context, input string) (string, error) {
	result, err := t.wait(ctx, input)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}
	screenshot, err := t.captureScreenshot()
	if err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "stable-screen wait completed with stable=%v elapsed_ms=%d, but screenshot failed: %v", result.Stable, result.ElapsedMs, err), nil
	}
	stable := result.Stable
	elapsed := result.ElapsedMs
	actionOutput, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("marshal stable-screen result: %w", err)
	}
	out, err := json.Marshal(waitStableScreenObservationResult{
		screenshotResult: screenshot,
		ActionOutput:     string(actionOutput),
		OK:               result.OK,
		Stable:           result.Stable,
		ElapsedMs:        result.ElapsedMs,
		ScreenStable:     &stable,
		StableWaitMs:     &elapsed,
		ScreenChanged:    result.ScreenChanged,
		LastDiff:         result.LastDiff,
	})
	if err != nil {
		return "", fmt.Errorf("marshal stable-screen screenshot observation: %w", err)
	}
	return string(out), nil
}

func (t *WaitStableScreenTool) captureScreenshot() (screenshotResult, error) {
	cropBlack := t.cropBlack()
	meta, jpegData, captureInfo, err := captureScreenshotJPEG(t.client, t.screen, cropBlack)
	if err != nil {
		return screenshotResult{}, err
	}
	if meta.Stale {
		return screenshotResult{}, fmt.Errorf("frame service: STALE_FRAME")
	}
	if meta.PixelFormat != "jpeg" {
		return screenshotResult{}, fmt.Errorf("screen capture returned an unsupported frame")
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("wait_stable.captureScreenshot frame meta=%s capture_backend=%q mapping_before={%s}", formatTouchscreenRCAMetadata(meta), captureInfo.Backend, t.screen.Format())
	}
	active := screen.ScreenActiveArea{}
	sourceWidth := int(meta.Width)
	sourceHeight := int(meta.Height)
	alreadyCropped := false
	if fullWidth, fullHeight, sourceActive, ok := frameMetadataSourceActiveArea(meta); ok {
		sourceWidth = fullWidth
		sourceHeight = fullHeight
		active = sourceActive
		alreadyCropped = true
	} else if cropBlack {
		active = detectScreenshotActiveAreaForScreen(t.screen, jpegData, int(meta.Width), int(meta.Height))
	} else {
		active = screen.ScreenActiveArea{X: 0, Y: 0, Width: sourceWidth, Height: sourceHeight, Valid: true}
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"wait_stable.captureScreenshot resolved active_area source=%dx%d active=%s already_cropped=%v jpeg_dimensions=%dx%d mapping_before_update={%s}",
			sourceWidth,
			sourceHeight,
			active.Format(),
			alreadyCropped,
			meta.Width,
			meta.Height,
			t.screen.Format(),
		)
	}
	if t.screen != nil {
		t.screen.UpdateActiveArea(sourceWidth, sourceHeight, active)
	}
	displayWidth := int(meta.Width)
	displayHeight := int(meta.Height)
	displayData := jpegData
	if cropBlack && !alreadyCropped && active.Valid && (active.X != 0 || active.Y != 0 || active.Width != displayWidth || active.Height != displayHeight) {
		croppedData, croppedWidth, croppedHeight, err := cropJPEGToActiveArea(jpegData, active, screenshotJPEGQuality)
		if err != nil {
			return screenshotResult{}, fmt.Errorf("crop screenshot to active area: %w", err)
		}
		displayWidth = croppedWidth
		displayHeight = croppedHeight
		displayData = croppedData
	}
	if t.screen != nil {
		t.screen.UpdateScreenshot(displayData, displayWidth, displayHeight)
	}
	result := screenshotResult{
		Width:        displayWidth,
		Height:       displayHeight,
		SourceWidth:  sourceWidth,
		SourceHeight: sourceHeight,
		ActiveArea:   &active,
		ActiveWidth:  active.Width,
		ActiveHeight: active.Height,
		Format:       "jpeg",
		Size:         len(displayData),
		Data:         base64.StdEncoding.EncodeToString(displayData),
	}
	applyScreenCaptureInfo(&result, captureInfo)
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"wait_stable.captureScreenshot result display=%dx%d size=%d capture_backend=%q mapping_after={%s}",
			displayWidth,
			displayHeight,
			len(displayData),
			result.CaptureBackend,
			t.screen.Format(),
		)
	}
	return result, nil
}

func (t *WaitStableScreenTool) captureWaitFrame() (*frameMetadata, []byte, screenCaptureInfo, error) {
	if t == nil || t.client == nil {
		return nil, nil, screenCaptureInfo{}, fmt.Errorf("screen capture client not configured")
	}
	hint := screenprovider.CropHint{}
	if t.cropBlack() {
		hint = screenshotCropHint(t.screen)
	}
	return t.client.LatestFrameWithFormat("jpeg", screenshotJPEGQuality, t.cropBlack(), hint)
}

func (t *WaitStableScreenTool) wait(ctx context.Context, input string) (waitStableScreenResult, error) {
	var args struct {
		TimeoutMs     int     `json:"timeout_ms"`
		StableMs      int     `json:"stable_ms"`
		DiffThreshold float64 `json:"diff_threshold"`
	}
	if trimmed := strings.TrimSpace(input); trimmed != "" {
		if err := json.Unmarshal([]byte(input), &args); err != nil {
			return waitStableScreenResult{}, fmt.Errorf("invalid input: %w", err)
		}
	}

	resolvedDefaults := t.defaults.Resolved()
	// Always use configured values; ignore LLM parameters to enforce config authority
	timeout := time.Duration(resolvedDefaults.TimeoutMs) * time.Millisecond
	stableFor := time.Duration(resolvedDefaults.StableMs) * time.Millisecond
	if stableFor > timeout {
		stableFor = timeout
	}
	diffThreshold := resolvedDefaults.DiffThreshold
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"wait_stable.wait start timeout_ms=%d stable_ms=%d diff_threshold=%.3f mapping_before={%s}",
			timeout.Milliseconds(),
			stableFor.Milliseconds(),
			diffThreshold,
			t.screen.Format(),
		)
	}

	start := time.Now()
	deadline := start.Add(timeout)

	prevMeta, prevFrame, prevCaptureInfo, err := t.captureWaitFrame()
	if err != nil {
		return waitStableScreenResult{}, err
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("wait_stable.wait initial frame meta=%s capture_backend=%q", formatTouchscreenRCAMetadata(prevMeta), prevCaptureInfo.Backend)
	}
	prevRGB, err := convertFrameToRGB(prevMeta, prevFrame)
	if err != nil {
		return waitStableScreenResult{}, err
	}
	prevSeq := prevMeta.Seq
	stableSince := time.Now()
	screenChanged := false
	var lastDiff *float64

	for {
		now := time.Now()
		if now.Sub(stableSince) >= stableFor {
			result := waitStableScreenResult{
				OK:            true,
				Stable:        true,
				ElapsedMs:     now.Sub(start).Milliseconds(),
				ScreenChanged: waitStableBoolPtr(screenChanged),
				LastDiff:      lastDiff,
			}
			if touchscreenRCADebugEnabledCached() {
				touchscreenRCALogf("wait_stable.wait completed stable=%v elapsed_ms=%d screen_changed=%v last_diff=%s mapping_after_wait={%s}", result.Stable, result.ElapsedMs, screenChanged, formatTouchscreenRCAFloatPtr(lastDiff), t.screen.Format())
			}
			return result, nil
		}
		if !now.Before(deadline) {
			stable := now.Sub(stableSince) >= stableFor
			result := waitStableScreenResult{
				OK:            true,
				Stable:        stable,
				ElapsedMs:     now.Sub(start).Milliseconds(),
				ScreenChanged: waitStableBoolPtr(screenChanged),
				LastDiff:      lastDiff,
			}
			if touchscreenRCADebugEnabledCached() {
				touchscreenRCALogf("wait_stable.wait timeout stable=%v elapsed_ms=%d screen_changed=%v last_diff=%s mapping_after_wait={%s}", result.Stable, result.ElapsedMs, screenChanged, formatTouchscreenRCAFloatPtr(lastDiff), t.screen.Format())
			}
			return result, nil
		}

		wait := stableWaitPollInterval
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		if wait > 0 {
			if err := sleepContext(ctx, wait); err != nil {
				return waitStableScreenResult{}, err
			}
		}

		meta, frame, captureInfo, err := t.captureWaitFrame()
		if err != nil {
			return waitStableScreenResult{}, err
		}
		if meta.Stale || meta.Seq <= prevSeq {
			if touchscreenRCADebugEnabledCached() {
				touchscreenRCALogf("wait_stable.wait skipped frame meta=%s capture_backend=%q prev_seq=%d", formatTouchscreenRCAMetadata(meta), captureInfo.Backend, prevSeq)
			}
			continue
		}

		rgb, err := convertFrameToRGB(meta, frame)
		if err != nil {
			if touchscreenRCADebugEnabledCached() {
				touchscreenRCALogf("wait_stable.wait convert error meta=%s capture_backend=%q err=%v", formatTouchscreenRCAMetadata(meta), captureInfo.Backend, err)
			}
			screenChanged = true
			prevSeq = meta.Seq
			stableSince = time.Now()
			continue
		}

		diff := meanRGBAbsDiff(prevMeta, prevRGB, meta, rgb)
		lastDiff = &diff
		if diff > 0 {
			screenChanged = true
		}
		if diff > diffThreshold {
			stableSince = time.Now()
		}
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf(
				"wait_stable.wait frame meta=%s capture_backend=%q diff=%.3f threshold=%.3f screen_changed=%v stable_for_ms=%d",
				formatTouchscreenRCAMetadata(meta),
				captureInfo.Backend,
				diff,
				diffThreshold,
				screenChanged,
				time.Since(stableSince).Milliseconds(),
			)
		}
		prevMeta = meta
		prevRGB = rgb
		prevSeq = meta.Seq
	}
}

func waitStableBoolPtr(value bool) *bool {
	return &value
}

func meanRGBAbsDiff(aMeta *frameMetadata, a []byte, bMeta *frameMetadata, b []byte) float64 {
	if aMeta == nil || bMeta == nil || aMeta.Width != bMeta.Width || aMeta.Height != bMeta.Height || len(a) != len(b) || len(a) == 0 {
		return 256.0
	}

	var sum uint64
	for i := range a {
		if a[i] > b[i] {
			sum += uint64(a[i] - b[i])
		} else {
			sum += uint64(b[i] - a[i])
		}
	}
	return float64(sum) / float64(len(a))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"aico-agent/internal/logging"
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
	// stableWaitMinPollInterval paces frame polling for providers that
	// return frames faster than the board's frame service.
	stableWaitMinPollInterval = 150 * time.Millisecond
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
	OK        bool  `json:"ok"`
	Stable    bool  `json:"stable"`
	ElapsedMs int64 `json:"elapsed_ms"`
	// FramesDiffered reports any frame-to-frame change during the wait. It is
	// not the post-action screen_changed before/after signal.
	FramesDiffered *bool `json:"frames_differed,omitempty"`
	// IgnoredMotionBlocks counts grid blocks of continuous motion (video,
	// carousel, clock) left out of the final stability decision.
	IgnoredMotionBlocks int `json:"ignored_motion_blocks,omitempty"`
	// LastDiff is the share of grid blocks, outside ignored motion, that
	// changed in the last frame comparison.
	LastDiff *float64 `json:"last_diff,omitempty"`
}

type waitStableScreenObservationResult struct {
	screenshotResult
	ActionOutput string `json:"action_output"`
	OK           bool   `json:"ok"`
	Stable       bool   `json:"stable"`
	ElapsedMs    int64  `json:"elapsed_ms"`
	ScreenStable *bool  `json:"screen_stable,omitempty"`
	StableWaitMs *int64 `json:"stable_wait_ms,omitempty"`
	// FramesDiffered and IgnoredMotionBlocks are described on
	// waitStableScreenResult.
	FramesDiffered      *bool    `json:"frames_differed,omitempty"`
	IgnoredMotionBlocks int      `json:"ignored_motion_blocks,omitempty"`
	LastDiff            *float64 `json:"last_diff,omitempty"`
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
			`Consecutive frames are compared on a grid of screen blocks; a block changes when its mean brightness moves more than diff_threshold. The screen is stable when no more than 1%% of blocks change for stable_ms. Small areas that keep moving in every frame (video, carousel, clock) are ignored after a few frames and reported as ignored_motion_blocks; large changes such as a page transition are always waited out. `+
			`Returns {"ok":true,"stable":true/false,"elapsed_ms":N,"frames_differed":true/false,...} and provides the screenshot as a visual observation. `+
			`frames_differed reports whether any frame-to-frame change was observed during this wait window; it is not a before/after action-success signal. Inspect the returned screenshot to judge the preceding action. `+
			`stable=false means the wait timed out while the screen was still changing; that is not an error and the screenshot is still captured as a best-effort observation.`,
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
		screenshotResult:    screenshot,
		ActionOutput:        string(actionOutput),
		OK:                  result.OK,
		Stable:              result.Stable,
		ElapsedMs:           result.ElapsedMs,
		ScreenStable:        &stable,
		StableWaitMs:        &elapsed,
		FramesDiffered:      result.FramesDiffered,
		IgnoredMotionBlocks: result.IgnoredMotionBlocks,
		LastDiff:            result.LastDiff,
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
	capture := t.newWaitFrameCapture()

	firstCapture := time.Now()
	prevMeta, prevMeans, err := capture()
	if err != nil {
		return waitStableScreenResult{}, err
	}
	captureTook := time.Since(firstCapture)
	prevSeq := prevMeta.Seq
	stableSince := time.Now()
	quietSeen := false
	framesDiffered := false
	tracker := stableMotionTracker{threshold: diffThreshold}
	var last stableMotionComparison
	var lastDiff *float64

	finish := func(stable bool) waitStableScreenResult {
		result := waitStableScreenResult{
			OK:             true,
			Stable:         stable,
			ElapsedMs:      time.Since(start).Milliseconds(),
			FramesDiffered: waitStableBoolPtr(framesDiffered),
			LastDiff:       lastDiff,
		}
		if stable {
			result.IgnoredMotionBlocks = last.ignored
		}
		logging.Infof("agent", "wait_stable", "wait done: stable=%v elapsed_ms=%d frames_differed=%v ignored_motion_blocks=%d last_changed_blocks=%d", result.Stable, result.ElapsedMs, framesDiffered, last.ignored, last.changed)
		return result
	}

	for {
		now := time.Now()
		if quietSeen && now.Sub(stableSince) >= stableFor {
			return finish(true), nil
		}
		if !now.Before(deadline) {
			return finish(false), nil
		}

		// Start another capture only when at least half of it fits before
		// the deadline, so the wait ends within about half a capture of the
		// configured timeout instead of overrunning it by a whole capture.
		if captureTook > 0 && time.Until(deadline) < captureTook/2 {
			return finish(false), nil
		}
		// Frame capture already takes hundreds of milliseconds on the
		// board; only pace providers that answer instantly.
		polled := time.Now()
		meta, means, err := capture()
		if err != nil {
			return waitStableScreenResult{}, err
		}
		captureTook = time.Since(polled)
		if pause := stableWaitMinPollInterval - time.Since(polled); pause > 0 {
			if err := sleepWithContext(ctx, min(pause, max(time.Until(deadline), 0))); err != nil {
				return waitStableScreenResult{}, err
			}
		}
		if meta.Stale || meta.Seq <= prevSeq {
			continue
		}
		prevSeq = meta.Seq

		comparison := tracker.compare(prevMeans, means)
		prevMeans = means
		last = comparison
		ratio := comparison.changedRatio()
		lastDiff = &ratio
		if comparison.anyChanged || !comparison.comparable {
			framesDiffered = true
		}
		if comparison.quiet() {
			quietSeen = true
		} else {
			quietSeen = false
			stableSince = time.Now()
		}
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("wait_stable.wait frame seq=%d changed_blocks=%d ignored_blocks=%d total=%d quiet=%v stable_for_ms=%d", meta.Seq, comparison.changed, comparison.ignored, comparison.total, comparison.quiet(), time.Since(stableSince).Milliseconds())
		}
	}
}

// newWaitFrameCapture returns block means of the current frame. It asks for
// raw frames, whose luminance plane needs no decoding on the board, and falls
// back to JPEG for providers without raw output.
func (t *WaitStableScreenTool) newWaitFrameCapture() func() (*frameMetadata, lumaBlockMeans, error) {
	useJPEG := false
	return func() (*frameMetadata, lumaBlockMeans, error) {
		if t == nil || t.client == nil {
			return nil, lumaBlockMeans{}, fmt.Errorf("screen capture client not configured")
		}
		var meta *frameMetadata
		var data []byte
		var err error
		if !useJPEG {
			meta, data, _, err = t.client.LatestFrameWithFormat("raw", 0, false, screenprovider.CropHint{})
			if err != nil {
				useJPEG = true
			}
		}
		if useJPEG {
			meta, data, _, err = t.captureWaitFrame()
			if err != nil {
				return nil, lumaBlockMeans{}, err
			}
		}
		if meta == nil {
			return nil, lumaBlockMeans{}, fmt.Errorf("frame service returned no metadata")
		}
		frame, err := frameLuma(meta, data)
		if err != nil {
			// An undecodable frame cannot be compared; it counts as change.
			return meta, lumaBlockMeans{}, nil
		}
		return meta, newLumaBlockMeans(frame, tapSequenceCompareRegion(t.screen, frame)), nil
	}
}

func waitStableBoolPtr(value bool) *bool {
	return &value
}

package agent

import (
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/agent/screenprovider"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
)

const screenshotJPEGQuality = 80

type screenshotResult struct {
	Width          int                      `json:"width"`
	Height         int                      `json:"height"`
	SourceWidth    int                      `json:"source_width,omitempty"`
	SourceHeight   int                      `json:"source_height,omitempty"`
	ActiveArea     *screen.ScreenActiveArea `json:"active_area,omitempty"`
	ActiveWidth    int                      `json:"active_width,omitempty"`
	ActiveHeight   int                      `json:"active_height,omitempty"`
	Format         string                   `json:"format"`
	Size           int                      `json:"size"`
	Data           string                   `json:"data,omitempty"`
	ScreenshotRef  string                   `json:"screenshot_ref,omitempty"`
	CaptureBackend string                   `json:"capture_backend,omitempty"`
	ADBDevice      *adbDeviceInfo           `json:"adb_device,omitempty"`
}

type screenshotFrameClient = screenprovider.Provider

type screenshotReadyFrameClient interface {
	LatestFrameWithFormatWhenReady(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error)
}

func captureScreenshotJPEG(client screenshotFrameClient, screenState *screen.ScreenState, cropBlack bool) (*frameMetadata, []byte, screenCaptureInfo, error) {
	return captureWithCropHint(screenState, cropBlack, func(hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
		return client.LatestFrameWithFormat("jpeg", screenshotJPEGQuality, cropBlack, hint)
	})
}

func captureScreenshotJPEGWhenReady(client screenshotFrameClient, screenState *screen.ScreenState, cropBlack bool) (*frameMetadata, []byte, screenCaptureInfo, error) {
	readyClient, ok := client.(screenshotReadyFrameClient)
	if !ok {
		return captureScreenshotJPEG(client, screenState, cropBlack)
	}
	return captureWithCropHint(screenState, cropBlack, func(hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
		return readyClient.LatestFrameWithFormatWhenReady("jpeg", screenshotJPEGQuality, cropBlack, hint)
	})
}

// captureWithCropHint passes the frame service the phone screen size it crops
// to. A live report is remembered with the frame size it cropped; without one
// (companion app closed, Agent restarted) the remembered size is used, and the
// frame is captured again without a hint if the frame size no longer matches.
func captureWithCropHint(screenState *screen.ScreenState, cropBlack bool, capture func(screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error)) (*frameMetadata, []byte, screenCaptureInfo, error) {
	if !cropBlack {
		return capture(screenprovider.CropHint{})
	}
	fallback := screenState.PhoneScreenFallback()
	if hint := screenshotCropHint(screenState); hint.ScreenWidth > 0 {
		meta, data, info, err := capture(hint)
		if err == nil && meta != nil && meta.SourceWidth > 0 && meta.CropWidth > 0 && fallback != nil {
			fallback.Remember(screenState.PhoneScreenInfo(), int(meta.SourceWidth), int(meta.SourceHeight))
		}
		return meta, data, info, err
	}
	if fallback == nil {
		return capture(screenprovider.CropHint{})
	}
	remembered, frameWidth, frameHeight, ok := fallback.LookupForDevice()
	if !ok {
		return captureAndLearn(fallback, capture)
	}
	meta, data, info, err := capture(cropHintForPhoneScreen(remembered))
	if err != nil || meta == nil || meta.SourceWidth == 0 {
		return meta, data, info, err
	}
	if frameWidth == 0 {
		// The app reported before any capture: this frame completes the entry.
		fallback.NoteFrame(int(meta.SourceWidth), int(meta.SourceHeight))
		return meta, data, info, err
	}
	if int(meta.SourceWidth) != frameWidth || int(meta.SourceHeight) != frameHeight {
		return captureAndLearn(fallback, capture)
	}
	if fallback.ShouldReprobe() {
		// The hinted frame is returned; this extra capture only lets a
		// remembered entry be corrected (another phone, a wrong detection).
		_, _, _, _ = captureAndLearn(fallback, capture)
	}
	return meta, data, info, err
}

// captureAndLearn captures without a hint, letting the frame service detect
// the black borders, and offers its crop to the fallback store.
func captureAndLearn(fallback screen.PhoneScreenFallback, capture func(screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error)) (*frameMetadata, []byte, screenCaptureInfo, error) {
	meta, data, info, err := capture(screenprovider.CropHint{})
	if err == nil && meta != nil && meta.SourceWidth > 0 {
		// Completes an entry the app reported before any capture.
		fallback.NoteFrame(int(meta.SourceWidth), int(meta.SourceHeight))
		fallback.ObserveCrop(int(meta.CropWidth), int(meta.CropHeight), int(meta.SourceWidth), int(meta.SourceHeight),
			cropEdgesOnScreen(meta, data))
	}
	return meta, data, info, err
}

// A cut edge of a black-border crop lies on the phone screen edge when most of
// it is clearly above black. The frame service treats a uniform near-black line
// as border, so on a near-black page an over-crop ends on content, but only on
// a few glyph or icon pixels; the real edge of any screen that is not
// near-black (a light page, a colourful dark wallpaper) is above black along
// most of its length. Rounded corners are skipped.
const (
	cropEdgeBrightness   = 20.0
	cropEdgeCoverage     = 0.6
	cropEdgeCornerMargin = 0.08
	cropEdgeDepth        = 3
)

// cropEdgesOnScreen reports whether every cut edge of the crop in this JPEG
// lies on the phone screen edge, so the crop is the whole screen and safe to
// learn. A crop that cut nothing proves nothing and is not trusted.
func cropEdgesOnScreen(meta *frameMetadata, jpegData []byte) bool {
	if meta == nil || meta.CropWidth == 0 || meta.CropHeight == 0 {
		return false
	}
	horizontal := meta.CropWidth < meta.SourceWidth
	vertical := meta.CropHeight < meta.SourceHeight
	if !horizontal && !vertical {
		return false
	}
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return false
	}
	b := img.Bounds()
	if horizontal && (!cropEdgeBright(img, b.Min.X, 1, b.Min.Y, b.Dy(), false) ||
		!cropEdgeBright(img, b.Max.X-1, -1, b.Min.Y, b.Dy(), false)) {
		return false
	}
	if vertical && (!cropEdgeBright(img, b.Min.Y, 1, b.Min.X, b.Dx(), true) ||
		!cropEdgeBright(img, b.Max.Y-1, -1, b.Min.X, b.Dx(), true)) {
		return false
	}
	return true
}

// cropEdgeBright checks the first cropEdgeDepth lines inward from edge (the
// crop keeps a pixel of border and JPEG blurs it) and reports whether any is
// mostly above black.
func cropEdgeBright(img image.Image, edge, step, start, length int, row bool) bool {
	margin := int(float64(length) * cropEdgeCornerMargin)
	start, length = start+margin, length-2*margin
	samples := min(64, length)
	if samples <= 1 {
		return false
	}
	for depth := 0; depth < cropEdgeDepth; depth++ {
		fixed := edge + depth*step
		bright := 0
		for i := 0; i < samples; i++ {
			position := start + i*(length-1)/(samples-1)
			var c color.Color
			if row {
				c = img.At(position, fixed)
			} else {
				c = img.At(fixed, position)
			}
			if pixelBrightness(c) > cropEdgeBrightness {
				bright++
			}
		}
		if float64(bright) >= cropEdgeCoverage*float64(samples) {
			return true
		}
	}
	return false
}

func screenshotCropHint(screenState *screen.ScreenState) screenprovider.CropHint {
	if screenState == nil {
		return screenprovider.CropHint{}
	}
	return cropHintForPhoneScreen(screenState.PhoneScreenInfo())
}

func cropHintForPhoneScreen(phoneScreen screen.PhoneScreenInfo) screenprovider.CropHint {
	width, height, ok := currentPhoneScreenDimensions(phoneScreen)
	if !ok {
		width, height, ok = nativePhoneScreenDimensions(phoneScreen)
		if !ok {
			return screenprovider.CropHint{}
		}
	}
	return screenprovider.CropHint{
		ScreenWidth:  int(math.Round(width)),
		ScreenHeight: int(math.Round(height)),
	}
}

// screenshotMinimalWidth is retained for callers that still need the legacy
// horizontal-only hint.
func screenshotMinimalWidth(screenState *screen.ScreenState) int {
	hint := screenshotCropHint(screenState)
	if hint.ScreenWidth <= 0 || hint.ScreenHeight <= 0 {
		return 0
	}
	minimalWidth := int(math.Round(1080 * float64(hint.ScreenWidth) / float64(hint.ScreenHeight)))
	if minimalWidth < 1 {
		return 0
	}
	return minimalWidth
}

// capturedFrame is a JPEG frame cropped to the Active Area, the region holding
// the mirrored device screen.
type capturedFrame struct {
	Meta           *frameMetadata
	Data           []byte
	Width          int
	Height         int
	SourceWidth    int
	SourceHeight   int
	ActiveArea     screen.ScreenActiveArea
	AlreadyCropped bool
	CaptureInfo    screenCaptureInfo
}

// captureActiveAreaFrame grabs the latest frame and optionally crops it to the
// Active Area without updating the shared mapping state.
func captureActiveAreaFrame(client screenshotFrameClient, screenState *screen.ScreenState, cropBlack bool) (capturedFrame, error) {
	if client == nil {
		return capturedFrame{}, fmt.Errorf("screen capture client not configured")
	}
	meta, jpegData, captureInfo, err := captureScreenshotJPEG(client, screenState, cropBlack)
	if err != nil {
		return capturedFrame{}, err
	}
	if meta == nil {
		return capturedFrame{}, fmt.Errorf("frame service returned no metadata")
	}
	if meta.Stale {
		return capturedFrame{}, fmt.Errorf("frame service: STALE_FRAME")
	}
	if meta.PixelFormat != "jpeg" {
		return capturedFrame{}, fmt.Errorf("screen capture returned an unsupported frame")
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
		active = detectScreenshotActiveAreaForScreen(screenState, jpegData, int(meta.Width), int(meta.Height))
	} else {
		active = screen.ScreenActiveArea{X: 0, Y: 0, Width: sourceWidth, Height: sourceHeight, Valid: true}
	}

	displayWidth := int(meta.Width)
	displayHeight := int(meta.Height)
	displayData := jpegData
	if cropBlack && !alreadyCropped && active.Valid &&
		(active.X != 0 || active.Y != 0 || active.Width != displayWidth || active.Height != displayHeight) {
		croppedData, croppedWidth, croppedHeight, cropErr := cropJPEGToActiveArea(jpegData, active, screenshotJPEGQuality)
		if cropErr != nil {
			return capturedFrame{}, fmt.Errorf("crop screenshot to active area: %w", cropErr)
		}
		displayWidth = croppedWidth
		displayHeight = croppedHeight
		displayData = croppedData
	}

	return capturedFrame{
		Meta:           meta,
		Data:           displayData,
		Width:          displayWidth,
		Height:         displayHeight,
		SourceWidth:    sourceWidth,
		SourceHeight:   sourceHeight,
		ActiveArea:     active,
		AlreadyCropped: alreadyCropped,
		CaptureInfo:    captureInfo,
	}, nil
}

func applyScreenCaptureInfo(result *screenshotResult, info screenCaptureInfo) {
	if result == nil {
		return
	}
	result.CaptureBackend = info.Backend
	result.ADBDevice = cloneADBDeviceInfo(info.ADBDevice)
}

// ScreenshotTool captures a screenshot from the frame service.
type ScreenshotTool struct {
	client       screenshotFrameClient
	screen       *screen.ScreenState
	deviceTypeFn func() string
}

func (t *ScreenshotTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func cropBlackForDeviceType(deviceTypeFn func() string) bool {
	deviceType := defaultDeviceType
	if deviceTypeFn != nil {
		deviceType = deviceTypeFn()
	}
	platform := deviceTypePlatform(deviceType)
	return platform == "ios" || platform == "android"
}

func (t *ScreenshotTool) cropBlack() bool {
	if t == nil {
		return cropBlackForDeviceType(nil)
	}
	return cropBlackForDeviceType(t.deviceTypeFn)
}

func frameMetadataSourceActiveArea(meta *frameMetadata) (sourceWidth, sourceHeight int, active screen.ScreenActiveArea, ok bool) {
	if meta == nil || meta.SourceWidth == 0 || meta.SourceHeight == 0 {
		return 0, 0, screen.ScreenActiveArea{}, false
	}
	sourceWidth = int(meta.SourceWidth)
	sourceHeight = int(meta.SourceHeight)
	active = screen.ScreenActiveArea{
		X:      int(meta.CropX),
		Y:      int(meta.CropY),
		Width:  int(meta.CropWidth),
		Height: int(meta.CropHeight),
		Valid:  meta.CropWidth > 0 && meta.CropHeight > 0,
	}
	if !active.Valid {
		active = screen.ScreenActiveArea{
			X:      0,
			Y:      0,
			Width:  sourceWidth,
			Height: sourceHeight,
			Valid:  true,
		}
	}
	if active.X < 0 || active.Y < 0 || active.Width <= 0 || active.Height <= 0 ||
		active.X+active.Width > sourceWidth || active.Y+active.Height > sourceHeight {
		return 0, 0, screen.ScreenActiveArea{}, false
	}
	return sourceWidth, sourceHeight, active, true
}

func NewScreenshotTool(provider screenprovider.Provider, screen *screen.ScreenState) *ScreenshotTool {
	return &ScreenshotTool{
		client: provider,
		screen: screen,
	}
}

func (t *ScreenshotTool) Name() string { return "screenshot" }

func (t *ScreenshotTool) ReturnsVisualObservation() bool { return true }

func (t *ScreenshotTool) Description() string {
	return `Capture a screenshot from the connected display. No input required (pass empty JSON {} or ""). ` +
		`Returns ok in the tool result and provides the image in a follow-up state message.`
}

func (t *ScreenshotTool) ArgsSchema() map[string]any {
	return objectArgsSchema(nil)
}

func (t *ScreenshotTool) Call(ctx context.Context, input string) (string, error) {
	return t.call(ctx, input, true)
}

func (t *ScreenshotTool) CallWithoutStartupWait(ctx context.Context, input string) (string, error) {
	return t.call(ctx, input, false)
}

func (t *ScreenshotTool) call(_ context.Context, _ string, waitForReady bool) (string, error) {
	// Request JPEG format directly from frame_service (hardware-encoded)
	cropBlack := t.cropBlack()
	var meta *frameMetadata
	var jpegData []byte
	var captureInfo screenCaptureInfo
	var err error
	if waitForReady {
		meta, jpegData, captureInfo, err = captureScreenshotJPEGWhenReady(t.client, t.screen, cropBlack)
	} else {
		meta, jpegData, captureInfo, err = captureScreenshotJPEG(t.client, t.screen, cropBlack)
	}
	if err != nil {
		return "", err
	}
	if meta.Stale {
		return "", fmt.Errorf("frame service: STALE_FRAME")
	}
	if meta.PixelFormat != "jpeg" {
		return "", fmt.Errorf("screen capture returned an unsupported frame")
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("screenshot frame meta=%s capture_backend=%q mapping_before={%s}", formatTouchscreenRCAMetadata(meta), captureInfo.Backend, t.screen.Format())
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
			"screenshot resolved active_area source=%dx%d active=%s already_cropped=%v jpeg_dimensions=%dx%d mapping_before_update={%s}",
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

	// Crop to active_area so LLM sees only the mirrored phone touch region.
	displayWidth := int(meta.Width)
	displayHeight := int(meta.Height)
	displayData := jpegData
	if cropBlack && !alreadyCropped && active.Valid && (active.X != 0 || active.Y != 0 || active.Width != displayWidth || active.Height != displayHeight) {
		croppedData, croppedWidth, croppedHeight, err := cropJPEGToActiveArea(jpegData, active, screenshotJPEGQuality)
		if err != nil {
			return "", fmt.Errorf("crop screenshot to active area: %w", err)
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
			"screenshot result display=%dx%d size=%d capture_backend=%q mapping_after={%s}",
			displayWidth,
			displayHeight,
			len(displayData),
			result.CaptureBackend,
			t.screen.Format(),
		)
	}

	out, _ := json.Marshal(postActionScreenshotResult{
		screenshotResult: result,
		ActionOutput:     "ok",
	})
	return string(out), nil
}

func detectScreenshotActiveArea(jpegData []byte, expectedWidth, expectedHeight int) screen.ScreenActiveArea {
	return detectScreenshotActiveAreaWithPhoneScreen(jpegData, expectedWidth, expectedHeight, nil)
}

func detectScreenshotActiveAreaForScreen(screen *screen.ScreenState, jpegData []byte, expectedWidth, expectedHeight int) screen.ScreenActiveArea {
	if screen == nil {
		return detectScreenshotActiveArea(jpegData, expectedWidth, expectedHeight)
	}
	phoneScreen := screen.PhoneScreenInfo()
	if !phoneScreenHasDimensions(phoneScreen) {
		// No live report (for example the companion app is closed and the
		// Agent restarted): use the last report for this frame size.
		if fallback := screen.PhoneScreenFallback(); fallback != nil {
			if remembered, ok := fallback.Lookup(expectedWidth, expectedHeight); ok {
				return detectScreenshotActiveAreaWithPhoneScreen(jpegData, expectedWidth, expectedHeight, &remembered)
			}
		}
		return detectScreenshotActiveAreaWithPhoneScreen(jpegData, expectedWidth, expectedHeight, &phoneScreen)
	}
	active := detectScreenshotActiveAreaWithPhoneScreen(jpegData, expectedWidth, expectedHeight, &phoneScreen)
	if fallback := screen.PhoneScreenFallback(); active.Valid && fallback != nil {
		fallback.Remember(phoneScreen, expectedWidth, expectedHeight)
	}
	return active
}

func phoneScreenHasDimensions(info screen.PhoneScreenInfo) bool {
	_, _, ok := entryScreenDimensions(info)
	return ok
}

func detectScreenshotActiveAreaWithPhoneScreen(jpegData []byte, expectedWidth, expectedHeight int, phoneScreen *screen.PhoneScreenInfo) screen.ScreenActiveArea {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return screen.ScreenActiveArea{}
	}
	approx := detectImageActiveArea(img, expectedWidth, expectedHeight)
	if phoneScreen != nil {
		if derived, ok := deriveActiveAreaFromPhoneScreen(expectedWidth, expectedHeight, *phoneScreen, approx); ok {
			return derived
		}
	}
	return approx
}

func deriveActiveAreaFromPhoneScreen(frameWidth, frameHeight int, phoneScreen screen.PhoneScreenInfo, approx screen.ScreenActiveArea) (screen.ScreenActiveArea, bool) {
	if frameWidth <= 0 || frameHeight <= 0 {
		return screen.ScreenActiveArea{}, false
	}
	candidates := phoneScreenAspectRatioCandidates(phoneScreen)
	if len(candidates) == 0 {
		return screen.ScreenActiveArea{}, false
	}
	if len(candidates) > 1 && !approx.Valid {
		return screen.ScreenActiveArea{}, false
	}
	best := screen.ScreenActiveArea{}
	bestScore := math.MaxFloat64
	for _, aspectRatio := range candidates {
		candidate, ok := projectAspectRatioToFrame(frameWidth, frameHeight, aspectRatio)
		if !ok {
			continue
		}
		score := scoreActiveAreaCandidate(candidate, approx, frameWidth, frameHeight)
		if score < bestScore {
			best = candidate
			bestScore = score
		}
	}
	if !best.Valid {
		return screen.ScreenActiveArea{}, false
	}
	return best, true
}

func phoneScreenAspectRatioCandidates(phoneScreen screen.PhoneScreenInfo) []float64 {
	candidates := make([]float64, 0, 2)
	if width, height, ok := currentPhoneScreenDimensions(phoneScreen); ok {
		candidates = appendAspectRatioOrientations(candidates, width, height)
	}
	if width, height, ok := nativePhoneScreenDimensions(phoneScreen); ok {
		candidates = appendAspectRatioOrientations(candidates, width, height)
	}
	return candidates
}

func currentPhoneScreenDimensions(phoneScreen screen.PhoneScreenInfo) (float64, float64, bool) {
	switch {
	case phoneScreen.WidthPixels != nil && phoneScreen.HeightPixels != nil && *phoneScreen.WidthPixels > 0 && *phoneScreen.HeightPixels > 0:
		return float64(*phoneScreen.WidthPixels), float64(*phoneScreen.HeightPixels), true
	case phoneScreen.Width != nil && phoneScreen.Height != nil && *phoneScreen.Width > 0 && *phoneScreen.Height > 0:
		return *phoneScreen.Width, *phoneScreen.Height, true
	default:
		return 0, 0, false
	}
}

func nativePhoneScreenDimensions(phoneScreen screen.PhoneScreenInfo) (float64, float64, bool) {
	if phoneScreen.NativeWidthPixels != nil && phoneScreen.NativeHeightPixels != nil &&
		*phoneScreen.NativeWidthPixels > 0 && *phoneScreen.NativeHeightPixels > 0 {
		return float64(*phoneScreen.NativeWidthPixels), float64(*phoneScreen.NativeHeightPixels), true
	}
	return 0, 0, false
}

func appendUniqueAspectCandidate(candidates []float64, aspectRatio float64) []float64 {
	if aspectRatio <= 0 || math.IsNaN(aspectRatio) || math.IsInf(aspectRatio, 0) {
		return candidates
	}
	for _, candidate := range candidates {
		if math.Abs(candidate-aspectRatio) < 1e-6 {
			return candidates
		}
	}
	return append(candidates, aspectRatio)
}

func appendAspectRatioOrientations(candidates []float64, width, height float64) []float64 {
	if width <= 0 || height <= 0 {
		return candidates
	}
	candidates = appendUniqueAspectCandidate(candidates, width/height)
	candidates = appendUniqueAspectCandidate(candidates, height/width)
	return candidates
}

func projectAspectRatioToFrame(frameWidth, frameHeight int, aspectRatio float64) (screen.ScreenActiveArea, bool) {
	if frameWidth <= 0 || frameHeight <= 0 || aspectRatio <= 0 || math.IsNaN(aspectRatio) || math.IsInf(aspectRatio, 0) {
		return screen.ScreenActiveArea{}, false
	}
	frameAspectRatio := float64(frameWidth) / float64(frameHeight)
	activeWidth := frameWidth
	activeHeight := frameHeight
	if aspectRatio <= frameAspectRatio {
		activeWidth = int(math.Round(float64(frameHeight) * aspectRatio))
	} else {
		activeHeight = int(math.Round(float64(frameWidth) / aspectRatio))
	}
	if activeWidth < 1 || activeWidth > frameWidth || activeHeight < 1 || activeHeight > frameHeight {
		return screen.ScreenActiveArea{}, false
	}
	return screen.ScreenActiveArea{
		X:      (frameWidth - activeWidth) / 2,
		Y:      (frameHeight - activeHeight) / 2,
		Width:  activeWidth,
		Height: activeHeight,
		Valid:  true,
	}, true
}

func scoreActiveAreaCandidate(candidate, approx screen.ScreenActiveArea, frameWidth, frameHeight int) float64 {
	if !approx.Valid || frameWidth <= 0 || frameHeight <= 0 {
		return 0
	}
	return math.Abs(float64(candidate.X-approx.X))/float64(frameWidth) +
		math.Abs(float64(candidate.Y-approx.Y))/float64(frameHeight) +
		math.Abs(float64(candidate.Width-approx.Width))/float64(frameWidth) +
		math.Abs(float64(candidate.Height-approx.Height))/float64(frameHeight)
}

func detectImageActiveArea(img image.Image, expectedWidth, expectedHeight int) screen.ScreenActiveArea {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return screen.ScreenActiveArea{}
	}
	if expectedWidth > 0 && expectedHeight > 0 && (width != expectedWidth || height != expectedHeight) {
		width = expectedWidth
		height = expectedHeight
	}

	threshold := 10.0
	left, right, horizontal := detectImageAxisBounds(width, func(position int) bool {
		return imageColumnDark(img, bounds.Min.X+position, bounds.Min.Y, height, threshold)
	})
	top, bottom, vertical := detectImageAxisBounds(height, func(position int) bool {
		return imageRowDark(img, bounds.Min.Y+position, bounds.Min.X, width, threshold)
	})
	if horizontal && vertical {
		horizontalRemoved := 1 - float64(right-left+1)/float64(width)
		verticalRemoved := 1 - float64(bottom-top+1)/float64(height)
		horizontal = horizontalRemoved > verticalRemoved
		vertical = !horizontal
	}
	if horizontal {
		return screen.ScreenActiveArea{X: left, Y: 0, Width: right - left + 1, Height: height, Valid: true}
	}
	if vertical {
		return screen.ScreenActiveArea{X: 0, Y: top, Width: width, Height: bottom - top + 1, Valid: true}
	}
	return screen.ScreenActiveArea{}
}

func detectImageAxisBounds(length int, dark func(int) bool) (int, int, bool) {
	if length <= 1 || !dark(0) || !dark(length-1) {
		return 0, length - 1, false
	}
	activeSeed := -1
	for _, position := range []int{length / 2, length / 4, length * 3 / 4, length / 8, length * 3 / 8, length * 5 / 8, length * 7 / 8} {
		if !dark(position) {
			activeSeed = position
			break
		}
	}
	if activeSeed < 0 {
		return 0, length - 1, false
	}
	first := 0
	for first < activeSeed && dark(first) {
		first++
	}
	last := length - 1
	for last > activeSeed && dark(last) {
		last--
	}
	if first > last {
		return 0, length - 1, false
	}
	activeLength := last - first + 1
	tolerance := max(4, length/100)
	valid := activeLength >= length/5 && activeLength <= length*95/100 &&
		cropAbsInt(first-(length-1-last)) <= tolerance
	return first, last, valid
}

func imageColumnDark(img image.Image, x, minY, height int, threshold float64) bool {
	return imageAxisDark(img, x, minY, height, threshold, false)
}

func imageRowDark(img image.Image, y, minX, width int, threshold float64) bool {
	return imageAxisDark(img, y, minX, width, threshold, true)
}

func imageAxisDark(img image.Image, fixed, varyingStart, varyingLength int, threshold float64, varyingX bool) bool {
	samples := min(64, varyingLength)
	if samples <= 0 {
		return true
	}
	if samples == 1 {
		if varyingX {
			return pixelBrightness(img.At(varyingStart, fixed)) <= threshold
		}
		return pixelBrightness(img.At(fixed, varyingStart)) <= threshold
	}
	bright := 0
	for i := 0; i < samples; i++ {
		position := varyingStart + int(math.Round(float64(i)*float64(varyingLength-1)/float64(samples-1)))
		var c color.Color
		if varyingX {
			c = img.At(position, fixed)
		} else {
			c = img.At(fixed, position)
		}
		if pixelBrightness(c) > threshold {
			bright++
		}
	}
	return bright <= samples/20
}

func cropAbsInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func pixelBrightness(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	return (0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8))
}

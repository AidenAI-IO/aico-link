package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
)

type coordinateDebugScreenshotOptions struct {
	CropBlackBars bool
}

type coordinateDebugScreenshotResult struct {
	screenshotResult
	SourceWidth                int                      `json:"source_width"`
	SourceHeight               int                      `json:"source_height"`
	SourceActiveArea           *screen.ScreenActiveArea `json:"source_active_area,omitempty"`
	OriginalScreenWidthPixels  *int                     `json:"original_screen_width_pixels,omitempty"`
	OriginalScreenHeightPixels *int                     `json:"original_screen_height_pixels,omitempty"`
}

type phoneScreenHintProvider struct {
	provider screenprovider.Provider
	screen   *screen.ScreenState
}

func (p *phoneScreenHintProvider) LatestFrameWithFormat(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	if cropBlack && hint.MinimalWidth <= 0 && (hint.ScreenWidth <= 0 || hint.ScreenHeight <= 0) {
		hint = screenshotCropHint(p.screen)
	}
	return p.provider.LatestFrameWithFormat(format, quality, cropBlack, hint)
}

func (s *Server) coordinateDebugScreen() *screen.ScreenState {
	if s == nil || s.runtime == nil || s.runtime.toolSnapshot() == nil {
		return nil
	}
	return s.runtime.toolSnapshot().screen
}

func (s *Server) coordinateDebugOriginalScreenSize() (*int, *int) {
	screen := s.coordinateDebugScreen()
	if screen == nil {
		return nil, nil
	}
	width, height, ok := preferredPhoneScreenPixels(screen.PhoneScreenInfo())
	if !ok {
		return nil, nil
	}
	return &width, &height
}

func (s *Server) newCoordinateDebugScreenshotResult(display screenshotResult, sourceWidth, sourceHeight int, sourceActive *screen.ScreenActiveArea) *coordinateDebugScreenshotResult {
	if sourceWidth <= 0 {
		sourceWidth = display.Width
	}
	if sourceHeight <= 0 {
		sourceHeight = display.Height
	}
	result := &coordinateDebugScreenshotResult{
		screenshotResult: display,
		SourceWidth:      sourceWidth,
		SourceHeight:     sourceHeight,
	}
	if sourceActive != nil {
		result.SourceActiveArea = sourceActive
	}
	result.OriginalScreenWidthPixels, result.OriginalScreenHeightPixels = s.coordinateDebugOriginalScreenSize()
	return result
}

func (s *Server) coordinateDebugScreenshotResultFromScreenState(display screenshotResult) *coordinateDebugScreenshotResult {
	sourceWidth := display.Width
	sourceHeight := display.Height
	var sourceActive *screen.ScreenActiveArea
	screen := s.coordinateDebugScreen()
	if screen != nil {
		if width, height, active, age, ok := screen.ActiveAreaWithAge(); ok && age < screenDimensionsStaleAfter {
			sourceWidth = width
			sourceHeight = height
			if active.Valid && (active.X != 0 || active.Y != 0 || active.Width != width || active.Height != height) {
				activeCopy := active
				sourceActive = &activeCopy
			}
		}
	}
	return s.newCoordinateDebugScreenshotResult(display, sourceWidth, sourceHeight, sourceActive)
}

func coordinateDebugScreenshotMatchesMapping(display screenshotResult, state screen.ScreenMappingState) bool {
	if state.Width <= 0 || state.Height <= 0 {
		return false
	}
	active := state.Active
	if !active.Valid {
		active = screen.ScreenActiveArea{
			X:      0,
			Y:      0,
			Width:  state.Width,
			Height: state.Height,
			Valid:  true,
		}
	}
	if active.X < 0 || active.Y < 0 || active.Width <= 0 || active.Height <= 0 ||
		active.X+active.Width > state.Width || active.Y+active.Height > state.Height {
		return false
	}
	return display.Width == active.Width && display.Height == active.Height
}

func coordinateDebugDisplayScreenshot(jpegData []byte, width, height int) screenshotResult {
	return screenshotResult{
		Width:  width,
		Height: height,
		Format: "jpeg",
		Size:   len(jpegData),
		Data:   base64.StdEncoding.EncodeToString(jpegData),
	}
}

func coordinateDebugSourceActiveArea(active screen.ScreenActiveArea, sourceWidth, sourceHeight int) *screen.ScreenActiveArea {
	if !active.Valid {
		return nil
	}
	if active.X == 0 && active.Y == 0 && active.Width == sourceWidth && active.Height == sourceHeight {
		return nil
	}
	activeCopy := active
	return &activeCopy
}

func (s *Server) coordinateDebugCaptureClient() screenprovider.Provider {
	if s == nil {
		return nil
	}
	s.screenCaptureMu.Lock()
	defer s.screenCaptureMu.Unlock()
	if s.screenCaptureClient == nil {
		s.screenCaptureClient = screenProviderFromRuntime(s.runtime)
	}
	return s.screenCaptureClient
}

func (s *Server) providerScreenshotClient() screenprovider.Provider {
	provider := s.coordinateDebugCaptureClient()
	if provider == nil {
		return nil
	}
	return &phoneScreenHintProvider{
		provider: provider,
		screen:   s.coordinateDebugScreen(),
	}
}

func (s *Server) captureCoordinateDebugScreenshot(options coordinateDebugScreenshotOptions) (*coordinateDebugScreenshotResult, []byte, error) {
	if s == nil || s.runtime == nil {
		return nil, nil, fmt.Errorf("runtime not configured")
	}
	_ = s.bridgeEnvironment()

	client := s.coordinateDebugCaptureClient()
	if client == nil {
		return nil, nil, fmt.Errorf("screen capture client not configured")
	}
	meta, jpegData, captureInfo, err := captureScreenshotJPEG(client, s.coordinateDebugScreen(), options.CropBlackBars)
	if err != nil {
		return nil, nil, err
	}
	if meta == nil || meta.PixelFormat != "jpeg" {
		return nil, nil, fmt.Errorf("expected jpeg frame from screen capture")
	}

	sourceWidth := int(meta.Width)
	sourceHeight := int(meta.Height)
	var sourceActive *screen.ScreenActiveArea
	if width, height, active, ok := frameMetadataSourceActiveArea(meta); ok {
		sourceWidth = width
		sourceHeight = height
		currentScreen := s.coordinateDebugScreen()
		if currentScreen != nil {
			currentScreen.UpdateActiveArea(sourceWidth, sourceHeight, active)
		}
		sourceActive = coordinateDebugSourceActiveArea(active, sourceWidth, sourceHeight)
	}
	display := coordinateDebugDisplayScreenshot(jpegData, int(meta.Width), int(meta.Height))
	applyScreenCaptureInfo(&display, captureInfo)
	result := s.newCoordinateDebugScreenshotResult(display, sourceWidth, sourceHeight, sourceActive)
	return result, jpegData, nil
}

func preferredPhoneScreenPixels(screen screen.PhoneScreenInfo) (int, int, bool) {
	if screen.NativeWidthPixels != nil && screen.NativeHeightPixels != nil &&
		*screen.NativeWidthPixels > 0 && *screen.NativeHeightPixels > 0 {
		return *screen.NativeWidthPixels, *screen.NativeHeightPixels, true
	}
	if screen.WidthPixels != nil && screen.HeightPixels != nil &&
		*screen.WidthPixels > 0 && *screen.HeightPixels > 0 {
		return *screen.WidthPixels, *screen.HeightPixels, true
	}
	return 0, 0, false
}

func (s *Server) captureCoordinateDebugScreenshotResult(options coordinateDebugScreenshotOptions) (*coordinateDebugScreenshotResult, error) {
	result, _, err := s.captureCoordinateDebugScreenshot(options)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func cropJPEGToActiveArea(jpegData []byte, active screen.ScreenActiveArea, quality int) ([]byte, int, int, error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode jpeg for crop: %w", err)
	}
	bounds := img.Bounds()
	if active.X < 0 || active.Y < 0 || active.Width <= 0 || active.Height <= 0 ||
		active.X+active.Width > bounds.Dx() || active.Y+active.Height > bounds.Dy() {
		return nil, 0, 0, fmt.Errorf("invalid active area for crop: %+v within %dx%d", active, bounds.Dx(), bounds.Dy())
	}

	rgba := image.NewRGBA(image.Rect(0, 0, active.Width, active.Height))
	for y := 0; y < active.Height; y++ {
		for x := 0; x < active.Width; x++ {
			rgba.Set(x, y, img.At(bounds.Min.X+active.X+x, bounds.Min.Y+active.Y+y))
		}
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgba, &jpeg.Options{Quality: quality}); err != nil {
		return nil, 0, 0, fmt.Errorf("encode cropped jpeg: %w", err)
	}
	return buf.Bytes(), active.Width, active.Height, nil
}

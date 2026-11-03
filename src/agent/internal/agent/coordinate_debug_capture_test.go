package agent

import (
	"errors"
	"strings"
	"testing"
	"unicode"

	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
)

type fakeScreenCaptureSource struct {
	latestFrameCalls           int
	latestFrameWithFormatCalls int
	latestFrameFn              func() (*frameMetadata, []byte, error)
	latestFrameWithFormatFn    func(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, error)
	lastCaptureInfo            screenCaptureInfo
}

func (f *fakeScreenCaptureSource) LatestFrame() (*frameMetadata, []byte, error) {
	f.latestFrameCalls++
	if f.latestFrameFn == nil {
		return nil, nil, errors.New("LatestFrame not configured")
	}
	return f.latestFrameFn()
}

func (f *fakeScreenCaptureSource) LatestFrameWithFormat(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, error) {
	f.latestFrameWithFormatCalls++
	if f.latestFrameWithFormatFn == nil {
		return nil, nil, errors.New("LatestFrameWithFormat not configured")
	}
	return f.latestFrameWithFormatFn(format, quality, cropBlack, hint)
}

func (f *fakeScreenCaptureSource) LastCaptureInfo() screenCaptureInfo {
	return cloneScreenCaptureInfo(f.lastCaptureInfo)
}

func TestCoordinateDebugScreenshotReusesSharedScreenCaptureClientFallbackState(t *testing.T) {
	primary := &fakeScreenCaptureSource{
		latestFrameWithFormatFn: func(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, error) {
			return nil, nil, errors.New("frame service: SERVICE_RECOVERING")
		},
	}
	fallback := &fakeScreenCaptureSource{
		latestFrameWithFormatFn: func(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, error) {
			return &frameMetadata{
				Seq:          1,
				Width:        2,
				Height:       1,
				SourceWidth:  2,
				SourceHeight: 1,
				CropWidth:    2,
				CropHeight:   1,
				PixelFormat:  "jpeg",
			}, []byte("jpeg"), nil
		},
		lastCaptureInfo: screenCaptureInfo{Backend: "adb"},
	}

	server := &Server{
		runtime:             &Runtime{config: Config{}},
		screenCaptureClient: newScreenCaptureClient(primary, fallback),
	}

	options := coordinateDebugScreenshotOptions{CropBlackBars: true}
	for i := 0; i < 2; i++ {
		result, jpegData, err := server.captureCoordinateDebugScreenshot(options)
		if err != nil {
			t.Fatalf("captureCoordinateDebugScreenshot() #%d error = %v", i+1, err)
		}
		if result == nil || result.CaptureBackend != "adb" {
			t.Fatalf("captureCoordinateDebugScreenshot() #%d backend = %#v, want adb", i+1, result)
		}
		if string(jpegData) != "jpeg" {
			t.Fatalf("captureCoordinateDebugScreenshot() #%d jpeg = %q, want jpeg", i+1, string(jpegData))
		}
	}

	if primary.latestFrameWithFormatCalls != 1 {
		t.Fatalf("primary calls = %d, want 1 after sticky fallback reuse", primary.latestFrameWithFormatCalls)
	}
	if fallback.latestFrameWithFormatCalls != 2 {
		t.Fatalf("fallback calls = %d, want 2", fallback.latestFrameWithFormatCalls)
	}
}

func TestProviderScreenshotUsesReportedPhoneWidthWhenRequestOmitsHint(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{
		WidthPixels:  intPtr(1200),
		HeightPixels: intPtr(2608),
	})

	provider := &fakeScreenshotFrameClient{
		meta: frameMetadata{
			Width:       498,
			Height:      1080,
			PixelFormat: "jpeg",
		},
		data: []byte("jpeg"),
	}
	server := &Server{
		runtime:             &Runtime{tools: &ToolSet{screen: screenState}},
		screenCaptureClient: provider,
	}

	rec := postProviderScreenshot(t, server, `{"format":"jpeg","quality":80,"crop_black":true,"minimal_width":0}`)
	if rec.Code != 200 {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if provider.cropHint.ScreenWidth != 1200 || provider.cropHint.ScreenHeight != 2608 {
		t.Fatalf("crop hint = %+v, want reported 1200x2608 screen", provider.cropHint)
	}
}

func TestCoordinateDebugHTMLUsesProviderScreenshot(t *testing.T) {
	if !strings.Contains(coordinateDebugHTML, "/api/providers/screenshot") {
		t.Fatal("coordinate debug page should load frames from POST /api/providers/screenshot")
	}
	if strings.Contains(coordinateDebugHTML, "/api/screenshot.jpg") {
		t.Fatal("coordinate debug page should not call /api/screenshot.jpg")
	}
}

func TestCoordinateDebugHTMLMatchesChatThemeAndLanguage(t *testing.T) {
	for _, expected := range []string{
		`<html lang="en">`,
		`href="/web-ui/styles.css"`,
		`class="topbar"`,
		`class="topbar-title"`,
		`class="new-chat-btn" href="/"`,
		`@media (max-width: 760px)`,
		`Capture device screen`,
		`Enter valid coordinates`,
		`Action failed:`,
	} {
		if !strings.Contains(coordinateDebugHTML, expected) {
			t.Errorf("coordinate debug page missing %q", expected)
		}
	}
	if strings.IndexFunc(coordinateDebugHTML, func(r rune) bool { return unicode.Is(unicode.Han, r) }) != -1 {
		t.Error("coordinate debug page still contains untranslated Chinese text")
	}
}

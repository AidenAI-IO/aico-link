package agent

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
)

type fakeScreenshotFrameClient struct {
	meta        frameMetadata
	data        []byte
	calls       int
	cropBlack   bool
	cropHint    screenprovider.CropHint
	captureInfo screenCaptureInfo
}

type fakeReadyScreenshotFrameClient struct {
	*fakeScreenshotFrameClient
	readyCalls int
	readyHint  screenprovider.CropHint
}

func (c *fakeReadyScreenshotFrameClient) LatestFrameWithFormatWhenReady(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	c.readyCalls++
	c.readyHint = hint
	return c.LatestFrameWithFormat(format, quality, cropBlack, hint)
}

func TestScreenshotToolWaitsForFrameServiceBeforeExplicitCapture(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{
		WidthPixels:  intPtr(1179),
		HeightPixels: intPtr(2556),
	})
	client := &fakeReadyScreenshotFrameClient{
		fakeScreenshotFrameClient: &fakeScreenshotFrameClient{
			meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
			data: jpegData,
		},
	}

	tool := &ScreenshotTool{client: client, screen: screenState}
	if _, err := tool.Call(context.Background(), "{}"); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if client.readyCalls != 1 || client.calls != 1 {
		t.Fatalf("ready calls=%d capture calls=%d, want 1 each", client.readyCalls, client.calls)
	}
	if client.readyHint.ScreenWidth != 1179 || client.readyHint.ScreenHeight != 2556 {
		t.Fatalf("ready crop hint = %+v, want 1179x2556", client.readyHint)
	}
}

func TestScreenshotToolSkipsStartupWaitForBackgroundCapture(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	client := &fakeReadyScreenshotFrameClient{
		fakeScreenshotFrameClient: &fakeScreenshotFrameClient{
			meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
			data: jpegData,
		},
	}

	tool := &ScreenshotTool{client: client, screen: &screen.ScreenState{}}
	if _, err := tool.CallWithoutStartupWait(context.Background(), false); err != nil {
		t.Fatalf("CallWithoutStartupWait() error = %v", err)
	}
	if client.readyCalls != 0 || client.calls != 1 {
		t.Fatalf("ready calls=%d capture calls=%d, want 0 and 1", client.readyCalls, client.calls)
	}
}

// The screenshot attached to a user turn follows the ui_grounding_enabled
// setting passed by the caller, never the model's input, and stays silent on
// a device without the detector.
func TestScreenshotToolBackgroundCaptureFollowsGroundingSetting(t *testing.T) {
	jpegData, err := encodeJPEG(bytes.Repeat([]byte{240, 240, 240}, 60*120), 60, 120, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	capture := func(grounder uiGrounder, ground bool) postActionScreenshotResult {
		t.Helper()
		tool := &ScreenshotTool{
			client:       &fakeScreenshotFrameClient{meta: frameMetadata{Width: 60, Height: 120, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))}, data: jpegData},
			screen:       &screen.ScreenState{},
			grounder:     grounder,
			deviceTypeFn: func() string { return "macos" },
		}
		output, err := tool.CallWithoutStartupWait(context.Background(), ground)
		if err != nil {
			t.Fatalf("CallWithoutStartupWait() error = %v", err)
		}
		var result postActionScreenshotResult
		if err := json.Unmarshal([]byte(output), &result); err != nil {
			t.Fatalf("decode %q: %v", output, err)
		}
		return result
	}
	box := []uiElementBox{{X1: 10, Y1: 20, X2: 50, Y2: 40, Score: 0.9}}

	off := &fakeUIGrounder{boxes: box}
	if result := capture(off, false); result.Grounding != nil || off.width != 0 {
		t.Fatalf("grounding off: result grounding = %+v, detector ran = %v", result.Grounding, off.width != 0)
	}
	if result := capture(&fakeUIGrounder{boxes: box}, true); result.Grounding == nil || len(result.Grounding.Elements) != 1 {
		t.Fatalf("grounding on: result grounding = %+v, want one element", result.Grounding)
	}
	if result := capture(nil, true); result.Grounding != nil {
		t.Fatalf("grounding on without a detector: grounding = %+v, want none", result.Grounding)
	}
}

func TestDetectImageAxisBoundsIgnoresInteriorDarkRun(t *testing.T) {
	left, right, valid := detectImageAxisBounds(40, func(position int) bool {
		return position < 2 || position >= 38 || position == 34
	})
	if left != 2 || right != 37 || !valid {
		t.Fatalf("bounds = (%d, %d), valid=%v, want (2, 37), true", left, right, valid)
	}
}

func (c *fakeScreenshotFrameClient) LatestFrameWithFormat(format string, quality int, cropBlack bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	if format != "jpeg" {
		return nil, nil, screenCaptureInfo{}, fmt.Errorf("unexpected format %q", format)
	}
	if quality != screenshotJPEGQuality {
		return nil, nil, screenCaptureInfo{}, fmt.Errorf("quality = %d, want %d", quality, screenshotJPEGQuality)
	}
	c.calls++
	c.cropBlack = cropBlack
	c.cropHint = hint
	meta := c.meta
	return &meta, append([]byte(nil), c.data...), cloneScreenCaptureInfo(c.captureInfo), nil
}

func TestScreenshotToolPassesReportedScreenWidthAt1080Height(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}

	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{
		WidthPixels:        intPtr(1179),
		HeightPixels:       intPtr(2556),
		NativeWidthPixels:  intPtr(1080),
		NativeHeightPixels: intPtr(2400),
	})
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
		data: jpegData,
	}

	tool := &ScreenshotTool{client: client, screen: screenState}
	if _, err := tool.Call(context.Background(), `{}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if client.cropHint.ScreenWidth != 1179 || client.cropHint.ScreenHeight != 2556 {
		t.Fatalf("crop hint = %+v, want current 1179x2556 screen", client.cropHint)
	}
	if !client.cropBlack {
		t.Fatal("crop_black = false, want true for default iOS device type")
	}
}

func TestScreenshotToolPassesLandscapeScreenDimensions(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{
		WidthPixels:  intPtr(2608),
		HeightPixels: intPtr(1200),
	})
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
		data: jpegData,
	}

	tool := &ScreenshotTool{client: client, screen: screenState}
	if _, err := tool.Call(context.Background(), `{}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if client.cropHint.ScreenWidth != 2608 || client.cropHint.ScreenHeight != 1200 {
		t.Fatalf("crop hint = %+v, want landscape 2608x1200 screen", client.cropHint)
	}
}

func TestScreenshotToolOmitsMinimalWidthWithoutReportedScreen(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
		data: jpegData,
	}

	tool := &ScreenshotTool{client: client, screen: &screen.ScreenState{}}
	if _, err := tool.Call(context.Background(), `{}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if client.cropHint != (screenprovider.CropHint{}) {
		t.Fatalf("crop hint = %+v, want empty", client.cropHint)
	}
}

func TestScreenshotToolDisablesBlackCropForDesktopDeviceTypes(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1080), HeightPixels: intPtr(1920)})
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
		data: jpegData,
	}
	tool := &ScreenshotTool{client: client, screen: screenState, deviceTypeFn: func() string { return "macOS" }}

	if _, err := tool.Call(context.Background(), `{}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if client.cropBlack {
		t.Fatal("crop_black = true, want false for macOS")
	}
	if client.cropHint != (screenprovider.CropHint{}) {
		t.Fatalf("crop hint = %+v, want empty when cropping is disabled", client.cropHint)
	}
}

func TestScreenshotToolEnablesBlackCropForAndroid(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{255, 255, 255}, 1, 1, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{Width: 1, Height: 1, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
		data: jpegData,
	}
	tool := &ScreenshotTool{client: client, screen: &screen.ScreenState{}, deviceTypeFn: func() string { return "Android" }}

	if _, err := tool.Call(context.Background(), `{}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if !client.cropBlack {
		t.Fatal("crop_black = false, want true for Android")
	}
}

func TestScreenshotToolUsesJPEGSourceMetadataForSharedScreenState(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{
		255, 255, 255, 255, 255, 255,
		255, 255, 255, 255, 255, 255,
	}, 2, 2, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}

	screenState := &screen.ScreenState{}
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{
			Seq:          99,
			Width:        2,
			Height:       2,
			SourceWidth:  16,
			SourceHeight: 9,
			CropX:        5,
			CropY:        0,
			CropWidth:    5,
			CropHeight:   9,
			PixelFormat:  "jpeg",
			Bytes:        uint64(len(jpegData)),
		},
		data: jpegData,
		captureInfo: screenCaptureInfo{
			Backend: "adb",
			ADBDevice: &adbDeviceInfo{
				Serial: "serial123",
				Name:   "Pixel 9",
				State:  "device",
			},
		},
	}
	tool := &ScreenshotTool{
		client: client,
		screen: screenState,
	}

	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid screenshot JSON: %v", err)
	}
	if result.ActionOutput != "ok" {
		t.Fatalf("action_output = %q, want ok", result.ActionOutput)
	}
	if result.Width != 2 || result.Height != 2 || result.Format != "jpeg" || result.Size != len(jpegData) {
		t.Fatalf("unexpected screenshot metadata: %#v", result)
	}
	want := screen.ScreenActiveArea{X: 5, Y: 0, Width: 5, Height: 9, Valid: true}
	if result.SourceWidth != 16 || result.SourceHeight != 9 || result.ActiveArea == nil || *result.ActiveArea != want || result.ActiveWidth != want.Width || result.ActiveHeight != want.Height {
		t.Fatalf("unexpected source mapping metadata: %#v", result)
	}
	if result.CaptureBackend != "adb" {
		t.Fatalf("capture backend = %q, want adb", result.CaptureBackend)
	}
	if result.ADBDevice == nil || result.ADBDevice.Serial != "serial123" || result.ADBDevice.Name != "Pixel 9" || result.ADBDevice.State != "device" {
		t.Fatalf("unexpected adb device info: %#v", result.ADBDevice)
	}
	if result.Data != base64.StdEncoding.EncodeToString(jpegData) {
		t.Fatalf("unexpected screenshot data: %q", result.Data)
	}
	if client.calls != 1 {
		t.Fatalf("LatestFrameWithFormat call count = %d, want 1", client.calls)
	}

	width, height, active, _, ok := screenState.ActiveAreaWithAge()
	if !ok || width != 16 || height != 9 {
		t.Fatalf("screen dimensions = %dx%d ok=%v, want 16x9 true", width, height, ok)
	}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
	latest, latestWidth, latestHeight, _, ok := screenState.LatestScreenshot(screenDimensionsStaleAfter)
	if !ok || latestWidth != 2 || latestHeight != 2 || !bytes.Equal(latest, jpegData) {
		t.Fatalf("latest screenshot = %dx%d bytes=%d ok=%v, want 2x2 bytes=%d true", latestWidth, latestHeight, len(latest), ok, len(jpegData))
	}
}

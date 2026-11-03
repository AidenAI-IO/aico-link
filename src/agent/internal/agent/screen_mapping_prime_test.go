package agent

import (
	"aico-agent/internal/agent/screen"
	"context"
	"errors"
	"testing"

	langtools "github.com/tmc/langchaingo/tools"
)

func TestToolSetPrimeScreenMappingCapturesScreenshotMetadata(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{
		255, 255, 255, 255, 255, 255,
		255, 255, 255, 255, 255, 255,
	}, 2, 2, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}

	currentScreen := &screen.ScreenState{}
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{
			Seq:          42,
			Width:        2,
			Height:       2,
			SourceWidth:  1920,
			SourceHeight: 1080,
			CropX:        711,
			CropY:        0,
			CropWidth:    497,
			CropHeight:   1080,
			PixelFormat:  "jpeg",
			Bytes:        uint64(len(jpegData)),
		},
		data: jpegData,
	}
	tools := &ToolSet{
		tools: map[string]langtools.Tool{
			"screenshot": &ScreenshotTool{client: client, screen: currentScreen},
		},
		screen: currentScreen,
	}

	if err := tools.PrimeScreenMapping(context.Background()); err != nil {
		t.Fatalf("PrimeScreenMapping() error = %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("LatestFrameWithFormat calls = %d, want 1", client.calls)
	}
	width, height, active, _, ok := currentScreen.ActiveAreaWithAge()
	if !ok {
		t.Fatal("screen mapping was not established")
	}
	if width != 1920 || height != 1080 {
		t.Fatalf("source dimensions = %dx%d, want 1920x1080", width, height)
	}
	wantActive := screen.ScreenActiveArea{X: 711, Y: 0, Width: 497, Height: 1080, Valid: true}
	if active != wantActive {
		t.Fatalf("active area = %+v, want %+v", active, wantActive)
	}
}

func TestToolSetPrimeScreenMappingTreatsFullFrameFallbackAsFresh(t *testing.T) {
	jpegData, err := encodeJPEG([]byte{
		255, 255, 255, 255, 255, 255,
		255, 255, 255, 255, 255, 255,
	}, 2, 2, screenshotJPEGQuality)
	if err != nil {
		t.Fatalf("encodeJPEG() error = %v", err)
	}

	currentScreen := &screen.ScreenState{}
	client := &fakeScreenshotFrameClient{
		meta: frameMetadata{
			Seq:         43,
			Width:       2,
			Height:      2,
			PixelFormat: "jpeg",
			Bytes:       uint64(len(jpegData)),
		},
		data: jpegData,
	}
	tools := &ToolSet{
		tools: map[string]langtools.Tool{
			"screenshot": &ScreenshotTool{client: client, screen: currentScreen},
		},
		screen: currentScreen,
	}

	if err := tools.PrimeScreenMapping(context.Background()); err != nil {
		t.Fatalf("PrimeScreenMapping() error = %v", err)
	}
	width, height, active, _, ok := currentScreen.ActiveAreaWithAge()
	if !ok {
		t.Fatal("screen mapping was not established")
	}
	if width != 2 || height != 2 {
		t.Fatalf("source dimensions = %dx%d, want 2x2", width, height)
	}
	wantActive := screen.ScreenActiveArea{X: 0, Y: 0, Width: 2, Height: 2, Valid: true}
	if active != wantActive {
		t.Fatalf("active area = %+v, want %+v", active, wantActive)
	}
	if !currentScreen.FreshActiveArea(screenDimensionsStaleAfter) {
		t.Fatal("expected full-frame fallback mapping to be fresh after successful prime")
	}
}

func TestToolSetPrimeScreenMappingFailureKeepsFreshFullFrameFallback(t *testing.T) {
	currentScreen := &screen.ScreenState{}
	currentScreen.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{})
	tools := &ToolSet{
		tools: map[string]langtools.Tool{
			"screenshot": failingPrimeScreenshotTool{},
		},
		screen: currentScreen,
	}

	if err := tools.PrimeScreenMapping(context.Background()); err == nil {
		t.Fatal("expected PrimeScreenMapping() error")
	}
	if !currentScreen.FreshActiveArea(screenDimensionsStaleAfter) {
		t.Fatal("expected fresh full-frame fallback mapping after capture failure")
	}
}

type failingPrimeScreenshotTool struct{}

func (failingPrimeScreenshotTool) Name() string { return "screenshot" }

func (failingPrimeScreenshotTool) Description() string { return "failing screenshot" }

func (failingPrimeScreenshotTool) Call(context.Context, string) (string, error) {
	return "", errors.New("capture failed")
}

package agent

import (
	"aico-agent/internal/agent/screen"
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func TestEncodeFrameAsJPEGPassthroughForJPEG(t *testing.T) {
	srcImage := image.NewRGBA(image.Rect(0, 0, 2, 1))
	srcImage.Set(0, 0, color.RGBA{R: 255, A: 255})
	srcImage.Set(1, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, srcImage, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatalf("encode source jpeg: %v", err)
	}

	meta := &frameMetadata{Width: 2, Height: 1, PixelFormat: "jpeg"}
	out, err := encodeFrameAsJPEG(meta, buf.Bytes(), 80)
	if err != nil {
		t.Fatalf("encodeFrameAsJPEG: %v", err)
	}
	if !bytes.Equal(out, buf.Bytes()) {
		t.Fatal("expected jpeg input to pass through unchanged")
	}
}

func TestConvertFrameToRGBSupportsPNG(t *testing.T) {
	srcImage := image.NewRGBA(image.Rect(0, 0, 2, 1))
	srcImage.Set(0, 0, color.RGBA{R: 255, A: 255})
	srcImage.Set(1, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, srcImage); err != nil {
		t.Fatalf("encode source png: %v", err)
	}

	meta := &frameMetadata{Width: 2, Height: 1, PixelFormat: "png"}
	rgb, err := convertFrameToRGB(meta, buf.Bytes())
	if err != nil {
		t.Fatalf("convertFrameToRGB: %v", err)
	}
	want := []byte{255, 0, 0, 0, 255, 0}
	if !bytes.Equal(rgb, want) {
		t.Fatalf("rgb = %v, want %v", rgb, want)
	}
}

func TestEncodeFrameAsJPEGConvertsPNG(t *testing.T) {
	srcImage := image.NewRGBA(image.Rect(0, 0, 2, 1))
	srcImage.Set(0, 0, color.RGBA{R: 255, A: 255})
	srcImage.Set(1, 0, color.RGBA{G: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, srcImage); err != nil {
		t.Fatalf("encode source png: %v", err)
	}

	meta := &frameMetadata{Width: 2, Height: 1, PixelFormat: "png"}
	out, err := encodeFrameAsJPEG(meta, buf.Bytes(), 80)
	if err != nil {
		t.Fatalf("encodeFrameAsJPEG: %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode converted jpeg: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("decoded bounds = %v, want 2x1", bounds)
	}
}

func TestCropJPEGToActiveArea(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 40), G: uint8(y * 40), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 100}); err != nil {
		t.Fatalf("encode source jpeg: %v", err)
	}

	cropped, width, height, err := cropJPEGToActiveArea(buf.Bytes(), screen.ScreenActiveArea{X: 1, Y: 1, Width: 2, Height: 2, Valid: true}, 100)
	if err != nil {
		t.Fatalf("cropJPEGToActiveArea: %v", err)
	}
	if width != 2 || height != 2 {
		t.Fatalf("crop size = %dx%d, want 2x2", width, height)
	}

	decoded, err := jpeg.Decode(bytes.NewReader(cropped))
	if err != nil {
		t.Fatalf("decode cropped jpeg: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 2 {
		t.Fatalf("decoded bounds = %v, want 2x2", bounds)
	}
}

func TestDeriveActiveAreaFromPhoneScreenUsesReportedAspectRatio(t *testing.T) {
	approx := screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true}
	active, ok := deriveActiveAreaFromPhoneScreen(1920, 1080, screen.PhoneScreenInfo{
		WidthPixels:  intPtr(1080),
		HeightPixels: intPtr(1920),
	}, approx)
	if !ok {
		t.Fatal("expected aspect-ratio derived active area")
	}
	want := screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestDeriveActiveAreaFromNativePhoneScreenUsesApproxToChooseOrientation(t *testing.T) {
	approx := screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true}
	active, ok := deriveActiveAreaFromPhoneScreen(1920, 1080, screen.PhoneScreenInfo{
		NativeWidthPixels:  intPtr(1920),
		NativeHeightPixels: intPtr(1080),
	}, approx)
	if !ok {
		t.Fatal("expected active area derived from native screen dimensions")
	}
	want := screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestDeriveActiveAreaFromPhoneScreenConsidersNativeDimensionsAlongsideCurrent(t *testing.T) {
	approx := screen.ScreenActiveArea{X: 717, Y: 0, Width: 486, Height: 1080, Valid: true}
	active, ok := deriveActiveAreaFromPhoneScreen(1920, 1080, screen.PhoneScreenInfo{
		WidthPixels:        intPtr(1080),
		HeightPixels:       intPtr(1920),
		NativeWidthPixels:  intPtr(1080),
		NativeHeightPixels: intPtr(2400),
	}, approx)
	if !ok {
		t.Fatal("expected active area derived from current and native screen dimensions")
	}
	want := screen.ScreenActiveArea{X: 717, Y: 0, Width: 486, Height: 1080, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestDeriveActiveAreaFromReportedPhoneScreenCanCropRows(t *testing.T) {
	approx := screen.ScreenActiveArea{X: 0, Y: 97, Width: 1920, Height: 886, Valid: true}
	active, ok := deriveActiveAreaFromPhoneScreen(1920, 1080, screen.PhoneScreenInfo{
		WidthPixels:  intPtr(1080),
		HeightPixels: intPtr(2340),
	}, approx)
	if !ok {
		t.Fatal("expected vertical active area candidate")
	}
	if active != approx {
		t.Fatalf("active area = %+v, want %+v", active, approx)
	}
}

func TestDetectImageActiveAreaChoosesAxisWithLargerRemovedFraction(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 8))
	for y := 2; y < 6; y++ {
		for x := 2; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}

	active := detectImageActiveArea(img, 10, 8)
	want := screen.ScreenActiveArea{X: 0, Y: 2, Width: 10, Height: 4, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestDetectImageActiveAreaChoosesHorizontalWhenMoreColumnsAreRemoved(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 8))
	for y := 1; y < 7; y++ {
		for x := 3; x < 7; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}

	active := detectImageActiveArea(img, 10, 8)
	want := screen.ScreenActiveArea{X: 3, Y: 0, Width: 4, Height: 8, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

func TestDetectImageActiveAreaCropsRowsWhenWidthIsFull(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 2; y < 8; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}

	active := detectImageActiveArea(img, 10, 10)
	want := screen.ScreenActiveArea{X: 0, Y: 2, Width: 10, Height: 6, Valid: true}
	if active != want {
		t.Fatalf("active area = %+v, want %+v", active, want)
	}
}

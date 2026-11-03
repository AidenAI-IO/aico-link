package screenprovider

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestADBScreenClientCapturesPNGAndJPEG(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script uses /bin/sh")
	}

	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}

	tmpDir := t.TempDir()
	pngPath := filepath.Join(tmpDir, "screen.png")
	if err := os.WriteFile(pngPath, pngBuf.Bytes(), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", pngPath, err)
	}

	adbPath := filepath.Join(tmpDir, "adb")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"devices\" ] && [ \"$2\" = \"-l\" ]; then\n" +
		"  printf 'List of devices attached\\nserial123\\tdevice product:panther model:Pixel_7_Pro device:panther transport_id:1\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"if [ \"$1\" = \"-s\" ] && [ \"$2\" = \"serial123\" ] && [ \"$3\" = \"exec-out\" ] && [ \"$4\" = \"screencap\" ] && [ \"$5\" = \"-p\" ]; then\n" +
		"  cat \"$AICO_TEST_PNG\"\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"unexpected args: $@\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(adbPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", adbPath, err)
	}

	t.Setenv("AICO_ADB_PATH", adbPath)
	t.Setenv("AICO_TEST_PNG", pngPath)
	t.Setenv("AICO_ADB_SERIAL", "")
	t.Setenv("ANDROID_SERIAL", "")
	defer ClearAutoConfiguredSerial("serial123")

	client := NewADB()

	rawMeta, rawFrame, err := client.LatestFrame()
	if err != nil {
		t.Fatalf("LatestFrame() error = %v", err)
	}
	if rawMeta == nil || rawMeta.PixelFormat != "png" || rawMeta.Width != 2 || rawMeta.Height != 1 {
		t.Fatalf("unexpected raw metadata: %#v", rawMeta)
	}
	if !bytes.Equal(rawFrame, pngBuf.Bytes()) {
		t.Fatal("raw adb frame does not match source png")
	}

	pngMeta, pngFrame, err := client.LatestFrameWithFormat("png", DefaultJPEGQuality, false, CropHint{})
	if err != nil {
		t.Fatalf("LatestFrameWithFormat(png) error = %v", err)
	}
	if pngMeta == nil || pngMeta.PixelFormat != "png" || pngMeta.Width != 2 || pngMeta.Height != 1 {
		t.Fatalf("unexpected png metadata: %#v", pngMeta)
	}
	if !bytes.Equal(pngFrame, pngBuf.Bytes()) {
		t.Fatal("png adb frame does not match source png")
	}

	jpegMeta, jpegFrame, err := client.LatestFrameWithFormat("jpeg", DefaultJPEGQuality, true, CropHint{})
	if err != nil {
		t.Fatalf("LatestFrameWithFormat() error = %v", err)
	}
	if jpegMeta == nil || jpegMeta.PixelFormat != "jpeg" || jpegMeta.Width != 2 || jpegMeta.Height != 1 {
		t.Fatalf("unexpected jpeg metadata: %#v", jpegMeta)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(jpegFrame))
	if err != nil {
		t.Fatalf("jpeg.Decode() error = %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 2 || bounds.Dy() != 1 {
		t.Fatalf("decoded jpeg bounds = %v, want 2x1", bounds)
	}

	info := client.LastCaptureInfo()
	if info.Backend != "adb" {
		t.Fatalf("capture backend = %q, want adb", info.Backend)
	}
	if info.ADBDevice == nil {
		t.Fatal("expected adb device info")
	}
	if info.ADBDevice.Serial != "serial123" {
		t.Fatalf("adb serial = %q, want serial123", info.ADBDevice.Serial)
	}
	if info.ADBDevice.Name != "Pixel 7 Pro" {
		t.Fatalf("adb device name = %q, want Pixel 7 Pro", info.ADBDevice.Name)
	}
	if info.ADBDevice.State != "device" {
		t.Fatalf("adb device state = %q, want device", info.ADBDevice.State)
	}
}

func TestADBScreenClientAutoSetsAICOADBSerialForUniqueDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test shell script uses /bin/sh")
	}

	tmpDir := t.TempDir()
	adbPath := filepath.Join(tmpDir, "adb")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"devices\" ] && [ \"$2\" = \"-l\" ]; then\n" +
		"  printf 'List of devices attached\\nserial123\\tdevice product:panther model:Pixel_7_Pro device:panther transport_id:1\\n'\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo \"unexpected args: $@\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(adbPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", adbPath, err)
	}

	t.Setenv("AICO_ADB_PATH", adbPath)
	t.Setenv("AICO_ADB_SERIAL", "")
	t.Setenv("ANDROID_SERIAL", "")
	defer ClearAutoConfiguredSerial("serial123")

	client := NewADB()
	serial, err := client.ResolveSerial(context.Background(), adbPath)
	if err != nil {
		t.Fatalf("ResolveSerial() error = %v", err)
	}
	if serial != "serial123" {
		t.Fatalf("ResolveSerial() = %q, want serial123", serial)
	}
	if got := os.Getenv("AICO_ADB_SERIAL"); got != "serial123" {
		t.Fatalf("AICO_ADB_SERIAL = %q, want serial123", got)
	}

	if err := os.Unsetenv("AICO_ADB_SERIAL"); err != nil {
		t.Fatalf("Unsetenv(AICO_ADB_SERIAL) error = %v", err)
	}
	serial, err = client.ResolveSerial(context.Background(), adbPath)
	if err != nil {
		t.Fatalf("ResolveSerial() cached error = %v", err)
	}
	if serial != "serial123" {
		t.Fatalf("ResolveSerial() cached = %q, want serial123", serial)
	}
	if got := os.Getenv("AICO_ADB_SERIAL"); got != "serial123" {
		t.Fatalf("cached resolve AICO_ADB_SERIAL = %q, want serial123", got)
	}

	client.InvalidateAutoSerial("serial123")
	if got := os.Getenv("AICO_ADB_SERIAL"); got != "" {
		t.Fatalf("AICO_ADB_SERIAL after invalidate = %q, want empty", got)
	}
}

func TestADBScreenClientDoesNotOverrideConfiguredADBSerial(t *testing.T) {
	t.Setenv("AICO_ADB_SERIAL", "manual123")
	t.Setenv("ANDROID_SERIAL", "")

	if err := SetAutoConfiguredSerial("auto123"); err != nil {
		t.Fatalf("SetAutoConfiguredSerial() error = %v", err)
	}
	if got := os.Getenv("AICO_ADB_SERIAL"); got != "manual123" {
		t.Fatalf("AICO_ADB_SERIAL = %q, want manual123", got)
	}
}

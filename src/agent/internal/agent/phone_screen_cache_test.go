package agent

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
)

// darkPhoneFrame is a 1920x1080 capture with a portrait phone in the middle
// whose page is mostly black, as a dark Safari page is: only a light bar at the
// bottom is clearly brighter than the letterbox.
func darkPhoneFrame(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	for y := 0; y < 1080; y++ {
		for x := 0; x < 1920; x++ {
			c := color.RGBA{A: 255}
			if x >= 711 && x < 1209 {
				c = color.RGBA{R: 12, G: 12, B: 12, A: 255}
				if y > 980 {
					c = color.RGBA{R: 200, G: 200, B: 200, A: 255}
				}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func iphoneScreen() screen.PhoneScreenInfo {
	w, h := 1179, 2556
	return screen.PhoneScreenInfo{WidthPixels: &w, HeightPixels: &h}
}

// cachedState returns a screen state with a fresh cache in dir.
func cachedState(dir string, deviceType *string) *screen.ScreenState {
	state := &screen.ScreenState{}
	state.SetPhoneScreenFallback(newPhoneScreenCache(dir, func() string { return *deviceType }))
	return state
}

func TestPhoneScreenCacheRememberAndLookup(t *testing.T) {
	deviceType := "iOS"
	cache := newPhoneScreenCache(t.TempDir(), func() string { return deviceType })
	cache.Remember(iphoneScreen(), 1920, 1080)
	if _, ok := cache.Lookup(1920, 1080); !ok {
		t.Fatal("remembered screen not found for the same frame size")
	}
	if _, ok := cache.Lookup(1280, 720); ok {
		t.Fatal("remembered screen must not apply to another frame size")
	}
	deviceType = "Android"
	if _, ok := cache.Lookup(1920, 1080); ok {
		t.Fatal("remembered screen must not apply to another device type")
	}
	if newPhoneScreenCache("", nil) != nil {
		t.Fatal("no config directory must mean no cache")
	}
}

func TestPhoneScreenCachePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ios := func() string { return "iOS" }
	newPhoneScreenCache(dir, ios).Remember(iphoneScreen(), 1920, 1080)
	got, ok := newPhoneScreenCache(dir, ios).Lookup(1920, 1080)
	if !ok || got.WidthPixels == nil || *got.WidthPixels != 1179 {
		t.Fatalf("lookup after restart = %+v, %v", got, ok)
	}
}

func TestScreenshotActiveAreaUsesRememberedScreenWithoutLiveReport(t *testing.T) {
	dir, deviceType := t.TempDir(), "iOS"
	frame := darkPhoneFrame(t)
	live := cachedState(dir, &deviceType)
	live.UpdatePhoneScreenInfo(iphoneScreen())
	want := detectScreenshotActiveAreaForScreen(live, frame, 1920, 1080)
	if !want.Valid {
		t.Fatal("live report did not map onto the frame")
	}
	// The app is gone and the Agent restarted: no live report.
	got := detectScreenshotActiveAreaForScreen(cachedState(dir, &deviceType), frame, 1920, 1080)
	if got != want {
		t.Fatalf("active area without a live report = %+v, want the remembered mapping %+v", got, want)
	}
}

func TestCaptureCropHintFallsBackToRememberedScreen(t *testing.T) {
	dir, deviceType := t.TempDir(), "iOS"
	var hints []screenprovider.CropHint
	frameWidth := uint32(1920)
	capture := func(hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
		hints = append(hints, hint)
		return &frameMetadata{SourceWidth: frameWidth, SourceHeight: 1080, CropWidth: 496, CropHeight: 1080}, nil, screenCaptureInfo{}, nil
	}
	live := cachedState(dir, &deviceType)
	live.UpdatePhoneScreenInfo(iphoneScreen())
	if _, _, _, err := captureWithCropHint(live, true, capture); err != nil {
		t.Fatal(err)
	}
	// The app is gone and the Agent restarted: the remembered size is sent.
	if _, _, _, err := captureWithCropHint(cachedState(dir, &deviceType), true, capture); err != nil {
		t.Fatal(err)
	}
	if len(hints) != 2 || hints[1] != hints[0] || hints[1].ScreenWidth != 1179 {
		t.Fatalf("hints = %+v, want the live hint reused", hints)
	}
	// A different capture frame size discards the remembered hint.
	hints, frameWidth = nil, 1280
	if _, _, _, err := captureWithCropHint(cachedState(dir, &deviceType), true, capture); err != nil {
		t.Fatal(err)
	}
	if len(hints) != 2 || hints[1] != (screenprovider.CropHint{}) {
		t.Fatalf("hints = %+v, want a retry without a hint", hints)
	}
}

func TestPhoneScreenCacheWritesOnReportBeforeAnyCapture(t *testing.T) {
	dir, deviceType := t.TempDir(), "iOS"
	// The app reports and is closed before any screenshot is taken.
	tools := &ToolSet{screen: cachedState(dir, &deviceType)}
	tools.UpdateDeviceEnvironment(&PhoneEnvironment{Screen: iphoneScreen()})

	restarted := cachedState(dir, &deviceType)
	var hints []screenprovider.CropHint
	capture := func(hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
		hints = append(hints, hint)
		return &frameMetadata{SourceWidth: 1920, SourceHeight: 1080, CropWidth: 496, CropHeight: 1080}, nil, screenCaptureInfo{}, nil
	}
	if _, _, _, err := captureWithCropHint(restarted, true, capture); err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0].ScreenWidth != 1179 {
		t.Fatalf("hints = %+v, want the reported size on the first capture", hints)
	}
	if _, ok := restarted.PhoneScreenFallback().Lookup(1920, 1080); !ok {
		t.Fatal("the first capture must complete the entry with its frame size")
	}
}

// cropJPEG encodes a cropped phone screen of the given size: a light page, or a
// black page with only a small bright block in the middle, as an over-crop of
// a dark page ends on content.
func cropJPEG(t *testing.T, width, height int, light bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			c := color.RGBA{R: 240, G: 240, B: 240, A: 255}
			if !light {
				c = color.RGBA{A: 255}
				if y > height*2/5 && y < height*3/5 {
					c = color.RGBA{R: 220, G: 220, B: 220, A: 255}
				}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// darkWallpaperJPEG encodes a dark home screen whose wallpaper varies between
// dark shades, as measured on a real iPhone capture (edge median about 30).
func darkWallpaperJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := uint8(22 + (x*7+y*13)%30)
			img.Set(x, y, color.RGBA{R: v, G: v + 4, B: v + 10, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCropEdgesOnScreen(t *testing.T) {
	portrait := &frameMetadata{SourceWidth: 1920, SourceHeight: 1080, CropWidth: 498, CropHeight: 1080}
	if !cropEdgesOnScreen(portrait, cropJPEG(t, 498, 1080, true)) {
		t.Fatal("a light screen's cut edges lie on the screen edge")
	}
	if !cropEdgesOnScreen(portrait, darkWallpaperJPEG(t, 498, 1080)) {
		t.Fatal("a dark but not near-black wallpaper's cut edges lie on the screen edge")
	}
	if cropEdgesOnScreen(portrait, cropJPEG(t, 498, 1080, false)) {
		t.Fatal("an edge that only touches a small bright block is not the screen edge")
	}
	landscape := &frameMetadata{SourceWidth: 1920, SourceHeight: 1080, CropWidth: 1920, CropHeight: 886}
	if !cropEdgesOnScreen(landscape, cropJPEG(t, 1920, 886, true)) {
		t.Fatal("a landscape crop checks its top and bottom edges")
	}
	full := &frameMetadata{SourceWidth: 1920, SourceHeight: 1080, CropWidth: 1920, CropHeight: 1080}
	if cropEdgesOnScreen(full, cropJPEG(t, 1920, 1080, true)) {
		t.Fatal("a crop that cut nothing must not be trusted")
	}
	if cropEdgesOnScreen(portrait, nil) {
		t.Fatal("undecodable data must not be trusted")
	}
}

func TestPhoneScreenCacheReportIgnoresFrameOfAnotherDeviceType(t *testing.T) {
	deviceType := "iOS"
	cache := newPhoneScreenCache(t.TempDir(), func() string { return deviceType })
	// The iPhone's capture frame is the latest one seen.
	cache.NoteFrame(1280, 720)
	// An Android phone replaces it and reports before any capture.
	deviceType = "Android"
	w, h := 1080, 2400
	cache.Remember(screen.PhoneScreenInfo{WidthPixels: &w, HeightPixels: &h}, 0, 0)
	if _, ok := cache.Lookup(1280, 720); ok {
		t.Fatal("the report must not take the previous device type's frame size")
	}
	// Its first capture completes the entry with its own frame size.
	cache.NoteFrame(1920, 1080)
	if info, ok := cache.Lookup(1920, 1080); !ok || *info.WidthPixels != 1080 {
		t.Fatalf("lookup = %+v, %v, want the report completed with 1920x1080", info, ok)
	}
}

func TestPhoneScreenCacheLearnsOnlyVerifiedCrops(t *testing.T) {
	cache := newPhoneScreenCache(t.TempDir(), func() string { return "iOS" })
	observe := func(w, h int, verified bool, times int) {
		for i := 0; i < times; i++ {
			cache.ObserveCrop(w, h, 1920, 1080, verified)
		}
	}
	size := func() [2]int {
		info, ok := cache.Lookup(1920, 1080)
		if !ok {
			return [2]int{}
		}
		return [2]int{*info.WidthPixels, *info.HeightPixels}
	}

	// A dark page over-crops; however often it repeats, it is never learned.
	observe(470, 1080, false, 10)
	if size() != [2]int{} {
		t.Fatalf("size = %v, an unverified crop must not be learned", size())
	}
	observe(498, 1080, true, 1)
	if size() != [2]int{} {
		t.Fatal("one verified crop must not be enough")
	}
	observe(498, 1080, true, 1)
	if size() != [2]int{498, 1080} {
		t.Fatalf("size = %v, want 498x1080 after two verified crops", size())
	}
	// Turning the phone keeps the aspect ratio: the entry stays.
	observe(1920, 886, true, 3)
	if size() != [2]int{498, 1080} {
		t.Fatalf("size = %v, a rotated crop with the same aspect must not replace the entry", size())
	}
	// A dark landscape page over-crops to a wider aspect; it is not verified.
	observe(1920, 830, false, 5)
	if size() != [2]int{498, 1080} {
		t.Fatalf("size = %v, an unverified landscape crop must not replace the entry", size())
	}
	// An implausible aspect is never learned, even when verified.
	fresh := newPhoneScreenCache(t.TempDir(), func() string { return "iOS" })
	for i := 0; i < 3; i++ {
		fresh.ObserveCrop(436, 1080, 1920, 1080, true)
	}
	if _, ok := fresh.Lookup(1920, 1080); ok {
		t.Fatal("a 436x1080 crop is narrower than any phone and must not be learned")
	}
}

func TestPhoneScreenCacheVerifiedCropReplacesAnotherPhone(t *testing.T) {
	cache := newPhoneScreenCache(t.TempDir(), func() string { return "iOS" })
	cache.Remember(iphoneScreen(), 1920, 1080)
	// The same phone, seen by detection: the reported size is kept.
	for i := 0; i < 3; i++ {
		cache.ObserveCrop(498, 1080, 1920, 1080, true)
	}
	if info, _ := cache.Lookup(1920, 1080); *info.WidthPixels != 1179 {
		t.Fatalf("width = %d, a matching detection must keep the reported size", *info.WidthPixels)
	}
	// A 16:9 phone is plugged in while its app is closed.
	for i := 0; i < 2; i++ {
		cache.ObserveCrop(608, 1080, 1920, 1080, true)
	}
	if info, _ := cache.Lookup(1920, 1080); *info.WidthPixels != 608 {
		t.Fatalf("width = %d, a verified crop of another aspect must replace the entry", *info.WidthPixels)
	}
}

func TestCaptureLearnsVerifiedCropWithoutApp(t *testing.T) {
	dir, deviceType := t.TempDir(), "iOS"
	light := cropJPEG(t, 498, 1080, true)
	var hints []screenprovider.CropHint
	capture := func(hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
		hints = append(hints, hint)
		return &frameMetadata{SourceWidth: 1920, SourceHeight: 1080, CropWidth: 498, CropHeight: 1080}, light, screenCaptureInfo{}, nil
	}
	state := cachedState(dir, &deviceType)
	for i := 0; i < 2; i++ {
		if _, _, _, err := captureWithCropHint(state, true, capture); err != nil {
			t.Fatal(err)
		}
	}
	// The app never ran; after a restart the learned size is the hint.
	hints = nil
	if _, _, _, err := captureWithCropHint(cachedState(dir, &deviceType), true, capture); err != nil {
		t.Fatal(err)
	}
	if len(hints) != 1 || hints[0] != (screenprovider.CropHint{ScreenWidth: 498, ScreenHeight: 1080}) {
		t.Fatalf("hints = %+v, want the learned crop as the hint", hints)
	}
}

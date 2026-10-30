package agent

import (
	"context"
	"image"
	"testing"
	"time"

	"aico-agent/internal/agent/screenprovider"
)

// stableTestFrame is an 80x160 luminance frame with rect painted at value.
func stableTestFrame(rect image.Rectangle, value byte) lumaFrame {
	frame := lumaFrame{width: 80, height: 160, stride: 80, y: make([]byte, 80*160)}
	for i := range frame.y {
		frame.y[i] = 40
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			frame.y[y*80+x] = value
		}
	}
	return frame
}

func stableTestMeans(rect image.Rectangle, value byte) lumaBlockMeans {
	return newLumaBlockMeans(stableTestFrame(rect, value), image.Rect(0, 0, 80, 160))
}

func TestStableMotionTrackerQuietOnStaticScreen(t *testing.T) {
	tracker := stableMotionTracker{threshold: defaultDiffThreshold}
	still := stableTestMeans(image.Rectangle{}, 0)
	if got := tracker.compare(still, still); !got.quiet() || got.anyChanged {
		t.Fatalf("comparison = %+v, want quiet and unchanged", got)
	}
}

// A small area that changes in every frame (a playing video tile) stops
// counting after three comparisons, so the wait can end.
func TestStableMotionTrackerIgnoresSmallContinuousMotion(t *testing.T) {
	tracker := stableMotionTracker{threshold: defaultDiffThreshold}
	video := image.Rect(10, 40, 40, 70) // about 7% of the screen
	previous := stableTestMeans(video, 100)
	var got stableMotionComparison
	for i := 0; i < stableMotionPersistentComparisons; i++ {
		current := stableTestMeans(video, byte(100+40*((i+1)%2)))
		got = tracker.compare(previous, current)
		previous = current
		if i < stableMotionPersistentComparisons-1 && got.quiet() {
			t.Fatalf("comparison %d quiet before the motion was persistent: %+v", i+1, got)
		}
	}
	if !got.quiet() || got.ignored == 0 || !got.anyChanged {
		t.Fatalf("comparison = %+v, want quiet with the video area ignored", got)
	}

	// A new change elsewhere still breaks the quiet.
	elsewhere := stableTestFrame(video, byte(100+40*((stableMotionPersistentComparisons+1)%2)))
	for y := 120; y < 150; y++ {
		for x := 10; x < 70; x++ {
			elsewhere.y[y*80+x] = 220
		}
	}
	if got := tracker.compare(previous, newLumaBlockMeans(elsewhere, image.Rect(0, 0, 80, 160))); got.quiet() {
		t.Fatalf("a new change outside the ignored motion was quiet: %+v", got)
	}
}

// Most of the screen changing in every frame is a transition, which must be
// waited out instead of ignored.
func TestStableMotionTrackerDoesNotIgnoreLargeContinuousChange(t *testing.T) {
	tracker := stableMotionTracker{threshold: defaultDiffThreshold}
	page := image.Rect(0, 20, 80, 160)
	previous := stableTestMeans(page, 100)
	for i := 0; i < stableMotionPersistentComparisons+2; i++ {
		current := stableTestMeans(page, byte(100+40*((i+1)%2)))
		if got := tracker.compare(previous, current); got.quiet() || got.ignored != 0 {
			t.Fatalf("comparison %d = %+v, want a transition that is not quiet", i+1, got)
		}
		previous = current
	}
}

func TestWaitStableScreenToolEndsWhileAVideoTileKeepsPlaying(t *testing.T) {
	video := image.Rect(10, 40, 40, 70)
	var frames []fakeWaitStableFrame
	for i := 0; i < 20; i++ {
		frames = append(frames, fakeWaitStableFrame{
			meta: frameMetadata{Seq: uint64(i + 1), Width: 80, Height: 160, Stride: 80, PixelFormat: "nv12"},
			data: append(stableTestFrame(video, byte(100+40*(i%2))).y, make([]byte, 80*80)...),
		})
	}
	tool := NewWaitStableScreenTool(&fakeWaitStableFrameClient{rawFrames: frames}, ScreenStableDefaults{TimeoutMs: 3000, StableMs: 250})
	result, err := tool.wait(context.Background(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Stable || result.IgnoredMotionBlocks == 0 || result.FramesDiffered == nil || !*result.FramesDiffered {
		t.Fatalf("result = %+v, want stable with the video tile ignored", result)
	}
	if result.ElapsedMs >= 3000 {
		t.Fatalf("elapsed_ms = %d, want the wait to end before the timeout", result.ElapsedMs)
	}
}

// The plane-reading comparison for decoded JPEG screenshots must classify
// blocks like the generic per-pixel comparison.
func TestScreenshotProgressYCbCrPathMatchesGenericPath(t *testing.T) {
	for _, changed := range []image.Rectangle{
		{},
		image.Rect(0, 0, 200, 32),
		image.Rect(20, 100, 120, 200),
		image.Rect(60, 300, 75, 316),
	} {
		before, _ := extractScreenshotImage(terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{}))
		after, _ := extractScreenshotImage(terminationPolicyScreenshotObservation(t, 200, 400, changed))
		if _, ok := after.(*image.YCbCr); !ok {
			t.Fatalf("decoded screenshot is %T, want *image.YCbCr", after)
		}
		bounds := screenshotProgressBounds(after.Bounds())
		fast, _ := screenshotProgressBlockDiffRatio(before, after, bounds, nil)
		generic, _ := screenshotProgressBlockDiffRatio(asRGBA(before), asRGBA(after), bounds, nil)
		if diff := fast - generic; diff > 0.002 || diff < -0.002 {
			t.Fatalf("changed=%v: plane path ratio %.4f, generic %.4f", changed, fast, generic)
		}
	}
}

func asRGBA(img image.Image) *image.RGBA {
	out := image.NewRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

type slowWaitFrameClient struct {
	fakeWaitStableFrameClient
	delay time.Duration
}

func (c *slowWaitFrameClient) LatestFrameWithFormat(format string, quality int, crop bool, hint screenprovider.CropHint) (*frameMetadata, []byte, screenCaptureInfo, error) {
	time.Sleep(c.delay)
	return c.fakeWaitStableFrameClient.LatestFrameWithFormat(format, quality, crop, hint)
}

// A capture that cannot finish before the deadline is not started, so a
// screen that never settles ends the wait near the configured timeout.
func TestWaitStableScreenToolDoesNotOverrunItsTimeout(t *testing.T) {
	var frames []fakeWaitStableFrame
	for i := 0; i < 20; i++ {
		frames = append(frames, fakeWaitStableFrame{
			meta: frameMetadata{Seq: uint64(i + 1), Width: 80, Height: 160, Stride: 80, PixelFormat: "nv12"},
			data: append(stableTestFrame(image.Rect(0, 20, 80, 160), byte(60+60*(i%2))).y, make([]byte, 80*80)...),
		})
	}
	client := &slowWaitFrameClient{fakeWaitStableFrameClient: fakeWaitStableFrameClient{rawFrames: frames}, delay: 300 * time.Millisecond}
	tool := NewWaitStableScreenTool(client, ScreenStableDefaults{TimeoutMs: 700, StableMs: 250})
	result, err := tool.wait(context.Background(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	if result.Stable || result.ElapsedMs > 750 {
		t.Fatalf("result = %+v, want an unstable result within the 700ms timeout", result)
	}
}

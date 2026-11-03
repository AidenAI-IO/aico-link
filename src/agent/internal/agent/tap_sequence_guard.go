package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"strings"
	"time"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/agent/screenprovider"
	"aico-agent/internal/logging"
)

// A tap_sequence is planned from one screenshot, so it stays valid only while
// the targets it has not tapped yet look as they did then. Between taps the
// guard compares a patch around each remaining target with the baseline frame.
// On the board's raw frames, untouched keys differ by at most 1 (mean absolute
// luma difference on 0-255) and the key just tapped, with the pointer and
// press highlight on it, by at most 16; on screenshots, keys stay within 21
// when the keypad dims slightly as a suggestion row appears, and keys covered
// by a new screen differ by 31 to 190.
const tapSequenceTargetChangeThreshold = 30

// tapSequenceTargetPatchFraction sizes the compared patch as a share of the
// phone screen width, about the inside of a keypad key on a phone.
const tapSequenceTargetPatchFraction = 0.035

// tapSequenceTargetShiftPixels tolerates a target moving by a pixel or two,
// which still leaves the planned point on it.
const tapSequenceTargetShiftPixels = 2

// tapSequenceLayoutChangeRatio is a backstop for changes the target patches
// can miss: when more than half of the phone screen's grid blocks differ from
// the baseline, the screen is a different one.
const tapSequenceLayoutChangeRatio = 0.5

// tapSequencePointerParkDelay lets the iOS pointer finish moving onto the
// first target, and its hover effect settle, before the baseline is captured.
const tapSequencePointerParkDelay = 150 * time.Millisecond

// Layout detection averages blocks, so sampling every other pixel per axis
// keeps the result while cutting the per-tap cost on the board CPU.
const tapSequenceSampleStep = 2

// lumaFrame is the luminance plane of one captured frame.
type lumaFrame struct {
	width, height, stride int
	y                     []byte
}

func (f lumaFrame) valid() bool {
	return f.width > 0 && f.height > 0 && f.stride >= f.width && len(f.y) >= f.stride*(f.height-1)+f.width
}

// frameCapturer returns the current frame for local comparison. It does not
// update the screenshot the model sees.
type frameCapturer func(ctx context.Context) (lumaFrame, error)

// newFrameCapturer requests raw frames: the frame service returns NV12 at the
// same latency as JPEG, and its Y plane needs no decoding.
func newFrameCapturer(client screenshotFrameClient) frameCapturer {
	if client == nil {
		return nil
	}
	return func(ctx context.Context) (lumaFrame, error) {
		if err := ctx.Err(); err != nil {
			return lumaFrame{}, err
		}
		meta, data, _, err := client.LatestFrameWithFormat("raw", 0, false, screenprovider.CropHint{})
		if err != nil {
			return lumaFrame{}, err
		}
		if meta == nil {
			return lumaFrame{}, fmt.Errorf("frame service returned no metadata")
		}
		if meta.Stale {
			return lumaFrame{}, fmt.Errorf("frame service returned a stale frame")
		}
		return frameLuma(meta, data)
	}
}

func frameLuma(meta *frameMetadata, data []byte) (lumaFrame, error) {
	w, h := int(meta.Width), int(meta.Height)
	switch meta.PixelFormat {
	case "nv12", "nv16":
		stride := int(meta.Stride)
		if stride < w {
			stride = w
		}
		frame := lumaFrame{width: w, height: h, stride: stride, y: data}
		if !frame.valid() {
			return lumaFrame{}, fmt.Errorf("%s frame too small: %d bytes for %dx%d", meta.PixelFormat, len(data), w, h)
		}
		return frame, nil
	case "yuyv", "uyvy":
		if len(data) < w*h*2 {
			return lumaFrame{}, fmt.Errorf("%s frame too small: %d bytes for %dx%d", meta.PixelFormat, len(data), w, h)
		}
		offset := 0
		if meta.PixelFormat == "uyvy" {
			offset = 1
		}
		y := make([]byte, w*h)
		for i := range y {
			y[i] = data[i*2+offset]
		}
		return lumaFrame{width: w, height: h, stride: w, y: y}, nil
	case "jpeg", "jpg", "png":
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return lumaFrame{}, fmt.Errorf("decode %s frame: %w", meta.PixelFormat, err)
		}
		return imageLuma(img), nil
	default:
		return lumaFrame{}, fmt.Errorf("unsupported pixel format %q", meta.PixelFormat)
	}
}

func imageLuma(img image.Image) lumaFrame {
	b := img.Bounds()
	if ycc, ok := img.(*image.YCbCr); ok && b.Min == (image.Point{}) {
		return lumaFrame{width: b.Dx(), height: b.Dy(), stride: ycc.YStride, y: ycc.Y}
	}
	if gray, ok := img.(*image.Gray); ok && b.Min == (image.Point{}) {
		return lumaFrame{width: b.Dx(), height: b.Dy(), stride: gray.Stride, y: gray.Pix}
	}
	y := make([]byte, b.Dx()*b.Dy())
	for row := 0; row < b.Dy(); row++ {
		for col := 0; col < b.Dx(); col++ {
			y[row*b.Dx()+col] = color.GrayModel.Convert(img.At(b.Min.X+col, b.Min.Y+row)).(color.Gray).Y
		}
	}
	return lumaFrame{width: b.Dx(), height: b.Dy(), stride: b.Dx(), y: y}
}

// tapSequenceInput parses a tap_sequence request; ok is false for any other
// input. The screen check is not optional: a model saving time must not be
// able to send the remaining taps onto a screen that changed.
func tapSequenceInput(input string) (points []normalizedPoint, ok bool) {
	var args struct {
		Type   string         `json:"type"`
		Points []pointerPoint `json:"points"`
	}
	if json.Unmarshal([]byte(input), &args) != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "tap_sequence") {
		return nil, false
	}
	if len(args.Points) == 0 {
		return nil, false
	}
	points = make([]normalizedPoint, len(args.Points))
	for i, point := range args.Points {
		points[i] = normalizedPoint{X: point.X.Float64(), Y: point.Y.Float64()}
	}
	return points, true
}

// newTapSequenceGuard checks, after each tap, that the targets not tapped yet
// still look as they did in the baseline, including a target the sequence
// taps again: the pointer and press highlight stay well below the threshold.
// A whole-screen comparison backs this up for layouts whose remaining targets
// all happen to sit outside the changed area.
func newTapSequenceGuard(capture frameCapturer, screenState *screen.ScreenState, baseline lumaFrame, points []normalizedPoint) mnk.TapSequenceGuard {
	return func(ctx context.Context, executed int) (bool, string, error) {
		started := time.Now()
		current, err := capture(ctx)
		if err != nil {
			return false, "", err
		}
		region := tapSequenceCompareRegion(screenState, current)
		ratio, comparable := tapSequenceFrameChange(baseline, current, region)
		target, diff := tapSequenceChangedTarget(baseline, current, region, points, executed)
		logging.Infof("agent", "tap_sequence", "screen check after tap %d: max_target_diff=%.1f changed_ratio=%.4f comparable=%v region=%v check_ms=%d", executed, diff, ratio, comparable, region, time.Since(started).Milliseconds())
		switch {
		case !comparable || ratio > tapSequenceLayoutChangeRatio:
			return true, fmt.Sprintf("screen layout changed after tap %d (%.0f%% of the screen changed); remaining taps were not sent", executed, ratio*100), nil
		case target >= 0:
			return true, fmt.Sprintf("target %d changed after tap %d, so the planned layout no longer matches; remaining taps were not sent", target+1, executed), nil
		}
		return false, "", nil
	}
}

// tapSequenceChangedTarget returns the index of the first remaining target
// whose patch differs from the baseline beyond the threshold, or -1, and the
// largest difference seen.
func tapSequenceChangedTarget(baseline, current lumaFrame, region image.Rectangle, points []normalizedPoint, executed int) (int, float64) {
	if region.Empty() || executed < 1 || executed > len(points) {
		return -1, 0
	}
	radius := max(4, int(float64(region.Dx())*tapSequenceTargetPatchFraction+0.5))
	locate := func(point normalizedPoint) image.Point {
		return image.Point{
			X: region.Min.X + int(point.X/1000*float64(region.Dx()-1)+0.5),
			Y: region.Min.Y + int(point.Y/1000*float64(region.Dy()-1)+0.5),
		}
	}
	maxDiff := 0.0
	for i := executed; i < len(points); i++ {
		center := locate(points[i])
		diff := lumaPatchDiff(baseline, current, center, radius, tapSequenceTargetShiftPixels)
		maxDiff = max(maxDiff, diff)
		if diff > tapSequenceTargetChangeThreshold {
			return i, maxDiff
		}
	}
	return -1, maxDiff
}

// lumaPatchDiff returns the smallest mean absolute luminance difference
// between the baseline patch at center and the current frame shifted by up to
// shift pixels, sampling every other pixel.
func lumaPatchDiff(baseline, current lumaFrame, center image.Point, radius, shift int) float64 {
	best := -1.0
	for dy := -shift; dy <= shift; dy++ {
		for dx := -shift; dx <= shift; dx++ {
			sum, count := 0, 0
			for y := center.Y - radius; y <= center.Y+radius; y += tapSequenceSampleStep {
				cy := y + dy
				if y < 0 || y >= baseline.height || cy < 0 || cy >= current.height {
					continue
				}
				for x := center.X - radius; x <= center.X+radius; x += tapSequenceSampleStep {
					cx := x + dx
					if x < 0 || x >= baseline.width || cx < 0 || cx >= current.width {
						continue
					}
					sum += absInt(int(baseline.y[y*baseline.stride+x]) - int(current.y[cy*current.stride+cx]))
					count++
				}
			}
			if count == 0 {
				continue
			}
			if diff := float64(sum) / float64(count); best < 0 || diff < best {
				best = diff
			}
		}
	}
	return max(best, 0)
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// tapSequenceCompareRegion returns the phone screen's active area when it is
// known for this frame size, and the whole frame otherwise.
func tapSequenceCompareRegion(screenState *screen.ScreenState, frame lumaFrame) image.Rectangle {
	full := image.Rect(0, 0, frame.width, frame.height)
	if screenState == nil {
		return full
	}
	width, height, active, _, ok := screenState.ActiveAreaWithAge()
	if !ok || !active.Valid || width != frame.width || height != frame.height {
		return full
	}
	region := image.Rect(active.X, active.Y, active.X+active.Width, active.Y+active.Height).Intersect(full)
	if region.Empty() {
		return full
	}
	return region
}

// tapSequenceFrameChange returns the share of progress-grid blocks whose mean
// luminance moved by more than the progress threshold. It skips the top
// status area like screenshotProgressChanged. Frames of different sizes are
// not comparable and count as a full change.
func tapSequenceFrameChange(before, after lumaFrame, region image.Rectangle) (float64, bool) {
	if !before.valid() || !after.valid() || before.width != after.width || before.height != after.height {
		return 1, false
	}
	region = region.Intersect(image.Rect(0, 0, after.width, after.height))
	region.Min.Y += int(float64(region.Dy()) * screenshotProgressIgnoreTopFraction)
	width, height := region.Dx(), region.Dy()
	if width <= 0 || height <= 0 {
		return 1, false
	}
	columns := min(screenshotProgressGridColumns, width)
	rows := min(screenshotProgressGridRows, height)
	changed := 0
	for row := 0; row < rows; row++ {
		y0 := region.Min.Y + height*row/rows
		y1 := region.Min.Y + height*(row+1)/rows
		for column := 0; column < columns; column++ {
			x0 := region.Min.X + width*column/columns
			x1 := region.Min.X + width*(column+1)/columns
			var sumBefore, sumAfter, count int
			for y := y0; y < y1; y += tapSequenceSampleStep {
				beforeRow := before.y[y*before.stride:]
				afterRow := after.y[y*after.stride:]
				for x := x0; x < x1; x += tapSequenceSampleStep {
					sumBefore += int(beforeRow[x])
					sumAfter += int(afterRow[x])
					count++
				}
			}
			if count == 0 {
				continue
			}
			diff := sumBefore - sumAfter
			if diff < 0 {
				diff = -diff
			}
			if diff > screenshotProgressBlockChannelThreshold*count {
				changed++
			}
		}
	}
	return float64(changed) / float64(columns*rows), true
}

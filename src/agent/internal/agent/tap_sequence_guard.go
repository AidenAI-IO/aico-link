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

// tapSequenceLayoutChangeRatio is the share of grid blocks inside the phone
// screen that must change between two consecutive taps before a tap_sequence
// stops. Entering a digit or toggling a checkbox changes a few percent of the
// phone screen and a suggestion row about a quarter of that; navigation,
// sheets, and dimmed dialogs change far more.
const tapSequenceLayoutChangeRatio = 0.25

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

// isTapSequenceInput reports whether input requests a tap_sequence that keeps
// the default screen-change check enabled.
func isTapSequenceInput(input string) (sequence, guarded bool) {
	var args struct {
		Type               string `json:"type"`
		StopOnScreenChange *bool  `json:"stop_on_screen_change"`
	}
	if json.Unmarshal([]byte(input), &args) != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "tap_sequence") {
		return false, false
	}
	return true, args.StopOnScreenChange == nil || *args.StopOnScreenChange
}

// newTapSequenceGuard compares each frame with the one before the previous
// tap, so the check measures what that single tap did rather than the
// accumulated sequence. screenState supplies the phone screen's active area
// inside the captured frame; black bars around it never change and would
// otherwise dilute the ratio.
func newTapSequenceGuard(capture frameCapturer, screenState *screen.ScreenState, baseline lumaFrame) mnk.TapSequenceGuard {
	previous := baseline
	return func(ctx context.Context, executed int) (bool, string, error) {
		started := time.Now()
		current, err := capture(ctx)
		if err != nil {
			return false, "", err
		}
		region := tapSequenceCompareRegion(screenState, current)
		ratio, comparable := tapSequenceFrameChange(previous, current, region)
		previous = current
		logging.Infof("agent", "tap_sequence", "screen check after tap %d: changed_ratio=%.4f comparable=%v region=%v check_ms=%d", executed, ratio, comparable, region, time.Since(started).Milliseconds())
		if comparable && ratio <= tapSequenceLayoutChangeRatio {
			return false, "", nil
		}
		return true, fmt.Sprintf("screen layout changed after tap %d (%.0f%% of the screen changed); remaining taps were not sent", executed, ratio*100), nil
	}
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

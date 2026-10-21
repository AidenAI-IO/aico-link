package mnk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

type releaseSample struct {
	at   time.Time
	data []byte
}

type releaseCaptureDevice struct{ samples []releaseSample }

func (d *releaseCaptureDevice) Write(b []byte) error {
	d.samples = append(d.samples, releaseSample{time.Now(), append([]byte(nil), b...)})
	return nil
}
func (d *releaseCaptureDevice) Close() {}

func TestSwipeReleaseHasRealSlowMotionAndExactEndpoint(t *testing.T) {
	for _, touchscreen := range []bool{false, true} {
		t.Run(fmt.Sprintf("touchscreen=%t", touchscreen), func(t *testing.T) {
			testSwipeReleaseHasRealSlowMotionAndExactEndpoint(t, touchscreen)
		})
	}
}

func TestHIDAtomicMoveUsesControlledAccelerationAndBraking(t *testing.T) {
	for _, touchscreen := range []bool{false, true} {
		t.Run(fmt.Sprintf("touchscreen=%t", touchscreen), func(t *testing.T) {
			testHIDAtomicMoveCurve(t, touchscreen)
		})
	}
}

// Verify the two public entry points emit the same contact trajectory, including
// the low-speed endpoint approach, so a new tool cannot silently omit braking.
func TestHIDSwipeAndAtomicMoveShareReleaseTrajectory(t *testing.T) {
	for _, touchscreen := range []bool{false, true} {
		for _, end := range []Point{{500, 200}, {500, 900}, {900, 500}, {200, 300}} {
			t.Run(fmt.Sprintf("touchscreen=%t/end=%v", touchscreen, end), func(t *testing.T) {
				start := Point{500, 600}
				swipeDevice, atomicDevice := &releaseCaptureDevice{}, &releaseCaptureDevice{}
				swipe := NewHIDProvider(swipeDevice, nil, nil, nil, touchscreen, "qwerty", nil)
				atomic := NewHIDProvider(atomicDevice, nil, nil, nil, touchscreen, "qwerty", nil)
				swipe.cursorSettleMs, atomic.cursorSettleMs = 0, 0
				if err := swipe.SwipeWithOptions(context.Background(), [][2]float64{{start.X, start.Y}, {end.X, end.Y}}, ButtonLeft, SwipeOptions{DurationMs: 40}); err != nil {
					t.Fatal(err)
				}
				if err := atomic.TouchActions(context.Background(), []TouchAction{
					{Type: "touch_down", Point: &start},
					{Type: "move_to", Point: &end, DurationMs: 40},
					{Type: "touch_up"},
				}); err != nil {
					t.Fatal(err)
				}
				if len(swipeDevice.samples) != len(atomicDevice.samples) {
					t.Fatalf("different report counts: swipe=%d atomic=%d", len(swipeDevice.samples), len(atomicDevice.samples))
				}
				for i, sample := range swipeDevice.samples {
					if string(sample.data) != string(atomicDevice.samples[i].data) {
						t.Fatalf("report %d differs between swipe and atomic move", i)
					}
				}
			})
		}
	}
}

func testHIDAtomicMoveCurve(t *testing.T, touchscreen bool) {
	d := &releaseCaptureDevice{}
	p := NewHIDProvider(d, nil, nil, nil, touchscreen, "qwerty", nil)
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 800}},
		{Type: "move_to", Point: &Point{X: 500, Y: 200}, DurationMs: 240},
		{Type: "touch_up"},
	}
	if err := p.TouchActions(context.Background(), actions); err != nil {
		t.Fatal(err)
	}
	var held []releaseSample
	for _, sample := range d.samples {
		if sample.data[0]&1 != 0 {
			held = append(held, sample)
		}
	}
	if len(held) != 1+2*p.swipeSteps {
		t.Fatalf("held reports = %d, want main movement and real release tail", len(held))
	}
	position := func(sample releaseSample) (int, int) {
		offset := 1
		if touchscreen {
			offset = 2
		}
		return int(binary.LittleEndian.Uint16(sample.data[offset : offset+2])), int(binary.LittleEndian.Uint16(sample.data[offset+2 : offset+4]))
	}
	distance := func(index int) float64 {
		x1, y1 := position(held[index-1])
		x2, y2 := position(held[index])
		return math.Hypot(float64(x2-x1), float64(y2-y1))
	}
	if distance(12) <= distance(1) || distance(12) <= distance(len(held)-1) {
		t.Fatalf("atomic move lacks acceleration/braking: first=%v middle=%v last=%v", distance(1), distance(12), distance(len(held)-1))
	}
	_, endY, _ := p.normalizedToAbsolute(500, 200)
	_, gotY := position(held[len(held)-1])
	if gotY != endY {
		t.Fatalf("atomic move ended at y=%d, want %d", gotY, endY)
	}
	if elapsed := held[len(held)-1].at.Sub(held[0].at); elapsed < 340*time.Millisecond {
		t.Fatalf("atomic motion was shortened: %v", elapsed)
	}
	_, tailStartY := position(held[p.swipeSteps])
	if distance := float64(tailStartY-endY) * 1000 / AbsMouseMaxPos; distance <= 0 || distance > 2.1 {
		t.Fatalf("release tail must move to endpoint, distance=%v", distance)
	}
	if elapsed := held[len(held)-1].at.Sub(held[p.swipeSteps].at); elapsed < 95*time.Millisecond {
		t.Fatalf("release tail too short: %v", elapsed)
	}
	_, previousY := position(held[0])
	for _, sample := range held[1:] {
		_, y := position(sample)
		if y > previousY || y < endY {
			t.Fatal("atomic motion reversed or overshot")
		}
		previousY = y
	}
}

func TestHIDAtomicCurveCancellationReleasesAtLastPosition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelAfterWritesDevice{cancel: cancel, cancelAt: 3}
	p := NewHIDProvider(d, nil, nil, nil, true, "qwerty", nil)
	err := p.TouchActions(ctx, []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 800}},
		{Type: "move_to", Point: &Point{X: 500, Y: 200}, DurationMs: 240},
		{Type: "touch_up"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	reports := d.bytes()
	if len(reports) != (3+p.releaseRepeatCount)*6 {
		t.Fatalf("missing cleanup reports: %d", len(reports))
	}
	lastY := binary.LittleEndian.Uint16(reports[16:18])
	for offset := 18; offset < len(reports); offset += 6 {
		if reports[offset] != 0 || binary.LittleEndian.Uint16(reports[offset+4:offset+6]) != lastY {
			t.Fatal("cleanup moved contact or left it pressed")
		}
	}
}

func testSwipeReleaseHasRealSlowMotionAndExactEndpoint(t *testing.T, touchscreen bool) {
	for _, tc := range []struct {
		name string
		path [][2]float64
	}{
		{"up", [][2]float64{{500, 800}, {500, 200}}},
		{"down", [][2]float64{{500, 200}, {500, 800}}},
		{"horizontal", [][2]float64{{800, 500}, {200, 500}}},
		{"diagonal", [][2]float64{{800, 800}, {200, 200}}},
		{"short", [][2]float64{{500, 500}, {500, 496}}},
		{"duplicate endpoint", [][2]float64{{500, 800}, {500, 200}, {500, 200}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &releaseCaptureDevice{}
			p := NewHIDProvider(d, nil, nil, nil, touchscreen, "qwerty", nil)
			err := p.SwipeWithOptions(context.Background(), tc.path, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 24})
			if err != nil {
				t.Fatal(err)
			}
			var held, releases []releaseSample
			for _, r := range d.samples {
				if r.data[0]&1 != 0 {
					if touchscreen && (r.data[0] != 0x03 || r.data[1] != 1) {
						t.Fatal("touchscreen contact lost tip/in-range flags or changed contact ID")
					}
					held = append(held, r)
				} else if len(held) > 0 {
					releases = append(releases, r)
				}
			}
			if len(held) < 10 || len(releases) != p.releaseRepeatCount {
				t.Fatal("missing release tail")
			}
			position := func(r releaseSample) (int, int) {
				offset := 1
				if touchscreen {
					offset = 2
				}
				return int(binary.LittleEndian.Uint16(r.data[offset : offset+2])), int(binary.LittleEndian.Uint16(r.data[offset+2 : offset+4]))
			}
			end := tc.path[len(tc.path)-1]
			wantX, wantY, _ := p.normalizedToAbsolute(end[0], end[1])
			x, y := position(held[len(held)-1])
			if x != wantX || y != wantY {
				t.Fatal("did not reach requested endpoint")
			}
			for _, r := range releases {
				if r.data[0] != 0 {
					t.Fatal("contact flags not cleared on release")
				}
				x, y := position(r)
				if x != wantX || y != wantY {
					t.Fatal("release moved endpoint")
				}
			}
			// Require real low-speed moves: unchanged-coordinate holds did not
			// clear fling velocity in the measured iOS AssistiveTouch trials.

			// The main movement builds speed and brakes, instead of entering or
			// leaving the fast section with a large constant-speed step.
			stepDistance := func(index int) float64 {
				a, b := position(held[index-1])
				c, d := position(held[index])
				return math.Hypot(float64(c-a), float64(d-b))
			}
			if stepDistance(12) <= stepDistance(2) || stepDistance(12) <= stepDistance(23) {
				t.Fatal("main movement lacks acceleration/braking")
			}
			tailStart := held[24]
			if duration := tailStart.at.Sub(held[0].at); duration < 180*time.Millisecond {
				t.Fatalf("main motion duration %v, want at least 180ms before the slow tail", duration)
			}
			x, y = position(tailStart)
			distance := math.Hypot(float64(wantX-x), float64(wantY-y)) * 1000 / AbsMouseMaxPos
			if distance <= 0 || distance > 2.1 {
				t.Fatalf("tail distance %v", distance)
			}
			if duration := held[len(held)-1].at.Sub(tailStart.at); duration < 95*time.Millisecond || duration > 250*time.Millisecond {
				t.Fatalf("tail duration %v", duration)
			}
			if gap := releases[0].at.Sub(held[len(held)-1].at); gap > 50*time.Millisecond {
				t.Fatalf("unexpected final hold %v", gap)
			}
			previousX, previousY := position(held[0])
			for _, r := range held[1:] {
				x, y := position(r)
				if (x-previousX)*(wantX-previousX) < 0 || (y-previousY)*(wantY-previousY) < 0 || math.Abs(float64(wantX-x)) > math.Abs(float64(wantX-previousX)) || math.Abs(float64(wantY-y)) > math.Abs(float64(wantY-previousY)) {
					t.Fatal("swipe reversed or overshot")
				}
				previousX, previousY = x, y
			}
		})
	}
}

func TestControlledSwipeMotionPreservesDurationAcrossSegments(t *testing.T) {
	for _, tc := range []struct {
		name       string
		durationMs int
		steps      int
	}{
		{"single step per segment", 181, 1},
		{"fractional intervals", 283, 24},
		{"sub-millisecond intervals", 181, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &releaseCaptureDevice{}
			p := NewHIDProvider(d, nil, nil, nil, true, "qwerty", nil)
			path := [][2]int{{1000, 1000}, {2000, 1000}, {2000, 1000}, {2000, 3000}, {6000, 3000}}
			started := time.Now()
			x, y, err := p.moveAlongPathWithSteps(context.Background(), path, 1, tc.durationMs, tc.steps, true)
			if err != nil {
				t.Fatal(err)
			}
			if x != 6000 || y != 3000 || len(d.samples) == 0 {
				t.Fatalf("main motion did not reach endpoint: (%d, %d)", x, y)
			}
			if elapsed := d.samples[len(d.samples)-1].at.Sub(started); elapsed < time.Duration(tc.durationMs)*time.Millisecond {
				t.Fatalf("main motion duration %v, want at least %dms", elapsed, tc.durationMs)
			}
		})
	}
}

func TestSwipeEdgeAndExplicitHoldRemainUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start [2]float64
		hold  int
		touch bool
	}{
		{"home", [2]float64{500, 999}, 0, false},
		{"back", [2]float64{1, 500}, 0, false},
		{"explicit", [2]float64{500, 800}, 20, false},
		{"touchscreen home", [2]float64{500, 999}, 0, true},
		{"touchscreen back", [2]float64{1, 500}, 0, true},
		{"touchscreen explicit", [2]float64{500, 800}, 20, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &releaseCaptureDevice{}
			p := NewHIDProvider(d, nil, nil, nil, tc.touch, "qwerty", nil)
			err := p.SwipeWithOptions(context.Background(), [][2]float64{tc.start, {500, 200}}, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 4, HoldAfterMs: tc.hold})
			if err != nil {
				t.Fatal(err)
			}
			firstRelease := len(d.samples) - p.releaseRepeatCount
			delay := d.samples[firstRelease].at.Sub(d.samples[firstRelease-1].at)
			if delay > 80*time.Millisecond {
				t.Fatalf("unexpected internal settle: %v", delay)
			}
			if tc.hold > 0 && delay < time.Duration(tc.hold)*time.Millisecond {
				t.Fatal("explicit hold was shortened")
			}
		})
	}
}

type failOnceSwipeDevice struct {
	releaseCaptureDevice
	failAt int
	writes int
}

func (d *failOnceSwipeDevice) Write(data []byte) error {
	d.writes++
	if d.writes == d.failAt {
		return errors.New("injected write failure")
	}
	return d.releaseCaptureDevice.Write(data)
}

func TestSwipeWriteFailureReleasesLastSuccessfulPosition(t *testing.T) {
	for _, failAt := range []int{3, 8} { // during movement; during the slow tail
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			d := &failOnceSwipeDevice{failAt: failAt}
			p := NewHIDProvider(d, nil, nil, nil, false, "qwerty", nil)
			err := p.SwipeWithOptions(context.Background(), [][2]float64{{500, 800}, {500, 200}}, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 4})
			if err == nil {
				t.Fatal("missing write error")
			}
			firstRelease := len(d.samples) - p.releaseRepeatCount
			lastHeld := d.samples[firstRelease-1].data
			for _, sample := range d.samples[firstRelease:] {
				if sample.data[0]&3 != 0 || string(sample.data[1:]) != string(lastHeld[1:]) {
					t.Fatal("cleanup jumped or failed to release")
				}
			}
		})
	}
}

func TestSwipeCanceledDuringSlowTailStillReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelAfterWritesDevice{cancel: cancel, cancelAt: 7}
	p := NewHIDProvider(d, nil, nil, nil, false, "qwerty", nil)
	err := p.SwipeWithOptions(ctx, [][2]float64{{500, 800}, {500, 200}}, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 4})
	if err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	data := d.bytes()
	if len(data) != (7+p.releaseRepeatCount)*6 {
		t.Fatal("contact reports continued after cancellation")
	}
	for offset := 7 * 6; offset < len(data); offset += 6 {
		if data[offset]&3 != 0 || string(data[offset+1:offset+6]) != string(data[6*6+1:7*6]) {
			t.Fatal("missing endpoint cleanup")
		}
	}
}

func TestSwipeCanceledDuringMainMotionStillReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &cancelAfterWritesDevice{cancel: cancel, cancelAt: 5}
	p := NewHIDProvider(d, nil, nil, nil, false, "qwerty", nil)
	err := p.SwipeWithOptions(ctx, [][2]float64{{500, 800}, {500, 200}}, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 4})
	if err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
	data := d.bytes()
	last := data[4*6 : 5*6]
	if len(data) != (5+p.releaseRepeatCount)*6 {
		t.Fatal("missing cleanup releases")
	}
	for offset := 5 * 6; offset < len(data); offset += 6 {
		r := data[offset : offset+6]
		if r[0]&3 != 0 || string(r[1:]) != string(last[1:]) {
			t.Fatal("contact not released at last position")
		}
	}
}

func TestTouchscreenSwipeCancellationReleasesLastSuccessfulPosition(t *testing.T) {
	for _, cancelAt := range []int{3, 6} { // during main movement; first slow-tail report
		t.Run(fmt.Sprint(cancelAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d := &cancelAfterWritesDevice{cancel: cancel, cancelAt: cancelAt}
			p := NewHIDProvider(d, nil, nil, nil, true, "qwerty", nil)
			err := p.SwipeWithOptions(ctx, [][2]float64{{500, 800}, {500, 200}}, ButtonLeft, SwipeOptions{DurationMs: 40, Steps: 4})
			if err != context.Canceled {
				t.Fatalf("error=%v, want context.Canceled", err)
			}
			data := d.bytes()
			if len(data) != (cancelAt+p.releaseRepeatCount)*6 {
				t.Fatal("contact reports continued after cancellation")
			}
			last := data[(cancelAt-1)*6 : cancelAt*6]
			for offset := cancelAt * 6; offset < len(data); offset += 6 {
				if data[offset] != 0 || string(data[offset+1:offset+6]) != string(last[1:]) {
					t.Fatal("touchscreen cleanup changed contact ID/position or kept contact active")
				}
			}
		})
	}
}

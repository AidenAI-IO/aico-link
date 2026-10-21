package mnk

import (
	"aiden-agent/internal/agent/screen"
	"context"
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func TestDragActivationPointMovesExactlyTwoHundredWithinBounds(t *testing.T) {
	const wantDistance = 200.0
	if dragStartMoveDistance != wantDistance {
		t.Fatalf("dragStartMoveDistance = %.2f, want %.2f", dragStartMoveDistance, wantDistance)
	}
	if dragStartMoveSpeed != 500 {
		t.Fatalf("dragStartMoveSpeed = %.2f, want 500", dragStartMoveSpeed)
	}
	if dragStartMoveDurationMs != 400 {
		t.Fatalf("dragStartMoveDurationMs = %d, want 400", dragStartMoveDurationMs)
	}
	for _, start := range []Point{
		{X: 500, Y: 500},
		{X: 0, Y: 0},
		{X: 1000, Y: 500},
		{X: 500, Y: 1000},
		{X: 17, Y: 983},
	} {
		activation := dragActivationPoint(start)
		distance := math.Hypot(activation.X-start.X, activation.Y-start.Y)
		if distance != wantDistance {
			t.Errorf("dragActivationPoint(%+v) = %+v, distance %.2f; want %.2f", start, activation, distance, wantDistance)
		}
		if activation.X < 0 || activation.X > 1000 || activation.Y < 0 || activation.Y > 1000 {
			t.Errorf("dragActivationPoint(%+v) = %+v, want coordinates in [0,1000]", start, activation)
		}
		if activation.X != start.X && activation.Y != start.Y {
			t.Errorf("dragActivationPoint(%+v) = %+v, want movement on one axis", start, activation)
		}
	}
}

func TestHIDProviderDragStartInterpolatesActivationMoveWithoutRelease(t *testing.T) {
	pointer := &layoutCaptureDevice{}
	provider := NewHIDProvider(pointer, nil, nil, nil, true, "qwerty", nil)
	provider.swipeSteps = 4

	if err := provider.DragStart(context.Background(), 600, 115, ButtonLeft); err != nil {
		t.Fatalf("DragStart() error = %v", err)
	}

	reports := pointer.bytes()
	const reportSize = 6
	if got, want := len(reports), (1+provider.swipeSteps)*reportSize; got != want {
		t.Fatalf("DragStart() wrote %d bytes, want %d", got, want)
	}

	startX, startY, err := provider.normalizedToAbsolute(600, 115)
	if err != nil {
		t.Fatalf("normalized start: %v", err)
	}
	activation := dragActivationPoint(Point{X: 600, Y: 115})
	endX, endY, err := provider.normalizedToAbsolute(activation.X, activation.Y)
	if err != nil {
		t.Fatalf("normalized activation point: %v", err)
	}

	previousY := -1
	for offset := 0; offset < len(reports); offset += reportSize {
		report := reports[offset : offset+reportSize]
		if report[0]&0x03 != 0x03 {
			t.Fatalf("report %d flags = %#x, want contact held", offset/reportSize, report[0])
		}
		x := int(binary.LittleEndian.Uint16(report[2:4]))
		y := int(binary.LittleEndian.Uint16(report[4:6]))
		if x != startX {
			t.Fatalf("report %d x = %d, want %d", offset/reportSize, x, startX)
		}
		if offset == 0 && y != startY {
			t.Fatalf("initial y = %d, want %d", y, startY)
		}
		if previousY >= 0 && y <= previousY {
			t.Fatalf("report %d y = %d, want greater than previous %d", offset/reportSize, y, previousY)
		}
		previousY = y
	}
	if gotX := int(binary.LittleEndian.Uint16(reports[len(reports)-4 : len(reports)-2])); gotX != endX {
		t.Fatalf("final x = %d, want %d", gotX, endX)
	}
	if gotY := int(binary.LittleEndian.Uint16(reports[len(reports)-2:])); gotY != endY {
		t.Fatalf("final y = %d, want %d", gotY, endY)
	}
	if !provider.dragActive {
		t.Fatal("DragStart() did not preserve active contact state")
	}
}

func TestHIDProviderDragStartCancellationReleasesAtLastActivationPosition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pointer := &cancelAfterWritesDevice{cancel: cancel, cancelAt: 2}
	provider := NewHIDProvider(pointer, nil, nil, nil, true, "qwerty", nil)
	provider.swipeSteps = 4

	err := provider.DragStart(ctx, 600, 115, ButtonLeft)
	if err != context.Canceled {
		t.Fatalf("DragStart() error = %v, want context canceled", err)
	}

	reports := pointer.bytes()
	const reportSize = 6
	wantReports := 2 + provider.releaseRepeatCount // press, first move, repeated cleanup releases
	if got := len(reports) / reportSize; got != wantReports {
		t.Fatalf("DragStart() reports = %d, want %d", got, wantReports)
	}
	lastMove := reports[reportSize : 2*reportSize]
	wantX := binary.LittleEndian.Uint16(lastMove[2:4])
	wantY := binary.LittleEndian.Uint16(lastMove[4:6])
	for offset := 2 * reportSize; offset < len(reports); offset += reportSize {
		release := reports[offset : offset+reportSize]
		if release[0]&0x03 != 0 {
			t.Fatalf("cleanup report %d flags = %#x, want contact released", offset/reportSize, release[0])
		}
		if gotX := binary.LittleEndian.Uint16(release[2:4]); gotX != wantX {
			t.Fatalf("cleanup report %d x = %d, want last emitted x %d", offset/reportSize, gotX, wantX)
		}
		if gotY := binary.LittleEndian.Uint16(release[4:6]); gotY != wantY {
			t.Fatalf("cleanup report %d y = %d, want last emitted y %d", offset/reportSize, gotY, wantY)
		}
	}
	if provider.dragActive {
		t.Fatal("DragStart() left drag active after successful cancellation cleanup")
	}
}

type cancelAfterWritesDevice struct {
	layoutCaptureDevice
	cancel   context.CancelFunc
	cancelAt int
	writes   int
}

func (d *cancelAfterWritesDevice) Write(report []byte) error {
	if err := d.layoutCaptureDevice.Write(report); err != nil {
		return err
	}
	d.writes++
	if d.writes == d.cancelAt {
		d.cancel()
	}
	return nil
}

func TestHIDProviderRejectsHorizontalScroll(t *testing.T) {
	pointer := &layoutCaptureDevice{}
	provider := NewHIDProvider(pointer, nil, nil, nil, false, "qwerty", nil)

	err := provider.Scroll(context.Background(), 3, 0)
	if got := AsError(err); got == nil || got.Kind != ErrInvalidArguments {
		t.Fatalf("Scroll() error = %v, want invalid arguments", err)
	}
	if report := pointer.bytes(); len(report) != 0 {
		t.Fatalf("report length = %d, want 0", len(report))
	}
}

func TestHIDProviderVerticalScrollUsesSixByteReport(t *testing.T) {
	pointer := &layoutCaptureDevice{}
	provider := NewHIDProvider(pointer, nil, nil, nil, false, "qwerty", nil)

	if err := provider.Scroll(context.Background(), 0, -3); err != nil {
		t.Fatalf("Scroll() error = %v", err)
	}
	report := pointer.bytes()
	if len(report) != 6 {
		t.Fatalf("report length = %d, want 6", len(report))
	}
	if report[5] != byte(253) {
		t.Fatalf("wheel byte = %d, want 253", report[5])
	}
}

func TestHIDProviderTouchActionsKeepsContactAcrossMoveAndWait(t *testing.T) {
	pointer := &layoutCaptureDevice{}
	provider := NewHIDProvider(pointer, nil, nil, nil, true, "qwerty", nil)
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 100, Y: 800}},
		{Type: "wait", DurationMs: 0},
		{Type: "move_to", Point: &Point{X: 100, Y: 200}},
		{Type: "touch_up"},
	}
	if err := provider.TouchActions(context.Background(), actions); err != nil {
		t.Fatalf("TouchActions() error = %v", err)
	}
	data := pointer.bytes()
	if len(data) != 5*6 {
		t.Fatalf("wrote %d bytes, want five 6-byte reports", len(data))
	}
	// The press, move, and final release reports must preserve the contact bit
	// until touch_up; the repeated release reports must be zero-contact.
	for i, offset := range []int{0, 6} {
		if data[offset]&0x03 != 0x03 {
			t.Fatalf("report %d flags = %#x, want contact", i, data[offset])
		}
	}
	for offset := 12; offset < len(data); offset += 6 {
		if data[offset]&0x03 != 0 {
			t.Fatalf("release report at %d flags = %#x, want no contact", offset, data[offset])
		}
	}
}

func TestHIDProviderTouchActionsRejectsInvalidDurationBeforeActionHandlers(t *testing.T) {
	tests := []struct {
		name   string
		action TouchAction
	}{
		{name: "touch down", action: TouchAction{Type: "touch_down", Point: &Point{X: 100, Y: 800}, DurationMs: 30001}},
		{name: "move", action: TouchAction{Type: "move_to", Point: &Point{X: 100, Y: 200}, DurationMs: -1}},
		{name: "touch up", action: TouchAction{Type: "touch_up", DurationMs: 30001}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pointer := &layoutCaptureDevice{}
			provider := NewHIDProvider(pointer, nil, nil, nil, true, "qwerty", nil)
			err := provider.TouchActions(context.Background(), []TouchAction{test.action})
			if got := AsError(err); got == nil || got.Kind != ErrInvalidArguments {
				t.Fatalf("TouchActions() error = %v, want invalid arguments", err)
			}
			if report := pointer.bytes(); len(report) != 0 {
				t.Fatalf("invalid duration wrote %d bytes before validation", len(report))
			}
		})
	}
}

func TestHIDProviderTouchActionsRejectsExcessiveTotalDurationBeforeWriting(t *testing.T) {
	pointer := &layoutCaptureDevice{}
	provider := NewHIDProvider(pointer, nil, nil, nil, true, "qwerty", nil)
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 100, Y: 800}},
		{Type: "move_to", Point: &Point{X: 100, Y: 600}, DurationMs: 30000},
		{Type: "wait", DurationMs: 30000},
		{Type: "move_to", Point: &Point{X: 100, Y: 200}, DurationMs: 1},
		{Type: "touch_up"},
	}

	err := provider.TouchActions(context.Background(), actions)
	if got := AsError(err); got == nil || got.Kind != ErrInvalidArguments || !strings.Contains(err.Error(), "total duration") {
		t.Fatalf("TouchActions() error = %v, want total-duration invalid arguments", err)
	}
	if report := pointer.bytes(); len(report) != 0 {
		t.Fatalf("excessive total duration wrote %d bytes before validation", len(report))
	}
}

func TestHIDProviderNormalizedToAbsoluteFallsBackToWholeSurface(t *testing.T) {
	provider := NewHIDProvider(nil, nil, nil, nil, false, "qwerty", nil)
	x, y, err := provider.normalizedToAbsolute(500, 250)
	if err != nil {
		t.Fatalf("normalizedToAbsolute returned error: %v", err)
	}
	if x != 16384 || y != 8192 {
		t.Fatalf("point = (%d,%d), want (16384,8192)", x, y)
	}
	if wantX, wantY := NormalizedToAbsolutePoint(500, 250); x != wantX || y != wantY {
		t.Fatalf("point = (%d,%d), want NormalizedToAbsolutePoint (%d,%d)", x, y, wantX, wantY)
	}
}

func TestHIDProviderNormalizedToAbsoluteRejectsNonFiniteCoordinates(t *testing.T) {
	provider := NewHIDProvider(nil, nil, nil, nil, false, "qwerty", nil)
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := provider.normalizedToAbsolute(value, 500); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("normalizedToAbsolute(%v, 500) error = %v, want finite-coordinate error", value, err)
		}
	}
}

func TestHIDProviderNormalizedToAbsoluteUsesActiveArea(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true})
	provider := NewHIDProvider(nil, nil, nil, screenState, false, "qwerty", nil)

	x, y, err := provider.normalizedToAbsolute(0, 500)
	if err != nil {
		t.Fatalf("normalizedToAbsolute returned error: %v", err)
	}
	if x != 0 {
		t.Fatalf("x = %d, want 0 at left edge of active_area", x)
	}
	if wantY := scalePixelToAbsolute(float64(1079)/2, 1080); y != wantY {
		t.Fatalf("y = %d, want %d", y, wantY)
	}
}

func TestHIDProviderTouchscreenNormalizedToAbsoluteUsesFrameSpaceWithinActiveArea(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true})
	provider := NewHIDProvider(nil, nil, nil, screenState, true, "qwerty", nil)

	x, y, err := provider.normalizedToAbsolute(627, 180)
	if err != nil {
		t.Fatalf("normalizedToAbsolute returned error: %v", err)
	}
	wantX := scalePixelToAbsolute(float64(656)+(627.0/1000.0)*float64(608-1), 1920)
	wantY := scalePixelToAbsolute((180.0/1000.0)*float64(1080-1), 1080)
	if x != wantX || y != wantY {
		t.Fatalf("point = (%d,%d), want (%d,%d)", x, y, wantX, wantY)
	}
}

func TestHIDProviderNormalizedToAbsolutePreservesNearlyFullAxisOffset(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 711, Y: 28, Width: 498, Height: 1052, Valid: true})
	provider := NewHIDProvider(nil, nil, nil, screenState, false, "qwerty", nil)

	x, y, err := provider.normalizedToAbsolute(500, 220)
	if err != nil {
		t.Fatalf("normalizedToAbsolute returned error: %v", err)
	}
	localX := 0.5 * float64(498-1)
	localY := 0.22 * float64(1052-1)
	wantX := scalePixelToAbsolute(localX, 498)
	wantY := scalePixelToAbsolute(28+localY, 1080)
	if x != wantX || y != wantY {
		t.Fatalf("point = (%d,%d), want (%d,%d)", x, y, wantX, wantY)
	}
}

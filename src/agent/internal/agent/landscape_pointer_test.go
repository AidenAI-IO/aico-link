package agent

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
)

// simulatedPointer draws the iOS pointer where the last absolute move lands,
// turned the way the phone maps its pointer.
type simulatedPointer struct {
	mock   *mnk.MockProvider
	region image.Rectangle
	turn   pointerTurn
	frameW int
	frameH int
}

func (s *simulatedPointer) capture(context.Context) (lumaFrame, error) {
	frame := lumaFrame{width: s.frameW, height: s.frameH, stride: s.frameW, y: make([]byte, s.frameW*s.frameH)}
	for i := range frame.y {
		frame.y[i] = 200
	}
	moves := s.mock.MoveCalls()
	if len(moves) == 0 {
		return frame, nil
	}
	x, y := s.turn.land(moves[len(moves)-1].X, moves[len(moves)-1].Y)
	center := landscapeFramePoint(s.region, x, y)
	const discRadius = 10
	for py := center.Y - discRadius; py <= center.Y+discRadius; py++ {
		for px := center.X - discRadius; px <= center.X+discRadius; px++ {
			if px >= 0 && py >= 0 && px < s.frameW && py < s.frameH && (px-center.X)*(px-center.X)+(py-center.Y)*(py-center.Y) <= discRadius*discRadius {
				frame.y[py*s.frameW+px] = 150
			}
		}
	}
	return frame, nil
}

func TestLandscapePointerCorrectsTurnedIOSPointer(t *testing.T) {
	landscapeArea := screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true}
	portraitArea := screen.ScreenActiveArea{X: 710, Y: 0, Width: 498, Height: 1080, Valid: true}
	cases := []struct {
		name       string
		deviceType string
		active     screen.ScreenActiveArea
		turn       pointerTurn
		wantClick  mnk.MockClick
	}{
		{name: "upright landscape is unchanged", deviceType: "iOS", active: landscapeArea, wantClick: mnk.MockClick{X: 200, Y: 700}},
		{name: "quarter turn is corrected", deviceType: "iOS", active: landscapeArea, turn: 1, wantClick: mnk.MockClick{X: 300, Y: 200}},
		{name: "half turn is corrected", deviceType: "iOS", active: landscapeArea, turn: 2, wantClick: mnk.MockClick{X: 800, Y: 300}},
		{name: "three-quarter turn is corrected", deviceType: "iOS", active: landscapeArea, turn: 3, wantClick: mnk.MockClick{X: 700, Y: 800}},
		{name: "portrait is not checked", deviceType: "iOS", active: portraitArea, turn: 2, wantClick: mnk.MockClick{X: 200, Y: 700}},
		{name: "a landscape Mac display is not checked", deviceType: "macOS", active: landscapeArea, turn: 2, wantClick: mnk.MockClick{X: 200, Y: 700}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &screen.ScreenState{}
			state.UpdateActiveArea(1920, 1080, tc.active)
			mock := mnk.NewMockProvider()
			region := image.Rect(tc.active.X, tc.active.Y, tc.active.X+tc.active.Width, tc.active.Y+tc.active.Height)
			sim := &simulatedPointer{mock: mock, region: region, turn: tc.turn, frameW: 1920, frameH: 1080}
			corrector := newLandscapePointerCorrector(sim.capture, state, nil)
			corrector.settle = 0
			corrector.setDeviceTypeFunc(func() string { return tc.deviceType })

			tool := &TouchGestureTool{mnkProvider: mock, screen: state, landscape: corrector}
			if _, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":200,"y":700}}`); err != nil {
				t.Fatalf("tap: %v", err)
			}
			clicks := mock.ClickCalls()
			if len(clicks) != 1 || clicks[0].X != tc.wantClick.X || clicks[0].Y != tc.wantClick.Y {
				t.Fatalf("clicks = %+v, want one at (%v,%v)", clicks, tc.wantClick.X, tc.wantClick.Y)
			}
			// Where the simulated phone puts the press is where the model aimed.
			if landed := landscapeLanding(sim, clicks[0]); tc.active == landscapeArea && tc.deviceType == "iOS" && landed != (mnk.Point{X: 200, Y: 700}) {
				t.Fatalf("tap landed at %+v, want (200,700)", landed)
			}
		})
	}
}

func landscapeLanding(sim *simulatedPointer, click mnk.MockClick) mnk.Point {
	x, y := sim.turn.land(click.X, click.Y)
	return mnk.Point{X: x, Y: y}
}

func TestLandscapePointerKeepsDragOrientationForRelease(t *testing.T) {
	state := &screen.ScreenState{}
	area := screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true}
	state.UpdateActiveArea(1920, 1080, area)
	mock := mnk.NewMockProvider()
	sim := &simulatedPointer{mock: mock, region: image.Rect(0, 98, 1920, 982), turn: 2, frameW: 1920, frameH: 1080}
	corrector := newLandscapePointerCorrector(sim.capture, state, nil)
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })

	if err := corrector.session(mock).DragStart(context.Background(), 150, 250, mnk.ButtonLeft); err != nil {
		t.Fatalf("drag_start: %v", err)
	}
	movesBefore := len(mock.MoveCalls())
	// The release comes in a later tool call and must not move the held
	// contact to check again.
	if err := corrector.session(mock).DragRelease(context.Background(), 600, 400); err != nil {
		t.Fatalf("drag_release: %v", err)
	}
	if len(mock.MoveCalls()) != movesBefore {
		t.Fatalf("drag_release probed the pointer while the contact was held")
	}
	if got := mock.DragReleaseCalls(); len(got) != 1 || got[0] != (mnk.MockMove{X: 400, Y: 600}) {
		t.Fatalf("drag releases = %+v, want one at the turned point (400,600)", got)
	}
}

func TestLandscapePointerReenumeratesAfterOrientationTurn(t *testing.T) {
	landscapeArea := screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true}
	portraitArea := screen.ScreenActiveArea{X: 710, Y: 0, Width: 498, Height: 1080, Valid: true}
	cases := []struct {
		name       string
		deviceType string
		areas      []screen.ScreenActiveArea
		want       int
	}{
		{name: "portrait stays enumerated", deviceType: "iOS", areas: []screen.ScreenActiveArea{portraitArea, portraitArea}, want: 0},
		{name: "first landscape sighting", deviceType: "iOS", areas: []screen.ScreenActiveArea{landscapeArea, landscapeArea}, want: 1},
		{name: "portrait to landscape and back", deviceType: "iOS", areas: []screen.ScreenActiveArea{portraitArea, landscapeArea, landscapeArea, portraitArea}, want: 2},
		{name: "other device types are left alone", deviceType: "macOS", areas: []screen.ScreenActiveArea{portraitArea, landscapeArea}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := &screen.ScreenState{}
			mock := mnk.NewMockProvider()
			calls := 0
			corrector := newLandscapePointerCorrector(func(context.Context) (lumaFrame, error) {
				return lumaFrame{}, errNoFrameForTest
			}, state, func(context.Context) error {
				calls++
				return nil
			})
			corrector.settle = 0
			corrector.setDeviceTypeFunc(func() string { return tc.deviceType })
			for _, area := range tc.areas {
				state.UpdateActiveArea(1920, 1080, area)
				if err := corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0); err != nil {
					t.Fatalf("click: %v", err)
				}
			}
			if calls != tc.want {
				t.Fatalf("re-enumerations = %d, want %d", calls, tc.want)
			}
		})
	}
}

var errNoFrameForTest = errors.New("no frame")

// A check that sees the whole screen change, as during an app switch, must
// not fall back on a turn learned in the previous app.
func TestLandscapePointerDropsStaleTurnWhileScreenChanges(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true})
	mock := mnk.NewMockProvider()
	frame := 0
	corrector := newLandscapePointerCorrector(func(context.Context) (lumaFrame, error) {
		frame++
		f := lumaFrame{width: 1920, height: 1080, stride: 1920, y: make([]byte, 1920*1080)}
		for i := range f.y {
			f.y[i] = byte(40 * (frame % 2))
		}
		return f, nil
	}, state, nil)
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })
	corrector.remember(1, image.Rect(0, 98, 1920, 982))

	if err := corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0); err != nil {
		t.Fatalf("click: %v", err)
	}
	if clicks := mock.ClickCalls(); len(clicks) != 1 || clicks[0].X != 200 || clicks[0].Y != 700 {
		t.Fatalf("clicks = %+v, want the point sent unturned", clicks)
	}
	if frame != 2*landscapePointerProbeAttempts {
		t.Fatalf("captured %d frames, want a repeated check (%d)", frame, 2*landscapePointerProbeAttempts)
	}
}

// Turning the phone can change how iOS maps the pointer, so a decision made
// while the phone was held one way is not reused once it is held another way.
func TestLandscapePointerForgetsTurnWhenPhoneIsTurned(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true})
	region := image.Rect(0, 98, 1920, 982)
	held := "portrait"
	corrector := newLandscapePointerCorrector(func(context.Context) (lumaFrame, error) {
		return lumaFrame{}, errNoFrameForTest
	}, state, nil)
	corrector.setDeviceOrientationFunc(func() (string, time.Time) { return held, time.Now() })
	corrector.remember(1, region)

	if got := corrector.recentTurn(region); got != 1 {
		t.Fatalf("turn while held the same way = %s, want turned 90", got)
	}
	held = "landscape_left"
	if got := corrector.recentTurn(region); got != 0 {
		t.Fatalf("turn after the phone turned = %s, want upright", got)
	}
}

// With the phone held in landscape the decided turn is reused without moving
// the pointer again; held upright, flat, or unknown it is checked every call.
func TestLandscapePointerReusesTurnOnlyWhileHeldInLandscape(t *testing.T) {
	cases := []struct {
		held      string
		wantProbe bool
	}{
		{held: "landscape_left", wantProbe: false},
		{held: "landscape_right", wantProbe: false},
		{held: "portrait", wantProbe: true},
		{held: "flat", wantProbe: true},
		{held: "", wantProbe: true},
	}
	area := screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true}
	for _, tc := range cases {
		t.Run("held="+tc.held, func(t *testing.T) {
			state := &screen.ScreenState{}
			state.UpdateActiveArea(1920, 1080, area)
			mock := mnk.NewMockProvider()
			sim := &simulatedPointer{mock: mock, region: image.Rect(0, 98, 1920, 982), turn: 1, frameW: 1920, frameH: 1080}
			corrector := newLandscapePointerCorrector(sim.capture, state, nil)
			corrector.settle = 0
			corrector.setDeviceTypeFunc(func() string { return "iOS" })
			corrector.setDeviceOrientationFunc(func() (string, time.Time) { return tc.held, time.Now() })

			if err := corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0); err != nil {
				t.Fatalf("first click: %v", err)
			}
			movesAfterFirst := len(mock.MoveCalls())
			if err := corrector.session(mock).Click(context.Background(), 800, 300, mnk.ButtonLeft, 0); err != nil {
				t.Fatalf("second click: %v", err)
			}
			if probed := len(mock.MoveCalls()) > movesAfterFirst; probed != tc.wantProbe {
				t.Fatalf("second click probed = %v, want %v", probed, tc.wantProbe)
			}
			clicks := mock.ClickCalls()
			if x, y := sim.turn.land(clicks[1].X, clicks[1].Y); x != 800 || y != 300 {
				t.Fatalf("second click landed at (%v,%v), want (800,300)", x, y)
			}
		})
	}
}

// A swipe that starts at the screen centre cannot be checked there, since
// every turn lands the centre in the same spot; the rest of the swipe must
// still be turned.
func TestLandscapePointerTurnsSwipeStartingAtCentre(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true})
	mock := mnk.NewMockProvider()
	sim := &simulatedPointer{mock: mock, region: image.Rect(0, 98, 1920, 982), turn: 1, frameW: 1920, frameH: 1080}
	corrector := newLandscapePointerCorrector(sim.capture, state, nil)
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })

	if err := corrector.session(mock).Swipe(context.Background(), [][2]float64{{500, 500}, {500, 200}}, mnk.ButtonLeft); err != nil {
		t.Fatalf("swipe: %v", err)
	}
	swipes := mock.SwipeCalls()
	if len(swipes) != 1 {
		t.Fatalf("swipes = %+v, want one", swipes)
	}
	end := swipes[0].Path[len(swipes[0].Path)-1]
	if x, y := sim.turn.land(end[0], end[1]); x != 500 || y != 200 {
		t.Fatalf("swipe end landed at (%v,%v), want (500,200)", x, y)
	}
}

// After a busy screen rejected the earlier decision, a later check that
// finds nothing must not bring that decision back.
func TestLandscapePointerBusyScreenForgetsEarlierTurn(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true})
	region := image.Rect(0, 98, 1920, 982)
	busy := true
	frame := 0
	corrector := newLandscapePointerCorrector(func(context.Context) (lumaFrame, error) {
		frame++
		f := lumaFrame{width: 1920, height: 1080, stride: 1920, y: make([]byte, 1920*1080)}
		if busy {
			for i := range f.y {
				f.y[i] = byte(40 * (frame % 2))
			}
		}
		return f, nil
	}, state, nil)
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })
	corrector.remember(1, region)

	mock := mnk.NewMockProvider()
	_ = corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0)
	busy = false
	if err := corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0); err != nil {
		t.Fatalf("click: %v", err)
	}
	if clicks := mock.ClickCalls(); clicks[1].X != 200 || clicks[1].Y != 700 {
		t.Fatalf("second click = %+v, want the point sent unturned", clicks[1])
	}
}

// A modified-keyboard profile switch also reattaches the pointer, so the
// corrector must not think the pointer is still bounded for the orientation
// it last enumerated in itself.
func TestLandscapePointerTracksReattachByKeyboardIsolation(t *testing.T) {
	landscapeArea := screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true}
	portraitArea := screen.ScreenActiveArea{X: 710, Y: 0, Width: 498, Height: 1080, Valid: true}
	state := &screen.ScreenState{}
	calls := 0
	corrector := newLandscapePointerCorrector(func(context.Context) (lumaFrame, error) {
		return lumaFrame{}, errNoFrameForTest
	}, state, func(context.Context) error {
		calls++
		return nil
	})
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })
	mock := mnk.NewMockProvider()

	state.UpdateActiveArea(1920, 1080, landscapeArea)
	_ = corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0)
	// A keyboard shortcut reattaches the pointer while the phone is upright.
	state.UpdateActiveArea(1920, 1080, portraitArea)
	corrector.noteReenumerated()
	state.UpdateActiveArea(1920, 1080, landscapeArea)
	_ = corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0)

	if calls != 2 {
		t.Fatalf("re-enumerations = %d, want 2 (first landscape, then back to landscape after the portrait reattach)", calls)
	}
}

// A measurement where one patch dominates but another also changed was taken
// while the screen moved; it must not be accepted as the pointer's turn.
func TestLandscapePointerRejectsDominantPatchOnBusyScreen(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 0, Y: 98, Width: 1920, Height: 884, Valid: true})
	mock := mnk.NewMockProvider()
	region := image.Rect(0, 98, 1920, 982)
	sim := &simulatedPointer{mock: mock, region: region, turn: 1, frameW: 1920, frameH: 1080}
	frame := 0
	corrector := newLandscapePointerCorrector(func(ctx context.Context) (lumaFrame, error) {
		f, err := sim.capture(ctx)
		frame++
		if frame%2 == 0 {
			// An animation next to where an upright pointer would land.
			c := landscapeFramePoint(region, 200, 700)
			for y := c.Y - 6; y <= c.Y+6; y++ {
				for x := c.X - 6; x <= c.X+6; x++ {
					f.y[y*f.stride+x] = 20
				}
			}
		}
		return f, err
	}, state, nil)
	corrector.settle = 0
	corrector.setDeviceTypeFunc(func() string { return "iOS" })

	if err := corrector.session(mock).Click(context.Background(), 200, 700, mnk.ButtonLeft, 0); err != nil {
		t.Fatalf("click: %v", err)
	}
	if got := corrector.recentTurn(region); got != 0 {
		t.Fatalf("cached turn = %s after a busy measurement, want none", got)
	}
}

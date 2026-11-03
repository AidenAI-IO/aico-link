package mnk

import "context"

// MockProvider is a recording Provider for tests and adapter contract checks.
type MockProvider struct {
	clicks       []MockClick
	doubleClicks []MockDoubleClick
	swipes       []MockSwipe
	dragStarts   []MockClick
	dragReleases []MockMove
	dragActive   bool
	keypresses   []MockKeypress
	moves        []MockMove
	scrolls      []MockScroll
	touchActions []TouchAction
}

type MockClick struct {
	X, Y   float64
	Button string
	HoldMs int
}

type MockDoubleClick struct {
	X, Y   float64
	Button string
}

type MockSwipe struct {
	Path         [][2]float64
	Button       string
	DurationMs   int
	HoldBeforeMs int
	HoldAfterMs  int
	Steps        int
}

type MockKeypress struct {
	Keys []string
}

type MockMove struct {
	X, Y float64
}

type MockScroll struct {
	ScrollX, ScrollY int
}

func NewMockProvider() *MockProvider {
	return &MockProvider{}
}

func (m *MockProvider) Click(ctx context.Context, x, y float64, button string, holdMs int) error {
	_ = ctx
	m.clicks = append(m.clicks, MockClick{X: x, Y: y, Button: button, HoldMs: holdMs})
	return nil
}

func (m *MockProvider) DoubleClick(ctx context.Context, x, y float64, button string) error {
	_ = ctx
	m.doubleClicks = append(m.doubleClicks, MockDoubleClick{X: x, Y: y, Button: button})
	return nil
}

func (m *MockProvider) Swipe(ctx context.Context, path [][2]float64, button string) error {
	return m.SwipeWithOptions(ctx, path, button, SwipeOptions{})
}

func (m *MockProvider) SwipeWithDuration(ctx context.Context, path [][2]float64, button string, durationMs int) error {
	return m.SwipeWithOptions(ctx, path, button, SwipeOptions{DurationMs: durationMs})
}

func (m *MockProvider) SwipeWithOptions(ctx context.Context, path [][2]float64, button string, options SwipeOptions) error {
	_ = ctx
	if options.DurationMs <= 0 {
		options.DurationMs = defaultSwipeGestureDurationMs
	}
	if options.Steps <= 0 {
		options.Steps = defaultSwipeSteps
	}
	m.swipes = append(m.swipes, MockSwipe{
		Path:         path,
		Button:       button,
		DurationMs:   options.DurationMs,
		HoldBeforeMs: options.HoldBeforeMs,
		HoldAfterMs:  options.HoldAfterMs,
		Steps:        options.Steps,
	})
	return nil
}

func (m *MockProvider) DragStart(ctx context.Context, x, y float64, button string) error {
	_ = ctx
	if m.dragActive {
		return InvalidArguments("drag_start is already active")
	}
	m.dragStarts = append(m.dragStarts, MockClick{X: x, Y: y, Button: button})
	m.dragActive = true
	return nil
}

func (m *MockProvider) DragRelease(ctx context.Context, x, y float64) error {
	_ = ctx
	if !m.dragActive {
		return InvalidArguments("drag_release requires an active drag_start")
	}
	m.dragReleases = append(m.dragReleases, MockMove{X: x, Y: y})
	m.dragActive = false
	return nil
}

func (m *MockProvider) Keypress(ctx context.Context, keys []string) error {
	_ = ctx
	m.keypresses = append(m.keypresses, MockKeypress{Keys: keys})
	return nil
}

func (m *MockProvider) Move(ctx context.Context, x, y float64) error {
	_ = ctx
	m.moves = append(m.moves, MockMove{X: x, Y: y})
	return nil
}

func (m *MockProvider) Scroll(ctx context.Context, scrollX, scrollY int) error {
	_ = ctx
	m.scrolls = append(m.scrolls, MockScroll{ScrollX: scrollX, ScrollY: scrollY})
	return nil
}

func (m *MockProvider) TouchActions(ctx context.Context, actions []TouchAction) error {
	_ = ctx
	m.touchActions = append(m.touchActions, cloneTouchActions(actions)...)
	return nil
}

func (m *MockProvider) TouchActionCalls() []TouchAction {
	if m == nil {
		return nil
	}
	return cloneTouchActions(m.touchActions)
}

func (m *MockProvider) Reset() {
	m.clicks = nil
	m.doubleClicks = nil
	m.swipes = nil
	m.dragStarts = nil
	m.dragReleases = nil
	m.dragActive = false
	m.keypresses = nil
	m.moves = nil
	m.scrolls = nil
	m.touchActions = nil
}

func cloneTouchActions(actions []TouchAction) []TouchAction {
	if actions == nil {
		return nil
	}
	copyActions := make([]TouchAction, len(actions))
	copy(copyActions, actions)
	for i := range copyActions {
		if actions[i].Point != nil {
			point := *actions[i].Point
			copyActions[i].Point = &point
		}
	}
	return copyActions
}

func (m *MockProvider) KeypressCalls() []MockKeypress {
	if m == nil {
		return nil
	}
	out := make([]MockKeypress, len(m.keypresses))
	copy(out, m.keypresses)
	return out
}

func (m *MockProvider) ClickCalls() []MockClick {
	if m == nil {
		return nil
	}
	out := make([]MockClick, len(m.clicks))
	copy(out, m.clicks)
	return out
}

func (m *MockProvider) MoveCalls() []MockMove {
	if m == nil {
		return nil
	}
	out := make([]MockMove, len(m.moves))
	copy(out, m.moves)
	return out
}

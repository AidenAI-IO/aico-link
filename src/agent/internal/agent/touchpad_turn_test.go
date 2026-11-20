package agent

import (
	"context"
	"testing"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
)

type fakeTouchpadTurn struct{ turn mnk.TouchpadTurn }

func (f *fakeTouchpadTurn) SetTouchpadTurn(turn mnk.TouchpadTurn) { f.turn = turn }
func (f *fakeTouchpadTurn) TouchpadTurn() mnk.TouchpadTurn        { return f.turn }
func (f *fakeTouchpadTurn) HoldTouch(context.Context, float64, float64, func() error) error {
	return nil
}

func TestTouchpadTurnFollowsScreenshotOrientation(t *testing.T) {
	state := &screen.ScreenState{}
	provider := &fakeTouchpadTurn{}
	c := &touchpadTurnCorrector{provider: provider, screen: state}
	jpeg := []byte{0xff}

	state.UpdateScreenshot(jpeg, 1920, 1080)
	if !c.prepare() {
		t.Fatal("landscape with unknown direction must ask for a probe")
	}
	c.remember(mnk.TouchpadTurnedRight)
	if c.prepare() || provider.turn != mnk.TouchpadTurnedRight {
		t.Fatalf("learned direction not applied: %v", provider.turn)
	}
	// No screenshot since an invalidation is not evidence of portrait.
	state.InvalidateCapture()
	if c.prepare() || provider.turn != mnk.TouchpadTurnedRight {
		t.Fatalf("unknown dimensions reset the turn: %v", provider.turn)
	}
	state.UpdateScreenshot(jpeg, 1080, 1920)
	if c.prepare() || provider.turn != mnk.TouchpadUpright {
		t.Fatalf("portrait did not reset the turn: %v", provider.turn)
	}
	state.UpdateScreenshot(jpeg, 1920, 1080)
	if !c.prepare() {
		t.Fatal("leaving landscape must forget the learned direction")
	}
}

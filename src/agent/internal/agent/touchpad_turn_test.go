package agent

import (
	"context"
	"encoding/json"
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
	if !c.prepare() || provider.turn != touchpadDefaultLandscapeTurn {
		t.Fatalf("landscape must start from the unconfirmed default, got %v", provider.turn)
	}
	// A touch that reacted at its mirror flips and confirms the direction.
	if _, err := c.confirm(true); err != nil {
		t.Fatal(err)
	}
	flipped := oppositeTouchpadTurn(touchpadDefaultLandscapeTurn)
	if c.prepare() || provider.turn != flipped {
		t.Fatalf("mirror report not applied: %v", provider.turn)
	}
	// No screenshot since an invalidation is not evidence of portrait.
	state.InvalidateCapture()
	if c.prepare() || provider.turn != flipped {
		t.Fatalf("unknown dimensions reset the turn: %v", provider.turn)
	}
	state.UpdateScreenshot(jpeg, 1080, 1920)
	if c.prepare() || provider.turn != mnk.TouchpadUpright {
		t.Fatalf("portrait did not reset the turn: %v", provider.turn)
	}
	if _, err := c.confirm(false); err == nil {
		t.Fatal("orientation_set in portrait must fail")
	}
	state.UpdateScreenshot(jpeg, 1920, 1080)
	if !c.prepare() || provider.turn != touchpadDefaultLandscapeTurn {
		t.Fatalf("leaving landscape must drop the confirmed direction: %v", provider.turn)
	}
}

func TestTurnCheckSequenceKeepsExplicitInterval(t *testing.T) {
	for input, want := range map[string]string{
		`{"type":"tap_sequence","points":[{"x":500,"y":500},{"x":900,"y":500}]}`:                   "1000",
		`{"type":"tap_sequence","points":[{"x":500,"y":500},{"x":900,"y":500}],"interval_ms":300}`: "300",
		`{"type":"tap","point":{"x":500,"y":500}}`:                                                 "",
	} {
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(withTurnCheckSequenceInterval(input)), &args); err != nil {
			t.Fatal(err)
		}
		if got := string(args["interval_ms"]); got != want {
			t.Errorf("%s: interval_ms=%q, want %q", input, got, want)
		}
	}
}

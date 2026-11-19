package agent

import (
	"context"
	"fmt"
	"image"
	"sync"
	"time"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/logging"
)

// The iOS touchpad reports in the phone panel's own coordinates, like a
// finger on the glass. The HDMI picture is always upright, so in landscape a
// point chosen on the screenshot lands turned by a quarter, in a direction set
// by which way the phone is turned. The screenshot cannot tell the two
// landscape directions apart, so a probe press learns it. A direct turn from
// one landscape direction to the other keeps the screenshot landscape and is
// not noticed; touches then land mirrored until the screen leaves landscape.
const (
	// touchpadTurnProbeHold keeps the probe contact down long enough for the
	// pressed control to draw its highlight before the frame is captured.
	touchpadTurnProbeHold = 250 * time.Millisecond
	// touchpadTurnProbeMinDiff is the smallest patch change that counts as a
	// press highlight. A pressed key changes its patch by tens of luma steps;
	// an untouched area stays within a few.
	touchpadTurnProbeMinDiff = 6.0
	// touchpadTurnProbeMargin is how much more one candidate must change than
	// the other before the probe trusts the result.
	touchpadTurnProbeMargin = 2.0
)

// touchpadTurnCorrector keeps the touchpad turn in step with the screen.
type touchpadTurnCorrector struct {
	provider mnk.TouchpadTurnProvider
	capture  frameCapturer
	screen   *screen.ScreenState

	mu sync.Mutex
	// landscapeTurn is the learned landscape direction; zero when unknown.
	landscapeTurn mnk.TouchpadTurn
}

func newTouchpadTurnCorrector(provider mnk.Provider, capture frameCapturer, screenState *screen.ScreenState) *touchpadTurnCorrector {
	turnProvider, ok := provider.(mnk.TouchpadTurnProvider)
	if !ok || capture == nil || screenState == nil {
		return nil
	}
	return &touchpadTurnCorrector{provider: turnProvider, capture: capture, screen: screenState}
}

// landscape reports whether the latest screenshot is wider than tall.
func (c *touchpadTurnCorrector) landscape() (bool, bool) {
	width, height, ok := c.screen.ScreenshotDimensions()
	if !ok {
		return false, false
	}
	return width > height, true
}

// prepare sets the provider's turn for an action on the latest screenshot.
// In landscape with an unknown direction it returns needsProbe; the caller
// then calls probe with the action's first point.
func (c *touchpadTurnCorrector) prepare() (needsProbe bool) {
	if c == nil {
		return false
	}
	landscape, ok := c.landscape()
	if !ok || !landscape {
		// The phone may come back turned the other way, so a learned
		// landscape direction only holds until the screen leaves landscape.
		c.forget()
		c.provider.SetTouchpadTurn(mnk.TouchpadUpright)
		return false
	}
	c.mu.Lock()
	turn := c.landscapeTurn
	c.mu.Unlock()
	if turn == mnk.TouchpadUpright {
		return true
	}
	c.provider.SetTouchpadTurn(turn)
	return false
}

func (c *touchpadTurnCorrector) remember(turn mnk.TouchpadTurn) {
	c.mu.Lock()
	c.landscapeTurn = turn
	c.mu.Unlock()
}

// forget drops the learned direction.
func (c *touchpadTurnCorrector) forget() {
	if c == nil {
		return
	}
	c.remember(mnk.TouchpadUpright)
}

// probe learns the landscape direction with a press at point, which the
// model chose as a control whose press is harmless. Sent with TurnedLeft, the
// contact lands at point when the phone is TurnedLeft and at the mirror of
// point through the screen centre when it is TurnedRight. The frame is
// compared at both spots while the contact is held; the spot showing the
// press highlight is where it landed. The release completes a tap there.
func (c *touchpadTurnCorrector) probe(ctx context.Context, point normalizedPoint) (mnk.TouchpadTurn, error) {
	baseline, err := c.capture(ctx)
	if err != nil {
		return mnk.TouchpadUpright, fmt.Errorf("capture before probe: %w", err)
	}
	c.provider.SetTouchpadTurn(mnk.TouchpadTurnedLeft)
	var pressed lumaFrame
	err = c.provider.HoldTouch(ctx, point.X, point.Y, func() error {
		if err := sleepWithContext(ctx, touchpadTurnProbeHold); err != nil {
			return err
		}
		var captureErr error
		pressed, captureErr = c.capture(ctx)
		return captureErr
	})
	if err != nil {
		return mnk.TouchpadUpright, fmt.Errorf("probe touch: %w", err)
	}
	region := tapSequenceCompareRegion(c.screen, pressed)
	radius := max(4, int(float64(region.Dx())*tapSequenceTargetPatchFraction+0.5))
	locate := func(p normalizedPoint) image.Point {
		return image.Point{
			X: region.Min.X + int(p.X/1000*float64(region.Dx()-1)+0.5),
			Y: region.Min.Y + int(p.Y/1000*float64(region.Dy()-1)+0.5),
		}
	}
	atPoint := lumaPatchDiff(baseline, pressed, locate(point), radius, 0)
	mirror := normalizedPoint{X: 1000 - point.X, Y: 1000 - point.Y}
	atMirror := lumaPatchDiff(baseline, pressed, locate(mirror), radius, 0)
	logging.Infof("agent", "touchpad_turn", "probe point=(%.0f,%.0f) diff_point=%.1f diff_mirror=%.1f", point.X, point.Y, atPoint, atMirror)
	switch {
	case atPoint >= touchpadTurnProbeMinDiff && atPoint > atMirror+touchpadTurnProbeMargin:
		return mnk.TouchpadTurnedLeft, nil
	case atMirror >= touchpadTurnProbeMinDiff && atMirror > atPoint+touchpadTurnProbeMargin:
		return mnk.TouchpadTurnedRight, nil
	default:
		return mnk.TouchpadUpright, fmt.Errorf("probe at (%.0f,%.0f) showed no press highlight (change %.1f there, %.1f at its mirror)", point.X, point.Y, atPoint, atMirror)
	}
}

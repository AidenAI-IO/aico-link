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
// landscape directions apart. A landscape screen starts from the direction
// apps that force landscape were seen to use; it stays unconfirmed until the
// model sees where a touch reacted and calls orientation_set, which can also
// flip a confirmed direction when a later touch reacts at its mirror.
const (
	// touchpadDefaultLandscapeTurn is the direction measured with an app
	// forcing landscape on an upright iPhone.
	touchpadDefaultLandscapeTurn = mnk.TouchpadTurnedRight
	// touchpadTurnPendingObserveDelay replaces the stable-screen wait after
	// an unconfirmed touch: the reaction that shows the direction, such as
	// video controls, can fade before a moving screen settles.
	touchpadTurnPendingObserveDelay = 300 * time.Millisecond
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
	// landscapeTurn is the landscape direction in use; upright while the
	// screen has not been landscape since it was last portrait.
	landscapeTurn mnk.TouchpadTurn
	// confirmed reports that a touch was seen landing with landscapeTurn.
	confirmed bool
}

func newTouchpadTurnCorrector(provider mnk.Provider, capture frameCapturer, screenState *screen.ScreenState) *touchpadTurnCorrector {
	turnProvider, ok := provider.(mnk.TouchpadTurnProvider)
	if !ok || capture == nil || screenState == nil {
		return nil
	}
	c := &touchpadTurnCorrector{provider: turnProvider, capture: capture, screen: screenState}
	screenState.SetScreenshotObserver(c.observeScreenshot)
	return c
}

// observeScreenshot ends the landscape turn on any portrait screenshot, not
// only one a touch reads: the phone may come back turned the other way.
func (c *touchpadTurnCorrector) observeScreenshot(width, height int) {
	if height >= width {
		c.reset()
	}
}

// reset drops the landscape turn after a portrait screenshot.
func (c *touchpadTurnCorrector) reset() {
	c.mu.Lock()
	wasLandscape := c.landscapeTurn != mnk.TouchpadUpright
	c.landscapeTurn = mnk.TouchpadUpright
	c.confirmed = false
	c.mu.Unlock()
	if wasLandscape {
		c.provider.SetTouchpadTurn(mnk.TouchpadUpright)
	}
}

// frameLandscape reads a current frame and reports whether the phone picture
// in it, the frame without its black borders, is wider than tall. It uses no
// stored screen state, which may predate a turn.
func (c *touchpadTurnCorrector) frameLandscape(ctx context.Context) (bool, error) {
	frame, err := c.capture(ctx)
	if err != nil {
		return false, err
	}
	content := lumaContentBounds(frame)
	if content.Empty() {
		return false, fmt.Errorf("the captured frame is black")
	}
	return content.Dx() > content.Dy(), nil
}

// lumaContentBounds returns the smallest rectangle holding every sampled row
// and column brighter than black, the phone picture inside HDMI borders.
func lumaContentBounds(frame lumaFrame) image.Rectangle {
	const step, black = 4, 16
	if !frame.valid() {
		return image.Rectangle{}
	}
	minX, minY, maxX, maxY := frame.width, frame.height, -1, -1
	for y := 0; y < frame.height; y += step {
		row := frame.y[y*frame.stride : y*frame.stride+frame.width]
		for x := 0; x < frame.width; x += step {
			if row[x] > black {
				minX, maxX = min(minX, x), max(maxX, x)
				minY, maxY = min(minY, y), max(maxY, y)
			}
		}
	}
	if maxX < 0 {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// landscape reports whether the latest screenshot is wider than tall.
func (c *touchpadTurnCorrector) landscape() (bool, bool) {
	width, height, ok := c.screen.ScreenshotDimensions()
	if !ok {
		return false, false
	}
	return width > height, true
}

// prepare sets the provider's turn for an action on the latest screenshot and
// reports whether that turn is still an unconfirmed landscape guess.
func (c *touchpadTurnCorrector) prepare() (pending bool) {
	if c == nil {
		return false
	}
	landscape, ok := c.landscape()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !ok:
		// No screenshot since the last capture invalidation: the orientation
		// has not been seen to change, so keep the turn last applied.
	case !landscape:
		// The phone may come back turned the other way, so a landscape
		// direction only holds until the screen leaves landscape. The
		// screenshot observer normally did this already.
		c.landscapeTurn = mnk.TouchpadUpright
		c.confirmed = false
		c.provider.SetTouchpadTurn(mnk.TouchpadUpright)
	default:
		if c.landscapeTurn == mnk.TouchpadUpright {
			c.landscapeTurn = touchpadDefaultLandscapeTurn
		}
		c.provider.SetTouchpadTurn(c.landscapeTurn)
	}
	return c.landscapeTurn != mnk.TouchpadUpright && !c.confirmed
}

// landscapeActive reports whether a landscape turn is in use.
func (c *touchpadTurnCorrector) landscapeActive() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.landscapeTurn != mnk.TouchpadUpright
}

// pending reports whether the landscape turn in use is still unconfirmed.
func (c *touchpadTurnCorrector) pending() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.landscapeTurn != mnk.TouchpadUpright && !c.confirmed
}

// remember confirms turn as the landscape direction.
func (c *touchpadTurnCorrector) remember(turn mnk.TouchpadTurn) {
	c.mu.Lock()
	c.landscapeTurn = turn
	c.confirmed = turn != mnk.TouchpadUpright
	c.mu.Unlock()
	c.provider.SetTouchpadTurn(turn)
}

// confirm records where the model saw the last touch react: at its aimed
// point (mirrored=false) or at the mirror through the screen centre.
func (c *touchpadTurnCorrector) confirm(mirrored bool) (mnk.TouchpadTurn, error) {
	c.mu.Lock()
	turn := c.landscapeTurn
	c.mu.Unlock()
	if turn == mnk.TouchpadUpright {
		return turn, fmt.Errorf("the screen is not in landscape, so there is no direction to set")
	}
	if mirrored {
		turn = oppositeTouchpadTurn(turn)
	}
	c.remember(turn)
	return turn, nil
}

func oppositeTouchpadTurn(turn mnk.TouchpadTurn) mnk.TouchpadTurn {
	switch turn {
	case mnk.TouchpadTurnedLeft:
		return mnk.TouchpadTurnedRight
	case mnk.TouchpadTurnedRight:
		return mnk.TouchpadTurnedLeft
	default:
		return turn
	}
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

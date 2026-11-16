package agent

import (
	"context"
	"image"
	"math"
	"sync"
	"time"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/logging"
)

// An iPhone mirrored over HDMI always shows its picture upright, but iOS keeps
// two pointer faults that the board corrects before a pointer action:
//
//   - After the phone turns between portrait and landscape, iOS keeps the
//     pointer bounds of the orientation the USB pointer was enumerated in, so
//     in landscape the pointer cannot pass the portrait width. Detaching and
//     reattaching the pointer in the new orientation restores the full screen.
//   - In landscape, iOS can map the absolute pointer surface turned by a
//     quarter, half, or three-quarter turn against the upright picture. Both a
//     180-degree turn ((x, y) lands at (1000-x, 1000-y)) and a 90-degree turn
//     ((x, y) lands at (y, 1000-x)) have been seen. The frame does not show the
//     turn, so before a landscape pointer action the pointer is moved onto the
//     target and the frame shows which of the four turned positions it reached.
//     One check covers every point of the tool call.
const (
	// landscapePointerProbeSettle lets the iOS pointer finish its animated
	// move, like tapSequencePointerParkDelay.
	landscapePointerProbeSettle = 150 * time.Millisecond
	// landscapePointerProbeRadiusFraction sizes the compared patch around a
	// candidate pointer position as a share of the phone screen's short side.
	// The iOS pointer disc is about 2.4% of it, so the patch holds the disc
	// and most of a button hover highlight.
	landscapePointerProbeRadiusFraction = 0.035
	// landscapePointerProbeParkDistance is how far from the screen centre, in
	// normalized units, the pointer is parked before the check. The park point
	// sits an eighth turn from the target around the centre, so none of its
	// four possible landings is near one of the target's four.
	landscapePointerProbeParkDistance = 350.0
	// landscapePointerProbeLumaDelta is the luminance change (0-255) that
	// counts a pixel as changed. Raw frames of a still screen stay within a
	// few levels.
	landscapePointerProbeLumaDelta = 12
	// landscapePointerProbeMinChanged is the share of a patch that must change
	// for the pointer to count as arrived there.
	landscapePointerProbeMinChanged = 0.04
	// landscapePointerProbeDominance is how many times more the best candidate
	// patch must change than any other for the check to decide.
	landscapePointerProbeDominance = 3.0
	// landscapePointerProbeReuseFor bounds how long an earlier decided check
	// stands in for one that could not decide, on the same screen area.
	landscapePointerProbeReuseFor = 2 * time.Minute
	// landscapePointerProbeAttempts is how many park-and-move checks run
	// before falling back. A check where several candidate patches changed
	// saw the screen itself change, for example during an app switch, and
	// is repeated rather than trusted.
	landscapePointerProbeAttempts = 2
)

// landscapePointerCorrector keeps what the orientation checks learned across
// tool calls. Each pointer tool call gets its own provider from session.
type landscapePointerCorrector struct {
	capture frameCapturer
	screen  *screen.ScreenState
	settle  time.Duration

	// reenumerate detaches and reattaches the USB pointer; nil disables it.
	reenumerate func(context.Context) error

	mu           sync.Mutex
	deviceTypeFn func() string
	// heldFn reports how the companion app last saw the phone held, read
	// from gravity; "" when unknown.
	heldFn   func() (string, time.Time)
	last     landscapePointerDecision
	dragTurn pointerTurn
	// enumerated is the orientation the gadget was last enumerated in, as
	// seen by this process; unknown until the first pointer action.
	enumerated screenOrientation
}

type screenOrientation int

const (
	orientationUnknown screenOrientation = iota
	orientationPortrait
	orientationLandscape
)

func (o screenOrientation) String() string {
	switch o {
	case orientationPortrait:
		return "portrait"
	case orientationLandscape:
		return "landscape"
	default:
		return "unknown"
	}
}

type landscapePointerDecision struct {
	turn   pointerTurn
	region image.Rectangle
	held   string
	at     time.Time
}

// pointerTurn is how many quarter turns iOS applies to a sent point: with
// turn 1 a point sent for (x, y) lands at (y, 1000-x).
type pointerTurn int

const pointerTurns = 4

func (t pointerTurn) String() string {
	return [...]string{"upright", "turned 90", "turned 180", "turned 270"}[t%pointerTurns]
}

// land returns where a point sent as (x, y) lands under turn t.
func (t pointerTurn) land(x, y float64) (float64, float64) {
	for range t % pointerTurns {
		x, y = y, 1000-x
	}
	return x, y
}

// send returns the point to send so that it lands on (x, y) under turn t.
func (t pointerTurn) send(x, y float64) (float64, float64) {
	return ((pointerTurns - t%pointerTurns) % pointerTurns).land(x, y)
}

func newLandscapePointerCorrector(capture frameCapturer, screenState *screen.ScreenState, reenumerate func(context.Context) error) *landscapePointerCorrector {
	if capture == nil || screenState == nil {
		return nil
	}
	return &landscapePointerCorrector{capture: capture, screen: screenState, settle: landscapePointerProbeSettle, reenumerate: reenumerate}
}

// setDeviceTypeFunc sets where the target device type comes from; the check
// only runs for iOS, the host that mirrors landscape with a turned pointer.
func (c *landscapePointerCorrector) setDeviceTypeFunc(fn func() string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.deviceTypeFn = fn
	c.mu.Unlock()
}

// setDeviceOrientationFunc sets where the physical phone orientation comes
// from. A turn decision is only reused while the phone stays held the same
// way, since turning the phone can change how iOS maps the pointer.
func (c *landscapePointerCorrector) setDeviceOrientationFunc(fn func() (string, time.Time)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.heldFn = fn
	c.mu.Unlock()
}

// held returns the last reported physical orientation, or "" when the app
// has not reported one.
func (c *landscapePointerCorrector) held() string {
	c.mu.Lock()
	fn := c.heldFn
	c.mu.Unlock()
	if fn == nil {
		return ""
	}
	orientation, _ := fn()
	return orientation
}

func (c *landscapePointerCorrector) targetsIOS() bool {
	c.mu.Lock()
	fn := c.deviceTypeFn
	c.mu.Unlock()
	deviceType := ""
	if fn != nil {
		deviceType = fn()
	}
	normalized, ok := normalizeDeviceType(deviceType)
	return ok && normalized == "iOS"
}

// session wraps inner for one tool call. The orientation is checked at the
// call's first point and applied to all of them.
func (c *landscapePointerCorrector) session(inner mnk.Provider) mnk.Provider {
	if c == nil || inner == nil {
		return inner
	}
	return &landscapePointerProvider{Provider: inner, corrector: c}
}

// currentOrientation reads the phone orientation from the latest screenshot's
// active area.
func (c *landscapePointerCorrector) currentOrientation() screenOrientation {
	_, _, active, age, ok := c.screen.ActiveAreaWithAge()
	if !ok || age >= screenDimensionsStaleAfter || !active.Valid {
		return orientationUnknown
	}
	if active.Width > active.Height {
		return orientationLandscape
	}
	return orientationPortrait
}

// ensureEnumeratedOrientation re-enumerates the gadget when the phone turned
// between portrait and landscape since the last enumeration, so iOS sets the
// pointer bounds for the current orientation. The board cannot know the
// phone's orientation at plug-in: a first portrait sighting is taken as the
// enumeration orientation, and a first landscape sighting re-enumerates once,
// because a phone plugged in while upright clamps the landscape pointer.
func (c *landscapePointerCorrector) ensureEnumeratedOrientation(ctx context.Context) {
	if c.reenumerate == nil || !c.targetsIOS() {
		return
	}
	current := c.currentOrientation()
	if current == orientationUnknown {
		return
	}
	c.mu.Lock()
	previous := c.enumerated
	if previous == current || (previous == orientationUnknown && current == orientationPortrait) {
		c.enumerated = current
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	started := time.Now()
	if err := c.reenumerate(ctx); err != nil {
		logging.Warnf("agent", "landscape_pointer", "pointer re-enumeration after %s to %s turn failed: %v", previous, current, err)
		return
	}
	c.noteReenumerated()
	logging.Infof("agent", "landscape_pointer", "re-enumerated USB pointer after %s to %s turn in %dms", previous, current, time.Since(started).Milliseconds())
}

// noteReenumerated records that the pointer was just detached and reattached,
// by this corrector or by a modified-keyboard profile switch, so the host
// now bounds it for the current orientation.
func (c *landscapePointerCorrector) noteReenumerated() {
	if c == nil {
		return
	}
	current := c.currentOrientation()
	c.mu.Lock()
	c.enumerated = current
	// A new enumeration may map the pointer differently.
	c.last = landscapePointerDecision{}
	c.mu.Unlock()
}

// landscapeRegion returns the phone screen inside the frame when it is known
// and wider than tall.
func (c *landscapePointerCorrector) landscapeRegion() (image.Rectangle, int, int, bool) {
	width, height, active, age, ok := c.screen.ActiveAreaWithAge()
	// Past this age the HID provider stops using the active area as well.
	if !ok || age >= screenDimensionsStaleAfter || !active.Valid || active.Width <= active.Height {
		return image.Rectangle{}, 0, 0, false
	}
	return image.Rect(active.X, active.Y, active.X+active.Width, active.Y+active.Height), width, height, true
}

// recentTurn returns an earlier decision for the same screen area and the
// same physical orientation when the current check cannot decide; without one
// the points are sent as is.
func (c *landscapePointerCorrector) recentTurn(region image.Rectangle) pointerTurn {
	held := c.held()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.region == region && c.last.held == held && !c.last.at.IsZero() && time.Since(c.last.at) < landscapePointerProbeReuseFor {
		return c.last.turn
	}
	return 0
}

// heldLandscapeTurn returns the last decided turn without a new check when the
// companion app reports the phone held in landscape and nothing changed since
// that decision. With the phone held in landscape the interface follows
// gravity, also across app switches, so the turn stays put. A phone held
// upright or flat with a landscape picture means an app forced landscape,
// which another app can force the other way, so those are checked every time.
func (c *landscapePointerCorrector) heldLandscapeTurn(region image.Rectangle) (pointerTurn, bool) {
	held := c.held()
	if held != "landscape_left" && held != "landscape_right" {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.region != region || c.last.held != held || c.last.at.IsZero() || time.Since(c.last.at) >= landscapePointerProbeReuseFor {
		return 0, false
	}
	return c.last.turn, true
}

func (c *landscapePointerCorrector) remember(turn pointerTurn, region image.Rectangle) {
	held := c.held()
	c.mu.Lock()
	c.last = landscapePointerDecision{turn: turn, region: region, held: held, at: time.Now()}
	c.mu.Unlock()
}

func (c *landscapePointerCorrector) setDragTurn(turn pointerTurn) {
	c.mu.Lock()
	c.dragTurn = turn
	c.mu.Unlock()
}

func (c *landscapePointerCorrector) currentDragTurn() pointerTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dragTurn
}

// probe reports how iOS turns the pointer in landscape. It sends (x, y) as
// is after parking the pointer and compares the frame around the four places
// the point can land. It leaves the pointer on (x, y) as sent, wherever that
// lands.
func (c *landscapePointerCorrector) probe(ctx context.Context, inner mnk.Provider, x, y float64) pointerTurn {
	if !c.targetsIOS() {
		return 0
	}
	region, frameWidth, frameHeight, ok := c.landscapeRegion()
	if !ok {
		return 0
	}
	if turn, ok := c.heldLandscapeTurn(region); ok {
		return turn
	}
	short := min(region.Dx(), region.Dy())
	radius := max(4, int(float64(short)*landscapePointerProbeRadiusFraction+0.5))
	candidates, parked, park := landscapeProbeLayout(region, x, y)
	if !pointsApart(candidates[:], candidates[:], 2*radius) || !pointsApart(candidates[:], parked[:], 2*radius) {
		// A point near the centre lands nearly in the same spot under every
		// turn, but the rest of a swipe or sequence does not, so the check
		// runs on a fixed point away from the centre instead.
		x, y = landscapePointerProbeFallback.X, landscapePointerProbeFallback.Y
		candidates, parked, park = landscapeProbeLayout(region, x, y)
	}
	var changed [pointerTurns]float64
	for attempt := 1; attempt <= landscapePointerProbeAttempts; attempt++ {
		var ok, busy bool
		changed, ok = c.measure(ctx, inner, x, y, park, candidates, radius, frameWidth, frameHeight)
		if !ok {
			return c.recentTurn(region)
		}
		best, decided := pickPointerTurn(changed)
		if decided {
			c.remember(best, region)
			logging.Infof("agent", "landscape_pointer", "pointer %s at (%.0f,%.0f) held=%q: changed=%.3f", best, x, y, c.held(), changed)
			return best
		}
		for _, fraction := range changed {
			if fraction >= landscapePointerProbeMinChanged {
				if busy {
					break
				}
				busy = true
			}
		}
		if !busy {
			// Nothing or one weak patch changed: the pointer is hidden or
			// sits on a flat area, and another check sees the same.
			break
		}
	}
	// The pointer was not found. An earlier decision only stands in when the
	// screen kept still, because a changing screen often means the app or its
	// orientation changed under the same active area.
	turn := c.recentTurn(region)
	if screenBusy(changed) {
		// The busy screen rejected the earlier decision as well; a later
		// undecided check must not bring it back.
		turn = 0
		c.mu.Lock()
		c.last = landscapePointerDecision{}
		c.mu.Unlock()
	}
	logging.Infof("agent", "landscape_pointer", "pointer check undecided at (%.0f,%.0f): changed=%.3f; using %s", x, y, changed, turn)
	return turn
}

// measure parks the pointer, moves it onto (x, y) as sent, and returns the
// changed share of each candidate patch.
func (c *landscapePointerCorrector) measure(ctx context.Context, inner mnk.Provider, x, y float64, park mnk.Point, candidates [pointerTurns]image.Point, radius, frameWidth, frameHeight int) ([pointerTurns]float64, bool) {
	var changed [pointerTurns]float64
	if err := inner.Move(ctx, park.X, park.Y); err != nil {
		logging.Warnf("agent", "landscape_pointer", "park move failed: %v", err)
		return changed, false
	}
	if sleepWithContext(ctx, c.settle) != nil {
		return changed, false
	}
	before, err := c.capture(ctx)
	if err != nil {
		logging.Warnf("agent", "landscape_pointer", "baseline capture failed: %v", err)
		return changed, false
	}
	if err := inner.Move(ctx, x, y); err != nil {
		logging.Warnf("agent", "landscape_pointer", "probe move failed: %v", err)
		return changed, false
	}
	if sleepWithContext(ctx, c.settle) != nil {
		return changed, false
	}
	after, err := c.capture(ctx)
	if err != nil {
		logging.Warnf("agent", "landscape_pointer", "probe capture failed: %v", err)
		return changed, false
	}
	if before.width != frameWidth || before.height != frameHeight || after.width != frameWidth || after.height != frameHeight {
		return changed, false
	}
	for turn := range pointerTurn(pointerTurns) {
		fraction, ok := lumaChangedFraction(before, after, candidates[turn], radius)
		if !ok {
			return changed, false
		}
		changed[turn] = fraction
	}
	return changed, true
}

// pickPointerTurn returns the candidate whose patch changed clearly more than all
// others.
func pickPointerTurn(changed [pointerTurns]float64) (pointerTurn, bool) {
	best := pointerTurn(0)
	for turn := range pointerTurn(pointerTurns) {
		if changed[turn] > changed[best] {
			best = turn
		}
	}
	if changed[best] < landscapePointerProbeMinChanged {
		return best, false
	}
	for turn := range pointerTurn(pointerTurns) {
		if turn != best && changed[best] < landscapePointerProbeDominance*changed[turn] {
			return best, false
		}
	}
	return best, true
}

// screenBusy reports whether more than one candidate patch changed, which a
// single pointer cannot cause.
func screenBusy(changed [pointerTurns]float64) bool {
	count := 0
	for _, fraction := range changed {
		if fraction >= landscapePointerProbeMinChanged {
			count++
		}
	}
	return count > 1
}

// landscapePointerProbeFallback stands in for a call whose first point is too
// close to the centre to tell the turns apart.
var landscapePointerProbeFallback = mnk.Point{X: 250, Y: 250}

// landscapeProbeLayout returns where (x, y) and its park point land in the
// frame under each turn, and the park point itself.
func landscapeProbeLayout(region image.Rectangle, x, y float64) (candidates, parked [pointerTurns]image.Point, park mnk.Point) {
	for turn := range pointerTurn(pointerTurns) {
		lx, ly := turn.land(x, y)
		candidates[turn] = landscapeFramePoint(region, lx, ly)
	}
	park = landscapeParkPoint(x, y, landscapePointerProbeParkDistance)
	for turn := range pointerTurn(pointerTurns) {
		px, py := turn.land(park.X, park.Y)
		parked[turn] = landscapeFramePoint(region, px, py)
	}
	return candidates, parked, park
}

// pointsApart reports whether every point of a is at least distance pixels
// from every distinct point of b.
func pointsApart(a, b []image.Point, distance int) bool {
	for i, p := range a {
		for j, q := range b {
			if &a[0] == &b[0] && i == j {
				continue
			}
			if math.Hypot(float64(p.X-q.X), float64(p.Y-q.Y)) < float64(distance) {
				return false
			}
		}
	}
	return true
}

// landscapeFramePoint places a normalized point inside region the way
// tap_sequence locates its targets.
func landscapeFramePoint(region image.Rectangle, x, y float64) image.Point {
	x, y = clampFloat64(x, 0, 1000), clampFloat64(y, 0, 1000)
	return image.Point{
		X: region.Min.X + int(x/1000*float64(region.Dx()-1)+0.5),
		Y: region.Min.Y + int(y/1000*float64(region.Dy()-1)+0.5),
	}
}

// landscapeParkPoint returns the normalized point distance units from the
// centre, an eighth turn from the target around it. Quarter turns move it to
// the other diagonals between the target's four turned positions.
func landscapeParkPoint(x, y, distance float64) mnk.Point {
	vx, vy := x-500, y-500
	length := math.Hypot(vx, vy)
	if length == 0 {
		vx, vy, length = 0, -1, 1
	}
	const cos45 = math.Sqrt2 / 2
	px := (vx - vy) * cos45 / length * distance
	py := (vx + vy) * cos45 / length * distance
	return mnk.Point{X: clampFloat64(500+px, 0, 1000), Y: clampFloat64(500+py, 0, 1000)}
}

func clampFloat64(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}

// lumaChangedFraction returns the share of pixels within radius of center
// whose luminance moved by more than landscapePointerProbeLumaDelta.
func lumaChangedFraction(before, after lumaFrame, center image.Point, radius int) (float64, bool) {
	if !before.valid() || !after.valid() || before.width != after.width || before.height != after.height {
		return 0, false
	}
	changed, count := 0, 0
	for y := max(0, center.Y-radius); y <= min(before.height-1, center.Y+radius); y++ {
		for x := max(0, center.X-radius); x <= min(before.width-1, center.X+radius); x++ {
			if absInt(int(before.y[y*before.stride+x])-int(after.y[y*after.stride+x])) > landscapePointerProbeLumaDelta {
				changed++
			}
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	return float64(changed) / float64(count), true
}

// landscapePointerProvider maps the points of one tool call through the
// orientation found at its first point. Keyboard and wheel input pass through.
type landscapePointerProvider struct {
	mnk.Provider
	corrector *landscapePointerCorrector
	resolved  bool
	turn      pointerTurn
}

func (p *landscapePointerProvider) resolve(ctx context.Context, x, y float64) pointerTurn {
	if !p.resolved {
		p.resolved = true
		p.corrector.ensureEnumeratedOrientation(ctx)
		p.turn = p.corrector.probe(ctx, p.Provider, x, y)
	}
	return p.turn
}

func (p *landscapePointerProvider) Click(ctx context.Context, x, y float64, button string, holdMs int) error {
	x, y = p.resolve(ctx, x, y).send(x, y)
	return p.Provider.Click(ctx, x, y, button, holdMs)
}

func (p *landscapePointerProvider) DoubleClick(ctx context.Context, x, y float64, button string) error {
	x, y = p.resolve(ctx, x, y).send(x, y)
	return p.Provider.DoubleClick(ctx, x, y, button)
}

func (p *landscapePointerProvider) Move(ctx context.Context, x, y float64) error {
	x, y = p.resolve(ctx, x, y).send(x, y)
	return p.Provider.Move(ctx, x, y)
}

func (p *landscapePointerProvider) mapPath(ctx context.Context, path [][2]float64) [][2]float64 {
	if len(path) == 0 {
		return path
	}
	turn := p.resolve(ctx, path[0][0], path[0][1])
	if turn == 0 {
		return path
	}
	mapped := make([][2]float64, len(path))
	for i, point := range path {
		mapped[i][0], mapped[i][1] = turn.send(point[0], point[1])
	}
	return mapped
}

func (p *landscapePointerProvider) Swipe(ctx context.Context, path [][2]float64, button string) error {
	return p.Provider.Swipe(ctx, p.mapPath(ctx, path), button)
}

// SwipeWithOptions keeps the swipe timing contract of providers that honor it.
func (p *landscapePointerProvider) SwipeWithOptions(ctx context.Context, path [][2]float64, button string, options mnk.SwipeOptions) error {
	path = p.mapPath(ctx, path)
	if inner, ok := p.Provider.(interface {
		SwipeWithOptions(context.Context, [][2]float64, string, mnk.SwipeOptions) error
	}); ok {
		return inner.SwipeWithOptions(ctx, path, button, options)
	}
	if inner, ok := p.Provider.(interface {
		SwipeWithDuration(context.Context, [][2]float64, string, int) error
	}); ok && options.DurationMs > 0 {
		return inner.SwipeWithDuration(ctx, path, button, options.DurationMs)
	}
	return p.Provider.Swipe(ctx, path, button)
}

// DragStart records the orientation so the drag_release of a later call
// lands in the same one without moving the held contact to check again.
func (p *landscapePointerProvider) DragStart(ctx context.Context, x, y float64, button string) error {
	turn := p.resolve(ctx, x, y)
	mx, my := turn.send(x, y)
	if err := p.Provider.DragStart(ctx, mx, my, button); err != nil {
		return err
	}
	p.corrector.setDragTurn(turn)
	return nil
}

func (p *landscapePointerProvider) DragRelease(ctx context.Context, x, y float64) error {
	x, y = p.corrector.currentDragTurn().send(x, y)
	return p.Provider.DragRelease(ctx, x, y)
}

func (p *landscapePointerProvider) TouchActions(ctx context.Context, actions []mnk.TouchAction) error {
	atomic, ok := p.Provider.(mnk.TouchActionProvider)
	if !ok || atomic == nil {
		return mnk.ModuleUnavailable("atomic touch actions are not supported by this input provider")
	}
	turn := pointerTurn(0)
	for _, action := range actions {
		if action.Point != nil {
			turn = p.resolve(ctx, action.Point.X, action.Point.Y)
			break
		}
	}
	if turn == 0 {
		return atomic.TouchActions(ctx, actions)
	}
	mapped := make([]mnk.TouchAction, len(actions))
	copy(mapped, actions)
	for i := range mapped {
		if mapped[i].Point != nil {
			x, y := turn.send(mapped[i].Point.X, mapped[i].Point.Y)
			mapped[i].Point = &mnk.Point{X: x, Y: y}
		}
	}
	return atomic.TouchActions(ctx, mapped)
}

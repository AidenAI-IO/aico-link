package mnk

import (
	"aiden-agent/internal/agent/screen"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ADBProvider implements Provider interface using Android Debug Bridge (adb).
// Regular actions use "adb shell input" commands. Atomic touch programs use
// the physical Linux input stream (getevent/sendevent) when permitted, with
// Android's input motionevent primitive as a best-effort fallback.
type ADBProvider struct {
	screen *screen.ScreenState
	client *ADBScreenClient
	runADB adbCommandRunner

	mu                sync.Mutex
	cachedScreenSize  adbInputScreenSize
	screenSizeExpires time.Time
	androidSDK        int
	androidSDKValid   bool

	// Timing constants
	commandTimeout    time.Duration
	gestureTimeoutPad time.Duration
	screenSizeTTL     time.Duration
	textRestoreWait   int // milliseconds
	maxDurationMs     int

	// Raw touchscreen event discovery is cached because getevent -lp is
	// relatively expensive and the input device topology is stable for the
	// lifetime of an Android boot. touchMu also serializes event programs so
	// two callers cannot interleave sendevent streams on the same device.
	touchMu            sync.Mutex
	cachedTouchDevice  adbTouchDevice
	touchDeviceExpires time.Time
	rawTouchBlockedTil time.Time
	nextTrackingID     int
	dragActive         bool
	dragRaw            bool
	dragDevice         adbTouchDevice
}

type adbCommandRunner func(context.Context, string, ...string) ([]byte, []byte, error)

type adbInputScreenSize struct {
	width  int
	height int
}

const (
	adbDefaultCommandTimeout      = 8 * time.Second
	adbDefaultGestureTimeoutPad   = 3 * time.Second
	adbDefaultScreenSizeTTL       = 30 * time.Second
	adbDefaultTextRestoreWait     = 80 // milliseconds
	adbMaxActionDurationMs        = 10000
	adbKeyboardIME                = "com.android.adbkeyboard/.AdbIME"
	adbScreenDimensionsStaleAfter = 30 * time.Second
	adbTouchDeviceTTL             = 30 * time.Second
	adbTouchMoveSteps             = 24
	adbEventSyn                   = 0
	adbEventKey                   = 1
	adbEventAbs                   = 3
	adbSynReport                  = 0
	adbSynMtReport                = 2
	adbBtnTouch                   = 330
	adbBtnToolFinger              = 325
	adbAbsX                       = 0
	adbAbsY                       = 1
	adbAbsMtSlot                  = 47
	adbAbsMtTouchMajor            = 48
	adbAbsMtPositionX             = 53
	adbAbsMtPositionY             = 54
	adbAbsMtToolType              = 55
	adbAbsMtTrackingID            = 57
	adbAbsMtPressure              = 58
)

var (
	adbWMSizePattern          = regexp.MustCompile(`(?m)(?:Physical|Override) size:\s*([0-9]+)x([0-9]+)`)
	adbInputDeviceNamePattern = regexp.MustCompile(`(?m)^\s*name:\s*"([^"]*)"`)
	adbInputAbsRangePattern   = regexp.MustCompile(`(?m)^\s*(?:ABS \(\d+\):\s*)?([A-Z0-9_]+)\s*:\s*value\s*-?\d+,\s*min\s*(-?\d+),\s*max\s*(-?\d+)`)
	adbInputEventPathPattern  = regexp.MustCompile(`^/dev/input/event[0-9]+$`)
)

type adbTouchDevice struct {
	path          string
	name          string
	xMin, xMax    int
	yMin, yMax    int
	mt            bool
	protocolB     bool
	hasAbsXY      bool
	hasTrackingID bool
	hasTouchMajor bool
	hasPressure   bool
	hasToolType   bool
	hasBtnTouch   bool
	hasToolFinger bool
}

// NewADBProvider creates a new ADB-based MNK provider.
func NewADBProvider(screen *screen.ScreenState, client *ADBScreenClient, runADB adbCommandRunner) *ADBProvider {
	if client == nil {
		client = NewADBScreenClient()
	}
	if runADB == nil {
		runADB = runADBCommand
	}
	client.setCommandRunner(runADB)

	return &ADBProvider{
		screen:            screen,
		client:            client,
		runADB:            runADB,
		commandTimeout:    adbDefaultCommandTimeout,
		gestureTimeoutPad: adbDefaultGestureTimeoutPad,
		screenSizeTTL:     adbDefaultScreenSizeTTL,
		textRestoreWait:   adbDefaultTextRestoreWait,
		maxDurationMs:     adbMaxActionDurationMs,
	}
}

// Click performs a tap at the specified position.
// holdMs is used to determine if this is a tap (default) or long press (500+).
func (p *ADBProvider) Click(ctx context.Context, x, y float64, button string, holdMs int) error {
	if err := p.rejectActiveDrag("click"); err != nil {
		return err
	}

	// Validate button (ADB only supports left/touch semantics)
	if button != "" && button != ButtonLeft && button != "left" {
		return fmt.Errorf("adb only supports left-button touch semantics, got %q", button)
	}

	// Validate and resolve coordinates
	pixelX, pixelY, err := p.normalizedToPixel(ctx, x, y)
	if err != nil {
		return err
	}

	// Long press vs tap
	if holdMs >= 500 {
		// Long press: swipe from point to itself with duration
		durationMs := p.clampDuration(holdMs, 500, 1)
		return p.runShellWithTimeout(ctx, p.timeoutForDuration(durationMs),
			"input", "swipe",
			strconv.Itoa(pixelX), strconv.Itoa(pixelY),
			strconv.Itoa(pixelX), strconv.Itoa(pixelY),
			strconv.Itoa(durationMs),
		)
	}

	// Regular tap
	return p.runShell(ctx, "input", "tap", strconv.Itoa(pixelX), strconv.Itoa(pixelY))
}

// DoubleClick performs two taps in rapid succession.
func (p *ADBProvider) DoubleClick(ctx context.Context, x, y float64, button string) error {
	// First tap
	if err := p.Click(ctx, x, y, button, 0); err != nil {
		return err
	}

	// Pause, while allowing a canceled caller to stop the second tap.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(100 * time.Millisecond):
	}

	// Second tap
	return p.Click(ctx, x, y, button, 0)
}

// Back invokes Android's system back action without requiring screen geometry.
func (p *ADBProvider) Back(ctx context.Context) error {
	return p.Keypress(ctx, []string{"keycode_back"})
}

// Home invokes Android's system home action without requiring screen geometry.
func (p *ADBProvider) Home(ctx context.Context) error {
	return p.Keypress(ctx, []string{"keycode_home"})
}

// Swipe performs a short gesture that avoids Android long-press recognition.
func (p *ADBProvider) Swipe(ctx context.Context, path [][2]float64, button string) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{})
}

// SwipeWithDuration performs a swipe using the caller-selected motion time.
func (p *ADBProvider) SwipeWithDuration(ctx context.Context, path [][2]float64, button string, durationMs int) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{DurationMs: durationMs})
}

// SwipeWithOptions keeps one contact down across a smooth path, using raw
// touch events when available and input motionevent as a fallback.
func (p *ADBProvider) SwipeWithOptions(ctx context.Context, path [][2]float64, button string, options SwipeOptions) error {
	if err := p.rejectActiveDrag("swipe"); err != nil {
		return err
	}
	options.DurationMs = p.clampDuration(options.DurationMs, defaultSwipeGestureDurationMs, 1)
	actions, err := adbSwipeActions(path, button, options)
	if err != nil {
		return err
	}
	return p.TouchActions(ctx, actions)
}

func adbSwipeActions(path [][2]float64, button string, options SwipeOptions) ([]TouchAction, error) {
	if len(path) < 2 || len(path) > 125 {
		return nil, InvalidArguments("swipe path must contain 2 to 125 points")
	}
	if button != "" && button != ButtonLeft {
		return nil, InvalidArgumentsf("adb only supports left-button touch semantics, got %q", button)
	}
	if options.HoldBeforeMs < 0 || options.HoldBeforeMs > MaxSwipeHoldMs || options.HoldAfterMs < 0 || options.HoldAfterMs > MaxSwipeHoldMs || options.Steps < 0 || options.Steps > MaxSwipeSteps {
		return nil, InvalidArguments("swipe holds or steps are outside supported limits")
	}
	lengths := make([]float64, len(path))
	total := 0.0
	lastMoving := 0
	for i, point := range path {
		if err := validateADBTouchPoint(Point{X: point[0], Y: point[1]}); err != nil {
			return nil, InvalidArgumentsf("invalid point %d: %v", i, err)
		}
		if i > 0 {
			lengths[i] = math.Hypot(point[0]-path[i-1][0], point[1]-path[i-1][1])
			total += lengths[i]
			if lengths[i] > 0 {
				lastMoving = i
			}
		}
	}
	if total == 0 {
		return nil, InvalidArguments("swipe requires distinct start and end points")
	}
	// Profile the whole path before distributing its duration. The generic
	// atomic transformer must not floor the final segment again, or treat
	// intermediate waypoints as independent acceleration/braking cycles.
	controlledRelease := controlledReleaseAllowed(Point{path[0][0], path[0][1]}, options.HoldAfterMs)
	endpoint := Point{path[lastMoving][0], path[lastMoving][1]}
	mainEnd := endpoint
	if controlledRelease {
		options.DurationMs = max(options.DurationMs, defaultSwipeSmoothMotionMinMs)
		previous := Point{path[lastMoving-1][0], path[lastMoving-1][1]}
		mainEnd = releaseTailStart(previous, endpoint)
		mainLength := math.Hypot(mainEnd.X-previous.X, mainEnd.Y-previous.Y)
		total += mainLength - lengths[lastMoving]
		lengths[lastMoving] = mainLength
	}
	actions := []TouchAction{{Type: "touch_down", Point: &Point{X: path[0][0], Y: path[0][1]}}}
	if options.HoldBeforeMs > 0 {
		actions = append(actions, TouchAction{Type: "wait", DurationMs: options.HoldBeforeMs})
	}
	steps := options.Steps
	if steps == 0 {
		steps = adbTouchMoveSteps
	}
	completed, elapsed := 0.0, 0
	for i := 1; i <= lastMoving; i++ {
		if lengths[i] == 0 {
			continue
		}
		completed += lengths[i]
		due := int(math.Round(float64(options.DurationMs) * completed / total))
		point := Point{X: path[i][0], Y: path[i][1]}
		if i == lastMoving {
			point = mainEnd
			due = options.DurationMs
		}
		actions = append(actions, TouchAction{
			Type: "move_to", Point: &point,
			DurationMs: due - elapsed,
			steps:      max(1, int(math.Round(float64(steps)*lengths[i]/total))),
			linear:     !controlledRelease || i != lastMoving,
		})
		elapsed = due
	}
	if controlledRelease {
		actions = append(actions, TouchAction{Type: "move_to", Point: &endpoint, DurationMs: defaultSwipeReleaseTailMs, releaseTail: true})
	}
	if options.HoldAfterMs > 0 {
		actions = append(actions, TouchAction{Type: "wait", DurationMs: options.HoldAfterMs})
	}
	actions = append(actions, TouchAction{Type: "touch_up"})
	return actions, nil
}

// DragStart starts a persistent Android touch contact. Raw sendevent is used
// whenever permitted so the contact survives the ADB command boundary.
func (p *ADBProvider) DragStart(ctx context.Context, x, y float64, button string) error {
	if button != "" && button != ButtonLeft && button != "left" {
		return InvalidArgumentsf("adb only supports left-button touch semantics, got %q", button)
	}
	start := Point{X: x, Y: y}
	if err := validateADBTouchPoint(start); err != nil {
		return InvalidArguments(err.Error())
	}
	activation := dragActivationPoint(start)

	p.touchMu.Lock()
	defer p.touchMu.Unlock()
	if p.dragActive {
		return InvalidArguments("drag_start is already active; call drag_release before starting another drag")
	}

	if !time.Now().Before(p.rawTouchBlockedTil) {
		device, err := p.adbTouchDevice(ctx)
		if err == nil {
			p.nextTrackingID++
			if p.nextTrackingID <= 0 || p.nextTrackingID > 65535 {
				p.nextTrackingID = 1
			}
			script := buildADBDragStartScript(device, start, activation, p.nextTrackingID)
			_, runErr := p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(dragStartHoldMs+dragStartMoveDurationMs), script)
			if runErr == nil {
				p.dragActive = true
				p.dragRaw = true
				p.dragDevice = device
				return nil
			}
			cleanupErr := p.bestEffortRawTouchUp(ctx, device)
			lower := strings.ToLower(runErr.Error())
			if !strings.Contains(lower, "permission denied") && !strings.Contains(lower, "operation not permitted") {
				if cleanupErr != nil {
					return fmt.Errorf("adb drag_start raw touch failed: %w; cleanup touch_up failed: %v", runErr, cleanupErr)
				}
				return fmt.Errorf("adb drag_start raw touch failed: %w", runErr)
			}
			p.cachedTouchDevice = adbTouchDevice{}
			p.touchDeviceExpires = time.Time{}
			p.rawTouchBlockedTil = time.Now().Add(adbTouchDeviceTTL)
		}
	}

	if err := p.runADBInputDragStart(ctx, start, activation); err != nil {
		return ModuleUnavailablef("adb drag_start is unavailable through sendevent and input motionevent: %v", err)
	}
	p.dragActive = true
	p.dragRaw = false
	p.dragDevice = adbTouchDevice{}
	return nil
}

// DragRelease completes the persistent contact with a direct move, a 200ms
// dwell, and an UP event. Failed releases remain active so callers can retry.
func (p *ADBProvider) DragRelease(ctx context.Context, x, y float64) error {
	target := Point{X: x, Y: y}
	if err := validateADBTouchPoint(target); err != nil {
		return InvalidArguments(err.Error())
	}

	p.touchMu.Lock()
	defer p.touchMu.Unlock()
	if !p.dragActive {
		return InvalidArguments("drag_release requires an active drag_start")
	}

	var err error
	if p.dragRaw {
		script := buildADBDragReleaseScript(p.dragDevice, target)
		_, err = p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(dragReleaseHoldMs), script)
	} else {
		err = p.runADBInputDragRelease(ctx, target)
	}
	if err != nil {
		return fmt.Errorf("adb drag_release failed: %w", err)
	}
	p.dragActive = false
	p.dragRaw = false
	p.dragDevice = adbTouchDevice{}
	return nil
}

func (p *ADBProvider) rejectActiveDrag(operation string) error {
	p.touchMu.Lock()
	defer p.touchMu.Unlock()
	if p.dragActive {
		return InvalidArgumentsf("drag_start is active; call drag_release before %s", operation)
	}
	return nil
}

// TouchActions executes an atomic touch program through Linux input events.
// Android's `input swipe` command cannot keep a contact alive across separate
// operations, but `sendevent` can write the underlying ABS/KEY/SYN stream. We
// discover a physical touchscreen with `getevent -lp`, then send the complete
// program as one shell script so no ADB round trip occurs between events.
func (p *ADBProvider) TouchActions(ctx context.Context, actions []TouchAction) error {
	if err := validateADBTouchActions(actions); err != nil {
		return err
	}
	actions = withTouchReleaseTails(actions)
	if err := validateTouchActionLimits(actions); err != nil {
		return InvalidArgumentsf("profiled touch program exceeds limits: %v", err)
	}

	p.touchMu.Lock()
	defer p.touchMu.Unlock()
	if p.dragActive {
		return InvalidArguments("drag_start is active; call drag_release before atomic touch actions")
	}
	if time.Now().Before(p.rawTouchBlockedTil) {
		return p.runADBInputMotionActions(ctx, actions)
	}

	device, err := p.adbTouchDevice(ctx)
	if err != nil {
		if fallbackErr := p.runADBInputMotionActions(ctx, actions); fallbackErr == nil {
			p.rawTouchBlockedTil = time.Now().Add(adbTouchDeviceTTL)
			return nil
		} else {
			return ModuleUnavailablef("adb raw touch discovery failed (%v); input motionevent fallback failed: %v", err, fallbackErr)
		}
	}

	p.nextTrackingID++
	if p.nextTrackingID <= 0 || p.nextTrackingID > 65535 {
		p.nextTrackingID = 1
	}
	script, totalDurationMs, err := buildADBTouchScript(device, actions, p.nextTrackingID)
	if err != nil {
		return err
	}

	_, err = p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(totalDurationMs), script)
	if err != nil {
		// Permission failures are common on production builds where the shell
		// domain may inspect input devices but is not allowed to inject them.
		if strings.Contains(strings.ToLower(err.Error()), "permission denied") ||
			strings.Contains(strings.ToLower(err.Error()), "operation not permitted") {
			// Newer Android releases expose the same DOWN/MOVE/UP primitive
			// through `input motionevent`. It does not require write access to
			// /dev/input, so use it as a best-effort fallback when raw injection
			// is blocked by SELinux or device permissions.
			fallbackErr := p.runADBInputMotionActions(ctx, actions)
			p.cachedTouchDevice = adbTouchDevice{}
			p.touchDeviceExpires = time.Time{}
			p.rawTouchBlockedTil = time.Now().Add(adbTouchDeviceTTL)
			if fallbackErr != nil {
				return ModuleUnavailablef("adb atomic touch actions are not permitted via sendevent (%v); input motionevent fallback failed: %v", err, fallbackErr)
			}
			return nil
		}
		if cleanupErr := p.bestEffortRawTouchUp(ctx, device); cleanupErr != nil {
			err = fmt.Errorf("%w; touch_up cleanup failed: %v", err, cleanupErr)
		}
		p.cachedTouchDevice = adbTouchDevice{}
		p.touchDeviceExpires = time.Time{}
		return fmt.Errorf("adb atomic touch actions failed: %w", err)
	}
	return nil
}

func (p *ADBProvider) runADBInputMotionActions(ctx context.Context, actions []TouchAction) error {
	size, err := p.screenSize(ctx)
	if err != nil {
		return err
	}
	script, totalDurationMs, err := buildADBInputMotionScript(actions, size)
	if err != nil {
		return err
	}
	_, err = p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(totalDurationMs), script)
	return err
}

func validateADBTouchActions(actions []TouchAction) error {
	if err := validateTouchActionLimits(actions); err != nil {
		return err
	}
	active := false
	for i, action := range actions {
		actionType := strings.ToLower(strings.TrimSpace(action.Type))
		if actionType == "" {
			return InvalidArgumentsf("touch action %d action is required", i)
		}
		if action.Button != "" && action.Button != ButtonLeft && action.Button != "left" {
			return InvalidArgumentsf("adb only supports left-button touch semantics, got %q", action.Button)
		}
		if action.Point != nil {
			if err := validateADBTouchPoint(*action.Point); err != nil {
				return InvalidArgumentsf("touch action %d: %v", i, err)
			}
		}

		switch actionType {
		case "touch_down":
			if action.Point == nil {
				return InvalidArgumentsf("touch action %d touch_down requires point", i)
			}
			if active {
				return InvalidArgumentsf("touch action %d touch_down while a contact is already active", i)
			}
			active = true
		case "move_to":
			if action.Point == nil {
				return InvalidArgumentsf("touch action %d move_to requires point", i)
			}
		case "wait":
		case "touch_up":
			if !active {
				return InvalidArgumentsf("touch action %d touch_up without an active contact", i)
			}
			active = false
		default:
			return InvalidArgumentsf("touch action %d has unsupported action %q; use touch_down, move_to, wait, or touch_up", i, action.Type)
		}
	}
	if active {
		return InvalidArguments("touch action sequence must end with touch_up")
	}
	return nil
}

func validateADBTouchPoint(point Point) error {
	if math.IsNaN(point.X) || math.IsInf(point.X, 0) || math.IsNaN(point.Y) || math.IsInf(point.Y, 0) {
		return fmt.Errorf("coordinates must be finite")
	}
	if point.X < 0 || point.X > 1000 || point.Y < 0 || point.Y > 1000 {
		return fmt.Errorf("coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", point.X, point.Y)
	}
	return nil
}

func (p *ADBProvider) adbTouchDevice(ctx context.Context) (adbTouchDevice, error) {
	now := time.Now()
	if p.cachedTouchDevice.path != "" && now.Before(p.touchDeviceExpires) {
		return p.cachedTouchDevice, nil
	}

	out, err := p.runShellOutput(ctx, "getevent", "-lp")
	if err != nil {
		return adbTouchDevice{}, ModuleUnavailablef("adb cannot inspect touchscreen devices with getevent: %v", err)
	}
	devices := parseADBTouchDevices(out)
	if len(devices) == 0 {
		return adbTouchDevice{}, ModuleUnavailable("adb getevent found no usable touchscreen input device")
	}
	device := devices[0]
	p.cachedTouchDevice = device
	p.touchDeviceExpires = now.Add(adbTouchDeviceTTL)
	return device, nil
}

func parseADBTouchDevices(output string) []adbTouchDevice {
	type scoredDevice struct {
		device adbTouchDevice
		score  int
	}
	var candidates []scoredDevice
	for _, block := range adbInputDeviceBlocks(output) {
		if !adbInputEventPathPattern.MatchString(block.path) {
			continue
		}
		body := block.body
		name := ""
		if nameMatch := adbInputDeviceNamePattern.FindStringSubmatch(body); len(nameMatch) == 2 {
			name = nameMatch[1]
		}
		ranges := make(map[string][2]int)
		for _, rangeMatch := range adbInputAbsRangePattern.FindAllStringSubmatch(body, -1) {
			if len(rangeMatch) != 4 {
				continue
			}
			minValue, minErr := strconv.Atoi(rangeMatch[2])
			maxValue, maxErr := strconv.Atoi(rangeMatch[3])
			if minErr == nil && maxErr == nil && maxValue > minValue {
				ranges[rangeMatch[1]] = [2]int{minValue, maxValue}
			}
		}

		xRange, mt := ranges["ABS_MT_POSITION_X"]
		yRange, mtY := ranges["ABS_MT_POSITION_Y"]
		if !mt || !mtY {
			xRange, mt = ranges["ABS_X"]
			yRange, mtY = ranges["ABS_Y"]
		}
		if !mt || !mtY {
			continue
		}
		hasMT := ranges["ABS_MT_POSITION_X"] != [2]int{} && ranges["ABS_MT_POSITION_Y"] != [2]int{}
		hasTrackingID := ranges["ABS_MT_TRACKING_ID"] != [2]int{}
		hasTouchMajor := ranges["ABS_MT_TOUCH_MAJOR"] != [2]int{}
		hasPressure := ranges["ABS_MT_PRESSURE"] != [2]int{}
		hasToolType := ranges["ABS_MT_TOOL_TYPE"] != [2]int{}
		protocolB := hasMT && hasTrackingID && strings.Contains(body, "ABS_MT_SLOT")
		hasBtnTouch := strings.Contains(body, "BTN_TOUCH")
		hasToolFinger := strings.Contains(body, "BTN_TOOL_FINGER")
		if !hasBtnTouch && !hasMT {
			continue
		}

		_, hasAbsX := ranges["ABS_X"]
		_, hasAbsY := ranges["ABS_Y"]
		device := adbTouchDevice{
			path:          block.path,
			name:          name,
			xMin:          xRange[0],
			xMax:          xRange[1],
			yMin:          yRange[0],
			yMax:          yRange[1],
			mt:            hasMT,
			protocolB:     protocolB,
			hasAbsXY:      hasAbsX && hasAbsY,
			hasTrackingID: hasTrackingID,
			hasTouchMajor: hasTouchMajor,
			hasPressure:   hasPressure,
			hasToolType:   hasToolType,
			hasBtnTouch:   hasBtnTouch,
			hasToolFinger: hasToolFinger,
		}

		lowerName := strings.ToLower(name)
		score := 0
		if strings.Contains(body, "INPUT_PROP_DIRECT") {
			score += 8
		}
		if hasBtnTouch {
			score += 5
		}
		if hasMT {
			score += 5
		}
		if protocolB {
			score += 2
		}
		for _, hint := range []string{"touch", "goodix", "synaptics", "focal", "fts", "elan", "ts"} {
			if strings.Contains(lowerName, hint) {
				score += 2
				break
			}
		}
		for _, virtualHint := range []string{"aiden", "hid", "uinput", "virtual"} {
			if strings.Contains(lowerName, virtualHint) {
				score -= 12
			}
		}
		candidates = append(candidates, scoredDevice{device: device, score: score})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	devices := make([]adbTouchDevice, 0, len(candidates))
	for _, candidate := range candidates {
		devices = append(devices, candidate.device)
	}
	return devices
}

type adbInputDeviceBlock struct {
	path string
	body string
}

func adbInputDeviceBlocks(output string) []adbInputDeviceBlock {
	lines := strings.Split(output, "\n")
	blocks := make([]adbInputDeviceBlock, 0)
	current := -1
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "add device ") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 4 && strings.HasSuffix(fields[2], ":") {
				blocks = append(blocks, adbInputDeviceBlock{path: fields[3]})
				current = len(blocks) - 1
				continue
			}
		}
		if current >= 0 {
			blocks[current].body += line + "\n"
		}
	}
	return blocks
}

func buildADBTouchScript(device adbTouchDevice, actions []TouchAction, trackingID int) (string, int, error) {
	var script strings.Builder
	script.WriteString("set -e\n")
	var cleanup strings.Builder
	appendADBTouchUp(&cleanup, device)
	fmt.Fprintf(&script, "trap %s EXIT\n", shellSingleQuote(cleanup.String()))
	active := false
	currentX, currentY := device.xMin, device.yMin
	totalDurationMs := 0
	for index, action := range actions {
		actionType := strings.ToLower(strings.TrimSpace(action.Type))
		switch actionType {
		case "wait":
			appendADBSleep(&script, action.DurationMs)
			totalDurationMs += action.DurationMs
		case "move_to":
			x, y := adbTouchPointToDevice(device, *action.Point)
			duration := action.DurationMs
			if duration > 0 {
				appendADBMotion(&script, currentX, currentY, x, y, duration, action.steps, active && !action.releaseTail && !action.linear, func(stepX, stepY int) {
					appendADBTouchPosition(&script, device, stepX, stepY, active)
				})
				totalDurationMs += duration
			} else {
				appendADBTouchPosition(&script, device, x, y, active)
			}
			currentX, currentY = x, y
		case "touch_down":
			x, y := adbTouchPointToDevice(device, *action.Point)
			appendADBTouchDown(&script, device, x, y, trackingID)
			currentX, currentY = x, y
			active = true
		case "touch_up":
			if action.Point != nil {
				x, y := adbTouchPointToDevice(device, *action.Point)
				if action.DurationMs > 0 {
					appendADBTouchPosition(&script, device, x, y, true)
					appendADBSleep(&script, action.DurationMs)
					totalDurationMs += action.DurationMs
				} else {
					appendADBTouchPosition(&script, device, x, y, true)
				}
				currentX, currentY = x, y
			}
			appendADBTouchUp(&script, device)
			active = false
		default:
			return "", 0, InvalidArgumentsf("touch action %d has unsupported action %q", index, action.Type)
		}
	}
	if active {
		return "", 0, InvalidArguments("touch action sequence must end with touch_up")
	}
	script.WriteString("trap - EXIT\n")
	return script.String(), totalDurationMs, nil
}

func adbTouchPointToDevice(device adbTouchDevice, point Point) (int, int) {
	x := int(math.Round(float64(device.xMin) + point.X/1000.0*float64(device.xMax-device.xMin)))
	y := int(math.Round(float64(device.yMin) + point.Y/1000.0*float64(device.yMax-device.yMin)))
	return x, y
}

func appendADBEvent(script *strings.Builder, devicePath string, eventType, code, value int) {
	fmt.Fprintf(script, "sendevent %s %d %d %d\n", devicePath, eventType, code, value)
}

func appendADBTouchPosition(script *strings.Builder, device adbTouchDevice, x, y int, active bool) {
	if device.mt && active {
		if device.protocolB {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtSlot, 0)
		}
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtPositionX, x)
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtPositionY, y)
		if !device.protocolB {
			appendADBEvent(script, device.path, adbEventSyn, adbSynMtReport, 0)
		}
	}
	if device.hasAbsXY {
		appendADBEvent(script, device.path, adbEventAbs, adbAbsX, x)
		appendADBEvent(script, device.path, adbEventAbs, adbAbsY, y)
	}
	appendADBEvent(script, device.path, adbEventSyn, adbSynReport, 0)
}

func appendADBTouchDown(script *strings.Builder, device adbTouchDevice, x, y, trackingID int) {
	if device.protocolB {
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtSlot, 0)
	}
	if device.mt {
		if device.hasTrackingID {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtTrackingID, trackingID)
		}
		if device.hasToolType {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtToolType, 0)
		}
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtPositionX, x)
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtPositionY, y)
		if device.hasTouchMajor {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtTouchMajor, 1)
		}
		if device.hasPressure {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtPressure, 1)
		}
	}
	if device.hasAbsXY {
		appendADBEvent(script, device.path, adbEventAbs, adbAbsX, x)
		appendADBEvent(script, device.path, adbEventAbs, adbAbsY, y)
	}
	if device.hasBtnTouch {
		appendADBEvent(script, device.path, adbEventKey, adbBtnTouch, 1)
	}
	if device.hasToolFinger {
		appendADBEvent(script, device.path, adbEventKey, adbBtnToolFinger, 1)
	}
	if device.mt && !device.protocolB {
		appendADBEvent(script, device.path, adbEventSyn, adbSynMtReport, 0)
	}
	appendADBEvent(script, device.path, adbEventSyn, adbSynReport, 0)
}

func appendADBTouchUp(script *strings.Builder, device adbTouchDevice) {
	if device.protocolB {
		appendADBEvent(script, device.path, adbEventAbs, adbAbsMtSlot, 0)
	}
	if device.mt {
		if device.hasTrackingID {
			appendADBEvent(script, device.path, adbEventAbs, adbAbsMtTrackingID, -1)
		}
		if !device.protocolB {
			appendADBEvent(script, device.path, adbEventSyn, adbSynMtReport, 0)
		}
	}
	if device.hasBtnTouch {
		appendADBEvent(script, device.path, adbEventKey, adbBtnTouch, 0)
	}
	if device.hasToolFinger {
		appendADBEvent(script, device.path, adbEventKey, adbBtnToolFinger, 0)
	}
	appendADBEvent(script, device.path, adbEventSyn, adbSynReport, 0)
}

func buildADBDragStartScript(device adbTouchDevice, start, activation Point, trackingID int) string {
	var script strings.Builder
	script.WriteString("set -e\n")
	startX, startY := adbTouchPointToDevice(device, start)
	activationX, activationY := adbTouchPointToDevice(device, activation)
	appendADBTouchDown(&script, device, startX, startY, trackingID)
	appendADBSleep(&script, dragStartHoldMs)
	for step := 1; step <= adbTouchMoveSteps; step++ {
		previousElapsed := dragStartMoveDurationMs * (step - 1) / adbTouchMoveSteps
		elapsed := dragStartMoveDurationMs * step / adbTouchMoveSteps
		appendADBSleep(&script, elapsed-previousElapsed)
		progress := float64(step) / float64(adbTouchMoveSteps)
		x := int(math.Round(float64(startX) + float64(activationX-startX)*progress))
		y := int(math.Round(float64(startY) + float64(activationY-startY)*progress))
		appendADBTouchPosition(&script, device, x, y, true)
	}
	return script.String()
}

func buildADBDragReleaseScript(device adbTouchDevice, target Point) string {
	var script strings.Builder
	script.WriteString("set -e\n")
	x, y := adbTouchPointToDevice(device, target)
	appendADBTouchPosition(&script, device, x, y, true)
	appendADBSleep(&script, dragReleaseHoldMs)
	appendADBTouchUp(&script, device)
	return script.String()
}

func (p *ADBProvider) bestEffortRawTouchUp(ctx context.Context, device adbTouchDevice) error {
	cleanupCtx := context.WithoutCancel(ctx)
	cleanupCtx, cancel := context.WithTimeout(cleanupCtx, p.commandTimeout)
	defer cancel()

	var script strings.Builder
	appendADBTouchUp(&script, device)
	_, err := p.runShellScriptWithTimeout(cleanupCtx, p.commandTimeout, script.String())
	return err
}

func appendADBSleep(script *strings.Builder, durationMs int) {
	if durationMs <= 0 {
		return
	}
	fmt.Fprintf(script, "sleep %d.%03d\n", durationMs/1000, durationMs%1000)
}

// appendADBMotion shares trajectory generation between raw and framework
// injection. Cumulative intervals preserve the requested duration even when it
// is not divisible by steps. Android process/injection overhead is additional.
func appendADBMotion(script *strings.Builder, fromX, fromY, toX, toY, duration, steps int, smooth bool, emit func(int, int)) {
	if steps <= 0 {
		steps = adbTouchMoveSteps
	}
	distance := math.Hypot(float64(toX-fromX), float64(toY-fromY))
	steps = min(steps, max(1, int(math.Round(distance))))
	for step := 1; step <= steps; step++ {
		appendADBSleep(script, duration*step/steps-duration*(step-1)/steps)
		progress := float64(step) / float64(steps)
		if smooth {
			progress = quinticSmoothStep(progress)
		}
		x := int(math.Round(float64(fromX) + float64(toX-fromX)*progress))
		y := int(math.Round(float64(fromY) + float64(toY-fromY)*progress))
		emit(x, y)
	}
}

func buildADBInputMotionScript(actions []TouchAction, size adbInputScreenSize) (string, int, error) {
	if size.width <= 1 || size.height <= 1 {
		return "", 0, ModuleUnavailable("adb input motionevent fallback requires known screen dimensions")
	}

	var script strings.Builder
	script.WriteString("set -e\n")
	script.WriteString("aiden_touch_active=0\naiden_touch_x=0\naiden_touch_y=0\n")
	script.WriteString("trap 'if [ \"$aiden_touch_active\" = 1 ]; then input touchscreen motionevent UP \"$aiden_touch_x\" \"$aiden_touch_y\"; fi' EXIT\n")
	emit := func(kind string, x, y int) {
		fmt.Fprintf(&script, "aiden_touch_x=%d\naiden_touch_y=%d\n", x, y)
		if kind == "DOWN" {
			script.WriteString("aiden_touch_active=1\n")
		}
		appendADBInputMotionEvent(&script, kind, x, y)
		if kind == "UP" {
			script.WriteString("aiden_touch_active=0\n")
		}
	}
	currentX, currentY := 0, 0
	active := false
	totalDurationMs := 0
	for index, action := range actions {
		actionType := strings.ToLower(strings.TrimSpace(action.Type))
		switch actionType {
		case "wait":
			appendADBSleep(&script, action.DurationMs)
			totalDurationMs += action.DurationMs
		case "touch_down":
			currentX, currentY = adbInputPointToPixel(*action.Point, size)
			emit("DOWN", currentX, currentY)
			active = true
		case "move_to":
			x, y := adbInputPointToPixel(*action.Point, size)
			duration := action.DurationMs
			if duration > 0 && active {
				appendADBMotion(&script, currentX, currentY, x, y, duration, action.steps, !action.releaseTail && !action.linear, func(stepX, stepY int) {
					emit("MOVE", stepX, stepY)
				})
				totalDurationMs += duration
			} else {
				emit("MOVE", x, y)
			}
			currentX, currentY = x, y
		case "touch_up":
			if action.Point != nil {
				currentX, currentY = adbInputPointToPixel(*action.Point, size)
			}
			emit("UP", currentX, currentY)
			active = false
		default:
			return "", 0, InvalidArgumentsf("touch action %d has unsupported action %q", index, action.Type)
		}
	}
	if active {
		return "", 0, InvalidArguments("touch action sequence must end with touch_up")
	}
	script.WriteString("trap - EXIT\n")
	return script.String(), totalDurationMs, nil
}

func (p *ADBProvider) runADBInputDragStart(ctx context.Context, start, activation Point) error {
	size, err := p.screenSize(ctx)
	if err != nil {
		return err
	}
	startX, startY := adbInputPointToPixel(start, size)
	activationX, activationY := adbInputPointToPixel(activation, size)
	var script strings.Builder
	script.WriteString("set -e\n")
	appendADBInputMotionEvent(&script, "DOWN", startX, startY)
	appendADBSleep(&script, dragStartHoldMs)
	for step := 1; step <= adbTouchMoveSteps; step++ {
		previousElapsed := dragStartMoveDurationMs * (step - 1) / adbTouchMoveSteps
		elapsed := dragStartMoveDurationMs * step / adbTouchMoveSteps
		appendADBSleep(&script, elapsed-previousElapsed)
		progress := float64(step) / float64(adbTouchMoveSteps)
		x := int(math.Round(float64(startX) + float64(activationX-startX)*progress))
		y := int(math.Round(float64(startY) + float64(activationY-startY)*progress))
		appendADBInputMotionEvent(&script, "MOVE", x, y)
	}
	_, err = p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(dragStartHoldMs+dragStartMoveDurationMs), script.String())
	return err
}

func (p *ADBProvider) runADBInputDragRelease(ctx context.Context, target Point) error {
	size, err := p.screenSize(ctx)
	if err != nil {
		return err
	}
	x, y := adbInputPointToPixel(target, size)
	var script strings.Builder
	script.WriteString("set -e\n")
	appendADBInputMotionEvent(&script, "MOVE", x, y)
	appendADBSleep(&script, dragReleaseHoldMs)
	appendADBInputMotionEvent(&script, "UP", x, y)
	_, err = p.runShellScriptWithTimeout(ctx, p.timeoutForDuration(dragReleaseHoldMs), script.String())
	return err
}

func adbInputPointToPixel(point Point, size adbInputScreenSize) (int, int) {
	return scaleNormalizedToDimension(point.X, size.width), scaleNormalizedToDimension(point.Y, size.height)
}

func scaleNormalizedToDimension(value float64, dimension int) int {
	if dimension <= 1 {
		return 0
	}
	return int(math.Round(clampFloat(value, 0, 1000) / 1000.0 * float64(dimension-1)))
}

func appendADBInputMotionEvent(script *strings.Builder, action string, x, y int) {
	fmt.Fprintf(script, "input touchscreen motionevent %s %d %d\n", action, x, y)
}

func (p *ADBProvider) runShellScriptWithTimeout(ctx context.Context, timeout time.Duration, script string) (string, error) {
	// adb shell concatenates its remaining local argv before asking the remote
	// shell to parse it. Keep the whole command in one argv and quote the inner
	// script explicitly; passing "sh", "-c", script as three argv values causes
	// the remote shell to split the script at its first whitespace character.
	command := "sh -c " + shellSingleQuote(script)
	return p.runShellOutputWithTimeout(ctx, timeout, command)
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func (p *ADBProvider) swipePathWithDuration(ctx context.Context, path [][2]float64, button string, totalDurationMs int) error {
	if len(path) < 2 {
		return fmt.Errorf("swipe path must contain at least 2 points, got %d", len(path))
	}

	// Validate button
	if button != "" && button != ButtonLeft && button != "left" {
		return fmt.Errorf("adb only supports left-button touch semantics, got %q", button)
	}

	// Validate all points and convert to pixels
	pixelPath := make([][2]int, len(path))
	for i, point := range path {
		x, y, err := p.normalizedToPixel(ctx, point[0], point[1])
		if err != nil {
			return fmt.Errorf("invalid point %d in path: %w", i, err)
		}
		pixelPath[i] = [2]int{x, y}
	}

	// Calculate total path length for duration distribution
	totalLength := 0.0
	for i := 1; i < len(pixelPath); i++ {
		dx := float64(pixelPath[i][0] - pixelPath[i-1][0])
		dy := float64(pixelPath[i][1] - pixelPath[i-1][1])
		totalLength += math.Sqrt(dx*dx + dy*dy)
	}

	if totalLength == 0 {
		return InvalidArguments("swipe requires distinct start and end points")
	}

	// Execute each segment as a separate adb swipe
	for i := 1; i < len(pixelPath); i++ {
		start := pixelPath[i-1]
		end := pixelPath[i]

		// Calculate proportional duration for this segment
		dx := float64(end[0] - start[0])
		dy := float64(end[1] - start[1])
		segmentLength := math.Sqrt(dx*dx + dy*dy)
		segmentDurationMs := int(math.Round(float64(totalDurationMs) * segmentLength / totalLength))
		if segmentDurationMs < 1 {
			segmentDurationMs = 1
		}
		segmentDurationMs = p.clampDuration(segmentDurationMs, 0, 0)

		// Execute segment
		if err := p.runShellWithTimeout(ctx, p.timeoutForDuration(segmentDurationMs),
			"input", "swipe",
			strconv.Itoa(start[0]), strconv.Itoa(start[1]),
			strconv.Itoa(end[0]), strconv.Itoa(end[1]),
			strconv.Itoa(segmentDurationMs),
		); err != nil {
			return fmt.Errorf("swipe segment %d failed: %w", i, err)
		}
	}

	return nil
}

// Keypress sends key events through adb.
// Supports single keys and combinations (up to 6 simultaneous keys).
func (p *ADBProvider) Keypress(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return fmt.Errorf("keypress requires at least one key")
	}

	// Resolve keys to Android keycodes
	keycodes, err := p.resolveKeycodes(keys)
	if err != nil {
		return err
	}

	// Single key
	if len(keycodes) == 1 {
		return p.runShell(ctx, "input", "keyevent", keycodes[0])
	}

	androidSDK, err := p.androidSDKVersion(ctx)
	if err != nil {
		return err
	}
	if androidSDK < 30 {
		return ModuleUnavailablef("adb input keycombination requires Android 11 (API 30) or later; detected API %d", androidSDK)
	}

	// Android 12 introduced the optional -t duration flag. Android 11 accepts
	// the same key combination without it.
	args := []string{"input", "keycombination"}
	if androidSDK >= 31 {
		args = append(args, "-t", "50")
	}
	args = append(args, keycodes...)

	return p.runShell(ctx, args...)
}

// Move is not supported on ADB (no hover capability).
func (p *ADBProvider) Move(ctx context.Context, x, y float64) error {
	_ = ctx
	_ = x
	_ = y
	return ModuleUnavailable("adb mouse_move is unsupported because adb input has no hover/pointer move primitive; use touch_gesture for taps or swipes")
}

// Scroll converts scroll to swipe gestures (ADB has no wheel/scroll primitive).
func (p *ADBProvider) Scroll(ctx context.Context, scrollX, scrollY int) error {
	if err := p.rejectActiveDrag("mouse_scroll"); err != nil {
		return err
	}
	// Convert scroll to directional swipe
	centerX := 500.0
	centerY := 500.0

	// Determine swipe strength based on scroll magnitude
	strength := "small"
	if math.Abs(float64(scrollY)) >= 3 || math.Abs(float64(scrollX)) >= 3 {
		strength = "medium"
	}

	distance := 200.0 // Small distance
	if strength == "medium" {
		distance = 500.0
	}

	// Determine swipe direction
	if math.Abs(float64(scrollY)) > math.Abs(float64(scrollX)) {
		// Vertical scroll
		if scrollY < 0 {
			// Scroll down -> swipe up
			return p.swipePathWithDuration(ctx, [][2]float64{
				{centerX, centerY + distance/2},
				{centerX, centerY - distance/2},
			}, ButtonLeft, 650)
		} else {
			// Scroll up -> swipe down
			return p.swipePathWithDuration(ctx, [][2]float64{
				{centerX, centerY - distance/2},
				{centerX, centerY + distance/2},
			}, ButtonLeft, 650)
		}
	} else if scrollX != 0 {
		// Horizontal scroll
		if scrollX < 0 {
			// Scroll left -> swipe right
			return p.swipePathWithDuration(ctx, [][2]float64{
				{centerX - distance/2, centerY},
				{centerX + distance/2, centerY},
			}, ButtonLeft, 650)
		} else {
			// Scroll right -> swipe left
			return p.swipePathWithDuration(ctx, [][2]float64{
				{centerX + distance/2, centerY},
				{centerX - distance/2, centerY},
			}, ButtonLeft, 650)
		}
	}

	return nil
}

// ============================================================================
// Coordinate conversion
// ============================================================================

func (p *ADBProvider) normalizedToPixel(ctx context.Context, x, y float64) (int, int, error) {
	// Validate coordinates
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, 0, fmt.Errorf("coordinates must be finite")
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return 0, 0, fmt.Errorf("coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", x, y)
	}

	// Get screen size
	size, err := p.screenSize(ctx)
	if err != nil {
		return 0, 0, err
	}

	// Convert normalized to pixel coordinates
	// ADB uses pixel coordinates directly
	pixelX := p.scaleToPixel(x, 1001, size.width)
	pixelY := p.scaleToPixel(y, 1001, size.height)

	return pixelX, pixelY, nil
}

func (p *ADBProvider) scaleToPixel(value float64, sourceSize int, targetSize int) int {
	if targetSize <= 1 || sourceSize <= 1 {
		return 0
	}
	maxSource := float64(sourceSize - 1)
	maxTarget := float64(targetSize - 1)
	clamped := clampFloat(value, 0, maxSource)
	return int(math.Round((clamped / maxSource) * maxTarget))
}

// ============================================================================
// Screen size resolution
// ============================================================================

func (p *ADBProvider) screenSize(ctx context.Context) (adbInputScreenSize, error) {
	// Try phone screen info first
	if size, ok := p.phoneScreenSize(); ok {
		return size, nil
	}

	// Check cache
	now := time.Now()
	p.mu.Lock()
	cachedSize := p.cachedScreenSize
	cachedExpiry := p.screenSizeExpires
	p.mu.Unlock()

	if cachedSize.width > 0 && cachedSize.height > 0 && now.Before(cachedExpiry) {
		return cachedSize, nil
	}

	// Query via adb
	out, err := p.runShellOutput(ctx, "wm", "size")
	if err == nil {
		if size, ok := p.parseWMSize(out); ok {
			p.mu.Lock()
			p.cachedScreenSize = size
			p.screenSizeExpires = now.Add(p.screenSizeTTL)
			p.mu.Unlock()
			return size, nil
		}
		err = fmt.Errorf("parse wm size: no screen size in %q", strings.TrimSpace(out))
	}

	// Fallback to screenshot dimensions
	if size, ok := p.fallbackScreenSize(); ok {
		return size, nil
	}

	return adbInputScreenSize{}, fmt.Errorf("resolve adb screen size: %w", err)
}

func (p *ADBProvider) phoneScreenSize() (adbInputScreenSize, bool) {
	if p.screen == nil {
		return adbInputScreenSize{}, false
	}

	info := p.screen.PhoneScreenInfo()
	if info.WidthPixels != nil && info.HeightPixels != nil && *info.WidthPixels > 0 && *info.HeightPixels > 0 {
		return adbInputScreenSize{width: *info.WidthPixels, height: *info.HeightPixels}, true
	}
	if info.NativeWidthPixels != nil && info.NativeHeightPixels != nil && *info.NativeWidthPixels > 0 && *info.NativeHeightPixels > 0 {
		return adbInputScreenSize{width: *info.NativeWidthPixels, height: *info.NativeHeightPixels}, true
	}

	return adbInputScreenSize{}, false
}

func (p *ADBProvider) fallbackScreenSize() (adbInputScreenSize, bool) {
	if p.screen == nil {
		return adbInputScreenSize{}, false
	}

	if width, height, active, age, ok := p.screen.ActiveAreaWithAge(); ok && age < adbScreenDimensionsStaleAfter {
		if active.Valid && active.Width > 0 && active.Height > 0 {
			return adbInputScreenSize{width: active.Width, height: active.Height}, true
		}
		if width > 0 && height > 0 {
			return adbInputScreenSize{width: width, height: height}, true
		}
	}

	return adbInputScreenSize{}, false
}

func (p *ADBProvider) parseWMSize(output string) (adbInputScreenSize, bool) {
	var size adbInputScreenSize
	for _, match := range adbWMSizePattern.FindAllStringSubmatch(output, -1) {
		if len(match) != 3 {
			continue
		}
		width, widthErr := strconv.Atoi(match[1])
		height, heightErr := strconv.Atoi(match[2])
		if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
			continue
		}
		size = adbInputScreenSize{width: width, height: height}
	}
	return size, size.width > 0 && size.height > 0
}

// ============================================================================
// Key resolution
// ============================================================================

func (p *ADBProvider) resolveKeycodes(rawKeys []string) ([]string, error) {
	keycodes := make([]string, 0, len(rawKeys))

	for _, rawKey := range rawKeys {
		normalized := strings.ToLower(strings.TrimSpace(rawKey))
		if normalized == "" {
			continue
		}

		if len(keycodes) >= 6 {
			return nil, fmt.Errorf("keypress supports at most 6 simultaneous keys")
		}

		keycode, err := p.resolveKeycode(normalized)
		if err != nil {
			return nil, err
		}
		keycodes = append(keycodes, keycode)
	}

	if len(keycodes) == 0 {
		return nil, fmt.Errorf("at least one key is required")
	}

	return keycodes, nil
}

func (p *ADBProvider) resolveKeycode(key string) (string, error) {
	// Check aliases first
	if keycode, ok := adbAndroidKeycodeAliases[key]; ok {
		return keycode, nil
	}

	// Check standard keyboard map
	if keycode, ok := adbKeyboardKeycodeMap[key]; ok {
		return keycode, nil
	}

	// Pass through KEYCODE_* format
	if strings.HasPrefix(strings.ToUpper(key), "KEYCODE_") {
		return strings.ToUpper(key), nil
	}

	// Reject HID-only keys
	if strings.HasPrefix(key, "key_usage_") {
		return "", fmt.Errorf("%s is a HID usage alias and has no adb keyevent equivalent", key)
	}

	return "", fmt.Errorf("unknown adb key: %q", key)
}

// ============================================================================
// ADB command execution
// ============================================================================

func (p *ADBProvider) runShell(ctx context.Context, args ...string) error {
	_, err := p.runShellOutput(ctx, args...)
	return err
}

func (p *ADBProvider) runShellWithTimeout(ctx context.Context, timeout time.Duration, args ...string) error {
	_, err := p.runShellOutputWithTimeout(ctx, timeout, args...)
	return err
}

func (p *ADBProvider) runShellOutput(ctx context.Context, args ...string) (string, error) {
	return p.runShellOutputWithTimeout(ctx, p.commandTimeout, args...)
}

func (p *ADBProvider) runShellOutputWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	shellArgs := make([]string, 0, len(args)+1)
	shellArgs = append(shellArgs, "shell")
	shellArgs = append(shellArgs, args...)
	return p.runWithTimeout(ctx, timeout, shellArgs...)
}

func (p *ADBProvider) runWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if timeout <= 0 {
		timeout = p.commandTimeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Get adb path
	adbPath, err := p.client.ADBPath()
	if err != nil {
		return "", fmt.Errorf("adb not available: %w", err)
	}

	// Resolve device serial
	serial, err := p.client.ResolveSerial(cmdCtx, adbPath)
	if err != nil {
		return "", fmt.Errorf("resolve adb device: %w", err)
	}

	// Build adb arguments
	adbArgs := make([]string, 0, len(args)+2)
	if serial != "" {
		adbArgs = append(adbArgs, "-s", serial)
	}
	adbArgs = append(adbArgs, args...)

	// Execute command
	stdout, stderr, err := p.runADB(cmdCtx, adbPath, adbArgs...)
	if err != nil {
		p.client.InvalidateAutoSerial(serial)
		if trimmed := strings.TrimSpace(string(stderr)); trimmed != "" {
			return strings.TrimSpace(string(stdout)), fmt.Errorf("adb command failed: %s", trimmed)
		}
		return strings.TrimSpace(string(stdout)), err
	}

	out := strings.TrimSpace(string(stdout))
	if errText := strings.TrimSpace(string(stderr)); errText != "" {
		if out != "" {
			out += "\n"
		}
		out += errText
	}

	return out, nil
}

// ============================================================================
// Utility functions
// ============================================================================

func (p *ADBProvider) timeoutForDuration(durationMs int) time.Duration {
	if durationMs <= 0 {
		return p.commandTimeout
	}
	timeout := time.Duration(durationMs)*time.Millisecond + p.gestureTimeoutPad
	if timeout < p.commandTimeout {
		return p.commandTimeout
	}
	return timeout
}

func (p *ADBProvider) clampDuration(value, fallback, minimum int) int {
	if value <= 0 {
		value = fallback
	}
	if value < minimum {
		value = minimum
	}
	if value > p.maxDurationMs {
		value = p.maxDurationMs
	}
	return value
}

// ADBScreenClient discovers the adb executable and resolves a target device.
type ADBScreenClient struct {
	mu              sync.Mutex
	adbPath         string
	autoSerial      string
	autoSerialValid bool
	commandRunner   adbCommandRunner
}

func NewADBScreenClient() *ADBScreenClient {
	return &ADBScreenClient{}
}

func (c *ADBScreenClient) setCommandRunner(runner adbCommandRunner) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commandRunner = runner
}

func (c *ADBScreenClient) ADBPath() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.adbPath != "" {
		return c.adbPath, nil
	}

	if configured := strings.TrimSpace(os.Getenv("AIDEN_ADB_PATH")); configured != "" {
		c.adbPath = configured
		return c.adbPath, nil
	}

	// Try to find adb in PATH
	path, err := exec.LookPath("adb")
	if err != nil {
		return "", fmt.Errorf("adb not found in PATH: %w", err)
	}

	c.adbPath = path
	return path, nil
}

func (c *ADBScreenClient) ResolveSerial(ctx context.Context, adbPath string) (string, error) {
	for _, key := range []string{"AIDEN_ADB_SERIAL", "ANDROID_SERIAL"} {
		if serial := strings.TrimSpace(os.Getenv(key)); serial != "" {
			return serial, nil
		}
	}

	c.mu.Lock()
	if c.autoSerialValid && c.autoSerial != "" {
		serial := c.autoSerial
		c.mu.Unlock()
		return serial, nil
	}
	runner := c.commandRunner
	c.mu.Unlock()

	// Use injectable runner if available, otherwise fall back to package-level function
	if runner == nil {
		runner = runADBCommand
	}

	// Query connected devices
	stdout, stderr, err := runner(ctx, adbPath, "devices")
	if err != nil {
		return "", fmt.Errorf("adb devices failed: %w", err)
	}

	output := string(stdout)
	if errText := strings.TrimSpace(string(stderr)); errText != "" {
		output += "\n" + errText
	}

	// Parse device list
	var devices []string
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			devices = append(devices, parts[0])
		}
	}

	if len(devices) == 0 {
		return "", fmt.Errorf("no adb devices connected")
	}

	if len(devices) > 1 {
		return "", fmt.Errorf("multiple adb devices connected; set AIDEN_ADB_SERIAL or ANDROID_SERIAL")
	}

	// There is exactly one connected device, so it is safe to cache it.
	serial := devices[0]

	c.mu.Lock()
	c.autoSerial = serial
	c.autoSerialValid = true
	c.mu.Unlock()

	return serial, nil
}

func (p *ADBProvider) androidSDKVersion(ctx context.Context) (int, error) {
	p.mu.Lock()
	if p.androidSDKValid {
		androidSDK := p.androidSDK
		p.mu.Unlock()
		return androidSDK, nil
	}
	p.mu.Unlock()

	output, err := p.runShellOutput(ctx, "getprop", "ro.build.version.sdk")
	if err != nil {
		return 0, fmt.Errorf("detect Android API level for key combination: %w", err)
	}
	androidSDK, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || androidSDK <= 0 {
		return 0, fmt.Errorf("detect Android API level for key combination: invalid value %q", output)
	}

	p.mu.Lock()
	p.androidSDK = androidSDK
	p.androidSDKValid = true
	p.mu.Unlock()
	return androidSDK, nil
}

func (c *ADBScreenClient) InvalidateAutoSerial(serial string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.autoSerial == serial {
		c.autoSerialValid = false
	}
}

func runADBCommand(ctx context.Context, adbPath string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, adbPath, args...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

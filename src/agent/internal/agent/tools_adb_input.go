package agent

import (
	"aico-agent/internal/agent/screen"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adbInputCommandTimeout    = 8 * time.Second
	adbInputGestureTimeoutPad = 3 * time.Second
	adbInputScreenSizeTTL     = 30 * time.Second
	adbKeyboardIME            = "com.android.adbkeyboard/.AdbIME"
	defaultADBTextRestoreWait = 80
	maxADBActionDurationMs    = 10_000
)

var (
	errADBInputInvalidArgument = errors.New("invalid adb input argument")
	errADBInputUnsupported     = errors.New("unsupported adb input")
	adbWMSizePattern           = regexp.MustCompile(`(?m)(?:Physical|Override) size:\s*([0-9]+)x([0-9]+)`)
)

type adbCommandRunner func(context.Context, string, ...string) ([]byte, []byte, error)

type adbInputScreenSize struct {
	width  int
	height int
}

// ADBInputController sends Android input events through adb shell input.
type ADBInputController struct {
	screen *screen.ScreenState
	client *ADBScreenClient
	runADB adbCommandRunner

	mu                sync.Mutex
	cachedScreenSize  adbInputScreenSize
	screenSizeExpires time.Time
}

func NewADBInputController(screen *screen.ScreenState) *ADBInputController {
	return &ADBInputController{
		screen: screen,
		client: NewADBScreenClient(),
		runADB: runADBCommand,
	}
}

func (c *ADBInputController) Tap(ctx context.Context, point resolvedPointerPoint) error {
	return c.runShell(ctx, "input", "tap", strconv.Itoa(point.x), strconv.Itoa(point.y))
}

func (c *ADBInputController) Swipe(ctx context.Context, start, end resolvedPointerPoint, durationMs int) error {
	durationMs = clampADBActionDurationMs(durationMs, 0, 0)
	return c.runShellWithTimeout(ctx, adbInputTimeoutForDuration(durationMs), "input", "swipe",
		strconv.Itoa(start.x), strconv.Itoa(start.y),
		strconv.Itoa(end.x), strconv.Itoa(end.y),
		strconv.Itoa(durationMs),
	)
}

func (c *ADBInputController) LongPress(ctx context.Context, point resolvedPointerPoint, durationMs int) error {
	durationMs = clampADBActionDurationMs(durationMs, 500, 1)
	return c.Swipe(ctx, point, point, durationMs)
}

func (c *ADBInputController) KeyTap(ctx context.Context, keys []string, holdMs int) error {
	keycodes, err := resolveADBKeycodes(keys)
	if err != nil {
		return err
	}
	if len(keycodes) == 0 {
		return fmt.Errorf("%w: at least one key is required", errADBInputInvalidArgument)
	}
	if len(keycodes) == 1 {
		return c.runShell(ctx, "input", "keyevent", keycodes[0])
	}
	if holdMs <= 0 {
		holdMs = defaultKeyboardTapHoldMs
	}
	holdMs = clampADBActionDurationMs(holdMs, defaultKeyboardTapHoldMs, 1)
	args := []string{"input", "keycombination", "-t", strconv.Itoa(holdMs)}
	args = append(args, keycodes...)
	return c.runShell(ctx, args...)
}

func (c *ADBInputController) Text(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}

	originalIME, err := c.runShellOutput(ctx, "settings", "get", "secure", "default_input_method")
	if err != nil {
		return fmt.Errorf("read current Android IME: %w", err)
	}
	originalIME = strings.TrimSpace(originalIME)

	switched := !strings.Contains(originalIME, adbKeyboardIME)
	if switched {
		out, err := c.runShellOutput(ctx, "ime", "set", adbKeyboardIME)
		if err != nil || strings.Contains(strings.ToLower(out), "error") {
			if fallbackErr := c.TextViaKeyEvents(ctx, text); fallbackErr != nil {
				if err != nil {
					return fmt.Errorf("set ADBKeyboard IME %s: %w; adb keyevent fallback failed: %v", adbKeyboardIME, err, fallbackErr)
				}
				return fmt.Errorf("set ADBKeyboard IME %s failed: %s; adb keyevent fallback failed: %v", adbKeyboardIME, strings.TrimSpace(out), fallbackErr)
			}
			return nil
		}
		defer func() {
			if originalIME == "" || strings.EqualFold(originalIME, "null") {
				return
			}
			sleepMs(defaultADBTextRestoreWait)
			restoreCtx, cancel := context.WithTimeout(context.Background(), adbInputCommandTimeout)
			defer cancel()
			_ = c.runShell(restoreCtx, "ime", "set", originalIME)
		}()
	}

	encodedText := base64.StdEncoding.EncodeToString([]byte(text))
	if err := c.runShell(ctx, "am", "broadcast", "-a", "ADB_INPUT_B64", "--es", "msg", encodedText); err != nil {
		return fmt.Errorf("broadcast ADBKeyboard text: %w", err)
	}
	return nil
}

func (c *ADBInputController) TextViaKeyEvents(ctx context.Context, text string) error {
	for _, ch := range text {
		keycode, shifted, ok := adbTextKeyEventForASCII(ch)
		if !ok {
			return fmt.Errorf("%w: adb keyevent fallback cannot type %q", errADBInputInvalidArgument, ch)
		}
		if shifted {
			if err := c.runShell(ctx, "input", "keycombination", "-t", strconv.Itoa(defaultKeyboardTapHoldMs), "KEYCODE_SHIFT_LEFT", keycode); err != nil {
				return err
			}
			continue
		}
		if err := c.runShell(ctx, "input", "keyevent", keycode); err != nil {
			return err
		}
	}
	return nil
}

func (c *ADBInputController) ResolveRequiredPoint(ctx context.Context, point *pointerPoint) (resolvedPointerPoint, error) {
	if point == nil {
		return resolvedPointerPoint{}, fmt.Errorf("%w: point is required", errADBInputInvalidArgument)
	}
	return c.ResolvePosition(ctx, point.X.Float64(), point.Y.Float64())
}

func (c *ADBInputController) ResolvePointOrDefaultNormalized(ctx context.Context, point *pointerPoint, defaultX, defaultY float64) (resolvedPointerPoint, error) {
	if point != nil {
		return c.ResolveRequiredPoint(ctx, point)
	}
	return c.ResolvePosition(ctx, defaultX, defaultY)
}

func (c *ADBInputController) ResolvePosition(ctx context.Context, x, y float64) (resolvedPointerPoint, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return resolvedPointerPoint{}, fmt.Errorf("%w: coordinates must be finite", errADBInputInvalidArgument)
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return resolvedPointerPoint{}, fmt.Errorf("%w: coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", errADBInputInvalidArgument, x, y)
	}
	size, err := c.screenSize(ctx)
	if err != nil {
		return resolvedPointerPoint{}, err
	}
	return normalizedToADBPoint(size, x, y), nil
}

func (c *ADBInputController) screenSize(ctx context.Context) (adbInputScreenSize, error) {
	if c == nil {
		return adbInputScreenSize{}, fmt.Errorf("%w: adb input controller is not configured", errADBInputUnsupported)
	}
	if size, ok := c.phoneScreenSize(); ok {
		return size, nil
	}

	now := time.Now()
	c.mu.Lock()
	cachedSize := c.cachedScreenSize
	cachedExpiry := c.screenSizeExpires
	c.mu.Unlock()
	if cachedSize.width > 0 && cachedSize.height > 0 && now.Before(cachedExpiry) {
		return cachedSize, nil
	}

	out, err := c.runShellOutput(ctx, "wm", "size")
	if err == nil {
		if size, ok := parseADBWMSize(out); ok {
			c.mu.Lock()
			c.cachedScreenSize = size
			c.screenSizeExpires = now.Add(adbInputScreenSizeTTL)
			c.mu.Unlock()
			return size, nil
		}
		err = fmt.Errorf("parse wm size: no screen size in %q", strings.TrimSpace(out))
	}

	if size, ok := c.fallbackScreenSize(); ok {
		return size, nil
	}
	return adbInputScreenSize{}, fmt.Errorf("resolve adb screen size: %w", err)
}

func (c *ADBInputController) phoneScreenSize() (adbInputScreenSize, bool) {
	if c == nil || c.screen == nil {
		return adbInputScreenSize{}, false
	}
	info := c.screen.PhoneScreenInfo()
	if info.WidthPixels != nil && info.HeightPixels != nil && *info.WidthPixels > 0 && *info.HeightPixels > 0 {
		return adbInputScreenSize{width: *info.WidthPixels, height: *info.HeightPixels}, true
	}
	if info.NativeWidthPixels != nil && info.NativeHeightPixels != nil && *info.NativeWidthPixels > 0 && *info.NativeHeightPixels > 0 {
		return adbInputScreenSize{width: *info.NativeWidthPixels, height: *info.NativeHeightPixels}, true
	}
	return adbInputScreenSize{}, false
}

func (c *ADBInputController) fallbackScreenSize() (adbInputScreenSize, bool) {
	if c == nil || c.screen == nil {
		return adbInputScreenSize{}, false
	}
	if width, height, active, age, ok := c.screen.ActiveAreaWithAge(); ok && age < screenDimensionsStaleAfter {
		if active.Valid && active.Width > 0 && active.Height > 0 {
			return adbInputScreenSize{width: active.Width, height: active.Height}, true
		}
		if width > 0 && height > 0 {
			return adbInputScreenSize{width: width, height: height}, true
		}
	}
	return adbInputScreenSize{}, false
}

func (c *ADBInputController) runShell(ctx context.Context, args ...string) error {
	_, err := c.runShellOutput(ctx, args...)
	return err
}

func (c *ADBInputController) runShellWithTimeout(ctx context.Context, timeout time.Duration, args ...string) error {
	_, err := c.runShellOutputWithTimeout(ctx, timeout, args...)
	return err
}

func (c *ADBInputController) runShellOutput(ctx context.Context, args ...string) (string, error) {
	return c.runShellOutputWithTimeout(ctx, adbInputCommandTimeout, args...)
}

func (c *ADBInputController) runShellOutputWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	shellArgs := make([]string, 0, len(args)+1)
	shellArgs = append(shellArgs, "shell")
	shellArgs = append(shellArgs, args...)
	return c.runWithTimeout(ctx, timeout, shellArgs...)
}

func (c *ADBInputController) run(ctx context.Context, args ...string) (string, error) {
	return c.runWithTimeout(ctx, adbInputCommandTimeout, args...)
}

func (c *ADBInputController) runWithTimeout(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: adb input controller is not configured", errADBInputUnsupported)
	}
	client := c.client
	if client == nil {
		client = NewADBScreenClient()
	}
	runADB := c.runADB
	if runADB == nil {
		runADB = runADBCommand
	}

	if timeout <= 0 {
		timeout = adbInputCommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	adbPath, err := client.ADBPath()
	if err != nil {
		return "", fmt.Errorf("%w: %v", errADBInputUnsupported, err)
	}
	serial, err := client.ResolveSerial(cmdCtx, adbPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errADBInputUnsupported, err)
	}

	adbArgs := make([]string, 0, len(args)+2)
	if serial != "" {
		adbArgs = append(adbArgs, "-s", serial)
	}
	adbArgs = append(adbArgs, args...)

	stdout, stderr, err := runADB(cmdCtx, adbPath, adbArgs...)
	if err != nil {
		client.InvalidateAutoSerial(serial)
		if trimmed := strings.TrimSpace(string(stderr)); trimmed != "" {
			return strings.TrimSpace(string(stdout)), fmt.Errorf("adb input failed: %s", trimmed)
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

func adbInputTimeoutForDuration(durationMs int) time.Duration {
	if durationMs <= 0 {
		return adbInputCommandTimeout
	}
	timeout := time.Duration(durationMs)*time.Millisecond + adbInputGestureTimeoutPad
	if timeout < adbInputCommandTimeout {
		return adbInputCommandTimeout
	}
	return timeout
}

func clampADBActionDurationMs(value, fallback, minimum int) int {
	if value <= 0 {
		value = fallback
	}
	if value < minimum {
		value = minimum
	}
	if value > maxADBActionDurationMs {
		value = maxADBActionDurationMs
	}
	return value
}

func parseADBWMSize(output string) (adbInputScreenSize, bool) {
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

func normalizedToADBPoint(size adbInputScreenSize, x, y float64) resolvedPointerPoint {
	return resolvedPointerPoint{
		x: scaleFloatToPixel(clampFloat(x, 0, 1000), 1001, size.width),
		y: scaleFloatToPixel(clampFloat(y, 0, 1000), 1001, size.height),
	}
}

func scaleFloatToPixel(value float64, sourceSize int, targetSize int) int {
	if targetSize <= 1 || sourceSize <= 1 {
		return 0
	}
	maxSource := float64(sourceSize - 1)
	maxTarget := float64(targetSize - 1)
	return int(math.Round((clampFloat(value, 0, maxSource) / maxSource) * maxTarget))
}

func resolveADBKeycodes(rawKeys []string) ([]string, error) {
	keycodes := make([]string, 0, len(rawKeys))
	for _, rawKey := range rawKeys {
		normalized := strings.ToLower(strings.TrimSpace(rawKey))
		if normalized == "" {
			continue
		}
		if len(keycodes) >= 6 {
			return nil, fmt.Errorf("%w: keyboard_tap supports at most 6 simultaneous keys", errADBInputInvalidArgument)
		}
		keycode, err := resolveADBKeycode(normalized)
		if err != nil {
			return nil, err
		}
		keycodes = append(keycodes, keycode)
	}
	if len(keycodes) == 0 {
		return nil, fmt.Errorf("%w: at least one key or modifier is required", errADBInputInvalidArgument)
	}
	return keycodes, nil
}

func resolveADBKeycode(key string) (string, error) {
	if keycode, ok := adbAndroidKeycodeAliases[key]; ok {
		return keycode, nil
	}
	if alias, ok := androidKeyboardTapAliases[key]; ok && alias.Keycode > 0 {
		return strconv.Itoa(alias.Keycode), nil
	}
	if keycode, ok := adbKeyboardKeycodeMap[key]; ok {
		return keycode, nil
	}
	if strings.HasPrefix(key, "keycode_") {
		return strings.ToUpper(key), nil
	}
	if strings.HasPrefix(key, "key_usage_") {
		return "", fmt.Errorf("%w: %s is a HID usage alias and has no adb keyevent equivalent", errADBInputInvalidArgument, key)
	}
	return "", fmt.Errorf("%w: unknown adb key: %q", errADBInputInvalidArgument, key)
}

var adbAndroidKeycodeAliases = map[string]string{
	"android_back":              "KEYCODE_BACK",
	"back":                      "KEYCODE_BACK",
	"keycode_back":              "KEYCODE_BACK",
	"android_home":              "KEYCODE_HOME",
	"home":                      "KEYCODE_HOME",
	"keycode_home":              "KEYCODE_HOME",
	"menu":                      "KEYCODE_MENU",
	"keycode_menu":              "KEYCODE_MENU",
	"search":                    "KEYCODE_SEARCH",
	"keycode_search":            "KEYCODE_SEARCH",
	"power":                     "KEYCODE_POWER",
	"keycode_power":             "KEYCODE_POWER",
	"sleep":                     "KEYCODE_SLEEP",
	"keycode_sleep":             "KEYCODE_SLEEP",
	"volume_mute":               "KEYCODE_VOLUME_MUTE",
	"keycode_volume_mute":       "KEYCODE_VOLUME_MUTE",
	"volumeup":                  "KEYCODE_VOLUME_UP",
	"volume_up":                 "KEYCODE_VOLUME_UP",
	"keycode_volume_up":         "KEYCODE_VOLUME_UP",
	"volumedown":                "KEYCODE_VOLUME_DOWN",
	"volume_down":               "KEYCODE_VOLUME_DOWN",
	"keycode_volume_down":       "KEYCODE_VOLUME_DOWN",
	"media_fast_forward":        "KEYCODE_MEDIA_FAST_FORWARD",
	"media_rewind":              "KEYCODE_MEDIA_REWIND",
	"media_next":                "KEYCODE_MEDIA_NEXT",
	"media_previous":            "KEYCODE_MEDIA_PREVIOUS",
	"media_stop":                "KEYCODE_MEDIA_STOP",
	"media_play_pause":          "KEYCODE_MEDIA_PLAY_PAUSE",
	"app_switch":                "KEYCODE_APP_SWITCH",
	"keycode_app_switch":        "KEYCODE_APP_SWITCH",
	"recents":                   "KEYCODE_APP_SWITCH",
	"return":                    "KEYCODE_ENTER",
	"send":                      "KEYCODE_ENTER",
	"delete_backward":           "KEYCODE_DEL",
	"backward_delete":           "KEYCODE_DEL",
	"esc":                       "KEYCODE_BACK",
	"escape":                    "KEYCODE_BACK",
	"move_home":                 "KEYCODE_MOVE_HOME",
	"keyboard_home":             "KEYCODE_MOVE_HOME",
	"screenshot":                "KEYCODE_SYSRQ",
	"key_usage_screenshot":      "KEYCODE_SYSRQ",
	"brightness_up":             "KEYCODE_BRIGHTNESS_UP",
	"key_usage_brightness_up":   "KEYCODE_BRIGHTNESS_UP",
	"brightness_down":           "KEYCODE_BRIGHTNESS_DOWN",
	"key_usage_brightness_down": "KEYCODE_BRIGHTNESS_DOWN",
}

var adbKeyboardKeycodeMap = map[string]string{
	"a": "KEYCODE_A", "b": "KEYCODE_B", "c": "KEYCODE_C", "d": "KEYCODE_D",
	"e": "KEYCODE_E", "f": "KEYCODE_F", "g": "KEYCODE_G", "h": "KEYCODE_H",
	"i": "KEYCODE_I", "j": "KEYCODE_J", "k": "KEYCODE_K", "l": "KEYCODE_L",
	"m": "KEYCODE_M", "n": "KEYCODE_N", "o": "KEYCODE_O", "p": "KEYCODE_P",
	"q": "KEYCODE_Q", "r": "KEYCODE_R", "s": "KEYCODE_S", "t": "KEYCODE_T",
	"u": "KEYCODE_U", "v": "KEYCODE_V", "w": "KEYCODE_W", "x": "KEYCODE_X",
	"y": "KEYCODE_Y", "z": "KEYCODE_Z",
	"0": "KEYCODE_0", "1": "KEYCODE_1", "2": "KEYCODE_2", "3": "KEYCODE_3",
	"4": "KEYCODE_4", "5": "KEYCODE_5", "6": "KEYCODE_6", "7": "KEYCODE_7",
	"8": "KEYCODE_8", "9": "KEYCODE_9",

	"enter": "KEYCODE_ENTER", "escape": "KEYCODE_ESCAPE", "backspace": "KEYCODE_DEL",
	"tab": "KEYCODE_TAB", "space": "KEYCODE_SPACE", "minus": "KEYCODE_MINUS",
	"equal": "KEYCODE_EQUALS", "leftbrace": "KEYCODE_LEFT_BRACKET", "rightbrace": "KEYCODE_RIGHT_BRACKET",
	"backslash": "KEYCODE_BACKSLASH", "semicolon": "KEYCODE_SEMICOLON", "apostrophe": "KEYCODE_APOSTROPHE",
	"grave": "KEYCODE_GRAVE", "comma": "KEYCODE_COMMA", "dot": "KEYCODE_PERIOD", "slash": "KEYCODE_SLASH",
	"capslock": "KEYCODE_CAPS_LOCK",
	"f1":       "KEYCODE_F1", "f2": "KEYCODE_F2", "f3": "KEYCODE_F3", "f4": "KEYCODE_F4",
	"f5": "KEYCODE_F5", "f6": "KEYCODE_F6", "f7": "KEYCODE_F7", "f8": "KEYCODE_F8",
	"f9": "KEYCODE_F9", "f10": "KEYCODE_F10", "f11": "KEYCODE_F11", "f12": "KEYCODE_F12",
	"printscreen": "KEYCODE_SYSRQ", "scrolllock": "KEYCODE_SCROLL_LOCK", "pause": "KEYCODE_BREAK",
	"insert": "KEYCODE_INSERT", "home": "KEYCODE_MOVE_HOME", "pageup": "KEYCODE_PAGE_UP",
	"delete": "KEYCODE_FORWARD_DEL", "end": "KEYCODE_MOVE_END", "pagedown": "KEYCODE_PAGE_DOWN",
	"right": "KEYCODE_DPAD_RIGHT", "left": "KEYCODE_DPAD_LEFT", "down": "KEYCODE_DPAD_DOWN", "up": "KEYCODE_DPAD_UP",

	"ctrl": "KEYCODE_CTRL_LEFT", "lctrl": "KEYCODE_CTRL_LEFT", "rctrl": "KEYCODE_CTRL_RIGHT",
	"shift": "KEYCODE_SHIFT_LEFT", "lshift": "KEYCODE_SHIFT_LEFT", "rshift": "KEYCODE_SHIFT_RIGHT",
	"alt": "KEYCODE_ALT_LEFT", "lalt": "KEYCODE_ALT_LEFT", "ralt": "KEYCODE_ALT_RIGHT",
	"meta": "KEYCODE_META_LEFT", "lmeta": "KEYCODE_META_LEFT", "rmeta": "KEYCODE_META_RIGHT",
	"super": "KEYCODE_META_LEFT", "win": "KEYCODE_META_LEFT", "cmd": "KEYCODE_META_LEFT",
}

func adbTextKeyEventForASCII(ch rune) (string, bool, bool) {
	switch {
	case ch >= 'a' && ch <= 'z':
		return "KEYCODE_" + string('A'+(ch-'a')), false, true
	case ch >= 'A' && ch <= 'Z':
		return "KEYCODE_" + string(ch), true, true
	case ch >= '1' && ch <= '9':
		return "KEYCODE_" + string(ch), false, true
	case ch == '0':
		return "KEYCODE_0", false, true
	}

	switch ch {
	case ' ':
		return "KEYCODE_SPACE", false, true
	case '\n', '\r':
		return "KEYCODE_ENTER", false, true
	case '\t':
		return "KEYCODE_TAB", false, true
	case '-':
		return "KEYCODE_MINUS", false, true
	case '=':
		return "KEYCODE_EQUALS", false, true
	case '[':
		return "KEYCODE_LEFT_BRACKET", false, true
	case ']':
		return "KEYCODE_RIGHT_BRACKET", false, true
	case '\\':
		return "KEYCODE_BACKSLASH", false, true
	case ';':
		return "KEYCODE_SEMICOLON", false, true
	case '\'':
		return "KEYCODE_APOSTROPHE", false, true
	case '`':
		return "KEYCODE_GRAVE", false, true
	case ',':
		return "KEYCODE_COMMA", false, true
	case '.':
		return "KEYCODE_PERIOD", false, true
	case '/':
		return "KEYCODE_SLASH", false, true
	case '!':
		return "KEYCODE_1", true, true
	case '@':
		return "KEYCODE_2", true, true
	case '#':
		return "KEYCODE_3", true, true
	case '$':
		return "KEYCODE_4", true, true
	case '%':
		return "KEYCODE_5", true, true
	case '^':
		return "KEYCODE_6", true, true
	case '&':
		return "KEYCODE_7", true, true
	case '*':
		return "KEYCODE_8", true, true
	case '(':
		return "KEYCODE_9", true, true
	case ')':
		return "KEYCODE_0", true, true
	case '_':
		return "KEYCODE_MINUS", true, true
	case '+':
		return "KEYCODE_EQUALS", true, true
	case '{':
		return "KEYCODE_LEFT_BRACKET", true, true
	case '}':
		return "KEYCODE_RIGHT_BRACKET", true, true
	case '|':
		return "KEYCODE_BACKSLASH", true, true
	case ':':
		return "KEYCODE_SEMICOLON", true, true
	case '"':
		return "KEYCODE_APOSTROPHE", true, true
	case '~':
		return "KEYCODE_GRAVE", true, true
	case '<':
		return "KEYCODE_COMMA", true, true
	case '>':
		return "KEYCODE_PERIOD", true, true
	case '?':
		return "KEYCODE_SLASH", true, true
	}
	return "", false, false
}

func adbInputToolErrorCode(err error) string {
	if errors.Is(err, errADBInputInvalidArgument) {
		return CodeInvalidArguments
	}
	if errors.Is(err, errADBInputUnsupported) {
		return CodeModuleUnavailable
	}
	return CodeToolExecutionFailed
}

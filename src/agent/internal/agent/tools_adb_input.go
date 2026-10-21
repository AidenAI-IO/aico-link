package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	adbInputCommandTimeout    = 8 * time.Second
	adbKeyboardIME            = "com.android.adbkeyboard/.AdbIME"
	defaultADBTextRestoreWait = 80
)

var (
	errADBInputInvalidArgument = errors.New("invalid adb input argument")
	errADBInputUnsupported     = errors.New("unsupported adb input")
)

type adbCommandRunner func(context.Context, string, ...string) ([]byte, []byte, error)

// ADBInputController types keyboard_text on Android through ADBKeyboard,
// falling back to adb shell input keyevents.
type ADBInputController struct {
	client *ADBScreenClient
	runADB adbCommandRunner
}

func NewADBInputController() *ADBInputController {
	return &ADBInputController{
		client: NewADBScreenClient(),
		runADB: runADBCommand,
	}
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

func (c *ADBInputController) runShell(ctx context.Context, args ...string) error {
	_, err := c.runShellOutput(ctx, args...)
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

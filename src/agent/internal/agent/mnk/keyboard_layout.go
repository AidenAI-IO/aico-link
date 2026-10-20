package mnk

import "strings"

const (
	KeyboardLayoutQWERTY = "qwerty"
	KeyboardLayoutAZERTY = "azerty"
	KeyboardLayoutQWERTZ = "qwertz"

	DefaultKeyboardLayout = KeyboardLayoutQWERTY

	hidModifierLeftShift = 0x02
	hidModifierRightAlt  = 0x40
)

type KeyboardLayoutDefinition struct {
	Value string
	Label string
}

// KeyboardLayoutDefinitions lists the supported layouts in display order. The
// agent config validates hid.keyboard_layout and builds its options from it.
var KeyboardLayoutDefinitions = []KeyboardLayoutDefinition{
	{Value: KeyboardLayoutQWERTY, Label: "QWERTY"},
	{Value: KeyboardLayoutAZERTY, Label: "AZERTY"},
	{Value: KeyboardLayoutQWERTZ, Label: "QWERTZ"},
}

// KeyStroke is the HID modifier byte and key usage that type one character.
type KeyStroke struct {
	Modifier uint8
	Usage    uint8
}

var keyboardTapTextKeyMap = map[string]byte{
	"space":      ' ',
	"minus":      '-',
	"equal":      '=',
	"leftbrace":  '[',
	"rightbrace": ']',
	"backslash":  '\\',
	"semicolon":  ';',
	"apostrophe": '\'',
	"grave":      '`',
	"comma":      ',',
	"dot":        '.',
	"slash":      '/',
}

var qwertySymbolKeyStrokes = map[byte]KeyStroke{
	' ':  {Usage: 0x2c},
	'\n': {Usage: 0x28},
	'\r': {Usage: 0x28},
	'\t': {Usage: 0x2b},
	'-':  {Usage: 0x2d},
	'=':  {Usage: 0x2e},
	'[':  {Usage: 0x2f},
	']':  {Usage: 0x30},
	'\\': {Usage: 0x31},
	';':  {Usage: 0x33},
	'\'': {Usage: 0x34},
	'`':  {Usage: 0x35},
	',':  {Usage: 0x36},
	'.':  {Usage: 0x37},
	'/':  {Usage: 0x38},
	'!':  {Modifier: hidModifierLeftShift, Usage: 0x1e},
	'@':  {Modifier: hidModifierLeftShift, Usage: 0x1f},
	'#':  {Modifier: hidModifierLeftShift, Usage: 0x20},
	'$':  {Modifier: hidModifierLeftShift, Usage: 0x21},
	'%':  {Modifier: hidModifierLeftShift, Usage: 0x22},
	'^':  {Modifier: hidModifierLeftShift, Usage: 0x23},
	'&':  {Modifier: hidModifierLeftShift, Usage: 0x24},
	'*':  {Modifier: hidModifierLeftShift, Usage: 0x25},
	'(':  {Modifier: hidModifierLeftShift, Usage: 0x26},
	')':  {Modifier: hidModifierLeftShift, Usage: 0x27},
	'_':  {Modifier: hidModifierLeftShift, Usage: 0x2d},
	'+':  {Modifier: hidModifierLeftShift, Usage: 0x2e},
	'{':  {Modifier: hidModifierLeftShift, Usage: 0x2f},
	'}':  {Modifier: hidModifierLeftShift, Usage: 0x30},
	'|':  {Modifier: hidModifierLeftShift, Usage: 0x31},
	':':  {Modifier: hidModifierLeftShift, Usage: 0x33},
	'"':  {Modifier: hidModifierLeftShift, Usage: 0x34},
	'~':  {Modifier: hidModifierLeftShift, Usage: 0x35},
	'<':  {Modifier: hidModifierLeftShift, Usage: 0x36},
	'>':  {Modifier: hidModifierLeftShift, Usage: 0x37},
	'?':  {Modifier: hidModifierLeftShift, Usage: 0x38},
}

var azertyLetterUsageOverrides = map[byte]uint8{
	'a': 0x14,
	'q': 0x04,
	'w': 0x1d,
	'z': 0x1a,
	'm': 0x33,
}

var azertySymbolKeyStrokes = map[byte]KeyStroke{
	' ':  {Usage: 0x2c},
	'\n': {Usage: 0x28},
	'\r': {Usage: 0x28},
	'\t': {Usage: 0x2b},
	'&':  {Usage: 0x1e},
	'~':  {Modifier: hidModifierRightAlt, Usage: 0x1f},
	'"':  {Usage: 0x20},
	'#':  {Modifier: hidModifierRightAlt, Usage: 0x20},
	'\'': {Usage: 0x21},
	'{':  {Modifier: hidModifierRightAlt, Usage: 0x21},
	'(':  {Usage: 0x22},
	'[':  {Modifier: hidModifierRightAlt, Usage: 0x22},
	'-':  {Usage: 0x23},
	'|':  {Modifier: hidModifierRightAlt, Usage: 0x23},
	'`':  {Modifier: hidModifierRightAlt, Usage: 0x24},
	'_':  {Usage: 0x25},
	'\\': {Modifier: hidModifierRightAlt, Usage: 0x25},
	'^':  {Modifier: hidModifierRightAlt, Usage: 0x26},
	'@':  {Modifier: hidModifierRightAlt, Usage: 0x27},
	')':  {Usage: 0x2d},
	']':  {Modifier: hidModifierRightAlt, Usage: 0x2d},
	'=':  {Usage: 0x2e},
	'+':  {Modifier: hidModifierLeftShift, Usage: 0x2e},
	'}':  {Modifier: hidModifierRightAlt, Usage: 0x2e},
	'$':  {Usage: 0x30},
	'*':  {Usage: 0x31},
	'%':  {Modifier: hidModifierLeftShift, Usage: 0x34},
	',':  {Usage: 0x10},
	'?':  {Modifier: hidModifierLeftShift, Usage: 0x10},
	';':  {Usage: 0x36},
	'.':  {Modifier: hidModifierLeftShift, Usage: 0x36},
	':':  {Usage: 0x37},
	'/':  {Modifier: hidModifierLeftShift, Usage: 0x37},
	'!':  {Usage: 0x38},
	'<':  {Usage: 0x64},
	'>':  {Modifier: hidModifierLeftShift, Usage: 0x64},
}

var qwertzSymbolKeyStrokes = map[byte]KeyStroke{
	'!':  {Modifier: hidModifierLeftShift, Usage: 0x1e},
	'"':  {Modifier: hidModifierLeftShift, Usage: 0x1f},
	'$':  {Modifier: hidModifierLeftShift, Usage: 0x21},
	'%':  {Modifier: hidModifierLeftShift, Usage: 0x22},
	'&':  {Modifier: hidModifierLeftShift, Usage: 0x23},
	'/':  {Modifier: hidModifierLeftShift, Usage: 0x24},
	'{':  {Modifier: hidModifierRightAlt, Usage: 0x24},
	'(':  {Modifier: hidModifierLeftShift, Usage: 0x25},
	'[':  {Modifier: hidModifierRightAlt, Usage: 0x25},
	')':  {Modifier: hidModifierLeftShift, Usage: 0x26},
	']':  {Modifier: hidModifierRightAlt, Usage: 0x26},
	'=':  {Modifier: hidModifierLeftShift, Usage: 0x27},
	'}':  {Modifier: hidModifierRightAlt, Usage: 0x27},
	'?':  {Modifier: hidModifierLeftShift, Usage: 0x2d},
	'\\': {Modifier: hidModifierRightAlt, Usage: 0x2d},
	'+':  {Usage: 0x30},
	'*':  {Modifier: hidModifierLeftShift, Usage: 0x30},
	'~':  {Modifier: hidModifierRightAlt, Usage: 0x30},
	'#':  {Usage: 0x31},
	'\'': {Modifier: hidModifierLeftShift, Usage: 0x31},
	',':  {Usage: 0x36},
	';':  {Modifier: hidModifierLeftShift, Usage: 0x36},
	'.':  {Usage: 0x37},
	':':  {Modifier: hidModifierLeftShift, Usage: 0x37},
	'-':  {Usage: 0x38},
	'_':  {Modifier: hidModifierLeftShift, Usage: 0x38},
	'<':  {Usage: 0x64},
	'>':  {Modifier: hidModifierLeftShift, Usage: 0x64},
	'|':  {Modifier: hidModifierRightAlt, Usage: 0x64},
	'@':  {Modifier: hidModifierRightAlt, Usage: 0x14},
}

// KeyboardLayoutKeyStroke maps an ASCII character to its key stroke on layout,
// falling back to QWERTY for an unknown layout.
func KeyboardLayoutKeyStroke(layout string, ch byte) (KeyStroke, bool) {
	normalized, _ := NormalizeKeyboardLayout(layout)
	switch normalized {
	case KeyboardLayoutAZERTY:
		return azertyKeyStroke(ch)
	case KeyboardLayoutQWERTZ:
		return qwertzKeyStroke(ch)
	default:
		return qwertyKeyStroke(ch)
	}
}

// NormalizeKeyboardLayout returns the canonical layout value and whether value
// names a supported layout. An empty value selects the default layout.
func NormalizeKeyboardLayout(value string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return DefaultKeyboardLayout, true
	}
	for _, layout := range KeyboardLayoutDefinitions {
		if normalized == layout.Value {
			return layout.Value, true
		}
	}
	return DefaultKeyboardLayout, false
}

func KeyboardLayoutValuesText() string {
	values := make([]string, 0, len(KeyboardLayoutDefinitions))
	for _, layout := range KeyboardLayoutDefinitions {
		values = append(values, layout.Value)
	}
	return strings.Join(values[:len(values)-1], ", ") + ", or " + values[len(values)-1]
}

func keyboardLayoutTapKeyStroke(layout, key string) (KeyStroke, bool) {
	if len(key) == 1 {
		return KeyboardLayoutKeyStroke(layout, key[0])
	}
	if ch, ok := keyboardTapTextKeyMap[key]; ok {
		return KeyboardLayoutKeyStroke(layout, ch)
	}
	usage, ok := hidKeyboardMap[key]
	return KeyStroke{Usage: usage}, ok
}

func qwertyKeyStroke(ch byte) (KeyStroke, bool) {
	switch {
	case ch >= 'a' && ch <= 'z':
		return KeyStroke{Usage: 0x04 + (ch - 'a')}, true
	case ch >= 'A' && ch <= 'Z':
		return KeyStroke{Modifier: hidModifierLeftShift, Usage: 0x04 + (ch - 'A')}, true
	case ch >= '1' && ch <= '9':
		return KeyStroke{Usage: 0x1e + (ch - '1')}, true
	case ch == '0':
		return KeyStroke{Usage: 0x27}, true
	}

	stroke, ok := mapKeyboardSymbol(ch, qwertySymbolKeyStrokes)
	return stroke, ok
}

func azertyKeyStroke(ch byte) (KeyStroke, bool) {
	if ch >= 'A' && ch <= 'Z' {
		stroke, ok := azertyKeyStroke(ch + ('a' - 'A'))
		stroke.Modifier |= hidModifierLeftShift
		return stroke, ok
	}
	if ch >= 'a' && ch <= 'z' {
		if usage, ok := azertyLetterUsageOverrides[ch]; ok {
			return KeyStroke{Usage: usage}, true
		}
		return qwertyKeyStroke(ch)
	}
	if ch >= '1' && ch <= '9' {
		return KeyStroke{Modifier: hidModifierLeftShift, Usage: 0x1e + (ch - '1')}, true
	}
	if ch == '0' {
		return KeyStroke{Modifier: hidModifierLeftShift, Usage: 0x27}, true
	}

	return mapKeyboardSymbol(ch, azertySymbolKeyStrokes)
}

func qwertzKeyStroke(ch byte) (KeyStroke, bool) {
	if ch >= 'A' && ch <= 'Z' {
		stroke, ok := qwertzKeyStroke(ch + ('a' - 'A'))
		stroke.Modifier |= hidModifierLeftShift
		return stroke, ok
	}
	if ch == 'y' {
		return KeyStroke{Usage: 0x1d}, true
	}
	if ch == 'z' {
		return KeyStroke{Usage: 0x1c}, true
	}
	if (ch >= 'a' && ch <= 'x') || (ch >= '0' && ch <= '9') || ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' {
		return qwertyKeyStroke(ch)
	}

	return mapKeyboardSymbol(ch, qwertzSymbolKeyStrokes)
}

func mapKeyboardSymbol(ch byte, symbols map[byte]KeyStroke) (KeyStroke, bool) {
	stroke, ok := symbols[ch]
	return stroke, ok
}

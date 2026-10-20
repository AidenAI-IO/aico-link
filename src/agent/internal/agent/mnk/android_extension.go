package mnk

// Absolute-mode hid.usb2 uses a packed bitfield report, not Consumer Control
// usage LE. Only this subset is wired on the absolute gadget profile.
var absolutePointerModeExtensionReports = map[string]uint16{
	"volume_mute":               1 << 0,
	"volumeup":                  1 << 1,
	"volume_up":                 1 << 1,
	"volumedown":                1 << 2,
	"volume_down":               1 << 2,
	"media_play_pause":          1 << 3,
	"media_stop":                1 << 4,
	"media_next":                1 << 5,
	"media_previous":            1 << 6,
	"media_rewind":              1 << 7,
	"media_fast_forward":        1 << 8,
	"screenshot":                1 << 9,
	"key_usage_screenshot":      1 << 9,
	"brightness_up":             1 << 10,
	"key_usage_brightness_up":   1 << 10,
	"brightness_down":           1 << 11,
	"key_usage_brightness_down": 1 << 11,
}

// AbsolutePointerModeExtensionKeyList names the Android keys that the
// absolute pointer-mode hid.usb2 report carries.
const AbsolutePointerModeExtensionKeyList = "KEYCODE_VOLUME_MUTE, KEYCODE_VOLUME_UP, KEYCODE_VOLUME_DOWN, KEYCODE_MEDIA_PLAY_PAUSE, KEYCODE_MEDIA_STOP, KEYCODE_MEDIA_NEXT, KEYCODE_MEDIA_PREVIOUS, KEYCODE_MEDIA_REWIND, KEYCODE_MEDIA_FAST_FORWARD, KEYCODE_SCREENSHOT, KEYCODE_BRIGHTNESS_UP, KEYCODE_BRIGHTNESS_DOWN"

type androidKeyboardTapAlias struct {
	Keycode           int
	Replacement       string
	UnsupportedReason string
}

// Android KEYCODE_* aliases map onto Consumer Control extension keys (or
// explicit unsupported reasons). Kept separate from the usage table.
var androidKeyboardTapAliases = map[string]androidKeyboardTapAlias{
	"keycode_call": {
		Keycode:           5,
		UnsupportedReason: "call pickup requires an Android telephony/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_endcall": {
		Keycode:           6,
		UnsupportedReason: "call hangup requires an Android telephony/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_home": {
		Keycode:     3,
		Replacement: "android_home",
	},
	"keycode_menu": {
		Keycode:     82,
		Replacement: "menu",
	},
	"keycode_back": {
		Keycode:     4,
		Replacement: "android_back",
	},
	"keycode_search": {
		Keycode:     84,
		Replacement: "search",
	},
	"keycode_camera": {
		Keycode:           27,
		UnsupportedReason: "camera shutter requires a camera/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_focus": {
		Keycode:           80,
		UnsupportedReason: "camera focus requires a camera/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_power": {
		Keycode:     26,
		Replacement: "power",
	},
	"keycode_sleep": {
		Keycode:     223,
		Replacement: "sleep",
	},
	"keycode_wakeup": {
		Keycode:           224,
		UnsupportedReason: "wakeup requires a Generic Desktop/System Control HID path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_soft_sleep": {
		Keycode:           276,
		UnsupportedReason: "soft sleep has no verified standard Consumer Control usage on this gadget",
	},
	"keycode_notification": {
		Keycode:           83,
		UnsupportedReason: "notification center has no verified standard Consumer Control usage on this gadget; use quick_action notification_center or touch_gesture instead",
	},
	"keycode_mute": {
		Keycode:           91,
		UnsupportedReason: "KEYCODE_MUTE is microphone mute, not speaker mute; hid.usb2 only exposes system speaker/stream volume mute",
	},
	"keycode_volume_mute": {
		Keycode:     164,
		Replacement: "volume_mute",
	},
	"keycode_volume_up": {
		Keycode:     24,
		Replacement: "volume_up",
	},
	"keycode_volume_down": {
		Keycode:     25,
		Replacement: "volume_down",
	},
	"keycode_media_play_pause": {
		Keycode:     85,
		Replacement: "media_play_pause",
	},
	"keycode_media_stop": {
		Keycode:     86,
		Replacement: "media_stop",
	},
	"keycode_media_next": {
		Keycode:     87,
		Replacement: "media_next",
	},
	"keycode_media_previous": {
		Keycode:     88,
		Replacement: "media_previous",
	},
	"keycode_media_rewind": {
		Keycode:     89,
		Replacement: "media_rewind",
	},
	"keycode_media_fast_forward": {
		Keycode:     90,
		Replacement: "media_fast_forward",
	},
	"keycode_app_switch": {
		Keycode:     187,
		Replacement: "app_switch",
	},
	"keycode_window": {
		Keycode:     171,
		Replacement: "window",
	},
	"keycode_settings": {
		Keycode:     176,
		Replacement: "settings",
	},
	"keycode_language_switch": {
		Keycode:     204,
		Replacement: "language_switch",
	},
	"keycode_brightness_down": {
		Keycode:     220,
		Replacement: "brightness_down",
	},
	"keycode_brightness_up": {
		Keycode:     221,
		Replacement: "brightness_up",
	},
	"keycode_media_audio_track": {
		Keycode:     222,
		Replacement: "media_audio_track",
	},
	"keycode_refresh": {
		Keycode:     285,
		Replacement: "refresh",
	},
	"keycode_profile_switch": {
		Keycode:     288,
		Replacement: "profile_switch",
	},
	"keycode_emoji_picker": {
		Keycode:     317,
		Replacement: "emoji_picker",
	},
	"keycode_screenshot": {
		Keycode:     318,
		Replacement: "screenshot",
	},
	"keycode_dictate": {
		Keycode:     319,
		Replacement: "dictate",
	},
	"keycode_new": {
		Keycode:     320,
		Replacement: "new",
	},
	"keycode_close": {
		Keycode:     321,
		Replacement: "close",
	},
	"keycode_print": {
		Keycode:     323,
		Replacement: "print",
	},
	"keycode_fullscreen": {
		Keycode:     325,
		Replacement: "fullscreen",
	},
}

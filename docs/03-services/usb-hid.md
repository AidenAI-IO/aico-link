---
sidebar_position: 7
---

# USB HID and Device Control

The project configures the Luckfox Pico Zero's USB-C port through the Linux USB
gadget framework. `aico-usb-gadget.service` creates a composite gadget with
keyboard HID, pointer/touch HID, Consumer Control, and CDC ECM networking; the
HID nodes are used to simulate keyboard, mouse, and touch input.

## Startup Method

The Debian firmware owns gadget startup through systemd:

```bash
systemctl status aico-usb-gadget.service --no-pager
systemctl restart aico-usb-gadget.service
```

The unit executes the Debian rootfs helper directly:

```bash
/usr/lib/aico/aico-usb-gadget start
```

It will:

1. Mount configfs: `/sys/kernel/config`;
2. Load the `dwc2` module;
3. Load the `libcomposite` module;
4. Create and bind the composite gadget.

`aico-usb-gadget.service` is the authoritative production startup path. It also
brings up `usb0` at `192.168.42.1` for the config page and local USB networking.
The completed composite is bound to the UDC exactly once. For iOS it is
Consumer Control + touchpad + ECM, without the keyboard (see
[iOS touchpad and on-demand keyboard](#ios-touchpad-and-on-demand-keyboard));
other device types also link the keyboard. Do not add a same-identity startup
unbind/rebind: while the cable remains attached, iOS can retain the physical USB
session but rebuild the HID interfaces with an inconsistent state. A physical
unplug/replug fully tears down that host session.

Restarting the unit does drop the configfs group and recreate it. The vendor
kernel mishandled that path: `CONFIG_USB_CONFIGFS_UEVENT` kept a global
`android_device` that was never cleared on teardown, so the next control
transfer dereferenced a freed `struct device`. The production kernel carries
the fix in `pico-sdk/sysdrv/source/kernel/drivers/usb/gadget/configfs.c`. It
derives gadget state from the gadget, cancels pending work before destroying
the device, and clears the global device pointer on teardown. The historical
[SDK commit](https://github.com/AidenAI-IO/aiden-sdk/commit/abe2bc1fa59691c81418c3cba64eaf703f55870c)
records the three defects under `usb: gadget: configfs: fix use-after-free on
gadget teardown`. Read that explanation before touching the teardown sequence.

## example_usb_hid Usage

```text
example_usb_hid [global options] setup <keyboard|touch|composite>
example_usb_hid [global options] cleanup
example_usb_hid [global options] keyboard press <KEY> [KEY ...]
example_usb_hid [global options] keyboard release [KEY ...]
example_usb_hid [global options] keyboard tap <KEY> [KEY ...]
example_usb_hid [global options] keyboard text <TEXT>
example_usb_hid [global options] touch move <X> <Y>
example_usb_hid [global options] touch click <X> <Y> [left|right|middle]
example_usb_hid [global options] touch click [left|right|middle]
example_usb_hid [global options] touch down [left|right|middle]
example_usb_hid [global options] touch up
example_usb_hid [global options] touch scroll <amount>
example_usb_hid [global options] server [PORT]
```

### Server Command

The `server` command starts a simple HTTP server that accepts HID commands via POST requests. This is useful for remote control or integration testing.

```bash
sudo example_usb_hid server 8090
```

The server listens on the specified port (default 8080 if not specified) and accepts JSON payloads with command specifications.

Common examples:

```bash
sudo example_usb_hid keyboard tap ENTER
sudo example_usb_hid keyboard tap CTRL ALT DELETE
sudo example_usb_hid keyboard text "hello from pico"
sudo example_usb_hid touch click 16000 16000
sudo example_usb_hid cleanup
```

## Global Parameters

| Parameter                           | Description                 |
| ----------------------------------- | --------------------------- |
| `--gadget-root PATH`                | configfs gadget root path   |
| `--gadget-name NAME`                | gadget name                 |
| `--keyboard-dev PATH`               | keyboard HID device         |
| `--touch-dev PATH`                  | touch/mouse HID device      |
| `--state-dir PATH`                  | state file directory        |
| `--manufacturer TEXT`               | USB manufacturer string     |
| `--product-name TEXT`               | USB product string          |
| `--serial TEXT`                     | USB serial string           |
| `--vendor INT` / `--product-id INT` | USB VID/PID                 |
| `--udc NAME`                        | specify UDC                 |
| `--width INT` / `--height INT`      | coordinate space dimensions |
| `--duration-ms INT`                 | input action duration       |
| `--force`                           | force certain operations    |

## Agent HID Configuration

```toml
[basic_settings.device]
device_type = "iOS"

[advanced_settings.hardware.hid]
keyboard_device = "/dev/hidg0"
keyboard_layout = "qwerty"
mouse_device = "/dev/hidg1"
android_keyboard_device = "/dev/hidg2"
frame_socket = "/run/frame_service/frame_service.sock"
```

`[basic_settings.device].device_type` selects the `hid.usb1` pointer:

| `device_type` | `pointer_mode` | `hid.usb1` |
| --- | --- | --- |
| `iOS` | `touchpad` | Digitizer touchpad, used by iOS as direct touch |
| `Android` | `touchscreen` | Digitizer touchscreen |
| `macOS`, `windows`, `linux` | `absolute` | Absolute mouse |

The firmware also binds `hid.usb2` as `/dev/hidg2`. This second keyboard-like
interface advertises Consumer Control usages. With `pointer_mode =
"touchscreen"` it is used for Android extension keys such as Back, Home, App
Switch, Search, Power, and Volume. The `touchpad` and `absolute` modes expose a
smaller bitmap media-key interface that advertises only volume mute/up/down,
media playback controls, screenshot, and brightness up/down.

Built-in Agent tools:

- `keyboard_tap`
- `keyboard_text`
- `mouse_move` (absolute mouse only)
- `mouse_scroll` (absolute mouse only; direct touch scrolls with a `touch_gesture` swipe)
- `touch_gesture`

`touch_gesture` uses standard `type` gestures for normal interaction. It also
supports a low-frequency atomic `actions` program for custom contact-sensitive
input that the standard gestures cannot express. The program uses normalized `0..1000` points and the actions
`touch_down`, `move_to`, `wait`, and `touch_up`; it executes in one HID pointer
session and must release every contact before returning. Example:

```json
{"actions":[{"action":"touch_down","point":{"x":500,"y":700}},{"action":"wait","ms":80},{"action":"move_to","point":{"x":500,"y":300},"speed":2500},{"action":"touch_up"}]}
```

`move_to.speed` is optional and uses normalized coordinate units per second.
When both `speed` and `duration_ms` are present, `duration_ms` takes precedence;
omitting both preserves the existing immediate-move behavior.
While contact is held, timed atomic moves use the same quintic acceleration
and braking curve as HID swipe main motion. For a known interior contact,
a timed move immediately followed by release also reserves the last two
normalized units (at most half the segment) for 100ms of real low-speed
movement, with a 180ms minimum main motion, matching the standard HID swipe.
This adds release time to the requested main-motion duration; it is not a
stationary endpoint hold. Edge-origin gestures, explicit end waits, immediate
moves and releases that specify a different endpoint preserve their timing.
The main curve also covers timed movements in predefined quick actions.
The profile is shared in `mnk/motion_profile.go`; new touch tools should call
`SwipeWithOptions` or `TouchActions` rather than implement their own curve.

A `touch_gesture` or `mouse_move` is not sent when the screen turned between
portrait and landscape after the screenshot the model chose its coordinates on;
the tool returns the current screenshot so the model can choose the target
again.

The Agent's ADB input backend executes standard swipes as one continuous
DOWN/MOVE/UP program using `sendevent`, with `input motionevent` as a fallback.
Both paths use the same curve and eligible release tail for timed moves.
For a standard multi-point swipe, the 180ms minimum applies once to the entire
eligible main path, followed by one 100ms release tail. Intermediate segments
remain linear; only the final main segment uses the braking curve, matching HID.
`steps` controls interpolation, and optional holds occur while contact is down.
ADB command and injection overhead can extend wall-clock duration.
These changes do not affect mouse-wheel events, ADB `mouse_scroll` approximation,
the dedicated drag interfaces, or the separate benchmark bridge backends.
Atomic programs are limited to 128 actions, 30 seconds per declared action
duration, and 60 seconds for the sum of declared durations. HID and ADB check
both the submitted program and the expanded motion profile, including its
release tails, before sending any input.

Supported one-object `type` forms are the default for normal quick actions and
scripts; `type:"drag"` is not supported.

Moving a draggable target never uses atomic `actions`. It uses two
`touch_gesture` calls so the Agent can
observe the drag state before choosing the final destination:

```json
{"type":"drag_start","point":{"x":400,"y":500}}
{"type":"drag_release","point":{"x":750,"y":500}}
```

`drag_start` presses the current target for 500ms, automatically moves exactly
200 normalized units at 500 normalized units per second (a 400ms interpolated
move) along the axis with the most available screen space, and keeps the
contact down during its internal screen-stability wait. When the wait succeeds,
contact remains down through final screenshot capture. The Agent confirms the
destination from the returned screenshot only when the result reports
`screen_stable=true`, then calls
`drag_release`. When the result reports `screen_stable=false`, runtime moves
back to the original `drag_start` point and releases the contact; the Agent
inspects the returned screenshot and retries `drag_start` instead of calling
`drag_release`. It does not call `wait_for_stable_screen` separately in the
normal drag flow or choose a destination from a `screen_stable=false` result.
The release call moves directly to the confirmed point, holds for 200ms, and
releases. The former one-call `type:"drag"` gesture is no longer supported.

It is recommended to use normalized coordinates (`0..1000`, with center at `500,500`) to avoid click position shifts due to display resolution changes.
For dense targets such as small buttons, list items, and input boxes, prioritize estimating the normalized coordinates of the target center. After successful input tool execution, a post-action screenshot is returned; screen changes should be confirmed before proceeding to avoid duplicate clicks.
`keyboard_layout` must match how the phone interprets the external USB HID keyboard. Supported values are `qwerty` (default), `azerty`, and `qwertz`. The visible soft-keyboard layout is not authoritative: a phone can display an AZERTY soft keyboard while still interpreting Aico's USB HID reports as QWERTY. Both `keyboard_text` and standard text-like keys in `keyboard_tap` use this mapping. The mapping itself is loaded by the Agent and does not change USB descriptors, but Config Web requires a board restart after saving so the host starts a clean USB session.

Select the matching layout under `[advanced_settings.hardware.hid]` in Config Web (`qwerty`, `azerty`, or `qwertz`). Most users keep the default `qwerty`; only change it if typed characters come out transposed (for example "shape" becomes "shqpe").

A phone running iOS locks its hardware-keyboard layout at the moment the USB keyboard is enumerated, based on the software keyboard that is active at that instant. Switching the on-screen keyboard afterwards does not change the locked interpretation. To align a non-QWERTY layout:

1. On the phone, switch the input language to the matching one (for example French for `azerty`, German for `qwertz`).
2. Set `keyboard_layout` in Config Web to the target layout and save.
3. Accept the Config Web reboot prompt and wait for the board and USB composite to reconnect. The phone then locks the hardware layout based on the language active in step 1.

The order matters: switch the language _before_ saving the configuration, because the layout is locked during enumeration. The Config Web save flow does not hot-reload or soft re-enumerate the same USB identity: iOS can retain an inconsistent keyboard and pointer session across that transition. If immediate reboot is cancelled, reboot the board manually before verifying the new layout.

`keyboard_text` can only input ASCII typeable characters. For Chinese or other non-ASCII text, use `enter_text` (which leverages the phone's on-screen keyboard and IME candidates) instead of transliterating to pinyin or romanized approximations. The configured layouts cover common ASCII keys, but country-specific punctuation variants may still require device verification.

## iOS touchpad and on-demand keyboard

### iPhone settings

```text
Settings > Accessibility > Zoom                      On
Settings > Accessibility > Touch > AssistiveTouch    Off
```

With Zoom on, iOS accepts the board's digitizer touchpad (`05 0d 09 05`,
report id 5, two contacts, 1920x1080 logical range) as direct touch: a contact
lands where it is sent, no pointer is drawn, and there is no hover. With Zoom
off, or AssistiveTouch on, iOS ignores every touch while keyboard input keeps
working. A digitizer touchscreen descriptor and the absolute mouse are not
accepted this way.

The touchpad's logical range covers the phone screen itself, not the HDMI
frame, so normalized `0..1000` coordinates map onto it directly and black bars
around the mirrored picture do not matter. Each report is 12 bytes:
`[0x05, scan_lo, scan_hi, buttons, finger1(4), finger2(4)]`, where a finger is
`tip | confidence<<1 | contact_id<<2` followed by X (12 bits) and Y (12 bits)
packed little endian. Only finger 1 is used.

### On-demand keyboard

iOS hides its software keyboard while a hardware keyboard is attached, so the
iOS composite idles without one:

| Profile | Interfaces | PID |
| --- | --- | --- |
| detached (boot) | Consumer Control (`hid.usb2`) -> touchpad (`hid.usb1`) -> ECM | `0x0180` |
| attached | keyboard (`hid.usb0`) -> Consumer Control -> touchpad -> ECM | `0x0181` |

Every HID stays before ECM: a HID after the ECM IAD makes iOS reset the bus.
Each profile has its own product ID and serial number so iOS never reuses
descriptors cached for the other.

One conversational Agent run owns the keyboard scope. Its first keyboard action
(`keyboard_tap`, `keyboard_text`, `enter_text`, `quick_action` shortcuts, the
`open_app` search query) attaches the keyboard; later keyboard, touch, and
media-key actions in the run reuse that profile without another enumeration.
The run detaches the keyboard when it ends: success, failure, cancellation,
preemption, and panic cleanup all leave the scope through a
context-independent final detach. A run that never types never re-enumerates.
A failed attach detaches immediately, and a detach that failed is retried
(after a 5-second cooldown) before the next HID input. Direct HTTP Tool API
calls use the same rules within one invocation. Each switch takes about two
seconds and is logged as `keyboard attach|detach ok=... ms=...`.

While the keyboard is attached, the software keyboard is hidden; that is
expected, and the Agent prompt says so. Modifier shortcuts (`Cmd+A`, `Cmd+V`,
`Cmd+Space`, `Shift`) work next to the touchpad, so no modifier isolation is
needed. Shortcuts that edit text need the text committed: with a Chinese IME
the letters can still be in pinyin composition, where `Cmd+A` selects nothing.

When `search_launch_app` omits `platform`, an active iOS keyboard controller is
also treated as an iOS platform signal. This keeps the documented
`{"app":"WeChat"}` input on the `Cmd+Space` Spotlight path even while Phone
Bridge is unavailable or backgrounded.

The Luckfox board has one UDC, so attach and detach briefly disconnect the
complete composite, including ECM. The dynamic controller and ECM watchdog
share `/run/aico_dynamic_keyboard.lock` to prevent overlapping UDC resets. The
controller also publishes a short post-switch grace deadline so the watchdog
does not count the planned ECM interruption toward its stall threshold. HID
node open recovery also honors this deadline: a transient `/dev/hidg*`
`ENXIO` waits and retries instead of forcing another composite refresh. Before
each switch the board sends a `usb_reenumeration` BLE notice so the companion
app ignores the ECM gap.

Inspect or exercise the profiles manually with:

```bash
/usr/lib/aico/aico-dynamic-keyboard status
/usr/lib/aico/aico-dynamic-keyboard attach
/usr/lib/aico/aico-dynamic-keyboard detach
```

After any test, `status` should report `mode=detached` and list `hid.usb1`,
`hid.usb2`, and `ecm.usb0`. The `/dev/hidgN` numbers follow the linked
functions, so look them up in configfs (`functions/hid.usbN/dev`) instead of
assuming `/dev/hidg1` is the touchpad. Use HDMI visual feedback to verify an
effect; a successful write only proves that the gadget accepted the report.

### iOS touchpad in landscape

The touchpad reports in the phone panel's own coordinates, like a finger on
the glass, while the HDMI picture is always upright. In landscape a point chosen
on the screenshot therefore lands turned by a quarter, in a direction set by
which way the phone is turned:

| Phone | Sent `(x, y)` lands at |
| --- | --- |
| Portrait | `(x, y)` |
| Landscape, `turned_left` | `(1000-y, x)` |
| Landscape, `turned_right` | `(y, 1000-x)` |

Unlike the AssistiveTouch pointer, the touchpad does not clamp at the portrait
width, and re-enumerating does not change the mapping. The provider sends every
point through the inverse of the current turn, so taps, swipes, drags, and
`tap_sequence` all land where they were chosen.

The screenshot cannot tell the two landscape directions apart, so a landscape
screen starts from `turned_right`, the direction measured with an app forcing
landscape on an upright iPhone, and the direction stays unconfirmed until the
model sees where a touch reacted:

- While it is unconfirmed, `touch_gesture` sends only `tap`, `double_tap`,
  `long_press`, and `tap_sequence` that carry `"mirror_ok": true`, meaning the
  mirror `(1000-x, 1000-y)` of every touched point is also harmless. Other
  gestures would run mirrored and are refused. The screen centre is its own
  mirror.
- A `mirror_ok` touch on a landscape screen, confirmed or not, is a direction
  check. Its screenshot is taken about 300 ms after the touch, without the
  pre-action baseline, stable-screen wait, or UI grounding, because a reaction
  such as video controls can fade within seconds. It marks the aimed point
  (the last tap of a `tap_sequence`) with a red ring and its mirror with a
  blue ring.
- A check `tap_sequence` skips its layout guard and, unless `interval_ms` is
  set, waits 1000 ms between taps. That lets one call reveal controls that hide
  again (tap the centre) and press one of them: with 400 ms the second tap
  landed during the controls' appearance animation and was lost.
- The model then calls `{"type":"orientation_set","landed":"expected"}` when
  the touch reacted at the red ring, or `"landed":"mirror"` when it reacted at
  the blue ring, which flips the direction; it then redoes the touch. Because
  a check also works on a confirmed direction, this covers turning the phone
  directly from one landscape direction to the other.
- The Coordinate Debug page sends its taps as `mirror_ok`, so in landscape it
  uses the current direction (the default until confirmed) instead of being
  refused; a tap that reacts at the mirror shows which way to adjust.

The direction is dropped when the screenshot is portrait again. A screenshot
cleared by a capture invalidation keeps the direction in use.
`{"type":"orientation_probe","point":...}` remains as an optional check: it
presses a control that highlights while pressed and compares the frame at the
point and at its mirror while the contact is held.

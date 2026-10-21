---
sidebar_position: 7
---

# USB HID and Device Control

The project configures the Luckfox Pico Zero's USB-C port through the Linux USB
gadget framework. `aiden-usb-gadget.service` creates a composite gadget with
keyboard HID, pointer/touch HID, Consumer Control, and CDC ECM networking; the
HID nodes are used to simulate keyboard, mouse, and touch input.

## Startup Method

The Debian firmware owns gadget startup through systemd:

```bash
systemctl status aiden-usb-gadget.service --no-pager
systemctl restart aiden-usb-gadget.service
```

The unit executes the Debian rootfs helper directly:

```bash
/usr/lib/aiden/aiden-usb-gadget start
```

It will:

1. Mount configfs: `/sys/kernel/config`;
2. Load the `dwc2` module;
3. Load the `libcomposite` module;
4. Create and bind the composite gadget.

`aiden-usb-gadget.service` is the authoritative production startup path. It also
brings up `usb0` at `192.168.42.1` for the config page and local USB networking.
The completed keyboard + pointer + Consumer Control + ECM composite is bound to
the UDC exactly once. Do not add a same-identity startup unbind/rebind: while the
cable remains attached, iOS can retain the physical USB session but rebuild the
HID interfaces with inconsistent external-keyboard and AssistiveTouch pointer
state, leaving the cursor operational while the on-screen keyboard stays
suppressed. A physical unplug/replug fully tears down that host session, which
is why it can recover the symptom.

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

The firmware also binds `hid.usb2` as `/dev/hidg2`. This second keyboard-like
interface advertises Consumer Control usages. When `[basic_settings.device].device_type =
"Android"` derives `pointer_mode = "touchscreen"`, it is used for Android
extension keys such as Back, Home, App Switch, Search, Power, and Volume. Other
device types derive `pointer_mode = "absolute"` and expose a smaller bitmap
media-key interface that advertises only volume mute/up/down, media playback
controls, screenshot, and brightness up/down.

Built-in Agent tools:

- `keyboard_tap`
- `keyboard_text`
- `mouse_move`
- `mouse_scroll`
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
`keyboard_layout` must match how the phone interprets the external USB HID keyboard. Supported values are `qwerty` (default), `azerty`, and `qwertz`. The visible soft-keyboard layout is not authoritative: a phone can display an AZERTY soft keyboard while still interpreting Aiden's USB HID reports as QWERTY. Both `keyboard_text` and standard text-like keys in `keyboard_tap` use this mapping. The mapping itself is loaded by the Agent and does not change USB descriptors, but Config Web requires a board restart after saving so the host starts a clean USB session.

Select the matching layout under `[advanced_settings.hardware.hid]` in Config Web (`qwerty`, `azerty`, or `qwertz`). Most users keep the default `qwerty`; only change it if typed characters come out transposed (for example "shape" becomes "shqpe").

A phone running iOS locks its hardware-keyboard layout at the moment the USB keyboard is enumerated, based on the software keyboard that is active at that instant. Switching the on-screen keyboard afterwards does not change the locked interpretation. To align a non-QWERTY layout:

1. On the phone, switch the input language to the matching one (for example French for `azerty`, German for `qwertz`).
2. Set `keyboard_layout` in Config Web to the target layout and save.
3. Accept the Config Web reboot prompt and wait for the board and USB composite to reconnect. The phone then locks the hardware layout based on the language active in step 1.

The order matters: switch the language _before_ saving the configuration, because the layout is locked during enumeration. The Config Web save flow does not hot-reload or soft re-enumerate the same USB identity: iOS can retain an inconsistent keyboard and pointer session across that transition. If immediate reboot is cancelled, reboot the board manually before verifying the new layout.

`keyboard_text` can only input ASCII typeable characters. For Chinese or other non-ASCII text, use `enter_text` (which leverages the phone's on-screen keyboard and IME candidates) instead of transliterating to pinyin or romanized approximations. The configured layouts cover common ASCII keys, but country-specific punctuation variants may still require device verification.

## iOS AssistiveTouch modifier isolation

On iOS, AssistiveTouch can make modifier routing unstable when an external
keyboard and pointer are advertised by the same USB composite. Plain key input
may work while shortcuts such as `Cmd+A` or `Cmd+V` are ignored.

When `[basic_settings.device].device_type` derives `pointer_mode = "absolute"`, firmware builds that include
`/usr/lib/aiden/aiden-dynamic-keyboard` automatically isolate keyboard actions
whose HID reports contain Ctrl, Shift, Option/Alt, or Cmd/Meta. This includes
`keyboard_text` values containing uppercase letters or symbols that require
Shift or AltGr on the configured keyboard layout. Plain key taps, unmodified text, pointer input, and
Consumer Control continue to use the normal keyboard + pointer + Consumer
Control + ECM composite.

Immediately before a modifier-bearing keyboard action, the Agent switches the
single USB gadget to a pointer-free keyboard + Consumer Control + ECM profile.
After the action, including error and cancellation paths, it restores the normal
composite. The isolated profile uses a distinct USB product ID and serial number
so iOS does not reuse the pointer-bearing descriptor for the input.

The normal composite must enumerate the pointer as the last HID interface:
`keyboard -> Consumer Control -> pointer -> ECM`. iOS builds its keyboard and
AssistiveTouch pointer subsystem state in interface order; a pointer that is
enumerated immediately after the keyboard leaves the on-screen keyboard policy
subject to an async race, so after an isolate/restore cycle the soft keyboard
only reappears about 80% of the time. With the Consumer Control interface between
them, the keyboard subsystem settles first and the pointer registers last, which
closes that race: field tests showed 10/10 successful soft-keyboard restores.
The pointer must stay before ECM: putting a HID interface after the ECM IAD
causes continuous USB reset loops on iOS.

One conversational Agent run owns a shared isolation scope. The first
modifier-bearing keyboard operation removes the pointer, and later keyboard or
Consumer Control operations reuse that profile without another enumeration. A
mouse, touch, scroll, or wheel operation restores the normal profile only when
the pointer is currently absent. If a later keyboard operation needs a modifier,
the run isolates again. Success, failure, cancellation, preemption, panic
cleanup, and normal termination all leave the scope through a context-independent
final restore.

Direct HTTP Tool API calls use the same rules within one invocation, but do not
share isolation state across separate requests. A modifier-bearing HTTP call
therefore isolates and restores once unless that invocation needs pointer input
before it completes.

When `search_launch_app` omits `platform`, an active iOS HID isolation
controller is also treated as an iOS platform signal. This keeps the documented
`{"app":"WeChat"}` input on the `Cmd+Space` Spotlight path even while Phone
Bridge is unavailable or backgrounded.

The Luckfox board has one UDC, so isolation and restore briefly disconnect the
complete composite, including ECM. The dynamic controller and ECM watchdog
share `/run/aiden_dynamic_keyboard.lock` to prevent overlapping UDC resets. The
controller also publishes a short post-switch grace deadline so the watchdog
does not count the planned ECM interruption toward its stall threshold. Normal
watchdog probes resume after the grace window and still recover persistent ECM
failures. HID node open recovery also honors this deadline: a transient
`/dev/hidg*` `ENXIO` waits and retries instead of forcing another composite
refresh during the planned switch.

Inspect or exercise the profiles manually with:

```bash
/usr/lib/aiden/aiden-dynamic-keyboard status
/usr/lib/aiden/aiden-dynamic-keyboard isolate
/usr/lib/aiden/aiden-dynamic-keyboard restore
```

After any test, `status` should report `mode=normal` and list `hid.usb0`,
`hid.usb1`, `hid.usb2`, and `ecm.usb0`. Use HDMI visual feedback to verify the
shortcut effect; a successful `/dev/hidg0` write only proves that the gadget
accepted the HID report.

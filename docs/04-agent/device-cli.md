---
sidebar_position: 4
---

# Device CLI

The backend agent operates the target screen using `device-cli` in a foreground
`shell` call. The CLI forwards to the existing [Tools HTTP API](tools-http-api.md);
it does not implement another device driver. The backend agent no longer receives
the legacy direct UI tools. The HTTP API and the standalone realtime voice path
still expose their existing device tools. Set `AICO_LEGACY_DEVICE_TOOLS=1` before
starting the daemon to opt out of CLI mode.

```sh
device-cli tap 500 650
device-cli swipe 500 800 500 300
device-cli tap 500 650 && device-cli type 'Hello'
```

Coordinates are normalized to 0–1000 on both axes. A foreground shell can
run up to 64 device operations and attaches one final screenshot, without a
collage or action markers. When UI Grounding is enabled and the detector is
available, that screenshot carries numbered UI boxes and normalized coordinates.
Actions do not automatically wait for stability or attach screenshots individually.
Split calls when the next step depends on the current layout. If a chain fails
or times out, inspect the final screen before retrying:
earlier operations may already have run. The orientation check can still reject
an input before dispatch if the display has turned since the Agent saw it.

Focus the intended editable field before either text command. Neither accepts
focus coordinates or chooses a field for you.

- `device-cli type TEXT` uses device input (native insertion where available,
  otherwise HID/IME). It does not use the companion app.
- `device-cli app type TEXT` requires a usable Phone Bridge clipboard route. It
  writes or reuses the clipboard and pastes into the focused field. It never falls back to device typing. If paste may have
  reached the field but verification fails, inspect the screen before retrying
  or switching routes. On iOS without PiP, this command needs a recently
  prepared matching clipboard.

Try `device-cli app` when `app_connected`, `app_fgs_enabled`, or
`app_pip_enabled` is true. This permits an attempt; each operation still checks
its own route, permissions, and prerequisites.
Without an active shell scope, the executable accepts `--url` to target a
daemon directly (default `http://127.0.0.1:8080`).

`drag` runs both phases on the controller. A failed or canceled shell that
started but did not release a contact attempts to release it at its origin.
A successful `drag-start` intentionally leaves it held for a later
`drag-release`. Direct HTTP drag retains its existing verification behavior.

## Companion app data

These commands use the same connection, FGS, or PiP check as `app type` and
still require the permissions needed for the requested operation. They return
structured data and do not attach a screenshot.

```sh
device-cli app clipboard read
device-cli app clipboard write 'Hello'
device-cli app calendar query '{"from":"2026-11-23T00:00:00+08:00","to":"2026-11-24T00:00:00+08:00"}'
device-cli app contacts query '{"query":"Alice","limit":5}'
device-cli app notification send '{"title":"Reminder","body":"Check the calendar","sound":false}'
```

Calendar also supports `create` and `delete`; contacts also supports `create`
and `update`. Use the existing Bridge tool argument fields, omitting `action`
because the CLI verb supplies it. Notification `send` accepts `schedule_at`
for a scheduled send. Confirm event details before creating or deleting events.
The existing Bridge tools remain available according to their background-route
policies. Notification queries still use `bridge_notification` with
`action=query` to read the board's notification ring. Top-level `open` and `url`
keep their existing routing and UI fallbacks.

## Keyboard shortcuts

`device-cli key meta+a` and `device-cli key meta a` both press Meta and A
simultaneously. Multiple modifiers are supported, for example `key ctrl+shift+tab`.
For platform-specific actions such as Select All, prefer `device-cli quick select_all`.
An unknown key or malformed chord makes the CLI exit nonzero. In an Agent shell,
a failed `key` request also marks the tool result as failed, even if a pipe or a
later successful command masks the CLI exit status. Use `&&` without a masking
pipe when the next action must not run after a failure.

## Viewing and saving screenshots

```sh
device-cli wait 0                                      # Attach the current screen without waiting for stability
device-cli look                                        # Save a JPEG in /tmp
device-cli look --dir /tmp --region 100 200 900 800     # Save a cropped JPEG
```

`look` prints the absolute path to a new JPEG. The destination directory must
already exist and be writable; `/tmp` is the default and recommended choice.
The Agent can inspect the saved JPEG with `read_image` using the printed path.
`--region X1 Y1 X2 Y2` sets the top-left and bottom-right corners in normalized
integer coordinates (0–1000). `look` only writes a file; it does not replace the shell's final screenshot.
An explicit `wait` observation is reused when no device action follows it.

`wait N` with a positive `N` sets the stability polling timeout in milliseconds
(1–30000). Capturing the final frame can take additional time.

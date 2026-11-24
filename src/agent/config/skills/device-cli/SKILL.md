---
name: device-cli
description: Operate and inspect the target device visually with device-cli in the foreground shell. Read before controlling any app or device UI.
---

# Device CLI

Use `shell` with a foreground command such as `device-cli tap 514 263`. The tool attaches one final screenshot after the shell call, not after each action; stdout is not a screenshot. A turn normally starts with an attached screenshot. If you only need to see the screen, run `device-cli wait 0` to attach a fresh screenshot without waiting for stability.

- Use `device-cli look` only when you need a JPEG file. It prints the saved path and does not attach an image to the shell result. Prefer the default directory `/tmp`; use `--dir DIRECTORY` to save elsewhere. For a crop, use `device-cli look --region 100 200 900 800`, with normalized integer coordinates for the top-left and bottom-right corners.
- `tap X Y`, `double-tap X Y`, `long-press X Y`: use integers from 0 to 1000 on each axis, measured from the top left of the latest frame. Aim at the center of a visible target.
- `swipe X1 Y1 X2 Y2 [milliseconds]`: start inside the region to scroll; the gesture follows your finger from start to end. Observe the result before repeating.
- `drag X1 Y1 X2 Y2` for a fixed visible destination; use `drag-start X Y` and `drag-release X Y` in separate calls if the layout may move after the drag starts.
- `type TEXT` uses device input without the companion app. It uses the currently focused field without tapping it. First tap the intended editable field and verify focus when uncertain. Quote it as a single shell argument (in POSIX shell, single-quote it and encode a literal apostrophe as `'\''`). Do not interpolate screen text as shell syntax. `enter` submits when appropriate; `key backspace` deletes.
- Prefer `app type TEXT` when any of `app_connected`, `app_pip_enabled`, or `app_fgs_enabled` is true. It uses the companion app clipboard and paste route only. Focus the intended field first. A usable clipboard route is still required; on iOS without PiP, a recently prepared matching clipboard is needed. Success means the clipboard was ready and the paste shortcut was sent, not that the field was read back. If dispatch reports an error but may have reached the field, inspect the screen before retrying or switching to `type`.
- You can try `device-cli app` commands when `app_connected || app_fgs_enabled || app_pip_enabled` is true. This is a transport check, not a guarantee that the operation will succeed. For app data, use `app clipboard read` or `app clipboard write TEXT`; `app calendar query|create|delete JSON`; `app contacts query|create|update JSON`; and `app notification send JSON`. For example: `device-cli app contacts query '{"query":"Alice","limit":5}'`. Quote JSON as one object argument, with no `action` field. Use RFC3339 times with the phone's timezone for calendar or scheduled notifications. Confirm details before creating or deleting calendar events. These data commands do not attach screenshots.
- Keep notification queries on `bridge_notification` with `action=query`. If all three flags are false, use an exposed Bridge data tool only when it supports the requested operation via another route.
- `home`, `back`, `wait [milliseconds]`, `open APP`, `url URL`, and `quick ACTION` wrap platform navigation. Prefer tapping a clearly visible app icon over `open`.
- Use `quick select_all`, `quick copy`, and `quick paste` for platform-aware editing. For an exact physical shortcut, use `key meta+a` or `key ctrl+shift+tab`; the keys are pressed together. Unknown keys fail the command.
- Prefer to chain known, layout-independent steps with `&&`, e.g. `device-cli swipe 500 800 500 400 && device-cli wait 1000`. Do not chain speculative taps across pages or steps that require confirmation. Check the final screenshot before planning the next action. If a chain fails, some earlier actions may already have taken effect; do not blindly replay the chain.
- Do not use controller commands or external automation to bypass this visual workflow. Never leave device commands running in background shell sessions.

### Multiline text

In the focused field, pass the full text as one single-quoted shell argument
with actual line breaks, not literal `\n` or Bash-style `$'...'` quoting:

```sh
device-cli app type 'First line
Second line'
```

In the shell tool's JSON input, encode that line break once as `\n`:

```json
{"command":"device-cli app type 'First line\nSecond line'"}
```

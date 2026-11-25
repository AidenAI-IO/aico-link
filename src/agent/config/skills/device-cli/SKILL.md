---
name: device-cli
description: Operate and inspect the target device visually with device-cli in the foreground shell. Read before controlling any app or device UI.
---

# Device CLI

Use `shell` with a foreground command such as `device-cli tap 514 263`. The tool attaches one final screenshot after the shell call, not after each action; stdout is not a screenshot. A turn normally starts with an attached screenshot. If you only need to see the screen, run `device-cli wait 0` to attach a fresh screenshot without adding a delay.

- Use `device-cli look` only when you need a JPEG file. It prints the saved path and does not attach an image to the shell result. Prefer the default directory `/tmp`; use `--dir DIRECTORY` to save elsewhere. For a crop, use `device-cli look --region 100 200 900 800`, with normalized integer coordinates for the top-left and bottom-right corners.
- `tap X Y`, `double-tap X Y`, `long-press X Y`: use integers from 0 to 1000 on each axis, measured from the top left of the latest frame. Aim at the center of a visible target.
- `swipe X1 Y1 X2 Y2 [milliseconds]`: start inside the region to scroll; the gesture follows your finger from start to end. When finding a specific row, inspect the screenshot after each swipe before swiping again; do not batch swipes that may pass the target.
- `drag X1 Y1 X2 Y2` for a fixed visible destination; use `drag-start X Y` and `drag-release X Y` in separate calls if the layout may move after the drag starts.
- `type TEXT` uses device input without the companion app. It uses the currently focused field without tapping it. First tap the intended editable field and verify focus when uncertain. Quote it as a single shell argument (in POSIX shell, single-quote it and encode a literal apostrophe as `'\''`). Do not interpolate screen text as shell syntax. `enter` submits when appropriate; `key backspace` deletes.
- Prefer `app type TEXT` when any of `app_connected`, `app_pip_enabled`, or `app_fgs_enabled` is true. It uses the companion app clipboard and paste route only. Focus the intended field first. A usable clipboard route is still required; on iOS without PiP, a recently prepared matching clipboard is needed. Success means the clipboard was ready and the paste shortcut was sent, not that the field was read back. If dispatch reports an error but may have reached the field, inspect the screen before retrying or switching to `type`.
- `device-cli app clipboard read` writes only the clipboard text to stdout, with no added newline. An empty clipboard is a successful read with empty output; check the exit status. To write text, use `device-cli app clipboard write TEXT`; pass `''` to clear it. Do not parse a clipboard read as JSON.
- Contacts use flags: `device-cli app contacts query --query Alice --limit 5`, `device-cli app contacts create --name Alice --phone-number '+15550100'`, or `device-cli app contacts update --contact-id CONTACT_ID --organization 'Example'`. Repeat `--phone-number` or `--email` for multiple values. Both write commands accept `--organization` and `--notes`.
- Send a notification with `device-cli app notification send --title Reminder --body 'Check the calendar' --sound=false`. Use `--schedule-at` with an RFC3339 time and timezone for a scheduled send, or omit it to send immediately. `--badge N` is optional. App data commands do not attach screenshots. Confirm details before creating or deleting calendar events.
- Calendar uses flags, not JSON. Use `device-cli app calendar query --from 2026-11-23T00:00:00+08:00 --to 2026-11-24T00:00:00+08:00`, `device-cli app calendar create --title 'Team sync' --start-at 2026-11-23T09:00:00+08:00 --end-at 2026-11-23T09:30:00+08:00`, or `device-cli app calendar delete --event-id EVENT_ID`. On create, `--all-day`, `--location TEXT`, `--notes TEXT`, and `--alarm-minutes-before N` are optional. Treat an error or missing result as unverified, not an empty event list.
- After a denied permission, report the blocker or follow a permitted on-screen alternative rather than retrying the same denied call. If a write may have completed despite an error, verify the result before retrying.
- Connected, FGS, or PiP state permits an app data attempt. Supported data actions can also use BLE Wake or the return entry when available. Use `bridge_notification` with `action=query` to read existing notification events when the tool is exposed; do not use it to send notifications.
- `home`, `back`, `open APP`, `url URL`, and `quick ACTION` wrap platform navigation. Prefer tapping a clearly visible app icon over `open`. To launch another app from inside Settings, use `device-cli open Clock` (or the requested app name), not the Settings search box.
- Use `device-cli quick dismiss_panel` to close the Android notification shade and return to the underlying app; `home` instead leaves that app. Use `device-cli quick app_switch` to open Recents, then inspect the screenshot and tap the intended app card. The action is `app_switch`, not `recent_apps`.
- `wait [milliseconds]` pauses for a fixed duration (0–30000 ms, default 2000 ms). It does not poll for screen stability or capture intermediate frames.
- Use `quick select_all`, `quick copy`, and `quick paste` for platform-aware editing. For an exact physical shortcut, use `key meta+a` or `key ctrl+shift+tab`; the keys are pressed together. Unknown keys fail the command.
- Prefer to chain known, layout-independent steps with `&&`, e.g. `device-cli swipe 500 800 500 400 && device-cli wait 1000`. Do not use `||` for fallbacks: inspect the tool result and current screen before choosing an alternative. Do not chain speculative taps across pages or steps that require confirmation. If a chain fails, some earlier actions may already have taken effect; check the final screenshot rather than blindly replaying the chain.
- Do not use controller commands or external automation to bypass this visual workflow. Never leave device commands running in background shell sessions.

### Command help

Use `device-cli --help` for the command list. Add `--help` immediately after
any command, group, or action to see its usage, for example
`device-cli app contacts --help` or `device-cli app notification send --help`.
Help runs locally and does not operate the device. After a syntax error, use
this help before trying another command rather than guessing a new verb.

### Exit status and output

Use the exit status (`0` for success, nonzero for failure), not stdout, to
check whether a command succeeded. Errors go to stderr. Successful commands
with no result data, such as `tap`, `type`, `app clipboard write`, and `wait`,
are silent. Success does not replace visual verification.

- One string field: the raw string, without quotes or an added newline.
  `app clipboard read` returns the text; `app calendar create`, `app contacts create`,
  and `app notification send` return their respective IDs. `look` prints a JPEG path on its own line.
- One non-string field: the JSON value itself. Calendar and contacts queries
  return arrays directly (`[]` for no matches), not objects with `events` or `contacts` keys.
- Multiple data fields: a JSON object. The `ok` and `restored_from_return_entry`
  status fields are not printed.

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

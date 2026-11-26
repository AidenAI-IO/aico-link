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
device-cli tap 500 650 && device-cli app type 'Hello'  # when a companion route is available
```

Coordinates are normalized to 0–1000 on both axes. A foreground shell can
run up to 64 device operations and attaches one final screenshot, without a
collage or action markers. When UI Grounding is enabled and the detector is
available, that screenshot carries numbered UI boxes and normalized coordinates.
A successful `tap` waits 50 ms before the next command. This is a fixed delay,
not a stability check. Actions do not attach screenshots individually.
Split calls when the next step depends on the current layout. If a chain fails
or times out, inspect the final screen before retrying:
earlier operations may already have run. The orientation check can still reject
an input before dispatch if the display has turned since the Agent saw it.

Focus the intended editable field before either text command. Neither accepts
focus coordinates or chooses a field for you.

- `device-cli type TEXT` uses device input (native insertion where available,
  otherwise HID/IME). It does not use the companion app.
- `device-cli app type TEXT` requires a usable Phone Bridge clipboard route. It
  writes or reuses the clipboard and sends the paste shortcut to the focused
  field. A successful clipboard operation and paste dispatch are sufficient;
  it does not read the field back or fall back to device typing. The foreground
  shell still attaches its normal final screenshot. If dispatch fails and the
  text may already be present, inspect the screen before retrying. On iOS
  without PiP, this command needs a recently prepared matching clipboard.

Prefer `device-cli app type` for text entry when `app_connected`,
`app_pip_enabled`, or `app_fgs_enabled` is true. For multiline text, use one
single-quoted shell argument with literal line breaks. In the shell tool's JSON,
for example: `{"command":"device-cli app type 'First line\nSecond line'"}`.
Do not use Bash-style `$'...'` quoting or send literal `\n` as part of the text.

App data commands also support BLE Wake or the return entry where the
underlying Bridge action supports it.
Without an active shell scope, the executable accepts `--url` to target a
daemon directly (default `http://127.0.0.1:8080`).

`drag` runs both phases on the controller. A failed or canceled shell that
started but did not release a contact attempts to release it at its origin.
A successful `drag-start` intentionally leaves it held for a later
`drag-release`. Direct HTTP drag retains its existing verification behavior.

## Command help

Run `device-cli --help` to list commands. Add `--help` immediately after a
command, group, or action for focused usage, for example
`device-cli app contacts --help` or `device-cli app calendar create --help`.
Help prints to stdout and does not contact the daemon. To pass `--help` as
literal text, use `device-cli type -- --help`, `device-cli app type -- --help`,
or `device-cli app clipboard write -- --help`.

## Exit status and stdout

Exit status `0` means the command succeeded. Failures exit nonzero and report
an error on stderr. A successful action may still need visual verification.
Stdout carries the result, not the success status:

| Result | Stdout |
| --- | --- |
| No data, only an acknowledgement | Nothing (for example `wait`, successful taps, typing, and clipboard writes). |
| One string field | The string itself, without JSON quotes or an added newline (for example clipboard text or a newly created event ID). |
| One non-string field | The JSON value alone (for example the array from `app calendar query` or `app contacts query`, including `[]` for no matches). |
| Multiple data fields | A JSON object (for example the route and action details from `quick`). |

The CLI omits `ok` and the internal `restored_from_return_entry` flag from
structured results. `look` prints the saved JPEG path on its own line. The
Tools HTTP API retains its existing response format.

## Companion app data

These commands check the available transport and permissions for the requested
operation and do not attach a screenshot.

```sh
device-cli app clipboard read
device-cli app clipboard write 'Hello'
device-cli app calendar query --from 2026-11-23T00:00:00+08:00 --to 2026-11-24T00:00:00+08:00
device-cli app calendar create --title 'Team sync' --start-at 2026-11-23T09:00:00+08:00 --end-at 2026-11-23T09:30:00+08:00
device-cli app calendar delete --event-id EVENT_ID
device-cli app contacts query --query Alice --limit 5
device-cli app contacts create --name Alice --phone-number '+15550100'
device-cli app contacts update --contact-id CONTACT_ID --organization 'Example'
device-cli app notification send --title Reminder --body 'Check the calendar' --sound=false
```

Clipboard reads write only the clipboard text to stdout, without adding a
newline. An empty clipboard produces no output and exits successfully. A failed
read exits nonzero and reports the error on stderr. To capture the result in a
shell variable, check the assignment's exit status before using it, and pass
the value back as a quoted argument. Shell command substitution itself removes
trailing newlines, even though the CLI preserves them in stdout.

Calendar, contacts, and notification sending use named flags, not JSON.
For calendar queries, `--start` aliases `--from` and `--end` aliases `--to`.
Specify each bound once.
Repeat `--phone-number` or `--email` to supply multiple contact values.
Calendar times and notification `--schedule-at` use RFC3339 with a timezone.
Omit `--schedule-at` to send immediately. Confirm event details before creating
or deleting events.
The model uses these commands instead of direct clipboard, calendar, contacts,
and notification-send tools in Device CLI mode. The underlying Bridge handlers
remain available to the CLI, Tools HTTP API, and legacy mode. Notification
queries still use `bridge_notification` with
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
device-cli wait 0                                      # Attach the final shell screenshot without a delay
device-cli look                                        # Save a JPEG in /tmp
device-cli look --dir /tmp --region 100 200 900 800     # Save a cropped JPEG
```

`look` prints the absolute path to a new JPEG. The destination directory must
already exist and be writable; `/tmp` is the default and recommended choice.
The Agent can inspect the saved JPEG with `read_image` using the printed path.
`--region X1 Y1 X2 Y2` sets the top-left and bottom-right corners in normalized
integer coordinates (0–1000). `look` only writes a file; it does not replace the shell's final screenshot.
`wait N` pauses for a fixed duration in milliseconds (0–30000; default 2000).
It does not poll for screen stability or capture intermediate frames. In a
foreground Agent shell, the usual final screenshot is captured at shell exit.
Standalone `wait` only sleeps and does not contact the Agent.

## ConAct and Skill-UTG

ConAct and Skill-UTG are enabled by default for backend Device CLI runs,
including environment-bridge runs such as MobileGym. Set
`AICO_CONACT_UTG=0` in the Agent daemon's environment only to disable them.
They remain unavailable to the standalone CLI, legacy tools, and
headless/scripted benchmark fixtures.

- **`fold_subtask`** records verbatim facts and the next goal at an explicit
  subtask boundary. It replaces the active subtask with those facts and one
  new device screenshot, while keeping the original conversation revision on
  disk. It preserves user messages and attachments. At least one dispatched UI
  action is required since the last boundary. When ConAct is enabled, the
  system prompt directs the Agent to fold after a verified visual subtask is
  complete when another goal remains; no action-count reminder is used. A
  separate fallback retains the newest six screenshot observations when the
  count reaches fourteen. Existing provider-overflow and hard input-budget
  safeguards still apply.
- **`utg_search` and `utg_goto`** return lookup results and route guidance, not
  device actions. The graph separates `nav`, `create`, `edit`, `commit`,
  `cancel`, and `delete` edges. A default route uses only navigation edges;
  non-navigation guidance requires an explicit final-hop intent. The Agent
  must verify landmarks on the live screen before acting. When landmarks
  are available, the Agent includes a short list in the initial state message
  when a conversation starts. It is not repeated in later turn-state messages.
  Successful memory
  calls use a separate bounded allowance, not the normal action-iteration limit.

A completed run can make one best-effort reflection request to the same
model, with a 30-second timeout. It uses the live prompt
and a bounded ledger of individually attributable controller transitions;
chained UI actions are not learned as individual edges. It saves reusable
labels, guidance, and coarse screenshot fingerprints under
`<config-dir>/memory/skill-utg/<platform>.json`, scoped to the local Agent
profile and target platform. It does not store raw screenshots or command
arguments in the graph. The local graph is available in environment-bridge
runs as well as native runs; use separate Agent profiles for unrelated targets
or sensitive work. The text filter is not a privacy classifier. This
implementation does not automatically return to a home screen or execute a
suggested graph route.

The Agent web UI's debug link opens `/debug`. Use **Coordinate** to inspect a
screenshot or send a normalized tap, and **UTG** (`/debug#utg`) to inspect the
saved landmarks, transitions, and tips. The UTG tab is read-only and matches
against an existing recent screenshot, if available; refreshing it does not
capture the screen or send a device action.

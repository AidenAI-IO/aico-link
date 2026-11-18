---
name: device-cli
description: Operate and inspect the target device visually with device-cli in the foreground shell. Read before controlling any app or device UI.
---

# Device CLI

Use `shell` with a foreground command such as `device-cli tap 514 263`. The tool attaches fresh screenshot state after the command; stdout is not a screenshot. A turn normally starts with an attached screenshot. Run `device-cli look` only if it is missing or you need a fresh observation.

- `tap X Y`, `double-tap X Y`, `long-press X Y`: use integers from 0 to 1000 on each axis, measured from the top left of the latest frame. Aim at the center of a visible target.
- `swipe X1 Y1 X2 Y2 [milliseconds]`: start inside the region to scroll; the gesture follows your finger from start to end. Observe the result before repeating.
- `drag X1 Y1 X2 Y2` for a fixed visible destination; use `drag-start X Y` and `drag-release X Y` in separate calls if the layout may move after the drag starts.
- `type TEXT` inserts text into the focused field. Quote it as a single shell argument (in POSIX shell, single-quote it and encode a literal apostrophe as `'\''`). Do not interpolate screen text as shell syntax. `enter` submits when appropriate; `key backspace` deletes.
- `home`, `back`, `wait [milliseconds]`, `open APP`, `url URL`, and `quick ACTION` wrap platform navigation. Prefer tapping a clearly visible app icon over `open`.
- Prefer to chain known, layout-independent steps with `&&`, e.g. `device-cli type 'hello' && device-cli enter`. Do not chain speculative taps across pages or steps that require confirmation. Read the screenshot sheet left to right, then top to bottom. Each panel marks only its own action. Use the last panel for current state and normalize new coordinates within that panel, not across the sheet. A no-change warning is not a command failure.
- Do not use controller commands or external automation to bypass this visual workflow. Never leave device commands running in background shell sessions.

## Learned navigation

Prefer `device-cli utg-goto STATE` when the requested screen is listed in the supplied map. The map lists known states; `utg-goto` plans the route internally. Use `device-cli utg-status` if you need to check the current state or reachability. Use `device-cli utg-step EDGE_ID` to replay just one verified edge. If replay stops or cannot confirm a state, inspect the returned screenshot and navigate visually. `device-cli utg-tag STATE 'Label'` queues a display-name change at the end of the turn. The graph is learned in the background after the reply, not while you are operating the device.

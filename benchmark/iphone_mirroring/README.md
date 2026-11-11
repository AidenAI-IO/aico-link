# iPhone Mirroring Environment Bridge

For the complete native Agent and Config Web startup sequence, start with
[Local Agent with a Real Phone](../../docs/01-getting-started/local-agent-real-phone.md).
This README provides the iPhone-specific reference and diagnostics.

Run the Agent on a Mac and operate a real iPhone through Apple's iPhone
Mirroring app. This experimental backend implements the existing screenshot
and MNK provider APIs. It does not require an RV1106 board, ADB, PyAutoGUI,
or modifications to the iPhone.

## Requirements

- A Mac and iPhone that already work with Apple's iPhone Mirroring. Connect
  the phone manually and keep it locked while mirroring.
- Xcode Command Line Tools (`xcrun swiftc`) and Python 3.10 or newer.
- Screen Recording permission for capture and Accessibility permission for
  input, granted to the terminal/bridge executable in macOS System Settings.

The native helper uses ScreenCaptureKit for window capture and CoreGraphics
for input. Normal startup checks existing permissions without requesting or
changing them. To explicitly show the macOS Accessibility authorization request,
run `python3 -m iphone_mirroring --request-input-access` and approve it yourself.
For screen capture, use `--request-screen-access`. macOS may list the launching
terminal or host application rather than the helper. Approve the application
identified by the system prompt. In newer macOS versions, Accessibility may
be labeled **Device Control and Data Access** under Privacy & Security.
Denial does not trigger repeated prompts. `--probe` reports both permissions
even when the mirror window is unavailable, and input/capture recheck their
permissions on every request so later revocation returns an error.
The HTTP server listens only on `127.0.0.1` and claims one task route. Run it
only while using the mirrored phone. Input focuses the mirror window and
temporarily uses the Mac's pointer and keyboard.

## Start

From `benchmark/`:

```bash
python3 -m iphone_mirroring --probe
python3 -m iphone_mirroring --port 8897 --task-id iphone-mirroring
```

The first invocation compiles the helper into
`.cache/iphone-mirroring/mirroring-native` under the repository root. `--helper`
can select an already compiled executable. Keep its path stable for macOS
permission attribution. If `input_available` is false, grant Accessibility
permission to the responsible terminal/bridge executable, restart the process,
and probe again. The bridge can serve screenshots while input is unavailable;
input requests return an error rather than silently succeeding.

`--probe` reports the visible window and permissions. A visible window does not
prove the phone is connected: `connection_verified` is deliberately false.
Inspect a screenshot before operating the phone, including after reconnecting.
Connection dialogs, phone use, protected content, or a minimized mirror can
interrupt the session. The bridge does not unlock or reconnect a phone.

## Connect the local Agent

Use a dedicated Agent data directory containing your model configuration.
Copy `iphone_mirroring/quick_actions.json` into that directory as
`quick_actions.json`. This selects mirror shortcuts for Home, App Switcher,
and Spotlight and omits board-specific quick actions. Do not replace the
mapping in a board's normal data directory.

From `src/agent/`, start the daemon:

```bash
go run ./cmd/daemon \
  -dir /absolute/path/to/mirror-agent-data \
  -addr 127.0.0.1:8080 \
  --device-type iOS \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://127.0.0.1:8897 \
  --benchmark-task-id iphone-mirroring
```

Then open Agent Web at `http://127.0.0.1:8080`. The task ID must match the
bridge's `--task-id`. The route is initially claimed by the CLI; subsequent
requests renew its 30-minute lease. After 30 minutes without requests, reclaim
it with **Restore iPhone Mirroring connection** on the local Config Web overview,
or through `POST /api/setup` with the same `benchmark-task-id` header. Stop the bridge with Ctrl-C when finished.

The native macOS Agent supports this bridge workflow. Board filesystem backup
and restore still require Linux `syncfs` and explicitly fail on macOS; they
are not silently replaced with weaker single-file synchronization.

To configure models and other host settings, start the same binary:

```bash
/absolute/path/to/agent config-web --local-agent \
  --config /absolute/path/to/mirror-agent-data/agent.toml \
  --web-root /absolute/path/to/repository/src/config_web/web \
  --agent-url http://127.0.0.1:8080
```

Then open `http://127.0.0.1:8000`. The overview shows a restore-connection button
only when the running Agent's configured bridge identifies itself as
`iphone_mirroring`. It uses that Agent's launch-time endpoint and task ID,
reclaims the session without a reset, and checks that a fresh mirror frame can
be captured. A screenshot confirms the capture route; it does not prove that
Apple's app has dismissed a phone-unlock or connection dialog. Keep the phone
locked and connect it in iPhone Mirroring first. The page reports missing
permissions, capture failures, and sessions owned by other tasks; it never
releases another task or restarts the Agent.
See [Configure a Native Local Agent](../../docs/01-getting-started/try-aico-on-pc.md#configure-a-native-local-agent).

For manual diagnostics:

```bash
curl http://127.0.0.1:8897/health
curl http://127.0.0.1:8897/api/providers/screenshot \
  -H 'Content-Type: application/json' \
  -H 'benchmark-task-id: iphone-mirroring' -d '{}'
```

The screenshot response contains a base64 JPEG in `data.image`. The bridge
also supports the standard setup, release, concurrency, and provider endpoints
described in [Environment Bridge Protocol](../environment_bridge.md).

## Coordinates and input

Screenshots contain only the phone content rectangle. Normalized 0–1000
coordinates are mapped to that rectangle in macOS logical points, while the
JPEG retains Retina pixels. The helper re-reads window geometry for each
operation. Default content insets are `8,38,8,8` (left, top, right, bottom) in
points; adjust `--content-insets` if the app's window chrome differs. Window
movement and Retina scaling do not require changing Agent coordinates.

Clicks, double clicks, long presses, key combinations, text, and continuous
pixel scroll are implemented. Swipes become scroll events with explicit
gesture phases; they are not mouse drags. Horizontal scrolling is supported
by this backend. Home/App Switcher/Spotlight use Cmd+1/2/3. Home from the App
Switcher may first return to the current app, requiring another Home action.

The Agent's `enter_text` automatically discovers the bridge's `insert_text`
capability. It focuses the supplied field, sends the original Unicode text
through macOS, and verifies the resulting iPhone screenshot. Chinese, mixed
Latin text, and Emoji do not need phone-side Pinyin or candidate selection.
The helper temporarily selects the Mac's ASCII keyboard layout, inserts
committed Unicode events, then restores the previous input source, including
Squirrel/Rime. It does not use or change either clipboard. A replacement query
selects the field inside that same native operation, after switching layout.
Raw `keyboard_text` and `keypress` retain physical-key behavior for explicitly
driving the selected IME. `--probe` reports the current Mac input source.

Insertion accepts at most 4096 UTF-16 units and rejects control characters
other than newline/tab. It rechecks input permission and mirror focus during
delivery. A delivery or verification failure does not replay text or fall back
to phone IME input: inspect the field before retrying because it may contain
part of the text. Keep the mirror focused during insertion. Capture, clicking,
scrolling, Chinese/Latin/Emoji insertion, and input-source restoration were
verified against a real iPhone mirrored on macOS 27.0.1. Behavior can differ
between macOS/iOS pairings; accepted events alone do not prove delivery.
Persistent drag, atomic touch programs, and multi-touch are unsupported.

The mirror does not supply an iOS accessibility tree, Aico Phone Bridge data
APIs, BLE, microphone access, or board peripherals. Contacts and calendar tools
are not made available merely by connecting this bridge.

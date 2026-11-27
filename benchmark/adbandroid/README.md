# ADB Android Environment Bridge

For interactive experiments with an Agent running directly on your computer,
start with [Local Agent with a Real Phone](../../docs/01-getting-started/local-agent-real-phone.md).
It covers native startup and Config Web before connecting this bridge.

For normal native use, run `python3 scripts/local_agent.py android` from the
repository root. It selects the only authorized device (or accepts
`--adb-serial`), then starts Config Web, this bridge, and the Agent together.

Controls an Android emulator (Genymotion) or a physical device through adb, and
exposes an HTTP protocol to the benchmark that is **fully compatible with the
MobileGym bridge**. The Aico Go Agent uses the bridge's screen and MNK
providers directly.

```text
benchmark/runner (test orchestration)
  ↓ /api/chat
Aico Go Daemon (environment-bridge mode)
  ↓ POST /api/providers/mnk              ← input primitives
  ↓ POST /api/providers/screenshot       ← screen provider
ADB Android Bridge (this module, local Python process)
  ↓ adb -s <serial> shell input ...
Android emulator / physical device
```

Differences from MobileGym: MobileGym runs in Docker and supports multiple
concurrent environments; the ADB bridge is a **local process driving a single
device** (`/api/concurrent` is always 1) and needs no Docker.

## Prerequisites

1. **adb available**: `adb` on `PATH` (or pass `--adb-path`), and `adb devices`
   shows the target device:

   ```text
   $ adb devices
   List of devices attached
   127.0.0.1:6555	device        # Genymotion (default)
   emulator-5554	device        # this is what an official AVD looks like
   XXXXXXXX	device            # a USB physical device
   ```

   ⚠️ The default serial is Genymotion's `127.0.0.1:6555`. For an AVD or a
   physical device you must pass `--adb-serial <serial>` explicitly (or set the
   `ANDROID_SERIAL` environment variable).

2. **Python dependencies**: just run under `uv` from the `benchmark/` directory
   (Pillow is used for screenshot compression and is already in
   `pyproject.toml`).

3. **agent config**: the Go daemon needs your own config directory (LLM provider
   and API key); see `benchmark/config/agent.toml.template`. The repo contains no
   secrets.

## Unicode text with ADBKeyBoard

For Chinese, emoji, multiline text, and literal `%s`, select ADBKeyBoard in the
Launcher. When the ADB bridge starts, it installs the selected IME if it is
missing. The bridge downloads the pinned upstream v2.5-dev APK and verifies its
SHA-256 before installing it. The upstream APK includes the receiver fix for
recent Android versions. Manual installation is optional:

```bash
curl -fL https://github.com/senzhk/ADBKeyBoard/releases/download/v2.5-dev/keyboardservice-debug.apk -o /tmp/ADBKeyboard.apk
shasum -a 256 /tmp/ADBKeyboard.apk
# Expected SHA-256: 41a8a0996d7397a2390d1ca16a75cb66c4a7bdaa89cf4e63600a4d3fb346fbbb
adb -s YOUR_ADB_SERIAL install -r /tmp/ADBKeyboard.apk
```

Keep your usual keyboard selected. The bridge advertises `insert_text` through
`GET /api/providers/mnk` when ADBKeyBoard is installed; installation does not
require a bridge restart after manual installation. The bridge enables
ADBKeyBoard on first use. The Agent's `enter_text` then uses its existing native
Unicode insertion and screen-verification path. Each insertion saves the
current IME, selects ADBKeyBoard, waits for its editor connection, sends UTF-8
text as Base64 with a package-targeted `ADB_INPUT_B64` broadcast, and restores
the original IME even on failure. Internal replacement requests send
`ADB_CLEAR_TEXT` first. No clipboard is modified. The direct bridge `enter_text`
tool also uses this route and verifies editable text through UIAutomator.

Input is limited to 1–4096 UTF-16 units and rejects control characters except
newline/tab. A broadcast response alone cannot prove that an app accepted text;
inspect the field after verification failures or timeouts before retrying.
The bridge does not replay an uncertain insertion. A restore failure is reported
explicitly; select your normal IME with `adb shell ime set YOUR_ORIGINAL_IME`.
If the selected IME APK cannot be downloaded or installed while the device is
ready, bridge startup reports the failure and stops before serving requests.
If the device is not ready, installation is deferred until a later capability
probe or text insertion. Preinstall the APK with the commands above when the
host is offline. The low-level `keyboard_text` primitive remains ASCII-only.

## Appium UnicodeIME alternative

Appium Settings' `UnicodeIME` is an alternative for devices where ADBKeyBoard
is unavailable. When selected, the bridge downloads the pinned v8.2.1 APK from
the [official Appium Settings repository](https://github.com/appium/io.appium.settings),
verifies its SHA-256, and installs it if the selected IME is missing. The bridge
does not grant SMS or notification permissions. Appium Settings has exported
receivers, so use it on a test device without sensitive SMS or notifications.
The bridge enables `io.appium.settings/.UnicodeIME` when needed, sends modified
UTF-7 through `adb input text`, and restores the previous IME. The Appium route
rejects literal `%s`, which Android interprets as a space in `input text`;
ADBKeyBoard supports it.

ADBKeyBoard remains the default. Select Appium explicitly when starting the
local phone launcher:

```bash
python3 scripts/local_agent.py android --unicode-input-method appium
```

In Aico Launcher, choose **Appium UnicodeIME** in the Android instance's
**Unicode input method** field. Stop the instance before editing it, save, and
start it again to apply the selection.

For direct bridge startup, pass the same option:

```bash
uv run python -m adbandroid.scripts.start_bridge \
  --adb-serial YOUR_ADB_SERIAL \
  --unicode-input-method appium
```

The selected path is fixed for the bridge process; it does not fall back to the
other Unicode IME if the selected one is unavailable. A running bridge reports
Unicode input as unavailable when installation fails, retaining its ASCII input
path. Explicit Unicode insertion still reports the installation error.
Appium replacement requests need Android's `input keycombination` support for
Ctrl+A and Delete.

## Quick start

### Option 1: CLI (recommended)

```bash
cd benchmark

# 1. Start the bridge (background process; prints environment_url and more)
uv run python -m runner start-adb-android-env \
  --adb-serial 127.0.0.1:6555 \
  --bridge-port 8899

# 2. Start the agent daemon (Docker container; builds the image automatically)
uv run python -m runner start-agent-daemon \
  --environment-bridge-endpoint http://127.0.0.1:8899

# 3. Run the benchmark (take --agent-url / token from step 2's output).
#    This command enables the judge — export AICO_BENCHMARK_JUDGE_API_KEY first;
#    see the "Judging" section. Append --no-judge to only exercise the pipeline.
uv run python -m runner run \
  --suite suites/adb_android_basic.json \
  --agent-url http://127.0.0.1:<agent-port> \
  --environment-url http://127.0.0.1:8899 \
  --benchmark-task-id cli-task \
  -v
```

Stop the bridge with the `stop_command` (`kill -TERM <pid>`) printed by step 1.

### Option 2: Manual startup (when debugging the Go daemon)

```bash
# 1. Start the bridge as a foreground process
cd benchmark
uv run python -m adbandroid.scripts.start_bridge \
  --adb-serial 127.0.0.1:6555 \
  --bridge-port 8899

# 2. Run the daemon locally (the daemon and the runner must use the SAME benchmark-task-id)
cd ../src/agent
go run ./cmd/daemon \
  -dir <your agent data directory> \
  --environment-bridge-mode \
  --local-phone-bridge \
  --environment-bridge-endpoint http://127.0.0.1:8899 \
  --benchmark-task-id cli-task

# 3. Run the benchmark (same as Option 1 step 3, with --benchmark-task-id cli-task)
```

### Option 3: WebUI

```bash
cd benchmark
uv run python -m runner webui
```

In the browser: pick a suite → Run → in the environment dialog switch to the
**ADB Android** tab → fill in the serial → Start → select that environment →
Run. The WebUI starts/stops the bridge process and, after a restart, can recover
a still-running bridge via its pidfile + a `/health` probe.

## Judging

The judge scores each task's rubric item by item using the pre/post screenshots
and the trace (e.g. "did Settings actually open?", "was the alarm count
correct?"), over an OpenAI-compatible endpoint. The judge model is
completely independent of the agent-under-test model (the agent model is set in
`agent.toml`).

- **`--no-judge`**: runs hard assertions to check the execution pipeline. Tasks
  with a rubric remain unscored (`skipped`) and are excluded from success rates
  until rejudged; known assertion failures still fail. Tasks without a rubric
  are decided by their deterministic assertions.
- **Judge enabled** (the default for `run`, i.e. without `--no-judge`): you must
  provide `AICO_BENCHMARK_JUDGE_API_KEY`, otherwise it fails with
  `missing env var AICO_BENCHMARK_JUDGE_API_KEY`.

```bash
cd benchmark
export AICO_BENCHMARK_JUDGE_API_KEY=...

uv run python -m runner run \
  --suite suites/adb_android_basic.json \
  --agent-url http://127.0.0.1:<agent-port> \
  --environment-url http://127.0.0.1:8899 \
  --benchmark-task-id cli-task \
  --judge-model anthropic/claude-sonnet-4-6 \
  -v
```

`--judge-model` can be omitted (default `anthropic/claude-sonnet-4-6`); pass the
model name accepted by the configured OpenAI-compatible endpoint to use a
different judge model.

> The correctness of the multi-step tasks (count alarms in Clock / check WiFi /
> open the app drawer) can only be verified by the judge; under `--no-judge`
> their rubrics are always 0/N.

### Re-scoring existing results (rejudge)

After a `--no-judge` run, the pre/post screenshots and the trace are already
saved in the run directory, so you can re-score **without re-running the
device**:

```bash
export AICO_BENCHMARK_JUDGE_API_KEY=...
uv run python -m runner rejudge \
  --run-dir runs/<that run's directory> \
  --judge-model anthropic/claude-sonnet-4-6
```

This saves a full round of device operations when tuning rubric wording, or when
you ran `--no-judge` first to verify the pipeline and want to score afterward.

Rejudging refreshes screenshot flags from saved history and episode references
when they align with the recorded trace. Automatic post-action screenshots count
as screenshot evidence; a separate `screenshot` call is not required unless the
rubric explicitly requests that tool. Earlier failed attempts do not invalidate
a later successful recovery. Recorded execution gates and original trace files
remain unchanged.

## Test suite

The native UI suites contain 40 distinct tasks:

- `adb_android_basic` (`suites/adb_android_basic.json`), 16 tasks: screen reading,
  Settings, Wi-Fi status, app drawer, Back, scrolling, Home, recents entry,
  notifications, cross-app navigation, and state recovery.
- `adb_android_text_editing` (`suites/adb_android_text_editing.json`), 4 tasks: spaces,
  punctuation, query replacement, and clearing in Settings search.
- `adb_android_builtin_apps` (`suites/adb_android_builtin_apps.json`), 20 tasks:
  Chrome navigation, in-page search, and tab switching; Google Maps place
  search, walking directions, step lists, and travel-mode switching;
  Camera-to-Photos capture and photo details; Files image browsing and preview;
  unsent Messages drafting and revision; Clock timer control; and Contacts
  search, creation, editing, and deletion.

The `adb` CI profile runs six cases and 70 task attempts per repeat. Basic and
text-editing each run with and without Aico App (40 attempts), built-in apps
run without Aico App (20 attempts), and the dedicated
`suites/aico_app/android_bridge_capabilities_v1.json` suite adds 10 tasks.
The daily `runnable` profile includes the five UI cases (60 attempts); the
dedicated Bridge capability case remains manual.

The built-in app tasks share one suite because they use the same ADB device and
setup. Task IDs carry an app prefix so results remain easy to filter by app.
Split an app into its own suite when it needs a separate account, fixture, or
run schedule. MobileGym's upstream app registry is a task reference, while
this benchmark runner loads JSON suites from `benchmark/suites`.

Use an unlocked device with an app drawer, Settings search, and Clock's alarm
list available. The built-in app suite also requires Chrome, Google Maps,
Camera, Google Photos, system Files, Messages, Clock, Contacts with Device storage,
and access to `www.iana.org` and Maps search.
Complete first-run onboarding before running the suites. Tasks use English
instructions and accept equivalent localized Android screens. The Contacts
tasks only modify an exact synthetic contact named `Aiden Benchmark Contact`;
creation removes a stale matching contact, and the delete task removes it.
The Camera and Files tasks create local JPEGs with timestamped names; they
must not assume an empty gallery. The Messages task leaves text unsent.

YouTube is present on the current runner2 emulator, but its installed app opens
only a mandatory update screen, while `m.youtube.com` currently fails to load
in emulator Chrome. Add a playback task after the emulator image and network
path can load public videos without an account; otherwise a failed score would
measure the environment rather than Agent operation.

The bridge returns Home and dismisses the keyboard before each task; it does
not reset app data or launcher layout. Task setup prepares the required screen
or search query. All ADB cases run serially on the connected device.

The three `quick_action_*` tasks require successful execution of the named
shortcut. Other tasks accept any supported route unless the goal explicitly
requires recents. Judging uses pre/post screenshots and the execution trace;
`--no-judge` checks execution only, not UI correctness.

In GitHub Actions, select profile `adb` and leave `suite` empty to run all
six cases, or enter one suite path to run its matching cases. Basic and
text-editing suite paths each select both comparison variants. The workflow requires an
already booted device and `BENCHMARK_ANDROID_SERIAL`; see
[CI configuration](../../docs/10-benchmark/README.md#github-actions-ci).
For local runs, use any suite path in the quick-start command above. Add
`--task-id <id>` to select individual tasks and `--repeats <count>` to repeat them.

## HTTP protocol

Consistent with the MobileGym bridge (both the runner and the Go agent integrate
against this):

| Endpoint | Description |
|---|---|
| `GET /health` | Returns 200 when the device is online; 503 when adb is unreachable |
| `GET /api/concurrent` | `{"ok":true,"data":{"concurrent":1,...}}`; always 1 for a single device |
| `POST /api/providers/screenshot` | Current screenshot (no setup required; used by the runner for pre/post capture) |
| `POST /api/providers/mnk` | Mouse/keyboard primitives used by the Go agent |
| `POST /api/setup` | Reset to home + create an episode; single-device ownership keyed on the `benchmark-task-id` header |
| `POST /api/prepare` | Run a deterministic starting-state recipe after `/api/setup`; `422 prepare_failed` when it cannot confirm the state |
| `POST /api/release` | Release the task id's ownership |
| `GET /api/tools` | Tool catalog |
| `POST /api/tools/{tool}` | Legacy tool endpoint for bridge clients and debugging |

Task-routing semantics (single-device variant, aligned with MobileGym's
single-env behavior):

- **Empty `benchmark-task-id`**: falls straight through to the one device state
  without an ownership check (the WebUI serial path relies on this, because the
  daemon's tool calls carry no such header).
- **With a task id**: the first setup takes ownership; the same id is idempotent;
  a different id returns `429 no_bridge_env_available` until the owner releases.

## Tools and coordinates

The tools match MobileGym: `touch_gesture` `keyboard_text`
`keyboard_tap` `enter_text`
`mouse_move` `mouse_scroll` `quick_action`.

All pointer and touch inputs use normalized 0-1000 coordinates. The bridge
converts them to device pixels using `adb shell wm size` (Override wins over
Physical). Values outside 0-1000 are rejected.

`quick_action` selects Android bindings automatically: `back` / `home` /
`app_switch` / `send` /
`open_settings` (`am start -a android.settings.SETTINGS`) / `notification_center`
/ `control_center` / `dismiss_panel` (the `cmd statusbar` family, falling back to
a gesture on failure). Pass `{"list": true}` to see the full catalog.

## Known limitations

- **Unicode needs ADBKeyBoard**: install the optional IME as described
  above. Without it, `adb input text` supports only ASCII and interprets literal
  `%s` as a space. That fallback temporarily selects a known enabled Latin IME
  when available, then restores the previous IME.
- **Text entry is verified best-effort**: `enter_text` reports `ok:true` only
  when Android UIAutomator
  exposes matching editable field text after insertion. The bridge dumps
  UIAutomator XML to a temporary device file and reads it back because some
  devices do not emit XML for `/dev/tty`. When verification is unavailable or
  the field text differs, it returns `ok:false` with a next-step suggestion and post-action
  screenshot so the agent can inspect the keyboard/IME state before retrying.
- **Single-device, serial**: one bridge drives one device and does not support
  concurrent tasks. For multiple devices run multiple bridge instances (different
  ports + different serials).
- **Text-editing shortcuts require native support**: `keyboard_tap` sends
  `Ctrl+A` and `Ctrl+Z` through Android's `input keycombination` when the device
  advertises it. These shortcuts let the Go text engine clear fields and undo
  input probes. Older devices and other Ctrl/Alt/Shift combinations return an
  explicit error; modifiers are never silently dropped. Nonzero `hold_ms` is
  rejected because `input keyevent` cannot hold a key for an arbitrary duration.
- **Reset verifies keyboard visibility**: setup returns Home, checks
  the current manager's `mInputShown` and the IME service's `mWindowVisible`
  in `dumpsys input_method`, and uses Back/Home to dismiss a remaining keyboard.
  Either available signal reporting visible keeps reset in its retry loop;
  all available signals must report hidden before setup proceeds.
  Historical records and `mIsInputViewShown` are ignored:
  the latter can remain true after the IME window has been hidden.
  Setup fails if the keyboard remains visible or its visibility cannot be
  determined, preventing state leakage into the next task.
- After an action the bridge waits 0.6s before taking the screenshot
  (`DEFAULT_ACTION_SETTLE_SEC`); on slow emulators a transition animation may
  still be in flight, and the agent can call `screenshot` again to confirm.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `/health` returns 503 `device_unavailable` | Check with `adb devices` that the serial is correct and the state is `device` (not `offline`/`unauthorized`) |
| Startup reports `adb binary not found` | adb is not on `PATH`; pass `--adb-path /path/to/adb` |
| A tool call returns 429 `no_bridge_env_available` | The device is owned by another task id: make sure the daemon and the runner use the same `--benchmark-task-id`, or call `/api/release` |
| A tool call returns 409 `no_active_episode` | `/api/setup` was not called first (the runner does this automatically; when debugging with curl you must setup first) |
| A screenshot raises a Pillow-related ImportError | The venv install is corrupt; `uv sync --reinstall-package pillow` |

Unit tests (no real device needed):

```bash
cd benchmark
uv run pytest tests/test_adbandroid_bridge.py tests/test_adbandroid_tools.py
```

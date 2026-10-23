# ADB Android Environment Bridge

Controls an Android emulator (Genymotion) or a physical device through adb, and
exposes an HTTP protocol to the benchmark that is **fully compatible with the
MobileGym bridge**. The Aiden Go agent uses the bridge's screen and MNK
providers directly.

```text
benchmark/runner (test orchestration)
  ↓ /api/chat
Aiden Go Daemon (environment-bridge mode)
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
#    This command enables the judge — export AIDEN_BENCHMARK_JUDGE_API_KEY first;
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

- **`--no-judge`**: no judging; rubrics record 0/N and pass/fail depends only on
  the hard assertions (an action ran, no timeout). Good for first verifying that
  the pipeline works end to end.
- **Judge enabled** (the default for `run`, i.e. without `--no-judge`): you must
  provide `AIDEN_BENCHMARK_JUDGE_API_KEY`, otherwise it fails with
  `missing env var AIDEN_BENCHMARK_JUDGE_API_KEY`.

```bash
cd benchmark
export AIDEN_BENCHMARK_JUDGE_API_KEY=...

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
export AIDEN_BENCHMARK_JUDGE_API_KEY=...
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

The `adb` CI profile expands coverage to six cases and 32 task executions per
repeat: the original eight tasks, twelve selected tasks reused unchanged from
existing suites, and twelve new native Android tasks. See
[Android coverage](coverage.md) for the compatibility matrix, exact selection,
device prerequisites, and commands. The `adb` profile uses a connected emulator
or physical device; it does not provision an emulator or run MobileGym apps.

`benchmark/suites/adb_android_basic.json`, 8 tasks:

- Basic capabilities (5): screenshot, go Home, open Settings, swipe, enter
  English text.
- Multi-step tasks (3, ported from mobilegym_basic): count alarms in Clock,
  check WiFi status, open the app drawer.

Note: `required_tools` for the action tasks is intentionally left empty — every
action tool on the daemon already returns a post-action screenshot, and the
agent may reach the same outcome with either `quick_action` or `touch_gesture`.
The correctness of the multi-step tasks is judged mainly by the rubric; under
`--no-judge` the hard assertions can only check "an action ran and it did not
time out".

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

- **Text input is English-only**: `adb input text` cannot type Chinese/IME
  composed text; non-ASCII returns an explicit error, and a literal `%s` in the
  text is typed as a space (the `input text` escaping convention). Chinese input
  would need a clipboard/IME approach, not yet implemented. For ASCII input, the
  bridge temporarily switches to a known enabled Latin IME when available, then
  restores the previous IME.
- **Text entry is verified best-effort**: `enter_text` reports `ok:true` only
  when Android UIAutomator
  exposes matching editable field text after `adb input text`. The bridge dumps
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

# Environment Bridge Protocol

Environment bridge is the HTTP contract between benchmark runners, agent
daemons, and any controllable environment. A bridge can represent a physical
device, a simulator, a browser, or another UI target.

Implementations in this repository:

- Go agent device bridge: fixed concurrency `1`.
- MobileGym bridge: concurrency equals the started env pool size.
- ADB Android bridge (`benchmark/adbandroid/`, see its README): drives an
  Android emulator or physical device through adb; fixed concurrency `1`.
- Desktop bridge (`benchmark/desktop/`): drives the host macOS, Linux, or
  Windows desktop through `pyautogui`; fixed concurrency `1`.
- [iPhone Mirroring bridge](iphone_mirroring/README.md): captures and controls
  Apple's iPhone Mirroring window on macOS; fixed concurrency `1`, platform `ios`.
- [Direct iOS device bridge](ios_device/README.md): controls a USB-connected
  iPhone through pymobiledevice3 and signed WDA/XCTest, including permission
  dialogs; fixed concurrency `1`, platform `ios`.

## Optional Native Text Insertion

`GET /api/providers/mnk` may return
`{"capabilities":{"insert_text":true}}` when the backend can insert committed
Unicode through its native text backend. Missing capability, HTTP 404, 405, or 501 retains
the Agent's existing Phone Bridge/HID/IME flow. Other discovery failures stop
the operation before changing the field.

For a supporting bridge, the Agent uses `POST /api/providers/mnk` with
`{"operation":"insert_text","insert_text":{"text":"中文ABC🔋","replace":false}}`.
The usual `benchmark-task-id` routing and `{"success":true}` response apply.
`replace:true` selects the entire focused field before insertion; the default
inserts at the current caret/selection. This is distinct from the existing
ASCII keystroke primitive `keyboard_text`. Text must be valid Unicode with
1–4096 UTF-16 units, without controls other than newline/tab.

The Agent keeps `enter_text` local, focuses its target and verifies the result
through screenshots. Once native insertion has started, failures do not trigger
an automatic replay or IME fallback because delivery can be partial.

## Optional Native System Navigation

The pymobiledevice3 iOS bridge additionally advertises
`{"capabilities":{"system_actions":["home"]}}`. Its target-specific Agent
quick-action config binds Home (including Spotlight's first step) to
`system_action` with `{"action":"home"}`. The Agent discovers support before
sending `{"operation":"system_action","system_action":{"action":"home"}}`
to `POST /api/providers/mnk`, using the usual task lease and success response.

The iOS bridge invokes WDA's native `/wda/pressButton` command with `name: home`
because synthetic bottom-edge swipes may be consumed as app scrolling. This
also dismisses Spotlight, unlike simply activating SpringBoard. Missing support or a
discovery error stops this explicit binding; failed input is never replayed as
a swipe. Existing Mirroring, Android, and HID quick-action configs keep their
current mappings. The system navigation binding is internal to `quick_action`,
not an additional public Agent tool.

## Required Endpoints

### `GET /health`

Returns bridge readiness and the controlled platform. Fixed-platform bridges
should expose one of the canonical lowercase `platform` values: `ios`,
`android`, `mac`, `windows`, or `linux`:

```json
{
  "ok": true,
  "data": {
    "status": "ok",
    "bridge_type": "vphone_ios",
    "platform": "ios"
  }
}
```

Benchmark orchestration uses this field for environment discovery, task
filtering, and platform consistency checks. When benchmark starts an agent
daemon, it passes the resolved platform through the process-local
`--device-type` option. The daemon applies it as a process-local override of
`[basic_settings.device].device_type` without modifying `agent.toml`, and reports the effective
value as `device_type`. A pre-started external daemon must receive the same
option from its caller. For compatibility with older bridges, known
`bridge_type` values may still be used as a fallback.

### `GET /api/tools` (compatibility)

Returns the bridge's legacy tool catalog for benchmark/debug clients. The Go
agent does not use this catalog; it wires its normal tools to the providers
below.

```json
{
  "tools": [
    {
      "name": "touch_gesture",
      "description": "Perform a touch gesture.",
      "args_schema": {"type": "object"}
    }
  ]
}
```

### `POST /api/tools/{tool_name}` (compatibility)

Invokes a legacy bridge tool. The request body accepts either structured `input`
or string `raw_input`. This endpoint is not the Go agent's EnvironmentBridge
transport.

```json
{"input": {"type": "tap", "point": {"x": 500, "y": 800}}}
```

The response should follow the Go agent tool response shape:

```json
{
  "tool": {"name": "touch_gesture"},
  "raw_input": "{\"type\":\"tap\"}",
  "output": "{\"ok\":true}",
  "is_error": false,
  "duration_ms": 42
}
```

### `POST /api/providers/screenshot`

Returns a raw screen frame. The runner uses this for `pre.jpg` / `post.jpg`,
and the agent uses it for its local `screenshot` tool. This is a provider call,
not a legacy tool invocation: when environment-bridge mode is on, the agent keeps running
`screenshot` locally and captures through this provider.

```json
{
  "format": "jpeg",
  "quality": 80,
  "crop_black": true,
  "minimal_width": 608
}
```

```json
{
  "ok": true,
  "data": {
    "meta": {
      "seq": 12,
      "width": 1080,
      "height": 2400,
      "source_width": 1080,
      "source_height": 2400,
      "crop_x": 0,
      "crop_y": 0,
      "crop_width": 1080,
      "crop_height": 2400,
      "pixel_format": "jpeg",
      "stride": 0,
      "bytes": 184320,
      "stale": false
    },
    "capture_info": {
      "capture_backend": "adb"
    },
    "image": "base64..."
  }
}
```

Bridges may ignore unsupported capture options such as `raw` or `crop_black`
and return the format they actually produced in `meta.pixel_format`.

Clients and bridges treat a single capture as a 30 second operation. The agent
HTTP provider, runner pre/post capture, and `POST /api/providers/screenshot`
handlers share that budget so a hung env fails at the caller instead of
continuing after the client has already given up. Action and setup requests
may use a longer timeout.

### `POST /state` and `POST /route`

Environments that support deterministic state judging may expose a final state
snapshot and the foreground route. Both requests use an empty JSON object and
the same `benchmark-task-id` header as setup and provider calls.

```json
{"ok": true, "data": {"apps": {"scroll_lab": {"selectedItemId": "scroll-item-083"}}}}
```

```json
{"ok": true, "data": {"app": "scroll_lab", "path": "/item/scroll-item-083"}}
```

The runner reads these endpoints only when a task declares
`environment_assertions`. It combines both payloads so assertions can use dotted
paths such as `apps.scroll_lab.selectedItemId` and `route.path`. A scalar value
is compared by exact equality. An object whose keys are a subset of
`{"min", "max"}` is treated as an inclusive numeric band, so a suite can assert
a measured position without hard-coding one exact value:

```json
{
  "apps.scroll_lab.firstVisibleOrdinal": {"min": 4, "max": 10}
}
```

A state read failure is reported as `judge_error`; a successfully-read value
mismatch is a deterministic task failure.

### `POST /api/providers/mnk`

Executes one mouse/keyboard provider operation. Requests use the normalized
0-1000 coordinate system and match the Go `mnk.Provider` interface:
`click`, `double_click`, `swipe`, `drag`, `keypress`, `move`, and `scroll`.

```json
{
  "operation": "swipe",
  "swipe": {
    "path": [[500, 800], [500, 400]],
    "button": "left",
    "duration_ms": 1200
  }
}
```

`swipe` and `drag` preserve a positive `duration_ms` through to the environment
gesture. The optional value must be an integer in `[0, 10000]`; zero or omission
keeps the backend's default timing. The Go agent also uses this field to carry
the duration calculated from its `speed` argument, so the bridge preserves the
agent's selected timing instead of replacing it with a backend default.

Success is `{"success": true}`. Invalid requests return HTTP 400 and device
execution failures return HTTP 500 with a top-level `error` string.

### `POST /api/setup`

Initializes or resets the environment route for a benchmark task.

Optional request body:

```json
{
  "episode_id": "task-episode",
  "setup_token": "optional-idempotency-token",
  "app_ids": ["settings"],
  "foreground_app_id": "settings"
}
```

`app_ids` is optional environment metadata. MobileGym uses it to preload only
the named apps; an omitted or empty list skips eager loading. Apps still load
their own data when opened. `foreground_app_id` is an optional exact app ID to
open after reset; it is not inferred from the preload list. A failed launch
fails setup.

For bridges that do not need setup, return success with `setup: false`.

`setup_token` is optional. When present, a bridge runs at most one concurrent
setup operation for the same `benchmark-task-id` and token. Concurrent
duplicates wait for the original operation, and successful results are replayed
without another reset. Failed operations are not cached, so a later request may
retry. Calls without a token retain the traditional behavior: every request
performs setup/reset. The cache is process-local, so a restarted bridge may
execute the same token once to recreate a lost route.
After `/api/release`, a client that starts a new logical session must use a new
token. Bridges may discard completed token entries for the released task, so an
old token no longer identifies an idempotent operation after release.

### `POST /api/release`

Releases any resources claimed by a benchmark task.

```json
{"ok": true, "data": {"released": true}}
```

### `GET /api/concurrent`

Returns the number of task workers this bridge can support at the same time.

```json
{
  "ok": true,
  "data": {
    "bridge_type": "mobilegym",
    "concurrent": 5
  }
}
```

Schedulers should treat missing or invalid values as `1`.

## Task Routing

Concurrent bridges must route all task-scoped requests by the
`benchmark-task-id` HTTP header. Runners and agent daemons send the same id for:

- `/api/setup`
- `/api/providers/screenshot`
- `/api/providers/mnk`
- `/api/release`

The bridge should keep requests with the same `benchmark-task-id` on the same
underlying env until `/api/release` is called.

## Agent Daemon Mode

Start the Go agent with screen and MNK providers backed by an environment
bridge. Normal agent tools remain local wrappers around these providers.

```bash
daemon \
  -dir /config \
  -addr 0.0.0.0:8080 \
  --device-type android \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://bridge:9090 \
  --benchmark-task-id suite.json:task-1
```

The agent still exposes its normal `/api/chat` endpoint. The agent also exposes
`POST /api/providers/screenshot` when it is itself used as a bridge.

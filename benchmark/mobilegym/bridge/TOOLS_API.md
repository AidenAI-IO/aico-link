# MobileGym Environment Bridge API

MobileGym Bridge Server implements the benchmark environment bridge protocol.
The same protocol is also exposed by the Go agent when it is used as a real
device bridge.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /api/tools` | List legacy tools exposed by the environment. |
| `POST /api/tools/{tool_name}` | Invoke a legacy environment tool. |
| `POST /api/providers/screenshot` | Return the current screen frame for pre/post capture and the agent screenshot tool. |
| `POST /api/providers/mnk` | Execute a Go `mnk.Provider` operation for agent input. |
| `POST /api/setup` | Initialize or reset a task route. |
| `POST /state` | Return the environment state snapshot for deterministic assertions. |
| `POST /route` | Return the foreground app and in-app route. |
| `POST /api/release` | Release a task route. |
| `GET /api/concurrent` | Return bridge concurrency capacity. |

MobileGym routes concurrent tasks by the `benchmark-task-id` header. The same id
must be sent to `/api/setup`, `/api/providers/screenshot`, `/api/providers/mnk`,
`/state`, `/route`, and `/api/release` for a task worker.

## Tool Catalog

```bash
curl http://localhost:8888/api/tools
```

The response contains a `tools` array. Each entry includes `name`,
`description`, and `args_schema`.

## Tool Invocation

```bash
curl -X POST http://localhost:8888/api/tools/touch_gesture \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{"input": {"type": "tap", "point": {"x": 500, "y": 800}}}'
```

Request bodies may use either structured `input` or string `raw_input`:

```json
{"input": {"text": "hello world"}}
```

```json
{"raw_input": "{\"text\":\"hello world\"}"}
```

The response matches Go agent `ToolInvokeResponse`:

```json
{
  "tool": {"name": "keyboard_text"},
  "raw_input": "{\"text\":\"hello world\"}",
  "output": "{\"action_output\":\"ok\"}",
  "is_error": false,
  "duration_ms": 123
}
```

## Screen Capture

```bash
curl -X POST http://localhost:8888/api/providers/screenshot \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{"format": "jpeg", "quality": 80}'
```

The benchmark runner uses this endpoint to save `pre.jpg` and `post.jpg`.

## MNK Provider

The Go agent sends normalized 0-1000 click, double-click, swipe, drag, keypress,
move, and vertical scroll operations to this endpoint. A successful request
returns `{"success": true}`.

```bash
curl -X POST http://localhost:8888/api/providers/mnk \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{"operation":"click","click":{"x":500,"y":800,"button":"left","hold_ms":0}}'
```

## Setup And Release

```bash
curl -X POST http://localhost:8888/api/setup \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{"app_ids":["settings"],"foreground_app_id":"settings"}'

curl -X POST http://localhost:8888/api/release \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{}'
```

`/api/setup` claims an env route and resets it. `/api/release` frees that route
for later tasks.

`app_ids` is optional. Missing or empty `app_ids` skips eager app data loading;
non-empty lists preload only the named apps. The app launch path still loads an
app's data on demand.

`foreground_app_id` is an optional exact installed app ID. Setup opens it after
reset and fails if it cannot be launched. The preload list does not determine
the foreground app. The agent's Phone Bridge tools (`open_app`, `bridge_*`)
are not routed through MobileGym's `/api/tools` catalog; they use the
`/api/phone-bridge/*` endpoints when setup includes `aico_app` (see
[Simulated Aico App](../README.md#simulated-aico-app)).

## Deterministic State And Route

Suites can judge a UI action by exact state instead of relying only on the final
screenshot. Both endpoints accept an empty JSON object and require the task's
route header:

```bash
curl -X POST http://localhost:8888/state \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{}'

curl -X POST http://localhost:8888/route \
  -H "Content-Type: application/json" \
  -H "benchmark-task-id: suite.json:task-1" \
  -d '{}'
```

For example, `mobilegym_scroll_regression.json` checks both the selected record
and its detail route:

```json
{
  "environment_assertions": {
    "route.path": "/item/scroll-item-083",
    "apps.scroll_lab.selectedItemId": "scroll-item-083"
  }
}
```

Scroll Lab also exposes its live scroll trajectory: `apps.scroll_lab.scrollTop`
is the scroll container offset in CSS pixels, `apps.scroll_lab.firstVisibleOrdinal`
is the first row still visible in the viewport, and
`apps.scroll_lab.maxFirstVisibleOrdinal` is the monotonic high-water mark of that
value for the episode. Because a scalar assertion must match exactly, a numeric
band is expressed as an object with `min`/`max` keys:

```json
{
  "environment_assertions": {
    "apps.scroll_lab.maxFirstVisibleOrdinal": {"max": 23}
  }
}
```

`mobilegym_scroll_regression.json` checks ordinals 24 and 83. The optional
`mobilegym_scroll_sweep.json` applies that bound to every row for calibration.
The high-water mark survives a scroll-back, so a recovered overshoot still fails.

## Concurrency

```bash
curl http://localhost:8888/api/concurrent
```

MobileGym returns the env pool size:

```json
{
  "ok": true,
  "data": {
    "bridge_type": "mobilegym",
    "concurrent": 5,
    "env_count": 5,
    "active_routes": {}
  }
}
```

The Go agent bridge returns `concurrent: 1`.

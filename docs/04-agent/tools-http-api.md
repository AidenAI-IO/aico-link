---
sidebar_position: 3
---

# Tools HTTP API

In every input mode, the Agent exposes Agent-owned tools that can be safely invoked via HTTP for the browser Tool Lab, external agents, or manual calls. Internal maintenance tools (such as `skill_manage`) are not exposed via HTTP.

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/tools` | List HTTP-visible tools with descriptions, input modes, examples, and HTTP bindings |
| `POST` | `/api/tools/{tool_name}` | Invoke the specified tool |
| `GET` | `/api/tool-skills` | Generate a `SKILL.md` bundle suitable for external agents |

## Request Format

JSON object input:

```json
{
  "input": {"command": "pwd"}
}
```

Raw string input:

```json
{
  "raw_input": "{\"command\":\"pwd\"}"
}
```

Use `raw_input` when you need to pass a raw string. Most tools (including skill tools) describe their input with JSON examples in the catalog:

```json
{
  "raw_input": "planner"
}
```

## Response Format

```json
{
  "tool": {
    "name": "shell",
    "category": "system",
    "description": "...",
    "input_mode": "json",
    "example_input": "{\"command\":\"pwd\"}",
    "args_schema": {
      "type": "object",
      "properties": {
        "command": {"type": "string"}
      },
      "additionalProperties": false
    },
    "http": {
      "method": "POST",
      "path": "/api/tools/shell"
    }
  },
  "raw_input": "{\"command\":\"pwd\"}",
  "output": "...",
  "is_error": false,
  "duration_ms": 12,
  "called_at": "2026-05-18T12:34:56Z"
}
```

Tool execution failures are also returned in JSON format. Check:

- `is_error`
- Whether `output` contains error information
- Whether the HTTP transport succeeded

## Catalog Scope

The HTTP catalog is generated from registered Agent-owned tools at runtime. It can include diagnostic, browser Tool Lab, and external-agent tools that are intentionally absent from the default conversational Agent prompt.

`current_time` and `calculator` are not registered and therefore do not appear in either the conversational or HTTP tool catalogs. Use `shell` for controller-local precise time, timezone, and deterministic calculations.

## Platform-Specific Conversational Catalog

The conversational Agent tool catalog is filtered by the global `[basic_settings.device].device_type` state. Valid configured device type values are `iOS`, `Android`, `macOS`, `windows`, and `linux`; the runtime derives lowercase platform identifiers (`ios`, `android`, `macos`, `windows`, `linux`) from those values for tool-catalog filtering. Tools with no platform metadata are treated as portable.

Platform-specific tools should stay registered and HTTP-visible for Tool Lab and manual diagnostics unless they are unsafe or impossible to invoke directly. The platform split applies to the model-facing catalog so the model is not taught tools that cannot run for the current device.

When adding or changing a tool, use this rule:

- Prefer one semantic tool that reads runtime `device_type` internally when the capability is the same across platforms.
- Add platform metadata in `builtInToolSpecMetadata` when the tool cannot run on every configured device type, its schema or description would imply the wrong platform behavior, or the supported action set is materially different.
- In `builtInToolSpecMetadata`, use derived platform identifiers: `ios` and `android` for phone-companion capabilities, `macos` for Mac desktop bindings, and add `windows` or `linux` only after the tool has verified bindings for those platforms. Do not use these lowercase identifiers as documented `[basic_settings.device].device_type` values.
- Do not ask the model to pass platform/device/os arguments for platform selection; the runtime derives the platform from global `device_type` state.
- If a tool has platform-specific subcommands, make its runtime `ArgsSchema()` list only the subcommands active for the current `device_type` (for example `quick_action.action` and `touch_gesture.type`).
- When a nominally platform-specific namespace has a portable subset, expose only that subset on other platforms instead of hiding the namespace entirely; for example non-Android `keyboard_tap` may list only the absolute pointer-mode `KEYCODE_*` media, volume, screenshot, and brightness aliases.
- Add focused tests for `AgentToolsForPlatform`, `Runtime.availableTools()`, or platform-specific tool schemas whenever a tool is introduced or moved between platform groups.

Once an HID-affecting tool invocation is accepted, its execution is independent
of the client socket and has a five-minute server-side timeout. This is important
when the client reaches the board through USB ECM: iOS keyboard profile isolation
briefly re-enumerates the USB composite and may drop the HTTP connection, but the
tool continues running and still performs its final HID profile restore. The
caller may need to reconnect and observe the resulting screen if the response
socket was lost. This applies to keyboard, pointer/touch, composite text/search,
and quick-action tools that can participate in the serialized HID flow.
Within one HTTP invocation, consecutive modifier-bearing actions share one
pointer-free phase; pointer/touch input restores the mouse before acting.
Separate HTTP invocations each manage their own isolation scope. Other HTTP
tools retain normal client-cancellation behavior.

The HTTP catalog is a separate policy. It exposes registered operational and specialized tools together with their `args_schema`, but internal maintenance tools such as `skill_manage` and `skill_mark_used` are never listed or callable through the default HTTP Tool API.

For app launch requests, inspect the latest screenshot first. If the requested app icon or card is clearly visible, unique, and unobscured, use `touch_gesture` on its visible non-overlapping center instead of `open_app`; use `open_app` only when the target is not clearly tappable or a direct tap did not produce a verified result. Both `open_app` and `open_url` return a post-action screenshot; always inspect it before answering or continuing. `ok:true` only means that the OS accepted the launch request, not that the requested app or page is visible.

The Android visible-UI fallback starts with the remembered launcher search entry,
or the app drawer on first use. Its entry screenshot check also looks for the
requested app icon. A clearly identified launcher icon is tapped directly and
the resulting app screen is verified before success is returned. If no target
icon is visible, the existing search-field and alternative-entry flow continues.
This avoids searching for an app that is already visible in the drawer.

Conversational `touch_gesture` calls with missing standard-gesture fields are
rejected before baseline capture. A swipe must include `type:"swipe"`, `start`,
and either `end` or `direction` together. Every supplied `point`, `start`, or
`end` must be an object containing both numeric `x` and `y`; zero is valid,
while missing or null coordinates are rejected before capture. The error
includes complete tap and swipe examples so a retry can retain all required
fields.

## curl Examples

```bash
curl http://127.0.0.1:8080/api/tools

curl -X POST http://127.0.0.1:8080/api/tools/shell \
  -H 'Content-Type: application/json' \
  -d '{"input":{"command":"pwd"}}'

curl -X POST http://127.0.0.1:8080/api/tools/keyboard_text \
  -H 'Content-Type: application/json' \
  -d '{"input":{"text":"hello from API"}}'

curl -X POST http://127.0.0.1:8080/api/tools/screenshot \
  -H 'Content-Type: application/json' \
  -d '{"input":{}}'

curl -X POST http://127.0.0.1:8080/api/tools/weather \
  -H 'Content-Type: application/json' \
  -d '{"input":{"location":"Shanghai"}}'
```

A successful `screenshot` output typically includes `width`, `height`, `format`, `size`, and base64 JPEG `data`.
A successful `wait_for_stable_screen` output includes stability fields `ok`, `stable`, `elapsed_ms`, `screen_changed`, and also returns a screenshot with `width`, `height`, `format`, `size`, and base64 JPEG `data`; `screen_changed=false` means no visible frame change was observed during the wait window, while `stable=false` indicates the screen is still changing but the screenshot can still be used as a current observation.
Before executing `keyboard_tap`, `mouse_move`, `mouse_scroll`, or `touch_gesture`, the system captures a best-effort baseline. After successful execution it waits for screen stability (or until timeout) and automatically takes a final screenshot. Their `output` is JSON containing the original action result `action_output`, `screen_stable`, `stable_wait_ms`, optional `screen_changed`, and the final screenshot's `width`, `height`, `format`, `size`, and base64 JPEG `data`. `enter_text` also captures a best-effort baseline and returns a short-delay post-action screenshot, but does not perform the stable-screen wait; its `screen_stable` and `stable_wait_ms` fields are omitted. When present, post-action `screen_changed` compares the baseline with the final screenshot using the PR #442 structural detector: it ignores the top 8% of the image, averages a 40 by 80 block grid, and requires more than 1% of blocks to exceed the color-difference threshold. If the best-effort baseline was unavailable, `screen_changed` is omitted; treat `screen_changed=false` as no meaningful change only when the field is present. `last_diff`, when present, remains a stability-wait diagnostic.
For `touch_gesture`, use the standard `type` syntax for normal taps, long presses, swipes, scrolling, and draggable targets. Atomic `actions` (`touch_down`, `move_to`, `wait`, `touch_up`) are a low-frequency advanced option only for uninterrupted custom contact timing that no standard gesture type can express; actions run as one contact-preserving input program, and every sequence that starts `touch_down` must finish with `touch_up`. Atomic `move_to` accepts optional `speed` in normalized coordinate units per second; its duration is calculated from the preceding point, while an explicit `duration_ms` takes precedence. Omitting both preserves immediate movement. Moving a draggable target must never use atomic actions. Call `drag_start` at the current target point; it performs its screen-stability wait internally while preserving contact and returns the final screenshot with `screen_stable`. When `screen_stable=true`, confirm the destination from that screenshot and call `drag_release` at the confirmed point. When `screen_stable=false`, runtime automatically moves back to the original `drag_start` point and releases the contact; inspect the returned screenshot and retry the complete drag flow instead of calling `drag_release`. Do not call `wait_for_stable_screen` separately in the normal drag flow or determine a destination from a `screen_stable=false` result. The start call holds for 500ms, then moves 200 normalized units at 500 normalized units per second (a 400ms interpolated move) without releasing when the screen becomes stable; the release call moves directly, holds for 200ms, then releases. The former one-call `type:"drag"` is unsupported. Parameterized swipes use `type:"swipe"`: provide `start` plus either `end` or `direction`; `speed` defaults to 2500 normalized coordinate units per second. `duration_ms` is optional: it overrides timing for `start` + `end`, or controls travel for `start` + `direction` (`speed * duration_ms / 1000`). Without it, a directional swipe travels to the corresponding edge. `hold_before_ms` and `hold_after_ms` are optional dwell times in milliseconds before movement and before release (default 0). `steps` optionally sets the HID interpolation count (default 24; larger values make motion smoother). Back/home fallbacks are explicit edge-aligned swipes, e.g. `start.x=1` or `start.y=999`.
`weather` supports location names or latitude/longitude coordinates, fetching geocoding, current weather, and short-term forecasts from Open-Meteo at runtime.
`wait_for_wakeup` is a terminating runtime tool. After a successful tool call, it immediately ends the current Agent run and returns the voice interaction to waiting for the next wakeup; it does not ask the model to provide an additional final answer. The run result will set `wait_for_wakeup_requested` / `wait_for_wakeup_reason`; the old fields `sleep_requested` / `sleep_reason` are retained as compatibility aliases only.
## Recommendations for External Agents

- Prioritize capability discovery via `GET /api/tools`
- When screen operations are needed, `screenshot` first, then click/input
- After successful click/input actions, directly check the post-action screenshot returned by that tool; no need to immediately call `screenshot` again
- For mouse and touch, use normalized 0-1000 coordinates
- When accessing via private IP or USB network adapter, note proxy bypass: set `NO_PROXY` / `no_proxy`
- For long-running `shell` tasks, use background sessions as described in the tool documentation, and stop them when done
- When the user requests "sleep / stop listening / wait for my next wakeup", use `wait_for_wakeup` rather than pretending to return to waiting for wakeup with a normal text reply

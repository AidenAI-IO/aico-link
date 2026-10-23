---
sidebar_position: 13
---

# Live Activity / Dynamic Island

Live Activity is Aico's iOS task-status panel and a system entry point back to
the companion app. Task state stays local to the board and phone. Aico does
not register ActivityKit push tokens, publish task state to an Aico-hosted backend,
or use APNs for Live Activity updates.

## Local Update Flow

Foreground and background updates read the same Agent snapshot:

```text
Agent task state changes
        |
        | coalesced BLE Wake (reason=live_activity, no task text)
        v
iOS CoreBluetooth wakes the companion app briefly
        |
        | USB ECM HTTP GET /api/live-activity/current?phone_id=<stable>
        v
Swift native layer parses the latest snapshot
        |
        v
Activity.request / Activity.update
```

The BLE value remains the 12-byte Wake envelope documented in
[BLE Service](../03-services/ble-service.md). It contains only protocol version,
reason, and a monotonic sequence. Task titles, steps, targets, errors, and
results are never carried in the GATT notification. The phone reads those fields
from `192.168.42.1` over the USB ECM link.

The Agent coalesces state-change Wake notifications to at most roughly one every
750 ms. This keeps step updates responsive without sending one BLE notification
per streamed token or character. The iOS side always fetches the newest
snapshot, so skipped intermediate Wake notifications do not lose final state.

## Behavior

- `ready` / `connected`: Aico is available. The app creates this standby Live
  Activity before or while entering background so Dynamic Island remains an
  entry point back to the Aico companion app.
- `running`: shows task title, current step, phase, progress, stop state, and
  the detailed `tool_status` lifecycle when available.
- `needs_app`: asks the user to return to the Aico companion app when an operation needs the
  foreground companion app.
- `waiting_user` with `current_action=request_user_input`: keeps the handoff
  instruction visible after `request_user_action` so the user can take over
  on the phone.
- `completed` / `failed` / `canceled`: keeps the terminal text visible in the
  existing Live Activity instead of immediately replacing it with standby.

The app requests a one-time system alert only for high-value transitions:
initial USB ECM connection, human handoff, foreground companion-app recovery,
completion, and failure. Ordinary tool calls, progress changes, and repeated
snapshots update silently. Alerts are deduplicated by request and transition;
an explicit new USB connection session receives a new connection event ID.

Tasks started in the App, Agent Web UI, or another local Agent entry point use
the same snapshot and local update path.

## Privacy and Availability Boundaries

- No ActivityKit push token or push-to-start token is requested or registered.
- The Agent contains no relay, registration, or direct APNs publisher path.
- BLE carries no task content. The content-bearing request stays on the USB ECM
  subnet and is restricted to the phone connected to the board.
- Live Activity does not grant continuous iOS background execution. BLE Wake
  opens only a short CoreBluetooth-triggered execution opportunity.
- A reliable background refresh requires an active BLE Wake subscription and a
  working USB ECM path to `192.168.42.1:8080`.
- The app should create a standby Live Activity before suspension. If the user
  force-quits the app, removes Bluetooth permission, disconnects USB, or iOS
  declines background execution, updates wait until the app is opened again.
- There is intentionally no APNs fallback and no remote push-to-start path.

## State Model

When an async chat request provides a `request_id`, the Agent maintains a
`live_activity` snapshot for that request:

```json
{
  "request_id": "chat_...",
  "status": "running",
  "phase": "observing",
  "task_title": "Open Settings",
  "current_step": "Checking the current screen",
  "current_action": "observe_screen",
  "current_target": "",
  "last_tool_name": "screenshot",
  "tool_status": "running",
  "tool_started_at": "2026-06-12T07:00:01Z",
  "progress": 0.21,
  "shows_progress": true,
  "can_stop": true,
  "requires_app": false,
  "started_at": "2026-06-12T07:00:00Z",
  "updated_at": "2026-06-12T07:00:02Z"
}
```

Status values:

- `running`: task is executing.
- `needs_app`: the user must return to the companion app.
- `completed`: task completed.
- `failed`: task failed; `last_error` may contain a display-safe summary.
- `canceled`: the request was canceled or interrupted.

Structured fields include:

- `phase`: `planning`, `observing`, `acting`, `phone_bridge`, `waiting_app`,
  `waiting_user`, `verifying`, or `answering`.
- `current_action`: machine-readable action such as `open_app`,
  `observe_screen`, `control_phone`, `clipboard`, or `calendar`.
- `current_target`: a short display target. Sensitive input should not be put in
  this field.
- `current_app`: the current target app when known.
- `requires_app`: whether the user must restore the foreground companion app.
- `tool_status`: detailed lifecycle state. Current values are `processing`,
  `thinking`, `running`, `preparing`, `verifying`, `succeeded`, `failed`,
  `retrying`, `waiting_app`, and `waiting_user`. Task-level `status` remains
  authoritative for terminal state.
- `tool_started_at`: optional start time for the current actively timed tool.
  The iOS client uses it to render elapsed time locally and clears the timer
  for non-timed and terminal states.

Ordinary model output uses `tool_status=processing`. Only a non-empty streamed
reasoning delta changes it to `thinking`; the Live Activity uses safe fixed
copy and does not display the raw reasoning stream. Successful app-launch
requests use `verifying` until a later screen observation can establish that
the target app is visible.

`phone_app_state` events additionally report `return_entry` and
`return_entry_available`. When the iOS app is backgrounded or inactive, it can
report `return_entry="dynamic_island"` so board-side recovery logic knows that a
visible companion-app entry may return the user to the App.

## API

### Query One Request

```http
GET /api/live-activity/status?request_id=<id>
```

Response:

```json
{
  "status": "ok",
  "live_activity": {
    "request_id": "chat_...",
    "status": "running",
    "task_title": "Open Settings",
    "current_step": "Checking the current screen",
    "progress": 0.21,
    "shows_progress": true,
    "can_stop": true
  }
}
```

### Query the Current Task

The native BLE Wake handler uses:

```http
GET /api/live-activity/current?phone_id=<stable-phone-id>
```

When no matching active or retained task exists:

```json
{"status":"not_found"}
```

`GET /api/chat/result?request_id=<id>&offset=<n>` also includes a
`live_activity` field so foreground chat polling can update ActivityKit through
the same local native module.

## Configuration

The only required Agent setting is that Live Activity snapshots remain enabled:

```toml
[advanced_settings.runtime.live_activity]
enabled = true
```

`enabled` is the complete Live Activity configuration surface. The Agent has no
backend URL, APNs credential, registration-token, or board-ID setting.

The iOS app may optionally set `LIVE_ACTIVITY_PHONE_ID`. When empty, the native
layer persists `identifierForVendor` as the stable phone ID used to filter the
board snapshot.

## Apple Developer Prerequisites

- Enable Live Activities for the app and include the
  `AidenLiveActivityExtension` Widget Extension.
- Push Notifications capability and APNs credentials are not required for this
  local-only flow.
- Enable the Bluetooth background mode required by the existing BLE Wake
  service and test on a real iPhone; the simulator cannot validate USB ECM or
  CoreBluetooth background delivery.

## Verification

1. Pair the iPhone with the board and confirm `wake_subscriber=true` from
   `/api/bluetooth/status`.
2. Connect the phone over USB and confirm it can reach
   `http://192.168.42.1:8080/api/live-activity/current`.
3. Background the app and confirm an `Aiden Ready` Live Activity already
   exists.
4. Start a task from Agent Web UI and confirm the card changes to `running`
   without reopening the app.
5. Trigger `request_user_action` and confirm the card alerts once, displays
   the suggested action, and remains in the handoff state after the Agent turn
   stops.
6. Confirm completed and failed text remains visible and alerts once; ordinary
   progress and tool updates must not alert.
7. Inspect App and Agent logs for `live_activity` Wake delivery and local board
   sync.
8. Query `/api/live-activity/current` from outside loopback and the USB ECM
   subnet and verify the request is rejected with HTTP `403`.

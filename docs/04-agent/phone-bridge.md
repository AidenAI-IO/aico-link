---
sidebar_position: 11
---

# Phone Bridge

## Purpose

# Purpose

The Aico companion app connects to the hardware board over USB ECM and executes phone-side operations available through public iOS and Android APIs.

The board remains responsible for screen observation, task planning, and HID fallback. Phone Bridge is a software fast path for operations such as launching apps, accessing the clipboard, and working with calendars, contacts, and notifications.

## Capabilities

| Capability | iOS | Android | Problem Solved |
| --- | --- | --- | --- |
| Open an app or URL | URL scheme / Universal Link / `openURL` | Package name / Intent / deep link | Avoid searching for an icon when the OS exposes a direct launch path. |
| Clipboard read/write | `UIPasteboard` | Clipboard API | Exchange text without UI-driven copy and paste. |
| Calendar | EventKit | Calendar provider | Query, create, and delete calendar events. |
| Contacts | Contacts framework | Contacts provider | Query, create, and update contacts. |
| Notification | Local notification and ANCS query | Local notification and Notification Access query | Deliver reminders and query the shared notification event ring. |
| Board communication | Foreground WebSocket with limited background recovery | Foreground WebSocket and FGS HTTP polling | Receive commands and return structured results. |

## Communication Method

The board has the fixed USB-network address `192.168.42.1`. The companion app acts as the client and connects to:

```text
Phone relay app -> ws://192.168.42.1:8080/api/phone-bridge
```

The board is the WebSocket server, so it does not need to discover the phone's DHCP address and the app does not need to expose a local server. The Agent HTTP APIs use the same port, `8080`.

The board also exposes `/api/phone-bridge/commands` and `/api/phone-bridge/results` HTTP queue endpoints, but React Native JS, WebSocket, and polling timers in the iOS background must not be treated as a general tool execution path. On iOS, Phone Bridge is normally a foreground fast path: if the Aico companion app is backgrounded and the app has reported `return_entry=dynamic_island`, Agent restores the Aico companion app through Dynamic Island, waits for foreground WebSocket bridge reconnection, then executes the requested tool command. Lock-screen Live Activity entries require visual confirmation rather than fixed-coordinate tapping.

PiP Bridge is a narrow exception. When the app reports `pip_bridge_enabled=true` while backgrounded, iOS gives PiP priority over the Dynamic Island, so the Dynamic Island return entry is not visible. `pip_bridge_enabled` is the authoritative PiP transport signal for background-safe data commands; ordinary foreground app-state timestamps are not an additional PiP expiry. The HTTP/Tool Lab catalog remains complete for direct diagnostics, while the conversational Agent catalog is filtered from live runtime capabilities before each run. `open_app` remains exposed because it can fall back to SearchLaunchApp; only executable background-safe data tools (`bridge_clipboard`, `bridge_calendar`, `bridge_contacts`, `bridge_notification`) are exposed through the HTTP queue, and unavailable App actions are omitted.

BLE Wake provides an on-demand iOS background route for a narrower command set: calendar create/query/delete, contacts query/create, and local notification send. Clipboard read/write is not reliable in the iOS background, and contacts update is not exposed through BLE Wake; those calls return an explicit `app_backgrounded` error unless foreground Phone Bridge, PiP, or Dynamic Island restoration provides another executable route. Before each Agent run, runtime capability filtering checks whether `ble_service` reports a connected Wake subscriber and exposes only executable tools and actions. The command is then placed in the existing HTTP queue and the board makes a best-effort `wake` call. BLE carries only the wake hint: the app polls with its `phone_id`, executes the native module, and posts the structured result over HTTP. The phone must therefore remain connected to the board's USB ECM network; BLE is not a replacement command transport. If BLE is unavailable, those App-only tools are omitted unless another route such as foreground Phone Bridge, PiP/FGS polling, or Dynamic Island restoration is usable; a failed Wake notify never removes an already queued command.

Android FGS Bridge follows the same HTTP queue contract without using WebSocket as a background transport. When the Android foreground service polls `/api/phone-bridge/commands` with `app_state=background` and `fgs_bridge_enabled=true`, the Agent keeps `open_app` unavailable and routes only background-safe data tools through the HTTP queue.

Android system-notification ingestion is a separate one-way path. After the
user grants Notification Access, the app's native listener posts bounded,
retry-safe event batches to `/api/phone-notifications/events` over USB ECM. The
Agent forwards them to the same `ble_service` event ring used by iOS ANCS;
Android does not need BLE pairing for this path.

## Queue And Clipboard Contract

`GET /api/phone-bridge/commands` returns `commands: []` when there are no eligible commands, including when platform, phone identity, or background capability filters exclude all queued commands. Clients can distinguish an empty queue from an invalid response without treating HTTP 200 as a schema failure.

For `bridge_clipboard`, `action: "write"` requires a `text` string. Missing, null, or non-string text is rejected before sending a phone command. An explicit empty string remains valid and clears the clipboard; `action: "read"` does not require text.

## Desktop Agent With ADB Reverse

When running the Agent on a development computer instead of the Luckfox board, the phone cannot reach `192.168.42.1` because the USB ECM board network does not exist. For Android development, use the ADB input backend and let the phone app connect through ADB reverse:

```text
Phone relay app -> ws://127.0.0.1:8080/api/phone-bridge
ADB reverse     -> host computer 127.0.0.1:8080
```

When `[advanced_settings.hardware.hid].input_backend = "adb"` is active, the desktop Agent attempts to configure the reverse mapping automatically at startup:

```bash
adb reverse tcp:8080 tcp:8080
```

The companion app keeps `192.168.42.1` as the first target, then falls back to the desktop ADB reverse target when the board API is unavailable. Android board-network binding is skipped for loopback URLs so the app can reach the ADB reverse socket.

For host microphone recording and TTS replies in this desktop/PC Agent mode, keep `audio.backend = "auto"` or set it to `"local"`. The Agent captures PCM through an installed host recorder command and plays temporary WAV files through the host OS player instead of `audio_service`.

## App Opening Flow

```text
Board HDMI sees screen / user issues task
        │
        ▼
AI identifies target app, e.g., "WeChat"
        │
        ▼
Look up mapping table
iOS: weixin://
Android: com.tencent.mm
        │
        ▼
Public open_app reads the live relay-app state:
If foreground Phone Bridge is ready:
    Send semantic open_app command
Otherwise:
    Use SearchLaunchApp through the visible system UI
        │
        ▼
Relay app executes:
iOS: openURL("weixin://")
Android: Intent launch package name
        │
        ├─ HDMI verification success -> continue next step
        └─ Failure / timeout -> fallback to hardware simulation: find icon coordinates + HID tap
```

## Key Boundaries

When `open_app` falls back to system search and iOS PiP is enabled, query
entry first writes fresh text through the background clipboard command and
pastes into the already-focused search field. The existing app-result lookup,
tap, and app-open confirmation then continue as before. If PiP is unavailable
or its clipboard/paste command fails, query entry uses the existing local
HID/IME path; a failed clipboard attempt clears the field before typing.

On iOS, the relay app is not a background-resident system agent. It's more like a foreground fast-path executor; Dynamic Island can be used as the automatic entry point back to the Aico companion app, while lock-screen Live Activity cards need visual confirmation:

```text
Aico companion app foreground
-> Receives board command
-> openURL opens WeChat
-> Aico companion app enters background
-> Subsequent operations continue via hardware HDMI observation + HID operation
```

So iOS cannot promise long-term background command reception. BLE Wake adds a
short on-demand HTTP Queue window for its allowlist, but it does not make the
App permanently resident. Android can be more stable through a foreground
service, with stronger background polling capability.

## iOS Key Points

- Don't use `canOpenURL` for large-scale pre-checking.
- Directly call `openURL(url)` to attempt opening.
- `LSApplicationQueriesSchemes` mainly limits `canOpenURL` queries, not dynamic `openURL` attempts.
- The companion app owns the current URL scheme/package/intent mapping; the
  board sends semantic targets.
- `openURL` returning success doesn't mean target page is fully usable; ultimately verify via HDMI visually.

## Android Key Points

- Prioritize launching by package name: `getLaunchIntentForPackage("com.tencent.mm")`.
- Android 11+ has package visibility restrictions, need to configure `<queries>`, or evaluate `QUERY_ALL_PACKAGES` for specific scenarios.
- Background Activity launching has system restrictions, but availability after foreground service, notifications, and user authorization is stronger than iOS.

## Companion App Implementation

The companion app is a bare React Native application. React Native owns the UI, WebSocket client, foreground command dispatch, and shared business logic. Native Swift and Kotlin/Java modules provide platform capabilities that are not exposed directly to JavaScript.

Important native areas include:

- iOS local-network permissions, URL launching, calendar, contacts, notifications, Live Activity, PiP Bridge, and BLE Wake integration.
- Android package and intent launching, package visibility, notifications, Notification Access, board-network binding, and foreground-service polling.

See the related `aiden-app` repository for the companion-app source and platform permission declarations. Its `package.json` is the source of truth for the current React Native and toolchain versions.

## Why WebSocket on Top of USB Network

USB ECM (`192.168.42.1`) and WebSocket are two layers:

| Layer | Function |
| --- | --- |
| USB ECM | Network link — enables IP packet communication between phone and board |
| WebSocket | Application protocol — transmits commands and acknowledgments on this link |

USB ECM is "road is built", WebSocket is "vehicles running on the road".

The existing `192.168.42.1:80` config page is HTTP request-response mode, suitable for human web operations. But the board needs to **actively push commands to phone app** (like "open WeChat now"), which HTTP cannot do — HTTP is client-initiated, server cannot actively talk to client.

WebSocket's core value:

- **Bidirectional real-time**: Board can push commands to app anytime, app can reply anytime
- **Long connection**: No need to rebuild connection each time, low latency
- **State awareness**: Connected means online, disconnection immediately known, triggers HID fallback

## Implemented Runtime Flow

1. The relay app connects to `ws://192.168.42.1:8080/api/phone-bridge`
   after startup and sends periodic heartbeats.
2. The app reports `phone_environment` after connection and foreground return,
   plus `phone_app_state` whenever its visible lifecycle changes. Android FGS
   Bridge reports `fgs_bridge_enabled` through HTTP queue polling.
3. The board maintains `bridge_connected`, `platform`, `last_heartbeat_at`,
   `app_state`, return-entry and background-bridge fields, plus the latest
   environment snapshot. `hid_connection_id` identifies the current physical
   USB HID session from the board UDC state. It remains stable across
   WebSocket reconnects and HTTP/FGS fallback, and changes only after the USB
   host disconnects and reconnects.
4. Before each conversational run, the Agent filters App-only tools and actions
   using foreground Phone Bridge, Dynamic Island restore, PiP/FGS polling, BLE
   Wake, and BLE notification-query capabilities.

`bridge_connected` only means the WebSocket is currently active. It is not equivalent to USB cable connectivity. After the iOS app enters background, WebSocket may disconnect while USB ECM remains reachable; Dynamic Island updates use a local BLE Wake followed by a USB ECM read of `/api/live-activity/current`, not the phone bridge WebSocket or a remote relay.

The Agent associates cached phone screen dimensions with
`hid_connection_id`. A heartbeat timeout therefore does not discard the
dimensions while the same USB HID session remains attached. A physical USB
disconnect invalidates the cache before a new host session can reuse it.

When `app_state=background|inactive`, `return_entry=dynamic_island`,
`return_entry_available=true`, and PiP Bridge mode is not enabled, `open_url`
and bridge data tools can click the Aico companion app Dynamic Island entry, wait for Phone
Bridge recovery, then send their commands. `open_app` instead selects
SearchLaunchApp whenever foreground Bridge app launch is unavailable.
Lock-screen Live Activity entries are not blind-tapped because their screen
position is not stable; use screenshot/HID fallback or visual confirmation
instead. When `pip_bridge_enabled=true` on iOS or `fgs_bridge_enabled=true` on
Android in the background, the generic background-safe data commands use the
HTTP queue. With an authenticated iOS Wake subscriber, the narrower BLE Wake
allowlist uses that same queue after a non-sensitive GATT hint.

### Command Protocol

Board sends `BridgeCommand` to app via WebSocket, app executes and replies with `BridgeCommandResponse`.

#### Common Fields

**BridgeCommand (board → app)**:
```json
{
  "id": "cmd_001",
  "type": "open_app | clipboard_read | clipboard_write | calendar_* | contacts_* | notification_send",
  "timeout_ms": 5000,
  "payload": { }  // Optional, command-related JSON (clipboard text, calendar event, etc.)
}
```

**BridgeCommandResponse (app → board)**:
```json
{
  "id": "cmd_001",
  "method": "calendar_create",
  "data": {"event_id": "event_123"}
}
```

Failure example:

```json
{
  "id": "cmd_001",
  "error": {
    "category": "user_action_required",
    "code": "permission_denied",
    "message": "Calendar access not granted"
  }
}
```

`error` is omitted on success. The current wire schema does not include an
`ok` boolean; `method` and `data` are optional success fields.

**App Active Event (app → board)**:
```json
{
  "id": "phone_environment",
  "method": "phone_environment",
  "data": {
    "platform": "ios",
    "system_name": "iOS",
    "system_version": "18.5",
    "locale": "zh-Hans-CN",
    "time_zone": "Asia/Shanghai",
    "utc_offset": "+08:00",
    "manufacturer": "Apple",
    "model": "iPhone16,2",
    "screen": {"width_pixels": 1179, "height_pixels": 2556, "scale": 3},
    "battery": {"level": 0.87, "charging": true},
    "system_apps": [{"name": "Camera", "available": true, "category": "system", "availability_source": "builtin"}],
    "third_party_apps": [{"name": "WeChat", "available": true, "category": "third_party", "availability_source": "can_open_url"}],
    "available_apps": [{"name": "WeChat", "available": true, "category": "third_party"}]
  }
}
```

`phone_environment` does not correspond to board command ID; board only updates bridge status, won't treat it as tool call acknowledgment.
`system_apps` is system built-in app/capability list; `third_party_apps` is installation/openability probe result. `available_apps` retained only as legacy field for old boards.

#### Command Types

##### 1. `open_app` — Open App

```json
{
  "id": "open_001",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

Reply:
```json
{
  "id": "open_001",
  "method": "ios_url_scheme"
}
```

App-side `method` represents the underlying mechanism (for example `ios_url_scheme`, `ios_shortcut`, `android_intent`, `android_deeplink`, `launch_package`, or `open_url`). The public `open_app` tool routes semantic app launches to the internal Phone Bridge launcher or visible system search. The public `open_url` tool sends `http`, `https`, `sms`, `mailto`, and `tel` URLs through Phone Bridge. Their results normalize task semantics into `method:"open_app"` or `method:"open_url"`, while the underlying app-side method is returned as `mechanism`.

App-launch and URL semantics are separated: call `open_app` with `{"app":"browser"}` to launch the browser itself, and call `open_url` with a supported URL such as `{"url":"https://example.com"}` or `{"url":"tel:+15551234567"}`. The companion app owns platform-specific URL/package/intent mapping.

##### 2. `clipboard_read` — Read Clipboard

```json
{
  "id": "clip_read_001",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

Reply:
```json
{
  "id": "clip_read_001",
  "data": {
    "text": "clipboard content"
  }
}
```

##### 3. `clipboard_write` — Write Clipboard

```json
{
  "id": "clip_write_001",
  "type": "clipboard_write",
  "payload": {
    "text": "content to copy"
  },
  "timeout_ms": 5000
}
```

Reply:
```json
{
  "id": "clip_write_001",
  "method": "clipboard_write"
}
```

##### 4. `calendar_create` — Create Calendar Event

```json
{
  "id": "cal_create_001",
  "type": "calendar_create",
  "payload": {
    "title": "Dentist appointment",
    "start_at": "2026-06-02T15:00:00+08:00",
    "end_at": "2026-06-02T16:00:00+08:00",
    "all_day": false,
    "location": "Clinic",
    "notes": "Bring insurance card"
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "cal_create_001",
  "data": {
    "event_id": "ios_calendar_id_123"
  }
}
```

##### 5. `calendar_query` — Query Calendar Events

```json
{
  "id": "cal_query_001",
  "type": "calendar_query",
  "payload": {
    "from": "2026-06-02T00:00:00+08:00",
    "to": "2026-06-03T00:00:00+08:00"
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "cal_query_001",
  "data": {
    "events": [
      {
        "event_id": "...",
        "title": "Dentist appointment",
        "start_at": "2026-06-02T15:00:00+08:00",
        "end_at": "2026-06-02T16:00:00+08:00",
        "location": "Clinic"
      }
    ]
  }
}
```

##### 6. `calendar_delete` — Delete Calendar Event

```json
{
  "id": "cal_delete_001",
  "type": "calendar_delete",
  "payload": {
    "event_id": "ios_calendar_id_123"
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "cal_delete_001",
  "method": "calendar_delete"
}
```

##### 7. `contacts_query` — Query Contacts

```json
{
  "id": "contacts_query_001",
  "type": "contacts_query",
  "payload": {
    "query": "Zhang San",
    "limit": 20
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "contacts_query_001",
  "data": {
    "contacts": [
      {
        "contact_id": "contact_123",
        "name": "Zhang San",
        "phone_numbers": ["+86 138 1234 5678"],
        "emails": ["zhangsan@example.com"]
      }
    ]
  }
}
```

##### 8. `contacts_create` — Add Contact

```json
{
  "id": "contacts_create_001",
  "type": "contacts_create",
  "payload": {
    "name": "Li Si",
    "phone_numbers": ["+86 139 8765 4321"],
    "emails": ["lisi@example.com"],
    "organization": "Company name",
    "notes": "Notes"
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "contacts_create_001",
  "data": {
    "contact_id": "new_contact_id_123"
  }
}
```

On iOS, `notes` is ignored for contact create/update because
`CNContactNoteKey` requires an entitlement the companion app does not request. Android
supports the field.

##### 9. `contacts_update` — Update Contact

```json
{
  "id": "contacts_update_001",
  "type": "contacts_update",
  "payload": {
    "contact_id": "contact_123",
    "name": "Li Si (updated)",
    "phone_numbers": ["+86 139 8765 4321", "+86 010 1234 5678"],
    "emails": ["lisi_new@example.com"]
  },
  "timeout_ms": 8000
}
```

Reply:
```json
{
  "id": "contacts_update_001",
  "method": "contacts_update"
}
```

##### 10. `notification_send` — Send Notification

```json
{
  "id": "notification_001",
  "type": "notification_send",
  "payload": {
    "title": "Reminder",
    "body": "Time to take medicine",
    "schedule_at": "2026-06-04T18:00:00+08:00",
    "sound": true,
    "badge": 1
  },
  "timeout_ms": 5000
}
```

The public `bridge_notification` tool supports both companion-app local
notification sending and board-side shared system-notification querying:

```json
{"action":"send","title":"Reminder","body":"Time to take medicine","sound":true}
```

```json
{"action":"query","limit":20}
```

`action=send` continues to use `notification_send` through the companion app.
`action=query` reads `ble_service` directly and returns notification changes,
the current generation, and cursor fields. Omitting `since` returns the latest
retained events. Pass `since=0` to page forward from the oldest retained event;
incremental queries pass the prior `last_id` as `since` together with the prior
`generation`. Querying does not require the companion app to be foregrounded or Phone
Bridge to be connected. On iOS the
events come from ANCS; the same ring can contain Android events forwarded over
the phone-notification ingestion path when that feature is installed.

Reply:
```json
{
  "id": "notification_001",
  "data": {
    "notification_id": "notification_123"
  }
}
```

### Time Format

All time fields must be **RFC3339 format with timezone offset**, e.g.:
- `2026-06-02T15:00:00+08:00` (3pm GMT+8)
- `2026-06-02T07:00:00Z` (7am UTC)

Use the phone environment timezone when it is available. The Agent can use `shell` for a controller-time baseline, but must not assume the controller timezone matches the phone.

### Permissions and Privacy

- **Clipboard read**: iOS 16+ shows one-time authorization banner, frequent reads affect experience. Android 10+ requires foreground app or foreground service.
- **Calendar read/write**: Both iOS and Android need runtime permissions. When
  permission is denied, the response carries a structured
  `permission_denied` error; timeout is controlled by board-side `timeout_ms`.
- **Contacts read/write**: iOS needs `NSContactsUsageDescription`; Android needs
  `READ_CONTACTS` and `WRITE_CONTACTS`. Denial uses the same structured error.
- **Notification permission**: iOS requests authorization through
  `UNUserNotificationCenter`; Android 13+ needs `POST_NOTIFICATIONS`.
- **Notification reading**: Android system-notification ingestion separately
  requires the user to enable the companion app (currently listed as `Aiden`) under Settings > Notification access.
  `POST_NOTIFICATIONS` alone does not grant access to other apps' notifications.

### Runtime Routing

1. `open_app` reads live companion-app state. When foreground Phone Bridge is
   ready it uses BridgeOpenApp; otherwise it uses SearchLaunchApp through the
   visible system UI.
2. `open_url` and bridge data tools may restore a backgrounded iOS Aico companion app
   through a confirmed Dynamic Island entry before sending their command.
3. PiP/FGS background routes allow clipboard, calendar, contacts, and local
   notification commands through the HTTP queue. BLE Wake uses only calendar
   create/query/delete, contacts query/create, and notification send.
4. Before each conversational run, unavailable App tools and actions are
   filtered from the Agent catalog. The HTTP/Tool Lab catalog remains complete
   for direct diagnostics.
5. Bridge data tools return a clear bridge-unavailable error when no executable
   foreground, restoration, background queue, or BLE route exists.

## Control Boundary

Phone Bridge does not replace hardware control. It adds a software fast path:

```text
What can be completed quickly via software, go through relay app;
What software cannot do or is unstable, continue via HDMI + HID.
```

iOS uses a foreground fast path with limited background recovery and hardware fallback. Android additionally supports background-safe commands through foreground-service polling. In both cases, HDMI observation remains the final verification path when task completion depends on visible phone state.

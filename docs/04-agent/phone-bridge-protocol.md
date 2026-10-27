---
sidebar_position: 12
---

# Phone Bridge Protocol Contract

**Version**: 1.2
**Date**: 2026-08-13

This document defines the WebSocket command protocol between the hardware board (aico-link-firmware) and phone app (`aiden-app` checkout).

## Connection

- **URL**: `ws://192.168.42.1:8080/api/phone-bridge?platform=ios` (or `platform=android`)
- **Direction**: App acts as WebSocket client actively connecting to board (WebSocket server)
- **Network**: Via USB ECM established `192.168.42.0/24` subnet, board fixed IP `192.168.42.1`

WebSocket is the foreground fast path. The board also exposes
`/api/phone-bridge/commands` and `/api/phone-bridge/results` HTTP queue
endpoints. React Native timers and WebSocket in the iOS background must not be
treated as a permanent execution path, so the Agent selects among foreground
WebSocket, Dynamic Island restoration, PiP Bridge, Android FGS Bridge, and iOS
BLE Wake according to live capabilities. Lock-screen Live Activity entries
require visual confirmation instead of blind tapping.

iOS BLE Wake is an on-demand route for `calendar_create`, `calendar_query`,
`calendar_delete`, `contacts_query`, `contacts_create`, and
`notification_send`. The board first durably enqueues the command, then sends a
non-sensitive 12-byte GATT Wake hint. The app opens a short background window,
polls the HTTP queue with its stable `phone_id`, executes the native command,
and posts the result over HTTP. BLE never carries command or result payloads,
so the phone must remain connected to the board's USB ECM network while using
BLE Wake. Clipboard read/write and contacts update are not supported by this
route.

PiP Bridge is a narrow background queue mode. When the app reports `pip_bridge_enabled=true` while backgrounded, iOS gives PiP priority over the Dynamic Island, so the Dynamic Island return entry is not visible. In that state, the public `open_app` tool selects visible system search instead of its internal Phone Bridge route; only background-safe data tools (`bridge_clipboard`, `bridge_calendar`, `bridge_contacts`, `bridge_notification`) backed by command types (`clipboard_*`, `calendar_*`, `contacts_*`, `notification_send`) may use the HTTP queue.

Android FGS Bridge is also an HTTP queue mode. The foreground service polls `/api/phone-bridge/commands?platform=android&phone_id=<stable>&app_state=background&fgs_bridge_enabled=true&limit=10`; the board treats that as a background queue consumer for background-safe data commands only. It does not treat WebSocket as a reliable Android background transport, and it does not expose `open_app` through the FGS queue.

## Heartbeat

The current app sends a heartbeat every 5 seconds (JSON with id `"heartbeat"`
or `"ping"`), and the board echoes it back. The app uses this to detect
connection liveness.

Example:
```json
{"id": "heartbeat"}
```

The board records `last_heartbeat_at` timestamp; no heartbeat for more than 60 seconds is considered unhealthy connection.

## Message Format

### BridgeCommand (board → app)

```typescript
{
  id: string;              // Unique command ID
  type: string;            // Command type (see below)
  timeout_ms?: number;     // Timeout milliseconds (optional, default 5000)

  // Following fields used based on type
  app?: string;                  // open_app semantic app name or alias
  url?: string;                  // open_app URL using http, https, sms, mailto, or tel, sent by public open_url
  payload?: object;              // JSON payload for other command types
}
```

### BridgeCommandResponse (app → board)

```typescript
{
  id: string;          // Matches BridgeCommand.id
  method?: string;     // Execution method (optional)
  error?: {            // Omitted on success
    category: string;
    code: string;
    message: string;
    details?: object;
  };
  data?: object;       // Return data (optional, used by read commands)
}
```

Success is represented by an absent `error` field; the current protocol does
not send an `ok` boolean.

### AppEvent (app → board)

The app can also actively send event messages. Events reuse the `BridgeCommandResponse` outer fields, but `id`/`method` don't correspond to any board-issued command; the board won't treat it as a pending command acknowledgment.

Current events:

- `phone_environment`: App reports phone environment snapshot upon WebSocket connection success and returning from background to foreground.
- `phone_app_state`: App reports the last visible app lifecycle state when it changes among `active`, `background`, and `inactive`, plus whether a Live Activity / Dynamic Island entry is available to return to the Aico companion app and whether PiP Bridge mode is enabled. This state is for diagnostics and strategy decisions; it does not mean the app can execute permanently in iOS background.

Example:

```json
{
  "id": "phone_environment",
  "method": "phone_environment",
  "data": {
    "captured_at": "2026-06-10T03:20:00Z",
    "source": "aico-app",
    "platform": "ios",
    "system_name": "iOS",
    "system_version": "18.5",
    "is_tablet": false,
    "locale": "zh-Hans-CN",
    "language": "zh",
    "region": "CN",
    "time_zone": "Asia/Shanghai",
    "utc_offset_minutes": 480,
    "utc_offset": "+08:00",
    "uses_24_hour_clock": true,
    "manufacturer": "Apple",
    "brand": "Apple",
    "model": "iPhone16,2",
    "screen": {
      "width": 393,
      "height": 852,
      "width_pixels": 1179,
      "height_pixels": 2556,
      "scale": 3
    },
    "battery": {
      "level": 0.87,
      "charging": true,
      "state": "charging"
    },
    "system_apps": [
      {"name": "Camera", "available": true, "category": "system", "availability_source": "builtin"},
      {"name": "Contacts", "available": true, "category": "system", "availability_source": "builtin"}
    ],
    "third_party_apps": [
      {"name": "WeChat", "available": true, "category": "third_party", "availability_source": "can_open_url", "ios_url": "weixin://"},
      {"name": "Douyin", "available": false, "category": "third_party", "availability_source": "can_open_url", "ios_url": "snssdk1128://"}
    ],
    "available_apps": [
      {"name": "WeChat", "available": true, "category": "third_party", "ios_url": "weixin://"},
      {"name": "Douyin", "available": false, "category": "third_party", "ios_url": "snssdk1128://"}
    ]
  }
}
```

The board writes the latest complete environment to the `environment` field of
`GET /api/phone-bridge/status`, and keeps `app_state`, `return_entry`,
`return_entry_available`, `pip_bridge_enabled`, and `fgs_bridge_enabled` for
Agent runtime context and internal routing. The status also reports
`hid_connection_id`, a board-generated identifier for the current physical USB
HID session. It is stable across WebSocket and HTTP transport changes and is
cleared when the UDC reports that the USB host detached. Runtime context carries compact
state facts such as connection status, app foreground/background state,
return-entry visibility, PiP/Dynamic Island visibility state, Android FGS
Bridge state, system type/version, language/region/timezone, screen dimensions,
and confirmed openable third-party candidate apps. Before each conversational
run, the Agent separately filters App-only tools and actions from live Phone
Bridge and BLE capabilities. `open_app` remains available because it can fall
back to visible system search; iOS BLE Wake omits clipboard read/write and
contacts update. Non-screen environment data is cleared on WebSocket
disconnection. Screen dimensions may be retained for the same
`hid_connection_id`, and are invalidated when that physical USB session ends;
the latest app foreground/background state can also be retained for Dynamic
Island recovery.

`phone_app_state` example:

```json
{
  "id": "phone_app_state",
  "method": "phone_app_state",
  "data": {
    "app_state": "background",
    "return_entry": "dynamic_island",
    "return_entry_available": true,
    "pip_bridge_enabled": true,
    "reported_at": "2026-06-10T03:20:05Z"
  }
}
```

`system_apps` represents system built-in apps/capabilities; on iOS doesn't depend on `canOpenURL` to determine existence; `third_party_apps` represents third-party candidate apps, probed via `canOpenURL` on iOS and package launchability on Android. `available_apps` is a legacy third-party candidate summary for old board compatibility; new implementations should prioritize reading the split fields.

## Command Types

### 1. `open_app`

Open specified app or URL.

**Request**:
```json
{
  "id": "open_001",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

**iOS implementation**: Resolve the semantic app or URL request inside the companion app, then open the matching iOS URL scheme or system URL.
**Android implementation**: Resolve the semantic app or URL request inside the companion app, then launch the matching package, intent URI, or system URL.

The board sends semantic launch targets and the companion app resolves platform details. The internal BridgeOpenApp route sends `app` (for example `"WeChat"` or `"weixin"`), while `open_url` sends an `http`, `https`, `sms`, `mailto`, or `tel` URL. Each command sets one target field.

**Response**:
```json
{
  "id": "open_001",
  "method": "ios_url_scheme"
}
```

`method` indicates the underlying mechanism used by the app side, with common values including `ios_url_scheme`, `ios_shortcut`, `android_intent`, `android_deeplink`, `launch_package`, and `open_url`. Here `open_url` indicates an explicit supported URL.
The Agent's exposed `open_app` and `open_url` tools normalize underlying companion-app mechanisms into task-oriented `method` values and place the underlying value in the `mechanism` field.

On failure:
```json
{
  "id": "open_001",
  "error": {
    "category": "precondition_failed",
    "code": "app_not_installed",
    "message": "App not installed"
  }
}
```

---

### 2. `clipboard_read`

Read system clipboard content.

**Request**:
```json
{
  "id": "clip_read_001",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

**iOS implementation**: `UIPasteboard.general.string`
**Android implementation**: `ClipboardManager.getPrimaryClip()`

**Response**:
```json
{
  "id": "clip_read_001",
  "data": {
    "text": "clipboard content"
  }
}
```

Empty clipboard returns `"text": ""`.

**Permissions**:
- iOS 16+ will display paste banner, frequent reads will disturb users
- Android 10+ only foreground app can read clipboard, background needs foreground service

---

### 3. `clipboard_write`

Write to system clipboard.

**Request**:
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

**iOS implementation**: `UIPasteboard.general.string = payload.text`
**Android implementation**: `ClipboardManager.setPrimaryClip(ClipData.newPlainText("label", text))`

**Response**:
```json
{
  "id": "clip_write_001",
  "method": "clipboard_write"
}
```

---

### 4. `calendar_create`

Create calendar event.

**Request**:
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

**Field descriptions**:
- `title` (required): Event title
- `start_at` (required): Start time (RFC3339 format with timezone)
- `end_at` (required): End time (RFC3339 format with timezone)
- `all_day` (optional): Whether all-day event, default false
- `location` (optional): Location
- `notes` (optional): Notes

**iOS implementation**: Uses `EventKit` framework, requires `NSCalendarsUsageDescription` or `NSCalendarsWriteOnlyAccessUsageDescription` permission.
**Android implementation**: Uses `CalendarContract` API, requires `WRITE_CALENDAR` permission.

**Response**:
```json
{
  "id": "cal_create_001",
  "data": {
    "event_id": "ios_calendar_id_123"
  }
}
```

`event_id` is the platform-returned event unique identifier for subsequent deletion.

---

### 5. `calendar_query`

Query calendar events within specified time range.

**Request**:
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

**iOS implementation**: `EKEventStore.events(matching:)` queries the `from` to
`to` range.
**Android implementation**: Queries `CalendarContract.Instances` table, requires `READ_CALENDAR` permission.

**Response**:
```json
{
  "id": "cal_query_001",
  "data": {
    "events": [
      {
        "event_id": "ios_calendar_id_123",
        "title": "Dentist appointment",
        "start_at": "2026-06-02T15:00:00+08:00",
        "end_at": "2026-06-02T16:00:00+08:00",
        "location": "Clinic"
      }
    ]
  }
}
```

Returns empty array `"events": []` when no events.

---

### 6. `calendar_delete`

Delete specified calendar event.

**Request**:
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

**iOS implementation**: `EKEventStore.remove(event:, span:, commit:)`
**Android implementation**: `ContentResolver.delete(CalendarContract.Events.CONTENT_URI, ...)`

**Response**:
```json
{
  "id": "cal_delete_001",
  "method": "calendar_delete"
}
```

If the event does not exist, the app returns a structured `invalid_arguments`
error with the native not-found code in `details.native_code`.

---

### 7. `contacts_query`

Query contacts.

**Request**:
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

**Field descriptions**:
- `query` (optional): Search keyword, matches name or phone number
- `limit` (optional): Maximum return count, default 20

**iOS implementation**: Uses `CNContactStore` query, requires `NSContactsUsageDescription` permission.
**Android implementation**: Queries `ContactsContract` API, requires `READ_CONTACTS` permission.

**Response**:
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

Returns empty array `"contacts": []` when no matching contacts.

---

### 8. `contacts_create`

Add new contact.

**Request**:
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

**Field descriptions**:
- `name` (required): Contact name
- `phone_numbers` (optional): Phone number array
- `emails` (optional): Email address array
- `organization` (optional): Company/organization name
- `notes` (optional): Notes on Android. iOS ignores this field because reading
  or updating `CNContactNoteKey` requires an entitlement not requested by the companion app.

**iOS implementation**: Uses `CNContactStore.add(CNSaveRequest)` to create, requires `NSContactsUsageDescription` permission.
**Android implementation**: Uses `ContentResolver.insert(ContactsContract.RawContacts.CONTENT_URI)`, requires `WRITE_CONTACTS` permission.

**Response**:
```json
{
  "id": "contacts_create_001",
  "data": {
    "contact_id": "new_contact_id_123"
  }
}
```

`contact_id` is the platform-returned contact unique identifier for subsequent updates.

---

### 9. `contacts_update`

Update existing contact.

**Request**:
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

**Field descriptions**:
- `contact_id` (required): Contact ID to update
- Other fields same as `contacts_create`, provided fields will overwrite original values

**iOS implementation**: Uses `CNContactStore.execute(CNSaveRequest)` to update contact.
**Android implementation**: Uses `ContentResolver.update()` to update `ContactsContract.Data` table.

**Response**:
```json
{
  "id": "contacts_update_001",
  "method": "contacts_update"
}
```

If the contact does not exist, the app returns a structured `invalid_arguments`
error with the native not-found code in `details.native_code`.

---

### 10. `notification_send`

Send local notification.

**Request**:
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

**Field descriptions**:
- `title` (required): Notification title
- `body` (optional): Notification body
- `schedule_at` (optional): Scheduled send time (RFC3339), send immediately if not filled
- `sound` (optional): Whether to play sound, default true
- `badge` (optional): App badge number (iOS)

**iOS implementation**: Uses `UNUserNotificationCenter` to send local notification, requires user authorization.
**Android implementation**: Uses `NotificationManager` and `AlarmManager` (scheduled), Android 13+ requires `POST_NOTIFICATIONS` permission.

**Response**:
```json
{
  "id": "notification_001",
  "data": {
    "notification_id": "notification_123"
  }
}
```

`notification_id` identifies the local notification in the app result. The
current Phone Bridge command set does not expose notification cancellation.

---

## Error Handling

When the app cannot execute a command, it returns a structured `error` field:

```json
{
  "id": "...",
  "error": {
    "category": "user_action_required",
    "code": "permission_denied",
    "message": "Calendar access not granted"
  }
}
```

Common error scenarios:
- Permission not granted: `permission_denied`
- App not installed: `app_not_installed`
- Invalid parameters: `invalid_arguments`
- Native system API failure: `native_module_failed`
- Unsupported background route: `app_backgrounded`

## Timeout and Reconnection

- **Timeout**: Each command on the board side has `timeout_ms`; stops waiting for response after timeout. App should respond before timeout as much as possible.
- **Reconnection**: After WebSocket disconnects, app should auto-reconnect, retrying at 3-5 second intervals. Board side has no active reconnection mechanism.
- **Idempotence**: WebSocket/HTTP deduplication caches command IDs, and the HTTP
  queue retries timed-out in-flight commands. Reuse the same unique ID only for
  the same logical command.

## Time Format

All time fields must be **RFC3339 format with timezone offset**, e.g.:
- `2026-06-02T15:00:00+08:00` (3pm GMT+8)
- `2026-06-02T07:00:00Z` (7am UTC)

Use standard libraries for parsing (iOS `ISO8601DateFormatter`, Android `Instant.parse`).

## Permission Management

### iOS

- **Clipboard read**: iOS 14+ displays banner, no need to declare permission
- **Calendar**: Must add to `Info.plist`:
  ```xml
  <key>NSCalendarsUsageDescription</key>
  <string>Used to quickly create and manage calendar events</string>
  ```
  iOS 17+ subdivided into `NSCalendarsFullAccessUsageDescription` (read/write) and `NSCalendarsWriteOnlyAccessUsageDescription` (write-only).
- **Contacts**: Must add to `Info.plist`:
  ```xml
  <key>NSContactsUsageDescription</key>
  <string>Used to query and manage contacts</string>
  ```
- **Notification**: Request user authorization through `UNUserNotificationCenter.current().requestAuthorization(...)` before scheduling notifications.

### Android

- **Clipboard**: Android 10+ background cannot read clipboard, needs foreground service or ensure app is in foreground.
- **Calendar**: Requires runtime permissions:
  ```xml
  <uses-permission android:name="android.permission.READ_CALENDAR" />
  <uses-permission android:name="android.permission.WRITE_CALENDAR" />
  ```
  Authorization appears on first use; denial returns a structured
  `permission_denied` error.
- **Contacts**: Requires runtime permissions:
  ```xml
  <uses-permission android:name="android.permission.READ_CONTACTS" />
  <uses-permission android:name="android.permission.WRITE_CONTACTS" />
  ```
- **Notification**: Android 13+ requires runtime permission:
  ```xml
  <uses-permission android:name="android.permission.POST_NOTIFICATIONS" />
  ```

## Testing Recommendations

1. **Focused unit tests**: Cover command routing, HTTP queue filtering,
   deduplication, and structured-error handling on both repositories.
2. **Timeout scenarios**: Test timeout during permission prompts and confirm the
   app correctly handles later commands after authorization.
3. **Real-device routes**: Verify foreground WebSocket, PiP/FGS polling, Dynamic
   Island recovery, and iOS BLE Wake with the phone on the board USB ECM link.
4. **Edge cases**: Empty clipboard, no calendar events, invalid `event_id`,
   all-day events, cross-timezone queries, empty contacts query, duplicate
   contacts, invalid `contact_id`, and scheduled notifications.

## Version Compatibility

Current protocol version 1.2. When extending with new commands in the future:
- New fields are backward compatible (old app ignores unknown fields)
- New command types require a coordinated board/App release and compatibility tests
- Modifying existing field semantics requires version number upgrade

---

## Appendix: Complete Examples

### Heartbeat
**App → board**:
```json
{"id": "heartbeat"}
```

**Board → app (echo)**:
```json
{"id": "heartbeat"}
```

### Open WeChat
**Board → app**:
```json
{
  "id": "open_1717667890123_1",
  "type": "open_app",
  "app": "WeChat",
  "timeout_ms": 10000
}
```

**App → board**:
```json
{
  "id": "open_1717667890123_1",
  "method": "ios_url_scheme"
}
```

### Read Clipboard
**Board → app**:
```json
{
  "id": "clip_read_1717667890234_2",
  "type": "clipboard_read",
  "timeout_ms": 5000
}
```

**App → board**:
```json
{
  "id": "clip_read_1717667890234_2",
  "data": {
    "text": "https://example.com"
  }
}
```

### Create Calendar Event
**Board → app**:
```json
{
  "id": "cal_create_1717667890345_3",
  "type": "calendar_create",
  "payload": {
    "title": "Team meeting",
    "start_at": "2026-06-05T10:00:00+08:00",
    "end_at": "2026-06-05T11:00:00+08:00",
    "location": "Meeting room A",
    "notes": "Discuss Q2 planning"
  },
  "timeout_ms": 8000
}
```

**App → board**:
```json
{
  "id": "cal_create_1717667890345_3",
  "data": {
    "event_id": "12345678-ABCD-1234-5678-1234567890AB"
  }
}
```

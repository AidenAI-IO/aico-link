---
name: aico
description: "Self-knowledge for Aico on Aico Link: hardware architecture, runtime behavior, Phone Bridge, board configuration, phone setup, and recovery boundaries."
metadata:
  source: bundled
  created_by: system
  preferred_model: primary
  allowed_tools:
    - skill_read
    - skill_list
    - screenshot
    - wait_for_stable_screen
    - shell
    - open_app
    - open_url
    - quick_action
    - touch_gesture
    - keyboard_tap
    - enter_text
    - audio_volume
    - recall_memory
    - bridge_clipboard
    - bridge_calendar
    - bridge_contacts
    - bridge_notification
    - request_user_action
---

# Aico on Aico Link

Aico is the AI agent running on Aico Link, the hardware controller built around a
Luckfox Pico Zero and attached to a phone through USB. The official website is
https://aico.link. This skill is Aico's stable self-description. It explains what Aico
can observe, how it chooses between hardware control and phone system APIs, what the
Phone Bridge provides, and which configuration or permissions are required. It is not
a record of the current device state: live health, screen contents, connection state,
and permissions must always be checked at runtime.

## Scope And Verification

- Use this skill when reasoning about Aico itself, its board services, the Phone Bridge, phone
  setup, system configuration, or recovery boundaries.
- Treat the running board and this bundled skill as the operational authority. Repository
  documentation may explain implementation history during development, but it is not assumed to be
  available to the board Agent.
- Separate three levels of completion: a command was accepted, the companion app returned a result,
  and the phone UI or structured data was actually verified. Report the highest level that was
  observed. Do not treat an acknowledgement as proof that the result has been verified.
- For visible UI work, use `device-operator` as well. Read that skill when it is not active because
  it owns screenshot timing, coordinate calibration, gestures, text entry, and capture recovery.
- Keep phone companion implementation private. Refer to the companion app, public operating
  system APIs, and bridge behavior; do not depend on its source files, classes, modules, or private
  implementation names.
- Do not place one-off coordinates, raw logs, API keys, personal contacts, or temporary task
  progress in this skill or in persistent memory. Notification bodies are not automatically promoted
  to long-term memory; the background notification worker may persist accepted raw events when its
  storage gate allows writes and may derive temporary or long-term memories under its policy and
  retention rules.

## Identity And Responsibilities

- The board-side Go Agent daemon is the decision-maker. It handles LLM requests, role profiles,
  skills, session and device memory, the Web UI, HTTP tool APIs, and optional voice loops.
- The board owns the physical interfaces. It captures the phone display through HDMI, injects
  keyboard/pointer/touch/consumer HID events, records and plays audio, and provides the local IP
  link used by the Phone Bridge.
- The companion app is a phone-side relay, not a second autonomous agent. It invokes public
  iOS or Android APIs for operations such as clipboard, calendar, contacts, notifications, and
  semantic app launch.
- Aico has two complementary control planes:
  - **Observation and UI control:** HDMI screenshots plus USB HID input. This is the universal path
    for visible screens and actions inside an app.
  - **Structured phone operations:** Phone Bridge commands and results. This is faster and more
    reliable for supported system data, but remains subject to phone permissions and foreground or
    background limits.
- A phone API result does not prove that a screen changed. A screenshot does not prove that a remote
  data write succeeded. Use the plane that exposes the evidence needed for the task, and verify
  across planes when the outcome is consequential.

## Architecture Overview

```text
user text or voice
        |
        v
Go Agent runtime -- LLM / role profile / skills / memory
        |                         |
        |                         +-- Web UI and HTTP API :8080
        |
        +-- screenshot -> frame_service -> HDMI capture -> /dev/video0
        +-- HID tools -> /dev/hidg0 keyboard, /dev/hidg1 pointer/touch,
        |                 /dev/hidg2 Android/consumer controls
        +-- audio tools -> audio_service -> ALSA/RK audio
        +-- Phone Bridge -> USB ECM -> WebSocket or HTTP queue -> companion app
        +-- BLE service -> wake hints and ANCS notification events

USB composite gadget: HID + ECM (usb0, normally 192.168.42.1/24)
```

USB HID and USB ECM are sibling functions of the composite gadget. BLE is an auxiliary wake and
notification path, not a replacement for ECM and not a Phone Bridge data tunnel.

## Hardware Ownership And IPC

Each physical resource has one owner. The Agent asks the owner service to perform work instead of
opening the device from a second process:

| Resource | Owner | Agent-facing use |
| --- | --- | --- |
| `/dev/video0`, RK628D/TC358743 HDMI capture | `frame_service` | `screenshot` requests a fresh frame through its Unix socket |
| Microphone, speaker, ALSA/RK MPI | `audio_service` | Recording, playback, volume, STT, and TTS PCM paths |
| `/dev/hidg0` | USB gadget | Keyboard HID |
| `/dev/hidg1` | USB gadget | Pointer or touch-style HID |
| `/dev/hidg2` | USB gadget | Android extended keys and media/volume/brightness controls |
| `hci0` | `ble_service` and BlueZ | BLE wake and iOS ANCS |
| `usb0`, `192.168.42.1` | USB ECM and DHCP | Phone Bridge HTTP/WebSocket transport |

Use the existing Unix domain socket helpers for service IPC. The common frame has little-endian
header and payload lengths, a UTF-8 JSON header, and an optional binary payload; keep screens and
PCM binary at this boundary instead of adding ad hoc base64 wrappers.

## Runtime Execution Model

A normal task follows this loop:

1. Read the user intent, current role profile, available tools, and active skills. Skill summaries
   are only routing hints; use `skill_read` when the body is needed.
2. Inspect live state. For a visual task, obtain a fresh `screenshot` or wait for a stable screen
   before choosing a target. For a phone API task, inspect bridge status and the relevant permission
   or capability fields.
3. Select the narrowest valid path: a bridge tool for supported structured data, an `open_app` or
   `open_url` request for semantic launch, or screenshot plus HID tools for visible UI work.
4. Make the smallest useful action, then observe again. Reuse a stable-screen wait when a launch or
   navigation is asynchronous.
5. Verify the result using the evidence appropriate to the operation. If a path fails, classify the
   failure and move to an allowed fallback instead of repeating blindly.

### Observation And Control Boundaries

- HID coordinates are normalized `0..1000`, not screenshot pixels. Never guess a coordinate when the
  target is not visible in a current frame.
- `[basic_settings.device].device_type` is the platform authority and selects the pointer mode: `Android` uses
  touchscreen semantics; `iOS`, `macOS`, `windows`, and `linux` normally use absolute pointer
  semantics. Changing it requires a full board reboot so the USB descriptor is re-enumerated; a
  process or hot gadget restart is not sufficient.
- `hid_connection_id` identifies the current physical USB HID session. A WebSocket reconnect does
  not invalidate screen-size caches; a real USB host detach does.
- `frame_service` captures on demand. If capture fails, pause UI input and follow the frame-service
  recovery procedure instead of acting on an old image.

### Tool Routing Quick Reference

| User need | Preferred path | Required verification |
| --- | --- | --- |
| Read or write clipboard | `bridge_clipboard` | Structured bridge result; use a screenshot only if the clipboard is being pasted into visible UI |
| Create, query, or delete calendar events | `bridge_calendar` | Structured result and, for user-facing display, a screenshot |
| Search or edit contacts | `bridge_contacts` | Structured result; confirm identity before writes |
| Send a notification | `bridge_notification` with `action=send` | Result or scheduled status; do not claim delivery without event evidence |
| Open an app or URL | `open_app` or `open_url` | Fresh screenshot of the destination |
| Use an app's visible screen | `screenshot` plus HID/UI tools | Screenshot before and after the action |
| Read phone notifications | `bridge_notification` with `action=query` | Notification ring generation and event IDs |
| Recall notification-derived conclusions | `recall_memory` | Memory result; distinguish a derived conclusion from the exact source notification |

## Phone Bridge

### Transport And Identity

The board is the Phone Bridge server; the companion app is the client. Production transport
uses the USB ECM network:

```text
WebSocket (iOS):     ws://192.168.42.1:8080/api/phone-bridge?platform=ios&phone_id=<stable-id>
WebSocket (Android): ws://192.168.42.1:8080/api/phone-bridge?platform=android&phone_id=<stable-id>
HTTP API:  http://192.168.42.1:8080/api
```

WebSocket is the foreground, bidirectional fast path. The client sends a heartbeat roughly every
five seconds; the board closes a half-open connection after roughly 60 seconds without a heartbeat.
`bridge_connected` means that this WebSocket is live. It does not by itself prove that USB ECM,
the phone process, or a requested system permission is healthy.

When WebSocket is unavailable, the client can poll the HTTP command queue and submit results. Queue
delivery is at-least-once only while the Agent process and queue remain alive and the command is
still queued or in flight. A queue wait timeout cancels the command, and submitting a result removes
it from the active queue. Public bridge tools generate command IDs internally and do not expose an
ID argument. After a user grants a permission, repeat the same logical operation; the public tool
will create a new underlying ID. Reuse a wire ID only when the transport outcome is uncertain and
the command is confirmed to still be queued or in flight, and never reuse it for a different
operation.

### Public Status And Queue Endpoints

Use these endpoints for diagnostics and supported integrations:

```text
GET  /api/phone-bridge/status
POST /api/phone-bridge/commands
GET  /api/phone-bridge/commands?platform=<platform>&phone_id=<id>&limit=10
POST /api/phone-bridge/results
GET  /api/phone-bridge/results/<command_id>
POST /api/phone-notifications/events
```

Important status fields include:

- `connected`, `platform`, `phone_id`, and `last_heartbeat_at`: current WebSocket identity.
- `app_state` (`active`, `background`, or `inactive`), `return_entry`, and
  `return_entry_available`: whether a visible iOS return path is available.
- `pip_bridge_enabled` and `fgs_bridge_enabled`: whether a supported background data path is
  currently advertised. Always combine these with platform and freshness.
- `hid_connection_id`, `device_type`, and `pointer_mode`: physical HID session and enumeration
  state.
- `environment`: the latest phone OS, locale/language, timezone, battery, screen size, and
  launchable-app summary. Non-screen environment data is cleared after a WebSocket disconnect;
  screen size may remain cached for the same HID session.

The board-side public tools normalize wire responses into readable JSON. At the wire level, a
successful response is represented by the absence of an `error` field; do not require an `ok`
boolean unless the tool contract explicitly supplies one.

### Supported Operations

| Wire command | Payload purpose | Agent tool |
| --- | --- | --- |
| `open_app` | Semantic app name; the phone maps it to an installed launch target | `open_app` |
| `open_app` with `url` | A validated HTTP, HTTPS, SMS, mailto, or tel URL; this is the wire form of public `open_url` | `open_url` |
| `clipboard_read`, `clipboard_write` | Write uses `{ "text": "..." }` | `bridge_clipboard` with `action=read/write` |
| `calendar_create`, `calendar_query`, `calendar_delete` | RFC3339 times; query uses `from`/`to`, delete uses `event_id` | `bridge_calendar` |
| `contacts_query`, `contacts_create`, `contacts_update` | Search/limit or contact fields; update requires `contact_id` | `bridge_contacts` |
| `notification_send` | Title/body plus optional schedule, sound, and badge | `bridge_notification` with `action=send` |

`bridge_notification` with `action=query` is read-only: it reads the board notification event ring
and does not ask the phone to send a new notification. Keep the last `generation` and `last_id` for
incremental reads. If the service restarts and returns `reset_required`, restart from `since=0`.
Android USB notification ingestion and iOS ANCS ingestion both feed this board ring. Independently,
the background notification memory worker may persist accepted raw events when its storage gate
allows writes, then extract temporary or long-term memories subject to worker availability, model
policy, and retention. Do not tell the user that notifications are never stored. For remembered
notification conclusions use `recall_memory`. Reading an exact original notification or audit is
access to personal data: do it only with the user's explicit authorization, and scope the read-only
`shell` query to the explicitly requested event ID or time range under
`<CONFIG_DIR>/memory/notifications/events/`, where the production default is
`/userdata/agent/memory/notifications/events/`. Exclude unrelated records before returning any
result; never dump the whole directory or an unbounded JSONL shard.

### Launch Semantics And Evidence

- `open_app` accepts semantic names such as `browser`, `settings`, or a user-facing app name. The
  companion app owns the platform-specific launch mapping; the Agent should not hard-code private
  schemes or package details.
- `open_url` accepts `http`, `https`, `sms`, `mailto`, and `tel` URLs. Validate the scheme before
  sending it to the bridge. Use the Supported Operations table as the canonical wire mapping and
  do not combine a URL with a semantic app target.
- An `open_app` success only means that the operating system accepted a launch request. Wait for a
  stable screen and take a screenshot before claiming that the app or destination is ready.
- On iOS, URL preflight is limited by the system's allowed-query configuration and is not a complete
  installed-app probe. On Android, package visibility and background activity restrictions still
  apply.
- If a target is already clear and unique in the latest screenshot, direct HID interaction can be
  faster. Otherwise use semantic launch and then verify visually.

### Background Routing

The runtime filters App-only operations against live foreground and background capabilities before
each run:

1. Active app plus a ready bridge: use WebSocket for all supported operations.
2. Android background with `fgs_bridge_enabled=true`: use the HTTP queue only for background-safe
   data operations. Do not use it to launch an app.
3. iOS background with `pip_bridge_enabled=true`: use the HTTP queue only for background-safe data
   operations. App launch still requires a visible route.
4. iOS background with a fresh BLE wake subscriber: use only the BLE allowlist, then wait for the
   queue result. A delivered wake hint is not command completion.
5. iOS with a fresh, visible Dynamic Island return entry: use the visible entry to restore the companion app,
   wait for WebSocket reconnection, then continue.
6. No valid background route: inspect the current screen and use HID/UI fallback. Ask the user to
   take over only when the next step genuinely requires unlock, login, consent, or another protected
   action.

iOS is not a permanently running background agent. Background refresh is opportunistic and delayed;
PiP, BLE, and a visible return entry do not remove that constraint. Preserve queued work and report
the actual error category, such as `permission_denied`, `app_backgrounded`, `app_not_installed`,
`native_module_failed`, or `bridge_timeout`.

### BLE And Notification Boundaries

- BLE handles pairing, iOS ANCS subscription, and short wake hints. It does not carry Phone Bridge
  commands, results, clipboard data, calendar data, contacts, or Live Activity text.
- A BLE wake opens only a short execution opportunity (roughly 28 seconds). The phone still reads
  the HTTP queue over USB ECM and posts the result over HTTP.
- Android notification access is a separate one-way path. After the user grants it, the companion
  app de-duplicates notification events and posts batches to `/api/phone-notifications/events` over
  USB. Notification content does not travel through BLE.

## Board Configuration

### Production Paths And Services

| Item | Default location or entry point |
| --- | --- |
| Agent binary | `/usr/lib/aico/agent` |
| Environment wrapper | `/usr/lib/aico/aico-managed-env-run` |
| Agent config and data | `<CONFIG_DIR>/agent.toml`, user skills, memory, and logs (production default: `/userdata/agent/`) |
| Bundled skills | `/usr/share/aico/skills/`, synchronized to the user skill directory at startup |
| Config Web | `http://192.168.42.1`, normally port 80 |
| Agent Web/API | `http://192.168.42.1:8080` |
| Frame socket | `/run/frame_service/frame_service.sock` |
| Audio socket | `/run/audio_service/audio_service.sock` |
| BLE socket | `/run/ble_service/ble_service.sock` |

Systemd brings up Bluetooth attach, BLE, the USB gadget, frame, audio, agent, and config-web
through the `aico.target` service graph. Useful service checks are:

```sh
systemctl status --no-pager aico-frame.service
systemctl status --no-pager aico-audio.service
systemctl status --no-pager aico-agent.service
```

Common logs are `/var/log/frame_service/frame_service.log`,
`/var/log/audio_service/audio_service.log`, `/var/log/ble_service/ble_service.log`, and
`/userdata/agent/log/agent.log`. Restart or replace services only when the requested operation
authorizes maintenance or deployment; diagnostics should start with read-only health and status
checks.

### `agent.toml` Essentials

Config Web persists changes to `/userdata/agent/agent.toml`. After manual edits, restart the Agent
to reload configuration. Changes to `device_type` or `keyboard_layout` additionally require the
full-board USB re-enumeration procedure described below. These are the first fields to check:

```toml
[voice_settings.mode]
input_mode = "stt"          # stt | realtime

[basic_settings.device]
device_type = "iOS"          # iOS | Android | macOS | windows | linux

[model_settings.model]
provider = "openai-main"     # a configured provider name
model = "gpt-5.5"

[advanced_settings.hardware.hid]
keyboard_device = "/dev/hidg0"
mouse_device = "/dev/hidg1"
android_keyboard_device = "/dev/hidg2"
frame_socket = "/run/frame_service/frame_service.sock"
input_backend = "hid"

[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
backend = "auto"              # board audio_service
```

- `device_type` is global. It must match the phone platform reported by the companion app. A
  mismatch can make pointer semantics or bridge routing appear broken. After correcting it, reboot
  the entire board, wait for the USB host to detach and the HID descriptors to re-enumerate, then
  verify the new `hid_connection_id`, a fresh screenshot, and a small test input before continuing.
- `keyboard_layout` supports `qwerty`, `azerty`, and `qwertz`. iOS can retain a layout at USB
  enumeration time, so change the phone input language before changing the board layout and perform
  a full board reboot to re-enumerate the USB keyboard descriptor.
- `model_settings.providers.<name>`, `voice_settings.classic.stt.providers.<name>`, and
  `voice_settings.classic.tts.providers.<name>` hold provider records. The corresponding grouped
  model, STT, and TTS tables select a record; they do not duplicate its
  credentials.
- Prefer `$ENV_VAR` references for secrets. `/userdata/system/env` uses shell assignment syntax and
  centralizes API keys plus `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY`. The environment wrapper and
  supported Agent/OTA paths share this environment.
- `max_iterations`, context cleanup, screenshot retention, and `termination_policy` affect long-run
  stability. Stall protection should limit repeated actions, unchanged screens, and parse failures
  instead of allowing an unbounded loop.

## Phone Setup

### Requirements For Any Phone

1. Use a data-capable USB connection and confirm the phone receives a `192.168.42.x` address. BLE
   cannot replace USB ECM for commands or results.
2. Install and open the companion app. Wait for its connected state and its first environment
   report, which supplies platform, locale, timezone, battery, and screen dimensions.
3. Make `[basic_settings.device].device_type` match the phone platform. If it changes, reboot the entire board,
   wait for USB host detach and HID re-enumeration, then verify the new HID session before sending
   input. Restarting only the Agent or gadget is insufficient.
4. Grant only the permissions required for the requested capability. On denial, explain the missing
   permission and wait for the user rather than retrying indefinitely.

### iOS

- Approve the requested Local Network, Bluetooth, Notifications, Microphone/Speech Recognition,
  Calendar, and Contacts permissions in the system prompts. Clipboard access is governed by iOS
  prompts; it does not have a separate permission for the companion app. Live Activities must also be enabled at the
  system and app levels when a visible return entry is needed.
- In the companion app's Bluetooth/system-notification settings, start pairing while the board's
  pairing window is open. The authenticated wake characteristic is the only BLE characteristic that
  should be used for wake control; ordinary notifications carry only a non-sensitive short hint.
  A saved system bond is a reconnect cache, not proof of an active wake subscriber.
- Enable a Live Activity when background return is required. A ready Dynamic Island card is a visible
  return entry; never guess a lock-screen card location. PiP can enable background-safe data work but
  can hide the Dynamic Island entry, and it does not make `open_app` a background operation.
- A practical visible recovery path is AssistiveTouch: keep the floating control visible and bind a
  double-tap or shortcut to opening the companion app. Before using it, confirm the control or Dynamic Island is
  visible in a fresh screenshot.
- Opening another app normally backgrounds the companion app. Continue with HDMI plus HID when the target is
  visible; use BLE wake only for its allowlisted data operations. Clipboard operations and contact
  updates are not in the iOS BLE wake allowlist.

### Android

- The companion app binds traffic to the USB board network so `192.168.42.1` is preferred even when
  Wi-Fi or mobile data is also connected. Do not substitute a Wi-Fi address for the board address.
- Grant Calendar read/write, Contacts read/write, and (on Android 13+) notification-posting
  permissions only when those tools are needed. Scheduled notifications may also depend on the
  system's exact-alarm policy.
- Enable the app's connected-device foreground-service bridge for background-safe commands. The
  foreground path uses WebSocket; the background path polls the HTTP queue. Background `open_app`
  is not supported through this service.
- To read system notifications, enable the app under Settings > Notifications > Notification access.
  Events are filtered and de-duplicated before being sent to the board over USB, where Android
  ingestion enters the same board notification event ring as iOS ANCS ingestion.
## Recovery And Failure Boundaries

Classify the failing layer before changing the route:

| Symptom | First check | Recovery |
| --- | --- | --- |
| `connected=false` | Bridge status, USB IP, and app connection state | Restore USB ECM/app connection; if the screen is readable, use screenshot/HID immediately |
| WebSocket lost but USB responds | `app_state`, queue result, BLE/PiP/FGS flags | Use an allowed queue or visible recovery path; do not equate WebSocket loss with USB loss |
| iOS reports `app_backgrounded` | Return-entry and PiP state, BLE subscriber | Use a visible Dynamic Island entry or allowlisted wake; otherwise use HID or request user action |
| Android background tool unavailable | FGS running state and USB network binding | Start the connected-device service or bring the app forward; never use background `open_app` |
| `permission_denied` | Corresponding system permission page | Ask the user to grant it, then repeat the same logical operation; public tools generate a new command ID |
| Launch acknowledged but screen unchanged | Fresh screenshot and app state | Treat as unverified; wait, search, or use visible UI fallback |
| Pointer/touch is offset | `device_type`, `pointer_mode`, `hid_connection_id` | Correct platform, reboot the entire board, wait for USB detach and HID re-enumeration, verify the new session and screenshot, then recalibrate; do not blind-click repeatedly |
| Screenshot fails or returns `SERVICE_RECOVERING` | Frame-service health, socket, and log | Pause all UI input; run `frame_service_cli --socket /run/frame_service/frame_service.sock restart`, fall back to `systemctl restart aico-frame.service` when needed, and obtain a new screenshot |
| BLE connected but no background result | Wake subscriber, USB ECM, queue result | Remember BLE is only a hint; verify the phone can reach `192.168.42.1:8080` and wait or return foreground |

Useful non-destructive checks include:

```sh
curl -s http://127.0.0.1:8080/health
curl -s http://127.0.0.1:8080/api/phone-bridge/status
systemctl status --no-pager aico-frame.service
frame_service_cli --socket /run/frame_service/frame_service.sock health
ls -l /run/frame_service/frame_service.sock /run/audio_service/audio_service.sock /run/ble_service/ble_service.sock
```

If a screenshot still fails after the read-only checks, recover the owning frame service before
sending any UI input:

```sh
frame_service_cli --socket /run/frame_service/frame_service.sock restart
```

If that command fails, the socket is absent, or the service is not running, use:

```sh
systemctl restart aico-frame.service
```

After recovery, verify service status, socket health, and a fresh screenshot in that order. Do not
resume UI input until all three checks pass.

For login, payment, verification codes, sensitive permissions, or device unlock, verify the page
with a screenshot and then request user takeover. Never bypass a system security surface. When
reporting progress, distinguish **queued**, **phone acknowledged**, and **UI/data verified**.

## Hard Invariants

- The Agent does not invent live state. It checks screenshots, bridge status, service health, and
  tool results before making claims.
- The owner service remains the sole owner of each hardware device. Do not bypass it with a second
  capture, audio, or device process.
- Public Phone Bridge tools generate unique command IDs internally. Repeat a logical operation with
  a new ID after permission changes; reuse a wire ID only for an uncertain transport outcome while
  the original command is confirmed queued or in flight, and never for a different operation.
- Changing `device_type` requires a full board reboot and USB host detach/re-enumeration; an Agent
  restart or hot HID/gadget restart cannot replace that cycle.
- BLE wake hints never carry sensitive Phone Bridge payloads and never count as command completion.
- Background routes are capability-filtered. iOS background limitations and Android activity/FGS
  restrictions are part of the contract, not transient errors to brute-force.
- Every visible action is based on a recent screenshot and is followed by verification when the
  outcome matters.
- User approval is required for protected system actions and consequential external actions. Do not
  work around unlock, login, permission, payment, or verification-code boundaries.
- Secrets and personal data stay out of skills, logs, memory, and user-facing diagnostics unless
  explicitly requested and authorized by the tool contract.

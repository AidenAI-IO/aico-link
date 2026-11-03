---
sidebar_position: 3
---

# Config Web Management API

Config Web is the `config-web` subcommand of the Go Agent binary. Config Web
and the Agent runtime listen on ports 80 and 8080 respectively, and each owns
its own PID and service lifecycle. Config Web calls the Agent's loopback-only
reload endpoint after configuration is saved, while the Agent reads Config
Web's storage-state mirror for archive lookup. Config Web does not proxy Agent
chat, session, or Phone Bridge traffic.

## Resource Endpoints

All public endpoints use the `/api` root without an additional version prefix:

| Resource | Method and path | Description |
| --- | --- | --- |
| Configuration | `GET /api/config` | Read the resolved `agent.toml` configuration |
| Configuration | `PATCH /api/config` | Persist a merge patch and queue application |
| Configuration | `POST /api/config/plan` | Validate a merge patch without saving and report its apply level |
| Configuration | `GET /api/config/application` | Read pending, applied, failed, or reboot-required state |
| Configuration | `GET /api/config/schema` | Read field types, defaults, choices, secret markers, and restart hints |
| Configuration | `PUT /api/config/locale` | Update the page language |
| Configuration | `POST /api/config/test` | Validate configuration and the device environment without saving |
| Configuration | `GET /api/config/backup` | Download the persisted grouped `agent.toml` backup |
| Configuration | `PUT /api/config/backup` | Validate, atomically restore, and apply a grouped TOML backup |
| Memory | `POST /api/memory/reset` | Clear conversation history and persisted memory, then restart the Agent |
| Conversation | `POST /api/conversation/reset` | Clear the current conversation history only; memory is kept and the Agent keeps running |
| Models | `GET /api/models?provider=...&locale=...` | Return the localized model catalog |
| STT test | `POST /api/config-test/stt/start` | Start a microphone recording test using the submitted unsaved settings |
| STT test | `POST /api/config-test/stt/stop` | Stop recording and return the transcription result |
| Storage | `GET /api/storage/status` | Read SD/eMMC state and formatting tasks |
| Storage | `POST /api/storage/format` | Format the SD card asynchronously |
| Storage | `POST /api/storage/eject` | Sync and safely eject the SD card |
| Device | `GET /api/device/snapshot` | Read the aggregated initial-page model: configuration, Wi-Fi, device, firmware/component versions, and storage summaries |
| Device | `GET /api/device/status` | Read the device, firmware/component versions, Agent process, USB/HID, and capability summary |
| Device | `POST /api/device/reboot` | Reboot the device |
| Agent | `POST /api/agent/restart` | Restart the Agent process; Config Web and the USB link stay up |
| Device | `POST /api/device/usb/reenumerate` | Re-enumerate USB HID/ECM |
| Network | `POST /api/network/wifi/scan` | Scan for nearby Wi-Fi networks |
| Network | `PUT /api/network/wifi/connection` | Start a bounded asynchronous connect-and-save task |
| Network | `GET /api/network/wifi/connection?task_id=...` | Poll connection, persistence, and rollback status |
| Network | `DELETE /api/network/wifi/connection?ssid=...` | Forget a network without relying on a DELETE request body |
| System | `GET/PUT /api/system/environment` | Read/save environment and report whether Agent restart is needed |
| System | `POST /api/system/environment/apply` | Explicitly apply saved environment and restart Agent |
| OTA | `GET /api/ota/status` | Read the current state, progress, and log summary |
| OTA | `POST /api/ota/updates` | Create an OTA task and return its `task_id` |
| Logs | `GET /api/logs/agent` | Read the Agent log summary |
| Logs | `GET /api/logs/llm` | List available LLM HTTP log files |
| Logs | `GET/PUT /api/logs/llm/{name}` | Export a raw LLM HTTP log or import one with the same name |
| Logs | `GET /api/logs/support` | Export a diagnostic support-log archive |

All endpoints above are served by Config Web on port 80. The page uses only
same-origin management requests. Agent port mappings therefore do not affect
the configuration portal, and the Agent does not expose duplicate models, STT
test, or storage-management handlers on port 8080.

The endpoints that restart or reset something from a plain POST (`POST
/api/agent/restart`, `/api/device/reboot`, `/api/memory/reset`,
`/api/conversation/reset`) and `POST /api/config/plan` refuse cross-site
requests with `403`: a browser request whose `Sec-Fetch-Site` is `cross-site`,
or whose `Origin` names another host. Requests without an `Origin` (curl, the
companion app's native code) are accepted.

Paths under the settings sections (`/wifi`, `/basic`, `/conversation`,
`/model`, `/voice`, `/memory`, `/storage`, `/advanced`, `/firmware`, and
anything below them) return the settings page so a page can be reloaded or
linked directly; any other unknown path is `404`.

`POST /api/network/wifi/scan` keeps the legacy `networks` string array and also
returns `network_details`. Each detail contains `ssid`, `secured`, and, when
the driver reports them, `signal_dbm` and `signal_percent`. The current
connection in `wifi_status` likewise includes `signal_dbm` when `iw link`
reports it. Signal values are observations from the scan/link operation and
may be absent on drivers that do not expose them.

`GET /api/config` and `GET /api/device/snapshot` include `config_valid` and a
`config_errors` array of `{field, message}` objects. Semantic validation errors
do not make these endpoints unavailable; they identify fields the recovery UI
must highlight. A field the page does not render, or a message no validator
attributed to one, reports an empty `field` and is surfaced in the details
instead. An `agent.toml` that cannot be read or decoded returns an unavailable
response, because there is no editable configuration to construct. A file that is
missing altogether is not a validation failure: the runtime reads it as the
built-in defaults, the page renders those defaults, and saving creates the file.

`GET /api/storage/status` and the `storage` field of
`GET /api/device/snapshot` share the following response shape:

```json
{
  "effective_mode": 1,
  "card": {
    "present": false,
    "mounted": false,
    "device": "",
    "total_bytes": 0,
    "free_bytes": 0,
    "reason": ""
  },
  "mount_point": "/mnt/sdcard",
  "format_job": {"status": "idle"},
  "migration": {"status": "idle"}
}
```

`format_job` is asynchronous and its status can be `idle`, `running`,
`success`, or `failed`. `migration` represents the background migration from
eMMC to SD and uses the same status values. A card can be present without being
mounted, so clients must use `card.present` and `card.mounted` independently
when enabling the format and eject actions.

## Configuration Save Response

`PATCH /api/config` accepts only `{"config":{...}}`. Retired request fields
(`apply_wifi`, `agent.default_platform`, `agent.instruction`, flat provider credentials,
`model.base_url`, `audio.playback_backend`, provider-record `provider` aliases) are rejected. Provider
records use `type`; credentials belong in their provider record. The response
uses `reboot_required`, without the former restart aliases.

A successful `PATCH /api/config` persists the file and queues runtime application:

```json
{
  "ok": true,
  "persisted": true,
  "applied": false,
  "pending": true,
  "state": "pending",
  "revision": 123,
  "changed_paths": ["model.model"],
  "reboot_required": false,
  "reboot_reasons": [],
  "apply": "live",
  "apply_reasons": [],
  "agent_restart_scheduled": false
}
```

Poll `GET /api/config/application` for `state`, `pending`, `applied`,
`reboot_required`, `revision`, `applied_revision`, and optional `error`.
`persisted` confirms the atomic file write; `applied` confirms all settings in
that revision are active. Revisions are opaque 64-bit values; browser clients
should use the returned state rather than perform numeric revision arithmetic.

| State | Meaning |
| --- | --- |
| `pending` | Waiting for the current task, tool operation, or affected voice session to finish |
| `applied` | The saved revision is active |
| `reboot_required` | Online settings are active; USB identity/layout changes await an explicit device reboot (`applied=false`) |
| `failed` | Application failed; inspect `error`, correct the configuration, and retry |

At daemon startup the status is initialized from the saved configuration and
the USB settings active for the current device boot.
The UI keeps persisted values after an application failure and offers **Retry
apply** (`PATCH /api/config` with `{"config":{}}`). Synchronous persistence,
service, or reload-request failures return an error HTTP status; asynchronous
component failures are reported by the status endpoint. Neither schedules an
Agent restart. Frame/storage application may have completed before a later
component fails. Failed frame/storage work is retried on the next save.
The status may include a `runtime_id` identifying the answering Agent process.
Reload-request errors are tied to the requested revision and clear once Agent
reports that revision as applied or awaiting USB reboot. Frame/storage failures
require a successful service retry.

`reboot_required` indicates USB descriptor changes
(Android versus non-Android device type) and `hid.keyboard_layout`. The runtime
keeps the current USB settings until reboot. A boot-ID-scoped cache at
`<config-dir>/cache/usb-boot.json` and the bound USB descriptor preserve the
exception across subsequent saves and Agent-only restarts; reverting those settings clears it. Reboot is an explicit
user action and is never triggered by saving; see Apply Levels for how pages ask for it. CLI `--device-type` continues to
override the file for the current process.

## Apply Levels

Every saved change has one apply level, defined in the Agent
(`agent.ApplyLevel`, `src/agent/internal/agent/apply_level.go`) so that Config
Web, the settings pages, and the companion app never keep their own lists:

| `apply` | Meaning | Carried out by |
| --- | --- | --- |
| `live` | Hot-reloaded by the running Agent | Nothing further |
| `agent_restart` | Read only at Agent start (the system environment file) | `POST /api/agent/restart`; the page and USB link stay up |
| `reboot` | Changes the USB gadget the phone enumerated (`apply_reasons`: `pointer_mode`, `keyboard_layout`) | `POST /api/device/reboot`; every connection to the device drops |

A reboot subsumes an Agent restart. The level depends on the resulting values,
not on which field was edited: moving the device type between iOS, macOS,
Windows and Linux is `live`, while moving to or from Android is `reboot`.

`POST /api/config/plan` takes the same `{"config":{...}}` body as
`PATCH /api/config`, runs `agent config-update --dry-run`, and writes nothing:

```json
{"ok": true, "apply": "reboot", "apply_reasons": ["keyboard_layout"], "changed_paths": ["basic_settings.device.hid.keyboard_layout"], "reboot_required": true}
```

Settings pages call it as the user edits. A `live` change is saved as soon as
it is committed and the page shows no save button; a change at either restart
level is staged, and the page offers "Save & Restart" (`apply.save_restart`) for everything staged. Tapping it opens a
confirmation that names the restart; only after the user confirms does the page
save and then call the restart endpoint, so a restart remains an explicit user
action and `PATCH /api/config` itself never restarts anything. The `apply` in
the save response is authoritative. `GET/PUT /api/system/environment` report
`apply` too.

Inside the companion app the page reports the level to the native navigation
bar with `aico_config_save_state` `{title, dirty, apply, action_label}`
(`title` and `action_label` already translated into the device's language, since
the app keeps no catalogue for them), and announces a
restart with `aico_config_restart` `{apply, phase}` (`started`, `finished`,
`failed`). During a `reboot` the app on Android re-binds the USB network until
the page reports the restart finished, because the process otherwise stays
bound to the network that disappeared.

## Agent Restart Lifecycle

`PUT /api/system/environment` persists the file and returns
`agent_restart_required`; it does not restart Agent. `GET` reports the same flag
by comparing the saved file with the environment revision loaded by Agent.
When Agent status is unavailable, `GET` returns `null` for the flag; the UI keeps
the last confirmed requirement. A save without Agent confirmation requires apply.
The flag survives Config Web and Agent process restarts and clears only when the
saved environment has been loaded. `POST /api/system/environment/apply` is the
explicit **Apply and restart Agent** action: it refreshes the Wi-Fi proxy and
then schedules Agent restart. Proxy/restart failures return an error and remain
retryable. Config changes use the online application lifecycle above.

## Storage Ownership

Config Web is the sole owner of `StorageManager`: it detects and mounts the SD
card, runs format/eject operations, migrates governed data, and writes
`/run/aico/storage.state`. The Agent runtime never performs those hardware
operations. It uses a read-only state view to include the SD archive root when
serving previously migrated audio.

## Wi-Fi Task Lifecycle

`PUT /api/network/wifi/connection` returns HTTP 202 with a `task_id` immediately.
The task has a 120-second total deadline covering candidate application,
verification, persistence, and rollback. Clients poll the GET form until the
status is `succeeded` or `failed`; failure responses distinguish the candidate
apply result from the rollback result.

Failed connection task responses include a stable `failure_reason` string. The
value is `wrong_password` only when `wpa_supplicant` reports a `WRONG_KEY`
control event for the requested SSID. Other values are `association_failed`,
`ip_unavailable`, `apply_failed`, `persistence_failed`, `timeout`,
`connection_unconfirmed`, or `configuration_error`. Clients must use this
field for user-facing error states instead of treating every `ok: false` as a
bad password.

The PUT body can include `proxy_mode` with `system`, `direct`, or `proxy`.
`proxy` also requires `proxy_url` using HTTP, HTTPS, or SOCKS5. On success the
policy is saved by SSID in `/userdata/system/wifi-proxies.json`; connection
failure rolls back both Wi-Fi and proxy changes. Snapshot and task responses
return a redacted proxy URL, and DELETE removes the SSID's proxy mapping along
with its Wi-Fi credentials.

`no_proxy` is accepted only with `proxy_mode=proxy` and defines the bypass
rules for that SSID's custom proxy. `proxy_mode=system` uses the proxy and
`NO_PROXY` values from `/userdata/system/env` without a per-Wi-Fi override.

## API Boundary

Retired routes such as `/api/wifi/*`, `/api/system/env`,
`/api/ota/update`, `/api/reboot`, `/api/agent/*`, and `/api/llm-logs/*` have
been removed and return `404 Not Found`; no compatibility adapters are
provided. Clients must use the canonical resources in the table above.
`GET /api/config` has only configuration semantics; use
`/api/device/snapshot` for the aggregated initial page load.

## Internal Agent Reload

`POST /api/internal/config/reload` accepts loopback requests only and requires
`{"revision":<nonzero revision>}`. The Agent validates the saved file revision, queues application,
and returns HTTP 202 with `pending=true`. `GET` on the same loopback-only route
returns application status. A stale revision returns HTTP 409. Pending saves
are coalesced; the latest queued snapshot wins after the current application.
A body that fails to decode, or that carries an unknown field, returns HTTP 400
with the decoder's reason, so a request that did include a revision is never
reported as a missing one.
These endpoints are not part of the public management API.

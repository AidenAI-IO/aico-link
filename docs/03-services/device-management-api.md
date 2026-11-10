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
| Security | `GET /api/security/lan-access` | Read the LAN access switch and whether a device password is set |
| Security | `PUT /api/security/lan-access` | Turn LAN access on or off and set the device password; USB only |
| Security | `GET /login` | Sign-in page for LAN browsers |
| Security | `POST /api/auth/login` | Exchange the device password for a session token |
| Security | `POST /api/auth/logout` | Clear the session cookie |
| Security | `GET /api/auth/session` | Report whether the request is signed in |
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

## LAN Access

LAN access decides whether the board answers clients that are not on the USB
link. It is off by default. While it is off:

- Config Web (port 80), the Agent (port 8080, including `/api/tools/*`, Phone
  Bridge, and `/webtty/`) refuse every request that neither arrives through
  their socket bound to `usb0` nor comes from the board itself. `/mcp` is not
  affected by the switch; see [MCP Access Tokens](#mcp-access-tokens). API
  clients receive `403` with `"error": "lan_access_disabled"`; browsers receive
  a short page that explains how to turn it on.
- sshd refuses every source outside `192.168.42.0/24`, `127.0.0.0/8`, and
  `::1`, for every account including `root`.
- ttyd listens on loopback only. USB clients use `http://192.168.42.1:3000/webtty/`,
  which Config Web serves on a `usb0`-bound socket; LAN clients use the Agent's
  `/webtty/`, which follows the switch.

While it is on, Config Web and the Agent require LAN clients to sign in with
the device password (see [LAN Sign-In](#lan-sign-in)); SSH and the web terminal
ask for the same password as the `aico` login password. USB clients and the
board itself never sign in. `/mcp` follows its own rule whether LAN access is
on or off: every client, USB and on-board included, sends an
[MCP access token](#mcp-access-tokens).

### Cross-Site Requests

USB clients need no credentials, but the phone or computer at the other end of
the cable also runs a browser, and a page it loads can send requests to
`192.168.42.1` that arrive on the USB socket like the app's own. Config Web,
the Agent, and the USB terminal on port 3000 therefore refuse browser requests
made on behalf of another site, before any other check and on every
connection, with `403` and one of these errors:

| Error | Request |
| --- | --- |
| `cross_site_request` | `Sec-Fetch-Site: cross-site`, except a top-level `GET` navigation (`Sec-Fetch-Mode: navigate`, `Sec-Fetch-Dest: document`), such as following a shared-file link |
| `host_not_allowed` | `Host` is a name other than `localhost`, not an IP address (DNS rebinding) |
| `origin_not_allowed` | `Origin` is `null`, is not an `http`/`https` URL whose host is an IP address or `localhost`, or does not match `Host` |

Native clients (the companion app, MCP hosts, `curl`) send neither `Origin` nor
`Sec-Fetch-Site` and address the board by IP, so they are unaffected. Open the
board by its IP address; a hostname such as an mDNS `.local` name is refused.

`GET /api/security/lan-access` never returns the password or its hash:

```json
{"ok": true, "enabled": false, "password_set": false, "via_usb": true, "min_length": 8}
```

`PUT /api/security/lan-access` is accepted only over the USB link (`403`
otherwise) and only from the same origin. The body contains `enabled`,
`password`, or both:

```jsonc
{"enabled": true, "password": "8 characters to 72 bytes"} // first switch-on
{"enabled": true}                                       // a password is already set
{"password": "new password"}                            // change the password
{"enabled": false}                                      // turn off, keep the password
```

Turning LAN access on without a stored or submitted password returns `400`.
No current password is required: whoever holds the USB cable owns the board,
and a forgotten password is replaced over USB. The response repeats the status
and adds `system_synced`, which is `false` when SSH or the login account could
not be updated. The HTTP services follow the stored switch immediately either
way. For SSH:

- If the device password could not be applied, the rule file keeps SSH on the
  USB link even when the switch is on, so the image's default `aico` password
  is not opened to the LAN.
- If sshd did not confirm the reload, the running sshd may still use its
  previous rule, for example staying open to the LAN after the switch was
  turned off. Every later sync reloads sshd again, whether or not the rule
  file changed, and sshd reads the rule file whenever it starts.

When `system_synced` is `false`, check `systemctl status aico-lan-access ssh`
and `/run/aico/sshd/lan.conf`, then retry the change over USB and check the new
`system_synced`. Each retry applies the password and reloads sshd again.
Restarting sshd only helps when the reload failed: it rereads the rule file but
does not apply the password, because `aico-lan-access.service` runs once per
boot. That unit retries on every boot, but a persistent failure (for example a
damaged password file) fails again.

A device password must be at least 8 characters and at most 72 bytes in UTF-8
(the bcrypt limit), so a password made of multi-byte characters can be
rejected with `400` before it reaches 72 characters.

State lives on userdata so it survives OTA:

| File | Content |
| --- | --- |
| `/userdata/system/lan-access-enabled` | Present while LAN access is on |
| `/userdata/system/device-password` | bcrypt hash of the device password (mode 0600) |
| `/userdata/system/lan-session-secret` | Key that signs LAN session tokens (mode 0600) |
| `/userdata/system/mcp-tokens.json` | Names, creation times, and SHA-256 hashes of MCP access tokens (mode 0600) |

`/usr/lib/aico/aico-lan-access-sync` applies the state: it writes
`/run/aico/sshd/lan.conf` (included last by `/etc/ssh/sshd_config.d/20-aico.conf`),
sets the `aico` password to the device password, and reloads sshd on every
run. Config Web waits for the reload to complete; the boot unit does not,
because `ssh.service` is ordered after it. Until a
device password is set, the account keeps the image's default password.
`aico-lan-access.service` runs it on every boot, because `/run` is volatile and
OTA resets `/etc/shadow`; `ssh.service` requires that unit so sshd never starts
without the rule. Config Web runs the helper after every change.

Docker and host runs of the Agent and Config Web do not pass `-lan-guard` /
`--lan-guard`, so they are not affected.

### LAN Sign-In

While LAN access is on, Config Web (port 80) and the Agent (port 8080) serve
the sign-in endpoints themselves and refuse every other LAN request without a
valid session token:

- Browsers (`GET` with `Accept: text/html`) are redirected with `303` to
  `/login?next=<original path>`.
- Other clients receive `401` with `"error": "login_required"` and
  `WWW-Authenticate: Bearer`.

While LAN access is off, the sign-in endpoints are refused like every other LAN
request.

`POST /api/auth/login` takes the device password:

```jsonc
// Content-Type: application/json
{"password": "device password"}
// 200
{"ok": true, "token": "v1.<expiry>.<issued>.<signature>", "expires_at": "2026-12-09T08:00:00Z"}
```

The response also sets the `aico_session` cookie (`HttpOnly`,
`SameSite=Strict`, `Path=/`). The `/login` page posts an HTML form to the same
endpoint and is redirected to its `next` path, which must be a path on the same
host. Errors are `401 invalid_password`, `403 cross_origin_rejected` (an
`Origin` header naming another host), and `429 too_many_attempts` with
`Retry-After`: after five failures from one address, each further failure
doubles a lockout that starts at 30 seconds and stops at 15 minutes. A
successful sign-in clears the count.

Requests authenticate with `Authorization: Bearer <token>` or the
`aico_session` cookie. Cookies are scoped to the host, not the port, so a
browser signed in on port 80 is also signed in on port 8080, including
`/webtty/` and the Phone Bridge WebSocket. Scripts send the bearer token. A
session token does not reach `/mcp`; MCP clients use an
[MCP access token](#mcp-access-tokens).

A token is valid for 30 days. A browser that uses its cookie more than a day
after it was issued receives a fresh 30-day cookie, so a browser in regular use
stays signed in. Tokens are not stored: they are signed with
`/userdata/system/lan-session-secret`, which the Agent and Config Web share.
Setting a new device password or turning LAN access off replaces that key and
signs every LAN client out. `POST /api/auth/logout` only clears the cookie; a
bearer token stays valid until it expires or the key is replaced.

`GET /api/auth/session` returns `{"ok": true, "via_usb": false,
"authenticated": true}`.

The board serves plain HTTP, so anyone who can capture traffic on the local
network can capture the password and the token. Turn LAN access on only on
trusted networks. The same applies to MCP access tokens, which travel over
Wi-Fi even while LAN access is off: create them only on trusted networks and
revoke any you no longer use.

### MCP Access Tokens

`/mcp` on the Agent needs an MCP access token from every client, including
a computer plugged straight into Aico Link and processes on the board, and
accepts nothing else: not a session token, not the USB link alone. It does not
depend on LAN access, so a computer on the same Wi-Fi can use MCP while LAN
access stays off. Creating a token, which needs USB or a signed-in session,
is what opens MCP. An MCP access token:

- reaches only `POST /mcp` on port 8080. It is refused everywhere else,
  including `/api/tools/*` (which would bypass the MCP tool allowlist and the
  `mcp_shell_enabled` gate), Config Web, backup, the terminal, and SSH;
- does not expire, and survives a password change and LAN access being
  turned off. Each token is named when it is created and revoked on its own.
  Turn MCP off (`mcp_enabled`) to stop every client at once.

Create and revoke tokens in Settings > Advanced > MCP, or with these
endpoints. They are accepted over USB or from a LAN
client signed in with the device password; a token never manages tokens.
Writes must come from the same origin.

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/api/security/mcp-tokens` | List tokens (never their secret or hash) |
| `POST` | `/api/security/mcp-tokens` | Create a token: `{"name": "MacBook Claude Code"}` |
| `DELETE` | `/api/security/mcp-tokens/{id}` | Revoke one token |

```jsonc
// POST /api/security/mcp-tokens → 201
{"ok": true,
 "token": {"id": "3f9a1c0b7e2d", "name": "MacBook Claude Code", "created_at": "2026-11-10T08:00:00Z"},
 "secret": "aico_mcp_3f9a1c0b7e2d_<43 characters>"}
// GET /api/security/mcp-tokens → 200
{"ok": true, "tokens": [{"id": "3f9a1c0b7e2d", "name": "MacBook Claude Code", "created_at": "2026-11-10T08:00:00Z"}],
 "max_tokens": 20}
```

The secret is returned once and is not stored: the board keeps only its
SHA-256 hash. A name is 1 to 64 characters on one line; at most 20 tokens
exist at a time (`400` otherwise). Revoking an unknown ID returns `404`. A
missing or damaged token file accepts no token. Clients send the secret as
`Authorization: Bearer <secret>`; see [MCP](../04-agent/mcp.md#authentication)
for client configuration.

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

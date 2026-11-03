---
sidebar_position: 2
---

# Config Web: Web-based Configuration Interface

Config Web is the bundled browser client served by the `config-web` subcommand
of the Go Agent binary (`/usr/lib/aico/agent`). Device operations are exposed by
the independently mountable [Device Management API](device-management-api.md),
so the page can be replaced or removed without coupling those operations to the
static UI. Configuration changes are persisted to
`/userdata/agent/agent.toml` and applied online. The page shows whether settings
are pending, applied, failed, or waiting for a device reboot.

Model discovery, STT configuration tests, storage operations, and all other
device-management calls are served directly by Config Web on port `80`. The
page does not construct cross-port Agent URLs, so Docker host-port overrides
and USB-ECM access use the same API behavior.

## Default Parameters

| Parameter | Default Value |
| --- | --- |
| Port | `80` |
| Config | `/userdata/agent/agent.toml` |
| Binary | `/usr/lib/aico/agent config-web` |
| Web root | `/usr/share/aico/config-web` |

## Startup

```bash
systemctl start aico-config-web.service
systemctl stop aico-config-web.service
systemctl restart aico-config-web.service
systemctl status aico-config-web.service
```

The service is enabled through the Debian systemd preset and is also pulled in
by `aico.target`.

Config Web treats the Agent configuration as a recovery resource. A readable,
parseable `agent.toml` with invalid values does not stop port 80 from starting:
the page reports the configuration as invalid, highlights the responsible
field, and keeps editing available. Startup fails only when the configuration
file itself cannot be read or decoded, such as a permissions error, a damaged
TOML document, or an invalid file target.

## Access

Connect the device to your computer via USB-C. The device establishes a USB network at `192.168.42.1`. Visit:

```text
http://192.168.42.1
```

The web interface allows:
- Opening the browser terminal exposed by ttyd at `http://192.168.42.1:3000/webtty/`
- Switching the device language between Simplified Chinese (`zh-CN`) and English (`en-US`); this also controls user-facing Agent responses and `<tts>` content
- Switching among registered model provider types such as OpenAI, Anthropic, OpenRouter, Kimi, Volcengine, and Ollama; Google Gemini models are available through compatible providers such as OpenRouter
- Configuring API keys and model names
- Selecting STT/TTS providers
- Testing voice recognition and synthesis
- Applying saved settings online, retrying failures, and explicitly rebooting only when USB settings require it
- Applying saved settings online whenever the running Agent supports the change; documented restart exceptions are surfaced explicitly in the UI
- Saving a system-default, direct, or custom proxy policy with each Wi-Fi network; custom proxy credentials are never returned to the browser

The Wi-Fi dialog accepts `http://`, `https://`, and `socks5://` proxy URLs. A
saved policy is activated automatically when that SSID becomes current. The
Agent, OTA commands, managed subprocesses, and login shells continue to use
the fixed local address `127.0.0.1:18080`. That address accepts both HTTP and
SOCKS5, and the local URL scheme matches the selected upstream scheme. Switching
between HTTP and SOCKS5 restarts the Agent so its long-lived clients use the
matching protocol; switching between proxies of the same type does not. The
local SOCKS5 URL is rendered as `socks5h://` to resolve target hostnames through
the proxy; this does not change the SOCKS5 wire protocol.

`NO_PROXY` in the Wi-Fi dialog belongs only to that SSID's custom proxy. When
`Use system default` is selected, both the upstream proxy and `NO_PROXY` come
from `/userdata/system/env`; no Wi-Fi-specific `NO_PROXY` value is stored or
merged into the system setting.

The header switch saves only the top-level `locale` through
`PUT /api/config/locale`. The UI updates immediately and rolls back if
persistence fails. The last confirmed value is cached in `localStorage` for
first paint, but `GET /api/device/snapshot` remains authoritative. The next Agent task
creates a new context session when the locale-specific system prompt changes; it does not rewrite the previous session. `locale` is intentionally
separate from `[voice_settings.classic.stt].language`, which only configures speech recognition.

## Configuration Application Policy

| Category changed by this implementation | Application boundary |
| --- | --- |
| Ordinary settings: locale/prompts, iteration and context limits, screenshot retention, telemetry, log retention, notifications, Live Activity, quick-capture TTL | Published online for subsequent tasks/operations; shared managers keep their identity |
| Model Settings: ordinary model/provider, credentials, endpoint, and model options | Wait for the current Agent task to finish, then replace the model client; subsequent tasks use the new settings even while a voice session remains open |
| Components: input mode, STT/TTS/realtime voice and their provider credentials, audio/VAD, HID clients, capture backend, search, quick-capture GPIO, storage policies | Drain affected work, prepare replacements, and switch components without restarting Agent; HTTP and Phone Bridge remain alive |
| `frame_service.keep_streamon` | Restart only `frame_service`, wait for its listening socket, then queue the Agent snapshot; capture is briefly unavailable |

Existing exceptions remain: Android/non-Android USB pointer descriptor changes
and `hid.keyboard_layout` need a device reboot. Other device-type changes can
apply online. Saving system environment variables shows **Apply and restart Agent**; only
that explicit action restarts Agent. Deployment
of a new binary still requires restarting Agent and Config Web; that is separate
from changing runtime settings.

Voice component changes wait for the current voice session. Ordinary model
changes do not restart or drain the voice loop. Failed replacements
retain the previous runtime configuration and drained dialog. A held drag must
be released before HID component replacement can succeed. Old model requests
and TTS sessions retain their current provider until completion. Prompt/provider
changes open a new context session at the next task/session boundary and leave
existing transcript files intact.

The editor shows the persisted configuration even when runtime application
fails. Provider records and references remain saved; the application status
reports the error, and **Retry** reapplies the complete saved configuration.
An earlier failed voice-mode change can therefore also block a later model
save until the voice dependency is repaired or that saved change is reverted.
Saving a section preserves unsaved drafts in other fields. Manual TOML edits
and configuration imports use the same save queue and application status as
form and provider edits.

USB restart requirements survive Agent-only restarts: the bound USB descriptor
and `<config-dir>/cache/usb-boot.json` retain the active settings for the current
Linux boot ID. A real device reboot accepts the saved USB identity and layout.
Failed Quick Capture GPIO replacement retains the active configuration, so the
same saved configuration can be retried after the hardware fault is corrected.

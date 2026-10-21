---
sidebar_position: 3.5
---

# MCP (Streamable HTTP)

The Agent HTTP server exposes Model Context Protocol over
[Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
at a single endpoint on the device.

## Endpoint

| Method | Path | Description |
| --- | --- | --- |
| `POST` | `/mcp` | MCP JSON-RPC (initialize, tools/list, tools/call, …) |

URL examples:

- Wi-Fi: `http://<board-ip>:8080/mcp`
- USB ECM: `http://192.168.42.1:8080/mcp`

Point an external MCP client at that URL. If the board IP changes (DHCP), update the client URL. Bypass HTTP proxies for the device address (`NO_PROXY` / `no_proxy`).

## Enable / disable

| Setting | Default | Where |
| --- | --- | --- |
| `mcp_enabled` | `true` | `[conversation_settings.agent]` in `agent.toml`, or Config Web → Advanced |
| `mcp_shell_enabled` | `false` | `[conversation_settings.agent]` only (TOML; not on Config Web) |
| `mcp_config_web_base_url` | `http://127.0.0.1` | Loopback HTTP only; used by `device_status` / `support_bundle` |

When `mcp_enabled=false`, `POST /mcp` returns 404. Voice and other HTTP APIs are unchanged. There is no authentication in this release; use a trusted LAN/USB link or turn the switch off.

## Allowlist

`tools/list` returns only these tools (plus `shell` when `mcp_shell_enabled=true`):

| MCP tool | Backend |
| --- | --- |
| `phone_bridge_status` | Agent phone-bridge status |
| `screenshot` | `POST /api/tools/screenshot` |
| `wait_for_stable_screen` | `POST /api/tools/wait_for_stable_screen` |
| `touch_gesture` | `POST /api/tools/touch_gesture` |
| `enter_text` | `POST /api/tools/enter_text` |
| `keyboard_tap` | `POST /api/tools/keyboard_tap` |
| `open_app` | `POST /api/tools/open_app` |
| `open_url` | `POST /api/tools/open_url` |
| `quick_action` | `POST /api/tools/quick_action` |
| `bridge_clipboard` | `POST /api/tools/bridge_clipboard` |
| `shell` | `POST /api/tools/shell` (gated) |
| `device_status` | config-web `GET /api/device/status` |
| `support_bundle` | config-web `GET /api/logs/support` (embedded resource blob; one in-flight call at a time, extras rejected) |

Use MCP `tools/list` for discovery. Do not expect a separate `list_tools` tool. `GET /health` is not exposed over MCP.

## Transport notes (2026-07-28 Streamable HTTP)

- Dual-era: legacy clients may use `initialize` and older `MCP-Protocol-Version` values (`2025-*`) without `params._meta`.
- Modern (`2026-07-28`) clients must send `MCP-Protocol-Version`, `Mcp-Method`, and (for `tools/call`) `Mcp-Name`, matching `params._meta` / body fields. Mismatches return HTTP 400 with JSON-RPC `-32020`.
- Unknown methods return HTTP 404 with JSON-RPC `-32601`.
- Responses are `application/json` (SSE streams not used yet). No `subscriptions/listen` / MRTR in this release.

## Origin

Clients that omit `Origin` (typical MCP host apps / curl) are accepted.

If the client sends an `Origin` header (browser requests), it must:

1. Use a non-rebindable hostname: an IP literal or `localhost`
2. Match the request `Host` (scheme-insensitive `host:port`)

This blocks ordinary cross-origin browser calls and rejects rebound attacker names even when `Origin` and `Host` agree. It is not a full Host allowlist; point browsers at the board by IP (or `localhost` over USB/loopback). There is still no authentication on `/mcp` in this release.

## Related

- Tools HTTP API (same Runtime tools): [tools-http-api.md](./tools-http-api.md)

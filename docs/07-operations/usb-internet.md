---
sidebar_position: 8
---

# Internet over USB

When board Wi-Fi is unavailable, a USB-connected Mac or Android phone can
provide the Agent, HTTP/HTTPS APIs, WebSockets, and downloads with Internet
access. This uses the existing ECM link and the board's local proxy; it does
not change USB HID, DHCP, or the board address `192.168.42.1`.

## Enable and use

Open **Config Web → Advanced Settings → USB Internet** over the USB connection.
The switch is **off by default**, including when its settings file is absent.
The same switch controls Mac and Android. It is saved immediately and survives
reboots. Turning it on displays "Waiting for Internet sharing" below the title
until a host registers a session. Platform instructions appear below the switch
row and wrap to fit phone screens.

- **Mac:** run `scripts/usb-internet-macos.sh start` from this firmware checkout.
  The helper runs in the background and follows the board switch. It never
  enables the switch itself. It reconnects after cable loss or board restart.
- **Android:** install an App build containing `UsbInternetRelay`, connect the
  board, and open Aico App. Its existing `connectedDevice` foreground service
  reads the board switch and starts/stops the native relay automatically.
  Keep that service running for background use. The notification indicates
  when the phone is sharing. Validated Wi-Fi is preferred over cellular data;
  cellular sharing can consume the phone's data allowance.

Turning the board switch off revokes the session and closes established proxy
connections immediately. The host removes its listener on its next status
check (normally within five seconds). The original Wi-Fi/system proxy policy
then applies. Enabling the switch again requires a new live session.

The setting can be changed only through the USB-bound Config Web listener or
from a local board process. An authenticated LAN settings page can read it but
cannot change it. A failed save restores the last confirmed switch state.

### Mac helper

Requirements: Python 3.8+, OpenSSH 7.6+, trusted board SSH host key, and working
key-based access as `aico`. Neither Mac administrator access nor board sudo is
needed by the helper. Board services perform the authorized setting and clock
operations themselves.

```bash
scripts/usb-internet-macos.sh start
scripts/usb-internet-macos.sh status
scripts/usb-internet-macos.sh check
scripts/usb-internet-macos.sh stop
```

An optional second argument is an SSH config alias or `user@host`.
`start` starts a host worker, even if the board switch is currently off.
`status` reports worker state; `check` verifies an active session with a real
HTTPS request. `stop` stops the host worker without changing the board switch.
Worker state and a short state-transition log are under
`~/.cache/aico-usb-internet/`. They contain no proxy passwords.

```text
Agent / managed command
  -> board 127.0.0.1:18080 (existing local HTTP/SOCKS proxy)
  -> board 127.0.0.1:18081 (SSH reverse dynamic listener)
  -> USB ECM / SSH
  -> Mac DNS + TCP connection
  -> Internet
```

The helper creates the reverse listener only while sharing is enabled. A
local `ssh -D` would create the SOCKS listener on the wrong end of this link.
Host sleep pauses service; resume/cable reconnect triggers a retry. The helper
is not installed as a login item and must be started again after a Mac reboot.
SSH opens normal host sockets, so browser-only HTTP proxy settings do not
necessarily provide a usable upstream for the helper.

### Android relay

The phone remains the USB **host**, and the board remains an ECM/HID USB
**device**. Do not enable Android's system USB tethering: its usual USB roles
are the opposite of this topology.

```text
Agent / managed command
  -> board 127.0.0.1:18080
  -> phone's DHCP-assigned ECM address:18081
  -> authenticated native SOCKS5 CONNECT relay
  -> DNS and socket explicitly bound to phone Internet Network
  -> Wi-Fi / cellular / validated VPN
```

The listener binds only to the phone's USB IPv4 address and accepts only the
board peer `192.168.42.1`. Each session uses SOCKS credentials consisting of the
username `aico` and a random password, kept only in memory. The board derives
the proxy address from the control request's USB peer, never from a supplied
host URL. The App does not assume a fixed phone DHCP address.

The existing React Native process may remain bound to the board network.
Every relay lookup uses `Network.getAllByName`, and every outgoing socket uses
`Network.bindSocket` before connecting to a numeric result. The App never
switches process-wide binding around Internet requests. It prefers validated
Wi-Fi, then cellular; if a VPN network exists, it requires a validated VPN
instead of silently using an underlying physical network.

Native workers limit the relay to 32 simultaneous tunnels and bounded buffers.
SOCKS5 CONNECT accepts domain, IPv4, and IPv6 targets; BIND and UDP ASSOCIATE
are refused. Half-close preserves responses after upload completion. USB loss,
upstream changes, service shutdown, and a disabled board switch close sockets.
An upstream change interrupts existing requests; application retry rules apply.
OEM background restrictions, VPN lockdown, and ECM driver availability still
need testing on the actual target phone.

## Session and configuration contract

The proxy process owns the switch and leases. Config Web forwards approved
requests through a root-only Unix socket at
`/run/wifi_proxy/usb-internet/control.sock`. The switch is stored at
`/userdata/system/usb-internet.json`. Sessions, upstream credentials, and active
connections are in memory only and disappear when the proxy process restarts.
The switch is currently device-local and is not included in network backups.
If USB settings or the control socket cannot be initialized, the proxy logs
the failure and continues serving the existing Wi-Fi/system proxy policy with
USB sharing unavailable. Repair the reported path and restart the proxy to retry.

| Endpoint | Behavior |
| --- | --- |
| `GET /api/network/usb-internet` | Returns `enabled`, `active`, optional `host`, `expires_in_seconds`, and `via_usb`; never returns credentials or session tokens |
| `PUT /api/network/usb-internet` | Saves `{"enabled":true}` or `{"enabled":false}`; only USB/local and same-origin requests |
| `PUT /api/network/usb-internet/session` | Registers/renews `session_id`, `host` (`mac` or `android`), Android `password`, and optional `unix_time_ms` |
| `DELETE /api/network/usb-internet/session` | Releases the matching `session_id` from its owning peer |

Session IDs and Android passwords contain 32–64 lowercase hexadecimal
characters. The host renews every five seconds. The board expires a lease
30 seconds after its last successful renewal, using a monotonic clock; a
one-second reaper also closes idle/streaming sockets after expiry. Only one
host owns a lease at a time; a competing owner receives HTTP 409. Disabled
registration returns HTTP 403.

Mac session registration is local-only, normally through SSH executing curl
on the board. Its upstream is fixed to `127.0.0.1:18081`. Android registration
requires real USB ingress and uses the peer's IP with fixed port 18081. A
remote client cannot register a loopback or arbitrary Internet proxy.

A successful session can bootstrap a board clock that differs from its host
by more than two minutes. Host timestamps must fall between 2024 and 2100.
Clock synchronization happens only after enabled/ownership validation, over
the trusted USB/local path. This is necessary for boards that boot without a
valid RTC or NTP. TLS verification remains enabled; changing wall time does
not extend a lease.

USB sessions override saved SSID policies only while active. Local addresses
and the USB subnet bypass the relay. The generated client environment uses
the existing local HTTP proxy, so DNS names travel through CONNECT/SOCKS to the
host. Clients do not receive host credentials. Session renewal does not restart
the Agent. A change to the generated proxy environment may trigger the existing
Agent restart watcher once when entering/leaving a different proxy policy.

## Coverage and validation

This is **proxy-aware TCP connectivity**, not a full IP gateway. It does not
supply arbitrary UDP, QUIC, ICMP/ping, system DNS, or NTP. Applications that
ignore proxy variables or use their own exclusions can bypass it. For shell
tools, apply the managed environment explicitly when necessary:

```bash
ssh aico@192.168.42.1 \
  '/usr/lib/aico/aico-managed-env-run curl -fI https://example.com'
```

`make check` includes the USB Internet manager/proxy/API regression tests and
web switch tests. They cover default-off persistence, ingress restrictions,
credential redaction, session ownership/expiry, disabled and pending connection
revocation, HTTP forwarding, fallback restoration, failed saves, and stale UI
responses. The companion App includes JVM socket tests for authentication,
forwarding, half-close, peer rejection, and shutdown, run with:

```bash
cd android
./gradlew :app:testDebugUnitTest --tests link.aico.app.UsbSocksServerTest
```

Phone acceptance must additionally test screen off, background service survival,
Wi-Fi/cellular transitions, VPN policy, IPv6-only cellular/NAT64, cable replug
with a changed DHCP address, and a real Agent API/streaming request. JVM tests
and an APK build do not establish hardware compatibility.

## Full IP alternatives

A Mac administrator can instead configure forwarding and PF NAT for the ECM
subnet, with a stable host USB address, a board default route through that
address, and explicit DNS. That is a separate configuration and is not enabled
by this switch. Preserve existing PF/VPN rules and track rollback state.
macOS Internet Sharing manages its own DHCP/subnet, so it conflicts with the
current board DHCP server unless the topology is deliberately reconfigured.

The inspected board kernel has `CONFIG_TUN` and `CONFIG_USB_NET_DRIVERS`
disabled. A TUN-based full-IP Android design or reversing USB roles requires
additional kernel/firmware work. Neither is needed for the implemented proxy
path.

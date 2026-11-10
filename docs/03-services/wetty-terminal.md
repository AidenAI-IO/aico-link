---
sidebar_position: 3
---

# ttyd Browser Terminal

The Debian firmware image integrates ttyd as a browser terminal for board-side
maintenance. Over USB it is at `http://192.168.42.1:3000/webtty/`; from the
local network it is at `http://<board-ip>:8080/webtty/` while LAN access is on
(see [LAN Access](./device-management-api.md#lan-access)).

## Debian Integration

The production build downloads the pinned static ttyd 1.7.3 armhf binary,
verifies its SHA-256 digest, and installs it in the business package without adding a
Node.js runtime. Build it with:

```bash
./debian_build.sh
```

## Startup

ttyd is managed by systemd:

```bash
systemctl start aico-ttyd.service
systemctl status aico-ttyd.service --no-pager
systemctl restart aico-ttyd.service
```

The service reads `/etc/aico_boot.conf`. Set `ENABLE_TTYD=0` to disable
startup. Existing `ENABLE_WETTY`, `WETTY_*`, and `WETTY_COMMAND` overrides are
accepted as migration fallbacks.

Default runtime values:

| Parameter | Default |
| --- | --- |
| Listen interface | `lo` (fixed, not configurable) |
| Port | `3000` |
| Base path | `/webtty/` |
| Command | `/usr/lib/aico/aico-ttyd-login` |
| Log | `/var/log/ttyd/ttyd.log` |

## Access

ttyd listens on loopback only, so it is never reached from the network
directly. Two front ends forward to it:

- Config Web serves `http://192.168.42.1:3000/webtty/` on a socket bound to
  `usb0` (`--usb-terminal-port=3000`), so only USB clients can use it. The
  config web page's `Terminal` link opens this address over USB.
- The Agent proxies `/webtty/` on port 8080. LAN clients reach it only while
  LAN access is on and after signing in with the device password (see
  [LAN Sign-In](./device-management-api.md#lan-sign-in)); USB clients always
  can.

The Docker sandbox runs ttyd itself and proxies `/webtty/` on its published
Agent Web port.

The ttyd daemon runs as the unprivileged `aico` user. Its default login helper
prompts for a local account name, then uses `su --login` to authenticate that
account's password. The `aico` password is the device password set in the
Agent settings, once one has been set. SSH follows the same LAN access switch
as the web services. This avoids a Debian `login`/PTY interaction that leaves
ttyd waiting for input without displaying a prompt. A custom `TTYD_COMMAND`
still runs as `aico`.

Both `aico` and `root` login shells (SSH, ttyd, or `sudo -i`) load the current
network proxy from `/run/wifi_proxy/proxy-env`. HTTP, HTTPS and SOCKS-aware tools
use the loopback relay at `127.0.0.1:18080`; the relay selects the configured
upstream for the active Wi-Fi network. Both uppercase and lowercase proxy
variables are exported. The generated file is readable by both accounts and
contains no upstream credentials; `/userdata/system/wifi-proxies.json` and
`/run/aico/system.env` remain private.

`sudo` preserves only the proxy variables via `/etc/sudoers.d/20-aico-proxy`,
so `sudo apt update` uses the same route as the login shell. Authentication is
still required. After installing this fix, existing terminals can reload it with
`. /etc/profile.d/aico-env.sh`; newly opened login terminals load it automatically.
An SSH one-shot command does not load a login profile; use
`ssh luckfox 'bash -lc "COMMAND"'` when it needs the same environment.

## Mobile browser defaults

ttyd 1.7.3 does not include a mobile virtual-keyboard toolbar, but its
`--client-option` mechanism tunes the bundled xterm.js client. Aico Link firmware applies
these defaults for the small touch screen and for lower-end mobile browsers:

| Option | Default | Purpose |
| --- | --- | --- |
| `rendererType` | `canvas` | Avoid requiring a WebGL context on mobile browsers |
| `fontSize` | `24` | Keep text readable and avoid iOS input auto-zoom |
| `scrollback` | `500` | Bound the browser-side terminal buffer |
| `cursorStyle` | `bar` | Make the insertion point easier to follow while typing |
| `disableResizeOverlay` | `true` | Avoid transient overlays when mobile browser chrome resizes the viewport |
| `max-clients` | `2` | Bound concurrent shells on the memory-constrained board |

Set the corresponding `TTYD_*` variables in `/etc/aico_boot.conf` to adjust
these values. The legacy `WETTY_*` names remain accepted for migration. The
mobile toolbar and viewport metadata documented by newer ttyd releases are not
available in the pinned 1.7.3 client.

# Desktop Environment Bridge

The desktop bridge exposes the standard [Environment Bridge Protocol](../environment_bridge.md)
for the host macOS, Linux, or Windows desktop. It captures the primary screen
and translates normalized (0–1000) pointer coordinates and keyboard operations
to the active desktop session.

Start it from `benchmark/`:

```bash
uv run python -m desktop.scripts.start_bridge --bridge-port 8898
```

Install `pyautogui` in the benchmark environment before using input operations:

```bash
uv pip install pyautogui
```

The bridge can also be started through the benchmark service CLI, which writes
the endpoint and a stop command to stdout:

```bash
uv run python -m runner start-desktop-env --bridge-port 8898
```

On macOS, grant Screen Recording and Accessibility permissions to the terminal
or Python interpreter. Linux may require an X11/Wayland-compatible screenshot
utility (`gnome-screenshot`, `scrot`, or ImageMagick `import`) when pyautogui is
not installed. A desktop bridge is single-environment (`/api/concurrent` = 1)
and `/api/setup` claims a route but does not reset the host desktop.

## Bind address

The bridge binds `127.0.0.1` by default and `--bridge-host` accepts only
`localhost` or an IPv4 loopback address, because anyone who can reach the
port can drive the desktop. On macOS, Docker Desktop containers reach the host
loopback through `host.docker.internal`, so loopback is enough.

On Linux, containers reach the host only through the Docker bridge gateway
(typically `172.17.0.1`), which is not a loopback address. Pass
`--allow-private-bind` to also accept a private-use IPv4 address (RFC 1918
`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, shared `100.64.0.0/10`, or
link-local `169.254.0.0/16`):

```bash
uv run python -m desktop.scripts.start_bridge \
  --bridge-host 172.17.0.1 --allow-private-bind --bridge-port 8898
```

Public addresses and `0.0.0.0` are refused even with this flag. The Aico
Launcher passes it automatically when it binds the bridge to the Docker
gateway.

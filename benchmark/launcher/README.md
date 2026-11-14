# Aico Launcher

Aico Launcher is a local web UI that runs Aico Link on your computer with one
click. It builds or selects an `aico-link` Docker image, starts an environment
bridge for the device the Agent should control, and runs the image with
Config Web and the Agent Web UI published on `127.0.0.1`.

The launcher is a developer tool that lives in this repository. It is not
published as a package and is not part of the device firmware.

## Start the Launcher

Requirements: Docker Desktop or Docker Engine, Git, and
[`uv`](https://docs.astral.sh/uv/getting-started/installation/).

From the repository root:

```bash
make launcher
```

or, equivalently:

```bash
cd benchmark
uv run python -m launcher
```

The launcher prints a URL such as
`http://127.0.0.1:4180/?token=...` and opens it in your browser. Open that exact
URL; the token signs the browser in for this launcher process.

| Option | Default | Purpose |
| --- | --- | --- |
| `--port N` | `4180` | Port on `127.0.0.1` for the launcher UI |
| `--no-open` | off | Print the URL without opening a browser |
| `--state-dir DIR` | `<repo>/.cache/launcher` | Where instances, builds, and logs are kept |

With `make`, pass options through `LAUNCHER_ARGS`, for example
`make launcher LAUNCHER_ARGS="--port 4181 --no-open"`.

Press Ctrl+C to quit the launcher. Running instances keep running; the next
launcher start finds them again.

## Typical Workflow

1. **Build an image.** In the New instance dialog, choose **Build from this
   checkout**. The launcher runs `docker build -f docker/dev/Dockerfile` on the
   current checkout and tags the result `aico-link:local-<commit>`, with a
   `-dirty` suffix when the working tree has uncommitted changes or untracked
   files that `.gitignore` does not exclude (they are part of the build context). The build log is
   shown while it runs. You can also enter any image reference instead; an image
   with a registry or namespace (for example `registry.example/aico/aico-link:1.2`)
   is pulled on first start.
2. **Pick a bridge type** and fill in its fields. **Check** probes the host and
   reports what is missing.
3. **Save and start.** The instance moves through `starting` (with the current
   step) to `running`, and the UI shows links to Config Web, the Agent Web UI,
   the terminal (when enabled), and the MobileGym view (when present).
4. **Configure a model** in Config Web, then chat in the Agent Web UI.

Each instance keeps its Agent data (configuration, memory, logs) in its own
Docker volume, `aico-launcher-<id>-data`, so stopping and starting keeps your
settings. Deleting an instance with **Also delete the saved data volume** removes the volume.

Default ports are 8000 (Config Web) and 8080 (Agent Web UI). When they are in
use, or used by another instance, the launcher picks free ports; you can set
fixed ports under the instance's advanced options.

## Bridge Types

| Type | What it controls | Prerequisites |
| --- | --- | --- |
| No device | Nothing. Chat only. | None |
| MobileGym simulator | A simulated Android phone in a Docker container, with its own web view | Docker and a Git checkout of this repository (the launcher reads the pinned MobileGym commit; the submodule does not need to be initialized). The first start builds `aico-mobilegym-simulator:py311`, which can take 10 minutes or more. |
| Android (ADB) | An Android phone, emulator, or VM | [Android SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools) (`adb`) on the host and an authorized device. **Check** lists devices from `adb devices`. One device can serve only one running instance. |
| Custom bridge | Any service that implements the [Environment Bridge Protocol](../environment_bridge.md) | A running bridge. Enter the URL the Agent container should use, normally `http://host.docker.internal:<port>`; `127.0.0.1` and `localhost` are rewritten to `host.docker.internal` for the container. The launcher checks `/health` at the address containers reach this computer at and derives the device type from the reported platform. On Linux that address is the Docker bridge gateway (usually `172.17.0.1`), so a bridge on this computer must listen on `0.0.0.0` or that gateway; a bridge that listens only on `127.0.0.1` is reported by **Check** and refused at start. |

Host desktop, iPhone Mirroring, iOS device, and virtual iPhone bridges are
listed in the UI but are planned for a later launcher release.

The device type passed to the Agent (`AICO_DEVICE_TYPE`) comes from the bridge:
`android` maps to `Android`, `ios` to `iOS`, `mac` or `macos` to `macOS`,
`windows` to `windows`, and `linux` to `linux`.

Each instance claims its own bridge route with the task and episode id
`launcher-<id>`. Stopping an instance stops the Agent container first, which
releases that route, and then stops the bridge the launcher started (the ADB
bridge process or the MobileGym container).

## What Runs Inside the Image

The image runs Config Web on container port 80 and the Agent Web UI on 8080,
over plain HTTP. The launcher publishes them on `127.0.0.1` only.

- **Classic voice is off in Docker.** Classic (speech-to-text) conversations need
  on-device RKNN models that the image does not contain, so the image starts the
  Agent with Classic voice disabled (`AICO_CLASSIC_VOICE=0`). Classic mode is
  text-only.
- **Realtime voice works in the browser.** `localhost` and `127.0.0.1` are secure
  contexts, so the Agent Web UI can use the microphone without TLS.
- **The browser terminal is off by default.** Turn on **Enable the web
  terminal** in an instance's advanced options to start `ttyd` behind
  `http://127.0.0.1:<agent port>/webtty/`. It is a root shell inside the
  container; leave it off unless you need it.

See [Try Aico on PC](../../docs/01-getting-started/try-aico-on-pc.md) for the
image's runtime variables and build arguments.

## Where State Lives

Everything the launcher keeps is under the state directory
(`<repo>/.cache/launcher` by default, ignored by Git):

| Path | Contents |
| --- | --- |
| `state.json` | Settings, instances, and recent builds |
| `builds/<id>.log` | Image build output |
| `instances/<id>/launcher.log` | Lifecycle steps and errors for one instance |
| `instances/<id>/bridge.log` | ADB bridge output, or MobileGym image build output |

Agent output comes from `docker logs` and from `/userdata/agent/log/agent.log`
inside the container; the **Logs** dialog shows all three sources.

Docker objects carry the label `io.aico.launcher.instance=<id>` and are named
`aico-launcher-<id>-agent`, `aico-launcher-<id>-mobilegym`, and
`aico-launcher-<id>-data` (volume). To list them:

```bash
docker ps -a --filter label=io.aico.launcher.instance
```

## Security Model

- The launcher listens on `127.0.0.1` only. It starts containers and host
  processes, so it must never be exposed to a network.
- A random token is generated at every start. Opening the printed URL exchanges
  it for an `HttpOnly`, `SameSite=Strict` cookie named `aico_launcher_<port>`;
  every API request needs that cookie. The port in the name keeps launchers on
  different ports from replacing each other's sign-in. The token appears only in
  the startup line, never in other logs.
- Requests whose `Host` header is not `127.0.0.1:<port>` or `localhost:<port>`
  are refused, which blocks DNS rebinding.
- Requests that change state must send `application/json` and are refused when
  their `Origin` is not the launcher itself, which blocks cross-site requests.
- User input (names, tags, image references, serials, endpoints, ports) is
  validated, and every command is run with an argument list, never through a
  shell.
- Instances publish Config Web and the Agent Web UI on `127.0.0.1` only. The
  ADB bridge listens where `host.docker.internal` resolves: `127.0.0.1` on macOS
  and Windows, and the Docker bridge gateway on Linux. Bridges are
  unauthenticated, so other containers on the same host can reach them.

## Settings and Registry Mirrors

The **Settings** panel applies to image builds and new starts:

| Setting | Effect |
| --- | --- |
| Base image | Passed as `BASE_IMAGE` to `docker build`. Use a mirror of the pinned Debian image, for example `docker.1ms.run/library/debian:trixie-slim@sha256:<digest>`. Keep the same digest. |
| Go download base | Passed as `GO_DOWNLOAD_BASE`, for example `https://golang.google.cn/dl`. The archive checksum is still verified. |
| MobileGym Node image | Passed as `MOBILEGYM_NODE_IMAGE` when the simulator image is built, for example `docker.1ms.run/library/node:22-bookworm`. |
| Pass host proxy settings | Passes the launcher's `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` (and lowercase forms) to builds and to Agent containers. `NO_PROXY` always includes `localhost`, `127.0.0.1`, and `host.docker.internal`. A proxy on the host's loopback is rewritten to `host.docker.internal`. On Linux that name reaches the Docker bridge gateway, not `127.0.0.1`, so the proxy must listen on `0.0.0.0` or the gateway address. |

Empty fields keep the Dockerfile defaults. Images that Docker pulls by name
(`docker pull` for instance images) use Docker's own registry mirror settings,
such as `registry-mirrors` in Docker Desktop's Docker Engine configuration.

## Troubleshooting

- **The page says to open the URL printed by the launcher.** The browser has no
  valid cookie, usually because the launcher restarted and generated a new
  token. Open the URL from the latest launcher output.
- **Docker is shown as not available.** Start Docker Desktop or the Docker
  daemon; the status refreshes within a few seconds.
- **The image build fails while downloading.** Set a base image mirror, a Go
  download base, or enable host proxy settings, then build again. The full log
  is in `builds/<id>.log` under the state directory.
- **"build or pull the image first".** The selected image does not exist
  locally and has no registry or namespace to pull from. Build it, or enter a
  full reference such as `registry.example/aico/aico-link:<tag>`.
- **A port is already in use.** Another program uses a fixed port you set.
  Clear the port fields to let the launcher choose, or pick other ports.
- **The ADB device is unauthorized.** Unlock the phone and accept the USB
  debugging prompt, then run **Check** again.
- **The instance shows an error after the launcher restarted.** A container or
  bridge exited while the launcher was not running. Open **Logs**, then start
  the instance again; the launcher removes leftovers first.
- **The MobileGym start takes a long time.** The first start builds the
  simulator image. Follow the bridge log; later starts reuse the image until the
  pinned MobileGym commit changes.

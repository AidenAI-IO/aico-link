# Aico Launcher

Aico Launcher is a local web UI that runs Aico Link on your computer with one
click. It builds or selects an `aico-link` Docker image, starts an environment
bridge for the device the Agent should control, and runs the image with
Config Web and the Agent Web UI published on `127.0.0.1`.

Like the rest of the PC workflow, it simulates Aico Link in software so you can
try Aico before the hardware ships; it is not an official product. The launcher is a
developer tool that lives in this repository. It is not published as a package
and is not part of the device firmware.

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
launcher start finds them again. Host desktop, iPhone Mirroring, and iOS device
instances need the launcher to renew their bridge route; see
[Bridge Types](#bridge-types).

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

Launcher instances use the PC Config Web profile (`AICO_CONFIG_WEB_PROFILE=container`).
It provides model providers, conversation and search settings, language and time
zone, logs, and browser Realtime voice. Board Wi-Fi, GPIO, storage maintenance,
firmware, device audio, and Classic voice controls are excluded from both the
navigation and the API. Select and reconnect ADB devices in the launcher. The
overview links to the instance's published Agent Web port.

Existing instances keep their saved image reference. Rebuild that same tag,
then stop and start the instance to use this profile. If the rebuild produces a
new tag, stop the instance, update its image reference under **Edit**, save, and
start it again. Editing the existing instance preserves its data volume;
creating a new instance from the new tag uses a separate data volume.

A plain Compose sandbox retains its existing full configuration profile.

Default ports are 8000 (Config Web) and 8080 (Agent Web UI). When they are in
use, or used by another instance, the launcher picks free ports; you can set
fixed ports under the instance's advanced options.

## Bridge Types

| Type | What it controls | Prerequisites |
| --- | --- | --- |
| No device | Nothing. Chat only. | None |
| MobileGym simulator | A simulated Android phone in a Docker container, with its own web view | Docker and a Git checkout of this repository (the launcher reads the pinned MobileGym commit; the submodule does not need to be initialized). The first start builds `aico-mobilegym-simulator:py311`, which can take 10 minutes or more. |
| Android (ADB) | An Android phone, emulator, or VM | [Android SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools) (`adb`) and an authorized device. **Check** lists devices from `adb devices` and reports a missing or wrong **adb path**. |
| Host desktop | This computer's screen, mouse, and keyboard | macOS or Linux with a graphical session. See [Host desktop](#host-desktop). |
| iPhone Mirroring | A real iPhone through Apple's iPhone Mirroring app | macOS with iPhone Mirroring set up and Xcode Command Line Tools. See [iPhone Mirroring](#iphone-mirroring). |
| iOS device | A real iPhone over USB through WebDriverAgent | macOS, Xcode, and a signed WebDriverAgent installed on the phone. See [iOS device](#ios-device). |
| Virtual iPhone | A [vphone-cli](../../docs/10-benchmark/vphone-cli-setup-guide-en.md) virtual iPhone | macOS with a running vphone VM. See [Virtual iPhone](#virtual-iphone). |
| Custom bridge | Any service that implements the [Environment Bridge Protocol](../environment_bridge.md) | A running bridge. Enter the URL the Agent container should use, normally `http://host.docker.internal:<port>`; `127.0.0.1` and `localhost` are rewritten to `host.docker.internal` for the container. The launcher checks `/health` at the address containers reach this computer at and derives the device type from the reported platform. On Linux that address is the Docker bridge gateway (usually `172.17.0.1`), so a bridge on this computer must listen on `0.0.0.0` or that gateway; a bridge that listens only on `127.0.0.1` is reported by **Check** and refused at start. |

Launcher-started MobileGym environments enable an active simulated Aico App for
each Agent route by default. The Agent state therefore reports
`app_connected=true` and `app_fgs_enabled=false` by default. A task can
explicitly enable the Android foreground service; iOS PiP is not simulated.
ConAct and Skill-UTG are enabled for backend Device CLI runs in Launcher
MobileGym environments.
Standalone MobileGym benchmark environments still opt in to simulated App state
per task.

Bridge types that the host cannot run (for example the iPhone types on Linux)
are shown greyed out with the reason. Some types show setup notes under their
fields and extra buttons next to **Check**, such as **Install dependencies**.
One device can serve only one running instance: the launcher refuses to start
a second instance on the same ADB serial, iPhone UDID, or vphone VM. Host
desktop and iPhone Mirroring both drive this computer's real pointer and
keyboard, so only one instance of either type runs at a time.

The device type passed to the Agent (`AICO_DEVICE_TYPE`) comes from the bridge:
`android` maps to `Android`, `ios` to `iOS`, `mac` or `macos` to `macOS`,
`windows` to `windows`, and `linux` to `linux`.

Android (ADB) instances keep Phone Bridge on the Agent. The ADB bridge receives
only screen and input requests; companion App status, WebSocket connections,
and HTTP command queues are handled locally. Companion App features require a
phone-to-Agent network path; see [Phone Bridge](../../docs/05-agent/phone-bridge.md#desktop-agent-with-adb-reverse).

For Unicode text, choose **Unicode input method** in the instance's Android
settings. **ADBKeyBoard** is the default and uses Base64 broadcasts.
**Appium UnicodeIME** uses Appium Settings with modified UTF-7 input. Install
the selected method automatically when the Android bridge starts if it is not
already installed. The bridge verifies the selected APK's SHA-256 and does not
grant additional Android permissions.
Both paths restore the original keyboard after insertion. Appium cannot type
literal `%s`; replacement also requires Android's `input keycombination`.
Existing instances use ADBKeyBoard until you edit the selection. Stop an
instance before editing, then save and start to apply the change. See
[Unicode setup](../adbandroid/README.md#unicode-text-with-adbkeyboard).

Each instance claims its own bridge route with the task and episode id
`launcher-<id>`; bridges that claim a route at startup (iPhone Mirroring, iOS
device, virtual iPhone) get the same id. Stopping an instance stops the Agent
container first, which releases that route, and then stops the bridge the
launcher started (a bridge process on this computer or the MobileGym
container). A bridge process is signalled only while its command line still
runs the bridge module, so a recycled process id is never stopped. The iOS
device bridge gets 15 seconds to stop WebDriverAgent and close its device
tunnel before it is killed; the other bridge processes get 5 seconds.

The Host desktop, iPhone Mirroring, and iOS device bridges release a route
after 30 minutes without requests, and the Agent does not claim it again. The
launcher therefore claims the route of every running instance of these types
again (`POST /api/setup` with the instance's route id, which does not reset the
device) when it starts and every 5 minutes while it runs. If the launcher is
not running for more than 30 minutes, an idle instance of these types loses
its route until the launcher runs again.

For the iPhone Mirroring and iOS device types, the Agent container runs with
`AICO_QUICK_ACTIONS_PRESET` set to `iphone_mirroring` or `ios_device`. The image
then installs the matching iOS quick actions (Home, App Switcher, Spotlight) as
the Agent's `quick_actions.json`, unless you edited that file yourself.

### Bridge Dependencies

The desktop and iOS device bridges need Python packages that are not part of
the benchmark environment. The launcher keeps a separate virtual environment
for each under `<state dir>/pyenv/desktop` (Pillow and pyautogui) and
`<state dir>/pyenv/ios_device` (from `ios_device/requirements.txt`, including
`pymobiledevice3==11.52.1`, on Python 3.12). It creates them with `uv` the
first time you click **Install dependencies** or start such an instance, and
reuses an environment whose packages are already installed. Install output goes
to the action result or to the instance's bridge log. Delete the directory to
force a clean reinstall. The iPhone Mirroring and virtual iPhone bridges use the
launcher's own Python environment.

### Host Desktop

The Agent sees this computer's primary screen and controls the real mouse and
keyboard. Before saving, confirm **I understand the Agent will move the mouse
and type on this computer**; the instance does not start without it. Avoid
using the pointer while the instance runs, and stop the instance to take
control back.

- **macOS permissions.** Screen capture needs Screen Recording, and input needs
  Accessibility (labeled **Device Control and Data Access** in newer macOS).
  macOS grants both to the app that runs the launcher (Terminal, iTerm,
  VS Code, ...), not to the launcher itself. Grant them in System Settings >
  Privacy & Security, then quit and restart the launcher.
- **Test screen capture** captures one screenshot through the bridge code
  without sending input and reports its size. On macOS the first run may show
  the Screen Recording prompt. Without the permission, macOS may return an
  image that shows only the desktop background.
- **Linux.** Start the launcher from a graphical session (X11, or Wayland with
  XWayland) so `DISPLAY` or `WAYLAND_DISPLAY` is set. The bridge listens on the
  Docker bridge gateway, which the launcher allows with the bridge's
  `--allow-private-bind` option. If pyautogui cannot capture the screen, set
  **Screenshot command** to a command prefix that writes a PNG to the path
  appended to it, for example `gnome-screenshot -f` or `scrot -o`.
- Windows hosts are not supported yet.

### iPhone Mirroring

Requires macOS with Apple's iPhone Mirroring and the Xcode Command Line Tools
(`xcode-select --install`), which the bridge uses to compile a small Swift
helper into `<repo>/.cache/iphone-mirroring` on the first **Check** or start.
See the [bridge README](../iphone_mirroring/README.md) for details.

1. Open iPhone Mirroring and connect the phone. Keep the iPhone locked while
   it is mirrored.
2. Click **Request permissions**. macOS asks for Accessibility and Screen
   Recording for the app that runs the launcher. Approve both in System
   Settings > Privacy & Security, then restart the launcher.
3. Click **Check**. It reports whether the mirror window was found and whether
   screen capture and input are permitted.

Input focuses the mirror window and briefly takes over the Mac's pointer and
keyboard. **Content insets** (left, top, right, bottom, in macOS points) crop
the window frame from screenshots; keep the default unless screenshots include
window chrome. The bridge listens on `127.0.0.1`, which Docker Desktop reaches
through `host.docker.internal`.

### iOS Device

Controls an iPhone over USB through WebDriverAgent (WDA) and pymobiledevice3.
Requires macOS with full Xcode.

1. **One-time WDA setup (manual).** Sign and install WebDriverAgent with your
   Apple development team, as described in
   [Prerequisites and Installation](../ios_device/README.md#prerequisites-and-installation)
   (`python -m ios_device.setup ...`). The launcher does not sign or install WDA.
2. Connect the iPhone by USB, enable Developer Mode, unlock it, and accept
   **Trust This Computer**.
3. Click **Install dependencies** (or let the first start do it), then
   **Check**. Check lists the connected USB iPhones, then probes the selected
   one: WDA connection, lock state, and whether input is available.

**Device** may stay empty when exactly one iPhone is connected. **WebDriverAgent
runner** is the bundle ID of the installed runner (default
`io.aiden.aico.WebDriverAgentRunner.xctrunner`). **Permission policy file** is
an optional JSON file of rules for answering iOS permission prompts; see
[Opt-in Automatic Permission Rules](../ios_device/README.md#opt-in-automatic-permission-rules).
Only one WDA controller can drive a device at a time, so stop Xcode test runs
or other WDA sessions on that phone first.

### Virtual iPhone

Controls a [vphone-cli](../../docs/10-benchmark/vphone-cli-setup-guide-en.md)
virtual iPhone on this Mac. Start the VM first.

- **VM socket** is the VM's host-control socket, `vm/vphone.sock` in the
  vphone-cli checkout.
- **vphone.env file** is optional. Its values fill the fields you leave empty,
  exactly as `vphone/start.sh` uses it (for example `VPHONE_SOCKET`,
  `VPHONE_GUEST_SSH_*`). Without it, `benchmark/vphone/vphone.env` is used when
  present. The launcher passes the socket, env file, and guest SSH settings it
  resolved to the bridge explicitly, so `VPHONE_ENV_FILE` or other `VPHONE_*`
  variables in the launcher's own environment do not apply.
- **Guest SSH port** and **Guest SSH user** may stay empty: the env file's
  `VPHONE_GUEST_SSH_PORT` and `VPHONE_GUEST_SSH_USER` apply, otherwise `22222`
  and `root`.
- **Use guest SSH fallback** (on by default) lets the bridge use SSH into the
  guest for actions the socket cannot do. With **Guest SSH host** empty, the
  launcher detects the guest address from `/var/db/dhcpd_leases`: it takes the
  newest lease named `iPhone` whose SSH port accepts a connection, like
  `start.sh detect_guest_ip`, and falls back to `VPHONE_GUEST_SSH_HOST`. The
  address changes on every VM boot, so leave the field empty. **Guest SSH key**
  is the private key for the guest.

**Check** reports whether the socket exists and is a Unix socket, the detected
guest address, and whether its SSH port answers.

## What Runs Inside the Image

The image runs Config Web on container port 80 and the Agent Web UI on 8080,
over plain HTTP. The launcher publishes them on `127.0.0.1` only.

- **Classic settings remain available.** The PC Config Web shares the Realtime
  and Classic pages, including STT/TTS provider credentials, models, and Extra
  settings. The image starts the Agent with Classic voice disabled
  (`AICO_CLASSIC_VOICE=0`) because it lacks the VAD helper and matching models.
  Config Web reports that Classic is text-only; saving providers does not enable
  its runtime. Supply those dependencies before enabling Classic at startup.
- **Realtime voice works in the browser.** `localhost` and `127.0.0.1` are secure
  contexts, so the Agent Web UI can use the microphone without TLS.
- **The browser terminal is off by default.** Turn on **Enable the web
  terminal** in an instance's advanced options to start `ttyd` behind
  `http://127.0.0.1:<agent port>/webtty/`. It is a root shell inside the
  container; leave it off unless you need it.
- **Software memory and logs remain available.** Set screen/notification memory
  retention, reset conversation and memory, and export Agent support logs from
  the PC Config Web. The overview also links to an enabled container terminal
  using the published Agent port. Board capture buttons, speaker enrollment,
  device audio, Wi-Fi, GPIO, storage maintenance, and firmware controls are
  excluded.

See [Try Aico on PC](../../docs/01-getting-started/try-aico-on-pc.md) for the
image's runtime variables and build arguments.

## Where State Lives

Everything the launcher keeps is under the state directory
(`<repo>/.cache/launcher` by default, ignored by Git):

| Path | Contents |
| --- | --- |
| `state.json` | Settings, instances, and recent builds |
| `launcher.lock` | Held by the running launcher so only one uses this directory |
| `builds/<id>.log` | Image build output |
| `instances/<id>/launcher.log` | Lifecycle steps and errors for one instance |
| `instances/<id>/bridge.log` | Bridge process output, bridge dependency installs, or MobileGym image build output |
| `pyenv/<name>/` | Python environments for the desktop and iOS device bridges |

Only one launcher can use a state directory at a time; a second one exits
with an error. To run another launcher alongside it, give it its own
`--port` and `--state-dir`.

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
- User input (names, tags, image references, serials, UDIDs, paths,
  endpoints, ports) is validated, and every command is run with an argument list, never through a
  shell.
- Instances publish Config Web and the Agent Web UI on `127.0.0.1` only. The
  ADB and desktop bridges listen where `host.docker.internal` resolves:
  `127.0.0.1` on macOS and Windows, and the Docker bridge gateway on Linux. The
  iPhone bridges are macOS-only and listen on `127.0.0.1`. Bridges are
  unauthenticated, so other containers on the same host can reach them; a
  running desktop bridge lets them control this computer.
- User paths (permission policy, env file, socket, SSH key) must be absolute
  paths on this computer; `~` is expanded.

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
- **`device-cli: not found` in the Agent shell.** Use **Build from this checkout**
  and select the new image for the instance. A running container keeps its old
  image; recreate it with the rebuilt image and keep its saved data volume.
- **A port is already in use.** Another program uses a fixed port you set.
  Clear the port fields to let the launcher choose, or pick other ports.
- **The ADB device is unauthorized.** Unlock the phone and accept the USB
  debugging prompt, then run **Check** again.
- **"Install uv ..." when installing bridge dependencies.** The launcher
  creates bridge environments with `uv`. Install it, restart the launcher, and
  try again.
- **A bridge process exited while starting.** The error shows the bridge's last
  output line; **Logs > Bridge** has the full output, including dependency
  installs.
- **macOS screenshots show only the desktop background, or input does
  nothing.** Screen Recording or Accessibility is missing for the app that runs
  the launcher. Grant it in System Settings > Privacy & Security and restart
  the launcher (not only the instance).
- **iPhone Mirroring: no mirror window.** Open iPhone Mirroring, connect the
  phone, and keep the iPhone locked. Do not minimize the window.
- **iOS device: "Several iPhones are connected" or no device listed.** Select
  the device after **Check**. If none is listed, unlock the phone, accept
  **Trust This Computer**, and check the cable.
- **iOS device: WDA does not answer.** Make sure the signed runner is installed
  under the configured bundle ID and that no other WDA session drives the
  phone. Reinstall it with `python -m ios_device.setup` if signing expired.
- **Virtual iPhone: the socket does not exist or no guest address.** Start the
  vphone VM, then **Check** again. Enter **Guest SSH host** if your VM does not
  use the macOS DHCP server.
- **The instance shows an error after the launcher restarted.** A container or
  bridge exited while the launcher was not running. Open **Logs**, then start
  the instance again; the launcher removes leftovers first.
- **The MobileGym start takes a long time.** The first start builds the
  simulator image. Follow the bridge log; later starts reuse the image until the
  pinned MobileGym commit changes.

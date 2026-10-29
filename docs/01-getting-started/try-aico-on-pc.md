---
sidebar_position: 1
title: Try Aico on PC
description: Run and develop Aico locally without a development board.
---

# Try Aico on PC

The Docker sandbox is the fastest way to run Aico without a Luckfox
board. It starts the same configuration and Agent web experiences used during
device development, while keeping configuration and Agent state in a Docker
named volume.

Use this workflow for interactive development, configuration checks, and Agent
testing. For repeatable task suites, parallel environments, scoring, and reports,
use the [Benchmark workflow](../09-benchmark/README.md) instead.

## Prerequisites

- Docker Desktop, or Docker Engine with the Compose plugin;
- Git;
- a model provider and API key to configure after startup;
- an optional environment bridge if the Agent should observe and control a
  simulator, emulator, physical ADB device, or virtual machine;
- host-side [`uv`](https://docs.astral.sh/uv/getting-started/installation/) for
  MobileGym and ADB Android bridge workflows;
- [Android SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools),
  which provide `adb`, for the ADB Android workflow.

The sandbox does not build or use the vendored `pico-sdk/` BSP.

## Get the Source

The basic sandbox does not require a recursive clone:

```bash
git clone https://github.com/AidenAI-IO/aico-link.git aico-link-firmware
cd aico-link-firmware
```

## Start the Sandbox

From the repository root:

```bash
make sandbox-start
```

The command rebuilds the image from the current source, starts the sandbox in the
background, and waits for both web services to become healthy. Docker layer
caching keeps repeat runs fast when nothing changed, so the running sandbox
always matches the working tree. When the services are ready, open:

| Page | URL | Purpose |
| --- | --- | --- |
| Config Web | [http://localhost:8000](http://localhost:8000) | Configure model, STT, TTS, Agent, and other supported settings |
| Agent Web | [http://localhost:8080](http://localhost:8080) | Chat with and inspect the running Agent |
| Terminal | [http://localhost:8080/webtty/](http://localhost:8080/webtty/) | Open a ttyd shell inside the sandbox container |

Config Web writes changes into the sandbox's persistent named volume. Restarting
or rebuilding the containers therefore does not discard the saved configuration
or other persisted Agent state. Keep API keys out of tracked repository files;
enter them through Config Web instead. Saving a supported Agent setting requests
an Agent restart automatically, so the new configuration takes effect without
restarting the whole Compose stack.

To build and run the sandbox in the foreground instead:

```bash
docker compose up --build
```

Follow service output with:

```bash
docker compose logs -f
```

After changing Aico source code, run `make sandbox-start` again to rebuild the
image and replace the running container.

Starting the sandbox preserves the `aico-data` volume. Use `make sandbox-logs`
to follow the Agent logs and `make sandbox-stop` to stop the sandbox.

## Connect an Environment Bridge

Without an environment bridge, Config Web and Agent Web work, but the Agent has
no external screen to observe or control. A bridge exposes screenshots and input
tools through the repository's
[Environment Bridge Protocol](../../benchmark/environment_bridge.md).

Start the bridge on the host, then pass its host-reachable endpoint when starting
the sandbox:

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
  docker compose up --build
```

Use `host.docker.internal`, not `localhost` or `127.0.0.1`: from inside the Agent
container, the latter addresses refer to the container itself. The root Compose
stack maps `host.docker.internal` to the host gateway where required.

The endpoint is applied when the Agent container is created. If the sandbox is
already running, stop it and run the command again with the environment variable.

The sandbox uses `AICO_BENCHMARK_TASK_ID=docker-sandbox` by default and calls
the bridge's `/api/setup` endpoint during startup. It then sends the same task id
with forwarded tool requests so a multi-environment bridge keeps this interactive
session on one underlying environment. To choose a different stable route, set
both variables when starting Compose:

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
AICO_BENCHMARK_TASK_ID=my-sandbox-session \
AICO_BRIDGE_EPISODE_ID=my-sandbox-session \
  docker compose up --build
```

Keep the task id stable when a multi-instance bridge should identify requests as
one logical sandbox session. Use a different id for each simultaneously running
sandbox session.

Agent watchdog restarts and Config Web saves preserve the claimed bridge
session. Changing the bridge endpoint, task id, or episode id releases the old
route before claiming the new one. Stopping the sandbox releases the current
route; the next full start claims a fresh session.

The default sandbox device type is `iOS`, which also lets Agent Web become ready
without waiting for an unavailable Android frame service when no bridge is
configured. Set `AICO_DEVICE_TYPE` to match the bridge platform whenever the
target differs. Accepted values are `iOS`, `Android`, `macOS`, `windows`, and
`linux`. If no runtime override was passed, a device type change in Config Web
takes effect after `docker compose restart`. If `AICO_DEVICE_TYPE` was passed,
that environment value remains authoritative: stop the stack and run
`docker compose up` again with the updated value, or without the override.

### MobileGym

Start a one-environment MobileGym bridge from a separate terminal:

```bash
git submodule update --init benchmark/mobilegym/vendor/mobilegym
cd benchmark
uv sync
uv run python -m runner start-mobilegym-env --envs 1 --bridge-port 19090
```

Verify the bridge from the host:

```bash
curl http://127.0.0.1:19090/health
```

Then start the root Compose stack with:

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
  docker compose up --build
```

Use Agent Web at `http://localhost:8080` to give the Agent a task and watch it
operate the MobileGym UI. The sandbox claims one MobileGym route through
`/api/setup` using the configured `AICO_BENCHMARK_TASK_ID`; the bridge process
prints its own stop command when it starts.

### Android Through ADB

First confirm that the emulator, Android virtual machine, or physical device is
visible to ADB:

```bash
adb devices
```

Start the ADB Android bridge, replacing the serial with the value shown by
`adb devices`:

```bash
cd benchmark
uv sync
uv run python -m runner start-adb-android-env \
  --adb-serial emulator-5554 \
  --bridge-port 8899
```

Then start the sandbox from the repository root:

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:8899 \
  docker compose up --build
```

See [ADB Android Environment Bridge](../../benchmark/adbandroid/README.md) for
supported tools, serial selection, and device-specific limitations.

### Other Virtual Machines or Targets

The sandbox can connect to any host-side service that implements the
[Environment Bridge Protocol](../../benchmark/environment_bridge.md). Start the
bridge for the virtual machine or UI target, expose its HTTP port on the host,
and set:

```bash
AICO_DEVICE_TYPE=<device-type> \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:<bridge-port> \
  docker compose up --build
```

The environment bridge is the control boundary: the sandbox does not gain direct
access to an arbitrary virtual machine merely because the VM is running. For the
project's virtual iPhone workflow, use `AICO_DEVICE_TYPE=iOS` and see the
[vphone CLI setup guide](../09-benchmark/vphone-cli-setup-guide-en.md).

### Host Desktop

To let the Agent operate the desktop session running the benchmark, start the
host desktop bridge from `benchmark/`:

```bash
uv run python -m runner start-desktop-env --bridge-port 8898
```

Then point the sandbox at the reported endpoint:

```bash
AICO_DEVICE_TYPE=macOS \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:8898 \
  docker compose up --build
```

The bridge uses `pyautogui` for pointer and keyboard input and normalized
0–1000 coordinates for all pointer operations. Install it with
`uv pip install pyautogui`, and grant Screen Recording/Accessibility permission
on macOS (or the equivalent desktop-session permissions on Linux/Windows).

## Stop or Reset the Sandbox

For a foreground run, press `Ctrl-C`. Then remove the stopped containers and
network while preserving the named volume:

```bash
docker compose down
```

To delete the saved configuration and all other Agent state in the Compose named
volume, remove volumes as well:

```bash
docker compose down -v
```

The `-v` reset is destructive. The next `docker compose up --build` starts with
fresh sandbox data.

## Hardware Capability Boundaries

The sandbox runs software services; it does not emulate the Aico Link development
board or its peripherals. In particular, it does not simulate:

- board Wi-Fi scanning, access-point setup, USB networking, or Wi-Fi connection
  management;
- OTA, bootloader, partition, firmware flashing, or rollback behavior;
- HDMI-to-CSI screen capture, `/dev/video0`, or the frame capture hardware path;
- Linux USB gadget devices and real USB HID output through `/dev/hidg*`;
- onboard microphone, speaker, codec, hardware VAD, or the board audio service;
- board BLE and other board-specific peripheral services.

Hardware-related controls may still appear in shared configuration pages, but
they are not evidence that the corresponding hardware path works in Docker. An
environment bridge substitutes a screen-and-input target for Agent testing; it
does not emulate Wi-Fi, OTA, USB HID, audio, or other board behavior.

Use a physical board and the [Newcomer Quickstart](quickstart.md) when validating
those capabilities.

## Troubleshooting

### A web page does not open

Check service status and logs:

```bash
docker compose ps
docker compose logs -f
```

Also confirm that ports `8000` and `8080` are not already in use.

### The Agent cannot reach the environment bridge

1. Confirm the bridge's `/health` endpoint works from the host.
2. Confirm the sandbox was started with `AICO_ENVIRONMENT_BRIDGE_ENDPOINT`.
3. Confirm the endpoint uses `host.docker.internal`, not `localhost`.
4. Check both the bridge output and `docker compose logs -f` for the failed
   request.

### The Agent Web opens but model requests fail

Open Config Web and verify the provider, model name, API key, and optional base
URL. The sandbox does not provide model credentials automatically.

## Next Steps

- [Benchmark Overview](../09-benchmark/README.md) for repeatable suites, parallel
  environments, scoring, and reports;
- [Agent Configuration](../04-agent/configuration.md) for configuration fields;
- [Hardware & Wiring](hardware.md) when moving to a physical board.

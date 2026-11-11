---
sidebar_position: 1.5
title: Local Agent with a Real Phone
description: Run the Agent directly on your computer and experiment with an Android phone or a mirrored iPhone.
---

# Local Agent with a Real Phone

Start here to run Aico directly on your computer and experiment with a real
phone through Agent Web. The computer runs three processes: Config Web for
settings, a phone bridge for screenshots and input, and the Agent daemon.
This workflow does not require an Aico Link board or a Docker sandbox.

Choose the phone connection before starting:

| Phone | Computer | Connection | Detailed reference |
| --- | --- | --- | --- |
| Android | macOS or Linux | USB or paired wireless ADB | [ADB Android bridge](../../benchmark/adbandroid/README.md) |
| iPhone | A Mac that supports Apple's iPhone Mirroring | An already connected iPhone Mirroring window | [iPhone Mirroring bridge](../../benchmark/iphone_mirroring/README.md) |

For a container-based Agent, use [Try Aico on PC](try-aico-on-pc.md).
For automated suites, scoring, and reports, continue with
[Benchmark](../09-benchmark/README.md) after verifying the phone connection.

## Prepare the Computer

Install Git, Go 1.26 or a compatible newer toolchain, and Python 3.10 or newer.
Have a model provider and API key ready to enter in Config Web.
Android also needs [Android SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools)
and [`uv`](https://docs.astral.sh/uv/getting-started/installation/).
iPhone Mirroring needs Xcode Command Line Tools (`xcrun swiftc`).

Clone the source, or use an existing checkout:

```bash
git clone https://github.com/AidenAI-IO/aico-link.git aico-link-firmware
cd aico-link-firmware
```

The phone bridges do not require the MobileGym submodule or the board BSP build.

Open three terminals at the repository root. Run this shared setup in each
terminal, using the same target everywhere. Set `AICO_PHONE=iphone` for iPhone
Mirroring, or leave it as `android`:

```bash
export AICO_REPO="$PWD"
export AICO_LAB="$AICO_REPO/.cache/native-phone-lab"
export AICO_PHONE=android
export AICO_AGENT_DATA="$AICO_LAB/$AICO_PHONE"
mkdir -p "$AICO_AGENT_DATA"
```

Use a separate data directory for each target. It contains the model
configuration, conversations, memory, logs, and target-specific quick actions.
The example stores these files in the ignored `.cache/` directory.

Build the native Agent once:

```bash
(cd "$AICO_REPO/src/agent" && CGO_ENABLED=0 go build -o "$AICO_LAB/agent" ./cmd/daemon)
```

## Terminal 1: Configure the Agent

Start Config Web before the daemon so a fresh data directory can be configured:

```bash
"$AICO_LAB/agent" config-web --local-agent \
  --config "$AICO_AGENT_DATA/agent.toml" \
  --web-root "$AICO_REPO/src/config_web/web" \
  --agent-url http://127.0.0.1:8080
```

Open [Config Web](http://127.0.0.1:8000). Add a model provider and API key,
select the main model, test the credentials, and save. The first save creates
`agent.toml`; the Agent can remain offline during configuration. Keep this
terminal running.

The local profile exposes model, conversation, language, time zone, and log
settings. It applies supported changes to a running Agent. The target device,
bridge endpoint, and task ID are selected by the daemon's launch arguments in
Terminal 3. Config Web does not start or restart the daemon.

## Terminal 2: Connect the Phone

Follow one of the two paths below and keep its bridge running.

### Android Through ADB

Enable USB debugging and authorize the computer on the phone, or pair and
connect the phone using Android's wireless debugging. Confirm the connection:

```bash
adb devices -l
```

The selected serial must have state `device`. An `unauthorized` device needs
approval on the phone; an `offline` device needs its ADB connection restored.
Use the exact serial shown by ADB, including the full service name for a
wireless connection.

Create a bridge environment with its Pillow dependency, then start the bridge.
Replace `YOUR_ADB_SERIAL` with the connected device's serial:

```bash
uv venv --python python3 "$AICO_LAB/adb-venv"
uv pip install --python "$AICO_LAB/adb-venv/bin/python" 'pillow>=10.2.0'
cd "$AICO_REPO/benchmark"
"$AICO_LAB/adb-venv/bin/python" -m adbandroid.scripts.start_bridge \
  --adb-serial YOUR_ADB_SERIAL \
  --bridge-host 127.0.0.1 \
  --bridge-port 8899
```

The bridge supports Android navigation, screenshots, taps, and scrolling.
Its ADB text backend supports ASCII; direct Chinese input requires an additional
IME/clipboard integration. See the [bridge limitations](../../benchmark/adbandroid/README.md#known-limitations)
before planning text-entry experiments.

### iPhone Through macOS iPhone Mirroring

Connect the phone in Apple's iPhone Mirroring app and keep the phone locked.
Install the mirror's default quick actions only on first use, preserving any
customizations in this target's data directory:

```bash
if [ ! -e "$AICO_AGENT_DATA/quick_actions.json" ]; then
  cp "$AICO_REPO/benchmark/iphone_mirroring/quick_actions.json" \
    "$AICO_AGENT_DATA/quick_actions.json"
fi
cd "$AICO_REPO/benchmark"
python3 -m iphone_mirroring --probe
```

If the probe reports missing permissions, explicitly request them and approve
the application identified by macOS:

```bash
python3 -m iphone_mirroring --request-input-access
python3 -m iphone_mirroring --request-screen-access
```

Probe again after granting Accessibility and Screen Recording. Keep the helper
executable at its generated path so permission attribution remains stable.
Then start the bridge:

```bash
python3 -m iphone_mirroring --port 8897 --task-id iphone-mirroring
```

This bridge supports committed Unicode input, including Chinese, through the
Mac's text input path. The [bridge reference](../../benchmark/iphone_mirroring/README.md)
describes permissions, input behavior, and unsupported gestures.

## Terminal 3: Start the Agent

Use the shared setup block from above in this terminal. Set host-writable
shared-file paths and keep the file listener on loopback:

```bash
export AICO_SHARE_DIR="$AICO_AGENT_DATA/shared"
export AICO_SHARE_ADDR=127.0.0.1:18083
export AICO_SYSTEM_ENV="$AICO_AGENT_DATA/system.env"
```

For **Android**, check the bridge and create its session before starting the
daemon. Setup returns the phone to Home and dismisses its keyboard; it does not
erase app data. Use the same task ID in setup and daemon requests:

```bash
curl --fail http://127.0.0.1:8899/health
curl --fail http://127.0.0.1:8899/api/setup \
  -H 'Content-Type: application/json' \
  -H 'benchmark-task-id: local-android' -d '{}'
"$AICO_LAB/agent" \
  -dir "$AICO_AGENT_DATA" -addr 127.0.0.1:8080 \
  --device-type Android \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://127.0.0.1:8899 \
  --benchmark-task-id local-android
```

For **iPhone**, the bridge claims its session at startup. Match its task ID:

```bash
curl --fail http://127.0.0.1:8897/health
"$AICO_LAB/agent" \
  -dir "$AICO_AGENT_DATA" -addr 127.0.0.1:8080 \
  --device-type iOS \
  --environment-bridge-mode \
  --environment-bridge-endpoint http://127.0.0.1:8897 \
  --benchmark-task-id iphone-mirroring
```

The native Agent uses `127.0.0.1` to reach the host bridge. The
`host.docker.internal` examples in the sandbox guide apply to a containerized
Agent.

## Verify and Run an Experiment

Open [Agent Web](http://127.0.0.1:8080). Check that Config Web's overview reports
the expected target (`Android` or `iOS`) and that this health request agrees:

```bash
curl --fail http://127.0.0.1:8080/health
```

For the first task, ask: “Describe the current phone screen without tapping or
typing.” Confirm that the Agent observes the intended phone before giving it
an interactive task. A working health endpoint alone does not verify capture.
To check the screenshot provider directly, save its JSON response locally:

```bash
curl --fail http://127.0.0.1:8080/api/providers/screenshot \
  -H 'Content-Type: application/json' -d '{}' \
  -o "$AICO_AGENT_DATA/frame.json"
```

A successful response has `ok: true`, a non-stale `data.meta`, and a base64 JPEG
in `data.image`. The capture backend should be `adb` for Android or
`iphone-mirroring` for iPhone. An iPhone connection dialog is also capturable:
inspect the phone screen, keep the phone locked, and finish connecting in
Apple's app before an interactive task.

## Stop, Restore, or Switch Targets

Press `Ctrl-C` in Terminal 3 to stop the Agent, then in Terminal 2 to stop the
bridge. Stop Config Web in Terminal 1 when finished. The data directories remain
available for the next run.

For an idle iPhone session, use **Restore iPhone Mirroring connection** on the
Config Web overview. It reclaims the running Agent's bridge session and checks
capture. Connect the phone in Apple's app first; the button cannot unlock the
phone or dismiss Apple's connection prompts.

To switch between Android and iPhone, stop the three processes, select the other
`AICO_PHONE` in the shared setup block, and repeat Terminals 1–3. Each daemon
must use the intended data directory, device type, endpoint, and task ID.
Keep the iPhone quick-action mapping in the iPhone directory.

For an ADB disconnection, restore the device to state `device`; restart the
bridge with the current serial if wireless debugging assigned a new one.
For bridge errors, start with the relevant bridge's `/health` endpoint and
reference README. Phone Bridge contacts/calendar APIs and board audio, GPIO,
USB HID, and firmware services are separate capabilities; a screenshot/input
connection alone does not provide them.

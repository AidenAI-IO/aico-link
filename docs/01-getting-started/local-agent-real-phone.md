---
sidebar_position: 1.5
title: Local Agent with a Real Phone
description: Start the native Agent, settings page, and Android or iPhone connection with one command.
---

# Local Agent with a Real Phone

Run Aico directly on your computer with a real phone, without an Aico Link
board or a Docker sandbox. One launcher starts Config Web, the phone bridge,
and Agent Web, and stops its own processes together on `Ctrl-C`.

## Prepare Once

Install Git, Go 1.26 or newer, Python 3.10 or newer, and
[`uv`](https://docs.astral.sh/uv/getting-started/installation/) for isolated
phone-bridge dependencies. Have a model provider and API key ready.

```bash
git clone https://github.com/AidenAI-IO/aico-link.git
cd aico-link
```

The phone workflow does not require the MobileGym submodule or a board build.
Prepare the connection for the selected mode:

| Mode | Computer and phone prerequisites | Reference |
| --- | --- | --- |
| `android` | macOS/Linux, Android SDK Platform Tools (`adb`), USB debugging authorized or wireless ADB paired; `adb devices` shows `device` | [ADB Android](../../benchmark/adbandroid/README.md) |
| `iphone-mirroring` | Compatible Mac, Xcode Command Line Tools (`xcrun swiftc`), phone connected in Apple's iPhone Mirroring app and kept locked | [iPhone Mirroring](../../benchmark/iphone_mirroring/README.md) |
| `ios` | macOS, compatible full Xcode, USB pairing, Developer Mode, an installed signed WDA runner, phone kept unlocked | [pymobiledevice3 / WDA setup](../../benchmark/ios_device/README.md#prerequisites-and-installation) |

For iPhone Mirroring, explicitly request macOS Accessibility and Screen
Recording access once and approve the application identified by macOS:

```bash
python3 scripts/local_agent.py iphone-mirroring --request-permissions
```

Repeat normal startup after approving access. The generated helper path stays
stable so macOS can retain its permission grants. For direct USB iOS, initial
Trust This Computer and developer/signing authorization still require you;
the launcher does not bypass them.

## Start with One Command

Run the command for your connection from the repository root:

```bash
python3 scripts/local_agent.py android
```

```bash
python3 scripts/local_agent.py iphone-mirroring
```

```bash
python3 scripts/local_agent.py ios
```

The launcher builds the native Agent using Go's build cache, prepares an
isolated Python environment when needed, installs this target's quick actions
only if absent, and opens Config Web. On first use, add the model provider/API
key and save; startup automatically continues when `agent.toml` is created.
Subsequent starts reuse that file. Config Web can still apply supported settings
to the running Agent.

Before reporting ready, the launcher checks the bridge type, claims the task
session, starts the Agent with matching device/endpoint/task arguments, and
verifies a fresh screenshot through the Agent. Android setup returns to Home
and dismisses its keyboard; it does not erase app data. An iPhone Mirroring
connection screen can itself be captured, so also inspect Apple's connection
and the Agent's first observation before interacting.

| Mode | Agent Web | Config Web | Bridge | Data directory |
| --- | --- | --- | --- | --- |
| `android` | `http://127.0.0.1:8080` | `http://127.0.0.1:8000` | `8899` | `.cache/native-phone-lab/android` |
| `iphone-mirroring` | `http://127.0.0.1:8081` | `http://127.0.0.1:8001` | `8897` | `.cache/native-phone-lab/iphone-mirroring` |
| `ios` | `http://127.0.0.1:8082` | `http://127.0.0.1:8002` | `8896` | `.cache/native-phone-lab/ios` |

Each mode keeps its configuration, conversations, quick actions, and logs in a
separate directory. The launcher preserves existing configuration and customized
quick actions. It refuses a data directory already marked for another mode and
does not stop a process occupying one of its ports. Run only one iPhone control
mode at a time: Mirroring needs the phone locked, while USB/WDA needs it unlocked.

## Existing Configurations and Device Selection

Import a configuration on first use without replacing an existing target file:

```bash
python3 scripts/local_agent.py ios --config /absolute/path/to/agent.toml
```

Reuse an existing Agent data directory (including its conversations):

```bash
python3 scripts/local_agent.py ios --data-dir /absolute/path/to/ios-agent-data
```

Android automatically selects the only authorized device. When several phones
are connected, select one explicitly:

```bash
python3 scripts/local_agent.py android --adb-serial YOUR_ADB_SERIAL
python3 scripts/local_agent.py ios --udid YOUR_DEVICE_ID
```

The default installed iOS runner is
`io.aiden.aico.WebDriverAgentRunner.xctrunner`. Select a different signed runner
or opt into app-specific permission rules as needed:

```bash
python3 scripts/local_agent.py ios --xctrunner YOUR_RUNNER_ID.xctrunner --permission-policy /absolute/path/to/permissions.json
```

Unknown or ambiguous permission prompts remain visible. See the
[direct iOS reference](../../benchmark/ios_device/README.md) for rule matching,
512-UTF-16-unit input limits, and secure-surface restrictions. Android's ADB
text backend supports ASCII; direct Chinese input needs an additional IME or
clipboard integration. Mirroring supports committed Unicode through the Mac.

For an existing bridge, pass its loopback origin. The launcher verifies that
its type matches the mode and claims the same task route; it leaves that bridge
process running on exit:

```bash
python3 scripts/local_agent.py ios --bridge-url http://127.0.0.1:8896
```

Use `--agent-binary` to reuse a native binary, `--python` for a prepared bridge
environment, and `--no-open` to print URLs without opening a browser. Override
`--agent-port`, `--config-port`, `--bridge-port`, or `--share-port` when necessary.
Run `python3 scripts/local_agent.py --help` for all options.

## Stop, Recover, or Switch Modes

Press `Ctrl-C` in the launch terminal. The Agent, Config Web, and bridge started
by that invocation stop together; configuration and conversations remain.
Switch modes by running the other one-line command after stopping the current
one. A child-process failure also stops the other owned processes and reports
its log path. Logs are `agent.log`, `config-web.log`, and `bridge.log` under the
target's data directory; credential values are never printed by the launcher.

For an idle Mirroring session, first restore the connection in Apple's app,
then use **Restore iPhone Mirroring connection** on the Config Web overview.
For USB/WDA disconnection, unlock/reconnect the phone and rerun the launcher.
For ADB, restore the device to state `device`, selecting its new serial if
wireless debugging changed it. Inspect the screen after any input timeout;
startup and recovery never replay a potentially delivered text/touch request.

Phone Bridge contacts/calendar APIs and board audio, GPIO, USB HID, and firmware
services are separate capabilities. A screenshot/input connection alone does
not provide them. For a container-based Agent, see [Try Aico on PC](try-aico-on-pc.md).
After verifying your phone, continue with [Benchmark](../09-benchmark/README.md)
for suites, scoring, and reports. The bridge-specific references retain manual
startup commands for debugging individual components.

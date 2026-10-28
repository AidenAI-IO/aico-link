# Aico Link Firmware

Website: [aico.link](https://aico.link) | Docs: [Documentation Hub](docs/README.md) | Community: [Join the Discord](https://discord.gg/tFrQsgFGTy)

**Aico Link** is the hardware. **Aico** is the AI Agent that runs on it.
This repository provides Aico Link firmware, hardware services, and the Aico
runtime. The board observes a target device through HDMI capture and controls
it through USB HID, so Aico can operate normal mobile or desktop apps without
relying on app-specific automation APIs.

This repository is for the development-board implementation: firmware overlay,
C++ hardware services, the Go Agent, OTA tooling, tests, and benchmark support.
It is not a final integrated hardware product.

![Aico Link Development Board](./aico-dev-board.png)

## Hardware & Self-Assembly

The current development setup centers on:

- [Luckfox Pico Zero](https://wiki.luckfox.com/Luckfox-Pico-Zero), RV1106 / Rockchip platform running Debian 13;
- [Firefly HDMI TO MIPI CSI RK628D](https://wiki.t-firefly.com/HDMI-TO-MIPI-CSI-RK628D/rk628d.html), four-lane HDMI-to-CSI bridge for external screen capture, or the legacy TC358743 two-lane bridge;
- [ASRPRO 2.0](https://item.taobao.com/item.htm?id=676711841241) for voice recognition, connected to the Pico Zero alongside a 1 W / 8 Ω speaker connected directly to the Pico Zero speaker output;
- A USB-C hub that provides HDMI output for capture and a USB data path back to the target device;
- The Pico Zero Linux USB gadget stack for keyboard, pointer/touch, and ECM networking.

Some prototype hardware revisions may include additional USB modules, but the firmware-documented HID path is the Linux gadget path: `/dev/hidg0` for the keyboard, `/dev/hidg1` for pointer/touch input, and `/dev/hidg2` for Android extension keys or Consumer Control media keys.

For the complete parts list, self-assembly instructions, wiring diagram, and target-device prerequisites, see [Hardware & Wiring](docs/01-getting-started/hardware.md).

## What Aico Does

The current hardware setup connects to a phone or computer through a USB-C hub:

- the target device's display output is captured through HDMI and the detected RK628D or TC358743 HDMI-to-CSI path;
- the Luckfox Pico Zero exposes a composite USB gadget with keyboard, pointer/touch, auxiliary control HID, and USB ECM networking;
- the Go Agent sends screenshots to a configured multimodal model, decides the next action, and writes keyboard, pointer/touch, and Android extension or Consumer Control reports to `/dev/hidg0`, `/dev/hidg1`, and `/dev/hidg2`;
- voice mode records audio on the board, applies VAD, then uses configured STT, LLM, and TTS providers.

The basic control path does not require jailbreak, ADB, developer mode, or a custom app on the target device. It does require a target that can output video to the capture path and accept USB HID input. iOS control also requires AssistiveTouch to be enabled.

## Key Features

### 1. **True Portability**
Most mobile agent projects are lab prototypes that require a laptop or desktop to run. Aico Link is:
- **Pocket-sized** — Powered entirely by your phone's USB-C port (future versions will be credit-card-sized and magnetically attach to phone backs)
- **Plug-and-play core control** — HDMI observation and USB HID control work
  without pairing or a companion app. Optional Phone Bridge, BLE Wake, and
  notification features require the companion app and their documented setup.
- **Production-ready** — Designed for daily use, not just research prototypes

### 2. **Fully Open**
- **Open-source firmware** — Complete C++ services and Go agent code
- **Development board reference** — Current prototyping board schematics and assembly guide available
- **Flexible model backends** — Configure registered providers such as OpenAI, OpenRouter, Kimi, Volcengine, DeepSeek, or Ollama; use OpenRouter for Anthropic Claude and Google Gemini models
- **Exportable data** — Your memory, skills, and learned behaviors are yours to keep and migrate
- **Community-driven** — Contributions welcome; flash custom firmware freely

### 3. **Privacy-First**
- **No Aico backend** — All services (LLM, STT, TTS) are user-configured external endpoints
- **Zero data collection** — We never see your screen, conversations, or behavior
- **Self-hostable** — Run your own models; keep everything on your infrastructure
- **Auditable** — Open-source code under community scrutiny

### 4. **Adaptive Intelligence**
- **Persistent memory** — Aico remembers device-specific context, user preferences, and past interactions
- **Self-improving skills** — Skills update based on usage patterns and new workflows
- **Personalized over time** — The more you use Aico, the better it understands your habits and needs

### 5. **Real-Time Voice Interaction**
- **Hardware VAD** (Voice Activity Detection) — Wake-word-free voice control with sub-100ms latency
- **Streaming STT/TTS** — Natural conversation flow with live transcription and speech synthesis
- **Audio feedback** — Aico speaks responses and status updates

## Repository Scope

- **Firmware integration**: Debian rootfs overlay and business package, systemd services, USB gadget setup, Wi-Fi/config portal defaults, and full `update.img` generation.
- **C++ services**: `frame_service` owns HDMI capture and exposes screenshots over Unix domain sockets; `audio_service` owns recording/playback and volume state.
- **Go Agent**: the device-side LLM runtime, voice loop, skills, memory, and built-in screenshot/HID/audio/shell tools.
- **USB networking**: The board exposes `usb0` at `192.168.42.1` for the device config page and local board-to-phone communication.
- **OTA**: A/B partition updates, signed manifests, health confirmation, and `abctl` diagnostics.

The firmware has no required Aico-hosted backend. Screenshots, audio, and text are sent only to the model/STT/TTS/search endpoints you configure.

## Architecture

```text
Target display
    -> HDMI / RK628D or TC358743 / CSI
    -> /dev/video0
    -> frame_service
    -> screenshot tool
    -> Go Agent
    -> HID tools
    -> /dev/hidg0, /dev/hidg1, and /dev/hidg2
    -> target device input

Board audio
    -> audio_service
    -> VAD / STT or audio attachment
    -> Go Agent / LLM
    -> TTS
    -> audio_service playback
```

At runtime, the Agent is intentionally decoupled from C++ service internals: Frame and Audio capabilities go through Unix domain sockets, while device control goes through the Linux USB gadget HID device nodes.

## Quick Start

For the fastest hardware-free experience, start Config Web and Agent Web with
the [Try Aico on PC](docs/01-getting-started/try-aico-on-pc.md) guide:

```bash
docker compose up --build
```

Open `http://localhost:8000` for Config Web and `http://localhost:8080` for
Agent Web. The sandbox can also connect to MobileGym, an ADB target, or another
compatible environment bridge. For benchmark suites and reports, see
[Benchmark](docs/09-benchmark/README.md).

First-time hardware users should start with [Newcomer Quickstart](docs/01-getting-started/quickstart.md). It walks through wiring, flashing, Wi-Fi setup, Agent configuration, voice verification, and troubleshooting.

Clone the repository (the BSP is included in `pico-sdk/`; `--recursive` also
initializes the MobileGym dependency):

```bash
git clone --recursive git@github.com:AidenAI-IO/aico-link.git aico-link-firmware
cd aico-link-firmware
```

Build and audit the Debian ARM application bundle:

```bash
scripts/debian-apps/build-apps.sh all
```

Build the full firmware image:

```bash
./debian_build.sh
```

Flash a prebuilt or locally built `update.img` on Linux. The guarded helper
checks the digest and requires an explicit confirmation because a factory
flash overwrites userdata:

```bash
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
SHA256=$(awk '{print $1}' "${IMAGE}.sha256")
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash \
  --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" \
  --sha256 "${SHA256}" \
  --confirm-erase-all-data
```

The repository-root `upgrade_tool/upgrade_tool` is a macOS Mach-O binary and
is not executable on Linux. For a locally built image, the Linux tool path and
image path above are the usual defaults. On macOS, use the repository-root
tool directly:

```bash
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img
```

## Documentation

The full documentation is organized under [docs/](docs/README.md):

- [Documentation Hub](docs/README.md)
- [Product Names and Technical Identifiers](docs/02-architecture/naming.md)
- [Try Aico on PC](docs/01-getting-started/try-aico-on-pc.md)
- [Newcomer Quickstart](docs/01-getting-started/quickstart.md)
- [Hardware & Wiring](docs/01-getting-started/hardware.md)
- [Firmware Build & Flashing](docs/01-getting-started/firmware.md)
- [Build & Development Environment](docs/01-getting-started/build.md)
- [Deployment to Device](docs/01-getting-started/deployment.md)
- [Testing & Verification](docs/01-getting-started/testing.md)
- [Architecture Overview](docs/02-architecture/overview.md)
- [Frame Service](docs/03-services/frame-service.md)
- [Audio Service](docs/03-services/audio-service.md)
- [USB HID & Device Control](docs/03-services/usb-hid.md)
- [Go Agent](docs/04-agent/overview.md)
- [Agent Configuration Reference](docs/04-agent/configuration.md)
- [Tools HTTP API](docs/04-agent/tools-http-api.md)
- [OTA](docs/08-ota/README.md)
- [Benchmark](docs/09-benchmark/README.md)
- [SkillOpt](docs/10-skillopt/README.md)
- [Troubleshooting](docs/07-operations/troubleshooting.md)

## Contributing

Contributions are welcome for firmware, services, Agent runtime, skills, tests, benchmarks, and documentation. Before contributing, read [LICENSE](LICENSE); it defines the dual-license terms, contribution license grant, patent terms, and hardware-design licensing notes for this repository.

Do not commit secrets, device-specific credentials, or local environment files.

## License

This repository is distributed under the dual-license terms described in [LICENSE](LICENSE), with the AGPL-3.0 text included in [LICENSE-AGPL-3.0](LICENSE-AGPL-3.0). Third-party components keep their own licenses; see [NOTICE](NOTICE) for details.

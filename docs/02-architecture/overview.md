---
sidebar_position: 1
---

# System Architecture Overview

Aico Link is the hardware platform. It combines HDMI video capture, audio
recording/playback, and USB HID control with Aico, the AI Agent running on the
board. The official website is [aico.link](https://aico.link).

## Overall Architecture

```text
                 ┌──────────────────────────┐
                 │        Go Agent          │
                 │ Web UI / HTTP Tool API   │
                 │ LLM / Skills / Memory    │
                 └───────┬─────────┬────────┘
                         │         │
          screenshot UDS │         │ audio UDS / volume
                         │         │
             ┌───────────▼───┐ ┌───▼────────────┐
             │ frame_service │ │ audio_service   │
             │ C++ daemon    │ │ C++ daemon      │
             └───────┬───────┘ └───────┬────────┘
                     │                 │
           /dev/video0 + subdev        │ ALSA / RK MPI
                     │                 │
             ┌───────▼──────┐          │
             │ RK628D /     │          │
             │ TC358743 HDMI│          │
             │ capture path │          │
             └──────────────┘          │

                 ┌──────────────────────────┐
                 │ USB HID Gadget           │
                 │ hidg0 / hidg1 / hidg2     │
                 └───────────▲──────────────┘
                             │
                         Agent tools
```

## Module Layers

| Layer | Module | Description |
| --- | --- | --- |
| Hardware Abstraction | `src/aico_sdk.*` | Encapsulates GPIO, audio, video, HID-related low-level capabilities |
| Common Transport | `src/uds_*` | Unix domain socket one-shot request/response transport layer |
| Hardware Services | `frame_service`, `audio_service`, `ble_service` | Centrally manage hardware resources and expose them to other processes |
| Utility Programs | `*_cli`, `example_*`, `image_process` | Debugging, validation, and single-capability examples |
| Go Agent | `src/agent` | LLM runtime, tool invocation, Web UI, HTTP Tool API, voice pipeline |
| Firmware Integration | `overlay-debian/`, `assets/business/`, `pico-sdk/` | Debian systemd integration, rootfs assets and business package, and RV1106 BSP |

## Key Design Principles

1. **Single Hardware Resource Owner**: For example, `/dev/video0` is exclusively owned by `frame_service`, and other consumers read frames via UDS, avoiding conflicts from multiple processes opening the video device simultaneously.
2. **Cross-Language Protocol**: UDS envelope uses JSON header + binary payload, allowing Go / C++ / other languages to implement clients.
3. **Agent and Hardware Decoupling**: Agent does not directly depend on C++ ABI, completing observation/operation through sockets and HID devices.
4. **Systemd Supervision**: Debian units declare ordering, restart policy, and failure handling for each device service.

## Main Data Flows

### Screenshot / Visual Observation

```text
Go screenshot request → frame_service STREAMON + one fresh /dev/video0 frame → STREAMOFF → LLM image input
```

### Device Control

```text
LLM tool call → Go HID tool → /dev/hidg0, /dev/hidg1, or /dev/hidg2 → target device
```

### Voice Interaction

```text
audio_service recording → RKNN Silero VAD → STT or audio attachment → LLM → TTS → audio_service playback
```

### Bluetooth Notifications and Wake

```text
iOS ANCS → BlueZ/hci0 → ble_service event ring → UDS events_since
Phone Bridge HTTP queue → ble_service UDS wake → Wake Notify → iOS native HTTP poll
```

BLE carries notification metadata and a wake hint only. Phone Bridge commands
and results remain on WebSocket/HTTP transports over USB ECM, so BLE Wake still
requires the phone's wired board network. ANCS events do not enter Agent memory
in this layer.

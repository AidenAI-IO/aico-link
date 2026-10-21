---
sidebar_position: 2
---

# Source Code and Directory Structure

```text
aiden-firmware/
├── CMakeLists.txt                 # C/C++ targets; Debian/glibc is the device default
├── Makefile                       # Host test shortcut
├── debian_build.sh                # Complete Debian firmware build entry point
├── cmake/
│   ├── platforms/                 # Device userspace integration
│   └── toolchains/                # Debian armhf cross-toolchain
├── docs/                          # Structured documentation
├── edid/                          # HDMI EDID development assets
├── overlay-debian/                # Debian rootfs files, helpers, and systemd units
├── assets/business/              # Business package models and notification sounds
├── pico-sdk/                      # Vendored Luckfox BSP SDK
├── scripts/
│   ├── debian-apps/               # Debian armhf application build and audit
│   ├── debian-system/             # Rootfs, BSP, image assembly, and image audit
│   └── debian/                    # Migration inventories and service maps
├── src/                           # C/C++ SDK, services, tools, and protocols
├── src/agent/                     # Go device Agent
├── tests/                         # Host-native C++ tests
├── third_party/                   # Vendored libraries and device runtimes
└── upgrade_tool/                  # Rockchip flashing tool
```

## Key Files in `src/`

| File / Module | Description |
| --- | --- |
| `aiden_sdk.*` | C++ SDK entry point for wakeup, audio, camera, and HID capabilities |
| `frame_*` | Frame service, capture lifecycle, JPEG encoding, protocol, and client |
| `audio_*` | Audio service, recording/playback sessions, protocol, and client |
| `uds_*` | Unix domain socket transport |
| `service_status.*` | Common service status enums |
| `image_process.*` | Image cropping, black border removal, scaling, rotation |
| `hid_server.*`, `example_usb_hid.cpp` | USB HID gadget and HTTP example server |
| `agent/internal/configweb/` | Go Config Web HTTP service, maintaining Agent TOML and Wi-Fi configuration |
| `agent_toml.*` | C++ side Agent TOML parsing/writing |
| `system_env_*` | Strict persistent-to-runtime environment generation |

## Key Directories in `src/agent/`

| Path | Description |
| --- | --- |
| `cmd/daemon` | Agent daemon, HTTP/Web UI, and the legacy `stt` or provider-backed `realtime` voice loop |
| `cmd/abctl` | A/B metadata diagnostic utility |
| `cmd/ota` | OTA update client |
| `cmd/benchmark-http` | Standalone LLM endpoint latency probe |
| `cmd/test-warmup` | Connection warmup measurement harness |
| `internal/agent/runtime.go` | Model invocation, tool loop, and runtime lifecycle |
| `internal/agent/realtimevoice` | Provider-neutral realtime voice session seam and native Qwen, OpenAI, xAI, Gemini, and Speko adapters |
| `internal/agent/server.go` | HTTP server, Web UI, and tool API |
| `internal/agent/tools*.go` | HID, screenshot, audio, shell, and other tools |
| `internal/agent/stt.go`, `internal/agent/tts/`, `vad.go` | Voice pipeline |
| `internal/agent/skills*.go` | Skill loading, activation, and restrictions |
| `config/agent.toml` | Agent configuration example |

## Debian Rootfs Overlay

```text
overlay-debian/
├── etc/
│   ├── aiden_*.conf
│   ├── systemd/system/aiden-*.service
│   ├── systemd/system/aiden.target
│   ├── systemd/network/
│   └── profile.d/aiden-python.sh
└── usr/lib/aiden/
    ├── aiden-frame-start
    ├── aiden-usb-gadget
    ├── aiden-userdata-migrate
    └── ...
```

These files are copied only into the Debian rootfs. Executable source files
remain executable in the image; all other files and directories are normalized
to deterministic modes and root ownership.

## Platform and Business Files

`overlay-debian/usr/lib/aiden/` contains base-owned startup helpers and
`platform/lib/` contains the pinned VQE libraries. Rootfs assembly adds audited
RGA libraries to `platform/lib/` and SDK modules/firmware to `platform/modules/`.
EDID and VQE configuration live under `overlay-debian/usr/share/aiden/`.

`assets/business/models/` and `assets/business/audio/voice-notifications/` are
inputs to `aiden-business`, together with application binaries, web files and
skills. They are installed by dpkg into rootfs. The package does not own platform
libraries, modules, EDID, VQE configuration, or the OTA trust key.

## Build Layers

The production build has two explicit stages:

1. `scripts/debian-apps/build-apps.sh all` cross-compiles and audits C/C++ and Go applications for Debian armhf.
2. `scripts/debian-system/build.sh` builds the Debian rootfs and pinned BSP, assembles A/B images, and audits their contents.

`debian_build.sh` orchestrates both stages, creates local signed OTA metadata,
and publishes the flashable result under `output/debian/image/`.

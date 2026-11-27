---
sidebar_position: 2
---

# 源码与目录结构

```text
aico-link-firmware/
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

## `src/` 中的关键文件

| 文件 / 模块 | 说明 |
| --- | --- |
| `aico_sdk.*` | 唤醒、音频、摄像头和 HID 能力的 C++ SDK 入口 |
| `frame_*` | Frame 服务、采集生命周期、JPEG 编码、协议和客户端 |
| `audio_*` | 音频服务、录制/播放会话、协议和客户端 |
| `uds_*` | Unix 域套接字传输 |
| `service_status.*` | 通用服务状态枚举 |
| `image_process.*` | 图像裁剪、黑边去除、缩放、旋转 |
| `hid_server.*`、`example_usb_hid.cpp` | USB HID gadget 和 HTTP 示例服务器 |
| `agent/internal/configweb/` | Go Config Web HTTP 服务，维护 Agent TOML 和 Wi-Fi 配置 |
| `agent_toml.*` | C++ 端的 Agent TOML 解析/写入 |
| `system_env_*` | 严格的持久化环境到运行时环境的生成 |

## `src/agent/` 中的关键目录

| 路径 | 说明 |
| --- | --- |
| `cmd/daemon` | Agent 守护进程、HTTP/Web UI，以及旧版 `stt` 或基于提供商的 `realtime` 语音回路 |
| `cmd/abctl` | A/B 元数据诊断工具 |
| `cmd/ota` | OTA 更新客户端 |
| `cmd/benchmark-http` | 独立的 LLM 端点延迟探测工具 |
| `cmd/benchmark-mcu` | 用于隔离的基准测试守护进程容器的 MCU 固件模拟器；不会随设备发布 |
| `cmd/test-warmup` | 连接预热测量工具 |
| `internal/agent/runtime.go` | 模型调用、工具循环和运行时生命周期 |
| `internal/agent/realtimevoice` | 与提供商无关的实时语音会话接口，以及原生的 Qwen、OpenAI、xAI、Gemini、Speko 和 Moss 适配器 |
| `internal/agent/server.go` | HTTP 服务器、Web UI 和工具 API |
| `internal/agent/tools*.go` | HID、截图、音频、shell 等工具 |
| `internal/agent/stt.go`、`internal/agent/tts/`、`vad.go` | 语音流水线 |
| `internal/agent/skills*.go` | Skill 加载、激活和限制 |
| `config/agent.toml` | Agent 配置示例 |

## Debian rootfs overlay

```text
overlay-debian/
├── etc/
│   ├── aico_*.conf
│   ├── systemd/system/aico-*.service
│   ├── systemd/system/aico.target
│   ├── systemd/network/
│   └── profile.d/aico-python.sh
└── usr/lib/aico/
    ├── aico-frame-start
    ├── aico-usb-gadget
    ├── aico-userdata-migrate
    └── ...
```

这些文件只会复制到 Debian rootfs 中。可执行的源文件在镜像中保持可执行；其他所有文件和目录都会被规范化为确定的权限模式，
并归 root 所有。

## 平台文件与业务文件

`overlay-debian/usr/lib/aico/` 包含由基础系统负责的启动辅助脚本，`platform/lib/` 包含固定版本的 VQE 库。
rootfs 组装会把经过审计的 RGA 库添加到 `platform/lib/`，把 SDK 模块/固件添加到 `platform/modules/`。
EDID 和 VQE 配置位于 `overlay-debian/usr/share/aico/` 下。

`assets/business/models/` 和 `assets/business/audio/voice-notifications/` 与应用程序二进制文件、网页文件和
Skill 一起作为 `aico-business` 的输入。它们由 dpkg 安装到 rootfs 中。该软件包不负责平台库、模块、EDID、
VQE 配置或 OTA 信任密钥。

## 构建分层

生产构建有两个明确的阶段：

1. `scripts/debian-apps/build-apps.sh all` 为 Debian armhf 交叉编译并审计 C/C++ 和 Go 应用程序。
2. `scripts/debian-system/build.sh` 构建 Debian rootfs 和固定版本的 BSP，组装 A/B 镜像并审计其内容。

`debian_build.sh` 编排这两个阶段，生成本地已签名的 OTA 元数据，并把可烧录的结果发布到 `output/debian/image/` 下。

---
sidebar_position: 1
---

# 系统架构概览

Aico Link 是硬件平台。它将 HDMI 视频采集、音频录制/播放和 USB HID 控制与运行在开发板上的 AI Agent Aico 结合在一起。
官方网站是 [aico.link](https://aico.link)。

## 整体架构

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

## 模块分层

| 层 | 模块 | 说明 |
| --- | --- | --- |
| 硬件抽象 | `src/aico_sdk.*` | 封装 GPIO、音频、视频、HID 相关的底层能力 |
| 公共传输 | `src/uds_*` | Unix 域套接字一次性请求/响应传输层 |
| 硬件服务 | `frame_service`、`audio_service`、`ble_service` | 集中管理硬件资源并提供给其他进程使用 |
| 工具程序 | `*_cli`、`example_*`、`image_process` | 调试、验证和单一能力示例 |
| Go Agent | `src/agent` | LLM 运行时、工具调用、Web UI、HTTP Tool API、语音流水线 |
| 固件集成 | `overlay-debian/`、`assets/business/`、`pico-sdk/` | Debian systemd 集成、rootfs 资源与业务软件包，以及 RV1106 BSP |

## 关键设计原则

1. **硬件资源单一所有者**：例如 `/dev/video0` 由 `frame_service` 独占，其他使用方通过 UDS 读取帧，避免多个进程同时打开视频设备造成冲突。
2. **跨语言协议**：UDS 信封使用 JSON 头 + 二进制负载，Go / C++ / 其他语言都可以实现客户端。
3. **Agent 与硬件解耦**：Agent 不直接依赖 C++ ABI，而是通过套接字和 HID 设备完成观察/操作。
4. **systemd 监管**：Debian 单元为每个设备服务声明启动顺序、重启策略和失败处理。

## 主要数据流

### 截图 / 视觉观察

```text
Go screenshot request → frame_service STREAMON + one fresh /dev/video0 frame → STREAMOFF → LLM image input
```

### 设备控制

```text
LLM tool call → Go HID tool → /dev/hidg0, /dev/hidg1, or /dev/hidg2 → target device
```

### 语音交互

```text
audio_service recording → RKNN Silero VAD → STT or audio attachment → LLM → TTS → audio_service playback
```

### 蓝牙通知与唤醒

```text
iOS ANCS → BlueZ/hci0 → ble_service event ring → UDS events_since
Phone Bridge HTTP queue → ble_service UDS wake → Wake Notify → iOS native HTTP poll
```

BLE 只承载通知元数据和唤醒提示。手机桥接的命令和结果仍然通过 USB ECM 上的 WebSocket/HTTP 传输，
因此 BLE 唤醒仍然需要手机连接开发板的有线网络。在这一层，ANCS 事件不会进入 Agent 记忆。

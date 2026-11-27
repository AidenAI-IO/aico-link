---
sidebar_position: 3
---

# 硬件、接线与自行组装

## 目标硬件

本页介绍旧版的自行组装 Aico Link 开发板方案。该方案已不再维护；最后一个支持它的固件提交是 [`ad07c4d363f2948048415ed3af0b5e4da86bcac0`](https://github.com/AidenAI-IO/aico-link/tree/ad07c4d363f2948048415ed3af0b5e4da86bcac0)。请在断电状态下完成所有 FPC 和杜邦线连接，并在首次上电前重新检查电源和信号接线。

开发板丝印、排针方向和连接器位置可能因模块版本而异，因此请始终以实物模块上的丝印为准。

| 硬件 | 用途 | 数量 |
| --- | --- | --- |
| [Luckfox Pico Zero](https://wiki.luckfox.com/Luckfox-Pico-Zero)（RV1106） | 主板。运行 Aico Link 固件，提供 CSI、USB HID 和网络。 | 1 |
| [Firefly HDMI TO MIPI CSI](https://wiki.t-firefly.com/HDMI-TO-MIPI-CSI-RK628D/rk628d.html)（RK628D） | 四通道 HDMI 转 CSI 视频采集板，由 5 V 供电。 | 1 |
| HDMI ESD 保护器 | 保护 HDMI 视频输入通路。 | 1 |
| USB 转 TTL 模块 | 当 Pico Zero 的 USB 端口被占用时，用于串口控制台调试。 | 1（可选） |
| ASRPro 2.0 开发板 | 语音识别模块。 | 1 |
| Type-C 扩展坞（带 HDMI 和 USB） | 将目标设备的 Type-C 连接拆分为 HDMI 视频和 USB 数据/供电。 | 1 |
| USB-C 转 USB-A 数据线 | 将扩展坞的 USB-A 端口连接到 Pico Zero。 | 1 |
| HDMI 线 | 连接扩展坞、ESD 保护器和 RK628D 板。 | 1 |
| 定制 30P 转 22P FPC 转接板 | 将 Firefly RK628D 的 CSI 输出和控制信号连接到 Pico Zero 的 CSI 输入。 | 1 |
| 1 W / 8 Ω 扬声器 | 通过 Pico Zero 的扬声器输出播放语音。 | 1 |
| 杜邦线 | 连接 ASRPro、Pico Zero 和 USB 转 TTL 模块。准备公对公、公对母和母对母三种。 | 按需 |

## 核心连接拓扑

目标设备（iPhone / PC）通过 USB-C 扩展坞连接到 Aico Link。扩展坞将连接拆分为 HDMI 输出（用于屏幕采集）和 USB-C 输出（用于向 Pico Zero 提供数据/供电）：

```text
Target Device (iPhone / PC)
          │ USB-C
          ▼
      USB-C Hub
          ├─ HDMI out ──→ Firefly RK628D (HDMI to CSI bridge)
          │                      │ four-lane CSI + I2C + reset
          │                      ▼
          └─ USB-A out ──→ Luckfox Pico Zero (CSI input + USB)
                                   │
                                   ├─ Video capture: /dev/video0 (from RK628D CSI)
                                   ├─ HID keyboard/pointer/control: /dev/hidg0, /dev/hidg1, /dev/hidg2
                                   ├─ Audio codec / ALSA
                                   └─ Network: Wi-Fi or USB gadget network
```

物理接线概要：

1. 目标设备 USB-C → USB-C 扩展坞输入
2. 扩展坞 HDMI 输出 → Firefly RK628D HDMI 输入
3. Firefly RK628D 30 针输出 → 定制转接板 → Pico Zero 22 针 CSI 输入
4. 扩展坞 USB-A 输出 → Pico Zero USB-C 端口（数据 + 供电）

## RK628D 30 针转 Pico Zero 22 针转接板

请按照各开发板上连接器的丝印编号引脚，而不要根据翻转后 FPC 排线上可见铜箔的方向编号。

| Firefly 30 针 | 信号 | Pico Zero 22 针 | 信号 |
| ---: | --- | ---: | --- |
| 1 | SDA，1.8 V | 21 | SDA |
| 2 | SCL，1.8 V | 20 | SCL |
| 4 | HDMIIN_RESET | 17 | RST |
| 11 | D0P | 3 | D0P |
| 12 | D0N | 2 | D0N |
| 14 | D1P | 6 | D1P |
| 15 | D1N | 5 | D1N |
| 17 | CLK_P | 9 | CLK_P |
| 18 | CLK_N | 8 | CLK_N |
| 20 | D2P | 12 | D2P |
| 21 | D2N | 11 | D2N |
| 23 | D3P | 15 | D3P |
| 24 | D3N | 14 | D3N |
| 30 | 5 V 输入 | 5 V | 5 V |
| GND 引脚 | GND | GND 引脚 | GND |

保留差分对之间所有可用的地回路；不要把转接板简化成一根共用地线。Firefly 开发板规格书将引脚 29 和 30
都列为同一个 5 V 输入电源轨；当前转接板从引脚 30 供电。Pico 22 针的 MCLK（引脚 18）和 3.3 V（引脚 22）保持不连接。

在当前转接板中，Firefly 的引脚 7（`HDMIIN_INT`）和 8（`HDMIRX_DET`）也未连接。因此内核使用 RK628 驱动的
I2C 轮询路径。该开发板有自己的晶振，因此不需要 Pico 的 MCLK。

Pico 从 GPIO3_C5 驱动 `HDMIIN_RESET`，作为低电平有效的推挽输出，不带内部上拉或下拉。
I2C4 以 100 kHz 运行，以保证 RK628D 32 位寄存器传输的可靠性；同一总线也保留了对 TC358743 的支持。

## 组装步骤

1. 将目标手机或电脑连接到 Type-C 扩展坞的上行 Type-C 端口。
2. 依次将扩展坞的 HDMI 输出连接到 HDMI ESD 保护器、HDMI 线和 Firefly RK628D 的 HDMI 输入。
3. 使用定制的 30P 转 22P 转接板，将 RK628D 输出连接到 Pico Zero 的 CSI 连接器。确保两端连接器的卡扣都已扣紧，并在接通 5 V 之前核对引脚编号。
4. 使用 USB-C 转 USB-A 数据线，将扩展坞的 USB-A 端口连接到 Pico Zero 的 USB-C 端口。此连接在目标设备和 Pico Zero 之间提供 USB 数据通路和供电。
5. 将 ASRPro 的电源、地和控制信号连接到 Pico Zero。将 1 W / 8 Ω 扬声器直接连接到 Pico Zero 的扬声器输出，而不是 ASRPro。对照开发板丝印检查每一处连接。
6. 如需串口调试，将 Pico Zero 的调试排针连接到 USB 转 TTL 模块。除非已明确验证过供电设计，否则不要把 TTL 模块的电源线连接到 Pico Zero。

## 上电前检查清单

- HDMI、FPC 和 USB 线缆均已插紧；FPC 排线没有接反或损坏。
- 已对照开发板丝印核对 ASRPro 和 Pico Zero 的电源线和地线；没有杜邦线短接相邻引脚。
- 扬声器直接连接到 Pico Zero 的扬声器输出，而不是 ASRPro。
- HDMI ESD 保护器安装在扩展坞和 RK628D 板之间。
- RK628D 转接板只提供 5 V；Pico 的 MCLK 和 3.3 V 未连接。
- 全部四条 MIPI 通道、时钟对、I2C、复位以及可用的地回路端到端连通。
- USB 转 TTL 模块只使用信号线和地线，并且 `TX` 和 `RX` 交叉连接。
- 首次测试时，先确认系统检测到视频采集设备，再测试 USB HID 和语音功能。

## iOS HID 前提条件

在本页适用的旧版固件上（截至
[`ad07c4d363f2948048415ed3af0b5e4da86bcac0`](https://github.com/AidenAI-IO/aico-link/tree/ad07c4d363f2948048415ed3af0b5e4da86bcac0)），
iOS 通过绝对坐标鼠标进行控制，因此需要在目标 iOS 设备上开启：

```text
Settings > Accessibility > Touch > AssistiveTouch
```

还建议在 AssistiveTouch 页面中开启 **Show Onscreen Keyboard**，以获得更稳定的键盘输入体验。

当前固件改用 HID 触控板驱动 iOS，需要打开 Zoom 并关闭 AssistiveTouch；参见 [USB HID](../03-services/usb-hid.md)。

## 默认设备节点

| 能力 | 默认节点 / 路径 | 说明 |
| --- | --- | --- |
| 视频采集 | `/dev/video0` | RK628D 或 TC358743 桥接芯片输出到 V4L2 采集设备 |
| HDMI 子设备 | 自动检测到的 `rk628-csi` 或 `tc358743` 节点 | EDID / DV timings / HDMI 同步；具体的 `/dev/v4l-subdevX` 编号可能不同 |
| 键盘 HID | `/dev/hidg0` | Go Agent 和示例工具使用的默认键盘设备 |
| 鼠标/触控 HID | `/dev/hidg1` | Go Agent 使用同一个 HID 设备进行鼠标/触控输入 |
| 辅助控制 HID | `/dev/hidg2` | 触屏模式下的 Android 扩展按键；绝对坐标模式下的 Consumer Control 媒体、音量、亮度和截图按键 |
| Frame 套接字 | `/run/frame_service/frame_service.sock` | 系统服务部署时的默认路径 |
| 音频套接字 | `/run/audio_service/audio_service.sock` | 系统服务部署时的默认路径 |

在开发期间直接运行二进制文件时，默认的 frame 套接字为 `/tmp/frame_service.sock`；使用固件启动脚本部署后，默认路径为 `/run/frame_service/frame_service.sock`。

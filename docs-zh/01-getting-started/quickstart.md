---
sidebar_position: 2
---

# 新手快速上手

本页是首次接触 Aico Link 硬件的主线路径。它按照 **接线 → 烧录 → 连接 Wi-Fi → 配置 → 运行 → 开发 → 升级 → 故障排查** 的顺序，把整个上手流程串在一页中。每一步只介绍关键操作；细节链接到对应的专题文档。

> 还没有开发板？先从 [在 PC 上试用 Aico](try-aico-on-pc.md) 开始，在本地打开 Config Web 和 Agent Web，
> 然后可以选择将 Agent 连接到 MobileGym、ADB 目标或其他兼容的环境桥接。

> 语音交互是 Aico 的核心使用场景。Agent Web UI 在所有输入模式下都可用。`text` 模式主要用于开发和测试，因为它会关闭设备端的音频回路，同时保留 HTTP 接口可用。

## 概览

| 步骤 | 目标 | 详细文档 |
| --- | --- | --- |
| 1 | 按照硬件设计接线（仅限旧版自行组装开发板） | [硬件与接线（旧版）](hardware.md) |
| 2 | 将 `update.img` 固件烧录到 Pico Zero | [固件构建与烧录](firmware.md) |
| 3 | 打开配置页面并连接 Wi-Fi | [Config Web](../04-agent/configuration.md#config-web设备配置页面) |
| 4 | 配置 model / tts / stt 等密钥 | [Agent 配置](../04-agent/configuration.md) |
| 5 | 启动开发板并验证 | [测试与验证](testing.md) |
| 6 | 了解架构和模块 | [架构概览](../02-architecture/overview.md) |
| 7 | 固件构建与 OTA 升级 | [固件](firmware.md) / [OTA](../08-ota/README.md) |
| 8 | 出现问题时进行故障排查 | [故障排查](../07-operations/troubleshooting.md) |

## 1. 硬件与接线

本步骤涉及的自行组装开发板属于旧版，已不再维护；最后一个支持它的固件提交参见 [硬件与接线（旧版）](hardware.md)。按照该页面完成接线。关键连接流程：

- 目标设备（iPhone / PC）连接到 **USB-C 扩展坞**；
- 扩展坞的 **HDMI 输出** 接到 Firefly RK628D HDMI 转 CSI 桥接板，后者通过四条 CSI 通道为 Pico Zero 的 `/dev/video0` 提供画面；
- 扩展坞的 **USB-C 输出** 连接到 Pico Zero，提供数据和供电；
- Pico Zero 通过 USB 向目标设备暴露 `/dev/hidg0`（键盘输入）、`/dev/hidg1`（指针/触控输入）以及 `/dev/hidg2`（Android 扩展按键或 Consumer Control 媒体按键）；
- 音频通过 Pico Zero 板载的 codec / ALSA；网络通过 Wi-Fi 或 USB gadget 网络。

在当前固件上通过 USB HID 控制 iOS 设备时，请在目标设备上开启 `Settings > Accessibility > Zoom`，并关闭 `Settings > Accessibility > Touch > AssistiveTouch`。该组装方案对应的旧版固件则需要开启 AssistiveTouch；参见 [iOS HID 前提条件](hardware.md#ios-hid-前提条件)。

## 2. 烧录固件

将 Pico Zero 的 USB-C 端口连接到电脑，并烧录预构建的 `update.img`。烧录方法有多种；主要参考 Pico Zero 官方文档：

- [Luckfox Pico Zero 烧录指南](https://wiki.luckfox.com/zh/Luckfox-Pico-Zero/Flash-image/)

核心流程是先让开发板进入烧录（Maskrom / Loader）模式，再用烧录工具写入 `update.img`：

在 Linux 主机上，使用带保护机制的烧录辅助脚本，以便在出厂覆盖之前校验镜像摘要：

```bash
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=./update.img
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" --sha256 "<verified-sha256>" \
  --confirm-erase-all-data
```

macOS 命令和完整的本地构建示例参见 [固件构建与烧录](firmware.md)。

进入烧录模式最常用的方法是按住 BOOT 按键的同时插入 USB-C。**如果 BOOT 按键不能可靠地工作**，可以先通过 USB 网络上的 SSH 或 TTL 串口控制台登录开发板，然后让 systemd 传递 loader 参数：

```bash
systemctl reboot --reboot-argument=loader
```

即可进入烧录模式。预构建固件、本地构建路径、`upgrade_tool` 的用法和分区布局参见 [固件构建与烧录](firmware.md)。

## 3. 连接 Wi-Fi

烧录完成且设备启动后，通过 USB-C 将设备连接到电脑，并在浏览器中打开设备配置页面：

```text
http://192.168.42.1
```

`192.168.42.1` 是 USB 网络的网关地址。配置页面由设备上 Go Agent 的 `config-web` 子命令提供（默认端口 80）。

在页面上先配置 Wi-Fi：

- **只支持 2.4GHz 网络**；5GHz 无法使用；
- 输入 SSID 和密码；保存时会写入
  `/userdata/debian/wifi/wpa_supplicant-wlan0.conf`。

配置页面还维护 Agent 配置和系统环境变量。详情参见 [Config Web](../04-agent/configuration.md#config-web设备配置页面)。

## 4. 配置密钥

在同一个配置页面中填写各项服务的密钥。其中：

- **`[model_settings.model]` 是必填项**，并且必须是多模态 LLM（Agent 需要把截图作为图像输入发送给模型）；
- **`[voice_settings.classic.tts]`** 启用语音播放（把模型的回复转成语音）；
- **`[voice_settings.classic.stt]`** 启用语音转文字，用于语音输入。

### 选择 Agent 模式

Agent 的 HTTP 服务器和 Web UI 始终会启动。`input_mode` 决定是否同时运行设备端的语音回路：

| 模式 | 行为 | 使用场景 |
| --- | --- | --- |
| `stt` | HTTP 服务器 + Web UI，同时并行运行设备录音 → VAD → STT 转文字 → LLM → TTS 播放 | 语音交互，Web UI 仍然可用 |
| `text` | 仅 HTTP 服务器 + Web UI | 主要用于测试；使用 8080 端口上的网页进行文字交互 |

`stt` 模式会先将语音转写为文字再发送给 LLM，同时提供相同的 HTTP 接口。`text` 模式主要用于测试，因为它跳过了设备的音频回路。已移除的 `audio` 模式（将原始音频直接发送给 LLM）不再受支持。

> 注意这是两个不同的网页：`http://192.168.42.1`（端口 80）是设备配置页面；`http://<device-ip>:8080` 是 Agent Web UI，在 `text` 和 `stt` 两种模式下都可用。

字段含义、最小可用配置示例以及 TTS/STT 提供商取值参见 [Agent 配置](../04-agent/configuration.md)。

## 5. 启动开发板

完成上述四步后，开发板就可以运行了。Agent 由 `aico-agent.service` 管理，随固件一起启动。

**配置变更**：Config Web 会在你保存时在线应用受支持的设置。语言区域和提示词的变更在下一个任务或会话边界生效；只有文档中注明的重启例外项需要显式重启 Agent 或重启设备，页面会给出提示。

### 目标设备设置（iPhone）

在当前固件上通过 USB HID 控制 iPhone 时，请在目标设备上启用以下设置：

1. **Settings > Accessibility > Zoom** - 开启。HID 触控输入需要它
2. **Settings > Accessibility > Touch > AssistiveTouch** - 关闭。开启时 iOS 会忽略开发板的触控输入

旧版固件使用绝对坐标鼠标，需要改为开启 AssistiveTouch。详情参见 [硬件与接线](hardware.md#ios-hid-前提条件)。

### 验证

用于验证整条链路（Frame / Audio / Agent 服务、截图、语音）的命令参见 [测试与验证](testing.md)。服务日志位置和常用服务命令参见 [部署](deployment.md)。

接下来是开发部分。

## 6. 架构与模块

开始开发之前，先建立整体认识：

- [架构概览](../02-architecture/overview.md)：HDMI 采集、音频录制/播放、USB HID 和 LLM Agent 如何协同工作，以及主要的数据流。

然后深入各个模块：

- [Frame 服务](../03-services/frame-service.md)：HDMI 帧采集服务
- [音频服务](../03-services/audio-service.md)：音频录制/播放服务
- [USB HID 与设备控制](../03-services/usb-hid.md)
- [Go Agent 概览](../04-agent/overview.md)：LLM 运行时、工具调用、语音流水线
- [Agent 配置](../04-agent/configuration.md)：agent.toml 参考以及 Config Web 配置页面
- [Tools HTTP API](../04-agent/tools-http-api.md)

源码布局参见 [源码树](../02-architecture/source-tree.md)；启动服务和运行时布局参见 [启动服务与运行时布局](../02-architecture/boot-services.md)。

## 7. 固件构建与 OTA 升级

开发完成后，构建固件并升级设备：

- 原生 / 交叉编译开发环境：[构建与开发环境](build.md)；
- 完整的 Debian 固件构建（`./debian_build.sh`）和烧录：[固件构建与烧录](firmware.md)；
- 空中升级：[OTA 概览](../08-ota/README.md)。

### 开发构建的 OTA

**Aico Link Channel Release** 工作流可以手动发布 `dev`、`staging` 或 `prod`，并根据自该通道上一个发布版本以来的改动选择业务软件包或完整 OTA。参见 [通道发布](../08-ota/channel-release.md)。
对于未发布的本地构建，使用通道匹配的测试设备，从开发用的 HTTP(S) 端点提供其产物，并显式地将设备指向其清单：

```bash
BASE_URL="http://192.168.1.100:8000"

ota update \
  --manifest-url "$BASE_URL/manifest.json" \
  --public-key /usr/share/keyrings/aico-ota.pem \
  --dry-run
```

公钥必须与本地构建使用的密钥匹配。先使用 `--dry-run` 验证下载，而不切换槽位。托管细节参见 [面向外部开发者的 OTA](../08-ota/ota-external-developers.md)。

## 8. 故障排查

遇到问题时，先查看 [故障排查](../07-operations/troubleshooting.md)，其中涵盖常见的连接、服务、视频/音频、Agent 和 OTA 问题。日志位置参见 [部署](deployment.md)。

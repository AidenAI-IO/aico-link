---
sidebar_position: 1.5
title: 使用真实手机运行本地 Agent
description: 一条命令启动原生 Agent、设置页面以及 Android 或 iPhone 连接。
---

# 使用真实手机运行本地 Agent

无需 Aico Link 开发板或 Docker 沙箱，直接在你的电脑上配合真实手机运行 Aico。
一个启动器即可启动 Config Web、手机桥接（Phone Bridge）和 Agent Web，并在按下 `Ctrl-C` 时一并停止它自己启动的进程。

> **这是模拟器，而不是产品。** 在 PC 上运行 Aico 是用软件模拟 Aico Link，让你在硬件发货前试用 Aico。
> 它不是正式发布的产品，也无法复现硬件体验：它依赖 ADB 或其他软件桥接，而不是 HDMI 采集和 USB HID，
> 并且没有开发板上的麦克风或扬声器。

## 一次性准备

安装 Git、Go 1.26 或更高版本、Python 3.10 或更高版本，以及用于隔离手机桥接依赖的
[`uv`](https://docs.astral.sh/uv/getting-started/installation/)。准备好模型提供商和 API key。

首次启动时，启动器会通过 `scripts/build-config-web.sh` 把 Config Web 资源构建到 `src/config_web/dist`，
这一步需要 Docker 和 `rsync`。之后的启动会复用已构建的资源，只有使用 `--rebuild-web` 时才需要再次用到 Docker。
Agent 本身仍然原生运行，而不是运行在 Docker 沙箱中。

```bash
git clone https://github.com/AidenAI-IO/aico-link.git
cd aico-link
```

手机工作流不需要 MobileGym 子模块，也不需要构建开发板固件。为所选模式准备连接：

| 模式 | 电脑和手机的前提条件 | 参考 |
| --- | --- | --- |
| `android` | macOS/Linux、Android SDK Platform Tools（`adb`）、已授权 USB 调试或已配对无线 ADB；`adb devices` 显示 `device` | [ADB Android](../../benchmark/adbandroid/README.md) |
| `iphone-mirroring` | 兼容的 Mac、Xcode Command Line Tools（`xcrun swiftc`），手机已在 Apple 的 iPhone 镜像 App 中连接并保持锁定 | [iPhone 镜像](../../benchmark/iphone_mirroring/README.md) |
| `ios` | macOS、兼容的完整 Xcode、USB 配对、开发者模式、已安装的已签名 WDA runner，手机保持解锁 | [pymobiledevice3 / WDA 设置](../../benchmark/ios_device/README.md#prerequisites-and-installation) |

对于 iPhone 镜像，需要显式请求一次 macOS 辅助功能和屏幕录制权限，并批准 macOS 标识出的应用程序：

```bash
python3 scripts/local_agent.py iphone-mirroring --request-permissions
```

批准权限后，重新执行常规启动。生成的辅助程序路径保持不变，以便 macOS 保留已授予的权限。
对于 USB 直连的 iOS，首次的“信任此电脑”以及开发者/签名授权仍需你手动完成；启动器不会绕过它们。

## 一条命令启动

在仓库根目录下运行与你的连接对应的命令：

```bash
python3 scripts/local_agent.py android
```

```bash
python3 scripts/local_agent.py iphone-mirroring
```

```bash
python3 scripts/local_agent.py ios
```

启动器会利用 Go 的构建缓存构建原生 Agent，在需要时准备隔离的 Python 环境，仅在缺失时安装该目标的快捷操作，
并打开 Config Web。首次使用时，添加模型提供商/API key 并保存；创建 `agent.toml` 后启动会自动继续。
之后的启动会复用该文件。Config Web 仍然可以把支持的设置应用到正在运行的 Agent。

在报告就绪之前，启动器会检查桥接类型、占用任务会话、使用匹配的设备/端点/任务参数启动 Agent，
并通过 Agent 验证一张新截图。Android 设置流程会返回主屏幕并收起键盘；不会清除应用数据。
iPhone 镜像的连接界面本身也可能被截取，因此在交互之前，还要检查 Apple 的连接状态以及 Agent 的第一次观察结果。

| 模式 | Agent Web | Config Web | 桥接 | 数据目录 |
| --- | --- | --- | --- | --- |
| `android` | `http://127.0.0.1:8080` | `http://127.0.0.1:8000` | `8899` | `.cache/native-phone-lab/android` |
| `iphone-mirroring` | `http://127.0.0.1:8081` | `http://127.0.0.1:8001` | `8897` | `.cache/native-phone-lab/iphone-mirroring` |
| `ios` | `http://127.0.0.1:8082` | `http://127.0.0.1:8002` | `8896` | `.cache/native-phone-lab/ios` |

每种模式都把其配置、对话、快捷操作和日志保存在单独的目录中。启动器会保留现有配置和自定义的快捷操作。
它会拒绝使用已被标记为其他模式的数据目录，也不会停止占用其端口的进程。
同一时间只运行一种 iPhone 控制模式：镜像模式需要手机锁定，而 USB/WDA 需要手机解锁。

## 现有配置与设备选择

首次使用时导入一份配置，且不会替换已有的目标文件：

```bash
python3 scripts/local_agent.py ios --config /absolute/path/to/agent.toml
```

复用已有的 Agent 数据目录（包括其中的对话）：

```bash
python3 scripts/local_agent.py ios --data-dir /absolute/path/to/ios-agent-data
```

Android 会自动选择唯一已授权的设备。连接了多部手机时，请显式选择一部：

```bash
python3 scripts/local_agent.py android --adb-serial YOUR_ADB_SERIAL
python3 scripts/local_agent.py ios --udid YOUR_DEVICE_ID
```

默认安装的 iOS runner 是 `io.aiden.aico.WebDriverAgentRunner.xctrunner`。
可以按需选择其他已签名的 runner，或启用针对特定应用的权限规则：

```bash
python3 scripts/local_agent.py ios --xctrunner YOUR_RUNNER_ID.xctrunner --permission-policy /absolute/path/to/permissions.json
```

未知或含糊的权限弹窗会保持显示。关于规则匹配、512 个 UTF-16 单元的输入限制以及安全界面的限制，参见
[iOS 直连参考](../../benchmark/ios_device/README.md)。Android 通过可选的
[ADBKeyBoard 设置](../../benchmark/adbandroid/README.md#unicode-text-with-adbkeyboard)支持提交 Unicode 文本。
默认使用 ADBKeyBoard：桥接会临时选择它进行 Base64 输入，然后恢复原来的键盘。如果想改用 Appium Settings 的 UnicodeIME，
请用 `--unicode-input-method appium` 启动 Android 模式。如果所选输入法缺失且设备在启动时已就绪，桥接会自动安装它；
安装失败时，启动会在桥接服务器启动之前终止。如果设备尚未就绪，安装会推迟到设备响应之后的某次能力探测或文本输入时进行。
桥接运行期间，如果所选输入法无法安装，则保留 ASCII 输入。Appium 路径通过 `adb input text` 发送 modified UTF-7，
无法发送字面量 `%s`。两条路径在桥接启动时选定，彼此不会相互回退。参见
[Appium UnicodeIME 设置](../../benchmark/adbandroid/README.md#appium-unicodeime-alternative)。
镜像模式通过 Mac 支持提交 Unicode 文本。

在 USB/WDA 模式下，Agent 通过原生的 `insert_text` 发送原始 Unicode，并通过截图验证结果。
直接调用桥接的 `/api/tools/enter_text` 需要提供 `text` 和 `focus`；它们会通过 WDA 验证同一个无障碍字段的值。
两条路径都会避免重放投递结果不确定的输入。重试失败的文本输入之前，请先检查该字段。

对于已有的桥接，传入它的回环地址。启动器会验证其类型与模式匹配，并占用同一条任务路由；退出时不会停止该桥接进程：

```bash
python3 scripts/local_agent.py ios --bridge-url http://127.0.0.1:8896
```

使用 `--agent-binary` 复用原生二进制文件，使用 `--python` 指定已准备好的桥接环境，使用 `--no-open`
只打印 URL 而不打开浏览器。必要时可以覆盖 `--agent-port`、`--config-port`、`--bridge-port` 或 `--share-port`。
运行 `python3 scripts/local_agent.py --help` 查看所有选项。

## 停止、恢复或切换模式

在启动终端中按 `Ctrl-C`。该次调用启动的 Agent、Config Web 和桥接会一并停止；配置和对话会保留。
停止当前模式后，运行另一条单行命令即可切换模式。子进程失败也会停止其他由启动器管理的进程，并报告其日志路径。
日志为目标数据目录下的 `agent.log`、`config-web.log` 和 `bridge.log`；启动器从不打印凭据值。

对于空闲的镜像会话，先在 Apple 的 App 中恢复连接，然后在 Config Web 概览页上使用
**Restore iPhone Mirroring connection**。对于 USB/WDA 断开，解锁/重新连接手机并重新运行启动器。
对于 ADB，将设备恢复到 `device` 状态；如果无线调试改变了序列号，请选择新的序列号。
任何输入超时之后都要检查屏幕；启动和恢复流程绝不会重放可能已经投递的文本/触摸请求。

手机桥接的联系人/日历 API，以及开发板的音频、GPIO、USB HID 和固件服务，都是独立的能力。
仅有截图/输入连接并不能提供它们。基于容器的 Agent 参见 [在 PC 上试用 Aico](try-aico-on-pc.md)。
验证好手机之后，继续阅读 [Benchmark](../09-benchmark/README.md) 了解测试套件、评分和报告。
各桥接的专属参考文档保留了手动启动命令，用于调试单个组件。

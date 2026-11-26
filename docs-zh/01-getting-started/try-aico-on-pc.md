---
sidebar_position: 1
title: 在 PC 上试用 Aico
description: 在 Aico Link 硬件发货之前，在你的电脑上试用 Aico 模拟器。
---

# 在 PC 上试用 Aico

> **这是模拟器，而不是产品。** 在 PC 上运行 Aico 是用软件模拟 Aico Link，让你在硬件发货前试用 Aico。
> 它不是正式发布的产品，也无法复现硬件体验：它依赖 ADB 或其他软件桥接，而不是 HDMI 采集和 USB HID，
> 并且没有开发板上的麦克风或扬声器。在 Realtime 模式下，语音通过浏览器麦克风进行。

如果想在电脑上直接运行原生 Agent 并配合真实的 Android 手机或 iPhone，请从
[使用真实手机运行本地 Agent](local-agent-real-phone.md) 开始。该指南提供了完整的 Config Web 和手机桥接启动流程。

Docker 沙箱是在没有 Luckfox 开发板的情况下运行 Aico 的最快方式。它启动的配置和 Agent 网页体验与设备开发时所用的相同，
同时把配置和 Agent 状态保存在一个 Docker 命名卷中。

此工作流适用于交互式开发、配置检查和 Agent 测试。如需可重复的任务套件、并行环境、评分和报告，
请改用 [Benchmark 工作流](../09-benchmark/README.md)。

## Aico Launcher

试用 Aico 最快的方式是仓库自带的 Aico Launcher。它是一个 Web UI，可以构建或选择 `aico-link` 镜像、
启动环境桥接，并一键运行镜像。桥接类型包括：

- **No device**：仅聊天；
- **MobileGym simulator**：在 Docker 中模拟的 Android 手机；
- **Android (ADB)**：通过 `adb` 连接的手机、模拟器或虚拟机；
- **Host desktop**：这台电脑的屏幕、鼠标和键盘（macOS 和 Linux）；
- **iPhone Mirroring**：通过 macOS iPhone 镜像连接的真实 iPhone；
- **iOS device**：通过 WebDriverAgent 经 USB 连接的真实 iPhone（macOS）；
- **Virtual iPhone**：vphone-cli 虚拟 iPhone（macOS）；
- **Custom bridge**：你已经在运行的任意 Environment Bridge。

在仓库根目录下运行：

```bash
make launcher
```

打开启动器打印的 URL。它需要 Docker 和 [`uv`](https://docs.astral.sh/uv/getting-started/installation/)。
关于桥接的前提条件、状态、安全性和故障排查，参见 [Aico Launcher 指南](../../benchmark/launcher/README.md)。
在 Host desktop、iPhone Mirroring 或 iOS device 实例运行期间，请保持启动器运行：它会续期这些桥接的路由，
否则这些桥接会在空闲 30 分钟后释放路由。Launcher 实例使用 PC 设置配置，包括模型、搜索、语言、日志与导出、
Realtime/Classic 语音提供商，以及记忆保留与重置。启用容器终端时，概览页会提供指向它的链接；
仅适用于开发板的控件不可用。ADB 设备选择和连接仍在启动器中完成。本页其余部分介绍 Compose 沙箱和手动桥接设置。

## 前提条件

- Docker Desktop，或带 Compose 插件的 Docker Engine；
- Git；
- 一个模型提供商和 API key，启动后进行配置；
- 如果希望 Agent 观察和控制模拟器、仿真器、ADB 实体设备或虚拟机，还需要一个可选的环境桥接；
- 对于 MobileGym 和 ADB Android 桥接工作流，主机上需要 [`uv`](https://docs.astral.sh/uv/getting-started/installation/)；
- 对于 ADB Android 工作流，需要提供 `adb` 的
  [Android SDK Platform Tools](https://developer.android.com/tools/releases/platform-tools)。

沙箱不会构建或使用内置的 `pico-sdk/` BSP。

## 获取源码

基础沙箱不需要递归克隆：

```bash
git clone https://github.com/AidenAI-IO/aico-link.git aico-link-firmware
cd aico-link-firmware
```

## 启动沙箱

在仓库根目录下运行：

```bash
make sandbox-start
```

该命令会从当前源码重新构建镜像，在后台启动沙箱，并等待两个 Web 服务都变为健康状态。
没有改动时，Docker 的层缓存会让重复运行很快，因此运行中的沙箱始终与工作区保持一致。服务就绪后，打开：

| 页面 | URL | 用途 |
| --- | --- | --- |
| Config Web | [http://localhost:8000](http://localhost:8000) | 配置模型、STT、TTS、Agent 以及其他受支持的设置 |
| Agent Web | [http://localhost:8080](http://localhost:8080) | 与运行中的 Agent 聊天并查看其状态 |
| 终端 | [http://localhost:8080/webtty/](http://localhost:8080/webtty/) | 在沙箱容器内打开 ttyd shell |

Config Web 会把改动写入沙箱的持久化命名卷。因此重启或重建容器不会丢失已保存的配置或其他持久化的 Agent 状态。
不要把 API key 放在受版本控制的仓库文件中；请通过 Config Web 输入。保存受支持的 Agent 设置会自动请求重启 Agent，
因此新配置无需重启整个 Compose 栈即可生效。

如果想改为在前台构建并运行沙箱：

```bash
docker compose up --build
```

使用以下命令跟踪服务输出：

```bash
docker compose logs -f
```

修改 Aico 源码后，再次运行 `make sandbox-start` 以重新构建镜像并替换运行中的容器。

启动沙箱会保留 `aico-data` 卷。使用 `make sandbox-logs` 跟踪 Agent 日志，使用 `make sandbox-stop` 停止沙箱。

### 镜像运行时与构建选项

沙箱和 Aico Launcher 使用同一个镜像，它由 `docker/dev/Dockerfile` 针对 `linux/amd64` 或 `linux/arm64` 原生构建。
它在容器端口 80 上提供 Config Web，在 8080 上提供 Agent Web UI，均为纯 HTTP；只能将它们发布在 `127.0.0.1` 上。
除了下文介绍的桥接变量外，镜像还会读取：

| 变量 | 镜像默认值 | 作用 |
| --- | --- | --- |
| `AICO_CONFIG_WEB_PROFILE` | `board` | `container` 提供 PC 设置配置，包括模型提供商、对话、搜索、MCP 服务器、语言、日志/导出、Realtime 和 Classic 语音提供商，以及记忆保留/重置。Launcher 会自动设置它。在此配置下，开发板相关的 API 和页面不可用。 |
| `AICO_ENABLE_TERMINAL` | `0` | `1` 会在 Agent Web UI 的 `/webtty/` 后面启动 `ttyd`。Compose 沙箱将其设为 `1`。 |
| `AICO_CLASSIC_VOICE` | `0` | Classic 语音需要 VAD 辅助程序和匹配的模型，而镜像中没有它们，因此 Classic 模式默认只支持文字。Config Web 仍会显示其 STT/TTS 设置并报告启动门槛；保存提供商并不会补齐缺失的运行时。启用 Classic 语音之前，请先提供运行时依赖。Realtime 模式保留文字和浏览器语音：`localhost` 是安全上下文，因此无需 TLS 浏览器麦克风即可工作。 |
| `AICO_LOCAL_PHONE_BRIDGE` | `0` | `1` 会在环境桥接负责屏幕和输入时，让配套 App 的连接、状态和命令队列保留在本 Agent 上。ADB Android 桥接没有 Phone Bridge API，因此应为其设置此项。Aico Launcher 会为 Android (ADB) 实例自动设置。 |
| `AICO_QUICK_ACTIONS_PRESET` | 空 | `ios_device` 或 `iphone_mirroring` 会在 Agent 启动前，将对应的内置 iOS 快捷操作预设安装为 `/userdata/agent/quick_actions.json`。Aico Launcher 会为匹配的桥接类型设置它。未修改过的预设副本会在预设变化时被替换，在清空该变量时被删除；你编辑过的 `quick_actions.json` 永远不会被覆盖。 |

构建参数可以让你通过镜像源进行构建，并为构建打上标记：

| 构建参数 | 默认值 | 用途 |
| --- | --- | --- |
| `BASE_IMAGE` | 固定版本的 `debian:trixie-slim@sha256:...` | 所有阶段的基础镜像，例如镜像仓库加速源 `docker.1ms.run/library/debian:trixie-slim@sha256:<same digest>` |
| `GO_DOWNLOAD_BASE` | `https://go.dev/dl` | Go 工具链下载地址，例如 `https://golang.google.cn/dl` |
| `GOPROXY` | Go 的默认值 | `go mod download` 使用的 Go 模块代理，例如 `https://goproxy.cn,direct` |
| `AICO_VERSION` | `dev` | 写入 Agent 和 `org.opencontainers.image.version` 标签的版本 |
| `AICO_COMMIT` | `unknown` | 写入 Agent 和 `org.opencontainers.image.revision` 标签的提交 |

例如：

```bash
docker build -f docker/dev/Dockerfile \
  --build-arg GO_DOWNLOAD_BASE=https://golang.google.cn/dl \
  --build-arg AICO_VERSION=local \
  --build-arg AICO_COMMIT="$(git rev-parse HEAD)" \
  -t aico-link:local .
```

Compose 沙箱会把 shell 中的 `AICO_DOCKER_BASE_IMAGE`、`AICO_GO_DOWNLOAD_BASE`、`AICO_VERSION` 和
`AICO_COMMIT` 转发为这些构建参数。Aico Launcher 会根据其 Settings 面板和当前检出设置它们，
并在 shell 中设置了 `GOPROXY` 时将其传递下去。

Dockerfile 需要 BuildKit，Docker Desktop 以及安装了 `docker-buildx` 插件的 Docker Engine 默认都使用它；
不支持 `DOCKER_BUILDKIT=0`。

在 Apple Silicon 上，沙箱现在以原生 `arm64` 运行，而不是模拟的 `amd64`。包含原生代码的 Python 用户软件包
只能在安装它们的架构上加载，因此入口脚本会把架构记录在 `/userdata/agent/python/.aico-arch` 中，
并在架构变化时把该目录移到一边。不带该标记的非空目录（来自标记出现之前创建的卷）架构未知，
因此会被移到 `/userdata/agent/python.unknown.<timestamp>`。切换之后，请重新安装 Agent Skill 所需的 Python 软件包，
并在不再需要旧目录时将其删除。

## 配置原生本地 Agent

对于直接在 macOS 或 Linux 上运行的 Agent，使用 Config Web 的主机配置。构建原生二进制文件和静态前端，
然后在仓库根目录下启动该门户：

```bash
(cd src/agent && go build -o /absolute/path/to/agent ./cmd/daemon)
bash scripts/build-config-web.sh
```

```bash
/absolute/path/to/agent config-web --local-agent \
  --config /absolute/path/to/agent-data/agent.toml \
  --web-root "$PWD/src/config_web/dist" \
  --agent-url http://127.0.0.1:8080
```

打开 [http://127.0.0.1:8000](http://127.0.0.1:8000)。使用与运行中守护进程的 `-dir` 相同的 `agent.toml`。
`--local-agent` 默认使用端口 8000，并且只接受回环地址绑定。`--port` 和 `--agent-url` 支持其他本地端口。
该命令继承主机进程的环境变量，包括显式的代理设置；它不会加载开发板的系统环境或 Wi-Fi 代理。

主机配置提供模型提供商及凭据测试、主模型和决策模型、提示词和上下文限制、网页搜索、语言、时区以及 Agent/模型日志。
API key 仍然是只写的。API 和导航都不包含开发板的 Wi-Fi、GPIO、语音硬件、存储维护、固件和系统服务控制。
设备类型和环境桥接连接仍然是启动参数；概览页会报告运行中 Agent 的实际目标类型。

保存会保留其他配置段，并请求实时重新加载。状态栏会区分已保存、待应用、已应用和失败的配置。
Agent 离线时仍然可以保存配置；使用相同的数据目录启动它即可加载已保存的值。该门户不会启动或重启守护进程。
现有的开发板和 Docker 沙箱配置保持不变。

## 连接环境桥接

没有环境桥接时，Config Web 和 Agent Web 可以工作，但 Agent 没有可观察或控制的外部屏幕。
桥接通过仓库的 [Environment Bridge 协议](../../benchmark/environment_bridge.md) 提供截图和输入工具。

先在主机上启动桥接，然后在启动沙箱时传入其可从主机访问的端点：

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
  docker compose up --build
```

请使用 `host.docker.internal`，而不是 `localhost` 或 `127.0.0.1`：在 Agent 容器内部，后两个地址指向容器自身。
根目录的 Compose 栈会在需要时把 `host.docker.internal` 映射到主机网关。

端点在创建 Agent 容器时生效。如果沙箱已经在运行，请先停止它，再带上该环境变量重新运行命令。

沙箱默认使用 `AICO_BENCHMARK_TASK_ID=docker-sandbox`，并在启动期间调用桥接的 `/api/setup` 端点。
之后它会在转发的工具请求中附带同一个 task id，使多环境桥接把这个交互式会话保持在同一个底层环境上。
如需选择另一条稳定的路由，请在启动 Compose 时同时设置这两个变量：

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
AICO_BENCHMARK_TASK_ID=my-sandbox-session \
AICO_BRIDGE_EPISODE_ID=my-sandbox-session \
  docker compose up --build
```

当多实例桥接需要把请求识别为同一个逻辑沙箱会话时，请保持 task id 不变。
对于同时运行的每个沙箱会话，请使用不同的 id。

Agent 看门狗重启和 Config Web 保存都会保留已占用的桥接会话。更改桥接端点、task id 或 episode id
会先释放旧路由，再占用新路由。停止沙箱会释放当前路由；下一次完整启动会占用一个新的会话。

沙箱默认的设备类型是 `iOS`，这样在未配置桥接时，Agent Web 也能就绪，而无需等待不可用的 Android frame 服务。
只要目标不同，就将 `AICO_DEVICE_TYPE` 设置为与桥接平台一致。可接受的值为 `iOS`、`Android`、`macOS`、
`windows` 和 `linux`。如果没有传入运行时覆盖，Config Web 中的设备类型更改会在 `docker compose restart` 后生效。
如果传入了 `AICO_DEVICE_TYPE`，则以该环境变量的值为准：停止整个栈，然后使用更新后的值（或不带覆盖）
再次运行 `docker compose up`。

### MobileGym

在另一个终端中启动单环境的 MobileGym 桥接：

```bash
git submodule update --init benchmark/mobilegym/vendor/mobilegym
cd benchmark
uv sync
uv run python -m runner start-mobilegym-env --envs 1 --bridge-port 19090
```

在主机上验证桥接：

```bash
curl http://127.0.0.1:19090/health
```

然后使用以下命令启动根目录的 Compose 栈：

```bash
AICO_DEVICE_TYPE=Android \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:19090 \
  docker compose up --build
```

在 `http://localhost:8080` 上使用 Agent Web 给 Agent 下达任务，并观察它操作 MobileGym 界面。
沙箱会使用配置的 `AICO_BENCHMARK_TASK_ID`，通过 `/api/setup` 占用一条 MobileGym 路由；
桥接进程启动时会打印它自己的停止命令。

### 通过 ADB 连接 Android

首先确认模拟器、Android 虚拟机或实体设备对 ADB 可见：

```bash
adb devices
```

启动 ADB Android 桥接，并将序列号替换为 `adb devices` 显示的值：

```bash
cd benchmark
uv sync
uv run python -m runner start-adb-android-env \
  --adb-serial emulator-5554 \
  --bridge-port 8899
```

然后在仓库根目录下启动沙箱：

```bash
AICO_DEVICE_TYPE=Android \
AICO_LOCAL_PHONE_BRIDGE=1 \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:8899 \
  docker compose up --build
```

关于支持的工具、序列号选择和特定设备的限制，参见 [ADB Android 环境桥接](../../benchmark/adbandroid/README.md)。
关于配套 App 的单独网络要求，参见 [手机桥接](../04-agent/phone-bridge.md#使用-adb-reverse-的桌面-agent)。

### 其他虚拟机或目标

沙箱可以连接任何实现了 [Environment Bridge 协议](../../benchmark/environment_bridge.md) 的主机端服务。
为虚拟机或 UI 目标启动桥接，在主机上暴露其 HTTP 端口，然后设置：

```bash
AICO_DEVICE_TYPE=<device-type> \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:<bridge-port> \
  docker compose up --build
```

环境桥接是控制边界：沙箱不会仅仅因为某个虚拟机正在运行，就获得对它的直接访问权限。
对于本项目的虚拟 iPhone 工作流，请使用 `AICO_DEVICE_TYPE=iOS`，并参见
[vphone CLI 设置指南](../09-benchmark/vphone-cli-setup-guide-en.md)。

### 通过 macOS iPhone 镜像连接真实 iPhone

实验性的 [iPhone 镜像桥接](../../benchmark/iphone_mirroring/README.md) 让在 Mac 上原生运行的 Agent
可以使用已经连接到 Apple iPhone 镜像 App 的真实 iPhone。它会截取手机窗口，并把 Agent 的坐标映射到该窗口。
在 `benchmark/` 下使用 `python3 -m iphone_mirroring` 启动它，然后使用其 README 中的原生 Agent 参数和专用的快捷操作映射。
该桥接绑定在 Mac 的回环地址上；文档中的工作流是在 Mac 上运行 Agent，而不是在 Docker 沙箱中。

### 主机桌面

若要让 Agent 操作运行基准测试的桌面会话，在 `benchmark/` 下启动主机桌面桥接：

```bash
uv run python -m runner start-desktop-env --bridge-port 8898
```

然后将沙箱指向报告的端点：

```bash
AICO_DEVICE_TYPE=macOS \
AICO_ENVIRONMENT_BRIDGE_ENDPOINT=http://host.docker.internal:8898 \
  docker compose up --build
```

该桥接使用 `pyautogui` 进行指针和键盘输入，所有指针操作都使用归一化的 0–1000 坐标。
使用 `uv pip install pyautogui` 安装它，并在 macOS 上授予屏幕录制/辅助功能权限
（或在 Linux/Windows 上授予相应的桌面会话权限）。

## 停止或重置沙箱

如果在前台运行，按 `Ctrl-C`。然后删除已停止的容器和网络，同时保留命名卷：

```bash
docker compose down
```

如需删除 Compose 命名卷中已保存的配置以及所有其他 Agent 状态，请同时删除卷：

```bash
docker compose down -v
```

`-v` 重置具有破坏性。下一次 `docker compose up --build` 会使用全新的沙箱数据启动。

## 硬件能力边界

沙箱运行的是软件服务；它不会模拟 Aico Link 开发板或其外设。具体而言，它不模拟：

- 开发板的 Wi-Fi 扫描、热点设置、USB 网络或 Wi-Fi 连接管理；
- OTA、bootloader、分区、固件烧录或回滚行为；
- HDMI 转 CSI 屏幕采集、`/dev/video0` 或帧采集硬件路径；
- Linux USB gadget 设备以及通过 `/dev/hidg*` 的真实 USB HID 输出；
- 板载麦克风、扬声器、codec、硬件 VAD 或开发板音频服务；
- 开发板 BLE 以及其他开发板专属的外设服务。

共享的配置页面中仍可能出现与硬件相关的控件，但这并不能证明对应的硬件路径在 Docker 中可以工作。
环境桥接只是为 Agent 测试替代一个屏幕和输入目标；它不会模拟 Wi-Fi、OTA、USB HID、音频或其他开发板行为。

验证这些能力时，请使用实体开发板以及 [新手快速上手](quickstart.md)。

## 故障排查

### 网页打不开

检查服务状态和日志：

```bash
docker compose ps
docker compose logs -f
```

同时确认端口 `8000` 和 `8080` 没有被占用。

### Agent 无法访问环境桥接

1. 确认在主机上可以访问桥接的 `/health` 端点。
2. 确认启动沙箱时设置了 `AICO_ENVIRONMENT_BRIDGE_ENDPOINT`。
3. 确认端点使用的是 `host.docker.internal`，而不是 `localhost`。
4. 在桥接输出和 `docker compose logs -f` 中查找失败的请求。

### Agent Web 可以打开，但模型请求失败

打开 Config Web，核对提供商、模型名称、API key 以及可选的 base URL。沙箱不会自动提供模型凭据。

## 后续步骤

- [Benchmark 概览](../09-benchmark/README.md)：可重复的测试套件、并行环境、评分和报告；
- [Agent 配置](../04-agent/configuration.md)：配置字段；
- [硬件与接线](hardware.md)：迁移到实体开发板时参考。

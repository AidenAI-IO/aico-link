---
sidebar_position: 2
---

# Config Web：基于 Web 的配置界面

Config Web 是内置的浏览器客户端，由 Go Agent 二进制文件（`/usr/lib/aico/agent`）的
`config-web` 子命令提供服务。设备操作由可独立挂载的[设备管理 API](device-management-api.md)
对外提供，因此可以替换或移除该页面，而不会把这些操作与静态 UI 耦合在一起。配置变更会持久化到
`/userdata/agent/agent.toml` 并在线应用。页面会显示设置处于待应用、已应用、失败还是等待设备重启状态。

模型发现、STT 配置测试、存储操作以及所有其他设备管理调用都直接由 Config Web 在 `80`
端口提供服务。页面不会拼接跨端口的 Agent URL，因此 Docker 主机端口覆盖和 USB-ECM
访问的 API 行为完全一致。

## 前端开发

前端使用 React、TypeScript、Jotai 和 Vite。在仓库根目录下运行
`bash scripts/build-config-web.sh` 构建静态资源，然后用 `agent config-web --web-root`
提供 `src/config_web/dist`。Docker 构建会固定 Node 版本并按 npm lockfile 安装依赖；
设备上不需要任何前端运行时。Debian 打包和开发启动脚本会自动构建这些资源。
本地开发、格式化、校验以及保留的编辑器兼容边界，参见[前端指南](../../src/config_web/README.md)。

外壳、导航和实时视图共用同一个共享 store。可见的设备标签页每三秒观测一次存储，
每两秒观测一次配置应用和 OTA，每十秒观测一次固件。Agent 进程状态只在需要它的概览、
Agent 日志和容器语音路由中读取。USB Internet、LAN/密码和已安装 MCP 的状态在各自路由上每五秒刷新一次。
OTA 空闲时仍会进行轻量轮询，因此能检测到外部发起的更新。完整日志只在固件和 OTA 日志路由上轮询。
标签页隐藏时所有轮询都会暂停，并在返回页面或浏览器网络恢复时刷新。保存操作会使更早的读取失效；
即使 Agent 离线或尚未应用外部编辑，持久化文件的版本变化也会刷新配置摘要。保存还会刷新规范的校验错误。
静态 schema 在各编辑器之间共享，本地 Agent 端点发现结果最多共享三十秒，连接失败后会重新发现。
轮询会保留已打开的对话框、获得焦点的控件和编辑器草稿；观测时间戳不会使 UI 视图失效。

## 默认参数

| 参数 | 默认值 |
| --- | --- |
| 端口 | `80` |
| HTTPS 端口 | 关闭；随附的服务单元设置为 `443` |
| TLS 目录 | 无；随附的服务单元设置为 `/userdata/agent/tls` |
| 配置 | `/userdata/agent/agent.toml` |
| 二进制文件 | `/usr/lib/aico/agent config-web` |
| Web 根目录 | `/usr/share/aico/config-web` |

Config Web 同时在 `443` 端口通过 HTTPS 提供服务，使用开发板本地证书颁发机构签发的证书，
Agent Web UI 在 `8443` 端口共用该证书。为兼容起见，`80` 端口仍然可用。证书的签发与信任方式参见
[浏览器音频所需的 HTTPS](../05-agent/realtime-voice.md#浏览器音频的-https)。

## 启动

```bash
systemctl start aico-config-web.service
systemctl stop aico-config-web.service
systemctl restart aico-config-web.service
systemctl status aico-config-web.service
```

该服务通过 Debian systemd preset 启用，也会被 `aico.target` 拉起。

Config Web 将 Agent 配置视为一种恢复资源。一个可读取、可解析但包含无效值的 `agent.toml`
不会阻止 80 端口启动：页面会报告配置无效，高亮出问题的字段，并保持可编辑。只有当配置文件本身无法读取或解码时
（例如权限错误、TOML 文档损坏或文件目标无效），启动才会失败。

### PC 配置档

原生主机上的 Agent 使用 `config-web --local-agent`，它绑定到回环地址，提供模型提供商、对话、搜索、
MCP 服务器、语言、时区和日志设置，而不初始化开发板服务。Launcher 容器使用 `--container-agent`，
额外提供 Realtime 和 Classic 两种语音提供商设置、屏幕/通知记忆保留、记忆重置以及 Agent 支持日志导出。
共享的语音页面保留 STT/TTS 凭据、模型和 Extra 设置。语音标签页只是在前端内存中选择一个配置视图，
不会规划、保存或应用模式变更。该选择在子页面导航和后台观测中保持不变；重新加载页面会恢复已保存的设备模式。
显式选择提供商，或保存所选提供商的凭据，可以通过后端的计划和保存校验来应用预览中的模式。
Realtime 需要已保存的语音凭据；Classic 需要同时选择 STT 和 TTS 提供商。
提供商选择不完整时可以保存，但不会启用对应模式。开发板和 PC 配置档在“所选标签页”与“已保存设备模式”的区分上行为一致。
设备音频、开发板麦克风说话人注册、Wi-Fi、GPIO、存储维护、固件和系统服务控制在 PC 配置档的页面和 API 中仍不可用。
完整 TOML 编辑和备份/恢复也不在 PC 设置配置档之内；需要这些操作时，请在外部编辑实例的持久化配置文件。

Launcher 为其 Agent 容器设置 `AICO_CONFIG_WEB_PROFILE=container`。镜像入口点据此选择配置档，
并监听容器的 80 端口；Launcher 只将其发布到主机回环地址上。概览页中的 Agent 链接使用对外发布的
Agent Web 端口，而不是容器内部地址。容器终端链接使用同一端口，且只有在 Launcher 启用了 Web 终端时才会出现。
支持日志导出包含 Agent、模型请求和 episode 日志，但不收集开发板服务日志或内核诊断信息。
默认镜像在启动时禁用 Classic 语音（`AICO_CLASSIC_VOICE=0`），因为它缺少 VAD 辅助程序/模型。
语音页面会报告这一限制：STT/TTS 配置仍可编辑，但在提供运行时依赖并在启动时启用 Classic 语音之前，
Classic 只能进行文字交互。两种配置档都要求浏览器请求同源且使用回环主机名，并通过现有的配置更新和在线应用流程保存。
镜像默认的 Compose 配置档仍提供开发板界面。参见[在 PC 上试用 Aico](../01-getting-started/try-aico-on-pc.md)。

## 访问

通过 USB-C 将设备连接到电脑。设备会建立一个地址为 `192.168.42.1` 的 USB 网络。访问：

```text
http://192.168.42.1
```

在 Web 界面中你可以：
- 打开由 ttyd 提供的浏览器终端：`http://192.168.42.1:3000/webtty/`
- 在 Advanced → LAN access 下开启或关闭 LAN 访问，并在 Advanced → Device password 下设置、修改或清除设备密码；两者相互独立（参见 [LAN 访问](./device-management-api.md#lan-访问)）
- 在简体中文（`zh-CN`）和英文（`en-US`）之间切换设备语言；这也决定面向用户的 Agent 回复和 `<tts>` 内容的语言
- 在已注册的模型提供商类型之间切换，例如 OpenAI、Anthropic、OpenRouter、Kimi、Volcengine 和 Ollama；Google Gemini 模型可通过 OpenRouter 等兼容提供商使用
- 配置 API key 和模型名称
- 选择 STT/TTS 提供商
- 测试语音识别和语音合成
- 在线应用已保存的设置、重试失败项，并且只在 USB 设置需要时才显式重启
- 只要运行中的 Agent 支持该变更，就在线应用已保存的设置；文档中列出的需要重启的例外情况会在 UI 中明确提示
- 为每个 Wi-Fi 网络保存系统默认、直连或自定义代理策略；自定义代理凭据永远不会返回给浏览器

Wi-Fi 对话框接受 `http://`、`https://` 和 `socks5://` 代理 URL。当对应 SSID 成为当前网络时，
已保存的策略会自动激活。Agent、OTA 命令、受管子进程和登录 shell 始终使用固定的本地地址
`127.0.0.1:18080`。该地址同时接受 HTTP 和 SOCKS5，本地 URL 的协议与所选上游的协议一致。
在 HTTP 和 SOCKS5 之间切换会重启 Agent，使其长连接客户端使用匹配的协议；在同类型代理之间切换则不会。
本地 SOCKS5 URL 以 `socks5h://` 形式呈现，以便通过代理解析目标主机名；这并不改变 SOCKS5 的线路协议。

Wi-Fi 对话框中的 `NO_PROXY` 只属于该 SSID 的自定义代理。选择 `Use system default` 时，
上游代理和 `NO_PROXY` 都来自 `/userdata/system/env`；不会存储任何 Wi-Fi 专属的 `NO_PROXY` 值，
也不会将其合并到系统设置中。

Config Web 可见时，会在上一次读取完成后每秒通过轻量的 `GET /api/network/wifi/status` 端点刷新 Wi-Fi 状态。
设置首页、导航栏、Wi-Fi 列表和网络详情共享这些观测结果。连接和忽略网络的结果会立即更新页面，
新关联的网络会在其配置保存之前就显示出来。后台标签页会暂停轮询，并在返回页面或浏览器网络恢复时立即刷新。
读取失败会清除连接徽标，并将摘要标记为未知，直到有新的观测结果到达。状态更新会保留键盘焦点、
已打开的加入对话框以及未保存的代理编辑。已连接的 SSID 在其配置保存之前，详情是只读的；
保存后，已打开的详情页会启用配置操作并采用已保存的设置，而不会覆盖代理草稿。

页眉中的切换开关只通过 `PUT /api/config/locale` 保存顶层的 `locale`。UI 会立即更新，
如果持久化失败则回滚。最近一次确认的值会缓存在 `localStorage` 中用于首次绘制，但 `GET /api/config`
始终是权威来源。当与语言相关的系统提示词发生变化时，下一个 Agent 任务会创建新的上下文会话；
它不会改写之前的会话。`locale` 被有意与 `[voice_settings.classic.stt].language` 分开，后者只用于配置语音识别。

## 配置应用策略

| 本实现所变更的类别 | 应用边界 |
| --- | --- |
| 普通设置：语言/提示词、迭代和上下文限制、截图保留、遥测、日志保留、通知、实时活动、快速捕获 TTL | 在线发布，供后续任务/操作使用；共享管理器保持其身份不变 |
| 模型设置：普通的模型/提供商、凭据、端点和模型选项 | 等待当前 Agent 任务结束后替换模型客户端；即使语音会话仍保持打开，后续任务也会使用新设置 |
| 组件：输入模式、STT/TTS/实时语音及其提供商凭据、音频/VAD、HID 客户端、采集后端、搜索、快速捕获 GPIO、存储策略 | 排空受影响的工作，准备替换组件，并在不重启 Agent 的情况下切换组件；HTTP 和手机桥接保持在线 |
| `frame_service.keep_streamon` | 只重启 `frame_service`，等待其监听 socket 就绪后再排队 Agent 快照；采集会短暂不可用 |

现有的例外情况仍然存在：Android/非 Android 的 USB 指针描述符变更以及 `hid.keyboard_layout`
需要重启设备。其他设备类型的变更可以在线应用。保存系统环境变量时会显示 **Apply and restart Agent**；
只有这个显式操作才会重启 Agent。部署新的二进制文件仍需要重启 Agent 和 Config Web；这与修改运行时设置是两回事。

语音组件变更会等待当前语音会话结束。普通的模型变更不会重启或排空语音循环。替换失败时会保留之前的运行时配置和已排空的对话。
必须先释放正在按住的拖拽，HID 组件替换才能成功。旧的模型请求和 TTS 会话在完成之前会继续使用其当前提供商。
提示词/提供商变更会在下一个任务/会话边界开启新的上下文会话，并保留现有的对话记录文件不变。

即使运行时应用失败，编辑器也会显示已持久化的配置。提供商记录和引用仍会保存；应用状态会报告错误，
**Retry** 会重新应用完整的已保存配置。因此，较早一次失败的语音模式变更也可能阻塞之后的模型保存，
直到修复语音依赖或撤销那次已保存的变更为止。保存某个部分时会保留其他字段中未保存的草稿。
手动 TOML 编辑和配置导入与表单编辑及提供商编辑使用相同的保存队列和应用状态。

USB 重启要求在仅重启 Agent 时依然有效：已绑定的 USB 描述符和 `<config-dir>/cache/usb-boot.json`
会为当前 Linux boot ID 保留生效中的设置。真正重启设备后，会采用已保存的 USB 标识和布局。
快速捕获 GPIO 替换失败时会保留当前生效的配置，因此在修复硬件故障后可以重试同一份已保存的配置。

---
sidebar_position: 17
---

# 实时语音模块

实时语音是 Agent 在 `input_mode = "realtime"` 下的全双工音频路径。
守护进程在一个实时会话中保持麦克风和扬声器开启，由所选提供商负责轮次检测、
语音生成、转写以及（在支持时）工具调用。

本页介绍仓库中的实现。提供商的线路协议细节单独记录在
[Qwen Realtime API 参考](qwen-realtime/websocket-api.md)中；
不得将适配器包视为与 OpenAI 兼容的线路协议。

## 职责与边界

### 输入转写

实时音频理解和面向用户的转写是同一个提供商会话的两路独立输出。提供商直接使用
麦克风音频生成回复；最终的用户转写文本用于 Web 历史记录，不会作为第二条文本提示词回传。

OpenAI 在 GA 和旧版会话载荷中都请求 `gpt-4o-mini-transcribe`。
Qwen、xAI 和 Gemini 使用各自提供商原生的转写路径。普通的 `[stt]` 配置只适用于
`input_mode = "stt"`，不会配置实时会话。已配置的 Agent locale 通过会话指令控制
实时回复的语言；它不会设置或限制提供商的输入转写语言。

OpenAI 诊断信息会记录请求的和已确认的转写模型、语音开始/结束、音频提交、
最终转写或转写失败、回复 ID 以及工具调用 ID。提供商可能省略或重排这些事件，
因此诊断信息不会改变回复准入，也不会再次提交 server-VAD 缓冲区。最终转写文本属于
用户数据，必须相应地妥善处理。

实时路径分为三层：

```text
daemon/realtime_wakeup.go
  session lifecycle, audio capture/playback, history, task updates,
  notifications, tool execution, and chat bridge
              │
              ▼
internal/agent/realtimevoice
  provider registry, normalized events, optional capabilities,
  device/provider media negotiation, and managed resampling
              │
              ▼
  qwen.go   openai.go   xai.go   gemini.go   speko.go
  native provider sessions       mint credential, then delegate to
                                  Gemini Live or xAI
```

`realtimevoice.Provider` 和 `Session` 只暴露守护进程需要的语义：
PCM 上传、归一化事件、会话信息以及关闭/错误流。
认证、端点构造、JSON 帧、事件名称和原生音频格式都留在各个适配器内部。

`ProviderRegistry` 将提供商名称映射到工厂。`ProviderRegistry.Open`
随后将原始会话包装为托管会话，该托管会话会：

- 协商面向设备的输入和输出 `AudioFormat`；
- 当设备和提供商的采样率不同时，对单声道 PCM16 进行重采样；
- 转发归一化事件，而不暴露提供商的 JSON；
- 当 Gemini 声明运行时采样率时刷新输出重采样器；以及
- 根据实际的会话实现报告可选能力。

`Conversation` 包装器让可选操作保持显式。调用方必须对这些接口进行特性检测，
而不是假定每个提供商都支持它们：

| 能力 | 用途 |
| --- | --- |
| `TurnCommitter` | 在提供商要求时发送客户端侧的轮次结束标记 |
| `ResponseInterrupter` | 在插话打断时取消/截断回复 |
| `ToolResultSender` | 在实时连接上返回工具结果 |
| `TextSession` | 注入文本并显式请求回复 |
| `ContextReplayer` | 重连后恢复最近的用户上下文 |

## 已内置的适配器

| 适配器 | 传输方式 | 说明 |
| --- | --- | --- |
| Qwen | JSON WebSocket | DashScope 实时协议；服务端 VAD/smart-turn、音频转写、工具以及回复取消 |
| OpenAI | JSON WebSocket | 原生 OpenAI Realtime 事件；显式启用输入转写，回复打断包含截断 |
| xAI | JSON WebSocket | xAI 实时事件别名被归一化为共享的事件模型 |
| Gemini | Gemini Live WebSocket | `setup`/`serverContent` 协议；输入/输出转写和函数响应；服务端 VAD 不提供 Aico 语音边界事件 |
| Speko S2S | HTTP 签发 + 委托给提供商的 WebSocket | 签发一个短期有效的提供商凭据，然后使用所选的 Gemini 或 xAI 适配器；有意拒绝 OpenAI/WebRTC 路由 |
| Moss | JSON + 二进制 WebSocket | MOSS-LIVE（`moss-live-1.0`）；16 kHz PCM 上行 / 48 kHz PCM 下行；立即确认播放数据块；客户端工具通过 `attach_session` 的 `delegation: {"type": "client"}` 提供；同步工具调用会保持本次运行不结束，在 `conversation.item.created` 确认每个 `function_call_output` 之后，适配器用指明这些调用 ID 的 `response.create` 继续运行（`response.done` 是调用摘要，不是终止事件）；`web_search` 等保留名称会以 `aico_` 别名导出；客户端工具最多 32 个；仅支持语音（暂不支持 `/api/chat` 文本注入、不可配置 `voice`、暂无摄像头/屏幕帧）。运行时会忽略 `use_backend_agent`（实际生效值为 `false`，存储的值不变；Config Web 会隐藏该开关）。 |

Speko 是一个控制面适配器，而不是另一个 PCM 中继。它的租约包装器会在过期前发出
`ErrSessionRotated`，使守护进程可以重连并重放持久化的实时上下文。

## 事件与历史生命周期

守护进程消费与提供商无关的 `Event` 值。一个典型的音频轮次如下：

```text
audio → speech_started → speech_stopped → input_committed → transcript_final
      → response_started → transcript_delta/audio/tool_call
      → response_done or response_cancelled
```

提供商可能省略或重排这个序列中的部分事件。例如 Gemini 不发出 Aico 语音边界事件，
因此守护进程使用本地能量门限，仅用于防止用户说话时插入后台任务更新。
它并不替代 Gemini 服务端的轮次检测器。

用户的最终转写一到达就会追加到实时用户上下文中。助手的文本/转写只有在回复完成
且未被抑制时才会持久化。工具调用由支持后台执行的实时工具执行器执行；工具结果通过
`ToolResultSender` 回传，对于需要显式继续的提供商，会在该回复的所有结果都到达后
发送一个新的 `CreateResponse`。

返回视觉观察结果的工具（例如 `screenshot` 或操作后截图）绝不会把图片以 base64 文本
的形式发给模型或写入用户上下文。实时用户上下文的存储方式与后端循环相同：一条不含图片
的工具结果，后面跟一条 `state` 消息，其中以附件形式包含图片及其说明（截图尺寸、
屏幕变化说明、UI grounding）。提供商实际收到的内容取决于模型：

- 支持在对话中接收图片的模型，会收到带说明图片的后端工具结果。该列表维护在
  `realtimevoice.SupportsImageInput` 中；包括 `gpt-realtime` 系列
  （OpenAI Realtime）和 `gemini-3.8-live` 模型。OpenAI 会在 `function_call_output`
  之后以 `input_image` 用户消息的形式收到图片；Gemini 会以函数响应中的 `inlineData`
  部分收到图片，说明则放在响应中。
- 其他模型，包括默认的 Gemini 3.1 Flash Live、Qwen、xAI 和
  MOSS Live，会在工具结果中收到包裹在 `<state>` 中的说明，并用
  `[Image omitted: ...]` 占位符代替图片。它们的协议只能以流式视频帧或输入缓冲区的
  形式接收图片，或者根本不接收图片。

重连后的上下文重放会把每条 `state` 消息恢复到其工具结果中。支持图片的模型会再次
收到最新的一张图片；更早的图片，以及其他模型的所有图片，都会被占位符替代。

在同一个回复中，语音和工具或后台任务的执行可能重叠，因此实时指令要求一道结果声明
门槛：在能体现实际结果的结果可用之前，模型只能确认自己正在处理请求；之后必须依据该
证据报告成功或失败。只表明工作已被接受、已排队或已开始的工具结果不算数。工具错误则
算作启动或更新工作失败，因此模型必须报告失败，而不是继续说自己正在处理。在后端模式下，
成功的 `create_agent_task` 结果只是一个确认，因此已接受工作的结果来自状态为
`completed` 或 `failed` 的任务更新或 `query_agent_task` 结果。取消是另一回事：
`cancelling` 表示停止仍在进行中，只有 `cancelled` 才确认工作已停止；该状态既不是原
工作的成功也不是失败。不相关的请求必须启动自己的任务，而不是并入已有工作。直接工具模式
（`use_backend_agent = false`）会对 `open_app` 等运行时工具应用这道门槛，直到它们返回。

任务更新和语音通知只在会话空闲时注入。通知回复是私有语音回复：它们通过扬声器播放，
并且只在播放排空后才被确认，但不会加入用户可见的对话历史。

## 音频路由与 Web UI 语音模式

实时会话从一个 `RealtimeAudioIO` 端点
（`src/agent/internal/agent/realtime_voice.go`）获取音频：麦克风使用一个录音后端，
回复使用一个播放后端，格式均与提供商协商。对话、工具、转写和历史对每个端点都相同；
只有音频路径不同。

| 路由 | 发起方式 | 麦克风与扬声器 |
| --- | --- | --- |
| `device` | 唤醒词、MCU 唤醒或任务更新 | 通过 `audio_service`（或本地主机后端）使用开发板音频 |
| `web` | Web UI 中的语音按钮 | 按下按钮的浏览器，通过 `/api/realtime/voice/ws` |
| `text` | 没有对话在运行时的一条 `/api/chat` 消息 | 无；向提供商请求文本回复 |

对于 `device` 和 `web` 路由，Web UI 显示相同的语音模式：输入框上方的光球、
麦克风和扬声器静音按钮，以及一个结束按钮。会话期间在输入框中键入的文本会像以前一样
注入到实时会话中。只有发起 `web` 会话的标签页会采集和播放音频；其他标签页，以及
`device` 会话期间的所有标签页，都只做显示。

`text` 会话不是语音模式。它不打开麦克风或扬声器，Web UI 不显示光球，
`GET /api/realtime/voice` 报告 `idle`。其回复以文本形式流式返回给聊天请求。
会话期间完成的后台任务结果会以文本形式投递到该会话中；需要播报的语音通知会保持待处理，
等待语音会话或独立的语音路径。

同一时间只运行一个对话。在 `device` 或 `web` 对话期间，唤醒词或按键会被忽略，
因此 `web` 对话结束时不会打开开发板麦克风。在 `text` 会话期间，唤醒词、按键或
Web UI 语音启动会结束该会话，并改为打开所请求的语音路由；对话历史会延续下来。
当提供商限制了会话时长时，守护进程会重连，对话在同一路由上以相同的静音状态继续。

Classic（`input_mode = "stt"`）会话使用相同的 Web UI 语音模式和端点，但只有
`device` 和 `web` 两种路由；Classic 会话在各路由上的行为请参见
[Web UI 语音模式](audio-features.md#web-ui-语音模式)。

Web UI 使用以下端点：

- `GET /api/realtime/voice` 返回 `available`（实时或 Classic 持续语音循环正在运行）、
  `state`（`idle`、`connecting` 或 `active`）、`route`、`muted`、
  `speaker_muted` 以及 `https_port`（Agent 正在提供服务时的 HTTPS 端口，用于把普通
  HTTP 页面重新打开到允许使用麦克风的地址）。
  已被要求停止的对话会立即报告 `idle`，即使守护进程仍在关闭它。
- `POST /api/realtime/voice/stop` 结束 `device` 或 `web` 会话，并让守护进程回到待机。
  它不会结束 `text` 会话。
- `POST /api/realtime/voice/mute` 接受 `{"muted": bool}`、
  `{"speaker_muted": bool}` 或两者同时提供。麦克风静音时发送静音，因此提供商持续收到
  连续的音频流，其轮次检测看到的是静音而不是停顿。扬声器静音时用静音替代回复播放，
  并丢弃已排队等待播放的语音；转写、工具和历史照常继续。扬声器静音期间本应播报的通知
  会保持待处理。
- `GET /api/realtime/voice/ws` 启动 `web` 会话。二进制帧在两个方向上都携带有符号
  16 位小端 PCM。Agent 发送 JSON 控制消息：`capture_start` 和 `playback_start`
  携带 PCM 格式，`capture_stop` 结束麦克风采集，`playback_stop` 在插话打断后丢弃
  排队中的语音，`ended` 携带会话结束的原因。关闭 socket 会结束会话。当另一个语音会话
  正在连接或处于活跃状态时，该端点会拒绝启动；正在运行的 `text` 会话会被结束，
  以便语音会话启动。

守护进程从不等待浏览器：回复音频会排队并在后台写入 socket。落后约 40 秒语音的浏览器
会被断开，会话随之结束。Agent 日志会记录浏览器语音 socket 关闭的原因。

浏览器回声消除不保证能去除通过 Web Audio 播放的语音，因此如果回复打断了自己，请使用耳机。
失败的 `web` 会话会在 Web UI 中报告错误，不会播放设备上的失败播报。

### 浏览器音频的 HTTPS

浏览器只向安全上下文开放麦克风，即 HTTPS 或 `localhost`。因此开发板的 Web 服务
同时监听 HTTP 和 HTTPS：

| 服务 | HTTP | HTTPS |
| --- | --- | --- |
| Config Web | `http://192.168.42.1`（80） | `https://192.168.42.1`（443） |
| Agent Web UI | `http://192.168.42.1:8080` | `https://192.168.42.1:8443` |

开发板的 Wi-Fi 地址使用相同的端口。HTTP 端口为兼容而保留。每个 HTTPS 端口遵循与
其 HTTP 端口相同的局域网访问规则，经由 USB 链路的请求在两者上都受信任。Agent 通过
`-https-addr` 启用 HTTPS，Config Web 通过 `--https-port` 和 `--tls-dir` 启用；
随固件发布的 systemd 单元已设置好这些参数。

开发板没有公网 DNS 名称，因此无法使用公开受信任的证书。开发板改为在
`/userdata/agent/tls/` 中保存一个本地证书颁发机构（`ca.pem` 和私有的 `ca-key.pem`），
由两个服务共享。先启动的服务负责创建它。每个服务会为客户端所连接的地址签发证书，
例如 `192.168.42.1`、Wi-Fi 地址或 `localhost`，并保存在 `certs/` 下，使其在重启
之间保持不变，直到临近过期。该颁发机构受名称约束，仅限回环、私有和链路本地地址以及
`localhost`，因此信任它不会让浏览器接受某个公网站点的证书。它保存在用户数据中，
因此在固件升级后依然保留。

使用浏览器语音：

1. 打开 `https://192.168.42.1:8443`（或开发板 Wi-Fi 地址的 8443 端口）。
   在普通 HTTP 页面上按下语音按钮会自动打开这个地址；通过 Wi-Fi 访问时，浏览器可能会
   在那里再次要求输入设备密码，因为它把 HTTPS 页面视为不同的站点。
2. 针对这个地址接受浏览器的证书警告。不需要安装颁发机构：接受警告后，页面仍然是安全
   上下文。每个地址都是一个独立站点，因此 USB 地址和 Wi-Fi 地址需要分别确认，
   有些浏览器在一段时间后还会再次询问。
3. 可选：将该颁发机构安装为受信任的根证书，以在该设备上消除警告。通过 USB 链路从
   `/tls/ca.pem` 下载，例如 `http://192.168.42.1/tls/ca.pem`。通过 Wi-Fi 下载会被
   拒绝，即使已经登录也是如此：安装根证书是一项信任决策，而局域网上的普通 HTTP 响应
   可能已被替换。在 macOS 上，用“钥匙串访问”打开该文件并将其设为“始终信任”。
   Firefox 有自己的证书存储，因此也需要在其中导入。

证书有效期以开发板的时钟为准。固件设置了一个接近真实时间的默认系统时间，因此在时钟
同步之前签发的证书仍会被浏览器接受。

要更换颁发机构，请删除 `/userdata/agent/tls/` 并同时重启 `aico-agent` 和
`aico-config-web`；只重启其中一个会让两个服务使用不同的颁发机构。之后，信任过旧
颁发机构的浏览器会再次显示警告。

## 配置

使用 `[voice_settings.mode].input_mode = "realtime"` 并选择一个命名的提供商记录：

```toml
[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime.providers.qwen-main]
type = "qwen"
api_key = "$DASHSCOPE_API_KEY"
model = "qwen-audio-3.0-realtime-plus"
voice = "longanqian"

[voice_settings.realtime]
provider = "qwen-main"
```

提供商专用的凭据和路由字段属于
`[voice_settings.realtime.providers.<name>]`；`[voice_settings.realtime]` 包含所选记录
以及指令、轮次检测等会话级选项。所有字段及提供商可见性规则请参见
[Agent 配置参考](configuration.md#voice_settingsrealtime)。

## 测试与扩展点

提供商转换器和线路帧在 `src/agent/internal/agent/realtimevoice` 下有针对性测试。
托管会话测试覆盖媒体转换、事件转发和能力检测。守护进程测试覆盖配置、事件循环准入、
播放和通知行为。

添加一个提供商：

1. 在 `realtimevoice` 中实现 `Provider.Open` 和一个原始 `Session`。
2. 将原生事件归一化为 `Event` 值，并让原生载荷类型保持为适配器私有。
3. 只实现该提供商实际支持的可选能力接口。
4. 在 `DefaultProviderRegistry` 中注册工厂，并将其描述符添加到共享的提供商元数据中。
5. 在 Config Web 中启用该提供商之前，添加协议重放测试。

旧的 `rtclient` 包曾是 Qwen/OpenAI 专用的传输抽象层。它已不再是运行时依赖；
新代码应放在 `realtimevoice` 中，提供商特定的协议行为保留在对应的适配器里。

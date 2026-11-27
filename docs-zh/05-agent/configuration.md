---
sidebar_position: 2
---

# Agent 配置参考

Agent 守护进程接受 `-dir` 参数，即它工作所用的数据目录。`agent.toml` 只是该目录中的内容之一：Skill、记忆、缓存和日志都相对于它解析（参见 [目录布局](#目录布局)）。`config`、`config-check` 和 `config-test` 子命令接受 `-config` 参数，指定 TOML 配置文件的路径。下面的所有字段都位于 `agent.toml` 中。大多数字段都可以通过设备上的 [Config Web 页面](#config-web设备配置页面) 编辑；没有专用控件的部分会被 Config Web 原样保留，可以手动编辑。TOML 是唯一受支持的配置格式；JSON 配置已弃用。

`config-check --config <path>` 会应用严格校验器，它是镜像构建和发布工作流运行的关卡，因此运行时在启动时会悄悄恢复的配置，在这里仍会失败——例如持久化了 `input_mode = "realtime"` 但其凭据已被删除，此时 Agent 会回退到文本模式而不是拒绝启动。Config Web 恢复页面有意给出不同的判定：它使用运行时加载器，因为它需要描述 Agent 实际启动时的状态，并指出需要修复的字段。用户即将保存的候选值由 `config-check --stdin` 进行严格校验。

守护进程还接受 `-device-type <value>`，作为 `[basic_settings.device].device_type` 的进程内覆盖。该覆盖在加载 `agent.toml` 之后应用，因此命令行的值优先级更高，并且不会改写配置文件。可接受的值和别名会被规范化为下文 `[basic_settings.device]` 中记录的标准值。

## 配置分组

`agent.toml` 使用与 Agent 设置页面相同的、面向产品的层级结构。运行时代码在加载边界处将这些分组表映射到其内部结构；Config Web 只写入下面这些分组路径。

1. **Wi-Fi 和蓝牙设置**：通过 Config Web 配置，不在 `agent.toml` 中
   - WiFi 配置：网络凭据、代理设置
   - 蓝牙配置：由独立的蓝牙服务管理

2. **基本设置**：
   - **语言和时区**：`[basic_settings.language_timezone]`
   - **设备设置**：`[basic_settings.device]`，包括 `[basic_settings.device.hid].keyboard_layout`

3. **对话设置**：
   - Agent 提示词：`prompt`
   - 最大迭代次数：`max_iterations`
   - 上下文管理：`context_prune_threshold`、`context_prune_state_limit`、`context_compaction_threshold`
   - 工具设置：`[conversation_settings.termination_policy]`、网页搜索 `[conversation_settings.search]`

4. **模型设置**：
   - 提供商配置：`[model_settings.providers.<name>]`
   - 模型选择：`[model_settings.model]`，包括 provider、model、api_key、temperature、max_response_tokens、context_window、reasoning_effort 和 api_mode 设置
   - 可选的决策模型：`[model_settings.decision]`

5. **语音设置**：
   - 模式选择：`[voice_settings.mode].input_mode`（`stt` 表示 Classic，`realtime` 表示 Realtime）
   - **Realtime 模式**：`[voice_settings.realtime.providers.<name>]`、`[voice_settings.realtime]`
   - **Classic 模式**：`[voice_settings.classic.runtime]`、`[voice_settings.classic.stt]`、`[voice_settings.classic.stt.providers.<name>]`、`[voice_settings.classic.tts]`、`[voice_settings.classic.tts.providers.<name>]`、`[voice_settings.classic.audio]`、`[voice_settings.classic.audio_archive]`

6. **记忆设置**：
   - 屏幕记忆保留时长：`[memory_settings.screen].screen_memory_ttl`
   - 通知记忆：`[memory_settings.notification]` 及过期策略
   - 重置对话：通过 Config Web UI 处理

7. **存储设置**：
   - 存储状态：通过 Config Web 显示（总空间/可用空间）
   - microSD 设置：`[storage_settings.storage]` 配置，以及格式化/弹出操作
   - 备份与恢复：通过 Config Web 导出或导入标准的分组 `agent.toml`
   - 内部存储策略：`[storage_settings.storage.degraded_mode]`、`[storage_settings.storage.cleanup]`

8. **高级设置**：
   - 日志：详细的模型请求捕获、Agent 日志级别、保留时长以及支持日志导出
   - 手动编辑配置：针对标准分组 `agent.toml` 的带校验的原始编辑器
   - MCP：`[conversation_settings.agent].mcp_enabled`（高级页面上的开关）；参见 [MCP](mcp.md)

   硬件和运行时调试表仍可在 TOML 中使用，但不会作为产品设置在 Config Web 中暴露。

9. **关于**：
   - 固件版本：从 OTA 状态读取；在第一次 OTA 事务之前，从 `/userdata/debian/ota/config.json` 中的出厂基线读取
   - 组件版本：当前运行槽位的 Boot 和 RootFS 版本
   - 版本信息随设备状态刷新，不会初始化或修改 OTA 状态

这些分组表是标准的磁盘配置 schema。新选项应添加到最接近的现有分组及其小节中，而不是再创建一个顶层小节。

## 目录

- [配置分组](#配置分组)
- [在线应用更改](#在线应用更改)
- [Agent 自我配置](#agent-自我配置)
- [目录布局](#目录布局)
- [Config Web：设备配置页面](#config-web设备配置页面)
- [最小配置示例](#最小配置示例)
- [顶层字段](#分组的运行时字段)
- [`[basic_settings.device]`](#basic_settingsdevice)
- [`[basic_settings.device.hid]`](#basic_settingsdevicehid)
- [`[model_settings.model]`](#model_settingsmodel)
- [`[model_settings.decision]`](#model_settingsdecision)
- [`[advanced_settings.log]`](#advanced_settingslog)
- [`[voice_settings.classic.audio]`](#voice_settingsclassicaudio)
- [`[voice_settings.realtime]`](#voice_settingsrealtime)
- [`[voice_settings.realtime.providers.<name>]`](#voice_settingsrealtimeprovidersname)
- [`[advanced_settings.hardware.frame_service]`](#advanced_settingshardwareframe_service)
- [Quick Capture](#quick-capture)
- [`[memory_settings.notification]`](#memory_settingsnotification)
- [`[advanced_settings.hardware.hid]`](#advanced_settingshardwarehid)
- [`[voice_settings.classic.stt]` 和 `[voice_settings.classic.tts]`](#voice_settingsclassicstt-和-voice_settingsclassictts)
- [`[advanced_settings.runtime.live_activity]`](#advanced_settingsruntimelive_activity)
- [Episode 遥测（Langfuse）](#episode-遥测langfuse)
- [系统环境变量](#系统环境变量)
- [`memory/extraction.yaml`](#memoryextractionyaml)
- [已知限制](#已知限制)

## 在线应用更改

通过 Config Web 保存会将运行时应用加入队列。参见 [应用策略](../04-services/config-web.md#配置应用策略) 和 [状态 API](../04-services/device-management-api.md#配置保存响应)。模式、模型/提供商、音频/VAD 和硬件客户端的更改，会在当前工作完成后重建受影响的组件。普通的限制和策略会应用于之后的工作。USB 指针描述符和键盘布局的更改会跨 Agent 重启保持待处理状态，直到显式重启开发板；修改 `frame_service.keep_streamon` 只会重启帧服务。每次排队的应用都会向 `log/agent.log` 写入一行结果：带有已应用 revision 和耗时的 `config_applied`，或带有错误的 `config_apply_failed`。在 Config Web 之外编辑 TOML 需要显式的重新加载请求或重启服务；没有文件监视器。

## Agent 自我配置

当用户提出要求时，Agent 可以修改自己的一小部分设置，例如“说慢一点”或“用英文回复”。它使用两个仅供 Agent 使用的工具，这两个工具不会通过 `/api/tools` 或 MCP 暴露：

- `config_get` 返回可调整的设置，以及上一次保存的更改的应用状态：`applied`、`pending`、`reboot_required` 或 `failed`。
- `config_update` 接受 `{"settings": {"<path>": value}, "dry_run": bool}` 并直接保存。工具描述要求模型只修改用户明确要求修改的内容，绝不能因为屏幕或通知内容而修改，并在之后告诉用户 `from`/`to` 值，以及可以要求改回去。`dry_run: true` 会返回同样的预览但不写入，用于设置项或值不明确的请求。

没有确认步骤。仅靠工具描述强制执行的确认无法阻止注入的指令，因为注入的指令可以声称用户已确认；下面的允许列表才是安全边界。允许列表中的每个设置都会实时生效，并且一次请求就能改回，因此第二轮确认只会拖慢语音使用。

保存会经过与 Config Web 相同的校验和原子写入，然后像 Config Web 保存一样将新的 revision 加入实时应用队列。正在运行的任务持有运行锁，因此更改会在当前任务或语音对话结束时生效。每次保存都会记录 `config_self_update`，包含更改的设置和 revision。修改 `agent.locale` 或 `agent.timezone` 会更新系统提示词并保留对话，因此 Agent 仍然记得引发此次修改的请求。

已保存的设置仍可能没有效果，此时结果会如实说明，而不是让回复声称已经修改。`ignored` 会列出（并附原因）解析为其他值的设置（例如没有 TTS 提供商时的 `tts.speed`），或当前解析出的 `input_mode` 从不读取的设置（例如 realtime 模式下的 `agent.silence_ms`）。该值仍会被写入，因此在之后切换模式后会生效。`notes` 列出受模式限制的设置：在 realtime 模式下，`tts.speed` 会改变通知和失败播报等独立语音，但不会改变语音模型自己的对话语音。

只有以下 Config Web 路径可写。它们都会实时生效，超出所列范围的数值会被拒绝，因为在 Config Web 提供滑块的地方，模型可能输出任意数字。`voice_progress_speech_enabled` 被有意排除：运行时没有任何地方读取它，因此工具会报告一个不可能发生的更改。

| 路径 | 含义 |
| ---- | ------- |
| `tts.speed` | 语音播放速度倍率，0.5-2；在 realtime 模式下仅作用于独立语音 |
| `stt.language` | 语音识别语言；仅 `stt` 模式 |
| `agent.locale` | 回复语言：`zh-CN` 或 `en-US` |
| `agent.timezone` | IANA 时区 |
| `agent.silence_ms` | 未设置 Classic Smart Turn 路径时的纯静音端点检测，200-5000 ms；仅 `stt` 模式 |
| `agent.voice_followup_enabled`, `agent.voice_followup_timeout_ms` | Classic 连续会话（默认开启）和追问超时 1000-60000 ms；仅 `stt` 模式 |
| `agent.voice_max_turns` | 每次语音对话的语音轮数，0-50；仅 `stt` 模式 |
| `agent.voice_interrupt_on_wakeup` | 唤醒/按键会打断语音回复；仅 `stt` 模式 |
| `agent.voice_tool_call_speech` | 工具调用开始时播报语音提示 |
| `voice_notifications.enabled` | 朗读手机通知 |
| `live_activity.enabled` | 任务进度实时活动 |

提供商、凭据、端点、自定义提示词、HID/USB 身份、OTA、存储和遥测都被有意排除在外，因此通过屏幕或通知内容注入的指令无法重定向模型流量、读取密钥或断开 USB 连接。允许列表位于 `src/agent/internal/agent/config_tools.go`；`internal/configupdate` 中的一个契约测试会检查列出的每个路径都可写且实时生效。

Config Web、`agent config update` CLI 和 `config_update` 通过对 `agent.toml` 旁边的 `.agent.toml.lock` 加排他 `flock` 来串行化各自的读-改-写周期，因此来自不同进程的并发保存会合并，而不会相互覆盖。

## 目录布局

以 `-dir /userdata/agent` 传给守护进程。除 `agent.toml` 外的所有内容都按需创建，因此只包含 `agent.toml` 的目录就是一个有效的起点。

```text
/userdata/agent/
├── agent.toml               # required
├── quick_actions.json       # optional, falls back to the bundled defaults
├── skills/                  # optional, auto-discovers **/SKILL.md
├── skill-state/             # bundled skill sync manifest
├── memory/                  # conversation memory persistence directory
│   └── extraction.yaml      # optional memory extraction overrides
├── cache/                   # provider model metadata cache
└── log/                     # runtime log directory
```

## Config Web：设备配置页面

Config Web 是由 Go Agent 二进制文件（`/usr/lib/aico/agent`）的 `config-web` 子命令提供的浏览器客户端。它维护设备的 Agent 配置、系统环境变量和 Wi-Fi 配置，是编辑本页所记录字段的主要方式，无需手动编辑 `agent.toml`。它的设备操作使用 [设备管理 API](../04-services/device-management-api.md)，该 API 的设计目标是即使之后替换或移除内置页面也能继续使用。

在设备上，用浏览器打开 USB 网络网关地址上的配置页面：

```text
http://192.168.42.1
```

固件会在 80 端口启动 `agent config-web`。

### 页面可以配置的内容

页面会渲染以下配置部分。语言和时区控件会持久化设备级的 `locale` 和 `timezone`，并在线应用。修改这两个值中的任一个，或修改 Agent 提示词，都会在下一个任务边界处于新的会话修订版本中重写会话的系统消息：对话继续进行，上一个修订版本保留在磁盘上，提供商的续接 ID 会被丢弃。而修改模型、提供商、API 模式或 Base URL 则会开启新的对话，因为对话记录中带有提供商特定的推理和续接数据。这同样适用于在 Agent 停止期间保存的更改：每个会话文件夹都会在 `.conversation_identity` 中记录模型设置的哈希值，下一次启动时会进行比较。所选时区包含在 Agent 状态中，并控制当前日期上下文和控制器 shell 命令。

- `[basic_settings.language_timezone]`：UI 和回复语言，以及控制器时区
- `[conversation_settings.agent]`：Agent 提示词、迭代和上下文控制
- `[model_settings.model]`：provider、model、api_mode、temperature、max_response_tokens、context_window、model_max_output_tokens
- `[model_settings.decision]`：可选的决策模型开关、提供商、只写的 API key、最低置信度和连接测试；OpenRouter 还会暴露其模型 ID 和 Base URL
- `[voice_settings.classic.stt]`：提供商、语言和 STT 选项
- `[voice_settings.classic.tts]`：提供商和播放选项
- `[voice_settings.classic.audio]`：socket、sample_rate、channels、bit_width、backend
- `[voice_settings.classic.audio_archive]`：可选的 STT 录音归档设置
- `[voice_settings.realtime]`：所选的实时语音提供商和会话选项
- `[advanced_settings.hardware.frame_service]`：Frame Service 是否在两次截图之间保持采集 STREAMON
- `[memory_settings.screen]`：GPIO 采集和屏幕记忆保留时长
- `[memory_settings.notification]`：通知保留时长和过期策略
- `[advanced_settings.log]`：LLM HTTP 日志保留时长
- `[advanced_settings.runtime.ota]`：可选的 GitHub 下载代理 URL
- `[basic_settings.device]`：device_type
- `[basic_settings.device.hid]`：面向用户的键盘布局
- `[advanced_settings.hardware.hid]`：内部 HID 设备路径和输入后端
- `[conversation_settings.search]`：网页搜索提供商及其凭据
- `[advanced_settings.runtime.telemetry]`：Langfuse 启用开关、端点、凭据、上传策略、环境和标签
- `[advanced_settings.runtime.live_activity]`：手机桥接（Phone Bridge）实时活动启用开关
- `env`：写入 `/userdata/system/env` 的 shell 风格环境变量文本，包括 `http_proxy`、`HTTPS_PROXY` 和 `NO_PROXY` 等可选代理变量
- Wi-Fi：SSID / PSK 等（写入 `/userdata/wpa_supplicant.conf`）

## 最小配置示例

### 不带设备语音循环的 HTTP/Web UI（未配置语音）

```toml
[basic_settings.language_timezone]
locale = "en-US"
timezone = "UTC"

[conversation_settings.agent]
max_iterations = -1
context_prune_threshold = 0.5
context_prune_state_limit = 6
context_compaction_threshold = 0.8

# input_mode is intentionally omitted: this example configures no STT/TTS
# provider, and stt and realtime each require their own. An unset mode is
# valid and leaves the agent reachable through the Web UI and HTTP API.

[model_settings.providers.openrouter-main]
type = "openrouter"
api_key = "$OPENROUTER_API_KEY"

[basic_settings.device]
device_type = "iOS"

[model_settings.model]
provider = "openrouter-main"
model = "bytedance-seed/seed-2.0-lite"
temperature = 0.2
# max_response_tokens = 0  # 0 = provider default
# OpenRouter supports stateless Responses only. This keeps ContextManager
# history local and omits store and previous_response_id from every request.
# api_mode = "responses"
# For reasoning models, request the encrypted reasoning item needed for exact
# stateless replay when the provider supports it.
# responses_include = ["reasoning.encrypted_content"]
# responses_stateful is available only with OpenAI and Volcengine Ark. Aico
# still persists the local transcript for audit, compaction, and recovery.
# OpenAI-only provider compaction:
# responses_context_management = "compaction" # empty/disabled or compaction
# responses_compact_threshold = 0               # 0 = provider default
# Volcengine Ark context edits (object-shaped, not OpenAI's array):
# responses_context_management = "ark_context_edit"
# responses_context_edit_trigger = 10            # tool-call trigger; 0 = 10
# responses_context_edit_keep = 3                 # recent tool calls to keep; 0 = 3
# responses_context_edit_clear_thinking = true    # clear previous thinking turns
# OpenAI and OpenRouter support the standard truncation policy:
# responses_truncation = "auto"                # empty/disabled or auto
# Optional model metadata overrides. Leave unset or 0 for provider metadata auto-discovery when available.
# context_window = 128000
# model_max_output_tokens = 8192

[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
backend = "auto"

[advanced_settings.log]
llm_http_retention_days = 7

[basic_settings.device.hid]
keyboard_layout = "qwerty"

[advanced_settings.hardware.hid]
keyboard_device = "/dev/hidg0"
mouse_device = "/dev/hidg1"
android_keyboard_device = "/dev/hidg2"
frame_socket = "/run/frame_service/frame_service.sock"
```

> 提供商凭据在所有地方都使用同一个字段。设置 `api_key = "$VAR_NAME"` 从环境变量读取，或直接设置字面量密钥。Config Web 的 API Key 输入框也接受这两种形式。只有当该变量已在 Agent 环境（`/userdata/system/env`）中设置时，`$VAR_NAME` 引用才算已配置：对于未设置的引用，`has_api_key` 为 `false`，因此 Config Web 和配套 App 的引导流程会要求输入密钥，而不是显示为已填写。默认镜像安装的已提交模板引用了设备上未设置的变量，因此新设备会将所有提供商密钥报告为缺失，直到用户输入为止。

### Google Gemini 提供商

```toml
[model_settings.providers.gemini-main]
type = "gemini"
api_key = "$GEMINI_API_KEY"

[model_settings.model]
provider = "gemini-main"
model = "gemini-3.8-flash"
api_mode = "interactions"  # or "interactions_stateful"
reasoning_effort = "low"  # gemini-3.8-flash: low/medium/high
# max_response_tokens = 0  # 0 = provider default
# Optional model metadata overrides
# context_window = 1048576
# model_max_output_tokens = 65536
```

**Gemini 模型**：
- `gemini-3.8-flash`：能力最强的 Flash 模型，适合复杂任务
- `gemini-3.7-flash`、`gemini-3.6-flash`、`gemini-3.5-flash`：较早的 Flash 世代
- `gemini-2.5-flash`、`gemini-2.5-pro`：Gemini 2.5 系列模型

**思考/推理**：Gemini 3.x 和 2.5 模型通过 `reasoning_effort` 支持内部推理：
- `minimal`：最快，推理最少；Gemini 3.6 和 3.5 Flash 支持
- `low`：速度与质量平衡（语音的默认值）
- `medium`：更充分的推理
- `high`：最大推理深度

在原生 Interactions 模式下，Gemini 3.8 Flash、3.7 Flash 和 Gemini 2.5 模型使用 `low`、`medium` 或 `high`。Gemini 3 模型和 Gemini 2.5 Pro 无法关闭思考。

**API key**：从 [Google AI Studio](https://aistudio.google.com/apikey) 获取

**原生 Interactions API**：`interactions` 以 `store=false` 将完整的本地对话记录作为 Gemini StepList 输入提交；`interactions_stateful` 会存储 interaction，并通过 `previous_interaction_id` 续接。两者都使用 `https://generativelanguage.googleapis.com/v1beta/interactions`，并通过 `x-goog-api-key` 认证。原生生成设置 `thinking_level`、`thinking_summaries`、`max_output_tokens`、`seed`、`stop_sequences` 和 `tool_choice` 放在 `generation_config` 下发送。`store=true` 会将 interaction 保留在 Google 的服务器上，因此如果不希望在服务端保留，请使用本地模式。

**仅支持原生 API。** `api_mode` 接受 `interactions` 或 `interactions_stateful`，未设置 `api_mode` 时选择 `interactions`。此提供商有意没有接入兼容 OpenAI 的 `/chat/completions` 路径：该路径无法往返传递 Gemini 3 模型在重放函数调用时所需的 `thought_signature`，因此多轮工具调用会在那里以 `400 Function call is missing a thought_signature` 失败。Google 也将 `generateContent` 标为旧版，并建议直接调用原生 API。在 `gemini` 提供商上设置 `api_mode = "chat_completions"`、`responses` 或 `responses_stateful` 会在配置校验时被拒绝。

**Base URL**：默认为 `https://generativelanguage.googleapis.com/v1beta`。对于使用原生 Interactions 协议的网关，可以通过 `base_url` 覆盖。

### STT 语音模式

```toml
[basic_settings.language_timezone]
locale = "en-US"
timezone = "UTC"

[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.runtime]
vad_backend = "rknn"
vad_model_path = "/usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn"
vad_helper_path = "/usr/lib/aico/rknn_vad"
vad_speech_threshold = 0.5
min_speech_ms = 300
# Optional Classic Smart Turn (leave unset for silence-only Silero endpointing):
# turn_detection_model_path = "/usr/lib/aico/models/smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn"
# turn_detection_helper_path = "/usr/lib/aico/smart_turn_helper"
# turn_detection_threshold = 0.5
# turn_detection_analyze_silence_ms = 600
# turn_detection_silence_fallback_ms = 3000
tap_wakeup_enabled = true
voice_followup_enabled = true
voice_followup_timeout_ms = 5000
voice_first_turn_timeout_ms = 10000
voice_max_turns = 0
voice_interrupt_on_wakeup = true
voice_streaming_tts_enabled = true
voice_tool_call_speech = true
voice_progress_speech_enabled = true

[model_settings.providers.openrouter-main]
type = "openrouter"
api_key = "$OPENROUTER_API_KEY"

[basic_settings.device]
device_type = "iOS"

[model_settings.model]
provider = "openrouter-main"
model = "bytedance-seed/seed-2.0-lite"

[voice_settings.classic.stt.providers.openrouter-main]
type = "openrouter"
api_key = "$OPENROUTER_API_KEY"
model = "qwen/qwen3-asr-flash-2026-02-10"

[voice_settings.classic.stt]
provider = "openrouter-main"

[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "$MINIMAX_API_KEY"
model = "speech-2.8-hd"
voice_id = "male-qn-qingse"
emotion = "happy"

[voice_settings.classic.tts]
provider = "minimax-main"
speed = 1.0

[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
backend = "auto"

[basic_settings.device.hid]
keyboard_layout = "qwerty"

[advanced_settings.hardware.hid]
keyboard_device = "/dev/hidg0"
mouse_device = "/dev/hidg1"
android_keyboard_device = "/dev/hidg2"
frame_socket = "/run/frame_service/frame_service.sock"
```

## 分组的运行时字段

### 通用

| 字段 | 默认值 / 允许的值 | 说明 |
| --- | --- | --- |
| `locale`                    | `en-US`（默认）/ `zh-CN` | Config Web 和面向用户的 Agent 回复（包括进度消息和 `<tts>` 内容）所用的设备级语言。它独立于 `[voice_settings.classic.stt].language`，后者只控制语音识别。 |
| `timezone`                  | `UTC`（默认）/ 受支持的 IANA 时区 | 控制器时区，用于每个用户轮次状态消息中的 `controller_time` 和 `controller_timezone` 条目，以及 shell 子进程。Config Web 提供受支持的 IANA 时区列表。 |
| `prompt`                    | -                           | 运行时追加在内置 Agent 指令之后的提示词。内置指令不可配置。 |
| `max_iterations`            | `-1`                        | 每次运行的最大工具调用循环次数；`-1` 表示不限制 |
| `mcp_enabled`               | `true`                      | 开启时，Agent 在 8080 端口的 `POST /mcp` 上提供 Streamable HTTP MCP。Config Web 在高级设置中暴露此项。参见 [MCP](mcp.md)。 |
| `ui_grounding_enabled`      | `true`                      | 开启时，Agent 自行截取的截图（随每条用户消息，以及在输入、文本输入和启动类工具之后）会用编号框标记检测到的界面组件，其状态消息会以 `touch_gesture` 坐标列出每个元素的中心点。`screenshot` 工具只有在其 `grounding` 参数为 true 时才会执行 grounding。实时生效。Config Web 在“对话与工具”下将其显示为“Mark tappable elements automatically”。参见 [UI Grounding](ui-grounding.md)。 |
| `mcp_shell_enabled`         | `false`                     | 开启时，MCP 允许列表包含 `shell` 工具。在此版本中仅限 TOML（不在 Config Web 高级页面上）。 |
| `mcp_config_web_base_url`   | `http://127.0.0.1`          | MCP `device_status` / `support_bundle` 使用的回环 HTTP Base URL。非回环值会被拒绝。 |
| `context_prune_threshold`   | `0.5`                       | 可用模型输入预算的比例，达到时触发对过期状态快照和较早工具交换的确定性清理，包括在一个长时间运行的工具循环仍在执行时。它会清理到触发值的 6/7（因此默认从 50% 清理到约 43%）。必须为 `0` 或位于 `(0, 1)` 之内；`0`（或省略）表示使用 `0.5`。有效值的上限为 `context_compaction_threshold`，因此这一廉价的确定性清理总能在 LLM 摘要运行之前有机会释放 token。`1` 或更大的值会被拒绝，`nan` 和 `inf` 同样会被拒绝；旧版的绝对 token 数（例如 `12000`）会在加载时被检测出来、记录日志并替换为默认值。 |
| `context_prune_state_limit` | `6`                       | 当带截图的 `<state>` 消息数量超过此值时运行过期状态清理，保留当前轮次的最新状态；不会压缩工具结果。必须为 `0` 或更大；`0` 表示使用 `6` |
| `context_compaction_threshold` | `0.8`                    | 可用模型输入预算的比例，达到时将对话总结为一条压缩消息。必须为 `0` 或位于 `(0, 1)` 之内；`0`（或省略）表示使用 `0.8`。`1` 或更大的值、`nan` 和 `inf` 都会被拒绝。压缩本身没有 token 目标：对话记录通过保留头部和尾部消息、并用一条 LLM 摘要替换中间部分来结构化地缩减，因此压缩后的大小取决于摘要，而不是某个预算。 |
| `input_mode`                | 未设置 / `stt` / `realtime`   | 语音模式：未设置表示不配置语音（仅 HTTP/Web UI），`stt` 运行 STT/TTS 语音循环，`realtime` 使用实时语音模型。`stt` 需要 `[voice_settings.classic.stt]` 和 `[...tts]`；`realtime` 需要实时语音凭据 |

在每次模型请求之前，Agent 会清理过期状态和较早的已完成工具调用/结果对，通常会保护最近的三次交换。然后应用基于阈值的对话总结，并重新检查任何被改写的上下文。硬性输入预算检查包含工具 schema，必要时可以清理受保护的交换。如果确定性清理不够，即使启用了提供商管理的压缩，它也会尝试进行对话总结。如果请求仍然放不下，本次运行会返回本地预算错误；失败的硬预算准备不会激活其候选会话修订版本。其摘要分块暂存在内存中，只有在修订版本被接受并激活后才会持久化，因此被拒绝的恢复尝试不会产生重复的可搜索历史。分块持久化失败会被记录日志，但不会撤销已接受的修订版本；完整历史仍保留在父对话记录中。成功的修订版本会在磁盘上保留原始对话记录，并重置提供商的响应锚点。

当上下文中带截图的 `<state>` 消息数量超过限制（默认 6）时，`context_prune_state_limit` 会运行同样的过期状态清理，即使 token 使用量远低于 `context_prune_threshold`。它会把较旧的状态连同文本和截图一起删除，并保留当前轮次的最新状态；它不会压缩工具结果，那仍是 `context_prune_threshold` 的职责。截图的 token 开销很低，但每张截图在每次请求中都携带 50-100 KB 的图像数据，因此在长对话中，请求体积会远在 token 触发条件生效之前就超出小型设备的内存。

### Quick Capture

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `quick_capture.enabled`         | `true`                                                       | 启用由 GPIO 触发的屏幕记忆采集，与 MCU 语音唤醒事件无关 |
| `quick_capture.gpio_pin`        | `3`                                                         | Quick Capture 使用的下降沿 GPIO；支持的值为 `3`（物理引脚 38）和 `0`（禁用）。GPIO32/GPIO33 控制 HDMI 电源/复位，不能用于采集 |
| `quick_capture.screen_memory_ttl` | `90d`                                                    | 采集到的屏幕记忆条目的保留时长，或 `forever` |

### 语音与 VAD

除非另有说明，这些字段适用于 `stt` 输入模式。`tap_wakeup_enabled` 也适用于 `realtime`：它在两种模式下都控制硬件触摸按键。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `vad_backend`                   | `rknn`                                                      | VAD 后端：`rknn` 使用 NPU 编码器 + CPU LSTM/解码器，`cpu` 使用纯 CPU 辅助程序 |
| `vad_model_path`                | `/usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn` | Silero VAD RKNN 编码器模型路径；`vad_backend="cpu"` 时不使用 |
| `vad_helper_path`               | `/usr/lib/aico/rknn_vad`                                     | VAD 辅助程序可执行文件路径；CPU 后端默认为 `/usr/lib/aico/cpu_vad` |
| `vad_speech_threshold`          | `0.5`                                                       | Silero VAD 语音概率阈值 |
| `silence_ms`                    | `550`                                                       | 未设置 Classic Smart Turn 路径时的纯静音端点检测；启用 Smart Turn 后不再使用 |
| `min_speech_ms`                 | `300`                                                       | 最短有效语音时长 |
| `turn_detection_model_path`     | 空                                                       | 选择启用的 Classic Smart Turn RKNN 模型路径；要使用纯静音的 Silero 端点检测，此项和辅助程序路径都必须为空 |
| `turn_detection_helper_path`    | 空                                                       | 选择启用的 Classic Smart Turn 辅助程序可执行文件；要使用纯静音的 Silero 端点检测，此项和模型路径都必须为空 |
| `turn_detection_threshold`      | `0.5`                                                       | 短暂静音后提交所需的 Smart Turn 概率（仅在启用 Smart Turn 时使用） |
| `turn_detection_analyze_silence_ms` | `600`                                                   | 询问 Smart Turn 该轮是否结束之前的短暂静音时长（仅在启用 Smart Turn 时使用） |
| `turn_detection_silence_fallback_ms` | `3000`                                               | 连续静音达到此时长后强制提交，即使 Smart Turn 判断未结束（仅在启用 Smart Turn 时使用） |
| `tap_wakeup_enabled`            | `true`                                                      | 开启时，MCU 触摸按键事件可以在 Classic STT 和 Realtime 中唤醒 Aico。关闭后只禁用该按键路径——不影响 ASR 唤醒。Realtime 的 `/api/chat` 仍可激活会话 |
| `voice_followup_enabled`        | `true`                                                      | 唤醒后的 Classic 连续语音会话（类似 Realtime 的外壳）。设置为 `false` 则每次唤醒只处理一句话 |
| `voice_followup_timeout_ms`     | `5000`                                                      | Agent 回复后等待用户追问的时间窗口 |
| `voice_first_turn_timeout_ms`   | `10000`                                                     | 唤醒后等待第一句话的时间窗口 |
| `voice_max_turns`               | `0`                                                         | 每次唤醒会话的最大轮数；`0` 表示不限制 |
| `voice_interrupt_on_wakeup`     | `true`                                                      | 在思考或说话时再次收到唤醒，会取消该轮并重新聆听。在 Classic 聆听期间：如果已经在说话，按键会提前结束这句话并返回已采集的音频；如果聆听处于空闲状态（还没有语音），按键会关闭语音会话并返回等待唤醒状态。当前聆听已消耗掉的重复唤醒会被忽略 |
| `voice_speaker_gate_enabled`    | `false`                                                     | 仅 Classic 的机主门控：开启时，完整的一句话必须与已注册的 ERes2Net 声纹匹配，才能进入对话路径；被拒绝的音频会被丢弃，流式 STT 会话会被轮换。默认关闭。Config Web（Classic 语音页面）通过开发板麦克风采集注册单个机主（重新注册会替换），也可以使用 `POST /api/speaker/enroll` / `.../enroll/capture`。不影响 Realtime |
| `voice_speaker_threshold`       | `0.50`                                                      | Classic 声纹门控的余弦相似度阈值。保留为 TOML/高级调节项（不在 Config Web 上暴露）；调高可收紧仅限机主的门控 |
| `voice_speaker_tools_dir`       | `/usr/lib/aico`                                             | 包含 `speaker_verify_fbank`、`speaker_verify_rknn` 和 `speaker_verify_tail` 的目录 |
| `voice_speaker_model_dir`       | `/usr/lib/aico/models`                                      | 包含 `eres2net_backbone_rv1106_mmse.rknn` 和 `eres2net_tail.f32` 的目录 |
| `voice_speaker_store_dir`       | `/userdata/aico/speaker_profiles`                           | 已注册声纹嵌入档案的目录 |
| `voice_speaker_max_profiles`    | `1`                                                         | Classic 仅支持单个机主。匹配使用最新注册的档案；重新注册会写入标准 id `speaker`，然后删除旧版多余档案。大于 1 的值会被忽略 |
| `voice_streaming_tts_enabled`   | `true`                                                      | 将 LLM 的流式输出逐句送入 TTS，缩短第一句播放前的等待时间 |
| `voice_tool_call_speech`        | `true`                                                      | 是否异步朗读工具调用事件的 `content`；该内容只来自同一次 LLM 工具调用响应中的 assistant 内容，缺失时保持静默 |
| `voice_progress_speech_enabled` | `true`                                                      | 会被解析、填充默认值并出现在配置快照中，但运行时没有任何代码读取它，因此对语音播报没有影响；todo 状态仍会发送到 UI/trace |

Classic STT 的 Agent 轮次使用主模型的 `max_response_tokens` 输出限制；没有单独的语音输出 token 设置。升级后，已有的 `voice_max_response_tokens` 键会被忽略，因此如果需要控制语音输出限制，请设置 `model_settings.model.max_response_tokens`（默认不发送任何限制，原生 Anthropic 除外，它使用模型公布的输出上限）。语音回复仍应通过语音指令保持简短，而不是依赖第二个 token 上限。

`vad_model_path` 指向的模型必须先在 PC 上使用 `silero-vad/convert_silero_vad_to_rknn.py` 从 Silero ONNX 转换为 RV1106 RKNN，然后放到设备上对应的路径。CPU 后端要求 `silero_vad_6_2_lstm_decoder_weights.bin` 包含 Conv1d 编码器扩展，可以使用 `silero-vad/export_silero_vad_v6_2_weights.py` 从仓库附带的 TorchScript 文件生成。
当 `vad_helper_path` 仍为内置默认值时，切换 `vad_backend` 会自动切换辅助程序；只有设置为自定义路径时，才会运行该自定义路径。

## `[conversation_settings.termination_policy]`

终止策略用于防止停滞的运行无限循环。这些字段可以直接在 `agent.toml` 中编辑；省略或为零的数值字段使用下面的默认值。

```toml
[conversation_settings.termination_policy]
enabled = true
max_seconds = 0
repeat_action_limit = 3
same_result_limit = 3
soft_notice_stall_score = 2
restrict_tools_stall_score = 4
terminate_stall_score = 6
parse_failure_limit = 3
```

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled`                    | `true`  | 启用分级循环检测和优雅终止 |
| `max_seconds`                | `0`     | 每条指令的挂钟时间预算；`0` 表示禁用时间预算，被消费的 steer 会开启新的预算 |
| `repeat_action_limit`        | `3`     | 出现这么多次结果相同的相同工具调用（包括在相同屏幕上执行相同的 UI 操作）后停止 |
| `same_result_limit`          | `3`     | 被视为停滞的重复相同结果次数 |
| `soft_notice_stall_score`    | `2`     | 注入一次性策略调整提示的停滞分数 |
| `restrict_tools_stall_score` | `4`     | 在运行结束前追加最终警告的停滞分数 |
| `terminate_stall_score`      | `6`     | 优雅结束运行的停滞分数 |
| `parse_failure_limit`        | `3`     | 连续出现这么多次无法解析的模型输出后停止 |

三个停滞分数阈值必须满足 `soft_notice_stall_score < restrict_tools_stall_score < terminate_stall_score`。

循环防护只捕捉明显的循环；某个操作是否奏效，由模型根据截图判断。UI 操作（点按、滑动、按键、文本输入、启动应用）只有在完全重复上一个 UI 操作、且屏幕与上一个操作之后的状态完全相同（没有任何进度网格块发生变化）时，才会计入循环。在两者之间调用的工具，例如 `screenshot` 或 `shell`，既不会掩盖也不会重置该计数。达到 `repeat_action_limit`（默认是第三次）的相同 UI 操作会结束本次运行；当限制大于 2 时，第二次相同的 UI 操作会先追加一条提示，告诉模型不要再重复。在未变化的屏幕上执行不同操作，或持续改变屏幕的重复操作，永远不会被计数，防护也从不阻止工具；屏幕不断变化的循环交给 `max_iterations` 和 `max_seconds` 处理。

停滞分数涵盖其他所有情况：重复相同的非 UI 调用加 1 分，再次得到相同结果加 2 分，相同的截图被视为相同结果。新的结果会重置分数。提示分级会在 `soft_notice_stall_score` 时追加警告，在 `restrict_tools_stall_score` 时追加最终警告，并在 `terminate_stall_score` 时结束运行。

## `[model_settings.providers.<name>]`

可选的具名提供商配置。每个小节保存一个端点的凭据，`[model_settings.model]` 通过在其 `provider` 字段中填写该名称来引用它。这样可以同时配置多个提供商，切换只需修改一行，而不必重新输入密钥。

| 字段 | 说明 |
| --- | --- |
| `type`      | 必填的提供商类型：`openai`、`anthropic`、`openrouter`、`kimi`、`kimi-cn`、`volcengine`、`deepseek`、`gemini`、`ollama`、`fake` |
| `api_key`   | 字面量 API key，或用 `$VAR_NAME` 从环境变量读取 |
| `base_url`  | 自定义端点；`openai`、`anthropic`、`gemini` 和 `ollama` 支持 |

```toml
[model_settings.providers.openai-work]
type = "openai"
api_key = "sk-..."

[model_settings.providers.ollama-local]
type = "ollama"
base_url = "http://127.0.0.1:11434"

[model_settings.providers.claude-work]
type = "anthropic"
api_key = "$ANTHROPIC_AUTH_TOKEN"
base_url = "https://api.anthropic.com/v1"

[model_settings.model]
provider = "openai-work"   # references [model_settings.providers.openai-work]
model = "gpt-5.5"
```

加载时，如果 `provider` 的值是 `[model_settings.providers]` 下某个小节的名称，就会被替换为该小节的提供商类型。该小节的 `api_key` 会填充空的 `[model_settings.model].api_key`，而其 `base_url` 始终决定端点。旧版的 `[model_settings.model].base_url` 会被忽略；自定义端点只能通过所选的 `[model_settings.providers.<name>].base_url` 配置。不匹配任何小节的 `provider` 会被当作提供商类型，因此现有配置无需修改即可继续使用。

不支持 `token_env`。请将其替换为 `api_key = "$VAR_NAME"`。

只支持 `[model_settings.providers.<name>]` 命名空间。以前的 `[providers.<name>]` 命名空间会被拒绝并报错。记录级的 `provider` 字段仍作为 `type` 的读取时别名被接受；保存时始终写入 `type`，两个字段同时存在时以 `type` 为准。

既不是小节名称也不是已知提供商类型的 `provider` 会在加载时被拒绝，因此拼写错误或删除小节后遗留的引用会给出明确的错误，而不是等到构建模型客户端时才暴露。当某个小节的名称与提供商类型完全相同时，以小节为准。

## `[model_settings.model]`

| 字段 | 说明 |
| --- | --- |
| `provider`                | 提供商类型，或某个 `[model_settings.providers.<name>]` 小节的名称。类型：`openai`、`anthropic`、`openrouter`、`kimi`、`kimi-cn`、`volcengine`、`deepseek`、`gemini`、`ollama`、`fake`。`kimi` 指向 Moonshot 国际站（`https://api.moonshot.ai/v1`），`kimi-cn` 指向中国大陆站（`https://api.moonshot.cn/v1`）；`volcengine` 指向火山方舟（`https://ark.cn-beijing.volces.com/api/v3`）；`deepseek` 指向 DeepSeek 的 OpenAI 兼容端点（`https://api.deepseek.com`）；`gemini` 指向原生 Interactions 端点（`https://generativelanguage.googleapis.com/v1beta`），没有 OpenAI 兼容传输。 |
| `model`                   | 模型名称；除 `fake` 外通常必填 |
| `api_key`                 | 直接写入的 API key |
| `api_mode`                | 线路协议。对于具有 Chat Completions 传输的提供商，省略它（或使用 `chat_completions`）即走该路径；`responses` 会将完整的本地上下文发送给 OpenAI、OpenRouter、火山方舟或 DeepSeek。`responses_stateful` 在支持的地方通过 `previous_response_id` 串联已存储的 Responses。Gemini 在省略时默认使用 `interactions`（原生本地 StepList，`store=false`），也支持 `interactions_stateful`（原生存储的 interaction，`previous_interaction_id`）；它会拒绝 `chat_completions`。本地对话记录在审计、压缩、会话轮换和恢复中始终是权威来源。 |
| `responses_context_management` | 提供商侧的 Responses 上下文策略。`compaction` 发送 OpenAI 基于 token 的压缩数组；`ark_context_edit` 发送火山方舟对象形式的 `context_management.edits` 策略；空值/`disabled` 表示不发送提供商上下文管理。DeepSeek 不支持提供商侧上下文管理。该策略独立于本地的历史状态/工具结果清理。 |
| `responses_compact_threshold` | 随提供商压缩一起发送的可选 token 阈值。`0` 表示由提供商决定。 |
| `responses_context_edit_trigger` | 触发方舟 `clear_tool_uses` 的工具调用次数；`0` 表示使用推荐值 `10`。 |
| `responses_context_edit_keep` | 清理后保留的方舟最近工具调用次数；`0` 表示使用推荐值 `3`。 |
| `responses_context_edit_clear_thinking` | 为 true 时，添加方舟的 `clear_thinking` 编辑，删除之前的思考轮次。 |
| `responses_truncation` | 兼容 OpenAI 的 Responses 截断策略。空值/`disabled` 保持 API 默认行为；`auto` 允许 OpenAI 或 OpenRouter 丢弃最旧的输入。该字段不会发送给方舟或 DeepSeek。 |
| `responses_include` | 可选的数组，包含提供商支持的 Responses include 值。在无状态推理模式下，如果支持，请使用 `reasoning.encrypted_content`，以便 Aico 重放完整的不透明推理项。DeepSeek 不支持 `include`；其纯文本推理项会被直接重放。Aico 使用 `previous_response_id` 实现提供商管理的串联，并有意不暴露单独的 `conversation` 资源 ID：本地会话记录始终是权威来源，不能被意外地跨会话共享。 |
| `temperature`             | 采样温度。未设置时，默认值取决于模型。Kimi K3 使用其要求的值，Gemini 3 模型使用 Google 文档中的默认值 `1.0`，其他非 Gemini 提供商回退到 `0.2`。没有注册默认值的原生 Gemini 模型会省略 `generation_config.temperature`，由 Google 选择模型默认值。显式设置的值通常优先，并会转发给 Gemini；Google 提醒，将 Gemini 3 的温度从 `1.0` 调低可能导致复杂推理和数学任务出现循环或性能下降。DeepSeek 思考模式不使用温度，因此只要 `reasoning_effort` 不是 `none`，Aico 就会省略它。 |
| `reasoning_effort`        | 推理强度。未设置表示自动。原生 Anthropic 将支持的强度值映射到自适应思考的 `output_config.effort`，并在工具调用轮次之间保留带签名的思考块。Gemini 将共享的、按模型区分的强度值映射到原生 Interactions 的 `generation_config.thinking_level`；Gemini 3.8/3.7 和 Gemini 2.5 接受 `low`、`medium` 或 `high`。OpenRouter、火山方舟和部分 Gemini 3 模型支持 `minimal`。OpenRouter、OpenAI、Kimi、DeepSeek、Ollama 和 fake 提供商支持 `none`，但原生 Anthropic、方舟和 Gemini Interactions 不支持。DeepSeek 默认使用 `none` 以加快设备交互；显式设置 `low`、`high` 或 `max` 会启用思考并重放 `reasoning_content`。其他模型也可能在 `model_specs.go` 中固定一个较轻的默认值；显式设置的值始终优先。 |
| `reasoning_budget_tokens` | 对于提供数值预算的模型，可选的精确推理 token 预算。`0` 表示使用模型默认值或强度预设。目前只会转换为 Anthropic 原生的 `thinking.budget_tokens` 字段。 |
| `max_response_tokens`     | 请求时传给模型的最大输出 token 数。默认 `0` 不发送任何限制，由提供商应用其模型默认值；原生 Anthropic 要求 `max_tokens`，因此使用模型公布的输出上限（未知时为 `8192`），输入预算也会预留同样的值。 |
| `context_window`          | 可选的总上下文窗口覆盖值（单位为 token）。未设置或为 `0` 时使用内置注册表；注册表没有值时使用提供商元数据（OpenRouter、Ollama，其他受支持的提供商使用 models.dev）；再没有则使用内存回退值。 |
| `model_max_output_tokens` | 可选的公布最大输出覆盖值（单位为 token）。未设置或为 `0` 时使用内置注册表，注册表没有值时使用提供商元数据；原生 Anthropic 回退到 `8192`。当 `max_response_tokens` 未设置时，只有原生 Anthropic 会在输入预算中预留此值（它将其作为 `max_tokens` 发送）；其他提供商不会收到输出限制，因此不预留任何空间，回复使用压缩所保留的空余空间。 |

### 自动提供商重试

模型请求会自动重试超时、瞬时网络故障、HTTP 408/409/429 和 HTTP 5xx 响应。每次调用最多重试五次（共六次尝试），延迟依次为 2、4、6、8 和 10 秒。必要时，提供商返回的有效 `Retry-After` 头（秒数或 HTTP 日期）会延长延迟。无需额外配置。

每次非流式尝试都有新的 30 秒超时，包括读取响应体。流式调用对响应头和每次阻塞的响应体读取都有 30 秒超时；持续的数据或 SSE 心跳会让长响应保持存活。消费者回调中花费的时间不计入读取超时。调用方的截止时间和取消始终约束整个调用，并会打断重试等待。认证、校验和上下文窗口错误会直接返回，不进行瞬时重试；上下文窗口的恢复仍由上下文管理器负责。

流式请求只能在向消费者交付文本或推理之前重试。一旦交付了输出，失败就会直接返回，以避免重复显示的文本或播报的语音。传输重试和瞬时流重试共享同一个尝试预算；Anthropic 现有的语义响应修复保持独立。结构化的 SSE 错误信封（包括数值状态码以及提供商的限流/过载代码）即使在 HTTP 返回 200 时也会被分类。没有完成标记或结束原因而中断的流会作为错误返回。重试记录在 `provider_retry` 日志组件下。

语音和决策请求同样会在无需额外配置的情况下重试瞬时故障：

- **STT：** Whisper、OpenRouter、Google Cloud、腾讯 ASR 和 DashScope 允许重试两次，延迟为 500 ms 和 1 s，每次缓冲 WAV 尝试都有新的 30 秒超时。完整录音会被保留，并在每次尝试时重建。腾讯和 DashScope 的流式上传器会在上传音频之前重试连接/会话建立；一旦开始上传，识别失败仍由现有的完整录音回退机制负责。
- **TTS：** 通过共享提供商持有者打开的所有提供商会话，在连接建立和合成操作中共享一个两次重试的预算，延迟为 500 ms 和 1 s。第一次写入 PCM 之前的阻塞式建立/合成操作被限制在 30 秒内。输入只保留到音频开始播放为止（最多 256 KiB）；失败的尝试会在重放之前被取消并关闭，且不能结束共享的播放接收端。一旦有任何 PCM 被提交播放，失败就会直接返回，不会重放语音。音频开始后，提供商特定的合成/播放超时仍然适用。
- **决策模型：** TypeSafe、OpenAI、Gemini 和 OpenRouter 共享两次重试，延迟为 400 ms 和 800 ms，总预算为 15 秒（或更短的调用方截止时间）。
- **实时语音：** OpenAI、Qwen、Gemini Live、xAI、Moss 和 Speko 会将失败的连接/会话建立重试两次，延迟为 500 ms 和 1 s。WebSocket 升级失败会保留 HTTP 状态和 `Retry-After`；建立超时和结构化的瞬时服务端错误可以重试。建立过程仍受调用方连接截止时间以及适配器握手/就绪超时的约束。嵌套的 mint 和 WebSocket 请求共享建立预算，Speko 在多次尝试之间保留相同的 mint 幂等键。活动会话中的音频、工具结果和对话轮次从不自动重放。

所有这些路径都会遵守更长的有效 `Retry-After`，并在调用方取消时立即停止。未知的提供商业务代码除非经过显式映射，否则都被视为永久失败；仅凭错误消息文本不会触发重试。

### Moonshot Kimi K3

使用专用的 `kimi`（国际站）或 `kimi-cn`（中国大陆站）提供商。它们都内置了 Moonshot 的 OpenAI 兼容端点，因此只需要 `model` 和 API key。`kimi-k3` 的上下文窗口和最大输出已在内置注册表中，因此元数据覆盖项可以保持未设置。

Moonshot 官方端点实现的是兼容 OpenAI 的 Chat Completions，而不是 Responses API。请保持 `api_mode` 未设置（或设置为 `chat_completions`）。如果某个第三方协议转换网关确实暴露了 `/responses`，可以将其配置为自定义 `openai` 提供商。Kimi 思考模型要求在多轮和工具调用请求之间保留思考内容，因此 Aico 会通过共享的兼容传输重放每个返回的 assistant `reasoning_content` 字段。参见官方的 [Kimi Thinking Models](https://platform.kimi.ai/docs/guide/use-thinking-models) 指南。

```toml
# Global site (https://api.moonshot.ai/v1)
[model_settings.model]
provider = "kimi"
model = "kimi-k3"
api_key = "MOONSHOT_API_KEY"

# Mainland China site (https://api.moonshot.cn/v1)
# [model_settings.model]
# provider = "kimi-cn"
# model = "kimi-k3"
# api_key = "MOONSHOT_API_KEY"
```

### 火山方舟（豆包）

使用 `volcengine` 提供商。它指向方舟的 OpenAI 兼容端点（`https://ark.cn-beijing.volces.com/api/v3`），因此只需要 `model` 和 API key。`model` 是方舟模型 ID，`api_key` 是方舟 API key。

```toml
[model_settings.model]
provider = "volcengine"
model = "doubao-seed-2-1-pro-260628"
api_key = "ARK_API_KEY"
```

方舟支持带 `store=true` 和 `previous_response_id` 的 `responses_stateful`；`caching` 是另一个可选的成本/延迟功能，存储式响应串联并不需要它。方舟的请求 schema 与 OpenAI 并不完全相同：Aico 会保留受支持的 `instructions`、`include` 和 `reasoning` 字段，但不会向方舟发送 OpenAI 的压缩数组或 `truncation` 字段。设置 `responses_context_management = "ark_context_edit"` 可发送方舟对象形式的 `context_management` 编辑 schema。默认编辑会在 10 次工具调用后清除所有旧的工具输入，同时保留最近的 3 次；可选的 `clear_thinking` 编辑还会删除之前的思考轮次。以 `type = "openai"` 和标准方舟主机保存的旧版提供商记录会被自动识别，并使用相同的兼容性配置。

如果要从环境变量读取密钥而不是写在这里，请把它放在一个具名提供商上并引用它：

```toml
[model_settings.providers.ark]
type = "volcengine"
api_key = "$ARK_API_KEY"

[model_settings.model]
provider = "ark"
model = "doubao-seed-2-1-pro-260628"
```

方舟还在 `/api/compatible` 提供了 Anthropic 协议端点。本 Agent 始终使用 OpenAI 兼容协议，因此请使用上面的 `/api/v3` 路径。

`reasoning_effort` 接受 `minimal`（无推理）、`low`、`medium` 和 `high`。方舟将省略的值视为 `high`，这会让第一个流式 token 延迟数秒；因此内置注册表为 `doubao-seed-2-1-pro-260628` 固定了默认值 `low`，以保持语音回复的响应速度。显式设置该字段即可覆盖。注意 `none` 不是方舟的等级——请使用 `minimal`。

`doubao-seed-2-1-pro-260628` 的上下文窗口和最大输出已在内置注册表中，因此元数据覆盖项可以保持未设置。其他带日期的版本没有注册；对于这些版本，请显式设置 `context_window` 和 `model_max_output_tokens`，或者添加一个以确切方舟模型 ID 为键的注册表条目。

`[voice_settings.classic.tts]` 小节中有一个同样名为 `volcengine` 的无关提供商。它使用独立的 WebSocket 协议，有自己的主机和凭据，因此方舟的 API key 和 Base URL 不能用于它。

### DeepSeek

使用 `deepseek` 提供商访问 DeepSeek 的 OpenAI 兼容端点。Aico 预置了 `deepseek-flash`，它支持视觉和工具调用，上下文窗口为 1M。`deepseek-v4-pro` 预设被注释掉了，因为它不支持视觉；在验证视觉支持后可以恢复。在 Aico 中，DeepSeek 默认使用非思考模式，即 `reasoning_effort = "none"`。将 `reasoning_effort` 设置为 `low`、`high` 或 `max` 可启用思考。在 Chat Completions 模式下，Aico 会发送 DeepSeek 的 `thinking` 开关，并在后续工具调用请求中从对话记录重放 assistant 的 `reasoning_content`。在 Responses 模式下，它会发送 `reasoning.effort`，并通过 Aico 的本地上下文重放返回的推理、消息和已执行的函数调用项。由于 DeepSeek 在思考模式下不应用温度，Aico 在两种传输中都会省略该字段，而在非思考模式下保留它。

设置 `api_mode = "responses"` 可使用 DeepSeek 的无状态 `/responses` 端点。DeepSeek 不支持 `responses_stateful`、`store` 或 `previous_response_id`；因此 Aico 会保留并重新提交完整的本地对话记录。该提供商使用内置端点，因此 `base_url` 不可配置。

```toml
[model_settings.providers.deepseek-main]
type = "deepseek"
api_key = "$DEEPSEEK_API_KEY"

[model_settings.model]
provider = "deepseek-main"
model = "deepseek-flash"
api_mode = "responses"
```

官方参考：[模型能力与限制](https://api-docs.deepseek.com/quick_start/pricing)、
[视觉](https://api-docs.deepseek.com/guides/vision)、
[思考模式下的工具调用](https://api-docs.deepseek.com/guides/thinking_mode)、
[Responses API 兼容性](https://api-docs.deepseek.com/guides/responses_api)。

## `[model_settings.decision]`

可选的决策模型，用于在确定性工作流中回答固定选项的问题（选择题、是/否题、图像中的框选）。它与主模型相互独立：只返回类型化的答案，不做规划，也不输出面向用户的文本。它默认关闭；开启需要 `api_key`。关闭时，所有决策都由主模型完成。同一时间只有一个提供商生效；其密钥独立于主模型的密钥。支持以下提供商：

- `typesafe`：TypeSafe 的 System One API（Jev）。可回答选择、是/否和框选问题，因此也能定位点按目标。
- `openai`：OpenAI 的 Decisions API（GPT-6 Luna，公开测试版）。可回答选择和是/否（谓词）问题，但没有框选答案，因此点按目标类决策（`app_search`、`paste_menu`、Android 搜索框）始终由主模型完成。
- `gemini`：Google 的 Gemini Decisions API（`gemini-decision-1`）。可回答选择、是/否和评分问题，但没有框选答案，因此同样的点按目标类决策仍由主模型完成。
- `openrouter`：OpenRouter 原生的 `/api/alpha/decisions` API。默认使用 Jev（`~typesafe/jev-latest`），目前解析为 Jev 1.14，可回答选择、是/否、评分和框选问题。其他模型来自 [Decisions 模型目录](https://openrouter.ai/models?output_modalities=decisions&order=top-weekly)。只提供 Extractions 的条目会被排除。非 Jev 模型和 1.14 之前的 Jev 版本不处理框选问题，点按目标类决策交给主模型。截图类决策需要图像输入；不支持的能力保留现有的回退行为。原生 Decisions 端点目前是 alpha API。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled`        | `false`                      | 开启决策模型。需要 `api_key`。 |
| `provider`       | `typesafe`                   | `typesafe`（TypeSafe Jev）、`openai`（OpenAI Luna）、`gemini`（Gemini Decision 1）或 `openrouter`。 |
| `api_key`        | 空                        | 提供商 API key。Config Web 将其视为只写字段，并报告 `has_api_key`。 |
| `base_url`       | 提供商默认值             | 提供商端点；必须是绝对的 `http(s)` URL。默认分别为 `https://api.typesafe.ai/v1`、`https://api.openai.com/v1`、`https://generativelanguage.googleapis.com/v1beta` 或 `https://openrouter.ai/api/alpha`。 |
| `model`          | 提供商默认值             | 决策模型名称。默认分别为 `jev-preview`（Jev 1.14，框选问题需要它）、`gpt-6-luna`、`gemini-decision-1` 或 `~typesafe/jev-latest`。 |
| `min_confidence` | `0.8`                        | 置信度低于此值（0 到 1）的答案会交还给主模型。是/否答案在 `p >= min_confidence` 或 `p <= 1 - min_confidence` 时视为置信。`0` 表示使用默认值。 |

```toml
[model_settings.decision]
enabled = true
api_key = "ts-..."
```

对于 OpenRouter，请使用单独的 OpenRouter 密钥：

```toml
[model_settings.decision]
enabled = true
provider = "openrouter"
api_key = "sk-or-..."
# model = "~typesafe/jev-latest"
# base_url = "https://openrouter.ai/api/alpha"
```

下面的每个决策都会同时询问两个模型。达到 `min_confidence` 的决策模型答案一到达就会被采用，并取消主模型调用；否则使用主模型的答案，因此不确定的决策模型不会增加延迟。

| 日志中的 Op | 决策 | 决策模型可以决定的内容 |
| --- | --- | --- |
| `ime_probe`             | 输入探测字母后的输入法状态  | 该状态 |
| `probe_cleanup`         | 探测字母是否仍在输入框中    | 是否删除它 |
| `screen_analysis`       | 输入的文本是否匹配、是否有待上屏的组字 | 仅匹配和待上屏标志；会等待两个模型的结果，输入框文本始终来自主模型 |
| `app_search`            | 搜索页面上的直接应用结果及其点按位置 | 仅限置信的“找到”且带有置信的点按框；“未找到”始终交给主模型 |
| `app_open_confirmation` | 应用是否已打开 | 仅限置信的“已打开”；否则由主模型决定 |
| `android_search_entry`  | Android 启动器搜索是否已打开 | 答案和搜索框位置 |
| `paste_menu`            | 长按菜单中的“粘贴”操作 | 点按位置 |

这些决策是在设备上的运行（2026-10，iOS 和 Android）中，决策模型的置信答案与主模型一致或纠正了主模型的那些。没有从中获益的决策，例如输入法候选词选择、对话框检查、循环防护建议和记忆保留，不会发送给决策模型。

`app_open_confirmation` 只信任“已打开”：在应用切换的过渡过程中（屏幕变暗、“正在打开应用”浮层），Jev 曾经很有把握地错误判断为“未打开”，但从未在判断“已打开”时出错。否则，由主模型比较点按结果时所在的搜索页面和点按后的屏幕，并回答屏幕是否离开了搜索页面，无需识别应用本身：设置在其自身搜索页面重新打开、Safari 的起始页，仅凭第二张截图都会被判断为“未打开”。`app_search` 只信任“找到”：在结果稳定之前或应用已经打开之后截取的截图会被判断为“未找到”，此时由主模型重新检查。

`step_gate` op 有所不同：它没有对应的主模型调用。它在多步 `quick_action` 的步骤之间进行检查。`quick_actions.json` 中的步骤可以声明 `expect`，描述该步骤应该留下的屏幕状态；例如，`spotlight_search` 的第一步期望在 Cmd+A 和 Backspace 清空其输入框之前，Spotlight 已经打开。决策模型开启时，运行时会在该步骤之后截图，并询问截图是否显示了预期状态。批次执行中途无法回退到主模型，因此只有置信的“是”才会继续；“否”或不确定的答案会以 `step_expectation_unmet` 停止序列，并列出已运行的步骤。该结果不带截图，因此主模型会在继续之前先截一张图。请求或截图失败，或决策模型已禁用时，序列不做检查。最后一步上的 `expect` 没有效果。

`touch_gesture` 的 `tap_sequence` 也会记录 `step_gate`。它的像素检查仍会在两次点按之间运行；当发现剩余目标发生变化（而不是整屏变化，整屏变化总是会停止）时，决策模型会收到规划点按时的帧和变化后的帧，并被询问剩余目标是否仍是相同的控件、仍在原位。置信的“是”会再用一张新帧复核，只有当屏幕没有再次变化时，序列才会以该帧为新基线继续；其他任何情况（包括决策模型已禁用）都会像以前一样停止序列。step-gate 请求限时 4 秒；失败或超时的请求视为未检查。

每个决策都会在 `decision` 组件下写入一行 Agent 日志：
`op=... provider=... dm={...} conf=... llm={...} agree=... dm_ms=... llm_ms=... fallback=...`，
其中 `dm` 是决策模型的答案；当主模型的答案也被使用时，会填写 `llm` 和 `agree`。

每个用户轮次还会在截取该轮截图（`turn_screenshot`）的同时，根据话语文本和之前的一次交互（上一个请求、它使用的工具以及回复）询问决策模型一次：回应是否需要手机屏幕？之前的交互使得像打开应用后说“现在按等号”这样的追问会被判断为需要屏幕。只有置信的“否”（例如闲聊、算术或天气）才会从该轮中去掉截图；主模型仍然可以调用 `screenshot`。该请求限时 4 秒，不确定的答案、失败的请求或已禁用的决策模型都会保留截图。

同一个请求还会决定哪些已保存的记忆与该请求有关（`memory_recall`）。画像、偏好和规则记忆已经通过系统提示词中的用户画像传给主模型，因此只提供事实和操作流程记忆：最多提供优先级最高的 20 条，其中最多 5 条被置信判定为相关的记忆会被加入该轮的状态消息。敏感记忆以及由通知写入的记忆永远不会以这种方式提供，因为通知文本由第三方控制；`recall_memory` 仍然可以按主题找到它们，以及任何没有被自动召回的记忆。不确定的答案或失败的请求不会召回任何内容。提供的记忆文本会被发送给决策模型提供商。

TypeSafe API 有地区限制（在受支持地区之外返回 HTTP 451）；Agent 通过与其他模型提供商相同的代理环境访问所有决策提供商。

在 Config Web 中，**Models** 列出了主模型和决策模型。决策模型页面可以编辑开关、提供商、密钥和最低置信度，**Test connection** 会用已保存的密钥发送一个小问题（`POST /api/config/test`，section 为 `decision`），即使开关关闭也可以。切换提供商会清除已保存的密钥、关闭模型，并将 Base URL 和模型重置为新提供商的默认值。OpenRouter 页面提供一个带有原生 Decisions 目录建议的模型 ID 输入框，以及一个单独的全宽 Base URL 输入框。目录只用于辅助输入；建议之外的 ID 也会被接受，即使目录无法加载。清空模型 ID 可恢复默认的 Jev 模型；Base URL 留空则恢复官方端点。目录通过 OpenRouter 的公开 Models API 以 `output_modalities=decisions` 和 `order=top-weekly` 获取，不会发送任何已保存的 API key；条目保持该 API 返回的顺序。其他提供商的 Base URL 和模型覆盖项需在 TOML 文件中编辑。

SkillOpt 样本分类器（`--sample-classifier typesafe`）可以离线使用 Jev；它从 `TYPESAFE_API_KEY` 读取密钥。

## `[advanced_settings.log]`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `level`                   | `info`  | Agent 日志的最低级别：`debug`、`info`、`warn` 或 `error`。更改无需重启 Agent 即可生效。 |
| `llm_http_retention_days` | `7`     | 在 `<config_dir>/log` 下保留原始 LLM HTTP 日志（`llm-http-*.log`）的天数。清理在 Agent 启动时运行；未设置或为 `0` 时使用默认值。 |
| `log_raw_http`            | `true`  | 记录详细的模型 HTTP 请求和响应。它对应运行时的模型传输日志记录器，但存放在高级设置的日志分组中。 |

## `[voice_settings.classic.audio]`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `socket`           | `/run/audio_service/audio_service.sock` | Audio Service socket |
| `sample_rate`      | `16000`                                 | 采样率 |
| `channels`         | `1`                                     | 声道数 |
| `bit_width`        | `16`                                    | 位宽 |
| `backend`           | `auto`                                  | 录音和播放后端。`auto` 在开发板上使用 `audio_service`，在桌面/PC 模式下通过 ADB 输入后端或环境桥接使用主机的录音/播放命令。使用 `audio_service` 或 `local` 可强制两个方向使用同一个后端。 |

## `[voice_settings.realtime]`

本小节选择 GPIO 唤醒后使用的具名实时语音提供商记录。它在 `input_mode = "realtime"` 时生效；决定守护进程是否启动实时语音路径的是模式，而不是是否存在 API key。守护进程将麦克风 PCM 流式发送给所选适配器，并通过开发板音频路径播放其响应 PCM。
对于具备文本输入能力的提供商，`/api/chat` 可以启动会话，并将排队的文本作为其第一条用户消息发送。Speko S2S 以音频为先；它所委托的原生提供商可能暴露可选的文本能力，但开发板麦克风路径仍是标准的全双工路径。所选的直连提供商负责轮次检测和打断；Speko 不中继 PCM，也不会另外合成一个 VAD 循环。
使用 `input_mode = "stt"` 可选择现有的 VAD/STT/LLM/TTS 唤醒循环。
当 `agent.input_mode = "realtime"` 时，Config Web 会渲染选择器。提供商凭据和模型设置位于 `[voice_settings.realtime.providers.<name>]` 中，因此切换选择器永远不会覆盖其他提供商已保存的配置。
当前的适配器包括 Qwen、Speko S2S、OpenAI Realtime、Google Gemini Live 和 xAI Grok Voice。
会话始终以内置的实时语音指令开始。如果 `[conversation_settings.agent].prompt` 非空，它会被追加在该基础指令之后，而不会替换它。

| 字段 | 默认值 | 说明 |
| ----- | ------- | ----------- |
| `provider` | `qwen` | 具名的 `[voice_settings.realtime.providers.<name>]` 记录。为了兼容，仍接受裸值 `qwen`、`speko`、`openai`、`gemini`、`xai` 或 `moss`。 |
| `enable_speech_emotion` | `true` | 启用实时语音情感。 |
| `input_audio_format` / `output_audio_format` | `pcm` | 实时语音 API 接受的音频格式。 |

本小节下旧版的 Qwen `turn_detection*` 字段会在加载配置或下次保存时迁移到所选的 Qwen 提供商记录中。

## `[voice_settings.realtime.providers.<name>]`

实时语音提供商记录遵循与 `[model_settings.providers.<name>]` 相同的具名记录模式。每种实时语音提供商的多个配置可以共存；`[voice_settings.realtime].provider` 按名称选择其中一个。

```toml
[voice_settings.realtime.providers.qwen-main]
type = "qwen"
api_key = "$DASHSCOPE_API_KEY"
model = "qwen-audio-3.0-realtime-plus"
region = "cn-beijing"
voice = "longanqian"
turn_detection = "server_vad"
turn_detection_threshold = 0.5
turn_detection_silence_ms = 800

[voice_settings.realtime.providers.speko-main]
type = "speko"
api_key = "$SPEKO_API_KEY"
upstream_provider = "google"
model = "gemini-3.1-flash-live-preview"
voice = "Puck"

[voice_settings.realtime.providers.gemini-vertex]
type = "gemini"
auth_mode = "vertex"
api_key = "$GOOGLE_OAUTH_ACCESS_TOKEN"
project_id = "my-project"
location = "us-central1"
model = "gemini-live"
voice = "Puck"

[voice_settings.realtime.providers.openai-gateway]
type = "openai"
api_key = "$OPENAI_API_KEY"
model = "gpt-realtime"
endpoint = "wss://gateway.example/v1/realtime"
realtime_protocol = "legacy"
voice = "alloy"

[voice_settings.realtime.providers.moss-main]
type = "moss"
api_key = "$MOSS_API_KEY"
model = "moss-live-1.0"

[voice_settings.realtime]
provider = "speko-main"
```

| 字段 | 提供商 | 说明 |
| ----- | --------- | ----------- |
| `type` | 全部 | 适配器类型：`qwen`、`speko`、`openai`、`gemini`、`xai` 或 `moss`。 |
| `api_key` | 全部 | 提供商凭据；支持 `$ENV_VAR` 展开。对于 Gemini Vertex，这里是 OAuth access token。 |
| `model` / `voice` | 全部 | 提供商特定的模型和音色。Speko 要求显式指定模型；音色可以留空，以使用所选上游的默认值。Moss Live 使用其默认人设，忽略 `voice`。 |
| `workspace_id` / `region` | Qwen | 可选的 DashScope 路由设置。 |
| `turn_detection` | Qwen | Qwen 轮次检测器：`server_vad`（默认）或 `smart_turn`。Config Web 在该提供商的高级设置中暴露此项。 |
| `turn_detection_threshold` | Qwen `server_vad` | 可选的 Qwen VAD 阈值；不设置则使用服务默认值。`smart_turn` 会忽略此项。 |
| `turn_detection_silence_ms` | Qwen `server_vad` | 可选的 Qwen 结束一个轮次之前的静音时长；不设置则使用服务默认值。`smart_turn` 会忽略此项。 |
| `auth_mode` | Gemini | `api_key`（默认）表示使用 Gemini Developer API，`vertex` 表示使用 Vertex OAuth。 |
| `project_id` / `location` | Gemini Vertex | 必填的 Google Cloud 项目和 Vertex 区域，例如 `us-central1`。 |
| `endpoint` | Qwen, OpenAI, Gemini, xAI | 可选的 WebSocket 端点覆盖，主要用于区域网关和协议测试。 |
| `realtime_protocol` | OpenAI | OpenAI Realtime 线路 schema：空值或 `ga`（默认）使用当前 GA 会话负载；`legacy`（别名 `beta`）使用某些兼容网关所需的较旧的 `modalities`、`input_audio_format` 和 `output_audio_format` 字段。请显式设置；永远不会根据端点 URL 推断协议。 |
| `use_backend_agent` | 实时模式（`[voice_settings.realtime]`） | 控制工具归属：省略/`true`（默认）会启用后台 Agent 并暴露其通信工具（`*_agent_task`）；省略时如果后台模型（`[model_settings.model]`）没有可用凭据，运行时会回退为 `false`，以免委派的任务失败；`false` 会禁用 Agent 通信工具，并将原本由后台 Agent 直接使用的运行时工具暴露给实时语音模型。此设置不控制提供商的推理，推理仍是提供商/模型的能力。Moss Live 始终以有效值 `false` 运行（不注入任务更新文本），但不改写已存储的值，会隐藏 Config Web 开关，并且在没有后台 Agent 运行时仍将客户端工具上限设为 32 个（目录更大时丢弃优先级较低的工具）。 |
| `thinking_level` | Gemini | Live Extended Thinking 模型的思考深度：`LOW`（默认）、`MINIMAL`、`MEDIUM` 或 `HIGH`。只适用于支持思考的模型；其他模型会忽略它。 |
| `upstream_provider` | Speko | 必填的 S2S 上游：`google`（或 `gemini`）或 `xai`，与 `model` 配对。自动路由已禁用，因为它可能选择不受支持的 WebRTC 路由。在 Aico 中，OpenAI 不是受支持的 Speko 路由；请改用顶层的 `openai` 提供商。 |
| `agent_id` / `base_url` | Speko | 可选的 Speko agent ID 和 API Base URL 覆盖。 |

## `[advanced_settings.hardware.frame_service]`

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `keep_streamon` | `false` | 为 `true` 时，Frame Service 在两次截图之间保持采集 STREAMON，并为每个请求丢弃 6 个预热帧。为 `false` 时，它会在两次截图之间暂停，并使用 0 个预热帧。 |

## `[memory_settings.notification]`

语音通知会将系统提醒附加到正常的语音回复中，或用固定的错误消息替换最终失败的 LLM 轮次。在 Realtime 模式下，空闲的会话还可以将待处理的提醒作为私有语音响应播报出来。生命周期和投递约定参见 [语音通知](voice-notifications.md)。

```toml
[memory_settings.notification]
enabled = true
max_pending = 8
retention_days = 14

[memory_settings.notification.response_tail]
enabled = true
max_items = 1
max_text_chars = 40

[memory_settings.notification.expiration]
default_ttl_seconds = 0

[memory_settings.notification.expiration.code_ttl_seconds]
storage = 900
```

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled`                            | `true`          | 启用持久尾部提醒和最终轮次替换 |
| `max_pending`                        | `8`             | 内存管理器保留的有效条件记录的最大数量 |
| `retention_days`                     | `14`            | 正常存储清理期间保留已处理通知记忆的天数 |
| `response_tail.enabled`              | `true`          | 允许将持久提醒追加到正常回复中 |
| `response_tail.max_items`            | `1`             | 每条回复的最大提醒数；当前实现只支持 `1` |
| `response_tail.max_text_chars`       | `40`            | 提醒的最大长度（Unicode 字符数） |
| `expiration.default_ttl_seconds`     | `0`             | 默认的有效条件租约；`0` 表示禁用自动过期 |
| `expiration.code_ttl_seconds.<code>` | `storage = 900` | 按代码覆盖的租约，每次有效心跳都会续期 |

Config Web 在记忆设置中暴露 `retention_days`。生命周期和租约字段仍可通过手动编辑 TOML 修改。

## `[basic_settings.device]`

| 字段 | 默认值 | 说明 |
| ------------- | ------- | ----------- |
| `device_type` | `iOS`   | 用于 USB HID 描述符和 Agent 全局设备状态的目标主机类型。可接受的值：`iOS`、`Android`、`macOS`、`windows`、`linux`。`iOS` 推导出 HID `pointer_mode = "touchpad"`（需要在 iPhone 上开启缩放并关闭辅助触控），`Android` 推导出 `"touchscreen"`，`macOS`、`windows` 和 `linux` 推导出 `"absolute"`。改变指针模式的更改需要重启，以便重新枚举 USB 描述符；在 `macOS`、`windows` 和 `linux` 之间切换则在线生效。 |

## `[basic_settings.device.hid]`

| 字段 | 默认值 | 说明 |
| ----------------- | -------- | ----------- |
| `keyboard_layout` | `qwerty` | 手机如何解释外接 USB HID 键盘：`qwerty`、`azerty` 或 `qwertz`。参见 [USB HID](../04-services/usb-hid.md)。 |

## `[advanced_settings.hardware.hid]`

保留此表用于内部 HID 设备设置以及与旧配置的兼容。请将面向用户的 `keyboard_layout` 值放在 `[basic_settings.device.hid]` 下；两个表同时存在时，以基本设置中的值为准。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `keyboard_device`         | `/dev/hidg0`                            | 键盘 HID 设备 |
| `mouse_device`            | `/dev/hidg1`                            | 鼠标/触摸 HID 设备 |
| `android_keyboard_device` | `/dev/hidg2`                            | Consumer Control HID 设备（`hid.usb2`），在 `[basic_settings.device].device_type = "Android"` 时用于 Android 扩展按键，在其他设备类型下用于媒体/音量/亮度/截图按键 |
| `frame_socket`            | `/run/frame_service/frame_service.sock` | 截图工具使用的 Frame Service socket |
| `input_backend`           | `hid`                                   | 点击/触摸/键盘工具的底层输入后端。`hid` 写入 USB HID 报告；`adb` 使用已配对的 Android ADB 连接以及 `adb shell input`/ADBKeyboard 命令。原子触摸程序还会尝试 `getevent`/`sendevent`，在原始事件注入被阻止时回退到 `input touchscreen motionevent`。 |

## `[voice_settings.classic.tts.providers.<name>]` 和 `[voice_settings.classic.stt.providers.<name>]`

具名的语音提供商配置，形式与 `[model_settings.providers.<name>]` 之于 `[model_settings.model]` 相同。每个小节保存一个语音服务的凭据和设置，`[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` 通过在自己的 `provider` 字段中填写名称来引用其中一个。可以同时配置多个提供商，切换只需修改一行，而不必重新输入密钥。

与 `[model_settings.providers.<name>]` 不同，这些是独立的命名空间：`[voice_settings.classic.tts]` 的 `volcengine` 提供商与同名的方舟 LLM 提供商使用不同的协议，有自己的主机和凭据，因此一个映射无法同时服务两者。每个命名空间也各自校验自己的提供商类型——TTS 类型在 `[model_settings.model]` 中会被拒绝，反之亦然。

多条记录可以共用一个提供商类型，这样同一服务的两个账号（不同的密钥、不同的音色）就可以同时保持配置。

```toml
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "sk-aaa"
voice_id = "male-qn-qingse"

[voice_settings.classic.tts.providers.minimax-alt]     # same type, second account
type = "minimax"
api_key = "sk-bbb"
voice_id = "female-shaonv"

[voice_settings.classic.tts.providers.fish]
type = "fish-audio"
api_key = "$FISH_API_KEY"
reference_id = "abc123"

[voice_settings.classic.tts]
provider = "minimax-main"       # references [voice_settings.classic.tts.providers.minimax-main]
speed = 1.0

[voice_settings.classic.stt.providers.tencent]
type = "tencent-asr"
app_id = "123"
secret_id = "AKID..."
secret_key = "..."
region = "ap-shanghai"

[voice_settings.classic.stt]
provider = "tencent"
language = "zh"
```

### 字段放置

当某个字段在更换提供商后就失去意义时，它放在记录上；当它与提供商无关时，则保留在 `[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` 上。

| | 记录字段 | 保留在共享小节上 |
| ---- | ---- | ---- |
| TTS | `type`、`api_key`、`model`、`voice_id`、`emotion`、`reference_id` | `provider`（引用）、`speed` |
| STT | `type`、`api_key`、`model`、`base_url`、`app_id`、`secret_id`、`secret_key`、`region`、`engine_model_type` | `provider`（引用）、`language` |

`speed` 是收听偏好，`language` 是转写偏好：两者都不应因为音色改变而改变，因此都保持全局。

对于所有提供商记录，`api_key` 接受字面量密钥或 `$VAR_NAME`。Config Web 存储的表示形式完全相同。

### 提供商配置兼容性

Config Web 的请求契约只接受标准的提供商记录。记录使用 `type`，所选记录的名称仍保留在父小节的 `provider` 字段中。已停用的记录别名和扁平的凭据请求字段会被拒绝。现有的 TOML 文件在保存时仍可能被规范化，以便在将文件转换为标准布局的同时保留凭据。

无法解析的引用不会阻止设备启动：语音在运行时是可选的，因此过期的名称会被报告，Agent 会在没有语音的情况下启动。Config Web 会在保存时、表单仍在屏幕上时拒绝这样的引用。

## `[voice_settings.classic.stt]` 和 `[voice_settings.classic.tts]`

当 `input_mode = "stt"` 时，`[voice_settings.classic.stt]` 是必需的；当 `input_mode = "stt"` 时，`[voice_settings.classic.tts]` 是必需的。

这里的 `provider` 是对某条 `[voice_settings.classic.tts.providers.<name>]` / `[voice_settings.classic.stt.providers.<name>]` 记录的引用（裸提供商类型仍然可用——见上文）。下面列出的提供商特定凭据位于该记录上；Config Web 在提供商对话框中编辑它们，而不是在 `[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` 卡片上。

STT：

- `provider = "openai-whisper"`：当前可用；
- `provider = "openrouter"`：当前可用，默认端点为 `https://openrouter.ai/api/v1/audio/transcriptions`，请求体使用 base64 WAV；
- `provider = "tencent-asr"`：腾讯云一句话识别（SentenceRecognition），使用 `secret_id` / `secret_key`，不需要 `base_url`；旧值 `tencent` / `tencent_asr` 仅作为兼容别名保留；
- `provider = "qwen-asr"`：基于 WebSocket 的阿里云 DashScope 实时 ASR，默认模型 `qwen3-asr-flash-realtime`；支持流式上传，并接受可选的 `model` / `base_url` 覆盖；
- `provider = "google-cloud"`：使用 API key 认证的 Google Cloud Speech-to-Text REST API，默认端点 `https://speech.googleapis.com/v1/speech:recognize`；接受可选的 `model` / `base_url` 覆盖，不支持流式上传。

TTS：

- `provider = "ekho"`：本地 Ekho CLI（内置默认值），普通话，支持中英混合；不需要 API key。安装要求、句子缓冲和语速行为参见 [本地 Ekho](audio-features.md#本地-ekho默认)；
- `provider = "minimax"`：Minimax WebSocket，国际端点 `api.minimax.io`；
- `provider = "minimax-cn"`：Minimax WebSocket，中国大陆端点 `api.minimaxi.com`；
- `provider = "fish-audio"`：Fish Audio WebSocket；
- `provider = "alicloud"`：阿里云 Qwen-TTS Realtime；
- `provider = "volcengine"`：火山引擎 WebSocket 双向流式 V3。目前只支持新控制台的 `X-Api-Key` 认证：`api_key` 对应 `X-Api-Key`，`model` 对应 `X-Api-Resource-Id`（默认 `seed-tts-2.0`），`voice_id` 对应发音人；
- `provider = "openrouter"`：OpenRouter HTTP 语音 API，默认模型 `google/gemini-3.1-flash-tts-preview`；`model` 和 `voice_id` 选择路由到的 TTS 模型及其音色；
- `provider = "google-cloud"`：使用 API key 认证的 Google Cloud Text-to-Speech REST API；`voice_id` 默认为 `en-US-Neural2-C`。

TTS 配置字段：

| 字段 | 位置 | 说明 |
| --- | --- | --- |
| `provider`     | `[voice_settings.classic.tts]`                          | 必填，引用一条具名提供商记录；为了向后兼容，仍支持裸提供商类型 |
| `api_key`      | `[voice_settings.classic.tts.providers.<name>]`         | 云端提供商必填；本地 Ekho 不使用 |
| `model`        | `[voice_settings.classic.tts.providers.<name>]`         | 可选的 Minimax 模型、Fish Audio 模型请求头、阿里云 Realtime 模型、火山引擎 `X-Api-Resource-Id` 或 OpenRouter 模型 |
| `voice_id`     | `[voice_settings.classic.tts.providers.<name>]`         | 可选的 Minimax、阿里云、OpenRouter 或 Google Cloud 音色；火山引擎发音人。Fish Audio 不使用（参见 `reference_id`） |
| `reference_id` | `[voice_settings.classic.tts.providers.<name>]`         | 可选的 Fish Audio reference id；默认为 Config Web 中显示的内置演示音色。其他提供商会忽略 |
| `emotion`      | `[voice_settings.classic.tts.providers.<name>]`         | 可选的 Minimax 情感；火山引擎将其作为 `audio_params.emotion` 透传，需要音色支持 |
| `speed`        | `[voice_settings.classic.tts]`                          | 可选的语速，默认 `1.0`；支持的范围因提供商而异，请参考官方文档 |

示例使用占位密钥，以明确所需的记录位置。

常见的 TTS 适配器配置：

| 提供商 | `model` 示例 | 音色/参考字段 | 说明 |
| --- | --- | --- | --- |
| `minimax`      | `speech-2.8-hd`                           | `voice_id = "male-qn-qingse"`                       | 通过 `api.minimax.io` 的 Minimax WebSocket；`emotion` 会透传给 Minimax |
| `minimax-cn`   | `speech-2.8-hd`                           | `voice_id = "male-qn-qingse"`                       | 通过 `api.minimaxi.com` 的 Minimax WebSocket；`emotion` 会透传给 Minimax |
| `fish-audio`   | `s2-pro`                                  | `reference_id = "98655a12fa944e26b274c535e5e03842"` | WebSocket 实时 TTS；默认使用所示的 reference，不使用 `voice_id` |
| `alicloud`     | `qwen3-tts-flash-realtime`                | `voice_id = "Cherry"`                               | DashScope Realtime；适配器输出 24 kHz PCM，采样率不同时会自动重采样 |
| `volcengine`   | `seed-tts-2.0`                            | `voice_id = "zh_female_vv_uranus_bigtts"`           | `model` 对应 `X-Api-Resource-Id`，`voice_id` 对应发音人，两者必须匹配 |
| `openrouter`   | `google/gemini-3.1-flash-tts-preview`     | `voice_id = "Kore"`                                 | OpenRouter `/audio/speech`；默认音色取决于所选模型，输出为 24 kHz PCM |
| `google-cloud` | —                                         | `voice_id = "en-US-Neural2-C"`                      | 使用 API key 认证的 Google Cloud Text-to-Speech REST API；输出为 24 kHz PCM |

### 提供商示例

Minimax WebSocket：

```toml
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "..."
model = "speech-2.8-hd"
voice_id = "male-qn-qingse"
emotion = "happy"

[voice_settings.classic.tts]
provider = "minimax-main"
speed = 1.0
```

Fish Audio WebSocket：

```toml
[voice_settings.classic.tts.providers.fish-main]
type = "fish-audio"
api_key = "..."
model = "s2-pro"
reference_id = "98655a12fa944e26b274c535e5e03842"

[voice_settings.classic.tts]
provider = "fish-main"
speed = 1.0
```

Fish Audio 的 `model` 默认为 `s2-pro`，作为 WebSocket 握手请求头发送。`reference_id` 为空时使用 Config Web 中显示的内置演示音色；在所选的 `[voice_settings.classic.tts.providers.<name>]` 记录上配置 `reference_id` 即可覆盖。Fish Audio 不使用 `voice_id`，会将其忽略（这样可以避免继承为其他提供商设置的 `voice_id`）。在某些网络中，公共 Fish Audio 端点可能需要在 `/userdata/system/env` 中设置 `ALL_PROXY` 或 `HTTPS_PROXY`。

阿里云 Qwen-TTS Realtime：

```toml
[voice_settings.classic.tts.providers.alicloud-main]
type = "alicloud"
api_key = "..."
model = "qwen3-tts-flash-realtime"
voice_id = "Cherry"

[voice_settings.classic.tts]
provider = "alicloud-main"
speed = 1.0
```

阿里云适配器使用 DashScope WebSocket Realtime 端点，输出固定的 24 kHz PCM；当设备播放采样率不同时，会自动重采样。

火山引擎 WebSocket 双向流式 V3：

```toml
[voice_settings.classic.tts.providers.volcengine-main]
type = "volcengine"
api_key = "..."
model = "seed-tts-2.0"
voice_id = "zh_female_vv_uranus_bigtts"

[voice_settings.classic.tts]
provider = "volcengine-main"
speed = 1.0
```

对于火山引擎，`api_key` 是新控制台的 `X-Api-Key`，`model` 是 `X-Api-Resource-Id`，`voice_id` 是发音人。`voice_id` 必须与 `model` 对应的资源匹配；不匹配时，服务端会返回 `resource ID is mismatched with speaker related resource`。`seed-tts-2.0` 的一个已验证可用的音色示例是 `zh_female_vv_uranus_bigtts`。

### 在运行时切换提供商

```bash
curl -X POST http://<device-ip>:8080/api/settings/tts \
  -H 'Content-Type: application/json' \
  -d '{"provider":"volcengine","voice":"zh_female_vv_uranus_bigtts"}'
```

如果需要在同一份配置中保存多个提供商的密钥，请使用具名提供商记录。通过运行时 POST 切换提供商时，会先读取对应的记录，再用请求体覆盖。

```toml
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "..."
model = "speech-2.8-hd"
voice_id = "male-qn-qingse"

[voice_settings.classic.tts.providers.fish-main]
type = "fish-audio"
api_key = "..."
model = "s2-pro"
reference_id = "98655a12fa944e26b274c535e5e03842"

[voice_settings.classic.tts.providers.alicloud-main]
type = "alicloud"
api_key = "..."
model = "qwen3-tts-flash-realtime"
voice_id = "Cherry"

[voice_settings.classic.tts.providers.volcengine-main]
type = "volcengine"
api_key = "..."
model = "seed-tts-2.0"
voice_id = "zh_female_vv_uranus_bigtts"

[voice_settings.classic.tts]
provider = "minimax-main"
speed = 1.0
```

## `[advanced_settings.runtime.live_activity]`

用于 iOS 配套 App 的实时活动 / 灵动岛任务状态。快照默认启用，并通过 BLE Wake 加 USB ECM 在本地投递。参见 [实时活动 / 灵动岛](./live-activity.md)。

| 字段 | 默认值 | 说明 |
| --------- | ------- | ----------- |
| `enabled` | `true`  | 维护本地任务状态快照，并发送合并后的 `live_activity` BLE Wake 通知 |

## Episode 遥测（Langfuse）

可选。任务结束后，异步将完整的 Episode 上报到 Langfuse；详情参见 [telemetry-langfuse.md](./telemetry-langfuse.md)。

```toml
[advanced_settings.runtime.telemetry]
enabled = false
provider = "langfuse"
base_url = "http://langfuse.example.com:3000"
public_key = "pk-lf-..."
secret_key = "sk-lf-..."
upload_screenshots = true
upload_timeout_sec = 30
max_retry = 2
environment = "default"
tags = ["aico-hardware"]
```

## 系统环境变量

Agent 不再从 `agent.toml` 读取 `[proxy]`。`/userdata/system/env` 中的值定义系统/默认上游代理。出站 HTTP/WebSocket 请求、shell 工具子进程、通过 `aico-env-run` 启动的 OTA 命令以及 SSH 登录 shell 都使用固定的本地地址 `127.0.0.1:18080`；它会根据当前 Wi-Fi 已保存的策略选择系统上游、直连模式或自定义上游。该监听端口同时接受 HTTP 代理和 SOCKS5 协议。它生成的环境会让本地 URL 的协议与所选上游保持一致，因此 `socks5://` 的 Wi-Fi 代理在本地端点两侧都保持为 SOCKS5，而不会被包装进 HTTP CONNECT。本地 URL 使用 `socks5h://`，因此主机名解析也会经由 SOCKS5，而不依赖开发板的 DNS。该文件按 shell 语法加载，例如：

```sh
HTTP_PROXY=http://127.0.0.1:7890
HTTPS_PROXY=http://127.0.0.1:7890
NO_PROXY=localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7,fe80::/10
OPENROUTER_API_KEY=...
```

| 变量 | 说明 |
| --- | --- |
| `HTTP_PROXY` / `http_proxy`   | HTTP 代理 URL，例如 `http://127.0.0.1:7890` |
| `HTTPS_PROXY` / `https_proxy` | HTTPS 代理 URL，通常与 HTTP 代理端点相同 |
| `ALL_PROXY` / `all_proxy`     | 没有适用于特定协议的代理时使用的回退代理 |
| `NO_PROXY` / `no_proxy`       | 逗号分隔的绕过规则；设置了代理 URL 但没有绕过值时，启动器会注入默认的私有网络绕过列表 |

Agent 的 HTTP 客户端（包括 Google Cloud 和 OpenRouter TTS 适配器）使用与请求协议对应的代理，并回退到 `ALL_PROXY`。WebSocket 客户端（包括实时 STT 客户端以及 Fish Audio、MiniMax 和火山引擎 TTS 适配器）优先使用 `HTTPS_PROXY`，其次是 `HTTP_PROXY`，然后是 `ALL_PROXY`。它们都会直接连接匹配 `NO_PROXY` 的主机（即使代理是 SOCKS5），并在设置了代理 URL 但没有 `NO_PROXY` 时使用默认的私有网络绕过列表。阿里云 TTS 适配器始终直连。

`web_scraper` 工具会拒绝本身是或解析为回环、私有或链路本地地址的目标，并以同样的方式检查每一次重定向。当请求经由代理转发时，抓取器会用开发板的 DNS 检查目标名称，但代理会再次解析它并自行连接。因此配置的代理是受信任的：即使代理位于回环地址或局域网上，抓取器也可以连接它；如果需要这层保护，应由代理负责拒绝私有、回环和链路本地目标。`127.0.0.1:18080` 上的本地代理及其上游不会过滤目标地址。

按 Wi-Fi 的策略在 Config Web 的连接对话框中配置，并以 `0600` 权限存储在 `/userdata/system/wifi-proxies.json` 中。在系统环境中设置 `AICO_WIFI_PROXY_ENABLED=0` 是一种紧急绕过手段，在受影响的服务或 shell 重启后，会恢复直接使用原始环境代理变量。

所选的本地 URL 会写入 `/run/wifi_proxy/proxy-env`。新的受管命令和登录 shell 会读取该文件。如果 Wi-Fi 变化导致在 HTTP 和 SOCKS5 之间切换，`aico-wifi-proxy.service` 会重启长期运行的 Agent，使其 HTTP 和 WebSocket 客户端也采用新的 URL 协议；仅更改上游主机或端口则不需要重启 Agent。

对于按 Wi-Fi 的自定义代理，对话框中的 `NO_PROXY` 值属于该 SSID 的自定义上游，在自定义策略生效期间会替换环境中的 `NO_PROXY`。系统默认策略同时使用 `/userdata/system/env` 中的代理变量和 `NO_PROXY`；它不存储按 Wi-Fi 的绕过值。

## `memory/extraction.yaml`

可选。在配置目录下放置 `memory/extraction.yaml`，可以控制会话记忆压缩和分块提取。文件缺失或字段无效时回退到默认值。完整流程参见 [session-memory.md](./session-memory.md)。

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `reserve_tokens`                     | `8192`                  | 在当前模型上下文窗口之下预留的 token 余量。当 `prompt_tokens >= context_window - reserve_tokens` 时触发压缩。该值最多被限制为窗口的一半，以保证小窗口模型仍然可用。 |
| `keep_recent_tokens`                 | `20000`                 | 基于 token 的切分点选择所保留的热窗口的大致 token 预算。它会与 `reserve_tokens` 一起被限制，以适应当前窗口。 |
| `hot_window_events`                  | `30`                    | 按数量回退时保留的最近事件目标数量。仅在没有 prompt token 数据时使用。 |
| `count_compress_after_events`        | `hot_window_events * 2` | 仅在没有 prompt token 数据时使用的事件数量触发条件。省略时根据规范化后的 `hot_window_events` 推导；显式设置的值必须大于 `hot_window_events`。 |
| `context_window`                     | `32000`                 | 当前模型不在 `model_specs` 中时用于压缩的回退上下文窗口。运行时通常从 `ModelResolver.Spec()` 推导该值；此值只用于未知模型。 |
| `compress_at_percent`                | `50`                    | 百分比触发条件：当 `prompt_tokens / context_window >= compress_at_percent%` 时开始压缩。 |
| `summary_max_chunks`                 | `10`                    | `summary.md` 的 Recent Chunks 部分保留的分块摘要数量。较旧的条目会移入归档，并合并到 Rolling Summary 中。 |
| `session_boundary_enabled`           | `true`                  | 将每个新的用户轮次分类为延续当前会话或开启新会话。`new` 边界会归档当前的 `memory/session/` 目录，并重新创建一个空的活动会话。 |
| `session_boundary_short_gap_seconds` | `300`                   | 间隔低于此值时，无论词汇信号如何，该轮次都被视为延续。 |
| `session_boundary_long_gap_seconds`  | `1800`                  | 间隔高于此值时，无论词汇信号如何，该轮次都被视为新会话。 |
| `episode_memory_idle_delay_seconds`  | `300`                   | 符合条件的已完成 Episode 在后台被整合进设备记忆之前的空闲时间。前台任务会取消或推迟整合。 |
| `tag_candidates`                     | 见默认值            | 为分块摘要打标签时匹配的候选关键词。 |
| `entity_suffixes`                    | `["App","app","APP"]`   | 实体提取时识别的后缀。 |

## 已知限制

- `preferred_model` 和 `allowed_children` 目前会被解析，但尚未完全接入执行；

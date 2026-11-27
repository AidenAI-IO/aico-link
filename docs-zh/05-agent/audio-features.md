---
sidebar_position: 5
---

# 语音能力：VAD / STT / TTS

Go Agent 支持设备端语音交互，主要由 `internal/agent/audio_client.go`、`audio_dialog.go`、`vad.go`、`stt.go` 以及 `tts/` 提供商组成。

## 架构

```text
┌─────────────┐
│   daemon    │
└──────┬──────┘
       ├─────────────────────────► HTTP Server / Web UI (all input modes)
       │
       ├─ input_mode=stt ────────► Classic continuous voice session
       │                            (Silero VAD, optional Smart Turn, STT/LLM/TTS)
       └─ input_mode=realtime ───► Realtime Voice Session
```

## 组件

| 组件 | 文件 | 说明 |
| --- | --- | --- |
| 音频客户端 | `audio_client.go` | 连接 `audio_service`，启动录音/播放会话，读写 PCM 数据块 |
| VAD | `vad.go` + `/usr/lib/aico/rknn_vad` 或 `/usr/lib/aico/cpu_vad` | Silero VAD 语音活动检测；输入固定为 16 kHz、每帧 512 个采样/32 ms，`state` 由 helper 维护 |
| Smart Turn | `turn_detect.go` + `/usr/lib/aico/smart_turn_helper` | 可选启用的设备端轮次结束概率；仅在设置了 Classic `turn_detection_*` 路径时启用，否则 Silero 只按静音判断端点 |
| STT | `stt.go`、各提供商的客户端 | OpenAI Whisper、OpenRouter、腾讯云 ASR、Qwen ASR 和 Google Cloud STT；`tencent` / `tencent_asr` 仍作为 `tencent-asr` 的兼容别名保留 |
| TTS | `tts/`、`tts_helpers.go` | 可插拔的 TTS 提供商，为已配置的播放后端输出 PCM；必要时自动重采样到目标播放采样率 |
| 对话管理器 | `audio_dialog.go` | 编排持续开麦的会话录音、VAD（可选 Smart Turn）、STT 轮换、LLM/TTS 以及插话打断 |
| 语音通知 | `voice_notification.go`、`tts_fallback.go` | 在 TTS 之前添加持久的回复末尾提醒或最终轮次失败的替代回复；最终 TTS 不可用时使用内置的本地 WAV 回退 |

## 输入模式

### `input_mode = "stt"`

与 HTTP 服务和 Web UI 一起运行 Classic 持续语音会话
（类似 Realtime 的外壳，内部是本地 STT/LLM/TTS 流水线）：

1. 唤醒后打开一个会话，并在整个会话期间保持麦克风开启；
2. Silero VAD 检测语音，并在静音时结束本轮；配置了 Classic Smart Turn 路径时，短暂静音会询问 Smart Turn 本轮是否已结束（长时间静音则强制提交）；
3. 在每个轮次边界完成并轮换流式 STT，不关闭麦克风；
4. STT 文本交给 Agent 运行时；
5. TTS 合成回复，同时麦克风保持开启以支持插话打断；
6. 会话在以下情况结束：监听超时、达到最大轮数、空闲监听时按下监听按钮、在告别/待机意图之后成功调用 `wait_for_wakeup`，或进程停止。只是口头告别而没有调用该工具不会结束会话。

#### Web UI 语音模式

Agent Web UI 在 Classic 模式下提供与 realtime 模式相同的语音按钮
（参见[音频路由与 Web UI 语音模式](realtime-voice.md#音频路由与-web-ui-语音模式)）。
按下它会打开一个 Classic 会话，通过 `/api/realtime/voice/ws` 在该浏览器中采集和播放音频，
并按照[浏览器音频的 HTTPS](realtime-voice.md#浏览器音频的-https)所述使用 HTTPS。
STT、VAD、Agent 和 TTS 流水线不变，只有音频端点不同。由开发板发起的会话同样显示
光球、静音开关和结束按钮，但音频在设备上。

- 浏览器会话会忽略监听超时和 `voice_max_turns`；它会一直持续到按下结束、
  浏览器断开连接或配置重新加载为止。`wait_for_wakeup` 仍会结束它。
- 浏览器会话期间的唤醒词或按键会被忽略，因此不会截断回复，也不会打开
  开发板麦克风。
- 麦克风静音时，向 VAD 和 STT 输入静音；扬声器静音时，用静音替代回复播放，
  提示音也包括在内。两者在任一路由上都生效。
- 浏览器会话运行期间，在输入框中键入的消息的语音回复也会在该浏览器中播放。

在 Classic 模式下，通过 `/api/chat` 键入的消息只有文本：它们永远不会打开麦克风，
在语音会话之外也不会播放提示音或语音回复（包括工具调用语音和失败轮次的语音替代回复）。
在语音会话期间，键入的消息会加入对话，因此其回复会在该会话的路由上播报：
设备扬声器，或发起该会话的浏览器。只有唤醒词、按键或 Web UI 语音按钮才会打开会话。

如需临时测试设备扬声器，可以用 `AICO_TEXT_CHAT_TTS=1` 启动 Agent。
这样键入的聊天会使用已配置的独立 TTS 播报流式语音、工具进度和最终回复，而不打开麦克风。
活跃的语音会话保持其现有路由和扬声器静音行为。
删除该变量并重启 Agent 即可恢复静默的文本聊天。
放在 `/run/systemd/system/aico-agent.service.d/` 下的 systemd drop-in 可以启用该覆盖，
且只在下次设备重启前有效。

旧版单轮模式（`voice_followup_enabled = false`）不提供 Web UI 语音按钮。

在最终的非流式 TTS 之前，运行时会将语音回复交给共享的[语音通知管理器](voice-notifications.md)处理。成功的轮次可能会附加一条简短的持久尾巴；最终 LLM 失败时可能使用固定的替代回复。这些改动只影响播报文本，不影响助手历史或 UI 中的回复文本。

### `input_mode = "realtime"`

运行由 `[voice_settings.realtime].provider` 从命名的
`[voice_settings.realtime.providers]` 记录中选出的实时语音会话。Realtime 通过
`input_mode = "realtime"` 显式选择启用，不由是否存在 API key 控制。
在硬件唤醒按钮不可用时，实时会话也支持通过 `/api/chat` 激活。

适配器分层、各提供商的能力差异以及事件生命周期，请参见[实时语音模块](realtime-voice.md)。

## 唤醒触发

等待硬件触摸按钮来打开 Classic 语音会话。在当前开发板上，MCU 会向 Agent 上报 `touch_key` 唤醒事件；MCU 的 ASR 唤醒事件也走同样的语音流程。此路径优先使用 MCU 事件服务。`tap_wakeup_enabled` 默认为 `true`；设为 `false` 只会忽略触摸按钮，不会禁用 ASR 或其他激活路径。在 Realtime 模式下，关闭触摸唤醒后 `/api/chat` 仍可激活会话。在 Classic STT 模式下，唤醒会打开持续语音会话（`voice_followup_enabled` 默认为 `true`）；只有在需要旧版“每次唤醒只说一句”路径时才设为 `false`。唤醒后，麦克风在 `voice_followup_timeout_ms` 内保持开启以接收后续轮次；`voice_first_turn_timeout_ms` 控制第一轮的等待窗口，`voice_max_turns` 控制每个会话的最大轮数。Silero 会在思考和 TTS 播放期间检测插话语音，使近场说话可以强制停止输出；`voice_interrupt_on_wakeup` 仍控制再次唤醒是否取消当前轮次。在 Classic 监听期间，如果正在说话时按下按钮，会提前结束这句话；如果在空闲监听（尚未有语音）时按下按钮，会关闭会话并回到等待唤醒状态。当前监听中已经消化过的重复唤醒会被忽略。

另外，如果 MCU 服务 socket 不可用，守护进程只会在这些引脚仍是语音按键的开发板上（例如 Luckfox Pico Zero），或设置了 `AICO_LEGACY_GPIO_VOICE_WAKEUP=1` 时，使用受限的 GPIO 33/32 回退。在 Aico Link SCH v1 上，这两个 GPIO 用于驱动 HDMI 电源/复位，因此即使 MCU socket 缺失，该回退在这里也保持禁用。

Classic STT 的 Agent 轮次与主模型共用 `max_response_tokens` 输出上限，包括提供商计入该上限的推理 token。默认值 `0` 表示不发送上限，因此使用提供商的模型默认值；原生 Anthropic 要求必须提供上限，因此使用模型公布的输出上限（未知时为 `8192`）。它们不会额外应用语音专用的 token 上限。


## 配置片段

```toml
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

[voice_settings.classic.audio]
socket = "/run/audio_service/audio_service.sock"
sample_rate = 16000
channels = 1
bit_width = 16
backend = "auto"

[voice_settings.classic.stt.providers.openai-main]
type = "openai-whisper"
api_key = "$OPENAI_API_KEY"
model = "whisper-1"

[voice_settings.classic.stt]
provider = "openai-main"

# OpenRouter alternative:
# [voice_settings.classic.stt.providers.openrouter-main]
# type = "openrouter"
# api_key = "$OPENROUTER_API_KEY"
# model = "qwen/qwen3-asr-flash-2026-02-10"
# Set [voice_settings.classic.stt].provider = "openrouter-main" to select it.

[voice_settings.classic.tts.providers.alicloud-main]
type = "alicloud"
api_key = "$DASHSCOPE_API_KEY"
model = "qwen3-tts-flash-realtime"
voice_id = "Cherry"
emotion = "happy"

[voice_settings.classic.tts]
provider = "alicloud-main"
speed = 1.0
```

### 工具调用语音

当 `voice_tool_call_speech = true` 时，运行时会在工具调用事件到达时，异步地对其 `content` 进行 TTS 并播放。该 `content` 只来自同一个 LLM 工具调用回复中的助手内容，而不来自工具参数。

语音解析器会在工具调用完成之前流式播报开头的 `<tts>...</tts>` 块。
工具事件语音是对未被流式播报的带标签内容的回退。下一个语音块会立即接管：
运行时在开始新语音块之前，会取消上一个合成会话并停止其排队中的播放。
工具回复结束本身不会切断语音；替换发生在下一个语音块开始时。
待处理的异步工具语音不会延迟启动并替换更新的回答。

如果 LLM 没有在工具调用的同一个回复中生成助手内容，该工具调用将保持静默。运行时不会从工具参数中的 `speech`、`description` 或其他上下文字段推导简短的语音播报。

## TTS 提供商用法

### 本地 Ekho（默认）

Ekho 是默认的 TTS 适配器。Debian 应用构建会交叉编译
[Ekho](https://github.com/hgneng/ekho) 并准备其普通话语音数据；
`aico-business` 将可执行文件安装到 `/usr/lib/aico/ekho`，数据安装到
`/usr/share/aico/ekho-data`。Debian 提供 Flite SLT 运行时和音色，以及
libsndfile、Sonic 和 mpg123 依赖。参见
[构建输入与 Flite SLT 补丁](../../scripts/debian-apps/ekho/README.md)
和[软件包安装](../09-ota/debian-package.md)。
Agent 优先使用 `/usr/lib/aico/ekho`，其次在 `PATH` 上查找 `ekho`，或检查
`/usr/local/bin/ekho` 和 `/userdata/ekho/bin/ekho` 这两个手动安装位置。
已显式选择云端提供商的现有配置会保留该选择；未声明 TTS 配置段时，语音仍需主动启用。

```toml
[voice_settings.classic.tts]
provider = "ekho-local"
speed = 1.0

[voice_settings.classic.tts.providers.ekho-local]
type = "ekho"
```

不需要 API key。双语基线等价于
`ekho -v Mandarin -s 20 --english-speed -10 -a 0`。对于只包含拉丁字母、
不含其他文字字母的合成分块，Agent 会选择 `-v English`。这样会把完整分块
（包括数字、小数、日期、货币和百分比）交给 Flite 的英文文本规范化处理。
普通话模式会在英文回退之前把这些数字转换为中文。
中文以及中英混合的分块保持 `-v Mandarin`。只有数字的分块沿用前一个分块的语言；
新会话和 `ResetBuffer` 默认使用普通话。这也让英文发音在跨越 160 字符分块上限时保持一致。
直接使用 CLI 时，如果所有文本都应按英文朗读，请显式传入 `-v English`。
内置补丁通过 `src/english.cpp` 中 Flite 的 `register_cmu_us_slt()` 选用美式英语
女声 `slt`，普通话输入中嵌入的英文短语也使用该音色。普通话模式分块中的单个字母
仍使用 Ekho 的字母录音。英文 PCM 会重采样到所选音色的源采样率，并在通用输出处理之前
施加经过校准的 2 倍幅度增益（带 PCM16 饱和处理），使英文响度接近普通话。
该增益只作用于 Flite 英文音频。`speed` 是
相对于该预设的值：Ekho 的 `-s` 为 `round(120 * speed - 100)`，而
`--english-speed -10` 保持固定。对 Flite 而言，这表示在 Ekho 通用语速处理之前，
相对 SLT 的自然语速降低 10%。它不会保持旧 eSpeak 音色的精确时长。仅适用于云端的
音色/模型字段会被忽略。

设备集成已使用上游提交
[`796241e`](https://github.com/hgneng/ekho/commit/796241e13e4c52fff2055128575ce68164f89d8e)
的 Ekho 9.0 验证。选择该提供商之前，请以 Agent 的服务用户身份、使用相同的 `PATH`
和语音数据访问权限运行一次合成检查：

```bash
printf '%s\n' '你好。Hello, this is Ekho.' | \
  /usr/lib/aico/ekho -v Mandarin -s 20 --english-speed -10 -a 0 \
    --samplerate 22050 -f - -o /tmp/ekho-smoke.wav
```

这只检查合成。请使用已配置的音频后端单独验证扬声器路由和音量，然后删除
`/tmp/ekho-smoke.wav`。请将 `-a 0` 放在 `--english-speed` 之后：否则 Ekho 9.0
的选项解析器也会把英文语速值应用到音量上。软件包在
`/usr/share/doc/aico-business/ekho/` 下包含精确的上游源码归档、补丁、构建输入，
以及原始代码/音色许可证。独立安装的上游构建不会自动选用这个英文音色。

到达的 LLM 片段会缓冲到中英文句末标点或换行处。英文句点会向后查看，以保留小数
和常见缩写；即使没有空格，也会在紧邻的中文文本之前结束句子。没有标点的文本每次合成
最多 160 个字符，并尽量在空白或分句标点处切分。flush 或语音块结束时会输出剩余文本。
合成 worker 准备下一句的同时，独立的播放 worker 发送当前句的 PCM。
交接时每个会话最多保留两个句子 WAV（当前句和下一句）、一个排队中的文本句子
和一个合成进程。队列满时会对文本输入施加背压。播放顺序保持不变；取消会终止活跃的
子进程、停止播放并删除两个 WAV。合成和播放错误会由 `Err`、`Flush` 和 `Close` 保留。

在容量允许时，`WriteText` 会将完整句子入队而不等待播放。`Flush` 输出未完成的尾部，
并等待之前的 PCM 写入完成，但不结束播放；`Close` 还会等待两个 worker 结束并将
sink 排空一次。当文本到达得足够早时，预取可以隐藏下一句的合成时间；它不会降低第一句的合成延迟。

Agent 直接调用 `ekho`，通过 stdin 传入文本。它为每个句子创建一个临时的可 seek WAV，
以小块读取 22050 Hz 单声道 PCM16，并在成功、失败或取消时删除该 WAV。现有的音频后端
会重采样到其配置的播放采样率。Ekho 声明固定的单声道 PCM16 布局；即使通用音频配置
选择了立体声或其他位宽，其合成和播放会话也使用该布局。设备音频写入会按接近实时的速度
进行节流，以遵守音频服务的有界队列；桌面端文件播放不需要这种节流。不使用 `ekho-say`
及其独立的 `aplay` 进程。这提供的是句子级流式，而不是在未完成的句子内部进行音频流式。

### 流式会话与云端提供商

所选的提供商名称位于 `[voice_settings.classic.tts]`；提供商专用字段位于对应的 `[voice_settings.classic.tts.providers.<name>]` 记录中。完整说明参见 [Agent 配置参考](configuration.md#voice_settingsclassicstt-和-voice_settingsclassictts)。以下示例省略了 `api_key`，只展示与适配器行为相关的配置。

所有 TTS 提供商都通过统一的流式会话调用：Agent 将 LLM 输出片段写入适配器，由适配器决定何时发送给后端。Fish Audio、Alicloud 和 Volcengine 是真正的流式 WebSocket 链路；Minimax WebSocket 适配器会在内部按句子边界缓冲后再发送，因此上层无需区分“真流式”还是“句子级流式”。运行时可以通过 `POST /api/settings/tts` 切换提供商；已经开始的播放会继续使用旧提供商，后续请求使用新提供商。

每条助手回复都使用同一种协议：开头的字节恰好是一个前置 `<tts>...</tts>` 块，后面跟着面向用户的文本。这同时适用于最终回答和随工具调用输出的进度内容。播报形式和可见形式不必完全相同。

前置 TTS 块会随字节到达而流式播报。如果在可见文本之前又开始了另一个块，它会立即替换先前的语音；提示词仍要求每条回复恰好有一个前置块。收到结束标签 `</tts>` 时，运行时会 flush 提供商，使简短的 MiniMax 语音不必等待剩余的可见回复。在工具调用回复的边界处，流式块会在处理工具事件之前完成，避免同一段进度语音再通过工具事件路径重复播放，同时仍保留 `voice_tool_call_speech` 以及 `wait_for_wakeup` 等工具特定的静音处理。出现在可见文本之后的标签以及缺失的结束标签，仍作为针对旧版或格式错误的模型输出的运行时兼容回退，但它们不是合法的提示词输出。如果模型省略了结束标签，回复边界会充当隐式的 `</tts>`，flush 剩余语音而不是丢弃。

```toml
# Minimax WebSocket
[voice_settings.classic.tts]
provider = "minimax-main"
speed = 1.0

[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
model = "speech-2.8-hd"
voice_id = "male-qn-qingse"
emotion = "happy"
```

```toml
# Fish Audio WebSocket
[voice_settings.classic.tts]
provider = "fish-main"
speed = 1.0

[voice_settings.classic.tts.providers.fish-main]
type = "fish-audio"
model = "s2-pro"
reference_id = "98655a12fa944e26b274c535e5e03842"
```

```toml
# Alicloud Qwen-TTS Realtime
[voice_settings.classic.tts]
provider = "alicloud-main"
speed = 1.0

[voice_settings.classic.tts.providers.alicloud-main]
type = "alicloud"
model = "qwen3-tts-flash-realtime"
voice_id = "Cherry"
```

```toml
# Volcengine WebSocket Bidirectional Streaming V3
[voice_settings.classic.tts]
provider = "volcengine-main"
speed = 1.0

[voice_settings.classic.tts.providers.volcengine-main]
type = "volcengine"
model = "seed-tts-2.0"
voice_id = "zh_female_vv_uranus_bigtts"
```

## 依赖

- `audio.backend` 同时选择录音和播放后端。`auto` 在开发板上使用 `audio_service`，在桌面/PC Agent 模式下通过 ADB 输入后端或环境桥接使用本地后端；
- 当 `audio.backend = "audio_service"` 时，`audio_service` 必须在运行；
- 本地录音后端在 macOS 上使用 SoX `rec` 或 `ffmpeg`（AVFoundation），在 Linux 上使用 `pw-record`、`parec`、`arecord`、SoX `rec` 或 `ffmpeg`（PulseAudio）。播放在 macOS 上使用 `afplay`/`ffplay`，在 Linux 上使用 `pw-play`/`paplay`/`aplay`/`ffplay`，在 Windows 上使用 PowerShell。会选择第一个可用的命令。Windows 目前不支持本地录音；
- `rknn_vad` / `cpu_vad` helper 必须可执行；当 `vad_backend="rknn"` 时，`vad_model_path` 指向转换后的 encoder RKNN；当 `vad_backend="cpu"` 时，helper 默认为 `/usr/lib/aico/cpu_vad`；
- 启用 Classic Smart Turn 时，`smart_turn_helper` 必须可执行，并且必须设置 `turn_detection_model_path` / `turn_detection_helper_path`；两者都留空则保持仅按静音判断端点的 Silero；
- 云端 STT/TTS 需要外部 API key；本地 Ekho 需要其可执行文件和语音数据；
- STT 模式通常使用硬件触摸按钮启动持续语音会话；设置 `tap_wakeup_enabled = false` 只会忽略该按钮（Realtime 的 `/api/chat` 激活不受影响）。

可以直接在开发板上验证 VAD 和 Smart Turn helper：

```bash
/usr/lib/aico/rknn_vad --model /usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/cpu_vad --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/smart_turn_helper --model /usr/lib/aico/models/smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn --self-test
```

成功时，helper 会输出一行 `P <probability>`；如果仍有 RKNN 输入配置问题，则输出 `ERR ...`。

## 已知限制

- Web UI 和设备语音交互共享同一个 Agent 运行时，因此同一时间只能执行一次 Agent 运行；

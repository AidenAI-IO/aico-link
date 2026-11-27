---
sidebar_position: 1
---

# Agent：AI 助手服务

`agent` 是核心 Go 服务，提供 Agent Web UI、HTTP API 以及可选的设备端语音交互。它负责编排模型调用、工具执行（HID 控制、截图、音频播放）和对话会话。

## 默认参数

| 参数 | 默认值 |
| --- | --- |
| 配置 | `/userdata/agent/agent.toml` |
| 日志 | `/userdata/agent/log/agent.log` |
| 二进制文件 | `/usr/lib/aico/agent` |
| 会话数据 | `/userdata/agent/` |

## 启动

```bash
systemctl start aico-agent.service
systemctl status aico-agent.service
systemctl restart aico-agent.service
```

## 输入模式

HTTP 服务器和 Web UI 在所有输入模式下都会运行：

- **`input_mode = "stt"`**：与 HTTP 服务器并行运行设备音频循环，使用 `audio_service`、VAD、所选的 STT 提供商、模型以及所选的 TTS 提供商。
- **`input_mode = "realtime"`**：直接运行已配置的实时语音模型；实时模式通过该模式显式选择启用。

守护进程参数 `-classic-voice`（默认 `true`）控制 `stt` 模式下能否进行语音对话。
没有 VAD 模型的主机（例如 Docker 镜像）会以 `-classic-voice=false` 启动 Agent。
此时在 `stt` 模式下，Agent 不会构建音频对话，也不会启动 VAD 或 Smart Turn 辅助程序：
来自 MCU 或 GPIO 按键的唤醒会被忽略，`GET /api/realtime/voice` 返回 `available: false`，
因此 Web UI 不显示语音按钮，浏览器发起的语音也会被拒绝。文字聊天仍可正常使用，并以文本回复。
该设置在配置重新加载后依然保留，且不影响 `realtime` 模式。该参数关闭时，Agent 会在启动时记录一行日志。

## 清除会话

在 Agent Web UI 中，**Clear session** 和 **Clear session & memory** 会先停止正在进行的
Agent 工作，再重置对话。**Clear session & memory** 还会删除长期记忆和用户画像。

## 任务历史

在 Agent Web UI 中，打开 **Background tasks** 下的 **Task history**。
选择一个任务，即可查看其工具调用时间线、后台模型用量和费用来源。

在 **Cost** 列中，短横线（`—`）表示金额不可用，金额后的 `+` 表示可能未计入部分模型调用。
任务历史遵循 Agent 的本地记录保留策略，即使禁用了遥测导出也可以使用。

## 配置

该服务读取 `/userdata/agent/agent.toml`。提供商表定义具名的提供商实例，对应的选择表引用其中某个名称：

```toml
[voice_settings.mode]
input_mode = "stt"

[model_settings.providers.openai-main]
type = "openai"
api_key = "$OPENAI_API_KEY"
# base_url = "https://api.openai.com/v1"

[model_settings.model]
provider = "openai-main"
model = "gpt-5.5"
# temperature = 0.2
# max_response_tokens = 0  # 0 = provider default

[voice_settings.classic.audio]
# socket = "/run/audio_service/audio_service.sock"
# backend = "auto"

[advanced_settings.hardware.hid]
# keyboard_device = "/dev/hidg0"
# mouse_device = "/dev/hidg1"
# android_keyboard_device = "/dev/hidg2"
# frame_socket = "/run/frame_service/frame_service.sock"

[voice_settings.classic.stt.providers.openai-main]
type = "openai-whisper"
api_key = "$OPENAI_API_KEY"
model = "whisper-1"

[voice_settings.classic.stt]
provider = "openai-main"

[voice_settings.classic.tts.providers.minimax-main]
type = "minimax-cn"
api_key = "$MINIMAX_API_KEY"
voice_id = "male-qn-qingse"

[voice_settings.classic.tts]
provider = "minimax-main"
speed = 1.0
```

内置的模型提供商类型包括 `openai`、`anthropic`、`openrouter`、`kimi`、`kimi-cn`、`volcengine`、`deepseek`、`gemini` 和 `ollama`。Anthropic Claude 可以通过原生的 `anthropic` 提供商直接访问，也可以通过 OpenRouter 等兼容提供商访问。Google Gemini 模型使用原生的 `gemini` 提供商，该提供商使用 Interactions API。

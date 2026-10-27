---
sidebar_position: 1
---

# Agent: AI Assistant Service

`agent` is the core Go service that provides the Agent Web UI, HTTP APIs, and optional device-side voice interaction. It orchestrates model calls, tool execution (HID control, screenshots, audio playback), and conversation sessions.

## Default Parameters

| Parameter | Default Value |
| --- | --- |
| Config | `/userdata/agent/agent.toml` |
| Log | `/userdata/agent/log/agent.log` |
| Binary | `/usr/lib/aico/agent` |
| Session data | `/userdata/agent/` |

## Startup

```bash
systemctl start aico-agent.service
systemctl status aico-agent.service
systemctl restart aico-agent.service
```

## Input Modes

The HTTP server and Web UI run in every input mode:

- **`input_mode = "stt"`**: Runs the device audio loop in parallel with the HTTP server, using `audio_service`, VAD, the selected STT provider, the model, and the selected TTS provider.
- **`input_mode = "realtime"`**: Runs the configured realtime voice model directly; realtime activation is selected explicitly by this mode.

## Configuration

The service reads `/userdata/agent/agent.toml`. Provider tables define named provider instances, and the corresponding selection table references one of those names:

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
# max_response_tokens = 8192

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

Built-in model provider types include `openai`, `anthropic`, `openrouter`, `kimi`, `kimi-cn`, `volcengine`, `deepseek`, `gemini`, and `ollama`. Anthropic Claude can be accessed directly through the native `anthropic` provider or through a compatible provider such as OpenRouter. Google Gemini models use the native `gemini` provider, which speaks the Interactions API.

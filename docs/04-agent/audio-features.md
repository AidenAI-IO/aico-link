---
sidebar_position: 5
---

# Voice Capabilities: VAD / STT / TTS

The Go Agent supports device-side voice interaction, primarily consisting of `internal/agent/audio_client.go`, `audio_dialog.go`, `vad.go`, `stt.go`, and the `tts/` provider.

## Architecture

```text
┌─────────────┐
│   daemon    │
└──────┬──────┘
       ├─────────────────────────► HTTP Server / Web UI (all input modes)
       │
       ├─ input_mode=stt ────────► Legacy Audio Dialog Loop
       └─ input_mode=realtime ───► Realtime Voice Session
                                   │
                                   ├─ AudioServiceClient
                                   ├─ VAD
                                   ├─ STT
                                   ├─ LLM
                                   └─ TTS
```

## Components

| Component | Files | Description |
| --- | --- | --- |
| Audio client | `audio_client.go` | Connects to `audio_service`, starts recording/playback sessions, reads/writes PCM chunks |
| VAD | `vad.go` + `/usr/lib/aiden/rknn_vad` or `/usr/lib/aiden/cpu_vad` | Silero VAD inference; input is fixed at 16 kHz, 512 samples/32 ms, with `state` maintained in helper |
| STT | `stt.go`, provider-specific clients | OpenAI Whisper, OpenRouter, Tencent Cloud ASR, Qwen ASR, and Google Cloud STT; `tencent` / `tencent_asr` remain compatibility aliases for `tencent-asr` |
| TTS | `tts/`, `tts_helpers.go` | Pluggable TTS provider, outputs PCM for the configured playback backend; automatically resamples to the target playback sample rate when necessary |
| Dialog manager | `audio_dialog.go` | Orchestrates recording, VAD, STT/LLM/TTS flow |
| Voice notifications | `voice_notification.go`, `tts_fallback.go` | Adds a persistent response tail or final-turn failure replacement before TTS, with bundled local WAV fallback when final TTS is unavailable |

## Input Modes

### `input_mode = "stt"`

Runs the device-side audio loop alongside the HTTP server and Web UI:

1. Records PCM via `audio_service`;
2. VAD determines sentence boundaries;
3. Sends WAV to STT;
4. STT text goes to Agent runtime;
5. TTS synthesizes the reply and plays it through the configured playback backend.

Before final non-streaming TTS, the runtime passes the spoken reply through the shared [Voice Notification manager](voice-notifications.md). A successful turn may receive one short persistent tail. A final LLM failure may use a fixed replacement. These changes affect spoken text only, not assistant history or UI response text.

### `input_mode = "realtime"`

Runs the realtime voice session selected by `[voice_settings.realtime].provider` from the
named `[voice_settings.realtime.providers]` records. Realtime activation is selected
explicitly by `input_mode = "realtime"`; it is not controlled by API key
presence. The realtime session also supports `/api/chat` activation when GPIO
is unavailable.

See [Realtime Voice Module](realtime-voice.md) for the adapter layers,
provider capability differences, and event lifecycle.

## Wakeup Trigger

Waits for the hardware tap button to start recording. On current boards that button is wired to GPIO 33/GPIO 32 (falling edge); both pins enter the same wakeup flow. `tap_wakeup_enabled` defaults to `true`; set it to `false` to ignore the tap button only—this does not disable other activation paths. In Realtime mode, `/api/chat` can still activate a session when tap wakeup is off. In Classic STT mode, the tap button is the normal way to open a voice session, so disabling it leaves the voice loop waiting until the setting is turned back on. When `input_mode = "stt"` and `voice_followup_enabled = true`, wakeup opens a continuous voice session: the first turn still requires the tap button (when tap wakeup is enabled), but after the Agent replies it will continue listening for follow-up questions within the `voice_followup_timeout_ms` window. `voice_first_turn_timeout_ms` controls the first-turn waiting window, and `voice_max_turns` controls the maximum number of turns per session. Repeated wakeup triggers during listening or recording are merged or ignored and will not restart recording or discard already-recorded audio; whether triggering wakeup again during thinking cancels the current LLM request is controlled by `voice_interrupt_on_wakeup`; the microphone is not open by default during TTS playback, and recording continues only after playback ends; triggering wakeup again during playback will interrupt the current turn and immediately start recording. On current boards this path requires Linux GPIO sysfs and completed button wiring.

## Configuration Snippet

```toml
[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.runtime]
vad_backend = "rknn"
vad_model_path = "/usr/lib/aiden/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn"
vad_helper_path = "/usr/lib/aiden/rknn_vad"
vad_speech_threshold = 0.5
silence_ms = 550
min_speech_ms = 300
tap_wakeup_enabled = true
voice_followup_enabled = false
voice_followup_timeout_ms = 5000
voice_first_turn_timeout_ms = 10000
voice_max_turns = 0
voice_interrupt_on_wakeup = true
voice_streaming_tts_enabled = true
voice_tool_call_speech = true
voice_max_response_tokens = 300

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

### Tool Call Speech

When `voice_tool_call_speech = true`, the runtime will asynchronously TTS and play the `content` of tool call events when they arrive. This `content` comes only from the assistant content in the same LLM tool-call response, not from tool parameters.

If the LLM does not generate assistant content in the same response as the tool call, that tool call remains silent. The runtime does not derive short speech announcements from `speech`, `description`, or other context fields in tool parameters.

## TTS Provider Usage

The selected provider name lives in `[voice_settings.classic.tts]`; provider-specific fields live in its `[voice_settings.classic.tts.providers.<name>]` record. See [Agent Configuration Reference](configuration.md#voice_settingsclassicstt-and-voice_settingsclassictts) for full details. The following examples omit `api_key` and only show adapter behavior-related configuration.

All TTS providers are called through a unified streaming session: the Agent writes LLM output fragments to the adapter, and the adapter decides when to send to the backend. Fish Audio, Alicloud, and Volcengine are true streaming WebSocket links; the Minimax WebSocket adapter buffers internally at sentence boundaries before sending, so the upper layer doesn't need to distinguish between “true streaming” or “sentence-level streaming”. The runtime can switch providers via `POST /api/settings/tts`; playback that has already started will continue using the old provider, and subsequent requests will use the new provider.

Every assistant response uses one protocol: its first bytes are exactly one leading `<tts>...</tts>` block, followed by the user-facing text. This applies both to final answers and to progress content emitted with tool calls. The spoken and visible forms do not need to be identical.

Only the first leading TTS block in each LLM response is streamed. When its closing `</tts>` tag arrives, the runtime immediately flushes the provider so short MiniMax speech does not wait for the remaining visible response. At a tool-call response boundary, the streamed block is finalized before the tool event is handled, preventing the same progress speech from being played again through the tool-event path while still preserving `voice_tool_call_speech` and tool-specific suppression such as `wait_for_wakeup`. Trailing tags, extra TTS blocks, and missing closing tags remain runtime compatibility fallbacks for older or malformed model output, but they are not valid prompt output. If the model omits the closing tag, the response boundary acts as an implicit `</tts>` and flushes the remaining speech instead of dropping it.

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

## Dependencies

- `audio.backend` selects both recording and playback. `auto` uses `audio_service` on the board and the local backend in desktop/PC Agent mode through the ADB input backend or environment bridge;
- `audio_service` must be running when `audio.backend = "audio_service"`;
- the local recording backend uses SoX `rec` or `ffmpeg` (AVFoundation) on macOS and `pw-record`, `parec`, `arecord`, SoX `rec`, or `ffmpeg` (PulseAudio) on Linux. Playback uses `afplay`/`ffplay` on macOS, `pw-play`/`paplay`/`aplay`/`ffplay` on Linux, and PowerShell on Windows. The first available command is selected. Local recording is not currently available on Windows;
- `rknn_vad` / `cpu_vad` helper must be executable; when `vad_backend="rknn"`, `vad_model_path` points to a converted encoder RKNN; when `vad_backend="cpu"`, helper defaults to `/usr/lib/aiden/cpu_vad`;
- STT/TTS require external API keys;
- STT mode normally uses the hardware tap button to start a voice session; set `tap_wakeup_enabled = false` to ignore that button only (Realtime `/api/chat` activation is unchanged).

VAD helper can be verified directly on the board:

```bash
/usr/lib/aiden/rknn_vad --model /usr/lib/aiden/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn --weights /usr/lib/aiden/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aiden/cpu_vad --weights /usr/lib/aiden/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
```

On success, it will output a line `P <probability>`; if there are still RKNN input configuration issues, it will directly output `ERR ...`.

## Known Limitations

- Web UI and device voice interactions share one Agent runtime, so only one Agent run can execute at a time;

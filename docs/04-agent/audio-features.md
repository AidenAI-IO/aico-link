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
       ├─ input_mode=stt ────────► Classic continuous voice session
       │                            (Silero VAD, optional Smart Turn, STT/LLM/TTS)
       └─ input_mode=realtime ───► Realtime Voice Session
```

## Components

| Component | Files | Description |
| --- | --- | --- |
| Audio client | `audio_client.go` | Connects to `audio_service`, starts recording/playback sessions, reads/writes PCM chunks |
| VAD | `vad.go` + `/usr/lib/aico/rknn_vad` or `/usr/lib/aico/cpu_vad` | Silero VAD speech activity; input is fixed at 16 kHz, 512 samples/32 ms, with `state` maintained in helper |
| Smart Turn | `turn_detect.go` + `/usr/lib/aico/smart_turn_helper` | Opt-in on-device turn-complete probability; enabled only when Classic `turn_detection_*` paths are set. Otherwise Silero uses silence-only endpointing |
| STT | `stt.go`, provider-specific clients | OpenAI Whisper, OpenRouter, Tencent Cloud ASR, Qwen ASR, and Google Cloud STT; `tencent` / `tencent_asr` remain compatibility aliases for `tencent-asr` |
| TTS | `tts/`, `tts_helpers.go` | Pluggable TTS provider, outputs PCM for the configured playback backend; automatically resamples to the target playback sample rate when necessary |
| Dialog manager | `audio_dialog.go` | Orchestrates continuous-mic session recording, VAD (optional Smart Turn), STT rotate, LLM/TTS, and barge-in |
| Voice notifications | `voice_notification.go`, `tts_fallback.go` | Adds a persistent response tail or final-turn failure replacement before TTS, with bundled local WAV fallback when final TTS is unavailable |

## Input Modes

### `input_mode = "stt"`

Runs the Classic continuous voice session alongside the HTTP server and Web UI
(Realtime-like shell, local STT/LLM/TTS pipeline):

1. Wakeup opens a session and keeps the microphone open for the whole session;
2. Silero VAD detects speech and ends the turn on silence; when Classic Smart Turn paths are configured, short silence asks Smart Turn whether the turn is complete (long silence force-commits);
3. Streaming STT is finalized and rotated at each turn boundary without closing the mic;
4. STT text goes to Agent runtime;
5. TTS synthesizes the reply while the mic stays open for barge-in;
6. Session ends on listen timeout, max turns, goodbye/tool wait-for-wakeup, or process stop.

Before final non-streaming TTS, the runtime passes the spoken reply through the shared [Voice Notification manager](voice-notifications.md). A successful turn may receive one short persistent tail. A final LLM failure may use a fixed replacement. These changes affect spoken text only, not assistant history or UI response text.

### `input_mode = "realtime"`

Runs the realtime voice session selected by `[voice_settings.realtime].provider` from the
named `[voice_settings.realtime.providers]` records. Realtime activation is selected
explicitly by `input_mode = "realtime"`; it is not controlled by API key
presence. The realtime session also supports `/api/chat` activation when the
hardware wakeup button is unavailable.

See [Realtime Voice Module](realtime-voice.md) for the adapter layers,
provider capability differences, and event lifecycle.

## Wakeup Trigger

Waits for the hardware tap button to open a Classic voice session. On current boards the MCU reports a `touch_key` wakeup event to the Agent; the MCU's ASR wakeup event uses the same voice flow. `tap_wakeup_enabled` defaults to `true`; set it to `false` to ignore the tap button only—this does not disable ASR or other activation paths. In Realtime mode, `/api/chat` can still activate a session when tap wakeup is off. In Classic STT mode, wakeup opens a continuous voice session (`voice_followup_enabled` defaults to `true`); set it to `false` only when you need the legacy one-utterance-per-wakeup path. After wakeup, the mic stays open for follow-up turns within `voice_followup_timeout_ms`; `voice_first_turn_timeout_ms` controls the first-turn waiting window, and `voice_max_turns` controls the maximum number of turns per session. Silero detects barge-in speech during thinking and TTS playback so near-field talk can force-stop output; `voice_interrupt_on_wakeup` still controls whether another wakeup cancels the active turn. Repeated wakeup triggers during listening are merged or ignored. This path requires the MCU event service to be running.

Classic STT Agent turns share the main model's `max_response_tokens` output limit (8192 by default), including any reasoning tokens counted toward that limit by the provider. They do not apply a separate voice-specific token cap.


## Configuration Snippet

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

### Tool Call Speech

When `voice_tool_call_speech = true`, the runtime will asynchronously TTS and play the `content` of tool call events when they arrive. This `content` comes only from the assistant content in the same LLM tool-call response, not from tool parameters.

The speech parser streams leading `<tts>...</tts>` blocks before the tool call
finishes. Tool-event speech is a fallback for tagged content that was not
streamed. The next spoken block takes over immediately: the runtime cancels the
previous synthesis session and stops its queued playback before starting the
new block. A tool response ending does not itself cut off speech; replacement
occurs when the next spoken block begins. Pending asynchronous tool speech
cannot start late and replace a newer answer.

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
- `rknn_vad` / `cpu_vad` helper must be executable; when `vad_backend="rknn"`, `vad_model_path` points to a converted encoder RKNN; when `vad_backend="cpu"`, helper defaults to `/usr/lib/aico/cpu_vad`;
- when Classic Smart Turn is enabled, `smart_turn_helper` must be executable and `turn_detection_model_path` / `turn_detection_helper_path` must be set; leave both empty to keep silence-only Silero endpointing;
- STT/TTS require external API keys;
- STT mode normally uses the hardware tap button to start a continuous voice session; set `tap_wakeup_enabled = false` to ignore that button only (Realtime `/api/chat` activation is unchanged).

VAD and Smart Turn helpers can be verified directly on the board:

```bash
/usr/lib/aico/rknn_vad --model /usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/cpu_vad --weights /usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin --self-test
/usr/lib/aico/smart_turn_helper --model /usr/lib/aico/models/smart-turn-v3.2-dyt-4s-rv1106-int8-mmse.rknn --self-test
```

On success, helpers output a line `P <probability>`; if there are still RKNN input configuration issues, they output `ERR ...`.

## Known Limitations

- Web UI and device voice interactions share one Agent runtime, so only one Agent run can execute at a time;

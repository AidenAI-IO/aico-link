---
sidebar_position: 2
---

# Agent Configuration Reference

The Agent daemon takes `-dir`, the data directory it works out of. `agent.toml` is only one of the things that live there: skills, memory, cache and logs are all resolved relative to it (see [Directory layout](#directory-layout)). The `config`, `config-check` and `config-test` subcommands take `-config` with the path to a TOML config file. Every field below lives in `agent.toml`. Most fields can be edited through the on-device [Config Web page](#config-web-the-device-config-page); sections without dedicated controls are preserved by Config Web and can be edited by hand. TOML is the only supported config format; JSON config is deprecated.

`config-check --config <path>` applies the strict validator and is the gate the image
build and release workflows run, so a configuration the runtime would quietly recover
from at boot still fails there — for example a persisted `input_mode = "realtime"` whose
credential was removed, where the Agent falls back to text mode instead of refusing to
start. The Config Web recovery page reports a different verdict on purpose: it uses the
runtime loader, because it has to describe the state the Agent actually boots in and name
the field to repair. Candidate values a user is about to save are validated strictly by
`config-check --stdin`.

The daemon also accepts `-device-type <value>` as a process-local override for
`[basic_settings.device].device_type`. The override is applied after `agent.toml` is loaded, so
the command-line value has higher priority and does not rewrite the config file.
Accepted values and aliases are normalized to the canonical values documented
under `[basic_settings.device]` below.

## Configuration groups

`agent.toml` uses the same product-oriented hierarchy as the Agent settings
page. Runtime code maps these grouped tables to its internal structures at the
load boundary; Config Web writes only the grouped paths below.

1. **Wi-Fi and Bluetooth Settings**: Configured through Config Web, not in `agent.toml`
   - WiFi Configuration: Network credentials, proxy settings
   - Bluetooth Configuration: Managed by separate bluetooth service

2. **Basic Settings**:
   - **Language & Time Zone**: `[basic_settings.language_timezone]`
   - **Device Settings**: `[basic_settings.device]`, including `[basic_settings.device.hid].keyboard_layout`

3. **Conversation Settings**:
   - Agent Prompt: `prompt`
   - Max Iterations: `max_iterations`
   - Context Management: `context_prune_threshold`, `context_compaction_threshold`
   - Screenshot Pruning: `screenshot_keep_n`, `screenshot_prune_interval`
   - Tool Settings: `[conversation_settings.termination_policy]`, web search `[conversation_settings.search]`

4. **Main Model Settings**:
   - Provider Configuration: `[model_settings.providers.<name>]`
   - Model Selection: `[model_settings.model]` including provider, model, api_key, temperature, max_response_tokens, context_window, reasoning_effort, and api_mode settings

5. **Voice Settings**:
   - Mode Selection: `[voice_settings.mode].input_mode` (`stt` for Classic or `realtime` for Realtime)
   - **Realtime Mode**: `[voice_settings.realtime.providers.<name>]`, `[voice_settings.realtime]`
   - **Classic Mode**: `[voice_settings.classic.runtime]`, `[voice_settings.classic.stt]`, `[voice_settings.classic.stt.providers.<name>]`, `[voice_settings.classic.tts]`, `[voice_settings.classic.tts.providers.<name>]`, `[voice_settings.classic.audio]`, `[voice_settings.classic.audio_archive]`

6. **Memory Settings**:
   - Screen Memory Retention: `[memory_settings.screen].screen_memory_ttl`
   - Notification Memory: `[memory_settings.notification]` and expiration policies
   - Reset Conversation: Handled through Config Web UI

7. **Storage Settings**:
   - Storage Status: Displayed through Config Web (total/available space)
   - microSD Settings: `[storage_settings.storage]` configuration, format/eject operations
   - Backup & Restore: Export or import the canonical grouped `agent.toml` through Config Web
   - Internal storage policy: `[storage_settings.storage.degraded_mode]`, `[storage_settings.storage.cleanup]`

8. **Advanced Settings**:
   - Logs: detailed model request capture, Agent log level, retention, and support-log export
   - Manual Config Edit: validated raw editor for the canonical grouped `agent.toml`
   - MCP: `[conversation_settings.agent].mcp_enabled` (toggle on the Advanced page); see [MCP](mcp.md)

   Hardware and runtime debug tables remain available in TOML but are not
   exposed as product settings in Config Web.

9. **About**:
   - Firmware Version: Read from OTA state; before the first OTA transaction,
     read from the factory baseline in `/userdata/debian/ota/config.json`
   - Component Versions: Boot and RootFS versions for the running slot
   - Version information refreshes with device status without initializing or
     modifying OTA state

The group tables are the canonical on-disk configuration schema. New options
should be added to the closest existing group and its section rather than
creating another top-level section.

## Contents

- [Configuration groups](#configuration-groups)
- [Directory layout](#directory-layout)
- [Config Web: the device config page](#config-web-the-device-config-page)
- [Minimal config examples](#minimal-config-examples)
- [Top-level fields](#top-level-fields)
- [`[basic_settings.device]`](#basic_settingsdevice)
- [`[basic_settings.device.hid]`](#basic_settingsdevicehid)
- [`[model_settings.model]`](#model_settingsmodel)
- [`[advanced_settings.log]`](#advanced_settingslog)
- [`[voice_settings.classic.audio]`](#voice_settingsclassicaudio)
- [`[voice_settings.realtime]`](#voice_settingsrealtime)
- [`[voice_settings.realtime.providers.<name>]`](#voice_settingsrealtimeprovidersname)
- [`[advanced_settings.hardware.frame_service]`](#advanced_settingshardwareframeservice)
- [Quick Capture](#quick-capture)
- [`[memory_settings.notification]`](#memory_settingsnotification)
- [`[advanced_settings.hardware.hid]`](#advanced_settingshardwarehid)
- [`[voice_settings.classic.stt]` and `[voice_settings.classic.tts]`](#voice_settingsclassicstt-and-voice_settingsclassictts)
- [`[advanced_settings.runtime.live_activity]`](#advanced_settingsruntimelive_activity)
- [Episode telemetry (Langfuse)](#episode-telemetry-langfuse)
- [System environment variables](#system-environment-variables)
- [`memory/extraction.yaml`](#memoryextractionyaml)
- [Known limitations](#known-limitations)

## Applying Changes Online

Saving through Config Web queues runtime application. See the
[application policy](../03-services/config-web.md#configuration-application-policy)
and [status API](../03-services/device-management-api.md#configuration-save-response).
Mode, model/provider, audio/VAD, and hardware-client changes rebuild the affected
components after current work drains. Ordinary limits and policies apply to
subsequent work. USB pointer descriptor and keyboard layout changes remain
pending across Agent restarts until an explicit board reboot; changing `frame_service.keep_streamon`
restarts only the frame service. Every queued application logs one outcome line
to `log/agent.log`: `config_applied` with the applied revision and duration, or
`config_apply_failed` with the error. Editing the TOML outside Config Web
requires an explicit reload request or service restart; there is no file watcher.

## Directory layout

Passed to the daemon as `-dir /userdata/agent`. Everything except `agent.toml`
is created on demand, so a directory holding only `agent.toml` is a valid start.

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

## Config Web: the device config page

Config Web is the browser client served by the `config-web` subcommand of the
Go Agent binary (`/usr/lib/aiden/agent`). It maintains the device Agent
configuration, system environment variables, and Wi-Fi configuration, and is
the primary way to edit the fields documented on this page without manually
editing `agent.toml`. Its device operations use the
[Device Management API](../03-services/device-management-api.md), which is
designed to remain usable if the bundled page is later replaced or removed.

On a device, open the config page in a browser at the USB-network gateway address:

```text
http://192.168.42.1
```

The firmware starts `agent config-web` on port 80.

### What the page can configure

The page renders the following config sections. The Language & Time Zone controls persist the device-level `locale` and `timezone` and apply them online. Changing either value rotates the context at the next task boundary instead of rewriting the previous session. The selected time zone is included in Agent state and controls the current-date context and controller shell commands.

- `[basic_settings.language_timezone]`: UI and response language plus controller time zone
- `[conversation_settings.agent]`: Agent prompt, iteration and context controls
- `[model_settings.model]`: provider, model, api_mode, temperature, max_response_tokens, context_window, model_max_output_tokens
- `[voice_settings.classic.stt]`: provider, language and STT options
- `[voice_settings.classic.tts]`: provider and playback options
- `[voice_settings.classic.audio]`: socket, sample_rate, channels, bit_width, backend
- `[voice_settings.classic.audio_archive]`: optional STT recording archive settings
- `[voice_settings.realtime]`: selected realtime provider and session options
- `[advanced_settings.hardware.frame_service]`: whether Frame Service keeps capture STREAMON between screenshots
- `[memory_settings.screen]`: GPIO capture and Screen Memory retention
- `[memory_settings.notification]`: notification retention and expiration policies
- `[advanced_settings.log]`: LLM HTTP log retention
- `[advanced_settings.runtime.ota]`: optional GitHub download proxy URL
- `[basic_settings.device]`: device_type
- `[basic_settings.device.hid]`: user-facing keyboard layout
- `[advanced_settings.hardware.hid]`: internal HID device paths and input backend
- `[conversation_settings.search]`: web-search provider and provider credential
- `[advanced_settings.runtime.telemetry]`: Langfuse enablement, endpoint, credentials, upload policy, environment, and tags
- `[advanced_settings.runtime.live_activity]`: Phone Bridge Live Activity enablement
- `env`: shell-style environment text written to `/userdata/system/env`, including optional proxy variables such as `http_proxy`, `HTTPS_PROXY`, and `NO_PROXY`
- Wi-Fi: SSID / PSK etc. (written to `/userdata/wpa_supplicant.conf`)

## Minimal config examples

### HTTP/Web UI without the device voice loop (voice unconfigured)

```toml
[basic_settings.language_timezone]
locale = "en-US"
timezone = "UTC"

[conversation_settings.agent]
max_iterations = -1
context_prune_threshold = 0.5
context_compaction_threshold = 0.8
screenshot_keep_n = 3
screenshot_prune_interval = 2

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
max_response_tokens = 8192
# OpenRouter supports stateless Responses only. This keeps ContextManager
# history local and omits store and previous_response_id from every request.
# api_mode = "responses"
# For reasoning models, request the encrypted reasoning item needed for exact
# stateless replay when the provider supports it.
# responses_include = ["reasoning.encrypted_content"]
# responses_stateful is available only with OpenAI and Volcengine Ark. Aiden
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

> Provider credentials use one field everywhere. Set `api_key = "$VAR_NAME"` to read from an environment variable, or set a literal key directly. Config Web accepts the same two forms in its API Key box.

### Google Gemini provider

```toml
[model_settings.providers.gemini-main]
type = "gemini"
api_key = "$GEMINI_API_KEY"

[model_settings.model]
provider = "gemini-main"
model = "gemini-3.8-flash"
api_mode = "interactions"  # or "interactions_stateful"
reasoning_effort = "low"  # gemini-3.8-flash: low/medium/high
max_response_tokens = 8192
# Optional model metadata overrides
# context_window = 1048576
# model_max_output_tokens = 65536
```

**Gemini models**:
- `gemini-3.8-flash`: Most capable Flash model for complex tasks
- `gemini-3.7-flash`, `gemini-3.6-flash`, `gemini-3.5-flash`: Earlier Flash generations
- `gemini-2.5-flash`, `gemini-2.5-pro`: Gemini 2.5 series models

**Thinking/reasoning**: Gemini 3.x and 2.5 models support internal reasoning through `reasoning_effort`:
- `minimal`: Fastest, least reasoning; supported by Gemini 3.6 and 3.5 Flash
- `low`: Balanced speed and quality (default for voice)
- `medium`: More thorough reasoning
- `high`: Maximum reasoning depth

Gemini 3.8 Flash, 3.7 Flash, and the Gemini 2.5 models use `low`, `medium`, or `high` in native Interactions mode. Gemini 3 models and Gemini 2.5 Pro cannot disable thinking.

**API key**: Get from [Google AI Studio](https://aistudio.google.com/apikey)

**Native Interactions API**: `interactions` submits the complete local transcript as Gemini StepList input with `store=false`; `interactions_stateful` stores the interaction and continues with `previous_interaction_id`. Both use `https://generativelanguage.googleapis.com/v1beta/interactions` and authenticate with `x-goog-api-key`. Native generation settings `thinking_level`, `thinking_summaries`, `max_output_tokens`, `seed`, `stop_sequences`, and `tool_choice` are sent under `generation_config`. `store=true` retains the interaction on Google's servers, so use the local mode when server-side retention is not desired.

**Only the native API is supported.** `api_mode` accepts `interactions` or `interactions_stateful`, and an unset `api_mode` selects `interactions`. The OpenAI-compatible `/chat/completions` path is deliberately not wired up for this provider: it cannot round-trip the `thought_signature` that Gemini 3 models require on replayed function calls, so multi-turn tool use fails there with `400 Function call is missing a thought_signature`. Google also documents `generateContent` as legacy and recommends calling the native API directly. Setting `api_mode = "chat_completions"`, `responses`, or `responses_stateful` on a `gemini` provider is rejected at config validation.

**Base URL**: Defaults to `https://generativelanguage.googleapis.com/v1beta`. It can be overridden with `base_url` for a gateway that speaks the native Interactions protocol.

### STT voice mode

```toml
[basic_settings.language_timezone]
locale = "en-US"
timezone = "UTC"

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
voice_progress_speech_enabled = true
voice_max_response_tokens = 300

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

## Grouped runtime fields

### General

| Field                       | Default / allowed values    | Description                                                                                                                                                                                               |
| --------------------------- | --------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `locale`                    | `en-US` (default) / `zh-CN` | Device-level language for Config Web and user-facing Agent responses, including progress messages and `<tts>` content. This is independent from `[voice_settings.classic.stt].language`, which only controls speech recognition. |
| `timezone`                  | `UTC` (default) / supported IANA zone | Controller time zone used for the model-facing current date, `controller_timezone` state, and shell child processes. Config Web provides the supported IANA zone list. |
| `prompt`                    | -                           | Prompt appended after the built-in Agent instruction at runtime. The built-in instruction is not configurable. |
| `max_iterations`            | `-1`                        | Maximum number of tool-call loops per run; `-1` means unlimited                                                                                                                                           |
| `mcp_enabled`               | `true`                      | When on, the Agent serves Streamable HTTP MCP at `POST /mcp` on port 8080. Config Web exposes this under Advanced Settings. See [MCP](mcp.md).                                                              |
| `mcp_shell_enabled`         | `false`                     | When on, the MCP allowlist includes the `shell` tool. TOML-only in this release (not on the Config Web Advanced page).                                                                                      |
| `mcp_config_web_base_url`   | `http://127.0.0.1`          | Loopback HTTP base URL used by MCP `device_status` / `support_bundle`. Non-loopback values are rejected.                                                                                                   |
| `context_prune_threshold`   | `0.5`                       | Fraction of the usable model input budget that triggers deterministic cleanup of stale state snapshots and older tool exchanges, including while one long-running tool loop is still executing. It cleans down to 6/7 of the trigger (so the default cleans from 50% to ~43%). Must be `0` or within `(0, 1)`; `0` (or an omitted value) uses `0.5`. The effective value is capped at `context_compaction_threshold`, so this cheap deterministic pass always gets a chance to free tokens before the LLM summary runs. A value of `1` or greater is rejected, as are `nan` and `inf`; a legacy absolute token count (for example `12000`) is detected on load, logged, and replaced by the default. |
| `context_compaction_threshold` | `0.8`                    | Fraction of the usable model input budget at which the conversation is summarized into a compaction message. Must be `0` or within `(0, 1)`; `0` (or an omitted value) uses `0.8`. Values of `1` or greater, `nan`, and `inf` are rejected. Compaction itself has no token target: the transcript is reduced structurally by retaining head and tail messages and replacing the middle with one LLM summary, so the post-compaction size follows from the summary rather than from a budget. |
| `screenshot_keep_n`         | `3`                         | Number of most recent screenshots to keep when pruning screenshots from the LLM context; unset or `0` uses the default                                                                                    |
| `screenshot_prune_interval` | `2`                         | Once screenshots exceed `screenshot_keep_n + screenshot_prune_interval`, replace old screenshots with placeholders in batches; unset or `0` uses the default                                              |
| `input_mode`                | unset / `stt` / `realtime`   | Voice mode: unset leaves voice unconfigured (HTTP/Web UI only), `stt` runs the STT/TTS voice loop, `realtime` uses a realtime voice model. `stt` requires `[voice_settings.classic.stt]` and `[...tts]`; `realtime` requires a realtime credential |

Before each model request, the Agent prunes stale state and older completed
tool-call/result pairs, normally protecting the latest three exchanges. It then
applies threshold-based conversation summarization and rechecks any rewritten
context. The hard input-budget check includes tool schemas and can prune the
protected exchanges if necessary. If deterministic pruning is insufficient, it
attempts a conversation summary even when provider-managed compaction is enabled.
If the request still cannot fit, the run returns a local budget error; a failed
hard-budget preparation does not activate its candidate session revision.
Its summary chunk is staged in memory and only persisted after the revision is
accepted and activated, so rejected recovery attempts do not create duplicate
searchable history. A chunk persistence failure is logged without undoing the
accepted revision; the full history remains in the parent transcript.
Successful revisions preserve the original transcript on disk and reset provider
response anchors.

### Quick Capture

| Field                           | Default                                                     | Description                                                                                                                                                                            |
| ------------------------------- | ----------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `quick_capture.enabled`         | `true`                                                       | Enables GPIO-triggered Screen Memory capture; legacy GPIO32/GPIO33 wakeup remains independent                                                                                           |
| `quick_capture.gpio_pin`        | `3`                                                         | Falling-edge GPIO for Quick Capture; supported values are `3` (physical pin 38) and `0` (disabled), while GPIO32/GPIO33 remain reserved for legacy wakeup                          |
| `quick_capture.screen_memory_ttl` | `90d`                                                    | Retention period for captured Screen Memory entries, or `forever`                                                                                                                       |

### Voice & VAD

Unless noted otherwise, these fields apply to the `stt` input mode.
`tap_wakeup_enabled` also applies in `realtime`: it gates the hardware tap
button in both modes.

| Field                           | Default                                                     | Description                                                                                                                                                                            |
| ------------------------------- | ----------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `vad_backend`                   | `rknn`                                                      | VAD backend: `rknn` uses NPU encoder + CPU LSTM/decoder, `cpu` uses a pure-CPU helper                                                                                                  |
| `vad_model_path`                | `/usr/lib/aiden/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn` | Silero VAD RKNN encoder model path; not used when `vad_backend="cpu"`                                                                                                                  |
| `vad_helper_path`               | `/usr/lib/aiden/rknn_vad`                                     | VAD helper executable path; the CPU backend defaults to `/usr/lib/aiden/cpu_vad`                                                                                                         |
| `vad_speech_threshold`          | `0.5`                                                       | Silero VAD speech probability threshold                                                                                                                                                |
| `silence_ms`                    | `550`                                                       | How many milliseconds of silence before an utterance is considered finished                                                                                                            |
| `min_speech_ms`                 | `300`                                                       | Minimum valid speech duration                                                                                                                                                          |
| `tap_wakeup_enabled`            | `true`                                                      | When on, the hardware tap button can wake Aiden in Classic STT and Realtime. Turning it off disables only that button path—not all wakeup. Realtime `/api/chat` can still activate a session; Classic STT normally needs the button |
| `voice_followup_enabled`        | `false`                                                     | Enable continuous follow-up after a single wakeup in wakeup mode; defaults to one wakeup per turn                                                                                      |
| `voice_followup_timeout_ms`     | `5000`                                                      | Window to wait for a user follow-up after the Agent replies                                                                                                                            |
| `voice_first_turn_timeout_ms`   | `10000`                                                     | Window to wait for the first utterance after wakeup                                                                                                                                    |
| `voice_max_turns`               | `0`                                                         | Maximum turns per wakeup session; `0` means unlimited                                                                                                                                  |
| `voice_interrupt_on_wakeup`     | `true`                                                      | When a wakeup is received again within a session, cancel thinking/TTS and listen again; repeated wakeups during the listening or recording phase are merged or ignored                 |
| `voice_streaming_tts_enabled`   | `true`                                                      | Feed the LLM streaming output into TTS sentence by sentence, reducing the wait before the first sentence plays                                                                         |
| `voice_tool_call_speech`        | `true`                                                      | Whether to asynchronously read the `content` of a tool-call event; this content comes only from the assistant content in the same LLM tool-call response, and stays silent when absent |
| `voice_progress_speech_enabled` | `true`                                                      | Whether to announce a short progress message when a todo item enters `in_progress`; todo state is still sent to the UI/trace                                                           |
| `voice_max_response_tokens`     | `300`                                                       | Per-turn output token limit for voice replies (must be `>= 0`)                                                                                                                         |

The model pointed to by `vad_model_path` must first be converted from the Silero ONNX to RV1106 RKNN on a PC using `silero-vad/convert_silero_vad_to_rknn.py`, then placed at the corresponding path on the device. The CPU backend requires `silero_vad_6_2_lstm_decoder_weights.bin` to include the Conv1d encoder extension, which can be generated from the TorchScript file shipped with the repo using `silero-vad/export_silero_vad_v6_2_weights.py`.
When `vad_helper_path` is still the built-in default, switching `vad_backend` automatically switches the helper; only when set to a custom path does it run that custom path.

## `[conversation_settings.termination_policy]`

The termination policy prevents stalled runs from looping indefinitely. These
fields can be edited directly in `agent.toml`; omitted or zero-valued numeric
fields use the defaults below.

```toml
[conversation_settings.termination_policy]
enabled = true
max_seconds = 0
repeat_action_limit = 3
same_result_limit = 3
screen_unchanged_limit = 5
soft_notice_stall_score = 2
restrict_tools_stall_score = 4
terminate_stall_score = 6
parse_failure_limit = 3
```

| Field                        | Default | Description                                                                                                 |
| ---------------------------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `enabled`                    | `true`  | Enable tiered loop detection and graceful termination                                                       |
| `max_seconds`                | `0`     | Wall-clock budget per instruction; `0` disables the time budget, and a consumed steer starts a fresh budget |
| `repeat_action_limit`        | `3`     | Stop after this many identical tool calls with identical results                                            |
| `same_result_limit`          | `3`     | Number of repeated identical results considered stalled                                                     |
| `screen_unchanged_limit`     | `5`     | Stop after this many UI actions without a screen change                                                     |
| `soft_notice_stall_score`    | `2`     | Stall score that injects a one-shot strategy-change notice                                                  |
| `restrict_tools_stall_score` | `4`     | Stall score that temporarily blocks repeated UI action tools                                                |
| `terminate_stall_score`      | `6`     | Stall score that ends the run gracefully                                                                    |
| `parse_failure_limit`        | `3`     | Stop after this many consecutive unparseable model outputs                                                  |

The three stall-score thresholds must satisfy
`soft_notice_stall_score < restrict_tools_stall_score < terminate_stall_score`.

## `[model_settings.providers.<name>]`

Optional named provider configurations. Each section holds the credentials for
one endpoint, and `[model_settings.model]` references it by putting the name in its `provider`
field. This lets several providers stay configured at once
so switching is a one-line change instead of a re-entry of keys.

| Field       | Description                                                                                        |
| ----------- | -------------------------------------------------------------------------------------------------- |
| `type`      | Required provider type: `openai`, `anthropic`, `openrouter`, `kimi`, `kimi-cn`, `volcengine`, `deepseek`, `gemini`, `ollama`, `fake`   |
| `api_key`   | Literal API key, or `$VAR_NAME` to read it from an environment variable                              |
| `base_url`  | Custom endpoint; supported by `openai`, `anthropic`, `gemini`, and `ollama`                         |

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

On load, a `provider` value that names a section under `[model_settings.providers]` is replaced
by that section's provider type. Its `api_key` fills an empty `[model_settings.model].api_key`,
while its `base_url` always controls the endpoint. A legacy `[model_settings.model].base_url`
is ignored; configure custom endpoints only through the selected
`[model_settings.providers.<name>].base_url`. A `provider` that matches no section is
treated as a provider type, so existing configs keep working unchanged.

`token_env` is not supported. Replace it with `api_key = "$VAR_NAME"`.

Only the `[model_settings.providers.<name>]` namespace is supported. The former
`[providers.<name>]` namespace is rejected with an error. The record-level
`provider` field is still accepted as a read-time alias for `type`; saving always
writes `type`, and `type` wins when both fields are present.

A `provider` that is neither a section name nor a known provider type is
rejected at load, so a typo or a reference left behind after deleting a section
fails with a clear error instead of surfacing later when the model client is
built. When a section is named exactly like a provider type, the section wins.

## `[model_settings.model]`

| Field                     | Description                                                                                                                                                                                                                                          |
| ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `provider`                | A provider type, or the name of a `[model_settings.providers.<name>]` section. Types: `openai`, `anthropic`, `openrouter`, `kimi`, `kimi-cn`, `volcengine`, `deepseek`, `gemini`, `ollama`, `fake`. `kimi` targets the Moonshot global site (`https://api.moonshot.ai/v1`) and `kimi-cn` targets the mainland China site (`https://api.moonshot.cn/v1`); `volcengine` targets Volcengine Ark (`https://ark.cn-beijing.volces.com/api/v3`); `deepseek` targets DeepSeek's OpenAI-compatible endpoint (`https://api.deepseek.com`); `gemini` targets the native Interactions endpoint (`https://generativelanguage.googleapis.com/v1beta`) and has no OpenAI-compatible transport. |
| `model`                   | Model name; usually required except for `fake`                                                                                                                                                                                                       |
| `api_key`                 | API key written directly                                                                                                                                                                                                                             |
| `api_mode`                | Wire protocol. For providers with a Chat Completions transport, omit it (or use `chat_completions`) for that path; `responses` sends full local context to OpenAI, OpenRouter, Volcengine Ark, or DeepSeek. `responses_stateful` chains stored Responses with `previous_response_id` where supported. Gemini defaults an omitted value to `interactions` (native local StepList, `store=false`) and also supports `interactions_stateful` (native stored interaction, `previous_interaction_id`); it rejects `chat_completions`. The local transcript remains authoritative for audit, compaction, session rotation, and recovery. |
| `responses_context_management` | Provider-side Responses context policy. `compaction` sends OpenAI's token-based compaction array; `ark_context_edit` sends Volcengine Ark's object-shaped `context_management.edits` policy; empty/`disabled` omits provider context management. DeepSeek does not support provider-side context management. This policy is independent from local historical state/tool-result pruning. |
| `responses_compact_threshold` | Optional token threshold sent with provider compaction. `0` lets the provider choose. |
| `responses_context_edit_trigger` | Ark tool-call count that triggers `clear_tool_uses`; `0` uses the recommended value `10`. |
| `responses_context_edit_keep` | Ark recent tool-call count to retain after cleanup; `0` uses the recommended value `3`. |
| `responses_context_edit_clear_thinking` | When true, adds Ark's `clear_thinking` edit and removes previous thinking turns. |
| `responses_truncation` | OpenAI-compatible Responses truncation policy. Empty/`disabled` preserves the API default; `auto` lets OpenAI or OpenRouter discard the oldest input. This field is not sent to Ark or DeepSeek. |
| `responses_include` | Optional array of provider-supported Responses include values. In stateless reasoning mode, use `reasoning.encrypted_content` when supported so Aiden can replay the complete opaque reasoning item. DeepSeek does not support `include`; its plain-text reasoning items are replayed directly. Aiden uses `previous_response_id` for provider-managed chaining and intentionally does not expose the separate `conversation` resource ID: the local session transcript remains authoritative and must not be shared across sessions accidentally. |
| `temperature`             | Sampling temperature. When unset, the default is model-dependent. Kimi K3 uses its required value, Gemini 3 models use Google's documented default of `1.0`, and other non-Gemini providers fall back to `0.2`. Native Gemini models without a registered default omit `generation_config.temperature` so Google can choose the model default. An explicit value normally takes precedence and is forwarded to Gemini; Google warns that lowering Gemini 3 temperature from `1.0` may cause looping or degraded performance on complex reasoning and math. DeepSeek thinking mode does not use temperature, so Aiden omits it whenever `reasoning_effort` is not `none`. |
| `reasoning_effort`        | Reasoning effort. Unset is auto. Native Anthropic maps supported effort values to adaptive thinking `output_config.effort` and preserves signed thinking blocks across tool-call turns. Gemini maps the shared model-specific effort values to native Interactions `generation_config.thinking_level`; Gemini 3.8/3.7 and Gemini 2.5 accept `low`, `medium`, or `high`. `minimal` is supported by OpenRouter, Volcengine Ark, and selected Gemini 3 models. `none` is supported by OpenRouter, OpenAI, Kimi, DeepSeek, Ollama, and the fake provider, but not by native Anthropic, Ark, or Gemini Interactions. DeepSeek defaults to `none` for faster device interactions; explicit `low`, `high`, or `max` enables thinking with `reasoning_content` replay. Other models may also pin a lighter default in `model_specs.go`; an explicit value always wins. |
| `reasoning_budget_tokens` | Optional exact reasoning-token budget for models that expose a numeric budget. `0` uses the model default or effort preset. It is currently translated only to Anthropic's native `thinking.budget_tokens` field. |
| `max_response_tokens`     | Maximum output tokens passed to the model on request                                                                                                                                                                                                 |
| `context_window`          | Optional total context window override in tokens. Unset or `0` uses provider metadata for OpenRouter/Ollama when available, then the built-in registry, then memory fallback.                                                                        |
| `model_max_output_tokens` | Optional advertised max output override in tokens. Unset or `0` uses provider metadata when fetched, then the built-in registry.                                                                                                                     |

### Moonshot Kimi K3

Use the dedicated `kimi` (global) or `kimi-cn` (mainland China) provider. Each has a built-in Moonshot OpenAI-compatible endpoint, so only `model` and the API key are required. The `kimi-k3` context window and max output are in the built-in registry, so the metadata overrides can stay unset.

Moonshot's official endpoint implements OpenAI-compatible Chat Completions, not
the Responses API. Keep `api_mode` unset (or set it to `chat_completions`). A
third-party protocol-conversion gateway can instead be configured as a custom
`openai` provider if it genuinely exposes `/responses`. Kimi thinking models
require preserved thinking across multi-turn and tool-call requests, so Aiden
replays each returned assistant `reasoning_content` field through the shared
compatible transport. See the official [Kimi Thinking Models](https://platform.kimi.ai/docs/guide/use-thinking-models)
guide.

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

### Volcengine Ark (Doubao)

Use the `volcengine` provider. It targets Ark's OpenAI-compatible endpoint
(`https://ark.cn-beijing.volces.com/api/v3`), so only `model` and the API key are
required. `model` is the Ark model ID, and `api_key` is an Ark API key.

```toml
[model_settings.model]
provider = "volcengine"
model = "doubao-seed-2-1-pro-260628"
api_key = "ARK_API_KEY"
```

Ark supports `responses_stateful` with `store=true` and
`previous_response_id`; `caching` is a separate optional cost/latency feature
and is not required for stored response chaining. Ark's request schema is not
identical to OpenAI's: Aiden keeps supported `instructions`, `include`, and
`reasoning` fields, but does not send OpenAI's compaction array or `truncation`
field to Ark. Set `responses_context_management = "ark_context_edit"` to send
Ark's object-shaped `context_management` edit schema. The default edit clears
all old tool inputs after 10 tool calls while retaining the latest 3; the
optional `clear_thinking` edit removes previous thinking turns as well.
An older provider record saved as `type = "openai"` with the standard Ark host
is recognized automatically and uses the same compatibility profile.

To read the key from the environment instead of writing it here, put it on a
named provider and reference that:

```toml
[model_settings.providers.ark]
type = "volcengine"
api_key = "$ARK_API_KEY"

[model_settings.model]
provider = "ark"
model = "doubao-seed-2-1-pro-260628"
```

Ark also exposes an Anthropic-protocol endpoint at `/api/compatible`. This agent
always speaks the OpenAI-compatible protocol, so use the `/api/v3` path above.

`reasoning_effort` accepts `minimal` (no reasoning), `low`, `medium`, and `high`.
Ark treats an omitted value as `high`, which delays the first streamed token by
several seconds; the built-in registry therefore pins `low` as the default for
`doubao-seed-2-1-pro-260628` so voice replies stay responsive. Set the field
explicitly to override. Note that `none` is not an Ark level — use `minimal`.

The context window and max output for `doubao-seed-2-1-pro-260628` are in the
built-in registry, so the metadata overrides can stay unset. Other dated releases
are not registered; for those, set `context_window` and
`model_max_output_tokens` explicitly, or add a registry entry keyed by the exact
Ark model ID.

The `[voice_settings.classic.tts]` section has an unrelated provider that is also named `volcengine`. It
speaks a separate WebSocket protocol with its own host and credentials, so an Ark
API key and base URL do not carry over to it.

### DeepSeek

Use the `deepseek` provider for DeepSeek's OpenAI-compatible endpoint. Aiden
presets `deepseek-flash`, which supports vision and tool calls with a 1M context
window. The `deepseek-v4-pro` preset is commented out because it does not support
vision; it can be restored once vision support is verified.
DeepSeek defaults to non-thinking mode in Aiden with `reasoning_effort = "none"`.
Set `reasoning_effort` to `low`, `high`, or `max` to enable thinking. In Chat
Completions mode, Aiden sends DeepSeek's `thinking` toggle and replays assistant
`reasoning_content` from the transcript on subsequent tool-call requests. In
Responses mode, it sends `reasoning.effort` and replays the returned reasoning,
message, and executed function-call items through Aiden's local context. Since
DeepSeek does not apply temperature in thinking mode, Aiden omits the field for
both transports while preserving it in non-thinking mode.

Set `api_mode = "responses"` to use DeepSeek's stateless `/responses` endpoint.
DeepSeek does not support `responses_stateful`, `store`, or
`previous_response_id`; Aiden therefore keeps and resubmits the full local
transcript. The provider uses its built-in endpoint, so `base_url` is not
configurable.

```toml
[model_settings.providers.deepseek-main]
type = "deepseek"
api_key = "$DEEPSEEK_API_KEY"

[model_settings.model]
provider = "deepseek-main"
model = "deepseek-flash"
api_mode = "responses"
```

Official references: [model capabilities and limits](https://api-docs.deepseek.com/quick_start/pricing),
[vision](https://api-docs.deepseek.com/guides/vision),
[thinking with tool calls](https://api-docs.deepseek.com/guides/thinking_mode),
[Responses API compatibility](https://api-docs.deepseek.com/guides/responses_api).

## `[advanced_settings.log]`

| Field                     | Default | Description                                                                                                                                              |
| ------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `level`                   | `info`  | Minimum Agent log severity: `debug`, `info`, `warn`, or `error`. Changes apply without restarting the Agent.                                             |
| `llm_http_retention_days` | `7`     | Number of days to keep raw LLM HTTP logs under `<config_dir>/log` (`llm-http-*.log`). Cleanup runs when the agent starts; unset or `0` uses the default. |
| `log_raw_http`            | `true`  | Record detailed model HTTP requests and responses. This maps to the runtime model transport logger but is stored in the Advanced Settings log group.      |

## `[voice_settings.classic.audio]`

| Field              | Default                                 | Description                                                                                                                                                                                                                   |
| ------------------ | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `socket`           | `/run/audio_service/audio_service.sock` | Audio Service socket                                                                                                                                                                                                          |
| `sample_rate`      | `16000`                                 | Sample rate                                                                                                                                                                                                                   |
| `channels`         | `1`                                     | Number of channels                                                                                                                                                                                                            |
| `bit_width`        | `16`                                    | Bit width                                                                                                                                                                                                                     |
| `backend`           | `auto`                                  | Recording and playback backend. `auto` uses `audio_service` on the board and host recorder/player commands in desktop/PC mode through the ADB input backend or environment bridge. Use `audio_service` or `local` to force both directions to one backend. |

## `[voice_settings.realtime]`

This section selects a named realtime provider record used after a GPIO wakeup.
It is active when `input_mode = "realtime"`; the
mode, not API key presence, controls whether the daemon starts the realtime
path. The daemon streams microphone PCM to the selected adapter and plays its
response PCM through the board audio path.
For providers with text-input capability, `/api/chat` can start a session and send the queued text as its first user message. Speko S2S is audio-first; its delegated native provider may expose optional text capabilities, but the board microphone path remains the canonical full-duplex path. The selected direct provider owns turn detection and interruption; Speko does not relay PCM or synthesize a separate VAD loop.
Use `input_mode = "stt"` to select the existing VAD/STT/LLM/TTS wakeup loop.
Config Web renders the selector when `agent.input_mode = "realtime"`. Provider
credentials and model settings live in `[voice_settings.realtime.providers.<name>]`, so
switching the selector never overwrites another provider's saved configuration.
The current adapters are Qwen, Speko S2S, OpenAI Realtime, Google Gemini Live, and xAI Grok Voice.
The session always starts with the built-in realtime voice instruction. If
`[conversation_settings.agent].prompt` is non-empty, it is appended after that
base instruction; it does not replace it.

| Field | Default | Description |
| ----- | ------- | ----------- |
| `provider` | `qwen` | Named `[voice_settings.realtime.providers.<name>]` record. Bare `qwen`, `speko`, `openai`, `gemini`, or `xai` values remain accepted for compatibility. |
| `enable_speech_emotion` | `true` | Enable realtime speech emotion. |
| `input_audio_format` / `output_audio_format` | `pcm` | Audio formats accepted by the realtime API. |

Legacy Qwen `turn_detection*` fields under this section are migrated to the selected Qwen provider record when the configuration is loaded or next saved.

## `[voice_settings.realtime.providers.<name>]`

Realtime provider records follow the same named-record pattern as
`[model_settings.providers.<name>]`. Multiple configurations of each realtime provider can coexist;
`[voice_settings.realtime].provider` selects one by name.

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

[voice_settings.realtime]
provider = "speko-main"
```

| Field | Providers | Description |
| ----- | --------- | ----------- |
| `type` | all | Adapter type: `qwen`, `speko`, `openai`, `gemini`, or `xai`. |
| `api_key` | all | Provider credential; supports `$ENV_VAR` expansion. For Gemini Vertex, this is an OAuth access token. |
| `model` / `voice` | all | Provider-specific model and voice. Speko requires an explicit model; voice may stay empty for the selected upstream default. |
| `workspace_id` / `region` | Qwen | Optional DashScope routing settings. |
| `turn_detection` | Qwen | Qwen turn detector: `server_vad` (default) or `smart_turn`. Config Web exposes this under the provider's Advanced Settings. |
| `turn_detection_threshold` | Qwen `server_vad` | Optional Qwen VAD threshold; leave unset to use the service default. Ignored by `smart_turn`. |
| `turn_detection_silence_ms` | Qwen `server_vad` | Optional silence duration before Qwen completes a turn; leave unset to use the service default. Ignored by `smart_turn`. |
| `auth_mode` | Gemini | `api_key` (default) for the Gemini Developer API, or `vertex` for Vertex OAuth. |
| `project_id` / `location` | Gemini Vertex | Required Google Cloud project and Vertex region, for example `us-central1`. |
| `endpoint` | Qwen, OpenAI, Gemini, xAI | Optional WebSocket endpoint override, primarily for regional gateways and protocol tests. |
| `realtime_protocol` | OpenAI | OpenAI Realtime wire schema: empty or `ga` (default) uses the current GA session payload; `legacy` (alias `beta`) uses the older `modalities`, `input_audio_format`, and `output_audio_format` fields required by some compatible gateways. Set this explicitly; the endpoint URL is never used to infer the protocol. |
| `use_backend_agent` | realtime mode (`[voice_settings.realtime]`) | Controls tool ownership: omitted/`false` (default) disables agent communication tools and exposes the runtime tools that the backend agent would otherwise use directly to the realtime model; `true` enables the backend agent and exposes its communication tools (`*_agent_task`). This setting does not control provider reasoning, which remains a provider/model capability. |
| `thinking_level` | Gemini | Thinking depth for Live Extended Thinking models: `LOW` (default), `MINIMAL`, `MEDIUM`, or `HIGH`. Only applies to models that support thinking; other models ignore it. |
| `upstream_provider` | Speko | Required S2S upstream: `google` (or `gemini`) or `xai`, paired with `model`. Automatic routing is disabled because it may select an unsupported WebRTC route. OpenAI is not a supported Speko route in Aiden; use the top-level `openai` provider instead. |
| `agent_id` / `base_url` | Speko | Optional Speko agent ID and API base URL override. |

## `[advanced_settings.hardware.frame_service]`

| Field | Default | Description |
| --- | --- | --- |
| `keep_streamon` | `false` | When `true`, Frame Service keeps capture STREAMON between screenshots and discards 6 warm-up frames for each request. When `false`, it pauses between screenshots and uses 0 warm-up frames. |

## `[memory_settings.notification]`

Voice notifications attach system reminders to a normal spoken reply or replace a final failed LLM turn with a fixed error message. In Realtime mode, an idle session can also announce a pending reminder as a private speech response. See [Voice Notifications](voice-notifications.md) for the lifecycle and delivery contract.

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

| Field                                | Default         | Description                                                               |
| ------------------------------------ | --------------- | ------------------------------------------------------------------------- |
| `enabled`                            | `true`          | Enable persistent tails and final-turn replacements                       |
| `max_pending`                        | `8`             | Maximum active condition records kept by the in-memory manager            |
| `retention_days`                     | `14`            | Days to keep processed notification memory during normal storage cleanup  |
| `response_tail.enabled`              | `true`          | Allow persistent reminders to be appended to normal replies               |
| `response_tail.max_items`            | `1`             | Maximum reminders per reply; the current implementation supports only `1` |
| `response_tail.max_text_chars`       | `40`            | Maximum reminder length in Unicode characters                             |
| `expiration.default_ttl_seconds`     | `0`             | Default active-condition lease; `0` disables automatic expiration         |
| `expiration.code_ttl_seconds.<code>` | `storage = 900` | Per-code lease override renewed by each active heartbeat                  |

Config Web exposes `retention_days` in Memory Settings. The lifecycle and lease fields remain available through manual TOML editing.

## `[basic_settings.device]`

| Field         | Default | Description |
| ------------- | ------- | ----------- |
| `device_type` | `iOS`   | Target host type for USB HID descriptors and Agent global device state. Accepted values: `iOS`, `Android`, `macOS`, `windows`, `linux`. `Android` derives HID `pointer_mode = "touchscreen"`; every other value derives `pointer_mode = "absolute"`. Switching between Android and a non-Android type requires a reboot so USB descriptors are re-enumerated; changes among non-Android types apply online. |

## `[basic_settings.device.hid]`

| Field             | Default  | Description |
| ----------------- | -------- | ----------- |
| `keyboard_layout` | `qwerty` | How the phone interprets the external USB HID keyboard: `qwerty`, `azerty`, or `qwertz`. See [USB HID](../03-services/usb-hid.md). |

## `[advanced_settings.hardware.hid]`

This table is retained for internal HID device settings and compatibility with
older configurations. Put the user-facing `keyboard_layout` value under
`[basic_settings.device.hid]`; when both tables are present, the Basic Settings
value is authoritative.

| Field                     | Default                                 | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------------------------- | --------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `keyboard_device`         | `/dev/hidg0`                            | Keyboard HID device                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `mouse_device`            | `/dev/hidg1`                            | Mouse/touch HID device                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `android_keyboard_device` | `/dev/hidg2`                            | Consumer Control HID device (`hid.usb2`) used for Android extension keys when `[basic_settings.device].device_type = "Android"` and media/volume/brightness/screenshot keys for other device types                                                                                                                                                                                                                                                                                                                                          |
| `frame_socket`            | `/run/frame_service/frame_service.sock` | Frame Service socket used by the screenshot tool                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `input_backend`           | `hid`                                   | Low-level input backend for click/touch/keyboard tools. `hid` writes USB HID reports; `adb` uses the paired Android ADB connection and `adb shell input`/ADBKeyboard commands. Atomic touch programs additionally try `getevent`/`sendevent`, then fall back to `input touchscreen motionevent` when raw event injection is blocked. |

## `[voice_settings.classic.tts.providers.<name>]` and `[voice_settings.classic.stt.providers.<name>]`

Named voice provider configurations, the same shape `[model_settings.providers.<name>]` gives
`[model_settings.model]`. Each section holds the credentials and settings for one voice service,
and `[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` reference one by putting the name in their own `provider`
field. Several providers stay configured at once, so switching is a one-line
change instead of a re-entry of keys.

Unlike `[model_settings.providers.<name>]`, these are separate namespaces: the `[voice_settings.classic.tts]`
`volcengine` provider speaks a different protocol with its own host and
credentials than the Ark LLM provider of the same name, so one map could not
serve both. Each namespace also validates its own provider types — a TTS type is
rejected for `[model_settings.model]` and vice versa.

Several records may share one provider type, which is how two accounts of the
same service (different keys, different voices) stay configured together.

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

### Field placement

A field lives on the record when it stops meaning anything once the provider
changes; it stays on `[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` when it holds regardless of provider.

| | Record fields | Stays on the shared section |
| ---- | ---- | ---- |
| TTS | `type`, `api_key`, `model`, `voice_id`, `emotion`, `reference_id` | `provider` (reference), `speed` |
| STT | `type`, `api_key`, `model`, `base_url`, `app_id`, `secret_id`, `secret_key`, `region`, `engine_model_type` | `provider` (reference), `language` |

`speed` is a listening preference and `language` a transcription preference:
neither should change because the voice changed, so both stay global.

For all provider records, `api_key` accepts either a literal key or `$VAR_NAME`.
Config Web stores exactly the same representation.

### Provider configuration compatibility

The Config Web request contract accepts only canonical provider records. Records
use `type`, while the selected record name remains in the parent section's
`provider` field. Retired record aliases and flat credential request fields are
rejected. Existing TOML files may still be normalized on save so credentials are
preserved while the file is converted to the canonical layout.

An unresolvable reference does not stop the device from booting: voice is
optional at runtime, so a stale name is reported and the agent starts without
voice. Config Web rejects such a reference when saving, while the form is
still on screen.

## `[voice_settings.classic.stt]` and `[voice_settings.classic.tts]`

`[voice_settings.classic.stt]` is required when `input_mode = "stt"`; `[voice_settings.classic.tts]` is required when `input_mode = "stt"`.

`provider` here is a reference to a `[voice_settings.classic.tts.providers.<name>]` /
`[voice_settings.classic.stt.providers.<name>]` record (a bare provider type still works — see above).
The provider-specific credentials listed below live on that record; Config Web
edits them in the provider dialog rather than on the `[voice_settings.classic.tts]` / `[voice_settings.classic.stt]` card.

STT:

- `provider = "openai-whisper"`: currently available;
- `provider = "openrouter"`: currently available, default endpoint is `https://openrouter.ai/api/v1/audio/transcriptions`, request body uses base64 WAV;
- `provider = "tencent-asr"`: Tencent Cloud Sentence Recognition (SentenceRecognition), uses `secret_id` / `secret_key`, no `base_url` needed; the legacy values `tencent` / `tencent_asr` are retained only as compatibility aliases;
- `provider = "qwen-asr"`: Alibaba Cloud DashScope Realtime ASR over WebSocket, default model `qwen3-asr-flash-realtime`; supports streaming upload and accepts optional `model` / `base_url` overrides;
- `provider = "google-cloud"`: Google Cloud Speech-to-Text REST API with API key authentication, default endpoint `https://speech.googleapis.com/v1/speech:recognize`; accepts optional `model` / `base_url` overrides and does not support streaming upload.

TTS:

- `provider = "minimax"`: Minimax WebSocket, global endpoint `api.minimax.io`;
- `provider = "minimax-cn"`: Minimax WebSocket, mainland China endpoint `api.minimaxi.com`;
- `provider = "fish-audio"`: Fish Audio WebSocket;
- `provider = "alicloud"`: Alibaba Cloud Qwen-TTS Realtime;
- `provider = "volcengine"`: Volcengine WebSocket bidirectional streaming V3. Currently only the new console's `X-Api-Key` authentication is supported: `api_key` maps to `X-Api-Key`, `model` maps to `X-Api-Resource-Id` (default `seed-tts-2.0`), and `voice_id` maps to the speaker;
- `provider = "openrouter"`: OpenRouter HTTP speech API, default model `google/gemini-3.1-flash-tts-preview`; `model` and `voice_id` select the routed TTS model and its voice;
- `provider = "google-cloud"`: Google Cloud Text-to-Speech REST API with API key authentication; `voice_id` defaults to `en-US-Neural2-C`.

TTS configuration fields:

| Field          | Location                         | Description                                                                                                              |
| -------------- | -------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `provider`     | `[voice_settings.classic.tts]`                          | Required reference to a named provider record; a bare provider type remains supported for backward compatibility        |
| `api_key`      | `[voice_settings.classic.tts.providers.<name>]`         | Required authentication key for the selected provider                                                                   |
| `model`        | `[voice_settings.classic.tts.providers.<name>]`         | Optional Minimax model, Fish Audio model header, Alibaba Cloud Realtime model, Volcengine `X-Api-Resource-Id`, or OpenRouter model |
| `voice_id`     | `[voice_settings.classic.tts.providers.<name>]`         | Optional Minimax, Alibaba Cloud, OpenRouter, or Google Cloud voice; Volcengine speaker. Not used by Fish Audio (see `reference_id`) |
| `reference_id` | `[voice_settings.classic.tts.providers.<name>]`         | Optional Fish Audio reference id; defaults to the built-in demo voice shown by Config Web. Ignored by other providers    |
| `emotion`      | `[voice_settings.classic.tts.providers.<name>]`         | Optional Minimax emotion; Volcengine passes it through as `audio_params.emotion` and requires voice support             |
| `speed`        | `[voice_settings.classic.tts]`                          | Optional speech rate, default `1.0`; the supported range varies by provider, refer to the official docs                 |

The examples use placeholder keys to make the required record placement explicit.

Common TTS adapter configs:

| Provider       | `model` example                           | Voice/reference field                               | Description                                                                                                         |
| -------------- | ----------------------------------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `minimax`      | `speech-2.8-hd`                           | `voice_id = "male-qn-qingse"`                       | Minimax WebSocket via `api.minimax.io`; `emotion` is passed through to Minimax                                      |
| `minimax-cn`   | `speech-2.8-hd`                           | `voice_id = "male-qn-qingse"`                       | Minimax WebSocket via `api.minimaxi.com`; `emotion` is passed through to Minimax                                    |
| `fish-audio`   | `s2-pro`                                  | `reference_id = "98655a12fa944e26b274c535e5e03842"` | WebSocket live TTS; the shown reference is used by default, and `voice_id` is not used                              |
| `alicloud`     | `qwen3-tts-flash-realtime`                | `voice_id = "Cherry"`                               | DashScope Realtime; the adapter outputs 24 kHz PCM, automatically resampling when the sample rate differs           |
| `volcengine`   | `seed-tts-2.0`                            | `voice_id = "zh_female_vv_uranus_bigtts"`           | `model` maps to `X-Api-Resource-Id`, `voice_id` maps to the speaker, and the two must match                         |
| `openrouter`   | `google/gemini-3.1-flash-tts-preview`     | `voice_id = "Kore"`                                 | OpenRouter `/audio/speech`; voice defaults depend on the selected model and output is 24 kHz PCM                    |
| `google-cloud` | —                                         | `voice_id = "en-US-Neural2-C"`                      | Google Cloud Text-to-Speech REST API with API key authentication; output is 24 kHz PCM                              |

### Provider examples

Minimax WebSocket:

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

Fish Audio WebSocket:

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

Fish Audio `model` defaults to `s2-pro` and is sent as a WebSocket handshake header. An empty `reference_id` uses the built-in demo voice shown in Config Web; configure `reference_id` on the selected `[voice_settings.classic.tts.providers.<name>]` record to override it. `voice_id` is not used by Fish Audio and is ignored (this avoids inheriting a `voice_id` meant for another provider). In some networks, the public Fish Audio endpoint may require `ALL_PROXY` or `HTTPS_PROXY` in `/userdata/system/env`.

Alibaba Cloud Qwen-TTS Realtime:

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

The Alibaba Cloud adapter uses the DashScope WebSocket Realtime endpoint and outputs a fixed 24 kHz PCM; when the device playback sample rate differs, it automatically resamples.

Volcengine WebSocket bidirectional streaming V3:

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

For Volcengine, `api_key` is the new console's `X-Api-Key`, `model` is the `X-Api-Resource-Id`, and `voice_id` is the speaker. `voice_id` must match the resource corresponding to `model`; when they do not match, the server returns `resource ID is mismatched with speaker related resource`. A verified working voice example for `seed-tts-2.0` is `zh_female_vv_uranus_bigtts`.

### Switching providers at runtime

```bash
curl -X POST http://<device-ip>:8080/api/settings/tts \
  -H 'Content-Type: application/json' \
  -d '{"provider":"volcengine","voice":"zh_female_vv_uranus_bigtts"}'
```

If you need to store the keys of multiple providers in the same config, use named provider records. When switching providers via runtime POST, the corresponding record is read first, then overridden by the request body.

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

For the iOS companion app's Live Activity / Dynamic Island task status.
Snapshots are enabled by default and delivered locally through BLE Wake plus
USB ECM. See [Live Activity / Dynamic Island](./live-activity.md).

| Field     | Default | Description |
| --------- | ------- | ----------- |
| `enabled` | `true`  | Maintain local task-status snapshots and send coalesced `live_activity` BLE Wake notifications |

## Episode telemetry (Langfuse)

Optional. After a task ends, asynchronously report the full episode to Langfuse; see [telemetry-langfuse.md](./telemetry-langfuse.md) for details.

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
tags = ["aiden-hardware"]
```

## System environment variables

The Agent no longer reads `[proxy]` from `agent.toml`. Values in `/userdata/system/env` define the system/default upstream proxy. Outbound HTTP/WebSocket requests, shell tool subprocesses, OTA commands launched through `aiden-env-run`, and SSH login shells use the fixed local address `127.0.0.1:18080`; it selects the system upstream, direct mode, or a custom upstream from the active Wi-Fi's saved policy. The listener accepts both HTTP proxy and SOCKS5 protocols. Its generated environment keeps the local URL scheme aligned with the selected upstream, so a `socks5://` Wi-Fi proxy remains SOCKS5 on both sides of the local endpoint instead of being wrapped in HTTP CONNECT. The local URL uses `socks5h://` so hostname resolution also travels through SOCKS5 rather than depending on the board's DNS. The file is loaded with shell syntax, for example:

```sh
HTTP_PROXY=http://127.0.0.1:7890
HTTPS_PROXY=http://127.0.0.1:7890
NO_PROXY=localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7,fe80::/10
OPENROUTER_API_KEY=...
```

| Variable                      | Description                                                                                                                                        |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HTTP_PROXY` / `http_proxy`   | HTTP proxy URL, for example `http://127.0.0.1:7890`                                                                                                |
| `HTTPS_PROXY` / `https_proxy` | HTTPS proxy URL, usually the same HTTP proxy endpoint                                                                                              |
| `ALL_PROXY` / `all_proxy`     | Generic proxy used by HTTP clients and some WebSocket adapters                                                                                     |
| `NO_PROXY` / `no_proxy`       | Comma-separated bypass rules; when a proxy URL is set and no bypass value is present, the launcher injects the default private-network bypass list |

Per-Wi-Fi policies are configured in the Config Web connection dialog and
stored in `/userdata/system/wifi-proxies.json` with mode `0600`. Setting
`AIDEN_WIFI_PROXY_ENABLED=0` in the system environment is an emergency bypass
that restores direct use of the raw environment proxy variables after the
affected services or shell are restarted.

The selected local URLs are written to `/run/wifi_proxy/proxy-env`. New managed
commands and login shells read that file. If a Wi-Fi change switches between
HTTP and SOCKS5, `aiden-wifi-proxy.service` restarts the long-running Agent so its HTTP and
WebSocket clients also pick up the new URL scheme; changing only the upstream
host or port does not require an Agent restart.

For a custom per-Wi-Fi proxy, the dialog's `NO_PROXY` value belongs to that
SSID's custom upstream and replaces the environment `NO_PROXY` while the
custom policy is active. The system-default policy uses the proxy variables
and `NO_PROXY` from `/userdata/system/env` together; it stores no per-Wi-Fi
bypass value.

## `memory/extraction.yaml`

Optional. Place `memory/extraction.yaml` under the config directory to control session-memory compaction and chunk extraction. Missing files and invalid fields fall back to defaults. See [session-memory.md](./session-memory.md) for the full flow.

| Field                                | Default                 | Description                                                                                                                                                                                                                          |
| ------------------------------------ | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `reserve_tokens`                     | `8192`                  | Token headroom reserved below the active model context window. Compaction triggers when `prompt_tokens >= context_window - reserve_tokens`. The value is clamped to at most half of the window so small-window models remain usable. |
| `keep_recent_tokens`                 | `20000`                 | Approximate token budget for the hot window retained by token-based cut-point selection. It is clamped together with `reserve_tokens` to fit the active window.                                                                      |
| `hot_window_events`                  | `30`                    | Target number of recent events retained by the count fallback. Used only when prompt-token data is unavailable.                                                                                                                      |
| `count_compress_after_events`        | `hot_window_events * 2` | Event-count trigger used only when prompt-token data is unavailable. If omitted, it is derived from the normalized `hot_window_events`; explicit values must be greater than `hot_window_events`.                                    |
| `context_window`                     | `32000`                 | Fallback context window for compaction when the active model is not present in `model_specs`. Runtime normally derives this from `ModelResolver.Spec()`; this value is only used for unknown models.                                 |
| `compress_at_percent`                | `50`                    | Percentage trigger: compaction starts when `prompt_tokens / context_window >= compress_at_percent%`.                                                                                                                                 |
| `summary_max_chunks`                 | `10`                    | Number of chunk summaries kept in the Recent Chunks section of `summary.md`. Older entries move to the archive and are folded into the Rolling Summary.                                                                              |
| `session_boundary_enabled`           | `true`                  | Classify each new user turn as continuing the current session or starting a new one. A `new` boundary archives the current `memory/session/` directory and recreates an empty active session.                                        |
| `session_boundary_short_gap_seconds` | `300`                   | Gap below which a turn is treated as continuation regardless of lexical signals.                                                                                                                                                     |
| `session_boundary_long_gap_seconds`  | `1800`                  | Gap above which a turn is treated as a fresh session regardless of lexical signals.                                                                                                                                                  |
| `episode_memory_idle_delay_seconds`  | `300`                   | Idle time before eligible completed Episodes are consolidated into Device Memory in the background. Foreground tasks cancel or defer consolidation.                                                                                  |
| `tag_candidates`                     | see defaults            | Candidate keywords matched when tagging chunk summaries.                                                                                                                                                                             |
| `entity_suffixes`                    | `["App","app","APP"]`   | Suffixes recognized during entity extraction.                                                                                                                                                                                        |

## Known limitations

- `preferred_model` and `allowed_children` are currently parsed but not fully wired into execution;

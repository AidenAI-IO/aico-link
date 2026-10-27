---
sidebar_position: 17
---

# Realtime Voice Module

Realtime voice is the Agent's full-duplex audio path for
`input_mode = "realtime"`. The daemon keeps the microphone and speaker open
for one live session, while the selected provider performs turn detection,
speech generation, transcription, and (when supported) tool calls.

This page describes the repository implementation. Provider wire details are
documented separately in the [Qwen Realtime API reference](qwen-realtime/websocket-api.md);
the adapter package must not be treated as an OpenAI-compatible wire protocol.

## Responsibilities and boundaries

### Input transcription

Realtime audio understanding and user-facing transcription are separate
outputs of the same provider session. The provider consumes microphone audio
directly to produce a response; the final user transcript is used for Web
history and is not sent back as a second text prompt.

OpenAI requests `gpt-4o-mini-transcribe` in both GA and legacy session payloads.
Qwen, xAI, and Gemini use their provider-native transcription paths. The
ordinary `[stt]` configuration applies only to `input_mode = "stt"` and does
not configure a realtime session. The configured Agent locale controls the
realtime response language through the session instructions; it does not set
or restrict the provider's input transcription language.

OpenAI diagnostics record the requested and acknowledged transcription model,
speech start/stop, audio commit, final transcript or transcription failure,
response ID, and tool call IDs. These events may be omitted or reordered by a
provider, so diagnostics do not change response admission or commit a
server-VAD buffer a second time. Final transcript text is user data and must be
handled accordingly.

The realtime path is split into three layers:

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

`realtimevoice.Provider` and `Session` expose only semantics the daemon needs:
PCM upload, normalized events, session information, and close/error streams.
Authentication, endpoint construction, JSON framing, event names, and native
audio formats stay inside each adapter.

`ProviderRegistry` maps a provider name to a factory. `ProviderRegistry.Open`
then wraps the raw session in a managed session that:

- negotiates the device-facing input and output `AudioFormat`;
- resamples mono PCM16 when the device and provider rates differ;
- forwards normalized events without exposing provider JSON;
- refreshes the output resampler when Gemini announces a runtime rate; and
- reports optional capabilities from the actual session implementation.

The `Conversation` wrapper keeps optional operations explicit. Callers must
feature-detect these interfaces instead of assuming every provider supports
them:

| Capability | Purpose |
| --- | --- |
| `TurnCommitter` | Send a client-side end-of-turn marker when the provider requires it |
| `ResponseInterrupter` | Cancel/truncate a response during barge-in |
| `ToolResultSender` | Return a tool result on the live connection |
| `TextSession` | Inject text and explicitly request a response |
| `ContextReplayer` | Restore recent user context after reconnecting |

## Shipped adapters

| Adapter | Transport | Notes |
| --- | --- | --- |
| Qwen | JSON WebSocket | DashScope realtime protocol; server VAD/smart-turn, audio transcription, tools, and response cancellation |
| OpenAI | JSON WebSocket | Native OpenAI Realtime events; input transcription is enabled explicitly and response interruption includes truncation |
| xAI | JSON WebSocket | xAI realtime event aliases are normalized to the shared event model |
| Gemini | Gemini Live WebSocket | `setup`/`serverContent` protocol; input/output transcription and function responses; server VAD does not expose Aico speech boundary events |
| Speko S2S | HTTP mint + delegated provider WebSocket | Mints a short-lived provider credential, then uses the selected Gemini or xAI adapter; OpenAI/WebRTC routing is intentionally rejected |

Speko is a control-plane adapter, not another PCM relay. Its lease wrapper
emits `ErrSessionRotated` before expiry so the daemon can reconnect and replay
the persisted realtime context.

## Event and history lifecycle

The daemon consumes provider-neutral `Event` values. A typical audio turn is:

```text
audio → speech_started → speech_stopped → input_committed → transcript_final
      → response_started → transcript_delta/audio/tool_call
      → response_done or response_cancelled
```

Providers may omit or reorder parts of this sequence. Gemini, for example,
does not emit Aico speech boundary events, so the daemon uses a local energy
gate only to prevent background task updates from entering while the user is
speaking. It does not replace Gemini's server-side turn detector.

User final transcripts are appended to the realtime user context as soon as
they arrive. Assistant text/transcripts are persisted only for a completed,
non-suppressed response. Tool calls are executed by the background-capable
realtime tool executor; tool results are sent back through
`ToolResultSender`, and providers that require explicit continuation receive a
new `CreateResponse` after all results for that response have arrived.

Task updates and voice notifications are injected only when the session is
idle. Notification responses are private speech responses: they are played
through the speaker and acknowledged only after playback drains, but they are
not added to user-visible conversation history.

## Configuration

Use `[voice_settings.mode].input_mode = "realtime"` and select a named provider record:

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

Provider-specific credentials and routing fields belong under
`[voice_settings.realtime.providers.<name>]`; `[voice_settings.realtime]` contains the selected record
and session-wide options such as instructions and turn detection. See the
[Agent Configuration Reference](configuration.md#voice_settingsrealtime) for all fields
and provider visibility rules.

## Testing and extension points

Provider translators and wire framing have focused tests under
`src/agent/internal/agent/realtimevoice`. Managed-session tests cover media
conversion, event forwarding, and capability detection. Daemon tests cover
configuration, event-loop admission, playback, and notification behavior.

To add a provider:

1. Implement `Provider.Open` and a raw `Session` in `realtimevoice`.
2. Normalize native events into `Event` values and keep native payload types
   private to the adapter.
3. Implement only the optional capability interfaces the provider actually
   supports.
4. Register the factory in `DefaultProviderRegistry` and add its descriptor to
   the shared provider metadata.
5. Add protocol replay tests before enabling the provider in Config Web.

The old `rtclient` package was the Qwen/OpenAI-specific transport seam. It is
no longer a runtime dependency; new code belongs in `realtimevoice`, with
provider-specific protocol behavior kept in the corresponding adapter.

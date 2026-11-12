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
| Moss | JSON + binary WebSocket | MOSS-LIVE (`moss-live-1.0`); 16 kHz PCM uplink / 48 kHz PCM downlink; immediate playback chunk acks; client tools via `delegation=client` + OpenAI-style `function_call_output` / deferred `session.continue` (`ResponseContinuer`, not text injection); reserved names such as `web_search` are exported under an `aico_` alias; client tools are capped at 32; voice-only (no `/api/chat` text inject, no configurable `voice`, no camera/screen frames yet). `use_backend_agent` is ignored at runtime (effective `false`, stored value unchanged; Config Web hides the toggle). |

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

Speech and tool or background-task execution can overlap in one response, so
the realtime instructions require an outcome-claim gate: before a result showing
the actual outcome is available, the model may only acknowledge that it is
handling the request; it must report success or failure from that evidence
afterward. A tool result that only says work was accepted, queued, or started
does not count. A tool error does count as failure to start or update the work,
so the model must report that instead of continuing to say it is handling the
request. In backend mode the successful `create_agent_task` result is only an
acknowledgement, so outcomes of accepted work come from a task update or
`query_agent_task` result whose status is `completed` or `failed`. Cancellation
is separate: `cancelling` means the stop is still in progress, and only
`cancelled` confirms that the work stopped; that status is not a success or
failure of the original work. Unrelated requests must start their own task
instead of folding into existing work. Direct-tool mode
(`use_backend_agent = false`) applies the gate to runtime tools such as
`open_app` until they return.

Task updates and voice notifications are injected only when the session is
idle. Notification responses are private speech responses: they are played
through the speaker and acknowledged only after playback drains, but they are
not added to user-visible conversation history.

## Audio routes and the Web UI voice mode

A realtime session takes its audio from a `RealtimeAudioIO` endpoint
(`src/agent/internal/agent/realtime_voice.go`): a recording backend for the
microphone and a playback backend for the reply, in the formats negotiated with
the provider. The conversation, tools, transcripts, and history are the same
for every endpoint; only the audio path differs.

| Route | Started by | Microphone and speaker |
| --- | --- | --- |
| `device` | Wake word, MCU wakeup, task update, or a `/api/chat` message | Board audio through `audio_service` (or the local host backend) |
| `web` | The voice button in the Web UI | The browser that pressed it, over `/api/realtime/voice/ws` |

The Web UI shows the same voice mode for both routes: an orb above the
composer, a mute button, and an end button. Text typed in the composer during
the session is injected into the live session as before. Only the tab that
started a `web` session captures and plays audio; other tabs, and every tab
during a `device` session, only display it.

The Web UI uses these endpoints:

- `GET /api/realtime/voice` returns `available` (realtime input mode is
  active), `state` (`idle`, `connecting`, or `active`), `route`, and `muted`.
- `POST /api/realtime/voice/stop` ends the session on either route and returns
  the daemon to standby.
- `POST /api/realtime/voice/mute` with `{"muted": true}` replaces microphone
  input with silence for the rest of the session. The provider keeps receiving
  a continuous stream, so its turn detection sees silence instead of a stall.
- `GET /api/realtime/voice/ws` starts a `web` session. Binary frames carry
  signed 16-bit little-endian PCM in both directions. The agent sends JSON
  control messages: `capture_start` and `playback_start` carry the PCM format,
  `playback_stop` drops queued speech after a barge-in, and `ended` carries the
  reason the session ended. Closing the socket ends the session. Only one
  session runs at a time, so the endpoint rejects a start while another
  session is connecting or active.

Browser echo cancellation is not guaranteed to remove speech played through
Web Audio, so use headphones if the reply interrupts itself. A failed `web`
session reports its error in the Web UI and does not play the device failure
announcement.

### HTTPS for browser audio

Browsers expose the microphone only to a secure context, which means HTTPS or
`localhost`. The board's web services therefore listen on both HTTP and HTTPS:

| Service | HTTP | HTTPS |
| --- | --- | --- |
| Config Web | `http://192.168.42.1` (80) | `https://192.168.42.1` (443) |
| Agent Web UI | `http://192.168.42.1:8080` | `https://192.168.42.1:8443` |

The same ports apply on the board's Wi-Fi address. The HTTP ports stay for
compatibility. Each HTTPS port follows the same LAN access rules as its HTTP
port, and requests over the USB link are trusted on both. The Agent enables
HTTPS with `-https-addr`, and Config Web with `--https-port` and `--tls-dir`;
the shipped systemd units set them.

The board has no public DNS name, so it cannot use a publicly trusted
certificate. Instead the board keeps a local certificate authority in
`/userdata/agent/tls/` (`ca.pem` and the private `ca-key.pem`), shared by both
services. Whichever service starts first creates it. Each service issues a
certificate for the address a client connects to, such as `192.168.42.1`, a
Wi-Fi address, or `localhost`, and stores it under `certs/` so it stays the
same across restarts until shortly before it expires. The authority is
name-constrained to loopback, private, and link-local addresses and to
`localhost`, so trusting it cannot make a browser accept a certificate for a
public site. It is kept in user data so it survives firmware updates.

To use browser voice:

1. Open `https://192.168.42.1:8443` (or the board's Wi-Fi address on port
   8443). Pressing the voice button on a plain HTTP page opens this address
   automatically.
2. Accept the browser's certificate warning for this address. Installing the
   authority is not required: the page is still a secure context after the
   warning is accepted. Each address is a separate site, so USB and Wi-Fi
   addresses each need their own confirmation, and some browsers ask again
   after some time.
3. Optionally, install the authority as a trusted root to remove the warning
   on that device. Download it from `/tls/ca.pem` over the USB link, for
   example `http://192.168.42.1/tls/ca.pem`. The download is refused over
   Wi-Fi, even after signing in: installing a root is a trust decision, and a
   plain-HTTP response on the local network could have been replaced. The
   response carries the authority's SHA-256 fingerprint in the
   `X-Aico-CA-SHA256` header; compare it with the fingerprint your system
   shows before trusting the file. On macOS, open the file in Keychain Access
   and set it to Always Trust. Firefox keeps its own certificate store, so
   import it there as well.

Deleting `/userdata/agent/tls/` makes the board create a new authority on the
next service start; browsers that trusted the old one then show the warning
again.

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

---
sidebar_position: 18
---

# Streaming-First Agent Refactor: Research and Implementation Plan

Chinese translation: [中文版](streaming-first-refactor-plan.zh-CN.md).

**Status:** Scope fixed by product feedback; the compatibility-preserving Phase 1
foundation and playback queue hardening are implemented in this branch. Remaining
product policies and later migration phases are not yet approved.

**Research date:** 2026-10-29.

**Repository baseline:** `3aa92741d`, plus the worktree inspected on that date.

**Scope:** Streaming LLM, STT, TTS, realtime, tool/task progress, web text/events,
conversation state, and audio-service boundaries **inside the existing Go/C++
runtime**. All product voice input comes from the board microphone. Browser,
phone, and desktop microphones, browser/phone live audio, and a replacement or
hosted agent runtime are out of scope. Existing desktop playback remains an
output-side development/regression concern, not another microphone source. This
is not a rewrite of the BSP, MCU firmware, OTA system, or unrelated hardware services.

## 1. Recommendation

Refactor Aico into a **streaming-first conversation runtime**, keeping the
existing Go daemon, C++ audio service, provider adapters, tools, and memory plane.
Use **one shared lifecycle and delivery contract**, with two first-class engines:

1. **Cascaded voice:** streaming STT → LLM/tool loop → streaming TTS.
2. **Native realtime voice:** a provider's bidirectional speech-to-speech session.

Text chat uses the same LLM/tool engine without audio. The engines share session
ownership, event identity, cancellation, presentation, history projection, and
observability; they do not pretend to have identical provider protocols.

The project does **not** need streaming added from scratch. It already streams
LLM text, TTS audio, realtime events, and board playback. The main gaps are
inconsistent completion/cancellation semantics, final-only STT output,
and weak board-audio delivery/backpressure contracts. Desktop playback is also
buffered; its streaming work concerns development/regression output only.

Borrow design lessons from LiveKit's session/turn model, Pipecat's
interruptible frames, and native realtime APIs' response/item identities.
**Do not replace the existing runtime at any stage of this plan or outsource its
voice orchestration.** The Go/C++ streaming core is the only implementation
option; external frameworks and products are research references, not candidates
for adoption.

### What success means

- Users see text and interim transcripts as they become available.
- On streaming-capable configurations, users hear audio before generation ends.
- Interrupting stops local playback promptly, cancels eligible upstream work,
  and cannot leak old audio into the next response.
- Transcript hypotheses, generated content, completed model responses, and
  presented/played content have different, explicit meanings.
- Slow clients, slow speakers, provider failures, and reconnects have bounded
  memory/latency behavior instead of silent loss or unbounded buffering.
- Batch-only providers remain usable, but are visibly labeled degraded; wrapping
  a completed result in a stream does not make it a low-latency stream.
- Existing tool safety, foreground/background isolation, context compaction,
  provider-specific message metadata, and configuration compatibility survive.

### Recommended first delivery

Land a **vertical streaming slice**, not a repository-wide interface rename:
board-microphone voice / web text → LLM deltas → spoken-text projection → TTS
PCM → board playback, with interruption and delivery-aware finalization.
Exercise the existing desktop playback backend as a development output sink,
not a capture source. Add incremental STT from the board microphone and migrate
native realtime onto the same lifecycle in subsequent slices.

## 2. Evidence and limitations

This proposal combines static code inspection with current official product/API
and framework documentation. The sources in section 15 are representative
established systems, **not a measured market-share ranking**. Commercial-product
documentation describes exposed behavior, not necessarily internal architecture.

No provider latency benchmark, hardware acoustic test, or live-provider
compatibility test was performed. Numeric budgets below are **proposed acceptance
targets**, not measured performance or vendor guarantees. Living documentation
can change; implementation must pin each adapter's model/API version and retain
protocol fixtures. Do not change model defaults just because a research page
shows a newer model.

## 3. Current architecture and concrete gaps

### 3.1 Current paths

```text
Classic voice
  device PCM capture ──> optional streaming STT upload
                  └──> local VAD / complete utterance fallback
  final transcript ──> Runtime.Run / AgentLoop
  LLM callbacks ──> leading <tts> parser ──> TTS adapter ──> playback
  complete LLM response ──> tool parsing / history / next iteration

Native realtime
  device PCM <──> realtimevoice managed provider session
  normalized events ──> daemon session loop
                    ├──> playback
                    ├──> chat deltas / completed conversation history
                    └──> foreground tools ──> independent background tasks

Web
  POST /api/chat ──> NDJSON text/progress events
  GET /api/events ──> SSE observer events
  audio attachments / archived audio ≠ live bidirectional browser audio
```

This is an inventory of current behavior, not the target scope. Existing audio
attachments and local recording do not authorize alternative voice-input sources
in the refactor; the target source is the board microphone only.

### 3.2 Audit findings

| Component | Observed implementation | Consequence and migration target |
| --- | --- | --- |
| LLM | [Runtime streaming wiring](../../src/agent/internal/agent/runtime.go#L1156) supplies `llms` callbacks when a writer is present. The [OpenAI-compatible decoder](../../src/agent/internal/agent/openai_compatible_model.go#L739) also assembles the final response. | Keep final aggregation for history and tools; replace text-only callback coupling with typed response events. Aggregation itself is not a streaming defect. |
| Agent loop | [Generation and parsing](../../src/agent/internal/agent/agent_loop.go#L256) await a complete `ContentResponse` before deciding tool execution. | Expose progress early, but never execute incomplete tool arguments. Preserve existing validation and policy boundaries. |
| STT | [`STTStreamUploader`](../../src/agent/internal/agent/stt.go#L23) accepts PCM but `Finalize()` returns one string. | Streaming upload is not streamed transcription. Add revisioned interim and segment-final results separately from end-of-turn. |
| STT startup | [Asynchronous setup](../../src/agent/internal/agent/audio_dialog.go#L361) and [chunk upload](../../src/agent/internal/agent/audio_dialog.go#L496) do not upload early chunks while the uploader is absent. | A ready stream can miss initial speech without failing. Add bounded pre-ready capture and an explicit readiness barrier. |
| Batch STT | [`TranscribeWAV`](../../src/agent/internal/agent/stt.go#L66) lacks a context parameter and uses a request not bound to the turn context. | Context cancellation must reach HTTP requests and fallback transcription, not only the streaming path. |
| TTS | [`TTSProvider`, `StreamSession`, and `AudioSink`](../../src/agent/internal/agent/tts/provider.go#L15) already support incremental text/audio and sink stop/drain. | Evolve these abstractions rather than creating another TTS subsystem. Separate input finish, synthesis completion, playback drain, and resource close. |
| TTS compatibility | [Speech projection](../../src/agent/internal/agent/speech/text.go#L198) parses the leading `<tts>` block; [OpenRouter TTS](../../src/agent/internal/agent/tts/adapters/openrouter/session.go#L195) reads the complete PCM response. | Preserve speech/display separation. Report actual streaming granularity instead of advertising every adapter as equivalent. |
| Realtime | [`realtimevoice.Session` and events](../../src/agent/internal/agent/realtimevoice/interface.go#L199) already normalize provider behavior and expose optional capabilities. | Reuse the registry, native adapters, managed resampling, and capability detection. Extract shared lifecycle policy from daemon orchestration, not provider wire logic. |
| Realtime orchestration | [Daemon event loop](../../src/agent/cmd/daemon/realtime_wakeup.go#L1444) owns admission, history, playback, tools, notifications, and chat bridging. | Move policy incrementally into testable controllers; preserve response-ID filtering, cancellation acknowledgments, watchdogs, and provider-specific ordering rules. |
| Board playback | [`AudioPlaybackSession`](../../src/audio_playback_session.cpp#L66) plays chunks before finalization; a full queue waits 200 ms and then pushes without rechecking the cap. | Preserve actual streaming; enforce a hard duration/byte bound with explicit saturation behavior. |
| Capture queue | [Record queue insertion](../../src/audio_record_session.cpp#L241) silently skips new chunks when the 512-chunk cap is reached. | Add loss accounting, sequence/timing metadata, and an explicit discontinuity policy. A chunk-count cap is not a latency budget. |
| Existing desktop playback | [`local_audio_playback.go`](../../src/agent/internal/agent/local_audio_playback.go#L64) spools a WAV and starts the OS player only on finalization. | Evolve this development/regression output sink toward live PCM; preserve a labeled buffered fallback. This does not authorize desktop microphone input. |
| Web delivery | [Chat fetch](../../src/agent/internal/agent/web_ui/scripts/chat.js#L33) uses NDJSON. [`EventBroadcaster`](../../src/agent/internal/agent/event_broadcaster.go#L21) is intentionally best-effort and drops events for full subscriber channels. | Keep NDJSON/SSE compatibility. Add identity, resynchronization, and loss detection; do not use the observer broadcaster as the authoritative media/control path. |
| Phone bridge | [Bridge contract](phone-bridge.md#communication-method) is commands/results, not a microphone/speaker media protocol. | Keep it command/result-only; phone microphone capture and media transport are excluded, not deferred features. |
| Background work | [Foreground/background separation](foreground-background-agents.md#foreground-and-background-agents) isolates live conversation from queued device tasks. | Preserve task ownership and cancellation scope; canceling speech must not silently cancel an independent task. |
| Telemetry | [Episode exporter](telemetry-langfuse.md#data-flow) already uses asynchronous OTLP export. | Extend existing spans with stream milestones and delivery outcomes; do not introduce a second telemetry stack or per-audio-frame traces. |

The current [voice documentation](audio-features.md#tts-provider-usage) already
states that TTS accepts LLM fragments. A plan based on “everything is blocking”
would discard working behavior and miss the actual gaps.

## 4. Market and framework research

### 4.1 Comparison and lessons

| System / category | Documented pattern | Pattern to apply within Aico | Do not assume or copy blindly |
| --- | --- | --- | --- |
| **LiveKit Agents — voice framework** [S1] | A session orchestrates media, pipeline or realtime models, turns, interruption, and events. Turn detection can be model-, STT-, VAD-, realtime-, or manually driven. | A session controller with explicit input/turn/output ownership; configurable endpointing and interruption as different policies. | An SFU, Python/Node worker migration, and cloud deployment are not prerequisites for one on-device speaker. |
| **Pipecat — voice pipeline framework** [S2] | Typed frames; urgent interruption frames bypass ordinary work, cancel eligible generation/tools, clear TTS, and flush unplayed output. Its context aggregation tracks spoken output. | Separate urgent control from queued media; interrupt the entire output path; distinguish generated and played text. | A universal processor graph is not necessary for every Aico operation. Exact played-word tracking requires actual alignment and playback evidence. |
| **OpenAI Realtime / Agents SDK — native speech model and SDK** [S3] | Stateful response/item events, streamed audio/text, VAD or semantic VAD; browser WebRTC and server/custom-audio WebSocket paths. | Response/item correlation; provider-ready gating; cancel/truncate support and a provider capability contract. | OpenAI-shaped JSON is not a universal protocol; WebSocket clients still own their playback queues and progress accounting. |
| **Gemini Live — native speech model** [S4] | Bidirectional WebSocket with distinct generation-complete, turn-complete, interrupted, tool, and transcription signals. | Separate completion milestones; process all parts of each provider message; preserve native cancellation and context behavior. | Provider-maintained history of transmitted content is not proof of audio actually played on the device. |
| **Azure Voice Live — managed realtime service** [S5] | Versioned session/event APIs, turn-detection configuration, streamed function arguments and output. | Pin protocol versions and negotiate features explicitly. | Features in a preview API must not be inferred for another version or every model/region. |
| **Vapi — hosted voice-agent platform** [S6] | Session/call event envelopes, correlated tools and model output; WebSocket media is distinct from server webhook events. | Clear control/media separation and per-turn event identity. | Webhooks are not a low-latency media path; exposed APIs do not establish internal backpressure guarantees. |
| **Retell AI — hosted voice-agent platform** [S7] | Custom-LLM WebSocket supplies live transcript and `response_id`; newer requests invalidate earlier responses. | Generation invalidation and cancellation of stale model work; measure time to first useful speech segment, not just first token. | Custom-LLM integration still leaves response latency, uptime, and reconnection with the application. |
| **ElevenLabs Agents / Speech Engine — hosted voice stack** [S8] | Typed transcript/response/audio/interruption events; Speech Engine supports an application-owned streaming LLM and cancellation as newer transcript events arrive. | Maintain separate generated, corrected, and delivered response states; make playback queues explicit. | Streamed text does not remove the need for client playback buffering or capability checks. |
| **LangChain / LangGraph — general agent orchestration** [S9] | Separate projections for message text, tool arguments/execution, state, subagents, custom progress, and final outputs. | Typed projections over one run rather than several unrelated callback paths. | A workflow/token stream is not an acoustic turn detector or a media scheduler. No Python rewrite is proposed. |
| **Dify — general agent product** [S10] | SSE agent deltas, tool/progress events, a complete final message, then a terminal marker. Speech endpoints are separate. | Final snapshots reconcile deltas; distinguish task progress from final response. | A streaming chat API plus file STT/TTS is not full-duplex voice. |
| **Deepgram — streaming speech component** [S11] | Interim hypotheses can change; `is_final` finalizes a segment, while `speech_final` identifies an utterance boundary. | Separate transcript revision, segment finality, and turn completion. | Do not launch tools for every interim transcript or equate any final segment with a completed user turn. |
| **Alibaba/Qwen — relevant regional speech provider** [S12] | Realtime STT has readiness/task boundaries; TTS supports incremental text, flush, and finalization; realtime voice has native turn modes and events. | Readiness buffering, explicit flush/finish, native adapters, and Chinese-language/region validation. | Qwen ASR, Qwen TTS model families, and Qwen realtime voice do not share one interchangeable protocol. |

### 4.2 Shared architectural conclusions

1. **Streaming is a lifecycle, not a transport switch.** SSE/NDJSON/WebSocket
   only move bytes; they do not define correctness at interruption or completion.
2. **Conversation and execution are distinct.** A responsive foreground voice
   session must not wait synchronously for long device operations.
3. **STT, LLM, and TTS are asymmetric.** STT revises hypotheses; LLM emits text
   and structured tool data; TTS consumes committed speech units and emits audio.
4. **Generation completion is not playback completion.** A response can finish
   generating while seconds of unplayed audio remain.
5. **Provider capability differences are part of the contract.** Normalize
   concepts, not every wire operation or unsupported feature.
6. **Cancellation and backpressure belong in the architecture from day one.**
   An interface named `Stream` is insufficient evidence of bounded latency.

### 4.3 Chosen implementation boundary

**Only the streaming core within the existing Go/C++ system is in scope.** It
preserves deployed device services, Agent tools/memory, and provider adapters,
while Aico takes responsibility for lifecycle and protocol tests. Extra
callbacks are transitional shims, not a permanent parallel architecture.
LiveKit, Pipecat, Vapi, Retell, and ElevenLabs remain sources of design evidence
in section 4.1, not alternative runtimes or hosted voice channels in this plan.
No WebRTC gateway, browser/phone media transport, or desktop microphone is
planned.

## 5. Target architecture and ownership

```text
Inputs
  Board microphone PCM     Web text (no live media)
            │                        │
            └────────────┬───────────┘
                           │
                 Conversation controller
             admission, turn policy, output epoch,
              interruption, reconnect, config snapshot
                           │
             ┌─────────────┴─────────────┐
             │                           │
       Cascaded engine             Native realtime engine
       STT event session           existing realtimevoice adapters
       LLM/tool run                native audio / text / tools
       spoken-text projection
       TTS session
             │                           │
             └─────────────┬─────────────┘
                           │
             Typed semantic events + audio frames
                           │
              Presentation / playback coordinator
                           │
              Board speaker / desktop development sink
                           │
                  Playback progress receipts

Read-only projections: web events, history, task progress, episode telemetry
Independent execution: agenttask queue + existing backend Runtime and tools
```

### Ownership rules

- One controller owns admission and lifecycle transitions for a conversation.
  Devices with one microphone/speaker have one foreground media owner; this
  proposal does not imply unlimited simultaneous conversations.
- The board microphone is the sole product voice-input producer, with
  explicitly bounded consumers. The VAD branch uses 16 kHz, 512-sample frames;
  STT/realtime branches use negotiated formats. Web text never becomes PCM.
- The run owns model attempts and tool invocations. The presentation coordinator
  owns speaker ordering, priority, stop, drain, and delivery receipts.
- User replies outrank queued notifications. Task updates and notifications
  retain the existing idle-admission and privacy rules; they are not mixed into
  every assistant text stream.
- Providers own authentication, transport framing, native event translation,
  and protocol-specific capabilities. They do not directly write web history.
- The history projection persists committed conversation records. A separate
  delivery record says what was shown/played, interrupted, or unknown.
- Telemetry and observer clients cannot block microphone capture or playback.

### Package boundaries, without an immediate directory reshuffle

Introduce a small leaf package, tentatively `internal/stream`, for value types,
IDs, terminal/error conventions, audio-frame metadata, and bounded queue helpers.
It must not import `agent`, a provider SDK, or HTTP handlers.

Extract controllers incrementally into a package such as `internal/voice` that
depends on narrow interfaces and the leaf types, not on `agent.Runtime` concrete
internals. The daemon remains the composition root and supplies adapters for
runtime, history, tools, playback, and `agenttask`. Existing `tts`, `speech`, and
`realtimevoice` packages stay where they are until moving them has a concrete
benefit. Avoid introducing an import cycle while extracting the daemon loop.

This is **not** a generic event-bus framework, distributed event store, Kafka
pipeline, or arbitrary graph executor. Use direct typed connections on the hot
path and subscriptions only for projections.

## 6. Shared streaming contract

### 6.1 Identity and ordering

A semantic event has a versioned envelope and exactly one typed payload:

| Field | Meaning |
| --- | --- |
| `schema_version` | External event schema; proposed initial new version `2` |
| `event_id`, `sequence` | Stable event identity and controller-assigned sequence within a conversation epoch; replay cursors include both epoch and sequence |
| `conversation_id`, `session_epoch` | Durable conversation and current live connection generation; reconnect advances the epoch |
| `turn_id`, `run_id` | Accepted user turn and execution; a run may contain several model responses/tools |
| `response_id`, `attempt_id`, `item_id` | One model response, one attempt, and a content item; never reuse attempt identity on retry |
| `stream_id`, `output_epoch` | Output track and its invalidation generation; input capture is not invalidated when output stops |
| `call_id`, `task_id`, `parent_id` | Optional tool/background-work correlation |
| `occurred_at`, `received_at` | Provider timestamp when available and local receipt time; local monotonic clocks are used for durations |
| `origin`, `visibility` | Provider/module source and allowed projections: user display, speech, internal, or diagnostic |

Not every event needs every optional ID. Enforce required fields by event kind.
Provider IDs remain available as metadata; adapters assign local identities for
providers that omit them. Do not infer a global provider ordering from arrival
time or fabricate missing speech-boundary events.

Audio is a separate binary/data path with the same correlation IDs, `frame_seq`,
format, sample count, and presentation position in **samples per channel**.
Do not base64-encode internal PCM or emit every PCM frame as a history event.
Cross-machine clocks are not directly subtracted for latency measurements.

For web resynchronization, take an active-turn snapshot and its event watermark
atomically, then deliver events after that watermark. Projection-specific cursors
must account for filtering: an omitted internal event is not a lost public event.
A process restart invalidates the ephemeral replay window and requires a new
snapshot; it must not reuse an old epoch/cursor pair.

### 6.2 Event families

| Family | Representative events | Important semantics |
| --- | --- | --- |
| Session | `session.ready`, `session.reconnecting`, `session.closed` | Ready means negotiated configuration was accepted, not merely socket connected |
| Input | `input.speech_started`, `input.speech_stopped`, `transcript.updated`, `transcript.segment_final`, `turn.committed` | Interim text can be revised; segment finality and user turn completion differ |
| Model | `response.started`, `text.delta`, `text.final`, `tool.arguments_delta`, `tool.call_ready`, `response.terminal` | A final snapshot replaces/reconciles its deltas, never appends the full text again |
| Speech | `speech.text_committed`, `audio.started`, `audio.ended` | Only speech-approved text enters TTS; audio end is production end, not playback drain |
| Presentation | `playback.started`, `playback.progress`, `playback.drained`, `playback.stopped` | Receipts include epoch, item, played position, and confidence/measurement source |
| Work | `tool.started`, `tool.progress`, `tool.completed`, `tool.failed`, `task.updated` | Progress is optional; final results remain correlated and bounded |
| Control | `cancel.requested`, `cancel.acknowledged`, `stream.discontinuity`, `stream.error`, `usage.updated` | Cancellation can bypass queued data; errors distinguish recoverable and terminal scopes |
| Run | `run.terminal` | Execution completion is distinct from presentation completion |

Keep provider-exposed reasoning and signed/opaque metadata separate from visible
text and speech. Preserve the current replay requirements and permitted
reasoning-display settings, but do not make raw reasoning a mandatory public
stream or a telemetry requirement.

### 6.3 Lifecycle invariants

1. Each response attempt, synthesis, tool invocation, and run has one internally
   recorded terminal outcome: `completed`, `cancelled`, or `failed`.
2. Channel closure / EOF without a terminal outcome is a failure, not success.
3. No late output from an invalidated epoch reaches a user or a new response.
   Late usage can update accounting only; it cannot revive output or a watchdog.
4. `FinishInput` is a half-close: pending output may still arrive.
5. `Flush` commits currently buffered speech text without ending input.
6. `Cancel` requests prompt termination; it is not proof of remote acknowledgment
   or playback stop. Both outcomes are observed separately.
7. `Close` is idempotent resource release, not an overloaded finish/drain call.
   If work is still active, it initiates cancellation and uses a bounded shutdown
   deadline; it cannot wait forever for a consumer to drain an abandoned channel.
8. A normal end barrier includes the last relevant sequence/frame position and
   cannot overtake preceding data. Urgent cancel/error signals may bypass data
   because they explicitly invalidate it.
9. Every blocking send/read/write has a context or deadline. One owner closes
   each channel; producers cannot send after ownership is released.
10. Exactly-once state transitions are a local invariant, **not a claim of
    exactly-once network delivery**. Replayed events may repeat; consumers dedupe.

### 6.4 Interface direction

The following is a design sketch, not a proposed copy-paste patch. Types such as
`ModelRequest`, `TranscriptEvent`, and `AudioFrame` remain modality-specific.

```go
// Runtime output is streamed while this call runs; final state remains available.
type Runner interface {
    RunStream(context.Context, TurnInput, EventSink) (RunResult, error)
}

type EventSink interface {
    Emit(context.Context, Event) error
}

type RecognizerSession interface {
    WriteAudio(context.Context, AudioFrame) error
    FinishInput(context.Context) error
    Events() <-chan TranscriptEvent
    Cancel(context.Context) error
    Close() error
}

type SynthesisSession interface {
    WriteText(context.Context, SpeechText) error
    Flush(context.Context) error
    FinishInput(context.Context) error
    Events() <-chan SynthesisEvent
    Cancel(context.Context) error
    Close() error
}

type Playback interface {
    WriteAudio(context.Context, AudioFrame) error
    FinishInput(context.Context) error
    Progress() <-chan PlaybackEvent
    Stop(context.Context, StopReason) error
    Close() error
}
```

Keep native realtime's bidirectional session and optional operations; do not
force it through `RecognizerSession` plus `SynthesisSession`. Bridge its events
into the shared contract. TTS audio can continue using a typed sink instead of
an events channel where that is simpler; lifecycle and ownership semantics must
match either representation.

Blocking APIs remain collectors over the new stream for compatible callers.
Retain `Runtime.Run`, `TranscribeWAV`, and old HTTP response forms during
migration. Batch-only implementations use bounded adapters and declare
`output_granularity=final_only`; do not emit fake incremental tokens.

## 7. Module-by-module changes

### 7.1 LLM and the agent/tool loop

- Introduce typed model events for text, exposed reasoning, tool-argument deltas,
  tool-call readiness, usage, errors, and terminal state. Preserve final response
  aggregation, provider-specific context, cache metadata, and signed reasoning.
- Make all foreground model invocations stream-native internally. Background
  compaction, memory extraction, and other noninteractive calls can collect the
  stream; their events remain internal and must not speak or enter chat history.
- Scope output to a response/item rather than one mutable global text buffer.
  Preserve reset/abort/finish boundaries between tool-loop iterations and steers.
- Assemble arguments by call ID and index. Only complete, valid, schema-checked,
  policy-approved calls become executable. Initially preserve the current
  response-terminal execution boundary; earlier execution after a complete call
  is a later optimization, with explicit side-effect tests.
- Emit tool lifecycle and bounded progress where supported. An atomic HID action
  can honestly emit only start/result; “all modules stream” does not require
  fabricated progress or a new streaming implementation of every tool.
- Preserve existing cancellation/preemption notices, run budgets, context
  overflow recovery, and foreground/background task semantics.

**Retry rule:** automatic retry is safe before externally visible output and
before side effects, subject to the existing retry policy. After text is shown,
mark/reconcile a failed attempt before replacement. After audio has played,
never transparently replay the answer from the beginning. Resume only with
verified provider support or an explicit new response; otherwise report a
partial failure. Unknown side-effect outcomes are never automatically retried.

### 7.2 STT and input turn detection

Add independent capability fields for audio streaming input, interim results,
segment finals, provider endpointing, word timing, and cancel/resume support.
Do not infer transcript streaming from `SupportsStreamingUpload`.

Use a revisioned transcript segment:

```text
segment_id, revision, text_snapshot, is_final,
audio_start_sample, audio_end_sample,
optional stable_prefix / word timings / language / confidence
```

A newer snapshot replaces that segment's earlier hypothesis. An empty snapshot
can retract a hypothesis. Only committed finals enter durable user history;
final segments are assembled into a turn by one configured turn authority.

- Start connection preparation early, but release audio only after provider
  readiness. Keep a bounded pre-ready queue including the initial speech prefix.
- If readiness exceeds its budget, explicitly switch to bounded batch fallback
  or fail the input turn. Never silently skip the prefix then accept a partial
  successful transcript. Replay queued audio only at a rate the provider allows.
- Keep local VAD for activity and classic endpointing; use provider or semantic
  endpointing only when selected. Native realtime remains provider-authoritative
  where required. Do not run competing turn-committers against the same input.
- Stream interim captions to the UI. Default: start the authoritative LLM/tool
  run only after turn commit. Optional speculative generation can be explored
  later, with no tool side effects, no history commit, and no audible output
  before acceptance; measure wasted tokens and false endpoints.
- Add context-aware batch transcription. Bound utterance duration, bytes, setup
  time, finalization time, and fallback attempts. Define behavior for no speech,
  uncertain transcription, noise-only interruptions, and partial network loss.

### 7.3 Spoken-text projection and TTS

Preserve the existing **spoken text versus displayed text** distinction. During
migration, the leading `<tts>...</tts>` parser emits `speech.text_committed`
segments; it is an adapter for the current model-output format, not a protocol
that every downstream component parses independently.

Do not change prompts and streaming infrastructure simultaneously. A later
prompt-format change needs its own multilingual quality evaluation. Whether
speech is eventually structured content or explicitly tagged text is independent
of the stream contract.

- Normalize chunk boundaries without splitting UTF-8 sequences. Preserve current
  behavior for split tags, missing closing tags, tool-response boundaries,
  extra tags, and duplicate-speech suppression until deliberately changed.
- Keep a shared configurable sentence/clause segmenter for adapters that require
  text units. Handle Chinese punctuation, abbreviations, numbers, URLs, Markdown,
  code blocks, and unclosed structured content. Never speak tool JSON or internal
  reasoning. A maximum wait may flush only a safe speech unit, not arbitrary
  partial markup or a number in the middle of being generated.
- Separate native incremental-text TTS, sentence-input/audio-stream TTS, and
  batch synthesis. Leave provider-specific buffering and prosody controls inside
  adapters, but expose their effective granularity and first-audio milestones.
- Split current close behavior into finish-input, synthesis-end, playback-drain,
  and close. Do not block a provider read loop on a slow speaker indefinitely.
- Retain provider-holder lifetime rules: a configuration change affects new
  sessions; an existing session finishes or is explicitly canceled, never
  silently changes voice/model halfway through a sentence.
- Preserve notification tails, local fallback clips, and tool-call speech rules.
  Private notification speech is a separate presentation item. A failure after
  partial speech must not replay the whole response as a fallback.

### 7.4 Realtime voice

Reuse the existing provider registry, native translators, media negotiation,
managed resampling, and optional interfaces. The refactor is primarily extracting
shared controller/presentation responsibilities, not replacing working adapters.

- Map native response/item/tool IDs to shared identities and preserve native
  information required for replay, usage, interruption, and truncation.
- Keep generation-end, provider turn-end, input transcript finality, and local
  playback drain distinct. Some native events are missing or reordered.
- Maintain cancellation acknowledgment and no-progress watchdog behavior;
  background microphone traffic must not keep a stuck response alive.
- Interrupt/truncate using confirmed or explicitly estimated playback position,
  never simply bytes submitted to the sink. If provider context cannot be made
  consistent after interruption, reconnect/replay conservatively.
- Do not resubmit realtime input transcripts as a second prompt. They are a
  user-facing projection of audio the provider already consumed.
- Retain provider-specific tool continuation. Execute only complete validated
  calls, correlate parallel results, and handle provider-canceled call IDs.
- On reconnect/session rotation, retain the conversation ID, advance connection
  epoch, restore committed context, and do not replay stale audio or tool actions.
  Session resume is used only when the provider advertises and validates it.

### 7.5 Board capture, audio formats, and playback

Keep the existing binary-payload UDS protocol rather than sending PCM through
JSON. Add backward-compatible fields/capability negotiation for frame sequence,
capture timestamp, queued duration, dropped frames, played position, and explicit
stop/drain acknowledgment. Old clients and services must continue to interoperate.

PCM16 little-endian mono is the initial internal baseline. Do **not** force every
provider to 16 kHz: preserve negotiated provider/device rates and resample only
at boundaries. Keep stateful resamplers across frames; reset/flush deliberately
at epoch changes and handle odd-byte fragments. Add codec/channels/sample-rate
metadata to every stream descriptor, not implicit assumptions in callers.

- Enforce playback bounds by bytes **and audio duration**, rechecking after every
  wait. Saturation must produce bounded waiting or an explicit error, not an
  extra unbounded push.
- Capture cannot wait indefinitely for a remote consumer. Prefer bounded
  freshness-preserving drop-oldest behavior with a discontinuity counter, then
  reset/fail affected recognition if the gap invalidates a turn. Never present
  corrupted input as a reliable complete transcript.
- Distinguish data accepted by the service, submitted to hardware, and estimated
  or confirmed rendered samples. Current hardware-tail grace is not a precise
  playback clock. Advertise confidence; validate with loopback/acoustic tests.
- Evolve desktop whole-WAV playback into a live PCM sink for development and
  output regression tests, with explicit stop/drain and a labeled buffered
  fallback when needed. It is an output concern only: do not add desktop
  microphone capture or describe file-only fallback as realtime.
- Preserve the Rockchip VQE/AEC pipeline. Classic full-duplex barge-in is opt-in
  until simultaneous capture/playback passes on-board echo and false-trigger
  tests. A hardware button remains a reliable interruption path.

### 7.6 Web text, phone commands, and background consumers

**Text API:** retain current NDJSON `/api/chat` and SSE `/api/events`. Add an
explicit v2 negotiation option; both transports serialize the same semantic
projection rather than inventing different event types. Keep existing nonstream
clients working through collectors. Final snapshots are replacements, not deltas.

Define request ownership: an owning chat connection can cancel its request on
disconnect according to the existing product behavior; an observer SSE disconnect
must never cancel a run. Explicit cancel targets a request/run/response ID, not
“whatever is currently active.” Slow observers receive `resync_required` or are
disconnected and reload history plus the active-turn snapshot. Use a bounded
replay window with cursor/epoch; after restart or cursor expiry, request a
snapshot instead of promising infinite event replay.

**Voice-input boundary:** the board microphone exposed by `audio_service` is the
only product voice-input source. Browser microphone capture, browser live audio,
phone microphone/media transport, and desktop microphone capture are permanently
out of scope for this plan. The Web UI remains a text/event client only; Phone
Bridge remains command/result-only. Do not add a future media-session extension
for those clients under this architecture.

**Background consumers:** tasks, MCP/HTTP tool callers, schedulers, memory work,
and benchmarks consume relevant run/progress projections. They need not consume
PCM. Keep existing task IDs, steers, context lineage, and cancellation ownership.
Tool progress should not automatically become speech; the foreground decides
when and how to summarize it.

## 8. Interruption, state, and history correctness

### 8.1 State is not one linear enum

A conversation can be listening while speaking. Track orthogonal states:

| State domain | States |
| --- | --- |
| Connection | opening → ready → reconnecting / closing → closed |
| Input turn | idle → speech / hypothesis → committed or discarded |
| Generation | idle → running → cancelling → terminal |
| Playback | idle → buffering / playing → draining → drained, or stopped |
| Background task | Existing independent `agenttask` lifecycle |

A response may be terminal while playback is still running. A new utterance may
be captured during playback. The controller decides admission; these facts must
not be collapsed into one ambiguous `busy` flag.

### 8.2 Barge-in transaction

1. Local/provider turn policy accepts an interruption (not every energy spike).
2. Atomically invalidate the current output epoch so late packets become stale.
3. Stop/flush local playback immediately; do not wait for the provider's network
   round trip. Record played position and whether it is measured or estimated.
4. Cancel LLM/TTS/eligible foreground tools and send the native response cancel
   or truncate request where supported. Background tasks continue unless the
   user explicitly cancels or updates their task.
5. Record separate local-stop and remote-terminal outcomes. Retain admission
   ownership while the provider's required cancellation acknowledgment is pending.
6. Commit the delivery-aware partial record and interruption reason; admit the
   next response only when safe. Missing acknowledgment times out into reconnect.
7. For false interruption/no transcript, follow an explicit product policy:
   resume only if supported safely, otherwise prompt for repetition or remain
   listening. Do not silently restart speech from the beginning.

### 8.3 History and delivery projections

Keep the current user/backend context split. Add delivery metadata rather than
turning every transient event into a durable conversation message:

- Final user transcripts are durable input. Interim hypotheses are ephemeral.
- Final generated assistant content is the canonical response for text/history,
  with terminal status and attempt identity. Failed partial output is marked,
  not passed off as a completed answer.
- Voice replay context uses delivered speech/segments when reliable alignment
  exists. Keep generated text separately for diagnostics and display, clearly
  distinguished from what was spoken.
- With word timing and a playback clock, truncate to the confirmed played range.
  Without them, commit only confirmed complete segments plus an interrupted /
  delivery-unknown marker. Do not estimate spoken words by byte ratio and claim
  accuracy. A rendered-audio receipt is still not proof that a human heard it.
- Preserve tool call/result pairs and action outcomes even if speech is canceled.
  An interrupted acknowledgment does not undo a device action.
- Notification speech remains private and acknowledged according to actual
  playback policy. It must not leak into user conversation replay.
- On crash, retain existing interrupted-run recovery and add incomplete delivery
  status. Do not replay audio or side effects automatically during recovery.

## 9. Backpressure, errors, and recovery

### 9.1 Queue classes and provisional budgets

These are starting values for experiments, not new configuration defaults to
apply without measurement. At PCM16 mono, 16 kHz costs 32 KB/s, 24 kHz 48 KB/s,
and 48 kHz 96 KB/s before framing/copies.

| Queue / operation | Initial experimental budget | Saturation policy |
| --- | --- | --- |
| Capture → STT/realtime | 200–500 ms steady-state queue; up to 2 s pre-ready buffer | Emit discontinuity and reset/fail/fallback explicitly; never silently lose the speech prefix |
| Batch STT fallback | At most 30 s per utterance initially, configurable by product needs | End/reject overlong input with a clear status; no unlimited recording in memory |
| Speech text waiting for TTS | Bound bytes and estimated speech duration, initially 2 s worth of queued speech | Propagate bounded backpressure; if the provider cannot pause, fail the response explicitly rather than accumulating indefinitely |
| Output PCM | 60–120 ms initial playout buffer; target <300 ms queued, hard cap 1 s | Stop accepting, propagate pressure, then cancel on deadline; no silent holes in an active spoken sentence |
| Observer events | Bounded by count/bytes, with active-turn snapshot and bounded replay | Coalesce replaceable snapshots, or disconnect/resync; never lose a terminal event silently |
| Control | Small reserved mailbox plus cancellation signal | Cancel/stop is independent of media queue fullness; normal end remains an ordered barrier |
| Overall session buffers | Initial target ≤8 MiB additional application buffering | Reject excess with explicit error; measure actual allocations/copies on target hardware |

A provider may generate audio faster than real time and may not support pausing.
TCP flow control alone is not an application-level guarantee. The adapter must
report inability to keep up before hidden socket/SDK buffers destroy latency.
Native realtime providers may require a tuned budget different from cascaded
TTS; keep the same policy categories and measured bounds.

### 9.2 Error taxonomy and retry rules

Use a scoped error with `code`, `stage`, `retryable`, `response/attempt_id`,
`partial_output`, and `side_effect_state`; sanitize provider payloads.

| Failure | Required behavior |
| --- | --- |
| Setup/auth/config | Fail before capture/output, or explicitly choose an approved fallback; invalid credentials are not endlessly retried |
| STT setup deadline | Bounded buffered/batch fallback or input failure; no prefix loss masquerading as successful streaming |
| Provider transient failure before output | Limited exponential backoff with jitter and shared deadline; no nested retry multiplication |
| Failure after partial text/audio | Mark partial outcome; do not transparently concatenate/replay another attempt |
| Downstream saturation | Respect deadline; cancel relevant output; surface `backpressure_timeout` and queue metrics |
| Tool timeout/cancel after side effect | Record success/failure/unknown independently of speech; require idempotency support or reconciliation before retry |
| Provider event after cancellation | Drop from presentation; preserve only permitted accounting/diagnostics |
| Reconnect/session expiry | Advance epoch; restore committed context; resume only with verified provider support |
| Telemetry unavailable | Continue conversation; bounded best-effort export, no audio-thread blocking |

## 10. Configuration, capabilities, and compatibility

Continue using the current named provider records under `model_settings` and
`voice_settings`. Do not introduce a parallel provider registry or rewrite user
TOML as a prerequisite. Extend the existing provider metadata used by Config Web.

Capability resolution is **provider + model + API version + mode**, finalized
when a session is ready. Distinguish static known support from negotiated support.
Expose effective capabilities and degraded reasons to operators and tests:

| Area | Example capability dimensions |
| --- | --- |
| LLM | Text deltas, tool-argument deltas, usage timing, cancel, resumability |
| STT | Live audio input, interim snapshots, segment finals, endpointing, word timing |
| TTS | Incremental text, sentence input, incremental PCM, alignment, flush, cancellation |
| Realtime | Turn authority, interruption authority, cancel/truncate, tool continuation, context replay/resume |
| Playback | Live writes, stop, drain, queue duration, played-position accuracy |

Illustrative policy additions: `streaming_policy = "prefer" | "required" |
"disabled"`, bounded queue/deadline settings, turn-detection authority, and
full-duplex/barge-in mode. Names and placement require a configuration ADR before
implementation; this document does not claim those keys already exist.

- `prefer`: use real streaming where supported; visibly disclose batch fallback.
- `required`: reject a configuration that cannot satisfy the requested pipeline
  (including its playback sink), rather than silently degrading.
- `disabled`: compatibility collector behavior, not a separate permanent engine.

Retain existing `voice_streaming_tts_enabled`, input modes, request IDs, event
names, and APIs through adapters during the deprecation period. Hot reload uses
per-session configuration snapshots; mode/media-format changes restart at a
safe boundary. Additive UDS and HTTP changes precede client rollout. A rollback
must remain able to read newly written history, ignoring additive delivery data.

Input-source validation is independent of playback selection. Voice capture must
use the board microphone through `audio_service`; missing board audio must fail
voice activation explicitly, never fall back to a desktop, browser, or phone
microphone. Batch STT/fallback may process PCM/WAV captured on the board; uploaded
audio must not become an alternate product voice source. Preserve archived-audio
viewing and isolated test fixtures without turning them into live-input routes.
The implementation must document/reject incompatible legacy input settings;
compatibility does not override this fixed source boundary. This document does
not change any currently deployed behavior.

## 11. Security and observability

### 11.1 Security requirements

- Streamed data can become irreversible once shown or spoken. Apply visibility,
  redaction, and safety policy **before** the relevant output projection, not
  only when the completed response is persisted.
- Do not speak raw model reasoning, tool arguments, credentials, or untrusted
  markup. Existing tool validation/approval remains authoritative.
- Put maximum sizes on frames, events, tool arguments, transcripts, and buffered
  content; validate lengths and formats before allocating.
- Web text/event endpoints require authenticated ownership, origin checks, TLS
  where applicable, rate limits, and session-scoped cancel permissions. Provider
  secrets stay on the server. No client-to-provider live-media route is planned.
- Audio/transcript retention and telemetry upload are explicit policy choices.
  Do not add raw audio uploads or private transcripts to telemetry by default.
  Preserve current opt-in telemetry and sanitize diagnostic captures.

### 11.2 Measures and trace structure

Extend the existing episode/OTLP integration, preserving stable names and
per-call model/usage attribution. Keep the current OTLP contract and ingestion
version; no Langfuse SDK or platform upgrade is required by this proposal.
Consult current Langfuse guidance [S13] when implementing instrumentation.

Group turns/runs by conversation; link background tasks rather than duplicating
them as both dispatch and execution. Useful stages are capture, endpointing,
STT, each model attempt, tool execution, TTS, and playback. Emit summaries and
milestones, not one span per token/audio frame.

Record at minimum:

- Last user speech sample, accepted end-of-turn, first/final transcript.
- LLM request, first visible text, first speech-approved text, final response.
- TTS request, first decoded PCM, first rendered audio, last rendered audio.
- Interruption detected/accepted, local stop, remote cancellation acknowledgment.
- Queue duration/bytes, overflow/discontinuity, underruns, stale-event drops.
- Provider/model/API version, mode, formats, network region, token/audio usage,
  retries, fallback, cold/warm setup, terminal/delivery status, and task outcomes.

Report `end_of_user_speech → first_rendered_audio` as the principal voice latency.
Also report `turn_commit → first_rendered_audio` to separate endpointing delay.
Do not call provider first-audio time “user-perceived latency.” Acknowledgment
speech and first useful answer have separate metrics so filler cannot game SLOs.

### 11.3 Proposed acceptance budgets

For a warm, tool-free turn on a selected provider/model/region and healthy test
network, start with these hypotheses; establish the actual baseline in phase 0:

| Measure | Initial target |
| --- | --- |
| Last user speech → first useful rendered audio | p50 ≤1.0 s, p95 ≤1.8 s |
| Accepted interruption → board playback stopped | p95 ≤150 ms |
| Local processing overhead excluding provider/network | p95 ≤100 ms per turn on target hardware |
| First streaming audio versus generation end | Audio begins before response completion in streaming-capable fixture and live smoke cases |
| Stale output after stop acknowledgment | Zero frames presented from invalidated epochs |
| Duplicate tool execution / duplicate spoken segment | Zero in deterministic fault/replay tests |
| Session memory and goroutines | Bounded queues; no sustained growth during a 1-hour soak |

Tool-heavy, cold-start, degraded-network, and batch-fallback scenarios get
separate distributions. Do not block a release on an invented provider SLA;
approve measured thresholds after baselining and report regressions explicitly.

## 12. Validation strategy

### 12.1 Deterministic tests first

Build a fake clock, scripted provider events, and fake playback receipts so the
following do not depend on network timing or paid APIs:

1. Arbitrary token/UTF-8/tag boundaries; multiple tool calls; reasoning/content
   separation; final snapshot equals folded deltas without duplicate speech.
2. STT hypothesis replacement/retraction; multiple final segments per utterance;
   delayed readiness; empty speech; setup/finalization failure and bounded fallback.
3. TTS flush versus finish; sentence/prosody boundaries; slow synthesis/speaker;
   odd PCM fragments; resampler continuity and format changes.
4. Interrupt before first token, during arguments, during synthesis, after
   generation but before drain, and at the same time as response completion.
5. Saturated data queues while cancellation arrives; stalled writes; EOF without
   terminal; duplicate/out-of-order provider events; anonymous realtime events.
6. Disconnect/reconnect/lease expiry, stale epochs, missing cancel acknowledgment,
   retry after visible output, and crash recovery with uncertain delivery.
7. Tool side effects that complete during cancellation; parallel calls; background
   task continuation; steer revisions; notification/farewell playback admission.
8. Old/new HTTP and UDS compatibility; slow SSE consumers; snapshot resync;
   observer disconnect versus owning-request disconnect.
9. Per-session format/provider reload isolation; malformed frames and oversized
   payloads; concurrency/race tests and goroutine leak checks.
10. Board-only source validation: reject alternative microphone/input settings;
    missing `audio_service` never selects a local recorder; board-captured batch
    fallback works, and offline fixtures cannot enable a product input route.

Preserve existing tests around [speech parsing](../../src/agent/internal/agent/speech/text_test.go),
[STT finalization](../../src/agent/internal/agent/stt_streaming_session_test.go),
[Anthropic reasoning replay](../../src/agent/internal/agent/anthropic_thinking_replay_test.go),
[realtime admission](../../src/agent/cmd/daemon/realtime_busy_test.go), and
[board playback](../../tests/audio_playback_session_test.cpp). Expand them; do
not replace them with only happy-path stream tests.

Add sanitized provider protocol fixtures for every shipped adapter. Pin the
model/API/protocol version in fixture metadata and make unsupported capability
cases part of the matrix. Existing providers remain supported even if some stay
explicitly batch-only.

### 12.2 Real workflow checks

- Board: use the board microphone for a long reply and verify audible output
  starts before generation finishes. Interrupt repeatedly and verify the next
  turn never plays old audio. Measure sound/loopback where possible, not merely
  socket timestamps. Desktop output tests use approved board recordings or
  isolated fixtures, never a desktop microphone; keep those checks distinct from
  board acoustic acceptance.
- Chinese, English, and code-switching: pauses, self-correction, numbers, names,
  backchannels, noise, and long utterances. Evaluate STT accuracy and spoken-text
  intelligibility as well as latency.
- At least one existing native realtime provider with explicit IDs/cancellation
  and Gemini's differing event semantics; then run conformance for every shipped
  adapter. Cover one native incremental TTS and one buffered/batch adapter.
- Test slow/unstable networks, cold connection setup, provider throttling, missed
  speech boundaries, AEC during speaker playback, and an hour-long session.
- Test text clients, browser observers, background tasks, notifications, and
  settings reload while board voice is active. Browser live audio, phone audio,
  and desktop microphone input have no acceptance matrix because they are out of
  scope.

Paid provider tests and audio recording require approved credentials, cost
limits, and data consent. Deterministic CI never depends on personal keys.

### 12.3 Repository commands

Follow the repository's Docker-backed test workflow. These are commands for
implementation validation, **not checks run for this research document**:

```bash
make check
bash scripts/run_tests_in_docker.sh --list
bash scripts/run_tests_in_docker.sh --suite go-unit
bash scripts/run_tests_in_docker.sh --suite cpp-host
bash scripts/run_tests_in_docker.sh --suite web
```

Add focused streaming/race suites to the authoritative manifest when introduced;
do not invent an out-of-band host-toolchain validation path. CI runs
`make check-full`; run that locally only when explicitly requested or when
investigating a full-suite failure. A successful quick profile alone does not
prove the full agent matrix or acoustic behavior. If Docker is unavailable,
report it rather than treating unrun checks as passed.

## 13. Migration plan and delivery gates

Use a strangler migration: new contracts wrap existing implementations, then
move one responsibility at a time. No big-bang rewrite and no unrelated package
renaming. Indicative elapsed time is **8–12 weeks for 2–3 engineers with board
hardware and provider access**; it is a planning range, not a commitment.
Browser/WAN voice, phone media, desktop microphone input, and runtime replacement
are explicitly excluded rather than separately estimated.

| Phase | Work and likely touch points | Exit gate |
| --- | --- | --- |
| **0 — Baseline and ADRs** (about 1 week) | Capture cold/warm timelines, effective capability matrix, and board microphone/speaker measurements; settle event identity, ownership, queue and history policies | Reproducible baseline plus approved scope/SLO hypotheses; no changed behavior |
| **1 — Core contracts** (1–2 weeks) | Add leaf stream types, collector shims, attempt/epoch identity, bounded queues, terminal rules, fake-clock tests; bridge current runtime writer and realtime events | Contract tests pass; existing callers and event formats unchanged |
| **2 — Output vertical slice** (about 2 weeks) | LLM → existing speech parser → TTS → presentation coordinator; split close/drain; hard board queue bound and stop receipts; desktop development output sink | Long board response is audible before generation ends; cancellation remains prompt under saturation; no duplicate tool speech |
| **3 — Streaming input** (1–2 weeks) | STT transcript events, pre-ready buffer, context-aware batch fallback, turn authority and live captions | First phonemes preserved; hypotheses revise correctly; exactly one accepted turn per utterance |
| **4 — Realtime convergence** (about 2 weeks) | Extract daemon policy behind interfaces; use shared presentation/history/telemetry; preserve native adapters and foreground/background behavior | Provider replay/cancel/watchdog matrix passes, including omitted/reordered events |
| **5 — Client rollout and hardening** (1–2 weeks) | v2 event negotiation, observer resync, capability UI, mixed-version compatibility, soak and multilingual tests; remove proven-unused shims | Measured acceptance, documented degraded modes, rollback rehearsed |

Phases overlap only after their contracts are stable. Suggested parallel work:
one engineer owns controller/runtime, one owns STT/TTS/provider conformance,
and one owns C++ board audio and text UI. Hardware/QA time is needed throughout.

### Concrete review-sized work items

1. Add stream identity/terminal fixtures and capability inventory; no behavior change.
2. Add typed events around existing LLM callbacks plus a blocking collector.
3. Add generation/attempt invalidation and retry-after-output tests.
4. Extract presentation ownership and introduce finish/drain/stop distinctions.
5. Fix board queue saturation and add loss/progress protocol fields with compatibility tests.
6. Implement live PCM for the desktop development output sink; do not add desktop capture.
7. Bridge the existing speech parser/TTS adapters; preserve tool and notification speech behavior.
8. Add STT readiness buffering and context-aware batch cancellation.
9. Add transcript revision/final/turn events to current streaming STT adapters.
10. Migrate native realtime policy behind the controller while retaining adapters.
11. Add web v2 event projection, cursor/snapshot recovery, and effective-capability UI.
12. Add end-to-end metrics, fault/soak suites, canary flags, and migration docs.

The existing [runtime](../../src/agent/internal/agent/runtime.go#L1156),
[audio dialog](../../src/agent/internal/agent/audio_dialog.go#L886),
[realtime daemon loop](../../src/agent/cmd/daemon/realtime_wakeup.go#L1444),
[TTS contract](../../src/agent/internal/agent/tts/provider.go#L15), and
[STT contract](../../src/agent/internal/agent/stt.go#L13) are the main seams.
Do not begin by moving hundreds of files or replacing `langchaingo` globally.

### Rollout and rollback

- Use a temporary session-level engine feature flag; one session uses one engine.
  Keep device-level/user-level canaries and an immediate fallback to the legacy
  engine. Do not switch engines midway through an utterance.
- Shadow only pure event folding/projection with sanitized fixtures or recorded
  data. Never double-run tools, play both engines, or send live audio to a second
  provider without an approved experiment and consent.
- Ship server/protocol compatibility before client opt-in. Retain the legacy
  projection until all supported clients have migrated and rollback is tested.
- Store additive delivery metadata or a sidecar with an explicit schema version;
  avoid destructive history migration. Validate old binaries can still operate.
- Roll back on stale speech, duplicate actions, cancel latency regressions,
  persistent memory growth, or worse task success—not only on process crashes.
- Remove compatibility code only after measured parity and an agreed support
  window; do not leave two permanent orchestration implementations.

## 14. Confirmed scope and remaining decisions

### Confirmed product boundaries

- Build the streaming core only inside the existing Go/C++ runtime. Do not
  replace it with a third-party framework or outsource voice orchestration.
- The board microphone through `audio_service` is the sole product voice input,
  not merely the first release's default. No browser, phone, or desktop input.
- No browser live voice or phone media path. Web text/captions/events and Phone
  Bridge commands/results remain in their existing roles.

### Policies still requiring approval

Only these remaining product tradeoffs need owner approval:

| Decision | Recommended default | Why confirmation matters |
| --- | --- | --- |
| Mandatory streaming providers | Keep all current providers; label final-only adapters and offer `required` policy | True streaming cannot be promised for unsupported provider/model combinations |
| Default voice engine | Preserve current selected mode; support both cascade and native realtime | Quality, region, cost, tool control, and language coverage differ; no universal winner was measured |
| Classic barge-in | Button interruption by default; full-duplex opt-in after AEC validation | False interruptions and echo depend on real hardware/acoustics |
| Interrupted conversation history | Retain generated content with status; replay only delivery-supported speech plus interruption metadata | This is user-visible behavior and changes model context |
| Latency / cost targets | Baseline first; use section 11 targets as initial hypotheses | Aggressive endpointing/speculation may improve latency but harm accuracy and cost |
| Regional validation | Prioritize existing China-accessible providers plus one global reference path | Endpoint region/proxy routing materially changes perceived latency and privacy constraints |
| Resourcing and rollout | 2–3 engineers, 8–12-week staged core migration, session-level canaries | Board hardware, provider access, and compatibility scope drive schedule |

After approval of the remaining policies, write the short contract ADRs and
implement phase 0/1. The confirmed scope does not by itself authorize the whole
implementation or landing changes.

## 15. Primary sources

All sources accessed on **2026-10-29**. These are living documentation unless
noted. Framework lessons are design recommendations, not claims that Aico has
already adopted those frameworks or reproduced their performance.

- **[S1] LiveKit Agents:** [Agent sessions](https://docs.livekit.io/agents/logic/sessions/), [turn detection and interruptions](https://docs.livekit.io/agents/logic/turns/), [turn tuning](https://docs.livekit.io/agents/logic/turns/tuning/).
- **[S2] Pipecat:** [Frames](https://docs.pipecat.ai/api-reference/server/frames/overview), [interruption lifecycle and context](https://docs.pipecat.ai/pipecat/fundamentals/interruptions).
- **[S3] OpenAI:** [Realtime VAD](https://developers.openai.com/api/docs/guides/realtime-vad), [Realtime client events](https://developers.openai.com/api/reference/resources/realtime/client-events), [Agents SDK voice transports](https://openai.github.io/openai-agents-js/guides/voice-agents/transport/), [RealtimeSession](https://openai.github.io/openai-agents-js/openai/agents/realtime/classes/realtimesession/).
- **[S4] Google:** [Live API messages](https://ai.google.dev/api/live), [WebSocket setup](https://ai.google.dev/gemini-api/docs/live-api/get-started-websocket), [Live API capabilities](https://ai.google.dev/gemini-api/docs/live-api/capabilities).
- **[S5] Microsoft:** [Azure Voice Live API, 2026-01-01 preview reference](https://learn.microsoft.com/en-us/azure/ai-services/speech-service/voice-live-api-reference-2026-01-01-preview). This is a versioned preview reference, not a guarantee of deployment/model feature parity.
- **[S6] Vapi:** [Server events](https://docs.vapi.ai/server-url/events), [WebSocket transport](https://docs.vapi.ai/calls/websocket-transport).
- **[S7] Retell AI:** [Custom LLM overview](https://docs.retellai.com/integrate-llm/overview), [LLM WebSocket protocol](https://docs.retellai.com/api-references/llm-websocket), [streaming and discarded responses](https://docs.retellai.com/integrate-llm/integrate-llm).
- **[S8] ElevenLabs:** [Agent WebSocket API](https://elevenlabs.io/docs/eleven-agents/api-reference/eleven-agents/websocket), [Speech Engine](https://elevenlabs.io/docs/eleven-api/guides/cookbooks/speech-engine).
- **[S9] LangChain / LangGraph:** [Typed event streaming](https://docs.langchain.com/oss/python/langchain/event-streaming), [graph stream modes](https://docs.langchain.com/oss/python/langgraph/streaming). The inspected Python docs describe event streaming `version="v3"`; incremental tool-argument chunk semantics note `langchain-core>=1.6.13`. These are research references, not new Aico dependencies.
- **[S10] Dify:** [Agent API](https://docs.dify.ai/en/api-reference/guides/agent), [chat message streaming](https://docs.dify.ai/en/api-reference/chat-messages/send-chat-message).
- **[S11] Deepgram:** [Endpointing versus interim/final results](https://developers.deepgram.com/docs/understand-endpointing-interim-results).
- **[S12] Alibaba Cloud:** [Realtime STT guide](https://www.alibabacloud.com/help/en/model-studio/real-time-speech-recognition-user-guide), [ASR streaming client events/readiness](https://www.alibabacloud.com/help/en/model-studio/qwen-audio-asr-streaming-client-events), [realtime TTS and flush/finalization](https://www.alibabacloud.com/help/en/model-studio/realtime-tts-user-guide), [Qwen realtime WebSocket guide](https://help.aliyun.com/zh/model-studio/fun-audiochat-realtime-websocket-api).
- **[S13] Langfuse:** [Tracing best practices](https://langfuse.com/docs/observability/best-practices), together with Aico's [existing OTLP integration](telemetry-langfuse.md).

## 16. Work completed for this proposal

- Inspected current provider, runtime, voice, audio-service, client, task, and
  telemetry boundaries and their existing test contracts.
- Researched the official sources above and separated observed behavior from
  recommendations and unmeasured targets.
- Added this proposal and a link from the Agent overview. Provider protocols,
  prompts, configuration keys, dependencies, firmware, and deployment interfaces
  remain unchanged by this initial slice.
- Implemented the first compatibility-preserving slice: the provider-independent
  `src/agent/internal/stream` package now defines v2 event identity, terminal
  outcomes, negotiated audio-frame metadata, and context-aware bounded queues,
  with deterministic unit tests. `AudioPlaybackSession` now rechecks its hard
  queue limit after backpressure waits, returns `TIMEOUT` when saturation
  persists, and rejects writes after stop.
- Continued the compatibility-preserving implementation through the streaming
  input boundary: runtime callbacks now emit typed stream events; TTS sessions
  distinguish input finish, cancellation, synthesis completion, and playback
  drain; STT providers expose context-aware batch calls, bounded pre-ready PCM
  buffering, revisioned hypotheses, segment finals, turn commits, and optional
  live observers. DashScope and Tencent realtime adapters now publish bounded
  transcript event channels, while batch-only providers remain explicit.
- Added provider-independent transcript folding and bounded replay-window
  contracts. The replay window detects stale epochs, expired cursors, duplicate
  sequences, and gaps that require a snapshot/resync. TTS capability metadata
  now reports effective text granularity and incremental-audio support, and
  board playback bounds its queue by both chunk count and PCM duration.
- Added focused regression coverage for stale STT sessions, readiness overflow,
  transcript revision ordering, provider cancellation, observer shutdown,
  replay cursors, TTS capability declarations, and playback queue saturation.
- Checked relative links/source-line anchors, Markdown heading fragments, code
  fences, and whitespace; a separate design review identified a broken local
  heading link, which was corrected. These are document checks, not a docs build.
- Focused Go tests (including `-race`), Go vet, and the standalone C++ playback
  suite passed for the implemented stages. The Docker-backed firmware suites,
  full host suite, and paid live/hardware provider smoke tests were not run:
  the Docker daemon and some host listener facilities are unavailable in the
  current environment. Credentials are loaded but were not used for paid
  provider calls.

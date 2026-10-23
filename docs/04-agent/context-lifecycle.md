---
sidebar_position: 7
---

# Agent Context Lifecycle

This document gives the end-to-end view of what enters an Aico Agent run, where each context layer comes from, and when it is refreshed. It complements [Session Memory Compaction](session-memory.md), which focuses on conversation-window compression, and [Memory Plane](memory-plane.md), which focuses on device, long-term, and task-episode memory.

## Context Layers

Each run is assembled from several layers. Some layers are persisted memory, while others are request-local prompt context.

| Layer | Source | Visibility | Persistence |
| --- | --- | --- | --- |
| Base instruction | built-in Agent instruction plus `agent.toml` `prompt` | Agent | configuration |
| Runtime defaults | built-in prompt rules, current date, host runtime information | Agent | not persisted |
| Skills | skill index plus active `SKILL.md` content | Agent | skill files and skill state |
| Runtime context | `RunRequest.RuntimeContext`, for example phone bridge state | Agent | not persisted |
| Tool catalog | resolved built-in tools plus skill tools | Agent can call | not persisted in memory |
| Session summary and user profile | session and long-term profile files | Agent system prompt | filesystem memory |
| Recalled device memory | `recall_device_memory` tool results | Agent tool context | filesystem memory |
| Conversation history | hot-window memory, optional compressed-history markers | Agent | filesystem memory |
| Input attachments | `RunRequest.Attachments` | Agent user messages | current run only |

## Agent Execution Loop

The Agent uses a streamlined execution loop that processes user input through tool calls and generates responses directly. All runs share a single iteration budget controlled by `MaxIterations`.

Execution flow:

```text
Run starts
  |
  v
Agent receives user input and context
  |
  v
Agent decides on action (tool call or final answer)
  |
  v
Execute tool calls as needed
  |
  v
Generate final response
  |
  v
Run ends
```

The Agent has access to all registered tools throughout execution and manages its own task breakdown strategy based on the request complexity.

## Prompt Shape

For the current execution loop, `Runtime.Run` builds a `RoleProfile` before execution. The Agent system prompt contains:

1. role identity;
2. base instruction;
3. built-in default behavior;
4. available skills summary;
5. active skill instructions;
6. request-local runtime context, if provided;
7. role rules;
8. available tool information;
9. session summary and long-term profile, if available.

The per-call user message is built from the current loop state, including the original request, conversation history, world state, and any previous tool results.

## Memory Recall

The active session summary and synthesized long-term profile can enter the Agent system prompt. Device Memory content is not injected into every run.

Device Memory content is not injected before execution. The model may call `recall_device_memory` on demand when the task materially depends on saved device or UI knowledge. Runtime does not run a pre-execution relevance router, force a first tool call, or rewrite the model's query; the tool records the IDs it actually returns on the Episode.

Normal Device Memory recall returns only active and applicable records. It is capped at five results and approximately 4,800 output characters. Disputed and legacy conflicted records remain available only to background consolidation.

## Run Lifecycle

At a high level, one run follows this sequence:

```text
RunRequest
  |
  v
normalize input and activate requested skills
  |
  v
resolve model, context window, tools, and memory handle
  |
  v
begin session: detect/rotate boundary and append current user input
  |
  v
start EpisodeRecorder
  |
  v
build role profile and agent memory
  |
  v
agent execution loop
  |
  v
commit session: append assistant output and save snapshot
  |
  v
request session-memory maintenance
  |
  v
commit task Episode and notify background consolidation
```

`CurrentEnvironmentHints` are lightweight facts already known to runtime, such as screenshot size, language, and last observed app name. They must not perform device actions. If fresh screen evidence is needed, the Agent must use tools.

## Update Mechanisms

### Runtime Context

Runtime context is supplied per request and is not written back into memory. The phone bridge is the main producer today. It keeps connection status, platform, heartbeat time, app foreground/background state, return-entry state, PiP Bridge state, and the latest phone environment. Each run receives only compact state facts: connection state, system type and version, locale/timezone, screen size, return-entry visibility, PiP/Dynamic Island visibility constraints, and confirmed launchable third-party app candidates. When the bridge disconnects, stale environment data is cleared.

Tool availability is evaluated separately from the model-facing runtime-context
text. Before each conversational run, the Agent filters App-only tools and
their supported actions from the same live Phone Bridge and BLE capabilities.
`open_app` remains exposed because it can select BridgeOpenApp or fall back to
SearchLaunchApp. `open_url`, `bridge_clipboard`, `bridge_calendar`,
`bridge_contacts`, and `bridge_notification` are exposed only when foreground
Phone Bridge, Dynamic Island restoration, PiP/FGS polling, iOS BLE Wake, or the
direct BLE notification-query path can execute the relevant operation. In an
iOS BLE Wake-only state, clipboard actions and contacts update are omitted;
contacts query/create and notification send/query are filtered independently.

External runtime signals may add model-facing facts to runtime context without
overriding session-boundary detection. For example, when the physical wakeup
button interrupts an in-flight voice turn, the next voice input receives runtime
context describing that interruption. Session continuation still follows the
normal boundary rules, such as the short-gap continuation rule. A normal wakeup
after the previous turn has finished goes through the same automatic detection.
These signal facts are not persisted as session-event relationship labels.

### Session Hot Window

At run begin, session management detects whether the input starts a new session
and appends the current user input to `memory/session/events.jsonl`. At run
commit, it appends the assistant output, persists the current snapshot with
`SaveSnapshot`, and schedules filesystem maintenance with
`RequestMaintenance`.

The hot window lives in:

```text
memory/session/events.jsonl
```

At prompt time, retained hot-window events are converted into native chat
messages and inserted between the system prompt and the current
task message. Prompt construction does not render them inside a
`Conversation history:` text block and does not add hot-window labels or
boundary markers.

When session-boundary detection classifies a user turn as a new session, the
runtime archives the whole active `memory/session/` directory into
`memory/session_archive/<closed_session_id>/`, recreates an empty
`memory/session/events.jsonl`, and clears the in-memory conversation window.
Archived sessions are preserved as logs only; they are not restored, listed,
switched back into, injected into prompts, or searched by
`recall_session_chunks`.

### Session Compaction

Session compaction is handled by `MemoryManager.maintainFilesystemMemory`. It reads active `events.jsonl`, decides whether compaction is needed, writes compacted chunks, updates active `summary.md`, and replaces `events.jsonl` with the retained hot window.

Compression can be triggered by:

- prompt-token usage relative to the active model context window;
- reserved token headroom;
- configured percentage threshold;
- event count fallback when prompt-token data is unavailable.

The active context window comes from `ModelResolver.Spec()` when available. Unknown models fall back to `memory/extraction.yaml`.

Compaction preserves context by:

- snapping cuts to legal event boundaries;
- keeping adjacent leading `system_event` and `screen_context` events;
- pinning the root user input when needed;
- summarizing split-turn prefixes and injecting a synthetic hot-window event;
- scrubbing screenshot base64 payloads before persistence;
- resetting `lastPromptTokens` to the retained hot-window estimate after compaction.

The compressed artifacts live in:

```text
memory/session/summary.md
memory/session/summary_archive.md
memory/session/chunks/index.yaml
memory/session/chunks/<chunk_id>.jsonl
```

### Long-Term Memory And Profile

Long-term memory stores user preferences, rules, facts, procedures, and manually saved memories under:

```text
memory/long_term/
```

The `save_memory` and `forget_memory` tools update this store. `profile.md` is rebuilt through the long-term memory profile pipeline, with debouncing to avoid repeated rebuilds during bursts of writes.

Within one runtime, memory tools, `MemoryPlane`, and profile rebuilding share a single `LongTermMemoryStore`. Its parsed-Markdown cache has a fixed admission bound so full scans cannot grow memory without limit or continually replace the useful working set. Long-term index version 2 carries `expires_at`, allowing search and profile generation to reject expired entries before reading their Markdown files.

### Device And Episode Memory

The episode recorder captures tool calls, tool results, screenshots, user corrections, and recorded outcome data during the run. `MemoryPlane.CommitEpisode` persists the trace and notifies the shared background `MemoryWorker`; the worker later invokes `EpisodeProcessor` during the common idle window.

Common episode event types:

| Event type | When recorded |
| --- | --- |
| `tool_call` / `tool_result` | Agent tool use during execution |

The main stores are:

```text
memory/device/
memory/episodes/
memory/lifecycle/
```

Success and failure do not gate learning. The worker independently assesses goal completion, then can create or update procedure, navigation, calibration, failure, or fact Device Memory when direct evidence supports it.

Deterministic device and app profiles remain separate from LLM-generated lessons. Automatic Episode learning does not write user profile, preference, or rule entries to Long-Term Memory.

## Storage Map

```text
/userdata/agent/memory/
|-- session/
|   |-- events.jsonl             # hot window
|   |-- summary.md               # compressed session summary
|   |-- summary_archive.md
|   `-- chunks/
|-- session_archive/             # closed session logs, not active context
|   `-- <closed_session_id>/
|-- long_term/
|   |-- profile.md               # synthesized profile
|   |-- index.yaml
|   `-- memories/
|-- device/
|   |-- profile.yaml
|   |-- apps/
|   |-- procedures/
|   |-- navigation/
|   |-- calibration/
|   `-- failures/
|-- episodes/
|   |-- index.yaml
|   `-- <yyyy>/<episode_id>/
`-- lifecycle/
```

## Invariants

- Runtime context is request-local; do not use it as durable memory.
- Active session summaries are durable within the current session; archived
  session summaries are logs and are not prompt or recall context.
- Hot-window labels or boundary markers are not injected into prompts or
  durable memory.
- The Agent sees retrieved experience memory and manages its own execution strategy.
- Tool results and screenshots are evidence for the current run; reusable lessons are written only after episode commit or explicit memory-tool calls.
- Device actions must be based on current tool observations or screenshots, not on stale remembered state alone.

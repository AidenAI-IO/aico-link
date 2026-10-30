---
sidebar_position: 1
---

# Agent Overview

Aico is the AI Agent running on Aico Link hardware. Its Go implementation is
located in `src/agent/` and is built on `github.com/tmc/langchaingo`. It serves
as both a long-running daemon and the device-side tool control plane.

## Binaries

| Entry Point  | Description                                                     |
| ------------ | --------------------------------------------------------------- |
| `cmd/daemon` | Long-running daemon providing the Web UI and optional device voice loop |

After cross-compilation, the daemon binary is:

```text
output/debian-apps/apps/bin/agent
```

In the firmware, it is installed by default to:

```text
/usr/lib/aico/agent
```

## Current Capabilities

- Registered model providers: `openai`, `anthropic`, `openrouter`, `kimi`, `kimi-cn`, `volcengine`, `deepseek`, `gemini`, `ollama`
- Built-in tool calling: HID, screenshots, audio volume, shell
- HTTP Tool API for Web UI, external agents, or manual invocation
- Auto-discovery and runtime activation of skills from `SKILL.md`
- Single-agent execution loop with streamlined tool calling and response handling
- Conversation memory persistence, session memory compaction; see [Session Memory Compaction](session-memory.md)
- Device and Task Episode memory; see [Memory Plane](memory-plane.md)
- Web UI: chat history, browser recording, attachments, Tool Lab, Skill Export
- iOS Live Activity / Dynamic Island task status; see [Live Activity / Dynamic Island](live-activity.md)
- Device-side voice pipeline: VAD / STT / TTS; [Realtime Voice](realtime-voice.md)

## Run Modes

The HTTP server and Web UI start in every input mode. `input_mode` controls whether the daemon also runs the device-side voice loop:

| Mode   | Behavior                                                                    |
| ------ | --------------------------------------------------------------------------- |
| `text` | HTTP server and Web UI only                                                  |
| `stt`  | HTTP server and Web UI plus device recording → VAD → STT → LLM → TTS loop    |
| `realtime` | HTTP server and Web UI plus the configured realtime voice model session |

In `stt` mode, Web UI requests and device voice interactions share the same Agent runtime and run in the same daemon process.

The Web UI reveals streamed answer and reasoning text character by character,
with faster playback for large bursts. It keeps the complete received text in
memory and waits for playback to finish before reconciling completed history.
Text accompanying a tool call remains an ordinary assistant message before the
tool details, including after history reloads. It retains its pending animation
at the tool boundary; explicit stream resets discard the active draft.
History loads, background tabs, and reduced-motion preferences show text without
animation.

## Proposed Architecture Work

The [Streaming-First Agent Refactor Plan](streaming-first-refactor-plan.md)
compares current voice-agent frameworks with Aico's existing implementation and
proposes a staged migration for LLM, STT, TTS, realtime, playback, and client
streaming within the existing Go/C++ runtime. The proposed voice-input source is
exclusively the board microphone; browser and phone live voice are excluded.
It is a research/design proposal, not a description of shipped behavior.
A [Chinese translation](streaming-first-refactor-plan.zh-CN.md) is also available.

## Startup

Local development:

```bash
cd src/agent
go run ./cmd/daemon -dir ./config -addr :8080
```

Device service:

```bash
/usr/lib/aico/aico-managed-env-run /usr/lib/aico/agent -dir /userdata/agent -addr :8080
```

## Built-in Tools

The conversational Agent receives every registered, currently available tool needed for memory, device operation, skill management, shell and web research, and phone data.

`current_time` and `calculator` are not registered built-in tools; the Agent uses `shell` for controller-local precise time, timezone, and deterministic calculations. `skill_manage` and `skill_mark_used` remain unavailable through the HTTP Tool API.

For tool details and HTTP invocation methods, see [Tools HTTP API](tools-http-api.md).
For the same Agent tools over MCP Streamable HTTP (`POST /mcp`), see [MCP](mcp.md).

## Python Package Environment

Firmware-provided pip, persistent Agent-installed packages under `/userdata`,
direct shell-based installation, and limited StorageMonitor cleanup are
documented in [Persistent Python Package Environment](python-packages.md). The
implementation reuses the existing `shell` tool and does not add a built-in
package-install tool.

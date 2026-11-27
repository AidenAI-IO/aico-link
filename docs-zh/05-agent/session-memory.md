---
sidebar_position: 8
---

# 会话记忆压缩

本文介绍 Aico 会话级别的对话记忆压缩。该机制把不断增长的事件流转换为 chunk 摘要，并只保留最新的热窗口，以原生聊天消息的形式提供给 Agent。

这与[记忆平面设计](memory-plane.md)是两回事。记忆平面处理设备和任务 episode 的经验记忆；会话记忆处理单个活动会话的对话历史。

## 流程

运行时在运行开始时追加当前用户输入，在运行提交时追加助手输出。提交后，Agent 调度 `MemoryManager.RequestMaintenance`。对于直接调用 `MemoryManager.Save` 的调用方，它会同步执行同一条维护路径。

```text
read session/events.jsonl under FileLock
  |
  v
shouldCompress? -- no --> leave events unchanged
  |
 yes
  v
planCompaction -- choose cut point (token first, count fallback)
  |
  v
summarize history (with split-turn prefix when needed)
  |
  v
compressEvents --> write chunk and update summary.md / index
  |
  v
re-read session/events.jsonl under FileLock and merge new appends
  |
  v
replaceEvents --> keep only the hot window in events.jsonl
```

`/userdata/agent/memory/session/` 始终是当前的活动会话：

```text
session/
├── events.jsonl          # hot window: recent uncompacted events
├── summary.md            # Rolling Summary + Recent Chunks, injected into the prompt
├── summary_archive.md    # older chunk summaries after summary_max_chunks overflow
└── chunks/
    ├── index.yaml        # chunk index, with optional cut_meta
    └── <chunk_id>.jsonl  # full compacted events, retrievable by recall_session_chunks
```

已关闭的会话归档在活动目录之外：

```text
session_archive/
└── <closed_session_id>/
    ├── events.jsonl
    ├── summary.md
    ├── summary_archive.md
    └── chunks/
```

归档会话仅作为日志保留。它们不会被加载到提示词中，不会使 `HasCompressedHistory()` 为 true，不会被活动会话的维护流程压缩，也不会被 `recall_session_chunks` 搜索。

## 召回结果来源

`recall_session_chunks` 返回的每条结果都带有 `source` 字段。来自 `memory/session/chunks/index.yaml` 的已索引 chunk 使用 `source: "active"`，即使其 `chunk_id` 带有旧式或源自 pending 的前缀（例如 `pending-...`）。实时 pending 文件的召回路径在这些文件被消化为正常的已索引 chunk 之前，必须使用 `source: "pending"`。

遥测使用这个显式的 source 字段。`pending_chunks_recalled` 只统计 `source: "pending"` 的结果；它不会根据 chunk ID 推断来源。chunk ID 是不透明的标识符，不得用作 pending/实时状态的信号。

## 会话边界轮转

当会话边界检测把某个轮次判定为 `new` 时，`MemoryManager` 会通过把整个 `memory/session/` 目录原子地移动到 `memory/session_archive/<closed_session_id>/` 来关闭当前活动会话。然后，它把 `memory/session/events.jsonl` 重新创建为空文件，并清空内存中的对话窗口和事件计数器。

这只实现了多会话的存储隔离。Aico 目前还不能恢复、列出或切换回旧会话。始终只有一个活动会话，新的活动会话以空的对话上下文开始。

## 并发与事件保留

维护可能在下一轮次正在追加时异步运行。为了避免 LLM 摘要阻塞热路径，`maintainFilesystemMemory` 使用两阶段文件锁模式：

1. 在 `FileLock` 下读取当前的 `events.jsonl` 快照。
2. 在规划切分点和生成摘要期间释放锁。
3. 重新获取 `FileLock`，重新读取 `events.jsonl`，并在替换文件之前，把原始快照之后新增的事件追加到保留的热窗口中。

`RequestMaintenance` 用一个 pending 标志合并并发请求，因此重复的轮次完成只会再调度一次维护，而不会运行相互重叠的压缩。压缩成功后，`lastPromptTokens` 会被重置为保留热窗口的估算大小；这可以防止待执行的那次维护用压缩前的过期 token 数据，立即再次压缩同一个短窗口。

## 压缩触发条件

`shouldCompress` 有两条路径：

- 当有提示词 token 数据且上下文窗口已知时，满足以下任一条件即触发压缩：
  - `prompt_tokens >= context_window - reserve_tokens`，其中 reserve 会被限制为最多窗口的一半。
  - `prompt_tokens / context_window >= compress_at_percent%`。
- 当没有提示词 token 数据时（例如冷启动、提供商响应中不含 usage 元数据，或在第一次 LLM 调用之前），当 `event_count > count_compress_after_events` 时触发压缩。

提示词 token 值取最近一次运行中观察到的最大单次 LLM 提示词。这样可以避免在主提示词很大、但同一轮次中后续提示词较小时漏掉压缩。

`context_window` 优先使用 `ModelResolver` 给出的当前模型窗口，因此运行时切换模型无需重启即可生效。未知模型会回退到 `extraction.yaml` 中的 `context_window`。

压缩后，存储的提示词 token 值会被重置为保留热窗口的估算大小。这可以防止维护循环用压缩前的过期值立即再次压缩。

## 切分点选择

`planCompaction` 决定在事件流的何处切分。它优先使用 token 路径，当 token 选择无法得到有用的切分时回退到计数路径，并且两条路径都会对齐到合法的切分点。

### 合法切分点

每个事件按 `Type` 分类，没有时回退到 `Role`：

| 类别 | 事件类型 | 能否作为热窗口的起点 |
| --- | --- | --- |
| `cutTurnBoundary` | `user_input` | 能。这是完整的轮次边界，不会产生拆分轮次。 |
| `cutSplitAllowed` | `assistant_output` / `role_output` / `tool_call` | 能，但会在轮次内部切分，产生拆分轮次。 |
| `cutForbidden` | `tool_result` / `system_event` / `screen_context` | 不能。这些事件必须与前一个事件保持在一起。 |

### Token 路径

Token 路径从最新事件开始向前回溯，累加估算的 token，直到达到 `keep_recent_tokens`。然后它把切分点向后对齐到最近的合法切分点。如果所有事件都能放进 `keep_recent_tokens`，则不产生 token 切分。

Token 估算按文字系统切分文本。CJK 字符大约按每个字符一个 token 计算，而 ASCII 和拉丁文本使用 `chars/4` 启发式。这是为了避免低估热窗口中的中文文本。

### 计数回退

当 token 路径无法产生有用的切分时，计数路径以最近 `hot_window_events` 个事件为目标，并使用 `snapToLegalCutAtOrBefore`，避免热窗口以 `tool_result` 或 `system_event` 等禁止的事件开头。

### 前导上下文事件

选定切分点后，紧挨在切分点之前的相邻 `system_event` 和 `screen_context` 事件会被拉入热窗口。这样可以避免热窗口以脱离上下文的环境信息开头。

### 根用户输入固定

实时事件流中最早的 `user_input` 被视为活动会话的根任务目标。如果某次切分会把该事件移入压缩历史，维护流程会改为把它固定为保留热窗口中的第一个事件。

被固定的根输入不会计入 chunk 证据，也不会进入生成的摘要。目的是让原始任务指令能逐字保留，而不依赖摘要的质量。如果固定根输入后没有可压缩的事件，维护流程会推进到下一个合法切分点；如果不再有有用的切分，则跳过压缩。

## 拆分轮次

当切分点落在轮次内部而不是 `user_input` 边界上时，热窗口的开头会缺少导致助手/工具输出的那条用户输入。拆分轮次处理会保留这部分上下文：

1. `history` 是轮次开始之前的事件范围，单独进行摘要。
2. `turn prefix` 是从轮次开始到切分点的事件范围，单独进行摘要。
3. chunk 摘要存储 `history + "\n\n---\n\nTurn Context (split turn):\n" + prefixSummary`，供召回使用。
4. 热窗口会收到一个包含前缀摘要的合成 `system_event`，作为即时的提示词上下文。

前缀摘要被有意同时写入 chunk 摘要和热窗口：前者用于历史检索，后者用于下一次 Agent 提示词。

## summary.md 与 Rolling Summary

活动会话的 `summary.md` 会被注入 Agent 提示词，包含两个部分：

```markdown
# Session History (compressed chunks)

## Rolling Summary

<accumulated summaries of older chunks pushed out of the active summary window>

## Recent Chunks

- **chunk_xxx**
  <summary>
```

`summary_max_chunks` 控制 Recent Chunks 中保留多少条 chunk 摘要。超出限制时，最旧的摘要会被移到 `summary_archive.md`，并折叠进 Rolling Summary。

Rolling Summary 目前有 100 行的上限（`maxRollingSummaryLines`）。溢出时，较旧的行会被丢弃，并添加一个截断标记。未来的滚动检查点可以对 Rolling Summary 本身再做摘要。

## 热窗口在提示词中的位置

压缩后的会话摘要会被加载到角色记忆上下文中，而保留的热窗口事件会被转换为原生聊天消息，插入到系统提示词和当前任务消息之间。它们不会渲染在 `Conversation history:` 文本块中。压缩历史的状态不会通过热窗口标签暴露，也不会添加合成的热窗口开始/结束标记。会话压缩阈值和模型上下文预算是控制提示词增长的手段。

合成的提示词文本不得持久化到 `ChatMessageHistory` 中。`Snapshot()` 原样读取历史，`appendSessionEvents()` 按索引写入记录，因此在其中存储非事件文本会使 `eventCount` 与真实事件流不同步。

## 截图数据清洗

截图工具的结果包含 base64 图像载荷。在持久化之前，`stripScreenshotData` 会移除 `data` 字段，并保留 `width`、`height`、`format`、`size` 和 `action_output` 等元数据。否则，几 KB 的 base64 会被当作 ASCII 文本，抬高热窗口的 token 估算值。

该清洗器是幂等的，并在会话记忆的每个持久化边界上使用：

- `sanitizeMessageRecords`：在把 `MessageRecord` 切片同步到会话事件之前。
- `sessionEventFromRecord`：在把 langchain `MessageRecord` 值转换为 `SessionEvent` 值时。
- `SessionMemoryStore.AppendEvent`：用于绕过转换路径的直接写入。

## chunk cut_meta

chunk 索引条目可以包含可选的 `cut_meta`，用于 token 切分诊断。这些元数据仅用于调试和基准测试；不影响召回或摘要渲染。

| 字段 | 含义 |
| --- | --- |
| `first_kept_event_id` | 保留热窗口中第一个事件的事件 ID。 |
| `tokens_before` | 压缩前所有事件的估算 token 数。 |
| `kept_tokens_estimate` | 保留热窗口中的估算 token 数。 |
| `is_split_turn` | 切分点是否落在轮次内部。 |
| `turn_start_event_id` | 拆分轮次开头的 `user_input` 事件 ID。 |

所有字段都使用 `omitempty`，因此较旧的索引和计数路径的压缩仍保持 YAML 兼容。

## 配置

压缩由 `<config>/memory/extraction.yaml` 配置。参见 [configuration.md 中的 `memory/extraction.yaml` 一节](configuration.md#memoryextractionyaml)。

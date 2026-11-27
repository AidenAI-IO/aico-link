---
sidebar_position: 7
---

# Agent 上下文生命周期

本文从端到端的角度说明哪些内容会进入一次 Aico Agent 运行、每个上下文层来自哪里，以及何时刷新。它是对 [会话记忆压缩](session-memory.md) 和 [记忆平面](memory-plane.md) 的补充：前者侧重对话窗口压缩，后者侧重设备记忆、长期记忆和任务 Episode 记忆。

## 上下文层

每次运行都由多个层组装而成。有些层是持久化的记忆，另一些则是仅在本次请求内有效的提示词上下文。

| 层 | 来源 | 可见范围 | 持久化方式 |
| --- | --- | --- | --- |
| 基础指令 | 内置 Agent 指令加上 `agent.toml` 中的 `prompt` | Agent | 配置 |
| 运行时默认值 | 内置提示词规则、主机运行时信息 | Agent | 不持久化 |
| 轮次状态 | 每个用户轮次之前的状态消息：设备类型、`controller_time`、`controller_timezone`、`shared_files`、截图 | Agent | 对话历史 |
| Skill | Skill 索引加上已激活的 `SKILL.md` 内容 | Agent | Skill 文件和 Skill 状态 |
| 运行时上下文 | `RunRequest.RuntimeContext`，例如手机桥接状态 | Agent | 不持久化 |
| 工具目录 | 解析后的内置工具加上 Skill 工具 | Agent 可调用 | 不在记忆中持久化 |
| 会话摘要和用户画像 | 会话文件和长期画像文件 | Agent 系统提示词；用户画像也会放在实时指令的末尾 | 文件系统记忆 |
| 召回的设备记忆 | `recall_device_memory` 工具结果 | Agent 工具上下文 | 文件系统记忆 |
| 对话历史 | 热窗口记忆，可选的压缩历史标记 | Agent | 文件系统记忆 |
| 输入附件 | `RunRequest.Attachments` | Agent 用户消息 | 仅当前运行 |

## Agent 执行循环

Agent 使用一个精简的执行循环：通过工具调用处理用户输入，并直接生成响应。所有运行共享由 `MaxIterations` 控制的同一个迭代预算。

执行流程：

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

在整个执行过程中，Agent 可以使用所有已注册的工具，并根据请求的复杂度自行决定任务拆解策略。

## 提示词结构

在当前执行循环中，`Runtime.Run` 会在执行前构建一个 `RoleProfile`。Agent 系统提示词包含：

1. 角色身份；
2. 基础指令；
3. 内置默认行为；
4. 可用 Skill 摘要；
5. 已激活 Skill 的指令；
6. 请求级运行时上下文（如果提供）；
7. 角色规则；
8. 可用工具信息；
9. 会话摘要和长期画像（如果有）。

每次调用的用户消息根据当前循环状态构建，包括原始请求、对话历史、世界状态以及之前的所有工具结果。

## 记忆召回

当前会话摘要和合成的长期画像可以进入 Agent 系统提示词。设备记忆（Device Memory）的内容不会注入每次运行。

设备记忆内容不会在执行前注入。当任务实质上依赖已保存的设备或 UI 知识时，模型可以按需调用 `recall_device_memory`。运行时不会在执行前运行相关性路由器，不会强制第一次工具调用，也不会改写模型的查询；该工具会把它实际返回的 ID 记录到 Episode 上。

正常的设备记忆召回只返回处于活跃状态且适用的记录，最多五条结果，输出约 4,800 个字符。有争议的记录和旧版冲突记录只对后台整合可见。

## 运行生命周期

从宏观上看，一次运行遵循以下顺序：

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

`CurrentEnvironmentHints` 是运行时已经知道的轻量事实，例如截图尺寸、语言和最后观察到的 App 名称。它们不得执行任何设备操作。如果需要最新的屏幕证据，Agent 必须使用工具。

## 更新机制

### 运行时上下文

运行时上下文按请求提供，不会写回记忆。目前它的主要来源是手机桥接（Phone Bridge）。手机桥接维护连接状态、平台、心跳时间、App 前台/后台状态、返回入口状态、PiP Bridge 状态以及最新的手机环境。每次运行只接收紧凑的状态事实：连接状态、系统类型和版本、区域设置/时区、屏幕尺寸、返回入口是否可见、PiP Bridge 状态，以及已确认可启动的第三方 App 候选项。桥接断开时，过期的环境数据会被清除。

工具可用性与面向模型的运行时上下文文本分开评估。在每次对话运行之前，Agent 会根据同一份实时的 Phone Bridge 和 BLE 能力，过滤仅限 App 的工具及其支持的操作。`open_app` 始终保留，因为它可以选择 BridgeOpenApp，或回退到 SearchLaunchApp。当为 HTTP(S) URL 配置了可见浏览器回退时，`open_url` 也会保留。只有当前台 Phone Bridge、灵动岛恢复、PiP/FGS 轮询、iOS BLE Wake 或直接 BLE 通知查询路径能够执行相应操作时，才会暴露 `bridge_clipboard`、`bridge_calendar`、`bridge_contacts` 和 `bridge_notification`。在仅有 iOS BLE Wake 的状态下，剪贴板操作和联系人更新会被省略；联系人查询/创建以及通知发送/查询则各自独立过滤。

外部运行时信号可以向运行时上下文添加面向模型的事实，但不会覆盖会话边界检测。例如，当物理唤醒按键打断一个进行中的语音轮次时，下一次语音输入会收到描述该打断的运行时上下文。会话是否延续仍遵循正常的边界规则，例如短间隔延续规则。在上一轮已经结束后进行的普通唤醒，也走同样的自动检测。这些信号事实不会作为会话事件关系标签持久化。

### 会话热窗口

在运行开始时，会话管理会检测输入是否开启一个新会话，并把当前用户输入追加到 `memory/session/events.jsonl`。在运行提交时，它会追加助手输出，通过 `SaveSnapshot` 持久化当前快照，并通过 `RequestMaintenance` 安排文件系统维护。

热窗口位于：

```text
memory/session/events.jsonl
```

在构建提示词时，保留的热窗口事件会被转换为原生聊天消息，插入到系统提示词和当前任务消息之间。提示词构建不会把它们渲染进 `Conversation history:` 文本块，也不会添加热窗口标签或边界标记。

当会话边界检测把某个用户轮次判定为新会话时，运行时会把整个活跃的 `memory/session/` 目录归档到 `memory/session_archive/<closed_session_id>/`，重新创建一个空的 `memory/session/events.jsonl`，并清空内存中的对话窗口。归档的会话仅作为日志保留：它们不会被恢复、列出、切换回去、注入提示词，也不会被 `recall_session_chunks` 搜索。

### 会话压缩

会话压缩由 `MemoryManager.maintainFilesystemMemory` 处理。它读取活跃的 `events.jsonl`，判断是否需要压缩，写入压缩后的分块，更新活跃的 `summary.md`，并用保留的热窗口替换 `events.jsonl`。

压缩可由以下条件触发：

- 提示词 token 用量相对于当前模型上下文窗口的比例；
- 预留的 token 余量；
- 配置的百分比阈值；
- 无法获得提示词 token 数据时，按事件数量回退。

当前上下文窗口在可用时来自 `ModelResolver.Spec()`。未知模型回退到 `memory/extraction.yaml`。

压缩通过以下方式保留上下文：

- 把切分点对齐到合法的事件边界；
- 保留相邻的前导 `system_event` 和 `screen_context` 事件；
- 必要时固定根用户输入；
- 对被切分轮次的前缀生成摘要，并注入一个合成的热窗口事件；
- 在持久化之前清除截图 base64 负载；
- 压缩后把 `lastPromptTokens` 重置为保留热窗口的估算值。

压缩产物位于：

```text
memory/session/summary.md
memory/session/summary_archive.md
memory/session/chunks/index.yaml
memory/session/chunks/<chunk_id>.jsonl
```

### 长期记忆和画像

长期记忆把用户偏好、规则、事实、操作流程和手动保存的记忆存放在：

```text
memory/long_term/
```

`save_memory` 和 `forget_memory` 工具会更新这个存储。`profile.md` 通过长期记忆画像流水线重建，并带有防抖，以避免在集中写入时反复重建。

在同一个运行时内，记忆工具、`MemoryPlane` 和画像重建共享同一个 `LongTermMemoryStore`。它的已解析 Markdown 缓存有固定的准入上限，因此全量扫描既不会让内存无限增长，也不会不断替换掉有用的工作集。长期索引第 2 版带有 `expires_at`，使搜索和画像生成可以在读取 Markdown 文件之前就排除已过期的条目。

### 设备记忆和 Episode 记忆

Episode 记录器会在运行期间捕获工具调用、工具结果、截图、用户纠正和记录的结果数据。`MemoryPlane.CommitEpisode` 持久化这份轨迹并通知共享的后台 `MemoryWorker`；该 worker 随后在公共空闲窗口内调用 `EpisodeProcessor`。

常见的 Episode 事件类型：

| 事件类型 | 记录时机 |
| --- | --- |
| `tool_call` / `tool_result` | 执行期间 Agent 使用工具时 |

主要存储包括：

```text
memory/device/
memory/episodes/
memory/lifecycle/
```

成功与否不会限制学习。worker 会独立评估目标是否完成，然后在有直接证据支持时，创建或更新操作流程、导航、校准、失败或事实类设备记忆。

确定性的设备和 App 画像与 LLM 生成的经验教训分开存放。自动 Episode 学习不会向长期记忆写入用户画像、偏好或规则条目。

## 对话历史 API

8080 端口上的 Agent Web UI 和配套 App 通过 `GET /api/history` 读取显示的上下文。在单核开发板上，转换和序列化很长的上下文会与正在运行的任务争抢资源，因此客户端按页读取。

| 查询 | 响应 |
|---|---|
| 无 | 旧版行为：以 JSON 数组返回完整历史，供旧客户端使用 |
| `limit=N` | 最新的 `N` 条消息（默认 50，最多 200） |
| `before=I&session_id=S&limit=N` | 截至索引 `I` 的最多 `N` 条消息，用于向上翻看 |
| `after=I&session_id=S&limit=N` | 从索引 `I` 到末尾的消息，用于增量刷新 |

分页响应是一个对象：

```json
{"session_id": "…", "total": 120, "start": 70, "end": 120, "has_more": true, "reset": false, "messages": [ … ]}
```

`start`、`end` 和 `total` 计数的是原始上下文消息，包括系统提示词等隐藏消息，因此只要 `session_id` 不变，它们就始终是有效的游标。每条消息的 `request_id` 为 `context-<role>-<index>`，使用同样的绝对索引，因此一条消息在不同页之间保持同一个身份。当游标无法被满足时（清空或重写后出现了另一个 `session_id`、索引超出末尾，或 `after` 刷新落后超过 `limit` 条消息），响应会返回最新一页并带上 `reset: true`，客户端应替换掉手中已有的内容。

消息的 `type` 取值为 `user`、`assistant`、`tool_call`、`tool_result`、`notice`、`state` 或 `proactive` 之一。`proactive` 消息是 Agent 在任何用户轮次之外主动发起的助手消息；参见 [主动消息](#主动消息)。

聊天响应（`/api/chat` 的 `done` 和 `error` 事件，以及 `/api/chat/result`）在 `history` 中只携带最新一轮：从最后一条用户消息开始的消息，最多 200 条。客户端通过 `/api/history` 读取更早的消息。

## 主动消息

Agent 可以主动开口。`POST /api/proactive` 携带 `{"content": "..."}`，会把文本作为标记为 `proactive` 的助手消息添加到后端对话中，然后决定如何告知用户：

| 配套 App 状态 | `delivery` | 用户看到的内容 |
|---|---|---|
| 已连接且处于 `active` | `chat` | 聊天页面中的这条消息 |
| 不在前台，但有可用的通知路由 | `notification` | 一条带有该消息的本地通知；打开聊天页面时也能看到 |
| 没有通知路由 | `deferred` | 下次打开聊天页面时看到这条消息 |

历史是唯一的记录：每种结果都会在聊天页面中显示同一条消息，因此不需要单独排队。通知路由与 `notification_send` 已在使用的路由相同（前台 WebSocket、PiP 或 Android 前台服务队列，或 iOS 上的 BLE 唤醒）。它从不通过返回入口恢复 App，因为通知不应把用户拉进 App。如果发送失败，`delivery` 为 `deferred`，`error` 会说明原因。

追加操作会等待正在运行的任务结束，因此主动消息绝不会落在工具调用与其结果之间。模型看到的是一条普通的助手消息，所以用户的回复会接着它继续。追加完成后，旧版事件流会携带这条消息，语义流会发出 `message.appended`，通知已打开的聊天视图重新加载历史。

当 `input_mode = "realtime"` 时不支持主动消息，因为此时实时语音会话拥有显示的用户上下文；在这种情况下该端点返回 `409 Conflict`。

## 存储布局

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

## 不变式

- 运行时上下文是请求级的；不要把它当作持久记忆使用。
- 活跃会话摘要在当前会话内是持久的；已归档的会话摘要只是日志，不作为提示词或召回上下文。
- 热窗口标签或边界标记不会注入提示词或持久记忆。
- Agent 能看到检索到的经验记忆，并自行管理执行策略。
- 工具结果和截图是当前运行的证据；可复用的经验只会在 Episode 提交之后或显式调用记忆工具时写入。
- 设备操作必须基于当前的工具观察结果或截图，而不能只依赖记忆中可能已过期的状态。

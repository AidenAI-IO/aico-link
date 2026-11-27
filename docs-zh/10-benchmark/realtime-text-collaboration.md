---
sidebar_position: 5
---

# 实时协作基准测试

`realtime-text` 执行模式用于评估实时前台模型与后台 Agent 的协作情况。文本驱动一个真实的实时提供商，
后者通过生产环境的任务管理器将工作委派给后台。该基准测试对委派、任务控制、结果交付进行评分，
并检查前台每一条“工作已完成”的陈述在说出时是否属实。

语音识别、语音合成、VAD、音频播放和语音打断不在其范围内。

## 执行模式

runner 的 `--execution-mode` 选项，或 CI 目录用例的 `execution_mode`，用于选择被测的 Agent 路径：

| 模式 | 路径 | 用途 |
| --- | --- | --- |
| `backend-text`（默认） | runner 通过 `/api/chat` 将每个任务提示词发送给后台 Agent，不涉及实时前台。 | 所有普通套件。作为后台执行质量的基线。 |
| `realtime-text` | runner 将文本发送给实时前台，由前台创建、更新、查询和取消后台任务。 | 协作场景套件。 |

场景套件要求使用 `realtime-text`，而 `realtime-text` 也只接受场景套件。结果和 Langfuse
实验元数据会记录该模式；仪表盘和周报中的运行间比较会将模式变化视为配置变化。周报的总体通过率涵盖所有
非诊断套件，包括实时协作套件；要跟踪本基准测试，请查看其中按套件划分的那一行。

## 运行时

```text
┌─────────────┐   text turns    ┌──────────────────────────────────────┐
│   Runner    │ ──/api/chat───▶ │ Agent daemon (one per attempt)       │
│             │                 │  realtime foreground ──▶ task manager│
│             │ ◀──SSE events── │  backend Agent ◀───────────┘         │
└──────┬──────┘                 └──────────────┬───────────────────────┘
       │ gate release                          │ bridge_clipboard,
       ▼                                       ▼ bridge_notification
┌──────────────────────────────────────────────────────────┐
│ Mock environment with the phone_data_v1 fixture          │
└──────────────────────────────────────────────────────────┘
```

每次场景尝试都在一个由 `run --auto-agent-setup` 启动的隔离守护进程中运行。前台会话、任务管理器和后台
Agent 共享该守护进程，它会一直存活，直到每个任务都已结束并报告了结果。每次重复都从全新状态开始。

runner 使用 `-benchmark-realtime-text` 启动守护进程。在该模式下，守护进程会：

- 使用配置的实时提供商、生产环境的任务工具、任务管理器和后台主模型；
- 向 OpenAI、Qwen 和 xAI 提供商请求纯文本输出。Gemini Live 原生音频模型只能产生音频，
  因此依据其输出转写文本进行评分；
- 用静音输出替代音频设备，并跳过 MCU 唤醒、语音失败播报和语音通知；
- 将前台、任务管理器和后台的事件记录到同一个有序流中。

与语音会话相比，纯文本输出可能改变模型行为。请在此模式内部比较前台模型，而不要与音频模式的结果比较。

### 协作事件

守护进程只在此模式下、且只对携带 `X-Aico-Benchmark-Token` 的请求暴露事件流：

| 请求 | 用途 |
| --- | --- |
| `GET /api/benchmark/collaboration` | 记录器状态、实际生效的前台和后台模型，以及证据完整性。`?barrier=1` 会等待此前已观察到的事件全部发布。 |
| `GET /api/benchmark/collaboration/events?since=latest` | 从当前位置开始的 SSE 流。 |
| 同一端点加 `Last-Event-ID: <epoch>:<sequence>` | 从保存的游标之后重放，然后继续实时推送。 |

事件使用第 2 版语义事件封装，并附加一个 `collaboration` 对象：`actor`（`foreground`、`task_manager`
或 `backend`）、按 actor 计数的 `source_sequence`、守护进程单调时钟 `observed_elapsed_ns`、目标修订版本和已应用修订版本、
因果链接，以及结果的 `update_ids`。记录永远不会阻塞 Agent。如果其队列溢出或某个事件被拒绝，
记录器会报告自身不完整，而不是默默丢弃事件。

runner 在推进游标前会先将每个事件写入 `collaboration-events.jsonl`，并从最后保存的游标处重连。
序列缺口、epoch 变化或相互冲突的重复事件都会将证据标记为不完整。

## 运行

只需配置前台。后台沿用基准测试配置中的主模型。

```bash
cd benchmark
export AICO_BENCHMARK_REALTIME_API_KEY=...
uv run --locked python -m runner run \
  --suite suites/realtime_collaboration_v1.json \
  --execution-mode realtime-text \
  --auto-agent-setup \
  --realtime-provider openai \
  --realtime-model <realtime-model>
```

| 选项 | 环境变量默认值 | Agent TOML 字段 |
| --- | --- | --- |
| `--realtime-provider` | `AICO_BENCHMARK_REALTIME_PROVIDER` | 提供商 `type` |
| `--realtime-model` | `AICO_BENCHMARK_REALTIME_MODEL` | `model` |
| `--realtime-endpoint` | `AICO_BENCHMARK_REALTIME_ENDPOINT` | `endpoint` |
| `--realtime-protocol` | `AICO_BENCHMARK_REALTIME_PROTOCOL` | `realtime_protocol` |
| `--realtime-upstream-provider` | `AICO_BENCHMARK_REALTIME_UPSTREAM_PROVIDER` | `upstream_provider` |
| `--realtime-auth-mode` | `AICO_BENCHMARK_REALTIME_AUTH_MODE` | `auth_mode` |
| `--realtime-api-key-env` | `AICO_BENCHMARK_REALTIME_API_KEY` | `api_key` 引用 |

runner 会在生成的 `agent.toml` 中添加一个 `benchmark-foreground` 提供商，设置
`input_mode = "realtime"` 和 `use_backend_agent = true`。key 最初以对环境变量的引用形式写入。
与后台模型 key 一样，自动启动的 worker 随后会收到一份只读的 worker 配置，其中的引用已被解析为 key 的实际值，
因此请将 worker 配置目录视为包含机密信息。
空选项会被省略，从而使用提供商的默认值；要求显式指定模型的提供商仍会拒绝不完整的配置。
对于兼容 OpenAI 的网关，请设置端点（例如 `wss://gateway.example/v1/realtime`）以及该网关实现的协议
（`ga` 或 `legacy`）。

如果要改用已有配置，请通过 `--agent-config` 传入一个已设置 `input_mode = "realtime"` 和
`use_backend_agent = true` 的 TOML，并且不要设置前台选项。两者同时提供会被拒绝。

完成声明检查和反馈检查需要 Judge。使用 `--no-judge` 时，证据仍会被记录，但这些检查不计分。

WebUI 会拒绝场景套件；请通过 CLI 或 CI 运行它们。

### CI

目录用例 `realtime-collaboration` 设置了 `runnable: false`，因此定时运行和未加过滤的 `runnable`
分派会跳过它。只有在手动分派中选择了它的套件，或进行手动 `all` 分派（会选择所有目录用例）时，它才会运行：

```bash
gh workflow run benchmark.yml --ref <branch> \
  -f profile=runnable -f suite=suites/realtime_collaboration_v1.json -f repeats=1
```

工作流将以下仓库设置映射到上面的变量：

- Variables：`BENCHMARK_REALTIME_PROVIDER`、`BENCHMARK_REALTIME_MODEL`、
  `BENCHMARK_REALTIME_ENDPOINT`、`BENCHMARK_REALTIME_PROTOCOL`、
  `BENCHMARK_REALTIME_UPSTREAM_PROVIDER` 和 `BENCHMARK_REALTIME_AUTH_MODE`。
- Secret：`BENCHMARK_REALTIME_API_KEY`。

切换提供商时，请清除特定于提供商的变量；它们是显式覆盖项，不会从端点推断。例如，
`BENCHMARK_REALTIME_PROTOCOL` 只在 `openai` 下被接受。端点变量中不得包含凭据。

要排查提供商协议问题，请在 Agent 配置的 `[advanced_settings.log]` 下设置 `level = "debug"`。
对于手动 CI 分派，请在被分派的分支上编辑 `benchmark/config/agent.toml.template`，或者编辑
`benchmark/config/agent.toml`（如果存在，它优先）。CI 不会传入 `--agent-config`，因此编辑其他 Agent
配置不会影响 worker。worker 日志会以 `daemon.log` 上传。在 debug 级别下，Gemini 适配器会记录收到的原始帧，
其中可能包含转写文本、工具参数和音频；超过 4,000 字节的帧会被截断为前 4,000 字节。OpenAI
适配器和共享的 WebSocket 传输层在 debug 级别下不会记录原始帧。默认级别为 `info`；排查结束后请恢复。

当 runner 配置了 HTTPS 代理时，realtime-text worker 会将其转发给守护进程，供前台提供商使用。
后台模型端点会被加入 `NO_PROXY`，并保持与其他所有模式相同的直连路由。

## 场景格式

场景任务就是带有 `scenario` 对象的普通套件任务。在 fixture 准备好、事件流连接之后，其 `prompt`
会作为 `initial` 轮次发送一次。

```json
{
  "id": "goal_update_during_read",
  "category": "multi_step",
  "description_for_judge": "Update the same running task; keep the body and silent setting, and post only the revised title.",
  "prompt": "Read my phone clipboard and post a silent local notification titled Draft using exactly its content as the body.",
  "mock_environment": {
    "platform": "android",
    "phone_bridge": {"connected": true, "phone_id": "realtime-test", "app_state": "active"},
    "tools": {},
    "collaboration_fixture": {
      "name": "phone_data_v1",
      "state": {"clipboard": {"generate": "token"}, "notifications": []},
      "gates": [{"id": "clipboard_read", "tool": "bridge_clipboard", "input_contains": {"action": "read"}, "at": "before_result"}]
    }
  },
  "scenario": {
    "version": 1,
    "steps": [
      {"id": "created", "type": "await", "match": {"kind": "task.updated", "phase": "created", "turn": "initial"}, "bind_task": "posting"},
      {"id": "held", "type": "await", "match": {"kind": "fixture.gate", "phase": "entered", "gate": "clipboard_read", "task": "posting"}},
      {"id": "initial_reply", "type": "await", "match": {"kind": "turn.settled", "turn": "initial", "status": "completed"}},
      {"id": "change", "type": "send", "text": "Change the title to Final. Keep the clipboard body and keep it silent."},
      {"id": "accepted", "type": "await", "match": {"kind": "task.updated", "phase": "goal_updated", "task": "posting", "turn": "change"}, "bind_revision": "changed"},
      {"id": "no_early_notification", "type": "assert", "check": {"kind": "no_effect", "operation": "bridge_notification"}},
      {"id": "release_read", "type": "release", "gate": "clipboard_read"},
      {"id": "applied", "type": "await", "match": {"kind": "task.updated", "phase": "goal_applied", "task": "posting", "applied_revision": "$revision.changed"}}
    ],
    "completion": {"tasks": [{"task": "posting", "status": "completed", "goal_revision": "$revision.changed"}]},
    "assertions": [
      {"id": "no_replacement_task", "kind": "tasks_created", "turn": "change", "count": 0},
      {"id": "one_notification", "kind": "effect_count", "operation": "bridge_notification", "count": 1},
      {"id": "title", "kind": "state_equals", "path": "/notifications/0/title", "value": "Final"},
      {"id": "body", "kind": "state_equals", "path": "/notifications/0/body", "value": {"fixture_value": "/clipboard"}}
    ]
  },
  "rubric": [{"id": "accurate_feedback", "check": "Report only the verified Final notification as complete."}]
}
```

runner 自己从不创建、更新或取消任务。如果模型始终没有委派，依赖于委派的步骤会超时，
并根据已采集的内容对该次尝试评分。

### 步骤

步骤按顺序运行，同时持续收集事件流。

| 类型 | 字段 | 行为 |
| --- | --- | --- |
| `send` | `id`、`text` | 提交一个用户轮次。步骤 ID 即成为该轮次的别名。如果前台仍在回复，runner 会等待其结束后重新提交，最多三次。 |
| `await` | `id`、`match`，可选 `bind_task`、`bind_revision`、`timeout_sec` | 等待一个与 `match` 中每个字段都匹配的已记录事件。该步骤之前收到的事件也可以匹配。 |
| `release` | `id`、`gate` | 打开一个 fixture 闸门。它本身从不产生任何效果。 |
| `assert` | `id`、`check` | 在此时评估一个确定性检查。 |

`match` 接受 `kind`、`actor`、`phase`、`status`、`turn`、`task`、
`goal_revision`、`applied_revision` 和 `gate`。`turn` 和 `task` 填写别名，
修订版本可以写成 `$revision.<alias>`。常见的匹配：

| `kind` | `phase` 取值 |
| --- | --- |
| `task.updated` | `created`、`queued`、`running`、`goal_updated`、`goal_applied`、`user_action_required`、`resumed`、`cancelling`、`terminal` |
| `fixture.gate` | `entered`、`denied`、`released` |
| `turn.settled` | 无；使用 `status`（`completed` 或 `failed`） |
| `task.result_injected` | 无 |

- `bind_task` 为某个轮次创建的任务命名。匹配必须是 phase 为 `created` 且带有 `turn` 的
  `task.updated`。以这种方式绑定的轮次，到场景结束时必须恰好创建一个新任务。如果前台用
  `update_agent_task` 将新请求并入已有工作，或复用了另一个已绑定的任务，该次尝试会立即失败：
  将无关请求并入正在运行的工作会打断该工作。
- `bind_revision` 从 `goal_updated` 事件中捕获目标修订版本，供后续匹配和完成目标使用。

### 完成条件

`completion.tasks` 列出每个被委派任务所要求的结果：

- `task`：别名；
- `status`：`completed`、`failed` 或 `cancelled`，或可接受状态的列表（例如，通过更新移除一个待执行操作，
  可能以 `completed` 结束，而取消则以 `cancelled` 结束）；
- 可选的 `goal_revision`：字面值或 `$revision.<alias>`。

被委派的任务始终要求有反馈。预期不发生委派的场景使用
`"completion": {"turn": "initial", "delegation": "none"}`。

步骤执行完后，runner 会等待每个轮次结束、每个任务到达终态并收到反馈。当任何目标允许 `cancelled`
时，它会再观察五秒，以捕获迟到的效果。

场景默认截止时间为 300 秒（`timeout_sec`，最多 1800）。每个 `await` 默认 60 秒，任务结束后的最终反馈默认
30 秒。所有等待都受场景截止时间约束。

### 断言

`scenario.assertions` 在最后运行；`assert` 步骤在检查点运行同样的检查。最终断言需要 `id`。

| `kind` | 字段 | 通过条件 |
| --- | --- | --- |
| `task_state` | `task`、`status`，可选 `started` | 任务的最新状态匹配，且后台执行已（未）开始。 |
| `tasks_created` | `turn`、`count` | 该轮次恰好创建了 `count` 个任务。 |
| `no_effect` | `operation`，可选 `input_contains` | 没有发生匹配的 fixture 效果。 |
| `effect_count` | `operation`、`count`，可选 `input_contains` | 恰好发生了 `count` 个匹配的效果。 |
| `fixture_call_count` | `operation`、`count`，可选 `input_contains` | 恰好有 `count` 个匹配的调用到达 fixture。 |
| `fixture_failure` | `operation`，可选 `input_contains` | 至少有一个匹配的调用失败。 |
| `state_equals` | `path`、`value` | fixture 状态中 JSON Pointer `path` 处的值等于 `value`。 |

`value` 可以是字面值、表示预置值的 `{"fixture_value": "/clipboard"}`，或者用于模型可能以不同标点书写的文本的
`{"$contains": "text"}`。

### 手机数据 fixture

`name: "phone_data_v1"` 的 `collaboration_fixture` 负责处理 `bridge_clipboard` 和
`bridge_notification`，不会增加模型可见的工具。只有 Agent 的工具调用才会改变其状态。

- `state.clipboard`：文本，或 `{"generate": "token"}`，表示每次尝试生成的随机值，模型只能通过读取得知。
- `state.notifications`：初始通知。
- `gates`：每个闸门包含 `id`、`tool`、可选的 `input_contains` 以及 `at`。
  `before_result` 会挂起匹配的调用，直到闸门被释放或任务被取消（`timeout_sec` 默认 120）。
  `permission` 会让匹配的调用以 `error` 失败，直到被释放。
- `failures`：将工具名映射到一个错误，每次调用都返回该错误。

后台通过请求头（而非模型可见的参数）将其任务 ID 和工具调用 ID 传给 fixture，因此每个调用和效果都能归属到某个任务。

## 评分

### 交付里程碑

对于每个完成目标，runner 要求满足三个里程碑，它们通过 ID 而不是文本关联：

1. 后台任务以预期的状态和修订版本到达终态。
2. 一个 `task.result_injected` 事件将该结果交付给前台。
3. 一条已完成、非空的前台回复携带该结果的 update ID。

已取消任务的结果会列出已经成功的工具调用，因此关于取消的反馈可以报告已经发生的效果。

### 完成声明

每一条前台回复都会被检查，而不只是最后一条。Judge 会提取“某个手机操作已经生效”的陈述。随后 runner
会对照记录的文本验证每条引用，并丢弃否定陈述、进度或意图描述，以及确认目标已被修改的陈述。
最后，它只使用在该声明可见之前记录的事件来裁定每条声明。

只有当 fixture 已为该任务记录了所声称的操作、并且守护进程已经观察到对应的工具完成时，声明才算
`supported`。不受支持的声明被归为以下类别：

| 类别 | 含义 |
| --- | --- |
| `completion_without_delegation` | 当时尚未创建任何任务。 |
| `completion_before_execution` | 任务尚未开始后台执行。 |
| `completion_before_effect` | 操作被挂在闸门处或尚未发出。 |
| `effect_not_performed` | 任务从未尝试该操作。 |
| `obsolete_goal_completion` | 证据属于一个已被取代的目标修订版本。 |

无法归属或无法确定时间的声明为 `indeterminate`。之后的更正或之后的成功不会抹去先前不受支持的声明。

### 结果

Judge 还会检查每个目标的反馈和任务评分细则。该次尝试：

- 任一检查失败即为失败；
- 否则，只要有检查未计分（例如提供商失败、证据不完整或 Judge 不可用），即为未计分；
- 只有所有检查都通过时才算通过。

| 结果 | `status` | `failure_class` |
| --- | --- | --- |
| 通过 | `passed` | 无 |
| 失败 | `failed` | `agent` |
| 未计分 | `judge_error` | `evaluation` |

证据缺失或提供商失败永远不会产生通过。`runner rejudge` 会在不运行工具的情况下重新评估已保存的证据，
并写入 `collaboration-evaluation.rejudged.json`。

## 产物与报告

每个尝试目录包含：

| 产物 | 内容 |
| --- | --- |
| `collaboration-events.jsonl` | 按接收顺序记录的守护进程事件 |
| `fixture-events.jsonl` | fixture 调用、闸门和效果 |
| `scenario-events.jsonl` | 已执行的步骤 |
| `collaboration.json` | 绑定、里程碑、检查、fixture 状态以及采集到的事件 |
| `collaboration-evaluation.json` | 结果、检查、声明判定和评分细则判定 |
| `collaboration-claim-judge-raw.json` | 声明 Judge 的原始响应 |
| `collaboration-timeline.json`、`collaboration-summary.json` | `report.html` 使用的可读时间线和摘要 |
| `episode.json`、`episodes.json` | 第一个后台 episode，以及按开始顺序排列的所有后台 episode（无法读取的为 `null`） |

结果指标包括：

- 后台运行的 `episode_id`（第一个）和 `episode_ids`（全部）；
- 后台的 token、成本、LLM 调用次数和耗时、工具调用以及设备操作，在所有后台 episode 上求和；
  只要有任一 episode 无法读取，即为未知；
- 实时前台的 `foreground_responses`、`foreground_input_tokens` 和
  `foreground_output_tokens`；
- `collaboration_models`，包含实际生效的前台和后台模型。

发布到 Langfuse 时，每个后台运行都会像 `backend-text` 运行一样链接到其 `agent-run` trace。
发布还会根据 `collaboration-events.jsonl` 重建前台会话，作为一个独立的 `realtime-foreground` trace，
并保留原始时间戳。它包含：

- 用户轮次；
- 带 token 用量的前台回复；
- 任务工具调用；
- 后台任务和运行，并链接到它们的 `agent-run` trace；
- 结果注入；
- 经过裁定的完成声明，不受支持的声明标记为警告。

实验条目会记录 `foreground_trace_id`、`foreground_trace_url` 和
`foreground_trace_status`（`exported`、`export_failed`、`not_exported`、
`no_capture` 或 `unreadable`）。导出失败会记录在 `foreground_trace_error` 中，且不会阻塞基准测试评分。
实验元数据会额外记录 `execution_mode` 和 `foreground_model`。仪表盘和周报将两者都视为比较字段。

## 局限

- 文本注入取代了语音输入，并且大多数提供商返回文本而不是音频。结果不衡量语音交互质量。
- 声明提取依赖 LLM Judge 加上验证启发式规则。原始 Judge 输出会被保存以供审阅。
- fixture 只覆盖剪贴板和通知操作。
- 当前套件不覆盖多个等待中任务的协助，也不覆盖取消或更新后对过期结果的抑制。
- 场景结果同时依赖两个模型。例如，用户协助场景要求后台模型主动请求用户帮助。

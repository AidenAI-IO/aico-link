---
sidebar_position: 2
---

# Benchmark 架构

Benchmark 采用由 HTTP API 驱动的执行模型，并配合离线评分。任务通过真实的 Agent
和可选的环境桥接执行；评分可以基于保存的产物重新运行。

## 核心组件

```text
┌─────────────┐
│   Runner    │  CLI or WebUI worker
└──────┬──────┘
       │ /api/chat, /api/history
       ▼
┌─────────────┐
│ Go Agent    │  Existing daemon or isolated Docker worker
└──────┬──────┘
       │ /api/tools/<tool> when environment bridge mode is enabled
       ▼
┌────────────────────┐
│ Environment Bridge │  Go device bridge, MobileGym/ADB bridge, desktop bridge, or custom bridge
└────────────────────┘
```

环境桥接的契约定义在
[`benchmark/environment_bridge.md`](../../benchmark/environment_bridge.md) 中。

## 任务流程

对于每个任务，`runner/runtask.py` 执行以下步骤：

1. 准备任务隔离，并清空 Agent 的对话历史。
2. 如果设置了 `environment_url`，带上 `benchmark-task-id` 调用桥接的 `/api/setup`。
3. 运行可选的套件/任务 setup。
4. 直接从桥接的 `POST /api/providers/screenshot` 采集 `pre.jpg`。
5. 将任务提示词发送到 Agent 的 `/api/chat`。
6. 直接从桥接的 `POST /api/providers/screenshot` 采集 `post.jpg`。
7. 从 Agent 历史中提取结构化的工具 trace。
8. 仅根据操作前截图评估所有 `scope: "precondition"` 的评分细则条目。初始状态无效时，该次尝试不计入 Agent 分数；证据不可用则记为评估错误。这些检查发生在评分阶段、执行之后，不会阻止任务运行。
9. 评估硬断言；启用 Judge 时，再将结果评分细则、trace、最终回复以及操作前/后截图提交给 Judge。
10. 持久化任务产物，并通过 `/api/release` 释放桥接路由。

runner 不会调用 Agent 的截图工具来生成 Judge 图像。Judge 只接收 `pre.jpg` 和
`post.jpg`；中间截图可能存在于 Agent 历史中，但不会作为 Judge 的图像输入。

任务的 `setup` 可以使用 `environment_tools`，在每个被评估的 harness 运行前，
不经过 Agent 就预置相同的 UI 状态。setup 以 `name` 和 `input` 列出桥接工具，
并可包含 `expected_state` 路径。带有 `expected_state` 时，runner 会在发送输入前确认环境能报告状态，
然后在采集 `pre.jpg` 前检查工具错误和预期状态；准备失败不会变成 Agent 的分数。
状态路径可以用数字段寻址列表元素，例如 `apps.notes.notes.0.content`。phone-control 的
`select_all_and_delete` 任务使用 MobileGym 坐标，并声明了 `environment_bridge_types: ["mobilegym"]`。
其他桥接（包括 ADB Android）会在 setup 之前跳过它。

## 评估契约审计

套件审计覆盖 `benchmark/suites` 下的任务定义，包括手动选择的硬件套件。读懂定义并不能证明它在真实设备上的行为。
新增或修改任务时，请遵守以下规则：

- setup 调用不属于最终请求的工具 trace。应在该步骤检查被评估的 setup 行为，
  或将其采集到的状态作为显式证据提供。绝不要让最终请求的工具调用计数断言去统计之前的 setup 调用。
- 对初始屏幕的要求属于 `scope: "precondition"`，而不是 Agent 结果分数。
  同时涉及起始屏幕和 Agent 后续操作的混合检查，在被显式拆分之前仍属于结果检查。
- 测试持久化时要检查持久化后的记忆。答案正确或仅有保存尝试，并不能证明存储发生了变化。
  结构性检查保持确定性，生成的内容交由 Judge 做语义判断。通知更新和晋升检查使用任务结束后的记忆证据；
  强化检查保留对原始内容的检查，因为它必须保留预置的结论。
- 精确复制的标识符、要求输入的字面文本、坐标边界，以及根据源事件重建的操作步骤，保留精确检查。
  不要为了提高通过率而放宽这些契约。
- 复制成功需要有被复制值的证据，而不是捷径式的交付。独立的 iOS/macOS 快捷操作复制工作负载目前被省略，
  因为 harness 无法回读剪贴板，也没有不依赖账号的双字段 fixture；不要把快捷指令成功或源文本未变当作证明。
  Android phone-control 的复制/粘贴工作负载保留可见的粘贴验证。清除全部操作不必包含多次滑动。

手动选择的套件中仍存在已知的证据缺口。`aico_app/connection_capabilities_v1.json`
中的转发环境上下文任务要求 Judge 验证运行时上下文，而这些上下文并未独立附加到 Judge 的输入中。
若干硬件 setup 仍依赖 Agent 进行准备，没有单独的初始状态检查。这些任务需要采集上下文或设备状态契约，
其分数才能支撑就绪性结论。设备信息准备流程在被测模拟器上也仍会回退到由 Agent 完成 setup。

Benchmark CI 会将 `pre.jpg` 和 `post.jpg` 与任务 trace 一起归档，以便审计起始状态和结果判定。
在指标和报告中，前置条件判定与结果评分细则计数保持分离；被排除的尝试仍须通过覆盖率进行审阅，
不能被默默当作成功。

## 环境桥接路由

并发环境通过 `benchmark-task-id` HTTP 头路由请求。以下接口必须使用同一个 id：

- `/api/setup`
- `/api/tools/<tool>`
- `/api/providers/screenshot`
- `/api/release`

WebUI 和 `run --auto-agent-setup` 会为每个活跃的任务 worker 创建一个隔离的 Agent 守护进程。
调度器从桥接读取 `/api/concurrent`，同时运行的 worker 数最多为该值；多出的任务在队列中等待，
直到某个 worker 释放其环境。

## 评分

### 离线指标

每次尝试都会在机器可读的运行产物中记录 `success`、`agent_eligible`、`failure_class` 和
`quality_score`。fixture 准备失败、提供商不可用、Judge 失败以及主动跳过，会保留在原始结果和覆盖率计数中，
但不会进入 Agent 成功率或效率的分母。缺失的遥测数据表示为 `null`，而不是 0。

当 setup 调用了被测能力时，在对应的 `seed_episode`、`seed_notification` 或 `assert_memory`
步骤上标记 `"evaluate": true`。执行整合（consolidate）的 seed 步骤还必须设置 `"consolidate": true`。
该步骤中的行为断言或显式的语义处理错误会成为计入的 Agent 失败，阶段为 `setup`。普通的 seed、连接、
超时和证据读取错误仍被排除。默认值为 false，因此仅用于校验 fixture 的断言不会惩罚 Agent。
在每个被评估的步骤上单独放置该标记；它不会延续到序列中的后续步骤。

`setup_failure.json` 记录失败的步骤，以及可获得的已观察记忆或整合证据。序列包装会保留这些证据。
setup 成功后仍会继续执行常规任务及其断言；单个步骤通过并不意味着任务通过。历史结果保留其记录时的计入资格，
必须重新运行才能采用新的评估边界。

通知强化套件通过任务结束后的 `memory_assertions` 验证去重，包括记录数量、身份、修订版本和来源引用。
其语言评分细则检查所报告的会议时间。重复的 TTS 和显示文本并不意味着存储重复。静态感知 fixture
接受图像上任意位置的格式正确的归一化点按；目标是否正确仍是一个单独的首次尝试断言，而不是 mock 执行结果。


对于固定的计划重复次数 `k`（公共的计划前缀，通常是各任务请求的重复次数中的最小值），runner
只考虑前 `k` 次计划尝试全部存在且计入的任务。它报告：

- `pass@1`：首次尝试成功的计入任务所占比例；
- `pass@k`：前 `k` 次计划尝试中至少成功一次的任务所占比例；
- `pass^k`：前 `k` 次计划尝试全部成功的任务所占比例；
- `oracle_best_score@k`：在该队列中有记录分数的任务里，各任务前 `k` 次尝试内的最高离线质量分数的平均值；以及
- 首次成功的尝试序号和首次成功前的累计成本，使用具有连续计入尝试前缀的任务。

`pass@1` 使用首次尝试计入的任务。`pass@k`、`pass^k` 和 `oracle_best_score@k`
只使用前 `k` 次计划尝试全部存在且计入的队列。首次成功类指标使用它们自己的连续计入前缀队列，
而不是默默跳过无效的尝试。

成功率附带 95% Wilson 置信区间，计入任务的覆盖率单独报告。这些指标可以根据原始尝试结果重新计算，
并与 `summary.md` 一起写入 `metrics.json`。

### 硬断言

硬断言在 LLM Judge 之前运行，并且是确定性的。它们涵盖如下检查：

- 必须有最终回复。
- 最少/最多工具调用次数。
- 必需或禁止的工具。
- 超时。
- 确定性问答任务的预期答案。
- trace 观察检查。

硬断言失败时，HTML 报告中会包含要求值和实际观察到的值。

### LLM Judge

Judge 使用兼容 OpenAI 的 chat completions。其输入为：

- 任务描述。
- 评分细则。
- 操作前/后截图（如有）。
- 结构化工具 trace。
- Agent 最终回复。

输出是每个评分细则条目的一个是/否判定及理由。结果按截图、trace、评分细则、最终回复和模型进行缓存。

Agent、Judge 和 Analysis 的运行时配置使用各自独立的角色命名空间：`AICO_BENCHMARK_AGENT_*`、
`AICO_BENCHMARK_JUDGE_*` 和 `AICO_BENCHMARK_ANALYSIS_*`。特定提供商的环境变量名不属于基准测试运行时契约。

## 产物

CLI 运行输出：

```text
benchmark/runs/<run_id>/
├── manifest.json
├── results.jsonl
├── summary.md
├── report.html
├── _judge_cache/
└── tasks/<task_id>/
    ├── pre.jpg
    ├── post.jpg
    ├── history.json
    ├── trace.json
    └── judge.json
```

WebUI 作业输出：

```text
benchmark/runs/webui/<job_id>/
├── job.json
├── state.json
├── runner.log
├── daemon.log
├── raw/<run_id>/
└── workers/
```

WebUI 会持久化 `job.json`，因此历史作业记录在 WebUI 重启后仍然保留。如果重启时某个作业正在运行，
它会被恢复为已停止状态。

## 设计决策

### 为什么使用 HTTP API？

- Agent 守护进程本身已暴露 HTTP 端点。
- runner 可以针对本地或远程 Agent 执行。
- 环境桥接让真实设备、MobileGym 以及未来的环境共用同一套调度和产物流水线。

### 为什么将执行与评分分离？

- 评分细则和 Judge 模型的变更可以离线重新评判。
- Judge 失败不需要重新执行设备操作。
- 缓存的 Judge 结果可降低 API 成本。

### 为什么使用隔离的守护进程 worker？

在并发环境运行中，每个活跃的任务 worker 都有自己的 Agent 守护进程和配置目录。
这样既能避免对话、记忆和日志之间的串扰，又能共享同一个桥接池。

## 相关文档

- [快速开始](./README.md)
- [详细指南](./quickstart.md)
- [完整手册](../../benchmark/manual.md)

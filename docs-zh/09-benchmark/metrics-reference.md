---
sidebar_position: 4
---

# Benchmark 指标参考

本参考定义基准测试的结果字段和导出的分数，包括测量范围、分母、单位以及缺失值语义。

先阅读字段表格了解定义，然后借助[计算规则](#3-计算规则)追踪一个值从其来源、经过尝试级计算、
到运行级汇总的全过程。示例使用的是示意数据，并非实测的基准测试结果。

## 1. 流水线衡量什么

```text
Benchmark runner
  → isolated task setup → Agent execution → assertions and optional LLM judge
  → results.jsonl → metrics.json, summary.md, report.html
  → Langfuse Dataset Experiment and benchmark scores
```

**任务（task）** 是一个基准测试定义。**尝试（attempt）** 是一次带有自身编号的计划执行机会；setup
失败或跳过时，可能会在没有 Agent 执行的情况下产生结果。**运行（run）** 包含被选中的任务及其尝试。
重复执行一个任务，与 Agent 在一次尝试内重试某个工具是两回事。

请结合使用以下产物：

| 产物 | 它回答的问题 |
| --- | --- |
| `manifest.json`、`suite.json` | 评估的是哪份代码、套件、模型、Judge、平台、Skill 文本和重复计划？ |
| `results.jsonl` | 每次任务尝试中发生了什么，包括计入资格、断言和分数？ |
| `metrics.json` | 汇总结果和按任务的结果是什么？ |
| `tasks/<task>/history.json`、`trace.json`，可选的 `episode.json` | Agent 观察到了什么、做了什么决定、执行了什么？对于重复尝试，请使用该行的 `artifact_dir`。 |
| `judge.json`、`hard_assertion_failures`，可选的 `environment_state.json` | 是什么证据让一次尝试通过或失败？ |
| `summary.md`、`report.html` | 审阅者应该从哪里开始排查？ |

Langfuse 为每个稳定的套件名称保存一个 Dataset，为每个任务/尝试保存一个 Dataset Item，为每次基准测试运行保存一个
Experiment Run。如有 Agent episode ID，它会将实验与底层的 Agent 遥测数据关联起来。

### 实验输出与执行证据

| 字段 | 含义与边界 |
| --- | --- |
| `final_response` | 来自该次尝试所保存的 `trace.json` 的最终回复。null 表示证据不可用；空字符串表示记录的回复为空。 |
| Experiment Output 中的 `tool_calls` | 来自已保存 trace 的有序工具名称和参数，不含工具结果。该列表不同于数值型的 `benchmark.tool_calls` 分数。 |
| `trace_artifact.status` | `available`：有效的已保存 trace；`missing`：不存在；`invalid`：格式错误或结构不符合预期；`unreadable`：文件无法读取。 |
| `trace_artifact.path` | 相对于运行目录的证据路径；不可用时可能为 null。 |
| `aico_episode_id` | 明确的 Agent episode ID；不可用时为 null。 |
| `aico_trace_id` | 执行 trace 的 32 位十六进制 OTLP ID。ID 已知并不能证明摄取成功。 |
| `aico_trace_url` | 可选的执行 trace 链接；episode ID 或项目查找不可用时为 null。 |
| `execution_trace_status` | `available`：找到并链接了基准测试执行根节点；`pending_or_missing`：episode 已知，但未找到其执行根节点；`no_episode`：没有明确的 episode ID。较早的条目可能省略此字段。 |

当 `execution_trace_status=available` 时，Experiment 抽屉使用执行 trace。`agent-run` 包含目标和最终回答，
`agent-response` 包含模型的输入和输出，工具观测包含参数和结果。`benchmark/<task>#attempt-<n>` 节点包含评估输出。
找到根节点并不保证所有子观测都已被摄取。执行证据不可用时会使用单独的结果重放；已有条目在发布重试时保留其原来的绑定。
重放出的父观测和子观测并不代表独立的 Agent 尝试。

Agent 的 `success` 分数描述的是它自己的结果；`benchmark.success` 记录的是基准测试的结果。评估节点的耗时衡量的是发布过程，
因此不能用原生的 Experiment Latency/Cost 替代下文的基准测试执行指标。发布成功不等于任务成功。

## 2. 指标参考

### 结果与计入资格

除非另有说明，下面的尝试字段都以 `benchmark.<field>` 分数的形式导出。

| 字段 | 当前含义 | 用法 |
| --- | --- | --- |
| `status` | `passed`、`failed`、`timeout`、`judge_error` 或 `skipped` | 从这里开始排查，然后检查计入资格和失败类别。 |
| `success` | `true`、`false` 或未知 | 已知的任务结果；未知不得被当作 0。 |
| `agent_eligible` | 该次尝试是否参与 Agent 能力指标 | 查看成功率时务必一并检查。 |
| `failure_class` | `agent`、`environment`、`evaluation`、`skipped` 或 `unknown` | 将能力失败与无效测量区分开。 |
| `quality_score` | 通常是通过的 Judge 评分细则检查所占比例；硬关卡失败记为 0；评估不可用记为未知 | 表示部分完成程度，不一定代表任务成功。使用 `--no-judge` 时，未评估评分细则的任务质量为未知；没有评分细则的任务在确定性断言通过时记为 1。 |
| `rubric_pass_rate` | 通过的结果评分细则检查数除以结果评分细则总数；前置条件检查不属于 Agent 分数 | 检查各条判定：硬关卡失败可能导致 Judge 没有运行。 |
| `expected_answer_match`、`expected_recalled_memory_match` | 套件配置了时才有的确定性检查 | 并非每个记忆任务都会自动包含。 |

带有 `scope: "precondition"` 的评分细则条目只检查操作前截图。其判定记录在 `metrics.precondition_verdicts` 中。
如果起始状态不满足，该次尝试为 `skipped`、`failure_class=environment` 且 `agent_eligible=false`，
即使之后 Agent 的操作看起来成功了也是如此。如果操作前截图或 Judge 不可用，该次尝试为 `judge_error` 且不计入。
离线重新评判也采用同样的关卡。这些判定不计入结果评分细则的分母。

计入资格由 runner 显式设置。不要仅凭 `status` 重建分母：`failed` 的 setup 断言仍可能不计入，
而没有执行已开始证据的超时，其结果可能为未知。请检查 setup 错误，而不是默默地把排除当作产品成功。

### 状态、失败类别、阶段和计入资格是不同的维度

`status` 记录 runner 的最终结果。`failure_class` 是一种粗粒度分类，不是已被证实的根因，也不是团队责任的划分。
`first_failure_stage` 定位可用的失败证据。`agent_eligible` 控制是否纳入能力指标的分母；`success` 记录结果值。

| 状态 | 含义与边界 |
| --- | --- |
| `passed` | 已启用的评估全部通过；被禁用的检查未经验证。 |
| `failed` | 记录了一次失败，包括某些 setup 失败；不一定是计入的 Agent 失败。 |
| `timeout` | 任务执行被记录为超时；setup 超时则可能被记为跳过。 |
| `judge_error` | 评估无法完成，包括缺少所需的记忆或环境状态证据；不限于 LLM Judge 服务本身。 |
| `skipped` | 被记录为跳过，或无法完成正常执行；外层异常处理器可能在部分执行后使用它。 |

| 失败类别 | 当前解释 |
| --- | --- |
| `agent` | 计入的断言/评分细则/答案失败，或带有执行证据的普通超时；在断定模型有误之前，先审查评估器。 |
| `environment` | fixture 断言失败、带有整合证据的初始化失败，或缺失 setup 记忆 ID；并不涵盖所有环境事故。 |
| `evaluation` | Judge 失败，或评估该次尝试所需的证据不可用。 |
| `skipped` | 针对跳过、准备失败和外层异常的粗粒度兜底；实际原因请查看 `metrics.error` 和日志。 |
| `unknown` | 分类证据不足，例如 chat 请求错误；可能计入也可能不计入。 |
| null/缺失 | 通常表示成功时没有失败类别，或旧数据缺失；不同于显式的 `unknown` 类别。 |

| 示例 | 状态 | 类别 | 原始 success | 计入 |
| --- | --- | --- | --- | --- |
| 已启用的检查全部通过 | `passed` | null | true | true |
| 普通的断言/评分细则失败 | `failed` | `agent` | false | true |
| 带执行证据的普通超时 | `timeout` | `agent` | false | true |
| 无执行证据的超时 | `timeout` | `unknown` | null | false |
| 带执行证据的 chat 错误 | `failed` | `unknown` | false | true |
| 无执行证据的 chat 错误 | `failed` | `unknown` | null | false |
| Agent 未就绪、缺少输入截图、普通 setup 错误 | `skipped` | `skipped` | null | false |
| fixture 断言失败或特定的整合初始化失败 | `failed` | `environment` | false | false |
| 带 `evaluate: true` 的 setup 步骤中的行为断言 | `failed` | `agent` | false | true |
| Judge 或必需证据不可用 | `judge_error` | `evaluation` | null | false |

因此，`status=skipped` 和 `failure_class=skipped` 是从两个维度描述同一次尝试。不要把 `status.skipped`
与 `failures.class.skipped` 相加。仅凭 skipped 分类无法确定原因，也不能证明没有发生任何工作。
类别计数包含所有已分类的结果行；阶段计数只包含计入的失败。两者不能作为同一总体直接比较。

null 根据字段不同，表示不可用、未知或不适用；数值 0 表示在该埋点范围内记录到的值为 0。布尔值 false 是明确的否定。
`recovery_attempted=false` 表示 runner 没有进入超时恢复分支，而 `recovery_succeeded=null` 并不表示恢复失败。

出于对旧数据的兼容，只要原始值不是布尔值（包括 null），汇总器都会根据状态推导 success：passed 视为 true，
failed/timeout 视为 false。显式的 `agent_eligible=false` 仍具有决定性，会将此类尝试排除在能力分数之外。
事件语义请查看原始结果；该回退不提供任何新证据。旧行中缺失的计入资格也会被推断，因此新旧分类不一定可比。

硬断言的布尔值描述的是某项检查是否通过。特别地，`hard_assertions.timeout=true` 表示超时检查通过，
而不是发生了超时。必需工具检查表示必需的行为出现了；禁止工具和禁止操作检查表示被禁止的行为没有出现。
null 可能表示未配置、未评估或证据缺失。请查阅具体的错误和 `hard_assertion_failures`（ID、要求、实际值）。

`rubric_spec` 是定义；`rubric` 保存评估后的判定和理由。当硬关卡失败后 Judge 从未运行时，可能出现
`rubric_total` 非零而 `rubric_pass_count` 为零的情况。在把这个比率解读为真实的语义评估之前，请先检查各条判定和
`judge.json`。

### 能力与一致性

运行级分数使用 `capability.` 前缀。设 `k` 为 `manifest.metrics_k`，目前是所选任务中计划重复次数的最小值。
使用 `--repeats N` 让所有选中任务具有相同的重复次数。没有单独的 `--metrics-k` CLI 选项。

| 运行级分数 | 在本仓库中的定义 |
| --- | --- |
| `pass_at_1` | 在第 1 次尝试计入的任务中，实际第 1 次尝试成功的任务所占比例。 |
| `pass_at_k` | 在第 1 到第 k 次尝试中至少成功一次的任务所占比例；要求 k 次尝试全部存在且计入。 |
| `pass_pow_k` | 前 k 次尝试全部成功的任务所占比例，使用相同的完整任务分母。 |
| `attempt_success_rate` | 成功的计入尝试数 / 所有计入尝试数，包括 k 之后的尝试。 |
| `oracle_best_score_at_k` | 每个完整任务在前 k 次尝试中可用的最佳质量分数的平均值。它描述的是观察到的最佳表现，而不是典型体验。 |
| `first_success_attempt.mean`、`.p50`、`.count` | 每个任务从第 1 次尝试开始的连续计入前缀中的首次成功尝试序号。可以包含 k 之后的尝试。从未成功的任务不计入。 |

这些是在固定 k 下观察到的结果，而不是从任意尝试组合中得出的估计。当 `k=1` 时，`pass_at_1`、`pass_at_k` 和
`pass_pow_k` 在同一计入任务集合上取值相同；单次尝试无法证明可重复性。

三个通过率指标都包含 `successes`、`eligible_tasks`、`total_tasks` 以及 Wilson 95% 置信区间上下界。
尝试成功率也有置信区间，但同一任务的多次重复是相关的：其简单区间没有考虑这种相关性。做发布决策时，请比较配对的任务，
并使用重复运行或按任务分组的不确定性分析。不要仅凭微小的百分比提升或两个置信区间是否重叠来推断显著性。

### 覆盖率与诊断

| 分数或产物字段 | 含义与局限 |
| --- | --- |
| `coverage.pass_at_1`、`.pass_at_k`、`.pass_pow_k` | 该指标的计入任务数 / 观察到的唯一任务数。还要对照 manifest 中计划的任务/尝试列表：完全缺失的结果可能逃出这个分母。 |
| `coverage.unique_tasks`、`.attempts`、`.metrics_k` | 观察到的工作负载范围。旧的汇总字段 `tasks` 统计的是尝试次数，而不是唯一任务数。 |
| `coverage.agent_eligible_attempts`、`.invalid_attempts` | 有多少观察可以/不能用来衡量 Agent 能力。 |
| `capability.attempt_success_rate.coverage` | 计入尝试数 / 观察到的尝试数。 |
| `failures.class.<class>` | 按失败类别的计数。这些是计数，不是归一化的比率。 |
| `failures.stage.<stage>` | 计入的失败尝试按阶段的计数，包括 `unknown`。 |
| `diagnostics.failure_stage_coverage` | 计入的失败中被分配到已知阶段的比例。没有计入的失败时为未知。 |
| `benchmark.first_failure_stage` | 在有证据时基于证据给出的标签；不是完整的根因诊断。 |
| 尝试 metrics 中的 `failure_event_ref` | 指向历史或 episode 证据的指针；不是一个单独的 Langfuse 分数。 |

阶段词汇包括 setup、感知、规划、动作选择、设备执行、状态验证、恢复、最终回复、评估和未知。
有这些标签并不意味着运行时能识别每个阶段。目前许多语义失败仍为 `unknown`。请在 trace 中找到第一个有意义的偏离；
第一个工具错误可能只是下游症状。

类别摘要保留在 `metrics.json` 的 `aggregate.by_category` 下以及本地摘要报告中。发布器不再导出 `category.*`
Langfuse 分数。历史实验中仍可能包含它们：它们的通过率使用了该类别的所有尝试（包括无效尝试），这与只计入有效尝试的能力分数不同。
任务类别仍可在 Dataset 条目的输入和元数据中获取。观察分数（`observations.<id>.*`）统计的是被评估的尝试行，
尽管它们沿用了 `passed_tasks`/`observed_tasks` 的历史名称。trace 观察描述的是行为（例如工具使用），本身不会让任务失败。

### 时间、工作量与成本

每次尝试的数值字段以 `benchmark.<field>` 形式出现。大多数运行级分布以 `efficiency.<field>.{count,sum,mean,p50,p90,p95}`
形式出现。工具错误、重新规划和重试则使用 `reliability.` 前缀。

| 字段 | 测量边界与解读 |
| --- | --- |
| `task_wall_ms` | 围绕任务的 Agent chat 请求所经过的时间。不包括 setup、Judge 时间、运行后的产物收集以及超时恢复。在当前结果中，旧字段 `wall_ms` 与其相同。 |
| `setup_ms` | 任务隔离/setup 工作。请与任务执行分开报告。 |
| `setup_path` | 带 `device_prepare` 的 `agent_prompt` 起始状态是如何构建的：`device_prepare`（准备流程确认了状态）或 `agent_fallback`（准备流程失败，由 Agent 构建）。其他 setup 不含此字段。 |
| `llm_time_ms` | episode 中所有被包装的模型调用经过时间之和，包括失败的调用和压缩。较早的 episode 会回退为使用带 usage 的 assistant/tool-call 消息上记录的时长；这些时长不完整时为不可用。提供商内部的重试计入经过时间，但不单独计数。 |
| `vision_llm_calls`、`vision_llm_time_ms` | 根据模型消息之前的图像/截图观察推断出的调用。这是一种埋点启发式，并不是提供商计费的精确类别。 |
| `time_to_first_token_ms` | episode 报告的首 token 时间（如有）。 |
| `device_execution_ms` | 被识别为设备操作结果的 episode 时长之和；缺少所需时长时为不可用。工具分类基于一份显式白名单。 |
| `screenshot_capture_ms` | runner 侧计时的截图采集；不包括 Agent 在内部拍摄的所有截图。请查看 `screenshot_capture_source`。 |
| `tool_calls`、`device_actions` | 所有被追踪的工具调用，与被识别为会改变设备状态的调用。只读的桥接查询不计入设备操作。 |
| `llm_calls` | episode 中被包装的模型调用次数，包括失败和压缩。较早的 episode 会回退为统计携带 usage 的消息。不统计提供商传输层内部的重试。 |
| `input_tokens`、`output_tokens`、`total_tokens` | 报告的 Agent 用量；episode 总数可以覆盖由历史推导出的总数。不衡量整个运行的成本，也不包括 Judge/setup 的消耗。 |
| `cached_input_tokens`、`reasoning_tokens` | 可选的用量细节；未报告时为未知。不要把它们作为独立的额外用量加到总数上。 |
| `tool_errors` | 记录的工具结果错误，包括 Agent 之后已经恢复的错误。 |
| `replan_count` | episode 中显式的 `needs_replan` 证据；不根据工具名推断。 |
| `cost_usd` | episode 中提供商报告的模型调用美元成本之和。兼容 OpenAI 的 `usage.cost` 会被保留，包括 OpenRouter 的计费。每个被包装的调用都必须报告一个有限的非负成本；否则任务成本为未知。显式的 0 是有效的。不包括 setup、评判以及 Experiment 的发布/重放；从不根据 token 单价推算。 |
| `retry_count` | 当前的任务 runner 不填充 Agent 重试计数器。除非有显式来源提供，否则应为未知。 |
| `recovery_attempted`、`recovery_succeeded` | Agent 超时后 runner 的恢复；不代表用户任务成功，也不是 Agent 策略重试的次数。 |

效率分布使用带数值的计入尝试；`setup_ms` 使用所有带值的观察尝试。务必查看 `count`：基于更少测量得到的更低平均值未必是改进。
缺失的分数在 Langfuse 中会被省略，而不是发布为 0。百分位数使用插值计算，样本很少时证据很弱。各计时字段相互重叠或边界不同；
不要把它们相加当作完整的耗时分解。

`cost_to_first_success.{task_wall_ms,total_tokens,cost_usd}.{count,mean,p50}` 对每个字段从第 1 次尝试累加到首次成功，
遇到缺失或不计入的尝试即停止。某个字段必须在首次成功之前都完整，才会被计入。从未成功的任务不贡献任何值。
请将这些指标与成功率、覆盖率和总资源消耗结合使用；仅凭它们无法说明服务每个用户请求的成本。

### 数值后缀及其他解读细节

| 字段或后缀 | 含义 |
| --- | --- |
| `value` | JSON 指标的主值；比例在 0–1 之间。Langfuse 通常在主分数名称中省略 `.value`。 |
| `count` | 数值样本数量；对效率指标通常是尝试数，对首次成功类汇总通常是任务数。 |
| `sum`、`mean` | 可用值的总和与算术平均；部分覆盖不能构成完整的账单。 |
| `p50`、`p90`、`p95` | 以指标单位表示的插值百分位数，不是置信水平。 |
| `ci95.lower/upper` | JSON 中的 Wilson 区间上下界；Langfuse 中的后缀为 `ci95_lower/ci95_upper`。 |
| `successes` | 对通过率指标是成功的任务数，对尝试成功率是成功的尝试数。 |
| `eligible_tasks/total_tasks` | 该指标的计入任务数与观察到的唯一任务数。 |
| `eligible_attempts/total_attempts` | 计入尝试数与观察到的尝试数。 |
| `_ms`、`_tokens`、`_usd` | 分别为毫秒、token 数和美元；毫秒除以 1000 即为秒。 |
| `screenshots_taken` | 标记了 `has_screenshot` 的工具调用，而不是 Agent/runner 的每一张截图。 |
| `pre_screenshot_file/post_screenshot_file` | 本地截图文件是否存在，而不是其内容是否正确。 |
| `screenshot_capture_source/cost_source` | 结果中的来源元数据，不是独立的 Langfuse 数值分数。 |
| `error/agent_error/judge_error/environment_state_error` | 解读粗粒度失败标签所需的细节。 |
| `episode_error/pre_screenshot_error/post_screenshot_error` | 产物获取错误；可能减少证据/指标，但不一定让每个任务失败。 |
| `episode_id/skills_fingerprint` | 遥测关联，以及每个 worker 所加载 Skill 文本的内容摘要。同等条件的运行请比较 `manifest.skills_fingerprint.sha256`；`source` 用于区分可观察的目录树与无法观察的外部守护进程。 |
| `started_at/finished_at` | 结果时间戳，二者之差不能与仅任务的耗时互换使用。 |
| `environment_state_assertions/trace_observations` | 单项已配置的状态检查 / 行为观察。 |

`aggregate.per_task` 报告的总尝试数包含无效尝试，但通过/失败/通过率使用计入的尝试，最佳与平均质量以及平均耗时使用已知的数值。
它当前的 `first_attempt_passed` 指的是排序后的第一次**计入**尝试，这与要求实际第 1 次尝试的 `pass_at_1` 不同。
不要用一个替代另一个。

质量分数缺失时，`oracle_best_score_at_k.count` 可能小于其 `eligible_tasks`。请读取每次运行的 `manifest.metrics_k`；
不同的运行不会被自动合并为 k 次尝试。

## 3. 计算规则

### 3.1 来源与优先级

尝试级指标在运行级汇总之前组装：

1. runner 记录 setup、chat 请求和截图的计时。
2. 保存的历史提供模型用量、工具调用以及初始错误证据。
3. 该请求的 episode 提供设备结果时长、显式重新规划、首 token 时间以及可用的 token 总数。此步骤产出的字段会覆盖由历史推导出的同名字段；
   两个来源不会相加。
4. 断言和 Judge 决定结果、计入资格、质量以及最终的失败标签。通过的结果会清除失败阶段和事件引用，即使计数器中仍保留已恢复的工具错误。
5. 汇总器读取尝试行，计算任务级能力和尝试级资源分布。Langfuse 发布器导出这些值；它不会根据 trace span 重建它们。

| 事实来源 | 职责 |
| --- | --- |
| [任务 runner](../../benchmark/runner/runtask.py) | 测量边界、评估顺序、结果字段、来源优先级 |
| [确定性断言](../../benchmark/runner/assertions.py) | 预期答案解析和记忆召回证据检查 |
| [指标推导与汇总](../../benchmark/runner/metrics.py) | 历史/episode 计算、设备分类、固定 k 指标、分布 |
| [trace 提取](../../benchmark/runner/trace.py) | 从历史中提取工具调用列表和最终回复 |
| [工具执行](../../src/agent/internal/agent/tool_execution.go)和 [episode 记录](../../src/agent/internal/agent/task_episode.go) | Agent 侧的设备调用计时器和记录的事件时长 |
| [运行时回调](../../src/agent/internal/agent/runtime.go) | 首 token 时间和 Agent 用量总数 |
| [运行规划](../../benchmark/runner/main.py) | 计划的重复次数和 manifest 中的 `metrics_k` |
| [Langfuse 发布器](../../benchmark/runner/langfuse_reporter.py) | 分数名称、尝试级评分细则比率，以及省略不可用分数 |

### 3.2 任务、setup 和截图计时器

runner 使用单调时钟，并将经过的毫秒数截断为整数：

```text
elapsed_ms = int((end_monotonic - start_monotonic) * 1000)
```

| 字段 | 起点与终点 | 缺失或部分测量 |
| --- | --- | --- |
| `task_wall_ms` / `wall_ms` | 从紧接提示词准备和 `client.chat` 之前开始，到返回或抛出异常为止。包含请求/响应开销。 | 如果执行从未到达该计时器，则不可用。超时时长在恢复之前记录。离线评估使用提供的任务计时；其旧版回退可能使用提供的起始时钟，因此请核查来源。 |
| `setup_ms` | 围绕 `prepare_task_isolation`，包括已配置的 setup 工作。 | setup 失败时记录从该次尝试初始时钟开始经过的时间。之后的就绪检查和 runner 截图不在正常的 setup 计时器内。 |
| `screenshot_capture_ms` | runner 成功执行的操作前/后 `take_environment_screenshot` 调用之和，包括获取和本地保存。 | 失败的采集不贡献时长。操作前采集成功而操作后采集失败会得到部分总和；请检查截图错误。没有任何成功的计时采集时为 null。复制输入图像不会增加采集时长。 |
| `time_to_first_token_ms` | 复制自 `episode.extra.first_token_time_ms`；运行时从其运行开始测量到第一个流式回调。 | 只有为正值时运行时才会导出。没有该 episode 值时为不可用。这不是每次模型请求的 TTFT，也不是最终回答可见为止的时间。 |

runner 截图通常发生在 `task_wall_ms` 之外。Agent 工具内部的截图或等待则可能属于该工具的时长。
因此这些计时器并不构成任务总时长中互斥的组成部分。

### 3.3 模型调用、模型时间与视觉推断

历史推导按顺序遍历消息。只有当消息类型为 `assistant` 或 `tool_call` 且其 `usage` 是一个映射时，才计为一次模型调用。
不会再按提供商请求 ID 去重。

```text
llm_calls   = number of qualifying usage-bearing messages
llm_time_ms = sum(duration_ms on those messages)
```

如果没有符合条件的 usage，调用次数为 null。如果任一符合条件的消息缺少非负的数值时长，时长总和为 null；它不是仅对已测量子集的求和。
历史中缺失或没有 usage 的调用无法通过此计算恢复。记录的模型时长衡量的是调用延迟，而不是单独测量的 GPU 计算时间。

视觉推断使用一个“待处理图像”标志：

1. 遇到图像附件或类截图的工具结果时设置该标志：工具名为 `screenshot`、文本包含 `screenshot observation`，
   或 JSON 中带有字符串 `data` 字段以及宽度或高度。截图结果检测要求内容为字符串；图像附件则单独检查。
2. 将下一条符合条件的带 usage 消息计为一次视觉模型调用，并将其时长计入视觉总和。
3. 在该符合条件的消息之后清除标志。

当存在 usage 但没有调用匹配该启发式时，`vision_llm_calls` 为 0；没有符合条件的 usage 时为 null。
`vision_llm_time_ms` 要求至少有一次匹配的调用，且这些调用的时长完整有效；否则为 null。在一次模型调用之前的多张截图计为一次视觉调用。
保留在后续模型上下文中的图像不会被自动再次计数。

例如，900、1,400 和 700 ms 的三次模型调用得到 `llm_calls=3` 和 `llm_time_ms=3000`。如果只有第二次调用紧跟在截图之后，
则 `vision_llm_calls=1`、`vision_llm_time_ms=1400`。视觉时长已经包含在模型时长中；将两者相加会重复计算。

### 3.4 设备操作与设备执行时间

当前的设备操作白名单为：

```text
touch_gesture, enter_text, keyboard_text, keyboard_tap, mouse_move,
mouse_scroll, quick_action, open_app, launch_app, open_url,
search_launch_app, bridge_open_app, bridge_clipboard, bridge_contacts,
bridge_calendar, bridge_notification, bridge_media,
swipe, tap, long_press, drag, press_key
```

名称必须与白名单一致。带布尔值 `list=true` 的 `quick_action` 被排除。任何列出的 `bridge_*` 工具，如果其 `action`
（不区分大小写）为 `query`、`read`、`get`、`inspect`、`list` 或 `search`，也会被排除。输入缺失或无法解析时，
不构成任何一种排除。新增工具需要显式更新分类；该指标不会根据输出推断设备效果。

```text
device_actions = count of qualifying tool_call messages in history
device_execution_ms = sum(max(0, duration_ms))
                      over qualifying tool_result events in the episode
```

对 episode 进行分类时，结果中的 `tool_input` 优先。如果它缺失或为 null，则使用同名工具之前最后一次的输入。
此回退基于工具名称，而不是唯一的调用 ID。

运行时在处理工具调用时（在输入规范化和校验之前）启动计时器，并一直测量到工具返回和错误处理结束。它在调用后钩子和结果发出之前记录时长。
正的时长在 episode 中会被转换为整数毫秒，因此不足 1 毫秒的调用可能被记为 0。

该时长可能包含通信、内部等待、嵌套工作和设备执行。它不是纯粹的硬件时间，不是手势所请求的 `duration_ms`，
也不能独立证明页面已经加载完成。失败和被拒绝的设备调用只要记录了时长，同样会计入。独立的截图、搜索、记忆和模型调用不在白名单内；
但在列出的工具内部执行的类似工作仍计入该工具的计时器。

只有当 episode 中有事件、且每个符合条件的结果都有数值时长时，总和才可用。负的记录值会被截为 0。有事件但没有符合条件的结果时，值为 0。
没有 episode 事件，或某个符合条件的结果缺少时长时，该值不可用。此完整性检查只覆盖已记录的结果：结果事件完全缺失的调用仍可能被排除在总和之外。
因此，`device_actions` 与有计时的结果数量可能不同。

例如，200 ms 的点按、800 ms 的文本输入和 400 ms 的失败滑动共贡献 1,400 ms。单独的 300 ms 截图和 11 ms 的
`bridge_contacts` 查询对这个设备总和没有贡献。

### 3.5 Token、工作量计数器与失败证据

token 计算使用与 `llm_calls` 相同的符合条件的消息。每个字段独立求和；任一符合条件的消息缺少该字段时，由历史推导出的总数为 null。

| 指标 | 历史来源与计算 | episode 中为数值时的覆盖来源 |
| --- | --- | --- |
| `input_tokens` | 对 `usage.input_tokens` 求和，每条消息缺失时回退到 `prompt_tokens` | `extra.prompt_tokens` |
| `output_tokens` | 对 `usage.output_tokens` 求和，缺失时回退到 `completion_tokens` | `extra.completion_tokens` |
| `total_tokens` | 对 `usage.total_tokens` 求和；若缺失，仅当 input 和 output 都已知时使用两者之和 | `extra.total_tokens` |
| `cached_input_tokens` | 对 `usage.cached_input_tokens` 求和，缺失时回退到 `cached_tokens` | `extra.cached_prompt_tokens` |
| `reasoning_tokens` | 对 `usage.reasoning_tokens` 求和 | `extra.reasoning_tokens` |

episode 中的值会替换该字段由历史得出的整个总和（包括数值 0），而不是与之相加。显式的总数会被保留，而不会强制等于 input 加 output。
可选的 cached/reasoning 字段缺失时不会被假定为 0。这些值覆盖的是被记录的任务 Agent，不包括 Judge 或单独的 setup 模型消耗。
当前 runner 中，`cost_usd` 不会根据 token 单价自动计算。

| 字段 | 推导方式与边界情况 |
| --- | --- |
| `tool_calls` | 从历史中提取的工具调用数量，包括之后没有结果的调用。暴露出来的组合工具计为一次，而不是按其内部的每个操作计数。 |
| `screenshots_taken` | 统计结果中带有真值 JSON `data` 或包含 `screenshot observation` 的已提取调用。这是基于结果形态的启发式，不是对每张截图的计数。 |
| `tool_errors` | 历史中统计结果级的 `is_error`、JSON 内容中的 `is_error` 或 JSON `ok=false`。非空的 episode 事件列表会将此计数替换为标记了 `is_error` 的 `tool_result` 事件数量；不会再次解析其内容。空历史得到 0，但仅凭这一点不能证明遥测完整。 |
| `replan_count` | 统计所有 episode 事件中为真的 `needs_replan` 值。至少要有一个事件包含该键；否则为 null。显式全为 false 的证据得到 0。 |
| `retry_count` | 没有显式来源时保持 null。重复的工具名或基准测试尝试不会被计为 Agent 重试。 |
| `first_failure_stage`、`failure_event_ref` | 历史最初指向其第一个被识别的工具错误；episode 错误可以替换此引用。白名单内的设备错误映射为 `device_execution`，其他工具错误映射为 `unknown`。之后的结果处理可能将阶段覆盖为 setup/evaluation/unknown，或在成功时清除这两个字段。 |
| `recovery_attempted`、`recovery_succeeded` | 当 runner 捕获到 Agent 超时并调用恢复时设置。恢复返回的布尔值与失败的任务结果分开记录。这一对字段不统计工具重试，也不统计所有初始化恢复工作。 |

### 3.6 质量分数与评分细则通过率

评估顺序解释了为什么这两个值可能不同：

1. 首先检查执行错误和硬断言。计入的失败得到 `quality_score=0`；没有执行证据的执行错误/超时得到 null，且不计入。
2. 接着运行已配置的预期答案和记忆召回检查。不匹配得到 0；所需的召回证据不可用时产生评估错误，质量为未知。
3. 如果 Judge 被禁用，通过前述关卡即得到质量 1。否则，质量为 `rubric_pass_count / rubric_total`，其中通过指判定为
   `yes`。任务完全成功要求所有评分细则检查都通过。Judge 错误会让质量保持未知。评分细则条目数和通过数都为 0 时，
   结果为通过且质量为 1。
4. 之后应用已配置的环境状态检查。状态缺失会让质量为未知且该次尝试不计入；状态检查失败会把原本通过的结果变为计入的失败，质量为 0。

`expected_answer_match` 目前支持 `option_letter` 格式：断言解析器会规范化预期选项，从最终回复中提取预测的选项，然后进行比较。
预期答案或预测答案无法解析时即为失败；这不是语义相似度分数。

`expected_recalled_memory_match` 要求调用已配置的召回工具，并检查所有预期的记忆 ID 是否出现在其证据中；额外的 ID 不会导致检查失败。
完整的内联工具结果优先。如果它们不完整，除非任务要求内联证据，否则 episode 的 `retrieved_memory_refs` 可作为回退证据。
回退会对照其他召回工具检查归属：缺失预期 ID 即失败，但归属不明确或所需证据不可用时得到 null。没有调用已配置的工具则为 false。
请结合 `memory_recall_evidence_source` 一起解读该布尔值。

Langfuse 的 `benchmark.rubric_pass_rate` 在总数非零时，会独立地用存储的通过数除以存储的评分细则总数。总数为 0 时省略该分数。
例如，四条中有三条判定为 `yes` 时，质量为 0.75、评分细则通过率为 0.75，但任务失败。使用 `--no-judge` 时，同样的四条已配置检查保持未评估：
质量为未知，导出的评分细则比率为 0/4。这些尝试为 `skipped`，带有 `judging_disabled=true`，在重新评判之前不计入能力指标。
已知的硬关卡失败仍计为失败。比较这些字段之前，请先检查各条判定。

### 3.7 固定 k 能力与首次成功的计算

按 `task_id` 和实际尝试序号对结果行分组。设 `T` 为观察到的唯一任务数，`E1` 为第 1 次尝试计入的任务，`Ek` 为第 1 到第 k
次尝试全部存在且计入的任务。

```text
pass_at_1 = tasks in E1 whose attempt 1 succeeds / size(E1)
pass_at_k = tasks in Ek with any success in attempts 1..k / size(Ek)
pass_pow_k = tasks in Ek with all successes in attempts 1..k / size(Ek)
coverage.pass_at_1 = size(E1) / T
coverage.pass_at_k = coverage.pass_pow_k = size(Ek) / T
attempt_success_rate = successful eligible rows / eligible rows
```

没有计入的分母时通过率为 null；没有观察到任何任务时覆盖率为 0。`attempt_success_rate` 包括 k 之后的计入尝试。
结果中完全缺失的行不会进入观察到的覆盖率；请另行检查 manifest 中计划的尝试。重复的任务/尝试行在任务分组中会相互覆盖，
而尝试分布仍会使用所有行；输入结果中每个计划的任务/尝试必须只有一行。

以 k=2 为例：

| 任务 | 第 1 次尝试 | 第 2 次尝试 | 属于 E1？ | 属于 Ek？ |
| --- | --- | --- | --- | --- |
| A | 通过 | 失败 | 是 | 是 |
| B | 失败 | 通过 | 是 | 是 |
| C | 通过 | 通过 | 是 | 是 |
| D | 通过 | 不计入 | 是 | 否 |

此时 `pass_at_1=3/4`、`pass_at_k=3/3`、`pass_pow_k=1/3`，固定 k 覆盖率为 `3/4`，尝试成功率为 `5/7`。
较高的 pass-at-k 描述的是机会更多时观察到的成功，而不是首次尝试可靠性的提升。

对于 `oracle_best_score_at_k`，取 Ek 中每个任务可用的最高质量分数，再对这些最高值求平均。没有数值质量的任务不计入 `count`；
只有部分质量已知的任务仍贡献其已知的最佳值。

对于首次成功类指标，从实际第 1 次尝试开始，遇到第一个缺失或不计入的尝试时停止。在这个连续前缀内找到第一次成功；
搜索可以延伸到 k 之后。其尝试序号计入 `first_success_attempt`。对于 `cost_to_first_success`，将每个资源字段分别累加到
（并包括）这次成功；该字段只有在直到成功为止的每个值都是数值时才会被计入。从未成功的任务被省略。

例如，耗时分别为 2,000 和 3,000 ms 的计入尝试“失败/通过”，贡献首次成功尝试序号 2 和累计任务时间 5,000 ms。
如果第二次尝试的 token 缺失，该任务仍贡献计时，但不贡献累计 token 值。“失败/不计入/通过”对两个首次成功类指标都没有贡献，
因为计入前缀在成功之前就已结束。

### 3.8 分布、置信区间与诊断比率

对于每个效率/可靠性字段，从计入的尝试行（包括计入的失败）中收集非布尔的数值。`setup_ms` 则使用所有观察到的行。
因此每个字段都有自己的样本数。

```text
count = number of included values
sum = sum of included values
mean = sum / count
```

没有值时，count 为 0，sum/mean/百分位数为 null。对于排序后的值 `x[0]` 到 `x[n-1]`，百分位数 P 使用线性插值：

```text
h = (n - 1) * P / 100
f = floor(h)
c = min(f + 1, n - 1)
percentile(P) = x[f] + (x[c] - x[f]) * (h - f)
```

对于 100、200 和 900 ms，p50 为 200 ms，p90 为 760 ms。后者不一定是某个实际观察到的时长。只有一个样本时，所有百分位数都等于它；
这并不能说明尾部延迟是稳定的。

通过率指标和尝试成功率使用 Wilson 95% 区间。设成功数为 `s`、计入试验数为 `n`、`p=s/n`、`z=1.959963984540054`：

```text
d = 1 + z*z/n
center = (p + z*z/(2*n)) / d
margin = z * sqrt(p*(1-p)/n + z*z/(4*n*n)) / d
lower = max(0, center - margin)
upper = min(1, center + margin)
```

n=0 时上下界为 null。对通过率指标，试验是任务；对尝试成功率，试验是尝试，后者的区间不建模任务内部的相关性。

其余诊断比率使用不同的总体：

| 字段 | 计算方式 |
| --- | --- |
| `coverage.invalid_attempts` | 观察到的尝试行数减去计入的尝试行数 |
| `diagnostics.failure_stage_coverage` | 具有已识别的非 unknown 阶段的计入失败尝试数 / 计入失败尝试数；没有计入失败时为 null |
| `aggregate.by_category.<category>.passed/total` | 本地产物中的类别通过比率：`status=passed` 的行数 / 该类别的所有行数（包括不计入的行）；不再导出为 `category.<category>.pass_rate` |
| `aggregate.by_category.<category>.rubric_pass/rubric_total` | 本地报告中的类别评分细则比率：存储的评分细则通过数之和 / 评分细则总数之和（为正时）；按评分细则加权，而不是任务比率的平均值；不再导出为 `category.<category>.rubric_pass_rate` |
| `observations.<id>.pass_rate` | 该 ID 至少有一次观察通过的行数 / 包含该 ID 的行数；同一行内对同一 ID 的重复检查只计一次 |

失败类别计数使用所有已分类的行；阶段计数只使用计入的失败。没有显式分母时，类别计数和阶段计数都不是百分比。

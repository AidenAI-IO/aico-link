---
sidebar_position: 2
---

# SkillOpt 架构

SkillOpt 优化 Skill 文本的方式是：运行当前 Skill，提取失败模式，提出一个候选补丁，
并且只有当候选在留出的验证任务上有所提升时才接受它。

## 核心组件

```text
+--------------+
| SkillOpt CLI |  or standalone SkillOpt WebUI
+------+-------+
       | optimize_skill()
       v
+----------------+
| Orchestrator   |  baseline -> train -> reflect -> patch -> selection -> gate
+------+---------+
       | rollout backend
       v
+--------------------------+
| Device or Runner Backend |
+------+-------------------+
       | direct /api/chat, or benchmark runner child run
       v
+------------------------+
| Agent and Environment  |  physical device, bridge, or MobileGym
+------------------------+
```

相关文件：

| 文件 | 职责 |
| --- | --- |
| `skillopt/main.py` | CLI、参数校验、写入产物、渲染报告。 |
| `skillopt/webui.py` | 独立 WebUI、作业记录、MobileGym 环境控制。 |
| `skillopt/orchestrator.py` | 主优化循环和闸门决策。 |
| `skillopt/benchmark_backend.py` | 通过 `benchmark/runner` 进行基于桥接的 rollout。 |
| `skillopt/backends.py` | 直连已有守护进程的 rollout 后端。 |
| `skillopt/phase_artifacts.py` | SkillOpt 自有的阶段记录，用于进度、报告和失败重建。 |
| `skillopt/score.py` | 将基准测试任务结果转换为 SkillOpt 的 hard 和 soft 分数。 |
| `skillopt/reflect.py` | 根据 rollout 失败构建优化器提示词。 |
| `skillopt/aggregate.py` | 对提出的编辑去重并排序。 |
| `skillopt/patch.py` | 将候选编辑应用到 Skill 文本。 |

## 优化流程

对于显式指定了训练套件和验证套件的运行，SkillOpt 依次执行：

1. `baseline_selection`：在留出的验证任务上运行原始 Skill。
2. `step_N_train`：在训练任务上运行当前 Skill。
3. `reflect`：让优化器模型根据 rollout 证据给出补丁建议。
4. `aggregate`：去重，并将编辑数量限制在 `--edit-budget` 以内。
5. `apply`：生成 `step_N/candidate.md`。
6. `step_N_selection`：在留出的验证任务上运行候选。
7. `gate`：仅当 `candidate.hard > current.hard + min_delta` 时接受。
8. 持久化 `best_skill.md`、`diff.patch`、`result.json` 和 `report.html`。

如果没有候选通过验证闸门，当前最佳 Skill 仍是最后一个被接受的版本。

`--budget` 是最大优化迭代次数。`--edit-budget` 是每次迭代中提出的最大 Skill 编辑数。

## 后端

### 直连设备后端

不带 `--environment-url` 的 `--backend device` 使用 `AicoDeviceBackend`。它通过
`/api/chat` 与已有的 Agent 守护进程通信，并在每个阶段运行期间临时覆盖 Skill 文本。

当你已经有一个守护进程、并且不需要桥接截图或并发时，可用该模式进行快速本地调试。

### Benchmark Runner 后端

任何带 `--environment-url` 的运行都使用 `BenchmarkRunnerBackend`。这包括
`--backend mobilegym` 以及基于桥接的 `--backend device` 运行。

对于每个阶段，SkillOpt 都会启动一次子基准测试运行：

```text
skillopt/runs/<run_id>/benchmark/<run_id>-<phase>/
```

子运行使用：

- `runner.main run`
- `--auto-agent-setup`
- `--environment-url <bridge>`
- 每个活跃的任务 worker 对应一个隔离的 Agent 守护进程 worker
- 基准测试的操作前/后截图、trace 提取、Judge 和 HTML 报告

这是 MobileGym 和可复现验证的首选路径。

子基准测试报告是原始的执行证据。SkillOpt 将它们的链接保存在阶段记录中，并在顶层
SkillOpt 报告中渲染为 `report` 下钻链接。

## 套件与目标

SkillOpt 自有的套件位于：

```text
skillopt/suites/<skill>/
```

WebUI 按文件名发现套件对：

```text
<prefix>_train.json
<prefix>_verification.json
<prefix>_validation.json
```

例如，下面这些文件会成为 `device-operator` 的两个可选目标：

```text
device_operator_train.json
device_operator_verification.json
shopping_scenario_train.json
shopping_scenario_verification.json
```

CLI 标签使用逻辑形式 `skillopt/<skill>/<suite_stem>`，例如
`skillopt/device-operator/device_operator_train`。

也可以通过 `--suite` 传入基准测试套件；此时 SkillOpt 会将套件按 70/30 拆分为训练任务和选择任务。
推荐使用 SkillOpt 套件，因为它们让训练集和留出验证集的边界更明确。

## 评分

SkillOpt 将每个基准测试 `TaskResult` 转换为一个 rollout 分数：

| 分数 | 定义 |
| --- | --- |
| `hard` | 任务状态为 `passed` 时为 `1`，否则为 `0`。 |
| `soft` | 通过的评分细则条目数除以评分细则总数；没有评分细则时等于 `hard`。 |

阶段分数是所有任务的平均值：

```text
hard = fully passed tasks / total tasks
soft = passed rubric items / total rubric items
```

验证闸门以 `hard` 作为主要分数。只有当候选在当前验证 hard 分数基础上的提升超过
`--min-delta` 时，才会被接受。

## Judge 与证据

启用 Judge 时，SkillOpt 将任务评分委托给基准测试 Judge。对于基于桥接的运行，
Judge 的输入是基准测试证据：

- 任务提示词和评分细则
- 来自环境桥接 `POST /api/providers/screenshot` 的操作前/后截图
- 来自 Agent 历史的结构化工具 trace
- 最终回复

Agent 运行过程中的中间截图不会作为基准测试 Judge 的图像输入。如果某个任务依赖视觉验证，
缺少 `pre.jpg` 或 `post.jpg` 会降低评分细则的可靠性。

`--no-judge` 会跳过评分细则 Judge，只使用硬断言。它适合快速收集 trace，
但不应被当作最终验证。

## 产物

CLI 输出默认位于 `skillopt/runs/<run_id>/`。WebUI 输出默认位于
`skillopt/runs/webui/<job_id>/`。

```text
<run_id>/
|-- manifest.json              # run config, totals, linked reports
|-- result.json                # serialized OptimizationResult
|-- report.html                # top-level SkillOpt report
|-- best_skill.md              # best accepted skill text
|-- diff.patch                 # original skill -> best skill
|-- skillopt.log               # WebUI job log when run from WebUI
|-- phases/                    # SkillOpt phase/task status records
|   |-- baseline_selection.json
|   |-- step_01_train.json
|   `-- step_01_selection.json
|-- step_01/
|   |-- candidate.md           # candidate skill text for this step
|   |-- patch.json             # aggregated edits
|   |-- patch_reports.json     # patch application results
|   `-- decision.json          # gate decision
|-- configs/<phase>/           # per-phase agent config and skills
|-- logs/<phase>.log           # child benchmark stdout/stderr
`-- benchmark/<run_id>-<phase>/# child benchmark report and task artifacts
```

用顶层 SkillOpt 报告判断哪个 Skill 胜出。用链接的子基准测试报告调试任务级失败。

每个 `phases/*.json` 文件使用 `skillopt.phase.v1` schema，记录阶段名称、类型、套件、
状态、任务列表、计数、分数（如有）、错误（失败时），以及后端生成的基准测试报告链接（如有）。
WebUI 读取这些文件来显示当前阶段进度和最佳分数，而不依赖 Benchmark WebUI 的状态。

如果优化器在产物初始化之后因异常退出，SkillOpt 仍会写入 `manifest.json`、`result.json`
和 `report.html`。失败报告会显示错误、阶段表、已生成的产物，以及已存在的 diff 或最佳 Skill。

## 环境变量

| 变量 | 含义 |
| --- | --- |
| `OPENROUTER_API_KEY` | 优化器和 Judge 的 API key。 |
| `AICO_SKILLS_DIR` | 可选，覆盖 Skill 源目录。 |
| `AICO_ENVIRONMENT_URL` | CLI 运行的默认桥接端点。 |
| `AICO_DAEMON_IMAGE` | benchmark runner worker 使用的默认守护进程镜像。 |

## 设计决策

### 为什么将 SkillOpt 与 Benchmark 分离？

Benchmark 评估任务并产出证据。SkillOpt 消费这些证据来优化 Skill。保持单向依赖可以防止
仅用于优化的套件和候选 Skill 污染基准测试的套件发现、报告和 WebUI 状态。

### 为什么使用留出验证？

Skill 的修改可能对训练集中的失败过拟合。选择阶段会在留出任务上重新运行候选，
闸门会拒绝没有提升当前验证 hard 分数的候选。

### 为什么 MobileGym 使用基于桥接的运行？

MobileGym 并发、setup/release、操作前/后截图以及隔离的守护进程 worker 都已在
benchmark runner 中实现。复用这条路径能让 SkillOpt 获得与常规基准测试运行相同的执行语义和报告。

## 相关文档

- [快速上手](./quickstart.md)
- [SkillOpt 概览](./README.md)
- [Benchmark 架构](../09-benchmark/architecture.md)
- [环境桥接协议](../../benchmark/environment_bridge.md)

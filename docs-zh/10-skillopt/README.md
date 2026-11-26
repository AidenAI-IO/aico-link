---
sidebar_label: 概览
sidebar_position: 0
---

# SkillOpt

SkillOpt 是一套开发者工作流，通过反复执行 rollout、反思、打补丁和留出集验证，
来改进 Agent 的 `SKILL.md` 文件。它优化的是 Skill 文本，而不是模型权重或基准测试套件定义。

依赖方向刻意保持单向：

```text
SkillOpt -> benchmark runner APIs -> environment bridge -> device or MobileGym
```

Benchmark 保持通用：它运行套件并生成任务报告。SkillOpt 把这些 runner 能力用作
rollout 后端，再决定是否接受某个候选 Skill。

## 从这里开始

- [快速上手](./quickstart.md) - WebUI、CLI、MobileGym 和常用命令。
- [架构](./architecture.md) - 优化循环、后端、评分和产物。
- [Benchmark 文档](../09-benchmark/README.md) - runner、桥接、Judge 和报告的细节。

## 推荐入口

### WebUI

日常优化运行请使用独立的 SkillOpt WebUI：

```bash
uv run --project skillopt python -m skillopt webui --host 127.0.0.1 --port 8766
```

打开 `http://127.0.0.1:8766`。

SkillOpt WebUI 可以从 `skillopt/suites` 中发现目标、管理 MobileGym 环境、
运行优化作业、显示当前阶段和任务进度、展示最佳分数，并打开生成的 SkillOpt 报告。

### CLI

脚本化运行和调试请使用 CLI：

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend mobilegym \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --edit-budget 2 \
  --output /tmp/device-operator-best.md
```

应用前先审阅：

```bash
diff src/agent/config/skills/device-operator/SKILL.md /tmp/device-operator-best.md
```

## SkillOpt 的产出

每次运行都会在 `skillopt/runs/` 或 `skillopt/runs/webui/` 下写入一个自包含的产物目录：

```text
skillopt/runs/<run_id>/
|-- manifest.json
|-- result.json
|-- report.html
|-- best_skill.md
|-- diff.patch
|-- phases/
|   |-- baseline_selection.json
|   |-- step_01_train.json
|   `-- step_01_selection.json
|-- step_01/
|   |-- candidate.md
|   |-- patch.json
|   |-- patch_reports.json
|   `-- decision.json
|-- logs/
`-- benchmark/<run_id>-<phase>/
```

`report.html` 是顶层的 SkillOpt 报告；启用产物时，无论 CLI/WebUI 运行成功还是失败
都会写入。它回答这些问题：运行了哪个优化阶段、运行为何停止、Skill 是否得到改进、
接受了哪个候选。`best_skill.md` 是（若存在）被接受的最佳 Skill 文本，
`diff.patch` 展示它与原始 Skill 的差异。

`phases/*.json` 是 SkillOpt 自有的阶段记录，WebUI 和报告用它来显示排队中、运行中、
通过、失败、跳过、judge 错误和超时的任务数量。子基准测试报告仍可通过 `report`
下钻链接查看，作为任务级证据。

## 与 Benchmark 的边界

- Benchmark WebUI 只列出基准测试套件。
- SkillOpt WebUI 是一个单独的进程，默认使用端口 `8766`。
- SkillOpt 自有的套件位于 `skillopt/suites` 下。
- 基于桥接的 SkillOpt 运行使用 benchmark runner 的子运行。
- Benchmark 报告用于下钻查看证据；SkillOpt 报告决定哪个 Skill 文本胜出、
  以及优化器在哪里停止。
- `no_judge` 运行对收集 trace 很有用，但不能作为可靠的验证。

## 当前目标

当前仓库包含一个 SkillOpt 目标族：

```text
skillopt/suites/device-operator/
|-- device_operator_train.json
|-- device_operator_verification.json
|-- shopping_scenario_train.json
`-- shopping_scenario_verification.json
```

传给 CLI 的套件标签省略 `suites/` 和 `.json`，例如
`skillopt/device-operator/device_operator_train`。

## 相关文档

- [快速上手](./quickstart.md)
- [架构](./architecture.md)
- [Benchmark 快速开始](../09-benchmark/README.md)
- [Benchmark 架构](../09-benchmark/architecture.md)
- [环境桥接协议](../../benchmark/environment_bridge.md)

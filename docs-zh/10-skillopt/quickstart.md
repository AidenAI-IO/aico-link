---
sidebar_position: 1
---

# SkillOpt 快速上手

本页介绍运行 SkillOpt 的最短路径。内部原理见[架构](./architecture.md)。
任务执行细节见 [Benchmark 文档](../09-benchmark/README.md)。

## 前提条件

1. 在仓库根目录安装 Python 依赖：

   ```bash
   uv sync --project skillopt
   uv sync --project benchmark
   ```

2. 配置优化器和 Judge 的 API key：

   ```bash
   export OPENROUTER_API_KEY=...
   ```

3. 确认 Skill 存在：

   ```text
   src/agent/config/skills/device-operator/SKILL.md
   ```

4. 对于 MobileGym 或基于桥接的运行，启动或选择一个环境桥接。

## WebUI

在仓库根目录启动 SkillOpt WebUI：

```bash
uv run --project skillopt python -m skillopt webui --host 127.0.0.1 --port 8766
```

打开 `http://127.0.0.1:8766`。

推荐的 WebUI 流程：

1. 从 `Targets` 表中选择一个目标。
2. 保存一份 Agent 配置，其中模型 API key 不能为空。
3. 启动或选择一个 MobileGym 环境。
4. 设置 Judge 模型、最大迭代次数、每次迭代的最大编辑数以及 `min_delta`。
5. 运行作业，并在 `Report` 链接出现后打开它。

WebUI 将作业保存在 `skillopt/runs/webui/<job_id>/` 下。作业运行时，进度区域跟随
SkillOpt 自己的阶段记录，包括当前阶段、任务数量、失败的任务 ID、最佳分数，
以及顶层报告是否可用。历史作业和失败的作业只有在 `report.html` 存在时才会显示报告链接。

## MobileGym CLI 运行

在 benchmark 项目中启动 MobileGym 桥接：

```bash
cd benchmark
uv run python -m runner start-mobilegym-env --envs 5 --bridge-port 19090
```

在另一个 shell 中，从仓库根目录通过 benchmark runner 后端运行 SkillOpt：

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend mobilegym \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --edit-budget 2 \
  --min-delta 0.03 \
  --no-build-daemon-image \
  --output /tmp/device-operator-best.md
```

MobileGym 运行使用隔离的 benchmark 守护进程 worker，并以稳定的 `benchmark-task-id`
将每个任务路由到环境桥接。CLI 在启动优化循环前会在日志中输出
`Max iterations`、`Max edits / iteration` 和 `min_delta`。

## 使用已有设备守护进程运行

如果已有一个 Aico Agent 守护进程在运行，SkillOpt 可以直接使用它：

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --agent-url http://127.0.0.1:8080 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --output /tmp/device-operator-best.md
```

该模式在 rollout 期间临时覆盖目标 Skill，并重新加载 Agent 的 Skill 注册表。
如果你需要操作前后的截图、隔离的守护进程 worker 或 MobileGym 并发，请使用基于桥接的运行。

## 基于桥接的物理设备运行

对于通过环境桥接暴露的物理设备，保留 `--backend
device`，但提供 `--environment-url`：

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --environment-url http://127.0.0.1:19090 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 3 \
  --output /tmp/device-operator-best.md
```

设置了 `--environment-url` 后，即使使用 `--backend device`，SkillOpt 也会使用
benchmark runner 后端。

## 试运行与审阅

只打印建议的 diff，不写入输出文件或 Web 产物：

```bash
uv run --project skillopt python -m skillopt \
  --skill device-operator \
  --backend device \
  --agent-url http://127.0.0.1:8080 \
  --train-suite skillopt/device-operator/device_operator_train \
  --validation-suite skillopt/device-operator/device_operator_verification \
  --budget 1 \
  --dry-run
```

正常运行后，应用前先审阅：

```bash
diff src/agent/config/skills/device-operator/SKILL.md /tmp/device-operator-best.md
```

查看产物：

```bash
open skillopt/runs/<run_id>/report.html
```

顶层 SkillOpt 报告与子基准测试报告相互独立。用 SkillOpt 报告查看优化时间线、
被接受/拒绝的候选、最佳分数、失败原因、产物和 diff。用每个阶段的 `report`
下钻链接查看任务级的基准测试证据。

## 常用选项

| 选项 | 含义 |
| --- | --- |
| `--skill` | `src/agent/config/skills` 下的 Skill 目录名。 |
| `--suite` | 一个套件，按 70/30 拆分为训练任务和选择任务。 |
| `--train-suite` | 显式指定的训练套件标签。 |
| `--validation-suite` | 显式指定的留出验证套件标签。 |
| `--budget` | 最大优化迭代次数。 |
| `--edit-budget` | 每次优化迭代中提出的最大 Skill 编辑数。 |
| `--min-delta` | 接受候选所需的 hard 分数提升幅度。 |
| `--optimizer-model` | 用于反思和生成补丁的 OpenRouter 模型。 |
| `--judge-model` | 用于基准测试评分细则 Judge 的 OpenRouter 模型。 |
| `--no-judge` | 跳过 LLM Judge，只使用硬断言。 |
| `--sample-classifier` | `rules`（默认）或 `typesafe`：让置信度足够的 TypeSafe Jev 回答覆盖基于规则的样本质量判定。需要 `TYPESAFE_API_KEY`；规则结果保留为 `sample_quality_rule`。 |
| `--environment-url` | 用于基于 benchmark 的 rollout 的环境桥接端点。 |
| `--agent-config` | 传给 benchmark 守护进程 worker 的 Agent 配置。 |
| `--artifact-root` | SkillOpt 运行产物的根目录。 |
| `--run-id` | 稳定的运行 ID，便于编写可复现的脚本。 |

## 故障排查

`mobilegym backend requires environment_url`

启动一个 MobileGym 桥接，或从 WebUI 的环境表中传入端点。

`missing env var OPENROUTER_API_KEY`

设置 `OPENROUTER_API_KEY`，或提供一份 Agent 配置，其模型 API key 能被共享的
runner 配置辅助函数解析。

`skill not found`

SkillOpt 依次查找 `AICO_SKILLS_DIR`、
`src/agent/config/skills/<skill>/SKILL.md`、`skills/<skill>/SKILL.md`。

Judge 模型返回 HTTP 403 或区域错误

将 `--judge-model` 改为当前 OpenRouter 账号和区域可用的模型。Judge 出错并不代表任务执行有误。

使用 `--no-judge` 时验证结果好得不真实

`--no-judge` 只运行硬断言。它适合收集 trace，但对于依赖评分细则的 UI 任务可能产生假阳性。

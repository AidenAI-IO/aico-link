---
sidebar_position: 1
---

# Agent 驱动的基准测试快速上手

本页是精简版快速上手。完整指南见
[`benchmark/manual.md`](../../benchmark/manual.md)。

## 前提条件

1. Python 环境：

   ```bash
   cd benchmark
   uv sync
   ```

2. 使用内置配置模板时，配置 Agent 模型：

   ```bash
   export AICO_BENCHMARK_AGENT_PROVIDER=openai
   export AICO_BENCHMARK_AGENT_MODEL=your-agent-model
   export AICO_BENCHMARK_AGENT_BASE_URL=https://your-agent-endpoint.example/v1
   export AICO_BENCHMARK_AGENT_API_KEY=...
   ```

3. 启用 Judge 时，配置 Judge：

   ```bash
   export AICO_BENCHMARK_JUDGE_MODEL=your-judge-model
   export AICO_BENCHMARK_JUDGE_BASE_URL=https://your-judge-endpoint.example/v1
   export AICO_BENCHMARK_JUDGE_API_KEY=...
   ```

4. 可选的运行后 Analysis 设置默认沿用 Judge 的模型、端点和 API key。仅当
   Analysis 使用不同的运行时时，才需要设置该角色专用的覆盖项：

   ```bash
   export AICO_BENCHMARK_ANALYSIS_MODEL=your-analysis-model
   export AICO_BENCHMARK_ANALYSIS_BASE_URL=https://your-analysis-endpoint.example/v1
   export AICO_BENCHMARK_ANALYSIS_API_KEY=...
   ```

5. 一个已有的 Agent 守护进程，或者一个环境桥接加上
   `--auto-agent-setup`。

## 针对已有 Agent 运行

```bash
cd benchmark

uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --agent-url http://127.0.0.1:8080
```

跳过 Judge 以快速收集 trace：

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --agent-url http://127.0.0.1:8080 \
  --no-judge
```

运行指定任务：

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --task-id open_settings \
  --task-id scroll_page_down
```

## 使用 MobileGym 运行

启动 MobileGym 环境桥接：

```bash
uv run python -m runner start-mobilegym-env --envs 5 --bridge-port 19090
```

使用自动创建的隔离 Agent worker 运行套件：

```bash
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup
```

runner 从 `/api/concurrent` 读取桥接的容量。如果套件中的任务数超过桥接容量，
多出的任务会在队列中等待。

## WebUI

```bash
cd benchmark
uv run python -m runner webui
```

打开 `http://127.0.0.1:8765`。日常的 MobileGym 并发运行推荐使用 WebUI，
因为它会显示每个任务的 worker 状态、屏幕、日志以及持久化的作业记录。

## 输出

每次 CLI 运行都会创建 `runs/<run_id>/`：

```text
<run_id>/
├── manifest.json
├── results.jsonl
├── summary.md
├── report.html
└── tasks/<task_id>/
    ├── pre.jpg
    ├── post.jpg
    ├── history.json
    ├── trace.json
    └── judge.json
```

当配置了 `--environment-url` 且桥接通过 `POST /api/providers/screenshot`
返回截图时，才会生成 `pre.jpg` 和 `post.jpg`。Judge 会使用这两张图片以及
trace 和最终回复。

## 重新评判

无需在硬件上重新运行，即可更换评分细则（rubric）措辞或 Judge 模型：

```bash
uv run python -m runner rejudge --run-dir runs/<id> --judge-model claude-sonnet-4-6
```

## 比较运行结果

```bash
uv run python -m runner compare --runs runs/<id_a> runs/<id_b>
```

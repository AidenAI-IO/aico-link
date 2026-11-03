---
sidebar_position: 1
---

# Agent-Driven Benchmark Quickstart

This page is a compact quickstart. The complete guide is
[`benchmark/manual.md`](../../benchmark/manual.md).

## Prerequisites

1. Python environment:

   ```bash
   cd benchmark
   uv sync
   ```

2. Configure the Agent model when using the built-in config template:

   ```bash
   export AICO_BENCHMARK_AGENT_PROVIDER=openai
   export AICO_BENCHMARK_AGENT_MODEL=your-agent-model
   export AICO_BENCHMARK_AGENT_BASE_URL=https://your-agent-endpoint.example/v1
   export AICO_BENCHMARK_AGENT_API_KEY=...
   ```

3. Configure the Judge when it is enabled:

   ```bash
   export AICO_BENCHMARK_JUDGE_MODEL=your-judge-model
   export AICO_BENCHMARK_JUDGE_BASE_URL=https://your-judge-endpoint.example/v1
   export AICO_BENCHMARK_JUDGE_API_KEY=...
   ```

4. Optional post-run Analysis settings default to the Judge model, endpoint, and
   API key. Set role-specific overrides only when Analysis uses a different
   runtime:

   ```bash
   export AICO_BENCHMARK_ANALYSIS_MODEL=your-analysis-model
   export AICO_BENCHMARK_ANALYSIS_BASE_URL=https://your-analysis-endpoint.example/v1
   export AICO_BENCHMARK_ANALYSIS_API_KEY=...
   ```

5. Either an existing agent daemon, or an environment bridge plus
   `--auto-agent-setup`.

## Run Against An Existing Agent

```bash
cd benchmark

uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --agent-url http://127.0.0.1:8080
```

Skip judge for fast trace collection:

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --agent-url http://127.0.0.1:8080 \
  --no-judge
```

Run selected tasks:

```bash
uv run python -m runner run \
  --suite suites/phone_control_v1.json \
  --task-id open_settings \
  --task-id scroll_page_down
```

## Run With MobileGym

Start a MobileGym environment bridge:

```bash
uv run python -m runner start-mobilegym-env --envs 5 --bridge-port 19090
```

Run the suite with automatic isolated agent workers:

```bash
uv run python -m runner run \
  --suite suites/mobilegym_basic.json \
  --environment-url http://127.0.0.1:19090 \
  --auto-agent-setup
```

The runner reads bridge capacity from `/api/concurrent`. If the suite has more
tasks than the bridge capacity, extra tasks wait in the queue.

## WebUI

```bash
cd benchmark
uv run python -m runner webui
```

Open `http://127.0.0.1:8765`. The WebUI is the recommended path for routine
MobileGym concurrency because it shows per-task worker status, screens, logs,
and persisted job records.

## Output

Each CLI run creates `runs/<run_id>/`:

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

`pre.jpg` and `post.jpg` are present when `--environment-url` is configured and
the bridge returns screenshots through `POST /api/providers/screenshot`. Judge uses those two
images plus trace/final response.

## Rejudge

Change rubric phrasing or judge model without re-running on hardware:

```bash
uv run python -m runner rejudge --run-dir runs/<id> --judge-model claude-sonnet-4-6
```

## Compare Runs

```bash
uv run python -m runner compare --runs runs/<id_a> runs/<id_b>
```

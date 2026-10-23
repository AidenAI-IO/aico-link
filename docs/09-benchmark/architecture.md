---
sidebar_position: 2
---

# Benchmark Architecture

Benchmark uses an HTTP API-driven execution model plus offline scoring. Task
execution happens through the real agent and an optional environment bridge;
scoring can be re-run from saved artifacts.

## Core Components

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

The environment bridge contract is defined in
[`benchmark/environment_bridge.md`](../../benchmark/environment_bridge.md).

## Task Flow

For each task, `runner/runtask.py` performs:

1. Prepare task isolation and clear agent conversation history.
2. If `environment_url` is set, call bridge `/api/setup` with `benchmark-task-id`.
3. Run optional suite/task setup.
4. Capture `pre.jpg` directly from bridge `POST /api/providers/screenshot`.
5. Send the task prompt to agent `/api/chat`.
6. Capture `post.jpg` directly from bridge `POST /api/providers/screenshot`.
7. Extract structured tool trace from agent history.
8. Evaluate hard assertions.
9. If judge is enabled, submit rubric, trace, final response, and pre/post screenshots.
10. Persist task artifacts and release the bridge route through `/api/release`.

The runner does not call agent screenshot tools to create judge images. Judge
only receives `pre.jpg` and `post.jpg`; intermediate screenshots may exist in
agent history, but they are not the judge image input.

## Environment Bridge Routing

Concurrent environments route requests by the `benchmark-task-id` HTTP header.
The same id must be used for:

- `/api/setup`
- `/api/tools/<tool>`
- `/api/providers/screenshot`
- `/api/release`

WebUI and `run --auto-agent-setup` create one isolated agent daemon per active
task worker. The scheduler reads `/api/concurrent` from the bridge and runs at
most that many workers at once; extra tasks wait in the queue until a worker
releases its env.

## Scoring

### Offline metrics

Each attempt records `success`, `agent_eligible`, `failure_class`, and
`quality_score` in the machine-readable run artifacts. Setup failures,
unavailable providers, judge failures, and deliberate skips remain in raw
results and coverage counts but do not enter Agent success or efficiency
denominators. Missing telemetry is represented as `null`, not zero.

For a fixed planned repeat count `k` (the common planned prefix, normally the
smallest repeat count requested by the tasks), the runner considers only tasks
whose first `k` planned attempts are all present and eligible. It reports:

- `pass@1`: the fraction of eligible tasks whose first attempt succeeds;
- `pass@k`: the fraction with at least one success among those first `k`
  planned attempts;
- `pass^k`: the fraction whose first `k` planned attempts are all successful;
- `oracle_best_score@k`: the mean, across tasks with a recorded score in that
  cohort, of each task's maximum offline quality score within the first `k`
  attempts; and
- first-success attempt and cumulative cost-to-first-success, using tasks with
  a contiguous eligible attempt prefix.

`pass@1` uses tasks with an eligible first attempt. `pass@k`, `pass^k`, and
`oracle_best_score@k` use only the cohort whose first `k` planned attempts are
all present and eligible. First-success metrics use their own contiguous
eligible-prefix cohort rather than silently skipping invalid attempts.

Success rates include 95% Wilson confidence intervals, and eligible-task
coverage is reported separately. The metrics are recomputable from the raw
attempt results and are written to `metrics.json` alongside `summary.md`.

### Hard Assertions

Hard assertions run before LLM judge and are deterministic. They cover checks
such as:

- Required final response.
- Minimum/maximum tool calls.
- Required or forbidden tools.
- Timeout.
- Expected answer for deterministic QA tasks.
- Trace observation checks.

Hard assertion failures include the requirement and actual observed value in
the HTML report.

### LLM Judge

The judge uses OpenAI-compatible chat completions. Its input is:

- Task description.
- Rubric.
- Pre/post screenshots, when available.
- Structured tool trace.
- Agent final response.

The output is one yes/no verdict plus reason per rubric item. Results are
cached by screenshots, trace, rubric, final response, and model.

Agent, Judge, and Analysis runtime configuration use separate role-specific
namespaces: `AICO_BENCHMARK_AGENT_*`, `AICO_BENCHMARK_JUDGE_*`, and
`AICO_BENCHMARK_ANALYSIS_*`. Provider-specific environment variable names are
not part of the benchmark runtime contract.

## Artifacts

CLI run output:

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

WebUI job output:

```text
benchmark/runs/webui/<job_id>/
├── job.json
├── state.json
├── runner.log
├── daemon.log
├── raw/<run_id>/
└── workers/
```

WebUI persists `job.json`, so historical job records survive WebUI restarts.
If a job was running during restart, it is recovered as stopped.

## Design Decisions

### Why HTTP APIs?

- Agent daemon already exposes HTTP endpoints.
- Runner can execute against local or remote agents.
- Environment bridge lets real devices, MobileGym, and future environments use
  the same scheduling and artifact pipeline.

### Why Separate Execution And Scoring?

- Rubric and judge model changes can be rejudged offline.
- Judge failures do not require re-running device actions.
- Cached judge results reduce API cost.

### Why Isolated Daemon Workers?

For concurrent environment runs, each active task worker gets its own agent
daemon and config directory. This avoids conversation, memory, and log
cross-talk while still sharing the same bridge pool.

## Related Documentation

- [Quick Start](./README.md)
- [Detailed Guide](./quickstart.md)
- [Full Manual](../../benchmark/manual.md)

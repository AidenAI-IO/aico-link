# MobileGym Device CLI parity

The final comparison used `mobilegym_cli_parity`, two attempts per task. Both
variants passed **6/6 attempts** (0 percentage-point gap, within the 10-point
success-rate target on this sample). The three tasks are too few to establish a
10-point bound for other tasks or devices.

| Metric | Original direct tools | Device CLI + background UTG |
| --- | ---: | ---: |
| Attempts passed | 6/6 | 6/6 |
| Alarm count | 2/2 | 2/2 |
| Wi-Fi status | 2/2 | 2/2 |
| List swipe | 2/2 | 2/2 |
| Rubric items passed | 16/16 | 16/16 |
| Mean task wall time | 23.70 s | 23.90 s (+0.8%) |
| Median task wall time | 19.65 s | 22.79 s (+15.9%) |
| p90 task wall time | 38.94 s | 36.31 s (−6.7%) |

The 10-point target refers to task success, not latency. The existing benchmark
counters do not attribute nested HTTP device calls or shell screenshot states to
the same buckets as direct tools, so device-action and screenshot counts are not
comparable. Background UTG inference is not counted in foreground model usage.

## Setup and reproduction

- Original: `b7b59368d`. Candidate: `0089551b2`.
- MobileGym: submodule revision `d0549195beccbe08f963bf7758bef3a65fe3ac08`,
  matching the repository pin. Both ran the simulator via the standard bridge.
- Agent and judge: OpenRouter `qwen/qwen3-vl-235b-a22b-instruct`.
- Start a MobileGym environment as described in the [integration guide](README.md).
  Build and start each revision with its own fresh copy of `src/agent/config`,
  Android device type, and the environment bridge. Install `device-cli` next to
  the candidate daemon or on its `PATH`. Set the active OpenAI-compatible model
  provider `base_url` to `https://openrouter.ai/api/v1`, set its `api_key` to
  `$OPENROUTER_API_KEY` (as a variable reference), and use the model above. Give
  each daemon a `-benchmark-task-id` matching the runner invocation.

```sh
cd benchmark
AICO_BENCHMARK_JUDGE_API_KEY="$OPENROUTER_API_KEY" uv run python -m runner run \
  --suite suites/mobilegym_cli_parity.json \
  --agent-url "$AGENT_URL" --environment-url "$MOBILEGYM_URL" \
  --benchmark-task-id "$TASK_ROUTE" \
  --judge-base-url https://openrouter.ai/api/v1 \
  --judge-model qwen/qwen3-vl-235b-a22b-instruct \
  --repeats 2 --out runs/cli-utg --run-id "$RUN_ID"
```

The parity suite preserves the three basic prompts, rubrics, and time limits.
It drops the scroll task's requirement to invoke a literal `touch_gesture`
conversational tool, while keeping the simulator assertion that later rows
became visible. Both variants were evaluated with that same suite.

## Retained evidence

Full runner results, judge verdicts, and simulator observations are in the local
`benchmark/runs/cli-utg/parity-baseline-repeat/` and
`benchmark/runs/cli-utg/parity-final/` directories. The generated artifacts stay
untracked. Intermediate runs are also retained: one run failed because the model
called legacy tools despite the new schema, and a later run passed 2/3 because
it counted only enabled alarms. The final prompt removes the conflicting tool
rules and distinguishes total counts from counts of active items.

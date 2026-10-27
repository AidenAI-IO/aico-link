---
sidebar_position: 2
---

# SkillOpt Architecture

SkillOpt optimizes skill text by running the current skill, extracting failure
patterns, proposing a candidate patch, and accepting the candidate only if it
improves held-out validation tasks.

## Core Components

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

Relevant files:

| File | Responsibility |
| --- | --- |
| `skillopt/main.py` | CLI, argument validation, artifact writing, report rendering. |
| `skillopt/webui.py` | Standalone WebUI, job records, MobileGym environment controls. |
| `skillopt/orchestrator.py` | Main optimization loop and gate decisions. |
| `skillopt/benchmark_backend.py` | Bridge-backed rollout through `benchmark/runner`. |
| `skillopt/backends.py` | Direct existing-daemon rollout backend. |
| `skillopt/phase_artifacts.py` | SkillOpt-owned phase records for progress, reports, and failure reconstruction. |
| `skillopt/score.py` | Converts benchmark task results into hard and soft SkillOpt scores. |
| `skillopt/reflect.py` | Builds optimizer prompts from rollout failures. |
| `skillopt/aggregate.py` | Deduplicates and ranks proposed edits. |
| `skillopt/patch.py` | Applies candidate edits to skill text. |

## Optimization Flow

For a run with explicit train and validation suites, SkillOpt executes:

1. `baseline_selection`: run the original skill on held-out validation tasks.
2. `step_N_train`: run the current skill on train tasks.
3. `reflect`: ask the optimizer model for patch suggestions from rollout evidence.
4. `aggregate`: deduplicate and limit edits to `--edit-budget`.
5. `apply`: create `step_N/candidate.md`.
6. `step_N_selection`: run the candidate on held-out validation tasks.
7. `gate`: accept only if `candidate.hard > current.hard + min_delta`.
8. Persist `best_skill.md`, `diff.patch`, `result.json`, and `report.html`.

If no candidate beats the validation gate, the current best skill remains the
last accepted version.

`--budget` is the maximum number of optimization iterations. `--edit-budget` is
the maximum number of skill edits proposed in each iteration.

## Backends

### Direct Device Backend

`--backend device` without `--environment-url` uses `AicoDeviceBackend`. It
talks to an existing agent daemon through `/api/chat` and temporarily overrides
the skill text while each phase runs.

Use this mode for quick local debugging when you already have one daemon and do
not need bridge screenshots or concurrency.

### Benchmark Runner Backend

Any run with `--environment-url` uses `BenchmarkRunnerBackend`. This includes
`--backend mobilegym` and bridge-backed `--backend device` runs.

For every phase, SkillOpt launches a child benchmark run:

```text
skillopt/runs/<run_id>/benchmark/<run_id>-<phase>/
```

The child run uses:

- `runner.main run`
- `--auto-agent-setup`
- `--environment-url <bridge>`
- one isolated agent daemon worker per active task worker
- benchmark pre/post screenshots, trace extraction, judge, and HTML report

This is the preferred path for MobileGym and reproducible validation.

Child benchmark reports are raw execution evidence. SkillOpt stores their links
on phase records and renders them as `report` drilldowns in the top-level
SkillOpt report.

## Suites And Targets

SkillOpt-owned suites live under:

```text
skillopt/suites/<skill>/
```

The WebUI discovers suite pairs by filename:

```text
<prefix>_train.json
<prefix>_verification.json
<prefix>_validation.json
```

For example, these files become two selectable targets for `device-operator`:

```text
device_operator_train.json
device_operator_verification.json
shopping_scenario_train.json
shopping_scenario_verification.json
```

CLI labels use the logical form `skillopt/<skill>/<suite_stem>`, such as
`skillopt/device-operator/device_operator_train`.

Benchmark suites can also be passed through `--suite`; SkillOpt then splits the
suite 70/30 into train and selection tasks. SkillOpt suites are preferred
because they make train and held-out validation boundaries explicit.

## Scoring

SkillOpt converts every benchmark `TaskResult` into a rollout score:

| Score | Definition |
| --- | --- |
| `hard` | `1` if the task status is `passed`, else `0`. |
| `soft` | Rubric pass count divided by rubric total, or `hard` when no rubric exists. |

Phase score is the mean across tasks:

```text
hard = fully passed tasks / total tasks
soft = passed rubric items / total rubric items
```

The validation gate uses `hard` as the primary score. A candidate is accepted
only when it improves the current validation hard score by more than
`--min-delta`.

## Judge And Evidence

When judge is enabled, SkillOpt delegates task scoring to benchmark judge. For
bridge-backed runs, judge input is benchmark evidence:

- task prompt and rubric
- pre/post screenshots from environment bridge `POST /api/providers/screenshot`
- structured tool trace from agent history
- final response

Intermediate agent screenshots are not the benchmark judge image input. If a
task depends on visual verification, missing `pre.jpg` or `post.jpg` will make
the rubric less reliable.

`--no-judge` skips rubric judge and uses hard assertions only. This is useful
for fast traces, but it should not be treated as final validation.

## Artifacts

CLI output defaults to `skillopt/runs/<run_id>/`. WebUI output defaults to
`skillopt/runs/webui/<job_id>/`.

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

Use the top-level SkillOpt report to decide which skill won. Use linked child
benchmark reports to debug task-level failures.

Each `phases/*.json` file uses the `skillopt.phase.v1` schema and records the
phase name, kind, suite, status, task list, counts, score when available, error
when failed, and a benchmark report link when the backend produced one. The
WebUI reads these files to show current phase progress and best score without
depending on benchmark WebUI state.

If the optimizer exits with an exception after artifact setup, SkillOpt still
writes `manifest.json`, `result.json`, and `report.html`. Failed reports show
the error, phase table, artifacts that were produced, and the diff or best skill
when those files already exist.

## Environment Variables

| Variable | Meaning |
| --- | --- |
| `OPENROUTER_API_KEY` | Optimizer and judge API key. |
| `AICO_SKILLS_DIR` | Optional override for skill source directory. |
| `AICO_ENVIRONMENT_URL` | Default bridge endpoint for CLI runs. |
| `AICO_DAEMON_IMAGE` | Default daemon image for benchmark runner workers. |

## Design Decisions

### Why Separate SkillOpt From Benchmark?

Benchmark evaluates tasks and produces evidence. SkillOpt consumes that evidence
to optimize a skill. Keeping the dependency one-way prevents benchmark suite
discovery, reports, and WebUI state from being polluted by optimization-only
suites and candidate skills.

### Why Held-Out Validation?

Skill changes can overfit train failures. The selection phase reruns a candidate
on held-out tasks, and the gate rejects candidates that do not improve the
current validation hard score.

### Why Bridge-Backed Runs For MobileGym?

MobileGym concurrency, setup/release, pre/post screenshots, and isolated daemon
workers already exist in benchmark runner. Reusing that path gives SkillOpt the
same execution semantics and reports as normal benchmark runs.

## Related Documentation

- [Quickstart](./quickstart.md)
- [SkillOpt Overview](./README.md)
- [Benchmark Architecture](../09-benchmark/architecture.md)
- [Environment Bridge Protocol](../../benchmark/environment_bridge.md)

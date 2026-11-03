"""Run one native agent experiment in CI and summarize the paired results.

The workflow installs pinned external agent CLIs, prepares the daemon and
MobileGym images, and calls this driver. It starts a MobileGym environment,
writes the experiment configuration from workflow inputs, runs `runner
experiment`, and appends a Markdown summary for the job page.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
from typing import Any

from ci.plan import BENCHMARK_ROOT
from ci.run_case import ACTION_ENVIRONMENT_ALIASES


# External agents share the repository's benchmark model endpoint and key.
EXTERNAL_KINDS = {"pi", "openclaw", "claude-code", "codex"}
KEY_ENV = "AIDEN_BENCHMARK_PI_API_KEY"


def agent_configs(spec: str, *, provider: str, model: str, base_url: str) -> list[dict[str, Any]]:
    """Turn `aiden,pi,codex,pi-raw` into experiment agents; `-raw` selects raw tools."""
    agents = []
    for item in (part.strip() for part in spec.split(",")):
        if not item:
            continue
        kind, raw = (item[:-4], True) if item.endswith("-raw") else (item, False)
        if kind == "aiden":
            if raw:
                raise ValueError("Aiden always uses its own tool layer")
            agents.append({"id": "aiden", "kind": "aiden"})
        elif kind in EXTERNAL_KINDS:
            agents.append({"id": item, "kind": kind, "provider": provider, "model": model,
                           "base_url": base_url, "api_key_env": KEY_ENV,
                           "tool_layer": "raw" if raw else "aiden"})
        else:
            raise ValueError(f"Unknown agent {item!r}; use aiden, pi, openclaw, claude-code, codex, or <external>-raw")
    if not agents:
        raise ValueError("Select at least one agent")
    return agents


def start_mobilegym(runs_dir: Path, image: str, name: str, envs: int) -> dict[str, Any]:
    output = subprocess.run(
        [sys.executable, "-m", "runner", "start-mobilegym-env", "--runs-dir", str(runs_dir), "--name", name,
         "--envs", str(envs), "--mobilegym-image", image, "--no-build-mobilegym-image", "--json"],
        cwd=BENCHMARK_ROOT, check=True, capture_output=True, text=True,
    ).stdout
    return json.loads(output[output.index("{"):])


def summary_markdown(root: Path) -> str:
    manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
    data = json.loads((root / "comparison.json").read_text(encoding="utf-8"))
    lines = [f"## Agent experiment `{manifest['id']}`", "",
             f"Suite `{manifest['suite']}`, {manifest['repeats']} repeat(s), status **{manifest['status']}**.", "",
             f"Up to {manifest.get('concurrency', 1)} attempt(s) ran in parallel.", "",
             "| Agent | Tool layer | Success | Coverage | Median execution | Timeouts | Retried attempts |",
             "| --- | --- | --- | --- | --- | --- | --- |"]
    for agent in data["agents"]:
        rate = f"{agent['success_rate']:.0%}" if agent["success_rate"] is not None else "—"
        wall = f"{agent['median_wall_ms'] / 1000:.1f}s" if agent["median_wall_ms"] is not None else "—"
        lines.append(f"| {agent['id']} | {agent.get('tool_layer', 'aiden')} | {agent['passed']}/{agent['eligible']} "
                     f"({rate}) | {agent['coverage']:.0%} | {wall} | {agent['timeouts']} | "
                     f"{agent.get('api_retry_attempts', 0)} |")
    lines += ["", "Matched pairs compare only attempts where both agents were eligible.", "",
              "| Pair | Eligible pairs | Both pass | Left only | Right only |", "| --- | --- | --- | --- | --- |"]
    for pair in data["pairwise"]:
        lines.append(f"| {' vs '.join(pair['agent_ids'])} | {pair['eligible_pairs']}/{pair['planned_pairs']} | "
                     f"{pair['both_pass']} | {pair['left_only_pass']} | {pair['right_only_pass']} |")
    return "\n".join(lines) + "\n"


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run a native agent experiment in CI")
    parser.add_argument("--suite", required=True)
    parser.add_argument("--agents", required=True, help="Comma-separated: aiden, pi, openclaw, claude-code, codex, <external>-raw")
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--seed", type=int, default=0)
    parser.add_argument("--concurrency", type=int, default=3,
                        help="Parallel attempts; starts this many MobileGym environments")
    parser.add_argument("--runs-dir", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--daemon-image", required=True)
    parser.add_argument("--mobilegym-image", required=True)
    args = parser.parse_args(argv)
    # Workflow variables use the benchmark workflow's names; the runner reads the
    # AICO_* names for Aiden's config template and the Judge credential.
    for action_name, runtime_name in ACTION_ENVIRONMENT_ALIASES.items():
        if os.environ.get(action_name):
            os.environ.setdefault(runtime_name, os.environ[action_name])

    agents = agent_configs(args.agents, provider=os.environ["BENCHMARK_AGENT_PROVIDER"],
                           model=os.environ["BENCHMARK_AGENT_MODEL"], base_url=os.environ["BENCHMARK_AGENT_BASE_URL"])
    if any(agent["kind"] != "aiden" for agent in agents) and not os.environ.get(KEY_ENV):
        raise SystemExit(f"{KEY_ENV} is required for external agents")
    runs_dir = args.runs_dir.resolve()
    runs_dir.mkdir(parents=True, exist_ok=True)
    environment = start_mobilegym(runs_dir, args.mobilegym_image, args.run_id, args.concurrency)
    config = {"suite": args.suite.removeprefix("suites/"), "environment_url": environment["environment_url"],
              "agents": agents, "repeats": args.repeats, "seed": args.seed, "concurrency": args.concurrency}
    config_path = runs_dir / "experiment-config.json"
    config_path.write_text(json.dumps(config, indent=2), encoding="utf-8")
    try:
        status = subprocess.run(
            # Aiden's agent.toml is rendered from the benchmark template and AICO_* model variables.
            [sys.executable, "-m", "runner", "experiment", "--config", str(config_path), "--out", str(runs_dir),
             "--agent-config", str(runs_dir / "experiment-agent.toml"),
             "--daemon-image", args.daemon_image, "--no-build-daemon-image",
             "--judge-model", os.environ["BENCHMARK_JUDGE_MODEL"],
             "--judge-base-url", os.environ["BENCHMARK_JUDGE_BASE_URL"]],
            cwd=BENCHMARK_ROOT,
            env={**os.environ, "AICO_BENCHMARK_EXPERIMENT_RUN_ID": args.run_id},
        ).returncode
    finally:
        subprocess.run(["docker", "rm", "-f", environment["container_name"]], capture_output=True)
    roots = sorted((runs_dir / "experiments").glob("exp-*/comparison.json"))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if roots and summary:
        with open(summary, "a", encoding="utf-8") as stream:
            stream.write(summary_markdown(roots[-1].parent))
    return status


if __name__ == "__main__":
    raise SystemExit(cli())

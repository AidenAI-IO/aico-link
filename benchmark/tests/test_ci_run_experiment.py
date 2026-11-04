import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import textwrap

import pytest

from ci import run_experiment
from runner.harnesses import validate_agents


def workflow_script(name):
    workflow = (Path(__file__).resolve().parents[2] / ".github/workflows/agent-experiment.yml").read_text()
    step = re.search(rf"(?ms)^      - name: {re.escape(name)}\n(.*?)(?=^      - |\Z)", workflow).group(1)
    return textwrap.dedent(step.split("        run: |\n", 1)[1])


@pytest.mark.parametrize("spec,kinds", [
    ("aiden, pi", ["pi"]),
    (" pi , pi-raw, codex-raw , openclaw , claude-code-raw ", ["pi", "openclaw", "claude-code", "codex"]),
    (" aiden , ", []),
])
def test_workflow_installs_selected_clis_with_whitespace_and_raw_layers(tmp_path, spec, kinds):
    # Exercise the actual shell step; only replace package installation and uv.
    shim = tmp_path / "bin"
    shim.mkdir()
    uv = shim / "uv"
    uv.write_text(f'#!/bin/sh\nshift\nshift\nexec {shlex.quote(sys.executable)} "$@"\n')
    uv.chmod(0o755)
    npm = shim / "npm"
    npm.write_text(f'#!{sys.executable}\nimport json, os, sys\nfrom pathlib import Path\n'
                   'Path(os.environ["NPM_ARGS"]).write_text(json.dumps(sys.argv[1:]))\n')
    npm.chmod(0o755)
    package_vars = {"PI_PACKAGE": "pi@1", "OPENCLAW_PACKAGE": "openclaw@1",
                    "CLAUDE_CODE_PACKAGE": "claude-code@1", "CODEX_PACKAGE": "codex@1"}
    args_path = tmp_path / "npm-args.json"
    subprocess.run(["bash", "-c", workflow_script("Install external agent CLIs")], check=True,
                   cwd=run_experiment.BENCHMARK_ROOT,
                   env={**os.environ, **package_vars, "AGENTS": spec, "RUNNER_TEMP": str(tmp_path),
                        "GITHUB_ENV": str(tmp_path / "github-env"), "NPM_ARGS": str(args_path),
                        "PATH": str(shim) + os.pathsep + os.environ["PATH"]})
    if kinds:
        assert json.loads(args_path.read_text())[6:] == [f"{kind}@1" for kind in kinds]
    else:
        assert not args_path.exists()


def test_ci_driver_passes_run_ownership_to_runner(tmp_path, monkeypatch):
    for name in run_experiment.ACTION_ENVIRONMENT_ALIASES.values():
        monkeypatch.setenv(name, "test")
    for name in ("BENCHMARK_AGENT_PROVIDER", "BENCHMARK_AGENT_MODEL", "BENCHMARK_AGENT_BASE_URL",
                 "BENCHMARK_JUDGE_MODEL", "BENCHMARK_JUDGE_BASE_URL"):
        monkeypatch.setenv(name, "test")
    monkeypatch.setattr(run_experiment, "start_mobilegym", lambda *args: {
        "environment_url": "http://env", "container_name": "owned-env"})
    calls = []
    def run(command, **kwargs):
        calls.append((command, kwargs))
        return subprocess.CompletedProcess(command, 0)
    monkeypatch.setattr(run_experiment.subprocess, "run", run)
    assert run_experiment.cli(["--suite", "suite.json", "--agents", "aiden", "--runs-dir", str(tmp_path),
                               "--run-id", "ci-exp-123-1", "--daemon-image", "daemon", "--mobilegym-image", "env"]) == 0
    assert calls[0][1]["env"]["AICO_BENCHMARK_EXPERIMENT_RUN_ID"] == "ci-exp-123-1"
    assert calls[1][0] == ["docker", "rm", "-f", "owned-env"]


def test_workflow_cleanup_only_removes_current_run_containers(tmp_path):
    shim = tmp_path / "docker"
    shim.write_text(f'#!{sys.executable}\n' + textwrap.dedent('''\
        import json, os, re, sys
        from pathlib import Path
        args = sys.argv[1:]
        if args[0] == "ps":
            pattern = args[args.index("--filter") + 1].removeprefix("name=")
            for name in json.loads(os.environ["CONTAINERS"]):
                if re.search(pattern, "/" + name):
                    print(name)
        elif args[:2] == ["rm", "-f"]:
            with Path(os.environ["REMOVED"]).open("a") as stream:
                stream.write(args[2] + "\\n")
        else:
            sys.exit(1)
        '''))
    shim.chmod(0o755)
    prefix = "aico-benchmark-agent-experiment-"
    owned = ["aico-mobilegym-env-mobilegym-ci-exp-123-1",
             prefix + "ci-exp-123-1-abcdef0123456789-daemon-1",
             prefix + "ci-exp-123-1-fedcba9876543210-daemon-1"]
    unrelated = ["aico-mobilegym-env-mobilegym-ci-exp-123-10",
                 "aico-mobilegym-env-mobilegym-ci-exp-456-1",
                 prefix + "ci-exp-123-2-abcdef0123456789-daemon-1",
                 prefix + "ci-exp-123-10-abcdef0123456789-daemon-1",
                 prefix + "ci-exp-456-1-abcdef0123456789-daemon-1",
                 prefix + "abcdef0123456789-daemon-1"]
    removed = tmp_path / "removed"
    subprocess.run(["bash", "-c", workflow_script("Clean up experiment services")], check=True,
                   env={**os.environ, "PATH": str(tmp_path) + os.pathsep + os.environ["PATH"],
                        "RUN_ID": "ci-exp-123-1", "CONTAINERS": json.dumps(owned + unrelated),
                        "REMOVED": str(removed)})
    assert removed.read_text().splitlines() == owned


def test_agent_spec_shares_the_benchmark_endpoint_and_selects_raw_tools():
    agents = run_experiment.agent_configs("aiden, pi,codex , pi-raw", provider="openrouter",
                                          model="google/gemini-3.1-flash-lite", base_url="https://openrouter.ai/api/v1")
    assert [(agent["id"], agent.get("tool_layer", "aiden")) for agent in agents] == [
        ("aiden", "aiden"), ("pi", "aiden"), ("codex", "aiden"), ("pi-raw", "raw")]
    assert {agent.get("api_key_env") for agent in agents[1:]} == {run_experiment.KEY_ENV}
    # The generated configuration must pass the experiment's own validation.
    assert [agent["api"] for agent in validate_agents(agents)[1:]] == [
        "openai-completions", "openai-responses", "openai-completions"]


@pytest.mark.parametrize("spec,message", [("aiden-raw", "own tool layer"), ("gemini", "Unknown agent"), (" , ", "at least one")])
def test_agent_spec_rejects_invalid_selections(spec, message):
    with pytest.raises(ValueError, match=message):
        run_experiment.agent_configs(spec, provider="p", model="m", base_url="https://g.example/v1")


def test_summary_reports_each_agent_layer_and_matched_pairs(tmp_path: Path):
    (tmp_path / "manifest.json").write_text(json.dumps({"id": "exp-1", "suite": "phone_control_v1",
                                                        "repeats": 3, "status": "completed", "concurrency": 3}))
    (tmp_path / "comparison.json").write_text(json.dumps({"agents": [
        {"id": "aiden", "tool_layer": "aiden", "passed": 9, "eligible": 10, "success_rate": .9, "coverage": .8,
         "median_wall_ms": 31000, "timeouts": 0, "api_retry_attempts": 1},
        {"id": "pi-raw", "tool_layer": "raw", "passed": 0, "eligible": 0, "success_rate": None, "coverage": 0,
         "median_wall_ms": None, "timeouts": 2},
    ], "pairwise": [{"agent_ids": ["aiden", "pi-raw"], "eligible_pairs": 0, "planned_pairs": 12,
                     "both_pass": 0, "left_only_pass": 0, "right_only_pass": 0}]}))
    summary = run_experiment.summary_markdown(tmp_path)
    assert "Up to 3 attempt(s) ran in parallel." in summary
    assert "| aiden | aiden | 9/10 (90%) | 80% | 31.0s | 0 | 1 |" in summary
    assert "| pi-raw | raw | 0/0 (—) | 0% | — | 2 | 0 |" in summary
    assert "| aiden vs pi-raw | 0/12 | 0 | 0 | 0 |" in summary

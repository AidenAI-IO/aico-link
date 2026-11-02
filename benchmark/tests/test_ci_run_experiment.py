import json
from pathlib import Path

import pytest

from ci import run_experiment
from runner.harnesses import validate_agents


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
                                                        "repeats": 3, "status": "completed"}))
    (tmp_path / "comparison.json").write_text(json.dumps({"agents": [
        {"id": "aiden", "tool_layer": "aiden", "passed": 9, "eligible": 10, "success_rate": .9, "coverage": .8,
         "median_wall_ms": 31000, "timeouts": 0, "api_retry_attempts": 1},
        {"id": "pi-raw", "tool_layer": "raw", "passed": 0, "eligible": 0, "success_rate": None, "coverage": 0,
         "median_wall_ms": None, "timeouts": 2},
    ], "pairwise": [{"agent_ids": ["aiden", "pi-raw"], "eligible_pairs": 0, "planned_pairs": 12,
                     "both_pass": 0, "left_only_pass": 0, "right_only_pass": 0}]}))
    summary = run_experiment.summary_markdown(tmp_path)
    assert "| aiden | aiden | 9/10 (90%) | 80% | 31.0s | 0 | 1 |" in summary
    assert "| pi-raw | raw | 0/0 (—) | 0% | — | 2 | 0 |" in summary
    assert "| aiden vs pi-raw | 0/12 | 0 | 0 | 0 |" in summary

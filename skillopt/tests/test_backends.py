from contextlib import contextmanager
from pathlib import Path

from runner.models import TaskResult
from runner.suite import HardAssertions, Suite, TaskSpec
from skillopt import backends


def test_aico_device_backend_overrides_when_skill_text_differs(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "SKILL.md"
    skill_path.write_text("base", encoding="utf-8")
    suite = Suite(
        name="suite",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="task",
        category="single_step",
        description_for_judge="desc",
        prompt="prompt",
        rubric=[],
        hard_assertions=HardAssertions(),
    )
    events = []

    class FakeClient:
        def __init__(self, base_url: str):
            self.base_url = base_url

        def close(self):
            events.append(("close", self.base_url))

    @contextmanager
    def fake_skill_override(client, got_skill_path, candidate):
        events.append(("override", client.base_url, got_skill_path, candidate))
        yield

    def fake_run_one_task(**kwargs):
        events.append(("run", kwargs["artifact_dir"], kwargs.get("active_skills")))
        return TaskResult(
            suite=kwargs["suite"].name,
            run_id=kwargs["run_id"],
            task_id=kwargs["task"].id,
            category=kwargs["task"].category,
            attempt=kwargs["attempt"],
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(kwargs["artifact_dir"]),
            description_for_judge=kwargs["task"].description_for_judge,
        )

    monkeypatch.setattr(backends, "AgentClient", FakeClient)
    monkeypatch.setattr(backends, "with_skill_override", fake_skill_override)
    monkeypatch.setattr(backends, "run_one_task", fake_run_one_task)

    backend = backends.AicoDeviceBackend(agent_url="http://agent.local")
    rollouts = backend.run_rollout(
        suite=suite,
        tasks=[task],
        skill_name="device-operator",
        skill_path=skill_path,
        skill_text="candidate",
        phase="phase",
        run_id="run-1",
        run_root=tmp_path / "runs",
        judge_cfg=None,
    )
    backend.close()

    assert rollouts[0].hard == 1
    assert events == [
        ("override", "http://agent.local", skill_path, "candidate"),
        ("run", tmp_path / "runs" / "phase" / "task", ["device-operator"]),
        ("close", "http://agent.local"),
    ]

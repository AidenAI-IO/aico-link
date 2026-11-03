from pathlib import Path
import json
import typing

import pytest

from skillopt import main
from skillopt.types import Edit, OptimizationResult, PhaseSummary, ScoreSummary, StepDecision


TRAIN_LABEL = "skillopt/device-operator/device_operator_train"
VERIFICATION_LABEL = "skillopt/device-operator/device_operator_verification"


class FakeDeviceBackend:
    def __init__(self, *, agent_url: str):
        self.agent_url = agent_url

    def close(self):
        pass


class FakeBenchmarkRunnerBackend:
    def __init__(self, **kwargs):
        self.kwargs = kwargs

    def close(self):
        pass


def _set_roots(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(main, "REPO_ROOT", tmp_path)
    monkeypatch.setattr(main, "SKILLOPT_ROOT", tmp_path / "skillopt")


def test_resolve_skill_path_uses_packaged_board_skills(monkeypatch, tmp_path: Path):
    monkeypatch.delenv("AICO_SKILLS_DIR", raising=False)
    skill_path = tmp_path / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("---\nname: device-operator\n---\n", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)

    assert main._resolve_skill_path("device-operator") == skill_path


def test_resolve_skill_path_prefers_env_skills_dir(monkeypatch, tmp_path: Path):
    repo_skill = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    repo_skill.parent.mkdir(parents=True)
    repo_skill.write_text("repo", encoding="utf-8")
    env_root = tmp_path / "custom-skills"
    env_skill = env_root / "device-operator" / "SKILL.md"
    env_skill.parent.mkdir(parents=True)
    env_skill.write_text("env", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setenv("AICO_SKILLS_DIR", str(env_root))

    assert main._resolve_skill_path("device-operator") == env_skill


def test_json_for_script_escapes_unicode_line_separators():
    rendered = main._json_for_script({"text": "a\u2028b\u2029c</script>"})

    assert "\u2028" not in rendered
    assert "\u2029" not in rendered
    assert "\\u2028" in rendered
    assert "\\u2029" in rendered
    assert "<\\/script>" in rendered


def _write_suite(path: Path, name: str, task_ids: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({
        "name": name,
        "tasks": [
            {
                "id": task_id,
                "category": "single_step",
                "description_for_judge": f"Judge {task_id}",
                "prompt": f"Run {task_id}",
                "rubric": [{"id": "ok", "check": "Task succeeds."}],
                "hard_assertions": {"min_tool_calls": 0, "max_tool_calls": 5},
            }
            for task_id in task_ids
        ],
    }), encoding="utf-8")


def _write_device_operator_suites(tmp_path: Path) -> None:
    _write_suite(
        tmp_path / "skillopt" / "suites" / "device-operator" / "device_operator_train.json",
        "device_operator_train",
        ["train_one", "train_two"],
    )
    _write_suite(
        tmp_path / "skillopt" / "suites" / "device-operator" / "device_operator_verification.json",
        "device_operator_verification",
        ["validation_one", "validation_two"],
    )


def test_cli_uses_explicit_train_and_selection_suites(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    captured = {}

    def fake_optimize_skill(cfg):
        captured["train_ids"] = [task.id for task in cfg.train_tasks]
        captured["selection_ids"] = [task.id for task in cfg.selection_tasks]
        captured["suite_name"] = cfg.suite.name
        captured["selection_suite_name"] = cfg.selection_suite.name
        captured["rollout_backend"] = cfg.rollout_backend
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--output", str(tmp_path / "optimized.md"),
    ])

    assert rc == 0
    assert captured == {
        "train_ids": ["train_one", "train_two"],
        "selection_ids": ["validation_one", "validation_two"],
        "suite_name": "device_operator_train",
        "selection_suite_name": "device_operator_verification",
        "rollout_backend": captured["rollout_backend"],
    }
    assert isinstance(captured["rollout_backend"], FakeDeviceBackend)
    assert captured["rollout_backend"].agent_url == "http://localhost:8080"


def test_cli_writes_run_artifacts_for_web_report(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    artifact_root = tmp_path / "skillopt" / "runs"

    def fake_optimize_skill(cfg):
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.25,
            best_score=0.75,
            best_skill="optimized skill\n",
            steps=[
                StepDecision(
                    step=1,
                    candidate_score=0.75,
                    current_score=0.25,
                    accepted=True,
                    reason="candidate improved",
                    edits_applied=[Edit(op="append", content="new rule")],
                )
            ],
            accepted_count=1,
            rejected_count=0,
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--backend", "device",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--artifact-root", str(artifact_root),
        "--run-id", "skillopt-test-run",
        "--output", str(artifact_root / "skillopt-test-run" / "best_skill.md"),
    ])

    assert rc == 0
    run_dir = artifact_root / "skillopt-test-run"
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    result = json.loads((run_dir / "result.json").read_text(encoding="utf-8"))
    assert manifest["mode"] == "skillopt"
    assert manifest["backend"] == "device"
    assert manifest["run_id"] == "skillopt-test-run"
    assert manifest["skill"] == "device-operator"
    assert manifest["totals"] == {"tasks": 2, "passed": 2, "failed": 0}
    assert result["initial_score"] == 0.25
    assert result["best_score"] == 0.75
    assert "-original skill" in (run_dir / "diff.patch").read_text(encoding="utf-8")
    assert "+optimized skill" in (run_dir / "diff.patch").read_text(encoding="utf-8")
    assert "SkillOpt Report" in (run_dir / "report.html").read_text(encoding="utf-8")


def test_cli_returns_zero_when_optimization_completes_without_improvement(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    artifact_root = tmp_path / "skillopt" / "runs"

    def fake_optimize_skill(cfg):
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=1.0,
            best_score=1.0,
            best_skill="original skill\n",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--backend", "device",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--artifact-root", str(artifact_root),
        "--run-id", "skillopt-no-improvement",
        "--output", str(artifact_root / "skillopt-no-improvement" / "best_skill.md"),
    ])

    assert rc == 0
    assert (artifact_root / "skillopt-no-improvement" / "best_skill.md").read_text(encoding="utf-8") == "original skill\n"


def test_cli_prints_clear_iteration_and_edit_limits(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    _write_device_operator_suites(tmp_path)

    def fake_optimize_skill(cfg):
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=1.0,
            best_score=1.0,
            best_skill="original skill\n",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--backend", "device",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--budget", "5",
        "--edit-budget", "3",
        "--output", str(tmp_path / "best_skill.md"),
    ])

    out = capsys.readouterr().out
    assert rc == 0
    assert "Max iterations: 5" in out
    assert "Max edits / iteration: 3" in out
    assert "Budget:" not in out
    assert "edits/step" not in out


def test_cli_writes_failure_report_when_optimization_raises(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    artifact_root = tmp_path / "skillopt" / "runs"

    def fake_optimize_skill(cfg):
        run_dir = cfg.artifact_root / cfg.run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "best_skill.md").write_text("best accepted skill\n", encoding="utf-8")
        step_dir = run_dir / "step_01"
        step_dir.mkdir(parents=True)
        (step_dir / "decision.json").write_text(json.dumps({
            "accepted": False,
            "reason": "candidate hard 0.250 not better than current 0.500",
            "candidate_score": 0.25,
            "current_score": 0.50,
        }), encoding="utf-8")
        (step_dir / "patch.json").write_text(json.dumps({
            "reasoning": "optimizer found a retry loop",
            "edits": [
                {
                    "op": "append",
                    "content": "Retry failed actions with changed parameters.",
                    "support_count": 2,
                    "source_type": "failure",
                }
            ],
        }), encoding="utf-8")
        (step_dir / "patch_reports.json").write_text(json.dumps([
            {"op": "append", "status": "applied_append", "index": 1}
        ]), encoding="utf-8")
        phase_dir = cfg.artifact_root / cfg.run_id / "phases"
        phase_dir.mkdir(parents=True)
        (phase_dir / "step_01_train.json").write_text(json.dumps({
            "schema": "skillopt.phase.v1",
            "phase": "step_01_train",
            "kind": "train",
            "suite_name": "device_operator_train",
            "status": "failed",
            "counts": {"total": 1, "queued": 0, "passed": 0, "failed": 0, "skipped": 1, "error": 1},
            "tasks": [
                {
                    "id": "train_one",
                    "category": "single_step",
                    "status": "skipped",
                    "turns": 0,
                    "reason": "setup endpoint failed",
                    "raw_report": "benchmark/raw/report.html",
                }
            ],
            "error": "environment setup failed",
            "raw_report": "benchmark/raw/report.html",
        }), encoding="utf-8")
        raise RuntimeError("environment setup failed for 1/1 task results")

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--backend", "device",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--artifact-root", str(artifact_root),
        "--run-id", "skillopt-failed-run",
        "--output", str(artifact_root / "skillopt-failed-run" / "best_skill.md"),
    ])

    run_dir = artifact_root / "skillopt-failed-run"
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    result = json.loads((run_dir / "result.json").read_text(encoding="utf-8"))
    report = (run_dir / "report.html").read_text(encoding="utf-8")
    assert rc == 1
    assert manifest["status"] == "failed"
    assert manifest["artifacts"]["best_skill"] == "best_skill.md"
    assert manifest["artifacts"]["diff"] == "diff.patch"
    assert "environment setup failed for 1/1" in manifest["error"]
    assert result["error"] == manifest["error"]
    assert "SkillOpt Report" in report
    assert "best_skill.md" in report
    assert "diff.patch" in report
    assert "Diff" in report
    assert len(result["steps"]) == 1
    assert result["steps"][0]["step"] == 1
    assert result["steps"][0]["reason"] == "candidate hard 0.250 not better than current 0.500"
    assert "candidate hard 0.250 not better than current 0.500" in report
    assert "View edits" in report
    assert 'data-edit-step="1"' in report
    assert "optimizer found a retry loop" in report
    assert "Retry failed actions with changed parameters." in report
    assert "Run failed before final edit summary" not in report
    assert "drawer-backdrop" in report
    assert "function openArtifactDrawer" in report
    assert "environment setup failed" in report
    assert "Task Records" not in report
    assert "train_one" not in report


def test_failure_report_uses_same_report_shell_as_success_report():
    manifest = {
        "run_id": "skillopt-failed-run",
        "skill": "device-operator",
        "train_suite": TRAIN_LABEL,
        "validation_suite": VERIFICATION_LABEL,
        "error": "environment setup failed",
        "phase_records": [],
        "artifacts": {"result": "result.json"},
    }

    report = main._render_failure_report_html(
        manifest,
        original_skill="original skill\n",
        diff_text="-old\n+new\n",
    )

    assert "This is the SkillOpt-owned optimization timeline" in report
    assert report.count("<h2>SkillOpt Phases</h2>") == 1
    assert "<h2>Scores</h2>" not in report
    assert "<h2>Steps</h2>" in report
    assert "<h2>Edits</h2>" in report
    assert "<strong>Status:</strong> failed" in report
    assert "<strong>Initial score:</strong> 0.000 <strong>Best score:</strong> 0.000" in report
    assert "<strong>Stop reason:</strong> environment setup failed" in report
    assert ".detail-btn{" in report
    assert ".edit-summary{" in report
    assert "@media (max-width:768px)" in report
    assert "h1{font-size:24px" in report


def test_failure_partial_result_reconstructs_scores_from_phase_records(tmp_path: Path):
    cfg = main.OptimizationConfig(
        skill_name="device-operator",
        skill_path=tmp_path / "SKILL.md",
        suite=object(),
        train_tasks=[],
        selection_tasks=[],
        artifact_root=tmp_path / "runs",
        run_id="skillopt-failed-with-scores",
        optimizer_cfg=main.OptimizerConfig(),
    )
    run_dir = cfg.artifact_root / cfg.run_id
    phases_dir = run_dir / "phases"
    phases_dir.mkdir(parents=True)
    (phases_dir / "baseline_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "baseline_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "counts": {"total": 6, "passed": 2, "failed": 1, "error": 3},
        "tasks": [],
        "score": {"hard": 1 / 3, "soft": 0.389, "n": 6, "n_passed": 2},
    }), encoding="utf-8")
    (phases_dir / "step_01_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "step_01_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "counts": {"total": 6, "passed": 3, "failed": 0, "error": 3},
        "tasks": [],
        "score": {"hard": 0.5, "soft": 0.5, "n": 6, "n_passed": 3},
    }), encoding="utf-8")

    result = main._failure_partial_result(cfg, run_dir, "original skill\n", "environment setup failed")
    report = main._render_failure_report_html(
        {
            "skill": "device-operator",
            "train_suite": TRAIN_LABEL,
            "validation_suite": VERIFICATION_LABEL,
            "error": "environment setup failed",
            "phase_records": main.load_phase_records(run_dir),
            "artifacts": {"result": "result.json"},
        },
        original_skill="original skill\n",
        result=result,
    )

    assert result.initial_score == pytest.approx(1 / 3)
    assert result.best_score == pytest.approx(0.5)
    assert "<strong>Initial score:</strong> 0.333 <strong>Best score:</strong> 0.500" in report


def test_cli_writes_aggregated_skillopt_summary_report(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    artifact_root = tmp_path / "skillopt" / "runs"

    train_score = ScoreSummary(hard=0.25, soft=0.50, n=4, n_passed=1)
    candidate_score = ScoreSummary(hard=0.75, soft=0.875, n=4, n_passed=3)
    result = OptimizationResult(
        skill_name="device-operator",
        initial_score=0.50,
        best_score=0.75,
        best_skill="optimized skill\n",
        phase_summaries=[
            PhaseSummary(
                phase="baseline_selection",
                kind="verification",
                suite_name="device_operator_verification",
                score=ScoreSummary(hard=0.50, soft=0.75, n=4, n_passed=2),
            ),
            PhaseSummary(
                phase="step_01_train",
                kind="train",
                suite_name="device_operator_train",
                score=train_score,
            ),
            PhaseSummary(
                phase="step_01_selection",
                kind="verification",
                suite_name="device_operator_verification",
                score=candidate_score,
            ),
        ],
        steps=[
            StepDecision(
                step=1,
                candidate_score=0.75,
                current_score=0.50,
                accepted=True,
                reason="candidate improved",
                edits_applied=[Edit(op="append", content="new rule")],
                train_score=train_score,
                candidate_selection_score=candidate_score,
                patch_reasoning="failure analyst found missing launch guidance\nSecond line should wrap cleanly",
            )
        ],
        accepted_count=1,
        rejected_count=0,
    )
    cfg = main.OptimizationConfig(
        skill_name="device-operator",
        skill_path=skill_path,
        suite=object(),
        train_tasks=[object()] * 4,
        selection_tasks=[object()] * 4,
        artifact_root=artifact_root,
        run_id="skillopt-summary-run",
        optimizer_cfg=main.OptimizerConfig(),
    )
    child_report = artifact_root / "skillopt-summary-run" / "benchmark" / "skillopt-summary-run-step_01_train" / "report.html"
    child_report.parent.mkdir(parents=True)
    child_report.write_text("<html>child benchmark report</html>", encoding="utf-8")

    main._write_web_artifacts(
        cfg=cfg,
        result=result,
        original_skill="original skill\n",
        diff_text="-original skill\n+optimized skill\n",
        optimizer_model="optimizer",
        judge_model="judge",
        train_suite_label=TRAIN_LABEL,
        selection_suite_label=VERIFICATION_LABEL,
        backend="mobilegym",
    )

    run_dir = artifact_root / "skillopt-summary-run"
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["score_summary"]["baseline_verification"]["hard"] == 0.50
    assert manifest["score_summary"]["latest_train"]["hard"] == 0.25
    assert manifest["score_summary"]["best_verification"]["hard"] == 0.75
    assert manifest["linked_reports"]["step_01_train"] == "benchmark/skillopt-summary-run-step_01_train/report.html"

    report = (run_dir / "report.html").read_text(encoding="utf-8")
    assert report.count("<h2>SkillOpt Phases</h2>") == 1
    assert "<h2>Scores</h2>" not in report
    assert "baseline_selection" in report
    assert "step_01_train" in report
    assert "0.25" in report
    assert "0.75" in report
    assert "failure analyst found missing launch guidance" in report
    assert "Second line should wrap cleanly" in report
    assert "View edits" in report
    assert 'data-edit-step="1"' in report
    assert "editDetailData" in report
    assert "function openEditDrawer" in report
    assert "pre.skillopt-reasoning" not in report
    assert '<pre class="skillopt-reasoning">' not in report
    assert '<pre class="skillopt-edits">' not in report
    assert "new rule" in report
    assert "best_skill.md" in report
    assert "diff.patch" in report
    assert 'href="best_skill.md"' in report
    assert 'href="diff.patch"' in report
    assert 'href="result.json"' in report
    assert 'class="artifact-link"' in report
    assert 'data-artifact-url="best_skill.md"' in report
    assert 'drawer-backdrop' in report
    assert 'class="drawer"' in report
    assert "function openArtifactDrawer" in report
    assert "benchmark/skillopt-summary-run-step_01_train/report.html" in report


def test_web_report_shows_raw_mobilegym_and_skillopt_scores_for_no_edit_run(tmp_path: Path):
    artifact_root = tmp_path / "skillopt" / "runs"
    run_id = "skillopt-no-edit-run"
    raw_dir = artifact_root / run_id / "benchmark" / f"{run_id}-step_01_train"
    raw_dir.mkdir(parents=True)
    (raw_dir / "manifest.json").write_text(json.dumps({
        "totals": {
            "tasks": 12,
            "passed": 11,
            "failed": 0,
            "skipped": 0,
            "judge_error": 1,
            "timeout": 0,
        }
    }), encoding="utf-8")
    cfg = main.OptimizationConfig(
        skill_name="device-operator",
        skill_path=tmp_path / "SKILL.md",
        suite=object(),
        train_tasks=[object()] * 12,
        selection_tasks=[object()] * 6,
        artifact_root=artifact_root,
        run_id=run_id,
        optimizer_cfg=main.OptimizerConfig(),
    )
    result = OptimizationResult(
        skill_name="device-operator",
        initial_score=5 / 6,
        best_score=5 / 6,
        best_skill="best skill\n",
        phase_summaries=[
            PhaseSummary(
                phase="step_01_train",
                kind="train",
                suite_name="device_operator_train",
                score=ScoreSummary(hard=10 / 12, soft=10 / 12, n=12, n_passed=10),
            )
        ],
        stop_reason="step 1: aggregate produced 0 edits after dedup",
    )

    main._write_web_artifacts(
        cfg=cfg,
        result=result,
        original_skill="best skill\n",
        diff_text="",
        optimizer_model="optimizer",
        judge_model="judge",
        train_suite_label=TRAIN_LABEL,
        selection_suite_label=VERIFICATION_LABEL,
        backend="mobilegym",
    )

    run_dir = artifact_root / run_id
    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["raw_score_summary"]["step_01_train"] == {
        "passed": 11,
        "tasks": 12,
        "failed": 0,
        "error": 1,
        "pass_rate": 11 / 12,
    }
    report = (run_dir / "report.html").read_text(encoding="utf-8")
    assert "MobileGym result" not in report
    assert "Optimization score" not in report
    assert "11/12" in report
    assert "10/12" in report
    assert "Task pass rate" not in report
    assert "Rubric pass rate" not in report
    assert "Hard" not in report
    assert "Soft" not in report
    assert "Step 1" in report
    assert "stopped before candidate verification" in report
    assert "aggregate produced 0 edits after dedup" in report


def test_web_report_keeps_task_records_out_of_summary_html(tmp_path: Path):
    artifact_root = tmp_path / "skillopt" / "runs"
    run_id = "skillopt-phase-report"
    run_dir = artifact_root / run_id
    (run_dir / "phases").mkdir(parents=True)
    (run_dir / "phases" / "baseline_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "baseline_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "counts": {"total": 2, "passed": 1, "failed": 1, "error": 0},
        "tasks": [
            {"id": "open_settings", "category": "single_step", "status": "passed", "hard": 1, "soft": 1.0, "turns": 2, "reason": ""},
            {"id": "tap_wifi", "category": "single_step", "status": "failed", "hard": 0, "soft": 0.0, "turns": 0, "reason": "No tool calls.", "raw_report": "benchmark/raw/report.html"},
        ],
    }), encoding="utf-8")
    cfg = main.OptimizationConfig(
        skill_name="device-operator",
        skill_path=tmp_path / "SKILL.md",
        suite=object(),
        train_tasks=[object()] * 2,
        selection_tasks=[object()] * 2,
        artifact_root=artifact_root,
        run_id=run_id,
        optimizer_cfg=main.OptimizerConfig(),
    )
    result = OptimizationResult(
        skill_name="device-operator",
        initial_score=0.5,
        best_score=0.5,
        best_skill="best skill\n",
        phase_summaries=[
            PhaseSummary(
                phase="baseline_selection",
                kind="verification",
                suite_name="device_operator_verification",
                score=ScoreSummary(hard=0.5, soft=0.5, n=2, n_passed=1),
            )
        ],
        stop_reason="step 1: no patches produced by reflect",
    )

    main._write_web_artifacts(
        cfg=cfg,
        result=result,
        original_skill="best skill\n",
        diff_text="",
        optimizer_model="optimizer",
        judge_model="judge",
        train_suite_label=TRAIN_LABEL,
        selection_suite_label=VERIFICATION_LABEL,
        backend="mobilegym",
    )

    manifest = json.loads((run_dir / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["phase_records"][0]["tasks"][1]["id"] == "tap_wifi"
    report = (run_dir / "report.html").read_text(encoding="utf-8")
    assert "SkillOpt Phases" in report
    assert "Task Records" not in report
    assert "Tool calls" not in report
    assert ">Turns<" not in report
    assert "tap_wifi" not in report
    assert "No tool calls." not in report
    assert "benchmark/raw/report.html" in report
    assert ">report</a>" in report
    assert "raw evidence" not in report


def test_web_report_uses_child_benchmark_totals_for_bridge_device_backend(tmp_path: Path):
    artifact_root = tmp_path / "skillopt" / "runs"
    run_id = "skillopt-device-bridge-run"
    raw_dir = artifact_root / run_id / "benchmark" / f"{run_id}-baseline_selection"
    raw_dir.mkdir(parents=True)
    (raw_dir / "manifest.json").write_text(json.dumps({
        "totals": {
            "tasks": 4,
            "passed": 3,
            "failed": 1,
            "skipped": 0,
            "judge_error": 0,
            "timeout": 0,
        }
    }), encoding="utf-8")
    cfg = main.OptimizationConfig(
        skill_name="device-operator",
        skill_path=tmp_path / "SKILL.md",
        suite=object(),
        train_tasks=[object()] * 4,
        selection_tasks=[object()] * 4,
        artifact_root=artifact_root,
        run_id=run_id,
        optimizer_cfg=main.OptimizerConfig(),
    )
    result = OptimizationResult(
        skill_name="device-operator",
        initial_score=0.75,
        best_score=0.75,
        best_skill="best skill\n",
        phase_summaries=[
            PhaseSummary(
                phase="baseline_selection",
                kind="verification",
                suite_name="device_operator_verification",
                score=ScoreSummary(hard=0.75, soft=0.75, n=4, n_passed=3),
            )
        ],
    )

    main._write_web_artifacts(
        cfg=cfg,
        result=result,
        original_skill="best skill\n",
        diff_text="",
        optimizer_model="optimizer",
        judge_model="judge",
        train_suite_label=TRAIN_LABEL,
        selection_suite_label=VERIFICATION_LABEL,
        backend="device",
    )

    manifest = json.loads((artifact_root / run_id / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["raw_score_summary"]["baseline_selection"] == {
        "passed": 3,
        "tasks": 4,
        "failed": 1,
        "error": 0,
        "pass_rate": 0.75,
    }


def test_web_report_renders_diff_with_structured_line_classes():
    result = OptimizationResult(
        skill_name="device-operator",
        initial_score=0.0,
        best_score=1.0,
        best_skill="new <rule>\ncontext\n",
    )
    diff_text = "\n".join([
        "--- original",
        "+++ best_skill",
        "@@ -1,2 +1,2 @@",
        "-old <rule>",
        "+new <rule>",
        " context",
        "",
    ])

    report = main._render_report_html(
        manifest={"skill": "device-operator", "train_suite": "train", "validation_suite": "verify"},
        result=result,
        original_skill="old <rule>\ncontext\n",
        diff_text=diff_text,
    )

    assert 'class="diff-viewer"' in report
    assert 'class="diff-line diff-file"' in report
    assert 'class="diff-line diff-hunk"' in report
    assert 'class="diff-line diff-del"' in report
    assert 'class="diff-line diff-add"' in report
    assert 'class="diff-line diff-context"' in report
    assert '<span class="diff-old">1</span>' in report
    assert '<span class="diff-new">1</span>' in report
    assert '+new &lt;rule&gt;' in report
    assert '-old &lt;rule&gt;' in report


def test_cli_dry_run_does_not_write_output_or_web_artifacts(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("original skill\n", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    artifact_root = tmp_path / "skillopt" / "runs"
    output_path = tmp_path / "optimized.md"

    def fake_optimize_skill(cfg):
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill\n",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "AicoDeviceBackend", FakeDeviceBackend, raising=False)

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--artifact-root", str(artifact_root),
        "--run-id", "skillopt-dry-run",
        "--output", str(output_path),
        "--dry-run",
    ])

    assert rc == 0
    assert not output_path.exists()
    assert not (artifact_root / "skillopt-dry-run").exists()


def test_web_artifact_type_hints_resolve():
    hints = typing.get_type_hints(main._write_web_artifacts)

    assert hints["result"] is OptimizationResult


@pytest.mark.parametrize("skill", ["../device-operator", ".", ".."])
def test_cli_rejects_unsafe_skill_names(monkeypatch, tmp_path: Path, capsys, skill: str):
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--skill", skill,
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 2
    assert "invalid skill name" in capsys.readouterr().err


def test_cli_rejects_cross_skill_skillopt_suite(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", "skillopt/planner/train",
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 2
    assert "must be under skillopt/device-operator" in capsys.readouterr().err


def test_cli_rejects_unsafe_suite_traversal(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", "skillopt/device-operator/../x",
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 2
    assert "invalid suite label" in capsys.readouterr().err


def test_cli_rejects_unsafe_split_suite_traversal(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--skill", "device-operator",
        "--suite", "../outside",
    ])

    assert rc == 2
    assert "invalid suite label" in capsys.readouterr().err


def test_cli_rejects_cross_skill_split_suite(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--skill", "device-operator",
        "--suite", "skillopt/planner/train",
    ])

    assert rc == 2
    assert "must be under skillopt/device-operator" in capsys.readouterr().err


def test_cli_rejects_invalid_run_id(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(
        main,
        "optimize_skill",
        lambda cfg: (_ for _ in ()).throw(AssertionError("optimize_skill should not be called")),
    )

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
        "--run-id", "../x",
    ])

    assert rc == 2
    assert "invalid run_id" in capsys.readouterr().err


def test_cli_rejects_invalid_backend():
    with pytest.raises(SystemExit):
        main.cli([
            "--backend", "simulator",
            "--skill", "device-operator",
            "--train-suite", TRAIN_LABEL,
            "--validation-suite", VERIFICATION_LABEL,
        ])


def test_cli_uses_benchmark_runner_backend_for_mobilegym(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    captured = {}

    def fake_optimize_skill(cfg):
        captured["backend"] = cfg.rollout_backend
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "BenchmarkRunnerBackend", FakeBenchmarkRunnerBackend, raising=False)

    rc = main.cli([
        "--backend", "mobilegym",
        "--environment-url", "http://127.0.0.1:50196",
        "--mobilegym-parallel", "3",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 0
    backend = captured["backend"]
    assert isinstance(backend, FakeBenchmarkRunnerBackend)
    assert backend.kwargs["environment_url"] == "http://127.0.0.1:50196"
    assert backend.kwargs["backend"] == "mobilegym"
    assert backend.kwargs["base_config_dir"] == tmp_path / "benchmark" / "config"


def test_cli_passes_agent_config_to_optimizer(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    agent_config = tmp_path / "agent.toml"
    agent_config.write_text('[model]\napi_key = "sk-test"\n', encoding="utf-8")
    captured = {}

    def fake_optimize_skill(cfg):
        captured["agent_config_path"] = cfg.optimizer_cfg.agent_config_path
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "BenchmarkRunnerBackend", FakeBenchmarkRunnerBackend, raising=False)

    rc = main.cli([
        "--backend", "mobilegym",
        "--environment-url", "http://127.0.0.1:50196",
        "--agent-config", str(agent_config),
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 0
    assert captured["agent_config_path"] == str(agent_config)


def test_cli_uses_agent_config_model_when_optimizer_and_judge_unspecified(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    agent_config = tmp_path / "agent.toml"
    agent_config.write_text('[model]\nmodel = "openrouter/agent-model"\napi_key = "sk-test"\n', encoding="utf-8")
    captured = {}

    def fake_optimize_skill(cfg):
        captured["optimizer_model"] = cfg.optimizer_cfg.model
        captured["judge_model"] = cfg.judge_cfg.model
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "BenchmarkRunnerBackend", FakeBenchmarkRunnerBackend, raising=False)

    rc = main.cli([
        "--backend", "mobilegym",
        "--environment-url", "http://127.0.0.1:50196",
        "--agent-config", str(agent_config),
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 0
    assert captured == {
        "optimizer_model": "openrouter/agent-model",
        "judge_model": "openrouter/agent-model",
    }


def test_cli_uses_device_base_config_by_default_with_bridge(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    captured = {}

    def fake_optimize_skill(cfg):
        captured["backend"] = cfg.rollout_backend
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "BenchmarkRunnerBackend", FakeBenchmarkRunnerBackend, raising=False)

    rc = main.cli([
        "--backend", "device",
        "--environment-url", "http://127.0.0.1:50196",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 0
    backend = captured["backend"]
    assert isinstance(backend, FakeBenchmarkRunnerBackend)
    assert backend.kwargs["backend"] == "device"
    assert backend.kwargs["base_config_dir"] == tmp_path / "benchmark" / "config"


def test_cli_honors_explicit_base_config_for_mobilegym(monkeypatch, tmp_path: Path):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    custom_config = tmp_path / "custom-config"
    captured = {}

    def fake_optimize_skill(cfg):
        captured["backend"] = cfg.rollout_backend
        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=0.0,
            best_score=1.0,
            best_skill="optimized skill",
        )

    _set_roots(monkeypatch, tmp_path)
    monkeypatch.setattr(main, "optimize_skill", fake_optimize_skill)
    monkeypatch.setattr(main, "BenchmarkRunnerBackend", FakeBenchmarkRunnerBackend, raising=False)

    rc = main.cli([
        "--backend", "mobilegym",
        "--environment-url", "http://127.0.0.1:50196",
        "--base-config-dir", str(custom_config),
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 0
    backend = captured["backend"]
    assert backend.kwargs["base_config_dir"] == custom_config


def test_cli_requires_environment_url_for_mobilegym(monkeypatch, tmp_path: Path, capsys):
    skill_path = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    skill_path.parent.mkdir(parents=True)
    skill_path.write_text("skill", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    _set_roots(monkeypatch, tmp_path)

    rc = main.cli([
        "--backend", "mobilegym",
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 2
    assert "--backend mobilegym requires --environment-url" in capsys.readouterr().err


def test_cli_dispatches_webui_subcommand(monkeypatch):
    called = {}

    def fake_webui_cli(argv):
        called["argv"] = argv
        return 0

    monkeypatch.setattr(main, "webui_cli", fake_webui_cli, raising=False)

    rc = main.cli(["webui", "--port", "8766"])

    assert rc == 0
    assert called["argv"] == ["--port", "8766"]


def test_resolve_skill_path_prefers_shared_skill_over_mobilegym_template(monkeypatch, tmp_path: Path):
    shared = tmp_path / "src" / "agent" / "config" / "skills" / "device-operator" / "SKILL.md"
    shared.parent.mkdir(parents=True)
    shared.write_text("shared", encoding="utf-8")
    template = tmp_path / "benchmark" / "mobilegym" / "config" / "skills" / "device-operator" / "SKILL.md"
    template.parent.mkdir(parents=True)
    template.write_text("template", encoding="utf-8")
    _set_roots(monkeypatch, tmp_path)
    monkeypatch.delenv("AICO_SKILLS_DIR", raising=False)

    assert main._resolve_skill_path("device-operator") == shared


def test_cli_does_not_use_mobilegym_template_as_skill_source(monkeypatch, tmp_path: Path, capsys):
    template = tmp_path / "benchmark" / "mobilegym" / "config" / "skills" / "device-operator" / "SKILL.md"
    template.parent.mkdir(parents=True)
    template.write_text("template", encoding="utf-8")
    _write_device_operator_suites(tmp_path)
    _set_roots(monkeypatch, tmp_path)
    monkeypatch.delenv("AICO_SKILLS_DIR", raising=False)

    rc = main.cli([
        "--skill", "device-operator",
        "--train-suite", TRAIN_LABEL,
        "--validation-suite", VERIFICATION_LABEL,
    ])

    assert rc == 2
    assert "skill not found" in capsys.readouterr().err

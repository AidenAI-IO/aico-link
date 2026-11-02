import json
from pathlib import Path

import pytest

from ci import main_daily_guard
from ci.main_daily_guard import build_card, commits_since_last_pass, evaluate_case, load_guard
from ci.plan import CatalogError, SuiteCase, load_catalog


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "main-daily-guard.yml"


def _root(tmp_path: Path) -> Path:
    suites = tmp_path / "suites"
    suites.mkdir(exist_ok=True)
    (suites / "native.json").write_text(json.dumps({"tasks": [
        {"id": "open", "platforms": ["android"]},
        {"id": "back"},
        {"id": "twice", "repeats": 2},
        {"id": "ios-only", "platforms": ["ios"]},
    ]}), encoding="utf-8")
    (suites / "offline.json").write_text(json.dumps({"tasks": [{"id": "tap"}]}), encoding="utf-8")
    (suites / "gym.json").write_text(json.dumps({"tasks": [{"id": "open"}]}), encoding="utf-8")
    return tmp_path


CATALOG = (
    SuiteCase(id="native", suite="suites/native.json", environment="adb",
              target_platform="android", timeout_minutes=240),
    SuiteCase(id="offline", suite="suites/offline.json", environment="isolated"),
    SuiteCase(id="gym", suite="suites/gym.json", environment="mobilegym",
              target_platform="android", max_concurrency=2),
    SuiteCase(id="bridge", suite="suites/native.json", environment="adb",
              target_platform="android", android_phone_bridge=True),
)


def _load(tmp_path: Path, cases) -> tuple[SuiteCase, ...]:
    path = tmp_path / "guard.json"
    path.write_text(json.dumps({"version": 1, "cases": cases}), encoding="utf-8")
    return load_guard(guard_path=path, catalog_cases=CATALOG, benchmark_root=_root(tmp_path))


def test_shipped_guard_reuses_catalog_cases_without_sharing_their_history() -> None:
    catalog_ids = {case.id for case in load_catalog().cases}
    cases = load_guard()
    assert {case.environment for case in cases} == {"adb", "isolated"}
    for case in cases:
        assert case.id not in catalog_ids
        assert case.task_ids and not case.runnable and not case.android_phone_bridge
        if case.environment == "adb":
            assert case.target_platform == "android" and case.max_concurrency == 1


def test_guard_case_inherits_the_source_case_with_its_own_tasks(tmp_path: Path) -> None:
    adb, offline = _load(tmp_path, [
        {"id": "guard-adb", "source": "native", "task_ids": ["open", "back"]},
        {"id": "guard-offline", "source": "offline", "task_ids": ["tap"]},
    ])
    assert (adb.id, adb.suite, adb.timeout_minutes, adb.task_ids) == (
        "guard-adb", "suites/native.json", 240, ["open", "back"])
    assert (offline.environment, offline.task_ids) == ("isolated", ["tap"])


@pytest.mark.parametrize("entry", [
    {"id": "guard", "source": "missing", "task_ids": ["open"]},
    {"id": "guard", "source": "gym", "task_ids": ["open"]},
    {"id": "guard", "source": "bridge", "task_ids": ["open"]},
    {"id": "native", "source": "native", "task_ids": ["open"]},
    {"id": "must pass", "source": "native", "task_ids": ["open"]},
    {"id": "guard", "source": "native", "task_ids": []},
    {"id": "guard", "source": "native", "task_ids": ["open", "open"]},
    {"id": "guard", "source": "native", "task_ids": ["unknown"]},
    {"id": "guard", "source": "native", "task_ids": ["ios-only"]},
    {"id": "guard", "source": "native", "task_ids": ["open"], "runnable": True},
])
def test_guard_rejects_invalid_entries(tmp_path: Path, entry) -> None:
    with pytest.raises(CatalogError):
        _load(tmp_path, [entry])


def test_guard_rejects_duplicate_and_empty_case_lists(tmp_path: Path) -> None:
    entry = {"id": "guard", "source": "native", "task_ids": ["open"]}
    with pytest.raises(CatalogError, match="duplicate"):
        _load(tmp_path, [entry, entry])
    with pytest.raises(CatalogError, match="non-empty"):
        _load(tmp_path, [])


def _case(task_ids) -> dict:
    return {"id": "guard", "suite": "suites/native.json", "task_ids": task_ids}


def _run(tmp_path: Path, rows) -> Path:
    run_dir = tmp_path / "runs" / "ci-1-1-guard"
    run_dir.mkdir(parents=True)
    (run_dir / "results.jsonl").write_text("".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8")
    return run_dir


def test_guard_passes_only_when_every_attempt_passed(tmp_path: Path) -> None:
    root = _root(tmp_path)
    run_dir = _run(tmp_path, [
        {"task_id": "open", "attempt": 1, "status": "passed"},
        {"task_id": "back", "attempt": 1, "status": "passed"},
    ])
    verdict = evaluate_case(_case(["open", "back"]), run_dir, repeats=None, benchmark_root=root)
    assert verdict["status"] == "pass"
    assert (verdict["passed_attempts"], verdict["expected_attempts"]) == (2, 2)


def test_guard_fails_on_agent_failure_with_its_evidence(tmp_path: Path) -> None:
    root = _root(tmp_path)
    run_dir = _run(tmp_path, [
        {"task_id": "open", "attempt": 1, "status": "failed",
         "rubric": [{"id": "settings-visible", "verdict": "no", "reason": "Home screen still visible"}]},
    ])
    verdict = evaluate_case(_case(["open"]), run_dir, repeats=None, benchmark_root=root)
    assert verdict["status"] == "fail"
    (failure,) = verdict["failures"]
    assert failure["task_id"] == "open" and failure["failure_class"] == "agent"
    assert "Home screen still visible" in failure["reason"]


def test_guard_fails_on_excluded_or_missing_attempts(tmp_path: Path) -> None:
    root = _root(tmp_path)
    run_dir = _run(tmp_path, [
        {"task_id": "open", "attempt": 1, "status": "skipped",
         "metrics": {"failure_class": "environment", "agent_eligible": False}},
        {"task_id": "twice", "attempt": 1, "status": "passed"},
    ])
    verdict = evaluate_case(_case(["open", "twice", "back"]), run_dir, repeats=None, benchmark_root=root)
    assert verdict["status"] == "fail"
    assert verdict["expected_attempts"] == 4
    assert [(f["task_id"], f["attempt"], f["status"]) for f in verdict["failures"]] == [
        ("open", 1, "skipped"), ("twice", 2, "missing"), ("back", 1, "missing"),
    ]
    assert verdict["failures"][0]["failure_class"] == "environment"


def test_repeats_override_expects_every_attempt(tmp_path: Path) -> None:
    root = _root(tmp_path)
    run_dir = _run(tmp_path, [{"task_id": "open", "attempt": 1, "status": "passed"}])
    verdict = evaluate_case(_case(["open"]), run_dir, repeats=3, benchmark_root=root)
    assert verdict["status"] == "fail" and verdict["expected_attempts"] == 3


def test_guard_fails_when_the_case_produced_no_results(tmp_path: Path) -> None:
    root = _root(tmp_path)
    verdict = evaluate_case(_case(["open"]), tmp_path / "runs" / "absent", repeats=None, benchmark_root=root)
    assert verdict["status"] == "fail" and "results.jsonl" in verdict["reason"]


def test_verdict_cli_records_status_for_later_steps(tmp_path: Path, monkeypatch) -> None:
    root = _root(tmp_path)
    monkeypatch.setattr(main_daily_guard, "BENCHMARK_ROOT", root)
    _run(tmp_path, [{"task_id": "open", "attempt": 1, "status": "failed"}])
    output, summary = tmp_path / "github-output", tmp_path / "summary.md"
    monkeypatch.setenv("GITHUB_OUTPUT", str(output))
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(summary))
    verdict_path = tmp_path / "verdict.json"
    assert main_daily_guard.cli([
        "verdict", "--matrix-json", json.dumps({"include": [_case(["open"])]}),
        "--runs-dir", str(tmp_path / "runs"), "--run-id-prefix", "ci-1-1",
        "--repeats", "1", "--output", str(verdict_path),
    ]) == 0
    assert output.read_text(encoding="utf-8") == "status=fail\n"
    assert json.loads(verdict_path.read_text(encoding="utf-8"))["status"] == "fail"
    assert "| guard | ❌ fail | 0/1 |" in summary.read_text(encoding="utf-8")


def _github(responses: dict[str, object], calls: list[tuple[str, dict]]):
    def get(api_url: str, path: str, token: str, **query):
        calls.append((path, query))
        return responses[path]
    return get


def test_commits_since_last_pass_skips_this_run_and_lists_newest_first(monkeypatch) -> None:
    calls: list[tuple[str, dict]] = []
    monkeypatch.setattr(main_daily_guard, "_github_get", _github({
        "/repos/o/r/actions/runs/9": {"workflow_id": 77},
        "/repos/o/r/actions/workflows/77/runs": {"workflow_runs": [
            {"id": 9, "head_sha": "head"}, {"id": 8, "head_sha": "base"}]},
        "/repos/o/r/compare/base...head": {"total_commits": 2, "commits": [
            {"sha": "a" * 40, "commit": {"message": "first\n\nbody", "author": {"name": "Ann"}}, "author": None},
            {"sha": "b" * 40, "commit": {"message": "second", "author": {"name": "Bo"}}, "author": {"login": "bo"}},
        ]},
    }, calls))
    history = commits_since_last_pass(api_url="https://api", repo="o/r", run_id="9", head_sha="head", token="t")
    assert history["base_sha"] == "base" and history["total"] == 2
    assert [(c["title"], c["author"]) for c in history["commits"]] == [("second", "bo"), ("first", "Ann")]
    assert calls[1][1] == {"branch": "main", "status": "success", "per_page": 10}


def test_commits_since_last_pass_handles_first_run_and_unchanged_head(monkeypatch) -> None:
    monkeypatch.setattr(main_daily_guard, "_github_get", _github({
        "/repos/o/r/actions/runs/9": {"workflow_id": 77},
        "/repos/o/r/actions/workflows/77/runs": {"workflow_runs": []},
    }, []))
    assert commits_since_last_pass(api_url="a", repo="o/r", run_id="9", head_sha="h", token="t")["base_sha"] is None
    monkeypatch.setattr(main_daily_guard, "_github_get", _github({
        "/repos/o/r/actions/runs/9": {"workflow_id": 77},
        "/repos/o/r/actions/workflows/77/runs": {"workflow_runs": [{"id": 8, "head_sha": "h"}]},
    }, []))
    assert commits_since_last_pass(api_url="a", repo="o/r", run_id="9", head_sha="h", token="t") == {
        "base_sha": "h", "commits": [], "total": 0}


def _failed_verdict() -> dict:
    return {"status": "fail", "cases": [
        {"case": "ok", "status": "pass", "passed_attempts": 1, "expected_attempts": 1,
         "reason": "", "failures": []},
        {"case": "guard", "status": "fail", "passed_attempts": 0, "expected_attempts": 1,
         "reason": "1 attempt(s) did not pass",
         "failures": [{"task_id": "open", "attempt": 1, "status": "failed",
                       "failure_class": "agent", "reason": "Home screen still visible"}]},
    ]}


def test_alert_card_lists_failed_attempts_and_suspect_commits() -> None:
    commits = [{"sha": f"{index:040d}", "title": f"change [{index}]", "author": "dev"} for index in range(20)]
    card = build_card(_failed_verdict(), run_url="https://example.invalid/run", cases_outcome="success",
                      head_sha="f" * 40, server_url="https://github.com", repo="o/r",
                      history={"base_sha": "e" * 40, "commits": commits, "total": 20})
    content = card["elements"][0]["content"]
    assert "**ok**" not in content
    assert "- open #1 failed (agent): Home screen still visible" in content
    assert f"[ffffffff](https://github.com/o/r/commit/{'f' * 40})" in content
    assert "last passing run ([eeeeeeee]" in content
    assert "change (0) — dev" in content and "change (15)" not in content
    assert "… and 5 more" in content
    assert card["elements"][1]["actions"][0]["url"] == "https://example.invalid/run"


@pytest.mark.parametrize(("history", "expected"), [
    (None, "unavailable"),
    ({"base_sha": None, "commits": [], "total": 0}, "no earlier passing run"),
    ({"base_sha": "e" * 40, "commits": [], "total": 0}, "No new commits"),
])
def test_alert_card_explains_missing_commit_history(history, expected) -> None:
    card = build_card(_failed_verdict(), run_url="u", cases_outcome="failure", head_sha="",
                      server_url="https://github.com", repo="o/r", history=history)
    assert expected in card["elements"][0]["content"]


def test_unsigned_card_posts_without_a_signature(monkeypatch) -> None:
    sent = {}

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return b'{"code": 0}'

    def urlopen(request, timeout):
        sent["body"] = json.loads(request.data)
        return Response()

    monkeypatch.setattr(main_daily_guard.urllib.request, "urlopen", urlopen)
    main_daily_guard.send_card({"elements": []}, webhook_url="https://hook.invalid")
    assert sent["body"] == {"msg_type": "interactive", "card": {"elements": []}}


def test_alert_is_skipped_without_its_own_lark_group(tmp_path: Path, monkeypatch, capsys) -> None:
    monkeypatch.delenv("MAIN_DAILY_GUARD_LARK_WEBHOOK_URL", raising=False)
    monkeypatch.setenv("BENCHMARK_REPORT_LARK_WEBHOOK_URL", "https://weekly.invalid")
    assert main_daily_guard.cli(["notify", "--verdict-json", str(tmp_path / "none.json")]) == 0
    assert "skipping alert" in capsys.readouterr().out


def test_workflow_runs_daily_at_noon_and_gates_after_evidence_upload() -> None:
    text = WORKFLOW.read_text(encoding="utf-8")
    assert "- cron: '0 4 * * *'" in text
    assert "permissions:\n  contents: read\n  # The failure alert reads this workflow's last successful run on main.\n  actions: read\n" in text
    steps = [line.strip().removeprefix("- name: ") for line in text.splitlines() if line.strip().startswith("- name: ")]
    assert steps.index("Upload guard artifacts") < steps.index("Enforce main daily guard")
    assert steps.index("Check guard verdict") < steps.index("Alert Lark on guard failure")
    assert "python3 -m ci.main_daily_guard matrix" in text
    assert "secrets.MAIN_DAILY_GUARD_LARK_WEBHOOK_URL" in text
    assert "BENCHMARK_REPORT_LARK_WEBHOOK" not in text
    assert "android_phone_bridge" not in text
    assert "BENCHMARK_REPEATS: ${{ inputs.repeats || '1' }}" in text
    artifact_paths = text.split("path: |", 1)[1].split("if-no-files-found:", 1)[0]
    assert "cli-services" not in artifact_paths
    worker_paths = [path for path in artifact_paths.splitlines() if "/workers/" in path]
    assert worker_paths
    assert all(path.endswith(("/workers/*/daemon.log", "/workers/*/runner.log")) for path in worker_paths)

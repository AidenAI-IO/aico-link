import json

from runner import analysis


def test_config_from_env_enables_analysis_and_reads_limits(monkeypatch):
    monkeypatch.setenv("AICO_BENCHMARK_LLM_ANALYSIS", "1")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_MODEL", "anthropic/claude-sonnet-4-6")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_BASE_URL", "https://analysis.example.com/v1")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_MAX_LOG_BYTES", "1234")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_MAX_CODE_BYTES", "5678")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_TIMEOUT_SEC", "9")

    cfg = analysis.config_from_env()

    assert cfg.enabled is True
    assert cfg.model == "anthropic/claude-sonnet-4-6"
    assert cfg.base_url == "https://analysis.example.com/v1"
    assert cfg.max_log_bytes == 1234
    assert cfg.max_code_bytes == 5678
    assert cfg.timeout_sec == 9


def test_resolve_analysis_api_key_uses_expected_precedence(monkeypatch):
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY_ENV", "CUSTOM_ANALYSIS_KEY")
    monkeypatch.setenv("CUSTOM_ANALYSIS_KEY", "custom-secret")
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY", "analysis-secret")
    monkeypatch.setenv("AICO_BENCHMARK_JUDGE_API_KEY", "judge-secret")
    monkeypatch.setenv("OPENROUTER_API_KEY", "openrouter-secret")
    monkeypatch.setenv("MODEL_API_KEY", "model-secret")
    monkeypatch.setenv("AICO_MODEL_API_KEY", "aico-secret")

    cfg = analysis.AnalysisConfig(enabled=True)

    assert analysis.resolve_analysis_api_key(cfg) == ("CUSTOM_ANALYSIS_KEY", "custom-secret")

    monkeypatch.delenv("AICO_BENCHMARK_ANALYSIS_API_KEY_ENV")
    monkeypatch.delenv("CUSTOM_ANALYSIS_KEY")
    cfg = analysis.AnalysisConfig(enabled=True)
    assert analysis.resolve_analysis_api_key(cfg) == (
        "AICO_BENCHMARK_ANALYSIS_API_KEY",
        "analysis-secret",
    )

    monkeypatch.delenv("AICO_BENCHMARK_ANALYSIS_API_KEY")
    assert analysis.resolve_analysis_api_key(cfg) == (
        "AICO_BENCHMARK_JUDGE_API_KEY",
        "judge-secret",
    )

    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY")
    try:
        analysis.resolve_analysis_api_key(cfg)
    except analysis.AnalysisError as exc:
        assert "AICO_BENCHMARK_ANALYSIS_API_KEY" in str(exc)
    else:
        raise AssertionError("expected legacy provider credentials to be ignored")


def test_resolve_analysis_api_key_accepts_explicit_override(monkeypatch):
    monkeypatch.delenv("AICO_BENCHMARK_ANALYSIS_API_KEY", raising=False)
    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY", raising=False)

    cfg = analysis.AnalysisConfig(enabled=True, api_key_value="ui-secret")

    assert analysis.resolve_analysis_api_key(cfg) == ("provided", "ui-secret")
    assert "ui-secret" not in analysis.redact_text("token ui-secret", cfg)


def test_chat_analysis_model_uses_configured_base_url(monkeypatch):
    seen = {}

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return json.dumps({
                "choices": [{"message": {"content": json.dumps({
                    "summary": "ok",
                    "classification_counts": {"insufficient_evidence": 0},
                    "failure_clusters": [],
                    "recommendations": [],
                })}}]
            }).encode("utf-8")

    def fake_urlopen(req, timeout):
        seen["url"] = req.full_url
        seen["authorization"] = req.headers.get("Authorization")
        seen["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr(analysis.urllib.request, "urlopen", fake_urlopen)

    raw = analysis.chat_analysis_model(
        analysis.AnalysisConfig(enabled=True, base_url="https://analysis.example.com/v1/", timeout_sec=7),
        {"failures": []},
        "sk-analysis",
    )

    assert "summary" in raw
    assert seen == {
        "url": "https://analysis.example.com/v1/chat/completions",
        "authorization": "Bearer sk-analysis",
        "timeout": 7,
    }


def test_redact_removes_known_and_custom_secrets(monkeypatch):
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY_ENV", "CUSTOM_ANALYSIS_KEY")
    monkeypatch.setenv("CUSTOM_ANALYSIS_KEY", "custom-secret-value")
    text = (
        "OPENROUTER_API_KEY=sk-or-v1-secret bearer Bearer abcdefghijklmnop "
        "jwt abcdefgh.ijklmnop.qrstuvwx CUSTOM_ANALYSIS_KEY=custom-secret-value "
        'api_key = "quoted-secret" {"api_key":"json-secret"} Authorization: Basic basicsecret123'
    )

    redacted = analysis.redact_text(
        text, analysis.AnalysisConfig(enabled=True, api_key_env="CUSTOM_ANALYSIS_KEY")
    )

    assert "sk-or-v1-secret" not in redacted
    assert "abcdefghijklmnop" not in redacted
    assert "abcdefgh.ijklmnop.qrstuvwx" not in redacted
    assert "custom-secret-value" not in redacted
    assert "quoted-secret" not in redacted
    assert "json-secret" not in redacted
    assert "basicsecret123" not in redacted
    assert "[REDACTED" in redacted


def test_redact_removes_benchmark_api_key_assignments_from_captured_logs(monkeypatch):
    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY", "current-analysis-secret")
    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY", raising=False)
    text = (
        "AICO_BENCHMARK_ANALYSIS_API_KEY=captured-analysis-secret "
        "AICO_BENCHMARK_JUDGE_API_KEY=captured-judge-secret"
    )

    redacted = analysis.redact_text(text, analysis.AnalysisConfig(enabled=True))

    assert "captured-analysis-secret" not in redacted
    assert "captured-judge-secret" not in redacted


def test_render_markdown_includes_clusters_and_recommendations():
    payload = {
        "summary": "Two failures share a timeout pattern.",
        "classification_counts": {"project_code_issue": 1},
        "failure_clusters": [
            {
                "title": "Chat timeout",
                "task_ids": ["suite.task_a"],
                "suspected_cause": "Daemon did not answer",
                "category": "project_code_issue",
                "confidence": "medium",
                "evidence": ["console.log: timed out"],
            }
        ],
        "recommendations": [
            {"priority": "high", "target": "src/agent", "suggestion": "Add timeout logging"}
        ],
        "evidence_gaps": ["No daemon stderr log"],
    }

    md = analysis.render_markdown(payload)

    assert "# LLM 基准分析" in md
    assert "## 失败聚类" in md
    assert "## 建议" in md
    assert "Two failures" in md
    assert "suite.task_a" in md
    assert "Add timeout logging" in md
    assert "No daemon stderr log" in md


def test_collect_context_native_run_includes_failed_task_suite_trace_and_code(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-1"
    task_dir = run_dir / "tasks" / "task_a"
    task_dir.mkdir(parents=True)
    suite_path = repo / "benchmark" / "suites" / "demo.json"
    suite_path.parent.mkdir(parents=True)
    suite_path.write_text(
        json.dumps({"name": "demo", "tasks": [{"id": "task_a", "prompt": "do it"}]}),
        encoding="utf-8",
    )
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {"run_id": "run-1", "suite_path": str(suite_path), "totals": {"tasks": 1, "failed": 1}}
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task_a",
                "status": "failed",
                "metrics": {"agent_error": "WidgetError in widget_handler"},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (task_dir / "trace.json").write_text(
        json.dumps(
            {"final_response": "failed", "tool_calls": [{"tool": "shell", "input": {"command": "widget_handler"}}]}
        ),
        encoding="utf-8",
    )
    (task_dir / "history.json").write_text(
        json.dumps([{"type": "tool_call", "tool_name": "shell", "input": "widget_handler"}]),
        encoding="utf-8",
    )
    code = repo / "src" / "agent" / "widget_handler.go"
    code.parent.mkdir(parents=True)
    code.write_text("package agent\nfunc widget_handler() {}\n", encoding="utf-8")

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert ctx["run"]["kind"] == "native"
    assert ctx["suite"]["path"].endswith("demo.json")
    assert ctx["failures"][0]["task_id"] == "task_a"
    assert "WidgetError" in json.dumps(ctx)
    assert any(item["path"].endswith("widget_handler.go") for item in ctx["code"])


def test_collect_context_native_run_rejects_unsafe_task_artifact_paths(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-unsafe"
    (run_dir / "outside").mkdir(parents=True)
    (run_dir / "outside" / "trace.json").write_text("leaked secret", encoding="utf-8")
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run-unsafe", "totals": {"failed": 1}}), encoding="utf-8"
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps({"task_id": "../outside", "status": "failed", "metrics": {"error": "boom"}}) + "\n",
        encoding="utf-8",
    )

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))
    encoded = json.dumps(ctx, ensure_ascii=False)

    assert "leaked secret" not in encoded
    assert any("unsafe task artifact path" in warning for warning in ctx["collection_warnings"])


def test_collect_context_native_run_reads_repeated_attempt_artifacts(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-repeat"
    attempt_dir = run_dir / "tasks" / "task_a" / "attempt_2"
    attempt_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run-repeat", "totals": {"failed": 1}}), encoding="utf-8"
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task_a",
                "attempt": 2,
                "status": "failed",
                "artifact_dir": str(attempt_dir),
                "metrics": {"error": "AttemptBoom"},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (attempt_dir / "trace.json").write_text(json.dumps({"error": "AttemptBoom"}), encoding="utf-8")

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert "AttemptBoom" in ctx["failures"][0]["trace_excerpt"]
    assert "tasks/task_a/attempt_2/trace.json" in ctx["failures"][0]["artifact_refs"]


def test_collect_context_mobilegym_run_includes_errors_runner_logs_and_bridge_actions(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "mobilegym" / "batch-1"
    shard = run_dir / "clock" / "shard-0"
    raw = shard / "raw" / "run"
    raw.mkdir(parents=True)
    (run_dir / "summary.json").write_text(
        json.dumps({"batch_id": "batch-1", "tasks": 1, "error": 1}), encoding="utf-8"
    )
    (shard / "shard.json").write_text(
        json.dumps({"suite": "clock", "selected_task_ids": ["clock.Task"], "selected_task_count": 1}),
        encoding="utf-8",
    )
    (shard / "runner.log").write_text("runner saw ClockBoom in clock_runner", encoding="utf-8")
    (shard / "compose.log").write_text("compose ok", encoding="utf-8")
    (raw / "console.log").write_text("console ClockBoom", encoding="utf-8")
    (raw / "errors.jsonl").write_text(json.dumps({"id": "clock.Task", "error": "ClockBoom"}) + "\n", encoding="utf-8")
    (raw / "aico_bridge_actions.json").write_text(
        json.dumps([{"tool_name": "tap", "error": "ClockBoom"}]), encoding="utf-8"
    )
    code = repo / "benchmark" / "mobilegym" / "clock_runner.py"
    code.parent.mkdir(parents=True)
    code.write_text("class ClockBoom(Exception): pass\n", encoding="utf-8")

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert ctx["run"]["kind"] == "mobilegym"
    assert ctx["failures"][0]["task_id"] == "clock.Task"
    assert any("runner saw ClockBoom" in item["excerpt"] for item in ctx["logs"])
    assert any(item["path"].endswith("clock_runner.py") for item in ctx["code"])


def test_collect_context_mobilegym_bridge_inactive_setup_errors_add_known_issue(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "mobilegym" / "batch-bridge-inactive"
    shard = run_dir / "skillopt" / "shard-0"
    raw = shard / "raw" / "run"
    raw.mkdir(parents=True)
    error = (
        "AicoAdapterError: setup tool keyboard_tap failed: error: "
        "keyboard_tap failed: mobilegym bridge episode is not active"
    )
    (run_dir / "summary.json").write_text(
        json.dumps({"batch_id": "batch-bridge-inactive", "tasks": 2, "error": 2}),
        encoding="utf-8",
    )
    (shard / "shard.json").write_text(
        json.dumps({"selected_task_ids": ["suite.task_a", "suite.task_b"]}), encoding="utf-8"
    )
    (raw / "errors.jsonl").write_text(
        json.dumps({"id": "suite.task_a", "error": error})
        + "\n"
        + json.dumps({"id": "suite.task_b", "error": error})
        + "\n",
        encoding="utf-8",
    )

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert ctx["known_issues"] == [
        {
            "id": "mobilegym_setup_before_episode_start",
            "category": "benchmark_infra_issue",
            "task_ids": ["suite.task_a", "suite.task_b"],
            "summary": "MobileGym setup tools ran before the bridge episode was active.",
            "evidence": [
                "AicoAdapterError: setup tool keyboard_tap failed: error: keyboard_tap failed: mobilegym bridge episode is not active"
            ],
            "suspected_cause": (
                "Aico Agent reset/setup lifecycle invoked setup tool_sequence before "
                "bridge /episode/start and daemon /api/mobilegym/episode/start bound an active episode."
            ),
        }
    ]


def test_analyze_run_normalizes_bridge_inactive_analysis_payload(monkeypatch, tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "mobilegym" / "batch-bridge-inactive"
    shard = run_dir / "skillopt" / "shard-0"
    raw = shard / "raw" / "run"
    raw.mkdir(parents=True)
    error = (
        "AicoAdapterError: setup tool keyboard_tap failed: error: "
        "keyboard_tap failed: mobilegym bridge episode is not active"
    )
    (run_dir / "summary.json").write_text(
        json.dumps({"batch_id": "batch-bridge-inactive", "tasks": 1, "error": 1}),
        encoding="utf-8",
    )
    (shard / "shard.json").write_text(
        json.dumps({"selected_task_ids": ["suite.task_a"]}), encoding="utf-8"
    )
    (raw / "errors.jsonl").write_text(json.dumps({"id": "suite.task_a", "error": error}) + "\n", encoding="utf-8")

    def fake_chat(cfg, context, api_key):
        assert context["known_issues"][0]["id"] == "mobilegym_setup_before_episode_start"
        return json.dumps(
            {
                "summary": "All tasks failed because the bridge episode was inactive.",
                "classification_counts": {"benchmark_infra_issue": 1, "agent_behavior_issue": 0},
                "failure_clusters": [{}],
                "recommendations": [
                    "Resolve the container platform mismatch.",
                    "Fix the missing benchmark directory.",
                ],
                "evidence_gaps": [],
            }
        )

    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY", "test-key")
    monkeypatch.setattr(analysis, "chat_analysis_model", fake_chat)

    result = analysis.analyze_run(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    payload = json.loads((run_dir / "llm_analysis.json").read_text(encoding="utf-8"))
    markdown = (run_dir / "llm_analysis.md").read_text(encoding="utf-8")
    assert result.ok is True
    assert payload["failure_clusters"][0]["category"] == "benchmark_infra_issue"
    assert payload["failure_clusters"][0]["task_ids"] == ["suite.task_a"]
    assert "MobileGym setup 早于 bridge episode 激活执行" in markdown
    assert "分类: `unknown`" not in markdown


def test_bridge_inactive_known_issue_does_not_overwrite_unrelated_blank_cluster():
    context = {
        "known_issues": [
            {
                "id": "mobilegym_setup_before_episode_start",
                "task_ids": ["suite.bridge"],
                "summary": "bridge inactive",
                "evidence": ["mobilegym bridge episode is not active"],
            }
        ]
    }
    payload = {
        "classification_counts": {"agent_behavior_issue": 1},
        "failure_clusters": [
            {
                "title": "Agent picked the wrong target",
                "category": "",
                "task_ids": ["suite.agent"],
                "evidence": ["agent tapped settings instead of clock"],
            }
        ],
    }

    normalized = analysis._normalize_analysis_payload(context, payload)

    assert normalized["failure_clusters"][0]["category"] == "benchmark_infra_issue"
    assert normalized["failure_clusters"][0]["task_ids"] == ["suite.bridge"]
    assert normalized["failure_clusters"][1]["title"] == "Agent picked the wrong target"
    assert normalized["failure_clusters"][1]["task_ids"] == ["suite.agent"]


def test_collect_context_records_warnings_for_malformed_artifacts(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "mobilegym" / "batch-bad"
    run_dir.mkdir(parents=True)
    (run_dir / "summary.json").write_text("{bad", encoding="utf-8")

    ctx = analysis.collect_context(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert ctx["run"]["kind"] == "mobilegym"
    assert ctx["collection_warnings"]


def test_collect_context_redacts_sensitive_files_and_enforces_budget(tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-budget"
    task_dir = run_dir / "tasks" / "task_a"
    task_dir.mkdir(parents=True)
    suite_path = repo / "benchmark" / "suites" / "demo.json"
    suite_path.parent.mkdir(parents=True)
    suite_path.write_text(
        json.dumps({"name": "demo", "tasks": [{"id": "task_a", "prompt": "BudgetBoom"}]}),
        encoding="utf-8",
    )
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run-budget", "suite_path": str(suite_path), "totals": {"failed": 1}}),
        encoding="utf-8",
    )
    (run_dir / "summary.md").write_text("BudgetBoom " + "s" * 5000, encoding="utf-8")
    (run_dir / "results.jsonl").write_text(
        json.dumps({"task_id": "task_a", "status": "failed", "metrics": {"error": "BudgetBoom"}}) + "\n",
        encoding="utf-8",
    )
    (task_dir / "trace.json").write_text(
        json.dumps({"final_response": "BudgetBoom " + "x" * 5000}), encoding="utf-8"
    )
    sensitive = repo / "src" / "agent" / "agent.toml"
    sensitive.parent.mkdir(parents=True)
    sensitive.write_text('api_key = "sk-sensitive"\n# BudgetBoom\n', encoding="utf-8")
    code = repo / "src" / "agent" / "budget_boom.go"
    code.write_text("package agent\n// BudgetBoom\n" + "x" * 5000, encoding="utf-8")

    ctx = analysis.collect_context(
        run_dir,
        repo,
        analysis.AnalysisConfig(enabled=True, total_context_bytes=2000, max_code_bytes=2000),
    )
    encoded = json.dumps(ctx, ensure_ascii=False)

    assert len(encoded.encode("utf-8")) <= 2500
    assert "agent.toml" not in encoded
    assert "sk-sensitive" not in encoded
    assert ctx["collection_warnings"]


def test_analyze_run_writes_json_and_markdown_with_mocked_llm(monkeypatch, tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-1"
    run_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run-1", "totals": {"tasks": 0}}), encoding="utf-8"
    )
    (run_dir / "results.jsonl").write_text("", encoding="utf-8")

    def fake_chat(cfg, context, api_key):
        assert api_key == "test-key"
        assert context["run"]["id"] == "run-1"
        return json.dumps(
            {
                "summary": "Looks stable",
                "failure_clusters": [],
                "recommendations": [],
                "classification_counts": {},
                "evidence_gaps": [],
            }
        )

    monkeypatch.setenv("AICO_BENCHMARK_ANALYSIS_API_KEY", "test-key")
    monkeypatch.setattr(analysis, "chat_analysis_model", fake_chat)

    result = analysis.analyze_run(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert result.ok is True
    assert (run_dir / "llm_analysis.json").exists()
    assert (run_dir / "llm_analysis.md").exists()
    assert "Looks stable" in (run_dir / "llm_analysis.md").read_text(encoding="utf-8")


def test_analyze_run_writes_error_artifact_without_raising(monkeypatch, tmp_path):
    repo = tmp_path / "repo"
    run_dir = repo / "benchmark" / "runs" / "run-1"
    run_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(json.dumps({"run_id": "run-1"}), encoding="utf-8")
    monkeypatch.delenv("AICO_BENCHMARK_ANALYSIS_API_KEY", raising=False)
    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY", raising=False)

    result = analysis.analyze_run(run_dir, repo, analysis.AnalysisConfig(enabled=True))

    assert result.ok is False
    assert (run_dir / "llm_analysis_error.txt").exists()
    assert "missing analysis API key" in (run_dir / "llm_analysis_error.txt").read_text(encoding="utf-8")

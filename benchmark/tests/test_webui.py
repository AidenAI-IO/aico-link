import json
import subprocess
from pathlib import Path

import pytest

try:
    import tomllib
except ModuleNotFoundError:
    import tomli as tomllib

from runner import config as runner_config
from runner import webui


def test_list_benchmark_suites_discovers_nested_benchmark(tmp_path: Path):
    suites = tmp_path / "suites"
    (suites / "nested").mkdir(parents=True)
    (suites / "nested" / "memory.json").write_text(
        json.dumps(
            {
                "name": "memory_suite",
                "tasks": [
                    {
                        "id": "t1",
                        "category": "memory",
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    result = webui.list_benchmark_suites(suites)

    by_key = {item["key"]: item for item in result}
    assert by_key["nested/memory.json"]["kind"] == "benchmark"
    assert by_key["nested/memory.json"]["task_count"] == 1
    assert by_key["nested/memory.json"]["categories"] == ["memory"]
    assert by_key["nested/memory.json"]["suite_category"] == "Other"
    assert by_key["nested/memory.json"]["mock_environment"] is False


def test_list_benchmark_suites_marks_task_level_mock_environment(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "mock.json").write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "tasks": [
                    {
                        "id": "t1",
                        "category": "single_step",
                        "mock_environment": {
                            "phone_bridge": {"platform": "ios"},
                            "tools": {},
                        },
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    result = webui.list_benchmark_suites(suites)

    assert result[0]["mock_environment"] is True


def test_resolve_suite_path_rejects_traversal(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "ok.json").write_text("{}", encoding="utf-8")

    assert webui.resolve_suite_path(suites, "ok.json") == (suites / "ok.json").resolve()

    for bad in ("../x.json", "/tmp/x.json", "nested/../../x.json", "x.txt"):
        try:
            webui.resolve_suite_path(suites, bad)
        except ValueError:
            pass
        else:
            raise AssertionError(f"accepted invalid suite key {bad!r}")


def test_endpoint_for_docker_rewrites_localhost():
    assert webui.endpoint_for_docker("http://localhost:8080") == "http://host.docker.internal:8080"
    assert webui.endpoint_for_docker("http://127.0.0.1:9090/api") == "http://host.docker.internal:9090/api"
    assert webui.endpoint_for_docker("http://192.168.1.20:8080") == "http://192.168.1.20:8080"


def test_container_boot_failure_detail_ignores_timed_out_diagnostics(monkeypatch):
    calls = []

    def fake_run(command, **kwargs):
        calls.append((command, kwargs))
        raise subprocess.TimeoutExpired(command, kwargs["timeout"])

    monkeypatch.setattr(webui.subprocess, "run", fake_run)

    assert webui.container_boot_failure_detail("container-id") == ""
    assert len(calls) == 2
    assert all(call[1]["timeout"] == webui.DOCKER_DIAGNOSTIC_TIMEOUT_SEC for call in calls)


def test_webui_task_screen_url_points_at_webui_viewer():
    assert (
        webui.webui_task_screen_url("job-1", "suite.json:t1")
        == "/screens/jobs/job-1/tasks/suite.json%3At1"
    )


def test_task_screen_payload_proxies_bridge_screen_with_task_header(tmp_path: Path, monkeypatch):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    record = webui.TaskRecord(
        id="task-record",
        suite="suite.json",
        task_id="t1",
        benchmark_task_id="suite.json:t1",
    )
    job = webui.Job(
        id="job-1",
        endpoint="http://host.docker.internal:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        environment_endpoint="http://127.0.0.1:19090",
        task_records=[record],
        state_file=str(tmp_path / "state.json"),
    )
    app._jobs[job.id] = job
    seen = {}

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"ok": true, "data": {"meta": {"width": 100, "height": 200, "pixel_format": "jpeg"}, "image": "AAAA"}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["method"] = req.get_method()
        seen["headers"] = {key.lower(): value for key, value in req.header_items()}
        seen["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr(webui.urllib.request, "urlopen", fake_urlopen)

    payload = app.task_screen_payload("job-1", "task-record")

    assert payload["ok"] is True
    assert seen["url"] == "http://127.0.0.1:19090/api/providers/screenshot"
    assert seen["method"] == "POST"
    assert seen["headers"]["benchmark-task-id"] == "suite.json:t1"
    assert seen["headers"]["content-type"] == "application/json"
    assert seen["timeout"] == 30


def test_task_screen_html_fetches_webui_screen_api():
    assert "/api/jobs/" in webui.TASK_SCREEN_HTML
    assert "/api/screen" not in webui.TASK_SCREEN_HTML


def test_read_environment_bridge_concurrency_uses_base_url(monkeypatch):
    seen = {}

    class FakeResponse:
        def read(self):
            return b'{"ok": true, "data": {"concurrent": 3}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(url, timeout=None):
        seen["url"] = url
        seen["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr(webui.urllib.request, "urlopen", fake_urlopen)

    assert webui.read_environment_bridge_concurrency(
        "http://127.0.0.1:19090/bridge/", timeout=1.5
    ) == 3
    assert seen == {
        "url": "http://127.0.0.1:19090/bridge/api/concurrent",
        "timeout": 1.5,
    }


def test_prepare_run_config_renders_template(tmp_path: Path, monkeypatch):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml.template").write_text(
        '\n'.join(
            [
                '[model]',
                'provider = "{{AIDEN_BENCHMARK_AGENT_PROVIDER}}"',
                'model = "{{AIDEN_BENCHMARK_AGENT_MODEL}}"',
                'base_url = "{{AIDEN_BENCHMARK_AGENT_BASE_URL}}"',
                'api_key = "{{AIDEN_BENCHMARK_AGENT_API_KEY}}"',
                'control = "{{CONTROL_TOKEN_FILE}}"',
            ]
        ),
        encoding="utf-8",
    )
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_PROVIDER", "openai")
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_MODEL", "gpt-test")
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_BASE_URL", "https://agent.example/v1")
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_API_KEY", "sk-test")

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest)

    rendered = (dest / "agent.toml").read_text(encoding="utf-8")
    assert 'provider = "openai"' in rendered
    assert 'model = "gpt-test"' in rendered
    assert 'base_url = "https://agent.example/v1"' in rendered
    assert 'api_key = "sk-test"' in rendered
    assert 'control = "/config/control_token"' in rendered
    assert "voice_streaming_tts_enabled = false" in rendered
    assert "voice_tool_call_speech = false" in rendered
    assert "voice_progress_speech_enabled = false" in rendered
    assert (dest / "control_token").exists()
    assert (dest / "memory").is_dir()
    assert (dest / "skill-state").is_dir()


def test_prepare_run_config_ignores_legacy_agent_model_environment(tmp_path: Path, monkeypatch):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml.template").write_text(
        '\n'.join(
            [
                '[model]',
                'provider = "{{AIDEN_BENCHMARK_AGENT_PROVIDER}}"',
                'model = "{{AIDEN_BENCHMARK_AGENT_MODEL}}"',
                'base_url = "{{AIDEN_BENCHMARK_AGENT_BASE_URL}}"',
                'api_key = "{{AIDEN_BENCHMARK_AGENT_API_KEY}}"',
            ]
        ),
        encoding="utf-8",
    )
    for name, value in {
        "MODEL_PROVIDER": "legacy-provider",
        "MODEL_NAME": "legacy-model",
        "MODEL_BASE_URL": "https://legacy.example/v1",
        "MODEL_API_KEY": "legacy-key",
        "AIDEN_MODEL": "legacy-aiden-model",
        "AIDEN_MODEL_PROVIDER": "legacy-aiden-provider",
        "AIDEN_MODEL_BASE_URL": "https://legacy-aiden.example/v1",
        "AIDEN_MODEL_API_KEY": "legacy-aiden-key",
        "OPENAI_MODEL": "legacy-openai-model",
        "OPENROUTER_API_KEY": "legacy-openrouter-key",
    }.items():
        monkeypatch.setenv(name, value)
    for name in (
        "AIDEN_BENCHMARK_AGENT_PROVIDER",
        "AIDEN_BENCHMARK_AGENT_MODEL",
        "AIDEN_BENCHMARK_AGENT_BASE_URL",
        "AIDEN_BENCHMARK_AGENT_API_KEY",
    ):
        monkeypatch.delenv(name, raising=False)

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest)

    rendered = (dest / "agent.toml").read_text(encoding="utf-8")
    assert 'provider = "fake"' in rendered
    assert 'model = ""' in rendered
    assert 'base_url = ""' in rendered
    assert 'api_key = ""' in rendered


def test_prepare_run_config_enables_ci_telemetry_only_when_requested(tmp_path: Path, monkeypatch):
    config_text = '[model_settings.model]\nprovider = "fake"\n'
    base, dest = tmp_path / "base", tmp_path / "dest"
    monkeypatch.setenv("LANGFUSE_BASE_URL", "https://langfuse.example")
    monkeypatch.setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
    secret = 'sk-quote"back\\slash'
    monkeypatch.setenv("LANGFUSE_SECRET_KEY", secret)
    monkeypatch.delenv("AIDEN_BENCHMARK_TELEMETRY", raising=False)
    webui.prepare_run_config(base, dest, agent_config_text=config_text)
    config = tomllib.loads((dest / "agent.toml").read_text())
    assert "advanced_settings" not in config

    monkeypatch.setenv("AIDEN_BENCHMARK_TELEMETRY", "1")
    webui.prepare_run_config(base, dest, agent_config_text=config_text)
    config = tomllib.loads((dest / "agent.toml").read_text())
    telemetry = config["advanced_settings"]["runtime"]["telemetry"]
    assert telemetry["enabled"] is True
    assert telemetry["base_url"] == "https://langfuse.example"
    assert telemetry["public_key"] == "pk-test"
    assert telemetry["secret_key"] == secret
    assert telemetry["upload_screenshots"] is False
    assert telemetry["environment"] == "benchmark"

    # An explicit opt-out is preserved; missing CI credentials fail clearly.
    monkeypatch.delenv("LANGFUSE_SECRET_KEY")
    explicit = config_text + '[advanced_settings.runtime.telemetry]\nenabled = false\n'
    webui.prepare_run_config(base, dest, agent_config_text=explicit)
    config = tomllib.loads((dest / "agent.toml").read_text())
    assert config["advanced_settings"]["runtime"]["telemetry"] == {"enabled": False}
    with pytest.raises(ValueError, match="benchmark telemetry requires LANGFUSE_SECRET_KEY"):
        webui.prepare_run_config(base, dest, agent_config_text=config_text)


def test_prepare_run_config_uses_agent_config_text(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml.template").write_text('[model]\nprovider = "template"\n', encoding="utf-8")
    agent_config = 'custom_instruction = "custom"\n[model]\nprovider = "saved"\n'

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest, agent_config_text=agent_config)

    rendered = (dest / "agent.toml").read_text(encoding="utf-8")
    assert 'custom_instruction = "custom"' in rendered
    assert 'provider = "saved"' in rendered
    assert "voice_streaming_tts_enabled = false" in rendered
    assert "voice_tool_call_speech = false" in rendered
    assert "voice_progress_speech_enabled = false" in rendered
    assert (dest / "control_token").exists()
    assert (dest / "memory").is_dir()


def test_prepare_run_config_materializes_agent_credential_environment_references(
    tmp_path: Path, monkeypatch
):
    base = tmp_path / "base"
    base.mkdir()
    monkeypatch.setenv("ANTHROPIC_AUTH_TOKEN", "sk-agent-secret")
    agent_config = """
[model_providers.benchmark]
type = "anthropic"
api_key = "$ANTHROPIC_AUTH_TOKEN"

[model]
provider = "benchmark"
model = "claude-opus-5"
""".strip()

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest, agent_config_text=agent_config)

    rendered = (dest / "agent.toml").read_text(encoding="utf-8")
    assert 'api_key = "sk-agent-secret"' in rendered
    assert "$ANTHROPIC_AUTH_TOKEN" not in rendered


def test_prepare_run_config_rejects_missing_agent_credential_environment_reference(
    tmp_path: Path, monkeypatch
):
    base = tmp_path / "base"
    base.mkdir()
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    agent_config = """
[model_providers.benchmark]
type = "anthropic"
api_key = "$ANTHROPIC_AUTH_TOKEN"

[model]
provider = "benchmark"
model = "claude-opus-5"
""".strip()

    with pytest.raises(ValueError, match="ANTHROPIC_AUTH_TOKEN"):
        webui.prepare_run_config(
            base,
            tmp_path / "dest",
            agent_config_text=agent_config,
        )


def test_prepare_run_config_does_not_copy_runtime_state(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml").write_text('[model]\nprovider = "fake"\n', encoding="utf-8")
    for name in ("memory", "log", "cache", "sessions", "skill-state"):
        state_dir = base / name
        state_dir.mkdir()
        (state_dir / "stale-state").write_text("stale", encoding="utf-8")
    (base / "memory" / "extraction.yaml").write_text("hot_window_events: 20\n", encoding="utf-8")

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest)

    for name in ("memory", "log", "cache", "sessions", "skill-state"):
        assert not (dest / name / "stale-state").exists()
    assert (dest / "memory").is_dir()
    assert (dest / "memory" / "extraction.yaml").read_text(encoding="utf-8") == "hot_window_events: 20\n"
    assert (dest / "log").is_dir()
    assert (dest / "skill-state").is_dir()


def test_prepare_run_config_includes_bundled_skills(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml.template").write_text('[model]\nprovider = "template"\n', encoding="utf-8")

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest)

    assert (dest / "skills" / "device-operator" / "SKILL.md").exists()


def test_prepare_run_config_merges_missing_bundled_skills_with_custom_skills(tmp_path: Path):
    base = tmp_path / "base"
    custom_skill = base / "skills" / "custom-skill"
    custom_skill.mkdir(parents=True)
    (base / "agent.toml.template").write_text('[model]\nprovider = "template"\n', encoding="utf-8")
    (custom_skill / "SKILL.md").write_text("---\nname: custom-skill\n---\n", encoding="utf-8")

    dest = tmp_path / "dest"
    webui.prepare_run_config(base, dest)

    assert (dest / "skills" / "custom-skill" / "SKILL.md").exists()
    assert (dest / "skills" / "device-operator" / "SKILL.md").exists()


def test_prepare_run_config_requires_agent_config_source(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    dest = tmp_path / "dest"

    with pytest.raises(FileNotFoundError, match=r"agent\.toml"):
        webui.prepare_run_config(base, dest)

    assert not dest.exists()


def test_agent_config_manager_requires_agent_config_source(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    manager = runner_config.AgentConfigManager(
        base_config_dir=base,
        config_path=tmp_path / "runs" / "agent.toml",
    )

    with pytest.raises(FileNotFoundError, match=r"agent\.toml"):
        manager.get_config()


@pytest.mark.parametrize("source_name", ["agent.toml", "agent.toml.template"])
def test_agent_config_manager_rejects_invalid_generated_config(
    tmp_path: Path, source_name: str
):
    base = tmp_path / "base"
    base.mkdir()
    (base / source_name).write_text("invalid = [", encoding="utf-8")
    config_path = tmp_path / "runs" / "agent.toml"
    manager = runner_config.AgentConfigManager(
        base_config_dir=base,
        config_path=config_path,
    )

    with pytest.raises(ValueError, match=r"invalid agent\.toml"):
        manager.get_config()

    assert not config_path.exists()


def test_agent_config_manager_reset_preserves_saved_config_without_source(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = base / "agent.toml"
    saved_content = '[model]\nprovider = "saved"\n'
    config_path.write_text(saved_content, encoding="utf-8")
    manager = runner_config.AgentConfigManager(base_config_dir=base)

    with pytest.raises(FileNotFoundError, match=r"agent\.toml"):
        manager.reset_config()

    assert config_path.read_text(encoding="utf-8") == saved_content


def test_agent_config_manager_reset_default_path_uses_template(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = base / "agent.toml"
    config_path.write_text('[model]\nprovider = "saved"\n', encoding="utf-8")
    (base / "agent.toml.template").write_text(
        '[model]\nprovider = "template"\n', encoding="utf-8"
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base)

    content, source = manager.reset_config()

    assert source == "generated"
    assert 'provider = "template"' in content
    assert config_path.read_text(encoding="utf-8") == content


def test_agent_config_manager_migrates_saved_config_missing_voice_defaults(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text('custom_instruction = "saved"\n[model]\nprovider = "fake"\n', encoding="utf-8")
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    content, source = manager.get_config()

    assert source == "saved"
    assert "voice_streaming_tts_enabled = false" in content
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content
    assert "[voice_settings.classic.runtime]" in content
    assert content.index("[model]") < content.index("[voice_settings.classic.runtime]")
    assert config_path.read_text(encoding="utf-8") == content


def test_agent_config_manager_ignores_table_keys_when_migrating_voice_defaults(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text(
        'custom_instruction = "saved"\n[model]\nvoice_streaming_tts_enabled = true\nprovider = "fake"\n',
        encoding="utf-8",
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    content, source = manager.get_config()

    assert source == "saved"
    assert "voice_streaming_tts_enabled = false" in content
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content
    assert "[model]\nvoice_streaming_tts_enabled = true" in content
    assert "[voice_settings.classic.runtime]" in content
    assert content.index("[model]") < content.index("[voice_settings.classic.runtime]")


def test_agent_config_manager_recognizes_commented_voice_runtime_header(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text(
        "[voice_settings.classic.runtime] # benchmark overrides\n"
        "voice_streaming_tts_enabled = true\n"
        "[model_settings.model]\n"
        'provider = "fake"\n',
        encoding="utf-8",
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    content, source = manager.get_config()

    assert source == "saved"
    assert content.count("[voice_settings.classic.runtime]") == 1
    assert content.count("voice_streaming_tts_enabled") == 1
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content


def test_agent_config_manager_migrates_legacy_runtime_table(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text(
        "[runtime] # retired benchmark settings\n"
        "voice_streaming_tts_enabled = true\n"
        "[model_settings.model]\n"
        'provider = "fake"\n',
        encoding="utf-8",
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    content, source = manager.get_config()

    assert source == "saved"
    assert "[runtime]" not in content
    assert "[voice_settings.classic.runtime] # retired benchmark settings" in content
    assert content.count("voice_streaming_tts_enabled") == 1
    assert "voice_streaming_tts_enabled = true" in content
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content
    assert config_path.read_text(encoding="utf-8") == content


def test_agent_config_manager_rejects_legacy_and_grouped_runtime_tables(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text(
        "[runtime]\n"
        "voice_streaming_tts_enabled = true\n"
        "[voice_settings.classic.runtime]\n"
        "voice_tool_call_speech = false\n",
        encoding="utf-8",
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    with pytest.raises(ValueError, match=r"both \[runtime\]"):
        manager.get_config()


def test_agent_config_manager_preserves_quoted_root_voice_default(tmp_path: Path):
    base = tmp_path / "base"
    base.mkdir()
    config_path = tmp_path / "runs" / "agent.toml"
    config_path.parent.mkdir()
    config_path.write_text(
        '"voice_streaming_tts_enabled" = true\n[model]\nprovider = "fake"\n',
        encoding="utf-8",
    )
    manager = runner_config.AgentConfigManager(base_config_dir=base, config_path=config_path)

    content, _ = manager.get_config()

    assert content.count("voice_streaming_tts_enabled") == 1
    assert '"voice_streaming_tts_enabled" = true' in content
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content
    assert "[voice_settings.classic.runtime]" in content

    assert "voice_progress_speech_enabled = false" in content


def test_webui_agent_config_persists_under_runs_dir(tmp_path: Path, monkeypatch):
    base = tmp_path / "base"
    base.mkdir()
    (base / "agent.toml.template").write_text(
        '[model]\nprovider = "{{AIDEN_BENCHMARK_AGENT_PROVIDER}}"\nmodel = "{{AIDEN_BENCHMARK_AGENT_MODEL}}"\n',
        encoding="utf-8",
    )
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_PROVIDER", "openai")
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_MODEL", "gpt-test")
    app = webui.BenchmarkWebApp(webui.WebUIConfig(runs_dir=tmp_path / "runs", base_config_dir=base))

    initial = app.get_agent_config()
    assert initial["path"] == str(tmp_path / "runs" / "agent.toml")
    assert initial["source"] == "generated"
    assert 'provider = "openai"' in initial["content"]

    saved = 'custom_instruction = "saved"\n[model]\nprovider = "fake"\n'
    updated = app.save_agent_config({"content": saved})
    assert updated["source"] == "saved"
    saved_content = (tmp_path / "runs" / "agent.toml").read_text(encoding="utf-8")
    assert 'custom_instruction = "saved"' in saved_content
    assert "voice_streaming_tts_enabled = false" in saved_content
    assert "voice_tool_call_speech = false" in saved_content
    assert "voice_progress_speech_enabled = false" in saved_content
    assert 'provider = "fake"' in saved_content
    assert app.get_agent_config()["content"] == saved_content


def test_webui_settings_persist_judge_and_device_environments(tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    saved = app.save_webui_settings(
        {
            "judge": {
                "enabled": True,
                "model": "anthropic/test-judge",
                "api_key": "sk-judge-secret",
            },
            "device_environments": [
                {
                    "id": "dev-1",
                    "name": "Bench board",
                    "endpoint": "http://192.168.1.50:8080",
                }
            ],
            "selected_environment_id": "dev-1",
        }
    )

    assert saved["judge"] == {
        "enabled": True,
        "model": "anthropic/test-judge",
        "base_url": webui.DEFAULT_JUDGE_BASE_URL,
        "has_api_key": True,
    }
    assert "api_key" not in saved["judge"]
    assert saved["device_environments"][0]["name"] == "Bench board"
    assert saved["selected_environment_id"] == "dev-1"

    settings_text = (tmp_path / "runs" / webui.WEBUI_SETTINGS_FILE).read_text(encoding="utf-8")
    persisted = json.loads(settings_text)
    assert persisted["judge"]["has_api_key"] is True
    assert "api_key" not in persisted["judge"]
    assert "sk-judge-secret" not in settings_text

    reloaded = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    assert reloaded.get_webui_settings() == saved


def test_webui_settings_migrates_legacy_plaintext_judge_key(tmp_path: Path):
    runs_dir = tmp_path / "runs"
    runs_dir.mkdir()
    (runs_dir / webui.WEBUI_SETTINGS_FILE).write_text(
        json.dumps(
            {
                "judge": {
                    "enabled": True,
                    "model": "anthropic/test-judge",
                    "api_key": "sk-legacy-secret",
                }
            }
        ),
        encoding="utf-8",
    )

    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=runs_dir,
            base_config_dir=tmp_path / "config",
        )
    )

    assert app.get_webui_settings()["judge"]["has_api_key"] is True
    settings_text = (runs_dir / webui.WEBUI_SETTINGS_FILE).read_text(encoding="utf-8")
    assert "api_key" not in json.loads(settings_text)["judge"]
    assert "sk-legacy-secret" not in settings_text


def test_start_job_uses_persisted_judge_settings(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    app.save_webui_settings(
        {
            "judge": {
                "enabled": True,
                "model": "anthropic/persisted-judge",
                "api_key": "sk-persisted",
            }
        }
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)

    job = app.start_job({"endpoint": "http://127.0.0.1:9090", "suites": ["suite.json"]})

    assert job["judge_model"] == "anthropic/persisted-judge"
    assert job["judge_api_key_set"] is True
    assert app._job_judge_api_keys[job["id"]] == "sk-persisted"


def test_start_job_records_judge_settings_without_exposing_api_key(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)
    job = app.start_job(
        {
            "endpoint": "http://127.0.0.1:9090",
            "suites": ["suite.json"],
            "judge_model": "anthropic/test-judge",
            "judge_api_key": "sk-judge-secret",
        }
    )

    assert job["judge_model"] == "anthropic/test-judge"
    assert job["judge_api_key_set"] is True
    assert "judge_api_key" not in job
    assert app._job_judge_api_keys[job["id"]] == "sk-judge-secret"


def test_start_job_rejects_recovered_mobilegym_environment(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    recovered = webui.MobileGymEnvironment(
        id="mg-old",
        name="MobileGym recovered",
        endpoint="http://host.docker.internal:19090",
        public_endpoint="http://127.0.0.1:19090",
        web_url="http://127.0.0.1:18173",
        status="stale",
        container_name="aiden-mobilegym-env-mg-old",
    )
    app.env_manager._environments[recovered.id] = recovered
    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", lambda endpoint: 3)

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)

    with pytest.raises(ValueError, match="fresh MobileGym environment"):
        app.start_job(
            {
                "environment_type": "mobilegym",
                "environment_id": recovered.id,
                "suites": ["suite.json"],
                "no_judge": True,
            }
        )


def test_start_job_persists_job_record_without_api_key(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)
    queried = []

    def fake_read_concurrency(endpoint: str):
        queried.append(endpoint)
        return 4

    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", fake_read_concurrency)

    job = app.start_job(
        {
            "endpoint": "http://127.0.0.1:9090",
            "suites": ["suite.json"],
            "judge_api_key": "sk-judge-secret",
        }
    )

    record_path = tmp_path / "runs" / job["id"] / webui.JOB_RECORD_FILE
    record = json.loads(record_path.read_text(encoding="utf-8"))
    assert record["id"] == job["id"]
    assert record["suites"] == ["suite.json"]
    assert record["judge_api_key_set"] is True
    assert "judge_api_key" not in record


def test_webui_loads_persisted_job_records_and_marks_active_stopped(tmp_path: Path):
    runs_dir = tmp_path / "runs"
    job_dir = runs_dir / "job-test"
    raw_runs_dir = job_dir / "raw"
    raw_runs_dir.mkdir(parents=True)
    state_file = job_dir / "state.json"
    state_file.write_text(json.dumps({"status": "running", "run_id": "job-test"}), encoding="utf-8")
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        status="running",
        created_at="2026-06-22T00:00:00+00:00",
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(state_file),
        runner_log=str(job_dir / "runner.log"),
        daemon_log=str(job_dir / "daemon.log"),
        task_records=[
            webui.TaskRecord(
                id="suite-json-t1",
                suite="suite.json",
                task_id="t1",
                benchmark_task_id="suite.json:t1",
                status="running",
                runner_log=str(job_dir / "workers" / "suite-json-t1.runner.log"),
                daemon_log=str(job_dir / "workers" / "suite-json-t1.daemon.log"),
            )
        ],
    )
    webui.persist_job_record(job)

    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=runs_dir,
            base_config_dir=tmp_path / "config",
        )
    )
    loaded = app.get_job("job-test")

    assert loaded is not None
    assert loaded["status"] == "stopped"
    assert loaded["message"] == "restored after WebUI restart"
    assert loaded["progress"]["status"] == "stopped"
    assert loaded["task_records"][0]["benchmark_task_id"] == "suite.json:t1"
    assert loaded["task_records"][0]["status"] == "stopped"
    record = json.loads((job_dir / webui.JOB_RECORD_FILE).read_text(encoding="utf-8"))
    assert record["status"] == "stopped"
    assert record["task_records"][0]["status"] == "stopped"


def test_task_record_updates_are_persisted(tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        environment_endpoint="http://127.0.0.1:19090",
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        daemon_log=str(tmp_path / "runs" / "job-test" / "daemon.log"),
    )
    app._jobs[job.id] = job

    record = app._ensure_task_record(job, "suite.json", "t1")
    app._set_task_record(job, record.id, status="running", agent_url="http://127.0.0.1:18081")

    persisted = json.loads((tmp_path / "runs" / "job-test" / webui.JOB_RECORD_FILE).read_text(encoding="utf-8"))
    assert persisted["task_records"][0]["status"] == "running"
    assert persisted["task_records"][0]["agent_url"] == "http://127.0.0.1:18081"


def test_start_job_derives_mobilegym_environment_endpoint(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    env = webui.MobileGymEnvironment(
        id="env-1",
        name="MobileGym",
        endpoint="http://host.docker.internal:19090",
        public_endpoint="http://127.0.0.1:19090",
        web_url="http://127.0.0.1:18173",
        status="running",
        parallel_envs=3,
    )
    app.env_manager._environments["env-1"] = env

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)
    queried = []

    def fake_read_concurrency(endpoint: str):
        queried.append(endpoint)
        return 4

    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", fake_read_concurrency)

    job = app.start_job(
        {
            "environment_type": "mobilegym",
            "environment_id": "env-1",
            "suites": ["suite.json"],
            "no_judge": True,
        }
    )

    assert job["environment_endpoint"] == "http://127.0.0.1:19090"
    assert job["endpoint"] == "http://127.0.0.1:19090"
    assert job["docker_endpoint"] == "http://host.docker.internal:19090"
    assert job["environment_type"] == "mobilegym"
    assert job["environment_web_url"] == "http://127.0.0.1:18173"
    assert job["parallel_tasks"] == 4
    assert queried == ["http://127.0.0.1:19090"]


def test_start_job_rejects_mismatched_environment_endpoints(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    with pytest.raises(ValueError, match="does not match resolved environment endpoint"):
        app.start_job(
            {
                "endpoint": "http://host.docker.internal:19090",
                "environment_endpoint": "http://127.0.0.1:19091",
                "environment_type": "mobilegym",
                "environment_id": "missing-env",
                "suites": ["suite.json"],
                "no_judge": True,
            }
        )


def test_start_job_rejects_missing_resolved_environment_endpoint(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    with pytest.raises(ValueError, match="resolved environment endpoint is required"):
        app.start_job(
            {
                "endpoint": "http://host.docker.internal:19090",
                "environment_type": "mobilegym",
                "environment_id": "missing-env",
                "suites": ["suite.json"],
                "no_judge": True,
            }
        )


def test_start_job_uses_device_endpoint_as_environment_url(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)

    job = app.start_job(
        {
            "endpoint": "http://127.0.0.1:19090/",
            "environment": {"id": "dev-1", "name": "Desk bridge", "type": "device"},
            "suites": ["suite.json"],
            "no_judge": True,
        }
    )

    assert job["environment_endpoint"] == "http://127.0.0.1:19090"
    assert job["environment_type"] == "device"


def test_start_job_uses_mock_environment_without_device_endpoint(
    tmp_path: Path,
    monkeypatch,
):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "mock.json").write_text(
        json.dumps(
            {
                "name": "mock",
                "mock_environment": {
                    "phone_bridge": {"platform": "ios"},
                    "tools": {},
                },
                "tasks": [{"id": "t1", "category": "diagnostic"}],
            }
        ),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)

    job = app.start_job({"suites": ["mock.json"], "no_judge": True})

    assert job["environment_type"] == "mock"
    assert job["environment_name"] == "Mock Aiden App environment"
    assert job["endpoint"] == ""
    assert job["environment_endpoint"] == ""
    assert job["agent_url"] == ""
    assert job["parallel_tasks"] == 1


def test_start_job_rejects_mixed_mock_and_external_suites(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "mock.json").write_text(
        json.dumps(
            {
                "name": "mock",
                "mock_environment": {
                    "phone_bridge": {"platform": "ios"},
                    "tools": {},
                },
                "tasks": [{"id": "t1", "category": "diagnostic"}],
            }
        ),
        encoding="utf-8",
    )
    (suites / "device.json").write_text(
        json.dumps(
            {"name": "device", "tasks": [{"id": "t2", "category": "diagnostic"}]}
        ),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )

    with pytest.raises(ValueError, match="separate jobs"):
        app.start_job(
            {
                "endpoint": "http://127.0.0.1:19090",
                "suites": ["mock.json", "device.json"],
                "no_judge": True,
            }
        )


def test_run_suite_passes_judge_model_and_api_key_env(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        agent_url="http://127.0.0.1:18080",
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        judge_model="anthropic/test-judge",
        judge_base_url="https://judge.example.com/v1",
        judge_api_key_set=True,
    )
    app._job_judge_api_keys[job.id] = "sk-judge-secret"
    captured = {}

    class FakeProc:
        def wait(self):
            return 0

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["env"] = kwargs.get("env") or {}
        return FakeProc()

    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)

    app._run_suite(job, "suite.json")

    assert "--judge-model" in captured["cmd"]
    assert captured["cmd"][captured["cmd"].index("--judge-model") + 1] == "anthropic/test-judge"
    assert "--judge-base-url" in captured["cmd"]
    assert captured["cmd"][captured["cmd"].index("--judge-base-url") + 1] == "https://judge.example.com/v1"
    assert "--no-judge" not in captured["cmd"]
    assert captured["env"]["AIDEN_BENCHMARK_JUDGE_API_KEY"] == "sk-judge-secret"
    assert "OPENROUTER_API_KEY" not in captured["env"]


def test_run_suite_passes_mobilegym_environment_url(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://host.docker.internal:19090",
        docker_endpoint="http://host.docker.internal:19090",
        environment_endpoint="http://127.0.0.1:19090",
        suites=["suite.json"],
        environment_type="mobilegym",
        agent_url="http://127.0.0.1:18080",
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        no_judge=True,
    )
    captured = {}

    class FakeProc:
        def wait(self):
            return 0

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["env"] = kwargs["env"]
        return FakeProc()

    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)

    app._run_suite(job, "suite.json")

    assert "--environment-url" in captured["cmd"]
    assert captured["cmd"][captured["cmd"].index("--environment-url") + 1] == "http://127.0.0.1:19090"
    assert captured["env"][webui.MOBILEGYM_PREFLIGHT_COMPLETE_ENV] == "1"


def test_shared_daemon_job_uses_one_benchmark_task_id_for_daemon_and_runner(
    tmp_path: Path, monkeypatch
):
    # A device job runs every task through a single daemon. If that daemon and
    # the runner do not claim the same route id, a bridge that enforces
    # single-environment ownership answers every tool call with
    # 429 no_bridge_env_available.
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
            build_daemon_image=False,
        )
    )
    config_dir = tmp_path / "config"
    config_dir.mkdir()
    (config_dir / "agent.toml").write_text(
        '[model]\nprovider = "fake"\n',
        encoding="utf-8",
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://host.docker.internal:8899",
        docker_endpoint="http://host.docker.internal:8899",
        environment_endpoint="http://127.0.0.1:8899",
        suites=["suite.json"],
        environment_type="device",
        agent_url="http://127.0.0.1:18080",
        config_dir=str(tmp_path / "runs" / "job-test" / "config"),
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        daemon_log=str(tmp_path / "runs" / "job-test" / "daemon.log"),
        no_judge=True,
    )
    captured: dict[str, Any] = {}

    class FakeProc:
        def wait(self):
            return 0

    def fake_start_daemon_compose(*args, **kwargs):
        captured["daemon_task_id"] = kwargs.get("benchmark_task_id")
        return "container-id"

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        return FakeProc()

    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        webui,
        "read_environment_health",
        lambda endpoint: {"platform": "ios"},
    )
    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(app, "_wait_for_daemon", lambda *args, **kwargs: None)
    monkeypatch.setattr(app, "_refresh_job_report", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)
    releases: list[tuple[str, str | None]] = []
    monkeypatch.setattr(
        webui,
        "call_environment_release",
        lambda environment_url, timeout=30, task_id=None: releases.append((environment_url, task_id)),
    )

    app._run_job(job)

    expected = webui.job_benchmark_task_id("job-test")
    assert captured["daemon_task_id"] == expected
    assert "--target-platform" not in captured["cmd"]
    assert "--resolved-target-platform" not in captured["cmd"]
    assert captured["cmd"][captured["cmd"].index("--benchmark-task-id") + 1] == expected
    # A stopped or crashed job must not leave the lease behind: the id is never
    # reused, so a leak would 429 every later job.
    assert releases == [("http://127.0.0.1:8899", expected)]


def test_mobilegym_task_worker_uses_task_id_for_daemon_and_runner(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    suite_path = suites / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "suite",
                "tasks": [
                    {
                        "id": "t1",
                        "category": "diagnostic",
                        "description_for_judge": "judge",
                        "prompt": "prompt",
                        "rubric": [{"id": "ok", "check": "ok"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
            build_daemon_image=False,
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://host.docker.internal:19090",
        docker_endpoint="http://host.docker.internal:19090",
        environment_endpoint="http://127.0.0.1:19090",
        suites=["suite.json"],
        environment_type="mobilegym",
        agent_url="http://127.0.0.1:18080",
        container_name="aiden-benchmark-agent-job-test",
        config_dir=str(tmp_path / "runs" / "job-test" / "config"),
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        daemon_log=str(tmp_path / "runs" / "job-test" / "daemon.log"),
        no_judge=True,
        parallel_tasks=2,
        target_platform="android",
    )
    captured = {}
    releases = []

    monkeypatch.setattr(webui, "docker_published_port", lambda container_id, container_port: 18081)
    monkeypatch.setattr(app, "_wait_for_daemon", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        webui,
        "call_environment_release",
        lambda environment_url, timeout=30, task_id=None: releases.append((environment_url, timeout, task_id)),
    )

    def fake_start_daemon_compose(*args, **kwargs):
        captured["worker_job"] = args[0]
        captured["benchmark_task_id"] = kwargs["benchmark_task_id"]
        captured["host_port"] = kwargs["host_port"]
        captured["compose_log_path"] = kwargs["log_path"]
        return "container-id"

    def fake_run_runner_process(parent_job, cmd, env, *, owner_job_id=None, extra_owner_ids=None):
        captured["runner_job"] = parent_job
        captured["owner_job_id"] = owner_job_id
        captured["extra_owner_ids"] = extra_owner_ids
        captured["cmd"] = cmd
        captured["env"] = env
        run_id = cmd[cmd.index("--run-id") + 1]
        run_path = raw_runs_dir / run_id
        run_path.mkdir(parents=True)
        (run_path / "manifest.json").write_text(
            json.dumps({"totals": {"tasks": 1, "passed": 1}}),
            encoding="utf-8",
        )
        return 0

    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(app, "_run_runner_process", fake_run_runner_process)

    result = app._run_mobilegym_task_worker(job, "suite.json", suite_path, "t1")

    assert captured["host_port"] == 0
    assert captured["benchmark_task_id"] == "suite.json:t1"
    assert captured["worker_job"].runner_log != job.runner_log
    assert captured["worker_job"].daemon_log != job.daemon_log
    assert captured["compose_log_path"] == Path(captured["worker_job"].runner_log)
    assert captured["runner_job"].runner_log == captured["worker_job"].runner_log
    assert captured["owner_job_id"] == job.id
    assert captured["extra_owner_ids"] == [webui.task_worker_key(job.id, job.task_records[0].id)]
    assert captured["cmd"][captured["cmd"].index("--task-id") + 1] == "t1"
    assert captured["cmd"][captured["cmd"].index("--benchmark-task-id") + 1] == "suite.json:t1"
    assert captured["cmd"][captured["cmd"].index("--benchmark-token-file") + 1] == str(
        Path(job.config_dir) / "control_token"
    )
    assert captured["cmd"][captured["cmd"].index("--environment-url") + 1] == "http://127.0.0.1:19090"
    assert captured["env"][webui.MOBILEGYM_PREFLIGHT_COMPLETE_ENV] == "1"
    assert "--target-platform" not in captured["cmd"]
    assert releases == [("http://127.0.0.1:19090", 2, "suite.json:t1")]
    assert result["exit_code"] == 0
    assert result["manifest"]["totals"]["passed"] == 1
    assert len(job.task_records) == 1
    task_record = job.task_records[0]
    assert task_record.status == "passed"
    assert task_record.benchmark_task_id == "suite.json:t1"
    assert task_record.agent_url == "http://127.0.0.1:18081"
    assert task_record.report_url == f"/reports/{job.id}/{result['run_id']}/report.html"
    assert task_record.screen_url == webui.webui_task_screen_url(job.id, task_record.id)


@pytest.mark.parametrize("parallel_tasks", [1, 2])
def test_mobilegym_webui_job_preflights_before_starting_workers(
    tmp_path: Path, monkeypatch, parallel_tasks: int
):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=tmp_path / "suites",
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "base-config",
            build_daemon_image=False,
        )
    )
    job_dir = tmp_path / "runs" / "job-test"
    (job_dir / "raw").mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        environment_endpoint="http://127.0.0.1:19090",
        suites=["suite.json"],
        environment_type="mobilegym",
        agent_url="http://127.0.0.1:18080",
        config_dir=str(job_dir / "config"),
        raw_runs_dir=str(job_dir / "raw"),
        state_file=str(job_dir / "state.json"),
        runner_log=str(job_dir / "runner.log"),
        daemon_log=str(job_dir / "daemon.log"),
        no_judge=True,
        parallel_tasks=parallel_tasks,
    )
    events = []

    monkeypatch.setattr(app, "get_agent_config", lambda: {"content": ""})
    monkeypatch.setattr(webui, "prepare_run_config", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        webui,
        "read_environment_health",
        lambda endpoint: {
            "bridge_type": "mobilegym",
            "platform": "android",
            "env_count": parallel_tasks,
        },
    )
    monkeypatch.setattr(
        webui,
        "preflight_mobilegym_environment",
        lambda endpoint, health=None: events.append(("preflight", endpoint, health)),
    )
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(app, "_refresh_job_report", lambda *args, **kwargs: None)
    if parallel_tasks > 1:
        monkeypatch.setattr(
            app,
            "_run_mobilegym_suite_parallel",
            lambda current_job, suite_key: events.append(("parallel", suite_key)),
        )
    else:
        monkeypatch.setattr(webui, "start_daemon_compose", lambda *args, **kwargs: "container-id")
        monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
        monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
        monkeypatch.setattr(app, "_wait_for_daemon", lambda *args, **kwargs: None)
        monkeypatch.setattr(app, "_release_job_environment", lambda *args, **kwargs: None)
        monkeypatch.setattr(
            app,
            "_run_suite",
            lambda current_job, suite_key: events.append(("single", suite_key)),
        )

    app._run_job(job)

    assert events[0][0] == "preflight"
    assert events[0][1] == "http://127.0.0.1:19090"
    assert events[0][2]["env_count"] == parallel_tasks
    assert events[1] == ("parallel" if parallel_tasks > 1 else "single", "suite.json")


def test_read_task_log_returns_task_worker_logs(tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    job_dir = tmp_path / "runs" / "job-test"
    worker_dir = job_dir / "workers"
    worker_dir.mkdir(parents=True)
    runner_log = worker_dir / "suite-json-t1.runner.log"
    daemon_log = worker_dir / "suite-json-t1.daemon.log"
    runner_log.write_text("runner output\n", encoding="utf-8")
    daemon_log.write_text("daemon output\n", encoding="utf-8")
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        runner_log=str(job_dir / "runner.log"),
        daemon_log=str(job_dir / "daemon.log"),
        task_records=[
            webui.TaskRecord(
                id="suite-json-t1",
                suite="suite.json",
                task_id="t1",
                benchmark_task_id="suite.json:t1",
                runner_log=str(runner_log),
                daemon_log=str(daemon_log),
            )
        ],
    )
    app._jobs[job.id] = job

    text = app.read_task_log("job-test", "suite-json-t1")

    assert text is not None
    assert "== runner ==" in text
    assert "runner output" in text
    assert "== daemon ==" in text
    assert "daemon output" in text


def test_refresh_job_report_merges_task_runs_into_single_report(tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)

    def write_task_run(run_id: str, suite_key: str, task_id: str, status: str) -> None:
        run_dir = raw_runs_dir / run_id
        artifact_dir = run_dir / "tasks" / task_id
        artifact_dir.mkdir(parents=True)
        (artifact_dir / "trace.json").write_text(
            json.dumps({"tool_calls": [], "final_response": f"done {task_id}"}),
            encoding="utf-8",
        )
        (artifact_dir / "history.json").write_text(
            json.dumps([{"type": "user", "content": f"prompt {task_id}"}]),
            encoding="utf-8",
        )
        (run_dir / "results.jsonl").write_text(
            json.dumps(
                {
                    "suite": suite_key,
                    "run_id": run_id,
                    "task_id": task_id,
                    "category": "diagnostic",
                    "attempt": 1,
                    "status": status,
                    "rubric": [],
                    "rubric_pass_count": 1 if status == "passed" else 0,
                    "rubric_total": 1,
                    "metrics": {"wall_ms": 7, "tool_calls": 0},
                    "artifact_dir": str(artifact_dir),
                }
            )
            + "\n",
            encoding="utf-8",
        )

    write_task_run("run-a", "suite/a.json", "same-task", "passed")
    write_task_run("run-b", "suite/b.json", "same-task", "failed")
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite/a.json", "suite/b.json"],
        raw_runs_dir=str(raw_runs_dir),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        started_at="2026-06-22T00:00:00+00:00",
        suite_results=[
            {"suite": "suite/a.json", "task_id": "same-task", "exit_code": 0, "run_id": "run-a"},
            {"suite": "suite/b.json", "task_id": "same-task", "exit_code": 1, "run_id": "run-b"},
        ],
    )

    app._refresh_job_report(job)

    assert job.report_url == "/reports/job-test/_job-report/report.html"
    report_dir = raw_runs_dir / "_job-report"
    manifest = json.loads((report_dir / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["totals"] == {"tasks": 2, "passed": 1, "failed": 1, "skipped": 0, "judge_error": 0, "timeout": 0}
    metrics = json.loads((report_dir / "metrics.json").read_text(encoding="utf-8"))
    assert metrics["metrics_k"] == 1
    assert metrics["aggregate"]["pass_at_1"]["value"] == 0.5
    assert metrics["aggregate"]["pass_at_k"]["value"] == 0.5
    assert (report_dir / "summary.md").exists()
    rows = [
        json.loads(line)
        for line in (report_dir / "results.jsonl").read_text(encoding="utf-8").splitlines()
    ]
    assert len(rows) == 2
    assert rows[0]["task_id"] != rows[1]["task_id"]
    assert rows[0]["metrics"]["source_task_id"] == "same-task"
    assert rows[1]["metrics"]["source_task_id"] == "same-task"
    for row in rows:
        assert (report_dir / "tasks" / row["task_id"] / "trace.json").exists()
    html = (report_dir / "report.html").read_text(encoding="utf-8")
    assert "Benchmark:" in html
    assert "Capability Metrics" in html
    assert "Pass@k" in html
    assert rows[0]["task_id"] in html
    assert rows[1]["task_id"] in html


def test_job_report_uses_source_manifest_metrics_k_when_job_record_is_legacy(tmp_path: Path):
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    suite_results = []
    for attempt in (1, 2):
        run_id = f"run-{attempt}"
        run_dir = raw_runs_dir / run_id
        run_dir.mkdir(parents=True)
        (run_dir / "manifest.json").write_text(
            json.dumps({"metrics_k": 2}), encoding="utf-8"
        )
        (run_dir / "results.jsonl").write_text(
            json.dumps(
                {
                    "suite": "suite/a.json",
                    "task_id": "same-task",
                    "attempt": attempt,
                    "status": "passed",
                    "metrics": {"success": True, "agent_eligible": True, "quality_score": 1.0},
                }
            )
            + "\n",
            encoding="utf-8",
        )
        suite_results.append({"suite": "suite/a.json", "run_id": run_id})

    job = webui.Job(
        id="job-test",
        endpoint="",
        docker_endpoint="",
        suites=["suite/a.json"],
        raw_runs_dir=str(raw_runs_dir),
        suite_results=suite_results,
    )

    webui.write_job_report(job)

    metrics = json.loads((raw_runs_dir / "_job-report" / "metrics.json").read_text(encoding="utf-8"))
    assert metrics["metrics_k"] == 2
    assert metrics["aggregate"]["pass_at_k"]["value"] == 1.0


def test_job_report_fallback_rows_are_ineligible_for_agent_metrics():
    row = webui.fallback_report_row(
        {"suite": "suite/a.json", "task_id": "task", "exit_code": 1},
        "job-test",
        1,
        set(),
    )

    assert row is not None
    assert row["metrics"]["agent_eligible"] is False
    assert row["metrics"]["success"] is None
    assert row["metrics"]["failure_class"] == "environment"


def test_refresh_job_report_runs_llm_analysis_by_default_with_webui_judge_key(monkeypatch, tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    run_dir = raw_runs_dir / "run-a"
    artifact_dir = run_dir / "tasks" / "t1"
    artifact_dir.mkdir(parents=True)
    (artifact_dir / "trace.json").write_text(json.dumps({"tool_calls": [], "final_response": ""}), encoding="utf-8")
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "suite": "suite.json",
                "run_id": "run-a",
                "task_id": "t1",
                "category": "diagnostic",
                "attempt": 1,
                "status": "failed",
                "rubric": [],
                "rubric_pass_count": 0,
                "rubric_total": 1,
                "metrics": {"wall_ms": 7, "tool_calls": 0},
                "artifact_dir": str(artifact_dir),
            }
        )
        + "\n",
        encoding="utf-8",
    )
    calls = []

    def fake_analyze(run_dir_arg, repo_root, cfg):
        calls.append((run_dir_arg, repo_root, cfg))
        (run_dir_arg / "llm_analysis.md").write_text("中文分析", encoding="utf-8")
        return webui.AnalysisResult(ok=True, markdown_path=run_dir_arg / "llm_analysis.md")

    monkeypatch.delenv("AIDEN_BENCHMARK_LLM_ANALYSIS", raising=False)
    monkeypatch.setattr(webui, "analyze_run", fake_analyze)
    with app._lock:
        app._webui_judge_api_key = "ui-judge-key"
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        raw_runs_dir=str(raw_runs_dir),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        started_at="2026-06-22T00:00:00+00:00",
        judge_model="bytedance-seed/seed-2.0-lite",
        judge_base_url="https://judge.example.com/v1",
        suite_results=[{"suite": "suite.json", "task_id": "t1", "exit_code": 1, "run_id": "run-a"}],
    )

    app._refresh_job_report(job)

    report_dir = raw_runs_dir / "_job-report"
    assert calls and calls[0][0] == report_dir
    assert calls[0][2].enabled is True
    assert calls[0][2].model == "bytedance-seed/seed-2.0-lite"
    assert calls[0][2].base_url == "https://judge.example.com/v1"
    assert calls[0][2].api_key_value == "ui-judge-key"
    html = (report_dir / "report.html").read_text(encoding="utf-8")
    assert "LLM 分析" in html
    assert "中文分析" in html


def test_refresh_job_report_falls_back_to_single_existing_report(tmp_path: Path):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["memory_v1.json"],
        raw_runs_dir=str(raw_runs_dir),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        suite_results=[
            {
                "suite": "memory_v1.json",
                "exit_code": 0,
                "run_id": "memory-run",
                "report_url": "/reports/job-test/memory-run/report.html",
            }
        ],
    )

    app._refresh_job_report(job)

    assert job.report_url == "/reports/job-test/memory-run/report.html"


def test_stop_job_marks_stopping_terminates_runner_and_removes_container(tmp_path: Path, monkeypatch):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        status="running",
        container_name="aiden-benchmark-agent-job-test",
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
    )
    proc = object()
    app._jobs[job.id] = job
    app._job_runner_procs[job.id] = proc
    terminated = []
    stopped_projects = []

    monkeypatch.setattr(webui, "terminate_process_tree", lambda p: terminated.append(p))

    monkeypatch.setattr(
        webui,
        "stop_daemon_compose",
        lambda stopped_job: stopped_projects.append(webui.daemon_compose_project(stopped_job)),
    )

    stopped = app.stop_job("job-test")

    assert stopped is not None
    assert stopped["status"] == "stopping"
    assert stopped["message"] == "stop requested"
    assert terminated == [proc]
    assert stopped_projects == ["aiden-benchmark-agent-job-test"]
    assert json.loads(Path(job.state_file).read_text(encoding="utf-8"))["status"] == "stopping"
    assert "STOP requested" in Path(job.runner_log).read_text(encoding="utf-8")


def test_stop_task_worker_marks_stopping_and_terminates_only_that_worker(tmp_path: Path, monkeypatch):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    job_dir = tmp_path / "runs" / "job-test"
    worker_dir = job_dir / "workers"
    worker_dir.mkdir(parents=True)
    record = webui.TaskRecord(
        id="suite-json-t1",
        suite="suite.json",
        task_id="t1",
        benchmark_task_id="suite.json:t1",
        status="running",
        state_file=str(worker_dir / "suite-json-t1.state.json"),
        runner_log=str(worker_dir / "suite-json-t1.runner.log"),
        daemon_log=str(worker_dir / "suite-json-t1.daemon.log"),
        container_name="aiden-benchmark-agent-job-test-suite-json-t1",
        run_id="job-test-suite-json-t1",
    )
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        status="running",
        runner_log=str(job_dir / "runner.log"),
        task_records=[record],
    )
    worker_job = webui.Job(
        id="job-test-suite-json-t1",
        endpoint=job.endpoint,
        docker_endpoint=job.docker_endpoint,
        suites=["suite.json"],
        container_name=record.container_name,
    )
    proc = object()
    task_key = webui.task_worker_key(job.id, record.id)
    app._jobs[job.id] = job
    app._job_runner_procs[job.id] = object()
    app._job_runner_procs[task_key] = proc
    app._task_daemon_jobs[task_key] = [worker_job]
    terminated = []
    stopped_projects = []

    monkeypatch.setattr(webui, "terminate_process_tree", lambda p: terminated.append(p))
    monkeypatch.setattr(
        webui,
        "stop_daemon_compose",
        lambda stopped_job: stopped_projects.append(webui.daemon_compose_project(stopped_job)),
    )

    stopped = app.stop_task_worker(job.id, record.id)

    assert stopped is not None
    assert stopped["status"] == "running"
    assert stopped["task_records"][0]["status"] == "stopping"
    assert stopped["task_records"][0]["message"] == "stop requested"
    assert terminated == [proc]
    assert stopped_projects == [record.container_name]
    assert json.loads(Path(record.state_file).read_text(encoding="utf-8"))["status"] == "stopping"
    assert "STOP requested" in Path(record.runner_log).read_text(encoding="utf-8")


def test_run_suite_tracks_runner_process_and_finishes_stopped(tmp_path: Path, monkeypatch):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    raw_runs_dir = tmp_path / "runs" / "job-test" / "raw"
    raw_runs_dir.mkdir(parents=True)
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        suites=["suite.json"],
        status="running",
        agent_url="http://127.0.0.1:18080",
        container_name="aiden-benchmark-agent-job-test",
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        no_judge=True,
    )
    app._jobs[job.id] = job
    captured = {}
    terminated = []
    stopped_projects = []

    class FakeProc:
        returncode = None

        def poll(self):
            return self.returncode

        def wait(self):
            app.stop_job(job.id)
            self.returncode = -15
            return self.returncode

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["start_new_session"] = kwargs.get("start_new_session")
        proc = FakeProc()
        captured["proc"] = proc
        return proc

    def fake_terminate(proc):
        terminated.append(proc)
        proc.returncode = -15

    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(webui, "terminate_process_tree", fake_terminate)
    monkeypatch.setattr(
        webui,
        "stop_daemon_compose",
        lambda stopped_job: stopped_projects.append(webui.daemon_compose_project(stopped_job)),
    )

    try:
        app._run_suite(job, "suite.json")
    except webui.JobStopped:
        pass
    else:
        raise AssertionError("stopped suite did not raise JobStopped")

    assert app._job_runner_procs == {}
    assert terminated == [captured["proc"]]
    assert stopped_projects == ["aiden-benchmark-agent-job-test"]
    assert job.suite_results[-1]["stopped"] is True
    assert json.loads(Path(job.state_file).read_text(encoding="utf-8"))["status"] == "stopped"
    if webui.os.name == "posix":
        assert captured["start_new_session"] is True


def test_ensure_docker_image_stops_cancelable_build(tmp_path: Path, monkeypatch):
    class InspectResult:
        returncode = 1

    class FakeProc:
        returncode = None

        def poll(self):
            return self.returncode

    captured = {}
    terminated = []

    monkeypatch.setattr(webui.subprocess, "run", lambda *args, **kwargs: InspectResult())

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["start_new_session"] = kwargs.get("start_new_session")
        proc = FakeProc()
        captured["proc"] = proc
        return proc

    def fake_terminate(proc):
        terminated.append(proc)
        proc.returncode = -15

    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(webui, "terminate_process_tree", fake_terminate)

    try:
        webui.ensure_docker_image(
            "aiden-test:local",
            True,
            tmp_path / "build.log",
            "mobilegym-base",
            stop_requested=lambda: True,
        )
    except webui.JobStopped:
        pass
    else:
        raise AssertionError("cancelable Docker build did not stop")

    assert terminated == [captured["proc"]]
    assert "--target" in captured["cmd"]
    assert "mobilegym-base" in captured["cmd"]
    if webui.os.name == "posix":
        assert captured["start_new_session"] is True


def test_ensure_daemon_image_uses_compose_build(tmp_path: Path, monkeypatch):
    class FakeProc:
        returncode = 0

        def poll(self):
            return self.returncode

    captured = {}

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["cwd"] = kwargs.get("cwd")
        captured["env"] = kwargs.get("env") or {}
        captured["start_new_session"] = kwargs.get("start_new_session")
        return FakeProc()

    monkeypatch.setattr(webui.subprocess, "Popen", fake_popen)

    webui.ensure_daemon_image("aiden-test-daemon:local", True, tmp_path / "build.log")

    assert captured["cmd"] == webui.daemon_compose_command("build", "daemon")
    assert captured["cwd"] == webui.BENCHMARK_DOCKER_DIR
    assert captured["env"]["AIDEN_DAEMON_IMAGE"] == "aiden-test-daemon:local"
    if webui.os.name == "posix":
        assert captured["start_new_session"] is True


def test_index_html_exposes_judge_settings_panel():
    assert "grid-template-columns: 600px minmax(0, 1fr);" in webui.INDEX_HTML
    assert 'id="judgeEnabled"' in webui.INDEX_HTML
    assert 'id="judgeModel"' in webui.INDEX_HTML
    assert 'id="judgeApiKey"' in webui.INDEX_HTML
    assert 'id="noJudge"' not in webui.INDEX_HTML
    assert "/api/webui-settings" in webui.INDEX_HTML
    assert "localStorage" not in webui.INDEX_HTML
    assert 'id="editAgentConfig"' in webui.INDEX_HTML
    assert 'id="agentConfigText" spellcheck="false" readonly' in webui.INDEX_HTML
    assert "function setAgentConfigEditing" in webui.INDEX_HTML
    assert 'id="activeStopJob"' in webui.INDEX_HTML
    assert "function stopJob" in webui.INDEX_HTML
    assert "/api/jobs/${encodeURIComponent(id)}/stop" in webui.INDEX_HTML
    assert 'id="runEnvDialog"' in webui.INDEX_HTML
    assert 'id="confirmRunBtn"' in webui.INDEX_HTML
    assert "document.getElementById('runBtn').onclick = openRunEnvironmentDialog" in webui.INDEX_HTML
    assert "document.getElementById('confirmRunBtn').onclick = confirmRun" in webui.INDEX_HTML
    assert "let runEnvBackdropPointerDown = false" in webui.INDEX_HTML
    assert "runEnvDialog.onpointerdown" in webui.INDEX_HTML
    assert "e.target === runEnvDialog && runEnvBackdropPointerDown" in webui.INDEX_HTML
    assert "runButton.disabled = selectedSuites.size === 0 || mode === 'mixed'" in webui.INDEX_HTML
    assert "document.getElementById('runEnvDialog').hidden = false" in webui.INDEX_HTML
    assert "async function startRun(env)" in webui.INDEX_HTML
    assert "if(started) closeRunEnvironmentDialog()" in webui.INDEX_HTML
    assert '<th style="width:220px">Suite</th>' in webui.INDEX_HTML
    assert 'colspan="6">No jobs yet' in webui.INDEX_HTML
    assert "function suiteDisplayName" in webui.INDEX_HTML
    assert "const suiteNames = suiteKeys.map(suiteDisplayName)" in webui.INDEX_HTML
    assert "function stopTask" in webui.INDEX_HTML
    assert "/tasks/${encodeURIComponent(taskId)}/stop" in webui.INDEX_HTML
    assert "data-stop-task" in webui.INDEX_HTML
    assert '<th style="width:160px"></th>' in webui.INDEX_HTML
    assert 'class="inline-actions env-actions"' in webui.INDEX_HTML
    assert "web_url: env.web_url" in webui.INDEX_HTML
    assert 'id="mobilegymParallelEnvs"' in webui.INDEX_HTML
    assert 'id="mobilegymParallelEnvs" type="number" min="1" step="1" value="5"' in webui.INDEX_HTML
    assert "mobilegymParallelEnvs').value || '5'" in webui.INDEX_HTML
    assert "env.parallel_envs || 1" not in webui.INDEX_HTML
    assert "parallel_envs" in webui.INDEX_HTML
    assert ">screen</a>" in webui.INDEX_HTML
    assert "job.environment_web_url" not in webui.INDEX_HTML
    assert "screenLink" not in webui.INDEX_HTML
    assert 'id="taskRows"' in webui.INDEX_HTML
    assert "task_records" in webui.INDEX_HTML
    assert "/tasks/${encodeURIComponent(task.id)}/log" in webui.INDEX_HTML
    assert "screen_url" in webui.INDEX_HTML
    assert "const report = job.report_url" in webui.INDEX_HTML
    assert "(job.suite_results || []).filter(r => r.report_url)" not in webui.INDEX_HTML
    assert "const progressTotals = progress.totals || {}" in webui.INDEX_HTML
    assert "Object.keys(progressTotals).length ? progressTotals : suiteTotals" in webui.INDEX_HTML


def test_run_job_uses_saved_webui_agent_config(tmp_path: Path, monkeypatch):
    base = tmp_path / "base"
    base.mkdir()
    app = webui.BenchmarkWebApp(webui.WebUIConfig(runs_dir=tmp_path / "runs", base_config_dir=base, build_daemon_image=False))
    saved = 'custom_instruction = "from web ui"\n[model]\nprovider = "fake"\n[device]\ndevice_type = "iOS"\n'
    app.save_agent_config({"content": saved})
    job = webui.Job(
        id="job-test",
        endpoint="http://127.0.0.1:19090",
        docker_endpoint="http://host.docker.internal:19090",
        environment_endpoint="http://127.0.0.1:19090",
        suites=["mobilegym_basic.json"],
        environment_type="device",
        agent_url="http://127.0.0.1:18080",
        container_name="aiden-benchmark-agent-job-test",
        config_dir=str(tmp_path / "runs" / "job-test" / "config"),
        raw_runs_dir=str(tmp_path / "runs" / "job-test" / "raw"),
        state_file=str(tmp_path / "runs" / "job-test" / "state.json"),
        runner_log=str(tmp_path / "runs" / "job-test" / "runner.log"),
        daemon_log=str(tmp_path / "runs" / "job-test" / "daemon.log"),
    )
    app._jobs[job.id] = job

    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        webui,
        "read_environment_health",
        lambda endpoint: {"platform": "android"},
    )
    captured = {}

    def fake_start_daemon_compose(*args, **kwargs):
        captured.update(kwargs)
        return "container-id"

    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(app, "_wait_for_daemon", lambda job: None)

    def fake_run_suite(job, suite_key):
        job.suite_results.append({"suite": suite_key, "exit_code": 0})

    monkeypatch.setattr(app, "_run_suite", fake_run_suite)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)

    app._run_job(job)

    saved_content = (Path(job.config_dir) / "agent.toml").read_text(encoding="utf-8")
    assert 'custom_instruction = "from web ui"' in saved_content
    assert "voice_streaming_tts_enabled = false" in saved_content
    assert "voice_tool_call_speech = false" in saved_content
    assert "voice_progress_speech_enabled = false" in saved_content
    assert 'provider = "fake"' in saved_content
    assert 'device_type = "iOS"' in saved_content
    assert captured["device_type"] == "android"
    assert job.status == "passed"


def test_daemon_compose_command_and_env_use_environment_bridge_providers(tmp_path: Path):
    config = tmp_path / "config"
    config.mkdir()

    cmd = webui.daemon_compose_command(
        "up",
        "-d",
        "--force-recreate",
        "daemon",
        project="aiden-benchmark-agent-test",
    )
    env = webui.daemon_compose_env(
        image="aiden-agent-daemon:local",
        host_port=18081,
        config_dir=config,
        environment_bridge_endpoint="http://host.docker.internal:18080",
        benchmark_task_id="suite.json:t1",
        device_type="android",
    )

    assert cmd[:4] == ["docker", "compose", "-f", str(webui.AGENT_DAEMON_COMPOSE_FILE)]
    assert "-p" in cmd
    assert "aiden-benchmark-agent-test" in cmd
    assert cmd[-4:] == ["up", "-d", "--force-recreate", "daemon"]
    assert env["AIDEN_DAEMON_IMAGE"] == "aiden-agent-daemon:local"
    assert env["AIDEN_DAEMON_HOST_PORT"] == "18081"
    assert env["AIDEN_CONFIG_DIR"] == str(config.resolve())
    assert env["ENVIRONMENT_BRIDGE_ENDPOINT"] == "http://host.docker.internal:18080"
    assert env["AIDEN_BENCHMARK_TASK_ID"] == "suite.json:t1"
    assert env["AIDEN_ENVIRONMENT_BRIDGE_MODE"] == "1"
    assert env["AIDEN_DEVICE_TYPE"] == "android"
    assert "host.docker.internal" in env["NO_PROXY"]
    compose_text = webui.AGENT_DAEMON_COMPOSE_FILE.read_text(encoding="utf-8")
    entrypoint_text = (webui.BENCHMARK_DOCKER_DIR / "agent-daemon-entrypoint.sh").read_text(
        encoding="utf-8"
    )
    assert "AIDEN_ENVIRONMENT_BRIDGE_MODE: ${AIDEN_ENVIRONMENT_BRIDGE_MODE:-0}" in compose_text
    assert "AIDEN_ENVIRONMENT_BRIDGE_TOOLS" not in compose_text
    assert "AIDEN_BENCHMARK_TASK_ID" in compose_text
    assert "AIDEN_DEVICE_TYPE" in compose_text
    assert "--environment-bridge-mode" in entrypoint_text
    assert '--environment-bridge-endpoint "$ENVIRONMENT_BRIDGE_ENDPOINT"' in entrypoint_text
    assert "environment-bridge-tools" not in entrypoint_text
    assert '--device-type "$AIDEN_DEVICE_TYPE"' in entrypoint_text


def test_start_daemon_compose_leaves_auto_host_port_for_docker(tmp_path: Path, monkeypatch):
    captured = {}
    job = webui.Job(
        id="auto-port-job",
        endpoint="",
        docker_endpoint="",
        suites=[],
    )

    monkeypatch.setattr(
        webui,
        "reserve_free_port",
        lambda: pytest.fail("Docker should allocate an ephemeral host port"),
    )

    def fake_run_logged_command(command, log_path, **kwargs):
        captured["command"] = command
        captured["env"] = kwargs["env"]

    monkeypatch.setattr(webui, "run_logged_command", fake_run_logged_command)
    monkeypatch.setattr(webui.subprocess, "check_output", lambda *args, **kwargs: "container-id\n")

    container_id = webui.start_daemon_compose(
        job,
        image="aiden-agent-daemon:test",
        host_port=0,
        config_dir=tmp_path / "config",
        environment_bridge_endpoint="",
        log_path=tmp_path / "daemon.log",
    )

    assert container_id == "container-id"
    assert captured["env"]["AIDEN_DAEMON_HOST_PORT"] == "0"
    assert captured["command"][-1] == "daemon"


def test_build_mobilegym_environment_command_starts_preview_and_bridge(tmp_path: Path):
    benchmark_dir = tmp_path / "benchmark"
    benchmark_dir.mkdir()

    cmd = webui.build_mobilegym_environment_command(
        image=webui.DEFAULT_MOBILEGYM_IMAGE,
        container_name="aiden-mobilegym-env-test",
        host_web_port=18173,
        host_bridge_port=19090,
        benchmark_dir=benchmark_dir,
        parallel_envs=3,
    )

    assert cmd[:5] == ["docker", "run", "--rm", "-d", "--name"]
    assert "aiden-mobilegym-env-test" in cmd
    assert "127.0.0.1:18173:4173" in cmd
    assert "19090:9090" in cmd
    assert f"{benchmark_dir.resolve()}:/app/benchmark:ro" in cmd
    assert "MOBILEGYM_ROOT=/mobilegym" in cmd
    assert "--entrypoint" in cmd
    assert cmd[-2] == "-c"
    script = cmd[-1]
    assert 'export PATH="/opt/venv/bin:$PATH"' in script
    assert "python3 -c" in script
    assert "npm run preview -- --host 0.0.0.0 --port 4173" in script
    assert "exec python3 /app/benchmark/mobilegym/scripts/start_simulator.py" in script
    assert "--mobilegym-root /mobilegym" in script
    assert "--env-url http://127.0.0.1:4173" in script
    assert "--bridge-host 0.0.0.0" in script
    assert "--bridge-port 9090" in script
    assert "--parallel-envs 3" in script
    assert "--headless" in script
    assert webui.DEFAULT_MOBILEGYM_IMAGE in cmd


def test_start_mobilegym_environment_returns_docker_reachable_endpoint(tmp_path: Path, monkeypatch):
    config_dir = tmp_path / "config"
    config_dir.mkdir()
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=config_dir,
            build_mobilegym_image=False,
        )
    )
    health_urls = []
    commands = []

    from runner import environment as env_module

    monkeypatch.setattr(env_module, "ensure_mobilegym_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(env_module, "wait_for_http_health", lambda url, timeout: health_urls.append((url, timeout)))
    monkeypatch.setattr(env_module, "start_docker_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        env_module,
        "docker_published_port",
        lambda container_name, container_port: 18173 if container_port == 4173 else 19090,
    )

    def fake_check_output(command, cwd=None, text=False):
        commands.append(command)
        return "container-id\n"

    monkeypatch.setattr(env_module.subprocess, "check_output", fake_check_output)

    env = app.start_mobilegym_environment({"name": "MobileGym smoke", "parallel_envs": 2})

    assert env["name"] == "MobileGym smoke"
    assert env["type"] == "mobilegym"
    assert env["status"] == "running"
    assert env["endpoint"] == "http://host.docker.internal:19090"
    assert env["public_endpoint"] == "http://127.0.0.1:19090"
    assert env["web_url"] == "http://127.0.0.1:18173"
    assert env["container_id"] == "container-id"
    assert env["parallel_envs"] == 2
    assert health_urls == [("http://127.0.0.1:19090/health", webui.DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC)]
    assert commands and "aiden-mobilegym-env-" in commands[0][5]
    assert "127.0.0.1::4173" in commands[0]
    assert "127.0.0.1::9090" in commands[0]
    assert "--parallel-envs 2" in commands[0][-1]


def test_start_mobilegym_environment_defaults_to_five_envs(tmp_path: Path, monkeypatch):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
            build_mobilegym_image=False,
        )
    )
    commands = []

    from runner import environment as env_module

    monkeypatch.setattr(env_module, "ensure_mobilegym_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(env_module, "wait_for_http_health", lambda *args, **kwargs: None)
    monkeypatch.setattr(env_module, "start_docker_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        env_module,
        "docker_published_port",
        lambda container_name, container_port: 18173 if container_port == 4173 else 19090,
    )
    monkeypatch.setattr(
        env_module.subprocess,
        "check_output",
        lambda command, cwd=None, text=False: commands.append(command) or "container-id\n",
    )

    env = app.start_mobilegym_environment({"name": "MobileGym smoke"})

    assert env["parallel_envs"] == webui.DEFAULT_MOBILEGYM_PARALLEL_ENVS == 5
    assert commands and "--parallel-envs 5" in commands[0][-1]


def test_stop_mobilegym_environment_removes_container(tmp_path: Path, monkeypatch):
    app = webui.BenchmarkWebApp(webui.WebUIConfig(runs_dir=tmp_path / "runs", base_config_dir=tmp_path / "config"))
    env = webui.MobileGymEnvironment(
        id="mg-test",
        name="MobileGym",
        endpoint="http://host.docker.internal:19090",
        public_endpoint="http://127.0.0.1:19090",
        web_url="http://127.0.0.1:18173",
        status="running",
        container_name="aiden-mobilegym-env-mg-test",
        log_path=str(tmp_path / "env.log"),
    )
    app.env_manager._environments[env.id] = env
    removed = []

    def fake_run(command, **kwargs):
        removed.append(command)

        class Result:
            returncode = 0

        return Result()

    monkeypatch.setattr(webui.subprocess, "run", fake_run)

    stopped = app.stop_mobilegym_environment("mg-test")

    assert stopped is not None
    assert stopped["status"] == "stopped"
    assert removed == [["docker", "rm", "-f", "aiden-mobilegym-env-mg-test"]]


def test_adb_android_environment_lifecycle(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    launched = []
    killed = []

    class FakeProc:
        pid = 6161

    monkeypatch.setattr(
        adb_env_mod, "start_adb_bridge_process", lambda **kwargs: launched.append(kwargs) or FakeProc()
    )
    monkeypatch.setattr(adb_env_mod, "wait_for_http_health", lambda url, timeout: None)
    monkeypatch.setattr(adb_env_mod, "pid_alive", lambda pid: pid == 6161)
    monkeypatch.setattr(adb_env_mod, "check_endpoint_health", lambda url, timeout=2.0: True)
    monkeypatch.setattr(adb_env_mod, "terminate_pid", lambda pid, **kwargs: killed.append(pid))

    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(runs_dir=tmp_path / "runs", base_config_dir=tmp_path / "config")
    )

    environment = app.start_adb_android_environment(
        {"name": "Genymotion", "serial": "127.0.0.1:6555", "bridge_port": 18899}
    )
    assert environment["status"] == "running"
    assert environment["serial"] == "127.0.0.1:6555"
    assert environment["public_endpoint"] == "http://127.0.0.1:18899"
    assert environment["endpoint"] == "http://host.docker.internal:18899"
    assert environment["parallel_envs"] == 1
    assert launched[0]["serial"] == "127.0.0.1:6555"
    assert launched[0]["bridge_port"] == 18899
    manifest_path = Path(environment["manifest_path"])
    assert manifest_path.exists()

    listed = app.list_adb_android_environments()
    assert [env["id"] for env in listed] == [environment["id"]]

    stopped = app.stop_adb_android_environment(environment["id"])
    assert stopped["status"] == "stopped"
    assert killed == [6161]

    deleted = app.delete_adb_android_environment(environment["id"])
    assert deleted is not None
    assert not manifest_path.exists()
    assert app.list_adb_android_environments() == []


def test_adb_android_environment_recovery(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    runs_dir = tmp_path / "runs"
    env_dir = runs_dir / "environments" / "adb-recovered"
    env_dir.mkdir(parents=True)
    (env_dir / adb_env_mod.ADB_ENV_MANIFEST_NAME).write_text(
        json.dumps(
            {
                "id": "adb-recovered",
                "name": "Genymotion",
                "pid": 7171,
                "serial": "127.0.0.1:6555",
                "bridge_port": 18899,
                "public_endpoint": "http://127.0.0.1:18899",
                "log_path": str(env_dir / "adb_android.log"),
                "created_at": "2026-07-10T00:00:00+00:00",
            }
        ),
        encoding="utf-8",
    )

    monkeypatch.setattr(adb_env_mod, "pid_alive", lambda pid: True)
    monkeypatch.setattr(adb_env_mod, "check_endpoint_health", lambda url, timeout=2.0: True)
    manager = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    envs = manager.list_all()
    assert len(envs) == 1
    assert envs[0].status == "running"
    assert "recovered" in envs[0].name
    assert envs[0].public_endpoint == "http://127.0.0.1:18899"

    # Dead pid (or failing health) must mark the recovered env unhealthy.
    monkeypatch.setattr(adb_env_mod, "pid_alive", lambda pid: False)
    manager_unhealthy = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    envs = [env for env in manager_unhealthy._environments.values()]
    assert envs[0].status == "unhealthy"


def test_start_job_resolves_adb_android_environment(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )

    class FakeProc:
        pid = 8181

    monkeypatch.setattr(adb_env_mod, "start_adb_bridge_process", lambda **kwargs: FakeProc())
    monkeypatch.setattr(adb_env_mod, "wait_for_http_health", lambda url, timeout: None)

    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    environment = app.start_adb_android_environment(
        {"name": "Genymotion", "serial": "127.0.0.1:6555", "bridge_port": 18899}
    )

    class FakeThread:
        def __init__(self, *args, **kwargs):
            pass

        def start(self):
            pass

    monkeypatch.setattr(webui.threading, "Thread", FakeThread)
    monkeypatch.setattr(webui, "reserve_free_port", lambda: 18080)

    job = app.start_job(
        {
            "endpoint": environment["endpoint"],
            "environment_type": "adb_android",
            "environment_id": environment["id"],
            "environment": {"id": environment["id"], "type": "adb_android"},
            "suites": ["suite.json"],
            "parallel_tasks": 4,
            "no_judge": True,
        }
    )

    assert job["environment_type"] == "adb_android"
    assert job["environment_endpoint"] == "http://127.0.0.1:18899"
    assert job["docker_endpoint"] == "http://host.docker.internal:18899"
    assert job["environment_web_url"] == ""
    # Single adb device: parallel_tasks is always forced to 1.
    assert job["parallel_tasks"] == 1


def test_start_job_rejects_unknown_environment_type(tmp_path: Path):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "suite.json").write_text(
        json.dumps({"name": "suite", "tasks": [{"id": "t1", "category": "diagnostic"}]}),
        encoding="utf-8",
    )
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    with pytest.raises(ValueError, match="environment_type"):
        app.start_job(
            {
                "endpoint": "http://127.0.0.1:9090",
                "environment_type": "bogus",
                "suites": ["suite.json"],
            }
        )


def test_run_mock_suite_uses_auto_agent_setup_and_updates_task_records(
    tmp_path: Path,
    monkeypatch,
):
    suites = Path(__file__).resolve().parents[1] / "suites"
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=suites,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
            build_daemon_image=False,
        )
    )
    job_dir = tmp_path / "runs" / "job-mock"
    raw_runs_dir = job_dir / "raw"
    raw_runs_dir.mkdir(parents=True)
    config_dir = job_dir / "config"
    config_dir.mkdir()
    job = webui.Job(
        id="job-mock",
        endpoint="",
        docker_endpoint="",
        suites=["aiden_app/notes_entry_policy_v1.json"],
        environment_type="mock",
        environment_name="Mock Aiden App environment",
        config_dir=str(config_dir),
        raw_runs_dir=str(raw_runs_dir),
        state_file=str(job_dir / "state.json"),
        runner_log=str(job_dir / "runner.log"),
        daemon_log=str(job_dir / "daemon.log"),
        no_judge=True,
    )
    seen = {}

    def fake_run_runner_process(run_job, cmd, env):
        seen["job"] = run_job
        seen["cmd"] = list(cmd)
        seen["env"] = dict(env)
        run_id = cmd[cmd.index("--run-id") + 1]
        run_dir = raw_runs_dir / run_id
        run_dir.mkdir(parents=True)
        (run_dir / "manifest.json").write_text(
            json.dumps({"run_id": run_id}),
            encoding="utf-8",
        )
        task_ids = [
            "ios_pip_notes_already_open",
            "ios_pip_notes_icon_visible",
            "ios_pip_notes_icon_missing",
        ]
        (run_dir / "results.jsonl").write_text(
            "".join(
                json.dumps({"task_id": task_id, "status": "passed"}) + "\n"
                for task_id in task_ids
            ),
            encoding="utf-8",
        )
        return 0

    monkeypatch.setattr(app, "_run_runner_process", fake_run_runner_process)

    app._run_mock_suite(job, "aiden_app/notes_entry_policy_v1.json")

    cmd = seen["cmd"]
    assert "--auto-agent-setup" in cmd
    assert "--no-build-daemon-image" in cmd
    assert "--no-judge" in cmd
    assert "--environment-url" not in cmd
    assert "--agent-url" not in cmd
    assert cmd[cmd.index("--base-config-dir") + 1] == str(config_dir)
    assert len(job.task_records) == 3
    assert {record.status for record in job.task_records} == {"passed"}
    assert job.suite_results[0]["exit_code"] == 0
    assert job.suite_results[0]["run_id"].startswith("job-mock-")


@pytest.mark.parametrize(
    ("platforms", "expected_platform"),
    [
        (["ios"], "ios"),
        (["ios", "android"], "mixed"),
    ],
)
def test_run_job_mock_mode_uses_runner_platform_summary(
    tmp_path: Path,
    monkeypatch,
    platforms,
    expected_platform,
):
    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(
            suites_dir=tmp_path,
            runs_dir=tmp_path / "runs",
            base_config_dir=tmp_path / "config",
        )
    )
    job_dir = tmp_path / "runs" / "job-mock"
    job_dir.mkdir(parents=True)
    suites = [f"{platform}.json" for platform in platforms]
    job = webui.Job(
        id="job-mock",
        endpoint="",
        docker_endpoint="",
        suites=suites,
        environment_type="mock",
        environment_name="Mock Aiden App environment",
        config_dir=str(job_dir / "config"),
        raw_runs_dir=str(job_dir / "raw"),
        state_file=str(job_dir / "state.json"),
        runner_log=str(job_dir / "runner.log"),
        daemon_log=str(job_dir / "daemon.log"),
        no_judge=True,
    )
    app._jobs[job.id] = job
    calls = []

    monkeypatch.setattr(app, "get_agent_config", lambda: {"content": ""})
    monkeypatch.setattr(webui, "prepare_run_config", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        webui,
        "ensure_daemon_image",
        lambda *args, **kwargs: pytest.fail("mock mode must not start a shared daemon"),
    )
    monkeypatch.setattr(
        webui,
        "start_daemon_compose",
        lambda *args, **kwargs: pytest.fail("mock mode must not start a shared daemon"),
    )

    def fake_run_mock_suite(run_job, suite_key):
        calls.append(suite_key)
        platform = Path(suite_key).stem
        run_job.suite_results.append(
            {
                "suite": suite_key,
                "exit_code": 0,
                "manifest": {"target_platform": platform},
            }
        )

    monkeypatch.setattr(app, "_run_mock_suite", fake_run_mock_suite)
    monkeypatch.setattr(app, "_refresh_job_report", lambda run_job: None)

    app._run_job(job)

    assert calls == suites
    assert job.status == "passed"
    assert job.target_platform == expected_platform
    persisted = json.loads((job_dir / "job.json").read_text(encoding="utf-8"))
    assert persisted["target_platform"] == expected_platform


def test_webui_html_exposes_mock_environment_run_mode():
    assert "Mock Aiden App environment" in webui.INDEX_HTML
    assert "selectedSuiteEnvironmentMode" in webui.INDEX_HTML
    assert "Mock suites and external device suites must run in separate jobs" in webui.INDEX_HTML


def test_webui_inline_script_has_balanced_braces():
    script = webui.INDEX_HTML.split("<script>", 1)[1].split("</script>", 1)[0]

    assert script.count("{") == script.count("}")


def test_failed_adb_start_does_not_leave_resurrectable_manifest(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    class FakeProc:
        pid = 9191

    killed = []
    monkeypatch.setattr(adb_env_mod, "start_adb_bridge_process", lambda **kwargs: FakeProc())
    monkeypatch.setattr(adb_env_mod, "terminate_pid", lambda pid, **kwargs: killed.append(pid))

    def failing_health(url, timeout):
        raise RuntimeError("bridge never became healthy")

    monkeypatch.setattr(adb_env_mod, "wait_for_http_health", failing_health)

    runs_dir = tmp_path / "runs"
    manager = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    with pytest.raises(RuntimeError, match="failed to start"):
        manager.start_adb_android(name="Genymotion", serial="127.0.0.1:6555", bridge_port=18899)
    assert killed == [9191]

    env = next(iter(manager._environments.values()))
    env_dir = runs_dir / "environments" / env.id
    # Manifest and pidfile must be gone; the log stays for debugging.
    assert not (env_dir / adb_env_mod.ADB_ENV_MANIFEST_NAME).exists()
    assert not (env_dir / adb_env_mod.ADB_ENV_PID_NAME).exists()
    assert (env_dir / adb_env_mod.ADB_ENV_LOG_NAME).exists()

    # A restarted WebUI must not resurrect the failed environment.
    reloaded = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    assert reloaded.list_all() == []


def test_stopped_adb_environment_does_not_resurface_after_restart(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    class FakeProc:
        pid = 9292

    monkeypatch.setattr(adb_env_mod, "start_adb_bridge_process", lambda **kwargs: FakeProc())
    monkeypatch.setattr(adb_env_mod, "wait_for_http_health", lambda url, timeout: None)
    monkeypatch.setattr(adb_env_mod, "terminate_pid", lambda pid, **kwargs: None)

    runs_dir = tmp_path / "runs"
    manager = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    env = manager.start_adb_android(name="Genymotion", serial="127.0.0.1:6555", bridge_port=18899)
    manifest_path = Path(env.manifest_path)
    assert manifest_path.exists()

    stopped = manager.stop(env.id)
    assert stopped.status == "stopped"
    # Deliberate stop removes the manifest: it must not come back as a
    # misleading "(recovered, unhealthy)" ghost after a WebUI restart.
    assert not manifest_path.exists()

    reloaded = adb_env_mod.ADBAndroidEnvironmentManager(runs_dir=runs_dir)
    assert reloaded.list_all() == []


def test_start_adb_android_environment_treats_port_zero_as_auto(tmp_path: Path, monkeypatch):
    from runner import adb_android_environment as adb_env_mod

    class FakeProc:
        pid = 9393

    launched = []
    monkeypatch.setattr(
        adb_env_mod, "start_adb_bridge_process", lambda **kwargs: launched.append(kwargs) or FakeProc()
    )
    monkeypatch.setattr(adb_env_mod, "wait_for_http_health", lambda url, timeout: None)
    monkeypatch.setattr(adb_env_mod, "reserve_free_port", lambda: 28899)

    app = webui.BenchmarkWebApp(
        webui.WebUIConfig(runs_dir=tmp_path / "runs", base_config_dir=tmp_path / "config")
    )

    # 0 (form input) means auto-pick, matching the CLI --bridge-port 0 sentinel.
    environment = app.start_adb_android_environment({"serial": "127.0.0.1:6555", "bridge_port": 0})
    assert environment["bridge_port"] == 28899
    assert launched[0]["bridge_port"] == 28899

    # Explicit positive port still validated and honored.
    environment = app.start_adb_android_environment({"serial": "127.0.0.1:6555", "bridge_port": "18899"})
    assert environment["bridge_port"] == 18899

    # Garbage still rejected.
    with pytest.raises(ValueError, match="bridge_port"):
        app.start_adb_android_environment({"serial": "127.0.0.1:6555", "bridge_port": "abc"})
    with pytest.raises(ValueError, match="bridge_port"):
        app.start_adb_android_environment({"serial": "127.0.0.1:6555", "bridge_port": -1})

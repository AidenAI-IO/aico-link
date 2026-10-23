import json
from pathlib import Path

from runner.judge import JudgeConfig, judge_task
from runner.suite import RubricItem


def test_judge_uses_configured_api_key_env(monkeypatch):
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
                    "items": [{"id": "ok", "verdict": "yes", "reason": "passed"}],
                    "overall_notes": "ok",
                })}}]
            }).encode("utf-8")

    def fake_urlopen(req, timeout):
        seen["authorization"] = req.headers.get("Authorization")
        seen["user_agent"] = req.headers.get("User-agent")
        seen["url"] = req.full_url
        seen["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setenv("JUDGE_API_KEY", "sk-judge")
    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = judge_task(
        description="check",
        rubric=[RubricItem(id="ok", check="pass")],
        pre_screenshot=None,
        post_screenshot=None,
        trace={"tool_calls": []},
        final_response="done",
        cfg=JudgeConfig(api_key_env="JUDGE_API_KEY", base_url="https://judge.example.com/v1/"),
    )

    assert result.verdicts[0].verdict == "yes"
    assert seen == {
        "authorization": "Bearer sk-judge",
        "user_agent": "aico-benchmark/1.0",
        "url": "https://judge.example.com/v1/chat/completions",
        "timeout": 120,
    }


def test_judge_sends_only_pre_and_post_screenshots(monkeypatch, tmp_path: Path):
    seen = {}
    pre = tmp_path / "pre.jpg"
    post = tmp_path / "post.jpg"
    pre.write_bytes(b"pre-image")
    post.write_bytes(b"post-image")

    class FakeResponse:
        status = 200

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

        def read(self):
            return json.dumps({
                "choices": [{"message": {"content": json.dumps({
                    "items": [{"id": "ok", "verdict": "yes", "reason": "passed"}],
                    "overall_notes": "ok",
                })}}]
            }).encode("utf-8")

    def fake_urlopen(req, timeout):
        seen["payload"] = json.loads(req.data.decode("utf-8"))
        return FakeResponse()

    monkeypatch.setenv("JUDGE_API_KEY", "sk-judge")
    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = judge_task(
        description="check",
        rubric=[RubricItem(id="ok", check="pass")],
        pre_screenshot=pre,
        post_screenshot=post,
        trace={"tool_calls": []},
        final_response="done",
        cfg=JudgeConfig(api_key_env="JUDGE_API_KEY"),
    )

    content = seen["payload"]["messages"][0]["content"]
    labels = [part["text"].rstrip(":") for part in content if part["type"] == "text" and part["text"].endswith(":")]
    images = [part for part in content if part["type"] == "image_url"]
    assert labels == ["PRE-SCREENSHOT", "POST-SCREENSHOT"]
    assert len(images) == 2
    assert result.image_count == 2
    assert result.image_labels == ["PRE-SCREENSHOT", "POST-SCREENSHOT"]


def test_judge_does_not_fallback_to_agent_toml_api_key(monkeypatch, tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text('[model]\napi_key = "sk-agent"\n', encoding="utf-8")

    monkeypatch.setenv("AICO_AGENT_CONFIG", str(config))
    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY", raising=False)
    monkeypatch.delenv("OPENAI_AUTH_TOKEN", raising=False)
    monkeypatch.delenv("OPENAI_API_KEY", raising=False)
    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)

    try:
        judge_task(
            description="check",
            rubric=[RubricItem(id="ok", check="pass")],
            pre_screenshot=None,
            post_screenshot=None,
            trace={"tool_calls": []},
            final_response="done",
            cfg=JudgeConfig(),
        )
    except RuntimeError as e:
        assert "AICO_BENCHMARK_JUDGE_API_KEY" in str(e)
    else:
        raise AssertionError("expected missing judge API key error")


def test_default_judge_uses_benchmark_specific_api_key_env(monkeypatch):
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
                    "items": [{"id": "ok", "verdict": "yes", "reason": "passed"}],
                    "overall_notes": "ok",
                })}}]
            }).encode("utf-8")

    def fake_urlopen(req, timeout):
        seen["authorization"] = req.headers.get("Authorization")
        return FakeResponse()

    monkeypatch.setenv("AICO_BENCHMARK_JUDGE_API_KEY", "sk-benchmark-judge")
    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)
    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    judge_task(
        description="check",
        rubric=[RubricItem(id="ok", check="pass")],
        pre_screenshot=None,
        post_screenshot=None,
        trace={"tool_calls": []},
        final_response="done",
        cfg=JudgeConfig(),
    )

    assert seen["authorization"] == "Bearer sk-benchmark-judge"


def test_default_judge_does_not_read_provider_specific_api_key_envs(monkeypatch):
    monkeypatch.delenv("AICO_BENCHMARK_JUDGE_API_KEY", raising=False)
    monkeypatch.setenv("OPENAI_AUTH_TOKEN", "sk-openai-compatible")
    monkeypatch.setenv("OPENAI_API_KEY", "sk-openai")
    monkeypatch.setenv("OPENROUTER_API_KEY", "sk-openrouter")

    try:
        judge_task(
            description="check",
            rubric=[RubricItem(id="ok", check="pass")],
            pre_screenshot=None,
            post_screenshot=None,
            trace={"tool_calls": []},
            final_response="done",
            cfg=JudgeConfig(),
        )
    except RuntimeError as exc:
        assert str(exc) == "missing env var AICO_BENCHMARK_JUDGE_API_KEY"
    else:
        raise AssertionError("expected provider-specific API key envs to be ignored")

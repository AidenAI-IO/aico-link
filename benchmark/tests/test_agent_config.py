from pathlib import Path

try:
    import tomllib
except ModuleNotFoundError:
    import tomli as tomllib

from runner import config as runner_config
from runner import agent_config
from runner.agent_config import (
    default_agent_config_path,
    load_agent_model_config,
    resolve_agent_model_api_key,
)


def test_benchmark_agent_template_omits_legacy_instruction():
    template = (
        Path(__file__).resolve().parents[1]
        / "config"
        / "agent.toml.template"
    )
    config = tomllib.loads(template.read_text(encoding="utf-8"))

    assert "instruction" not in config


def test_benchmark_agent_template_uses_benchmark_agent_environment_placeholders():
    template = Path(__file__).resolve().parents[1] / "config" / "agent.toml.template"
    config = tomllib.loads(template.read_text(encoding="utf-8"))

    assert config["model_settings"]["providers"]["benchmark"] == {
        "type": "{{AICO_BENCHMARK_AGENT_PROVIDER}}",
        "base_url": "{{AICO_BENCHMARK_AGENT_BASE_URL}}",
        "api_key": "{{AICO_BENCHMARK_AGENT_API_KEY}}",
    }
    assert config["model_settings"]["model"]["model"] == "{{AICO_BENCHMARK_AGENT_MODEL}}"


def test_render_agent_template_escapes_complete_toml_string_content(monkeypatch):
    api_key = 'secret\\path\nnext\t"quoted"'
    monkeypatch.setenv(runner_config.BENCHMARK_AGENT_API_KEY_ENV, api_key)

    rendered = runner_config.render_agent_template(
        'api_key = "{{AICO_BENCHMARK_AGENT_API_KEY}}"\n'
    )

    assert tomllib.loads(rendered)["api_key"] == api_key


def test_load_agent_model_config_reads_grouped_model_section(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[model_settings.model]
provider = "openrouter"
model = "google/gemini-3.5-flash"
api_key = "sk-direct"
""".strip(),
        encoding="utf-8",
    )

    assert load_agent_model_config(config) == {
        "provider": "openrouter",
        "model": "google/gemini-3.5-flash",
        "api_key": "sk-direct",
    }


def test_load_agent_model_config_resolves_canonical_named_provider(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[model_settings.providers.work]
type = "openrouter"
api_key = "sk-record"
base_url = "https://openrouter.ai/api/v1"

[model_settings.model]
provider = "work"
model = "google/gemini-3.5-flash"
""".strip(),
        encoding="utf-8",
    )

    assert load_agent_model_config(config) == {
        "provider": "openrouter",
        "model": "google/gemini-3.5-flash",
        "api_key": "sk-record",
        "base_url": "https://openrouter.ai/api/v1",
    }


def test_load_agent_model_config_prefers_canonical_namespace_and_type(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[providers.work]
provider = "openai"
api_key = "sk-legacy"

[model_settings.providers.work]
provider = "ollama"
type = "openrouter"
api_key = "sk-canonical"

[model_settings.model]
provider = "work"
model = "google/gemini-3.5-flash"
""".strip(),
        encoding="utf-8",
    )

    resolved = load_agent_model_config(config)

    assert resolved["provider"] == "openrouter"
    assert resolved["api_key"] == "sk-canonical"


def test_load_agent_model_config_does_not_fall_back_from_empty_canonical_type(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[model_settings.providers.work]
type = ""
provider = "openai"

[model_settings.model]
provider = "work"
model = "gpt-4o"
""".strip(),
        encoding="utf-8",
    )

    assert load_agent_model_config(config)["provider"] == ""


def test_load_agent_model_config_reads_legacy_named_provider(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[providers.work]
provider = "openai"
api_key = "sk-legacy"

[model_settings.model]
provider = "work"
model = "gpt-4o"
""".strip(),
        encoding="utf-8",
    )

    resolved = load_agent_model_config(config)

    assert resolved["provider"] == "openai"
    assert resolved["api_key"] == "sk-legacy"


def test_resolve_agent_model_api_key_uses_api_key_environment_reference(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text(
        """
[model_settings.providers.work]
type = "openrouter"
api_key = "$OPENROUTER_API_KEY"

[model_settings.model]
provider = "work"
model = "google/gemini-3.5-flash"
""".strip(),
        encoding="utf-8",
    )

    assert resolve_agent_model_api_key(config, env={"OPENROUTER_API_KEY": "sk-env"}) == "sk-env"


def test_resolve_agent_model_api_key_uses_direct_agent_toml_value(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text('[model_settings.model]\napi_key = "sk-direct"\n', encoding="utf-8")

    assert resolve_agent_model_api_key(config, env={}) == "sk-direct"


def test_resolve_agent_model_api_key_treats_unprefixed_value_as_literal(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text('[model_settings.model]\napi_key = "OPENROUTER_API_KEY"\n', encoding="utf-8")

    assert resolve_agent_model_api_key(config, env={"OPENROUTER_API_KEY": "sk-env"}) == "OPENROUTER_API_KEY"


def test_resolve_agent_model_api_key_keeps_unprefixed_value_without_matching_env(tmp_path: Path):
    config = tmp_path / "agent.toml"
    config.write_text('[model_settings.model]\napi_key = "OPENROUTER_API_KEY"\n', encoding="utf-8")

    assert resolve_agent_model_api_key(config, env={}) == "OPENROUTER_API_KEY"


def test_default_agent_config_path_ignores_directory_env_path(monkeypatch, tmp_path: Path):
    config_dir = tmp_path / "agent.toml"
    config_dir.mkdir()
    monkeypatch.setenv("AICO_AGENT_CONFIG", str(config_dir))
    monkeypatch.setattr(agent_config, "REPO_ROOT", tmp_path / "missing-repo")

    assert default_agent_config_path() != config_dir


def test_resolve_agent_model_api_key_ignores_directory_path(tmp_path: Path):
    config_dir = tmp_path / "agent.toml"
    config_dir.mkdir()

    assert resolve_agent_model_api_key(config_dir, env={}) is None

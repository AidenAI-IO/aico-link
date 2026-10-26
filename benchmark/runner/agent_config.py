from __future__ import annotations

import os
from pathlib import Path
from typing import Mapping


REPO_ROOT = Path(__file__).resolve().parents[2]


def _strip_toml_comment(line: str) -> str:
    in_quote = False
    escaped = False
    out: list[str] = []
    for ch in line:
        if ch == '"' and not escaped:
            in_quote = not in_quote
        if ch == "#" and not in_quote:
            break
        out.append(ch)
        escaped = ch == "\\" and not escaped
        if ch != "\\":
            escaped = False
    return "".join(out).strip()


def _parse_toml_string(value: str) -> str:
    value = value.strip()
    if len(value) >= 2 and value[0] == '"' and value[-1] == '"':
        return bytes(value[1:-1], "utf-8").decode("unicode_escape")
    return value


def load_agent_model_config(path: Path) -> dict[str, str]:
    section = ""
    sections: dict[str, dict[str, str]] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = _strip_toml_comment(raw_line)
        if not line:
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1].strip()
            continue
        if not section or "=" not in line:
            continue
        key, value = line.split("=", 1)
        sections.setdefault(section, {})[key.strip()] = _parse_toml_string(value)

    # The firmware currently ships the grouped model_settings shape, while
    # older local configs still use a flat [model] table. Consumers such as
    # SkillOpt's optimizer and model fallback must read both shapes; callers
    # that intentionally report only benchmark credentials can still apply
    # their own policy after this normalized parse.
    model = dict(sections.get("model_settings.model") or sections.get("model", {}))
    provider_ref = model.get("provider", "").strip()
    if not provider_ref:
        return model

    canonical = sections.get(f"model_settings.providers.{provider_ref}")
    legacy = sections.get(f"providers.{provider_ref}")
    record = canonical if canonical is not None else legacy
    if record is None:
        return model

    if "type" in record:
        model["provider"] = record["type"].strip()
    elif "provider" in record:
        model["provider"] = record["provider"].strip()
    for key in ("api_key", "base_url"):
        if not model.get(key, "").strip() and record.get(key, "").strip():
            model[key] = record[key]
    return model


def default_agent_config_path() -> Path | None:
    candidates = []
    if env_path := os.environ.get("AICO_AGENT_CONFIG"):
        candidates.append(Path(env_path))
    candidates.extend([
        Path("/userdata/agent/agent.toml"),
        REPO_ROOT / "agent.toml",
        REPO_ROOT / "src" / "agent" / "config" / "agent.toml",
    ])
    for path in candidates:
        if path.is_file():
            return path
    return None


def resolve_agent_model_api_key(
    path: Path | None = None,
    *,
    env: Mapping[str, str] | None = None,
) -> str | None:
    if env is None:
        env = os.environ
    if path is None:
        path = default_agent_config_path()
    if path is None or not path.is_file():
        return None

    model = load_agent_model_config(path)
    configured = model.get("api_key", "").strip()
    if not configured:
        return None
    if configured.startswith("$"):
        return env.get(configured[1:].strip()) or None
    return configured


def resolve_api_key(
    api_key_env: str,
    *,
    agent_config_path: str | Path | None = None,
    env: Mapping[str, str] | None = None,
) -> str | None:
    if env is None:
        env = os.environ
    if env.get(api_key_env):
        return env[api_key_env]
    path = Path(agent_config_path) if agent_config_path else None
    return resolve_agent_model_api_key(path, env=env)

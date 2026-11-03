from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Iterable


ARTIFACT_FILENAME = "aico_bridge_actions.json"
ACTION_LOG_FIELDS = (
    "episode_id",
    "action_id",
    "tool_name",
    "tool_input",
    "mobilegym_action",
    "screenshot",
    "duration_ms",
    "error",
)


def export_bridge_actions(
    artifact_dir: str | Path,
    logs: Iterable[dict[str, Any]],
    filename: str = ARTIFACT_FILENAME,
) -> Path:
    output_dir = Path(artifact_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    output_path = output_dir / filename
    payload = [_normalize_action_log(entry) for entry in logs]
    output_path.write_text(json.dumps(payload, indent=2, ensure_ascii=False) + "\n")
    return output_path


def _normalize_action_log(entry: dict[str, Any]) -> dict[str, Any]:
    return {field: entry.get(field) for field in ACTION_LOG_FIELDS}
